package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	firmwarev1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/firmware/v1alpha1"
)

// FirmwarePolicyReconciler reconciles TenstorrentFirmwarePolicy resources.
//
// Reconciliation model: per-CR, the controller walks each matching node and
// decides whether to spawn a flash Job. The source of truth for "where is
// this node in the upgrade" is the Job's existence + status; the
// firmware.tenstorrent.com/upgrade-state label is a denormalized view for
// human kubectl-grepping, not read back by the controller. Per-node Job
// names are deterministic (jobName) so creation is idempotent.
type FirmwarePolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=firmware.tenstorrent.com,resources=tenstorrentfirmwarepolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=firmware.tenstorrent.com,resources=tenstorrentfirmwarepolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

func (r *FirmwarePolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("ttfwp", req.Name)

	cr := &firmwarev1alpha1.TenstorrentFirmwarePolicy{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !cr.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	matched, err := r.matchedNodes(ctx, cr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list matched nodes: %w", err)
	}

	// Snapshot owner conflicts: a node owned by another CR is excluded and
	// surfaced as a Pending entry in status with a Conflict message.
	owned, conflicts := r.partitionByOwnership(matched, cr.Name)

	autoUpgrade := cr.Spec.UpgradePolicy.AutoUpgrade == nil || *cr.Spec.UpgradePolicy.AutoUpgrade
	maxParallel := int(cr.Spec.UpgradePolicy.MaxParallel)
	if maxParallel == 0 {
		maxParallel = 1
	}

	nodeStates := make([]firmwarev1alpha1.NodeStatus, 0, len(owned)+len(conflicts))

	// First pass: observe current state without mutating cluster state.
	now := metav1.Now()
	for _, node := range owned {
		ns := r.observeNode(ctx, cr, node)
		ns.LastTransitionTime = now
		nodeStates = append(nodeStates, ns)
	}
	for _, c := range conflicts {
		nodeStates = append(nodeStates, firmwarev1alpha1.NodeStatus{
			Name:               c.Name,
			State:              firmwarev1alpha1.NodeStatePending,
			Message:            MessageNodeConflict,
			LastTransitionTime: now,
		})
	}

	// HaltOnFailure (default true): if any matched node has already failed,
	// don't spawn new work — let the existing status path mark Progressing=Halted.
	// The summary.Failed > 0 branch in setTopLevelConditions handles the message.
	haltOnFailure := cr.Spec.UpgradePolicy.HaltOnFailure == nil || *cr.Spec.UpgradePolicy.HaltOnFailure
	halted := false
	if haltOnFailure {
		for _, ns := range nodeStates {
			if ns.State == firmwarev1alpha1.NodeStateFailed {
				logger.Info("halting rollout on first node failure", "node", ns.Name)
				halted = true
				break
			}
		}
	}

	// Count in-flight Jobs by listing the Jobs we own (label JobLabelCR=cr.Name)
	// and filtering out finished ones. Using the live Job list — rather than
	// the per-node observed state we just derived — keeps maxParallel honest
	// even if a previous reconcile created a Job but failed before recording
	// it in node state, or if observed state is otherwise out of sync. This
	// is the source of truth for "how many slots are currently consumed."
	inFlight, err := r.countInFlightJobs(ctx, cr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("count in-flight jobs: %w", err)
	}

	// Second pass: advance up to (maxParallel - inFlight) nodes through
	// the next state transition. Advanceable states (per isAdvanceable):
	// Pending starts work; Cordoning / Draining / Uncordoning continue
	// in-flight work that's already counted against capacity. Flashing
	// waits on the Job; Done / Failed are terminal.
	capacity := maxParallel - inFlight
	for i := range nodeStates {
		if !autoUpgrade || cr.Spec.Paused || halted {
			break
		}
		ns := &nodeStates[i]
		if !isAdvanceable(ns.State) {
			continue
		}
		// Gate on observed-state messages that mean "don't touch this":
		// another CR owns it, or someone else cordoned it.
		if ns.Message == MessageNodeConflict || ns.Message == MessageExternalCordon {
			continue
		}
		// Only Pending consumes a fresh slot; in-flight states proceed
		// regardless because they already count.
		startingFresh := ns.State == firmwarev1alpha1.NodeStatePending
		if startingFresh && capacity <= 0 {
			continue
		}
		node := findNode(owned, ns.Name)
		if node == nil {
			continue
		}
		advanced, advErr := r.advanceNode(ctx, cr, node, ns)
		// Decrement capacity if the slot is now spent — either advanceNode
		// reported it (advanced=true) or spawnFlashJob marked ns.State =
		// Flashing after a successful Create even though a downstream patch
		// errored. Crucially, run this BEFORE the err-continue so a failed
		// patch can't leak the slot back into the pool.
		if startingFresh && (advanced || ns.State == firmwarev1alpha1.NodeStateFlashing) {
			capacity--
		}
		if advErr != nil {
			logger.Error(advErr, "advance node", "node", ns.Name)
			continue
		}
	}

	// Aggregate summary from final per-node states (post-advance) and keep the
	// node's upgrade-state label fresh. The label is a denormalized view for
	// `kubectl get nodes -L firmware.tenstorrent.com/upgrade-state`; the CR's
	// status is the source of truth, but the label is what humans grep.
	summary := firmwarev1alpha1.StatusSummary{Matched: int32(len(nodeStates))}
	for _, ns := range nodeStates {
		switch {
		case ns.State == firmwarev1alpha1.NodeStateDone:
			summary.UpToDate++
		case ns.State == firmwarev1alpha1.NodeStateFailed:
			summary.Failed++
		case isInFlight(ns.State):
			// Cordoning / Draining / Flashing / Uncordoning all show up
			// as InProgress in the summary — the per-node array carries
			// the fine-grained state for anyone who wants it.
			summary.InProgress++
		default:
			summary.Pending++
		}
		if node := findNode(owned, ns.Name); node != nil {
			cur := node.Labels[LabelUpgradeState]
			if cur != string(ns.State) {
				_ = r.labelNode(ctx, node, map[string]string{LabelUpgradeState: string(ns.State)})
			}
			if ns.State == firmwarev1alpha1.NodeStateDone {
				// Readback version = what tt-smi reports after a successful
				// flash. Default is "<spec.version>.0" — same convention as
				// tt-ansible. Label is for selectors; annotation is a
				// human-readable history pointer.
				readback := cr.Spec.ReadbackVersion
				if readback == "" {
					readback = cr.Spec.Version + ".0"
				}
				if node.Annotations[AnnoCurrentVersion] != readback {
					_ = r.annotateNode(ctx, node, map[string]string{AnnoCurrentVersion: readback})
				}
				if node.Labels[LabelFWVersion] != readback {
					_ = r.labelNode(ctx, node, map[string]string{LabelFWVersion: readback})
				}
			}
		}
	}

	// Sort for stable status output.
	sort.Slice(nodeStates, func(i, j int) bool { return nodeStates[i].Name < nodeStates[j].Name })

	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.DesiredVersion = cr.Spec.Version
	cr.Status.Summary = summary
	cr.Status.Nodes = nodeStates
	setTopLevelConditions(cr, summary)

	if err := r.Status().Update(ctx, cr); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	// Requeue while any node is mid-flight. Jobs trigger watches anyway, but a
	// short fallback covers cases where Job conditions land but no event fires.
	if summary.InProgress > 0 || summary.Pending > 0 {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// matchedNodes returns nodes matching the CR's nodeSelector AND the
// Tenstorrent NFD label (unless REQUIRE_TT_PCI_LABEL=false).
// Nodes carrying firmware.tenstorrent.com/skip=true are excluded.
func (r *FirmwarePolicyReconciler) matchedNodes(ctx context.Context, cr *firmwarev1alpha1.TenstorrentFirmwarePolicy) ([]corev1.Node, error) {
	sel, err := metav1.LabelSelectorAsSelector(&cr.Spec.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid nodeSelector: %w", err)
	}

	var all corev1.NodeList
	if err := r.List(ctx, &all); err != nil {
		return nil, err
	}

	out := make([]corev1.Node, 0, len(all.Items))
	for _, n := range all.Items {
		l := labels.Set(n.Labels)
		if !sel.Matches(l) {
			continue
		}
		if l.Get(LabelSkip) == "true" {
			continue
		}
		if requireTenstorrentLabel() && l.Get(LabelTenstorrentPresent) != "true" {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// partitionByOwnership splits nodes into (ours, conflicts). A node is "ours"
// when the LabelOwnerCR is unset OR equals this CR's name. If two CRs match the
// same node, whichever gets there first claims ownership; the other reports
// Conflict until the operator splits the selectors.
func (r *FirmwarePolicyReconciler) partitionByOwnership(nodes []corev1.Node, crName string) (owned, conflicts []corev1.Node) {
	for _, n := range nodes {
		owner := n.Labels[LabelOwnerCR]
		if owner == "" || owner == crName {
			owned = append(owned, n)
		} else {
			conflicts = append(conflicts, n)
		}
	}
	return
}

// observeNode reads the current state of a node from cluster state — Job
// existence + status, node.Spec.Unschedulable, and (when drain is enabled)
// the presence of device-using pods.
//
// The state machine, drain enabled:
//
//	Pending → Cordoning → Draining → Flashing → Uncordoning → Done
//	                                                       ↘ Failed
//
// Drain disabled collapses to Pending → Flashing → Done. Each transition
// is derived from observable cluster state; the upgrade-state label is
// denormalized for kubectl-grepping but not read back.
//
// A transient API-server error (anything other than NotFound) is treated
// as Pending-with-message — never Failed — so a blip doesn't flip the
// top-level Ready condition.
func (r *FirmwarePolicyReconciler) observeNode(ctx context.Context, cr *firmwarev1alpha1.TenstorrentFirmwarePolicy, node corev1.Node) firmwarev1alpha1.NodeStatus {
	ns := firmwarev1alpha1.NodeStatus{
		Name:           node.Name,
		CurrentVersion: node.Annotations[AnnoCurrentVersion],
		LastFlashJob:   node.Annotations[AnnoLastFlashJob],
	}

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: operatorNamespace(),
		Name:      jobName(cr, node.Name, cr.Spec.Version),
	}, job)

	jobExists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		ns.State = firmwarev1alpha1.NodeStatePending
		ns.Message = fmt.Sprintf("transient: lookup job: %v", err)
		return ns
	}
	if jobExists {
		ns.LastFlashJob = job.Name
		completed, succeeded := jobFinished(job)
		switch {
		case completed && succeeded:
			// Flash succeeded. If we cordoned this node, we still owe an
			// uncordon before declaring Done.
			if drainEnabled(cr) && node.Annotations[AnnoCordonedBy] == cr.Name {
				ns.State = firmwarev1alpha1.NodeStateUncordoning
			} else {
				ns.State = firmwarev1alpha1.NodeStateDone
			}
			return ns
		case completed && !succeeded:
			ns.State = firmwarev1alpha1.NodeStateFailed
			ns.Message = "Flash Job failed; see Job logs"
			return ns
		default:
			ns.State = firmwarev1alpha1.NodeStateFlashing
			return ns
		}
	}

	// No Job exists yet. If drain is disabled, we're ready to flash directly.
	if !drainEnabled(cr) {
		ns.State = firmwarev1alpha1.NodeStatePending
		return ns
	}

	// Drain enabled. Walk through the pre-flash gates.
	weCordoned := node.Annotations[AnnoCordonedBy] == cr.Name
	if !node.Spec.Unschedulable {
		// Not cordoned yet (or cordon was lost) → Cordoning is the next step.
		ns.State = firmwarev1alpha1.NodeStateCordoning
		return ns
	}
	if !weCordoned {
		// Cordoned by someone else — refuse to flash. Surfaced via the
		// MessageExternalCordon constant which the advance loop refuses
		// to act on (same gating as MessageNodeConflict).
		ns.State = firmwarev1alpha1.NodeStatePending
		ns.Message = MessageExternalCordon
		return ns
	}

	// We cordoned. Check device pods.
	force := cr.Spec.UpgradePolicy.Drain.Force
	devicePods, err := r.listDevicePodsOnNode(ctx, node.Name, force)
	if err != nil {
		ns.State = firmwarev1alpha1.NodeStateDraining
		ns.Message = fmt.Sprintf("transient: list device pods: %v", err)
		return ns
	}
	if len(devicePods) > 0 {
		// Check timeout.
		if elapsed := cordonElapsed(&node); elapsed > drainTimeout(cr) {
			ns.State = firmwarev1alpha1.NodeStateFailed
			names := make([]string, 0, len(devicePods))
			for _, p := range devicePods {
				names = append(names, p.Namespace+"/"+p.Name)
			}
			ns.Message = fmt.Sprintf("drain timeout after %s; blocking pods: %s",
				drainTimeout(cr), strings.Join(names, ","))
			return ns
		}
		ns.State = firmwarev1alpha1.NodeStateDraining
		ns.Message = fmt.Sprintf("waiting for %d pod(s) to evict", len(devicePods))
		return ns
	}

	// Cordoned, no device pods left — ready to spawn the flash Job.
	ns.State = firmwarev1alpha1.NodeStatePending
	return ns
}

// advanceNode dispatches per observed state. Each branch performs the
// single action that nudges the node toward Done; the next reconcile
// re-observes and walks one step further.
//
// Returns (advanced, error) where `advanced` means we used a parallelism
// slot. We count *any* in-flight node (Cordoning / Draining / Flashing /
// Uncordoning) against maxParallel — not just Flashing — because all of
// them tie up workload availability on a node.
func (r *FirmwarePolicyReconciler) advanceNode(ctx context.Context, cr *firmwarev1alpha1.TenstorrentFirmwarePolicy, node *corev1.Node, ns *firmwarev1alpha1.NodeStatus) (bool, error) {
	if err := r.claimOwnership(ctx, node, cr.Name); err != nil {
		return false, fmt.Errorf("claim ownership: %w", err)
	}

	switch ns.State {
	case firmwarev1alpha1.NodeStateCordoning:
		// We observed drain enabled + node not yet cordoned. Cordon.
		if err := r.cordonNode(ctx, node, cr.Name); err != nil {
			return false, fmt.Errorf("cordon: %w", err)
		}
		log.FromContext(ctx).Info("cordoned node", "cr", cr.Name, "node", node.Name)
		// Next reconcile picks up: Cordoning → Draining (or → Flashing if no device pods).
		return true, nil

	case firmwarev1alpha1.NodeStateDraining:
		// Evict device pods one by one. PDB-blocked evictions surface as
		// a transient note in status; the next reconcile retries.
		force := cr.Spec.UpgradePolicy.Drain.Force
		pods, err := r.listDevicePodsOnNode(ctx, node.Name, force)
		if err != nil {
			return false, fmt.Errorf("list device pods: %w", err)
		}
		for i := range pods {
			if err := r.evictPod(ctx, &pods[i]); err != nil {
				if _, blocked := err.(errEvictionBlocked); blocked {
					ns.Message = fmt.Sprintf("eviction of %s/%s blocked by PDB; will retry",
						pods[i].Namespace, pods[i].Name)
					log.FromContext(ctx).Info("eviction blocked by PDB",
						"cr", cr.Name, "node", node.Name,
						"pod", pods[i].Namespace+"/"+pods[i].Name)
					continue
				}
				return false, fmt.Errorf("evict %s/%s: %w", pods[i].Namespace, pods[i].Name, err)
			}
			log.FromContext(ctx).Info("evicted pod",
				"cr", cr.Name, "node", node.Name,
				"pod", pods[i].Namespace+"/"+pods[i].Name)
		}
		return true, nil

	case firmwarev1alpha1.NodeStatePending:
		// Pending means "ready to spawn the flash Job":
		//  - drain disabled: from scratch
		//  - drain enabled: post-cordon + post-drain
		return r.spawnFlashJob(ctx, cr, node, ns)

	case firmwarev1alpha1.NodeStateUncordoning:
		if err := r.uncordonNode(ctx, node, cr.Name); err != nil {
			return false, fmt.Errorf("uncordon: %w", err)
		}
		log.FromContext(ctx).Info("uncordoned node", "cr", cr.Name, "node", node.Name)
		// Next observation will see Job succeeded + no cordon → Done.
		return true, nil
	}

	return false, nil
}

// countInFlightJobs returns the number of unfinished Jobs owned by this CR,
// identified by the JobLabelCR label. "Unfinished" means jobFinished returns
// (false, _) — i.e. no terminal Complete/Failed condition is set yet. This is
// the source of truth for "how many parallelism slots are currently consumed"
// and is robust to observed-state drift across reconciles.
func (r *FirmwarePolicyReconciler) countInFlightJobs(ctx context.Context, cr *firmwarev1alpha1.TenstorrentFirmwarePolicy) (int, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs,
		client.InNamespace(operatorNamespace()),
		client.MatchingLabels{JobLabelCR: cr.Name},
	); err != nil {
		return 0, err
	}
	n := 0
	for i := range jobs.Items {
		completed, _ := jobFinished(&jobs.Items[i])
		if !completed {
			n++
		}
	}
	return n, nil
}

// spawnFlashJob creates the per-node flash Job and writes the
// denormalized labels/annotations the operator-guide tells humans to
// grep. Idempotent on AlreadyExists.
func (r *FirmwarePolicyReconciler) spawnFlashJob(ctx context.Context, cr *firmwarev1alpha1.TenstorrentFirmwarePolicy, node *corev1.Node, ns *firmwarev1alpha1.NodeStatus) (bool, error) {
	job := buildFlashJob(cr, node.Name, defaultToolsImage())
	setOwnerRef(job, cr)
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			ns.State = firmwarev1alpha1.NodeStateFlashing
			ns.LastFlashJob = job.Name
			return true, nil
		}
		return false, fmt.Errorf("create flash job: %w", err)
	}
	log.FromContext(ctx).Info("spawned flash Job",
		"cr", cr.Name, "node", node.Name, "version", cr.Spec.Version, "job", job.Name)

	// The slot is consumed the instant the apiserver accepts the Create.
	// Reflect that in ns immediately so the reconcile loop's capacity
	// accounting sees this node as Flashing even if a subsequent
	// annotate/label patch fails — otherwise the loop would happily keep
	// the slot "open" and spawn another Job for a different Pending node
	// despite a Job already existing here.
	ns.State = firmwarev1alpha1.NodeStateFlashing
	ns.LastFlashJob = job.Name

	if err := r.annotateNode(ctx, node, map[string]string{
		AnnoDesiredVersion: cr.Spec.Version,
		AnnoLastFlashJob:   job.Name,
	}); err != nil {
		return false, fmt.Errorf("annotate node: %w", err)
	}
	if err := r.labelNode(ctx, node, map[string]string{
		LabelUpgradeState: string(firmwarev1alpha1.NodeStateFlashing),
	}); err != nil {
		return false, fmt.Errorf("label node: %w", err)
	}
	return true, nil
}

func (r *FirmwarePolicyReconciler) claimOwnership(ctx context.Context, node *corev1.Node, crName string) error {
	if node.Labels[LabelOwnerCR] == crName {
		return nil
	}
	return r.labelNode(ctx, node, map[string]string{LabelOwnerCR: crName})
}

func (r *FirmwarePolicyReconciler) labelNode(ctx context.Context, node *corev1.Node, kv map[string]string) error {
	patch := client.MergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	for k, v := range kv {
		node.Labels[k] = v
	}
	return r.Patch(ctx, node, patch)
}

func (r *FirmwarePolicyReconciler) annotateNode(ctx context.Context, node *corev1.Node, kv map[string]string) error {
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	for k, v := range kv {
		node.Annotations[k] = v
	}
	return r.Patch(ctx, node, patch)
}

func findNode(nodes []corev1.Node, name string) *corev1.Node {
	for i := range nodes {
		if nodes[i].Name == name {
			return &nodes[i]
		}
	}
	return nil
}

func setTopLevelConditions(cr *firmwarev1alpha1.TenstorrentFirmwarePolicy, s firmwarev1alpha1.StatusSummary) {
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
	case s.Failed > 0:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "NodesFailed", fmt.Sprintf("%d node(s) failed", s.Failed)
		progressing.Status, progressing.Reason = metav1.ConditionFalse, "Halted"
	case s.InProgress > 0 || s.Pending > 0:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "NodesPending", fmt.Sprintf("%d/%d up to date", s.UpToDate, s.Matched)
		progressing.Status, progressing.Reason = metav1.ConditionTrue, "NodesUpgrading"
	default:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionTrue, "AllUpToDate", "All matched nodes at desired version"
		progressing.Status, progressing.Reason = metav1.ConditionFalse, "Idle"
	}
	meta.SetStatusCondition(&cr.Status.Conditions, ready)
	meta.SetStatusCondition(&cr.Status.Conditions, progressing)
}

// SetupWithManager wires watches: the CR itself, Jobs we own, and Nodes
// (so a node-label change re-triggers the controller).
func (r *FirmwarePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
		Owns(&batchv1.Job{}).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.mapNodeToCRs),
			builder.WithPredicates(),
		).
		Complete(r)
}

// mapNodeToCRs: any node change enqueues every CR. We don't try to be clever
// about which CR matches — selectors are cheap, and the alternative (parsing
// every CR's selector on every node event) gets buggy fast.
func (r *FirmwarePolicyReconciler) mapNodeToCRs(ctx context.Context, _ client.Object) []ctrl.Request {
	var list firmwarev1alpha1.TenstorrentFirmwarePolicyList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "list TenstorrentFirmwarePolicys for node event")
		return nil
	}
	out := make([]ctrl.Request, 0, len(list.Items))
	for _, cr := range list.Items {
		out = append(out, ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
	}
	return out
}

