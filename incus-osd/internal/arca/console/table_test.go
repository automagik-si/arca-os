package console

import (
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
)

func TestCPUPercentDelta(t *testing.T) {
	t.Parallel()

	r := &incusReader{samples: map[string]cpuSample{}}

	inst := api.InstanceFull{
		Instance: api.Instance{Name: "web", Project: "default", Type: "container", Status: "Running"},
		State:    &api.InstanceState{Status: "Running", CPU: api.InstanceStateCPU{Usage: int64(time.Second)}},
	}

	t0 := time.Date(2026, 11, 3, 14, 22, 0, 0, time.UTC)

	// One sample is not a delta.
	if got := r.cpuPercent(inst, t0, 8); got != -1 {
		t.Errorf("the first sample is %v, want -1", got)
	}

	// Half a second of one core over one second on 8 threads is 6.25% of the host.
	inst.State.CPU.Usage += int64(500 * time.Millisecond)

	if got := r.cpuPercent(inst, t0.Add(time.Second), 8); got < 6.24 || got > 6.26 {
		t.Errorf("the delta is %v, want 6.25", got)
	}

	// A counter that goes backwards is not a delta.
	inst.State.CPU.Usage = 0

	if got := r.cpuPercent(inst, t0.Add(2*time.Second), 8); got != -1 {
		t.Errorf("a backwards counter is %v, want -1", got)
	}

	// A stopped instance has no sample.
	inst.Status = "Stopped"

	if got := r.cpuPercent(inst, t0.Add(3*time.Second), 8); got != -1 {
		t.Errorf("a stopped instance is %v, want -1", got)
	}
}

func TestMiniBarLimits(t *testing.T) { //nolint:paralleltest // points the package's host limits at fixtures
	savedThreads, savedMem := hostThreads, hostMemoryBytes

	t.Cleanup(func() { hostThreads, hostMemoryBytes = savedThreads, savedMem })

	hostThreads = func() int { return 4 }
	hostMemoryBytes = func() int64 { return 4 << 30 } // 4 GiB

	if got := bar(5, 5); got != "█████" {
		t.Errorf("a bar at its limit is %q", got)
	}

	if got := bar(0, 5); got != "░░░░░" {
		t.Errorf("an empty bar is %q", got)
	}

	if got := bar(2, 4); got != "███░░" { // 50% of five cells rounds to three
		t.Errorf("a half-full bar is %q", got)
	}

	if tag := barColor(90, 100); tag != tagBold {
		t.Errorf("a bar at 90%% is %q, want the plain tag", tag)
	}

	if tag := barColor(91, 100); tag != tagWarn {
		t.Errorf("a bar above 90%% is %q, want the warning tag", tag)
	}

	// No limit of its own: the instance is measured against the host.
	inst := Instance{Status: "Running", CPUPercent: 50, CPULimit: 0, MemUsage: 2 << 30, MemLimit: 0}

	use, limit := cpuUse(inst)
	if use != 50 || limit != 100 {
		t.Errorf("a CPU without a limit is %v of %v, want 50 of 100", use, limit)
	}

	muse, mlimit := memUse(inst, hostMemoryBytes())
	if muse != 2<<30 || mlimit != 4<<30 {
		t.Errorf("memory without a limit is %d of %d", muse, mlimit)
	}

	// With a limit, its own limit counts: two threads of four is half the host.
	inst.CPULimit, inst.MemLimit = 2, 1<<30

	if use, limit := cpuUse(inst); use != 50 || limit != 50 {
		t.Errorf("a CPU with a limit is %v of %v", use, limit)
	}

	if use, limit := memUse(inst, hostMemoryBytes()); use != 2<<30 || limit != 1<<30 {
		t.Errorf("memory with a limit is %d of %d", use, limit)
	}

	// The limits a saved instance carries are read from its configuration.
	if got := cpuLimit(map[string]string{"limits.cpu": "0-3"}); got != 4 {
		t.Errorf("limits.cpu 0-3 is %v, want 4", got)
	}

	if got := cpuLimit(map[string]string{"limits.cpu": "2"}); got != 2 {
		t.Errorf("limits.cpu 2 is %v, want 2", got)
	}

	if got := memoryLimit(map[string]string{"limits.memory": "1GiB"}, hostMemoryBytes()); got != 1<<30 {
		t.Errorf("limits.memory 1GiB is %d", got)
	}

	if got := memoryLimit(map[string]string{"limits.memory": "50%"}, hostMemoryBytes()); got != 2<<30 {
		t.Errorf("limits.memory 50%% is %d", got)
	}

	if got := memoryLimit(nil, hostMemoryBytes()); got != 0 {
		t.Errorf("no limit is %d, want 0", got)
	}
}
