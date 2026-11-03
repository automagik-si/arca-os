#!/bin/sh
# The fixture test of mkosi.images/base/mkosi.extra/usr/libexec/arca-power.
#
#   scripts/arca/test/power.sh
#
# It runs the script under dash against fake /sys and /proc trees: apply on a
# full tree sets every knob and a second apply changes nothing, apply on an
# empty tree skips every knob and succeeds, a value the knob does not offer is
# left alone, a knob that refuses the write is logged and the rest still set;
# the lid turns the panel backlight off and back on (an external DDC/CI one is
# left alone); the battery states follow the thresholds and the power source;
# and the watch loop powers off only after three critical readings in a row,
# never with a Mains supply online or on a 0% glitch. Prints PASS/FAIL lines;
# exits 1 when any case fails.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
script=$here/../../../mkosi.images/base/mkosi.extra/usr/libexec/arca-power
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failed=0
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1: $2"; failed=1; }
# has <pattern>: $out has a line matching it; count <pattern>: how many.
has() { printf '%s\n' "$out" | grep -q -- "$1"; }
count() { printf '%s\n' "$out" | grep -c -- "$1" || true; }

# put <root> <path> <value>: writes one fake sysfs file.
put() {
	mkdir -p "$(dirname "$1/$2")"
	printf '%s\n' "$3" > "$1/$2"
}
# val <root> <path>: one fake file's value.
val() { cat "$1/$2"; }
# run <root> <args...>: the script against a root, output in $out.
run() {
	root=$1
	shift
	out=$(ARCA_POWER_ROOT=$root ARCA_POWER_RUN=$root/run PATH=$tmp/bin:$PATH dash "$script" "$@" 2>&1)
}
# expect <case> <root> <path> <value>
expect() {
	got=$(val "$2" "$3" 2>/dev/null || echo '<missing>')
	if [ "$got" = "$4" ]; then
		return 0
	fi
	fail "$1" "$3 is $got, want $4"
	return 1
}

# A stand-in sleep, so the watch loop runs at once, and a stand-in poweroff.
mkdir -p "$tmp/bin"
printf '#!/bin/sh\nexit 0\n' > "$tmp/bin/sleep"
printf '#!/bin/sh\necho poweroff >> "%s/poweroffs"\n' "$tmp" > "$tmp/bin/fake-poweroff"
chmod 0755 "$tmp/bin/sleep" "$tmp/bin/fake-poweroff"

# full <root>: a laptop with every knob.
full() {
	r=$1
	for p in 0 1; do
		put "$r" "sys/devices/system/cpu/cpufreq/policy$p/scaling_governor" powersave
		put "$r" "sys/devices/system/cpu/cpufreq/policy$p/scaling_available_governors" "performance powersave"
		put "$r" "sys/devices/system/cpu/cpufreq/policy$p/energy_performance_preference" balance_power
		put "$r" "sys/devices/system/cpu/cpufreq/policy$p/energy_performance_available_preferences" "default performance balance_performance balance_power power"
		put "$r" "sys/devices/system/cpu/cpufreq/policy$p/boost" 0
		put "$r" "sys/devices/system/cpu/cpu$p/power/energy_perf_bias" 6
	done
	put "$r" sys/devices/system/cpu/intel_pstate/no_turbo 1
	put "$r" sys/devices/system/cpu/cpufreq/boost 0
	put "$r" sys/firmware/acpi/platform_profile balanced
	put "$r" sys/firmware/acpi/platform_profile_choices "low-power balanced performance"
	put "$r" sys/module/pcie_aspm/parameters/policy "[default] performance powersave powersupersave"
	put "$r" sys/class/power_supply/BAT0/type Battery
	put "$r" sys/class/power_supply/BAT0/charge_control_start_threshold 96
	put "$r" sys/class/power_supply/BAT0/charge_control_end_threshold 100
	put "$r" sys/class/power_supply/hid-mouse/type Battery
	put "$r" sys/class/power_supply/hid-mouse/scope Device
	put "$r" sys/class/power_supply/hid-mouse/charge_control_end_threshold 100
	put "$r" sys/class/power_supply/AC/type Mains
	put "$r" sys/bus/platform/drivers/ideapad_acpi/VPC2004:00/conservation_mode 0
}

# Apply on a full tree, then again.
r=$tmp/full
full "$r"
run "$r" apply && rc=0 || rc=$?
ok=1
[ "$rc" -eq 0 ] || { fail apply-full "exit $rc: $out"; ok=0; }
for f in policy0/scaling_governor policy1/scaling_governor policy0/energy_performance_preference; do
	expect apply-full "$r" "sys/devices/system/cpu/cpufreq/$f" performance || ok=0
done
expect apply-full "$r" sys/devices/system/cpu/cpu1/power/energy_perf_bias 0 || ok=0
expect apply-full "$r" sys/devices/system/cpu/intel_pstate/no_turbo 0 || ok=0
expect apply-full "$r" sys/devices/system/cpu/cpufreq/boost 1 || ok=0
expect apply-full "$r" sys/devices/system/cpu/cpufreq/policy1/boost 1 || ok=0
expect apply-full "$r" sys/firmware/acpi/platform_profile performance || ok=0
expect apply-full "$r" sys/module/pcie_aspm/parameters/policy performance || ok=0
expect apply-full "$r" sys/class/power_supply/BAT0/charge_control_end_threshold 80 || ok=0
expect apply-full "$r" sys/class/power_supply/BAT0/charge_control_start_threshold 75 || ok=0
expect apply-full "$r" sys/class/power_supply/hid-mouse/charge_control_end_threshold 100 || ok=0
expect apply-full "$r" sys/bus/platform/drivers/ideapad_acpi/VPC2004:00/conservation_mode 1 || ok=0
has 'cpufreq governor: set to performance (2)' || { fail apply-full "no governor line: $out"; ok=0; }
has 'BAT0: charge limit set, stops at 80%, restarts below 75%' || { fail apply-full "no charge line: $out"; ok=0; }
[ "$ok" = 1 ] && pass apply-full
run "$r" apply && rc=0 || rc=$?
if [ "$rc" -eq 0 ] && ! has 'set' && has 'cpufreq governor: already performance'; then
	pass apply-idempotent
else
	fail apply-idempotent "exit $rc: $out"
fi

# Apply on an empty tree: every knob skipped, once.
r=$tmp/empty
mkdir -p "$r"
run "$r" apply && rc=0 || rc=$?
n=$(count 'not on this machine, skipped' || true)
if [ "$rc" -eq 0 ] && [ "$n" -eq 8 ] && ! has '<4>'; then
	pass apply-empty
else
	fail apply-empty "exit $rc, $n skipped: $out"
fi

# A governor the CPU does not offer, an EPP knob without its list, and a
# charge end below the start: the end goes first.
r=$tmp/partial
put "$r" sys/devices/system/cpu/cpufreq/policy0/scaling_governor schedutil
put "$r" sys/devices/system/cpu/cpufreq/policy0/scaling_available_governors "schedutil powersave"
put "$r" sys/devices/system/cpu/cpufreq/policy0/energy_performance_preference balance_power
put "$r" sys/class/power_supply/BAT1/type Battery
put "$r" sys/class/power_supply/BAT1/charge_control_start_threshold 50
put "$r" sys/class/power_supply/BAT1/charge_control_end_threshold 60
run "$r" apply && rc=0 || rc=$?
ok=1
[ "$rc" -eq 0 ] || { fail apply-not-offered "exit $rc"; ok=0; }
expect apply-not-offered "$r" sys/devices/system/cpu/cpufreq/policy0/scaling_governor schedutil || ok=0
expect apply-not-offered "$r" sys/devices/system/cpu/cpufreq/policy0/energy_performance_preference balance_power || ok=0
expect apply-not-offered "$r" sys/class/power_supply/BAT1/charge_control_end_threshold 80 || ok=0
expect apply-not-offered "$r" sys/class/power_supply/BAT1/charge_control_start_threshold 75 || ok=0
has 'cpufreq governor: not on this machine, skipped' || { fail apply-not-offered "$out"; ok=0; }
[ "$ok" = 1 ] && pass apply-not-offered

# A knob that refuses the write (a read-only file; root writes it anyway).
if [ "$(id -u)" -ne 0 ]; then
	r=$tmp/refused
	full "$r"
	chmod 0444 "$r/sys/firmware/acpi/platform_profile"
	run "$r" apply && rc=0 || rc=$?
	ok=1
	[ "$rc" -eq 0 ] || { fail apply-refused "exit $rc"; ok=0; }
	expect apply-refused "$r" sys/firmware/acpi/platform_profile balanced || ok=0
	expect apply-refused "$r" sys/devices/system/cpu/cpufreq/policy0/scaling_governor performance || ok=0
	has '<4>arca-power: platform profile: 1 not set to performance' || { fail apply-refused "$out"; ok=0; }
	[ "$ok" = 1 ] && pass apply-refused
fi

# The lid.
r=$tmp/lid
put "$r" proc/acpi/button/lid/LID0/state "state:      open"
put "$r" sys/class/backlight/intel_backlight/brightness 4000
put "$r" sys/class/backlight/intel_backlight/max_brightness 9000
put "$r" sys/class/backlight/intel_backlight/bl_power 0
put "$r" sys/class/backlight/ddcci0/brightness 50
put "$r" sys/class/backlight/ddcci0/bl_power 0
run "$r" lid
if [ -z "$out" ] && [ "$(val "$r" sys/class/backlight/intel_backlight/brightness)" = 4000 ]; then
	pass lid-open-noop
else
	fail lid-open-noop "$out"
fi
put "$r" proc/acpi/button/lid/LID0/state "state:      closed"
run "$r" lid
ok=1
expect lid-closed "$r" sys/class/backlight/intel_backlight/brightness 0 || ok=0
expect lid-closed "$r" sys/class/backlight/intel_backlight/bl_power 4 || ok=0
expect lid-closed "$r" sys/class/backlight/ddcci0/brightness 50 || ok=0
expect lid-closed "$r" sys/class/backlight/ddcci0/bl_power 0 || ok=0
has 'lid closed: intel_backlight off' || { fail lid-closed "$out"; ok=0; }
run "$r" lid
[ -z "$out" ] || { fail lid-closed "second step logged: $out"; ok=0; }
expect lid-closed "$r" run/intel_backlight.brightness 4000 || ok=0
[ "$ok" = 1 ] && pass lid-closed
put "$r" proc/acpi/button/lid/LID0/state "state:      open"
run "$r" lid
ok=1
expect lid-reopened "$r" sys/class/backlight/intel_backlight/brightness 4000 || ok=0
expect lid-reopened "$r" sys/class/backlight/intel_backlight/bl_power 0 || ok=0
has 'lid open: intel_backlight on' || { fail lid-reopened "$out"; ok=0; }
[ "$ok" = 1 ] && pass lid-reopened
put "$r" sys/class/backlight/intel_backlight/brightness 0
run "$r" lid
expect lid-open-dark "$r" sys/class/backlight/intel_backlight/brightness 9000 && pass lid-open-dark
rm -rf "$r/proc"
put "$r" sys/class/backlight/intel_backlight/brightness 4000
run "$r" lid
expect lid-unknown "$r" sys/class/backlight/intel_backlight/brightness 4000 && pass lid-unknown

# bat <root> <name> <status> <capacity> [<level>]: one fake battery.
bat() {
	put "$1" "sys/class/power_supply/$2/type" Battery
	put "$1" "sys/class/power_supply/$2/status" "$3"
	put "$1" "sys/class/power_supply/$2/capacity" "$4"
	put "$1" "sys/class/power_supply/$2/capacity_level" "${5:-Normal}"
}
# state <case> <want>: the battery state of $r.
state() {
	run "$r" battery
	if [ "$out" = "$2" ]; then
		pass "$1"
	else
		fail "$1" "got '$out', want '$2'"
	fi
}
r=$tmp/b-none
mkdir -p "$r"
state battery-none none
r=$tmp/b-mains
bat "$r" BAT0 Charging 3
state battery-mains "mains 3"
r=$tmp/b-laptop
bat "$r" BAT0 "Not charging" 91
put "$r" sys/class/power_supply/ADP1/type Mains
put "$r" sys/class/power_supply/ADP1/online 1
state battery-not-charging-on-ac "mains 91"
put "$r" sys/class/power_supply/ADP1/online 0
state battery-not-charging-unplugged "battery 91"
bat "$r" BAT0 "Not charging" 4
state battery-not-charging-empty "critical 4"
r=$tmp/b-ac-online
bat "$r" BAT0 Discharging 4
put "$r" sys/class/power_supply/AC/type Mains
put "$r" sys/class/power_supply/AC/online 1
state battery-ac-online "mains 4"
r=$tmp/b-ups
bat "$r" BAT0 Discharging 50
state battery-ups "battery 50"
r=$tmp/b-low
bat "$r" BAT0 Discharging 20
state battery-low "low 20"
r=$tmp/b-crit
bat "$r" BAT0 Discharging 5
state battery-critical "critical 5"
r=$tmp/b-alarm
bat "$r" BAT0 Discharging 9 Critical
state battery-firmware-critical "critical 9"
r=$tmp/b-two
bat "$r" BAT0 Discharging 3
bat "$r" BAT1 Unknown 60
state battery-second-full "battery 60"
bat "$r" BAT1 Discharging 4
state battery-both-empty "critical 4"
r=$tmp/b-energy
put "$r" sys/class/power_supply/BAT0/type Battery
put "$r" sys/class/power_supply/BAT0/status Discharging
put "$r" sys/class/power_supply/BAT0/energy_now 2000
put "$r" sys/class/power_supply/BAT0/energy_full 50000
put "$r" sys/class/power_supply/hid-pen/type Battery
put "$r" sys/class/power_supply/hid-pen/scope Device
put "$r" sys/class/power_supply/hid-pen/status Discharging
put "$r" sys/class/power_supply/hid-pen/capacity 90
state battery-energy-device "critical 4"

# wrun <iterations>: the watch loop on $r, output in $out.
wrun() {
	rm -f "$tmp/poweroffs"
	out=$(ARCA_POWER_ROOT=$r ARCA_POWER_RUN=$r/run ARCA_POWER_POWEROFF=fake-poweroff ARCA_POWER_ITERATIONS=$1 \
		PATH=$tmp/bin:$PATH dash "$script" watch 2>&1)
}
# poweroffs: how many times the loop powered off.
poweroffs() { [ -e "$tmp/poweroffs" ] && wc -l < "$tmp/poweroffs" | tr -d ' ' || echo 0; }

# The watch loop: the battery is read every fifth lid poll, so ten polls give
# two critical readings and eleven give three.
r=$tmp/w
bat "$r" BAT0 Discharging 4
wrun 10
if [ "$(poweroffs)" = 0 ] && has '<3>arca-power: battery critical, 4% and discharging'; then
	pass watch-two-critical
else
	fail watch-two-critical "$(poweroffs) poweroffs: $out"
fi
wrun 16
if [ "$(poweroffs)" = 1 ] && has '<2>arca-power: battery critical, 4% and discharging: clean poweroff'; then
	pass watch-poweroff
else
	fail watch-poweroff "$(poweroffs) poweroffs: $out"
fi
r=$tmp/w-low
bat "$r" BAT0 Discharging 15
wrun 16
if [ "$(poweroffs)" = 0 ] && [ "$(count 'battery low, 15% and discharging')" -eq 1 ]; then
	pass watch-low-once
else
	fail watch-low-once "$out"
fi
# A Mains supply online: never a poweroff, whatever the battery says.
r=$tmp/w-mains
bat "$r" BAT0 Discharging 4
put "$r" sys/class/power_supply/AC/type Mains
put "$r" sys/class/power_supply/AC/online 1
wrun 16
if [ "$(poweroffs)" = 0 ] && ! has critical; then
	pass watch-mains-online
else
	fail watch-mains-online "$(poweroffs) poweroffs: $out"
fi
# Mains offline and the firmware's status Unknown: discharging.
r=$tmp/w-unknown
put "$r" sys/class/power_supply/AC/type Mains
put "$r" sys/class/power_supply/AC/online 0
bat "$r" BAT0 Unknown 15
wrun 1
has 'battery low, 15% and discharging' || fail watch-unknown "no warning: $out"
bat "$r" BAT0 Unknown 3
wrun 16
if [ "$(poweroffs)" = 1 ] && has 'battery critical, 3% and discharging: clean poweroff'; then
	pass watch-unknown
else
	fail watch-unknown "$(poweroffs) poweroffs: $out"
fi
# A capacity of 0 that energy_now/energy_full (60%) disagrees with.
r=$tmp/w-glitch
bat "$r" BAT0 Discharging 0
put "$r" sys/class/power_supply/BAT0/energy_now 30000
put "$r" sys/class/power_supply/BAT0/energy_full 50000
wrun 16
if [ "$(poweroffs)" = 0 ] && has 'on battery at 60%'; then
	pass watch-capacity-glitch
else
	fail watch-capacity-glitch "$(poweroffs) poweroffs: $out"
fi

exit "$failed"
