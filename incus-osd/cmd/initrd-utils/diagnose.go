package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/lxc/incus-os/incus-osd/internal/secureboot"
)

// diagnoseTPM prints, in plain words, why the TPM will not unlock the root volume and the way back,
// comparing the seal state record with the boot's measurements. It prints nothing when they match.
// Arguments, all optional: the record, the efivars directory, the PCR directory and the root LUKS device.
func diagnoseTPM(w io.Writer, args []string) {
	paths := []string{secureboot.SealStateFile, "/sys/firmware/efi/efivars", "/sys/class/tpm/tpm0/pcr-sha256", "/dev/gpt-auto-root-luks"}
	copy(paths, args)

	// An unreadable record is as good as none: the owner gets the generic hint.
	record, err := secureboot.ReadSealState(paths[0])
	if err != nil {
		record = nil
	}

	current := secureboot.ReadMeasurements(paths[1], paths[2])
	cause := secureboot.DiagnoseSeal(record, current)

	// A volume systemd-repart just created holds only its TPM keyslot; its first boot has nothing to explain.
	if cause == secureboot.SealRecordMissing {
		slots, err := luksKeyslots(paths[3])
		if err == nil && slots < 2 {
			return
		}
	}

	for _, line := range secureboot.SealMessage(cause, record, current) {
		_, _ = fmt.Fprintln(w, line)
	}
}

// luksKeyslots returns the number of keyslots in a LUKS2 header.
func luksKeyslots(device string) (int, error) {
	f, err := os.Open(device)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	binaryHeader := make([]byte, 4096)

	_, err = io.ReadFull(f, binaryHeader)
	if err != nil {
		return 0, err
	}

	if !bytes.Equal(binaryHeader[:6], []byte("LUKS\xba\xbe")) || binary.BigEndian.Uint16(binaryHeader[6:8]) != 2 {
		return 0, errors.New("not a LUKS2 header")
	}

	headerSize := binary.BigEndian.Uint64(binaryHeader[8:16])
	if headerSize <= 4096 || headerSize > 4<<20 {
		return 0, fmt.Errorf("bad LUKS2 header size %d", headerSize)
	}

	area := make([]byte, headerSize-4096)

	_, err = io.ReadFull(f, area)
	if err != nil {
		return 0, err
	}

	end := bytes.IndexByte(area, 0)
	if end >= 0 {
		area = area[:end]
	}

	var metadata struct {
		Keyslots map[string]json.RawMessage `json:"keyslots"`
	}

	err = json.Unmarshal(area, &metadata)
	if err != nil {
		return 0, err
	}

	return len(metadata.Keyslots), nil
}
