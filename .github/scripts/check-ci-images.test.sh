#!/usr/bin/env bash
# Tests for `check-ci-images.sh`: one red case per rule it enforces, under the mode that owns
# that refusal, one per ground of its exception list, and the repository's own tree in each mode.
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
# answers for one. Every case here sets it to `head.sh` below, which answers 200 for every
# digest but the ones MISSING names, 404 for those, and the label LABELS pairs with a digest.
# So the two checks that read ghcr.io are driven by a case, and nothing is pulled.
#
# EVERY CASE NAMES ITS MODE. The check takes one mode: `tree` runs the checks that read the
# files of the tree, and `package` runs the two that read ghcr.io. Each case runs under the
# mode that owns its refusal, and one case holds each mode to its own half — the tree mode runs
# the stand-in for the network never, and the package mode passes a tree the tree mode refuses.
#
# Run from anywhere: .github/scripts/check-ci-images.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

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

# The spy the mode case drives the check with: it answers 200 for every digest and writes each
# one it was asked about to $SPY_LOG. The check reads ghcr.io through this seam alone, so a run
# that leaves the log empty made no network read.
spy_script="$work/spy.sh"
cat > "$spy_script" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$1" >> "${SPY_LOG:?}"
echo 200
SH
chmod +x "$spy_script"

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
    "$root/scripts/e2e/lib" "$root/packages/pi-legion/scripts/skill-scenarios"
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
    steps:
      - uses: ./.github/actions/pull-test-images
      - run: docker pull $REGISTRY@$NATS
      # The two shapes IMAGE_FROM covers in the real tree, so this fixture holds its entries
      # to a command each, as the check demands.
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
  cat > "$root/packages/pi-legion/scripts/skill-scenarios/rig.sh" <<YAML
#!/usr/bin/env bash
judge_image=$REGISTRY@$NATS
docker run --rm --init "\$judge_image" bash -c 'echo judged'
YAML
  cat > "$root/scripts/e2e/stage1-skeleton.sh" <<YAML
#!/usr/bin/env bash
docker run -d --name pg --cpus 2 --memory 2g \\
  -e POSTGRES_DB=legion $REGISTRY@$NATS >/dev/null
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

echo "case: a tree whose every image is a row of .github/ci-images.yaml"
root=$(fixture green)
run_check tree "$root"
check "the tree mode exits 0" "$(is "$status" 0)"
check "says how many rows it read" "$(contains "$out" 'holds 4 row(s);')"
check "counts the reach" "$(contains "$out" "reference(s) in .* file(s) of CI's reach")"
run_check package "$root"
check "the package mode exits 0" "$(is "$status" 0)"
check "says what the package holds" "$(contains "$out" 'holds 4 row(s), 4 of them in the package')"

echo "case: each mode runs its own half"
# The tree mode's refusals read files, and the package mode's one refusal reads ghcr.io. This
# tree carries a refusal of the tree half (a Go string on another registry, rule 4) and a package
# that holds every row, so each mode answers for its half alone.
root=$(fixture mode-halves)
cat > "$root/packages/envoy/internal/testpg/testpg.go" <<'YAML'
package testpg

const Image = "docker.io/pgvector/pgvector:pg17"
YAML
spy_log="$work/spy.log"
: > "$spy_log"
set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$spy_script" SPY_LOG="$spy_log" \
  "$root/.github/scripts/check-ci-images.sh" tree 2>&1)
status=$?
set -e
check "the tree mode refuses the reference" "$(is "$status" 1)"
check "naming the registry it found" "$(contains "$out" 'this string names docker.io/pgvector/pgvector')"
check "and reads the package for no row" "$(is "$(wc -l < "$spy_log")" 0)"

set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$spy_script" SPY_LOG="$spy_log" \
  "$root/.github/scripts/check-ci-images.sh" package 2>&1)
status=$?
set -e
check "the package mode passes the same tree" "$(is "$status" 0)"
check "naming that reference nowhere" "$(is "$(contains "$out" 'docker.io/pgvector')" false)"
check "and reads the package once per row" "$(is "$(wc -l < "$spy_log")" 4)"

echo "case: rule 1 — a row this check cannot read"
root=$(fixture row-no-tag)
sed -i '/tag: nats-2.10-0a1b2c3d/d' "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a row with no tag fails" "$(is "$status" 1)"
check "says why a tag is needed" "$(contains "$out" 'a package version nothing names')"
check "names the row" "$(contains "$out" 'the row for docker.io/library/nats:2.10 has no tag')"

root=$(fixture row-empty-tag)
sed -i 's/tag: nats-2.10-0a1b2c3d/tag: ""/' "$root/.github/ci-images.yaml"
run_check tree "$root"
check "an empty tag fails too" "$(is "$status" 1)"
check "names the row" "$(contains "$out" 'has no tag')"

root=$(fixture row-bad-digest)
sed -i "s|digest: $NATS|digest: sha256:notadigest|" "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a digest that is not 64 hex characters fails" "$(is "$status" 1)"
check "says what to run" "$(contains "$out" 'crane digest docker.io/library/nats:2.10')"

echo "case: rule 2 — two rows naming one digest, or one tag"
root=$(fixture duplicate-digest)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/nats:2.11
    tag: nats-2.11-0a1b2c3d
    digest: $NATS
YAML
run_check tree "$root"
check "a second row on one digest fails" "$(is "$status" 1)"
check "says why one digest is one row" "$(contains "$out" 'one digest names one row')"

root=$(fixture duplicate-tag)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/nats:2.11
    tag: nats-2.10-0a1b2c3d
    digest: $UNKNOWN
YAML
run_check tree "$root"
check "a second row on one tag fails" "$(is "$status" 1)"
check "says the copy would move the tag" "$(contains "$out" 'would move the tag off the first')"

echo "case: rule 2 — a tag that is not the one its row's upstream and digest give it"
# The whole string is read. The eight characters at its end are what makes a shared tag
# impossible: without them the nats row's tag is the tag every digest that image ever has can
# take, and a later copy moves it off the digest this tree's consumers pin.
root=$(fixture tag-without-digest)
sed -i 's/tag: nats-2.10-0a1b2c3d/tag: nats-2.10/' "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a tag without those characters fails" "$(is "$status" 1)"
check "names the tag it read" "$(contains "$out" 'tag nats-2.10 is not nats-2.10-0a1b2c3d')"
check "and the tag the row should carry" "$(contains "$out" 'Name this row nats-2.10-0a1b2c3d')"

root=$(fixture tag-suffix-alone)
sed -i 's/tag: nats-2.10-0a1b2c3d/tag: anything-0a1b2c3d/' "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a tag that ends with them and names another image fails" "$(is "$status" 1)"
check "naming the whole tag the rule gives it" \
  "$(contains "$out" 'tag anything-0a1b2c3d is not nats-2.10-0a1b2c3d')"

root=$(fixture tag-without-digest-untagged-upstream)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/ubuntu@$UNKNOWN
    tag: ubuntu
    digest: $UNKNOWN
YAML
run_check tree "$root"
check "a row whose upstream names no tag fails the same way" "$(is "$status" 1)"
check "and is named the digest alone" "$(contains "$out" 'Name this row ubuntu-9f8e7d6c')"

echo "case: a row this repository builds, and the job that runs in it"
# A built row carries the Dockerfile it is built from where a copied row carries its upstream.
# Nothing copies it — .github/workflows/browser-image.yaml pushes it — but the package holds it
# like any other row, so the package mode reads it like any other row.
root=$(fixture built-row)
browser_row "$root"
browser_job "$root" "$BROWSER"
run_check tree "$root"
check "the tree mode exits 0" "$(is "$status" 0)"
check "counts the built row with the rest" "$(contains "$out" 'holds 5 row(s);')"
LABELS="$BROWSER=$(recipe "$root")" run_check package "$root"
check "the package mode exits 0" "$(is "$status" 0)"
check "and reports it held like the others" \
  "$(contains "$out" 'holds 5 row(s), 5 of them in the package')"

echo "case: rule 8 — a built row naming a Dockerfile the build workflow does not build"
# .github/workflows/browser-image.yaml builds one Dockerfile. A row built from another file is
# a row nothing pushes, so no image of the package was pushed from it and rule 10 below refuses
# it over the network — the case after this one is that refusal — while the tree alone says the
# same, so the tree mode is where it is said on every change.
root=$(fixture built-other-dockerfile)
cp "$root/.github/docker/browser.Dockerfile" "$root/.github/docker/other.Dockerfile"
cat >> "$root/.github/ci-images.yaml" <<YAML
  - built: .github/docker/other.Dockerfile
    tag: other-60718293
    digest: $BROWSER
YAML
browser_job "$root" "$BROWSER"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "names the file the row gives and the one that workflow builds" \
  "$(contains "$out" 'this row is built from .github/docker/other.Dockerfile, and .github/workflows/browser-image.yaml builds .github/docker/browser.Dockerfile and no other file')"
check "and says nothing builds the row" "$(contains "$out" 'so nothing builds this row')"

echo "case: rule 10 — two Dockerfiles of one directory are two recipes"
# What rule 8 says rule 10 does, done. The two files share a context directory, so the files
# hashed are the same files and the workflow is the same workflow: the Dockerfile's own path is
# what tells the two recipes apart, folded into the hash by .github/scripts/ci_images.py's
# recipe_sha256. Without it this row would carry the very sha256 the published browser image's
# label holds, and this mode would pass a row no build ever pushed an image from.
root=$(fixture built-two-dockerfiles)
cp "$root/.github/docker/browser.Dockerfile" "$root/.github/docker/other.Dockerfile"
cat >> "$root/.github/ci-images.yaml" <<YAML
  - built: .github/docker/other.Dockerfile
    tag: other-60718293
    digest: $BROWSER
YAML
browser_job "$root" "$BROWSER"
published=$(recipe "$root")
other=$(recipe "$root" .github/docker/other.Dockerfile)
check "the two Dockerfiles of one directory hash to two recipes" \
  "$(is "$(is "$published" "$other")" false)"
LABELS="$BROWSER=$published" run_check package "$root"
check "fails" "$(is "$status" 1)"
check "naming the recipe the row's own Dockerfile gives" \
  "$(contains "$out" "hash to sha256 $other as this tree holds them")"
check "and the sha256 the published image carries" \
  "$(contains "$out" "the image at that digest carries sha256 $published in its legion.browser-image.source-sha256 label")"

echo "case: rule 10 — a built row whose image was built from the recipe this tree holds"
# The build writes the recipe's own sha256 into the image it pushes — every file of the build
# context directory and the workflow that builds it — as the label this mode reads back out of
# the image the row's digest names. Here the two agree, which is what a merged bump leaves
# behind: the build pushed the edited recipe's image, and a later push moved the row onto that
# digest.
root=$(fixture built-label-matches)
browser_row "$root"
browser_job "$root" "$BROWSER"
published=$(recipe "$root")
LABELS="$BROWSER=$published" run_check package "$root"
check "the package mode exits 0" "$(is "$status" 0)"
check "and counts the built row it compared" "$(contains "$out" '1 built row(s) at the build recipe this tree holds')"

echo "case: rule 10 — a built row whose image was built from another Dockerfile"
# The hole this rule closes: a one-line edit to the Dockerfile that no build pushed. The row
# still names a digest the package holds, so rule 9 passes and the tag rule passes, and every
# job pinning the row would keep running the image built before the edit.
root=$(fixture built-label-stale)
browser_row "$root"
browser_job "$root" "$BROWSER"
# The label the build wrote when it pushed: the recipe's hash before the edit below.
published=$(recipe "$root")
echo 'RUN echo a layer the published image does not carry' \
  >> "$root/.github/docker/browser.Dockerfile"
edited=$(recipe "$root")
check "the edit moved the recipe's hash" "$(is "$(is "$published" "$edited")" false)"
LABELS="$BROWSER=$published" run_check package "$root"
check "fails" "$(is "$status" 1)"
check "names the recipe and the sha256 this tree holds" \
  "$(contains "$out" "is not the image this tree's build recipe makes: the name .github/docker/browser.Dockerfile, every file of .github/docker and .github/workflows/browser-image.yaml hash to sha256 $edited")"
check "and the sha256 the published image carries" \
  "$(contains "$out" "the image at that digest carries sha256 $published in its legion.browser-image.source-sha256 label")"
check "and the remedy" \
  "$(contains "$out" "move this row's digest onto what it pushed")"
# The tree mode reads the same edited Dockerfile and passes: the label is on the published
# image, so no read of this tree can tell the two apart, which is why rule 10 is the package
# mode's.
run_check tree "$root"
check "and the tree mode passes the same tree" "$(is "$status" 0)"

echo "case: rule 10 — a built row whose image was built by another build workflow"
# The half a hash of the Dockerfile alone left uncovered, and the reason this rule hashes the
# recipe: the workflow's own `labels:`, `outputs:`, `platforms:`, `provenance:`, build-args and
# action pin decide the bytes that get pushed, so a change to the build step with the
# Dockerfile untouched is a change the published image does not carry. ci.yaml's
# `browser_image` filter selects the build for it, and before this the build read the one
# file's hash, found it unchanged and pushed nothing.
root=$(fixture built-label-workflow-moved)
browser_row "$root"
browser_job "$root" "$BROWSER"
published=$(recipe "$root")
echo '      - run: echo a build input the published image was not built with' \
  >> "$root/.github/workflows/browser-image.yaml"
edited=$(recipe "$root")
check "the workflow edit moved the recipe's hash" "$(is "$(is "$published" "$edited")" false)"
LABELS="$BROWSER=$published" run_check package "$root"
check "fails" "$(is "$status" 1)"
check "names the recipe and the sha256 this tree holds" \
  "$(contains "$out" "is not the image this tree's build recipe makes: the name .github/docker/browser.Dockerfile, every file of .github/docker and .github/workflows/browser-image.yaml hash to sha256 $edited")"
check "and says a file of the recipe changed with no build behind it" \
  "$(contains "$out" "the two differ when a file of the recipe changed and no build pushed the change")"

echo "case: rule 10 — a built row whose context directory grew a file"
# The other half: the build context is the directory, not the Dockerfile, so a file added
# beside it is a file the next `COPY` can take into the image. Nothing in this tree reads it
# yet, and the hash moves all the same, because what the build receives has changed.
root=$(fixture built-label-context-grew)
browser_row "$root"
browser_job "$root" "$BROWSER"
published=$(recipe "$root")
echo 'the entrypoint a later COPY takes' > "$root/.github/docker/entrypoint.sh"
check "the new context file moved the recipe's hash" \
  "$(is "$(is "$published" "$(recipe "$root")")" false)"
LABELS="$BROWSER=$published" run_check package "$root"
check "fails" "$(is "$status" 1)"
check "naming the recipe this tree holds" \
  "$(contains "$out" "is not the image this tree's build recipe makes: the name .github/docker/browser.Dockerfile, every file of .github/docker and .github/workflows/browser-image.yaml hash to sha256 $(recipe "$root")")"

echo "case: rule 10 — a tree holding no build workflow"
# Half the recipe is that workflow's own text, so a tree without it is a tree this rule cannot
# hash a row against. It says so, rather than leaving a traceback where a refusal belongs.
root=$(fixture built-workflow-missing)
browser_row "$root"
browser_job "$root" "$BROWSER"
published=$(recipe "$root")
rm "$root/.github/workflows/browser-image.yaml"
LABELS="$BROWSER=$published" run_check package "$root"
check "fails" "$(is "$status" 1)"
check "names the workflow that is gone" \
  "$(contains "$out" "this tree holds no .github/workflows/browser-image.yaml, and that workflow's own text is half of the build recipe")"
check "without a traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: rule 10 — a stand-in that answers for the manifest and not for the label"
# CI_IMAGES_MANIFEST_HEAD stands in for the whole of the package read, so a stand-in that
# prints a status and no label has answered half of it. Rule 10 refuses the row rather than
# passing it over: a rule a seam can turn off, with nothing but a count to say it went quiet,
# is a rule nothing holds. LABELS names no digest here, which is that stand-in.
root=$(fixture built-label-unanswered)
browser_row "$root"
browser_job "$root" "$BROWSER"
run_check package "$root"
check "fails" "$(is "$status" 1)"
check "names the stand-in and what it left out" \
  "$(contains "$out" "the CI_IMAGES_MANIFEST_HEAD stand-in .* printed a status and no legion.browser-image.source-sha256 label after it")"
check "and says how to answer for the label" \
  "$(contains "$out" "Have it print that label after the status, or unset CI_IMAGES_MANIFEST_HEAD")"
check "and holds the row to the recipe this tree holds" \
  "$(contains "$out" "is not the image this tree's build recipe makes: the name .github/docker/browser.Dockerfile, every file of .github/docker and .github/workflows/browser-image.yaml hash to sha256 $(recipe "$root")")"

echo "case: a row carrying both upstream: and built:"
root=$(fixture row-both-kinds)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/ubuntu@$UNKNOWN
    built: .github/docker/browser.Dockerfile
    tag: browser-60718293
    digest: $BROWSER
YAML
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "says a row is one kind or the other" \
  "$(contains "$out" 'this row carries both .upstream:. and .built:., and a row is one or the other')"
check "and why both cannot stand" \
  "$(contains "$out" 'two different things about where its bytes came from')"

echo "case: a row carrying neither upstream: nor built:"
root=$(fixture row-neither-kind)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - tag: browser-60718293
    digest: $BROWSER
YAML
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "names both keys" "$(contains "$out" 'this row carries neither .upstream:. nor .built:.')"
check "and says what each of them is" \
  "$(contains "$out" 'the Dockerfile this repository builds the image from')"

echo "case: a built row whose tag is not its Dockerfile's name and the digest"
root=$(fixture built-tag-wrong)
browser_row "$root"
sed -i 's/tag: browser-60718293/tag: browser-image/' "$root/.github/ci-images.yaml"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "names the tag the rule gives it" \
  "$(contains "$out" "tag browser-image is not browser-60718293, the tag this row's Dockerfile and digest give it")"
check "and what to name the row" "$(contains "$out" 'Name this row browser-60718293')"

echo "case: a built row whose Dockerfile this tree does not hold"
root=$(fixture built-file-missing)
browser_row "$root"
sed -i 's|built: .github/docker/browser.Dockerfile|built: .github/docker/gone.Dockerfile|' \
  "$root/.github/ci-images.yaml"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "names the path it looked for" \
  "$(contains "$out" 'this row is built from .github/docker/gone.Dockerfile')"
check "and says nothing builds the row" "$(contains "$out" 'so nothing builds the row')"

echo "case: rule 3 — a version pinned elsewhere that no row carries"
root=$(fixture bun-drift)
sed -i 's|upstream: docker.io/oven/bun:1.3.14|upstream: docker.io/oven/bun:1.4.2|' \
  "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a tree whose every bun row moved off the pin fails" "$(is "$status" 1)"
check "names the pin" "$(contains "$out" '.bun-version locks 1.3.14')"
check "and the row it wants" "$(contains "$out" 'no row copies oven/bun:1.3.14')"
check "and the order of a bump" "$(contains "$out" 'Raising Bun is two commits')"

root=$(fixture golang-drift)
sed -i 's|ARG GO_VERSION=1.25.0|ARG GO_VERSION=1.26.3|' \
  "$root/packages/daemon/docker/worker.Dockerfile"
run_check tree "$root"
check "a tree whose golang row is not the pinned release fails" "$(is "$status" 1)"
check "names the pin" "$(contains "$out" 'pins ARG GO_VERSION=1.26.3')"
check "and what to write" "$(contains "$out" 'golang:1.26.3-alpine')"

# The first commit of a bump: the next version's row lands, and is copied, while the pin and
# every FROM still name the version before it. An extra row is admitted for that reason.
root=$(fixture next-version-rows)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/oven/bun:1.4.2-slim
    tag: bun-1.4.2-slim-4e5f6071
    digest: sha256:4e5f60718293a4b54e5f60718293a4b54e5f60718293a4b54e5f60718293a4b5
YAML
golang_next_row "$root"
run_check tree "$root"
check "a row for the next Bun and Go, beside the pinned rows, passes" "$(is "$status" 0)"

echo "case: rule 4 — a reference on another registry"
root=$(fixture other-registry)
cat > "$root/packages/envoy/internal/testpg/testpg.go" <<YAML
package testpg

const Image = "docker.io/pgvector/pgvector@$NATS"
YAML
run_check tree "$root"
check "a docker.io reference fails" "$(is "$status" 1)"
check "names the registry it found" "$(contains "$out" 'this string names docker.io/pgvector/pgvector')"
check "and the one package" "$(contains "$out" "a copy in $REGISTRY")"

echo "case: rule 5 — a reference that is not pinned by digest"
root=$(fixture tag-not-digest)
sed -i "s|image: $REGISTRY@$NATS|image: $REGISTRY:nats-2.10-0a1b2c3d|" "$root/.github/workflows/ci.yaml"
run_check tree "$root"
check "a tagged reference fails" "$(is "$status" 1)"
check "says a tag can move" "$(contains "$out" 'a name the registry can move')"
check "names the service and its line" "$(contains "$out" 'ci.yaml:8: the nats service')"

echo "case: rule 6 — a reference whose digest no row holds"
root=$(fixture digest-unknown)
sed -i "s|$NATS|$UNKNOWN|" "$root/packages/envoy-client/src/__tests__/nats-server.ts"
run_check tree "$root"
check "a digest outside the file fails" "$(is "$status" 1)"
check "names the file to add it to" "$(contains "$out" 'names a digest no row of .github/ci-images.yaml holds')"
check "and the line to edit" "$(contains "$out" 'nats-server.ts:1:')"

# A job's container is read from the parsed YAML, like a service's image, and rule 6 reads its
# digest against the rows whatever kind of row holds it.
root=$(fixture container-digest-unknown)
browser_job "$root" "$UNKNOWN"
run_check tree "$root"
check "a container on a digest outside the file fails" "$(is "$status" 1)"
check "names the container it read" \
  "$(contains "$out" "this job's container image names a digest no row of .github/ci-images.yaml holds")"

echo "case: rule 7 — a reference on a golang row at another version than ARG GO_VERSION"
# A bump's first commit lands the next Go's row beside the pinned one, and rule 3 admits it. A
# FROM moved onto that row before the ARG would build its stage with a Go the worker's toolchain
# stage never downloads, so every reference is read against the ARG.
root=$(fixture golang-from-off-the-arg)
golang_next_row "$root"
sed -i "s|$GOLANG|$GOLANG_NEXT|" "$root/packages/envoy/docker/Dockerfile"
run_check tree "$root"
check "a FROM on the other golang row fails" "$(is "$status" 1)"
check "names the row and the Go it copies" \
  "$(contains "$out" 'the row for docker.io/library/golang:1.26.3-alpine on line .*, which copies Go 1.26.3-alpine')"
check "and the ARG it disagrees with" "$(contains "$out" 'pins ARG GO_VERSION=1.25.0')"
check "and the line of the FROM" "$(contains "$out" 'packages/envoy/docker/Dockerfile:1:')"

# The second commit of a bump: the ARG and every golang FROM move to the new row together.
root=$(fixture golang-from-moved-with-the-arg)
golang_next_row "$root"
sed -i "s|$GOLANG|$GOLANG_NEXT|" "$root/packages/envoy/docker/Dockerfile"
sed -i 's|ARG GO_VERSION=1.25.0|ARG GO_VERSION=1.26.3|' \
  "$root/packages/daemon/docker/worker.Dockerfile"
run_check tree "$root"
check "the ARG and the FROM moved together passes" "$(is "$status" 0)"

# The same rows, with Docker Hub's implicit `library/` left out of the golang upstreams:
# docker.io/golang:1.25.0-alpine and docker.io/library/golang:1.25.0-alpine are one image. Rule
# 3 and rule 7 read a repository of one path element under `library/`, so both rules read the
# two spellings alike. Reading the row's text instead would lose the pinned row to rule 3 and
# hide the FROM below from rule 7.
root=$(fixture golang-short-spelling)
sed -i 's|upstream: docker.io/library/golang:|upstream: docker.io/golang:|' \
  "$root/.github/ci-images.yaml"
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/golang:1.26.3-alpine
    tag: golang-1.26.3-alpine-5f607182
    digest: $GOLANG_NEXT
YAML
sed -i "s|$GOLANG|$GOLANG_NEXT|" "$root/packages/envoy/docker/Dockerfile"
run_check tree "$root"
check "a FROM on the short-spelled other golang row fails" "$(is "$status" 1)"
check "names that row and the Go it copies" \
  "$(contains "$out" 'the row for docker.io/golang:1.26.3-alpine on line .*, which copies Go 1.26.3-alpine')"
check "and rule 3 reads the pinned row under its short spelling" \
  "$(is "$(contains "$out" 'no row copies library/golang')" false)"

echo "case: rule 9 — a row the package does not hold"
root=$(fixture not-published)
MISSING=$GOLANG run_check package "$root"
check "a row ghcr.io answers 404 for fails" "$(is "$status" 1)"
check "names the row and the status" "$(contains "$out" 'is not in the package: a manifest HEAD answered 404')"
check "and says what copies it" "$(contains "$out" "ci.yaml's ci-images job copies a row")"

root=$(fixture published)
MISSING=$UNKNOWN run_check package "$root"
check "a 404 on a digest no row holds changes nothing" "$(is "$status" 0)"

echo "case: a docker command whose image this check cannot read"
root=$(fixture opaque-docker)
cat > "$root/scripts/e2e/stage1-skeleton.sh" <<'YAML'
#!/usr/bin/env bash
docker run -d --name pg "$PG_IMAGE" >/dev/null
YAML
run_check tree "$root"
check "a variable from nowhere fails" "$(is "$status" 1)"
check "says what to do with it" "$(contains "$out" 'add the variable that holds it to IMAGE_FROM')"

echo "case: an IMAGE_FROM entry no docker command uses"
root=$(fixture image-from-stale)
rm -r "$root/.github/actions/pull-test-images"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "names the variable" "$(contains "$out" 'IMAGE_FROM names \$image here')"
check "says what to delete" "$(contains "$out" 'Delete the entry')"

echo "case: a FROM that is neither a stage nor an ARG"
root=$(fixture from-undeclared)
cat > "$root/packages/envoy/docker/Dockerfile" <<'YAML'
FROM ${NOT_AN_ARG} AS build
YAML
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "names the ARG it wanted" "$(contains "$out" 'does not declare as an ARG')"

echo "case: a FROM on an ARG whose default is a third-party image"
# A Dockerfile may name its base image through an ARG. Docker builds FROM that ARG's default
# value when no --build-arg overrides it, so the default is the image the build fetches and is a
# reference like any other. This is the shape the FROM lines had before this change
# (`ARG BUN_VERSION=1.4.4` with `FROM oven/bun:${BUN_VERSION}`), so it is the shape a return to a
# third-party registry takes.
root=$(fixture from-arg-default)
cat > "$root/packages/envoy/docker/Dockerfile" <<'YAML'
ARG BASE_IMAGE=docker.io/library/debian:trixie-slim
FROM ${BASE_IMAGE} AS build
YAML
run_check tree "$root"
check "a third-party default fails" "$(is "$status" 1)"
check "names the image the build fetches" "$(contains "$out" 'docker.io/library/debian:trixie-slim')"

echo "case: a file of the \`no CI job runs it\` ground, moved into CI's reach"
# scripts/e2e/stage3-devbox-workflow.sh may name mirror.gcr.io, because no CI job runs it. The
# same line in stage1-skeleton.sh, which the test job runs, is refused.
root=$(fixture mirror-in-reach-script)
cat > "$root/scripts/e2e/stage1-skeleton.sh" <<'YAML'
#!/usr/bin/env bash
docker run -d --name pg mirror.gcr.io/library/postgres:16 >/dev/null
YAML
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "says a CI job runs it" "$(contains "$out" 'a CI job runs this file and it names mirror.gcr.io')"
check "and names the line" "$(contains "$out" 'stage1-skeleton.sh:2:')"

echo "case: a file of the \`it fetches nothing\` ground, moved into CI's reach"
# packages/envoy/scripts/dev-postgres.test.sh may name mirror.gcr.io, because the job that runs
# it installs a fake docker on PATH. The same shape in a workflow of the reach is refused: there
# the docker is the runner's.
root=$(fixture mirror-in-reach-workflow)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - run: docker run --rm mirror.gcr.io/library/postgres:16 pg_isready
YAML
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "says a CI job runs it" "$(contains "$out" 'a CI job runs this file and it names mirror.gcr.io')"

echo "case: a new file naming mirror.gcr.io that is in neither list"
root=$(fixture mirror-new-file)
mkdir -p "$root/docs"
printf 'Start it with mirror.gcr.io/library/redis:7.\n' > "$root/docs/caching.md"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "names the file and the line" "$(contains "$out" 'docs/caching.md:1:')"
check "says it is in neither list" "$(contains "$out" "in neither CI's reach nor the exception list")"
check "and names both grounds" "$(contains "$out" 'no CI job runs it')"

echo "case: an exception the tree outgrew"
# scripts/e2e/README.md is in the exception list. A tree holding it without mirror.gcr.io in it
# is a ground that holds nothing any more.
root=$(fixture exception-stale)
printf 'The live proofs.\n' > "$root/scripts/e2e/README.md"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "names the file" "$(contains "$out" 'scripts/e2e/README.md:1:')"
check "says what to delete" "$(contains "$out" 'Delete its line from EXCEPTIONS')"

echo "case: a file of CI's reach that is gone"
root=$(fixture reach-missing)
rm "$root/packages/envoy/internal/tests3/seaweedfs.go"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "says the list names it" "$(contains "$out" "CI's reach names this file, and it is gone")"

echo "case: a workflow spelled .yml, which neither glob reads"
root=$(fixture workflow-yml)
cp "$root/.github/workflows/ci.yaml" "$root/.github/workflows/extra.yml"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "says to rename it" "$(contains "$out" 'Rename it, or widen the two globs')"

echo "case: .github/ci-images.yaml itself"
root=$(fixture file-missing)
rm "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a missing file fails" "$(is "$status" 1)"
check "says what it is for" "$(contains "$out" 'the one place that says which image a digest is')"

root=$(fixture registry-moved)
sed -i "s|^registry: .*|registry: ghcr.io/cybermonklabs/legion/other|" "$root/.github/ci-images.yaml"
run_check tree "$root"
check "another registry in the file itself fails" "$(is "$status" 1)"
check "names the one every consumer reads" "$(contains "$out" "every consumer names $REGISTRY")"

root=$(fixture file-unparsable)
printf 'images: [\n' > "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a file that is not YAML fails" "$(is "$status" 1)"
check "names the file, without a traceback" "$(contains "$out" 'is not valid YAML')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a call that names no mode, and one that names both"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "no mode fails" "$(is "$status" 1)"
check "naming the tree mode" "$(contains "$out" 'this check takes one mode: .tree. reads the files of this tree')"
check "and the package mode" "$(contains "$out" '.package. reads ghcr.io/cybermonklabs/legion/ci')"

set +e
out=$("$check_script" tree package 2>&1)
status=$?
set -e
check "both modes in one call fails" "$(is "$status" 1)"
check "naming them" "$(contains "$out" 'this check takes one mode')"

echo "case: the repository's own tree, in each mode"
set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$head_script" "$check_script" tree 2>&1)
status=$?
set -e
check "the real tree passes the tree mode" "$(is "$status" 0)"
check "says what it read" "$(contains "$out" "reference(s) in .* file(s) of CI's reach")"

# This tree's own `built` row carries the digest of an image this repository published, and
# rule 10 holds that image's label to the recipe this tree holds. The row is read out of
# .github/ci-images.yaml rather than written here, so a row that moves takes this case with
# it, and the label the stand-in answers with is the hash the build stamps into what it
# pushes: the row passes here for the reason it passes in CI.
repository=$(cd "$script_dir/../.." && pwd)
built_digest=$(cd "$repository" && PYTHONDONTWRITEBYTECODE=1 python3 -c '
import yaml

rows = yaml.safe_load(open(".github/ci-images.yaml"))["images"]
print(next(row["digest"] for row in rows if "built" in row))
')
set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$head_script" LABELS="$built_digest=$(recipe "$repository")" \
  "$check_script" package 2>&1)
status=$?
set -e
check "the real tree passes the package mode, with every row held" "$(is "$status" 0)"
check "says so" "$(contains "$out" "holds .* row(s), .* of them in the package")"
check "and compared its built row" \
  "$(contains "$out" '1 built row(s) at the build recipe this tree holds')"

summary "check-ci-images.sh"
