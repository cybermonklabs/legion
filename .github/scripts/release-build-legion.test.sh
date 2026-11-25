#!/usr/bin/env bash
# Tests for `release-build-legion.sh`. With no argument, as release.yaml's legion_build job runs it,
# it packs one archive per legion target of .github/go-release-targets.json, each holding that
# target's static binary, built with the target's CGO_ENABLED=0, GOOS and GOARCH, and the
# THIRD_PARTY_NOTICES the notices script writes under the same environment, and it stops, packing
# nothing, when release-push.sh legion-targets refuses the list. It links in the stamp
# $LEGION_CONTRACT_ORDER, and refuses to start, writing nothing, when the stamp is unset or empty.
# Its modes, which check-legion-release-paths.sh asks: `list`, go list over ./cmd/legion in
# packages/daemon with a target's environment; `env`, go env with that environment; and
# `modules`, the go.mod of each main module.
# The modes read none of the build's variables, the stamp among them, so they run with none of
# them set, as lint runs them; and a mode it does not know, one without its target, or one given
# more than it takes, is refused.
#
# Each case builds a miniature repository under a temporary directory: a daemon module with a
# legion command, the target list, and a stand-in scripts/go-third-party-notices.sh that writes
# where it ran, its environment and its arguments to the file the notices go to. The real script,
# and release-push.sh, whose legion-targets it runs, are symlinked into it, so the scripts under
# test are the files the release runs. Needs Go and jq.
#
# Run from anywhere: .github/scripts/release-build-legion.test.sh
# CI runs it in ci.yaml's test job, after setup-go.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
build_script="$script_dir/release-build-legion.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree whose target list names a linux and a darwin target for legion, as the
# real one does. Echoes its root.
fixture() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts" "$root/scripts" "$root/packages/daemon/cmd/legion"
  ln -s "$build_script" "$root/.github/scripts/release-build-legion.sh"
  ln -s "$script_dir/release-push.sh" "$root/.github/scripts/release-push.sh"
  printf 'module example.com/daemon\n\ngo 1.22\n' > "$root/packages/daemon/go.mod"
  cat > "$root/packages/daemon/cmd/legion/main.go" <<'GO'
package main

var revision, release, piLegionVersion, piEnvoyVersion string

func main() { println(revision, release, piLegionVersion, piEnvoyVersion) }
GO
  printf '{\n  "legion": ["linux/amd64", "darwin/arm64"],\n  "envoy": ["amd64"]\n}\n' > "$root/.github/go-release-targets.json"
  cat > "$root/scripts/go-third-party-notices.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
out=$1
shift
printf 'dir=%s CGO_ENABLED=%s GOOS=%s GOARCH=%s args=%s\n' \
  "${PWD##*/}" "${CGO_ENABLED-unset}" "${GOOS-unset}" "${GOARCH-unset}" "$*" > "$out"
SH
  chmod +x "$root/scripts/go-third-party-notices.sh"
  echo "$root"
}

# The stamp the build cases give, in the form scripts/daemon-contract-order.sh --stamp prints.
stamp='v1:entry-one,entry-two'

# run_build <root> [<stamp>]: the build, from <root>, with the six variables release.yaml's step
# gives it. <stamp> is $LEGION_CONTRACT_ORDER: $stamp when it is omitted, and the variable is not
# set at all when it is the word unset. Leaves its exit code in status and its output in out.
run_build() {
  local root=$1
  local -a order=("LEGION_CONTRACT_ORDER=$stamp")
  if [ $# -ge 2 ]; then
    if [ "$2" = unset ]; then
      order=(-u LEGION_CONTRACT_ORDER)
    else
      order=("LEGION_CONTRACT_ORDER=$2")
    fi
  fi
  set +e
  out=$(cd "$root" && env "${order[@]}" VERSION=1.2.3 PI_LEGION_VERSION=0.4.0 PI_ENVOY_VERSION=0.5.0 \
    GITHUB_SHA=0123456789abcdef0123456789abcdef01234567 GITHUB_WORKSPACE="$root" \
    .github/scripts/release-build-legion.sh 2>&1)
  status=$?
  set -e
}

# run_mode <root> <arg>...: the script with <arg>..., from <root>, with none of the build's six
# variables set. Leaves its exit code in status, its stdout in out and its stderr in err.
run_mode() {
  local root=$1
  shift
  set +e
  out=$(cd "$root" && env -u VERSION -u PI_LEGION_VERSION -u PI_ENVOY_VERSION -u GITHUB_SHA -u GITHUB_WORKSPACE \
    -u LEGION_CONTRACT_ORDER .github/scripts/release-build-legion.sh "$@" 2>"$work/err")
  status=$?
  set -e
  err=$(<"$work/err")
}

echo "case: the build packs one archive per legion target, each built with its target's environment"
root=$(fixture build)
run_build "$root"
check "exits 0" "$(is "$status" 0)"
for target in linux/amd64 darwin/arm64; do
  os=${target%/*} arch=${target#*/} dir="legion-${target%/*}-${target#*/}"
  listing=$(tar tzf "$root/assets/$dir.tar.gz" 2>&1 | LC_ALL=C sort | tr '\n' ' ')
  check "$target: the archive holds the binary and its notices" "$(is "$listing" "$dir/ $dir/THIRD_PARTY_NOTICES $dir/legion ")"
  info=$(go version -m "$root/out/$dir/legion" 2>&1)
  check "$target: the binary is built for $os/$arch without cgo" \
    "$(is "$(contains "$info" "GOOS=$os$")|$(contains "$info" "GOARCH=$arch$")|$(contains "$info" 'CGO_ENABLED=0$')" 'true|true|true')"
  check "$target: the binary carries the release's version, its plugin pair and the contract-order stamp" \
    "$(contains "$info" "-X main.revision=0123456789abcdef0123456789abcdef01234567 -X main.release=v1.2.3 -X main.piLegionVersion=0.4.0 -X main.piEnvoyVersion=0.5.0 -X github.com/sjawhar/legion/daemon/internal/api.contractOrder=$stamp")"
  check "$target: the notices are written in packages/daemon for ./cmd/legion with the same environment" \
    "$(is "$(cat "$root/out/$dir/THIRD_PARTY_NOTICES")" "dir=daemon CGO_ENABLED=0 GOOS=$os GOARCH=$arch args=./cmd/legion")"
done

# The stamp numbers the daemon's contract entries; an unstamped binary numbers them by id instead,
# so the build must not start without one, and must not pack the archives before it says so.
for order in unset ''; do
  echo "case: the build refuses to start when the contract-order stamp is ${order:-empty}"
  root=$(fixture "build-no-stamp-${order:-empty}")
  run_build "$root" "$order"
  check "exits non-zero" "$([ "$status" -ne 0 ] && echo true || echo false)"
  check "names the variable and the command that prints it" \
    "$(contains "$out" 'LEGION_CONTRACT_ORDER: is unset or empty: set it to the stamp scripts/daemon-contract-order.sh --stamp prints for this commit')"
  check "writes neither out/ nor assets/" "$([ ! -e "$root/out" ] && [ ! -e "$root/assets" ] && echo true || echo false)"
done

echo "case: the build stops when it cannot read the targets"
root=$(fixture build-no-list)
printf '{\n  "envoy": ["amd64"]\n}\n' > "$root/.github/go-release-targets.json"
run_build "$root"
check "exits non-zero" "$([ "$status" -ne 0 ] && echo true || echo false)"
check "packs no archive" "$(is "$(find "$root/assets" -type f 2>/dev/null | wc -l)" 0)"

echo "case: the build stops at an empty legion list"
root=$(fixture build-empty-list)
printf '{\n  "legion": [],\n  "envoy": ["amd64"]\n}\n' > "$root/.github/go-release-targets.json"
run_build "$root"
check "exits non-zero" "$([ "$status" -ne 0 ] && echo true || echo false)"
check "says so, as release-push.sh legion-targets does" \
  "$(contains "$out" 'release-push.sh legion-targets: .github/go-release-targets.json lists no legion target')"
check "packs no archive" "$(is "$(find "$root/assets" -type f 2>/dev/null | wc -l)" 0)"

echo "case: the build stops at a target that is not <os>/<arch>"
# go builds an empty GOOS or GOARCH for the host's own, so the empty target would pack a host
# binary as legion--.tar.gz.
root=$(fixture build-bad-target)
printf '{\n  "legion": ["linux/amd64", ""],\n  "envoy": ["amd64"]\n}\n' > "$root/.github/go-release-targets.json"
run_build "$root"
check "exits non-zero" "$([ "$status" -ne 0 ] && echo true || echo false)"
check "names the target" "$(contains "$out" 'release-push.sh legion-targets: the legion target "" is not <os>/<arch>')"
check "packs no archive" "$(is "$(find "$root/assets" -type f 2>/dev/null | wc -l)" 0)"

echo "case: list runs go list over ./cmd/legion in packages/daemon with the target's environment"
root=$(fixture list)
printf '//go:build darwin\n\npackage main\n' > "$root/packages/daemon/cmd/legion/darwin.go"
format='{{context.GOOS}}/{{context.GOARCH}} cgo={{context.CgoEnabled}} {{.ImportPath}} {{.GoFiles}}'
run_mode "$root" list darwin/arm64 -f "$format"
check "darwin/arm64: exits 0" "$(is "$status" 0)"
check "darwin/arm64: lists the command for darwin/arm64 without cgo, with its darwin file" \
  "$(is "$out" 'darwin/arm64 cgo=false example.com/daemon/cmd/legion [darwin.go main.go]')"
run_mode "$root" list linux/amd64 -f "$format"
check "linux/amd64: lists the command for linux/amd64 without cgo, without the darwin file" \
  "$(is "$status|$out" '0|linux/amd64 cgo=false example.com/daemon/cmd/legion [main.go]')"
run_mode "$root" list linux/amd64 -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}'
check "passes every flag on: -deps lists the command among its dependencies" \
  "$(is "$status|$(grep -v '^$' <<<"$out")" '0|example.com/daemon/cmd/legion')"

echo "case: env runs go env in packages/daemon with the target's environment"
root=$(fixture env)
run_mode "$root" env darwin/arm64 GOOS GOARCH CGO_ENABLED
check "prints the target's GOOS, GOARCH and CGO_ENABLED" "$(is "$status|$out" $'0|darwin\narm64\n0')"
run_mode "$root" env linux/amd64 GOWORK
check "no go.work: GOWORK is empty" "$(is "$status|$out" '0|')"
printf 'go 1.22\n\nuse ./packages/daemon\n' > "$root/go.work"
run_mode "$root" env linux/amd64 GOWORK
check "a go.work at the root: GOWORK names it" "$(is "$status|$out" "0|$(cd "$root" && pwd -P)/go.work")"

echo "case: modules prints the go.mod of each module the build's go.work uses"
root=$(fixture modules)
run_mode "$root" modules linux/amd64
check "no go.work: the daemon's go.mod alone" "$(is "$status|$out" "0|$(cd "$root" && pwd -P)/packages/daemon/go.mod")"
mkdir -p "$root/packages/other"
printf 'module example.com/other\n\ngo 1.22\n' > "$root/packages/other/go.mod"
printf 'go 1.22\n\nuse (\n\t./packages/daemon\n\t./packages/other\n)\n' > "$root/go.work"
run_mode "$root" modules linux/amd64
check "a go.work: the go.mod of each module it uses" \
  "$(is "$status|$out" "0|$(cd "$root" && pwd -P)/packages/daemon/go.mod"$'\n'"$(cd "$root" && pwd -P)/packages/other/go.mod")"
run_mode "$root" modules
check "modules without a target: exits 2 with the usage" "$(is "$status|$(contains "$err" 'usage: release-build-legion.sh ')" '2|true')"
run_mode "$root" modules linux/amd64 extra
check "modules with more than its target: exits 2 printing nothing, with the usage" \
  "$(is "$status|$out|$(contains "$err" '^::error::release-build-legion.sh: modules takes a target alone; usage: release-build-legion.sh ')" '2||true')"

echo "case: a mode it does not know, or one without its target"
root=$(fixture usage)
run_mode "$root" bogus
check "an unknown mode: exits 2 printing nothing, with the usage" \
  "$(is "$status|$out|$(contains "$err" '^::error::release-build-legion.sh: unknown mode bogus; usage: release-build-legion.sh ')" '2||true')"
run_mode "$root" list
check "list without a target: exits 2 with the usage" "$(is "$status|$(contains "$err" 'usage: release-build-legion.sh ')" '2|true')"
run_mode "$root" env
check "env without a target: exits 2 with the usage" "$(is "$status|$(contains "$err" 'usage: release-build-legion.sh ')" '2|true')"
check "builds nothing" "$(is "$([ -e "$root/out" ] || [ -e "$root/assets" ] && echo built || echo none)" none)"

summary "release-build-legion.sh"
