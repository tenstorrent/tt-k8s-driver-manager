package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	firmwarev1alpha1 "github.com/tenstorrent/tt-k8s-driver-manager/api/firmware/v1alpha1"
)

// flashJob builds a Job named the way the controller names it, with the
// identifying labels the pod-lookup path matches on.
func flashJob(cr *firmwarev1alpha1.TenstorrentFirmwarePolicy, nodeName string, conds ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(cr, nodeName, cr.Spec.Version),
			Namespace: "tt-operator-system",
			Labels: map[string]string{
				JobLabelCR:   cr.Name,
				JobLabelNode: nodeName,
			},
		},
		Status: batchv1.JobStatus{Conditions: conds},
	}
}

func jobCond(t batchv1.JobConditionType) batchv1.JobCondition {
	return batchv1.JobCondition{Type: t, Status: corev1.ConditionTrue}
}

// flasherPod builds the flash Job's pod with a single container stuck in
// the given kubelet Waiting.Reason ("" for a running container).
func flasherPod(cr *firmwarev1alpha1.TenstorrentFirmwarePolicy, nodeName, waitingReason string) *corev1.Pod {
	cs := corev1.ContainerStatus{Name: "flasher"}
	if waitingReason != "" {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: waitingReason}
	} else {
		cs.State.Running = &corev1.ContainerStateRunning{}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "flash-" + nodeName,
			Namespace: "tt-operator-system",
			Labels: map[string]string{
				JobLabelCR:   cr.Name,
				JobLabelNode: nodeName,
			},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

// TestObserveNode_Reasons pins the (state, reason) pair for every branch
// observeNode can take. The reason codes flow into scripts and
// dashboards, so a change here is an API change.
func TestObserveNode_Reasons(t *testing.T) {
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	enable, disable := true, false

	cases := []struct {
		name       string
		drain      *bool
		node       corev1.Node
		objects    []client.Object
		wantState  firmwarev1alpha1.NodeState
		wantReason string
	}{
		{
			name:       "no Job, drain disabled = Pending, ready to flash, no reason",
			drain:      &disable,
			node:       node("worker-0", nil),
			wantState:  firmwarev1alpha1.NodeStatePending,
			wantReason: "",
		},
		{
			name:       "Job running = Flashing",
			drain:      &disable,
			node:       node("worker-0", nil),
			objects:    []client.Object{flashJob(newCR(), "worker-0")},
			wantState:  firmwarev1alpha1.NodeStateFlashing,
			wantReason: ReasonFlashing,
		},
		{
			name:  "Job running but pod can't pull image = still Flashing, image-pull reason",
			drain: &disable,
			node:  node("worker-0", nil),
			objects: []client.Object{
				flashJob(newCR(), "worker-0"),
				flasherPod(newCR(), "worker-0", "ImagePullBackOff"),
			},
			wantState:  firmwarev1alpha1.NodeStateFlashing,
			wantReason: ReasonFlasherImagePullFailed,
		},
		{
			name:  "ErrImagePull is treated the same as ImagePullBackOff",
			drain: &disable,
			node:  node("worker-0", nil),
			objects: []client.Object{
				flashJob(newCR(), "worker-0"),
				flasherPod(newCR(), "worker-0", "ErrImagePull"),
			},
			wantState:  firmwarev1alpha1.NodeStateFlashing,
			wantReason: ReasonFlasherImagePullFailed,
		},
		{
			name:  "a running pod leaves the plain Flashing reason alone",
			drain: &disable,
			node:  node("worker-0", nil),
			objects: []client.Object{
				flashJob(newCR(), "worker-0"),
				flasherPod(newCR(), "worker-0", ""),
			},
			wantState:  firmwarev1alpha1.NodeStateFlashing,
			wantReason: ReasonFlashing,
		},
		{
			name:       "Job Complete, nothing to uncordon = Done",
			drain:      &disable,
			node:       node("worker-0", nil),
			objects:    []client.Object{flashJob(newCR(), "worker-0", jobCond(batchv1.JobComplete))},
			wantState:  firmwarev1alpha1.NodeStateDone,
			wantReason: ReasonFlashSucceeded,
		},
		{
			name:       "Job Failed = Failed",
			drain:      &disable,
			node:       node("worker-0", nil),
			objects:    []client.Object{flashJob(newCR(), "worker-0", jobCond(batchv1.JobFailed))},
			wantState:  firmwarev1alpha1.NodeStateFailed,
			wantReason: ReasonFlashJobFailed,
		},
		{
			name:       "drain enabled, node not cordoned yet = Cordoning",
			drain:      &enable,
			node:       node("worker-0", nil),
			wantState:  firmwarev1alpha1.NodeStateCordoning,
			wantReason: ReasonCordoning,
		},
		{
			name:  "cordoned by someone else = Pending with ExternalCordon",
			drain: &enable,
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-0"},
				Spec:       corev1.NodeSpec{Unschedulable: true},
			},
			wantState:  firmwarev1alpha1.NodeStatePending,
			wantReason: ReasonExternalCordon,
		},
		{
			name:  "we cordoned, no device pods = Pending, ready to spawn, no reason",
			drain: &enable,
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "worker-0",
					Annotations: map[string]string{AnnoCordonedBy: "test-cr"},
				},
				Spec: corev1.NodeSpec{Unschedulable: true},
			},
			wantState:  firmwarev1alpha1.NodeStatePending,
			wantReason: "",
		},
		{
			name:  "Job Complete while our cordon is still on = Uncordoning",
			drain: &enable,
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "worker-0",
					Annotations: map[string]string{AnnoCordonedBy: "test-cr"},
				},
				Spec: corev1.NodeSpec{Unschedulable: true},
			},
			objects:    []client.Object{flashJob(newCR(), "worker-0", jobCond(batchv1.JobComplete))},
			wantState:  firmwarev1alpha1.NodeStateUncordoning,
			wantReason: ReasonUncordoning,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := newCR()
			cr.Spec.UpgradePolicy.Drain.Enable = tc.drain

			objs := append([]client.Object{cr}, tc.objects...)
			r := &FirmwarePolicyReconciler{
				Scheme: testScheme(t),
				Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build(),
			}

			ns := r.observeNode(context.Background(), cr, tc.node)
			if ns.State != tc.wantState {
				t.Errorf("state = %q; want %q", ns.State, tc.wantState)
			}
			if ns.Reason != tc.wantReason {
				t.Errorf("reason = %q; want %q (message: %q)", ns.Reason, tc.wantReason, ns.Message)
			}
		})
	}
}

// A drain window that expires with device pods still holding
// /dev/tenstorrent is terminal, and must say so by reason — the
// existing message prefix is what the metrics path reads, not humans.
func TestObserveNode_DrainTimeoutReason(t *testing.T) {
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	enable := true
	cr := newCR()
	cr.Spec.UpgradePolicy.Drain.Enable = &enable
	cr.Spec.UpgradePolicy.Drain.TimeoutSeconds = 1

	n := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-0",
			Annotations: map[string]string{
				AnnoCordonedBy: cr.Name,
				AnnoCordonedAt: metav1.NewTime(time.Now().Add(-time.Hour)).Format(time.RFC3339),
			},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
	}

	// A ReplicaSet-owned pod holding /dev/tenstorrent keeps the drain
	// from ever finishing.
	controller := true
	holder := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "holder", Namespace: "workloads",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "ReplicaSet", Name: "training", APIVersion: "apps/v1",
				UID: "rs-uid", Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{
			NodeName: "worker-0",
			Volumes: []corev1.Volume{{
				Name: "tt",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/dev/tenstorrent"},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(cr, toClientObject(n), holder).Build(),
	}

	ns := r.observeNode(context.Background(), cr, n)
	if ns.State != firmwarev1alpha1.NodeStateFailed {
		t.Fatalf("state = %q; want Failed (message: %q)", ns.State, ns.Message)
	}
	if ns.Reason != ReasonDrainTimeout {
		t.Errorf("reason = %q; want %q", ns.Reason, ReasonDrainTimeout)
	}
	if !strings.HasPrefix(ns.Message, MessageDrainTimeoutPrefix) {
		t.Errorf("message %q lost the prefix the metrics path reads", ns.Message)
	}
}

// TestFirmwareRecordNodeStateEvent confirms the recorder wiring: reasons that
// need a human get Warning, rollout progress gets Normal, and a nil
// Recorder or empty reason is a safe no-op.
func TestFirmwareRecordNodeStateEvent(t *testing.T) {
	cr := newCR()

	warnings := []string{
		ReasonFlashJobFailed,
		ReasonFlasherImagePullFailed,
		ReasonDrainTimeout,
		ReasonEvictionBlocked,
		ReasonNodeConflict,
		ReasonExternalCordon,
	}
	for _, reason := range warnings {
		t.Run("Warning for "+reason, func(t *testing.T) {
			fr := record.NewFakeRecorder(4)
			r := &FirmwarePolicyReconciler{Recorder: fr}
			r.recordNodeStateEvent(cr, "e01cs01", firmwarev1alpha1.NodeStateFlashing, reason, "boom")
			select {
			case evt := <-fr.Events:
				if !strings.HasPrefix(evt, "Warning ") {
					t.Errorf("event type = %q; want Warning prefix", evt)
				}
				if !strings.Contains(evt, reason) {
					t.Errorf("event %q missing reason %q", evt, reason)
				}
				if !strings.Contains(evt, "e01cs01") {
					t.Errorf("event %q missing node name", evt)
				}
			default:
				t.Fatal("no event recorded")
			}
		})
	}

	normals := []string{
		ReasonFlashing,
		ReasonFlashSucceeded,
		ReasonCordoning,
		ReasonDraining,
		ReasonUncordoning,
		ReasonPaused,
		ReasonRolloutHalted,
		ReasonAutoUpgradeDisabled,
		ReasonTransientAPIError,
	}
	for _, reason := range normals {
		t.Run("Normal for "+reason, func(t *testing.T) {
			fr := record.NewFakeRecorder(4)
			r := &FirmwarePolicyReconciler{Recorder: fr}
			r.recordNodeStateEvent(cr, "e01cs02", firmwarev1alpha1.NodeStateFlashing, reason, "")
			select {
			case evt := <-fr.Events:
				if !strings.HasPrefix(evt, "Normal ") {
					t.Errorf("event type = %q; want Normal prefix", evt)
				}
			default:
				t.Fatal("no event recorded")
			}
		})
	}

	// state=Failed forces Warning even for a reason not in the list.
	t.Run("state Failed forces Warning", func(t *testing.T) {
		fr := record.NewFakeRecorder(4)
		r := &FirmwarePolicyReconciler{Recorder: fr}
		r.recordNodeStateEvent(cr, "e01cs03", firmwarev1alpha1.NodeStateFailed, ReasonTransientAPIError, "")
		select {
		case evt := <-fr.Events:
			if !strings.HasPrefix(evt, "Warning ") {
				t.Errorf("event type = %q; want Warning prefix", evt)
			}
		default:
			t.Fatal("no event recorded")
		}
	})

	t.Run("nil Recorder is a no-op (no NPE)", func(t *testing.T) {
		r := &FirmwarePolicyReconciler{}
		r.recordNodeStateEvent(cr, "e01cs04", firmwarev1alpha1.NodeStateDone, ReasonFlashSucceeded, "")
	})

	t.Run("empty reason is a no-op", func(t *testing.T) {
		fr := record.NewFakeRecorder(4)
		r := &FirmwarePolicyReconciler{Recorder: fr}
		r.recordNodeStateEvent(cr, "e01cs05", firmwarev1alpha1.NodeStatePending, "", "")
		select {
		case evt := <-fr.Events:
			t.Errorf("unexpected event for empty reason: %q", evt)
		default:
		}
	})
}

// A reconcile that moves a node into Flashing must publish the reason
// and fire exactly one event; a second reconcile that observes the same
// (state, reason) must fire nothing more and keep the original
// LastTransitionTime.
func TestReconcile_EmitsEventOncePerTransition(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	disable := false
	cr := newCR()
	cr.Spec.UpgradePolicy.Drain.Enable = &disable
	n := node("worker-0", nil)

	fr := record.NewFakeRecorder(16)
	r := &FirmwarePolicyReconciler{
		Scheme:   testScheme(t),
		Recorder: fr,
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(cr, toClientObject(n)).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	var got firmwarev1alpha1.TenstorrentFirmwarePolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &got); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	if len(got.Status.Nodes) != 1 {
		t.Fatalf("expected 1 node status, got %+v", got.Status.Nodes)
	}
	if got.Status.Nodes[0].Reason != ReasonFlashing {
		t.Errorf("reason = %q; want %q", got.Status.Nodes[0].Reason, ReasonFlashing)
	}
	firstTransition := got.Status.Nodes[0].LastTransitionTime

	var events []string
	for done := false; !done; {
		select {
		case e := <-fr.Events:
			events = append(events, e)
		default:
			done = true
		}
	}
	if len(events) != 1 || !strings.Contains(events[0], ReasonFlashing) {
		t.Fatalf("expected exactly one %s event, got %v", ReasonFlashing, events)
	}

	// Second reconcile: same observed state, so no new event and the
	// timestamp is preserved rather than rewritten.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	select {
	case e := <-fr.Events:
		t.Errorf("unexpected second event for an unchanged state: %q", e)
	default:
	}

	if err := r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &got); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	if !got.Status.Nodes[0].LastTransitionTime.Equal(&firstTransition) {
		t.Errorf("LastTransitionTime rewritten on a steady-state reconcile: %v → %v",
			firstTransition, got.Status.Nodes[0].LastTransitionTime)
	}
}

// The three policy knobs that park an otherwise-ready node each get
// their own reason — without them, "Pending" can't be told apart from
// "queued behind maxParallel".
func TestReconcile_IdleReasons(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	disable := false

	cases := []struct {
		name       string
		mutate     func(*firmwarev1alpha1.TenstorrentFirmwarePolicy)
		wantReason string
	}{
		{
			name:       "paused",
			mutate:     func(cr *firmwarev1alpha1.TenstorrentFirmwarePolicy) { cr.Spec.Paused = true },
			wantReason: ReasonPaused,
		},
		{
			name: "autoUpgrade disabled",
			mutate: func(cr *firmwarev1alpha1.TenstorrentFirmwarePolicy) {
				cr.Spec.UpgradePolicy.AutoUpgrade = &disable
			},
			wantReason: ReasonAutoUpgradeDisabled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := newCR()
			cr.Spec.UpgradePolicy.Drain.Enable = &disable
			tc.mutate(cr)

			r := &FirmwarePolicyReconciler{
				Scheme: testScheme(t),
				Client: fake.NewClientBuilder().WithScheme(testScheme(t)).
					WithObjects(cr, toClientObject(node("worker-0", nil))).
					WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
					Build(),
			}
			if _, err := r.Reconcile(context.Background(),
				ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			var got firmwarev1alpha1.TenstorrentFirmwarePolicy
			_ = r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &got)
			if len(got.Status.Nodes) != 1 {
				t.Fatalf("expected 1 node status, got %+v", got.Status.Nodes)
			}
			if got.Status.Nodes[0].State != firmwarev1alpha1.NodeStatePending {
				t.Errorf("state = %q; want Pending", got.Status.Nodes[0].State)
			}
			if got.Status.Nodes[0].Reason != tc.wantReason {
				t.Errorf("reason = %q; want %q", got.Status.Nodes[0].Reason, tc.wantReason)
			}
		})
	}
}

// haltOnFailure parks the healthy nodes behind a failed one; they must
// say RolloutHalted rather than sitting in a bare Pending.
func TestReconcile_RolloutHaltedReason(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	disable := false
	cr := newCR()
	cr.Spec.UpgradePolicy.Drain.Enable = &disable
	cr.Spec.UpgradePolicy.MaxParallel = 2

	// worker-0's Job already failed; worker-1 has no Job at all.
	r := &FirmwarePolicyReconciler{
		Scheme: testScheme(t),
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(cr,
				toClientObject(node("worker-0", nil)),
				toClientObject(node("worker-1", nil)),
				flashJob(cr, "worker-0", jobCond(batchv1.JobFailed)),
			).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got firmwarev1alpha1.TenstorrentFirmwarePolicy
	_ = r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &got)

	byNode := map[string]firmwarev1alpha1.NodeStatus{}
	for _, ns := range got.Status.Nodes {
		byNode[ns.Name] = ns
	}
	if r := byNode["worker-0"].Reason; r != ReasonFlashJobFailed {
		t.Errorf("worker-0 reason = %q; want %q", r, ReasonFlashJobFailed)
	}
	if r := byNode["worker-1"].Reason; r != ReasonRolloutHalted {
		t.Errorf("worker-1 reason = %q; want %q", r, ReasonRolloutHalted)
	}
}

// A node claimed by another CR is reported, never touched — and the
// reason says which of the two "we won't act" cases it is.
func TestReconcile_NodeConflictReason(t *testing.T) {
	t.Setenv("REQUIRE_TT_PCI_LABEL", "false")
	t.Setenv("OPERATOR_NAMESPACE", "tt-operator-system")

	disable := false
	cr := newCR()
	cr.Spec.UpgradePolicy.Drain.Enable = &disable

	owned := node("worker-0", map[string]string{LabelOwnerCR: "some-other-cr"})

	fr := record.NewFakeRecorder(8)
	r := &FirmwarePolicyReconciler{
		Scheme:   testScheme(t),
		Recorder: fr,
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(cr, toClientObject(owned)).
			WithStatusSubresource(&firmwarev1alpha1.TenstorrentFirmwarePolicy{}).
			Build(),
	}
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got firmwarev1alpha1.TenstorrentFirmwarePolicy
	_ = r.Get(context.Background(), types.NamespacedName{Name: cr.Name}, &got)
	if len(got.Status.Nodes) != 1 || got.Status.Nodes[0].Reason != ReasonNodeConflict {
		t.Fatalf("expected NodeConflict reason, got %+v", got.Status.Nodes)
	}

	select {
	case evt := <-fr.Events:
		if !strings.HasPrefix(evt, "Warning ") || !strings.Contains(evt, ReasonNodeConflict) {
			t.Errorf("event = %q; want a Warning naming %s", evt, ReasonNodeConflict)
		}
	default:
		t.Error("no conflict event recorded")
	}

	// And no Job was created for a node we don't own.
	var jobs batchv1.JobList
	_ = r.List(context.Background(), &jobs)
	if len(jobs.Items) != 0 {
		t.Errorf("expected no Jobs for a conflicted node, got %d", len(jobs.Items))
	}
}
