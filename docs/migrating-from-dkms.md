# Migrating from DKMS-managed to operator-managed tt-kmd

Most production Tenstorrent clusters today install `tt-kmd` per-host via
DKMS (typically through `tt-ansible`'s `tt_kmd` role). When driver-manager
lands on those nodes it detects the DKMS state and stays out of the way —
the node is labelled `driver.tenstorrent.com/install-mode=host` and the
builder DaemonSet idles. This guide is for cluster operators who want to
switch a fleet (or one node at a time) from that host-managed mode to
operator-managed lifecycle.

## Why migrate

Operator-driven kmd management gives you:

- **Rolling upgrades** of `tt-kmd` across the fleet by patching a single
  `TenstorrentDriverPolicy` (`ttdp`) CR — no per-host SSH, no ansible run.
- **Version pinning** via the CR's `spec.version`, so a node's kmd is a
  declared piece of cluster state, not a side-effect of whoever last
  ran the playbook.
- **Drain-coordinated upgrades**: the controller cordons + evicts
  `/dev/tenstorrent` holders before `rmmod`, so workloads don't fall over
  on module swap.
- **Fleet-wide consistency**: every matched node converges on the same
  kmd build (operator's per-kernel `.ko` cache at
  `/var/cache/tt-kmd/<kver>/<v>/`), instead of N independent DKMS builds
  that can drift per host gcc / header state.

DKMS install is per-host and side-band of Kubernetes — perfectly fine
operationally, but invisible to anything that wants to reason about kmd
version as a fleet property.

## The two install modes

driver-manager makes a binary decision per node and stamps it onto a node
label:

| Mode | Label | Who owns kmd | When it's set |
|---|---|---|---|
| **Host-managed** | `driver.tenstorrent.com/install-mode=host` | Sysadmin via DKMS (apt / `tt-ansible` / manual `dkms install`) | Builder pod sees DKMS signals on the host and idles. |
| **Container-managed** | `driver.tenstorrent.com/install-mode=container` | Operator: builder pod compiles tt-kmd from source, `insmod`s, manages version per the CR | Builder pod sees no DKMS signals, falls through to its build path. |

The operator never tries to "convert" a host from one mode to the other.
It detects state, picks a mode, and stays in that mode. Migration is
explicit: you remove the DKMS signals on the host, and the builder pod
re-detects on the next reconcile.

## The detection heuristic

The builder pod checks two paths on the host filesystem at start-up:

- `/var/lib/dkms/tenstorrent/` — DKMS's per-module metadata directory.
- `/usr/src/tenstorrent-*/dkms.conf` — DKMS source registration.

If **either** exists, the node is treated as host-managed and the builder
idles. Both must be gone for the builder to fall through to its build
path. This is the same check documented in
[driver.md → Mixed mode](driver.md#mixed-mode).

## Per-node vacate procedure

Run this on the node as root (via Ansible, `kubectl debug node`, or SSH).
It's idempotent — re-running on a clean host is a no-op:

```bash
# 1. Confirm nothing is holding the device
cat /sys/module/tenstorrent/refcnt        # must be 0
lsof /dev/tenstorrent/* 2>/dev/null       # must be empty

# 2. Unload the running module
sudo rmmod tenstorrent

# 3. Remove DKMS install signals
sudo dkms remove tenstorrent --all 2>/dev/null || true
sudo rm -rf /var/lib/dkms/tenstorrent
sudo rm -rf /usr/src/tenstorrent-*
sudo find /lib/modules/$(uname -r) -name 'tenstorrent.ko*' -delete
sudo depmod -a

# 4. Verify
! [ -e /sys/module/tenstorrent/version ] && \
! [ -d /var/lib/dkms/tenstorrent ] && \
! compgen -G "/usr/src/tenstorrent-*" >/dev/null && \
! sudo modprobe tenstorrent 2>/dev/null && \
echo "host vacated; operator can take over"
```

If the final verification block prints `host vacated; operator can take
over`, the host has no DKMS state, no in-kernel module, and no installable
`.ko` on the modules path — the builder pod's next reconcile will fall
through to its build path.

If the verification fails, see [Watch-outs](#watch-outs) below.

To run via `kubectl debug` without SSH access:

```bash
kubectl debug node/<name> --profile=sysadmin -it --image=ubuntu:22.04 \
  -- chroot /host bash -c '<script-above>'
```

## Cluster-side coordination (cordon, drain, vacate, uncordon)

For a single-node test or a rolling fleet migration, wrap the per-node
script with the standard cordon + drain dance. **Do one node at a time**
for production; the script tears down the running kmd, so the node
briefly has no `/dev/tenstorrent`.

```bash
NODE=e01cs01

# 1. Cordon and drain device-holding workloads (and anything else
#    on the node). Adjust the selector to match how your workloads
#    label themselves; "tenstorrent.com/uses-device=true" is the
#    convention this guide assumes.
kubectl cordon "$NODE"
kubectl drain "$NODE" \
  --ignore-daemonsets \
  --delete-emptydir-data \
  --pod-selector='tenstorrent.com/uses-device=true' \
  --timeout=10m

# 2. Run the vacate procedure on the node (SSH / Ansible / kubectl debug)
#    — see "Per-node vacate procedure" above.

# 3. Uncordon. The builder DS will reschedule onto the node and,
#    finding no DKMS signals, fall through to the build path.
kubectl uncordon "$NODE"
```

If `spec.upgradePolicy.drain.enable=true` on your CR (the default), the
controller will also cordon/drain on its own as part of the rebuild — but
that drain happens *after* the vacate, when the operator is already
trying to load its own kmd. Doing the drain yourself first gives a
quieter sequence: device-holders are gone before you `rmmod`, so the
verification block doesn't trip on `refcnt > 0`.

## Watch the takeover

Three signals to confirm the operator picked the node up:

```bash
# install-mode label flips host → container
kubectl get nodes -L driver.tenstorrent.com/install-mode

# Builder pod logs: cloning tt-kmd, running make modules, insmod
kubectl -n tt-k8s-driver-manager-system logs \
  -l driver.tenstorrent.com/cr=<ttdp-name> \
  --tail=200 -f

# Device nodes reappear once insmod completes
ls /dev/tenstorrent/
```

After the builder finishes (~30–90s for the first build, ~5s on cache
hit), `kubectl get ttdp` should show the CR's `READY` count increment
for this node.

## A minimal ttdp for the test

Apply the CR with `paused: true` *before* you start vacating, so the
operator doesn't fire on the node before you're ready. Flip to
`paused: false` once the host is clean and you want the operator to take
over:

```yaml
apiVersion: driver.tenstorrent.com/v1alpha1
kind: TenstorrentDriverPolicy
metadata:
  name: migration-test
spec:
  version: "2.8.0"                      # whatever DKMS was pinning, or the version you want to land on
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: e01cs01   # one node only for the first cut
  paused: true                          # flip to false after the vacate
  upgradePolicy:
    drain:
      enable: true
```

```bash
# After the vacate script reports "host vacated; operator can take over":
kubectl patch ttdp migration-test --type merge -p '{"spec":{"paused":false}}'
```

Once this single-node migration is clean, widen `nodeSelector` (or apply
a fleet-scoped CR like the one in [driver.md → Whole-fleet install](driver.md#whole-fleet-install))
and migrate the rest of the fleet one node at a time.

## Watch-outs

- **Both DKMS signal dirs must be gone.** The detection is an OR: a
  leftover `/usr/src/tenstorrent-*` directory keeps the node in
  host-managed mode even with `/var/lib/dkms/tenstorrent` cleared. The
  verification block in the vacate script tests both — don't skip it.
- **`modprobe tenstorrent` should fail after cleanup.** If `modprobe`
  still loads the module, a `tenstorrent.ko*` file slipped past the
  `find -delete` somewhere under `/lib/modules/$(uname -r)`. Locate
  with `find /lib/modules/$(uname -r) -name 'tenstorrent.ko*'` and
  delete by hand, then re-run `depmod -a`.
- **Image-pull and proxy.** The builder pod pulls
  `ghcr.io/tenstorrent/tt-k8s-driver-manager-tools` and `git clone`s
  tt-kmd from `github.com`. In proxied clusters, set the
  `controller.extraEnv` chart value (added in
  [#31](https://github.com/tenstorrent/tt-k8s-driver-manager/pull/31))
  to propagate `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` from the
  controller into the spawned builder pod — without that the builder
  hangs on the git clone.
- **First switchover is slower than steady state.** The first reconcile
  has to pull the builder image, clone tt-kmd, and run `make modules`
  against host kernel headers — a few minutes per node. Subsequent
  reconciles (version bumps, pod restarts) hit `/var/cache/tt-kmd/` and
  finish in seconds.
- **`refcnt > 0` means something's still holding `/dev/tenstorrent`.**
  Don't reach for `rmmod -f`; find the holder. Common culprits:
  `tt-smi`, `tt-telemetry`, an FM agent, a stuck inference pod. Use
  `lsof /dev/tenstorrent/*` and `fuser -v /dev/tenstorrent/*` to find
  the PID, stop it, then re-run the vacate script.

## Rollback / safety net

If the operator-managed install misbehaves on a migrated node and you
want to fall back to DKMS quickly:

```bash
# 1. Stop the operator from reconciling against this node.
kubectl patch ttdp <name> --type merge -p '{"spec":{"paused":true}}'

# 2. On the node, unload the operator's kmd and reinstall via DKMS.
sudo rmmod tenstorrent
sudo apt install --reinstall tenstorrent-dkms    # or: dkms install tenstorrent/<v>
sudo modprobe tenstorrent
```

`paused: true` is load-bearing here: it stops the controller from
fighting the manual DKMS reinstall. Once `dkms install` re-creates
`/var/lib/dkms/tenstorrent`, the builder pod on the next reconcile will
re-detect host-managed mode and stand down — but only if the CR is
unpaused with the host already in host-managed state, so flip
`paused: false` only *after* you've confirmed `install-mode=host` on the
node label.

To take a node out of driver-manager reconciliation entirely (not just
"operator detected host ownership"), set the
[skip label](driver.md#skip-label) instead: `kubectl label node <name>
driver.tenstorrent.com/skip=true`.

## See also

- [driver.md → Mixed mode](driver.md#mixed-mode) — the detection logic
  from the operator's side.
- [troubleshooting.md → Fully clean a host](troubleshooting.md#fully-clean-a-host)
  — a broader sweep that also clears operator-side state (cache,
  tt-smi, etc.); the vacate script above is the DKMS-only subset.
- [`controller.extraEnv`](../charts/tt-k8s-driver-manager/README.md) —
  proxy env propagation to spawned builder pods (PR
  [#31](https://github.com/tenstorrent/tt-k8s-driver-manager/pull/31)).
- [tt-operator integration test](https://github.com/tenstorrent/tt-operator/blob/main/.github/workflows/integration-rke2.yaml)
  — exercises the vacate-and-rebuild sequence in CI on a single-node
  N150 runner; canonical reference for the exact shell commands.
