package console

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// hostHardware is the host's resource use, read once per snapshot and drawn by
// the hardware box, the compact strip and the resources column. The zero value
// means "not read yet": the pane shows a dash, never a made-up number.
type hostHardware struct {
	HasCPU     bool
	CPUPercent int
	Threads    int

	MemUsed  int64
	MemTotal int64

	DiskUsed  int64
	DiskTotal int64

	HasTemp bool
	TempC   float64
}

// hostCPUSample is the host's cumulative CPU jiffies at one read.
type hostCPUSample struct {
	idle  uint64
	total uint64
}

// percent returns the CPU use between the sample and now, 0..100.
func (s hostCPUSample) percent(now hostCPUSample) int {
	total := now.total - s.total
	if total == 0 {
		return 0
	}

	idle := min(now.idle-s.idle, total)

	return int((total - idle) * 100 / total) //nolint:gosec // the division bounds the value to 0..100
}

// readHostCPUSample reads the aggregate "cpu" line of /proc/stat.
func readHostCPUSample(procRoot string) (hostCPUSample, error) {
	body, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return hostCPUSample{}, err
	}

	for line := range strings.SplitSeq(string(body), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}

		fields := strings.Fields(line)[1:]
		if len(fields) < 4 {
			return hostCPUSample{}, errors.New("short /proc/stat cpu line")
		}

		var total, idle uint64

		for i, field := range fields {
			// The guest fields are already counted in user and nice.
			if i >= 8 {
				break
			}

			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return hostCPUSample{}, err
			}

			total += value

			// idle is the fourth field and iowait the fifth.
			if i == 3 || i == 4 {
				idle += value
			}
		}

		return hostCPUSample{idle: idle, total: total}, nil
	}

	return hostCPUSample{}, errors.New("no /proc/stat cpu line")
}

// readMemInfo reads the host's total and available memory in bytes. A kernel
// without MemAvailable falls back to MemFree, and a missing value reads as 0.
func readMemInfo(procRoot string) (int64, int64, error) {
	body, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0, 0, err
	}

	var totalKB, availableKB, freeKB int64

	for line := range strings.SplitSeq(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB = meminfoKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			availableKB = meminfoKB(line)
		case strings.HasPrefix(line, "MemFree:"):
			freeKB = meminfoKB(line)
		default:
			// Other meminfo lines are not part of the hardware pane.
		}
	}

	if availableKB == 0 {
		availableKB = freeKB
	}

	return totalKB * 1024, availableKB * 1024, nil
}

// meminfoKB returns the kB value of one /proc/meminfo line, or 0.
func meminfoKB(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}

	value, _ := strconv.ParseInt(fields[1], 10, 64)

	return value
}

// diskUsage returns the used and total bytes of the filesystem holding path.
func diskUsage(path string) (int64, int64, error) {
	var stat unix.Statfs_t

	err := unix.Statfs(path, &stat)
	if err != nil {
		return 0, 0, err
	}

	// Block counts and block size fit in int64 on any real filesystem.
	total := int64(stat.Blocks) * stat.Bsize //nolint:gosec
	free := int64(stat.Bfree) * stat.Bsize   //nolint:gosec

	return total - free, total, nil
}

// readTemperature reads the CPU temperature in degrees Celsius: the k10temp
// hwmon sensor first (the AMD boards), then the first thermal zone. A host
// without either reads as no temperature and the pane shows a dash.
func readTemperature(sysRoot string) (float64, bool) {
	hwmonRoot := filepath.Join(sysRoot, "class", "hwmon")

	entries, err := os.ReadDir(hwmonRoot)
	if err == nil {
		for _, entry := range entries {
			name, err := os.ReadFile(filepath.Join(hwmonRoot, entry.Name(), "name"))
			if err != nil || strings.TrimSpace(string(name)) != "k10temp" {
				continue
			}

			if temp, ok := readMillidegrees(filepath.Join(hwmonRoot, entry.Name(), "temp1_input")); ok {
				return temp, true
			}
		}
	}

	zones, err := filepath.Glob(filepath.Join(sysRoot, "class", "thermal", "thermal_zone*", "temp"))
	if err == nil {
		for _, zone := range zones {
			if temp, ok := readMillidegrees(zone); ok {
				return temp, true
			}
		}
	}

	return 0, false
}

// readMillidegrees reads one millidegree Celsius sensor file.
func readMillidegrees(path string) (float64, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	value, err := strconv.ParseFloat(strings.TrimSpace(string(body)), 64)
	if err != nil {
		return 0, false
	}

	return value / 1000, true
}

// The roots and the reader are package variables so the tests can point them at
// fixture trees and the golden screens at a fixed host, never a live machine.
var (
	diskRoot           = "/"
	hostHardwareReader = realReadHostHardware
)

// realReadHostHardware reads the host's hardware use. The previous sample is
// needed to turn the CPU's cumulative jiffies into a percentage; without it the
// first read reports no CPU figure yet.
func realReadHostHardware(prev hostCPUSample, havePrev bool) (hostHardware, hostCPUSample, bool) {
	hw := hostHardware{Threads: hostThreads()}

	sample, err := readHostCPUSample(procRoot)
	if err != nil {
		return hw, prev, false
	}

	if havePrev {
		hw.HasCPU = true
		hw.CPUPercent = prev.percent(sample)
	}

	total, available, err := readMemInfo(procRoot)
	if err == nil && total > 0 {
		hw.MemTotal = total
		hw.MemUsed = total - available
	}

	used, size, err := diskUsage(diskRoot)
	if err == nil && size > 0 {
		hw.DiskTotal = size
		hw.DiskUsed = used
	}

	if temp, ok := readTemperature(sysRoot); ok {
		hw.HasTemp = true
		hw.TempC = temp
	}

	return hw, sample, true
}

// refreshHardware reads the host's hardware use for the next draw. It runs on
// the event loop, like update.
func (v *view) refreshHardware() {
	hw, sample, ok := hostHardwareReader(v.cpu, v.haveCPU)

	v.hw = hw
	if ok {
		v.cpu = sample
		v.haveCPU = true
	}
}

// hostCPUText is the CPU figure: the use of the host's threads, or a dash until the
// second sample makes a percentage possible.
func hostCPUText(hw hostHardware) string {
	percent := "—"
	if hw.HasCPU {
		percent = fmt.Sprintf("%d%%", hw.CPUPercent)
	}

	return fmt.Sprintf("%s of %d threads", percent, hw.Threads)
}

// hostMemText is the host memory used over total.
func hostMemText(hw hostHardware) string {
	return shortBytes(hw.MemUsed) + " / " + shortBytes(hw.MemTotal)
}

// hostDiskText is the root filesystem used over total.
func hostDiskText(hw hostHardware) string {
	return shortBytes(hw.DiskUsed) + " / " + shortBytes(hw.DiskTotal)
}

// hostTempText is the CPU temperature, or a dash when the host has no sensor.
func hostTempText(hw hostHardware) string {
	if !hw.HasTemp {
		return "—"
	}

	return fmt.Sprintf("%.0f°C", hw.TempC)
}
