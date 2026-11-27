package console

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testHardware is the fixed host the golden screens draw. TestMain points the
// reader at it, so no test reads the machine it runs on.
var testHardware = hostHardware{
	HasCPU:     true,
	CPUPercent: 12,
	Threads:    8,
	MemUsed:    3 << 30,
	MemTotal:   8 << 30,
	DiskUsed:   20 << 30,
	DiskTotal:  100 << 30,
	HasTemp:    true,
	TempC:      45,
}

// writeFixture writes one file under a fixture tree.
func writeFixture(t *testing.T, root, name, body string) {
	t.Helper()

	path := filepath.Join(root, name)

	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(body), 0o644) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
}

func TestCPUSample(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "stat", "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 9 9 9 9\n")

	first, err := readHostCPUSample(root)
	if err != nil {
		t.Fatal(err)
	}

	writeFixture(t, root, "stat", "cpu  200 0 100 1500 0 0 0 0 0 0\n")

	second, err := readHostCPUSample(root)
	if err != nil {
		t.Fatal(err)
	}

	if got := first.percent(second); got != 12 {
		t.Errorf("percent = %d, want 12", got)
	}

	if got := first.percent(first); got != 0 {
		t.Errorf("percent with no time passed = %d, want 0", got)
	}
}

func TestMemInfo(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "meminfo", "MemTotal:       8388608 kB\nMemFree:         1048576 kB\nMemAvailable:    3145728 kB\n")

	total, available, err := readMemInfo(root)
	if err != nil {
		t.Fatal(err)
	}

	if total != 8<<30 || available != 3<<30 {
		t.Errorf("total/available = %d/%d, want %d/%d", total, available, int64(8<<30), int64(3<<30))
	}

	// A kernel without MemAvailable falls back to MemFree.
	writeFixture(t, root, "meminfo", "MemTotal:       8388608 kB\nMemFree:         1048576 kB\n")

	total, available, err = readMemInfo(root)
	if err != nil {
		t.Fatal(err)
	}

	if total != 8<<30 || available != 1<<30 {
		t.Errorf("fallback total/available = %d/%d, want %d/%d", total, available, int64(8<<30), int64(1<<30))
	}
}

func TestDiskUsage(t *testing.T) {
	t.Parallel()

	used, total, err := diskUsage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if total <= 0 || used < 0 || used > total {
		t.Errorf("disk usage = %d/%d, want 0 <= used <= total and total > 0", used, total)
	}
}

func TestTemperature(t *testing.T) {
	t.Parallel()

	// k10temp wins over the thermal zone.
	root := t.TempDir()
	writeFixture(t, root, "class/hwmon/hwmon0/name", "acpitz\n")
	writeFixture(t, root, "class/hwmon/hwmon0/temp1_input", "42000\n")
	writeFixture(t, root, "class/thermal/thermal_zone0/temp", "39000\n")
	writeFixture(t, root, "class/hwmon/hwmon1/name", "k10temp\n")
	writeFixture(t, root, "class/hwmon/hwmon1/temp1_input", "45000\n")

	temp, ok := readTemperature(root)
	if !ok || temp != 45 {
		t.Errorf("k10temp temperature = %v (found %t), want 45", temp, ok)
	}

	// No k10temp: the first thermal zone answers.
	root = t.TempDir()
	writeFixture(t, root, "class/thermal/thermal_zone0/temp", "43000\n")

	temp, ok = readTemperature(root)
	if !ok || temp != 43 {
		t.Errorf("thermal fallback = %v (found %t), want 43", temp, ok)
	}

	// No sensor at all: the pane shows a dash, not zero degrees.
	if _, ok := readTemperature(t.TempDir()); ok {
		t.Error("a host without a sensor reported a temperature")
	}
}

func TestReadHostHardware(t *testing.T) { //nolint:paralleltest // points the package roots at fixtures
	root := t.TempDir()
	writeFixture(t, root, "proc/stat", "cpu  100 0 100 800 0 0 0 0 0 0\n")
	writeFixture(t, root, "proc/meminfo", "MemTotal:       8388608 kB\nMemAvailable:    3145728 kB\n")
	writeFixture(t, root, "sys/class/hwmon/hwmon0/name", "k10temp\n")
	writeFixture(t, root, "sys/class/hwmon/hwmon0/temp1_input", "45000\n")

	oldProc, oldSys, oldDisk := procRoot, sysRoot, diskRoot

	t.Cleanup(func() { procRoot, sysRoot, diskRoot = oldProc, oldSys, oldDisk })

	procRoot, sysRoot, diskRoot = filepath.Join(root, "proc"), filepath.Join(root, "sys"), t.TempDir()

	first, sample, ok := realReadHostHardware(hostCPUSample{}, false)
	if !ok {
		t.Fatal("the first hardware read failed")
	}

	if first.HasCPU {
		t.Error("the first read reported a CPU percentage without a previous sample")
	}

	if first.Threads != 8 || first.MemTotal != 8<<30 || first.MemUsed != 5<<30 {
		t.Errorf("threads/memory = %d/%d of %d, want 8/%d of %d", first.Threads, first.MemUsed, first.MemTotal, int64(5<<30), int64(8<<30))
	}

	if first.DiskTotal <= 0 {
		t.Error("the disk total was not read")
	}

	if !first.HasTemp || first.TempC != 45 {
		t.Errorf("temperature = %v (found %t), want 45", first.TempC, first.HasTemp)
	}

	writeFixture(t, root, "proc/stat", "cpu  200 0 100 1500 0 0 0 0 0 0\n")

	second, _, ok := realReadHostHardware(sample, true)
	if !ok || !second.HasCPU || second.CPUPercent != 12 {
		t.Errorf("second read = %+v (ok %t), want a 12%% CPU reading", second, ok)
	}
}

func TestHardwareText(t *testing.T) {
	t.Parallel()

	if got := hostCPUText(hostHardware{Threads: 8}); got != "— of 8 threads" {
		t.Errorf("empty CPU text = %q, want the dash", got)
	}

	if got := hostTempText(hostHardware{}); got != "—" {
		t.Errorf("empty temperature text = %q, want the dash", got)
	}

	if got := hostMemText(testHardware); got != "3G / 8G" {
		t.Errorf("memory text = %q, want 3G / 8G", got)
	}

	if got := hostDiskText(testHardware); got != "20G / 100G" {
		t.Errorf("disk text = %q, want 20G / 100G", got)
	}

	if got := hostTempText(testHardware); got != "45°C" {
		t.Errorf("temperature text = %q, want 45°C", got)
	}
}

func TestHardwareLayouts(t *testing.T) {
	t.Parallel()

	wide := screenText(draw(t, tracerSnapshot(), 120, 36))

	for _, want := range []string{"Hardware", "CPU 12% of 8 threads", "45°C", "Mem 3G / 8G", "Disk 20G / 100G", "Resources"} {
		if !strings.Contains(wide, want) {
			t.Errorf("the 120x36 layout has no %q", want)
		}
	}

	compact := screenText(draw(t, tracerSnapshot(), 80, 25))
	for _, want := range []string{"Hardware 12% of 8 threads", "3G / 8G", "20G / 100G", "45°C"} {
		if !strings.Contains(compact, want) {
			t.Errorf("the 80x25 layout has no %q", want)
		}
	}

	if strings.Contains(compact, "Resources") {
		t.Error("the 80x25 layout drew the wide resources column")
	}

	if strings.Contains(screenText(draw(t, tracerSnapshot(), 60, 20)), "Hardware") {
		t.Error("a too-small screen drew the hardware strip")
	}
}
