# Firmware Flashing Operator — Design

Status: v1 implemented (2026-05-14, kluong@). Initial implementation under
`api/`, `internal/`, `cmd/`, `charts/`, `images/flasher/`. Sections below
reflect what's actually in code; deferred work is called out explicitly so
the next contributor can pick a clean slice.

## 1. Problem

We need to flash specific firmware versions on Tenstorrent cards across a
Kubernetes cluster, declaratively, with the same safety guarantees as the
current Ansible-based workflow but without requiring an operator to babysit a
GitHub Actions run for every change.

Today firmware is flashed by
[`tenstorrent/exabox-infra` workflow `exabox-flash-firmware`][exabox-workflow],
which invokes
[`tenstorrent/tt-ansible` role `tt_firmware`][tt-ansible-firmware]. The flow is:

1. Operator triggers a `workflow_dispatch` with hosts, fw version, optional
   tt-kmd / tt-smi versions, and `confirm_execution=true`.
2. Workflow creates a Kubernetes `Allocation` CR
   (`tenstorrent.com/v1alpha1`, owned by `tt-orchestration`) to reserve the
   nodes — this is the cluster's mutex against CI / dev sessions that also
   want those nodes. Blocks until the nodes are free.
3. Workflow drains SLURM nodes (`scontrol update NodeName=X State=DRAIN`),
   waits for jobs to leave.
4. Ansible roles install/update `tt-kmd` (DKMS), `tt-smi`, `tt-flash` (pip in
   per-tool venvs under `/opt/tenstorrent/tt-tools`, wrappers in
   `/usr/local/bin`), then download the firmware bundle from
   `tt-system-firmware` releases and run
   `tt-flash --no-color flash --fw-tar <bundle> [--force]`.
5. Verifies the flash via `tt-smi -s` JSON snapshot (parses
   `device_info[*].firmwares.fw_bundle_version`).
6. Resumes SLURM nodes; on failure, leaves them drained for manual recovery.
7. Releases the `Allocation` (always, even on failure).

What this design replaces / extends:

- The orchestration layer (steps 1–3, 6, 7) becomes a controller in this
  operator instead of GitHub Actions + Ansible plays.
- The actual flash primitive (step 4) is reused — we still call `tt-flash` and
  `tt-smi`, just from a Job running on the target node instead of via SSH.

## 2. Goals / Non-goals

**v1 (implemented):**
- Declarative `TenstorrentFirmwarePolicy` CRD pinning a fw version on selector-matched nodes.
- Single-node, PCIe-card systems (n150 / n300 / p150 / p300 / Wormhole / Blackhole).
- Bounded parallelism (default 1) so a botched fw version can't simultaneously
  brick the whole cluster.
- Per-node status surface and `kubectl`-friendly printer columns.
- Idempotent at the Job level: a Job for `(CR, node, version)` is created at most
  once; the flasher entrypoint itself short-circuits if the device already
  reports the target readback.
- Node ownership: when two CRs match the same node, first-write-wins via
  `firmware.tenstorrent.com/owned-by` label; the loser reports `Conflict` in status.

**v1 — drain (implemented):**
- `spec.upgradePolicy.drain.enable: true` walks each node through
  Cordoning → Draining → Flashing → Uncordoning before declaring Done.
- Cordon sets `node.spec.unschedulable=true` plus our
  `firmware.tenstorrent.com/cordoned-{by,at}` annotations (so we only
  uncordon nodes *we* cordoned, never stomping on an external maintenance
  window).
- "Device-using pod" identification is hostPath-based: pods that mount
  `/dev/tenstorrent`. Excludes the operator's own namespace, DaemonSet-
  owned pods, and (by default) bare pods with no OwnerReference.
- Eviction uses the policy/v1 Eviction subresource — **PDBs are respected
  automatically**; 429s surface as transient "blocked by PDB" status
  messages and retry on the next reconcile.
- Timeout: `drain.timeoutSeconds` (default 600s) bounds Draining. On
  timeout the node moves to Failed with the blocking pod list in the
  message; cordon stays applied for operator investigation.

**Known caveat — drain treadmill:** Deployment/ReplicaSet-managed pods
with `tolerations: [{operator: Exists}]` *bypass cordon* (the cordon's
implicit `node.kubernetes.io/unschedulable:NoSchedule` taint is tolerated
by Exists). Eviction succeeds; the controller respawns the pod on the
same cordoned node; eviction loops until timeout. **Workaround:** don't
use `Exists` tolerations on workloads that should respect drain. NVIDIA
sidesteps this by applying a custom taint (`nvidia.com/gpu-driver-upgrade`)
that workloads are unlikely to tolerate; we may add the same for v2.

**v1 — driver scope (minimal):**
- The chart ships a privileged `DaemonSet` (`tt-operator-driver`) that
  installs `tt-kmd` on each NFD-labeled node via DKMS, then sleeps.
  See `images/driver/install.sh` — it's a thin shim around what
  `tt-ansible/roles/tt_kmd` does on bare metal.
- No `TenstorrentDriver` CRD yet; version is set via chart values
  (`driver.version`). When heterogeneous driver versions across the
  cluster become a thing, promote this to a CRD with the same
  per-pool-selector shape as `TenstorrentFirmwarePolicy`.
- The flasher Job assumes `/dev/tenstorrent` exists — if the driver
  DaemonSet hasn't finished installing yet on a node when a flash Job
  spawns, the flash will fail with "No Tenstorrent driver detected".
  Acceptable for v1; future work can gate the flash Job on a node-readiness
  annotation written by the driver pod.

**v2+ — documented hooks, not implemented:**
- Galaxy / TG out-of-band firmware (HTTP API to galaxy host, plus
  `tt-topo` post-flash mesh config).
- LLMBox `tt-topo` post-flash.
- `TenstorrentDriver` CRD with per-pool driver-version selection (NVIDIA
  `NVIDIADriver` shape).
- Tensix harvesting bundle regeneration (`tt-update-tensix-disable-count`).
- Operator-managed `tt-flash` / `tt-smi` host installs (the flasher image is
  the only consumer, so we ship them inside it).
- Drift probing without flashing — see §5.7.

**Explicitly out of scope:**
- Workload-aware drain (e.g. waiting for tt-metalium jobs to finish gracefully
  rather than being SIGKILL'd). Use a `PodDisruptionBudget` per workload owner.

## 3. References

- NVIDIA GPU Operator — driver upgrade controller. We borrow:
  - Node label state machine (`nvidia.com/gpu-driver-upgrade-state` → values
    `upgrade-required`, `cordon-required`, `drain-required`,
    `pod-restart-required`, `validation-required`, `uncordon-required`,
    `upgrade-done`, `upgrade-failed`).
  - `maxParallelUpgrades` and a drain config block with timeout / force /
    podSelector.
  - The "skip this node" opt-out label.
  - DaemonSet-or-Job-per-node for the privileged work, not the controller pod.
- NVIDIA GPU Operator — what we **don't** borrow:
  - The `ClusterPolicy` god-object. We have one operand (the flash Job), not
    eight (driver, toolkit, device plugin, MIG manager, DCGM, etc.). A small
    CRD beats a 2000-line ClusterPolicy.
  - Validator chains and the `gpu-feature-discovery` companion. NFD already
    labels Tenstorrent nodes for us; a single readback check is enough
    validation.
- Tenstorrent Allocation Controller (`tenstorrent.com/v1alpha1.Allocation`,
  in tt-orchestration): mentioned for historical context — today's exabox fw
  flow reserves nodes via this CR before invoking Ansible. The operator does
  **not** integrate with it in v1; cluster-wide reservation coordination is a
  future concern (see §10).

## 4. CRD: `TenstorrentFirmwarePolicy`

- API group / version: `firmware.tenstorrent.com/v1alpha1`
- Kind: `TenstorrentFirmwarePolicy` (short: `ttfwp`)
- Scope: **Cluster** (selects nodes via `nodeSelector`)

Multiple `TenstorrentFirmwarePolicy` resources may coexist (e.g. different
node-pools at different versions). A node matched by more than one is an
error — the controller refuses to act on it and reports
`Conflict` in status.

### 4.1 Example

```yaml
apiVersion: firmware.tenstorrent.com/v1alpha1
kind: TenstorrentFirmwarePolicy
metadata:
  name: prod-blackhole
spec:
  version: "19.8.0"          # required; bundle filename derived as fw_pack-19.8.0.fwbundle
  # readbackVersion: "19.8.0.0"   # optional; default is <version>.0 (matches tt-ansible)
  # bundleURL: ""                  # optional override; default uses tt-system-firmware release URL

  # User-facing selector — pool / role labels.
  # The operator implicitly AND's in the Tenstorrent NFD label
  # (feature.node.kubernetes.io/pci-1200_1e52.present=true) so the head node
  # can't be hit by accident even with a wide selector. See §5.4.
  nodeSelector:
    matchLabels:
      node-role.tenstorrent.com/pool: blackhole

  # Pause everything — useful for emergencies / debugging without deleting the CR.
  paused: false

  # tt-flash --force. Bypasses the device-side version check.
  force: false

  upgradePolicy:
    autoUpgrade: true              # if false, only reports drift; doesn't act
    maxParallel: 1                 # nodes flashing simultaneously across this CR
    flashTimeoutSeconds: 900       # per-node Job timeout. 900s matches tt-ansible (Galaxy headroom); PCIe-only typically <120s.
    drain:
      enable: true                 # cordon + drain k8s pods before flash
      force: false                 # delete pods without controllers
      deleteEmptyDir: true
      timeoutSeconds: 600
      podSelector: {}              # optional; restrict eviction set

  # Dev-mode escape hatches. Omit in prod.
  flasher:
    image: ""                      # override; defaults to the operator's bundled flasher image
    imagePullPolicy: ""            # set to Always while iterating on the flasher entrypoint

status:
  observedGeneration: 3
  desiredVersion: "19.8.0"
  conditions:
    - type: Progressing
      status: "True"
      reason: NodesUpgrading
      message: "2/5 nodes upgrading"
    - type: Ready
      status: "False"
      reason: NodesPending
  summary:
    matched: 5
    upToDate: 2
    inProgress: 2
    failed: 0
    pending: 1
  nodes:
    - name: bh-glx-b04u02
      currentVersion: "19.8.0.0"
      state: UpgradeDone
      lastTransitionTime: "2026-05-14T18:00:01Z"
    - name: bh-glx-b04u03
      currentVersion: "19.7.0.0"
      state: DrainRequired
      message: "Waiting for pod prod/sft-worker-7 to evict"
      lastTransitionTime: "2026-05-14T18:02:11Z"
```

### 4.2 Field notes

- **`version` vs `readbackVersion`** — `tt-flash` is told `19.8.0`; `tt-smi`
  reports back `19.8.0.0` (the trailing `.0` is the build counter). We mirror
  the tt-ansible default of `<version>.0` and let the user override when a
  firmware release breaks the convention.
- **`bundleURL`** lets us point at an artifact repo / internal mirror for
  air-gapped clusters. Default is computed:
  `https://github.com/tenstorrent/tt-system-firmware/releases/download/v{version}/fw_pack-{version}.fwbundle`.
- **`paused: true`** is the operator's emergency brake. Controller stops
  reconciling new transitions but leaves whatever state nodes are in alone.
- We intentionally do **not** expose `tt_kmd_version`, `tt_smi_version`,
  `tt_flash_version` on the CRD in v1. The flash Job pins its own
  `tt-smi` / `tt-flash` (baked into the image), and `tt-kmd` lives outside
  the operator's purview (managed by Ansible / MAAS).

### 4.3 Printer columns

`kubectl get ttfwp` returns:

```
NAME             VERSION   MATCHED  UPTODATE  INPROGRESS  FAILED  AGE
prod-blackhole   19.8.0    5        2         2           0       4m12s
```

## 5. Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  control plane                                                   │
│  ┌─────────────────────────────────────────────────────────────┐ │
│  │ tt-operator controller manager                              │ │
│  │  - watches: TenstorrentFirmwarePolicy, Node, Job           │ │
│  │  - reconciles: matches nodes, drives per-node state machine│ │
│  │  - writes: node labels/annotations, spawns Jobs            │ │
│  └─────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────┘
                              │ creates one Job per node, per upgrade
                              ▼
┌──────────────────────────────────────────────────────────────────┐
│  worker node (matches CR nodeSelector ∧ NFD pci-1200_1e52.present) │
│  ┌─────────────────────────────────────────────────────────────┐ │
│  │ flash Job (privileged, pinned to node)                      │ │
│  │  - initContainer: download bundle to shared emptyDir         │ │
│  │  - main: tt-fw-flasher (tt-flash + tt-smi)                  │ │
│  │  - mounts: /dev/tenstorrent, hugepages, /sys                │ │
│  │  - runs tt-flash flash --fw-tar <bundle>                    │ │
│  │  - readback via tt-smi -s and exits 0/non-0                 │ │
│  └─────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────┘
```

### 5.1 Per-node state machine

The controller drives each node through a small state machine. The source of
truth is observable cluster state (Job existence + status, node labels) — the
state label is denormalized for `kubectl get nodes -L` ergonomics, not the
authority.

**v1 — 4 states.** The intermediate cordon/drain states from NVIDIA's GPU
operator are intentionally collapsed until that subsystem is wired up.

| State | Meaning | Transition out |
|---|---|---|
| `Pending` | Matched but no Job exists for `(CR, node, spec.version)` yet, or pre-Job gating (drain/alloc) hasn't completed | `Flashing` once advanceNode creates the Job |
| `Flashing` | Job exists and is not yet Complete or Failed | `Done` on Job success; `Failed` on Job failure |
| `Done` | Job for the current `spec.version` completed successfully | Re-enters `Pending` if `spec.version` changes (new Job name) |
| `Failed` | Job completed non-zero | Operator deletes the Job (or fixes underlying cause); next reconcile re-derives state |

Per-node label: `firmware.tenstorrent.com/upgrade-state` carries the v1 state.

Per-node annotations:
- `firmware.tenstorrent.com/desired-version` — denormalized from the CR.
- `firmware.tenstorrent.com/last-flash-job` — most recent Job name for log lookup.
- `firmware.tenstorrent.com/current-version` — reserved for v2 (see §5.7).

Opt-out: `firmware.tenstorrent.com/skip=true` makes the controller ignore the
node entirely (matches NVIDIA `gpu-driver-upgrade.skip`).

**v2 expansion path.** When drain gets implemented, `Pending` splits into a
sequence of observable gating states:

```
Pending → Cordoning → Draining → Flashing → Uncordoning → Done
                                          ↘ Failed
```

The label name stays the same, the values just gain new variants. Existing
v1 deployments transitioning from old labels to new is a no-op since `Done`
remains `Done` and any in-flight `Flashing` finishes through to its terminal
state regardless of the additional gating states ahead of `Flashing`.

### 5.2 Reconciliation loop

Per `TenstorrentFirmwarePolicy`, what `Reconcile()` actually does (matches
`internal/controller/firmware_policy_controller.go`):

```
1. Resolve matched nodes: spec.nodeSelector AND NFD label
   (feature.node.kubernetes.io/pci-1200_1e52.present=true unless
   REQUIRE_TT_PCI_LABEL=false), minus nodes carrying the skip label.
2. Partition by ownership label (firmware.tenstorrent.com/owned-by):
   - Matches our CR name (or unset) → owned, ours to drive.
   - Set to a different CR name → conflict, surface in status, don't act.
3. For each owned node, observe state:
   - Look up Job by deterministic name ttfwp-<cr>-<node>-<verHash>.
   - No Job → Pending. Job not done → Flashing. Job Complete → Done.
   - Job Failed → Failed.
4. Compute capacity = spec.upgradePolicy.maxParallel - count(Flashing).
5. For up to `capacity` Pending nodes (skipping conflicts), advanceNode():
   - Claim ownership (idempotent label patch).
   - If drain.enable: refuse (v1 stub), keep node Pending with a clear
     status message.
   - Otherwise: create the Job (idempotent on AlreadyExists).
6. Re-aggregate summary from final per-node state.
7. Write status (with Ready / Progressing conditions).
8. Requeue after 30s if anything is Pending or Flashing.
```

Each step idempotent and gated on observable conditions — controller crashes
mid-flow resume cleanly because the next reconcile re-derives "where is this
node" from cluster state, not from in-memory state. Watches on
`TenstorrentFirmwarePolicy`, owned `Job`s, and `Node`s give us prompt
re-reconciliation on the only events that matter; the 30-second fallback
covers Job conditions that land without firing a watch event.

### 5.3 Flash Job design

One Job per node per upgrade. Garbage-collected via owner ref on the
`TenstorrentFirmwarePolicy`.

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: ttfwp-<crname>-<node>-<gen>
  labels:
    firmware.tenstorrent.com/cr: prod-blackhole
    firmware.tenstorrent.com/node: <node>
  ownerReferences:
    - apiVersion: firmware.tenstorrent.com/v1alpha1
      kind: TenstorrentFirmwarePolicy
      name: prod-blackhole
spec:
  backoffLimit: 0                     # don't auto-retry; controller decides
  activeDeadlineSeconds: 900          # spec.upgradePolicy.flashTimeoutSeconds
  ttlSecondsAfterFinished: 86400      # keep logs for a day
  template:
    spec:
      restartPolicy: Never
      hostNetwork: true               # for galaxy BMC reach in v2
      nodeName: <node>
      tolerations: [ ... ]            # NoExecute toleration for the upgrade taint
      initContainers:
        - name: fetch-bundle
          image: curlimages/curl:latest
          command: ["sh","-c","curl -fsSL \"$TT_FW_BUNDLE_URL\" -o /work/bundle.fwbundle"]
          env:
            - { name: TT_FW_BUNDLE_URL, value: "https://.../fw_pack-19.8.0.fwbundle" }
          volumeMounts:
            - { name: work, mountPath: /work }
      containers:
        - name: flash
          image: ghcr.io/tenstorrent/tt-fw-flasher:<operator-version>
          imagePullPolicy: IfNotPresent
          securityContext:
            privileged: true
            capabilities: { add: ["SYS_RAWIO", "SYS_ADMIN"] }
          env:
            - { name: TT_FW_BUNDLE_PATH, value: "/work/bundle.fwbundle" }
            - { name: TT_FW_READBACK,    value: "19.8.0.0" }
            - { name: TT_FLASH_ARGS,     value: "" }       # "--force" if spec.force
            - { name: TT_FORCE,          value: "false" }  # spec.force
          volumeMounts:
            - { name: work,      mountPath: /work, readOnly: true }
            - { name: dev,       mountPath: /dev/tenstorrent }
            - { name: hugepages, mountPath: /dev/hugepages }
            - { name: sys,       mountPath: /sys }
      volumes:
        - { name: work,      emptyDir: {} }
        - { name: dev,       hostPath: { path: /dev/tenstorrent } }
        - { name: hugepages, hostPath: { path: /dev/hugepages } }
        - { name: sys,       hostPath: { path: /sys } }
```

The initContainer / main split means the flasher image stays minimal
(`tt-flash`, `tt-smi`, a tiny entrypoint shell) and bundle fetching is
swappable — air-gapped clusters can replace `curlimages/curl` with an image
that pulls from an internal artifact store, or a hostPath volume can stand
in for the emptyDir to share a cached bundle across re-runs.

Image: `ghcr.io/tenstorrent/tt-fw-flasher` — a thin container with `tt-flash`
and `tt-smi` baked in plus an entrypoint that:

1. Runs `tt-smi -s` to capture pre-flash state. If this fails and
   `$TT_FORCE != true`, exits non-zero. (Matches tt-ansible's `rescue`
   semantics: bad device state is fatal unless the operator explicitly opted
   into force.)
2. Runs `tt-flash --no-color flash --fw-tar $TT_FW_BUNDLE_PATH $TT_FLASH_ARGS`.
   **Treats stdout containing `Config space reset not completed for device`
   as a failure even when the exit code is 0** — this matches the explicit
   `failed_when` in the tt-ansible flash task; without it a known-bad
   condition slips past the readback.
3. Runs `tt-smi -s` again and asserts **every** entry in
   `device_info[*].firmwares.fw_bundle_version` equals `$TT_FW_READBACK`.
   A node with multiple cards is all-or-nothing: any laggard fails the Job.
4. Exits 0 on match, non-zero otherwise. Pre/post snapshots emitted as
   structured stdout for log scraping.

The entrypoint pins minimum `tt-smi` / `tt-flash` versions in the image at
build time (matching `min_tt_smi_version` / `min_tt_flash_version` in
`tt_firmware/defaults`). Bumping these means a new flasher image tag.

This is intentionally **the same primitive `tt-ansible` runs**, just packaged
as a one-shot container that runs on the node it targets rather than over SSH.
The Ansible roles `tt_flash` / `tt_firmware` install the venv + wrapper on the
host; we ship the same Python tools inside an image and skip touching the
host filesystem. Less host state to reason about, faster cold-start on
clusters that haven't been hand-provisioned.

### 5.4 NFD integration

The chart bundles `node-feature-discovery` and relies entirely on NFD's
native PCI labeling — same pattern as NVIDIA's gpu-operator. No custom
`NodeFeatureRule` is shipped.

NFD's PCI source emits `feature.node.kubernetes.io/pci-<class>_<vendor>.present=true`
for every device whose class is in its `deviceClassWhitelist`. Class `12`
(Processing Accelerator) — where Tenstorrent sits — is in NFD's upstream
default allowlist, so base NFD picks up our cards without configuration.

The controller looks for the label `feature.node.kubernetes.io/pci-1200_1e52.present=true`
(class `1200` = class+subclass, vendor `1e52` = Tenstorrent — both should
be verified with `lspci -nn` on a real card before shipping). It refuses
to act on nodes that don't carry this label, even if they match
`spec.nodeSelector`, so a typo'd selector can't accidentally target the
head node. A controller env var `REQUIRE_TT_PCI_LABEL=false` disables this
guard for the mock dev path (§8.2).

**Why no custom rule?** A custom `NodeFeatureRule` would let us pick a
friendlier label key (e.g. `tenstorrent.present`), but it adds a moving
part: another CRD, another controller (`NodeFeatureRule` is reconciled by
NFD's master), and another API surface that has churned across NFD
versions. NVIDIA's gpu-operator concluded the same thing — they configure
NFD's PCI whitelist if needed and consume the native label. Richer per-card
labels (think Tenstorrent equivalents of `nvidia.com/gpu.product=Tesla-T4`)
would justify a separate "tenstorrent-feature-discovery" DaemonSet
parallel to NVIDIA's GFD — but that's a different problem from "is there
a card here?".

### 5.5 Node ownership

When two `TenstorrentFirmwarePolicy` CRs match the same node (e.g. you accidentally
write two selectors that overlap), the operator needs a deterministic answer
for "who's driving this node". The contract:

- `firmware.tenstorrent.com/owned-by=<crName>` is set on the Node when a CR
  first acts on it (during `advanceNode`).
- Subsequent reconciles by other CRs see the label and short-circuit — the
  node shows up in the *other* CR's status as `Pending` with the message
  `Conflict: node owned by a different TenstorrentFirmwarePolicy CR`.
- The owning CR remains in charge until the node is removed from its
  selector (label change) or the CR is deleted. The operator does **not**
  auto-clear the owned-by label on selector changes — that's an explicit
  operator action (`kubectl label node X firmware.tenstorrent.com/owned-by-`).

This is deliberately simple. Smarter conflict resolution (priority, age, CR
generation) is forward-thinkable but YAGNI — overlap is a configuration bug
and the operator should refuse to act rather than make a choice that might be
wrong.

### 5.6 Drift detection model

The operator does **not** read the current firmware version of a card
without flashing. There's no "probe" Job that runs `tt-smi` and reports
back. Implications:

- The annotation `firmware.tenstorrent.com/current-version` is reserved
  but unwritten in v1 — populating it would require either (a) a probe
  Job with K8s API access to patch its own Node, or (b) parsing Job
  stdout logs, both of which add complexity disproportionate to the
  benefit.
- "Does this node need a flash?" is answered by Job existence — if a Job
  for `(CR, node, spec.version)` exists and is `Complete`, the node is
  considered Done. If the Job doesn't exist, the node is Pending.
- A node whose card was flashed out-of-band (via `tt-flash` on the host)
  before the operator was installed will get a Job spawned anyway. The
  flasher entrypoint runs `tt-smi -s` first and **exits 0 immediately**
  if all devices already report the desired readback (matches
  tt-ansible's behavior). So the operator-side cost of "we don't know
  the truth" is one no-op Job per node per CR-version-change.

This is a conscious simplification. v2 can add a probe-Job mechanism if
the no-op cost ever matters (it shouldn't — fw changes are infrequent).

### 5.7 Drain integration with SLURM (deferred)

The current Ansible flow drains SLURM nodes via `scontrol`. In a pure-k8s
cluster (closetbox, single-node dev) SLURM isn't in the picture and cordon +
drain is enough. For exabox-style clusters that overlay SLURM on k8s, drain
is a separate concern owned by tt-orchestration's job-eviction logic. v1
defers: assume k8s pod eviction is the only drain step, and document that
operators draining SLURM-backed clusters should still bracket the
`TenstorrentFirmwarePolicy` change with a manual `scontrol drain` (or trigger via
tt-orchestration's existing tooling).

A future v2 can add `spec.drainHooks: [{ preFlash: ..., postFlash: ... }]`
to wire SLURM in.

## 6. Out-of-scope, but worth wiring hooks for

These all exist in tt-ansible; we want the operator to make room for them
without baking them in:

- **Tensix disable count (P150 harvesting).** Today the bundle is regenerated
  via `tt-update-tensix-disable-count` before flash. Future
  `spec.bundleTransform: { tensixDisableCount: 1 }` triggers the flash image
  to run the transform inside the container before flashing.
- **Galaxy / TG firmware.** Out-of-band via HTTP API to `http://{galaxy}/api/update/fw`.
  Future `spec.galaxy: { host: ..., credentialsSecret: ... }` makes the flash
  Job target the galaxy controller from one node instead of a card. The flash
  Job already sets `hostNetwork: true` so this is wiring, not architecture.
- **`tt-topo` post-flash.** Wormhole LLMBox / Blackhole multi-card setups need
  `tt-topo` after every fw change. Future `spec.postFlashTopo: { layout: mesh, ranks: ... }`.

## 7. Failure modes

| Scenario | Behavior |
|---|---|
| Flash Job fails | Node moves to `UpgradeFailed`. Controller does **not** advance other nodes past `InProgress` count until operator clears the label. CR status `Ready=False`. |
| Readback mismatch | Same as flash failure. Job stdout / logs preserved via `ttlSecondsAfterFinished=86400`. |
| Controller pod crashes mid-flash | Job continues; on restart the controller re-derives state from labels + Job status. No double-flash because the Job's existence is the lock. |
| Two `TenstorrentFirmwarePolicy` resources match the same node | Both report `Conflict` on the node; neither acts. |
| `tt-smi -s` segfaults / hangs (real failure mode on bad fw) | Job hits an inner timeout (default 60s), exits non-zero, node fails. |
| Card disappears from `/dev/tenstorrent` post-flash (bricked) | Readback fails; node fails. Operator pages on `Ready=False` for >N minutes. |

## 8. Dev / test strategy

This is the part that matters for daily use, because the user's dev cluster
is single-node single-card. Three layers of fidelity:

### 8.1 Envtest (no node, no cards) — milliseconds per run

Use kubebuilder's `envtest` (apiserver + etcd, no kubelet). Covers:
- CRD validation (`make manifests` then schema round-trip).
- Controller reconciliation logic: feed fake Nodes + Job status and assert
  label transitions.
- State-machine table tests.

This is where 90% of bugs get caught. Cycle: edit → `go test ./internal/...`,
under a second.

### 8.2 Kind cluster with a mock flasher — seconds per run

`make kind-dev` brings up a 3-node kind cluster. None of the nodes have a
Tenstorrent card, so we fake it:

- A small `mock-nfd-labeler` job runs once at cluster start and slaps
  `feature.node.kubernetes.io/pci-1200_1e52.present=true` on every worker
  (or set the operator's `REQUIRE_TT_PCI_LABEL=false` env var to skip the
  check entirely on dev clusters).
- The flasher image has a `MOCK=true` env knob that, instead of running
  `tt-flash`, sleeps a configurable duration and writes a fake readback
  string. The operator sees a real Job lifecycle (Pending → Running → Complete)
  and exercises the full state machine end-to-end without touching hardware.
- A unit-tested mode that simulates failure: `MOCK_FAIL_AFTER=2` flashes the
  first 2 nodes then fails — covers `UpgradeFailed` paths.

Cycle: `skaffold dev` rebuilds the controller binary, hot-reloads in 5–10
seconds. Local registry inside kind avoids push round trips.

### 8.3 Single-node dev cluster with a real card — minutes per run

This is your single-node single-card box. Use this only for:
- Validating the flasher image actually flashes (real `tt-flash`).
- Catching driver / hugepages / privileged-pod issues.
- Verifying readback parsing against a real `tt-smi` output.

To make this loop bearable:

1. **Same-version reflash via `force: true`.** Otherwise the controller
   sees a successful Job for the version and no-ops, and you can't exercise
   the flash path repeatedly without bumping versions.
2. **Skip drain on dev:** `upgradePolicy.drain.enable: false`. Avoids
   the controller trying to evict the operator's own pod off your
   single node — which would either fail (PDBs) or deadlock. The
   controller pod itself should also carry the `…/skip=true` node
   tolerance so it's never on the eviction list even if drain is on.
3. **Cache the firmware bundle.** A `bundleURL` of `file:///host/cache/fw_pack-19.8.0.fwbundle`
   (with a hostPath mount) shaves a download off every iteration.
4. **`make e2e-dev`** — applies `hack/dev/ttfwp-mock.yaml`, polls
   `kubectl get ttfwp -w` and `kubectl get job -l firmware.tenstorrent.com/cr=<name> -w`
   side-by-side, attaches to the Job pod's logs as soon as one shows up, and
   on Ctrl-C deletes the CR and cleans up. Replaces five `kubectl` commands
   per iteration.
6. **Iterate on the flasher independently of the controller.** Most dev
   iteration is on either (a) the controller's reconcile logic or (b) the
   flasher entrypoint. They have separate images and separate build cycles.
   `spec.flasher.image` + `spec.flasher.imagePullPolicy: Always` lets you
   point the operator at a dev flasher image without rebuilding the
   controller — important because the flasher is the only thing that
   actually touches hardware, so it's where bugs surface and where you'll
   iterate most.

Galaxy / multi-card configurations are intentionally out of v1 — for those,
plan to test in a shared lab cluster (exabox staging) gated behind a CI job
that runs only on `kluong/sw-operator → main` merges. Lab-time is precious;
the kind+mock path should catch ~all controller bugs first.

### 8.4 Recommended scaffolding

- **kubebuilder** for the operator. Standard, generates CRD + RBAC + main +
  reconciler skeleton in one shot. We get `make manifests`, `make test`,
  envtest, deepcopy generation for free.
- **Skaffold or Tilt** for the inner loop on the operator pod.
- **kind** with a local registry mirror (`registry.localhost:5000`) so
  `docker build` + `kubectl apply` of the controller is <15s.
- **golangci-lint + ginkgo/gomega** matching upstream kubebuilder defaults.
- A `kubectl-tt-fw` plugin (`kubectl tt fw`) that prints per-node state
  one-line-per-node — saves a lot of `kubectl get nodes -L
  firmware.tenstorrent.com/upgrade-state -o wide` typing during iteration.

### 8.5 CI and per-branch images

`.github/workflows/images.yaml` builds and pushes both the controller and
the flasher to GHCR on every push, tagged with:

- The branch name (slashes → dashes; `kluong/sw-operator` → `kluong-sw-operator`).
- The short SHA (`sha-<7>`), immutable.
- `pr-<num>` on PR events.
- Semver on `v*.*.*` tags.
- `latest` only on `main`.

The branch tag is the dev-loop primitive: open a branch, push, and within
a few minutes you can deploy that branch into a test cluster:

```bash
helm upgrade --install tt-operator ./charts/tt-operator \
  --set controller.image=ghcr.io/tenstorrent/tt-operator:my-branch \
  --set flasher.image=ghcr.io/tenstorrent/tt-fw-flasher:my-branch
```

Use the `sha-<7>` tag when you want immutability (the branch tag moves on
each push; the sha tag doesn't). Both images build for `linux/amd64` and
`linux/arm64` so exabox controllers can pull either.

`ci.yaml` runs `go test`, `go vet`, `go build`, `helm lint`, and verifies
that committed generated code (`api/v1alpha1/zz_generated.deepcopy.go`,
`config/crd`, `config/rbac`) matches what `make generate` would produce.

### 8.6 Self-host gotcha on a single-node dev cluster

On a single-node box the controller pod runs on the **same node** it's
flashing. Two things to wire up day-one or you'll get bitten:

- Set `priorityClassName: system-cluster-critical` on the controller
  Deployment so `kubectl drain` skips it. Otherwise drain (when enabled)
  will evict the controller mid-reconcile and you'll get whichever half of
  the state machine ran before eviction.
- Tolerate the upgrade taint if you choose to apply one. v1 doesn't add a
  taint — pure cordon/drain — so this is forward-looking.

Even with both, the safest dev posture on a single-node cluster is
`upgradePolicy.drain.enable: false` and just trust that no workloads are
on the card.

## 9. Open questions

1. **Cluster-scoped vs namespaced CRD.** Cluster-scoped is right because we
   target nodes (cluster resources). Namespacing it would just force us to
   pick an arbitrary namespace. Going with cluster.
2. **Should we own `tt-kmd` lifecycle too?** Tempting — the operator could
   then guarantee a (kmd, fw) tuple. But DKMS + reboot + module unload is a
   much bigger blast radius and is well-handled by MAAS/Ansible today. Leave
   for a `TenstorrentDriver` CRD if/when there's appetite.
3. **Should we share the `tenstorrent.com/v1alpha1` group with the
   Allocation controller?** No. They're separate operators with separate
   release cadences; a separate `firmware.tenstorrent.com` group makes RBAC
   and versioning independent.
4. **What if the node reboots mid-flash?** Same risk as the Ansible flow
   today — `tt-flash` writes to SPI in chunks and a power loss during a
   write can leave the card in a half-flashed state. We do **not** try to
   detect or recover from this; we just don't introduce new ways to cause
   it. The controller never voluntarily reboots a node; the user is
   expected to keep MAAS / power management out of fw-flashing windows.
5. **Coordination with tt-kmd.** Some fw versions require a minimum
   tt-kmd. We don't manage tt-kmd, so if the deployed kmd is too old
   the flash will fail in `tt-smi` with a clear error. Long-term, the
   fw bundle could expose its min-kmd in metadata and we could refuse to
   flash with a clear status message. v2 problem.

## 10. Out-of-band: deliberately simple v1 cuts

To resist scope creep, v1 specifically does **not** do:

- Bundle signing / verification (rely on HTTPS + GitHub release integrity).
- Status history / per-flash audit trail (`ttlSecondsAfterFinished` keeps
  recent Jobs; cluster log retention has the rest).
- Webhook validation (kubebuilder generates CEL validations on the CRD;
  webhooks add an admission failure domain we don't need yet).
- Custom metrics. Prometheus scrape of controller-runtime defaults gives us
  reconcile rate / errors; per-node state can be reconstructed from the
  status object.

When any of these show up as actual pain, we'll know what they should look
like; speculating now would just bloat the CRD.

---

[exabox-workflow]: https://github.com/tenstorrent/exabox-infra/blob/main/.github/workflows/exabox-flash-firmware.yaml
[tt-ansible-firmware]: https://github.com/tenstorrent/tt-ansible/blob/main/roles/tt_firmware/tasks/main.yaml
