#!/usr/bin/env bash
# Tests for `check-playwright-version.sh`: the head it passes, each refusal it names, and the
# repository's own tree, which the last case runs the real check over.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script, and the reference grammar it reads an image reference with (ci_images.py), into it, so
# the code under test is the code CI runs, not a copy that can drift: the script resolves its
# root from its own path (`cd "$(dirname "$0")/../.."`), which the symlink puts at the fixture
# root, and imports the module from the .github/scripts under it.
#
# Run from anywhere: .github/scripts/check-playwright-version.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-playwright-version.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The Playwright the fixture tree agrees on, and the one a drift case moves a single side to.
# Neither is the release the repository itself resolves, so a case that read the real tree
# instead of its fixture would fail rather than pass by accident.
FIXTURE_VERSION=1.59.2
OTHER_VERSION=1.60.3
# Made-up digests: nothing pulls them, and what each case reads is the row they match.
BASE_DIGEST=sha256:0a1b2c3d4e5f60710a1b2c3d4e5f60710a1b2c3d4e5f60710a1b2c3d4e5f6071
WEED_DIGEST=sha256:9f8e7d6c5b4a30219f8e7d6c5b4a30219f8e7d6c5b4a30219f8e7d6c5b4a3021

# fixture <name>: a tree that passes — a bun.lock resolving Playwright, the row of the base
# image that copies that same release, and a browser Dockerfile built on that row's digest, with
# a named stage above the base as the repository's own file has. Echoes the fixture root. Each
# case names its own tree: two cases sharing a name would share a tree, so a repeated name is
# refused here rather than surfacing as the `ln -s` below failing.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/docker"
  ln -s "$check_script" "$root/.github/scripts/check-playwright-version.sh"
  ln -s "$script_dir/ci_images.py" "$root/.github/scripts/ci_images.py"
  cat > "$root/bun.lock" <<LOCK
{
  "lockfileVersion": 1,
  "packages": {
    "@playwright/test": ["@playwright/test@$FIXTURE_VERSION", "", { "dependencies": { "playwright": "$FIXTURE_VERSION" } }, "sha512-fixture"],
    "playwright": ["playwright@$FIXTURE_VERSION", "", { "dependencies": { "playwright-core": "$FIXTURE_VERSION" } }, "sha512-fixture"],
    "playwright-core": ["playwright-core@$FIXTURE_VERSION", "", {}, "sha512-fixture"],
  }
}
LOCK
  cat > "$root/.github/ci-images.yaml" <<YAML
images:
  - upstream: docker.io/chrislusf/seaweedfs:3.97
    tag: seaweedfs-3.97-9f8e7d6c
    digest: $WEED_DIGEST

  - upstream: mcr.microsoft.com/playwright:v$FIXTURE_VERSION-noble
    tag: playwright-v$FIXTURE_VERSION-noble-0a1b2c3d
    digest: $BASE_DIGEST
YAML
  cat > "$root/.github/docker/browser.Dockerfile" <<YAML
FROM ghcr.io/cybermonklabs/legion/ci@$WEED_DIGEST AS weed

FROM ghcr.io/cybermonklabs/legion/ci@$BASE_DIGEST

COPY --from=weed /usr/bin/weed /usr/local/bin/weed
YAML
  echo "$root"
}

# run_check <root>: runs the script under test, setting `out` to its combined output and
# `status` to its exit code. Not a subshell, so both survive the call.
run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-playwright-version.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a tree whose lockfile and browser image name one Playwright"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "names both versions and says they agree" \
  "$(contains "$out" "resolves playwright@$FIXTURE_VERSION and the browser image carries Playwright $FIXTURE_VERSION")"
check "names the row it read the image's version from" \
  "$(contains "$out" "the copy of mcr.microsoft.com/playwright:v$FIXTURE_VERSION-noble")"
check "and the base FROM it read, not the stage above it" \
  "$(contains "$out" '.github/docker/browser.Dockerfile:3')"

echo "case: the repository's own tree"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "the real tree passes" "$(is "$status" 0)"
check "says so" "$(contains "$out" 'they agree')"

echo "case: bun.lock moved to another Playwright and the row left behind"
root=$(fixture lock-drift)
sed -i "s/playwright@$FIXTURE_VERSION/playwright@$OTHER_VERSION/g" "$root/bun.lock"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names what the lockfile resolves" "$(contains "$out" "bun.lock resolves playwright@$OTHER_VERSION")"
check "and what the image carries" "$(contains "$out" "the browser image carries Playwright $FIXTURE_VERSION")"
check "reported at the lockfile's own entry" "$(contains "$out" '^::error file=bun.lock,line=5::bun.lock:5:')"
check "and names the row the image's version came from" \
  "$(contains "$out" ".github/ci-images.yaml:6 says the base FROM at .github/docker/browser.Dockerfile:3")"

echo "case: the row moved to another Playwright and bun.lock left behind"
root=$(fixture row-drift)
sed -i "s|playwright:v$FIXTURE_VERSION-noble|playwright:v$OTHER_VERSION-noble|" "$root/.github/ci-images.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names what the lockfile resolves" "$(contains "$out" "bun.lock resolves playwright@$FIXTURE_VERSION")"
check "and what the image carries" "$(contains "$out" "the browser image carries Playwright $OTHER_VERSION")"

echo "case: a base FROM whose digest no row holds"
root=$(fixture digest-unknown)
sed -i "s|$BASE_DIGEST|sha256:1111111111111111111111111111111111111111111111111111111111111111|" \
  "$root/.github/docker/browser.Dockerfile"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the digest" \
  "$(contains "$out" 'names sha256:1111111111111111111111111111111111111111111111111111111111111111, which no row of .github/ci-images.yaml holds')"
check "at the base FROM's line" "$(contains "$out" '^::error file=.github/docker/browser.Dockerfile,line=3::')"

echo "case: a tree with no browser Dockerfile"
root=$(fixture no-dockerfile)
rm "$root/.github/docker/browser.Dockerfile"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "says why there is nothing to compare" \
  "$(contains "$out" 'no .github/docker/browser.Dockerfile: no CI image carries')"

echo "case: a base FROM naming a tag rather than a digest"
root=$(fixture base-tagged)
sed -i "s|ghcr.io/cybermonklabs/legion/ci@$BASE_DIGEST|mcr.microsoft.com/playwright:v$FIXTURE_VERSION-noble|" \
  "$root/.github/docker/browser.Dockerfile"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says the base names no digest" "$(contains "$out" 'which names no digest')"
check "and what to write instead" "$(contains "$out" 'write ghcr.io/cybermonklabs/legion/ci@<digest>')"

echo "case: a bun.lock resolving no playwright"
root=$(fixture lock-without-playwright)
# The client's own entry, not @playwright/test's and not playwright-core's: those two resolve
# alongside it, and neither is the package this gate reads the release out of.
sed -i '/^    "playwright": \["playwright@/d' "$root/bun.lock"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says the lockfile resolves no playwright" \
  "$(contains "$out" 'bun.lock resolves no playwright')"
check "and names the entry it looked for" \
  "$(contains "$out" 'the entry reads "playwright": \["playwright@<version>"')"

echo "case: a Dockerfile whose every FROM opens a named stage"
root=$(fixture no-base-from)
sed -i "s|^FROM ghcr.io/cybermonklabs/legion/ci@$BASE_DIGEST\$|FROM ghcr.io/cybermonklabs/legion/ci@$BASE_DIGEST AS base|" \
  "$root/.github/docker/browser.Dockerfile"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says the file holds no base FROM" \
  "$(contains "$out" '.github/docker/browser.Dockerfile holds no base FROM')"
check "and says what makes every FROM a stage" "$(contains "$out" 'opens a named build stage (AS <name>)')"

echo "case: a base FROM landing on a row this repository builds"
root=$(fixture base-built-row)
sed -i "s|upstream: mcr.microsoft.com/playwright:v$FIXTURE_VERSION-noble|built: .github/docker/browser.Dockerfile|" \
  "$root/.github/ci-images.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the row's kind" \
  "$(contains "$out" 'carries built: .github/docker/browser.Dockerfile and no upstream:')"
check "and says why no release can be read from it" \
  "$(contains "$out" 'an image this repository builds is a copy of no published Playwright')"
check "reported at the row's built: key" \
  "$(contains "$out" '^::error file=.github/ci-images.yaml,line=6::.github/ci-images.yaml:6:')"

echo "case: a row whose upstream tag carries no Playwright release"
root=$(fixture tag-without-release)
sed -i "s|playwright:v$FIXTURE_VERSION-noble|playwright:latest|" "$root/.github/ci-images.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the tag it could not read a release out of" \
  "$(contains "$out" 'whose tag latest carries no Playwright release')"
check "and says how Playwright publishes its image" \
  "$(contains "$out" 'Playwright publishes its image as v<X.Y.Z>-<variant>')"
check "reported at the row's upstream: key" \
  "$(contains "$out" '^::error file=.github/ci-images.yaml,line=6::.github/ci-images.yaml:6:')"

summary "check-playwright-version.sh"
