#!/usr/bin/env bash
# Publish a finished build into the update tree served to nodes. Runs on the
# build host. It drives publish-build.sh in the builder (which signs with the
# persistent test CA), then copies the resulting tree to a local directory a
# static file server exports to the LAN.
#
#   publish-update.sh --version <ver> [--serve-dir <dir>] [--channel stable]
#                     [--apps incus,arca] [--with-image]
#                     [--project arcaos-dev] [--builder arcaos-builder]
#                     [--checkout <worktree>] [--builder-tree /srv/artifacts/updates]
#
# The LAN address and port the tree is served at are NOT configured here: they
# are arguments to whatever serves --serve-dir (see scripts/arca/README.md,
# "Releasing updates to nodes"). The tree accumulates releases, so republishing
# adds a version; a node picks the newest in its channel. Defaults: --serve-dir $ARCA_UPDATES_DIR or
# ~/.cache/arca-os/updates, channel stable, applications incus and arca.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
checkout=$(cd "$here/../.." && pwd)
project=arcaos-dev
builder=arcaos-builder
serve_dir=${ARCA_UPDATES_DIR:-$HOME/.cache/arca-os/updates}
builder_tree=/srv/artifacts/updates
channel=stable
version='' apps=incus,arca
pub_args=()

log() { printf 'publish-update: %s\n' "$*" >&2; }
die() { printf 'publish-update: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'publish-update: refused: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || refuse "need a value for $1"; version=$2; shift 2 ;;
	--serve-dir) [ $# -ge 2 ] || refuse "need a value for $1"; serve_dir=$2; shift 2 ;;
	--channel) [ $# -ge 2 ] || refuse "need a value for $1"; channel=$2; shift 2 ;;
	--apps) [ $# -ge 2 ] || refuse "need a value for $1"; apps=$2; shift 2 ;;
	--with-image) pub_args+=(--with-image); shift ;;
	--project) [ $# -ge 2 ] || refuse "need a value for $1"; project=$2; shift 2 ;;
	--builder) [ $# -ge 2 ] || refuse "need a value for $1"; builder=$2; shift 2 ;;
	--checkout) [ $# -ge 2 ] || refuse "need a value for $1"; checkout=$2; shift 2 ;;
	--builder-tree) [ $# -ge 2 ] || refuse "need a value for $1"; builder_tree=$2; shift 2 ;;
	-h | --help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
	*) refuse "unknown argument: $1" ;;
	esac
done

[ -n "$version" ] || refuse "--version <ver> is required"
printf '%s' "$version" | grep -Eqx '[0-9]{12}' || refuse "version '$version' is not 12 digits"
case "$project" in arcaos-*) ;; *) refuse "project '$project' does not start with arcaos-" ;; esac
case "$builder_tree" in /srv/artifacts/*) ;; *) refuse "builder tree '$builder_tree' is not under /srv/artifacts/" ;; esac
command -v incus >/dev/null || die "incus is not on PATH"

# Publish into the builder's persistent tree (in a clean checkout, so the Go
# tools are rebuilt and the persistent CA is read).
log "publishing $version in the builder"
"$here/in-builder.sh" --project "$project" --vm "$builder" "$checkout" -- \
	scripts/arca/publish-build.sh --version "$version" --out "$builder_tree" \
	--channel "$channel" --apps "$apps" "${pub_args[@]}"

# Copy the tree to the serving directory on this host.
install -d -m 0755 "$serve_dir"
log "copying the tree to $serve_dir"
incus exec --project "$project" "$builder" -- tar -C "$builder_tree" -cf - . | tar -C "$serve_dir" -xf -

# Verify the served copy's checksums against the index it carries.
idx=$serve_dir/index.json
[ -f "$idx" ] || die "no index.json in $serve_dir after copy"
command -v jq >/dev/null || die "jq is not on PATH"
jq -r --arg v "$version" '.updates[] | select(.version==$v) | .files[] | .filename+" "+.sha256' "$idx" |
	while read -r fn sha; do
		p=$serve_dir/$version/$fn
		[ -f "$p" ] || die "served tree is missing $fn"
		[ "$(sha256sum "$p" | cut -d' ' -f1)" = "$sha" ] || die "served $fn does not match the index checksum"
	done

echo "serve-dir: $serve_dir"
echo "published-version: $version"
echo "serve-hint: python3 -m http.server --bind <lan-address> --directory '$serve_dir' <port>"
echo "provider server_url for nodes: http://<lan-address>:<port>"
