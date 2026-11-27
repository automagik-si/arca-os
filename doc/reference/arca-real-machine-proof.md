---
orphan: true
---

# First real-machine proof

**Operating rule:** the proof machine's existing disks stay untouched and its
existing system stays bootable as the way back. The new SSD is the only
install target. The laptop that already runs Arca OS takes no part in this
proof: it is not the update server and is never reinstalled for it. At most,
it receives one normal update at the very end, with the owner's explicit
word, and it restarts only if the owner approved that restart beforehand.

"Untouched" describes the bytes on the old disks. Two things the old system
shares with the new one do change: the firmware's Secure Boot keys, and the
TPM, which the installer resets at the end of every completed install.
**That reset reaches disks the installer never erases:** a volume on any disk
that is unlocked only by the TPM can no longer be opened afterwards, although
not one byte of it was written. The hard gate below exists for that reason.

This is a written plan, not a record of a completed installation. Use already
published production installer and release artifacts. Do not build images as
part of this run. Stop at the first failed prerequisite; a missing measurement
is not a pass. Record `PASS <step>` or `FAIL <step>: <reason>` after the gate,
each step and each NVIDIA subcheck. Record an unrun check as
`FAIL <step>: not run`.

## Hard gate: disks unlocked by the TPM

This gate comes before any firmware change and before any install. It is not
a decision line: without a recorded `PASS gate`, do not start.

The installer runs a TPM clear at the end of every completed install and
carries on when it fails, only logging the failure
(`incus-osd/internal/install/install.go:969-976`), so plan as if it always
works. The erase screen says so above the target, including that other disks
in the machine are affected (`incus-osd/internal/install/confirmation.go:61-67`).
A clear discards every key the TPM held or had sealed. A machine with another
disk whose encrypted volume is unlocked only by its TPM (for example a LUKS
volume with `tpm2-device=auto` in `/etc/crypttab`) therefore loses that unlock
on a successful install, while the disk itself is never touched and its
digest never changes. A change of Secure Boot state or keys breaks the same
unlock even earlier: such an unlock is normally bound to PCR 7, so the old
volume may ask for its recovery key from step 1b on, not only after the
install. Everything below therefore happens on the old system while its TPM
still unlocks, before step 1b.

1. List every encrypted volume on every disk, not only those in
   `/etc/crypttab`: `lsblk -o PATH,FSTYPE` and
   `sudo blkid -t TYPE=crypto_LUKS`. For each, read the key slots and tokens
   with `sudo cryptsetup luksDump <device>`. Every volume with a TPM-bound
   token is in scope (`systemd-tpm2`, or a `clevis` token using the TPM),
   including volumes on disks the installer will not erase. A BitLocker
   volume with a TPM protector is the same case under another system: it
   needs its recovery password (`manage-bde -protectors -get <drive>`),
   tested with that system's own tools.
2. Back up each volume's LUKS header to separate storage before changing it:
   `sudo cryptsetup luksHeaderBackup <device> --header-backup-file <file>`.
   Protect the file like a key: with it, a key that is removed later still
   opens the volume.
3. Each volume must have a recovery key that does not depend on the TPM,
   written down outside recordings and held by the team under the vault rule.
   Adding one needs the volume unlocked, and `systemd-cryptenroll` asks for
   an existing passphrase by default, which a TPM-only volume does not have.
   Use `sudo systemd-cryptenroll --unlock-tpm2-device=auto --recovery-key <device>`
   (systemd 256 or newer; check `systemctl --version`), or
   `--unlock-key-file=<file>` where a key file exists. If neither applies,
   the gate cannot be met with these tools: stop. Run `luksDump` again and
   write down the number of the new recovery key slot.
4. **Test every recovery key once, in its own slot and without the TPM:**

   ```sh
   sudo cryptsetup open --test-passphrase --disable-external-tokens \
     --key-slot <recovery slot> -v <device>
   ```

   The command must prompt, the key is typed from the stored copy, and the
   output must contain `Key slot <n> unlocked.` for the recovery slot. Without
   `--disable-external-tokens` the command unlocks through the TPM token and
   never prompts, so it proves nothing: a run that did not prompt, or that
   names another slot, is not a test. An enrolled but untested key does not
   pass.
5. The owner confirms that nothing else on the machine exists only in the TPM
   (a key an application created or sealed there), or that each such secret
   has a tested copy elsewhere. The owner confirms this again immediately
   before step 3 and before any reinstall in step 8.

**PASS/FAIL gate:** PASS only if every TPM-unlocked volume was found, has a
header backup and a recovery key in the vault, that key's own slot was
unlocked in the test above, and the owner gave the confirmation in item 5.
Otherwise FAIL; do not start.

## Before starting

The owner must settle these decisions and supply their values privately:

- **Installer:** the published installer is built from code that has the
  erase screen with its typed `ERASE` confirmation and its TPM reset warning.
  Both are on main (`incus-osd/internal/install/confirmation.go`); an
  installer published before them is not this plan's installer.
- **SSD arrival:** the new SSD has arrived, is installed, may be erased, and
  has no partition table. The installer refuses a target that already has
  one unless the seed sets `force_install`, which the first install here
  never does. If the SSD arrives with a partition table, stop: clearing it is
  a separate erase that needs the owner's word and the same identification.
- **Keys:** the production root keys are generated and held by the team in
  the vault; the owner confirms before they are generated. The team's key
  holders confirm the production PK and update CA and approve the published
  artifacts. They also confirm that the previous and the new release are
  signed with the same Secure Boot key, that no Secure Boot key update
  published with the new release revokes the previous one (step 8 boots it
  again), and whether the key set the installer enrols includes Microsoft's
  UEFI CA. Signing keys stay in the vault, never on the proof machine.
- **Bucket:** which public update bucket and server/channel are authoritative,
  and which two distinct published releases will be used. Call them the
  previous release and the new release. Retain both installers and updates
  until recovery has been proved; select `stable` for the proof machine.
  Also confirm how the previous release's applications are supplied during
  first boot, before the machine switches to the new release's update view.
- **TPM:** the owner accepts that a successful install resets the TPM, and
  confirms that the old system has no TPM-only secrets or that its recovery
  keys are in hand. The hard gate records the evidence.
- **Recovery:** the old system's firmware settings and recovery material are
  available. Once the installer has enrolled the Arca OS keys, the old system
  boots only with Secure Boot turned off; an unlock tied to the TPM or to the
  Secure Boot state then needs its recovery key. The original keys are
  exported in step 1; putting them back ends the new installation's own TPM
  unlock and is done only when that installation is abandoned. This plan has
  no command that writes the exported files back: the keys return through the
  firmware's own option to restore factory keys, or its page for loading key
  files. If the firmware has neither, record that before step 1b; the
  original keys then cannot be put back, and the owner decides whether to
  proceed. If the way back cannot be assured, do not start.
- **Display:** the firmware screens, boot menu and installer must be readable
  without the NVIDIA card: an onboard output, or a serial console verified to
  show firmware setup. Setup Mode removes every existing key, and a card's
  firmware display driver is normally signed by Microsoft's UEFI CA; once
  only the Arca OS keys are enrolled the firmware can refuse it and every
  screen before the operating system stays black. The only alternative is an
  explicit owner decision to keep Microsoft's UEFI CA in the firmware's `db`.
  That route widens what the firmware trusts and is recorded in step 4. It
  does not use Setup Mode: the keys are loaded by hand in firmware setup, and
  step 1b says what replaces its Setup Mode items and its PASS line. This
  plan gives no screen-by-screen procedure for it; the firmware's own pages
  and the [physical installation guide](../getting-started/installation/physical.md)
  are the reference.
- **Driver track:** the separate NVIDIA work has delivered a signed pack for
  the chosen OS kernel, an approved pinned branch/build, and a prebuilt CUDA
  test container with a known-answer workload and its exact invocation.

Bring a keyboard, the display path chosen above, wired network, the published
installer for the previous release, and separate recovery media. From the
board manual, write down the firmware (CMOS) reset procedure and what it does
to Secure Boot and to the TPM on this board (a reset can disable or clear a
firmware TPM) before the run, not during it. Check that the
firmware clock is correct: signature and TLS checks depend on it.
Prepare an authenticated management client with the matching Incus OS
commands and the client's trusted certificate in the install preseed; see
[access](../getting-started/access.md) and [Incus](applications/incus.md).
Keep credentials and recovery material outside console recordings.

The installer only installs when the seed contains an `install.yaml`, and
with more than one disk present it needs a target selector. Prepare this
one; see the [seed reference](seed.md):

```yaml
version: "1"
force_install: false
force_reboot: false
target:
  id: "<full /dev/disk/by-id name of the new SSD>"
```

`target.id` is a case-sensitive substring match, so use the full by-id name,
serial included, and check that no by-id name of any other disk contains it.
Leave `sort_order` unset: with more than one match the installer then stops
instead of choosing. Do not add `confirmation` or `force_install_confirmation`.

Prepare the same seed with both `incus` and `arca` applications and an
`update.yaml` containing these top-level fields (seed files have no `config`
wrapper):

```yaml
channel: stable
check_frequency: never
auto_updates_off: true
auto_reboot: false
```

These settings do not hold on first boot. With no primary application
installed yet, the first start checks for updates anyway, and an OS or Secure
Boot key update applied before the system is ready restarts the machine at
once. Establish beforehand what that check will select. From the management
client, fetch `index.sjson` from the server URL the seed's provider uses and
verify its signature against the production root and update CA, the way the
node does (`incus-osd/internal/util/ssl.go`). In its `updates` list, the
first entry whose `channels` contains `stable` and which has files for the
machine's architecture is the one the machine takes. Its `version` must be
the previous release, and its files must include both applications. If it is
the new release, stop: an older installer alone does not guarantee an older
baseline. Keep the fetched index with the private evidence. Do not change a
shared channel or rebuild images to arrange this proof.

The examples use `proof:` as a local Incus remote alias for the proof machine,
not as a host name. Add that remote using the access guide, check its server
certificate against the console, and authenticate with the preseeded client.
Run management commands from the management client, never against the
laptop's remote; the only exception is step 10. For raw API calls, `incus query` returns the response
metadata, so the fields below do not have an extra `metadata` wrapper.

Keep raw evidence privately on separate storage. Public results contain only
redacted observations: no host names, addresses, serial numbers, account or
personal names, credentials, keys, or recovery passphrases. Do not commit raw
disk inventories, logs, seed files, or screenshots containing those values.

## 1. Preconditions

1. Boot the old disk normally and confirm its usual system reaches a usable
   session. Shut it down cleanly. Record the original boot order and Secure
   Boot state privately. Confirm the old disk and the new SSD are both
   physically present and distinguishable by serial number, model, byte
   capacity and contents.
2. Before booting the installer, use separate trusted recovery media with
   automount, swap activation and automatic RAID and LVM assembly disabled.
   Do not mount or unlock the old disk. Inventory disks with
   `lsblk -b -o PATH,MODEL,SERIAL,SIZE,TYPE,PTTYPE,FSTYPE,MOUNTPOINTS` and
   `ls -l /dev/disk/by-id/`. Privately resolve the old disk's whole-device
   path into `OLD_DISK` and the new SSD's into `NEW_SSD`, and write down the
   new SSD's serial, model, byte size and by-id name: the erase screen and
   the seed use them. The new SSD must show no partition table and no
   file system. Confirm neither disk has mounted children or active swap
   (`findmnt` and `swapon --show`); unmounting an already written disk does
   not count as an untouched baseline. Start again from a clean shutdown.
   Every further disk in the machine is treated as an old disk for the
   identification, selector and read-only rules; whether a full digest of a
   large data disk is taken is an owner decision recorded before the run.
3. Mark the old disk read-only with `sudo blockdev --setro "$OLD_DISK"` and
   require `sudo blockdev --getro "$OLD_DISK"` to return `1`. Save
   `sudo blockdev --getsize64 "$OLD_DISK"` and a whole-device digest from
   `sudo sha256sum "$OLD_DISK"` privately on separate storage. Also save the
   new SSD's size and complete digest with the same read commands using
   `"$NEW_SSD"`, for the no-write rejection test. Wait for a
   successful exit; a partial read is not evidence. Read-only flags do not
   persist across boots: every later recovery-media session must repeat this
   protection and identify disks afresh. Never assume device paths stay fixed.
4. Export the firmware's Secure Boot variables to separate storage before any
   firmware key change: `PK`, `KEK`, `db` and `dbx`, for example with
   `efi-readvar -v <name> -o <name>.esl` for each. They hold certificates and
   hashes, no secrets. A variable that is empty today is recorded as empty.
5. Read the TPM event log with
   `sudo tpm2_eventlog /sys/kernel/security/tpm0/binary_bios_measurements`
   and look for `EV_EFI_BOOT_SERVICES_DRIVER` events (PCR 2): each is a driver the
   firmware loaded from an add-in card. If the display card is among them,
   the display decision applies in full. Whatever the log shows, prove the
   chosen display path now: firmware setup and the boot menu must be readable
   on it with the NVIDIA card's output unplugged.
6. Check TPM 2.0 is enabled and healthy. On the trusted recovery system,
   `sudo tpm2_getcap properties-fixed` must succeed and identify TPM family
   2.0. Do not clear or reset the TPM by hand to make this pass. In firmware,
   verify UEFI boot. Leave Secure Boot settings alone until step 1b.
7. Compare hardware with the [public requirements](../getting-started/requirements.md):
   supported modern architecture (including `x86_64_v3` for x86), UEFI Secure
   Boot, hardware TPM 2.0, at least 4 GiB system RAM, at least 50 GiB on the
   new SSD, and a wired network port. Confirm the cable is connected and the
   switch reports a link. Record the installer release and approved provenance.

**PASS/FAIL 1:** PASS only when the gate passed, every prerequisite is
checked, the old disk has booted, its offline inventory and full digest are
saved, the Secure Boot variables are exported, the display path is proved,
and the new SSD alone, without a partition table, is approved for erasure.
Otherwise FAIL and stop before any firmware change.

**Abort/recovery:** nothing has been changed yet. Remove recovery media and
use the firmware's one-time boot menu to boot the old disk. A broken way back
blocks this proof even when the new SSD could be installed.

## 1b. Secure Boot: enable it and enter Setup Mode

The machine may have Secure Boot disabled today, as a system installed
without it leaves it. This plan does not run degraded. With Secure Boot
disabled the installer refuses to install unless the seed explicitly allows
a degraded state (see [installing without Secure Boot](installing-without-secureboot.md)),
and a system that has ever booted that way reports
`system_state_is_trusted: false` until it is reinstalled, so step 4 could not
pass. Secure Boot is therefore enabled and brought to Setup Mode as a step of
its own. Entering Setup Mode removes every existing key.

1. Record today's state privately: the firmware's Secure Boot page (enabled
   or disabled, mode, which keys are present, whether it offers to restore
   factory keys) and, from the recovery system, the Secure Boot line of
   `bootctl status`. Confirm the old system boots in UEFI mode (`/sys/firmware/efi`
   exists on it). If it boots through legacy/CSM, stop: turning CSM off would
   leave it unbootable, and that is an owner decision outside this plan.
2. Confirm step 1's export is on separate storage and readable.
3. In firmware setup, enable Secure Boot, then enter Setup Mode with the
   firmware's own option; names vary, see the vendor notes in the
   [physical installation guide](../getting-started/installation/physical.md).
   Enrol nothing by hand. Change no TPM option on these pages.
4. Boot the recovery media through the one-time boot menu. `bootctl status`
   must report Setup Mode. Firmware setup and the boot menu must still be
   readable on the chosen display path.
5. Boot the old disk once in this state and require its usual session. An
   unlock bound to the TPM may refuse now, because the measured Secure Boot
   state changed; use the tested recovery key. Shut down cleanly, then repeat
   step 1's read-only digest of the old disk: step 2 compares against it.

On the route that keeps Microsoft's UEFI CA, items 3 and 4 are replaced. In
firmware setup, load the Arca OS PK, KEK and db keys from the installer
media's `keys` folder, remove everything else from `KEK` and `db` except
Microsoft's UEFI CA in `db`, and leave Secure Boot on in user mode. Nothing
is in Setup Mode afterwards and the installer enrols nothing. Item 5 is then
done with Secure Boot turned off, as the firmware rules below describe; from
the recovery system, `bootctl status` must show user mode and a fresh export
must list exactly the approved certificates plus that CA.

**PASS/FAIL 1b:** PASS only if the original state was recorded, the firmware
has Secure Boot switched on and is in Setup Mode with no key enrolled, the
display path still works, the old system still boots and unlocks (by itself or with
the tested recovery key), and its baseline was refreshed. On the route that
keeps Microsoft's UEFI CA, "in Setup Mode with no key enrolled" is replaced
by: in user mode, with the approved Arca OS keys and nothing else but that
CA. Otherwise FAIL and stop before the installer.

**Abort/recovery:** put the firmware back as recorded: restore the factory
keys with the firmware's own option if they were present, and set Secure Boot
to its recorded state. Boot the old disk. This step writes to no disk and
does not clear the TPM.

## Firmware rules once the installer has enrolled its keys

On its first boot in Setup Mode the installer media enrols the Arca OS keys
by itself, after a countdown, and restarts. From then on these rules hold
until the proof ends:

- The firmware accepts only what those keys sign. Recovery media and the old
  system do not boot while Secure Boot is on.
- To boot recovery media or the old disk, turn Secure Boot **off** in firmware
  setup and change no key. Do not restore keys, clear keys or enter Setup
  Mode again. If the firmware cannot turn Secure Boot off without changing
  keys, stop.
- Check that, the first time Secure Boot is turned off (step 2). From the
  recovery system, `bootctl status` must show Secure Boot disabled in user
  mode, not Setup Mode, and a fresh export of `PK`, `KEK`, `db` and `dbx`
  must hold the Arca OS keys the team's key holders confirm. Keep that export
  as the enrolled set. In every later session with Secure Boot off, a fresh
  export must be identical to it; after step 7, `KEK`, `db` and `dbx` may
  differ by a published key update, `PK` may not. Any other difference: stop.
- While Secure Boot is off, never let the new SSD or the installer boot. The
  installed system records a boot without Secure Boot as an untrusted state
  that only a reinstall removes. Always pick the device in the one-time boot
  menu; never rely on the boot order. If the new SSD starts by mistake in
  that state, it stops and asks for its recovery passphrase: power off, do
  not enter the passphrase.
- Before booting the new SSD or the installer, turn Secure Boot back on with
  the same keys.
- The installed system binds its disk unlock to the exact Secure Boot
  measurement (PCR 7). If the keys change, it asks for its recovery
  passphrase. Step 3 therefore retrieves that passphrase right after first
  boot, before Secure Boot is turned off for the first time.
- If every screen before the operating system goes black after enrolment,
  power off and use the display path from the display decision. If that also
  fails, apply the firmware reset procedure written down beforehand, then
  check what state Secure Boot and the TPM are in before booting anything. If Secure Boot
  is now off or the keys are gone, do not boot the new SSD: boot the old disk
  and record FAIL.

## 2. Installer confirmation and rejection test

The installer's erase screen opens with a warning that the TPM will also be
reset and that other disks in the machine are affected, then shows the
target. It requires a typed `ERASE` and aborts after five minutes without it
(`incus-osd/internal/install/confirmation.go`). An installer that shows no
such screen was published before that code: stop. Use the prepared `install.yaml`: its
`target` selector is restricted to the new SSD and cannot also match another
disk. Do not use a broad size-sort rule, `force_install` or a pre-answered
`confirmation`.

1. Boot the public installer through the proof machine's one-time boot menu.
   Setup Mode key enrollment restarts the proof machine before the installer
   starts. Capture the console, excluding secrets.
2. Read the erase screen. There is no list to choose from: it shows exactly
   one target with its path, model, size in bytes, serial, partition table
   and current contents. Compare the serial first, then model and byte size,
   with the values written down in step 1. The partition table must read
   `none detected`. A serial of `none detected`, any mismatch or any doubt is
   a FAIL; an inventory listing of the old disk is not permission to install
   to it. The screen aborts after five minutes: have the expected values in
   hand before booting, and let it abort rather than rush.
3. Deliberately type an answer other than `ERASE`. Require an explicit abort
   before disk writes. The installer may show the screen again after a short
   pause; do not answer it and do not type `ERASE` in this attempt. Power off.
   An aborted attempt does not reach the TPM clear.
4. Remove installer media. Turn Secure Boot off without changing keys, boot
   the trusted recovery media through the one-time boot menu with automount
   disabled, and repeat the read-only size and full-digest checks on the old
   disk. Compare with step 1b. Also compare the new SSD's complete digest,
   size and partition summary with its baseline; both disks must be unchanged.
   Before booting the old system, require the old disk's digest to match.
   Then boot the old disk, still with Secure Boot off, to confirm the way
   back still works.

**PASS/FAIL 2:** PASS only if the confirmation identified only the new SSD
by its serial, the wrong answer aborted without writes, and the old disk's
offline digest and boot test passed. Otherwise FAIL; do not proceed to a
real erasure.

**Abort/recovery:** cancel or power off while still awaiting confirmation,
remove install media, turn Secure Boot off and boot the old disk.
If any old-disk difference appears, preserve evidence and stop; do not
attempt a repair or reinstall on that disk.

## 3. Install on the new SSD

After the rejection test's old-system boot, shut down cleanly and repeat
step 1's offline baseline: normal operation of the old system can change its
digest. Fetch the update index again and require the same answer as before.
Turn Secure Boot back on with the same keys, then boot the installer.

Recheck the target from scratch inside the five-minute limit: serial first,
then model, byte size and contents. The warning at the top of the erase
screen says the TPM will also be reset and that this affects other disks in
the machine. Read it; the owner repeats the confirmation of the gate's item 5
now. Typing `ERASE` accepts both the erasure of the new SSD and that TPM
reset, which reaches the old system as the hard gate describes. Type `ERASE` **by hand on the proof
machine's console**, only for the new SSD. Wait for installation to finish.
Remove install and seed media when asked, then allow the proof machine to
boot its installed system without that media.

Record the installed version from the console and establish the `proof:`
management remote. **Before any firmware change**, retrieve the
[encryption recovery material](system/security.md) with
`incus admin os system security show proof:` into the owner's private store,
outside recordings, and only then run
`incus admin os system security retrieved proof:`.

Only now, and before any later boot of the old disk, shut down the proof
machine cleanly, turn Secure Boot off without changing keys, and use recovery
media to compare the old disk's complete offline digest and size with the
refreshed baseline. Turn Secure Boot back on and return to the new SSD. It
must unlock by itself. If it asks for the recovery passphrase, power off and
check in firmware setup that Secure Boot is really on before typing
anything. With Secure Boot on, the firmware changed the measurement: enter
the passphrase and record that for step 4.

**PASS/FAIL 3:** PASS only if installation completed, the new SSD boots
without installer media, its recovery material was stored before any
firmware change, and the old disk still matches the baseline.
Otherwise FAIL and stop.

**Abort/recovery (destructive step):** before `ERASE`, cancel without writes.
After confirmation, an interrupted installation may leave the new SSD
unbootable; do not assume its previous contents are recoverable. If safe,
allow writes to finish, shut down, remove media, turn Secure Boot off and
boot the old disk. After a completed install, assume the TPM was cleared:
the old system needs its tested recovery keys from then on. Retry only on
the new SSD after repeating target identification and confirmation.

## 4. Trust

Read the current boot journal privately with
`incus query 'proof:/os/1.0/debug/log?unit=incus-osd&boot=0'`.
The startup entry must report `mode=production`; `mode=dev`, `unsafe`, or a
missing startup entry fails. Read `incus query proof:/os/1.0/system/security`
privately: that endpoint also returns recovery secrets. Inspect only these
fields for the public result:

- `state.secure_boot_enabled` is `true`.
- The certificate with `type: PK` in `state.secure_boot_certificates` matches
  the Arca OS production PK approved by the team's key holders; a matching
  name alone is insufficient. Compare the fingerprint privately. Every `KEK`
  and `db` certificate listed is one they approve. If the owner decided to
  keep Microsoft's UEFI CA for the display, it appears here: record that
  decision with the result, and never describe the run as Arca OS keys only.
- `state.tpm_status` is `ok`, and encrypted root and swap volumes report
  `unlocked (TPM)` in `state.encrypted_volumes`, after the Secure Boot
  off-and-on round trip of step 3.
- `state.system_state_is_trusted` is `true`; review
  `state.system_state_status` for a contrary condition.

The recovery material was stored in step 3; `state.encryption_recovery_keys_retrieved`
must be `true`. Never publish the full
security response. Do not install a debug application or unlock a shell to
make a check work: that can invalidate the trusted state being measured.

**PASS/FAIL 4:** PASS only if mode is production, Secure Boot uses the
approved Arca OS PK, hardware TPM unlocking works, and security is trusted.
Otherwise FAIL; do not accept a degraded state for this proof.

**Abort/recovery:** save a redacted reason, stop updates, and return to the
old disk with Secure Boot off. Do not clear the TPM by hand, replace keys, or
rebind it speculatively.

## 5. Services and console

Run these read-only requests from the management client:

```sh
incus query proof:/os/1.0
incus query proof:/os/1.0/applications/incus
incus query proof:/os/1.0/applications/arca
incus query proof:/1.0
```

Require `environment.system_is_ready: true`, both applications initialized
with recorded nonempty `state.version`, and a successful Incus response with
`api_status`. For Arca, use an owner-provisioned viewer credential and client
configuration directed at the proof machine; keep their paths and contents
private. With `ARCA_CLIENT_CONFIG` and `ARCA_VIEWER_TOKEN_FILE` set locally:

```sh
arca call watch_report --config "$ARCA_CLIENT_CONFIG" \
  --token-file "$ARCA_VIEWER_TOKEN_FILE" --args '{}'
```

Require exit 0 and the expected `default` project in `projects[].project`.
Read the `incus` and `arca` unit logs using the journal request from step 4
with the corresponding `unit` query parameter. Require successful startup and no crash or
restart loop over at least 60 seconds. Confirm the local console renders on
the display path chosen in the display decision, accepts navigation and can
return to its normal status screen.

**PASS/FAIL 5:** PASS only if Incus and Arca are installed and active, both
calls answer correctly, the system is ready, and the console is usable.
Missing client credentials or an untestable Arca call is FAIL, not a waiver.

**Abort/recovery:** retain logs, stop at this release, and use the old disk
if access cannot be restored through the console or [fallback API](recovery.md).

## 6. Wired network and update-server reachability

Read `incus query proof:/os/1.0/system/network`. Identify the wired interface
privately; require a carrier/link indication, negotiated speed, assigned
address and usable route in `state`. Inspect its `state` and `speed` rather
than treating an address alone as a link test. Check the physical link LED.

Save the existing provider and update configuration privately, from
`incus admin os system provider show proof:` and
`incus admin os system update show proof:`. Using
`incus admin os system provider edit proof:`, select provider `images` and
set `config.config.server_url` to the owner-approved public update-server
base URL. Keep signed-metadata validation enabled. Use
`incus admin os system update edit proof:` to set `config.channel: stable`,
`config.auto_updates_off: true` and `config.auto_reboot: false` while staging
the controlled test. Preserve other fields. Do not leave the channel empty.
Save `provider show` again after the edit, and fetch and keep that server's
signed index as it stands before the check.

Where available, a fresh `state.connectivity.online: true` with an advanced
`last_check` confirms that the node's connectivity monitor reached the update
server. That monitor may be absent on wired-only machines and does not
verify signatures. In that case, measure reachability during step 7's manual
check: require successful signed-index processing in the update journal and
a newly advanced update `state.last_check`. Read all of step 7's prerequisites
before invoking that check: it can also install updates. Record steps 6 and 7
from that one operation rather than running an extra check. A fetch from the
management client alone does not prove this path.

**PASS/FAIL 6:** PASS only if the wired link is up and a fresh connectivity
monitor result or signed-index fetch proves the proof machine reached the
update server. Otherwise FAIL and
stop; if the manual check has started, use step 7's recovery procedure.

**Abort/recovery:** restore the saved provider/update settings with their
edit commands, keep automatic updates off and use the local console if the
network is lost. Do not change the laptop's network or restart it.

## 7. One real production update

This step changes the new SSD and applications. Record
`environment.os_version` from `incus query proof:/os/1.0`, and both application
versions from step 5. They must match the previous release selected by the
owner. Confirm the new release is different and newer, its published
manifest includes both applications, and the previous boot slot is retained.

Before checking, have the team's key holders confirm that the installed
release embeds the production update CA, that both releases are signed with
the same Secure Boot key, and that no Secure Boot key update offered with the
new release revokes the previous one. The provider must have no `update_ca`
override, unless its certificate has independently been confirmed as that
same production CA. HTTP/TLS reachability alone is not update verification.
The normal images provider verifies signed metadata using that CA; see
`incus-osd/internal/providers/provider_images.go` and its `VerifySMIME` path.
Do not substitute unsigned JSON or a development provider.

Run `incus admin os system update check proof:`. This is a check **and apply**
operation, including immediate application updates; it is not a dry run.
A published Secure Boot key update is applied first: it is written to the
firmware and the disk unlock is bound to the new measurement. From then on
the firmware no longer holds exactly what the installer enrolled, one more
reason never to restore or re-enrol keys afterwards.
Observe `incus query proof:/os/1.0/system/update` and the incus-osd journal.
Require successful signed-metadata processing, release downloads and staging,
without signature, CA or image-verification errors. If the selected release
differs from the agreed one, stop and investigate before rebooting.

When the console/API asks for a reboot, record `state.needs_reboot`, then
run `incus admin os system reboot proof:` and answer its confirmation.
After reconnection, repeat steps 4, 5 and 6. Require
`environment.os_version` to equal the new release and both applications'
`state.version` to equal their versions in its published manifest. Observe
`state.health` in the update API through the configured bake time (15 minutes
by default); require the new release to be marked good, without a failure
or an owner-intervention condition. Save the update log across both boots.

**PASS/FAIL 7:** PASS only if a distinct published release was applied, the
new OS and application versions run and pass their checks, and signed update
verification used the confirmed production update CA. If CA provenance or
verification cannot be established, record FAIL even if the version changed.

**Abort/recovery (destructive step):** before the manual check, restore saved
configuration to cancel. Once downloads/application writes begin, preserve
logs and let the operation finish when safe; disable automatic updates to
prevent another attempt. Use step 8 for an OS failure. OS rollback preserves
newer application versions to avoid database downgrade; never force older
applications onto their newer databases. The old disk is the final escape.

## 8. Go back to the previous release, then the old disk

1. With automatic updates off, cleanly reboot the proof machine and select
   the previous Arca OS release in its A/B boot menu. See [recovery](recovery.md).
   Record `environment.os_version`; it must equal the previous release.
   Repeat the trust and service checks. Application versions may remain
   newer, as documented in [automatic rollback](system/arca-updates.md).
2. If that slot cannot boot, the fallback is a reinstall of the previous
   release **on the new SSD only**, with its retained published installer.
   The first seed cannot do it: the installer refuses a target that has a
   partition table unless `force_install: true` is set, and when it
   recognises the media's version in the target's GPT partition labels it also
   demands `force_install_confirmation`: the first six lowercase characters of the
   target ESP's PARTUUID, printed by the installer without unlocking the disk.
   This value survives boots and updates and changes after a reinstall; the
   running system's machine ID is not used. This needs a second seed, approved
   by the owner for this fallback only: the same full by-id `target`,
   `force_install: true`, the printed `force_install_confirmation`, still no
   `confirmation`. With
   `force_install` the partition-table refusal protects no disk any more;
   the selector and the serial on the erase screen are the only guards, and
   the screen may be shown twice with a restart in between. Repeat steps 2
   and 3. The reinstall destroys data on the new SSD and attempts another TPM
   clear, so the owner first repeats the confirmation of the gate's item 5;
   the earlier recovery passphrase is void, so retrieve the new one
   right after first boot. Save needed evidence first. A reinstall
   demonstrates recovery, not a successful A/B rollback: record which route
   was used and the failed A/B result separately.
3. Shut down cleanly. Turn Secure Boot off without changing keys. From
   trusted recovery media, compare the old disk's size and complete digest
   against step 3's baseline before booting it. Then remove recovery media,
   select the old disk in the one-time boot menu and require its normal
   usable session. **Check that the old system unlocks after the TPM clear:**
   every encrypted volume from the hard gate must open, by itself or with its
   tested recovery key, and its data must be readable. Record which.

**PASS/FAIL 8:** PASS only if the previous release boots by the recorded
route, the old disk's offline digest is unchanged, the old disk boots, and
every volume it unlocked before the proof unlocks after the TPM clear. A
volume that cannot be opened is FAIL even with a matching digest. Do not
overwrite the old disk to recover the new installation.

**Abort/recovery (destructive fallback):** cancel any reinstall that names a
different target. If reinstall fails, remove media and boot the old disk.
If neither boots, stop and use the owner's recovery process; preserve both
disks and do not attempt repeated wipes, factory resets or TPM clears by hand.

## 9. NVIDIA owner condition (separate driver track)

This step depends on the driver track and real hardware. It is not proved by
a GPU-less VM. After step 8, shut down the old system cleanly, refresh its
offline baseline, turn Secure Boot back on with the same keys (do not
re-enrol or restore any), and boot the desired tested release on the new
SSD. Repeat trust and service checks. If the driver track is ready, this
step may instead run after step 8's first item and before the old-disk boot,
which saves one Secure Boot off-and-on round trip; record the order used.
Require a signed driver pack compatible with that running kernel before
installing it; see [NVIDIA](applications/nvidia.md).

- Install with `incus admin os application add proof:nvidia`. Inspect
  `incus query proof:/os/1.0/applications/nvidia` and reboot **the proof machine**
  if requested. **PASS/FAIL 9a:** PASS only if the pack is installed, its
  matching extension is active and `state.reason` reports no unresolved
  failure; otherwise FAIL.
- With `incus admin os application edit proof:nvidia`, set
  `config.pin_current_versions: true`. Read it back and compare
  `state.requested_branch`, `state.active_branch`, `state.version`, and
  `state.active_extension_version` against the driver's approved branch/build
  and kernel. **PASS/FAIL 9b:** PASS only if the pin is true and the intended
  branch/build is actually active; a downloaded but inactive pack fails.
- On the management client, set `CUDA_IMAGE` to the prebuilt, approved image
  reference and `GPU_SELECTOR` to the proof GPU selector supplied by the driver
  track. Ensure `proof-cuda` is a fresh disposable instance name, then run:

  ```sh
  incus init "$CUDA_IMAGE" proof:proof-cuda -c security.privileged=false -c nvidia.runtime=true
  incus config device add proof:proof-cuda proof-gpu gpu "id=$GPU_SELECTOR"
  incus start proof:proof-cuda
  incus config show proof:proof-cuda --expanded
  incus exec proof:proof-cuda -- nvidia-smi --query-gpu=name,driver_version --format=csv,noheader
  ```

  **PASS/FAIL 9c:** PASS only if `nvidia-smi` exits 0, identifies the expected
  card model and approved driver version. This query deliberately omits UUIDs
  and serial numbers. Preserve its redacted output.
- Run the driver track's exact CUDA workload command using
  `incus exec proof:proof-cuda --` followed by that command and arguments.
  Record the image digest, command, exit code and known-answer result.
  **PASS/FAIL 9d:** PASS only if expanded configuration confirms
  `security.privileged: "false"`, the workload actually executes CUDA kernels
  on the GPU, exits 0 and matches its expected result. `nvidia-smi` alone,
  a CPU fallback or a missing workload invocation fails this check.

**PASS/FAIL 9:** PASS only if 9a through 9d pass. A pending driver release,
missing approved workload/image, or unavailable production diagnostic access
is FAIL with the dependency recorded; do not weaken host trust to proceed.

**Abort/recovery (driver and container writes):** stop and remove only the
disposable `proof-cuda` instance after saving evidence. Do not delete any
existing workload. Follow the NVIDIA application's previous-pack recovery;
if needed set `config.driver: off` through its application edit command and
reboot the proof machine to use the fallback. Do not clear quarantine to
retry a failing pack. If console or boot fails, return to the old disk with
the digest and boot checks from step 8. No laptop reboot is involved.

## 10. Laptop boundary

The laptop takes no part in this run; updates come from the owner-approved
public update server. Its installation, disk layout, firmware and keys are
out of scope. Record that it was neither reinstalled nor restarted for the
proof. Only after the proof, and only with the owner's explicit word, may it
take one normal update.

A normal update can restart the laptop by itself. Before the update, read,
without editing, its update configuration with
`incus admin os system update show` against the laptop's own remote:

- With automatic updates on, which is the default (`auto_updates_off` absent
  or `false`), an installed OS update restarts the machine in its restart
  window, 03:00 to 05:00 local time unless `restart_window` says otherwise.
- With `auto_updates_off: true` and `auto_reboot: true`, it restarts right
  after applying the update.
- Only `auto_updates_off: true` with `auto_reboot: false` leaves the update
  waiting for a manual restart.

Proceed only in the third case, or when the owner has approved the restart
beforehand. Do not change the laptop's update configuration to arrange this
unless the owner says so. Application updates reload their applications at
once in every case.

**PASS/FAIL 10:** PASS if the laptop was not reinstalled, any end-of-run
update had the owner's explicit approval, and any restart was one the owner
approved beforehand. With no approval, leaving the laptop as it is passes.
Otherwise FAIL.

**Abort/recovery:** omit the optional laptop update. If the update server
becomes unavailable, pause the proof; the laptop is not a substitute for it.

## Evidence and checks still requiring a run

Keep a private run record containing the gate's volume list and recovery-key
test results (never the keys), the exported Secure Boot variables, the
fetched update indexes and provider settings, the serial-console or
display-console log, target confirmation and abort transcript, offline disk
comparisons, installed version before/after/rollback, selected trust fields,
Incus and Arca call results, update journal and bake result, NVIDIA
pin/active-build observations, `nvidia-smi` output and CUDA command/result.
Retain raw evidence privately and publish only reviewed redacted extracts
and PASS/FAIL lines.

This document alone measures no real machine. The gate and steps 1, 1b and 2
require physical SSD, firmware, TPM, disk-baseline and console checks; how a
given firmware behaves when Secure Boot is turned off and on again, and what
its reset procedure does, is known only from that machine. Steps 3 and 8
require actual boots, disk comparisons and recovery; they cannot be replaced
by a VM result.
Steps 4 through 7 have API/log checks that can be automated once production
artifacts, owner-approved CA provenance, client access and the proof machine
exist. Step 9 additionally requires the separate driver track, GPU and
known-answer CUDA workload. Step 10 and the pending owner decisions require
human confirmation. Report unavailable checks explicitly, without claiming a
hardware PASS from repository tests.

## Automated VM rehearsals only

**AUTOMATION-ONLY WARNING:** the installer requires typed `ERASE` on the
console and aborts after five minutes without it. A disposable VM harness
must either drive that console or put the documented automation-only override
in its `install.yaml` seed:

```yaml
confirmation: "ERASE"
```

This override bypasses the human confirmation. Never put it in the
real-machine seed; type `ERASE` there by hand. Require the installer release
to support this field before rehearsing: older installers without the
confirmation feature are not suitable. Rehearsals use disposable VM disks
only, never physical disk passthrough, and no image builds are part of this
proof plan. They cannot prove physical disk safety, the firmware's key
behaviour, the effect of the TPM clear on another system or the NVIDIA
workload.
