package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	firmwarev1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/firmware/v1alpha1"
)

// drainEnabled returns true when spec.upgradePolicy.drain.enable is unset
// (default true) or explicitly true.
func drainEnabled(cr *firmwarev1alpha1.TenstorrentFirmwarePolicy) bool {
	return cr.Spec.UpgradePolicy.Drain.Enable == nil || *cr.Spec.UpgradePolicy.Drain.Enable
}

// isInFlight is true for any node state that occupies a parallelism slot
// — anything between "matched but not acted on" and "terminal."
func isInFlight(s firmwarev1alpha1.NodeState) bool {
	switch s {
	case firmwarev1alpha1.NodeStateCordoning,
		firmwarev1alpha1.NodeStateDraining,
		firmwarev1alpha1.NodeStateFlashing,
		firmwarev1alpha1.NodeStateUncordoning:
		return true
	}
	return false
}

// isAdvanceable is true for states the controller can take an action on
// to move toward Done. Flashing waits for the Job; Done/Failed are
// terminal.
func isAdvanceable(s firmwarev1alpha1.NodeState) bool {
	switch s {
	case firmwarev1alpha1.NodeStatePending,
		firmwarev1alpha1.NodeStateCordoning,
		firmwarev1alpha1.NodeStateDraining,
		firmwarev1alpha1.NodeStateUncordoning:
		return true
	}
	return false
}

// drainTimeout returns the drain.timeoutSeconds with a 600s default.
func drainTimeout(cr *firmwarev1alpha1.TenstorrentFirmwarePolicy) time.Duration {
	t := cr.Spec.UpgradePolicy.Drain.TimeoutSeconds
	if t <= 0 {
		t = 600
	}
	return time.Duration(t) * time.Second
}

// cordonNode patches node.Spec.Unschedulable=true and records ownership.
// Idempotent: re-running on an already-owned cordoned node is a no-op.
func (r *FirmwarePolicyReconciler) cordonNode(ctx context.Context, node *corev1.Node, crName string) error {
	if node.Spec.Unschedulable && node.Annotations[AnnoCordonedBy] == crName {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[AnnoCordonedBy] = crName
	if node.Annotations[AnnoCordonedAt] == "" {
		node.Annotations[AnnoCordonedAt] = time.Now().UTC().Format(time.RFC3339)
	}
	return r.Patch(ctx, node, patch)
}

// uncordonNode clears the cordon + ownership annotations. Idempotent.
// Only acts if we previously cordoned the node (avoids fighting external
// cordons applied for unrelated reasons).
func (r *FirmwarePolicyReconciler) uncordonNode(ctx context.Context, node *corev1.Node, crName string) error {
	if node.Annotations[AnnoCordonedBy] != crName {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = false
	delete(node.Annotations, AnnoCordonedBy)
	delete(node.Annotations, AnnoCordonedAt)
	return r.Patch(ctx, node, patch)
}

// cordonElapsed returns how long the node has been cordoned by us. Returns
// 0 if the cordoned-at annotation is missing or malformed.
func cordonElapsed(node *corev1.Node) time.Duration {
	v := node.Annotations[AnnoCordonedAt]
	if v == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return 0
	}
	return time.Since(t)
}

// listDevicePodsOnNode returns the active pods on this node that hold
// /dev/tenstorrent open and would conflict with a flash. Excludes:
//   - Pods in the operator's own namespace (flasher Job, driver DS,
//     controller itself, NFD)
//   - Pods owned by a DaemonSet (these come back automatically on drain
//     and would fight the eviction loop)
//   - Pods in a terminal phase (Succeeded / Failed)
//
// When `force=false` (the default), bare pods (no OwnerReference) are
// also excluded — kubectl-drain semantics: refuse to evict unmanaged
// pods unless the operator opts in.
func (r *FirmwarePolicyReconciler) listDevicePodsOnNode(
	ctx context.Context, nodeName string, force bool,
) ([]corev1.Pod, error) {
	var all corev1.PodList
	if err := r.List(ctx, &all); err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	out := make([]corev1.Pod, 0, len(all.Items))
	for _, p := range all.Items {
		if p.Spec.NodeName != nodeName {
			continue
		}
		if p.Namespace == operatorNamespace() {
			continue
		}
		if podUsesTenstorrentDevice(&p) == false {
			continue
		}
		if isTerminalPhase(p.Status.Phase) {
			continue
		}
		if podIsDaemonSetOwned(&p) {
			continue
		}
		if !force && podIsBare(&p) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func podUsesTenstorrentDevice(p *corev1.Pod) bool {
	for _, v := range p.Spec.Volumes {
		if v.HostPath != nil && strings.HasPrefix(v.HostPath.Path, "/dev/tenstorrent") {
			return true
		}
	}
	return false
}

func podIsDaemonSetOwned(p *corev1.Pod) bool {
	for _, ref := range p.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

func podIsBare(p *corev1.Pod) bool {
	return len(p.OwnerReferences) == 0
}

func isTerminalPhase(phase corev1.PodPhase) bool {
	return phase == corev1.PodSucceeded || phase == corev1.PodFailed
}

// evictPod calls the eviction subresource on a pod. Returns three
// outcomes:
//   - nil error: pod accepted for eviction (or already gone)
//   - errEvictionBlocked: 429 Too Many Requests, a PDB is blocking us;
//     caller should retry later
//   - other error: real failure
func (r *FirmwarePolicyReconciler) evictPod(ctx context.Context, pod *corev1.Pod) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	if err := r.SubResource("eviction").Create(ctx, pod, eviction); err != nil {
		if apierrors.IsNotFound(err) {
			// Already gone — same effect as eviction succeeding.
			return nil
		}
		if apierrors.IsTooManyRequests(err) {
			// PDB block — surface as a sentinel.
			return errEvictionBlocked{Reason: err.Error()}
		}
		return err
	}
	return nil
}

// errEvictionBlocked is returned when a PDB prevented eviction. The
// caller treats this as "transient, retry" rather than a hard failure.
type errEvictionBlocked struct{ Reason string }

func (e errEvictionBlocked) Error() string { return "eviction blocked: " + e.Reason }
