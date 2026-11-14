package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	cli "github.com/lxc/incus/v7/shared/cmd"
	"github.com/spf13/cobra"

	"github.com/lxc/incus-os/incus-osd/api"
)

// IncusOS system command.
type cmdAdminOSSystem struct {
	os *cmdAdminOS
}

//nolint:revive
func (c *cmdAdminOSSystem) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = cli.Usage("system")
	cmd.Short = "Manage IncusOS system details"
	cmd.Long = cli.FormatSection("Description", "Manage IncusOS system details")

	// Backup.
	backupCmd := cmdGenericRun{
		os:            c.os,
		action:        "backup",
		description:   "Backup the system",
		endpoint:      "system",
		hasData:       true,
		defaultData:   "{}",
		hasFileOutput: true,
	}
	cmd.AddCommand(backupCmd.command())

	// Factory reset.
	factoryResetCmd := cmdGenericRun{
		os:          c.os,
		action:      "factory-reset",
		description: "Factory reset the system",
		endpoint:    "system",
		hasData:     true,
		defaultData: "{}",
		confirm:     "factory-reset the system",
	}
	cmd.AddCommand(factoryResetCmd.command())

	// Power off.
	poweroffCmd := cmdGenericRun{
		os:          c.os,
		action:      "poweroff",
		description: "Power off the system",
		endpoint:    "system",
		hasData:     true,
		confirm:     "power off the system",
	}
	cmd.AddCommand(poweroffCmd.command())

	// Reboot.
	rebootCmd := cmdGenericRun{
		os:          c.os,
		action:      "reboot",
		description: "Reboot the system",
		endpoint:    "system",
		hasData:     true,
		confirm:     "reboot the system",
	}
	cmd.AddCommand(rebootCmd.command())

	// Restore.
	restoreCmd := cmdGenericRun{
		os:           c.os,
		action:       "restore",
		description:  "Restore a system backup",
		endpoint:     "system",
		hasFileInput: true,
		confirm:      "restore the system state to provided backup",
		extraArgs: []cmdGenericRunArgs{
			{
				shortFlag:   "s",
				longFlag:    "skip",
				description: "Comma-separated list of items to skip",
			},
		},
	}
	cmd.AddCommand(restoreCmd.command())

	// Suspend.
	suspendCmd := cmdGenericRun{
		os:          c.os,
		action:      "suspend",
		description: "Suspend the system",
		endpoint:    "system",
		confirm:     "suspend the system",
	}
	cmd.AddCommand(suspendCmd.command())

	// Add sub-commands.
	type subCommand struct {
		name          string
		description   string
		isWritable    bool
		extraCommands func() []*cobra.Command
		info          func(os *cmdAdminOS, endpoint string, description string) *cobra.Command
	}

	subCommands := []subCommand{
		{
			name:        "fallback-listener",
			description: "System fallback HTTPS listener configuration",
			isWritable:  true,
		},
		{
			name:        "kernel",
			description: "System kernel configuration",
			isWritable:  true,
			info:        systemInfoKernelCommand,
		},
		{
			name:        "logging",
			description: "System logging",
			isWritable:  true,
		},
		{
			name:        "network",
			description: "Network configuration",
			isWritable:  true,
			info:        systemInfoNetworkCommand,
			extraCommands: func() []*cobra.Command {
				// Confirm new network configuration.
				networkConfirmCmd := cmdGenericRun{
					os:          c.os,
					action:      "confirm",
					description: "Confirm the new network configuration",
					endpoint:    "system/network",
				}

				// Flush DNS cache.
				flushDNSCmd := cmdGenericRun{
					os:          c.os,
					action:      "flush-dns",
					description: "Flush the DNS cache",
					endpoint:    "system/network",
				}

				return []*cobra.Command{networkConfirmCmd.command(), flushDNSCmd.command()}
			},
		},
		{
			name:        "provider",
			description: "Image and management provider",
			isWritable:  true,
		},
		{
			name:        "resources",
			description: "System resources",
			isWritable:  false,
			info:        systemInfoResourcesCommand,
		},
		{
			name:        "security",
			description: "Security configuration",
			isWritable:  true,
			extraCommands: func() []*cobra.Command {
				// Recovery keys retrieved.
				retrievedCmd := cmdGenericRun{
					os:          c.os,
					action:      "retrieved",
					description: "Mark the encryption recovery keys as retrieved",
					endpoint:    "system/security",
				}

				// TPM rebind.
				tpmRebindCmd := cmdGenericRun{
					os:          c.os,
					action:      "tpm-rebind",
					description: "Rebind the TPM (after using recovery key)",
					endpoint:    "system/security",
					confirm:     "rebind the TPM and reboot the system",
				}

				return []*cobra.Command{retrievedCmd.command(), tpmRebindCmd.command()}
			},
		},
		{
			name:        "storage",
			description: "Storage configuration",
			isWritable:  true,
			info:        systemInfoStorageCommand,
			extraCommands: func() []*cobra.Command {
				// Clean up the root partition.
				cleanupRootCmd := cmdGenericRun{
					os:          c.os,
					action:      "cleanup-root",
					description: "Clean up the root partition",
					endpoint:    "system/storage",
				}

				// Create storage volume.
				createVolumeCmd := cmdGenericRun{
					os:          c.os,
					name:        "create-volume",
					description: "Create a storage volume",
					action:      "create-volume",
					endpoint:    "system/storage",
					hasData:     true,
				}

				// Delete storage pool.
				deletePoolCmd := cmdGenericRun{
					os:          c.os,
					name:        "delete",
					description: "Delete the storage pool",
					action:      "delete-pool",
					endpoint:    "system/storage",
					hasData:     true,
					confirm:     "delete the storage pool",
				}

				// Delete storage volume.
				deleteVolumeCmd := cmdGenericRun{
					os:          c.os,
					name:        "delete-volume",
					description: "Delete the storage volume",
					action:      "delete-volume",
					endpoint:    "system/storage",
					hasData:     true,
					confirm:     "delete the storage volume",
				}

				// Encrypt drive.
				encryptDriveCmd := cmdGenericRun{
					os:          c.os,
					action:      "encrypt-drive",
					description: "Encrypt the drive",
					endpoint:    "system/storage",
					hasData:     true,
					confirm:     "wipe and encrypt the drive",
				}

				// Import encrypted drive.
				importEncryptedDriveCmd := cmdGenericRun{
					os:          c.os,
					action:      "import-encrypted-drive",
					description: "Import an existing encrypted drive",
					endpoint:    "system/storage",
					hasData:     true,
				}

				// Import storage pool.
				importPoolCmd := cmdGenericRun{
					os:          c.os,
					name:        "import-storage-pool",
					description: "Import an existing encrypted storage pool",
					action:      "import-pool",
					endpoint:    "system/storage",
					hasData:     true,
				}

				// Wipe drive.
				wipeDriveCmd := cmdGenericRun{
					os:          c.os,
					action:      "wipe-drive",
					description: "Wipe the drive",
					endpoint:    "system/storage",
					hasData:     true,
					confirm:     "wipe the drive",
				}

				// Scrub pool.
				scrubPoolCmd := cmdGenericRun{
					os:          c.os,
					action:      "scrub-pool",
					description: "Scrub the storage pool",
					endpoint:    "system/storage",
					hasData:     true,
				}

				// Trim pool.
				trimPoolCmd := cmdGenericRun{
					os:          c.os,
					action:      "trim-pool",
					description: "Trim the storage pool",
					endpoint:    "system/storage",
					hasData:     true,
				}

				// Rescan SCSI buses.
				rescanSCSICmd := cmdGenericRun{
					os:          c.os,
					action:      "rescan-scsi",
					description: "Rescan the SCSI buses for new devices",
					endpoint:    "system/storage",
				}

				return []*cobra.Command{cleanupRootCmd.command(), createVolumeCmd.command(), deletePoolCmd.command(), deleteVolumeCmd.command(), encryptDriveCmd.command(), importEncryptedDriveCmd.command(), importPoolCmd.command(), wipeDriveCmd.command(), scrubPoolCmd.command(), trimPoolCmd.command(), rescanSCSICmd.command()}
			},
		},
		{
			name:        "update",
			description: "Update configuration",
			isWritable:  true,
			extraCommands: func() []*cobra.Command {
				// Check updates.
				checkUpdatesCmd := cmdGenericRun{
					os:          c.os,
					action:      "check",
					name:        "check",
					description: "Check for updates and apply them",
					endpoint:    "system/update",
					hasData:     true,
					confirm:     "apply any pending update",
					defaultData: "{}",
				}

				return []*cobra.Command{checkUpdatesCmd.command()}
			},
		},
	}

	for _, sub := range subCommands {
		subCmd := &cobra.Command{}
		subCmd.Use = cli.Usage(sub.name)
		subCmd.Short = sub.description
		subCmd.Long = cli.FormatSection("Description", sub.description)

		if sub.isWritable {
			// Edit.
			editCmd := cmdGenericEdit{os: c.os, endpoint: "system/" + sub.name, entityShort: "configuration"}
			subCmd.AddCommand(editCmd.command())
		}

		// Show.

		// Define a custom post-function for the "security" sub-command that will
		// display a note about marking encryption keys as retrieved if they have
		// not already been marked as such.
		f := func(data json.RawMessage) {
			var sec api.SystemSecurity

			err := json.Unmarshal(data, &sec)
			if err == nil {
				for _, key := range sec.Config.EncryptionRecoveryKeys {
					groups := passphraseGroups(key)
					if groups != nil {
						_, _ = fmt.Fprintf(os.Stderr, "\nRecovery passphrase, to read over:\n  %s\n", strings.Join(groups, "\n  "))
					}
				}

				if !sec.State.EncryptionRecoveryKeysRetrieved {
					_, _ = fmt.Fprintf(os.Stderr, "\nNOTE: Once you have securely recorded the recovery keys, run\n      `%s admin os system security retrieved` to mark that they have\n      been manually retrieved.\n", os.Args[0])
				}
			}
		}

		switch sub.name {
		case "security":
		case "update":
			f = func(data json.RawMessage) { printUpdateNote(os.Stderr, data) }

		default:
			f = nil
		}

		showCmd := cmdGenericShow{os: c.os, endpoint: "system/" + sub.name, postFunc: f}
		subCmd.AddCommand(showCmd.command())

		// Info.
		if sub.info != nil {
			subCmd.AddCommand(sub.info(c.os, "system/"+sub.name, "Show "+sub.description))
		}

		cmd.AddCommand(subCmd)

		// Extra commands.
		if sub.extraCommands != nil {
			for _, extraCmd := range sub.extraCommands() {
				subCmd.AddCommand(extraCmd)
			}
		}
	}

	// Workaround for subcommand usage errors. See: https://github.com/spf13/cobra/issues/706.
	cmd.Args = cobra.NoArgs
	cmd.Run = func(cmd *cobra.Command, _ []string) { _ = cmd.Usage() }

	return cmd
}

// passphraseGroups returns a generated recovery passphrase (eight lowercase words) in
// lines of four words, or nil for any other key.
func passphraseGroups(key string) []string {
	words := strings.Split(key, " ")
	if len(words) != 8 {
		return nil
	}

	for _, w := range words {
		if w == "" || strings.Trim(w, "abcdefghijklmnopqrstuvwxyz") != "" {
			return nil
		}
	}

	return []string{strings.Join(words[:4], " "), strings.Join(words[4:], " ")}
}

// printUpdateNote prints a note on the last failed update check, if any.
func printUpdateNote(w io.Writer, data json.RawMessage) {
	var update api.SystemUpdate

	err := json.Unmarshal(data, &update)
	if err != nil {
		return
	}

	_, _ = fmt.Fprint(w, updateFailureNote(update.State))
}

// updateFailureNote describes the last failed update check, with both CAs when the update
// server isn't trusted, or returns "" when the last check succeeded.
func updateFailureNote(st api.SystemUpdateState) string {
	if st.LastErrorKind == "" {
		return ""
	}

	note := "\nUpdate status: " + st.Status + "\n  Last error (" + st.LastErrorKind + "): " + st.LastError + "\n"

	if st.EmbeddedCAFingerprint != "" || st.PresentedCAFingerprint != "" {
		note += "  Embedded CA:  " + orUnknown(st.EmbeddedCAFingerprint) + "\n"
		note += "  Presented CA: " + orUnknown(st.PresentedCAFingerprint) + "\n"
	}

	if st.TrustFailures > 0 {
		note += fmt.Sprintf("  Consecutive trust failures: %d\n", st.TrustFailures)
	}

	return note
}

func orUnknown(v string) string {
	if v == "" {
		return "unknown"
	}

	return v
}
