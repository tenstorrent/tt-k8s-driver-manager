package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenstorrentDriverPolicySpec declares the desired tt-kmd version for a set of nodes.
//
// +kubebuilder:validation:XValidation:rule="has(self.nodeAffinity) != has(self.nodeSelector)",message="exactly one of spec.nodeAffinity or spec.nodeSelector must be set"
type TenstorrentDriverPolicySpec struct {
	// Version is the tt-kmd release, e.g. "2.8.0". Maps to the upstream tag
	// ttkmd-<version>.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+\.[0-9]+$`
	Version string `json:"version"`

	// NodeAffinity picks the nodes this policy applies to. Shape matches
	// metav1.LabelSelector (matchLabels and/or matchExpressions); the
	// controller ANDs in the Tenstorrent NFD label so a wide selector
	// can't accidentally hit the head node.
	//
	// Exactly one of nodeAffinity or the deprecated nodeSelector must be set.
	// +optional
	NodeAffinity *metav1.LabelSelector `json:"nodeAffinity,omitempty"`

	// NodeSelector is the v1alpha1 name for NodeAffinity, kept for
	// backwards compatibility with CRs written before the rename. Same
	// shape, same semantics. Prefer NodeAffinity for new CRs.
	//
	// Deprecated: use NodeAffinity instead. Will be removed in a future
	// API version.
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`

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

	// Unmanage, when true, asks the controller to vacate every in-scope
	// node so an external manager (typically DKMS via tt-ansible) can take
	// over the host's tt-kmd. Per node, the controller drains
	// /dev/tenstorrent holders, spawns a one-shot unload Job that rmmods
	// tt-kmd and deletes the operator-built .ko, then drops the
	// driver.tenstorrent.com/install-mode=container label. Once every
	// matched node is Unmanaged the DaemonSet is torn down and the
	// controller stays out of the way regardless of host DKMS state.
	//
	// Strict failure mode: if any node's unload Job fails (e.g. refcnt > 0
	// after drain), the controller halts and surfaces UnloadFailed
	// per-node. Operator must fix the stuck node before the rest proceed.
	//
	// Reversible: flipping back to false resumes normal reconcile —
	// host-managed signals take precedence (the builder pod stands down)
	// or, absent those, the operator re-installs from container.
	// +optional
	Unmanage bool `json:"unmanage,omitempty"`
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

	// Phase is a coarse top-level state — easy to grep with kubectl. Today
	// only Unmanaged is set explicitly (when every in-scope node has been
	// vacated for external KMD management); the empty/default value means
	// the controller is reconciling normally, look at .status.conditions
	// for finer detail.
	// +optional
	Phase DriverPolicyPhase `json:"phase,omitempty"`

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

	// CurrentVersion is the tt-kmd version currently loaded on this node,
	// reported by the installer pod's TT_KMD_VERSION env (ground-truth
	// for what was last successfully installed).
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`

	// Message is human-readable context, surfaced when the state isn't
	// self-explanatory (e.g. "PDB blocking eviction of pod X").
	// +optional
	Message string `json:"message,omitempty"`

	// Reason is a machine-readable tag for the current State — short,
	// stable, suitable for alert-rule matching (e.g. "Unmanaged",
	// "UnloadFailed"). Empty when no specific reason applies. Shape
	// matches the sibling status-reason PR; small overlap risk is
	// resolved at merge time.
	// +optional
	Reason string `json:"reason,omitempty"`

	// LastTransitionTime is when the state last changed.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// DriverNodeState mirrors firmware/v1alpha1.NodeState but uses Upgrading
// (kmd build + insmod via the builder pod) where firmware uses Flashing.
//
//	Pending → Cordoning → Draining → Upgrading → Uncordoning → Done
//	                                                       ↘ Failed
//
// Cordoning / Draining / Uncordoning are only visited when
// spec.upgradePolicy.drain.enable is true; otherwise the controller goes
// straight from Pending → Upgrading → Done.
//
// When spec.unmanage=true a separate flow runs:
//
//	Pending → Cordoning → Draining → Unloading → Unmanaged
//	                                          ↘ UnloadFailed
//
// Unmanaged is terminal-stable (the controller stays out of the way of
// the node); UnloadFailed is terminal-bad (operator must intervene).
//
// +kubebuilder:validation:Enum=Pending;Cordoning;Draining;Upgrading;Uncordoning;Done;Failed;Unloading;Unmanaged;UnloadFailed
type DriverNodeState string

const (
	DriverNodeStatePending      DriverNodeState = "Pending"
	DriverNodeStateCordoning    DriverNodeState = "Cordoning"
	DriverNodeStateDraining     DriverNodeState = "Draining"
	DriverNodeStateUpgrading    DriverNodeState = "Upgrading"
	DriverNodeStateUncordoning  DriverNodeState = "Uncordoning"
	DriverNodeStateDone         DriverNodeState = "Done"
	DriverNodeStateFailed       DriverNodeState = "Failed"
	DriverNodeStateUnloading    DriverNodeState = "Unloading"
	DriverNodeStateUnmanaged    DriverNodeState = "Unmanaged"
	DriverNodeStateUnloadFailed DriverNodeState = "UnloadFailed"
)

// DriverPolicyPhase is the coarse top-level state. Today only "Unmanaged"
// is set explicitly; empty is the implicit default ("controller is
// reconciling normally"). No enum validation — future phases may be
// added without bumping the API version.
type DriverPolicyPhase string

const (
	// DriverPolicyPhaseUnmanaged is set when every in-scope node has been
	// vacated via spec.unmanage=true. The controller stays out of the way
	// of those nodes regardless of DKMS-signal state.
	DriverPolicyPhaseUnmanaged DriverPolicyPhase = "Unmanaged"
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

// EffectiveNodeAffinity returns the resolved label selector for matching
// nodes. nodeAffinity wins if set; otherwise falls back to the deprecated
// nodeSelector alias. Returns an empty selector (matches all) if both are
// nil — CRD CEL validation already rejects that case.
func (s *TenstorrentDriverPolicySpec) EffectiveNodeAffinity() metav1.LabelSelector {
	if s.NodeAffinity != nil {
		return *s.NodeAffinity
	}
	if s.NodeSelector != nil {
		return *s.NodeSelector
	}
	return metav1.LabelSelector{}
}
