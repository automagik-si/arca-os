package console

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
)

// testSecrets are the values no screen may hold.
var testSecrets = []string{"CANARY-abc123", "OWNER-SECRET", "TOKEN-SECRET", "PRIVATE KEY", "Sup3r-Secret-Phrase"}

// secretInstance is one instance whose configuration, environment and devices
// hold values outside the details allowlist.
func secretInstance() api.InstanceFull {
	return api.InstanceFull{
		Instance: api.Instance{
			InstancePut: api.InstancePut{
				Description: "debian/13",
				Devices: api.DevicesMap{
					"root": {"type": "disk", "path": "/", "pool": "local"},
					"tpm":  {"type": "tpm", "path": testSecrets[4]},
				},
			},
			Name:       "web",
			Project:    "default",
			Type:       "container",
			Status:     "Running",
			CreatedAt:  testNow.Add(-48 * time.Hour),
			LastUsedAt: testNow.Add(-2 * time.Hour),
			ExpandedConfig: map[string]string{
				"image.description":   "debian/13",
				"user.secret-probe":   testSecrets[0],
				"user.droid.owner":    testSecrets[1],
				"environment.TOKEN":   testSecrets[2],
				"limits.memory":       "1GiB",
				"security.privileged": "false",
			},
		},
		State: &api.InstanceState{
			Status:    "Running",
			Processes: 31,
			CPU:       api.InstanceStateCPU{Usage: int64(12 * time.Second)},
			Memory:    api.InstanceStateMemory{Usage: 212 << 20, UsagePeak: 300 << 20},
			Disk:      map[string]api.InstanceStateDisk{"root": {Usage: 3 << 30}},
			Network: map[string]api.InstanceStateNetwork{
				"lo":   {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Address: "127.0.0.1", Scope: "local"}}},
				"eth0": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Address: "10.42.0.2", Scope: "global"}}, Counters: api.InstanceStateNetworkCounters{BytesReceived: 1200, BytesSent: 400}},
			},
		},
	}
}

func TestDetailsAllowlist(t *testing.T) {
	t.Parallel()

	d := toDetails(secretInstance(), 4<<30)
	text := strings.Join(detailsLines(d), "\n")

	for _, want := range []string{"web", "CT", "default", "debian/13", "Running", "31", "12s", "212M", "1G", "eth0", "10.42.0.2"} {
		if !strings.Contains(text, want) {
			t.Errorf("the details lack %q:\n%s", want, text)
		}
	}

	for _, secret := range testSecrets {
		if strings.Contains(text, secret) {
			t.Errorf("the details hold %q:\n%s", secret, text)
		}
	}

	// The same page drawn on a screen holds none of them either.
	snap := tracerSnapshot()
	snap.Page = pageDetails
	snap.Details = []InstanceDetails{d}
	snap.Instances = []Instance{{Name: d.Name, Project: d.Project, Type: d.Type, Status: d.Status}}
	snap.Selected = 0

	screen := draw(t, snap, 80, 25)
	golden(t, "details-80x25.txt", screen)

	got := screenText(screen)
	if !strings.Contains(got, "debian/13") {
		t.Errorf("the details page does not show the description:\n%s", got)
	}

	for _, secret := range testSecrets {
		if strings.Contains(got, secret) {
			t.Errorf("the details page holds %q:\n%s", secret, got)
		}
	}
}

func TestGoldenNoSecrets(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob(filepath.Join("testdata", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}

	if len(paths) == 0 {
		t.Fatal("no goldens to check")
	}

	for _, path := range paths {
		body, err := os.ReadFile(path) //nolint:gosec
		if err != nil {
			t.Fatal(err)
		}

		for _, secret := range testSecrets {
			if strings.Contains(string(body), secret) {
				t.Errorf("%s holds %q", path, secret)
			}
		}
	}
}
