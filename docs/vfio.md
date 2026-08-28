# VFIO passthrough

Binding a Tenstorrent device to the `vfio-pci` driver hands it to userspace so
a virtual machine can claim it directly. The Driver Manager ships an optional
`vfio-manage` DaemonSet that performs and maintains that binding on the nodes
you choose.

A device bound to `vfio-pci` is no longer available to `tt-kmd`. Container
workloads on that node cannot use it. Enable this only on nodes set aside for
VM passthrough.

## What it does

On every node it runs on, `vfio-manage`:

1. Loads the `vfio-pci` kernel module if it is not already loaded.
2. Scans `/sys/bus/pci/devices` for devices matching the configured vendor and
   device IDs.
3. Unbinds each match from its current driver, sets `driver_override`, and
   binds it to `vfio-pci`.
4. Repeats the scan every `bindInterval`, so a device that drifts off
   `vfio-pci` — after a driver reload, for example — is rebound without
   restarting the pod.

It performs the binding only. Advertising the bound devices to the kubelet as
a schedulable resource is a separate job; configure whatever does that with
the same device list.

## Enable it

`vfio-manage` is off by default. Turn it on and give it a device list:

```yaml
vfioManager:
  enabled: true
  nodeSelector:
    tenstorrent.com/vfio: "true"
  devices:
    - resourceName: tenstorrent.com/wormhole
      vendorId: "1e52"
      deviceId: ["401e"]
```

```bash
helm upgrade --install tt-k8s-driver-manager \
  oci://ghcr.io/tenstorrent/charts/tt-k8s-driver-manager \
  --namespace tt-system --create-namespace \
  -f values.yaml
```

Label the nodes you want it on, then confirm the pods landed only there:

```bash
kubectl label node <node> tenstorrent.com/vfio=true
kubectl -n tt-system get pods -l app.kubernetes.io/component=vfio-manage -o wide
```

Without a `nodeSelector` the DaemonSet runs everywhere and binds every
matching device in the cluster.

## Verify the binding

Check the driver each device is attached to on a target node:

```bash
lspci -d 1e52: -k
```

Devices in use show `Kernel driver in use: vfio-pci`, and a matching
`/dev/vfio/<group>` character device appears on the host.

The pod logs one line per device it binds:

```bash
kubectl -n tt-system logs -l app.kubernetes.io/component=vfio-manage
```

## Configuration

| Value | Default | Purpose |
|---|---|---|
| `vfioManager.enabled` | `false` | Deploy the DaemonSet. |
| `vfioManager.devices` | `[]` | Vendor and device IDs to bind. Empty binds nothing. |
| `vfioManager.bindInterval` | `30s` | How often to re-assert the binding. |
| `vfioManager.restoreOnExit` | `false` | On `SIGTERM`, return devices to their previous driver. |
| `vfioManager.nodeSelector` | `{}` | Restrict the DaemonSet to passthrough nodes. |
| `vfioManager.metricsPort` | `9401` | Port for `/metrics` and `/healthz`; `0` disables both. |

The full values reference is in [Configuration](configuration.md).

`restoreOnExit` is off deliberately. In steady state the DaemonSet restarts
and re-asserts the binding, and handing devices back to `tt-kmd` on every pod
restart only churns the driver. Turn it on when you want a node to come back
to container workloads cleanly after you disable passthrough.

## Requirements

- **An IOMMU.** Enable VT-d or AMD-Vi in firmware and turn it on in the kernel
  command line (`intel_iommu=on` or `amd_iommu=on`). Without one the module
  falls back to unsafe no-IOMMU mode, which offers no memory isolation between
  the VM and the host; `tt_vfio_noiommu_mode` reports `1` when that happens.
- **`/lib/modules` on the host**, so the module can be loaded. The DaemonSet
  mounts it read-only.
- **A privileged container.** Writing to `/sys/bus/pci` requires it.

## Metrics

`vfio-manage` serves its own endpoint on `metricsPort` — separate from the
controller's, since it runs per node. See [Metrics](metrics.md) for the
families it exports.

## Troubleshooting

**Pods crash-loop with `loading vfio-pci kernel module`.** The module is
missing and `modprobe` could not find it. Confirm `/lib/modules` is mounted
and that the host has `vfio-pci` available for its running kernel.

**A device never binds.** Compare the vendor and device IDs in your config
against what the host reports:

```bash
lspci -d 1e52: -nn
```

The IDs are 4 hex digits with no `0x` prefix, and the match is
case-insensitive.

**A device binds but the VM cannot claim it.** Devices pass through in whole
IOMMU groups. If other devices share a group, they must all be bound to
`vfio-pci` before the group can be assigned. Inspect the groups with:

```bash
find /sys/kernel/iommu_groups -type l
```
