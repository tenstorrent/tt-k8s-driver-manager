// Package drain provides controller-agnostic primitives for cordoning a
// node, listing device-using pods, and evicting them via the policy/v1
// Eviction subresource.
//
// Lifted out of internal/controller/drain.go (which was firmware-specific
// receiver methods) so both the firmware and driver controllers can use
// the same implementation. Each controller passes its own
// annotation key prefix + owner value so that, e.g. a firmware cordon
// doesn't get accidentally unlocked by the driver controller and vice
// versa.
package drain

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
)

// CordonOpts identifies the cordon's owner via two annotation keys (so the
// caller controls the prefix — e.g. firmware.tenstorrent.com/cordoned-by
// vs driver.tenstorrent.com/cordoned-by) and an owner string (typically the
// CR name).
type CordonOpts struct {
	AnnoBy string
	AnnoAt string
	Owner  string
}

// CordonNode patches node.Spec.Unschedulable=true and records ownership.
// Idempotent: re-running on a node already cordoned by the same owner is
// a no-op. Caller-supplied annotation keys keep firmware and driver
// controllers from accidentally stomping on each other's cordons.
func CordonNode(ctx context.Context, c client.Client, node *corev1.Node, opts CordonOpts) error {
	if node.Spec.Unschedulable && node.Annotations[opts.AnnoBy] == opts.Owner {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[opts.AnnoBy] = opts.Owner
	if node.Annotations[opts.AnnoAt] == "" {
		node.Annotations[opts.AnnoAt] = time.Now().UTC().Format(time.RFC3339)
	}
	return c.Patch(ctx, node, patch)
}

// UncordonNode clears the cordon + ownership annotations. Idempotent.
// Only acts if we previously cordoned the node (avoids fighting external
// cordons applied for unrelated reasons).
func UncordonNode(ctx context.Context, c client.Client, node *corev1.Node, opts CordonOpts) error {
	if node.Annotations[opts.AnnoBy] != opts.Owner {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = false
	delete(node.Annotations, opts.AnnoBy)
	delete(node.Annotations, opts.AnnoAt)
	return c.Patch(ctx, node, patch)
}

// CordonElapsed returns how long the node has been cordoned according to
// the AnnoAt timestamp. Returns 0 if the annotation is missing or
// malformed.
func CordonElapsed(node *corev1.Node, annoAt string) time.Duration {
	v := node.Annotations[annoAt]
	if v == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return 0
	}
	return time.Since(t)
}

// PodFilter decides whether a pod should be considered for drain.
// Returning false skips the pod.
type PodFilter func(p *corev1.Pod) bool

// ListDevicePodsOnNode returns active pods on the named node that match
// the supplied filter. Always excludes:
//   - pods in `excludeNamespace` (typically the operator's own namespace)
//   - DaemonSet-owned pods (they respawn immediately on drain and would
//     fight the eviction loop)
//   - pods in a terminal phase (Succeeded / Failed)
//
// When force=false, also excludes bare pods (no OwnerReference) —
// kubectl-drain semantics: refuse to evict unmanaged pods unless the
// operator opts in.
func ListDevicePodsOnNode(
	ctx context.Context,
	c client.Client,
	nodeName, excludeNamespace string,
	force bool,
	filter PodFilter,
) ([]corev1.Pod, error) {
	var all corev1.PodList
	if err := c.List(ctx, &all); err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	out := make([]corev1.Pod, 0, len(all.Items))
	for _, p := range all.Items {
		if p.Spec.NodeName != nodeName {
			continue
		}
		if p.Namespace == excludeNamespace {
			continue
		}
		if filter != nil && !filter(&p) {
			continue
		}
		if IsTerminalPhase(p.Status.Phase) {
			continue
		}
		if PodIsDaemonSetOwned(&p) {
			continue
		}
		if !force && PodIsBare(&p) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// PodUsesTenstorrentDevice is the canonical filter: matches pods that
// mount /dev/tenstorrent via hostPath. Both firmware and driver
// controllers care about the same set.
func PodUsesTenstorrentDevice(p *corev1.Pod) bool {
	for _, v := range p.Spec.Volumes {
		if v.HostPath != nil && strings.HasPrefix(v.HostPath.Path, "/dev/tenstorrent") {
			return true
		}
	}
	return false
}

func PodIsDaemonSetOwned(p *corev1.Pod) bool {
	for _, ref := range p.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

func PodIsBare(p *corev1.Pod) bool {
	return len(p.OwnerReferences) == 0
}

// PodHasEmptyDir reports whether the pod has any emptyDir volume —
// kubectl drain's --delete-emptydir-data gate. The full-node drain pass
// uses this to skip pods whose emptyDir data would be silently lost
// unless the operator explicitly opted in.
func PodHasEmptyDir(p *corev1.Pod) bool {
	for _, v := range p.Spec.Volumes {
		if v.EmptyDir != nil {
			return true
		}
	}
	return false
}

func IsTerminalPhase(phase corev1.PodPhase) bool {
	return phase == corev1.PodSucceeded || phase == corev1.PodFailed
}

// EvictPod calls the eviction subresource on a pod. Returns three
// outcomes:
//   - nil: eviction accepted (or pod already gone)
//   - ErrEvictionBlocked: 429 from a PDB; caller should retry later
//   - other error: real failure
func EvictPod(ctx context.Context, c client.Client, pod *corev1.Pod) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	if err := c.SubResource("eviction").Create(ctx, pod, eviction); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		if apierrors.IsTooManyRequests(err) {
			return ErrEvictionBlocked{Reason: err.Error()}
		}
		return err
	}
	return nil
}

// ErrEvictionBlocked is returned when a PDB blocked an eviction call.
// Callers treat this as transient and retry on the next reconcile.
type ErrEvictionBlocked struct{ Reason string }

func (e ErrEvictionBlocked) Error() string { return "eviction blocked: " + e.Reason }
