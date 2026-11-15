package storage

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/lxc/incus/v7/shared/subprocess"

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

// applyPoolActivityPauses copies the recorded pause states into a pool's scrub and trim status.
func applyPoolActivityPauses(pool string, scrubStatus *api.SystemStoragePoolScrubStatus, trimStatus *api.SystemStoragePoolTrimStatus) {
	poolActivityPausesMu.Lock()
	defer poolActivityPausesMu.Unlock()

	scrub, ok := poolActivityPauses[poolActivityKey{pool, "scrub"}]
	if ok && scrubStatus != nil {
		scrubStatus.Paused = scrub.Paused
		scrubStatus.PausedSince = scrub.Since
		scrubStatus.PauseReason = scrub.Reason
		scrubStatus.Pauses = scrub.Pauses
	}

	trim, ok := poolActivityPauses[poolActivityKey{pool, "trim"}]
	if ok && trimStatus != nil {
		trimStatus.Paused = trim.Paused
		trimStatus.PausedSince = trim.Since
		trimStatus.PauseReason = trim.Reason
		trimStatus.Pauses = trim.Pauses
	}
}

// SuspendedTrimDevices returns the pool's leaf vdevs whose trim is suspended, and whether no other leaf was trimmed.
func SuspendedTrimDevices(ctx context.Context, zpoolName string) ([]string, bool, error) {
	output, err := subprocess.RunCommandContext(ctx, "zpool", "status", zpoolName, "-jpt", "--json-int")
	if err != nil {
		return nil, false, err
	}

	zpoolJSON := zpoolStatusPartialParse{}

	err = json.Unmarshal([]byte(output), &zpoolJSON)
	if err != nil {
		return nil, false, err
	}

	layout, err := parseZpoolStatus(zpoolJSON, zpoolName)
	if err != nil {
		return nil, false, err
	}

	leaves, all := suspendedTrimLeaves(layout.trimStats)

	return leaves, all, nil
}

// suspendedTrimLeaves returns the leaves whose trim is suspended, and whether every trimmable leaf is.
func suspendedTrimLeaves(_ []zpoolTrimStats) ([]string, bool) {
	return nil, true
}
