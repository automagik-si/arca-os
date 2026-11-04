#!/usr/bin/env bash
# The console cases of vm-test.sh --checks console, run on this host against a
# running test VM whose seeded applications are installed (after vm-test.sh's
# step 8).
#
#   run.sh --project arcaos-* --vm <name>
#
# It waits up to 300 s for "Console: started" in incus-osd's journal, then cuts
# the VM's console capture (`incus console --show-log`, which only grows) at the
# size it has at that moment, so nothing drawn before counts. The screen is
# rebuilt from the rest of the capture: the cursor moves (ESC [<r>;<c>H), the
# clears and the text are replayed into a grid of rows, and every other escape
# sequence is dropped. The capture and the rows stay in a private temporary
# directory and are never printed. The TUI draws only what changed, and repaints
# the whole screen once a minute, so a complete screen shows within about 60 s
# of the cut. Cases:
#   header          within 75 s of the cut, row 1 holds the VM's hostname and
#                   "Arca OS <release>" (environment.os_version of the VM's
#                   GET /1.0), and row 2 the IPv4 address of the VM's default
#                   route device
#   instances       after `incus create --empty probe-1` in the VM (or the same
#                   request over the Incus socket when the VM has no incus
#                   command), within 15 s one row holds probe-1, CT and STOPPED
#   frame-replaced  from the cut on, for at least 30 s and one whole-screen
#                   repaint, upstream's footer label "Network configuration:" is
#                   neither on the rebuilt screen nor anywhere in the capture's
#                   text, and the journal has no "Console: disabled"
#   keys            Home, then Down until probe-1's row is selected, Enter
#                   shows its details page (project default), and Esc returns
#                   to the instances; the screen is read after a whole-screen
#                   repaint each time
#   upstream-toggle L shows upstream's footer "Network configuration:", and L
#                   again removes it from a freshly repainted screen
#   privacy         with the details page open, the rebuilt screen and the
#                   capture hold none of two canaries set in probe-1's
#                   configuration and none of an Incus trust token, all
#                   generated, searched and then removed inside the VM
# Each case prints "PASS|FAIL|SKIP <case> <detail>"; no detail holds an
# address, a name or another value read from the VM.
#
# Exit codes: 0 no FAIL, 1 a FAIL, 2 usage.
set -euo pipefail

OSD_SOCK=/run/incus-os/unix.socket
INCUS_SOCK=/var/lib/incus/unix.socket
PROBE=probe-1
FOOTER='Network configuration:'

project="" vm=""
usage() { awk 'NR > 1 && /^set -euo pipefail$/ { exit } NR > 1' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'console-test: %s\n' "$*" >&2; }
while [ $# -gt 0 ]; do
	case "$1" in
	--project) [ $# -ge 2 ] || usage; project=$2; shift 2 ;;
	--vm) [ $# -ge 2 ] || usage; vm=$2; shift 2 ;;
	*) usage ;;
	esac
done
case "$project" in arcaos-*) ;; *) usage ;; esac
printf '%s' "$vm" | grep -Eqx '[a-z0-9][a-z0-9-]*' || usage

P=(--project "$project")
tmp=$(mktemp -d)
fails=0
result() { # <PASS|FAIL|SKIP> <case> <detail>
	printf '%s %s %s\n' "$1" "$2" "$3"
	[ "$1" != FAIL ] || fails=$((fails + 1))
}
vexec() { incus exec "${P[@]}" "$vm" -- "$@"; }
# privacy_done keeps the EXIT trap from cleaning up twice.
privacy_done=0
# privacy_cleanup removes probe-1's canaries and the console-probe trust token
# from the VM. Every step is best-effort and silent: the VM may be gone by the
# time the EXIT trap runs.
privacy_cleanup() {
	[ "$privacy_done" -eq 0 ] || return 0
	privacy_done=1
	vexec sh -c 'incus config unset probe-1 user.secret-probe' >/dev/null 2>&1 || true
	vexec sh -c 'incus config unset probe-1 user.droid.owner' >/dev/null 2>&1 || true
	# shellcheck disable=SC2016  # expanded by the shell in the VM
	vexec sh -c 'if [ -s /run/console-test/token.txt ]; then incus config trust revoke-token "$(cat /run/console-test/token.txt)"; fi' >/dev/null 2>&1 || true
}
# shellcheck disable=SC2317,SC2329  # called from the EXIT trap
cleanup() {
	privacy_cleanup || true
	rm -rf "$tmp"
}
trap cleanup EXIT
# grep reads to the end: an early exit would fail the pipeline (pipefail) on SIGPIPE.
journal_has() { vexec journalctl -b0 _SYSTEMD_UNIT=incus-osd.service -o cat --no-pager 2>/dev/null | grep -F -- "$1" >/dev/null; }
# until_s <deadline, in $SECONDS> <command...>: 0 once the command succeeds, trying
# every 2 s and once more at the deadline.
until_s() {
	local limit=$1
	shift
	while :; do
		if "$@"; then return 0; fi
		[ "$SECONDS" -lt "$limit" ] || return 1
		sleep 2
	done
}

# The replay (python3, isolated): argv is the capture, the cut and the output
# directory. It writes screen.txt (one line per row, trailing blanks dropped),
# text.txt (the capture's text without escape sequences) and repaints (how many
# whole-screen clears, ESC c or ESC [2J, came after the cut). Every character
# takes one cell; writes past the right edge do not wrap (tcell turns automatic
# margins off).
# shellcheck disable=SC2016  # a python program
REPLAY='
import sys, unicodedata

raw, cut, out = sys.argv[1], int(sys.argv[2]), sys.argv[3]
with open(raw, "rb") as f:
    f.seek(cut)
    s = f.read().decode("utf-8", "replace")

LIMIT = 500
grid = {}
r = c = 0
repaints = 0
text = []


def erase(row, start, end):
    line = grid.get(row)
    if line:
        for k in [k for k in line if start <= k < end]:
            del line[k]


def drop_rows(keep):
    for k in [k for k in grid if not keep(k)]:
        del grid[k]


def num(p, i, d):
    try:
        return int(p[i]) if p[i] else d
    except (IndexError, ValueError):
        return d


i, n = 0, len(s)
while i < n:
    ch = s[i]
    if ch == "\x1b":
        if i + 1 >= n:
            break
        nx = s[i + 1]
        if nx == "[":
            j = i + 2
            while j < n and "\x30" <= s[j] <= "\x3f":
                j += 1
            params = s[i + 2:j]
            while j < n and "\x20" <= s[j] <= "\x2f":
                j += 1
            if j >= n:
                break
            final, i = s[j], j + 1
            if params and params[0] in "<=>?":
                continue
            p = params.split(";")
            k = max(num(p, 0, 1), 1)
            if final in "Hf":
                r, c = k - 1, max(num(p, 1, 1), 1) - 1
            elif final == "J":
                m = num(p, 0, 0)
                if m == 2:
                    grid.clear()
                    repaints += 1
                elif m == 0:
                    erase(r, c, LIMIT)
                    drop_rows(lambda row: row <= r)
                elif m == 1:
                    erase(r, 0, c + 1)
                    drop_rows(lambda row: row >= r)
            elif final == "K":
                m = num(p, 0, 0)
                erase(r, c if m == 0 else 0, c + 1 if m == 1 else LIMIT)
            elif final == "X":
                erase(r, c, c + k)
            elif final == "A":
                r = max(r - k, 0)
            elif final == "B":
                r += k
            elif final == "C":
                c += k
            elif final == "D":
                c = max(c - k, 0)
            elif final == "G":
                c = k - 1
            elif final == "d":
                r = k - 1
            continue
        if nx in "]PX^_":
            j = i + 2
            while j < n and s[j] != "\x07" and not (s[j] == "\x1b" and s[j + 1:j + 2] == "\\"):
                j += 1
            i = j + (1 if j < n and s[j] == "\x07" else 2)
            continue
        if nx == "c":
            grid.clear()
            r = c = 0
            repaints += 1
            i += 2
            continue
        j = i + 1
        while j < n and "\x20" <= s[j] <= "\x2f":
            j += 1
        i = j + 1
        continue
    i += 1
    if ch == "\r":
        c = 0
    elif ch == "\n":
        r += 1
        text.append("\n")
    elif ch == "\b":
        c = max(c - 1, 0)
    elif ch == "\t":
        c = (c // 8 + 1) * 8
    elif ch < " " or "\x7f" <= ch <= "\x9f":
        pass
    elif unicodedata.combining(ch):
        text.append(ch)
    else:
        if r < LIMIT and c < LIMIT:
            grid.setdefault(r, {})[c] = ch
        c += 1
        text.append(ch)

with open(out + "/screen.txt", "w", encoding="utf-8") as f:
    for row in range(max(grid) + 1 if grid else 0):
        line = grid.get(row, {})
        width = max(line) + 1 if line else 0
        f.write("".join(line.get(x, " ") for x in range(width)).rstrip() + "\n")
with open(out + "/text.txt", "w", encoding="utf-8") as f:
    f.write("".join(text))
with open(out + "/repaints", "w", encoding="utf-8") as f:
    f.write("%d\n" % repaints)
'

cut=0 shrunk=0 replay_failed=0
# screen: fetches the capture into $tmp/console.raw and replays what follows the
# cut (see REPLAY). 1 when the capture is shorter than the cut or the replay
# failed; neither prints any of the capture.
# shellcheck disable=SC2317,SC2329  # called through until_s
screen() {
	incus console "${P[@]}" "$vm" --show-log >"$tmp/console.raw" 2>/dev/null || true
	if [ "$(stat -c %s "$tmp/console.raw")" -lt "$cut" ]; then
		shrunk=1
		return 1
	fi
	if ! python3 -I -c "$REPLAY" "$tmp/console.raw" "$cut" "$tmp" 2>"$tmp/replay.err"; then
		replay_failed=1
		return 1
	fi
}
repaints() { cat "$tmp/repaints" 2>/dev/null || echo 0; }
# why_screen: the reason screen failed, for a detail.
why_screen() {
	if [ "$shrunk" -eq 1 ]; then echo "the console capture shrank below the cut"; return; fi
	if [ "$replay_failed" -eq 1 ]; then echo "the replay of the capture failed"; return; fi
	echo "no whole-screen repaint after the cut"
}
# repainted <base>: a whole-screen repaint came after the one counted as base.
# shellcheck disable=SC2317,SC2329  # called through until_s
repainted() { screen && [ "$(repaints)" -gt "$1" ]; }
# kbd_setup <dir> (in the VM, bash): a virtual keyboard and one event file per key.
# shellcheck disable=SC2317,SC2329  # run inside the VM
kbd_setup() {
	local d=$1 us=0 k
	install -d -m 0700 "$d"
	printf '%s\n' "# EVEMU 1.3" "N: arca-test-keyboard" "I: 0006 0001 0001 0001" \
		"P: 00 00 00 00 00 00 00 00" "B: 00 03 00 00 00 00 00 00 00" \
		"B: 01 fe ff ff ff ff ff ff ff" "B: 01 ff ff ff ff ff ff ff ff" > "$d/kbd.desc"
	systemd-run -q --unit=arca-test-keyboard --collect evemu-device "$d/kbd.desc" || return 1
	vkey() { case $1 in home) echo 102 ;; up) echo 103 ;; down) echo 108 ;; enter) echo 28 ;; esc) echo 1 ;; l) echo 38 ;; esac; }
	vevents() { local n c v; for n in "$@"; do c=$(vkey "$n"); for v in 1 0; do us=$((us + 40000))
		printf 'E: %d.%06d 0001 %04x %d\nE: %d.%06d 0000 0000 0\n' $((us / 1000000)) $((us % 1000000)) "$c" "$v" \
			$((us / 1000000)) $((us % 1000000)); done; done; }
	for k in home up down enter esc l; do us=0; vevents "$k" > "$d/$k.events"; done
	return 0
}
# type_keys <file in the VM>: plays a key sequence on the virtual keyboard there.
# shellcheck disable=SC2016  # expanded by the shell in the VM
type_keys() { vexec bash -c 'n=$(grep -l "^arca-test-keyboard$" /sys/class/input/event*/device/name | head -1); n=/dev/input/$(basename "$(dirname "$(dirname "$n")")"); evemu-play "$n" < "$1"' _ "$1"; }
# shellcheck disable=SC2317,SC2329  # called through until_s
kbd_ready() { vexec sh -c 'grep -lq "^arca-test-keyboard$" /sys/class/input/event*/device/name' 2>/dev/null; }

# The cut.
t0=$SECONDS
if ! until_s $((t0 + 300)) journal_has 'Console: started'; then
	for c in header instances frame-replaced; do
		result FAIL "$c" "no 'Console: started' in incus-osd's journal after 300 s"
	done
	exit 1
fi
incus console "${P[@]}" "$vm" --show-log >"$tmp/console.raw" 2>/dev/null || true
cut=$(stat -c %s "$tmp/console.raw")
t_cut=$SECONDS
log "the console started; the capture is cut at byte $cut"

# header
host=$(vexec cat /proc/sys/kernel/hostname 2>/dev/null | tr -d '\r\n' || true)
release=$(vexec curl -sf --max-time 30 --unix-socket "$OSD_SOCK" http://localhost/1.0 2>/dev/null \
	| jq -r '.metadata.environment.os_version // empty' 2>/dev/null || true)
dev=$(vexec ip -4 route show default 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit } }' || true)
ipv4=""
if [ -n "$dev" ]; then
	ipv4=$(vexec ip -4 -o addr show dev "$dev" scope global 2>/dev/null | awk '{ split($4, a, "/"); print a[1]; exit }' || true)
fi
row() { sed -n "${1}p" "$tmp/screen.txt" 2>/dev/null || true; }
# header_why: what rows 1 and 2 lack, or nothing.
header_why() {
	local r1 r2
	r1=" $(row 1) " r2=" $(row 2) "
	if [[ $r1 != *" $host "* ]]; then echo "row 1 has no hostname"; return; fi
	if [[ $r1 != *" Arca OS $release "* ]]; then echo "row 1 has no 'Arca OS <release>'"; return; fi
	if [[ $r2 != *" $ipv4 "* ]]; then echo "row 2 has no default route IPv4 address"; fi
}
# shellcheck disable=SC2317,SC2329  # called through until_s
header_ok() { screen && [ -z "$(header_why)" ]; }
if [ -z "$host" ] || [ -z "$release" ] || [ -z "$ipv4" ]; then
	result FAIL header "could not read the VM's hostname, release or default route IPv4 address"
elif until_s $((t_cut + 75)) header_ok; then
	result PASS header "hostname and 'Arca OS <release>' on row 1, the default route IPv4 address on row 2, $((SECONDS - t_cut)) s after the cut"
elif [ "$(repaints)" -eq 0 ] || [ "$shrunk" -eq 1 ] || [ "$replay_failed" -eq 1 ]; then
	result FAIL header "$(why_screen) within 75 s"
else
	result FAIL header "$(header_why) 75 s after the cut ($(repaints) whole-screen repaints)"
fi

# instances
# create_probe: creates the stopped, empty container probe-1 in the VM's Incus.
# shellcheck disable=SC2317,SC2329  # called through until_s
create_probe() {
	local op
	vexec incus create --empty "$PROBE" >/dev/null 2>&1 && return 0
	op=$(vexec curl -sf --max-time 60 --unix-socket "$INCUS_SOCK" -X POST -H 'Content-Type: application/json' \
		--data "{\"name\":\"$PROBE\",\"source\":{\"type\":\"none\"}}" http://incus/1.0/instances 2>/dev/null \
		| jq -r '.operation // empty' 2>/dev/null || true)
	[[ $op =~ ^/1\.0/operations/[0-9a-f-]+$ ]] || return 1
	vexec curl -sf --max-time 90 --unix-socket "$INCUS_SOCK" "http://incus$op/wait?timeout=60" 2>/dev/null \
		| jq -e '.metadata.status == "Success"' >/dev/null 2>&1
}
# probe_row: one row of the rebuilt screen holds probe-1, CT and STOPPED as words.
# shellcheck disable=SC2317,SC2329  # called through until_s
probe_row() {
	screen || return 1
	awk -v p="$PROBE" '{ a = b = s = 0; for (i = 1; i <= NF; i++) { a += ($i == p); b += ($i == "CT"); s += ($i == "STOPPED") } if (a && b && s) f = 1 }
		END { exit !f }' "$tmp/screen.txt"
}
# Incus may still be starting after step 8: the create is retried for 120 s.
if ! until_s $((SECONDS + 120)) create_probe; then
	result FAIL instances "could not create $PROBE in the VM's Incus within 120 s"
else
	t_create=$SECONDS
	if until_s $((t_create + 15)) probe_row; then
		result PASS instances "a row holds $PROBE, CT and STOPPED $((SECONDS - t_create)) s after the create"
	elif [ "$shrunk" -eq 1 ] || [ "$replay_failed" -eq 1 ]; then
		result FAIL instances "$(why_screen)"
	else
		result FAIL instances "no row holds $PROBE, CT and STOPPED 15 s after the create"
	fi
fi

# frame-replaced
# shellcheck disable=SC2317,SC2329  # called through until_s
window_done() { screen && [ $((SECONDS - t_cut)) -ge 30 ] && [ "$(repaints)" -ge 1 ]; }
until_s $((t_cut + 90)) window_done || true
if [ "$shrunk" -eq 1 ] || [ "$replay_failed" -eq 1 ]; then
	result FAIL frame-replaced "$(why_screen)"
elif grep -qF -- "$FOOTER" "$tmp/screen.txt" "$tmp/text.txt"; then
	result FAIL frame-replaced "upstream's footer label was drawn after the cut"
elif journal_has 'Console: disabled'; then
	result FAIL frame-replaced "the journal says the console was disabled"
elif [ "$(repaints)" -eq 0 ]; then
	result FAIL frame-replaced "no whole-screen repaint within 90 s of the cut"
else
	result PASS frame-replaced "no upstream footer label in $((SECONDS - t_cut)) s after the cut ($(repaints) whole-screen repaints)"
fi

# keys, upstream-toggle and privacy: the console's keys, typed on a virtual keyboard
# in the VM. Each waits for a whole-screen repaint, so the rebuilt screen holds only
# what the new state drew, never a stale row from the state before it.
# The kbd_* predicates are called through until_s.
# shellcheck disable=SC2317,SC2329
kbd_details_open() { screen && [ "$(repaints)" -gt "$1" ] && grep -Eq 'Project +default' "$tmp/screen.txt"; }
# shellcheck disable=SC2317,SC2329
kbd_dashboard_back() { screen && [ "$(repaints)" -gt "$1" ] && grep -q 'Instances' "$tmp/screen.txt" && ! grep -q 'Project' "$tmp/screen.txt"; }
# shellcheck disable=SC2317,SC2329
kbd_upstream_shown() { screen && grep -qF -- "$FOOTER" "$tmp/screen.txt"; }
# shellcheck disable=SC2317,SC2329
kbd_console_back() { screen && [ "$(repaints)" -gt "$1" ] && grep -q 'Instances' "$tmp/screen.txt" && ! grep -qF -- "$FOOTER" "$tmp/screen.txt"; }
# kbd_probe_selected <base>: a whole-screen repaint after base marks probe-1's row.
# shellcheck disable=SC2317,SC2329  # called through until_s
kbd_probe_selected() { screen && [ "$(repaints)" -gt "$1" ] && grep -qF "▶ $PROBE" "$tmp/screen.txt"; }
# select_probe: Home, then Down until a repainted screen marks probe-1's row, at
# most 10 Down presses. 1 when probe-1 was never selected.
select_probe() {
	local base try
	base=$(repaints)
	type_keys "/run/console-test/home.events"
	sleep 1
	if until_s $((SECONDS + 30)) kbd_probe_selected "$base"; then return 0; fi
	for ((try = 0; try < 10; try++)); do
		base=$(repaints)
		type_keys "/run/console-test/down.events"
		if until_s $((SECONDS + 90)) kbd_probe_selected "$base"; then return 0; fi
	done
	return 1
}

# The virtual keyboard, once, inside the VM. Its setup prints nothing.
vexec bash -c "$(declare -f kbd_setup); kbd_setup /run/console-test" >/dev/null 2>&1 || true

if ! until_s $((SECONDS + 60)) kbd_ready; then
	for c in keys upstream-toggle privacy; do
		result FAIL "$c" "the virtual keyboard did not appear within 60 s"
	done
else
	# keys: Home and Down select probe-1, Enter opens its details, Esc closes it.
	if select_probe; then
		base=$(repaints)
		type_keys "/run/console-test/enter.events"
		if until_s $((SECONDS + 90)) kbd_details_open "$base"; then
			base=$(repaints)
			type_keys "/run/console-test/esc.events"
			if until_s $((SECONDS + 90)) kbd_dashboard_back "$base"; then
				result PASS keys "home and down selected probe-1; Enter showed its details page with project default; Esc returned to the instances"
			else
				result FAIL keys "the details page did not close within 90 s after Esc ($(why_screen))"
			fi
		else
			result FAIL keys "the details page with project default did not show within 90 s after Enter ($(why_screen))"
		fi
	else
		result FAIL keys "home and down never selected probe-1 on a repainted screen ($(why_screen))"
	fi

	# upstream-toggle: L shows upstream's screen, and L brings the console back.
	type_keys "/run/console-test/l.events"
	if until_s $((SECONDS + 60)) kbd_upstream_shown; then
		base=$(repaints)
		type_keys "/run/console-test/l.events"
		if until_s $((SECONDS + 90)) kbd_console_back "$base"; then
			result PASS upstream-toggle "L brought back upstream's footer, and L again removed it"
		else
			result FAIL upstream-toggle "the console did not come back within 90 s after the second L ($(why_screen))"
		fi
	else
		result FAIL upstream-toggle "upstream's footer did not appear within 60 s of the first L"
	fi
fi

# privacy: canaries and a trust token set inside the VM, searched inside it.
if ! kbd_ready; then
	result FAIL privacy "no virtual keyboard"
else
	# shellcheck disable=SC2016  # expanded by the shell in the VM
	vexec bash -uc 'd=/run/console-test
		c1=CANARY-$(od -An -N6 -tx1 /dev/urandom | tr -d " \n")
		c2=CANARY-$(od -An -N6 -tx1 /dev/urandom | tr -d " \n")
		tok=console-probe-$(od -An -N4 -tx1 /dev/urandom | tr -d " \n")
		incus config set probe-1 user.secret-probe "$c1" || exit 1
		incus config set probe-1 user.droid.owner "$c2" || exit 1
		printf "%s\n%s\n" "$c1" "$c2" > "$d/canaries.txt"
		t=$(incus config trust add "$tok" 2>/dev/null | head -n 1)
		[ -n "$t" ] || t=$(incus config trust add --name "$tok" 2>/dev/null | head -n 1)
		[ -z "$t" ] || printf "%s\n" "$t" > "$d/token.txt"
		[ -z "$t" ] || printf "%s\n" "$t" >> "$d/canaries.txt"' 2>/dev/null

	if select_probe; then
		base=$(repaints)
		type_keys "/run/console-test/enter.events"
		if until_s $((SECONDS + 90)) kbd_details_open "$base"; then
			incus file push -q -p "${P[@]}" "$tmp/screen.txt" "$vm/run/console-test/screen.txt" 2>/dev/null || true
			incus file push -q -p "${P[@]}" "$tmp/text.txt" "$vm/run/console-test/text.txt" 2>/dev/null || true
			# shellcheck disable=SC2016  # expanded by the shell in the VM
			scan=$(vexec bash -euc 'n=0; bad=0
				while IFS= read -r s; do
					[ -n "$s" ] || continue
					n=$((n + 1))
					if grep -qF -- "$s" /run/console-test/screen.txt /run/console-test/text.txt; then bad=$((bad + 1)); fi
				done < /run/console-test/canaries.txt
				echo "$n $bad"' 2>/dev/null || echo "0 0")
			n=${scan%% *}
			bad=${scan##* }
			if [ "$n" -ge 3 ] && [ "$bad" -eq 0 ]; then
				result PASS privacy "with the details page open, the screen and the capture hold none of the $n canaries or the trust token"
			elif [ "$n" -lt 3 ]; then
				result FAIL privacy "the canaries or the trust token could not be set inside the VM"
			else
				result FAIL privacy "$bad of the $n canaries or the trust token reached the screen or the capture"
			fi
			type_keys "/run/console-test/esc.events"; sleep 2
		else
			result FAIL privacy "the details page did not show, so the search proves nothing ($(why_screen))"
		fi
	else
		result FAIL privacy "home and down never selected probe-1, so the search proves nothing ($(why_screen))"
	fi
fi
privacy_cleanup || true

[ "$fails" -eq 0 ] || exit 1
exit 0
