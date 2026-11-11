#!/usr/bin/env bash
# Check, without a VM, that dev-vm.sh pushes every script builder provisioning runs there.
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
fail() { echo "FAIL $*" >&2; exit 1; }
pass() { echo "PASS $*"; }

# check <dir>: print what is wrong with the pushes in <dir>/dev-vm.sh, nothing when sound.
check() {
	local dir=$1 pushed run name dest ref
	# shellcheck disable=SC2016  # literal text of dev-vm.sh
	pushed=$(sed -nE 's/^[[:space:]]*incus file push .*"\$HERE\/([A-Za-z0-9._-]+)" "\$VM\/root\/([A-Za-z0-9._-]+)"$/\1 \2/p' "$dir/dev-vm.sh")
	[ -n "$pushed" ] || { echo "dev-vm.sh pushes no script"; return 0; }
	while read -r name dest; do
		[ "$name" = "$dest" ] || echo "$name is pushed as $dest"
		[ -f "$dir/$name" ] || { echo "$name is pushed but does not exist"; continue; }
		# Any sibling script named outside a comment has to be in the VM too.
		while read -r ref; do
			[ -f "$dir/$ref" ] || continue
			printf '%s\n' "$pushed" | grep -qx "$ref $ref" || echo "$name names $ref, which dev-vm.sh does not push"
		done < <(grep -v '^[[:space:]]*#' "$dir/$name" | grep -oE '[A-Za-z0-9._-]+\.sh' | sort -u)
	done <<<"$pushed"
	run=$(sed -nE 's/^[[:space:]]*incus exec .* -- bash \/root\/([A-Za-z0-9._-]+)$/\1/p' "$dir/dev-vm.sh")
	printf '%s\n' "$pushed" | grep -qx "$run $run" || echo "dev-vm.sh runs '$run' in the VM without pushing it"
}

problems=$(check "$here")
[ -z "$problems" ] || fail "$problems"
pass 'dev-vm.sh pushes the provisioning script and every sibling script it names'

# The same check on a copy without the helper push: provisioning would stop with exit 127.
cp "$here"/provision-*.sh "$t/"
# shellcheck disable=SC2016  # literal text of dev-vm.sh
grep -v 'provision-rclone.sh" "\$VM' "$here/dev-vm.sh" >"$t/dev-vm.sh"
problems=$(check "$t")
[ "$problems" = 'provision-builder.sh names provision-rclone.sh, which dev-vm.sh does not push' ] ||
	fail "a helper that is not pushed was not caught: ${problems:-no finding}"
pass 'a helper that is not pushed is caught'
