#!/usr/bin/env bash
# Upload a published tree without changing its signatures or layout.
# Usage: upload-tree.sh --tree <dir> --config <file> [--dry-run]
set +x
set -euo pipefail

tree='' config='' dry_run=false
bucket='' endpoint='' region='' prefix=''
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
	bucket | endpoint | region | prefix)
		[ -z "${!key}" ] || refuse "duplicate config key"
		printf -v "$key" '%s' "$value"
		;;
	*) refuse "unknown config key" ;;
	esac
done <"$config"
if [ -z "$bucket" ] || [ -z "$endpoint" ] || [ -z "$region" ]; then
	refuse "config requires bucket, endpoint and region"
fi
[[ "$bucket" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] || refuse "invalid bucket"
[[ "$endpoint" =~ ^https?://[a-zA-Z0-9.-]+(:[0-9]+)?/?$ ]] || refuse "endpoint must be an HTTP(S) origin without credentials or a path"
[[ "$region" =~ ^[a-zA-Z0-9-]+$ ]] || refuse "invalid region"
if [ -n "$prefix" ]; then
	[[ "$prefix" =~ ^[a-zA-Z0-9._-]+(/[a-zA-Z0-9._-]+)*$ ]] || refuse "invalid prefix"
	case "/$prefix/" in */../* | */./*) refuse "prefix must not contain dot segments" ;; esac
fi
if [ -z "${RCLONE_S3_ACCESS_KEY_ID:-}" ] || [ -z "${RCLONE_S3_SECRET_ACCESS_KEY:-}" ]; then
	refuse "set RCLONE_S3_ACCESS_KEY_ID and RCLONE_S3_SECRET_ACCESS_KEY"
fi
command -v rclone >/dev/null || die "rclone is not on PATH"
if [ ! -d "$tree" ] || [ ! -s "$tree/index.json" ] || [ ! -s "$tree/index.sjson" ]; then
	refuse "tree requires nonempty index.json and index.sjson"
fi
[ -n "$(find "$tree" -mindepth 2 -type f -print -quit)" ] || refuse "tree has no release files"
[ -z "$(find "$tree" ! -type f ! -type d -print -quit)" ] || refuse "tree must contain only regular files and directories"

remote=:s3:$bucket
[ -z "$prefix" ] || remote=$remote/$prefix
payload=(--exclude /index.json --exclude /index.sjson)
indexes=(--include /index.json --include /index.sjson)

# Ignore ambient rclone logging, filters, remote definitions and configuration.
rc() (
	for name in ${!RCLONE_@}; do
		case "$name" in
		RCLONE_S3_ACCESS_KEY_ID | RCLONE_S3_SECRET_ACCESS_KEY | RCLONE_S3_SESSION_TOKEN) ;;
		*) unset "$name" ;;
		esac
	done
	rclone --config /dev/null --s3-provider Other --s3-env-auth=false \
		--s3-endpoint "$endpoint" --s3-region "$region" --s3-force-path-style \
		--log-level ERROR --stats 0 --retries 1 "$@" 2>/dev/null
)

log "target bucket=$bucket endpoint=$endpoint"
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
