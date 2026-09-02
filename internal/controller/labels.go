// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

package controller

const (
	// LabelTenstorrentPresent is NFD's native PCI-presence label for Tenstorrent
	// devices. Format: pci-<class>_<vendor>.present; class 1200 (Processing
	// Accelerator), vendor 1e52 (Tenstorrent). NFD emits this automatically.
	LabelTenstorrentPresent = "feature.node.kubernetes.io/pci-1200_1e52.present"

	// LabelKMDVersion is the per-node label set by the builder pod's
	// entrypoint from the actual loaded module version
	// (/sys/module/tenstorrent/version). Cleared by the controller when
	// no Ready installer pod is present on the node.
	LabelKMDVersion = "driver.tenstorrent.com/kmd-version"

	// LabelDriverSkip opts a node out of driver reconciliation. Mirrors
	// LabelSkip on the firmware side — set to "true" on a node and the
	// driver controller will refuse to schedule its installer DaemonSet
	// there even if the node otherwise matches spec.nodeAffinity.
	LabelDriverSkip = "driver.tenstorrent.com/skip"

	// Driver-side cordon annotations. Distinct prefix from the firmware
	// controller's `firmware.tenstorrent.com/cordoned-*` so the two
	// controllers don't accidentally uncordon each other's cordons.
	AnnoDriverCordonedBy = "driver.tenstorrent.com/cordoned-by"
	AnnoDriverCordonedAt = "driver.tenstorrent.com/cordoned-at"

	// LabelInstallMode reports how tt-kmd got onto this node.
	//   "container" — operator built + insmod'd via the builder pod
	//   "host"      — pre-existing host install (DKMS, apt, configuration management).
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

	// Firmware-side labels / annotations (managed by the firmware controller).

	LabelUpgradeState = "firmware.tenstorrent.com/upgrade-state"
	LabelSkip         = "firmware.tenstorrent.com/skip"
	LabelOwnerCR      = "firmware.tenstorrent.com/owned-by"
	LabelFWVersion    = "firmware.tenstorrent.com/fw-version"

	AnnoCurrentVersion = "firmware.tenstorrent.com/current-version"
	AnnoDesiredVersion = "firmware.tenstorrent.com/desired-version"
	AnnoLastFlashJob   = "firmware.tenstorrent.com/last-flash-job"
	AnnoCordonedBy     = "firmware.tenstorrent.com/cordoned-by"
	AnnoCordonedAt     = "firmware.tenstorrent.com/cordoned-at"

	JobLabelCR      = "firmware.tenstorrent.com/cr"
	JobLabelNode    = "firmware.tenstorrent.com/node"
	JobLabelVersion = "firmware.tenstorrent.com/version"
)

// MessageNodeConflict is set on the NodeStatus.Message when this CR's
// selector matches a node already owned by a different CR.
const MessageNodeConflict = "Conflict: node owned by a different TenstorrentFirmwarePolicy CR"

// MessageDrainTimeoutPrefix starts the NodeStatus.Message of a node whose
// drain window expired with device pods still running. Exported as a
// prefix (the rest of the message names the blocking pods) so the metrics
// path can tell a drain stall apart from a failed flash Job without
// re-deriving the timeout.
const MessageDrainTimeoutPrefix = "drain timeout after "

// MessageExternalCordon is set when the node is cordoned but the cordon
// wasn't applied by us — likely an external maintenance window. We
// refuse to flash to avoid running tt-flash concurrent with whatever
// the human cordoned the node for.
const MessageExternalCordon = "node is cordoned but not by this operator"

// Driver reason codes — stable, programmatic identifiers for why a
// node is in its current DriverNodeStatus.State. Surface in two places:
//
//  1. cr.status.nodes[].reason — picked up by `kubectl describe ttdp`.
//  2. The Reason field on emitted k8s Events so `kubectl get events
//     --field-selector reason=HostManagedKMD` works for fleet-wide
//     diagnosis.
//
// Only add codes the controller actually has a code path for —
// inventing reasons that never fire is worse than no reason. Keep
// CamelCase, no spaces, stable across releases (these strings flow into
// scripts / dashboards).
const (
	// ReasonHostManagedKMD: builder pod detected DKMS signals
	// (/var/lib/dkms/tenstorrent or /usr/src/tenstorrent-*) and labeled
	// the node install-mode=host. Operator has stood down on this node.
	ReasonHostManagedKMD = "HostManagedKMD"

	// ReasonBuilderImagePullFailed: kubelet can't pull the builder
	// image (private registry, missing imagePullSecrets, network).
	ReasonBuilderImagePullFailed = "BuilderImagePullFailed"

	// ReasonBuilderCrashLoop: builder pod has restarted ≥3 times and
	// is still NotReady. Covers the catch-all failure shapes the
	// entrypoint can hit and exit non-zero on: rmmod refused (refcnt
	// not drained), make / dkms build failed, insmod failed, missing
	// host kernel-build tree. The pod's logs distinguish them; the
	// status surface keeps to one code so the API doesn't lie about
	// which the controller actually detected (it doesn't — it just
	// sees "the pod keeps dying").
	ReasonBuilderCrashLoop = "BuilderCrashLoop"

	// ReasonInstalling: pod's templated TT_KMD_VERSION matches the
	// CR's spec.version but the readiness probe isn't passing yet —
	// either still building or insmod hasn't completed.
	ReasonInstalling = "Installing"

	// ReasonReady: pod is Ready against the target version. Emitted
	// once per state-transition into Done.
	ReasonReady = "Ready"

	// ReasonDraining: this CR cordoned the node and is waiting on
	// pass-1/pass-2 eviction to drain /dev/tenstorrent holders before
	// the builder pod rolls.
	ReasonDraining = "Draining"

	// ReasonCordoning: node has been cordoned by this CR; pre-drain.
	ReasonCordoning = "Cordoning"

	// ReasonUncordoning: pod is Ready at target; the cordon we placed
	// has not yet been lifted.
	ReasonUncordoning = "Uncordoning"

	// ReasonPaused: spec.paused=true, controller is idle.
	ReasonPaused = "Paused"
)
