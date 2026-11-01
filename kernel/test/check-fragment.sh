#!/usr/bin/env bash
# Test the binder fragment and build-kernel.sh's two config modes, without a
# kernel build:
#
#   kernel/test/check-fragment.sh
#
# On both fixtures it runs --detect-upstream and --verify-config, then merges
# kernel/arca.config with the kernel's own scripts/kconfig/merge_config.sh and
# checks that the result verifies. merge_config.sh comes from $KSRC when that
# names a kernel tree, else from Zabbly's x86_64 branch on GitHub.
#
# It prints PASS or FAIL per check, and exits 1 on any FAIL.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
K=$here/../build-kernel.sh
FRAG=$here/../arca.config
FX=$here/fixtures
MERGE_URL=https://raw.githubusercontent.com/zabbly/linux/refs/heads/x86_64/scripts/kconfig/merge_config.sh

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
fails=0 passes=0
rc=0

# run <cmd...>: output in $T/out, exit code in $rc.
run() {
	rc=0
	"$@" > "$T/out" 2>&1 || rc=$?
}
result() {
	if [ "$2" -eq 1 ]; then
		printf 'PASS %s\n' "$1"; passes=$((passes + 1))
	else
		printf 'FAIL %s\n' "$1"; fails=$((fails + 1))
		sed 's/^/  | /' "$T/out"
	fi
}
# expect <name> <rc> [<line>...]: the last run exited <rc> and printed each line.
expect() {
	local name=$1 want=$2 line ok=1
	shift 2
	[ "$rc" -eq "$want" ] || ok=0
	for line in "$@"; do
		grep -qxF -- "$line" "$T/out" || ok=0
	done
	result "$name (rc=$rc)" "$ok"
}
# absent <name> <line>...: the last run printed none of the lines.
absent() {
	local name=$1 line ok=1
	shift
	for line in "$@"; do
		if grep -qxF -- "$line" "$T/out"; then ok=0; fi
	done
	result "$name" "$ok"
}
# has <name> <file> <line>...: <file> has every line.
has() {
	local name=$1 file=$2 line ok=1
	shift 2
	for line in "$@"; do
		grep -qxF -- "$line" "$file" || ok=0
	done
	cp "$file" "$T/out"
	result "$name" "$ok"
}

IPC='CONFIG_ANDROID_BINDER_IPC=y'
FS='CONFIG_ANDROID_BINDERFS=y'
DEV='CONFIG_ANDROID_BINDER_DEVICES=""'
LV='CONFIG_LOCALVERSION="-zabbly-arca"'

# The fragment is exactly the four lines Arca OS needs.
printf '%s\n' "$IPC" "$FS" "$DEV" "$LV" > "$T/want"
run diff "$T/want" "$FRAG"
expect "fragment has exactly the four lines" 0

# --detect-upstream
run "$K" --detect-upstream "$FX/zabbly-no-binder.config"
expect "detect-upstream no-binder fixture" 0 'upstream-binder: no'
run "$K" --detect-upstream "$FX/zabbly-with-binder.config"
expect "detect-upstream with-binder fixture" 0 'upstream-binder: yes'
grep -vx "$FS" "$FX/zabbly-with-binder.config" > "$T/ipc-only.config"
run "$K" --detect-upstream "$T/ipc-only.config"
expect "detect-upstream needs binderfs too" 0 'upstream-binder: no'

# --verify-config before any merge
run "$K" --verify-config "$FX/zabbly-no-binder.config"
expect "verify-config no-binder fixture names all four lines" 3 \
	"verify-config: missing $IPC" "verify-config: missing $FS" "verify-config: missing $DEV" "verify-config: missing $LV"
run "$K" --verify-config "$FX/zabbly-with-binder.config"
expect "verify-config with-binder fixture names devices and localversion" 3 \
	"verify-config: missing $DEV" "verify-config: missing $LV"
absent "verify-config with-binder fixture does not name ipc or binderfs" \
	"verify-config: missing $IPC" "verify-config: missing $FS"

# The kernel's own merge tool.
if [ -n "${KSRC:-}" ]; then
	MC=$KSRC/scripts/kconfig/merge_config.sh
else
	MC=$T/merge_config.sh
	curl -fsSL --retry 3 -o "$MC" "$MERGE_URL"
fi
[ -s "$MC" ] || { echo "FAIL no merge_config.sh at $MC"; exit 1; }

# merge <fixture> <dir>: <dir>/.config is the fixture with the fragment merged in.
merge() {
	mkdir -p "$2"
	cp "$1" "$2/base.config"
	(cd "$2" && sh "$MC" -m -O "$2" base.config "$FRAG") > "$T/out" 2>&1
}

for fx in no-binder with-binder; do
	d=$T/merge-$fx
	run merge "$FX/zabbly-$fx.config" "$d"
	expect "merge_config.sh -m on $fx fixture" 0
	run "$K" --verify-config "$d/.config"
	expect "verify-config after merge, $fx fixture" 0 'verify-config: ok'
	run "$K" --detect-upstream "$d/.config"
	expect "detect-upstream after merge, $fx fixture" 0 'upstream-binder: yes'
	has "merge keeps the fixture's other lines, $fx fixture" "$d/.config" \
		'CONFIG_MMU=y' 'CONFIG_NET=y' 'CONFIG_MODULE_SIG=y' 'CONFIG_SECURITY_LOCKDOWN_LSM=y' '# CONFIG_LOCALVERSION_AUTO is not set'
	run grep -cx -e '# CONFIG_ANDROID_BINDER_IPC is not set' -e 'CONFIG_LOCALVERSION="-zabbly"' \
		-e 'CONFIG_ANDROID_BINDER_DEVICES="binder,hwbinder,vndbinder"' "$d/.config"
	expect "merge replaces the old values, $fx fixture" 1 0
done

# Usage errors exit 2.
run "$K" --verify-config "$T/missing.config"
expect "verify-config on a missing file" 2
run "$K" --detect-upstream "$T/missing.config"
expect "detect-upstream on a missing file" 2
run "$K"
expect "no mode" 2
run "$K" --verify-config "$FX/zabbly-no-binder.config" --detect-upstream "$FX/zabbly-no-binder.config"
expect "two modes" 2
run "$K" --verify-config "$FX/zabbly-no-binder.config" --ref 0000000000000000000000000000000000000000
expect "--ref without --out" 2

printf 'check-fragment: %d passed, %d failed\n' "$passes" "$fails"
[ "$fails" -eq 0 ]
