#!/usr/bin/env bash
# Tests for the references in CI's reach that `check-ci-images.sh` holds, the third of its three
# harnesses: rules 4 to 7 (the one package, a pin by digest, a digest a row holds, the Go that
# ARG GO_VERSION pins), a docker command or a FROM whose image the check cannot read, the
# IMAGE_FROM list, the sweep for the registry CI fetched from before and its exception list, CI's
# reach itself, and a workflow spelled .yml.
#
# The fixtures, the stand-in for the package's reads and the helpers are
# check-ci-images-test-lib.sh's, whose header says how a case builds its tree, why no case
# reaches the network and why every case names its mode.
#
# Run from anywhere: .github/scripts/check-ci-images-references.test.sh
# CI runs it in ci.yaml's test job, in its own step, "Test the CI image check's references".
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

# shellcheck source=.github/scripts/check-ci-images-test-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-ci-images-test-lib.sh"

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

summary "check-ci-images.sh (the references in CI's reach)"
