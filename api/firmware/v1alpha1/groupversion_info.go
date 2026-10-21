// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

// Package v1alpha1 contains API Schema definitions for the firmware v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=firmware.tenstorrent.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "firmware.tenstorrent.com", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&TenstorrentFirmwarePolicy{}, &TenstorrentFirmwarePolicyList{})
}
