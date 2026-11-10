#!/usr/bin/env bash
# VM proof of automatic updates and rollback (no GPU).
#
#   scripts/arca/test/updates-vm.sh <evidence-dir> <snapshot-dir> [all|crash]
#
# <snapshot-dir> is what updates-build.sh made. "all" (default) needs X Y Z C built in that order
# (Z with a broken incus, C resetting the machine 240 s after each boot): the console switch Off
# keeps Y from applying; On, Y restarts in the window, bakes and is marked good; Z rolls back by
# itself with one reboot and is never applied again; an API reboot then stays on Y; C rolls back
# after two unplanned boots, with no loop. "crash" needs Y C and runs only that last case, from Y.
# The VM has cpu 2 and the builder's disk caps, and is deleted at the end with its image. Prints
# PASS/FAIL lines to <evidence-dir>/vm-proof.results; exits 1 when any case fails. Takes the
# builder lock unless ARCA_BUILDER_LOCK_HELD=1.
set -uo pipefail

[ $# -ge 2 ] || {
	echo "usage: $0 <evidence-dir> <snapshot-dir> [all|crash]" >&2
	exit 2
}
EV=$1 SNAP=$2 MODE=${3:-all}
VM=arcaos-updates-proof
P=(--project "${ARCA_PROJECT:-arcaos-dev}")
ADDR=${PROOF_ADDR:-10.10.10.1}
PORT=${PROOF_PORT:-18095}
SERVE=$SNAP/serve
OSD=/run/incus-os/unix.socket
Y=$(cat "$EV/version-Y.txt") C=$(cat "$EV/version-C.txt")
if [ "$MODE" = all ]; then
	X=$(cat "$EV/version-X.txt") Z=$(cat "$EV/version-Z.txt")
	CT=t4
else
	X=$Y Z=
	CT=t2
fi

if [ "${ARCA_BUILDER_LOCK_HELD:-}" != 1 ]; then
	exec 9>>"${ARCA_BUILDER_LOCK:-${XDG_RUNTIME_DIR:-/tmp}/arcaos-builder.lock}"
	flock 9
	export ARCA_BUILDER_LOCK_HELD=1
fi

fails=0
log() { printf '%s vm-proof: %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$EV/vm-proof.log" >&2; }
result() { printf '%s %s%s\n' "$1" "$2" "${3:+ $3}" | tee -a "$EV/vm-proof.results"; [ "$1" = PASS ] || fails=$((fails + 1)); }
vexec() { incus exec "${P[@]}" "$VM" -- "$@" </dev/null; }
osd() { vexec curl -sf --max-time 60 --unix-socket "$OSD" "$@"; }
upd() { osd http://localhost/1.0/system/update 2>/dev/null | jq -c "$1" 2>/dev/null; }
running() { osd http://localhost/1.0 2>/dev/null | jq -r '.metadata.environment.os_version // empty' 2>/dev/null; }
bootid() { vexec cat /proc/sys/kernel/random/boot_id 2>/dev/null | tr -d '\r\n'; }
until_s() { local end=$1; shift; while [ "$SECONDS" -lt "$end" ]; do "$@" && return 0; sleep 10; done; return 1; }
agent() { vexec true >/dev/null 2>&1; }
serve() { rm -rf "$SERVE"; mkdir -p "$SERVE"; cp -a "$SNAP/$1/." "$SERVE/"; log "serving tree $1"; }
journal() { vexec journalctl -u incus-osd -b"${1:-0}" --no-pager -o cat 2>/dev/null; }

# Every new boot, with its version and the ESP's UKI names at the time it was first seen.
tracker() {
	local seen="" b v
	while :; do
		b=$(bootid)
		if [ -n "$b" ] && ! grep -qx "$b" <<<"$seen"; then
			v=$(vexec sh -c '. /usr/lib/os-release; echo $IMAGE_VERSION' 2>/dev/null)
			printf '%s boot %s version %s esp %s\n' "$(date -u +%H:%M:%S)" "$b" "$v" "$(vexec ls /boot/EFI/Linux 2>/dev/null | tr '\n' ' ')" >>"$EV/esp-timeline.txt"
			seen=$(printf '%s\n%s' "$seen" "$b")
		fi
		sleep 5
	done
}
# Boot IDs after the given one (journalctl's indexes are relative to the current boot), each with the version incus-osd started on.
boots_since() {
	vexec journalctl --list-boots --no-pager -o json 2>/dev/null | jq -r --arg b "${1//-/}" '. as $l | ($l | map(.boot_id) | index($b)) as $i | if $i == null then "start-boot-not-found" else ($l[($i + 1):][] | .boot_id) end' |
		while read -r id; do
			printf '%s %s\n' "$id" "$(vexec journalctl -b "$id" -u incus-osd -o cat --no-pager 2>/dev/null | sed -n 's/.*System is starting up.* version=\([0-9]*\).*/\1/p' | head -1 | grep . || echo -)"
		done
}
backHealthy() { upd .metadata.state.health.last_result | grep -q "Rolled back to $Y; it is healthy"; }
appver() { osd "http://localhost/1.0/applications/$1" 2>/dev/null | jq -r '.metadata.state.version // .metadata.version // empty'; }

cleanup() {
	[ -z "${srv:-}" ] || kill "$srv" 2>/dev/null
	[ -z "${trk:-}" ] || kill "$trk" 2>/dev/null
	if [ "${KEEP:-0}" != 1 ]; then
		incus delete "${P[@]}" --force "$VM" </dev/null >/dev/null 2>&1
		incus delete "${P[@]}" --force "$VM-sb" </dev/null >/dev/null 2>&1
		incus image delete "${P[@]}" "$VM" </dev/null >/dev/null 2>&1
		log "VM and image deleted"
	fi
}
trap cleanup EXIT

: >"$EV/vm-proof.results"
: >"$EV/esp-timeline.txt"
log "mode $MODE, versions X=$X Y=$Y Z=$Z C=$C"

# Update server on the host's incusbr0 address.
serve t1
python3 -m http.server --bind "$ADDR" --directory "$SERVE" "$PORT" >"$EV/http.log" 2>&1 &
srv=$!
sleep 2

# The VM: cpu 2, the builder's disk caps.
incus delete "${P[@]}" --force "$VM" </dev/null >/dev/null 2>&1
incus image delete "${P[@]}" "$VM" </dev/null >/dev/null 2>&1
incus image import -q "${P[@]}" "$SNAP/vm/metadata.tar.xz" "$SNAP/vm/os.qcow2" --alias "$VM" </dev/null || exit 1
incus create -q --vm "${P[@]}" "$VM" "$VM" -c security.secureboot=false -c limits.cpu=2 -c limits.memory=8GiB \
	-d root,size=50GiB -d root,limits.read=150MB -d root,limits.write=80MB </dev/null || exit 1
incus config device add -q "${P[@]}" "$VM" vtpm tpm </dev/null
incus config set "${P[@]}" "$VM" systemd.credential.fully-enable-incus-agent=true </dev/null
incus config show "${P[@]}" "$VM" </dev/null | grep -E "limits\.|secureboot" >"$EV/vm-limits.txt"
incus start "${P[@]}" "$VM" </dev/null
log "VM started"

# P1: first boot on X installs the applications.
until_s $((SECONDS + 600)) agent || { result FAIL boot "no agent"; exit 1; }
osd -X POST http://localhost/1.0/system/update/:check >/dev/null 2>&1
apps_ok() { [ "$(osd http://localhost/1.0/applications | jq -r '.metadata | length')" -ge 3 ] && [ "$(vexec systemctl is-active arca.service)" = active ]; }
if until_s $((SECONDS + 900)) apps_ok; then result PASS boot-x "running $(running), incus+arca+debug installed"; else result FAIL boot-x "applications missing"; exit 1; fi
tracker &
trk=$!

# Secure Boot as the machine sees it (Incus secureboot=false only means no Microsoft keys; the image enrolls its own).
sb=$(vexec bootctl status 2>/dev/null | grep -m1 -i "secure boot" | sed 's/^ *//')
tpm=$(vexec sh -c 'ls /dev/tpmrm0 2>/dev/null')
{ echo "incus security.secureboot=$(incus config get "${P[@]}" "$VM" security.secureboot </dev/null)"; echo "bootctl: $sb"; echo "tpm: ${tpm:-none}"; } >"$EV/secureboot.txt"
if grep -qi "enabled (user)\|enabled" <<<"$sb" && [ -n "$tpm" ]; then result PASS secureboot-on "$sb; vTPM $tpm"; else result FAIL secureboot-on "${sb:-no bootctl line}; tpm ${tpm:-none}"; fi

# The proof's settings: checks every minute, a five minute bake, a window around now.
window_now() {
	local hour start end cfg
	hour=$((10#$(vexec date +%H)))
	start=$(((hour + 23) % 24)) end=$(((hour + 3) % 24))
	cfg=$(osd http://localhost/1.0/system/update | jq -c --argjson s "$start" --argjson e "$end" --argjson off "$1" \
		'{config: (.metadata.config | .check_frequency="1m" | .bake_time="5m" | .auto_updates_off=$off | .restart_window={start_hour:$s,start_minute:0,end_hour:$e,end_minute:0})}')
	osd -X PUT -d "$cfg" http://localhost/1.0/system/update >/dev/null
	log "config: $(upd .metadata.config)"
}
window_now false

# A container that runs before the updates, to see it come back.
if vexec incus launch images:alpine/3.21 c1 >"$EV/c1-launch.log" 2>&1; then
	have_c1=1
	log "container c1 running"
else
	have_c1=0
	log "container c1 could not be launched; instance checks skipped (see c1-launch.log)"
fi

if [ "$MODE" = all ]; then
	# The console's virtual keyboard (u opens the updates page, o switches).
	vexec bash -c '
	d=/run/proof-kbd; install -d -m 0700 $d
	printf "%s\n" "# EVEMU 1.3" "N: arca-proof-keyboard" "I: 0006 0001 0001 0001" "P: 00 00 00 00 00 00 00 00" \
		"B: 00 03 00 00 00 00 00 00 00" "B: 01 fe ff ff ff ff ff ff ff" "B: 01 ff ff ff ff ff ff ff ff" > $d/kbd.desc
	systemd-run -q --unit=arca-proof-keyboard --collect evemu-device $d/kbd.desc
	for k in u:22 o:24 esc:1; do c=${k#*:}; printf "E: 0.040000 0001 %04x 1\nE: 0.040000 0000 0000 0\nE: 0.080000 0001 %04x 0\nE: 0.080000 0000 0000 0\n" $c $c > $d/${k%:*}.events; done' >/dev/null 2>&1
	sleep 3
	press() { vexec bash -c 'n=$(grep -l "^arca-proof-keyboard$" /sys/class/input/event*/device/name | head -1); n=/dev/input/$(basename "$(dirname "$(dirname "$n")")"); evemu-play "$n" < /run/proof-kbd/$1.events' _ "$1" >>"$EV/vm-proof.log" 2>&1; sleep 2; }

	# P2: switch Off on the console, then publish Y: nothing applies.
	press u
	vexec sh -c 'fold -w 100 /dev/vcs1' | grep -a "Automatic" >"$EV/screen-updates-page.txt"
	press o
	press esc
	off=$(upd .metadata.config.auto_updates_off)
	if [ "$off" = true ] && journal | grep "Automatic updates switched by the owner" | tail -1 | grep -q false; then
		result PASS console-switch-off "auto_updates_off=true, journal: switched by the owner on=false"
	else
		result FAIL console-switch-off "auto_updates_off=$off"
		exit 1
	fi
	b0=$(bootid)
	serve t2
	sleep 300
	st=$(upd .metadata.state.status)
	next=$(osd http://localhost/1.0 | jq -r '.metadata.environment.os_version_next // empty' 2>/dev/null)
	staged=$(vexec sh -c "ls /var/lib/updates 2>/dev/null | grep -c $Y" || true)
	if [ "$(bootid)" = "$b0" ] && [ "$(running)" = "$X" ] && [ "${staged:-0}" = 0 ] && [ "$st" = '"Automatic updates are off"' ]; then
		result PASS switch-off-nothing-applies "5 min with Y published: still $X, same boot, no Y files staged, status $st"
	else
		result FAIL switch-off-nothing-applies "running $(running) next $next staged $staged status $st"
	fi

	# P3: switch On on the console: Y installs, restarts in the window, bakes and is marked good.
	press u
	press o
	press esc
	log "switched on: $(upd .metadata.config.auto_updates_off)"
	t0=$SECONDS
	onY() { [ "$(running)" = "$Y" ]; }
	if until_s $((t0 + 1200)) onY; then
		log "running $Y after $((SECONDS - t0)) s"
		vexec ls /boot/EFI/Linux >"$EV/esp-Y-first-boot.txt" 2>&1
	else
		result FAIL good-update-restart "not on $Y after 1200 s: $(upd .metadata.state)"
	fi
	goodY() { [ "$(upd .metadata.state.health.good_version)" = "\"$Y\"" ]; }
	if until_s $((SECONDS + 900)) goodY; then
		result PASS good-update-baked "$Y marked good: $(upd '.metadata.state.health | {current_version,previous_version,good_version,last_result}')"
	else
		result FAIL good-update-baked "$(upd .metadata.state.health)"
	fi
	journal | grep -E "Update health check|marked good|Started an instance" >"$EV/journal-Y.txt"
	journal -1 | grep -E "Restarting into the update|Update restart deferred" >"$EV/journal-X-restart.txt"
	vexec ls /boot/EFI/Linux >"$EV/esp-after-Y.txt" 2>&1
	if [ "$have_c1" = 1 ]; then
		if [ "$(vexec incus list c1 -f csv -c s)" = RUNNING ]; then result PASS instance-back "c1 running on $Y"; else result FAIL instance-back "c1 not running"; fi
	fi
	if grep -q "IncusOS_$Y.efi" "$EV/esp-after-Y.txt" && ! grep -q "$Y+" "$EV/esp-after-Y.txt"; then
		result PASS boot-entry-blessed "$(tr '\n' ' ' <"$EV/esp-after-Y.txt")"
	else
		result FAIL boot-entry-blessed "$(tr '\n' ' ' <"$EV/esp-after-Y.txt")"
	fi

	# P4: publish the broken Z: it installs, restarts, fails its bake and rolls back to Y by itself.
	boots_before=$(vexec journalctl --list-boots --no-pager 2>/dev/null | grep -c .)
	serve t3
	t0=$SECONDS
	backOnY() { [ "$(running)" = "$Y" ] && [ "$(upd '.metadata.state.health.failed_versions | has("'"$Z"'")')" = true ]; }
	sawZ=0
	ids=$(bootid)
	while [ $((SECONDS - t0)) -lt 2400 ]; do
		[ "$(running)" != "$Z" ] || sawZ=1
		b=$(bootid)
		[ -z "$b" ] || grep -qx "$b" <<<"$ids" || ids=$(printf '%s\n%s' "$ids" "$b")
		backOnY && break
		sleep 5
	done
	newboots=$(($(grep -c . <<<"$ids") - 1))
	boots_after=$(vexec journalctl --list-boots --no-pager 2>/dev/null | grep -c .)
	journal -1 | grep -E "Update health check|rolling back|Boot entry" >"$EV/journal-Z.txt"
	health=$(upd '.metadata.state.health | {current_version,good_version,failed_versions,last_result,needs_owner}')
	if backOnY; then
		result PASS broken-update-rolled-back "$health; booted Z seen=$sawZ; boots $boots_before -> $boots_after"
	else
		result FAIL broken-update-rolled-back "$health"
	fi
	# Exactly two new boots since P4: Z, then the one rollback reboot into Y.
	if [ "$newboots" -eq 2 ]; then
		result PASS one-rollback-reboot "$newboots new boot IDs (Z, then Y); journal boots $boots_before -> $boots_after"
	else
		result FAIL one-rollback-reboot "$newboots new boot IDs; journal boots $boots_before -> $boots_after"
	fi
	if until_s $((SECONDS + 900)) backHealthy; then result PASS rollback-healthy "$(upd .metadata.state.health.last_result)"; else result FAIL rollback-healthy "$(upd .metadata.state.health)"; fi
	vexec ls /boot/EFI/Linux >"$EV/esp-after-Z.txt" 2>&1
	if grep -q "IncusOS_$Z+0-" "$EV/esp-after-Z.txt"; then result PASS bad-entry-marked "$(tr '\n' ' ' <"$EV/esp-after-Z.txt")"; else result FAIL bad-entry-marked "$(tr '\n' ' ' <"$EV/esp-after-Z.txt")"; fi
	if [ "$have_c1" = 1 ]; then
		if [ "$(vexec incus list c1 -f csv -c s)" = RUNNING ]; then result PASS instance-back-after-rollback "c1 running on $Y"; else result FAIL instance-back-after-rollback "c1 not running"; fi
	fi

	# P5: Z stays published for 5 more minutes of checks: it is never applied again.
	b1=$(bootid)
	sleep 300
	skips=$(journal | grep -c "failed before")
	if [ "$(bootid)" = "$b1" ] && [ "$(running)" = "$Y" ] && [ "$skips" -ge 1 ]; then
		result PASS failed-never-reapplied "same boot, still $Y, $skips 'failed before' skips in the journal"
	else
		result FAIL failed-never-reapplied "running $(running) skips $skips"
	fi

	# P6: an API reboot after the rollback comes back on Y, not on the failed Z.
	b2=$(bootid)
	osd -X POST http://localhost/1.0/system/:reboot >/dev/null 2>&1
	newBootUp() { b=$(bootid); [ -n "$b" ] && [ "$b" != "$b2" ] && [ -n "$(running)" ]; }
	if until_s $((SECONDS + 900)) newBootUp && [ "$(running)" = "$Y" ]; then
		result PASS api-reboot-skips-failed "new boot, running $Y; Z still failed: $(upd '.metadata.state.health.failed_versions | keys')"
	else
		result FAIL api-reboot-skips-failed "running $(running) boot $(bootid)"
	fi
	journal -1 | grep -E "System is shutting down|Clean stop" >"$EV/journal-api-reboot.txt"
fi

# P7: publish C, which resets the machine 240 s after each boot: two unplanned boots roll it back, with no loop.
if [ "$have_c1" = 1 ]; then vexec incus list c1 -f csv -c s >"$EV/c1-before-C.txt" 2>&1; fi
window_now "$(upd .metadata.config.auto_updates_off)"
apps_before="incus=$(appver incus) arca=$(appver arca)"
idx=$(bootid)
serve "$CT"
t0=$SECONDS
backFromC() { [ "$(running)" = "$Y" ] && [ "$(upd '.metadata.state.health.failed_versions | has("'"$C"'")')" = true ]; }
until_s $((t0 + 3600)) backFromC
okC=$?
kept=$(upd .metadata.state.health.kept_applications)
apps_after="incus=$(appver incus) arca=$(appver arca)"
boots_since "$idx" >"$EV/boots-C.txt"
cboots=$(grep -c " $C$" "$EV/boots-C.txt")
reasonC=$(upd ".metadata.state.health.failed_versions[\"$C\"]")
journal -1 | grep -E "Update health check|restarted|rolling back|Rolling back|Boot entry" >"$EV/journal-C.txt"
for i in 1 2 3 4; do vexec journalctl -b -$i -u incus-osd -o short-iso --no-pager 2>/dev/null | grep -E "starting up|health|restart|Roll|roll|bake" ; done >"$EV/journal-C-boots.txt"
if [ "$okC" = 0 ] && grep -q "restarted unexpectedly" <<<"$reasonC"; then
	result PASS crash-rolled-back "back on $Y after $((SECONDS - t0)) s; $C failed: $reasonC; boots: $(tr '\n' ',' <"$EV/boots-C.txt")"
else
	result FAIL crash-rolled-back "running $(running); reason $reasonC; boots: $(tr '\n' ',' <"$EV/boots-C.txt")"
fi
# The C boots' own journal: one strike per unplanned boot, one rollback, and the last boot on Y.
total=$(grep -c . "$EV/boots-C.txt")
lastv=$(tail -1 "$EV/boots-C.txt" | cut -d' ' -f2)
strikes=0 rollbacks=0
for id in $(grep " $C$" "$EV/boots-C.txt" | cut -d' ' -f1); do
	vexec journalctl -b "$id" -u incus-osd -o short-iso --no-pager >"$EV/journal-C-$id.txt" 2>/dev/null
	strikes=$((strikes + $(grep -c "reason=the machine restarted unexpectedly during the health check" "$EV/journal-C-$id.txt")))
	rollbacks=$((rollbacks + $(grep -c "Update failed its health check; rolling back" "$EV/journal-C-$id.txt")))
done
if [ "$cboots" -eq 3 ] && [ "$total" -eq 4 ] && [ "$lastv" = "$Y" ] && [ "$strikes" -eq 2 ] && [ "$rollbacks" -eq 1 ]; then
	result PASS crash-no-loop "$cboots boots on $C (1 planned, then $strikes unplanned-boot strikes, $rollbacks rollback), then 1 on $Y; $total new boots"
else
	result FAIL crash-no-loop "$cboots boots on $C, $total new boots, last on $lastv, $strikes strikes, $rollbacks rollbacks"
fi
if [ "$kept" != null ] && [ -n "$kept" ]; then result PASS rollback-keeps-apps "kept $kept; before C $apps_before, after $apps_after"; else result FAIL rollback-keeps-apps "kept $kept; before C $apps_before, after $apps_after"; fi
if until_s $((SECONDS + 900)) backHealthy; then result PASS crash-rollback-healthy "$(upd .metadata.state.health.last_result)"; else result FAIL crash-rollback-healthy "$(upd .metadata.state.health)"; fi
vexec ls /boot/EFI/Linux >"$EV/esp-after-C.txt" 2>&1
b3=$(bootid)
sleep 300
skipsC=$(journal | grep "failed before" | grep -c "$C")
if [ "$(bootid)" = "$b3" ] && [ "$(running)" = "$Y" ] && [ "$skipsC" -ge 1 ]; then
	result PASS crash-never-reapplied "same boot, still $Y, $skipsC 'failed before' skips of $C"
else
	result FAIL crash-never-reapplied "running $(running) skips $skipsC"
fi
upd .metadata >"$EV/final-update-state.json"
journal >"$EV/journal-final-boot.txt"
if [ "$MODE" = all ]; then
	# Feasibility only: the same image with Incus security.secureboot=true (Microsoft keys, firmware not in setup mode).
	incus stop "${P[@]}" --force "$VM" </dev/null >/dev/null 2>&1
	incus create -q --vm "${P[@]}" "$VM" "$VM-sb" -c security.secureboot=true -c limits.cpu=2 -c limits.memory=8GiB \
		-d root,size=50GiB -d root,limits.read=150MB -d root,limits.write=80MB </dev/null
	incus config device add -q "${P[@]}" "$VM-sb" vtpm tpm </dev/null
	incus config set "${P[@]}" "$VM-sb" systemd.credential.fully-enable-incus-agent=true </dev/null
	incus start "${P[@]}" "$VM-sb" </dev/null
	sbagent() { incus exec "${P[@]}" "$VM-sb" -- true </dev/null >/dev/null 2>&1; }
	if until_s $((SECONDS + 420)) sbagent; then sbres="boots: $(incus exec "${P[@]}" "$VM-sb" -- bootctl status </dev/null 2>/dev/null | grep -m1 -i 'secure boot')"; else sbres="no agent within 420 s; $(incus info "${P[@]}" "$VM-sb" </dev/null | grep -m1 Status)"; fi
	echo "NOTE secureboot-true $sbres" | tee -a "$EV/vm-proof.results"
	incus delete "${P[@]}" --force "$VM-sb" </dev/null >/dev/null 2>&1
fi

log "done, $fails failures"
exit $((fails > 0))
