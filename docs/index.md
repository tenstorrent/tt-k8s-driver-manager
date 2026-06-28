# Driver Manager

The Driver Manager installs, upgrades, and node-scopes the Tenstorrent kernel
driver (`tt-kmd`) and flashes device firmware, all driven by Kubernetes policy
resources. It is one of the components installed by
[tt-operator](https://docs.tenstorrent.com/tt-operator/).

The guides below cover installing it, applying driver and firmware policies,
upgrades, migrating from a DKMS-managed driver, and troubleshooting.

```{toctree}
:maxdepth: 1

install
driver
upgrades
migrating-from-dkms
troubleshooting
configuration
```
