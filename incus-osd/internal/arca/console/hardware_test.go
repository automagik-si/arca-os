package console

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// testHardware is the fixed host the golden screens draw.
var testHardware = hostHardware{
	HasCPU:     true,
	CPUPercent: 12,
	Threads:    8,
	HasMem:     true,
	MemUsed:    3 << 30,
	MemTotal:   8 << 30,
	HasDisk:    true,
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

// cpuFixture is one /proc/stat aggregate line: user nice system idle iowait irq
// softirq steal guest guest_nice.
func cpuFixture(user, nice, system, idle, iowait string) string {
	return "cpu  " + strings.Join([]string{user, nice, system, idle, iowait, "0", "0", "0", "0", "0"}, " ") + "\n"
}

func TestCPUSample(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "stat", cpuFixture("100", "0", "100", "700", "100")+"cpu0 9 9 9 9\n")

	// idle = 700+100 (iowait) = 800, total = 1000.
	first, err := readHostCPUSample(root)
	if err != nil {
		t.Fatal(err)
	}

	// idle = 1400+100 = 1500, total = 1800; 100 busy of 800 is 12%.
	writeFixture(t, root, "stat", cpuFixture("200", "0", "100", "1400", "100"))

	second, err := readHostCPUSample(root)
	if err != nil {
		t.Fatal(err)
	}

	if got := first.percent(second); got != 12 {
		t.Errorf("percent = %d, want 12 (iowait counts as idle)", got)
	}

	writeFixture(t, root, "stat", "cpu 1 2\n")

	_, err = readHostCPUSample(root)
	if err == nil {
		t.Error("a short /proc/stat cpu line was accepted")
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

	// Missing or malformed totals are an error, not a zero.
	for _, body := range []string{
		"MemFree:         1048576 kB\n",
		"MemTotal:        lots kB\nMemAvailable:    3145728 kB\n",
		"MemTotal:        -1 kB\nMemAvailable:    3145728 kB\n",
	} {
		writeFixture(t, root, "meminfo", body)

		_, _, err = readMemInfo(root)
		if err == nil {
			t.Errorf("malformed meminfo %q was accepted", strings.SplitN(body, "\n", 2)[0])
		}
	}
}

func TestDiskMath(t *testing.T) { //nolint:paralleltest // injects the package statfs
	old := statfs

	t.Cleanup(func() { statfs = old })

	statfs = func(_ string, stat *unix.Statfs_t) error {
		stat.Blocks, stat.Bfree, stat.Bsize = 1000, 250, 4096

		return nil
	}

	used, total, err := diskUsage("/")
	if err != nil {
		t.Fatal(err)
	}

	if used != 750*4096 || total != 1000*4096 {
		t.Errorf("used/total = %d/%d, want %d/%d", used, total, 750*4096, 1000*4096)
	}

	statfs = func(_ string, _ *unix.Statfs_t) error { return errors.New("no statfs") }

	_, _, err = diskUsage("/")
	if err == nil {
		t.Error("a failed statfs read as a disk figure")
	}

	statfs = func(_ string, stat *unix.Statfs_t) error {
		stat.Blocks, stat.Bfree, stat.Bsize = 10, 11, 4096

		return nil
	}

	_, _, err = diskUsage("/")
	if err == nil {
		t.Error("more free blocks than total blocks read as a disk figure")
	}
}

func TestTemperature(t *testing.T) {
	t.Parallel()

	// k10temp wins over the other sensors.
	root := t.TempDir()
	writeFixture(t, root, "class/hwmon/hwmon0/name", "coretemp\n")
	writeFixture(t, root, "class/hwmon/hwmon0/temp1_input", "50000\n")
	writeFixture(t, root, "class/hwmon/hwmon1/name", "k10temp\n")
	writeFixture(t, root, "class/hwmon/hwmon1/temp1_input", "45000\n")

	temp, ok := readTemperature(root)
	if !ok || temp != 45 {
		t.Errorf("temperature = %v (found %t), want the k10temp 45", temp, ok)
	}

	// An Intel host with only coretemp still reports its CPU.
	root = t.TempDir()
	writeFixture(t, root, "class/hwmon/hwmon0/name", "coretemp\n")
	writeFixture(t, root, "class/hwmon/hwmon0/temp1_input", "50000\n")

	temp, ok = readTemperature(root)
	if !ok || temp != 50 {
		t.Errorf("coretemp temperature = %v (found %t), want 50", temp, ok)
	}

	// The thermal fallback needs the x86_pkg_temp type, not the first zone.
	root = t.TempDir()
	writeFixture(t, root, "class/thermal/thermal_zone0/type", "acpitz\n")
	writeFixture(t, root, "class/thermal/thermal_zone0/temp", "20000\n")
	writeFixture(t, root, "class/thermal/thermal_zone10/type", "x86_pkg_temp\n")
	writeFixture(t, root, "class/thermal/thermal_zone10/temp", "43000\n")

	temp, ok = readTemperature(root)
	if !ok || temp != 43 {
		t.Errorf("thermal fallback = %v (found %t), want the x86_pkg_temp 43", temp, ok)
	}

	// An acpitz zone alone is not a CPU temperature.
	root = t.TempDir()
	writeFixture(t, root, "class/thermal/thermal_zone0/type", "acpitz\n")
	writeFixture(t, root, "class/thermal/thermal_zone0/temp", "20000\n")

	_, ok = readTemperature(root)
	if ok {
		t.Error("an acpitz zone was read as a CPU temperature")
	}

	// An implausible value is a dash, not -273 or 2000 degrees.
	for _, value := range []string{"-273200\n", "2000000\n"} {
		root = t.TempDir()
		writeFixture(t, root, "class/hwmon/hwmon0/name", "k10temp\n")
		writeFixture(t, root, "class/hwmon/hwmon0/temp1_input", value)

		_, ok = readTemperature(root)
		if ok {
			t.Errorf("temperature %q was accepted", strings.TrimSpace(value))
		}
	}

	_, ok = readTemperature(t.TempDir())
	if ok {
		t.Error("a host without a sensor reported a temperature")
	}
}

func TestReadHostHardware(t *testing.T) { //nolint:paralleltest // points the package roots and statfs at fixtures
	root := t.TempDir()
	writeFixture(t, root, "proc/stat", cpuFixture("100", "0", "100", "700", "100"))
	writeFixture(t, root, "proc/meminfo", "MemTotal:       8388608 kB\nMemAvailable:    3145728 kB\n")
	writeFixture(t, root, "sys/class/hwmon/hwmon0/name", "k10temp\n")
	writeFixture(t, root, "sys/class/hwmon/hwmon0/temp1_input", "45000\n")

	oldProc, oldSys, oldDisk, oldStatfs := procRoot, sysRoot, diskRoot, statfs

	t.Cleanup(func() { procRoot, sysRoot, diskRoot, statfs = oldProc, oldSys, oldDisk, oldStatfs })

	procRoot, sysRoot, diskRoot = filepath.Join(root, "proc"), filepath.Join(root, "sys"), t.TempDir()
	statfs = func(_ string, stat *unix.Statfs_t) error {
		stat.Blocks, stat.Bfree, stat.Bsize = 1000, 250, 4096

		return nil
	}

	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	first := readHostHardware(hostCPUSample{}, false, time.Time{}, base)
	if !first.have {
		t.Fatal("the first read did not keep a CPU sample")
	}

	if first.hardware.HasCPU {
		t.Error("the first read reported a CPU percentage without a previous sample")
	}

	if !first.hardware.HasMem || first.hardware.MemTotal != 8<<30 || first.hardware.MemUsed != 5<<30 {
		t.Errorf("memory = %+v, want 5G used of 8G", first.hardware)
	}

	if !first.hardware.HasDisk || first.hardware.DiskTotal != 1000*4096 || first.hardware.DiskUsed != 750*4096 {
		t.Errorf("disk = %+v, want the injected 750/1000 blocks", first.hardware)
	}

	if !first.hardware.HasTemp || first.hardware.TempC != 45 {
		t.Errorf("temperature = %+v, want 45", first.hardware)
	}

	// A window shorter than hardwareWindow keeps the dash.
	short := readHostHardware(first.cpu, first.have, first.at, base.Add(500*time.Millisecond))
	if short.hardware.HasCPU {
		t.Error("a 500 ms window produced a CPU percentage")
	}

	// Two seconds later the same fixture gives the 12% figure.
	writeFixture(t, root, "proc/stat", cpuFixture("200", "0", "100", "1400", "100"))

	second := readHostHardware(first.cpu, first.have, first.at, base.Add(2*time.Second))
	if !second.hardware.HasCPU || second.hardware.CPUPercent != 12 {
		t.Errorf("second read = %+v, want 12%% CPU", second.hardware)
	}

	// A failed /proc/stat leaves the CPU unknown but keeps the other figures.
	err := os.Remove(filepath.Join(root, "proc", "stat"))
	if err != nil {
		t.Fatal(err)
	}

	partial := readHostHardware(hostCPUSample{}, false, time.Time{}, base)
	if partial.hardware.HasCPU {
		t.Error("a failed /proc/stat reported a CPU percentage")
	}

	if !partial.hardware.HasMem || !partial.hardware.HasDisk || !partial.hardware.HasTemp {
		t.Errorf("a failed /proc/stat dropped another figure: %+v", partial.hardware)
	}

	// A malformed meminfo fails only the memory figure.
	writeFixture(t, root, "proc/meminfo", "MemTotal:        lots kB\n")

	partial = readHostHardware(hostCPUSample{}, false, time.Time{}, base)
	if partial.hardware.HasMem {
		t.Error("a malformed meminfo reported memory")
	}

	if !partial.hardware.HasDisk || !partial.hardware.HasTemp {
		t.Errorf("a malformed meminfo dropped another figure: %+v", partial.hardware)
	}

	// A failed statfs fails only the disk figure.
	writeFixture(t, root, "proc/meminfo", "MemTotal:       8388608 kB\nMemAvailable:    3145728 kB\n")

	statfs = func(_ string, _ *unix.Statfs_t) error { return errors.New("no statfs") }

	partial = readHostHardware(hostCPUSample{}, false, time.Time{}, base)
	if partial.hardware.HasDisk {
		t.Error("a failed statfs reported a disk figure")
	}

	if !partial.hardware.HasMem {
		t.Error("a failed statfs dropped the memory figure")
	}
}

func TestHardwareText(t *testing.T) {
	t.Parallel()

	if got := hostCPUText(hostHardware{Threads: 8}); got != "— of 8 threads" {
		t.Errorf("empty CPU text = %q, want the dash", got)
	}

	if got := hostCPUPercentText(hostHardware{}); got != "—" {
		t.Errorf("empty CPU percent = %q, want the dash", got)
	}

	if got := hostMemText(hostHardware{}); got != "—" {
		t.Errorf("empty memory text = %q, want the dash", got)
	}

	if got := hostDiskText(hostHardware{}); got != "—" {
		t.Errorf("empty disk text = %q, want the dash", got)
	}

	if got := hostTempText(hostHardware{}); got != "—" {
		t.Errorf("empty temperature text = %q, want the dash", got)
	}

	if got := hostCPUText(testHardware); got != "12% of 8 threads" {
		t.Errorf("CPU text = %q, want 12%% of 8 threads", got)
	}

	if got := hostCPUPercentText(testHardware); got != "12%" {
		t.Errorf("CPU percent = %q, want 12%%", got)
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

	wide := draw(t, tracerSnapshot(), 120, 36)
	text := screenText(wide)

	for _, want := range []string{"Hardware", "CPU 12% of 8 threads", "45°C", "Mem 3G / 8G", "Disk 20G / 100G", "Resources"} {
		if !strings.Contains(text, want) {
			t.Errorf("the 120x36 layout has no %q", want)
		}
	}

	// The 24-column resources column uses the short CPU figure and fits.
	resources := rowOf(wide, "Resources")
	cpuLine := screenLine(wide, resources+1)

	if !strings.Contains(cpuLine, "CPU 12%") || strings.Contains(cpuLine, "threads") {
		t.Errorf("the resources CPU line is %q, want the short CPU 12%%", cpuLine)
	}

	compact := screenText(draw(t, tracerSnapshot(), 80, 25))
	for _, want := range []string{"Hardware 12% of 8 threads", "Mem 3G / 8G", "Disk 20G / 100G", "45°C"} {
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

func TestHardwareFailedReading(t *testing.T) {
	t.Parallel()

	empty := hostHardware{Threads: 8}

	wide := screenText(drawWithHardware(t, tracerSnapshot(), empty, 120, 36))
	for _, want := range []string{"CPU — of 8 threads", "Mem —", "Disk —", "Temp —"} {
		if !strings.Contains(wide, want) {
			t.Errorf("a failed read has no %q", want)
		}
	}

	if strings.Contains(wide, "0 / 0") {
		t.Error("a failed read was drawn as 0 / 0")
	}

	compact := screenText(drawWithHardware(t, tracerSnapshot(), empty, 80, 25))
	if !strings.Contains(compact, "Hardware — of 8 threads") {
		t.Error("a failed read has no compact dash")
	}

	if strings.Contains(compact, "0 / 0") {
		t.Error("a failed read was drawn as 0 / 0 in the compact strip")
	}
}
