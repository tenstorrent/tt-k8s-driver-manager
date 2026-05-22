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

	// Installer overrides the installer image / pull policy. Useful for dev
	// iteration on the install.sh entrypoint.
	// +optional
	Installer *InstallerOverride `json:"installer,omitempty"`
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
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ttdp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=`.status.summary.matched`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.summary.ready`
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
