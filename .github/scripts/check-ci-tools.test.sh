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
repo_paths_script="$script_dir/repo-paths.sh"
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
SOLO=5f60718293a4b5c65f60718293a4b5c65f60718293a4b5c65f60718293a4b5c6
REGISTRY=ghcr.io/example/fixture/ci
TOOL_URL=https://github.com/example/tool/releases/download/v1.0.0/tool_1.0.0_linux_amd64.tar.gz
MISE_URL=https://github.com/example/mise/releases/download/v7.7.7/mise-v7.7.7-linux-x64.tar.gz
JJ_URL=https://github.com/example/jj/releases/download/v5.5.5/jj-v5.5.5-x86_64.tar.gz
BUN_URL=https://registry.npmjs.org/@oven/bun-linux-x64/-/bun-linux-x64-9.9.9.tgz
SOLO_URL=https://github.com/example/solo/releases/download/v2.2.2/solo-2.2.2.tar.gz

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
  # The gate reads this repository's own name out of what repo-paths.sh answers, and stops with
  # no verdict at all when it cannot run it, so the fixture holds the real script beside the
  # gate. The name every fixture is judged against is the one that script defaults to,
  # cybermonklabs/legion, because the gate takes GITHUB_REPOSITORY out of its environment.
  ln -s "$repo_paths_script" "$root/.github/scripts/repo-paths.sh"
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
check "counts the rows, counts the entries, and says each row agrees with its pin" \
  "$(contains "$out" '^.github/ci-tools.yaml holds 4 row(s), each agreeing with the pin that installs it, and 0 no_second_source entries;')"
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
check "and says a fetch nothing names has no second source" \
  "$(contains "$out" 'a fetch nothing names has none')"

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

echo "case: rule 3 — a script that fetches a release asset no row names"
# The rule reads every file under .github/, and not the two directories its npm half reads: a
# script installs a tool as readily as a workflow step does.
root=$(fixture reach-script-asset)
cat > "$root/.github/scripts/install-thing.sh" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
curl -fsSL -O https://github.com/other/thing/releases/download/v3.0.0/thing-3.0.0.tar.gz
SCRIPT
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the script, the line and the URL" \
  "$(contains "$out" '^.github/scripts/install-thing.sh:3: this file fetches a release asset of other/thing at https://github.com/other/thing/releases/download/v3.0.0/thing-3.0.0.tar.gz, and no row of .github/ci-tools.yaml names that URL')"

echo "case: rule 3 — a URL built with a bare \$version"
# A built address is any URL whose text holds a dollar, so a bare \$version is refused like the
# \${MISE_VERSION} above. The rule read \${ alone until this gate was rewritten, and a job writing
# the shorter form went through.
root=$(fixture reach-bare-variable)
cat > "$root/.github/workflows/uv.yaml" <<'YAML'
name: uv
on: [push]
jobs:
  uv:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: curl -fsSL -O https://github.com/example/uv/releases/download/$version/uv.tar.gz
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the file, the line and the address the job builds" \
  "$(contains "$out" '^.github/workflows/uv.yaml:8: this file builds the address of a release asset of example/uv (https://github.com/example/uv/releases/download/$version/uv.tar.gz)')"

echo "case: rule 3 — release-asset addresses the gate's one spelling misses"
# GitHub serves one release asset at more than one address, and this rule compares text against
# one of them. All three below were measured against github.com on 2026-11-09:
# /releases/latest/download/<asset> answered 302 to the tagged asset, and the http:// and the
# www.github.com spellings each answered 301 to it. None of the three is an address a job
# assembles from pieces, and none is in a file whose name holds .test., so none is one of the two
# bounds AGENTS.md names: each is a release asset a new install fetches with no row, no entry and
# nothing saying so, which is the class R1 of dispatch://INFRA-1974 closes.
root=$(fixture reach-asset-spellings)
cat > "$root/.github/workflows/spellings.yaml" <<'YAML'
name: Spellings
on: [push]
jobs:
  spellings:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: |
          curl -fsSL -O https://github.com/other/thing/releases/latest/download/thing.tar.gz
          curl -fsSL -O http://github.com/other/thing/releases/download/v3.0.0/thing-3.0.0.tar.gz
          curl -fsSL -O https://www.github.com/other/thing/releases/download/v3.0.0/thing-3.0.0.tar.gz
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the latest-release permalink" \
  "$(contains "$out" '^.github/workflows/spellings.yaml:9: this file fetches a release asset of other/thing at https://github.com/other/thing/releases/latest/download/thing.tar.gz, and no row of .github/ci-tools.yaml names that URL')"
check "names the http:// spelling of a tagged asset" \
  "$(contains "$out" '^.github/workflows/spellings.yaml:10: this file fetches a release asset of other/thing at http://github.com/other/thing/releases/download/v3.0.0/thing-3.0.0.tar.gz, and no row of .github/ci-tools.yaml names that URL')"
check "names the www.github.com spelling of a tagged asset" \
  "$(contains "$out" '^.github/workflows/spellings.yaml:11: this file fetches a release asset of other/thing at https://www.github.com/other/thing/releases/download/v3.0.0/thing-3.0.0.tar.gz, and no row of .github/ci-tools.yaml names that URL')"

echo "case: rule 3 — the host is case-insensitive and the path is not"
# A URL's host is not case-sensitive and github.com's release path is, measured against
# github.com on 2026-11-09: https://GitHub.com/<o>/<r>/releases/download/<tag>/<asset> answered
# 302 with the asset, and so did https://GITHUB.COM/<o>/<r>/releases/latest/download/<asset>,
# while /RELEASES/download/ and /releases/DOWNLOAD/ each answered 404. So the two spellings that
# serve the asset are refused and the one that serves nothing is not: a rule that reads the whole
# address without regard to case would refuse an address github.com does not answer, which is a
# false refusal of its own. The count assertion is what holds those two apart.
root=$(fixture reach-host-case)
cat > "$root/.github/workflows/host-case.yaml" <<'YAML'
name: Host case
on: [push]
jobs:
  host-case:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: |
          curl -fsSL -O https://GitHub.com/other/thing/releases/download/v3.0.0/thing-3.0.0.tar.gz
          curl -fsSL -O https://GITHUB.COM/other/thing/releases/latest/download/thing.tar.gz
          curl -fsSL -O https://github.com/other/thing/RELEASES/download/v3.0.0/thing-3.0.0.tar.gz
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the mixed-case host" \
  "$(contains "$out" '^.github/workflows/host-case.yaml:9: this file fetches a release asset of other/thing at https://GitHub.com/other/thing/releases/download/v3.0.0/thing-3.0.0.tar.gz, and no row of .github/ci-tools.yaml names that URL')"
check "names the upper-case host on the latest permalink" \
  "$(contains "$out" '^.github/workflows/host-case.yaml:10: this file fetches a release asset of other/thing at https://GITHUB.COM/other/thing/releases/latest/download/thing.tar.gz, and no row of .github/ci-tools.yaml names that URL')"
check "and refuses those two alone, leaving the upper-case path github.com answers 404 for" \
  "$(contains "$out" '^::error::2 problem(s) between .github/ci-tools.yaml and the tools CI installs$')"

echo "case: rule 3 — a workflow that fetches Bun's own release asset"
# No bun row carries a primary, because oven-sh/setup-bun fetches that asset. So this URL is a
# release asset no row and no entry names, and the gate refuses it as it refuses any other,
# whatever repository it names.
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

echo "case: rule 3 — the URLs the rows name, beside two of a repository no row covers"
# The three a row names are read and admitted: a row's primary, a row's mirror_from, and the npm
# tarball a row derives. The other two are refused, because the rule now reads a release asset of
# every repository: one is an address nothing names, and one is an address the job assembles.
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
check "fails" "$(is "$status" 1)"
check "names the asset of the repository no row covers" \
  "$(contains "$out" '^.github/workflows/tools.yaml:12: this file fetches a release asset of other/thing at https://github.com/other/thing/releases/download/v3.0.0/thing-3.0.0.tar.gz, and no row of .github/ci-tools.yaml names that URL')"
check "names the address the job builds" \
  "$(contains "$out" '^.github/workflows/tools.yaml:13: this file builds the address of a release asset of other/built (https://github.com/other/built/releases/download/${VERSION}/built.tar.gz)')"
check "and refuses those two alone, so the three the rows name still pass" \
  "$(contains "$out" '^::error::2 problem(s) between .github/ci-tools.yaml and the tools CI installs$')"

echo "case: rule 3 — a release asset of this repository's own"
# These bytes this repository publishes, and no job of it installs them, so the address is read
# and passed over. The name it is compared against is what repo-paths.sh answers.
root=$(fixture reach-own-asset)
cat > "$root/.github/workflows/publish.yaml" <<'YAML'
name: Publish
on: [push]
jobs:
  publish:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: curl -fsSL -O https://github.com/cybermonklabs/legion/releases/download/envoy-infra-v1.2.3/envoy-infra-linux-amd64.tar.gz
YAML
run_gate "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: rule 3 — a URL in a file whose name holds .test."
# A test's URLs are fixtures it feeds to a gate, and the gate reading them is the code under
# test. The file you are reading is one of those, and every URL above is one of these.
root=$(fixture reach-test-file)
cat > "$root/.github/scripts/install-thing.test.sh" <<'SCRIPT'
#!/usr/bin/env bash
# A fixture address a test feeds to a gate. Nothing fetches it.
url=https://github.com/other/thing/releases/download/v3.0.0/thing-3.0.0.tar.gz
SCRIPT
run_gate "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: rule 3 — an npm tarball outside the two directories the npm half reads"
# The release-asset half widened to every file under .github/; the npm half still reads
# .github/actions and .github/workflows alone, because this repository writes the npm registry's
# address outside those two for reasons that fetch nothing.
root=$(fixture reach-npm-outside)
cat > "$root/.github/scripts/detect-bun.sh" <<'SCRIPT'
#!/usr/bin/env bash
# The address a lockfile line carries, written here so a check can find it. Nothing fetches it.
pattern=https://registry.npmjs.org/@oven/bun-linux-x64/-/bun-linux-x64-9.9.8.tgz
SCRIPT
run_gate "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a no_second_source entry allows the one URL it names"
# Both halves in one tree, so the entry is shown to be what allows the URL: the workflow alone is
# refused, and the same workflow passes once the entry names its address.
root=$(fixture decline-allows)
cat > "$root/.github/workflows/solo.yaml" <<YAML
name: Solo
on: [push]
jobs:
  solo:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: |
          curl -fsSL -O $SOLO_URL
          echo "$SOLO  solo-2.2.2.tar.gz" | sha256sum -c -
YAML
run_gate "$root"
check "fails while no entry names the URL" "$(is "$status" 1)"
check "and names it as a fetch nothing names" \
  "$(contains "$out" "this file fetches a release asset of example/solo at $SOLO_URL")"
cat >> "$root/.github/ci-tools.yaml" <<YAML

no_second_source:
  - url: $SOLO_URL
    digest: $SOLO
    pinned_in: .github/workflows/solo.yaml
    why: No registry outside this release serves these bytes today, and copying them into the ghcr package is the work dispatch://INFRA-1986 owns.
YAML
run_gate "$root"
check "exits 0 once the entry names it" "$(is "$status" 0)"
check "and counts the entry in the summary" \
  "$(contains "$out" ', and 1 no_second_source entry;')"

echo "case: a no_second_source entry that carries no why"
# The shape is read before the four rules, so an entry the gate cannot read is refused once
# rather than four times. Its URL is in no file here: an entry this short allows nothing.
root=$(fixture decline-no-why)
cat >> "$root/.github/ci-tools.yaml" <<YAML

no_second_source:
  - url: $SOLO_URL
    digest: $SOLO
    pinned_in: .github/workflows/ci.yaml
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the missing field and the four an entry carries" \
  "$(contains "$out" '^.github/ci-tools.yaml:45: this no_second_source entry carries no why, and an entry carries url, digest, pinned_in, why: the URL it declines, the digest its bytes are pinned at, the file that pins them, and why there is no second source.$')"

echo "case: a no_second_source entry whose why is a placeholder"
# Whether the reason is true is the reviewer's judgement. Whether anything was written at all is
# this gate's, and TODO is the word a reviewer would have had to catch.
root=$(fixture decline-placeholder-why)
cat > "$root/.github/workflows/solo.yaml" <<YAML
name: Solo
on: [push]
jobs:
  solo:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: |
          curl -fsSL -O $SOLO_URL
          echo "$SOLO  solo-2.2.2.tar.gz" | sha256sum -c -
YAML
cat >> "$root/.github/ci-tools.yaml" <<YAML

no_second_source:
  - url: $SOLO_URL
    digest: $SOLO
    pinned_in: .github/workflows/solo.yaml
    why: TODO
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "quotes the placeholder and says how much the gate wants" \
  "$(contains "$out" "^.github/ci-tools.yaml:48: this entry's why is TODO, which says nothing a reader could act on. Write at least 40 characters naming which second source was looked for, why there is none today, and what building one would take.")"

echo "case: a no_second_source entry whose why is a placeholder padded past the length bound"
# Every placeholder the gate knows is shorter than the 40 characters it wants, so the case above
# would be refused on length alone and the list of placeholders would never be read. This one is
# 41 characters of `unknown` and padding: only the list refuses it.
root=$(fixture decline-padded-placeholder-why)
cat > "$root/.github/workflows/solo.yaml" <<YAML
name: Solo
on: [push]
jobs:
  solo:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: |
          curl -fsSL -O $SOLO_URL
          echo "$SOLO  solo-2.2.2.tar.gz" | sha256sum -c -
YAML
cat >> "$root/.github/ci-tools.yaml" <<YAML

no_second_source:
  - url: $SOLO_URL
    digest: $SOLO
    pinned_in: .github/workflows/solo.yaml
    why: unknown . . . . . . . . . . . . . . . . .
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "refuses the padded placeholder, which is long enough for the length bound" \
  "$(contains "$out" "^.github/ci-tools.yaml:48: this entry's why is unknown . . . . . . . . . . . . . . . . ., which says nothing a reader could act on. Write at least 40 characters naming which second source was looked for, why there is none today, and what building one would take.")"

echo "case: a no_second_source entry whose URL no file under .github writes"
# The install this entry declined a second source for was deleted and the entry stayed. Refusing
# it is how the list prunes itself.
root=$(fixture decline-no-writer)
cat > "$root/.github/workflows/solo.yaml" <<YAML
name: Solo
on: [push]
jobs:
  solo:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: echo "$SOLO  solo-2.2.2.tar.gz" | sha256sum -c -
YAML
cat >> "$root/.github/ci-tools.yaml" <<YAML

no_second_source:
  - url: $SOLO_URL
    digest: $SOLO
    pinned_in: .github/workflows/solo.yaml
    why: No registry outside this release serves these bytes today, and copying them into the ghcr package is the work dispatch://INFRA-1986 owns.
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the URL nothing writes and what to do with the entry" \
  "$(contains "$out" "^.github/ci-tools.yaml:45: no file under .github writes $SOLO_URL, so this entry declines a second source for an install that is no longer here. Delete the entry, or correct its url to the text the file that fetches those bytes writes.$")"

echo "case: a no_second_source entry whose digest the file it names does not hold"
# One source never means no checksum: the file that fetches these bytes checks nothing, so the
# digest the entry carries holds the install to nothing.
root=$(fixture decline-digest-unpinned)
cat > "$root/.github/workflows/solo.yaml" <<YAML
name: Solo
on: [push]
jobs:
  solo:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: curl -fsSL -O $SOLO_URL
YAML
cat >> "$root/.github/ci-tools.yaml" <<YAML

no_second_source:
  - url: $SOLO_URL
    digest: $SOLO
    pinned_in: .github/workflows/solo.yaml
    why: No registry outside this release serves these bytes today, and copying them into the ghcr package is the work dispatch://INFRA-1986 owns.
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the digest and the file it does not occur in" \
  "$(contains "$out" "^.github/ci-tools.yaml:46: this entry's digest $SOLO does not occur in .github/workflows/solo.yaml, the file it names as the pin, so nothing checks the bytes this entry declines a second source for. One source never means no checksum.$")"

echo "case: a no_second_source entry whose repository a row already covers"
# A second version of the tool row's asset. Those bytes have a second source already, and the
# call site reads it through the helper rather than declining one.
root=$(fixture decline-covered-repository)
cat > "$root/.github/workflows/tool.yaml" <<YAML
name: Tool
on: [push]
jobs:
  tool:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: |
          curl -fsSL -O https://github.com/example/tool/releases/download/v2.0.0/tool_2.0.0_linux_amd64.tar.gz
          echo "$SOLO  tool_2.0.0_linux_amd64.tar.gz" | sha256sum -c -
YAML
cat >> "$root/.github/ci-tools.yaml" <<YAML

no_second_source:
  - url: https://github.com/example/tool/releases/download/v2.0.0/tool_2.0.0_linux_amd64.tar.gz
    digest: $SOLO
    pinned_in: .github/workflows/tool.yaml
    why: No registry outside this release serves these bytes today, and copying them into the ghcr package is the work dispatch://INFRA-1986 owns.
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the row that covers the repository and where to fetch the bytes" \
  "$(contains "$out" "^.github/ci-tools.yaml:45: the tool row's primary already covers example/tool, so this tool has a second source and its call site reads it with .github/scripts/ci-tools.sh. An entry declines a URL nothing else serves; delete this one and fetch the bytes from the row.$")"

echo "case: a no_second_source entry whose url is not a release asset"
# An entry declines one release-asset address. Anything else is outside the class this rule
# reads, and an entry for it would allow nothing.
root=$(fixture decline-url-not-an-asset)
cat >> "$root/.github/ci-tools.yaml" <<YAML

no_second_source:
  - url: https://registry.npmjs.org/solo/-/solo-2.2.2.tgz
    digest: $SOLO
    pinned_in: .github/workflows/ci.yaml
    why: No registry outside this release serves these bytes today, and copying them into the ghcr package is the work dispatch://INFRA-1986 owns.
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "quotes the url and says what an entry declines" \
  "$(contains "$out" "^.github/ci-tools.yaml:45: this no_second_source entry's url is https://registry.npmjs.org/solo/-/solo-2.2.2.tgz, which is not a GitHub release-asset address. An entry declines one such URL, written exactly as the file that fetches it writes it.$")"

echo "case: two no_second_source entries for one URL"
# Two entries for one URL carry two reasons, and a reader has no way to tell which one holds.
root=$(fixture decline-two-entries)
cat > "$root/.github/workflows/solo.yaml" <<YAML
name: Solo
on: [push]
jobs:
  solo:
    runs-on: ubuntu-24.04
    steps:
      - shell: bash
        run: |
          curl -fsSL -O $SOLO_URL
          echo "$SOLO  solo-2.2.2.tar.gz" | sha256sum -c -
YAML
cat >> "$root/.github/ci-tools.yaml" <<YAML

no_second_source:
  - url: $SOLO_URL
    digest: $SOLO
    pinned_in: .github/workflows/solo.yaml
    why: No registry outside this release serves these bytes today, and copying them into the ghcr package is the work dispatch://INFRA-1986 owns.

  - url: $SOLO_URL
    digest: $SOLO
    pinned_in: .github/workflows/solo.yaml
    why: The same bytes again, with another reason a reader would have to choose between.
YAML
run_gate "$root"
check "fails" "$(is "$status" 1)"
check "names the line the earlier entry is on" \
  "$(contains "$out" '^.github/ci-tools.yaml:50: this URL is also declined by the no_second_source entry on line 45, and two entries for one URL carry two reasons a reader must choose between. Keep one.$')"

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
