package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenstorrentFirmwarePolicySpec declares the desired firmware version for a set of nodes.
type TenstorrentFirmwarePolicySpec struct {
	// Version is the firmware bundle version, e.g. "19.8.0".
	// Used to derive the bundle filename (fw_pack-<version>.fwbundle) and
	// the default readback expectation (<version>.0).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+\.[0-9]+$`
	Version string `json:"version"`

	// ReadbackVersion is the version string tt-smi reports after a successful
	// flash. Defaults to "<version>.0" if empty.
	// +optional
	ReadbackVersion string `json:"readbackVersion,omitempty"`

	// BundleURL overrides the firmware bundle download location.
	// Defaults to https://github.com/tenstorrent/tt-system-firmware/releases/download/v<version>/fw_pack-<version>.fwbundle.
	// +optional
	BundleURL string `json:"bundleURL,omitempty"`

	// NodeSelector selects the nodes this CR applies to. The operator ANDs in
	// the Tenstorrent NFD label so the head node can't be hit by a wide selector.
	// +kubebuilder:validation:Required
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// Paused stops the controller from advancing state transitions on nodes
	// matched by this CR. In-flight flashes are not interrupted.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// Force passes --force to tt-flash, bypassing the device-side version check.
	// +optional
	Force bool `json:"force,omitempty"`

	// UpgradePolicy controls how aggressively the operator drives upgrades.
	// +optional
	UpgradePolicy UpgradePolicy `json:"upgradePolicy,omitempty"`

	// Flasher overrides the flasher image. Useful for dev iteration.
	// +optional
	Flasher *FlasherOverride `json:"flasher,omitempty"`
}

type UpgradePolicy struct {
	// AutoUpgrade enables actual state transitions. If false, the controller
	// only reports drift in status.
	// +kubebuilder:default=true
	// +optional
	AutoUpgrade *bool `json:"autoUpgrade,omitempty"`

	// MaxParallel caps the number of nodes flashing simultaneously across
	// this CR. Default 1.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxParallel int32 `json:"maxParallel,omitempty"`

	// FlashTimeoutSeconds is the per-node Job activeDeadlineSeconds.
	// Default 900s (matches tt-ansible Galaxy headroom; PCIe-only is typically <120s).
	// +kubebuilder:default=900
	// +kubebuilder:validation:Minimum=60
	// +optional
	FlashTimeoutSeconds int32 `json:"flashTimeoutSeconds,omitempty"`

	// Drain controls cordon+drain behavior before flashing.
	// +optional
	Drain DrainPolicy `json:"drain,omitempty"`

	// HaltOnFailure halts the rollout as soon as any matched node hits state=Failed,
	// rather than continuing to flash other nodes. Default true.
	// +kubebuilder:default=true
	// +optional
	HaltOnFailure *bool `json:"haltOnFailure,omitempty"`
}

type DrainPolicy struct {
	// Enable cordon+drain before flash. Disable on single-node dev clusters
	// where the controller is on the node being flashed.
	// +kubebuilder:default=true
	// +optional
	Enable *bool `json:"enable,omitempty"`

	// Force eviction of pods not managed by a controller.
	// +optional
	Force bool `json:"force,omitempty"`

	// DeleteEmptyDir allows eviction of pods with emptyDir volumes.
	// +kubebuilder:default=true
	// +optional
	DeleteEmptyDir *bool `json:"deleteEmptyDir,omitempty"`

	// TimeoutSeconds is the drain deadline. Default 600s.
	// +kubebuilder:default=600
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

type FlasherOverride struct {
	// Image overrides the flasher image.
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullPolicy overrides the flasher imagePullPolicy.
	// Set to "Always" while iterating on the entrypoint.
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

// TenstorrentFirmwarePolicyStatus is the observed state.
type TenstorrentFirmwarePolicyStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
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

	// Summary aggregates per-node state for printer columns.
	// +optional
	Summary StatusSummary `json:"summary,omitempty"`

	// Nodes lists per-node state.
	// +listType=map
	// +listMapKey=name
	// +optional
	Nodes []NodeStatus `json:"nodes,omitempty"`
}

type StatusSummary struct {
	Matched    int32 `json:"matched"`
	UpToDate   int32 `json:"upToDate"`
	InProgress int32 `json:"inProgress"`
	Failed     int32 `json:"failed"`
	Pending    int32 `json:"pending"`
}

type NodeStatus struct {
	// Name is the Node name.
	Name string `json:"name"`

	// State is the per-node upgrade state.
	State NodeState `json:"state"`

	// CurrentVersion is the readback fw_bundle_version from the most recent flash.
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`

	// LastFlashJob is the most recent Job name for log lookup.
	// +optional
	LastFlashJob string `json:"lastFlashJob,omitempty"`

	// Message is human-readable context.
	// +optional
	Message string `json:"message,omitempty"`

	// LastTransitionTime is when the state last changed.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// NodeState is the per-node upgrade state. Natural flow:
//
//	Pending → Cordoning → Draining → Flashing → Uncordoning → Done
//	                                                       ↘ Failed
//
// The Cordoning / Draining / Uncordoning states are only visited when
// spec.upgradePolicy.drain.enable is true; otherwise the controller goes
// straight from Pending → Flashing → Done.
//
// +kubebuilder:validation:Enum=Pending;Cordoning;Draining;Flashing;Uncordoning;Done;Failed
type NodeState string

const (
	NodeStatePending     NodeState = "Pending"
	NodeStateCordoning   NodeState = "Cordoning"
	NodeStateDraining    NodeState = "Draining"
	NodeStateFlashing    NodeState = "Flashing"
	NodeStateUncordoning NodeState = "Uncordoning"
	NodeStateDone        NodeState = "Done"
	NodeStateFailed      NodeState = "Failed"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ttfwp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=`.status.summary.matched`
// +kubebuilder:printcolumn:name="UpToDate",type=integer,JSONPath=`.status.summary.upToDate`
// +kubebuilder:printcolumn:name="InProgress",type=integer,JSONPath=`.status.summary.inProgress`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.summary.failed`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TenstorrentFirmwarePolicy pins a firmware bundle version to a set of nodes.
type TenstorrentFirmwarePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenstorrentFirmwarePolicySpec   `json:"spec,omitempty"`
	Status TenstorrentFirmwarePolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type TenstorrentFirmwarePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenstorrentFirmwarePolicy `json:"items"`
}
