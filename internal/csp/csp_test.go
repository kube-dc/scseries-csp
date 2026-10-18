package csp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/kube-dc/scseries-csp/internal/sc"
	"github.com/kube-dc/scseries-csp/internal/sc/scfake"
)

func setup(t *testing.T) (*scfake.Array, *sc.Client, *Service, context.Context) {
	t.Helper()
	a := scfake.New()
	t.Cleanup(a.Close)
	host, port := a.HostPort()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := sc.New(ctx, sc.Config{Host: host, Port: port, User: a.User, Password: a.Password, InsecureSkipVerify: true}, log, nil)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return a, c, &Service{Cfg: Defaults(), Log: log}, ctx
}

func status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func TestLoginNegotiatesAPIVersion(t *testing.T) {
	a, c, _, _ := setup(t)
	if c.SSN() != scfake.SSN {
		t.Fatalf("ssn = %q", c.SSN())
	}
	if !strings.Contains(strings.Join(a.Calls, ","), "POST ApiConnection/Login,POST ApiConnection/Login") {
		t.Fatalf("expected a retried login after version negotiation, calls: %v", a.Calls)
	}
}

func TestCreateVolumeIdempotentAndProfiles(t *testing.T) {
	a, c, s, ctx := setup(t)
	req := CreateVolumeRequest{Name: "pvc-1", Size: 1<<30 + 1, Config: map[string]any{"storage_profile": "High Priority (Tier 1)", "data_reduction_profile": "None"}}
	v1, err := s.CreateVolume(ctx, c, req)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Size != 2<<30 {
		t.Errorf("size rounds up to whole GiB: got %d", v1.Size)
	}
	if v1.Config["storage_profile"] != "High Priority (Tier 1)" || v1.Config["data_reduction_profile"] != "None" {
		t.Errorf("profiles not applied: %v", v1.Config)
	}
	if v1.SerialNumber == "" {
		t.Error("serial_number (deviceId) missing")
	}
	v2, err := s.CreateVolume(ctx, c, req)
	if err != nil || v2.ID != v1.ID {
		t.Fatalf("second create should return the same volume: %v %v", v2.ID, err)
	}
	if got := a.Volume(v1.ID)["volumeFolderPath"]; got != "scseries-csp/" {
		t.Errorf("volume folder = %v", got)
	}
	if _, err := s.CreateVolume(ctx, c, CreateVolumeRequest{Name: "pvc-x", Size: 1 << 30, Config: map[string]any{"storageProfile": "nope"}}); status(err) != 400 {
		t.Errorf("unknown storage profile should be 400, got %v", err)
	}
}

func TestOwnershipGuard(t *testing.T) {
	a, c, s, ctx := setup(t)
	foreign := a.AddVolume("datastore-1", "", 1000)
	if _, err := s.GetVolume(ctx, c, foreign); status(err) != 404 {
		t.Errorf("volume outside folder must be invisible, got %v", err)
	}
	if err := s.DeleteVolume(ctx, c, foreign); status(err) != 404 {
		t.Errorf("delete outside folder must be 404, got %v", err)
	}
	if a.Volume(foreign) == nil {
		t.Fatal("foreign volume was deleted")
	}
	if vols, _ := s.ListVolumes(ctx, c, ""); len(vols) != 0 {
		t.Errorf("list must not show foreign volumes: %v", vols)
	}
}

func TestAssertHostPartialAndForeign(t *testing.T) {
	a, c, s, ctx := setup(t)
	a.AddServer("legacy-esxi-01", "", "21000024ff000002") // legacy ESXi object pins one port
	h, err := s.AssertHost(ctx, c, HostRequest{Name: "node-a", UUID: "u1", WWPNs: []string{"21000024ff000001", "0x21000024FF000002"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.WWPNs) != 1 || h.WWPNs[0] != "21000024ff000001" {
		t.Errorf("only the free port should be used: %v", h.WWPNs)
	}
	srv := a.Servers()["node-a"]
	if srv == nil || !strings.Contains(srv["notes"].(string), "csp-node-uuid=u1") || srv["serverFolderPath"] != "scseries-csp/" {
		t.Fatalf("server not created as expected: %v", srv)
	}
	// re-assert with a new uuid (reinstalled node): same server, uuid replaced
	h2, err := s.AssertHost(ctx, c, HostRequest{Name: "node-a", UUID: "u2", WWPNs: []string{"21000024ff000001"}})
	if err != nil || h2.ID != h.ID {
		t.Fatalf("expected same server, got %v %v", h2.ID, err)
	}
	if n := a.Servers()["node-a"]["notes"].(string); strings.Contains(n, "u1") || !strings.Contains(n, "csp-node-uuid=u2") {
		t.Errorf("uuid not replaced: %q", n)
	}
	a.AddServer("legacy-esxi-03", "", "21000024ff000031", "21000024ff000032")
	if _, err := s.AssertHost(ctx, c, HostRequest{Name: "node-c", UUID: "u3", WWPNs: []string{"21000024ff000031", "21000024ff000032"}}); status(err) != 409 {
		t.Errorf("all ports pinned must be 409, got %v", err)
	}
}

func TestPublishRWXAndSelectiveUnpublish(t *testing.T) {
	a, c, s, ctx := setup(t)
	v, _ := s.CreateVolume(ctx, c, CreateVolumeRequest{Name: "pvc-rwx", Size: 2 << 30})
	for _, n := range []struct{ name, uuid, wwpn string }{{"node-a", "ua", "aa01"}, {"node-b", "ub", "bb01"}} {
		if _, err := s.AssertHost(ctx, c, HostRequest{Name: n.name, UUID: n.uuid, WWPNs: []string{n.wwpn}}); err != nil {
			t.Fatal(err)
		}
	}
	pa, err := s.Publish(ctx, c, v.ID, PublishRequest{HostUUID: "ua", AccessProtocol: "fc"})
	if err != nil {
		t.Fatal(err)
	}
	if pa.SerialNumber != v.SerialNumber || pa.LunID != 1 || len(pa.TargetNames) == 0 || pa.TargetNames[0] != strings.ToLower(pa.TargetNames[0]) {
		t.Errorf("bad publish info: %+v", pa)
	}
	again, _ := s.Publish(ctx, c, v.ID, PublishRequest{HostUUID: "ua"})
	if again.LunID != pa.LunID || len(a.Mappings(v.ID)) != 1 {
		t.Errorf("publish must be idempotent: %+v %v", again, a.Mappings(v.ID))
	}
	if _, err := s.Publish(ctx, c, v.ID, PublishRequest{HostUUID: "ub"}); err != nil {
		t.Fatal(err)
	}
	if got := a.Mappings(v.ID); len(got) != 2 {
		t.Fatalf("RWX should map both nodes: %v", got)
	}
	if err := s.DeleteVolume(ctx, c, v.ID); status(err) != 409 {
		t.Errorf("delete while published must be 409, got %v", err)
	}
	if err := s.Unpublish(ctx, c, v.ID, PublishRequest{HostUUID: "ua"}); err != nil {
		t.Fatal(err)
	}
	if got := a.Mappings(v.ID); len(got) != 1 || got[0] != "node-b" {
		t.Errorf("unpublish must keep the other node's mapping: %v", got)
	}
	if _, err := s.Publish(ctx, c, v.ID, PublishRequest{HostUUID: "unknown"}); status(err) != 404 {
		t.Errorf("unknown host must be 404, got %v", err)
	}
	if _, err := s.Publish(ctx, c, v.ID, PublishRequest{HostUUID: "ub", AccessProtocol: "iscsi"}); status(err) != 400 {
		t.Errorf("iscsi must be rejected, got %v", err)
	}
}

func TestDeletePurgesRecycleBinAndIsIdempotent(t *testing.T) {
	a, c, s, ctx := setup(t)
	v, _ := s.CreateVolume(ctx, c, CreateVolumeRequest{Name: "pvc-del", Size: 1 << 30})
	if err := s.DeleteVolume(ctx, c, v.ID); err != nil {
		t.Fatal(err)
	}
	if a.Volume(v.ID) != nil {
		t.Error("volume left in the recycle bin")
	}
	if err := s.DeleteVolume(ctx, c, v.ID); status(err) != 404 {
		t.Errorf("second delete must be 404, got %v", err)
	}
	if err := s.Unpublish(ctx, c, v.ID, PublishRequest{HostUUID: "x"}); err != nil {
		t.Errorf("unpublish of a deleted volume must succeed, got %v", err)
	}
}

func TestExpand(t *testing.T) {
	_, c, s, ctx := setup(t)
	v, _ := s.CreateVolume(ctx, c, CreateVolumeRequest{Name: "pvc-exp", Size: 1 << 30})
	got, err := s.ExpandVolume(ctx, c, v.ID, 3<<30)
	if err != nil || got.Size != 3<<30 {
		t.Fatalf("expand: %v %v", got.Size, err)
	}
	same, _ := s.ExpandVolume(ctx, c, v.ID, 1<<30)
	if same.Size != 3<<30 {
		t.Errorf("shrink must be ignored: %d", same.Size)
	}
}

func TestSnapshotsAndClones(t *testing.T) {
	a, c, s, ctx := setup(t)
	src, _ := s.CreateVolume(ctx, c, CreateVolumeRequest{Name: "pvc-src", Size: 1 << 30})
	long := "snapshot-8e26c65c-8a91-4823-9de6-aba59c3923c7"
	if _, err := s.CreateSnapshot(ctx, c, SnapshotRequest{Name: long, VolumeID: src.ID}); status(err) != 400 {
		t.Errorf("snapshot of a never-written volume must be 400, got %v", err)
	}
	a.SetActive(src.ID)
	snap, err := s.CreateSnapshot(ctx, c, SnapshotRequest{Name: long, VolumeID: src.ID})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Name != long || !snap.ReadyToUse {
		t.Errorf("snapshot reported with full name and ready: %+v", snap)
	}
	again, _ := s.CreateSnapshot(ctx, c, SnapshotRequest{Name: long, VolumeID: src.ID})
	if again.ID != snap.ID {
		t.Errorf("snapshot create must be idempotent despite 31-char truncation: %v vs %v", again.ID, snap.ID)
	}
	found, err := s.ListSnapshots(ctx, c, src.ID, long)
	if err != nil || len(found) != 1 || found[0].ID != snap.ID {
		t.Fatalf("lookup by full name: %v %v", found, err)
	}
	// restore from the user snapshot
	restored, err := s.CreateVolume(ctx, c, CreateVolumeRequest{Name: "pvc-restored", BaseSnapshotID: snap.ID, Clone: true, Size: 2 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Size != 2<<30 {
		t.Errorf("clone not grown to requested size: %d", restored.Size)
	}
	if list, _ := s.ListSnapshots(ctx, c, restored.ID, ""); len(list) != 0 {
		t.Errorf("a view volume must not report its parent's replay as its own: %v", list)
	}
	// PVC->PVC clone: driver creates a helper snapshot, then clones from it
	helper, _ := s.CreateSnapshot(ctx, c, SnapshotRequest{Name: "snap-for-clone-2026-10-17T18:34:27Z", VolumeID: src.ID})
	if list, _ := s.ListSnapshots(ctx, c, src.ID, ""); len(list) != 1 {
		t.Errorf("helper snapshots must be hidden from listings: %v", list)
	}
	if _, err := s.CreateVolume(ctx, c, CreateVolumeRequest{Name: "pvc-clone", BaseSnapshotID: helper.ID, Clone: true}); err != nil {
		t.Fatal(err)
	}
	for _, r := range a.Replays(src.ID) {
		if r == helper.ID {
			t.Error("helper snapshot must be expired after the clone")
		}
	}
	if err := s.DeleteSnapshot(ctx, c, snap.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSnapshot(ctx, c, snap.ID); status(err) != http.StatusNotFound {
		t.Errorf("deleted snapshot must be 404, got %v", err)
	}
}

func TestLegacyNodeTagStillRecognised(t *testing.T) {
	if got := NodeUUIDFromNotes("kube-dc-csp managed kube-dc-node-uuid=abc-1"); got != "abc-1" {
		t.Errorf("legacy tag: got %q", got)
	}
	if got := NodeUUIDFromNotes("scseries-csp managed csp-node-uuid=abc-2"); got != "abc-2" {
		t.Errorf("current tag: got %q", got)
	}
	if got := NodeUUIDFromNotes("hand-made server"); got != "" {
		t.Errorf("no tag: got %q", got)
	}
}

func TestArrayDefaultsAndArrayDefaultSnapshotProfile(t *testing.T) {
	a, c, s, ctx := setup(t)
	v, err := s.CreateVolume(ctx, c, CreateVolumeRequest{Name: "pvc-default", Size: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if v.Config["storage_profile"] != "Recommended (All Tiers)" {
		t.Errorf("without a configured profile the array default must apply, got %v", v.Config)
	}
	keep, err := s.CreateVolume(ctx, c, CreateVolumeRequest{Name: "pvc-keep", Size: 1 << 30, Config: map[string]any{"snapshot_profile": ArrayDefault}})
	if err != nil {
		t.Fatal(err)
	}
	_ = a
	_ = keep
}
