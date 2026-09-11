package binder

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/metrics"
)

// The other half of the driver_override bug fix: when the bind write fails,
// the override must be cleared with a newline write, or the device is left
// pinned to vfio-pci with no driver at all.
func TestRunOnce_FailedBindClearsDriverOverride(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "tenstorrent")

	// Make the bind attribute unwritable so the bind itself fails after the
	// unbind and override writes have gone through.
	bindPath := filepath.Join(f.root, "bus/pci/drivers/vfio-pci/bind")
	if err := os.Remove(bindPath); err != nil {
		t.Fatal(err)
	}

	before := testutil.ToFloat64(metrics.BindErrors.WithLabelValues("bind"))

	// RunOnce logs the failure but does not return it — one bad device must
	// not stop the pass for the others.
	if err := New(wormholeConfig(), false).RunOnce(); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, "bus/pci/devices/0000:01:00.0/driver_override"); got != "\n" {
		t.Errorf("driver_override after failed bind = %q; want a newline (cleared)", got)
	}
	if got := testutil.ToFloat64(metrics.BindErrors.WithLabelValues("bind")); got != before+1 {
		t.Errorf("bind_errors_total = %v; want %v", got, before+1)
	}
}

// A device that drifts back to its original driver between ticks (driver
// reload, host-side rebind) must be re-bound on the next pass, and the
// remembered original driver must not be clobbered by the second sighting.
func TestRunOnce_RebindsDriftedDevice(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "tenstorrent")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	// The kernel would repoint the symlink on bind; the fake does it here.
	// Then the device drifts back to tt-kmd, as after a driver reload.
	f.setDriver(t, "0000:01:00.0", "vfio-pci")
	f.setDriver(t, "0000:01:00.0", "tenstorrent")
	// Clear the attribute files so the second pass's writes are visible.
	write(t, filepath.Join(f.root, "bus/pci/drivers/vfio-pci/bind"), "")

	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, "bus/pci/drivers/vfio-pci/bind"); got != "0000:01:00.0" {
		t.Errorf("second pass vfio-pci/bind = %q; want the BDF", got)
	}
	if got := b.originalDrivers["0000:01:00.0"]; got != "tenstorrent" {
		t.Errorf("originalDrivers after drift = %q; want tenstorrent, not overwritten", got)
	}
}

// DevicesBound must be seeded at zero for every configured resource, so a
// resource whose devices all vanish reads 0 rather than its last value.
func TestRunOnce_DevicesBoundGauge(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "vfio-pci")
	f.addDevice(t, "0000:02:00.0", "1e52", "401e", "vfio-pci")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}
	gauge := metrics.DevicesBound.WithLabelValues("tenstorrent.com/wormhole")
	if got := testutil.ToFloat64(gauge); got != 2 {
		t.Errorf("devices_bound_total = %v; want 2", got)
	}

	// Both devices disappear (hot-unplug); the next pass must read 0.
	for _, bdf := range []string{"0000:01:00.0", "0000:02:00.0"} {
		if err := os.RemoveAll(filepath.Join(f.root, "bus/pci/devices", bdf)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(gauge); got != 0 {
		t.Errorf("devices_bound_total after unplug = %v; want 0", got)
	}
}

// A bind failure on one device must not prevent binding the others in the
// same pass.
func TestRunOnce_OneFailureDoesNotStopThePass(t *testing.T) {
	f := newFakeSysfs(t)
	// 01:00.0 will fail: its unbind attribute is missing.
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "tenstorrent")
	if err := os.Remove(filepath.Join(f.root, "bus/pci/drivers/tenstorrent/unbind")); err != nil {
		t.Fatal(err)
	}
	f.addDevice(t, "0000:02:00.0", "1e52", "401e", "")

	if err := New(wormholeConfig(), false).RunOnce(); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, "bus/pci/drivers/vfio-pci/bind"); got != "0000:02:00.0" {
		t.Errorf("vfio-pci/bind = %q; want the healthy device bound", got)
	}
}
