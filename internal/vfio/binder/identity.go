package binder

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tenstorrent/tt-k8s-driver-manager/internal/vfio/metrics"
)

// Identity is a device's board identity.
//
// n150 and n300 share PCI ID 1e52:401e, so the SKU needs more than the
// device ID. Two sources, in order:
//
//  1. The PCI subsystem device ID. Verified on n150 hardware: it carries the
//     same board-type code tt-kmd decodes from telemetry (0x0018 = n150), and
//     config space stays readable no matter which driver is bound — including
//     vfio-pci. This is how NVIDIA-style config-space identification works.
//  2. tt-kmd's telemetry sysfs attributes (tt_card_type/tt_serial), readable
//     only while the device is on tt-kmd. Used as a fallback for boards whose
//     subsystem ID is not in the table, and as the only source for the serial
//     number; results persist to a state file that outlives pod restarts.
type Identity struct {
	BoardType string `json:"boardType"`
	Serial    string `json:"serial"`
}

// boardTypeBySubsystem maps the PCI subsystem device ID to the board type.
// The code space is the same one tt-kmd's tt_card_type decode uses (tt-kmd
// telemetry.c) — confirmed on hardware for n150 (subsystem_device=0x0018).
var boardTypeBySubsystem = map[uint16]string{
	// Wormhole
	0x0014: "n300",
	0x0018: "n150",
	0x0035: "galaxy-wormhole",
	// Blackhole
	0x0036: "p100",
	0x0040: "p150a",
	0x0041: "p150b",
	0x0042: "p150c",
	0x0043: "p100a",
	0x0044: "p300b",
	0x0045: "p300a",
	0x0046: "p300c",
	0x0047: "galaxy-blackhole",
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

// identify determines the device's board identity.
//
// Order: cached (memory/state file) → PCI subsystem ID (works regardless of
// bound driver) → tt-kmd telemetry (only while on tt-kmd). The serial number
// only exists in telemetry, so a subsystem-identified device still upgrades
// its entry when telemetry becomes readable. BoardType is "unknown" only
// when every source fails.
func (b *Binder) identify(dev pciDevice) Identity {
	cached, haveCached := b.identities[dev.BDF]
	if haveCached && cached.Serial != "" {
		return cached
	}

	base := filepath.Join(pciDevicesDir(), dev.BDF)

	// Telemetry is the richest source (board type + serial); take it
	// whenever the device happens to be on tt-kmd.
	if cardType, err := readTelemetryAttr(base, "tt_card_type"); err == nil {
		serial, err := readTelemetryAttr(base, "tt_serial")
		if err != nil {
			serial = ""
		}
		id := Identity{BoardType: cardType, Serial: serial}
		b.identities[dev.BDF] = id
		log.Printf("identity: %s is a %s (serial %s, via telemetry)", dev.BDF, id.BoardType, id.Serial)
		b.saveState()
		return id
	}

	if haveCached {
		return cached
	}

	// Config space: readable even on vfio-pci.
	if boardType, ok := boardTypeFromSubsystem(base); ok {
		id := Identity{BoardType: boardType}
		b.identities[dev.BDF] = id
		log.Printf("identity: %s is a %s (via subsystem ID)", dev.BDF, id.BoardType)
		b.saveState()
		return id
	}

	// Nothing worked. Don't cache: a later pass may catch the device on
	// tt-kmd after a drift, or the subsystem ID may join the table.
	return Identity{BoardType: "unknown"}
}

// boardTypeFromSubsystem identifies the board from the PCI subsystem device
// ID, which sysfs exposes for every PCI device regardless of driver.
func boardTypeFromSubsystem(pciDevDir string) (string, bool) {
	raw, err := readTrimmed(filepath.Join(pciDevDir, "subsystem_device"))
	if err != nil {
		return "", false
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(raw, "0x"), 16, 16)
	if err != nil {
		return "", false
	}
	boardType, ok := boardTypeBySubsystem[uint16(v)]
	return boardType, ok
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
// tt-kmd attaches the telemetry attribute group to its class device, a
// child of the PCI device. Verified on an n150 hardware runner (kmd 2.8.0),
// the attribute path is:
//
//	/sys/bus/pci/devices/<bdf>/tenstorrent/tenstorrent!<N>/tt_card_type
//
// (an intermediate "tenstorrent" dir, then the class device named
// "tenstorrent/<N>" which sysfs renders as "tenstorrent!<N>"). The ordinal
// N is assigned in probe order, so glob for it. Both layouts are tried in
// case the intermediate dir is version-dependent.
func readTelemetryAttr(pciDevDir, attr string) (string, error) {
	for _, pattern := range []string{
		filepath.Join(pciDevDir, "tenstorrent", "tenstorrent!*", attr),
		filepath.Join(pciDevDir, "tenstorrent!*", attr),
	} {
		if matches, err := filepath.Glob(pattern); err == nil && len(matches) > 0 {
			return readTrimmed(matches[0])
		}
	}
	return "", fmt.Errorf("no tt-kmd telemetry attribute %s under %s", attr, pciDevDir)
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
