package binder

import (
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
// device ID. Two sources:
//
//   - BoardType comes from the PCI subsystem device ID (the board's UPI),
//     readable from config space no matter which driver is bound — including
//     vfio-pci — so it needs no persisted state and survives pod restarts.
//     tt-kmd's tt_card_type telemetry attribute is the fallback for boards
//     missing from the UPI table.
//   - Serial only exists in tt-kmd's telemetry (tt_serial), readable while
//     the device is on tt-kmd; it is cached in memory and re-learned the
//     next time the device is seen there.
type Identity struct {
	BoardType string
	Serial    string
}

// boardTypeBySubsystem maps the PCI subsystem device ID to the board
// product. The code is the board's UPI, mirroring board_upi_map in tt-umd
// (device/api/umd/device/types/cluster_descriptor_types.hpp) — the same
// table tt-feature-discovery labels nodes from, so board_type here always
// agrees with the node's tenstorrent.com/product label. Hardware-confirmed
// via the e2e matrix: n150 = 0x0018, n300 = 0x0014, p150 = 0x0041 (each
// cross-checked against tt-kmd's tt_card_type on the same device).
var boardTypeBySubsystem = map[uint16]string{
	// Wormhole
	0x0014: "n300",
	0x0018: "n150",
	0x000b: "galaxy",
	0x0035: "ubb",
	// Blackhole
	0x0036: "p100",
	0x0043: "p100",
	0x0040: "p150",
	0x0041: "p150",
	0x0042: "p150",
	0x0044: "p300",
	0x0045: "p300",
	0x0046: "p300",
	0x0047: "ubb_blackhole",
}

// identify determines the device's board identity.
//
// BoardType: subsystem UPI (primary — driver-independent and consistent with
// tt-feature-discovery's product label) → telemetry tt_card_type (fallback
// for boards missing from the table) → "unknown". Serial: telemetry, when
// the device happens to be on tt-kmd; a serial-less cached entry upgrades
// the next time telemetry is readable.
func (b *Binder) identify(dev pciDevice) Identity {
	cached, haveCached := b.identities[dev.BDF]
	if haveCached && cached.Serial != "" {
		return cached
	}

	base := filepath.Join(pciDevicesDir(), dev.BDF)

	boardType, haveType := boardTypeFromSubsystem(base)
	if !haveType {
		boardType, haveType = readTelemetryCardType(base)
	}

	// The serial is telemetry-only; readable while the device is on tt-kmd.
	serial, err := readTelemetryAttr(base, "tt_serial")
	if err != nil {
		serial = ""
	}

	if !haveType && serial == "" {
		if haveCached {
			return cached
		}
		// Nothing worked. Don't cache: a later pass may catch the device on
		// tt-kmd after a drift, or the subsystem ID may join the table.
		return Identity{BoardType: "unknown"}
	}
	if !haveType {
		boardType = "unknown"
	}

	id := Identity{BoardType: boardType, Serial: serial}
	if haveCached && serial == "" {
		// Never downgrade a cached entry that already has a serial source.
		id.Serial = cached.Serial
	}
	if !haveCached || id != cached {
		log.Printf("identity: %s is a %s (serial %q)", dev.BDF, id.BoardType, id.Serial)
	}
	b.identities[dev.BDF] = id
	return id
}

// readTelemetryCardType is the board-type fallback for devices whose
// subsystem UPI is not in the table: tt-kmd's own decode, readable only
// while the device is on tt-kmd.
func readTelemetryCardType(pciDevDir string) (string, bool) {
	cardType, err := readTelemetryAttr(pciDevDir, "tt_card_type")
	if err != nil || cardType == "" || cardType == "unknown" {
		return "", false
	}
	return cardType, true
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
