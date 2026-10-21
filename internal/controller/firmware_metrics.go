// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

package controller

import (
	"context"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	firmwarev1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/firmware/v1alpha1"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/metrics"
)

// firmwareEvents dedupes the firmware controller's level-triggered
// observations down to one metric increment per real event. Package-level
// because the reconciler is constructed in main.go without wiring, and the
// state is keyed by CR name so CRs can't clobber each other's history.
var firmwareEvents = metrics.NewEventDedup()

// firmwareNodeStates enumerates every NodeState, so the per-CR gauge
// publishes zeros for the states nobody is in rather than dropping the
// series.
var firmwareNodeStates = []firmwarev1alpha1.NodeState{
	firmwarev1alpha1.NodeStatePending,
	firmwarev1alpha1.NodeStateCordoning,
	firmwarev1alpha1.NodeStateDraining,
	firmwarev1alpha1.NodeStateFlashing,
	firmwarev1alpha1.NodeStateUncordoning,
	firmwarev1alpha1.NodeStateDone,
	firmwarev1alpha1.NodeStateFailed,
}

// dedup key namespaces. Both are keyed by object names, which stay inside
// the dedup map and never reach a metric label.
func flashJobKey(jobName string) string  { return "job/" + jobName }
func drainTimeoutKey(node string) string { return "drain-timeout/" + node }

// recordFirmwarePolicyMetrics publishes the per-CR gauges from the node
// states the reconciler just computed.
func recordFirmwarePolicyMetrics(cr *firmwarev1alpha1.TenstorrentFirmwarePolicy, nodeStates []firmwarev1alpha1.NodeStatus) {
	counts := make(map[string]int, len(firmwareNodeStates))
	for _, s := range firmwareNodeStates {
		counts[string(s)] = 0
	}
	for _, ns := range nodeStates {
		counts[string(ns.State)]++
	}
	metrics.SetFirmwarePolicyNodes(cr.Name, counts)
	metrics.SetFirmwareDesiredVersion(cr.Name, cr.Spec.Version)
}

// recordFlashJobMetrics records the terminal outcome and wall time of each
// of the CR's flash Jobs exactly once, and returns the dedup keys for every
// Job that still exists so the caller can prune.
//
// Wall time comes from the Job's own timestamps rather than a timer in the
// controller: the flash outlives any single reconcile, and the controller
// may well have restarted while it ran.
func recordFlashJobMetrics(crName string, jobs []batchv1.Job) []string {
	keys := make([]string, 0, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		key := flashJobKey(job.Name)
		keys = append(keys, key)

		completed, succeeded := jobFinished(job)
		if !completed {
			continue
		}
		result := metrics.ResultSuccess
		if !succeeded {
			result = metrics.ResultFailed
		}
		// The Job stays terminal (and visible) until its TTL expires, so
		// every subsequent reconcile re-derives this same outcome. Count the
		// first sighting only.
		if !firmwareEvents.FirstSeen(crName, key) {
			continue
		}
		metrics.FirmwareFlashJobsTotal.WithLabelValues(crName, result).Inc()
		if d, ok := flashJobDuration(job); ok {
			metrics.FirmwareFlashDuration.WithLabelValues(crName, result).Observe(d.Seconds())
		}
	}
	return keys
}

// recordDrainTimeoutMetrics counts nodes that blew their drain deadline,
// once per stall. A timed-out node keeps reporting the timeout on every
// reconcile until a human clears whatever is holding the device, so the
// dedup key is what makes this a count of stalls rather than a count of
// reconciles. Returns the keys of the nodes still timed out.
func recordDrainTimeoutMetrics(crName string, nodeStates []firmwarev1alpha1.NodeStatus) []string {
	var keys []string
	for _, ns := range nodeStates {
		if !isDrainTimeout(ns) {
			continue
		}
		key := drainTimeoutKey(ns.Name)
		keys = append(keys, key)
		if firmwareEvents.FirstSeen(crName, key) {
			metrics.FirmwareDrainBlockedTotal.WithLabelValues(crName, metrics.ReasonDrainTimeout).Inc()
		}
	}
	return keys
}

// isDrainTimeout reports whether this node failed because its drain window
// expired, as opposed to a failed flash Job.
func isDrainTimeout(ns firmwarev1alpha1.NodeStatus) bool {
	return ns.State == firmwarev1alpha1.NodeStateFailed &&
		strings.HasPrefix(ns.Message, MessageDrainTimeoutPrefix)
}

// recordFWVersionMetrics groups every node in the cluster by its
// fw-version label — the label the controller writes after a verified
// tt-smi readback. Cluster-wide for the same reason as the kmd-version
// gauge: "what firmware is out there" isn't a per-policy question.
func (r *FirmwarePolicyReconciler) recordFWVersionMetrics(ctx context.Context) error {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return err
	}
	counts := map[string]int{}
	for i := range nodes.Items {
		if v := nodes.Items[i].Labels[LabelFWVersion]; v != "" {
			counts[v]++
		}
	}
	metrics.SetNodesByFWVersion(counts)
	return nil
}
