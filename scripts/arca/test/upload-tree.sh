#!/usr/bin/env bash
# Exercise the upload against a temporary loopback S3 server, never a real bucket.
set +x
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
for tool in rclone python3 shellcheck; do command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 1; }; done
"$here/provision-rclone.sh" --check
shellcheck "$here/upload-tree.sh" "$here/provision-rclone.sh" "$0"
printf 'TOOLS %s; ShellCheck %s; %s\n' "$(rclone version | sed -n '1p')" \
	"$(shellcheck --version | sed -n 's/^version: //p')" "$(python3 --version)"
t=$(mktemp -d)
server=''
cleanup() {
	if [ -n "$server" ]; then kill "$server" 2>/dev/null || true; wait "$server" 2>/dev/null || true; fi
	rm -rf "$t"
}
trap cleanup EXIT
fail() { echo "FAIL $*" >&2; exit 1; }
pass() { echo "PASS $*"; }

export RCLONE_S3_ACCESS_KEY_ID RCLONE_S3_SECRET_ACCESS_KEY
RCLONE_S3_ACCESS_KEY_ID=$(python3 -c 'import secrets; print(secrets.token_hex(12))')
RCLONE_S3_SECRET_ACCESS_KEY=$(python3 -c 'import secrets; print(secrets.token_hex(24))')
unset RCLONE_S3_SESSION_TOKEN
export RCLONE_AUTH_KEY="\"$RCLONE_S3_ACCESS_KEY_ID,$RCLONE_S3_SECRET_ACCESS_KEY\""
export UPLOAD_TEST_DIR=$t UPLOAD_REAL_RCLONE
UPLOAD_REAL_RCLONE=$(command -v rclone)
mkdir -p "$t/store/updates/channel" "$t/tree/202601010001/x86_64" "$t/tree/keys" "$t/bin"
"$UPLOAD_REAL_RCLONE" serve s3 "$t/store" --config /dev/null --addr 127.0.0.1:0 \
	--dir-cache-time 0 --log-level INFO >"$t/server.log" 2>&1 &
server=$!
for ((i=0; i<100; i++)); do
	if grep -q 'Serving S3' "$t/server.log"; then break; fi
	kill -0 "$server" 2>/dev/null || fail "local S3 server stopped"
	sleep 0.1
done
endpoint=$(sed -nE 's/.*(http:\/\/127\.0\.0\.1:[0-9]+).*/\1/p' "$t/server.log" | head -1)
[ -n "$endpoint" ] || fail "local S3 server did not start"
unset RCLONE_AUTH_KEY
printf 'bucket=updates\nendpoint=%s\nregion=auto\nprefix=channel\n' "$endpoint" >"$t/config"
printf 'release-one\n' >"$t/tree/202601010001/x86_64/arca.raw.gz"
printf 'public certificate\n' >"$t/tree/keys/update.crt"
printf '{"updates":[{"version":"202601010001"}]}\n' >"$t/tree/index.json"
printf 'signed index one\n' >"$t/tree/index.sjson"
printf 'outside prefix\n' >"$t/store/updates/unrelated"

# Observe phase boundaries while all transfers still use the real S3 client.
cat >"$t/bin/rclone" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
t=$UPLOAD_TEST_DIR
args=" $* "
case "$args" in
*' copy '*|*' check '*|*' sync '*) ;;
*) exit 1 ;;
esac
if [[ "$args" == *' copy '* && "$args" == *' --include '* ]]; then
	while IFS= read -r -d '' f; do
		rel=${f#"$t/tree/"}
		case "$rel" in index.json|index.sjson) continue ;; esac
		cmp "$f" "$t/store/updates/channel/$rel"
	done < <(find "$t/tree" -type f -print0)
	echo indexes >>"$t/events"
fi
if [[ "$args" == *' check '* && "$args" == *' --download '* && "$args" != *' --exclude '* ]]; then
	echo readback >>"$t/events"
	if [ -f "$t/corrupt" ]; then printf 'broken!!!!!\n' >"$t/store/updates/channel/202601010001/x86_64/arca.raw.gz"; fi
fi
if [[ "$args" == *' sync '* && "$args" != *' --dry-run '* ]]; then
	cmp "$t/tree/index.json" "$t/store/updates/channel/index.json"
	cmp "$t/tree/index.sjson" "$t/store/updates/channel/index.sjson"
	grep -q readback "$t/events"
	echo prune >>"$t/events"
fi
exec "$UPLOAD_REAL_RCLONE" "$@"
EOF
chmod +x "$t/bin/rclone"
export PATH=$t/bin:$PATH
upload() { "$here/upload-tree.sh" --tree "$t/tree" --config "$t/config" "$@"; }
run() {
	upload "$@" >"$t/run.log" 2>&1 || fail "upload failed"
	cat "$t/run.log" >>"$t/output.log"
}
refused() {
	local result=0
	"$@" >"$t/run.log" 2>&1 || result=$?
	[ "$result" -eq 2 ] || fail "expected refusal with exit 2"
	cat "$t/run.log" >>"$t/output.log"
}

run
cmp "$t/tree/index.sjson" "$t/store/updates/channel/index.sjson"
[ "$(cat "$t/events")" = $'indexes\nreadback\nprune' ] || fail "phase order"
pass 'clean upload, payload before indexes, readback before pruning'
cp -a "$t/store" "$t/unchanged"
python3 - "$t/store" >"$t/before" <<'EOF'
import pathlib
import sys
for p in sorted(pathlib.Path(sys.argv[1]).rglob('*')):
    if p.is_file():
        s = p.stat()
        print(p, s.st_ino, s.st_mtime_ns, s.st_ctime_ns)
EOF
run
python3 - "$t/store" >"$t/after" <<'EOF'
import pathlib
import sys
for p in sorted(pathlib.Path(sys.argv[1]).rglob('*')):
    if p.is_file():
        s = p.stat()
        print(p, s.st_ino, s.st_mtime_ns, s.st_ctime_ns)
EOF
cmp "$t/before" "$t/after" || fail "idempotent run rewrote objects"
diff -r "$t/unchanged" "$t/store" || fail "idempotence"
pass 'second run is idempotent'

mkdir -p "$t/tree/202601010002"
printf 'new release\n' >"$t/tree/202601010002/update.sjson"
printf 'release-two\n' >"$t/tree/202601010001/x86_64/arca.raw.gz"
printf '{"updates":[{"version":"202601010002"}]}\n' >"$t/tree/index.json"
printf 'signed index two\n' >"$t/tree/index.sjson"
printf 'obsolete\n' >"$t/store/updates/channel/stale"
rm "$t/tree/keys/update.crt"
rmdir "$t/tree/keys"
run --dry-run
diff -r --exclude stale "$t/unchanged" "$t/store" || fail "dry-run wrote data"
grep -q '202601010002/update.sjson' "$t/run.log" || fail "dry-run omitted upload"
grep -q 'stale' "$t/run.log" || fail "dry-run omitted deletion"
pass 'dry-run lists changes and writes nothing'
run
[ ! -e "$t/store/updates/channel/stale" ] || fail "stale object survived"
[ ! -e "$t/store/updates/channel/keys/update.crt" ] || fail "stale key survived"
[ -f "$t/store/updates/unrelated" ] || fail "prefix isolation"
diff -r "$t/tree" "$t/store/updates/channel" || fail "changed tree differs"
pass 'changed tree, stale-file removal, prefix isolation and verified readback'

printf 'obsolete\n' >"$t/store/updates/channel/stale"
touch "$t/corrupt"
result=0
upload >"$t/run.log" 2>&1 || result=$?
[ "$result" -eq 1 ] || fail "readback corruption did not fail"
grep -q 'readback verification mismatch' "$t/run.log" || fail "missing verification error"
[ -f "$t/store/updates/channel/stale" ] || fail "pruned despite failed verification"
cat "$t/run.log" >>"$t/output.log"
rm "$t/corrupt"
pass 'same-size readback corruption fails and preserves stale files'
run

refused "$here/upload-tree.sh" --tree "$t/tree" --config "$t/missing"
printf 'bucket=updates\n' >"$t/partial"
refused "$here/upload-tree.sh" --tree "$t/tree" --config "$t/partial"
refused env -u RCLONE_S3_ACCESS_KEY_ID "$here/upload-tree.sh" --tree "$t/tree" --config "$t/config"
refused env -u RCLONE_S3_SECRET_ACCESS_KEY "$here/upload-tree.sh" --tree "$t/tree" --config "$t/config"
pass 'missing/partial config and either missing credential are refused'

# Hostile ambient rclone settings must not enable dumps or suppress transfers.
printf 'ambient settings check\n' >"$t/tree/202601010002/notes.json"
RCLONE_DUMP=auth RCLONE_LOG_LEVEL=DEBUG RCLONE_DRY_RUN=true run
cmp "$t/tree/202601010002/notes.json" "$t/store/updates/channel/202601010002/notes.json"
python3 - "$t" <<'EOF'
import os
import pathlib
import sys
root = pathlib.Path(sys.argv[1])
for path in (root / 'output.log', root / 'run.log', root / 'server.log'):
    data = path.read_bytes()
    for key in ('RCLONE_S3_ACCESS_KEY_ID', 'RCLONE_S3_SECRET_ACCESS_KEY'):
        if os.environ[key].encode() in data:
            raise SystemExit('FAIL credential appeared in output')
EOF
pass 'credentials never appear in output, including with ambient debug settings'
