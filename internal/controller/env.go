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
// a CR's nodeSelector.
func requireTenstorrentLabel() bool {
	return os.Getenv("REQUIRE_TT_PCI_LABEL") != "false"
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
