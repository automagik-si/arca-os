package api

import "time"

// SystemStorageImport lists the drives opened read-only for import and the copy jobs.
//
// swagger:model
type SystemStorageImport struct {
	Disks  []SystemStorageImportDisk `json:"disks"  yaml:"disks"`
	Copies []SystemStorageImportCopy `json:"copies" yaml:"copies"`
}

// SystemStorageImportDisk is a drive opened read-only for import.
type SystemStorageImportDisk struct {
	Serial      string                          `json:"serial"               yaml:"serial"`
	ID          string                          `json:"id"                   yaml:"id"`
	Layers      []string                        `json:"layers"               yaml:"layers"`
	Filesystems []SystemStorageImportFilesystem `json:"filesystems"          yaml:"filesystems"`
	Unreadable  []string                        `json:"unreadable,omitempty" yaml:"unreadable,omitempty"`
}

// SystemStorageImportFilesystem is one file system (or zfs pool) found on an import drive.
type SystemStorageImportFilesystem struct {
	Name      string                        `json:"name"      yaml:"name"`
	Type      string                        `json:"type"      yaml:"type"`
	Instances []SystemStorageImportInstance `json:"instances" yaml:"instances"`
	Volumes   []SystemStorageImportVolume   `json:"volumes"   yaml:"volumes"`
	Folders   []string                      `json:"folders"   yaml:"folders"`
}

// SystemStorageImportInstance is an Incus instance found on an import drive.
type SystemStorageImportInstance struct {
	Name      string   `json:"name"                yaml:"name"`
	Project   string   `json:"project"             yaml:"project"`
	Type      string   `json:"type"                yaml:"type"`
	Source    string   `json:"source"              yaml:"source"`
	Autostart bool     `json:"autostart"           yaml:"autostart"`
	Snapshots []string `json:"snapshots,omitempty" yaml:"snapshots,omitempty"`
}

// SystemStorageImportVolume is an Incus custom volume found on an import drive.
type SystemStorageImportVolume struct {
	Name    string `json:"name"    yaml:"name"`
	Project string `json:"project" yaml:"project"`
	Source  string `json:"source"  yaml:"source"`
}

// SystemStorageImportOpen asks to open a drive read-only for import.
//
// swagger:model
type SystemStorageImportOpen struct {
	Serial string `json:"serial" yaml:"serial"`
	// Passphrase of a LUKS layer or an encrypted zfs root; used once, never stored or logged.
	Passphrase string `json:"passphrase,omitempty" yaml:"passphrase,omitempty"` //nolint:gosec
}

// SystemStorageImportClose asks to close an import drive.
//
// swagger:model
type SystemStorageImportClose struct {
	Serial string `json:"serial" yaml:"serial"`
}

// SystemStorageImportCopyRequest asks to copy one item of an import drive into a native pool.
//
// swagger:model
type SystemStorageImportCopyRequest struct {
	Serial     string `json:"serial"     yaml:"serial"`
	Filesystem string `json:"filesystem" yaml:"filesystem"`
	// Kind is one of "instance", "volume" or "folder".
	Kind          string   `json:"kind"                     yaml:"kind"`
	Source        string   `json:"source"                   yaml:"source"`
	TargetPool    string   `json:"target_pool"              yaml:"target_pool"`
	TargetName    string   `json:"target_name,omitempty"    yaml:"target_name,omitempty"`
	TargetProject string   `json:"target_project,omitempty" yaml:"target_project,omitempty"`
	Profiles      []string `json:"profiles,omitempty"       yaml:"profiles,omitempty"`
}

// SystemStorageImportCopy is a copy job and its progress.
type SystemStorageImportCopy struct {
	ID         int                            `json:"id"                 yaml:"id"`
	Request    SystemStorageImportCopyRequest `json:"request"            yaml:"request"`
	State      string                         `json:"state"              yaml:"state"`
	BytesDone  int64                          `json:"bytes_done"         yaml:"bytes_done"`
	BytesTotal int64                          `json:"bytes_total"        yaml:"bytes_total"`
	Error      string                         `json:"error,omitempty"    yaml:"error,omitempty"`
	Started    time.Time                      `json:"started"            yaml:"started"`
	Finished   *time.Time                     `json:"finished,omitempty" yaml:"finished,omitempty"`
}

// SystemStorageImportConvert asks to wipe an import drive and make it a native pool.
//
// swagger:model
type SystemStorageImportConvert struct {
	Serial string `json:"serial" yaml:"serial"`
	Pool   string `json:"pool"   yaml:"pool"`
	// Confirm must repeat the serial.
	Confirm string `json:"confirm" yaml:"confirm"`
}
