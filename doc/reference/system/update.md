# Update

IncusOS will check the configured [provider](providers.md) for stable updates at each boot and then by default every six hours thereafter, unless `check_frequency` is set to `never`. When an update is available, IncusOS will download it, update/restart any applications, and stage the OS update for next boot. Occasionally a Secure Boot key update may also be published, which will be automatically applied before any other available updates.

Updates to the `stable` channel are normally published once a week to pick up the latest stable bug fix release of the Linux kernel as well as any relevant security issues, while the `testing` channel may see more frequent updates as new features are developed. It is generally recommended to remain on the `stable` channel.
In addition, a `daily` channel is also available which features completely automated and untested daily builds, those should only be used by developers.

When an OS update is installed, IncusOS will display a message on the console that a reboot is required to finish applying the update. It will also report this via the REST API update state when queried.

Application updates are applied immediately while OS updates only take effect on reboot. Systems are expected to be updated and rebooted regularly, as applications may stop working if the running OS is more than three months older than the applications.

## Configuration options

Configuration fields are defined in the [`SystemUpdateConfig` struct](https://github.com/lxc/incus-os/blob/main/incus-osd/api/system_update.go).

The following configuration options can be set:

* `auto_reboot`: If `true`, IncusOS will automatically restart itself after applying an update. Note that this will cause some period of service interruption for any applications running on that server while it reboots. (IncusOS will always automatically reboot if it applies an update on system boot.)

* `channel`: `stable`, `testing`, `daily`, ...

* `check_frequency`: A string that is parsable as a duration by Go's `time.ParseDuration()` or the special value `never`. Controls the frequency that IncusOS will use when checking for updates. Setting to `never` disables any automatic updates; this is typically discouraged as the system will be dependent on manual update checks to receive any security updates.

* `maintenance_windows`: An optional list of maintenance windows.

* `auto_updates_off`: If `true`, no automatic check, install or restart happens. A manual update check still works. Defaults to `false` (automatic updates on).

* `restart_window`: When an installed OS update may restart the machine, in the same format as a maintenance window. Defaults to 03:00 to 05:00 local time.

* `bake_time`: How long a new OS version must stay healthy before it's marked good, as a Go duration of at least one minute. Defaults to `15m`.

## Maintenance windows

IncusOS supports defining maintenance windows that limit when the system will check for and apply updates. This can be useful to prevent updates from being installed during normal business hours or other inconvenient times. Each maintenance window consists of a start time and an end time (assumed to be in the system's configured timezone) and an optional start day of week and end day of week.

### Examples

Allow updates daily each night between 10pm - 6am:

```yaml
config:
  maintenance_windows:
  - start_hour: 22
    start_minute: 0
    end_hour: 6
    end_minute: 0
```

Allow updates only on the weekend:

```yaml
config:
  maintenance_windows:
  - start_day_of_week: "Saturday"
    start_hour: 0
    start_minute: 0
    end_day_of_week: "Sunday"
    end_hour: 23
    end_minute: 59
```

## Automatic updates and rollback

See [automatic updates and rollback](arca-updates.md): restarts in the window, the health bake, rollback and the failed-version list.

## Available version

After each OS update check, `GET /1.0/system/update` reports what the channel offers under `state`:

* `available_version`: the newest OS version in the channel, from the last valid answer of the update server. It's empty when no check has seen one, and a check that finds no OS image in the channel clears it.
* `update_available`: whether that check found the version newer than the running one and not yet applied. A version that failed before, or one older than the running version, isn't available. Installing the update clears it, and so does starting up on the available version.

A check that fails before the update server gives a valid answer (`network`, `parse`, `signature` or `trust`) changes neither field, so they can be older than `last_check`. A check that fails after a valid answer, for example while downloading or installing the update, has already recorded what the server offered. Secure Boot and application checks don't change them.

The console's updates page shows the version next to `Available`, with "Waiting for a restart" when the version is installed but not running yet, "Behind the channel" when `update_available` is set, "Up to date" when the system runs it, and "No update to apply" otherwise. When the last check failed, it adds "(the last check failed)". `incus admin os system update show` prints the version and the same words after the YAML.

## Update check failures

When an update check fails, `GET /1.0/system/update` reports why under `state`:

* `last_error_kind`: why the last check failed. It's empty after a successful check.
   * `network`: the server can't be reached or doesn't return the file.
   * `parse`: the index or a file is malformed, or the certificates in the signed index are damaged.
   * `signature`: the signature doesn't match the signed content.
   * `trust`: the index is signed under a CA other than the one this system expects, or that CA doesn't chain to the root CA this system trusts.
   * `other`: anything else, including a signing certificate that has expired or isn't valid yet (check the system clock), or a check that was cancelled.
* `last_error`: a one-line summary of the error, without URL query strings.
* `embedded_ca_fingerprint` and `presented_ca_fingerprint`: for a `trust` failure, the SHA-256 fingerprints of the update CA this system expects and of the CA the server's signature was issued by.
* `trust_failures`: consecutive checks that failed with `trust`. Network and parse failures don't reset it; a successful check does, and clears the fingerprints.
* `trust_escalated`: set once `trust_failures` reaches three.

A `trust` failure sets the status to "Update server key is not trusted by this system". The console's updates page shows both fingerprints, and after three such checks the alert line says the system hasn't received updates. `incus admin os system update show` prints the same details.

The system keeps running and keeps checking on its normal schedule, but it won't install anything from that server until the CAs match. See [update server trust](../security.md#update-server-trust) for what to do.

## Manually checking for an update

You can instruct IncusOS to check for an update at any time by running

```
incus admin os system update check
```
