#!/usr/bin/env bash
# Tests for `apt-archive-cache-key.sh`: the key the ~/apt-archives cache family is read and
# written under - by .github/workflows/dispatch-browser.yaml's shard and alone jobs, which
# restore the entry, and by .github/workflows/ci-caches.yaml's apt-debs job, which fills it on a
# push to main. The key is a pure function of one line of bun.lock and the runner's ImageOS and
# ImageVersion, so each case lays down a checkout of its own, sets or unsets those two, and
# asserts the string the script prints.
#
# What that string is worth: a key that changes in any way misses every cache entry already
# saved under the old one, and the install step then fetches 114 MB of .deb files again. So the
# first case holds the whole key, character for character, against the one a real run restored
# (run 42349037034, shard 1: `Cache hit for: apt-playwright-1.64.1-ubuntu24-20261101.360.1`),
# and the cases after it hold the two readings that are allowed to change it.
#
# The refusals matter as much: a missing version or a runner that set no image name would
# otherwise make a key that names nothing, which every job would then share and overwrite. Each
# refusal case asserts the exit status, that the message names what is missing, and that nothing
# reached stdout - the callers read stdout as the key.
#
# Each case runs a copy of the script from inside its own fixture, because the script reads the
# checkout it sits in: it cds to `dirname "$0"/../..` and reads bun.lock from there. So a fixture
# is a checkout root - a bun.lock, and the copy under .github/scripts/ - rather than a directory
# the script is started from. That is also how this suite is held to be a real test of the
# script: copy it, test-lib.sh and a deliberately broken script into a directory of their own,
# run it there, and the cases fail.
#
# NO CASE RUNS APT, PLAYWRIGHT OR A NETWORK CALL: each one is a bun.lock, two variables and the
# script.
#
# Run from anywhere: .github/scripts/apt-archive-cache-key.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Every fixture is built inside `dir=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a directory that failed to build is used anyway
# and the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
key_script="$script_dir/apt-archive-cache-key.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name> [version]: a checkout root holding a bun.lock and, under .github/scripts/, the
# copy of the script that case runs. With a version the lockfile holds the playwright-core entry
# for that version, surrounded by the neighbouring lines a real lockfile has; with none it holds
# a lockfile that never pinned Playwright. Echoes the root's path. A repeated name is refused
# here rather than surfacing as one case reading another's lockfile.
fixture() {
  local dir="$work/$1"
  if [ -e "$dir" ]; then
    echo "fixture $1 is already there, so two cases would share it" >&2
    return 1
  fi
  mkdir -p "$dir/.github/scripts"
  cp "$key_script" "$dir/.github/scripts/apt-archive-cache-key.sh"
  {
    echo '  "packages": {'
    echo '    "picocolors": ["picocolors@1.1.1", "", {}, "sha512-xxx"],'
    if [ "$#" -eq 2 ]; then
      echo "    \"playwright-core\": [\"playwright-core@$2\", \"\", { \"bin\": { \"playwright-core\": \"cli.js\" } }, \"sha512-yyy\"],"
    fi
    echo '    "zod": ["zod@3.24.1", "", {}, "sha512-zzz"],'
    echo '  }'
  } > "$dir/bun.lock"
  echo "$dir"
}

# run_key <dir> [NAME=VALUE …]: runs that checkout's copy of the script with ImageOS and
# ImageVersion unset except as the arguments set them, setting `out` to its stdout, `err` to its
# stderr and `status` to its exit code. Started from $work, which holds no bun.lock: the script
# reads the checkout it sits in, so no case depends on a working directory. Not a subshell, so
# all three survive the call.
run_key() {
  local dir="$1"
  shift
  set +e
  out=$(cd "$work" && env -u ImageOS -u ImageVersion "$@" "$dir/.github/scripts/apt-archive-cache-key.sh" 2>"$work/stderr")
  status=$?
  set -e
  err=$(cat "$work/stderr")
}

echo "case: a lockfile and a runner image give the key the cache is keyed on"
dir=$(fixture normal 1.64.1)
run_key "$dir" ImageOS=ubuntu24 ImageVersion=20261101.360.1
check "exits 0" "$(is "$status" 0)"
check "prints the key run 42349037034's shard 1 restored" \
  "$(is "$out" apt-playwright-1.64.1-ubuntu24-20261101.360.1)"
check "and says nothing else" "$(is "$err" "")"
keys=("$out")

echo "case: the version is read out of bun.lock"
# Same runner image, another pinned Playwright: the browsers and the apt list are that version's,
# so the entry must be another one.
other=$(fixture other-version 1.70.0)
run_key "$other" ImageOS=ubuntu24 ImageVersion=20261101.360.1
check "exits 0" "$(is "$status" 0)"
check "names that version" "$(is "$out" apt-playwright-1.70.0-ubuntu24-20261101.360.1)"
keys+=("$out")

echo "case: a new runner image is a new key"
# The .deb files of an entry are the versions that image's apt indexes held, so a bumped image
# must not read them back.
run_key "$dir" ImageOS=ubuntu24 ImageVersion=20261115.1.0
check "exits 0" "$(is "$status" 0)"
check "names the new image version" \
  "$(is "$out" apt-playwright-1.64.1-ubuntu24-20261115.1.0)"
keys+=("$out")
run_key "$dir" ImageOS=ubuntu26 ImageVersion=20261101.360.1
check "and a new ImageOS is a new key too" \
  "$(is "$out" apt-playwright-1.64.1-ubuntu26-20261101.360.1)"
keys+=("$out")
# The point of the three cases above: no two of those readings share an entry.
check "the four keys are four" \
  "$(is "$(printf '%s\n' "${keys[@]}" | sort -u | wc -l)" 4)"

echo "case: a lockfile with no playwright-core line is refused"
# The suite's Playwright is gone from the lockfile, or the entry's shape changed under the sed.
# Keying on an empty version would make one entry every version shares.
missing=$(fixture no-playwright)
run_key "$missing" ImageOS=ubuntu24 ImageVersion=20261101.360.1
check "exits 1" "$(is "$status" 1)"
check "names bun.lock and what it wanted from it" \
  "$(contains "$err" '^::error::no playwright-core version in bun.lock, so the apt archive cache key cannot name the Playwright version$')"
check "and prints no key" "$(is "$out" "")"

echo "case: a runner that set no ImageOS is refused by that name"
run_key "$dir" ImageVersion=20261101.360.1
check "exits 1" "$(is "$status" 1)"
check "names ImageOS" \
  "$(contains "$err" '^::error::the runner set no ImageOS, so the apt archive cache key cannot name the image$')"
check "and prints no key" "$(is "$out" "")"

echo "case: a runner that set no ImageVersion is refused by that name"
run_key "$dir" ImageOS=ubuntu24
check "exits 1" "$(is "$status" 1)"
check "names ImageVersion" \
  "$(contains "$err" '^::error::the runner set no ImageVersion, so the apt archive cache key cannot name the image$')"
check "and prints no key" "$(is "$out" "")"

echo "case: an empty ImageOS is a runner that set none"
# `env ImageOS=` sets the name with no value, which names no image either.
run_key "$dir" ImageOS= ImageVersion=20261101.360.1
check "exits 1" "$(is "$status" 1)"
check "and names ImageOS" "$(contains "$err" 'the runner set no ImageOS')"

echo "case: the checkout the script sits in decides the key, not the caller's directory"
# Not through run_key: this one starts the first fixture's copy from inside the second fixture,
# whose bun.lock pins another version. The script cds to its own ../.., so the key is the one of
# the checkout it sits in. Its head says it runs from anywhere, and that is what this holds:
# dispatch-browser.yaml's two steps and ci-caches.yaml's run it from the checkout root, while a
# person on a machine runs it from wherever they are, and reading the caller's lockfile would
# key the entry on whatever version that directory pins.
set +e
out=$(cd "$other" && env ImageOS=ubuntu24 ImageVersion=20261101.360.1 \
  "$dir/.github/scripts/apt-archive-cache-key.sh" 2>"$work/stderr")
status=$?
set -e
err=$(cat "$work/stderr")
check "exits 0" "$(is "$status" 0)"
check "names the version of the checkout it sits in, not the caller's" \
  "$(is "$out" apt-playwright-1.64.1-ubuntu24-20261101.360.1)"

summary "apt-archive-cache-key.sh"
