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
# list holds, the loops its DECLARED_LOOPS list declares the arithmetic of, each at the file,
# step and verb that list holds, and the three Dockerfiles it reads. Both lists fail the check
# when an entry matches nothing, which is why the baseline carries one site for each. A case
# that proves a refusal adds a workflow, an action or a Dockerfile of its own beside that
# baseline, so a refusal is the case's own and not a hole in the baseline, and the baseline's
# own case proves it passes.
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
        # The browser download's 5 x 300 + 150 = 1650 s. Rule 8 asks every step holding a
        # retried call for a budget above its arithmetic, so the baseline's own steps carry
        # one: a fixture without them fails for a reason no case is about.
        timeout-minutes: 28
        run: |
          .github/scripts/playwright-install.sh
          . .github/scripts/ci-retry.sh
          retry_fetch "Playwright's Chromium" 300 -- bunx playwright install chromium
YAML

  # Exception (a)'s fourth site: the browser lane's visual job pulls the pinned Playwright copy
  # from the same package. The entry naming it is in EXEMPTIONS, and an entry matching nothing
  # fails this check, so every fixture carries the site its entry names.
  cat > "$root/.github/workflows/dispatch-browser.yaml" <<YAML
name: Dispatch browser
on:
  workflow_call:
jobs:
  visual:
    runs-on: ubuntu-24.04
    steps:
      - name: Pull the pinned Playwright copy
        run: docker pull ghcr.io/example/legion/ci@$DIGEST
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
        # 5 x 60 + 150 = 450 s.
        timeout-minutes: 8
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

  # The loops DECLARED_LOOPS declares the arithmetic of, one call of each at the file, step
  # and verb that list holds. Every fixture carries them for the reason it carries the four
  # exceptions: a declaration matching no such loop fails the check, so a tree without them
  # fails for a reason no case is about. Each is the shape that list is for — a loop over the
  # work the run found, whose pass count the check cannot read off the loop — and each passes
  # every other rule, so a case that adds one bad site still proves that site alone. Each
  # carries the budget rule 8 asks of a step holding a retried call, 6 minutes above the 300 s
  # its declaration writes, so none of them is a budget a case has to reckon with.
  cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  cancel-started:
    runs-on: ubuntu-24.04
    steps:
      - name: Force-cancel the runs of the groups the queue removed
        timeout-minutes: 6
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
        timeout-minutes: 6
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
        timeout-minutes: 6
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

# declare_entry <root> <one DECLARED_LOOPS entry, in Python>: replaces the check this fixture
# links with a copy of it carrying that entry at the end of its DECLARED_LOOPS table. A
# declaration is proved this way because the tree's own table holds only the entries the tree
# needs, and none of them covers a loop a fixture writes. The copy is the same file otherwise,
# so every other rule reads the fixture as it always does.
declare_entry() {
  local root=$1 entry=$2 copy="$root/.github/scripts/check-download-retry.sh"
  rm "$copy"
  awk -v entry="$entry" '
    /^DECLARED_LOOPS/ { inside = 1 }
    inside && $0 == "]" { print entry; inside = 0 }
    { print }
  ' "$check_script" > "$copy"
  chmod +x "$copy"
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
check "counts the sites the exceptions name" "$(contains "$out" '7 site(s) the exceptions name')"
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
        # 8 x 30 + 127 = 367 s.
        timeout-minutes: 7
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
        # 8 x 60 + 7 x 20 = 620 s.
        timeout-minutes: 11
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
        # Three passes of 5 x 300 + 150 = 1650 s. The loop writes its passes so that this case
        # proves rule 2 alone: an unreadable one would be refused for want of a figure.
        timeout-minutes: 83
        run: |
          . .github/scripts/ci-retry.sh
          for row in one two three; do
            retry_fetch "the copy of $row" 300 -- crane copy "$row" "ghcr.io/example/ci:$row"
          done
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
        # 5 x 60 + 150 = 450 s.
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the worker image's jj" 60 -- mise install "$jj_tool"
          jj_path=$(mise x "$jj_tool" -- sh -c 'command -v jj')
          install -m 0755 "$jj_path" "$HOME/.local/bin/jj"
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
list_check "$root"
check "the install is on the list" "$(contains "$out" 'case.yaml	11	mise install	')"
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

# The same scoping in a workflow step, which is where ci.yaml's proof-sdk-offline job reads its
# fault proxy: two requests through the proxy before the installs, one to a faulted host that
# must be refused and one to a control that must be answered. Each reads whether the proxy is in
# the path and brings nothing into the job, so each is written the way
# .github/scripts/download-retry-proof.sh's through_proxy writes one — error silenced, status
# consumed into the reading — and this check reads it as the probe it is. Written as a plain
# fetch the same two requests are two downloads, and the pair below proves both halves: a step
# that takes the scoping out of the shape is refused by rule 1 for each curl, and by rule 8 for
# the budget the ladders it would then make need.
echo "case: a workflow step's curl whose status is the reading is no download"
root=$(fixture probe-step)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  offline:
    runs-on: ubuntu-24.04
    steps:
      - name: Probe the proxy before the install
        run: |
          faulted=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 30 https://api.example.com/meta 2>/dev/null || true)
          [ "${faulted:-000}" = 000 ]
          passed=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 60 https://registry.example.org/ 2>/dev/null || true)
          [ "${passed:-000}" != 000 ]
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
list_check "$root"
check "neither request is on the list" "$(is "$(contains "$out" 'case.yaml')" false)"

echo "case: the same two requests written as plain fetches are two downloads"
root=$(fixture probe-step-not)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  offline:
    runs-on: ubuntu-24.04
    steps:
      - name: Probe the proxy before the install
        run: |
          if curl -sS -o /dev/null --max-time 30 https://api.example.com/meta; then
            exit 1
          fi
          curl -sS -o /dev/null --max-time 60 https://registry.example.org/
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the first request" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=8::.curl. in step .Probe the proxy before the install. fetches without the floor')"
check "names the second request" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=11::.curl. in step .Probe the proxy before the install. fetches without the floor')"
check "asks the step for the budget those ladders would need" \
  "$(contains "$out" "holds 974 s of retries and carries no \`timeout-minutes\`")"

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
        # 5 x 95 + 150 = 625 s and 5 x 125 + 150 = 775 s; the unpack after them takes no
        # network and is no term.
        timeout-minutes: 24
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
        # 5 x 95 + 150 = 625 s and 5 x 125 + 150 = 775 s.
        timeout-minutes: 24
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "apt's package lists" 95 -- sudo timeout 90 apt-get update
          retry_fetch "the packages" 125 -- \
            sudo timeout 120 apt-get install -y --no-install-recommends -d $PACKAGES
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line, the step and the option" \
  "$(contains "$out" "file=.github/workflows/case.yaml,line=11::.apt-get update. in step .Install the packages. passes no .-o APT::Update::Error-Mode=any.")"
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
        # 5 x 300 + 150 = 1650 s.
        timeout-minutes: 28
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
  "$(contains "$out" '^.github/workflows/case.yaml	11	go mod download	Download the Go modules this job builds$')"
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

# Two uncounted loops, one inside the other's script: the step's loop cannot be counted, and
# neither can the loop inside the script it runs, so the figure for one run of that script is
# one this check cannot make either. It printed `0 s` here while the read's None was taken for
# a zero, which is the gate stating a cost it never computed - rule 8's own defect, one layer
# in (dispatch://INFRA-1545, round 4).
echo "case: a script whose own loop is uncounted is refused with no figure at all"
root=$(fixture script-loop-unreadable)
cat > "$root/.github/scripts/c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
while read -r id; do
  retry_fetch "the run $id" 30 -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/$id"
done <<<"$ids"
SH
chmod +x "$root/.github/scripts/c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read every run the decision named
        timeout-minutes: 30
        run: |
          while read -r id; do
            .github/scripts/c.sh "$id"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and the call it cannot count" \
  "$(contains "$out" "step .Read every run the decision named. carries .timeout-minutes: 30. and this check can put no figure on it")"
check "names the script call and the loop around it" \
  "$(contains "$out" '..github/scripts/c.sh. at .github/workflows/case.yaml:10, inside the loop at line 9')"
check "says one run of that script reaches an uncounted loop" \
  "$(contains "$out" 'One run of ..github/scripts/c.sh. reaches a loop, its own or one in a script it calls, whose passes this check cannot read either')"
check "prints no figure for a cost it cannot compute" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"
check "still says what to do instead" \
  "$(contains "$out" 'take the call out of the loop')"

# The loop can be two scripts in: the step loops over a script that calls another outside any
# loop, and that second one holds the `while read`. The figure for one run of the first is
# still one this check cannot make, because the run reaches the uncounted loop either way, but
# the loop is not the first script's own — the sentence said it was while the site list named
# the second script, so the two halves of one refusal pointed a reader at different files
# (dispatch://INFRA-1545, round 5).
echo "case: a loop in a script the called script calls is refused without naming the wrong file"
root=$(fixture script-loop-two-deep)
cat > "$root/.github/scripts/c2.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/d.sh "$1"
SH
cat > "$root/.github/scripts/d.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
while read -r id; do
  retry_fetch "the run $id" 30 -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/$id"
done <<<"$ids"
SH
chmod +x "$root/.github/scripts/c2.sh" "$root/.github/scripts/d.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read every run the decision named
        timeout-minutes: 30
        run: |
          while read -r id; do
            .github/scripts/c2.sh "$id"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the script the step runs and the loop around that call" \
  "$(contains "$out" '..github/scripts/c2.sh. at .github/workflows/case.yaml:10, inside the loop at line 9')"
check "names the site in the second script that stopped the reading" \
  "$(contains "$out" '.gh api. at .github/scripts/d.sh:5, inside the loop at line 4')"
check "says the loop one run reaches may sit in a script that script calls" \
  "$(contains "$out" 'One run of ..github/scripts/c2.sh. reaches a loop, its own or one in a script it calls')"
check "does not call the uncounted loop the first script's own" \
  "$(is "$(contains "$out" 'holds a loop of its own')" false)"

# A loop this check cannot count around a script that retries nothing is no loop rule 8 has
# anything to say about: nobody waits for an attempt under it, so there is no sum for a budget
# to exceed and no pass count to ask the step's author for. The gate refused this shape and
# priced the run it was refusing at `0 s`, because blocked_calls lists every script call in an
# uncounted loop and the script's own arithmetic of 0 is not the None the no-figure sentence
# reads (dispatch://INFRA-1545, round 5).
echo "case: a loop around a script that fetches nothing passes"
root=$(fixture script-no-fetch-in-loop)
cat > "$root/.github/scripts/zz-e.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "$1"
SH
chmod +x "$root/.github/scripts/zz-e.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Echo every id the decision named
        timeout-minutes: 30
        run: |
          printf 'a\nb\n' | while read -r id; do .github/scripts/zz-e.sh "$id"; done
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "asks the step for no pass count" \
  "$(is "$(contains "$out" 'can put no figure on it')" false)"
check "prices no run of that script at nothing" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"

# One layer deeper: the loop sits in a script the step runs, around a call of the costless
# one, so the item blocked_calls lists is that inner call rather than the step's own. The one
# reading drops it there too, because the step's list and the lists of every script and
# action it reaches take the same filter (dispatch://INFRA-1545, R5). What leaves the step
# with a figure rather than none is carried(), which answers 0 for a call of a script that
# costs nothing however many passes are above it; the case below holding a priced script
# beside such a loop is the one that pins that answer.
echo "case: a loop around a script that calls a script that fetches nothing passes"
root=$(fixture script-no-fetch-two-deep)
cat > "$root/.github/scripts/zz-g.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "$1"
SH
cat > "$root/.github/scripts/zz-h.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
while read -r id; do
  .github/scripts/zz-g.sh "$id"
done <<<"$ids"
SH
chmod +x "$root/.github/scripts/zz-g.sh" "$root/.github/scripts/zz-h.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Echo every id the decision named
        timeout-minutes: 30
        run: .github/scripts/zz-h.sh
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "asks the step for no pass count" \
  "$(is "$(contains "$out" 'can put no figure on it')" false)"

# A script this check cannot price is not a script that retries nothing. A curl making its own
# attempts passes rule 1 on the options it writes, and `--max-time "$bound"` is one of them:
# the flag is there, so each attempt is bounded, and the seconds are not, so one_call reads no
# figure off the call. Answering 0 for that made the script read as one holding no retry at
# all, and the filter above dropped the loop around it, where the fetch is real and the step
# waits for the whole of it once per pass (dispatch://INFRA-1545, R6).
echo "case: a loop around a script whose curl writes its bound as a variable is refused"
root=$(fixture script-unread-bound-in-loop)
cat > "$root/.github/scripts/zz-i.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
bound=60
curl -fsSL --retry 7 --retry-all-errors --max-time "$bound" -o run.json \
  "https://api.github.com/repos/$GITHUB_REPOSITORY/actions/runs/$1"
SH
chmod +x "$root/.github/scripts/zz-i.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read every run the decision named
        timeout-minutes: 1
        run: |
          printf 'a\nb\n' | while read -r id; do .github/scripts/zz-i.sh "$id"; done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the script call and the loop around it" \
  "$(contains "$out" '..github/scripts/zz-i.sh. at .github/workflows/case.yaml:9, inside the loop at line 9')"
check "says the bound is written as a variable rather than as a number" \
  "$(contains "$out" 'makes attempts whose bound is written as a variable rather than as a number')"
check "prices no run of that script at nothing" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"

# The third member of that family, and the one the two cases above leave open: the site whose
# bound this check cannot read, written in the body's own loop rather than in a script the
# loop runs. One call of it has no figure — `--max-time "$bound"` bounds the attempt where the
# seconds are not written — and the loop around it has no pass count, so both halves of what
# the step waits for are unread. uncounted_of dropped it with the sites that cost nothing,
# because `one is None` was read as "this item is a script call" and a site with no figure had
# no sentence of its own to be refused with, so rule 8 priced the step's whole wait at 0 and
# refused nobody for it (dispatch://INFRA-1545, R8).
#
# The fetch is written in a function the loop calls, because rule 2 refuses any fetch that is
# not a `retry_fetch` call written in a loop's own body, and a fetch in a function the loop
# runs sits in no loop of its own. That is rule 8's second reading of a loop — times_run folds
# the loops around the calls of the function holding the call — so the case is about rule 8
# alone, and the fixture passes every other rule.
echo "case: a loop around the step's own curl whose bound is a variable is refused"
root=$(fixture site-unread-bound-in-loop)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read every run the decision named
        timeout-minutes: 30
        run: |
          read_run() {
            curl -fsSL --retry 7 --retry-all-errors --max-time "$bound" \
              -o "run-$1.json" "https://api.github.com/repos/$GITHUB_REPOSITORY/actions/runs/$1"
          }
          while read -r id; do
            read_run "$id"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and the site it cannot count" \
  "$(contains "$out" "step .Read every run the decision named. carries .timeout-minutes: 30. and this check can put no figure on it")"
check "names the site's line and the loop around it" \
  "$(contains "$out" '.curl. at .github/workflows/case.yaml:10, inside the loop at line 13')"
check "says what one call of it takes is a figure it cannot make" \
  "$(contains "$out" 'What one call of .curl. there takes is a figure this check cannot make, because its bound is written as a variable rather than as a number')"
check "says the passes of the loop are unread too" \
  "$(contains "$out" 'the step waits for that once per pass of a loop whose passes this check cannot read either')"
check "prints no figure for a call it cannot price" \
  "$(is "$(contains "$out" 'One call there takes')" false)"
check "prices the site at nothing nowhere" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"
check "gives the site's advice and not a script call's" \
  "$(contains "$out" 'write that bound as a number')"
check "says no declaration answers an unread bound" \
  "$(contains "$out" 'No entry of DECLARED_LOOPS answers this one: an entry writes the seconds a site contributes to a budget')"
check "names the refusal that follows a bound written as a number, and what answers it" \
  "$(contains "$out" 'this check refuses the step again with the seconds one call takes, and an entry of DECLARED_LOOPS answers that refusal')"
check "offers no pass count as a way out of this one" \
  "$(is "$(contains "$out" 'Write the count where this check reads it')" false)"
check "does not tell the author to take a call out of a loop" \
  "$(is "$(contains "$out" 'take the call out of the loop')" false)"

# The other half of that reading, which the case above must not take with it: a site one_call
# prices at 0 is still dropped. The loop the exempt action writes is as unreadable as the one
# above, and the `docker pull` under it is a verb this check puts no attempt behind, so there
# is nothing under that loop for the step's budget to exceed and no pass count to ask the
# action's author for. Keeping every site with no figure, rather than only the ones whose
# bound is unread, refuses this step — and the baseline's own two image loops with it.
echo "case: a step whose action loops, uncounted, over a verb that waits for nothing passes"
root=$(fixture site-costless-in-loop)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Pull the images tests start
        timeout-minutes: 30
        uses: ./.github/actions/pull-test-images
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "asks the step for no pass count" \
  "$(is "$(contains "$out" 'can put no figure on it')" false)"
check "prices no call of that loop at nothing" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"

# The filter's question asked one layer in: the step's loop runs a script whose own work is a
# loop this check cannot count over a script that fetches nothing. No attempt waits under
# either loop, so neither is a loop rule 8 has a pass count to ask the step's author for.
# Reading the question as "the script holds no uncounted item of its own" refused this with
# the `0 s` sentence, because the inner call is an item of the script the step runs
# (dispatch://INFRA-1545, R6).
echo "case: a loop around a script that loops over a script that fetches nothing passes"
root=$(fixture script-no-fetch-in-loop-two-deep)
cat > "$root/.github/scripts/zz-j.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "$1"
SH
cat > "$root/.github/scripts/zz-k.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
while read -r id; do
  .github/scripts/zz-j.sh "$id"
done <<<"$ids"
SH
chmod +x "$root/.github/scripts/zz-j.sh" "$root/.github/scripts/zz-k.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Echo every id the decision named
        timeout-minutes: 30
        run: |
          printf 'a\nb\n' | while read -r id; do .github/scripts/zz-k.sh "$id"; done
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "asks the step for no pass count" \
  "$(is "$(contains "$out" 'can put no figure on it')" false)"
check "prices no run of either script at nothing" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"

# A script this check does price, holding beside that ladder a loop it cannot count around a
# costless script. The ladder is 450 s the step waits for and the costless loop takes nothing
# off it, so the budget is held to the 450 s. carried()'s answer of 0 for a call of a script
# that costs nothing is what keeps that figure readable: without it the costless call answers
# None, the whole script's arithmetic answers None with it, and the step is compared with
# nothing and passes in silence (dispatch://INFRA-1545, R6).
echo "case: a priced script that also loops, uncounted, over a costless one keeps its figure"
root=$(fixture script-priced-beside-costless-loop)
cat > "$root/.github/scripts/zz-l.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "$1"
SH
cat > "$root/.github/scripts/zz-m.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "the run" 60 -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/$1"
while read -r id; do
  .github/scripts/zz-l.sh "$id"
done <<<"$ids"
SH
chmod +x "$root/.github/scripts/zz-l.sh" "$root/.github/scripts/zz-m.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the run the decision named
        timeout-minutes: 1
        run: .github/scripts/zz-m.sh a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "holds the step to the ladder the priced script makes" \
  "$(contains "$out" 'its own retries take 450 s to exhaust')"
check "asks the step for no pass count" \
  "$(is "$(contains "$out" 'can put no figure on it')" false)"

# The same loop around a script that does fetch is the shape rule 8 is for: the step waits for
# that script's whole ladder once per pass, and the passes are unread.
echo "case: the same loop around a script that does fetch is refused"
root=$(fixture script-fetch-in-loop)
cat > "$root/.github/scripts/zz-f.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "the run" 60 -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/$1"
SH
chmod +x "$root/.github/scripts/zz-f.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read every run the decision named
        timeout-minutes: 30
        run: |
          printf 'a\nb\n' | while read -r id; do .github/scripts/zz-f.sh "$id"; done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the script call and the loop around it" \
  "$(contains "$out" '..github/scripts/zz-f.sh. at .github/workflows/case.yaml:9, inside the loop at line 9')"
check "says what one run of that script takes" \
  "$(contains "$out" 'One run of that script takes 450 s to exhaust its attempts')"

# A step with no budget of its own is no longer a step nothing judges: rule 8 asks a budget of
# every step holding a retried call, so the loop this check cannot read is refused here too,
# and the refusal names the loop rather than the missing budget — a reader cannot write the
# budget until the site has a figure.
echo "case: the same loop in a step with no budget is refused and named all the same"
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
check "exits 1" "$(is "$status" 1)"
check "says the step writes no budget" \
  "$(contains "$out" "step .Cancel every run the decision named. carries no .timeout-minutes. and this check can put no figure on it")"
check "names the loop's file and line" \
  "$(contains "$out" '.gh api. at .github/workflows/case.yaml:10, inside the loop at line 9')"

# A loop whose passes are as many as the run finds work has its own arithmetic declared in the
# check's own DECLARED_LOOPS, and the budget is held to that figure. The baseline's
# ci-ok-reconcile.sh is one of them, declared at the 900 s three PATCHes take: a 15-minute
# budget does not strictly exceed them.
echo "case: a declared loop is counted at the seconds the declaration writes"
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
check "counts the 900 s the declaration writes" \
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
check "says what the declaration was for" "$(contains "$out" 'written for 1650 s')"

# A site whose passes this check cannot resolve is answered in seconds, not in passes, and the
# figure stands for the whole of what that site contributes to its step.
echo "case: a declared arithmetic is the figure the budget is held to"
root=$(fixture declared-arithmetic)
declare_entry "$root" '    DeclaredArithmetic(".github/workflows/case.yaml",
                       "Cancel every run the decision named", "gh api", 1200,
                       "four cancels at 5 x 30 + 150 = 300 s each: 4 x 300 = 1200 s"),'
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Cancel every run the decision named
        timeout-minutes: 20
        run: |
          . .github/scripts/ci-retry.sh
          while read -r id; do
            retry_fetch "the cancel of run $id" 30 -- \
              gh api --silent --method POST "repos/$GITHUB_REPOSITORY/actions/runs/$id/cancel"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "uses the declared seconds and not one call of the site" \
  "$(contains "$out" 'retries take 1200 s to exhaust')"
check "no longer refuses the site for want of a figure" \
  "$(is "$(contains "$out" 'can put no figure on it')" false)"

echo "case: a declared arithmetic below one call of its site is refused"
root=$(fixture declared-arithmetic-floor)
declare_entry "$root" '    DeclaredArithmetic(".github/workflows/case.yaml",
                       "Cancel every run the decision named", "gh api", 200,
                       "a figure nobody derived"),'
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Cancel every run the decision named
        timeout-minutes: 20
        run: |
          . .github/scripts/ci-retry.sh
          while read -r id; do
            retry_fetch "the cancel of run $id" 30 -- \
              gh api --silent --method POST "repos/$GITHUB_REPOSITORY/actions/runs/$id/cancel"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the site and the floor one call of it sets" \
  "$(contains "$out" 'writes 200 s, and one call of that site at .github/workflows/case.yaml:11 takes 300 s')"
check "says a site costs one call of itself" \
  "$(contains "$out" 'A site costs at least one call of itself')"

# The shape that reached no comparison at all: a step whose whole arithmetic is a composite
# action's, carrying no budget of its own. ci.yaml's `airlock-lean` held install-elan's
# 8 x 60 + 127 = 607 s ladder that way, under a job budget with no term for it, and this check
# built a budget only for a step that wrote one.
echo "case: a step running a composite action's ladder with no budget of its own is refused"
root=$(fixture action-unbudgeted)
mkdir -p "$root/.github/actions/install-elan"
cat > "$root/.github/actions/install-elan/action.yml" <<'YAML'
name: Install elan
description: Installs elan, the Lean toolchain manager.
runs:
  using: composite
  steps:
    - name: Install elan
      shell: bash
      run: |
        curl -fsSL --retry 7 --retry-all-errors --max-time 60 -o elan.tar.gz \
          https://example.com/elan.tar.gz
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install elan
        uses: ./.github/actions/install-elan
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step, its arithmetic and the budget it writes none of" \
  "$(contains "$out" "step .Install elan. holds 607 s of retries and carries no .timeout-minutes. of its own")"
check "names the measured shape it comes from" "$(contains "$out" 'install-elan')"
check "says what to write" "$(contains "$out" 'timeout-minutes: 11')"

echo "case: the same step carrying that budget passes"
root=$(fixture action-budgeted)
mkdir -p "$root/.github/actions/install-elan"
cat > "$root/.github/actions/install-elan/action.yml" <<'YAML'
name: Install elan
description: Installs elan, the Lean toolchain manager.
runs:
  using: composite
  steps:
    - name: Install elan
      shell: bash
      run: |
        curl -fsSL --retry 7 --retry-all-errors --max-time 60 -o elan.tar.gz \
          https://example.com/elan.tar.gz
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install elan
        uses: ./.github/actions/install-elan
        timeout-minutes: 11
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# A job ceiling under one of its own steps' arithmetic cuts that step before the step's own
# budget can name the site, so the step's margin buys nothing.
echo "case: a job whose budget is under one of its steps' retries is refused"
root=$(fixture job-budget)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 5
    steps:
      - name: Install changie
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the changie tarball" 60 -- \
            curl -fsSL --max-time 60 -o changie.tar.gz https://example.com/changie.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job, its budget and the step's arithmetic" \
  "$(contains "$out" "job .build. carries .timeout-minutes: 5., 300 s, and its step .Install changie. holds retries taking 450 s to exhaust")"
check "says what to write on the job" "$(contains "$out" 'timeout-minutes: 8. or more on this job')"

echo "case: the same job with a budget above that step passes"
root=$(fixture job-budget-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 9
    steps:
      - name: Install changie
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the changie tarball" 60 -- \
            curl -fsSL --max-time 60 -o changie.tar.gz https://example.com/changie.tar.gz
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

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

# A heredoc body belongs to the command consuming it. A shell runs what it reads on its standard
# input, so the calls a `bash <<EOF` body writes are the step's own and their attempts are the
# step's cost; `cat`, a redirect into a file and a `python3 <<'PY'` take their body as data,
# where a path is a path something prints rather than runs. The baseline's ci-ok-reconcile.sh is
# 900 s of its own, which is the figure every case below turns on.
echo "case: a script called inside a bash heredoc is the step's own arithmetic"
root=$(fixture heredoc-bash)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Reconcile the ci-oks the queue left
        timeout-minutes: 15
        run: |
          bash <<'EOF'
          .github/scripts/ci-ok-reconcile.sh
          EOF
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts the call the body makes" "$(contains "$out" 'retries take 900 s to exhaust')"
check "names the script that body runs" \
  "$(contains "$out" 'that of .github/scripts/ci-ok-reconcile.sh, which it runs')"
check "names the budget that would hold it" "$(contains "$out" 'timeout-minutes: 16')"

echo "case: the same body under a budget above the call it makes passes"
root=$(fixture heredoc-bash-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Reconcile the ci-oks the queue left
        timeout-minutes: 16
        run: |
          bash <<'EOF'
          .github/scripts/ci-ok-reconcile.sh
          EOF
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: the same call inside a cat heredoc is data and costs the step nothing"
root=$(fixture heredoc-cat)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Write the note that names the script a reader runs
        run: |
          cat > note.txt <<'EOF'
          Run .github/scripts/ci-ok-reconcile.sh once the queue has settled.
          EOF
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "the step holding no call is asked for no budget" \
  "$(is "$(contains "$out" '900 s')" false)"

echo "case: the same call inside a python3 heredoc is data and costs the step nothing"
root=$(fixture heredoc-python)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Print the script a reader runs
        run: |
          python3 <<'PY'
          print("run .github/scripts/ci-ok-reconcile.sh once the queue has settled")
          PY
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "the step holding no call is asked for no budget" \
  "$(is "$(contains "$out" '900 s')" false)"

# However the shell is written, the body is the commands it runs: a bare word, an absolute path,
# options of its own, a `-s` that says the script is standard input, and a `-` written for the
# script argument, which is standard input spelled out.
echo "case: every spelling of a shell reads its body as the step's commands"
root=$(fixture heredoc-interpreters)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A shell written as an absolute path
        timeout-minutes: 15
        run: |
          /bin/bash <<'EOF'
          .github/scripts/ci-ok-reconcile.sh
          EOF
      - name: A shell carrying options of its own
        timeout-minutes: 15
        run: |
          sudo bash -euo pipefail <<'EOF'
          .github/scripts/ci-ok-reconcile.sh
          EOF
      - name: A shell told to read its script from standard input
        timeout-minutes: 15
        run: |
          bash -s stage1 <<'EOF'
          .github/scripts/ci-ok-reconcile.sh
          EOF
      - name: A dash written for the script argument
        timeout-minutes: 15
        run: |
          sh - <<'EOF'
          .github/scripts/ci-ok-reconcile.sh
          EOF
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the absolute path runs its body" \
  "$(contains "$out" "step .A shell written as an absolute path. carries .timeout-minutes: 15")"
check "the shell carrying options runs its body" \
  "$(contains "$out" "step .A shell carrying options of its own. carries .timeout-minutes: 15")"
check "the shell reading standard input runs its body" \
  "$(contains "$out" "step .A shell told to read its script from standard input. carries")"
check "the dash for the script argument runs its body" \
  "$(contains "$out" "step .A dash written for the script argument. carries")"
check "each of the four is one refusal" \
  "$(contains "$out" '4 download(s) outside the one retry rule')"

# An interpreter whose script is somewhere else reads its body as that script's input, which is
# the shell's own rule: the word behind `-c` is the script, and so is a file named on the line.
echo "case: an interpreter whose body is another script's input is data"
root=$(fixture heredoc-interpreter-input)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A script behind -c, whose body is that script's input
        run: |
          bash -c 'cat > note.txt' <<'EOF'
          .github/scripts/ci-ok-reconcile.sh
          EOF
      - name: A script file, whose body is that file's input
        run: |
          bash tools/note.sh <<'EOF'
          .github/scripts/ci-ok-reconcile.sh
          EOF
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "neither step is asked for a budget" "$(is "$(contains "$out" '900 s')" false)"

echo "case: a data heredoc inside a shell's heredoc is still data"
root=$(fixture heredoc-nested)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Reconcile the ci-oks and leave a note naming the other script
        timeout-minutes: 15
        run: |
          bash <<'OUTER'
          cat > note.txt <<'INNER'
          Read .github/scripts/check-pr-text.sh for the page this reconcile skipped.
          INNER
          .github/scripts/ci-ok-reconcile.sh
          OUTER
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the outer body is commands and the inner one is not" \
  "$(contains "$out" 'retries take 900 s to exhaust')"
check "the script the inner body names is no call of this step" \
  "$(is "$(contains "$out" 'check-pr-text.sh')" false)"

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
check "says what the entry was written for" "$(contains "$out" 'This entry said 900 s')"

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
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a tarball" 60 -- curl -fsSL https://example.com/changie.tar.gz | tar -xz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line and the step" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=10::.curl. in step .Install changie. is not the last command of its pipeline')"
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
        timeout-minutes: 6
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
        # Two curls of 8 x 60 + 127 = 607 s.
        timeout-minutes: 21
        run: |
          echo x | xargs curl -fsSL --retry 7 --retry-all-errors --max-time 60 -o out
          cat urls.txt | curl -fsSL --retry 7 --retry-all-errors --max-time 60 -K - -o out2
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
list_check "$root"
check "the curl at the end of its pipeline is a site like any other" \
  "$(contains "$out" '^.github/workflows/case.yaml	11	curl	Read what this job downloads$')"

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
        timeout-minutes: 8
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
        timeout-minutes: 8
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
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a tarball" 60 -- curl -fsSL https://example.com/changie.tar.gz | tar -xz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line and the step" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=13::.curl. in step .Install changie. is not the last command of its pipeline')"

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
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a tarball" 60 -- curl -fsSL https://example.com/changie.tar.gz |& tar -xz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line and the step" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=10::.curl. in step .Install changie. is not the last command of its pipeline')"
check "counts the one refusal" "$(contains "$out" '1 download(s) outside the one retry rule')"

summary "check-download-retry.sh"
