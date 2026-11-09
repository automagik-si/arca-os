package applications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

type nvidiaCard struct {
	address string
	id      string
	driver  string
}

func (n *nvidia) cards() ([]nvidiaCard, error) {
	entries, err := os.ReadDir(n.path("/sys/bus/pci/devices"))
	if err != nil {
		return nil, errors.New("PCI devices could not be inspected")
	}

	cards := []nvidiaCard{}

	var readError error

	for _, entry := range entries {
		path := n.path("/sys/bus/pci/devices/" + entry.Name())
		vendor, vendorErr := os.ReadFile(filepath.Join(path, "vendor"))
		class, classErr := os.ReadFile(filepath.Join(path, "class"))

		id, idErr := os.ReadFile(filepath.Join(path, "device"))
		if vendorErr != nil || classErr != nil || idErr != nil {
			readError = errors.New("some PCI devices could not be inspected")

			continue
		}

		if strings.TrimSpace(string(vendor)) != "0x10de" {
			continue
		}

		if !slices.Contains([]string{"0x030000", "0x030200"}, strings.TrimSpace(string(class))) {
			continue
		}

		driver, err := os.Readlink(filepath.Join(path, "driver"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			readError = errors.New("a GPU driver could not be inspected")

			continue
		}

		cards = append(cards, nvidiaCard{entry.Name(), strings.TrimPrefix(strings.TrimSpace(string(id)), "0x"), filepath.Base(driver)})
	}

	return cards, readError
}

func (n *nvidia) passthrough(card nvidiaCard) bool {
	if card.driver == "vfio-pci" {
		return true
	}

	pci := n.state.System.Kernel.Config.PCI
	if pci == nil {
		return false
	}

	return slices.ContainsFunc(pci.Passthrough, func(p api.SystemKernelConfigPCIPassthrough) bool {
		return (p.PCIAddress != "" && strings.EqualFold(p.PCIAddress, card.address)) || (p.PCIAddress == "" && strings.EqualFold(p.VendorID, "10de") && strings.EqualFold(p.ProductID, card.id))
	})
}

func (n *nvidia) prepare(ctx context.Context, installed bool) (result error) {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()

	boot := nvidiaBoot{}

	for _, branch := range []string{nvidiaStable, nvidiaLTS} {
		target, err := os.Readlink(n.path("/var/lib/extensions/" + branch + ".raw"))
		if err == nil {
			boot.ExtensionVersion = filepath.Base(filepath.Dir(target))

			old := n.activeBuild()
			if old.Version != "" && (old.Version != boot.ExtensionVersion || old.Branch != branch) {
				n.rememberPrevious(old)
			}

			n.state.Applications.NVIDIA.State.ActiveBranch = branch
			n.state.Applications.NVIDIA.State.ActiveExtensionVersion = boot.ExtensionVersion
		}
	}

	completed := false
	defer func() {
		if !completed {
			boot.Reason = "NVIDIA startup was interrupted"
			boot.Failure = boot.Attempted
		}

		err := n.finishBoot(ctx, &boot)
		if result == nil {
			result = err
		}
	}()

	config := &n.state.Applications.NVIDIA.Config
	if installed && config.Driver == "" {
		config.Driver = "nvidia"
	}

	normalised := map[string]string{}
	for address, use := range config.Cards {
		normalised[normaliseNVIDIAAddress(address)] = use
	}

	config.Cards = normalised
	boot.Config = *config

	boot.Reason = n.checkPreviousAttempt()
	if installed && config.Driver == "nvidia" && boot.Reason == "" {
		version, err := n.validate(ctx)

		boot.Version = version
		if err != nil {
			boot.Reason = err.Error()
		}

		reason := n.state.Applications.NVIDIA.State.BadVersions[n.attemptKey(version)]
		if reason != "" {
			boot.Reason = reason
		}
	}

	if !installed && config.Driver == "nvidia" {
		boot.Reason = systemd.NVIDIAMissingReason
	}

	cards, err := n.cards()
	if err != nil {
		boot.Reason = "some PCI devices could not be inspected"
	}

	supported, _ := os.ReadFile(n.path("/usr/share/nvidia/supported-gpus"))
	vfioCards := []string{}

	for _, card := range cards {
		if card.driver != "." {
			continue
		}

		use := config.Cards[card.address]
		if n.passthrough(card) {
			use = "vfio-pci"
		}

		if use == "" || use == "nvidia" {
			use = "nouveau"
			if installed && config.Driver == "nvidia" && boot.Reason == "" && slices.Contains(strings.Fields(string(supported)), card.id) && !gpuModuleBlacklisted("nvidia", n.state.System.Kernel.Config.BlacklistModules) {
				use = "nvidia"
			}
		}

		if use == "off" {
			use = "none"
		}

		err := os.WriteFile(n.path("/sys/bus/pci/devices/"+card.address+"/driver_override"), []byte(use), 0o600)
		if err != nil {
			boot.Reason = "a GPU driver choice could not be applied"

			continue
		}

		switch use {
		case "vfio-pci":
			vfioCards = append(vfioCards, card.address)
		case "nvidia":
			boot.Cards = append(boot.Cards, card.address)
		default:
		}
	}

	_, err = n.command(ctx, "/sbin/ldconfig")
	if err != nil {
		boot.Reason = "the GPU library cache could not be refreshed"
	}

	err = n.writeModprobeConfig()
	if err != nil {
		boot.Reason = "the GPU module configuration could not be written"
	}

	if len(vfioCards) > 0 {
		_, err = n.command(ctx, "/sbin/modprobe", "vfio-pci")
		if err != nil {
			boot.Reason = "PCI passthrough is unavailable; check that IOMMU is enabled"
		} else {
			for _, address := range vfioCards {
				err := os.WriteFile(n.path("/sys/bus/pci/drivers_probe"), []byte(address), 0o600)
				if err != nil {
					boot.Reason = "a GPU could not be assigned to PCI passthrough"
				}
			}
		}
	}

	if len(boot.Cards) > 0 && boot.Reason == "" {
		n.startAttempt(ctx, &boot)
	}

	if installed && config.Driver == "nvidia" && boot.Reason == "" && len(boot.Cards) == 0 {
		boot.Reason = "no NVIDIA GPU"
		if len(cards) > 0 {
			boot.Reason = "no GPUs are assigned to NVIDIA"
		}

		if gpuModuleBlacklisted("nvidia", n.state.System.Kernel.Config.BlacklistModules) {
			boot.Reason = "the NVIDIA driver is disabled by the kernel configuration"
		}
	}

	completed = true

	return nil
}

func (n *nvidia) startAttempt(ctx context.Context, boot *nvidiaBoot) {
	err := n.verifyModules(ctx)
	if err != nil {
		boot.Reason = "the NVIDIA modules are not signed by this operating system"

		return
	}

	err = n.saveAttempt(boot.Version)
	if err != nil {
		boot.Reason = "NVIDIA startup could not be recorded safely"

		return
	}

	boot.Attempted = true

	err = n.loadModules(ctx)
	if err == nil {
		err = n.bindCards(boot.Cards)
	}

	if err != nil {
		boot.Reason = "NVIDIA driver failed to bind an assigned GPU"
		boot.Failure = true

		return
	}

	err = n.markBound()
	if err != nil {
		boot.Reason = "completion of NVIDIA binding could not be recorded safely"
		boot.Failure = true
	}
}

func (n *nvidia) finishBoot(ctx context.Context, boot *nvidiaBoot) error {
	if !boot.Attempted {
		for _, address := range boot.Cards {
			_ = os.WriteFile(n.path("/sys/bus/pci/devices/"+address+"/driver_override"), []byte("nouveau"), 0o600)
		}

		boot.Cards = nil
	}
	// Use a fresh bounded context even when an earlier command exhausted the preparation deadline.
	fallbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if !gpuModuleBlacklisted("nouveau", n.state.System.Kernel.Config.BlacklistModules) {
		_, err := n.command(fallbackCtx, "/sbin/modprobe", "nouveau")
		if err != nil {
			boot.Reason = "the Nouveau fallback driver could not be loaded"
		}
	}

	err := os.MkdirAll(n.path("/run/incus-os"), 0o700)
	if err != nil {
		return errors.New("the GPU boot status could not be recorded")
	}

	body, err := json.Marshal(boot)
	if err != nil {
		return errors.New("the GPU boot status could not be recorded")
	}

	err = os.WriteFile(n.path(nvidiaBootFile), body, 0o600)
	if err != nil {
		return errors.New("the GPU boot status could not be recorded")
	}

	return nil
}

func (n *nvidia) verifyModules(ctx context.Context) error {
	kernel, err := n.command(ctx, "uname", "-r")
	if err != nil {
		return err
	}

	for _, name := range []string{"nvidia", "nvidia-modeset", "nvidia-uvm", "nvidia-drm", "nvidia-peermem"} {
		path := n.modulePath(strings.TrimSpace(kernel), name)
		if n.verifyModule != nil {
			err = n.verifyModule(path)
		} else {
			err = systemd.VerifyKernelModule(path, n.path("/usr/lib/verity.d"))
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func (n *nvidia) validate(ctx context.Context) (string, error) {
	version, err := os.ReadFile(n.path("/usr/share/nvidia/version"))
	if err != nil {
		return "", errors.New(systemd.NVIDIAMissingReason)
	}

	v := strings.TrimSpace(string(version))

	kernel, err := os.ReadFile(n.path("/usr/share/nvidia/kernel"))
	if err != nil {
		return v, errors.New(systemd.NVIDIAMissingReason)
	}

	running, err := n.command(ctx, "uname", "-r")
	if err != nil || strings.TrimSpace(running) != strings.TrimSpace(string(kernel)) {
		return v, errors.New(systemd.NVIDIAMissingReason)
	}

	module := n.modulePath(strings.TrimSpace(running), "nvidia")

	actual, err := n.command(ctx, "/sbin/modinfo", "-F", "version", module)
	if err != nil || strings.TrimSpace(actual) != v {
		return v, errors.New("NVIDIA module version does not match its userspace")
	}

	firmware, err := n.command(ctx, "/sbin/modinfo", "-F", "firmware", module)
	if err != nil || strings.TrimSpace(firmware) == "" {
		return v, errors.New("NVIDIA firmware requirements are unavailable")
	}

	for name := range strings.FieldsSeq(firmware) {
		if !strings.HasPrefix(name, "nvidia/"+v+"/") || filepath.Clean(name) != name {
			return v, errors.New("NVIDIA firmware does not match the driver version")
		}

		info, err := os.Stat(n.path("/usr/lib/firmware/" + name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return v, errors.New("NVIDIA firmware is missing")
		}
	}

	return v, nil
}

func (n *nvidia) modulePath(kernel string, name string) string {
	return n.path("/usr/lib/modules/" + kernel + "/nvidia/" + name + ".ko")
}

func (n *nvidia) writeModprobeConfig() error {
	err := os.MkdirAll(n.path("/run/modprobe.d"), 0o755)
	if err != nil {
		return err
	}

	return os.WriteFile(n.path("/run/modprobe.d/50-nvidia-choice.conf"), []byte("blacklist nouveau\nblacklist nvidia\nblacklist nvidia_drm\nblacklist nvidia_modeset\nblacklist nvidia_uvm\noptions nvidia_drm modeset=1 fbdev=1\n"), 0o644)
}

func (n *nvidia) loadModules(ctx context.Context) error {
	kernel, err := n.command(ctx, "uname", "-r")
	if err != nil {
		return err
	}

	for _, name := range []string{"nvidia", "nvidia-modeset", "nvidia-uvm", "nvidia-drm"} {
		path := n.modulePath(strings.TrimSpace(kernel), name)

		depends, err := n.command(ctx, "/sbin/modinfo", "-F", "depends", path)
		if err != nil {
			return err
		}

		for dep := range strings.SplitSeq(strings.TrimSpace(depends), ",") {
			if dep == "" || strings.HasPrefix(dep, "nvidia") {
				continue
			}

			_, err := n.command(ctx, "/sbin/modprobe", dep)
			if err != nil {
				return err
			}
		}

		args := []string{path}
		if name == "nvidia-drm" {
			args = append(args, "modeset=1", "fbdev=1")
		}

		_, err = n.command(ctx, "/sbin/insmod", args...)
		if err != nil {
			return err
		}
	}

	_, err = n.command(ctx, "/usr/bin/nvidia-modprobe", "-u")

	return err
}

func (n *nvidia) bindCards(cards []string) error {
	for _, address := range cards {
		driver, _ := os.Readlink(n.path("/sys/bus/pci/devices/" + address + "/driver"))
		if filepath.Base(driver) != "nvidia" {
			err := os.WriteFile(n.path("/sys/bus/pci/drivers/nvidia/bind"), []byte(address), 0o600)
			if err != nil {
				return err
			}
		}

		driver, _ = os.Readlink(n.path("/sys/bus/pci/devices/" + address + "/driver"))
		if filepath.Base(driver) != "nvidia" {
			return fmt.Errorf("GPU %s did not bind to NVIDIA", address)
		}
	}

	return nil
}
