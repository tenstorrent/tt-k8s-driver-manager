# Install

## Prerequisites

Per-node:

- **Linux**, x86_64. Build-tested on Ubuntu 22.04 (jammy) with HWE 6.x kernel.
  Other distros work as long as the operator's base image (currently
  `ubuntu:22.04`) is libc/libssl-compatible with the host — see
  [Troubleshooting → tt-smi can't execute on host](troubleshooting.md#tt-smi-cant-execute-on-host).
- **`linux-headers-$(uname -r)`** installed on the host. The builder pod
  builds tt-kmd against `/lib/modules/$(uname -r)/build`; if the headers
  aren't there the pod CrashLoops with `host has no kernel build tree`.
  Typically `apt install linux-headers-generic-hwe-22.04` or equivalent.
- **`gcc-12`** is the implicit compiler for HWE 6.8 kernels. The builder
  image ships gcc-12; if the host kernel was compiled with a different gcc
  (`cat /proc/version` to check), the build will fail with
  `gcc-12: not found` from the kernel Makefile. Rebuild the builder image
  with the matching gcc version.
- **`feature.node.kubernetes.io/pci-1200_1e52.present=true`** label on
  Tenstorrent nodes. node-feature-discovery emits this automatically — see
  [NFD setup](#nfd-setup) below.

Per-cluster:

- **Kubernetes 1.27+** (anything that supports kubebuilder v1).
- **Helm 3.8+** (for OCI registry support).
- **Ability to pull from `ghcr.io/tenstorrent/*`** — both for container
  images and the chart. See [Image-pull setup](#image-pull-setup).

## Install via Helm (driver-manager only)

```bash
helm install tt-k8s-driver-manager \
  oci://ghcr.io/tenstorrent/helm/tt-k8s-driver-manager \
  --namespace tt-k8s-driver-manager-system --create-namespace
```

This installs:

- The controller-manager Deployment (one pod, leader-elected).
- The CRDs `TenstorrentDriverPolicy` and `TenstorrentFirmwarePolicy`.
- RBAC: a controller `ClusterRole` for managing those CRs + DaemonSets +
  Jobs, plus an installer `ClusterRole` granting per-pod `nodes:patch`
  so the builder can label its own node.
- Two `ServiceAccount`s in the install namespace: one for the controller,
  one for the per-CR builder/flasher pods.

It does **not** install `node-feature-discovery`. Use the
[tt-operator](https://github.com/tenstorrent/tt-operator) umbrella chart
if you want NFD installed for you, or [install NFD separately](#nfd-setup).

## Install via the umbrella chart (driver-manager + NFD)

```bash
helm repo add node-feature-discovery https://kubernetes-sigs.github.io/node-feature-discovery/charts
helm install tt-operator oci://ghcr.io/tenstorrent/helm-charts/tt-operator \
  --namespace tt-operator-system --create-namespace
```

Brings up node-feature-discovery + tt-k8s-driver-manager in one release. The
`tt-k8s-driver-manager.*` block in the umbrella's `values.yaml` is forwarded
to the subchart unchanged.

## Image-pull setup

`ghcr.io/tenstorrent/*` images are private. You need a
`kubernetes.io/dockerconfigjson` secret in the install namespace and the
relevant `ServiceAccount`s patched to use it.

```bash
# Replace <PAT> with a classic PAT (read:packages scope, SAML-authorized
# for the tenstorrent org).
kubectl create secret docker-registry ghcr-pull \
  --namespace tt-k8s-driver-manager-system \
  --docker-server=ghcr.io --docker-username=<your-gh-handle> --docker-password=<PAT>

kubectl -n tt-k8s-driver-manager-system patch sa default \
  --type merge -p '{"imagePullSecrets":[{"name":"ghcr-pull"}]}'
kubectl -n tt-k8s-driver-manager-system patch sa tt-k8s-driver-manager-controller \
  --type merge -p '{"imagePullSecrets":[{"name":"ghcr-pull"}]}'
kubectl -n tt-k8s-driver-manager-system patch sa tt-k8s-driver-manager-installer \
  --type merge -p '{"imagePullSecrets":[{"name":"ghcr-pull"}]}'

# Restart the controller so its pod picks the secret up:
kubectl -n tt-k8s-driver-manager-system rollout restart deploy tt-k8s-driver-manager-controller
```

The classic-PAT-via-SAML requirement comes from GitHub's enterprise SSO
policy on the org, not from this operator.

## NFD setup

If you didn't install via the umbrella, install NFD separately:

```bash
helm repo add node-feature-discovery https://kubernetes-sigs.github.io/node-feature-discovery/charts
helm repo update
helm install nfd node-feature-discovery/node-feature-discovery \
  --version 0.18.3 \
  --namespace node-feature-discovery --create-namespace \
  --set 'worker.config.core.featureSources={pci}' \
  --set 'worker.config.core.labelSources={pci}'
```

Tenstorrent's PCI class (`1200` — Processing Accelerator) is in NFD's
default whitelist, so the `pci-1200_1e52.present` label appears
automatically once NFD's worker is running on a node. The `featureSources`
/ `labelSources` restriction keeps NFD from emitting hundreds of
unrelated labels (CPU, memory, network, etc.) that we don't use.

## Verifying the install

```bash
$ kubectl -n tt-k8s-driver-manager-system get all
NAME                                              READY   STATUS
pod/tt-k8s-driver-manager-controller-...              1/1     Running
deployment.apps/tt-k8s-driver-manager-controller      1/1
```

Then check NFD labelled your Tenstorrent nodes:

```bash
$ kubectl get nodes -L feature.node.kubernetes.io/pci-1200_1e52.present
NAME      STATUS   PRESENT
e01cs01   Ready    true
e01cs02   Ready    true
e01cs03   Ready    true
```

If `PRESENT` is empty on a node that has a Tenstorrent card, NFD isn't
seeing the device — check the NFD worker pod's logs on that node.

Apply a CR to actually install the driver — see [docs/driver.md](driver.md).

## Uninstall

```bash
helm -n tt-k8s-driver-manager-system uninstall tt-k8s-driver-manager
```

What `helm uninstall` removes:

- Controller Deployment + ServiceAccounts + RBAC.

What it does NOT remove (intentional):

- The CRDs (Helm convention: CRDs survive uninstall to avoid losing CRs).
- The DaemonSets the controller created (they were owned by the CRs,
  not by helm). Delete the CRs first if you want full cleanup:
  `kubectl delete ttdp --all`.
- Any state on the hosts: `/var/cache/tt-kmd/*`,
  `/usr/local/bin/tt-smi`. The kernel module stays loaded too. Clean
  these manually if you're tearing down a node — see
  [docs/troubleshooting.md → fully clean a host](troubleshooting.md#fully-clean-a-host).
