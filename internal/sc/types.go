package sc

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Ref is the reference form the Storage Center API uses for related objects.
type Ref struct {
	InstanceID   string `json:"instanceId"`
	InstanceName string `json:"instanceName"`
}

// ID returns the referenced instance id, or "" for a nil reference.
func (r *Ref) ID() string {
	if r == nil {
		return ""
	}
	return r.InstanceID
}

type StorageCenter struct {
	InstanceID     string      `json:"instanceId"`
	Name           string      `json:"name"`
	ScSerialNumber json.Number `json:"scSerialNumber"`
	Version        string      `json:"version"`
}

type Volume struct {
	InstanceID       string `json:"instanceId"`
	Name             string `json:"name"`
	DeviceID         string `json:"deviceId"`
	SerialNumber     string `json:"serialNumber"`
	ConfiguredSize   string `json:"configuredSize"`
	VolumeFolderPath string `json:"volumeFolderPath"`
	Mapped           bool   `json:"mapped"`
	Active           bool   `json:"active"`
	InRecycleBin     bool   `json:"inRecycleBin"`
}

type VolumeConfiguration struct {
	StorageProfile       *Ref `json:"storageProfile"`
	DataReductionProfile *Ref `json:"dataReductionProfile"`
}

type Server struct {
	InstanceID       string `json:"instanceId"`
	Name             string `json:"name"`
	Notes            string `json:"notes"`
	ServerFolderPath string `json:"serverFolderPath"`
	Type             string `json:"type"`
	VolumeCount      int    `json:"volumeCount"`
}

type ServerHBA struct {
	InstanceID   string `json:"instanceId"`
	InstanceName string `json:"instanceName"`
	Server       *Ref   `json:"server"`
}

type Mapping struct {
	InstanceID     string `json:"instanceId"`
	Lun            int    `json:"lun"`
	Server         *Ref   `json:"server"`
	ServerHba      *Ref   `json:"serverHba"`
	ControllerPort *Ref   `json:"controllerPort"`
	Profile        *Ref   `json:"profile"`
}

type MappingProfile struct {
	InstanceID string `json:"instanceId"`
	Server     *Ref   `json:"server"`
}

type Replay struct {
	InstanceID          string `json:"instanceId"`
	Description         string `json:"description"`
	FreezeTime          string `json:"freezeTime"`
	Size                string `json:"size"`
	Active              bool   `json:"active"`
	MarkedForExpiration bool   `json:"markedForExpiration"`
	CreateVolume        *Ref   `json:"createVolume"`
}

type ControllerPort struct {
	InstanceID string `json:"instanceId"`
	Name       string `json:"name"`
	WWN        string `json:"wwn"` // encoding/json matches "WWN" too
}

type Named struct {
	InstanceID string `json:"instanceId"`
	Name       string `json:"name"`
}

// Bytes parses SC sizes like "1073741824 Bytes".
func Bytes(s string) int64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return int64(v)
}
