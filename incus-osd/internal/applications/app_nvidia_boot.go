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

	"github.com/lxc/incus-os/incus-osd/api"
)

type nvidiaCard struct {
	address string
	id      string
	driver  string
}

func (n *nvidia) cards() ([]nvidiaCard, error) {
	entries, err := os.ReadDir(n.path("/sys/bus/pci/devices"))
	if err != nil {
		return nil, err
	}

	cards := []nvidiaCard{}

	for _, entry := range entries {
		path := n.path("/sys/bus/pci/devices/" + entry.Name())
		vendor, vendorErr := os.ReadFile(filepath.Join(path, "vendor"))
		class, classErr := os.ReadFile(filepath.Join(path, "class"))

		id, idErr := os.ReadFile(filepath.Join(path, "device"))
		if vendorErr != nil || classErr != nil || idErr != nil || strings.TrimSpace(string(vendor)) != "0x10de" {
			continue
		}

		if !slices.Contains([]string{"0x030000", "0x030200"}, strings.TrimSpace(string(class))) {
			continue
		}

		driver, err := os.Readlink(filepath.Join(path, "driver"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}

		cards = append(cards, nvidiaCard{entry.Name(), strings.TrimPrefix(strings.TrimSpace(string(id)), "0x"), filepath.Base(driver)})
	}

	return cards, nil
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
		return (p.PCIAddress != "" && p.PCIAddress == card.address) || (p.PCIAddress == "" && strings.EqualFold(p.VendorID, "10de") && strings.EqualFold(p.ProductID, card.id))
	})
}

func (n *nvidia) prepare(ctx context.Context, installed bool) error {
	boot := nvidiaBoot{}

	config := &n.state.Applications.NVIDIA.Config
	if installed && config.Driver == "" {
		config.Driver = "nvidia"

		err := n.state.Save()
		if err != nil {
			return err
		}
	}

	boot.Config = *config

	if installed && config.Driver == "nvidia" {
		version, err := n.validate(ctx)

		boot.Version = version
		if err != nil {
			boot.Reason = err.Error()
		}

		reason := n.state.Applications.NVIDIA.State.BadVersions[n.state.OS.RunningRelease+":"+version]
		if reason != "" {
			boot.Reason = reason
		}
	}

	cards, err := n.cards()
	if err != nil {
		return err
	}

	supported, _ := os.ReadFile(n.path("/usr/share/nvidia/supported-gpus"))
	vfioCards := []string{}

	for _, card := range cards {
		// Existing owners and configured passthroughs never enter the NVIDIA bind path.
		if card.driver != "." {
			continue
		}

		use := config.Cards[card.address]
		if n.passthrough(card) {
			use = "vfio-pci"
		}

		if use == "vfio-pci" {
			vfioCards = append(vfioCards, card.address)
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
			return err
		}

		if use == "nvidia" {
			boot.Cards = append(boot.Cards, card.address)
		}
	}

	_, err = n.command(ctx, "/sbin/ldconfig")
	if err != nil {
		return err
	}

	err = n.writeModprobeConfig()
	if err != nil {
		return err
	}

	if len(vfioCards) > 0 {
		_, err = n.command(ctx, "/sbin/modprobe", "vfio-pci")
		if err != nil {
			return err
		}
	}

	for _, address := range vfioCards {
		err := os.WriteFile(n.path("/sys/bus/pci/drivers_probe"), []byte(address), 0o600)
		if err != nil {
			return err
		}
	}

	if len(boot.Cards) > 0 {
		err = n.loadModules(ctx)
		if err == nil {
			err = n.bindCards(boot.Cards)
		}

		if err != nil {
			boot.Reason = "NVIDIA driver failed to bind an assigned GPU"
		}
	} else if installed && config.Driver == "nvidia" && boot.Reason == "" {
		boot.Reason = "no NVIDIA GPU"
	}

	// Explicit loads bypass the factory autoload blacklist after all overrides are in place.
	if !gpuModuleBlacklisted("nouveau", n.state.System.Kernel.Config.BlacklistModules) {
		_, err = n.command(ctx, "/sbin/modprobe", "nouveau")
		if err != nil {
			return err
		}
	}

	err = os.MkdirAll(n.path("/run/incus-os"), 0o700)
	if err != nil {
		return err
	}

	body, err := json.Marshal(boot)
	if err != nil {
		return err
	}

	return os.WriteFile(n.path(nvidiaBootFile), body, 0o600)
}

func (n *nvidia) validate(ctx context.Context) (string, error) {
	version, err := os.ReadFile(n.path("/usr/share/nvidia/version"))
	if err != nil {
		return "", err
	}

	v := strings.TrimSpace(string(version))

	kernel, err := os.ReadFile(n.path("/usr/share/nvidia/kernel"))
	if err != nil {
		return v, err
	}

	running, err := n.command(ctx, "uname", "-r")
	if err != nil || strings.TrimSpace(running) != strings.TrimSpace(string(kernel)) {
		return v, errors.New("NVIDIA application does not match the running kernel")
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
