# kube-dc profile

[Kube-DC](https://kube-dc.com) uses scseries-csp for KubeVirt VM disks and container PVCs on SC Series arrays. [charts/scseries-csp/values-kube-dc.yaml](../charts/scseries-csp/values-kube-dc.yaml) carries the platform choices; nothing in the CSP depends on kube-dc.

| Setting | kube-dc choice | Why |
|---|---|---|
| `csp.volumeFolder` / `serverFolder` | `kube-dc` | one folder pair per platform on shared arrays |
| StorageClass `scseries-block` | `allowedTopologies` on `csi.hpe.com/zone=sc` | only FC-labelled nodes attach SC volumes |
| `cdi.storageProfiles.enabled` | true | VM DataVolumes default to RWX raw block (live migration) |
| `serviceMonitor`, `prometheusRule` | enabled | CSP and array health in platform monitoring |
| `audit.enabled` | true | hourly consistency check |

Site specifics (array address, certificate pin, credentials via `existingSecret`, storage profiles per tier) belong in a separate values file that is not committed:

```bash
helm upgrade --install scseries-csp charts/scseries-csp -n hpe-storage --create-namespace \
  -f charts/scseries-csp/values-kube-dc.yaml -f site.yaml
```

Node preparation (multipath drop-ins, containerd device ownership, inotify, the zone label) belongs in the node image or provisioning; see [node-requirements.md](node-requirements.md).
