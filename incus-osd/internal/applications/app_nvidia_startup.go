package applications

import (
	"errors"
	"os"
	"path/filepath"
)

const nvidiaStartedFile = "/run/incus-os/nvidia-started"

// MarkNVIDIABootComplete prevents startup health probes after a daemon restart in the same boot.
func MarkNVIDIABootComplete() error {
	_, err := claimNVIDIABoot(nvidiaStartedFile)

	return err
}

func claimNVIDIABoot(path string) (bool, error) {
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		return false, err
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return true, file.Close()
}

func (n *nvidia) bootStarted() bool {
	_, err := os.Lstat(n.path(nvidiaStartedFile))

	return !errors.Is(err, os.ErrNotExist)
}
