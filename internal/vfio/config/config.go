// Package config loads the PCI device list that drives vfio-pci binding.
package config

import (
	"fmt"
	"log"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// DeviceGroup maps one Kubernetes resource name onto a set of PCI device IDs.
type DeviceGroup struct {
	// ResourceName is the Kubernetes extended resource, e.g. "tenstorrent.com/wormhole".
	ResourceName string `yaml:"resourceName"`
	// VendorID is the 4-hex-digit PCI vendor ID without the "0x" prefix.
	VendorID string `yaml:"vendorId"`
	// DeviceIDs is the list of 4-hex-digit PCI device IDs without the "0x" prefix.
	DeviceIDs []string `yaml:"deviceId"`
	// Locality controls how devices are grouped for allocation by the
	// consumer that advertises them. The binder itself ignores it, but the
	// field is parsed and validated here so a single config file can be
	// shared with that consumer without tripping strict validation.
	Locality string `yaml:"locality"`
}

// Config is the top-level configuration structure.
type Config struct {
	// Devices is the list of device groups to bind to vfio-pci.
	Devices []DeviceGroup `yaml:"devices"`
}

var resourceNameRE = regexp.MustCompile(`^[a-z0-9.\-]+/[a-z0-9.\-]+$`)

// Load reads and validates a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	for i, g := range cfg.Devices {
		if !resourceNameRE.MatchString(g.ResourceName) {
			return nil, fmt.Errorf("invalid resourceName %q: must match %s", g.ResourceName, resourceNameRE)
		}
		if cfg.Devices[i].Locality == "" {
			cfg.Devices[i].Locality = "individual"
		}
		switch cfg.Devices[i].Locality {
		case "individual", "numa", "all":
		default:
			return nil, fmt.Errorf("invalid locality %q for resource %s", cfg.Devices[i].Locality, g.ResourceName)
		}
		log.Printf("config: resource %s vendor=%s devices=%v locality=%s",
			g.ResourceName, g.VendorID, g.DeviceIDs, cfg.Devices[i].Locality)
	}

	return &cfg, nil
}
