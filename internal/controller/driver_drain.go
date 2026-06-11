package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	driverv1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/driver/v1alpha1"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/drain"
)

// defaultDriverDeployGates is the fallback list used when the
// DRIVER_DEPLOY_GATES env var is unset. The chart sets the env from
// controller.deployGates in values.yaml; this fallback exists so raw
// `go run` and unit-test paths still get the documented default.
//
// Keep in sync with charts/tt-k8s-driver-manager/values.yaml's
// controller.deployGates default — the chart is the user-facing knob.
var defaultDriverDeployGates = []string{
	"tenstorrent.com/deploy.tt-telemetry",
}

// driverDeployGates returns the list of node-label keys the controller
// flips off (value="false") during a kmd upgrade to drain sibling DSes
// that hold /dev/tenstorrent. After the per-node builder pod becomes
// Ready against the new version, the label is REMOVED (not flipped to
// "true") so the chart's NotIn ["false"] semantic naturally schedules
// the DS back. Mirrors NVIDIA's `nvidia.com/gpu.deploy.<component>=true`
// pattern, with the label keys chart-side instead of operator-side.
//
// Comma-separated `DRIVER_DEPLOY_GATES` env var overrides the default;
// the chart threads `controller.deployGates` through to it. Whitespace
// is trimmed and empty entries dropped. An empty env value disables the
// gate flip entirely.
func driverDeployGates() []string {
	raw, set := envSet("DRIVER_DEPLOY_GATES")
	if !set {
		return defaultDriverDeployGates
	}
	out := []string{}
	for _, g := range strings.Split(raw, ",") {
		g = strings.TrimSpace(g)
		if g != "" {
			out = append(out, g)
		}
	}
	return out
}

// drainEnabledForCR returns true when spec.upgradePolicy.drain.enable is
// unset (default true via kubebuilder) or explicitly true. Gates pass 1
// (targeted /dev/tenstorrent-holder eviction).
func drainEnabledForCR(cr *driverv1alpha1.TenstorrentDriverPolicy) bool {
	return cr.Spec.UpgradePolicy.Drain.Enable == nil || *cr.Spec.UpgradePolicy.Drain.Enable
}

// fullNodeDrainEnabledForCR gates pass 2 (full-node drain). Default true
// — mirrors NVIDIA's ENABLE_AUTO_DRAIN default. Catches privileged pods
// that use /dev/tenstorrent via containerd auto-mount and so wouldn't
// match the pass-1 hostPath filter.
func fullNodeDrainEnabledForCR(cr *driverv1alpha1.TenstorrentDriverPolicy) bool {
	return cr.Spec.UpgradePolicy.Drain.FullNode == nil || *cr.Spec.UpgradePolicy.Drain.FullNode
}

// deleteEmptyDirForCR returns true when spec.upgradePolicy.drain.deleteEmptyDir
// is unset (default true) or explicitly true. Mirrors kubectl drain's
// --delete-emptydir-data; pass 2 honors it.
func deleteEmptyDirForCR(cr *driverv1alpha1.TenstorrentDriverPolicy) bool {
	return cr.Spec.UpgradePolicy.Drain.DeleteEmptyDir == nil || *cr.Spec.UpgradePolicy.Drain.DeleteEmptyDir
}

// flipDeployGatesOff patches each driverDeployGates label to "false" on
// the given node so the corresponding sibling DSes' nodeAffinity stops
// matching — DS controller deletes the pod, FDs close, refcnt drops.
// Idempotent: a label already at "false" is a no-op.
func flipDeployGatesOff(ctx context.Context, c client.Client, node *corev1.Node) error {
	patch := client.MergeFrom(node.DeepCopy())
	changed := false
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	for _, key := range driverDeployGates() {
		if node.Labels[key] != "false" {
			node.Labels[key] = "false"
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return c.Patch(ctx, node, patch)
}

// removeDeployGates deletes the driverDeployGates labels from the node
// (rather than setting them back to "true"). The chart's nodeAffinity
// is `NotIn ["false"]`, so absent and "true" both schedule — removal is
// the cleanest "back to default" state.
func removeDeployGates(ctx context.Context, c client.Client, node *corev1.Node) error {
	patch := client.MergeFrom(node.DeepCopy())
	changed := false
	for _, key := range driverDeployGates() {
		if _, ok := node.Labels[key]; ok {
			delete(node.Labels, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return c.Patch(ctx, node, patch)
}

// driverCordonOpts is the cordon owner/annotation set for the driver
// controller — distinct prefix from firmware so the two controllers
// don't stomp on each other.
func driverCordonOpts(crName string) drain.CordonOpts {
	return drain.CordonOpts{
		AnnoBy: AnnoDriverCordonedBy,
		AnnoAt: AnnoDriverCordonedAt,
		Owner:  crName,
	}
}

// listMatchedNodes returns the node objects that this CR's selector
// matches. Mirrors the filter logic in countMatchedNodes but returns the
// full Node objects so callers can cordon / patch them.
func (r *DriverPolicyReconciler) listMatchedNodes(
	ctx context.Context, cr *driverv1alpha1.TenstorrentDriverPolicy,
) ([]corev1.Node, error) {
	sel, err := metav1.LabelSelectorAsSelector(&cr.Spec.NodeSelector)
	if err != nil {
		return nil, err
	}
	var all corev1.NodeList
	if err := r.List(ctx, &all); err != nil {
		return nil, err
	}
	out := make([]corev1.Node, 0, len(all.Items))
	for _, node := range all.Items {
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
		out = append(out, node)
	}
	return out, nil
}

// prepareUpgrade cordons each matched node and evicts device-using pods
// from it. Called BEFORE the DaemonSet template is updated — by the
// time the DS controller starts rolling new builder pods, the nodes
// already have refcnt=0 (in the common case) so rmmod succeeds without
// needing the forceUnload SIGKILL path.
//
// Two-pass drain modeled on NVIDIA gpu-operator:
//   - Pass 1 (gated by drain.enable, default true): evict pods that
//     declare /dev/tenstorrent use via hostPath (drain.PodUsesTenstorrentDevice).
//   - Pass 2 (gated by drain.fullNode, default true): evict every
//     non-DS pod on the node — kubectl drain semantics. Catches
//     privileged pods that get /dev via containerd's auto-mount and
//     don't declare anything. Optionally restricted by
//     drain.podSelectorLabel and gated against emptyDir loss.
//
// Best-effort: errors on individual nodes are logged and skipped rather
// than aborting the whole reconcile. The builder pod's existing refcnt
// check + forceUnload fallback covers the case where eviction didn't
// fully drain a node before its DS pod rolled.
func (r *DriverPolicyReconciler) prepareUpgrade(
	ctx context.Context, cr *driverv1alpha1.TenstorrentDriverPolicy,
) error {
	logger := log.FromContext(ctx)
	nodes, err := r.listMatchedNodes(ctx, cr)
	if err != nil {
		return fmt.Errorf("list matched nodes: %w", err)
	}

	opts := driverCordonOpts(cr.Name)
	force := cr.Spec.UpgradePolicy.Drain.Force

	pass2Filter, err := buildFullNodeDrainFilter(cr)
	if err != nil {
		return fmt.Errorf("build full-node drain filter: %w", err)
	}
	runFullNode := fullNodeDrainEnabledForCR(cr)

	for i := range nodes {
		node := &nodes[i]
		if err := drain.CordonNode(ctx, r.Client, node, opts); err != nil {
			logger.Error(err, "cordon node", "node", node.Name)
			continue
		}
		// Flip sibling-DS deploy gates BEFORE evicting non-DS pods —
		// eviction can't drain DS-owned holders (drain.go intentionally
		// excludes them to avoid the respawn-loop), so the gate flip
		// covers the cooperative-drain DS-side. DS controller deletes
		// the pod whose nodeAffinity stops matching.
		if err := flipDeployGatesOff(ctx, r.Client, node); err != nil {
			logger.Error(err, "flip deploy gates off", "node", node.Name)
			// continue — best effort
		}
		r.evictMatching(ctx, node.Name, force, drain.PodUsesTenstorrentDevice, "pass1")
		if runFullNode {
			r.evictMatching(ctx, node.Name, force, pass2Filter, "pass2")
		}
	}
	return nil
}

// evictMatching lists pods on the node matching `filter` and evicts
// each via the Eviction subresource. Best-effort: PDB blocks and other
// errors are logged and the loop continues. `passLabel` is just a log
// tag so pass 1 / pass 2 can be told apart in events.
func (r *DriverPolicyReconciler) evictMatching(
	ctx context.Context,
	nodeName string,
	force bool,
	filter drain.PodFilter,
	passLabel string,
) {
	logger := log.FromContext(ctx)
	pods, err := drain.ListDevicePodsOnNode(
		ctx, r.Client, nodeName, operatorNamespace(), force, filter,
	)
	if err != nil {
		logger.Error(err, "list pods for drain", "node", nodeName, "pass", passLabel)
		return
	}
	for j := range pods {
		pod := &pods[j]
		if err := drain.EvictPod(ctx, r.Client, pod); err != nil {
			if _, ok := err.(drain.ErrEvictionBlocked); ok {
				logger.Info("eviction blocked by PDB; will retry",
					"pod", pod.Name, "namespace", pod.Namespace, "pass", passLabel)
				continue
			}
			logger.Error(err, "evict pod",
				"pod", pod.Name, "namespace", pod.Namespace, "pass", passLabel)
		}
	}
}

// buildFullNodeDrainFilter returns the PodFilter for pass 2 — match
// everything, then strip pods whose emptyDir we shouldn't blow away and
// pods that don't match an explicit podSelectorLabel. Returns an error
// if the user-supplied podSelectorLabel doesn't parse.
func buildFullNodeDrainFilter(cr *driverv1alpha1.TenstorrentDriverPolicy) (drain.PodFilter, error) {
	sel := labels.Everything()
	if raw := cr.Spec.UpgradePolicy.Drain.PodSelectorLabel; raw != "" {
		parsed, err := labels.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse podSelectorLabel %q: %w", raw, err)
		}
		sel = parsed
	}
	deleteEmpty := deleteEmptyDirForCR(cr)
	return func(p *corev1.Pod) bool {
		if !sel.Matches(labels.Set(p.Labels)) {
			return false
		}
		if !deleteEmpty && drain.PodHasEmptyDir(p) {
			return false
		}
		return true
	}, nil
}

// uncordonReadyNodes uncordons every matched node we previously cordoned
// whose builder pod is now Ready against the current spec.Version. Idempotent:
// nodes we don't own (annotation mismatch) are skipped.
func (r *DriverPolicyReconciler) uncordonReadyNodes(
	ctx context.Context,
	cr *driverv1alpha1.TenstorrentDriverPolicy,
	ds *appsv1.DaemonSet,
) error {
	if ds == nil {
		return nil
	}
	logger := log.FromContext(ctx)
	nodes, err := r.listMatchedNodes(ctx, cr)
	if err != nil {
		return fmt.Errorf("list matched nodes: %w", err)
	}

	// Build node→ready-at-target lookup from current installer pods.
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(operatorNamespace()),
		client.MatchingLabels{"driver.tenstorrent.com/cr": cr.Name},
	); err != nil {
		return fmt.Errorf("list installer pods: %w", err)
	}
	readyAtTarget := make(map[string]bool)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" {
			continue
		}
		// Builder pod's TT_KMD_VERSION env reflects the template it was
		// rolled from. Compare against spec.Version — if equal AND pod is
		// PodReady, the pod has successfully insmod'd the target.
		if podEnv(p, "TT_KMD_VERSION") != cr.Spec.Version {
			continue
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				readyAtTarget[p.Spec.NodeName] = true
				break
			}
		}
	}

	opts := driverCordonOpts(cr.Name)
	for i := range nodes {
		node := &nodes[i]
		if node.Annotations[AnnoDriverCordonedBy] != cr.Name {
			continue
		}
		if !readyAtTarget[node.Name] {
			continue
		}
		if err := drain.UncordonNode(ctx, r.Client, node, opts); err != nil {
			logger.Error(err, "uncordon node", "node", node.Name)
			continue
		}
		// Restore deploy gates so the sibling DSes reschedule on this
		// node. Removing the labels (not setting them to "true") is the
		// chart's "default scheduled" state.
		if err := removeDeployGates(ctx, r.Client, node); err != nil {
			logger.Error(err, "remove deploy gates", "node", node.Name)
		}
	}
	return nil
}

