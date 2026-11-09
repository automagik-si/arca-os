#!/usr/bin/env bash
# Change an update tree's release state. Runs in the builder, as root, from the
# top of a checkout (through in-builder.sh), and signs with the persistent
# update key at $ARCA_TEST_CA_DIR (default /srv/arca-test-ca).
#
#   release-tree.sh --tree <dir> notes   --version <ver> --file <notes.json>
#   release-tree.sh --tree <dir> promote --version <ver> --base <ver> [--to stable] [--from testing]
#   release-tree.sh --tree <dir> fail    --version <ver> --base <ver> --reason <text> [--from testing]
#   release-tree.sh --tree <dir> prune   --remove <ver,ver,...>
#
# notes    copies <notes.json> to <ver>/notes.json, signs it as notes.sjson,
#          and marks the build /srv/artifacts/os/<ver>/ as autopublished.
# promote  writes <ver>/canary.json (state passed), then adds the same signed
#          release to --to with image-publisher promote: nothing is rebuilt.
#          It refuses a version whose canary.json says failed.
# fail     writes <ver>/canary.json (state failed, with the reason) and
#          removes the version from --from, so no node in that channel takes it.
# prune    deletes the named versions from the tree, and their builds under
#          /srv/artifacts/os/ when autopublish made them, then regenerates and
#          re-signs the index. It refuses to remove the newest release of any
#          channel.
# Every command re-reads the index and checks both signatures afterwards.
set -euo pipefail

CA_DIR=${ARCA_TEST_CA_DIR:-/srv/arca-test-ca}
OS_ARTIFACTS=/srv/artifacts/os
tree='' cmd='' version='' file='' base='' reason='' remove=''
to=stable from=testing

log() { printf 'release-tree: %s\n' "$*" >&2; }
die() { printf 'release-tree: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'release-tree: refused: %s\n' "$*" >&2; exit 2; }

[ $# -ge 3 ] && [ "$1" = --tree ] || refuse "usage: release-tree.sh --tree <dir> <notes|promote|fail|prune> [options]"
tree=$2 cmd=$3
shift 3
while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || refuse "need a value for $1"; version=$2; shift 2 ;;
	--file) [ $# -ge 2 ] || refuse "need a value for $1"; file=$2; shift 2 ;;
	--base) [ $# -ge 2 ] || refuse "need a value for $1"; base=$2; shift 2 ;;
	--reason) [ $# -ge 2 ] || refuse "need a value for $1"; reason=$2; shift 2 ;;
	--remove) [ $# -ge 2 ] || refuse "need a value for $1"; remove=$2; shift 2 ;;
	--to) [ $# -ge 2 ] || refuse "need a value for $1"; to=$2; shift 2 ;;
	--from) [ $# -ge 2 ] || refuse "need a value for $1"; from=$2; shift 2 ;;
	*) refuse "unknown argument: $1" ;;
	esac
done

is_version() { printf '%s' "$1" | grep -Eqx '[0-9]{12}'; }
case "$cmd" in
notes) is_version "$version" || refuse "--version <12 digits> is required"; [ -f "$file" ] || refuse "no notes file '$file'" ;;
promote) is_version "$version" || refuse "--version <12 digits> is required"; is_version "$base" || refuse "--base <12 digits> is required" ;;
fail)
	is_version "$version" || refuse "--version <12 digits> is required"
	is_version "$base" || [ "$base" = none ] || refuse "--base <12 digits or none> is required"
	[ -n "$reason" ] || refuse "--reason is required"
	;;
prune) printf '%s' "$remove" | grep -Eqx '[0-9]{12}(,[0-9]{12})*' || refuse "--remove <ver,...> is required" ;;
*) refuse "unknown command '$cmd'" ;;
esac
[ "$(id -u)" -eq 0 ] || die "run as root (inside the builder)"
[ -f "$tree/index.json" ] || die "no index.json in $tree"
cd "$(dirname "$0")/../.."
[ -f incus-osd/go.mod ] || die "not at the top of an Arca OS checkout"
for c in jq openssl; do command -v "$c" >/dev/null || die "missing tool: $c"; done

sig_key=$CA_DIR/certs/update-E1.key
sig_cert=$CA_DIR/certs/update-E1.crt
sig_chain=$CA_DIR/files/update-E1.crt
for f in "$sig_key" "$sig_cert" "$sig_chain"; do
	[ -f "$f" ] || die "persistent CA file missing: $f"
done
export SIG_KEY=$sig_key SIG_CERTIFICATE=$sig_cert SIG_CHAIN=$sig_chain

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

publisher() {
	if [ ! -x "$tmp/image-publisher" ]; then
		log "building image-publisher"
		(cd incus-osd && go build -o "$tmp/image-publisher" ./cmd/image-publisher) || die "could not build image-publisher"
	fi
	"$tmp/image-publisher" "$@" >&2
}
# sign_json <file>: writes <file without .json>.sjson, signed like the index.
sign_json() {
	openssl smime -text -sign -signer "$sig_cert" -inkey "$sig_key" -certfile "$sig_chain" \
		-in "$1" -out "${1%.json}.sjson" || die "could not sign $1"
}
verify_tree() {
	local s
	for s in index "$@"; do
		openssl smime -verify -text -partial_chain -CAfile "$sig_chain" \
			-in "$tree/$s.sjson" -out "$tmp/verified.json" 2>/dev/null || die "$s.sjson does not verify against the update CA"
		[ "$(jq -S . "$tmp/verified.json")" = "$(jq -S . "$tree/$s.json")" ] || die "$s.sjson does not carry $s.json"
	done
}
channels_of() { jq -r --arg v "$1" '.updates[] | select(.version == $v) | .channels // [] | join(",")' "$tree/index.json"; }
in_channel() { jq -e --arg v "$1" --arg c "$2" 'any(.updates[]; .version == $v and ((.channels // []) | index($c) != null))' "$tree/index.json" >/dev/null; }
write_canary() { # <state> <reason>
	jq -n --arg v "$version" --arg s "$1" --arg r "$2" --arg b "$base" --arg t "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		'{version: $v, state: $s, reason: $r, base: $b, checked_at: $t}' >"$tree/$version/canary.json"
	sign_json "$tree/$version/canary.json"
}

case "$cmd" in
notes)
	[ -d "$tree/$version" ] || die "no release $version in $tree"
	jq -e --arg v "$version" '.version == $v and (.changes | type == "array")' "$file" >/dev/null \
		|| die "$file is not notes for $version"
	jq . "$file" >"$tree/$version/notes.json"
	sign_json "$tree/$version/notes.json"
	if [ -d "$OS_ARTIFACTS/$version" ]; then
		cp "$tree/$version/notes.json" "$OS_ARTIFACTS/$version/autopublish.json"
	fi
	verify_tree "$version/notes"
	log "notes for $version: $(jq '.changes | length' "$file") changes"
	;;
promote)
	[ -d "$tree/$version" ] || die "no release $version in $tree"
	if [ -f "$tree/$version/canary.json" ] && [ "$(jq -r .state "$tree/$version/canary.json")" = failed ]; then
		refuse "$version failed its canary; it is never promoted"
	fi
	in_channel "$version" "$from" || die "$version is not in channel $from (channels: $(channels_of "$version"))"
	write_canary passed "the canary updated from $base to $version and passed its checks"
	if in_channel "$version" "$to"; then
		log "$version is already in channel $to"
	else
		publisher promote "$tree" "$version" "$to"
	fi
	in_channel "$version" "$to" || die "$version is not in channel $to after promote"
	verify_tree "$version/update" "$version/canary"
	echo "promoted: $version to $to (channels: $(channels_of "$version"))"
	;;
fail)
	[ -d "$tree/$version" ] || die "no release $version in $tree"
	write_canary failed "$reason"
	if in_channel "$version" "$from"; then
		publisher demote "$tree" "$version" "$from"
	fi
	! in_channel "$version" "$from" || die "$version is still in channel $from"
	! in_channel "$version" "$to" || die "$version is in channel $to although it failed"
	verify_tree "$version/update" "$version/canary"
	echo "failed: $version (channels: $(channels_of "$version" | sed 's/^$/none/'))"
	;;
prune)
	IFS=, read -r -a vs <<<"$remove"
	newest=$(jq -r '[.updates[] | .channels // [] | .[]] | unique | .[]' "$tree/index.json" | while read -r c; do
		jq -r --arg c "$c" '[.updates[] | select((.channels // []) | index($c) != null)][0].version' "$tree/index.json"
	done)
	for v in "${vs[@]}"; do
		if printf '%s\n' "$newest" | grep -qx "$v"; then refuse "$v is the newest release of a channel"; fi
	done
	for v in "${vs[@]}"; do
		if [ -d "$tree/$v" ]; then
			rm -rf "${tree:?}/$v"
			log "removed $v from the tree"
		fi
		if [ -f "$OS_ARTIFACTS/$v/autopublish.json" ]; then
			rm -rf "${OS_ARTIFACTS:?}/$v"
			log "removed the build $OS_ARTIFACTS/$v"
		fi
	done
	# image-publisher regenerates and re-signs the index on any change; setting a
	# kept release's severity to its own value is a change of nothing else.
	keep=$(find "$tree" -mindepth 2 -maxdepth 2 -name update.json -printf '%h\n' | xargs -rn1 basename | sort -n | tail -1)
	[ -n "$keep" ] || die "no release would be left in $tree"
	publisher severity "$tree" "$keep" "$(jq -r '.severity // "none"' "$tree/$keep/update.json")"
	for v in "${vs[@]}"; do
		jq -e --arg v "$v" 'any(.updates[]; .version == $v) | not' "$tree/index.json" >/dev/null || die "$v is still in the index"
	done
	verify_tree
	echo "pruned: $remove"
	;;
esac
