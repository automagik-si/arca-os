package install

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRebootWaitsForShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	queued := make(chan struct{})
	result := make(chan error, 1)

	go func() {
		result <- rebootAndWait(ctx, func(context.Context) error {
			close(queued)

			return nil
		})
	}()

	<-queued

	select {
	case err := <-result:
		t.Fatalf("queued reboot let installation continue: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
}

func TestRebootFailureStopsInstallation(t *testing.T) {
	t.Parallel()

	failure := errors.New("reboot could not be queued")
	err := rebootAndWait(t.Context(), func(context.Context) error { return failure })
	require.ErrorIs(t, err, failure)
}
