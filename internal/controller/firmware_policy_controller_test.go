package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	firmwarev1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/firmware/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := firmwarev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func node(name string, labels map[string]string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func newCR() *firmwarev1alpha1.TenstorrentFirmwarePolicy {
	return &firmwarev1alpha1.TenstorrentFirmwarePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cr", Generation: 1},
		Spec: firmwarev1alpha1.TenstorrentFirmwarePolicySpec{
			Version:      "19.8.0",
			NodeSelector: metav1.LabelSelector{},
			UpgradePolicy: firmwarev1alpha1.UpgradePolicy{
				MaxParallel: 1,
			},
		},
	}
}

// nodeSelector and the NFD label gate are the cluster's safety belt against
// accidentally flashing the head node — these must hold even when the
// selector is permissive.
func TestMatchedNodes_RequiresNFDLabel(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "true")

	cr := newCR()
	r := &FirmwarePolicyReconciler{
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
			cr,
			toClientObject(node("with-tt", map[string]string{LabelTenstorrentPresent: "true"})),
			toClientObject(node("without-tt", nil)),
		).Build(),
	}

	got, err := r.matchedNodes(context.Background(), cr)
	if err != nil {
		t.Fatalf("matchedNodes: %v", err)
	}
	if len(got) != 1 || got[0].Name != "with-tt" {
		t.Fatalf("expected only with-tt, got %+v", got)
	}
}

// REQUIRE_TT_PCI_LABEL=false disables the safety belt — used on dev clusters.
func TestMatchedNodes_DisabledLabelRequirement(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	cr := newCR()
	r := &FirmwarePolicyReconciler{
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
			cr,
			toClientObject(node("a", nil)),
			toClientObject(node("b", nil)),
		).Build(),
	}
	got, err := r.matchedNodes(context.Background(), cr)
	if err != nil {
		t.Fatalf("matchedNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(got))
	}
}

// The skip label opts a node out even if it otherwise matches.
func TestMatchedNodes_SkipLabel(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "true")
	cr := newCR()
	r := &FirmwarePolicyReconciler{
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
			cr,
			toClientObject(node("included", map[string]string{LabelTenstorrentPresent: "true"})),
			toClientObject(node("skipped", map[string]string{
				LabelTenstorrentPresent: "true",
				LabelSkip:               "true",
			})),
		).Build(),
	}
	got, err := r.matchedNodes(context.Background(), cr)
	if err != nil {
		t.Fatalf("matchedNodes: %v", err)
	}
	if len(got) != 1 || got[0].Name != "included" {
		t.Fatalf("skip label not honored: %+v", got)
	}
}

// End-to-end happy path against a fake client: a matched node has no Job yet,
// reconcile should spawn one and report InProgress.
func TestReconcile_SpawnsFlashJob(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "true")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	cr := newCR()
	// Drain disabled (dev-mode); the controller spawns the Job directly.
	disable := false
	cr.Spec.UpgradePolicy.Drain.Enable = &disable

	n := node("worker-0", map[string]string{LabelTenstorrentPresent: "true"})

	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(cr, toClientObject(n)).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Requeue && res.RequeueAfter == 0 {
		t.Errorf("expected requeue while node is in progress, got %+v", res)
	}

	// Job should exist.
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 job spawned, got %d", len(jobs.Items))
	}
	job := jobs.Items[0]
	if job.Labels[JobLabelCR] != cr.Name || job.Labels[JobLabelNode] != "worker-0" {
		t.Errorf("job missing identifying labels: %v", job.Labels)
	}

	// Status should reflect 1 in-progress.
	var got firmwarev1alpha1.TenstorrentFirmwarePolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &got); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	if got.Status.Summary.InProgress != 1 {
		t.Errorf("expected 1 InProgress, got summary=%+v", got.Status.Summary)
	}
}

// Drain enabled, no device pods on node → first reconcile cordons + spawns
// the flash Job (no pods to evict). Validates the happy-path drain → flash
// transition.
func TestReconcile_DrainEnabled_NoPods_CordonsAndFlashes(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	enable := true
	cr := newCR()
	cr.Spec.UpgradePolicy.Drain.Enable = &enable
	n := node("worker-0", nil)

	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(cr, toClientObject(n)).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}

	// 1st reconcile: drain enabled, node not yet cordoned → Cordoning advances cordon.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile #1: %v", err)
	}
	var afterCordon corev1.Node
	if err := r.Get(context.Background(), types.NamespacedName{Name: n.Name}, &afterCordon); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if !afterCordon.Spec.Unschedulable {
		t.Fatal("node should be cordoned after first reconcile")
	}
	if afterCordon.Annotations[AnnoCordonedBy] != cr.Name {
		t.Errorf("expected cordoned-by=%s, got %q", cr.Name, afterCordon.Annotations[AnnoCordonedBy])
	}

	// 2nd reconcile: cordoned, no device pods → Pending → spawn Job.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile #2: %v", err)
	}
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 flash Job after drain, got %d", len(jobs.Items))
	}
}

// Drain enabled with a device-using pod on the node: state stays Draining
// until the pod is gone.
func TestReconcile_DrainEnabled_WaitsForDevicePods(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	enable := true
	cr := newCR()
	cr.Spec.UpgradePolicy.Drain.Enable = &enable
	n := node("worker-0", nil)

	owner := true
	devicePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "training-pod", Namespace: "workloads",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "ReplicaSet", Name: "training", APIVersion: "apps/v1",
				UID: "rs-uid", Controller: &owner,
			}},
		},
		Spec: corev1.PodSpec{
			NodeName: n.Name,
			Volumes:  []corev1.Volume{{Name: "tt", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/tenstorrent"}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(cr, toClientObject(n), devicePod).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}

	// Cordon + start draining.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile #1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile #2: %v", err)
	}

	// Reconcile #2 observed the pod, started Draining, and called evict.
	// Status reflects Draining. (In a real cluster eviction is graceful;
	// fake client deletes immediately, but the controller's flow doesn't
	// spawn the Job in the same reconcile as the eviction — it waits for
	// the next observation to see the empty node.)
	var jobs batchv1.JobList
	_ = r.List(context.Background(), &jobs)
	if len(jobs.Items) != 0 {
		t.Fatalf("Job must not be spawned in the same reconcile that evicted; got %d", len(jobs.Items))
	}
	var got firmwarev1alpha1.TenstorrentFirmwarePolicy
	_ = r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &got)
	if len(got.Status.Nodes) != 1 || got.Status.Nodes[0].State != firmwarev1alpha1.NodeStateDraining {
		t.Errorf("expected node in Draining; got %+v", got.Status.Nodes)
	}

	// Reconcile #3: pod is gone (fake client eviction deleted it; real
	// cluster would too once termination completes). Job gets spawned.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile #3: %v", err)
	}
	_ = r.List(context.Background(), &jobs)
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 Job after drain completed; got %d", len(jobs.Items))
	}
}

// DaemonSet-managed pods don't block drain.
func TestReconcile_DrainIgnoresDaemonSetPods(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	enable := true
	cr := newCR()
	cr.Spec.UpgradePolicy.Drain.Enable = &enable
	n := node("worker-0", nil)

	owner := true
	dsPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fluentd", Namespace: "logging",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "DaemonSet", Name: "fluentd", APIVersion: "apps/v1",
				UID: "ds-uid", Controller: &owner,
			}},
		},
		Spec: corev1.PodSpec{
			NodeName: n.Name,
			Volumes:  []corev1.Volume{{Name: "tt", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/tenstorrent"}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(cr, toClientObject(n), dsPod).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}

	// Cordon then drain-check.
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
			t.Fatalf("reconcile #%d: %v", i+1, err)
		}
	}

	// DS pod should be ignored → Job spawned despite the pod existing.
	var jobs batchv1.JobList
	_ = r.List(context.Background(), &jobs)
	if len(jobs.Items) != 1 {
		t.Fatalf("DS-owned pod should not block drain; expected Job, got %d", len(jobs.Items))
	}
}

// Operator-namespace pods (our own flasher Jobs, NFD, etc.) don't block drain.
func TestReconcile_DrainIgnoresOperatorOwnPods(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	enable := true
	cr := newCR()
	cr.Spec.UpgradePolicy.Drain.Enable = &enable
	n := node("worker-0", nil)

	// A "device-using" pod in OUR namespace — should be ignored.
	ourPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "old-flash-job-xyz", Namespace: "tt-operator-system"},
		Spec: corev1.PodSpec{
			NodeName: n.Name,
			Volumes:  []corev1.Volume{{Name: "tt", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/tenstorrent"}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(cr, toClientObject(n), ourPod).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}

	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
			t.Fatalf("reconcile #%d: %v", i+1, err)
		}
	}

	var jobs batchv1.JobList
	_ = r.List(context.Background(), &jobs)
	if len(jobs.Items) != 1 {
		t.Fatalf("operator-ns pod should not block drain; expected Job, got %d", len(jobs.Items))
	}
}

// After Job completes successfully, drain-enabled node goes Uncordoning →
// uncordon → Done; node ends up un-cordoned.
func TestReconcile_DrainEnabled_UncordonOnDone(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	enable := true
	cr := newCR()
	cr.Spec.UpgradePolicy.Drain.Enable = &enable
	n := node("worker-0", map[string]string{LabelOwnerCR: cr.Name})
	// Simulate "already cordoned by us" — this is the state we'd be in
	// when the flash Job has just completed successfully.
	n.Spec.Unschedulable = true
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[AnnoCordonedBy] = cr.Name

	// Pre-create the Job marked Complete.
	jName := jobName(cr, n.Name, cr.Spec.Version)
	existing := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jName, Namespace: "tt-operator-system"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}

	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(cr, toClientObject(n), existing).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}

	// 1st reconcile observes Uncordoning, advances by clearing the cordon.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile #1: %v", err)
	}
	var afterUncordon corev1.Node
	_ = r.Get(context.Background(), types.NamespacedName{Name: n.Name}, &afterUncordon)
	if afterUncordon.Spec.Unschedulable {
		t.Error("node should be uncordoned after first reconcile post-Job")
	}
	if afterUncordon.Annotations[AnnoCordonedBy] != "" {
		t.Errorf("cordoned-by annotation should be cleared; got %q", afterUncordon.Annotations[AnnoCordonedBy])
	}

	// 2nd reconcile sees Job Complete + node uncordoned → Done.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile #2: %v", err)
	}
	var got firmwarev1alpha1.TenstorrentFirmwarePolicy
	_ = r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &got)
	if got.Status.Summary.UpToDate != 1 {
		t.Errorf("expected UpToDate=1, got %+v", got.Status.Summary)
	}
}

// Two CRs targeting the same node — second one sees a Conflict, doesn't spawn.
func TestReconcile_ConflictBetweenCRs(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	disable := false
	first := newCR()
	first.Name = "first"
	first.Spec.UpgradePolicy.Drain.Enable = &disable
	second := newCR()
	second.Name = "second"
	second.Spec.UpgradePolicy.Drain.Enable = &disable

	// Node is already owned by "first" from a previous reconcile.
	n := node("worker-0", map[string]string{LabelOwnerCR: "first"})

	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(first, second, toClientObject(n)).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "second"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var jobs batchv1.JobList
	_ = r.List(context.Background(), &jobs)
	if len(jobs.Items) != 0 {
		t.Fatalf("second CR should not spawn a Job for a node owned by first; got %d", len(jobs.Items))
	}

	var got firmwarev1alpha1.TenstorrentFirmwarePolicy
	_ = r.Get(context.Background(), types.NamespacedName{Name: "second"}, &got)
	if len(got.Status.Nodes) != 1 || got.Status.Nodes[0].Message == "" {
		t.Errorf("expected conflict message, got %+v", got.Status.Nodes)
	}
}

// Paused CR: no Jobs spawn even though a matched node is Pending.
func TestReconcile_PausedNoOp(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	disable := false
	cr := newCR()
	cr.Spec.Paused = true
	cr.Spec.UpgradePolicy.Drain.Enable = &disable

	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(cr, toClientObject(node("worker-0", nil))).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var jobs batchv1.JobList
	_ = r.List(context.Background(), &jobs)
	if len(jobs.Items) != 0 {
		t.Fatalf("paused CR must not spawn a Job; got %d", len(jobs.Items))
	}
}

// MaxParallel caps the number of simultaneously-running Jobs.
func TestReconcile_MaxParallelBound(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	disable := false
	cr := newCR()
	cr.Spec.UpgradePolicy.Drain.Enable = &disable
	cr.Spec.UpgradePolicy.MaxParallel = 2

	objs := []runtime.Object{cr}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		objs = append(objs, toClientObject(node(name, nil)))
	}
	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(toClientObjects(objs)...).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 2 {
		t.Fatalf("maxParallel=2 should spawn 2 Jobs, got %d", len(jobs.Items))
	}
}

// A Job that has already completed → node reports Done.
func TestReconcile_JobCompleteMeansDone(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	disable := false
	cr := newCR()
	cr.Spec.UpgradePolicy.Drain.Enable = &disable
	n := node("worker-0", nil)

	// Pre-create a Job named what the controller would name it, marked Complete.
	jName := jobName(cr, n.Name, cr.Spec.Version)
	existing := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jName, Namespace: "tt-operator-system"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}

	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(cr, toClientObject(n), existing).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got firmwarev1alpha1.TenstorrentFirmwarePolicy
	_ = r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &got)
	if got.Status.Summary.UpToDate != 1 {
		t.Errorf("expected UpToDate=1, got %+v", got.Status.Summary)
	}
	if len(got.Status.Nodes) != 1 || got.Status.Nodes[0].State != firmwarev1alpha1.NodeStateDone {
		t.Errorf("expected node state Done, got %+v", got.Status.Nodes)
	}
}

// Helper to satisfy the fake builder's client.Object requirement.
func toClientObject(n corev1.Node) *corev1.Node {
	out := n
	return &out
}

// toClientObjects unwraps a []runtime.Object into the variadic client.Object the fake builder wants.
func toClientObjects(in []runtime.Object) []client.Object {
	out := make([]client.Object, 0, len(in))
	for _, o := range in {
		if c, ok := o.(client.Object); ok {
			out = append(out, c)
		}
	}
	return out
}
