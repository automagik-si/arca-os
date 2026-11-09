#!/usr/bin/env bash
# make-usb.sh: write an Arca OS installer image to a USB stick, on Linux or macOS.
#
#   scripts/arca/make-usb.sh [IMAGE]              pick a stick, confirm, write, verify
#   scripts/arca/make-usb.sh --list               list the USB sticks it would offer
#   scripts/arca/make-usb.sh --dry-run [IMAGE]    every step except the write
#
# IMAGE defaults to the newest *-installer.img in the current directory. Only removable or USB disks are offered, never
# the disk the running system boots from. The write erases the whole stick, so the stick's name must be typed back to
# confirm. A SHA256SUMS file beside the image is checked first, and the stick is read back and compared after the write.
# Written for bash 3.2, which macOS ships.
set -euo pipefail

prog=$(basename "$0")
os=$(uname -s)
image=""
device=""
assume_yes=0
dry_run=0
list_only=0
verify=1

if [ -t 1 ]; then
	b=$'\033[1m' dim=$'\033[2m' red=$'\033[31m' grn=$'\033[32m' ylw=$'\033[33m' cyn=$'\033[36m' rst=$'\033[0m'
else
	b="" dim="" red="" grn="" ylw="" cyn="" rst=""
fi

say() { printf '%s\n' "$*"; }
step() { printf '\n%s==>%s %s%s%s\n' "$cyn" "$rst" "$b" "$*" "$rst"; }
ok() { printf '    %s✓%s %s\n' "$grn" "$rst" "$*"; }
warn() { printf '    %s!%s %s\n' "$ylw" "$rst" "$*" >&2; }
die() {
	printf '%s%s: %s%s\n' "$red" "$prog" "$*" "$rst" >&2
	exit 1
}

# Background helpers that must not outlive the script: the macOS dd, the sudo keepalive and their temp files.
ddpid=""
keepalive=""
tmpf=""
errf=""
cleanup() {
	if [ -n "$keepalive" ]; then
		kill "$keepalive" 2>/dev/null || true
		keepalive=""
	fi
	if [ -n "$ddpid" ]; then
		sudo -n pkill -P "$ddpid" 2>/dev/null || true
		sudo -n kill "$ddpid" 2>/dev/null || true
		ddpid=""
	fi
	if [ -n "$tmpf" ]; then rm -f "$tmpf"; fi
	if [ -n "$errf" ]; then rm -f "$errf"; fi
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT TERM

usage() {
	cat <<EOF
usage: $prog [options] [IMAGE]

Writes an Arca OS installer image to a USB stick. Everything on the stick is erased.

options:
  --device DEV   use this stick instead of asking (for example /dev/sdb or /dev/disk4)
  --yes          skip the typed confirmation (needs --device)
  --no-verify    skip reading the stick back after the write
  --dry-run      do every step except the write
  --list         list the USB sticks and exit
  -h, --help     show this help
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--device)
		[ $# -ge 2 ] || die "--device needs a value"
		device=$2
		shift 2
		;;
	--yes) assume_yes=1 && shift ;;
	--no-verify) verify=0 && shift ;;
	--dry-run) dry_run=1 && shift ;;
	--list) list_only=1 && shift ;;
	-h | --help) usage && exit 0 ;;
	-*) usage >&2 && exit 2 ;;
	*)
		[ -z "$image" ] || die "only one IMAGE may be given"
		image=$1
		shift
		;;
	esac
done
[ "$assume_yes" = 0 ] || [ -n "$device" ] || die "--yes needs --device"

case "$os" in
Linux | Darwin) ;;
*) die "this script runs on Linux or macOS, not $os" ;;
esac

human() { # bytes -> "3.4 GB"
	awk -v n="$1" 'BEGIN { split("B KB MB GB TB", u); i = 1; while (n >= 1000 && i < 5) { n /= 1000; i++ } printf (i == 1 ? "%d %s" : "%.1f %s"), n, u[i] }'
}

file_size() {
	if [ "$os" = Darwin ]; then stat -f %z "$1"; else stat -c %s "$1"; fi
}

sha256_of() { # reads stdin
	if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1; else shasum -a 256 | cut -d' ' -f1; fi
}

# --- Disks ------------------------------------------------------------------------------------------------------------
# Each candidate is one line: "DEV<TAB>BYTES<TAB>DESCRIPTION".

linux_boot_sources() { # the devices behind /, /boot, the EFI partition, /usr and active swap
	local m src
	for m in / /boot /boot/efi /efi /usr; do
		src=$(findmnt -nvo SOURCE "$m" 2>/dev/null) || continue
		if printf '%s\n' "$src" | grep -qx systemd-1; then
			# An automount point (common for /efi and /boot): the filesystem is mounted on first access, so touch it
			# and ask for what is mounted there now. If that still names no device, the mount failed and the
			# system is not running from it, so it is skipped.
			ls "$m" >/dev/null 2>&1 || true
			src=$(findmnt -nvo SOURCE --target "$m" 2>/dev/null) || continue
		fi
		printf '%s\n' "$src"
	done
	swapon --show=NAME --noheadings 2>/dev/null || true
}

linux_boot_disks() { # the whole disks behind linux_boot_sources (lsblk -s walks LVM, LUKS, RAID and btrfs down to them)
	local src
	linux_boot_sources | while read -r src; do
		case "$src" in /dev/*) ;; *) continue ;; esac
		lsblk -nslpo NAME,TYPE "$src" 2>/dev/null | awk '$2 == "disk" { print $1 }'
	done | sort -u
}

linux_in_use() { # dies when the disk or a partition is held by LVM, RAID or encryption, or is active swap
	local p n h held swaps
	swaps=$(swapon --show=NAME --noheadings 2>/dev/null || true)
	for p in $(lsblk -nlpo NAME "$1" 2>/dev/null); do
		n=$(basename "$p")
		held=""
		for h in "/sys/class/block/$n/holders/"*; do
			if [ -e "$h" ]; then held="$held ${h##*/}"; fi
		done
		[ -z "$held" ] || die "$p is in use by${held} (LVM, RAID or encryption); stop that first, then run again"
		if printf '%s\n' "$swaps" | grep -qx "$p"; then die "$p is active swap; turn it off with: sudo swapoff $p"; fi
	done
}

linux_field() { # disk column -> value (asked one column at a time, since an empty column would shift a joined line)
	lsblk -dnbo "$2" "$1" 2>/dev/null | sed 's/^ *//; s/ *$//'
}

linux_candidates() {
	local boot name tran rm size model
	boot=$(linux_boot_disks)
	lsblk -dnpo NAME 2>/dev/null | while read -r name; do
		[ "$(linux_field "$name" TYPE)" = disk ] || continue
		tran=$(linux_field "$name" TRAN)
		rm=$(linux_field "$name" RM)
		size=$(linux_field "$name" SIZE)
		model=$(linux_field "$name" MODEL)
		[ "$tran" = usb ] || [ "$rm" = 1 ] || continue
		[ "$size" -gt 0 ] 2>/dev/null || continue
		if printf '%s\n' "$boot" | grep -qx "$name"; then continue; fi
		printf '%s\t%s\t%s\n' "$name" "$size" "${model:-USB disk}"
	done
}

mac_info() { # device key -> value from `diskutil info`
	diskutil info "$1" 2>/dev/null | awk -F: -v k="$2" '{ key = $1; sub(/^ +/, "", key) } key == k { v = $0; sub(/^[^:]*: */, "", v); print v; exit }'
}

mac_boot_disks() { # the APFS container of / and the physical disk behind it (APFS Physical Store disk0s2 -> disk0)
	local d
	for d in $(mac_info / "APFS Physical Store") $(mac_info / "Part of Whole"); do
		printf '/dev/%s\n' "$d" | sed 's/\(disk[0-9]*\)s[0-9]*$/\1/'
	done | sort -u
}

boot_disks() {
	if [ "$os" = Darwin ]; then mac_boot_disks; else linux_boot_disks; fi
}

mac_candidates() {
	local dev size name boot
	boot=$(mac_boot_disks)
	diskutil list external physical 2>/dev/null | awk '/^\/dev\/disk[0-9]+/ { print $1 }' | while read -r dev; do
		if printf '%s\n' "$boot" | grep -qx "$dev"; then continue; fi
		size=$(mac_info "$dev" "Disk Size" | sed -n 's/.*(\([0-9]*\) Bytes).*/\1/p')
		[ -n "$size" ] || continue
		name=$(mac_info "$dev" "Device / Media Name")
		printf '%s\t%s\t%s\n' "$dev" "$size" "${name:-External disk}"
	done
}

candidates() {
	if [ "$os" = Darwin ]; then mac_candidates; else linux_candidates; fi
}

check_target() { # dies unless $dev is still the stick that was picked ($line), not a boot disk and not in use
	local now
	if boot_disks | grep -qx "$dev"; then die "$dev holds the running system; refusing to write to it"; fi
	now=$(candidates | awk -F'\t' -v d="$dev" '$1 == d' | head -n 1)
	[ "$now" = "$line" ] || die "the stick changed; run again"
	[ "$os" = Darwin ] || linux_in_use "$dev"
}

none_found="No USB sticks found. Built-in SD-card readers and internal disks are not offered."

describe_contents() { # a short look at what the stick holds now
	if [ "$os" = Darwin ]; then
		diskutil list "$1" 2>/dev/null | awk 'NR > 2 && NF { sub(/^ +/, ""); print "      " $0 }'
	else
		lsblk -npo NAME,SIZE,FSTYPE,LABEL,MOUNTPOINT "$1" 2>/dev/null | awk '{ print "      " $0 }'
	fi
}

# --- Listing ----------------------------------------------------------------------------------------------------------

list=$(candidates)
if [ "$list_only" = 1 ]; then
	[ -n "$list" ] || {
		say "$none_found"
		exit 0
	}
	printf '%s\n' "$list" | while IFS=$'\t' read -r dev size name; do
		printf '%s  %8s  %s\n' "$dev" "$(human "$size")" "$name"
	done
	exit 0
fi

say "${b}Arca OS USB creator${rst}"

# --- The image --------------------------------------------------------------------------------------------------------

step "Installer image"
if [ -z "$image" ]; then
	# shellcheck disable=SC2012 # installer image names are plain; ls -t gives the newest first on both systems
	image=$(ls -t ./*-installer.img 2>/dev/null | head -n 1 || true)
	[ -n "$image" ] || die "no *-installer.img here; pass the image path, for example: $prog path/to/IncusOS_<version>-installer.img"
fi
[ -f "$image" ] || die "no such image: $image"
img_size=$(file_size "$image")
[ "$img_size" -gt 0 ] || die "the image is empty: $image"
ok "$(basename "$image") ($(human "$img_size"))"

sums="$(dirname "$image")/SHA256SUMS"
img_sha=""
if [ -f "$sums" ]; then
	want=$(awk -v f="$(basename "$image")" '{ n = $2; sub(/^\*/, "", n) } n == f { print $1; exit }' "$sums")
	if [ -n "$want" ]; then
		printf '    checking its checksum… '
		img_sha=$(sha256_of <"$image")
		[ "$img_sha" = "$want" ] || {
			say
			die "the image does not match SHA256SUMS; download or copy it again"
		}
		say "${grn}matches SHA256SUMS${rst}"
	else
		warn "SHA256SUMS does not list this image; skipping the checksum"
	fi
else
	warn "no SHA256SUMS beside the image; skipping the checksum"
fi

# --- The stick --------------------------------------------------------------------------------------------------------

step "USB stick"
if [ -n "$device" ]; then
	line=$(printf '%s\n' "$list" | awk -F'\t' -v d="$device" '$1 == d' | head -n 1)
	[ -n "$line" ] || die "$device is not a USB stick this script will write to (run $prog --list)"
else
	[ -n "$list" ] || die "$none_found Plug one in and run $prog again."
	count=$(printf '%s\n' "$list" | wc -l | tr -d ' ')
	i=0
	printf '%s\n' "$list" | while IFS=$'\t' read -r dev size name; do
		i=$((i + 1))
		printf '    %s%d)%s %-12s %8s  %s\n' "$b" "$i" "$rst" "$dev" "$(human "$size")" "$name"
	done
	if [ "$count" = 1 ]; then
		pick=1
		printf '    Only one stick found. Use it? [y/N] '
		read -r answer </dev/tty
		case "$answer" in y | Y | yes | YES) ;; *) die "stopped; nothing was written" ;; esac
	else
		printf '    Which stick? [1-%s] ' "$count"
		read -r pick </dev/tty
		case "$pick" in '' | *[!0-9]*) die "not a number: $pick" ;; esac
		if [ "$pick" -lt 1 ] || [ "$pick" -gt "$count" ]; then die "no stick number $pick"; fi
	fi
	line=$(printf '%s\n' "$list" | sed -n "${pick}p")
fi
dev=$(printf '%s' "$line" | cut -f1)
dev_size=$(printf '%s' "$line" | cut -f2)
dev_name=$(printf '%s' "$line" | cut -f3)
[ "$dev_size" -ge "$img_size" ] || die "$dev holds $(human "$dev_size"), but the image needs $(human "$img_size")"
check_target

say
say "    ${b}${red}Everything on this stick will be erased:${rst}"
say "      ${b}$dev${rst}  $(human "$dev_size")  $dev_name"
describe_contents "$dev"
say
if [ "$assume_yes" = 1 ]; then
	ok "confirmed by --yes"
else
	short=$(basename "$dev")
	printf '    Type %s%s%s to erase it and write the installer: ' "$b" "$short" "$rst"
	read -r typed </dev/tty
	[ "$typed" = "$short" ] || die "you typed '$typed', not '$short'; nothing was written"
fi

# --- Write ------------------------------------------------------------------------------------------------------------

if [ "$dry_run" = 1 ]; then
	step "Dry run"
	ok "would unmount $dev and write $(human "$img_size") to it"
	exit 0
fi

step "Preparing"
say "    Writing needs administrator rights; your password may be asked for."
sudo -v || die "sudo is needed to write to $dev"
# Keep sudo's timestamp fresh through a long write and verify; cleanup stops it.
(while sleep 60; do sudo -n -v 2>/dev/null || exit 0; done) >/dev/null 2>&1 &
keepalive=$!
check_target
if [ "$os" = Darwin ]; then
	diskutil unmountDisk "$dev" >/dev/null || die "could not unmount $dev; close any window using it and try again"
	target=${dev/\/dev\/disk//dev/rdisk}
	bs=4m
else
	mounted() { for p in $(lsblk -nlpo NAME "$dev"); do findmnt -rno SOURCE -S "$p" >/dev/null 2>&1 && echo "$p"; done; }
	for part in $(mounted); do
		if command -v udisksctl >/dev/null 2>&1; then
			udisksctl unmount -b "$part" >/dev/null 2>&1 || sudo umount "$part" ||
				die "could not unmount $part; close any window or program using it and try again"
		else
			sudo umount "$part" || die "could not unmount $part; close any window or program using it and try again"
		fi
	done
	[ -z "$(mounted)" ] || die "$dev still has a mounted partition"
	target=$dev
	bs=4M
fi
ok "$dev unmounted"
check_target

bar() { # done total -> one redrawn progress line
	awk -v d="$1" -v t="$2" -v s="$3" 'BEGIN {
		p = t > 0 ? d / t : 0; if (p > 1) p = 1; w = 30; f = int(p * w)
		line = ""; for (i = 0; i < w; i++) line = line (i < f ? "█" : "░")
		rate = s > 0 ? sprintf("%.0f MB/s", d / s / 1e6) : ""
		printf "\r    %s %3d%%  %-9s", line, p * 100, rate
	}'
}

step "Writing (this takes a few minutes)"
start=$(date +%s)
if [ "$os" = Linux ]; then
	# dd's own progress, redrawn as a bar. Its exit status is the pipeline's, through pipefail. Lines that do not start
	# with a digit are dd's (or sudo's) errors, kept for the failure message.
	errf="${TMPDIR:-/tmp}/make-usb.$$.err"
	: >"$errf"
	if ! sudo dd if="$image" of="$target" bs="$bs" oflag=direct conv=fsync status=progress 2>&1 >/dev/null |
		tr '\r' '\n' | while read -r ln; do
			case "$ln" in
			'') ;;
			[0-9]*)
				n=${ln%% *}
				case "$n" in *[!0-9]*) ;; *) bar "$n" "$img_size" "$(($(date +%s) - start))" ;; esac
				;;
			*) printf '%s\n' "$ln" >>"$errf" ;;
			esac
		done; then
		say
		errs=$(cat "$errf" 2>/dev/null || true)
		die "the write to $dev failed${errs:+: $errs}"
	fi
	bar "$img_size" "$img_size" "$(($(date +%s) - start))"
	say
else
	# macOS dd prints its progress on SIGINFO. $ddpid is sudo's pid; ps -p works on it although it runs as root.
	tmpf="${TMPDIR:-/tmp}/make-usb.$$"
	sudo dd if="$image" of="$target" bs="$bs" 2>"$tmpf" &
	ddpid=$!
	while ps -p "$ddpid" >/dev/null 2>&1; do
		sleep 2
		# Under sudo's pty monitor dd is a grandchild of $ddpid, so root's dd is signalled by name, only while ours
		# runs. A concurrent root dd elsewhere would print its progress too.
		if ps -p "$ddpid" >/dev/null 2>&1; then sudo -n pkill -INFO -x dd -U 0 2>/dev/null || true; fi
		n=$(tr '\r' '\n' <"$tmpf" | awk '/bytes/ { n = $1 } END { print n + 0 }')
		bar "$n" "$img_size" "$(($(date +%s) - start))"
	done
	rc=0
	wait "$ddpid" || rc=$?
	ddpid=""
	[ "$rc" = 0 ] || die "the write to $dev failed: $(grep -v '^[0-9]' "$tmpf" 2>/dev/null || true)"
	bar "$img_size" "$img_size" "$(($(date +%s) - start))"
	say
fi
# Flush the stick only: a whole-system sync can take minutes on a busy machine.
if [ "$os" = Linux ]; then sudo sync "$target"; else sync; fi
ok "written in $(($(date +%s) - start)) s"

# --- Verify -----------------------------------------------------------------------------------------------------------

if [ "$verify" = 1 ]; then
	step "Verifying (reading the stick back)"
	[ -n "$img_sha" ] || img_sha=$(sha256_of <"$image")
	# Whole 4 MiB blocks, since a raw macOS device refuses unaligned reads; head trims the read to the image's length.
	blocks=$(((img_size + 4194303) / 4194304))
	# On Linux iflag=direct reads the stick itself, not the page cache the write just filled.
	iflag=""
	[ "$os" = Darwin ] || iflag="iflag=direct"
	# shellcheck disable=SC2086 # $iflag is one word or nothing
	got=$({ sudo dd if="$target" bs=4194304 count="$blocks" $iflag 2>/dev/null || true; } | head -c "$img_size" | sha256_of)
	[ "$got" = "$img_sha" ] || die "the stick does not read back as the image; try another stick"
	ok "the stick matches the image"
fi

if [ "$os" = Darwin ]; then
	if diskutil eject "$dev" >/dev/null 2>&1; then ok "ejected; you can unplug it"; else warn "eject $dev before unplugging it"; fi
else
	if command -v udisksctl >/dev/null 2>&1 && udisksctl power-off -b "$dev" >/dev/null 2>&1; then
		ok "powered off; you can unplug it"
	else
		ok "you can unplug it"
	fi
fi

# --- Next steps -------------------------------------------------------------------------------------------------------

cat <<EOF

${b}${grn}The installer stick is ready.${rst}

${b}Install on the target machine${rst}
  1. Connect it to the network, ideally by cable. Its first boot downloads its applications.
  2. In its firmware settings (usually F2, Del or Esc at power-on), put Secure Boot in ${b}Setup Mode${rst}
     (often "Reset to Setup Mode" or "Clear Secure Boot keys"). The installer enrolls its own keys.
  3. Boot from the stick (the boot menu key is usually F12, F10 or Esc).
  4. ${red}${b}The installer erases the machine's internal disk.${rst} With an image built to install without asking,
     it starts right away.
  5. When the screen says the install is complete, unplug the stick. The machine reboots into Arca OS.
  6. On first boot it downloads its applications. The screen shows its network address when it is ready.

${dim}It has no login prompt: it is managed over the network, from the machine whose client certificate the
image was built with.${rst}
EOF
