package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	driverv1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/driver/v1alpha1"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/drain"
)

// drainEnabledForCR returns true when spec.upgradePolicy.drain.enable is
// unset (default true via kubebuilder) or explicitly true.
func drainEnabledForCR(cr *driverv1alpha1.TenstorrentDriverPolicy) bool {
	return cr.Spec.UpgradePolicy.Drain.Enable == nil || *cr.Spec.UpgradePolicy.Drain.Enable
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

	for i := range nodes {
		node := &nodes[i]
		if err := drain.CordonNode(ctx, r.Client, node, opts); err != nil {
			logger.Error(err, "cordon node", "node", node.Name)
			continue
		}
		pods, err := drain.ListDevicePodsOnNode(
			ctx, r.Client, node.Name, operatorNamespace(), force, drain.PodUsesTenstorrentDevice,
		)
		if err != nil {
			logger.Error(err, "list device pods", "node", node.Name)
			continue
		}
		for j := range pods {
			pod := &pods[j]
			if err := drain.EvictPod(ctx, r.Client, pod); err != nil {
				if _, ok := err.(drain.ErrEvictionBlocked); ok {
					// PDB block — transient, picked up next reconcile.
					logger.Info("eviction blocked by PDB; will retry",
						"pod", pod.Name, "namespace", pod.Namespace)
					continue
				}
				logger.Error(err, "evict pod", "pod", pod.Name, "namespace", pod.Namespace)
			}
		}
	}
	return nil
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
	}
	return nil
}

