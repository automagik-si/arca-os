#!/usr/bin/env bash
# The fixture test of mkosi.images/base/mkosi.conf.d/arca-wifi.sh.
#
#   scripts/arca/test/wifi-fw.sh
#
# It builds a tiny linux-firmware tree from wifi-fw/WHENCE and wifi-fw/files.txt,
# a module directory of text "modules" read by a stand-in modinfo, and a driver
# list, then runs the script against them: the complete fixture must install
# exactly the expected files and links, each completeness rule must fail its
# own broken fixture with exit 1 and that rule's message, and an empty driver
# list, a failing modinfo and an unsafe WHENCE path must be refused before
# anything is installed. Prints PASS/FAIL lines; exits 1 when any case fails.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=$here/../../../mkosi.images/base/mkosi.conf.d/arca-wifi.sh
fix=$here/wifi-fw
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failed=0
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1: $2"; failed=1; }

# The linux-firmware tree.
mkdir -p "$tmp/src"
cp "$fix/WHENCE" "$tmp/src/WHENCE"
while IFS= read -r f; do
	mkdir -p "$tmp/src/$(dirname "$f")"
	printf 'fixture %s\n' "$f" > "$tmp/src/$f"
done < "$fix/files.txt"

# The stand-in modinfo: modinfo -F <field> <module> prints the module's
# "<field>: <value>" lines.
cat > "$tmp/modinfo" <<'EOF'
#!/bin/sh
[ "$1" = -F ] && [ $# -eq 3 ] || exit 2
sed -n "s/^$2: //p" "$3"
EOF
chmod 0755 "$tmp/modinfo"
# One that prints nothing, and one that fails.
mkdir -p "$tmp/bin"
printf '#!/bin/sh\nexit 0\n' > "$tmp/bin/modinfo-empty"
printf '#!/bin/sh\nexit 1\n' > "$tmp/bin/modinfo-fail"
chmod 0755 "$tmp/bin/modinfo-empty" "$tmp/bin/modinfo-fail"

# module <dir> <path under kernel/> <line>...: writes one text module.
module() {
	local d=$1 p=$2; shift 2
	mkdir -p "$d/kernel/$(dirname "$p")"
	printf '%s\n' "$@" > "$d/kernel/$p"
}
# modules <dir> [extra firmware line for fakewifi.ko]: the complete module set.
modules() {
	local d=$1
	module "$d" drivers/net/wireless/fake/fakewifi.ko 'firmware: fakewifi/fw.bin' 'depends: cfg80211' ${2:+"$2"}
	module "$d" drivers/net/wireless/intel/iwlwifi/iwlwifi.ko.zst \
		'firmware: iwlwifi-so-a0-gf-a0-89.ucode' 'firmware: iwlwifi-so-a0-gf-a0.pnvm' \
		'firmware: iwlwifi-bz-b0-fm-c0-c102.ucode' 'depends: cfg80211'
	# A staging Wi-Fi module counts; a staging module of something else does not,
	# though it names a file of the tree that is not installed.
	module "$d" drivers/staging/stagewifi/stagewifi.ko 'firmware: fakewifi/board.txt' 'depends: mac80211,cfg80211'
	module "$d" drivers/staging/other/other.ko 'firmware: other/blob.bin' 'depends: '
}

# drivers <file> [sed expression]: the complete driver list, edited.
drivers() {
	printf '%s\n' '# fixture' \
		'driver fakewifi - A fixture Wi-Fi driver' \
		'driver iwlwifi - Intel Wireless Wifi' \
		'iwlwifi iwlwifi-bz-?0-fm*-?0 c101 c102' \
		'iwlwifi iwlwifi-so-?0-gf*-?0 89 89' | sed -e "${2:-}" > "$1"
}

# run <case> <src> <drivers> <modules> [modinfo]: runs the script into
# $tmp/<case>/dest.
run() {
	local c=$1
	mkdir -p "$tmp/$c/dest"
	ARCA_WIFI_FW_SRC=$2 ARCA_WIFI_DRIVERS=$3 ARCA_WIFI_MODULES=$4 ARCA_WIFI_MODINFO=${5:-$tmp/modinfo} \
		DESTDIR=$tmp/$c/dest sh "$script" > "$tmp/$c/out" 2>&1
}

# The complete fixture.
modules "$tmp/mod"
drivers "$tmp/drivers"
rc=0
run complete "$tmp/src" "$tmp/drivers" "$tmp/mod" || rc=$?
if [ "$rc" -ne 0 ]; then
	fail complete "exit $rc: $(cat "$tmp/complete/out")"
else
	fw=$tmp/complete/dest/usr/lib/firmware
	want_files='fakewifi/board name.txt
fakewifi/fw-1.bin
fakewifi/fw-2.bin
intel/iwlwifi/iwlwifi-bz-b0-fm-c0-c102.ucode
intel/iwlwifi/iwlwifi-so-a0-gf-a0-89.ucode
intel/iwlwifi/iwlwifi-so-a0-gf-a0.pnvm'
	want_links='fakewifi/board.txt
fakewifi/fw.bin
fakewifi/up.bin
iwlwifi-bz-b0-fm-c0-c102.ucode
iwlwifi-so-a0-gf-a0-89.ucode
iwlwifi-so-a0-gf-a0.pnvm'
	got_files=$(cd "$fw" && find . -type f | sed 's#^\./##' | LC_ALL=C sort)
	got_links=$(cd "$fw" && find . -type l | sed 's#^\./##' | LC_ALL=C sort)
	bytes=0
	while IFS= read -r f; do bytes=$((bytes + $(wc -c < "$tmp/src/$f"))); done <<< "$want_files"
	list=$tmp/complete/dest/usr/share/arca-wifi/firmware.txt
	line="wifi-firmware: 6 files, $bytes bytes raw"
	if [ "$got_files" != "$want_files" ]; then fail complete "files: $(echo "$got_files" | tr '\n' ' ')"
	elif [ "$got_links" != "$want_links" ]; then fail complete "links: $(echo "$got_links" | tr '\n' ' ')"
	elif [ -n "$(cd "$fw" && find . -type l ! -exec test -e {} \; -print)" ]; then fail complete "a link does not resolve"
	elif [ -n "$(cd "$fw" && find . -type f ! -perm 0644 -print)" ]; then fail complete "a file is not mode 0644"
	elif [ "$(tail -n 1 "$tmp/complete/out")" != "$line" ]; then fail complete "printed: $(tail -n 1 "$tmp/complete/out")"
	elif [ "$(head -n 1 "$list")" != "$line" ]; then fail complete "the list's first line differs"
	elif [ "$(sed -n 's/^f //p' "$list" | LC_ALL=C sort)" != "$want_files" ] \
		|| [ "$(sed -n 's/^l //p' "$list" | LC_ALL=C sort)" != "$want_links" ]; then fail complete "the list's entries differ"
	else pass "complete ($line; so-a0-gf-a0-88 and bz c107 outside their windows, other/blob.bin unlisted)"; fi
fi

# fails <case> <message regex> <src> <drivers> <modules> [modinfo]: the run
# must exit 1 with a message matching "^arca-wifi: <regex>" and install no list.
fails() {
	local c=$1 re="^arca-wifi: $2" rc=0
	run "$c" "$3" "$4" "$5" ${6:+"$6"} || rc=$?
	if [ "$rc" -ne 1 ]; then fail "$c" "exit $rc, want 1"
	elif ! grep -q "$re" "$tmp/$c/out"; then fail "$c" "no '$re' message: $(cat "$tmp/$c/out")"
	elif [ -e "$tmp/$c/dest/usr/share/arca-wifi/firmware.txt" ]; then fail "$c" "the list was written"
	else pass "$c ($(grep -m 1 "$re" "$tmp/$c/out"))"; return 0; fi
	return 1
}
# broken <case> <rule> <src> <drivers> <modules> [modinfo]: that rule's message.
broken() { fails "$1" "incomplete: $2\\. " "${@:3}" || true; }
# refused <case> <message regex> <src> <drivers> <modules> [modinfo]: also
# nothing installed.
refused() {
	fails "$@" || return 0
	[ -z "$(find "$tmp/$1/dest" ! -type d)" ] || fail "$1" "something was installed"
}

# Rule 1: a listed file is missing from the tree; a listed driver has no section.
cp -a "$tmp/src" "$tmp/src-missing"
rm "$tmp/src-missing/fakewifi/fw-1.bin"
broken rule1-missing-file 1 "$tmp/src-missing" "$tmp/drivers" "$tmp/mod"
# shellcheck disable=SC2016  # a sed expression
drivers "$tmp/drivers-nosection" '$a driver nosuch - Not in WHENCE'
broken rule1-no-section 1 "$tmp/src" "$tmp/drivers-nosection" "$tmp/mod"
# Rule 2: an iwlwifi .ucode prefix with no window row.
drivers "$tmp/drivers-norow" '/iwlwifi-bz-/d'
broken rule2-no-row 2 "$tmp/src" "$tmp/drivers-norow" "$tmp/mod"
# Rule 3: a window's maximum is not the API the module declares.
drivers "$tmp/drivers-max" 's/^\(iwlwifi iwlwifi-so-.*\) 89 89$/\1 89 90/'
broken rule3-window-max 3 "$tmp/src" "$tmp/drivers-max" "$tmp/mod"
# Rule 4: a wireless module declares a file of the tree that was not installed.
modules "$tmp/mod-undeclared" 'firmware: other/blob.bin'
broken rule4-not-installed 4 "$tmp/src" "$tmp/drivers" "$tmp/mod-undeclared"

# A listed section with no files, and modules that declare nothing: nothing is
# installed, which is no pass.
cp -a "$tmp/src" "$tmp/src-nofiles"
printf '%s\n' '' '-----' '' 'Driver: emptywifi - A fixture section with no files' '' 'Licence: fixture' >> "$tmp/src-nofiles/WHENCE"
printf '%s\n' 'driver emptywifi - A fixture section with no files' > "$tmp/drivers-nofiles"
module "$tmp/mod-none" drivers/net/wireless/none/none.ko 'depends: cfg80211'
broken no-files 1 "$tmp/src-nofiles" "$tmp/drivers-nofiles" "$tmp/mod-none"
# iwlwifi is listed, but modinfo declares no iwlwifi .ucode.
broken modinfo-empty 3 "$tmp/src" "$tmp/drivers" "$tmp/mod" "$tmp/bin/modinfo-empty"
# An empty driver list, and one with only comments and windows.
: > "$tmp/drivers-empty"
refused list-empty 'no driver line' "$tmp/src" "$tmp/drivers-empty" "$tmp/mod"
drivers "$tmp/drivers-nodriver" '/^driver /d'
refused list-no-driver 'no driver line' "$tmp/src" "$tmp/drivers-nodriver" "$tmp/mod"
# A modinfo that fails.
fails modinfo-fails 'modinfo -F [a-z]* .* failed' "$tmp/src" "$tmp/drivers" "$tmp/mod" "$tmp/bin/modinfo-fail" || true
# Unsafe WHENCE paths in a listed section: refused before anything is
# installed, and nothing lands outside the destination.
n=0
for l in 'File: ../escape.bin' 'RawFile: "/escape.bin"' 'File: fakewifi/../../escape.bin' \
	'Link: ../escape.bin -> fakewifi/fw-1.bin' 'Link: /escape.bin -> fakewifi/fw-1.bin' \
	'Link: fakewifi/escape.bin -> ../../escape.bin' 'Link: fakewifi/escape.bin -> /escape.bin'; do
	n=$((n + 1))
	cp -a "$tmp/src" "$tmp/src-unsafe$n"
	sed -i "s#^Link: fakewifi/fw.bin -> fw-2.bin\$#&\\n$l#" "$tmp/src-unsafe$n/WHENCE"
	grep -qxF "$l" "$tmp/src-unsafe$n/WHENCE" || { fail "unsafe$n" "fixture not edited"; continue; }
	refused "unsafe$n" "unsafe path: " "$tmp/src-unsafe$n" "$tmp/drivers" "$tmp/mod"
	if [ -e "$tmp/escape.bin" ] || [ -e "$tmp/unsafe$n/escape.bin" ]; then fail "unsafe$n" "a file escaped"; fi
done

exit "$failed"
