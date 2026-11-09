#!/usr/bin/env bash
# Tests for `check-ci-tools.sh`: one red case per refusal it makes and one green case beside it,
# so each rule is shown to refuse what it should and to pass what it should.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into its .github/scripts/, so the code under test is the code CI runs, not a copy that
# can drift: the script resolves its root from its own path (`cd "$(dirname "$0")/../.."`), which
# the symlink puts at the fixture root.
# A fixture holds the whole green tree — the manifest, the two pins the rows agree with, a
# workflow and an action — and a red case changes one thing in it.
#
# NO CASE REACHES THE NETWORK. The check under test reads the tree alone and has no mode that
# reads the package, because .github/scripts/mirror-ci-tools.sh reads the package on every run of
# the ci-images job and is the only judge of a copy's state. The last case runs the check over
# this repository's own tree, which is the same tree read.
#
# Run from anywhere: .github/scripts/check-ci-tools.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-ci-tools.sh"
repository=$(cd "$script_dir/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The digests the fixtures use. Made up: nothing fetches them, and what each case reads is the
# row and the pin they match.
TOOL=0a1b2c3d4e5f60710a1b2c3d4e5f60710a1b2c3d4e5f60710a1b2c3d4e5f6071
MISE=1b2c3d4e5f6071821b2c3d4e5f6071821b2c3d4e5f6071821b2c3d4e5f607182
JJ=2c3d4e5f607182932c3d4e5f607182932c3d4e5f607182932c3d4e5f60718293
BUN=3d4e5f60718293a43d4e5f60718293a43d4e5f60718293a43d4e5f60718293a4
REGISTRY=ghcr.io/example/fixture/ci
TOOL_URL=https://github.com/example/tool/releases/download/v1.0.0/tool_1.0.0_linux_amd64.tar.gz
MISE_URL=https://github.com/example/mise/releases/download/v7.7.7/mise-v7.7.7-linux-x64.tar.gz
JJ_URL=https://github.com/example/jj/releases/download/v5.5.5/jj-v5.5.5-x86_64.tar.gz
BUN_URL=https://registry.npmjs.org/@oven/bun-linux-x64/-/bun-linux-x64-9.9.9.tgz

# pins <root> [bun version] [mise version] [mise sha256] [jj tool]: the two files rule 2 holds
# the rows to. Each value defaults to the one the green rows agree with, so the fixture names
# none of them and a red case names only the values up to the one it changes.
pins() {
  local root=$1 bun=${2:-9.9.9} mise_version=${3:-v7.7.7} mise_sha256=${4:-$MISE} \
    jj_tool=${5:-github:example/jj@5.5.5}
  printf '%s\n' "$bun" > "$root/.bun-version"
  cat > "$root/packages/daemon/docker/worker.Dockerfile" <<DOCKERFILE
# syntax=docker/dockerfile:1
ARG MISE_VERSION=$mise_version
ARG MISE_SHA256=$mise_sha256
ARG JJ_TOOL=$jj_tool
FROM scratch
DOCKERFILE
}

# fixture <name>: a green tree — four rows, one of each shape the manifest holds, the pins they
# agree with, and a workflow and an action that fetch a tool through the helper and name no URL
# of their own. Echoes the fixture root. Each case names its own tree: two cases sharing a name
# would share a tree, so a repeated name is refused here rather than surfacing as the `ln -s`
# below failing.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/.github/actions/install-tool" \
    "$root/packages/daemon/docker"
  ln -s "$check_script" "$root/.github/scripts/check-ci-tools.sh"
  pins "$root"
  cat > "$root/.github/ci-tools.yaml" <<YAML
registry: $REGISTRY

tools:
  - name: tool
    version: 1.0.0
    digest: $TOOL
    digest_of: the release tarball, whole
    primary: $TOOL_URL
    second_source: ghcr-mirror
    max_time: 60
    mirror_from: $TOOL_URL
    mirror_member: ""

  - name: mise
    version: 7.7.7
    digest: $MISE
    digest_of: the release tarball, whole
    primary: $MISE_URL
    second_source: ghcr-mirror
    max_time: 60
    mirror_from: $MISE_URL
    mirror_member: ""

  - name: jj
    version: 5.5.5
    digest: $JJ
    digest_of: the binary inside the release tarball
    primary: ""
    second_source: ghcr-mirror
    max_time: 60
    mirror_from: $JJ_URL
    mirror_member: ./jj

  - name: bun-linux-x64
    version: 9.9.9
    digest: $BUN
    digest_of: the installed bun binary
    primary: ""
    second_source: npm
    max_time: 60
    npm_package: "@oven/bun-linux-x64"
    npm_member: package/bin/bun
YAML
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: [push]
jobs:
  lint:
    runs-on: ubuntu-24.04
    steps:
      - uses: ./.github/actions/install-tool
YAML
  cat > "$root/.github/actions/install-tool/action.yml" <<'YAML'
name: Install tool
description: Installs the tool through the one helper, so it names no URL of its own.
runs:
  using: composite
  steps:
    - shell: bash
      run: |
        . "$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh"
        ci_tools_fetch tool "$RUNNER_TEMP/tool.tar.gz"
YAML
  echo "$root"
}

# run_gate <root>: the check under test against that tree. Sets `out` to the combined output and
# `status` to the exit code. Not a subshell, so both survive the call.
run_gate() {
  set +e
  out=$("$1/.github/scripts/check-ci-tools.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a tree every rule passes"
root=$(fixture green)
run_gate "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the rows and says each agrees with its pin" \
  "$(contains "$out" '^.github/ci-tools.yaml holds 4 row(s), each agreeing with the pin that installs it;')"
check "and sweeps .github for a short reference" \
  "$(contains "$out" 'none names a tool blob by a registry reference$')"

echo "case: rule 1 — a row that carries no digest_of"
root=$(fixture shape-missing-field)
cat >> "$root/.github/ci-tools.yaml" <<YAML

  - name: partial
    version: 1.0.0
    digest: $TOOL
    primary: ""
    second_source: npm
    max_time: 60
    npm_package: "@oven/bun-linux-x64"
    npm_member: package/bin/bun
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the row and the field it carries no value for" \
  "$(contains "$out" 'the partial row carries no digest_of, and a npm row carries name, version, digest, digest_of, primary, second_source, max_time, npm_package, npm_member')"
check "and counts the problem" "$(contains "$out" '^::error::1 problem(s) between .github/ci-tools.yaml and the tools CI installs$')"

echo "case: rule 1 — a digest that is not 64 lower-case hex"
root=$(fixture shape-digest)
cat >> "$root/.github/ci-tools.yaml" <<YAML

  - name: shouty
    version: 1.0.0
    digest: sha256:${TOOL^^}
    digest_of: the release tarball, whole
    primary: ""
    second_source: npm
    max_time: 60
    npm_package: "@oven/bun-linux-x64"
    npm_member: package/bin/bun
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the row and what a digest is" \
  "$(contains "$out" "the shouty row's digest is sha256:${TOOL^^}, which is not 64 lower-case hex characters")"
check "and says to write it bare" "$(contains "$out" 'Write it bare, with no sha256: prefix')"

echo "case: rule 1 — two rows with one name"
root=$(fixture shape-two-names)
cat >> "$root/.github/ci-tools.yaml" <<YAML

  - name: tool
    version: 2.0.0
    digest: $BUN
    digest_of: the release tarball, whole
    primary: ""
    second_source: npm
    max_time: 60
    npm_package: "@oven/bun-linux-x64"
    npm_member: package/bin/bun
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the row the earlier one is on" \
  "$(contains "$out" 'tool is also the name of the row on line 4, and a call site naming it would read one of the two')"

echo "case: rule 1 — a second_source neither source serves"
root=$(fixture shape-second-source)
cat >> "$root/.github/ci-tools.yaml" <<YAML

  - name: elsewhere
    version: 1.0.0
    digest: $BUN
    digest_of: the release tarball, whole
    primary: ""
    second_source: s3
    max_time: 60
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the row and the two sources the helper serves" \
  "$(contains "$out" "the elsewhere row's second_source is s3, and .github/scripts/ci-tools.sh serves ghcr-mirror and npm")"

echo "case: rule 1 — a primary that does not hold the row's version"
root=$(fixture shape-primary-version)
cat >> "$root/.github/ci-tools.yaml" <<YAML

  - name: adrift
    version: 4.4.4
    digest: $BUN
    digest_of: the release tarball, whole
    primary: https://github.com/example/tool/releases/download/v4.4.3/adrift-4.4.3.tar.gz
    second_source: ghcr-mirror
    max_time: 60
    mirror_from: https://github.com/example/tool/releases/download/v4.4.4/adrift-4.4.4.tar.gz
    mirror_member: ""
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the URL and the version it does not hold" \
  "$(contains "$out" "the adrift row's primary is https://github.com/example/tool/releases/download/v4.4.3/adrift-4.4.3.tar.gz and this row's version is 4.4.4, which that URL does not hold")"
check "and says what such a row does" \
  "$(contains "$out" 'installs one version and pins another')"

echo "case: rule 2 — a bun row at a version .bun-version does not pin"
root=$(fixture pin-bun)
cat >> "$root/.github/ci-tools.yaml" <<YAML

  - name: bun-linux-aarch64
    version: 9.9.8
    digest: $BUN
    digest_of: the installed bun binary
    primary: ""
    second_source: npm
    max_time: 60
    npm_package: "@oven/bun-linux-aarch64"
    npm_member: package/bin/bun
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names both values and both files" \
  "$(contains "$out" "^.github/ci-tools.yaml:[0-9]*: the bun-linux-aarch64 row's version is 9.9.8 and .bun-version pins 9.9.9")"
check "and says the bun rows move in the second commit of a Bun raise, with the pin, and that no call site moves" \
  "$(contains "$out" 'Raising Bun takes two commits, unlike raising any other tool here: the new oven/bun row of .github/ci-images.yaml first, then .bun-version, the ARG BUN_VERSION default of packages/daemon/docker/worker.Dockerfile and every FROM on the row it leaves, and the three bun-\* rows of this file move in that second commit, with .bun-version. No call site moves, because a call site names a row rather than a version; that is also why a second row of this tool would have no name one could use.')"

echo "case: rule 2 — the mise row and ARG MISE_VERSION disagree"
root=$(fixture pin-mise-version)
pins "$root" 9.9.9 v8.8.8
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names both values and both files" \
  "$(contains "$out" "the mise row's version is 7.7.7 and packages/daemon/docker/worker.Dockerfile pins ARG MISE_VERSION=v8.8.8, which is 8.8.8 without its leading v")"

echo "case: rule 2 — the mise row and ARG MISE_SHA256 disagree"
root=$(fixture pin-mise-digest)
pins "$root" 9.9.9 v7.7.7 "$BUN"
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names both digests and both files" \
  "$(contains "$out" "the mise row's digest is $MISE and packages/daemon/docker/worker.Dockerfile pins ARG MISE_SHA256=$BUN")"
check "and says one admits what the other refuses" \
  "$(contains "$out" 'one of the two admits bytes the other refuses')"

echo "case: rule 2 — the jj row and ARG JJ_TOOL disagree"
root=$(fixture pin-jj)
pins "$root" 9.9.9 v7.7.7 "$MISE" github:example/jj@6.6.6
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names both values and both files" \
  "$(contains "$out" "the jj row's version is 5.5.5 and packages/daemon/docker/worker.Dockerfile pins ARG JJ_TOOL=github:example/jj@6.6.6, whose version is 6.6.6")"

echo "case: rule 3 — an action that fetches a covered release asset no row names"
root=$(fixture reach-unnamed-asset)
cat > "$root/.github/actions/install-tool/action.yml" <<YAML
name: Install tool
description: Fetches the asset itself, at a version no row of the manifest holds.
runs:
  using: composite
  steps:
    - shell: bash
      run: |
        curl -fsSL -o "\$RUNNER_TEMP/tool.tar.gz" \\
          https://github.com/example/tool/releases/download/v2.0.0/tool_2.0.0_linux_amd64.tar.gz
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the file, the line and the URL" \
  "$(contains "$out" '^.github/actions/install-tool/action.yml:9: this file fetches a release asset of example/tool at https://github.com/example/tool/releases/download/v2.0.0/tool_2.0.0_linux_amd64.tar.gz, and no row of .github/ci-tools.yaml names that URL')"
check "and says a fetch no row names has no second source" \
  "$(contains "$out" 'a fetch no row names has none')"

echo "case: rule 3 — a workflow that builds a covered release URL"
root=$(fixture reach-built-url)
cat > "$root/.github/workflows/tools.yaml" <<'YAML'
name: Tools
on: [push]
jobs:
  tools:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: |
          curl -fsSL -O "https://github.com/example/mise/releases/download/${MISE_VERSION}/mise.tar.gz"
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the built URL" \
  "$(contains "$out" 'this file builds the address of a release asset of example/mise (https://github.com/example/mise/releases/download/${MISE_VERSION}/mise.tar.gz)')"
check "and says a built URL cannot be compared with a row" \
  "$(contains "$out" 'a built URL cannot be compared with a row')"

echo "case: rule 3 — a workflow that fetches Bun's own release asset"
# No bun row carries a primary, because oven-sh/setup-bun fetches that asset. The repository is
# covered all the same, so a workflow that reached for it names a Bun no row pins.
root=$(fixture reach-bun-asset)
cat > "$root/.github/workflows/bun.yaml" <<'YAML'
name: Bun
on: [push]
jobs:
  bun:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: curl -fsSL -O https://github.com/oven-sh/bun/releases/download/bun-v9.9.9/bun-linux-x64.zip
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the asset" \
  "$(contains "$out" 'this file fetches a release asset of oven-sh/bun at https://github.com/oven-sh/bun/releases/download/bun-v9.9.9/bun-linux-x64.zip')"

echo "case: rule 3 — a workflow that fetches an npm tarball no row derives"
root=$(fixture reach-npm-tarball)
cat > "$root/.github/workflows/bun.yaml" <<'YAML'
name: Bun
on: [push]
jobs:
  bun:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: curl -fsSL -O https://registry.npmjs.org/@oven/bun-linux-x64/-/bun-linux-x64-9.9.8.tgz
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the package and the URL" \
  "$(contains "$out" 'this file fetches an npm package under @oven at https://registry.npmjs.org/@oven/bun-linux-x64/-/bun-linux-x64-9.9.8.tgz')"

echo "case: rule 3 — the URLs the rows themselves name, and a repository out of scope"
# Each of these is a URL this check reads and admits: a row's primary, a row's mirror_from, the
# npm tarball a row derives, and a release asset of a repository no row covers.
root=$(fixture reach-named-urls)
cat > "$root/.github/workflows/tools.yaml" <<YAML
name: Tools
on: [push]
jobs:
  tools:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: |
          curl -fsSL -O $TOOL_URL
          curl -fsSL -O $JJ_URL
          curl -fsSL -O $BUN_URL
          curl -fsSL -O https://github.com/other/thing/releases/download/v3.0.0/thing-3.0.0.tar.gz
          curl -fsSL -O "https://github.com/other/built/releases/download/\${VERSION}/built.tar.gz"
YAML
run_gate "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the three URLs that name a row" \
  "$(contains "$out" '; 3 tool asset URL(s) in 3 file(s) under .github/actions and .github/workflows name one of those rows;')"

echo "case: rule 4 — a short reference to a tool blob"
root=$(fixture short-reference)
cat > "$root/.github/workflows/tools.yaml" <<YAML
name: Tools
on: [push]
jobs:
  tools:
    runs-on: ubuntu-24.04
    container:
      image: $REGISTRY@sha256:$TOOL
    steps:
      - run: true
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the file, the line and the row" \
  "$(contains "$out" "^.github/workflows/tools.yaml:7: this file names the tool row's blob as $REGISTRY@sha256:$TOOL, a registry reference")"
check "says which gate it would meet" \
  "$(contains "$out" 'Rule 6 of .github/scripts/check-ci-images.sh refuses a reference of this shape')"
check "and names the URL a reader of a tool blob uses" \
  "$(contains "$out" 'A tool blob is read at the full blobs/sha256: URL .github/scripts/ci-tools.sh builds from the row')"

echo "case: rule 4 — the same string in .github/ci-images.yaml"
# That file's own rows are images, and its digests are read by check-ci-images.sh. This sweep
# skips it, so a row of it that happened to carry a tool row's digest is no problem of this
# gate. It is the one case that needs the file at all: every other case's tree holds none, and
# the gate reads that path only as one to skip.
root=$(fixture short-reference-in-ci-images)
cat > "$root/.github/ci-images.yaml" <<YAML
registry: $REGISTRY

images:
  - upstream: docker.io/library/nats:2.10
    tag: nats-2.10-0a1b2c3d
    digest: sha256:4e5f60718293a4b54e5f60718293a4b54e5f60718293a4b54e5f60718293a4b5
  # $REGISTRY@sha256:$TOOL
YAML
run_gate "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: this repository's own tree"
run_gate "$repository"
check "exits 0" "$(is "$status" 0)"
check "names the manifest and its rows" "$(contains "$out" '^.github/ci-tools.yaml holds [0-9]* row(s)')"

summary "check-ci-tools.sh"
