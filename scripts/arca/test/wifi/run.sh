#!/usr/bin/env bash
# The Wi-Fi cases of vm-test.sh --checks wifi, run on this host against a
# running test VM that has no NIC and has simulated radios (mac80211_hwsim).
#
#   run.sh --project arcaos-* --vm <name> --server-url http://10.42.0.1:<port>
#          --updates <dir> --debug-sysext <file>
#
# Once incus-osd's first-boot gate holds, it merges the debug sysext (the test
# tools) from /run/extensions, pushes the update tree <dir>, moves the last radio
# into network namespace "ap", serves the tree there with lighttpd on a dummy
# interface at 10.42.0.1:<port>, and starts the access point Velho (WPA2-PSK,
# 2.4 GHz channel 1, HT20) on 10.42.1.0/24 with dnsmasq for DHCP and DNS. The
# other radios stay in the root namespace as stations. Velho's passphrase is
# generated in the VM into a 0600 file and typed from there through /dev/uinput
# (evemu); the screen is read with `incus console --show-log`. No passphrase,
# 0600 file or GET body leaves the VM: they are compared there.
#
# It prints one "PASS|FAIL|SKIP <case> <detail>" line per case, in this order:
#   first-boot-gate  "Wi-Fi: picker shown" within 60 s of incus-osd's first log
#                    line, and no application listed while "Wi-Fi: gate waiting
#                    for a network" holds
#   join             typing Velho's passphrase joins it, "Wi-Fi: gate released"
#                    follows, incus and arca are listed within 10 min, the server
#                    log shows a 10.42.1.x station fetching both, arca.service is
#                    active, no application was pushed, and neither the journal
#                    nor the console capture holds "already in progress"
# Progress goes to stderr. Exit codes: 0 all PASS, 1 a FAIL, 2 usage.
set -euo pipefail

OSD_SOCK=/run/incus-os/unix.socket
D=/run/arca-wifi-test
TREE=/root/wifi-tree

project="" vm="" server_url="" updates="" sysext=""
usage() { sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'wifi-test: %s\n' "$*" >&2; }
while [ $# -gt 0 ]; do
	case "$1" in
	--project) [ $# -ge 2 ] || usage; project=$2; shift 2 ;;
	--vm) [ $# -ge 2 ] || usage; vm=$2; shift 2 ;;
	--server-url) [ $# -ge 2 ] || usage; server_url=$2; shift 2 ;;
	--updates) [ $# -ge 2 ] || usage; updates=$2; shift 2 ;;
	--debug-sysext) [ $# -ge 2 ] || usage; sysext=$2; shift 2 ;;
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
diagnose() {
	log "last incus-osd and supplicant lines:"
	vexec journalctl -b0 -u incus-osd -u 'arca-wifi@*' -n 40 --no-pager -o cat >&2 2>/dev/null || true
}

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

# --- join ----------------------------------------------------------------------
log "merging the debug sysext"
vexec mkdir -p /run/extensions
incus file push -q "${P[@]}" "$sysext" "$vm/run/extensions/debug.raw"
vexec systemd-sysext refresh >/dev/null
# shellcheck disable=SC2016  # expanded by the shell in the VM
if ! vsh 'for c in hostapd iw dnsmasq lighttpd evemu-device evemu-play; do command -v "$c" >/dev/null || { echo "no $c" >&2; exit 1; }; done'; then
	result FAIL join "the debug sysext did not bring the test tools"
	exit 1
fi
log "pushing the update tree and starting Velho"
vexec mkdir -p "$TREE"
incus file push -q -r "${P[@]}" "$updates" "$vm$TREE/"
# shellcheck disable=SC2016  # expanded by the shell in the VM
if ! vsh '
d=$1 tree=$2 port=$3
install -d -m 0700 "$d"
umask 077
last=""
for n in /sys/class/net/*; do if [ -e "$n/phy80211" ]; then last=$n; fi; done
[ -n "$last" ] || { echo "no Wi-Fi device" >&2; exit 1; }
ip netns add ap
iw phy "$(cat "$last/phy80211/name")" set netns name ap
apif=$(ip netns exec ap iw dev | awk "\$1 == \"Interface\" { print \$2; exit }")
ip -n ap link set lo up
ip -n ap link add d0 type dummy
ip -n ap addr add 10.42.0.1/24 dev d0
ip -n ap link set d0 up
ip -n ap addr add 10.42.1.1/24 dev "$apif"
ip netns exec ap sysctl -qw net.ipv4.ip_forward=1
tr -dc a-z0-9 < /dev/urandom | head -c 24 > "$d/velho.pass"
{ printf "interface=%s\ndriver=nl80211\nssid=Velho\nhw_mode=g\nchannel=1\nieee80211n=1\n" "$apif"
  printf "wpa=2\nwpa_key_mgmt=WPA-PSK\nrsn_pairwise=CCMP\nwpa_passphrase="; cat "$d/velho.pass"; echo; } > "$d/velho.conf"
ip netns exec ap hostapd -B -P "$d/hostapd.pid" -f "$d/hostapd.log" "$d/velho.conf" >/dev/null
for _ in $(seq 20); do ip -n ap addr show dev "$apif" | grep -q "state UP" && break; sleep 1; done
ip netns exec ap dnsmasq --conf-file=/dev/null --no-resolv --no-hosts --bind-interfaces --interface="$apif" \
	--dhcp-range=10.42.1.10,10.42.1.200,255.255.255.0,1h --dhcp-option=option:router,10.42.1.1 \
	--dhcp-option=option:dns-server,10.42.1.1 --dhcp-leasefile="$d/dnsmasq.leases" --pid-file="$d/dnsmasq.pid" \
	--user=root --group=root --log-facility="$d/dnsmasq.log"
cat > "$d/lighttpd.conf" <<EOF
server.document-root = "$tree"
server.bind = "10.42.0.1"
server.port = $port
server.modules = ("mod_accesslog")
accesslog.filename = "$d/access.log"
server.errorlog = "$d/lighttpd.log"
server.pid-file = "$d/lighttpd.pid"
# A client that stops reading for a while (a slow disk under load) keeps its download.
server.max-write-idle = 3600
mimetype.assign = ("" => "application/octet-stream")
EOF
ip netns exec ap lighttpd -f "$d/lighttpd.conf"
# A virtual keyboard: EV_SYN and EV_KEY, keys 1-127.
printf "# EVEMU 1.3\nN: arca-test-keyboard\nI: 0006 0001 0001 0001\nP: 00 00 00 00 00 00 00 00\nB: 00 03 00 00 00 00 00 00 00\nB: 01 fe ff ff ff ff ff ff ff\nB: 01 ff ff ff ff ff ff ff ff\n" > "$d/kbd.desc"
systemd-run -q --unit=arca-test-keyboard --collect evemu-device "$d/kbd.desc"
# Enter alone, then the passphrase and Enter, as evemu events (0600, from the 0600 file).
key() { case $1 in
	[1-9]) echo $(($1 + 1)) ;; 0) echo 11 ;;
	q) echo 16 ;; w) echo 17 ;; e) echo 18 ;; r) echo 19 ;; t) echo 20 ;; y) echo 21 ;; u) echo 22 ;; i) echo 23 ;; o) echo 24 ;; p) echo 25 ;;
	a) echo 30 ;; s) echo 31 ;; d) echo 32 ;; f) echo 33 ;; g) echo 34 ;; h) echo 35 ;; j) echo 36 ;; k) echo 37 ;; l) echo 38 ;;
	z) echo 44 ;; x) echo 45 ;; c) echo 46 ;; v) echo 47 ;; b) echo 48 ;; n) echo 49 ;; m) echo 50 ;; enter) echo 28 ;;
	esac; }
us=0
events() { for k in "$@"; do c=$(key "$k"); for v in 1 0; do us=$((us + 40000))
	printf "E: %d.%06d 0001 %04x %d\nE: %d.%06d 0000 0000 0\n" $((us / 1000000)) $((us % 1000000)) "$c" "$v" $((us / 1000000)) $((us % 1000000)); done; done; }
events enter > "$d/enter.events"
us=0
events $(fold -w1 "$d/velho.pass") enter > "$d/velho.events"
' sh "$D" "$TREE/$(basename "$updates")" "$port"; then
	result FAIL join "the access point, its servers or the virtual keyboard did not start"
	diagnose
	exit 1
fi
# shellcheck disable=SC2016  # expanded by the shell in the VM
if ! vsh 'for _ in $(seq 30); do grep -qx arca-test-keyboard /sys/class/input/event*/device/name 2>/dev/null && exit 0; sleep 1; done; exit 1'; then
	result FAIL join "the virtual keyboard did not appear"
	exit 1
fi

detail=""
if ! wait_for 180 sh -c 'incus console "$@" --show-log 2>/dev/null | grep -qF Velho' sh "${P[@]}" "$vm"; then
	detail="Velho never showed on the console"
fi
if [ -z "$detail" ]; then
	log "Velho is listed; typing its passphrase"
	sleep 5
	type_keys "$D/enter.events"
	sleep 3
	type_keys "$D/velho.events"
	t0=$SECONDS
	wait_for 300 journal_has 'Wi-Fi: gate released' || detail="no 'Wi-Fi: gate released' 300 s after the passphrase"
fi
if [ -z "$detail" ]; then
	gate_s=$((SECONDS - t0))
	wait_for 600 apps_listed 'index("incus") != null and index("arca") != null' || detail="incus and arca not both listed after 600 s"
fi
if [ -z "$detail" ]; then
	apps_s=$((SECONDS - t0))
	wait_for 600 sh -c 'incus exec "$@" -- systemctl is-active -q arca.service' sh "${P[@]}" "$vm" || detail="arca.service not active after 600 s"
fi
if [ -z "$detail" ]; then
	# shellcheck disable=SC2016  # expanded by the shell in the VM
	station=$(vsh 's=""; for a in incus arca; do x=$(grep -E "^10\.42\.1\.[0-9]+ .*\"GET [^\"]*/$a\.raw\.gz HTTP/[0-9.]+\" 200 " "$1/access.log" | cut -d" " -f1 | head -1); [ -n "$x" ] || exit 1; s=${s:-$x}; done; echo "$s"' sh "$D" || true)
	[ -n "$station" ] || detail="the server log has no 10.42.1.x GET of incus.raw.gz and arca.raw.gz answered with 200"
fi
if [ -z "$detail" ] && vexec test -e /root/updates; then detail="applications were pushed into /root/updates"; fi
console
if [ -z "$detail" ] && { journal_has 'already in progress' || grep -qF 'already in progress' "$tmp/console.txt"; }; then
	detail="'already in progress' in the journal or on the console"
fi
vexec rm -f "$D/velho.events" "$D/enter.events"
if [ -z "$detail" ]; then
	result PASS join "Velho joined; gate released ${gate_s} s and incus,arca listed ${apps_s} s after the passphrase; station $station fetched both; arca.service active; nothing pushed; no 'already in progress'"
else
	result FAIL join "$detail"
	diagnose
fi

[ "$fails" -eq 0 ]
