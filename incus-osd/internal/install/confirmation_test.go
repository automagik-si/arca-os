package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	apiseed "github.com/lxc/incus-os/incus-osd/api/seed"
)

func TestInspectPartitionedInstallDevice(t *testing.T) { //nolint:paralleltest // Stubs lsblk through PATH.
	flat, err := os.ReadFile("testdata/target-lsblk.json")
	require.NoError(t, err)

	var snapshot struct {
		BlockDevices []installDevice `json:"blockdevices"`
	}

	require.NoError(t, json.Unmarshal(flat, &snapshot))
	require.Len(t, snapshot.BlockDevices, 12)

	disk := snapshot.BlockDevices[0]
	disk.Children = snapshot.BlockDevices[1:]
	snapshot.BlockDevices = []installDevice{disk}
	tree, err := json.Marshal(snapshot)
	require.NoError(t, err)

	dir := t.TempDir()
	flatPath := filepath.Join(dir, "flat.json")
	treePath := filepath.Join(dir, "tree.json")

	require.NoError(t, os.WriteFile(flatPath, flat, 0o600))
	require.NoError(t, os.WriteFile(treePath, tree, 0o600))
	t.Setenv("LSBLK_FLAT", flatPath)
	t.Setenv("LSBLK_TREE", treePath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stub := `#!/bin/sh
case " $* " in
	*" --tree "*) exec cat "$LSBLK_TREE" ;;
	*) exec cat "$LSBLK_FLAT" ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lsblk"), []byte(stub), 0o700)) //nolint:gosec // Executable test stub.

	plan, err := inspectInstallDevice(t.Context(), "/dev/sda")
	require.NoError(t, err)

	for _, want := range []string{"Target: /dev/sda", "50.00 GiB", "/dev/sda1", "/dev/sda11", "IncusOS_202611022009", "luks-root-x86-64", "zfs_member"} {
		require.Contains(t, plan, want)
	}

	// A tree still must identify exactly one disk with a nonzero size.
	snapshot.BlockDevices = append(snapshot.BlockDevices, disk)
	ambiguous, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(treePath, ambiguous, 0o600))
	plan, err = inspectInstallDevice(t.Context(), "/dev/sda")
	require.ErrorContains(t, err, "cannot identify target")
	require.Empty(t, plan)

	disk.Size = 0
	snapshot.BlockDevices = []installDevice{disk}
	zeroSize, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(treePath, zeroSize, 0o600))
	plan, err = inspectInstallDevice(t.Context(), "/dev/sda")
	require.ErrorContains(t, err, "cannot identify target")
	require.Empty(t, plan)
}

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
	for _, want := range []string{"/dev/test", "Example disk", "64.00 GiB", "example-serial", "gpt", "/dev/test1", "32.00 GiB", "ntfs", "Microsoft basic data", "Windows", "Everything on the target disk shown below will be erased"} {
		require.Contains(t, plan, want)
	}

	for line := range strings.SplitSeq(plan, "\n") {
		require.LessOrEqual(t, utf8.RuneCountInString(line), 78, "plan line: %s", line)
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
			require.Equal(t, "WARNING: Everything on the target disk shown below will be erased.\n"+
				"The TPM will also be reset at the end of the install, destroying any keys or\n"+
				"credentials stored in it. This also affects OTHER disks in this machine that\n"+
				"the installer does not erase: an encrypted disk (BitLocker, LUKS) that\n"+
				"unlocks with this TPM will then ask for its recovery key or passphrase.\n"+
				"Before typing ERASE, save recovery keys or passphrases off this machine.\n"+
				"If none exists, add one before installing. Esc cancels.", warning)
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

			if tt.preanswered {
				require.Contains(t, logs.String(), "resetting the TPM")
				require.Contains(t, logs.String(), "other disks may need recovery keys or passphrases")
			}
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
		targetESP    string
		allowed      bool
	}{
		{name: "installed OS refusal", confirmation: "abcdef", targetESP: "abcdef00-1111-2222-3333-444444444444"},
		{name: "missing target confirmation", force: true, targetESP: "abcdef00-1111-2222-3333-444444444444"},
		{name: "wrong target confirmation", force: true, confirmation: "wrong", targetESP: "abcdef00-1111-2222-3333-444444444444"},
		{name: "invalid ESP UUID", force: true, confirmation: "abcdef", targetESP: "abc"},
		{name: "matching target ESP", force: true, confirmation: "abcdef", targetESP: "abcdef00-1111-2222-3333-444444444444", allowed: true},
		{name: "existing force override", force: true, confirmation: "FORCE-WIPE-I-KNOW-WHAT-IAM-DOING", targetESP: "abcdef00-1111-2222-3333-444444444444", allowed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			config := &apiseed.Install{ForceInstall: tt.force, ForceInstallConfirmation: tt.confirmation, Confirmation: "ERASE"}
			prompted := false
			err := withReinstallSafety(config, "Example OS", tt.targetESP, func() error {
				prompted = true

				return nil
			})
			require.Equal(t, tt.allowed, prompted, "existing safety checks must run before the new prompt, even with a seed pre-answer")
			require.Equal(t, !tt.allowed, err != nil)
		})
	}
}

func TestTargetReinstallSafety(t *testing.T) {
	t.Parallel()

	target := installDevice{Path: "/dev/target", Children: []installDevice{
		{PartLabel: "esp", PartUUID: "ABCDEF00-1111-2222-3333-444444444444"},
		{PartLabel: "seed-data"},
		{PartLabel: "IncusOS_202001010000"},
	}}

	for _, usrOnTarget := range []bool{false, true} {
		for _, tt := range []struct {
			name         string
			confirmation string
			allowed      bool
		}{
			{name: "missing confirmation"},
			{name: "old running machine ID", confirmation: "fedcba"},
			{name: "target ESP confirmation", confirmation: "abcdef", allowed: true},
			{name: "explicit force override", confirmation: "FORCE-WIPE-I-KNOW-WHAT-IAM-DOING", allowed: true},
		} {
			t.Run(fmt.Sprintf("%s/usr-on-target=%t", tt.name, usrOnTarget), func(t *testing.T) {
				t.Parallel()

				config := &apiseed.Install{ForceInstall: true, ForceInstallConfirmation: tt.confirmation, Confirmation: "ERASE"}
				writes := 0

				err := withTargetReinstallSafety(config, "IncusOS", "202001010000", target, usrOnTarget, func() error {
					writes++

					return nil
				})
				if tt.allowed {
					require.NoError(t, err)
					require.Equal(t, 1, writes)
				} else {
					require.Error(t, err)
					require.Zero(t, writes, "neither GPT zap nor full discard may run")
					require.Contains(t, err.Error(), "abcdef", "the error must identify the target's stable token")
					require.Contains(t, err.Error(), "first six characters of the target ESP PARTUUID")
				}
			})
		}
	}
}

func TestTargetReinstallDetection(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name        string
		children    []installDevice
		usrOnTarget bool
		allowed     bool
	}{
		{name: "empty disk", allowed: true},
		{name: "foreign disk", children: []installDevice{{PartLabel: "Windows"}}, allowed: true},
		{name: "ESP alone", children: []installDevice{{PartLabel: "esp"}}, allowed: true},
		{name: "same release", children: []installDevice{{PartLabel: "IncusOS_202001010000"}}},
		{name: "different release without confirmation", children: []installDevice{{PartLabel: "IncusOS_201901010000"}, {PartLabel: "esp", PartUUID: "abcdef00-1111-2222-3333-444444444444"}}, allowed: true},
		{name: "different verity without confirmation", children: []installDevice{{PartLabel: "IncusOS_201901010000_verity"}, {PartLabel: "esp", PartUUID: "abcdef00-1111-2222-3333-444444444444"}}, allowed: true},
		{name: "different signature without confirmation", children: []installDevice{{PartLabel: "IncusOS_201901010000_verity_sig"}, {PartLabel: "esp", PartUUID: "abcdef00-1111-2222-3333-444444444444"}}, allowed: true},
		{name: "matching A/B slot", children: []installDevice{{PartLabel: "IncusOS_201901010000"}, {PartLabel: "IncusOS_202001010000"}}},
		{name: "verity only", children: []installDevice{{PartLabel: "IncusOS_202001010000_verity"}}},
		{name: "signature only", children: []installDevice{{PartLabel: "IncusOS_202001010000_verity_sig"}}},
		{name: "invalid target UUID", children: []installDevice{{PartLabel: "IncusOS_202001010000"}, {PartLabel: "esp", PartUUID: "invalid"}}},
		{name: "ambiguous ESP", children: []installDevice{{PartLabel: "IncusOS_202001010000"}, {PartLabel: "esp"}, {PartLabel: "esp"}}},
		{name: "active usr without labels", usrOnTarget: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			writes := 0
			config := &apiseed.Install{ForceInstall: true, Confirmation: "ERASE"}

			err := withTargetReinstallSafety(config, "IncusOS", "202001010000", installDevice{Children: tt.children}, tt.usrOnTarget, func() error {
				writes++

				return nil
			})
			if tt.allowed {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}

			require.Equal(t, tt.allowed, writes == 1)
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
