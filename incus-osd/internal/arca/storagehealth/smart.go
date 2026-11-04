// Package storagehealth reads the NVMe health detail that smartctl reports in
// its JSON log and adds it to the daemon's storage document. The daemon already
// runs smartctl as root; this package only parses that output, so nothing here
// opens a device or needs new rights.
package storagehealth

import (
	"encoding/json"

	"github.com/lxc/incus-os/incus-osd/api"
)

// smartctl is the part of smartctl's JSON output this package reads.
//
// The NVMe SMART/Health Information Log (log page 02h) carries the health
// counters; the composite temperature thresholds (WCTEMP and CCTEMP) come from
// the controller's Identify data, which smartctl reports separately. Both are
// pointers so a field the drive does not report stays absent instead of reading
// as zero.
type smartctl struct {
	Log struct {
		MediaErrors             *int `json:"media_errors"`
		AvailableSpareThreshold *int `json:"available_spare_threshold"`
		UnsafeShutdowns         *int `json:"unsafe_shutdowns"`
	} `json:"nvme_smart_health_information_log"`

	Threshold struct {
		Warning  *int `json:"warning"`
		Critical *int `json:"critical"`
	} `json:"nvme_composite_temperature_threshold"`
}

// Apply adds the NVMe health fields smartctl reported to the storage document.
// A drive that reports none of them, such as a SATA drive, leaves every field
// nil; a field the drive reports as zero is set to zero. It never fails on
// malformed input: the fields simply stay nil.
func Apply(status *api.SystemStorageDriveSMART, smartctlJSON []byte) {
	if status == nil || len(smartctlJSON) == 0 {
		return
	}

	var out smartctl
	if json.Unmarshal(smartctlJSON, &out) != nil {
		return
	}

	status.MediaErrors = out.Log.MediaErrors
	status.AvailableSpareThreshold = out.Log.AvailableSpareThreshold
	status.WarningTempC = out.Threshold.Warning
	status.CriticalTempC = out.Threshold.Critical
	status.UnsafeShutdowns = out.Log.UnsafeShutdowns
}
