#!/usr/bin/env bash
# Tests for `check-download-retry.sh`: every network fetch of this repository's CI retries at
# R1's floor, a curl whose own options would put its last attempt inside that floor is refused,
# an `apt-get update` fails rather than warns when an index read fails so that the retry has
# something to retry, a `mise x` carries no retry at all because it exits 0 when it cannot
# resolve the tool, a fetch left of a `|` in a step whose shell leaves pipefail off is refused
# because the pipeline's status is its last command's, a step's budget holds every pass of a
# loop around a retried call, one rule holds that retry, and the four exceptions R2 lists are
# the only sites left bare (dispatch://INFRA-1545, R2).
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs. Every fixture starts from the
# same baseline: the four exceptions, each at the file, job and step name the check's exception
# list holds, the loops its DECLARED_LOOPS list declares, each at the file, step and verb that
# list holds, and the three Dockerfiles it reads. Both lists fail the check when an entry
# matches nothing, which is why the baseline carries one site for each. A case that proves a
# refusal adds a workflow, an action or a Dockerfile of its own beside that baseline, so a
# refusal is the case's own and not a hole in the baseline, and the baseline's own case proves
# it passes.
#
# Run from anywhere: .github/scripts/check-download-retry.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-download-retry.sh"
work=$(mktemp -d "${TMPDIR:-/tmp}/check-download-retry.XXXXXX")
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

PIN=fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09
DIGEST=sha256:5498ba57b9471840be3d15b033a4eec554d1c02fa6c2cc0ca2d888637f6c6e2f

# The canonical inline retry every network RUN of the three Dockerfiles opens with. A fixture
# that means to pass writes this text; a fixture that means to be refused writes another.
read -r -d '' CANONICAL <<'TEXT' || true
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
TEXT

# fixture <name>: a tree holding the baseline below, with the script linked in. Echoes its root.
#
# The baseline is the four exceptions of R2, which every fixture carries for two reasons: the
# check refuses an exception that matches no fetch, so a tree without them fails for a reason no
# case is about; and a case that then adds one bad site proves that site alone.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" \
    "$root/.github/actions/pull-test-images" \
    "$root/packages/daemon/docker" "$root/packages/envoy/docker" \
    "$root/packages/envoy/infra/host/heldportwrapper"
  ln -s "$check_script" "$root/.github/scripts/check-download-retry.sh"
  ln -s "$script_dir/workflow_paths.py" "$root/.github/scripts/workflow_paths.py"
  for dockerfile in \
    "$root/packages/daemon/docker/worker.Dockerfile" \
    "$root/packages/envoy/docker/Dockerfile" \
    "$root/packages/envoy/infra/host/heldportwrapper/Dockerfile"; do
    printf 'FROM scratch\nCOPY . /src\n' > "$dockerfile"
  done

  # Exception (a), first site: the ghcr.io pull of a package this repository publishes, read from
  # a Go const and pulled in a loop.
  cat > "$root/.github/actions/pull-test-images/action.yml" <<'YAML'
name: Pull the images tests start
description: Pulls each image a job's tests start, from the package this repository publishes.
runs:
  using: composite
  steps:
    - name: Pull the images tests start
      shell: bash
      run: |
        for image in $IMAGES; do
          docker pull "$image"
        done
YAML

  # Exceptions (a) again and (b), and the step that runs exception (c)'s script: its apt phase
  # is .github/scripts/playwright-install.sh below, and the browser download beside it retries
  # like any other site.
  cat > "$root/.github/workflows/ci.yaml" <<YAML
name: CI
on: [push]
jobs:
  envoy-client:
    runs-on: ubuntu-24.04
    steps:
      - name: Pre-pull the NATS image the credential tests run
        run: docker pull ghcr.io/example/legion/ci@$DIGEST
  download-retry-images:
    runs-on: ubuntu-24.04
    steps:
      - name: Pull every base image the three builds read
        run: |
          for image in \$images; do docker pull "\$image"; done
  dispatch:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@$PIN
      - name: Install Playwright Chromium, WebKit and Firefox
        run: |
          .github/scripts/playwright-install.sh
          . .github/scripts/ci-retry.sh
          retry_fetch "Playwright's Chromium" 300 -- bunx playwright install chromium
YAML

  # Exception (c): the apt phase of that step, which is a script of its own since
  # cybermonklabs/legion#2891 — its attempts() loop and the apt-get calls the loop runs. The
  # update here carries no `-o APT::Update::Error-Mode=any`, so a fixture that passes is a
  # fixture whose exemption took rule 5 along with rules 1 and 2.
  cat > "$root/.github/scripts/playwright-install.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
attempts() {
  local left=$1 what=$2 status
  shift 2
  while :; do
    "$@" && return 0
    status=$?
    left=$((left - 1))
    [ "$left" -gt 0 ] || return 1
  done
}
attempts 2 "apt-get update" sudo timeout 90 apt-get update
attempts 2 "the package download" sudo timeout 300 apt-get install -y -d $packages
sudo apt-get install -y $packages
SH
  chmod +x "$root/.github/scripts/playwright-install.sh"

  # Exception (d): a credentialed publish job, whose pinned command set no wrapper may change.
  cat > "$root/.github/workflows/release.yaml" <<'YAML'
name: Release
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install dependencies
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the workspace's dependencies" 60 -- bun install --frozen-lockfile
  publish:
    runs-on: ubuntu-24.04
    permissions:
      contents: write
    steps:
      - name: Determine latest
        run: gh api "repos/$GITHUB_REPOSITORY/releases/latest"
YAML

  # The loops DECLARED_LOOPS names, one call of each at the file, step and verb that list
  # holds. Every fixture carries them for the reason it carries the four exceptions: a
  # declaration matching no such loop fails the check, so a tree without them fails for a
  # reason no case is about. Each is the shape that list is for — a loop over the work the run
  # found, whose pass count the check cannot read off the loop — and each passes every other
  # rule, so a case that adds one bad site still proves that site alone. None carries a
  # `timeout-minutes`, so none of them is a budget a case has to reckon with.
  cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  cancel-started:
    runs-on: ubuntu-24.04
    steps:
      - name: Force-cancel the runs of the groups the queue removed
        run: |
          . .github/scripts/ci-retry.sh
          while read -r act id; do
            retry_fetch "the force-cancel of run $id" 30 -- \
              gh api --silent --method POST "repos/$GITHUB_REPOSITORY/actions/runs/$id/force-cancel"
          done <<<"$acts"
YAML
  cat > "$root/.github/workflows/dequeued.yaml" <<'YAML'
name: CI (dequeued pull request)
on: [pull_request]
jobs:
  cancel-dequeued:
    runs-on: ubuntu-24.04
    steps:
      - name: Cancel the removed group's ci.yaml run
        run: |
          . .github/scripts/ci-retry.sh
          while read -r id; do
            retry_fetch "the force-cancel of run $id" 30 -- \
              gh api --silent --method POST "repos/$GITHUB_REPOSITORY/actions/runs/$id/force-cancel"
          done <<<"$doomed"
YAML
  cat > "$root/.github/workflows/main-bypass-check.yaml" <<'YAML'
name: CI (main bypass check)
on: [push]
jobs:
  bypass-check:
    runs-on: ubuntu-24.04
    steps:
      - name: Start ci.yaml when this push's head was never tested
        run: |
          . .github/scripts/ci-retry.sh
          while read -r act sha; do
            retry_fetch "the ci.yaml dispatch" 30 -- \
              gh workflow run ci.yaml --repo "$GITHUB_REPOSITORY" --ref "$GITHUB_REF_NAME"
          done <<<"$tokens"
YAML
  cat > "$root/.github/scripts/check-pr-text.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
while :; do
  retry_fetch "page $page of the $route route" 30 -- \
    gh api "repos/$repository/$route?per_page=100&page=$page" > "$work/answer"
  [ "$(jq length "$work/answer")" = 100 ] || break
  page=$((page + 1))
done
SH
  cat > "$root/.github/scripts/ci-ok-reconcile.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
while read -r id status was marker; do
  retry_fetch "the PATCH of ci-ok check run $id" 30 -- \
    gh api --silent --method PATCH "repos/$GITHUB_REPOSITORY/check-runs/$id" --input "$body"
done <<<"$runs"
SH
  # The last of them is read through two functions: the call sits in image_go(), which
  # check_go() calls, and the loop runs check_go. A declaration covers it for the loop that
  # runs those calls, which is the loop the entry names.
  cat > "$root/.github/scripts/mirror-ci-images.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
image_go() {
  retry_fetch "the config of $1" 60 -- crane config --platform linux/amd64 "$1"
}
check_go() {
  local carried_go
  carried_go=$(image_go "$1")
  [ "$carried_go" = "$2" ] || exit 1
}
while IFS=$'\t' read -r registry tag digest source claimed_go; do
  check_go "$registry@$digest" "$claimed_go"
  retry_fetch "the copy of $source" 300 -- crane copy "$source" "$registry:$tag"
  have=$(retry_fetch "the digest of $registry:$tag" 60 -- crane digest "$registry:$tag")
done <<<"$rows"
SH
  chmod +x "$root/.github/scripts/check-pr-text.sh" "$root/.github/scripts/ci-ok-reconcile.sh" \
    "$root/.github/scripts/mirror-ci-images.sh"
  echo "$root"
}

run_check() {
  local root=$1
  shift
  set +e
  out=$(cd "$root" && "./.github/scripts/check-download-retry.sh" "$@" 2>&1)
  status=$?
  set -e
}

list_check() {
  local root=$1
  set +e
  out=$(cd "$root" && "./.github/scripts/check-download-retry.sh" --list 2>/dev/null)
  status=$?
  set -e
}

echo "case: a tree holding each of R2's four exceptions passes"
root=$(fixture exceptions)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it read" "$(contains "$out" 'every network fetch in CI retries at the floor')"
check "counts the sites the exceptions name" "$(contains "$out" '6 site(s) the exceptions name')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a workflow with a bare curl of a release asset"
root=$(fixture bare-curl)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install changie
        run: |
          curl -fsSL https://github.com/miniscruff/changie/releases/download/v1.21.1/changie.tar.gz \
            | tar -xz -C /usr/local/bin changie
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line and the step" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=8::.curl. in step .Install changie. fetches without the floor')"
check "names each option it lacks" "$(contains "$out" 'it passes none')"
check "names --retry-all-errors" "$(contains "$out" 'retry-all-errors')"
check "names --max-time" "$(contains "$out" 'max-time')"
check "counts the refusal" "$(contains "$out" '1 download(s) outside the one retry rule')"

echo "case: a workflow whose curl carries a retry below the floor"
root=$(fixture short-retry)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install crane
        run: |
          curl -fsSL --retry 3 --retry-all-errors --max-time 30 -o crane.tar.gz \
            https://github.com/google/go-containerregistry/releases/download/v0.21.1/crane.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the count it passes" "$(contains "$out" 'it passes .--retry 3.')"
check "names the floor" "$(contains "$out" 'only 7 reaches 127 s')"

echo "case: a curl that retries but bounds no attempt"
root=$(fixture unbounded-curl)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install uv
        run: curl -fsSL --retry 7 --retry-all-errors -o uv.tar.gz https://example.com/uv.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the missing bound" "$(contains "$out" 'which curl resets for each attempt')"

echo "case: a curl that retries and never fails on a persistent 504"
root=$(fixture no-fail)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install uv
        run: curl -sSL --retry 7 --retry-all-errors --max-time 30 -o uv.tar.gz https://example.com/uv.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names --fail and what it costs" "$(contains "$out" 'writes the error body to the output')"

echo "case: a curl that reads the status itself answers for --fail"
root=$(fixture status-read)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the release
        run: |
          code=$(curl -sSL --retry 7 --retry-all-errors --max-time 30 -o out.json -w '%{http_code}' https://example.com/r)
          [ "$code" = 200 ]
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# Two of curl's options rewrite the waits between its own attempts, and each can put the last
# attempt inside R1's floor of 120 s while every other test of that floor passes. The rule is
# curl's own, read at curl 8.14.1: the first wait is one second and doubles for each of the
# rest, `--retry-delay <n>` replaces that with a fixed n, and `--retry-max-time <n>` lets a
# retry start only while the timer reset at the first attempt has not reached n.
echo "case: a curl whose --retry-delay puts the last attempt seconds after the first"
root=$(fixture retry-delay)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install uv
        run: curl -fsSL --retry 7 --retry-all-errors --retry-delay 1 --max-time 60 -o uv.tar.gz https://example.com/uv.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the ladder it computed" \
  "$(contains "$out" 'its last one starts 7 s after the first, inside R1.s floor of 120 s')"
check "names the option that made it so" "$(contains "$out" '.--retry-delay 1., which replaces')"

echo "case: a curl whose --retry-max-time stops the ladder inside the floor"
root=$(fixture retry-max-time)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install uv
        run: curl -fsSL --retry 7 --retry-all-errors --retry-max-time 5 --max-time 60 -o uv.tar.gz https://example.com/uv.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the ladder the limit allows" \
  "$(contains "$out" 'its last one starts 7 s after the first')"
check "names the option that made it so" \
  "$(contains "$out" '.--retry-max-time 5., which stops the ladder')"

echo "case: a curl whose --retry-delay reaches the floor passes"
root=$(fixture retry-delay-wide)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install uv
        run: curl -fsSL --retry 7 --retry-all-errors --retry-delay 20 --max-time 60 -o uv.tar.gz https://example.com/uv.tar.gz
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a curl whose retry wait is not a number this check can read is refused"
root=$(fixture retry-delay-unreadable)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install uv
        run: curl -fsSL --retry 7 --retry-all-errors --retry-delay "$delay" --max-time 60 -o uv.tar.gz https://example.com/uv.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says it cannot read the wait" \
  "$(contains "$out" 'with a value this check cannot read')"

echo "case: a composite action holding a second copy of the retry helper"
root=$(fixture copied-helper)
mkdir -p "$root/.github/actions/install-thing"
cat > "$root/.github/actions/install-thing/action.yml" <<'YAML'
name: Install the thing
description: Installs the thing, with a retry loop of its own.
runs:
  using: composite
  steps:
    - name: Install the thing
      shell: bash
      run: |
        attempt=1
        while [ "$attempt" -le 5 ]; do
          curl -fsSL --retry 7 --retry-all-errors --max-time 30 -o thing.tar.gz https://example.com/thing && break
          sleep $((10 * attempt))
          attempt=$((attempt + 1))
        done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the loop as a copy" \
  "$(contains "$out" 'file=.github/actions/install-thing/action.yml,line=9::this loop retries .curl. itself, which makes it a second copy')"
check "names the one rule" "$(contains "$out" 'One rule holds the attempts')"

echo "case: a loop that hands a fetch to a retry function of its own is a copy too"
root=$(fixture copied-helper-indirect)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the packages
        run: |
          attempts() {
            local left=$1 what=$2 status
            shift 2
            while :; do
              "$@" && return 0
              status=$?
              left=$((left - 1))
              [ "$left" -gt 0 ] || return 1
            done
          }
          attempts 2 "apt-get update" sudo timeout 90 apt-get update
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the loop the call reaches" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=11::this loop retries .apt-get update. itself')"

echo "case: a loop whose fetch is a retry_fetch call is no copy"
root=$(fixture loop-with-helper)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Copy each row
        run: |
          . .github/scripts/ci-retry.sh
          while IFS= read -r row; do
            retry_fetch "the copy of $row" 300 -- crane copy "$row" "ghcr.io/example/ci:$row"
          done <<<"$rows"
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a retry_fetch call written without a per-attempt bound"
root=$(fixture unbounded-helper)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the workspace's dependencies
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the workspace's dependencies" -- bun install --frozen-lockfile
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the missing bound" \
  "$(contains "$out" 'is retried and no attempt of it is bounded in time. Give .retry_fetch. a per-attempt bound')"

echo "case: a Dockerfile with an unretried apt-get"
root=$(fixture bare-apt)
cat > "$root/packages/envoy/docker/Dockerfile" <<'DOCKER'
FROM debian:trixie-slim AS build
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
DOCKER
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the Dockerfile, the line and the verb" \
  "$(contains "$out" 'file=packages/envoy/docker/Dockerfile,line=2::.apt-get update. fetches inside a Dockerfile RUN with no retry')"
check "names the inline loop to use" "$(contains "$out" 'the canonical inline loop')"

echo "case: a Dockerfile RUN that opens with the canonical retry and wraps each fetch passes"
root=$(fixture canonical-retry)
{
  echo 'FROM debian:trixie-slim AS build'
  printf '%s\n' "$CANONICAL"
  cat <<'DOCKER'
    && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*
DOCKER
} > "$root/packages/envoy/docker/Dockerfile"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a Dockerfile's retried apt-get update that keeps apt's warning on a failed index"
root=$(fixture dockerfile-apt-error-mode)
{
  echo 'FROM debian:trixie-slim AS build'
  printf '%s\n' "$CANONICAL"
  cat <<'DOCKER'
    && retry 120 "apt-get update" apt-get update \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*
DOCKER
} > "$root/packages/envoy/docker/Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the Dockerfile, the line and the option" \
  "$(contains "$out" 'file=packages/envoy/docker/Dockerfile,line=5::.apt-get update. passes no .-o APT::Update::Error-Mode=any.')"
check "says what goes wrong, not only what is missing" \
  "$(contains "$out" 'the retry around it stops after one attempt having read no list')"
check "names the install the unread list breaks" \
  "$(contains "$out" 'has no installation candidate')"

echo "case: a Dockerfile RUN with a retry loop of its own text"
root=$(fixture other-retry)
cat > "$root/packages/envoy/docker/Dockerfile" <<'DOCKER'
FROM debian:trixie-slim AS build
RUN retry() { for i in 1 2 3; do "$@" && return 0; sleep 5; done; return 1; } \
    && retry apt-get update \
    && rm -rf /var/lib/apt/lists/*
DOCKER
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the canonical text as the one definition" \
  "$(contains "$out" 'this RUN defines a retry loop of its own text')"

echo "case: a Dockerfile RUN whose one fetch is a curl carrying curl's own options passes"
root=$(fixture dockerfile-curl)
cat > "$root/packages/daemon/docker/worker.Dockerfile" <<'DOCKER'
FROM debian:trixie-slim AS build
RUN set -eu; \
    curl -fsSL --retry 7 --retry-all-errors --max-time 120 -o /tmp/mise.tar.gz \
      "https://example.com/mise.tar.gz"; \
    tar -xzf /tmp/mise.tar.gz -C /tmp
DOCKER
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a workflow that wraps a mise x in a retry"
root=$(fixture retried-mise-x)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the worker image's jj
        run: |
          . .github/scripts/ci-retry.sh
          jj_path=$(retry_fetch "the worker image's jj" 60 -- mise x "$jj_tool" -- sh -c 'command -v jj')
          install -m 0755 "$jj_path" "$HOME/.local/bin/jj"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line, the step and the verb" \
  "$(contains "$out" "file=.github/workflows/case.yaml,line=9::.mise x. in step .Install the worker image.s jj. is retried, and that retry protects nothing")"
check "says what goes wrong, not only what to write instead" \
  "$(contains "$out" 'runs whatever binary of that name is already on PATH')"
check "names the cure" "$(contains "$out" 'The cure is .mise install <tool>. under the retry')"
check "names the resolution that follows it" \
  "$(contains "$out" 'with the resolution after it')"

echo "case: a Dockerfile RUN that wraps a mise x in the canonical retry"
root=$(fixture retried-mise-x-dockerfile)
{
  echo 'FROM debian:trixie-slim AS build'
  printf '%s\n' "$CANONICAL"
  cat <<'DOCKER'
    && install -m 0755 "$(retry 300 "jj download" mise x "$JJ_TOOL" -- sh -c 'command -v jj')" /opt/tools/jj
DOCKER
} > "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the Dockerfile, the line and the verb" \
  "$(contains "$out" 'file=packages/daemon/docker/worker.Dockerfile,line=5::.mise x. is retried, and that retry protects nothing')"
check "names the cure" "$(contains "$out" 'The cure is .mise install <tool>. under the retry')"
check "counts the one refusal" \
  "$(contains "$out" '1 download(s) outside the one retry rule')"

echo "case: a workflow's retried mise install with the resolution after it passes"
root=$(fixture mise-install-cured)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the worker image's jj
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the worker image's jj" 60 -- mise install "$jj_tool"
          jj_path=$(mise x "$jj_tool" -- sh -c 'command -v jj')
          install -m 0755 "$jj_path" "$HOME/.local/bin/jj"
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
list_check "$root"
check "the install is on the list" "$(contains "$out" 'case.yaml	9	mise install	')"
check "the resolution after it is on no list" \
  "$(is "$(contains "$out" 'mise x')" false)"

echo "case: a Dockerfile's retried mise install with the resolution after it passes"
root=$(fixture mise-install-cured-dockerfile)
{
  echo 'FROM debian:trixie-slim AS build'
  printf '%s\n' "$CANONICAL"
  cat <<'DOCKER'
    && retry 300 "jj download" mise install "$JJ_TOOL" \
    && install -m 0755 "$(mise x "$JJ_TOOL" -- sh -c 'command -v jj')" /opt/tools/jj
DOCKER
} > "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
list_check "$root"
check "the install is on the list" \
  "$(contains "$out" '^packages/daemon/docker/worker.Dockerfile	5	mise install	-$')"
check "the resolution after it is on no list" \
  "$(is "$(contains "$out" 'mise x')" false)"

echo "case: a readiness poll of the loopback is no download"
root=$(fixture loopback)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Wait for the S3-compatible server
        run: |
          for attempt in $(seq 1 60); do
            curl -sf -o /dev/null http://127.0.0.1:18333/ && break
            sleep 1
          done
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
list_check "$root"
check "the poll is on no list" "$(is "$(contains "$out" 'case.yaml')" false)"

echo "case: a probe whose failure is the answer is no download"
root=$(fixture probe)
cat > "$root/.github/scripts/mirror.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
have=$(crane digest "ghcr.io/example/ci:$tag" 2>/dev/null || true)
[ "$have" = "$digest" ] || echo copy
SH
chmod +x "$root/.github/scripts/mirror.sh"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: the same read with its failure a fault is a download"
root=$(fixture probe-not)
cat > "$root/.github/scripts/mirror.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
have=$(crane digest "ghcr.io/example/ci:$tag")
echo "$have"
SH
chmod +x "$root/.github/scripts/mirror.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the read" "$(contains "$out" 'file=.github/scripts/mirror.sh,line=3::.crane digest. fetches with no retry')"

echo "case: a Python read that goes straight to urllib"
root=$(fixture python-read)
cat > "$root/.github/scripts/read.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
python3 <<'PY'
import urllib.request

with urllib.request.urlopen("https://ghcr.io/token", timeout=30) as answer:
    print(answer.read())
PY
SH
chmod +x "$root/.github/scripts/read.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file and the one reader" \
  "$(contains "$out" 'file=.github/scripts/read.sh,line=6::this read goes straight to urllib.request')"
check "names the floor it carries" "$(contains "$out" 'waits of 10, 20, 40 and 80')"

echo "case: the same read inside another file's read_retrying is still refused"
root=$(fixture python-copy)
cat > "$root/.github/scripts/read.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
python3 <<'PY'
import urllib.request


def read_retrying(request):
    for wait in (10, 20, 40, 80, None):
        try:
            return urllib.request.urlopen(request, timeout=30)
        except Exception:
            if wait is None:
                raise
PY
SH
chmod +x "$root/.github/scripts/read.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the copy" "$(contains "$out" 'this read goes straight to urllib.request')"

echo "case: an apt unpack after a retried download takes no network"
root=$(fixture apt-unpack)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install this proof's packages
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "apt's package lists" 95 -- \
            sudo timeout 90 apt-get update -o APT::Update::Error-Mode=any
          retry_fetch "the proof's apt packages" 125 -- \
            sudo timeout 120 apt-get install -y --no-install-recommends -d $PACKAGES
          sudo apt-get install -y --no-install-recommends $PACKAGES
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a workflow's retried apt-get update that keeps apt's warning on a failed index"
root=$(fixture apt-error-mode)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the packages
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "apt's package lists" 95 -- sudo timeout 90 apt-get update
          retry_fetch "the packages" 125 -- \
            sudo timeout 120 apt-get install -y --no-install-recommends -d $PACKAGES
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line, the step and the option" \
  "$(contains "$out" "file=.github/workflows/case.yaml,line=9::.apt-get update. in step .Install the packages. passes no .-o APT::Update::Error-Mode=any.")"
check "says what goes wrong, not only what is missing" \
  "$(contains "$out" 'the retry around it stops after one attempt having read no list')"
check "the retried install beside it is no problem of this rule" \
  "$(contains "$out" '1 download(s) outside the one retry rule')"

echo "case: a command that only answers about itself is no fetch"
root=$(fixture versions)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the toolchain
        run: curl --version; wget --version; bunx biome check .
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: --list prints every site with its step name"
root=$(fixture listing)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Download the Go modules this job builds
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "packages/envoy's Go modules" 300 -- go -C packages/envoy mod download
YAML
cat > "$root/.github/scripts/report.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "the run's artifact list" 60 -- gh api "$route" > "$out"
SH
chmod +x "$root/.github/scripts/report.sh"
list_check "$root"
check "exits 0" "$(is "$status" 0)"
check "a workflow site carries its step name" \
  "$(contains "$out" '^.github/workflows/case.yaml	9	go mod download	Download the Go modules this job builds$')"
check "a script site carries a dash for the step" \
  "$(contains "$out" '^.github/scripts/report.sh	4	gh api	-$')"
check "the baseline's own retried site is on it" \
  "$(contains "$out" 'playwright install	Install Playwright Chromium, WebKit and Firefox$')"
check "an excepted site is not on it" "$(is "$(contains "$out" 'docker pull')" false)"
check "the count goes to standard error, not into the rows" \
  "$(is "$(contains "$out" 'site\(s\)')" false)"

echo "case: --list over a tree with a problem refuses instead of listing"
root=$(fixture listing-refused)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install changie
        run: curl -fsSL -o changie.tar.gz https://example.com/changie.tar.gz
YAML
run_check "$root" --list
check "exits 1" "$(is "$status" 1)"
check "names the problem" "$(contains "$out" 'fetches without the floor')"

echo "case: an exception that matches no fetch fails"
root=$(fixture stale-exception)
rm "$root/.github/actions/pull-test-images/action.yml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the exception and what it no longer covers" \
  "$(contains "$out" 'the exception for .github/actions/pull-test-images/action.yml matches no fetch')"

echo "case: one of the three Dockerfiles missing fails"
root=$(fixture missing-dockerfile)
rm "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'packages/daemon/docker/worker.Dockerfile is one of the three Dockerfiles')"

echo "case: a workflow that cannot be parsed as YAML"
root=$(fixture bad-yaml)
printf 'name: Case\non: [push\n' > "$root/.github/workflows/case.yaml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" "$(contains "$out" 'this file is not valid YAML')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

# A step's own budget must be larger than the attempts it allows, not equal to them. Measured in
# run 42386541065's download-retry job, where `Read the run's artifact list under the stalling
# fault` carries `timeout-minutes: 5` against its own arithmetic of 5 x 30 + 150 = 300 s: the
# fifth attempt ended at 300.03 s and the runner killed the step in the same tenth of a second
# (`the run's artifact list: attempt 5 of 5 reached its 30s bound` at 17:22:16.7444966Z, then
# `The action 'Read the run's artifact list under the stalling fault' has timed out after 5
# minutes.` at 17:22:16.7483230Z). The helper's own `::error::` line won that race by four
# milliseconds; a run whose attempts drift later loses it, and the job then reports a timeout
# rather than the download that failed, which is the diagnosis this change exists to give.
echo "case: a step whose own timeout-minutes is no larger than the attempts it allows"
root=$(fixture budget-no-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the run's artifact list
        timeout-minutes: 5
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the run's artifact list" 30 -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/1/artifacts"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step" "$(contains "$out" "Read the run's artifact list")"
check "names the budget" "$(contains "$out" 'timeout-minutes')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: the same step with a budget above its attempts passes"
root=$(fixture budget-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the run's artifact list
        timeout-minutes: 6
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the run's artifact list" 30 -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/1/artifacts"
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# A call inside a loop is one call per pass of that loop, and the step waits for all of them.
# dequeued.yaml's timeline read is the site these cases close: one `retry_fetch` call of
# 5 x 30 + 150 = 300 s written inside a seven-pass poll with a 10 s sleep between the passes,
# 7 x 300 + 6 x 10 = 2160 s, which the check read as 300 s before this change and passed under
# a budget of 16 minutes.
echo "case: a counted loop multiplies the call in it, and a budget written for one call fails"
root=$(fixture loop-counted)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Poll the pull request's timeline
        timeout-minutes: 6
        run: |
          . .github/scripts/ci-retry.sh
          for attempt in 1 2 3 4 5 6 7; do
            removal=$(retry_fetch "the timeline" 30 -- gh api graphql -f query="$query") && break
            sleep 10
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step" "$(contains "$out" "Poll the pull request's timeline")"
check "multiplies the call by the passes and adds the loop's own sleeps" \
  "$(contains "$out" 'retries take 2160 s to exhaust')"
check "names the budget that would hold it" "$(contains "$out" 'timeout-minutes: 37')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: the same counted loop under a budget above the multiplied figure passes"
root=$(fixture loop-counted-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Poll the pull request's timeline
        timeout-minutes: 37
        run: |
          . .github/scripts/ci-retry.sh
          for attempt in 1 2 3 4 5 6 7; do
            removal=$(retry_fetch "the timeline" 30 -- gh api graphql -f query="$query") && break
            sleep 10
          done
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a loop counted with seq multiplies the same way"
root=$(fixture loop-seq)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read each page of the route
        timeout-minutes: 15
        run: |
          . .github/scripts/ci-retry.sh
          for page in $(seq 1 3); do
            retry_fetch "page $page" 30 -- gh api "repos/$GITHUB_REPOSITORY/issues?page=$page"
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts the three passes and no sleep" "$(contains "$out" 'retries take 900 s to exhaust')"

echo "case: a step whose budget reaches a loop with no readable count is refused by name"
root=$(fixture loop-unreadable)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Cancel every run the decision named
        timeout-minutes: 30
        run: |
          . .github/scripts/ci-retry.sh
          while read -r id; do
            retry_fetch "the cancel of run $id" 30 -- \
              gh api --silent --method POST "repos/$GITHUB_REPOSITORY/actions/runs/$id/cancel"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and the call it cannot count" \
  "$(contains "$out" "step .Cancel every run the decision named. carries .timeout-minutes: 30. and this check can put no figure on it")"
check "names the loop's file and line" \
  "$(contains "$out" '.gh api. at .github/workflows/case.yaml:11, inside the loop at line 10')"
check "says what one call of it costs" "$(contains "$out" 'One call there takes 300 s')"
check "says where a count can be written or declared" "$(contains "$out" 'DECLARED_LOOPS')"
check "does not judge the budget against a figure it is missing" \
  "$(is "$(contains "$out" 'does not strictly exceed')" false)"

echo "case: the same loop in a step with no budget is nobody's arithmetic and passes"
root=$(fixture loop-unreadable-unbudgeted)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Cancel every run the decision named
        run: |
          . .github/scripts/ci-retry.sh
          while read -r id; do
            retry_fetch "the cancel of run $id" 30 -- \
              gh api --silent --method POST "repos/$GITHUB_REPOSITORY/actions/runs/$id/cancel"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# A loop whose passes are as many as the run finds work has its count declared in the check's
# own DECLARED_LOOPS, and the budget is held to that figure. The baseline's ci-ok-reconcile.sh
# is one of them, declared at three PATCHes: one call is 300 s, so three are 900 s and a
# 15-minute budget does not strictly exceed them.
echo "case: a declared loop is counted at the passes the declaration writes"
root=$(fixture loop-declared)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Set every other ci-ok on this head to this job's conclusion
        timeout-minutes: 15
        run: .github/scripts/ci-ok-reconcile.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts the three passes the declaration writes" \
  "$(contains "$out" 'retries take 900 s to exhaust')"
check "names the script the step runs" "$(contains "$out" 'ci-ok-reconcile.sh')"

echo "case: the same declared loop under a budget above its figure passes"
root=$(fixture loop-declared-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Set every other ci-ok on this head to this job's conclusion
        timeout-minutes: 16
        run: .github/scripts/ci-ok-reconcile.sh
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a declared loop that matches no loop in the tree fails"
root=$(fixture stale-declaration)
rm "$root/.github/scripts/mirror-ci-images.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the declaration and what it no longer covers" \
  "$(contains "$out" 'the declared loop for .github/scripts/mirror-ci-images.sh .crane copy. matches no retried call')"
check "says what the declaration was for" "$(contains "$out" 'written for 1 pass')"

# A step waits for every pass of every loop around a call, for every call of the function that
# holds it, and for every pass of the loops around those calls — and it waits for all of that
# again in each script it runs inside a loop of its own. Each shape below was a budget this
# check read at a fraction of what the step allows: a loop inside a loop counted once, a
# function called three times counted once, and a script in a three-pass loop counted once.
# The live instance was the required `pr-title` check: check-pr-text.sh makes five `gh api`
# ladders and this check counted three, because the third sits in read_list's page loop and
# the `for ((i = 0; i < ${#lists[@]}; i += 2))` under it calls read_list once for each of the
# three lists.
echo "case: a loop inside a loop multiplies the call by both"
root=$(fixture loop-nested)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read each page of each list
        timeout-minutes: 30
        run: |
          . .github/scripts/ci-retry.sh
          for list in comments reviews; do
            for page in 1 2 3; do
              retry_fetch "page $page of $list" 30 -- \
                gh api "repos/$GITHUB_REPOSITORY/issues/1/$list?page=$page"
            done
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts both loops and not the innermost alone" \
  "$(contains "$out" 'retries take 1800 s to exhaust')"
check "names the budget that would hold it" "$(contains "$out" 'timeout-minutes: 31')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: the same nest under a budget above the figure both loops make passes"
root=$(fixture loop-nested-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read each page of each list
        timeout-minutes: 31
        run: |
          . .github/scripts/ci-retry.sh
          for list in comments reviews; do
            for page in 1 2 3; do
              retry_fetch "page $page of $list" 30 -- \
                gh api "repos/$GITHUB_REPOSITORY/issues/1/$list?page=$page"
            done
          done
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a function holding a retried call is counted once for each call of it"
root=$(fixture function-called-thrice)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the three lists
        timeout-minutes: 15
        run: |
          . .github/scripts/ci-retry.sh
          read_list() {
            retry_fetch "the $1 list" 30 -- gh api "repos/$GITHUB_REPOSITORY/issues/1/$1"
          }
          read_list comments
          read_list reactions
          read_list events
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts the call once for each of the three" \
  "$(contains "$out" 'retries take 900 s to exhaust')"
check "names the budget that would hold it" "$(contains "$out" 'timeout-minutes: 16')"

echo "case: the same function under a budget above its three calls passes"
root=$(fixture function-called-thrice-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the three lists
        timeout-minutes: 16
        run: |
          . .github/scripts/ci-retry.sh
          read_list() {
            retry_fetch "the $1 list" 30 -- gh api "repos/$GITHUB_REPOSITORY/issues/1/$1"
          }
          read_list comments
          read_list reactions
          read_list events
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# The baseline's ci-ok-reconcile.sh is 3 x (5 x 30 + 150) = 900 s of its own, so a step that
# runs it three times round a loop waits 2700 s for it.
echo "case: a script the step runs inside its own loop is counted once for each pass"
root=$(fixture script-in-loop)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Reconcile the ci-oks of each head the queue left
        timeout-minutes: 45
        run: |
          for head in 1 2 3; do
            .github/scripts/ci-ok-reconcile.sh "$head"
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts the script once for each pass of the step's loop" \
  "$(contains "$out" 'retries take 2700 s to exhaust')"
check "names the script the step runs" "$(contains "$out" 'ci-ok-reconcile.sh')"
check "names the budget that would hold it" "$(contains "$out" 'timeout-minutes: 46')"

echo "case: the same script in the same loop under a budget above the three runs passes"
root=$(fixture script-in-loop-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Reconcile the ci-oks of each head the queue left
        timeout-minutes: 46
        run: |
          for head in 1 2 3; do
            .github/scripts/ci-ok-reconcile.sh "$head"
          done
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# A declaration writes one number, so it answers for one loop. A second loop of the same file,
# step and verb would take that number without anybody writing it one, which is the undercount
# the declaration exists to stop.
echo "case: a declaration that would cover two loops fails"
root=$(fixture declaration-two-loops)
cat > "$root/.github/scripts/ci-ok-reconcile.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
while read -r id status was marker; do
  retry_fetch "the PATCH of ci-ok check run $id" 30 -- \
    gh api --silent --method PATCH "repos/$GITHUB_REPOSITORY/check-runs/$id" --input "$body"
done <<<"$runs"
while read -r id; do
  retry_fetch "the ci-ok of run $id" 30 -- \
    gh api "repos/$GITHUB_REPOSITORY/check-runs/$id"
done <<<"$others"
SH
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the entry and the two loops it covers" \
  "$(contains "$out" 'the declared loop for .github/scripts/ci-ok-reconcile.sh .gh api. covers the loops at lines 4, 8')"
check "says what the entry was written for" "$(contains "$out" 'This entry said 3 passes')"

# Rule 8 reads a curl's own attempts off the ladder rule 1 holds it to, not off a fixed eight:
# `--retry 10` is eleven attempts at `--max-time` with 1, 2, 4, …, 512 s between them, so
# 11 x 30 + 1023 = 1353 s.
echo "case: a curl's own attempts are counted at the ladder its options write"
root=$(fixture curl-ladder-budget)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install crane
        timeout-minutes: 22
        run: |
          curl -fsSL --retry 10 --retry-all-errors --max-time 30 -o crane.tar.gz \
            https://example.com/crane.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts the eleven attempts and curl's own waits" \
  "$(contains "$out" 'retries take 1353 s to exhaust')"
check "names the budget that would hold it" "$(contains "$out" 'timeout-minutes: 23')"

echo "case: the same curl under a budget above its ladder passes"
root=$(fixture curl-ladder-budget-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install crane
        timeout-minutes: 23
        run: |
          curl -fsSL --retry 10 --retry-all-errors --max-time 30 -o crane.tar.gz \
            https://example.com/crane.tar.gz
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: an argument this check does not take"
root=$(fixture bad-argument)
run_check "$root" --everything
check "exits 2" "$(is "$status" 2)"
check "says what it takes" "$(contains "$out" 'this check takes no argument')"

# Rule 9. A workflow `run:` that names no shell runs under GitHub's `/usr/bin/bash -e {0}`:
# errexit on, pipefail off, so a pipeline's status is its last command's and a fetch left of a
# `|` can fail with the step exiting 0. The retry around such a pipeline sees that 0 and never
# retries, which is the hole these cases close.
echo "case: a retried fetch left of a pipe in a workflow run"
root=$(fixture piped-retry)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install changie
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a tarball" 60 -- curl -fsSL https://example.com/changie.tar.gz | tar -xz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line and the step" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=9::.curl. in step .Install changie. is not the last command of its pipeline')"
check "says whose status the step reads" \
  "$(contains "$out" "a pipeline's status is its last command's alone")"
check "says the retry around it never retries" "$(contains "$out" 'never retries')"
check "names the cure" "$(contains "$out" 'write the pipeline as two commands')"
check "counts the one refusal" "$(contains "$out" '1 download(s) outside the one retry rule')"

echo "case: a bare fetch left of a pipe is refused for the pipeline as well as the floor"
root=$(fixture piped-bare-curl)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install changie
        run: |
          curl -fsSL https://example.com/changie.tar.gz | tar -xz -C /usr/local/bin changie
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the missing floor" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=8::.curl. in step .Install changie. fetches without the floor')"
check "names the pipeline too, not only the missing retry" \
  "$(contains "$out" 'is not the last command of its pipeline')"
check "the two faults of one site are one annotation" \
  "$(contains "$out" '1 download(s) outside the one retry rule')"

echo "case: the same piped fetch in a composite action passes, since its shell sets pipefail"
root=$(fixture piped-action)
mkdir -p "$root/.github/actions/install-thing"
cat > "$root/.github/actions/install-thing/action.yml" <<'YAML'
name: Install the thing
description: Installs the thing through a shell that carries the fetch's failure.
runs:
  using: composite
  steps:
    - name: Install the thing
      shell: bash
      run: |
        . "$GITHUB_ACTION_PATH/../../scripts/ci-retry.sh"
        retry_fetch "a tarball" 60 -- curl -fsSL https://example.com/thing.tar.gz | tar -xz
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a workflow's gh api with a logical OR after it is no pipeline"
root=$(fixture logical-or)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Cancel the runs this push started
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the force-cancel" 30 -- gh api --silent --method POST "repos/$GITHUB_REPOSITORY/actions/runs/1/force-cancel" ||
            echo "failed"
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a fetch that is the last command of its pipeline passes"
root=$(fixture piped-last)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read what this job downloads
        run: |
          echo x | xargs curl -fsSL --retry 7 --retry-all-errors --max-time 60 -o out
          cat urls.txt | curl -fsSL --retry 7 --retry-all-errors --max-time 60 -K - -o out2
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
list_check "$root"
check "the curl at the end of its pipeline is a site like any other" \
  "$(contains "$out" '^.github/workflows/case.yaml	9	curl	Read what this job downloads$')"

echo "case: a workflow step that names shell bash carries the fetch's failure and passes"
root=$(fixture piped-step-shell)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install changie
        shell: bash
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a tarball" 60 -- curl -fsSL https://example.com/changie.tar.gz | tar -xz
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a workflow whose defaults.run.shell names bash passes"
root=$(fixture piped-workflow-defaults)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
defaults:
  run:
    shell: bash
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install changie
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a tarball" 60 -- curl -fsSL https://example.com/changie.tar.gz | tar -xz
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a job whose defaults.run.shell names sh is refused, since sh sets no pipefail"
root=$(fixture piped-job-defaults)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    defaults:
      run:
        shell: sh
    steps:
      - name: Install changie
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a tarball" 60 -- curl -fsSL https://example.com/changie.tar.gz | tar -xz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line and the step" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=12::.curl. in step .Install changie. is not the last command of its pipeline')"

echo "case: a retried fetch left of a pipe that carries standard error too is refused"
root=$(fixture piped-stderr)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install changie
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a tarball" 60 -- curl -fsSL https://example.com/changie.tar.gz |& tar -xz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line and the step" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=9::.curl. in step .Install changie. is not the last command of its pipeline')"
check "counts the one refusal" "$(contains "$out" '1 download(s) outside the one retry rule')"

summary "check-download-retry.sh"
