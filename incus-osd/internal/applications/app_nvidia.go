package applications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

const nvidiaStable = "nvidia"

const nvidiaLTS = "nvidia-lts"

const nvidiaBootFile = "/run/incus-os/nvidia-boot.json"

type nvidia struct {
	common

	variant           string
	sysroot           string
	runCommand        func(context.Context, string, ...string) (string, error)
	verifyModule      func(string) error
	verifyImage       func(context.Context, string) error
	refreshExtensions func(context.Context) error
	findExtension     func(context.Context, string, string, bool) (string, []string, string)
	extensionFiles    *nvidiaExtensionFiles
	bootRecovery      bool
	healthTimeout     time.Duration
}

type nvidiaBoot struct {
	ExtensionVersion string                      `json:"extension_version,omitempty"`
	Config           api.ApplicationNVIDIAConfig `json:"config"`
	Version          string                      `json:"version"`
	Cards            []string                    `json:"cards"`
	Reason           string                      `json:"reason"`
	Failure          bool                        `json:"failure,omitempty"`
	Attempted        bool                        `json:"attempted,omitempty"`
	Deferred         bool                        `json:"deferred,omitempty"`
}

func (n *nvidia) Name() string {
	return n.variant
}

func (n *nvidia) Get(_ context.Context) (any, error) {
	app := n.state.Applications.NVIDIA

	app.State.RequestedBranch = n.requestedBranch()
	if app.State.ActiveBranch == "" && app.State.ActiveExtensionVersion != "" {
		app.State.ActiveBranch = nvidiaStable
	}

	return app, nil
}

func (*nvidia) Struct() any {
	return &api.ApplicationNVIDIA{}
}

func (*nvidia) GetDependencies() []string {
	return []string{"gpu-support"}
}

func (n *nvidia) IsInstalled() bool {
	return n.Version() != "" && n.variant == n.requestedBranch()
}

func (n *nvidia) IsRunning(_ context.Context) bool {
	return n.state.Applications.NVIDIA.State.ActiveDriver == "nvidia"
}

func (*nvidia) StartupWeight() int {
	return -2
}

func (n *nvidia) SetFriendlyVersion(_ context.Context) error {
	version, err := os.ReadFile(n.path("/usr/share/nvidia/version"))
	if err != nil {
		if !n.state.Applications.NVIDIA.State.NeedsReboot && n.state.Applications.NVIDIA.State.Reason != systemd.NVIDIAQuarantinedReason {
			n.state.Applications.NVIDIA.State.Reason = systemd.NVIDIAMissingReason
		}

		return errors.New(systemd.NVIDIAMissingReason)
	}

	n.state.Applications.NVIDIA.State.DriverVersion = strings.TrimSpace(string(version))

	active := n.state.Applications.NVIDIA.State.ActiveExtensionVersion
	if active == "" {
		active = n.Version()
	}

	n.appState.FriendlyVersion = strings.TrimSpace(string(version)) + " [" + active + "]"

	return nil
}

func (n *nvidia) UpdateConfig(_ context.Context, req any) error {
	app, ok := req.(*api.ApplicationNVIDIA)
	if !ok {
		return fmt.Errorf("request type %T isn't expected ApplicationNVIDIA", req)
	}

	if app.Config.Driver != "nvidia" && app.Config.Driver != "off" {
		return errors.New("driver must be nvidia or off")
	}

	cards := map[string]string{}

	for address, use := range app.Config.Cards {
		if !regexp.MustCompile(`^[[:xdigit:]]{4}:[[:xdigit:]]{2}:[[:xdigit:]]{2}\.[0-7]$`).MatchString(address) {
			return fmt.Errorf("invalid PCI address %q", address)
		}

		if use != "nvidia" && use != "vfio-pci" && use != "off" {
			return fmt.Errorf("invalid use %q for %s", use, address)
		}

		canonical := normaliseNVIDIAAddress(address)

		_, exists := cards[canonical]
		if exists {
			return fmt.Errorf("duplicate PCI address %q", canonical)
		}

		cards[canonical] = use
	}

	app.Config.Cards = cards
	if app.Config.ResetBadVersions {
		clear(n.state.Applications.NVIDIA.State.BadVersions)
		clear(n.state.Applications.NVIDIA.State.BadBuilds)
		n.state.Applications.NVIDIA.State.PendingAttempt = ""
		n.state.Applications.NVIDIA.State.AttemptBootID = ""
		n.state.Applications.NVIDIA.State.PendingHealth = ""
		n.state.Applications.NVIDIA.State.HealthBootID = ""
		app.Config.ResetBadVersions = false
	}

	previous := n.state.Applications.NVIDIA.Config

	n.state.Applications.NVIDIA.Config = app.Config
	if previous.Driver == app.Config.Driver && maps.Equal(previous.Cards, app.Config.Cards) {
		return n.state.SaveDurable()
	}

	return n.deferSwitch("Reboot required to apply the NVIDIA driver choice")
}

func (n *nvidia) Initialize(_ context.Context) error {
	if n.state.Applications.NVIDIA.Config.Driver == "" {
		n.state.Applications.NVIDIA.Config.Driver = "nvidia"
	}

	n.appState.Initialized = true
	if n.state.Applications.NVIDIA.State.ActiveExtensionVersion != "" {
		return n.state.SaveDurable()
	}

	return n.requireReboot()
}

func (n *nvidia) Stop(_ context.Context) error {
	return n.requireReboot()
}

func (n *nvidia) Update(_ context.Context) error {
	if n.state.OS.SystemIsReady {
		return n.state.SaveDurable()
	}

	if n.state.Applications.NVIDIA.State.ActiveExtensionVersion != n.Version() {
		return n.state.SaveDurable()
	}

	n.clearBadVersion()

	return n.requireReboot()
}

func (n *nvidia) Restart(ctx context.Context) error {
	return n.reconcile(WithNVIDIALiveSwitch(ctx, n.Name()))
}

func (n *nvidia) Start(ctx context.Context) error {
	if n.state.OS.SystemIsReady {
		return n.state.SaveDurable()
	}

	claimed, err := claimNVIDIABoot(n.path(nvidiaStartedFile))
	if err != nil {
		slog.WarnContext(ctx, "NVIDIA boot startup could not be claimed; skipping the startup check", "error", err)

		n.state.Applications.NVIDIA.State.Reason = "NVIDIA startup check was skipped because its boot marker could not be written"

		return nil
	}

	if !claimed {
		return n.state.SaveDurable()
	}

	n.bootRecovery = true
	defer func() { n.bootRecovery = false }()

	boot := nvidiaBoot{}

	body, err := os.ReadFile(n.path(nvidiaBootFile))
	if errors.Is(err, os.ErrNotExist) {
		n.state.Applications.NVIDIA.State.Reason = "Reboot required to apply the NVIDIA driver choice"

		return n.requireReboot()
	} else if err != nil {
		return err
	}

	err = json.Unmarshal(body, &boot)
	if err != nil {
		return err
	}

	config := n.state.Applications.NVIDIA.Config
	if config.Driver != boot.Config.Driver || !maps.Equal(config.Cards, boot.Config.Cards) {
		err = n.requireReboot()
		if err != nil {
			return err
		}
	}

	if boot.Deferred {
		return n.deferSwitch(boot.Reason)
	}

	if boot.Failure {
		return n.fallback(ctx, boot, boot.Reason)
	}

	if flagNVIDIAReboot(n.state, boot) {
		return n.requireReboot()
	}

	if boot.ExtensionVersion == n.state.Applications.NVIDIA.State.ActiveExtensionVersion && config.Driver == boot.Config.Driver && maps.Equal(config.Cards, boot.Config.Cards) {
		n.state.Applications.NVIDIA.State.NeedsReboot = false
	}

	if boot.Reason != "" || len(boot.Cards) == 0 {
		n.state.Applications.NVIDIA.State.ActiveDriver = "nouveau"
		n.state.Applications.NVIDIA.State.Reason = boot.Reason

		recovery := n.recoveryReason()
		if recovery != "" {
			n.state.Applications.NVIDIA.State.Reason = recovery
		}

		return n.state.SaveDurable()
	}

	reason := n.state.Applications.NVIDIA.State.BadVersions[n.attemptKey(boot.Version)]
	if reason != "" {
		return n.fallback(ctx, boot, reason)
	}

	err = n.saveHealthAttempt(boot.Version)
	if err != nil {
		return err
	}

	err = n.health(ctx, boot.Cards)
	if err != nil {
		return n.fallback(ctx, boot, err.Error())
	}

	n.state.Applications.NVIDIA.State.PendingAttempt = ""
	n.state.Applications.NVIDIA.State.AttemptBootID = ""
	n.state.Applications.NVIDIA.State.PendingHealth = ""
	n.state.Applications.NVIDIA.State.HealthBootID = ""

	n.state.Applications.NVIDIA.State.ActiveDriver = "nvidia"
	n.state.Applications.NVIDIA.State.Reason = n.recoveryReason()

	return n.state.SaveDurable()
}

func flagNVIDIAReboot(s *state.State, boot nvidiaBoot) bool {
	app := &s.Applications.NVIDIA
	if app.Config.Driver != "nvidia" || app.State.ActiveExtensionVersion == "" || app.State.ActiveExtensionVersion == boot.ExtensionVersion {
		return false
	}

	app.State.Reason = "reboot to enable NVIDIA"
	if !boot.Attempted {
		app.State.ActiveDriver = "nouveau"
	}

	s.System.Update.State.NeedsReboot = true

	return true
}

func (n *nvidia) path(path string) string {
	return filepath.Join(n.sysroot, path)
}

func (n *nvidia) command(ctx context.Context, name string, args ...string) (string, error) {
	if n.runCommand != nil {
		return n.runCommand(ctx, name, args...)
	}

	return subprocess.RunCommandContext(ctx, name, args...)
}

func (n *nvidia) requireReboot() error {
	n.state.Applications.NVIDIA.State.NeedsReboot = true
	n.state.System.Update.State.NeedsReboot = true

	return n.state.SaveDurable()
}

func (n *nvidia) health(ctx context.Context, cards []string) error {
	if len(cards) == 0 {
		return errors.New("no NVIDIA GPU")
	}

	parent := ctx

	timeout := n.healthTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	out, err := n.command(ctx, "/usr/bin/nvidia-smi", "--query-gpu=pci.bus_id", "--format=csv,noheader")
	if err != nil {
		if parent.Err() != nil {
			return parent.Err()
		}

		return fmt.Errorf("%w: NVIDIA could not initialise every assigned GPU", errNVIDIAHealth)
	}

	initialised := map[string]bool{}
	for address := range strings.FieldsSeq(out) {
		initialised[normaliseNVIDIAAddress(address)] = true
	}

	for _, address := range cards {
		if !initialised[address] {
			return fmt.Errorf("NVIDIA health check failed for %s", address)
		}
	}

	return nil
}

func normaliseNVIDIAAddress(address string) string {
	address = strings.ToLower(address)
	if len(address) == 16 && strings.HasPrefix(address, "0000") {
		address = address[4:]
	}

	return address
}

func (n *nvidia) clearBadVersion() {
	n.clearBadDriverVersion(n.state.Applications.NVIDIA.State.DriverVersion)
}

func (n *nvidia) clearBadDriverVersion(version string) {
	s := &n.state.Applications.NVIDIA.State
	key := n.attemptKey(version)
	delete(s.BadVersions, key)

	id, err := n.bootID()
	if err == nil && s.PendingAttempt == key && s.AttemptBootID != id {
		s.PendingAttempt = ""
		s.AttemptBootID = ""
	}

	if err == nil && s.PendingHealth == key && s.HealthBootID != id {
		s.PendingHealth = ""
		s.HealthBootID = ""
	}
}

func (n *nvidia) attemptKey(version string) string {
	return n.state.OS.RunningRelease + ":" + version
}

func (n *nvidia) bootID() (string, error) {
	body, err := os.ReadFile(n.path("/proc/sys/kernel/random/boot_id"))
	if err != nil || strings.TrimSpace(string(body)) == "" {
		return "", errors.New("the current boot could not be identified")
	}

	return strings.TrimSpace(string(body)), nil
}

func (n *nvidia) saveAttempt(version string) error {
	id, err := n.bootID()
	if err != nil {
		return err
	}

	s := &n.state.Applications.NVIDIA.State
	previousAttempt, previousBoot := s.PendingAttempt, s.AttemptBootID
	s.PendingAttempt = n.attemptKey(version)
	s.AttemptBootID = id

	err = n.state.SaveDurable()
	if err != nil {
		s.PendingAttempt, s.AttemptBootID = previousAttempt, previousBoot

		return errors.New("NVIDIA startup could not be recorded safely")
	}

	return nil
}

func (n *nvidia) markBound() error {
	s := &n.state.Applications.NVIDIA.State
	s.PendingAttempt, s.AttemptBootID = "", ""

	return n.state.SaveDurable()
}

func (n *nvidia) saveHealthAttempt(version string) error {
	id, err := n.bootID()
	if err != nil {
		return err
	}

	s := &n.state.Applications.NVIDIA.State
	previousHealth, previousBoot := s.PendingHealth, s.HealthBootID
	s.PendingHealth, s.HealthBootID = n.attemptKey(version), id

	err = n.state.SaveDurable()
	if err != nil {
		s.PendingHealth, s.HealthBootID = previousHealth, previousBoot

		return errors.New("NVIDIA health check could not be recorded safely")
	}

	return nil
}

func (n *nvidia) checkPreviousAttempt() string {
	s := &n.state.Applications.NVIDIA.State
	if s.PendingAttempt == "" && s.PendingHealth == "" {
		return ""
	}

	id, err := n.bootID()
	if err != nil {
		return "the previous NVIDIA startup could not be checked"
	}

	key, attemptBoot := s.PendingAttempt, s.AttemptBootID
	reason := "NVIDIA did not complete its previous startup"

	if key == "" {
		key, attemptBoot = s.PendingHealth, s.HealthBootID
		reason = "NVIDIA did not complete its previous health check"
	}

	if id == attemptBoot {
		return "NVIDIA startup is already in progress"
	}

	if s.BadVersions == nil {
		s.BadVersions = map[string]string{}
	}

	s.BadVersions[key] = reason
	if s.Previous != nil {
		if s.BadBuilds == nil {
			s.BadBuilds = map[string]string{}
		}

		s.BadBuilds[n.buildKey(api.ApplicationNVIDIABuild{Branch: n.requestedBranch(), Version: s.Version})] = reason
	}

	s.PendingAttempt = ""
	s.AttemptBootID = ""
	s.PendingHealth = ""
	s.HealthBootID = ""

	err = n.state.SaveDurable()
	if err != nil {
		return "NVIDIA recovery state could not be saved"
	}

	return ""
}

func (n *nvidia) fallback(ctx context.Context, boot nvidiaBoot, reason string) error {
	if n.state.OS.SystemIsReady || !n.bootRecovery {
		return n.deferRollback(ctx, reason, false)
	}

	s := &n.state.Applications.NVIDIA.State
	if s.BadVersions == nil {
		s.BadVersions = map[string]string{}
	}

	if s.BadBuilds == nil {
		s.BadBuilds = map[string]string{}
	}

	build := n.activeBuild()
	if build.Version == "" {
		build = api.ApplicationNVIDIABuild{Branch: n.requestedBranch(), Version: n.appState.Version}
	}

	// A good previous build may be a rebuilt pack of the same driver version, so quarantining the
	// whole version would also block the rollback target. Only the failed build is quarantined then.
	goodPrevious := s.Previous != nil && s.Previous.Version != "" && *s.Previous != build && s.BadBuilds[n.buildKey(*s.Previous)] == ""
	if !goodPrevious {
		s.BadVersions[n.attemptKey(boot.Version)] = reason
	}

	if build.Version != "" {
		s.BadBuilds[n.buildKey(build)] = reason
	}

	s.PendingAttempt, s.AttemptBootID, s.PendingHealth, s.HealthBootID = "", "", "", ""

	err := n.state.SaveDurable()
	if err != nil {
		return errors.New("NVIDIA recovery state could not be saved; reboot the host")
	}

	if s.Previous != nil && s.Previous.Version != "" {
		return n.rollback(ctx, boot, reason)
	}

	id, idErr := n.bootID()
	previous := s.FallbackBootID
	reboot := idErr == nil && boot.Attempted && len(boot.Cards) > 0 && previous != id

	note := n.restoreNouveau(ctx, boot.Cards, reboot)

	s.ActiveDriver, s.DriverVersion, s.Reason = "nouveau", "", withNVIDIANote(reason, note)

	if idErr != nil {
		s.Reason = "NVIDIA recovery could not identify the current boot; reboot the host"

		return n.state.SaveDurable()
	}

	if reboot {
		s.FallbackBootID = id
		s.NeedsReboot = true
		n.state.System.Update.State.NeedsReboot = true
	}

	err = n.state.SaveDurable()
	if err != nil {
		s.FallbackBootID = previous
		s.Reason = "NVIDIA recovery state could not be saved; reboot the host"

		return errors.New(s.Reason)
	}

	if reboot {
		_, err = n.command(ctx, "systemctl", "--no-block", "reboot")
		if err != nil {
			s.Reason = "NVIDIA recovery needs a reboot"
		}
	}

	return nil
}

// PrepareGPUs applies per-address ownership before explicitly loading Nouveau or NVIDIA.
func PrepareGPUs(ctx context.Context, s *state.State) error {
	branch := s.Applications.NVIDIA.State.RequestedBranch
	if branch == "" {
		branch = nvidiaStable
	}

	app, err := Load(ctx, s, branch)
	if err != nil {
		return err
	}

	n, ok := app.(*nvidia)
	if !ok {
		return errors.New("unexpected NVIDIA application type")
	}

	return n.prepare(ctx, n.IsInstalled())
}
