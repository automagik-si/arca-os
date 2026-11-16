#!/bin/sh
# Verify that repart follows the boot ESP rather than an identically named /usr on another disk.
set -eu

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
script=$here/../../../mkosi.packages/initrd-utils/initrd-repart.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"

cat > "$tmp/bin/findmnt" <<'EOF'
#!/bin/sh
for mountpoint; do :; done
[ "$mountpoint" = /boot ] || exit 99
[ "${NO_BOOT:-0}" = 0 ] || exit 2
printf '%s\n' "$TEST_ESP"
EOF
cat > "$tmp/bin/lsblk" <<'EOF'
#!/bin/sh
for device; do :; done
[ "$device" = "$TEST_ESP" ] || exit 99
printf '%s\n' "$TEST_DISK"
EOF
cat > "$tmp/bin/systemd-repart" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" > "$REPART_LOG"
exit "${REPART_STATUS:-0}"
EOF
chmod +x "$tmp/bin/"*
export PATH="$tmp/bin:$PATH" REPART_LOG="$tmp/repart.log"

for TEST_ESP in /dev/sdb1 /dev/nvme0n1p1; do
    case "$TEST_ESP" in
        /dev/sdb1) TEST_DISK=/dev/sdb ;;
        *) TEST_DISK=/dev/nvme0n1 ;;
    esac
    export TEST_ESP TEST_DISK
    sh "$script"
    printf '%s\n' --dry-run=no "$TEST_DISK" > "$tmp/expected"
    cmp "$tmp/expected" "$REPART_LOG"
    echo "PASS repart targets $TEST_DISK from $TEST_ESP"
done

status=0
REPART_STATUS=1 sh "$script" || status=$?
[ "$status" -eq 1 ]
echo 'PASS allocation failure remains a failure'

rm "$REPART_LOG"
status=0
TEST_DISK='' sh "$script" || status=$?
[ "$status" -ne 0 ] && [ ! -e "$REPART_LOG" ]
echo 'PASS missing parent never invokes automatic disk discovery'

status=0
NO_BOOT=1 sh "$script" || status=$?
[ "$status" -eq 2 ] && [ ! -e "$REPART_LOG" ]
echo 'PASS missing boot mount never invokes repart'
