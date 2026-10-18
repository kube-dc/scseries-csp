# Node requirements

Every node that may attach SC volumes needs the following.

## Packages and HPE node plugin

- `multipath-tools` (multipathd running) and `sg3-utils`.
- The chart configures the HPE node plugin with `disableNodeConformance`, `disableNodeConfiguration` and `disableNodeMonitor`. It therefore installs no packages, does not rewrite `/etc/multipath.conf`, and never removes multipath devices it does not own. The settings below are the node's responsibility.

## Multipath

1. **Finite queueing and WWID exceptions.** The built-in `COMPELNT` entry queues I/O forever on total path loss, which hangs pods, VMs and CSI unstage. Generate a drop-in per array serial:
   ```bash
   hack/multipath-config.sh <ssn> [<ssn>...] | sudo tee /etc/multipath/conf.d/sc-series.conf
   sudo multipathd reconfigure
   ```
   - `no_path_retry 24`: about 2 minutes of queueing, then I/O errors.
   - A `blacklist_exceptions` WWID entry for the array's volumes (`^36000d31<serial hex>00`), which excludes vVol protocol endpoints.
2. **Images that claim nothing by default** (`find_multipaths "strict"` plus `blacklist { wwid ".*" }`, a safe choice when hosts see other arrays' LUNs) also need [deploy/node/multipath-fc-worker.conf](../deploy/node/multipath-fc-worker.conf) (`find_multipaths greedy`). Otherwise CSI-attached volumes never get a dm device. With `on` instead of `greedy`, a volume seen on a single path also gets none.
3. **Verify:**
   ```bash
   multipath -v3 -d 2>&1 | grep -E 'whitelisted|blacklisted'
   ```
   SC volume paths should be whitelisted, and everything else (other arrays, local disks) blacklisted.

## Container runtime (KubeVirt / CDI)

KubeVirt virt-launcher and the CDI importer run as non-root. For raw block PVCs, containerd must hand device ownership to the pod:

```toml
# containerd 2.x (RKE2: /var/lib/rancher/rke2/agent/etc/containerd/config-v3.toml.tmpl)
[plugins."io.containerd.cri.v1.runtime"]
  device_ownership_from_security_context = true
```

Without it, importer and VM pods fail with "Permission denied" on `/dev/...`.

## Kernel limits

```
fs.inotify.max_user_instances = 8192
```

At the default 128, KubeVirt's virt-handler device plugins fail ("too many open files") and the node is marked `kubevirt.io/schedulable=false`.

## Topology label

The HPE node plugin reports topology only from the label `csi.hpe.com/zone`. Without it, provisioning fails with "no topology key found for node".

```bash
kubectl label node <node> csi.hpe.com/zone=<value>
kubectl -n <ns> delete pod -l app=hpe-csi-node --field-selector spec.nodeName=<node>
```

Label only nodes with working FC access. Use the same value in the StorageClass `allowedTopologies`.

## KubeVirt live migration

- VMs with `host-model` CPUs cannot migrate between CPU generations. Set a common model, either in the KubeVirt CR (`spec.configuration.cpuModel`) or per VM (`spec.domain.cpu.model`). The node label `cpu-model.node.kubevirt.io/<model>` shows what each node offers.
- VM disks must be RWX raw block. The chart's CDI integration makes that the default for DataVolumes on its StorageClasses.
- Set `evictionStrategy: LiveMigrate` on VMs.
