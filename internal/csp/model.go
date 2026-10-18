// Package csp implements the HPE CSI Driver Container Storage Provider operations on Dell SC Series.
package csp

import (
	"fmt"
	"net/http"
)

// Volume is the CSP volume object (HPE CSP OpenAPI "Volume").
type Volume struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Size           int64          `json:"size"`
	Description    string         `json:"description"`
	Published      bool           `json:"published"`
	BaseSnapshotID string         `json:"base_snapshot_id"`
	VolumeGroupID  string         `json:"volume_group_id"`
	SerialNumber   string         `json:"serial_number"`
	Config         map[string]any `json:"config"`
}

// Snapshot is the CSP snapshot object.
type Snapshot struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	VolumeID     string         `json:"volume_id"`
	VolumeName   string         `json:"volume_name"`
	Size         int64          `json:"size"`
	CreationTime int64          `json:"creation_time"`
	ReadyToUse   bool           `json:"ready_to_use"`
	Config       map[string]any `json:"config"`
}

// Host is the CSP host (Kubernetes node) object.
type Host struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	UUID     string   `json:"uuid"`
	WWPNs    []string `json:"wwpns"`
	IQNs     []string `json:"iqns"`
	Networks []string `json:"networks"`
}

// PublishInfo is what the node needs to find the device.
type PublishInfo struct {
	SerialNumber   string   `json:"serial_number"`
	AccessProtocol string   `json:"access_protocol"`
	LunID          int      `json:"lun_id"`
	TargetNames    []string `json:"target_names"`
}

// CreateVolumeRequest is POST /volumes (new volume or clone).
type CreateVolumeRequest struct {
	Name           string         `json:"name"`
	Size           int64          `json:"size"`
	Description    string         `json:"description"`
	BaseSnapshotID string         `json:"base_snapshot_id"`
	Clone          bool           `json:"clone"`
	Config         map[string]any `json:"config"`
}

// HostRequest is POST /hosts.
type HostRequest struct {
	Name     string   `json:"name"`
	UUID     string   `json:"uuid"`
	WWPNs    []string `json:"wwpns"`
	IQNs     []string `json:"iqns"`
	Networks []string `json:"networks"`
}

// PublishRequest is PUT /volumes/{id}/actions/publish|unpublish.
type PublishRequest struct {
	HostUUID       string `json:"host_uuid"`
	AccessProtocol string `json:"access_protocol"`
}

// SnapshotRequest is POST /snapshots.
type SnapshotRequest struct {
	Name        string         `json:"name"`
	VolumeID    string         `json:"volume_id"`
	Description string         `json:"description"`
	Config      map[string]any `json:"config"`
}

// Error is returned to the HPE driver as {"errors":[{"code","message"}]} with Status.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// Code is the textual code in the CSP error body.
func (e *Error) Code() string { return http.StatusText(e.Status) }

func errorf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, args...)}
}
