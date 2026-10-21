// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	driverv1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/driver/v1alpha1"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/metrics"
)

// driverNodeStates enumerates every DriverNodeState the reconciler can
// compute. The per-CR gauge publishes all of them on every reconcile —
// including the zeros — so a state that empties out reads as 0 rather than
// vanishing from the graph mid-rollout.
var driverNodeStates = []driverv1alpha1.DriverNodeState{
	driverv1alpha1.DriverNodeStatePending,
	driverv1alpha1.DriverNodeStateCordoning,
	driverv1alpha1.DriverNodeStateDraining,
	driverv1alpha1.DriverNodeStateUpgrading,
	driverv1alpha1.DriverNodeStateUncordoning,
	driverv1alpha1.DriverNodeStateDone,
	driverv1alpha1.DriverNodeStateFailed,
}

// recordDriverPolicyMetrics publishes the per-CR gauges from the status the
// reconciler just computed. Called from updateStatus so there's exactly one
// place that decides what status.nodes says and what the gauge says.
func recordDriverPolicyMetrics(cr *driverv1alpha1.TenstorrentDriverPolicy) {
	counts := make(map[string]int, len(driverNodeStates))
	for _, s := range driverNodeStates {
		counts[string(s)] = 0
	}
	for _, ns := range cr.Status.Nodes {
		counts[string(ns.State)]++
	}
	metrics.SetDriverPolicyNodes(cr.Name, counts)
	metrics.SetDriverDesiredVersion(cr.Name, cr.Spec.Version)
}

// recordKMDVersionMetrics groups every node in the cluster by its
// kmd-version + install-mode labels.
//
// Cluster-wide rather than per-CR on purpose: "which kmd is actually loaded
// out there" isn't a per-policy question, and each CR's reconcile derives
// the same answer from the same (cached) node list, so whichever CR
// reconciles last leaves the gauge correct. Nodes with no kmd-version label
// are skipped — the metric counts labeled nodes, and an "unlabeled" bucket
// would silently include every non-Tenstorrent node in the cluster.
func (r *DriverPolicyReconciler) recordKMDVersionMetrics(ctx context.Context) error {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return err
	}
	counts := map[metrics.VersionMode]int{}
	for i := range nodes.Items {
		l := nodes.Items[i].Labels
		version := l[LabelKMDVersion]
		if version == "" {
			continue
		}
		counts[metrics.VersionMode{Version: version, InstallMode: l[LabelInstallMode]}]++
	}
	metrics.SetNodesByKMDVersion(counts)
	return nil
}
