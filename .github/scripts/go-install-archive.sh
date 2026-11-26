#!/usr/bin/env bash
# go-install-archive.sh <go.mod>: installs the Go that go.mod names (its `toolchain` line, else its
# `go` line, as actions/setup-go's go-version-file reads it) from go.dev's archive for this machine,
# puts its bin directory first on PATH for the job's later steps and sets GOTOOLCHAIN=local for
# them: what actions/setup-go does with `go-download-base-url: https://go.dev/dl`, `token: ''` and
# `cache: false`, from the same source.
#
# Why not setup-go: ci.yaml's changes job and ci-caches-changes.yaml's fill of its cache entries
# read the toolchain only in that job, and setup-go spent 4.1 s of its 7 s copying the unpacked
# tree into the runner's tool cache, which nothing reads after the job (job 139712493810: the
# unpack ended 2.4 s into the step, "Successfully cached go" came at 6.5 s). This script took 2.1 s
# on the same runner, the listing, the download, the digest and the unpack together (job
# 139866053159, 11:07:40.38Z to its `go version` line at 11:07:42.46Z).
# .github/ci-notes/ci.md#the-changes-job-computes-its-filters-when-it-runs says what the job needs
# it for.
#
# GOTOOLCHAIN=local is written first, before anything can fail, as setup-go writes it: a failed
# install then leaves the job either no go at all or the image's own go, which refuses the version
# go.mod names rather than fetching it, and in both the generation's go commands fail and the
# filters fall open (ci-filters-generate.sh). The PATH entry is written only once the toolchain
# runs.
#
# The archive is downloaded to a file and checked against the sha256 go.dev lists for that file
# (https://go.dev/dl/?mode=json&include=all, the listing setup-go reads its versions from) before
# it is unpacked. That catches a cut or corrupted download, which tar would otherwise unpack in part.
# Each fetch retries through .github/scripts/ci-retry.sh, as every download of this repository's
# CI does (.github/ci-budgets.md's Retries note): five attempts, each bounded on its own, the last
# starting 150 s after the first, and curl starts the file again on each attempt. The listing's
# bound is 15 s an attempt, the one this script first gave it, about seven times the 2.1 s the
# whole script took in the job above; the listing was 2,279,774 bytes, fetched in 0.47 s from a
# pod on 2026-11-26. The archive's bound is 60 s an attempt, the one the Retries note gives a
# release archive: go1.27.3's was 67,076,044 bytes for linux-arm64 and 70,588,678 for
# linux-amd64, read the same day. So the listing takes at most 5 x 15 + 150 = 225 s and the
# archive 5 x 60 + 150 = 450 s, 675 s together, which each step that runs this holds in its own
# limit.
set -euo pipefail

echo "GOTOOLCHAIN=local" >>"${GITHUB_ENV:?GITHUB_ENV must name the env file of the runner}"
# shellcheck source=.github/scripts/ci-retry.sh
. "$(dirname "$0")/ci-retry.sh"
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
file=go$version.linux-$arch.tar.gz
temp=${RUNNER_TEMP:?RUNNER_TEMP must name the temporary directory of the runner}
dest=$temp/go$version

retry_fetch "go.dev's list of Go releases" 15 -- \
  curl -fsSL --connect-timeout 10 --max-time 15 -o "$temp/go-releases.json" "https://go.dev/dl/?mode=json&include=all" >&2
want=$(jq -r --arg f "$file" 'first(.[].files[] | select(.filename == $f) | .sha256) // ""' "$temp/go-releases.json")
if ! [[ $want =~ ^[0-9a-f]{64}$ ]]; then
  echo "::error::go.dev lists no sha256 for $file" >&2
  exit 1
fi
retry_fetch "the Go archive $file" 60 -- \
  curl -fsSL --connect-timeout 10 --max-time 60 -o "$temp/$file" "https://go.dev/dl/$file" >&2
got=$(sha256sum "$temp/$file" | cut -d' ' -f1)
if [ "$got" != "$want" ]; then
  echo "::error::$file downloaded with sha256 $got, and go.dev lists $want" >&2
  exit 1
fi
rm -rf "$dest"
mkdir -p "$dest"
tar -xzf "$temp/$file" -C "$dest"
rm -f "$temp/$file" "$temp/go-releases.json"
"$dest/go/bin/go" version
echo "$dest/go/bin" >>"${GITHUB_PATH:?GITHUB_PATH must name the path file of the runner}"
