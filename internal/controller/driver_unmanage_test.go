package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	driverv1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/driver/v1alpha1"
)

// testSchemeAll returns a scheme with the core types AND both CRD groups
// registered — the unmanage flow walks across Nodes, Pods, Jobs,
// DaemonSets, and the TTDP CR, so all of them must be on the scheme.
func testSchemeAll(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := testScheme(t) // firmware + core/clientgo
	if err := driverv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// containerNode builds a Node already in install-mode=container — the
// state every in-scope unmanage-target node arrives at.
func containerNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			LabelTenstorrentPresent: "true",
			LabelInstallMode:        "container",
		},
	}}
}

// runReconcile is a tiny convenience wrapper: fewer lines per test.
func runReconcile(t *testing.T, r *DriverPolicyReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

// Scenario 1: unmanage=true on a fresh cluster. One container-managed
// node. Walk through cordon → drain → spawn unload Job. Then mark the
// Job succeeded; the next reconcile should remove the install-mode
// label, uncordon, set Unmanaged, emit Normal event, and delete the DS.
func TestUnmanage_FromScratch_HappyPath(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	cr := newDriverCR()
	cr.Spec.Unmanage = true
	n := containerNode("worker-0")
	// Seed the existing DaemonSet that reconcile will tear down on
	// terminal success.
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name:      driverDaemonSetName(cr.Name),
		Namespace: "tt-operator-system",
	}}

	rec := record.NewFakeRecorder(10)
	r := &DriverPolicyReconciler{
		Scheme:   testSchemeAll(t),
		Recorder: rec,
		Client: fake.NewClientBuilder().
			WithScheme(testSchemeAll(t)).
			WithObjects(cr, n, ds).
			WithStatusSubresource(&driverv1alpha1.TenstorrentDriverPolicy{}).
			Build(),
	}

	// Reconcile 1: cordons the node, sets state=Cordoning.
	runReconcile(t, r, cr.Name)
	var got corev1.Node
	if err := r.Get(context.Background(), types.NamespacedName{Name: n.Name}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if !got.Spec.Unschedulable {
		t.Fatal("node should be cordoned after reconcile 1")
	}
	if got.Annotations[AnnoDriverCordonedBy] != cr.Name {
		t.Errorf("cordoned-by annotation missing, got %+v", got.Annotations)
	}

	// Reconcile 2: cordoned + no device pods → spawn unload Job.
	runReconcile(t, r, cr.Name)
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 unload Job, got %d", len(jobs.Items))
	}
	if jobs.Items[0].Labels[unmanageJobLabelCR] != cr.Name {
		t.Errorf("unload Job missing CR label: %+v", jobs.Items[0].Labels)
	}
	if jobs.Items[0].Labels[unmanageJobLabelNode] != n.Name {
		t.Errorf("unload Job missing node label: %+v", jobs.Items[0].Labels)
	}
	// Pod-spec sanity checks: privileged + hostPID for kernel-state visibility.
	podSpec := jobs.Items[0].Spec.Template.Spec
	if !podSpec.HostPID {
		t.Error("unload pod missing HostPID — rmmod won't see other process FDs")
	}
	if c := podSpec.Containers[0]; c.SecurityContext == nil ||
		c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Error("unload container not privileged — rmmod will fail without CAP_SYS_MODULE")
	}

	// Simulate the Job completing successfully.
	job := jobs.Items[0]
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	}}
	if err := r.Status().Update(context.Background(), &job); err != nil {
		t.Fatalf("status update job: %v", err)
	}

	// Reconcile 3: finalize. Removes install-mode label, uncordons,
	// emits Event, deletes DS, sets Phase=Unmanaged.
	runReconcile(t, r, cr.Name)

	var afterFinalize corev1.Node
	if err := r.Get(context.Background(), types.NamespacedName{Name: n.Name}, &afterFinalize); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if _, present := afterFinalize.Labels[LabelInstallMode]; present {
		t.Errorf("install-mode label should be cleared, still: %q",
			afterFinalize.Labels[LabelInstallMode])
	}
	if afterFinalize.Spec.Unschedulable {
		t.Error("node should be uncordoned after finalize")
	}

	// DaemonSet should be gone.
	var dsAfter appsv1.DaemonSet
	err := r.Get(context.Background(), types.NamespacedName{
		Name: driverDaemonSetName(cr.Name), Namespace: "tt-operator-system",
	}, &dsAfter)
	if !apierrors.IsNotFound(err) {
		t.Errorf("DS should be deleted after unmanage terminal; got err=%v", err)
	}

	// CR status: Phase=Unmanaged, per-node state=Unmanaged with Reason=Unmanaged.
	var crAfter driverv1alpha1.TenstorrentDriverPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &crAfter); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	if crAfter.Status.Phase != driverv1alpha1.DriverPolicyPhaseUnmanaged {
		t.Errorf("phase = %q; want Unmanaged", crAfter.Status.Phase)
	}
	if len(crAfter.Status.Nodes) != 1 {
		t.Fatalf("got %d node status entries; want 1", len(crAfter.Status.Nodes))
	}
	ns := crAfter.Status.Nodes[0]
	if ns.State != driverv1alpha1.DriverNodeStateUnmanaged {
		t.Errorf("node state = %q; want Unmanaged", ns.State)
	}
	if ns.Reason != "Unmanaged" {
		t.Errorf("node reason = %q; want Unmanaged", ns.Reason)
	}

	// Event recorder should have a Normal "Unmanaged" event.
	select {
	case ev := <-rec.Events:
		if !contains(ev, "Unmanaged") {
			t.Errorf("first event = %q; expected to mention Unmanaged", ev)
		}
	default:
		t.Error("expected an Unmanaged event")
	}
}

// Scenario 2: unload Job fails (simulating refcnt > 0). Status flips to
// UnloadFailed with the Job's failure message; uncordon does NOT happen
// (node stays cordoned for operator intervention); a Warning event is
// emitted; phase stays empty (NOT Unmanaged) because strict failure
// halts the batch.
func TestUnmanage_UnloadFailed_StaysCordoned(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	cr := newDriverCR()
	cr.Spec.Unmanage = true
	n := containerNode("worker-0")

	rec := record.NewFakeRecorder(10)
	r := &DriverPolicyReconciler{
		Scheme:   testSchemeAll(t),
		Recorder: rec,
		Client: fake.NewClientBuilder().
			WithScheme(testSchemeAll(t)).
			WithObjects(cr, n).
			WithStatusSubresource(&driverv1alpha1.TenstorrentDriverPolicy{}).
			Build(),
	}

	// Reconcile 1 + 2 to spawn the unload Job.
	runReconcile(t, r, cr.Name)
	runReconcile(t, r, cr.Name)
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 unload Job, got %d", len(jobs.Items))
	}

	// Mark the Job as failed with a refcnt-style message.
	job := jobs.Items[0]
	job.Status.Conditions = []batchv1.JobCondition{{
		Type:    batchv1.JobFailed,
		Status:  corev1.ConditionTrue,
		Message: "FAIL: refcnt > 0; something is holding /dev/tenstorrent",
	}}
	if err := r.Status().Update(context.Background(), &job); err != nil {
		t.Fatalf("status update job: %v", err)
	}

	// Reconcile 3: observes failure. Per-node status = UnloadFailed; node
	// stays cordoned; install-mode still present.
	runReconcile(t, r, cr.Name)

	var afterFail corev1.Node
	if err := r.Get(context.Background(), types.NamespacedName{Name: n.Name}, &afterFail); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if !afterFail.Spec.Unschedulable {
		t.Error("node should STAY cordoned after UnloadFailed for operator intervention")
	}
	if afterFail.Labels[LabelInstallMode] != "container" {
		t.Errorf("install-mode label removed prematurely on UnloadFailed; got %q",
			afterFail.Labels[LabelInstallMode])
	}

	var crAfter driverv1alpha1.TenstorrentDriverPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &crAfter); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	if crAfter.Status.Phase == driverv1alpha1.DriverPolicyPhaseUnmanaged {
		t.Error("phase should NOT be Unmanaged when a node hit UnloadFailed")
	}
	if len(crAfter.Status.Nodes) != 1 {
		t.Fatalf("want 1 node status; got %d", len(crAfter.Status.Nodes))
	}
	ns := crAfter.Status.Nodes[0]
	if ns.State != driverv1alpha1.DriverNodeStateUnloadFailed {
		t.Errorf("state = %q; want UnloadFailed", ns.State)
	}
	if ns.Reason != "UnloadFailed" {
		t.Errorf("reason = %q; want UnloadFailed", ns.Reason)
	}
	if !contains(ns.Message, "refcnt") {
		t.Errorf("message should mention refcnt; got %q", ns.Message)
	}

	// Warning event.
	gotWarning := false
	for {
		select {
		case ev := <-rec.Events:
			if contains(ev, "Warning") && contains(ev, "UnloadFailed") {
				gotWarning = true
			}
		default:
			if !gotWarning {
				t.Error("expected a Warning UnloadFailed event")
			}
			return
		}
	}
}

// Scenario 3: round-trip. unmanage=true → all terminal → set back to
// false → normal reconcile resumes (DaemonSet re-created, phase
// cleared). Validates the reversibility contract.
func TestUnmanage_RoundTrip_ResumesContainerManagement(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	cr := newDriverCR()
	cr.Spec.Unmanage = true
	n := containerNode("worker-0")

	rec := record.NewFakeRecorder(10)
	r := &DriverPolicyReconciler{
		Scheme:   testSchemeAll(t),
		Recorder: rec,
		Client: fake.NewClientBuilder().
			WithScheme(testSchemeAll(t)).
			WithObjects(cr, n).
			WithStatusSubresource(&driverv1alpha1.TenstorrentDriverPolicy{}).
			Build(),
	}

	// Drive through cordon + Job spawn.
	runReconcile(t, r, cr.Name)
	runReconcile(t, r, cr.Name)

	// Complete the Job successfully.
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	job := jobs.Items[0]
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	}}
	if err := r.Status().Update(context.Background(), &job); err != nil {
		t.Fatalf("status update job: %v", err)
	}

	// Reconcile to finalize → Phase=Unmanaged.
	runReconcile(t, r, cr.Name)
	var crUnmanaged driverv1alpha1.TenstorrentDriverPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &crUnmanaged); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	if crUnmanaged.Status.Phase != driverv1alpha1.DriverPolicyPhaseUnmanaged {
		t.Fatalf("phase = %q; want Unmanaged before round-trip", crUnmanaged.Status.Phase)
	}

	// Operator flips unmanage back to false.
	crUnmanaged.Spec.Unmanage = false
	if err := r.Update(context.Background(), &crUnmanaged); err != nil {
		t.Fatalf("update cr: %v", err)
	}

	// Reconcile → normal flow re-creates the DaemonSet, clears Phase.
	runReconcile(t, r, cr.Name)

	var dsAfter appsv1.DaemonSet
	err := r.Get(context.Background(), types.NamespacedName{
		Name: driverDaemonSetName(cr.Name), Namespace: "tt-operator-system",
	}, &dsAfter)
	if err != nil {
		t.Errorf("expected DS to be re-created after unmanage=false; err=%v", err)
	}

	var crResumed driverv1alpha1.TenstorrentDriverPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &crResumed); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	if crResumed.Status.Phase != "" {
		t.Errorf("phase should be cleared after round-trip; got %q", crResumed.Status.Phase)
	}
}

// Strict halt: with two container-managed nodes, if node A's unload Job
// fails before node B's Job is spawned, node B should NOT have its Job
// spawned in subsequent reconciles. The cluster halts mid-batch.
func TestUnmanage_Strict_HaltsBatchOnFirstFailure(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	cr := newDriverCR()
	cr.Spec.Unmanage = true
	nA := containerNode("worker-a")
	nB := containerNode("worker-b")

	rec := record.NewFakeRecorder(10)
	r := &DriverPolicyReconciler{
		Scheme:   testSchemeAll(t),
		Recorder: rec,
		Client: fake.NewClientBuilder().
			WithScheme(testSchemeAll(t)).
			WithObjects(cr, nA, nB).
			WithStatusSubresource(&driverv1alpha1.TenstorrentDriverPolicy{}).
			Build(),
	}

	// Reconcile twice — listMatchedNodes returns nodes in cluster order,
	// so both get cordoned, then both get Jobs. Two reconciles cover
	// cordon-then-spawn for both nodes.
	runReconcile(t, r, cr.Name)
	runReconcile(t, r, cr.Name)

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 2 {
		t.Fatalf("expected 2 unload Jobs (one per node) after batch spawn, got %d", len(jobs.Items))
	}

	// Mark Job A as failed. Now the halt should engage; on the next
	// reconcile, Job A's status becomes UnloadFailed but the controller
	// must not re-spawn a fresh Job for either node (it sees Job B still
	// in flight + Node A in UnloadFailed and stops).
	for i := range jobs.Items {
		if jobs.Items[i].Labels[unmanageJobLabelNode] == nA.Name {
			j := jobs.Items[i]
			j.Status.Conditions = []batchv1.JobCondition{{
				Type:    batchv1.JobFailed,
				Status:  corev1.ConditionTrue,
				Message: "FAIL: refcnt > 0",
			}}
			if err := r.Status().Update(context.Background(), &j); err != nil {
				t.Fatalf("status update job: %v", err)
			}
		}
	}

	// Multiple reconciles after the failure — the fleet must NOT proceed
	// to remove Node B's install-mode label or delete the DS, and the
	// Job count must not grow (no respawn).
	for i := 0; i < 3; i++ {
		runReconcile(t, r, cr.Name)
	}

	var jobsAfter batchv1.JobList
	if err := r.List(context.Background(), &jobsAfter); err != nil {
		t.Fatalf("list jobs after halt: %v", err)
	}
	if len(jobsAfter.Items) != 2 {
		t.Errorf("unload Job count grew after halt; want 2, got %d", len(jobsAfter.Items))
	}

	var crAfter driverv1alpha1.TenstorrentDriverPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &crAfter); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	if crAfter.Status.Phase == driverv1alpha1.DriverPolicyPhaseUnmanaged {
		t.Error("phase should NOT be Unmanaged when one node halted with UnloadFailed")
	}
}

// contains is a thin alias around strings.Contains so test assertions
// read as English ("contains UnloadFailed") without an awkward
// `strings.` prefix on every line.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
