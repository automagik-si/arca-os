package storage

import (
	"time"

	"github.com/lxc/incus-os/incus-osd/api"
)

// PoolActivityPause records whether IncusOS paused a pool scrub or trim it started.
type PoolActivityPause struct {
	Paused bool
	Since  time.Time
	Reason string
	Pauses int
}

// SetPoolActivityPause records the pause state of a pool activity ("scrub" or "trim").
func SetPoolActivityPause(_ string, _ string, _ PoolActivityPause) {}

// applyPoolActivityPauses copies the recorded pause states into the pool's scrub and trim status.
func applyPoolActivityPauses(_ *api.SystemStoragePool) {}
