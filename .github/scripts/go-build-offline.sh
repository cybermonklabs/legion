# shellcheck shell=bash
# go-build-offline.sh — go_build_offline, the one Go build a test launcher runs, from the module
# cache alone. Sourced, never run: packages/envoy/scripts/e2e-local.sh,
# packages/pi-envoy/scripts/local-stack.sh and packages/dispatch/e2e's run-server.sh, run-broker.sh
# and run-signin-issuer.sh source it.
#
# A fetch while a test runs turns a fault of the module proxy or the checksum database into that
# test's failure, on a change that touched nothing it tests. So the build runs with GOPROXY=off:
# each CI job whose tests start one of those launchers fetches the build's modules in a step before
# them (go-fetch.sh deps, beside this file, which tries again on failure), and a module the cache
# still lacks fails the build here, with that fetch named after go's own output.
#
#   go_build_offline MODULE OUTPUT PACKAGE...
#     `go build -o OUTPUT PACKAGE...` in MODULE, with GOPROXY=off. MODULE is the module's
#     directory, named from the repository root as go-fetch.sh deps names it (packages/envoy), or
#     an absolute path; OUTPUT and each PACKAGE are read in MODULE, as go reads them there. What go
#     prints goes to stderr, whether the build succeeds or fails. When it fails and its output
#     names GOPROXY=off - a module the cache lacks - the fetch of this MODULE and these PACKAGEs
#     follows it, MODULE named from the repository root when it is inside it and whole otherwise.
#     Returns 0 when the build succeeds, 1 when it fails, and 2, running nothing, when called
#     without a module, an output and a package.
#
# What is the caller's stays at its call: the lock it builds under, the prebuilt binary it runs
# instead, the exec of what it built, and its own words when the build fails.
go_build_offline() {
  if (($# < 3)); then
    printf 'go_build_offline: takes MODULE OUTPUT PACKAGE..., got %s arguments\n' "$#" >&2
    return 2
  fi
  local dir=$1 out=$2 root output
  shift 2
  root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
  [[ "$dir" == /* ]] || dir="$root/$dir"
  if output=$(cd -- "$dir" && GOPROXY=off go build -o "$out" "$@" 2>&1); then
    [[ -z "$output" ]] || printf '%s\n' "$output" >&2
    return 0
  fi
  printf '%s\n' "$output" >&2
  if [[ "$output" == *GOPROXY=off* ]]; then
    dir=$(cd -- "$dir" && pwd)
    [[ "$dir" != "$root"/* ]] || dir=${dir#"$root"/}
    printf '%s\n  %s\n' \
      "The module cache lacks a module this build needs, and a test fetches no module while it runs. Fetch them before the tests, from the repository root:" \
      ".github/scripts/go-fetch.sh deps $dir $*" >&2
  fi
  return 1
}
