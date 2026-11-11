---
orphan: true
---

# First real-machine proof

**Operating rule:** the proof machine's existing disk stays untouched and
bootable as the way back. The new SSD is the only install target. The laptop
that already runs Arca OS is the update server; never reboot or reinstall it
for this proof. At most, it receives one normal update at the very end, with
the owner's explicit word and without a reboot for this proof.

This is a written plan, not a record of a completed installation. Use already
published production installer and release artifacts. Do not build images as
part of this run. Stop at the first failed prerequisite; a missing measurement
is not a pass. Record `PASS <step>` or `FAIL <step>: <reason>` after each step
and each NVIDIA subcheck. Record an unrun check as `FAIL <step>: not run`.

## Before starting

The owner must settle these decisions and supply their values privately:

- **SSD arrival:** the new SSD has arrived, is installed, and may be erased.
- **Root-key holder:** who holds the production root keys, who can confirm the
  production PK and update CA, and how they approve the published artifacts.
  Signing keys stay with their holder, never on the proof machine.
- **Bucket:** which public update bucket and server/channel are authoritative,
  and which two distinct published releases will be used. Call them the
  previous release and the new release. Retain both installers and updates
  until recovery has been proved; select `stable` for the proof machine.
  Also confirm how the previous release's applications are supplied during
  first boot, before the machine switches to the new release's update view.
- **Recovery:** the old system's firmware settings and recovery material are
  available, including any disk-unlock recovery material affected by a Secure
  Boot policy change. The owner can restore its original boot policy without
  clearing the TPM. If that cannot be assured, do not start.
- **Driver track:** the separate NVIDIA work has delivered a signed pack for
  the chosen OS kernel, an approved pinned branch/build, and a prebuilt CUDA
  test container with a known-answer workload and its exact invocation.

Bring a keyboard, display or working serial console, wired network, the
published installer for the previous release, and separate recovery media.
Prepare an authenticated management client with the matching Incus OS
commands and the client's trusted certificate in the install preseed; see
[access](../getting-started/access.md) and [Incus](applications/incus.md).
Keep credentials and recovery material outside console recordings.

Prepare the install seed with both `incus` and `arca` applications and an
`update.yaml` containing these top-level fields (seed files have no `config`
wrapper):

```yaml
channel: stable
check_frequency: never
auto_updates_off: true
auto_reboot: false
```

Confirm that the previous release's application images are already included,
or use an already published signed bootstrap view selecting that release.
An absent primary application forces an initial update check even with these
settings; an older installer alone does not guarantee an older application
baseline. Stop if first boot would select the new release. Do not change a
shared channel or rebuild images to arrange this proof.

The examples use `proof:` as a local Incus remote alias for the proof machine,
not as a host name. Add that remote using the access guide, check its server
certificate against the console, and authenticate with the preseeded client.
Run management commands from the management client, never against the
laptop's remote. For raw API calls, `incus query` returns the response
metadata, so the fields below do not have an extra `metadata` wrapper.

Keep raw evidence privately on separate storage. Public results contain only
redacted observations: no host names, addresses, serial numbers, account or
personal names, credentials, keys, or recovery passphrases. Do not commit raw
disk inventories, logs, seed files, or screenshots containing those values.

## 1. Preconditions

1. Boot the old disk normally and confirm its usual system reaches a usable
   session. Shut it down cleanly. Record the original boot order and Secure
   Boot policy privately. Confirm the old disk and the new SSD are both
   physically present and distinguishable by model, byte capacity and contents.
2. Before booting the installer, use separate trusted recovery media with
   automount and swap activation disabled. Do not mount or unlock the old
   disk. Inventory disks with `lsblk -b -o PATH,MODEL,SIZE,TYPE,FSTYPE,MOUNTPOINTS`.
   Privately resolve the old disk's whole-device path into `OLD_DISK` and the
   new SSD's into `NEW_SSD`. Confirm neither has mounted children or active
   swap (`findmnt` and `swapon --show`); unmounting an already written disk
   does not count as an untouched baseline. Start again from a clean shutdown.
3. Mark the old disk read-only with `sudo blockdev --setro "$OLD_DISK"` and
   require `sudo blockdev --getro "$OLD_DISK"` to return `1`. Save
   `sudo blockdev --getsize64 "$OLD_DISK"` and a whole-device digest from
   `sudo sha256sum "$OLD_DISK"` privately on separate storage. Also save the
   new SSD's size and complete digest with the same read commands using
   `"$NEW_SSD"`, for the no-write rejection test. Wait for a
   successful exit; a partial read is not evidence. Read-only flags do not
   persist across boots: every later recovery-media session must repeat this
   protection and identify disks afresh. Never assume device paths stay fixed.
4. In firmware, verify UEFI boot, Secure Boot **Setup Mode**, and no custom
   keys enrolled yet. Check TPM 2.0 is enabled and healthy. On the trusted
   recovery system, `sudo tpm2_getcap properties-fixed` must succeed and
   identify TPM family 2.0. Do not clear or reset the TPM to make this pass.
5. Compare hardware with the [public requirements](../getting-started/requirements.md):
   supported modern architecture (including `x86_64_v3` for x86), UEFI Secure
   Boot, hardware TPM 2.0, at least 4 GiB system RAM, at least 50 GiB on the
   new SSD, and a wired network port. Confirm the cable is connected and the
   switch reports a link. Record the installer release and approved provenance.

**PASS/FAIL 1:** PASS only when every prerequisite is checked, the old disk
has booted, its offline inventory and full digest are saved, and the new SSD
alone is approved for erasure. Otherwise FAIL and stop before the installer.

**Abort/recovery:** remove recovery media and use the firmware's one-time
boot menu to boot the old disk. Restore its saved Secure Boot policy if
needed; use its owner-held recovery material if requested. A broken way back
blocks this proof even when the new SSD could be installed.

## 2. Installer confirmation and rejection test

Use a published installer that implements the console `ERASE` confirmation
and the five-minute abort timeout. This is a release prerequisite: if the
prompt or target details are absent, stop. Do not use an unattended install
seed on the proof machine. Restrict the seed's `target` selector to the new
SSD using the [seed reference](seed.md); verify the selector cannot also match
the old disk. Do not use a broad size-sort rule or a force-wipe override.

1. Boot the public installer through the proof machine's one-time boot menu.
   Setup Mode key enrollment may restart the proof machine before the
   installer starts. Capture the console, excluding secrets.
2. Read the proposed target's model, size and existing contents/partition
   summary. Compare each with the inventory. Only the new SSD may be offered
   as an install target or default; an inventory listing of the old disk is
   not permission to install to it. Ambiguity or an old-disk target is a FAIL.
3. Deliberately type an answer other than `ERASE`. Require an explicit abort
   before disk writes. Do not retry by typing `ERASE` in the same attempt.
4. Remove installer media, boot the trusted recovery media with automount
   disabled, and repeat the read-only size and full-digest checks on the old
   disk. Compare with step 1. Also compare the new SSD's complete digest, size
   and partition summary with its baseline; both disks must be unchanged.
   Before booting the old system, require the old disk's digest to match.
   Then boot the old disk to confirm the way back still works.

**PASS/FAIL 2:** PASS only if the confirmation identified only the new SSD,
the wrong answer aborted without writes, and the old disk's offline digest
and boot test passed. Otherwise FAIL; do not proceed to a real erasure.

**Abort/recovery:** cancel or power off while still awaiting confirmation,
remove install media, and boot the old disk using its saved firmware policy.
If any old-disk difference appears, preserve evidence and stop; do not
attempt a repair or reinstall on that disk.

## 3. Install on the new SSD

After the rejection test's old-system boot, shut down cleanly and repeat
step 1's offline baseline: normal operation of the old system can change its
digest. Return firmware to Setup Mode if necessary, then boot the installer.
Recheck the target's model, capacity and contents from scratch. Type `ERASE`
**by hand on the proof machine's console**, only for the new SSD. Wait for
installation to finish. Remove install and seed media when asked, then allow
the proof machine to boot its installed system without that media.

Record the installed version from the console. Before any later boot of the
old disk, shut down the proof machine cleanly and use recovery media to
compare the old disk's complete offline digest and size with the refreshed
baseline. Return to the new SSD and establish the `proof:` management remote.

**PASS/FAIL 3:** PASS only if installation completed, the new SSD boots
without installer media, and the old disk still matches the baseline.
Otherwise FAIL and stop.

**Abort/recovery (destructive step):** before `ERASE`, cancel without writes.
After confirmation, an interrupted installation may leave the new SSD
unbootable; do not assume its previous contents are recoverable. If safe,
allow writes to finish, shut down, remove media and boot the old disk. Retry
only on the new SSD after repeating target identification and confirmation.

## 4. Trust

Read the current boot journal privately with
`incus query 'proof:/os/1.0/debug/log?unit=incus-osd&boot=0'`.
The startup entry must report `mode=production`; `mode=dev`, `unsafe`, or a
missing startup entry fails. Read `incus query proof:/os/1.0/system/security`
privately: that endpoint also returns recovery secrets. Inspect only these
fields for the public result:

- `state.secure_boot_enabled` is `true`.
- The certificate with `type: PK` in `state.secure_boot_certificates` matches
  the Arca OS production PK approved by the root-key holder; a matching name
  alone is insufficient. Compare the fingerprint privately.
- `state.tpm_status` is `ok`, and encrypted root and swap volumes report
  `unlocked (TPM)` in `state.encrypted_volumes`.
- `state.system_state_is_trusted` is `true`; review
  `state.system_state_status` for a contrary condition.

Retrieve the [encryption recovery material](system/security.md) into the
owner's private store, outside recordings, and only then run
`incus admin os system security retrieved proof:`. Never publish the full
security response. Do not install a debug application or unlock a shell to
make a check work: that can invalidate the trusted state being measured.

**PASS/FAIL 4:** PASS only if mode is production, Secure Boot uses the
approved Arca OS PK, hardware TPM unlocking works, and security is trusted.
Otherwise FAIL; do not accept a degraded state for this proof.

**Abort/recovery:** save a redacted reason, stop updates, and return to the
old disk. Do not clear the TPM, replace keys, or rebind it speculatively.

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
restart loop over at least 60 seconds. Confirm the local console renders,
accepts navigation and can return to its normal status screen.

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

Save the existing provider and update configuration privately. Using
`incus admin os system provider edit proof:`, select provider `images` and
set `config.config.server_url` to the owner-approved public update-server
base URL. Keep signed-metadata validation enabled. Use
`incus admin os system update edit proof:` to set `config.channel: stable`,
`config.auto_updates_off: true` and `config.auto_reboot: false` while staging
the controlled test. Preserve other fields. Do not leave the channel empty.

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

Before checking, have the root-key holder confirm that the installed release
embeds the production update CA. The provider must have no `update_ca`
override, unless its certificate has independently been confirmed as that
same production CA. HTTP/TLS reachability alone is not update verification.
The normal images provider verifies signed metadata using that CA; see
`incus-osd/internal/providers/provider_images.go` and its `VerifySMIME` path.
Do not substitute unsigned JSON or a development provider.

Run `incus admin os system update check proof:`. This is a check **and apply**
operation, including immediate application updates; it is not a dry run.
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
2. If that slot cannot boot, use the retained published installer for the
   previous release and repeat steps 2 and 3 **on the new SSD only**. This
   fallback destroys data on the new SSD; save needed evidence first. A
   reinstall demonstrates recovery, not a successful A/B rollback: record
   which route was used and the failed A/B result separately.
3. Shut down cleanly. From trusted recovery media, compare the old disk's
   size and complete digest against step 3's baseline before booting it.
   Then remove recovery media, select the old disk in firmware, restore its
   saved boot policy if necessary, and require its normal usable session.

**PASS/FAIL 8:** PASS only if the previous release boots by the recorded
route, the old disk's offline digest is unchanged, and the old disk boots.
Otherwise FAIL; do not overwrite the old disk to recover the new installation.

**Abort/recovery (destructive fallback):** cancel any reinstall that names a
different target. If reinstall fails, remove media and boot the old disk.
If neither boots, stop and use the owner's recovery process; preserve both
disks and do not attempt repeated wipes, factory resets or TPM clears.

## 9. NVIDIA owner condition (separate driver track)

This step depends on the driver track and real hardware. It is not proved by
a GPU-less VM. After step 8, shut down the old system cleanly, refresh its
offline baseline, restore the proof machine's Arca OS boot policy, and boot
the desired tested release on the new SSD. Repeat trust and service checks.
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

The laptop remains an update server throughout this run. Its installation,
disk layout, firmware, keys and reboot behavior are out of scope. Record that
it remained available and was neither reinstalled nor rebooted for the proof.
Only after the proof, and only with the owner's explicit word, may it take
one normal update. If that update requires a reboot, leave the reboot for
separately authorized work outside this proof.

**PASS/FAIL 10:** PASS if no laptop reinstall or reboot occurred, and any
end-of-run normal update had the owner's explicit approval. With no approval,
leaving the laptop as it is passes. Otherwise FAIL.

**Abort/recovery:** omit the optional laptop update. If server availability
is lost, pause the proof rather than restarting or reinstalling the laptop.

## Evidence and checks still requiring a run

Keep a private run record containing the serial-console or display-console
log, target confirmation and abort transcript, offline disk comparisons,
installed version before/after/rollback, selected trust fields, Incus and
Arca call results, update journal and bake result, NVIDIA pin/active-build
observations, `nvidia-smi` output and CUDA command/result. Retain raw evidence
privately and publish only reviewed redacted extracts and PASS/FAIL lines.

This document alone measures no real machine. Steps 1 and 2 require physical
SSD, firmware, disk-baseline and console checks. Steps 3 and 8 require actual
boots, disk comparisons and recovery; they cannot be replaced by a VM result.
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
proof plan. They cannot prove physical disk safety or the NVIDIA workload.
