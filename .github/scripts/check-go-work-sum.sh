#!/usr/bin/env bash
# Fails when a Go build of this repository would need a checksum no committed file holds.
#
#   .github/scripts/check-go-work-sum.sh
#
# Why: a `go mod download` verifies each module against go.work.sum and each module's go.sum, and
# asks sum.golang.org, the checksum database, for every hash it does not find there. Two image
# builds died on that request on 2026-11-24: the worker image's after 864 s on `stream error
# INTERNAL_ERROR` for two modules (job 138667218057) and the listener image's go-licenses install
# at 18:42Z (job 138687314598). The hashes go.work.sum lacked (240 lines at main 0dbe504849a, 229
# module content lines at 77d3d14c) are what sent those builds to the database at all. With
# every hash committed, a build reads the proxy alone, and a database that fails cannot fail a
# build that never asks it.
#
# How: `go mod download -json all` reports, for each module of the workspace graph, two hashes:
# `Sum`, of its content (the `h1:` line), and `GoModSum`, of its go.mod. This check compares each
# against the committed files and names every module whose hash is in none of them. It downloads
# what the module cache lacks, with GOSUMDB=off, so it asks the checksum database for nothing: on
# a cold cache a module whose hash no file holds is downloaded unverified and named here, where
# a build would have asked the database for it. So neither a toolchain switch, nor an unreachable
# database, nor a cold module cache can fail this check for a reason that is not a missing hash.
# The committed hashes are read before the download, so a hash the download writes into
# go.work.sum cannot hide a missing one.
#
# It needs the Go this repository pins, because the graph is the one that Go resolves: CI's
# setup-go installs it, and a box whose `go` reaches it through a toolchain module serves too,
# since this check lets the switch happen.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

sums=$(mktemp)
graph=$(mktemp)
trap 'rm -f "$sums" "$graph"' EXIT

# Every committed hash: go.work.sum and each module's go.sum, as `<module> <version>[/go.mod] <hash>`.
{ cat go.work.sum; git ls-files -z '*go.sum' | xargs -0 -r cat; } > "$sums"

# A Go that switches to the pinned toolchain downloads it as a module and verifies it against the
# checksum database on every switch, which GOSUMDB=off refuses. So the switch happens once here,
# verified as a build verifies it, and the download below runs that toolchain's own go with
# GOTOOLCHAIN=local, which switches to nothing.
goroot=$(go env GOROOT 2>"$graph.err") && [ -x "$goroot/bin/go" ] || {
  echo "::error::the Go toolchain this workspace pins could not be started, so this check says nothing about the committed checksums:" >&2
  sed -n '1,10p' "$graph.err" >&2
  rm -f "$graph.err"
  exit 1
}
GOSUMDB=off GOTOOLCHAIN=local "$goroot/bin/go" mod download -json all > "$graph" 2>"$graph.err" || {
  echo "::error::the module graph could not be listed, so this check says nothing about the committed checksums:" >&2
  sed -n '1,10p' "$graph.err" >&2
  rm -f "$graph.err"
  exit 1
}
rm -f "$graph.err"

missing=$(
  python3 - "$graph" "$sums" <<'PY'
import json, sys

graph, sums = sys.argv[1], sys.argv[2]
held = set()
for line in open(sums, encoding="utf-8", errors="replace"):
    parts = line.split()
    if len(parts) == 3:
        held.add((parts[0], parts[1]))

seen = 0
gaps = []
decoder = json.JSONDecoder()
text = open(graph, encoding="utf-8", errors="replace").read()
index = 0
while index < len(text):
    while index < len(text) and text[index].isspace():
        index += 1
    if index >= len(text):
        break
    module, index = decoder.raw_decode(text, index)
    path, version = module.get("Path"), module.get("Version")
    if not path or not version:  # a module of the workspace itself, which has no hash to commit
        continue
    seen += 1
    # A replaced module is reported under the path and version it is replaced by, which are the
    # ones its hashes are committed under; a directory replacement is a workspace module, above.
    if module.get("Sum") and (path, version) not in held:
        gaps.append(f"{path} {version}")
    if module.get("GoModSum") and (path, f"{version}/go.mod") not in held:
        gaps.append(f"{path} {version}/go.mod")

print(seen)
for gap in sorted(set(gaps)):
    print(gap)
PY
)

count=$(printf '%s\n' "$missing" | sed -n '1p')
gaps=$(printf '%s\n' "$missing" | sed -n '2,$p')

if [ -z "$count" ] || [ "$count" -eq 0 ]; then
  echo "::error::the module graph came back empty, so this check enumerated nothing and says nothing about the committed checksums" >&2
  exit 1
fi

if [ -n "$gaps" ]; then
  echo "::error file=go.work.sum::$(printf '%s\n' "$gaps" | wc -l) of the $count modules this workspace needs have a checksum in no committed file, so every build that downloads asks sum.golang.org for them and fails when that request fails (the worker image's job 138667218057 and the listener image's 138687314598 died that way). Run \`go mod download all\` in the repository root and commit go.work.sum. The modules:" >&2
  printf '%s\n' "$gaps" | sed 's/^/  /' >&2
  exit 1
fi

echo "every checksum the workspace's $count modules need is committed (go.work.sum and the modules' go.sum files)"
