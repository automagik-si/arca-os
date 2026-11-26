package update

import (
	"context"
	"log/slog"
	"time"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// nvidiaAfterReadyWait is how long after "system ready" the NVIDIA driver is installed by itself.
const nvidiaAfterReadyWait = time.Minute

// installNVIDIAAfterReady installs the NVIDIA driver once, after wait, when the system should get it by itself.
func installNVIDIAAfterReady(ctx context.Context, s *state.State, wait time.Duration, missing func(context.Context, *state.State) ([]string, error), install func(context.Context, *state.State, []string) error) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(wait):
	}

	names, err := missing(ctx, s)
	if err != nil {
		slog.WarnContext(ctx, "Unable to tell whether the NVIDIA driver is needed", "error", err)

		return
	}

	if len(names) == 0 {
		return
	}

	slog.InfoContext(ctx, "Installing the NVIDIA driver", "applications", names)

	err = install(ctx, s, names)
	if err != nil {
		slog.WarnContext(ctx, "Unable to install the NVIDIA driver; the next update check tries again", "error", err)
	}
}

// installNamedApplications installs or updates exactly the named applications.
func installNamedApplications(ctx context.Context, s *state.State, names []string) error {
	return CheckApplications(ctx, s, names, false, false)
}
