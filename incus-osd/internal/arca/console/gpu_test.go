package console

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxc/incus-os/incus-osd/internal/applications"
)

// testGPUs are two cards as the golden screens draw them.
var testGPUs = []hostGPU{
	{Name: "RTX 4060 Ti", HasUse: true, UsePercent: 37, HasMem: true, MemUsed: 2150 << 20, MemTotal: 16 << 30, HasTemp: true, TempC: 56},
	{Name: "RTX 4060 Ti", HasUse: true, UsePercent: 4, HasMem: true, MemUsed: 512 << 20, MemTotal: 16 << 30, HasTemp: true, TempC: 41},
}

// gpuFixture writes one PCI device under a fixture /sys.
func gpuFixture(t *testing.T, sys, address, class, vendor, device, driver string) string {
	t.Helper()

	dir := filepath.Join("bus", "pci", "devices", address)
	writeFixture(t, sys, filepath.Join(dir, "class"), class+"\n")
	writeFixture(t, sys, filepath.Join(dir, "vendor"), vendor+"\n")
	writeFixture(t, sys, filepath.Join(dir, "device"), device+"\n")

	if driver != "" {
		err := os.Symlink(filepath.Join("..", "..", "drivers", driver), filepath.Join(sys, dir, "driver"))
		if err != nil {
			t.Fatal(err)
		}
	}

	return filepath.Join(sys, dir)
}

// testdata reads one captured output.
func testdata(t *testing.T, name string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", "gpu", name))
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

// TestNvidiaSMIRealOutput parses what driver 620.70.06 printed on a host with two cards.
func TestNvidiaSMIRealOutput(t *testing.T) {
	t.Parallel()

	rows := parseNvidiaSMI(testdata(t, "nvidia-smi-two-cards.csv"))
	if len(rows) != 2 {
		t.Fatalf("parsed %d cards, want 2: %+v", len(rows), rows)
	}

	first, ok := rows["0000:04:00.0"]
	if !ok {
		t.Fatalf("no card at 0000:04:00.0: %+v", rows)
	}

	want := hostGPU{Name: "RTX 4060 Ti", HasUse: true, UsePercent: 0, HasMem: true, MemUsed: 11092 << 20, MemTotal: 16380 << 20, HasTemp: true, TempC: 40.52}
	if first != want {
		t.Errorf("first card = %+v, want %+v", first, want)
	}

	// The bus address is printed in upper case; sysfs names it in lower case.
	second, ok := rows["0000:2b:00.0"]
	if !ok || second.MemUsed != 11108<<20 || second.TempC != 39.36 {
		t.Errorf("second card = %+v (found %t), want 11108 MiB at 39.36", second, ok)
	}

	if got := gpuTempText(first); got != "41°C" {
		t.Errorf("temperature text = %q, want the rounded 41°C", got)
	}

	if got := gpuMemText(first); got != "10.8G / 16G" {
		t.Errorf("memory text = %q, want 10.8G / 16G", got)
	}
}

func TestNvidiaSMIUnknownFields(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		line                    string
		hasUse, hasMem, hasTemp bool
		use                     int
		comment                 string
	}{
		{line: "0, 00000000:01:00.0, NVIDIA GeForce RTX 4060 Ti, 37, 2150, 16380, 56", hasUse: true, hasMem: true, hasTemp: true, use: 37, comment: "all known"},
		{line: "0, 00000000:01:00.0, NVIDIA GeForce RTX 4060 Ti, [N/A], 2150, 16380, 56", hasMem: true, hasTemp: true, comment: "use not supported"},
		{line: "0, 00000000:01:00.0, NVIDIA GeForce RTX 4060 Ti, 37, [N/A], 16380, 56", hasUse: true, hasTemp: true, use: 37, comment: "used memory unknown"},
		{line: "0, 00000000:01:00.0, NVIDIA GeForce RTX 4060 Ti, 37, 2150, [N/A], 56", hasUse: true, hasTemp: true, use: 37, comment: "total memory unknown"},
		{line: "0, 00000000:01:00.0, NVIDIA GeForce RTX 4060 Ti, 37, 2150, 16380, [N/A]", hasUse: true, hasMem: true, use: 37, comment: "temperature unknown"},
		{line: "0, 00000000:01:00.0, NVIDIA GeForce RTX 4060 Ti, 250, 20000, 16380, 900", comment: "implausible figures"},
		{line: "0, 00000000:01:00.0, NVIDIA GeForce RTX 4060 Ti, -1, -5, 0, 0", comment: "negative and zero figures"},
		{line: "0, 00000000:01:00.0, NVIDIA GeForce RTX 4060 Ti, [Unknown Error], [Unknown Error], [Unknown Error], [Unknown Error]", comment: "a card that fell off the bus"},
	} {
		rows := parseNvidiaSMI(row.line + "\n")

		got, ok := rows["0000:01:00.0"]
		if !ok {
			t.Errorf("%s: the card was dropped", row.comment)

			continue
		}

		if got.HasUse != row.hasUse || got.HasMem != row.hasMem || got.HasTemp != row.hasTemp || got.UsePercent != row.use {
			t.Errorf("%s: got %+v", row.comment, got)
		}

		if got.Name != "RTX 4060 Ti" {
			t.Errorf("%s: name = %q", row.comment, got.Name)
		}
	}

	// A line that is not a card is no card, and never a card of zeros.
	for _, body := range []string{"", "\n", "No devices were found\n", "Unable to determine the device handle for GPU0000:01:00.0: Unknown Error\n", "1, 2, 3\n"} {
		if rows := parseNvidiaSMI(body); len(rows) != 0 {
			t.Errorf("%q parsed as %+v", body, rows)
		}
	}
}

// TestSysfsGPURealValues reads what an amdgpu card showed in sysfs on a real host.
func TestSysfsGPURealValues(t *testing.T) {
	t.Parallel()

	sys := t.TempDir()
	dir := gpuFixture(t, sys, "0000:c6:00.0", "0x038000", "0x1002", "0x150e", "amdgpu")
	writeFixture(t, dir, "gpu_busy_percent", "0\n")
	writeFixture(t, dir, "mem_info_vram_used", "1205350400\n")
	writeFixture(t, dir, "mem_info_vram_total", "2147483648\n")
	writeFixture(t, dir, "hwmon/hwmon9/name", "amdgpu\n")
	writeFixture(t, dir, "hwmon/hwmon9/temp1_input", "44000\n")

	var g hostGPU

	readSysfsGPU(dir, &g)

	want := hostGPU{HasUse: true, UsePercent: 0, HasMem: true, MemUsed: 1205350400, MemTotal: 2147483648, HasTemp: true, TempC: 44}
	if g != want {
		t.Errorf("sysfs card = %+v, want %+v", g, want)
	}

	if got := gpuMemText(g); got != "1.1G / 2G" {
		t.Errorf("memory text = %q, want 1.1G / 2G", got)
	}
}

func TestSysfsGPUUnknownRule(t *testing.T) {
	t.Parallel()

	// A card with no figure file at all, like an Intel one, is all unknown.
	sys := t.TempDir()
	dir := gpuFixture(t, sys, "0000:00:02.0", "0x030000", "0x8086", "0x46a6", "i915")

	var g hostGPU

	readSysfsGPU(dir, &g)

	if g.HasUse || g.HasMem || g.HasTemp {
		t.Errorf("a card without figures read as %+v", g)
	}

	// Each figure fails on its own.
	for _, row := range []struct {
		file, body              string
		hasUse, hasMem, hasTemp bool
	}{
		{"gpu_busy_percent", "busy\n", false, true, true},
		{"gpu_busy_percent", "101\n", false, true, true},
		{"mem_info_vram_total", "0\n", true, false, true},
		{"mem_info_vram_used", "9999999999\n", true, false, true},
		{"mem_info_vram_used", "-1\n", true, false, true},
		{"hwmon/hwmon0/temp1_input", "0\n", true, true, false},
		{"hwmon/hwmon0/temp1_input", "2000000\n", true, true, false},
	} {
		sys := t.TempDir()
		dir := gpuFixture(t, sys, "0000:03:00.0", "0x030000", "0x1002", "0x73bf", "amdgpu")
		writeFixture(t, dir, "gpu_busy_percent", "37\n")
		writeFixture(t, dir, "mem_info_vram_used", "1073741824\n")
		writeFixture(t, dir, "mem_info_vram_total", "8589934592\n")
		writeFixture(t, dir, "hwmon/hwmon0/temp1_input", "56000\n")
		writeFixture(t, dir, row.file, row.body)

		var g hostGPU

		readSysfsGPU(dir, &g)

		if g.HasUse != row.hasUse || g.HasMem != row.hasMem || g.HasTemp != row.hasTemp {
			t.Errorf("%s=%q read as %+v", row.file, strings.TrimSpace(row.body), g)
		}
	}
}

func TestListGPUCards(t *testing.T) {
	t.Parallel()

	sys := t.TempDir()
	gpuFixture(t, sys, "0000:04:00.0", "0x030000", "0x10de", "0x2805", "nvidia")
	gpuFixture(t, sys, "0000:04:00.1", "0x040300", "0x10de", "0x22bd", "snd_hda_intel")
	gpuFixture(t, sys, "0000:2b:00.0", "0x030200", "0x10de", "0x2805", "")
	gpuFixture(t, sys, "0000:c6:00.0", "0x038000", "0x1002", "0x150e", "amdgpu")
	gpuFixture(t, sys, "0000:00:01.0", "0x030000", "0x1af4", "0x1050", "virtio-pci")
	gpuFixture(t, sys, "0000:05:00.0", "0x030000", "0x1a03", "0x2000", "ast")
	gpuFixture(t, sys, "0000:00:1f.6", "0x020000", "0x8086", "0x15bb", "e1000e")

	// A virtual function of a card is not a card of the host.
	vf := gpuFixture(t, sys, "0000:00:02.1", "0x030000", "0x8086", "0x46a6", "")
	writeFixture(t, vf, "physfn/class", "0x030000\n")

	cards := listGPUCards(sys)

	got := make([]string, 0, len(cards))
	for _, card := range cards {
		got = append(got, card.address+" "+card.vendor+":"+card.device+" "+card.driver)
	}

	want := []string{"0000:04:00.0 10de:2805 nvidia", "0000:2b:00.0 10de:2805 ", "0000:c6:00.0 1002:150e amdgpu"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("cards = %q, want %q", got, want)
	}

	if cards := listGPUCards(t.TempDir()); len(cards) != 0 {
		t.Errorf("a host without PCI devices has cards: %+v", cards)
	}
}

func TestGPUNames(t *testing.T) {
	t.Parallel()

	// The model udev printed for a real AMD card.
	if got := udevModel(testdata(t, "udevadm-amd.txt")); got != "Strix [Radeon 880M / 890M]" {
		t.Errorf("udev model = %q", got)
	}

	if got := udevModel("DRIVER=nvidia\nPCI_ID=10DE:2805\n"); got != "" {
		t.Errorf("a device without a database name has the model %q", got)
	}

	for _, row := range []struct {
		vendor, device, model, want string
	}{
		{"1002", "150e", "Strix [Radeon 880M / 890M]", "Radeon 880M / 890M"},
		{"10de", "2805", "AD106 [GeForce RTX 4060 Ti 16GB]", "RTX 4060 Ti 16GB"},
		{"10de", "1eb8", "TU104GL [Tesla T4]", "Tesla T4"},
		{"8086", "46a6", "Alder Lake-P GT2 [Iris Xe Graphics]", "Iris Xe Graphics"},
		{"8086", "a780", "Raptor Lake-S GT1 [UHD Graphics 770]", "UHD Graphics 770"},
		{"8086", "4c8a", "RocketLake-S GT1", "Intel RocketLake-S GT1"},
		{"10de", "2805", "", "NVIDIA 2805"},
		{"1002", "150e", "", "AMD 150e"},
		{"8086", "46a6", "", "Intel 46a6"},
	} {
		if got := shortGPUName(row.vendor, row.device, row.model); got != row.want {
			t.Errorf("shortGPUName(%s, %s, %q) = %q, want %q", row.vendor, row.device, row.model, got, row.want)
		}
	}

	for name, want := range map[string]string{
		"NVIDIA GeForce RTX 4060 Ti": "RTX 4060 Ti",
		"NVIDIA RTX A4000":           "RTX A4000",
		"Tesla T4":                   "Tesla T4",
		"":                           "",
	} {
		if got := shortNvidiaName(name); got != want {
			t.Errorf("shortNvidiaName(%q) = %q, want %q", name, got, want)
		}
	}
}

// fakeTools are the reader's two external tools, counted.
type fakeTools struct {
	smi      string
	smiErr   error
	smiCalls atomic.Int32
	udev     map[string]string
	udevErr  error
	udevHits atomic.Int32
}

func (f *fakeTools) reader(sys string) *gpuReader {
	return &gpuReader{
		sysRoot: sys,
		nvidia: func(context.Context, ...string) (string, error) {
			f.smiCalls.Add(1)

			return f.smi, f.smiErr
		},
		udev: func(_ context.Context, path string) (string, error) {
			f.udevHits.Add(1)

			if f.udevErr != nil {
				return "", f.udevErr
			}

			return "ID_MODEL_FROM_DATABASE=" + f.udev[filepath.Base(path)] + "\n", nil
		},
	}
}

func TestGPUReader(t *testing.T) {
	t.Parallel()

	sys := t.TempDir()
	gpuFixture(t, sys, "0000:04:00.0", "0x030000", "0x10de", "0x2805", "nvidia")
	gpuFixture(t, sys, "0000:2b:00.0", "0x030000", "0x10de", "0x2805", "nvidia")

	amd := gpuFixture(t, sys, "0000:c6:00.0", "0x038000", "0x1002", "0x150e", "amdgpu")
	writeFixture(t, amd, "gpu_busy_percent", "9\n")
	writeFixture(t, amd, "hwmon/hwmon9/temp1_input", "44000\n")

	tools := &fakeTools{
		smi: testdata(t, "nvidia-smi-two-cards.csv"),
		udev: map[string]string{
			"0000:04:00.0": "AD106 [GeForce RTX 4060 Ti 16GB]",
			"0000:2b:00.0": "AD106 [GeForce RTX 4060 Ti 16GB]",
			"0000:c6:00.0": "Strix [Radeon 880M / 890M]",
		},
	}
	r := tools.reader(sys)

	gpus := r.read(context.Background())
	if len(gpus) != 3 {
		t.Fatalf("read %d cards, want 3: %+v", len(gpus), gpus)
	}

	// The driver's own name wins over the database's.
	if gpus[0].Name != "RTX 4060 Ti" || !gpus[0].HasMem || gpus[0].MemUsed != 11092<<20 || gpus[0].Note != "" {
		t.Errorf("first NVIDIA card = %+v", gpus[0])
	}

	if gpus[1].TempC != 39.36 {
		t.Errorf("second NVIDIA card = %+v", gpus[1])
	}

	// The AMD card comes from sysfs: use and temperature known, memory unknown.
	if gpus[2].Name != "Radeon 880M / 890M" || !gpus[2].HasUse || gpus[2].UsePercent != 9 || gpus[2].HasMem || !gpus[2].HasTemp {
		t.Errorf("AMD card = %+v", gpus[2])
	}

	// One tool call for all NVIDIA cards, and names are read once.
	_ = r.read(context.Background())

	if tools.smiCalls.Load() != 2 || tools.udevHits.Load() != 3 {
		t.Errorf("two reads made %d nvidia-smi and %d udevadm calls, want 2 and 3", tools.smiCalls.Load(), tools.udevHits.Load())
	}

	// A failed tool leaves the cards by name with dashes, and no reason.
	tools.smiErr = errors.New("nvidia-smi failed")

	gpus = r.read(context.Background())
	if gpus[0].Name != "RTX 4060 Ti 16GB" || gpus[0].HasUse || gpus[0].HasMem || gpus[0].HasTemp || gpus[0].Note != "" {
		t.Errorf("a failed nvidia-smi read as %+v", gpus[0])
	}

	if !gpus[2].HasUse {
		t.Errorf("a failed nvidia-smi dropped the AMD card's figures: %+v", gpus[2])
	}

	// A card nvidia-smi does not list keeps its dashes.
	tools.smiErr, tools.smi = nil, "0, 00000000:04:00.0, NVIDIA GeForce RTX 4060 Ti, 5, 100, 16380, 40\n"

	gpus = r.read(context.Background())
	if !gpus[0].HasUse || gpus[1].HasUse || gpus[1].HasMem || gpus[1].HasTemp {
		t.Errorf("a card missing from nvidia-smi read as %+v", gpus[1])
	}
}

func TestGPUReaderDriverNotActive(t *testing.T) {
	t.Parallel()

	sys := t.TempDir()
	gpuFixture(t, sys, "0000:04:00.0", "0x030000", "0x10de", "0x2805", "")
	gpuFixture(t, sys, "0000:2b:00.0", "0x030000", "0x10de", "0x2805", "vfio-pci")

	nouveau := gpuFixture(t, sys, "0000:41:00.0", "0x030000", "0x10de", "0x1eb8", "nouveau")
	writeFixture(t, nouveau, "hwmon/hwmon3/temp1_input", "38000\n")

	tools := &fakeTools{smi: testdata(t, "nvidia-smi-two-cards.csv"), udevErr: errors.New("no udevadm")}

	gpus := tools.reader(sys).read(context.Background())
	if len(gpus) != 3 {
		t.Fatalf("read %d cards, want 3: %+v", len(gpus), gpus)
	}

	// No card is on the NVIDIA driver, so its tool never runs.
	if tools.smiCalls.Load() != 0 {
		t.Errorf("nvidia-smi ran %d times with no card on the NVIDIA driver", tools.smiCalls.Load())
	}

	if gpus[0].Name != "NVIDIA 2805" || gpus[0].Note != "driver not active" || gpus[0].HasUse || gpus[0].HasMem || gpus[0].HasTemp {
		t.Errorf("an NVIDIA card without a driver read as %+v", gpus[0])
	}

	if gpus[1].Note != "reserved for a VM" || gpus[1].HasUse {
		t.Errorf("a passed-through card read as %+v", gpus[1])
	}

	// The in-kernel driver still gives a temperature; the other figures stay unknown.
	if gpus[2].Note != "driver not active" || !gpus[2].HasTemp || gpus[2].TempC != 38 || gpus[2].HasUse || gpus[2].HasMem {
		t.Errorf("an NVIDIA card on the in-kernel driver read as %+v", gpus[2])
	}

	// A reader without the driver's tool shows dashes for a card on the NVIDIA driver.
	sys = t.TempDir()
	gpuFixture(t, sys, "0000:04:00.0", "0x030000", "0x10de", "0x2805", "nvidia")

	r := tools.reader(sys)
	r.nvidia = nil

	gpus = r.read(context.Background())
	if len(gpus) != 1 || gpus[0].HasUse || gpus[0].Note != "" {
		t.Errorf("a reader without nvidia-smi read %+v", gpus)
	}

	// A host without a card reads as no card at all.
	if gpus := tools.reader(t.TempDir()).read(context.Background()); len(gpus) != 0 {
		t.Errorf("a host without a card read %+v", gpus)
	}
}

// TestGPUReaderRefusedQuery checks that a card on the NVIDIA driver whose query
// the application refuses says why, and that a query that merely failed does not.
func TestGPUReaderRefusedQuery(t *testing.T) {
	t.Parallel()

	sys := t.TempDir()
	gpuFixture(t, sys, "0000:04:00.0", "0x030000", "0x10de", "0x2805", "nvidia")
	gpuFixture(t, sys, "0000:2b:00.0", "0x030000", "0x10de", "0x2805", "")

	amd := gpuFixture(t, sys, "0000:c6:00.0", "0x038000", "0x1002", "0x150e", "amdgpu")
	writeFixture(t, amd, "gpu_busy_percent", "9\n")

	for _, row := range []struct {
		err  error
		note string
	}{
		{applications.ErrNVIDIAUnchecked, "driver not checked"},
		{applications.ErrNVIDIAStarting, "driver starting"},
		{applications.ErrNVIDIASwitching, "driver switching"},
		{applications.ErrNVIDIAInactive, "driver not active"},
		{fmt.Errorf("query: %w", applications.ErrNVIDIASwitching), "driver switching"},
		{applications.ErrNVIDIANotReady, ""},
		{errors.New("nvidia-smi failed"), ""},
		{context.DeadlineExceeded, ""},
	} {
		tools := &fakeTools{smiErr: row.err, udevErr: errors.New("no udevadm")}

		gpus := tools.reader(sys).read(context.Background())
		if len(gpus) != 3 {
			t.Fatalf("%v: read %d cards, want 3", row.err, len(gpus))
		}

		if gpus[0].Note != row.note || gpus[0].HasUse || gpus[0].HasMem || gpus[0].HasTemp {
			t.Errorf("%v: the card on the NVIDIA driver read as %+v, want the note %q", row.err, gpus[0], row.note)
		}

		// The other cards keep their own reason and their own figures.
		if gpus[1].Note != "driver not active" || gpus[2].Note != "" || !gpus[2].HasUse {
			t.Errorf("%v: the other cards read as %+v and %+v", row.err, gpus[1], gpus[2])
		}
	}

	// An answer that leaves one card out gives that card dashes and no reason.
	gpuFixture(t, sys, "0000:41:00.0", "0x030000", "0x10de", "0x2805", "nvidia")

	tools := &fakeTools{smi: "0, 00000000:04:00.0, NVIDIA GeForce RTX 4060 Ti, 5, 100, 16380, 40\n", udevErr: errors.New("no udevadm")}

	gpus := tools.reader(sys).read(context.Background())
	if !gpus[0].HasUse || gpus[2].HasUse || gpus[2].Note != "" {
		t.Errorf("a card missing from the answer read as %+v", gpus[2])
	}

	// The reason is drawn after the dashes at both sizes.
	refused := []hostGPU{{Name: "RTX 4060 Ti", Note: "driver switching"}}

	for _, size := range [][2]int{{80, 25}, {120, 36}} {
		if text := drawWithGPUs(t, refused, size[0], size[1]); !strings.Contains(text, "GPU0 RTX 4060 Ti · — · — · — · driver switching") {
			t.Errorf("the %dx%d screen does not say why the card has no figures", size[0], size[1])
		}
	}
}

// TestBoundedCall checks that a hung tool costs one bound, is never started twice and frees its slot when it ends.
func TestBoundedCall(t *testing.T) {
	t.Parallel()

	var (
		call   boundedCall
		starts atomic.Int32
	)

	release := make(chan struct{})
	hung := func(context.Context) (string, error) {
		starts.Add(1)
		<-release

		return "late", nil
	}

	// The call must come back by itself: the test never frees the tool before it does.
	returned := make(chan error, 1)

	go func() {
		_, err := call.run(context.Background(), 30*time.Millisecond, hung)
		returned <- err
	}()

	select {
	case err := <-returned:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a hung tool returned %v, want the deadline", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("a hung tool held the reader past its bound")
	}

	// The tool is still running: the next call does not start it again.
	_, err := call.run(context.Background(), 30*time.Millisecond, hung)
	if !errors.Is(err, errToolBusy) || starts.Load() != 1 {
		t.Fatalf("a second call on a hung tool returned %v after %d starts, want busy after 1", err, starts.Load())
	}

	close(release)

	// Once the hung tool ends, the slot is free again.
	deadline := time.Now().Add(2 * time.Second)

	for {
		out, err := call.run(context.Background(), time.Second, func(context.Context) (string, error) { return "ok", nil })
		if err == nil && out == "ok" {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("the slot stayed taken after the tool ended: %v", err)
		}

		time.Sleep(5 * time.Millisecond)
	}

	// A tool that panics is a failed read, and its slot is free again.
	_, err = call.run(context.Background(), time.Second, func(context.Context) (string, error) { panic("test panic in a tool") })
	if err == nil || !strings.Contains(err.Error(), "test panic in a tool") {
		t.Errorf("a panicking tool returned %v", err)
	}

	// The tool sees the deadline too, so one that honours it is told to stop.
	_, err = call.run(context.Background(), 20*time.Millisecond, func(ctx context.Context) (string, error) {
		<-ctx.Done()

		return "", ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a tool that honours its deadline returned %v", err)
	}
}

// TestGPUReaderHungTool checks the whole read against a tool that never answers.
func TestGPUReaderHungTool(t *testing.T) { //nolint:paralleltest // shortens the package bound
	old := gpuCallBound

	t.Cleanup(func() { gpuCallBound = old })

	gpuCallBound = 30 * time.Millisecond

	sys := t.TempDir()
	gpuFixture(t, sys, "0000:04:00.0", "0x030000", "0x10de", "0x2805", "nvidia")
	gpuFixture(t, sys, "0000:2b:00.0", "0x030000", "0x10de", "0x2805", "nvidia")

	release := make(chan struct{})
	defer close(release)

	var starts atomic.Int32

	r := &gpuReader{
		sysRoot: sys,
		nvidia: func(context.Context, ...string) (string, error) {
			starts.Add(1)
			<-release

			return "", nil
		},
		udev: func(context.Context, string) (string, error) {
			starts.Add(1)
			<-release

			return "", nil
		},
	}

	// The reads must come back by themselves: the tools are only freed when the test ends.
	read := make(chan []hostGPU, 3)

	go func() {
		for range 3 {
			read <- r.read(context.Background())
		}
	}()

	for range 3 {
		select {
		case gpus := <-read:
			if len(gpus) != 2 || gpus[0].Name != "NVIDIA 2805" || gpus[0].HasUse || gpus[0].HasMem || gpus[0].HasTemp {
				t.Fatalf("a hung tool read as %+v", gpus)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a hung tool held the read past its bound")
		}
	}

	if starts.Load() != 2 {
		t.Errorf("hung tools were started %d times, want once each", starts.Load())
	}
}

func TestGPUText(t *testing.T) {
	t.Parallel()

	unknown := hostGPU{Name: "RTX 4060 Ti"}

	if gpuUseText(unknown) != "—" || gpuMemText(unknown) != "—" || gpuTempText(unknown) != "—" {
		t.Errorf("an unknown card reads %q %q %q, want dashes", gpuUseText(unknown), gpuMemText(unknown), gpuTempText(unknown))
	}

	known := testGPUs[0]
	if gpuUseText(known) != "37%" || gpuMemText(known) != "2.1G / 16G" || gpuTempText(known) != "56°C" {
		t.Errorf("a known card reads %q %q %q", gpuUseText(known), gpuMemText(known), gpuTempText(known))
	}

	// An idle card is a known zero, not a dash.
	idle := hostGPU{HasUse: true, HasMem: true, MemTotal: 16 << 30}
	if gpuUseText(idle) != "0%" || gpuMemText(idle) != "0 / 16G" {
		t.Errorf("an idle card reads %q %q", gpuUseText(idle), gpuMemText(idle))
	}

	for _, row := range []struct {
		cards, shown, more int
	}{
		{0, 0, 0}, {1, 1, 0}, {4, 4, 0}, {5, 3, 2}, {8, 3, 5},
	} {
		shown, more := gpuShown(row.cards)
		if shown != row.shown || more != row.more {
			t.Errorf("gpuShown(%d) = %d cards and %d more, want %d and %d", row.cards, shown, more, row.shown, row.more)
		}

		rows := gpuRows(row.cards)
		if rows != min(row.cards, maxGPURows) {
			t.Errorf("gpuRows(%d) = %d", row.cards, rows)
		}
	}
}

// drawWithGPUs draws the tracer snapshot with the golden hardware and the given cards.
func drawWithGPUs(t *testing.T, gpus []hostGPU, w, h int) string {
	t.Helper()

	return screenText(drawWithHardwareAndGPUs(t, tracerSnapshot(), testHardware, gpus, w, h))
}

func TestGPULayoutRows(t *testing.T) {
	t.Parallel()

	// No card: the layout is the one a host without a GPU always had.
	for _, size := range [][2]int{{80, 24}, {80, 25}, {120, 36}, {160, 45}} {
		none := chooseLayout(size[0], size[1], 0)
		if none.Hardware.H != map[layoutKind]int{layoutWide: hardwareWide, layoutCompact: hardwareRows}[none.kind] {
			t.Errorf("%dx%d without a card has a %d-row hardware pane", size[0], size[1], none.Hardware.H)
		}
	}

	// The wide box holds the first card in its spare row and grows by one row per further card.
	for rows, want := range map[int]int{1: hardwareWide, 2: hardwareWide + 1, 4: hardwareWide + 3} {
		l := chooseLayout(120, 36, rows)
		if l.Hardware.H != want {
			t.Errorf("the wide box is %d rows for %d cards, want %d", l.Hardware.H, rows, want)
		}

		if l.Instances.Y != l.Hardware.Y+l.Hardware.H || l.Instances.H < minTable || l.Events.H < minEvents {
			t.Errorf("the wide layout for %d cards is %+v", rows, l)
		}

		if l.Keys.Y != l.H-1 || l.Alert.Y != l.H-2 || l.Events.Y+l.Events.H != l.Alert.Y {
			t.Errorf("the wide layout for %d cards misplaces its last rows: %+v", rows, l)
		}
	}

	// The compact strip grows by one row per card and the smallest screen keeps its table.
	for rows := 1; rows <= maxGPURows; rows++ {
		l := chooseLayout(80, 24, rows)
		if l.Hardware.H != hardwareRows+rows || l.Instances.H < minTable {
			t.Errorf("the compact layout for %d cards is %+v", rows, l)
		}

		if l.Keys.Y != l.H-1 || l.Log.Y != l.H-2 || l.Alert.Y != l.H-3 || l.Summary.Y != l.H-4 {
			t.Errorf("the compact layout for %d cards misplaces its last rows: %+v", rows, l)
		}
	}
}

func TestGPUScreens(t *testing.T) {
	t.Parallel()

	// No card: no GPU text anywhere, at any size.
	for _, size := range [][2]int{{80, 25}, {120, 36}} {
		if text := drawWithGPUs(t, nil, size[0], size[1]); strings.Contains(text, "GPU") {
			t.Errorf("a host without a card shows GPU text at %dx%d", size[0], size[1])
		}
	}

	wide := drawWithHardwareAndGPUs(t, tracerSnapshot(), testHardware, testGPUs, 120, 36)
	text := screenText(wide)

	for _, want := range []string{"GPU0 RTX 4060 Ti · 37% · 2.1G / 16G · 56°C", "GPU1 RTX 4060 Ti · 4% · 512M / 16G · 41°C", "GPU0 37% · 56°C", "GPU1 4% · 41°C"} {
		if !strings.Contains(text, want) {
			t.Errorf("the 120x36 screen has no %q", want)
		}
	}

	// The short lines are in the resources column, under the temperature.
	if line := screenLine(wide, rowOf(wide, "Temp 45°C")+1); !strings.Contains(line, "│ GPU0 37% · 56°C") {
		t.Errorf("the line under the temperature is %q", line)
	}

	compact := drawWithGPUs(t, testGPUs, 80, 25)
	for _, want := range []string{" GPU0 RTX 4060 Ti · 37% · 2.1G / 16G · 56°C", " GPU1 RTX 4060 Ti · 4% · 512M / 16G · 41°C", "Hardware 12% of 8 threads", "─ Instances 4 · 3 running"} {
		if !strings.Contains(compact, want) {
			t.Errorf("the 80x25 screen has no %q", want)
		}
	}

	// Unknown figures are dashes with the reason, never a zero.
	unknown := []hostGPU{{Name: "RTX 4060 Ti 16GB", Note: "driver not active"}, {Name: "Radeon 880M / 890M", HasTemp: true, TempC: 44}}

	for _, size := range [][2]int{{80, 25}, {120, 36}} {
		text := drawWithGPUs(t, unknown, size[0], size[1])
		for _, want := range []string{"GPU0 RTX 4060 Ti 16GB · — · — · — · driver not active", "GPU1 Radeon 880M / 890M · — · — · 44°C"} {
			if !strings.Contains(text, want) {
				t.Errorf("the %dx%d screen has no %q", size[0], size[1], want)
			}
		}

		if strings.Contains(text, "0%") || strings.Contains(text, "0 / 0") {
			t.Errorf("the %dx%d screen drew an unknown figure as a zero", size[0], size[1])
		}
	}

	// More cards than the bound: the last row counts the rest.
	many := make([]hostGPU, 0, 6)
	for range 6 {
		many = append(many, testGPUs[0])
	}

	for _, size := range [][2]int{{80, 24}, {120, 36}} {
		text := drawWithGPUs(t, many, size[0], size[1])
		if !strings.Contains(text, "GPU2 RTX 4060 Ti") || strings.Contains(text, "GPU3") || !strings.Contains(text, "+3 more") {
			t.Errorf("six cards at %dx%d do not end in +3 more:\n%s", size[0], size[1], text)
		}
	}

	// A too-small screen draws no card.
	if strings.Contains(drawWithGPUs(t, testGPUs, 60, 20), "GPU") {
		t.Error("a too-small screen drew a card")
	}
}

func TestGPUHostileName(t *testing.T) {
	t.Parallel()

	hostile := []hostGPU{{Name: "[red]evil\x1b[2J" + strings.Repeat("x", 200), HasUse: true, UsePercent: 5}}

	for _, size := range [][2]int{{80, 25}, {120, 36}} {
		screen := drawWithHardwareAndGPUs(t, tracerSnapshot(), testHardware, hostile, size[0], size[1])
		text := screenText(screen)

		if !strings.Contains(text, "GPU0 [red]evil?[2J") || strings.Contains(text, "\x1b") {
			t.Errorf("a hostile name was not drawn as plain text at %dx%d", size[0], size[1])
		}

		// The figures after a long name stay on the screen.
		if !strings.Contains(text, "… · 5%") {
			t.Errorf("a long name pushed the figures off the %dx%d screen", size[0], size[1])
		}
	}
}

func TestGoldenGPU(t *testing.T) {
	t.Parallel()

	unknown := []hostGPU{{Name: "RTX 4060 Ti 16GB", Note: "driver not active"}, {Name: "Radeon 880M / 890M", HasTemp: true, TempC: 44}}

	many := make([]hostGPU, 0, 6)
	for range 6 {
		many = append(many, testGPUs[0])
	}

	for _, row := range []struct {
		name string
		gpus []hostGPU
		w, h int
	}{
		// A host without a card draws the screens it drew before the pane knew cards.
		{"normal-80x25.txt", nil, 80, 25},
		{"normal-120x36.txt", nil, 120, 36},
		{"normal-160x45.txt", nil, 160, 45},
		{"gpu-one-80x25.txt", testGPUs[:1], 80, 25},
		{"gpu-one-120x36.txt", testGPUs[:1], 120, 36},
		{"gpu-two-80x25.txt", testGPUs, 80, 25},
		{"gpu-two-120x36.txt", testGPUs, 120, 36},
		{"gpu-unknown-80x25.txt", unknown, 80, 25},
		{"gpu-unknown-120x36.txt", unknown, 120, 36},
		{"gpu-many-80x24.txt", many, 80, 24},
		{"gpu-many-120x36.txt", many, 120, 36},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			golden(t, row.name, drawWithHardwareAndGPUs(t, tracerSnapshot(), testHardware, row.gpus, row.w, row.h))
		})
	}
}
