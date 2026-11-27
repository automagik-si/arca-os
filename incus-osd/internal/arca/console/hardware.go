package console

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// hostHardware is the host's resource use, read off the screen's event loop and
// drawn from the last sample. Every figure is known or unknown on its own: a
// failed read shows a dash for that figure and never a made-up zero.
type hostHardware struct {
	HasCPU     bool
	CPUPercent int
	Threads    int

	HasMem   bool
	MemUsed  int64
	MemTotal int64

	HasDisk   bool
	DiskUsed  int64
	DiskTotal int64

	HasTemp bool
	TempC   float64
}

// Hardware sampling: the pane reads on its own timer, and a CPU percentage is
// only meaningful across two samples at least hardwareWindow apart.
const hardwareWindow = time.Second

// hardwareEvery is the hardware sampling period. It is a variable so a test can
// shorten it; the console samples the host every two seconds.
var hardwareEvery = 2 * time.Second

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
// without MemAvailable falls back to MemFree; a missing or malformed MemTotal,
// or no available figure at all, is an error so the pane shows a dash.
func readMemInfo(procRoot string) (int64, int64, error) {
	body, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0, 0, err
	}

	var totalKB, availableKB, freeKB int64

	var haveTotal, haveAvailable, haveFree bool

	for line := range strings.SplitSeq(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB, haveTotal = meminfoKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			availableKB, haveAvailable = meminfoKB(line)
		case strings.HasPrefix(line, "MemFree:"):
			freeKB, haveFree = meminfoKB(line)
		default:
			// Other meminfo lines are not part of the hardware pane.
		}
	}

	if !haveTotal || totalKB <= 0 {
		return 0, 0, errors.New("no usable MemTotal")
	}

	if !haveAvailable || availableKB <= 0 {
		if !haveFree {
			return 0, 0, errors.New("no usable MemAvailable or MemFree")
		}

		availableKB = freeKB
	}

	availableKB = min(availableKB, totalKB)

	return totalKB * 1024, availableKB * 1024, nil
}

// meminfoKB returns the kB value of one /proc/meminfo line.
func meminfoKB(line string) (int64, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, false
	}

	value, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || value < 0 {
		return 0, false
	}

	return value, true
}

// diskRoot is the filesystem the disk figure describes, and statfs is
// unix.Statfs; both are injected by the tests so the disk maths is checked on
// fixed block counts rather than the machine running the tests.
var (
	diskRoot = "/"
	statfs   = unix.Statfs
)

// diskUsage returns the used and total bytes of the filesystem holding path.
func diskUsage(path string) (int64, int64, error) {
	var stat unix.Statfs_t

	err := statfs(path, &stat)
	if err != nil {
		return 0, 0, err
	}

	// Block counts and block size fit in int64 on any real filesystem.
	total := int64(stat.Blocks) * stat.Bsize //nolint:gosec
	free := int64(stat.Bfree) * stat.Bsize   //nolint:gosec

	if total <= 0 || free < 0 || free > total {
		return 0, 0, errors.New("implausible statfs figures")
	}

	return total - free, total, nil
}

// cpuHwmonNames are the CPU temperature sensors, in preference order: the AMD
// k10temp and zenpower, then the Intel coretemp.
var cpuHwmonNames = []string{"k10temp", "zenpower", "coretemp"}

// readTemperature reads the CPU temperature in degrees Celsius from the CPU
// hwmon sensor, then from an x86_pkg_temp thermal zone. A value outside 0..150
// is not a CPU temperature and reads as no temperature at all.
func readTemperature(sysRoot string) (float64, bool) {
	hwmonRoot := filepath.Join(sysRoot, "class", "hwmon")

	entries, err := os.ReadDir(hwmonRoot)
	if err == nil {
		for _, wanted := range cpuHwmonNames {
			for _, entry := range entries {
				name, err := os.ReadFile(filepath.Join(hwmonRoot, entry.Name(), "name"))
				if err != nil || strings.TrimSpace(string(name)) != wanted {
					continue
				}

				if temp, ok := readMillidegrees(filepath.Join(hwmonRoot, entry.Name(), "temp1_input")); ok && plausibleTemp(temp) {
					return temp, true
				}
			}
		}
	}

	zones, err := filepath.Glob(filepath.Join(sysRoot, "class", "thermal", "thermal_zone*"))
	if err == nil {
		for _, zone := range zones {
			kind, err := os.ReadFile(filepath.Join(zone, "type"))
			if err != nil || strings.TrimSpace(string(kind)) != "x86_pkg_temp" {
				continue
			}

			if temp, ok := readMillidegrees(filepath.Join(zone, "temp")); ok && plausibleTemp(temp) {
				return temp, true
			}
		}
	}

	return 0, false
}

// plausibleTemp reports whether a Celsius reading can be a CPU temperature.
func plausibleTemp(temp float64) bool {
	return temp > 0 && temp <= 150
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

// hostHardwareRead is one sampling result, with the CPU sample and time the next
// tick needs to compute a percentage across a window of at least hardwareWindow.
type hostHardwareRead struct {
	hardware hostHardware
	cpu      hostCPUSample
	at       time.Time
	have     bool
}

// realReadHostHardware reads every figure independently: a failed /proc/stat
// leaves the CPU unknown but still reads memory, disk and temperature.
func realReadHostHardware(prev hostCPUSample, havePrev bool, prevAt time.Time, now time.Time) hostHardwareRead {
	hw := hostHardware{Threads: hostThreads()}

	sample, err := readHostCPUSample(procRoot)
	if err == nil {
		if havePrev && now.Sub(prevAt) >= hardwareWindow {
			hw.HasCPU = true
			hw.CPUPercent = prev.percent(sample)
		}

		prev, havePrev, prevAt = sample, true, now
	}

	total, available, err := readMemInfo(procRoot)
	if err == nil {
		hw.HasMem = true
		hw.MemTotal = total
		hw.MemUsed = total - available
	}

	used, size, err := diskUsage(diskRoot)
	if err == nil {
		hw.HasDisk = true
		hw.DiskUsed = used
		hw.DiskTotal = size
	}

	temp, ok := readTemperature(sysRoot)
	if ok {
		hw.HasTemp = true
		hw.TempC = temp
	}

	return hostHardwareRead{hardware: hw, cpu: prev, at: prevAt, have: havePrev}
}

// readHostHardware is the reader collect runs. A test replaces it to count the
// timer's ticks, so removing the timer from collect fails a test.
var readHostHardware = realReadHostHardware

// setHardware replaces the sample the view draws. It runs on the event loop.
func (v *view) setHardware(hw hostHardware) {
	v.hw = hw
}

// hostCPUText is the CPU use over the host's threads, or a dash until two
// samples a window apart make a percentage possible.
func hostCPUText(hw hostHardware) string {
	if hw.Threads <= 0 {
		return "—"
	}

	percent := "—"
	if hw.HasCPU {
		percent = fmt.Sprintf("%d%%", hw.CPUPercent)
	}

	return fmt.Sprintf("%s of %d threads", percent, hw.Threads)
}

// hostCPUPercentText is the percentage alone, for the narrow resources column.
func hostCPUPercentText(hw hostHardware) string {
	if !hw.HasCPU {
		return "—"
	}

	return fmt.Sprintf("%d%%", hw.CPUPercent)
}

// hostMemText is the host memory used over total, or a dash.
func hostMemText(hw hostHardware) string {
	if !hw.HasMem {
		return "—"
	}

	return shortBytes(hw.MemUsed) + " / " + shortBytes(hw.MemTotal)
}

// hostDiskText is the root filesystem used over total, or a dash.
func hostDiskText(hw hostHardware) string {
	if !hw.HasDisk {
		return "—"
	}

	return shortBytes(hw.DiskUsed) + " / " + shortBytes(hw.DiskTotal)
}

// hostTempText is the CPU temperature, or a dash when the host has no usable
// sensor.
func hostTempText(hw hostHardware) string {
	if !hw.HasTemp || !plausibleTemp(hw.TempC) {
		return "—"
	}

	return fmt.Sprintf("%.0f°C", hw.TempC)
}
