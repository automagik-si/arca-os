package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
)

func TestCalculateScrubProgress(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		stats    zpoolScanStats
		expected string
	}{
		{
			name: "Finished returns 100.00% regardless of values",
			stats: zpoolScanStats{
				State:     "FINISHED",
				Examined:  4268032,
				ToExamine: 4276224,
			},
			expected: "100.00%",
		},
		{
			name: "Scanning returns current progress",
			stats: zpoolScanStats{
				State:     "SCANNING",
				Examined:  4268032,
				ToExamine: 4276224,
			},
			expected: "99.81%",
		},
		{
			name: "Scanning with progress overflow",
			stats: zpoolScanStats{
				State:     "SCANNING",
				Examined:  5268081,
				ToExamine: 4276224,
			},
			expected: "99.99%",
		},
		{
			name: "Scanning with no reported ToExamine",
			stats: zpoolScanStats{
				State:     "SCANNING",
				Examined:  5268081,
				ToExamine: 0,
			},
			expected: "0.00%",
		},
		{
			name: "Scanning with no reported Examined",
			stats: zpoolScanStats{
				State:     "SCANNING",
				Examined:  0,
				ToExamine: 4276224,
			},
			expected: "0.00%",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := calculateScrubProgress(tc.stats)
			require.Equal(t, tc.expected, got, tc.name)
		})
	}
}

func TestCalculateTrimStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		leaves []zpoolTrimStats
		want   *api.SystemStoragePoolTrimStatus
	}{
		{
			name:   "no leaves",
			leaves: []zpoolTrimStats{},
			want:   nil,
		},
		{
			name: "untrimmed and unsupported",
			leaves: []zpoolTrimStats{
				{TrimState: "UNTRIMMED"},
				{TrimState: "COMPLETE", TrimNotSup: 1},
			},
			want: nil,
		},
		{
			name: "all unsupported",
			leaves: []zpoolTrimStats{
				{TrimState: "UNTRIMMED", TrimNotSup: 1},
				{TrimState: "COMPLETE", TrimNotSup: 1},
			},
			want: &api.SystemStoragePoolTrimStatus{
				State: api.SystemStoragePoolTrimUnsupported,
			},
		},
		{
			name: "all complete",
			leaves: []zpoolTrimStats{
				{TrimState: "COMPLETE", Trimmed: 100, ToTrim: 100, TrimTime: 1000},
				{TrimState: "COMPLETE", Trimmed: 50, ToTrim: 50, TrimTime: 2000, TrimErrors: 1},
			},
			want: &api.SystemStoragePoolTrimStatus{
				State:          api.SystemStoragePoolTrimFinished,
				LastActionTime: time.Unix(2000, 0),
				Progress:       "100.00%",
				Errors:         1,
			},
		},
		{
			name: "partially in progress",
			leaves: []zpoolTrimStats{
				{TrimState: "COMPLETE", Trimmed: 100, ToTrim: 100, TrimTime: 3000},
				{TrimState: "ACTIVE", Trimmed: 20, ToTrim: 100, TrimTime: 3100},
				{TrimState: "UNTRIMMED"},
			},
			want: &api.SystemStoragePoolTrimStatus{
				State:          api.SystemStoragePoolTrimInProgress,
				LastActionTime: time.Unix(3100, 0),
				Progress:       "60.00%",
				Errors:         0,
			},
		},
		{
			name: "suspended",
			leaves: []zpoolTrimStats{
				{TrimState: "SUSPENDED", Trimmed: 0, ToTrim: 0, TrimTime: 4000},
			},
			want: &api.SystemStoragePoolTrimStatus{
				State:          api.SystemStoragePoolTrimSuspended,
				LastActionTime: time.Unix(4000, 0),
				Progress:       "0.00%",
				Errors:         0,
			},
		},
		{
			name: "unknown state",
			leaves: []zpoolTrimStats{
				{TrimState: "SOMETHING", Trimmed: 10, ToTrim: 10, TrimTime: 5000},
			},
			want: &api.SystemStoragePoolTrimStatus{
				State:          api.SystemStoragePoolTrimUnknown,
				LastActionTime: time.Unix(5000, 0),
				Progress:       "100.00%",
				Errors:         0,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, calculateTrimStatus(tc.leaves))
		})
	}
}

func TestParseZpoolStatus(t *testing.T) {
	t.Parallel()

	byID := func(names ...string) []string {
		ret := make([]string, 0, len(names))
		for _, name := range names {
			ret = append(ret, "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_incus_"+name)
		}

		return ret
	}

	cases := []struct {
		fixture     string
		pool        string
		poolType    string
		specialType string
		devices     map[string][]string
	}{
		{
			fixture:  "raid0-healthy",
			pool:     "p0",
			poolType: "zfs-raid0",
			devices:  map[string][]string{"devices": byID("d1", "d2", "d3")},
		},
		{
			fixture:  "raid0-replacing",
			pool:     "p0",
			poolType: "zfs-raid0",
			devices:  map[string][]string{"devices": byID("d1", "d2", "d3", "d4")},
		},
		{
			fixture:  "raidz1-replacing-faulted",
			pool:     "p1",
			poolType: "zfs-raidz1",
			devices:  map[string][]string{"devices": byID("d1", "d3", "d4", "d5"), "devices_degraded": byID("d2")},
		},
		{
			fixture:  "mirror-spare-active",
			pool:     "p2",
			poolType: "zfs-raid1",
			devices:  map[string][]string{"devices": byID("d2", "d3"), "devices_degraded": byID("d1")},
		},
		{
			fixture:     "mirror-log-special-replacing",
			pool:        "p3",
			poolType:    "zfs-raid1",
			specialType: "zfs-raid1",
			devices:     map[string][]string{"devices": byID("d1", "d2"), "log": byID("d3", "d4", "d7"), "special": byID("d5", "d6", "d8")},
		},
		{
			fixture:     "raid0-log-special-replacing",
			pool:        "p4",
			poolType:    "zfs-raid0",
			specialType: "zfs-raid0",
			devices:     map[string][]string{"devices": byID("d1"), "log": byID("d2", "d5"), "special": byID("d3", "d6"), "cache": byID("d4")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			content, err := os.ReadFile(filepath.Join("testdata", tc.fixture+".json"))
			require.NoError(t, err)

			zpoolJSON := zpoolStatusPartialParse{}
			require.NoError(t, json.Unmarshal(content, &zpoolJSON))

			layout, err := parseZpoolStatus(zpoolJSON, tc.pool)
			require.NoError(t, err)
			require.Equal(t, tc.poolType, layout.poolType)
			require.Equal(t, tc.specialType, layout.specialType)
			require.Equal(t, tc.devices, layout.devices)
		})
	}
}

// scanStats parses a zpool status scan_stats object.
func scanStats(t *testing.T, raw string) zpoolScanStats {
	t.Helper()

	var scan zpoolScanStats

	require.NoError(t, json.Unmarshal([]byte(raw), &scan))

	return scan
}

func TestPoolActivityStatusReadsScrubPause(t *testing.T) {
	t.Parallel()

	paused := scanStats(t, `{"function": "SCRUB", "state": "SCANNING", "start_time": 1793280000, "end_time": 0, "to_examine": 100, "examined": 10, "errors": 0, "scrub_pause": 1793280600}`)
	running := scanStats(t, `{"function": "SCRUB", "state": "SCANNING", "start_time": 1793280000, "end_time": 0, "to_examine": 100, "examined": 10, "errors": 0, "scrub_pause": 0}`)

	// Paused on disk with no record, as after incus-osd stopped uncleanly.
	scrub, _ := poolActivityStatus("pause-crashed", paused, nil)
	require.True(t, scrub.Paused)
	require.Equal(t, time.Unix(1793280600, 0), scrub.PausedSince)
	require.Empty(t, scrub.PauseReason)

	// Running on disk, whatever an old record says.
	SetPoolActivityPause("pause-stale", "scrub", PoolActivityPause{Paused: true, Since: time.Unix(5, 0), Reason: "io pressure: some avg60 45.0", Pauses: 1})

	scrub, _ = poolActivityStatus("pause-stale", running, nil)
	require.False(t, scrub.Paused)
	require.True(t, scrub.PausedSince.IsZero())
	require.Empty(t, scrub.PauseReason)
	require.Equal(t, 1, scrub.Pauses)
}

func TestPoolActivityPauseReadsBackThroughAPI(t *testing.T) {
	t.Parallel()

	scan := scanStats(t, `{"function": "SCRUB", "state": "SCANNING", "start_time": 1793280000, "end_time": 0, "to_examine": 100, "examined": 10, "errors": 0, "scrub_pause": 1793280610}`)
	trimStats := []zpoolTrimStats{{TrimState: "SUSPENDED", Trimmed: 10, ToTrim: 100, TrimTime: 1793280000, leaf: "d1"}}

	SetPoolActivityPause("pause-api", "scrub", PoolActivityPause{Paused: true, Since: time.Unix(1793280610, 0), Reason: "io pressure: full avg10 23.4", Pauses: 2})
	SetPoolActivityPause("pause-api", "trim", PoolActivityPause{Paused: true, Since: time.Unix(1793280620, 0).UTC(), Reason: "io pressure: some avg60 45.0", Pauses: 1})

	// The same path GetZpoolMembers and GetStorageInfo take.
	scrub, trim := poolActivityStatus("pause-api", scan, trimStats)

	raw, err := json.Marshal(api.SystemStoragePool{Name: "pause-api", LastScrub: scrub, LastTrim: trim})
	require.NoError(t, err)
	require.Contains(t, string(raw), `"pause_reason":"io pressure: full avg10 23.4","pauses":2`)

	var got api.SystemStoragePool

	err = json.Unmarshal(raw, &got)
	require.NoError(t, err)
	require.True(t, got.LastScrub.Paused)
	require.True(t, got.LastScrub.PausedSince.Equal(time.Unix(1793280610, 0)))
	require.Equal(t, "io pressure: full avg10 23.4", got.LastScrub.PauseReason)
	require.Equal(t, 2, got.LastScrub.Pauses)
	require.True(t, got.LastTrim.Paused)
	require.True(t, got.LastTrim.PausedSince.Equal(time.Unix(1793280620, 0)))
	require.Equal(t, "io pressure: some avg60 45.0", got.LastTrim.PauseReason)
	require.Equal(t, 1, got.LastTrim.Pauses)

	// Pools IncusOS never paused report nothing.
	idle := scanStats(t, `{"function": "SCRUB", "state": "FINISHED", "start_time": 1793280000, "end_time": 1793281000, "to_examine": 100, "examined": 100, "errors": 0, "scrub_pause": 0}`)
	scrub, _ = poolActivityStatus("pause-api-other", idle, nil)
	require.False(t, scrub.Paused)
	require.Zero(t, scrub.Pauses)
}

func TestSuspendedTrimLeaves(t *testing.T) {
	t.Parallel()

	all, every := suspendedTrimLeaves([]zpoolTrimStats{
		{TrimState: "SUSPENDED", leaf: "d1"},
		{TrimState: "SUSPENDED", leaf: "d2"},
		{TrimNotSup: 1, leaf: "hdd"},
	})
	require.Equal(t, []string{"d1", "d2"}, all)
	require.True(t, every)

	some, every := suspendedTrimLeaves([]zpoolTrimStats{
		{TrimState: "COMPLETE", leaf: "d1"},
		{TrimState: "SUSPENDED", leaf: "d2"},
	})
	require.Equal(t, []string{"d2"}, some)
	require.False(t, every)
}
