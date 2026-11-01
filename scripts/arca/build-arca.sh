#!/usr/bin/env bash
# Build the arca binary for the arca sysext (from the top of a checkout).
#
#   build-arca.sh
#
# It takes no arguments. It reads the arca tag from scripts/arca/arca.version
# and the commit that tag must point to from scripts/arca/arca.commit. It
# clones https://github.com/weunfold/arca.git at that tag into app-build/arca
# (a path upstream's .gitignore already covers), refuses to build when the
# tag points anywhere else (a moved tag), and builds it with
# `cargo build --release --locked --target x86_64-unknown-linux-musl -p arca`,
# using the Rust toolchain that arca's rust-toolchain.toml names. It strips the
# binary, refuses one that is not static, and installs it at 0755 as
# mkosi.images/arca/mkosi.extra/usr/local/bin/arca. It writes
# mkosi.images/arca/mkosi.extra/usr/lib/arca/version ("<tag> (<commit12>)")
# and app-build/arca.json, the application manifest that generate-manifests
# merges into the arca image manifest.
# CARGO_TARGET_DIR, when set, moves cargo's build directory out of the clone.
set -euo pipefail

REPO=https://github.com/weunfold/arca.git
TARGET=x86_64-unknown-linux-musl
SRC=app-build/arca
DEST=mkosi.images/arca/mkosi.extra
MANIFEST=app-build/arca.json

usage() { sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'build-arca: %s\n' "$*" >&2; }
die() { printf 'build-arca: %s\n' "$*" >&2; exit 1; }

case "${1:-}" in
"") ;;
-h | --help) usage ;;
*) printf 'build-arca: takes no arguments: %s\n' "$*" >&2; exit 2 ;;
esac

cd "$(dirname "$0")/../.."
if [ ! -f mkosi.conf ] || [ ! -f mkosi.images/arca/mkosi.conf ]; then
	die "not at the top of an Arca OS checkout"
fi
for c in git cargo rustup strip readelf file jq; do
	command -v "$c" >/dev/null || die "$c is not on PATH"
done

tag=$(tr -d '[:space:]' < scripts/arca/arca.version)
printf '%s' "$tag" | grep -Eqx 'v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?' \
	|| die "scripts/arca/arca.version does not hold a release tag: '$tag'"
[ -r scripts/arca/arca.commit ] || die "no scripts/arca/arca.commit (the commit $tag must point to)"
pin=$(tr -d '[:space:]' < scripts/arca/arca.commit)
printf '%s' "$pin" | grep -Eqx '[0-9a-f]{40}' \
	|| die "scripts/arca/arca.commit does not hold a full 40-digit commit id: '$pin'"

# --- Source ----------------------------------------------------------------
if [ -d "$SRC/.git" ]; then
	log "updating $SRC to $tag"
	git -C "$SRC" fetch -q --depth 1 origin "refs/tags/$tag:refs/tags/$tag"
	git -C "$SRC" checkout -q -f --detach "refs/tags/$tag"
	git -C "$SRC" clean -q -ffdx -e /target/
else
	log "cloning $REPO at $tag"
	rm -rf "$SRC"
	git -c advice.detachedHead=false clone -q --depth 1 --branch "$tag" "$REPO" "$SRC"
fi
commit=$(git -C "$SRC" rev-parse HEAD)
[ "$commit" = "$(git -C "$SRC" rev-parse "refs/tags/$tag^{commit}")" ] || die "$SRC is not at $tag"
[ "$commit" = "$pin" ] || die "refused: $tag points to $commit, not the pinned $pin (a moved tag)"
[ -z "$(git -C "$SRC" status --porcelain)" ] || die "$SRC has local changes"
log "arca $tag = $commit (pinned)"

# --- Toolchain -------------------------------------------------------------
channel=$(sed -n 's/^channel *= *"\([^"]*\)".*/\1/p' "$SRC/rust-toolchain.toml")
[ -n "$channel" ] || die "no channel in $SRC/rust-toolchain.toml"
rustup toolchain list | awk -v c="$channel-" 'index($1, c) == 1 { f = 1 } END { exit !f }' \
	|| die "Rust toolchain $channel is not installed"
rustup target list --installed --toolchain "$channel" | grep -qx "$TARGET" \
	|| die "target $TARGET is not installed for Rust $channel"
log "rust $channel, target $TARGET"

# --- Build -----------------------------------------------------------------
target_dir=${CARGO_TARGET_DIR:-$PWD/$SRC/target}
start=$(date +%s)
(cd "$SRC" && CARGO_TARGET_DIR="$target_dir" cargo "+$channel" build --release --locked --target "$TARGET" -p arca)
built="$target_dir/$TARGET/release/arca"
[ -f "$built" ] || die "cargo did not produce $built"

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
cp "$built" "$tmp"
strip "$tmp"

# A static binary has no program interpreter and needs no shared library.
desc=$(file -b "$tmp")
case "$desc" in
*"statically linked"* | *"static-pie linked"*) ;;
*) die "refused: the binary is not static: $desc" ;;
esac
if readelf -lW "$tmp" | grep -q 'Requesting program interpreter'; then
	die "refused: the binary requests a program interpreter"
fi
if readelf -dW "$tmp" | grep -q '(NEEDED)'; then
	die "refused: the binary needs shared libraries"
fi

# --- Install ---------------------------------------------------------------
install -d -m 0755 "$DEST/usr/local/bin" "$DEST/usr/lib/arca"
install -m 0755 "$tmp" "$DEST/usr/local/bin/arca"
printf '%s (%s)\n' "$tag" "${commit:0:12}" > "$DEST/usr/lib/arca/version"
chmod 0644 "$DEST/usr/lib/arca/version"
jq -n --arg version "$tag" --arg repo "$REPO" \
	'[{name: "arca", version: $version, repo: $repo, installed_artifacts: ["/usr/local/bin/arca"]}]' > "$MANIFEST"

end=$(date +%s)
echo "build-seconds: $((end - start))"
echo "arca: $DEST/usr/local/bin/arca ($(stat -c %s "$DEST/usr/local/bin/arca") bytes)"
echo "file: $desc"
echo "version: $(cat "$DEST/usr/lib/arca/version")"
