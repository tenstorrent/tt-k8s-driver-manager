# Troubleshooting

Common failures, ordered by how often they bite. Each entry has the
symptom you see in `kubectl`, the root cause, and the fix.

## Why isn't my driver installing?

Two places to look first:

```bash
# Per-node status with reason codes (HostManagedKMD, BuilderCrashLoop, ...)
kubectl describe ttdp <name>

# Event stream — same reason codes, filterable across CRs
kubectl get events --field-selector involvedObject.name=<ttdp>
```

`.status.nodes[]` shows each node's `state`, `reason`, and `message`:

```
Status:
  Nodes:
    Name:    e01cs01
    State:   HostManaged
    Reason:  HostManagedKMD
    Message: DKMS / host-managed kmd detected; operator standing down ...
```

Reason codes the controller emits today:

| Reason | State | Meaning |
|---|---|---|
| `HostManagedKMD` | `HostManaged` | DKMS signals on host; operator stood down. Expected — see [migrating-from-dkms.md](migrating-from-dkms.md) if you want to take over. |
| `BuilderImagePullFailed` | `Failed` | kubelet can't pull the builder image. Check `imagePullSecrets` on the installer SA. |
| `BuilderCrashLoop` | `Failed` | Builder pod restarted ≥3x. Logs distinguish: rmmod refused, build failed, insmod failed, missing headers. |
| `Installing` | `Upgrading` | Builder pod running; readiness probe not yet passing. |
| `Cordoning` / `Draining` / `Uncordoning` | (same) | Drain machinery in flight. |
| `Ready` | `Done` | Pod is Ready against the target version. |

Each state-or-reason transition also fires a Kubernetes Event, so a
`kubectl describe ttdp` shows the trail without operators having to
`kubectl logs` the controller.

## Why isn't my firmware flashing?

Same two places as the driver, one CR kind over:

```bash
# Per-node status with reason codes (FlashJobFailed, DrainTimeout, ...)
kubectl describe ttfwp <name>

# Event stream — same reason codes, filterable across CRs
kubectl get events --field-selector involvedObject.name=<ttfwp>
```

`.status.nodes[]` shows each node's `state`, `reason`, and `message`:

```
Status:
  Nodes:
    Name:           node-3
    State:          Pending
    Reason:         RolloutHalted
    Message:        another node in this CR is Failed and spec.upgradePolicy.haltOnFailure=true
    Current Version: 19.8.0.0
```

Reason codes the firmware controller emits today:

| Reason | State | Meaning |
|---|---|---|
| `Flashing` | `Flashing` | Flash Job's pod is running — tt-flash mid-write or doing readback. |
| `FlasherImagePullFailed` | `Flashing` | kubelet can't pull the flasher image. Check `spec.flasher.image` and `imagePullSecrets`. State stays `Flashing` — the Job's `activeDeadlineSeconds` decides when it's terminal. |
| `FlashJobFailed` | `Failed` | Job hit a terminal failure: non-zero tt-flash exit, readback mismatch, or the flash deadline. `kubectl logs job/<lastFlashJob>` distinguishes them. |
| `FlashSucceeded` | `Done` | Job completed and no uncordon was owed. |
| `Cordoning` / `Draining` / `Uncordoning` | (same) | Drain machinery in flight. |
| `EvictionBlocked` | `Draining` | A PDB refused an eviction this pass. Retried every reconcile until the drain deadline. |
| `DrainTimeout` | `Failed` | Drain window expired with pods still holding `/dev/tenstorrent`. Terminal — see the message for the blocking pods. |
| `ExternalCordon` | `Pending` | Node is cordoned, but not by us. See [Policy never matches](#policy-never-matches-messageexternalcordon). |
| `NodeConflict` | `Pending` | Node is owned by another `ttfwp`. See [Multiple policies match](#multiple-policies-match-the-same-node-messagenodeconflict). |
| `Paused` | `Pending` | `spec.paused=true`. |
| `RolloutHalted` | `Pending` | Another node is `Failed` and `spec.upgradePolicy.haltOnFailure=true` (the default). Fix or exclude the failed node to resume. |
| `AutoUpgradeDisabled` | `Pending` | `spec.upgradePolicy.autoUpgrade=false` — drift is reported, never acted on. |
| `TransientAPIError` | `Pending` / `Draining` | An API read blipped mid-observation. Deliberately not `Failed`; clears itself. |

A `Pending` node with **no** reason is the ordinary case: it's ready to
flash and waiting on a `spec.upgradePolicy.maxParallel` slot.

Each state-or-reason transition also fires a Kubernetes Event, so
`kubectl describe ttfwp` shows the trail — including a `Flashing` node
sliding into `FlasherImagePullFailed` and back — without operators
having to `kubectl logs` the controller.

## ImagePullBackOff on the driver/flasher pods

```
NAME                  READY   STATUS             RESTARTS   AGE
ttdrv-default-...     0/1     ImagePullBackOff   0          5m
```

```bash
$ kubectl -n tt-k8s-driver-manager-system describe pod ttdrv-...
... Failed to pull image ... HTTP 4xx / network unreachable ...
```

Image registries are public, so this is almost always one of:

- **Egress blocked** to `ghcr.io` or `pkg-containers.githubusercontent.com`.
  Check your cluster's proxy / firewall allowlist.
- **Wrong image tag** in the policy or chart values — `docker pull
  <image>:<tag>` from a workstation to confirm the tag exists.
- **Pull secret left over** from a previous private-registry setup that
  no longer authenticates. Drop the secret from the ServiceAccount:
  `kubectl -n tt-k8s-driver-manager-system patch sa tt-k8s-driver-manager-installer
  --type=json -p='[{"op":"remove","path":"/imagePullSecrets"}]'`.

## "Pod is in use; cannot reinstall" — refcnt > 0

```
$ kubectl -n tt-k8s-driver-manager-system logs ttdrv-default-...
ERROR: tt-kmd 2.7.0 loaded with refcnt > 0; cannot reinstall 2.8.0
Holders: 12345 23456
Drain workloads holding /dev/tenstorrent and let the next reconcile retry.
```

A workload has `/dev/tenstorrent/*` open. The builder refuses to
`rmmod` (the kernel would refuse anyway with `Module is in use`).

Find the workloads:

```bash
$ kubectl get pods --all-namespaces -o json | jq -r '
  .items[] | select(.spec.volumes[]? | .hostPath.path? == "/dev/tenstorrent")
  | "\(.metadata.namespace)/\(.metadata.name) on \(.spec.nodeName)"'
```

Or use the PIDs from the log + `kubectl debug node/X` to
`chroot /host ps -fp <pid>`.

Drain those pods (`kubectl delete pod` for bare pods,
`kubectl scale deploy --replicas=0` for Deployments). The next
reconcile will see `refcnt=0` and proceed.

## `/bin/sh: 1: gcc-12: not found`

```
warning: the compiler differs from the one used to build the kernel
  The kernel was built by: x86_64-linux-gnu-gcc-13 ...
  You are using:           ...
/bin/sh: 1: gcc-12: not found
make[2]: *** [/tmp/.../module.o] Error 127
```

The kernel headers' Makefile hardcodes a specific `gcc-<version>`
binary name (matching the gcc the kernel was compiled with). The
builder image ships gcc-12 by default — fine for Ubuntu 22.04 jammy +
HWE 6.x kernels. If your host's kernel was compiled with a different
gcc, the build dies.

Check the host's compiler:

```bash
$ cat /proc/version
Linux version 6.x.0-... (Ubuntu gcc-13 ...)
```

Two fixes:

1. **Use a builder image built with the matching gcc.** Bump
   `gcc-12` → `gcc-N` in `images/driver-build/Dockerfile`, rebuild,
   push, point `driver.image` at the new tag.
2. **Use a host with a different kernel.** Match the kernel against
   the builder image — `apt install linux-image-generic-hwe-22.04`
   pulls a jammy/gcc-12 kernel.

Long-term, the builder should detect the kernel's compiler at runtime
and `apt install` the right gcc — open TODO.

## `host has no kernel build tree at /lib/modules/<kver>/build`

```
ERROR: host has no kernel build tree at /lib/modules/6.8.0-111-generic/build
Install linux-headers-6.8.0-111-generic on the host.
```

The builder needs the host's kernel headers to compile against. The
host doesn't have them installed (or has the wrong ones — e.g. headers
for an older kernel after a kernel upgrade + reboot).

```bash
sudo apt install linux-headers-$(uname -r)
# Or, on HWE:
sudo apt install linux-headers-generic-hwe-22.04
```

After that, restart the failing pod:

```bash
kubectl -n tt-k8s-driver-manager-system delete pod -l app.kubernetes.io/component=driver \
  --field-selector spec.nodeName=<that-node>
```

## NFD label missing — node not picked up

```bash
$ kubectl get nodes -L feature.node.kubernetes.io/pci-1200_1e52.present
NAME      STATUS   PRESENT
node-1   Ready
```

(empty, but the node has a Tenstorrent card)

Causes:

1. **NFD worker not running on that node.** Check
   `kubectl get pod -l app.kubernetes.io/name=node-feature-discovery -A`.
   The worker pod should be `Running` on every Tenstorrent node.
2. **NFD's PCI source disabled.** Check the NFD chart's
   `worker.config.core.featureSources` — must include `pci`. The
   tt-operator umbrella sets this to `["pci"]` (only); a separate NFD
   install might have it disabled.
3. **NFD's deviceClassWhitelist excludes class 12.** Class 1200 is in
   the default whitelist. If you narrowed it, add class 12 back.

If you want to bypass NFD entirely (dev clusters, kind), set on the
controller deployment:

```bash
helm upgrade tt-k8s-driver-manager ... --set controller.requireTenstorrentLabel=false
```

The DaemonSet's `nodeAffinity` will still require the label though, so
also `kubectl label node <name> feature.node.kubernetes.io/pci-1200_1e52.present=true`
on dev nodes. There's a `hack/dev/label-fake-tt-nodes.yaml` for kind.

## Module loaded but pod NotReady

```bash
$ kubectl -n tt-k8s-driver-manager-system get pod -l app.kubernetes.io/component=driver
ttdrv-default-...   0/1     Running   0   30s
```

Pod's readiness probe checks `/sys/module/tenstorrent/version ==
$TT_KMD_VERSION`. If the loaded version doesn't match what the CR
wants:

- Pod is honest: it's NotReady because the kernel isn't at the
  declared state.
- Most common cause: `rmmod` failed earlier (refcnt > 0) so the new
  `.ko` was built and `insmod` was called, but `insmod` failed
  silently because the module is already loaded with the same name.

Check the pod's logs for the actual error. Then either drain
workloads (refcnt → 0) or reboot the host.

## Host has tt-kmd from DKMS/apt; operator ignored it

```bash
$ kubectl get node node-1 -L driver.tenstorrent.com/install-mode
NAME     INSTALL-MODE
node-1   host
```

Expected, not a bug. The builder pod detected `/var/lib/dkms/tenstorrent`
or `/usr/src/tenstorrent-<v>/dkms.conf` and stood down. The host's
tt-kmd stays, the operator doesn't `rmmod` or rebuild.

If you want the operator to take over, follow
[Migrating from DKMS](migrating-from-dkms.md) — it has the
per-node vacate script, the cluster-side cordon/drain coordination, and
the watch-outs (both DKMS signal dirs, refcnt > 0, proxied builder).

If you want to keep the host install and just stop driver-manager from
touching the node entirely:

```bash
kubectl label node <name> driver.tenstorrent.com/skip=true
```

## tt-smi can't execute on host

```
$ tt-smi -s
tt-smi: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.38' not found
```

`/usr/local/bin/tt-smi` is the self-contained binary from tt-smi's
GitHub releases, which ships one flavor per Ubuntu release
(`tt-smi-<v>-ubuntu-22.04`, `-ubuntu-24.04`, ...). The builder image
bakes in the flavor matching its `ARG UBUNTU_VERSION`; if that's newer
than the host OS, the binary needs glibc symbols the host doesn't have.

Fix: set `ARG UBUNTU_VERSION` in `images/driver-build/Dockerfile` to
the hosts' Ubuntu release and push. Mixed-OS fleets need one builder
image (and so one `TenstorrentDriverPolicy` with a matching
`nodeAffinity`) per Ubuntu release.

## Builder pod can't clone tt-kmd — proxy / DNS

```
$ kubectl -n tt-k8s-driver-manager-system logs ttdrv-default-...
fatal: unable to access 'https://github.com/tenstorrent/tt-kmd.git/':
  Could not resolve host: github.com
```

Builder pod has no network path to GitHub for the cache-miss clone.
Common when pod egress goes through a proxy (CI behind squid, isolated
clusters) and `HTTPS_PROXY` isn't set on the builder.

The controller propagates its own `HTTPS_PROXY` / `HTTP_PROXY` /
`NO_PROXY` env to spawned builder pods, so the fix is usually on the
controller side. Check that the controller pod itself has the proxy
env set:

```bash
kubectl -n tt-k8s-driver-manager-system get deploy \
  tt-k8s-driver-manager-controller -o jsonpath='{.spec.template.spec.containers[0].env}' \
  | jq '.[] | select(.name | test("PROXY"))'
```

If empty, set via Helm:

```bash
helm upgrade tt-k8s-driver-manager ... \
  --set controller.extraEnv[0].name=HTTPS_PROXY \
  --set controller.extraEnv[0].value=http://proxy.internal:3128 \
  --set controller.extraEnv[1].name=NO_PROXY \
  --set controller.extraEnv[1].value=10.0.0.0/8,.svc,.svc.cluster.local
```

The controller restart re-templates the builder DaemonSet with the
new env; existing pods need a delete to re-roll.

## Policy never matches — `MessageExternalCordon` on the CR

```bash
$ kubectl describe ttfp <name>
...
Status:
  Per Node:
    Name:     node-1
    State:    Pending
    Reason:   ExternalCordon
    Message:  node is cordoned but not by this operator
```

The node has `spec.unschedulable: true` but no
`firmware.tenstorrent.com/cordoned-by=<policy-name>` (or the driver
equivalent `driver.tenstorrent.com/cordoned-by`) annotation. The
controller refuses to flash a node it didn't cordon itself — protects
SREs who've taken nodes out of rotation for unrelated reasons.

Either uncordon and let the controller cordon it itself:

```bash
kubectl uncordon <node>
```

…or claim the existing cordon by stamping the annotation:

```bash
kubectl annotate node <node> \
  firmware.tenstorrent.com/cordoned-by=<policy-name>
```

(Same pattern for driver policies, with `driver.tenstorrent.com/cordoned-by`.)

Alternative: set `upgradePolicy.drain.enable: false` on the CR to skip
the cordon gate entirely — flash Job will land via its universal
toleration regardless of cordon state. Loses the device-pod-eviction
safety.

## Flash didn't re-run after editing `spec.flasher.image`

You bumped `TenstorrentFirmwarePolicy.spec.flasher.image` (or
`forceWrite`, `imagePullPolicy`, etc.) and the controller did nothing.

Known limitation:
the per-node flash Job name is hashed on `(CR name, node name, kmd
version)`. The flasher fields are NOT in the hash. If a Job already
exists at that name with `Completed` status, the controller treats
the node as Done.

Workarounds until #42 ships a fix:
- Bump `spec.version` (forces a new Job name).
- Delete the per-node Job:
  `kubectl -n tt-k8s-driver-manager-system delete job -l driver.tenstorrent.com/cr=<name>,driver.tenstorrent.com/node=<node>`.
- Wait out the 24h Job TTL — the next reconcile will spawn a fresh
  Job that picks up the new flasher fields.

## Multiple policies match the same node — `MessageNodeConflict`

```bash
$ kubectl describe ttfp <name>
...
    Reason:   NodeConflict
    Message:  node is also matched by another firmware policy
```

Two `TenstorrentFirmwarePolicy` (or two `TenstorrentDriverPolicy`) CRs
have `nodeSelector` overlap. Controller refuses to flash to avoid
racing each other.

Find the offenders:

```bash
kubectl get ttfp -o json | jq -r '
  .items[] | {name: .metadata.name, selector: .spec.nodeSelector}'
```

Narrow one selector so each node matches exactly one CR. Typical
mistake: a "wildcard" CR (empty `nodeSelector`) sitting next to a
team-scoped CR. Add a `matchExpressions` `NotIn` on the wildcard or
delete it.

## CR keeps re-flashing despite node being at the right version

The firmware controller's "this node is done" signal is a `Complete`
Job in its own namespace. If you (a) moved the controller to a new
namespace, or (b) deleted all old flash Jobs, the controller has no
record that the node was flashed and re-runs.

Pre-flash readback (inside the flasher Job) will report the existing
version matches the desired, and tt-flash with `--force=false` will
no-op. So it's harmless, just slow. The node's `current-version`
annotation is the cheaper source of truth — bumping the controller to
read it as a tiebreaker is open work.

## Controller is in a tight reconcile loop

`kubectl logs -n tt-k8s-driver-manager-system deploy/tt-k8s-driver-manager-controller`
shows "updated tt-kmd DaemonSet" multiple times per second.

Old symptom (fixed): the diff predicate compared image but missed
other template fields. If you see this on a current version, file a
bug — the `driver.tenstorrent.com/template-hash` annotation on the DS
should be making this impossible.

## Fully clean a host

When a node has been managed by both this operator and a host-side
DKMS installer (or earlier non-container installer pods) and you want
to start fresh:

```bash
# As root on the node:
rmmod tenstorrent                                      # may fail if refcnt>0
rm -rf /usr/src/tenstorrent-*
rm -rf /var/lib/dkms/tenstorrent
rm -f /lib/modules/*/updates/dkms/tenstorrent.ko*
rm -f /etc/modules-load.d/tenstorrent.conf
rm -rf /opt/tt
rm -f /usr/local/bin/tt-smi
rm -rf /var/cache/tt-kmd/
for kdir in /lib/modules/*/; do depmod -a "$(basename $kdir)"; done
```

After reboot, the host should have no trace of tt-kmd, and the
builder pod will fall through to its build path on first reconcile.

This can also be scripted via `kubectl debug node/<n> --profile=sysadmin
-- chroot /host bash -c '<script>'` so you don't need SSH access.

## When all else fails

```bash
kubectl -n tt-k8s-driver-manager-system logs deploy/tt-k8s-driver-manager-controller --tail=100
kubectl -n tt-k8s-driver-manager-system get events --sort-by=.lastTimestamp | tail -20
kubectl -n tt-k8s-driver-manager-system describe ttdp <name>
kubectl describe node <name>
```

Most issues are visible in one of those four. File a bug with:

- The CR's full YAML (`kubectl get ttdp <name> -o yaml`)
- Affected node's labels (`kubectl get node <name> -o yaml | grep -A20 labels`)
- The failing pod's logs (current and `--previous`)
- The events from the operator namespace.
