#!/usr/bin/env bash
# Exercise the upload against a temporary loopback S3 server, never a real bucket.
set +x
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
for tool in rclone python3 shellcheck jq flock; do command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 1; }; done
"$here/provision-rclone.sh" --check
shellcheck "$here/upload-tree.sh" "$here/provision-rclone.sh" "$here/test/provision-push.sh" "$0"
"$here/test/provision-push.sh"
printf 'TOOLS %s; ShellCheck %s; %s\n' "$(rclone version | sed -n '1p')" \
	"$(shellcheck --version | sed -n 's/^version: //p')" "$(python3 --version)"
t=$(mktemp -d)
server=''
cleanup() {
	if [ -n "$server" ]; then kill "$server" 2>/dev/null || true; wait "$server" 2>/dev/null || true; fi
	rm -rf "$t"
}
trap cleanup EXIT
fail() {
	echo "FAIL $*" >&2
	if [ -s "$t/run.log" ]; then sed 's/^/  last output: /' "$t/run.log" >&2; fi
	exit 1
}
pass() { echo "PASS $*"; }

export RCLONE_S3_ACCESS_KEY_ID RCLONE_S3_SECRET_ACCESS_KEY
RCLONE_S3_ACCESS_KEY_ID=$(python3 -c 'import secrets; print(secrets.token_hex(12))')
RCLONE_S3_SECRET_ACCESS_KEY=$(python3 -c 'import secrets; print(secrets.token_hex(24))')
unset RCLONE_S3_SESSION_TOKEN
export RCLONE_AUTH_KEY="\"$RCLONE_S3_ACCESS_KEY_ID,$RCLONE_S3_SECRET_ACCESS_KEY\""
export UPLOAD_TEST_DIR=$t UPLOAD_REAL_RCLONE
# The upload's locks live under the cache directory; keep them out of the real one.
export XDG_CACHE_HOME=$t/cache
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
# write_index <version/filename>...: an index listing those files with their sizes, renamed into place.
# sign_index: this proof generates no keys, so its signed index is a marked copy (see the openssl stand-in).
sign_index() {
	{ echo 'stand-in signature over:'; cat "$t/tree/index.json"; } >"$t/sjson.new"
	mv "$t/sjson.new" "$t/tree/index.sjson"
}
write_index() {
	local p
	for p in "$@"; do
		jq -n --arg v "${p%%/*}" --arg f "${p#*/}" --argjson s "$(stat -c %s "$t/tree/$p")" \
			'{version: $v, files: [{filename: $f, size: $s}]}'
	done | jq -s '{updates: .}' >"$t/index.new"
	mv "$t/index.new" "$t/tree/index.json"
	sign_index
}
write_index 202601010001/x86_64/arca.raw.gz
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
# The source rclone was given: the upload's snapshot, never the tree itself.
src='' prev=''
for a in "$@"; do
	case "$prev" in copy|check|sync) src=$a ;; esac
	prev=$a
done
echo "$src" >>"$t/sources"
# Change the tree once the upload has started, the way a publishing run would.
if [[ "$args" == *' copy '* && "$args" == *' --exclude '* && -f "$t/mutate" ]]; then
	rm "$t/mutate"
	mkdir "$t/tree/202601010003"
	printf 'late release\n' >"$t/tree/202601010003/late.json"
	printf '{"updates":[{"version":"202601010003","files":[{"filename":"late.json","size":13}]}]}\n' >"$t/index.late"
	mv "$t/index.late" "$t/tree/index.json"
	{ echo 'stand-in signature over:'; cat "$t/tree/index.json"; } >"$t/sjson.late"
	mv "$t/sjson.late" "$t/tree/index.sjson"
	rm "$t/tree/202601010002/pending.json"
fi
if [[ "$args" == *' copy '* && "$args" == *' --include '* ]]; then
	while IFS= read -r -d '' f; do
		rel=${f#"$src/"}
		case "$rel" in index.json|index.sjson) continue ;; esac
		cmp "$f" "$t/store/updates/channel/$rel"
	done < <(find "$src" -type f -print0)
	echo indexes >>"$t/events"
fi
if [[ "$args" == *' check '* && "$args" == *' --download '* && "$args" != *' --exclude '* ]]; then
	echo readback >>"$t/events"
	if [ -f "$t/corrupt" ]; then printf 'broken!!!!!\n' >"$t/store/updates/channel/202601010001/x86_64/arca.raw.gz"; fi
fi
if [[ "$args" == *' sync '* && "$args" != *' --dry-run '* ]]; then
	cmp "$src/index.json" "$t/store/updates/channel/index.json"
	cmp "$src/index.sjson" "$t/store/updates/channel/index.sjson"
	grep -q readback "$t/events"
	echo prune >>"$t/events"
fi
# A slow last phase: on TERM it takes a second to end, then records whether its source is still there.
if [[ "$args" == *' sync '* && "$args" != *' --dry-run '* && -f "$t/slow" ]]; then
	rm "$t/slow"
	sleep 30 &
	nap=$!
	ended() {
		kill "$nap" 2>/dev/null || true
		sleep 1
		if [ -s "$src/index.json" ] && [ -s "$src/index.sjson" ]; then echo intact >"$t/slow.end"; else echo gone >"$t/slow.end"; fi
		exit 143
	}
	trap ended TERM
	echo "$$" >"$t/slow.pid"
	wait "$nap" || true
	exit 1
fi
exec "$UPLOAD_REAL_RCLONE" "$@"
EOF
chmod +x "$t/bin/rclone"
# Stand-in for `openssl smime -verify`: it hands back what sign_index marked, and fails on anything else.
cat >"$t/bin/openssl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-} ${2:-}" = 'smime -verify' ] || exit 1
in='' out=''
while [ $# -gt 0 ]; do
	case "$1" in
	-in) in=$2; shift 2 ;;
	-out) out=$2; shift 2 ;;
	*) shift ;;
	esac
done
[ "$(sed -n '1p' "$in")" = 'stand-in signature over:' ] || exit 1
sed '1d' "$in" >"$out"
EOF
chmod +x "$t/bin/openssl"
# publish-update.sh must stop at the upload lock; were it to go on, it finds only this.
printf '#!/bin/sh\nexit 1\n' >"$t/bin/incus"
chmod +x "$t/bin/incus"
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
said() { grep -qF -- "$1" "$t/run.log" || fail "expected message: $1"; }

run
cmp "$t/tree/index.sjson" "$t/store/updates/channel/index.sjson"
[ "$(cat "$t/events")" = $'indexes\nreadback\nprune' ] || fail "phase order"
said "destination updates/channel at $endpoint"
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
write_index 202601010002/update.sjson 202601010001/x86_64/arca.raw.gz
printf 'obsolete\n' >"$t/store/updates/channel/stale"
rm "$t/tree/keys/update.crt"
rmdir "$t/tree/keys"
run --dry-run
diff -r --exclude stale "$t/unchanged" "$t/store" || fail "dry-run wrote data"
grep -q '202601010002/update.sjson' "$t/run.log" || fail "dry-run omitted upload"
grep -q 'stale' "$t/run.log" || fail "dry-run omitted deletion"
said "destination updates/channel at $endpoint"
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
grep -q '^upload-tree: rclone: .*arca\.raw\.gz' "$t/run.log" || fail "rclone's error did not name the file"
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

# The config names the destination exactly: a prefix, or the whole bucket asked for by name.
config_case() {
	printf '%s\n' "$@" >"$t/case.conf"
	refused "$here/upload-tree.sh" --tree "$t/tree" --config "$t/case.conf"
}
base=(bucket=updates "endpoint=$endpoint" region=auto)
rm -rf "$t/unchanged"
cp -a "$t/store" "$t/unchanged"
config_case "${base[@]}" prefix=
said 'config key prefix has no value'
config_case "${base[@]}"
said 'config requires prefix'
config_case "${base[@]}" prefix=channel mirror_bucket_root=yes
said 'config sets both prefix and mirror_bucket_root'
config_case "${base[@]}" mirror_bucket_root=true
said 'mirror_bucket_root accepts only yes'
for bad in /channel channel/ 'chan nel' 'channel?x'; do
	config_case "${base[@]}" "prefix=$bad"
	said 'invalid prefix'
done
config_case "${base[@]}" prefix=channel/../other
said 'prefix must not contain dot segments'
config_case "${base[@]}" prefix=channel secret=value
said 'unknown config key'
config_case "${base[@]}" prefix=channel prefix=other
said 'duplicate config key prefix'
config_case "${base[@]}" bucket=other prefix=channel
said 'duplicate config key bucket'
printf '%s\n' "${base[@]}" mirror_bucket_root=yes >"$t/root.conf"
"$here/upload-tree.sh" --tree "$t/tree" --config "$t/root.conf" --dry-run >"$t/run.log" 2>&1 || fail "whole-bucket dry-run failed"
cat "$t/run.log" >>"$t/output.log"
said "destination updates (the whole bucket) at $endpoint"
grep -q 'unrelated' "$t/run.log" || fail "whole-bucket dry-run omitted the deletion outside the prefix"
diff -r "$t/unchanged" "$t/store" || fail "a refused config or a dry-run wrote data"
pass 'empty, missing, bad or duplicate prefix and unknown keys are refused; the whole bucket only by mirror_bucket_root=yes'

# Refused trees: the builder's own, by name or through a link, and one that another user publishes.
refused "$here/upload-tree.sh" --tree /srv/artifacts/updates --config "$t/config"
said "is the builder's own tree"
if [ -d /srv/artifacts ]; then
	ln -s /srv/artifacts "$t/builder-link"
	refused "$here/upload-tree.sh" --tree "$t/builder-link" --config "$t/config"
	said "/srv/artifacts is the builder's own tree"
	rm "$t/builder-link"
fi
if [ "$(id -u)" -eq 0 ]; then
	chown nobody "$t/tree"
	refused upload
	said 'the tree belongs to another user'
	chown 0 "$t/tree"
	pass "the builder's own tree and another user's tree are refused"
else
	pass "the builder's own tree is refused (another user's tree: not checked, this run is not root)"
fi

# Wrong credentials: the server refuses, so the variables really reach rclone; its error shows, they do not.
wrong=$(python3 -c 'import secrets; print(secrets.token_hex(24))')
for name in RCLONE_S3_ACCESS_KEY_ID RCLONE_S3_SECRET_ACCESS_KEY; do
	result=0
	env "$name=$wrong" RCLONE_DUMP=auth,headers RCLONE_LOG_LEVEL=DEBUG \
		"$here/upload-tree.sh" --tree "$t/tree" --config "$t/config" >"$t/run.log" 2>&1 || result=$?
	[ "$result" -eq 1 ] || fail "a wrong $name was not refused by the server"
	said 'release/key upload failed; indexes unchanged'
	grep -Eq '^upload-tree: rclone: .*(403|Forbidden|SignatureDoesNotMatch|InvalidAccessKeyId|AccessDenied)' "$t/run.log" ||
		fail "rclone's error did not show the refusal"
	if grep -qF "$wrong" "$t/run.log"; then fail "a credential appeared in the error output"; fi
	cat "$t/run.log" >>"$t/output.log"
done
diff -r "$t/unchanged" "$t/store" || fail "wrong credentials wrote data"
pass 'wrong credentials are refused by the server; the error names the refusal and no credential'

# A publishing run or another upload in progress: refuse, write nothing, upload once they are done.
printf 'held back\n' >"$t/tree/202601010002/pending.json"
write_index 202601010002/update.sjson 202601010002/pending.json 202601010001/x86_64/arca.raw.gz
rm -rf "$t/unchanged"
cp -a "$t/store" "$t/unchanged"
mkdir -p "$XDG_CACHE_HOME"
exec 9>>"$XDG_CACHE_HOME/arca-autopublish.lock"
flock -n 9 || fail "cannot hold the autopublish lock"
refused upload
said 'an automatic publishing run holds'
refused upload --dry-run
said 'an automatic publishing run holds'
exec 9>&-
exec 9>>"$XDG_CACHE_HOME/arca-upload-tree.lock"
flock -n 9 || fail "cannot hold the upload lock"
refused upload
said 'another upload holds'
ARCA_BUILDER_LOCK_HELD=1 refused "$here/publish-update.sh" --version 202601010009 --serve-dir "$t/tree"
said 'an upload of the tree holds the upload lock'
exec 9>&-
diff -r "$t/unchanged" "$t/store" || fail "a refused upload wrote data"
run
cmp "$t/tree/202601010002/pending.json" "$t/store/updates/channel/202601010002/pending.json"
pass 'a held autopublish lock or upload lock refuses and writes nothing, publish-update.sh too; released, the upload runs'

# Plain HTTP only for a loopback host; a missing tree stops the accepted ones before any network use.
endpoint_case() {
	printf 'bucket=updates\nendpoint=%s\nregion=auto\nprefix=channel\n' "$1" >"$t/endpoint.conf"
	refused "$here/upload-tree.sh" --tree "$t/absent" --config "$t/endpoint.conf"
}
for e in http://example.com http://example.com:9000 http://localhost.example.com http://127.0.0.1.example.com; do
	endpoint_case "$e"
	said 'plain HTTP only for a loopback host'
done
for e in https://example.com http://127.0.0.1:9000 http://localhost:9000 'http://[::1]:9000'; do
	endpoint_case "$e"
	said -- '--tree must be a directory'
done
printf 'bucket=updates\nendpoint=http://localhost:%s\nregion=auto\nprefix=channel\n' "${endpoint##*:}" >"$t/localhost.conf"
"$here/upload-tree.sh" --tree "$t/tree" --config "$t/localhost.conf" >"$t/run.log" 2>&1 || fail "upload through http://localhost failed"
cat "$t/run.log" >>"$t/output.log"
pass 'http is refused for other hosts and accepted for 127.0.0.1, localhost and [::1]; https is accepted'

# An index that lists a file the tree lacks, or with another size, is refused before any write.
rm -rf "$t/unchanged"
cp -a "$t/store" "$t/unchanged"
cp "$t/tree/index.json" "$t/index.good"
jq '.updates[0].files += [{filename: "absent.raw.gz", size: 1}]' "$t/index.good" >"$t/index.new"
mv "$t/index.new" "$t/tree/index.json"
refused upload
said 'index.sjson does not carry index.json'
sign_index
refused upload
said 'missing or have another size: "202601010002/absent.raw.gz"'
refused upload --dry-run
jq '.updates[0].files[0].size += 1' "$t/index.good" >"$t/index.new"
mv "$t/index.new" "$t/tree/index.json"
sign_index
refused upload
said 'missing or have another size: "202601010002/update.sjson"'
mv "$t/index.good" "$t/tree/index.json"
printf 'not a signed index\n' >"$t/sjson.new"
mv "$t/sjson.new" "$t/tree/index.sjson"
refused upload
said 'index.sjson is not a readable signed index'
diff -r "$t/unchanged" "$t/store" || fail "a refused index wrote data"
sign_index
pass 'an index.sjson that does not carry index.json, a listed file that is missing or of another size: refused, nothing written'

# A tree that changes after the snapshot: this run uploads the snapshot, the next one the new tree.
printf 'another file\n' >"$t/tree/202601010002/more.json"
write_index 202601010002/update.sjson 202601010002/pending.json 202601010002/more.json 202601010001/x86_64/arca.raw.gz
cp -a "$t/tree" "$t/expected"
touch "$t/mutate"
run
[ ! -e "$t/mutate" ] || fail "the tree was not changed during the upload"
[ -d "$t/tree/202601010003" ] || fail "the tree was not changed during the upload"
diff -r "$t/expected" "$t/store/updates/channel" || fail "the upload followed changes made after the snapshot"
if grep -qx "$t/tree" "$t/sources"; then fail "a phase read the tree instead of the snapshot"; fi
[ -z "$(find "$t" -maxdepth 1 -name '.upload-tree.*')" ] || fail "a snapshot was left behind"
run
diff -r "$t/tree" "$t/store/updates/channel" || fail "the next run did not upload the changed tree"
pass 'a tree changed after the snapshot does not change what is uploaded; no snapshot is left behind'

# A snapshot left by a killed upload is removed after a day, and said; a recent one is left alone.
mkdir "$t/.upload-tree.killed" "$t/.upload-tree.recent"
touch -d '2 days ago' "$t/.upload-tree.killed"
run
said "removing a snapshot left by a killed upload more than a day ago: $t/.upload-tree.killed"
[ ! -e "$t/.upload-tree.killed" ] || fail "an old leftover snapshot survived"
[ -d "$t/.upload-tree.recent" ] || fail "a recent snapshot was removed"
rmdir "$t/.upload-tree.recent"
pass 'a snapshot left more than a day ago is removed and reported; a recent one is kept'

# TERM to the script alone during the last phase: rclone is stopped and waited for, then the snapshot goes.
rm -rf "$t/unchanged"
cp -a "$t/store" "$t/unchanged"
touch "$t/slow"
"$here/upload-tree.sh" --tree "$t/tree" --config "$t/config" >"$t/run.log" 2>&1 &
up=$!
for ((i=0; i<300; i++)); do
	if [ -s "$t/slow.pid" ]; then break; fi
	kill -0 "$up" 2>/dev/null || fail "the upload ended before its last phase"
	sleep 0.1
done
[ -s "$t/slow.pid" ] || fail "the slow stand-in never started"
kill -TERM "$up"
result=0
wait "$up" || result=$?
cat "$t/run.log" >>"$t/output.log"
[ "$result" -eq 1 ] || fail "a stopped upload must exit 1"
said 'stopped by a signal'
[ "$(cat "$t/slow.end" 2>/dev/null)" = intact ] || fail "the snapshot was removed while rclone was still running"
[ -z "$(find "$t" -maxdepth 1 -name '.upload-tree.*')" ] || fail "a stopped upload left its snapshot"
diff -r "$t/unchanged" "$t/store" || fail "a stopped upload changed the remote tree"
pass 'TERM during the last phase: rclone ends before the snapshot is removed, the remote tree is unchanged'

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
