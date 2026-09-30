// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

package controller

import (
	"testing"

	firmwarev1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/firmware/v1alpha1"
)

func flashEnv(t *testing.T, flasher *firmwarev1alpha1.FlasherOverride) map[string]string {
	t.Helper()
	cr := &firmwarev1alpha1.TenstorrentFirmwarePolicy{}
	cr.Name = "fw"
	cr.Spec.Version = "19.11.0"
	cr.Spec.Flasher = flasher

	job := buildFlashJob(cr, "node-a", "flasher:test")
	env := map[string]string{}
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name != "flash" {
			continue
		}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
	}
	return env
}

func TestBuildFlashJobHomogenizeFirmwareVersionsEnv(t *testing.T) {
	cases := []struct {
		name          string
		flasher       *firmwarev1alpha1.FlasherOverride
		wantHomog     string
		wantForce     string
		wantFlashArgs string
	}{
		{
			name:      "no flasher override",
			flasher:   nil,
			wantHomog: "false", wantForce: "false", wantFlashArgs: "",
		},
		{
			// The script decides whether --force is needed from the
			// pre-flash readback, so the controller must not add it.
			name:      "homogenize only",
			flasher:   &firmwarev1alpha1.FlasherOverride{HomogenizeFirmwareVersions: true},
			wantHomog: "true", wantForce: "false", wantFlashArgs: "",
		},
		{
			name: "homogenize with forceWrite",
			flasher: &firmwarev1alpha1.FlasherOverride{
				HomogenizeFirmwareVersions: true,
				ForceWrite:                 true,
			},
			wantHomog: "true", wantForce: "true", wantFlashArgs: "--force",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := flashEnv(t, tc.flasher)
			if got := env["TT_HOMOGENIZE_FIRMWARE_VERSIONS"]; got != tc.wantHomog {
				t.Errorf("TT_HOMOGENIZE_FIRMWARE_VERSIONS = %q, want %q", got, tc.wantHomog)
			}
			if got := env["TT_FORCE_WRITE"]; got != tc.wantForce {
				t.Errorf("TT_FORCE_WRITE = %q, want %q", got, tc.wantForce)
			}
			if got := env["TT_FLASH_ARGS"]; got != tc.wantFlashArgs {
				t.Errorf("TT_FLASH_ARGS = %q, want %q", got, tc.wantFlashArgs)
			}
		})
	}
}
