#!/usr/bin/env bash
# Builds one Dispatch build from a commit of this repository into a directory: the server, its web
# app, its `dispatch` command, and the test data the two-build test (cmd/dispatch/twobuild_*_test.go,
# `go test -tags twobuild`) reads from it. It builds from `git archive` of the commit, so it makes
# no worktree, branch or checkout. The server and its web app take the steps the commit's image
# build takes (packages/envoy/docker/Dockerfile): `bun install --frozen-lockfile`, the contracts'
# Go codegen, `bun run build:web`, and `go build` of ./cmd/dispatch with `-X main.buildCommit`.
# The image does not build the `dispatch` command: the script builds the contracts bundle, then
# runs the commit's own build-dispatch-cli.sh. `go build` runs with `GOTOOLCHAIN=auto`, so a
# commit that needs another Go toolchain gets it.
#
#   packages/envoy/scripts/build-dispatch-build.sh --commit <sha|tag> --out <dir>
#   packages/envoy/scripts/build-dispatch-build.sh --previous-release --out <dir>
#
# --previous-release builds the newest legion-envoy-v* tag that is an ancestor of
# `git merge-base HEAD origin/main`: the release production ran before this branch. --commit HEAD
# builds the current branch the same way. Put <dir> under $TMPDIR. <dir> must be absent or empty,
# or hold a complete build of the same commit, which the script then leaves as it is. The commit is
# resolved, and refused, by resolve-dispatch-build.sh beside this script, which prints the full sha
# this script would build and builds nothing, so a caller can key a cache of <dir> by it before the
# build (CI's dispatch-two-builds job).
#
# <dir> then holds:
#   src/                              the commit's archive, where the parts were built
#   envoy-dispatch                    the server, built with -X main.buildCommit=<sha>
#   web/                              the web app (packages/dispatch/web/dist), DISPATCH_WEB_DIST
#   dispatch.js                       the `dispatch` command bundle, run as `bun dispatch.js`
#   pmdoc/testdata/corpus/            the commit's document corpus
#   pmdoc/schema/blocks.json          the commit's block manifest (its schema version)
#   text/testdata/                    the commit's reference-form fixtures
#   COMMIT                            the full sha, written last: a directory with it is complete
#
# The refusals, which resolve-dispatch-build.sh makes: a commit that is not an ancestor of HEAD, and
# a commit older than the release
# legion-envoy-v30.11.0, the oldest build either two-build test can run as the earlier side. That
# release carries migration 0117, from which two builds share one database, and migration
# 20261111210103, after which a project must name a default assignee, which an older build does not
# write when it creates one (cmd/dispatch/README.md, "Database migrations"); it serves on a socket handed to it (DISPATCH_LISTEN_FD), which the
# browser suite's harness holds for it; and it knows DISPATCH_DEV_SIGNIN_SHARED_KEY, the one cookie
# key that run gives both of its servers (packages/dispatch/e2e/run-server.sh).
set -euo pipefail

usage() {
  printf 'usage: %s (--commit <sha|tag> | --previous-release) --out <dir>\n' "$0" >&2
  exit 2
}

fail() {
  printf 'build-dispatch-build: %s\n' "$*" >&2
  exit 1
}

note() {
  printf 'build-dispatch-build: %s\n' "$*" >&2
}

which=()
out=
while (($# > 0)); do
  case "$1" in
  --commit)
    (($# >= 2)) || usage
    which+=(--commit "$2")
    shift 2
    ;;
  --previous-release)
    which+=(--previous-release)
    shift
    ;;
  --out)
    (($# >= 2)) || usage
    out=$2
    shift 2
    ;;
  *) usage ;;
  esac
done
[[ -n "$out" && ${#which[@]} -gt 0 ]] || usage

for command in git tar go bun; do
  command -v "$command" >/dev/null || fail "the command $command is necessary. Install it and run this script again."
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)
readonly root

sha=$("$root/packages/envoy/scripts/resolve-dispatch-build.sh" "${which[@]}") || exit
commit=${which[1]:-$sha}

mkdir -p -- "$out"
out=$(cd -- "$out" && pwd -P)
readonly out
if [[ -f "$out/COMMIT" && "$(<"$out/COMMIT")" == "$sha" ]]; then
  note "$out already holds the build of $sha. It stays as it is."
  exit 0
fi
[[ -z "$(ls -A -- "$out")" ]] ||
  fail "$out is not empty and holds no complete build of $sha. Remove it, or give an empty directory."

note "building $commit ($sha) in $out/src"
mkdir -- "$out/src"
git -C "$root" archive "$sha" | tar -x -C "$out/src" || fail "the archive of $sha did not extract into $out/src."
readonly src=$out/src

# The JavaScript parts: the install, the Go contracts the server compiles (regenerated, never the
# copy in the tree) and the contracts bundle, then the web app and the command. The install, the
# codegen and the web app are in the order the commit's image build makes them. The install reads
# the npm registry for any package the job's own install did not leave in Bun's cache, so it runs
# under .github/scripts/network-install.sh's bound for bun, as every install of a required job does.
(cd -- "$src" && "$root/.github/scripts/network-install.sh" bun -- bun install --frozen-lockfile) ||
  fail "bun install --frozen-lockfile failed in the archive of $sha."
(cd -- "$src/packages/contracts" && bun run gen:go && bun run build) ||
  fail "the contracts build (gen:go, build) of $sha failed."
(cd -- "$src/packages/dispatch" && bun run build:web) || fail "the web build (bun run build:web) of $sha failed."
[[ -f "$src/packages/dispatch/web/dist/index.html" ]] || fail "the web build of $sha wrote no web/dist/index.html."
cp -R -- "$src/packages/dispatch/web/dist" "$out/web"
cli_script=$src/packages/envoy-client/scripts/build-dispatch-cli.sh
[[ -f "$cli_script" ]] ||
  fail "$sha has no packages/envoy-client/scripts/build-dispatch-cli.sh. This script builds the dispatch command only with that script."
bash "$cli_script" "$out" || fail "the dispatch command bundle of $sha failed to build."

(cd -- "$src/packages/envoy" &&
  GOTOOLCHAIN=auto go build -ldflags "-X main.buildCommit=$sha" -o "$out/envoy-dispatch" ./cmd/dispatch) ||
  fail "go build ./cmd/dispatch of $sha failed."

readonly dispatch_dir=$src/packages/envoy/internal/dispatch
mkdir -p -- "$out/pmdoc/testdata" "$out/pmdoc/schema" "$out/text"
cp -R -- "$dispatch_dir/pmdoc/testdata/corpus" "$out/pmdoc/testdata/corpus"
cp -- "$dispatch_dir/pmdoc/schema/blocks.json" "$out/pmdoc/schema/blocks.json"
cp -R -- "$dispatch_dir/text/testdata" "$out/text/testdata"

printf '%s\n' "$sha" >"$out/COMMIT"
note "built $commit ($sha) in $out"
