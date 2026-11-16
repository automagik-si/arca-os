package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/rivo/tview"

	apiseed "github.com/lxc/incus-os/incus-osd/api/seed"
	"github.com/lxc/incus-os/incus-osd/internal/tui"
)

const installConfirmation = "ERASE"

// installDevice describes the read-only lsblk snapshot shown before erasing a disk.
type installDevice struct {
	Path         string          `json:"path"`
	Type         string          `json:"type"`
	Size         uint64          `json:"size"`
	Model        string          `json:"model"`
	Serial       string          `json:"serial"`
	PTType       string          `json:"pttype"`
	FSType       string          `json:"fstype"`
	PartTypeName string          `json:"parttypename"`
	Label        string          `json:"label"`
	PartLabel    string          `json:"partlabel"`
	PartUUID     string          `json:"partuuid"`
	Children     []installDevice `json:"children"`
}

func withTargetReinstallSafety(config *apiseed.Install, osName string, osVersion string, target installDevice, usrOnTarget bool, proceed func() error) error {
	installed := usrOnTarget
	esps := []string{}
	release := osName + "_" + osVersion

	var inspect func(installDevice)

	inspect = func(device installDevice) {
		if device.PartLabel == release || device.PartLabel == release+"_verity" || device.PartLabel == release+"_verity_sig" {
			installed = true
		}

		if device.PartLabel == "esp" {
			esps = append(esps, device.PartUUID)
		}

		for _, child := range device.Children {
			inspect(child)
		}
	}

	inspect(target)

	if !installed {
		return proceed()
	}

	if len(esps) != 1 {
		return errors.New("cannot identify existing installation ESP; refusing to overwrite")
	}

	return withReinstallSafety(config, osName, esps[0], proceed)
}

func readInstallDevice(ctx context.Context, device string) (installDevice, error) {
	output, err := subprocess.RunCommandContext(ctx, "lsblk", "--json", "--tree", "--bytes", "--paths", "--output", "PATH,TYPE,SIZE,MODEL,SERIAL,PTTYPE,FSTYPE,PARTTYPENAME,LABEL,PARTLABEL,PARTUUID", device)
	if err != nil {
		return installDevice{}, fmt.Errorf("cannot inspect target; installation aborted without erasing any data: %w", err)
	}

	var devices struct {
		BlockDevices []installDevice `json:"blockdevices"`
	}

	err = json.Unmarshal([]byte(output), &devices)
	if err != nil {
		return installDevice{}, err
	}

	if len(devices.BlockDevices) != 1 || devices.BlockDevices[0].Size == 0 {
		return installDevice{}, errors.New("cannot identify target; installation aborted without erasing any data")
	}

	return devices.BlockDevices[0], nil
}

func inspectInstallDevice(ctx context.Context, device string) (string, error) {
	target, err := readInstallDevice(ctx, device)
	if err != nil {
		return "", err
	}

	return renderInstallPlan(target), nil
}

func renderInstallPlan(device installDevice) string {
	var plan strings.Builder

	_, _ = plan.WriteString("WARNING: Everything on the target disk shown below will be erased.\n" +
		"The TPM will also be reset at the end of the install, destroying any keys or\n" +
		"credentials stored in it. This also affects OTHER disks in this machine that\n" +
		"the installer does not erase: an encrypted disk (BitLocker, LUKS) that\n" +
		"unlocks with this TPM will then ask for its recovery key or passphrase.\n" +
		"Before typing ERASE, save recovery keys or passphrases off this machine.\n" +
		"If none exists, add one before installing. Esc cancels.\n\n")
	fmt.Fprintf(&plan, "Target: %s\nModel: %s\nSize: %.2f GiB (%d bytes)\nSerial: %s\nPartition table: %s\n", device.Path, readableValue(device.Model), float64(device.Size)/(1024*1024*1024), device.Size, readableValue(device.Serial), readableValue(device.PTType))
	_, _ = plan.WriteString("Current contents (OS names, when detected, appear in labels/types):\n")
	writeInstallContents(&plan, device, "")

	return strings.Join(tview.WordWrap(plan.String(), 78), "\n")
}

func writeInstallContents(plan *strings.Builder, device installDevice, indent string) {
	fmt.Fprintf(plan, "%s%s: %s, %.2f GiB, filesystem: %s, type: %s, label: %s, partition label: %s\n", indent, device.Path, device.Type, float64(device.Size)/(1024*1024*1024), readableValue(device.FSType), readableValue(device.PartTypeName), readableValue(device.Label), readableValue(device.PartLabel))

	for _, child := range device.Children {
		writeInstallContents(plan, child, indent+"  ")
	}
}

func readableValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none detected"
	}

	return value
}

type destructionConfirmation struct {
	inspect func(context.Context, string) (string, error)
	update  func(string)
	prompt  func(context.Context, string) (string, error)
}

// run keeps the destructive operation behind both the plan and the exact confirmation.
func (c destructionConfirmation) run(ctx context.Context, device string, seedAnswer string, destroy func() error) error {
	plan, err := c.inspect(ctx, device)
	if err != nil {
		return err
	}

	plan += "\nType ERASE exactly and press Enter to continue.\nEmpty or wrong input, Ctrl-C, Esc, or a 5 minute timeout aborts.\nUse Up/Down or PageUp/PageDown to review the contents."
	c.update(plan)

	answer := seedAnswer
	if seedAnswer == installConfirmation {
		slog.WarnContext(ctx, "WARNING: AUTOMATION-ONLY install seed confirmation override is erasing the target and resetting the TPM without operator input; other disks may need recovery keys or passphrases", "device", device)
	} else {
		promptCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		answer, err = c.prompt(promptCtx, plan)
		if err == nil {
			err = promptCtx.Err()
		}
	}

	if err == nil {
		err = ctx.Err()
	}

	if err != nil || answer != installConfirmation {
		message := "Installation aborted: confirmation was not received; no disk data was erased."
		c.update(message)
		slog.WarnContext(ctx, message)

		if err != nil {
			return fmt.Errorf("%s %w", message, err)
		}

		return errors.New(strings.TrimSuffix(message, "."))
	}

	return destroy()
}

func confirmTargetDestruction(ctx context.Context, modal *tui.Modal, device string, seedAnswer string, destroy func() error) error {
	confirmation := destructionConfirmation{
		inspect: inspectInstallDevice,
		update:  modal.Update,
		prompt:  modal.Prompt,
	}

	return confirmation.run(ctx, device, seedAnswer, destroy)
}

func withReinstallSafety(config *apiseed.Install, osName string, targetESP string, proceed func() error) error {
	if !config.ForceInstall {
		return fmt.Errorf("%s is already installed on this system, and `ForceInstall` from install configuration isn't true", osName)
	}

	partUUID, err := uuid.Parse(targetESP)
	if err != nil || partUUID == uuid.Nil {
		return errors.New("cannot read existing installation ESP PARTUUID; refusing to overwrite")
	}

	confirmationValue := partUUID.String()[:6]

	switch config.ForceInstallConfirmation {
	case "":
		return fmt.Errorf("%s is already installed on this system; to confirm overwriting, add `ForceInstallConfirmation=%s` (first six characters of the target ESP PARTUUID) to the install seed and reboot", osName, confirmationValue)
	case confirmationValue, "FORCE-WIPE-I-KNOW-WHAT-IAM-DOING":
		return proceed()
	default:
		return fmt.Errorf("%s is already installed on this system; the value of `ForceInstallConfirmation` '%s' != '%s' (first six characters of the target ESP PARTUUID), refusing to continue", osName, config.ForceInstallConfirmation, confirmationValue)
	}
}
