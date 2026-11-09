package applications

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

var errNVIDIAModulesBusy = errors.New("NVIDIA modules are still in use")

func (n *nvidia) requestedBranch() string {
	branch := n.state.Applications.NVIDIA.State.RequestedBranch
	if branch == "" {
		return nvidiaStable
	}

	return branch
}

func (n *nvidia) Version() string {
	if n.variant != n.requestedBranch() {
		return ""
	}

	return n.appState.Version
}

func (n *nvidia) SetVersions(version string, versions []string) {
	n.state.Applications.NVIDIA.State.RequestedBranch = n.variant
	if n.state.Applications.NVIDIA.Config.Driver == "" {
		n.state.Applications.NVIDIA.Config.Driver = "nvidia"
	}

	n.common.SetVersions(version, versions)
}

func (n *nvidia) SwitchVersion(version string) error {
	previous := n.state.Applications.NVIDIA.State.Previous
	if version == "" && previous != nil {
		_, err := os.Stat(n.path(filepath.Join(systemd.LocalExtensionsPath, previous.Version, previous.Branch+".raw")))
		if err != nil {
			return errors.New("the previous NVIDIA driver is no longer available on disk")
		}

		n.state.Applications.NVIDIA.State.RequestedBranch = previous.Branch
		n.appState.Version = previous.Version

		return nil
	}

	return n.common.SwitchVersion(version)
}

func (n *nvidia) deferSwitch(reason string) error {
	s := &n.state.Applications.NVIDIA.State
	s.NeedsReboot, s.Reason = true, reason

	return n.requireReboot()
}

func (n *nvidia) buildKey(build api.ApplicationNVIDIABuild) string {
	return n.state.OS.RunningRelease + ":" + build.Branch + ":" + build.Version
}

func (n *nvidia) activeBuild() api.ApplicationNVIDIABuild {
	s := &n.state.Applications.NVIDIA.State

	branch := s.ActiveBranch
	if branch == "" {
		branch = nvidiaStable
	}

	return api.ApplicationNVIDIABuild{Branch: branch, Version: s.ActiveExtensionVersion}
}

// reconcile selects the requested build while preserving the running pack until it can be replaced safely.
func (n *nvidia) reconcile(ctx context.Context) error {
	s := &n.state.Applications.NVIDIA.State
	version, versions, reason := systemd.FindNVIDIAExtension(ctx, n.requestedBranch(), n.appState.Version, true)

	s.AvailableVersions = versions
	if version == "" && !n.state.Applications.NVIDIA.Config.PinCurrentVersions {
		version, versions, reason = systemd.FindNVIDIAExtension(ctx, n.requestedBranch(), n.appState.Version, false)
		s.AvailableVersions = versions
	}

	if version == "" {
		s.Reason = reason

		return n.state.SaveDurable()
	}

	target := api.ApplicationNVIDIABuild{Branch: n.requestedBranch(), Version: version}

	reason = s.BadBuilds[n.buildKey(target)]
	if reason != "" {
		s.Reason = n.recoveryReason()

		return n.state.SaveDurable()
	}

	if target == n.activeBuild() {
		return n.state.SaveDurable()
	}

	return n.switchBuild(ctx, target)
}

// busy fails closed when process file descriptors cannot be inspected.
func (n *nvidia) busy() bool {
	processes, err := os.ReadDir(n.path("/proc"))
	if err != nil {
		return true
	}

	for _, process := range processes {
		_, err := strconv.Atoi(process.Name())
		if err != nil {
			continue
		}

		directory := n.path("/proc/" + process.Name() + "/fd")

		descriptors, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return true
		}

		for _, descriptor := range descriptors {
			target, err := os.Readlink(filepath.Join(directory, descriptor.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			if err != nil || strings.HasPrefix(target, "/dev/nvidia") {
				return true
			}
		}
	}

	return false
}

func (n *nvidia) switchBuild(ctx context.Context, target api.ApplicationNVIDIABuild) error {
	if n.busy() {
		return n.deferSwitch("NVIDIA devices are in use; the driver switch will run at the next boot")
	}

	s := &n.state.Applications.NVIDIA.State

	previous := n.activeBuild()
	if previous.Version != "" {
		s.Previous = &previous
	}

	err := n.state.SaveDurable()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	err = n.activateBuild(ctx, target)
	if err == nil {
		err = n.checkBuild(ctx)
	}

	if errors.Is(err, errNVIDIAModulesBusy) {
		return n.deferSwitch("NVIDIA modules are still in use; the driver switch will run at the next boot")
	}

	if err != nil {
		reason := "NVIDIA switch failed: " + err.Error()

		if s.BadBuilds == nil {
			s.BadBuilds = map[string]string{}
		}

		s.BadBuilds[n.buildKey(target)] = reason

		saveErr := n.state.SaveDurable()
		if saveErr != nil {
			return saveErr
		}

		if previous.Version != "" {
			rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), 120*time.Second)
			defer rollbackCancel()

			rollbackErr := n.activateBuild(rollbackCtx, previous)
			if rollbackErr == nil {
				rollbackErr = n.checkBuild(rollbackCtx)
			}

			if rollbackErr == nil {
				s.Reason = reason + "; restored the previous driver from disk"
				s.NeedsReboot = false

				return n.state.SaveDurable()
			}
		}

		return n.deferRollback(ctx, reason)
	}

	s.NeedsReboot = false

	return n.state.SaveDurable()
}

func (n *nvidia) activateBuild(ctx context.Context, build api.ApplicationNVIDIABuild) error {
	image := n.path(filepath.Join(systemd.LocalExtensionsPath, build.Version, build.Branch+".raw"))

	err := n.validateImage(ctx, image)
	if err != nil {
		return errors.New("the requested NVIDIA pack could not be verified for this kernel")
	}

	_, err = n.command(ctx, "systemd-sysext", "unmerge")
	if err != nil {
		return errors.New("the current NVIDIA extension could not be unmerged")
	}

	for _, module := range []string{"nvidia_peermem", "nvidia_drm", "nvidia_uvm", "nvidia_modeset", "nvidia"} {
		_, err := os.Stat(n.path("/sys/module/" + module))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err == nil {
			_, err = n.command(ctx, "/sbin/rmmod", module)
		}

		if err != nil {
			_, _ = n.command(ctx, "systemd-sysext", "refresh")

			return errNVIDIAModulesBusy
		}
	}

	link := n.path(filepath.Join(systemd.SystemExtensionsPath, build.Branch+".raw"))

	err = os.MkdirAll(filepath.Dir(link), 0o700)
	if err != nil {
		return err
	}

	_ = os.Remove(link + ".new")

	err = os.Symlink(image, link+".new")
	if err != nil {
		return err
	}

	err = os.Rename(link+".new", link)
	if err != nil {
		return err
	}

	other := nvidiaStable
	if build.Branch == other {
		other = nvidiaLTS
	}

	err = os.Remove(n.path(filepath.Join(systemd.SystemExtensionsPath, other+".raw")))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	_, err = n.command(ctx, "systemd-sysext", "refresh")
	if err != nil {
		return errors.New("the requested NVIDIA extension could not be merged")
	}

	s := &n.state.Applications.NVIDIA.State
	s.ActiveBranch, s.ActiveExtensionVersion = build.Branch, build.Version
	_, err = n.command(ctx, "/sbin/ldconfig")

	return err
}

func (n *nvidia) validateImage(ctx context.Context, image string) error {
	if n.verifyImage != nil {
		return n.verifyImage(ctx, image)
	}

	return systemd.ValidateNVIDIAExtension(ctx, image)
}

func (n *nvidia) checkBuild(ctx context.Context) error {
	version, err := n.validate(ctx)
	if err != nil {
		return err
	}

	s := &n.state.Applications.NVIDIA.State

	s.DriverVersion = version
	if n.state.Applications.NVIDIA.Config.Driver != "nvidia" || gpuModuleBlacklisted("nvidia", n.state.System.Kernel.Config.BlacklistModules) {
		s.ActiveDriver, s.Reason = "nouveau", "the NVIDIA driver is disabled by configuration"

		return nil
	}

	cards, err := n.cards()
	if err != nil {
		return err
	}

	assigned := []string{}

	supported, err := os.ReadFile(n.path("/usr/share/nvidia/supported-gpus"))
	if err != nil {
		return err
	}

	for _, card := range cards {
		use := n.state.Applications.NVIDIA.Config.Cards[card.address]
		if n.passthrough(card) || use == "vfio-pci" || use == "off" {
			continue
		}

		if !strings.Contains("\n"+string(supported), "\n"+card.id+"\n") {
			continue
		}

		if card.driver != "." && card.driver != "nvidia" {
			return errNVIDIAModulesBusy
		}

		err := os.WriteFile(n.path("/sys/bus/pci/devices/"+card.address+"/driver_override"), []byte("nvidia"), 0o600)
		if err != nil {
			return err
		}

		assigned = append(assigned, card.address)
	}

	if len(assigned) == 0 {
		s.ActiveDriver, s.Reason = "nouveau", "no NVIDIA GPU"

		return nil
	}

	err = n.verifyModules(ctx)
	if err != nil {
		return err
	}

	err = n.saveAttempt(version)
	if err != nil {
		return err
	}

	err = n.loadModules(ctx)
	if err == nil {
		err = n.bindCards(assigned)
	}

	if err != nil {
		return fmt.Errorf("NVIDIA driver could not bind: %w", err)
	}

	err = n.markBound()
	if err != nil {
		return err
	}

	err = n.saveHealthAttempt(version)
	if err != nil {
		return err
	}

	err = n.health(ctx, assigned)
	if err != nil {
		return err
	}

	s.PendingHealth, s.HealthBootID = "", ""

	n.clearBadVersion()

	s.ActiveDriver, s.Reason = "nvidia", ""

	return nil
}

func (n *nvidia) rollback(ctx context.Context, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	s := &n.state.Applications.NVIDIA.State
	if s.BadBuilds == nil {
		s.BadBuilds = map[string]string{}
	}

	s.BadBuilds[n.buildKey(n.activeBuild())] = reason

	err := n.state.SaveDurable()
	if err != nil {
		return err
	}

	if n.busy() {
		return n.deferRollback(ctx, reason)
	}

	err = n.activateBuild(ctx, *s.Previous)
	if err == nil {
		err = n.checkBuild(ctx)
	}

	if err != nil {
		return n.deferRollback(ctx, reason)
	}

	s.NeedsReboot, s.Reason = false, reason+"; restored the previous driver from disk"

	return n.state.SaveDurable()
}

func (n *nvidia) recoveryReason() string {
	s := &n.state.Applications.NVIDIA.State
	requested := api.ApplicationNVIDIABuild{Branch: n.requestedBranch(), Version: s.Version}

	reason := s.BadBuilds[n.buildKey(requested)]
	if reason != "" && s.Previous != nil && n.activeBuild() == *s.Previous && s.ActiveExtensionVersion != "" {
		return reason + "; restored the previous driver from disk"
	}

	return reason
}

func (n *nvidia) deferRollback(ctx context.Context, reason string) error {
	err := n.deferSwitch(reason + "; reboot to restore the previous driver")
	if err != nil {
		return err
	}

	s := &n.state.Applications.NVIDIA.State

	id, err := n.bootID()
	if err != nil {
		return err
	}

	if s.FallbackBootID == id {
		return nil
	}

	s.FallbackBootID = id

	err = n.state.SaveDurable()
	if err != nil {
		return err
	}

	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	_, err = n.command(recoveryCtx, "systemctl", "--no-block", "reboot")
	if err != nil {
		s.Reason = reason + "; automatic recovery needs a reboot"

		return n.state.SaveDurable()
	}

	return nil
}
