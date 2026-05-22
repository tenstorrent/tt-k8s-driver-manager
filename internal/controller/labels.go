package controller

const (
	// LabelTenstorrentPresent is NFD's native PCI-presence label for Tenstorrent
	// devices. Format: pci-<class>_<vendor>.present; class 1200 (Processing
	// Accelerator), vendor 1e52 (Tenstorrent). NFD emits this automatically.
	LabelTenstorrentPresent = "feature.node.kubernetes.io/pci-1200_1e52.present"

	// LabelKMDVersion is the per-node label set by the driver controller
	// when an installer pod transitions to Ready (i.e. /sys/module reports
	// the expected version).
	LabelKMDVersion = "driver.tenstorrent.com/kmd-version"

	// LabelDriverSkip opts a node out of driver reconciliation. Mirrors
	// LabelSkip on the firmware side — set to "true" on a node and the
	// driver controller will refuse to schedule its installer DaemonSet
	// there even if the node otherwise matches spec.nodeSelector.
	LabelDriverSkip = "driver.tenstorrent.com/skip"

	// LabelInstallMode reports how tt-kmd got onto this node.
	//   "container" — operator built + insmod'd via the builder pod
	//   "host"      — pre-existing host install (DKMS, apt, tt-ansible).
	//                 Operator stands down; doesn't touch the module.
	// The builder pod sets this label itself at startup based on
	// /var/lib/dkms/tenstorrent and /usr/src/tenstorrent-* probes.
	// Mirrors RBLN's rebellions.ai/npu.deploy.driver={true,pre-installed}.
	LabelInstallMode = "driver.tenstorrent.com/install-mode"

	// LabelSMIVersion is stamped after the builder pod copies its
	// bundled tt-smi venv to the host /opt/tt + writes a shim at
	// /usr/local/bin/tt-smi. Empty on host-managed nodes — we don't
	// claim ownership of tt-smi when the host already has its own.
	LabelSMIVersion = "tt-smi.driver.tenstorrent.com/version"
)
