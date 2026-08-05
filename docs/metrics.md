# Metrics

The controller serves Prometheus metrics on `:8080/metrics` (set by
`--metrics-bind-address`; the chart wires it from `metrics.port`). Two groups
land there: controller-runtime's built-ins, and the `ttdriver_*` / `ttfw_*`
families this operator adds.

## Scraping

The chart ships a ClusterIP Service in front of the endpoint
(`metrics.service.enabled`, on by default) and an optional ServiceMonitor:

```bash
helm upgrade --install tt-k8s-driver-manager ... \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.serviceMonitor.additionalLabels.release=kube-prometheus-stack
```

`additionalLabels` has to match your Prometheus' `serviceMonitorSelector` or
the ServiceMonitor is created and silently never scraped. The
ServiceMonitor needs the `monitoring.coreos.com` CRD — leave it off on
clusters without the Prometheus Operator and scrape the Service directly.

To eyeball the endpoint without Prometheus:

```bash
kubectl -n tt-k8s-driver-manager-system port-forward \
  svc/tt-k8s-driver-manager-metrics 8080:8080
curl -s localhost:8080/metrics | grep -E '^tt(driver|fw)_'
```

## Driver metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `ttdriver_nodes_by_kmd_version` | gauge | `version`, `install_mode` | Nodes carrying `driver.tenstorrent.com/kmd-version`. Fleet-wide, not per policy. |
| `ttdriver_policy_nodes` | gauge | `cr`, `state` | Matched nodes per policy, broken down by `status.nodes[].state`. |
| `ttdriver_policy_desired_version` | gauge | `cr`, `version` | Always 1; the label carries the policy's `spec.version`. |
| `ttdriver_daemonset_operations_total` | counter | `cr`, `operation`, `result` | Installer-DaemonSet `create` / `update` attempts, by `success` / `error`. |
| `ttdriver_pods_evicted_total` | counter | `cr`, `pass` | Pods evicted during pre-upgrade drain (`pass1` = declared device users, `pass2` = full-node drain). |
| `ttdriver_drain_blocked_total` | counter | `cr`, `pass` | Evictions a PodDisruptionBudget refused. |
| `ttdriver_errors_total` | counter | `cr`, `stage` | Errors by reconcile stage, including the best-effort steps the reconciler logs and swallows. |
| `ttdriver_last_successful_reconcile_timestamp_seconds` | gauge | `cr` | Unix time of the last reconcile that returned no error. |

`install_mode` is `container` (the operator built and loaded the module),
`host` (a pre-existing DKMS/apt install; the operator stands down) or
`unknown`.

## Firmware metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `ttfw_flash_duration_seconds` | histogram | `cr`, `result` | Per-node flash Job wall time, from the Job's own timestamps. Buckets 10s→~21m. |
| `ttfw_flash_jobs_total` | counter | `cr`, `result` | Flash Jobs that reached a terminal state (`success` / `failed`). |
| `ttfw_flash_jobs_created_total` | counter | `cr`, `result` | Flash Job creation attempts. |
| `ttfw_flash_jobs_in_flight` | gauge | `cr` | Flash Jobs not yet terminal — compare against `spec.upgradePolicy.maxParallel`. |
| `ttfw_drain_blocked_total` | counter | `cr`, `reason` | `pdb` (an eviction the API server refused) or `drain_timeout` (a node that outlived `drain.timeoutSeconds`). |
| `ttfw_pods_evicted_total` | counter | `cr` | Pods evicted to free a node for flashing. |
| `ttfw_nodes_by_fw_version` | gauge | `version` | Nodes carrying `firmware.tenstorrent.com/fw-version`. Fleet-wide. |
| `ttfw_policy_nodes` | gauge | `cr`, `state` | Matched nodes per policy, by `status.nodes[].state`. |
| `ttfw_policy_desired_version` | gauge | `cr`, `version` | Always 1; the label carries the policy's `spec.version`. |
| `ttfw_errors_total` | counter | `cr`, `stage` | Errors by reconcile stage. |
| `ttfw_last_successful_reconcile_timestamp_seconds` | gauge | `cr` | Unix time of the last reconcile that returned no error. |

## Built-ins worth knowing

From controller-runtime and client-go, per controller (`TenstorrentDriverPolicy`,
`TenstorrentFirmwarePolicy`):

- `controller_runtime_reconcile_total{controller,result}`,
  `controller_runtime_reconcile_errors_total`,
  `controller_runtime_reconcile_time_seconds`
- `workqueue_depth{name}`, `workqueue_adds_total`, `workqueue_retries_total`
- `rest_client_requests_total`, `rest_client_request_duration_seconds`
- the usual `go_*` / `process_*` runtime families

The `ttdriver_errors_total` / `ttfw_errors_total` pair is deliberately
finer-grained than `controller_runtime_reconcile_errors_total`: it names the
stage that failed, and it also counts the best-effort steps (drain, label
sync, uncordon) that the reconcilers log without failing the reconcile — so
those never show up in the built-in error counter at all.

## Cardinality

Every label is bounded by cluster *configuration*, not cluster *size*: a
handful of policy names, the versions in play, the node-state enum, and small
closed sets for `result` / `reason` / `operation` / `pass` / `install_mode`.

There is no node, pod, Job or device label anywhere, and never an error
string. Node-level detail lives in `status.nodes[]` on the policy and in node
labels — reach for `kubectl` or kube-state-metrics when you need to slice by
node:

```bash
kubectl get nodes -L driver.tenstorrent.com/kmd-version \
                  -L firmware.tenstorrent.com/fw-version \
                  -L firmware.tenstorrent.com/upgrade-state
```

## Useful queries

Nodes not yet on the version their policy wants:

```promql
sum(ttdriver_policy_nodes{state!="Done"}) by (cr)
```

Flash failure rate over the last hour:

```promql
sum(rate(ttfw_flash_jobs_total{result="failed"}[1h])) by (cr)
  / sum(rate(ttfw_flash_jobs_total[1h])) by (cr)
```

95th-percentile flash time:

```promql
histogram_quantile(0.95, sum(rate(ttfw_flash_duration_seconds_bucket[6h])) by (le, cr))
```

A controller that is up but wedged — reconciles running, nothing
progressing:

```promql
time() - ttdriver_last_successful_reconcile_timestamp_seconds > 300
```

Fleet version spread, for spotting a stalled rollout:

```promql
ttdriver_nodes_by_kmd_version
ttfw_nodes_by_fw_version
```
