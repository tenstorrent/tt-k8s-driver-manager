package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	firmwarev1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/firmware/v1alpha1"
	"github.com/tenstorrent/tt-k8s-driver-manager/internal/metrics"
)

// finishedJob builds a Job that ran for `dur` and ended in the given
// terminal condition. Successful Jobs get a CompletionTime; failed ones
// don't, matching what the Job controller actually writes.
func finishedJob(name string, dur time.Duration, condType batchv1.JobConditionType) batchv1.Job {
	start := metav1.NewTime(time.Now().Add(-dur))
	end := metav1.NewTime(start.Add(dur))
	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: operatorNamespace()},
		Status: batchv1.JobStatus{
			StartTime: &start,
			Conditions: []batchv1.JobCondition{{
				Type:               condType,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: end,
			}},
		},
	}
	if condType == batchv1.JobComplete {
		job.Status.CompletionTime = &end
	}
	return job
}

func TestFlashJobDuration_Succeeded(t *testing.T) {
	job := finishedJob("j", 90*time.Second, batchv1.JobComplete)
	got, ok := flashJobDuration(&job)
	if !ok {
		t.Fatal("no duration for a completed Job")
	}
	if got.Round(time.Second) != 90*time.Second {
		t.Errorf("duration = %s; want 90s", got)
	}
}

// A failed Job has no CompletionTime, so the terminal condition's
// LastTransitionTime has to stand in — otherwise every failure would be
// dropped from the histogram, which is exactly the tail we care about.
func TestFlashJobDuration_FailedUsesConditionTime(t *testing.T) {
	job := finishedJob("j", 300*time.Second, batchv1.JobFailed)
	if job.Status.CompletionTime != nil {
		t.Fatal("test fixture wrong: failed Jobs have no CompletionTime")
	}
	got, ok := flashJobDuration(&job)
	if !ok {
		t.Fatal("no duration for a failed Job")
	}
	if got.Round(time.Second) != 300*time.Second {
		t.Errorf("duration = %s; want 300s", got)
	}
}

func TestFlashJobDuration_NotStarted(t *testing.T) {
	if _, ok := flashJobDuration(&batchv1.Job{}); ok {
		t.Error("reported a duration for a Job that never started")
	}
}

func TestCountInFlightJobs(t *testing.T) {
	jobs := []batchv1.Job{
		finishedJob("done", time.Minute, batchv1.JobComplete),
		finishedJob("failed", time.Minute, batchv1.JobFailed),
		{ObjectMeta: metav1.ObjectMeta{Name: "running"}},
	}
	if got := countInFlightJobs(jobs); got != 1 {
		t.Errorf("countInFlightJobs = %d; want 1", got)
	}
}

// The controller is level-triggered: a terminal Job keeps showing up on
// every reconcile until its TTL expires. Recording it more than once would
// make the histogram count reconciles instead of flashes.
func TestRecordFlashJobMetrics_CountsEachJobOnce(t *testing.T) {
	const cr = "dedup-cr"
	t.Cleanup(func() {
		metrics.DeleteFirmwarePolicySeries(cr)
		firmwareEvents.Forget(cr)
	})

	jobs := []batchv1.Job{
		finishedJob("ok-1", 60*time.Second, batchv1.JobComplete),
		finishedJob("bad-1", 120*time.Second, batchv1.JobFailed),
		{ObjectMeta: metav1.ObjectMeta{Name: "still-running"}},
	}

	keys := recordFlashJobMetrics(cr, jobs)
	if len(keys) != 3 {
		t.Fatalf("returned %d dedup keys; want 3 (one per existing Job)", len(keys))
	}
	firmwareEvents.Retain(cr, keys)
	// Second and third reconciles see the same cluster state.
	firmwareEvents.Retain(cr, recordFlashJobMetrics(cr, jobs))
	firmwareEvents.Retain(cr, recordFlashJobMetrics(cr, jobs))

	if got := testutil.ToFloat64(metrics.FirmwareFlashJobsTotal.WithLabelValues(cr, metrics.ResultSuccess)); got != 1 {
		t.Errorf("success count = %v; want 1", got)
	}
	if got := testutil.ToFloat64(metrics.FirmwareFlashJobsTotal.WithLabelValues(cr, metrics.ResultFailed)); got != 1 {
		t.Errorf("failure count = %v; want 1", got)
	}
	// _count on the histogram must agree with the counter.
	if got := testutil.CollectAndCount(metrics.FirmwareFlashDuration); got != 2 {
		t.Errorf("histogram series = %d; want 2 (success + failure)", got)
	}
}

// A Job that ages out and is later recreated for the same node+version is a
// genuinely new flash, so it must count again once Retain has pruned it.
func TestRecordFlashJobMetrics_RecountsAfterJobDisappears(t *testing.T) {
	const cr = "recount-cr"
	t.Cleanup(func() {
		metrics.DeleteFirmwarePolicySeries(cr)
		firmwareEvents.Forget(cr)
	})

	jobs := []batchv1.Job{finishedJob("ok-1", 60*time.Second, batchv1.JobComplete)}
	firmwareEvents.Retain(cr, recordFlashJobMetrics(cr, jobs))
	// TTL deletes the Job: nothing left to retain.
	firmwareEvents.Retain(cr, recordFlashJobMetrics(cr, nil))
	firmwareEvents.Retain(cr, recordFlashJobMetrics(cr, jobs))

	if got := testutil.ToFloat64(metrics.FirmwareFlashJobsTotal.WithLabelValues(cr, metrics.ResultSuccess)); got != 2 {
		t.Errorf("success count = %v; want 2", got)
	}
}

func TestIsDrainTimeout(t *testing.T) {
	timedOut := firmwarev1alpha1.NodeStatus{
		State:   firmwarev1alpha1.NodeStateFailed,
		Message: MessageDrainTimeoutPrefix + "10m0s; blocking pods: ns/pod",
	}
	if !isDrainTimeout(timedOut) {
		t.Error("drain-timeout failure not recognized")
	}
	flashFailed := firmwarev1alpha1.NodeStatus{
		State:   firmwarev1alpha1.NodeStateFailed,
		Message: "Flash Job failed; see Job logs",
	}
	if isDrainTimeout(flashFailed) {
		t.Error("flash failure misread as a drain timeout")
	}
	draining := firmwarev1alpha1.NodeStatus{State: firmwarev1alpha1.NodeStateDraining}
	if isDrainTimeout(draining) {
		t.Error("in-progress drain misread as a timeout")
	}
}

// A node sits in the timed-out state until a human clears the blocker, and
// the reconciler re-derives it every 30s. One stall must be one increment.
func TestRecordDrainTimeoutMetrics_OncePerStall(t *testing.T) {
	const cr = "timeout-cr"
	t.Cleanup(func() {
		metrics.DeleteFirmwarePolicySeries(cr)
		firmwareEvents.Forget(cr)
	})

	stuck := []firmwarev1alpha1.NodeStatus{{
		Name:    "e01cs01",
		State:   firmwarev1alpha1.NodeStateFailed,
		Message: MessageDrainTimeoutPrefix + "10m0s; blocking pods: ns/pod",
	}}

	for range 3 {
		firmwareEvents.Retain(cr, recordDrainTimeoutMetrics(cr, stuck))
	}
	if got := testutil.ToFloat64(metrics.FirmwareDrainBlockedTotal.WithLabelValues(cr, metrics.ReasonDrainTimeout)); got != 1 {
		t.Errorf("drain-timeout count = %v; want 1", got)
	}

	// Blocker cleared, then the node stalls again — that's a second event.
	firmwareEvents.Retain(cr, recordDrainTimeoutMetrics(cr, nil))
	firmwareEvents.Retain(cr, recordDrainTimeoutMetrics(cr, stuck))
	if got := testutil.ToFloat64(metrics.FirmwareDrainBlockedTotal.WithLabelValues(cr, metrics.ReasonDrainTimeout)); got != 2 {
		t.Errorf("drain-timeout count after a second stall = %v; want 2", got)
	}
}

func TestRecordFirmwarePolicyMetrics_PublishesZeroStates(t *testing.T) {
	cr := newCR()
	cr.Name = "states-cr"
	t.Cleanup(func() { metrics.DeleteFirmwarePolicySeries(cr.Name) })

	recordFirmwarePolicyMetrics(cr, []firmwarev1alpha1.NodeStatus{
		{Name: "a", State: firmwarev1alpha1.NodeStateDone},
		{Name: "b", State: firmwarev1alpha1.NodeStateDone},
		{Name: "c", State: firmwarev1alpha1.NodeStateFlashing},
	})

	if got := testutil.ToFloat64(metrics.FirmwarePolicyNodes.WithLabelValues(cr.Name, "Done")); got != 2 {
		t.Errorf("Done = %v; want 2", got)
	}
	if got := testutil.ToFloat64(metrics.FirmwarePolicyNodes.WithLabelValues(cr.Name, "Flashing")); got != 1 {
		t.Errorf("Flashing = %v; want 1", got)
	}
	// States nobody is in are published as 0 so dashboards don't see gaps.
	if got := testutil.ToFloat64(metrics.FirmwarePolicyNodes.WithLabelValues(cr.Name, "Failed")); got != 0 {
		t.Errorf("Failed = %v; want 0", got)
	}
	if got := testutil.CollectAndCount(metrics.FirmwarePolicyNodes); got != len(firmwareNodeStates) {
		t.Errorf("series = %d; want %d (one per state)", got, len(firmwareNodeStates))
	}
	if got := testutil.ToFloat64(metrics.FirmwarePolicyDesiredVersion.WithLabelValues(cr.Name, cr.Spec.Version)); got != 1 {
		t.Errorf("desired-version info gauge = %v; want 1", got)
	}
}

func TestRecordFWVersionMetrics_CountsLabeledNodesOnly(t *testing.T) {
	t.Cleanup(metrics.FirmwareNodesByFWVersion.Reset)

	r := &FirmwarePolicyReconciler{
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
			ptr(node("tt-1", map[string]string{LabelFWVersion: "18.5.0.0"})),
			ptr(node("tt-2", map[string]string{LabelFWVersion: "18.5.0.0"})),
			ptr(node("tt-3", map[string]string{LabelFWVersion: "19.8.0.0"})),
			ptr(node("cpu-only", nil)),
		).Build(),
	}
	if err := r.recordFWVersionMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := testutil.ToFloat64(metrics.FirmwareNodesByFWVersion.WithLabelValues("18.5.0.0")); got != 2 {
		t.Errorf("18.5.0.0 = %v; want 2", got)
	}
	// Unlabeled nodes are skipped — an "" bucket would sweep in every
	// non-Tenstorrent node in the cluster.
	if got := testutil.CollectAndCount(metrics.FirmwareNodesByFWVersion); got != 2 {
		t.Errorf("series = %d; want 2", got)
	}
}

func ptr(n corev1.Node) *corev1.Node { return &n }
