package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	apiseed "github.com/lxc/incus-os/incus-osd/api/seed"
)

func TestRenderInstallPlan(t *testing.T) {
	t.Parallel()

	plan := renderInstallPlan(installDevice{
		Path: "/dev/test", Model: "Example disk", Size: 64 * 1024 * 1024 * 1024,
		Serial: "example-serial", PTType: "gpt",
		Children: []installDevice{{
			Path: "/dev/test1", Type: "part", Size: 32 * 1024 * 1024 * 1024,
			FSType: "ntfs", PartTypeName: "Microsoft basic data", Label: "Windows",
		}},
	})
	for _, want := range []string{"/dev/test", "Example disk", "64.00 GiB", "example-serial", "gpt", "/dev/test1", "32.00 GiB", "ntfs", "Microsoft basic data", "Windows", "Everything on this device will be erased"} {
		require.Contains(t, plan, want)
	}

	require.Contains(t, renderInstallPlan(installDevice{}), "none detected")
}

func TestDestructionConfirmationDisclosure(t *testing.T) {
	t.Parallel()

	var displayed string

	confirmation := destructionConfirmation{
		inspect: func(context.Context, string) (string, error) {
			return renderInstallPlan(installDevice{Path: "/dev/test"}), nil
		},
		update: func(plan string) { displayed = plan },
		prompt: func(_ context.Context, plan string) (string, error) {
			require.Equal(t, displayed, plan)
			warning, _, found := strings.Cut(plan, "\n\nTarget:")
			require.True(t, found)
			require.Equal(t, "WARNING: Everything on this device will be erased.\n"+
				"The TPM will be reset at the end of the install, destroying any keys or\n"+
				"credentials a previous system stored in it.\n"+
				"If the other system uses the TPM to unlock its disk, export that system's\n"+
				"recovery key before continuing.", warning)
			require.Greater(t, strings.Index(plan, "Type ERASE exactly"), len(warning))

			return "ERASE", nil
		},
	}
	require.NoError(t, confirmation.run(t.Context(), "/dev/test", "", func() error { return nil }))
}

func TestDestructionConfirmation(t *testing.T) { //nolint:paralleltest // Captures the process-wide default logger.
	var logs bytes.Buffer

	previousLogger := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	for _, tt := range []struct { //nolint:paralleltest // Each case captures the shared process-wide logger.
		name        string
		answer      string
		seed        string
		promptErr   error
		proceed     bool
		preanswered bool
	}{
		{name: "empty"},
		{name: "wrong", answer: "erase"},
		{name: "whitespace", answer: " ERASE "},
		{name: "exact", answer: "ERASE", proceed: true},
		{name: "ctrl-c", answer: "ERASE", promptErr: errors.New("cancelled")},
		{name: "timeout", promptErr: context.DeadlineExceeded},
		{name: "closed console", promptErr: errors.New("console closed")},
		{name: "seed", seed: "ERASE", proceed: true, preanswered: true},
		{name: "malformed seed", seed: "true"},
		{name: "malformed seed still prompts", seed: " ERASE", answer: "ERASE", proceed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs.Reset()

			var (
				messages []string
				commands []string
			)

			prompted := false
			confirmation := destructionConfirmation{
				inspect: func(context.Context, string) (string, error) { return "Example disk: 64 GiB, gpt, Windows", nil },
				update:  func(message string) { messages = append(messages, message) },
				prompt: func(ctx context.Context, plan string) (string, error) {
					prompted = true
					_, hasDeadline := ctx.Deadline()
					require.True(t, hasDeadline)
					require.Contains(t, plan, "Example disk: 64 GiB, gpt, Windows")
					require.Contains(t, plan, "Type ERASE exactly")

					return tt.answer, tt.promptErr
				},
			}
			commandRunner := func() error {
				commands = append(commands, "wipe")

				return nil
			}
			err := confirmation.run(t.Context(), "/dev/test", tt.seed, commandRunner)
			require.Equal(t, !tt.preanswered, prompted)

			if tt.proceed {
				require.NoError(t, err)
				require.Equal(t, []string{"wipe"}, commands)
			} else {
				require.ErrorContains(t, err, "no disk data was erased")
				require.Empty(t, commands, "command runner must be untouched on abort")
				require.Contains(t, messages[len(messages)-1], "Installation aborted")
			}

			require.Equal(t, tt.preanswered, strings.Contains(logs.String(), "WARNING: AUTOMATION-ONLY"))
		})
	}
}

func TestDestructionConfirmationFailsClosed(t *testing.T) {
	t.Parallel()

	for _, inspectionErr := range []error{nil, errors.New("unreadable device")} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		confirmation := destructionConfirmation{
			inspect: func(context.Context, string) (string, error) { return "plan", inspectionErr },
			update:  func(string) {},
			prompt: func(context.Context, string) (string, error) {
				t.Fatal("seed override must not need a prompt")

				return "", nil
			},
		}
		err := confirmation.run(ctx, "/dev/test", "ERASE", func() error {
			t.Fatal("must not run any destructive command after cancellation or inspection failure")

			return nil
		})
		require.Error(t, err)
	}
}

func TestReinstallSafetyBeforePrompt(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name         string
		force        bool
		confirmation string
		machineID    string
		allowed      bool
	}{
		{name: "installed OS refusal", confirmation: "abcdef", machineID: "abcdef012345"},
		{name: "missing machine confirmation", force: true, machineID: "abcdef012345"},
		{name: "wrong machine confirmation", force: true, confirmation: "wrong", machineID: "abcdef012345"},
		{name: "short machine ID", force: true, confirmation: "abcdef", machineID: "abc"},
		{name: "matching machine ID", force: true, confirmation: "abcdef", machineID: "abcdef012345", allowed: true},
		{name: "existing force override", force: true, confirmation: "FORCE-WIPE-I-KNOW-WHAT-IAM-DOING", machineID: "abcdef012345", allowed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			config := &apiseed.Install{ForceInstall: tt.force, ForceInstallConfirmation: tt.confirmation, Confirmation: "ERASE"}
			prompted := false
			err := withReinstallSafety(config, "Example OS", []byte(tt.machineID), func() error {
				prompted = true

				return nil
			})
			require.Equal(t, tt.allowed, prompted, "existing safety checks must run before the new prompt, even with a seed pre-answer")
			require.Equal(t, !tt.allowed, err != nil)
		})
	}
}

func TestInstallConfirmationSeedTypes(t *testing.T) {
	t.Parallel()

	for _, decode := range []func([]byte, any) error{json.Unmarshal, yaml.Unmarshal} {
		var config apiseed.Install
		require.NoError(t, decode([]byte(`{"confirmation":"ERASE"}`), &config))
		require.Equal(t, "ERASE", config.Confirmation)

		for _, malformed := range []string{`{"confirmation":true}`, `{"confirmation":{}}`, `{"confirmation":[]}`, `{"confirmation":42}`} {
			config = apiseed.Install{}
			_ = decode([]byte(malformed), &config)
			require.NotEqual(t, installConfirmation, config.Confirmation)
		}
	}
}
