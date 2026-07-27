// Package metrics holds the Prometheus metrics tt-k8s-driver-manager
// exports next to controller-runtime's built-ins (controller_runtime_*,
// workqueue_*, rest_client_*, go_*, process_*).
//
// Everything here registers into controller-runtime's registry
// (sigs.k8s.io/controller-runtime/pkg/metrics.Registry) — that registry is
// what the manager serves on --metrics-bind-address. Using promauto's
// package-level constructors instead would register into the default
// prometheus registry, compile fine, and export nothing.
//
// # Cardinality
//
// Every label below is bounded by cluster *configuration*, never by cluster
// *size*:
//
//	cr             one value per TenstorrentDriverPolicy /
//	               TenstorrentFirmwarePolicy — a handful, cluster-wide
//	version        the kmd / firmware versions actually in play
//	state          the CR's node-state enum
//	install_mode   container | host | unknown
//	result, reason, operation, pass
//	               small closed sets, defined as constants below
//
// Deliberately absent: node names, pod names, Job names, PCI addresses and
// error strings. The controller is a single cluster-scoped Deployment, so
// the scrape target already identifies it; a node dimension here would
// multiply every series by the fleet size for no operational gain. Per-node
// detail lives in the CR's status.nodes array and in node labels, both of
// which kube-state-metrics can expose for anyone who needs to slice by node.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Bounded label values. Anything written into a `result`, `reason`,
// `operation`, `pass` or `install_mode` label comes from this list.
const (
	// ResultSuccess / ResultError describe an operation the controller
	// performed itself (an API call, a Job creation).
	ResultSuccess = "success"
	ResultError   = "error"

	// ResultFailed is the terminal outcome of work the controller
	// delegated to a Job or pod. Successful delegated work reports
	// ResultSuccess.
	ResultFailed = "failed"

	OperationCreate = "create"
	OperationUpdate = "update"

	// ReasonPDB is an eviction the API server refused because of a
	// PodDisruptionBudget; ReasonDrainTimeout is a node that sat in
	// Draining past spec.upgradePolicy.drain.timeoutSeconds.
	ReasonPDB          = "pdb"
	ReasonDrainTimeout = "drain_timeout"

	// DrainPass1 evicts pods that declare /dev/tenstorrent via hostPath;
	// DrainPass2 is the full-node kubectl-drain-equivalent pass.
	DrainPass1 = "pass1"
	DrainPass2 = "pass2"

	// InstallModeUnknown stands in for nodes with a kmd-version label but
	// no install-mode label (builder pods older than the label, or a
	// hand-loaded module).
	InstallModeUnknown = "unknown"
)

// Driver-side metrics (TenstorrentDriverPolicy / tt-kmd).
var (
	// DriverNodesByKMDVersion is cluster-wide, not per-CR: it counts every
	// node carrying driver.tenstorrent.com/kmd-version, which is written
	// only once the node's builder pod reports the module actually loaded.
	// This is the "what is really running out there" metric — compare
	// against DriverPolicyDesiredVersion to spot a stalled rollout.
	DriverNodesByKMDVersion = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "ttdriver_nodes_by_kmd_version",
		Help: "Number of nodes labeled with this kmd-version.",
	}, []string{"version", "install_mode"})

	// DriverPolicyNodes is the per-CR rollout breakdown, mirroring
	// status.nodes[].state. Sum over states == matched nodes.
	DriverPolicyNodes = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "ttdriver_policy_nodes",
		Help: "Nodes matched by a TenstorrentDriverPolicy, by per-node upgrade state.",
	}, []string{"cr", "state"})

	// DriverPolicyDesiredVersion is an info-style gauge: always 1, the
	// payload is the version label. Only one series per CR at a time.
	DriverPolicyDesiredVersion = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "ttdriver_policy_desired_version",
		Help: "Always 1; the version label carries the TenstorrentDriverPolicy's spec.version.",
	}, []string{"cr", "version"})

	// DriverDaemonSetOperations counts the installer-DaemonSet writes the
	// controller issues — a create/update spike is a rollout, a sustained
	// result=error is a broken one.
	DriverDaemonSetOperations = promauto.With(ctrlmetrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "ttdriver_daemonset_operations_total",
		Help: "Installer DaemonSet create/update attempts by result.",
	}, []string{"cr", "operation", "result"})

	DriverPodsEvictedTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "ttdriver_pods_evicted_total",
		Help: "Pods evicted during pre-upgrade drain, by drain pass.",
	}, []string{"cr", "pass"})

	DriverDrainBlockedTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "ttdriver_drain_blocked_total",
		Help: "Pre-upgrade evictions refused by a PodDisruptionBudget, by drain pass.",
	}, []string{"cr", "pass"})

	// DriverErrorsTotal is finer-grained than
	// controller_runtime_reconcile_errors_total, which can't say *which*
	// step failed — and which never sees the best-effort steps (drain,
	// label sync, uncordon) the driver reconciler logs and swallows.
	DriverErrorsTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "ttdriver_errors_total",
		Help: "Errors during driver reconciliation, by reconcile stage.",
	}, []string{"cr", "stage"})

	DriverLastReconcileTimestamp = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "ttdriver_last_successful_reconcile_timestamp_seconds",
		Help: "Unix timestamp of the last reconcile of this TenstorrentDriverPolicy that completed without returning an error.",
	}, []string{"cr"})
)

// Firmware-side metrics (TenstorrentFirmwarePolicy / tt-flash).
var (
	// FirmwareFlashDuration is per-node flash Job wall time, recorded once
	// per Job when it reaches a terminal condition. Buckets span 10s..~21m
	// because a flash is minutes, and the ActiveDeadlineSeconds default is
	// 900s.
	FirmwareFlashDuration = promauto.With(ctrlmetrics.Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ttfw_flash_duration_seconds",
		Help:    "Per-node firmware flash job wall time.",
		Buckets: prometheus.ExponentialBuckets(10, 2, 8),
	}, []string{"cr", "result"})

	// FirmwareFlashJobsTotal counts terminal flash Jobs. Same dedup as the
	// histogram, so _count and this agree.
	FirmwareFlashJobsTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "ttfw_flash_jobs_total",
		Help: "Firmware flash Jobs that reached a terminal state, by result.",
	}, []string{"cr", "result"})

	// FirmwareFlashJobsCreatedTotal counts Create calls, not outcomes —
	// the gap between this and FirmwareFlashJobsTotal is work in flight or
	// Jobs that vanished (TTL, CR delete).
	FirmwareFlashJobsCreatedTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "ttfw_flash_jobs_created_total",
		Help: "Firmware flash Job creation attempts by result.",
	}, []string{"cr", "result"})

	FirmwareFlashJobsInFlight = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "ttfw_flash_jobs_in_flight",
		Help: "Firmware flash Jobs owned by this CR that have not reached a terminal state.",
	}, []string{"cr"})

	// FirmwareDrainBlockedTotal covers both ways a drain stalls: an
	// eviction the API server refuses (reason=pdb, once per refused
	// attempt) and a node that outlives its drain deadline
	// (reason=drain_timeout, once per node per stall — see EventDedup).
	FirmwareDrainBlockedTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "ttfw_drain_blocked_total",
		Help: "Drain attempts that hit a PDB / drain-timeout.",
	}, []string{"cr", "reason"})

	FirmwarePodsEvictedTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "ttfw_pods_evicted_total",
		Help: "Pods evicted to free a node for firmware flashing.",
	}, []string{"cr"})

	// FirmwareNodesByFWVersion is cluster-wide, sourced from the
	// firmware.tenstorrent.com/fw-version node label the controller writes
	// after a verified readback.
	FirmwareNodesByFWVersion = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "ttfw_nodes_by_fw_version",
		Help: "Number of nodes labeled with this firmware version.",
	}, []string{"version"})

	FirmwarePolicyNodes = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "ttfw_policy_nodes",
		Help: "Nodes matched by a TenstorrentFirmwarePolicy, by per-node upgrade state.",
	}, []string{"cr", "state"})

	FirmwarePolicyDesiredVersion = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "ttfw_policy_desired_version",
		Help: "Always 1; the version label carries the TenstorrentFirmwarePolicy's spec.version.",
	}, []string{"cr", "version"})

	FirmwareErrorsTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "ttfw_errors_total",
		Help: "Errors during firmware reconciliation, by reconcile stage.",
	}, []string{"cr", "stage"})

	FirmwareLastReconcileTimestamp = promauto.With(ctrlmetrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "ttfw_last_successful_reconcile_timestamp_seconds",
		Help: "Unix timestamp of the last reconcile of this TenstorrentFirmwarePolicy that completed without returning an error.",
	}, []string{"cr"})
)

// VersionMode keys the cluster-wide kmd-version gauge. Struct rather than a
// joined string so callers can't accidentally swap the two dimensions.
type VersionMode struct {
	Version     string
	InstallMode string
}

// gaugeMu serializes the Reset+Set pairs below. Reconciles are serialized
// per controller today (MaxConcurrentReconciles defaults to 1), but the
// driver and firmware controllers run concurrently and a future bump of
// either would otherwise race.
var gaugeMu sync.Mutex

// SetNodesByKMDVersion replaces the whole ttdriver_nodes_by_kmd_version
// family. Reset-then-Set (rather than incremental Set) is what drops series
// for versions that no longer exist anywhere in the fleet — otherwise an
// upgraded-away version would sit at its last count forever.
func SetNodesByKMDVersion(counts map[VersionMode]int) {
	gaugeMu.Lock()
	defer gaugeMu.Unlock()
	DriverNodesByKMDVersion.Reset()
	for k, n := range counts {
		mode := k.InstallMode
		if mode == "" {
			mode = InstallModeUnknown
		}
		DriverNodesByKMDVersion.WithLabelValues(k.Version, mode).Set(float64(n))
	}
}

// SetNodesByFWVersion replaces the whole ttfw_nodes_by_fw_version family.
func SetNodesByFWVersion(counts map[string]int) {
	gaugeMu.Lock()
	defer gaugeMu.Unlock()
	FirmwareNodesByFWVersion.Reset()
	for v, n := range counts {
		FirmwareNodesByFWVersion.WithLabelValues(v).Set(float64(n))
	}
}

// SetDriverPolicyNodes publishes one series per state for this CR. Callers
// pass every state — including the zeros — so a state that empties out
// reads as 0 instead of disappearing mid-graph.
func SetDriverPolicyNodes(cr string, counts map[string]int) {
	for state, n := range counts {
		DriverPolicyNodes.WithLabelValues(cr, state).Set(float64(n))
	}
}

// SetFirmwarePolicyNodes is SetDriverPolicyNodes for the firmware side.
func SetFirmwarePolicyNodes(cr string, counts map[string]int) {
	for state, n := range counts {
		FirmwarePolicyNodes.WithLabelValues(cr, state).Set(float64(n))
	}
}

// SetDriverDesiredVersion points the CR's info gauge at version, dropping
// the series for whatever version it targeted before.
func SetDriverDesiredVersion(cr, version string) {
	gaugeMu.Lock()
	defer gaugeMu.Unlock()
	DriverPolicyDesiredVersion.DeletePartialMatch(prometheus.Labels{"cr": cr})
	DriverPolicyDesiredVersion.WithLabelValues(cr, version).Set(1)
}

// SetFirmwareDesiredVersion is SetDriverDesiredVersion for the firmware side.
func SetFirmwareDesiredVersion(cr, version string) {
	gaugeMu.Lock()
	defer gaugeMu.Unlock()
	FirmwarePolicyDesiredVersion.DeletePartialMatch(prometheus.Labels{"cr": cr})
	FirmwarePolicyDesiredVersion.WithLabelValues(cr, version).Set(1)
}

// DriverReconcileSucceeded stamps the CR's freshness gauge. Alert on
// `time() - ttdriver_last_successful_reconcile_timestamp_seconds` to catch a
// controller that is up but wedged — which reconcile_total can't show,
// since a hot error loop keeps incrementing it.
func DriverReconcileSucceeded(cr string) {
	DriverLastReconcileTimestamp.WithLabelValues(cr).Set(float64(time.Now().Unix()))
}

// FirmwareReconcileSucceeded is DriverReconcileSucceeded for the firmware side.
func FirmwareReconcileSucceeded(cr string) {
	FirmwareLastReconcileTimestamp.WithLabelValues(cr).Set(float64(time.Now().Unix()))
}

// DeleteDriverPolicySeries drops every per-CR series for a deleted
// TenstorrentDriverPolicy. Without it the gauges would report a rollout
// that no longer exists. Counters go too: a CR recreated under the same
// name is a fresh rollout, and leaving the old totals in place would make
// the increase() look like it went backwards.
func DeleteDriverPolicySeries(cr string) {
	l := prometheus.Labels{"cr": cr}
	DriverPolicyNodes.DeletePartialMatch(l)
	DriverPolicyDesiredVersion.DeletePartialMatch(l)
	DriverDaemonSetOperations.DeletePartialMatch(l)
	DriverPodsEvictedTotal.DeletePartialMatch(l)
	DriverDrainBlockedTotal.DeletePartialMatch(l)
	DriverErrorsTotal.DeletePartialMatch(l)
	DriverLastReconcileTimestamp.DeletePartialMatch(l)
}

// DeleteFirmwarePolicySeries is DeleteDriverPolicySeries for the firmware side.
func DeleteFirmwarePolicySeries(cr string) {
	l := prometheus.Labels{"cr": cr}
	FirmwarePolicyNodes.DeletePartialMatch(l)
	FirmwarePolicyDesiredVersion.DeletePartialMatch(l)
	FirmwareFlashDuration.DeletePartialMatch(l)
	FirmwareFlashJobsTotal.DeletePartialMatch(l)
	FirmwareFlashJobsCreatedTotal.DeletePartialMatch(l)
	FirmwareFlashJobsInFlight.DeletePartialMatch(l)
	FirmwareDrainBlockedTotal.DeletePartialMatch(l)
	FirmwarePodsEvictedTotal.DeletePartialMatch(l)
	FirmwareErrorsTotal.DeletePartialMatch(l)
	FirmwareLastReconcileTimestamp.DeletePartialMatch(l)
}

// EventDedup turns a steady-state observation into a one-shot event.
//
// Both controllers are level-triggered: they re-derive "this Job finished"
// and "this node blew its drain deadline" from cluster state on every
// reconcile, every 30 seconds, for as long as the condition holds. Counting
// those directly would measure reconcile frequency, not events. FirstSeen
// gates the increment; Retain forgets keys whose underlying object or
// condition is gone, so a genuinely new occurrence counts again.
//
// Keys are node and Job names — used as map keys only, never as metric
// label values.
type EventDedup struct {
	mu   sync.Mutex
	seen map[string]map[string]struct{}
}

// NewEventDedup returns a ready-to-use EventDedup.
func NewEventDedup() *EventDedup {
	return &EventDedup{seen: map[string]map[string]struct{}{}}
}

// FirstSeen records (scope, key) and reports whether it was new. Scope is
// the CR name, so Retain and Forget can prune one CR without touching
// another's history.
func (d *EventDedup) FirstSeen(scope, key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	keys, ok := d.seen[scope]
	if !ok {
		keys = map[string]struct{}{}
		d.seen[scope] = keys
	}
	if _, dup := keys[key]; dup {
		return false
	}
	keys[key] = struct{}{}
	return true
}

// Retain drops every key in scope that isn't in keep. Callers pass the keys
// still backed by cluster state, which bounds the map by live objects
// rather than by uptime.
func (d *EventDedup) Retain(scope string, keep []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cur, ok := d.seen[scope]
	if !ok {
		return
	}
	keepSet := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		keepSet[k] = struct{}{}
	}
	for k := range cur {
		if _, live := keepSet[k]; !live {
			delete(cur, k)
		}
	}
	if len(cur) == 0 {
		delete(d.seen, scope)
	}
}

// Forget drops a whole scope — used when its CR is deleted.
func (d *EventDedup) Forget(scope string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.seen, scope)
}
