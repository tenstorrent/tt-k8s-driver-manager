# tt-k8s-driver-manager

Kubernetes operator that owns the host-side software stack for Tenstorrent
hardware:

| Layer | Managed via | Where it lives at runtime |
|---|---|---|
| `tt-kmd` (kernel module) | `TenstorrentDriverPolicy` CR | loaded in host kernel; `.ko` cached at `/var/cache/tt-kmd/<kver>/<v>/` |
| `tt-smi` (userspace CLI) | bundled in builder image, tracks CR | self-contained binary at host `/usr/local/bin/tt-smi` |
| device firmware | `TenstorrentFirmwarePolicy` CR | flashed on-chip via per-node `tt-flash` Job |

One declarative CR per concern (no `ClusterPolicy` god-object). One privileged
DaemonSet per CR that builds tt-kmd in-container, against host kernel headers,
and `insmod`s into the shared kernel.

## Quick install

Driver-manager is published as a Helm chart (OCI). For everything-in-one
(NFD + driver-manager + CRDs), use the [tt-operator](https://github.com/tenstorrent/tt-operator)
umbrella; for just driver-manager:

```bash
helm install tt-k8s-driver-manager oci://ghcr.io/tenstorrent/helm/tt-k8s-driver-manager \
  --namespace tt-k8s-driver-manager-system --create-namespace
```

Then apply CRs for the components you want managed. Each CR is independent
— install the driver without flashing firmware, or flash firmware without
managing the driver.

**Driver** (`TenstorrentDriverPolicy`, short name `ttdp`) — pins a `tt-kmd`
version on selected nodes; the operator builds + insmods it via a per-CR
DaemonSet, cordoning + draining each node before swapping kernel modules:

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
      fullNode: true    # also run full kubectl-drain semantics (NVIDIA pattern)
    forceUnload: false  # set true to SIGKILL surviving holders before rmmod
```

**Firmware** (`TenstorrentFirmwarePolicy`, short name `ttfwp`) — pins a
firmware-bundle version on selected nodes; the operator drives each node
through cordon → drain → flash → uncordon via a per-node Job:

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
      enable: true      # cordon + PDB-respecting eviction before flash
```

See [`docs/driver.md`](docs/driver.md) and [`docs/firmware.md`](docs/firmware.md)
for the full spec — drain policy, forceUnload, deploy gates, per-CR image
overrides, skip labels, multi-pool examples.

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

- `/var/cache/tt-kmd/<kver>/<version>/tenstorrent.ko` — operator's per-kernel
  build cache. Survives pod restarts. Not in `/lib/modules` — the operator
  doesn't use depmod, doesn't write to modules.alias, doesn't compete with
  any host package.
- `/usr/local/bin/tt-smi` — self-contained binary (the PyInstaller build
  from tt-smi releases; no Python needed on the host). Owned by the
  operator, replaced atomically on each upgrade, available to anyone who
  SSHes onto the node.

The kernel module itself is loaded into the running kernel (via
`init_module(2)` from the privileged builder pod); rebooting the host
unloads it, and the builder rebuilds + loads on next boot.

## Mixed environments

If a node already has `tt-kmd` installed via apt or DKMS (e.g. by a host-side config-management tool), the operator
detects this on first reconcile, labels the node
`driver.tenstorrent.com/install-mode=host`, and stands down — no `rmmod`,
no rebuild, no overwriting `tt-smi`. The operator still reports the loaded
version via `driver.tenstorrent.com/kmd-version`, so dashboards and
`nodeSelector`-based scheduling work the same way regardless of who owns
the install. See [docs/driver.md](docs/driver.md#mixed-mode) for the full
detection logic.

To opt a node out of all reconciliation (vs. just "operator detected host
ownership"), set `driver.tenstorrent.com/skip=true` on the node.

## Docs

| | |
|---|---|
| [Install](docs/install.md) | Prerequisites, NFD, verifying |
| [Drivers](docs/driver.md) | `TenstorrentDriverPolicy`, upgrade flow, drain policy, deploy gates, install-mode, skip label |
| [Firmware](docs/firmware.md) | `TenstorrentFirmwarePolicy`, drain config, force-write flag |
| [Upgrades](docs/upgrades.md) | Rolling-update semantics for tt-kmd, tt-smi, firmware, operator itself |
| [Migrating from DKMS](docs/migrating-from-dkms.md) | Vacate a DKMS install so the operator can take over kmd lifecycle |
| [Troubleshooting](docs/troubleshooting.md) | Common failures and how to read the symptoms |

## Related repos

- [tt-operator](https://github.com/tenstorrent/tt-operator) — umbrella Helm
  chart bundling driver-manager + node-feature-discovery
- [tt-kmd](https://github.com/tenstorrent/tt-kmd) — the kernel module the
  builder compiles from source
- [tt-smi](https://github.com/tenstorrent/tt-smi) — the userspace CLI
- [tt-system-firmware](https://github.com/tenstorrent/tt-system-firmware) —
  firmware bundle releases

## For contributors

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

**Images** (linux/amd64; on arm64 push a branch and let GHA build):

```bash
make controller-image    # ghcr.io/.../tt-k8s-driver-manager:dev
make builder-image       # ghcr.io/.../tt-k8s-driver-manager-builder:dev
make flasher-image       # ghcr.io/.../tt-k8s-driver-manager-flasher:dev
make helm-install        # deploy :dev images to the current kube context
```

**kubectl plugins:**

```bash
make install-plugins     # kubectl-tt-driver + kubectl-tt-fw → ~/.local/bin
```

### Where to start

1. `internal/controller/driver_policy_controller.go` — core reconcile loop for driver installs + upgrades.
2. `internal/controller/firmware_policy_controller.go` — same pattern for firmware flash jobs.
3. `images/driver-build/entrypoint.sh` — what runs in the privileged pod: kernel build + insmod.

API types (spec fields, status conditions): `api/driver/v1alpha1/` and `api/firmware/v1alpha1/`.
