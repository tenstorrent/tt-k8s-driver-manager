// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestSetNodesByKMDVersion_DropsVanishedVersions(t *testing.T) {
	t.Cleanup(DriverNodesByKMDVersion.Reset)

	SetNodesByKMDVersion(map[VersionMode]int{
		{Version: "2.7.0", InstallMode: "container"}: 3,
		{Version: "2.8.0", InstallMode: "container"}: 1,
	})
	if got := testutil.CollectAndCount(DriverNodesByKMDVersion); got != 2 {
		t.Fatalf("series after first set = %d; want 2", got)
	}

	// Fleet finishes the upgrade: 2.7.0 is gone from every node. The old
	// series must disappear rather than freeze at 3, which is the whole
	// reason the setter resets.
	SetNodesByKMDVersion(map[VersionMode]int{
		{Version: "2.8.0", InstallMode: "container"}: 4,
	})
	if got := testutil.CollectAndCount(DriverNodesByKMDVersion); got != 1 {
		t.Fatalf("series after upgrade = %d; want 1", got)
	}
	if got := testutil.ToFloat64(DriverNodesByKMDVersion.WithLabelValues("2.8.0", "container")); got != 4 {
		t.Errorf("2.8.0 count = %v; want 4", got)
	}
}

func TestSetNodesByKMDVersion_UnlabeledInstallMode(t *testing.T) {
	t.Cleanup(DriverNodesByKMDVersion.Reset)

	SetNodesByKMDVersion(map[VersionMode]int{{Version: "2.8.0"}: 2})
	if got := testutil.ToFloat64(DriverNodesByKMDVersion.WithLabelValues("2.8.0", InstallModeUnknown)); got != 2 {
		t.Errorf("count under install_mode=%s = %v; want 2", InstallModeUnknown, got)
	}
}

func TestSetDriverDesiredVersion_OneSeriesPerCR(t *testing.T) {
	t.Cleanup(DriverPolicyDesiredVersion.Reset)

	SetDriverDesiredVersion("default", "2.7.0")
	SetDriverDesiredVersion("default", "2.8.0")
	SetDriverDesiredVersion("canary", "2.9.0")

	// A version bump must replace the CR's series, not accumulate one per
	// version the CR ever targeted.
	if got := testutil.CollectAndCount(DriverPolicyDesiredVersion); got != 2 {
		t.Fatalf("series = %d; want 2 (one per CR)", got)
	}
	if got := testutil.ToFloat64(DriverPolicyDesiredVersion.WithLabelValues("default", "2.8.0")); got != 1 {
		t.Errorf("default@2.8.0 = %v; want 1", got)
	}
}

func TestDeleteDriverPolicySeries(t *testing.T) {
	t.Cleanup(DriverPolicyNodes.Reset)
	t.Cleanup(DriverErrorsTotal.Reset)

	SetDriverPolicyNodes("doomed", map[string]int{"Done": 2, "Failed": 0})
	SetDriverPolicyNodes("survivor", map[string]int{"Done": 1})
	DriverErrorsTotal.WithLabelValues("doomed", "update_status").Inc()

	DeleteDriverPolicySeries("doomed")

	if got := testutil.CollectAndCount(DriverPolicyNodes); got != 1 {
		t.Errorf("node-state series after delete = %d; want 1 (survivor only)", got)
	}
	if got := testutil.CollectAndCount(DriverErrorsTotal); got != 0 {
		t.Errorf("error series after delete = %d; want 0", got)
	}
}

func TestDeleteFirmwarePolicySeries(t *testing.T) {
	t.Cleanup(FirmwareFlashDuration.Reset)
	t.Cleanup(FirmwareFlashJobsTotal.Reset)

	FirmwareFlashDuration.WithLabelValues("doomed", ResultSuccess).Observe(42)
	FirmwareFlashJobsTotal.WithLabelValues("doomed", ResultSuccess).Inc()
	FirmwareFlashJobsTotal.WithLabelValues("survivor", ResultSuccess).Inc()

	DeleteFirmwarePolicySeries("doomed")

	if got := testutil.CollectAndCount(FirmwareFlashDuration); got != 0 {
		t.Errorf("histogram series after delete = %d; want 0", got)
	}
	if got := testutil.CollectAndCount(FirmwareFlashJobsTotal); got != 1 {
		t.Errorf("job-total series after delete = %d; want 1 (survivor only)", got)
	}
}

func TestEventDedup_FirstSeenThenRetain(t *testing.T) {
	d := NewEventDedup()

	if !d.FirstSeen("cr-a", "job/one") {
		t.Fatal("first sighting reported as duplicate")
	}
	if d.FirstSeen("cr-a", "job/one") {
		t.Error("second sighting reported as new")
	}
	// Scopes are independent: two CRs can hold the same key.
	if !d.FirstSeen("cr-b", "job/one") {
		t.Error("same key under a different CR reported as duplicate")
	}

	// The Job disappears (TTL), so its key is pruned. If it comes back it's
	// a genuinely new flash and must count again.
	d.Retain("cr-a", []string{"job/two"})
	if !d.FirstSeen("cr-a", "job/one") {
		t.Error("key not forgotten by Retain")
	}
	if d.FirstSeen("cr-b", "job/one") {
		t.Error("Retain on cr-a leaked into cr-b")
	}
}

func TestEventDedup_RetainKeepsLiveKeys(t *testing.T) {
	d := NewEventDedup()

	d.FirstSeen("cr", "drain-timeout/node-1")
	d.Retain("cr", []string{"drain-timeout/node-1"})

	// Still timed out, so the stall must not be counted a second time.
	if d.FirstSeen("cr", "drain-timeout/node-1") {
		t.Error("Retain dropped a key that is still live")
	}
}

func TestEventDedup_Forget(t *testing.T) {
	d := NewEventDedup()

	d.FirstSeen("cr", "job/one")
	d.Forget("cr")
	if !d.FirstSeen("cr", "job/one") {
		t.Error("Forget left the scope's history behind")
	}
}

// Registration into controller-runtime's registry (not the default one) is
// what makes these show up on --metrics-bind-address. A promauto default
// would compile and export nothing, so assert on the wiring.
func TestMetricsRegisteredWithControllerRuntimeRegistry(t *testing.T) {
	t.Cleanup(DriverNodesByKMDVersion.Reset)
	t.Cleanup(FirmwareFlashJobsTotal.Reset)

	SetNodesByKMDVersion(map[VersionMode]int{{Version: "2.8.0", InstallMode: "host"}: 1})
	FirmwareFlashJobsTotal.WithLabelValues("default", ResultFailed).Inc()

	want := map[string]bool{
		"ttdriver_nodes_by_kmd_version": false,
		"ttfw_flash_jobs_total":         false,
	}
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s not served by the controller-runtime registry", name)
		}
	}
}
