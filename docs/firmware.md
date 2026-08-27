# Firmware Management

The firmware controller flashes Tenstorrent device firmware via per-node
`Job` pods that run `tt-flash`. State is declared via
`TenstorrentFirmwarePolicy` (short name: `ttfwp`).

## The minimum CR

```yaml
apiVersion: firmware.tenstorrent.com/v1alpha1
kind: TenstorrentFirmwarePolicy
metadata:
  name: default
spec:
  version: "19.8.0"
  nodeAffinity: {}
```

What happens:

1. Controller walks each matched node through a state machine:
   `Pending → (Cordoning → Draining)? → Flashing → Uncordoning → Done`.
2. For each node, a Job is created in the operator namespace using the
   flasher image (`ghcr.io/tenstorrent/tt-k8s-driver-manager-flasher`).
   The Job:
   - Reads pre-flash version via `tt-smi -s`.
   - Downloads `fw_pack-<version>.fwbundle` from
     `github.com/tenstorrent/tt-system-firmware` releases.
   - Runs `tt-flash --no-color flash <bundle>` (with `--force` if
     `spec.flasher.forceWrite=true`).
   - Asserts post-flash readback equals `spec.readbackVersion` (default
     `<version>.0` to match the firmware bundle's readback format).
3. Job's exit code is the controller's signal — no separate readback
   step in the reconcile loop. A non-zero exit moves the node to
   `Failed` with the Job's last log lines surfaced in CR status.

## Spec fields

| Field | Default | Purpose |
|---|---|---|
| `version` | required | Firmware bundle version. `^[0-9]+\.[0-9]+\.[0-9]+$`. |
| `readbackVersion` | `<version>.0` | What `tt-smi -s` should report post-flash. Override if a bundle's filename version doesn't match its readback. |
| `bundleURL` | github tt-system-firmware release | Pin to a specific URL (mirror, internal repo, signed copy). |
| `nodeAffinity` | required | Same shape as the driver CR. The v1alpha1 alias `nodeSelector` accepts the same shape and is deprecated. |
| `paused` | `false` | Soft stop. In-flight Jobs not interrupted; new ones don't start. |
| `upgradePolicy.maxParallel` | `1` | Nodes flashing simultaneously across this CR. Crank up only if a bad fw bundle can't brick the fleet faster than you can `paused: true`. |
| `upgradePolicy.haltOnFailure` | `true` | Halt the rollout the moment any node hits `Failed`. Set `false` to keep flashing the rest of the matched nodes. |
| `upgradePolicy.flashTimeoutSeconds` | `900` | Per-node Job timeout. PCIe-only typically <120s; Galaxy headroom. |
| `upgradePolicy.drain.enable` | `true` | Cordon+drain pods that hold `/dev/tenstorrent` before flashing. See [Drain semantics](#drain-semantics). |
| `upgradePolicy.drain.timeoutSeconds` | `600` | Per-node drain timeout. After this, node moves to `Failed` with the blocking pod list. |
| `upgradePolicy.drain.force` | `false` | Delete pods that have no controller (bare Pods) instead of evicting. |
| `flasher.image` | chart's `flasher.image` | Per-CR override of the flasher image. |
| `flasher.imagePullPolicy` | `IfNotPresent` | Override for the above. |
| `flasher.forceWrite` | `false` | Bypass the "current readback already matches target" short-circuit and pass `--force` to tt-flash. Use for re-flashing the same version, downgrades, or suspected silent ROM corruption. |
| `flasher.continueOnReadbackFailure` | `false` | Continue with the flash even if `tt-smi` pre-flash readback fails (chip wedged / driver detached). Independent of `forceWrite`: a chip that subsequently recovers and reports the target version will still skip the flash unless `forceWrite` is also set. |

## CR examples

### Full-fleet flash

```yaml
apiVersion: firmware.tenstorrent.com/v1alpha1
kind: TenstorrentFirmwarePolicy
metadata: { name: fleet }
spec:
  version: "19.8.0"
  nodeAffinity: {}
  upgradePolicy:
    maxParallel: 1     # one node at a time — bad fw shouldn't lose the cluster
    drain:
      enable: true
      timeoutSeconds: 600
```

### Force re-flash (same version)

```yaml
spec:
  version: "19.8.0"
  flasher:
    forceWrite: true
```

`forceWrite` bypasses the "already at target" short-circuit and passes
`--force` to tt-flash — overwrite, readback re-asserts.

### Downgrade

```yaml
spec:
  version: "19.7.0"
  flasher:
    forceWrite: true     # 19.8.0 → 19.7.0 needs --force
  upgradePolicy:
    maxParallel: 1       # downgrade is the riskiest direction; serial
```

### Mirror / pinned bundle

```yaml
spec:
  version: "19.8.0"
  bundleURL: "https://internal.example.com/fw/fw_pack-19.8.0.fwbundle"
  readbackVersion: "19.8.0.0"   # explicit; helps when bundle metadata is odd
```

## Drain semantics

When `upgradePolicy.drain.enable: true` (default), the per-node state
machine walks: `Pending → Cordoning → Draining → Flashing → Uncordoning
→ Done`.

- **Cordoning** sets `node.spec.unschedulable=true` plus our annotation
  `firmware.tenstorrent.com/cordoned-by=<crname>`. The annotation is
  load-bearing: we only uncordon nodes WE cordoned, never stomping on
  an external maintenance window's cordon.
- **Draining** identifies "device-using pods" by hostPath mount on
  `/dev/tenstorrent` and evicts them via the policy/v1 Eviction
  subresource (PDB-respecting; 429s on PDB-block are surfaced as
  transient status with retry on next reconcile). Excludes the
  operator's own namespace and DaemonSet-owned pods.
- **Flashing** is the actual Job described above.
- **Uncordoning** removes the unschedulable flag + our annotation.

### Drain treadmill caveat

Deployment-managed pods with `tolerations: [{operator: Exists}]` bypass
cordon (the implicit unschedulable taint is tolerated). Eviction
succeeds; the deployment controller respawns the pod on the same
cordoned node; eviction loops until `drain.timeoutSeconds` fires. On
timeout the node moves to `Failed` with the blocking pod list in
status.

Workaround: don't give workloads `tolerations: Exists` unless you have
to. Or set `drain.enable: false` on this CR (and accept that flashing
may race against in-flight workloads).

### Skipping drain on a specific node

`kubectl label node <name> firmware.tenstorrent.com/skip=true` — opts
that one node out of *all* firmware reconciliation regardless of
selector. Separate from the driver-side `driver.tenstorrent.com/skip`.

## Upgrade flow

Same pattern as the driver: patch `spec.version`. Per-node Jobs roll
through with whatever parallelism + drain config is set:

```bash
kubectl patch ttfwp default --type merge -p '{"spec":{"version":"19.9.0"}}'
```

The controller is **idempotent at the Job level**: a Job for
`(CR, node, version)` is created at most once. If the same flash is
re-requested (e.g. you `kubectl delete pod` a stuck flasher) the
Complete Job is reused as evidence that this node is done.

### Watch progress

```bash
$ kubectl get ttfwp default
NAME      VERSION   MATCHED   UPTODATE   INPROGRESS   FAILED   AGE
default   19.9.0    3         2          1            0        3m

$ kubectl get ttfwp default -o jsonpath='{.status.nodes}' | jq
[
  {"name":"node-1","currentVersion":"19.9.0.0","state":"Done",
   "reason":"FlashSucceeded"},
  {"name":"node-2","currentVersion":"19.9.0.0","state":"Done",
   "reason":"FlashSucceeded"},
  {"name":"node-3","currentVersion":"19.8.0.0","state":"Flashing",
   "reason":"Flashing",
   "lastFlashJob":"ttfwp-default-node-3-19-9-0-abc1234"}
]
```

`reason` is a stable CamelCase code for *why* a node is in its state —
`FlashJobFailed`, `DrainTimeout`, `RolloutHalted`, `Paused`, and so on.
Every state-or-reason transition also fires a Kubernetes Event against
the CR, so the history is readable without the controller logs:

```bash
$ kubectl describe ttfwp default
...
Events:
  Type     Reason          Age    From                       Message
  ----     ------          ----   ----                       -------
  Normal   Cordoning       3m     tt-k8s-driver-manager      node-3
  Normal   Flashing        2m     tt-k8s-driver-manager      node-3
  Warning  FlashJobFailed  30s    tt-k8s-driver-manager      node-3: Flash Job failed; see Job logs

# Or fleet-wide, filtered by code:
$ kubectl get events --field-selector reason=FlashJobFailed
```

The full code table is in
[troubleshooting.md](troubleshooting.md#why-isnt-my-firmware-flashing).

### Watch the flasher Job

```bash
$ kubectl -n tt-operator-system get jobs -l firmware.tenstorrent.com/cr=default
NAME                                       STATUS    COMPLETIONS   DURATION
ttfwp-default-node-1-19-9-0-abc1234       Complete  1/1           34s
ttfwp-default-node-2-19-9-0-abc1234       Complete  1/1           36s
ttfwp-default-node-3-19-9-0-abc1234       Running   0/1           18s

$ kubectl -n tt-operator-system logs job/ttfwp-default-node-3-19-9-0-abc1234
[flasher] pre-flash: tt-smi -s
[flasher] pre-flash versions: 19.8.0.0 19.8.0.0 19.8.0.0 ...
[flasher] flash: tt-flash --no-color flash --fw-tar /work/bundle.fwbundle
Stage: DETECT (8 chips)
Stage: FLASH (~30s)
...
```

## Node labels + annotations

| Field | Where | Purpose |
|---|---|---|
| `firmware.tenstorrent.com/fw-version` | label | currentVersion after a successful flash |
| `firmware.tenstorrent.com/owned-by` | label | which CR is reconciling this node (first-write-wins) |
| `firmware.tenstorrent.com/upgrade-state` | label | per-node SM position |
| `firmware.tenstorrent.com/current-version` | annotation | readback after last flash |
| `firmware.tenstorrent.com/last-flash-job` | annotation | most recent flash Job name |
| `firmware.tenstorrent.com/cordoned-by` | annotation | the CR that cordoned (so we only uncordon what we cordoned) |
| `firmware.tenstorrent.com/cordoned-at` | annotation | RFC3339 — drain timeout reference |

## kubectl plugin

`kubectl-tt-fw` collapses CR + per-node state + last Job into a single
table. Install via `make install-plugins`.

```bash
kubectl tt fw                # per-CR table
kubectl tt fw logs <crname>  # tail logs from in-flight Jobs
```
