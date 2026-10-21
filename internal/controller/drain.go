// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"

	firmwarev1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/firmware/v1alpha1"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/drain"
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

// firmwareCordonOpts returns the cordon annotation keys + owner the
// firmware controller uses, so other firmware reconciler methods don't
// need to assemble them inline.
func firmwareCordonOpts(crName string) drain.CordonOpts {
	return drain.CordonOpts{
		AnnoBy: AnnoCordonedBy,
		AnnoAt: AnnoCordonedAt,
		Owner:  crName,
	}
}

// cordonNode / uncordonNode / cordonElapsed / listDevicePodsOnNode /
// evictPod are thin wrappers around the shared `drain` package — they
// exist so the FirmwarePolicyReconciler method-style call sites elsewhere
// in this package don't need to know about the package's signature
// changes.
func (r *FirmwarePolicyReconciler) cordonNode(ctx context.Context, node *corev1.Node, crName string) error {
	return drain.CordonNode(ctx, r.Client, node, firmwareCordonOpts(crName))
}

func (r *FirmwarePolicyReconciler) uncordonNode(ctx context.Context, node *corev1.Node, crName string) error {
	return drain.UncordonNode(ctx, r.Client, node, firmwareCordonOpts(crName))
}

func cordonElapsed(node *corev1.Node) time.Duration {
	return drain.CordonElapsed(node, AnnoCordonedAt)
}

func (r *FirmwarePolicyReconciler) listDevicePodsOnNode(
	ctx context.Context, nodeName string, force bool,
) ([]corev1.Pod, error) {
	return drain.ListDevicePodsOnNode(ctx, r.Client, nodeName, operatorNamespace(), force, drain.PodUsesTenstorrentDevice)
}

func (r *FirmwarePolicyReconciler) evictPod(ctx context.Context, pod *corev1.Pod) error {
	return drain.EvictPod(ctx, r.Client, pod)
}

// errEvictionBlocked is preserved as a package-local alias so existing
// firmware code that does `errors.As(err, &errEvictionBlocked{})` keeps
// working. New code in this package should prefer drain.ErrEvictionBlocked.
type errEvictionBlocked = drain.ErrEvictionBlocked
