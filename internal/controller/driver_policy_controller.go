package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	driverv1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/driver/v1alpha1"
)

// dsTemplateHashAnnotation tracks the rendered pod template across
// reconciles. Changes to ANY field we manage flip the hash; matches
// across reconciles mean no Update is needed. Beats field-by-field
// diff predicates that drift out of sync with what buildDaemonSet
// actually produces.
const dsTemplateHashAnnotation = "driver.tenstorrent.com/template-hash"

// DriverPolicyReconciler manages tt-kmd installation via a per-CR DaemonSet.
//
// Model: one TenstorrentDriverPolicy → one DaemonSet. The DaemonSet's pod
// template is rendered from the CR's spec.version. The CR's status mirrors
// the DaemonSet's rollout state — there's no per-node state machine; the
// DaemonSet's own status is the source of truth.
type DriverPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=driver.tenstorrent.com,resources=tenstorrentdriverpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=driver.tenstorrent.com,resources=tenstorrentdriverpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create

func (r *DriverPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("ttdp", req.Name)

	cr := &driverv1alpha1.TenstorrentDriverPolicy{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !cr.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Count matched nodes for status. The DaemonSet's own nodeAffinity is
	// what actually schedules pods; this is just for the summary.
	matched, err := r.countMatchedNodes(ctx, cr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("count matched nodes: %w", err)
	}

	dsName := driverDaemonSetName(cr.Name)
	desiredDS := r.buildDaemonSet(cr, dsName)

	existing := &appsv1.DaemonSet{}
	err = r.Get(ctx, types.NamespacedName{Namespace: operatorNamespace(), Name: dsName}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if cr.Spec.Paused {
			logger.Info("paused; skipping DaemonSet creation")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, r.updateStatus(ctx, cr, nil, matched)
		}
		if err := r.Create(ctx, desiredDS); err != nil {
			return ctrl.Result{}, fmt.Errorf("create daemonset: %w", err)
		}
		logger.Info("created tt-kmd DaemonSet", "name", dsName, "version", cr.Spec.Version)
		existing = desiredDS
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get daemonset: %w", err)
	default:
		// Update if spec drift (most commonly: version change).
		if !cr.Spec.Paused && daemonSetNeedsUpdate(existing, desiredDS) {
			// Cordon matched nodes + evict device-using pods BEFORE bumping
			// the DS template. By the time the DS controller starts rolling
			// new builder pods, each node should already have refcnt=0 so
			// rmmod succeeds — no forceUnload needed for the common case.
			// Best-effort: errors on individual nodes are logged, not
			// returned; the builder's existing refcnt check is the safety
			// net.
			if drainEnabledForCR(cr) {
				if err := r.prepareUpgrade(ctx, cr); err != nil {
					logger.Error(err, "pre-upgrade drain")
				}
			}
			existing.Spec = desiredDS.Spec
			if existing.Annotations == nil {
				existing.Annotations = map[string]string{}
			}
			existing.Annotations[dsTemplateHashAnnotation] = desiredDS.Annotations[dsTemplateHashAnnotation]
			if err := r.Update(ctx, existing); err != nil {
				return ctrl.Result{}, fmt.Errorf("update daemonset: %w", err)
			}
			logger.Info("updated tt-kmd DaemonSet", "name", dsName, "version", cr.Spec.Version)
		}
	}

	if err := r.updateStatus(ctx, cr, existing, matched); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	// Sync per-node kmd-version labels based on actual pod readiness. The
	// label is what workloads / dashboards / Prometheus read; pod-Ready
	// is the ground-truth signal (readiness probe in buildDaemonSet
	// checks /sys/module/tenstorrent/version against the desired version).
	if existing != nil {
		if err := r.syncKMDVersionLabels(ctx, cr, existing); err != nil {
			logger.Error(err, "sync kmd-version labels")
		}
	}

	// Uncordon nodes whose builder pod is Ready against the current
	// spec.Version. Skips nodes we didn't cordon (annotation mismatch),
	// so external cordons are left alone.
	if drainEnabledForCR(cr) && existing != nil {
		if err := r.uncordonReadyNodes(ctx, cr, existing); err != nil {
			logger.Error(err, "uncordon ready nodes")
		}
	}

	// Requeue while rollout in flight.
	if existing != nil && existing.Status.NumberReady < existing.Status.DesiredNumberScheduled {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// syncKMDVersionLabels writes `driver.tenstorrent.com/kmd-version=<v>` on
// nodes whose installer pod is currently Ready, and removes the label on
// nodes whose pod is NotReady (or gone). Pod-Ready is wired to a probe
// that checks /sys/module/tenstorrent/version, so this label is honest
// about the kernel's actual state.
func (r *DriverPolicyReconciler) syncKMDVersionLabels(
	ctx context.Context, cr *driverv1alpha1.TenstorrentDriverPolicy, ds *appsv1.DaemonSet,
) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(operatorNamespace()),
		client.MatchingLabels{"driver.tenstorrent.com/cr": cr.Name},
	); err != nil {
		return fmt.Errorf("list installer pods: %w", err)
	}

	for i := range pods.Items {
		p := &pods.Items[i]
		nodeName := p.Spec.NodeName
		if nodeName == "" {
			continue
		}
		// The version this pod was templated with — NOT cr.Spec.Version.
		// During a DS rollout the old pods keep running with their old
		// env vars; their probes are passing against the OLD version (the
		// kernel hasn't been touched yet). Writing cr.Spec.Version here
		// would have us say "kmd=2.8.0 on this node" the instant the spec
		// flipped from 2.7.0→2.8.0, even though the kernel still has 2.7.0.
		// Read the truth from the pod's own env.
		podVersion := podEnv(p, "TT_KMD_VERSION")
		if podVersion == "" {
			continue
		}
		ready := false
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}

		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			// Node went away or transient — skip; next reconcile retries.
			continue
		}

		patch := client.MergeFrom(node.DeepCopy())
		cur := node.Labels[LabelKMDVersion]
		if ready {
			if cur == podVersion {
				continue
			}
			if node.Labels == nil {
				node.Labels = map[string]string{}
			}
			node.Labels[LabelKMDVersion] = podVersion
		} else {
			if cur == "" {
				continue
			}
			delete(node.Labels, LabelKMDVersion)
		}
		if err := r.Patch(ctx, node, patch); err != nil {
			// Don't fail the whole reconcile on a single label patch error.
			continue
		}
	}
	return nil
}

// podEnv returns the value of an env var on the pod's first container, or
// "" if not set. Used to read what version a running installer pod was
// templated with (the source of truth for its node's loaded version, not
// the CR's current spec.version which may have just rolled).
func podEnv(p *corev1.Pod, name string) string {
	if len(p.Spec.Containers) == 0 {
		return ""
	}
	for _, e := range p.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func (r *DriverPolicyReconciler) countMatchedNodes(ctx context.Context, cr *driverv1alpha1.TenstorrentDriverPolicy) (int32, error) {
	effSel := cr.Spec.EffectiveNodeAffinity()
	sel, err := metav1.LabelSelectorAsSelector(&effSel)
	if err != nil {
		return 0, err
	}
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return 0, err
	}
	var n int32
	for _, node := range nodes.Items {
		l := labels.Set(node.Labels)
		if !sel.Matches(l) {
			continue
		}
		if requireTenstorrentLabel() && l.Get(LabelTenstorrentPresent) != "true" {
			continue
		}
		if l.Get(LabelDriverSkip) == "true" {
			continue
		}
		n++
	}
	return n, nil
}

func (r *DriverPolicyReconciler) updateStatus(
	ctx context.Context,
	cr *driverv1alpha1.TenstorrentDriverPolicy,
	ds *appsv1.DaemonSet,
	matched int32,
) error {
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.DesiredVersion = cr.Spec.Version
	cr.Status.Summary.Matched = matched

	if ds == nil {
		cr.Status.DaemonSet = ""
		cr.Status.Summary.Desired = 0
		cr.Status.Summary.Ready = 0
		cr.Status.Summary.Available = 0
		cr.Status.Summary.Failed = 0
	} else {
		cr.Status.DaemonSet = ds.Name
		cr.Status.Summary.Desired = ds.Status.DesiredNumberScheduled
		cr.Status.Summary.Ready = ds.Status.NumberReady
		cr.Status.Summary.Available = ds.Status.NumberAvailable
		// Anything scheduled but not Available is in trouble — either
		// CrashLooping (real failure) or just starting (transient). The DS
		// status alone can't distinguish; surface as Failed and let the
		// per-node pod state in `kubectl tt driver` show the detail.
		// (Previous formula did double-bookkeeping and ended up always 0
		// when one pod CrashLooped.)
		cr.Status.Summary.Failed = ds.Status.DesiredNumberScheduled - ds.Status.NumberAvailable
		if cr.Status.Summary.Failed < 0 {
			cr.Status.Summary.Failed = 0
		}
	}

	// Populate per-node state. Best-effort: errors in computing per-node
	// state don't block the status update — the summary still goes out.
	if err := r.populateNodeStatuses(ctx, cr); err != nil {
		log.FromContext(ctx).Error(err, "compute per-node status")
	}

	setDriverConditions(cr)
	return r.Status().Update(ctx, cr)
}

// populateNodeStatuses fills cr.Status.Nodes + cr.Status.Summary.UpToDate /
// InProgress by combining: (a) the set of matched nodes, (b) the installer
// pod on each node and its readiness, (c) cordon ownership from the
// node's annotations.
func (r *DriverPolicyReconciler) populateNodeStatuses(
	ctx context.Context, cr *driverv1alpha1.TenstorrentDriverPolicy,
) error {
	nodes, err := r.listMatchedNodes(ctx, cr)
	if err != nil {
		return fmt.Errorf("list matched nodes: %w", err)
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(operatorNamespace()),
		client.MatchingLabels{"driver.tenstorrent.com/cr": cr.Name},
	); err != nil {
		return fmt.Errorf("list installer pods: %w", err)
	}
	byNode := map[string]podInfo{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" {
			continue
		}
		var rc int32
		if len(p.Status.ContainerStatuses) > 0 {
			rc = p.Status.ContainerStatuses[0].RestartCount
		}
		info := podInfo{version: podEnv(p, "TT_KMD_VERSION"), restartCount: rc}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				info.ready = true
				break
			}
		}
		byNode[p.Spec.NodeName] = info
	}

	// Preserve LastTransitionTime when state hasn't changed.
	prev := map[string]driverv1alpha1.DriverNodeStatus{}
	for _, ns := range cr.Status.Nodes {
		prev[ns.Name] = ns
	}

	out := make([]driverv1alpha1.DriverNodeStatus, 0, len(nodes))
	var upToDate, inProgress int32
	for i := range nodes {
		node := &nodes[i]
		info := byNode[node.Name]
		state := computeDriverNodeState(node, info, cr)
		ns := driverv1alpha1.DriverNodeStatus{
			Name:           node.Name,
			State:          state,
			CurrentVersion: info.version,
		}
		if old, ok := prev[node.Name]; ok && old.State == state {
			ns.LastTransitionTime = old.LastTransitionTime
		} else {
			ns.LastTransitionTime = metav1.Now()
		}
		if state == driverv1alpha1.DriverNodeStateFailed && info.restartCount > 0 {
			ns.Message = fmt.Sprintf("installer pod CrashLoopBackOff (restarts=%d)", info.restartCount)
		}
		out = append(out, ns)

		switch state {
		case driverv1alpha1.DriverNodeStateDone:
			upToDate++
		case driverv1alpha1.DriverNodeStateCordoning,
			driverv1alpha1.DriverNodeStateDraining,
			driverv1alpha1.DriverNodeStateUpgrading,
			driverv1alpha1.DriverNodeStateUncordoning:
			inProgress++
		}
	}
	cr.Status.Nodes = out
	cr.Status.Summary.UpToDate = upToDate
	cr.Status.Summary.InProgress = inProgress
	return nil
}

// podInfo for computeDriverNodeState — duplicated from populateNodeStatuses
// because Go anon-struct types don't carry across function boundaries.
type podInfo struct {
	version      string
	ready        bool
	restartCount int32
}

// computeDriverNodeState derives the per-node state from cordon/pod
// signals. The state model collapses across both drain-enabled and
// drain-disabled paths — for the latter, Cordoning / Draining /
// Uncordoning are never observed.
func computeDriverNodeState(
	node *corev1.Node, info podInfo, cr *driverv1alpha1.TenstorrentDriverPolicy,
) driverv1alpha1.DriverNodeState {
	target := cr.Spec.Version
	cordonedByUs := node.Annotations[AnnoDriverCordonedBy] == cr.Name

	// Failed: pod has restarted ≥3 times (CrashLoopBackOff threshold) AND
	// isn't currently Ready. Stays Failed until restarts settle.
	if info.restartCount >= 3 && !info.ready {
		return driverv1alpha1.DriverNodeStateFailed
	}

	// Done: pod is Ready against the target version.
	if info.ready && info.version == target {
		if cordonedByUs {
			// We cordoned this node but haven't uncordoned yet — between
			// successful insmod and the next reconcile that calls
			// uncordonReadyNodes. Surface as Uncordoning.
			return driverv1alpha1.DriverNodeStateUncordoning
		}
		return driverv1alpha1.DriverNodeStateDone
	}

	// If we have a builder pod templated against the target version (but
	// it isn't Ready yet), the node is mid-Upgrading regardless of cordon
	// state.
	if info.version == target {
		return driverv1alpha1.DriverNodeStateUpgrading
	}

	// Builder pod targets an older version (DS hasn't rolled this node
	// yet) but we've already cordoned → drain is in progress.
	if cordonedByUs {
		return driverv1alpha1.DriverNodeStateDraining
	}

	// No cordon yet — first reconcile after a version bump, or
	// drain.enable=false.
	return driverv1alpha1.DriverNodeStatePending
}

func setDriverConditions(cr *driverv1alpha1.TenstorrentDriverPolicy) {
	s := cr.Status.Summary
	ready := metav1.Condition{Type: "Ready", ObservedGeneration: cr.Generation, LastTransitionTime: metav1.Now()}
	progressing := metav1.Condition{Type: "Progressing", ObservedGeneration: cr.Generation, LastTransitionTime: metav1.Now()}

	switch {
	case cr.Spec.Paused:
		ready.Status, ready.Reason = metav1.ConditionUnknown, "Paused"
		progressing.Status, progressing.Reason = metav1.ConditionFalse, "Paused"
	case s.Desired == 0 && s.Matched == 0:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionTrue, "NoMatchingNodes", "No nodes match selector"
		progressing.Status, progressing.Reason = metav1.ConditionFalse, "Idle"
	case s.Ready == s.Desired && s.Desired > 0:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionTrue, "AllReady", fmt.Sprintf("%d/%d nodes have tt-kmd loaded", s.Ready, s.Desired)
		progressing.Status, progressing.Reason = metav1.ConditionFalse, "Idle"
	default:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "Installing", fmt.Sprintf("%d/%d nodes ready", s.Ready, s.Desired)
		progressing.Status, progressing.Reason = metav1.ConditionTrue, "Rolling"
	}
	meta.SetStatusCondition(&cr.Status.Conditions, ready)
	meta.SetStatusCondition(&cr.Status.Conditions, progressing)
}

// driverDaemonSetName is deterministic per CR — re-applying the CR yields
// the same DS name (idempotent create).
func driverDaemonSetName(crName string) string {
	return "ttdrv-" + crName
}

func (r *DriverPolicyReconciler) buildDaemonSet(cr *driverv1alpha1.TenstorrentDriverPolicy, name string) *appsv1.DaemonSet {
	image := defaultDriverImage()
	pull := corev1.PullIfNotPresent
	if cr.Spec.Installer != nil {
		if cr.Spec.Installer.Image != "" {
			image = cr.Spec.Installer.Image
		}
		if cr.Spec.Installer.ImagePullPolicy != "" {
			pull = cr.Spec.Installer.ImagePullPolicy
		}
	}

	priv := true
	hostPathDir := corev1.HostPathDirectory
	hostPathDirOrCreate := corev1.HostPathDirectoryOrCreate
	// NOTE: app.kubernetes.io/name stays "tt-kmd-installer" so the DaemonSet
	// selector matches what existing clusters have on disk. DS selector is
	// immutable; changing this would break in-place upgrades from the
	// previous nsenter-based template. The container name + image reflect
	// the new builder reality.
	podLabels := map[string]string{
		"app.kubernetes.io/name":      "tt-kmd-installer",
		"app.kubernetes.io/component": "driver",
		"driver.tenstorrent.com/cr":   cr.Name,
	}

	// Merge spec.nodeAffinity with the NFD-presence requirement and a
	// DoesNotExist gate on LabelDriverSkip. Adding the skip label to a
	// running node makes the existing installer pod no longer match
	// nodeAffinity, so kubelet evicts it — the skip takes effect
	// immediately, not just on the next scheduling decision.
	nodeSelectorTerms := []corev1.NodeSelectorTerm{{
		MatchExpressions: append(
			labelSelectorToExpressions(cr.Spec.EffectiveNodeAffinity()),
			corev1.NodeSelectorRequirement{
				Key: LabelTenstorrentPresent, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"},
			},
			corev1.NodeSelectorRequirement{
				Key: LabelDriverSkip, Operator: corev1.NodeSelectorOpDoesNotExist,
			},
		),
	}}

	builderEnv := []corev1.EnvVar{
		{Name: "TT_KMD_VERSION", Value: cr.Spec.Version},
		// Builder pod needs to know which node it's on
		// to label that node post-detection.
		{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		// Surface spec.forceUnload to the entrypoint, which
		// gates the `fuser -k /dev/tenstorrent/*` escape
		// hatch when the loaded module's refcount > 0.
		{Name: "TT_FORCE_UNLOAD", Value: boolEnv(cr.Spec.UpgradePolicy.ForceUnload)},
	}
	// Propagate proxy env from the controller's own pod to the spawned
	// builder. On cache miss the builder git-clones tt-kmd source from
	// github.com; on clusters whose pod egress goes through a proxy
	// (e.g. CI behind squid), the clone fails without these set.
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"} {
		if v := os.Getenv(k); v != "" {
			builderEnv = append(builderEnv, corev1.EnvVar{Name: k, Value: v})
		}
	}

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: operatorNamespace(),
			Labels:    podLabels,
		},
		Spec: appsv1.DaemonSetSpec{
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: intStrPtr(1),
				},
			},
			Selector: &metav1.LabelSelector{MatchLabels: podLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					// No hostNetwork — the builder reads kernel state via its
					// own sysfs mount (kernel-shared) and invokes init_module(2)
					// / delete_module(2) directly with CAP_SYS_MODULE
					// (privileged=true). hostPID is set ONLY when forceUnload
					// is true: fuser walks /proc/<pid>/fd looking for device
					// holders and only sees the host's processes when the pod
					// shares the host's PID namespace. Without hostPID the
					// builder's /proc is just its own one process and fuser-k
					// is a no-op.
					HostPID:            cr.Spec.UpgradePolicy.ForceUnload,
					ServiceAccountName: envOrDefault("INSTALLER_SERVICE_ACCOUNT", "tt-k8s-driver-manager-installer"),
					Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: nodeSelectorTerms,
							},
						},
					},
					PriorityClassName: "system-node-critical",
					Containers: []corev1.Container{{
						Name:            "builder",
						Image:           image,
						ImagePullPolicy: pull,
						Env:             builderEnv,
						SecurityContext: &corev1.SecurityContext{Privileged: &priv},
						VolumeMounts: []corev1.VolumeMount{
							// Kernel build tree for headers. On Ubuntu the
							// /lib/modules/<kver>/build path symlinks into
							// /usr/src/linux-headers-<kver>, so we mount both.
							{Name: "lib-modules", MountPath: "/lib/modules", ReadOnly: true},
							{Name: "usr-src", MountPath: "/usr/src", ReadOnly: true},
							// Per-(kver, version) cache for the built .ko so
							// pod restarts don't repay the build cost.
							{Name: "tt-kmd-cache", MountPath: "/var/cache/tt-kmd"},
							// Read-only view of host DKMS state so the
							// entrypoint can detect a host-managed install
							// (e.g. tt-ansible's tt_kmd role) and stand
							// down — see install-mode label.
							{Name: "var-lib-dkms", MountPath: "/var/lib/dkms", ReadOnly: true},
							// Host /usr/local/bin for tt-smi delivery:
							// entrypoint copies the self-contained binary to
							// /host/usr/local/bin/tt-smi. /host/opt stays
							// mounted so it can remove the venv that pre-5.x
							// builders installed at /opt/tt.
							{Name: "host-opt", MountPath: "/host/opt"},
							{Name: "host-usr-local-bin", MountPath: "/host/usr/local/bin"},
							// /host/etc/udev/rules.d for staging tt-kmd's
							// upstream udev rule so /dev/tenstorrent/* land
							// with MODE=0666 (matches DKMS/apt install).
							// /host/dev for chmod-ing the device nodes that
							// devtmpfs already created at the default 0600.
							{Name: "host-udev-rules", MountPath: "/host/etc/udev/rules.d"},
							{Name: "host-dev", MountPath: "/host/dev"},
						},
						// Pod is Ready iff /sys/module reports the desired
						// version. The container has its own sysfs mount but
						// kernel state is shared, so no nsenter is needed.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{
										"sh", "-c",
										`cat /sys/module/tenstorrent/version 2>/dev/null | grep -qFx "$TT_KMD_VERSION"`,
									},
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
							FailureThreshold:    30,
							SuccessThreshold:    1,
							TimeoutSeconds:      5,
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "lib-modules", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules", Type: &hostPathDir}}},
						{Name: "usr-src", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/usr/src", Type: &hostPathDir}}},
						{Name: "tt-kmd-cache", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/cache/tt-kmd", Type: &hostPathDirOrCreate}}},
						{Name: "var-lib-dkms", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/dkms", Type: &hostPathDirOrCreate}}},
						{Name: "host-opt", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/opt", Type: &hostPathDirOrCreate}}},
						{Name: "host-usr-local-bin", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/usr/local/bin", Type: &hostPathDirOrCreate}}},
						{Name: "host-udev-rules", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/udev/rules.d", Type: &hostPathDirOrCreate}}},
						{Name: "host-dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev", Type: &hostPathDir}}},
					},
				},
			},
		},
	}
	// Owner reference so CR delete cascades to DS delete.
	gvk := driverv1alpha1.GroupVersion.WithKind("TenstorrentDriverPolicy")
	controller := true
	blockOwnerDeletion := true
	ds.OwnerReferences = []metav1.OwnerReference{{
		APIVersion:         gvk.GroupVersion().String(),
		Kind:               gvk.Kind,
		Name:               cr.Name,
		UID:                cr.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}}
	// Hash AFTER all spec fields are set; annotation is the drift sentinel.
	if ds.Annotations == nil {
		ds.Annotations = map[string]string{}
	}
	ds.Annotations[dsTemplateHashAnnotation] = computeDSTemplateHash(ds)
	return ds
}

func labelSelectorToExpressions(sel metav1.LabelSelector) []corev1.NodeSelectorRequirement {
	out := make([]corev1.NodeSelectorRequirement, 0, len(sel.MatchLabels)+len(sel.MatchExpressions))
	for k, v := range sel.MatchLabels {
		out = append(out, corev1.NodeSelectorRequirement{
			Key: k, Operator: corev1.NodeSelectorOpIn, Values: []string{v},
		})
	}
	for _, e := range sel.MatchExpressions {
		var op corev1.NodeSelectorOperator
		switch e.Operator {
		case metav1.LabelSelectorOpIn:
			op = corev1.NodeSelectorOpIn
		case metav1.LabelSelectorOpNotIn:
			op = corev1.NodeSelectorOpNotIn
		case metav1.LabelSelectorOpExists:
			op = corev1.NodeSelectorOpExists
		case metav1.LabelSelectorOpDoesNotExist:
			op = corev1.NodeSelectorOpDoesNotExist
		}
		out = append(out, corev1.NodeSelectorRequirement{
			Key: e.Key, Operator: op, Values: e.Values,
		})
	}
	return out
}

func intStrPtr(i int) *intstr.IntOrString {
	v := intstr.FromInt(i)
	return &v
}

// computeDSTemplateHash hashes the pod template + update strategy
// fields buildDaemonSet sets. Returned hash is stamped as an annotation
// on the DS so future reconciles can detect drift without a brittle
// field-by-field comparison.
func computeDSTemplateHash(ds *appsv1.DaemonSet) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode(ds.Spec.Template.Spec)
	_ = enc.Encode(ds.Spec.UpdateStrategy)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// daemonSetNeedsUpdate compares only the bits we manage so unrelated
// kubelet/controller writes don't trigger a rolling restart.
func daemonSetNeedsUpdate(existing, desired *appsv1.DaemonSet) bool {
	return existing.Annotations[dsTemplateHashAnnotation] != desired.Annotations[dsTemplateHashAnnotation]
}

func defaultDriverImage() string {
	if v := envOrDefault("DRIVER_IMAGE", ""); v != "" {
		return v
	}
	// The chart always wires DRIVER_IMAGE via env. This fallback is a
	// sane-default for ad-hoc `go run` / envtest paths only — match the
	// builder image since the nsenter installer no longer exists.
	return "ghcr.io/tenstorrent/tt-k8s-driver-manager-builder:dev"
}

func (r *DriverPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&driverv1alpha1.TenstorrentDriverPolicy{}).
		Owns(&appsv1.DaemonSet{}).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.mapNodeToDriverCRs),
			builder.WithPredicates(),
		).
		Complete(r)
}

func (r *DriverPolicyReconciler) mapNodeToDriverCRs(ctx context.Context, _ client.Object) []ctrl.Request {
	var list driverv1alpha1.TenstorrentDriverPolicyList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "list TenstorrentDriverPolicies for node event")
		return nil
	}
	out := make([]ctrl.Request, 0, len(list.Items))
	for _, cr := range list.Items {
		out = append(out, ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
	}
	return out
}
