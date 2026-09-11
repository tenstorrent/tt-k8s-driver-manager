package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_Valid(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
devices:
  - resourceName: tenstorrent.com/wormhole
    vendorId: "1e52"
    deviceId: ["401e", "b140"]
    locality: numa
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Devices) != 1 {
		t.Fatalf("parsed %d device groups; want 1", len(cfg.Devices))
	}
	g := cfg.Devices[0]
	if g.ResourceName != "tenstorrent.com/wormhole" || g.VendorID != "1e52" ||
		len(g.DeviceIDs) != 2 || g.Locality != "numa" {
		t.Errorf("parsed %+v; want the YAML values", g)
	}
}

func TestLoad_LocalityDefaultsToIndividual(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
devices:
  - resourceName: tenstorrent.com/wormhole
    vendorId: "1e52"
    deviceId: ["401e"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Devices[0].Locality; got != "individual" {
		t.Errorf("locality = %q; want the default \"individual\"", got)
	}
}

func TestLoad_RejectsInvalidLocality(t *testing.T) {
	_, err := Load(writeConfig(t, `
devices:
  - resourceName: tenstorrent.com/wormhole
    vendorId: "1e52"
    deviceId: ["401e"]
    locality: per-socket
`))
	if err == nil || !strings.Contains(err.Error(), "locality") {
		t.Errorf("Load() = %v; want an invalid-locality error", err)
	}
}

func TestLoad_RejectsInvalidResourceName(t *testing.T) {
	// No vendor prefix — not a valid extended resource name.
	_, err := Load(writeConfig(t, `
devices:
  - resourceName: wormhole
    vendorId: "1e52"
    deviceId: ["401e"]
`))
	if err == nil || !strings.Contains(err.Error(), "resourceName") {
		t.Errorf("Load() = %v; want an invalid-resourceName error", err)
	}
}

func TestLoad_RejectsMalformedYAML(t *testing.T) {
	if _, err := Load(writeConfig(t, "devices: [broken")); err == nil {
		t.Error("Load() accepted malformed YAML")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("Load() succeeded on a missing file")
	}
}
