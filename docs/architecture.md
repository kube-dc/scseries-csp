# Architecture

## Components

| Component | Role |
|---|---|
| HPE CSI Driver controller | Kubernetes CSI controller (provisioner, attacher, resizer, snapshotter sidecars). Calls the CSP for every array operation |
| HPE CSI Driver node plugin | Runs on every node: FC rescans, multipath device discovery by serial, filesystems, raw block publishing |
| **scseries-csp** | Implements the [HPE CSP API](https://github.com/hpe-storage/container-storage-provider) (`/containers/v1/...`) on the Dell Storage Center REST API |
| Storage Center | Volumes, server objects with FC HBAs, mappings, replays (snapshots), view volumes (clones) |

The driver finds the CSP through the backend Secret it receives on each CSI call (`serviceName`, `servicePort`, `backend`, `username`, `password`).

## CSP calls → Storage Center objects

| CSP call | Storage Center |
|---|---|
| `POST /tokens` | Log in (API version negotiated), return a sealed session token |
| `POST /hosts` | Find the node's server object by FC WWPN (`ScServerHba`), or create one (`ScPhysicalServer` + `AddHba`) in the CSP server folder. Record the node UUID in the server's Notes (`csp-node-uuid=<uuid>`). Ports owned by servers outside the folder are skipped |
| `GET /volumes?name=`, `POST /volumes` | Volume in the CSP volume folder (idempotent by name). Storage and data-reduction profiles applied; the default snapshot schedule removed unless configured |
| `PUT /volumes/{id}/actions/publish` | `MapToServer` for the node's server. Returns `serial_number` = volume `deviceId` (NAA, equals the multipath WWID without the leading `3`), the target WWPNs of the mapping, and the LUN |
| `PUT /volumes/{id}/actions/unpublish` | Delete only that server's mapping profile; other nodes of an RWX volume keep theirs |
| `PUT /volumes/{id}` | `ExpandToSize` |
| `DELETE /volumes/{id}` | Refused while mapped. `DELETE`, then purge if the array only recycled it |
| `POST /snapshots` | `CreateReplay` (never expires). Refused for never-written volumes (the array cannot snapshot them) |
| `POST /volumes` with `base_snapshot_id` | `ScReplay/{id}/CreateView`, grown to the requested size |
| `DELETE /snapshots/{id}` | Expire the replay |

Storage Center details the CSP handles:
- Replay descriptions are truncated to 31 characters, so name lookups match on that prefix.
- A view volume inherits its parent's replays; those are not reported as the clone's own snapshots (the driver refuses to delete volumes with snapshots).
- PVC→PVC clones go through a driver-created `snap-for-clone-…` helper snapshot. The CSP expires it right after the view is created (the clone keeps the pages it needs), so the source stays deletable.

## High availability

- Two replicas share a Kubernetes Lease. Only the leader changes the array.
- The Lease holder identity is `<pod>@<ip>:<port>`. A standby forwards API calls to the leader, so both replicas stay Ready, and rolling updates and node drains work. A forwarded request is never forwarded again.
- Session tokens are AES-256-GCM sealed (array, user, password, expiry) with a key shared by all replicas. A failover needs no re-login; an invalid or expired token gets 401 and the driver logs in again.
- Every operation is idempotent, so the driver's retries are safe. A leader killed mid-provisioning produced no duplicate volumes in testing.

## Ownership and safety

- The CSP only sees volumes in its volume folder and servers in its server folder. Everything else returns 404 or is never touched.
- WWPNs already owned by a server object outside the folder (for example a legacy ESXi object) are skipped with a warning. The node is refused only if no WWPN is usable.
- The HPE node plugin is configured not to rewrite multipath settings and not to "clean up" devices (`disableNodeConfiguration`, `disableNodeMonitor`). This is essential on hosts that also see LUNs of other arrays.
