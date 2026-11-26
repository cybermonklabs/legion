#!/usr/bin/env bash
# Tests for `check-release-credentials.sh`: a job of the four workflows it reads (the three release
# workflows and the reusable notes names workflow) that holds a credential runs no dependency
# code, uses only allow-listed actions, restores no cache and holds the deploy key only in the env
# of a step that runs the one push command (a release's push-tag);
# a job that runs dependency code holds no credential and ends in the no-credential
# tripwire; every checkout persists no credential, and a checkout comes before the step that runs
# release-push.sh;
# release_publish stays in its concurrency group; and a job condition downstream of a skippable job
# calls `always()` and tests the result of every job whose outputs it reads. The check is a tripwire
# over named patterns (the commands, actions and paths its lists hold), not a proof that no other
# code runs, and these cases exercise those patterns, on each side of each line.
#
# Each case builds a miniature repository under a temporary directory, with the real script and
# the shell_commands.py module it imports symlinked into it and the four workflows written green,
# and mutates one file.
#
# Run from anywhere: .github/scripts/check-release-credentials.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-release-credentials.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree with the script and its module linked in and the four green workflows
# written. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows"
  ln -s "$check_script" "$root/.github/scripts/check-release-credentials.sh"
  ln -s "$script_dir/shell_commands.py" "$root/.github/scripts/shell_commands.py"
  write_green "$root/.github/workflows"
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$(cd "$root" && "./.github/scripts/check-release-credentials.sh" 2>&1)
  status=$?
  set -e
}

# mutate <file> <old> <new> [nth]: replaces the nth (default: the only) literal occurrence of <old>
# with <new>. A fixture without that occurrence fails the run, so a case cannot pass by mutating
# nothing.
mutate() {
  python3 - "$1" "$2" "$3" "${4:-0}" <<'PY'
import sys

path, old, new, nth = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
text = open(path).read()
count = text.count(old)
if nth == 0 and count != 1:
    sys.exit(f"{path}: expected exactly one occurrence of {old!r}, found {count}")
if nth > count:
    sys.exit(f"{path}: expected at least {nth} occurrence(s) of {old!r}, found {count}")
start = -1
for _ in range(max(nth, 1)):
    start = text.index(old, start + 1)
open(path, "w").write(text[:start] + new + text[start + len(old):])
PY
}

# line_of <file> <regex>: the 1-based line of the first match, for the `<file>:<line>:` checks.
line_of() {
  grep -n -m1 -- "$2" "$1" | cut -d: -f1
}

SHA=3d3c42e5aac5ba805825da76410c181273ba90b1
RELEASE=.github/workflows/release.yaml
LISTENER=.github/workflows/release-envoy-listener.yaml
INFRA=.github/workflows/release-envoy-infra.yaml
NOTES_NAMES=.github/workflows/release-notes-names.yaml

# write_green <workflows dir>: the four workflows in a shape every rule passes, not a copy of the
# real files. release.yaml: a build job (install, setup-bun, a `go build` behind `&&`,
# install-changie, `release-push.sh fold-notes`, upload, tripwire) with an `if:` of its own, two
# credentialed publish jobs (setup-node, `npm publish` of the handed tarball, the tag push and a
# GitHub release) whose `if:` calls `always()` and tests the result of the build whose outputs it
# reads, a plain job that checks out and runs no dependency code, and a reusable-workflow call
# with no steps and no `if:`. pi_legion_publish needing pi_envoy_publish and testing its result is a generic rule-5
# shape, a chain of skippable jobs, which the if-two-needs-one-result and if-steps-less-job cases
# below mutate; the real release.yaml publishes every package from one job. The listener: a docker
# build job (`packages: write`, a build-push) and release_publish in its concurrency group, with
# `docker buildx imagetools create`. Infra: a build job (install-pulumi, `pulumi package
# get-schema`, tripwire) and a publish job. The notes names workflow: one job that checks out and
# runs check-private-names.sh from the checkout with the term list, and no dependency code.
write_green() {
  local dir=$1
  cat > "$dir/release.yaml" <<'YAML'
name: Release Packages
on:
  push:
    branches: [main]
permissions: {}
jobs:
  pi_envoy_build:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    if: github.ref == 'refs/heads/main'
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: ./.github/actions/setup-bun
      - name: Install dependencies
        run: bun install --frozen-lockfile
      - name: Build the daemon
        run: cd packages/daemon && go build ./cmd/legion
      - uses: ./.github/actions/install-changie
      - name: Render the changelog entries
        run: .github/scripts/release-push.sh fold-notes 1.2.3 "$RUNNER_TEMP/notes" packages/pi-envoy pi-envoy-v
      - uses: actions/upload-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          name: pi-envoy-tarball
          path: packages/pi-envoy/*.tgz
      - name: Check this job holds no credential (tripwire)
        run: .github/scripts/release-push.sh no-credential
  pi_envoy_publish:
    needs: pi_envoy_build
    if: always() && needs.pi_envoy_build.result == 'success' && needs.pi_envoy_build.outputs.skip != 'true'
    runs-on: ubuntu-24.04
    permissions:
      contents: write
      id-token: write
    cache-mode: none
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/setup-node@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          node-version: "24"
          registry-url: "https://registry.npmjs.org"
      - uses: actions/download-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          name: pi-envoy-tarball
          path: tarball
      - name: Check the hand-over
        id: handed
        env:
          VERSION: ${{ needs.pi_envoy_build.outputs.version }}
        run: |
          stem="sjawhar-pi-envoy-$VERSION"
          tag=$("$GITHUB_WORKSPACE/.github/scripts/release-push.sh" handed pi-envoy-v "$VERSION" tarball "$stem.tgz")
          { echo "tag=$tag"; echo "stem=$stem"; } >> "$GITHUB_OUTPUT"
      - name: Publish to npm
        env:
          STEM: ${{ steps.handed.outputs.stem }}
        run: npm publish "tarball/$STEM.tgz" --access public --provenance
      - name: Push tag
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          TAG: ${{ steps.handed.outputs.tag }}
        run: |
          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" push-tag "$TAG"
      - name: Create GitHub release
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          TAG: ${{ steps.handed.outputs.tag }}
          STEM: ${{ steps.handed.outputs.stem }}
        run: |
          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" release "$TAG" "Pi Envoy v1" "tarball/$STEM.tgz"
  pi_legion_publish:
    needs: [pi_legion_build, pi_envoy_build, pi_envoy_publish]
    # pi_legion_build names no job of this fixture: the checker ignores a dangling needs name on
    # purpose (GitHub refuses such a workflow at parse).
    if: always() && needs.pi_legion_build.result == 'success' && needs.pi_legion_build.outputs.skip != 'true' && (needs.pi_envoy_publish.result == 'success' || needs.pi_envoy_publish.result == 'skipped')
    runs-on: ubuntu-24.04
    permissions:
      contents: write
      id-token: write
    cache-mode: none
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/setup-node@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          node-version: "24"
          registry-url: "https://registry.npmjs.org"
      - uses: actions/download-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          name: pi-legion-tarball
          path: tarball
      - name: Check the hand-over
        id: handed
        env:
          VERSION: ${{ needs.pi_legion_build.outputs.version }}
        run: |
          stem="sjawhar-pi-legion-$VERSION"
          tag=$("$GITHUB_WORKSPACE/.github/scripts/release-push.sh" handed pi-legion-v "$VERSION" tarball "$stem.tgz")
          { echo "tag=$tag"; echo "stem=$stem"; } >> "$GITHUB_OUTPUT"
      - name: Publish to npm
        env:
          STEM: ${{ steps.handed.outputs.stem }}
        run: npm publish "tarball/$STEM.tgz" --access public --provenance
      - name: Push tag
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          TAG: ${{ steps.handed.outputs.tag }}
        run: |
          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" push-tag "$TAG"
      - name: Create GitHub release
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          TAG: ${{ steps.handed.outputs.tag }}
          STEM: ${{ steps.handed.outputs.stem }}
        run: |
          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" release "$TAG" "Pi Legion v1" "tarball/$STEM.tgz"
  changes:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          persist-credentials: false
      - run: echo hello
  worker_image:
    needs: [pi_legion_publish]
    uses: ./.github/workflows/worker-image.yaml
    permissions:
      contents: read
      packages: write
      id-token: write
      attestations: write
  notes_names:
    permissions:
      contents: read
    uses: ./.github/workflows/release-notes-names.yaml
    with:
      artifact: pi-envoy-notes
    secrets:
      PRIVATE_NAME_TERMS: ${{ secrets.PRIVATE_NAME_TERMS }}
YAML
  cat > "$dir/release-envoy-listener.yaml" <<'YAML'
name: Release Envoy Listener
on:
  push:
    branches: [main]
permissions: {}
jobs:
  docker:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          persist-credentials: false
      - uses: docker/setup-qemu-action@3d3c42e5aac5ba805825da76410c181273ba90b1
      - uses: docker/login-action@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/build-push-action@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          context: .
          push: true
          tags: ghcr.io/cybermonklabs/legion/envoy:${{ github.sha }}
      - name: Check this job holds no credential (tripwire)
        run: .github/scripts/release-push.sh no-credential
  release_publish:
    needs: docker
    concurrency:
      group: release-envoy-listener
      cancel-in-progress: false
      queue: max
    runs-on: ubuntu-24.04
    permissions:
      contents: write
      packages: write
      id-token: write
      attestations: write
      actions: write
    cache-mode: none
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: docker/login-action@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Advance latest image
        run: docker buildx imagetools create --tag x y
      - name: Tag release
        env:
          TAG: legion-envoy-v1.0.0
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
        run: |
          git tag "$TAG"
          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" push-tag "$TAG"
YAML
  cat > "$dir/release-envoy-infra.yaml" <<'YAML'
name: Release envoy-infra
on:
  push:
    branches: [main]
permissions: {}
jobs:
  build:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: ./.github/actions/install-pulumi
      - name: Smoke test
        run: pulumi package get-schema "$GITHUB_WORKSPACE/out/linux-amd64/pulumi-resource-envoy-infra"
      - uses: actions/upload-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          name: envoy-infra-assets
          path: assets/
      - name: Check this job holds no credential (tripwire)
        run: .github/scripts/release-push.sh no-credential
  publish:
    needs: build
    runs-on: ubuntu-24.04
    permissions:
      contents: write
      id-token: write
      attestations: write
    cache-mode: none
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/download-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          name: envoy-infra-assets
          path: assets
      - name: Tag release
        env:
          TAG: envoy-infra-v1.0.0
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
        run: |
          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" push-tag "$TAG"
      - uses: actions/attest-build-provenance@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          subject-path: assets/*.tar.gz
      - name: Create GitHub release
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" release "$TAG" "envoy-infra v1" assets/*.tar.gz
YAML
  cat > "$dir/release-notes-names.yaml" <<'YAML'
name: Release Notes Names
on:
  workflow_call:
    inputs:
      artifact:
        type: string
        required: true
    secrets:
      PRIVATE_NAME_TERMS:
        required: false
permissions:
  contents: read
jobs:
  names:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          sparse-checkout: .github
          persist-credentials: false
      - uses: actions/download-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          name: ${{ inputs.artifact }}
          path: ${{ runner.temp }}/notes
      - name: Check the entries name nothing private
        env:
          PRIVATE_NAME_TERMS: ${{ secrets.PRIVATE_NAME_TERMS }}
        run: |
          "$GITHUB_WORKSPACE/.github/scripts/check-private-names.sh" "$RUNNER_TEMP/notes/release-notes.md"
YAML
}

# add_publish_step <root> <step yaml lines>: a step before pi_envoy_publish's "Publish to npm".
add_publish_step() {
  mutate "$1/$RELEASE" $'      - name: Publish to npm\n' "$2"$'      - name: Publish to npm\n' 1
}

# add_push_tag_step <root> <name> <run lines>: add_publish_step of a step that holds the deploy key,
# whose `run:` is the given lines (each indented ten spaces, newline-ended) and then the push-tag
# call that allows the key there, so the check's verdict rests on the lines alone.
add_push_tag_step() {
  add_publish_step "$1" "      - name: $2"$'\n        env:\n          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG: ${{ steps.handed.outputs.tag }}\n        run: |\n'"$3"$'          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" push-tag "$TAG"\n'
}

echo "case: the four workflows in a shape every rule passes"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it covered" \
  "$(contains "$out" 'check-release-credentials: 4 credentialed job(s) and 3 build job(s) across 4 workflows keep credentials out of jobs that run dependency code')"

echo "case: the notes names workflow's job, which holds the term list, runs bun"
# The control that this check reads the reusable workflow at all: the release workflows' names
# jobs only call it, so a dependency command in its job is caught here or nowhere.
root=$(fixture names-workflow-bun)
mutate "$root/$NOTES_NAMES" $'      - name: Check the entries name nothing private\n' $'      - run: bun install --frozen-lockfile\n      - name: Check the entries name nothing private\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job's secret, beside the dependency code" \
  "$(contains "$out" "^::error::$NOTES_NAMES:[0-9]*: job 'names' runs dependency code (step 3 runs 'bun'), but its text names secrets.PRIVATE_NAME_TERMS")"
check "and the missing tripwire" \
  "$(contains "$out" "^::error::$NOTES_NAMES:[0-9]*: job 'names' runs dependency code (step 3 runs 'bun'), so its last step must be the tripwire")"

echo "case: the notes names workflow's checkout keeps its credential"
root=$(fixture names-workflow-persist)
mutate "$root/$NOTES_NAMES" $'          persist-credentials: false\n' $'          persist-credentials: true\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the reusable workflow's checkout" "$(contains "$out" "^::error::$NOTES_NAMES:[0-9]*: .*persist-credentials")"

echo "case: the notes names workflow is missing"
root=$(fixture names-workflow-missing)
rm "$root/$NOTES_NAMES"
run_check "$root"
check "exits 1, so the release workflows' calls cannot reach a workflow this check never read" \
  "$(is "$status:$(contains "$out" "^::error::$NOTES_NAMES is missing\$")" 1:true)"

echo "case: a names job pointed at a workflow this check does not read, still handing it the term list"
# The job has no steps, so rules 1-3 and 6 never read it; rule 7 is what sees where the list goes.
root=$(fixture names-call-elsewhere)
mutate "$root/$RELEASE" '    uses: ./.github/workflows/release-notes-names.yaml' '    uses: ./.github/workflows/worker-image.yaml'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the call and the secret at the secret's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      PRIVATE_NAME_TERMS:'): job 'notes_names' calls ./.github/workflows/worker-image.yaml, a workflow this check does not read, but its text names secrets.PRIVATE_NAME_TERMS")"

echo "case: a call to the names workflow with secrets: inherit, which hides from rule 8 which secret it passes"
root=$(fixture names-call-inherit)
mutate "$root/$RELEASE" $'    secrets:\n      PRIVATE_NAME_TERMS: ${{ secrets.PRIVATE_NAME_TERMS }}\n' $'    secrets: inherit\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the call at its secrets line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    secrets: inherit'): job 'notes_names' calls ./.github/workflows/release-notes-names.yaml with 'secrets: inherit'")"

echo "case: a names job uses only the allow-listed actions, through its local composite actions too"
third_party=$'      - uses: some-org/any-js-action@3d3c42e5aac5ba805825da76410c181273ba90b1\n'
root=$(fixture names-third-party)
mutate "$root/$NOTES_NAMES" $'      - name: Check the entries name nothing private\n' "$third_party"$'      - name: Check the entries name nothing private\n'
run_check "$root"
check "a third-party action beside the term list: exits 1" "$(is "$status" 1)"
check "names the job and the action" \
  "$(contains "$out" "^::error::$NOTES_NAMES:[0-9]*: job 'names' holds the deployment's term list (secrets.PRIVATE_NAME_TERMS), but step 'some-org/any-js-action@[0-9a-f]*' uses 'some-org/any-js-action@[0-9a-f]*' which is not on the checker's allow-list")"
root=$(fixture names-composite-third-party)
mkdir -p "$root/.github/actions/wrap"
printf 'name: wrap\nruns:\n  using: composite\n  steps:\n    - uses: some-org/any-js-action@3d3c42e5aac5ba805825da76410c181273ba90b1\n' \
  > "$root/.github/actions/wrap/action.yml"
mutate "$root/$NOTES_NAMES" $'      - name: Check the entries name nothing private\n' $'      - uses: ./.github/actions/wrap\n      - name: Check the entries name nothing private\n'
run_check "$root"
check "a local composite action that brings one in: exits 1" "$(is "$status" 1)"
check "names the action it brings in" \
  "$(contains "$out" "uses './.github/actions/wrap', which brings in 'some-org/any-js-action@[0-9a-f]*', which is not on the checker's allow-list")"
root=$(fixture names-composite-allowed)
mkdir -p "$root/.github/actions/wrap"
printf 'name: wrap\nruns:\n  using: composite\n  steps:\n    - uses: actions/download-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1\n' \
  > "$root/.github/actions/wrap/action.yml"
mutate "$root/$NOTES_NAMES" $'      - name: Check the entries name nothing private\n' $'      - uses: ./.github/actions/wrap\n      - name: Check the entries name nothing private\n'
run_check "$root"
check "a local composite action that uses only allow-listed actions passes" "$(is "$status" 0)"

echo "case: worker_image, which calls a workflow this check does not read, is handed a secret"
root=$(fixture worker-image-secret)
mutate "$root/$RELEASE" $'    uses: ./.github/workflows/worker-image.yaml\n' $'    uses: ./.github/workflows/worker-image.yaml\n    secrets:\n      DEPLOY_TOKEN: ${{ secrets.PRIVATE_NAME_TERMS }}\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names worker_image and the secret" \
  "$(contains "$out" "job 'worker_image' calls ./.github/workflows/worker-image.yaml, a workflow this check does not read, but its text names secrets.PRIVATE_NAME_TERMS")"
root=$(fixture worker-image-inherit)
mutate "$root/$RELEASE" $'    uses: ./.github/workflows/worker-image.yaml\n' $'    uses: ./.github/workflows/worker-image.yaml\n    secrets: inherit\n'
run_check "$root"
check "secrets: inherit, which hands it every secret, is refused too" \
  "$(is "$status:$(contains "$out" "job 'worker_image' calls .*, but its text names every secret ('secrets: inherit')")" 1:true)"
root=$(fixture worker-image-github-token)
mutate "$root/$RELEASE" $'    uses: ./.github/workflows/worker-image.yaml\n' $'    uses: ./.github/workflows/worker-image.yaml\n    secrets:\n      TOKEN: ${{ secrets.GITHUB_TOKEN }}\n'
run_check "$root"
check "GITHUB_TOKEN alone, which every job holds, passes" "$(is "$status" 0)"

echo "case: a step that starts publishing a build's output waits for the names job that checks it"
# gated_fixture <name>: the green fixture with notes_names checking pi_envoy_build's output (it
# needs that build and holds the term list) and pi_envoy_publish needing both, so rule 8 reads
# pi_envoy_publish's steps.
gated_fixture() {
  local dir
  dir=$(fixture "$1")
  mutate "$dir/$RELEASE" $'  notes_names:\n    permissions:\n' $'  notes_names:\n    needs: pi_envoy_build\n    permissions:\n'
  mutate "$dir/$RELEASE" $'  pi_envoy_publish:\n    needs: pi_envoy_build\n' $'  pi_envoy_publish:\n    needs: [pi_envoy_build, notes_names]\n'
  echo "$dir"
}
root=$(gated_fixture names-gate-missing)
add_publish_step "$root" $'      - name: Download the tarball\n        if: needs.pi_envoy_build.result == \'success\' && needs.pi_envoy_build.outputs.skip != \'true\'\n        run: echo download\n'
run_check "$root"
check "a step that tests the build's result and not the names job's: exits 1" "$(is "$status" 1)"
check "names the job, the step and the names job at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" "^        if: needs.pi_envoy_build.result == 'success' && needs.pi_envoy_build.outputs"): job 'pi_envoy_publish' step 'Download the tarball' tests needs.pi_envoy_build.result == 'success' but not needs.notes_names.result == 'success'")"
root=$(gated_fixture names-gate-present)
add_publish_step "$root" $'      - name: Download the tarball\n        if: needs.pi_envoy_build.result == \'success\' && needs.notes_names.result == \'success\'\n        run: echo download\n'
run_check "$root"
check "the same step testing the names job's result too: exits 0" "$(is "$status" 0)"
root=$(gated_fixture names-gate-index)
add_publish_step "$root" $'      - name: Download the tarball\n        if: needs.pi_envoy_build.result == \'success\' && needs[\'notes_names\'].result == \'success\'\n        run: echo download\n'
run_check "$root"
check "the names job's result tested by index: exits 0" "$(is "$status" 0)"
root=$(gated_fixture names-gate-disjunction)
add_publish_step "$root" $'      - name: Set up node for any package\n        if: (needs.pi_envoy_build.result == \'success\') || (needs.pi_legion_build.result == \'success\')\n        run: echo node\n'
run_check "$root"
check "an if: that joins packages with ||, no step of one package: exits 0" "$(is "$status" 0)"

echo "case: a credentialed job runs bun install"
root=$(fixture bun-install)
add_publish_step "$root" $'      - name: Install the publish dependencies\n        run: bun install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job, the step and the command at the step's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - name: Install the publish dependencies$'): job 'pi_envoy_publish' holds a credential, but step 'Install the publish dependencies' runs 'bun'")"

echo "case: go behind && in a credentialed job"
root=$(fixture go-after-and)
add_publish_step "$root" $'      - name: Build the daemon\n        run: cd packages/daemon && go build ./cmd/legion\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names go" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Build the daemon' runs 'go'")"

# A here-document a shell reads as its script runs its lines: `bash <<'EOT'` holding `bunx …` runs
# bunx, whatever the delimiter's quoting. An unquoted body's `$(…)` runs, and so does a
# substitution inside arithmetic. This check reads every body as commands, as main's reader did.
echo "case: a dependency command in a here-document a shell reads, in a credentialed job"
root=$(fixture heredoc-shell)
add_publish_step "$root" $'      - name: Run a script from a here-document\n        run: |\n          bash <<\'EOT\'\n          bunx evil-package\n          EOT\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names bunx" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Run a script from a here-document' runs 'bunx'")"
root=$(fixture heredoc-substitution)
add_publish_step "$root" $'      - name: Expand a here-document\n        run: |\n          cat <<EOF\n          $(bun install)\n          EOF\n          n=$(( $(bunx count) + 1 ))\n'
run_check "$root"
check "an unquoted body's substitution exits 1" "$(is "$status" 1)"
check "names bun, and the substitution in arithmetic, bunx" \
  "$(contains "$out" "step 'Expand a here-document' runs 'bun', 'bunx'")"
root=$(fixture heredoc-text)
add_publish_step "$root" $'      - name: Print a here-document\n        run: |\n          cat <<\'EOF\'\n          bunx evil-package\n          $(bun install)\n          EOF\n'
run_check "$root"
# Main's rule, which this check keeps (every_body): a body is read as commands even where no shell
# of its line reads it, so a quoted `cat` body naming bunx is refused too, reading more than bash
# runs rather than less.
check "a quoted body no shell of its line reads is read too: exits 1" "$(is "$status" 1)"

# The forms main's reader read as commands and the here-document reader first missed (review
# 6052147931): a process substitution among an array's values, an array whose quoted `(` hides
# the lines up to a later `)`, a shell after the pipe a here-document feeds, a shell given
# /dev/stdin, and a shell behind setsid or timeout's long option. Each runs bunx in a credentialed
# job, and each is refused by name.
echo "case: dependency code in the forms a shell runs from an array or a here-document, in a credentialed job"
root=$(fixture heredoc-and-array-forms)
add_publish_step "$root" $'      - name: Array process substitution\n        run: files=(<(bunx evil-package))\n      - name: Array quoted paren\n        run: |\n          x=("(")\n          bunx evil-package\n          # )\n      - name: Pipe to a shell\n        run: |\n          cat <<\'EOT\' | bash\n          bunx evil-package\n          EOT\n      - name: Shell given stdin\n        run: |\n          bash /dev/stdin <<\'EOT\'\n          bunx evil-package\n          EOT\n      - name: Shell behind setsid\n        run: |\n          setsid bash <<\'EOT\'\n          bunx evil-package\n          EOT\n      - name: Shell behind timeout\n        run: |\n          timeout --signal KILL 5m bash <<\'EOT\'\n          bunx evil-package\n          EOT\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for step in 'Array process substitution' 'Array quoted paren' 'Pipe to a shell' 'Shell given stdin' \
  'Shell behind setsid' 'Shell behind timeout'; do
  check "names bunx in '$step'" "$(contains "$out" "holds a credential, but step '$step' runs [^;]*'bunx'")"
done

# The forms of review 6054247075: a here-document body that reaches a shell by a route the line
# rule does not follow, which the release check reads anyway because it reads every body as
# commands; a `((` that is two subshells to bash, not arithmetic; a substitution in a case
# pattern, which bash expands; and env with an expansion of many words before the shell. Each runs
# bunx in a credentialed job under bash, and each is refused by name.
echo "case: the routes into a shell the line rule misses, in a credentialed job"
root=$(fixture every-body-forms)
add_publish_step "$root" $'      - name: Eval of a substitution\n        run: |\n          eval "$(cat <<\'EOT\'\n          bunx evil-package\n          EOT\n          )"\n      - name: Bash -c of a substitution\n        run: |\n          bash -c "$(cat <<\'EOT\'\n          bunx evil-package\n          EOT\n          )"\n      - name: Source of a process substitution\n        run: |\n          . <(cat <<\'EOT\'\n          bunx evil-package\n          EOT\n          )\n      - name: Bash of a process substitution\n        run: |\n          bash <(cat <<\'EOT\'\n          bunx evil-package\n          EOT\n          )\n      - name: Pipe after the body\n        run: |\n          cat <<\'EOT\' |\n          bunx evil-package\n          EOT\n          bash\n      - name: Shell behind taskset\n        run: |\n          taskset -c 0 bash <<\'EOT\'\n          bunx evil-package\n          EOT\n      - name: Body ended by EOF paren\n        run: |\n          x=$(cat <<\'EOF\'\n          bunx evil-package\n          EOF)\n          eval "$x"\n      - name: Two subshells\n        run: |\n          ((bunx evil-package) || true)\n      - name: Case pattern\n        run: |\n          case x in "$(bunx evil-package)") : ;; esac\n      - name: Env with an expansion\n        run: |\n          env "${args[@]}" bash <<\'EOT\'\n          bunx evil-package\n          EOT\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for step in 'Eval of a substitution' 'Bash -c of a substitution' 'Source of a process substitution' \
  'Bash of a process substitution' 'Pipe after the body' 'Shell behind taskset' 'Body ended by EOF paren' \
  'Two subshells' 'Case pattern' 'Env with an expansion'; do
  check "names bunx in '$step'" "$(contains "$out" "holds a credential, but step '$step' runs [^;]*'bunx'")"
done

# The forms of review 6058074395 and of 5aef29db5a's reviews: a string handed a shell (`bash -c`,
# eval) is read by the release check's own every-body rule and with the arrays of the text around
# it; a quoted paren counts for nothing in `((` or in a case pattern's substitution, as bash parses
# them; and each command word a shell builds that the text says what it is: a trap's string, a
# coprocess, a one-line function, a here-string, an echo piped to a shell, xargs -I running a
# shell's script, an alias, an ANSI-C escape, env -S, and env's `set -u` form of an array. Each runs
# bunx in a credentialed job under bash, and each is refused by name.
echo "case: the strings, quoted parens and built command words that run bunx, in a credentialed job"
root=$(fixture built-word-forms)
add_publish_step "$root" "$(cat <<'YAML'
      - name: Bash -c of eval of a body
        run: |
          bash -c 'eval "$(cat <<"EOT"
          bunx evil-package
          EOT
          )"'
      - name: Eval of eval of a body
        run: |
          eval 'eval "$(cat <<"EOT"
          bunx evil-package
          EOT
          )"'
      - name: Bash -c of a body ended by EOF paren
        run: |
          bash -c 'x=$(cat <<"EOF"
          bunx evil-package
          EOF)
          eval "$x"'
      - name: Eval of a body that sets an array
        run: |
          eval "$(cat <<'EOT'
          a=(bunx evil-package)
          "${a[@]}"
          EOT
          )"
      - name: Piped body that sets an array
        run: |
          cat <<'EOT' |
          a=(bunx evil-package)
          "${a[@]}"
          EOT
          bash
      - name: Quoted paren in two subshells
        run: |
          ((echo "))"; bunx evil-package) || true)
      - name: Quoted paren in a case pattern
        run: |
          case x in "$(echo ")"; bunx evil-package)") : ;; esac
      - name: Trap
        run: |
          trap 'bunx evil-package' EXIT
      - name: Coprocess
        run: |
          coproc bunx evil-package
      - name: Named coprocess
        run: |
          coproc C { bunx evil-package; }
      - name: One-line function
        run: |
          function f { bunx evil-package; }
          f
      - name: Here-string
        run: |
          bash <<< 'bunx evil-package'
      - name: Echo piped to a shell
        run: |
          echo 'bunx evil-package' | bash
      - name: Xargs running a shell script
        run: |
          cat <<'EOT' | xargs -I{} bash -c '{}'
          bunx evil-package
          EOT
      - name: Alias
        run: |
          shopt -s expand_aliases
          alias b='bunx evil-package'
          b
      - name: ANSI-C escape
        run: |
          $'\x62unx' evil-package
      - name: Env -S
        run: |
          env -S 'bunx evil-package'
      - name: Env with a set -u array
        run: |
          env ${args[@]+"${args[@]}"} bash <<'EOT'
          bunx evil-package
          EOT
YAML
)"$'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for step in 'Bash -c of eval of a body' 'Eval of eval of a body' 'Bash -c of a body ended by EOF paren' \
  'Eval of a body that sets an array' 'Piped body that sets an array' 'Quoted paren in two subshells' \
  'Quoted paren in a case pattern' 'Trap' 'Coprocess' 'Named coprocess' 'One-line function' 'Here-string' \
  'Echo piped to a shell' 'Xargs running a shell script' 'Alias' 'ANSI-C escape' 'Env -S' 'Env with a set -u array'; do
  check "names bunx in '$step'" "$(contains "$out" "holds a credential, but step '$step' runs [^;]*'bunx'")"
done

# The forms of review 6065555289: an array that an eval or a `bash -c` string assigns is the text's
# too, so the text after the string, or a function of it, runs its values; a `#` comment inside a
# `$(…)` hides the parens and quotes after it, as bash reads a command substitution; and env takes a
# slice of an array or of the positional parameters as its arguments. Each runs bunx in a
# credentialed job under bash, and each is refused by name.
echo "case: an array an eval string assigns, a comment inside a substitution, and env slices, in a credentialed job"
root=$(fixture eval-array-comment-forms)
add_publish_step "$root" "$(cat <<'YAML'
      - name: Eval of a single-quoted array
        run: |
          eval 'a=(bunx evil-package)'
          "${a[@]}"
      - name: Eval of a double-quoted array
        run: |
          eval "a=(bunx evil-package)"
          "${a[@]}"
      - name: Eval of a declared array
        run: |
          eval 'declare -a a=(bunx evil-package)'
          "${a[@]}"
      - name: Eval of an array a function runs
        run: |
          eval 'a=(bunx evil-package)'
          f() { "${a[@]}"; }
          f
      - name: Bash -c of an eval of an array
        run: |
          bash -c 'eval "a=(bunx evil-package)"; "${a[@]}"'
      - name: Comment in a case pattern's substitution
        run: |
          case x in "$(: # )
          bunx evil-package)") : ;; esac
      - name: Comment hiding a quote in a case pattern's substitution
        run: |
          case "" in "$(: # "
          )") bunx evil-package ;; esac
      - name: Comment in arithmetic's substitution
        run: |
          (( $(: # )
          bunx evil-package)))
      - name: Env with a slice of the parameters
        run: |
          env "${@:2}" bunx evil-package
      - name: Env with a slice of an array
        run: |
          env "${a[@]:0}" bunx evil-package
YAML
)"$'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for step in 'Eval of a single-quoted array' 'Eval of a double-quoted array' 'Eval of a declared array' \
  'Eval of an array a function runs' 'Bash -c of an eval of an array' 'Comment in a case pattern'"'"'s substitution' \
  'Comment hiding a quote in a case pattern'"'"'s substitution' 'Comment in arithmetic'"'"'s substitution' \
  'Env with a slice of the parameters' 'Env with a slice of an array'; do
  check "names bunx in '$step'" "$(contains "$out" "holds a credential, but step '$step' runs [^;]*'bunx'")"
done

# The forms of review 6066469064: an assignment or an alias definition that bash never runs (a trap
# that never fires, a `bash -c` child, an eval under `if false`, an uncalled function) shadows no
# other, so an array appended to after such a decoy and an alias redefined by one still read as
# what bash runs; a `#` after the `)` of a nested substitution continues the word; and a copy, a
# slice, an element or a nameref of an array runs its values, which this check reads as a command.
# Each runs bunx in a credentialed job under bash, and each is refused by name.
echo "case: decoy assignments, a hash after a substitution, and built array words, in a credentialed job"
root=$(fixture decoy-and-hash-forms)
add_publish_step "$root" "$(cat <<'YAML'
      - name: Append after a trap decoy
        run: |
          trap 'a=(true)' USR1
          a+=(bunx evil-package)
          "${a[@]}"
      - name: Append after a bash -c decoy
        run: |
          bash -c 'a=(true)'
          a+=(bunx evil-package)
          "${a[@]}"
      - name: Append after a subshell decoy
        run: |
          ( bash -c 'a=(true)' )
          a+=(bunx evil-package)
          "${a[@]}"
      - name: Append after an uncalled function decoy
        run: |
          f() { eval 'a=(true)'; }
          a+=(bunx evil-package)
          "${a[@]}"
      - name: Append after an if false decoy
        run: |
          if false; then eval 'a=(true)'; fi
          a+=(bunx evil-package)
          "${a[@]}"
      - name: Alias after a trap decoy
        run: |
          shopt -s expand_aliases
          alias b='bunx evil-package'
          trap "alias b=true" USR1
          b
      - name: Alias after a bash -c decoy
        run: |
          shopt -s expand_aliases
          alias b='bunx evil-package'
          bash -c "alias b=true"
          b
      - name: Alias after an if false decoy
        run: |
          shopt -s expand_aliases
          alias b='bunx evil-package'
          if false; then eval "alias b=true"; fi
          b
      - name: Hash after a nested substitution
        run: |
          case x in "$(echo $(echo y)#)") : ;; esac
          bunx evil-package
      - name: Hash after a process substitution
        run: |
          case x in "$(cat <(echo a)#)") : ;; esac
          bunx evil-package
      - name: Array copy
        run: |
          base=(bunx evil-package)
          args=("${base[@]}")
          "${args[@]}"
      - name: Array nameref
        run: |
          a=(bunx evil-package)
          declare -n r=a
          "${r[@]}"
YAML
)"$'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for step in 'Append after a trap decoy' 'Append after a bash -c decoy' 'Append after a subshell decoy' \
  'Append after an uncalled function decoy' 'Append after an if false decoy' 'Alias after a trap decoy' \
  'Alias after a bash -c decoy' 'Alias after an if false decoy' 'Hash after a nested substitution' \
  'Hash after a process substitution' 'Array copy' 'Array nameref'; do
  check "names bunx in '$step'" "$(contains "$out" "holds a credential, but step '$step' runs [^;]*'bunx'")"
done

echo "case: a built binary under out/ in a credentialed job"
root=$(fixture out-path)
add_publish_step "$root" $'      - name: Smoke test\n        run: out/legion version\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the path" "$(contains "$out" "step 'Smoke test' runs 'out/legion'")"

echo "case: a built binary under \$GITHUB_WORKSPACE/out/ in a credentialed job"
root=$(fixture out-workspace-path)
add_publish_step "$root" $'      - name: Smoke test\n        run: |\n          "$GITHUB_WORKSPACE/out/legion" version\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the path" "$(contains "$out" "step 'Smoke test' runs '.GITHUB_WORKSPACE/out/legion'")"

echo "case: a scripts/ program in a credentialed job"
root=$(fixture scripts-path)
add_publish_step "$root" $'      - name: Notices\n        run: scripts/foo.sh\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the path" "$(contains "$out" "step 'Notices' runs 'scripts/foo.sh'")"

echo "case: a scripts/ program through ../../ in a credentialed job"
root=$(fixture scripts-relative-path)
add_publish_step "$root" $'      - name: Notices\n        run: cd packages/daemon && ../../scripts/foo.sh out\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the path" "$(contains "$out" "step 'Notices' runs '../../scripts/foo.sh'")"

echo "case: bun in a comment-only line and in a trailing comment"
root=$(fixture comment-bun)
add_publish_step "$root" $'      - name: Comments\n        run: |\n          # bun install would be dependency code\n          echo ok # bun install\n'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: npm install, npm ci and npm exec in a credentialed job (npm view and npm publish stay allowed)"
root=$(fixture npm-dependency-subcommands)
add_publish_step "$root" $'      - name: Install the publish dependencies\n        run: npm ci\n      - name: Install one more\n        run: npm install --no-save left-pad\n      - name: Run a tool\n        run: npm exec -- some-tool\n      - name: Look up the version\n        run: npm view "@sjawhar/pi-envoy@1.0.0" version\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names npm ci" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Install the publish dependencies' runs 'npm ci'")"
check "names npm install" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Install one more' runs 'npm install'")"
check "names npm exec" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Run a tool' runs 'npm exec'")"
check "does not name npm view" "$(is "$(contains "$out" "step 'Look up the version'")" false)"
check "does not name npm publish" "$(is "$(contains "$out" "step 'Publish to npm'")" false)"

echo "case: a scripts/ program run through an interpreter in a credentialed job"
root=$(fixture scripts-via-interpreter)
add_publish_step "$root" $'      - name: Notices\n        run: bash scripts/go-third-party-notices.sh out/NOTICES ./cmd/legion\n      - name: Notices under sh\n        run: sh scripts/foo.sh\n      - name: Source a helper\n        run: source scripts/lib/helpers.sh\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the script behind bash" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Notices' runs 'scripts/go-third-party-notices.sh'")"
check "names the script behind sh" "$(contains "$out" "step 'Notices under sh' runs 'scripts/foo.sh'")"
check "names the sourced script" "$(contains "$out" "step 'Source a helper' runs 'scripts/lib/helpers.sh'")"

echo "case: a step whose shell is an interpreter in a credentialed job"
root=$(fixture shell-python)
add_publish_step "$root" $'      - name: Helper\n        shell: python\n        run: |\n          import subprocess\n          subprocess.run(["bun", "install"], check=True)\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and its shell" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Helper' .*'python'")"

echo "case: docker build and docker buildx build in a credentialed job"
root=$(fixture docker-build)
add_publish_step "$root" $'      - name: Build image\n        run: docker build .\n      - name: Build image with buildx\n        run: docker buildx build .\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names docker build" "$(contains "$out" "step 'Build image' runs 'docker build'")"
check "names docker buildx build" "$(contains "$out" "step 'Build image with buildx' runs 'docker buildx build'")"

echo "case: a credentialed job sets up Go"
root=$(fixture setup-go)
add_publish_step "$root" "      - uses: actions/setup-go@$SHA"$'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the action as dependency code" \
  "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'actions/setup-go@$SHA' uses 'actions/setup-go@$SHA'; dependency code")"

echo "case: a credentialed job uses an action off the allow-list"
root=$(fixture other-action)
add_publish_step "$root" "      - uses: some/other-action@$SHA"$'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the allow-list" \
  "$(contains "$out" "step 'some/other-action@$SHA' uses 'some/other-action@$SHA', which is not on the checker's allow-list for a job that holds a credential; add it there deliberately")"

echo "case: a credentialed job without cache-mode"
root=$(fixture no-cache-mode)
mutate "$root/$RELEASE" $'    cache-mode: none\n' '' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^  pi_envoy_publish:$'): job 'pi_envoy_publish' holds a credential, so it must set 'cache-mode: none'")"

echo "case: a credentialed job with cache-mode: read"
root=$(fixture cache-mode-read)
mutate "$root/$RELEASE" $'    cache-mode: none\n' $'    cache-mode: read\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the value" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, so it must set 'cache-mode: none' to restore no cache (found \"read\")")"

echo "case: a checkout without persist-credentials"
root=$(fixture checkout-no-persist)
mutate "$root/$RELEASE" $'          persist-credentials: false\n' '' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - uses: actions/checkout@'): job 'pi_envoy_build' step 'actions/checkout@$SHA' must set 'persist-credentials: false', the boolean, so no token is left in the checkout (it is not set)")"

echo "case: a checkout with the string \"false\""
root=$(fixture checkout-persist-string)
mutate "$root/$RELEASE" $'          persist-credentials: false\n' $'          persist-credentials: "false"\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the string at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" 'persist-credentials: "false"'): job 'pi_envoy_build' step 'actions/checkout@$SHA' must set 'persist-credentials: false', the boolean.*(found \"false\", a string)")"

echo "case: a checkout with persist-credentials: true"
root=$(fixture checkout-persist-true)
mutate "$root/$RELEASE" $'          persist-credentials: false\n' $'          persist-credentials: true\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the value" "$(contains "$out" "must set 'persist-credentials: false', the boolean.*(found true)")"

# Rule 6, on each side of its line: the same step before and after its job's checkout, at each of
# the three paths that name the one script (the checkout's own path behind $GITHUB_WORKSPACE, the
# only form of it that is no dependency code, and two that are no scripts/ path), and twice in one
# job. The script it runs is in the checkout, so before it the step exits 127, which is what left
# every legion release's "## Worker image" section unwritten.
# shellcheck disable=SC2016 # the step's own shell expands $GITHUB_WORKSPACE, not this one
for cmd in '$GITHUB_WORKSPACE/.github/scripts/release-push.sh' './release-push.sh' 'release-push.sh'; do
  echo "case: a step runs $cmd before its job's checkout"
  root=$(fixture "release-push-before-checkout-$(printf %s "$cmd" | tr -c '[:alnum:]' -)")
  mutate "$root/$RELEASE" "      - uses: actions/checkout@$SHA"$'\n' $'      - name: Read the tag\n        run: '"$cmd"$' tagged pi-envoy-v\n'"      - uses: actions/checkout@$SHA"$'\n' 2
  run_check "$root"
  check "exits 1" "$(is "$status" 1)"
  check "names the job and the step at its line" \
    "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - name: Read the tag$'): job 'pi_envoy_publish' step 'Read the tag' runs .github/scripts/release-push.sh, but no actions/checkout before it puts the script in the workspace, so the step would exit 127 and the job would carry on\$")"
  check "reports that one problem only" "$(contains "$out" '^1 release credential problem(s)$')"
done

echo "case: the same step after its job's checkout"
root=$(fixture release-push-after-checkout)
add_publish_step "$root" $'      - name: Read the tag\n        run: $GITHUB_WORKSPACE/.github/scripts/release-push.sh tagged pi-envoy-v\n'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: two steps run release-push.sh before their job's checkout"
root=$(fixture two-release-push-before-checkout)
mutate "$root/$RELEASE" "      - uses: actions/checkout@$SHA"$'\n' $'      - name: Read the tag\n        run: ./release-push.sh tagged pi-envoy-v\n      - name: Read the other tag\n        run: release-push.sh tagged pi-legion-v\n'"      - uses: actions/checkout@$SHA"$'\n' 2
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the first step at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - name: Read the tag$'): job 'pi_envoy_publish' step 'Read the tag' runs .github/scripts/release-push.sh, but no actions/checkout")"
check "names the second step at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - name: Read the other tag$'): job 'pi_envoy_publish' step 'Read the other tag' runs .github/scripts/release-push.sh, but no actions/checkout")"
check "reports both" "$(contains "$out" '^2 release credential problem(s)$')"

echo "case: the deploy key as a checkout's ssh-key"
root=$(fixture ssh-key)
mutate "$root/$RELEASE" $'          persist-credentials: false\n' $'          persist-credentials: false\n          ssh-key: ${{ secrets.DEPLOY_KEY }}\n' 2
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the site at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" 'ssh-key:'): job 'pi_envoy_publish' names secrets.DEPLOY_KEY at step 'actions/checkout@$SHA' with.ssh-key; the deploy key may appear only as 'DEPLOY_KEY: \${{ secrets.DEPLOY_KEY }}' in the env of a step whose run calls release-push.sh push-tag\$")"

echo "case: the deploy key in a job-level env"
root=$(fixture job-env-key)
mutate "$root/$RELEASE" $'    cache-mode: none\n' $'    cache-mode: none\n    env:\n      DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the site" "$(contains "$out" "job 'pi_envoy_publish' names secrets.DEPLOY_KEY at env.DEPLOY_KEY;")"

echo "case: the deploy key in the env of a step that runs release-push.sh release"
root=$(fixture release-step-key)
add_publish_step "$root" $'      - name: Create GitHub release\n        env:\n          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG: ${{ steps.handed.outputs.tag }}\n        run: .github/scripts/release-push.sh release "$TAG" "Pi Envoy v1" "tarball/$FILE"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step" "$(contains "$out" "job 'pi_envoy_publish' names secrets.DEPLOY_KEY at step 'Create GitHub release' env.DEPLOY_KEY;")"

echo "case: the deploy key under another env key of a push-tag step"
root=$(fixture other-env-key)
mutate "$root/$RELEASE" $'          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG:' $'          OTHER_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG:' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the key" "$(contains "$out" "job 'pi_envoy_publish' names secrets.DEPLOY_KEY at step 'Push tag' env.OTHER_KEY;")"

echo "case: the deploy key named by index, secrets['DEPLOY_KEY']"
root=$(fixture deploy-key-index-syntax)
mutate "$root/$RELEASE" $'        run: bun install --frozen-lockfile\n' $'        env:\n          KEY: ${{ secrets[\'DEPLOY_KEY\'] }}\n        run: bun install --frozen-lockfile\n' 1
add_publish_step "$root" $'      - name: Create GitHub release\n        env:\n          KEY: ${{ secrets["DEPLOY_KEY"] }}\n        run: .github/scripts/release-push.sh release "$TAG" "Pi Envoy v1" "tarball/$FILE"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the build job's text" "$(contains "$out" "job 'pi_envoy_build' runs dependency code (.*), but its text names .*DEPLOY_KEY")"
check "names the step of the credentialed job" "$(contains "$out" "job 'pi_envoy_publish' names .*DEPLOY_KEY at step 'Create GitHub release' env.KEY")"

echo "case: a build job holding id-token: write"
root=$(fixture build-id-token)
mutate "$root/$RELEASE" $'    permissions:\n      contents: read\n' $'    permissions:\n      contents: read\n      id-token: write\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the scope at the permissions line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    permissions:$'): job 'pi_envoy_build' runs dependency code (step './.github/actions/setup-bun' uses './.github/actions/setup-bun'), but holds 'id-token: write'")"

echo "case: a build job holding actions: write"
root=$(fixture build-actions-write)
mutate "$root/$RELEASE" $'    permissions:\n      contents: read\n' $'    permissions:\n      contents: read\n      actions: write\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the scope at the permissions line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    permissions:$'): job 'pi_envoy_build' runs dependency code (step './.github/actions/setup-bun' uses './.github/actions/setup-bun'), but holds 'actions: write'")"

echo "case: a build job naming the deploy key"
root=$(fixture build-deploy-key)
mutate "$root/$RELEASE" $'        run: bun install --frozen-lockfile\n' $'        env:\n          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n        run: bun install --frozen-lockfile\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the text" "$(contains "$out" "job 'pi_envoy_build' runs dependency code (.*), but its text names secrets.DEPLOY_KEY")"

echo "case: a build job with no permissions block under a workflow granting contents: write"
root=$(fixture build-inherits-write)
mutate "$root/$RELEASE" $'permissions: {}\n' $'permissions:\n  contents: write\n'
mutate "$root/$RELEASE" $'    permissions:\n      contents: read\n' '' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the inherited scope at the workflow's permissions line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^permissions:$'): job 'pi_envoy_build' runs dependency code (.*), but holds 'contents: write' through the workflow's permissions, having none of its own")"

echo "case: a build job with no permissions block in a workflow with none"
root=$(fixture build-no-permissions-anywhere)
mutate "$root/$RELEASE" $'permissions: {}\n' ''
mutate "$root/$RELEASE" $'    permissions:\n      contents: read\n' '' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job, whose token is whatever the repository grants a job that declares no permissions" \
  "$(contains "$out" "job 'pi_envoy_build' runs dependency code (.*), but .*permissions")"

tripwire=$'      - name: Check this job holds no credential (tripwire)\n        run: .github/scripts/release-push.sh no-credential\n'

echo "case: a build job whose tripwire is not its last step"
root=$(fixture tripwire-moved)
mutate "$root/$RELEASE" "$tripwire" ''
mutate "$root/$RELEASE" $'      - uses: actions/upload-artifact@' "$tripwire"$'      - uses: actions/upload-artifact@' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the last step at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - uses: actions/upload-artifact@'): job 'pi_envoy_build' runs dependency code (.*), so its last step must be the tripwire 'run: .github/scripts/release-push.sh no-credential'")"

echo "case: a tripwire step with an if"
root=$(fixture tripwire-if)
mutate "$root/$RELEASE" $'        run: .github/scripts/release-push.sh no-credential\n' $'        if: always()\n        run: .github/scripts/release-push.sh no-credential\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the if at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^        if: always()$'): job 'pi_envoy_build' runs dependency code (.*), so its tripwire step must carry no 'if:'")"

echo "case: a build job with no tripwire"
root=$(fixture no-tripwire)
mutate "$root/$RELEASE" "$tripwire" ''
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "wants the tripwire last" "$(contains "$out" "job 'pi_envoy_build' runs dependency code (.*), so its last step must be the tripwire")"

echo "case: release_publish with cancel-in-progress: true"
root=$(fixture cancel-true)
mutate "$root/$LISTENER" $'      cancel-in-progress: false\n' $'      cancel-in-progress: true\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the concurrency at its line" \
  "$(contains "$out" "^::error::$LISTENER:$(line_of "$root/$LISTENER" '^    concurrency:$'): job 'release_publish' must run in concurrency group 'release-envoy-listener' with 'cancel-in-progress: false', the boolean, and 'queue: max' (found concurrency: {\"group\": \"release-envoy-listener\", \"cancel-in-progress\": true, \"queue\": \"max\"})")"

echo "case: release_publish without queue: max"
root=$(fixture no-queue)
mutate "$root/$LISTENER" $'      queue: max\n' ''
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the concurrency at its line" \
  "$(contains "$out" "^::error::$LISTENER:$(line_of "$root/$LISTENER" '^    concurrency:$'): job 'release_publish' must run in concurrency group 'release-envoy-listener' with 'cancel-in-progress: false', the boolean, and 'queue: max' (found concurrency: {\"group\": \"release-envoy-listener\", \"cancel-in-progress\": false})")"

echo "case: release_publish with queue: single"
root=$(fixture queue-single)
mutate "$root/$LISTENER" $'      queue: max\n' $'      queue: single\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names what it found" "$(contains "$out" "job 'release_publish' must run in concurrency group .*(found concurrency: {\"group\": \"release-envoy-listener\", \"cancel-in-progress\": false, \"queue\": \"single\"})")"

echo "case: release_publish without concurrency"
root=$(fixture no-concurrency)
mutate "$root/$LISTENER" $'    concurrency:\n      group: release-envoy-listener\n      cancel-in-progress: false\n      queue: max\n' ''
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job at its line" \
  "$(contains "$out" "^::error::$LISTENER:$(line_of "$root/$LISTENER" '^  release_publish:$'): job 'release_publish' must run in concurrency group .*(found concurrency: null)")"

echo "case: release_publish in another group"
root=$(fixture other-group)
mutate "$root/$LISTENER" $'      group: release-envoy-listener\n' $'      group: release-${{ github.ref }}\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the group" "$(contains "$out" "job 'release_publish' must run in concurrency group 'release-envoy-listener'.*(found concurrency: {\"group\": \"release-\${{ github.ref }}\"")"

echo "case: no release_publish"
root=$(fixture no-release-publish)
mutate "$root/$LISTENER" $'  release_publish:\n' $'  publish:\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the missing job" "$(contains "$out" "^::error::$LISTENER:$(line_of "$root/$LISTENER" '^jobs:$'): no job 'release_publish'")"

# The green job-level conditions of the two publish jobs, for the rule-5 cases to replace.
envoy_if=$'    if: always() && needs.pi_envoy_build.result == \'success\' && needs.pi_envoy_build.outputs.skip != \'true\'\n'
legion_if=$'    if: always() && needs.pi_legion_build.result == \'success\' && needs.pi_legion_build.outputs.skip != \'true\' && (needs.pi_envoy_publish.result == \'success\' || needs.pi_envoy_publish.result == \'skipped\')\n'

echo "case: a publish job's if: reads the build's outputs behind an implicit success() (the bug's shape)"
root=$(fixture if-implicit-success)
mutate "$root/$RELEASE" "$envoy_if" $'    if: needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job, the missing always() and the skippable ancestor at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.pi_envoy_build.outputs.skip'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own; GitHub's implicit success() is then false whenever any job upstream was skipped, whatever this if: says, so the if: must call always() and test needs.<job>.result for each job it reads")"
check "reports that one problem only" "$(contains "$out" '^1 release credential problem(s)$')"

echo "case: a publish job's if: calls success() explicitly"
root=$(fixture if-explicit-success)
mutate "$root/$RELEASE" "$envoy_if" $'    if: success() && needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the explicit success() is the implicit one: names the job at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: success()'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: a publish job's if: tests the build's result, without always()"
root=$(fixture if-result-no-always)
mutate "$root/$RELEASE" "$envoy_if" $'    if: needs.pi_envoy_build.result == \'success\' && needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the result test decides nothing behind the implicit success(): names the job at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.pi_envoy_build.result'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: a publish job's if: tests the build's result only, reading no outputs"
root=$(fixture if-result-only)
mutate "$root/$RELEASE" "$envoy_if" $'    if: needs.pi_envoy_build.result == \'success\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.pi_envoy_build.result'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: a publish job's if: calls always() and reads the build's outputs without testing its result"
root=$(fixture if-always-no-result)
mutate "$root/$RELEASE" "$envoy_if" $'    if: always() && needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the outputs read and the result not tested at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: always() && needs.pi_envoy_build.outputs'): job 'pi_envoy_publish' calls always() in its if: and reads needs.pi_envoy_build.outputs, but does not test needs.pi_envoy_build.result; always() turns GitHub's own gating off, so a failed or skipped job is read through its empty outputs")"
check "reports that one problem only" "$(contains "$out" '^1 release credential problem(s)$')"

echo "case: an if: over two needs that tests one result and reads the other's outputs (the release note's shape)"
root=$(fixture if-two-needs-one-result)
mutate "$root/$RELEASE" "$legion_if" $'    if: always() && needs.pi_envoy_publish.result == \'success\' && needs.pi_legion_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names only the job whose outputs are read untested, not the one whose result is tested" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: always() && needs.pi_envoy_publish.result'): job 'pi_legion_publish' calls always() in its if: and reads needs.pi_legion_build.outputs, but does not test needs.pi_legion_build.result; always() turns")"

echo "case: a steps-less job's if: reads a skippable job's outputs behind an implicit success()"
root=$(fixture if-steps-less-job)
mutate "$root/$RELEASE" $'    needs: [pi_legion_publish]\n    uses: ./.github/workflows/worker-image.yaml\n' $'    needs: [pi_legion_publish]\n    if: needs.pi_legion_publish.outputs.version != \'\'\n    uses: ./.github/workflows/worker-image.yaml\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job and every skippable ancestor, at any distance, at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.pi_legion_publish.outputs.version'): job 'worker_image' has an if: that calls no always(), but jobs 'pi_envoy_build', 'pi_envoy_publish', 'pi_legion_publish' upstream of it through needs carry an if: of their own")"

echo "case: the build named by index, needs['pi_envoy_build'], behind always()"
root=$(fixture if-index-no-result)
mutate "$root/$RELEASE" "$envoy_if" $'    if: always() && needs[\'pi_envoy_build\'].outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job read by index as by dot" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: always() && needs\['): job 'pi_envoy_publish' calls always() in its if: and reads needs.pi_envoy_build.outputs, but does not test needs.pi_envoy_build.result;")"
root=$(fixture if-index-with-result)
mutate "$root/$RELEASE" "$envoy_if" $'    if: always() && needs[\'pi_envoy_build\'].result == \'success\' && needs[\'pi_envoy_build\'].outputs.skip != \'true\'\n'
run_check "$root"
check "exits 0: the result tested by index is the result read by dot" "$(is "$status" 0)"

echo "case: a boolean if: true"
root=$(fixture if-boolean)
mutate "$root/$RELEASE" "$envoy_if" $'    if: true\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "a boolean calls no always(): names the job at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: true$'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: a folded if: >- carrying the bug (the production shape)"
root=$(fixture if-folded)
mutate "$root/$RELEASE" "$envoy_if" $'    if: >-\n      needs.pi_envoy_build.result == \'success\' &&\n      needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job at the if: >- line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: >-$'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: Always() and the needs context in another casing"
root=$(fixture if-casing)
mutate "$root/$RELEASE" "$envoy_if" $'    if: Always() && NEEDS.pi_envoy_build.Result == \'success\' && needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 0: the function and the context are case-insensitive" "$(is "$status" 0)"

echo "case: 'always()' in a string literal is text (a YAML double-quoted scalar, since a plain one cannot start with a quote)"
root=$(fixture if-quoted-always)
mutate "$root/$RELEASE" "$envoy_if" $'    if: "\'always()\' == needs.pi_envoy_build.outputs.skip"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the quoted always() calls nothing: names the job at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" "^    if: \"'always()' == "): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: the build's outputs read inside fromJSON(…), behind always()"
root=$(fixture if-fromjson)
mutate "$root/$RELEASE" "$envoy_if" $'    if: always() && fromJSON(needs.pi_envoy_build.outputs.skip) != true\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the outputs read inside the call" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: always() && fromJSON('): job 'pi_envoy_publish' calls always() in its if: and reads needs.pi_envoy_build.outputs, but does not test needs.pi_envoy_build.result;")"

echo "case: two jobs needing each other (one needs: a string, one a list), each with an if: reading the other's outputs"
root=$(fixture if-needs-cycle)
cat >> "$root/$RELEASE" <<'YAML'
  cyc_a:
    needs: cyc_b
    if: needs.cyc_b.outputs.x != ''
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    steps:
      - run: echo a
  cyc_b:
    needs: [cyc_a]
    if: needs.cyc_a.outputs.x != ''
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    steps:
      - run: echo b
YAML
run_check "$root"
check "exits 1, the walk having ended" "$(is "$status" 1)"
check "names cyc_a, whose string needs names cyc_b, at its if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.cyc_b.outputs.x'): job 'cyc_a' has an if: that calls no always(), but job 'cyc_b' upstream of it through needs carries an if: of its own")"
check "names cyc_b, whose list needs names cyc_a, at its if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.cyc_a.outputs.x'): job 'cyc_b' has an if: that calls no always(), but job 'cyc_a' upstream of it through needs carries an if: of its own")"
check "reports those two problems only: each job once" "$(contains "$out" '^2 release credential problem(s)$')"

echo "case: a step-level if: reading the build's outputs behind an implicit success()"
root=$(fixture step-if)
add_publish_step "$root" $'      - name: Step-level condition\n        if: needs.pi_envoy_build.outputs.skip != \'true\'\n        run: echo ok\n'
run_check "$root"
check "exits 0: a step's implicit success() is over the job's own earlier steps, not over needs" "$(is "$status" 0)"

echo "case: a missing workflow"
root=$(fixture missing-infra)
rm "$root/$INFRA"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" "$(contains "$out" "^::error::$INFRA is missing$")"

echo "case: a workflow that cannot be parsed as YAML"
root=$(fixture bad-yaml)
printf 'name: CI\non: [push\n' > "$root/$RELEASE"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, without a traceback" "$(contains "$out" "^::error::$RELEASE is not valid YAML")"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a build job holding another secret and the whole secrets context"
root=$(fixture build-other-secrets)
mutate "$root/$RELEASE" $'        run: bun install --frozen-lockfile\n' $'        env:\n          NPM_TOKEN: ${{ secrets.NPM_TOKEN }}\n          ALL: ${{ toJSON(secrets) }}\n        run: bun install --frozen-lockfile\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the secret at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" 'NPM_TOKEN:'): job 'pi_envoy_build' runs dependency code (.*), but its text names secrets.NPM_TOKEN; a build job holds no secret but GITHUB_TOKEN")"
check "names the whole secrets context" \
  "$(contains "$out" "job 'pi_envoy_build' runs dependency code (.*), but its text names \${{ toJSON(secrets) }}; a build job holds no secret but GITHUB_TOKEN")"
check "the listener's docker job, whose login holds secrets.GITHUB_TOKEN, is not named" \
  "$(is "$(contains "$out" "job 'docker'")" false)"

echo "case: check-private-names.sh, run from the checkout, is no dependency code, so a job running it may hold the term list"
# The step release.yaml's claude_envoy_names runs, in the fixture's plain job. Only that one
# script, at the checkout's own path behind $GITHUB_WORKSPACE, is classed so: another scripts/
# path beside it, a script of the same name elsewhere, or its path in a comment or an argument
# leaves the job a build job, which holds no secret.
terms_env=$'        env:\n          PRIVATE_NAME_TERMS: ${{ secrets.PRIVATE_NAME_TERMS }}\n'
names_step=$'      - name: Check the packed plugin names nothing private\n'"$terms_env"$'        run: $GITHUB_WORKSPACE/.github/scripts/check-private-names.sh packages/claude-envoy/dist packages/claude-envoy/legion\n'
bare_step=$'      - name: Check the packed plugin names nothing private\n'"$terms_env"$'        run: .github/scripts/check-private-names.sh packages/claude-envoy/dist packages/claude-envoy/legion\n'
terms_refused="job 'changes' runs dependency code (.*), but its text names secrets.PRIVATE_NAME_TERMS; a build job holds no secret but GITHUB_TOKEN"
root=$(fixture terms-names-job)
mutate "$root/$RELEASE" $'      - run: echo hello\n' "$names_step"
run_check "$root"
check "the check behind \$GITHUB_WORKSPACE, with the list in its step: exits 0" "$(is "$status" 0)"
check "names no problem in that job" "$(is "$(contains "$out" "job 'changes'")" false)"

root=$(fixture terms-names-job-other-script)
mutate "$root/$RELEASE" $'      - run: echo hello\n' "$names_step"$'      - name: Check the plugin names\n        run: .github/scripts/check-plugin-names.sh\n'
run_check "$root"
check "another scripts/ path in the same job: exits 1" "$(is "$status" 1)"
check "refuses the list in what is now a build job" "$(contains "$out" "$terms_refused")"

root=$(fixture terms-same-name-elsewhere)
mutate "$root/$RELEASE" $'      - run: echo hello\n' "${names_step/.github\/scripts\/check-private-names.sh/packages\/x\/scripts\/check-private-names.sh}"
run_check "$root"
check "a script of the same name under another scripts/ directory: exits 1" "$(is "$status" 1)"
check "refuses the list" "$(contains "$out" "$terms_refused")"

root=$(fixture terms-comment-only)
mutate "$root/$RELEASE" $'      - run: echo hello\n' $'      - name: Install\n'"$terms_env"$'        run: bun install --frozen-lockfile # .github/scripts/check-private-names.sh\n'
run_check "$root"
check "the path in a comment only: exits 1" "$(is "$status" 1)"
check "refuses the list" "$(contains "$out" "$terms_refused")"

root=$(fixture terms-argument-only)
mutate "$root/$RELEASE" $'      - run: echo hello\n' $'      - name: Install\n'"$terms_env"$'        run: ./scripts/build.sh .github/scripts/check-private-names.sh\n'
run_check "$root"
check "the path as another command's argument only: exits 1" "$(is "$status" 1)"
check "refuses the list" "$(contains "$out" "$terms_refused")"

echo "case: check-private-names.sh is no dependency code only behind \$GITHUB_WORKSPACE"
# A bare, ./ or ../ path names whatever file sits there once a working-directory, the step's or
# the job's default, or a cd earlier in the run has moved the shell; a path behind
# $GITHUB_WORKSPACE, in either spelling, is the checkout's own wherever the shell stands.
anchored_step=$'      - name: Check the packed plugin names nothing private\n'"$terms_env"$'        run: |\n          "$GITHUB_WORKSPACE/.github/scripts/check-private-names.sh" packages/claude-envoy/dist\n'
root=$(fixture terms-anchored-braces)
mutate "$root/$RELEASE" $'      - run: echo hello\n' "${anchored_step/\$GITHUB_WORKSPACE/\$\{GITHUB_WORKSPACE\}}"
run_check "$root"
check "the path behind \${GITHUB_WORKSPACE}: exits 0" "$(is "$status" 0)"

root=$(fixture terms-anchored-elsewhere)
mutate "$root/$RELEASE" $'      - run: echo hello\n' "${anchored_step/        run:/        working-directory: packages/other
        run:}"
run_check "$root"
check "the anchored path from another working-directory: exits 0" "$(is "$status" 0)"

# The two cd steps are written out whole: in a ${var/pattern/replacement}, Bash 5.2 reads an "&"
# in the replacement as the matched text.
root=$(fixture terms-anchored-after-cd)
mutate "$root/$RELEASE" $'      - run: echo hello\n' $'      - name: Check after a cd\n'"$terms_env"$'        run: |\n          cd packages/other && "$GITHUB_WORKSPACE/.github/scripts/check-private-names.sh" ../claude-envoy/dist\n'
run_check "$root"
check "the anchored path after a cd in the run: exits 0" "$(is "$status" 0)"
check "names no problem in that job" "$(is "$(contains "$out" "job 'changes'")" false)"

root=$(fixture terms-bare-at-root)
mutate "$root/$RELEASE" $'      - run: echo hello\n' "$bare_step"
run_check "$root"
check "the bare path, with no working-directory: exits 1" "$(is "$status" 1)"
check "refuses the list" "$(contains "$out" "$terms_refused")"

root=$(fixture terms-dot-slash-at-root)
mutate "$root/$RELEASE" $'      - run: echo hello\n' "${bare_step/        run: /        run: ./}"
run_check "$root"
check "the ./ path, with no working-directory: exits 1" "$(is "$status" 1)"
check "refuses the list" "$(contains "$out" "$terms_refused")"

root=$(fixture terms-cd-in-step)
mutate "$root/$RELEASE" $'      - run: echo hello\n' $'      - name: Check after a cd\n'"$terms_env"$'        run: cd packages/other && .github/scripts/check-private-names.sh ../claude-envoy/dist\n'
run_check "$root"
check "a cd and then the bare path, in a step: exits 1" "$(is "$status" 1)"
check "refuses the list" "$(contains "$out" "$terms_refused")"
check "names the step and the script it runs" \
  "$(contains "$out" "step 'Check after a cd' runs '.github/scripts/check-private-names.sh'")"

root=$(fixture terms-step-working-directory)
mutate "$root/$RELEASE" $'      - run: echo hello\n' "${bare_step/        run:/        working-directory: packages/other
        run:}"
run_check "$root"
check "the bare path under the step's working-directory: exits 1" "$(is "$status" 1)"
check "refuses the list" "$(contains "$out" "$terms_refused")"

root=$(fixture terms-job-working-directory)
mutate "$root/$RELEASE" $'  changes:\n    runs-on: ubuntu-24.04\n' $'  changes:\n    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        working-directory: packages/other\n'
mutate "$root/$RELEASE" $'      - run: echo hello\n' "$bare_step"
run_check "$root"
check "the bare path under the job's default working-directory: exits 1" "$(is "$status" 1)"
check "refuses the list" "$(contains "$out" "$terms_refused")"

root=$(fixture terms-parent-path)
mutate "$root/$RELEASE" $'      - run: echo hello\n' "${bare_step/.github\/scripts\/check-private-names.sh/..\/.github\/scripts\/check-private-names.sh}"
run_check "$root"
check "the path behind ../: exits 1" "$(is "$status" 1)"
check "refuses the list" "$(contains "$out" "$terms_refused")"

echo "case: a local composite action is read through its steps"
# action <name> <run>: a composite action in the fixture whose one step runs that text, as a YAML
# block.
action() {
  mkdir -p "$root/.github/actions/$1"
  # shellcheck disable=SC2016 # the `${{ … }}` is the action's own text, written literally
  printf 'name: %s\nruns:\n  using: composite\n  steps:\n    - name: Scan\n      shell: bash\n      env:\n        PRIVATE_NAME_TERMS: ${{ inputs.terms }}\n      run: |\n        %s\n' \
    "$1" "$2" > "$root/.github/actions/$1/action.yml"
}
uses_step=$'      - uses: ./.github/actions/scan\n        with:\n          terms: ${{ secrets.PRIVATE_NAME_TERMS }}\n'
root=$(fixture terms-action-check)
# shellcheck disable=SC2016 # the step's own shell expands $GITHUB_WORKSPACE, not this one
action scan '"$GITHUB_WORKSPACE/.github/scripts/check-private-names.sh" packages/claude-envoy/dist'
mutate "$root/$RELEASE" $'      - run: echo hello\n' "$uses_step"
run_check "$root"
check "an action whose step runs the checkout's check-private-names.sh: exits 0" "$(is "$status" 0)"

root=$(fixture terms-action-bun)
action scan 'bun install --frozen-lockfile'
mutate "$root/$RELEASE" $'      - run: echo hello\n' "$uses_step"
run_check "$root"
check "an action whose step runs bun: exits 1" "$(is "$status" 1)"
check "names the action's step" \
  "$(contains "$out" "job 'changes' runs dependency code (step './.github/actions/scan' uses './.github/actions/scan', whose step 'Scan' runs 'bun'), but its text names secrets.PRIVATE_NAME_TERMS")"

root=$(fixture terms-action-missing)
mutate "$root/$RELEASE" $'      - run: echo hello\n' "$uses_step"
run_check "$root"
check "an action the check cannot read: exits 1" "$(is "$status" 1)"
check "refuses the list" "$(contains "$out" "$terms_refused")"

root=$(fixture terms-action-bare)
action scan '.github/scripts/check-private-names.sh packages/claude-envoy/dist'
mutate "$root/$RELEASE" $'      - run: echo hello\n' "$uses_step"
run_check "$root"
check "an action whose step runs the bare path: exits 1" "$(is "$status" 1)"
check "refuses the list" "$(contains "$out" "$terms_refused")"

root=$(fixture terms-action-cd)
action scan 'cd packages/other && .github/scripts/check-private-names.sh packages/claude-envoy/dist'
mutate "$root/$RELEASE" $'      - run: echo hello\n' "$uses_step"
run_check "$root"
check "an action whose step runs a cd and then the bare path: exits 1" "$(is "$status" 1)"
check "names the action's step and the script it runs" \
  "$(contains "$out" "whose step 'Scan' runs '.github/scripts/check-private-names.sh'")"

echo "case: the term list in a build job's verify-build step is refused, as any secret but GITHUB_TOKEN is"
root=$(fixture terms-in-build-job)
mutate "$root/$RELEASE" $'        run: bun install --frozen-lockfile\n' \
  $'        run: bun install --frozen-lockfile\n      - name: Build, check and pack the plugin\n'"$terms_env"$'        run: bun run verify-build "$RUNNER_TEMP/plugin.tar"\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the list at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" 'PRIVATE_NAME_TERMS:'): job 'pi_envoy_build' runs dependency code (.*), but its text names secrets.PRIVATE_NAME_TERMS; a build job holds no secret but GITHUB_TOKEN")"

echo "case: docker run in a credentialed job (docker buildx imagetools, docker login and docker buildx version stay allowed)"
root=$(fixture docker-run)
add_publish_step "$root" $'      - name: Smoke the image\n        run: docker run alpine sh\n      - name: Inspect the image\n        run: docker buildx imagetools inspect x && docker login ghcr.io && docker buildx version\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names docker run" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Smoke the image' runs 'docker run'")"
check "does not name the inspect step" "$(is "$(contains "$out" "step 'Inspect the image'")" false)"

echo "case: a checkout written Actions/Checkout, without persist-credentials"
root=$(fixture checkout-casing)
mutate "$root/$RELEASE" "      - uses: actions/checkout@$SHA"$'\n        with:\n          fetch-depth: 0\n          persist-credentials: false\n' "      - uses: Actions/Checkout@$SHA"$'\n        with:\n          fetch-depth: 0\n' 1
mutate "$root/$RELEASE" "      - uses: actions/checkout@$SHA"$'\n' "      - uses: Actions/Checkout@$SHA"$'\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the build job's checkout at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - uses: Actions/Checkout@'): job 'pi_envoy_build' step 'Actions/Checkout@$SHA' must set 'persist-credentials: false', the boolean, so no token is left in the checkout (it is not set)")"
check "the credentialed job's Actions/Checkout, with persist-credentials: false, is on the allow-list" \
  "$(is "$(contains "$out" "not on the checker's allow-list")" false)"
check "reports that one problem only" "$(contains "$out" '^1 release credential problem(s)$')"

echo "case: the deploy key in the workflow-level env"
root=$(fixture workflow-env-key)
mutate "$root/$RELEASE" $'permissions: {}\n' $'permissions: {}\nenv:\n  DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the workflow's env for the build job at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^  DEPLOY_KEY:'): job 'pi_envoy_build' runs dependency code (.*), but its text names secrets.DEPLOY_KEY; a build job holds no secret but GITHUB_TOKEN")"
check "names the workflow's env for a credentialed job at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^  DEPLOY_KEY:'): job 'pi_envoy_publish' names secrets.DEPLOY_KEY at the workflow's env.DEPLOY_KEY; the deploy key may appear only as 'DEPLOY_KEY: \${{ secrets.DEPLOY_KEY }}' in the env of a step whose run calls release-push.sh push-tag\$")"
check "names the workflow's env for the other credentialed job too" \
  "$(contains "$out" "job 'pi_legion_publish' names secrets.DEPLOY_KEY at the workflow's env.DEPLOY_KEY;")"
check "the plain job, credentialed through the workflow's env, must set cache-mode: none" \
  "$(contains "$out" "job 'changes' holds a credential, so it must set 'cache-mode: none'")"

echo "case: a workflow-level defaults.run.shell that is an interpreter"
root=$(fixture workflow-shell-python)
mutate "$root/$RELEASE" $'permissions: {}\n' $'permissions: {}\ndefaults:\n  run:\n    shell: python\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for step in 'Check the hand-over' 'Publish to npm' 'Push tag' 'Create GitHub release'; do
  check "names the credentialed job's run step '$step' under the workflow's shell" \
    "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step '$step' runs its body under shell 'python', not bash or sh; dependency code runs only in a job that holds none")"
done
check "does not name a step with no run" "$(is "$(contains "$out" "step 'actions/checkout@$SHA' runs its body")" false)"

echo "case: a job-level defaults.run.shell that is an interpreter"
root=$(fixture job-shell-pwsh)
mutate "$root/$RELEASE" $'    cache-mode: none\n' $'    cache-mode: none\n    defaults:\n      run:\n        shell: pwsh\n' 1
add_publish_step "$root" $'      - name: Under bash\n        shell: bash\n        run: echo ok\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for step in 'Check the hand-over' 'Publish to npm' 'Push tag' 'Create GitHub release'; do
  check "names pi_envoy_publish's run step '$step' under the job's shell" \
    "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step '$step' runs its body under shell 'pwsh', not bash or sh")"
done
check "a step's own shell: bash wins over the job's" "$(is "$(contains "$out" "step 'Under bash'")" false)"
check "does not name pi_legion_publish, whose shell is bash" "$(is "$(contains "$out" "job 'pi_legion_publish'")" false)"

echo "case: the deploy key in lower case, secrets.deploy_key"
root=$(fixture deploy-key-lower-case)
mutate "$root/$RELEASE" $'      - run: echo hello\n' $'      - env:\n          KEY: ${{ secrets.deploy_key }}\n        run: echo hello\n'
mutate "$root/$RELEASE" $'          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG:' $'          DEPLOY_KEY: ${{ Secrets.deploy_key }}\n          TAG:' 1
mutate "$root/$RELEASE" $'          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG:' $'          deploy_key: ${{ secrets.DEPLOY_KEY }}\n          TAG:'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the plain job is credentialed: names the site at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" 'secrets.deploy_key }}$'): job 'changes' names secrets.DEPLOY_KEY at step 2 env.KEY; the deploy key may appear only as")"
check "the plain job is credentialed: wants cache-mode: none" \
  "$(contains "$out" "job 'changes' holds a credential, so it must set 'cache-mode: none' to restore no cache (it is not set)")"
check "the value form in another casing, under DEPLOY_KEY of a push step, is allowed" \
  "$(is "$(contains "$out" "job 'pi_envoy_publish'")" false)"
check "the env key deploy_key is not DEPLOY_KEY, which release-push.sh reads" \
  "$(contains "$out" "job 'pi_legion_publish' names secrets.DEPLOY_KEY at step 'Push tag' env.deploy_key;")"

echo "case: npm publish without a tarball right after it in a credentialed job"
root=$(fixture npm-publish-directory)
add_publish_step "$root" $'      - name: Publish the directory\n        run: npm publish --access public --provenance\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the directory publish and why" \
  "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Publish the directory' runs 'npm publish' without a tarball right after it (the tarball must be the word after \`publish\`; a directory publish runs the package's prepack); dependency code runs only in a job that holds none")"
check "does not name the publish of the handed tarball" "$(is "$(contains "$out" "step 'Publish to npm'")" false)"

echo "case: a tripwire step with continue-on-error"
root=$(fixture tripwire-continue-on-error)
mutate "$root/$RELEASE" $'        run: .github/scripts/release-push.sh no-credential\n' $'        continue-on-error: true\n        run: .github/scripts/release-push.sh no-credential\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the key at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^        continue-on-error: true$'): job 'pi_envoy_build' runs dependency code (.*), so its tripwire step must carry no 'continue-on-error:'")"

echo "case: a script behind an interpreter's -o or +O option, commands behind eval, and docker's long forms, in a credentialed job"
root=$(fixture option-flags-eval-docker-long-forms)
add_publish_step "$root" $'      - name: Behind -o\n        run: bash -o pipefail scripts/x.sh\n      - name: Behind +O\n        run: bash +O extglob scripts/y.sh\n      - name: Eval quoted\n        run: eval "bun install"\n      - name: Eval bare\n        run: eval bun install\n      - name: Docker long forms\n        run: |\n          docker container run alpine sh\n          docker image build .\n          docker buildx bake\n          docker container exec x sh\n          docker image ls\n          docker container ls\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the script behind -o pipefail" "$(contains "$out" "step 'Behind -o' runs 'scripts/x.sh'")"
check "names the script behind +O extglob" "$(contains "$out" "step 'Behind +O' runs 'scripts/y.sh'")"
check "names bun behind a quoted eval" "$(contains "$out" "step 'Eval quoted' runs 'bun'")"
check "names bun behind a bare eval" "$(contains "$out" "step 'Eval bare' runs 'bun'")"
check "names docker's long forms as written, and not its listings" \
  "$(contains "$out" "step 'Docker long forms' runs 'docker container run', 'docker image build', 'docker buildx bake', 'docker container exec'; dependency code")"

echo "case: a build job with continue-on-error"
root=$(fixture build-continue-on-error)
mutate "$root/$RELEASE" $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n' $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n    continue-on-error: true\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job's key at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    continue-on-error: true$'): job 'pi_envoy_build' runs dependency code (.*), so it must carry no 'continue-on-error:' (a true or an expression there reports a failed tripwire as success to the jobs that need it)")"
check "does not name the tripwire step, which carries none" "$(is "$(contains "$out" "tripwire step must carry no")" false)"
check "reports that one problem only" "$(contains "$out" '^1 release credential problem(s)$')"

echo "case: a build job whose defaults.run.shell is cat {0}, which prints the tripwire instead of running it"
root=$(fixture build-shell-cat)
mutate "$root/$RELEASE" $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n' $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        shell: \'cat {0}\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the tripwire's shell at the job's shell line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" "^        shell: 'cat {0}'$"): job 'pi_envoy_build' runs dependency code (.*), so its tripwire step must run under bash or sh (found 'cat'), which is what runs .github/scripts/release-push.sh no-credential")"
check "reports that one problem only: the build job's other steps may run under anything" \
  "$(contains "$out" '^1 release credential problem(s)$')"

echo "case: a workflow-level defaults.run.shell that is an interpreter reaches the build job's tripwire"
root=$(fixture workflow-shell-python-tripwire)
mutate "$root/$RELEASE" $'permissions: {}\n' $'permissions: {}\ndefaults:\n  run:\n    shell: python\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the tripwire's shell at the workflow's shell line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    shell: python$'): job 'pi_envoy_build' runs dependency code (.*), so its tripwire step must run under bash or sh (found 'python'), which is what runs .github/scripts/release-push.sh no-credential")"
check "names a credentialed job's step under the same shell" \
  "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Publish to npm' runs its body under shell 'python', not bash or sh")"

echo "case: a tripwire step with its own shell: python"
root=$(fixture tripwire-shell-python)
mutate "$root/$RELEASE" $'        run: .github/scripts/release-push.sh no-credential\n' $'        shell: python\n        run: .github/scripts/release-push.sh no-credential\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the tripwire's shell at the step's shell line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^        shell: python$'): job 'pi_envoy_build' runs dependency code (.*), so its tripwire step must run under bash or sh (found 'python')")"

echo "case: a tripwire step whose own shell is a bash template under a job default of pwsh"
root=$(fixture tripwire-shell-template)
mutate "$root/$RELEASE" $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n' $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        shell: pwsh\n'
mutate "$root/$RELEASE" $'        run: .github/scripts/release-push.sh no-credential\n' $'        shell: bash -e {0}\n        run: .github/scripts/release-push.sh no-credential\n'
run_check "$root"
check "exits 0: the step's bash wins over the job's pwsh" "$(is "$status" 0)"

echo "case: npm publish with a .tgz behind a flag in a credentialed job"
root=$(fixture npm-publish-tgz-behind-flag)
add_publish_step "$root" $'      - name: Publish under a tag\n        run: npm publish --tag x.tgz\n      - name: Publish with an otp\n        run: npm publish --otp 1.tgz\n      - name: Publish under a one-word tag\n        run: npm publish --tag=x.tgz\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for step in 'Publish under a tag' 'Publish with an otp' 'Publish under a one-word tag'; do
  check "names '$step' as a publish without a tarball right after it" \
    "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step '$step' runs 'npm publish' without a tarball right after it (the tarball must be the word after \`publish\`; a directory publish runs the package's prepack)")"
done
check "does not name the publish of the handed tarball, right after publish" "$(is "$(contains "$out" "step 'Publish to npm'")" false)"

echo "case: docker's global options before its subcommand in a credentialed job"
root=$(fixture docker-global-options)
add_publish_step "$root" $'      - name: Global config\n        run: docker --config /tmp/c run alpine\n      - name: Global host\n        run: docker -H tcp://x:2375 run alpine\n      - name: Global log level\n        run: docker --log-level debug container run alpine\n      - name: Global one-word and boolean\n        run: |\n          docker --config=/tmp/c run alpine\n          docker -D build .\n      - name: Global config before imagetools\n        run: docker --config /tmp/c buildx imagetools create --tag x y\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names docker run behind --config" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Global config' runs 'docker run'")"
check "names docker run behind -H" "$(contains "$out" "step 'Global host' runs 'docker run'")"
check "names docker container run behind --log-level" "$(contains "$out" "step 'Global log level' runs 'docker container run'")"
check "names docker run behind --config=value and docker build behind -D" \
  "$(contains "$out" "step 'Global one-word and boolean' runs 'docker run', 'docker build'")"
check "does not name imagetools behind --config" "$(is "$(contains "$out" "step 'Global config before imagetools'")" false)"

echo "case: a backslash-continued command is one command, not one per line"
root=$(fixture backslash-continuation)
add_publish_step "$root" $'      - name: Hand over a long list\n        run: |\n          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" handed pi-envoy-v "$VERSION" tarball \\\n            scripts/not-a-command.tgz \\\n            out/not-a-command-either.tgz\n      - name: Install across lines\n        run: |\n          bun \\\n            install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "does not read a continued argument line as a command" "$(is "$(contains "$out" "step 'Hand over a long list'")" false)"
check "still names the command a continuation starts" "$(contains "$out" "step 'Install across lines' runs 'bun'")"

echo "case: a backslash ending a comment, or an escaped backslash, continues nothing"
root=$(fixture comment-backslash)
add_publish_step "$root" $'      - name: Install behind a comment\n        env:\n          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG: ${{ steps.handed.outputs.tag }}\n        run: |\n          # keep the toolchain pinned \\\n          bun install --frozen-lockfile\n          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" push-tag "$TAG"\n      - name: Install behind an escaped backslash\n        run: |\n          printf \'x\' \\\\\n          bun install\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command under a comment's trailing backslash" "$(contains "$out" "step 'Install behind a comment' runs 'bun'")"
check "names the command under an escaped backslash" "$(contains "$out" "step 'Install behind an escaped backslash' runs 'bun'")"

echo "case: a comment after a separator, ending in a backslash, continues nothing"
root=$(fixture separator-comment)
add_publish_step "$root" $'      - name: Push tag behind a comment\n        env:\n          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG: ${{ steps.handed.outputs.tag }}\n        run: |\n          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" push-tag "$TAG" ;# keep the toolchain pinned \\\n          bun install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command under the comment's trailing backslash" "$(contains "$out" "step 'Push tag behind a comment' runs 'bun'")"

echo "case: a quote carries across newlines, so a # opening its second line is no comment"
root=$(fixture quoted-newline)
add_push_tag_step "$root" 'Install after a quoted note' $'          notes=\'Release notes:\n          # keep the toolchain pinned\' ; bun install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command after the closing quote" "$(contains "$out" "step 'Install after a quoted note' runs 'bun'")"

echo "case: an escaped space before a #, or a continuation gluing a # to a word, makes no comment"
root=$(fixture escaped-hash)
add_push_tag_step "$root" 'Escaped space' $'          echo a\\ #; bun install --frozen-lockfile\n'
add_push_tag_step "$root" 'Glued hash' $'          echo a\\\n          #;bun install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command after an escaped space and a #" "$(contains "$out" "step 'Escaped space' runs 'bun'")"
check "names the command after a glued #" "$(contains "$out" "step 'Glued hash' runs 'bun'")"

echo "case: an odd run of backslashes before a newline continues the command"
root=$(fixture odd-backslashes)
add_push_tag_step "$root" 'Three backslashes' $'          printf \'x\' \\\\\\\n            bun\n'
run_check "$root"
check "exits 0: the bun on the next line is printf's argument" "$(is "$status" 0)"

echo "case: a # inside a word, in \${#x} or in a string that resumes behind \$(…) is no comment"
root=$(fixture hash-in-word)
add_push_tag_step "$root" 'Resumed string' $'          echo "$(date) # not a comment"; bun install --frozen-lockfile\n'
add_push_tag_step "$root" 'Length expansion' $'          echo ${#x}; bun install --frozen-lockfile\n'
add_push_tag_step "$root" 'Hash mid-word' $'          echo a#b; bun install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command after a string that resumes behind \$(…)" "$(contains "$out" "step 'Resumed string' runs 'bun'")"
check "names the command after \${#x}" "$(contains "$out" "step 'Length expansion' runs 'bun'")"
check "names the command after a#b" "$(contains "$out" "step 'Hash mid-word' runs 'bun'")"

echo "case: a command name or a separator inside a string is text"
root=$(fixture quoted-text)
add_push_tag_step "$root" 'Quoted bun' $'          echo "bun install"\n'
add_push_tag_step "$root" 'Quoted separator' $'          echo "a; bun install"\n'
run_check "$root"
check "exits 0: bun inside a string runs nothing" "$(is "$status" 0)"

echo "case: a command substitution, by \$(…) or backticks, is a command of its own"
root=$(fixture substitution)
add_push_tag_step "$root" 'Dollar paren' $'          x=$(bun install)\n'
add_push_tag_step "$root" 'Backticks' $'          y=`bun install`\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command inside \$(…)" "$(contains "$out" "step 'Dollar paren' runs 'bun'")"
check "names the command inside backticks" "$(contains "$out" "step 'Backticks' runs 'bun'")"

echo "case: a redirection's target is not a command; the same path in command position is"
root=$(fixture redirect-target)
add_push_tag_step "$root" 'Write a file' $'          echo x > out/x.txt\n'
run_check "$root"
check "exits 0: out/x.txt behind > is a target" "$(is "$status" 0)"
root=$(fixture redirect-command)
add_push_tag_step "$root" 'Run the file' $'          out/x.txt\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the path in command position" "$(contains "$out" "step 'Run the file' runs 'out/x.txt'")"

echo "case: changie in a credentialed job"
root=$(fixture publish-changie)
add_publish_step "$root" $'      - name: Check changie\n        run: changie --version\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job, the step and changie at the step's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - name: Check changie$'): job 'pi_envoy_publish' holds a credential, but step 'Check changie' runs 'changie'; dependency code runs only in a job that holds none$")"
check "and, the job now running dependency code, that it holds contents: write" \
  "$(contains "$out" "job 'pi_envoy_publish' runs dependency code (step 'Check changie' runs 'changie'), but holds 'contents: write'$")"

echo "case: changie in a build job"
root=$(fixture build-changie)
mutate "$root/$RELEASE" $'      - name: Render the changelog entries\n' $'      - name: Check changie\n        run: changie --version\n      - name: Render the changelog entries\n'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: release-push.sh fold-notes in a credentialed job"
root=$(fixture publish-fold-notes)
add_publish_step "$root" $'      - name: Render notes\n        run: .github/scripts/release-push.sh fold-notes 1.2.3 "$RUNNER_TEMP/n" packages/pi-envoy pi-envoy-v\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job, the step and release-push.sh fold-notes at the step's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - name: Render notes$'): job 'pi_envoy_publish' holds a credential, but step 'Render notes' runs '.github/scripts/release-push.sh fold-notes' (it runs changie); dependency code runs only in a job that holds none$")"

echo "case: release-push.sh fold-notes with no path, behind bash, in a credentialed job"
root=$(fixture publish-bare-fold-notes)
add_publish_step "$root" $'      - name: Render notes\n        run: bash release-push.sh fold-notes "$RUNNER_TEMP/n"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names release-push.sh fold-notes" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Render notes' runs 'release-push.sh fold-notes' (it runs changie);")"

echo "case: release-push.sh fold-notes alone makes a job a build job"
root=$(fixture plain-fold-notes)
mutate "$root/$RELEASE" $'      - run: echo hello\n' $'      - run: .github/scripts/release-push.sh fold-notes 1.2.3 "$RUNNER_TEMP/n" packages/pi-envoy pi-envoy-v\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the plain job, running changie through fold-notes alone, must end in the tripwire" \
  "$(contains "$out" "job 'changes' runs dependency code (step 2 runs '.github/scripts/release-push.sh fold-notes' (it runs changie)), so its last step must be the tripwire 'run: .github/scripts/release-push.sh no-credential'$")"

echo "case: release-push.sh through ./ in a credentialed job is no longer the checkout's own"
# A bare or ./ path names whatever file a working-directory or a `cd` left there, so only the path
# behind $GITHUB_WORKSPACE is the checkout's release-push.sh.
root=$(fixture publish-push-tag-dot-slash)
mutate "$root/$RELEASE" $'          "$GITHUB_WORKSPACE/.github/scripts/release-push.sh" push-tag "$TAG"\n' $'          ./.github/scripts/release-push.sh push-tag "$TAG"\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the ./ path as dependency code in the credentialed job" \
  "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Push tag' runs './.github/scripts/release-push.sh'")"
check "and refuses the deploy key in a step that no longer runs the checkout's push" \
  "$(contains "$out" "job 'pi_envoy_publish' names secrets.DEPLOY_KEY at step 'Push tag' env.DEPLOY_KEY")"

echo "case: the deploy key in a build job's render step"
root=$(fixture build-render-deploy-key)
mutate "$root/$RELEASE" $'        run: .github/scripts/release-push.sh fold-notes ' $'        env:\n          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n        run: .github/scripts/release-push.sh fold-notes '
run_check "$root"
check "exits 1" "$(is "$status" 1)"
# shellcheck disable=SC2016 # the `${{ … }}` is the workflow's own text, matched literally
check "names the key in a job that runs dependency code, at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^          DEPLOY_KEY: \${{ secrets.DEPLOY_KEY }}$'): job 'pi_envoy_build' runs dependency code (step './.github/actions/setup-bun' uses './.github/actions/setup-bun'), but its text names secrets.DEPLOY_KEY; a build job holds no secret but GITHUB_TOKEN$")"
check "names the key outside a step that pushes" \
  "$(contains "$out" ": job 'pi_envoy_build' names secrets.DEPLOY_KEY at step 'Render the changelog entries' env.DEPLOY_KEY; ")"

summary "check-release-credentials.sh"
