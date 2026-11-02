#!/bin/sh
# Base image build script: the Wi-Fi drivers' firmware.
#
# It installs every File:, RawFile: and Link: of the WHENCE sections that
# arca-wifi.drivers lists, from the linux-firmware checkout the base image
# already uses for its NIC firmware, into /usr/lib/firmware; an iwlwifi .ucode
# only inside its API window (see arca-wifi.drivers), and a link only when its
# target is installed. Before it installs anything, it refuses a list with no
# driver line, a File:, RawFile: or Link: name that is absolute or has a ".."
# component, and a link whose target leaves the firmware tree. Then it fails
# the build when
#   1. a file of a listed section (after the windows) is not in the image, or
#      no file is installed at all;
#   2. an iwlwifi .ucode of a listed section has a prefix with no window row;
#   3. a window's maximum is not the API a wireless module declares for that
#      prefix (modinfo -F firmware), a declared prefix has no row, or iwlwifi
#      is listed but no module declares an iwlwifi .ucode;
#   4. a name a wireless module declares is in the checkout (as a file or a
#      WHENCE link) but was not installed.
# A modinfo that fails stops it too.
# Wireless modules are those under drivers/net/wireless and those in staging
# that depend on cfg80211 or mac80211. It writes the installed list, headed by the
# line "wifi-firmware: <files> files, <bytes> bytes raw" that it also prints,
# to /usr/share/arca-wifi/firmware.txt.
#
# The regulatory database comes from wireless-regdb: it refuses a build without
# the upstream (signed) regulatory.db-upstream and regulatory.db.p7s-upstream,
# installs copies of both into /usr/lib/firmware, and links regulatory.db and
# regulatory.db.p7s to them.
#
# The environment can point it elsewhere (the fixture test does):
# ARCA_WIFI_FW_SRC (the checkout), DESTDIR (the tree it installs into),
# ARCA_WIFI_MODULES (the kernel's module directory), ARCA_WIFI_DRIVERS
# (the list), ARCA_WIFI_MODINFO (the modinfo command) and ARCA_WIFI_REGDB (the
# wireless-regdb directory, /buildroot/usr/lib/firmware by default).
set -eu

die() { echo "arca-wifi: $*" >&2; exit 1; }
PATH=$PATH:/usr/sbin:/sbin
src=${ARCA_WIFI_FW_SRC:-/work/src/app-build/linux-firmware}
list=${ARCA_WIFI_DRIVERS:-${SRCDIR:-/work/src}/mkosi.images/base/mkosi.conf.d/arca-wifi.drivers}
modinfo=${ARCA_WIFI_MODINFO:-modinfo}
moddir=${ARCA_WIFI_MODULES:-}
if [ -z "$moddir" ]; then
	set -- /buildroot/usr/lib/modules/*
	if [ $# -ne 1 ] || [ ! -d "$1" ]; then echo "arca-wifi: want one kernel in /buildroot/usr/lib/modules" >&2; exit 1; fi
	moddir=$1
fi
dest=${DESTDIR:?}/usr/lib/firmware
out=$DESTDIR/usr/share/arca-wifi/firmware.txt
[ -f "$src/WHENCE" ] || die "no WHENCE in $src"
[ -f "$list" ] || die "no list $list"
regdb=${ARCA_WIFI_REGDB:-/buildroot/usr/lib/firmware}
for f in regulatory.db-upstream regulatory.db.p7s-upstream; do
	[ -f "$regdb/$f" ] || die "no wireless-regdb file $f in $regdb"
done
[ -d "$moddir/kernel" ] || die "no kernel/ in $moddir"
grep -Eq '^driver[[:space:]]+[^[:space:]]' "$list" || die "no driver line in $list"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
tab=$(printf '\t')
grep '^iwlwifi ' "$list" > "$tmp/windows" || true

# The windows as awk code: window(name) prints "<min> <max>" for an iwlwifi
# .ucode prefix, or nothing.
# shellcheck disable=SC2016  # awk programs
WINDOWS='
function glob2re(g) { gsub(/\./, "\\.", g); gsub(/\?/, ".", g); gsub(/\*/, ".*", g); return "^" g "$" }
function loadwin(f,   l, w) { while ((getline l < f) > 0) { split(l, w, " "); nwin++; wre[nwin] = glob2re(w[2]); wlo[nwin] = w[3]; whi[nwin] = w[4] } }
function window(pre,   i) { for (i = 1; i <= nwin; i++) if (pre ~ wre[i]) return wlo[i] " " whi[i]; return "" }
function apinum(a) { return a ~ /^c/ ? 1000 + substr(a, 2) : a + 0 }
'

# The plan: F <file>, D <file dropped by its window>, L <link> <target>,
# S <link to a dropped file>, E <error>, U <unsafe path>; and every WHENCE link
# name, for rule 4.
# shellcheck disable=SC2016
awk -v windows="$tmp/windows" -v list="$list" -v links="$tmp/whence-links" "$WINDOWS"'
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
function unq(s) { s = trim(s); gsub(/"/, "", s); return s }
# unsafe(p): p is absolute or has a ".." component.
function unsafe(p) { return p ~ /^\// || p ~ /(^|\/)\.\.(\/|$)/ }
# norm(p): p without "." and ".." components; esc is set when a ".." would
# leave the tree.
function norm(p,   n, a, i, o, k) {
	n = split(p, a, "/"); k = 0; esc = 0
	for (i = 1; i <= n; i++) {
		if (a[i] == "" || a[i] == ".") continue
		if (a[i] == "..") { if (k > 0) k--; else esc = 1; continue }
		o[++k] = a[i]
	}
	p = ""; for (i = 1; i <= k; i++) p = p (i > 1 ? "/" : "") o[i]
	return p
}
BEGIN {
	loadwin(windows)
	while ((getline l < list) > 0) if (l ~ /^driver[ \t]/) { sub(/^driver[ \t]+/, "", l); want[trim(l)] = 1 }
}
/^Driver:/ { d = $0; sub(/^Driver:/, "", d); cur = trim(d); on = cur in want; if (on) seen[cur] = 1; next }
/^Link:/ {
	l = $0; sub(/^Link:/, "", l); i = index(l, "->"); if (!i) next
	name = unq(substr(l, 1, i - 1)); tgt = unq(substr(l, i + 2))
	print name > links
	if (on) {
		if (unsafe(name)) { print "U\tLink: name " name; next }
		r = norm((name ~ /\// ? name "/../" : "") tgt)
		if (tgt ~ /^\// || esc) { print "U\tLink: " name " -> " tgt " leaves the firmware tree"; next }
		nl++; ln[nl] = name; lt[nl] = tgt; lr[nl] = r
	}
	next
}
on && /^(File|RawFile):/ {
	f = $0; sub(/^(RawFile|File):/, "", f); f = unq(f); b = f; sub(/.*\//, "", b)
	if (unsafe(f)) { print "U\tFile: " f; next }
	if (b ~ /^iwlwifi-.*-c?[0-9]+\.ucode$/) {
		pre = b; sub(/-c?[0-9]+\.ucode$/, "", pre); api = b; sub(/\.ucode$/, "", api); sub(/.*-/, "", api)
		w = window(pre)
		if (w == "") { print "E\t2. no iwlwifi window row for " f; next }
		split(w, ww, " ")
		if (apinum(api) < apinum(ww[1]) || apinum(api) > apinum(ww[2])) { drop[f] = 1; print "D\t" f; next }
	}
	if (!(f in file)) { file[f] = 1; print "F\t" f }
	next
}
END {
	for (d in want) if (!(d in seen)) print "E\t1. no WHENCE section \"" d "\""
	# Links to links: repeat until nothing changes.
	do {
		more = 0
		for (i = 1; i <= nl; i++) {
			if (done[i]) continue
			if (lr[i] in file || lr[i] in made) { made[ln[i]] = 1; done[i] = 1; more = 1; print "L\t" ln[i] "\t" lt[i] }
			else if (lr[i] in drop || lr[i] in skip) { skip[ln[i]] = 1; done[i] = 1; more = 1; print "S\t" ln[i] }
		}
	} while (more)
	for (i = 1; i <= nl; i++) if (!done[i]) print "E\t1. link " ln[i] " -> " lt[i] " has no target in its sections"
}
' "$src/WHENCE" > "$tmp/plan"
if grep -q "^U$tab" "$tmp/plan"; then
	grep "^U$tab" "$tmp/plan" | cut -f2- | sed 's/^/arca-wifi: unsafe path: /' >&2
	exit 1
fi

# Install.
while IFS="$tab" read -r kind a b; do
	case "$kind" in
	F)
		if [ -f "$src/$a" ]; then
			mkdir -p "$dest/$(dirname "$a")"
			cp -L "$src/$a" "$dest/$a"
			chmod 0644 "$dest/$a"
		fi ;;
	L)
		mkdir -p "$dest/$(dirname "$a")"
		ln -sfn "$b" "$dest/$a" ;;
	esac
done < "$tmp/plan"

# The regulatory database: the upstream copies, and the names the kernel reads
# linked to them.
mkdir -p "$dest"
for f in regulatory.db-upstream regulatory.db.p7s-upstream; do
	cp -L "$regdb/$f" "$dest/$f"
	chmod 0644 "$dest/$f"
done
ln -sfn regulatory.db-upstream "$dest/regulatory.db"
ln -sfn regulatory.db.p7s-upstream "$dest/regulatory.db.p7s"

# Rule 1, and the list.
grep "^E$tab" "$tmp/plan" | cut -f2- > "$tmp/errors" || true
files=0 bytes=0
: > "$tmp/list"
while IFS="$tab" read -r kind a b; do
	case "$kind" in
	F)
		if [ -f "$dest/$a" ] && [ ! -L "$dest/$a" ]; then
			files=$((files + 1)); bytes=$((bytes + $(wc -c < "$dest/$a")))
			printf 'f %s\n' "$a" >> "$tmp/list"
		else
			echo "1. missing $a" >> "$tmp/errors"
		fi ;;
	L)
		if [ -L "$dest/$a" ] && [ -e "$dest/$a" ]; then printf 'l %s\n' "$a" >> "$tmp/list"
		else echo "1. link $a does not resolve" >> "$tmp/errors"; fi ;;
	esac
done < "$tmp/plan"
[ "$files" -gt 0 ] || echo "1. no file installed" >> "$tmp/errors"

# Rules 3 and 4: what the wireless modules declare.
find "$moddir/kernel/drivers/net/wireless" -name '*.ko*' > "$tmp/modules" 2>/dev/null || true
find "$moddir/kernel/drivers/staging" -name '*.ko*' > "$tmp/staging" 2>/dev/null || true
while IFS= read -r m; do
	deps=$("$modinfo" -F depends "$m") || die "modinfo -F depends $m failed"
	if printf '%s\n' "$deps" | tr ',' '\n' | grep -Eqx 'cfg80211|mac80211'; then echo "$m" >> "$tmp/modules"; fi
done < "$tmp/staging"
: > "$tmp/declared"
while IFS= read -r m; do
	"$modinfo" -F firmware "$m" >> "$tmp/declared" || die "modinfo -F firmware $m failed"
done < "$tmp/modules"
[ -s "$tmp/modules" ] || echo "4. no wireless module in $moddir" >> "$tmp/errors"
sort -u -o "$tmp/declared" "$tmp/declared"
if grep -Eq '^driver[[:space:]]+iwlwifi[[:space:]]' "$list" && ! grep -Eq '^iwlwifi-.*-c?[0-9]+\.ucode$' "$tmp/declared"; then
	echo "3. iwlwifi is listed, but no wireless module declares an iwlwifi .ucode" >> "$tmp/errors"
fi
# shellcheck disable=SC2016
awk -v windows="$tmp/windows" "$WINDOWS"'
BEGIN { loadwin(windows) }
$0 ~ /^iwlwifi-.*-c?[0-9]+\.ucode$/ {
	pre = $0; sub(/-c?[0-9]+\.ucode$/, "", pre); api = $0; sub(/\.ucode$/, "", api); sub(/.*-/, "", api)
	w = window(pre)
	if (w == "") { print "3. declared " $0 ", but no window row for " pre; next }
	split(w, ww, " ")
	if (apinum(ww[2]) != apinum(api)) print "3. declared " $0 ", but the window of " pre " ends at " ww[2]
}' "$tmp/declared" >> "$tmp/errors"
touch "$tmp/whence-links"
while IFS= read -r n; do
	if { [ -f "$src/$n" ] || grep -Fxq -- "$n" "$tmp/whence-links"; } && ! [ -e "$dest/$n" ]; then
		echo "4. $n is declared and in the checkout, but not installed" >> "$tmp/errors"
	fi
done < "$tmp/declared"

if [ -s "$tmp/errors" ]; then
	sed 's/^/arca-wifi: incomplete: /' "$tmp/errors" >&2
	exit 1
fi
line="wifi-firmware: $files files, $bytes bytes raw"
mkdir -p "$(dirname "$out")"
{ echo "$line"; cat "$tmp/list"; } > "$out"
chmod 0644 "$out"
echo "$line"
