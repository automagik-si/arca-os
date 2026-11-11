package tui

import (
	"context"
	"errors"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Prompt reads a single answer on the same console screen used by the installer.
func (m *Modal) Prompt(ctx context.Context, message string) (string, error) {
	type response struct {
		answer string
		err    error
	}

	result := make(chan response, 1)
	finish := func(answer string, err error) {
		select {
		case result <- response{answer: answer, err: err}:
		default:
		}
	}

	t := m.t

	var (
		previousCapture func(*tcell.EventKey) *tcell.EventKey
		previousFocus   tview.Primitive
	)

	installed := false

	defer func() {
		_ = t.promptUpdate(context.WithoutCancel(ctx), func() {
			if installed {
				t.pages.RemovePage("install-confirmation")
				t.app.SetInputCapture(previousCapture)
				t.app.SetFocus(previousFocus)
			}
		})
	}()

	err := t.promptUpdate(ctx, func() {
		installed = true
		previousCapture = t.app.GetInputCapture()
		previousFocus = t.app.GetFocus()
		text := tview.NewTextView().SetText(message).SetWordWrap(true)
		input := tview.NewInputField().SetLabel("Confirmation: ")
		input.SetDoneFunc(func(key tcell.Key) {
			if key == tcell.KeyEnter {
				finish(input.GetText(), nil)
			}
		})

		root := tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(text, 0, 1, false).
			AddItem(tview.NewTextView().SetText("WARNING: the disk is erased and the TPM reset; other disks may need keys.\nType ERASE then Enter. Ctrl-C/Esc cancels. Up/Down/PageUp/PageDown scrolls."), 3, 0, false).
			AddItem(input, 1, 0, true)
		root.SetBorder(true).SetTitle(" " + m.title + " - confirm disk erasure ")
		t.pages.AddPage("install-confirmation", root, true, true)
		t.app.SetFocus(input)
		t.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			switch event.Key() { //nolint:exhaustive // Only cancellation and scrolling keys need interception.
			case tcell.KeyCtrlC, tcell.KeyEscape, tcell.KeyCtrlD:
				finish("", errors.New("confirmation cancelled"))

				return nil
			case tcell.KeyUp, tcell.KeyDown, tcell.KeyPgUp, tcell.KeyPgDn, tcell.KeyHome, tcell.KeyEnd:
				text.InputHandler()(event, func(tview.Primitive) {})

				return nil
			default:
				return event
			}
		})
	})
	if err != nil {
		return "", err
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-t.runDone:
		return "", errors.New("install console closed")
	case reply := <-result:
		select {
		case <-t.runDone:
			return "", errors.New("install console closed")
		default:
		}

		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		return reply.answer, reply.err
	}
}

// promptUpdate also unblocks if the console closes while a UI update is pending.
func (t *TUI) promptUpdate(ctx context.Context, update func()) error {
	select {
	case <-t.runDone:
		return errors.New("install console closed")
	default:
	}

	done := make(chan struct{})

	go func() {
		t.app.QueueUpdateDraw(func() {
			if ctx.Err() == nil {
				update()
			}
		})
		close(done)
	}()

	select {
	case <-t.runDone:
		return errors.New("install console closed")
	case <-done:
		return ctx.Err()
	}
}
