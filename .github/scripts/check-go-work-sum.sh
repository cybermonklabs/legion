#!/usr/bin/env bash
# Fails when a download in the root Go workspace needs a checksum that neither go.work.sum
# nor the go.sum files of the modules it uses hold.
#
# Why: the worker image's go mod download (the go-modules stage of
# packages/daemon/docker/worker.Dockerfile) died on 2026-11-24 after 864 s on
# `stream error INTERNAL_ERROR` asking sum.golang.org for two modules (job 138667218057).
# A workspace download asks the checksum database only for a hash no committed file holds.
# With every hash committed, the download needs only the module proxy.
# This check does not cover downloads that do not read the root go.work.sum: go install
# pkg@version outside a module (the go-licenses install in scripts/go-third-party-notices.sh,
# whose database request failed the listener image's job 138687314598), module-mode downloads
# such as the listener image's go mod download, or packages/envoy/infra with its own go.work.
#
# Resolve the pinned toolchain once with go env GOROOT. An older Go can switch here and verify
# the toolchain against the checksum database, as a build does. Every later command uses that
# toolchain's Go with GOTOOLCHAIN=local.
#
# Copy go.work, go.work.sum, and each used module's go.mod and go.sum at the same relative paths
# so local replacements still resolve. Run go mod download all in the copy with GOSUMDB=off,
# so the module download asks the checksum database for nothing. Go writes any missing content
# hashes and the go.mod hashes of versions it reads, even ones it does not select. Compare
# go.work.sum in the checkout and the copy: it is the only checksum file a workspace download
# writes. Unverified hashes stay in the copy, even if a kill prevents its cleanup.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
copy="$scratch/workspace"
mkdir -- "$copy"

copy_inputs() {
  local dir
  jq -j '.Use[]? | .DiskPath + "\u0000"' "$scratch/work.json" > "$scratch/uses" || return
  cp -- go.work "$copy/go.work" || return
  if [[ -e go.work.sum ]]; then
    cp -- go.work.sum "$copy/go.work.sum" || return
  fi
  while IFS= read -r -d '' dir; do
    if [[ "$dir" = /* || "/$dir/" = */../* ]]; then
      printf '::error file=go.work::use path must stay inside the root workspace: %s\n' "$dir" >&2
      return 1
    fi
    dir=${dir#./}
    mkdir -p -- "$copy/$dir" && cp -- "./$dir/go.mod" "$copy/$dir/go.mod" || return
    if [[ -e "$dir/go.sum" ]]; then
      cp -- "./$dir/go.sum" "$copy/$dir/go.sum" || return
    fi
  done < "$scratch/uses"
}

{
  goroot=$(GOWORK="$PWD/go.work" go env GOROOT) &&
    GOTOOLCHAIN=local "$goroot/bin/go" work edit -json go.work > "$scratch/work.json" &&
    copy_inputs &&
    (cd "$copy" && GOWORK="$copy/go.work" GOSUMDB=off GOTOOLCHAIN=local "$goroot/bin/go" mod download all >&2)
} 2> "$scratch/check.err" || {
  echo '::error::Could not download the root workspace in a copy, so this check says nothing about the committed checksums:' >&2
  cat -- "$scratch/check.err" >&2
  exit 1
}

compare_workspace_sum() {
  if [[ -e go.work.sum && -e "$copy/go.work.sum" ]]; then
    if cmp -s -- go.work.sum "$copy/go.work.sum"; then
      return 0
    elif [[ $? -ne 1 ]]; then
      echo '::error file=go.work.sum::Could not compare the checksum files.' >&2
      return 1
    fi
  elif [[ ! -e go.work.sum && ! -e "$copy/go.work.sum" ]]; then
    return 0
  fi

  if [[ -e go.work.sum ]]; then
    LC_ALL=C sort -- go.work.sum
  fi > "$scratch/before"
  if [[ -e "$copy/go.work.sum" ]]; then
    LC_ALL=C sort -- "$copy/go.work.sum"
  fi > "$scratch/after"
  LC_ALL=C comm -13 "$scratch/before" "$scratch/after" > "$scratch/added"
  if [[ -s "$scratch/added" ]]; then
    local count
    count=$(wc -l < "$scratch/added")
    printf '::error file=go.work.sum::no committed file holds %d of the checksums a download in the root workspace needs, so that download asks sum.golang.org for each of them and fails when that request fails. Run `go mod download all` at the repository root with the checksum database on, and commit what it writes. The lines:\n' "$count" >&2
    printf 'go.work.sum:\n' >&2
    sed 's/^/  /' "$scratch/added" >&2
  else
    echo '::error file=go.work.sum::Go rewrote go.work.sum in the copy without adding a checksum line. Run `go mod download all` at the repository root with the checksum database on, and commit what it writes.' >&2
  fi
  return 1
}

compare_workspace_sum
printf 'no checksum is missing: go mod download all in a copy of the root workspace added nothing to go.work.sum\n'
