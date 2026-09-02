// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EffectiveNodeAffinity backs the controller's selector resolution; the
// behavioral contract (new field wins, deprecated alias works) is the
// migration story for in-cluster CRs written before the rename, so it's
// worth pinning explicitly.
func TestEffectiveNodeAffinity_Driver(t *testing.T) {
	cases := []struct {
		name        string
		affinity    *metav1.LabelSelector
		selector    *metav1.LabelSelector
		wantLabels  map[string]string
	}{
		{
			name:       "nodeAffinity only",
			affinity:   &metav1.LabelSelector{MatchLabels: map[string]string{"a": "1"}},
			wantLabels: map[string]string{"a": "1"},
		},
		{
			name:       "deprecated nodeSelector only",
			selector:   &metav1.LabelSelector{MatchLabels: map[string]string{"b": "2"}},
			wantLabels: map[string]string{"b": "2"},
		},
		{
			name:       "both set: nodeAffinity wins",
			affinity:   &metav1.LabelSelector{MatchLabels: map[string]string{"new": "1"}},
			selector:   &metav1.LabelSelector{MatchLabels: map[string]string{"old": "2"}},
			wantLabels: map[string]string{"new": "1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &TenstorrentDriverPolicySpec{NodeAffinity: tc.affinity, NodeSelector: tc.selector}
			got := s.EffectiveNodeAffinity()
			if len(got.MatchLabels) != len(tc.wantLabels) {
				t.Fatalf("len(matchLabels) = %d; want %d", len(got.MatchLabels), len(tc.wantLabels))
			}
			for k, v := range tc.wantLabels {
				if got.MatchLabels[k] != v {
					t.Errorf("matchLabels[%q] = %q; want %q", k, got.MatchLabels[k], v)
				}
			}
		})
	}
}
