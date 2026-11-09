package applications

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGPUSupportModules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		nvidiaAvailable bool
		blacklist       []string
		want            []string
	}{
		{
			name:            "prefer open NVIDIA modules",
			nvidiaAvailable: true,
			want:            []string{"amdgpu", "i915", "intel_vpu", "nvidia", "xe"},
		},
		{
			name: "nouveau fallback without NVIDIA modules",
			want: []string{"amdgpu", "i915", "intel_vpu", "nouveau", "xe"},
		},
		{
			name:            "blacklisted NVIDIA does not fall back to nouveau",
			nvidiaAvailable: true,
			blacklist:       []string{"nvidia", "i915", "intel-vpu"},
			want:            []string{"amdgpu", "xe"},
		},
		{
			name:      "blacklisted nouveau is not reloaded",
			blacklist: []string{"nouveau"},
			want:      []string{"amdgpu", "i915", "intel_vpu", "xe"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, gpuSupportModules(tt.nvidiaAvailable, tt.blacklist))
		})
	}
}
