package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driverv1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/driver/v1alpha1"
)

func newDriverCR() *driverv1alpha1.TenstorrentDriverPolicy {
	return &driverv1alpha1.TenstorrentDriverPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cr", Generation: 1, UID: "00000000-0000-0000-0000-000000000001"},
		Spec: driverv1alpha1.TenstorrentDriverPolicySpec{
			Version:      "2.8.0",
			NodeSelector: metav1.LabelSelector{},
		},
	}
}

func TestDriverDaemonSetName(t *testing.T) {
	if got := driverDaemonSetName("default"); got != "ttdrv-default" {
		t.Fatalf("driverDaemonSetName(default) = %q; want ttdrv-default", got)
	}
	if got := driverDaemonSetName("canary-pool"); got != "ttdrv-canary-pool" {
		t.Fatalf("driverDaemonSetName(canary-pool) = %q; want ttdrv-canary-pool", got)
	}
}

func TestPodEnv(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Env: []corev1.EnvVar{
			{Name: "TT_KMD_VERSION", Value: "2.8.0"},
			{Name: "NODE_NAME", Value: "e01cs01"},
		}}},
	}}
	if got := podEnv(pod, "TT_KMD_VERSION"); got != "2.8.0" {
		t.Errorf("podEnv(TT_KMD_VERSION) = %q; want 2.8.0", got)
	}
	if got := podEnv(pod, "MISSING"); got != "" {
		t.Errorf("podEnv(MISSING) = %q; want empty", got)
	}
	emptyPod := &corev1.Pod{}
	if got := podEnv(emptyPod, "TT_KMD_VERSION"); got != "" {
		t.Errorf("podEnv on empty pod = %q; want empty", got)
	}
}

func TestLabelSelectorToExpressions_MatchLabels(t *testing.T) {
	sel := metav1.LabelSelector{MatchLabels: map[string]string{"pool": "canary"}}
	exprs := labelSelectorToExpressions(sel)
	if len(exprs) != 1 {
		t.Fatalf("got %d exprs; want 1", len(exprs))
	}
	e := exprs[0]
	if e.Key != "pool" || e.Operator != corev1.NodeSelectorOpIn || len(e.Values) != 1 || e.Values[0] != "canary" {
		t.Errorf("unexpected expr: %+v", e)
	}
}

func TestLabelSelectorToExpressions_AllOperators(t *testing.T) {
	sel := metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "a", Operator: metav1.LabelSelectorOpIn, Values: []string{"x"}},
			{Key: "b", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"y"}},
			{Key: "c", Operator: metav1.LabelSelectorOpExists},
			{Key: "d", Operator: metav1.LabelSelectorOpDoesNotExist},
		},
	}
	exprs := labelSelectorToExpressions(sel)
	if len(exprs) != 4 {
		t.Fatalf("got %d exprs; want 4", len(exprs))
	}
	want := map[string]corev1.NodeSelectorOperator{
		"a": corev1.NodeSelectorOpIn,
		"b": corev1.NodeSelectorOpNotIn,
		"c": corev1.NodeSelectorOpExists,
		"d": corev1.NodeSelectorOpDoesNotExist,
	}
	for _, e := range exprs {
		if got, ok := want[e.Key]; !ok || got != e.Operator {
			t.Errorf("expr key %q operator = %q; want %q", e.Key, e.Operator, want[e.Key])
		}
	}
}

func TestComputeDSTemplateHash_Stable(t *testing.T) {
	r := &DriverPolicyReconciler{}
	cr := newDriverCR()
	a := r.buildDaemonSet(cr, driverDaemonSetName(cr.Name))
	b := r.buildDaemonSet(cr, driverDaemonSetName(cr.Name))
	if a.Annotations[dsTemplateHashAnnotation] == "" {
		t.Fatal("hash annotation not set")
	}
	if a.Annotations[dsTemplateHashAnnotation] != b.Annotations[dsTemplateHashAnnotation] {
		t.Errorf("hash drifted between identical builds:\n  a=%s\n  b=%s",
			a.Annotations[dsTemplateHashAnnotation], b.Annotations[dsTemplateHashAnnotation])
	}
}

func TestComputeDSTemplateHash_DetectsVersionDrift(t *testing.T) {
	r := &DriverPolicyReconciler{}
	a := r.buildDaemonSet(newDriverCR(), "ttdrv-test-cr")

	cr := newDriverCR()
	cr.Spec.Version = "2.7.0"
	b := r.buildDaemonSet(cr, "ttdrv-test-cr")

	if a.Annotations[dsTemplateHashAnnotation] == b.Annotations[dsTemplateHashAnnotation] {
		t.Error("hash unchanged across version flip; daemonSetNeedsUpdate would miss upgrades")
	}
}

func TestComputeDSTemplateHash_DetectsImageDrift(t *testing.T) {
	r := &DriverPolicyReconciler{}
	a := r.buildDaemonSet(newDriverCR(), "ttdrv-test-cr")

	cr := newDriverCR()
	cr.Spec.Installer = &driverv1alpha1.InstallerOverride{Image: "ghcr.io/example/builder:custom"}
	b := r.buildDaemonSet(cr, "ttdrv-test-cr")

	if a.Annotations[dsTemplateHashAnnotation] == b.Annotations[dsTemplateHashAnnotation] {
		t.Error("hash unchanged across image override; dev iteration on installer image would silently no-op")
	}
}

func TestDaemonSetNeedsUpdate(t *testing.T) {
	r := &DriverPolicyReconciler{}
	cr := newDriverCR()
	a := r.buildDaemonSet(cr, "ttdrv-test-cr")
	b := r.buildDaemonSet(cr, "ttdrv-test-cr")
	if daemonSetNeedsUpdate(a, b) {
		t.Error("identical DSes report needing update; would cause reconcile churn")
	}

	cr2 := newDriverCR()
	cr2.Spec.Version = "2.7.0"
	c := r.buildDaemonSet(cr2, "ttdrv-test-cr")
	if !daemonSetNeedsUpdate(a, c) {
		t.Error("version-drift DSes report no update; upgrades would be skipped")
	}
}

func TestBuildDaemonSet_Defaults(t *testing.T) {
	r := &DriverPolicyReconciler{}
	cr := newDriverCR()
	ds := r.buildDaemonSet(cr, driverDaemonSetName(cr.Name))

	if ds.Name != "ttdrv-test-cr" {
		t.Errorf("name = %q; want ttdrv-test-cr", ds.Name)
	}
	if len(ds.OwnerReferences) != 1 || ds.OwnerReferences[0].Name != cr.Name {
		t.Errorf("owner ref not pointing back at CR: %+v", ds.OwnerReferences)
	}
	if got := ds.Spec.Template.Spec.ServiceAccountName; got != "tt-k8s-driver-manager-installer" {
		t.Errorf("default SA = %q; want tt-k8s-driver-manager-installer", got)
	}
	if got := ds.Spec.Template.Spec.PriorityClassName; got != "system-node-critical" {
		t.Errorf("priorityClass = %q; want system-node-critical", got)
	}
	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("got %d containers; want 1", len(ds.Spec.Template.Spec.Containers))
	}
	c := ds.Spec.Template.Spec.Containers[0]
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Error("builder container is not privileged; insmod will fail")
	}
	wantEnv := map[string]string{"TT_KMD_VERSION": "2.8.0"}
	for k, v := range wantEnv {
		found := false
		for _, e := range c.Env {
			if e.Name == k && e.Value == v {
				found = true
			}
		}
		if !found {
			t.Errorf("env %q=%q missing from container", k, v)
		}
	}
	wantMounts := []string{"lib-modules", "usr-src", "tt-kmd-cache", "var-lib-dkms", "host-opt", "host-usr-local-bin"}
	have := map[string]bool{}
	for _, m := range c.VolumeMounts {
		have[m.Name] = true
	}
	for _, name := range wantMounts {
		if !have[name] {
			t.Errorf("expected mount %q not present (host detection / cache / tt-smi delivery will break)", name)
		}
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil {
		t.Error("readiness probe missing; controller will mark pods Ready without checking /sys/module")
	}
}

func TestBuildDaemonSet_InstallerOverride(t *testing.T) {
	r := &DriverPolicyReconciler{}
	cr := newDriverCR()
	cr.Spec.Installer = &driverv1alpha1.InstallerOverride{
		Image:           "ghcr.io/example/builder:v9",
		ImagePullPolicy: corev1.PullAlways,
	}
	ds := r.buildDaemonSet(cr, driverDaemonSetName(cr.Name))
	c := ds.Spec.Template.Spec.Containers[0]
	if c.Image != "ghcr.io/example/builder:v9" {
		t.Errorf("image = %q; want override to take effect", c.Image)
	}
	if c.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("pullPolicy = %q; want Always", c.ImagePullPolicy)
	}
}

func TestBuildDaemonSet_NodeAffinityGatesNFDAndSkip(t *testing.T) {
	r := &DriverPolicyReconciler{}
	cr := newDriverCR()
	ds := r.buildDaemonSet(cr, driverDaemonSetName(cr.Name))
	terms := ds.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 {
		t.Fatalf("got %d nodeSelectorTerms; want 1", len(terms))
	}
	// Look for the NFD-presence In gate and the skip-label DoesNotExist gate
	// — both protect against accidental head-node installs and per-node opt-out.
	gotNFD := false
	gotSkip := false
	for _, e := range terms[0].MatchExpressions {
		if e.Key == LabelTenstorrentPresent && e.Operator == corev1.NodeSelectorOpIn {
			gotNFD = true
		}
		if e.Key == LabelDriverSkip && e.Operator == corev1.NodeSelectorOpDoesNotExist {
			gotSkip = true
		}
	}
	if !gotNFD {
		t.Error("NFD-presence gate missing from node affinity; wide selectors could hit head node")
	}
	if !gotSkip {
		t.Error("driver-skip DoesNotExist gate missing; per-node opt-out will not take effect")
	}
}

func TestSetDriverConditions(t *testing.T) {
	cases := []struct {
		name           string
		mutate         func(cr *driverv1alpha1.TenstorrentDriverPolicy)
		wantReady      metav1.ConditionStatus
		wantProgress   metav1.ConditionStatus
		wantReadyHint  string
		wantProgrHint  string
	}{
		{
			name:           "paused",
			mutate:         func(cr *driverv1alpha1.TenstorrentDriverPolicy) { cr.Spec.Paused = true },
			wantReady:      metav1.ConditionUnknown,
			wantProgress:   metav1.ConditionFalse,
			wantReadyHint:  "Paused",
			wantProgrHint:  "Paused",
		},
		{
			name: "no matching nodes",
			mutate: func(cr *driverv1alpha1.TenstorrentDriverPolicy) {
				cr.Status.Summary = driverv1alpha1.DriverSummary{}
			},
			wantReady:      metav1.ConditionTrue,
			wantProgress:   metav1.ConditionFalse,
			wantReadyHint:  "NoMatchingNodes",
			wantProgrHint:  "Idle",
		},
		{
			name: "all ready",
			mutate: func(cr *driverv1alpha1.TenstorrentDriverPolicy) {
				cr.Status.Summary = driverv1alpha1.DriverSummary{Matched: 3, Desired: 3, Ready: 3}
			},
			wantReady:      metav1.ConditionTrue,
			wantProgress:   metav1.ConditionFalse,
			wantReadyHint:  "AllReady",
			wantProgrHint:  "Idle",
		},
		{
			name: "progressing",
			mutate: func(cr *driverv1alpha1.TenstorrentDriverPolicy) {
				cr.Status.Summary = driverv1alpha1.DriverSummary{Matched: 3, Desired: 3, Ready: 1}
			},
			wantReady:      metav1.ConditionFalse,
			wantProgress:   metav1.ConditionTrue,
			wantReadyHint:  "Installing",
			wantProgrHint:  "Rolling",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := newDriverCR()
			tc.mutate(cr)
			setDriverConditions(cr)
			ready := findCondition(cr.Status.Conditions, "Ready")
			progr := findCondition(cr.Status.Conditions, "Progressing")
			if ready == nil || progr == nil {
				t.Fatalf("missing condition; have %+v", cr.Status.Conditions)
			}
			if ready.Status != tc.wantReady {
				t.Errorf("Ready.Status = %q; want %q", ready.Status, tc.wantReady)
			}
			if progr.Status != tc.wantProgress {
				t.Errorf("Progressing.Status = %q; want %q", progr.Status, tc.wantProgress)
			}
			if !strings.Contains(ready.Reason, tc.wantReadyHint) {
				t.Errorf("Ready.Reason = %q; want it to contain %q", ready.Reason, tc.wantReadyHint)
			}
			if !strings.Contains(progr.Reason, tc.wantProgrHint) {
				t.Errorf("Progressing.Reason = %q; want it to contain %q", progr.Reason, tc.wantProgrHint)
			}
		})
	}
}

func findCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}
