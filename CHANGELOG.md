# Changelog

## 0.3.0

- Helm chart `charts/scseries-csp`: optional pinned HPE CSI Driver 3.3.0 dependency, StorageClass and VolumeSnapshotClass, PodDisruptionBudget, NetworkPolicy, ServiceMonitor, PrometheusRule, audit CronJob, CDI StorageProfile integration.
- Array health monitor: `scseries_array_*` metrics (status, disks, spares, alerts, capacity), polled by the leader.
- `audit --fix stale-mapping,stale-server` with `--fix-match`.
- `cdi-profiles` command: makes CDI StorageProfiles default to RWX raw block with CSI clones.
- Standby replicas forward to the leader instead of being unready, so rolling updates and node drains work.
- Storage and data-reduction profiles default to the array's settings; `array-default` keeps the array's snapshot schedule.
- Generic defaults (folders `scseries-csp`); `values-kube-dc.yaml` profile.

## 0.2.x

- Go implementation: sealed stateless tokens, Lease leader election, idempotent operations, per-array locking, read-only retries, TLS fingerprint pinning, Prometheus metrics, JSON logs.
- Distroless nonroot image.

## 0.1.0

- Python prototype of the CSP contract, used to validate the design against an SC5020.
