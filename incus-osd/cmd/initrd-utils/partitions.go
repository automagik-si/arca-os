package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// checkPartitionUUIDs warns about ambiguous ESP PARTUUIDs without waiting for devices or changing them.
func checkPartitionUUIDs(w io.Writer, scanRoot string) {
	partitions, err := filepath.Glob(filepath.Join(scanRoot, "*", "uevent"))
	if err != nil {
		return
	}

	devices := map[string][]string{}

	for _, partition := range partitions {
		uevent, err := os.ReadFile(partition)
		if err != nil {
			continue
		}

		var uuid, name string

		for line := range strings.SplitSeq(string(uevent), "\n") {
			value, found := strings.CutPrefix(line, "PARTUUID=")
			if found {
				uuid = strings.ToLower(value)
			}

			value, found = strings.CutPrefix(line, "PARTNAME=")
			if found {
				name = value
			}
		}

		if name == "esp" && uuid != "" {
			devices[uuid] = append(devices[uuid], "/dev/"+filepath.Base(filepath.Dir(partition)))
		}
	}

	duplicates := []string{}

	for uuid, names := range devices {
		if len(names) > 1 {
			duplicates = append(duplicates, uuid)
		}
	}

	slices.Sort(duplicates)

	for _, uuid := range duplicates {
		_, _ = fmt.Fprintf(w, "WARNING: duplicate PARTUUID %s on %s; continuing boot.\n", uuid, strings.Join(devices[uuid], ", "))
	}
}
