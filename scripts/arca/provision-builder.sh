#!/usr/bin/env bash
# Provision a Debian 13 (trixie, amd64) machine as an Arca OS builder.
#
# Run as root. It is idempotent: running it again only installs what is
# missing or at the wrong version. It ends by printing one version line per
# tool.
#
# Pins (override by environment for a rebuild of an older setup):
#   MKOSI_REF       mkosi git tag            (default v27.1)
#   GO_SERIES       Go release series        (default 1.27; newest patch release is used)
#   GO_VERSION      exact Go version, e.g. go1.27.2 (default: newest of GO_SERIES)
#   RUST_TOOLCHAIN  rustup toolchain         (default 1.97.1)
set -euo pipefail

MKOSI_REF="${MKOSI_REF:-v27.1}"
GO_SERIES="${GO_SERIES:-1.27}"
GO_VERSION="${GO_VERSION:-}"
RUST_TOOLCHAIN="${RUST_TOOLCHAIN:-1.97.1}"
RUST_TARGET="x86_64-unknown-linux-musl"

export RUSTUP_HOME=/usr/local/rustup
export CARGO_HOME=/usr/local/cargo
export DEBIAN_FRONTEND=noninteractive

log() { printf 'provision: %s\n' "$*" >&2; }
die() { printf 'provision: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root"
# shellcheck disable=SC1091
. /etc/os-release
if [ "${ID:-}" != debian ] || [ "${VERSION_CODENAME:-}" != trixie ]; then
	die "needs Debian 13 (trixie), found ${PRETTY_NAME:-unknown}"
fi
[ "$(dpkg --print-architecture)" = amd64 ] || die "needs amd64"

umask 022
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# --- Debian packages -------------------------------------------------------
# Upstream build.yml (image build), Zabbly builds.yml (kernel build), and the
# Arca OS additions. build-essential, fakeroot and patch are needed by debuild
# and the kernel build; gdisk by scripts/convert-img-to-iso.sh (build-iso).
pkgs_upstream="binutils debian-archive-keyring devscripts libnbd-dev make mtools parted pigz pkgconf pipx qemu-utils zip"
pkgs_kernel="bc bison cpio debhelper dpkg-dev flex git kmod libdw-dev libelf-dev libssl-dev rsync zstd pahole"
pkgs_arca="equivs reprepro rclone gnupg efitools openssl jq curl musl-tools shellcheck sudo python3-requests ca-certificates file"
pkgs_extra="build-essential fakeroot patch gdisk xz-utils"

log "apt: updating package lists"
apt-get update -q
# Debian names the pahole package dwarves in some releases.
# shellcheck disable=SC2086
set -- $pkgs_upstream $pkgs_kernel $pkgs_arca $pkgs_extra
want=()
for p in "$@"; do
	if [ "$p" = pahole ] && ! apt-cache show pahole >/dev/null 2>&1; then
		p=dwarves
	fi
	want+=("$p")
done
log "apt: installing ${#want[@]} packages"
apt-get install -y -q "${want[@]}"

# --- mkosi -----------------------------------------------------------------
mkosi_want="${MKOSI_REF#v}"
if command -v mkosi >/dev/null 2>&1 && mkosi --version 2>/dev/null | grep -qw "$mkosi_want"; then
	log "mkosi: $mkosi_want already installed"
else
	log "mkosi: installing $MKOSI_REF with pipx (system-wide)"
	PIPX_HOME=/opt/pipx PIPX_BIN_DIR=/usr/local/bin PIPX_MAN_DIR=/usr/local/share/man \
		pipx install --force "git+https://github.com/systemd/mkosi.git@${MKOSI_REF}"
fi

# --- Go --------------------------------------------------------------------
log "go: reading the release list from go.dev"
curl -fsSL 'https://go.dev/dl/?mode=json&include=all' -o "$tmp/go.json"
if [ -z "$GO_VERSION" ]; then
	GO_VERSION=$(jq -r --arg s "go${GO_SERIES}." '.[] | select(.stable and (.version | startswith($s))) | .version' "$tmp/go.json" | sort -V | tail -1)
fi
[ -n "$GO_VERSION" ] || die "go: no stable release in series $GO_SERIES"
case "$GO_VERSION" in "go${GO_SERIES}."*) ;; *) die "go: $GO_VERSION is not in series $GO_SERIES" ;; esac
go_file=$(jq -r --arg v "$GO_VERSION" '.[] | select(.version == $v) | .files[] | select(.os == "linux" and .arch == "amd64" and .kind == "archive") | .filename' "$tmp/go.json")
go_sha=$(jq -r --arg v "$GO_VERSION" '.[] | select(.version == $v) | .files[] | select(.os == "linux" and .arch == "amd64" and .kind == "archive") | .sha256' "$tmp/go.json")
if [ -z "$go_file" ] || [ ${#go_sha} -ne 64 ]; then die "go: no linux-amd64 archive listed for $GO_VERSION"; fi
if [ -x /usr/local/go/bin/go ] && /usr/local/go/bin/go version | grep -q " ${GO_VERSION} "; then
	log "go: $GO_VERSION already installed"
else
	log "go: installing $GO_VERSION"
	curl -fsSL "https://go.dev/dl/${go_file}" -o "$tmp/$go_file"
	printf '%s  %s\n' "$go_sha" "$tmp/$go_file" | sha256sum -c --quiet - || die "go: sha256 mismatch for $go_file"
	rm -rf /usr/local/go
	tar -C /usr/local -xzf "$tmp/$go_file"
fi
ln -sfn /usr/local/go/bin/go /usr/local/bin/go
ln -sfn /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# --- Rust ------------------------------------------------------------------
if [ ! -x "$CARGO_HOME/bin/rustup" ]; then
	log "rust: installing rustup"
	base=https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-gnu
	curl -fsSL "$base/rustup-init" -o "$tmp/rustup-init"
	curl -fsSL "$base/rustup-init.sha256" -o "$tmp/rustup-init.sha256"
	printf '%s  %s\n' "$(cut -d' ' -f1 "$tmp/rustup-init.sha256")" "$tmp/rustup-init" | sha256sum -c --quiet - \
		|| die "rust: sha256 mismatch for rustup-init"
	chmod 0755 "$tmp/rustup-init"
	"$tmp/rustup-init" -y -q --no-modify-path --profile minimal --default-toolchain none
fi
log "rust: toolchain $RUST_TOOLCHAIN with $RUST_TARGET"
"$CARGO_HOME/bin/rustup" toolchain install "$RUST_TOOLCHAIN" --profile minimal \
	--component clippy --component rustfmt --target "$RUST_TARGET"
"$CARGO_HOME/bin/rustup" default "$RUST_TOOLCHAIN"
# Wrappers on the default PATH, so every user and every non-login shell finds
# the shared toolchain. Only rustup itself writes under CARGO_HOME.
for bin in "$CARGO_HOME"/bin/*; do
	name=$(basename "$bin")
	if [ "$name" = rustup ]; then
		extra="export CARGO_HOME=\"\${CARGO_HOME:-$CARGO_HOME}\""
	else
		extra=":"
	fi
	cat > "/usr/local/bin/$name" <<EOF
#!/bin/sh
export RUSTUP_HOME="\${RUSTUP_HOME:-$RUSTUP_HOME}"
$extra
exec "$bin" "\$@"
EOF
	chmod 0755 "/usr/local/bin/$name"
done

# --- Build directories -----------------------------------------------------
install -d -m 0755 /srv/work /srv/artifacts

# --- Version lines ---------------------------------------------------------
ver() { printf '%-22s %s\n' "$1:" "$2"; }
pkgver() { dpkg-query -W -f '${Version}' "$1" 2>/dev/null || echo missing; }
echo "--- toolchain ---"
ver mkosi "$(mkosi --version 2>&1 | head -1)"
ver go "$(go version)"
ver rustc "$(rustc "+$RUST_TOOLCHAIN" --version)"
ver cargo "$(cargo "+$RUST_TOOLCHAIN" --version)"
ver rust-target "$(rustup target list --installed --toolchain "$RUST_TOOLCHAIN" | grep -x "$RUST_TARGET" || echo missing)"
ver musl-gcc "$(command -v musl-gcc || echo missing) (musl-tools $(pkgver musl-tools))"
ver gcc "$(gcc --version | head -1)"
ver qemu-img "$(qemu-img --version | head -1)"
ver shellcheck "$(shellcheck --version | sed -n 's/^version: //p')"
ver efitools "$(command -v cert-to-efi-sig-list sign-efi-sig-list | tr '\n' ' ')($(pkgver efitools))"
ver equivs-build "$(command -v equivs-build) ($(pkgver equivs))"
ver debuild "$(command -v debuild) (devscripts $(pkgver devscripts))"
ver reprepro "$(reprepro --version 2>&1 | head -1)"
ver rclone "$(rclone version 2>/dev/null | head -1)"
ver pipx "$(pipx --version)"
ver python3 "$(python3 --version)"
ver openssl "$(openssl version)"
ver gpg "$(gpg --version | head -1)"
ver jq "$(jq --version)"
ver git "$(git --version)"
ver pahole "$(pahole --version 2>&1 | head -1)"
ver make "$(make --version | head -1)"
ver pigz "$(pigz --version 2>&1 | head -1)"
ver zip "$(zip -v | sed -n 2p | sed 's/^ *//')"
ver sgdisk "$(sgdisk --version | head -1)"
ver losetup "$(losetup --version)"
