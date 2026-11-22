#!/usr/bin/env bash
# Tests make-usb.sh's read-back verification without a USB stick, through
# stub lsblk/dd/sudo (Linux) and diskutil/dd/sudo (macOS) commands.
#
#   scripts/arca/test/make-usb.sh
#
# It prints "PASS <case>" or "FAIL <case>" per case and exits 1 on any FAIL.
# MAKE_USB overrides the script under test, to replay the same cases against
# another revision.
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
make_usb=${MAKE_USB:-$here/make-usb.sh}
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
fails=0
real_dd=$(command -v dd)
real_stat=$(command -v stat)
image=$t/IncusOS_202611100434-installer.img
# 1 MiB and 123 bytes: not a whole number of 4 MiB read blocks.
head -c 1048699 /dev/zero >"$image"
(cd "$t" && sha256sum -- "$(basename "$image")" >SHA256SUMS)
img_size=$(wc -c <"$image" | tr -d '[:space:]')

write_stubs() { # <dir>
	local d=$1
	mkdir -p "$d"

	cat >"$d/sudo" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
-v) exit 0 ;;
-n) shift; [ "${1:-}" = -v ] && exit 0; exec "$@" ;;
*) exec "$@" ;;
esac
EOF

	cat >"$d/pkill" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

	cat >"$d/kill" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

	cat >"$d/sync" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

	cat >"$d/udisksctl" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

	cat >"$d/findmnt" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF

	cat >"$d/swapon" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

	cat >"$d/lsblk" <<'EOF'
#!/usr/bin/env bash
case "${1:-} ${2:-}" in
'-dnpo NAME') printf '%s\n' "$FAKE_DEV" ;;
'-dnbo TYPE') echo disk ;;
'-dnbo TRAN') echo usb ;;
'-dnbo RM') echo 1 ;;
'-dnbo SIZE') printf '%s\n' "$FAKE_SIZE" ;;
'-dnbo MODEL') echo 'Fake Stick' ;;
'-nlpo NAME') printf '%s\n' "$FAKE_DEV" ;;
'-npo NAME,SIZE,FSTYPE,LABEL,MOUNTPOINT') printf '%s 1G ext4 FAKE /mnt\n' "$FAKE_DEV" ;;
*) exit 1 ;;
esac
EOF

	cat >"$d/dd" <<'EOF'
#!/usr/bin/env bash
in="" out="" bs=0 count=0 direct=0
for a in "$@"; do
	case "$a" in
	if=*) in=${a#if=} ;;
	of=*) out=${a#of=} ;;
	bs=*) bs=${a#bs=} ;;
	count=*) count=${a#count=} ;;
	iflag=direct) direct=1 ;;
	esac
done
case "$in" in /dev/disk* | /dev/rdisk*) in=$FAKE_STICK ;; esac
case "$out" in /dev/disk* | /dev/rdisk*) out=$FAKE_STICK ;; esac
if [ -n "$out" ]; then
	if [ "$FAKE_MODE" = differ ]; then
		cp "$in" "$out"
		printf X | "$REAL_DD" of="$out" bs=1 seek=0 conv=notrunc status=none 2>/dev/null
	else
		cp "$in" "$out"
	fi
	printf '%s bytes copied\n' "$(wc -c <"$out" | tr -d ' ')" >&2
	exit 0
fi
case "$FAKE_MODE:$direct" in
all-fail:*) printf "dd: error reading '%s': Input/output error\n" "$in" >&2; exit 1 ;;
direct-fail:1) printf "dd: error reading '%s': Input/output error\n" "$in" >&2; exit 1 ;;
short:*) head -c 10 "$in"; exit 0 ;;
esac
cat "$in"
want=$((bs * count))
have=$(wc -c <"$in" | tr -d ' ')
if [ "$want" -gt "$have" ]; then head -c $((want - have)) /dev/zero; fi
exit 0
EOF

	chmod +x "$d"/*
}

write_mac_stubs() { # <dir>
	local d=$1
	write_stubs "$d"

	cat >"$d/uname" <<'EOF'
#!/usr/bin/env bash
echo Darwin
EOF

	cat >"$d/stat" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = -f ] && [ "${2:-}" = %z ]; then
	wc -c <"$3" | tr -d ' '
else
	exec "$REAL_STAT" "$@"
fi
EOF

	cat >"$d/diskutil" <<'EOF'
#!/usr/bin/env bash
case "${1:-} ${2:-}" in
'list external') printf '%s\n' "$FAKE_DEV" ;;
'info /') exit 0 ;;
'info '*) printf '   Disk Size: 100.0 GB (%s Bytes)\n   Device / Media Name: Fake Stick\n' "$FAKE_SIZE" ;;
'unmountDisk '*) exit 0 ;;
'eject '*) exit 0 ;;
'list '*) printf '   0: FDisk_partition_scheme *%s\n' "$FAKE_DEV" ;;
*) exit 0 ;;
esac
EOF

	chmod +x "$d/diskutil" "$d/uname" "$d/stat"
}

last_rc=0
last_out=""
run() { # <mode> <stub dir> <device>
	last_out=$t/last.out
	if PATH=$2:$PATH FAKE_MODE=$1 FAKE_DEV=$3 "$make_usb" --device "$3" --yes "$image" >"$last_out" 2>&1; then
		last_rc=0
	else
		last_rc=$?
	fi
}

check_result() { # <case> <want rc> <wanted text> [forbidden text]
	local c=$1 want_rc=$2 want=$3 forbid=${4:-}
	if [ "$last_rc" -eq "$want_rc" ] && grep -qF -- "$want" "$last_out" &&
		{ [ -z "$forbid" ] || ! grep -qF -- "$forbid" "$last_out"; }; then
		printf 'PASS %s: rc %s, output has "%s"\n' "$c" "$want_rc" "$want"
	else
		printf 'FAIL %s: wanted rc %s and "%s"%s\n' "$c" "$want_rc" "$want" "${forbid:+, without \"$forbid\"}"
		sed 's/^/    /' "$last_out"
		fails=$((fails + 1))
	fi
}

export FAKE_STICK=$t/stick.img FAKE_SIZE=100000000 FAKE_IMAGE=$image
export REAL_DD=$real_dd REAL_STAT=$real_stat

write_stubs "$t/bin-linux"
run good "$t/bin-linux" "$FAKE_STICK"
check_result good 0 'the stick matches the image (direct read)' 'buffered read'

run direct-fail "$t/bin-linux" "$FAKE_STICK"
check_result direct-fail 0 'the stick matches the image (buffered read after sync)' 'could not be read back'
check_result direct-fail-warn 0 'the direct read failed' 'does not read back as the image'

run all-fail "$t/bin-linux" "$FAKE_STICK"
check_result all-fail 1 'the device could not be read back' 'does not read back as the image'
check_result all-fail-dd 1 'Input/output error'

run short "$t/bin-linux" "$FAKE_STICK"
check_result short 1 "the read stopped after 10 of $img_size bytes" 'does not read back as the image'

run differ "$t/bin-linux" "$FAKE_STICK"
check_result differ 1 'the stick does not read back as the image (direct read)' 'could not be read back'

write_mac_stubs "$t/bin-mac"
run good "$t/bin-mac" /dev/disk9
check_result mac-good 0 'the stick matches the image (buffered read)' 'iflag=direct'

if [ "$fails" -ne 0 ]; then
	printf '%d case(s) failed\n' "$fails"
	exit 1
fi
