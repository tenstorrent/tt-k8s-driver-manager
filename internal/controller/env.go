// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

package controller

import "os"

// operatorNamespace returns the namespace the manager was deployed in.
// Falls back to "tt-k8s-driver-manager-system" if the downward-API env var
// isn't set (e.g. during envtest).
func operatorNamespace() string {
	if ns := os.Getenv("OPERATOR_NAMESPACE"); ns != "" {
		return ns
	}
	return "tt-k8s-driver-manager-system"
}

// requireTenstorrentLabel returns false when REQUIRE_TT_PCI_LABEL=false is set.
// Default is true: nodes without the NFD label are ignored even if they match
// a CR's nodeAffinity.
func requireTenstorrentLabel() bool {
	return os.Getenv("REQUIRE_TT_PCI_LABEL") != "false"
}

// defaultFlasherImageFromEnv lets the chart override the flasher image
// without rebuilding the controller binary.
func defaultFlasherImageFromEnv() string {
	if v := os.Getenv("FLASHER_IMAGE"); v != "" {
		return v
	}
	return defaultFlasherImage
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envSet returns (value, isSet) — distinguishes "set to empty" from
// "unset". Useful when the empty string is a meaningful override (e.g.
// DRIVER_DEPLOY_GATES="" intentionally disables the feature, but unset
// means "fall back to the documented default").
func envSet(name string) (string, bool) {
	return os.LookupEnv(name)
}
