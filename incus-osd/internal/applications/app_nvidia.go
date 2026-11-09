package applications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

const nvidiaStable = "nvidia"

const nvidiaBootFile = "/run/incus-os/nvidia-boot.json"

type nvidia struct {
	common

	variant    string
	sysroot    string
	runCommand func(context.Context, string, ...string) (string, error)
}

type nvidiaBoot struct {
	Config  api.ApplicationNVIDIAConfig `json:"config"`
	Version string                      `json:"version"`
	Cards   []string                    `json:"cards"`
	Reason  string                      `json:"reason"`
}

func (n *nvidia) Name() string {
	return n.variant
}

func (n *nvidia) Get(_ context.Context) (any, error) {
	return n.state.Applications.NVIDIA, nil
}

func (*nvidia) Struct() any {
	return &api.ApplicationNVIDIA{}
}

func (*nvidia) GetDependencies() []string {
	return []string{"gpu-support"}
}

func (n *nvidia) IsInstalled() bool {
	return isInstalled(n.Name(), n.Version())
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
		return err
	}

	n.state.Applications.NVIDIA.State.DriverVersion = strings.TrimSpace(string(version))
	n.appState.FriendlyVersion = strings.TrimSpace(string(version)) + " [" + n.Version() + "]"

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

	for address, use := range app.Config.Cards {
		if !regexp.MustCompile(`^[[:xdigit:]]{4}:[[:xdigit:]]{2}:[[:xdigit:]]{2}\.[0-7]$`).MatchString(address) {
			return fmt.Errorf("invalid PCI address %q", address)
		}

		if use != "nvidia" && use != "vfio-pci" && use != "off" {
			return fmt.Errorf("invalid use %q for %s", use, address)
		}
	}

	n.state.Applications.NVIDIA.Config = app.Config

	return n.requireReboot()
}

func (n *nvidia) Initialize(_ context.Context) error {
	if n.state.Applications.NVIDIA.Config.Driver == "" {
		n.state.Applications.NVIDIA.Config.Driver = "nvidia"
	}

	n.appState.Initialized = true

	return n.requireReboot()
}

func (n *nvidia) Stop(_ context.Context) error {
	return n.requireReboot()
}

func (n *nvidia) Update(_ context.Context) error {
	return n.requireReboot()
}

func (n *nvidia) Restart(_ context.Context) error {
	return n.requireReboot()
}

func (n *nvidia) Start(ctx context.Context) error {
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

	if boot.Reason != "" {
		return n.fallback(ctx, boot, boot.Reason)
	}

	if boot.Version == "" && n.state.Applications.NVIDIA.Config.Driver == "nvidia" {
		n.state.Applications.NVIDIA.State.Reason = "Reboot required to apply the NVIDIA driver choice"

		return n.requireReboot()
	}

	if boot.Version == "" {
		n.state.Applications.NVIDIA.State.ActiveDriver = "nouveau"

		return n.state.Save()
	}

	err = n.health(ctx, boot.Cards)
	if err != nil {
		return n.fallback(ctx, boot, err.Error())
	}

	n.state.Applications.NVIDIA.State.ActiveDriver = "nvidia"
	n.state.Applications.NVIDIA.State.Reason = ""

	return n.state.Save()
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
	n.state.System.Update.State.NeedsReboot = true

	return n.state.Save()
}

func (n *nvidia) health(ctx context.Context, cards []string) error {
	if len(cards) == 0 {
		return errors.New("no NVIDIA GPU")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := n.command(ctx, "/usr/bin/nvidia-smi", "--query-gpu=pci.bus_id", "--format=csv,noheader")
	if err != nil {
		return errors.New("NVIDIA health check could not initialise every assigned GPU")
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

func (n *nvidia) fallback(ctx context.Context, boot nvidiaBoot, reason string) error {
	s := &n.state.Applications.NVIDIA.State
	if s.BadVersions == nil {
		s.BadVersions = map[string]string{}
	}

	if boot.Version != "" {
		s.BadVersions[n.state.OS.RunningRelease+":"+boot.Version] = reason
	}

	s.ActiveDriver = "nouveau"
	s.Reason = reason

	err := n.state.Save()
	if err != nil {
		return err
	}

	// A reboot releases DRM consoles safely and applies the fallback before any driver probes.
	if len(boot.Cards) > 0 {
		err = n.requireReboot()
		if err != nil {
			return err
		}

		_, err = n.command(ctx, "systemctl", "--no-block", "reboot")

		return err
	}

	_, err = n.command(ctx, "/sbin/modprobe", "nouveau")

	return err
}

// PrepareGPUs applies per-address ownership before explicitly loading Nouveau or NVIDIA.
func PrepareGPUs(ctx context.Context, s *state.State) error {
	app, err := Load(ctx, s, nvidiaStable)
	if err != nil {
		return err
	}

	n, ok := app.(*nvidia)
	if !ok {
		return errors.New("unexpected NVIDIA application type")
	}

	return n.prepare(ctx, n.IsInstalled())
}
