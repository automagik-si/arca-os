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
// Otherwise a version being checked must reach "system ready" in time, or each wait is a failed check.
func ReconcileBoot(ctx context.Context, s *state.State) {
	a := &auto{s: s, m: &liveMachine{s: s}}

	s.LockUpdateHealth()
	defer s.UnlockUpdateHealth()

	util.SetFailedBootVersions(FailedVersions(s))

	reason := a.reconcileBoot(ctx)
	if reason == "" {
		go watchReady(ctx, a, readyDeadline)

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

// watchReady counts each wait of the given length for "system ready" as a failed check.
func watchReady(ctx context.Context, a *auto, wait time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		if a.s.OS.SystemIsReady {
			return
		}

		a.s.LockUpdateHealth()
		a.notReady(ctx)
		a.s.UnlockUpdateHealth()
	}
}

// Run drives automatic OS updates once the system is ready, until ctx ends: the health bake of a
// new version, its rollback, and restarts into a pending version inside the restart window.
func Run(ctx context.Context, s *state.State) {
	started.Do(func() {
		go loop(ctx, &auto{s: s, m: &liveMachine{s: s}})
	})
}

func loop(ctx context.Context, a *auto) {
	a.s.LockUpdateHealth()
	a.ready(ctx)
	a.s.UnlockUpdateHealth()

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

			a.s.LockUpdateHealth()
			defer a.s.UnlockUpdateHealth()

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

	fails = append(fails, nvidiaHealth(h.NVIDIAWasActive, l.nvidiaActive(), l.s.Applications.NVIDIA.State)...)

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

	all, err := c.GetInstancesAllProjects(incusapi.InstanceTypeAny)
	if err != nil {
		return []string{"the Incus instances could not be listed"}
	}

	return instanceFailures(instances, all)
}

// instanceFailures returns the recorded instances that don't run. One that no longer exists isn't a failure.
func instanceFailures(recorded []string, all []incusapi.Instance) []string {
	fails := []string{}

	for _, name := range recorded {
		i := slices.IndexFunc(all, func(inst incusapi.Instance) bool { return inst.Project+"/"+inst.Name == name })
		if i >= 0 && all[i].StatusCode != incusapi.Running {
			fails = append(fails, "instance "+name+" is not running")
		}
	}

	return fails
}

// recordedInstances returns the running instances to bring back after a restart; ephemeral ones go away by design.
func recordedInstances(all []incusapi.Instance) []string {
	running := []string{}

	for _, inst := range all {
		if inst.StatusCode == incusapi.Running && !inst.Ephemeral {
			running = append(running, inst.Project+"/"+inst.Name)
		}
	}

	slices.Sort(running)

	return running
}

func (l *liveMachine) snapshot(ctx context.Context) restartSnapshot {
	snap := restartSnapshot{addresses: l.addresses(ctx), nvidia: l.nvidiaActive()}

	if !l.incusInstalled(ctx) {
		return snap
	}

	c, err := incusClient(ctx)
	if err == nil {
		var all []incusapi.Instance

		all, err = c.GetInstancesAllProjects(incusapi.InstanceTypeAny)
		snap.instances = recordedInstances(all)
	}

	if err != nil {
		slog.WarnContext(ctx, "Running instances could not be listed before the update restart", "error", err)
	}

	return snap
}

// nvidiaActive reports whether the NVIDIA driver drives a GPU now.
func (l *liveMachine) nvidiaActive() bool {
	return l.s.Applications.NVIDIA.State.ActiveDriver == "nvidia"
}

// nvidiaHealth fails only when the NVIDIA driver ran before the restart and no longer does. A node
// whose GPUs all go to passthrough never ran it, so it isn't checked.
func nvidiaHealth(wasActive bool, active bool, nv api.ApplicationNVIDIAState) []string {
	if !wasActive || active || nv.NeedsReboot {
		return nil
	}

	reason := "the NVIDIA driver no longer runs"
	if nv.Reason != "" {
		reason += ": " + nv.Reason
	}

	return []string{reason}
}

func (l *liveMachine) applications(ctx context.Context) map[string]string {
	versions := map[string]string{}

	for _, name := range checkedApplications {
		app, err := applications.Load(ctx, l.s, name)
		if err == nil && app.IsInstalled() {
			versions[name] = app.Version()
		}
	}

	return versions
}

func (*liveMachine) startInstances(ctx context.Context, instances []string) {
	c, err := incusClient(ctx)
	if err != nil {
		return
	}

	all, err := c.GetInstancesAllProjects(incusapi.InstanceTypeAny)
	if err != nil {
		return
	}

	for _, inst := range instances {
		i := slices.IndexFunc(all, func(a incusapi.Instance) bool { return a.Project+"/"+a.Name == inst })
		if i < 0 || all[i].StatusCode == incusapi.Running {
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

	return switchBoot(ctx, bootSwitch{
		version:     version,
		target:      v.OtherFilepath,
		current:     v.CurrentFilepath,
		sbDisabled:  l.s.SecureBootDisabled,
		keyChanged:  secureboot.UKIHasDifferentSecureBootCertificate,
		rekey:       func(ctx context.Context, uki string) error { return secureboot.HandleSecureBootKeyChange(ctx, uki, "") },
		rebindPCR4:  secureboot.UpdatePCR4Binding,
		nextVersion: util.SetNextBootVersion,
		nextCurrent: func(ctx context.Context) error { return util.SetNextBootID(ctx, util.BootIDCurrent) },
	})
}

// bootSwitch is what switchBoot uses to start another UKI; tests replace it.
type bootSwitch struct {
	version, target, current string
	sbDisabled               bool

	keyChanged  func(uki string) (bool, error)
	rekey       func(ctx context.Context, uki string) error
	rebindPCR4  func(ctx context.Context, uki string) error
	nextVersion func(ctx context.Context, version string) error
	nextCurrent func(ctx context.Context) error
}

// switchBoot makes the next boot start the target UKI, with the disks bound so it can unlock them.
// A target signed by another Secure Boot key is enrolled first; when the disks can't be bound to the
// target, the next boot stays on the current UKI and an error says why.
func switchBoot(ctx context.Context, b bootSwitch) error {
	changed, err := b.keyChanged(b.target)
	if err != nil {
		return errors.New("the Secure Boot key of " + b.version + " could not be checked: " + err.Error())
	}

	err = b.nextVersion(ctx, b.version)
	if err != nil {
		return err
	}

	switch {
	case changed:
		// The target is signed by another key: the TPM must trust it, or the disks won't unlock.
		err = b.rekey(ctx, b.target)
		if err != nil {
			rerr := b.rekey(ctx, b.current)
			if rerr != nil {
				slog.ErrorContext(ctx, "Disks could not be bound back to the running version", "error", rerr)
			}
		}
	case b.sbDisabled:
		// Without Secure Boot the disks are bound to one UKI; bind them to the target.
		err = b.rebindPCR4(ctx, b.target)
		if err != nil {
			rerr := b.rebindPCR4(ctx, b.current)
			if rerr != nil {
				slog.ErrorContext(ctx, "Disks could not be bound back to the running version", "error", rerr)
			}
		}
	default:
		// Secure Boot on and the same key: the disks unlock in the target as they are.
	}

	if err != nil {
		nerr := b.nextCurrent(ctx)
		if nerr != nil {
			slog.ErrorContext(ctx, "Next boot could not be set back to the running version", "error", nerr)
		}

		return errors.New("the disks could not be bound to " + b.version + ": " + err.Error())
	}

	return nil
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
