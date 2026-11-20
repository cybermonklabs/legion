#!/usr/bin/env bash
# Tests for the rows of .github/ci-images.yaml that `check-ci-images.sh` holds, the second of its
# three harnesses: rules 1 and 2 (a row the check can read, one row a digest and a tag, the tag
# its upstream and digest give it), a row this repository builds with rules 8 and 10 (the
# Dockerfile the build workflow builds, and the build recipe the published image's label
# carries), a row's kind, rule 3 (a row at each pinned version), rule 9 (the package holds every
# row) and the file itself.
#
# The fixtures, the stand-in for the package's reads and the helpers are
# check-ci-images-test-lib.sh's, whose header says how a case builds its tree, why no case
# reaches the network and why every case names its mode.
#
# Run from anywhere: .github/scripts/check-ci-images-rows.test.sh
# CI runs it in ci.yaml's test job, in its own step, "Test the CI image check's rows".
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

# shellcheck source=.github/scripts/check-ci-images-test-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-ci-images-test-lib.sh"

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

echo "case: rule 9 — a row the package does not hold"
root=$(fixture not-published)
MISSING=$GOLANG run_check package "$root"
check "a row ghcr.io answers 404 for fails" "$(is "$status" 1)"
check "names the row and the status" "$(contains "$out" 'is not in the package: a manifest HEAD answered 404')"
check "and says what copies it" "$(contains "$out" "ci.yaml's ci-images job copies a row")"

root=$(fixture published)
MISSING=$UNKNOWN run_check package "$root"
check "a 404 on a digest no row holds changes nothing" "$(is "$status" 0)"

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

summary "check-ci-images.sh (the rows of .github/ci-images.yaml)"
