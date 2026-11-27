## Requirements

Kubernetes: `>=1.27.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| controller | object | `{"affinity":{"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"preference":{"matchExpressions":[{"key":"feature.node.kubernetes.io/pci-1200_1e52.present","operator":"DoesNotExist"}]},"weight":100}]}},"deployGates":["tenstorrent.com/deploy.tt-telemetry"],"extraEnv":[],"image":{"pullPolicy":"IfNotPresent","repository":"ghcr.io/tenstorrent/tt-k8s-driver-manager","tag":""},"leaderElection":false,"nodeSelector":{},"replicas":1,"requireTenstorrentLabel":"true","resources":{"limits":{"cpu":"500m","memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}},"tolerations":[]}` | Controller Deployment settings (image, replicas, RBAC gates, env, resources). |
| controller.affinity | object | `{"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"preference":{"matchExpressions":[{"key":"feature.node.kubernetes.io/pci-1200_1e52.present","operator":"DoesNotExist"}]},"weight":100}]}}` | Controller pod affinity. Defaults to preferring nodes without the NFD Tenstorrent PCI-presence label, keeping the controller off accelerator nodes. NFD omits the label on non-TT nodes, hence DoesNotExist. Preferred rather than required so single-node dev clusters still schedule it. |
| controller.deployGates | list | `["tenstorrent.com/deploy.tt-telemetry"]` | Node-label keys the controller flips to "false" during a kmd upgrade to drain sibling DaemonSets that hold /dev/tenstorrent. Each listed key is set to "false" on every matched node BEFORE the DaemonSet template is bumped (sibling DS's nodeAffinity stops matching → DS controller deletes the pod → FDs close → refcnt drops). Labels are REMOVED (not flipped back to "true") once the new builder pod is Ready — the chart's NotIn ["false"] semantic schedules the DS back when the label is absent. Unset (key missing entirely) → controller uses its compiled-in default (today: tenstorrent.com/deploy.tt-telemetry). An explicit empty list disables the gate-flip step entirely. |
| controller.extraEnv | list | `[]` | Extra env vars appended to the controller and propagated to spawned builder pods; escape hatch for HTTPS_PROXY/HTTP_PROXY/NO_PROXY in proxied clusters where the builder must git-clone tt-kmd. |
| controller.image.pullPolicy | string | `"IfNotPresent"` | Controller image pull policy. |
| controller.image.repository | string | `"ghcr.io/tenstorrent/tt-k8s-driver-manager"` | Controller image repository. |
| controller.image.tag | string | `""` | Controller image tag; falls back to .Chart.AppVersion when empty. |
| controller.leaderElection | bool | `false` | Enable leader election on the controller manager. |
| controller.nodeSelector | object | `{}` | Controller pod nodeSelector. |
| controller.replicas | int | `1` | Controller Deployment replica count. |
| controller.requireTenstorrentLabel | string | `"true"` | Require nodes to carry the Tenstorrent NFD PCI label before reconciling. |
| controller.resources | object | `{"limits":{"cpu":"500m","memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | Controller container resource requests/limits. |
| controller.tolerations | list | `[]` | Controller pod tolerations. |
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
| vfioManager | object | `{"affinity":{},"bindInterval":"30s","devices":[],"enabled":false,"extraArgs":[],"image":{"pullPolicy":"IfNotPresent","repository":"ghcr.io/tenstorrent/tt-k8s-driver-manager-vfio","tag":""},"metricsPort":9401,"nodeSelector":{},"resources":{},"restoreOnExit":false,"tolerations":[{"effect":"NoSchedule","operator":"Exists"}]}` | Privileged DaemonSet that binds Tenstorrent PCI devices to vfio-pci for passthrough into VMs. Off by default: binding a device to vfio-pci takes it away from tt-kmd, so container workloads on that node lose it. Enable only on nodes dedicated to VM passthrough, and scope them with nodeSelector.  Whatever advertises these devices to the kubelet must be configured with the same device list — this component only performs the binding. |
| vfioManager.affinity | object | `{}` | vfio-manage pod affinity. |
| vfioManager.bindInterval | string | `"30s"` | How often to re-assert the binding. Each pass is a sysfs scan, so this is cheap; it exists to recover devices that drift off vfio-pci after a driver reload without waiting for a pod restart. |
| vfioManager.devices | list | `[]` | PCI devices to bind. Empty means the DaemonSet runs and binds nothing.  Example:   devices:     - resourceName: tenstorrent.com/wormhole       vendorId: "1e52"       deviceId: ["401e"] |
| vfioManager.enabled | bool | `false` | Deploy the vfio-manage DaemonSet. |
| vfioManager.extraArgs | list | `[]` | Extra arguments appended to the vfio-manage command line. |
| vfioManager.image.pullPolicy | string | `"IfNotPresent"` | vfio-manage image pull policy. |
| vfioManager.image.repository | string | `"ghcr.io/tenstorrent/tt-k8s-driver-manager-vfio"` | vfio-manage image repository. |
| vfioManager.image.tag | string | `""` | vfio-manage image tag; falls back to .Chart.AppVersion when empty. |
| vfioManager.metricsPort | int | `9401` | Port for the Prometheus /metrics and /healthz endpoints (0 disables both; the liveness probe is dropped with them). |
| vfioManager.nodeSelector | object | `{}` | Restrict the DaemonSet to nodes set aside for VM passthrough. |
| vfioManager.resources | object | `{}` | vfio-manage container resource requests/limits. |
| vfioManager.restoreOnExit | bool | `false` | On SIGTERM, re-bind devices to the driver they were on beforehand. Off by default: in production the DaemonSet restarts and re-asserts, and handing devices back mid-roll only churns the driver. |
| vfioManager.tolerations | list | `[{"effect":"NoSchedule","operator":"Exists"}]` | Defaults to tolerating every NoSchedule taint, matching how accelerator nodes are usually cordoned off. |
