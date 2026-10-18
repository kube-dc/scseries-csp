package csp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kube-dc/scseries-csp/internal/sc"
)

const (
	gib          = int64(1) << 30
	noteManaged  = "scseries-csp managed"
	noteUUID     = "csp-node-uuid="     // written into the Notes of server objects the CSP owns
	legacyUUID   = "kube-dc-node-uuid=" // tag written by earlier releases; still recognised
	helperPrefix = "snap-for-clone-"    // HPE driver's temporary snapshot for PVC->PVC clones
	descMax      = 31                   // SC truncates replay descriptions to 31 characters

	// ArrayDefault as snapshot profile keeps the array's default snapshot schedule on new volumes.
	ArrayDefault = "array-default"
)

var uuidTag = regexp.MustCompile(`(?:` + regexp.QuoteMeta(noteUUID) + `|` + regexp.QuoteMeta(legacyUUID) + `)(\S+)`)

// NodeUUIDFromNotes returns the node UUID recorded in a server object's Notes ("" if none).
func NodeUUIDFromNotes(notes string) string {
	if m := uuidTag.FindStringSubmatch(notes); m != nil {
		return m[1]
	}
	return ""
}

// Config holds the array-side conventions; StorageClass parameters override the volume defaults.
type Config struct {
	VolumeFolder    string // SC volume folder owned by the CSP (created on first use)
	ServerFolder    string // SC server folder owned by the CSP (created on first use)
	ServerOS        string // SC operating-system type for new server objects
	StorageProfile  string // "" = the array's default storage profile
	DataReduction   string // "" = the array's default data reduction profile
	SnapshotProfile string // "" = no snapshot schedule (CSI manages snapshots); ArrayDefault keeps the array's
}

// Defaults returns neutral conventions; deployments override them (see the Helm chart values).
func Defaults() Config {
	return Config{
		VolumeFolder: "scseries-csp",
		ServerFolder: "scseries-csp",
		ServerOS:     "Red Hat Linux 7.x",
	}
}

// Service implements the CSP operations against one Storage Center session.
type Service struct {
	Cfg Config
	Log *slog.Logger
}

// ---------------------------------------------------------------------------------------------- helpers

func inFolder(path, folder string) bool {
	p := strings.Trim(path, "/")
	return p != "" && strings.SplitN(p, "/", 2)[0] == folder
}

func (s *Service) toVolume(ctx context.Context, c *sc.Client, v *sc.Volume) Volume {
	cfg := map[string]any{}
	var vc sc.VolumeConfiguration
	if err := c.Get(ctx, "StorageCenter/ScVolume/"+v.InstanceID+"/VolumeConfiguration", &vc); err == nil {
		if vc.StorageProfile != nil {
			cfg["storage_profile"] = vc.StorageProfile.InstanceName
		}
		if vc.DataReductionProfile != nil {
			cfg["data_reduction_profile"] = vc.DataReductionProfile.InstanceName
		}
	}
	return Volume{
		ID: v.InstanceID, Name: v.Name, Size: sc.Bytes(v.ConfiguredSize), Published: v.Mapped,
		SerialNumber: v.DeviceID, Config: cfg,
	}
}

// getVolume returns a volume the CSP owns, or 404.
func (s *Service) getVolume(ctx context.Context, c *sc.Client, id string) (*sc.Volume, error) {
	if id == "" {
		return nil, errorf(http.StatusNotFound, "volume id is empty")
	}
	var v sc.Volume
	if err := c.Get(ctx, "StorageCenter/ScVolume/"+id, &v); err != nil {
		if sc.IsStatus(err, 400) || sc.IsStatus(err, 404) {
			return nil, errorf(http.StatusNotFound, "volume %s not found", id)
		}
		return nil, err
	}
	if v.InstanceID == "" || v.InRecycleBin || !inFolder(v.VolumeFolderPath, s.Cfg.VolumeFolder) {
		return nil, errorf(http.StatusNotFound, "volume %s not found", id)
	}
	return &v, nil
}

func (s *Service) ownedVolumes(ctx context.Context, c *sc.Client, filters map[string]any) ([]sc.Volume, error) {
	var all []sc.Volume
	if err := c.GetList(ctx, "ScVolume", filters, &all); err != nil {
		return nil, err
	}
	out := all[:0]
	for _, v := range all {
		if inFolder(v.VolumeFolderPath, s.Cfg.VolumeFolder) && !v.InRecycleBin {
			out = append(out, v)
		}
	}
	return out, nil
}

func (s *Service) findServerByUUID(ctx context.Context, c *sc.Client, uuid string) (*sc.Server, error) {
	if uuid == "" {
		return nil, nil
	}
	var servers []sc.Server
	if err := c.GetList(ctx, "ScPhysicalServer", nil, &servers); err != nil {
		return nil, err
	}
	for i := range servers {
		if NodeUUIDFromNotes(servers[i].Notes) == uuid && inFolder(servers[i].ServerFolderPath, s.Cfg.ServerFolder) {
			return &servers[i], nil
		}
	}
	return nil, nil
}

func ceilGiB(b int64) int64 {
	g := (b + gib - 1) / gib
	if g < 1 {
		g = 1
	}
	return g
}

// camel converts the driver's snake_cased StorageClass parameters back (storage_profile -> storageProfile).
func camel(conf map[string]any) map[string]string {
	re := regexp.MustCompile(`_([a-z])`)
	out := map[string]string{}
	for k, v := range conf {
		key := re.ReplaceAllStringFunc(k, func(m string) string { return strings.ToUpper(m[1:]) })
		out[key] = fmt.Sprint(v)
	}
	return out
}

func pick(conf map[string]string, key, def string) string {
	if v, ok := conf[key]; ok {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------------------------- volumes

// ListVolumes is GET /volumes[?name=].
func (s *Service) ListVolumes(ctx context.Context, c *sc.Client, name string) ([]Volume, error) {
	filters := map[string]any{}
	if name != "" {
		filters["Name"] = name
	}
	vols, err := s.ownedVolumes(ctx, c, filters)
	if err != nil {
		return nil, err
	}
	if name != "" && len(vols) == 0 {
		return nil, errorf(http.StatusNotFound, "volume %s not found", name)
	}
	out := make([]Volume, 0, len(vols))
	for i := range vols {
		out = append(out, s.toVolume(ctx, c, &vols[i]))
	}
	return out, nil
}

// GetVolume is GET /volumes/{id}.
func (s *Service) GetVolume(ctx context.Context, c *sc.Client, id string) (Volume, error) {
	v, err := s.getVolume(ctx, c, id)
	if err != nil {
		return Volume{}, err
	}
	return s.toVolume(ctx, c, v), nil
}

// CreateVolume is POST /volumes (plain volume, or clone from a snapshot).
func (s *Service) CreateVolume(ctx context.Context, c *sc.Client, req CreateVolumeRequest) (Volume, error) {
	if req.Clone || req.BaseSnapshotID != "" {
		if req.Name == "" || req.BaseSnapshotID == "" {
			return Volume{}, errorf(http.StatusBadRequest, "clone needs name and base_snapshot_id")
		}
		return s.cloneVolume(ctx, c, req)
	}
	if req.Name == "" || req.Size <= 0 {
		return Volume{}, errorf(http.StatusBadRequest, "name and size are required")
	}
	conf := camel(req.Config)
	c.Mutate.Lock()
	defer c.Mutate.Unlock()
	if existing, err := s.ownedVolumes(ctx, c, map[string]any{"Name": req.Name}); err != nil {
		return Volume{}, err
	} else if len(existing) > 0 {
		s.Log.Info("volume already exists", "name", req.Name, "id", existing[0].InstanceID)
		return s.toVolume(ctx, c, &existing[0]), nil
	}
	folder, err := c.FolderID(ctx, "ScVolumeFolder", s.Cfg.VolumeFolder, noteManaged)
	if err != nil {
		return Volume{}, err
	}
	g := ceilGiB(req.Size)
	payload := map[string]any{
		"Name": req.Name, "Size": fmt.Sprintf("%d GB", g), "StorageCenter": c.SSN(), "VolumeFolder": folder,
		"Notes": strings.Trim(noteManaged+"; "+req.Description, "; "),
	}
	if sp := pick(conf, "storageProfile", s.Cfg.StorageProfile); sp != "" {
		id, err := c.NamedID(ctx, "ScStorageProfile", sp)
		if err != nil {
			return Volume{}, errorf(http.StatusBadRequest, "%v", err)
		}
		payload["StorageProfile"] = id
	}
	if dr := pick(conf, "dataReductionProfile", s.Cfg.DataReduction); dr != "" {
		id, err := c.NamedID(ctx, "ScDataReductionProfile", dr)
		if err != nil {
			return Volume{}, errorf(http.StatusBadRequest, "%v", err)
		}
		payload["DataReductionProfile"] = id
	}
	var v sc.Volume
	if err := c.Post(ctx, "StorageCenter/ScVolume", payload, &v); err != nil {
		return Volume{}, err
	}
	if err := s.setSnapshotProfile(ctx, c, v.InstanceID, pick(conf, "snapshotProfile", s.Cfg.SnapshotProfile)); err != nil {
		return Volume{}, err
	}
	s.Log.Info("created volume", "name", req.Name, "id", v.InstanceID, "gb", g, "deviceId", v.DeviceID)
	if err := c.Get(ctx, "StorageCenter/ScVolume/"+v.InstanceID, &v); err != nil {
		return Volume{}, err
	}
	return s.toVolume(ctx, c, &v), nil
}

// setSnapshotProfile replaces the array default (usually a Daily schedule) by name, or by none.
func (s *Service) setSnapshotProfile(ctx context.Context, c *sc.Client, volID, name string) error {
	if name == ArrayDefault {
		return nil
	}
	ids := []string{}
	if name != "" {
		id, err := c.NamedID(ctx, "ScReplayProfile", name)
		if err != nil {
			return errorf(http.StatusBadRequest, "%v", err)
		}
		ids = append(ids, id)
	}
	return c.Put(ctx, "StorageCenter/ScVolumeConfiguration/"+volID, map[string]any{"ReplayProfileList": ids}, nil)
}

// ExpandVolume is PUT /volumes/{id} with a larger size.
func (s *Service) ExpandVolume(ctx context.Context, c *sc.Client, id string, size int64) (Volume, error) {
	v, err := s.getVolume(ctx, c, id)
	if err != nil {
		return Volume{}, err
	}
	if size > 0 {
		g := ceilGiB(size)
		if g*gib > sc.Bytes(v.ConfiguredSize) {
			c.Mutate.Lock()
			err := c.Post(ctx, "StorageCenter/ScVolume/"+id+"/ExpandToSize", map[string]any{"NewSize": fmt.Sprintf("%d GB", g)}, nil)
			c.Mutate.Unlock()
			if err != nil {
				return Volume{}, err
			}
			s.Log.Info("expanded volume", "id", id, "gb", g)
		}
	}
	return s.GetVolume(ctx, c, id)
}

// DeleteVolume is DELETE /volumes/{id}.
func (s *Service) DeleteVolume(ctx context.Context, c *sc.Client, id string) error {
	v, err := s.getVolume(ctx, c, id)
	if err != nil {
		return err
	}
	if v.Mapped {
		return errorf(http.StatusConflict, "volume %s is still published", id)
	}
	c.Mutate.Lock()
	defer c.Mutate.Unlock()
	if replays, err := s.userReplays(ctx, c, v, true); err == nil {
		for _, r := range replays {
			if isHelper(r.Description) {
				_ = c.Post(ctx, "StorageCenter/ScReplay/"+r.InstanceID+"/Expire", map[string]any{}, nil)
			}
		}
	}
	if err := c.Delete(ctx, "StorageCenter/ScVolume/"+id); err != nil {
		return errorf(http.StatusConflict, "volume %s cannot be deleted (clones or snapshots in use?): %v", id, err)
	}
	// a first DELETE may only move the volume to the recycle bin; purge it
	var again sc.Volume
	if err := c.Get(ctx, "StorageCenter/ScVolume/"+id, &again); err == nil && again.InRecycleBin {
		_ = c.Delete(ctx, "StorageCenter/ScVolume/"+id)
	}
	s.Log.Info("deleted volume", "id", id, "name", v.Name)
	return nil
}

// ---------------------------------------------------------------------------------------------- snapshots

func isHelper(desc string) bool { return strings.HasPrefix(desc, helperPrefix) }

func descMatches(desc, name string) bool {
	return desc != "" && (desc == name || (len(desc) >= descMax && strings.HasPrefix(name, desc)))
}

func epoch(ts string) int64 {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Unix()
	}
	return time.Now().Unix()
}

func toSnapshot(r *sc.Replay, v *sc.Volume, name string) Snapshot {
	if name == "" {
		name = r.Description
	}
	return Snapshot{
		ID: r.InstanceID, Name: name, Description: r.Description, VolumeID: v.InstanceID, VolumeName: v.Name,
		Size: sc.Bytes(v.ConfiguredSize), CreationTime: epoch(r.FreezeTime), ReadyToUse: true, Config: map[string]any{},
	}
}

// userReplays lists frozen replays created on this volume with a description. It skips the active replay,
// replays a view volume inherits from its parent and, unless helpers is set, the driver's clone helpers.
func (s *Service) userReplays(ctx context.Context, c *sc.Client, v *sc.Volume, helpers bool) ([]sc.Replay, error) {
	var all []sc.Replay
	if err := c.Get(ctx, "StorageCenter/ScVolume/"+v.InstanceID+"/ReplayList", &all); err != nil {
		return nil, err
	}
	out := []sc.Replay{}
	for _, r := range all {
		if r.Active || r.MarkedForExpiration || r.Description == "" {
			continue
		}
		if owner := r.CreateVolume.ID(); owner != "" && owner != v.InstanceID {
			continue
		}
		if isHelper(r.Description) && !helpers {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Service) getSnapshot(ctx context.Context, c *sc.Client, id string) (*sc.Replay, *sc.Volume, error) {
	var r sc.Replay
	if err := c.Get(ctx, "StorageCenter/ScReplay/"+id, &r); err != nil || r.InstanceID == "" || r.MarkedForExpiration {
		if err != nil && !sc.IsStatus(err, 400) && !sc.IsStatus(err, 404) {
			return nil, nil, err
		}
		return nil, nil, errorf(http.StatusNotFound, "snapshot %s not found", id)
	}
	parent := r.CreateVolume.ID()
	if parent == "" { // replay ids are <ssn>.<volume index>.<replay index>
		if i := strings.LastIndex(id, "."); i > 0 {
			parent = id[:i]
		}
	}
	v, err := s.getVolume(ctx, c, parent)
	if err != nil {
		return nil, nil, err
	}
	return &r, v, nil
}

// ListSnapshots is GET /snapshots[?volume_id=&name=].
func (s *Service) ListSnapshots(ctx context.Context, c *sc.Client, volumeID, name string) ([]Snapshot, error) {
	var vols []sc.Volume
	if volumeID != "" {
		v, err := s.getVolume(ctx, c, volumeID)
		if err != nil {
			return nil, err
		}
		vols = []sc.Volume{*v}
	} else {
		var err error
		if vols, err = s.ownedVolumes(ctx, c, nil); err != nil {
			return nil, err
		}
	}
	out := []Snapshot{}
	for i := range vols {
		replays, err := s.userReplays(ctx, c, &vols[i], name != "")
		if err != nil {
			return nil, err
		}
		for j := range replays {
			if name == "" {
				out = append(out, toSnapshot(&replays[j], &vols[i], ""))
			} else if descMatches(replays[j].Description, name) {
				out = append(out, toSnapshot(&replays[j], &vols[i], name))
			}
		}
	}
	if name != "" && len(out) == 0 {
		return nil, errorf(http.StatusNotFound, "snapshot %s not found", name)
	}
	return out, nil
}

// GetSnapshot is GET /snapshots/{id}.
func (s *Service) GetSnapshot(ctx context.Context, c *sc.Client, id string) (Snapshot, error) {
	r, v, err := s.getSnapshot(ctx, c, id)
	if err != nil {
		return Snapshot{}, err
	}
	return toSnapshot(r, v, ""), nil
}

// CreateSnapshot is POST /snapshots.
func (s *Service) CreateSnapshot(ctx context.Context, c *sc.Client, req SnapshotRequest) (Snapshot, error) {
	if req.Name == "" || req.VolumeID == "" {
		return Snapshot{}, errorf(http.StatusBadRequest, "name and volume_id are required")
	}
	v, err := s.getVolume(ctx, c, req.VolumeID)
	if err != nil {
		return Snapshot{}, err
	}
	c.Mutate.Lock()
	defer c.Mutate.Unlock()
	replays, err := s.userReplays(ctx, c, v, true)
	if err != nil {
		return Snapshot{}, err
	}
	for i := range replays {
		if descMatches(replays[i].Description, req.Name) {
			return toSnapshot(&replays[i], v, req.Name), nil
		}
	}
	if !v.Active {
		return Snapshot{}, errorf(http.StatusBadRequest, "volume %s has never been written; Storage Center cannot snapshot it", v.InstanceID)
	}
	var r sc.Replay
	if err := c.Post(ctx, "StorageCenter/ScVolume/"+v.InstanceID+"/CreateReplay",
		map[string]any{"description": req.Name, "expireTime": 0}, &r); err != nil {
		return Snapshot{}, err
	}
	s.Log.Info("created snapshot", "name", req.Name, "id", r.InstanceID, "volume", v.Name)
	return toSnapshot(&r, v, req.Name), nil
}

// DeleteSnapshot is DELETE /snapshots/{id}.
func (s *Service) DeleteSnapshot(ctx context.Context, c *sc.Client, id string) error {
	r, v, err := s.getSnapshot(ctx, c, id)
	if err != nil {
		return err
	}
	c.Mutate.Lock()
	defer c.Mutate.Unlock()
	if err := c.Post(ctx, "StorageCenter/ScReplay/"+id+"/Expire", map[string]any{}, nil); err != nil {
		return errorf(http.StatusConflict, "snapshot %s cannot be expired: %v", id, err)
	}
	s.Log.Info("expired snapshot", "id", id, "description", r.Description, "volume", v.Name)
	return nil
}

func (s *Service) cloneVolume(ctx context.Context, c *sc.Client, req CreateVolumeRequest) (Volume, error) {
	c.Mutate.Lock()
	defer c.Mutate.Unlock()
	if existing, err := s.ownedVolumes(ctx, c, map[string]any{"Name": req.Name}); err != nil {
		return Volume{}, err
	} else if len(existing) > 0 {
		return s.toVolume(ctx, c, &existing[0]), nil
	}
	r, src, err := s.getSnapshot(ctx, c, req.BaseSnapshotID)
	if err != nil {
		return Volume{}, err
	}
	folder, err := c.FolderID(ctx, "ScVolumeFolder", s.Cfg.VolumeFolder, noteManaged)
	if err != nil {
		return Volume{}, err
	}
	var v sc.Volume
	if err := c.Post(ctx, "StorageCenter/ScReplay/"+r.InstanceID+"/CreateView", map[string]any{
		"Name": req.Name, "VolumeFolder": folder,
		"Notes": fmt.Sprintf("%s; clone of %s snapshot %s", noteManaged, src.Name, r.Description),
	}, &v); err != nil {
		return Volume{}, err
	}
	if err := s.setSnapshotProfile(ctx, c, v.InstanceID, ""); err != nil {
		return Volume{}, err
	}
	if req.Size > sc.Bytes(v.ConfiguredSize) {
		if err := c.Post(ctx, "StorageCenter/ScVolume/"+v.InstanceID+"/ExpandToSize",
			map[string]any{"NewSize": fmt.Sprintf("%d GB", ceilGiB(req.Size))}, nil); err != nil {
			return Volume{}, err
		}
	}
	s.Log.Info("created clone", "name", req.Name, "id", v.InstanceID, "snapshot", r.InstanceID, "source", src.Name)
	if isHelper(r.Description) {
		// the clone keeps the pages it needs; drop the helper so the source stays deletable
		if err := c.Post(ctx, "StorageCenter/ScReplay/"+r.InstanceID+"/Expire", map[string]any{}, nil); err != nil {
			s.Log.Warn("could not expire clone helper snapshot", "id", r.InstanceID, "err", err)
		}
	}
	if err := c.Get(ctx, "StorageCenter/ScVolume/"+v.InstanceID, &v); err != nil {
		return Volume{}, err
	}
	return s.toVolume(ctx, c, &v), nil
}

// ---------------------------------------------------------------------------------------------- hosts

func normWWPN(w string) string {
	return strings.ToLower(strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(w), ":", ""), "0x"))
}

// AssertHost is POST /hosts: find or create the node's SC server by WWPN. WWPNs that belong to a server
// outside the CSP's folder (e.g. pinned in a legacy ESXi object) are skipped, never touched.
func (s *Service) AssertHost(ctx context.Context, c *sc.Client, req HostRequest) (Host, error) {
	name := req.Name
	if name == "" {
		name = req.UUID
	}
	var wwpns []string
	for _, w := range req.WWPNs {
		if w = normWWPN(w); w != "" {
			wwpns = append(wwpns, w)
		}
	}
	if req.UUID == "" || len(wwpns) == 0 {
		return Host{}, errorf(http.StatusBadRequest, "uuid and wwpns are required (Fibre Channel only)")
	}
	c.Mutate.Lock()
	defer c.Mutate.Unlock()

	owners, foreign := map[string]string{}, map[string]string{}
	for _, w := range wwpns {
		var hbas []sc.ServerHBA
		if err := c.GetList(ctx, "ScServerHba", map[string]any{"instanceName": strings.ToUpper(w)}, &hbas); err != nil {
			return Host{}, err
		}
		if len(hbas) == 0 || hbas[0].Server.ID() == "" {
			continue
		}
		var owner sc.Server
		if err := c.Get(ctx, "StorageCenter/ScServer/"+hbas[0].Server.ID(), &owner); err != nil {
			return Host{}, err
		}
		if inFolder(owner.ServerFolderPath, s.Cfg.ServerFolder) {
			owners[w] = owner.InstanceID
		} else {
			foreign[w] = owner.Name
		}
	}
	usable := wwpns[:0:0]
	for _, w := range wwpns {
		if _, bad := foreign[w]; !bad {
			usable = append(usable, w)
		}
	}
	if len(foreign) > 0 {
		s.Log.Warn("WWPNs belong to SC servers outside the CSP folder; using the rest",
			"node", name, "foreign", foreign, "using", usable, "folder", s.Cfg.ServerFolder)
	}
	if len(usable) == 0 {
		return Host{}, errorf(http.StatusConflict, "all WWPNs of %s belong to SC servers outside folder %q: %v", name, s.Cfg.ServerFolder, foreign)
	}
	ids := map[string]bool{}
	for _, id := range owners {
		ids[id] = true
	}
	if len(ids) > 1 {
		return Host{}, errorf(http.StatusConflict, "WWPNs of %s belong to several SC servers: %v", name, owners)
	}
	var srv sc.Server
	if len(ids) == 1 {
		for id := range ids {
			if err := c.Get(ctx, "StorageCenter/ScServer/"+id, &srv); err != nil {
				return Host{}, err
			}
		}
		if NodeUUIDFromNotes(srv.Notes) != req.UUID {
			notes := strings.TrimSpace(uuidTag.ReplaceAllString(srv.Notes, ""))
			if err := c.Put(ctx, "StorageCenter/ScPhysicalServer/"+srv.InstanceID,
				map[string]any{"Notes": strings.TrimSpace(notes + " " + noteUUID + req.UUID)}, nil); err != nil {
				return Host{}, err
			}
		}
	} else {
		osID, err := c.NamedID(ctx, "ScServerOperatingSystem", s.Cfg.ServerOS)
		if err != nil {
			return Host{}, err
		}
		folder, err := c.FolderID(ctx, "ScServerFolder", s.Cfg.ServerFolder, noteManaged)
		if err != nil {
			return Host{}, err
		}
		if err := c.Post(ctx, "StorageCenter/ScPhysicalServer", map[string]any{
			"Name": name, "StorageCenter": c.SSN(), "AlertOnConnectivity": false,
			"Notes": noteManaged + " " + noteUUID + req.UUID, "OperatingSystem": osID, "ServerFolder": folder,
		}, &srv); err != nil {
			return Host{}, err
		}
		s.Log.Info("created SC server", "node", name, "id", srv.InstanceID)
	}
	for _, w := range usable {
		if _, ok := owners[w]; ok {
			continue
		}
		if err := c.Post(ctx, "StorageCenter/ScPhysicalServer/"+srv.InstanceID+"/AddHba",
			map[string]any{"HbaPortType": "FibreChannel", "WwnOrIscsiName": strings.ToUpper(w), "AllowManual": true}, nil); err != nil {
			return Host{}, err
		}
		s.Log.Info("added HBA", "wwpn", w, "server", srv.Name)
	}
	return Host{ID: srv.InstanceID, Name: name, UUID: req.UUID, WWPNs: usable, IQNs: []string{}, Networks: nonNil(req.Networks)}, nil
}

// GetHost is GET /hosts/{uuid}.
func (s *Service) GetHost(ctx context.Context, c *sc.Client, uuid string) (Host, error) {
	srv, err := s.findServerByUUID(ctx, c, uuid)
	if err != nil {
		return Host{}, err
	}
	if srv == nil {
		return Host{}, errorf(http.StatusNotFound, "host %s not found", uuid)
	}
	return Host{ID: srv.InstanceID, Name: srv.Name, UUID: uuid, WWPNs: []string{}, IQNs: []string{}, Networks: []string{}}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---------------------------------------------------------------------------------------------- publish

func (s *Service) mappingsFor(ctx context.Context, c *sc.Client, volID, serverID string) ([]sc.Mapping, error) {
	var all []sc.Mapping
	if err := c.Get(ctx, "StorageCenter/ScVolume/"+volID+"/MappingList", &all); err != nil {
		return nil, err
	}
	out := []sc.Mapping{}
	for _, m := range all {
		if m.Server.ID() == serverID {
			out = append(out, m)
		}
	}
	return out, nil
}

// Publish maps a volume to the host's SC server and returns what the node needs to find it.
func (s *Service) Publish(ctx context.Context, c *sc.Client, volID string, req PublishRequest) (PublishInfo, error) {
	if p := strings.ToLower(req.AccessProtocol); p != "" && p != "fc" {
		return PublishInfo{}, errorf(http.StatusBadRequest, "only access_protocol=fc is supported")
	}
	v, err := s.getVolume(ctx, c, volID)
	if err != nil {
		return PublishInfo{}, err
	}
	srv, err := s.findServerByUUID(ctx, c, req.HostUUID)
	if err != nil {
		return PublishInfo{}, err
	}
	if srv == nil {
		return PublishInfo{}, errorf(http.StatusNotFound, "no SC server for host %s; POST /hosts first", req.HostUUID)
	}
	c.Mutate.Lock()
	maps, err := s.mappingsFor(ctx, c, volID, srv.InstanceID)
	if err == nil && len(maps) == 0 {
		err = c.Post(ctx, "StorageCenter/ScVolume/"+volID+"/MapToServer",
			map[string]any{"server": srv.InstanceID, "Advanced": map[string]any{"MapToDownServerHbas": true}}, nil)
		for i := 0; err == nil && i < 10; i++ {
			if maps, err = s.mappingsFor(ctx, c, volID, srv.InstanceID); err != nil || len(maps) > 0 {
				break
			}
			time.Sleep(time.Second)
		}
		if err == nil {
			s.Log.Info("mapped volume", "id", volID, "name", v.Name, "server", srv.Name)
		}
	}
	c.Mutate.Unlock()
	if err != nil {
		return PublishInfo{}, err
	}
	if len(maps) == 0 {
		return PublishInfo{}, errorf(http.StatusServiceUnavailable, "mapping of %s to %s not visible yet", volID, srv.Name)
	}
	luns := map[int]bool{}
	targets := map[string]bool{}
	for _, m := range maps {
		luns[m.Lun] = true
		if id := m.ControllerPort.ID(); id != "" {
			w, err := c.PortWWN(ctx, id)
			if err != nil {
				return PublishInfo{}, err
			}
			targets[w] = true
		}
	}
	if len(luns) != 1 {
		return PublishInfo{}, errorf(http.StatusInternalServerError, "inconsistent LUNs %v for %s on %s", luns, volID, srv.Name)
	}
	info := PublishInfo{SerialNumber: strings.ToLower(v.DeviceID), AccessProtocol: "fc"}
	for l := range luns {
		info.LunID = l
	}
	for t := range targets {
		info.TargetNames = append(info.TargetNames, t)
	}
	sort.Strings(info.TargetNames)
	return info, nil
}

// Unpublish removes only this host's mapping profile; other hosts of an RWX volume keep theirs.
func (s *Service) Unpublish(ctx context.Context, c *sc.Client, volID string, req PublishRequest) error {
	if _, err := s.getVolume(ctx, c, volID); err != nil {
		var e *Error
		if errors.As(err, &e) && e.Status == http.StatusNotFound {
			return nil // already gone
		}
		return err
	}
	srv, err := s.findServerByUUID(ctx, c, req.HostUUID)
	if err != nil || srv == nil {
		return err
	}
	c.Mutate.Lock()
	defer c.Mutate.Unlock()
	var profiles []sc.MappingProfile
	if err := c.Get(ctx, "StorageCenter/ScVolume/"+volID+"/MappingProfileList", &profiles); err != nil {
		return err
	}
	for _, mp := range profiles {
		if mp.Server.ID() == srv.InstanceID {
			if err := c.Delete(ctx, "StorageCenter/ScMappingProfile/"+mp.InstanceID); err != nil {
				return err
			}
			s.Log.Info("unmapped volume", "id", volID, "server", srv.Name)
		}
	}
	return nil
}
