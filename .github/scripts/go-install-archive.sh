#!/usr/bin/env bash
# go-install-archive.sh <go.mod>: installs the Go that go.mod names (its `toolchain` line, else its
# `go` line, as actions/setup-go's go-version-file reads it) from go.dev's archive for this machine,
# puts its bin directory first on PATH for the job's later steps and sets GOTOOLCHAIN=local for
# them: what actions/setup-go does with `go-download-base-url: https://go.dev/dl`, `token: ''` and
# `cache: false`, from the same source.
#
# Why not setup-go: ci.yaml's changes job and the fill of its cache entries
# (.github/workflows/ci-caches-changes.yaml) run once per runner and read the toolchain only in
# that job, and setup-go spent 4.1 s of its 7 s copying the unpacked tree into the runner's tool
# cache, which nothing reads after the job (job 139712493810: the unpack ended 2.4 s into the step,
# "Successfully cached go" came at 6.5 s). This downloads and unpacks in one stream: 1.4 s on
# the same runner (run 45766372051, job gen). .github/ci-notes/ci.md#the-changes-job-computes-its-filters-when-it-runs
# says what the job needs it for.
#
# The archive comes from go.dev over HTTPS, as setup-go's does; tar refuses a stream gzip finds
# cut short, and the step that runs this exits non-zero, so the job's fail-open path (the
# generation finds no go) takes over.
set -euo pipefail

gomod=${1:?usage: go-install-archive.sh <go.mod>}
version=$(sed -n 's/^toolchain go\([0-9][0-9.a-z]*\)$/\1/p' "$gomod")
[ -n "$version" ] || version=$(sed -n 's/^go \([0-9][0-9.a-z]*\)$/\1/p' "$gomod")
if [ -z "$version" ]; then
  echo "::error file=$gomod::names no Go version in a toolchain or go line" >&2
  exit 1
fi
case $(uname -m) in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "::error::no Go archive for machine $(uname -m)" >&2; exit 1 ;;
esac
dest=${RUNNER_TEMP:?RUNNER_TEMP must name the temporary directory of the runner}/go$version
rm -rf "$dest"
mkdir -p "$dest"
curl -fsSL --retry 3 "https://go.dev/dl/go$version.linux-$arch.tar.gz" | tar -xz -C "$dest"
"$dest/go/bin/go" version
echo "$dest/go/bin" >>"${GITHUB_PATH:?GITHUB_PATH must name the path file of the runner}"
echo "GOTOOLCHAIN=local" >>"${GITHUB_ENV:?GITHUB_ENV must name the env file of the runner}"
