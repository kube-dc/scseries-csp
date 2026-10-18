# Operations

## Metrics (`:9090/metrics`)

| Metric | Meaning |
|---|---|
| `scseries_csp_requests_total{route,method,code}`, `scseries_csp_request_duration_seconds` | CSP API traffic from the HPE driver |
| `scseries_csp_array_requests_total{method,object,code}`, `..._duration_seconds` | Storage Center REST calls (`code="0"` = transport error) |
| `scseries_csp_leader` | 1 on the leader |
| `scseries_array_up`, `scseries_array_status{status}` | REST reachability, array status (Up / Degraded / Down) |
| `scseries_array_disks{class,state}` | Disks per class: healthy, unhealthy, spare |
| `scseries_array_active_alerts{severity}` | Active, unacknowledged array alerts |
| `scseries_array_capacity_bytes{kind}` | available, free, used, allocated, configured |

Array health is polled by the leader (`monitor.interval`, default 60 s) with the backend credentials.

## Alerts (`prometheusRule.enabled`)

| Alert | Triggers when |
|---|---|
| `ScseriesCSPNoLeader` | no replica is leader for 2 min (provisioning and attach fail) |
| `ScseriesCSPLeaderFlapping` | more than 6 leadership changes in 30 min |
| `ScseriesCSPRequestErrors`, `ScseriesArrayCallErrors` | sustained 5xx answers from the CSP, or failing array calls |
| `ScseriesArrayUnreachable`, `ScseriesArrayPollStale` | the array REST API is down, or no successful health poll for 10 min |
| `ScseriesArrayDegraded` | array status is not Up for 10 min |
| `ScseriesArrayUnhealthyDisks`, `ScseriesArrayNoSpares` | failed disks, or a disk class without spares |
| `ScseriesArrayCriticalAlerts` | unacknowledged Critical, Emergency or Down alerts on the array |
| `ScseriesArrayFreeSpaceLow` | free space below `freeSpaceWarningRatio` (thin volumes only consume space as written) |

## Audit

`scseries-csp audit` compares the CSP folders with the cluster and exits 2 on findings. `audit.enabled` runs it as a CronJob; a failed Job is the signal.

| Finding | Meaning | `--fix` |
|---|---|---|
| `orphan-volume` | volume without a PV (e.g. a PV deleted with `Retain`) | report only |
| `missing-volume` | PV whose volume is gone | report only |
| `stale-mapping` | mapping without a VolumeAttachment | `stale-mapping`: unmaps |
| `stale-server` | server object without a cluster node | `stale-server`: deletes it if nothing is mapped |

Limit repairs with `--fix-match <regex>` on the volume or server name.

## Upgrades

- The CSP is stateless; a rolling update keeps one replica serving (the standby forwards).
- The HPE CSI Driver dependency is pinned. Test new driver releases with `hack/csi-e2e.sh` before raising the pin.
- Server objects written by older releases (`kube-dc-node-uuid=` tags) are still recognised.

## Credentials

- **Backend Secret** (`array.credentials.existingSecret`): keys `serviceName`, `servicePort`, `backend`, `username`, `password`.
  To rotate the password:
  1. `hack/sc-maint.py csp-user --reset-password`.
  2. Update the Secret.
  3. Restart the CSP pods.

  Existing tokens are rejected at the next array login, and the driver logs in again.
- **Token key** (`csp.tokenKey.existingSecret`, key `token-key` = base64 of 32 bytes): generated once by the chart and kept across upgrades. Rotating it invalidates all tokens; the driver logs in again automatically.

## Troubleshooting

| Symptom | Cause |
|---|---|
| PVC Pending: "no topology key found for node" | node lacks the `csi.hpe.com/zone` label, or the node plugin has not restarted since it was added |
| Pod: "device not found with serial …" | the node's multipath configuration does not claim the volume (WWID exception, `find_multipaths`), or no FC path to the array |
| Importer or VM pod: "Permission denied" on `/dev/...` | containerd `device_ownership_from_security_context` is off |
| Node `kubevirt.io/schedulable=false`, virt-handler "too many open files" | `fs.inotify.max_user_instances` too low |
| CSP log: "WWPNs … belong to SC servers outside folder" | ports pinned in a legacy server object, see [array setup](array-setup.md) |
| CSP log: "storage center certificate fingerprint … is not pinned" | certificate changed, or a controller with its own certificate; add its fingerprint |
| Snapshot fails: "never been written" | Storage Center cannot snapshot a volume without data |
| Live migration never schedules | VMs on `host-model` across CPU generations; set a common CPU model |
