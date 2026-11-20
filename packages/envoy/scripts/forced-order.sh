#!/usr/bin/env bash
# packages/envoy/scripts/forced-order.sh [-C <module dir>] [-server 2.15.1|2.10.29] [-drop <hold point>]... <package>...
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
# the build would then link other module versions than CI's. A module with no workspace builds on
# a copy of its go.mod with the replace added (go test -modfile, which go refuses in a workspace).
# The script reads go's JSON (go work edit -json, go mod download -json) with jq. Nothing else of
# nats-server changes, and no
# build without this script sees a hook.
#
#   -C <module dir>  the Go module to test, as `go -C` takes it (relative to the caller's
#                    directory); packages/envoy by default. The packages are relative to it.
#   -server <v>      the nats-server the tests link; by default the version the build links
#                    (2.15.1 today, the hub's), and 2.10.29 is the other hooked version. A version
#                    testdata/hooked has no directory for is refused, so a module that moves to a
#                    new nats-server fails here until its hooks are ported.
#   -drop <point>    removes every hook line naming that hold point from the patched copy before
#                    the build, so the point is never reached: the control that a test holding
#                    there fails without it. Repeatable.
#
# It fails, naming the package, when a package it is given declares no TestForcedOrder test, so
# a step that names a package whose tests were renamed or moved goes red rather than green with
# nothing run. GO_TEST_TIMEOUT replaces go test's 10-minute -timeout, which the CI jobs that run
# this script count in their time budgets. The copies of go.mod and go.sum, or of go.work and
# go.work.sum, may gain the checksums the build needs; the repository's own files are never
# written.
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
  echo "usage: forced-order.sh [-C <module dir>] [-server 2.15.1|2.10.29] [-drop <hold point>]... <package>..." >&2
  exit 2
}

module="$envoy"
server=""
drops=()
while [ $# -gt 0 ]; do
  case "$1" in
    -C)
      [ $# -ge 2 ] || usage
      module="$(cd "$2" && pwd)" || die "no module directory $2"
      shift 2
      ;;
    -server)
      [ $# -ge 2 ] || usage
      server="$2"
      shift 2
      ;;
    -drop)
      [ $# -ge 2 ] || usage
      drops+=("$2")
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
linked="$("${go_cmd[@]}" list ${modflags[@]+"${modflags[@]}"} -m -f '{{.Version}}' "$nats_server")" ||
  die "go list could not read the nats-server version $build links"
[ -n "$server" ] || server="${linked#v}"
patch_dir="$hooked/v$server"
[ -f "$patch_dir/server.patch" ] && [ -f "$patch_dir/zz_measure_write.go" ] ||
  die "no hooks for nats-server $server: $patch_dir holds no server.patch and zz_measure_write.go"

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

# -drop: every line of the hooked files that names the point, as the string its hook passes.
hooked_files=()
while IFS= read -r file; do
  hooked_files+=("$copy/server/$file")
done < <(sed -n 's|^+++ b/server/\(.*\.go\)$|\1|p' "$patch_dir/server.patch")
hooked_files+=("$copy/server/zz_measure.go" "$copy/server/zz_measure_write.go")
for point in ${drops[@]+"${drops[@]}"}; do
  removed=0
  for path in "${hooked_files[@]}"; do
    lines="$(grep -cF -- "\"$point\"" "$path" || true)"
    [ "$lines" -gt 0 ] || continue
    grep -vF -- "\"$point\"" "$path" >"$path.kept" || true
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

"${go_cmd[@]}" test "${flags[@]}" -count=1 -v -timeout "${GO_TEST_TIMEOUT:-10m}" -run '^TestForcedOrder' "$@"
