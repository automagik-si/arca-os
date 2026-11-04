package console

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

// phoneKey is the instance configuration key that marks an instance as a phone.
const phoneKey = "user.droid.image"

// incusTimeout bounds each request to Incus, so a daemon that hangs leaves the
// table stale instead of stopping its updates.
const incusTimeout = 10 * time.Second

// instancesResult is one read of the instance list: the table's rows and the
// details panel's allowlisted data.
type instancesResult struct {
	Rows    []Instance
	Details []InstanceDetails
}

// cpuSample is one instance's CPU time at a wall time, for the next sample's delta.
type cpuSample struct {
	usage int64
	at    time.Time
}

// incusReader reads the instance list from Incus's unix socket, as the REST API
// serves it. It keeps its connection between reads and reconnects after a failure,
// and it keeps each instance's last CPU sample.
type incusReader struct {
	sock    string // "" is Incus's own socket
	c       incusclient.InstanceServer
	samples map[string]cpuSample
}

// read returns every instance of every project, sorted by name, then project.
func (r *incusReader) read() (instancesResult, error) {
	if r.c == nil {
		c, err := incusclient.ConnectIncusUnix(r.sock, &incusclient.ConnectionArgs{HTTPClient: &http.Client{Timeout: incusTimeout}})
		if err != nil {
			return instancesResult{}, err
		}

		r.c = c
	}

	start := time.Now()
	threads, hostMem := hostThreads(), hostMemoryBytes()

	full, err := r.c.GetInstancesFullAllProjects(api.InstanceTypeAny)
	if err != nil {
		r.c.Disconnect()
		r.c = nil

		return instancesResult{}, err
	}

	if r.samples == nil {
		r.samples = map[string]cpuSample{}
	}

	rows := make([]Instance, 0, len(full))
	details := make([]InstanceDetails, 0, len(full))

	for _, inst := range full {
		pct := r.cpuPercent(inst, start, threads)

		rows = append(rows, toInstance(inst, pct, hostMem))
		details = append(details, toDetails(inst, hostMem))
	}

	sortRows(rows)
	slices.SortFunc(details, func(a, b InstanceDetails) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}

		return strings.Compare(a.Project, b.Project)
	})

	return instancesResult{Rows: rows, Details: details}, nil
}

// cpuPercent returns the share of the host's threads the instance used since the
// last read, or -1 until its second sample or while it is not running.
func (r *incusReader) cpuPercent(inst api.InstanceFull, now time.Time, threads int) float64 {
	key := inst.Project + "/" + inst.Name

	if inst.State == nil || inst.Status != "Running" {
		delete(r.samples, key)

		return -1
	}

	usage := inst.State.CPU.Usage
	prev, ok := r.samples[key]
	r.samples[key] = cpuSample{usage: usage, at: now}

	elapsed := now.Sub(prev.at)
	if !ok || elapsed <= 0 || usage < prev.usage {
		return -1
	}

	return float64(usage-prev.usage) / float64(elapsed) / float64(threads) * 100
}

func sortRows(rows []Instance) {
	slices.SortFunc(rows, func(a, b Instance) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}

		return strings.Compare(a.Project, b.Project)
	})
}

// toInstance maps one Incus instance to a row: PHONE when the phone key is set,
// else CT or VM, the first global IPv4 address it reports, and its use against
// its own limits.
func toInstance(inst api.InstanceFull, cpuPercent float64, hostMem int64) Instance {
	kind := "CT"
	if inst.Type == string(api.InstanceTypeVM) {
		kind = "VM"
	}

	if _, ok := inst.ExpandedConfig[phoneKey]; ok {
		kind = "PHONE"
	}

	row := Instance{
		Name:       inst.Name,
		Project:    inst.Project,
		Type:       kind,
		Status:     inst.Status,
		IPv4:       firstIPv4(inst.State),
		CPUPercent: cpuPercent,
		CPULimit:   cpuLimit(inst.ExpandedConfig),
		MemLimit:   memoryLimit(inst.ExpandedConfig, hostMem),
	}

	if inst.State != nil {
		row.MemUsage = inst.State.Memory.Usage
		row.Processes = int(inst.State.Processes)

		if d, ok := inst.State.Disk["root"]; ok {
			row.DiskUsage = d.Usage
		}
	}

	if inst.Status == "Running" {
		row.StartedAt = inst.LastUsedAt
	}

	return row
}

// toDetails copies the details panel's allowlist out of one instance: name, type,
// project, description, state, processes, CPU time, memory use, peak and limit,
// disk use per device, addresses and traffic per NIC, created and last started.
// Nothing else of the instance is read.
func toDetails(inst api.InstanceFull, hostMem int64) InstanceDetails {
	description := inst.Description
	if description == "" {
		description = inst.ExpandedConfig["image.description"]
	}

	d := InstanceDetails{
		Name:        inst.Name,
		Project:     inst.Project,
		Description: description,
		Status:      inst.Status,
		CreatedAt:   inst.CreatedAt,
		MemLimit:    memoryLimit(inst.ExpandedConfig, hostMem),
	}

	if inst.Type == string(api.InstanceTypeVM) {
		d.Type = "VM"
	} else {
		d.Type = "CT"

		if _, ok := inst.ExpandedConfig[phoneKey]; ok {
			d.Type = "PHONE"
		}
	}

	if inst.Status == "Running" {
		d.StartedAt = inst.LastUsedAt
	}

	if inst.State == nil {
		return d
	}

	d.Processes = int(inst.State.Processes)
	d.CPUTime = time.Duration(inst.State.CPU.Usage)
	d.MemUsage, d.MemPeak = inst.State.Memory.Usage, inst.State.Memory.UsagePeak

	for name, disk := range inst.State.Disk {
		d.Disks = append(d.Disks, DiskUse{Name: name, Usage: disk.Usage})
	}

	slices.SortFunc(d.Disks, func(a, b DiskUse) int { return strings.Compare(a.Name, b.Name) })

	for name, nic := range inst.State.Network {
		if name == "lo" {
			continue
		}

		item := NICUse{Name: name, Rx: nic.Counters.BytesReceived, Tx: nic.Counters.BytesSent}

		for _, a := range nic.Addresses {
			item.Addresses = append(item.Addresses, a.Address)
		}

		d.NICs = append(d.NICs, item)
	}

	slices.SortFunc(d.NICs, func(a, b NICUse) int { return strings.Compare(a.Name, b.Name) })

	return d
}

// firstIPv4 is the first global IPv4 address over the interfaces in name order, or
// "" when there is none, as for a stopped instance or a VM without its agent.
func firstIPv4(st *api.InstanceState) string {
	if st == nil {
		return ""
	}

	names := make([]string, 0, len(st.Network))
	for name := range st.Network {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		if name == "lo" {
			continue
		}

		for _, a := range st.Network[name].Addresses {
			if a.Family == "inet" && a.Scope == "global" {
				return a.Address
			}
		}
	}

	return ""
}

// cpuLimit returns limits.cpu as a thread count, or 0 when the instance has none.
func cpuLimit(cfg map[string]string) float64 {
	v := strings.TrimSpace(cfg["limits.cpu"])
	if v == "" {
		return 0
	}

	// "2" is two threads; "0-3" and "0,2" are as many as they name.
	if a, b, ok := strings.Cut(v, "-"); ok {
		lo, err1 := strconv.Atoi(strings.TrimSpace(a))

		hi, err2 := strconv.Atoi(strings.TrimSpace(b))
		if err1 != nil || err2 != nil || hi < lo {
			return 0
		}

		return float64(hi - lo + 1)
	}

	if strings.Contains(v, ",") {
		return float64(len(strings.Split(v, ",")))
	}

	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		return 0
	}

	return n
}

// memoryLimit returns limits.memory in bytes, 0 when the instance has none, and
// a percentage of hostTotal for a percentage limit.
func memoryLimit(cfg map[string]string, hostTotal int64) int64 {
	v := strings.TrimSpace(cfg["limits.memory"])
	if v == "" {
		return 0
	}

	if pct, ok := strings.CutSuffix(v, "%"); ok {
		n, err := strconv.ParseFloat(strings.TrimSpace(pct), 64)
		if err != nil || n <= 0 {
			return 0
		}

		return int64(float64(hostTotal) * n / 100)
	}

	return parseSize(v)
}

// parseSize parses an Incus size such as "1GiB", "512MiB", "1.5GB" or "1048576"
// into bytes; 0 when it does not parse.
func parseSize(v string) int64 {
	units := []struct {
		suffix string
		scale  float64
	}{
		{"kib", 1 << 10},
		{"mib", 1 << 20},
		{"gib", 1 << 30},
		{"tib", 1 << 40},
		{"kb", 1e3},
		{"mb", 1e6},
		{"gb", 1e9},
		{"tb", 1e12},
		{"k", 1 << 10},
		{"m", 1 << 20},
		{"g", 1 << 30},
		{"t", 1 << 40},
	}

	lower := strings.ToLower(strings.TrimSpace(v))

	scale := float64(1)

	for _, u := range units {
		if rest, ok := strings.CutSuffix(lower, u.suffix); ok {
			lower = rest
			scale = u.scale

			break
		}
	}

	n, err := strconv.ParseFloat(strings.TrimSpace(lower), 64)
	if err != nil || n <= 0 {
		return 0
	}

	return int64(n * scale)
}
