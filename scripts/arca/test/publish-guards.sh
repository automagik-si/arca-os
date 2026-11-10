#!/usr/bin/env bash
# Tests the publishing guards without a builder: published-apps.sh (the check
# publish-build.sh runs on the tree it wrote) and nvidia-packs.sh (which NVIDIA
# driver packs a build can publish), with objcopy and systemd-dissect stubbed.
#
#   scripts/arca/test/publish-guards.sh
#
# It prints "PASS <case>" or "FAIL <case>" per case and exits 1 on any FAIL.
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
fails=0
check() { # <case> <description> <command...>
	local c=$1 d=$2
	shift 2
	if "$@"; then printf 'PASS %s: %s\n' "$c" "$d"; else printf 'FAIL %s: %s\n' "$c" "$d"; fails=$((fails + 1)); fi
}
runs() { # <want rc> <want stdout> <command...>
	local want=$1 out=$2 got rc
	shift 2
	set +e
	got=$("$@" 2>/dev/null)
	rc=$?
	set -e
	[ "$rc" -eq "$want" ] && [ "$got" = "$out" ]
}

# published-apps.sh over an index with two releases.
idx=$t/index.json
jq -n '{updates: [
	{version: "202611100002", files: [
		{filename: "x86_64/incus.raw.gz", type: "application"},
		{filename: "x86_64/nvidia.raw.gz", type: "application"},
		{filename: "x86_64/nvidia-lts.manifest.json.gz", type: "image-manifest"}]},
	{version: "202611100001", files: [
		{filename: "x86_64/nvidia-lts.raw.gz", type: "application"}]}]}' >"$idx"
pa=$here/published-apps.sh
check published-apps "all listed: exit 0, prints nothing" runs 0 '' "$pa" "$idx" 202611100002 incus nvidia
check published-apps "an app the publisher skipped is missing" runs 1 'missing arca' "$pa" "$idx" 202611100002 incus arca
check published-apps "neither a manifest nor another release's file counts" runs 1 'missing nvidia-lts' "$pa" "$idx" 202611100002 nvidia-lts
check published-apps "every missing app is named" runs 1 $'missing arca\nmissing gpu-support' "$pa" "$idx" 202611100002 arca incus gpu-support
check published-apps "a name is not a prefix match" runs 1 'missing nvidia-l' "$pa" "$idx" 202611100002 nvidia-l
check published-apps "usage" runs 2 '' "$pa" "$idx" 202611100002

# nvidia-packs.sh: the UKI's kernel is k1 unless $t/uki-broken; a pack's file is its kernel stamp, or "unreadable".
mkdir -p "$t/bin"
cat >"$t/bin/objcopy" <<'EOF'
#!/usr/bin/env bash
[ ! -e "$T/uki-broken" ] || exit 1
printf 'k1\0' >"${*: -1}"
EOF
cat >"$t/bin/systemd-dissect" <<'EOF'
#!/usr/bin/env bash
[ "$SYSTEMD_DISSECT_VERITY_SIGNATURE" = 0 ] || sleep 30
[ "$(cat "$2")" != hangs ] || sleep 30
[ "$(cat "$2")" != unreadable ] || exit 1
cat "$2"
EOF
chmod +x "$t"/bin/*
export T=$t PATH=$t/bin:$PATH
np=$here/nvidia-packs.sh
b=$t/build
mkdir -p "$b"
: >"$b/IncusOS_202611100002.efi"
check nvidia-packs "nothing built" runs 0 $'nvidia missing\nnvidia-lts missing\ngpu-support missing' "$np" "$b"
echo k1 >"$b/nvidia.raw"
echo k0 >"$b/nvidia-lts.raw"
: >"$b/gpu-support.raw"
check nvidia-packs "a matching and a mismatched pack" runs 0 $'nvidia ok k1\nnvidia-lts kernel k0 k1\ngpu-support ok' "$np" "$b"
echo unreadable >"$b/nvidia-lts.raw"
check nvidia-packs "an unreadable pack is an error, not missing" runs 0 $'nvidia ok k1\nnvidia-lts error its kernel stamp cannot be read\ngpu-support ok' "$np" "$b"
echo hangs >"$b/nvidia-lts.raw"
check nvidia-packs "a read that hangs times out as an error" runs 0 $'nvidia ok k1\nnvidia-lts error its kernel stamp cannot be read\ngpu-support ok' env NVIDIA_PACKS_TIMEOUT=2 "$np" "$b"
: >"$b/nvidia-lts.raw"
check nvidia-packs "an empty stamp never matches" runs 0 $'nvidia ok k1\nnvidia-lts kernel none k1\ngpu-support ok' "$np" "$b"
touch "$t/uki-broken"
check nvidia-packs "an unreadable UKI fails the check" runs 1 '' "$np" "$b"
rm -f "$t/uki-broken"
: >"$b/IncusOS_202611100003.efi"
check nvidia-packs "two UKIs fail the check" runs 1 '' "$np" "$b"
check nvidia-packs "usage" runs 2 '' "$np"

[ "$fails" -eq 0 ] || exit 1
