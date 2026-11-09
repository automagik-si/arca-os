#!/usr/bin/env bash
# Rebuild Zabbly's x86_64 kernel tree with Android binder built in, as debs.
#
#   build-kernel.sh --out <root> [--ref <commit>] [--maintainer '<name> <email>']
#   build-kernel.sh --verify-config <file>
#   build-kernel.sh --detect-upstream <file>
#
# --out: shallow-clones Zabbly's tree at <commit> (default: the head of branch
#   x86_64), merges kernel/arca.config into Zabbly's committed .config, runs
#   `make olddefconfig` and `make bindeb-pkg`, and builds the meta packages
#   linux-image-zabbly and linux-zabbly with equivs. It writes
#   <root>/<krel>-<stamp>/ (debs/, extra/, config, arca.config, source.tar.xz,
#   BUILDINFO) and then points <root>/latest at it. The packages' Maintainer
#   is --maintainer, or else `git config user.name` and `user.email` of this
#   checkout; with neither, the build is refused before it starts.
# --verify-config: exits 0 when every line of kernel/arca.config is in <file>,
#   otherwise exits 3 and names each missing line.
# --detect-upstream: prints "upstream-binder: yes" when <file> already has
#   CONFIG_ANDROID_BINDER_IPC=y and CONFIG_ANDROID_BINDERFS=y, else "... no".
#
# Exit codes: 0 done; 1 build failure; 2 usage or missing input; 3 config check.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
FRAGMENT=$here/arca.config
META_DIR=$here/meta
WORK=$here/src
ZABBLY_URL=https://github.com/zabbly/linux.git
ZABBLY_BRANCH=x86_64
# Source, objects with debug info, staged packages and the debs, with margin.
MIN_FREE_GIB=60

usage() { sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'build-kernel: %s\n' "$*" >&2; }
die() { printf 'build-kernel: %s\n' "$*" >&2; exit 1; }
bad() { printf 'build-kernel: %s\n' "$*" >&2; exit 2; }

# The fragment's config lines (comments and blank lines skipped).
fragment_lines() {
	grep -Ev '^[[:space:]]*(#|$)' "$FRAGMENT"
}

# verify_config <file>: 0 when every fragment line is in <file>, else 3.
verify_config() {
	local file=$1 line missing=0
	[ -r "$file" ] || bad "no readable config: $file"
	[ -n "$(fragment_lines)" ] || die "fragment $FRAGMENT has no config lines"
	while IFS= read -r line; do
		if ! grep -qxF -- "$line" "$file"; then
			printf 'verify-config: missing %s\n' "$line"
			missing=1
		fi
	done < <(fragment_lines)
	if [ "$missing" -ne 0 ]; then
		return 3
	fi
	printf 'verify-config: ok\n'
}

# detect_upstream <file>: does Zabbly's own config already build binder in?
detect_upstream() {
	local file=$1
	[ -r "$file" ] || bad "no readable config: $file"
	if grep -qx 'CONFIG_ANDROID_BINDER_IPC=y' "$file" && grep -qx 'CONFIG_ANDROID_BINDERFS=y' "$file"; then
		printf 'upstream-binder: yes\n'
	else
		printf 'upstream-binder: no\n'
	fi
}

free_gib() { df -Pk "$1" | awk 'NR == 2 { printf "%d", $4 / 1048576 }'; }
sha() { sha256sum "$1" | cut -d' ' -f1; }

MODE='' ARG='' OUT='' REF='' MAINTAINER=''
while [ $# -gt 0 ]; do
	case "$1" in
	--out | --verify-config | --detect-upstream)
		[ $# -ge 2 ] || usage
		[ -z "$MODE" ] || bad "give one of --out, --verify-config, --detect-upstream"
		MODE=${1#--}; ARG=$2; shift 2 ;;
	--ref) [ $# -ge 2 ] || usage; REF=$2; shift 2 ;;
	--maintainer) [ $# -ge 2 ] || usage; MAINTAINER=$2; shift 2 ;;
	-h | --help) usage ;;
	*) bad "unknown argument: $1" ;;
	esac
done
[ -n "$MODE" ] || usage
[ -z "$REF" ] || [ "$MODE" = out ] || bad "--ref goes with --out"
[ -z "$MAINTAINER" ] || [ "$MODE" = out ] || bad "--maintainer goes with --out"

case "$MODE" in
verify-config)
	rc=0; verify_config "$ARG" || rc=$?
	exit "$rc" ;;
detect-upstream)
	detect_upstream "$ARG"
	exit 0 ;;
esac

# --out from here on.
OUT=$ARG

# The packages' Maintainer: --maintainer, else this checkout's git identity.
# It is never printed. It goes into sed replacements, so | & \ are refused.
if [ -z "$MAINTAINER" ]; then
	gname=$(git -C "$here" config user.name 2>/dev/null || true)
	gemail=$(git -C "$here" config user.email 2>/dev/null || true)
	if [ -z "$gname" ] || [ -z "$gemail" ]; then
		bad "no maintainer: pass --maintainer '<name> <email>' or set git config user.name and user.email"
	fi
	MAINTAINER="$gname <$gemail>"
fi
printf '%s' "$MAINTAINER" | grep -Eqx '[^<>|&\\[:space:]][^<>|&\\]* <[^<>|&\\[:space:]@]+@[^<>|&\\[:space:]@]+>' \
	|| bad "the maintainer must read '<name> <email>' (no < > | & or backslash in the name)"
MAINT_NAME=${MAINTAINER% <*}
MAINT_EMAIL=${MAINTAINER##* <}
MAINT_EMAIL=${MAINT_EMAIL%>}
for c in git make gcc bc bison flex pahole rsync kmod dpkg-buildpackage dpkg-deb equivs-build xz zstd flock; do
	command -v "$c" >/dev/null || die "missing tool: $c"
done
for f in "$FRAGMENT" "$META_DIR/linux-image-zabbly.ctl.in" "$META_DIR/linux-zabbly.ctl.in"; do
	[ -r "$f" ] || die "missing $f"
done
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)

# One build per checkout at a time: the work tree is shared.
exec 9<"$here"
flock -n 9 || die "another build is running from $here"

for d in "$here" "$OUT"; do
	avail=$(free_gib "$d")
	[ "$avail" -ge "$MIN_FREE_GIB" ] || die "$d has $avail GiB free, below $MIN_FREE_GIB GiB"
done

start=$(date +%s)

head=$(git ls-remote "$ZABBLY_URL" "refs/heads/$ZABBLY_BRANCH" | cut -f1)
printf '%s' "$head" | grep -Eqx '[0-9a-f]{40}' || die "cannot read the head of $ZABBLY_BRANCH from $ZABBLY_URL"
if [ -z "$REF" ]; then
	REF=$head
fi
printf '%s' "$REF" | grep -Eqx '[0-9a-f]{40}' || bad "--ref must be a full 40-digit commit id"
printf 'zabbly-head: %s\n' "$head"
printf 'zabbly-commit: %s\n' "$REF"

# Source: a fresh shallow clone at the commit.
rm -rf "$WORK"
mkdir -p "$WORK"
src=$WORK/linux
git init -q "$src"
log "fetching $ZABBLY_URL at $REF"
git -C "$src" fetch -q --depth 1 "$ZABBLY_URL" "$REF"
git -C "$src" checkout -q --detach FETCH_HEAD
[ "$(git -C "$src" rev-parse HEAD)" = "$REF" ] || die "checked out $(git -C "$src" rev-parse HEAD), not $REF"
cd "$src"
[ -f .config ] || die "the tree at $REF has no committed .config"

KVER=$(make -s kernelversion)
STAMP=$(date -u +%Y%m%d%H%M)
printf '%s' "$KVER" | grep -Eqx '[0-9]+\.[0-9]+(\.[0-9]+)?' || die "unexpected kernelversion '$KVER'"
PKGVERSION=${KVER}-amd64-${STAMP}-arca
METAVERSION=${STAMP}~amd64~arca

# The exact tree, for the GPL source offer; Zabbly's .config is part of it.
log "archiving the source"
git archive --format=tar --prefix="linux-zabbly-arca-$PKGVERSION/" HEAD | xz -T0 -6 > "$WORK/source.tar.xz"

# Config: Zabbly's committed .config plus our fragment.
cp .config "$WORK/zabbly.config"
UPSTREAM=$(detect_upstream "$WORK/zabbly.config")
printf '%s\n' "$UPSTREAM"
scripts/kconfig/merge_config.sh -m .config "$FRAGMENT" >&2
make -s olddefconfig >&2
rc=0; verify_config .config || rc=$?
[ "$rc" -eq 0 ] || exit "$rc"

# Packages, named as Zabbly names them, with our own version and source name.
export KDEB_PKGVERSION=$PKGVERSION
export KDEB_SOURCENAME=linux-zabbly-arca-$PKGVERSION
export KDEB_CHANGELOG_DIST=trixie
export KDEB_COMPRESS=zstd
export DPKG_DEB_COMPRESSOR_LEVEL=9
export DEBFULLNAME=$MAINT_NAME
export DEBEMAIL=$MAINT_EMAIL
log "make bindeb-pkg -j$(nproc) for $PKGVERSION"
make bindeb-pkg -j"$(nproc)" >&2

KREL=$(cat include/config/kernel.release)
case "$KREL" in
*-zabbly-arca*) ;;
*) die "kernel release '$KREL' lacks -zabbly-arca" ;;
esac
printf 'krel: %s\n' "$KREL"

img=$WORK/linux-image-${KREL}_${PKGVERSION}_amd64.deb
hdr=$WORK/linux-headers-${KREL}_${PKGVERSION}_amd64.deb
for f in "$img" "$hdr"; do
	[ -f "$f" ] || die "bindeb-pkg made no $(basename "$f")"
done

# The packaged config must be the verified one.
dpkg-deb --fsys-tarfile "$img" | tar -xO "./boot/config-$KREL" > "$WORK/packaged.config"
cmp -s "$WORK/packaged.config" .config || die "/boot/config-$KREL in the image deb differs from the built .config"

# Meta packages, under the names IncusOS installs.
meta=$WORK/meta
mkdir -p "$meta"
for m in linux-image-zabbly linux-zabbly; do
	sed -e "s|@KREL@|$KREL|g" -e "s|@PKGVERSION@|$PKGVERSION|g" -e "s|@METAVERSION@|$METAVERSION|g" \
		-e "s|@MAINTAINER@|$MAINTAINER|g" "$META_DIR/$m.ctl.in" > "$meta/$m.ctl"
	if grep -q '@[A-Z]*@' "$meta/$m.ctl"; then
		die "unfilled placeholder in $m.ctl"
	fi
	(cd "$meta" && equivs-build "$m.ctl" >&2)
	[ -f "$meta/${m}_${METAVERSION}_amd64.deb" ] || die "equivs-build made no ${m}_${METAVERSION}_amd64.deb"
done
want_img="linux-image-$KREL (= $PKGVERSION)"
want_hdr="linux-headers-$KREL (= $PKGVERSION)"
[ "$(dpkg-deb -f "$meta/linux-image-zabbly_${METAVERSION}_amd64.deb" Depends)" = "$want_img" ] \
	|| die "linux-image-zabbly does not depend on exactly $want_img"
[ "$(dpkg-deb -f "$meta/linux-zabbly_${METAVERSION}_amd64.deb" Depends)" = "$want_img, $want_hdr" ] \
	|| die "linux-zabbly does not depend on exactly $want_img, $want_hdr"
for f in "$img" "$hdr" "$meta/linux-image-zabbly_${METAVERSION}_amd64.deb" "$meta/linux-zabbly_${METAVERSION}_amd64.deb"; do
	[ "$(dpkg-deb -f "$f" Maintainer)" = "$MAINTAINER" ] || die "$(basename "$f") does not carry the requested Maintainer"
done
log "maintainer: all four packages carry the requested one"

# Output: staged next to the destination, then moved into place.
name=$KREL-$STAMP
dest=$OUT/$name
stage=$OUT/.$name.partial
[ ! -e "$dest" ] || die "$dest already exists"
rm -rf "$stage"
mkdir -p "$stage/debs" "$stage/extra"
mv "$img" "$hdr" "$meta/linux-image-zabbly_${METAVERSION}_amd64.deb" "$meta/linux-zabbly_${METAVERSION}_amd64.deb" "$stage/debs/"
for f in "$WORK"/*.deb "$WORK"/*.buildinfo "$WORK"/*.changes "$WORK/zabbly.config"; do
	[ ! -e "$f" ] || mv "$f" "$stage/extra/"
done
cp .config "$stage/config"
cp "$FRAGMENT" "$stage/arca.config"
mv "$WORK/source.tar.xz" "$stage/source.tar.xz"
{
	printf 'zabbly-commit: %s\n' "$REF"
	printf 'kver: %s\n' "$KVER"
	printf 'krel: %s\n' "$KREL"
	printf 'stamp: %s\n' "$STAMP"
	printf 'version: %s\n' "$PKGVERSION"
	printf 'config-sha256: %s\n' "$(sha "$stage/config")"
	printf 'fragment-sha256: %s\n' "$(sha "$stage/arca.config")"
	printf 'source-sha256: %s\n' "$(sha "$stage/source.tar.xz")"
	printf 'zabbly-head: %s\n' "$head"
	printf 'meta-version: %s\n' "$METAVERSION"
	printf '%s\n' "$UPSTREAM"
} > "$stage/BUILDINFO"
mv -T "$stage" "$dest"
ln -sfn "$name" "$OUT/.latest.new"
mv -T "$OUT/.latest.new" "$OUT/latest"

cd "$here"
rm -rf "$WORK"
printf 'version: %s\n' "$PKGVERSION"
printf '%s\n' "$UPSTREAM"
printf 'build-seconds: %s\n' "$(($(date +%s) - start))"
printf 'out: %s\n' "$dest"
