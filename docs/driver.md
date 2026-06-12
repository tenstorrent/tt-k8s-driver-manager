# Driver management

Driver-manager installs and maintains a specific `tt-kmd` version on each
Tenstorrent node via a `TenstorrentDriverPolicy` (short name: `ttdp`).

## The minimum CR

```yaml
apiVersion: driver.tenstorrent.com/v1alpha1
kind: TenstorrentDriverPolicy
metadata:
  name: default
spec:
  version: "2.8.0"
  nodeSelector: {}
```

What happens:

1. Controller creates a privileged DaemonSet named `ttdrv-<crname>` in
   the operator namespace.
2. Pod template's `nodeAffinity` is `nodeSelector` ∧
   `feature.node.kubernetes.io/pci-1200_1e52.present=true` ∧
   `!exists(driver.tenstorrent.com/skip)`. So pods schedule only on
   Tenstorrent nodes that aren't opted out.
3. Each pod's entrypoint:
   - Probes for an existing host install (DKMS + `/usr/src/tenstorrent-*`).
     If detected → label node `install-mode=host`, idle, **don't touch
     anything**.
   - Otherwise, compares `/sys/module/tenstorrent/version` against
     `spec.version`. Match → idle. Mismatch with `refcnt=0` → `rmmod`,
     clone tt-kmd at the requested tag, `make modules` against host
     kernel headers, `insmod`. Mismatch with `refcnt>0` (workload holding
     the device) → fail loudly with the holder PIDs.
4. Copies the bundled self-contained `tt-smi` binary to
   `/host/usr/local/bin/tt-smi`.
5. Stamps `kmd-version`, `tt-smi.driver.tenstorrent.com/version`, and
   `install-mode` labels on the node it's running on.
6. `exec sleep infinity` — pod stays Ready; readiness probe rechecks
   `/sys/module/tenstorrent/version` every 10s.

## Spec fields

| Field | Default | Purpose |
|---|---|---|
| `version` | required | tt-kmd release tag, minus `ttkmd-` prefix. Must match `^[0-9]+\.[0-9]+\.[0-9]+$`. |
| `nodeSelector` | required | Standard `metav1.LabelSelector`. Empty `{}` matches all nodes (still ANDed with NFD present-label, so only Tenstorrent nodes get hit). |
| `paused` | `false` | Soft stop. Controller stops reconciling; existing DS keeps running. Useful for blast-radius pauses without deleting the CR. |
| `upgradePolicy.drain.enable` | `true` | Pass 1: cordon + evict pods that `hostPath`-mount `/dev/tenstorrent` before the DS template is bumped, so refcount has dropped to 0 by the time the new builder pod runs `rmmod`. See [Upgrade flow](#upgrade-flow). |
| `upgradePolicy.drain.fullNode` | `true` | Pass 2: full-node `kubectl drain` semantics — evict every non-DS pod on the cordoned node. Catches privileged containers that get `/dev/tenstorrent` via containerd auto-mount (no explicit hostPath). Mirrors NVIDIA's `ENABLE_AUTO_DRAIN`. |
| `upgradePolicy.drain.podSelectorLabel` | `""` | Restricts pass 2 to pods matching this selector (`key=value`, `key`, `key notin (a,b)`). Empty = sweep everything. Mirrors NVIDIA's `DRAIN_POD_SELECTOR_LABEL`. |
| `upgradePolicy.drain.force` | `false` | Evict bare pods (no controller) instead of skipping. Applies to both passes. |
| `upgradePolicy.drain.deleteEmptyDir` | `true` | Pass 2 evicts pods with `emptyDir` volumes (kubectl drain's `--delete-emptydir-data`). |
| `upgradePolicy.drain.timeoutSeconds` | `600` | Per-node drain deadline. |
| `upgradePolicy.forceUnload` | `false` | Last resort: if `refcnt>0` after drain, the builder pod walks `/proc/*/fd` and SIGKILLs every process holding `/dev/tenstorrent` before `rmmod`. Off by default — prefer draining over killing workloads. |
| `installer.image` | chart's `driver.image` | Per-CR override of the builder image. Useful for canary-ing a new builder. |
| `installer.imagePullPolicy` | `IfNotPresent` | Override for the above. Set to `Always` when iterating on a moving image tag. |

## CR examples

### Whole-fleet install

```yaml
apiVersion: driver.tenstorrent.com/v1alpha1
kind: TenstorrentDriverPolicy
metadata:
  name: fleet
spec:
  version: "2.8.0"
  nodeSelector: {}
```

### Multiple versions across pools

Two non-overlapping CRs. Nodes are pinned to pools via a custom label
(here `tt.tenstorrent.com/pool`):

```yaml
apiVersion: driver.tenstorrent.com/v1alpha1
kind: TenstorrentDriverPolicy
metadata: { name: prod }
spec:
  version: "2.7.0"
  nodeSelector: { matchLabels: { tt.tenstorrent.com/pool: prod } }
---
apiVersion: driver.tenstorrent.com/v1alpha1
kind: TenstorrentDriverPolicy
metadata: { name: canary }
spec:
  version: "2.8.0"
  nodeSelector: { matchLabels: { tt.tenstorrent.com/pool: canary } }
```

The selectors must be disjoint — overlapping CRs both spawn DS pods on
the shared nodes, and the second pod loses (`rmmod` fails because the
first's module is loaded).

### Pause for incident response

```bash
kubectl patch ttdp default --type merge -p '{"spec":{"paused":true}}'
```

The controller stops reconciling. Pods keep running with whatever
version they last loaded. Unpause when ready:

```bash
kubectl patch ttdp default --type merge -p '{"spec":{"paused":false}}'
```

### Per-CR installer image (canary)

```yaml
spec:
  version: "2.8.0"
  installer:
    image: ghcr.io/tenstorrent/tt-k8s-driver-manager-builder:sha-abc1234
    imagePullPolicy: Always
```

## Upgrade flow

Bump `spec.version`:

```bash
kubectl patch ttdp default --type merge -p '{"spec":{"version":"2.8.0"}}'
```

Per-node state machine (mirrors `ttfwp`):

```
Pending → Cordoning → Draining → Upgrading → Uncordoning → Done
                                                       ↘ Failed
```

Cordoning / Draining / Uncordoning are skipped when
`upgradePolicy.drain.enable=false`. Per-node state is surfaced on
`status.nodes[]` — see [Watch progress](#watch-progress).

What the controller does:

1. Before bumping the DS template, **cordons matched nodes** and flips
   `controller.deployGates` labels (default
   `tenstorrent.com/deploy.tt-telemetry=false`) so sibling DaemonSets
   that hold `/dev/tenstorrent` evict themselves.
2. **Drains** each node in two passes: pass 1 evicts pods that
   `hostPath`-mount `/dev/tenstorrent`; pass 2 (gated by
   `drain.fullNode`, default on) runs full `kubectl drain` semantics.
3. Re-renders the DS pod template with the new `TT_KMD_VERSION` env
   and a sha256 `driver.tenstorrent.com/template-hash`. K8s rolling
   update kicks in with `maxUnavailable: 1`.
4. On each new pod start, entrypoint sees `LOADED != EXPECTED`, checks
   refcnt; with the drain done it's normally 0. `forceUnload=true` =
   SIGKILL holders via `/proc/*/fd` walk before `rmmod`. Then either
   pulls from `/var/cache/tt-kmd/<kver>/<new-version>/` (cache hit, ~5s
   total) or clones tt-kmd + `make modules` (cache miss, ~30-90s) and
   `insmod`s.
5. Pod's readiness probe (`/sys/module/tenstorrent/version` matches
   `$TT_KMD_VERSION`) passes; controller **uncordons** the node and
   removes the deploy-gate labels (sibling DSes reschedule); rolling
   update advances to the next node.

A 3-node cluster upgrade takes ~1–2 min cache-cold, ~30s cache-warm
(after a previous upgrade on this CR has already populated the cache).

### Watch progress

```bash
$ kubectl get ttdp default -o jsonpath='{.status.nodes}' | jq
[
  {"name":"e01cs01","currentVersion":"2.8.0","state":"Done"},
  {"name":"e01cs02","currentVersion":"2.7.0","state":"Draining"},
  {"name":"e01cs03","currentVersion":"2.7.0","state":"Pending"}
]
```

### Deploy gates

`controller.deployGates` in the chart's `values.yaml` is the list of
node-label keys the controller flips off (`=false`) during a kmd
upgrade and removes on uncordon. Sibling DaemonSets that consume
`/dev/tenstorrent` (tt-telemetry, future workloads) must include
`NotIn ["false"]` on the same label key in their `nodeAffinity` — the
chart-level pattern from NVIDIA gpu-operator. Default list:
`tenstorrent.com/deploy.tt-telemetry`. Set to `[]` to disable the
gate-flip entirely.

### Downgrade

Same flow; just patch `spec.version` to a lower number. The previous
version's `.ko` is already cached if you've used it before.

### Roll back via Helm

For driver-manager controller upgrades (not driver upgrades), use Helm:

```bash
helm -n tt-k8s-driver-manager-system rollback tt-k8s-driver-manager
```

CRs are unaffected; the controller pod restarts with the older image.

## What gets put on hosts

Per node, after a successful reconcile:

```
/var/cache/tt-kmd/
└── 6.8.0-111-generic/
    ├── 2.7.0/tenstorrent.ko   ← cached build from a previous CR
    └── 2.8.0/tenstorrent.ko   ← currently loaded
/usr/local/bin/tt-smi           ← self-contained binary (PyInstaller build,
                                  no host Python needed)
```

The kernel module itself is in-kernel — `/sys/module/tenstorrent/` shows
it; `lsmod | grep tenstorrent` confirms; the on-disk `.ko` is only
consulted at `insmod` time.

The operator does NOT touch:

- `/lib/modules/<kver>/updates/dkms/` — that's where DKMS would install.
  The containerized builder uses `insmod` from its cache, not `modprobe`
  / DKMS. Hosts that previously had a DKMS install will still have files
  there; the operator just ignores them and detects them as host-managed.
- `modules.alias`, `modules.dep`, modules-load.d. The operator's
  containerized model doesn't participate in the kernel module
  auto-load chain.

## Node labels the operator sets

| Label | Value | When |
|---|---|---|
| `driver.tenstorrent.com/kmd-version` | running module's version (from `/sys/module/tenstorrent/version`) | When the builder pod's readiness probe passes |
| `tt-smi.driver.tenstorrent.com/version` | `$TT_SMI_VERSION` baked into builder image | After the builder copies tt-smi to host |
| `driver.tenstorrent.com/install-mode` | `container` or `host` | Set every reconcile based on host-install detection |

Workloads requiring a specific tt-kmd version can `nodeSelector` against
`driver.tenstorrent.com/kmd-version`:

```yaml
nodeSelector:
  driver.tenstorrent.com/kmd-version: "2.8.0"
```

This is honest: the label reflects what's loaded *right now*, not what
the CR says it wants. During a rolling upgrade, the label flips per-node
as each pod's new version becomes Ready.

## Mixed mode

driver-manager auto-detects nodes that already have a host-managed
tt-kmd install (e.g. from `tt-ansible`'s `tt_kmd` role) and stands down
on them. Signals checked:

- `/var/lib/dkms/tenstorrent/` exists — DKMS tracks the module.
- `/usr/src/tenstorrent-<v>/dkms.conf` exists — DKMS source registered.

If either is present, the builder pod labels the node
`driver.tenstorrent.com/install-mode=host` and idles without:

- running `rmmod` or `insmod`;
- copying tt-smi to `/usr/local/bin`;
- writing to `/var/cache/tt-kmd/`.

The pod still propagates `kmd-version` to the node label, so observability
works the same as for container-managed nodes.

To force a host-managed node into container mode, remove the host's DKMS
state — see [docs/troubleshooting.md → fully clean a host](troubleshooting.md#fully-clean-a-host).
To do the inverse (keep operator out entirely, even from labelling), use
the [skip label](#skip-label).

## Skip label

`kubectl label node <name> driver.tenstorrent.com/skip=true` opts the node
out of all driver-manager reconciliation. The DaemonSet's `nodeAffinity`
includes a `DoesNotExist` requirement on this label, so labelling a
running node immediately evicts its installer pod (no `rmmod` first — the
loaded module stays, but pod-level state goes away).

Remove with `kubectl label node <name> driver.tenstorrent.com/skip-`.

Use this for hosts that are under hard manual management (debug nodes,
mid-incident hosts you don't want touched, etc.). For "host has its own
install but the operator should still observe it," use auto-detected
mixed mode instead.

## kubectl plugin

`hack/plugins/kubectl-tt-driver` collapses CR + DaemonSet + per-pod state
into one view. Install:

```bash
make install-plugins   # copies to ~/.local/bin/
kubectl tt driver
```

Output is a single table: per-CR rows, per-node columns, ANSI color for
state. See [`hack/plugins/kubectl-tt-driver --help`](../hack/plugins/kubectl-tt-driver)
for subcommands.
