package storagehealth

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lxc/incus-os/incus-osd/api"
)

// readLog returns the recorded smartctl JSON of a drive.
func readLog(t *testing.T, name string) []byte {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}

	return body
}

func TestNVMeHealth(t *testing.T) {
	t.Parallel()

	status := &api.SystemStorageDriveSMART{}
	Apply(status, readLog(t, "nvme-smart.json"))

	for _, row := range []struct {
		name string
		got  *int
		want int
	}{
		{"media_errors", status.MediaErrors, 0},
		{"available_spare_threshold", status.AvailableSpareThreshold, 10},
		{"warning_temp_c", status.WarningTempC, 82},
		{"critical_temp_c", status.CriticalTempC, 85},
		{"unsafe_shutdowns", status.UnsafeShutdowns, 7},
	} {
		if row.got == nil {
			t.Errorf("%s is absent, want %d", row.name, row.want)

			continue
		}

		if *row.got != row.want {
			t.Errorf("%s is %d, want %d", row.name, *row.got, row.want)
		}
	}
}

func TestSATAHealth(t *testing.T) {
	t.Parallel()

	status := &api.SystemStorageDriveSMART{}
	Apply(status, readLog(t, "sata-smart.json"))

	for name, got := range map[string]*int{
		"media_errors":              status.MediaErrors,
		"available_spare_threshold": status.AvailableSpareThreshold,
		"warning_temp_c":            status.WarningTempC,
		"critical_temp_c":           status.CriticalTempC,
		"unsafe_shutdowns":          status.UnsafeShutdowns,
	} {
		if got != nil {
			t.Errorf("a SATA drive reports %s = %d", name, *got)
		}
	}
}

func TestMalformedLog(t *testing.T) {
	t.Parallel()

	status := &api.SystemStorageDriveSMART{}

	// Malformed input leaves the document untouched, and a nil document is safe.
	Apply(status, []byte("{"))
	Apply(status, nil)
	Apply(nil, readLog(t, "nvme-smart.json"))

	if status.MediaErrors != nil || status.WarningTempC != nil {
		t.Error("malformed input set a health field")
	}
}
