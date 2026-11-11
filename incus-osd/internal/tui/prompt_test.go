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
				answer, err := modal.Prompt(ctx, "WARNING: Everything on this device will be erased.\n"+
					"The TPM will be reset at the end of the install, destroying any keys or\n"+
					"credentials a previous system stored in it.\n"+
					"If the other system uses the TPM to unlock its disk, export that system's\n"+
					"recovery key before continuing.\n\n"+
					"Model: Example disk\nSize: 64 GiB\nPartitions: Windows, ntfs, 32 GiB")
				result <- response{answer: answer, err: err}
			}()

			require.Eventually(t, func() bool {
				var visible bool

				ttui.app.QueueUpdate(func() { visible = ttui.pages.HasPage("install-confirmation") })

				return visible
			}, time.Second, time.Millisecond)

			contents := strings.Join(rows(ttui, screen), "\n")
			for _, want := range []string{"Example disk", "64 GiB", "Windows, ntfs, 32 GiB", "Type ERASE", "Confirmation:"} {
				require.Contains(t, contents, want)
			}

			for _, want := range []string{
				"Everything on this device will be erased.",
				"The TPM will be reset at the end of the install, destroying any keys or",
				"credentials a previous system stored in it.",
				"If the other system uses the TPM to unlock its disk, export that system's",
				"recovery key before continuing.",
			} {
				require.Contains(t, contents, want)
				require.Less(t, strings.Index(contents, want), strings.Index(contents, "Confirmation:"))
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
