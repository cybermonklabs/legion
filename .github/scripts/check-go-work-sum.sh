#!/usr/bin/env bash
# Fails when a Go build of this repository would need a checksum no committed file holds.
#
#   .github/scripts/check-go-work-sum.sh
#
# Why: a `go mod download` verifies each module against go.work.sum and each module's go.sum, and
# asks sum.golang.org, the checksum database, for every hash it does not find there. Two image
# builds died on that request on 2026-11-24: the worker image's after 864 s on `stream error
# INTERNAL_ERROR` for two modules (job 138667218057) and the listener image's go-licenses install
# at 18:42Z (job 138687314598). The 240 go.mod hashes go.work.sum lacked are what sent those builds
# to the database at all. With every hash committed, a build reads the proxy alone, and a database
# that fails cannot fail a build that never asks it.
#
# How: `go mod download -json all` reports, for each module of the workspace graph, two hashes:
# `Sum`, of its content (the `h1:` line), and `GoModSum`, of its go.mod. This check compares each
# against the committed files and names every module whose hash is in none of them. It asks the
# checksum database for nothing and downloads nothing it does not already hold, so it cannot be
# confused by a toolchain switch, an unreachable database or a module cache that is cold - the four
# ways the first version of this check failed for reasons that were not a missing hash.
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

go mod download -json all > "$graph" 2>"$graph.err" || {
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
    if not path or not version or module.get("Main"):
        continue
    seen += 1
    # A replaced module is verified under the path it is replaced by, and a local replacement has
    # no hash at all; `go mod download` reports the replacement in Replace.
    target = module.get("Replace") or module
    path, version = target.get("Path", path), target.get("Version", version)
    if not version:  # a directory replacement: nothing to verify
        continue
    if target.get("Sum") and (path, version) not in held:
        gaps.append(f"{path} {version}")
    if target.get("GoModSum") and (path, f"{version}/go.mod") not in held:
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
