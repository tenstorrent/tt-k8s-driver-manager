package binder

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/metrics"
)

// addIdentity writes the tt-kmd telemetry attributes onto a fake device.
// They live on tt-kmd's class device ("tenstorrent/<N>" → "tenstorrent!<N>"
// in sysfs), which is a child of the PCI device — matching real tt-kmd
// (verified on an n150 hardware runner; the attrs are NOT on the PCI node).
func (f *fakeSysfs) addIdentity(t *testing.T, bdf, cardType, serial string) {
	t.Helper()
	base := filepath.Join(f.root, "bus/pci/devices", bdf, "tenstorrent", "tenstorrent!0")
	mkdir(t, base)
	write(t, filepath.Join(base, "tt_card_type"), cardType+"\n")
	write(t, filepath.Join(base, "tt_serial"), serial+"\n")
}

func deviceInfoValue(t *testing.T, bdf, resource, boardType, serial string) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.DeviceInfo.WithLabelValues(bdf, resource, boardType, serial))
}

// setSubsystem writes the PCI subsystem IDs, present regardless of driver.
func (f *fakeSysfs) setSubsystem(t *testing.T, bdf, subsysDevice string) {
	t.Helper()
	base := filepath.Join(f.root, "bus/pci/devices", bdf)
	write(t, filepath.Join(base, "subsystem_vendor"), "0x1e52\n")
	write(t, filepath.Join(base, "subsystem_device"), "0x"+subsysDevice+"\n")
}

// A device already on vfio-pci with no telemetry and no state file is still
// identified from its PCI subsystem ID (config space survives the driver
// change) — verified on hardware: n150 reports subsystem_device 0x0018.
func TestIdentify_SubsystemIDWhenAlreadyOnVFIO(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "vfio-pci")
	f.setSubsystem(t, "0000:01:00.0", "0014")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	if id := b.identities["0000:01:00.0"]; id.BoardType != "n300" {
		t.Errorf("identities = %+v; want n300 via subsystem ID 0x0014", b.identities)
	}
	if got := deviceInfoValue(t, "0000:01:00.0", "tenstorrent.com/wormhole", "n300", ""); got != 1 {
		t.Errorf("tt_vfio_device_info{board_type=n300} = %v; want 1", got)
	}
}

// Telemetry outranks the subsystem table when the device is on tt-kmd: it
// carries the serial, which config space cannot provide. A subsystem-only
// entry is upgraded when telemetry becomes readable.
func TestIdentify_TelemetryUpgradesSubsystemEntry(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "vfio-pci")
	f.setSubsystem(t, "0000:01:00.0", "0018")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if id := b.identities["0000:01:00.0"]; id.BoardType != "n150" || id.Serial != "" {
		t.Fatalf("first pass identities = %+v; want serial-less n150", b.identities)
	}

	// Device drifts to tt-kmd; telemetry now readable.
	f.setDriver(t, "0000:01:00.0", "tenstorrent")
	f.addDriver(t, "tenstorrent")
	f.addIdentity(t, "0000:01:00.0", "n150", "0100181170800042")
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	if id := b.identities["0000:01:00.0"]; id.Serial != "0100181170800042" {
		t.Errorf("identities after telemetry = %+v; want the serial filled in", b.identities)
	}
}

// An unrecognised subsystem ID must not mislabel the board.
func TestIdentify_UnknownSubsystemIDStaysUnknown(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "vfio-pci")
	f.setSubsystem(t, "0000:01:00.0", "9999")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	if _, cached := b.identities["0000:01:00.0"]; cached {
		t.Errorf("identities = %+v; an unmapped subsystem ID must not be cached", b.identities)
	}
	if got := deviceInfoValue(t, "0000:01:00.0", "tenstorrent.com/wormhole", "unknown", ""); got != 1 {
		t.Errorf("tt_vfio_device_info{board_type=unknown} = %v; want 1", got)
	}
}

func TestIdentify_ReadsCardTypeBeforeBind(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "tenstorrent")
	f.addIdentity(t, "0000:01:00.0", "n300", "010014511708001d")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	id, ok := b.identities["0000:01:00.0"]
	if !ok || id.BoardType != "n300" || id.Serial != "010014511708001d" {
		t.Errorf("identities = %+v; want n300 with the serial", b.identities)
	}
	if got := deviceInfoValue(t, "0000:01:00.0", "tenstorrent.com/wormhole", "n300", "010014511708001d"); got != 1 {
		t.Errorf("tt_vfio_device_info = %v; want 1", got)
	}
}

func TestIdentify_UnknownWhenAlreadyOnVFIO(t *testing.T) {
	f := newFakeSysfs(t)
	// Already on vfio-pci and never seen on tt-kmd: no telemetry attrs.
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "vfio-pci")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	if _, cached := b.identities["0000:01:00.0"]; cached {
		t.Error("an unidentifiable device must not be cached, a later pass may catch it on tt-kmd")
	}
	if got := deviceInfoValue(t, "0000:01:00.0", "tenstorrent.com/wormhole", "unknown", ""); got != 1 {
		t.Errorf("tt_vfio_device_info{board_type=unknown} = %v; want 1", got)
	}
}

func TestIdentify_LearnsWhenDeviceDriftsBackToKmd(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "vfio-pci")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	// A driver reload put the device back on tt-kmd; telemetry is readable
	// again and the next pass must pick the identity up before re-binding.
	f.setDriver(t, "0000:01:00.0", "tenstorrent")
	f.addDriver(t, "tenstorrent")
	f.addIdentity(t, "0000:01:00.0", "n150", "0100181170800042")
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	if id := b.identities["0000:01:00.0"]; id.BoardType != "n150" {
		t.Errorf("identities after drift = %+v; want n150 learned", b.identities)
	}
}

// A pod restart with the device already on vfio-pci and no telemetry: the
// board type must still resolve, purely from config space — the reason no
// state file is needed.
func TestIdentify_SurvivesRestartViaSubsystemID(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "tenstorrent")
	f.setSubsystem(t, "0000:01:00.0", "0014")
	f.addIdentity(t, "0000:01:00.0", "n300", "010014511708001d")

	b := New(wormholeConfig(), false)
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	// "Restart": a fresh binder, device on vfio-pci, telemetry gone.
	f.setDriver(t, "0000:01:00.0", "vfio-pci")
	if err := os.RemoveAll(filepath.Join(f.root, "bus/pci/devices/0000:01:00.0/tenstorrent")); err != nil {
		t.Fatal(err)
	}

	b2 := New(wormholeConfig(), false)
	if err := b2.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if id := b2.identities["0000:01:00.0"]; id.BoardType != "n300" {
		t.Errorf("identities after restart = %+v; want n300 via subsystem ID", b2.identities)
	}
	// The serial was telemetry-only and is legitimately gone after restart.
	if got := deviceInfoValue(t, "0000:01:00.0", "tenstorrent.com/wormhole", "n300", ""); got != 1 {
		t.Errorf("tt_vfio_device_info{board_type=n300} = %v; want 1", got)
	}
}
