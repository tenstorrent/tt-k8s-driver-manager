// Package binder implements the vfio-pci bind/unbind loop. It continuously
// asserts that every configured PCI device is bound to vfio-pci,
// re-asserting on each tick so a device that drifts off the driver (host
// reboot, driver reload) is picked back up without a pod restart.
package binder

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/config"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/metrics"
)

const (
	vfioPCIDriver = "vfio-pci"
	// vfioIOMMUModule owns the dma_entry_limit tunable raised by
	// SetDMAEntryLimit.
	vfioIOMMUModule = "vfio_iommu_type1"
)

// sysfsRoot is the mount point of the host's sysfs. Overridden by tests to
// point at a fixture tree; in the DaemonSet the host /sys is mounted here.
var sysfsRoot = "/sys"

func pciDevicesDir() string { return filepath.Join(sysfsRoot, "bus/pci/devices") }
func driverDir(name string) string {
	return filepath.Join(sysfsRoot, "bus/pci/drivers", name)
}

var bdfRE = regexp.MustCompile(`^[a-f0-9]{4}:[a-f0-9]{2}:[a-f0-9]{2}\.[0-9a-f]$`)

// Binder owns the bind-loop state.
type Binder struct {
	cfg             *config.Config
	restoreOnExit   bool
	hotplugStub     string            // stub driver probed before binding; empty disables the pass
	originalDrivers map[string]string // BDF → driver the device was on before we touched it
}

// New creates a Binder from the given config. If restoreOnExit is true,
// SIGTERM triggers a best-effort re-bind to each device's original driver.
func New(cfg *config.Config, restoreOnExit bool) *Binder {
	return &Binder{
		cfg:             cfg,
		restoreOnExit:   restoreOnExit,
		originalDrivers: make(map[string]string),
	}
}

// SetHotplugStub enables the pre-bind hotplug-suppression pass, probing each
// device through the named stub driver before handing it to vfio-pci. Empty
// (the default) skips the pass entirely.
func (b *Binder) SetHotplugStub(driver string) {
	b.hotplugStub = driver
}

// RunOnce performs one bind-assertion pass: scan every PCI device, and bind
// any that matches the config and is not already on vfio-pci.
func (b *Binder) RunOnce() error {
	devices, err := scanAllPCI()
	if err != nil {
		return fmt.Errorf("scanning PCI: %w", err)
	}

	// Seed every configured resource at zero so a resource whose devices all
	// disappear reads as 0 rather than holding its last value.
	boundPerResource := make(map[string]float64)
	for _, grp := range b.cfg.Devices {
		boundPerResource[grp.ResourceName] = 0
	}

	for _, dev := range devices {
		resourceName, ok := b.matchesConfigName(dev)
		if !ok {
			continue
		}

		if dev.OriginalDriver == vfioPCIDriver {
			boundPerResource[resourceName]++
			continue
		}

		log.Printf("binder: binding %s (current driver: %q) to vfio-pci", dev.BDF, dev.OriginalDriver)

		// Remember the original driver the first time we see this device, so
		// a later restore doesn't record vfio-pci as the "original".
		if _, seen := b.originalDrivers[dev.BDF]; !seen && dev.OriginalDriver != "" {
			b.originalDrivers[dev.BDF] = dev.OriginalDriver
		}

		if err := bindToVFIO(dev, b.hotplugStub); err != nil {
			log.Printf("binder: failed to bind %s: %v", dev.BDF, err)
			metrics.BindErrors.WithLabelValues("bind").Inc()
		} else {
			boundPerResource[resourceName]++
		}
	}

	for resource, count := range boundPerResource {
		metrics.DevicesBound.WithLabelValues(resource).Set(count)
	}

	return nil
}

// Restore re-binds each touched device back to its original driver. Called
// on graceful shutdown when restoreOnExit is set.
func (b *Binder) Restore() {
	if !b.restoreOnExit {
		return
	}
	for bdf, origDriver := range b.originalDrivers {
		log.Printf("binder: restoring %s to %s", bdf, origDriver)
		if err := restoreDriver(bdf, origDriver); err != nil {
			log.Printf("binder: restore %s failed: %v", bdf, err)
			metrics.BindErrors.WithLabelValues("unbind").Inc()
		}
	}
}

// EnsureVFIOPCILoaded makes sure the vfio-pci kernel module is loaded on the
// host. No-op when the driver is already present. Requires a privileged
// container with the host's /lib/modules bind-mounted so modprobe(8) can
// find the .ko.
func EnsureVFIOPCILoaded() error {
	if _, err := os.Stat(driverDir(vfioPCIDriver)); err == nil {
		return nil
	}
	out, err := exec.Command("modprobe", vfioPCIDriver).CombinedOutput()
	if err != nil {
		return fmt.Errorf("modprobe vfio-pci: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat(driverDir(vfioPCIDriver)); err != nil {
		return fmt.Errorf("vfio-pci still missing after modprobe: %w", err)
	}
	return nil
}

// NoiommuActive reports whether vfio is running without an IOMMU.
func NoiommuActive() bool {
	data, err := os.ReadFile(filepath.Join(sysfsRoot, "module/vfio/parameters/enable_unsafe_noiommu_mode"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "Y"
}

// RunLoop calls RunOnce immediately, then on every tick, until done closes.
func (b *Binder) RunLoop(interval time.Duration, done <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := b.RunOnce(); err != nil {
		log.Printf("binder: initial bind pass: %v", err)
	}

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := b.RunOnce(); err != nil {
				log.Printf("binder: bind pass error: %v", err)
			}
		}
	}
}

type pciDevice struct {
	BDF            string
	VendorID       string
	DeviceID       string
	OriginalDriver string
}

func scanAllPCI() ([]pciDevice, error) {
	dir := pciDevicesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	var devices []pciDevice
	for _, e := range entries {
		name := e.Name()
		if !bdfRE.MatchString(name) {
			continue
		}

		base := filepath.Join(dir, name)

		vendor, err := readHex(filepath.Join(base, "vendor"))
		if err != nil {
			continue
		}
		device, err := readHex(filepath.Join(base, "device"))
		if err != nil {
			continue
		}

		currentDriver := ""
		if dest, err := os.Readlink(filepath.Join(base, "driver")); err == nil {
			currentDriver = filepath.Base(dest)
		}

		devices = append(devices, pciDevice{
			BDF:            name,
			VendorID:       vendor,
			DeviceID:       device,
			OriginalDriver: currentDriver,
		})
	}

	return devices, nil
}

// matchesConfigName returns the resource name of the first config group the
// device matches.
func (b *Binder) matchesConfigName(dev pciDevice) (string, bool) {
	for _, grp := range b.cfg.Devices {
		if !strings.EqualFold(dev.VendorID, grp.VendorID) {
			continue
		}
		for _, id := range grp.DeviceIDs {
			if strings.EqualFold(dev.DeviceID, id) {
				return grp.ResourceName, true
			}
		}
	}
	return "", false
}

// bindToVFIO unbinds a device from its current driver and binds it to vfio-pci.
// When hotplugStub is set, the device is first probed through that stub driver
// to suppress PCIe hotplug (see suppressHotplug).
func bindToVFIO(dev pciDevice, hotplugStub string) error {
	base := filepath.Join(pciDevicesDir(), dev.BDF)

	if dev.OriginalDriver != "" && dev.OriginalDriver != vfioPCIDriver {
		if err := writeFile(filepath.Join(base, "driver", "unbind"), dev.BDF); err != nil {
			return fmt.Errorf("unbind from %s: %w", dev.OriginalDriver, err)
		}
	}

	// Between the unbind and the vfio-pci bind is the only point where the
	// device is guaranteed unbound, which is what drivers_probe needs.
	if hotplugStub != "" {
		if err := suppressHotplug(dev.BDF, hotplugStub); err != nil {
			return err
		}
	}

	// driver_override makes the kernel offer this device to vfio-pci only.
	overridePath := filepath.Join(base, "driver_override")
	if err := writeFile(overridePath, vfioPCIDriver); err != nil {
		return fmt.Errorf("set driver_override: %w", err)
	}

	if err := writeFile(filepath.Join(driverDir(vfioPCIDriver), "bind"), dev.BDF); err != nil {
		// Clear the override so a failed bind doesn't strand the device.
		_ = clearDriverOverride(overridePath)
		return fmt.Errorf("bind to vfio-pci: %w", err)
	}

	return nil
}

// suppressHotplug probes an unbound device through the stub driver so the
// stub can call pci_ignore_hotplug() on it. The stub's probe returns -ENODEV
// by design, so a successful pass leaves the device unbound and ready for the
// vfio-pci bind that follows — which overwrites driver_override on its way.
//
// Without this, a Galaxy PCIe switch reports link-down when vfio-pci resets
// the device and pciehp tears it off the bus mid-bind.
func suppressHotplug(bdf, stubDriver string) error {
	overridePath := filepath.Join(pciDevicesDir(), bdf, "driver_override")

	if err := writeFile(overridePath, stubDriver); err != nil {
		return fmt.Errorf("set driver_override to %s: %w", stubDriver, err)
	}

	if err := writeFile(filepath.Join(sysfsRoot, "bus/pci/drivers_probe"), bdf); err != nil {
		// Leave no override behind, or the device is stranded on a driver
		// that refuses to bind it.
		_ = clearDriverOverride(overridePath)
		return fmt.Errorf("probe %s through %s: %w", bdf, stubDriver, err)
	}

	return nil
}

// HotplugStubRegistered reports whether the named stub driver is registered
// with the PCI bus. The initContainer is what builds and loads it, so a false
// here means that step did not run or did not succeed.
func HotplugStubRegistered(driver string) bool {
	_, err := os.Stat(driverDir(driver))
	return err == nil
}

// SetDMAEntryLimit raises vfio_iommu_type1's dma_entry_limit. The default
// (65535) is well short of what a Galaxy guest needs to map every device BAR,
// and exhausting it surfaces as an opaque VFIO_IOMMU_MAP_DMA failure at VM
// start rather than anything naming the limit.
func SetDMAEntryLimit(limit int) error {
	path := filepath.Join(sysfsRoot, "module", vfioIOMMUModule, "parameters/dma_entry_limit")

	if _, err := os.Stat(path); err != nil {
		out, mErr := exec.Command("modprobe", vfioIOMMUModule).CombinedOutput()
		if mErr != nil {
			return fmt.Errorf("modprobe %s: %w (%s)", vfioIOMMUModule, mErr, strings.TrimSpace(string(out)))
		}
	}

	want := strconv.Itoa(limit)
	if err := writeFile(path, want); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	// The parameter is writable but clamped by the kernel, so read it back
	// rather than trusting the write.
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading back %s: %w", path, err)
	}
	if strings.TrimSpace(string(got)) != want {
		return fmt.Errorf("%s is %s after write, expected %s", path, strings.TrimSpace(string(got)), want)
	}

	return nil
}

// restoreDriver re-binds a BDF to the given driver name.
func restoreDriver(bdf, driverName string) error {
	if driverName == "" {
		return nil
	}

	base := filepath.Join(pciDevicesDir(), bdf)

	if err := writeFile(filepath.Join(base, "driver", "unbind"), bdf); err != nil {
		return fmt.Errorf("unbind from vfio-pci: %w", err)
	}

	_ = clearDriverOverride(filepath.Join(base, "driver_override"))

	if err := writeFile(filepath.Join(driverDir(driverName), "bind"), bdf); err != nil {
		// The original driver may not be loaded any more; let the kernel pick.
		return writeFile(filepath.Join(sysfsRoot, "bus/pci/drivers_probe"), bdf)
	}

	return nil
}

func readHex(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	val := strings.TrimSpace(string(data))
	val = strings.TrimPrefix(val, "0x")
	return strings.ToLower(val), nil
}

// clearDriverOverride resets a device's driver_override so the kernel is
// free to match it against any driver again. The value must be a newline,
// not the empty string: a zero-length write never reaches the kernel's
// store handler, leaving the override — and therefore the vfio-pci pin — in
// place.
func clearDriverOverride(path string) error {
	return writeFile(path, "\n")
}

func writeFile(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(value)
	return err
}
