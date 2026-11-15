package storage

import (
	"sync"
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

type poolActivityKey struct {
	pool     string
	activity string
}

var (
	poolActivityPausesMu sync.Mutex
	poolActivityPauses   = map[poolActivityKey]PoolActivityPause{}
)

// SetPoolActivityPause records the pause state of a pool activity ("scrub" or "trim").
func SetPoolActivityPause(pool string, activity string, pause PoolActivityPause) {
	poolActivityPausesMu.Lock()
	defer poolActivityPausesMu.Unlock()

	poolActivityPauses[poolActivityKey{pool, activity}] = pause
}

// applyPoolActivityPauses copies the recorded pause states into the pool's scrub and trim status.
func applyPoolActivityPauses(pool *api.SystemStoragePool) {
	poolActivityPausesMu.Lock()
	defer poolActivityPausesMu.Unlock()

	scrub, ok := poolActivityPauses[poolActivityKey{pool.Name, "scrub"}]
	if ok && pool.LastScrub != nil {
		pool.LastScrub.Paused = scrub.Paused
		pool.LastScrub.PausedSince = scrub.Since
		pool.LastScrub.PauseReason = scrub.Reason
		pool.LastScrub.Pauses = scrub.Pauses
	}

	trim, ok := poolActivityPauses[poolActivityKey{pool.Name, "trim"}]
	if ok && pool.LastTrim != nil {
		pool.LastTrim.Paused = trim.Paused
		pool.LastTrim.PausedSince = trim.Since
		pool.LastTrim.PauseReason = trim.Reason
		pool.LastTrim.Pauses = trim.Pauses
	}
}
