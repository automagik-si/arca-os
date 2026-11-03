# The always-on power profile

Arca OS runs a laptop as an always-on server. It never suspends or hibernates, a closed lid only turns the panel off,
the CPU and firmware run at maximum performance on mains and on battery, and the battery is a free UPS: on a power cut
the machine keeps running until the battery is nearly empty, then powers off cleanly so the disks and ZFS pools are
safe.

## What it does

| Behaviour | How |
|---|---|
| No suspend, hibernation or poweroff from the lid, the idle timer, or a suspend or hibernate key | `usr/lib/systemd/logind.conf.d/arca-power.conf` (every lid, key and idle action `ignore`), `usr/lib/systemd/sleep.conf.d/arca-power.conf` (`AllowSuspend=no`, `AllowHibernation=no`, `AllowSuspendThenHibernate=no`, `AllowHybridSleep=no`) |
| A short press of the power key | unchanged: logind's default, a clean poweroff |
| Lid closed: the internal panel's backlight off; lid open: back on at the brightness it had | `arca-power-watch.service` polls `/proc/acpi/button/lid/*/state` every 2 s and writes `brightness` and `bl_power` of each `/sys/class/backlight` device except a DDC/CI (external) one |
| The console never blanks on a timer | `consoleblank=0` on the kernel command line (`mkosi.conf`) |
| CPU governor `performance`, energy performance preference `performance`, energy perf bias `0`, turbo and boost on | `arca-power.service` at boot, before incus-osd, so a `scaling_governor` set in incus-osd's kernel configuration still wins |
| Firmware platform profile `performance` | at boot, and again every 10 s, as some firmware resets it when the power source changes |
| PCIe ASPM policy `performance` | at boot, through `/sys/module/pcie_aspm/parameters/policy` (the kernel command line is untouched) |
| No USB autosuspend for network adapters | `usr/lib/udev/rules.d/arca-power.rules`: the USB device behind every network interface gets `power/control=on` |
| Charge limit for an always-plugged battery | at boot and every 10 s: `charge_control_end_threshold=80` and `charge_control_start_threshold=75` on every battery that has them; `conservation_mode=1` on a Lenovo ideapad (its firmware picks the level, 60% or 80%) |
| Battery as a UPS | every 10 s: one log line on battery, one warning at 20%, and `systemctl poweroff` after two readings in a row (10 s apart) at or below 5% and discharging, or at the firmware's critical level |

Each knob is written only when the machine has it and offers the value; a missing knob is skipped with one log line,
and a step done before is not repeated. The thresholds are constants at the top of
`mkosi.images/base/mkosi.extra/usr/libexec/arca-power`. A battery's own threshold is the highest one: with two
batteries the machine powers off only when both are low. A mouse's or a pen's battery (`scope` `Device`) is ignored.

`systemctl poweroff` stops incus-osd with `SIGTERM`, which runs its normal shutdown sequence before the machine powers
off. incus-osd's suspend action (`incus admin os system suspend`) fails, since suspend is not allowed.

## Is it active?

```sh
incus admin os debug log -u arca-power.service        # the boot step: one line per knob kind
incus admin os debug log -u arca-power-watch.service  # lid, battery and charge-limit events
```

The boot step prints lines such as `cpufreq governor: set to performance (16)`, `platform profile: set to
performance (1)`, `BAT0: charge limit set, stops at 80%, restarts below 75%`, or `... not on this machine, skipped`.
The watch prints `lid closed: intel_backlight off`, `on battery at 97%, running on as a UPS`, a warning `battery low,
20% and discharging; poweroff at 5%` and, at the end, `battery critical, 5% and discharging: clean poweroff`.

## Testing

`scripts/arca/test/power.sh` runs the script under `dash` against fake `/sys` and `/proc` trees: present, missing,
not offered and refused knobs, a second run that changes nothing, the lid in each state, the battery states, and the
watch loop's poweroff after two critical readings.
