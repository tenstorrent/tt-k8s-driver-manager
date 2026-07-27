## Requirements

Kubernetes: `>=1.27.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| controller | object | `{"deployGates":["tenstorrent.com/deploy.tt-telemetry"],"extraEnv":[],"image":{"pullPolicy":"IfNotPresent","repository":"ghcr.io/tenstorrent/tt-k8s-driver-manager","tag":""},"leaderElection":false,"replicas":1,"requireTenstorrentLabel":"true","resources":{"limits":{"cpu":"500m","memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}}` | Controller Deployment settings (image, replicas, RBAC gates, env, resources). |
| controller.deployGates | list | `["tenstorrent.com/deploy.tt-telemetry"]` | Node-label keys the controller flips to "false" during a kmd upgrade to drain sibling DaemonSets that hold /dev/tenstorrent. Each listed key is set to "false" on every matched node BEFORE the DaemonSet template is bumped (sibling DS's nodeAffinity stops matching → DS controller deletes the pod → FDs close → refcnt drops). Labels are REMOVED (not flipped back to "true") once the new builder pod is Ready — the chart's NotIn ["false"] semantic schedules the DS back when the label is absent. Unset (key missing entirely) → controller uses its compiled-in default (today: tenstorrent.com/deploy.tt-telemetry). An explicit empty list disables the gate-flip step entirely. |
| controller.extraEnv | list | `[]` | Extra env vars appended to the controller and propagated to spawned builder pods; escape hatch for HTTPS_PROXY/HTTP_PROXY/NO_PROXY in proxied clusters where the builder must git-clone tt-kmd. |
| controller.image.pullPolicy | string | `"IfNotPresent"` | Controller image pull policy. |
| controller.image.repository | string | `"ghcr.io/tenstorrent/tt-k8s-driver-manager"` | Controller image repository. |
| controller.image.tag | string | `""` | Controller image tag; falls back to .Chart.AppVersion when empty. |
| controller.leaderElection | bool | `false` | Enable leader election on the controller manager. |
| controller.replicas | int | `1` | Controller Deployment replica count. |
| controller.requireTenstorrentLabel | string | `"true"` | Require nodes to carry the Tenstorrent NFD PCI label before reconciling. |
| controller.resources | object | `{"limits":{"cpu":"500m","memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | Controller container resource requests/limits. |
| driver | object | `{"image":{"pullPolicy":"IfNotPresent","repository":"ghcr.io/tenstorrent/tt-k8s-driver-manager-builder","tag":""}}` | Image the controller spawns as the per-node builder DaemonSet pod (builds tt-kmd against host headers, insmods into the host kernel). |
| driver.image.pullPolicy | string | `"IfNotPresent"` | Builder image pull policy (reserved; per-CR policy set on the CR). |
| driver.image.repository | string | `"ghcr.io/tenstorrent/tt-k8s-driver-manager-builder"` | Builder image repository. |
| driver.image.tag | string | `""` | Builder image tag; falls back to .Chart.AppVersion when empty. |
| flasher | object | `{"image":{"pullPolicy":"IfNotPresent","repository":"ghcr.io/tenstorrent/tt-k8s-driver-manager-flasher","tag":""}}` | Image the controller spawns as the per-node firmware flash Job (bundles tt-flash + tt-smi, driven by TenstorrentFirmwarePolicy). |
| flasher.image.pullPolicy | string | `"IfNotPresent"` | Flasher image pull policy (reserved; per-CR policy set on the CR). |
| flasher.image.repository | string | `"ghcr.io/tenstorrent/tt-k8s-driver-manager-flasher"` | Flasher image repository. |
| flasher.image.tag | string | `""` | Flasher image tag; falls back to .Chart.AppVersion when empty. |
| imagePullSecrets | list | `[]` | Pull secrets for the controller + installer ServiceAccounts; inherited by spawned builder/flasher pods. Leave empty for public images. |
| metrics | object | `{"port":8080,"service":{"enabled":true},"serviceMonitor":{"additionalLabels":{},"enabled":false,"interval":"30s","metricRelabelings":[],"relabelings":[],"scrapeTimeout":"10s"}}` | Prometheus metrics. The controller always serves /metrics on metrics.port; these knobs only control how Prometheus finds it. Exported families are documented in docs/metrics.md — controller-runtime's built-ins plus ttdriver_* / ttfw_* for driver and firmware rollouts. |
| metrics.port | int | `8080` | Port the controller serves /metrics on (container port and Service port). |
| metrics.service.enabled | bool | `true` | Create a ClusterIP Service in front of the controller's metrics port. Needed by the ServiceMonitor below, and by pod-annotation-based scrapers. |
| metrics.serviceMonitor.additionalLabels | object | `{}` | Extra labels on the ServiceMonitor. Set whatever your Prometheus' serviceMonitorSelector matches (kube-prometheus-stack commonly wants `release: <prometheus release name>`), or it is silently ignored. |
| metrics.serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator ServiceMonitor. Off by default: it needs the monitoring.coreos.com CRD, and the release fails on clusters without one. |
| metrics.serviceMonitor.interval | string | `"30s"` | Scrape interval. The fleet-wide version gauges only move when a rollout does, so there is no reason to scrape aggressively. |
| metrics.serviceMonitor.metricRelabelings | list | `[]` | Prometheus metric_relabel_configs applied to scraped samples; the place to drop families you don't want to store. |
| metrics.serviceMonitor.relabelings | list | `[]` | Prometheus relabel_configs applied to the scrape target. |
| metrics.serviceMonitor.scrapeTimeout | string | `"10s"` | Scrape timeout; must not exceed the interval. |
