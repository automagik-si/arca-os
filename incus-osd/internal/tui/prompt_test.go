package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/stretchr/testify/require"
)

func TestInstallPrompt(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		answer  string
		key     tcell.Key
		cancel  bool
		close   bool
		wantErr bool
	}{
		{name: "exact", answer: "ERASE", key: tcell.KeyEnter},
		{name: "empty", key: tcell.KeyEnter},
		{name: "wrong", answer: "wrong", key: tcell.KeyEnter},
		{name: "ctrl-c", key: tcell.KeyCtrlC, wantErr: true},
		{name: "escape", key: tcell.KeyEscape, wantErr: true},
		{name: "ctrl-d", key: tcell.KeyCtrlD, wantErr: true},
		{name: "timeout", cancel: true, wantErr: true},
		{name: "console closes", close: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ttui, screen, _ := newConsoleTUI(t)
			modal := ttui.AddModal("Install", "install")

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			type response struct {
				answer string
				err    error
			}

			result := make(chan response, 1)

			go func() {
				answer, err := modal.Prompt(ctx, "WARNING: Everything on the target disk shown below will be erased.\n"+
					"The TPM will also be reset at the end of the install, destroying any keys or\n"+
					"credentials stored in it. This also affects OTHER disks in this machine that\n"+
					"the installer does not erase: an encrypted disk (BitLocker, LUKS) that\n"+
					"unlocks with this TPM will then ask for its recovery key or passphrase.\n"+
					"Before typing ERASE, save recovery keys or passphrases off this machine.\n"+
					"If none exists, add one before installing. Esc cancels.\n\n"+
					"Target: /dev/test\nModel: Example disk\nSize: 64 GiB\nSerial: example\nPartition table: gpt\n"+
					"Current contents (OS names, when detected, appear in labels/types):\n"+
					"/dev/test: disk, 64 GiB, filesystem: none detected, type: none detected,\nlabel: none detected, partition label: none detected\n"+
					"  /dev/test1: part, 1 GiB, filesystem: vfat, type: EFI System,\nlabel: ESP, partition label: EFI\n"+
					"  /dev/test2: part, 1 GiB, filesystem: none detected, type: reserved,\nlabel: none detected, partition label: reserved\n"+
					"  /dev/test3: part, 30 GiB, filesystem: ntfs, type: Microsoft basic data,\nlabel: Recovery, partition label: Recovery\n"+
					"  /dev/test4: part, 32 GiB, filesystem: ntfs, type: Microsoft basic data,\nlabel: Windows, partition label: Windows\n"+
					"\nType ERASE exactly and press Enter to continue.")
				result <- response{answer: answer, err: err}
			}()

			require.Eventually(t, func() bool {
				var visible bool

				ttui.app.QueueUpdate(func() { visible = ttui.pages.HasPage("install-confirmation") })

				return visible
			}, time.Second, time.Millisecond)

			contents := strings.Join(rows(ttui, screen), "\n")
			for _, want := range []string{"Example disk", "64 GiB", "Type ERASE", "Confirmation:"} {
				require.Contains(t, contents, want)
			}

			for _, want := range []string{
				"Everything on the target disk shown below will be erased.",
				"The TPM will also be reset at the end of the install, destroying any keys or",
				"credentials stored in it. This also affects OTHER disks in this machine that",
				"the installer does not erase: an encrypted disk (BitLocker, LUKS) that",
				"unlocks with this TPM will then ask for its recovery key or passphrase.",
				"Before typing ERASE, save recovery keys or passphrases off this machine.",
				"If none exists, add one before installing. Esc cancels.",
			} {
				require.Contains(t, contents, want)
				require.Less(t, strings.Index(contents, want), strings.Index(contents, "Confirmation:"))
			}

			require.NotContains(t, contents, "label: Windows")
			screen.InjectKey(tcell.KeyEnd, 0, tcell.ModNone)
			require.Eventually(t, func() bool {
				return strings.Contains(strings.Join(rows(ttui, screen), "\n"), "label: Windows")
			}, time.Second, time.Millisecond)

			contents = strings.Join(rows(ttui, screen), "\n")
			for _, want := range []string{
				"WARNING: the disk is erased and the TPM reset; other disks may need keys.",
				"Type ERASE then Enter.", "Confirmation:",
			} {
				require.Contains(t, contents, want)
			}

			// Rotating status modals must not obscure the confirmation input.
			modal.Update("Another status update")
			require.Contains(t, strings.Join(rows(ttui, screen), "\n"), "Confirmation:")

			switch {
			case tt.cancel:
				cancel()
			case tt.close:
				ttui.app.Stop()
			default:
				for _, r := range tt.answer {
					screen.InjectKey(tcell.KeyRune, r, tcell.ModNone)
				}

				screen.InjectKey(tt.key, 0, tcell.ModNone)
			}

			select {
			case reply := <-result:
				if tt.wantErr {
					require.Error(t, reply.err)
				} else {
					require.NoError(t, reply.err)
					require.Equal(t, tt.answer, reply.answer)
				}
			case <-ctx.Done():
				select {
				case reply := <-result:
					require.Error(t, reply.err)
				case <-time.After(time.Second):
					t.Fatal("prompt did not abort")
				}
			}
		})
	}
}
