package console

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/tui"
)

var update = flag.Bool("update", false, "rewrite the golden screens in testdata")

// testNow is the clock of every golden screen.
var testNow = time.Date(2026, 11, 3, 14, 22, 0, 0, time.UTC)

// draw draws snap on a simulated w×h screen and returns the screen.
func draw(t *testing.T, snap Snapshot, w, h int) tcell.SimulationScreen {
	t.Helper()

	screen := tcell.NewSimulationScreen("UTF-8")

	err := screen.Init()
	if err != nil {
		t.Fatal(err)
	}

	screen.SetSize(w, h)

	v := newView()
	v.update(snap)
	v.SetRect(0, 0, w, h)
	v.Draw(screen)

	return screen
}

// screenLine returns the text of row y, trailing blanks removed.
func screenLine(screen tcell.SimulationScreen, y int) string {
	w, _ := screen.Size()

	var b strings.Builder

	for x := 0; x < w; x++ {
		str, _, cw := screen.Get(x, y)
		b.WriteString(str)

		if cw > 1 {
			x += cw - 1
		}
	}

	return strings.TrimRight(b.String(), " ")
}

// screenText returns every row of the screen, one per line.
func screenText(screen tcell.SimulationScreen) string {
	_, h := screen.Size()

	var b strings.Builder

	for y := range h {
		b.WriteString(screenLine(screen, y))
		b.WriteByte('\n')
	}

	return b.String()
}

// golden compares the screen with testdata/<name>, or rewrites it with -update.
func golden(t *testing.T, name string, screen tcell.SimulationScreen) {
	t.Helper()

	got := screenText(screen)
	path := filepath.Join("testdata", name)

	if *update {
		err := os.WriteFile(path, []byte(got), 0o644) //nolint:gosec
		if err != nil {
			t.Fatal(err)
		}

		return
	}

	want, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		t.Fatalf("%v (run go test -update to write it)", err)
	}

	if got != string(want) {
		t.Errorf("the screen differs from %s\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// cellStyle returns the style of the first cell of sub on row y.
func cellStyle(t *testing.T, screen tcell.SimulationScreen, y int, sub string) (tcell.Color, tcell.Color, tcell.AttrMask) {
	t.Helper()

	line := screenLine(screen, y)

	i := strings.Index(line, sub)
	if i < 0 {
		t.Fatalf("row %d has no %q: %q", y+1, sub, line)
	}

	_, style, _ := screen.Get(utf8.RuneCountInString(line[:i]), y)

	return style.Decompose()
}

// lastCellStyle returns the foreground of the last occurrence of sub on row y.
func lastCellStyle(t *testing.T, screen tcell.SimulationScreen, y int, sub string) tcell.Color {
	t.Helper()

	line := screenLine(screen, y)

	i := strings.LastIndex(line, sub)
	if i < 0 {
		t.Fatalf("row %d has no %q: %q", y+1, sub, line)
	}

	_, style, _ := screen.Get(utf8.RuneCountInString(line[:i]), y)
	fg, _, _ := style.Decompose()

	return fg
}

// rowOf returns the row that holds sub, or -1.
func rowOf(screen tcell.SimulationScreen, sub string) int {
	_, h := screen.Size()
	for y := range h {
		if strings.Contains(screenLine(screen, y), sub) {
			return y
		}
	}

	return -1
}

// tracerSnapshot is the node of the golden screens: two applications, two
// addresses and the instances of testRows with the numbers a live node reports.
func tracerSnapshot() Snapshot {
	rows := testRows()

	// phone-1, probe-1, vm-1 and web, in the table's order.
	rows[0].CPUPercent, rows[0].CPULimit = 3.9, 2
	rows[0].MemUsage, rows[0].MemLimit = 2<<30, 4<<30
	rows[0].DiskUsage, rows[0].StartedAt = 9<<30+614<<20, testNow.Add(-(19*time.Hour + 12*time.Minute))
	rows[2].CPUPercent, rows[2].CPULimit = 12.1, 4
	rows[2].MemUsage, rows[2].MemLimit = 3<<30+819<<20, 4<<30
	rows[2].DiskUsage, rows[2].StartedAt = 18<<30+410<<20, testNow.Add(-(3*24*time.Hour + 4*time.Hour))
	rows[3].CPUPercent, rows[3].CPULimit = 1.1, 2
	rows[3].MemUsage, rows[3].MemLimit = 480<<20, 2<<30
	rows[3].DiskUsage, rows[3].StartedAt = 6<<30+307<<20, testNow.Add(-(3*24*time.Hour + 4*time.Hour))

	return Snapshot{
		Node:        "node-1",
		OSRelease:   "202611030000",
		Apps:        []App{{Name: "arca", Version: "0.1.0"}, {Name: "incus", Version: "6.0.0"}},
		Online:      Online{Known: true, Up: true},
		IPs:         []string{"192.0.2.10", "2001:db8::10"},
		Instances:   rows,
		Details:     []InstanceDetails{{Name: "phone-1", Project: "other", Type: "PHONE", Description: "example/phone", Status: "Running", Disks: []DiskUse{{Name: "root", Usage: rows[0].DiskUsage}}}},
		InstancesAt: testNow.Add(-5 * time.Second),
		Now:         testNow,
	}
}

// TestMain pins the host the bars are measured against, so the goldens do not
// depend on the machine running the tests.
func TestMain(m *testing.M) {
	hostThreads = func() int { return 8 }
	hostMemoryBytes = func() int64 { return 8 << 30 }

	m.Run()
}

func TestGoldenTracer(t *testing.T) {
	t.Parallel()

	for _, size := range []struct{ w, h int }{{80, 25}, {160, 45}} {
		screen := draw(t, tracerSnapshot(), size.w, size.h)

		// Colours on named cells.
		if fg, bg, _ := cellStyle(t, screen, 0, "ARCA"); bg != tcell.ColorTeal || fg != tcell.ColorBlack {
			t.Errorf("%dx%d: the brand is %v on %v, want black on teal", size.w, size.h, fg, bg)
		}

		if fg, _, _ := cellStyle(t, screen, 0, "● online"); fg != tcell.ColorGreen {
			t.Errorf("%dx%d: online is %v, want green", size.w, size.h, fg)
		}

		web := rowOf(screen, "web ")
		if fg, _, _ := cellStyle(t, screen, web, "RUNNING"); fg != tcell.ColorGreen {
			t.Errorf("%dx%d: web's RUNNING is %v, want green", size.w, size.h, fg)
		}

		probe := rowOf(screen, "probe-1")
		if fg, _, _ := cellStyle(t, screen, probe, "STOPPED"); fg != tcell.ColorGray {
			t.Errorf("%dx%d: probe-1's STOPPED is %v, want grey", size.w, size.h, fg)
		}

		if fg, _, _ := cellStyle(t, screen, rowOf(screen, "phone-1"), "PHONE"); fg != tcell.ColorDarkCyan {
			t.Errorf("%dx%d: PHONE is %v, want dark cyan", size.w, size.h, fg)
		}

		if fg := lastCellStyle(t, screen, probe, "—"); fg != tcell.ColorGray {
			t.Errorf("%dx%d: probe-1's missing address is %v, want grey", size.w, size.h, fg)
		}
	}

	// Below 80×24 only the header and a note show.
	small := draw(t, tracerSnapshot(), 60, 20)
	if got := screenLine(small, 2); got != " Screen too small (60×20)" {
		t.Errorf("a 60x20 screen shows %q", got)
	}

	if !strings.Contains(screenLine(small, 0), "node-1") || rowOf(small, "probe-1") >= 0 {
		t.Errorf("a 60x20 screen does not show only the header:\n%s", screenText(small))
	}
}

func TestHostileNames(t *testing.T) {
	t.Parallel()

	snap := tracerSnapshot()
	snap.Node = "node-1\x1b]0;x\a"
	snap.Instances = []Instance{
		{Name: "[red]x", Project: "default", Type: "CT", Status: "Running"},
		{Name: "a\x1b[2Jb", Project: "default", Type: "CT", Status: "Running"},
		{Name: "c\u009b31md", Project: "default", Type: "CT", Status: "Up[::b]"},
		{Name: "e\xffg", Project: "default", Type: "CT", Status: "Running"},
	}

	screen := draw(t, snap, 80, 25)
	text := screenText(screen)

	for _, want := range []string{"[red]x", "a?[2Jb", "c?31md", "UP[::B]", "e\uFFFDg", "node-1?]0;x?"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q is not on the screen as literal text:\n%s", want, text)
		}
	}

	if fg, _, _ := cellStyle(t, screen, rowOf(screen, "[red]x"), "x "); fg == tcell.ColorRed {
		t.Error("the name [red]x turned its text red")
	}

	cells, _, _ := screen.GetContents()
	for i, c := range cells {
		for _, r := range string(c.Bytes) {
			if r == 0x1b || r == 0x9b || r < 0x20 || r == 0x7f {
				t.Fatalf("cell %d holds the control character %U", i, r)
			}
		}
	}
}

// logRecorder records the messages logged through it and passes them on.
type logRecorder struct {
	next slog.Handler
	mu   *sync.Mutex
	msgs *[]string
}

func (l logRecorder) Enabled(ctx context.Context, level slog.Level) bool {
	return l.next.Enabled(ctx, level)
}

// Handle records r once the TUI has taken it, so a log line that blocks is never recorded.
func (l logRecorder) Handle(ctx context.Context, r slog.Record) error {
	err := l.next.Handle(ctx, r)

	l.mu.Lock()
	*l.msgs = append(*l.msgs, r.Message)
	l.mu.Unlock()

	return err
}

func (l logRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return logRecorder{next: l.next.WithAttrs(attrs), mu: l.mu, msgs: l.msgs}
}

func (l logRecorder) WithGroup(name string) slog.Handler {
	return logRecorder{next: l.next.WithGroup(name), mu: l.mu, msgs: l.msgs}
}

// logToTUI sends the default logger to tt, as the daemon does, and returns a
// function that waits for msg to be logged.
func logToTUI(t *testing.T, tt *tui.TUI) func(msg string) bool {
	t.Helper()

	var mu sync.Mutex

	msgs := []string{}

	prev := slog.Default()

	slog.SetDefault(slog.New(logRecorder{next: tui.NewCustomTextHandler(tt), mu: &mu, msgs: &msgs}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return func(msg string) bool {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			found := slices.Contains(msgs, msg)
			mu.Unlock()

			if found {
				return true
			}

			time.Sleep(10 * time.Millisecond)
		}

		return false
	}
}

// panicky panics in Draw or InputHandler once armed.
type panicky struct {
	tview.Primitive

	draw, input atomic.Bool
}

func (p *panicky) Draw(screen tcell.Screen) {
	if p.draw.Load() {
		panic("test panic in Draw")
	}

	p.Primitive.Draw(screen)
}

func (p *panicky) InputHandler() func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
	return func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
		if p.input.Load() {
			panic("test panic in InputHandler")
		}

		if handler := p.Primitive.InputHandler(); handler != nil {
			handler(event, setFocus)
		}
	}
}

// staticSources returns the tracer snapshot's values.
func staticSources() sources {
	snap := tracerSnapshot()

	return sources{
		now:     func() time.Time { return testNow },
		node:    func() string { return snap.Node },
		release: func() string { return snap.OSRelease },
		apps:    func(context.Context) ([]App, error) { return snap.Apps, nil },
		ips:     func(context.Context) []string { return snap.IPs },
		online:  func() Online { return snap.Online },
		instances: func() (instancesResult, error) {
			return instancesResult{Rows: snap.Instances, Details: snap.Details}, nil
		},
	}
}

func TestPanicFallsBack(t *testing.T) { //nolint:paralleltest // points the default logger at the test's TUI
	for _, where := range []string{"collector", "draw", "input"} { //nolint:paralleltest // the subtests share the default logger
		t.Run(where, func(t *testing.T) { //nolint:paralleltest // the subtests share the default logger
			s := &state.State{}
			s.OS.Name = "Example OS"
			s.OS.RunningRelease = "202611030000"

			screen := tcell.NewSimulationScreen("UTF-8")
			tt, redraw, stop := tui.NewArcaTestTUI(s, screen)
			t.Cleanup(stop)
			screen.SetSize(80, 25)

			logged := logToTUI(t, tt)

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			src := staticSources()

			if where == "collector" {
				src.instances = func() (instancesResult, error) {
					panic("test panic in a collector")
				}
			}

			c := newConsole(tt, src)
			p := &panicky{Primitive: c.view}
			c.guard.Primitive = p
			c.begin(ctx)

			if !logged("Console: started") || !tt.ArcaConsoleEnabled() {
				t.Fatal("the console did not start")
			}

			// The console works before the panic: its header is on the screen.
			c.publish()
			tt.QueueArcaUpdate(func() {})

			var row0 string

			tt.QueueArcaUpdate(func() { row0 = screenLine(screen, 0) })

			if !strings.Contains(row0, "Arca OS 202611030000") {
				t.Fatalf("the console is not on the screen before the panic: %q", row0)
			}

			switch where {
			case "collector":
				go c.collect(ctx)
			case "draw":
				p.draw.Store(true)
				tt.QueueArcaUpdate(func() {})
			case "input":
				p.input.Store(true)
				screen.InjectKey(tcell.KeyRune, 'x', tcell.ModNone)
			default:
				t.Fatalf("unknown panic site %q", where)
			}

			if !logged("Console: disabled after an error") {
				t.Fatal("no \"Console: disabled after an error\" was logged")
			}

			if tt.ArcaConsoleEnabled() {
				t.Fatal("the console is still enabled after the panic")
			}

			// The next periodic redraw draws upstream's frame.
			redraw()

			var text string

			tt.QueueArcaUpdate(func() { text = screenText(screen) })

			if !strings.Contains(strings.SplitN(text, "\n", 2)[0], "Example OS 202611030000") {
				t.Errorf("upstream's header is not on row 1 after the panic:\n%s", text)
			}

			if !strings.Contains(text, "Network configuration:") {
				t.Errorf("upstream's footer is not on the screen after the panic:\n%s", text)
			}
		})
	}
}
