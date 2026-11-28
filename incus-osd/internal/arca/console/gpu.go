package console

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/internal/applications"
)

// hostGPU is one graphics card of the host. Like the other hardware figures,
// each one is known or unknown on its own and an unknown one shows a dash.
type hostGPU struct {
	Name string
	// Note is the one plain reason the card's figures cannot be read, or empty.
	Note string

	HasUse     bool
	UsePercent int

	HasMem   bool
	MemUsed  int64
	MemTotal int64

	HasTemp bool
	TempC   float64
}

// GPU sampling: the cards are read on their own timer, slower than the other
// figures, and no external tool may take longer than gpuCallBound.
var (
	gpuEvery     = 5 * time.Second
	gpuCallBound = 3 * time.Second
)

// maxGPURows is the most rows the cards take; more cards end in a "+N more" row.
const maxGPURows = 4

// The PCI vendors whose cards the pane lists; a virtual or management display is not a GPU.
const (
	vendorNVIDIA = "10de"
	vendorAMD    = "1002"
	vendorIntel  = "8086"
)

// gpuVendors names each listed vendor for a card the database does not know.
var gpuVendors = map[string]string{vendorNVIDIA: "NVIDIA", vendorAMD: "AMD", vendorIntel: "Intel"}

// The reasons a card's figures cannot be read.
const (
	noteDriverInactive  = "driver not active"
	noteDriverUnchecked = "driver not checked"
	noteDriverStarting  = "driver starting"
	noteDriverSwitching = "driver switching"
	notePassedThrough   = "reserved for a VM"
)

// nvidiaRefusalNote is the reason the NVIDIA application gave for refusing the
// query, or empty for a query that merely failed: that one shows dashes alone.
func nvidiaRefusalNote(err error) string {
	switch {
	case errors.Is(err, applications.ErrNVIDIAUnchecked):
		return noteDriverUnchecked
	case errors.Is(err, applications.ErrNVIDIAStarting):
		return noteDriverStarting
	case errors.Is(err, applications.ErrNVIDIASwitching):
		return noteDriverSwitching
	case errors.Is(err, applications.ErrNVIDIAInactive):
		return noteDriverInactive
	default:
		return ""
	}
}

// gpuCard is one display-class PCI device of a listed vendor.
type gpuCard struct {
	address string
	vendor  string
	device  string
	driver  string
	path    string
}

// onNVIDIADriver reports whether the NVIDIA driver holds the card, so only its tool can read it.
func (c gpuCard) onNVIDIADriver() bool {
	return c.vendor == vendorNVIDIA && c.driver == "nvidia"
}

// listGPUCards returns the host's cards in PCI address order.
func listGPUCards(sysRoot string) []gpuCard {
	root := filepath.Join(sysRoot, "bus", "pci", "devices")

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var cards []gpuCard

	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())

		// Class 0x03 is a display controller; its audio function is another class.
		if !strings.HasPrefix(sysfsID(filepath.Join(path, "class")), "03") {
			continue
		}

		vendor := sysfsID(filepath.Join(path, "vendor"))
		if gpuVendors[vendor] == "" {
			continue
		}

		// A virtual function is a slice of a card, not a card of the host.
		_, err := os.Lstat(filepath.Join(path, "physfn"))
		if err == nil {
			continue
		}

		driver := ""

		link, err := os.Readlink(filepath.Join(path, "driver"))
		if err == nil {
			driver = filepath.Base(link)
		}

		cards = append(cards, gpuCard{
			address: strings.ToLower(entry.Name()),
			vendor:  vendor,
			device:  sysfsID(filepath.Join(path, "device")),
			driver:  driver,
			path:    path,
		})
	}

	return cards
}

// sysfsID reads one hexadecimal sysfs id without its 0x, or "" when it cannot be read.
func sysfsID(path string) string {
	body, err := os.ReadFile(path) //nolint:gosec // a sysfs file under the PCI device
	if err != nil {
		return ""
	}

	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(string(body))), "0x")
}

// readSysfsGPU fills g with the figures a kernel driver shows in sysfs: amdgpu
// has all three, and the others have what their driver offers.
func readSysfsGPU(path string, g *hostGPU) {
	use, err := readSysfsInt(filepath.Join(path, "gpu_busy_percent"))
	if err == nil && use >= 0 && use <= 100 {
		g.HasUse = true
		g.UsePercent = int(use)
	}

	used, usedErr := readSysfsInt(filepath.Join(path, "mem_info_vram_used"))
	total, totalErr := readSysfsInt(filepath.Join(path, "mem_info_vram_total"))

	if usedErr == nil && totalErr == nil && total > 0 && used >= 0 && used <= total {
		g.HasMem = true
		g.MemUsed = used
		g.MemTotal = total
	}

	sensors, err := filepath.Glob(filepath.Join(path, "hwmon", "hwmon*", "temp1_input"))
	if err != nil {
		return
	}

	for _, sensor := range sensors {
		temp, ok := readMillidegrees(sensor)
		if ok && plausibleTemp(temp) {
			g.HasTemp = true
			g.TempC = temp

			return
		}
	}
}

// readSysfsInt reads one decimal sysfs file.
func readSysfsInt(path string) (int64, error) {
	body, err := os.ReadFile(path) //nolint:gosec // a sysfs file under the PCI device
	if err != nil {
		return 0, err
	}

	return strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
}

// nvidiaSMIArgs asks the driver's own tool for every card's figures as one CSV line each.
var nvidiaSMIArgs = []string{
	"--query-gpu=index,pci.bus_id,name,utilization.gpu,memory.used,memory.total,temperature.gpu",
	"--format=csv,noheader,nounits",
}

// parseNvidiaSMI reads nvidiaSMIArgs's output into cards by PCI address. A field
// the tool could not give, such as "[N/A]", leaves that figure unknown.
func parseNvidiaSMI(out string) map[string]hostGPU {
	cards := map[string]hostGPU{}

	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 7 {
			continue
		}

		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}

		address := nvidiaAddress(fields[1])
		if address == "" {
			continue
		}

		// The four figures are the last fields, so a name with a comma stays whole.
		figures := fields[len(fields)-4:]
		g := hostGPU{Name: shortNvidiaName(strings.Join(fields[2:len(fields)-4], ", "))}

		use, err := strconv.ParseFloat(figures[0], 64)
		if err == nil && use >= 0 && use <= 100 {
			g.HasUse = true
			g.UsePercent = int(use + 0.5)
		}

		used, usedErr := strconv.ParseFloat(figures[1], 64)
		total, totalErr := strconv.ParseFloat(figures[2], 64)

		if usedErr == nil && totalErr == nil && total > 0 && used >= 0 && used <= total {
			g.HasMem = true
			g.MemUsed = int64(used * (1 << 20))
			g.MemTotal = int64(total * (1 << 20))
		}

		temp, err := strconv.ParseFloat(figures[3], 64)
		if err == nil && plausibleTemp(temp) {
			g.HasTemp = true
			g.TempC = temp
		}

		cards[address] = g
	}

	return cards
}

// nvidiaAddress turns the tool's "00000000:2B:00.0" into sysfs's "0000:2b:00.0", or "" for anything else.
func nvidiaAddress(id string) string {
	id = strings.ToLower(id)
	if len(id) == 16 && strings.HasPrefix(id, "0000") {
		id = id[4:]
	}

	if len(id) != 12 || id[4] != ':' || id[7] != ':' || id[10] != '.' {
		return ""
	}

	return id
}

// shortNvidiaName drops the maker and the family from the driver's card name.
func shortNvidiaName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "NVIDIA ")

	return strings.TrimPrefix(name, "GeForce ")
}

// udevModel returns the PCI database's model name from udevadm's properties.
func udevModel(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		model, ok := strings.CutPrefix(line, "ID_MODEL_FROM_DATABASE=")
		if ok {
			return strings.TrimSpace(model)
		}
	}

	return ""
}

// shortGPUName is the name the pane shows: the PCI database's marketing name,
// which it keeps in square brackets, or the maker and the device id without one.
func shortGPUName(vendor, device, model string) string {
	maker := gpuVendors[vendor]

	if model == "" {
		return maker + " " + device
	}

	start, end := strings.LastIndex(model, "["), strings.LastIndex(model, "]")
	if start < 0 || end <= start+1 {
		return maker + " " + model
	}

	return strings.TrimPrefix(strings.TrimSpace(model[start+1:end]), "GeForce ")
}

// errToolBusy means the tool's last call has not ended, so it was not started again.
var errToolBusy = errors.New("the tool is still running")

// boundedCall runs one external tool with a deadline. A call that outlives its
// deadline keeps the slot until it ends, so a hung tool is never started twice.
type boundedCall struct {
	busy atomic.Bool
}

func (b *boundedCall) run(ctx context.Context, bound time.Duration, tool func(context.Context) (string, error)) (string, error) {
	if !b.busy.CompareAndSwap(false, true) {
		return "", errToolBusy
	}

	type result struct {
		out string
		err error
	}

	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	done := make(chan result, 1)

	go func() {
		var r result

		// Only the call that took the slot frees it, once, before its answer is handed back.
		defer func() {
			// A panic here would end the daemon, not only the console: it becomes a failed read.
			p := recover()
			if p != nil {
				r = result{err: fmt.Errorf("tool: %v", p)}
			}

			b.busy.Store(false)

			done <- r
		}()

		r.out, r.err = tool(ctx)
	}()

	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		// An answer that is already there wins over the bound.
		select {
		case r := <-done:
			return r.out, r.err
		default:
			return "", ctx.Err()
		}
	}
}

// gpuReader reads the host's cards. NVIDIA cards on the NVIDIA driver are read
// with the driver's own tool, the others from sysfs; names come from udev's PCI
// database, once per card.
type gpuReader struct {
	sysRoot string
	// nvidia runs the driver's nvidia-smi; nil when the host has no way to run it.
	nvidia func(ctx context.Context, args ...string) (string, error)
	// udev prints a PCI device's udev properties.
	udev func(ctx context.Context, path string) (string, error)

	smiCall  boundedCall
	udevCall boundedCall
	models   map[string]string
}

// newGPUReader reads the running system; nvidia is the NVIDIA application's query.
func newGPUReader(nvidia func(ctx context.Context, args ...string) (string, error)) *gpuReader {
	return &gpuReader{
		sysRoot: sysRoot,
		nvidia:  nvidia,
		udev: func(ctx context.Context, path string) (string, error) {
			return subprocess.RunCommandContext(ctx, "udevadm", "info", "--query=property", "--path="+path)
		},
	}
}

// read returns one entry per card, in PCI address order.
func (r *gpuReader) read(ctx context.Context) []hostGPU {
	cards := listGPUCards(r.sysRoot)
	if len(cards) == 0 {
		return nil
	}

	var (
		driven  map[string]hostGPU
		refused string
	)

	// One call answers for every card on the NVIDIA driver; without such a card the tool never runs.
	if r.nvidia != nil && slices.ContainsFunc(cards, gpuCard.onNVIDIADriver) {
		out, err := r.smiCall.run(ctx, gpuCallBound, func(ctx context.Context) (string, error) {
			return r.nvidia(ctx, nvidiaSMIArgs...)
		})
		if err == nil {
			driven = parseNvidiaSMI(out)
		} else {
			refused = nvidiaRefusalNote(err)
		}
	}

	gpus := make([]hostGPU, 0, len(cards))

	for _, card := range cards {
		g := hostGPU{Name: shortGPUName(card.vendor, card.device, r.model(ctx, card))}

		switch {
		case card.driver == "vfio-pci":
			g.Note = notePassedThrough
		case card.onNVIDIADriver():
			answer, ok := driven[card.address]
			if ok {
				if answer.Name == "" {
					answer.Name = g.Name
				}

				g = answer
			} else {
				g.Note = refused
			}
		default:
			if card.vendor == vendorNVIDIA {
				g.Note = noteDriverInactive
			}

			readSysfsGPU(card.path, &g)
		}

		gpus = append(gpus, g)
	}

	return gpus
}

// model returns the PCI database's name of a card, asking udev once per card.
func (r *gpuReader) model(ctx context.Context, card gpuCard) string {
	key := card.address + " " + card.vendor + ":" + card.device

	model, ok := r.models[key]
	if ok {
		return model
	}

	out, err := r.udevCall.run(ctx, gpuCallBound, func(ctx context.Context) (string, error) {
		return r.udev(ctx, card.path)
	})
	if err != nil {
		return ""
	}

	if r.models == nil {
		r.models = map[string]string{}
	}

	r.models[key] = udevModel(out)

	return r.models[key]
}

// readGPUs reads the cards off the event loop and hands them to the view.
func (c *console) readGPUs(ctx context.Context) {
	if c.src.gpus == nil {
		return
	}

	gpus := c.src.gpus(ctx)

	c.h.QueueArcaUpdate(func() {
		defer func() {
			if r := recover(); r != nil {
				c.fail(fmt.Errorf("gpu: %v", r))
			}
		}()

		c.view.setGPUs(gpus)
	})
}

// setGPUs replaces the cards the view draws. It runs on the event loop.
func (v *view) setGPUs(gpus []hostGPU) {
	v.gpus = gpus
}

// gpuRows is how many rows n cards take in the pane.
func gpuRows(n int) int {
	return min(max(n, 0), maxGPURows)
}

// gpuShown is how many of n cards are drawn, and how many the "+N more" row counts.
func gpuShown(n int) (int, int) {
	if n <= maxGPURows {
		return max(n, 0), 0
	}

	return maxGPURows - 1, n - (maxGPURows - 1)
}

// gpuUseText is the card's use, or a dash.
func gpuUseText(g hostGPU) string {
	if !g.HasUse {
		return "—"
	}

	return fmt.Sprintf("%d%%", g.UsePercent)
}

// gpuMemText is the card's memory used over total, or a dash.
func gpuMemText(g hostGPU) string {
	if !g.HasMem {
		return "—"
	}

	return shortBytes(g.MemUsed) + " / " + shortBytes(g.MemTotal)
}

// gpuTempText is the card's temperature, or a dash.
func gpuTempText(g hostGPU) string {
	if !g.HasTemp || !plausibleTemp(g.TempC) {
		return "—"
	}

	return fmt.Sprintf("%.0f°C", g.TempC)
}

// gpuLines returns the pane's rows for the cards: one per card, the name cut so
// the figures stay within w cells, and a last "+N more" row past the bound.
func gpuLines(gpus []hostGPU, w int) []string {
	shown, more := gpuShown(len(gpus))
	lines := make([]string, 0, shown+1)

	for i, g := range gpus[:shown] {
		label := fmt.Sprintf("GPU%d", i)
		figures := " · " + gpuUseText(g) + " · " + gpuMemText(g) + " · " + gpuTempText(g)

		if g.Note != "" {
			figures += " · " + tagDim + g.Note + tagReset
		}

		room := w - 1 - width(label) - 1 - tview.TaggedStringWidth(figures)
		lines = append(lines, " "+tagDim+label+tagReset+" "+safe(fit(printable(g.Name), max(room, 1)))+figures)
	}

	if more > 0 {
		lines = append(lines, fmt.Sprintf(" %s+%d more%s", tagDim, more, tagReset))
	}

	return lines
}

// gpuShortLines returns the resources column's rows: the use and the temperature of each card.
func gpuShortLines(gpus []hostGPU) []string {
	shown, more := gpuShown(len(gpus))
	lines := make([]string, 0, shown+1)

	for i, g := range gpus[:shown] {
		lines = append(lines, fmt.Sprintf(" %sGPU%d%s %s · %s", tagDim, i, tagReset, gpuUseText(g), gpuTempText(g)))
	}

	if more > 0 {
		lines = append(lines, fmt.Sprintf(" %s+%d more%s", tagDim, more, tagReset))
	}

	return lines
}

// drawGPULines prints the cards' rows from row y down, never past row end.
func drawGPULines(screen tcell.Screen, lines []string, x, y, w, end int) {
	for i, line := range lines {
		if y+i >= end {
			return
		}

		tview.Print(screen, line, x, y+i, w, tview.AlignLeft, tcell.ColorWhite)
	}
}
