#!/usr/bin/env python3
"""Dell SC Series maintenance helpers (uses the connection setup of sc-probe.py / .env).

  status                         read-only: controllers, tiers, unhealthy disks, RAID/disk alerts
  rebalance                      start a RAID rebalance (POST ScDiskFolder/RaidRebalance)
  ack --before YYYY-MM-DD [--yes]  acknowledge active alerts created before a date (dry run without --yes)
  hardware [--all] [--json F]    read-only: disks with position/serial/health, enclosure and controller parts
  wipe-volumes [--match RE] [--yes]  unmap and PERMANENTLY delete volumes (dry run without --yes)
  wipe-servers [--match RE] [--yes]  delete server objects with no mapped volumes (dry run without --yes)
  remove-hbas --match RE [--yes]   detach HBAs from physical server objects
  csp-user [--user U] [--folder F] [--reset-password]
                                 VolumeManager user + user group (CSP folders) for the CSP, write .env/csp.env
"""
import argparse
import importlib.util
import os
import re
import sys
import time

_spec = importlib.util.spec_from_file_location(
    "scprobe", os.path.join(os.path.dirname(os.path.abspath(__file__)), "sc-probe.py"))
_probe = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_probe)
name = _probe.name


def size_tb(s):
    try:
        return f"{float(str(s).split()[0]) / 1e12:.2f}TB"
    except ValueError:
        return str(s)


def cmd_status(sc, _):
    ssn = sc.ssn
    cfg = sc.get(f"StorageCenter/ScConfiguration/{ssn}") or {}
    print(f"Storage Center {ssn}: status={sc.sc.get('status')} portRebalanceNeeded={cfg.get('portRebalanceNeeded')}")
    for c in sc.getlist("ScController", scSerialNumber=ssn):
        print(f"  {c['name']:<18} status={c.get('status')} leader={c.get('leader')} ip={c.get('ipAddress')} boot={c.get('lastBootTime')}")
    print("Tiers")
    for t in sc.get("StorageCenter/ScStorageTypeTier") or []:
        u = sc.get(f"StorageCenter/ScStorageTypeTier/{t['instanceId']}/StorageUsage") or {}
        print(f"  tier{t.get('tierNumber')} needsRebalance={t.get('needsRebalance')} "
              f"allocated={size_tb(u.get('allocatedSpace'))} used={size_tb(u.get('usedSpace'))} free={size_tb(u.get('freeSpace'))}")
    print("Disk classes")
    for c in sc.get(f"StorageCenter/StorageCenter/{ssn}/DiskClassList") or []:
        print(f"  {c['name']:<22} disks={c.get('diskCount')} spares={c.get('spareCount')} unhealthy={c.get('unhealthyCount')}")
    print("Unhealthy disks")
    for d in sc.get(f"StorageCenter/StorageCenter/{ssn}/DiskList") or []:
        if not d.get("healthy") or d.get("status") != "Up":
            u = sc.get(f"StorageCenter/ScDisk/{d['instanceId']}/StorageUsage") or {}
            print(f"  {d['name']:<8} id={d['instanceId']:<12} {name(d.get('diskClass')):<20} status={d.get('status'):<5} "
                  f"mask={d.get('healthMask')} allocated={size_tb(u.get('allocatedSpace'))} markedForRemoval={d.get('markedForRemoval')}")
    print("Active alerts: RAID / disk / progression / rebalance")
    keys = ("RAID", "Rebuild", "Rebalance", "Progression", "Disk")
    for a in sorted(sc.get(f"StorageCenter/StorageCenter/{ssn}/ActiveAlertList") or [], key=lambda a: a.get("createTime", "")):
        msg = a.get("message") or ""
        if any(k in msg for k in keys):
            print(f"  [{a.get('status')}] {a.get('createTime')} {name(a.get('controller')) or ''}: {msg[:150]}")


def cmd_rebalance(sc, _):
    t = time.strftime("%Y-%m-%d %H:%M:%S")
    r = sc.call("POST", "StorageCenter/ScDiskFolder/RaidRebalance", {"StorageCenter": str(sc.ssn)})
    print(f"{t} RaidRebalance -> {r}")


def cmd_ack(sc, a):
    alerts = [x for x in sc.get(f"StorageCenter/StorageCenter/{sc.ssn}/ActiveAlertList") or []
              if not x.get("acknowledged") and (x.get("createTime") or "") < a.before]
    for x in sorted(alerts, key=lambda x: x.get("createTime", "")):
        print(f"  {'ACK ' if a.yes else 'would ack'} {x.get('createTime')} [{x.get('status')}] {(x.get('message') or '')[:110]}")
        if a.yes:
            sc.call("POST", f"StorageCenter/ScAlert/{x['instanceId']}/Acknowledge", {})
    print(f"{len(alerts)} alerts {'acknowledged' if a.yes else 'would be acknowledged (add --yes)'}")


def cmd_hardware(sc, a):
    """Read-only: every disk with position/serial/health, enclosure modules, controller PSUs/fans/ports."""
    ssn = sc.ssn
    rows = []
    for d in sc.get(f"StorageCenter/StorageCenter/{ssn}/DiskList") or []:
        c = sc.get(f"StorageCenter/ScDisk/{d['instanceId']}/DiskConfiguration") or {}
        rows.append((d, c))
    if a.json:
        import json
        json.dump([{**d, "config": c} for d, c in rows], open(a.json, "w"), indent=1, default=str)
    print("Disks (not healthy, not Up, spare, or SSD endurance < 50)")
    print(f"  {'pos':<7} {'id':<12} {'class':<20} {'size':>8} {'status':<5} {'spare':<5} {'end':>4} {'hours':>6} {'product':<18} {'rev':<6} {'serial':<14} health")
    for d, c in sorted(rows, key=lambda r: (r[1].get("enclosureIndex") or 0, r[1].get("enclosurePosition") or 0)):
        endurance = d.get("endurance")
        flag = (not d.get("healthy")) or d.get("status") != "Up" or d.get("spare") or \
               (d.get("ssd") and isinstance(endurance, int) and endurance < 50) or a.all
        if flag:
            pos = f"{c.get('enclosureIndex')}-{c.get('enclosurePosition')}"
            print(f"  {pos:<7} {d['instanceId']:<12} {name(d.get('diskClass')):<20} {size_tb(d.get('size')):>8} {d.get('status'):<5} "
                  f"{str(d.get('spare')):<5} {str(endurance):>4} {str(d.get('powerOnTimeHours')):>6} {str(c.get('product')):<18} "
                  f"{str(c.get('revision')):<6} {str(c.get('serialNumber')):<14} {c.get('healthDescription') or d.get('healthMask')}")
    print("Enclosures")
    for e in sc.getlist("ScEnclosure", scSerialNumber=ssn):
        print(f"  {e['name']:<14} {e.get('model'):<10} st={e.get('serviceTag')} status={e.get('status')} "
              f"crit={e.get('criticalCondition')} noncrit={e.get('nonCriticalCondition')} fw A/B={e.get('aSideFirmware')}/{e.get('bSideFirmware')}")
        for sub in ("IoModuleList", "PowerSupplyList"):
            for m in sc.get(f"StorageCenter/ScEnclosure/{e['instanceId']}/{sub}") or []:
                print(f"      {sub[:-4]:<12} {m.get('name')} status={m.get('status')}")
    print("Controllers")
    for ctl in sc.getlist("ScController", scSerialNumber=ssn):
        print(f"  {ctl['name']} status={ctl.get('status')} mem={ctl.get('availableMemory')}")
        for p in sc.get(f"StorageCenter/ScController/{ctl['instanceId']}/PowerSupplyList") or []:
            print(f"      PSU {p.get('name')} status={p.get('status')} failed={p.get('failed')} acLost={p.get('acPowerLost')}")
        for f in sc.get(f"StorageCenter/ScController/{ctl['instanceId']}/FanSensorList") or []:
            if f.get("status") != "Up":
                print(f"      FAN {f.get('name')} status={f.get('status')} rpm={f.get('currentRpm')}")
        for t in sc.get(f"StorageCenter/ScController/{ctl['instanceId']}/TemperatureSensorList") or []:
            if t.get("status") != "Up":
                print(f"      TEMP {t.get('name')} status={t.get('status')} {t.get('currentTemperature')}")
        for p in sc.get(f"StorageCenter/ScController/{ctl['instanceId']}/ControllerPortList") or []:
            print(f"      port {p.get('name'):<18} {p.get('transportType'):<13} purpose={p.get('purpose'):<16} cabled={p.get('cabled')} status={p.get('status')}")


def cmd_wipe_volumes(sc, a):
    """Unmap and permanently delete volumes. Dry run unless --yes. Retries in passes for view/parent ordering."""
    vols = [v for v in sc.getlist("ScVolume", scSerialNumber=sc.ssn) if re.search(a.match, v.get("name", ""))]
    total = sum(float(str(v.get("configuredSize", "0")).split()[0]) for v in vols)
    blocked = [v for v in vols if not v.get("deleteAllowed")]
    print(f"{len(vols)} volumes match /{a.match}/: {total / 1e12:.1f} TB configured, "
          f"{sum(1 for v in vols if v.get('mapped'))} mapped, {len(blocked)} with deleteAllowed=False")
    for v in blocked:
        print(f"  deleteAllowed=False: {v['name']} ({size_tb(v.get('configuredSize'))})")
    if not a.yes:
        for v in sorted(vols, key=lambda v: v["name"]):
            print(f"  would delete {v['name']:<40} {size_tb(v.get('configuredSize')):>9} mapped={v.get('mapped')}")
        print("dry run: add --yes to unmap and PERMANENTLY delete")
        return
    pending = list(vols)
    for attempt in range(1, 4):
        failed = []
        for v in pending:
            try:
                if v.get("mapped") and not a.no_unmap:
                    for mp in sc.get(f"StorageCenter/ScVolume/{v['instanceId']}/MappingProfileList") or []:
                        sc.delete(f"StorageCenter/ScMappingProfile/{mp['instanceId']}")
                sc.delete(f"StorageCenter/ScVolume/{v['instanceId']}")
                print(f"  deleted {v['name']}")
            except RuntimeError as e:
                failed.append(v)
                print(f"  pass {attempt} FAILED {v['name']}: {str(e)[:220]}")
        if not failed or len(failed) == len(pending):
            pending = failed
            break
        pending = failed
    print(f"done: {len(vols) - len(pending)} deleted, {len(pending)} left")


def cmd_wipe_servers(sc, a):
    """Delete server objects (physical first, then clusters). Refuses servers that still have volumes mapped."""
    servers = [s for s in sc.getlist("ScServer", scSerialNumber=sc.ssn) if re.search(a.match, s.get("name", ""))]
    servers.sort(key=lambda s: s.get("type") == "Cluster")
    for s in servers:
        vols = s.get("volumeCount") or 0
        hbas = ", ".join(h.get("instanceName", "") for h in sc.get(f"StorageCenter/ScServer/{s['instanceId']}/HbaList") or [])
        if not a.yes:
            print(f"  would delete {s['name']:<16} type={s.get('type'):<8} volumes={vols} hbas=[{hbas}]")
            continue
        if vols and not a.force:
            print(f"  SKIP {s['name']}: {vols} volumes still mapped (use --force)")
            continue
        try:
            sc.delete(f"StorageCenter/ScServer/{s['instanceId']}")
            print(f"  deleted {s['name']} (hbas {hbas})")
        except RuntimeError as e:
            print(f"  FAILED {s['name']}: {str(e)[:220]}")
    if not a.yes:
        print(f"dry run: {len(servers)} server objects; add --yes to delete")


def cmd_remove_hbas(sc, a):
    """Detach HBAs from physical server objects (frees the WWPNs for new server objects). Dry run unless --yes."""
    for s in sc.getlist("ScPhysicalServer", scSerialNumber=sc.ssn):
        if not re.search(a.match, s.get("name", "")):
            continue
        for h in sc.get(f"StorageCenter/ScServer/{s['instanceId']}/HbaList") or []:
            if not a.yes:
                print(f"  would remove {h.get('instanceName')} from {s['name']}")
                continue
            try:
                sc.call("POST", f"StorageCenter/ScPhysicalServer/{s['instanceId']}/RemoveHba", {"ServerHba": h["instanceId"]})
                print(f"  removed {h.get('instanceName')} from {s['name']}")
            except RuntimeError as e:
                print(f"  FAILED {h.get('instanceName')} from {s['name']}: {str(e)[:220]}")


def _folder(sc, obj, folder):
    found = [f for f in sc.getlist(obj, scSerialNumber=sc.ssn) if f.get("name") == folder]
    if found:
        return found[0]["instanceId"]
    f = sc.call("POST", f"StorageCenter/{obj}", {"Name": folder, "StorageCenter": sc.ssn, "Notes": "scseries-csp managed"})
    print(f"  created {obj} {folder}")
    return f["instanceId"]


def _link_group(sc, gid, vf, sf, df, uid):
    """Attach folders and the user to the user group; method names are not in the public cookbook, so try
    the plausible ones and keep the first the array accepts."""
    def members(sub):
        return [x.get("instanceName") for x in sc.get(f"StorageCenter/ScUserGroup/{gid}/{sub}") or []]

    wanted = [("VolumeFolderList", "VolumeFolder", [vf]), ("ServerFolderList", "ServerFolder", [sf]),
              ("DiskFolderList", "DiskFolder", df)]
    for sub, kind, ids in wanted:
        if members(sub):
            print(f"  {sub}: {members(sub)}")
            continue
        for method, payload in ((f"Add{kind}s", {f"{kind}List": ids}), (f"Add{kind}", {kind: ids[0]}),
                                (f"Add{kind}List", {f"{kind}List": ids})):
            try:
                sc.call("POST", f"StorageCenter/ScUserGroup/{gid}/{method}", payload)
                print(f"  {method} accepted -> {sub}: {members(sub)}")
                break
            except RuntimeError as e:
                print(f"  {method}: {str(e)[60:170]}")
    if members("UserList"):
        print(f"  UserList: {members('UserList')}")
        return
    candidates = [("POST", f"ScUserGroup/{gid}/AddUsers", {"UserList": [uid]}),
                  ("POST", f"ScUserGroup/{gid}/AddUser", {"User": uid}),
                  ("POST", f"ScUserGroup/{gid}/AddUserToGroup", {"User": uid}),
                  ("POST", f"ScUser/{uid}/AddUserGroups", {"UserGroupList": [gid]}),
                  ("POST", f"ScUser/{uid}/AddUserGroup", {"UserGroup": gid}),
                  ("POST", f"ScUser/{uid}/AddToUserGroup", {"UserGroup": gid}),
                  ("POST", f"ScUser/{uid}/AddToGroup", {"UserGroup": gid}),
                  ("PUT", f"ScUser/{uid}", {"UserGroupList": [gid]}),
                  ("PUT", f"ScUser/{uid}", {"UserGroups": [gid]})]
    for verb, path, payload in candidates:
        try:
            sc.call(verb, f"StorageCenter/{path}", payload)
            got = members("UserList")
            print(f"  {verb} {path.split('/', 2)[-1]} {list(payload)} accepted -> UserList: {got}")
            if got:
                break
        except RuntimeError as e:
            print(f"  {verb} {path.split('/', 2)[-1]}: {str(e)[60:170]}")


def cmd_csp_user(sc, a):
    """Dedicated SC account for the CSP.

    SCOS 7.2 direct REST (no DSM) only accepts Admin-privilege logins: VolumeManager users get HTTP 401,
    and privileges can only be upgraded, never downgraded. So the account is Admin by default; scoping is
    enforced by the CSP's folder guard. The user group (folders) is still created so the account can
    be switched to VolumeManager when a DSM Data Collector fronts the array."""
    import secrets
    import string
    vf = _folder(sc, "ScVolumeFolder", a.folder)
    sf = _folder(sc, "ScServerFolder", a.folder)
    df = [d["instanceId"] for d in sc.getlist("ScDiskFolder", scSerialNumber=sc.ssn) if d.get("name") == "Assigned"]
    groups = [g for g in sc.getlist("ScUserGroup", scSerialNumber=sc.ssn) if g.get("name") == a.folder]
    if groups:
        gid = groups[0]["instanceId"]
        print(f"  user group {a.folder} exists ({gid})")
    else:
        g = sc.call("POST", "StorageCenter/ScUserGroup", {
            "Name": a.folder, "StorageCenter": sc.ssn,
            "VolumeFolderList": [vf], "ServerFolderList": [sf], "DiskFolderList": df})
        gid = g["instanceId"]
        print(f"  created user group {a.folder} ({gid})")
    alphabet = string.ascii_letters + string.digits
    password = "".join(secrets.choice(alphabet) for _ in range(20)) + "7a"
    out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".env", "csp.env")

    def save():
        fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            f.write(f"# scseries-csp service account on SC {sc.ssn} (VolumeManager, folders {a.folder})\n"
                    f"CSP_SC_USER={a.user}\nCSP_SC_PASSWORD={password}\n")
        print(f"  credentials written to {os.path.normpath(out)} (mode 600)")

    users = [u for u in sc.getlist("ScUser", scSerialNumber=sc.ssn) if u.get("name") == a.user]
    if users:
        uid = users[0]["instanceId"]
        print(f"  user {a.user} exists ({uid}, privilege {users[0].get('privilege')})")
        _link_group(sc, gid, vf, sf, df, uid)
        if a.privilege:
            r = sc.call("PUT", f"StorageCenter/ScUser/{uid}", {"Privilege": a.privilege})
            print(f"  privilege of {a.user} -> {r.get('privilege') if r else '?'}")
        if a.reset_password:
            sc.call("POST", f"StorageCenter/ScUser/{uid}/ResetPassword",
                    {"AuthorizationPassword": os.environ["SC_PASSWORD"], "NewPassword": password})
            print(f"  password of {a.user} reset")
            save()
        return
    u = sc.call("POST", "StorageCenter/ScUser", {
        "Name": a.user, "RealName": "scseries-csp", "Password": password, "Privilege": a.privilege or "Admin",
        "SessionTimeout": "Hours12", "StorageCenter": sc.ssn, "Notes": "scseries-csp service account",
        "UserGroupList": [gid]})
    print(f"  created user {a.user} ({u['instanceId']}, privilege {u.get('privilege')})")
    _link_group(sc, gid, vf, sf, df, u["instanceId"])
    # ScUser POST ignores Password on SCOS 7.2; set it explicitly
    sc.call("POST", f"StorageCenter/ScUser/{u['instanceId']}/ResetPassword",
            {"AuthorizationPassword": os.environ["SC_PASSWORD"], "NewPassword": password})
    save()


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("status")
    sub.add_parser("rebalance")
    k = sub.add_parser("ack")
    k.add_argument("--before", required=True)
    k.add_argument("--yes", action="store_true")
    h = sub.add_parser("hardware")
    h.add_argument("--all", action="store_true", help="list every disk, not only suspect ones")
    h.add_argument("--json", help="also save full disk data to this file")
    w = sub.add_parser("wipe-volumes")
    w.add_argument("--match", default=".*", help="regex on volume name (default: all)")
    w.add_argument("--yes", action="store_true")
    w.add_argument("--no-unmap", action="store_true", help="skip mapping removal (vVols bound via protocol endpoints)")
    s = sub.add_parser("wipe-servers")
    s.add_argument("--match", default=".*", help="regex on server name (default: all)")
    s.add_argument("--yes", action="store_true")
    s.add_argument("--force", action="store_true", help="also try servers that still have mappings (e.g. vVol bindings)")
    u = sub.add_parser("csp-user")
    u.add_argument("--user", default="scseries-csp")
    u.add_argument("--folder", default="scseries-csp")
    u.add_argument("--reset-password", action="store_true", help="set a new password and rewrite .env/csp.env")
    u.add_argument("--privilege", choices=["VolumeManager", "Reporter", "Admin"], help="privilege for a new user (default Admin), or upgrade an existing one")
    r = sub.add_parser("remove-hbas")
    r.add_argument("--match", required=True, help="regex on server name")
    r.add_argument("--yes", action="store_true")
    a = p.parse_args()
    sc = _probe.SC()
    sc.login()
    try:
        {"status": cmd_status, "rebalance": cmd_rebalance, "ack": cmd_ack, "hardware": cmd_hardware,
         "wipe-volumes": cmd_wipe_volumes, "wipe-servers": cmd_wipe_servers,
         "remove-hbas": cmd_remove_hbas, "csp-user": cmd_csp_user}[a.cmd](sc, a)
    finally:
        sc.logout()


if __name__ == "__main__":
    main()
