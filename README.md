# scseries-csp

Kubernetes storage for **Dell SC Series** (Compellent) arrays over **Fibre Channel**, built as a Container Storage Provider (CSP) for the [HPE CSI Driver for Kubernetes](https://github.com/hpe-storage/csi-driver).

Developed and maintained by the [Kube-DC](https://kube-dc.com) team.

```
Kubernetes ──CSI──▶ HPE CSI Driver ──HTTP──▶ scseries-csp ──REST──▶ Dell Storage Center (SCOS 7.x) / DSM
                         │                                                    │
                         └────────────── FC multipath (dm-multipath) ────────┘
```

The HPE CSI Driver already handles the Kubernetes side (provisioning, attach, FC rescans, multipath, filesystems, raw block, snapshots). It delegates array operations to a CSP over a documented REST contract. scseries-csp implements that contract for Dell SC Series, so no CSI node code had to be written.

## Features

- Dynamic provisioning of Fibre Channel volumes: filesystem (xfs, ext4) or **raw block**
- **ReadWriteMany raw block**, the basis for **KubeVirt live migration** without disk copies
- Online volume expansion
- CSI snapshots (Storage Center replays), restore from snapshot, and PVC→PVC clones (view volumes)
- Per-StorageClass storage profile, data-reduction profile and snapshot schedule
- Ownership guard: the CSP only creates, maps or deletes objects in its own Storage Center folders
- Works next to other arrays on the same FC fabric and hosts: ports pinned in foreign server objects are skipped, never modified
- HA: 2 replicas with Lease-based leader election and forward-to-leader; stateless sealed session tokens
- Array-health metrics (status, disks, spares, alerts, capacity), Prometheus alerts, and a consistency `audit` with safe repairs
- Helm chart with an optional pinned HPE CSI Driver dependency and an optional KubeVirt/CDI integration

## Compatibility

| Component | Validated |
|---|---|
| Storage Center | SC5020, SCOS 07.02.30 (REST API 3.4, served directly by the array; DSM Data Collector should work too) |
| HPE CSI Driver | 3.3.0 |
| Kubernetes | 1.36 (RKE2), Ubuntu 26.04, kernel 7.0, multipath-tools |
| FC | QLogic 16G HBAs, Brocade fabrics, SC virtual port mode |
| KubeVirt / CDI | 1.9.1 / 1.66.1 |

Other SC Series models and SCOS 7.x releases use the same REST API and should work; reports are welcome.

## Quick start

Prerequisites: FC zoning between the nodes and the array, nodes prepared as in [docs/node-requirements.md](docs/node-requirements.md), and an array account as in [docs/array-setup.md](docs/array-setup.md).

```bash
helm upgrade --install scseries-csp oci://ghcr.io/kube-dc/charts/scseries-csp --version 0.3.0 \
  -n hpe-storage --create-namespace \
  --set array.host=sc.example.com \
  --set array.credentials.username=scseries-csp \
  --set array.credentials.password='...' \
  --set array.tls.fingerprint='AB:CD:...'     # openssl s_client ... | openssl x509 -noout -fingerprint -sha256

kubectl label node <fc-node> csi.hpe.com/zone=sc   # each FC node that may attach volumes
kubectl -n hpe-storage delete pod -l app=hpe-csi-node   # node plugin re-reads the label
```

Then create a PVC with `storageClassName: scseries-fc`. If the HPE CSI Driver is already installed, use `--set installHpeCsiDriver=false` and install into its namespace.

Use `array.credentials.existingSecret` and `csp.tokenKey.existingSecret` to keep credentials out of Helm values (e.g. SOPS or External Secrets). [values-kube-dc.yaml](charts/scseries-csp/values-kube-dc.yaml) is a complete platform profile (CDI integration, monitoring, audit) that can serve as an example.

## StorageClass parameters

| Parameter | Default | Meaning |
|---|---|---|
| `storageProfile` | chart `csp.storageProfile` ("" = array default) | Storage Center storage profile name, e.g. `High Priority (Tier 1)` |
| `dataReductionProfile` | chart `csp.dataReductionProfile` ("" = array default) | e.g. `None`, `Compression` |
| `snapshotProfile` | chart `csp.snapshotProfile` ("" = none) | Storage Center snapshot schedule; `array-default` keeps the array's |
| `description` | — | Volume notes; `{pvc}`, `{namespace}`, `{pv}` are expanded by the driver |

The chart sets `accessProtocol: fc`, the CSI secret references and the filesystem type.

## Documentation

- [Architecture](docs/architecture.md): how CSP calls map to Storage Center objects, HA design, the contract
- [Array setup](docs/array-setup.md): account, folders, TLS pinning, FC, pitfalls with legacy server objects
- [Node requirements](docs/node-requirements.md): multipath, containerd, kernel limits, topology labels, KubeVirt
- [Operations](docs/operations.md): metrics, alerts, audit, upgrades, credential rotation, troubleshooting
- [Testing](docs/testing.md): unit tests, contract test against an array, CSI e2e, failover suites
- [kube-dc profile](docs/kube-dc.md)

## Limitations

- Fibre Channel only (no iSCSI yet). Volume groups and snapshot groups are not implemented (501).
- On SCOS 7.2 with direct REST access, only Admin-privilege accounts can log in. Use a dedicated account; the CSP's folder guard limits what it touches.
- Unattended VM recovery after a node failure needs node fencing (e.g. BMC power-off) in addition to RWX volumes; that is outside this project.

## Development

```bash
go test -race ./...                      # unit tests against an in-memory fake Storage Center
hack/build-image.sh 0.3.0                # image without a container engine (go + crane)
hack/csi-e2e.sh all                      # end-to-end on a cluster with the chart installed
```

## Disclaimer

This is an independent open-source project developed by the [Kube-DC](https://kube-dc.com) team. It is not affiliated with, endorsed by or supported by Dell Technologies or Hewlett Packard Enterprise. Dell, Compellent and Storage Center are trademarks of Dell Inc.; HPE is a trademark of Hewlett Packard Enterprise.

## License

[Apache License 2.0](LICENSE)
