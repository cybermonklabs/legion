#!/usr/bin/env bash
# Writes the THIRD_PARTY_NOTICES of every Go release archive this repository ships, and
# cross-builds the commands no other pull-request job builds for each of their targets, into the
# directory it is given; it prints the line count of each file it wrote.
#
# Two jobs run it, and that is why it is one script: ci.yaml's lint job (its notices step), on
# every pull request and merge group the go_notices filter selects, and
# .github/workflows/ci-caches.yaml's daemon job, on a push to main, to fill the go-notices cache
# entry lint restores. The fill must build exactly what lint builds, or the entry warms nothing
# and lint rebuilds it cold on every run with nothing failing, which is what two copies of these
# loops drift into the moment a target list or a command changes.
#
# What it covers, per release target: `legion` (release.yaml's legion_build job), the listener
# and the agent-secrets pair (release-envoy-listener.yaml), all three over
# .github/go-release-targets.json; the agent-secrets client, over
# .github/agent-secrets-client-targets.json; and every envoy-infra command
# (release-envoy-infra.yaml), over .github/envoy-infra-targets.json. Each build uses that
# release build's own GOOS, GOARCH and CGO_ENABLED, so the notices are the ones the release
# ships, and a module whose license cannot be determined fails the pull request before a release
# does. Writing notices loads packages without type-checking them, so the darwin agent-secrets
# client and every envoy-infra command are compiled here as well, with their release builds'
# CGO_ENABLED and -buildvcs: a compile error on one platform fails the pull request rather than
# the release after the merge.
#
# Run from anywhere: .github/scripts/go-release-notices.sh <directory to write the notices into>
set -euo pipefail
shopt -s inherit_errexit

notices=${1:-}
[ -n "$notices" ] ||
  { echo "::error::go-release-notices.sh needs the directory to write the notices into" >&2; exit 1; }
# Resolved before the cd, so a relative directory is the caller's, and the go commands below run
# from the repository root whatever directory the caller started this in.
mkdir -p "$notices"
notices=$(cd "$notices" && pwd)
cd "$(dirname "$0")/../.."

# One subshell per release target, rather than one around each command: the GOOS, GOARCH and
# CGO_ENABLED it exports, and the cd below, end with it, so no target's build reaches the next.
# Those three are exported rather than written in front of the command, and each go command names
# its module with `go -C` rather than being run from inside it, so that every one of these lines
# is a bare go command .github/scripts/check-ci-filters.sh can read: it reduces neither a line
# that sets a name choosing the files go compiles nor one wrapped in a cd-ing subshell, and an
# unreduced line would count as reading every Go file of the repository. The Run pinning each line
# in .github/scripts/ci_job_reads.py declares that target's three names instead. The notices
# script is the one command still run from inside its module: it reads the module of the directory
# it runs in (`go list -m`, and go-licenses over the packages it is given), and takes no -C.
for target in $(jq -r '.legion[]' .github/go-release-targets.json); do
  (
    export CGO_ENABLED=0 GOOS="${target%/*}" GOARCH="${target#*/}"
    cd packages/daemon
    ../../scripts/go-third-party-notices.sh "$notices/legion-${target%/*}-${target#*/}" ./cmd/legion
  )
done
for arch in $(jq -r '.envoy[]' .github/go-release-targets.json); do
  (
    export GOOS=linux GOARCH="$arch"
    cd packages/envoy
    # The listener's release build sets no CGO_ENABLED, so its notices are written with the
    # default; the agent-secrets pair's sets 0, which is exported after it rather than before.
    ../../scripts/go-third-party-notices.sh "$notices/legion-envoy-$arch" ./cmd/listener
    export CGO_ENABLED=0
    ../../scripts/go-third-party-notices.sh "$notices/agent-secrets-$arch" ./cmd/agent-secrets ./cmd/agent-secrets-helper
  )
done
for target in $(jq -r '.["agent-secrets-client"][]' .github/agent-secrets-client-targets.json); do
  (
    export CGO_ENABLED=0 GOOS="${target%/*}" GOARCH="${target#*/}"
    go -C packages/envoy build -buildvcs=false -o /dev/null ./cmd/agent-secrets
    cd packages/envoy
    ../../scripts/go-third-party-notices.sh "$notices/agent-secrets-${target%/*}-${target#*/}" ./cmd/agent-secrets
  )
done
for target in $(jq -r '.["envoy-infra"][]' .github/envoy-infra-targets.json); do
  (
    export CGO_ENABLED=0 GOOS="${target%/*}" GOARCH="${target#*/}"
    go -C packages/envoy/infra build -buildvcs=false -o /dev/null ./cmd/...
    cd packages/envoy/infra
    ../../../scripts/go-third-party-notices.sh "$notices/pulumi-resource-envoy-infra-${target%/*}-${target#*/}" ./cmd/pulumi-resource-envoy-infra
    ../../../scripts/go-third-party-notices.sh "$notices/envoy-infra-${target%/*}-${target#*/}" ./cmd/envoy-infra
  )
done
wc -l "$notices"/*
