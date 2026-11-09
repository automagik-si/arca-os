package applications

import (
	"context"
	"os"
	"strings"

	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

// arcaUnit is the systemd unit shipped in the arca sysext.
const arcaUnit = "arca.service"

// arcaStateDir holds arca's node key, desired state and token hashes.
const arcaStateDir = "/var/lib/arca"

// arca is the Arca node application. It talks to the local Incus socket, so
// it depends on Incus and starts after the primary application.
type arca struct {
	common
}

func (a *arca) Get(_ context.Context) (any, error) {
	return a.state.Applications.Arca, nil
}

// FactoryReset performs a full factory reset of the application.
func (a *arca) FactoryReset(ctx context.Context) error {
	// Stop the application.
	err := a.Stop(ctx)
	if err != nil {
		return err
	}

	// Wipe local data.
	err = a.WipeLocalData(ctx)
	if err != nil {
		return err
	}

	// Start the application again; systemd recreates the state directories.
	return a.Start(ctx)
}

// GetDependencies returns a list of other applications this application depends on.
func (*arca) GetDependencies() []string {
	return []string{incusVersionStable + " OR " + incusVersionLTS70}
}

// IsInstalled reports whether the application has been installed.
func (a *arca) IsInstalled() bool {
	return isInstalled(a.Name(), a.appState.Version)
}

// IsRunning reports if the application is currently running.
func (*arca) IsRunning(ctx context.Context) bool {
	return systemd.IsActive(ctx, arcaUnit)
}

func (*arca) Name() string {
	return "arca"
}

// Restart restarts the systemd unit.
func (*arca) Restart(ctx context.Context) error {
	return systemd.RestartUnit(ctx, arcaUnit)
}

// SetFriendlyVersion records the friendly version.
func (a *arca) SetFriendlyVersion(_ context.Context) error {
	version, err := os.ReadFile("/usr/lib/arca/version")
	if err != nil {
		return err
	}

	a.appState.FriendlyVersion = strings.TrimSpace(string(version)) + " [" + a.appState.Version + "]"

	return nil
}

// Start starts the systemd unit.
func (*arca) Start(ctx context.Context) error {
	return systemd.StartUnit(ctx, arcaUnit)
}

// StartupWeight starts arca after the primary application, whose socket it uses.
func (*arca) StartupWeight() int {
	return 1
}

// Stop stops the systemd unit.
func (*arca) Stop(ctx context.Context) error {
	return systemd.StopUnit(ctx, arcaUnit)
}

// Update restarts the unit after an application update, so the new binary runs.
func (*arca) Update(ctx context.Context) error {
	// Reload the systemd daemon to pick up any unit changes.
	err := systemd.ReloadDaemon(ctx)
	if err != nil {
		return err
	}

	return systemd.RestartUnit(ctx, arcaUnit)
}

// WipeLocalData removes local data created by the application.
func (*arca) WipeLocalData(_ context.Context) error {
	return os.RemoveAll(arcaStateDir)
}
