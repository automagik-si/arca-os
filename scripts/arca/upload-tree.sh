#!/usr/bin/env bash
# Upload a published tree without changing its signatures or layout.
# Usage: upload-tree.sh --tree <dir> --config <file> [--dry-run]
# Run it on the build host, as the user that publishes the served tree, on that copy.
# It refuses while an automatic publishing run or another upload holds its lock,
# and uploads from a hard-link snapshot taken next to the tree.
set +x
set -euo pipefail

tree='' config='' dry_run=false
bucket='' endpoint='' region='' prefix='' mirror_bucket_root=''
log() { printf 'upload-tree: %s\n' "$*" >&2; }
die() { log "$*"; exit 1; }
refuse() { log "refused: $*"; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--tree) [ $# -ge 2 ] || refuse "need --tree value"; tree=$2; shift 2 ;;
	--config) [ $# -ge 2 ] || refuse "need --config value"; config=$2; shift 2 ;;
	--dry-run) dry_run=true; shift ;;
	-h | --help) echo 'usage: upload-tree.sh --tree <dir> --config <file> [--dry-run]'; exit 0 ;;
	*) refuse "unknown argument (see --help)" ;;
	esac
done

if [ -z "$config" ] || [ ! -f "$config" ] || [ ! -r "$config" ]; then
	refuse "a readable --config file is required"
fi
while IFS= read -r line || [ -n "$line" ]; do
	case "$line" in '' | '#'* ) continue ;; esac
	case "$line" in *=*) ;; *) refuse "config must contain key=value lines" ;; esac
	key=${line%%=*} value=${line#*=}
	case "$key" in
	bucket | endpoint | region | prefix | mirror_bucket_root)
		[ -n "$value" ] || refuse "config key $key has no value"
		[ -z "${!key}" ] || refuse "duplicate config key $key"
		printf -v "$key" '%s' "$value"
		;;
	*) refuse "unknown config key" ;;
	esac
done <"$config"
if [ -z "$bucket" ] || [ -z "$endpoint" ] || [ -z "$region" ]; then
	refuse "config requires bucket, endpoint and region"
fi
[[ "$bucket" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] || refuse "invalid bucket"
# Plain HTTP shows the access key id and lets the path alter or replay requests.
if ! [[ "$endpoint" =~ ^https://[a-zA-Z0-9.-]+(:[0-9]+)?/?$ ]] &&
	! [[ "$endpoint" =~ ^http://(127\.0\.0\.1|localhost|\[::1\])(:[0-9]+)?/?$ ]]; then
	refuse "endpoint must be an HTTPS origin (plain HTTP only for a loopback host) without credentials or a path"
fi
[[ "$region" =~ ^[a-zA-Z0-9-]+$ ]] || refuse "invalid region"
# The destination is a mirror, so the whole bucket has to be asked for by name.
if [ -n "$prefix" ] && [ -n "$mirror_bucket_root" ]; then
	refuse "config sets both prefix and mirror_bucket_root"
fi
if [ -n "$mirror_bucket_root" ] && [ "$mirror_bucket_root" != yes ]; then
	refuse "mirror_bucket_root accepts only yes"
fi
if [ -z "$prefix" ] && [ -z "$mirror_bucket_root" ]; then
	refuse "config requires prefix (mirror_bucket_root=yes mirrors to the whole bucket and deletes everything else in it)"
fi
if [ -n "$prefix" ]; then
	[[ "$prefix" =~ ^[a-zA-Z0-9._-]+(/[a-zA-Z0-9._-]+)*$ ]] || refuse "invalid prefix"
	case "/$prefix/" in */../* | */./*) refuse "prefix must not contain dot segments" ;; esac
fi
if [ -z "${RCLONE_S3_ACCESS_KEY_ID:-}" ] || [ -z "${RCLONE_S3_SECRET_ACCESS_KEY:-}" ]; then
	refuse "set RCLONE_S3_ACCESS_KEY_ID and RCLONE_S3_SECRET_ACCESS_KEY"
fi
for tool in rclone jq flock openssl; do command -v "$tool" >/dev/null || die "$tool is not on PATH"; done
# release-tree.sh and the publisher rewrite the builder's tree in place, under no lock held here.
builder_tree() { refuse "$1 is the builder's own tree; upload the served copy on the build host"; }
case "$tree" in /srv/artifacts | /srv/artifacts/*) builder_tree "$tree" ;; esac
if [ -z "$tree" ] || [ ! -d "$tree" ]; then
	refuse "--tree must be a directory"
fi
source_tree=$(cd "$tree" && pwd -P)
case "$source_tree" in /srv/artifacts | /srv/artifacts/*) builder_tree "$source_tree" ;; esac
# The locks live in the user's cache directory, so only the publishing user's upload sees them.
[ -O "$source_tree" ] || refuse "the tree belongs to another user; run the upload as the user that publishes it"

# The same lock as autopublish.sh, which writes the served tree, then this script's own.
cache=${XDG_CACHE_HOME:-$HOME/.cache}
mkdir -p "$cache"
exec 7>>"$cache/arca-autopublish.lock"
flock -n 7 || refuse "an automatic publishing run holds $cache/arca-autopublish.lock"
exec 8>>"$cache/arca-upload-tree.lock"
flock -n 8 || refuse "another upload holds $cache/arca-upload-tree.lock"

# A signal must not remove the snapshot under a running rclone: its prune would follow the deletion.
rclone_pid=''
stop() {
	trap '' TERM INT HUP
	if [ -n "$rclone_pid" ]; then
		kill "$rclone_pid" 2>/dev/null || true
		wait "$rclone_pid" 2>/dev/null || true
	fi
	log "stopped by a signal; rclone has ended and the snapshot is removed"
	exit 1
}
trap stop TERM INT HUP

# A killed upload cannot clean up; its snapshot only holds hard links.
while IFS= read -r -d '' old; do
	log "removing a snapshot left by a killed upload more than a day ago: $old"
	rm -rf "$old"
done < <(find "$(dirname "$source_tree")" -maxdepth 1 -type d -name '.upload-tree.*' -mmin +1440 -print0)

# Every phase reads this snapshot; hard links need the tree's own file system.
work=$(mktemp -d "$(dirname "$source_tree")/.upload-tree.XXXXXX") || die "cannot create a snapshot directory next to the tree"
trap 'rm -rf "$work"' EXIT
tree=$work/tree
cp -al "$source_tree" "$tree" || die "cannot take a hard-link snapshot of the tree"

if [ ! -s "$tree/index.json" ] || [ ! -s "$tree/index.sjson" ]; then
	refuse "tree requires nonempty index.json and index.sjson"
fi
[ -n "$(find "$tree" -mindepth 2 -type f -print -quit)" ] || refuse "tree has no release files"
[ -z "$(find "$tree" ! -type f ! -type d -print -quit)" ] || refuse "tree must contain only regular files and directories"

# Machines read only index.sjson: it has to carry the index.json checked here (as publish-build.sh
# checks it, but without the CA, which stays in the builder: the signer's chain is not judged).
openssl smime -verify -noverify -text -in "$tree/index.sjson" -out "$work/signed.json" 2>/dev/null ||
	refuse "index.sjson is not a readable signed index"
signed=$(jq -S . "$work/signed.json") || refuse "index.sjson does not carry JSON"
plain=$(jq -S . "$tree/index.json") || refuse "index.json is not a readable index"
[ "$signed" = "$plain" ] || refuse "index.sjson does not carry index.json"

# Every file the index lists must be in the snapshot with the listed size.
(cd "$tree" && find . -type f -printf '%s\t%P\n') |
	jq -Rn '[inputs | split("\t") | {(.[1:] | join("\t")): (.[0] | tonumber)}] | add // {}' >"$work/have.json" ||
	die "cannot list the snapshot"
# shellcheck disable=SC2016  # jq variables, not shell
unlisted=$(jq -r --slurpfile have "$work/have.json" '(.updates // [])[] | .version as $v | (.files // [])[]
	| "\($v)/\(.filename)" as $p | select(($have[0][$p] // -1) != .size) | $p | @json' "$tree/index.json") ||
	refuse "index.json is not a readable index"
if [ -n "$unlisted" ]; then
	refuse "index.json lists files that are missing or have another size: $(printf '%s' "$unlisted" | head -5 | tr '\n' ' ')"
fi

remote=:s3:$bucket destination="$bucket (the whole bucket)"
# The trailing slash names a directory; without it rclone probes for a file and hides a refused request.
if [ -n "$prefix" ]; then
	remote=$remote/$prefix/ destination=$bucket/$prefix
fi
payload=(--exclude /index.json --exclude /index.sjson)
indexes=(--include /index.json --include /index.sjson)

# Ignore ambient rclone logging, filters, remote definitions and configuration.
# On failure show rclone's ERROR lines (file names and statuses), credentials blanked.
# rclone runs in the background so that a signal reaches stop() while it runs.
rc() {
	local result=0 errors name value
	(
		for name in ${!RCLONE_@}; do
			case "$name" in
			RCLONE_S3_ACCESS_KEY_ID | RCLONE_S3_SECRET_ACCESS_KEY | RCLONE_S3_SESSION_TOKEN) ;;
			*) unset "$name" ;;
			esac
		done
		exec rclone --config /dev/null --s3-provider Other --s3-env-auth=false \
			--s3-endpoint "$endpoint" --s3-region "$region" --s3-force-path-style \
			--log-level ERROR --stats 0 --retries 1 "$@"
	) 2>"$work/rclone.err" &
	rclone_pid=$!
	wait "$rclone_pid" || result=$?
	rclone_pid=''
	if [ "$result" -ne 0 ]; then
		errors=$(head -50 "$work/rclone.err")
		for name in RCLONE_S3_ACCESS_KEY_ID RCLONE_S3_SECRET_ACCESS_KEY RCLONE_S3_SESSION_TOKEN; do
			value=${!name:-}
			[ -z "$value" ] || errors=${errors//"$value"/<credential>}
		done
		[ -z "$errors" ] || printf '%s\n' "$errors" | sed 's/^/upload-tree: rclone: /' >&2
	fi
	return "$result"
}

log "destination $destination at $endpoint"
if "$dry_run"; then
	log "dry-run: planned changes (= unchanged, + upload, * replace, - remove); no writes"
	rc sync "$tree" "$remote" --checksum --dry-run --combined - || die "dry-run failed"
	exit 0
fi

log "phase 1: copy releases and public keys, then verify their bytes"
rc copy "$tree" "$remote" --checksum "${payload[@]}" || die "release/key upload failed; indexes unchanged"
rc check "$tree" "$remote" --download --one-way "${payload[@]}" || die "release/key verification mismatch; indexes unchanged"
log "phase 2: copy indexes last"
rc copy "$tree" "$remote" --checksum "${indexes[@]}" || die "index upload failed; stale files retained"
log "phase 3: read back and compare every file, including index.sjson and releases"
rc check "$tree" "$remote" --download --one-way || die "readback verification mismatch; stale files retained"
log "phase 4: remove stale files after verified indexes"
rc sync "$tree" "$remote" --checksum --delete-after || die "stale-file cleanup failed"
rc check "$tree" "$remote" || die "final tree verification mismatch"
log "complete: remote tree verified"
