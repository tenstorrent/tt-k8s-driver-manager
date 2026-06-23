package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	driverv1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/driver/v1alpha1"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/drain"
)

// unmanageJobLabelCR is the label set on every unload Job so we can list
// them per-CR (parallel to the JobLabelCR used by the firmware
// controller — we keep a separate key so the two controllers don't fight
// over a shared label space).
const unmanageJobLabelCR = "driver.tenstorrent.com/unmanage-cr"

// unmanageJobLabelNode lets us pull a per-node Job by selector without
// reconstructing the deterministic name.
const unmanageJobLabelNode = "driver.tenstorrent.com/unmanage-node"

// reconcileUnmanageFlow is the spec.unmanage=true branch of Reconcile.
// It runs the per-node vacate state machine, updates status, deletes the
// managed DaemonSet once every in-scope node is terminal, and requeues
// while work is in flight. Separate from the normal reconcile so the
// DaemonSet path is completely bypassed — we don't want a half-vacated
// fleet getting a refreshed DS template.
func (r *DriverPolicyReconciler) reconcileUnmanageFlow(
	ctx context.Context, cr *driverv1alpha1.TenstorrentDriverPolicy,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("flow", "unmanage")

	matched, err := r.countMatchedNodes(ctx, cr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("count matched nodes: %w", err)
	}

	allTerminal, anyFailed, err := r.reconcileUnmanage(ctx, cr)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Drop the DaemonSet only after every in-scope node is terminal AND
	// no node landed in UnloadFailed. The strict halt mode keeps the DS
	// around on partial failure so any nodes still labelled
	// install-mode=container retain their builder pod's idle/standby
	// state — we don't want a stuck node to lose its DS pod and become
	// unrecoverable from the operator's side.
	if allTerminal && !anyFailed {
		if err := r.deleteManagedDaemonSet(ctx, cr); err != nil {
			return ctrl.Result{}, err
		}
		cr.Status.Phase = driverv1alpha1.DriverPolicyPhaseUnmanaged
	} else {
		cr.Status.Phase = ""
	}

	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.DesiredVersion = cr.Spec.Version
	cr.Status.Summary.Matched = matched
	// During unmanage the DaemonSet is being torn down — clear the
	// summary counters that read from it so dashboards don't show stale
	// Ready/Desired counts against a deleted DS.
	cr.Status.Summary.Desired = 0
	cr.Status.Summary.Ready = 0
	cr.Status.Summary.Available = 0
	cr.Status.Summary.Failed = 0
	cr.Status.Summary.UpToDate = 0
	cr.Status.Summary.InProgress = 0
	for _, ns := range cr.Status.Nodes {
		switch ns.State {
		case driverv1alpha1.DriverNodeStateUnmanaged:
			cr.Status.Summary.UpToDate++
		case driverv1alpha1.DriverNodeStateUnloadFailed:
			cr.Status.Summary.Failed++
		case driverv1alpha1.DriverNodeStateCordoning,
			driverv1alpha1.DriverNodeStateDraining,
			driverv1alpha1.DriverNodeStateUnloading:
			cr.Status.Summary.InProgress++
		}
	}
	setUnmanageConditions(cr, allTerminal, anyFailed)

	if err := r.Status().Update(ctx, cr); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	if !allTerminal {
		// Job watches don't fire fast enough on Job-Completed transitions
		// in every flavour of envtest/cluster; 15s requeue mirrors the
		// upgrade path's rollout-in-flight cadence.
		logger.Info("unmanage in flight; requeueing",
			"matched", matched, "failed", anyFailed)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// deleteManagedDaemonSet tears down the DS this CR owns. Safe to call
// when the DS is already gone (NotFound is squashed). Called after every
// in-scope node is Unmanaged so the operator-managed install fully
// vacates.
func (r *DriverPolicyReconciler) deleteManagedDaemonSet(
	ctx context.Context, cr *driverv1alpha1.TenstorrentDriverPolicy,
) error {
	ds := &appsv1.DaemonSet{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: operatorNamespace(),
		Name:      driverDaemonSetName(cr.Name),
	}, ds)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get daemonset for unmanage teardown: %w", err)
	}
	if err := r.Delete(ctx, ds); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete daemonset: %w", err)
	}
	log.FromContext(ctx).Info("deleted managed DaemonSet (unmanage terminal)",
		"name", ds.Name)
	return nil
}

// setUnmanageConditions overwrites the standard Ready/Progressing
// conditions with unmanage-flow-specific reasons. Keeps the same two
// condition types (Ready, Progressing) so dashboards keying off them
// don't need a special case.
func setUnmanageConditions(
	cr *driverv1alpha1.TenstorrentDriverPolicy, allTerminal, anyFailed bool,
) {
	ready := metav1.Condition{
		Type:               "Ready",
		ObservedGeneration: cr.Generation,
		LastTransitionTime: metav1.Now(),
	}
	progressing := metav1.Condition{
		Type:               "Progressing",
		ObservedGeneration: cr.Generation,
		LastTransitionTime: metav1.Now(),
	}
	switch {
	case anyFailed:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "UnloadFailed"
		ready.Message = fmt.Sprintf("%d node(s) failed to unload tt-kmd", cr.Status.Summary.Failed)
		progressing.Status = metav1.ConditionFalse
		progressing.Reason = "Halted"
	case allTerminal:
		ready.Status = metav1.ConditionTrue
		ready.Reason = "Unmanaged"
		ready.Message = "all in-scope nodes vacated for external KMD management"
		progressing.Status = metav1.ConditionFalse
		progressing.Reason = "Idle"
	default:
		ready.Status = metav1.ConditionFalse
		ready.Reason = "Unmanaging"
		ready.Message = "vacating in-scope nodes"
		progressing.Status = metav1.ConditionTrue
		progressing.Reason = "Unmanaging"
	}
	meta.SetStatusCondition(&cr.Status.Conditions, ready)
	meta.SetStatusCondition(&cr.Status.Conditions, progressing)
}

// reconcileUnmanage is the entry point for spec.unmanage=true. It drives
// each in-scope node through Cordoning → Draining → Unloading → Unmanaged
// (or → UnloadFailed on a stuck node). Returns true when every matched
// node has reached a terminal state (Unmanaged or UnloadFailed) so the
// caller can flip .status.phase = Unmanaged and skip the DaemonSet path
// entirely.
//
// Failure mode is strict: as soon as one node lands in UnloadFailed the
// controller stops spawning new unload Jobs. The operator fixes the
// stuck node (typically: tracks down whatever still holds
// /dev/tenstorrent, kills it) and the next reconcile picks up where it
// left off. Rolling-through-failures would silently leave a mixed
// cluster — half the fleet vacated, half still operator-managed — which
// is harder to reason about than a halted batch with a per-node reason.
func (r *DriverPolicyReconciler) reconcileUnmanage(
	ctx context.Context, cr *driverv1alpha1.TenstorrentDriverPolicy,
) (allTerminal bool, anyFailed bool, err error) {
	logger := log.FromContext(ctx)

	// Only act on nodes the operator actually installed onto — i.e.
	// install-mode=container. host-managed nodes are already vacated by
	// definition; touching them would fight the host's own state.
	nodes, err := r.listMatchedNodes(ctx, cr)
	if err != nil {
		return false, false, fmt.Errorf("list matched nodes: %w", err)
	}
	inScope := make([]corev1.Node, 0, len(nodes))
	for i := range nodes {
		if nodes[i].Labels[LabelInstallMode] == "container" {
			inScope = append(inScope, nodes[i])
		}
	}

	// Walk through existing per-node status to detect a prior failure
	// (strict mode: halt on first UnloadFailed). Default to false; the
	// caller will surface anyFailed via setDriverConditions.
	prevByNode := map[string]driverv1alpha1.DriverNodeStatus{}
	for _, ns := range cr.Status.Nodes {
		prevByNode[ns.Name] = ns
	}
	for _, ns := range prevByNode {
		if ns.State == driverv1alpha1.DriverNodeStateUnloadFailed {
			anyFailed = true
			break
		}
	}

	// Each pass advances one node by one step. The reconcile is requeued
	// (see Reconcile) while any node is still in flight.
	for i := range inScope {
		node := &inScope[i]
		if anyFailed {
			// Strict halt: don't start any new work after a failure.
			break
		}
		if err := r.advanceUnmanage(ctx, cr, node); err != nil {
			logger.Error(err, "advance unmanage", "node", node.Name)
			// continue — other nodes still progress; this node will be
			// retried on the next reconcile.
		}
		// Re-check failure after this step in case advanceUnmanage just
		// transitioned the node into UnloadFailed — strict halt prevents
		// us from starting fresh work on a later node in this same pass.
		if cur := findNodeStatus(cr.Status.Nodes, node.Name); cur != nil &&
			cur.State == driverv1alpha1.DriverNodeStateUnloadFailed {
			anyFailed = true
		}
	}

	// Terminal-check: every in-scope node is Unmanaged (success) or
	// UnloadFailed (stuck). Empty in-scope counts as terminal too — there
	// was nothing to vacate.
	allTerminal = true
	for i := range inScope {
		ns := findNodeStatus(cr.Status.Nodes, inScope[i].Name)
		if ns == nil {
			allTerminal = false
			break
		}
		if ns.State != driverv1alpha1.DriverNodeStateUnmanaged &&
			ns.State != driverv1alpha1.DriverNodeStateUnloadFailed {
			allTerminal = false
			break
		}
	}
	return allTerminal, anyFailed, nil
}

// advanceUnmanage moves one node one step closer to Unmanaged. The state
// machine is observed-from-cluster-state (like the upgrade path): we
// look at cordon ownership + the unload Job's status and decide the next
// transition. Idempotent — re-running on a node mid-flight is a no-op.
func (r *DriverPolicyReconciler) advanceUnmanage(
	ctx context.Context, cr *driverv1alpha1.TenstorrentDriverPolicy, node *corev1.Node,
) error {
	logger := log.FromContext(ctx)

	// If the node is already past install-mode=container (i.e. we
	// already removed the label on a prior reconcile), it's Unmanaged.
	if node.Labels[LabelInstallMode] != "container" {
		r.setUnmanageNodeStatus(cr, node.Name, driverv1alpha1.DriverNodeStateUnmanaged,
			"Unmanaged", "tt-kmd unloaded; host vacated for external management")
		return nil
	}

	// Look up the per-node unload Job. Determines whether we're
	// pre-drain, mid-unload, or post-unload.
	job := &batchv1.Job{}
	jobErr := r.Get(ctx, types.NamespacedName{
		Namespace: operatorNamespace(),
		Name:      unloadJobName(cr, node.Name),
	}, job)
	jobExists := jobErr == nil
	if jobErr != nil && !apierrors.IsNotFound(jobErr) {
		return fmt.Errorf("get unload job: %w", jobErr)
	}

	if jobExists {
		completed, succeeded := jobFinished(job)
		switch {
		case completed && succeeded:
			return r.finalizeUnmanage(ctx, cr, node)
		case completed && !succeeded:
			msg := unloadFailureMessage(job)
			r.setUnmanageNodeStatus(cr, node.Name,
				driverv1alpha1.DriverNodeStateUnloadFailed,
				"UnloadFailed", msg)
			if r.Recorder != nil {
				r.Recorder.Eventf(cr, corev1.EventTypeWarning, "UnloadFailed",
					"Node %s: tt-kmd unload failed: %s", node.Name, msg)
			}
			logger.Info("unload failed; node stays cordoned for operator intervention",
				"node", node.Name, "message", msg)
			return nil
		default:
			r.setUnmanageNodeStatus(cr, node.Name, driverv1alpha1.DriverNodeStateUnloading,
				"Unloading", "unload Job in flight")
			return nil
		}
	}

	// No Job yet — walk through the pre-unload gates (cordon, drain).
	weCordoned := node.Annotations[AnnoDriverCordonedBy] == cr.Name
	if !node.Spec.Unschedulable {
		// Cordon first. Don't spawn the unload Job until we own a fresh
		// cordon so refcnt has a chance to drop while we drain.
		if err := drain.CordonNode(ctx, r.Client, node, driverCordonOpts(cr.Name)); err != nil {
			return fmt.Errorf("cordon: %w", err)
		}
		r.setUnmanageNodeStatus(cr, node.Name, driverv1alpha1.DriverNodeStateCordoning,
			"Cordoning", "cordoned for unload")
		return nil
	}
	if !weCordoned {
		// External cordon. Refuse to act — we don't know what the
		// operator cordoned the node for, and stepping on it could
		// surprise an unrelated maintenance window.
		r.setUnmanageNodeStatus(cr, node.Name, driverv1alpha1.DriverNodeStatePending,
			"ExternalCordon", MessageExternalCordon)
		return nil
	}

	// We cordoned. Drain /dev/tenstorrent holders. Mirrors prepareUpgrade
	// but synchronously per-node so we can decide when refcnt has dropped.
	force := cr.Spec.UpgradePolicy.Drain.Force
	pods, err := drain.ListDevicePodsOnNode(
		ctx, r.Client, node.Name, operatorNamespace(), force, drain.PodUsesTenstorrentDevice,
	)
	if err != nil {
		return fmt.Errorf("list device pods: %w", err)
	}
	if len(pods) > 0 {
		// Best-effort eviction; PDB-blocked pods come back next reconcile.
		// Also flip deploy gates off so sibling DSes (tt-telemetry etc.)
		// vacate themselves — same cooperative-drain trick as the upgrade
		// path.
		if err := flipDeployGatesOff(ctx, r.Client, node); err != nil {
			logger.Error(err, "flip deploy gates off", "node", node.Name)
		}
		for j := range pods {
			if err := drain.EvictPod(ctx, r.Client, &pods[j]); err != nil {
				if _, blocked := err.(drain.ErrEvictionBlocked); blocked {
					logger.Info("eviction blocked by PDB",
						"pod", pods[j].Namespace+"/"+pods[j].Name)
					continue
				}
				logger.Error(err, "evict pod",
					"pod", pods[j].Namespace+"/"+pods[j].Name)
			}
		}
		r.setUnmanageNodeStatus(cr, node.Name, driverv1alpha1.DriverNodeStateDraining,
			"Draining", fmt.Sprintf("draining %d /dev/tenstorrent holder(s)", len(pods)))
		return nil
	}

	// Cordoned + drained — spawn the unload Job. Job runs to completion;
	// the next reconcile sees its terminal status and proceeds to
	// finalizeUnmanage on success or UnloadFailed status on failure.
	if err := r.spawnUnloadJob(ctx, cr, node); err != nil {
		return fmt.Errorf("spawn unload job: %w", err)
	}
	r.setUnmanageNodeStatus(cr, node.Name, driverv1alpha1.DriverNodeStateUnloading,
		"Unloading", "unload Job spawned")
	return nil
}

// finalizeUnmanage runs the post-success steps for one node:
//   - remove install-mode=container label (so the builder DS skips this
//     node — the chart's NotIn["container"] semantics aren't in play, but
//     we do read the label to decide which nodes are in-scope for
//     unmanage in the first place; clearing it makes the node invisible
//     to future unmanage passes).
//   - emit a k8s Event for fleet-management dashboards.
//   - uncordon so workloads can land back (presumably the operator is
//     about to install DKMS in parallel).
//   - mark the node Unmanaged in status.
func (r *DriverPolicyReconciler) finalizeUnmanage(
	ctx context.Context, cr *driverv1alpha1.TenstorrentDriverPolicy, node *corev1.Node,
) error {
	logger := log.FromContext(ctx)

	// Label removal. Patch from a deep-copy snapshot so we only send a
	// single-field delta.
	if node.Labels[LabelInstallMode] == "container" {
		patch := client.MergeFrom(node.DeepCopy())
		delete(node.Labels, LabelInstallMode)
		if err := r.Patch(ctx, node, patch); err != nil {
			return fmt.Errorf("remove install-mode label: %w", err)
		}
		logger.Info("removed install-mode=container label", "node", node.Name)
	}

	// Uncordon. UncordonNode is a no-op when we don't own the cordon.
	if err := drain.UncordonNode(ctx, r.Client, node, driverCordonOpts(cr.Name)); err != nil {
		// Non-fatal — operator can uncordon manually. Still mark Unmanaged
		// because the host IS vacated; the cordon is just leftover.
		logger.Error(err, "uncordon node", "node", node.Name)
	}

	// Restore deploy gates we flipped off during the drain pass.
	if err := removeDeployGates(ctx, r.Client, node); err != nil {
		logger.Error(err, "remove deploy gates", "node", node.Name)
	}

	r.setUnmanageNodeStatus(cr, node.Name, driverv1alpha1.DriverNodeStateUnmanaged,
		"Unmanaged", "tt-kmd unloaded; host can now manage via DKMS")
	if r.Recorder != nil {
		r.Recorder.Eventf(cr, corev1.EventTypeNormal, "Unmanaged",
			"Node %s: tt-kmd unloaded; host can now manage via DKMS", node.Name)
	}
	return nil
}

// setUnmanageNodeStatus writes a NodeStatus entry for the given node in
// cr.Status.Nodes. Idempotent on identical state — preserves
// LastTransitionTime when state hasn't changed so dashboards don't see
// constant churn.
func (r *DriverPolicyReconciler) setUnmanageNodeStatus(
	cr *driverv1alpha1.TenstorrentDriverPolicy,
	name string,
	state driverv1alpha1.DriverNodeState,
	reason, message string,
) {
	idx := idxOfNode(cr.Status.Nodes, name)
	if idx == -1 {
		cr.Status.Nodes = append(cr.Status.Nodes, driverv1alpha1.DriverNodeStatus{
			Name:               name,
			State:              state,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		})
		return
	}
	prev := &cr.Status.Nodes[idx]
	if prev.State == state && prev.Reason == reason {
		// Update message only — keep the transition timestamp.
		prev.Message = message
		return
	}
	prev.State = state
	prev.Reason = reason
	prev.Message = message
	prev.LastTransitionTime = metav1.Now()
}

func findNodeStatus(nodes []driverv1alpha1.DriverNodeStatus, name string) *driverv1alpha1.DriverNodeStatus {
	if idx := idxOfNode(nodes, name); idx >= 0 {
		return &nodes[idx]
	}
	return nil
}

func idxOfNode(nodes []driverv1alpha1.DriverNodeStatus, name string) int {
	for i := range nodes {
		if nodes[i].Name == name {
			return i
		}
	}
	return -1
}

// unloadJobName is deterministic per (CR, node, "unload" tag). Same
// shape as firmware/job.go's jobName: readable prefix capped, hashed
// suffix for uniqueness when the prefix gets truncated.
func unloadJobName(cr *driverv1alpha1.TenstorrentDriverPolicy, nodeName string) string {
	prefix := fmt.Sprintf("ttunload-%s-%s", cr.Name, nodeName)
	if len(prefix) > 56 {
		prefix = prefix[:56]
	}
	h := sha256.Sum256([]byte(cr.Name + "/" + nodeName + "/unload"))
	suffix := hex.EncodeToString(h[:3])
	return strings.ToLower(fmt.Sprintf("%s-%s", prefix, suffix))
}

// spawnUnloadJob creates the per-node unload Job. Mirrors the firmware
// buildFlashJob shape: one-shot Job, BackoffLimit=0 (a failed unload is
// a real failure — don't retry against a still-holding refcnt), TTL so
// finished Jobs don't pile up, privileged container with hostPID so
// rmmod sees host kernel state.
//
// Idempotent on AlreadyExists: the next reconcile will see the existing
// Job and react to its terminal status. Pre-existing Jobs from a prior
// (failed) attempt are NOT deleted here — operator intervention reaches
// for `kubectl delete job` when they want a fresh attempt.
func (r *DriverPolicyReconciler) spawnUnloadJob(
	ctx context.Context, cr *driverv1alpha1.TenstorrentDriverPolicy, node *corev1.Node,
) error {
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
	backoff := int32(0)
	ttl := int32(86400)
	// Hard deadline on the rmmod step. The unload itself is fast; the
	// drain in front of it is the slow part, and that's handled by the
	// controller before the Job exists.
	timeout := int64(120)

	// The shell stays inline so the chart doesn't need a second image
	// just for unload. The builder image already has /bin/sh, find,
	// rmmod (kmod), depmod — same toolchain it uses to install.
	script := strings.Join([]string{
		`set -euo pipefail`,
		`[ "$(cat /sys/module/tenstorrent/refcnt)" = "0" ] \`,
		`  || { echo "FAIL: refcnt > 0; something is holding /dev/tenstorrent"; exit 1; }`,
		`rmmod tenstorrent`,
		`find /lib/modules/$(uname -r) -name 'tenstorrent.ko*' -delete`,
		`depmod -a`,
		`! [ -e /sys/module/tenstorrent/version ]`,
		`echo "OK: tt-kmd unloaded; host vacated for DKMS"`,
	}, "\n")

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      unloadJobName(cr, node.Name),
			Namespace: operatorNamespace(),
			Labels: map[string]string{
				unmanageJobLabelCR:   cr.Name,
				unmanageJobLabelNode: node.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &timeout,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						unmanageJobLabelCR:   cr.Name,
						unmanageJobLabelNode: node.Name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					HostPID:       true,
					NodeName:      node.Name,
					Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					// Reuse the installer SA so we don't have to thread a
					// new RBAC subject through the chart; the Job only
					// rmmods + writes to /lib/modules on the host (via the
					// privileged container's hostPath), no apiserver calls.
					ServiceAccountName: envOrDefault("INSTALLER_SERVICE_ACCOUNT", "tt-k8s-driver-manager-installer"),
					Containers: []corev1.Container{{
						Name:            "unload",
						Image:           image,
						ImagePullPolicy: pull,
						Command:         []string{"sh", "-c", script},
						SecurityContext: &corev1.SecurityContext{Privileged: &priv},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "lib-modules", MountPath: "/lib/modules"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "lib-modules", VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules", Type: &hostPathDir},
						}},
					},
				},
			},
		},
	}

	// Owner ref so Job deletion follows CR deletion.
	gvk := driverv1alpha1.GroupVersion.WithKind("TenstorrentDriverPolicy")
	controllerRef := true
	blockOwnerDeletion := true
	job.OwnerReferences = []metav1.OwnerReference{{
		APIVersion:         gvk.GroupVersion().String(),
		Kind:               gvk.Kind,
		Name:               cr.Name,
		UID:                cr.UID,
		Controller:         &controllerRef,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}}

	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create unload job: %w", err)
	}
	log.FromContext(ctx).Info("spawned unload Job",
		"cr", cr.Name, "node", node.Name, "job", job.Name)
	return nil
}

// unloadFailureMessage pulls a one-line failure hint from the unload Job
// for the per-node status. We don't reach back into pod logs from the
// controller (more machinery than this is worth — operators read logs
// via `kubectl logs job/<name>`), so we surface the Job's own failure
// condition message. Empty falls back to a stock message.
func unloadFailureMessage(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Message != "" {
				return c.Message
			}
		}
	}
	return "unload Job failed; see Job pod logs"
}
