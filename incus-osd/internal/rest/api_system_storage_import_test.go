package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
)

type protectedSystem struct{}

func (protectedSystem) Drives(context.Context) ([]api.SystemStorageDrive, error) {
	return []api.SystemStorageDrive{{ID: "/dev/disk/by-id/ata-RECOVERY_DISK", SerialNumber: "RECOVERY1"}}, nil
}

func (protectedSystem) ProtectedSerials() []string { return []string{"RECOVERY1"} }

func (protectedSystem) PoolExists(context.Context, string) bool { return false }

func (protectedSystem) WipeDrive(context.Context, string) error {
	panic("a protected drive reached the wipe")
}

func (protectedSystem) CreatePool(context.Context, string, string) error {
	panic("a protected drive reached the pool")
}

func (protectedSystem) Space(context.Context, string) (int64, int64, bool, error) {
	return 0, 0, false, nil
}

// The protected list is what stands between a typo and the owner's recovery drive.
func TestWipeAndEncryptRefuseProtectedDrive(t *testing.T) {
	t.Parallel()

	s := &Server{importSystem: protectedSystem{}}

	for name, handler := range map[string]http.HandlerFunc{"wipe-drive": s.apiSystemStorageWipeDrive, "encrypt-drive": s.apiSystemStorageEncryptDrive} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/1.0/system/storage/:"+name, strings.NewReader(`{"id":"/dev/disk/by-id/ata-RECOVERY_DISK"}`))
			rec := httptest.NewRecorder()

			handler(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), `drive \"RECOVERY1\" is protected`)
		})
	}
}
