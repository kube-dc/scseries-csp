// Package scfake is an in-memory Storage Center REST API for unit tests. It implements the calls the CSP
// uses and reproduces behaviours observed on a real SC5020 (SCOS 7.2.30): API-version negotiation on login,
// one-element lists from CreateReplay/CreateView, 31-character replay descriptions, view volumes inheriting
// their parent's replay, DELETE moving a volume to the recycle bin first, and no replays of never-written
// volumes.
package scfake

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// SSN is the synthetic Storage Center serial number of the fake array.
const SSN = "64012"

const ssnNum = 64012

type obj = map[string]any

// Array is the fake Storage Center.
type Array struct {
	Server   *httptest.Server
	User     string
	Password string

	mu       sync.Mutex
	next     int
	volumes  map[string]obj
	vconf    map[string]obj
	folders  map[string]obj // volume folders
	sfolders map[string]obj // server folders
	servers  map[string]obj
	hbas     map[string]obj
	profiles map[string]obj // mapping profiles: id -> {volume, server}
	mappings map[string][]obj
	replays  map[string]obj
	ports    map[string]obj
	named    map[string][]obj // ScStorageProfile, ScDataReductionProfile, ScReplayProfile, ScServerOperatingSystem
	Calls    []string

	// health, for the monitor
	Status string
	Disks  []obj
	Alerts []obj
}

// New starts a fake array with one admin user and two virtual front-end ports.
func New() *Array {
	a := &Array{User: "Admin", Password: "secret", volumes: map[string]obj{}, vconf: map[string]obj{}, folders: map[string]obj{},
		sfolders: map[string]obj{}, servers: map[string]obj{}, hbas: map[string]obj{}, profiles: map[string]obj{},
		mappings: map[string][]obj{}, replays: map[string]obj{}, ports: map[string]obj{}, named: map[string][]obj{}, next: 100,
		Status: "Up"}
	for _, n := range []string{"Recommended (All Tiers)", "High Priority (Tier 1)", "Low Priority (Tier 3)"} {
		a.named["ScStorageProfile"] = append(a.named["ScStorageProfile"], obj{"instanceId": a.id(), "name": n, "instanceName": n})
	}
	for _, n := range []string{"None", "Compression", "Deduplication with Compression"} {
		a.named["ScDataReductionProfile"] = append(a.named["ScDataReductionProfile"], obj{"instanceId": a.id(), "name": n, "instanceName": n})
	}
	a.named["ScReplayProfile"] = []obj{{"instanceId": a.id(), "name": "Daily", "instanceName": "Daily"}}
	for _, n := range []string{"VMware ESXi 6.5", "Red Hat Linux 7.x"} {
		a.named["ScServerOperatingSystem"] = append(a.named["ScServerOperatingSystem"], obj{"instanceId": a.id(), "name": n, "instanceName": n})
	}
	for i := 0; i < 2; i++ { // virtual front-end ports, one per fabric: 5000D31 + serial + port index
		w := fmt.Sprintf("5000D31%07X%02X", ssnNum, 0x3A+2*i)
		id := fmt.Sprintf("%s.port%d", SSN, i)
		a.ports[id] = obj{"instanceId": id, "name": w, "wwn": w}
	}
	a.Server = httptest.NewTLSServer(http.HandlerFunc(a.serve))
	return a
}

// Close stops the server.
func (a *Array) Close() { a.Server.Close() }

// Host and Port of the fake endpoint.
func (a *Array) HostPort() (string, string) {
	hp := strings.TrimPrefix(a.Server.URL, "https://")
	i := strings.LastIndex(hp, ":")
	return hp[:i], hp[i+1:]
}

func (a *Array) id() string { a.next++; return fmt.Sprintf("%s.%d", SSN, a.next) }

// AddServer seeds a server (e.g. a legacy ESXi object outside the CSP folder) owning the given WWPNs.
func (a *Array) AddServer(name, folder string, wwpns ...string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := a.id()
	path := ""
	if folder != "" {
		path = folder + "/"
	}
	a.servers[id] = obj{"instanceId": id, "name": name, "instanceName": name, "notes": "", "serverFolderPath": path, "type": "Physical", "scSerialNumber": ssnNum}
	for _, w := range wwpns {
		hid := a.id()
		a.hbas[hid] = obj{"instanceId": hid, "instanceName": strings.ToUpper(w), "server": ref(id, name), "scSerialNumber": ssnNum}
	}
	return id
}

// AddVolume seeds a volume in folder ("" = root).
func (a *Array) AddVolume(name, folder string, gib int) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.newVolume(name, folder, int64(gib)<<30, "")
}

// SetActive marks a volume as written (Storage Center only snapshots active volumes).
func (a *Array) SetActive(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.volumes[id]["active"] = true
}

// Volume returns a copy of a volume or nil.
func (a *Array) Volume(id string) obj {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v, ok := a.volumes[id]; ok {
		c := obj{}
		for k, x := range v {
			c[k] = x
		}
		return c
	}
	return nil
}

// Mappings returns server names a volume is mapped to.
func (a *Array) Mappings(volID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, p := range a.profiles {
		if p["volume"] == volID {
			out = append(out, a.servers[p["server"].(string)]["name"].(string))
		}
	}
	sort.Strings(out)
	return out
}

// Replays returns live (not expired) replay ids of a volume, including inherited ones.
func (a *Array) Replays(volID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for id, r := range a.replays {
		if r["volume"] == volID {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Servers returns the server objects by name.
func (a *Array) Servers() map[string]obj {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]obj{}
	for _, s := range a.servers {
		out[s["name"].(string)] = s
	}
	return out
}

func ref(id, name string) obj { return obj{"instanceId": id, "instanceName": name} }

func (a *Array) newVolume(name, folder string, size int64, parentReplay string) string {
	id := a.id()
	idx, _ := strconv.Atoi(strings.TrimPrefix(id, SSN+"."))
	path := ""
	if folder != "" {
		path = folder + "/"
	}
	a.volumes[id] = obj{"instanceId": id, "name": name, "instanceName": name, "configuredSize": fmt.Sprintf("%d Bytes", size),
		// NAA layout seen on SC arrays: 6000d31 + 7-hex serial + 16-hex volume index
		"deviceId": fmt.Sprintf("6000d31%07x%016x", ssnNum, idx), "serialNumber": fmt.Sprintf("%08x-%08x", ssnNum, idx), "volumeFolderPath": path,
		"mapped": false, "active": false, "inRecycleBin": false, "scSerialNumber": ssnNum, "parentReplay": parentReplay}
	a.vconf[id] = obj{"storageProfile": ref("x", "Recommended (All Tiers)"), "dataReductionProfile": ref("y", "Deduplication with Compression"),
		"replayProfileList": []any{"Daily"}}
	return id
}

func (a *Array) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/rest/")
	a.Calls = append(a.Calls, r.Method+" "+path)
	if !a.authorized(r) {
		http.Error(w, "<html>401 Authorization Required</html>", http.StatusUnauthorized)
		return
	}
	var body obj
	_ = json.NewDecoder(r.Body).Decode(&body)
	status, out := a.route(r.Method, path, body, r.Header.Get("x-dell-api-version"))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if out != nil {
		_ = json.NewEncoder(w).Encode(out)
	}
}

func (a *Array) authorized(r *http.Request) bool {
	h := strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
	b, err := base64.StdEncoding.DecodeString(h)
	return err == nil && string(b) == a.User+":"+a.Password
}

func errBody(msg string) obj { return obj{"result": msg} }

func size(s string) int64 {
	f := strings.Fields(s)
	n, _ := strconv.ParseInt(f[0], 10, 64)
	if len(f) > 1 && strings.EqualFold(f[1], "GB") {
		n <<= 30
	}
	return n
}

func (a *Array) route(method, path string, body obj, apiVersion string) (int, any) {
	p := strings.Split(strings.TrimPrefix(path, "StorageCenter/"), "/")
	switch {
	case path == "ApiConnection/Login":
		if v, _ := strconv.ParseFloat(apiVersion, 64); v < 3.0 || v > 3.4 {
			return 400, errBody("Invalid API version specified, the version must be in the range [3.0, 3.4]")
		}
		return 200, obj{"apiVersion": "3.4", "provider": "StorageCenter", "connected": true}
	case path == "ApiConnection/Logout":
		return 204, nil
	case path == "StorageCenter/StorageCenter":
		return 200, []obj{{"instanceId": SSN, "name": "SC-LAB", "scSerialNumber": ssnNum, "version": "07.02.30.021", "status": a.Status}}
	}
	if len(p) == 2 && p[1] == "GetList" && method == "POST" {
		return 200, a.getList(p[0], body)
	}
	kind := p[0]
	id := ""
	if len(p) > 1 {
		id = p[1]
	}
	action := ""
	if len(p) > 2 {
		action = p[2]
	}
	switch kind + "/" + action {
	case "ScVolume/":
		switch method {
		case "POST":
			name := body["Name"].(string)
			v := a.newVolume(name, a.folderName(a.folders, body["VolumeFolder"]), size(body["Size"].(string)), "")
			if sp, ok := body["StorageProfile"].(string); ok {
				a.vconf[v]["storageProfile"] = a.namedRef("ScStorageProfile", sp)
			}
			if dr, ok := body["DataReductionProfile"].(string); ok {
				a.vconf[v]["dataReductionProfile"] = a.namedRef("ScDataReductionProfile", dr)
			}
			return 201, a.volumes[v]
		case "GET":
			if v, ok := a.volumes[id]; ok {
				return 200, v
			}
			return 400, errBody("not found")
		case "DELETE":
			v, ok := a.volumes[id]
			if !ok {
				return 400, errBody("not found")
			}
			if v["mapped"] == true {
				return 400, errBody("Volume is mapped")
			}
			if v["inRecycleBin"] == true {
				delete(a.volumes, id)
				for rid, r := range a.replays {
					if r["volume"] == id {
						delete(a.replays, rid)
					}
				}
				return 200, obj{"result": true}
			}
			v["inRecycleBin"] = true
			return 200, obj{"result": true}
		}
	case "ScVolume/VolumeConfiguration":
		return 200, a.vconf[id]
	case "ScVolumeConfiguration/":
		if l, ok := body["ReplayProfileList"]; ok {
			a.vconf[id]["replayProfileList"] = l
		}
		return 200, a.vconf[id]
	case "ScVolume/ExpandToSize":
		a.volumes[id]["configuredSize"] = fmt.Sprintf("%d Bytes", size(body["NewSize"].(string)))
		return 200, a.volumes[id]
	case "ScVolume/MapToServer":
		return a.mapToServer(id, body["server"].(string))
	case "ScVolume/MappingList":
		return 200, a.mappingList(id)
	case "ScVolume/MappingProfileList":
		var out []obj
		for pid, pr := range a.profiles {
			if pr["volume"] == id {
				sid := pr["server"].(string)
				out = append(out, obj{"instanceId": pid, "server": ref(sid, a.servers[sid]["name"].(string))})
			}
		}
		return 200, out
	case "ScVolume/CreateReplay":
		v := a.volumes[id]
		if v["active"] != true {
			return 400, errBody("Unable to create snapshot from empty volume")
		}
		desc := body["description"].(string)
		if len(desc) > 31 {
			desc = desc[:31]
		}
		rid := fmt.Sprintf("%s.%d", id, len(a.replays)+1)
		a.replays[rid] = obj{"instanceId": rid, "description": desc, "active": false, "freezeTime": "2026-10-17T21:56:13+03:00",
			"size": "6815744 Bytes", "createVolume": ref(id, v["name"].(string)), "volume": id}
		return 200, []obj{a.replays[rid]}
	case "ScVolume/ReplayList":
		v := a.volumes[id]
		out := []obj{{"instanceId": id + ".active", "description": "", "active": true, "createVolume": ref(id, v["name"].(string))}}
		ids := []string{}
		for rid, r := range a.replays {
			if r["volume"] == id || rid == v["parentReplay"] {
				ids = append(ids, rid)
			}
		}
		sort.Strings(ids)
		for _, rid := range ids {
			out = append(out, a.replays[rid])
		}
		return 200, out
	case "ScReplay/":
		if r, ok := a.replays[id]; ok {
			return 200, r
		}
		return 400, errBody("not found")
	case "ScReplay/Expire":
		delete(a.replays, id)
		return 204, nil
	case "ScReplay/CreateView":
		r, ok := a.replays[id]
		if !ok {
			return 400, errBody("not found")
		}
		src := a.volumes[r["volume"].(string)]
		v := a.newVolume(body["Name"].(string), a.folderName(a.folders, body["VolumeFolder"]), size(src["configuredSize"].(string)), id)
		a.volumes[v]["active"] = true
		return 200, []obj{a.volumes[v]}
	case "ScVolumeFolder/":
		return 201, a.newFolder(a.folders, body)
	case "ScServerFolder/":
		return 201, a.newFolder(a.sfolders, body)
	case "ScPhysicalServer/":
		if method == "PUT" {
			if n, ok := body["Notes"]; ok {
				a.servers[id]["notes"] = n
			}
			return 200, a.servers[id]
		}
		sid := a.id()
		name := body["Name"].(string)
		a.servers[sid] = obj{"instanceId": sid, "name": name, "instanceName": name, "notes": body["Notes"],
			"serverFolderPath": a.folderName(a.sfolders, body["ServerFolder"]) + "/", "type": "Physical", "scSerialNumber": ssnNum}
		return 201, a.servers[sid]
	case "ScPhysicalServer/AddHba":
		hid := a.id()
		a.hbas[hid] = obj{"instanceId": hid, "instanceName": body["WwnOrIscsiName"], "server": ref(id, a.servers[id]["name"].(string)), "scSerialNumber": ssnNum}
		return 200, a.hbas[hid]
	case "ScServer/":
		if s, ok := a.servers[id]; ok {
			return 200, s
		}
		return 400, errBody("not found")
	case "ScServer/HbaList":
		var out []obj
		for _, h := range a.hbas {
			if h["server"].(obj)["instanceId"] == id {
				out = append(out, h)
			}
		}
		return 200, out
	case "ScMappingProfile/":
		pr, ok := a.profiles[id]
		if !ok {
			return 400, errBody("not found")
		}
		vol := pr["volume"].(string)
		delete(a.profiles, id)
		keep := a.mappings[vol][:0]
		for _, m := range a.mappings[vol] {
			if m["profile"].(obj)["instanceId"] != id {
				keep = append(keep, m)
			}
		}
		a.mappings[vol] = keep
		a.volumes[vol]["mapped"] = len(keep) > 0
		return 200, obj{"result": true}
	case "ScControllerPort/":
		return 200, a.ports[id]
	case "StorageCenter/StorageUsage":
		return 200, obj{"availableSpace": "185677074358272 Bytes", "freeSpace": "150373271904256 Bytes",
			"usedSpace": "35303802454016 Bytes", "allocatedSpace": "101469418946560 Bytes", "configuredSpace": "174221424590848 Bytes"}
	case "StorageCenter/DiskList":
		return 200, a.Disks
	case "StorageCenter/ActiveAlertList":
		return 200, a.Alerts
	}
	return 400, errBody(fmt.Sprintf("Invalid method %s %s", method, path))
}

func (a *Array) namedRef(kind, id string) obj {
	for _, n := range a.named[kind] {
		if n["instanceId"] == id {
			return ref(id, n["name"].(string))
		}
	}
	return ref(id, "?")
}

func (a *Array) folderName(folders map[string]obj, id any) string {
	if f, ok := folders[fmt.Sprint(id)]; ok {
		return f["name"].(string)
	}
	return ""
}

func (a *Array) newFolder(folders map[string]obj, body obj) obj {
	id := a.id()
	folders[id] = obj{"instanceId": id, "name": body["Name"], "instanceName": body["Name"], "scSerialNumber": ssnNum}
	return folders[id]
}

func (a *Array) mapToServer(volID, serverID string) (int, any) {
	if _, ok := a.servers[serverID]; !ok {
		return 400, errBody("server not found")
	}
	pid := a.id()
	a.profiles[pid] = obj{"volume": volID, "server": serverID}
	used := map[float64]bool{}
	for _, ms := range a.mappings {
		for _, m := range ms {
			if m["server"].(obj)["instanceId"] == serverID {
				used[m["lun"].(float64)] = true
			}
		}
	}
	lun := float64(1)
	for used[lun] {
		lun++
	}
	port := 0
	ports := []string{SSN + ".port0", SSN + ".port1"}
	hbaIDs := []string{}
	for hid, h := range a.hbas {
		if h["server"].(obj)["instanceId"] == serverID {
			hbaIDs = append(hbaIDs, hid)
		}
	}
	sort.Strings(hbaIDs)
	for _, hid := range hbaIDs {
		a.mappings[volID] = append(a.mappings[volID], obj{"instanceId": a.id(), "lun": lun,
			"server": ref(serverID, a.servers[serverID]["name"].(string)), "serverHba": ref(hid, a.hbas[hid]["instanceName"].(string)),
			"controllerPort": ref(ports[port%2], ""), "profile": ref(pid, "")})
		port++
	}
	a.volumes[volID]["mapped"] = true
	return 200, obj{"instanceId": pid}
}

func (a *Array) mappingList(volID string) []obj {
	out := []obj{}
	out = append(out, a.mappings[volID]...)
	return out
}

func (a *Array) getList(kind string, body obj) []obj {
	var src []obj
	switch kind {
	case "ScVolume":
		for _, v := range a.volumes {
			src = append(src, v)
		}
	case "ScVolumeFolder":
		for _, f := range a.folders {
			src = append(src, f)
		}
	case "ScServerFolder":
		for _, f := range a.sfolders {
			src = append(src, f)
		}
	case "ScPhysicalServer", "ScServer":
		for _, s := range a.servers {
			src = append(src, s)
		}
	case "ScServerHba":
		for _, h := range a.hbas {
			src = append(src, h)
		}
	default:
		src = a.named[kind]
	}
	filters := []any{}
	if f, ok := body["filter"].(obj); ok {
		filters, _ = f["filters"].([]any)
	}
	out := []obj{}
	for _, o := range src {
		ok := true
		for _, fl := range filters {
			f := fl.(obj)
			key, want := f["attributeName"].(string), fmt.Sprint(f["attributeValue"])
			got := ""
			for k, v := range o {
				if strings.EqualFold(k, key) {
					got = fmt.Sprint(v)
				}
			}
			if strings.EqualFold(key, "scSerialNumber") {
				continue
			}
			if got != want {
				ok = false
			}
		}
		if ok {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]["instanceId"]) < fmt.Sprint(out[j]["instanceId"]) })
	return out
}

// Fingerprint returns the SHA-256 fingerprint (hex) of the fake's TLS certificate.
func (a *Array) Fingerprint() string {
	sum := sha256.Sum256(a.Server.Certificate().Raw)
	return hex.EncodeToString(sum[:])
}

// VolumeNames returns the names of all volumes (including recycled ones).
func (a *Array) VolumeNames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, v := range a.volumes {
		out = append(out, v["name"].(string))
	}
	sort.Strings(out)
	return out
}
