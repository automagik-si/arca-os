# Storage

IncusOS allows for the configuration of complex ZFS storage pools. Each pool is automatically encrypted with a randomly generated key to protect data stored in the pool. The encryption keys can be retrieved from the system's security state.

When creating a storage pool, IncusOS can use local devices, or remote devices made available via a [service](../services.md), such as iSCSI.

To maintain data integrity, IncusOS automatically performs a weekly scrub of all storage pools in the system. The scrub schedule defaults to Sunday at 04:00, but can be configured by the user. It is also possible to manually trigger a scrub of any given pool.

To keep SSD write performance and endurance from degrading, IncusOS also performs a weekly trim (discard of unused blocks) of all storage pools. The trim schedule defaults to Saturday at 04:00, but can be configured by the user. It is also possible to manually trigger a trim of any given pool. Pools whose devices don't support trim, such as spinning disks, report an `UNSUPPORTED` trim state and are skipped.

The scheduled scrub and trim pause while the disks are busy, so they don't slow down the workloads on the same disks. IncusOS reads the kernel's IO pressure (`/proc/pressure/io`) every 10 seconds and pauses the scrub or trim it started once `full` avg10 has stayed at or above 10% for a minute, or as soon as `some` avg60 reaches 40%. It resumes once `full` avg10 has stayed below 2% and `some` avg60 below 10% for five minutes. A pause doesn't end the run: it carries on once resumed. The `last_scrub` and `last_trim` status report `paused`, `paused_since`, `pause_reason` and the number of `pauses` during the run. A scrub or trim started outside the scheduled run is never paused, and on systems without IO pressure information nothing is paused. A paused scrub stays paused on disk across restarts, so IncusOS resumes any paused scrub when it starts, and a scheduled or manual scrub resumes a paused one instead of reporting it as already running.

It is also possible to add, remove, and replace devices from an existing storage pool. This is accomplished by getting the current pool configuration, making the necessary changes in the relevant struct, then submitting the results back to IncusOS.

Devices that have failed, whether they have been physically removed or are still attached but faulted, are reported in the `devices_degraded`, `cache_degraded`, `log_degraded` and `special_degraded` fields rather than in the regular device lists. To replace such a device, add a new device to the corresponding list and IncusOS will use it to replace the degraded device instead of expanding the pool.

## Configuration options

Configuration fields are defined in the [`SystemStorageConfig` struct](https://github.com/lxc/incus-os/blob/main/incus-osd/api/system_storage.go).

The following system-wide configuration options can be set:

* `pools`: An array of zero or more user-defined storage pool definitions.
* `scrub_schedule`: A cron expression with five fields defining when to perform an automatic scrub of all the storage pools. Defaults to `0 4 * * 0`.
* `trim_schedule`: A cron expression with five fields defining when to perform an automatic trim of all the storage pools. Defaults to `0 4 * * 6`.

User-defined storage pools are defined in the [`SystemStoragePool` struct](https://github.com/lxc/incus-os/blob/main/incus-osd/api/system_storage.go), which has the following options:

* `name`: The name of the storage pool.
* `type`: The type of the storage pool, which must be one of `zfs-raid0`, `zfs-raid1`, `zfs-raid10`, `zfs-raidz1`, `zfs-raidz2`, or `zfs-raidz3`.
* `allow_mixed_dev_sizes`: If true, allow creation of a storage pool with devices of different sizes. Note that in most cases this will result in a storage pool whose total available capacity will be constrained by the smallest device size.
* `alignment`: Optional, if specified sets the default sector alignment size for the pool; must be a power of two.
* `devices`, `cache`, `log,` and `special`: Used to assign various storage devices to the pool in different roles.

```{warning}
When specifying devices for a pool, order is important. IncusOS will always return a sorted list which it will use when comparing the list of devices it receives via the API to determine what device(s) to add, remove, or replace in the pool. Put another way, `"devices": ["/dev/sda", "/dev/sdb"]` != `"devices": ["/dev/sdb", "/dev/sda"]`.
```

```{warning}
When creating a storage pool with a custom sector alignment value, be aware of the potential for significant performance degradation due to write amplification if the alignment value is smaller than that of the underlying physical device(s). For example, setting an alignment of 512 bytes for a storage pool on a drive with 4KiB sectors will result in write amplification of approximately 8x compared to using an alignment of 4096 bytes.

IncusOS defaults to an alignment value of 4096 bytes, which works well for drives with either 512 byte or 4KiB sector sizes.
```

### Examples

Create a storage pool `mypool` as ZFS raidz1 with four devices, one cache device, and one log device, setting the automatic scrub to run every Saturday at 00:00 and the automatic trim every Friday at 00:00:

```yaml
config:
  pools:
  - name: "mypool"
    type: "zfs-raidz1"

    devices:
    - "/dev/sdb"
    - "/dev/sdc"
    - "/dev/sdd"
    - "/dev/sde"

    cache:
    - "/dev/sdf"

    log:
    - "/dev/sdg"
  scrub_schedule: "0 0 * * 6"
  trim_schedule: "0 0 * * 5"
```

Replace failed device `/dev/sdb` in the above example with `/dev/sdh`:

```yaml
config:
  pools:
  - name: "mypool"
    type: "zfs-raidz1"

    devices:
    - "/dev/sdh"
    - "/dev/sdc"
    - "/dev/sdd"
    - "/dev/sde"

    cache:
    - "/dev/sdf"

    log:
    - "/dev/sdg"
  scrub_schedule: "0 0 * * 6"
```

Create a storage pool `mypool` as ZFS raidz1 with three spinning devices, and a special vdev backed by three NVME devices also in a raidz1 configuration:

```yaml
config:
  pools:
  - name: "mypool"
    type: "zfs-raidz1"

    devices:
    - "/dev/sdb"
    - "/dev/sdc"
    - "/dev/sdd"

    special:
      type: "zfs-raidz1"
      special_small_blocks_size_in_kb: 128
      devices:
      - "/dev/nvme0n1"
      - "/dev/nvme1n1"
      - "/dev/nvme2n1"
  scrub_schedule: "0 4 * * 0"
```

Get the pool encryption keys for safe storage (base64 encoded):

```
$ incus admin os system security show
[snip]
state:
  pool_recovery_keys:
    local: vIAKUWSxK5GrNrkn60kjEXh2M4WZdtX+hcyhx0W8q7U=
    mypool: zh9gkAgGsKenO48y7dwNg6aBFaD6OoedgSlSsivEq0Q=
```

## Converting a single device pool to a mirrored pool

It is possible to convert a singe device storage pool to a mirrored pool with two storage devices. The new device to be used for mirroring the data must be at least as large as the original device.

Given an existing storage pool:

```yaml
config:
  pools:
  - name: "mypool"
    type: "zfs-raid0"

    devices:
    - "/dev/sdb"
  scrub_schedule: "0 4 * * 0"
```

IncusOS can convert this to a mirrored storage pool using new device `/dev/sdc` with the following configuration. Note the change of pool type from `zfs-raid0` to `zfs-raid1`.

```yaml
config:
  pools:
  - name: "mypool"
    type: "zfs-raid1"

    devices:
    - "/dev/sdb"
    - "/dev/sdc"
  scrub_schedule: "0 4 * * 0"
```

IncusOS does not support other forms of in-place storage pool conversions.

## Deleting a storage pool

```{warning}
Deleting a storage pool will result in the unrecoverable loss of all data in that pool.
```

Delete the storage pool `mypool` by running

```
incus admin os system storage delete-pool -d '{"name":"mypool"}'
```

## Scrubbing a storage pool

Start a scrub for the storage pool `mypool` by running

```
incus admin os system storage scrub-pool -d '{"name":"mypool"}'
```

## Trimming a storage pool

Start a trim for the storage pool `mypool` by running

```
incus admin os system storage trim-pool -d '{"name":"mypool"}'
```

## Wiping a drive

```{warning}
Wiping a drive will result in the unrecoverable loss of all data on that drive.
```

Wipe drive `scsi-0QEMU_QEMU_HARDDISK_incus_disk`, which must be specified by its ID, by running

```
./incus admin os system storage wipe-drive -d '{"id":"/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_incus_disk"}'
```

By default, wiping a drive will attempt to use `blkdiscard` to quickly wipe a drive. However, for drives that do not support this operation, or to explicitly zero the entire device, you can include the `"secure_wipe":true` parameter when issuing the wipe command. Be aware that for large, spinning disks this may take a significant amount of time to complete.

## Detecting new SCSI devices

Newly mapped SCSI devices are not always detected automatically. Ask every SCSI host to scan for new devices and refresh the existing ones by running

```
incus admin os system storage rescan-scsi
```

## Importing an existing pool

When importing an existing encrypted storage pool, IncusOS needs to be informed of its encryption key before the data can be made available. Because there is no way to prompt for an encryption passphrase, only ZFS pools using a raw encryption key can be imported. Specify the raw base64 encoded encryption key when importing storage pool `mypool` by running

```
incus admin os system storage import-storage-pool -d '{"name":"mypool","type":"zfs","encryption_key":"THp6YZ33zwAEXiCWU71/l7tY8uWouKB5TSr/uKXCj2A="}'
```

If the storage pool was previously used on another system or was not cleanly exported, you can add `"force":true` to force the import:

```
incus admin os system storage import-storage-pool -d '{"name":"mypool","type":"zfs","encryption_key":"THp6YZ33zwAEXiCWU71/l7tY8uWouKB5TSr/uKXCj2A=","force":true}'
```

```{warning}
IncusOS can import an unencrypted ZFS storage pool, but this is **strongly** discouraged. Use of unencrypted storage risks accidental leakage of sensitive data from an encrypted pool to an unencrypted one.

An unencrypted storage pool should only be imported to facilitate migration of data into an encrypted storage pool.

IncusOS will not manage an unencrypted storage pool, and the unencrypted storage pool must be manually imported after each boot via the storage API.

To import an unencrypted ZFS storage pool, simply omit the `encryption_key` parameter from the API call.
```

## Managing volumes

It's possible to create and delete volumes within a storage pool.

Each volume has its own:

* Name
* Quota (in bytes, a zero value means unrestricted)
* Use (`incus` or `linstor`)

The list of volumes are visible directly in the storage configuration data.

Creating and deleting volumes can be done through the command line with:

```
incus admin os system storage create-volume -d '{"pool":"local","name":"my-volume","use":"linstor"}'
incus admin os system storage delete-volume -d '{"pool":"local","name":"my-volume"}'
```

```{note}
IncusOS automatically creates a new `incus` volume when setting up the `local` storage pool.
```

## Raw drive encryption

IncusOS supports encrypting disks that aren't part of one of its pools.
This may be useful when providing a container or virtual-machine with full block devices.

IncusOS will generate a random key, use it to encrypt the drive using LUKS and then automatically unlocking the drive on boot.
Existing encrypted drives can also be added to the system by providing IncusOS with their encryption key.

Wipe and encrypt drive `scsi-0QEMU_QEMU_HARDDISK_incus_disk` by running:

```
./incus admin os system storage encrypt-drive -d '{"id":"/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_incus_disk"}'
```

Import and decrypt an existing drive by running:

```
./incus admin os system storage import-encrypted-drive -d '{"id":"/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_incus_disk", "key": "BASE64-OF-KEYFILE"}'
```

Encrypted drives that are managed by IncusOS get two additional properties in `incus admin os system storage show`:

* Encrypted (indicates whether the drive is encrypted)
* Encrypted ID (provides the path to the decrypted block device)

## Importing an old drive

A drive that holds data from another system (LUKS, LVM, btrfs, ext4, xfs or zfs, in any stack) can be opened read-only,
its content listed and copied into a pool, and then, only on request, turned into a pool itself. Drives are named by
their serial number, as shown by `incus admin os system storage show`.

Opening a drive refuses the boot drive, pool members, remote drives, encrypted drives managed by IncusOS, protected
serials, and any drive that is mounted, held by another device or given to an instance. Before anything is read, the
drive and its partitions are set read-only in the kernel; LUKS is opened `--readonly`, LVM is activated read-only with
read-only metadata, file systems are mounted read-only without journal replay, and zfs pools are imported with
`readonly=on`. The kernel cannot run an LVM thin pool read-only, so thin volumes (and logical volumes that are not
active) are listed as unreadable while the rest of the drive opens. The passphrase of a LUKS layer (or of an encrypted zfs dataset) is given on the command's standard input
only, and is never stored or logged.

```
incus admin os system storage import-open -d '{"serial":"S0EXAMPLE","passphrase":"..."}'
incus admin os system storage import-show
```

The listing shows each file system with the Incus instances and custom volumes of an old host found on it (up to six
levels down, for instance under `var/lib/incus/storage-pools/<pool>`), and its top-level folders. Copy one of them
into a pool; the copy runs in the background and its progress is shown by `import-show`:

```
incus admin os system storage import-copy -d '{"serial":"S0EXAMPLE","filesystem":"vg0/root","kind":"instance","source":"@/var/lib/incus/storage-pools/default/containers/web","target_pool":"local"}'
incus admin os system storage import-copy -d '{"serial":"S0EXAMPLE","filesystem":"vg0/root","kind":"folder","source":"@home/owner","target_pool":"local","target_name":"owner-home"}'
```

`kind` is `instance`, `volume` or `folder`; a folder or volume becomes a custom volume. A copied instance arrives
stopped with `boot.autostart` set to false (the old value is kept in `user.import.autostart`), without its snapshots.
`profiles` replaces the instance's profiles; networks and profiles it uses must exist. Incus stores the whole copy in
its backups volume before unpacking it into the pool, so a copy is refused unless both have room for it, counting the
copies already running; when both are on the same zpool (as with the `local` pool), that zpool needs room for it twice.

Close the drive when done; this undoes every step and makes the drive writable again:

```
incus admin os system storage import-close -d '{"serial":"S0EXAMPLE"}'
```

```{warning}
Converting a drive erases it.
```

A closed import drive can be turned into an encrypted single-drive pool, handed to Incus as a storage pool of the same
name. The serial must be repeated in `confirm`:

```
incus admin os system storage import-convert -d '{"serial":"S0EXAMPLE","pool":"data","confirm":"S0EXAMPLE"}'
```

## Protected drives

`protected_serials` in the storage configuration lists drives that no wipe, encrypt, import or convert may touch. A
configuration update without the field keeps the list; an empty list clears it.
