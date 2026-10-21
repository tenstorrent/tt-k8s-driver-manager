// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

package controller

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	driverv1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/driver/v1alpha1"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/metrics"
)

func TestRecordDriverPolicyMetrics_PublishesZeroStates(t *testing.T) {
	cr := newDriverCR()
	cr.Name = "driver-states-cr"
	t.Cleanup(func() { metrics.DeleteDriverPolicySeries(cr.Name) })

	cr.Status.Nodes = []driverv1alpha1.DriverNodeStatus{
		{Name: "a", State: driverv1alpha1.DriverNodeStateDone},
		{Name: "b", State: driverv1alpha1.DriverNodeStateUpgrading},
		{Name: "c", State: driverv1alpha1.DriverNodeStateFailed},
	}
	recordDriverPolicyMetrics(cr)

	for state, want := range map[string]float64{
		"Done":      1,
		"Upgrading": 1,
		"Failed":    1,
		"Pending":   0,
	} {
		if got := testutil.ToFloat64(metrics.DriverPolicyNodes.WithLabelValues(cr.Name, state)); got != want {
			t.Errorf("state %s = %v; want %v", state, got, want)
		}
	}
	if got := testutil.CollectAndCount(metrics.DriverPolicyNodes); got != len(driverNodeStates) {
		t.Errorf("series = %d; want %d (one per state)", got, len(driverNodeStates))
	}
	if got := testutil.ToFloat64(metrics.DriverPolicyDesiredVersion.WithLabelValues(cr.Name, cr.Spec.Version)); got != 1 {
		t.Errorf("desired-version info gauge = %v; want 1", got)
	}
}

// The gauge is deliberately fleet-wide and keyed on the labels the builder
// pods' readiness produced, so it reports what is loaded rather than what
// any one policy wants.
func TestRecordKMDVersionMetrics_GroupsByVersionAndInstallMode(t *testing.T) {
	t.Cleanup(metrics.DriverNodesByKMDVersion.Reset)

	r := &DriverPolicyReconciler{
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
			ptr(node("tt-1", map[string]string{LabelKMDVersion: "2.8.0", LabelInstallMode: "container"})),
			ptr(node("tt-2", map[string]string{LabelKMDVersion: "2.8.0", LabelInstallMode: "container"})),
			ptr(node("tt-3", map[string]string{LabelKMDVersion: "2.7.0", LabelInstallMode: "host"})),
			// Labeled by an older builder that predates install-mode.
			ptr(node("tt-4", map[string]string{LabelKMDVersion: "2.7.0"})),
			ptr(node("cpu-only", nil)),
		).Build(),
	}
	if err := r.recordKMDVersionMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := testutil.ToFloat64(metrics.DriverNodesByKMDVersion.WithLabelValues("2.8.0", "container")); got != 2 {
		t.Errorf("2.8.0/container = %v; want 2", got)
	}
	if got := testutil.ToFloat64(metrics.DriverNodesByKMDVersion.WithLabelValues("2.7.0", "host")); got != 1 {
		t.Errorf("2.7.0/host = %v; want 1", got)
	}
	if got := testutil.ToFloat64(metrics.DriverNodesByKMDVersion.WithLabelValues("2.7.0", metrics.InstallModeUnknown)); got != 1 {
		t.Errorf("2.7.0/unknown = %v; want 1", got)
	}
	// The unlabeled node contributes no series.
	if got := testutil.CollectAndCount(metrics.DriverNodesByKMDVersion); got != 3 {
		t.Errorf("series = %d; want 3", got)
	}
}
