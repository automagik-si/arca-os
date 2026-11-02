#!/usr/bin/env bash
# Print the iwlwifi firmware window table for a kernel source tarball.
#
#   wifi-fw-windows.sh <kernel-source.tar.xz>
#
# iwlwifi asks for <prefix>-<api>.ucode from the highest API it supports down
# to the lowest (iwl-drv.c, iwl_request_firmware). Older devices have a fixed
# prefix and their own range in cfg/*.c. Newer ones build the prefix at run
# time, iwlwifi-<mac>-<step>0-<rf>[4]-<step>0, from the hardware, and combine
# the MAC family's range (its iwl_family_base_params) with the RF's range
# (cfg/rf-<rf>.c, IWL_<RF>_UCODE_API_MIN/MAX, if it has one) by the rule of
# iwl_get_ucode_api_versions. So the table has
#   - one row per fixed prefix the driver declares with MODULE_FIRMWARE, its
#     window from the *_MAX macro of the declaration and the *_MIN macro of the
#     same name (iwlegacy's 3945 and 4965 are declared the same way);
#   - one row per MAC name of a family (the MACs of the prefixes defined in the
#     family's cfg file) and RF, iwlwifi-<mac>-?0-<rf>*-?0, steppings and the
#     CDB "4" being the hardware's: iwlwifi-bz-b0-wh-b0 loads in the bz family's
#     window though no file declares it.
# Every declared run-time prefix must fall in a row whose window has the
# declared API as its maximum, or the script fails. Rows read
#   iwlwifi <prefix glob> <api-min> <api-max>
# with core numbers as in the file names (c102), sorted and unique.
# mkosi.images/base/mkosi.conf.d/arca-wifi.drivers carries this output; run it
# again when the kernel changes.
#
# Exit codes: 0, 1 when a declaration cannot be resolved, 2 on usage.
set -euo pipefail

[ $# -eq 1 ] && [ -f "$1" ] || { sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
tar -xJf "$1" -C "$tmp" --wildcards '*/drivers/net/wireless/intel/iwlwifi/cfg/*.c' \
	'*/drivers/net/wireless/intel/iwlegacy/*.[ch]'
mapfile -t cfgs < <(find "$tmp" \( -path '*/intel/iwlwifi/cfg/*.c' -o -path '*/intel/iwlegacy/*.[ch]' \) -type f \
	| LC_ALL=C sort)
[ ${#cfgs[@]} -gt 0 ] || { echo "wifi-fw-windows: no iwlwifi cfg/*.c in $1" >&2; exit 1; }

# The names are read across all files at once (iwlegacy defines its prefixes
# in headers); a name defined twice with different values is an error.
# shellcheck disable=SC2016  # an awk program
LC_ALL=C awk '
function fail(msg) { printf "wifi-fw-windows: %s\n", msg > "/dev/stderr"; bad = 1 }
function def(tab, name, v) {
	if (tab == "s") { if ((name in str) && str[name] != v) fail(name " is defined twice"); str[name] = v }
	else { if ((name in num) && num[name] != v) fail(name " is defined twice"); num[name] = v }
}
# value <macro or ENCODE_CORE_AS_API(macro)>: the API number, cores as 1000 + core.
function value(x,   core) {
	core = sub(/^ENCODE_CORE_AS_API\(/, "", x); sub(/\)$/, "", x)
	if (!(x in num)) { fail("no number for " x); return 0 }
	return num[x] + (core ? 1000 : 0)
}
function shown(v) { return v >= 1000 ? "c" (v - 1000) : v }
function row(glob, lo, hi) { rows[glob] = lo " " hi }
# built <prefix>: the MAC of a run-time prefix, or "" for a fixed one; rfof: its RF.
function built(p,   n, part, i, m) {
	n = split(p, part, "-")
	if (n < 5 || part[1] != "iwlwifi" || part[n] !~ /^[a-z]0$/ || part[n - 2] !~ /^[a-z]0$/ || part[n - 1] !~ /^[a-z][a-z]4?$/) return ""
	m = part[2]; for (i = 3; i < n - 2; i++) m = m "-" part[i]
	return m
}
function rfof(p,   n, part) { n = split(p, part, "-"); return substr(part[n - 1], 1, 2) }
function declared(pre, maxname,   p, minname) {
	if (!(pre in str)) { fail("no string for " pre); return }
	p = str[pre]; sub(/-$/, "", p)
	minname = maxname; sub(/_MAX$/, "_MIN", minname)
	if (built(p) == "") row(p, value(minname), value(maxname))
	else { ndecl++; decl[ndecl] = p; declmax[ndecl] = value(maxname) }
}
FNR == 1 {
	line = ""; inbase = 0; file = FILENAME; sub(/.*\//, "", file)
	if (file ~ /^rf-[a-z][a-z]\.c$/) rfs[substr(file, 4, 2)] = 1
}
{
	# Join continued lines, so a macro is read as one line.
	if (sub(/\\$/, "")) { line = line $0 " "; next }
	$0 = line $0; line = ""
}
$1 == "#define" && $2 ~ /_FW_PRE$/ && $3 ~ /^"[^"]*"$/ && NF == 3 {
	s = $3; gsub(/"/, "", s); def("s", $2, s)
	if (file ~ /\.c$/ && built(s) != "") macs[file, built(s)] = 1
	next
}
$1 == "#define" && $2 ~ /_UCODE_(API|CORE)_(MIN|MAX)$/ && $3 ~ /^[0-9]+$/ && NF == 3 {
	def("n", $2, $3)
	if (file ~ /^rf-[a-z][a-z]\.c$/) { rf = substr(file, 4, 2); if ($2 ~ /_MIN$/) rfmin[rf] = $2; else rfmax[rf] = $2 }
	next
}
/struct iwl_family_base_params [a-z0-9_]+ = \{/ { inbase = 1; next }
inbase && /^};/ { inbase = 0; next }
inbase && $1 ~ /^\.ucode_api_(min|max)$/ { v = $3; sub(/,$/, "", v); if ($1 ~ /min/) basemin[file] = v; else basemax[file] = v; next }
# X(api) PRE "-" __stringify(api) ".ucode", X(api) PRE #api ".ucode", or X(api) Y(api)
$1 == "#define" && $2 ~ /_MODULE_FIRMWARE\(api\)$/ {
	m = $2; sub(/\(api\)$/, "", m)
	if ($0 ~ /(__stringify\(api\)|#api) "\.ucode"/) mf[m] = $3
	else if ($3 ~ /^[A-Z0-9_]+\(api\)$/ && NF == 3) { alias[m] = $3; sub(/\(api\)$/, "", alias[m]) }
	next
}
/^MODULE_FIRMWARE\([A-Z0-9_]+_MODULE_FIRMWARE\([A-Z0-9_]+\)\);/ {
	s = $0; sub(/^MODULE_FIRMWARE\(/, "", s); m = s; sub(/\(.*/, "", m)
	sub(/^[A-Z0-9_]+\(/, "", s); sub(/\).*/, "", s)
	nwant++; want[nwant] = m; wantmax[nwant] = s; next
}
/^(IWL_FW_AND_PNVM|IWL_CORE_FW)\([A-Z0-9_]+, *[A-Z0-9_]+\);/ {
	s = $0; core = s ~ /^IWL_CORE_FW/; sub(/^[A-Z_]+\(/, "", s); sub(/\).*/, "", s)
	split(s, a, /, */); nwant++; want[nwant] = "=" a[1]; wantmax[nwant] = core ? "ENCODE_CORE_AS_API(" a[2] ")" : a[2]
	next
}
END {
	for (i = 1; i <= nwant; i++) {
		m = want[i]
		if (m ~ /^=/) { declared(substr(m, 2), wantmax[i]); continue }
		for (k = 0; !(m in mf) && (m in alias) && k < 4; k++) m = alias[m]
		if (m in mf) declared(mf[m], wantmax[i]); else fail("no prefix macro for " want[i])
	}
	# Family MAC x RF, by iwl_get_ucode_api_versions.
	for (fm in macs) {
		split(fm, fk, SUBSEP)
		# Only a family file (one with a family range) names MACs; RF files declare prefixes, checked below.
		if (!(fk[1] in basemax)) continue
		bmin = value(basemin[fk[1]]); bmax = value(basemax[fk[1]])
		for (rf in rfs) {
			lo = bmin; hi = bmax
			if (rf in rfmax) {
				rmin = value(rfmin[rf]); rmax = value(rfmax[rf])
				if (bmin > rmax) { lo = rmin; hi = rmax }
				else if (rmin <= bmax) { lo = rmin > bmin ? rmin : bmin; hi = rmax < bmax ? rmax : bmax }
			}
			row("iwlwifi-" fk[2] "-?0-" rf "*-?0", lo, hi)
		}
	}
	for (i = 1; i <= ndecl; i++) {
		p = decl[i]; g = "iwlwifi-" built(p) "-?0-" rfof(p) "*-?0"
		if (!(g in rows)) { fail(p ": declared, but no row " g); continue }
		split(rows[g], w, " ")
		if (w[2] != declmax[i]) fail(p ": declared at " shown(declmax[i]) ", but its row ends at " shown(w[2]))
	}
	for (g in rows) { split(rows[g], w, " "); print "iwlwifi " g " " shown(w[1]) " " shown(w[2]) }
	exit bad
}
' "${cfgs[@]}" | LC_ALL=C sort -u
