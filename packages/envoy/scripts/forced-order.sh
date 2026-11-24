#!/usr/bin/env bash
# packages/envoy/scripts/forced-order.sh [-C <module dir>] [-server 2.15.1|2.10.29] [-drop <hold point>]... [-timeout <duration>] <package>...
#
# Runs the forced-order tests of each package on the hooked nats-server: every test named
# TestForcedOrder…, built with -tags odatrace and -race, against a nats-server whose own code
# carries the hold points internal/testnats/hooks.go drives. The hooks are no fork: the script
# copies nats-server's module at that version out of the module cache, applies
# packages/envoy/internal/testnats/testdata/hooked/v<server>/server.patch to the copy, adds the
# shared testdata/hooked/zz_measure.go and that version's zz_measure_write.go to its server
# package, and builds the tests against the copy through
# a replace directive (go refuses an -overlay of a file in the module cache). A module of the
# workspace (go.work) builds in a copy of the workspace file, with absolute `use` paths and the
# replace added, so every other module version is the one CI's own build of it links, on either
# nats-server (2.10.29 adds go.uber.org/automaxprocs, which it requires, and changes no other
# version). A workspace that does not use the module, or that holds a replace, is refused, since
# the build would then link other module versions than CI's, and so is a module whose go.mod
# replaces nats-server, whose hooked copy would take the replacement's place. A module with no
# workspace builds on a copy of its go.mod with the replace added (go test -modfile, which go
# refuses in a workspace).
# The script reads go's JSON (go work edit -json, go mod download -json) with jq. Nothing else of
# nats-server changes, and no
# build without this script sees a hook.
#
#   -C <module dir>  the Go module to test, as `go -C` takes it (relative to the caller's
#                    directory); packages/envoy by default. The packages are relative to it.
#   -server <v>      the nats-server the tests link; by default the version the build links
#                    (2.15.1 today, the hub's), and 2.10.29 is the other hooked version. A version
#                    testdata/hooked has no directory for is refused, so a module that moves to a
#                    new nats-server fails here until its hooks are ported, and so is an empty one,
#                    so a step written `-server "$V"` with V unset does not test the default.
#   -drop <point>    removes the hook calls naming that point, exactly, from the patched copy
#                    before the build, so the point is never reached: the control that a test
#                    holding there fails without it. A name no hook call of that version's hooks
#                    passes is refused before the build, so a mistyped point cannot "fail" a test
#                    on a compile error instead. Repeatable.
#   -timeout <d>     go test's -timeout, in place of its 10 minutes (go's duration syntax: 25m).
#                    An empty value is refused, so a step written `-timeout "$T"` with T unset does
#                    not take the default.
#
# It fails, naming the package, when a package it is given declares no TestForcedOrder test, or
# runs none (every one it declares skipped), so a step that names a package whose tests were
# renamed, moved or skipped goes red rather than green with nothing held; such a package's `ok`
# line is printed as a FAIL line. -timeout, else GO_TEST_TIMEOUT, replaces go test's 10-minute
# -timeout, which the CI jobs that run this script count in their time budgets; a CI step gives the
# flag, since a GO* name in a step's env leaves its job's go commands unreduced (ci_filter_coverage.py's
# ENV_ALLOWED). The copies of go.mod and go.sum, or of
# go.work and go.work.sum, may gain the checksums the build needs; the repository's own files are
# never written. scripts/forced-order.test.sh holds it to these refusals.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
envoy="$(dirname "$here")"
hooked="$envoy/internal/testnats/testdata/hooked"
nats_server='github.com/nats-io/nats-server/v2'

die() {
  echo "forced-order.sh: $*" >&2
  exit 1
}

usage() {
  echo "usage: forced-order.sh [-C <module dir>] [-server 2.15.1|2.10.29] [-drop <hold point>]... [-timeout <duration>] <package>..." >&2
  exit 2
}

module="$envoy"
server=""
drops=()
while [ $# -gt 0 ]; do
  case "$1" in
    -C)
      [ $# -ge 2 ] || usage
      [ -n "$2" ] || die "-C '' names no module directory"
      module="$(cd "$2" && pwd)" || die "no module directory $2"
      shift 2
      ;;
    -server)
      [ $# -ge 2 ] || usage
      [ -n "$2" ] || die "-server '' names no version: give 2.15.1 or 2.10.29, or leave -server out for the version the build links"
      server="$2"
      shift 2
      ;;
    -drop)
      [ $# -ge 2 ] || usage
      drops+=("$2")
      shift 2
      ;;
    -timeout)
      [ $# -ge 2 ] || usage
      [ -n "$2" ] || die "-timeout '' names no duration: give one in go's syntax, such as 25m, or leave -timeout out for go test's 10 minutes"
      GO_TEST_TIMEOUT="$2"
      shift 2
      ;;
    -h | --help) usage ;;
    -*)
      echo "forced-order.sh: unknown flag $1" >&2
      usage
      ;;
    *) break ;;
  esac
done
[ $# -ge 1 ] || usage
command -v patch >/dev/null || die "patch(1) is not installed, and the hooks are a patch"
command -v jq >/dev/null || die "jq is not installed, and the script reads go's JSON with it"

cd "$module"
[ -f go.mod ] || die "$module holds no go.mod"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The workspace and its `use` paths, decoded from go's own JSON and made absolute. A module under
# a workspace that does not use it, or a workspace with a replace (whose relative path would mean
# something else in the copy), is refused: either would build with other module versions than CI's
# build of the module.
workspace="$(go env GOWORK)"
uses=()
in_workspace=""
if [ -n "$workspace" ] && [ "$workspace" != off ]; then
  workspace_dir="$(dirname "$workspace")"
  go work edit -json "$workspace" >"$work/workspace.json" || die "go work edit -json could not read $workspace"
  jq -j '.Use[].DiskPath + "\u0000"' "$work/workspace.json" >"$work/uses" ||
    die "jq could not read the use paths of $workspace"
  while IFS= read -r -d '' use; do
    uses+=("$use")
    [ "$(cd "$workspace_dir" && cd "$use" && pwd)" != "$module" ] || in_workspace=1
  done <"$work/uses"
  [ -n "$in_workspace" ] || die "$workspace names no use path that is $module"
  replaces="$(jq '(.Replace // []) | length' "$work/workspace.json")" ||
    die "jq could not read the replaces of $workspace"
  [ "$replaces" = 0 ] || die "$workspace holds $replaces replace(s), which the copy of it cannot carry"
fi

# The copy every go command of this script reads and may write in place of the repository's own
# files: a copy of the workspace file with absolute use paths, or of the module's go.mod and
# go.sum. Even reading the linked version can record checksums (a go.mod moved to a version whose
# checksums go.work.sum lacks), so that read goes to the copy too.
if [ -n "$in_workspace" ]; then
  cp "$workspace" "$work/go.work"
  [ ! -f "$workspace.sum" ] || cp "$workspace.sum" "$work/go.work.sum"
  for use in "${uses[@]}"; do
    go work edit -dropuse="$use" -use="$(cd "$workspace_dir" && cd "$use" && pwd)" "$work/go.work"
  done
  go_cmd=(env "GOWORK=$work/go.work" go)
  modflags=()
  build="a copy of $workspace"
else
  cp go.mod "$work/go.mod"
  [ ! -f go.sum ] || cp go.sum "$work/go.sum"
  go_cmd=(env GOWORK=off "GOFLAGS=${GOFLAGS:+$GOFLAGS }-mod=mod" go)
  modflags=(-modfile="$work/go.mod")
  build="a copy of $module/go.mod, with no workspace"
fi
flags=(${modflags[@]+"${modflags[@]}"} -tags odatrace -race)
# The version the build requires, and the module that replaces it, if any: for a replaced module
# go list's .Version is the requirement, not what the build links, and the hooked copy's replace
# below would take the replacement's place, so a module that replaces nats-server is refused.
listed="$("${go_cmd[@]}" list ${modflags[@]+"${modflags[@]}"} -m \
  -f '{{.Version}}{{with .Replace}} {{.Path}}{{with .Version}}@{{.}}{{end}}{{end}}' "$nats_server")" ||
  die "go list could not read the nats-server version $build links"
linked="${listed%% *}"
[ "$listed" = "$linked" ] ||
  die "$module's build replaces $nats_server with ${listed#* }: the hooks patch an upstream release, and the hooked copy would replace the replacement, so a replaced nats-server is refused"
[ -n "$server" ] || server="${linked#v}"
patch_dir="$hooked/v$server"
[ -f "$patch_dir/server.patch" ] && [ -f "$patch_dir/zz_measure_write.go" ] ||
  die "no hooks for nats-server $server: $patch_dir holds no server.patch and zz_measure_write.go"

# The points this version's hooks report: the "<area>.<event>" string each hook call passes, in
# the lines the patch adds and in the version's own state writer. -drop takes these names alone,
# and a name that is none is refused here, before anything is fetched, cut or built.
points="$({
  sed -n 's/^+\([^+].*\)$/\1/p' "$patch_dir/server.patch"
  cat "$patch_dir/zz_measure_write.go"
} | grep -E 'measure[A-Za-z]*\(' | grep -oE '"[a-z]+\.[A-Za-z]+"' | tr -d '"' | sort -u)" ||
  die "$patch_dir names no hook point"
for point in ${drops[@]+"${drops[@]}"}; do
  grep -qxF -- "$point" <<<"$points" ||
    die "-drop $point: no hook of nats-server $server reports \"$point\"; its points are $(paste -sd ' ' <<<"$points")"
done

# Downloaded from the work directory, where no go.mod is, so the module's own go.sum is not
# touched.
(cd "$work" && GOWORK=off go mod download -json "$nats_server@v$server") >"$work/download.json" ||
  die "go mod download could not fetch $nats_server@v$server"
source_dir="$(jq -r '.Dir // empty' "$work/download.json")" || die "jq could not read go mod download's answer"
[ -n "$source_dir" ] && [ -d "$source_dir" ] || die "go mod download found no source for $nats_server@v$server"
copy="$work/nats-server"
cp -R "$source_dir" "$copy"
chmod -R u+w "$copy"
grep -qF "VERSION = \"$server\"" "$copy/server/const.go" ||
  die "$source_dir is not nats-server $server: its server/const.go declares another VERSION"
patch --batch --forward --fuzz=0 --no-backup-if-mismatch --quiet -p1 -d "$copy" <"$patch_dir/server.patch" ||
  die "$patch_dir/server.patch does not apply to nats-server v$server's own files"
# The hooks' declarations, shared by every version, and the version's own state writer.
cp "$hooked/zz_measure.go" "$copy/server/zz_measure.go"
cp "$patch_dir/zz_measure_write.go" "$copy/server/zz_measure_write.go"

# -drop: the hook calls of the patched files that pass the point, exactly. nats-server's own lines
# call no measure function, so no line of its own is ever cut.
hooked_files=()
while IFS= read -r file; do
  hooked_files+=("$copy/server/$file")
done < <(sed -n 's|^+++ b/server/\(.*\.go\)$|\1|p' "$patch_dir/server.patch")
hooked_files+=("$copy/server/zz_measure.go" "$copy/server/zz_measure_write.go")
for point in ${drops[@]+"${drops[@]}"}; do
  removed=0
  for path in "${hooked_files[@]}"; do
    lines="$(awk -v q="\"$point\"" 'index($0, q) && /measure[A-Za-z]*\(/ { n++ } END { print n + 0 }' "$path")"
    [ "$lines" -gt 0 ] || continue
    awk -v q="\"$point\"" '!(index($0, q) && /measure[A-Za-z]*\(/)' "$path" >"$path.kept"
    mv "$path.kept" "$path"
    removed=$((removed + lines))
  done
  [ "$removed" -gt 0 ] || die "-drop $point: no hook line of the patched server names \"$point\""
  echo "forced-order.sh: dropped $removed hook line(s) naming \"$point\"" >&2
done

if [ -n "$in_workspace" ]; then
  go work edit -replace="$nats_server=$copy" "$work/go.work"
else
  go mod edit -replace="$nats_server=$copy" "$work/go.mod"
fi
echo "forced-order.sh: nats-server v$server, hooked by $patch_dir, in $module, built in $build" >&2

# Every package named must declare a forced-order test: go test -list prints each matching name,
# then the package's own `ok` line, or a `?` line for a package with no test files.
"${go_cmd[@]}" test "${flags[@]}" -list '^TestForcedOrder' "$@" >"$work/list" ||
  { cat "$work/list" >&2; die "go test -list failed"; }
empty="$(awk '
  /^TestForcedOrder/ { n++; next }
  /^ok[ \t]/ { if (n == 0) print $2; n = 0; seen++; next }
  /^\?[ \t]/ { print $2; n = 0; seen++; next }
  END { if (seen == 0) print "UNKNOWN: go test -list named no package" }
' "$work/list")"
[ -z "$empty" ] || die "no TestForcedOrder test in: $empty"

# Every package named must also run one. go test -v prints `--- PASS:` or `--- FAIL:` at the start
# of a line for each top-level test that ran (`--- SKIP:` for one that skipped), and ends each
# package's output, which it prints as one block, with that package's own `ok` or `FAIL` line. A
# package whose every forced-order test skipped would end in `ok`: its line is printed as a FAIL
# line instead, and the run fails. The go command stays a line of its own, the text
# .github/scripts/ci_job_reads.py pins.
run_forced_order_tests() {
  "${go_cmd[@]}" test "${flags[@]}" -count=1 -v -timeout "${GO_TEST_TIMEOUT:-10m}" -run '^TestForcedOrder' "$@"
}
run_forced_order_tests "$@" 2>&1 | awk '
  /^--- (PASS|FAIL): TestForcedOrder/ { ran++ }
  /^(ok|FAIL)[ \t]+[^ \t]/ {
    if ($1 == "ok" && ran == 0) {
      print "FAIL\t" $2 "\t(forced-order.sh: every TestForcedOrder test of the package skipped, so none ran)"
      fflush()
      skipped = skipped " " $2
      next
    }
    ran = 0
  }
  { print; fflush() }
  END { if (skipped != "") { print "forced-order.sh: no TestForcedOrder test ran in:" skipped; exit 1 } }
'
