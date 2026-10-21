# Upgrades

Three independent things can be upgraded; the procedure is different
for each.

## tt-kmd

Bump `spec.version` on the `TenstorrentDriverPolicy`:

```bash
kubectl patch ttdp default --type merge -p '{"spec":{"version":"2.10.0"}}'
```

Per-node sequence (full state machine in [Upgrade flow](driver.md#upgrade-flow)):

1. Controller cordons the node and flips
   `controller.deployGates` labels (default
   `tenstorrent.com/deploy.tt-telemetry=false`) so sibling DSes that
   hold `/dev/tenstorrent` evict themselves.
2. Two-pass drain: pass 1 evicts hostPath `/dev/tenstorrent` holders;
   pass 2 (`drain.fullNode`, default on) is full `kubectl drain`
   semantics.
3. Controller re-renders the DS template with the new
   `TT_KMD_VERSION` env, stamps a new `template-hash` annotation.
4. K8s rolling update: `maxUnavailable: 1` — one node at a time.
5. New pod's entrypoint: checks `refcnt`; if 0, `rmmod tenstorrent`,
   build from cache or clone + `make modules`, `insmod`.
   `upgradePolicy.forceUnload=true` SIGKILLs remaining holders via
   `/proc/*/fd` walk before `rmmod`.
6. Readiness probe (`/sys/module/tenstorrent/version` matches expected)
   becomes True; controller uncordons and removes the deploy-gate
   labels; rolling update advances.

Wall-clock per node:

- Cache hit (this version was loaded here before): ~5s.
- Cache miss, clone + build: ~30–90s on a Wormhole node with no
  workload.

Downgrade is the same — patch to a lower version. The `.ko` for the
target version is already cached if it's been on this node before;
otherwise the builder clones + makes it fresh.

### Blocked by workload

The pre-upgrade drain (above) normally drops `refcnt` to 0 before the
new builder pod runs. If a holder still survives — e.g.
`drain.enable=false`, or a pod with `tolerations: Exists` that respawns
on the cordoned node — the entrypoint fails loudly with the holder
PIDs and the pod CrashLoops; the CR's `status.summary.failed`
increments. To unblock, either:

- Set `spec.upgradePolicy.forceUnload=true` and let the next reconcile
  SIGKILL the holders (lose in-flight workloads on that node).
- Find + drain the holders manually, then delete the CrashLooping pod
  (`kubectl delete pod ttdrv-...`) to retry.

## tt-smi

tt-smi version is baked into the builder image at image-build time
(`ARG TT_SMI_VERSION`). To upgrade across the fleet, bump the chart's
`driver.image.tag` to a builder image tag built with the new tt-smi
version.

```bash
helm -n tt-k8s-driver-manager-system upgrade tt-k8s-driver-manager \
  oci://ghcr.io/tenstorrent/helm/tt-k8s-driver-manager \
  --reuse-values \
  --set driver.image.tag=sha-newer
```

(Or set `driver.image.repository` too if you're pulling from a mirror.)

Per-node sequence:

1. Controller's DS template gets the new image tag; template hash
   changes; rolling update.
2. New pod's entrypoint writes the binary to a sibling path and
   `rename(2)`s it over `/host/usr/local/bin/tt-smi` — concurrent
   `tt-smi -s` calls on the host see either the old binary or the new
   one, never a partial file.
3. Patches `tt-smi.driver.tenstorrent.com/version` on the node.

## Firmware

Bump `spec.version` on the `TenstorrentFirmwarePolicy`:

```bash
kubectl patch ttfwp default --type merge -p '{"spec":{"version":"19.12.0"}}'
```

Per-node sequence is the state machine described in [Firmware Management](firmware.md):

`Pending → (Cordoning → Draining)? → Flashing → Uncordoning → Done`

Wall-clock per node: ~40s for the Job itself plus drain time if drain
is enabled.

To re-flash the SAME version (e.g. recovery from a corrupted flash),
set `spec.flasher.forceWrite: true`. The controller deletes the
existing Complete Job for that `(CR, node, version)` and creates a
new one.

## The operator itself

Standard Helm upgrade:

```bash
helm -n tt-k8s-driver-manager-system upgrade tt-k8s-driver-manager \
  oci://ghcr.io/tenstorrent/helm/tt-k8s-driver-manager \
  --version 0.2.0   # the chart version, not the tt-kmd version
```

Controller Deployment rolls (1 replica → terminating → new pod up).
During the ~5–10s the controller is down:

- Existing DS pods keep running with their last-applied template.
- The CR is not reconciled — but no in-flight Job is affected.
- Webhooks (if any) are momentarily unavailable; helm/kubectl
  operations on the CRDs queue.

If the new operator changes the DS pod template (e.g. a new mount, a
new env), the rolling update of every DS kicks in as soon as the new
controller leader comes up. That's a fleet-wide pod churn; nothing
on the chip side moves, but expect the DS rollout to run for ~30s ×
number of nodes.

## Operator downgrade

```bash
helm -n tt-k8s-driver-manager-system rollback tt-k8s-driver-manager
```

CRs are unaffected. The controller pod restarts with the previous
image. If the previous version doesn't know about fields you've set
in the CR (e.g. you added a `spec.foo` post-upgrade), that field is
ignored — no data loss, just no enforcement.

## CRD upgrades

The CRDs themselves live in the chart's `crds/` directory. Helm's
convention is that CRDs in `crds/` are installed on first install and
NOT modified on upgrade — to prevent data loss from schema changes
that would invalidate existing CRs.

To pick up a CRD schema change (new field, new validation), apply
manually:

```bash
kubectl apply -f charts/tt-k8s-driver-manager/crds/
```

Or use `helm upgrade --force` — but read the chart's release notes first;
removing a required field from the schema mid-deploy will reject
existing CRs.

## Rolling-update knobs

| Concern | Knob |
|---|---|
| Per-CR parallelism (firmware) | `spec.upgradePolicy.maxParallel` |
| Per-CR parallelism (driver) | DS `maxUnavailable: 1` (hardcoded; one-node-at-a-time is the only sensible default for kernel modules) |
| Per-node timeout (firmware) | `spec.upgradePolicy.flashTimeoutSeconds` |
| Per-node drain timeout | `spec.upgradePolicy.drain.timeoutSeconds` |
| Drain pass 2 on/off (driver) | `spec.upgradePolicy.drain.fullNode` |
| Drain pass 2 label filter (driver) | `spec.upgradePolicy.drain.podSelectorLabel` |
| SIGKILL device holders (driver) | `spec.upgradePolicy.forceUnload` |
| Sibling-DS deploy gates (driver) | chart `controller.deployGates` |
| Soft pause | `spec.paused: true` on the CR (both kinds) |
| Hard pause (per-node) | `driver.tenstorrent.com/skip=true` or `firmware.tenstorrent.com/skip=true` label on the node |
