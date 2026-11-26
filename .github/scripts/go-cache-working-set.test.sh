#!/usr/bin/env bash
# Tests for `go-cache-working-set.sh`: after a fill's build, Go's build cache holds the entries
# that build used and no others, and a build on what is left compiles only what changed.
#
# The first cases run the real go over a scratch module, with a GOCACHE of their own: they are the
# check that the Go this repository pins still marks the cache entries a build uses
# (cmd/go/internal/cache/cache.go's markUsed), which is what the script reads; a Go that stopped
# would fail them. The later cases run the script against a stand-in `go` that only answers
# `go env GOCACHE` and `go env GOMODCACHE`, over cache files written here, for what no small real
# build reaches: an output another action wrote before, the second family of a job, the files that
# are not entries, a build that reads none of the restored entries, relative state directories,
# and the module zips `end` and `zips` delete.
#
# Run from anywhere: .github/scripts/go-cache-working-set.test.sh
# CI runs it in ci.yaml's test job, after its setup-go.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
prune="$script_dir/go-cache-working-set.sh"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'chmod -R u+w "$work" 2>/dev/null; rm -rf "$work"' EXIT

# run <args...>: the script under test; leaves its merged output in $out and its status in $status.
run() {
  set +e
  out=$("$prune" "$@" 2>&1)
  status=$?
  set -e
}

# entries <cache>: how many cache entries <cache> holds.
entries() {
  find "$1" -mindepth 2 -maxdepth 2 \( -name '*-a' -o -name '*-d' \) | wc -l | tr -d ' '
}

# lacks <text> <regex>: contains' opposite, for check.
lacks() { grep -q -- "$2" <<<"$1" && echo false || echo true; }

# --- the real go ---------------------------------------------------------------------------------
module=$work/module
mkdir -p "$module/a" "$module/b"
# source_file <file> <text>: writes a source file a minute old. go records some cache entries only for
# files at least two seconds old (cmd/go's modTimeCutoff), so a build straight after a write would
# add on the next fill what this one skipped; a fill's checkout is minutes old by its build.
source_file() {
  printf '%s\n' "$2" >"$1"
  touch -m -d '1 minute ago' "$1"
}
source_file "$module/go.mod" $'module example.com/m\n\ngo 1.21'
source_file "$module/a/a.go" $'package a\n\nfunc A() int { return 1 }'
source_file "$module/b/b.go" $'package b\n\nimport "example.com/m/a"\n\nfunc B() int { return a.A() + 1 }'
export GOCACHE=$work/real-cache GOFLAGS='' GOENV=off GOTOOLCHAIN=local GOPATH=$work/gopath
# build: the stand-in fill's build, with the commands it ran on stderr in $work/x.log (-x).
build() {
  (cd "$module" && go build -x ./... 2>"$work/x.log")
}
# compiled: the packages the last build compiled, by import path.
compiled() {
  grep -o -- '-p example.com/m/[a-z]*' "$work/x.log" | sed 's/^-p //' | sort -u | tr '\n' ' ' || true
}
build
cold=$(entries "$GOCACHE")

echo "case: a fill after a change keeps what its build used and deletes the replaced objects"
source_file "$module/a/a.go" $'package a\n\nfunc A() int { return 2 }'
run begin "$work/fill1"
check "begin exits 0" "$(is "$status" 0)"
check "begin names the cache and what it found" "$(contains "$out" "^go build cache $GOCACHE: $cold entries, ")"
build
run end "$work/fill1"
echo "  $out"
check "end exits 0" "$(is "$status" 0)"
check "names what it kept, what was new and what it deleted" \
  "$(contains "$out" "kept [1-9][0-9]* entries (.* MB) this build used, [1-9][0-9]* of them new (.* MB); deleted [1-9][0-9]* entries")"
fill1=$(entries "$GOCACHE")
check "holds no more entries than the cache it began with" \
  "$( ((fill1 <= cold)) && echo true || echo false)"
check "no entry it deleted is still there" \
  "$(is "$(LC_ALL=C comm -12 <(cd "$GOCACHE" && find . -mindepth 2 -maxdepth 2 \( -name '*-a' -o -name '*-d' \) -printf '%P\n' | LC_ALL=C sort) "$work/fill1/unused" | wc -l | tr -d ' ')" 0)"

echo "case: the next fill at the same commit compiles nothing and adds nothing"
run begin "$work/fill2"
build
check "the build compiled no package of the module" "$(is "$(compiled)" "")"
run end "$work/fill2"
echo "  $out"
check "keeps every entry, none new" "$(contains "$out" "kept $(entries "$GOCACHE") entries (.* MB) this build used, 0 of them new")"
check "and does not warn: the real go marked the restored entries it read" "$(lacks "$out" '::warning::')"

echo "case: a job that changes one package, on the pruned cache, compiles that package and what imports it"
source_file "$module/b/b.go" $'package b\n\nimport "example.com/m/a"\n\nfunc B() int { return a.A() + 3 }'
run begin "$work/fill3"
build
check "the build compiled b alone, not a" "$(is "$(compiled)" "example.com/m/b ")"
run end "$work/fill3"
echo "  $out"
# A build that compiles writes a few entries a build that hits never reads (cmd/go's intermediate
# outputs), so the next fill at the same commit, which only hits, drops them; the one after that
# finds nothing to drop and nothing to add: the cache is that commit's working set.
run begin "$work/fill4"
build
run end "$work/fill4"
echo "  $out"
settled=$(entries "$GOCACHE")
run begin "$work/fill5"
build
run end "$work/fill5"
echo "  $out"
if ! grep -q 'deleted 0 entries' <<<"$out"; then
  echo "  fill5 deleted:"; sed 's/^/    /' "$work/fill5/unused"
  echo "  fill4 kept:"; sed 's/^/    /' "$work/fill4/kept"
  echo "  fill5 used:"; sed 's/^/    /' "$work/fill5/used"
  echo "  compiled by fill5: $(compiled)"
fi
check "two fills after the change, the cache holds that build's working set and no more" \
  "$(contains "$out" "kept $settled entries (.* MB) this build used, 0 of them new (.* MB); deleted 0 entries")"

echo "case: without the script, the same changes grow the cache"
grown=$work/grown-cache
cp -a "$GOCACHE" "$grown"
source_file "$module/a/a.go" $'package a\n\nfunc A() int { return 4 }'
GOCACHE=$grown build
check "the cache holds more entries than the pruned one" "$( (($(entries "$grown") > settled)) && echo true || echo false)"
unset GOCACHE GOFLAGS GOENV GOTOOLCHAIN GOPATH

# --- a stand-in go over cache files written here -------------------------------------------------
# fake_cache <dir>: a cache directory with a stand-in `go` beside it on PATH, which answers
# `go env GOCACHE` with <dir>/cache, `go env GOMODCACHE` with <dir>/mod and nothing else.
fake_cache() {
  local dir=$1
  mkdir -p "$dir/cache/00" "$dir/cache/ab" "$dir/cache/cd" "$dir/cache/fuzz/x" "$dir/bin"
  printf '#!/usr/bin/env bash\n[ "$*" = "env GOCACHE" ] && echo %q && exit 0\n[ "$*" = "env GOMODCACHE" ] && echo %q && exit 0\nexit 2\n' \
    "$dir/cache" "$dir/mod" >"$dir/bin/go"
  chmod +x "$dir/bin/go"
}
hexid() { printf '%064d' "$1"; }
# action <cache> <dir> <id> <output id>: an action file naming that output, as Go writes one.
action() {
  printf 'v1 %s %s %20d %20d\n' "$3" "$4" 10 1 >"$1/$2/$3-a"
}

echo "case: an output a kept action names stays, though no build touched it"
fake=$work/fake1
fake_cache "$fake"
c=$fake/cache
out_id=ab$(hexid 1 | cut -c3-)
used_id=00$(hexid 2 | cut -c3-)
stale_id=cd$(hexid 3 | cut -c3-)
printf 'object' >"$c/ab/$out_id-d"
action "$c" 00 "$used_id" "$out_id"
action "$c" cd "$stale_id" "cd$(hexid 4 | cut -c3-)"
printf 'old' >"$c/cd/cd$(hexid 4 | cut -c3-)-d"
printf '123\n' >"$c/trim.txt"
printf 'readme\n' >"$c/README"
printf 'corpus\n' >"$c/fuzz/x/seed"
PATH=$fake/bin:$PATH run begin "$fake/state"
check "begin exits 0" "$(is "$status" 0)"
now=$(date +%s)
age=$((now - $(stat -c %Y "$c/ab/$out_id-d")))
check "begin ages the entries past Go's hour, short of its five-day trim" \
  "$( ((age >= 3600 && age < 5 * 86400)) && echo true || echo false)"
touch "$c/00/$used_id-a" # the build reads this action and finds its output already there
PATH=$fake/bin:$PATH run end "$fake/state"
check "end exits 0" "$(is "$status" 0)"
check "keeps the action the build read" "$( [ -e "$c/00/$used_id-a" ] && echo true || echo false)"
check "keeps the output that action names" "$( [ -e "$c/ab/$out_id-d" ] && echo true || echo false)"
check "deletes the action no build read" "$( [ -e "$c/cd/$stale_id-a" ] && echo false || echo true)"
check "deletes the output nothing kept names" "$( [ -e "$c/cd/cd$(hexid 4 | cut -c3-)-d" ] && echo false || echo true)"
check "leaves trim.txt, README and the fuzz directory alone" \
  "$( [ -f "$c/trim.txt" ] && [ -f "$c/README" ] && [ -f "$c/fuzz/x/seed" ] && echo true || echo false)"

echo "case: a build that reads none of the restored entries is named, and an empty restore is not"
fake=$work/fake5
fake_cache "$fake"
c=$fake/cache
printf 'object' >"$c/ab/ab$(hexid 7 | cut -c3-)-d"
PATH=$fake/bin:$PATH run begin "$fake/state"
printf 'compiled' >"$c/cd/cd$(hexid 8 | cut -c3-)-d" # the build writes one entry and reads none
PATH=$fake/bin:$PATH run end "$fake/state"
check "end exits 0" "$(is "$status" 0)"
check "warns that the build marked none of the restored entries" \
  "$(contains "$out" '^::warning::go-cache-working-set: the build marked none of the 1 entries the restore held')"
check "and keeps what the build wrote" "$( [ -e "$c/cd/cd$(hexid 8 | cut -c3-)-d" ] && echo true || echo false)"
fake=$work/fake6
fake_cache "$fake"
c=$fake/cache
PATH=$fake/bin:$PATH run begin "$fake/state"
printf 'compiled' >"$c/cd/cd$(hexid 9 | cut -c3-)-d"
PATH=$fake/bin:$PATH run end "$fake/state"
check "an end after a restore that held nothing does not warn" "$(lacks "$out" '::warning::')"

echo "case: relative state directories are taken from the current directory"
fake=$work/fake7
fake_cache "$fake"
c=$fake/cache
printf 'object' >"$c/ab/ab$(hexid 10 | cut -c3-)-d"
# in_fake <args...>: the script run from $fake, its exit status on stdout.
in_fake() { (cd "$fake" && PATH=$fake/bin:$PATH "$prune" "$@" >/dev/null 2>&1) && echo 0 || echo $?; }
check "begin with a relative state directory exits 0" "$(is "$(in_fake begin first)" 0)"
check "and writes its list there" "$( [ -s "$fake/first/before" ] && echo true || echo false)"
touch "$c/ab/ab$(hexid 10 | cut -c3-)-d"
check "end with it exits 0" "$(is "$(in_fake end first)" 0)"
check "and keeps the entry the build used" "$( [ -e "$c/ab/ab$(hexid 10 | cut -c3-)-d" ] && echo true || echo false)"

echo "case: end and zips delete the module zips, and keep the trees and the files beside them"
fake=$work/fake8
fake_cache "$fake"
v=$fake/mod/cache/download/example.com/x/@v
mkdir -p "$v" "$fake/mod/example.com/x@v1.0.0" "$fake/mod/cache/download/example.com/y/@v"
for f in v1.0.0.zip v1.0.0.ziphash v1.0.0.mod v1.0.0.info list; do printf '%s\n' "$f" >"$v/$f"; done
printf 'package x\n' >"$fake/mod/example.com/x@v1.0.0/x.go"
printf 'zip\n' >"$fake/mod/cache/download/example.com/y/@v/v2.0.0.zip"
PATH=$fake/bin:$PATH run begin "$fake/state"
PATH=$fake/bin:$PATH run end "$fake/state"
check "end exits 0" "$(is "$status" 0)"
check "and names the zips it deleted" "$(contains "$out" "^go module cache $fake/mod: deleted 2 module zips (0 MB), which no build reads$")"
check "and no zip is left" "$(is "$(find "$fake/mod" -name '*.zip' | wc -l | tr -d ' ')" 0)"
check "and the .ziphash, .mod, .info and list beside it stay" \
  "$( [ -f "$v/v1.0.0.ziphash" ] && [ -f "$v/v1.0.0.mod" ] && [ -f "$v/v1.0.0.info" ] && [ -f "$v/list" ] && echo true || echo false)"
check "and the extracted tree stays" "$( [ -f "$fake/mod/example.com/x@v1.0.0/x.go" ] && echo true || echo false)"
printf 'zip\n' >"$v/v1.0.0.zip"
PATH=$fake/bin:$PATH run zips
check "zips alone exits 0" "$(is "$status" 0)"
check "and deletes the zip a later download wrote" "$( [ -e "$v/v1.0.0.zip" ] && echo false || echo true)"
check "and counts it" "$(contains "$out" "deleted 1 module zips")"
PATH=$fake/bin:$PATH run zips
check "a second zips counts none" "$(contains "$out" "deleted 0 module zips (0 MB)")"
PATH=$fake/bin:$PATH run zips "$fake/state"
check "zips given a state directory is a usage error" "$(is "$status" 2)"
fake=$work/fake9
fake_cache "$fake"
PATH=$fake/bin:$PATH run zips
check "zips on a module cache with no download directory exits 0" "$(is "$status" 0)"
check "and says it found no zip to delete" "$(contains "$out" "no $fake/mod/cache/download, so no module zip to delete")"

echo "case: the script refuses what would prune the wrong cache or a cache it never marked"
fake=$work/fake3
fake_cache "$fake"
PATH=$fake/bin:$PATH run begin "$fake/state"
PATH=$fake/bin:$PATH run begin "$fake/state"
check "a second begin with the same state directory fails" "$(is "$status" 1)"
check "and names the directory" "$(contains "$out" "$fake/state exists already")"
PATH=$fake/bin:$PATH run end "$fake/never"
check "an end with no begin fails" "$(is "$status" 1)"
check "and says so" "$(contains "$out" "holds no mark; run begin with it first")"
other=$work/fake4
fake_cache "$other"
PATH=$other/bin:$PATH run end "$fake/state"
check "an end under another GOCACHE fails" "$(is "$status" 1)"
check "and names both caches" "$(contains "$out" "go env GOCACHE is $other/cache, but begin marked $fake/cache")"
PATH=$fake/bin:$PATH run begin "$fake/later" "$fake/state"
check "a begin given a second state directory is a usage error" "$(is "$status" 2)"

summary "go-cache-working-set.sh"
