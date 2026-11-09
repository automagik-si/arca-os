package systemd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNVIDIAKernelCompatibility(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateNVIDIAKernel("test-kernel\n", "test-kernel\n"))
	require.ErrorContains(t, validateNVIDIAKernel("previous-kernel\n", "test-kernel\n"), "does not match running kernel")
	require.Error(t, validateNVIDIAKernel("", "test-kernel"))
}
