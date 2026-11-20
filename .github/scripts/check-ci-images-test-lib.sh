#!/usr/bin/env bash
# Sourced by the three harnesses of `check-ci-images.sh`: check-ci-images.test.sh (the images a
# job pulls before checkout, the two modes, and the repository's own rows in the package mode),
# check-ci-images-rows.test.sh (the rows of .github/ci-images.yaml and the package that holds
# them) and check-ci-images-references.test.sh (the references CI's reach names, and the sweep
# for the registry CI fetched from before): what more than one reads and nothing one alone does.
# It creates the run's temporary directory, sources test-lib.sh, and holds the digests, the
# stand-in for the package's reads, the fixture tree and the helpers the cases share.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script, the path model its sweep walks the tree with (workflow_paths.py) and the reference
# grammar it reads a reference with (ci_images.py), into it, so the code under test is the code
# CI runs, not a copy that can drift: the script resolves its root from its own path
# (`cd "$(dirname "$0")/../.."`), which the symlink puts at the fixture root.
# A fixture holds every file CI's reach names, because the check fails on a reach file that is
# gone.
#
# NO CASE REACHES THE NETWORK. The check reads the package through CI_IMAGES_MANIFEST_HEAD:
# when that variable is set, it runs the command it names once per row and reads what the
# command prints — the HTTP status code of the manifest HEAD, and after it the
# legion.browser-image.source-sha256 label of the image at that digest, where the stand-in
# answers for one. Every case sets it to `head.sh` below, which answers 200 for every
# digest but the ones MISSING names, 404 for those, and the label LABELS pairs with a digest.
# So the two checks that read ghcr.io are driven by a case, and nothing is pulled.
#
# EVERY CASE NAMES ITS MODE. The check takes one mode: `tree` runs the checks that read the
# files of the tree, and `package` runs the two that read ghcr.io. Each case runs under the
# mode that owns its refusal, and one case of check-ci-images.test.sh holds each mode to its own
# half — the tree mode runs the stand-in for the network never, and the package mode passes a
# tree the tree mode refuses.
#
# Not executable: a harness sources it after its own `set -euo pipefail` and
# `shopt -s inherit_errexit`.
# shellcheck disable=SC2034 # the digests and paths below are the harnesses' to read

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-ci-images.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The digests the fixtures use. Made up: nothing pulls them, and what each case reads is the row
# they match.
NATS=sha256:0a1b2c3d4e5f60710a1b2c3d4e5f60710a1b2c3d4e5f60710a1b2c3d4e5f6071
BUN=sha256:1b2c3d4e5f6071821b2c3d4e5f6071821b2c3d4e5f6071821b2c3d4e5f607182
BUN_SLIM=sha256:2c3d4e5f607182932c3d4e5f607182932c3d4e5f607182932c3d4e5f60718293
GOLANG=sha256:3d4e5f60718293a43d4e5f60718293a43d4e5f60718293a43d4e5f60718293a4
GOLANG_NEXT=sha256:5f60718293a4b5c65f60718293a4b5c65f60718293a4b5c65f60718293a4b5c6
UNKNOWN=sha256:9f8e7d6c5b4a30219f8e7d6c5b4a30219f8e7d6c5b4a30219f8e7d6c5b4a3021
BROWSER=sha256:60718293a4b5c6d760718293a4b5c6d760718293a4b5c6d760718293a4b5c6d7
REGISTRY=ghcr.io/cybermonklabs/legion/ci

# The stand-in for the package's reads: the package holds every digest but the ones MISSING
# names, and the image at a digest LABELS pairs a sha256 with carries that sha256 in its
# legion.browser-image.source-sha256 label, which is what rule 10 holds a built row's build
# recipe to. LABELS is `<digest>=<sha256>`, space separated; a digest it does not name is
# answered with the status alone, which is an answer for the manifest and not for a label, and
# which rule 10 refuses for a `built` row rather than passing it over — so every case below
# whose package holds a built row names that row's label.
head_script="$work/head.sh"
cat > "$head_script" <<'SH'
#!/usr/bin/env bash
status=200
case " ${MISSING:-} " in
  *" $1 "*) status=404 ;;
esac
label=
for pair in ${LABELS:-}; do
  case "$pair" in
    "$1="*) label=" ${pair#*=}" ;;
  esac
done
echo "$status$label"
SH
chmod +x "$head_script"

# fixture <name>: a tree that passes — the rows, the pins, and every file of CI's reach, each
# naming a digest the rows hold. Echoes the fixture root. Each case names its own tree: two
# cases sharing a name would share a tree, so a repeated name is refused here rather than
# surfacing as the `ln -s` below failing.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/.github/docker" \
    "$root/.github/actions/pull-test-images" \
    "$root/packages/envoy/docker" "$root/packages/envoy/infra/host/heldportwrapper" \
    "$root/packages/daemon/docker" "$root/packages/daemon/internal/testnats" \
    "$root/packages/envoy/internal/testnats" "$root/packages/envoy/internal/testpg" \
    "$root/packages/envoy/internal/tests3" "$root/packages/envoy-client/src/__tests__" \
    "$root/scripts/e2e/lib" "$root/packages/dispatch/e2e" \
    "$root/packages/pi-legion/scripts/skill-scenarios" "$root/packages/envoy/deploy/scripts"
  ln -s "$check_script" "$root/.github/scripts/check-ci-images.sh"
  ln -s "$script_dir/workflow_paths.py" "$root/.github/scripts/workflow_paths.py"
  ln -s "$script_dir/ci_images.py" "$root/.github/scripts/ci_images.py"
  echo "1.3.14" > "$root/.bun-version"
  cat > "$root/.github/ci-images.yaml" <<YAML
registry: $REGISTRY

images:
  - upstream: docker.io/library/nats:2.10
    tag: nats-2.10-0a1b2c3d
    digest: $NATS
  - upstream: docker.io/oven/bun:1.3.14
    tag: bun-1.3.14-1b2c3d4e
    digest: $BUN
  - upstream: docker.io/oven/bun:1.3.14-slim
    tag: bun-1.3.14-slim-2c3d4e5f
    digest: $BUN_SLIM
  - upstream: docker.io/library/golang:1.25.0-alpine
    tag: golang-1.25.0-alpine-3d4e5f60
    digest: $GOLANG
YAML
  cat > "$root/.github/workflows/ci.yaml" <<YAML
name: CI
on: [push]
jobs:
  test:
    runs-on: ubuntu-24.04
    services:
      nats:
        image: $REGISTRY@$NATS
        credentials: \${{ fromJSON('{"username":"anonymous"}') }}
    steps:
      - uses: ./.github/actions/pull-test-images
      - run: docker pull $REGISTRY@$NATS
YAML
  # The second shape IMAGE_FROM covers in the real tree, so this fixture holds its entries to a
  # command each, as the check demands. It is outside ci.yaml: it lives in a called file of a
  # job, where ci.yaml's job of that id holds the gate and the file the steps.
  cat > "$root/.github/workflows/ci-job-envoy-smoke.yaml" <<YAML
name: CI job envoy-smoke
on: [workflow_call]
jobs:
  envoy-smoke:
    runs-on: ubuntu-24.04
    steps:
      - run: docker run --rm "\$ENVOY_SMOKE_IMAGE" ls
YAML
  cat > "$root/.github/actions/pull-test-images/action.yml" <<'YAML'
name: Pull the images tests start
description: Pull the images the tests start.
runs:
  using: composite
  steps:
    - shell: bash
      run: |
        image=$(sed -n "s/^const Image = \"\(.*\)\"$/\1/p" "$file")
        docker pull "$image"
YAML
  # The Dockerfile a `built:` row names. Every fixture holds it, because CI's reach names it
  # and the check fails on a reach file that is gone; the row itself is the business of the
  # cases that call browser_row below.
  cat > "$root/.github/docker/browser.Dockerfile" <<YAML
FROM $REGISTRY@$NATS
RUN echo the browser host image
YAML
  # The workflow that builds that Dockerfile. Every fixture holds it too: its own text is half
  # of the build recipe rule 10 holds a `built:` row's published image to, so a tree without it
  # is a tree that rule cannot hash a row against.
  cat > "$root/.github/workflows/browser-image.yaml" <<'YAML'
name: Browser Image
on: [workflow_dispatch]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - run: echo the build no fixture runs
YAML
  cat > "$root/packages/envoy/docker/Dockerfile" <<YAML
FROM $REGISTRY@$GOLANG AS build
RUN go build ./...
FROM build
YAML
  cat > "$root/packages/envoy/infra/host/heldportwrapper/Dockerfile" <<'YAML'
ARG BASE
FROM ${BASE} AS base
YAML
  cat > "$root/packages/daemon/docker/worker.Dockerfile" <<YAML
ARG GO_VERSION=1.25.0
FROM $REGISTRY@$BUN_SLIM AS plugin
RUN bun install
YAML
  local go_file
  for go_file in packages/daemon/internal/testnats/nats.go \
    packages/envoy/internal/testnats/nats.go packages/envoy/internal/testpg/testpg.go \
    packages/envoy/internal/tests3/seaweedfs.go; do
    cat > "$root/$go_file" <<YAML
package testimage

const Image = "$REGISTRY@$NATS"
YAML
  done
  cat > "$root/packages/envoy-client/src/__tests__/nats-server.ts" <<YAML
const NATS_IMAGE = "$REGISTRY@$NATS";
export { NATS_IMAGE };
YAML
  cat > "$root/scripts/e2e/lib/nats-stream.test.ts" <<YAML
docker("run", "--rm", "$REGISTRY@$NATS", "-js");
YAML
  cat > "$root/packages/dispatch/e2e/visual-run.sh" <<YAML
#!/bin/sh
docker run --rm --network host --cpus 2 --memory 4g $REGISTRY@$NATS \\
  node node_modules/@playwright/test/cli.js test --project=visual
YAML
  cat > "$root/.github/scripts/dispatch-browser-visual-suite.sh" <<YAML
#!/usr/bin/env bash
export DISPATCH_E2E_PG_IMAGE=$REGISTRY@$NATS
YAML
  cat > "$root/packages/pi-legion/scripts/skill-scenarios/rig.sh" <<YAML
#!/usr/bin/env bash
judge_image=$REGISTRY@$NATS
docker run --rm --init "\$judge_image" bash -c 'echo judged'
# The shape IMAGE_FROM covers in the real rig.sh: its NATS server's image, which
# scripts/e2e/lib/nats-image.sh sets.
docker run -d --name rig-nats "\$nats_image" -js >/dev/null
YAML
  cat > "$root/scripts/e2e/stage1-skeleton.sh" <<YAML
#!/usr/bin/env bash
docker run -d --name pg --cpus 2 --memory 2g \\
  -e POSTGRES_DB=legion $REGISTRY@$NATS >/dev/null
YAML
  cat > "$root/packages/envoy/deploy/scripts/compose-2.12.2_test.sh" <<YAML
# shellcheck shell=bash
compose_2_12_image=$REGISTRY@$NATS
container=\$(docker create "\$compose_2_12_image")
YAML
  echo "$root"
}

# run_check <mode> <root>: runs the script under test against that tree, in that mode, setting
# `out` to its combined output and `status` to its exit code. Not a subshell, so both survive the
# call. MISSING, which the caller may set, names the digests the stand-in package does not hold,
# and LABELS the sha256 the image at a digest carries in its source label.
run_check() {
  local mode=$1 root=$2
  set +e
  out=$(CI_IMAGES_MANIFEST_HEAD="$head_script" MISSING="${MISSING:-}" LABELS="${LABELS:-}" \
    "$root/.github/scripts/check-ci-images.sh" "$mode" 2>&1)
  status=$?
  set -e
}

# golang_next_row <root>: appends the row a Go bump's first commit lands, the next release
# beside the one ARG GO_VERSION pins.
golang_next_row() {
  cat >> "$1/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/golang:1.26.3-alpine
    tag: golang-1.26.3-alpine-5f607182
    digest: $GOLANG_NEXT
YAML
}

# browser_row <root>: appends the row for the image this repository builds rather than copies,
# the one .github/workflows/browser-image.yaml pushes. Its tag is the rule a built row's tag
# follows: the Dockerfile's name without the .Dockerfile suffix, then the first eight hex
# characters of the digest.
browser_row() {
  cat >> "$1/.github/ci-images.yaml" <<YAML
  - built: .github/docker/browser.Dockerfile
    tag: browser-60718293
    digest: $BROWSER
YAML
}

# browser_job <root> <digest>: appends a job that runs in a container at that digest, the shape
# every job that can launch WebKit or Firefox takes.
browser_job() {
  cat >> "$1/.github/workflows/ci.yaml" <<YAML
  browser:
    runs-on: ubuntu-24.04
    container:
      image: $REGISTRY@$2
      credentials: \${{ fromJSON('{"username":"anonymous"}') }}
    steps:
      - run: bun test
YAML
}

# recipe <root> [dockerfile]: the sha256 of that tree's build recipe — the Dockerfile's own
# path, every file of .github/docker and .github/workflows/browser-image.yaml — which is what
# the build stamps into the image it pushes and what rule 10 holds that label to. The rule is
# .github/scripts/ci_images.py's recipe_sha256, the one the check under test reads it with,
# because a second copy of the rule here would be a hash this test demands and no build ever
# writes; what the cases below prove is that the hash MOVES when any part of the recipe does,
# which is the whole of rule 10. The second argument is the row's Dockerfile,
# .github/docker/browser.Dockerfile where a case gives none: a case naming another file of
# that directory is what proves the hash tells two Dockerfiles of one context apart.
recipe() {
  (cd "$1" && PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts \
    DOCKERFILE="${2:-.github/docker/browser.Dockerfile}" python3 -c '
import os

from ci_images import recipe_sha256

print(recipe_sha256(os.environ["DOCKERFILE"], ".github/workflows/browser-image.yaml"))
')
}
