# Testing

| Level | Command | Needs |
|---|---|---|
| Unit | `go test -race ./...` | nothing (in-memory fake Storage Center, `internal/sc/scfake`) |
| CSP contract | `scseries-csp serve` on loopback + `hack/csp-smoke.py token / host / create / publish / snapshot / clone / unpublish / delete` | an array (`.env`), optionally a FC host for I/O checks with `hack/sc-node.sh` |
| CSI e2e | `STORAGE_CLASS=… SNAPSHOT_CLASS=… hack/csi-e2e.sh all` | a cluster with the chart; `ARRAY_CHECK=1` also verifies the array is clean |
| Resilience | `hack/failover-test.sh all` (path loss, control-plane restarts, all-path loss under I/O) | run on the node hosting the test pod |
| HA | `hack/leader-failover-test.sh 6` (kills the CSP leader during provisioning) | a cluster with the chart |
| KubeVirt | `deploy/tests/kubevirt-vm.yaml` + `VirtualMachineInstanceMigration` | KubeVirt and CDI; two FC nodes for migration |

The fake array reproduces behaviours observed on a real SC5020 (SCOS 7.2.30):
- API-version negotiation on login;
- one-element lists from `CreateReplay` and `CreateView`;
- 31-character replay descriptions;
- view volumes inheriting their parent's replay;
- recycle-bin-first deletes;
- no replays of never-written volumes.

## Validation results (SC5020, SCOS 07.02.30, HPE CSI 3.3.0, Kubernetes 1.36)

| Test | Result |
|---|---|
| Unit tests | all pass under `-race`. Includes 20 parallel same-name creates → exactly 1 volume, and 20 concurrent publishes to 2 hosts |
| CSI e2e | RWO xfs, RWX raw block shared by two pods, online expansion, VolumeSnapshot → restore with point-in-time data, PVC→PVC clone grown on creation, clean teardown |
| Two nodes | RWX raw block shared across nodes; detaching one node leaves the other's I/O running |
| KubeVirt | VM booted from a CDI-imported RWX block DataVolume; repeated live migrations between nodes with different CPU generations, 1–2 s each, guest never rebooted |
| Path loss | one path offline under direct I/O: 0 errors, 0 slow writes |
| Restarts | CSP (both replicas), CSI controller, CSI node plugin restarted under I/O: 0 errors |
| All-path loss | I/O held for ~120 s (`no_path_retry 24`), then failed fast; recovered right after the paths returned |
| Leader failover | leader killed during provisioning of 6 PVCs: new leader, 6/6 pods running, exactly 6 volumes, clean teardown |

Not yet validated: SC controller failover, real switch-port failures, multi-day soak, other SC models.
