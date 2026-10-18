#!/usr/bin/env python3
"""Probe the Dell SC Series REST API (DSM Data Collector or direct Storage Center).

Calls mirror OpenStack Cinder's storagecenter_api.py, so a pass here means the CSP can
use the same calls. Stdlib only.

Environment (unset values are read from <repo>/.env or <repo>/.env/*.env):
  SC_HOST      DSM Data Collector (or SC management) address        required
  SC_USER      DSM/SC user                                           required
  SC_PASSWORD  password                                              required
  SC_PORT      default 3033 (DSM). Direct-to-array is usually 443.
  SC_SSN       Storage Center serial number when DSM manages several
  SC_VERIFY    1 to verify TLS (default off: DSM ships a self-signed cert)

Commands:
  info                       read-only: API version, Storage Centers, usage, FC ports, OS types
  server <wwpn> [<wwpn>...]  read-only: server object owning these HBAs, its OS type and mappings
  lifecycle --wwpn W [--wwpn W2] [--size-gb 1] [--folder NAME] [--no-wait] [--keep]
                             create scseries-probe-* volume, map to each server, print the
                             CSP PublishInfo, wait for host checks, unmap, delete
  cleanup                    unmap and delete leftover scseries-probe-* volumes (asks first)
"""
import argparse
import base64
import http.cookiejar
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request

PREFIX = "scseries-probe-"


def load_dotenv():
    """Fill unset variables from <repo>/.env, or from <repo>/.env/*.env if it is a directory."""
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".env")
    if os.path.isdir(path):
        files = sorted(os.path.join(path, f) for f in os.listdir(path) if f.endswith(".env"))
    else:
        files = [path] if os.path.isfile(path) else []
    for f in files:
        for line in open(f):
            line = line.strip()
            if line.startswith("export "):
                line = line[7:]
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                os.environ.setdefault(k.strip(), v.strip().strip("'\""))


class SC:
    def __init__(self):
        load_dotenv()
        try:
            self.host = os.environ["SC_HOST"]
            user, pw = os.environ["SC_USER"], os.environ["SC_PASSWORD"]
        except KeyError as e:
            sys.exit(f"missing env {e}")
        self.base = f"https://{self.host}:{os.environ.get('SC_PORT', '3033')}/api/rest/"
        ctx = ssl.create_default_context()
        if os.environ.get("SC_VERIFY") != "1":
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPSHandler(context=ctx),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        self.headers = {
            "Content-Type": "application/json; charset=utf-8",
            "Accept": "application/json",
            "Authorization": "Basic " + base64.b64encode(f"{user}:{pw}".encode()).decode(),
            "x-dell-api-version": os.environ.get("SC_API_VERSION", "4.1"),
        }
        self.ssn = os.environ.get("SC_SSN")

    def call(self, method, path, payload=None, ok=(200, 201, 202, 204)):
        data = json.dumps(payload).encode() if payload is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method, headers=self.headers)
        try:
            with self.opener.open(req, timeout=300) as r:
                status, body = r.status, r.read().decode()
        except urllib.error.HTTPError as e:
            status, body = e.code, e.read().decode()
        if status not in ok:
            raise RuntimeError(f"{method} {path} -> {status}: {body[:500]}")
        return json.loads(body) if body.strip() else None

    def get(self, path):
        return self.call("GET", path)

    def post(self, path, payload):
        return self.call("POST", path, payload)

    def delete(self, path):
        return self.call("DELETE", path)

    def getlist(self, obj, **filters):
        """POST StorageCenter/<obj>/GetList with an AND of Equals filters (API >= 2.2 format)."""
        f = [{"attributeName": k, "attributeValue": v, "filterType": "Equals"} for k, v in filters.items()]
        return self.post(f"StorageCenter/{obj}/GetList", {"filter": {"filterType": "AND", "filters": f}}) or []

    def login(self):
        payload = {"Application": "scseries-csp sc-probe", "ApplicationVersion": "0.1"}
        try:
            r = self.post("ApiConnection/Login", payload)
        except RuntimeError as e:
            # Same negotiation as Cinder: the error names the accepted version range.
            msg = str(e)
            if "the version must be in the range [" not in msg:
                raise
            self.headers["x-dell-api-version"] = msg.split("[")[1].split(",")[0].strip()
            r = self.post("ApiConnection/Login", payload)
        self.api = r if isinstance(r, dict) else {}
        scs = self.get("StorageCenter/StorageCenter") or []
        if not scs:
            sys.exit("logged in, but no Storage Center is visible to this user")
        if self.ssn is None:
            if len(scs) > 1:
                sys.exit("several Storage Centers visible, set SC_SSN to one of: "
                         + ", ".join(str(s.get("scSerialNumber")) for s in scs))
            self.ssn = scs[0].get("scSerialNumber")
        self.sc = next((s for s in scs if str(s.get("scSerialNumber")) == str(self.ssn)), None)
        if self.sc is None:
            sys.exit(f"SC_SSN {self.ssn} not found")
        self.ssn = self.sc["scSerialNumber"]
        return scs

    def logout(self):
        try:
            self.post("ApiConnection/Logout", {})
        except Exception:
            pass

    # --- objects -----------------------------------------------------------

    def server_by_wwpn(self, wwpn):
        w = wwpn.lower().replace("0x", "").replace(":", "").upper()
        hbas = self.getlist("ScServerHba", scSerialNumber=self.ssn, instanceName=w)
        if not hbas or not hbas[0].get("server"):
            return None, w
        sid = hbas[0]["server"]["instanceId"]
        return self.get(f"StorageCenter/ScServer/{sid}"), w

    def port_wwn(self, cport_id, cache={}):
        if cport_id not in cache:
            p = self.get(f"StorageCenter/ScControllerPort/{cport_id}") or {}
            cache[cport_id] = next((v for k, v in p.items() if k.lower() == "wwn"), None)
        return cache[cport_id]

    def publish_info(self, vol, server):
        """What a CSP would return from PUT /volumes/{id}/actions/publish."""
        initiators = {h.get("instanceName", "").upper()
                      for h in self.get(f"StorageCenter/ScServer/{server['instanceId']}/HbaList") or []}
        luns, targets = set(), set()
        for m in self.get(f"StorageCenter/ScVolume/{vol['instanceId']}/MappingList") or []:
            hba = (m.get("serverHba") or {}).get("instanceName", "").upper()
            if hba in initiators:
                luns.add(m.get("lun"))
                wwn = self.port_wwn(m["controllerPort"]["instanceId"])
                if wwn:
                    targets.add(wwn.lower())
        return {"access_protocol": "fc", "serial_number": vol.get("deviceId"),
                "lun_id": next(iter(luns)) if len(luns) == 1 else sorted(luns, key=str),
                "target_names": sorted(targets)}


def name(o):
    return (o or {}).get("instanceName") or (o or {}).get("name")


def cmd_info(sc, _):
    scs = sc.login()
    print("## API connection")
    for k in ("provider", "apiVersion", "hostName", "userName", "connected", "minApiVersion"):
        if k in sc.api:
            print(f"  {k}: {sc.api[k]}")
    print(f"  x-dell-api-version used: {sc.headers['x-dell-api-version']}")
    print("\n## Storage Centers")
    for s in scs:
        print("  " + "  ".join(f"{k}={s.get(k)}" for k in
              ("name", "scSerialNumber", "version", "modelSeries", "model", "hostOrIpAddress", "status")
              if s.get(k) is not None))
    try:
        u = sc.get(f"StorageCenter/StorageCenter/{sc.sc['instanceId']}/StorageUsage") or {}
        print("\n## Storage usage")
        for k in ("availableSpace", "freeSpace", "allocatedSpace", "usedSpace", "configuredSpace"):
            if k in u:
                print(f"  {k}: {u[k]}")
    except RuntimeError as e:
        print(f"  (usage unavailable: {e})")
    print("\n## Controller ports (FC targets the hosts must be zoned to)")
    for p in sc.getlist("ScControllerPort", scSerialNumber=sc.ssn):
        wwn = next((v for k, v in p.items() if k.lower() == "wwn"), "")
        print(f"  {name(p):<28} wwn={wwn:<18} transport={p.get('transportType')} "
              f"virtual={p.get('virtual', p.get('virtualPort'))} status={p.get('status')}")
    print("\n## Server operating system types (pick a Linux one for CSP server objects)")
    for o in sc.getlist("ScServerOperatingSystem", scSerialNumber=sc.ssn):
        print(f"  {name(o)}")
    print("\n## Default volume preferences (check snapshot profile: every PVC inherits it)")
    try:
        prefs = sc.get(f"StorageCenter/StorageCenter/{sc.ssn}/UserPreferences") or {}
        for k, v in prefs.items():
            if any(t in k.lower() for t in ("replay", "snapshot", "storageprofile", "datareduction", "readcache", "writecache")):
                print(f"  {k}: {name(v) if isinstance(v, dict) else v}")
    except RuntimeError as e:
        print(f"  (unavailable: {e})")


def cmd_server(sc, a):
    sc.login()
    for wwpn in a.wwpn:
        srv, w = sc.server_by_wwpn(wwpn)
        if srv is None:
            print(f"{w}: no server object owns this HBA (not registered or not zoned)")
            continue
        print(f"{w}: server '{name(srv)}' id={srv['instanceId']} type={srv.get('objectType')} "
              f"os='{name(srv.get('operatingSystem'))}' parent='{name(srv.get('parent'))}'")
        for m in sc.get(f"StorageCenter/ScServer/{srv['instanceId']}/MappingList") or []:
            print(f"    lun={str(m.get('lun')):<4} volume='{name(m.get('volume'))}' via hba={name(m.get('serverHba'))}")


def unmap_and_delete(sc, vol, keep=False):
    if not vol.get("name", "").startswith(PREFIX):
        sys.exit(f"REFUSE: {vol.get('name')} is not a {PREFIX}* volume")
    for mp in sc.get(f"StorageCenter/ScVolume/{vol['instanceId']}/MappingProfileList") or []:
        sc.delete(f"StorageCenter/ScMappingProfile/{mp['instanceId']}")
        print(f"  unmapped from '{name(mp.get('server'))}'")
    if keep:
        print(f"  kept volume {vol['name']}")
        return
    sc.delete(f"StorageCenter/ScVolume/{vol['instanceId']}")
    print(f"  deleted volume {vol['name']}")


def cmd_lifecycle(sc, a):
    sc.login()
    servers = []
    for w in a.wwpn:
        srv, ww = sc.server_by_wwpn(w)
        if srv is None:
            sys.exit(f"no server object owns HBA {ww}; register the host on the SC first")
        if srv["instanceId"] not in [s["instanceId"] for s in servers]:
            servers.append(srv)
    vname = f"{PREFIX}{time.strftime('%Y%m%d-%H%M%S')}"
    payload = {"Name": vname, "Size": f"{a.size_gb} GB", "StorageCenter": sc.ssn,
               "Notes": "scseries-csp sc-probe disposable test volume"}
    if a.folder:
        folders = [f for f in sc.getlist("ScVolumeFolder", scSerialNumber=sc.ssn) if f.get("name") == a.folder]
        if not folders:
            sys.exit(f"volume folder '{a.folder}' not found")
        payload["VolumeFolder"] = folders[0]["instanceId"]

    t0 = time.time()
    vol = sc.post("StorageCenter/ScVolume", payload)
    print(f"1. created {vol['name']} id={vol['instanceId']} deviceId={vol.get('deviceId')} "
          f"serial={vol.get('serialNumber')} ({time.time() - t0:.1f}s)")
    try:
        for srv in servers:
            t0 = time.time()
            sc.post(f"StorageCenter/ScVolume/{vol['instanceId']}/MapToServer",
                    {"server": srv["instanceId"], "Advanced": {"MapToDownServerHbas": True}})
            print(f"2. mapped to '{name(srv)}' ({time.time() - t0:.1f}s)")
            print("   PublishInfo: " + json.dumps(sc.publish_info(vol, srv)))
        print(f"\n3. on each host:  sudo hack/sc-node.sh rescan && sudo hack/sc-node.sh show 3{vol.get('deviceId')}")
        print(f"   before step 4:  sudo hack/sc-node.sh release 3{vol.get('deviceId')}")
        if not a.no_wait:
            input("   press Enter when host checks are done and the device is released ... ")
    finally:
        print("4. cleanup")
        unmap_and_delete(sc, vol, keep=a.keep)


def cmd_cleanup(sc, _):
    sc.login()
    vols = [v for v in sc.getlist("ScVolume", scSerialNumber=sc.ssn) if v.get("name", "").startswith(PREFIX)]
    if not vols:
        print("nothing to clean")
        return
    for v in vols:
        print(f"  {v['name']}  deviceId={v.get('deviceId')}")
    if input(f"unmap and delete these {len(vols)} volumes? [y/N] ").lower() != "y":
        return
    for v in vols:
        unmap_and_delete(sc, v)


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("info")
    s = sub.add_parser("server")
    s.add_argument("wwpn", nargs="+")
    l = sub.add_parser("lifecycle")
    l.add_argument("--wwpn", action="append", required=True, help="one HBA WWPN per host to map to")
    l.add_argument("--size-gb", type=int, default=1)
    l.add_argument("--folder")
    l.add_argument("--no-wait", action="store_true")
    l.add_argument("--keep", action="store_true", help="unmap but keep the volume")
    sub.add_parser("cleanup")
    a = p.parse_args()
    sc = SC()
    try:
        {"info": cmd_info, "server": cmd_server, "lifecycle": cmd_lifecycle, "cleanup": cmd_cleanup}[a.cmd](sc, a)
    finally:
        sc.logout()


if __name__ == "__main__":
    main()
