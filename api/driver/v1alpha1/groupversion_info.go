// Package v1alpha1 contains the driver.tenstorrent.com/v1alpha1 API.
// +kubebuilder:object:generate=true
// +groupName=driver.tenstorrent.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "driver.tenstorrent.com", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&TenstorrentDriverPolicy{}, &TenstorrentDriverPolicyList{})
}
