package applications

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

type nvidiaLiveSwitchKey struct{}

// WithNVIDIALiveSwitch permits live driver changes for an explicit application API request.
func WithNVIDIALiveSwitch(ctx context.Context, name string) context.Context {
	if name != nvidiaStable && name != nvidiaLTS {
		return ctx
	}

	return context.WithValue(ctx, nvidiaLiveSwitchKey{}, true)
}

var nvidiaModules = []string{"nvidia_peermem", "nvidia_drm", "nvidia_uvm", "nvidia_modeset", "nvidia"}

var (
	errNVIDIAModulesBusy  = errors.New("NVIDIA modules are still in use")
	errNVIDIAVerification = errors.New("the requested NVIDIA pack could not be verified")
	errNVIDIABind         = errors.New("NVIDIA driver could not bind")
	errNVIDIAHealth       = errors.New("NVIDIA health check failed")
	errNVIDIARestore      = errors.New("the previous NVIDIA driver could not be reloaded; restart to restore it")
	errNVIDIAOtherDriver  = errors.New("the card is using another driver; restart to switch it to NVIDIA")
	errNVIDIANouveau      = errors.New("the Nouveau fallback driver could not take over the GPU; restart to finish")
)

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

	find := n.findExtension
	if find == nil {
		find = systemd.FindNVIDIAExtension
	}

	version, versions, reason := find(ctx, n.requestedBranch(), n.appState.Version, true)

	s.AvailableVersions = versions
	if version == "" && !n.state.Applications.NVIDIA.Config.PinCurrentVersions {
		version, versions, reason = find(ctx, n.requestedBranch(), n.appState.Version, false)
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

	live, explicit := ctx.Value(nvidiaLiveSwitchKey{}).(bool)
	if !explicit || !live {
		return n.deferSwitch("a new NVIDIA driver is ready; restart to apply the update")
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

func (n *nvidia) switchBuild(ctx context.Context, target api.ApplicationNVIDIABuild) (result error) {
	if n.busy() {
		return n.deferSwitch("NVIDIA devices are in use; the driver switch will run at the next boot")
	}

	if n.modulesBusy() {
		return n.deferSwitch("NVIDIA modules are still in use; the driver switch will run at the next boot")
	}

	other, err := n.usingAnotherDriver(ctx, target)
	if err != nil {
		return n.deferSwitch("GPU use could not be inspected; restart to apply the NVIDIA driver")
	}

	if other {
		return n.deferSwitch(errNVIDIAOtherDriver.Error())
	}

	s := &n.state.Applications.NVIDIA.State
	previous := n.activeBuild()
	retainedPrevious := s.Previous

	n.rememberPrevious(previous)

	err = n.state.SaveDurable()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	changed := false
	defer func() {
		if changed {
			n.refreshOnError(ctx, &result)
		}
	}()

	err = n.activateBuild(ctx, target)
	if errors.Is(err, errNVIDIAVerification) {
		s.Previous = retainedPrevious

		s.Reason = err.Error() + "; the active NVIDIA driver was left unchanged"

		saveErr := n.state.SaveDurable()
		if saveErr != nil {
			return errors.Join(err, saveErr)
		}

		return nil
	}

	changed = true

	if errors.Is(err, errNVIDIARestore) {
		return n.deferSwitch(errNVIDIARestore.Error())
	}

	if errors.Is(err, errNVIDIAModulesBusy) {
		return n.deferSwitch("NVIDIA devices or modules are still in use; the driver switch will run at the next boot")
	}

	if err != nil {
		return n.deferSwitch("NVIDIA switch was deferred: " + err.Error())
	}

	err = n.checkBuild(ctx)
	if err != nil {
		return n.recoverSwitch(ctx, target, previous, err)
	}

	s.NeedsReboot = false

	return n.state.SaveDurable()
}

func (n *nvidia) recoverSwitch(ctx context.Context, target api.ApplicationNVIDIABuild, previous api.ApplicationNVIDIABuild, failure error) error {
	if errors.Is(failure, errNVIDIAOtherDriver) {
		return n.deferSwitch(errNVIDIAOtherDriver.Error())
	}

	s := &n.state.Applications.NVIDIA.State
	reason := "NVIDIA switch failed: " + failure.Error()

	quarantine := errors.Is(failure, errNVIDIABind) || errors.Is(failure, errNVIDIAHealth)
	if quarantine {
		if s.BadBuilds == nil {
			s.BadBuilds = map[string]string{}
		}

		s.BadBuilds[n.buildKey(target)] = reason

		err := n.state.SaveDurable()
		if err != nil {
			return err
		}
	}

	if previous.Version != "" && n.restorePreviousBuild(ctx, previous) == nil {
		s.Reason = reason + "; restored the previous driver from disk"
		if !quarantine {
			return n.deferSwitch(s.Reason)
		}

		s.NeedsReboot = false

		return n.state.SaveDurable()
	}

	return n.deferRollback(ctx, reason, false)
}

func (n *nvidia) restorePreviousBuild(ctx context.Context, previous api.ApplicationNVIDIABuild) error {
	repairCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()

	err := n.activateBuild(repairCtx, previous)
	if err != nil {
		return err
	}

	return n.checkBuild(repairCtx)
}

// usingAnotherDriver reports whether a card the target build supports is held by another driver.
func (n *nvidia) usingAnotherDriver(ctx context.Context, target api.ApplicationNVIDIABuild) (bool, error) {
	if n.state.Applications.NVIDIA.Config.Driver != "nvidia" {
		return false, nil
	}

	cards, err := n.cards()
	if err != nil {
		return false, err
	}

	others := []nvidiaCard{}

	for _, card := range cards {
		use := n.state.Applications.NVIDIA.Config.Cards[card.address]
		if n.passthrough(card) || use == "off" || use == "vfio-pci" {
			continue
		}

		if card.driver != "." && card.driver != "nvidia" {
			others = append(others, card)
		}
	}

	if len(others) == 0 {
		return false, nil
	}

	// Older cards stay on Nouveau for good, so only the cards this build drives can need a restart.
	image := n.path(filepath.Join(systemd.LocalExtensionsPath, target.Version, target.Branch+".raw"))

	supported, err := n.command(ctx, "systemd-dissect", "--copy-from", image, "/usr/share/nvidia/supported-gpus")
	if err != nil {
		return false, err
	}

	for _, card := range others {
		if slices.Contains(strings.Fields(supported), card.id) {
			return true, nil
		}
	}

	return false, nil
}

type nvidiaExtensionFiles struct {
	mkdirAll func(string, os.FileMode) error
	remove   func(string) error
	symlink  func(string, string) error
	rename   func(string, string) error
}

func (n *nvidia) files() nvidiaExtensionFiles {
	if n.extensionFiles != nil {
		return *n.extensionFiles
	}

	return nvidiaExtensionFiles{os.MkdirAll, os.Remove, os.Symlink, os.Rename}
}

func (n *nvidia) refresh(ctx context.Context) error {
	if n.refreshExtensions != nil {
		return n.refreshExtensions(ctx)
	}

	return systemd.RefreshExtensions(ctx)
}

func (n *nvidia) refreshOnError(ctx context.Context, result *error) {
	if *result == nil {
		return
	}

	repairCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()

	*result = errors.Join(*result, n.refresh(repairCtx))
}

func (n *nvidia) detachNVIDIA(ctx context.Context) error {
	for _, branch := range []string{nvidiaStable, nvidiaLTS} {
		err := n.files().remove(n.path(filepath.Join(systemd.SystemExtensionsPath, branch+".raw")))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	return n.refresh(ctx)
}

func (n *nvidia) modulesBusy() bool {
	for _, module := range nvidiaModules {
		body, err := os.ReadFile(n.path("/sys/module/" + module + "/refcnt"))
		if errors.Is(err, os.ErrNotExist) {
			_, moduleErr := os.Stat(n.path("/sys/module/" + module))
			if errors.Is(moduleErr, os.ErrNotExist) {
				continue
			}

			return true
		}

		if err != nil {
			return true
		}

		refs, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil || refs < 0 {
			return true
		}

		holders, err := os.ReadDir(n.path("/sys/module/" + module + "/holders"))
		if err != nil {
			return true
		}

		for _, holder := range holders {
			if !slices.Contains(nvidiaModules, holder.Name()) || holder.Name() == module || holder.Type()&os.ModeSymlink == 0 {
				return true
			}

			target, err := filepath.EvalSymlinks(n.path("/sys/module/" + module + "/holders/" + holder.Name()))
			if err != nil || target != n.path("/sys/module/"+holder.Name()) {
				return true
			}
		}

		if refs != len(holders) {
			return true
		}
	}

	return false
}

func (n *nvidia) unloadModules(ctx context.Context) (bool, error) {
	changed := false
	if n.busy() || n.modulesBusy() {
		return changed, errNVIDIAModulesBusy
	}

	for _, module := range nvidiaModules {
		_, err := os.Stat(n.path("/sys/module/" + module))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return changed, err
		}

		_, err = n.command(ctx, "/sbin/rmmod", module)
		if err != nil {
			return changed, errNVIDIAModulesBusy
		}

		changed = true
	}

	return changed, nil
}

func (n *nvidia) activateBuild(ctx context.Context, build api.ApplicationNVIDIABuild) (result error) {
	image := n.path(filepath.Join(systemd.LocalExtensionsPath, build.Version, build.Branch+".raw"))

	reason := n.state.Applications.NVIDIA.State.BadBuilds[n.buildKey(build)]
	if reason != "" {
		return fmt.Errorf("%w: %s", errNVIDIAVerification, reason)
	}

	err := n.validateImage(ctx, image)
	if err != nil {
		return fmt.Errorf("%w: %w", errNVIDIAVerification, err)
	}

	if n.modulesBusy() {
		return errNVIDIAModulesBusy
	}

	previous := n.activeBuild()
	modulesChanged := false

	defer func() {
		if result != nil {
			n.refreshOnError(ctx, &result)

			repairCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
			defer cancel()

			restoreErr := n.restoreBuildLink(repairCtx, previous)
			if modulesChanged && previous.Version != "" {
				state := &n.state.Applications.NVIDIA.State
				state.ActiveDriver = ""

				if restoreErr == nil {
					_, restoreErr = n.command(repairCtx, "/sbin/ldconfig")
				}

				if restoreErr == nil {
					restoreErr = n.checkBuild(repairCtx)
				}

				if restoreErr != nil {
					clearNVIDIAAttempts(state)
					state.NeedsReboot, state.Reason = true, errNVIDIARestore.Error()
					n.state.System.Update.State.NeedsReboot = true
					restoreErr = errors.Join(errNVIDIARestore, restoreErr, n.state.SaveDurable())
				}
			}

			result = errors.Join(result, restoreErr)
			n.refreshOnError(repairCtx, &result)
		}
	}()

	err = n.detachNVIDIA(ctx)
	if err != nil {
		return err
	}

	state := &n.state.Applications.NVIDIA.State
	activeDriver := state.ActiveDriver
	state.ActiveDriver = ""

	modulesChanged, err = n.unloadModules(ctx)
	if err != nil {
		if !modulesChanged {
			state.ActiveDriver = activeDriver
		}

		return err
	}

	link := n.path(filepath.Join(systemd.SystemExtensionsPath, build.Branch+".raw"))
	files := n.files()

	err = files.mkdirAll(filepath.Dir(link), 0o700)
	if err != nil {
		return err
	}

	err = files.remove(link + ".new")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	err = files.symlink(image, link+".new")
	if err != nil {
		return err
	}

	err = files.rename(link+".new", link)
	if err != nil {
		return err
	}

	err = n.refresh(ctx)
	if err != nil {
		return err
	}

	s := &n.state.Applications.NVIDIA.State

	_, err = n.command(ctx, "/sbin/ldconfig")
	if err != nil {
		return err
	}

	s.ActiveBranch, s.ActiveExtensionVersion = build.Branch, build.Version

	return nil
}

func (n *nvidia) restoreBuildLink(ctx context.Context, build api.ApplicationNVIDIABuild) error {
	for _, branch := range []string{nvidiaStable, nvidiaLTS} {
		err := n.files().remove(n.path(filepath.Join(systemd.SystemExtensionsPath, branch+".raw")))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	if build.Version != "" {
		link := n.path(filepath.Join(systemd.SystemExtensionsPath, build.Branch+".raw"))
		image := n.path(filepath.Join(systemd.LocalExtensionsPath, build.Version, build.Branch+".raw"))

		err := n.files().mkdirAll(filepath.Dir(link), 0o700)
		if err != nil {
			return err
		}

		err = n.files().symlink(image, link)
		if err != nil {
			return err
		}
	}

	return n.refresh(ctx)
}

// PrepareUpdate clears failures only after the update pipeline has verified and saved the new pack.
func (n *nvidia) PrepareUpdate(ctx context.Context) error {
	image := n.path(filepath.Join(systemd.LocalExtensionsPath, n.appState.Version, n.requestedBranch()+".raw"))

	driver, err := n.command(ctx, "systemd-dissect", "--copy-from", image, "/usr/share/nvidia/version")
	if err != nil {
		return err
	}

	driver = strings.TrimSpace(driver)
	if driver == "" {
		return errors.New("the updated NVIDIA driver version is missing")
	}

	n.clearBadDriverVersion(driver)
	delete(n.state.Applications.NVIDIA.State.BadBuilds, n.buildKey(api.ApplicationNVIDIABuild{Branch: n.requestedBranch(), Version: n.appState.Version}))

	return n.state.SaveDurable()
}

func (n *nvidia) rememberPrevious(build api.ApplicationNVIDIABuild) {
	s := &n.state.Applications.NVIDIA.State
	if build.Version == "" || s.BadBuilds[n.buildKey(build)] != "" || s.BadVersions[n.attemptKey(s.DriverVersion)] != "" || s.PendingAttempt != "" || s.PendingHealth != "" {
		return
	}

	s.Previous = &build
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

	reason := s.BadBuilds[n.buildKey(n.activeBuild())]
	if reason == "" {
		reason = s.BadVersions[n.attemptKey(version)]
	}

	if reason != "" {
		return errors.New(reason)
	}

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
			return errNVIDIAOtherDriver
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
	if err != nil {
		return err
	}

	err = n.bindCards(assigned)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		return fmt.Errorf("%w: %w", errNVIDIABind, err)
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
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		return fmt.Errorf("%w: %w", errNVIDIAHealth, err)
	}

	s.PendingHealth, s.HealthBootID = "", ""
	forgetNVIDIAAttemptBuild(s)

	n.clearBadVersion()

	s.ActiveDriver, s.Reason = "nvidia", ""

	return nil
}

// rollback runs only during boot recovery. It selects the previous build for the next boot instead of
// restoring it live: after a GPU hang, unloading NVIDIA can block in the kernel and stop the reboot.
// Live restores, which unload the modules, stay limited to runtime switches.
func (n *nvidia) rollback(ctx context.Context, boot nvidiaBoot, reason string) error {
	s := &n.state.Applications.NVIDIA.State
	if s.BadBuilds == nil {
		s.BadBuilds = map[string]string{}
	}

	s.BadBuilds[n.buildKey(n.activeBuild())] = reason

	err := n.state.SaveDurable()
	if err != nil {
		return err
	}

	s.Recovery = &api.ApplicationNVIDIARecovery{Build: *s.Previous, Reason: "the NVIDIA driver " + nvidiaBuildLabel(n.activeBuild()) + " failed at startup; Arca went back to " + nvidiaBuildLabel(*s.Previous)}
	s.RequestedBranch, s.Version = s.Previous.Branch, s.Previous.Version

	return n.deferRollback(ctx, withNVIDIANote(reason, n.restoreNouveau(ctx, boot.Cards, true)), true)
}

func nvidiaBuildLabel(build api.ApplicationNVIDIABuild) string {
	return build.Branch + " " + build.Version
}

func withNVIDIANote(reason string, note string) string {
	if note == "" {
		return reason
	}

	return reason + "; " + note
}

// restoreNouveau hands the cards back to Nouveau and returns a plain reason when it could not.
// With keepModules it only records the choice, for a reboot that follows: after a GPU hang,
// unloading NVIDIA can block in the kernel and would stop the reboot from ever being issued.
func (n *nvidia) restoreNouveau(ctx context.Context, cards []string, keepModules bool) string {
	n.state.Applications.NVIDIA.State.ActiveDriver = "nouveau"
	n.state.Applications.NVIDIA.State.DriverVersion = ""

	failed := false

	for _, address := range cards {
		err := os.WriteFile(n.path("/sys/bus/pci/devices/"+address+"/driver_override"), []byte("nouveau"), 0o600)
		if err != nil {
			failed = true
		}
	}

	if keepModules {
		return ""
	}

	repairCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()

	_, err := n.unloadModules(repairCtx)
	if err != nil {
		return n.nouveauFailed()
	}

	if !gpuModuleBlacklisted("nouveau", n.state.System.Kernel.Config.BlacklistModules) {
		_, err = n.command(repairCtx, "/sbin/modprobe", "nouveau")
		if err != nil {
			return n.nouveauFailed()
		}

		// Nouveau is usually loaded already, so modprobe alone does not make it take the card.
		for _, address := range cards {
			err := os.WriteFile(n.path("/sys/bus/pci/drivers_probe"), []byte(address), 0o600)
			if err != nil {
				failed = true
			}
		}
	}

	if failed {
		return n.nouveauFailed()
	}

	return ""
}

// nouveauFailed asks for the restart that the "restart to finish" reason promises.
func (n *nvidia) nouveauFailed() string {
	n.state.Applications.NVIDIA.State.NeedsReboot = true
	n.state.System.Update.State.NeedsReboot = true

	return errNVIDIANouveau.Error()
}

func (n *nvidia) recoveryReason() string {
	s := &n.state.Applications.NVIDIA.State
	requested := api.ApplicationNVIDIABuild{Branch: n.requestedBranch(), Version: s.Version}

	// A boot-time return to the previous build stays explained while that build is requested and running.
	if s.Recovery != nil && s.Recovery.Build != requested {
		s.Recovery = nil
	}

	if s.Recovery != nil && n.activeBuild() == s.Recovery.Build && s.ActiveDriver == "nvidia" {
		return s.Recovery.Reason
	}

	reason := s.BadBuilds[n.buildKey(requested)]
	if reason != "" && s.Previous != nil && n.activeBuild() == *s.Previous && s.ActiveExtensionVersion != "" && s.ActiveDriver == "nvidia" {
		return reason + "; restored the previous driver from disk"
	}

	return reason
}

func (n *nvidia) deferRollback(ctx context.Context, reason string, boot bool) (result error) {
	if !boot || n.state.OS.SystemIsReady || !n.bootRecovery {
		repairCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()

		var cleanupErr error

		if !n.busy() && !n.modulesBusy() {
			defer func() { n.refreshOnError(ctx, &result) }()

			cleanupErr = n.detachNVIDIA(repairCtx)
			if cleanupErr == nil {
				n.state.Applications.NVIDIA.State.ActiveBranch = ""
				n.state.Applications.NVIDIA.State.ActiveExtensionVersion = ""

				_, cleanupErr = n.unloadModules(repairCtx)
				if cleanupErr == nil {
					n.state.Applications.NVIDIA.State.ActiveDriver = ""
					n.state.Applications.NVIDIA.State.DriverVersion = ""
				}
			}
		}

		s := &n.state.Applications.NVIDIA.State

		clearNVIDIAAttempts(s)

		if s.Previous != nil {
			s.RequestedBranch, s.Version = s.Previous.Branch, s.Previous.Version

			return errors.Join(cleanupErr, n.deferSwitch("the new NVIDIA driver failed its check; restart to go back to the previous driver"))
		}

		return errors.Join(cleanupErr, n.deferSwitch("the new NVIDIA driver failed its check; restart to use the fallback driver"))
	}

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
