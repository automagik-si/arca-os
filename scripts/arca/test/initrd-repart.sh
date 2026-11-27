#!/bin/sh
# Verify that repart follows the boot ESP rather than an identically named /usr on another disk.
set -eu

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
script=$here/../../../mkosi.packages/initrd-utils/initrd-repart.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin" "$tmp/efivars"
variable=$tmp/efivars/LoaderDevicePartUUID-4a67b082-0a4c-41cf-b6c7-440b29bb8c4f
TEST_UUID=01234567-89ab-cdef-0123-456789abcdef
export TEST_UUID ARCA_BOOT_EFIVARS="$tmp/efivars"

write_variable() {
    printf '\006\000\000\000' > "$variable"
    printf '%s\000' "$1" | iconv -f ASCII -t UTF-16LE >> "$variable"
}

write_variable "$(printf '%s' "$TEST_UUID" | tr 'a-f' 'A-F')"

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
case " $* " in
    *" --output PARTUUID "*) printf '%s\n' "$TEST_UUID" ;;
    *" --output PKNAME "*) printf '%s\n' "$TEST_DISK" ;;
    *) exit 99 ;;
esac
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

rm "$variable"
for fixture in missing-loader-uuid different-loader-uuid invalid-loader-uuid truncated-loader-uuid; do
    case "$fixture" in
        different-loader-uuid) write_variable 11111111-2222-3333-4444-555555555555 ;;
        invalid-loader-uuid) write_variable not-a-uuid ;;
        truncated-loader-uuid)
            write_variable "$TEST_UUID"
            printf '\001' >> "$variable"
            ;;
    esac
    status=0
    sh "$script" || status=$?
    if [ "$status" -eq 0 ] || [ -e "$REPART_LOG" ]; then
        echo "FAIL $fixture allowed repart on a label-fallback ESP"
        exit 1
    fi
    echo "PASS $fixture never invokes repart on a label-fallback ESP"
done
