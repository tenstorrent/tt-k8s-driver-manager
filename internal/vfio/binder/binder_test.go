package binder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/config"
)

// fakeSysfs builds a throwaway sysfs tree and points the package at it for
// the duration of the test.
type fakeSysfs struct{ root string }

func newFakeSysfs(t *testing.T) *fakeSysfs {
	t.Helper()
	root := t.TempDir()

	prev := sysfsRoot
	sysfsRoot = root
	t.Cleanup(func() { sysfsRoot = prev })

	mkdir(t, filepath.Join(root, "bus/pci/devices"))
	mkdir(t, filepath.Join(root, "bus/pci/drivers"))
	write(t, filepath.Join(root, "bus/pci/drivers_probe"), "")

	f := &fakeSysfs{root: root}
	f.addDriver(t, "vfio-pci")
	return f
}

// addDriver creates /bus/pci/drivers/<name> with the bind+unbind attributes.
func (f *fakeSysfs) addDriver(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(f.root, "bus/pci/drivers", name)
	mkdir(t, dir)
	write(t, filepath.Join(dir, "bind"), "")
	write(t, filepath.Join(dir, "unbind"), "")
}

// addDevice creates a PCI device. driver == "" leaves it unbound.
func (f *fakeSysfs) addDevice(t *testing.T, bdf, vendor, device, driver string) {
	t.Helper()
	base := filepath.Join(f.root, "bus/pci/devices", bdf)
	mkdir(t, base)
	write(t, filepath.Join(base, "vendor"), "0x"+vendor+"\n")
	write(t, filepath.Join(base, "device"), "0x"+device+"\n")
	write(t, filepath.Join(base, "driver_override"), "")

	if driver != "" {
		f.addDriver(t, driver)
		if err := os.Symlink(filepath.Join("../../drivers", driver), filepath.Join(base, "driver")); err != nil {
			t.Fatal(err)
		}
	}
}

// setDriver repoints a device's driver symlink, standing in for the kernel's
// half of a bind.
func (f *fakeSysfs) setDriver(t *testing.T, bdf, driver string) {
	t.Helper()
	link := filepath.Join(f.root, "bus/pci/devices", bdf, "driver")
	_ = os.Remove(link)
	if err := os.Symlink(filepath.Join("../../drivers", driver), link); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeSysfs) read(t *testing.T, parts ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{f.root}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func wormholeConfig() *config.Config {
	return &config.Config{Devices: []config.DeviceGroup{{
		ResourceName: "tenstorrent.com/wormhole",
		VendorID:     "1e52",
		DeviceIDs:    []string{"401e"},
	}}}
}

func TestRunOnce_BindsMatchingDevice(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "tenstorrent")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, "bus/pci/devices/0000:01:00.0/driver_override"); got != "vfio-pci" {
		t.Errorf("driver_override = %q; want vfio-pci", got)
	}
	if got := f.read(t, "bus/pci/drivers/vfio-pci/bind"); got != "0000:01:00.0" {
		t.Errorf("vfio-pci/bind = %q; want the BDF", got)
	}
	// The device must be unbound from its old driver first, or the bind fails.
	if got := f.read(t, "bus/pci/drivers/tenstorrent/unbind"); got != "0000:01:00.0" {
		t.Errorf("tenstorrent/unbind = %q; want the BDF", got)
	}
	// The pre-existing driver is remembered so Restore can put it back.
	if got := b.originalDrivers["0000:01:00.0"]; got != "tenstorrent" {
		t.Errorf("originalDrivers = %q; want tenstorrent", got)
	}
}

func TestRunOnce_LeavesNonMatchingDeviceAlone(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:02:00.0", "10de", "2204", "nvidia")

	if err := New(wormholeConfig(), false).RunOnce(); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, "bus/pci/devices/0000:02:00.0/driver_override"); got != "" {
		t.Errorf("driver_override on a non-matching device = %q; want empty", got)
	}
	if got := f.read(t, "bus/pci/drivers/vfio-pci/bind"); got != "" {
		t.Errorf("vfio-pci/bind = %q; want empty", got)
	}
}

func TestRunOnce_AlreadyBoundIsNotRebound(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "vfio-pci")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	// Re-binding an already-bound device is a write the kernel rejects, so
	// the pass must skip it entirely.
	if got := f.read(t, "bus/pci/drivers/vfio-pci/bind"); got != "" {
		t.Errorf("vfio-pci/bind = %q; want empty (device was already bound)", got)
	}
	if len(b.originalDrivers) != 0 {
		t.Errorf("originalDrivers = %v; want empty, vfio-pci is not an original", b.originalDrivers)
	}
}

func TestRunOnce_MatchIsCaseInsensitive(t *testing.T) {
	f := newFakeSysfs(t)
	// Real sysfs reports lowercase hex; a config written in uppercase must
	// still match.
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "")

	cfg := &config.Config{Devices: []config.DeviceGroup{{
		ResourceName: "tenstorrent.com/wormhole",
		VendorID:     "1E52",
		DeviceIDs:    []string{"401E"},
	}}}
	if err := New(cfg, false).RunOnce(); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, "bus/pci/drivers/vfio-pci/bind"); got != "0000:01:00.0" {
		t.Errorf("vfio-pci/bind = %q; want the BDF", got)
	}
}

func TestRunOnce_UnboundDeviceNeedsNoUnbind(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, "bus/pci/drivers/vfio-pci/bind"); got != "0000:01:00.0" {
		t.Errorf("vfio-pci/bind = %q; want the BDF", got)
	}
	// Nothing to restore: the device was on no driver to begin with.
	if len(b.originalDrivers) != 0 {
		t.Errorf("originalDrivers = %v; want empty", b.originalDrivers)
	}
}

func TestScanAllPCI_IgnoresNonBDFEntries(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "")
	mkdir(t, filepath.Join(f.root, "bus/pci/devices/not-a-bdf"))

	devices, err := scanAllPCI()
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("scanned %d devices; want 1", len(devices))
	}
	if devices[0].VendorID != "1e52" || devices[0].DeviceID != "401e" {
		t.Errorf("scanned %+v; want the 0x prefix stripped and lowercased", devices[0])
	}
}

func TestRestore_RebindsOriginalDriver(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "tenstorrent")

	b := New(wormholeConfig(), true)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}
	f.setDriver(t, "0000:01:00.0", "vfio-pci")
	b.Restore()

	if got := f.read(t, "bus/pci/drivers/tenstorrent/bind"); got != "0000:01:00.0" {
		t.Errorf("tenstorrent/bind = %q; want the BDF", got)
	}
	if got := strings.TrimSpace(f.read(t, "bus/pci/devices/0000:01:00.0/driver_override")); got != "" {
		t.Errorf("driver_override after restore = %q; want cleared", got)
	}
}

func TestRestore_NoopWhenDisabled(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "tenstorrent")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}
	b.Restore()

	if got := f.read(t, "bus/pci/drivers/tenstorrent/bind"); got != "" {
		t.Errorf("tenstorrent/bind = %q; want empty, restore-on-exit is off", got)
	}
}

func TestRestore_FallsBackToDriversProbe(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "tenstorrent")

	b := New(wormholeConfig(), true)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}
	f.setDriver(t, "0000:01:00.0", "vfio-pci")
	// The original driver went away while we held the device (module
	// unloaded); the kernel should be asked to re-probe instead.
	if err := os.RemoveAll(filepath.Join(f.root, "bus/pci/drivers/tenstorrent")); err != nil {
		t.Fatal(err)
	}
	b.Restore()

	if got := f.read(t, "bus/pci/drivers_probe"); got != "0000:01:00.0" {
		t.Errorf("drivers_probe = %q; want the BDF", got)
	}
}

func TestNoiommuActive(t *testing.T) {
	f := newFakeSysfs(t)

	if NoiommuActive() {
		t.Error("reported noiommu with no sysfs parameter present")
	}

	mkdir(t, filepath.Join(f.root, "module/vfio/parameters"))
	write(t, filepath.Join(f.root, "module/vfio/parameters/enable_unsafe_noiommu_mode"), "Y\n")
	if !NoiommuActive() {
		t.Error("did not report noiommu when the parameter reads Y")
	}

	write(t, filepath.Join(f.root, "module/vfio/parameters/enable_unsafe_noiommu_mode"), "N\n")
	if NoiommuActive() {
		t.Error("reported noiommu when the parameter reads N")
	}
}

func TestEnsureVFIOPCILoaded_NoopWhenDriverPresent(t *testing.T) {
	newFakeSysfs(t) // seeds bus/pci/drivers/vfio-pci

	// Must not shell out to modprobe when the driver directory already
	// exists — the test host has no vfio-pci and modprobe would fail.
	if err := EnsureVFIOPCILoaded(); err != nil {
		t.Errorf("EnsureVFIOPCILoaded() = %v; want nil when vfio-pci is present", err)
	}
}
