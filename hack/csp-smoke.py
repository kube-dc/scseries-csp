#!/usr/bin/env python3
"""Drive a running scseries-csp through the HPE CSP contract, step by step.

  csp-smoke.py [--url http://127.0.0.1:18080] STEP [args]

Steps (state is kept in /tmp/csp-smoke.json between calls):
  token [--csp-user]            POST /tokens with SC_HOST + SC_USER/SC_PASSWORD (or CSP_SC_* with --csp-user)
  host NAME UUID WWPN [WWPN..]  POST /hosts
  create NAME SIZE_GIB          POST /volumes   (then GET ?name= for idempotency)
  publish UUID                  PUT  /volumes/{id}/actions/publish
  unpublish UUID                PUT  /volumes/{id}/actions/unpublish
  expand SIZE_GIB               PUT  /volumes/{id}
  delete                        DELETE /volumes/{id}
  snapshot NAME                 POST /snapshots on the current volume (then GET ?name=&volume_id=)
  clone NAME                    POST /volumes clone from the last snapshot; becomes the current volume
  use VOLUME_ID                 switch the current volume
  delete-snapshot               DELETE /snapshots/{id}
  list                          GET  /volumes
"""
import argparse
import json
import os
import sys
import urllib.error
import urllib.request

STATE = "/tmp/csp-smoke.json"
REPO = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")


def env():
    vals = dict(os.environ)
    d = os.path.join(REPO, ".env")
    files = [os.path.join(d, f) for f in sorted(os.listdir(d)) if f.endswith(".env")] if os.path.isdir(d) else \
        ([d] if os.path.isfile(d) else [])
    for f in files:
        for line in open(f):
            line = line.strip().removeprefix("export ")
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                vals.setdefault(k.strip(), v.strip().strip("'\""))
    return vals


def call(url, method, path, body=None, token=None, array=None):
    headers = {"Content-Type": "application/json"}
    if token:
        headers.update({"x-auth-token": token, "x-array-ip": array})
    req = urllib.request.Request(url + path, method=method, headers=headers,
                                 data=json.dumps(body).encode() if body is not None else None)
    try:
        with urllib.request.urlopen(req, timeout=600) as r:
            status, text = r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        status, text = e.code, e.read().decode()
    print(f"{method} {path} -> {status}")
    out = json.loads(text) if text.strip() else None
    if out is not None:
        print(json.dumps(out, indent=1))
    return status, out


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--url", default="http://127.0.0.1:18080")
    p.add_argument("--csp-user", action="store_true", help="log in with CSP_SC_USER/CSP_SC_PASSWORD (.env/csp.env)")
    p.add_argument("step")
    p.add_argument("args", nargs="*")
    a = p.parse_args()
    st = json.load(open(STATE)) if os.path.exists(STATE) else {}
    base = "/containers/v1"
    t, arr = st.get("token"), st.get("array")

    if a.step == "token":
        e = env()
        user, pw = (e["CSP_SC_USER"], e["CSP_SC_PASSWORD"]) if a.csp_user else (e["SC_USER"], e["SC_PASSWORD"])
        status, out = call(a.url, "POST", f"{base}/tokens",
                           {"array_ip": e["SC_HOST"], "username": user, "password": pw})
        if status == 200:
            st.update(token=out["session_token"], array=e["SC_HOST"])
    elif a.step == "host":
        name, uid, *wwpns = a.args
        call(a.url, "POST", f"{base}/hosts", {"name": name, "uuid": uid, "wwpns": wwpns}, t, arr)
    elif a.step == "create":
        name, gib = a.args
        status, out = call(a.url, "POST", f"{base}/volumes",
                           {"name": name, "size": int(float(gib) * 1024 ** 3), "description": "csp smoke test",
                            "config": {}}, t, arr)
        if status == 200:
            st["volume"] = out["id"]
            call(a.url, "GET", f"{base}/volumes?name={name}", None, t, arr)
    elif a.step == "publish":
        call(a.url, "PUT", f"{base}/volumes/{st['volume']}/actions/publish",
             {"host_uuid": a.args[0], "access_protocol": "fc"}, t, arr)
    elif a.step == "unpublish":
        call(a.url, "PUT", f"{base}/volumes/{st['volume']}/actions/unpublish", {"host_uuid": a.args[0]}, t, arr)
    elif a.step == "expand":
        call(a.url, "PUT", f"{base}/volumes/{st['volume']}", {"size": int(float(a.args[0]) * 1024 ** 3)}, t, arr)
    elif a.step == "delete":
        call(a.url, "DELETE", f"{base}/volumes/{st['volume']}?force=false", None, t, arr)
    elif a.step == "snapshot":
        status, out = call(a.url, "POST", f"{base}/snapshots",
                           {"name": a.args[0], "volume_id": st["volume"], "description": a.args[0]}, t, arr)
        if status == 200:
            st["snapshot"] = out["id"]
            call(a.url, "GET", f"{base}/snapshots?name={a.args[0]}&volume_id={st['volume']}", None, t, arr)
    elif a.step == "clone":
        status, out = call(a.url, "POST", f"{base}/volumes",
                           {"name": a.args[0], "base_snapshot_id": st["snapshot"], "clone": True, "config": {}}, t, arr)
        if status == 200:
            st["source_volume"], st["volume"] = st["volume"], out["id"]
    elif a.step == "use":
        st["volume"] = a.args[0]
    elif a.step == "delete-snapshot":
        call(a.url, "DELETE", f"{base}/snapshots/{st['snapshot']}", None, t, arr)
    elif a.step == "list":
        call(a.url, "GET", f"{base}/volumes", None, t, arr)
    else:
        sys.exit(f"unknown step {a.step}")
    json.dump(st, open(STATE, "w"))


if __name__ == "__main__":
    main()
