#!/usr/bin/env bash
# The Wi-Fi cases of vm-test.sh --checks wifi, run on this host against a
# running test VM that has no NIC and has simulated radios (mac80211_hwsim, five
# of them: one is the station, four are access points).
#
#   run.sh --project arcaos-* --vm <name> --server-url http://10.42.0.1:<port>
#          --updates <dir> --debug-sysext <file> [--firmware <file>]
#
# Once incus-osd's first-boot gate holds, it merges the debug sysext (the test
# tools) from /run/extensions, pushes the update tree <dir>, and moves four radios
# into network namespace "ap", where four access points run, each on its own
# 10.42.<k>.0/24 with dnsmasq for DHCP and DNS:
#   Velho    k=1  WPA2-PSK, 2.4 GHz channel 1, HT20
#   Casa     k=2  WPA3-SAE, 5 GHz channel 36, 80 MHz with HE, PMF required
#   Oculta   k=3  hidden and open, 5 GHz channel 149, 80 MHz
#   Casa-6E  k=4  WPA3-SAE with H2E, 6 GHz channel 5, HE (best effort: when its
#                 hostapd cannot run, rank-6ghz is a SKIP with the reason)
# The 5 and 6 GHz access points and the station set country US (iw reg set US).
# The first radio stays in the root namespace as the station. The update tree is
# served from namespace ap at 10.42.0.1:<port>, with lighttpd on a dummy
# interface. Passphrases are generated in the VM into 0600 files and typed from
# there through /dev/uinput (evemu); the screen is read with
# `incus console --show-log`. No passphrase, 0600 file or GET body leaves the VM:
# they are compared there.
#
# It prints one "PASS|FAIL|SKIP <case> <detail>" line per case, in this order:
#   first-boot-gate  "Wi-Fi: picker shown" within 60 s of incus-osd's first log
#                    line, and no application listed while "Wi-Fi: gate waiting
#                    for a network" holds
#   rank             the picker's first two rows, read from the "Passphrase for
#                    <network>" label that Enter opens on each (Esc backs out,
#                    nothing joins): Casa or Casa-6E first, Casa in the first two,
#                    and "not recommended" on the screen (Velho, 2.4 GHz)
#   rank-6ghz        PASS when Casa-6E is first; SKIP with its access point's
#                    reason when the station hears nothing from it; FAIL when the
#                    station hears it but the picker does not list it first
#   sae              Casa joins with its passphrase: "Wi-Fi: gate released", then
#                    "Wi-Fi: picker closed", incus and arca listed within 10 min,
#                    the station (on 10.42.2.x) fetches both, arca.service is
#                    active, no application was pushed, neither the journal nor
#                    the console capture holds "already in progress", and
#                    arca-wifi-hwsim.service, active with its monitor device in
#                    namespace hwsim, starts again cleanly
#   secret           no passphrase, hex form or PSK of Velho, Casa or Casa-6E is
#                    in the journal, the console capture, /run/systemd/network/ or
#                    any /proc/*/cmdline (the checks run in the VM)
#   regdb            /usr/lib/firmware/regulatory.db links to regulatory.db-upstream,
#                    both copies equal their upstream files, the kernel log has no
#                    "failed to load regulatory.db", and iw reg get shows US
#   firmware         every "f <path>" of the build's Wi-Fi firmware list
#                    (--firmware: build.sh's wifi-firmware.txt) is a regular
#                    file under /usr/lib/firmware and every "l <path>" a link
#                    there that resolves, and the files' count and bytes equal
#                    its first line, "wifi-firmware: <files> files, <bytes>
#                    bytes raw"; the detail names the AX211 files present
#   debian-units     `systemctl is-enabled` prints "disabled" for
#                    wpa_supplicant.service, wpa_supplicant@.service,
#                    wpa_supplicant-nl80211@.service and
#                    wpa_supplicant-wired@.service, no link under
#                    /etc/systemd/system or /usr/lib/systemd/system points at a
#                    wpa_supplicant unit, D-Bus cannot activate
#                    fi.w1.wpa_supplicant1 (no activation file, not listed,
#                    StartServiceByName fails), wpa_supplicant.service is not
#                    active, and arca-wifi@<dev> is active for the device that holds
#                    the station's address
# On a FAIL it prints, to stderr, what tells a stalled download apart: a byte
# timeline of the download connection (every 5 s), the server's requests, the
# incus-osd journal around the longest stall, and the access points' and the
# station's connect and disconnect events. They are put together in the VM and
# withheld there when they hold a secret pattern.
# Progress goes to stderr. Exit codes: 0 all PASS (SKIP allowed), 1 a FAIL, 2 usage.
set -euo pipefail

OSD_SOCK=/run/incus-os/unix.socket
D=/run/arca-wifi-test
TREE=/root/wifi-tree

project="" vm="" server_url="" updates="" sysext="" firmware=""
usage() { awk 'NR > 1 && /^set -euo pipefail$/ { exit } NR > 1' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'wifi-test: %s\n' "$*" >&2; }
while [ $# -gt 0 ]; do
	case "$1" in
	--project) [ $# -ge 2 ] || usage; project=$2; shift 2 ;;
	--vm) [ $# -ge 2 ] || usage; vm=$2; shift 2 ;;
	--server-url) [ $# -ge 2 ] || usage; server_url=$2; shift 2 ;;
	--updates) [ $# -ge 2 ] || usage; updates=$2; shift 2 ;;
	--debug-sysext) [ $# -ge 2 ] || usage; sysext=$2; shift 2 ;;
	--firmware) [ $# -ge 2 ] || usage; firmware=$2; shift 2 ;;
	*) usage ;;
	esac
done
case "$project" in arcaos-*) ;; *) usage ;; esac
printf '%s' "$vm" | grep -Eqx '[a-z0-9][a-z0-9-]*' || usage
port=$(printf '%s' "$server_url" | sed -n 's|^http://10\.42\.0\.1:\([0-9]\{2,5\}\)$|\1|p')
[ -n "$port" ] || { log "--server-url must be http://10.42.0.1:<port>"; usage; }
[ -f "$updates/index.sjson" ] || { log "no index.sjson in --updates '$updates'"; usage; }
[ -f "$sysext" ] || { log "no --debug-sysext file '$sysext'"; usage; }

P=(--project "$project")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0
result() { # <PASS|FAIL|SKIP> <case> <detail>
	printf '%s %s %s\n' "$1" "$2" "$3"
	[ "$1" != FAIL ] || fails=$((fails + 1))
}
vexec() { incus exec "${P[@]}" "$vm" -- "$@"; }
vsh() { incus exec "${P[@]}" "$vm" -- sh -euc "$@"; }
# grep reads to the end: an early exit would fail the pipeline (pipefail) on SIGPIPE.
journal_has() { vexec journalctl -b0 _SYSTEMD_UNIT=incus-osd.service -o cat --no-pager 2>/dev/null | grep -F -- "$1" >/dev/null; }
console() { incus console "${P[@]}" "$vm" --show-log >"$tmp/console.txt" 2>/dev/null || true; }
# screen_text: the console capture with its terminal escape codes removed.
screen_text() { console; sed -E 's/\x1b\[[0-9;?]*[A-Za-z]//g' "$tmp/console.txt"; }
# wait_for <seconds> <command...>: 0 once the command succeeds.
wait_for() {
	local t0=$SECONDS limit=$1
	shift
	while [ $((SECONDS - t0)) -lt "$limit" ]; do
		if "$@"; then return 0; fi
		sleep 5
	done
	return 1
}
# apps_listed <jq condition on the names>: the condition holds on the VM's /1.0/applications.
apps_listed() {
	vexec curl -sf --max-time 30 --unix-socket "$OSD_SOCK" http://localhost/1.0/applications 2>/dev/null \
		| jq -e "[.metadata[]? | sub(\".*/\"; \"\")] | $1" >/dev/null
}
# type_keys <file in the VM>: plays a key sequence on a virtual keyboard there.
# shellcheck disable=SC2016  # expanded by the shell in the VM
type_keys() { vsh 'n=$(grep -l "^arca-test-keyboard$" /sys/class/input/event*/device/name | head -1); n=/dev/input/$(basename "$(dirname "$(dirname "$n")")"); evemu-play "$n" < "$1"' sh "$1"; }
# vm_fn <function> <args...>: runs a function of this script in the VM, with bash.
vm_fn() { local f=$1; shift; vexec bash -c "$(declare -f "$f"); $f \"\$@\"" "$f" "$@"; }

# ap_setup <dir> <tree> <port>, in the VM (bash): the access points (see the
# header), the update server in their namespace, the keyboard, the typed-key
# events and the secret patterns, all under <dir>. Radio 1 is the station and
# stays in the root namespace; radios 2 to 5 become the access points.
ap_setup() {
	local d=$1 tree=$2 port=$3 n k name w phy psk us=0
	local -a wifis=() ifs=() phys=()
	local -A ssids=([velho]=Velho [casa]=Casa [casa6e]=Casa-6E)
	install -d -m 0700 "$d"
	umask 077
	for n in /sys/class/net/*; do
		if [ -e "$n/phy80211" ]; then wifis+=("${n##*/}"); fi
	done
	[ "${#wifis[@]}" -eq 5 ] || { echo "want five simulated radios, have ${#wifis[@]}" >&2; return 1; }
	ip netns add ap || return 1
	ip -n ap link set lo up
	ip -n ap link add d0 type dummy
	ip -n ap addr add 10.42.0.1/24 dev d0
	ip -n ap link set d0 up
	ip netns exec ap sysctl -qw net.ipv4.ip_forward=1
	for k in 1 2 3 4; do
		ifs[k]=${wifis[k]}
		phys[k]=$(cat "/sys/class/net/${wifis[k]}/phy80211/name")
		iw phy "${phys[k]}" set netns name ap || return 1
	done
	iw reg set US || return 1
	for _ in $(seq 20); do iw reg get | grep -q '^country US:' && break; sleep 1; done
	iw reg get | grep -q '^country US:' || { echo "the regulatory domain is not US" >&2; return 1; }

	for name in velho casa casa6e; do
		tr -dc a-z0-9 < /dev/urandom | head -c 24 > "$d/$name.pass" || return 1
	done
	{ printf '%s\n' "interface=${ifs[1]}" driver=nl80211 ssid=Velho hw_mode=g channel=1 ieee80211n=1 \
		wpa=2 wpa_key_mgmt=WPA-PSK rsn_pairwise=CCMP
	  printf 'wpa_passphrase=%s\n' "$(cat "$d/velho.pass")"; } > "$d/velho.conf"
	{ printf '%s\n' "interface=${ifs[2]}" driver=nl80211 ssid=Casa hw_mode=a channel=36 country_code=US \
		ieee80211d=1 ieee80211n=1 'ht_capab=[HT40+]' ieee80211ac=1 vht_oper_chwidth=1 \
		vht_oper_centr_freq_seg0_idx=42 ieee80211ax=1 he_oper_chwidth=1 he_oper_centr_freq_seg0_idx=42 \
		wpa=2 wpa_key_mgmt=SAE rsn_pairwise=CCMP ieee80211w=2 sae_require_mfp=1 sae_pwe=2
	  printf 'wpa_passphrase=%s\n' "$(cat "$d/casa.pass")"; } > "$d/casa.conf"
	{ printf '%s\n' "interface=${ifs[3]}" driver=nl80211 ssid=Oculta ignore_broadcast_ssid=1 hw_mode=a \
		channel=149 country_code=US ieee80211d=1 ieee80211n=1 'ht_capab=[HT40+]' ieee80211ac=1 \
		vht_oper_chwidth=1 vht_oper_centr_freq_seg0_idx=155; } > "$d/oculta.conf"
	{ printf '%s\n' "interface=${ifs[4]}" driver=nl80211 ssid=Casa-6E hw_mode=a channel=5 op_class=131 \
		country_code=US ieee80211d=1 ieee80211ax=1 wpa=2 wpa_key_mgmt=SAE rsn_pairwise=CCMP ieee80211w=2 \
		sae_require_mfp=1 sae_pwe=1
	  printf 'wpa_passphrase=%s\n' "$(cat "$d/casa6e.pass")"; } > "$d/casa6e.conf"

	# ap_start <k> <name>: hostapd on radio k, the radio's fragmentation threshold,
	# its subnet and dnsmasq. hwsim takes CCMP keys as hardware keys and encrypts
	# in its own transmit, but mac80211's fast-xmit reserves no room for the CCMP
	# header and MIC, so a data frame without 8 spare bytes at either end is
	# dropped ("WARNING: net/mac80211/wpa.c" in the kernel log), on every
	# retransmission of it too, and a download stalls. hwsim has no fragmentation
	# of its own, so any threshold turns fast-xmit off; 2346 is above every frame
	# here, so nothing is fragmented. hostapd's fragm_threshold does not reach the
	# radio, so iw sets it, after hostapd, and it is checked.
	ap_start() {
		local k=$1 name=$2 w=${ifs[$1]} phy=${phys[$1]}
		ip netns exec ap hostapd -B -t -P "$d/$name.pid" -f "$d/$name.log" "$d/$name.conf" >/dev/null 2>&1 || return 1
		ip -n ap addr add "10.42.$k.1/24" dev "$w" || return 1
		ip netns exec ap iw phy "$phy" set frag 2346 || return 1
		ip netns exec ap iw phy "$phy" info | grep -q 'Fragmentation threshold: 2346' || return 1
		for _ in $(seq 20); do ip -n ap addr show dev "$w" | grep -q 'state UP' && break; sleep 1; done
		ip netns exec ap dnsmasq --conf-file=/dev/null --no-resolv --no-hosts --bind-interfaces --interface="$w" \
			--dhcp-range="10.42.$k.10,10.42.$k.200,255.255.255.0,1h" --dhcp-option=option:router,"10.42.$k.1" \
			--dhcp-option=option:dns-server,"10.42.$k.1" --dhcp-leasefile="$d/$name.leases" \
			--pid-file="$d/$name-dnsmasq.pid" --user=root --group=root --log-facility="$d/$name-dnsmasq.log" || return 1
	}
	ap_start 1 velho || { echo "the Velho access point did not start" >&2; return 1; }
	ap_start 2 casa || { echo "the Casa access point did not start" >&2; return 1; }
	ap_start 3 oculta || { echo "the Oculta access point did not start" >&2; return 1; }
	if ap_start 4 casa6e; then
		echo started > "$d/casa6e.status"
	else
		printf 'the 6 GHz access point did not start: %s\n' "$(tail -n 1 "$d/casa6e.log" 2>/dev/null)" > "$d/casa6e.status"
	fi

	cat > "$d/lighttpd.conf" <<EOF
server.document-root = "$tree"
server.bind = "10.42.0.1"
server.port = $port
server.modules = ("mod_accesslog")
accesslog.filename = "$d/access.log"
server.errorlog = "$d/lighttpd.log"
server.pid-file = "$d/lighttpd.pid"
mimetype.assign = ("" => "application/octet-stream")
EOF
	ip netns exec ap lighttpd -f "$d/lighttpd.conf" || return 1

	# A virtual keyboard: EV_SYN and EV_KEY, keys 1-127.
	printf '%s\n' "# EVEMU 1.3" "N: arca-test-keyboard" "I: 0006 0001 0001 0001" \
		"P: 00 00 00 00 00 00 00 00" "B: 00 03 00 00 00 00 00 00 00" \
		"B: 01 fe ff ff ff ff ff ff ff" "B: 01 ff ff ff ff ff ff ff ff" > "$d/kbd.desc"
	systemd-run -q --unit=arca-test-keyboard --collect evemu-device "$d/kbd.desc" || return 1
	# vkey: the event code of a key name; vevents: press and release of each key
	# in order, as evemu events with increasing times.
	vkey() { case $1 in
		[1-9]) echo $(($1 + 1)) ;; 0) echo 11 ;;
		q) echo 16 ;; w) echo 17 ;; e) echo 18 ;; r) echo 19 ;; t) echo 20 ;; y) echo 21 ;; u) echo 22 ;; i) echo 23 ;; o) echo 24 ;; p) echo 25 ;;
		a) echo 30 ;; s) echo 31 ;; d) echo 32 ;; f) echo 33 ;; g) echo 34 ;; h) echo 35 ;; j) echo 36 ;; k) echo 37 ;; l) echo 38 ;;
		z) echo 44 ;; x) echo 45 ;; c) echo 46 ;; v) echo 47 ;; b) echo 48 ;; n) echo 49 ;; m) echo 50 ;;
		enter) echo 28 ;; esc) echo 1 ;; home) echo 102 ;; down) echo 108 ;;
		esac; }
	vevents() {
		local k c v
		for k in "$@"; do c=$(vkey "$k"); for v in 1 0; do us=$((us + 40000))
			printf 'E: %d.%06d 0001 %04x %d\nE: %d.%06d 0000 0000 0\n' $((us / 1000000)) $((us % 1000000)) "$c" "$v" \
				$((us / 1000000)) $((us % 1000000)); done; done
	}
	vevents enter > "$d/enter.events"
	us=0; vevents esc > "$d/esc.events"
	us=0; vevents home > "$d/home.events"
	us=0; vevents down > "$d/down.events"
	us=0; vevents r > "$d/rescan.events"
	mapfile -t pw < <(fold -w1 "$d/casa.pass")
	us=0; vevents "${pw[@]}" enter > "$d/casa.events"

	# The secret patterns: each WPA network's passphrase, its hex form and its PSK
	# (the pbkdf2 of the passphrase and the SSID, from wpa_passphrase on stdin).
	: > "$d/secret.pats"
	for name in velho casa casa6e; do
		psk=$(wpa_passphrase "${ssids[$name]}" < "$d/$name.pass" | sed -n 's/^[[:space:]]*psk=\([0-9a-f]\{64\}\)$/\1/p')
		[ "${#psk}" -eq 64 ] || { echo "no PSK for ${ssids[$name]}" >&2; return 1; }
		{ cat "$d/$name.pass"; echo; od -An -tx1 "$d/$name.pass" | tr -d ' \n'; echo; echo "$psk"; } >> "$d/secret.pats"
	done
	[ "$(grep -c . "$d/secret.pats")" -eq 9 ] || return 1
	return 0
}

# secret_scan <dir> (in the VM): prints the name of each place that holds a
# pattern of <dir>/secret.pats, never the pattern. The journal capture must hold
# the picker's line, or the search means nothing.
secret_scan() {
	local d=$1 c j="$1/journal.txt"
	journalctl -b0 -o cat --no-pager > "$j" 2>/dev/null || true
	grep -qF 'Wi-Fi: picker shown' "$j" || echo "missing: the journal capture"
	[ -s "$d/console.txt" ] || echo "missing: the console capture"
	grep -qiFf "$d/secret.pats" "$j" && echo "leak: the journal"
	grep -qiFf "$d/secret.pats" "$d/console.txt" && echo "leak: the console capture"
	grep -rqiFf "$d/secret.pats" /run/systemd/network 2>/dev/null && echo "leak: /run/systemd/network"
	for c in /proc/[0-9]*/cmdline; do
		cat "$c" 2>/dev/null | tr '\0' ' ' > "$d/cmd.txt" || continue
		grep -qiFf "$d/secret.pats" "$d/cmd.txt" && echo "leak: $c"
	done
	rm -f "$j" "$d/cmd.txt"
	return 0
}

# sampler <port> <file> (in the VM): every 5 s, one line of numbers and test
# addresses: the server's open connections, then the download's alone (the
# connection that has acked the most bytes, named by the station's port: its
# acked bytes, send queue and retransmits, and the station side's unread bytes
# on it; the small HEAD checks run on other connections and never count),
# incus-osd's written bytes, io pressure, and the connected station's signal.
sampler() {
	local port=$1 out=$2 srv dl cli pid wb io sta n x
	while :; do
		# shellcheck disable=SC2016  # awk programs
		srv=$(ip netns exec ap ss -Htin "( sport = :$port )" 2>/dev/null | awk '
			function keep() { if (est && a >= ba) { ba = a; bp = p; bq = q; br = r } }
			/^[^ \t]/ { keep(); est = $1 == "ESTAB"; a = 0; r = 0; if (est) { n++; q = $3; p = $5; sub(".*:", "", p) } next }
			est { for (i = 1; i <= NF; i++) {
				if ($i ~ /^bytes_acked:/) { split($i, v, ":"); a = v[2] }
				if ($i ~ /^retrans:/) { split($i, v, "[:/]"); r = v[3] } } }
			END { keep(); printf "conns=%d dl=%s acked=%d sendq=%d retrans=%d", n, (n ? bp : "-"), ba, bq, br }')
		dl=$(printf '%s' "$srv" | sed -n 's/.* dl=\([0-9]*\) .*/\1/p')
		cli="recvq=0"
		# shellcheck disable=SC2016
		[ -z "$dl" ] || cli=$(ss -Htn "( sport = :$dl and dport = :$port )" 2>/dev/null | awk '/^ESTAB/ { q += $2 } END { printf "recvq=%d", q }')
		pid=$(systemctl show -p MainPID --value incus-osd.service)
		wb=$(awk '/^write_bytes/ { print $2 }' "/proc/$pid/io" 2>/dev/null)
		io=$(awk '/^some/ { sub("avg10=", "", $2); print $2 }' /proc/pressure/io 2>/dev/null)
		sta=""
		for n in /sys/class/net/*; do
			[ -e "$n/phy80211" ] || continue
			x=$(iw dev "${n##*/}" station dump 2>/dev/null | awk '$1 == "signal:" { print $2; exit }')
			[ -z "$x" ] || sta="${n##*/}:$x"
		done
		echo "$(date +%s) $srv $cli osd_write=${wb:-?} io_some10=${io:-?} sta=${sta:-none}"
		sleep 5
	done >>"$out"
}

# diagnostics <dir> (in the VM): the last incus-osd and supplicant lines and what
# classifies a stall (see the header), printed only when they hold no secret
# pattern.
diagnostics() {
	local d=$1 out=$1/diag.txt stall
	{
		echo "== last incus-osd and supplicant lines"
		journalctl -b0 -u incus-osd -u 'arca-wifi@*' -n 40 --no-pager -o cat 2>/dev/null || true
		if [ -s "$d/timeline.log" ]; then
			echo "== the download connection every 5 s, when it changes (VM time, seconds)"
			# shellcheck disable=SC2016  # awk programs
			awk '{ k = $2 " " $3 " " $4 " " $7 } NR == 1 || k != prev { print; p = NR } { prev = k; last = $0 } END { if (p != NR) print last }' "$d/timeline.log" | tail -n 150
			# The longest run of samples in which the same download connection has
			# unacked bytes and no new acked byte.
			# shellcheck disable=SC2016
			stall=$(awk '{ split($3, d, "="); split($4, a, "="); split($5, q, "=")
					stalled = NR > 1 && d[2] != "-" && d[2] == pd && a[2] == pa && q[2] > 0 }
				stalled { if (s == "") s = pt; if ($1 - s > best) { best = $1 - s; bs = s } }
				!stalled { s = "" }
				{ pa = a[2]; pd = d[2]; pt = $1 }
				END { if (best > 0) print bs, best }' "$d/timeline.log")
			if [ -n "$stall" ]; then
				# shellcheck disable=SC2086  # two numbers
				set -- $stall
				echo "== longest stall: $2 s without a new acked byte from $1; incus-osd from 60 s before to 420 s after"
				journalctl -b0 -u incus-osd --no-pager -o short-unix --since "@$(($1 - 60))" --until "@$(($1 + $2 + 420))" 2>/dev/null | tail -n 80 || true
			else
				echo "== no stall in the timeline"
			fi
		fi
		if [ -s "$d/access.log" ]; then
			echo "== server requests but the HEAD checks"
			grep -v '"HEAD ' "$d/access.log" | tail -n 40 || true
		fi
		cat "$d"/velho.log "$d"/casa.log "$d"/oculta.log "$d"/casa6e.log > "$d/ap.txt" 2>/dev/null || true
		if [ -s "$d/ap.txt" ]; then
			echo "== access point events"
			grep -E 'AP-STA-|IEEE 802\.11: (associated|disassociated|deauthenticated)|pairwise key handshake completed|inactivity|excessive' "$d/ap.txt" | tail -n 40 || true
		fi
		rm -f "$d/ap.txt"
		echo "== station events"
		journalctl -b0 -u 'arca-wifi@*' --no-pager -o short-unix 2>/dev/null \
			| grep -E 'CTRL-EVENT-(CONNECTED|DISCONNECTED|SSID-TEMP-DISABLED|BEACON-LOSS|ASSOC-REJECT)|Trying to associate|Associated with' | tail -n 40 || true
		echo "== kernel warnings in mac80211 and the simulated radios (count, place)"
		journalctl -b0 -k --no-pager -o cat 2>/dev/null | grep -oE 'WARNING: [^ ]*(mac80211|hwsim)[^ ]*' | sort | uniq -c || true
	} >"$out"
	if [ -s "$d/secret.pats" ] && grep -qiFf "$d/secret.pats" "$out"; then
		echo "wifi-test: the diagnostics hold a secret pattern; withheld"
	else
		sed 's/^/wifi-test: | /' "$out"
	fi
	rm -f "$out"
}
diagnose() { vm_fn diagnostics "$D" >&2 2>/dev/null || true; }

# --- first-boot-gate ---------------------------------------------------------
log "waiting for the first-boot gate"
if ! wait_for 600 journal_has 'Wi-Fi: gate waiting for a network'; then
	result FAIL first-boot-gate "no 'Wi-Fi: gate waiting for a network' after 600 s"
	diagnose
	exit 1
fi
wait_for 60 journal_has 'Wi-Fi: picker shown' || true
# shellcheck disable=SC2016  # an awk program
picker_s=$(vexec journalctl -b0 _SYSTEMD_UNIT=incus-osd.service -o short-unix --no-pager 2>/dev/null \
	| awk '$1 !~ /^[0-9]+\.[0-9]+$/ { next } t0 == "" { t0 = $1 } /Wi-Fi: picker shown/ { printf "%d", $1 - t0; exit }' || true)
empty=0
if apps_listed 'length == 0' && ! journal_has 'Wi-Fi: gate released'; then empty=1; fi
if [ -n "$picker_s" ] && [ "$picker_s" -le 60 ] && [ "$empty" -eq 1 ]; then
	result PASS first-boot-gate "picker shown ${picker_s} s after incus-osd's first line; no application while the gate holds"
else
	result FAIL first-boot-gate "picker shown after '${picker_s:-never}' s; no application while the gate holds: $([ "$empty" -eq 1 ] && echo yes || echo no)"
fi

# --- setup (no result line of its own; a failure is reported as "setup") -------
log "merging the debug sysext"
vexec mkdir -p /run/extensions
incus file push -q "${P[@]}" "$sysext" "$vm/run/extensions/debug.raw"
vexec systemd-sysext refresh >/dev/null
# shellcheck disable=SC2016  # expanded by the shell in the VM
if ! vsh 'for c in hostapd iw dnsmasq lighttpd evemu-device evemu-play; do command -v "$c" >/dev/null || { echo "no $c" >&2; exit 1; }; done'; then
	result FAIL setup "the debug sysext did not bring the test tools"
	exit 1
fi
# hwsim_ok: the radios' unit is active and their monitor device is in its namespace.
hwsim_ok() { vsh 'systemctl is-active -q arca-wifi-hwsim.service && ip -n hwsim -o link show | grep -q " link/ieee802.11/radiotap "'; }
if ! hwsim_ok; then
	result FAIL setup "arca-wifi-hwsim.service is not active with the monitor device in namespace hwsim"
	diagnose
	exit 1
fi
log "pushing the update tree and starting the access points"
vexec mkdir -p "$TREE"
incus file push -q -r "${P[@]}" "$updates" "$vm$TREE/"
if ! vm_fn ap_setup "$D" "$TREE/$(basename "$updates")" "$port"; then
	result FAIL setup "the access points, their servers or the virtual keyboard did not start"
	diagnose
	exit 1
fi
# shellcheck disable=SC2016  # expanded by the shell in the VM
if ! vsh 'for _ in $(seq 30); do grep -qx arca-test-keyboard /sys/class/input/event*/device/name 2>/dev/null && exit 0; sleep 1; done; exit 1'; then
	result FAIL setup "the virtual keyboard did not appear"
	exit 1
fi
# shellcheck disable=SC2016  # expanded by bash in the VM
vexec systemd-run -q --unit=arca-test-sampler --collect \
	bash -c "$(declare -f sampler); sampler \"\$@\"" sampler "$port" "$D/timeline.log"

# --- rank --------------------------------------------------------------------------
# The picker's first two rows, read from the "Passphrase for" label that Enter
# opens on each row. Esc backs out of that prompt, so nothing joins.
labels() {
	screen_text | grep -o 'Passphrase for [^:]*:' | sed -e 's/^Passphrase for //' -e 's/:$//' || true
}
# wait_label <previous>: the newest label, once it differs from <previous>.
wait_label() {
	local prev=$1 t0=$SECONDS cur
	while [ $((SECONDS - t0)) -lt 30 ]; do
		cur=$(labels | tail -n 1)
		if [ -n "$cur" ] && [ "$cur" != "$prev" ]; then printf '%s\n' "$cur"; return 0; fi
		sleep 2
	done
	return 1
}
detail=""
rank_top="" rank_second="" casa_at=""
if ! wait_for 180 sh -c 'incus console "$@" --show-log 2>/dev/null | grep -qF Velho' sh "${P[@]}" "$vm"; then
	detail="Velho never showed on the console"
fi
if [ -z "$detail" ]; then
	type_keys "$D/rescan.events"
	wait_for 60 sh -c 'incus console "$@" --show-log 2>/dev/null | grep -qF Casa' sh "${P[@]}" "$vm" \
		|| detail="Casa never showed on the console"
fi
if [ -z "$detail" ]; then
	type_keys "$D/home.events"; sleep 1
	type_keys "$D/enter.events"
	rank_top=$(wait_label "") || detail="Enter on the first row opened no 'Passphrase for' label"
fi
if [ -z "$detail" ]; then
	type_keys "$D/esc.events"; sleep 2
	type_keys "$D/down.events"; sleep 1
	type_keys "$D/enter.events"
	rank_second=$(wait_label "$rank_top") || detail="Enter on the second row opened no new 'Passphrase for' label"
	type_keys "$D/esc.events"; sleep 2
fi
if [ -z "$detail" ]; then
	case "$rank_top:$rank_second" in
	Casa:*) casa_at=0 ;;
	Casa-6E:Casa) casa_at=1 ;;
	*) detail="the first two rows are '$rank_top' and '$rank_second'; Casa must lead, or follow Casa-6E" ;;
	esac
fi
screen=$(screen_text)
if [ -z "$detail" ] && ! grep -qF 'not recommended' <<<"$screen"; then
	detail="the picker shows no 'not recommended' label (Velho, 2.4 GHz)"
fi
if [ -z "$detail" ]; then
	result PASS rank "the first two rows are $rank_top and $rank_second, Casa in the first two; the 2.4 GHz row carries 'not recommended'"
else
	result FAIL rank "$detail"
fi

# --- rank-6ghz ---------------------------------------------------------------------
if [ -z "$rank_top" ]; then
	result FAIL rank-6ghz "rank did not read the picker's first row"
elif [ "$rank_top" = Casa-6E ]; then
	result PASS rank-6ghz "Casa-6E (6 GHz, HE) is the first row, above Casa"
else
	# shellcheck disable=SC2016  # expanded by the shell in the VM
	sta=$(vsh 'for n in /sys/class/net/*; do if [ -e "$n/phy80211" ]; then basename "$n"; break; fi; done' || true)
	heard=$(vexec iw dev "$sta" scan dump 2>/dev/null | grep -c 'SSID: Casa-6E' || true)
	if [ "${heard:-0}" -gt 0 ]; then
		result FAIL rank-6ghz "the station hears Casa-6E, but the picker's first row is $rank_top"
	else
		why=$(vexec cat "$D/casa6e.status" 2>/dev/null | head -c 300 || true)
		result SKIP rank-6ghz "the station hears no Casa-6E beacon on 6 GHz (${why:-no status}); the simulated radios do not beacon there"
	fi
fi

# --- sae ---------------------------------------------------------------------------
# Casa joins with its passphrase: its row is selected (Home, then Down when
# Casa-6E leads), Enter opens its prompt, and the passphrase and Enter are typed.
detail=""
gate_s="" apps_s="" station=""
t0=$SECONDS
if [ -z "$casa_at" ]; then
	detail="rank did not place Casa in the first two rows"
else
	type_keys "$D/home.events"; sleep 1
	if [ "$casa_at" = 1 ]; then type_keys "$D/down.events"; sleep 1; fi
	type_keys "$D/enter.events"; sleep 3
	type_keys "$D/casa.events"
	t0=$SECONDS
	wait_for 300 journal_has 'Wi-Fi: gate released' || detail="no 'Wi-Fi: gate released' 300 s after the passphrase"
fi
if [ -z "$detail" ]; then
	gate_s=$((SECONDS - t0))
	wait_for 60 journal_has 'Wi-Fi: picker closed' || detail="no 'Wi-Fi: picker closed' 60 s after 'Wi-Fi: gate released'"
fi
if [ -z "$detail" ]; then
	wait_for 600 apps_listed 'index("incus") != null and index("arca") != null' || detail="incus and arca not both listed after 600 s"
fi
if [ -z "$detail" ]; then
	apps_s=$((SECONDS - t0))
	wait_for 600 sh -c 'incus exec "$@" -- systemctl is-active -q arca.service' sh "${P[@]}" "$vm" || detail="arca.service not active after 600 s"
fi
if [ -z "$detail" ]; then
	# shellcheck disable=SC2016  # expanded by the shell in the VM
	station=$(vsh 's=""; for a in incus arca; do x=$(grep -E "^10\.42\.2\.[0-9]+ .*\"GET [^\"]*/$a\.raw\.gz HTTP/[0-9.]+\" 200 " "$1/access.log" | cut -d" " -f1 | head -1); [ -n "$x" ] || exit 1; s=${s:-$x}; done; echo "$s"' sh "$D" || true)
	[ -n "$station" ] || detail="the server log has no 10.42.2.x GET of incus.raw.gz and arca.raw.gz answered with 200"
fi
if [ -z "$detail" ] && vexec test -e /root/updates; then detail="applications were pushed into /root/updates"; fi
console
if [ -z "$detail" ] && { journal_has 'already in progress' || grep -qF 'already in progress' "$tmp/console.txt"; }; then
	detail="'already in progress' in the journal or on the console"
fi
if [ -z "$detail" ] && ! { vexec systemctl restart arca-wifi-hwsim.service && hwsim_ok; }; then
	detail="arca-wifi-hwsim.service did not start again cleanly"
fi
# shellcheck disable=SC2016  # expanded by the shell in the VM
vsh 'rm -f "$1"/*.events' sh "$D" || true
vexec systemctl stop arca-test-sampler.service 2>/dev/null || true
# Reported, not judged: dropped encrypted frames (see the fragmentation threshold).
ccmp=$(vexec journalctl -b0 -k --no-pager -o cat 2>/dev/null | grep -c 'WARNING: net/mac80211/wpa\.c' || true)
if [ -z "$detail" ]; then
	result PASS sae "Casa joined; gate released ${gate_s} s and incus,arca listed ${apps_s} s after the passphrase; picker closed; station $station fetched both; arca.service active; nothing pushed; no 'already in progress'; hwsim unit restarts cleanly; ${ccmp:-?} CCMP drop warnings in the kernel log"
else
	result FAIL sae "$detail"
	diagnose
fi

# --- secret ------------------------------------------------------------------------
# The search runs in the VM against the patterns there; the console capture is
# pushed in, and only the names of the places that hold a pattern come back.
console
incus file push -q "${P[@]}" "$tmp/console.txt" "$vm$D/console.txt" || true
leaks=$(vm_fn secret_scan "$D" 2>/dev/null || true)
if [ -z "$leaks" ]; then
	result PASS secret "no passphrase, hex form or PSK of Velho, Casa or Casa-6E in the journal, the console capture, /run/systemd/network or any /proc/*/cmdline"
else
	result FAIL secret "$(printf '%s' "$leaks" | tr '\n' ' ')"
fi

# --- regdb -------------------------------------------------------------------------
detail=""
# shellcheck disable=SC2016  # expanded by the shell in the VM
if ! vsh 'f=/usr/lib/firmware; cmp -s "$f/regulatory.db" "$f/regulatory.db-upstream" && cmp -s "$f/regulatory.db.p7s" "$f/regulatory.db.p7s-upstream" && [ "$(readlink "$f/regulatory.db")" = regulatory.db-upstream ]'; then
	detail="/usr/lib/firmware/regulatory.db is not a link to the upstream copy"
fi
if [ -z "$detail" ]; then
	kl=$(vexec journalctl -b0 -k --no-pager -o cat 2>/dev/null || true)
	if grep -qF 'failed to load regulatory.db' <<<"$kl"; then detail="the kernel log has 'failed to load regulatory.db'"; fi
fi
if [ -z "$detail" ]; then
	reg=$(vexec iw reg get 2>/dev/null || true)
	grep -q '^country US:' <<<"$reg" || detail="iw reg get does not show US"
fi
if [ -z "$detail" ]; then
	result PASS regdb "regulatory.db and regulatory.db.p7s link to their upstream copies, which equal the signed files; no 'failed to load regulatory.db'; iw reg get shows US"
else
	result FAIL regdb "$detail"
fi

# --- firmware ------------------------------------------------------------------------
want=$(head -n 1 "$firmware" 2>/dev/null | sed -n 's/^wifi-firmware: \([0-9]*\) files, \([0-9]*\) bytes raw$/\1 \2/p' || true)
if [ -z "$want" ]; then
	result FAIL firmware "no Wi-Fi firmware list from the build (--firmware '$firmware')"
else
	read -r want_n want_b <<<"$want"
	want_l=$(grep -c '^l ' "$firmware" || true)
	vexec mkdir -p "$D"
	incus file push -q "${P[@]}" "$firmware" "$vm$D/firmware.txt"
	# Prints "<files> <bytes> <links> <bad> <first bad path>", then the AX211 files.
	# shellcheck disable=SC2016  # expanded by the shell in the VM
	got=$(vsh 'fw=/usr/lib/firmware n=0 b=0 l=0 bad=0 first=""
		while IFS= read -r line; do
			case $line in
			"f "*) p=${line#f }
				if [ -f "$fw/$p" ] && [ ! -L "$fw/$p" ]; then n=$((n + 1)); b=$((b + $(stat -c %s "$fw/$p")))
				else bad=$((bad + 1)); first=${first:-$p}; fi ;;
			"l "*) p=${line#l }
				if [ -L "$fw/$p" ] && [ -e "$fw/$p" ]; then l=$((l + 1)); else bad=$((bad + 1)); first=${first:-$p}; fi ;;
			esac
		done < "$1"
		echo "$n $b $l $bad ${first:--}"
		{ cd "$fw" && ls intel/iwlwifi/iwlwifi-so-a0-gf-a0-*.ucode intel/iwlwifi/iwlwifi-so-a0-gf-a0.pnvm 2>/dev/null || true; } \
			| sed "s#.*/##" | tr "\n" " "' sh "$D/firmware.txt" || true)
	read -r got_n got_b got_l bad first <<<"$(head -n 1 <<<"$got")"
	ax=$(sed -n 2p <<<"$got")
	if [ "${bad:-1}" != 0 ]; then
		result FAIL firmware "${bad:-?} listed entries missing or not resolving in the image, first: ${first:-?}"
	elif [ "$got_n $got_b $got_l" != "$want_n $want_b $want_l" ]; then
		result FAIL firmware "the image has $got_n files, $got_b bytes, $got_l links; the build listed $want_n, $want_b, $want_l"
	else
		result PASS firmware "$got_n files, $got_b bytes raw and $got_l links in /usr/lib/firmware, as the build listed; AX211: ${ax:-none}"
	fi
fi

# --- debian-units --------------------------------------------------------------------
detail=""
for u in wpa_supplicant.service wpa_supplicant@.service wpa_supplicant-nl80211@.service wpa_supplicant-wired@.service; do
	s=$(vexec systemctl is-enabled "$u" 2>/dev/null || true)
	[ "$s" = disabled ] || { detail="systemctl is-enabled $u prints '${s:-nothing}'"; break; }
done
if [ -z "$detail" ]; then
	links=$(vexec find /etc/systemd/system /usr/lib/systemd/system -type l -lname '*wpa_supplicant*' 2>/dev/null | tr '\n' ' ' || true)
	[ -z "$links" ] || detail="links to a Debian supplicant unit: $links"
fi
if [ -z "$detail" ]; then
	# D-Bus cannot start it by name: no activation file names it, the bus
	# does not list it as activatable, and asking the bus to start it fails.
	act=$(vsh 'grep -rl wpa_supplicant /usr/share/dbus-1/system-services /etc/dbus-1/system-services 2>/dev/null || true' | tr '\n' ' ' || true)
	names=$(vexec busctl --system call org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus ListActivatableNames 2>/dev/null || true)
	if [ -n "$act" ]; then detail="D-Bus activation files name the supplicant: $act"
	elif [ "${names#as }" = "$names" ]; then detail="the bus did not list its activatable names"
	elif [ "${names#*\"fi.w1.wpa_supplicant1\"}" != "$names" ]; then detail="the bus can activate fi.w1.wpa_supplicant1"
	elif vexec busctl --system call org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus StartServiceByName su fi.w1.wpa_supplicant1 0 >/dev/null 2>&1; then
		detail="the bus started fi.w1.wpa_supplicant1"
	fi
fi
if [ -z "$detail" ]; then
	s=$(vexec systemctl is-active wpa_supplicant.service 2>/dev/null || true)
	[ "$s" != active ] || detail="wpa_supplicant.service is active"
fi
dev=""
if [ -z "$detail" ]; then
	# shellcheck disable=SC2016  # an awk program
	[ -z "${station:-}" ] || dev=$(vexec ip -o -4 addr show 2>/dev/null \
		| awk -v a="$station" '{ split($4, x, "/"); if (x[1] == a) { print $2; exit } }' || true)
	if [ -z "$dev" ]; then detail="no device holds the station's address '${station:-none}'"
	elif ! vexec systemctl is-active -q "arca-wifi@$dev.service"; then detail="arca-wifi@$dev.service is not active"; fi
fi
if [ -z "$detail" ]; then
	result PASS debian-units "the four wpa_supplicant units disabled, no link to one, no D-Bus activation, wpa_supplicant.service not active; arca-wifi@$dev.service active"
else
	result FAIL debian-units "$detail"
fi

[ "$fails" -eq 0 ]
