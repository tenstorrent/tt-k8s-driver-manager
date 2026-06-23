package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenstorrentDriverPolicySpec declares the desired tt-kmd version for a set of nodes.
type TenstorrentDriverPolicySpec struct {
	// Version is the tt-kmd release, e.g. "2.8.0". Maps to the upstream tag
	// ttkmd-<version>.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+\.[0-9]+$`
	Version string `json:"version"`

	// NodeSelector picks the nodes this policy applies to. The controller
	// ANDs in the Tenstorrent NFD label so a wide selector can't accidentally
	// hit the head node.
	// +kubebuilder:validation:Required
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// Paused stops the controller from advancing state. The DaemonSet stays
	// running with its current spec; in-flight installs are not interrupted.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// UpgradePolicy controls how the controller drives kmd version
	// transitions: what to do when a node has device-using workloads
	// blocking rmmod, drain config, timeouts. Modeled on
	// TenstorrentFirmwarePolicy.UpgradePolicy (same shape, ttdp-relevant
	// subset).
	// +optional
	UpgradePolicy UpgradePolicy `json:"upgradePolicy,omitempty"`

	// Installer overrides the installer image / pull policy. Useful for dev
	// iteration on the install.sh entrypoint.
	// +optional
	Installer *InstallerOverride `json:"installer,omitempty"`
}

// UpgradePolicy controls the controller's per-node kmd-upgrade behavior.
type UpgradePolicy struct {
	// Drain controls cordon + device-pod eviction before the builder pod
	// attempts rmmod. When disabled the controller falls back to the
	// pre-drain behavior — builder races against refcount, errors if >0.
	// +optional
	Drain DrainPolicy `json:"drain,omitempty"`

	// ForceUnload SIGKILLs every process holding /dev/tenstorrent (via
	// /proc/*/fd walk) before rmmod when the loaded module's refcount is
	// non-zero. Default (false) is the safe path: the builder pod errors
	// and waits for the next reconcile, letting the operator drain
	// workloads manually. Set true on clusters where you'd rather lose
	// in-flight workloads than block a driver upgrade.
	// +optional
	ForceUnload bool `json:"forceUnload,omitempty"`
}

// DrainPolicy mirrors firmware/v1alpha1.DrainPolicy but is duplicated to
// keep the driver and firmware APIs independent — same shape, evolution
// can diverge.
//
// The drain runs in two passes, mirroring NVIDIA gpu-operator's
// ENABLE_GPU_POD_EVICTION + ENABLE_AUTO_DRAIN split:
//
//  1. Targeted eviction — pods that explicitly declare /dev/tenstorrent
//     use (hostPath). Gated by Enable.
//  2. Full-node drain — every non-DS pod on the cordoned node, kubectl
//     drain semantics. Catches privileged containers that get /dev via
//     containerd's auto-mount (no explicit hostPath / resource request).
//     Gated by FullNode.
type DrainPolicy struct {
	// Enable cordon + targeted device-pod eviction (pass 1) before the
	// builder pod runs. Disable on single-node dev clusters where the
	// controller is on the node being upgraded.
	// +kubebuilder:default=true
	// +optional
	Enable *bool `json:"enable,omitempty"`

	// FullNode triggers pass 2: evict every non-DS pod on the cordoned
	// node (kubectl drain semantics). Required to drain privileged
	// containers that don't declare /dev/tenstorrent use — the targeted
	// pass 1 filter only catches explicit hostPath mounts or
	// tenstorrent.com/* resource requests. Defaults true to mirror
	// NVIDIA's ENABLE_AUTO_DRAIN; set false on multi-tenant nodes where
	// collateral eviction of unrelated workloads is unacceptable.
	// +kubebuilder:default=true
	// +optional
	FullNode *bool `json:"fullNode,omitempty"`

	// PodSelectorLabel restricts the pass-2 full-node drain to pods
	// matching this label selector (k8s syntax: "key=value", "key",
	// "key notin (a,b)"). Empty = no restriction. Mirrors NVIDIA's
	// DRAIN_POD_SELECTOR_LABEL — lets multi-tenant clusters opt their
	// drainable workloads in by label instead of getting a blanket
	// sweep.
	// +optional
	PodSelectorLabel string `json:"podSelectorLabel,omitempty"`

	// Force eviction of pods not managed by a controller (bare pods).
	// Applies to both passes.
	// +optional
	Force bool `json:"force,omitempty"`

	// DeleteEmptyDir allows pass-2 eviction of pods with emptyDir
	// volumes. Mirrors kubectl drain's --delete-emptydir-data.
	// +kubebuilder:default=true
	// +optional
	DeleteEmptyDir *bool `json:"deleteEmptyDir,omitempty"`

	// TimeoutSeconds is the drain deadline. Default 600s.
	// +kubebuilder:default=600
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

type InstallerOverride struct {
	// +optional
	Image string `json:"image,omitempty"`
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

// TenstorrentDriverPolicyStatus surfaces DaemonSet state.
type TenstorrentDriverPolicyStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// DesiredVersion mirrors spec.version for at-a-glance status reads.
	// +optional
	DesiredVersion string `json:"desiredVersion,omitempty"`

	// Conditions report top-level readiness.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Summary aggregates from the managed DaemonSet.
	// +optional
	Summary DriverSummary `json:"summary,omitempty"`

	// DaemonSet is the name of the DaemonSet this CR manages.
	// +optional
	DaemonSet string `json:"daemonSet,omitempty"`

	// Nodes lists per-node upgrade state, mirroring
	// TenstorrentFirmwarePolicy.Status.Nodes — gives operators a
	// per-node view without having to inspect the underlying DaemonSet
	// pods. Updated on every reconcile.
	// +listType=map
	// +listMapKey=name
	// +optional
	Nodes []DriverNodeStatus `json:"nodes,omitempty"`
}

type DriverSummary struct {
	// Matched is the number of nodes the selector currently picks up.
	Matched int32 `json:"matched"`
	// Desired / Ready / Available come from the underlying DaemonSet.
	Desired   int32 `json:"desired"`
	Ready     int32 `json:"ready"`
	Available int32 `json:"available"`
	// Failed counts pods in CrashLoopBackoff or Error state.
	Failed int32 `json:"failed"`
	// UpToDate counts nodes whose loaded kmd version matches spec.version
	// AND whose installer pod reports Ready.
	// +optional
	UpToDate int32 `json:"upToDate,omitempty"`
	// InProgress counts nodes mid-transition (Cordoning / Draining /
	// Upgrading / Uncordoning).
	// +optional
	InProgress int32 `json:"inProgress,omitempty"`
}

// DriverNodeStatus captures the per-node upgrade state for a ttdp CR.
// Modeled on firmware/v1alpha1.NodeStatus.
type DriverNodeStatus struct {
	// Name is the Node name.
	Name string `json:"name"`

	// State is the per-node upgrade state.
	State DriverNodeState `json:"state"`

	// Reason is a stable, programmatic CamelCase code explaining why the
	// node is in its current State — populated for non-trivial outcomes
	// (HostManagedKMD, BuilderImagePullFailed, BuilderCrashLoop, ...) so
	// `kubectl describe ttdp` surfaces the cause without digging into
	// operator logs. Empty when the state is self-explanatory (e.g. a
	// fresh Pending node with nothing wrong).
	// +optional
	Reason string `json:"reason,omitempty"`

	// CurrentVersion is the tt-kmd version currently loaded on this node,
	// reported by the installer pod's TT_KMD_VERSION env (ground-truth
	// for what was last successfully installed).
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`

	// Message is human-readable context, surfaced when the state isn't
	// self-explanatory (e.g. "PDB blocking eviction of pod X").
	// +optional
	Message string `json:"message,omitempty"`

	// LastTransitionTime is when the state last changed.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// DriverNodeState mirrors firmware/v1alpha1.NodeState but uses Upgrading
// (kmd build + insmod via the builder pod) where firmware uses Flashing.
//
//	Pending → Cordoning → Draining → Upgrading → Uncordoning → Done
//	                                                       ↘ Failed
//	                                                       ↘ HostManaged
//
// Cordoning / Draining / Uncordoning are only visited when
// spec.upgradePolicy.drain.enable is true; otherwise the controller goes
// straight from Pending → Upgrading → Done.
//
// HostManaged is a terminal non-failure: the node's host already owns
// the kmd install (DKMS / apt / tt-ansible) and the operator has stood
// down. Distinct from Done so operators can tell the difference between
// "operator installed it" and "host installed it; operator is idle"
// without inspecting the install-mode label.
//
// +kubebuilder:validation:Enum=Pending;Cordoning;Draining;Upgrading;Uncordoning;Done;Failed;HostManaged
type DriverNodeState string

const (
	DriverNodeStatePending     DriverNodeState = "Pending"
	DriverNodeStateCordoning   DriverNodeState = "Cordoning"
	DriverNodeStateDraining    DriverNodeState = "Draining"
	DriverNodeStateUpgrading   DriverNodeState = "Upgrading"
	DriverNodeStateUncordoning DriverNodeState = "Uncordoning"
	DriverNodeStateDone        DriverNodeState = "Done"
	DriverNodeStateFailed      DriverNodeState = "Failed"
	DriverNodeStateHostManaged DriverNodeState = "HostManaged"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ttdp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=`.status.summary.matched`
// +kubebuilder:printcolumn:name="UpToDate",type=integer,JSONPath=`.status.summary.upToDate`
// +kubebuilder:printcolumn:name="InProgress",type=integer,JSONPath=`.status.summary.inProgress`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.summary.failed`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TenstorrentDriverPolicy pins a tt-kmd version onto a set of nodes via a
// privileged DaemonSet that runs DKMS + modprobe in the host namespace.
type TenstorrentDriverPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenstorrentDriverPolicySpec   `json:"spec,omitempty"`
	Status TenstorrentDriverPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type TenstorrentDriverPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenstorrentDriverPolicy `json:"items"`
}
