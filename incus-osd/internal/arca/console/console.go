// Package console draws the Arca console on the machine's screen: the node, its
// addresses and its instances, as the content of upstream's frame page. Upstream's
// modals and the Wi-Fi picker stay above it, and upstream's own screen takes over
// whenever the console fails.
package console

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/tui"
)

// How often each source is read.
const (
	identityEvery  = time.Minute
	ipsEvery       = 10 * time.Second
	onlineEvery    = 5 * time.Second
	instancesEvery = 10 * time.Second
	clockEvery     = time.Second
	installEvery   = 5 * time.Second
)

// host is what the console needs from the TUI; *tui.TUI provides it.
type host interface {
	EnableArcaOverlay()
	SetArcaConsole(root tview.Primitive, focus tview.Primitive)
	DisableArcaConsole()
	ArcaConsoleEnabled() bool
	QueueArcaUpdate(f func())
	ArcaLogView() *tview.TextView
	ArcaLastLog() string
}

// sources are the console's inputs, one per field; tests replace them.
type sources struct {
	now       func() time.Time
	node      func() string
	release   func() string
	apps      func(ctx context.Context) ([]App, error)
	ips       func(ctx context.Context) []string
	online    func() Online
	instances func() (instancesResult, error)
	updates   func() Updates
	actions   updateActions
}

// liveSources reads the running system.
func liveSources(s *state.State) sources {
	r := &incusReader{}

	return sources{
		now:       time.Now,
		node:      readNode,
		release:   func() string { return s.OS.RunningRelease },
		apps:      func(ctx context.Context) ([]App, error) { return readApps(ctx, s) },
		ips:       func(ctx context.Context) []string { return readIPs(ctx, s) },
		online:    func() Online { return readOnline(s) },
		instances: r.read,
		updates:   func() Updates { return liveUpdates(s) },
		actions:   liveActions{s: s},
	}
}

// Start runs the console in the background and returns at once. While the
// machine installs, upstream's screen stays.
func Start(ctx context.Context, s *state.State, t *tui.TUI) {
	go run(ctx, s, t, liveSources(s))
}

func run(ctx context.Context, s *state.State, h host, src sources) {
	for s.ShouldPerformInstall {
		if sleep(ctx, installEvery) != nil {
			return
		}
	}

	c := newConsole(h, src)
	c.begin(ctx)
	c.collect(ctx)
}

// console holds the snapshot the collectors write and the view that draws it.
type console struct {
	h     host
	src   sources
	view  *view
	guard *guard
	table instanceTable

	mu   sync.Mutex
	snap Snapshot

	pubMu     sync.Mutex
	shown     Snapshot
	published bool

	failMu sync.Mutex
}

func newConsole(h host, src sources) *console {
	c := &console{h: h, src: src, view: newView()}
	c.guard = &guard{Primitive: c.view, focus: c.view, on: h.ArcaConsoleEnabled, fail: c.fail}
	c.snap.OSRelease = src.release()
	c.snap.Now = src.now()
	c.view.key = c.key
	c.view.log = h.ArcaLogView()
	c.view.lastLog = h.ArcaLastLog
	c.view.followLog()
	c.view.update(c.snap.clone())

	return c
}

// begin puts the console on the screen.
func (c *console) begin(ctx context.Context) {
	c.h.EnableArcaOverlay()
	c.h.SetArcaConsole(c.guard, c.view)
	slog.InfoContext(ctx, "Console: started")
}

// collect runs one loop per source until ctx ends; the clock's runs here.
func (c *console) collect(ctx context.Context) {
	go c.every(ctx, identityEvery, func() {
		node, release := c.src.node(), c.src.release()
		apps, err := c.src.apps(ctx)

		c.mu.Lock()
		defer c.mu.Unlock()

		c.snap.Node, c.snap.OSRelease = node, release
		if err == nil {
			c.snap.Apps = apps
		}
	})

	go c.every(ctx, ipsEvery, func() {
		ips := c.src.ips(ctx)

		c.mu.Lock()
		defer c.mu.Unlock()

		c.snap.IPs = ips
	})

	go c.every(ctx, onlineEvery, func() {
		online := c.src.online()

		c.mu.Lock()
		defer c.mu.Unlock()

		c.snap.Online = online
	})

	go c.every(ctx, instancesEvery, func() {
		res, err := c.src.instances()
		now := c.src.now()

		c.mu.Lock()
		defer c.mu.Unlock()

		c.table.apply(&c.snap, res, err, now)
	})

	go c.every(ctx, updatesEvery, c.readUpdates)

	c.every(ctx, clockEvery, func() {})
}

// every runs read, then publishes, every d until ctx ends. A panic in read is
// recovered: it disables the console, and the loop goes on.
func (c *console) every(ctx context.Context, d time.Duration, read func()) {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					c.fail(fmt.Errorf("collector: %v", r))
				}
			}()

			read()
			c.publish()
		}()

		if sleep(ctx, d) != nil {
			return
		}
	}
}

// publish hands the snapshot to the view when it would draw a different screen.
func (c *console) publish() {
	c.mu.Lock()
	c.snap.Now = c.src.now()
	s := c.snap.clone()
	c.mu.Unlock()

	c.pubMu.Lock()
	defer c.pubMu.Unlock()

	if c.published && s.Equal(c.shown) {
		return
	}

	c.shown, c.published = s, true

	c.h.QueueArcaUpdate(func() {
		defer func() {
			if r := recover(); r != nil {
				c.fail(fmt.Errorf("update: %v", r))
			}
		}()

		c.view.update(s)
	})
}

// fail turns the console off after a recovered panic, so upstream's screen shows.
// It returns at once: it may run on the event loop, so the line is logged from
// another goroutine and the screen never waits on the journal.
func (c *console) fail(err error) {
	c.failMu.Lock()
	defer c.failMu.Unlock()

	if !c.h.ArcaConsoleEnabled() {
		return
	}

	c.h.DisableArcaConsole()

	msg := printable(err.Error())

	go slog.Error("Console: disabled after an error", "err", msg)
}

// guard wraps the console's root. A panic in its Draw or its InputHandler is
// recovered and turns the console off; while the console is off it draws nothing
// and takes no key. Focus goes to the console's focus primitive.
type guard struct {
	tview.Primitive

	focus tview.Primitive
	on    func() bool
	fail  func(error)
}

func (g *guard) Draw(screen tcell.Screen) {
	if !g.on() {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			g.fail(fmt.Errorf("draw: %v", r))
		}
	}()

	g.Primitive.Draw(screen)
}

func (g *guard) InputHandler() func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
	return func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
		if !g.on() {
			return
		}

		defer func() {
			if r := recover(); r != nil {
				g.fail(fmt.Errorf("input: %v", r))
			}
		}()

		if handler := g.Primitive.InputHandler(); handler != nil {
			handler(event, setFocus)
		}
	}
}

func (g *guard) Focus(delegate func(p tview.Primitive)) {
	delegate(g.focus)
}

func (g *guard) HasFocus() bool {
	return g.Primitive.HasFocus()
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
