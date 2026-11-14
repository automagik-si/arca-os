# System security
IncusOS actively relies on both UEFI Secure Boot and TPM for its boot security and encryption.

## Assumptions

- Prior to installing IncusOS, Secure Boot on the server will be placed into Setup mode.
   - If a specific PK or third-party db certificates are required, configuration of the PK/KEK/db certificates must be performed prior to starting the IncusOS install.
   - If no PK/KEK/db certificates are present, IncusOS will auto-enroll the Secure Boot certificates as part of the install.
- Post-install Secure Boot will run in User mode. This will allow IncusOS to automatically apply db and dbx updates that have been signed by an IncusOS KEK certificate. If the IncusOS PK key is enrolled, we could also update the KEK keys, although this is expected to be an incredibly rare operation.
   - Alternatively, if Secure Boot is in Deployed mode, db/dbx/KEK key updates will have to be applied out-of-band by IT staff. This will likely be an unsupported configuration.
- A new IncusOS Secure Boot Key will be created for each year and published well in advance.
   - Keys will be valid for 18-24 months (TDB) to allow time for rollover. Secure Boot doesn't actually check expiry of certificates, but there is a simple check in `incus-osd` prior to applying an OS update.
   - The first release of IncusOS after January 1st will pickup and use the new year's signing certificate. Some time after that has happened, the prior year's certificate can be invalidated and an update placing that certificate into dbx can be published via API.

## Certificate hierarchy
IncusOS relies on a hierarchy of TLS certificate authorities and certificates as shown below. Note that Secure Boot doesn't perform TLS-style validation of the certificates.

- The IncusOS Root and Secure Boot CAs use latest standards (ECDSA, etc).
- IncusOS PK, KEK, and db certificates are limited to RSA 2048 due to the Secure Boot standard. Each Secure Boot certificate is signed by the IncusOS Secure Boot CA.

```
    Incus OS - Root E1
          |
    Incus OS - Secure Boot E1
          |
          \-- Incus OS - Secure Boot PK R1
           \-- Incus OS - Secure Boot KEK R1
            |-- Incus OS - Secure Boot 2025 R1
            \-- Incus OS - Secure Boot 2026 R1
```

## Use of Secure Boot variables

- PK: OEM/owner-provided or Incus OS - Secure Boot PK R1
- KEK: Incus OS - Secure Boot KEK R1
- db: IncusOS signing key(s)
   - A new signing key will be generated each year and published in advance
   - 6-12 month overlap after January 1st when new signing key is placed into service
- dbx: Old IncusOS signing key(s)
   - After sufficient time for key rotation, old IncusOS signing keys will be revoked
- MOK: (not used/supported in IncusOS setup)

### Implementation notes

- Updates to db and dbx can be signed offline with an enrolled KEK or PK key, then distributed and automatically applied on any system
   - Only replacing existing values or appending are supported
   - Removal of a specific value requires local access to the KEK or PK private key
- Updates to KEK can be signed offline with an enrolled PK key, and then distributed like db and dbx updates
   - If IncusOS PK isn't present, KEK updates will have to be applied out-of-band
   - KEK updates are expected to be extremely rare
- IncusOS will receive db/dbx/KEK updates via the configured Provider in a similar manner to OS and application updates
- An update to IncusOS signed with a key not yet in the db or present in dbx will not be allowed to install, since on reboot the system would refuse to boot the new image
- Updating a trusted signing key and applying an IncusOS update will be two distinct operations, with a reboot in between. This is because updating Secure Boot state and expected TPM PCR values is critical to get right, and it's much simpler logic to only allow one change (a new trusted key or an update with a changed signing key) at a time.

## Secure Boot key updates
KEK, db, and dbx updates are signed offline and then made available via a Provider's API:

- Published as a simple list of `.auth` files, with one update per file
- Filename pattern of `<VAR>_<SHA256 fingerprint>.auth`: `db_8A78635EA12B2EF676045B661187E08D1412253220A1BD02EF79D177302DB83F.auth`, `dbx_DA39EF49E3F5D7B902ECE6CA338883623F61DC671ABE10DF2E7B1CBDEC4A2B47.auth`, etc
   - This naming convention makes it trivial for a client to quickly retrieve a list of all available keys, identify any missing ones, and then download needed updates
- Update check performed on same cadence as OS update checks (every six hours by default):
   - Apply each UEFI variable update one at a time, starting with KEK, then db, then dbx
   - Will need to reboot after each update; will automatically reboot if update applied during IncusOS startup, otherwise will require a user triggering a reboot
   - Will not apply a dbx update if the current or backup image are signed by it to prevent bricking

### Update availability and integrity

- An attacker could block IncusOS update checks to prevent application of Secure Boot key updates
- Each `.auth` file is signed by a KEK certificate already enrolled on the machine IncusOS is running on. If the file is tampered with, enrollment will fail, so there is no special need to protect or checksum received updates.

## Update server trust

IncusOS only installs updates whose signed index chains to the root CA and update CA embedded in the running image (or the provider's `update_ca`). If the update server signs with a CA this system doesn't trust, the update check reports a `trust` failure with the SHA-256 fingerprints of the expected and presented CAs (see [update check failures](system/update.md#update-check-failures)).

The system won't fix this by itself. Compare both fingerprints with the ones the update server publishes:

- If the presented CA is the legitimate newer CA, the system needs a release that migrates it to that CA, or a reinstall from a current image.
- If it isn't, the server or the path to it isn't the expected one; don't override `update_ca` to make the check pass.

## Use of TPM PCRs
IncusOS relies on five PCRs (4, 7, 11, 12 & 15) to bind disk encryption keys.

### PCR 4

```{note}
PCR 4 is only used on IncusOS systems where Secure Boot is disabled.
```

PCR 4 is computed based on the Portable Executable (PE) binaries involved in the boot process. For IncusOS, this includes the `systemd-boot` stub and the system UKI image. The UEFI computes the final value of this PCR by hashing the authenticode of each PE binary. The corresponding TPM event log entry contains additional information about the PE binary, such as its UEFI path, size, etc.

When IncusOS is configured to run with Secure Boot disabled, we cannot rely on PCR 7 containing useful data. Even if it is populated, we cannot trust its contents. Therefore, we add PCR 4 to the list of values that the disk encryption is bound to. This provides some measure of assurance that the disk is only automatically unlocked if we boot the expected IncusOS install.

Combined with IncusOS performing an early boot verification of the TPM PCR 4 event log and binary signatures, if IncusOS can automatically unlock the encrypted disks when starting we can be fairly confident that IncusOS hasn't been tampered with even when Secure Boot is disabled.

### PCR 7
PCR 7 is computed based on the current Secure Boot state and PK/KEK/db/dbx values. The final value of this PCR is calculated by UEFI before the `systemd-boot` UEFI stub starts. Binding to this PCR allows us to ensure data is only available when Secure Boot is enabled and IncusOS certificates are present. (Prevents an attacker from unlocking the disk on a different machine or launching attacks via live boot media.)

The calculation of PCR 7 is straightforward, and performed whenever a signing key is added or revoked, and when an IncusOS update is signed with a different key than the current running system:

- Fetch TPM event log and verify that the recomputed PCR 7 value matches the current TPM PCR 7 value
- Apply UEFI variable update
- Replay TPM event log, computing the future PCR 7 value using current UEFI variable values
- Use current TPM state to update PCR 7 binding of LUKS volumes using predicted PCR 7 value on next boot

### PCR 11
PCR 11 is computed based on the running UKI, and computed at various points during the boot process. Combined with a properly signed UKI image, this allows us to detect any tampering of the UKI and refuse to unlock the encrypted disks. Computation of PCR 11 is complex; systemd has `systemd-measure` which we rely on to create the PCR 11 policy at image build time which is then combined with the Secure Boot signing key to bind the TPM. The advantage is that this approach is much more flexible than an exact hash binding like we do with PCRs 4 & 7, and allows the build process to fully predict PCR 11 values and embed those values into the resulting signed UKI images.

IncusOS only ever needs to worry about re-binding PCR 11 when the Secure Boot key used by an UKI is changed, such as the yearly key transition. This is because the PCR 11 policies are bound to the TPM using the current Secure Boot signing key, and if it changes on reboot the TPM state won't match and auto-unlock will fail. The steps taken when installing an IncusOS update with a different Secure Boot key are:

- Verify the key of the updated UKI is present in the UEFI db variable, and isn't in dbx. This prevents installing an update which will immediately fail to boot with a Secure Boot policy violation.
- Replace the existing `systemd-boot` UEFI stub with a newly signed one from the pending OS update. `systemd-sysupdate` doesn't typically update the `systemd-boot` stub, but we need to ensure it's updated to a version signed by the new key.
- Changing the signature on the `systemd-boot` stub will affect the PCR 7 value at next boot, so follow the steps outlined above to predict the new PCR 7 value.
- Re-bind the TPM PCR 11 policies with the new signing certificate and predicted PCR 7 value. Doing this invalidates the current TPM state, so we must rely on a recovery key known to IncusOS to update the LUKS header. The update is performed in as an atomic process as possible, to prevent having the LUKS header in a state where it doesn't have a TPM enrolled.

### PCR 12

```{note}
PCR 12 is only used on IncusOS systems where Secure Boot is disabled.
```

PCR 12 is populated by `systemd-boot` and `systemd-stub` with the kernel command line passed by the boot loader, extra kernel command line arguments passed through SMBIOS, the selected UKI profile and any add-on files, credentials or extension images picked up from the ESP next to the UKI. System credentials passed through SMBIOS are measured into PCR 1 and don't affect PCR 12. On a normal IncusOS boot, nothing is passed this way and PCR 12 remains at its initial value (all zeros).

When Secure Boot is enabled, `systemd-stub` ignores any command line provided by the boot loader because the UKI already contains one. When Secure Boot is disabled, that protection doesn't apply and a modified boot entry on the (unencrypted) ESP could boot the genuine UKI with an altered kernel command line without affecting PCRs 4, 7 or 11. Binding PCR 12 to its expected value closes this gap: a command line that differs from the one built into the UKI causes an additional PCR 12 measurement and the TPM refuses to unlock the encrypted disks.

PCR 12 is expected to keep the same value from one boot to the next. The expected value is read from the TPM whenever the PCR 4 binding is updated, and existing installs are automatically migrated to also bind PCR 12 on their first boot of an IncusOS version supporting it. Passing extra kernel command line arguments to a virtual machine through SMBIOS will change PCR 12 and require entering the recovery passphrase on the next boot. The same happens when booting a different UKI profile, as `systemd-stub` measures the selected profile into PCR 12. In both cases, once the passphrase is entered, IncusOS [re-seals the TPM](#re-seal-after-a-recovery-unlock) to the new value, so the following boots unlock without it.

### PCR 15
PCR 15 is populated by systemd at various points during the boot process with system identity events, such as when the LUKS root volume is unlocked, the machine ID, and basic information about the root (`/`) file system. IncusOS binds its encryption to an uninitialized PCR 15 (all zeros), which allows for automatic decryption by the TPM in the initrd only. As part of opening the LUKS root volume in the initrd, systemd extends PCR 15 which changes its value. This prevents the TPM from unlocking the LUKS volume again once the system has booted.

IncusOS depends on PCR 15 to thwart the attack described in a [blog post](https://oddlama.org/blog/bypassing-disk-encryption-with-tpm2-unlock/) by oddlama. In the future, improvements in the `mkosi` and `ukify` build tools may allow for more targeted PCR 11 policies to be generated which would only be valid while in the initrd. This would remove the need to depend on PCR 15.

### Implications
Any unexpected change to PCR values will cause auto-unlock to fail, and require the entry of a recovery password to boot the system. Such a change is a **strong** indication that something unexpected occurred to the system's security configuration outside of what IncusOS expected. It could be triggered by an innocuous change, or could point to malicious activity. In either case, be careful before blindly entering a recovery password at boot.

One other thing to be aware of involves the annual rotation of the Secure Boot key that is used to sign the IncusOS images. IncusOS will automatically handle the transition, but if for some reason you need to boot into the recovery image that was signed by the prior year's Secure Boot key you will need to use a recovery password to boot. This is because the `systemd-boot` stub will be signed with the new Secure Boot key, but the recovery UKI is signed by the old one. This will cause a mismatch between expected and actual PCR values. After a second update signed by the new Secure Boot key is installed, the issue goes away because both UKI images will be signed with the same one that was used to sign the `systemd-boot` stub.

## Recovery passphrase
On first boot, IncusOS generates the recovery passphrase of the main system drive. It is the credential the owner types at the console when the TPM refuses to unlock the drive, so it is made to be read and typed: eight lowercase English words separated by single spaces, for example `abandon ability able about above absent absorb abstract` (never use this one).

### Strength
The words come from the 2048-word [BIP39 English list](https://github.com/trezor/python-mnemonic/blob/master/src/mnemonic/wordlist/english.txt) (MIT License, Copyright (c) 2013-2016 Pavol Rusnak), embedded in `incus-osd`. Each word is drawn independently from the system's random source and carries 11 bits, so a passphrase carries 88 bits. No two words of the list share their first four letters, which makes a typing mistake easy to spot.

The passphrase is enrolled as a traditional LUKS password slot on the root and swap volumes. Each volume also has its own passphrase, which IncusOS keeps in `/var/lib/incus-os/keys/` to update the TPM bindings; it is never shown.

### Hand-over at first boot
After generating the passphrase, the console shows it once, with what it is, when it will be asked for, that Arca keeps a copy in its encrypted settings, and that losing it means losing the drive. The owner then confirms it by typing its second and fifth words. A wrong answer shows the passphrase again and doesn't count; only a correct answer marks the recovery keys as retrieved. The system keeps starting while the page waits, and the page also closes once the keys are marked retrieved through the API. If IncusOS restarts before the hand-over is done, the page shows again.

The console is also drawn on the serial port, so serial console captures and BMC logs keep a copy of the passphrase.

Until then, the API and the CLI report the recovery keys as not retrieved. `incus admin os system security show` prints the passphrase in two groups of four words, for reading it over.

For unattended installs, setting `security.recovery_passphrase_acknowledged` to `true` in the [install seed](seed.md) pre-acknowledges the hand-over: the installer records it on the ESP, and first boot carries it into the system state. The passphrase is then shown for one minute without asking for any words, and the recovery keys stay marked as not retrieved until they are retrieved through the API.

### Unlocking at the console
When the TPM refuses to unlock the drive, the boot stops at a passphrase prompt. Type the eight words in lowercase, separated by single spaces, and press Enter. A typo is rejected and the prompt asks again. After IncusOS starts up, it re-seals the TPM, as described below, so the next boot unlocks automatically.

Systems installed before this change keep their existing recovery key, with its dashes, until it is replaced through the API.

### Re-seal after a recovery unlock
When the root volume was unlocked with a recovery passphrase instead of the TPM (typed at the console, or read from [`RESCUE_DATA`](recovery.md#unlocking-with-a-recovery-key-on-rescue_data)), IncusOS re-seals the TPM to the state the system just booted with, early during startup and once per boot:

* With Secure Boot enabled, the TPM key slot of each encrypted volume is enrolled again for the current PCR 7.
* With Secure Boot disabled, the PCR 4 binding is updated for the running image (or, when running from the backup image, for the newest image, which boots next), together with the current PCR 7 and the expected PCR 12.

If the same PCR values were already enrolled, as after the TPM was cleared or replaced, the key slot is still enrolled anew so the new TPM can unseal it. No re-seal happens when the TPM unlocked the drive, nor when a Secure Boot variable update is pending (the volume then reads `unlocked (TPM; PCR update pending)` and the existing binding already matches the next boot).

The result is kept in the security state and reported in plain words:

* The console's alert line reads `✓ Unlocked with the recovery passphrase; the TPM is re-sealed to this system`, or, when the screen is wide enough, `The disk was unlocked with the recovery passphrase; the TPM has been re-sealed to this system's current state`.
* `GET /1.0/system/security` reports `last_recovery_unlock` (when, RFC 3339), `reseal_status` (`re-sealed` or `reseal failed`) and `reseal_reason` (one short line, only on failure). The fields describe the most recent recovery unlock and stay until the next one. `incus admin os system security show` prints the same sentence after the state on a boot that used the passphrase.

A failed re-seal never stops the system: it keeps running and the failure is not retried until the next boot. The console shows `✗ TPM not re-sealed (passphrase asked again): <reason>`, and the API and CLI say `the TPM could not be re-sealed: <reason>; the passphrase will be asked again next boot`. The journal has the full error. Once the cause is fixed, either boot again and type the passphrase, which tries the re-seal again, or [reset the TPM bindings](system/security.md#resetting-tpm-bindings) by hand, which also reboots the system.

```{warning}
The re-seal trusts the state the passphrase unlocked. As noted under [Implications](#implications), a refused unlock can point to a change made by someone else: if the cause isn't known, find it before typing the passphrase, since typing it makes that state the one the TPM unlocks from then on.
```

### Why the TPM refused
Each time IncusOS seals the drive to the TPM (first boot, an OS update or Secure Boot key change, a db/dbx update, a [re-seal after a recovery unlock](#re-seal-after-a-recovery-unlock) and a TPM binding reset), it writes a record of what it sealed to on the ESP, `/boot/arca-tpm-seal.json`:

- `version`: the record's format, currently `1`
- `uki_version`: the OS version the drive was sealed to
- `secure_boot_enabled` and `setup_mode`: the Secure Boot state
- `pcr4`, `pcr7` and `pcr12`: the PCR values, as lowercase SHA-256 hex
- `sealed_at`: when, in RFC 3339

The record holds no secret, only public measurements. Failing to write it never fails the seal. Whenever the TPM unlocked the drive, IncusOS also rewrites a record that is missing, unreadable or no longer matches the measurements, so a drive sealed before the record existed gets one on its next boot. It does so at most once per boot, and never after a seal on that boot, whose record holds the next boot's values. The record must hold 64-digit hex PCR values and a numeric version, or it is ignored as unreadable.

Before the passphrase prompt, the initrd reads the record from the ESP (where the initrd mounted it, or by mounting it read-only by its partition label when it isn't), compares the record with the boot's measurements, and, when they differ, explains the cause in a few plain lines on the screen and the serial console, with the way back:

| What changed | What the screen says | Way back |
|---|---|---|
| Secure Boot turned on or off, or Setup Mode entered or left | Secure Boot changed, sealed with one state, now another | Set the firmware back to the sealed state and boot again |
| PCR 7 with the same Secure Boot state | Secure Boot keys changed | Restore the firmware's Secure Boot keys and boot again |
| PCR 4, only compared with Secure Boot off | A different OS image was started | Pick the sealed version in the boot menu and boot again |
| PCR 12, only compared with Secure Boot off | Boot parameters changed | Drop the extra command line, credentials or add-ons and boot again |
| No record, on a drive sealed before it existed | One line: if asked, type the recovery passphrase or the older recovery key | Type the passphrase or key |

A drive that `systemd-repart` created on this boot holds only its TPM keyslot and has no record yet; its first boot prints nothing.

Each message also offers the other way: type the recovery passphrase (eight words) or, on a drive installed before it, the older recovery key (with dashes) at the prompt below. That [re-seals the TPM](#re-seal-after-a-recovery-unlock) to the changed state, so the next boot unlocks without a prompt: the screen says to type it only if the owner made the change. The changed values are shown shortened, and no line is longer than 79 characters. When the record matches, nothing is printed. The check only explains: the boot, the prompt and its retries are unchanged.

A cleared or replaced TPM doesn't change the PCR values, so such a boot looks exactly like a healthy one before the unlock is tried, and the screen says nothing. The sealed key is then gone from the TPM: type the recovery passphrase at the prompt, which re-seals the drive to the new TPM.

On a virtual machine, Secure Boot changes can't be tried with the development images, as the firmware refuses their test-signed boot loader once Secure Boot is turned on.

### Useful tools
systemd has `systemd-pcrlock` which is useful to inspect current PCR values and how they were computed during the boot process.

## Useful links

- [UEFI Specification - Creating trust relationships](https://uefi.org/specs/UEFI/2.11/32_Secure_Boot_and_Driver_Signing.html#firmware-os-key-exchange-creating-trust-relationships)
- [UEFI Specification - Signature database update](https://uefi.org/specs/UEFI/2.11/32_Secure_Boot_and_Driver_Signing.html#signature-database-update)
- [Microsoft - Updating Microsoft Secure Boot keys](https://techcommunity.microsoft.com/blog/windows-itpro-blog/updating-microsoft-secure-boot-keys/4055324)
