package binder

import (
	"encoding/json"
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
	base := filepath.Join(f.root, "bus/pci/devices", bdf, "tenstorrent!0")
	mkdir(t, base)
	write(t, filepath.Join(base, "tt_card_type"), cardType+"\n")
	write(t, filepath.Join(base, "tt_serial"), serial+"\n")
}

func deviceInfoValue(t *testing.T, bdf, resource, boardType, serial string) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.DeviceInfo.WithLabelValues(bdf, resource, boardType, serial))
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

func TestStateFile_RoundTrip(t *testing.T) {
	f := newFakeSysfs(t)
	f.addDevice(t, "0000:01:00.0", "1e52", "401e", "tenstorrent")
	f.addIdentity(t, "0000:01:00.0", "n300", "010014511708001d")
	statePath := filepath.Join(t.TempDir(), "identity.json")

	b := New(wormholeConfig(), false)
	if err := b.UseStateFile(statePath); err != nil {
		t.Fatal(err)
	}
	if err := b.RunOnce(); err != nil {
		t.Fatal(err)
	}

	// A fresh binder (pod restart) sees the device already on vfio-pci with
	// no telemetry, but must recover the identity from the state file.
	f.setDriver(t, "0000:01:00.0", "vfio-pci")
	if err := os.RemoveAll(filepath.Join(f.root, "bus/pci/devices/0000:01:00.0/tenstorrent!0")); err != nil {
		t.Fatal(err)
	}

	b2 := New(wormholeConfig(), false)
	if err := b2.UseStateFile(statePath); err != nil {
		t.Fatal(err)
	}
	if err := b2.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if id := b2.identities["0000:01:00.0"]; id.BoardType != "n300" || id.Serial != "010014511708001d" {
		t.Errorf("restored identities = %+v; want the persisted n300", b2.identities)
	}

	// The file itself must be valid JSON with restrictive permissions.
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %o; want 600", perm)
	}
	var state identityState
	data, _ := os.ReadFile(statePath)
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
}

func TestStateFile_MissingIsNotAnError(t *testing.T) {
	newFakeSysfs(t)
	b := New(wormholeConfig(), false)
	if err := b.UseStateFile(filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Errorf("UseStateFile on a missing file = %v; want nil", err)
	}
}

func TestStateFile_CorruptIsAnError(t *testing.T) {
	newFakeSysfs(t)
	p := filepath.Join(t.TempDir(), "identity.json")
	write(t, p, "{not json")

	b := New(wormholeConfig(), false)
	if err := b.UseStateFile(p); err == nil {
		t.Error("UseStateFile accepted corrupt JSON")
	}
}
