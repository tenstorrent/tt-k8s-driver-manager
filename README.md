# tt-k8s-driver-manager

## Overview

tt-k8s-driver-manager is a Kubernetes operator that owns the host-side
software stack for Tenstorrent hardware. It installs and upgrades the
`tt-kmd` kernel module, the `tt-smi` command-line tool, and device firmware
on every node in a cluster that has a Tenstorrent device, driven by two
custom resources (CRs):

| Layer | Managed via | Where it lives at runtime |
|---|---|---|
| `tt-kmd` (kernel module) | `TenstorrentDriverPolicy` CR | loaded in host kernel; `.ko` cached at `/var/cache/tt-kmd/<kver>/<v>/` |
| `tt-smi` (userspace CLI) | bundled in builder image, tracks CR | self-contained binary at host `/usr/local/bin/tt-smi` |
| device firmware | `TenstorrentFirmwarePolicy` CR | flashed on-chip via per-node `tt-flash` Job |

There is one declarative CR per concern rather than a single cluster-wide
policy object. `TenstorrentDriverPolicy` is reconciled by a privileged
DaemonSet that builds `tt-kmd` in a container against the host kernel
headers and loads it into the host kernel with `insmod`.
`TenstorrentFirmwarePolicy` is reconciled by a per-node Job that cordons,
drains, flashes, and uncordons each node in turn.

## Getting started

The operator is published as a Helm chart in an OCI registry. For an
all-in-one install (Node Feature Discovery (NFD), the driver manager, and its
custom resource definitions (CRDs)), use the
[tt-operator](https://github.com/tenstorrent/tt-operator) umbrella chart. To
install only the driver manager:

```bash
helm install tt-k8s-driver-manager oci://ghcr.io/tenstorrent/helm/tt-k8s-driver-manager \
  --namespace tt-k8s-driver-manager-system --create-namespace
```

Then apply CRs for the components you want managed. Each CR is independent
— install the driver without flashing firmware, or flash firmware without
managing the driver.

**Driver** (`TenstorrentDriverPolicy`, short name `ttdp`) pins a `tt-kmd`
version on selected nodes. The operator builds and loads it through a per-CR
DaemonSet, cordoning and draining each node before swapping kernel modules:

```yaml
apiVersion: driver.tenstorrent.com/v1alpha1
kind: TenstorrentDriverPolicy
metadata:
  name: default
spec:
  version: "2.8.0"      # required: a tt-kmd release tag minus ttkmd-
  nodeAffinity: {}      # matches all nodes; ANDed with the NFD tt-present label
  upgradePolicy:
    drain:
      enable: true      # cordon + evict /dev/tenstorrent holders before rmmod
      fullNode: true    # also run full kubectl-drain semantics
    forceUnload: false  # set true to SIGKILL processes still holding /dev/tenstorrent before rmmod
```

**Firmware** (`TenstorrentFirmwarePolicy`, short name `ttfwp`) pins a
firmware-bundle version on selected nodes. The operator drives each node
through cordon, drain, flash, and uncordon using a per-node Job:

```yaml
apiVersion: firmware.tenstorrent.com/v1alpha1
kind: TenstorrentFirmwarePolicy
metadata:
  name: default
spec:
  version: "19.9.0"     # required: tt-system-firmware release version
  nodeAffinity: {}      # ANDed with the NFD tt-present label
  upgradePolicy:
    maxParallel: 1      # nodes flashing simultaneously across this CR
    drain:
      enable: true      # cordon + eviction that respects PodDisruptionBudgets before flash
```

See [`docs/driver.md`](docs/driver.md) and [`docs/firmware.md`](docs/firmware.md)
for the full specification: drain policy, `forceUnload`, deploy gates, per-CR
image overrides, skip labels, and multi-pool examples.

After ~1 minute the driver is loaded on every Tenstorrent node, `tt-smi`
is installed at `/usr/local/bin/tt-smi`, and the operator stamps version
labels:

```bash
$ kubectl get nodes -L driver.tenstorrent.com/kmd-version,tt-smi.driver.tenstorrent.com/version,driver.tenstorrent.com/install-mode,firmware.tenstorrent.com/fw-version
NAME      STATUS   KMD-VERSION   VERSION   INSTALL-MODE   FW-VERSION
node-1   Ready    2.8.0         5.2.0     container      19.9.0.0
node-2   Ready    2.8.0         5.2.0     container      19.9.0.0
node-3   Ready    2.8.0         5.2.0     container      19.9.0.0
```

```bash
$ kubectl get ttdp,ttfwp
NAME                                                       VERSION   MATCHED   READY   FAILED
tenstorrentdriverpolicy.driver.tenstorrent.com/default     2.8.0     3         3       0

NAME                                                         VERSION   MATCHED   UPTODATE   INPROGRESS   FAILED
tenstorrentfirmwarepolicy.firmware.tenstorrent.com/default   19.9.0    3         3          0            0
```

## What gets installed where

Once a `TenstorrentDriverPolicy` is reconciled, on each matched node:

- `/var/cache/tt-kmd/<kver>/<version>/tenstorrent.ko`: the operator's
  per-kernel build cache. It survives pod restarts. It is not placed under
  `/lib/modules`; the operator does not run `depmod`, does not write to
  `modules.alias`, and does not compete with any host package.
- `/usr/local/bin/tt-smi`: a self-contained binary (the PyInstaller build
  from tt-smi releases; no Python is needed on the host). It is owned by the
  operator, replaced atomically on each upgrade, and available to anyone who
  logs in to the node.

The kernel module itself is loaded into the running kernel through
`init_module(2)` from the privileged builder pod. Rebooting the host unloads
it, and the builder rebuilds and reloads it on the next boot.

## Mixed environments

If a node already has `tt-kmd` installed through apt or Dynamic Kernel Module
Support (DKMS), for example by a host-side configuration-management tool, the
operator detects this on first reconcile, labels the node
`driver.tenstorrent.com/install-mode=host`, and stands down: no `rmmod`, no
rebuild, and no overwriting of `tt-smi`. The operator still reports the
loaded version through `driver.tenstorrent.com/kmd-version`, so dashboards
and `nodeSelector`-based scheduling work the same way regardless of who owns
the install. See [docs/driver.md](docs/driver.md#mixed-mode) for the full
detection logic.

To opt a node out of all reconciliation, rather than relying on the operator
detecting host ownership, set `driver.tenstorrent.com/skip=true` on the node.

## Docs

| | |
|---|---|
| [Install](docs/install.md) | Prerequisites, NFD, verifying |
| [Drivers](docs/driver.md) | `TenstorrentDriverPolicy`, upgrade flow, drain policy, deploy gates, install-mode, skip label |
| [Firmware](docs/firmware.md) | `TenstorrentFirmwarePolicy`, drain config, force-write flag |
| [Upgrades](docs/upgrades.md) | Rolling-update semantics for tt-kmd, tt-smi, firmware, operator itself |
| [Migrating from DKMS](docs/migrating-from-dkms.md) | Vacate a DKMS install so the operator can take over kmd lifecycle |
| [Metrics](docs/metrics.md) | Prometheus endpoint, ServiceMonitor, exported `ttdriver_*` / `ttfw_*` families |
| [Troubleshooting](docs/troubleshooting.md) | Common failures and how to read the symptoms |

## Related repos

- [tt-operator](https://github.com/tenstorrent/tt-operator): umbrella Helm
  chart bundling the driver manager and Node Feature Discovery
- [tt-kmd](https://github.com/tenstorrent/tt-kmd): the kernel module the
  builder compiles from source
- [tt-smi](https://github.com/tenstorrent/tt-smi): the userspace CLI
- [tt-system-firmware](https://github.com/tenstorrent/tt-system-firmware):
  firmware bundle releases

## Development

### Repo layout

```
api/
  driver/v1alpha1/        TenstorrentDriverPolicy types + DeepCopy generated code
  firmware/v1alpha1/      TenstorrentFirmwarePolicy types
cmd/manager/              main.go — operator entrypoint
internal/controller/
  driver_policy_controller.go   reconcile loop for driver installs + upgrades
  firmware_policy_controller.go reconcile loop for firmware flash jobs
  job.go / labels.go / env.go   shared helpers
  driver_metrics.go / firmware_metrics.go  metric recording off reconcile state
internal/metrics/          Prometheus metric definitions (registered with controller-runtime)
images/driver-build/      Dockerfile + entrypoint for the privileged builder pod
images/flasher/           Dockerfile for the tt-flash job pod
charts/tt-k8s-driver-manager/  Helm chart
config/crd/               generated CRD manifests
config/rbac/              generated RBAC manifests
hack/plugins/             kubectl-tt-driver and kubectl-tt-fw plugins
docs/                     admin-focused guides (install, driver, firmware, upgrades, troubleshooting)
```

### Quickstart

```bash
go build ./...                          # build the manager binary
go test ./... -race -count=1            # run tests
make generate                           # regen CRDs, RBAC, DeepCopy after editing api/
make helm-lint                          # lint the Helm chart
helm template charts/tt-k8s-driver-manager   # render chart locally
```

**Images** (linux/amd64; on arm64 hosts, push a branch and let GitHub Actions build them):

```bash
make controller-image    # ghcr.io/.../tt-k8s-driver-manager:dev
make builder-image       # ghcr.io/.../tt-k8s-driver-manager-builder:dev
make flasher-image       # ghcr.io/.../tt-k8s-driver-manager-flasher:dev
make helm-install        # deploy :dev images to the current kube context
```

**kubectl plugins:**

```bash
make install-plugins     # installs kubectl-tt-driver and kubectl-tt-fw into ~/.local/bin
```

### Where to start

1. `internal/controller/driver_policy_controller.go`: core reconcile loop for driver installs and upgrades.
2. `internal/controller/firmware_policy_controller.go`: the same pattern for firmware flash jobs.
3. `images/driver-build/entrypoint.sh`: what runs in the privileged pod, the kernel module build and `insmod`.

API types (spec fields, status conditions): `api/driver/v1alpha1/` and `api/firmware/v1alpha1/`.

## Contributing

Contributions are welcome. Report bugs and request features through
[GitHub Issues](https://github.com/tenstorrent/tt-k8s-driver-manager/issues),
and submit bug fixes and new functionality as pull requests. Pull requests are
reviewed weekly. See [CONTRIBUTING.md](CONTRIBUTING.md) for the development
workflow and requirements, and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for
community expectations. To report a security vulnerability, follow
[SECURITY.md](SECURITY.md).

## License

- [LICENSE](LICENSE): Apache License 2.0, the overall license for this project,
  except where specified.
- [LICENSE-DOCS](LICENSE-DOCS): Creative Commons Attribution 4.0 International,
  the license for all documentation and images only.
- [LICENSE_understanding.txt](LICENSE_understanding.txt): Tenstorrent's
  clarification of how the Apache License 2.0 applies to this project.
- [NOTICE](NOTICE): copyright notice and third-party attributions.
