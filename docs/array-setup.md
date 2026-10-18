# Storage Center setup

## REST access

- **Direct to the array:** SCOS 7.x serves the REST API itself on TCP 3033. A DSM Data Collector works the same way; set `array.ssn` if it manages several Storage Centers.
- Find the management address in Storage Manager, then check: `curl -k https://<host>:3033/api/rest/` should answer 401.
- **TLS:** Storage Centers ship self-signed certificates, often issued for an old address, so hostname verification cannot succeed. Pin the certificate:
  ```bash
  openssl s_client -connect <host>:3033 </dev/null | openssl x509 -noout -fingerprint -sha256
  ```
  Controllers may present different certificates; give several fingerprints comma-separated.

## Account

On **SCOS 7.2 with direct REST access, only Admin-privilege accounts can log in** (Volume Manager accounts get HTTP 401), and Storage Center privileges can only be upgraded, never downgraded.

Create a dedicated account rather than using the built-in Admin. It is separate in the audit log and can be rotated independently:

```bash
python3 -I hack/sc-maint.py csp-user --user scseries-csp --folder scseries-csp   # needs .env with an Admin login
```

This creates the user, a user group with the CSP folders, and writes `.env/csp.env`. The REST calls it relies on are not in the public cookbook:
- `ScUser` POST ignores `Password`; the password is set with `ScUser/{id}/ResetPassword`.
- `ScUserGroup` POST ignores folder lists; they are added with `ScUserGroup/{id}/AddVolumeFolder|AddServerFolder|AddDiskFolder` and `AddUserToGroup`.

With a DSM Data Collector in front of the array, a Volume Manager account limited to the user group is possible.

## Folders and defaults

- The CSP creates its volume folder and server folder on first use (default `scseries-csp`).
- New volumes inherit the array's default snapshot profile (often "Daily") and data reduction. The CSP removes the snapshot schedule unless `snapshotProfile` is set, and applies the storage and data-reduction profiles from the StorageClass. Check the array defaults: `hack/sc-probe.py info`.
- Server objects get OS type `Red Hat Linux 7.x` (configurable).

## Fibre Channel

- Zone every node HBA to the array's front-end ports on both fabrics. In virtual port mode, zone to the virtual WWPNs.
- `hack/sc-node.sh inventory` on a node shows HBAs, visible target ports and multipath maps.

## Legacy server objects (migrating hosts from VMware)

- If a node's HBAs still belong to a server object outside the CSP folder (e.g. an ESXi host or cluster), the CSP skips those ports.
- Detach them first: `hack/sc-maint.py remove-hbas --match '<server>' --yes`.
- Storage Center refuses to remove the last HBA of a server while vVol bindings exist ("SCSISubMapZones"). Such bindings can only be released by vCenter/VASA or Dell support. Until then the node runs on the remaining ports (single path).
- Volumes still mapped to the legacy object stay visible on the node. Unmap them, or make sure the node's multipath configuration does not claim them (see node requirements).
