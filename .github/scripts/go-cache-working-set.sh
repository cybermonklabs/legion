#!/usr/bin/env bash
# Keeps in Go's build cache only the entries one fill's build used, and in its module cache no
# module zip, so the entry .github/workflows/ci-caches.yaml saves holds that build's working set
# and does not grow.
#
#   go-cache-working-set.sh begin <state-dir>
#   go-cache-working-set.sh end <state-dir>
#   go-cache-working-set.sh zips
#
# Run `begin` after the restore and before the build, `end` after the build and before the save,
# each with the GOCACHE the build uses (the script asks `go env GOCACHE`). <state-dir> is the
# script's own scratch directory for that pair; it must not exist before `begin`. A relative
# <state-dir> is taken from the current directory. `end` also does what `zips` does; run `zips`
# alone before the save of a fill that builds from nothing and so has no `begin`.
#
# zips deletes every module zip under `go env GOMODCACHE`/cache/download and prints how many and
# how large. Go reads a module from the tree it extracted it to; the zip is read only to extract
# that tree again when it is missing, and by `go mod download` and `go mod verify`, which no job
# that restores these entries runs. The .ziphash, .mod and .info files beside each zip stay: a
# build checks the .ziphash (cmd/go/internal/modfetch/fetch.go, DownloadDir). A zip does not
# compress again, so it is stored at its full size (.github/ci-notes/ci-caches.md, "Why the fills
# delete the module zips", has the measurements).
#
# Why: a fill restores the newest entry of its family, builds on it and saves the result. Each
# build adds the objects of every package whose inputs changed since that entry, and nothing
# takes the replaced objects out again: Go's own trim deletes only what no build has used for
# five days. So the entry grew with every push to main (go-envoy-test 690 MB on 2026-11-22,
# 2,039 MB at 19:18Z on 2026-11-23), and every job that restores it downloads all of it.
#
# How Go marks what it uses (cmd/go/internal/cache/cache.go of go1.27.3, the Go this repository
# pins): a cache hit reads the action file `<id>-a` (DiskCache.get) and the output `<out>-d`
# (DiskCache.OutputFile, a directory for a cached executable), and markUsed sets the file's
# modification time to now, but only when that time is an hour old or more (mtimeInterval). A
# miss writes both files new. A restored entry keeps the times of the fill that saved it, often
# minutes old, so its files would not be marked. So:
#
#   begin  lists the entries as they stand, sets the modification time of each to two hours ago,
#          past mtimeInterval so the build marks what it uses, and short of Trim's cutoff (trimLimit,
#          five days, plus mtimeInterval) so Go's own trim, which can run at the end of any go
#          command of the build, deletes none of them; then writes its mark, a minute old. One
#          pair per fill job: no job fills two families, so no restore lands on another build's
#          files (.github/ci-notes/ci.md#a-cache-restore-comes-before-every-step-that-can-write-a-path-it-names).
#   end    deletes every entry the build did not mark or write: an `-a` or `-d` file or directory
#          in one of the 256 two-hex directories whose time is not newer than the mark, unless a
#          kept action file names it as its output (a miss whose output another action already
#          wrote does not mark that output). It leaves everything else in GOCACHE alone (trim.txt,
#          README, the fuzz directory). It prints what it kept, deleted and found new. When the
#          restore held entries and the build marked none of them, it warns: a Go that no longer
#          marks what it reads would empty the entry on every fill, and every job that restores it
#          would build from nothing every other push.
set -euo pipefail

usage() {
  echo "usage: $0 begin <state-dir> | end <state-dir> | zips" >&2
  exit 2
}

fail() {
  echo "go-cache-working-set: $*" >&2
  exit 1
}

# entries <cache> [<find test>...]: every cache entry of <cache> that passes the find tests given,
# as paths relative to it, sorted. The one rule for what an entry is.
entries() {
  local cache=$1
  shift
  (cd "$cache" && find . -mindepth 2 -maxdepth 2 -path './[0-9a-f][0-9a-f]/*' \( -name '*-a' -o -name '*-d' \) \
    "$@" -printf '%P\n') | LC_ALL=C sort
}

# size <cache> <list>: the bytes of the entries <list> names, directories included.
size() {
  local cache=$1 list=$2
  if [[ ! -s $list ]]; then
    echo 0
    return
  fi
  (cd "$cache" && tr '\n' '\0' <"$list" | du -cb --files0-from=- | tail -n1 | cut -f1)
}

mb() {
  echo $((($1 + 524288) / 1048576))
}

# zips: deletes the module zips of `go env GOMODCACHE` and says how many and how large; the
# header says why. A module cache with no download directory holds no zip, and says so.
zips() {
  local modcache download deleted
  modcache=$(go env GOMODCACHE)
  [[ -n $modcache ]] || fail "go env GOMODCACHE names no directory."
  download=$modcache/cache/download
  if [[ ! -d $download ]]; then
    echo "go module cache $modcache: no $download, so no module zip to delete"
    return
  fi
  deleted=$(find "$download" -type f -name '*.zip' -printf '%s\n' -delete)
  echo "go module cache $modcache: deleted $(grep -c . <<<"$deleted" || true) module zips" \
    "($(mb "$(awk '{ s += $1 } END { print s + 0 }' <<<"$deleted")") MB), which no build reads"
}

(($# >= 1)) || usage
command=$1
case $command in
begin | end)
  (($# == 2)) || usage
  state=$(realpath -ms -- "$2")
  ;;
zips)
  (($# == 1)) || usage
  ;;
esac

case $command in
begin)
  [[ ! -e $state ]] || fail "$state exists already; give each begin/end pair a directory of its own."
  cache=$(go env GOCACHE)
  [[ -n $cache ]] || fail "go env GOCACHE names no directory."
  mkdir -p -- "$cache" "$state"
  printf '%s\n' "$cache" >"$state/cache"
  entries "$cache" >"$state/before"
  now=$(date +%s)
  (cd "$cache" && tr '\n' '\0' <"$state/before" | xargs -0 -r touch -m -d "@$((now - 7200))" --)
  # A minute back, between the aged entries and anything touched from now on: Linux stamps a
  # file with a coarse clock, so a file touched in the same tick as a mark made now would not be
  # newer than it.
  touch -m -d "@$((now - 60))" -- "$state/mark"
  echo "go build cache $cache: $(wc -l <"$state/before") entries, $(mb "$(size "$cache" "$state/before")") MB, marked unused until the build uses them"
  ;;
end)
  [[ -f $state/mark && -f $state/cache ]] || fail "$state holds no mark; run begin with it first."
  cache=$(<"$state/cache")
  [[ $cache == "$(go env GOCACHE)" ]] ||
    fail "go env GOCACHE is $(go env GOCACHE), but begin marked $cache; run both with the build's GOCACHE."
  entries "$cache" >"$state/now"
  entries "$cache" -newer "$state/mark" >"$state/used"
  # The outputs the used action files name: field 3 of "v1 <id> <out> <size> <time>".
  awk '/-a$/' "$state/used" | (cd "$cache" && tr '\n' '\0' | xargs -0 -r cat --) |
    awk '$1 == "v1" && $3 ~ /^[0-9a-f]+$/ { print substr($3, 1, 2) "/" $3 "-d" }' |
    LC_ALL=C sort -u >"$state/named"
  LC_ALL=C comm -12 "$state/now" "$state/named" | LC_ALL=C sort -u -m - "$state/used" >"$state/kept"
  LC_ALL=C comm -23 "$state/now" "$state/kept" >"$state/unused"
  LC_ALL=C comm -23 "$state/kept" "$state/before" >"$state/new"
  kept_bytes=$(size "$cache" "$state/kept")
  unused_bytes=$(size "$cache" "$state/unused")
  new_bytes=$(size "$cache" "$state/new")
  (cd "$cache" && tr '\n' '\0' <"$state/unused" | xargs -0 -r rm -rf --)
  echo "go build cache $cache: kept $(wc -l <"$state/kept") entries ($(mb "$kept_bytes") MB) this build used," \
    "$(wc -l <"$state/new") of them new ($(mb "$new_bytes") MB); deleted $(wc -l <"$state/unused") entries ($(mb "$unused_bytes") MB) it did not use"
  # The restored entries this build read: kept, and there at begin.
  reused=$(LC_ALL=C comm -12 "$state/kept" "$state/before" | wc -l)
  if [[ -s $state/before && $reused -eq 0 ]]; then
    echo "::warning::go-cache-working-set: the build marked none of the $(wc -l <"$state/before") entries the restore" \
      "held, so the saved entry holds only what it compiled. A restore of another Go version does this; so would a Go" \
      "that no longer marks the cache entries it reads (cmd/go/internal/cache's markUsed), which go-cache-working-set.test.sh checks."
  fi
  zips
  ;;
zips)
  zips
  ;;
*)
  usage
  ;;
esac
