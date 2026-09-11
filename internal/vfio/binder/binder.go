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
	"strings"
	"time"

	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/config"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/metrics"
)

const vfioPCIDriver = "vfio-pci"

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

		if err := bindToVFIO(dev); err != nil {
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
func bindToVFIO(dev pciDevice) error {
	base := filepath.Join(pciDevicesDir(), dev.BDF)

	if dev.OriginalDriver != "" && dev.OriginalDriver != vfioPCIDriver {
		if err := writeFile(filepath.Join(base, "driver", "unbind"), dev.BDF); err != nil {
			return fmt.Errorf("unbind from %s: %w", dev.OriginalDriver, err)
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
