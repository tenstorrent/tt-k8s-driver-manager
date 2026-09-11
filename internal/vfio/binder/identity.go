package binder

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/metrics"
)

// Identity is what tt-kmd knew about a device before we took it away.
//
// n150 and n300 share PCI ID 1e52:401e — the board type lives in ARC
// telemetry, which only tt-kmd can read (it exposes it as the tt_card_type
// and tt_serial sysfs attributes on the PCI device). Once the device is on
// vfio-pci those attributes are gone, so the binder reads them in the window
// where the device is still on tt-kmd and persists them to a state file that
// outlives pod restarts.
type Identity struct {
	BoardType string `json:"boardType"`
	Serial    string `json:"serial"`
}

// identityState is the on-disk format of the state file.
type identityState struct {
	Devices map[string]Identity `json:"devices"` // keyed by BDF
}

// UseStateFile loads previously recorded identities from path and arranges
// for newly learned ones to be saved back to it. A missing file is not an
// error — it just means no device has been identified yet.
func (b *Binder) UseStateFile(path string) error {
	b.statePath = path

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading identity state %s: %w", path, err)
	}

	var state identityState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parsing identity state %s: %w", path, err)
	}
	for bdf, id := range state.Devices {
		b.identities[bdf] = id
	}
	log.Printf("identity: loaded %d device identities from %s", len(state.Devices), path)
	return nil
}

func (b *Binder) saveState() {
	if b.statePath == "" {
		return
	}
	data, err := json.MarshalIndent(identityState{Devices: b.identities}, "", "  ")
	if err != nil {
		log.Printf("identity: marshalling state: %v", err)
		return
	}
	// Write-and-rename so a crash mid-write can't truncate the state file.
	tmp := b.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("identity: writing %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, b.statePath); err != nil {
		log.Printf("identity: renaming %s: %v", tmp, err)
	}
}

// identify records the device's board identity if it isn't known yet and the
// tt-kmd telemetry attributes are still readable. Returns the identity, with
// BoardType "unknown" when the device was never seen on tt-kmd (e.g. it was
// already on vfio-pci when this daemon first started and no state file entry
// exists).
func (b *Binder) identify(dev pciDevice) Identity {
	if id, ok := b.identities[dev.BDF]; ok {
		return id
	}

	base := filepath.Join(pciDevicesDir(), dev.BDF)
	cardType, err := readTelemetryAttr(base, "tt_card_type")
	if err != nil {
		// Not on tt-kmd (or a tt-kmd too old to expose telemetry attrs);
		// nothing to read. Don't cache: a later pass may catch the device
		// on tt-kmd after a drift.
		return Identity{BoardType: "unknown"}
	}
	serial, err := readTelemetryAttr(base, "tt_serial")
	if err != nil {
		serial = ""
	}

	id := Identity{BoardType: cardType, Serial: serial}
	b.identities[dev.BDF] = id
	log.Printf("identity: %s is a %s (serial %s)", dev.BDF, id.BoardType, id.Serial)
	b.saveState()
	return id
}

// publishIdentities re-exports the tt_vfio_device_info series for the
// devices matched in this pass. Reset first so unplugged devices drop out.
func publishIdentities(seen map[string]deviceIdentity) {
	metrics.DeviceInfo.Reset()
	for bdf, di := range seen {
		metrics.DeviceInfo.WithLabelValues(bdf, di.resource, di.id.BoardType, di.id.Serial).Set(1)
	}
}

type deviceIdentity struct {
	resource string
	id       Identity
}

// readTelemetryAttr reads a tt-kmd telemetry attribute for a PCI device.
//
// tt-kmd attaches the telemetry attribute group to its class device
// (named "tenstorrent/<N>", which sysfs renders as "tenstorrent!<N>"),
// whose parent is the PCI device — so the attribute lives at
// /sys/bus/pci/devices/<bdf>/tenstorrent!<N>/tt_card_type, not on the PCI
// device node itself. The ordinal N is assigned in probe order, so glob
// for it.
func readTelemetryAttr(pciDevDir, attr string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(pciDevDir, "tenstorrent!*", attr))
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("no tt-kmd telemetry attribute %s under %s", attr, pciDevDir)
	}
	return readTrimmed(matches[0])
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
