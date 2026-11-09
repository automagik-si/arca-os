# Automatic updates and rollback

With automatic updates on, updates download at any time, but an OS update only restarts the machine inside the restart window. Until then the update status says "Update ready; restarts in the window at 03:00". The owner can switch automatic updates on and off, move the restart window and try a failed version again from the machine's console (press `U`), or through the API.

Before restarting, IncusOS waits while the machine is busy: while Incus runs operations that a restart would break (migrations, exports, copies, snapshots), while the NVIDIA driver is being changed, or while the Arca application says it is busy. The Arca hook is optional: when `/run/arca/update.sock` exists, `GET /1.0/busy` on it is asked for `{"busy": true, "reason": "..."}`. A missing or failing hook never blocks a restart. A busy machine defers the restart for up to two hours past the end of the window, then the restart waits for the next window. The reason is written to the journal.

Before the restart, IncusOS records the running instances (ephemeral ones aren't brought back, by design), the machine's IPv4 addresses and whether the NVIDIA driver runs. The restart goes through the normal shutdown sequence, so Incus stops the instances the usual way, with their own stop timeout.

## Health bake

The first boot of a new OS version starts its health bake. Every minute IncusOS checks that:

* incus-osd is up;
* the Incus API answers;
* the network has the addresses it had before the restart;
* the Incus and Arca applications run;
* the NVIDIA driver still runs, if it ran before the restart (a node whose GPUs all go to passthrough isn't checked);
* the instances that ran before the restart run again (IncusOS starts any that didn't come back; one that was deleted isn't a failure).

When the checks pass at the end of the bake time, the version is marked good and its boot entry is blessed. The bake time counts in one boot: when the machine restarts during the bake, the bake starts again. IncusOS records every clean stop, so it can tell a restart from a crash:

* a boot that follows one that didn't stop cleanly (a crash, a hang ended by a watchdog or a reset, a power cut) is a failed check, and the second such boot goes back to the previous version at once;
* such a boot never blesses an entry; a version that passes its bake there is marked good, and its entry is blessed by the next boot that follows a clean stop;
* incus-osd must reach "ready" within ten minutes of starting; each ten minutes it doesn't is a failed check;
* after five restarts without passing, or when incus-osd keeps failing to start in one boot, the version fails too.

The new entry keeps systemd-boot's boot counter until it is blessed, but IncusOS doesn't rely on it: with UKI profiles, systemd-boot may not count boots at all.

If three checks fail, or no check passes within the bake time, the version is marked failed, its boot entry is marked bad and IncusOS restarts into the previous version. That restart happens at most once per boot. The disks must unlock in the previous version: without Secure Boot they are first bound to its UKI, and when it is signed by another Secure Boot key, the TPM is enrolled for that key. When that can't be done, IncusOS doesn't restart; it asks the owner. A fall back done by systemd-boot alone still asks for the recovery passphrase on a system without Secure Boot.

The Incus and Arca applications keep the versions that ran before the rollback rather than going back to older ones, as an older version may not read the newer database.

A failed version is never applied again automatically, but a newer version is. Every choice of the next boot skips failed versions, including a reboot or power-off through the API. The console and `POST /1.0/system/update/:retry` with `{"version": "<version>"}` release a failed version so it can be tried again. The record keeps the 20 newest failed versions.

If the version IncusOS went back to isn't healthy either, or a rollback can't be done, IncusOS stops trying. It shows "Needs you: <reason>" on the console and in the API, and switches automatic updates off. Switching them back on clears it.

## Application updates

After the Incus or Arca application is updated, IncusOS checks it for up to a minute. If it doesn't run, or the Incus API doesn't answer, the application goes back to the version it had before, without a reboot, and the failed version isn't applied again automatically, nor picked again at boot. The NVIDIA applications keep their own boot-time checks and recovery.

## Update state

A `PUT /1.0/system/update` that leaves out `auto_updates_off`, `restart_window` or `bake_time` keeps their current values, so an older client never switches automatic updates back on by accident.

`GET /1.0/system/update` returns the record under `state.health`: the current, previous, good and pending versions, the failed versions with their reasons, the next restart window, the last result and its reason, and any "Needs you" reason.
