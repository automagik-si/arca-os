# Automatic updates and rollback

With automatic updates on, updates download at any time, but an OS update only restarts the machine inside the restart window. Until then the update status says "Update ready; restarts in the window at 03:00". The owner can switch automatic updates on and off, move the restart window and try a failed version again from the machine's console (press `U`), or through the API.

Before restarting, IncusOS waits while the machine is busy: while Incus runs operations that a restart would break (migrations, exports, copies, snapshots), while the NVIDIA driver is being changed, or while the Arca application says it is busy. The Arca hook is optional: when `/run/arca/update.sock` exists, `GET /1.0/busy` on it is asked for `{"busy": true, "reason": "..."}`. A missing or failing hook never blocks a restart. A busy machine defers the restart for up to two hours past the end of the window, then the restart waits for the next window. The reason is written to the journal.

Before the restart, IncusOS records the running instances and the machine's IPv4 addresses. The restart goes through the normal shutdown sequence, so Incus stops the instances the usual way, with their own stop timeout.

## Health bake

The first boot of a new OS version starts its health bake. Every minute IncusOS checks that:

* incus-osd is up;
* the Incus API answers;
* the network has the addresses it had before the restart;
* the Incus and Arca applications run, and the NVIDIA driver didn't fall back;
* the instances that ran before the restart run again (IncusOS starts any that didn't come back).

When the checks pass at the end of the bake time, the version is marked good and its boot entry is blessed. Until then, systemd-boot's boot counting stays on for the new entry: a version that fails to start three times in a row falls back to the previous one, and IncusOS marks it failed.

If three checks fail, or no check passes within the bake time, the version is marked failed, its boot entry is marked bad and IncusOS restarts into the previous version. That restart happens at most once per boot. Without Secure Boot, the disks are first bound to the previous version's UKI so it can unlock them; a fall back done by systemd-boot alone still asks for the recovery passphrase on such a system.

A failed version is never applied again automatically, but a newer version is. The console and `POST /1.0/system/update/:retry` with `{"version": "<version>"}` release a failed version so it can be tried again.

If the version IncusOS went back to isn't healthy either, or a rollback can't be done, IncusOS stops trying. It shows "Needs you: <reason>" on the console and in the API, and switches automatic updates off. Switching them back on clears it.

## Application updates

After the Incus or Arca application is updated, IncusOS checks it for up to a minute. If it doesn't run, or the Incus API doesn't answer, the application goes back to the version it had before, without a reboot, and the failed version isn't applied again automatically. The NVIDIA applications keep their own boot-time checks and recovery.

## Update state

`GET /1.0/system/update` returns the record under `state.health`: the current, previous, good and pending versions, the failed versions with their reasons, the next restart window, the last result and its reason, and any "Needs you" reason.
