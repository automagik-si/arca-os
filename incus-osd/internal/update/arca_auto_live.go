package update

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/applications"
	"github.com/lxc/incus-os/incus-osd/internal/secureboot"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// ArcaBusySocket is the optional Arca hook: GET /1.0/busy on it answers {"busy": bool, "reason": string}.
var ArcaBusySocket = "/run/arca/update.sock"

// started guards against a second automatic updater in the same daemon.
var started sync.Once

// ReconcileBoot updates the automatic updater's record early in startup, before the next boot is
// chosen. When this boot runs a version that must not stay, it goes back to the previous one at once.
func ReconcileBoot(ctx context.Context, s *state.State) {
	a := &auto{s: s, m: &liveMachine{s: s}}

	reason := a.reconcileBoot(ctx)
	if reason == "" {
		return
	}

	previous := ""
	if b := s.UpdateHealth.Bake; b != nil && !b.Rollback && b.Version == s.OS.RunningRelease {
		previous = b.Previous
	}

	if previous == "" {
		v, err := util.GetUKIVersions()
		if err == nil {
			previous = v.OtherVersion
		}
	}

	a.rollback(ctx, s.OS.RunningRelease, previous, reason)

	if s.UpdateHealth.NeedsOwner == "" {
		// The restart is under way; don't go on starting this version.
		time.Sleep(time.Minute)
	}
}

// NextBootSkips returns the OS versions the next boot must not pick.
func NextBootSkips(s *state.State) []string {
	return FailedVersions(s)
}

// Run drives automatic OS updates once the system is ready, until ctx ends: the health bake of a
// new version, its rollback, and restarts into a pending version inside the restart window.
func Run(ctx context.Context, s *state.State) {
	started.Do(func() {
		go loop(ctx, &auto{s: s, m: &liveMachine{s: s}})
	})
}

func loop(ctx context.Context, a *auto) {
	a.ready(ctx)

	for {
		timer := time.NewTimer(tickEvery)

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C:
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.ErrorContext(ctx, "Automatic updater failed", "error", r)
				}
			}()

			a.s.UpdateMutex.Lock()
			defer a.s.UpdateMutex.Unlock()

			a.tick(ctx)
		}()
	}
}

// liveMachine is the running node.
type liveMachine struct {
	s *state.State
}

func (*liveMachine) now() time.Time {
	return time.Now()
}

func (*liveMachine) bootID() (string, error) {
	body, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(body)) == "" {
		return "", errors.New("the current boot could not be identified")
	}

	return strings.TrimSpace(string(body)), nil
}

func (l *liveMachine) busy(ctx context.Context) string {
	reasons := []string{}

	nv := l.s.Applications.NVIDIA.State
	if nv.PendingAttempt != "" || nv.PendingHealth != "" {
		reasons = append(reasons, "the NVIDIA driver is being changed")
	}

	if l.incusInstalled(ctx) {
		reason := incusBusy(ctx)
		if reason != "" {
			reasons = append(reasons, reason)
		}
	}

	reason := arcaBusy(ctx)
	if reason != "" {
		reasons = append(reasons, reason)
	}

	return strings.Join(reasons, "; ")
}

// incusBusy returns the running Incus operations that a restart would break.
func incusBusy(ctx context.Context) string {
	c, err := incusClient(ctx)
	if err != nil {
		return ""
	}

	ops, err := c.GetOperationsAllProjects()
	if err != nil {
		return ""
	}

	names := []string{}

	for _, op := range ops {
		if op.StatusCode != incusapi.Running {
			continue
		}

		if op.Class != incusapi.OperationClassTask && !strings.Contains(op.Description, "Migrat") {
			continue
		}

		names = append(names, op.Description)
	}

	if len(names) == 0 {
		return ""
	}

	slices.Sort(names)

	return "Incus is busy (" + strings.Join(slices.Compact(names), ", ") + ")"
}

// arcaBusy asks the optional Arca hook whether a restart should wait. A missing or broken hook never blocks.
func arcaBusy(ctx context.Context) string {
	_, err := os.Stat(ArcaBusySocket)
	if err != nil {
		return ""
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{}

			return d.DialContext(ctx, "unix", ArcaBusySocket)
		}},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/1.0/busy", nil)
	if err != nil {
		return ""
	}

	resp, err := client.Do(req)
	if err != nil {
		slog.DebugContext(ctx, "Arca busy hook did not answer", "error", err)

		return ""
	}
	defer resp.Body.Close()

	answer := struct {
		Busy   bool   `json:"busy"`
		Reason string `json:"reason"`
	}{}

	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&answer) != nil || !answer.Busy {
		return ""
	}

	if answer.Reason == "" {
		return "Arca is busy"
	}

	return "Arca is busy (" + answer.Reason + ")"
}

func incusClient(ctx context.Context) (incusclient.InstanceServer, error) {
	return incusclient.ConnectIncusUnixWithContext(ctx, "", &incusclient.ConnectionArgs{HTTPClient: &http.Client{Timeout: 30 * time.Second}})
}

func (l *liveMachine) incusInstalled(ctx context.Context) bool {
	for _, name := range []string{"incus", "incus-lts-7.0"} {
		app, err := applications.Load(ctx, l.s, name)
		if err == nil && app.IsInstalled() {
			return true
		}
	}

	return false
}

func (l *liveMachine) health(ctx context.Context, h *api.SystemUpdateHealth) []string {
	fails := []string{}

	if !l.s.OS.SystemIsReady {
		fails = append(fails, "incus-osd is not ready")
	}

	for _, name := range []string{"incus", "incus-lts-7.0", "arca"} {
		app, err := applications.Load(ctx, l.s, name)
		if err != nil || !app.IsInstalled() {
			continue
		}

		if !app.IsRunning(ctx) {
			fails = append(fails, "the "+name+" application is not running")
		}
	}

	nv := l.s.Applications.NVIDIA
	if nv.Config.Driver == "nvidia" && nv.State.ActiveDriver == "nouveau" && nv.State.Reason != "" && !nv.State.NeedsReboot {
		fails = append(fails, "the NVIDIA driver fell back: "+nv.State.Reason)
	}

	if l.incusInstalled(ctx) {
		fails = append(fails, incusHealth(ctx, h.Instances)...)
	}

	current := l.addresses(ctx)
	for _, addr := range h.Addresses {
		if !slices.Contains(current, addr) {
			fails = append(fails, "address "+addr+" is missing")
		}
	}

	return fails
}

// incusHealth checks that the Incus API answers and that the given instances run.
func incusHealth(ctx context.Context, instances []string) []string {
	c, err := incusClient(ctx)
	if err != nil {
		return []string{"the Incus API does not answer"}
	}

	_, _, err = c.GetServer()
	if err != nil {
		return []string{"the Incus API does not answer"}
	}

	if len(instances) == 0 {
		return nil
	}

	running, err := listRunning(c)
	if err != nil {
		return []string{"the Incus instances could not be listed"}
	}

	fails := []string{}

	for _, inst := range instances {
		if !slices.Contains(running, inst) {
			fails = append(fails, "instance "+inst+" is not running")
		}
	}

	return fails
}

func listRunning(c incusclient.InstanceServer) ([]string, error) {
	all, err := c.GetInstancesAllProjects(incusapi.InstanceTypeAny)
	if err != nil {
		return nil, err
	}

	running := []string{}

	for _, inst := range all {
		if inst.StatusCode == incusapi.Running {
			running = append(running, inst.Project+"/"+inst.Name)
		}
	}

	slices.Sort(running)

	return running, nil
}

func (l *liveMachine) runningInstances(ctx context.Context) ([]string, error) {
	if !l.incusInstalled(ctx) {
		return nil, nil
	}

	c, err := incusClient(ctx)
	if err != nil {
		return nil, err
	}

	return listRunning(c)
}

func (*liveMachine) startInstances(ctx context.Context, instances []string) {
	c, err := incusClient(ctx)
	if err != nil {
		return
	}

	running, err := listRunning(c)
	if err != nil {
		return
	}

	for _, inst := range instances {
		if slices.Contains(running, inst) {
			continue
		}

		project, name, ok := strings.Cut(inst, "/")
		if !ok {
			continue
		}

		op, err := c.UseProject(project).UpdateInstanceState(name, incusapi.InstanceStatePut{Action: "start"}, "")
		if err == nil {
			err = op.Wait()
		}

		if err != nil {
			slog.WarnContext(ctx, "Instance running before the update could not be started", "instance", inst, "error", err)

			continue
		}

		slog.InfoContext(ctx, "Started an instance that was running before the update", "instance", inst)
	}
}

// addresses returns the machine's global IPv4 addresses, without the instance bridges.
func (*liveMachine) addresses(_ context.Context) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	addrs := []string{}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}

		if slices.ContainsFunc([]string{"incusbr", "veth", "tap", "macvtap", "vnet", "lxdbr", "tailscale", "nb-"}, func(p string) bool {
			return strings.HasPrefix(iface.Name, p)
		}) {
			continue
		}

		list, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, a := range list {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil || !ipnet.IP.IsGlobalUnicast() {
				continue
			}

			addrs = append(addrs, ipnet.IP.String())
		}
	}

	slices.Sort(addrs)

	return slices.Compact(addrs)
}

func (*liveMachine) markGood(version string) error {
	return util.MarkUKIGood(version)
}

func (*liveMachine) markBad(version string) error {
	return util.MarkUKIBad(version)
}

func (l *liveMachine) bootInto(ctx context.Context, version string) error {
	v, err := util.GetUKIVersions()
	if err != nil {
		return err
	}

	if v.OtherVersion != version {
		return errors.New("version " + version + " is not on the other slot")
	}

	// Without Secure Boot the disks are bound to one UKI; bind them to the one we go back to.
	if l.s.SecureBootDisabled {
		err := secureboot.UpdatePCR4Binding(ctx, v.OtherFilepath)
		if err != nil {
			return errors.New("the disks could not be bound to " + version + ": " + err.Error())
		}
	}

	return util.SetNextBootVersion(ctx, version)
}

func (l *liveMachine) reboot(ctx context.Context) error {
	if l.s.TriggerReboot != nil {
		return rebootSystem(ctx, l.s)
	}

	// Early in startup the daemon's shutdown sequence isn't set up yet.
	err := l.s.SaveDurable()
	if err != nil {
		return err
	}

	return systemd.SystemReboot(ctx)
}

func (l *liveMachine) save() error {
	return l.s.SaveDurable()
}
