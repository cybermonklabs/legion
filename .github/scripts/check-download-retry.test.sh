#!/usr/bin/env bash
# Tests for `check-download-retry.sh`: every network fetch of this repository's CI retries at
# R1's floor, a curl whose own options would put its last attempt inside that floor is refused,
# an `apt-get update` fails rather than warns when an index read fails so that the retry has
# something to retry, a `mise x` carries no retry at all because it exits 0 when it cannot
# resolve the tool, a fetch left of a `|` in a step whose shell leaves pipefail off is refused
# because the pipeline's status is its last command's, a step's budget holds every pass of a
# loop around a retried call, one rule holds that retry, and the exceptions R2 lists are
# the only sites left bare (dispatch://INFRA-1545, R2).
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs. Every fixture starts from the
# same baseline: every exception that still has a site, each at the file, job and step name the
# check's exception
# list holds, the loops its DECLARED_LOOPS list declares the arithmetic of, each at the file,
# step and verb that list holds, the real harness-pool.sh, whose three runs RUN_EXEMPTIONS
# names, and the four Dockerfiles it reads. All three lists fail the check when an entry
# matches nothing, which is why the baseline carries one site or run for each. A case
# that proves a refusal adds a workflow, an action or a Dockerfile of its own beside that
# baseline, so a refusal is the case's own and not a hole in the baseline, and the baseline's
# own case proves it passes. The last of rule 8's cases is the one that builds no miniature: it
# copies this repository's own tree and lowers its real keys, each below the sum of the block the
# gate's --jobs says prices it, through .github/scripts/download_retry_real_tree.py.
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

# The canonical inline retry every network RUN of the Dockerfiles the gate reads opens with. A
# fixture that means to pass writes this text; a fixture that means to be refused writes another.
read -r -d '' CANONICAL <<'TEXT' || true
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  echo "retry: $what: attempt $i of 5 failed" >&2; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
TEXT

# fixture <name>: a tree holding the baseline below, with the script linked in. Echoes its root.
#
# The baseline is R2's exceptions that still have a site, which every fixture carries for two
# reasons: the
# check refuses an exception that matches no fetch, so a tree without them fails for a reason no
# case is about; and a case that then adds one bad site proves that site alone.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" \
    "$root/.github/actions/pull-test-images" "$root/.github/docker" \
    "$root/packages/daemon/docker" "$root/packages/envoy/docker" \
    "$root/packages/envoy/infra/host/heldportwrapper"
  ln -s "$check_script" "$root/.github/scripts/check-download-retry.sh"
  ln -s "$script_dir/workflow_paths.py" "$root/.github/scripts/workflow_paths.py"
  # The real harness-pool.sh, whose three runs of the command a caller hands it are the runs
  # RUN_EXEMPTIONS names: the check refuses an entry that matches no run, as it refuses a stale
  # exception, so a tree without the file fails for a reason no case is about
  # (dispatch://INFRA-1545, round 65).
  ln -s "$script_dir/harness-pool.sh" "$root/.github/scripts/harness-pool.sh"
  # The four Dockerfiles DOCKERFILES names, each of which the check refuses a tree for lacking.
  for dockerfile in \
    "$root/.github/docker/browser.Dockerfile" \
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

  # Exceptions (a) again and (b), and a browser download, which is a site like any other and
  # retries like one: exception (c)'s script held the apt phase beside it and went with that
  # script (cybermonklabs/legion#2938). The jobs the exceptions name sit in the files main's
  # #2945 moved them to, .github/workflows/ci-job-<id>.yaml, which is where the exceptions
  # name them.
  cat > "$root/.github/workflows/ci-job-envoy-client.yaml" <<YAML
name: CI job envoy-client
on:
  workflow_call:
jobs:
  envoy-client:
    runs-on: ubuntu-24.04
    steps:
      - name: Pre-pull the NATS image the credential tests run
        run: docker pull ghcr.io/example/legion/ci@$DIGEST
YAML
  # Exception (a) again: the claude-envoy job's pre-pull of that same NATS copy, which main
  # writes bare and EXEMPTIONS names by its step since review round 37 (dispatch://INFRA-1545).
  cat > "$root/.github/workflows/ci-job-claude-envoy.yaml" <<YAML
name: CI job claude-envoy
on:
  workflow_call:
jobs:
  claude-envoy:
    runs-on: ubuntu-24.04
    steps:
      - name: Pre-pull the NATS image the reply-grant row runs
        run: docker pull ghcr.io/example/legion/ci@$DIGEST
YAML
  # The download-retry-images job, which since the merge of main's #3675 is the second job of
  # ci.yaml's download-retry call (ci-job-download-retry.yaml), where the exception names it.
  cat > "$root/.github/workflows/ci-job-download-retry.yaml" <<YAML
name: CI job download-retry
on:
  workflow_call:
jobs:
  download-retry-images:
    runs-on: ubuntu-24.04
    steps:
      - name: Pull every base image the four builds read
        run: |
          for image in \$images; do docker pull "\$image"; done
YAML
  cat > "$root/.github/workflows/ci-job-airlock-lean.yaml" <<YAML
name: CI job airlock-lean
on:
  workflow_call:
jobs:
  airlock-lean:
    runs-on: ubuntu-24.04
    steps:
      # Rule 11's exemption names this job, and its guard refuses an entry matching no job that
      # compiles Go here, so every fixture carries the job its entry names, as each fixture
      # carries the site an EXEMPTIONS entry names. The real job builds a package importing the
      # standard library alone; a fixture needs only the verb.
      - name: Compare the Go airlock with the model
        run: go test ./internal/broker/airlock/
YAML
  cat > "$root/.github/workflows/ci.yaml" <<YAML
name: CI
on: [push]
jobs:
  dispatch:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@$PIN
      - name: Install Playwright Chromium
        # The browser download's 5 x 300 + 150 = 1650 s. Rule 8 asks every step holding a
        # retried call for a budget above its arithmetic, so the baseline's own steps carry
        # one: a fixture without them fails for a reason no case is about.
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "Playwright's Chromium" 300 -- bunx playwright install chromium
YAML

  # Exception (a)'s fourth to seventh sites: the browser lane's visual job pulls the pinned
  # Playwright copy and its stack's Postgres copy from the same package, and the multiprocess and
  # embedder jobs pull that same Postgres copy. The entries naming them are in EXEMPTIONS, and an
  # entry matching nothing fails this check, so every fixture carries the sites its entries name.
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
      - name: Pull the visual stack's Postgres copy
        run: docker pull ghcr.io/example/legion/ci@$DIGEST
  multiprocess:
    runs-on: ubuntu-24.04
    steps:
      - name: Pull the multiprocess stack's Postgres image
        run: docker pull ghcr.io/example/legion/ci@$DIGEST
  embedder:
    runs-on: ubuntu-24.04
    steps:
      - name: Pull the embedder stack's Postgres image
        run: docker pull ghcr.io/example/legion/ci@$DIGEST
YAML

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
  # every other rule, so a case that adds one bad site still proves that site alone. Each that a
  # workflow step holds carries the budget rule 8 asks of a step holding a retried call, 6
  # minutes above the 300 s its declaration writes, so none of them is a budget a case has to
  # reckon with. The merge queue's cancel act is a script of its own since main's #3675 moved
  # ci.yaml's cancel-started body into .github/scripts/cancel-started-act.sh, and its entry names
  # that script, so the fixture carries the script and a ci.yaml job that runs it by its path.
  cat > "$root/.github/scripts/cancel-started-act.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
while read -r act id; do
  retry_fetch "the force-cancel of run $id" 30 -- \
    gh api --silent --method POST "repos/$GITHUB_REPOSITORY/actions/runs/$id/force-cancel"
done <<<"$acts"
SH
  cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  cancel-started:
    runs-on: ubuntu-24.04
    steps:
      - name: Force-cancel the runs of the groups the queue removed
        # ci.yaml's cancel-started job runs the script by its path, as this one does, and the
        # loop in that script is this step's one retried call: 300 s, which DECLARED_LOOPS
        # declares for .github/scripts/cancel-started-act.sh, under a key of 6 minutes, 360 s.
        # The job keeps the eleven lines it held while that body was inline, so every case that
        # appends a job to this file still numbers its lines from line 27.
        timeout-minutes: 6
        run: .github/scripts/cancel-started-act.sh
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
  # check-pr-text.sh's three loops that hold a `gh api` read, each of which a DECLARED_LOOPS entry
  # names by its first line: `while :; do` (main's #4956 made it the GraphQL rounds; this fixture
  # keeps a page loop under that first line) and the two main's #4256 added. Every fixture
  # carries all three, so each entry matches its own.
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
while IFS= read -r tag; do
  retry_fetch "the release of $tag" 30 -- gh api "repos/$repository/releases/tags/$tag" > "$work/release"
done < "$work/containing"
while IFS= read -r sha; do
  retry_fetch "the files of commit $sha" 30 -- \
    gh api "repos/$repository/commits/$sha?per_page=100" --paginate > "$work/commit-$sha"
done < "$work/new-commits"
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
  # The annotations loop of runner-shutdown.sh, which one deadline bounds rather than a count of
  # passes (main's #2945 wrote the loop): its DECLARED_LOOPS entry matches nothing in a tree
  # without the file, so every fixture carries it, sourced as the repository sources it.
  cat > "$root/.github/scripts/runner-shutdown.sh" <<'SH'
#!/usr/bin/env bash
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ci-retry.sh"
runner_shutdown_kills() {
  local job url
  for job in "$@"; do
    while read -r url; do
      retry_fetch "the annotations of $job" 30 -- gh api --paginate "$url/annotations?per_page=100"
    done <<<"$urls"
  done
}
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
  # The entry of that table which is no loop: the one curl of .github/scripts/ci-tools.sh,
  # whose `--max-time` is the row's own field and so is a bound the check cannot read. One
  # call of it has no figure, so the declaration writes what the site contributes whole, and
  # an entry matching nothing fails the check — which is why every fixture carries this file
  # too. No step here runs it, so the seconds that entry writes sit in no budget a case has
  # to reckon with. It is sourced and not executed in the repository, and it is not
  # executable here either.
  cat > "$root/.github/scripts/ci-tools.sh" <<'SH'
#!/usr/bin/env bash
ci_tools_curl() {
  local file=$1 max_time=$2 url=$3
  shift 3
  curl -fsSL --retry 7 --retry-all-errors --retry-max-time 300 --max-time "$max_time" -o "$file" "$@" "$url"
}
SH
  chmod +x "$root/.github/scripts/check-pr-text.sh" "$root/.github/scripts/ci-ok-reconcile.sh" \
    "$root/.github/scripts/mirror-ci-images.sh"
  echo "$root"
}

# GATE_BOUND: how long one call of the gate over a fixture may take before this harness calls it
# hung and stops, naming the case that ran it. Measured on 2026-11-14 on an INFRA issue pod (2
# CPUs, gVisor): an unmutated harness run made 147 gate calls, each over a fixture, median 1.5 s,
# 99th percentile 1.87 s, slowest 1.94 s (`declaration-two-loops-below`). run_gate does not read a
# clock: it polls the call GATE_BOUND x 10 times, 600 polls of `sleep 0.1`, which is at least 60 s
# and longer under load, since each poll's sleep stretches with the load that slows the gate. That
# is at least thirty times the slowest fixture call, so it cannot fail a correct fixture call on a
# slow runner, and a gate that never returns fails here a minute or so in, under its case's name,
# rather than at the test job's own key with GitHub's bare timeout line. Round 32's mutation pass
# found two mutants of rule 8 that hang the gate (dispatch://INFRA-1545), and before this bound
# nothing below the job key ended them. It does not hold the real-tree case, the file's last, whose
# calls read a copy of the whole .github tree and take about thirty times a fixture's call on CI and
# forty in the pod: that case runs under GATE_TREE_BOUND below.
GATE_BOUND=60

# GATE_TREE_BOUND: GATE_BOUND for the real-tree case, which sets it as GATE_BOUND before it starts;
# that case is the file's last, so no fixture's call runs under it. The case calls the gate nine
# times over a copy of this repository's whole .github tree. On CI's runner (run 44744277980, test
# job 136315654414, 2026-11-20) each of the nine took about 16 s, the first 13:33:31Z ->
# 13:33:47Z. On an INFRA issue pod (2 CPUs, gVisor), idle, on 2026-11-20, one took about 63 s for
# the gate at c53b3c8d and about 66 s for this branch's; under load in that pod the same day, a full
# harness run cut the case's `arrows` call at GATE_BOUND's 60 s ('Results: 1875 passed, 1 failed').
# 600 s, 6000 polls of `sleep 0.1`, is over nine times the pod's idle call and over thirty-five
# times CI's, so a correct run in a loaded pod is not cut, and a gate that hangs over the tree still
# fails under this case's name well inside the test job's key: ci-job-test.yaml keys that job at 96
# minutes, job 136315654414 took 23 of them, and a hang cut here adds about ten.
GATE_TREE_BOUND=600

# run_gate <stderr: out|null> <root> <args…>: runs the gate in a session of its own with its
# output in a file, never through a pipe: a gate that hangs is bash waiting on its Python, and
# killing the bash alone leaves the Python holding a pipe open, which blocks a `$(…)` capture
# for ever. At the bound the whole session is killed, the hang is one FAIL under the current
# case, and the harness ends there, since every later case would hang the same way.
run_gate() {
  local stderr=$1 root=$2 log="$work/gate.out" pid tenths=0
  shift 2
  if [ "$stderr" = out ]; then
    ( cd "$root" && exec setsid ./.github/scripts/check-download-retry.sh "$@" ) > "$log" 2>&1 &
  else
    ( cd "$root" && exec setsid ./.github/scripts/check-download-retry.sh "$@" ) > "$log" 2>/dev/null &
  fi
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$tenths" -ge $((GATE_BOUND * 10)) ]; then
      kill -KILL -- "-$pid" 2>/dev/null
      wait "$pid" 2>/dev/null
      echo "  FAIL: the gate did not return within $((GATE_BOUND * 10)) polls of \`sleep 0.1\` after this case started it, which is at least ${GATE_BOUND} s and longer under load"
      FAIL=$((FAIL + 1))
      summary "check-download-retry.sh"
    fi
    sleep 0.1
    tenths=$((tenths + 1))
  done
  wait "$pid"
  status=$?
  out=$(cat "$log")
}

run_check() {
  local root=$1
  shift
  set +e
  run_gate out "$root" "$@"
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

# declare_in <root> <table> <one entry, in Python>: as declare_entry, for any table of
# declarations the gate holds — DECLARED_LOOPS, DECLARED_LIBRARIES or DECLARED_CALLS — and onto
# the copy an earlier call made, so one fixture can carry entries of more than one table
# (dispatch://INFRA-1545, round 37).
declare_in() {
  local root=$1 table=$2 entry=$3 copy="$root/.github/scripts/check-download-retry.sh"
  local source=$check_script
  if [ ! -L "$copy" ]; then
    source="$work/declare-in.sh"
    cp "$copy" "$source"
  fi
  rm "$copy"
  awk -v table="$table" -v entry="$entry" '
    index($0, table ":") == 1 { inside = 1 }
    inside && $0 == "]" { print entry; inside = 0 }
    { print }
  ' "$source" > "$copy"
  chmod +x "$copy"
}

list_check() {
  local root=$1
  set +e
  run_gate null "$root" --list
  set -e
}

echo "case: a tree holding each exception R2 still has a site for passes"
root=$(fixture exceptions)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
# Nine sites, not the seven this case counted while exception (c) had a script: that script
# went with the browser suite's move into an image this repository builds
# (cybermonklabs/legion#2938), and its two exempted apt sites went with it; the visual job's
# pull of its stack's Postgres copy is one added since (dispatch://INFRA-1545, round 37), the
# multiprocess and embedder jobs' pulls of that same copy are two more (round 41), and the
# claude-envoy job's pre-pull of the NATS copy is the ninth (review round 37).
check "counts the sites the exceptions name" "$(contains "$out" '9 site(s) the exceptions name')"
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
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=9::.curl. in step .Install changie. fetches without the floor')"
check "asks the curl at line 9 for --retry 7, naming no --retry it passes" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=9::' '--retry 7' --not '--retry ')"
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
check "refuses the curl at line 8 for --max-time <seconds> alone" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' 'Install uv' \
    '--max-time <seconds>' --not '--retry 7' '--retry-all-errors' '--fail')"

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
check "refuses the curl at line 8 for --fail alone" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' 'Install uv' \
    '--fail' --not '--retry 7' '--retry-all-errors' '--max-time <seconds>')"

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
check "refuses the curl at line 8 for --retry-delay or --retry-max-time it cannot read, against the 120 s floor" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' 'Install uv' \
    '--retry-delay' '--retry-max-time' '120 s')"

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
  "$(contains "$out" 'file=.github/actions/install-thing/action.yml,line=10::this loop retries .curl. itself, which makes it a second copy')"

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
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=12::this loop retries .apt-get update. itself')"

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

# A `retry_fetch` with no seconds between its `<what>` and its `--` is outside the helper's
# signature, and the helper refuses it with no attempt made, so it is refused by name and
# charged nothing (dispatch://INFRA-1545, round 37). Until round 37 it was read as a retried
# call with no bound, which it is not: nothing is retried.
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
check "refuses the call by name as outside the helper's signature" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Install the workspace's dependencies' of job 'build' calls .retry_fetch. in a form other than its signature")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

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

# `apk add` is one of THE VERBS since round 37, which found worker.Dockerfile's go-base stage
# fetching git from Alpine's CDN with no retry and nothing reporting it (dispatch://INFRA-1545).
echo "case: a Dockerfile with an unretried apk add is refused, naming the verb"
root=$(fixture bare-apk)
cat > "$root/packages/daemon/docker/worker.Dockerfile" <<'DOCKER'
FROM golang:1.27.3-alpine AS go-base
RUN apk add --no-cache git
DOCKER
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the Dockerfile, the line and the verb" \
  "$(contains "$out" 'file=packages/daemon/docker/worker.Dockerfile,line=2::.apk add. fetches inside a Dockerfile RUN with no retry around it')"
check "counts the one refusal" "$(contains "$out" '1 download(s) outside the one retry rule')"

# `apk update` reads Alpine's index from the same CDN, and is the other verb of APK_SUBCOMMANDS
# (dispatch://INFRA-1545, round 64, where a mutant that dropped it passed every case). This RUN
# holds no `apk add`, so the update is the only fetch that can refuse it.
echo "case: a Dockerfile with an unretried apk update is refused, naming the verb"
root=$(fixture bare-apk-update)
cat > "$root/packages/daemon/docker/worker.Dockerfile" <<'DOCKER'
FROM golang:1.27.3-alpine AS go-base
RUN apk update && echo "index read"
DOCKER
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the Dockerfile, the line and the verb" \
  "$(contains "$out" 'file=packages/daemon/docker/worker.Dockerfile,line=2::.apk update. fetches inside a Dockerfile RUN with no retry around it')"
check "counts the one refusal" "$(contains "$out" '1 download(s) outside the one retry rule')"

# Every subcommand the gate's verb sets name, each as a bare RUN of its own: a member dropped from
# APT_SUBCOMMANDS, APK_SUBCOMMANDS, MISE_SUBCOMMANDS, CRANE_SUBCOMMANDS, UV_SUBCOMMANDS,
# NPM_SUBCOMMANDS or BUN_SUBCOMMANDS fails its own row here. The list is written out rather than
# read from the gate, so a set that loses a member cannot take the row with it. Review round 38
# of dispatch://INFRA-1545 found `apk update` covered by no case; ten more members were in round
# 64's search for that class (apt-get upgrade, dist-upgrade, full-upgrade, build-dep and
# reinstall, crane manifest and blob, uv sync and pip, npm install-test).
echo "case: every subcommand of the gate's verb sets is refused bare, each naming its verb"
root=$(fixture every-verb-subcommand)
every_verb=(
  "apt-get update" "apt-get install" "apt-get download" "apt-get upgrade" "apt-get dist-upgrade"
  "apt-get full-upgrade" "apt-get source" "apt-get build-dep" "apt-get reinstall"
  "apk update" "apk add"
  "mise install" "mise use"
  "crane copy" "crane cp" "crane digest" "crane config" "crane pull" "crane export"
  "crane manifest" "crane blob"
  "uv sync" "uv add" "uv pip" "uv tool" "uv python" "uv lock" "uv export"
  "npm install" "npm ci" "npm i" "npm install-test" "npm add"
  "bun install" "bun add" "bun i"
)
# `apt-get update` carries the option rule 5 asks of it, so its one refusal is the missing retry.
{
  echo 'FROM debian:trixie-slim AS build'
  for verb in "${every_verb[@]}"; do
    case $verb in
      "apt-get update") echo "RUN $verb -o APT::Update::Error-Mode=any" ;;
      *) echo "RUN $verb x" ;;
    esac
  done
} > "$root/packages/envoy/docker/Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
line=2
for verb in "${every_verb[@]}"; do
  check "refuses $verb on line $line, naming it" \
    "$(contains "$out" "file=packages/envoy/docker/Dockerfile,line=$line::.$verb. fetches inside a Dockerfile RUN with no retry")"
  line=$((line + 1))
done
check "counts one refusal for each" "$(contains "$out" "${#every_verb[@]} download(s) outside the one retry rule")"

echo "case: the same apk add under the file's inline retry passes"
root=$(fixture canonical-retry-apk)
{
  echo 'FROM golang:1.27.3-alpine AS go-base'
  printf '%s\n' "$CANONICAL"
  echo '    && retry 420 "apk add" apk add --no-cache git'
} > "$root/packages/daemon/docker/worker.Dockerfile"
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
  "$(contains "$out" 'file=packages/envoy/docker/Dockerfile,line=6::.apt-get update. passes no .-o APT::Update::Error-Mode=any.')"

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
check "refuses the RUN at line 2 for the retry() it defines, naming CANONICAL" \
  "$(names_at "$out" '::error file=packages/envoy/docker/Dockerfile,line=2::' 'retry()' 'CANONICAL')"

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

# A comment line or a blank line inside a RUN's continuation is dropped by Docker, which reads
# on to the next line. Until round 37 the gate ended the RUN there, so a fetch below such a
# comment was no site at all: .github/docker/browser.Dockerfile, read that way, was a RUN with no
# fetch in it (dispatch://INFRA-1545, round 37).
echo "case: a Dockerfile RUN's bare fetches written below comment and blank lines of its continuation are refused"
root=$(fixture dockerfile-comment-continuation)
cat > "$root/packages/daemon/docker/worker.Dockerfile" <<'DOCKER'
FROM debian:trixie-slim AS build
RUN set -eu; \
    # The key, fetched bare.
    curl -fsSL -o /tmp/key.asc https://example.com/key.asc; \
    # The index, read bare below a blank line.

    apt-get update -o APT::Update::Error-Mode=any
DOCKER
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the curl below the first comment, on its own line" \
  "$(contains "$out" 'file=packages/daemon/docker/worker.Dockerfile,line=4::.curl. fetches without the floor')"
check "names the apt-get update below the blank line, on its own line" \
  "$(contains "$out" 'file=packages/daemon/docker/worker.Dockerfile,line=7::.apt-get update. fetches inside a Dockerfile RUN with no retry around it')"

echo "case: a retried fetch whose own words run on past a comment line of its continuation passes"
root=$(fixture dockerfile-comment-in-words)
{
  echo 'FROM debian:trixie-slim AS build'
  printf '%s\n' "$CANONICAL"
  cat <<'DOCKER'
    && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends \
      # curl, for the key a later RUN reads.
      curl \
      # Its certificates, for that read's TLS.
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*
DOCKER
} > "$root/packages/envoy/docker/Dockerfile"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
list_check "$root"
check "the install is one site, on the line it starts" \
  "$(contains "$out" '^packages/envoy/docker/Dockerfile	7	apt-get install	-$')"
check "the package named below a comment is no command of its own" \
  "$(is "$(contains "$out" '^packages/envoy/docker/Dockerfile	[0-9]*	curl')" false)"

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
  "$(contains "$out" "file=.github/workflows/case.yaml,line=10::.mise x. in step .Install the worker image.s jj. is retried, and that retry protects nothing")"

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
  "$(contains "$out" 'file=packages/daemon/docker/worker.Dockerfile,line=6::.mise x. is retried, and that retry protects nothing')"
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
check "the install is on the list" "$(contains "$out" 'case.yaml	12	mise install	')"
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
  "$(contains "$out" '^packages/daemon/docker/worker.Dockerfile	6	mise install	-$')"
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
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=9::.curl. in step .Probe the proxy before the install. fetches without the floor')"
check "names the second request" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=12::.curl. in step .Probe the proxy before the install. fetches without the floor')"
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
  "$(contains "$out" "file=.github/workflows/case.yaml,line=12::.apt-get update. in step .Install the packages. passes no .-o APT::Update::Error-Mode=any.")"
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
  "$(contains "$out" '^.github/workflows/case.yaml	12	go mod download	Download the Go modules this job builds$')"
check "a script site carries a dash for the step" \
  "$(contains "$out" '^.github/scripts/report.sh	4	gh api	-$')"
check "the baseline's own retried site is on it" \
  "$(contains "$out" 'playwright install	Install Playwright Chromium$')"
check "an excepted site is not on it" "$(is "$(contains "$out" 'docker pull')" false)"
check "prints nothing but its rows, each a file, a line, a verb and a step, so the count goes elsewhere" \
  "$(is "$(printf '%s\n' "$out" | grep -cvE $'^[^\t]+\t[0-9]+\t[^\t]+\t[^\t]+$')" 0)"

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
check "refuses the curl at line 8 instead, naming the three options it lacks" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' 'Install changie' \
    '--retry 7' '--retry-all-errors' '--max-time <seconds>')"

echo "case: an exception that matches no fetch fails"
root=$(fixture stale-exception)
rm "$root/.github/actions/pull-test-images/action.yml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the exception and what it no longer covers" \
  "$(contains "$out" 'the exception for .github/actions/pull-test-images/action.yml matches no fetch')"

echo "case: one of the Dockerfiles DOCKERFILES names missing fails"
root=$(fixture missing-dockerfile)
rm "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'packages/daemon/docker/worker.Dockerfile is one of the Dockerfiles a CI job builds')"

echo "case: the browser image's Dockerfile missing fails, as the other three do"
root=$(fixture missing-browser-dockerfile)
rm "$root/.github/docker/browser.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" '.github/docker/browser.Dockerfile is one of the Dockerfiles a CI job builds')"

# Rule 10 (dispatch://INFRA-1545, round 37): a step that builds a Dockerfile names one of
# DOCKERFILES, in the two shapes the gate reads, and the key holding it strictly exceeds the
# longest single ladder of that Dockerfile. Main's #2938 added a fourth Dockerfile CI builds,
# .github/docker/browser.Dockerfile, with a bare curl and a bare apt-get, while DOCKERFILES
# named three, and the gate said nothing; its build step's key was 2 minutes, under the 1650 s
# ladder of the package set its RUN now retries.
echo "case: a workflow step building a Dockerfile DOCKERFILES does not name is refused, naming it"
root=$(fixture build-unnamed-dockerfile)
cat > "$root/.github/workflows/image.yaml" <<YAML
name: Image
on: [push]
jobs:
  image:
    runs-on: ubuntu-24.04
    steps:
      - name: Build the other image
        uses: docker/build-push-action@$PIN
        with:
          context: .
          file: docker/other.Dockerfile
      - name: Build it again by hand
        run: docker buildx build --load -f ./docker/other.Dockerfile .
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the action's file, the step and the line" \
  "$(contains "$out" "file=.github/workflows/image.yaml,line=11::step 'Build the other image' of job 'image' builds docker/other.Dockerfile (docker/build-push-action's .file:.), and DOCKERFILES does not name it")"
check "names the run step's file, read from the repository's root" \
  "$(contains "$out" "file=.github/workflows/image.yaml,line=13::step 'Build it again by hand' of job 'image' builds docker/other.Dockerfile (.docker buildx build.'s .--file.)")"
check "says to add it to DOCKERFILES" \
  "$(contains "$out" 'Add docker/other.Dockerfile to DOCKERFILES in .github/scripts/check-download-retry.sh')"
check "counts the two refusals" "$(contains "$out" '2 download(s) outside the one retry rule')"

echo "case: a build of a Dockerfile DOCKERFILES names passes, in either shape and in an action"
root=$(fixture build-named-dockerfile)
mkdir -p "$root/.github/actions/build-images"
cat > "$root/.github/workflows/image.yaml" <<YAML
name: Image
on: [push]
jobs:
  image:
    runs-on: ubuntu-24.04
    steps:
      - name: Build the browser image
        uses: docker/build-push-action@$PIN
        with:
          context: .github/docker
          file: .github/docker/browser.Dockerfile
      - name: Build the worker image by hand
        run: |
          docker build --file=packages/daemon/docker/worker.Dockerfile .
          sudo docker buildx build --load -f ./packages/envoy/docker/Dockerfile .
      - uses: ./.github/actions/build-images
YAML
cat > "$root/.github/actions/build-images/action.yml" <<'YAML'
name: Build the held-port image
description: Builds one image.
runs:
  using: composite
  steps:
    - name: Build the held-port image
      shell: bash
      run: docker buildx build --load --file packages/envoy/infra/host/heldportwrapper/Dockerfile .
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads four Dockerfiles" "$(contains "$out" 'and 4 Dockerfile(s)')"

echo "case: a build whose Dockerfile the gate cannot read as a path is refused by name"
root=$(fixture build-unreadable-dockerfile)
mkdir -p "$root/.github/actions/build-images"
cat > "$root/.github/workflows/image.yaml" <<YAML
name: Image
on: [push]
jobs:
  image:
    runs-on: ubuntu-24.04
    steps:
      - name: Build the chosen image
        uses: docker/build-push-action@$PIN
        with:
          context: .
          file: \${{ inputs.dockerfile }}
      - name: Build the context's own Dockerfile
        uses: docker/build-push-action@$PIN
        with:
          context: .
      - name: Build the chosen image by hand
        run: docker build -f "\$DOCKERFILE" .
      - uses: ./.github/actions/build-images
YAML
cat > "$root/.github/actions/build-images/action.yml" <<'YAML'
name: Build an image
description: Builds one image.
runs:
  using: composite
  steps:
    - name: Build the image here
      shell: bash
      run: docker buildx build --load .
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the expression as written" \
  "$(contains "$out" "file=.github/workflows/image.yaml,line=11::step 'Build the chosen image' of job 'image' builds a Dockerfile (docker/build-push-action's .file:.) and names it as .\${{ inputs.dockerfile }}., which this check cannot read as a path")"
check "names an action step that writes no file" \
  "$(contains "$out" "step 'Build the context's own Dockerfile' of job 'image' builds a Dockerfile (docker/build-push-action's .file:.) and names it as no file at all, so it builds its context's own .Dockerfile.")"
check "names a variable as written" \
  "$(contains "$out" "file=.github/workflows/image.yaml,line=17::step 'Build the chosen image by hand' of job 'image' builds a Dockerfile (.docker build.'s .--file.) and names it as .\$DOCKERFILE.")"
check "names a composite action's build that writes no --file" \
  "$(contains "$out" "file=.github/actions/build-images/action.yml,line=8::step 'Build the image here' of .github/actions/build-images/action.yml builds a Dockerfile (.docker buildx build.'s .--file.) and names it as no file at all")"
check "counts the four refusals" "$(contains "$out" '4 download(s) outside the one retry rule')"

# The browser image's shape: one installing RUN whose longest ladder is the package set's
# `apt-get install`, 5 x 300 + 150 = 1650 s, built by a step under the 2-minute key it carried.
# An empty <step_minutes> writes no key on the build step, so the job's key holds the build alone.
build_key_fixture() {
  local root=$1 step_minutes=$2 job_minutes=$3 step_key=""
  if [ -n "$step_minutes" ]; then
    step_key=$'\n'"        timeout-minutes: $step_minutes"
  fi
  {
    echo 'FROM debian:trixie-slim AS build'
    printf '%s\n' "$CANONICAL"
    cat <<'DOCKER'
    && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*
DOCKER
  } > "$root/.github/docker/browser.Dockerfile"
  cat > "$root/.github/workflows/image.yaml" <<YAML
name: Image
on: [push]
jobs:
  image:
    runs-on: ubuntu-24.04
    timeout-minutes: $job_minutes
    steps:
      - name: Build and push$step_key
        uses: docker/build-push-action@$PIN
        with:
          context: .github/docker
          file: .github/docker/browser.Dockerfile
YAML
}

echo "case: a build step and its job keyed at or under the Dockerfile's longest single ladder are refused, naming both figures"
root=$(fixture build-key-under-ladder)
build_key_fixture "$root" 2 27
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step's key and the ladder" \
  "$(contains "$out" "file=.github/workflows/image.yaml,line=9::step 'Build and push' of job 'image' carries .timeout-minutes: 2., 120 s, and it builds .github/docker/browser.Dockerfile, whose longest single ladder is 1650 s, the .apt-get install. at .github/docker/browser.Dockerfile[:]7")"
check "names the job's key, at the ladder's own minute, and the ladder" \
  "$(contains "$out" "file=.github/workflows/image.yaml,line=6::job 'image' carries .timeout-minutes: 27., 1620 s, and step 'Build and push' of job 'image' builds .github/docker/browser.Dockerfile, whose longest single ladder is 1650 s")"
# Each refusal advises on the key it refuses (dispatch://INFRA-1545, round 74): the step's names
# the step's limit, and the job's names the Budgets note and not the step's limit.
check "advises the step's key with the ladder's seconds in the calculation" \
  "$(names_at "$out" '::error file=.github/workflows/image.yaml,line=9::' \
    'step limit' '1650 s' '1650 s')"
check "advises the job's key from the Budgets note alone" \
  "$(names_at "$out" '::error file=.github/workflows/image.yaml,line=6::' \
    '.github/ci-budgets.md#the-budgets-note' --not 'step limit' "the step's limit")"
check "counts the two refusals" "$(contains "$out" '2 download(s) outside the one retry rule')"

# A build step that writes no key of its own leaves the job's key to hold the build, and the
# job's refusal advises on that key alone (dispatch://INFRA-1545, round 74): until round 74 it
# told the author to allow time in the step's limit, which this step does not write.
echo "case: a job keyed at or under the Dockerfile's longest single ladder, whose build step writes no key, is refused at the job's key alone"
root=$(fixture build-key-job-only)
build_key_fixture "$root" "" 27
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the job's key at line 6, naming the job, its key, the ladder and the Budgets note, and not the step's limit" \
  "$(names_at "$out" '::error file=.github/workflows/image.yaml,line=6::' "job 'image'" \
    '`timeout-minutes: 27`' '1650 s' '.github/ci-budgets.md#the-budgets-note' \
    --not 'step limit' "the step's limit")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

echo "case: a build step and its job keyed above the Dockerfile's longest single ladder pass"
root=$(fixture build-key-above-ladder)
build_key_fixture "$root" 28 29
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a workflow that cannot be parsed as YAML"
root=$(fixture bad-yaml)
printf 'name: Case\non: [push\n' > "$root/.github/workflows/case.yaml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the file at its line 1, with the parser's position of the unclosed [push, line 2, column 5" \
  "$(is "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=1::')$(contains "$out" 'line 2, column 5:')" truetrue)"
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
# The figures a refusal prints are what its reader writes next, so each is pinned exactly: a
# `cost // 60 + 1` turned into `/` prints `6.0`, which a check for `timeout-minutes: 6` alone
# also matches (dispatch://INFRA-1545, round 33).
check "says the budget in seconds" "$(contains "$out" 'carries .timeout-minutes: 5., 300 s, and its own retries take 300 s')"
check "says exactly what to write" "$(contains "$out" 'so write .timeout-minutes: 6. or more\.')"
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
# a budget of 16 minutes. This fixture's `sleep 10` is a command of its own, which bash runs
# after every pass whose read did not break the loop, the seventh included: where every read
# exhausts its attempts, 7 x 300 + 7 x 10 = 2170 s (dispatch://INFRA-1545, round 44: until then
# it was charged 2160 s, its sleep once fewer times than the passes).
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
  "$(contains "$out" 'retries take 2170 s to exhaust')"
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

# A loop over `$(seq …)` is a count this check does not read, since the tree writes none around a
# priced call, so it is refused rather than multiplied (dispatch://INFRA-1545, round 37).
echo "case: a loop over seq is a loop whose passes this check does not read, and is refused"
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
check "refuses the loop by name, at the site's line and the loop's" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Read each page of the route' carries .timeout-minutes: 15. and this check can put no figure on it: it runs a retried call inside a loop whose passes it cannot read — .gh api. at .github/workflows/case.yaml:12, inside the loop at line 11\.")"

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
  "$(contains "$out" '.gh api. at .github/workflows/case.yaml:12, inside the loop at line 11')"
check "says what one call of it costs" "$(contains "$out" 'One call there takes 300 s')"
check "offers the step at line 8 a literal pass count, or a DeclaredArithmetic entry of the gate's DECLARED_LOOPS" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' 'for x in 1 2 3 4 5 6 7' \
    DECLARED_LOOPS DeclaredArithmetic .github/scripts/check-download-retry.sh)"
check "refuses nothing else, so it judges no budget against a figure it is missing" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
check "reads the call's bound at line 8 as its number, 300 s, asking for no retry_fetch seconds" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' '300 s' --not retry_fetch)"

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
  "$(contains "$out" '..github/scripts/c.sh. at .github/workflows/case.yaml:11, inside the loop at line 10')"
check "says one run of that script reaches an uncounted loop" \
  "$(contains "$out" 'One run of ..github/scripts/c.sh. reaches a loop, its own or one in a script it calls, whose passes this check cannot read either')"
check "prints no figure for a cost it cannot compute" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"
check "offers the script call at line 8 a literal pass count and no declaration in the gate" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' 'for x in 1 2 3 4 5 6 7' \
    DECLARED_LOOPS --not DeclaredArithmetic .github/scripts/check-download-retry.sh)"

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
  "$(contains "$out" '..github/scripts/c2.sh. at .github/workflows/case.yaml:11, inside the loop at line 10')"
check "names the site in the second script that stopped the reading" \
  "$(contains "$out" '.gh api. at .github/scripts/d.sh:5, inside the loop at line 4')"
check "says the loop one run reaches may sit in a script that script calls" \
  "$(contains "$out" 'One run of ..github/scripts/c2.sh. reaches a loop, its own or one in a script it calls')"
check "places the uncounted loop at line 8's refusal in d.sh, and none in c2.sh" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' .github/scripts/d.sh:5 \
    --not .github/scripts/c2.sh:)"

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
          printf 'a\nb\n' | while read -r id; do
            .github/scripts/zz-e.sh "$id"
          done
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "prices no run of that script at nothing" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"

# One layer deeper: the loop sits in a script the step runs, around a call of the costless
# one, so the item blocked_calls lists is that inner call rather than the step's own. The one
# reading drops it there too, because the step's list and the lists of every script and
# action it reaches take the same filter (review round 5 of dispatch://INFRA-1545). What leaves the step
# with a figure rather than none is shell_arithmetic(), which charges nothing for a call inside
# an uncounted loop where the script it reaches, with what that script calls, costs 0 s; the case
# below holding a priced script beside such a loop is the one that pins that answer.
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

# A script this check cannot price is not a script that retries nothing. A curl making its own
# attempts passes rule 1 on the options it writes, and `--max-time "$bound"` is one of them:
# the flag is there, so each attempt is bounded, and the seconds are not, so one_call reads no
# figure off the call. Answering 0 for that made the script read as one holding no retry at
# all, and the filter above dropped the loop around it, where the fetch is real and the step
# waits for the whole of it once per pass (review round 6 of dispatch://INFRA-1545).
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
          printf 'a\nb\n' | while read -r id; do
            .github/scripts/zz-i.sh "$id"
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the script call and the loop around it" \
  "$(contains "$out" '..github/scripts/zz-i.sh. at .github/workflows/case.yaml:11, inside the loop at line 10')"
check "names at line 8 the script and the kind of its bound, a variable" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' .github/scripts/zz-i.sh variable)"
check "prices no run of that script at nothing" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"

# The third member of that family, and the one the two cases above leave open: the site whose
# bound this check cannot read, written in the body's own loop rather than in a script the
# loop runs. One call of it has no figure — `--max-time "$bound"` bounds the attempt where the
# seconds are not written — and the loop around it has no pass count, so both halves of what
# the step waits for are unread. uncounted_of dropped it with the sites that cost nothing,
# because `one is None` was read as "this item is a script call" and a site with no figure had
# no sentence of its own to be refused with, so rule 8 priced the step's whole wait at 0 and
# refused nobody for it (review round 8 of dispatch://INFRA-1545).
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
  "$(contains "$out" '.curl. at .github/workflows/case.yaml:11, inside the loop at line 14')"
check "says what one call of it takes is a figure it cannot make" \
  "$(contains "$out" 'What one call of .curl. there takes is a figure this check cannot make, because its bound is written as a variable rather than as a number')"
check "prices the site at nothing nowhere" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"
check "offers the site at line 8 a DECLARED_LOOPS entry in the gate, with no pass count, DeclaredArithmetic or retry_fetch seconds" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' DECLARED_LOOPS \
    .github/scripts/check-download-retry.sh --not 'for x in 1 2 3 4 5 6 7' DeclaredArithmetic retry_fetch)"

# A site with no bound at all, in that same loop, is not a site whose bound is a variable. Rule 8
# kept it, rightly, since the loop is its own fault and rule 1 says nothing of a loop, but it
# told the author the bound was written as a variable and to write that bound as a number,
# when the curl writes no `--max-time` at all (dispatch://INFRA-1545, round 37). It now says it
# finds no bound, asks for one as a number, and then for the count, since a bound alone leaves
# the loop unread. The second step is the control: a `retry_fetch` whose seconds are `"$T"`
# around a curl whose `--max-time` is `"$T"` writes its bound as a variable, and is told so.
echo "case: a loop around the step's own curl with no bound names no variable bound"
root=$(fixture site-no-bound-in-loop)
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
            curl -fsSL --retry 7 --retry-all-errors \
              -o "run-$1.json" "https://api.github.com/repos/$GITHUB_REPOSITORY/actions/runs/$1"
          }
          while read -r id; do
            read_run "$id"
          done <<<"$ids"
      - name: Fetch every row at the caller's bound
        timeout-minutes: 5
        run: |
          . .github/scripts/ci-retry.sh
          while read -r url; do
            retry_fetch "a row" "$T" -- curl -fsSL --max-time "$T" -o f "$url"
          done < urls.txt
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "leaves rule 1 refusing the curl on its own line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::.curl. in step 'Read every run the decision named' fetches without the floor: it lacks .--max-time <seconds>.")"
check "refuses the step for the loop it cannot count" \
  "$(contains "$out" "step .Read every run the decision named. carries .timeout-minutes: 30. and this check can put no figure on it: it runs a retried call inside a loop whose passes it cannot read — .curl. at .github/workflows/case.yaml:11, inside the loop at line 14")"
check "says it finds no bound on one attempt of the call" \
  "$(contains "$out" "step .Read every run the decision named. .*What one call of .curl. there takes is a figure this check cannot make, because this check finds no bound on one attempt of it, and the step waits for that once per pass")"
check "does not say that call's bound is written as a variable" \
  "$(is "$(contains "$out" "step .Read every run the decision named. .*written as a variable")" false)"
check "asks for a bound written as a number, as a sentence of its own" \
  "$(contains "$out" "step .Read every run the decision named. .*it exists to allow\. Bound each attempt with a number of seconds — .--max-time <seconds>. on a curl, or the seconds .retry_fetch. takes before its .--. — which is the figure this check multiplies by the passes it reads\. Write the count where this check reads it")"
check "does not ask it to write as a number a bound it does not write" \
  "$(is "$(contains "$out" "step .Read every run the decision named. .*[Ww]rite that bound as a number")" false)"
check "tells the control its bound is written as a variable" \
  "$(contains "$out" "step .Fetch every row at the caller's bound. .*because its bound is written as a variable rather than as a number, .*it exists to allow\. Write that bound as a number")"

# The same false premise one script in: a script whose curl writes no bound, run in a loop the
# check cannot count, was said to make 'attempts whose bound is written as a variable'
# (dispatch://INFRA-1545, round 37). The second script holds one call of each kind, and is told
# of both.
echo "case: a loop around a script whose curl has no bound names no variable bound"
root=$(fixture script-no-bound-in-loop)
cat > "$root/.github/scripts/zz-nobound.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
curl -fsSL --retry 7 --retry-all-errors -o run.json \
  "https://api.github.com/repos/$GITHUB_REPOSITORY/actions/runs/$1"
SH
cat > "$root/.github/scripts/zz-mixed.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
bound=60
curl -fsSL --retry 7 --retry-all-errors --max-time "$bound" -o run.json \
  "https://api.github.com/repos/$GITHUB_REPOSITORY/actions/runs/$1"
curl -fsSL --retry 7 --retry-all-errors -o jobs.json \
  "https://api.github.com/repos/$GITHUB_REPOSITORY/actions/runs/$1/jobs"
SH
chmod +x "$root/.github/scripts/zz-nobound.sh" "$root/.github/scripts/zz-mixed.sh"
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
          printf 'a\nb\n' | while read -r id; do
            .github/scripts/zz-nobound.sh "$id"
          done
      - name: Read every run and its jobs
        timeout-minutes: 1
        run: |
          printf 'a\nb\n' | while read -r id; do
            .github/scripts/zz-mixed.sh "$id"
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "leaves rule 1 refusing the script's curl on its own line" \
  "$(contains "$out" "::error file=.github/scripts/zz-nobound.sh,line=3::.curl. fetches without the floor: it lacks .--max-time <seconds>.")"
check "names the script call and the loop around it" \
  "$(contains "$out" '..github/scripts/zz-nobound.sh. at .github/workflows/case.yaml:11, inside the loop at line 10')"
check "says it finds no bound on that script's attempts" \
  "$(contains "$out" 'One run of ..github/scripts/zz-nobound.sh. makes attempts on which this check finds no bound, there or in a script it calls')"
check "does not say that script's bound is written as a variable" \
  "$(is "$(contains "$out" "step .Read every run the decision named. .*written as a variable")" false)"
check "names both kinds in the script holding both" \
  "$(contains "$out" 'One run of ..github/scripts/zz-mixed.sh. makes attempts whose bound is written as a variable rather than as a number and attempts on which this check finds no bound, there or in a script it calls')"

# The fourth member of that family, and the one with no loop anywhere in it. A curl whose
# `--max-time` is written as a variable is bounded, so rule 4 passes it, and the seconds are
# the caller's, so one_call answers None: the passes here are plainly one, and one times a
# figure nobody can read is still no figure. Round 8 answered 0 for the site and the step
# passed under any budget at all, which is how every tool install fetching through
# .github/scripts/ci-tools.sh read green at a minute (review round 9 of dispatch://INFRA-1545).
echo "case: a step whose curl writes its bound as a variable, in no loop, is refused"
root=$(fixture site-unread-bound-no-loop)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the tool this job runs
        timeout-minutes: 1
        run: |
          bound=60
          curl -fsSL --retry 7 --retry-all-errors --max-time "$bound" \
            -o tool.tar.gz https://example.com/tool.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and the site whose bound it cannot read" \
  "$(contains "$out" "step .Install the tool this job runs. carries .timeout-minutes: 1. and this check can put no figure on it: it runs a retried call whose attempts are bounded by a variable rather than by a number — .curl. at .github/workflows/case.yaml:11")"
check "prices the step at nothing nowhere" \
  "$(is "$(contains "$out" 'holds 0 s of retries')" false)"

# And the declaration answers it, with no loop for the entry to name: the figure is the whole
# of what the site contributes, since there is no per-call number for a pass count to
# multiply. The entry is counted where it is read, so this check refuses it for matching
# nothing no more than it refuses the entries that answer a loop.
echo "case: a declaration answers that site, and is counted"
root=$(fixture site-unread-bound-declared)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the tool this job runs
        timeout-minutes: 31
        run: |
          bound=60
          curl -fsSL --retry 7 --retry-all-errors --max-time "$bound" \
            -o tool.tar.gz https://example.com/tool.tar.gz
YAML
declare_entry "$root" '    DeclaredArithmetic(".github/workflows/case.yaml", "Install the tool this job runs", "curl", 1821, "the three fetches one failing install makes, at 8 x 60 + 127 = 607 s each: 3 x 607 = 1821 s"),'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# The entry's seconds are the site's contribution and not a note beside it: the budget is held
# above that figure, and a budget under it is refused with it.
echo "case: the declared seconds are what that step's budget is held above"
root=$(fixture site-unread-bound-declared-under)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the tool this job runs
        timeout-minutes: 30
        run: |
          bound=60
          curl -fsSL --retry 7 --retry-all-errors --max-time "$bound" \
            -o tool.tar.gz https://example.com/tool.tar.gz
YAML
declare_entry "$root" '    DeclaredArithmetic(".github/workflows/case.yaml", "Install the tool this job runs", "curl", 1821, "the three fetches one failing install makes, at 8 x 60 + 127 = 607 s each: 3 x 607 = 1821 s"),'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "holds the budget above the declared figure" \
  "$(contains "$out" 'carries .timeout-minutes: 30., 1800 s, and its own retries take 1821 s to exhaust')"

# The floor an entry is held to is one call of the site it covers, and this site has none:
# one_call answered None, which is the absence of the very figure that comparison reads. An
# entry below one call of a site that has one is still refused, which the case for that floor
# elsewhere proves; here there is nothing to compare and the entry stands on its own `why`.
echo "case: no floor is compared with an entry answering a bound nobody can read"
root=$(fixture site-unread-bound-declared-small)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the tool this job runs
        timeout-minutes: 2
        run: |
          bound=60
          curl -fsSL --retry 7 --retry-all-errors --max-time "$bound" \
            -o tool.tar.gz https://example.com/tool.tar.gz
YAML
declare_entry "$root" '    DeclaredArithmetic(".github/workflows/case.yaml", "Install the tool this job runs", "curl", 60, "one attempt of one fetch, which is all this fixture claims"),'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

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
check "prices no call of that loop at nothing" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"

# The filter's question asked one layer in: the step's loop runs a script whose own work is a
# loop this check cannot count over a script that fetches nothing. No attempt waits under
# either loop, so neither is a loop rule 8 has a pass count to ask the step's author for.
# Reading the question as "the script holds no uncounted item of its own" refused this with
# the `0 s` sentence, because the inner call is an item of the script the step runs
# (review round 6 of dispatch://INFRA-1545).
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
          printf 'a\nb\n' | while read -r id; do
            .github/scripts/zz-k.sh "$id"
          done
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "prices no run of either script at nothing" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"

# A script this check does price, holding beside that ladder a loop it cannot count around a
# costless script. The ladder is 450 s the step waits for and the costless loop takes nothing
# off it, so the budget is held to the 450 s. shell_arithmetic() charging nothing for an
# uncounted call of a script that costs 0 s is what keeps that figure readable: without it the
# costless call answers None, the whole script's arithmetic answers None with it, and the step is
# compared with nothing and passes in silence (review round 6 of dispatch://INFRA-1545).
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
check "refuses nothing else, so the costless loop asks the step for no pass count" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

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
          printf 'a\nb\n' | while read -r id; do
            .github/scripts/zz-f.sh "$id"
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the script call and the loop around it" \
  "$(contains "$out" '..github/scripts/zz-f.sh. at .github/workflows/case.yaml:11, inside the loop at line 10')"
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
  "$(contains "$out" '.gh api. at .github/workflows/case.yaml:11, inside the loop at line 10')"

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
  "$(contains "$out" 'the declared arithmetic for .github/scripts/mirror-ci-images.sh .crane copy. matches no retried call this check could not price of itself')"
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
check "refuses nothing else, so the site is not refused for want of a figure" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

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
  "$(contains "$out" 'writes 200 s, and one call of that site at .github/workflows/case.yaml:12 takes 300 s')"

# The shape that reached no comparison at all: a step whose whole arithmetic is a composite
# action's, carrying no budget of its own. `.github/workflows/ci.yaml#airlock-lean` held
# install-elan's 8 x 60 + 127 = 607 s ladder that way, under a job budget with no term for it, and
# this check built a budget only for a step that wrote one.
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
check "says exactly what to write" "$(contains "$out" 'write .timeout-minutes: 11. or more on this step')"

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
check "says what to write on the job, above the step's own key of 8 as well" \
  "$(contains "$out" 'timeout-minutes: 9. or more on this job')"

echo "case: the same job with a budget above that step passes"
root=$(fixture job-budget-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 0 s measured, plus its slowest retried step alone, the changie tarball's
    # 5 x 60 + 150 = 450 s -> 9 min.
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

# Rule 8's `<=`, at the job ceiling against a step's arithmetic: a job whose budget EQUALS that
# arithmetic is refused. Every other fixture of this clause leaves a gap of seconds between the
# two, where `<` and `<=` answer alike, so this one is what refuses a revert of the comparison
# (dispatch://INFRA-1545, round 31). 5 x 90 + 150 = 600 s, which is 10 minutes exactly.
echo "case: a job whose budget equals one of its steps' retries to the second is refused"
root=$(fixture job-budget-equal)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 10
    steps:
      - name: Install changie
        timeout-minutes: 11
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the changie tarball" 90 -- \
            curl -fsSL --max-time 90 -o changie.tar.gz https://example.com/changie.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the job at 600 s against a step whose retries take 600 s" \
  "$(contains "$out" "job .build. carries .timeout-minutes: 10., 600 s, and its step .Install changie. holds retries taking 600 s to exhaust, which that budget does not strictly exceed")"
check "says what to write on the job, above the step's own key of 11 as well" \
  "$(contains "$out" 'timeout-minutes: 12. or more on this job')"

# The job half of the same rule, which the clause above cannot reach: a step whose retries fit
# inside its own budget is still cut by a job ceiling that is not above that budget, because the
# job holds the step AND everything else it runs. Twenty-two job keys of this repository stood
# below their own documented derivation while this check was green (dispatch://INFRA-1545,
# round 27), every one of them past the clause above.
echo "case: a job whose budget equals its slowest step's own budget is refused"
root=$(fixture job-holds-step)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 8
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
check "names the job's budget and the step's own" \
  "$(contains "$out" "and its step .Install changie. carries .timeout-minutes: 8.")"
check "says what to write on the job" "$(contains "$out" 'timeout-minutes: 9. or more on this job')"
check "does not also report the step's own retries against this job" \
  "$(is "$(printf '%s\n' "$out" | grep -c 'holds retries taking')" 0)"

# The figure the refusal of a job under its step's arithmetic advises is a key the job passes
# with (dispatch://INFRA-1545, round 37). That refusal is reached only where the step's own key
# already exceeds the step's arithmetic, so the arithmetic's own minute, which it advised, was
# never above the step's key: written as advised, 8 under a step key of 8, the job was refused
# for not exceeding its step's key. It advises the larger of the two minimums now, and the case
# writes the job at the figure the refusal names, with the comment pricing it, which passes.
echo "case: a job written at the figure its arithmetic refusal advises passes"
root=$(fixture job-budget-advice-followed)
write_advice_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: the changie tarball's ladder alone, 450 s -> $1 min.
    timeout-minutes: $1
    steps:
      - name: Install changie
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the changie tarball" 60 -- curl -fsSL --max-time 60 -o changie.tar.gz https://example.com/changie.tar.gz
YAML
}
write_advice_case 5
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the job under its step's arithmetic" \
  "$(contains "$out" "job .build. carries .timeout-minutes: 5., 300 s, and its step .Install changie. holds retries taking 450 s to exhaust")"
advised=$(grep -o 'Write .timeout-minutes: [0-9]*. or more on this job' <<<"$out" | grep -o '[0-9][0-9]*' || true)
check "advises the least key above both the arithmetic and the step's key of 8, which is 9" \
  "$(is "$advised" 9)"
write_advice_case "${advised:-0}"
run_check "$root"
check "with the job written at the figure advised, passes" "$(is "$status" 0)"
check "with the job written at the figure advised, refuses nothing" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 0)"

# The same advice reads every retried step of the job, not the refused one alone
# (dispatch://INFRA-1545, round 37). The step-key refusal holds the job above each of its retried
# steps' keys, and the advice read the refused step's key alone: under a job key of 7, 'Fetch the
# index' at 450 s keyed at 8 is refused for its arithmetic, and the job was told the larger of 8
# and 9, so 9, which 'Fetch the list''s key of 20 refuses. It is told 21 now, and the job written
# there passes. The class search's inputs H and H3.
echo "case: a job written at the figure its arithmetic refusal advises passes, above a longer key of another step"
root=$(fixture job-budget-advice-longest-key)
write_longest_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: above the index's ladder and the list step's own key -> $1 min.
    timeout-minutes: $1
    steps:
      - name: Fetch the index
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the index" 60 -- gh api repos/o/r
      - name: Fetch the list
        timeout-minutes: 20
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the list" 30 -- gh api repos/o/r/pulls
YAML
}
write_longest_case 7
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the job under the index step's arithmetic" \
  "$(contains "$out" "job .build. carries .timeout-minutes: 7., 420 s, and its step .Fetch the index. holds retries taking 450 s to exhaust")"
advised=$(grep 'holds retries taking 450 s' <<<"$out" | grep -o 'Write .timeout-minutes: [0-9]*. or more on this job' | grep -o '[0-9][0-9]*' || true)
check "advises the least key above the arithmetic and the list step's key of 20, which is 21" \
  "$(is "$advised" 21)"
check "names the step whose key is the job's longest" \
  "$(contains "$out" "holds retries taking 450 s .*The job's key must also strictly exceed the .timeout-minutes: 20. of its step 'Fetch the list'")"
first_refusal=$(grep -m1 '^::error' <<<"$out" || true)
check "keeps the refusal first among the two at the job's key" \
  "$(contains "$first_refusal" 'holds retries taking 450 s')"
write_longest_case "${advised:-0}"
run_check "$root"
check "with the job written at the figure advised, passes" "$(is "$status" 0)"
check "with the job written at the figure advised, refuses nothing" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 0)"

# The step-key refusal's advice reads every retried step of the job too (dispatch://INFRA-1545,
# round 37). Under a job key of 10, steps keyed at 20 and 30, each above its own 450 s and 300 s,
# are each refused for the job's key, and the first was told 21, which the second's key refuses.
# Both are told 31 now, the first naming the step whose key is 30, and the job written there
# passes.
echo "case: a job written at the figure its step-key refusals advise passes, above the longest key"
root=$(fixture job-step-key-advice-longest)
write_step_keys_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: above the longest of its steps' own keys -> $1 min.
    timeout-minutes: $1
    steps:
      - name: Fetch the index
        timeout-minutes: 20
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the index" 60 -- gh api repos/o/r
      - name: Fetch the list
        timeout-minutes: 30
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the list" 30 -- gh api repos/o/r/pulls
YAML
}
write_step_keys_case 10
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the job at the index step's key of 20" \
  "$(contains "$out" "job 'build' carries .timeout-minutes: 10., and its step 'Fetch the index' carries .timeout-minutes: 20., which that job budget does not strictly exceed")"
check "refuses the job at the list step's key of 30" \
  "$(contains "$out" "job 'build' carries .timeout-minutes: 10., and its step 'Fetch the list' carries .timeout-minutes: 30., which that job budget does not strictly exceed")"
advised=$(grep -o 'Write .timeout-minutes: [0-9]*. or more on this job' <<<"$out" | grep -o '[0-9][0-9]*' | sort -u | tr '\n' ' ' || true)
check "advises 31 in both refusals, the first minute above the longest key" "$(is "$advised" "31 ")"
check "tells the index step's refusal of the list step's longer key" \
  "$(contains "$out" "its step 'Fetch the index' carries .timeout-minutes: 20.,.*The job's key must also strictly exceed the .timeout-minutes: 30. of its step 'Fetch the list'")"
first_refusal=$(grep -m1 '^::error' <<<"$out" || true)
check "keeps the refusals in the order of their steps" \
  "$(contains "$first_refusal" "its step 'Fetch the index' carries")"
write_step_keys_case 31
run_check "$root"
check "with the job written at the figure advised, passes" "$(is "$status" 0)"
check "with the job written at the figure advised, refuses nothing" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 0)"

# The job-derivation clause's advice is a key the job passes with too (dispatch://INFRA-1545,
# round 37). It named the first minute above the sum it compares, and a job written there can sit
# at a step's own key, which the step-budget loop refuses a job for. Here the step's key is 8
# minutes and its retries take 450 s. A comment deriving 0 + 450 = 450 s over a job key of 5 was
# told 8, and one deriving 0 + 300 = 300 s over a key of 6, under the step's 450 s, was told 8 as
# well; each is told 9 now, the first minute above the step's key, and the job written at the
# figure the clause names, with its comment deriving the step's 450 s, passes.
echo "case: a job written at the figure the job-derivation clause advises passes"
root=$(fixture job-derivation-advice-followed)
write_derivation_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: nothing measured, plus the changie tarball's ladder: 0 + $1 = $1 s -> $2 min.
    timeout-minutes: $2
    steps:
      - name: Install changie
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the changie tarball" 60 -- curl -fsSL --max-time 60 -o changie.tar.gz https://example.com/changie.tar.gz
YAML
}
write_derivation_case 450 5
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the job under the sum its comment derives" \
  "$(contains "$out" "job 'build' carries .timeout-minutes: 5., 300 s, and its own comment derives 0 + 450 = 450 s, which that key does not strictly exceed")"
advised=$(grep 'which that key does not strictly exceed' <<<"$out" | grep -o 'Write .timeout-minutes: [0-9]*. or more, or correct the derivation' | grep -o '[0-9][0-9]*' || true)
check "advises the least key above both that sum and the step's key of 8, which is 9" \
  "$(is "$advised" 9)"
check "says the job's key must exceed the step's own key" \
  "$(contains "$out" "inside its own budget\. The job's key must also strictly exceed the .timeout-minutes: 8. of its step 'Install changie'")"
write_derivation_case 300 6
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the job under the sum its slowest step makes" \
  "$(contains "$out" "job 'build' carries .timeout-minutes: 6., 360 s, and its own comment derives 0 + 300 = 300 s, but the slowest retried step of this job is 'Install changie', whose retries take 450 s")"
readvised=$(grep 'SELF-CONSISTENT AND STILL WRONG' <<<"$out" | grep -o 'Write .timeout-minutes: [0-9]*. or more\.$' | grep -o '[0-9][0-9]*' || true)
check "advises the least key above both the step's sum and its key of 8, which is 9" \
  "$(is "$readvised" 9)"
write_derivation_case 450 "${advised:-0}"
run_check "$root"
check "with the job written at the figure advised, passes" "$(is "$status" 0)"
check "with the job written at the figure advised, refuses nothing" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 0)"

# The job-derivation clause, which reads a job key against THE SUM ITS OWN COMMENT STATES and
# recomputes the second term from this check's own step arithmetic. Its fixtures are the shapes
# that earned it: a comment naming a step that stopped being the slowest (eight jobs of this
# repository, dispatch://INFRA-1545 round 28), a key under the sum its own comment states
# (`download-retry`, 221 minutes against its own 14322 s), and the job it must NOT refuse — one
# whose comment derives its key by a construction rather than by a sum, which a clause that
# assumed first-plus-slowest would have refused. `tool-fallback`'s comment writes such a
# construction and, beside it, the sum the clause reads.
echo "case: a job whose comment names a step that is no longer its slowest is refused"
root=$(fixture job-derivation)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus its slowest retried step alone — the small fetch,
    # 450 s. 1200 + 450 = 1650 s -> 28 min.
    timeout-minutes: 28
    steps:
      - name: The small fetch
        timeout-minutes: 9
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the sum the comment states" \
  "$(contains "$out" "its own comment derives 1200 + 450 = 1650 s")"
check "names the step that is really the slowest, with this check's own figure" \
  "$(contains "$out" "slowest retried step of this job is .The large fetch., whose retries take 1050 s")"
check "says the sum this check computes" "$(contains "$out" "1200 + 1050 = 2250 s")"
check "says what the key must be" "$(contains "$out" 'timeout-minutes: 38. or more')"

echo "case: the same job with the derivation and the key corrected passes"
root=$(fixture job-derivation-fixed)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus its slowest retried step alone — the large fetch,
    # 1050 s. 1200 + 1050 = 2250 s -> 38 min.
    timeout-minutes: 38
    steps:
      - name: The small fetch
        timeout-minutes: 9
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a job whose key is under the sum its own comment states is refused"
root=$(fixture job-derivation-half-a)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus its slowest retried step alone — the large fetch,
    # 1050 s. 1200 + 1050 = 2250 s -> 30 min.
    timeout-minutes: 30
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the sum and says the key does not strictly exceed it" \
  "$(contains "$out" "its own comment derives 1200 + 1050 = 2250 s, which that key does not strictly exceed")"
check "says what to write" "$(contains "$out" 'timeout-minutes: 38. or more')"

# Rule 8's `<=` at the derivation clause's two halves: a key EXACTLY at the sum its comment
# states, and a key exactly at the sum this check recomputes. The fixtures above use
# 1200 + 1050 = 2250 s, which is 37.5 minutes, where `<` and `<=` answer alike, so a revert of
# either comparison left every case green (dispatch://INFRA-1545, round 31). 1350 + 1050 =
# 2400 s is 40 minutes exactly, the shape of the eleven keys round 29 raised.
echo "case: a job whose key equals the whole-minute sum its own comment states is refused"
root=$(fixture job-derivation-equal)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1350 s measured, plus its slowest retried step alone — the large fetch,
    # 1050 s. 1350 + 1050 = 2400 s -> 40 min.
    timeout-minutes: 40
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the sum and says the key does not strictly exceed it" \
  "$(contains "$out" "its own comment derives 1350 + 1050 = 2400 s, which that key does not strictly exceed")"
check "says what to write" "$(contains "$out" 'timeout-minutes: 41. or more')"

echo "case: a job whose key equals the whole-minute sum this check recomputes is refused"
root=$(fixture job-derivation-recomputed-equal)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1350 s measured, plus its slowest retried step alone — the small fetch,
    # 450 s. 1350 + 450 = 1800 s -> 40 min.
    timeout-minutes: 40
    steps:
      - name: The small fetch
        timeout-minutes: 9
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the recomputed sum and says the key does not strictly exceed it" \
  "$(contains "$out" "1350 + 1050 = 2400 s, which this key does not strictly exceed")"
check "says what the key must be" "$(contains "$out" 'timeout-minutes: 41. or more')"

# The derivation clause reads the sum whose arrow names the job's own key, and no other sum of
# its block. A block may derive the job and a step together, and a reviewer made the clause take
# the block's last sum whatever its arrow names (`if True:` in place of the arrow's test against
# the key): it read the step's 1663 s, under the job's 2280 s, and the job's own 2300 s, which
# 38 minutes does not exceed, went unread (dispatch://INFRA-1545, review round 28).
echo "case: of two derivations in a block, the one whose arrow names the key is read"
root=$(fixture job-derivation-arrow-names-key)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 650 + 1650 = 2300 s -> 38 min. The step: 13 + 1650 = 1663 s -> 28 min.
    timeout-minutes: 38
    steps:
      - name: The browser download
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a browser" 300 -- curl -fsSL --max-time 300 -o c https://example.com/c
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "reads the sum whose arrow is the job's key" \
  "$(contains "$out" "job 'build' carries .timeout-minutes: 38., 2280 s, and its own comment derives 650 + 1650 = 2300 s, which that key does not strictly exceed")"
check "says what to write" "$(contains "$out" 'timeout-minutes: 39. or more')"

# Of two sums whose arrows both name the key, the clause reads the last, the one written nearest
# the key. A `break` after the first match made it read the first, 2250 s, and the job went
# through under its second, 2350 s (dispatch://INFRA-1545, review round 28).
echo "case: of two derivations naming the key, the one nearest the key is read"
root=$(fixture job-derivation-last-of-two)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget, as first measured: 600 + 1650 = 2250 s -> 38 min.
    # As measured since the checkout slowed: 700 + 1650 = 2350 s -> 38 min.
    timeout-minutes: 38
    steps:
      - name: The browser download
        # The browser download's 5 x 300 + 150 = 1650 s -> 28 min.
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a browser" 300 -- curl -fsSL --max-time 300 -o c https://example.com/c
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "reads the second sum" \
  "$(contains "$out" "its own comment derives 700 + 1650 = 2350 s, which that key does not strictly exceed")"
check "says what to write" "$(contains "$out" 'timeout-minutes: 40. or more')"

# The clause reads its block upwards from the key and joins it back in reading order, so a sum
# split over two lines reads as one. Joined in the order the walk collects it, the block below
# reads `1750 s -> 28 min. 700 + 1050 =`, which is no derivation, and a key under its own sum
# went through (dispatch://INFRA-1545, review round 28, the mutation that dropped `reversed`).
echo "case: a derivation split over two comment lines is read in order"
root=$(fixture job-derivation-split)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 700 s measured, plus the large fetch's 1050 s:
    # 700 + 1050 =
    # 1750 s -> 28 min.
    timeout-minutes: 28
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "reads the sum the two lines state" \
  "$(contains "$out" "its own comment derives 700 + 1050 = 1750 s, which that key does not strictly exceed")"
check "says what to write" "$(contains "$out" 'timeout-minutes: 30. or more')"

# The stand-aside, which is the clause's whole design: a comment that derives its key by a
# construction, with no `<a> + <b> = <c> s -> <key> min` tying a sum to that key, is passed over,
# and a clause that refused it would be a correct job refused — which is how a check gets
# silenced (the case "a job whose comment derives its key another way is passed over" below).
# `tool-fallback` of this repository derives its key both ways: .github/ci-budgets.md states its
# construction, `114 was the 104 this construction gives with ten minutes added`, and the comment
# above its key also writes `1727 + 12383 = 14110 s -> 236 min`, which the clause reads.
# The arrow rule, which is this issue's own control moved into the check: a key lowered while
# its comment is left alone. Before it, each of the nine keys dispatch://INFRA-1545 raised could
# be put back with its sentence untouched and this check stayed green, because the derivation
# clause below finds no sum once the arrow and the key disagree, and stands aside.
echo "case: a key lowered while its comment is left alone is refused"
root=$(fixture arrow-against-key)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus its slowest retried step alone — the large fetch,
    # 1050 s. 1200 + 1050 = 2250 s -> 38 min.
    timeout-minutes: 30
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the figure the comment prices and the key beneath it" \
  "$(contains "$out" "this comment prices what it sits above at 38 min, and the .timeout-minutes: 30. below it, the key of job 'build', carries 30")"
check "points at the comment's first line, where a reader rewrites the sentence" \
  "$(contains "$out" "file=.github/workflows/case.yaml,line=6::this comment prices")"

echo "case: a comment block pricing two steps keeps both keys"
root=$(fixture arrow-two-steps)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  # Budget: 1350 s measured, plus the large fetch's 5 x 180 + 150 = 1050 s -> 40 min.
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 40
    steps:
      # Two steps of this job, at the bounds their own arithmetic gives: the large fetch's
      # 5 x 180 + 150 = 1050 s -> 18 min, and the small fetch's 5 x 60 + 150 = 450 s -> 8 min.
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
      - name: The small fetch
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# KEY_LINE reads a key line to its end. `timeout-minutes: 28 junk` is no key: YAML reads its value
# as the string `28 junk`, which written_minutes takes as no key at all, so the job below writes
# none and no clause holding a job's key reads it. With the `$` dropped from the pattern, the line
# read as key 28, and the block above it was refused for pricing 30 against a key YAML never
# reads (dispatch://INFRA-1545, review round 28). Since round 37 such a key, which GitHub does
# not read as minutes either, is refused by name where it is written (dispatch://INFRA-1545, round
# 37), and still prices nothing.
echo "case: a timeout-minutes line with words after its number is no key a block prices"
root=$(fixture arrow-over-string-key)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: the large fetch's 1050 s and the steps beside it -> 30 min.
    timeout-minutes: 28 junk
    steps:
      - name: The large fetch
        # The large fetch's 5 x 180 + 150 = 1050 s -> 18 min.
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the key's text by name at its line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=7::job 'build' writes .timeout-minutes: 28 junk., which is no whole number with no leading zero")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A key line may carry a YAML comment after its number, and its key is that number, as YAML reads
# it. Without the pattern's alternative for blanks, a `#` and the rest of the line, such a line
# read as no key, so the job below priced nothing and was refused (dispatch://INFRA-1545, review
# round 28).
echo "case: a key carrying a trailing comment is the key its block prices"
root=$(fixture arrow-key-trailing-comment)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the large fetch's 1050 s: 1200 + 1050 = 2250 s -> 38 min.
    timeout-minutes: 38  # the sum above
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# The same key lowered to 37 with its arrow, the trailing comment kept: the block still prices the
# key, and the derivation clause reads the 37 YAML reads and refuses it under its 2250 s.
echo "case: the same key lowered under its derivation, its trailing comment kept, is refused"
root=$(fixture arrow-key-trailing-comment-lowered)
sed 's/-> 38 min/-> 37 min/; s/^    timeout-minutes: 38  # the sum above$/    timeout-minutes: 37  # the sum above/' \
  "$work/arrow-key-trailing-comment/.github/workflows/case.yaml" > "$root/.github/workflows/case.yaml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the derivation clause refuses the key it reads" \
  "$(contains "$out" "job 'build' carries .timeout-minutes: 37., 2220 s, and its own comment derives 1200 + 1050 = 2250 s, which that key does not strictly exceed")"
check "refuses nothing else, so the block still prices that key" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# Each line of a block loses its indentation before its `#`, so an arrow split over two indented
# lines joins as `-> 38 min`. With the `#` stripped alone, the indented second line kept its `#`
# and joined as `-> # 38 min`, which prices nothing, and the job was refused
# (dispatch://INFRA-1545, review round 28).
echo "case: an arrow split over two indented comment lines prices the key below"
root=$(fixture arrow-split-indented)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the large fetch's 1050 s: 1200 + 1050 = 2250 s ->
    # 38 min.
    timeout-minutes: 38
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# A block's keys are read down to the next comment line, so a block above `runs-on:` never reaches
# past the job's own priced block to its key. Without that stop the first block below took 38 as
# its own key and refused it against its 30 (dispatch://INFRA-1545, review round 28).
echo "case: a block's reach ends at the next comment block"
root=$(fixture arrow-ends-at-next-block)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    # The runner queue's own wait, priced apart from this job -> 30 min.
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the large fetch's 1050 s: 1200 + 1050 = 2250 s -> 38 min.
    timeout-minutes: 38
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# EVERY CLAIM THIS RULE'S HEADER MAKES ABOUT WHAT IT REFUSES HAS A CASE HERE THAT FAILS WHEN
# THAT BEHAVIOUR IS REMOVED. The claims were written from memory of the change three rounds
# running and were wrong each time: a reviewer turned `<=` back into `<` and the gate and all
# 299 cases stayed green, so the eleven keys that operator raised were guarded by nothing
# (dispatch://INFRA-1545, review round 26). A claim with no case behind it is a sentence.
echo "case: a job key equal to the sum its own comment states is refused"
root=$(fixture job-key-equals-sum)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1170 s measured, plus its slowest retried step alone — the large fetch,
    # 1050 s. 1170 + 1050 = 2220 s -> 37 min.
    timeout-minutes: 37
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says the key does not strictly exceed the sum" \
  "$(contains "$out" "its own comment derives 1170 + 1050 = 2220 s, which that key does not strictly exceed")"
# 37 minutes is 2220 s, the sum to the second, and 38 minutes is 2280 s, a minute above it. The
# case above fails when `allowed <= total` becomes `allowed < total`: the key is then refused by
# the recomputed sum's sentence and not by the stated sum's, which it pins. The case below fails
# when the refusal reaches a key above the sum. This case stood at 1200 + 1050 = 2250 s against
# 37 minutes, under the sum and not at it, so it stayed green under the first of those mutations
# (dispatch://INFRA-1545, review round 28).
echo "case: one minute above that sum passes"
root=$(fixture job-key-above-sum)
sed 's/-> 37 min/-> 38 min/; s/^    timeout-minutes: 37$/    timeout-minutes: 38/' \
  "$work/job-key-equals-sum/.github/workflows/case.yaml" > "$root/.github/workflows/case.yaml"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a job holding a retried step whose comments price nothing is refused"
root=$(fixture job-prices-nothing)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the large fetch's 1050 s, which comes to 2250 s and takes
    # the whole minute above it: 38.
    timeout-minutes: 38
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses job build's key at line 8, naming the key, the retried step and its 1050 s, the -> N min to write and the gate's file" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' build 'timeout-minutes: 38' \
    'The large fetch' '1050 s' '-> N min' .github/scripts/check-download-retry.sh)"

# PRICED reads an arrow only where `min` follows its number: `-> 38.` names no unit, and the
# pricing clause asks for `-> N min`. With `min` dropped from the pattern, the job below went
# through (dispatch://INFRA-1545, review round 28).
echo "case: an arrow with no unit prices nothing"
root=$(fixture arrow-without-min)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the large fetch's 1050 s: 1200 + 1050 = 2250 s -> 38.
    timeout-minutes: 38
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the key" "$(contains "$out" "job 'build' carries .timeout-minutes: 38. and holds a retried step")"

echo "case: the same job priced as -> N min passes, wherever the block sits"
root=$(fixture job-priced-above-its-id)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  # Budget: 1200 s measured, plus the large fetch's 1050 s: 1200 + 1050 = 2250 s -> 38 min.
  # This block sits above the job's id rather than above its key, which is where two jobs of
  # this repository state their sums.
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 38
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# A block above the job's id prices the key only with no comment line between the two. A note
# above the key ends the block's reach, so the job below is refused; the same job without the
# note passes (dispatch://INFRA-1545, review round 28).
echo "case: a note between a job's priced block and its key is refused"
root=$(fixture job-priced-note-between)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  # Budget: 600 s measured, plus the browser download's 1650 s: 600 + 1650 = 2250 s -> 38 min.
  build:
    runs-on: ubuntu-24.04
    # A note about the runner.
    timeout-minutes: 38
    steps:
      - name: The browser download
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a browser" 300 -- curl -fsSL --max-time 300 -o c https://example.com/c
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the key" "$(contains "$out" "job 'build' carries .timeout-minutes: 38. and holds a retried step")"

echo "case: the same job with the note removed passes"
root=$(fixture job-priced-note-removed)
sed '/# A note about the runner\./d' \
  "$work/job-priced-note-between/.github/workflows/case.yaml" > "$root/.github/workflows/case.yaml"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# What "a job prices itself" means is what the priced-comment clause pairs: a block's arrow goes
# to the keys below it, one per figure, so an arrow belongs to the key it sits above and never to
# a job further down. A 40-line window stood here before, and a reviewer lowered `typecheck` to
# 20 minutes — 1200 s under its own 1650 s — with its derivation written in words: the gate
# stayed green, because `tool-fallback-arm`'s step comment 28 lines up carried `-> 22 min`
# (dispatch://INFRA-1545, review round 27). This case is that shape.
echo "case: a job priced only by a neighbouring job's arrow is refused"
root=$(fixture job-priced-by-neighbour)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  arm:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the small fetch's 450 s: 1200 + 450 = 1650 s -> 28 min.
    timeout-minutes: 28
    steps:
      # The small fetch's 5 x 60 + 150 = 450 s, the whole minute above it (ci-budgets.md's
      # Budgets note) -> 8 min.
      - name: The small fetch
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
  typecheck:
    runs-on: ubuntu-24.04
    # Budget: 600 s measured, plus the large fetch's 1050 s: 600 + 1050 = 1650 s, so the key
    # takes the whole minute above it: 28.
    timeout-minutes: 20
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses typecheck's key, which no block prices" \
  "$(contains "$out" "line=20::job 'typecheck' carries .timeout-minutes: 20. and holds a retried step")"
check "does not refuse the job whose own step comment it is" \
  "$(is "$(printf '%s\n' "$out" | grep -c "job 'arm'")" 0)"

# The walk stops at another job's id. Job `a`'s step comment prices a step that writes no key, so
# with that stop removed it reached job `b`'s key, priced it, and `b` went through with no comment
# of its own (dispatch://INFRA-1545, review round 28).
echo "case: one job's step comment does not price the next job's key"
root=$(fixture job-id-ends-step-comment)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  a:
    runs-on: ubuntu-24.04
    steps:
      # Prose for this step, priced -> 29 min.
      - name: Say hello
        run: echo hello
  b:
    runs-on: ubuntu-24.04
    timeout-minutes: 29
    steps:
      - name: The browser download
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a browser" 300 -- curl -fsSL --max-time 300 -o c https://example.com/c
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses b, which no block of its own prices" \
  "$(contains "$out" "line=12::job 'b' carries .timeout-minutes: 29. and holds a retried step")"

# The same stop where a step comment's second figure finds no key in its own job: `arm`'s step
# comment prices its step at 8 and a slow mirror's floor at 12, and the walk looking for a key
# for that 12 ends at `typecheck`'s id rather than taking `typecheck`'s 12 (dispatch://INFRA-1545,
# review round 28, the job-id stop removed).
echo "case: a step comment's unpaired figure does not price the next job's key"
root=$(fixture job-id-ends-second-figure)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  arm:
    runs-on: ubuntu-24.04
    steps:
      # The small fetch's 5 x 60 + 150 = 450 s -> 8 min, and the floor a slow mirror takes,
      # 720 s -> 12 min.
      - name: The small fetch
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
  typecheck:
    runs-on: ubuntu-24.04
    timeout-minutes: 12
    steps:
      - name: The other small fetch
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "another small asset" 60 -- curl -fsSL --max-time 60 -o d https://example.com/d
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses typecheck, which no block of its own prices" \
  "$(contains "$out" "line=16::job 'typecheck' carries .timeout-minutes: 12. and holds a retried step")"
check "does not refuse arm, whose step its comment prices" \
  "$(is "$(printf '%s\n' "$out" | grep -c "job 'arm'")" 0)"

# The one id a block passes is its own job's, written directly below a block indented no deeper
# than that id. A comment at step depth at the end of job `a` is `a`'s, though it sits directly
# above `b:`; with the indentation test dropped, it priced `b`'s key (dispatch://INFRA-1545,
# review round 28).
echo "case: a step-depth comment directly above the next job's id does not price that job"
root=$(fixture job-id-deeper-comment)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  a:
    runs-on: ubuntu-24.04
    steps:
      - name: Say hello
        run: echo hello
      # Priced for the job below -> 29 min.
  b:
    runs-on: ubuntu-24.04
    timeout-minutes: 29
    steps:
      - name: The browser download
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a browser" 300 -- curl -fsSL --max-time 300 -o c https://example.com/c
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses b, which no block of its own prices" \
  "$(contains "$out" "line=12::job 'b' carries .timeout-minutes: 29. and holds a retried step")"

# The one id a block passes is written on the line directly below the block's last line. A file's
# header comment sits at column 0, no deeper than the first job's id, so with `below == number`
# dropped from the job-id stop the header's `-> 30 min` priced the first job's key, though `name:`,
# `on:` and `jobs:` stand between the two. Review round 29 of dispatch://INFRA-1545 found that
# mutant, and in round 36 the harness still passed 469 of 469 checks under it.
echo "case: a file's header comment does not price the first job's key"
root=$(fixture job-id-file-header)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
# The nightly run -> 30 min.
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 30
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses build, which no block of its own prices" \
  "$(contains "$out" "line=7::job 'build' carries .timeout-minutes: 30. and holds a retried step")"

# A key is paired by its file and its line. The two files below put their job keys on the same
# line, and only the first prices its own; compared by the line alone, the pairing the first file
# made passed the second (dispatch://INFRA-1545, review round 28).
echo "case: a key priced in one file does not price the key on the same line of another"
root=$(fixture paired-by-file)
cat > "$root/.github/workflows/priced.yaml" <<'YAML'
name: Priced
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the large fetch's 1050 s: 1200 + 1050 = 2250 s -> 38 min.
    timeout-minutes: 38
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
cat > "$root/.github/workflows/unpriced.yaml" <<'YAML'
name: Unpriced
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the large fetch's 1050 s, 2250 s, the minute above: 38.
    timeout-minutes: 38
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the unpriced file's key" \
  "$(contains "$out" "file=.github/workflows/unpriced.yaml,line=7::job 'build' carries .timeout-minutes: 38. and holds a retried step")"
check "and nothing in the priced file" \
  "$(is "$(contains "$out" 'file=.github/workflows/priced.yaml')" false)"

echo "case: a job priced by its own arrow more than forty lines above its key passes"
root=$(fixture job-priced-far-above)
{
  printf 'name: Case\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-24.04\n'
  printf '    # Budget: 1200 s measured, plus the large fetch, 1200 + 1050 = 2250 s -> 38 min.\n'
  for line in $(seq 1 45); do
    printf '    # Line %s of the reasoning behind that sum, which this check never reads.\n' "$line"
  done
  cat <<'YAML'
    timeout-minutes: 38
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
} > "$root/.github/workflows/case.yaml"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# The same distance made of the job's own YAML rather than of its comment: a block's keys are
# read however many lines below it they sit. A 40-line reach on the walk let the 45 `env:` lines
# below hide the key from its own block, and the case above never measured that distance, since
# its 45 lines belong to the block and the walk starts below them (dispatch://INFRA-1545, review
# round 28).
echo "case: a job priced by its own arrow above forty-five lines of env passes"
root=$(fixture job-priced-far-above-env)
{
  printf 'name: Case\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-24.04\n'
  printf '    # Budget: 1200 s measured, plus the large fetch, 1200 + 1050 = 2250 s -> 38 min.\n'
  printf '    env:\n'
  for line in $(seq 1 45); do
    printf '      SETTING_%s: "%s"\n' "$line" "$line"
  done
  cat <<'YAML'
    timeout-minutes: 38
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
} > "$root/.github/workflows/case.yaml"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# The pricing clause's guard is `cost <= 0`: only a job whose retried steps cost nothing is passed
# over. A reviewer widened it to `<= 1000` and `download-retry-images`, its arrow written in words
# and its key put back to 95 minutes, went through (review round 27), so a retried job costing a
# few minutes is the case that holds that guard where it is.
echo "case: an unpriced job whose slowest retried step takes 450 s is refused"
root=$(fixture job-prices-nothing-small)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 300 s measured, plus the small fetch's 450 s, which takes the whole minute
    # above 750 s: 13.
    timeout-minutes: 13
    steps:
      - name: The small fetch
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the 450 s step" "$(contains "$out" "whose retries take 450 s to exhaust")"

# The pricing refusal names the job's slowest retried step by the step's own name, which is what
# a reader searches the workflow for. With the job's name put in its place, the refusal below
# named a step 'fetch-assets' that the workflow does not hold (dispatch://INFRA-1545, review
# round 28).
echo "case: an unpriced job's refusal names its slowest step by the step's own name"
root=$(fixture job-prices-nothing-step-name)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  fetch-assets:
    runs-on: ubuntu-24.04
    timeout-minutes: 13
    steps:
      - name: The small fetch
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step, not the job, as the slowest" \
  "$(contains "$out" "job 'fetch-assets' carries .timeout-minutes: 13. and holds a retried step — 'The small fetch', whose retries take 450 s")"

# The priced-comment clause holds a key to BEING ONE OF its block's figures. A reviewer weakened
# `key in figures` to `key >= min(figures)` and `re-record`, whose block prices both a step at 9
# and the job at 69, went through at 50 (review round 27): a key between two figures of its block
# names neither.
echo "case: a key between the two figures its block prices is refused"
root=$(fixture arrow-between-figures)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: the small fetch's 5 x 60 + 150 = 450 s -> 9 min, and the job's 3000 s measured
    # plus that, 3000 + 450 = 3450 s -> 69 min.
    timeout-minutes: 50
    steps:
      - name: The small fetch
        timeout-minutes: 9
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both figures and the key between them" \
  "$(contains "$out" "this comment prices what it sits above at 9 min and 69 min, and the .timeout-minutes: 50. below it, the key of job 'build', carries 50")"

# The same rule from above: a key over every figure its block prices names none of them. A
# reviewer let any key above the largest figure through (`key > max(figures)`), and a key raised
# while its sentence was left alone, a sentence as stale as one under a lowered key, went through
# (dispatch://INFRA-1545, review round 28).
echo "case: a key above every figure its block prices is refused"
root=$(fixture arrow-under-key)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: the large fetch's 1050 s and the steps beside it -> 28 min.
    timeout-minutes: 40
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the figure and the key above it" \
  "$(contains "$out" "this comment prices what it sits above at 28 min, and the .timeout-minutes: 40. below it, the key of job 'build', carries 40")"

# A block's walk reads one key for each figure it states, so a block pricing two figures holds
# the second key below it too. Stopped after the first key, the walk never read the step's 25
# below a block naming 28 and 18 (dispatch://INFRA-1545, review round 28).
echo "case: a block pricing two figures holds its second key too"
root=$(fixture arrow-second-key)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: the job's measured run and its large fetch -> 28 min, and the fetch's own -> 18 min.
    timeout-minutes: 28
    steps:
      - name: The large fetch
        timeout-minutes: 25
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both figures and the step's key" \
  "$(contains "$out" "this comment prices what it sits above at 28 min and 18 min, and the .timeout-minutes: 25. below it, the key of step 'The large fetch' of job 'build', carries 25")"

echo "case: a job whose comment derives its key another way is passed over"
root=$(fixture job-derivation-construction)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: NOT first-plus-slowest. The large fetch's own 18 minutes have to finish before
    # this ceiling, and the steps beside it measure 2 minutes, so the limit is those two with
    # room on top: 18 + 2 = 20, written as 30 for the ten minutes this job has always carried
    # -> 30 min.
    timeout-minutes: 30
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: one minute above that step passes, and the step's retries never reach its own budget"
root=$(fixture job-holds-step-headroom)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 0 s measured, plus the changie tarball's 5 x 60 + 150 = 450 s -> 9 min.
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

# A heredoc's body is data to every rule, as it is for the consumers this tree feeds one to,
# `python3`, `cat` and `read` (dispatch://INFRA-1545, round 37). A body fed to a shell runs as
# commands, which this check no longer models: where the body holds a fetch or names a script
# that fetches, the heredoc is refused by name at its operator, and what it runs is charged
# nothing. The baseline's ci-ok-reconcile.sh is 900 s of its own, and each body below names it.
echo "case: a script called inside a bash heredoc is refused by name, and not charged"
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
check "refuses the heredoc at its operator, naming the shell" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Reconcile the ci-oks the queue left' of job 'build' feeds a heredoc to .bash., and the body holds a fetch or names a script that fetches")"
check "charges the step nothing for the body" "$(is "$(contains "$out" '900 s')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

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

# However the shell is written — a bare word, an absolute path, options of its own, a `-s` that
# says the script is standard input, a `-` written for the script argument — it is a consumer
# this tree feeds no heredoc to, and each is refused by name at its operator.
echo "case: every spelling of a shell fed a heredoc is refused by name"
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
for refusal in "10	A shell written as an absolute path	/bin/bash" \
  "16	A shell carrying options of its own	bash" \
  "22	A shell told to read its script from standard input	bash" \
  "28	A dash written for the script argument	sh"; do
  IFS=$'\t' read -r line step consumer <<<"$refusal"
  check "refuses '$step' at its operator, naming $consumer" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' feeds a heredoc to .$consumer., and the body holds a fetch")"
done
# `-euo` is a group of the shell's options holding `o`, which takes the next word as its value, a
# form shell_operand does not read, so it is refused by name too (dispatch://INFRA-1545, round 74).
check "refuses the group -euo at line 16 by name" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=16::' '`bash`' '`-euo`')"
check "each of the four is one refusal, and the group -euo one more" \
  "$(contains "$out" '5 download(s) outside the one retry rule')"

# A shell whose script is somewhere else reads its body as that script's input, but the shell is
# no consumer this tree writes, so the heredoc is refused all the same where its body names a
# script that fetches: a refusal of a body that may be data, accepted (dispatch://INFRA-1545,
# round 37).
echo "case: a heredoc fed to a shell whose script is somewhere else is refused all the same"
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
check "exits 1" "$(is "$status" 1)"
for refusal in "9	A script behind -c, whose body is that script's input" \
  "14	A script file, whose body is that file's input"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' at its operator" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' feeds a heredoc to .bash., and the body holds a fetch")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"
check "neither step is asked for a budget" "$(is "$(contains "$out" '900 s')" false)"

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

# An entry may name its loop by that loop's first line, and then answers for that loop alone, so
# one file and one verb can hold two loops each priced by an entry of its own, as
# check-pr-text.sh's three `gh api` loops are since main's #4256. The baseline's entry for
# ci-ok-reconcile.sh names no loop, so it keeps the first loop here and the named one takes the
# second: a step running the script waits for 900 + 300 = 1200 s, a whole number of minutes.
echo "case: a declaration naming its loop answers for that loop alone"
for minutes in 20 21; do
  root=$(fixture "declaration-named-loop-$minutes")
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
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Reconcile the ci-ok check runs
        timeout-minutes: $minutes
        run: .github/scripts/ci-ok-reconcile.sh
YAML
  declare_entry "$root" '    DeclaredArithmetic(".github/scripts/ci-ok-reconcile.sh", None, "gh api", 300, "one read of the other ci-oks: 1 x (5 x 30 + 150) = 300 s", loop="while read -r id; do"),'
  run_check "$root"
  if [ "$minutes" = 20 ]; then
    check "a budget of 20 minutes exits 1" "$(is "$status" 1)"
    check "charges both loops, each at its own entry's figure" \
      "$(contains "$out" "step 'Reconcile the ci-ok check runs' carries .timeout-minutes: 20., 1200 s, and its own retries take 1200 s to exhaust")"
    check "and refuses nothing else, no entry for covering two loops" \
      "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
  else
    check "a budget of 21 minutes exits 0" "$(is "$status" 0)"
  fi
done

# A named loop whose first line the file no longer writes is a declaration outliving its loop,
# refused as any stale entry is, by the loop it names.
echo "case: a declaration naming a loop the file does not write fails"
root=$(fixture declaration-named-loop-gone)
declare_entry "$root" '    DeclaredArithmetic(".github/scripts/ci-ok-reconcile.sh", None, "gh api", 300, "one read of a loop that has gone: 1 x (5 x 30 + 150) = 300 s", loop="while read -r gone; do"),'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the entry by its loop" \
  "$(contains "$out" 'the declared arithmetic for .github/scripts/ci-ok-reconcile.sh .gh api. in the loop .while read -r gone; do. matches no retried call this check could not price of itself')"

# A harness's library, a `-test-lib.sh` directly in .github/scripts/, is no script a job runs:
# the harnesses source it, and the gh its functions reach is their stand-in. It is not read, as
# a harness is not, so its unretried call, its path held in a variable and its run of a script
# that fetches through that variable pass; and it stays a file this check does not read, so a
# step that sources it is refused by name (main's #4256 split check-private-names-test-lib.sh out
# of its harness; dispatch://INFRA-1545).
echo "case: a harness library is not read, and a step naming it is refused"
root=$(fixture harness-library)
cat > "$root/.github/scripts/r41-test-lib.sh" <<'SH'
#!/usr/bin/env bash
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
pr_text_script="$script_dir/check-pr-text.sh"
gh api "repos/example/repo/pulls/7"
run_pr_text() { "$pr_text_script"; }
SH
run_check "$root"
check "the library alone exits 0" "$(is "$status" 0)"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source the harness library
        run: . .github/scripts/r41-test-lib.sh
YAML
run_check "$root"
check "a step sourcing it exits 1" "$(is "$status" 1)"
check "refuses the step by name, as a file this check does not read" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Source the harness library' of job 'build' writes ..github/scripts/r41-test-lib.sh. as an argument of .\.., and it names .github/scripts/r41-test-lib.sh, a file under .github/scripts/ this check does not read as a script")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

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
check "refuses at the gate's own file, naming the two arguments it takes, --list and --jobs" \
  "$(names_at "$out" '::error file=.github/scripts/check-download-retry.sh::' --list --jobs)"

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
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=11::.curl. in step .Install changie. is not the last command of its pipeline')"
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
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=9::.curl. in step .Install changie. fetches without the floor')"
check "names pipefail and shell: bash at line 9 too, so the floor and the pipeline are one refusal" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=9::' 'Install changie' pipefail 'shell: bash')"
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
  "$(contains "$out" '^.github/workflows/case.yaml	12	curl	Read what this job downloads$')"

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
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=14::.curl. in step .Install changie. is not the last command of its pipeline')"

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
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=11::.curl. in step .Install changie. is not the last command of its pipeline')"
check "counts the one refusal" "$(contains "$out" '1 download(s) outside the one retry rule')"

echo "case: a job that compiles Go with no retried module download of its own"
# Rule 11's own fixture, driven both ways: a go command fetches this module's dependencies when
# the cache misses, and that fetch is in none of THE VERBS, so every other rule of this check
# passes the job. Two real jobs stood like this - the browser suite's embedder and the nightly
# re-record - each found by a person reading the job rather than by this check.
root=$(fixture go-build-no-download)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [pull_request]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
      - name: Build the server
        run: go build -o ./server ./cmd/server
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job, the step and the file" \
  "$(contains "$out" "file=.github/workflows/case.yaml,line=9::job .build. compiles Go in step .Build the server. and holds no retried")"
# The same job with the download step the browser suite's jobs carry.
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [pull_request]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
      - name: Download the Go modules this job builds
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a module's Go modules" 180 -- go -C packages/envoy mod download
      - name: Build the server
        run: go build -o ./server ./cmd/server
YAML
run_check "$root"
check "passes once the job downloads its modules in a step of its own" "$(is "$status" 0)"
# And a `go test` is read as readily as a `go build`, since both fetch on a cache miss.
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [pull_request]
jobs:
  unit:
    runs-on: ubuntu-24.04
    steps:
      - run: go test ./...
YAML
run_check "$root"
check "reads a go test too" \
  "$(contains "$out" "job .unit. compiles Go in step")"

echo "case: rule 11's exemption holds a job that compiles Go and stops holding a renamed one"
# The exemption path and its staleness guard, which the rule's own fixture above does not reach:
# the fixture's ci-job-airlock-lean.yaml carries `airlock-lean`, so a tree whose GO_BUILD_EXEMPT names it passes
# with that job compiling Go and no download, and the same tree with the job renamed is refused
# twice - once for the renamed job, which no entry covers, and once for the entry, which now
# matches no job compiling Go. That second refusal is what a guard that asked only whether the job
# still existed never made: it left a renamed job's exception live for the next job of that name.
root=$(fixture go-exempt)
run_check "$root"
check "the exempted job passes" "$(is "$status" 0)"
check "and no refusal names it" "$(is "$(contains "$out" 'airlock-lean')" false)"

root=$(fixture go-exempt-stale)
python3 - "$root/.github/workflows/ci-job-airlock-lean.yaml" <<'PY_EDIT'
import sys
p = sys.argv[1]
t = open(p).read()
open(p, "w").write(t.replace("  airlock-lean:", "  airlock-lean-renamed:", 1))
PY_EDIT
run_check "$root"
check "a renamed job is refused" "$(is "$status" 1)"
check "the renamed job is named" \
  "$(contains "$out" "job 'airlock-lean-renamed' compiles Go in step")"
check "and its stale exemption is named too" \
  "$(contains "$out" "GO_BUILD_EXEMPT holds 'airlock-lean' of this file")"

echo "case: a workflow whose job writes one key twice"
# GitHub refuses such a file and runs nothing of it, where PyYAML takes the last of the two, so
# this check reads it before any rule below can pass a file CI will not start. Driven both ways:
# the duplicate is refused by name and line, and the same tree with one of them deleted passes.
root=$(fixture duplicate-key)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 22
    timeout-minutes: 75
    steps:
      - run: echo hi
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the key and the second line" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=7::`timeout-minutes` is written twice')"

python3 - "$root/.github/workflows/case.yaml" <<'PY_EDIT'
import sys
p = sys.argv[1]
open(p, "w").write(open(p).read().replace("    timeout-minutes: 22\n", "", 1))
PY_EDIT
run_check "$root"
check "and the same tree with one of them gone passes" "$(is "$status" 0)"

# ---------------------------------------------------------------- rule 8's own mutants
# Every case below answers one mutant round 32's enumeration of rule 8 made and the harness
# missed: the code changed, the gate's every refusal on the tree stayed the same, and all 326
# cases passed (dispatch://INFRA-1545, round 32). Each names the behaviour its mutant removed.

# The four walks rule 8 recurses through each stop at a path already read, so a cycle ends and a
# file two calls reach is read once. A cycle is what no case wrote, so the stop could go and every
# case stayed green; without it the check recurses until Python gives up. Since round 37 a script
# that reaches itself and fetches is refused by name as well, each of the pair at its call of the
# other, since the run a path stops at is charged nothing (dispatch://INFRA-1545, round 37); the
# check still ends, with those two refusals and no other.
echo "case: two scripts that call each other are read once each, and the check ends"
root=$(fixture script-cycle)
cat > "$root/.github/scripts/cycle-a.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
.github/scripts/cycle-b.sh
SH
cat > "$root/.github/scripts/cycle-b.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/cycle-a.sh
SH
chmod +x "$root/.github/scripts/cycle-a.sh" "$root/.github/scripts/cycle-b.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through the pair
        timeout-minutes: 8
        run: .github/scripts/cycle-a.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for refusal in \
  ".github/scripts/cycle-a.sh	5	.github/scripts/cycle-b.sh" \
  ".github/scripts/cycle-b.sh	3	.github/scripts/cycle-a.sh"; do
  IFS=$'\t' read -r script line target <<<"$refusal"
  check "refuses $script, which reaches itself, at its call of $target" \
    "$(contains "$out" "::error file=$script,line=$line::$script runs or sources $target, and that call reaches $script again")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a loop around two scripts that call each other and fetch nothing passes"
root=$(fixture script-cycle-costless)
cat > "$root/.github/scripts/quiet-a.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "$1"
.github/scripts/quiet-b.sh "$1"
SH
cat > "$root/.github/scripts/quiet-b.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/quiet-a.sh "$1"
SH
chmod +x "$root/.github/scripts/quiet-a.sh" "$root/.github/scripts/quiet-b.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Print every name
        timeout-minutes: 5
        run: |
          while read -r name; do
            .github/scripts/quiet-a.sh "$name"
          done <<<"$names"
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a loop around two scripts that call each other and fetch is refused, and the check ends"
root=$(fixture script-cycle-uncounted)
cat > "$root/.github/scripts/loud-a.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "the asset $1" 60 -- curl -fsSL --max-time 60 -o a "https://example.com/$1"
.github/scripts/loud-b.sh "$1"
SH
cat > "$root/.github/scripts/loud-b.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/loud-a.sh "$1"
SH
chmod +x "$root/.github/scripts/loud-a.sh" "$root/.github/scripts/loud-b.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch every name
        timeout-minutes: 30
        run: |
          while read -r name; do
            .github/scripts/loud-a.sh "$name"
          done <<<"$names"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the loop around the first script" \
  "$(contains "$out" '..github/scripts/loud-a.sh. at .github/workflows/case.yaml:11, inside the loop at line 10')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: two composite actions that use each other are read once each, and the check ends"
root=$(fixture action-cycle)
mkdir -p "$root/.github/actions/cycle-x" "$root/.github/actions/cycle-y"
cat > "$root/.github/actions/cycle-x/action.yml" <<'YAML'
name: Cycle x
description: Fetches an asset and uses cycle-y.
runs:
  using: composite
  steps:
    - name: Fetch the asset
      shell: bash
      run: |
        curl -fsSL --retry 7 --retry-all-errors --max-time 60 -o x.tar.gz \
          https://example.com/x.tar.gz
    - uses: ./.github/actions/cycle-y
YAML
cat > "$root/.github/actions/cycle-y/action.yml" <<'YAML'
name: Cycle y
description: Uses cycle-x.
runs:
  using: composite
  steps:
    - uses: ./.github/actions/cycle-x
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the pair
        uses: ./.github/actions/cycle-x
        timeout-minutes: 11
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

# step_name: a `uses:` step with no `name:` reads as '-' in the workflow, so every step refusal
# names the action it runs instead (review round 26). No case wrote such a step until this one.
echo "case: a step with no name is named in a refusal by the action it runs"
root=$(fixture action-unnamed)
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
      - uses: ./.github/actions/install-elan
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step by the action it runs" \
  "$(contains "$out" "step 'the step running .github/actions/install-elan/action.yml' holds 607 s of retries")"
check "never names it '-'" "$(is "$(contains "$out" "step '-'")" false)"

# The pricing refusal names a job's slowest step as step_name does: a nameless `uses:` step by the
# action it runs. With the bare `budget.step` kept in JOB_SLOWEST in place of step_name, the
# refusal below named a step '-' (dispatch://INFRA-1545, review round 28).
echo "case: an unpriced job's nameless action step is named by the action it runs"
root=$(fixture job-prices-nothing-action-step)
mkdir -p "$root/.github/actions/fetch-asset"
cat > "$root/.github/actions/fetch-asset/action.yml" <<'YAML'
name: Fetch an asset
description: Fetches the asset a job's tests read.
runs:
  using: composite
  steps:
    - name: Fetch the asset
      shell: bash
      run: |
        . .github/scripts/ci-retry.sh
        retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 13
    steps:
      - uses: ./.github/actions/fetch-asset
        timeout-minutes: 8
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the slowest step by the action it runs" \
  "$(contains "$out" "holds a retried step — 'the step running .github/actions/fetch-asset/action.yml', whose retries take 450 s")"

# step_name names a step by an action only where the step runs one. A nameless `run:` step runs
# none, and with `and budget.uses` dropped the refusal below named it 'the step running None'. It
# names the step '-', as the workflow reads it (dispatch://INFRA-1545, review round 28).
echo "case: an unpriced job's nameless run step is named '-', never by an action"
root=$(fixture job-prices-nothing-nameless-run)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 13
    steps:
      - timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step '-'" "$(contains "$out" "holds a retried step — '-', whose retries take 450 s")"
check "never names an action it does not run" "$(is "$(contains "$out" 'None')" false)"

# The slowest step is the first one with the largest arithmetic: a later step that only ties it
# does not take its place, so the refusal names the step written first.
echo "case: of two steps that tie for the slowest, a refusal names the first"
root=$(fixture job-slowest-tie)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus its slowest retried step alone — the small fetch, 450 s.
    # 1200 + 450 = 1650 s -> 30 min.
    timeout-minutes: 30
    steps:
      - name: The first large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
      - name: The second large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "another large asset" 180 -- curl -fsSL --max-time 180 -o c https://example.com/c
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the first of the two" \
  "$(contains "$out" "slowest retried step of this job is 'The first large fetch'")"

# A step holding two loops this check cannot count is told about the first of them, by its figure
# and its name: the refusal's sentence is written for blocking[0], and the list beside it names
# the rest.
echo "case: a step with two uncounted loops is priced at the first loop's call"
root=$(fixture two-uncounted-first-figure)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch every name twice over
        timeout-minutes: 30
        run: |
          . .github/scripts/ci-retry.sh
          while read -r id; do
            retry_fetch "the small $id" 60 -- curl -fsSL --max-time 60 -o a "https://example.com/$id"
          done <<<"$ids"
          while read -r id; do
            retry_fetch "the large $id" 180 -- curl -fsSL --max-time 180 -o b "https://example.com/$id"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "prices one call of the first loop's site" "$(contains "$out" 'One call there takes 450 s')"

echo "case: a step with two uncounted loops whose bounds are unread names the first site"
root=$(fixture two-uncounted-first-unread)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch every name twice over
        timeout-minutes: 30
        run: |
          while read -r id; do
            curl -fsSL --retry 7 --retry-all-errors --max-time "$T" -o a "https://example.com/$id"
          done <<<"$ids"
          . .github/scripts/ci-retry.sh
          while read -r id; do
            retry_fetch "the run $id" "$T" -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/$id"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the first site's verb in the sentence" "$(contains "$out" 'What one call of .curl. there takes')"

# The two loops in the other order: the first loop's call has a bound the check reads and the
# second's is unread. Every blocked-item case above has the unread one first, or both alike,
# so reading `blocking[-1].one` where the gate reads `blocking[0].one` passed the whole harness
# and told the reader that the first call's bound is a variable when it is `--max-time 60`
# (dispatch://INFRA-1545, the tester's mutation pass at 663a6c05a083, mutant T045).
echo "case: a step with two uncounted loops, the first with a read bound, prices the first"
root=$(fixture two-uncounted-first-read)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch every name twice over
        timeout-minutes: 30
        run: |
          while read -r id; do
            curl -fsSL --retry 7 --retry-all-errors --max-time 60 -o a "https://example.com/$id"
          done <<<"$ids"
          . .github/scripts/ci-retry.sh
          while read -r id; do
            retry_fetch "the run $id" "$T" -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/$id"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "prices the first loop's call at its read bound" "$(contains "$out" 'One call there takes 607 s')"
check "does not call the first call's bound a variable" \
  "$(is "$(contains "$out" 'What one call of .curl. there takes is a figure this check cannot make')" false)"

echo "case: a step looping over two scripts, the first with an unread bound, names the first"
root=$(fixture two-uncounted-scripts)
cat > "$root/.github/scripts/early-unread.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
curl -fsSL --retry 7 --retry-all-errors --max-time "$T" -o a "https://example.com/$1"
SH
cat > "$root/.github/scripts/later-numbered.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
curl -fsSL --retry 7 --retry-all-errors --max-time 60 -o a "https://example.com/$1"
SH
chmod +x "$root/.github/scripts/early-unread.sh" "$root/.github/scripts/later-numbered.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch every name through both
        timeout-minutes: 30
        run: |
          while read -r id; do
            .github/scripts/early-unread.sh "$id"
          done <<<"$ids"
          while read -r id; do
            .github/scripts/later-numbered.sh "$id"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the first script and its unread bound" \
  "$(contains "$out" 'One run of ..github/scripts/early-unread.sh. makes attempts whose bound is written as a variable')"

# One refusal per step: a step told its retries sit in a loop nobody can count is told that alone,
# and not also about a second site of it whose bound is unread.
echo "case: a step with an uncounted loop and an unread bound is refused once"
root=$(fixture uncounted-and-unread)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch every name, then one more
        timeout-minutes: 30
        run: |
          . .github/scripts/ci-retry.sh
          while read -r id; do
            retry_fetch "the small $id" 60 -- curl -fsSL --max-time 60 -o a "https://example.com/$id"
          done <<<"$ids"
          curl -fsSL --retry 7 --retry-all-errors --max-time "$T" -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the step for its loop" "$(contains "$out" 'inside the loop at line 11')"
check "and only once" \
  "$(is "$(printf '%s\n' "$out" | grep -c 'this check can put no figure on it')" 1)"

# A step whose own budget is under its retries is told about its own budget first, and not also
# about the job's: the step's budget is the one its author writes next.
echo "case: a step under its own retries is told about its budget, not also its job's"
root=$(fixture step-and-job-under)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: the small fetch's 5 x 60 + 150 = 450 s alone -> 5 min.
    timeout-minutes: 5
    steps:
      - name: The small fetch
        timeout-minutes: 7
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the step's own budget" \
  "$(contains "$out" "step 'The small fetch' carries .timeout-minutes: 7.")"
check "and not the job's for the same retries" \
  "$(is "$(printf '%s\n' "$out" | grep -c "job 'build' carries .timeout-minutes: 5.")" 0)"

echo "case: a job under a step's retries is told about that, not also about the step's budget"
root=$(fixture job-under-step-retries)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: the small fetch's 5 x 60 + 150 = 450 s alone -> 7 min.
    timeout-minutes: 7
    steps:
      - name: The small fetch
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the job against the step's retries" \
  "$(contains "$out" "holds retries taking 450 s to exhaust, which that budget does not strictly exceed")"
check "and not also against the step's own budget" \
  "$(is "$(printf '%s\n' "$out" | grep -c "which that job budget does not strictly exceed")" 0)"

echo "case: a job that writes no timeout-minutes is told GitHub gives it 360 minutes"
root=$(fixture job-default-under-step)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: The small fetch
        timeout-minutes: 400
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says the job writes none and what GitHub gives it" \
  "$(contains "$out" "job 'build' writes no .timeout-minutes., so GitHub gives it 360 minutes, and its step 'The small fetch' carries .timeout-minutes: 400.")"

# The same default, under a step's retries this time: the refusal prints GitHub's 360 minutes in
# seconds as well, a figure its reader compares with the step's own, so it is pinned exactly.
echo "case: a job that writes no timeout-minutes, under a step's retries, is told the 21600 s it gets"
root=$(fixture job-default-under-retries)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: The large fetch, thirty times
        timeout-minutes: 600
        run: |
          . .github/scripts/ci-retry.sh
          for x in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30; do
            retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says what GitHub gives the job, in minutes and in seconds, against the step's retries" \
  "$(contains "$out" "job 'build' writes no .timeout-minutes., so GitHub gives it 360 minutes, 21600 s, and its step 'The large fetch, thirty times' holds retries taking 31500 s to exhaust")"
check "says exactly what to write on the job, above the step's own key of 600" \
  "$(contains "$out" 'Write .timeout-minutes: 601. or more on this job\.')"

# The priced-comment clause reads a block to the last line of the file, and a file whose last line
# is a comment with no newline after it ends the block there.
echo "case: a workflow whose last line is a comment with no newline after it passes"
root=$(fixture comment-at-end-of-file)
{
  cat <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the small fetch's 450 s: 1200 + 450 = 1650 s -> 28 min.
    timeout-minutes: 28
    steps:
      - name: The small fetch
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
YAML
  printf '    # The last line of this file, with no newline after it.'
} > "$root/.github/workflows/case.yaml"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

# The derivation clause's header says a sum that does not add up is passed over rather than
# refused, since a clause recomputing a total it cannot trust would be guessing. This is that sum.
echo "case: a derivation whose sum does not add up is passed over"
root=$(fixture job-derivation-bad-sum)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the small fetch's 450 s. 1200 + 450 = 2500 s -> 40 min.
    timeout-minutes: 40
    steps:
      - name: The small fetch
        timeout-minutes: 9
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# One refusal per job: a key under the sum its own comment states is told that, and not also the
# sum this check recomputes, which the corrected derivation will state anyway.
echo "case: a key under both its stated and its recomputed sum is refused once"
root=$(fixture job-derivation-both-under)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus its slowest retried step alone — the small fetch, 450 s.
    # 1200 + 450 = 1650 s -> 27 min.
    timeout-minutes: 27
    steps:
      - name: The small fetch
        timeout-minutes: 9
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a small asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the key under the sum its comment states" \
  "$(contains "$out" "own comment derives 1200 + 450 = 1650 s, which that key does not strictly exceed")"
check "and only once" "$(is "$(printf '%s\n' "$out" | grep -c 'own comment derives')" 1)"

# The pricing clause passes over a job whose steps retry nothing: there is no arithmetic under its
# key for a comment to price, and the tree holds such jobs.
echo "case: a job whose steps retry nothing needs no price"
root=$(fixture job-retries-nothing)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 5
    steps:
      - name: Say hello
        timeout-minutes: 2
        run: echo hello
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# A declaration two loops match is refused for that, once: its figure is wrong for one of the two
# loops whatever it is, so its floor is not told about as well.
echo "case: a declaration covering two loops and under a call's floor is refused once"
root=$(fixture declaration-two-loops-below)
declare_entry "$root" '    DeclaredArithmetic(".github/workflows/case.yaml",
                       "Cancel every run, then read every run", "gh api", 200,
                       "a figure nobody derived"),'
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Cancel every run, then read every run
        timeout-minutes: 20
        run: |
          . .github/scripts/ci-retry.sh
          while read -r id; do
            retry_fetch "the cancel of run $id" 30 -- \
              gh api --silent --method POST "repos/$GITHUB_REPOSITORY/actions/runs/$id/cancel"
          done <<<"$ids"
          while read -r id; do
            retry_fetch "the run $id" 30 -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/$id"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the entry at the gate's line 1 by its file, step and verb, the loops at lines 11 and 15, and its 200 s" \
  "$(names_at "$out" '::error file=.github/scripts/check-download-retry.sh,line=1::' .github/workflows/case.yaml \
    'Cancel every run, then read every run' 'gh api' '11, 15' '200 s')"
check "and not also for its floor" "$(is "$(contains "$out" 'writes 200 s, and one call')" false)"

echo "case: a declaration under the floor of two of its calls names the first of them"
root=$(fixture declared-arithmetic-two-floors)
declare_entry "$root" '    DeclaredArithmetic(".github/workflows/case.yaml",
                       "Cancel and read every run the decision named", "gh api", 200,
                       "a figure nobody derived"),'
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Cancel and read every run the decision named
        timeout-minutes: 20
        run: |
          . .github/scripts/ci-retry.sh
          while read -r id; do
            retry_fetch "the cancel of run $id" 30 -- \
              gh api --silent --method POST "repos/$GITHUB_REPOSITORY/actions/runs/$id/cancel"
            retry_fetch "the run $id" 60 -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/$id"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the first call under the floor and its floor" \
  "$(contains "$out" 'one call of that site at .github/workflows/case.yaml:12 takes 300 s')"

# The derivation clause reads the block the priced-comment clause pairs with the key, and a block
# directly above the job's id is one: this job's key, 30 minutes, 1800 s, sits under the 2250 s
# its own derivation states there, and is refused for it. Until round 35 the clause read only the
# comment block directly above the key, this case pinned that limit by passing, and five jobs of
# this repository could be lowered under their sums with their arrows moved and no refusal
# naming them (dispatch://INFRA-1545, round 35).
echo "case: a derivation written above the job's id is read by the derivation clause"
root=$(fixture job-derivation-above-id)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  # Budget: 1200 s measured, plus the large fetch's 1050 s. 1200 + 1050 = 2250 s -> 30 min.
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 30
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the key under the sum its above-id derivation states" \
  "$(contains "$out" "job 'build' carries .timeout-minutes: 30., 1800 s, and its own comment derives 1200 + 1050 = 2250 s, which that key does not strictly exceed")"

# Another of the derivation clause's stated limits, pinned so that reading it starts or stops in
# the harness: a sum with words between it and its arrow. The key sits under the sum it states,
# and passes, because this clause does not read it as a derivation.
echo "case: a derivation with words between its sum and its arrow is not read as one"
root=$(fixture job-derivation-words-between)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the large fetch's 1050 s. 1200 + 1050 = 2250 s, of which
    # this key carries the measured part -> 30 min.
    timeout-minutes: 30
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# The pricing clause passes over a job only where its retried steps cost nothing at all: a
# declaration answering a site whose bound is unread writes that site's seconds outright, so a
# retried step can cost one second, and its job is still a retried job that must price itself.
echo "case: an unpriced job whose retries cost one declared second is refused"
root=$(fixture job-prices-nothing-one-second)
declare_entry "$root" '    DeclaredArithmetic(".github/workflows/case.yaml",
                       "Read the run", "gh api", 1,
                       "a figure that is the smallest a declaration can write"),'
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 5
    steps:
      - name: Read the run
        timeout-minutes: 1
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the run" "$T" -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/1"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the job for its one second of retries" \
  "$(contains "$out" "whose retries take 1 s to exhaust — and no comment block prices that key")"

# Each case below answers a mutant round 35's mutation pass of rule 8 made and the harness
# missed: the code changed and all of its cases stayed green (dispatch://INFRA-1545, round 35).
#
# The first four answer a `break` written after the sum in one of the four loops rule 8 adds a
# step's arithmetic up over, so the loop counts its first item alone. On this repository's own
# tree each of them lowered a real step's cost — ci-filters' from 600 to 300 s, test's from 3012
# to 2700 s, pr-title's from 1800 to 1500 s — so a key could sit under its true sum, and nothing
# refused it, because no fixture held such a loop over two items that each cost something. In
# each case below the key is above either item's ladder alone and below the two together, so a
# sum of one item passes it whichever item that is. The scripts are 5 x 30 + 150 = 300 s and
# 5 x 60 + 150 = 450 s, 750 s together, and the actions' curls 8 x 30 + 127 = 367 s and
# 8 x 60 + 127 = 607 s, 974 s together.
echo "case: a step running two retried scripts is held to both of their ladders"
root=$(fixture two-scripts-in-a-step)
cat > "$root/.github/scripts/fetch-small.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
SH
cat > "$root/.github/scripts/fetch-large.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 60 -- curl -fsSL --max-time 60 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/fetch-small.sh" "$root/.github/scripts/fetch-large.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch both assets
        timeout-minutes: 12
        run: |
          .github/scripts/fetch-small.sh
          .github/scripts/fetch-large.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts both scripts' ladders" "$(contains "$out" 'its own retries take 750 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 13')"

echo "case: a script running two retried scripts carries both of their ladders"
root=$(fixture two-scripts-in-a-script)
cat > "$root/.github/scripts/fetch-small.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
SH
cat > "$root/.github/scripts/fetch-large.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 60 -- curl -fsSL --max-time 60 -o b https://example.com/b
SH
cat > "$root/.github/scripts/fetch-both.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/fetch-small.sh
.github/scripts/fetch-large.sh
SH
chmod +x "$root/.github/scripts/fetch-small.sh" "$root/.github/scripts/fetch-large.sh" \
  "$root/.github/scripts/fetch-both.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch both assets
        timeout-minutes: 12
        run: .github/scripts/fetch-both.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts both scripts' ladders" "$(contains "$out" 'its own retries take 750 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 13')"

echo "case: a composite action running two retried scripts carries both of their ladders"
root=$(fixture two-scripts-in-an-action)
cat > "$root/.github/scripts/fetch-small.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
SH
cat > "$root/.github/scripts/fetch-large.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 60 -- curl -fsSL --max-time 60 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/fetch-small.sh" "$root/.github/scripts/fetch-large.sh"
mkdir -p "$root/.github/actions/fetch-both"
cat > "$root/.github/actions/fetch-both/action.yml" <<'YAML'
name: Fetch both
description: Fetches both assets through their scripts.
runs:
  using: composite
  steps:
    - name: Fetch both assets
      shell: bash
      run: |
        .github/scripts/fetch-small.sh
        .github/scripts/fetch-large.sh
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch both assets
        uses: ./.github/actions/fetch-both
        timeout-minutes: 12
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts both scripts' ladders" "$(contains "$out" 'its own retries take 750 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 13')"

echo "case: a composite action using two retried actions carries both of their ladders"
root=$(fixture two-actions-in-an-action)
mkdir -p "$root/.github/actions/fetch-small" "$root/.github/actions/fetch-large" \
  "$root/.github/actions/fetch-both"
cat > "$root/.github/actions/fetch-small/action.yml" <<'YAML'
name: Fetch small
description: Fetches the small asset.
runs:
  using: composite
  steps:
    - name: Fetch the small asset
      shell: bash
      run: curl -fsSL --retry 7 --retry-all-errors --max-time 30 -o a https://example.com/a
YAML
cat > "$root/.github/actions/fetch-large/action.yml" <<'YAML'
name: Fetch large
description: Fetches the large asset.
runs:
  using: composite
  steps:
    - name: Fetch the large asset
      shell: bash
      run: curl -fsSL --retry 7 --retry-all-errors --max-time 60 -o b https://example.com/b
YAML
cat > "$root/.github/actions/fetch-both/action.yml" <<'YAML'
name: Fetch both
description: Fetches both assets through their actions.
runs:
  using: composite
  steps:
    - uses: ./.github/actions/fetch-small
    - uses: ./.github/actions/fetch-large
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch both assets
        uses: ./.github/actions/fetch-both
        timeout-minutes: 16
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts both actions' ladders" "$(contains "$out" 'its own retries take 974 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 17')"

# DERIVATION and PRICED read a sum and an arrow whatever space is written inside them, and every
# derivation and arrow of this tree is written with spaces, so each of the two patterns' `\s*`
# could become `\s+` with every case green (round 35's pass, mutants M265-M273 of DERIVATION and
# M183-M184 of PRICED). A sum written with no space in it is a derivation all the same, and its
# key at that sum is refused by the derivation clause; an arrow written with no space in it
# prices the key below it all the same, and the job it prices passes.
echo "case: a derivation written with no space in it is read, and its key at its sum refused"
root=$(fixture job-derivation-tight)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1170 s measured, plus its slowest retried step alone — the large fetch,
    # 1050 s. 1170+1050=2220s->37min.
    timeout-minutes: 37
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says the key does not strictly exceed the sum" \
  "$(contains "$out" "its own comment derives 1170 + 1050 = 2220 s, which that key does not strictly exceed")"

echo "case: an arrow written with no space in it prices the key below it"
root=$(fixture arrow-tight)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus the large fetch's 1050 s, which comes to 2250 s and takes
    # the whole minute above it ->38min.
    timeout-minutes: 38
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# A script two of a step's runs reach is charged once for every run, whatever the files are
# named. Review round 29 of dispatch://INFRA-1545 drove this input: the step runs zz-a.sh three
# times in a loop and zz-b.sh once, both run zz-c.sh, whose 180 s retry is 5 x 180 + 150 =
# 1050 s, and the step's key is 20 minutes, 1200 s. The step runs zz-c.sh four times, three
# through zz-a.sh and once through zz-b.sh: 4 x 1050 = 4200 s. A walk charging zz-c.sh through
# the first path it visited charged 3150 s here, and with zz-b.sh renamed zz-0.sh, which sorts
# before zz-a.sh, it charged 1050 s and passed the step; the walk after it charged each file
# once at its most expensive path, 3150 s under either name, which was the undercount round 37
# of dispatch://INFRA-1545 corrected. Both names are refused with the same 4200 s.
echo "case: a script two of a step's runs reach is charged once for every run"
root=$(fixture shared-script-most-expensive)
cat > "$root/.github/scripts/zz-c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/scripts/zz-a.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/zz-c.sh
SH
cp "$root/.github/scripts/zz-a.sh" "$root/.github/scripts/zz-b.sh"
chmod +x "$root/.github/scripts/zz-a.sh" "$root/.github/scripts/zz-b.sh" \
  "$root/.github/scripts/zz-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through both scripts
        timeout-minutes: 20
        run: |
          for pass in 1 2 3; do
            .github/scripts/zz-a.sh
          done
          .github/scripts/zz-b.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges zz-c.sh once for each of its four runs" \
  "$(contains "$out" 'its own retries take 4200 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 71')"

echo "case: the same step with zz-b.sh renamed zz-0.sh, which sorts first, is charged the same"
root=$(fixture shared-script-renamed)
cp "$work/shared-script-most-expensive/.github/scripts/zz-a.sh" \
  "$work/shared-script-most-expensive/.github/scripts/zz-c.sh" "$root/.github/scripts/"
cp "$work/shared-script-most-expensive/.github/scripts/zz-b.sh" "$root/.github/scripts/zz-0.sh"
chmod +x "$root/.github/scripts/zz-a.sh" "$root/.github/scripts/zz-0.sh" \
  "$root/.github/scripts/zz-c.sh"
sed 's/zz-b\.sh/zz-0.sh/' "$work/shared-script-most-expensive/.github/workflows/case.yaml" \
  > "$root/.github/workflows/case.yaml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges zz-c.sh once for each of its four runs" \
  "$(contains "$out" 'its own retries take 4200 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 71')"

# The loop waits of the path a file is charged through are charged at every call of that path,
# each multiplied by the passes above it, and not at its last call alone. The step runs l-a.sh
# twice, each pass ending with `sleep 120`, l-a.sh runs l-b.sh three times, each pass ending with
# `sleep 20`, and l-b.sh retries at 180 s, 5 x 180 + 150 = 1050 s. The step waits
# 6 x 1050 = 6300 s for the attempts, 2 x 120 = 240 s in its own loop and 2 x 60 = 120 s in
# l-a.sh's, 6660 s, which `timeout-minutes: 108`, 6480 s, does not exceed; 6660 s is 111
# minutes, so the key that holds it is 112. A walk charging the waits of the outer call alone
# (6540 s), of the inner call alone (6420 s), of neither (6300 s), or taking them away (5940 s)
# prints another figure, and each but the first passes the key; round 36's mutation pass of
# dispatch://INFRA-1545 found all four standing under the reading of then, which charged each
# sleep once fewer times than its loop's passes and this step 6500 s (round 44).
echo "case: the loop waits along a nested path are charged at every call of it"
root=$(fixture path-waits-nested)
cat > "$root/.github/scripts/l-b.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/scripts/l-a.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
for pass in 1 2 3; do
  .github/scripts/l-b.sh
  sleep 20
done
SH
chmod +x "$root/.github/scripts/l-a.sh" "$root/.github/scripts/l-b.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through a nest of loops
        timeout-minutes: 108
        run: |
          for pass in 1 2; do
            .github/scripts/l-a.sh
            sleep 120
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges both loops' waits along the path, once each" \
  "$(contains "$out" 'its own retries take 6660 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 112')"

# Two loops nested in one text, each sleeping in its passes, are read from the inner one out
# (close_loop): the outer loop runs the inner loop, its waits among it, once for each of its own
# passes. The step runs r43-fetch.sh, which retries at 30 s, 5 x 30 + 150 = 300 s, in two passes
# of an inner loop that ends each pass with `sleep 600`, inside two passes of an outer loop that
# ends each with `sleep 480`. It waits 4 x 300 = 1200 s for the attempts, 4 x 600 = 2400 s for
# the inner loop's sleeps and 2 x 480 = 960 s for the outer loop's, 4560 s, which
# `timeout-minutes: 48`, 2880 s, does not exceed, and 4560 s is 76 minutes, so the key that holds
# it is 77. A reading that adds the inner loop's waits to the outer loop's once, rather than at
# each of its passes, charges 3360 s. Round 43 asserted 2880 s, which charged
# each sleep once fewer times than its loop's passes; bash runs the sleep that ends a pass after
# the last pass too, and round 44 charges it at every pass (dispatch://INFRA-1545, rounds 43 and
# 44).
echo "case: r43 two loops nested in one text, each sleeping, charge the inner loop's waits at every pass of the outer"
root=$(fixture r43-nested-loop-waits)
cat > "$root/.github/scripts/r43-fetch.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
SH
chmod +x "$root/.github/scripts/r43-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch inside two nested loops, each sleeping between its passes
        timeout-minutes: 48
        run: |
          for round in 1 2; do
            for pass in 1 2; do
              .github/scripts/r43-fetch.sh
              sleep 600
            done
            sleep 480
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges the inner loop's waits at both passes of the outer one" \
  "$(contains "$out" 'its own retries take 4560 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 77')"

# T13. A `sleep` written as the last command of a loop's body runs after every pass, the last one
# included, so a loop of three passes sleeps three times. around() adds `(passes - 1) * sleep`,
# which is right only where the body skips its last sleep, as
# `.github/workflows/dequeued.yaml#[ "$attempt" = 7 ] || sleep 10` does. The step runs
# r44-fetch.sh, which retries at 30 s, 5 x 30 + 150 = 300 s, in three passes, each followed by
# `sleep 300`: bash waits
# 3 x 300 + 3 x 300 = 1800 s, which `timeout-minutes: 29`, 1740 s, does not exceed. At c53b3c8d
# the gate charges 3 x 300 + 2 x 300 = 1500 s and passes the step. Timed in bash, with every
# figure divided by 100, r43's step above ran 45.9 s where the gate charges it 28.8 s, and under
# the 49 minutes the gate names for it, scaled to 29.4 s, timeout ended it with exit 124
# (dispatch://INFRA-1545, the tester's round 43). The case asks only that the gate exits 1 with no
# traceback, so a refusal by name or a charged figure both pass it.
echo "case: r44 T13 a sleep that ends a loop's body is charged at every pass, the last included"
root=$(fixture r44-T13-trailing-sleep)
cat > "$root/.github/scripts/r44-fetch.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
SH
chmod +x "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch three times, sleeping after each pass
        timeout-minutes: 29
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 300
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

# T14. A sleep in a loop whose passes the gate reads, around a fetch, written in a form other than
# a bare `sleep <digits>`. Each step below runs r44-fetch.sh, 300 s, in three passes that each
# sleep 300 s, so bash waits 3 x 300 + 3 x 300 = 1800 s, which `timeout-minutes: 29`, 1740 s, does
# not exceed. At f470f13261 the gate charges each of these sleeps 0 s and passes all five steps:
# `sleep 5m` (GNU sleep's minutes), `sleep 100 200` (two arguments, which sleep sums),
# `sleep 299.9` (a decimal; bash waits 1799.7 s), `command sleep 300` (a sleep behind a wrapper)
# and a function the body calls whose own body sleeps. A sleep the gate prices exactly is priced,
# and one it cannot is refused by name (INFRA-1310's architect, dispatch://INFRA-1545, the
# tester's round 44). Each step asks only to be named in a refusal, so a charged figure or a
# refusal by name both pass it.
echo "case: r45 T14 a sleep in a priced loop that is no bare sleep of whole seconds is not charged nothing"
root=$(fixture r45-T14-unpriced-sleeps)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Sleep in minutes
        timeout-minutes: 29
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 5m
          done
      - name: Sleep two arguments
        timeout-minutes: 29
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 100 200
          done
      - name: Sleep a decimal
        timeout-minutes: 29
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 299.9
          done
      - name: Sleep behind command
        timeout-minutes: 29
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            command sleep 300
          done
      - name: Sleep in a function the loop calls
        timeout-minutes: 29
        run: |
          nap() { sleep 300; }
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            nap
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for r45_step in "Sleep in minutes" "Sleep two arguments" "Sleep a decimal" "Sleep behind command" \
  "Sleep in a function the loop calls"; do
  check "refuses or charges '$r45_step'" "$(contains "$out" "^::error file=.github/workflows/case.yaml,line=[0-9]*::step '$r45_step' ")"
done

# T15. A library the loop sources by its literal path, whose own top level sleeps. Each pass runs
# r44-fetch.sh, 300 s, and then `. .github/scripts/r46-nap.sh`, which holds `sleep 300` at its top
# level, so bash waits 3 x 300 + 3 x 300 = 1800 s, which `timeout-minutes: 29`, 1740 s, does not
# exceed. At 95587c62 the gate charges that sleep 0 s and passes the step, where it refuses by name
# a script the loop runs whose text sleeps, and a sourced library's function the loop calls whose
# body sleeps (INFRA-1310's architect, dispatch://INFRA-1545, the tester's round 45). The step asks
# only to be named in a refusal, so a charged figure or a refusal by name both pass it.
echo "case: r46 T15 a library a priced loop sources, whose own top level sleeps, is not charged nothing"
root=$(fixture r46-T15-sourced-sleep)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/scripts/r46-nap.sh" <<'SH'
#!/usr/bin/env bash
# Sourced: waits between two fetches of the loop that sources it.
sleep 300
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source a sleeping library after each fetch
        timeout-minutes: 29
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            . .github/scripts/r46-nap.sh
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses or charges the step, or refuses the library's sleep" \
  "$(contains "$out" '^::error .*\(Source a sleeping library after each fetch\|r46-nap\.sh\)')"

# A loop's own waits are each `sleep <n>` of its body, charged as bash runs it (sleep_of;
# dispatch://INFRA-1545, round 44). Two sleeps written as commands of their own both run after
# every pass: the first step runs r44-fetch.sh, 300 s, in three passes that each sleep 100 s and
# 200 s, 3 x 300 + 3 x 300 = 1800 s, which `timeout-minutes: 29`, 1740 s, does not exceed, and
# 1800 s is 30 minutes, so the key that holds it is 31. A reading keeping a loop's longest sleep
# alone charged 3 x 300 + 3 x 200 = 1500 s, and the reading before round 44, which also charged
# that sleep once fewer times than the passes, 1300 s: each passed the step. A sleep in one branch
# of an `if` runs at most once in a pass, so the `if` is charged its longest branch at every
# pass: the second step's passes sleep 200 s, 200 s and 100 s in bash, 500 s, and are charged
# 3 x 200 = 600 s, 1500 s with the attempts, which `timeout-minutes: 25`, 1500 s, does not
# strictly exceed. Summing the branches charged 1800 s, and charging the longest branch once
# fewer times than the passes 1300 s, which passed the step.
echo "case: r44 two sleeps of one pass are both charged at every pass, and an if its longest branch"
root=$(fixture r44-two-sleeps)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Sleep twice in each pass
        timeout-minutes: 29
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 100
            sleep 200
          done
      - name: Sleep in one branch of an if in each pass
        timeout-minutes: 25
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            if [ "$pass" = 3 ]; then
              sleep 100
            else
              sleep 200
            fi
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "charges both sleeps at every pass" \
  "$(contains "$out" "step 'Sleep twice in each pass' carries .timeout-minutes: 29., 1740 s, and its own retries take 1800 s to exhaust")"
check "names the budget that would hold them" \
  "$(contains "$out" "step 'Sleep twice in each pass' carries .*write .timeout-minutes: 31. or more")"
check "charges the if's longest branch at every pass" \
  "$(contains "$out" "step 'Sleep in one branch of an if in each pass' carries .timeout-minutes: 25., 1500 s, and its own retries take 1500 s to exhaust")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# The one sleep charged at every pass but the last is one the text skips on the last pass, read
# literally in the form dequeued.yaml writes, `[ "$attempt" = 7 ] || sleep 10`: the loop's own
# variable tested against the last word of its list (last_pass_guard; dispatch://INFRA-1545,
# round 44). Each step runs r44-fetch.sh, 300 s, in three passes, the first two sleeping 300 s:
# 3 x 300 + 2 x 300 = 1500 s, which `timeout-minutes: 25`, 1500 s, does not strictly exceed and
# `timeout-minutes: 26`, 1560 s, does. Charging the guarded sleep at every pass, 1800 s, refused
# both; charging it at one pass fewer still, 1200 s, passed both.
echo "case: r44 a sleep the text skips on the last pass is charged at every pass but the last"
root=$(fixture r44-last-pass-guard)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Skip the last pass's sleep under a key its waits reach
        timeout-minutes: 25
        run: |
          for attempt in 1 2 3; do
            .github/scripts/r44-fetch.sh
            [ "$attempt" = 3 ] || sleep 300
          done
      - name: Skip the last pass's sleep under a key above its waits
        timeout-minutes: 26
        run: |
          for attempt in 1 2 3; do
            .github/scripts/r44-fetch.sh
            [ "$attempt" = 3 ] || sleep 300
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "charges the guarded sleep at the first two passes" \
  "$(contains "$out" "step 'Skip the last pass's sleep under a key its waits reach' carries .timeout-minutes: 25., 1500 s, and its own retries take 1500 s to exhaust")"
check "names the budget that would hold them" \
  "$(contains "$out" "step 'Skip the last pass's sleep under a key its waits reach' carries .*write .timeout-minutes: 26. or more")"
check "passes the same loop under 26 minutes" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A sleep after any other test of an `&&` or an `||` runs at the passes that test lets it, which
# this check does not read, so it is refused by name where its loop holds a fetch, with the test
# written as the `if` that sleeps at the same passes as its cure (SLEEP_GUARD_REFUSAL;
# dispatch://INFRA-1545, round 44). The `||` test of another pass than the last, `[ "$attempt" =
# 2 ] || sleep 300`, skips the sleep at the second pass and sleeps at the third; the `&&` test
# sleeps at the first two. Both run under keys their attempts and sleeps cannot reach, so each
# refusal is by name and of no figure. The cure's `if` is charged at every pass,
# 3 x 300 + 3 x 300 = 1800 s, which `timeout-minutes: 31`, 1860 s, exceeds.
echo "case: r44 a sleep after another test of an && or an || is refused by name, and its if passes"
root=$(fixture r44-guarded-sleep)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Sleep after an and test
        timeout-minutes: 60
        run: |
          for attempt in 1 2 3; do
            .github/scripts/r44-fetch.sh
            [ "$attempt" -lt 3 ] && sleep 300
          done
      - name: Sleep after an or test of another pass
        timeout-minutes: 60
        run: |
          for attempt in 1 2 3; do
            .github/scripts/r44-fetch.sh
            [ "$attempt" = 2 ] || sleep 300
          done
      - name: Sleep in the if the refusal advises
        timeout-minutes: 31
        run: |
          for attempt in 1 2 3; do
            .github/scripts/r44-fetch.sh
            if [ "$attempt" -lt 3 ]; then sleep 300; fi
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the sleep after && by name, with the if as its cure" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Sleep after an and test' of job 'build' writes .sleep 300. after .&&. in the loop over .attempt. opened on line 10, and a fetch, or a call of a script that fetches, is in that loop\..*cannot tell how many of the loop's passes wait for this one\. Write the commands before the .&&. as the test of an .if. around the sleep, .if <test>; then sleep 300; fi.")"
check "refuses the sleep after || by name, with the negated if as its cure" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=19::step 'Sleep after an or test of another pass' of job 'build' writes .sleep 300. after .||. in the loop over .attempt. opened on line 17,.*.if ! <test>; then sleep 300; fi.*write .\[ \"\$attempt\" = 3 \] || sleep 300.")"
check "passes the if, charged at every pass" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# The tester's item 5 of round 44 (dispatch://INFRA-1545): a word that could name a script that
# fetches, written where a construct and not a program takes it, was refused as an argument of
# the construct's first word, `f` for a loop's list and `-e` and `$1` for a `[[ … ]]` and a
# `case`, and with the clause that bash fills in its patterns from the files, which it does not
# do inside `[[ … ]]` or from a `case` pattern (word_construct; round 44). Each refusal now says
# what the construct does with the word, and each cure it writes passes, as the last five steps
# show, and runs the same way in bash: a loop over the names `printf` prints, the same list where
# no name holds a blank; the `[[ … ]]`'s word quoted, where bash reads it as text either way; and
# the `case` pattern's dot written `[.]`, which matches the same text.
echo "case: r44 a pattern in a loop's list, a [[ … ]] or a case is refused for what that construct does"
root=$(fixture r44-construct-words)
cp "$work/shared-script-most-expensive/.github/scripts/zz-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/zz-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Print each name of a for loop's list
        timeout-minutes: 30
        run: for f in .github/scripts/zz-*.sh; do echo "$f"; done
      - name: Print each name of a for loop's list that names a file
        timeout-minutes: 30
        run: for f in .github/scripts/zz-*.sh; do [ -e "$f" ] || continue; echo "$f"; done
      - name: Offer each name of a select loop's list
        timeout-minutes: 30
        run: select f in .github/scripts/zz-*.sh; do break; done </dev/null
      - name: Test the word in a double-bracket test
        timeout-minutes: 30
        run: '[[ -e .github/scripts/zz-*.sh ]] && echo yes'
      - name: Match the word in a case pattern
        timeout-minutes: 30
        run: case "$1" in .github/scripts/zz-*.sh) echo m;; esac
      - name: Print each name printf prints
        timeout-minutes: 30
        run: for f in $(printf '%s\n' .github/scripts/zz-*.sh); do echo "$f"; done
      - name: Print each name printf prints that names a file
        timeout-minutes: 30
        run: for f in $(printf '%s\n' .github/scripts/zz-*.sh); do [ -e "$f" ] || continue; echo "$f"; done
      - name: Offer each name printf prints
        timeout-minutes: 30
        run: select f in $(printf '%s\n' .github/scripts/zz-*.sh); do break; done </dev/null
      - name: Test the quoted word in a double-bracket test
        timeout-minutes: 30
        run: '[[ -e ".github/scripts/zz-*.sh" ]] && echo yes'
      - name: Match the word with its dot in a bracket
        timeout-minutes: 30
        run: case "$1" in [.]github/scripts/zz-*.sh) echo m;; esac
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "9	Print each name of a for loop's list	for" \
  "12	Print each name of a for loop's list that names a file	for" \
  "15	Offer each name of a select loop's list	select"; do
  IFS=$'\t' read -r line step loop <<<"$refusal"
  check "refuses '$step' as a word of the loop's list, filled in, with the printf loop as its cure" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' writes ..github/scripts/zz-\*\.sh. in the list of the .$loop. loop over .f., which could name .github/scripts/zz-c.sh, a script that fetches or a file this check does not read, since bash fills in its patterns and expansions from the files .*Bash runs the loop's body once for each name the list fills in, with .f. set to it, .*.$loop f in .(printf '%s\\\\n' \.github/scripts/zz-\*\.sh); do.")"
done
check "refuses the [[ … ]] word as text bash fills in no file names from, with the quoted word as its cure" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=18::step 'Test the word in a double-bracket test' of job 'build' writes ..github/scripts/zz-\*\.sh. in a .\[\[ .* test, after .-e.\. Bash fills in no file names inside .\[\[ .*: the word is the one path its text spells.*as this one could name .github/scripts/zz-c.sh\. Quote the word, .\"\.github/scripts/zz-\*\.sh\".")"
check "refuses the case pattern as text bash fills in no file names from, with the bracketed dot as its cure" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=21::step 'Match the word in a case pattern' of job 'build' writes ..github/scripts/zz-\*\.sh. as a pattern of the .case. over .\$1.\. Bash fills in no file names from a .case. pattern: the pattern is matched against .\$1. as text.*as this one could name .github/scripts/zz-c.sh\. Write the dot that opens .\.github/. as a bracket expression, .\[\.\]github/scripts/zz-\*\.sh.")"
check "names no construct's first word as a program" \
  "$(is "$(contains "$out" 'as an argument of .\(f\|-e\|\$1\).')" false)"
check "passes each cure" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"

# A loop in the body of a loop runs, its sleeps among it, at each pass of the outer one, whether
# the fetch is in it or beside it (close_loop; dispatch://INFRA-1545, round 44). The first two
# steps run r44-fetch.sh, 300 s, in two passes, each of which runs a loop of three passes
# sleeping 100 s: 2 x 300 + 2 x 3 x 100 = 1200 s, which `timeout-minutes: 20`, 1200 s, does not
# strictly exceed and `timeout-minutes: 21`, 1260 s, does. Until round 44 the loop beside the
# fetch was charged nothing, 600 s, which passed both. A loop there whose passes this check does
# not read, a `while` poll that sleeps, is refused by name, and the counted poll its refusal
# advises is charged: 2 x 300 + 2 x 5 x 10 = 700 s, which `timeout-minutes: 12`, 720 s, exceeds.
echo "case: r44 a loop that sleeps beside a fetch is charged at every pass of the loop around both"
root=$(fixture r44-loop-beside-fetch)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Sleep in a loop beside the fetch under a key its waits reach
        timeout-minutes: 20
        run: |
          for i in 1 2; do
            .github/scripts/r44-fetch.sh
            for j in 1 2 3; do
              sleep 100
            done
          done
      - name: Sleep in a loop beside the fetch under a key above its waits
        timeout-minutes: 21
        run: |
          for i in 1 2; do
            .github/scripts/r44-fetch.sh
            for j in 1 2 3; do
              sleep 100
            done
          done
      - name: Poll in a loop beside the fetch
        timeout-minutes: 60
        run: |
          for i in 1 2; do
            .github/scripts/r44-fetch.sh
            while [ ! -e ready ]; do
              sleep 10
            done
          done
      - name: Poll in the counted loop the refusal advises
        timeout-minutes: 12
        run: |
          for i in 1 2; do
            .github/scripts/r44-fetch.sh
            for try in 1 2 3 4 5; do
              [ -e ready ] && break
              sleep 10
            done
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "charges the loop beside the fetch at both passes of the loop around them" \
  "$(contains "$out" "step 'Sleep in a loop beside the fetch under a key its waits reach' carries .timeout-minutes: 20., 1200 s, and its own retries take 1200 s to exhaust")"
check "names the budget that would hold them" \
  "$(contains "$out" "step 'Sleep in a loop beside the fetch under a key its waits reach' carries .*write .timeout-minutes: 21. or more")"
check "refuses the uncounted poll beside the fetch by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=30::step 'Poll in a loop beside the fetch' of job 'build' writes a loop on line 30, inside the loop opened on line 28, whose passes this check does not read and whose body sleeps, and a fetch, or a call of a script that fetches, is in that outer loop beside it\..*Write this loop's passes as literal words")"
check "passes the same loop under 21 minutes and the counted poll under 12" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# The guard `[ "$attempt" = 3 ] || sleep 300` skips the last pass only where the loop's body sets
# `attempt` nowhere (sets_variable; dispatch://INFRA-1545, round 44). The first step sets it to 0
# before the test, so the test never holds and bash sleeps at all three passes, 900 s, where the
# guard read as skipping the last pass charged 600 s; the second counts it up after the test.
# Each is refused by name. The third counts in a variable of its own and is charged
# 3 x 300 + 2 x 300 = 1500 s, which `timeout-minutes: 26`, 1560 s, exceeds.
echo "case: r44 a last-pass guard whose variable the body sets is refused by name"
root=$(fixture r44-guard-variable-set)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Set the guard's variable in the body
        timeout-minutes: 60
        run: |
          for attempt in 1 2 3; do
            .github/scripts/r44-fetch.sh
            attempt=0
            [ "$attempt" = 3 ] || sleep 300
          done
      - name: Count the guard's variable up in the body
        timeout-minutes: 60
        run: |
          for attempt in 1 2 3; do
            .github/scripts/r44-fetch.sh
            [ "$attempt" = 3 ] || sleep 300
            ((attempt++))
          done
      - name: Count in a variable of its own
        timeout-minutes: 26
        run: |
          for attempt in 1 2 3; do
            .github/scripts/r44-fetch.sh
            tries=$((tries + 1))
            [ "$attempt" = 3 ] || sleep 300
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the guard whose variable is set before it, by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'Set the guard's variable in the body' of job 'build' writes .sleep 300. after .||. in the loop over .attempt. opened on line 10,.*This test is written in that form, but the loop's body sets .attempt. on line 12,.*.if ! \[ \"\$attempt\" = 3 \]; then sleep 300; fi.")"
check "refuses the guard whose variable is counted up after it, by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=20::step 'Count the guard's variable up in the body' of job 'build' writes .sleep 300. after .||. .*the loop's body sets .attempt. on line 21,")"
check "passes the guard of a loop counting in a variable of its own" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A loop's sleep is priced where the bare word `sleep` opens it and each of its arguments is a
# literal number, with an optional `s`, `m`, `h` or `d`, decimals included: the arguments are
# summed, as GNU sleep sums them, and the total is rounded up to a whole second (sleep_form;
# dispatch://INFRA-1545, round 45). Until round 45 only `sleep <digits>` was priced, and each sleep
# below was charged 0 s. Each step runs r44-fetch.sh, 300 s, in three passes. `sleep 5m`, `sleep
# 100s 200` and `sleep 299.9` each sleep 300 s a pass, 299.9 rounded up: 3 x 300 + 3 x 300 =
# 1800 s, which `timeout-minutes: 29`, 1740 s, does not exceed, and the key that holds it is 31.
# `sleep 0.05h` sleeps 180 s a pass, 900 + 540 = 1440 s, which `timeout-minutes: 24`, 1440 s, does
# not strictly exceed. `sleep 0.001d` sleeps 86.4 s, charged 87 s a pass, 900 + 261 = 1161 s,
# over `timeout-minutes: 19`, 1140 s; and `sleep 0.4 0.4` sleeps 0.8 s, charged 1 s a pass,
# 900 + 3 = 903 s, over `timeout-minutes: 15`, 900 s, where rounding each argument up would
# charge 906 s and rounding down 900 s. The last-pass guard reads a sleep in minutes as round 44
# reads one in seconds: 3 x 300 + 2 x 300 = 1500 s, which `timeout-minutes: 25`, 1500 s, does not
# strictly exceed. `sleep 5m` under the 31 minutes its refusal advises passes.
echo "case: r45 a sleep's seconds are read in each unit, summed and rounded up to a whole second"
root=$(fixture r45-sleep-units)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Sleep five minutes
        timeout-minutes: 29
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 5m
          done
      - name: Sleep a suffixed count and a bare one
        timeout-minutes: 29
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 100s 200
          done
      - name: Sleep a decimal of seconds
        timeout-minutes: 29
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 299.9
          done
      - name: Sleep a decimal of hours
        timeout-minutes: 24
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 0.05h
          done
      - name: Sleep a decimal of days
        timeout-minutes: 19
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 0.001d
          done
      - name: Sleep two decimals whose sum is under a second
        timeout-minutes: 15
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 0.4 0.4
          done
      - name: Skip the last pass's sleep in minutes
        timeout-minutes: 25
        run: |
          for attempt in 1 2 3; do
            .github/scripts/r44-fetch.sh
            [ "$attempt" = 3 ] || sleep 5m
          done
      - name: Sleep five minutes under the key the refusal advises
        timeout-minutes: 31
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 5m
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for r45_sleep in "Sleep five minutes	29	1740	1800	31" \
  "Sleep a suffixed count and a bare one	29	1740	1800	31" \
  "Sleep a decimal of seconds	29	1740	1800	31" \
  "Sleep a decimal of hours	24	1440	1440	25" \
  "Sleep a decimal of days	19	1140	1161	20" \
  "Sleep two decimals whose sum is under a second	15	900	903	16" \
  "Skip the last pass's sleep in minutes	25	1500	1500	26"; do
  IFS=$'\t' read -r r45_step r45_minutes r45_key r45_figure r45_advised <<<"$r45_sleep"
  check "charges '$r45_step' $r45_figure s" \
    "$(contains "$out" "step '$r45_step' carries .timeout-minutes: $r45_minutes., $r45_key s, and its own retries take $r45_figure s to exhaust")"
  check "advises '$r45_step' $r45_advised minutes" \
    "$(contains "$out" "step '$r45_step' carries .*write .timeout-minutes: $r45_advised. or more")"
done
check "passes the five minutes under 31 minutes" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 7)"

# A sleep in a loop whose passes this check reads, around a fetch, that this check cannot price
# is refused by name, with a reason true of the form it is written in and a cure (sleep_form,
# SLEEP_FORM_REFUSAL, SLEEP_FUNCTION_REFUSAL; dispatch://INFRA-1545, round 45). Until round 45
# each was charged 0 s with nothing said: three passes of r44-fetch.sh, 300 s, each sleeping
# 300 s, wait 1800 s in bash and were charged 900 s. A sleep behind `command` or `timeout` is a
# command those words open; one in the body of a function the loop calls runs at each call, and
# this check charges a loop only the sleeps written inside it; `+300` carries a sign; and
# `"$wait"` is filled in as the step runs. Each step runs under 60 minutes, which no figure of
# these reaches, so each refusal is by name. The cure, `sleep 300` in the loop's body, is charged
# 1800 s, which `timeout-minutes: 31`, 1860 s, exceeds.
echo "case: r45 a sleep in a priced loop that this check cannot price is refused by name, and its cure passes"
root=$(fixture r45-sleep-refusals)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Sleep behind command
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            command sleep 300
          done
      - name: Sleep behind timeout
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            timeout 900 sleep 300
          done
      - name: Sleep in a function the loop calls
        timeout-minutes: 60
        run: |
          nap() { sleep 300; }
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            nap
          done
      - name: Sleep a signed count
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep +300
          done
      - name: Sleep the count a variable holds
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep "$wait"
          done
      - name: Sleep the literal count the refusals advise
        timeout-minutes: 31
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sleep 300
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the sleep behind command by name, with the bare sleep as its cure" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Sleep behind command' of job 'build' writes .command sleep 300. in the loop opened on line 10, and a fetch, or a call of a script that fetches, is in that loop\..* Here the word that opens the command is .command., not .sleep., so this check charges this sleep nothing\. Write the sleep as a command of its own that .sleep. opens, its seconds literal numbers, such as .sleep 300.")"
check "refuses the sleep behind timeout by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=19::step 'Sleep behind timeout' of job 'build' writes .timeout 900 sleep 300. in the loop opened on line 17,.* Here the word that opens the command is .timeout., not .sleep.")"
check "refuses the sleep in the function the loop calls by name, with the sleep in the loop as its cure" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=24::step 'Sleep in a function the loop calls' of job 'build' writes .sleep 300. in the body of the function .nap., which a call in the loop opened on line 25 runs, and a fetch, or a call of a script that fetches, is in that loop\. Bash runs the sleep at each call of .nap.,.* Write the sleep in the loop's body, where .nap. is called")"
check "refuses the signed count by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=34::step 'Sleep a signed count' of job 'build' writes .sleep +300. in the loop opened on line 32,.* Its argument .+300. is written with a sign, which this check does not read as seconds.* Write its seconds as literal numbers")"
check "refuses the variable's count by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=41::step 'Sleep the count a variable holds' of job 'build' writes .sleep \"\$wait\". in the loop opened on line 39,.* Its seconds are written as an expansion, which is filled in only as the step runs.* Write its seconds as literal numbers")"
check "passes the literal count under 31 minutes" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"

# A sleep outside every loop that holds a fetch stays as it was before round 45: charged nothing
# and refused nothing, whatever its form (dispatch://INFRA-1545, round 45). The first step sleeps
# before and after its one fetch, in forms a loop around a fetch has refused since round 45, and
# calls a function that sleeps; the second sleeps in those forms in a loop beside the fetch that
# holds none, and in a `while` poll, as the tree's readiness loops of `sleep 0.1` do. Each step
# is charged its fetch alone, 300 s, which `timeout-minutes: 6`, 360 s, exceeds.
echo "case: r45 a sleep outside a loop that holds a fetch is neither charged nor refused"
root=$(fixture r45-sleep-outside)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Sleep before and after the one fetch
        timeout-minutes: 6
        run: |
          nap() { sleep "$wait"; }
          sleep "$wait"
          command sleep 300
          sleep 5m
          .github/scripts/r44-fetch.sh
          nap
          sleep infinity
      - name: Sleep in loops beside the fetch that hold none
        timeout-minutes: 6
        run: |
          nap() { sleep 5m; }
          .github/scripts/r44-fetch.sh
          for pass in 1 2 3; do
            sleep "$wait"
            command sleep 300
            nap
          done
          while [ ! -s ready ]; do
            sleep 0.1
          done
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses nothing" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 0)"

# A sleep a call in a priced loop runs somewhere other than the loop's own body, where no loop of
# the file writing it charges it, is refused by name: one in a script the loop runs, or in a
# script that script runs (script_sleep, SLEEP_SCRIPT_REFUSAL); one in the body of a function of a
# library the text sources (library_sleeps, SLEEP_LIBRARY_REFUSAL); and one in the body of a
# function defined inside the loop, which this check reads as a command of the loop's body, once
# a pass, where a pass calling it twice sleeps twice (SLEEP_FUNCTION_INSIDE;
# dispatch://INFRA-1545, round 45). Until then the first three were charged 0 s and the fourth
# 300 s a pass: the fourth step's passes each sleep 600 s. Each step runs r44-fetch.sh, 300 s, in
# three passes, under 60 minutes, which no figure of these reaches, so each refusal is by name.
# The script, the library function and the function defined in the loop that sleep nowhere pass,
# and so does r45-polls.sh, whose sleep is in a loop of its own around a fetch and is charged
# there at each run: 3 x 300 + 3 x (2 x 300 + 2 x 10) = 2760 s.
echo "case: r45 a sleep a priced loop runs in a script, a library function or a function defined in it is refused by name"
root=$(fixture r45-callee-sleeps)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
printf '#!/usr/bin/env bash\nsleep 300\n' > "$root/.github/scripts/r45-nap.sh"
printf '#!/usr/bin/env bash\n.github/scripts/r45-nap.sh\n' > "$root/.github/scripts/r45-runs-nap.sh"
printf '#!/usr/bin/env bash\necho "nothing to wait for"\n' > "$root/.github/scripts/r45-quiet.sh"
printf '#!/usr/bin/env bash\nfor try in 1 2; do\n  .github/scripts/r44-fetch.sh\n  sleep 10\ndone\n' \
  > "$root/.github/scripts/r45-polls.sh"
printf '#!/usr/bin/env bash\nlib_nap() { sleep 300; }\nlib_quiet() { echo quiet; }\n' \
  > "$root/.github/scripts/r45-lib.sh"
chmod +x "$root/.github/scripts/r44-fetch.sh" "$root/.github/scripts/r45-nap.sh" \
  "$root/.github/scripts/r45-runs-nap.sh" "$root/.github/scripts/r45-quiet.sh" \
  "$root/.github/scripts/r45-polls.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a sleeping script in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            .github/scripts/r45-nap.sh
          done
      - name: Run a script that runs a sleeping script in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            .github/scripts/r45-runs-nap.sh
          done
      - name: Run a quiet script in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            .github/scripts/r45-quiet.sh
          done
      - name: Call a sleeping function of a sourced library in the loop
        timeout-minutes: 60
        run: |
          . .github/scripts/r45-lib.sh
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            lib_nap
          done
      - name: Call a quiet function of a sourced library in the loop
        timeout-minutes: 60
        run: |
          . .github/scripts/r45-lib.sh
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            lib_quiet
          done
      - name: Call a sleeping function defined in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            snooze() { sleep 300; }
            .github/scripts/r44-fetch.sh
            snooze
            snooze
          done
      - name: Call a quiet function defined in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            hush() { echo hush; }
            .github/scripts/r44-fetch.sh
            hush
          done
      - name: Run a script that sleeps in its own loop around a fetch
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            .github/scripts/r45-polls.sh
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the run of the sleeping script by name, with the sleep in the loop as its cure" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Run a sleeping script in the loop' of job 'build' runs .\.github/scripts/r45-nap\.sh. in the loop opened on line 10, and a fetch, or a call of a script that fetches, is in that loop; \.github/scripts/r45-nap\.sh writes .sleep 300. on its line 2, outside every loop of that file whose passes this check reads around a fetch\..* Write the wait in this loop's body as the bare word .sleep. and literal seconds")"
check "refuses the run of the script that runs the sleeping script by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=19::step 'Run a script that runs a sleeping script in the loop' of job 'build' runs .\.github/scripts/r45-runs-nap\.sh. in the loop opened on line 17,.*; \.github/scripts/r45-nap\.sh, which that run reaches, writes .sleep 300. on its line 2, outside every loop")"
check "refuses the call of the library's sleeping function by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=34::step 'Call a sleeping function of a sourced library in the loop' of job 'build' calls .lib_nap., which \.github/scripts/r45-lib\.sh defines, in the loop opened on line 32,.*; \.github/scripts/r45-lib\.sh writes .sleep 300. on its line 2, in the body of .lib_nap., which that call runs\.")"
check "refuses the sleeping function defined in the loop by name, for the calls a pass makes" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=48::step 'Call a sleeping function defined in the loop' of job 'build' writes .sleep 300. in the body of the function .snooze., which a call in the loop opened on line 47 runs,.* this check reads a sleep of a function defined in a loop's body as a command of that body, charged once a pass at most, so a pass that calls .snooze. more than once is charged less than it sleeps\.")"
check "passes the quiet script, the quiet library function, the quiet function defined in the loop and the script sleeping in its own loop around a fetch" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# A script a priced loop runs whose own sleep sits in a loop of that script that holds no fetch,
# beside the fetch it makes: bash runs that loop, its sleeps among them, at each run of the
# script, and this check charges a run the fetches it makes, each with the sleeps of a loop
# around it there, so that loop's sleeps are no fetch's, and the run is refused by name
# (script_sleep, charged_where_written; dispatch://INFRA-1545, round 47). pf.sh retries at 30 s,
# 5 x 30 + 150 = 300 s, then sleeps in `for i in 1 2; do sleep 100; done`, 200 s, and the step
# runs it in three passes: bash waits 3 x (300 + 200) = 1500 s, which `timeout-minutes: 16`,
# 960 s, does not exceed, where the fetches alone, 3 x 300 = 900 s, are under it. Review round
# 31's mutant M16, charged_where_written without its `holds_fetch` test, reads that loop of pf.sh
# as one charging its sleep where it is written, so it refuses nothing and charges the step
# 900 s, and the review found the sleep and loop cases above miss it.
echo "case: r47 a script a priced loop runs, sleeping in a loop of its own that holds no fetch, is refused by name"
root=$(fixture r47-fetchless-loop-sleep)
cat > "$root/.github/scripts/pf.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
for i in 1 2; do
  sleep 100
done
SH
chmod +x "$root/.github/scripts/pf.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a script whose sleep is in a loop of its own that holds no fetch
        timeout-minutes: 16
        run: |
          for p in 1 2 3; do
            .github/scripts/pf.sh
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the run of pf.sh by name, naming the sleep of its fetchless loop" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Run a script whose sleep is in a loop of its own that holds no fetch' of job 'build' runs .\.github/scripts/pf\.sh. in the loop opened on line 10, and a fetch, or a call of a script that fetches, is in that loop; \.github/scripts/pf\.sh writes .sleep 100. on its line 6, outside every loop of that file whose passes this check reads around a fetch\.")"

# A source in a pass of a priced loop runs the file's own commands at each pass, and the sleeps
# they reach are refused by name (script_sleep with `sourced`, SLEEP_SOURCE_REFUSAL;
# dispatch://INFRA-1545, round 46): a sleep written at the file's top level, one in the body of a
# function its top level calls, and one in a file it sources in turn, in a loop of three passes,
# and a source in a loop of one pass, which is a recognised call. The functions of a file sourced
# in a pass are read at the pass's calls of them as a library's (SLEEP_LIBRARY_REFUSAL). Until
# round 46 each was charged 0 s: the first step waits 3 x 300 + 3 x 300 = 1800 s in bash, charged
# 900 s (the tester's T15). Each step runs r44-fetch.sh, 300 s, under 60 minutes, which no figure
# here reaches, so each refusal is by name. A quiet file sourced in the loop passes, and so does a
# library sourced there whose sleeping function the loop never calls, since a source runs no body
# of a function its own commands do not call; so does the cure, the sleeping file sourced once
# before the loop.
echo "case: r46 a source in a pass of a priced loop is read as bash runs it, its functions at the pass's calls"
root=$(fixture r46-pass-sources)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
printf '#!/usr/bin/env bash\nsleep 300\n' > "$root/.github/scripts/r46-nap.sh"
printf '#!/usr/bin/env bash\necho "nothing to wait for"\n' > "$root/.github/scripts/r46-quiet.sh"
printf '#!/usr/bin/env bash\nown_nap() { sleep 300; }\nown_nap\n' > "$root/.github/scripts/r46-own-nap.sh"
printf '#!/usr/bin/env bash\n. .github/scripts/r46-nap.sh\n' > "$root/.github/scripts/r46-sources-nap.sh"
printf '#!/usr/bin/env bash\nlib_nap() { sleep 300; }\nlib_quiet() { echo quiet; }\n' \
  > "$root/.github/scripts/r46-lib.sh"
chmod +x "$root/.github/scripts/r44-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source a file whose own commands sleep in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            . .github/scripts/r46-nap.sh
          done
      - name: Source a file whose own commands call its sleeping function in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            . .github/scripts/r46-own-nap.sh
          done
      - name: Source a file that sources a sleeping file in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            . .github/scripts/r46-sources-nap.sh
          done
      - name: Call a sleeping function of a library sourced in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            . .github/scripts/r46-lib.sh
            lib_nap
          done
      - name: Source a sleeping file in a loop of one pass
        timeout-minutes: 60
        run: |
          for pass in 1; do
            .github/scripts/r44-fetch.sh
            . .github/scripts/r46-nap.sh
          done
      - name: Source a quiet file in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            . .github/scripts/r46-quiet.sh
          done
      - name: Source a library in the loop and call its quiet function
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            . .github/scripts/r46-lib.sh
            lib_quiet
          done
      - name: Source a sleeping file once before the loop
        timeout-minutes: 60
        run: |
          . .github/scripts/r46-nap.sh
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the source of a file whose top level sleeps, with the sleep in the loop and the source before it as its cures" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Source a file whose own commands sleep in the loop' of job 'build' sources \.github/scripts/r46-nap\.sh in a command that a pass of the loop opened on line 10 runs, and a fetch, or a call of a script that fetches, is in that loop; \.github/scripts/r46-nap\.sh writes .sleep 300. on its line 2, outside every loop of that file whose passes this check reads around a fetch, and the source runs it\..* Write the wait in this loop's body as the bare word .sleep. and literal seconds, such as .sleep 300., which this check charges at every pass, and take the sleep out of \.github/scripts/r46-nap\.sh; or source \.github/scripts/r46-nap\.sh once, before the loop that holds the fetch, where its commands run once (dispatch://INFRA-1545, round 46)\.")"
check "refuses the source of a file whose top level calls its sleeping function" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=19::step 'Source a file whose own commands call its sleeping function in the loop' of job 'build' sources \.github/scripts/r46-own-nap\.sh in a command that a pass of the loop opened on line 17 runs,.*; \.github/scripts/r46-own-nap\.sh writes .sleep 300. on its line 2,")"
check "refuses the source of a file that sources a sleeping file" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=26::step 'Source a file that sources a sleeping file in the loop' of job 'build' sources \.github/scripts/r46-sources-nap\.sh in a command that a pass of the loop opened on line 24 runs,.*; \.github/scripts/r46-nap\.sh, which that source reaches, writes .sleep 300. on its line 2,")"
check "refuses the call of a sleeping function of a library sourced in the loop" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=34::step 'Call a sleeping function of a library sourced in the loop' of job 'build' calls .lib_nap., which \.github/scripts/r46-lib\.sh defines, in the loop opened on line 31,.*; \.github/scripts/r46-lib\.sh writes .sleep 300. on its line 2, in the body of .lib_nap., which that call runs\.")"
check "refuses the source of a sleeping file in a loop of one pass" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=41::step 'Source a sleeping file in a loop of one pass' of job 'build' sources \.github/scripts/r46-nap\.sh in a command that a pass of the loop opened on line 39 runs,")"
check "passes the quiet file, the library whose sleeping function the loop never calls, and the sleeping file sourced before the loop" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"

# A file a pass names in a place this check does not read as a call (call_of) is read as a run of
# it, since that place may run it or source it, and a sleep its run reaches is refused by name
# (script_sleep, SLEEP_UNREAD_REFUSAL; dispatch://INFRA-1545, round 46), as such a place naming a
# file that fetches is refused wherever it is written (round 37): the body of a function the loop
# calls, running the file or sourcing it, a subshell, a wrapper, the operand of a shell, a shell's
# `-c` text, `xargs` and `find -exec`. Until round 46 each was charged 0 s. Each step runs
# r44-fetch.sh, 300 s, in three passes under 60 minutes, so each refusal is by name. A quiet
# script in a subshell passes, and so does a function running the sleeping script that the loop
# never calls.
echo "case: r46 a file a pass names in a place this check does not read as a call is read as a run of it"
root=$(fixture r46-pass-unread)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
printf '#!/usr/bin/env bash\nsleep 300\n' > "$root/.github/scripts/r46-nap.sh"
printf '#!/usr/bin/env bash\necho "nothing to wait for"\n' > "$root/.github/scripts/r46-quiet.sh"
chmod +x "$root/.github/scripts/r44-fetch.sh" "$root/.github/scripts/r46-nap.sh" \
  "$root/.github/scripts/r46-quiet.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a sleeping script from a function the loop calls
        timeout-minutes: 60
        run: |
          nap() { .github/scripts/r46-nap.sh; }
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            nap
          done
      - name: Source a sleeping file from a function the loop calls
        timeout-minutes: 60
        run: |
          nap_in() { . .github/scripts/r46-nap.sh; }
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            nap_in
          done
      - name: Run a sleeping script in a subshell in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            ( .github/scripts/r46-nap.sh )
          done
      - name: Run a sleeping script behind timeout in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            timeout 400 .github/scripts/r46-nap.sh
          done
      - name: Run a sleeping script with bash in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            bash .github/scripts/r46-nap.sh
          done
      - name: Run a sleeping script in bash -c text in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            bash -c '.github/scripts/r46-nap.sh'
          done
      - name: Hand a sleeping script to xargs in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            echo once | xargs .github/scripts/r46-nap.sh
          done
      - name: Hand a sleeping script to find -exec in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            find . -maxdepth 0 -exec .github/scripts/r46-nap.sh {} \;
          done
      - name: Run a quiet script in a subshell in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            ( .github/scripts/r46-quiet.sh )
          done
      - name: Define a function running a sleeping script that the loop never calls
        timeout-minutes: 60
        run: |
          idle() { .github/scripts/r46-nap.sh; }
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the run of a sleeping script in a function the loop calls, with the sleep in the loop as its cure" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Run a sleeping script from a function the loop calls' of job 'build' names \.github/scripts/r46-nap\.sh in a function body, a place this check does not read as a call, in a command that a pass of the loop opened on line 11 runs, and a fetch, or a call of a script that fetches, is in that loop; \.github/scripts/r46-nap\.sh writes .sleep 300. on its line 2, outside every loop of that file whose passes this check reads around a fetch\..* Write the wait in this loop's body as the bare word .sleep. and literal seconds, such as .sleep 300., which this check charges at every pass, and take the sleep out of \.github/scripts/r46-nap\.sh; or name \.github/scripts/r46-nap\.sh outside the loop that holds the fetch (dispatch://INFRA-1545, round 46)\.")"
for r46_place in "18	19	Source a sleeping file from a function the loop calls	a function body" \
  "28	26	Run a sleeping script in a subshell in the loop	a .( … ). subshell" \
  "35	33	Run a sleeping script behind timeout in the loop	a command behind .timeout." \
  "42	40	Run a sleeping script with bash in the loop	an argument of .bash." \
  "49	47	Run a sleeping script in bash -c text in the loop	an argument of .bash." \
  "56	54	Hand a sleeping script to xargs in the loop	an argument of .xargs." \
  "63	61	Hand a sleeping script to find -exec in the loop	an argument of .find."; do
  IFS=$'\t' read -r r46_line r46_loop r46_step r46_form <<<"$r46_place"
  check "refuses '$r46_step' by name" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$r46_line::step '$r46_step' of job 'build' names \.github/scripts/r46-nap\.sh in $r46_form, a place this check does not read as a call, in a command that a pass of the loop opened on line $r46_loop runs,")"
done
check "passes the quiet script in a subshell and the function the loop never calls" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 8)"

# A script a priced loop runs reaches the sleeps of the files it sources, read as each source runs
# them, and of the files it names in a place this check does not read as a call
# (script_sleep; dispatch://INFRA-1545, round 46), refused as round 45 refuses a script whose own
# text sleeps (SLEEP_SCRIPT_REFUSAL). Until round 46 each was charged 0 s. Each step runs
# r44-fetch.sh, 300 s, in three passes under 60 minutes. A script sourcing a library and calling
# its quiet function passes, since the source runs no body of the library's sleeping function,
# and so does one sourcing a quiet file.
echo "case: r46 a script a priced loop runs reaches the sleeps of the files it sources and names"
root=$(fixture r46-run-reaches)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
printf '#!/usr/bin/env bash\nsleep 300\n' > "$root/.github/scripts/r46-nap.sh"
printf '#!/usr/bin/env bash\necho "nothing to wait for"\n' > "$root/.github/scripts/r46-quiet.sh"
printf '#!/usr/bin/env bash\n. .github/scripts/r46-nap.sh\n' > "$root/.github/scripts/r46-sources-nap.sh"
printf '#!/usr/bin/env bash\n. .github/scripts/r46-quiet.sh\n' \
  > "$root/.github/scripts/r46-sources-quiet.sh"
printf '#!/usr/bin/env bash\nf() { .github/scripts/r46-nap.sh; }\nf\n' \
  > "$root/.github/scripts/r46-fn-runs-nap.sh"
printf '#!/usr/bin/env bash\nlib_nap() { sleep 300; }\nlib_quiet() { echo quiet; }\n' \
  > "$root/.github/scripts/r46-lib.sh"
printf '#!/usr/bin/env bash\n. .github/scripts/r46-lib.sh\nlib_quiet\n' \
  > "$root/.github/scripts/r46-sources-lib.sh"
chmod +x "$root/.github/scripts/r44-fetch.sh" "$root/.github/scripts/r46-nap.sh" \
  "$root/.github/scripts/r46-sources-nap.sh" "$root/.github/scripts/r46-sources-quiet.sh" \
  "$root/.github/scripts/r46-fn-runs-nap.sh" "$root/.github/scripts/r46-sources-lib.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a script that sources a sleeping file in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            .github/scripts/r46-sources-nap.sh
          done
      - name: Run a script whose function runs a sleeping script in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            .github/scripts/r46-fn-runs-nap.sh
          done
      - name: Run a script that sources a library and calls its quiet function in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            .github/scripts/r46-sources-lib.sh
          done
      - name: Run a script that sources a quiet file in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            .github/scripts/r46-sources-quiet.sh
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the run of a script that sources a sleeping file by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Run a script that sources a sleeping file in the loop' of job 'build' runs .\.github/scripts/r46-sources-nap\.sh. in the loop opened on line 10,.*; \.github/scripts/r46-nap\.sh, which that run reaches, writes .sleep 300. on its line 2, outside every loop")"
check "refuses the run of a script whose function runs a sleeping script by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=19::step 'Run a script whose function runs a sleeping script in the loop' of job 'build' runs .\.github/scripts/r46-fn-runs-nap\.sh. in the loop opened on line 17,.*; \.github/scripts/r46-nap\.sh, which that run reaches, writes .sleep 300. on its line 2, outside every loop")"
check "passes the script calling the library's quiet function and the one sourcing a quiet file" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A call, in a priced loop, of a library function whose body reaches a sleep through a file it
# names, or through a function it neither defines nor sources, of this text or of another library
# the text sources, which bash runs there, is refused by name at the call (library_sleeps,
# spread_sleeps, SLEEP_LIBRARY_REFUSAL; dispatch://INFRA-1545, round 46). Until round 46 each was
# charged 0 s. Each step runs r44-fetch.sh, 300 s, in three passes under 60 minutes. A library
# function running a quiet script passes, and so does one calling a quiet function of this text.
echo "case: r46 a library function whose body reaches a sleep through a file it names or a function it calls is refused at its call"
root=$(fixture r46-library-reaches)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
printf '#!/usr/bin/env bash\nsleep 300\n' > "$root/.github/scripts/r46-nap.sh"
printf '#!/usr/bin/env bash\necho "nothing to wait for"\n' > "$root/.github/scripts/r46-quiet.sh"
printf '#!/usr/bin/env bash\nb_nap() { sleep 300; }\n' > "$root/.github/scripts/r46-lib-b.sh"
cat > "$root/.github/scripts/r46-lib.sh" <<'SH'
#!/usr/bin/env bash
lib_go() { .github/scripts/r46-nap.sh; }
lib_calls() { text_nap; }
lib_hands() { b_nap; }
lib_quiet() { .github/scripts/r46-quiet.sh; }
lib_calls_quiet() { text_hush; }
SH
chmod +x "$root/.github/scripts/r44-fetch.sh" "$root/.github/scripts/r46-nap.sh" \
  "$root/.github/scripts/r46-quiet.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call a library function that runs a sleeping script in the loop
        timeout-minutes: 60
        run: |
          . .github/scripts/r46-lib.sh
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            lib_go
          done
      - name: Call a library function that calls a sleeping function of this text in the loop
        timeout-minutes: 60
        run: |
          text_nap() { sleep 300; }
          . .github/scripts/r46-lib.sh
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            lib_calls
          done
      - name: Call a library function that calls a sleeping function of another library in the loop
        timeout-minutes: 60
        run: |
          . .github/scripts/r46-lib.sh
          . .github/scripts/r46-lib-b.sh
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            lib_hands
          done
      - name: Call a library function that runs a quiet script in the loop
        timeout-minutes: 60
        run: |
          . .github/scripts/r46-lib.sh
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            lib_quiet
          done
      - name: Call a library function that calls a quiet function of this text in the loop
        timeout-minutes: 60
        run: |
          text_hush() { echo hush; }
          . .github/scripts/r46-lib.sh
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            lib_calls_quiet
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call of a library function that runs a sleeping script, with the sleep in the loop as its cure" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'Call a library function that runs a sleeping script in the loop' of job 'build' calls .lib_go., which \.github/scripts/r46-lib\.sh defines, in the loop opened on line 11,.*; \.github/scripts/r46-nap\.sh writes .sleep 300. on its line 2, reached from the body of .lib_go., which that call runs\..* and take it out of \.github/scripts/r46-nap\.sh (dispatch://INFRA-1545, round 46)\.")"
check "refuses the call of a library function that calls a sleeping function of this text" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=22::step 'Call a library function that calls a sleeping function of this text in the loop' of job 'build' calls .lib_calls., which \.github/scripts/r46-lib\.sh defines, in the loop opened on line 20,.*; \.github/workflows/case\.yaml writes .sleep 300. on its line 18, in the body of .text_nap., which that call runs\.")"
check "refuses the call of a library function that calls a sleeping function of another library" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=31::step 'Call a library function that calls a sleeping function of another library in the loop' of job 'build' calls .lib_hands., which \.github/scripts/r46-lib\.sh defines, in the loop opened on line 29,.*; \.github/scripts/r46-lib-b\.sh writes .sleep 300. on its line 2, in the body of .b_nap., which that call runs\.")"
check "passes the library functions that run a quiet script and call a quiet function" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# A heredoc something in a pass of a priced loop may run, whose body sleeps or names a file whose
# run reaches a sleep, is refused by name (heredoc_sleep, SLEEP_HEREDOC_REFUSAL;
# dispatch://INFRA-1545, round 46): this check reads every heredoc's body as data, and until round
# 46 a `bash <<'EOF'` around `sleep 300` in each of three passes of a 300 s fetch was charged 0 s
# for it. Each step runs r44-fetch.sh, 300 s, in three passes under 60 minutes. A quiet body fed
# to a shell passes, and so does a sleeping body fed to `cat`, which prints it.
echo "case: r46 a heredoc a pass of a priced loop may run is read for the sleeps its body reaches"
root=$(fixture r46-pass-heredocs)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
printf '#!/usr/bin/env bash\nsleep 300\n' > "$root/.github/scripts/r46-nap.sh"
chmod +x "$root/.github/scripts/r44-fetch.sh" "$root/.github/scripts/r46-nap.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Feed a sleeping heredoc to bash in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            bash <<'EOF'
          sleep 300
          EOF
          done
      - name: Feed a heredoc naming a sleeping script to sh in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            sh <<'EOF'
          .github/scripts/r46-nap.sh
          EOF
          done
      - name: Feed a quiet heredoc to bash in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            bash <<'EOF'
          echo "nothing to wait for"
          EOF
          done
      - name: Feed a heredoc holding a sleep to cat in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            cat <<'EOF'
          sleep 300
          EOF
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the sleeping heredoc fed to bash, with the sleep in the loop as its cure" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Feed a sleeping heredoc to bash in the loop' of job 'build' feeds a heredoc to .bash. in a command that a pass of the loop opened on line 10 runs, and a fetch, or a call of a script that fetches, is in that loop; its body writes .sleep 300. on line 13\..* Write the wait in this loop's body as the bare word .sleep. and literal seconds, such as .sleep 300., which this check charges at every pass, and take the sleep out of the heredoc's body; or feed the heredoc outside the loop that holds the fetch (dispatch://INFRA-1545, round 46)\.")"
check "refuses the heredoc naming a sleeping script fed to sh" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=21::step 'Feed a heredoc naming a sleeping script to sh in the loop' of job 'build' feeds a heredoc to .sh. in a command that a pass of the loop opened on line 19 runs,.*; its body names \.github/scripts/r46-nap\.sh, and \.github/scripts/r46-nap\.sh writes .sleep 300. on its line 2,")"
check "passes the quiet heredoc fed to bash and the sleeping one fed to cat" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A trap whose action reaches a sleep runs it each time its signal fires, which this check counts
# nowhere, so where a pass of a priced loop can fire it, it is refused by name
# (dispatch://INFRA-1545, round 46): a `sleep` in the action of a trap set in a pass is read as
# the loop's own sleep in another form (sleep_form, SLEEP_WRAPPED), a file the action names there
# as a file named in a place this check does not read as a call (SLEEP_UNREAD_REFUSAL), and a
# call there of a function whose body sleeps, and an action set before the loop on a signal other
# than EXIT that does any of the three, by the text or by a library it sources before the loop,
# by trap_sleep (SLEEP_TRAP_REFUSAL). Until round 46 each was charged 0 s. Each step runs
# r44-fetch.sh, 300 s, in three passes under 60 minutes. An EXIT trap set before the loop, whose
# action runs once as the shell ends, passes, by the text or by a library it sources, and so do a
# DEBUG trap that echoes and a trap in the loop calling a quiet function.
echo "case: r46 a trap whose action reaches a sleep, set in a pass of a priced loop or before it, is refused by name"
root=$(fixture r46-traps)
cp "$work/r44-T13-trailing-sleep/.github/scripts/r44-fetch.sh" "$root/.github/scripts/"
printf '#!/usr/bin/env bash\nsleep 300\n' > "$root/.github/scripts/r46-nap.sh"
printf "#!/usr/bin/env bash\ntrap 'sleep 300' DEBUG\n" > "$root/.github/scripts/r46-trap-lib.sh"
printf "#!/usr/bin/env bash\ntrap 'sleep 300' EXIT\n" > "$root/.github/scripts/r46-exit-lib.sh"
chmod +x "$root/.github/scripts/r44-fetch.sh" "$root/.github/scripts/r46-nap.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Set a trap whose action sleeps in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            trap 'sleep 300' RETURN
          done
      - name: Set a DEBUG trap that sleeps before the loop
        timeout-minutes: 60
        run: |
          trap 'sleep 300' DEBUG
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
          done
      - name: Set a trap calling a sleeping function in the loop
        timeout-minutes: 60
        run: |
          nap() { sleep 300; }
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            trap nap DEBUG
          done
      - name: Set an ERR trap running a sleeping script before the loop
        timeout-minutes: 60
        run: |
          trap '.github/scripts/r46-nap.sh' ERR
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
          done
      - name: Set a trap running a sleeping script in the loop
        timeout-minutes: 60
        run: |
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            trap '.github/scripts/r46-nap.sh' RETURN
          done
      - name: Set an EXIT trap that sleeps before the loop
        timeout-minutes: 60
        run: |
          trap 'sleep 300' EXIT
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
          done
      - name: Set a DEBUG trap that echoes before the loop
        timeout-minutes: 60
        run: |
          trap 'echo "a command runs"' DEBUG
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
          done
      - name: Set a trap calling a quiet function in the loop
        timeout-minutes: 60
        run: |
          hush() { echo hush; }
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
            trap hush DEBUG
          done
      - name: Source a library that sets a DEBUG trap that sleeps before the loop
        timeout-minutes: 60
        run: |
          . .github/scripts/r46-trap-lib.sh
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
          done
      - name: Source a library that sets an EXIT trap that sleeps before the loop
        timeout-minutes: 60
        run: |
          . .github/scripts/r46-exit-lib.sh
          for pass in 1 2 3; do
            .github/scripts/r44-fetch.sh
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the sleep in the action of a trap set in the loop as a sleep behind a word other than sleep" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Set a trap whose action sleeps in the loop' of job 'build' writes .trap 'sleep 300' RETURN. in the loop opened on line 10,.* Here the word that opens the command is .trap., not .sleep., so this check charges this sleep nothing\.")"
check "refuses the DEBUG trap that sleeps before the loop, with the sleep in the loop and an EXIT trap as its cures" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=17::step 'Set a DEBUG trap that sleeps before the loop' of job 'build' sets a trap on .DEBUG. before the loop opened on line 18; its action, .sleep 300., holds .sleep 300.; and a fetch, or a call of a script that fetches, is in that loop\..* Write the wait in that loop's body as the bare word .sleep. and literal seconds, such as .sleep 300., which this check charges at every pass, and take it out of the trap's action; or set the trap before that loop on EXIT alone, whose action runs once, as the shell ends (dispatch://INFRA-1545, round 46)\.")"
check "refuses the trap calling a sleeping function in the loop" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=27::step 'Set a trap calling a sleeping function in the loop' of job 'build' sets a trap on .DEBUG. in a command that a pass of the loop opened on line 25 runs; its action, .nap., calls .nap., and \.github/workflows/case\.yaml writes .sleep 300. on its line 24, in the body of .nap.; and a fetch")"
check "refuses the ERR trap running a sleeping script before the loop" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=32::step 'Set an ERR trap running a sleeping script before the loop' of job 'build' sets a trap on .ERR. before the loop opened on line 33; its action, .\.github/scripts/r46-nap\.sh., names \.github/scripts/r46-nap\.sh, and \.github/scripts/r46-nap\.sh writes .sleep 300. on its line 2; and a fetch")"
check "refuses the trap running a sleeping script in the loop as a file named in an argument of trap" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=41::step 'Set a trap running a sleeping script in the loop' of job 'build' names \.github/scripts/r46-nap\.sh in an argument of .trap., a place this check does not read as a call, in a command that a pass of the loop opened on line 39 runs,")"
check "refuses the DEBUG trap a library sourced before the loop sets, at its source" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=68::step 'Source a library that sets a DEBUG trap that sleeps before the loop' of job 'build' sets a trap on .DEBUG. on line 2 of \.github/scripts/r46-trap-lib\.sh, which this text sources on line 68, before the loop opened on line 69; its action, .sleep 300., holds .sleep 300.; and a fetch")"
check "passes the EXIT traps before the loop, the text's and a library's, the DEBUG trap that echoes and the trap calling a quiet function" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 6)"

# A trap's action is read for fetches as commands of the text on the trap's line
# (dispatch://INFRA-1545, round 46): one on EXIT alone set outside every loop and every
# function's body runs once, as the shell ends, and its fetch is charged once to the step, and a
# call there of a function of the step as one call of it; any other trap whose action reaches a
# fetch is refused by name (TRAP_FETCH_REFUSAL): one on another signal, which can fire any number
# of times, and one on EXIT set in a loop or in a function's body. Until round 46 each was charged
# 0 s. The fetch is one `retry_fetch` at 30 s, 5 x 30 + 150 = 300 s: under `timeout-minutes: 4`,
# 240 s, the EXIT trap's is refused at that figure, and under 6 minutes it passes; a function
# called once and again from an EXIT trap is charged twice, 600 s, which 9 minutes, 540 s, does not
# exceed. Traps that remove files on EXIT and on ERR beside a fetch fetch nothing and pass, as the
# tree's twelve do.
echo "case: r46 a trap's action is read for fetches, charged once on EXIT outside loops and functions and refused by name elsewhere"
root=$(fixture r46-trap-fetches)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch in an EXIT trap set once
        timeout-minutes: 4
        run: |
          . .github/scripts/ci-retry.sh
          trap 'retry_fetch "an asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a' EXIT
      - name: Fetch in an EXIT trap set once under a key above it
        timeout-minutes: 6
        run: |
          . .github/scripts/ci-retry.sh
          trap 'retry_fetch "an asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a' EXIT
      - name: Fetch in an ERR trap
        timeout-minutes: 60
        run: |
          . .github/scripts/ci-retry.sh
          trap 'retry_fetch "an asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a' ERR
      - name: Fetch in an EXIT trap set in a loop
        timeout-minutes: 60
        run: |
          . .github/scripts/ci-retry.sh
          for pass in 1 2 3; do
            trap 'retry_fetch "an asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a' EXIT
          done
      - name: Fetch in an EXIT trap set in a function
        timeout-minutes: 60
        run: |
          . .github/scripts/ci-retry.sh
          arm() { trap 'retry_fetch "an asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a' EXIT; }
          arm
      - name: Call a fetching function from an EXIT trap and once more
        timeout-minutes: 9
        run: |
          . .github/scripts/ci-retry.sh
          get() { retry_fetch "an asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a; }
          get
          trap get EXIT
      - name: Call a fetching function from an ERR trap
        timeout-minutes: 60
        run: |
          . .github/scripts/ci-retry.sh
          get() { retry_fetch "an asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a; }
          trap get ERR
      - name: Clean up in EXIT and ERR traps beside a fetch
        timeout-minutes: 6
        run: |
          work=$(mktemp -d)
          trap 'rm -rf "$work"' EXIT
          trap 'rm -f "$work/partial"' ERR
          . .github/scripts/ci-retry.sh
          retry_fetch "an asset" 30 -- curl -fsSL --max-time 30 -o "$work/a" https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "charges the fetch of an EXIT trap set once 300 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Fetch in an EXIT trap set once' carries .timeout-minutes: 4., 240 s, and its own retries take 300 s to exhaust")"
check "refuses the fetch of an ERR trap by name, with the EXIT trap and the command of its own as its cures" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=21::step 'Fetch in an ERR trap' of job 'build' sets a trap on .ERR. whose action, .retry_fetch \"an asset\" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a., holds a fetch or calls a function that fetches\..* this one is set on a signal other than EXIT, which can fire any number of times, and no figure here counts them\. Set the trap that way, or write the fetch as a command of its own where it is to run, which this check charges each time it runs (dispatch://INFRA-1545, round 46)\.")"
check "refuses the fetch of an EXIT trap set in a loop by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=27::step 'Fetch in an EXIT trap set in a loop' of job 'build' sets a trap on .EXIT. in the loop opened on line 26 whose action,.* this one is set in a loop, where a fetch its action writes is charged at each pass")"
check "refuses the fetch of an EXIT trap set in a function by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=33::step 'Fetch in an EXIT trap set in a function' of job 'build' sets a trap on .EXIT. in the body of .arm. whose action,.* this one is set in the body of .arm., where a fetch its action writes is charged at each call of .arm.")"
check "charges a function called once and again from an EXIT trap twice, 600 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=36::step 'Call a fetching function from an EXIT trap and once more' carries .timeout-minutes: 9., 540 s, and its own retries take 600 s to exhaust")"
check "refuses an ERR trap calling a fetching function by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=47::step 'Call a fetching function from an ERR trap' of job 'build' sets a trap on .ERR. whose action, .get., holds a fetch or calls a function that fetches\.")"
check "passes the EXIT trap under a key above its fetch and the cleanup traps beside a fetch" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 6)"

# Two paths reaching one file with the same product of loop factors are both charged, each run
# with the waits of the loops around it (dispatch://INFRA-1545, round 37; until then the file
# was charged once, through the path whose loops waited longer). The step runs pa.sh and pz.sh
# ten times and pm.sh twenty times, in loops that do not sleep. pa.sh runs pc1.sh twice, each
# pass ending with `sleep 9`, 2 x 300 + 2 x 9 = 618 s, and pm.sh runs it once; pz.sh runs pz2.sh
# twice the same way and pz2.sh runs pc2.sh once, 618 s again, while pm.sh reaches pc2.sh through
# pm2.sh. Each fetch retries at 30 s (300 s), so pm.sh is 600 s and the step waits
# 10 x (618 + 618) + 20 x 600 = 24360 s, which `timeout-minutes: 203`, 12180 s, does not exceed;
# 24360 s is 406 minutes, so the key that holds it is 407. Round 36's reading charged each file
# once, 2 x 20 x 300 + 2 x 90 = 12180 s, half of the runs this step makes, and each `sleep 9`
# once fewer times than its loop's passes; every reading did that until round 44, and the one
# before round 44 charged this step 24180 s (dispatch://INFRA-1545, round 44).
echo "case: of two paths of one product, every run along both is charged"
root=$(fixture path-waits-equal-product)
cat > "$root/.github/scripts/pc1.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
SH
cp "$root/.github/scripts/pc1.sh" "$root/.github/scripts/pc2.sh"
cat > "$root/.github/scripts/pa.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
for pass in 1 2; do
  .github/scripts/pc1.sh
  sleep 9
done
SH
cat > "$root/.github/scripts/pz.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
for pass in 1 2; do
  .github/scripts/pz2.sh
  sleep 9
done
SH
cat > "$root/.github/scripts/pz2.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/pc2.sh
SH
cat > "$root/.github/scripts/pm.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/pc1.sh
.github/scripts/pm2.sh
SH
cat > "$root/.github/scripts/pm2.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/pc2.sh
SH
chmod +x "$root/.github/scripts/pc1.sh" "$root/.github/scripts/pc2.sh" \
  "$root/.github/scripts/pa.sh" "$root/.github/scripts/pz.sh" "$root/.github/scripts/pz2.sh" \
  "$root/.github/scripts/pm.sh" "$root/.github/scripts/pm2.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch along paths of one product
        timeout-minutes: 203
        run: |
          for pass in 1 2 3 4 5 6 7 8 9 10; do
            .github/scripts/pa.sh
            .github/scripts/pz.sh
          done
          for pass in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
            .github/scripts/pm.sh
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges every run of each file along both paths, with their waits" \
  "$(contains "$out" 'its own retries take 24360 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 407')"

# Two paths of the same product and the same waits are both charged, and each loop's waits once
# for its own runs (dispatch://INFRA-1545, round 37; until then one of the two was charged, the
# first by name). The step runs t-a.sh twice, each pass ending with `sleep 100`, then t-b.sh
# twice the same way. Both run t-c.sh, and t-b.sh runs t-x.sh too, each retrying at 30 s
# (300 s). So the first loop waits 2 x 300 + 2 x 100 = 800 s and the second
# 2 x (300 + 300) + 2 x 100 = 1400 s, 2200 s, which `timeout-minutes: 23`, 1380 s, does not
# exceed; 2200 s is 36 minutes and 40 s, so the key that holds it is 37. Round 36's reading
# charged t-c.sh once, through t-a.sh, 4 x 300 + 200 = 1400 s, each loop's sleep once fewer
# times than its passes, as the readings until round 44 did, which charged this step 2000 s
# (dispatch://INFRA-1545, round 44).
echo "case: of two paths of one product and one wait, each path's runs are charged"
root=$(fixture path-tie-scripts)
cat > "$root/.github/scripts/t-c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
SH
cp "$root/.github/scripts/t-c.sh" "$root/.github/scripts/t-x.sh"
cat > "$root/.github/scripts/t-a.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/t-c.sh
SH
cat > "$root/.github/scripts/t-b.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/t-c.sh
.github/scripts/t-x.sh
SH
chmod +x "$root/.github/scripts/t-a.sh" "$root/.github/scripts/t-b.sh" \
  "$root/.github/scripts/t-c.sh" "$root/.github/scripts/t-x.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch along two equal paths
        timeout-minutes: 23
        run: |
          for pass in 1 2; do
            .github/scripts/t-a.sh
            sleep 100
          done
          for pass in 1 2; do
            .github/scripts/t-b.sh
            sleep 100
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges t-c.sh for its runs along both paths" \
  "$(contains "$out" 'its own retries take 2200 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 37')"

# The same through a composite action, whose steps are shells of their own and whose `uses:`
# is a run of the action it names (dispatch://INFRA-1545, round 37; until then one of the two
# paths was charged, the first by name). The step uses v-top, which runs v-s.sh twice, each pass
# ending with `sleep 100`, and then uses v-a. v-s.sh runs v-t.sh and v-x.sh once; v-a runs v-t.sh
# twice the same way. So v-top's first step waits 2 x (300 + 300) + 2 x 100 = 1400 s and v-a's
# one step 2 x 300 + 2 x 100 = 800 s, 2200 s, which `timeout-minutes: 23` does not exceed, and
# whose key is 37. Round 36's reading charged v-t.sh once, through v-a, 4 x 300 + 200 = 1400 s,
# each loop's sleep once fewer times than its passes, as the readings until round 44 did, which
# charged this step 2000 s (dispatch://INFRA-1545, round 44).
echo "case: of two equal paths through a composite action, each path's runs are charged"
root=$(fixture path-tie-action)
cat > "$root/.github/scripts/v-t.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
SH
cp "$root/.github/scripts/v-t.sh" "$root/.github/scripts/v-x.sh"
cat > "$root/.github/scripts/v-s.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/v-t.sh
.github/scripts/v-x.sh
SH
chmod +x "$root/.github/scripts/v-s.sh" "$root/.github/scripts/v-t.sh" \
  "$root/.github/scripts/v-x.sh"
mkdir -p "$root/.github/actions/v-top" "$root/.github/actions/v-a"
cat > "$root/.github/actions/v-top/action.yml" <<'YAML'
name: V top
description: Runs v-s.sh in a loop, then uses v-a.
runs:
  using: composite
  steps:
    - name: Run v-s.sh twice
      shell: bash
      run: |
        for pass in 1 2; do
          .github/scripts/v-s.sh
          sleep 100
        done
    - uses: ./.github/actions/v-a
YAML
cat > "$root/.github/actions/v-a/action.yml" <<'YAML'
name: V a
description: Runs v-t.sh in a loop.
runs:
  using: composite
  steps:
    - name: Run v-t.sh twice
      shell: bash
      run: |
        for pass in 1 2; do
          .github/scripts/v-t.sh
          sleep 100
        done
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch along two equal paths
        uses: ./.github/actions/v-top
        timeout-minutes: 23
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges v-t.sh for its runs along both paths" \
  "$(contains "$out" 'its own retries take 2200 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 37')"

# And a path's waits are its own: each loop's waits are charged with that loop's runs and with
# no other's (dispatch://INFRA-1545, round 37; until then one of the two paths was charged, the
# one waiting longer). w-top runs w-s.sh, which runs w-t.sh twice, each pass ending with
# `sleep 100`, and uses w-a, which runs w-t.sh twice with no sleep. The step waits
# 2 x 300 + 2 x 100 = 800 s through w-s.sh and 2 x 300 = 600 s through w-a, 1400 s, which
# `timeout-minutes: 11`, 660 s, does not exceed, and whose key is 24. Round 36's reading charged
# w-t.sh once, through w-s.sh, 700 s, its sleep once fewer times than its loop's passes, as the
# readings until round 44 did, which charged this step 1300 s (dispatch://INFRA-1545, round 44).
echo "case: a path waiting less and a path waiting more are each charged their own waits"
root=$(fixture path-name-not-wait)
cat > "$root/.github/scripts/w-t.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
SH
cat > "$root/.github/scripts/w-s.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
for pass in 1 2; do
  .github/scripts/w-t.sh
  sleep 100
done
SH
chmod +x "$root/.github/scripts/w-s.sh" "$root/.github/scripts/w-t.sh"
mkdir -p "$root/.github/actions/w-top" "$root/.github/actions/w-a"
cat > "$root/.github/actions/w-top/action.yml" <<'YAML'
name: W top
description: Runs w-s.sh, then uses w-a.
runs:
  using: composite
  steps:
    - name: Run w-s.sh
      shell: bash
      run: .github/scripts/w-s.sh
    - uses: ./.github/actions/w-a
YAML
cat > "$root/.github/actions/w-a/action.yml" <<'YAML'
name: W a
description: Runs w-t.sh in a loop that does not sleep.
runs:
  using: composite
  steps:
    - name: Run w-t.sh twice
      shell: bash
      run: |
        for pass in 1 2; do
          .github/scripts/w-t.sh
        done
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch along a waiting path and a named one
        uses: ./.github/actions/w-top
        timeout-minutes: 11
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges w-t.sh's runs along both paths, with w-s.sh's waits alone" \
  "$(contains "$out" 'its own retries take 1400 s to exhaust')"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 24')"

# The waits charged are those of the paths charging a file whose own arithmetic is above 0. A
# loop sleeping 100 s after each of its three passes around q-quiet.sh, which retries nothing,
# sits beside q-fetch.sh's 1050 s, and the step's figure is 1050 s, which `timeout-minutes: 18`,
# 1080 s, exceeds. A walk charging the waits of q-quiet.sh's path as well refused it at 1250 s
# (dispatch://INFRA-1545, round 36), its sleep charged once fewer times than the passes, and
# would refuse it at 1350 s since round 44.
echo "case: the waits of a loop around a script that retries nothing are not charged"
root=$(fixture path-waits-costless)
cat > "$root/.github/scripts/q-fetch.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/scripts/q-quiet.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "nothing to fetch"
SH
chmod +x "$root/.github/scripts/q-fetch.sh" "$root/.github/scripts/q-quiet.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Wait around nothing, then fetch
        timeout-minutes: 18
        run: |
          for pass in 1 2 3; do
            .github/scripts/q-quiet.sh
            sleep 100
          done
          .github/scripts/q-fetch.sh
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# A call of a priced script inside a loop whose passes this check cannot read leaves the walk
# with no figure, and the walk still reaches every file beside that call. The step loops,
# uncounted, over r-a.sh; r-a.sh loops, uncounted, over r-b.sh, which retries at 30 s, and then
# runs r-c.sh, which loops, uncounted, over r-b.sh too. One run of r-a.sh has no figure, and the
# refusal says so where a walk going on past the unread call printed `takes 0 s`, the figure of
# what is left; and it names r-c.sh's loop, which a walk stopping at the first unread call never
# reached (dispatch://INFRA-1545, round 36).
echo "case: a script reaching a priced script through an unread loop has no figure, and its other calls are read"
root=$(fixture path-unread)
cat > "$root/.github/scripts/r-b.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a small asset" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
SH
cat > "$root/.github/scripts/r-a.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
while read -r id; do
  .github/scripts/r-b.sh "$id"
done <<<"$ids"
.github/scripts/r-c.sh
SH
cat > "$root/.github/scripts/r-c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
while read -r id; do
  .github/scripts/r-b.sh "$id"
done <<<"$more"
SH
chmod +x "$root/.github/scripts/r-a.sh" "$root/.github/scripts/r-b.sh" \
  "$root/.github/scripts/r-c.sh"
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
            .github/scripts/r-a.sh "$id"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "is refused once" \
  "$(is "$(printf '%s\n' "$out" | grep -c 'this check can put no figure on it')" 1)"
check "says one run of r-a.sh reaches an uncounted loop" \
  "$(contains "$out" 'One run of ..github/scripts/r-a.sh. reaches a loop, its own or one in a script it calls')"
check "prints no figure for a cost it cannot compute" \
  "$(is "$(contains "$out" 'takes 0 s')" false)"
check "names r-a.sh's own loop" \
  "$(contains "$out" '..github/scripts/r-b.sh. at .github/scripts/r-a.sh:4, inside the loop at line 3')"
check "names r-c.sh's loop, beside the unread call" \
  "$(contains "$out" '..github/scripts/r-b.sh. at .github/scripts/r-c.sh:4, inside the loop at line 3')"

# A job writing its own `timeout-minutes` around a step with no figure: the step is refused for
# its uncounted loop, and the job's slowest retried step is read from the steps that have a
# figure, of which there is none here. Comparing that step's None with a figure ended the check
# in a TypeError (dispatch://INFRA-1545, round 36).
echo "case: a job whose only retried step has no figure is refused for that step alone"
root=$(fixture job-key-unread-step)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 40
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
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "is refused once, for the loop it cannot count" \
  "$(is "$(printf '%s\n' "$out" | grep -c 'this check can put no figure on it')" 1)"
check "names the loop's file and line" \
  "$(contains "$out" '.gh api. at .github/workflows/case.yaml:13, inside the loop at line 12')"

# The tester's input of round 37 (test handoff v51 of dispatch://INFRA-1545, round 37). zz-c.sh
# retries at 180 s, 5 x 180 + 150 = 1050 s; zz-a.sh and zz-b.sh each run it once, and the step
# runs zz-a.sh and then zz-b.sh, so it runs zz-c.sh twice and waits 2 x 1050 = 2100 s. Under
# `timeout-minutes: 30`, 1800 s, the step is refused for those 2100 s; under 36, 2160 s, it
# passes. A walk charging zz-c.sh once, at its most expensive path, read 1050 s and passed both.
echo "case: a step running two scripts that each run one retried script waits for both runs"
root=$(fixture runs-twice-under-30)
cat > "$root/.github/scripts/zz-c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/scripts/zz-a.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/zz-c.sh
SH
cp "$root/.github/scripts/zz-a.sh" "$root/.github/scripts/zz-b.sh"
chmod +x "$root/.github/scripts/zz-a.sh" "$root/.github/scripts/zz-b.sh" \
  "$root/.github/scripts/zz-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through both scripts
        timeout-minutes: 30
        run: |
          .github/scripts/zz-a.sh
          .github/scripts/zz-b.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges zz-c.sh once for each of its two runs" \
  "$(contains "$out" "step 'Fetch through both scripts' carries .timeout-minutes: 30., 1800 s, and its own retries take 2100 s to exhaust")"
check "names the budget that would hold them" "$(contains "$out" 'timeout-minutes: 36. or more')"

echo "case: the same step under timeout-minutes: 36 passes"
root=$(fixture runs-twice-under-36)
cp "$work/runs-twice-under-30/.github/scripts/zz-a.sh" \
  "$work/runs-twice-under-30/.github/scripts/zz-b.sh" \
  "$work/runs-twice-under-30/.github/scripts/zz-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/zz-a.sh" "$root/.github/scripts/zz-b.sh" \
  "$root/.github/scripts/zz-c.sh"
sed 's/timeout-minutes: 30/timeout-minutes: 36/' "$work/runs-twice-under-30/.github/workflows/case.yaml" \
  > "$root/.github/workflows/case.yaml"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "refuses nothing" "$(is "$(contains "$out" '::error')" false)"

# Each spelling a run is written in is a run, charged once for every time the step makes it
# (dispatch://INFRA-1545, round 37): the path after `bash`, after `sh` and an option of its own,
# with a `./` in front of it, as the whole command of a `$(…)`, and behind `timeout 600`. zz-c.sh
# retries at 180 s, 1050 s a run, so the step running it through both shells reads 2100 s and
# each other step 1050 s. Every key is one minute, so each step is refused with its own figure.
# A run is the path as the command word, after nothing but assignments, which is every run this
# tree writes; a path after a shell's name or behind `timeout` is refused by name where its script
# fetches, and charged nothing (dispatch://INFRA-1545, round 37).
echo "case: a run is charged in each spelling of its path, and refused behind a shell or a wrapper"
root=$(fixture run-spellings)
cp "$work/runs-twice-under-30/.github/scripts/zz-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/zz-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run through two shells
        timeout-minutes: 1
        run: |
          bash .github/scripts/zz-c.sh
          sh -e .github/scripts/zz-c.sh
      - name: Run by its dotted path
        timeout-minutes: 1
        run: ./.github/scripts/zz-c.sh
      - name: Run inside a command substitution
        timeout-minutes: 1
        run: out=$(.github/scripts/zz-c.sh)
      - name: Run behind timeout
        timeout-minutes: 1
        run: timeout 600 .github/scripts/zz-c.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the run after bash by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Run through two shells' of job 'build' names .github/scripts/zz-c.sh in an argument of .bash., a place this check does not read as a call")"
check "refuses the run after sh -e by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Run through two shells' of job 'build' names .github/scripts/zz-c.sh in an argument of .sh., a place this check does not read as a call")"
check "charges a run by ./.github/scripts/zz-c.sh, 1050 s" \
  "$(contains "$out" "step 'Run by its dotted path' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
check "charges a run inside \$(…), 1050 s" \
  "$(contains "$out" "step 'Run inside a command substitution' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
check "refuses the run behind timeout 600 by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=20::step 'Run behind timeout' of job 'build' names .github/scripts/zz-c.sh in a command behind .timeout., a place this check does not read as a call")"
check "charges neither refused step" "$(is "$(contains "$out" "step 'Run through two shells' carries\\|step 'Run behind timeout' carries")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"

# A library is charged once for each shell that sources it, however often that shell reaches it
# (dispatch://INFRA-1545, round 37). zz-lib.sh's own site retries at 180 s, 1050 s; zz-user.sh
# sources it. The first step sources it and then runs zz-user.sh, which sources it again: one
# shell, 1050 s and not 2100 s. zz-one's one step does the same, 1050 s. zz-two sources it in
# each of its two steps, which are two shells, so the step using zz-two reads 2100 s.
echo "case: a library sourced twice in one shell is charged once, and once in each of two shells"
root=$(fixture library-sourced-twice)
cat > "$root/.github/scripts/zz-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/scripts/zz-user.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/zz-lib.sh
SH
chmod +x "$root/.github/scripts/zz-user.sh"
mkdir -p "$root/.github/actions/zz-one" "$root/.github/actions/zz-two"
cat > "$root/.github/actions/zz-one/action.yml" <<'YAML'
name: ZZ one
description: Sources zz-lib.sh and runs a script that sources it again, in one step.
runs:
  using: composite
  steps:
    - name: Source the library and run its user
      shell: bash
      run: |
        . .github/scripts/zz-lib.sh
        .github/scripts/zz-user.sh
YAML
cat > "$root/.github/actions/zz-two/action.yml" <<'YAML'
name: ZZ two
description: Sources zz-lib.sh in each of two steps.
runs:
  using: composite
  steps:
    - name: Source the library
      shell: bash
      run: . .github/scripts/zz-lib.sh
    - name: Source the library again
      shell: bash
      run: . .github/scripts/zz-lib.sh
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source the library and run its user
        timeout-minutes: 1
        run: |
          . .github/scripts/zz-lib.sh
          .github/scripts/zz-user.sh
      - name: Use the action sourcing it in one step
        uses: ./.github/actions/zz-one
        timeout-minutes: 1
      - name: Use the action sourcing it in two steps
        uses: ./.github/actions/zz-two
        timeout-minutes: 1
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges the library once in a step that sources it and runs a script sourcing it" \
  "$(contains "$out" "step 'Source the library and run its user' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
check "charges it once in an action step doing the same" \
  "$(contains "$out" "step 'Use the action sourcing it in one step' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
check "charges it once in each of an action's two steps" \
  "$(contains "$out" "step 'Use the action sourcing it in two steps' carries .timeout-minutes: 1., 60 s, and its own retries take 2100 s to exhaust")"

# A path a step only prints, searches for or writes in a comment is no call, and is charged
# nothing and refused nothing (dispatch://INFRA-1545, round 37). These are the shapes round 37's
# tester found in this tree, written over zz-c.sh, whose 1050 s would refuse a one-minute key:
# browser-image.yaml's summary line naming a script inside `{ … } >> "$GITHUB_STEP_SUMMARY"`,
# ci.yaml tool-fallback's `::error` line, and a `grep` of a script's name in ci.yaml, with a
# `printf`, a comment line and a comment after a command beside them.
echo "case: a script's path in echo, grep or a comment is no call"
root=$(fixture non-calls)
cp "$work/runs-twice-under-30/.github/scripts/zz-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/zz-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Print and search the script's name
        timeout-minutes: 1
        run: |
          {
            echo "Pin a row of .github/ci-images.yaml; .github/scripts/zz-c.sh refuses anything else."
          } >> "$GITHUB_STEP_SUMMARY"
          echo "::error::.github/scripts/zz-c.sh answered nothing, so this step has no bound" >&2
          grep -q .github/scripts/zz-c.sh notes.txt || true
          # .github/scripts/zz-c.sh fetches the asset; this step only names it.
          true # and so does .github/scripts/zz-c.sh
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "neither charges nor refuses the step" "$(is "$(contains "$out" 'case.yaml')" false)"
check "names zz-c.sh nowhere" "$(is "$(contains "$out" 'zz-c.sh')" false)"

# Every other place a path is written in is unread, and naming a script that fetches it is
# refused by name: the file and the line, the step, the script, the form and the rewrite
# (dispatch://INFRA-1545, round 37). One step for each form, each naming zz-c.sh, which retries
# at 180 s. The process substitution is written twice: a run inside one, and the tester's
# `bash <(cat …)`, where the path is an argument of `cat` inside it and is named that way. A run
# inside a substitution has its path as the command word already, so it is told to take the run
# out of the substitution instead, which is advice it can follow. An `echo` or a `printf`
# piped into a shell is a pipe into a shell like `cat`'s, though the same `echo` with no pipe is
# a non-call (the case above): it was read as a non-call, and the script the shell runs was
# neither charged nor refused.
echo "case: each place rule 8 does not read is refused by name where the script fetches"
root=$(fixture unread-forms)
cp "$work/runs-twice-under-30/.github/scripts/zz-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/zz-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run it through a variable
        timeout-minutes: 1
        run: |
          p=.github/scripts/zz-c.sh; "$p"
      - name: List it in an array
        timeout-minutes: 1
        run: |
          a=( .github/scripts/zz-c.sh )
          "${a[@]}"
      - name: Run it through eval
        timeout-minutes: 1
        run: |
          eval .github/scripts/zz-c.sh
      - name: Run it in bash -c
        timeout-minutes: 1
        run: |
          bash -c '.github/scripts/zz-c.sh'
      - name: Pipe it into a shell
        timeout-minutes: 1
        run: |
          cat .github/scripts/zz-c.sh | bash
      - name: Run it in a process substitution
        timeout-minutes: 1
        run: |
          diff <(.github/scripts/zz-c.sh) expected.txt
      - name: Feed it to bash through a process substitution
        timeout-minutes: 1
        run: |
          bash <(cat .github/scripts/zz-c.sh)
      - name: Run it in a function
        timeout-minutes: 1
        run: |
          f() { .github/scripts/zz-c.sh; }; f
      - name: Name it to a helper
        timeout-minutes: 1
        run: |
          die() { echo "$*" >&2; exit 1; }
          die "see .github/scripts/zz-c.sh"
      - name: Echo it into a shell
        timeout-minutes: 1
        run: |
          echo .github/scripts/zz-c.sh | bash
      - name: Print it into a shell
        timeout-minutes: 1
        run: |
          printf '%s\n' .github/scripts/zz-c.sh | sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
command_word='Write the path as the command word'
substitution='Take the run out of the substitution: run the script as a command of its own, with its output written to a file, and name that file where the substitution stood'
for refusal in \
  "10	Run it through a variable	a variable assignment	$command_word" \
  "14	List it in an array	an array	$command_word" \
  "19	Run it through eval	an argument of .eval.	$command_word" \
  "23	Run it in bash -c	an argument of .bash.	$command_word" \
  "27	Pipe it into a shell	a pipe into a shell	$command_word" \
  "31	Run it in a process substitution	a process substitution	$substitution" \
  "35	Feed it to bash through a process substitution	an argument of .cat.	$command_word" \
  "39	Run it in a function	a function body	Call the script outside the function" \
  "44	Name it to a helper	an argument of .die.	$command_word" \
  "48	Echo it into a shell	a pipe into a shell	$command_word" \
  "52	Print it into a shell	a pipe into a shell	$command_word"; do
  IFS=$'\t' read -r line step form cure <<<"$refusal"
  check "refuses '$step' by its file and line, the step, the script and the form" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/zz-c.sh in $form, a place this check does not read as a call, and that script fetches")"
  check "gives '$step' its rewrite" \
    "$(contains "$out" "step '$step' of job 'build' names .github/scripts/zz-c.sh in $form, .*$cure")"
done
check "does not tell a run inside a substitution to write its path as the command word" \
  "$(is "$(contains "$out" "step 'Run it in a process substitution' of job 'build' .*$command_word")" false)"
# The array's run is refused too, at its own line: outside any function `"${a[@]}"` runs words
# the text sets itself, which this check follows to no program (E5's refusal), and writing the
# path as the command word cures both (dispatch://INFRA-1545, round 37).
check "refuses the array's run at its own line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=15::step 'List it in an array' of job 'build' runs \`\${a\[@\]}\` as a command, which runs words its callers hand it or that it sets itself")"
check "refuses nothing else" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 12)"

# The same places naming a script that fetches nothing are neither charged nor refused
# (dispatch://INFRA-1545, round 37): zz-quiet.sh holds no retried call, so whatever runs it
# waits for no attempt, and there is no figure to ask for. The run of `"$p"` is another matter:
# its target is built from data, so this check cannot know which script it runs or whether that
# one fetches, and it fails closed, refusing the run by name (round 37's one refusal of a run it
# cannot name). Until round 37 this step passed.
echo "case: a place rule 8 does not read, naming a script that fetches nothing, is not refused, and a run through a variable is refused by name"
root=$(fixture unread-quiet)
cat > "$root/.github/scripts/zz-quiet.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "nothing to fetch"
SH
chmod +x "$root/.github/scripts/zz-quiet.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run it through a variable
        timeout-minutes: 1
        run: |
          p=.github/scripts/zz-quiet.sh; "$p"
      - name: Run it in a function
        timeout-minutes: 1
        run: |
          f() { .github/scripts/zz-quiet.sh; }; f
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the run through a variable by name, since its target is unnamed" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Run it through a variable' of job 'build' runs .\$p. as a command, whose last path element is an expansion, so this check cannot name the script that runs there")"
check "names zz-quiet.sh nowhere" "$(is "$(contains "$out" 'zz-quiet.sh')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A script is read as a step is: a place it does not read, naming a script that fetches, is
# refused at the script's own file and line, and the script is named where a step would be
# (dispatch://INFRA-1545, round 37). zz-wrap.sh runs zz-c.sh, 1050 s, from a function body and
# through a variable, and the step running zz-wrap.sh is charged neither.
echo "case: a place rule 8 does not read, inside a script the step runs, is refused at that script's line"
root=$(fixture unread-in-script)
cp "$work/runs-twice-under-30/.github/scripts/zz-c.sh" "$root/.github/scripts/"
cat > "$root/.github/scripts/zz-wrap.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
fetch() {
  .github/scripts/zz-c.sh
}
fetch
p=.github/scripts/zz-c.sh
"$p"
SH
chmod +x "$root/.github/scripts/zz-c.sh" "$root/.github/scripts/zz-wrap.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the wrapper
        timeout-minutes: 1
        run: .github/scripts/zz-wrap.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the function body at zz-wrap.sh:4" \
  "$(contains "$out" "::error file=.github/scripts/zz-wrap.sh,line=4::.github/scripts/zz-wrap.sh names .github/scripts/zz-c.sh in a function body, a place this check does not read as a call, and that script fetches")"
check "refuses the variable at zz-wrap.sh:7" \
  "$(contains "$out" "::error file=.github/scripts/zz-wrap.sh,line=7::.github/scripts/zz-wrap.sh names .github/scripts/zz-c.sh in a variable assignment, a place this check does not read as a call, and that script fetches")"
check "charges the step nothing for them" "$(is "$(contains "$out" 'case.yaml')" false)"

# release.yaml's shape after round 37's rewrite (dispatch://INFRA-1545, round 37): a function
# computes a value, and each run is written where that value is used, as the whole command of a
# `$(…)` whose argument is the function's own `$(…)`. Two runs of zz-c.sh, 2 x 1050 = 2100 s,
# and neither is read as a function body.
echo "case: a run written at the call site of a function computing its argument is charged"
root=$(fixture release-shape)
cp "$work/runs-twice-under-30/.github/scripts/zz-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/zz-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Resolve both versions
        timeout-minutes: 1
        run: |
          released_of() {
            if [ "$(sed -n 's/^skip=//p' <<<"$1")" != true ]; then
              sed -n 's/^version=//p' <<<"$1"
            fi
          }
          envoy_version=$(.github/scripts/zz-c.sh pair-version pi-envoy-v "$(released_of "$envoy")")
          legion_version=$(.github/scripts/zz-c.sh pair-version pi-legion-v "$(released_of "$legion")")
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges both runs, 2100 s" \
  "$(contains "$out" "step 'Resolve both versions' carries .timeout-minutes: 1., 60 s, and its own retries take 2100 s to exhaust")"
check "refuses nothing else, so neither run is read as a place it does not read" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# The shapes the call scanner reads a script's path in, each a step whose verdict is that reading
# (dispatch://INFRA-1545, round 37). Round 37's mutation pass of rule 8 left the scanner's handling
# of backticks, subshells, quotes, escapes, comments and redirects standing, because no case wrote
# a path in those shapes. sa-c.sh retries at 180 s, 5 x 180 + 150 = 1050 s, and every step's key is
# one minute, so a step the scanner reads a run in is refused with 1050 s. These steps are runs:
# the whole command of a backtick substitution, one inside an array's parentheses, a run beside a
# backtick substitution that holds a case pattern's `)`, a run beside a `$(…)` holding a subshell, a
# run inside a subshell, a run on the line after a process substitution, after a comment that ends
# the command before it, and after a prefix-removing `#` expansion on its line, a run through
# `\bash`, a run after an escaped quote, a run through a quoted `"ba\<newline>sh"` (the shell joins
# the two lines inside the quotes, so the command is `bash`), and a run whose output is redirected
# before its path. A reading that loses the backtick, the subshell, the comment's newline or the
# escape reads the path as an argument, a variable or nothing and refuses it as unread, or crashes.
echo "case: a run written beside substitutions, quotes and escapes is charged, and one inside backticks, a subshell or after bash is refused"
root=$(fixture sa-scanner-runs)
cat > "$root/.github/scripts/sa-c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/sa-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run inside backticks
        timeout-minutes: 1
        run: |
          out=`.github/scripts/sa-c.sh`
      - name: Run inside backticks inside an array
        timeout-minutes: 1
        run: |
          files=( `.github/scripts/sa-c.sh` )
      - name: Run beside backticks holding a case pattern
        timeout-minutes: 1
        run: |
          bash .github/scripts/sa-c.sh "`case $1 in a) echo a;; esac`"
      - name: Run beside a subshell inside a substitution
        timeout-minutes: 1
        run: |
          .github/scripts/sa-c.sh "$( (cd sub && pwd) )"
      - name: Run inside a subshell
        timeout-minutes: 1
        run: |
          ( cd "$GITHUB_WORKSPACE" && .github/scripts/sa-c.sh )
      - name: Run after a process substitution
        timeout-minutes: 1
        run: |
          sort <(printf 'b\na\n')
          .github/scripts/sa-c.sh
      - name: Run after a comment that ends a command
        timeout-minutes: 1
        run: |
          cd "$GITHUB_WORKSPACE" # the scripts are read from the root
          .github/scripts/sa-c.sh
      - name: Run after an expansion holding a hash
        timeout-minutes: 1
        run: |
          tag=${GITHUB_REF#refs/tags/}; .github/scripts/sa-c.sh "$tag"
      - name: Run through a backslashed bash
        timeout-minutes: 1
        run: |
          \bash .github/scripts/sa-c.sh
      - name: Run after an escaped quote
        timeout-minutes: 1
        run: |
          echo it\'s done; .github/scripts/sa-c.sh
      - name: Run through a quoted bash broken over two lines
        timeout-minutes: 1
        run: |
          "ba\
          sh" .github/scripts/sa-c.sh
      - name: Run with its output redirected before its path
        timeout-minutes: 1
        run: |
          bash >run.log .github/scripts/sa-c.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for step in \
  "Run beside a subshell inside a substitution" \
  "Run after a process substitution" \
  "Run after a comment that ends a command" \
  "Run after an expansion holding a hash" \
  "Run after an escaped quote"; do
  check "charges '$step' one run of sa-c.sh, 1050 s" \
    "$(contains "$out" "step '$step' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
done
# Inside backticks or a subshell, or after a shell's name, is no place this tree writes a run,
# so each of these is refused by name and charged nothing (dispatch://INFRA-1545, round 37).
for refusal in "10	Run inside backticks	a backtick substitution" \
  "14	Run inside backticks inside an array	a backtick substitution" \
  "18	Run beside backticks holding a case pattern	an argument of .bash." \
  "26	Run inside a subshell	a .( … ). subshell" \
  "44	Run through a backslashed bash	an argument of .bash." \
  "53	Run through a quoted bash broken over two lines	an argument of .bash." \
  "57	Run with its output redirected before its path	an argument of .bash."; do
  IFS=$'\t' read -r line step form <<<"$refusal"
  check "refuses '$step' by name as $form" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/sa-c.sh in $form, a place this check does not read as a call")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 12)"

# The same scanner's other readings (dispatch://INFRA-1545, round 37). Each step below writes
# sa-c.sh's path in a place rule 8 does not read, and is refused there by its file, its line and
# the form, which is what the scanner read: `bash < path` is a redirect; an `echo` piped with its
# errors, `2>&1 |`, into bash is a pipe into a shell (the `&` of `2>&1` belongs to the redirect and
# ends no command); a quoted command name holding a backslash keeps it (`"ba\sh"` is not bash, nor
# is `"bas\xh"`); a quoted argument whose path is followed by another character before the quote
# closes on the next line is more than the path; `"$(command -v bash)"` is a command word the check
# does not name as bash; a `for` after a function's spaced parentheses lists the path in a `for`'s
# words; a process substitution assigned to a name is a process substitution, not an array; a
# path on the second line of a backslash continuation is refused on that line; and a path the run
# text ends in with a backslash after it is more than the path. Five steps are no call and are
# refused nothing: a path in a comment after an empty redirect target, an `echo` on the line
# before a `bash` run (a newline pipes nothing), a `grep` before `|| bash` (an OR pipes nothing),
# an `echo` of a script that sources sa-c.sh (a printed path is not walked), and an `echo` of a
# message quoting a command in escaped backticks, which open no substitution.
echo "case: a path the scanner reads in a redirect, a quoted word or a substitution is refused by that form"
root=$(fixture sa-scanner-forms)
cp "$work/sa-scanner-runs/.github/scripts/sa-c.sh" "$root/.github/scripts/"
cat > "$root/.github/scripts/sa-q.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "nothing to fetch"
SH
cat > "$root/.github/scripts/sa-user.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/sa-c.sh
SH
chmod +x "$root/.github/scripts/sa-c.sh" "$root/.github/scripts/sa-q.sh" \
  "$root/.github/scripts/sa-user.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Feed it to bash on standard input
        timeout-minutes: 1
        run: |
          bash < .github/scripts/sa-c.sh
      - name: Echo it into bash with its errors
        timeout-minutes: 1
        run: |
          echo .github/scripts/sa-c.sh 2>&1 | bash
      - name: Run it through a quoted name holding a backslash
        timeout-minutes: 1
        run: |
          "ba\sh" .github/scripts/sa-c.sh
      - name: Run it through a quoted name holding another backslash
        timeout-minutes: 1
        run: |
          "bas\xh" .github/scripts/sa-c.sh
      - name: Pass it to bash in quotes closing on the next line
        timeout-minutes: 1
        run: |
          bash ".github/scripts/sa-c.sh;
          "
      - name: Run it through a substitution naming bash
        timeout-minutes: 1
        run: |
          "$(command -v bash)" .github/scripts/sa-c.sh
      - name: List it after a function's spaced parentheses
        timeout-minutes: 1
        run: |
          sa_f () for x in .github/scripts/sa-c.sh; do "$x"; done
      - name: Read it from a process substitution assigned to a name
        timeout-minutes: 1
        run: |
          out=<(.github/scripts/sa-c.sh)
      - name: Run it through eval over two lines
        timeout-minutes: 1
        run: |
          eval \
            .github/scripts/sa-c.sh
      - name: Pass it to bash before a trailing backslash
        timeout-minutes: 1
        run: bash .github/scripts/sa-c.sh\
      - name: Name it in a comment after an empty redirect
        timeout-minutes: 1
        run: |
          : > ""; true # see .github/scripts/sa-c.sh
      - name: Print it before a run of bash
        timeout-minutes: 1
        run: |
          echo "see .github/scripts/sa-c.sh"
          bash .github/scripts/sa-q.sh
      - name: Search for it before a run of bash
        timeout-minutes: 1
        run: |
          grep -q .github/scripts/sa-c.sh list.txt || bash .github/scripts/sa-q.sh
      - name: Print the name of a script that sources it
        timeout-minutes: 1
        run: |
          echo "see .github/scripts/sa-user.sh"
      - name: Print a command quoted in escaped backticks
        timeout-minutes: 1
        run: |
          echo "run \`.github/scripts/sa-c.sh\` to refresh the asset"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "10	Feed it to bash on standard input	a redirect" \
  "14	Echo it into bash with its errors	a pipe into a shell" \
  "18	Run it through a quoted name holding a backslash	an argument of .ba\\\\sh." \
  "22	Run it through a quoted name holding another backslash	an argument of .bas\\\\xh." \
  "26	Pass it to bash in quotes closing on the next line	an argument of .bash." \
  "31	Run it through a substitution naming bash	an argument of .\$(command -v bash)." \
  "35	List it after a function's spaced parentheses	the words of a .for." \
  "39	Read it from a process substitution assigned to a name	a process substitution" \
  "44	Run it through eval over two lines	an argument of .eval." \
  "47	Pass it to bash before a trailing backslash	an argument of .bash."; do
  IFS=$'\t' read -r line step form <<<"$refusal"
  check "refuses '$step' at line $line as $form" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/sa-c.sh in $form, a place this check does not read as a call, and that script fetches")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 10)"
for step in \
  "Name it in a comment after an empty redirect" \
  "Print it before a run of bash" \
  "Search for it before a run of bash" \
  "Print the name of a script that sources it" \
  "Print a command quoted in escaped backticks"; do
  check "neither charges nor refuses '$step'" "$(is "$(contains "$out" "step '$step'")" false)"
done

# A library one shell reaches through a looped run is charged once, at that path's product, and
# the run's own loop waits are charged with the run (dispatch://INFRA-1545, round 37). sa-lib.sh
# and sa-lib2.sh each retry at 180 s, 1050 s; sa-b.sh sources sa-lib.sh and nothing else, so a
# loop of two runs of it, each pass ending with `sleep 61`, reaches sa-lib.sh with a product of
# 2, 2 x 1050 + 2 x 61 = 2222 s, charged 2161 s until round 44 with the sleep once fewer times
# than the passes (dispatch://INFRA-1545, round 44). One more step sources sa-lib.sh and then
# sa-lib2.sh, two libraries, 2100 s; a walk that stopped at the first library charged it 1050 s.
# The steps that sourced sa-lib.sh inside a loop beside such a run, which pinned how a source's
# loop waits were charged, are gone with that charge: a source inside a loop is refused by name,
# which the r37-library-order case below proves.
echo "case: a library reached through a looped run is charged at that run's product, and two libraries are both charged"
root=$(fixture sa-library-paths)
cat > "$root/.github/scripts/sa-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cp "$root/.github/scripts/sa-lib.sh" "$root/.github/scripts/sa-lib2.sh"
cat > "$root/.github/scripts/sa-b.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/sa-lib.sh
SH
chmod +x "$root/.github/scripts/sa-b.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: The library reached through a looped run
        timeout-minutes: 1
        run: |
          for pass in 1 2; do
            .github/scripts/sa-b.sh
            sleep 61
          done
      - name: Two libraries
        timeout-minutes: 1
        run: |
          . .github/scripts/sa-lib.sh
          . .github/scripts/sa-lib2.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for charge in \
  "The library reached through a looped run	2222" \
  "Two libraries	2100"; do
  IFS=$'\t' read -r step seconds <<<"$charge"
  check "charges '$step' $seconds s" \
    "$(contains "$out" "step '$step' carries .timeout-minutes: 1., 60 s, and its own retries take $seconds s to exhaust")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A script whose loops the check cannot count is a run with no figure as soon as one of them runs
# a script that fetches, whatever its later loops run (dispatch://INFRA-1545, round 37). sa-wrap.sh
# loops, uncounted, over sa-c.sh, which retries at 180 s, and then, uncounted again, over sa-q.sh,
# which fetches nothing; the step loops, uncounted, over sa-wrap.sh. The refusal says one run of
# sa-wrap.sh reaches such a loop; a walk letting the last loop's answer stand for all of them read
# the quiet loop alone and printed `takes 0 s`.
echo "case: an uncounted loop over a fetching script leaves its script with no figure, though a later loop runs a quiet one"
root=$(fixture sa-unread-then-quiet)
cp "$work/sa-scanner-runs/.github/scripts/sa-c.sh" "$work/sa-scanner-forms/.github/scripts/sa-q.sh" \
  "$root/.github/scripts/"
cat > "$root/.github/scripts/sa-wrap.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
while read -r id; do
  .github/scripts/sa-c.sh "$id"
done <<<"$ids"
while read -r id; do
  .github/scripts/sa-q.sh "$id"
done <<<"$ids"
SH
chmod +x "$root/.github/scripts/sa-c.sh" "$root/.github/scripts/sa-q.sh" \
  "$root/.github/scripts/sa-wrap.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the wrapper for every id
        timeout-minutes: 30
        run: |
          while read -r id; do
            .github/scripts/sa-wrap.sh "$id"
          done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "is refused once" \
  "$(is "$(printf '%s\n' "$out" | grep -c 'this check can put no figure on it')" 1)"
check "says one run of sa-wrap.sh reaches an uncounted loop" \
  "$(contains "$out" 'One run of ..github/scripts/sa-wrap.sh. reaches a loop, its own or one in a script it calls')"
check "prints no figure for a run it cannot read" "$(is "$(contains "$out" 'takes 0 s')" false)"

# Group B of round 37's mutation pass over call_of (dispatch://INFRA-1545, round 37): each place
# a path can be written in, read the way call_of reads it. zz-c.sh retries at 180 s, 5 x 180 +
# 150 = 1050 s, the script the cases above write; these fixtures write it again, so the block
# reads alone. Every key is one minute, so a run the gate charges is refused with its figure.
echo "case: a run after /bin/bash or bash's own options, or in a $( ) inside an array's, is refused by name"
root=$(fixture sb-run-spellings)
cat > "$root/.github/scripts/zz-c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/zz-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run it through /bin/bash
        timeout-minutes: 1
        run: /bin/bash .github/scripts/zz-c.sh
      - name: Run it inside an array's substitution
        timeout-minutes: 1
        run: files=( $(.github/scripts/zz-c.sh) )
      - name: Run it after bash --
        timeout-minutes: 1
        run: bash -- .github/scripts/zz-c.sh
      - name: Run it after bash --norc
        timeout-minutes: 1
        run: bash --norc .github/scripts/zz-c.sh
      - name: Run it after bash -eo pipefail
        timeout-minutes: 1
        run: bash -eo pipefail .github/scripts/zz-c.sh
      - name: Run it after bash -e -u
        timeout-minutes: 1
        run: bash -e -u .github/scripts/zz-c.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
# A shell's name, whatever its options, and a substitution written inside another are no places
# this tree writes a run, so each is refused by name and charged nothing (dispatch://INFRA-1545,
# round 37).
for refusal in "9	Run it through /bin/bash	an argument of .bash." \
  "12	Run it inside an array's substitution	a .\$( … ). inside another" \
  "15	Run it after bash --	an argument of .bash." \
  "18	Run it after bash --norc	an argument of .bash." \
  "21	Run it after bash -eo pipefail	an argument of .bash." \
  "24	Run it after bash -e -u	an argument of .bash."; do
  IFS=$'\t' read -r line step form <<<"$refusal"
  check "refuses '$step' by name as $form" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/zz-c.sh in $form, a place this check does not read as a call")"
done
# `-eo` is a group of bash's options holding `o`, which takes the next word as its value, a form
# shell_operand does not read, so it is refused by name too (dispatch://INFRA-1545, round 74).
check "refuses the group -eo at line 21 by name" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=21::' '`bash`' '`-eo`')"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 7)"

# A path that is only a comment or text a command prints or searches is no call, whatever sits
# around it (dispatch://INFRA-1545, round 37): a comment on a body's first line, a comment that
# opens the line after a continuation, a word that starts at the path's slash, an `echo` whose
# next command is a shell it does not pipe into, a `printf` piped into a command that is no
# shell, and an `echo` piped into a `{ … }` group.
# A `printf` of the path is no non-call any more, since this tree prints a script's path with
# `echo` alone: the step printing it into grep is refused by name (dispatch://INFRA-1545, round 37).
echo "case: a comment, and echo or grep output that no shell runs, name a script and pass, and printf is refused"
root=$(fixture sb-non-calls)
cp "$work/sb-run-spellings/.github/scripts/zz-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/zz-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Name it in a comment on the body's first line
        timeout-minutes: 1
        run: |
          true # .github/scripts/zz-c.sh fetches the asset; this step only names it
          true
      - name: Name it in a comment after a continuation
        timeout-minutes: 1
        run: |
          true \
          # .github/scripts/zz-c.sh fetches the asset; this step only names it
      - name: Search for its bare name
        timeout-minutes: 1
        run: grep -q /zz-c.sh notes.txt || true
      - name: Echo it before a shell it does not pipe into
        timeout-minutes: 1
        run: echo "fetching with .github/scripts/zz-c.sh" && bash -c true
      - name: Print it into grep
        timeout-minutes: 1
        run: printf '%s\n' .github/scripts/zz-c.sh | grep -c zz-c
      - name: Echo it into a group
        timeout-minutes: 1
        run: echo .github/scripts/zz-c.sh | { read -r p; echo "$p"; }
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the printf by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=25::step 'Print it into grep' of job 'build' names .github/scripts/zz-c.sh in an argument of .printf., a place this check does not read as a call")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# The places call_of reads as no call, past the ones the case above with the same forms proves,
# each refused by name, form and rewrite (dispatch://INFRA-1545, round 37): a redirect target, a
# `case` word, the text of a retry_fetch, a pipe into a shell written as a path and one after
# another command, the words of a `bash` that are no script (behind `-- -c`, an option holding
# the path, an empty word, another script, a longer word), a command word holding more than the
# path, a `.` given a second word and a `source` given a longer one, and the arguments of
# `export` and `make`. None is charged.
echo "case: each further place call_of does not read is refused by name, and none is charged"
root=$(fixture sb-unread-forms)
cp "$work/sb-run-spellings/.github/scripts/zz-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/zz-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Write it through a redirect
        timeout-minutes: 1
        run: echo done > .github/scripts/zz-c.sh
      - name: Write its bare name through a redirect
        timeout-minutes: 1
        run: echo done > /zz-c.sh
      - name: Match it in a case
        timeout-minutes: 1
        run: case .github/scripts/zz-c.sh in *) true ;; esac
      - name: Name it in a retry's text
        timeout-minutes: 6
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the asset .github/scripts/zz-c.sh names" 30 -- curl -fsSL --max-time 30 -o a https://example.com/a
      - name: Echo it into a shell written as a path
        timeout-minutes: 1
        run: echo .github/scripts/zz-c.sh | /bin/bash
      - name: Echo it into a shell after another command
        timeout-minutes: 1
        run: |
          cd .
          echo .github/scripts/zz-c.sh | bash
      - name: Give it to bash behind -- -c
        timeout-minutes: 1
        run: bash -- -c .github/scripts/zz-c.sh
      - name: Give it to bash in an option
        timeout-minutes: 1
        run: bash --rcfile=.github/scripts/zz-c.sh -i
      - name: Give it to bash after an empty word
        timeout-minutes: 1
        run: bash "" .github/scripts/zz-c.sh
      - name: Give it to another script bash runs
        timeout-minutes: 1
        run: bash ./run.sh .github/scripts/zz-c.sh
      - name: Give bash a longer word
        timeout-minutes: 1
        run: bash .github/scripts/zz-c.sh.orig
      - name: Run a longer command word
        timeout-minutes: 1
        run: .github/scripts/zz-c.sh.orig
      - name: Run a command word that goes on past it
        timeout-minutes: 1
        run: .github/scripts/zz-c.sh/grep x
      - name: Give it to . as a second word
        timeout-minutes: 1
        run: . ./lib.sh .github/scripts/zz-c.sh
      - name: Source a longer word
        timeout-minutes: 1
        run: source .github/scripts/zz-c.sh.orig
      - name: Export it
        timeout-minutes: 1
        run: export P=.github/scripts/zz-c.sh
      - name: Export it with no name
        timeout-minutes: 1
        run: export -n .github/scripts/zz-c.sh
      - name: Give it to make
        timeout-minutes: 1
        run: make SCRIPT=.github/scripts/zz-c.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
command_word='Write the path as the command word'
for refusal in \
  "9	Write it through a redirect	a redirect" \
  "12	Write its bare name through a redirect	a redirect" \
  "15	Match it in a case	the words of a .case." \
  "20	Name it in a retry's text	an argument of .retry_fetch." \
  "23	Echo it into a shell written as a path	a pipe into a shell" \
  "28	Echo it into a shell after another command	a pipe into a shell" \
  "31	Give it to bash behind -- -c	an argument of .bash." \
  "34	Give it to bash in an option	an argument of .bash." \
  "37	Give it to bash after an empty word	an argument of .bash." \
  "40	Give it to another script bash runs	an argument of .bash." \
  "43	Give bash a longer word	an argument of .bash." \
  "46	Run a longer command word	a command word that holds more than the script's path" \
  "49	Run a command word that goes on past it	a command word that holds more than the script's path" \
  "52	Give it to . as a second word	an argument of ..." \
  "55	Source a longer word	an argument of .source." \
  "58	Export it	an argument of .export." \
  "61	Export it with no name	an argument of .export." \
  "64	Give it to make	an argument of .make."; do
  IFS=$'\t' read -r line step form <<<"$refusal"
  check "refuses '$step' by its file and line, the step, the script and the form" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/zz-c.sh in $form, a place this check does not read as a call, and that script fetches")"
  check "gives '$step' its rewrite" \
    "$(contains "$out" "step '$step' of job 'build' names .github/scripts/zz-c.sh in $form, .*$command_word")"
done
check "refuses nothing else" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 18)"

# The function forms rule 8 reads and refuses (dispatch://INFRA-1545, round 37). A run written in
# the body of `name() { … }`, its `{` on the name's line or on the next one, is refused as a
# function body, because the function runs it once for every call, from wherever it is called.
# A function written with the `function` keyword, with or without `()`, is no form rule 8 reads:
# its body reads as commands of the step, so the run is charged once, 1050 s, and the definition
# is refused by name, with the form to write instead, since the run would be charged once for
# any number of calls. sc-c.sh retries at 180 s, 5 x 180 + 150 = 1050 s.
echo "case: a run in a function body is refused as one, and a function with the function keyword is refused by name"
root=$(fixture sc-function-forms)
cat > "$root/.github/scripts/sc-c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/sc-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Brace on the next line
        timeout-minutes: 1
        run: |
          fetch()
          {
            .github/scripts/sc-c.sh
          }
          fetch
      - name: The function keyword
        timeout-minutes: 1
        run: |
          function fetch { .github/scripts/sc-c.sh; }
          fetch
      - name: The function keyword with parentheses
        timeout-minutes: 1
        run: |
          function fetch() { .github/scripts/sc-c.sh; }
          fetch
      - name: The function keyword, brace on the next line
        timeout-minutes: 1
        run: |
          function fetch
          {
            .github/scripts/sc-c.sh
          }
          fetch
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the brace on the next line as a function body at its line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Brace on the next line' of job 'build' names .github/scripts/sc-c.sh in a function body, a place this check does not read as a call")"
for refusal in \
  "18	The function keyword" \
  "23	The function keyword with parentheses" \
  "28	The function keyword, brace on the next line"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' by name at its definition" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' defines a function with the .function. keyword, a form this check does not read as a function, and a fetch, or a call of a script that fetches, is in its reach: written on its line or after it")"
  check "tells '$step' the form to write" \
    "$(contains "$out" "step '$step' of job 'build' defines a function with the .function. keyword, .*this check counts those calls only for a function written .name() { …; }.*Write the function in that form")"
  check "charges '$step' its run once, 1050 s" \
    "$(contains "$out" "step '$step' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 7)"

# What is not a function body is read as the run it is (dispatch://INFRA-1545, round 37): a brace
# group at the top level, after a command of two words, and one in a `case` arm whose pattern is
# the word `function`. Each runs sc-c.sh once, 1050 s, which `timeout-minutes: 1` does not hold.
# Read as a function body, each was refused as one and charged nothing.
echo "case: a brace group that is no function body is charged as a run"
root=$(fixture sc-brace-groups)
cp "$work/sc-function-forms/.github/scripts/sc-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/sc-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Log the fetch
        timeout-minutes: 1
        run: |
          echo "fetching"
          { .github/scripts/sc-c.sh; } > fetch.log
      - name: Fetch for one kind
        timeout-minutes: 1
        run: |
          case "$kind" in
            function) { .github/scripts/sc-c.sh; } ;;
          esac
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges the brace group's run, 1050 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Log the fetch' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
check "charges the case arm's run, 1050 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'Fetch for one kind' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A function whose body is not a `{ … }` group — a `( … )` subshell, or an `if` — is no form
# rule 8 reads (dispatch://INFRA-1545, round 37): its body reads as commands of the step, so its
# run is charged once, 1050 s, and the definition is refused by name, since the run would be
# charged once for the two calls each step makes. Round 37 read both bodies for a while, the
# subshell refused as a function body and the `if` charged once for each call, 2100 s; no step
# of this tree writes either.
echo "case: a function whose body is a subshell or an if is refused by name"
root=$(fixture sc-function-bodies)
cp "$work/sc-function-forms/.github/scripts/sc-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/sc-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A subshell body
        timeout-minutes: 1
        run: |
          fetch() (
            .github/scripts/sc-c.sh
          )
          fetch
          fetch
      - name: An if body
        timeout-minutes: 1
        run: |
          fetch() if [ -n "$GITHUB_TOKEN" ]; then {
            .github/scripts/sc-c.sh
          }; fi
          fetch
          fetch
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the run in 'A subshell body' as a subshell's" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'A subshell body' of job 'build' names .github/scripts/sc-c.sh in a .( … ). subshell, a place this check does not read as a call")"
for refusal in \
  "18	An if body"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' by name at its definition" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' defines a function with a body that is not a .{ … }. group, such as a .( … ). subshell, an .if., a loop or a .case., a form this check does not read as a function")"
  check "charges '$step' its run once, 1050 s" \
    "$(contains "$out" "step '$step' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# The lines occurrences_of joins and the comments it leaves out, which are bash's
# (dispatch://INFRA-1545, round 37). A run whose continuation line holds only a commented-out
# flag is a run, since the comment starts at the `#` and not at the run's line; a run after a
# comment line is a run, wherever the comment sits; a run in a `run: |-` body whose last line
# ends in a backslash is a run, with no line after it to join; a word of one character followed
# by a backslash and a line starting with `#` is one word, `x` and `#y` joined, to which the path
# is an argument, refused by name as any argument of a command this check does not know is; and a
# line starting with `#` inside a string is part of the string, so a string echoed into a shell
# holds the path whatever that shell makes of the line, and is refused as a pipe into a shell,
# as `echo <path> | bash` is. Each of the first three steps runs sc-c.sh, 1050 s.
echo "case: continued lines and comment lines are read as the shell reads them"
root=$(fixture sc-continued-lines)
cp "$work/sc-function-forms/.github/scripts/sc-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/sc-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A flag commented out
        timeout-minutes: 1
        run: |
          .github/scripts/sc-c.sh --all \
            #--verbose
      - name: After a comment
        timeout-minutes: 1
        run: |
          echo start
          # fetch the asset
          .github/scripts/sc-c.sh
      - name: A stray backslash at the end
        timeout-minutes: 1
        run: |-
          .github/scripts/sc-c.sh \
      - name: One word over two lines
        timeout-minutes: 1
        run: |
          x\
          #y .github/scripts/sc-c.sh
      - name: A comment piped into a shell
        timeout-minutes: 1
        run: |
          echo "set -eu
            # .github/scripts/sc-c.sh is the next job's, and this shell only prepares
          mkdir -p out" | bash
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for refusal in \
  "8	A flag commented out" \
  "13	After a comment" \
  "19	A stray backslash at the end"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "charges '$step' its run, 1050 s" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
done
check "refuses the argument of the joined word by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=26::step 'One word over two lines' of job 'build' names .github/scripts/sc-c.sh in an argument of .x#y., a place this check does not read as a call")"
check "refuses the string echoed into a shell at the path's line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=31::step 'A comment piped into a shell' of job 'build' names .github/scripts/sc-c.sh in a pipe into a shell, a place this check does not read as a call")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

# A continuation onto the last line of a body names the call by the line its logical line starts
# on (dispatch://INFRA-1545, round 37): the `run: |-` body ends with no newline, so the line the
# call is written on is the body's last, and it is joined to the `do \` above it all the same.
echo "case: a run continued behind a do onto a body's last line is refused by name at its line"
root=$(fixture sc-last-line)
cp "$work/sc-function-forms/.github/scripts/sc-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/sc-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch every row
        timeout-minutes: 1
        run: |-
          while read -r row; do \
            .github/scripts/sc-c.sh "$row"; done <<<"$rows"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the run continued behind do by name, at the path's line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Fetch every row' of job 'build' names .github/scripts/sc-c.sh in a command behind .do., a place this check does not read as a call")"

# Exception (d)'s closure reads the paths a credentialed job could run, and no other
# (dispatch://INFRA-1545, round 37): a path its step prints is a recognised non-call, so the
# script it names is held to the floor as any other script is, and so is one a credentialed
# script only prints; a script a credentialed script runs is the job's, and carries no retry. A
# path naming no script of the tree, here one an earlier step would write, is read as nothing.
# sc-bare.sh, sc-echoed.sh and sc-pub-inner.sh each fetch with a bare curl, which rule 1 refuses
# outside a credentialed job.
echo "case: exception (d) follows a credentialed job's runs and not the paths it prints"
root=$(fixture sc-credentialed)
for name in sc-bare sc-echoed sc-pub-inner; do
  cat > "$root/.github/scripts/$name.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
curl -fsSL -o asset https://example.com/asset
SH
done
cat > "$root/.github/scripts/sc-pub.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/sc-pub-inner.sh
echo "the next job runs .github/scripts/sc-echoed.sh"
SH
chmod +x "$root/.github/scripts/sc-bare.sh" "$root/.github/scripts/sc-echoed.sh" \
  "$root/.github/scripts/sc-pub-inner.sh" "$root/.github/scripts/sc-pub.sh"
cat >> "$root/.github/workflows/release.yaml" <<'YAML'
      - name: Run the script the build wrote
        run: .github/scripts/sc-built.sh
      - name: Say what runs next
        run: echo "the next job runs .github/scripts/sc-bare.sh"
      - name: Publish
        run: .github/scripts/sc-pub.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "holds the script a credentialed step prints to the floor" \
  "$(contains "$out" "::error file=.github/scripts/sc-bare.sh,line=3::.curl. fetches without the floor")"
check "holds the script a credentialed script prints to the floor" \
  "$(contains "$out" "::error file=.github/scripts/sc-echoed.sh,line=3::.curl. fetches without the floor")"
check "exempts the script a credentialed script runs from the floor" \
  "$(is "$(contains "$out" 'sc-pub-inner.sh,line=3::.curl. fetches without the floor')" false)"
# Its bare curl writes no `--max-time`, which leaves the publish step with no figure, so round 37
# refuses it by name as an excepted curl with no bound (dispatch://INFRA-1545, round 37).
check "refuses the excepted curl for writing no bound" \
  "$(contains "$out" "::error file=.github/scripts/sc-pub-inner.sh,line=3::.curl. is a site an exception above leaves unretried, and it writes no .--max-time.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

# A step running each of two scripts in a loop whose passes this check cannot read names each
# script once, at its first such call, and in the order of their paths (dispatch://INFRA-1545,
# round 37): sc-b.sh is run at body lines 1 and 3 and sc-a.sh at line 2, so sc-a.sh is named
# first, and sc-b.sh at its first call. The figure is the first one's, 1050 s.
echo "case: scripts in uncounted loops are named once each, at the first call, by path"
root=$(fixture sc-blocked-order)
cp "$work/sc-function-forms/.github/scripts/sc-c.sh" "$root/.github/scripts/sc-a.sh"
cp "$work/sc-function-forms/.github/scripts/sc-c.sh" "$root/.github/scripts/sc-b.sh"
chmod +x "$root/.github/scripts/sc-a.sh" "$root/.github/scripts/sc-b.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch every row twice
        timeout-minutes: 1
        run: |
          while read -r row; do
            .github/scripts/sc-b.sh "$row"
          done <<<"$rows"
          while read -r row; do
            .github/scripts/sc-a.sh "$row"
          done <<<"$rows"
          while read -r row; do
            .github/scripts/sc-b.sh "$row"
          done <<<"$rows"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names sc-a.sh, then sc-b.sh at its first call" \
  "$(contains "$out" "inside a loop whose passes it cannot read — ..github/scripts/sc-a.sh. at .github/workflows/case.yaml:14, inside the loop at line 13; ..github/scripts/sc-b.sh. at .github/workflows/case.yaml:11, inside the loop at line 10\. One run of that script takes 1050 s")"

# The places rule 8 does not read are refused in the order they are written
# (dispatch://INFRA-1545, round 37), so the annotations read down the file as the step does.
echo "case: unread places are refused in the order they are written"
root=$(fixture sc-unread-order)
cp "$work/sc-function-forms/.github/scripts/sc-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/sc-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run it two ways
        timeout-minutes: 1
        run: |
          p=.github/scripts/sc-c.sh; "$p"
          eval .github/scripts/sc-c.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the variable at line 10, then eval at line 11" \
  "$(is "$(printf '%s\n' "$out" | grep '^::error' | cut -d: -f3 | tr '\n' ' ')" "error file=.github/workflows/case.yaml,line=10 error file=.github/workflows/case.yaml,line=11 ")"

# The bound rule 8 names for an item it cannot price is the first item's (dispatch://INFRA-1545,
# round 37): the step's first uncounted site writes its bound as a variable and its second
# writes none, and the refusal speaks of the first. The wget step's bound is the `"$T"` it hands
# `retry_fetch`, a variable, and it is told so: until round 37's class search it was told this
# check finds no bound on it, while the refusal of the site itself quoted that `$T`.
echo "case: the bound an uncounted site is told of is its own"
root=$(fixture sc-bound-advice)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read every run and its jobs
        timeout-minutes: 30
        run: |
          read_run() {
            curl -fsSL --retry 7 --retry-all-errors --max-time "$bound" \
              -o "run-$1.json" "https://api.github.com/repos/$GITHUB_REPOSITORY/actions/runs/$1"
          }
          read_jobs() {
            curl -fsSL --retry 7 --retry-all-errors \
              -o "jobs-$1.json" "https://api.github.com/repos/$GITHUB_REPOSITORY/actions/runs/$1/jobs"
          }
          while read -r id; do
            read_run "$id"
            read_jobs "$id"
          done <<<"$ids"
      - name: Mirror every site
        timeout-minutes: 30
        run: |
          . .github/scripts/ci-retry.sh
          while read -r site; do
            retry_fetch "the mirror of $site" "$T" -- wget -m -np "$site"
          done < sites.txt
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "tells the first step its first site's bound is a variable" \
  "$(contains "$out" "step 'Read every run and its jobs' .*What one call of .curl. there takes is a figure this check cannot make, because its bound is written as a variable rather than as a number")"
check "gives the first step the variable bound's advice" \
  "$(contains "$out" "step 'Read every run and its jobs' .*it exists to allow\. Write that bound as a number")"
check "tells the wget step its bound is written as a variable" \
  "$(contains "$out" "step 'Mirror every site' .*What one call of .wget. there takes is a figure this check cannot make, because its bound is written as a variable rather than as a number")"
check "does not tell the wget step it finds no bound" \
  "$(is "$(contains "$out" "step 'Mirror every site' .*finds no bound")" false)"

# A `retry_fetch` whose seconds are a variable, around a verb that is no curl, is told by rule 8
# that its bound is written as a variable, as the refusal of the site itself says
# (dispatch://INFRA-1545, round 37). Inside a loop this check cannot count, rule 8 said it found
# no bound on one attempt and asked for a bound, while the site's own refusal said its seconds
# were written as `$READ_BOUND`; and a script holding that call, run in such a loop, was said to
# make attempts on which this check finds no bound. Round 37's class search found both, inputs D
# and D2: bound_spelled read the helper's seconds only around a curl writing `--max-time`.
echo "case: a retry_fetch whose seconds are a variable is told so by rule 8, around a gh api"
root=$(fixture r37-helper-variable-bound)
cat > "$root/.github/scripts/zz-v.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "the repository" "$READ_BOUND" -- gh api "repos/$1"
SH
chmod +x "$root/.github/scripts/zz-v.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read every listed repository
        timeout-minutes: 30
        run: |
          . .github/scripts/ci-retry.sh
          while read -r repo; do
            retry_fetch "the repository" "$READ_BOUND" -- gh api "repos/$repo"
          done < repos.txt
      - name: Run the reader for every listed repository
        timeout-minutes: 30
        run: |
          while read -r repo; do
            .github/scripts/zz-v.sh "$repo"
          done < repos.txt
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the step's site for its seconds written as a variable" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=12::.gh api. in step .Read every listed repository. is retried by .retry_fetch. with its seconds written as .\$READ_BOUND.')"
check "refuses the script's site for its seconds written as a variable" \
  "$(contains "$out" '::error file=.github/scripts/zz-v.sh,line=4::.gh api. is retried by .retry_fetch. with its seconds written as .\$READ_BOUND.')"
check "tells the step its bound is written as a variable" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Read every listed repository' .*What one call of .gh api. there takes is a figure this check cannot make, because its bound is written as a variable rather than as a number")"
check "does not tell the step it finds no bound" \
  "$(is "$(contains "$out" "step 'Read every listed repository' .*finds no bound")" false)"
check "does not ask the step to bound attempts its seconds bound" \
  "$(is "$(contains "$out" "step 'Read every listed repository' .*Bound each attempt")" false)"
check "tells the step running the script its attempts' bound is written as a variable" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=15::step 'Run the reader for every listed repository' .*One run of ..github/scripts/zz-v.sh. makes attempts whose bound is written as a variable rather than as a number, there or in a script it calls")"
check "does not tell the step running the script it finds no bound" \
  "$(is "$(contains "$out" "step 'Run the reader for every listed repository' .*finds no bound")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# The advice for a `retry_fetch` whose seconds are a variable, in a loop this check cannot count,
# is advice that turns the gate green (dispatch://INFRA-1545, round 37). It offered a
# DECLARED_LOOPS entry in place of the number, and an entry answers rule 8's refusal while the
# refusal of the site itself, which no declaration answers, stands, so the gate stays red. It
# asks for the number now, and offers the loop's count or a declaration after it. The step is
# refused; with the site declared and its seconds still a variable it is still refused, at the
# site; with the seconds written as 60 and the site declared, three listed repositories,
# 3 x (5 x 60 + 150) = 1350 s under a key of 30 minutes, it passes.
echo "case: a retry_fetch whose seconds are a variable, in a loop, is asked for the number before any declaration"
root=$(fixture r37-helper-variable-advice)
write_helper_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read every listed repository
        timeout-minutes: 30
        run: |
          . .github/scripts/ci-retry.sh
          while read -r repo; do
            retry_fetch "the repository" $1 -- gh api "repos/\$repo"
          done < repos.txt
YAML
}
write_helper_case '"$READ_BOUND"'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "asks for the helper's seconds as a number, which no declaration replaces" \
  "$(contains "$out" "step 'Read every listed repository' .*it exists to allow\. Write that bound as a number, the seconds .retry_fetch. takes before its .--., which is the figure this check multiplies by the passes it reads: the refusal of the site asks for that number too, and no declaration answers that refusal\.")"
check "offers the loop's count, or a declaration, only after the number" \
  "$(contains "$out" "step 'Read every listed repository' .*no declaration answers that refusal\. Then write the count where this check reads it, as literal words — a .for x in 1 2 3 4 5 6 7. — or where the passes are as many as the run finds work, declare that site in DECLARED_LOOPS")"
check "offers no declaration in place of the number" \
  "$(is "$(contains "$out" "step 'Read every listed repository' .*or declare that site in DECLARED_LOOPS in .github/scripts/check-download-retry.sh\. An entry there writes the seconds the site contributes to this budget, whole")" false)"
declare_entry "$root" '    DeclaredArithmetic(".github/workflows/case.yaml", "Read every listed repository", "gh api", 1350, "three listed repositories: 3 x (5 x 60 + 150) = 1350 s"),'
run_check "$root"
check "with the site declared and its seconds a variable, exits 1" "$(is "$status" 1)"
check "with the site declared and its seconds a variable, still refuses the site" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::.gh api. in step 'Read every listed repository' is retried by .retry_fetch. with its seconds written as")"
check "with the site declared and its seconds a variable, refuses nothing else" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
write_helper_case 60
run_check "$root"
check "with the seconds written as a number and the site declared, passes" "$(is "$status" 0)"

# What a script in an uncounted loop is told of is every script it runs, not the first
# (dispatch://INFRA-1545, round 37): sc-outer.sh runs sc-quiet.sh, which fetches nothing, and
# then sc-var.sh, whose curl writes its bound as a variable.
echo "case: a script's unread bound is read through every script it runs"
root=$(fixture sc-unpriced-reach)
cat > "$root/.github/scripts/sc-quiet.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "nothing to fetch"
SH
cat > "$root/.github/scripts/sc-var.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
bound=60
curl -fsSL --retry 7 --retry-all-errors --max-time "$bound" -o run.json \
  "https://api.github.com/repos/$GITHUB_REPOSITORY/actions/runs/$1"
SH
cat > "$root/.github/scripts/sc-outer.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
.github/scripts/sc-quiet.sh
.github/scripts/sc-var.sh "$1"
SH
chmod +x "$root/.github/scripts/sc-quiet.sh" "$root/.github/scripts/sc-var.sh" \
  "$root/.github/scripts/sc-outer.sh"
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
          printf 'a\nb\n' | while read -r id; do
            .github/scripts/sc-outer.sh "$id"
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says the second script it runs bounds its attempts with a variable" \
  "$(contains "$out" 'One run of ..github/scripts/sc-outer.sh. makes attempts whose bound is written as a variable rather than as a number, there or in a script it calls')"

# A library sourced inside a loop is refused by name, whether the loop writes its passes or not
# (dispatch://INFRA-1545, round 37): it is no call rule 8 charges, so neither step is charged,
# and each is told to source the library once, before the loop. Round 37 charged the counted
# loop for a while, three passes of 1050 s and two sleeps of 10 s, 3170 s, through a model of
# every path to a library that no step of this tree needs; the uncounted loop was refused as one
# whose passes this check cannot read.
echo "case: a library sourced inside a loop is refused by name, counted loop or not"
root=$(fixture sc-library-waits)
cp "$work/sc-function-forms/.github/scripts/sc-c.sh" "$root/.github/scripts/sc-lib.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source it three times
        timeout-minutes: 1
        run: |
          for pass in 1 2 3; do
            . .github/scripts/sc-lib.sh
            sleep 10
          done
      - name: Source it on every row read
        timeout-minutes: 1
        run: |
          while read -r row; do
            . .github/scripts/sc-lib.sh
          done < rows.txt
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for refusal in \
  "11	Source it three times" \
  "18	Source it on every row read"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' by name at the source" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/sc-lib.sh in a source inside a loop, a place this check does not read as a call, and that script fetches")"
  check "tells '$step' to source the library once, before the loop" \
    "$(contains "$out" "step '$step' of job 'build' names .github/scripts/sc-lib.sh in a source inside a loop, .*Source the library once, before the loop")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A step using a composite action whose own step loops uncounted is refused for that loop
# (dispatch://INFRA-1545, round 37): the action's arithmetic is None, and the step running it is
# refused by name rather than priced.
echo "case: a uses: step whose action loops uncounted is refused for that loop"
root=$(fixture sc-action-loop)
mkdir -p "$root/.github/actions/sc-rows"
cat > "$root/.github/actions/sc-rows/action.yml" <<'YAML'
name: Rows
description: Reads every row it is handed.
runs:
  using: composite
  steps:
    - name: Read every row
      shell: bash
      run: |
        . .github/scripts/ci-retry.sh
        while read -r url; do
          retry_fetch "a row" 30 -- curl -fsSL --max-time 30 -o row "$url"
        done < urls.txt
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the rows
        timeout-minutes: 5
        uses: ./.github/actions/sc-rows
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the step for the action's loop" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Read the rows' carries .timeout-minutes: 5. and this check can put no figure on it: it runs a retried call inside a loop whose passes it cannot read — .curl. at .github/actions/sc-rows/action.yml:11, inside the loop at line 10\. One call there takes 300 s")"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

# A path in text the gate cannot read as any command is refused by name as such
# (dispatch://INFRA-1545, round 37): a `$(` that never closes leaves the words before it in no
# command, and a comment on the line above and a `#` inside a word before the path do not make
# it a comment. zz-c.sh retries at 180 s, 1050 s.
echo "case: a path in text the gate reads no command in is refused as such, not read as a comment"
root=$(fixture sb-no-command)
cat > "$root/.github/scripts/zz-c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/zz-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Name it before a substitution that never closes
        timeout-minutes: 1
        run: |
          true # a comment that names no script
          echo a#b .github/scripts/zz-c.sh $(
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the path at its line as a place it reads no command at" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Name it before a substitution that never closes' of job 'build' names .github/scripts/zz-c.sh in a place this check reads no command at, a place this check does not read as a call, and that script fetches")"

# A library sourced in a loop and again outside it is refused by name at the loop's source,
# whichever of the two is written first (dispatch://INFRA-1545, round 37). r37-lib.sh retries at
# 180 s, 5 x 180 + 150 = 1050 s; three passes sourcing it, each ending with `sleep 60`, are
# 3 x 1050 + 3 x 60 = 3330 s, and each step's key of 54 minutes, 3240 s, is under that. The gate
# before round 37's model, at 179c3dc9, read a path to a library as its names, so the two sources
# were one path and the wait of the source read later stood for both: it charged the step writing
# the loop first 3150 s and passed that key. A source inside a loop is now no call rule 8 charges,
# so neither loop is charged and each is refused by name; the source outside the loop is charged,
# 1050 s, which the key holds.
echo "case: a library sourced in a loop and again outside it is refused by name in either order"
root=$(fixture r37-library-order)
cat > "$root/.github/scripts/r37-lib.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: The loop written first
        timeout-minutes: 54
        run: |
          for pass in 1 2 3; do
            . .github/scripts/r37-lib.sh
            sleep 60
          done
          . .github/scripts/r37-lib.sh
      - name: The loop written last
        timeout-minutes: 54
        run: |
          . .github/scripts/r37-lib.sh
          for pass in 1 2 3; do
            . .github/scripts/r37-lib.sh
            sleep 60
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for refusal in \
  "11	The loop written first" \
  "20	The loop written last"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' by name at the loop's source" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/r37-lib.sh in a source inside a loop, a place this check does not read as a call, and that script fetches")"
  check "tells '$step' to source the library once, before the loop" \
    "$(contains "$out" "step '$step' of job 'build' names .github/scripts/r37-lib.sh in a source inside a loop, .*Source the library once, before the loop")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A comment is where bash reads one, from a `#` that starts a word outside quotes to the end of
# its physical line (dispatch://INFRA-1545, round 37). `true;#` and `(true)#` each open one, so
# the path after them is no call and is neither charged nor refused; a comment line ending in a
# backslash ends at its newline all the same, so the run on the next line is charged, 1050 s;
# and a line starting with `#` inside a string spanning lines is part of the string, so the path
# on it is an argument of `die`, refused at that line. These were decided by the line's text:
# the first two were refused as a place no command is read at, the backslash joined the run to
# the comment and charged it nothing, and the string's line was left out as a comment.
echo "case: a comment is read where bash reads one, and nowhere else"
root=$(fixture r37-comments)
cp "$work/r37-library-order/.github/scripts/r37-lib.sh" "$root/.github/scripts/r37-c.sh"
chmod +x "$root/.github/scripts/r37-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Name it in a comment after a semicolon
        timeout-minutes: 1
        run: true;# .github/scripts/r37-c.sh fetches the asset; this step only names it
      - name: Name it in a comment after a subshell
        timeout-minutes: 1
        run: (true)# .github/scripts/r37-c.sh fetches the asset
      - name: Run it after a comment ending in a backslash
        timeout-minutes: 1
        run: |
          # the next line fetches \
          .github/scripts/r37-c.sh
      - name: Name it in a string whose second line starts with a hash
        timeout-minutes: 1
        run: |
          die() { echo "$*" >&2; exit 1; }
          die "see
            # .github/scripts/r37-c.sh"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for step in "Name it in a comment after a semicolon" "Name it in a comment after a subshell"; do
  check "neither charges nor refuses '$step'" "$(is "$(contains "$out" "step '$step'")" false)"
done
check "charges the run after the comment, 1050 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=14::step 'Run it after a comment ending in a backslash' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
check "refuses the path in the string as an argument of die, at its line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=23::step 'Name it in a string whose second line starts with a hash' of job 'build' names .github/scripts/r37-c.sh in an argument of .die., a place this check does not read as a call, and that script fetches")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# bash's own options as rule 8 reads them (dispatch://INFRA-1545, round 37): `-o` and `+o` take
# the word after them as their value, so the script is the word after that, and the run is
# charged, 1050 s. A lone `-`, and the values `-O`, `+O`, `--rcfile` and `--init-file` take, are
# not modelled: each reads as the script, so the path after it is an argument of the shell,
# refused by name where its script fetches. That refusal is one this check accepts on a shape no
# step of this tree writes; round 37 read them as bash does for a while, and charged each run.
# Behind `-O extglob`, a `-c` is read the same way, the value of `-O` standing as the script.
echo "case: a run after bash or sh, whatever its options, is refused as an argument of the shell"
root=$(fixture r37-bash-options)
cp "$work/r37-comments/.github/scripts/r37-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/r37-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run it after bash -
        timeout-minutes: 1
        run: bash - .github/scripts/r37-c.sh
      - name: Run it after sh -
        timeout-minutes: 1
        run: sh - .github/scripts/r37-c.sh
      - name: Run it after bash -O extglob
        timeout-minutes: 1
        run: bash -O extglob .github/scripts/r37-c.sh
      - name: Run it after bash +O extglob
        timeout-minutes: 1
        run: bash +O extglob .github/scripts/r37-c.sh
      - name: Run it after bash -o pipefail
        timeout-minutes: 1
        run: bash -o pipefail .github/scripts/r37-c.sh
      - name: Run it after bash +o posix
        timeout-minutes: 1
        run: bash +o posix .github/scripts/r37-c.sh
      - name: Run it after bash --rcfile
        timeout-minutes: 1
        run: bash --rcfile /dev/null .github/scripts/r37-c.sh
      - name: Run it after bash --init-file
        timeout-minutes: 1
        run: bash --init-file /dev/null .github/scripts/r37-c.sh
      - name: Give it to bash -c after -O
        timeout-minutes: 1
        run: bash -O extglob -c .github/scripts/r37-c.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for refusal in \
  "9	Run it after bash -	bash" \
  "12	Run it after sh -	sh" \
  "15	Run it after bash -O extglob	bash" \
  "18	Run it after bash +O extglob	bash" \
  "27	Run it after bash --rcfile	bash" \
  "30	Run it after bash --init-file	bash" \
  "33	Give it to bash -c after -O	bash"; do
  IFS=$'\t' read -r line step shell <<<"$refusal"
  check "refuses '$step' as an argument of $shell" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/r37-c.sh in an argument of .$shell., a place this check does not read as a call, and that script fetches")"
done
for refusal in "21	Run it after bash -o pipefail" "24	Run it after bash +o posix"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' as an argument of bash, as every shell's run is" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/r37-c.sh in an argument of .bash., a place this check does not read as a call")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 9)"

# A word of a `[[ … ]]`, an extglob pattern and a `case` pattern are not modelled
# (dispatch://INFRA-1545, round 37): their `(`, `)`, `|`, `<` and `>` read as the shell's own, so
# a path written there is a run charged in full or an argument refused where its script fetches,
# never nothing. In an extglob pattern, a parenthesised pattern and a later parenthesised pattern,
# the `(` reads as a subshell, so the path is a run in a `( … )` subshell, refused by name. In a
# pattern on its own line the path reads as a run and is charged 1050 s, which the step does not
# wait for: an overcharge this check accepts on a shape no step of this tree writes. After `[[ -x`
# it is an argument of `-x`, refused by name (dispatch://INFRA-1545, round 37: until round 37
# this comment said the extglob and the parenthesised patterns were charged). A run in a case
# arm, after a `[[ … ]]` and inside a `$(…)` a `[[ … ]]` tests is a run, 1050 s each. Round 37
# read the patterns and the tests for a while, and refused each path written in one.
echo "case: a path in a [[ … ]] or a case pattern is charged as a run or refused, and a run beside one is charged"
root=$(fixture r37-tested-words)
cp "$work/r37-comments/.github/scripts/r37-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/r37-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Match it in an extglob pattern
        timeout-minutes: 1
        run: |
          [[ "$f" == @(.github/scripts/r37-c.sh|other) ]] && echo yes
      - name: Test it in an if
        timeout-minutes: 1
        run: |
          if [[ -x .github/scripts/r37-c.sh && ( -n "$f" || -z "$g" ) ]]; then echo yes; fi
      - name: Match it in a parenthesised case pattern
        timeout-minutes: 1
        run: |
          case "$f" in (.github/scripts/r37-c.sh) echo yes;; esac
      - name: Match it in a case pattern on its own line
        timeout-minutes: 1
        run: |
          case "$f" in
            other | .github/scripts/r37-c.sh) echo yes ;;
          esac
      - name: Match it in a later pattern
        timeout-minutes: 1
        run: |
          case "$f" in a) true;; (b|.github/scripts/r37-c.sh) true;; esac
      - name: Run it in a case arm
        timeout-minutes: 1
        run: |
          case "$kind" in (fetch) .github/scripts/r37-c.sh;; esac
      - name: Run it after a test
        timeout-minutes: 1
        run: |
          [[ -n "$f" ]] && .github/scripts/r37-c.sh
      - name: Run it in a substitution a test reads
        timeout-minutes: 1
        run: |
          [[ "$(.github/scripts/r37-c.sh)" == ok ]]
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the path after [[ -x as an argument of -x" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=14::step 'Test it in an if' of job 'build' names .github/scripts/r37-c.sh in an argument of .-x., a place this check does not read as a call, and that script fetches")"
for refusal in "10	Match it in an extglob pattern" "18	Match it in a parenthesised case pattern" \
  "28	Match it in a later pattern"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step', whose parenthesis reads as a subshell, by name" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/r37-c.sh in a .( … ). subshell, a place this check does not read as a call")"
done
for step in "Match it in a case pattern on its own line" \
  "Run it in a case arm" "Run it after a test" "Run it in a substitution a test reads"; do
  check "charges '$step' one run, 1050 s" \
    "$(contains "$out" "step '$step' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 8)"

# A function spelled any way but `name() { … }` is refused by name where a call of a script that
# fetches follows it (dispatch://INFRA-1545, round 37): a blank before the `()`, with or without
# the `function` keyword, a body that is a `( … )` subshell, on the name's line or on the next,
# with or without the keyword, and a definition after a reserved word on its line. Read as the
# commands of the step, its run is charged once, 1050 s, which each step's key of 18 minutes,
# 1080 s, holds; the three steps calling their function twice wait 2100 s. The gate at 179c3dc9
# charged those three 1050 s and passed them: the blank and the reserved word hid the function,
# and the subshell's calls sat on its line. Round 37 then read the blank and the subshell as a
# function body, which no step of this tree writes. The subshell on the name's line is charged
# for both calls, which sit on the line of the function, 2100 s, and refused for that too.
echo "case: a function spelled with a blank, the function keyword, a subshell body or after a reserved word is refused by name"
root=$(fixture r37-function-spellings)
cp "$work/r37-comments/.github/scripts/r37-c.sh" "$root/.github/scripts/"
chmod +x "$root/.github/scripts/r37-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A blank before the parentheses
        timeout-minutes: 18
        run: |
          fetch () { .github/scripts/r37-c.sh; }
          fetch
          fetch
      - name: The function keyword and a blank before the parentheses
        timeout-minutes: 18
        run: |
          function fetch () { .github/scripts/r37-c.sh; }
          fetch
      - name: A subshell body on the name's line
        timeout-minutes: 18
        run: |
          fetch() ( .github/scripts/r37-c.sh ); fetch; fetch
      - name: A subshell body on the next line
        timeout-minutes: 18
        run: |
          fetch()
          (
            .github/scripts/r37-c.sh
          )
          fetch
      - name: The function keyword and a subshell body
        timeout-minutes: 18
        run: |
          function fetch ( .github/scripts/r37-c.sh )
          fetch
      - name: A definition after a reserved word
        timeout-minutes: 18
        run: |
          if true; then fetch() { .github/scripts/r37-c.sh; }; fi
          fetch
          fetch
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for refusal in \
  "10	A blank before the parentheses	with a blank before or inside its .()." \
  "16	The function keyword and a blank before the parentheses	with the .function. keyword" \
  "38	A definition after a reserved word	after a reserved word on its line"; do
  IFS=$'\t' read -r line step shape <<<"$refusal"
  check "refuses '$step' by name at its definition" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' defines a function $shape.*, a form this check does not read as a function, and a fetch, or a call of a script that fetches, is in its reach: written on its line or after it")"
  check "tells '$step' the form to write" \
    "$(contains "$out" "step '$step' of job 'build' defines a function .*only for a function written .name() { …; }.*Write the function in that form")"
done
# A run inside a subshell body is a run in a subshell, which this tree writes nowhere, so it is
# refused by name as one and the definition holds no call this check reads (dispatch://INFRA-1545,
# round 37).
for refusal in "21	A subshell body on the name's line" "27	A subshell body on the next line" \
  "33	The function keyword and a subshell body"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses the run in '$step' as a subshell's" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/r37-c.sh in a .( … ). subshell, a place this check does not read as a call")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 6)"

# A fetch inside a function spelled any way but `name() { … }` is refused by name
# (dispatch://INFRA-1545, round 37): a blank before the `()`, the `function` keyword, a subshell
# body on the name's line or over several, an `if` body, and a function defined inside another
# function's body. Each step calls the function holding one `retry_fetch` at 300 s,
# 5 x 300 + 150 = 1650 s, twice, so it waits 2 x 1650 = 3300 s, and its key of 28 minutes,
# 1680 s, is under that. The gate at 179c3dc9 charged every one of them but the `if` body 1650 s
# and passed that key; round 37 then read each spelling but the nested one as a function, which
# no step of this tree writes. Read as commands of the step, each fetch is charged once, 1650 s,
# except where the calls sit on the line of the function, 3300 s, which the key does not hold.
echo "case: a fetch inside a function spelled any way but name() { … } is refused by name"
root=$(fixture r37-site-functions)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A blank before the parentheses
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch () { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fetch
          fetch
      - name: The function keyword
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          function fetch { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fetch
          fetch
      - name: A subshell body on the name's line
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() ( retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a ); fetch; fetch
      - name: A fetch below a subshell body's first command
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() (
            cd "$RUNNER_TEMP"
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          )
          fetch
          fetch
      - name: An if body
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() if true; then retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; fi
          fetch
          fetch
      - name: A function defined inside another
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          outer() {
            inner() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          }
          outer
          inner
          inner
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "11	A blank before the parentheses	with a blank before or inside its .()." \
  "18	The function keyword	with the .function. keyword" \
  "25	A subshell body on the name's line	with a body that is not a .{ … }. group" \
  "30	A fetch below a subshell body's first command	with a body that is not a .{ … }. group" \
  "40	An if body	with a body that is not a .{ … }. group" \
  "48	A function defined inside another	inside another function's body"; do
  IFS=$'\t' read -r line step shape <<<"$refusal"
  check "refuses '$step' by name at its definition" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' defines a function $shape.*, a form this check does not read as a function, and a fetch, or a call of a script that fetches, is in its reach: written on its line or after it")"
done
for step in "A subshell body on the name's line" "An if body"; do
  check "charges '$step' both calls, which sit on its line, 3300 s" \
    "$(contains "$out" "step '$step' carries .timeout-minutes: 28., 1680 s, and its own retries take 3300 s to exhaust")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 8)"

# A heredoc fed to a shell given `-O`, `+O`, `--rcfile` or `--init-file` is refused by name where
# its body holds a fetch or names a script that fetches (dispatch://INFRA-1545, round 37): this
# check reads that option's value as the shell's script and the body as data, where the shell
# runs the body, so the run of r37-c.sh in it, 1050 s, would be charged nothing under a key of
# 1 minute; the gate at 179c3dc9 passed each of those steps. A body holding a retried curl is
# refused the same way, and one that fetches nothing is not. Round 37 read those options as bash
# does for a while, and charged each body; no step of this tree writes one. A body fed to `-e`, a
# lone `-`, `sh -eu`, `bash -c` or `bash - <script>` is refused by name the same way and charged
# nothing, since every shell is a consumer this tree feeds no heredoc to, whatever its options:
# `-e`, `-` and `sh -eu` run the body, `bash -c` runs its text, which may read the body, and
# `bash - <script>` runs the script with the body as its input (dispatch://INFRA-1545, round 37:
# until round 37 this comment said the first three were charged, the fourth was data and the
# last an accepted overcharge).
echo "case: a heredoc fed to a shell given an option rule 8 does not read is refused by name where it fetches"
root=$(fixture r37-heredoc-options)
cat > "$root/.github/scripts/r37-c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/scripts/r37-quiet.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
cat
SH
chmod +x "$root/.github/scripts/r37-c.sh" "$root/.github/scripts/r37-quiet.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Feed it to bash -O extglob
        timeout-minutes: 1
        run: |
          bash -O extglob <<'EOF'
          .github/scripts/r37-c.sh
          EOF
      - name: Feed it to bash +O extglob
        timeout-minutes: 1
        run: |
          bash +O extglob <<'EOF'
          .github/scripts/r37-c.sh
          EOF
      - name: Feed it to bash --rcfile
        timeout-minutes: 1
        run: |
          bash --rcfile /dev/null <<'EOF'
          .github/scripts/r37-c.sh
          EOF
      - name: Feed it to bash --init-file
        timeout-minutes: 1
        run: |
          bash --init-file /dev/null <<'EOF'
          .github/scripts/r37-c.sh
          EOF
      - name: Feed it to bash -e
        timeout-minutes: 1
        run: |
          bash -e <<'EOF'
          .github/scripts/r37-c.sh
          EOF
      - name: Feed it to bash -
        timeout-minutes: 1
        run: |
          bash - <<'EOF'
          .github/scripts/r37-c.sh
          EOF
      - name: Feed it to sh -eu
        timeout-minutes: 1
        run: |
          sh -eu <<'EOF'
          .github/scripts/r37-c.sh
          EOF
      - name: Feed it to bash -c
        timeout-minutes: 1
        run: |
          bash -c 'cat' <<'EOF'
          .github/scripts/r37-c.sh
          EOF
      - name: Feed it to a script bash runs after -
        timeout-minutes: 1
        run: |
          bash - .github/scripts/r37-quiet.sh <<'EOF'
          .github/scripts/r37-c.sh
          EOF
      - name: Feed a fetch to bash -O extglob
        timeout-minutes: 1
        run: |
          bash -O extglob <<'EOF'
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
          EOF
      - name: Feed nothing that fetches to bash -O extglob
        timeout-minutes: 1
        run: |
          bash -O extglob <<'EOF'
          echo "nothing to fetch"
          EOF
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for refusal in \
  "10	Feed it to bash -O extglob" \
  "16	Feed it to bash +O extglob" \
  "22	Feed it to bash --rcfile" \
  "28	Feed it to bash --init-file" \
  "64	Feed a fetch to bash -O extglob"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' by name at its operator" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' feeds a heredoc to .[a-z/]*., and the body holds a fetch or names a script that fetches")"
  check "tells '$step' to write the option in the body" \
    "$(contains "$out" "step '$step' of job 'build' feeds a heredoc .*Write the body's commands in the step itself")"
done
# Every shell is a consumer this tree feeds no heredoc to, whatever its options, so each body
# that names a script that fetches is refused by name and charged nothing (dispatch://INFRA-1545,
# round 37).
for refusal in "34	Feed it to bash -e" "40	Feed it to bash -" "46	Feed it to sh -eu" \
  "52	Feed it to bash -c" "58	Feed it to a script bash runs after -"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' by name at its operator" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' feeds a heredoc to .[a-z]*., and the body holds a fetch")"
done
check "neither charges nor refuses the body that fetches nothing" \
  "$(is "$(contains "$out" "step 'Feed nothing that fetches to bash -O extglob'")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 10)"

# A fetch in the text of a `bash -c` or an `eval` runs once for each pass of the loops around the
# command, and a loop or a function inside that text is refused by name where the text fetches
# (dispatch://INFRA-1545, round 37). Each fetch is one `retry_fetch` at 300 s,
# 5 x 300 + 150 = 1650 s, run twice, 3300 s, under a key of 28 minutes, 1680 s; the gate at
# 179c3dc9 charged each of the first five steps 1650 s and passed it. The text's commands took no
# loop around the command, and a loop or a function in the text was not read. Text whose loop
# fetches nothing is neither charged nor refused.
echo "case: a fetch in bash -c or eval text is charged for the loops around it, and a loop or a function in the text is refused by name"
root=$(fixture r37-shell-text)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A fetch in bash -c text inside a loop
        timeout-minutes: 28
        run: |
          for pass in 1 2; do
            bash -c 'retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a'
          done
      - name: A fetch in eval text inside a loop
        timeout-minutes: 28
        run: |
          for pass in 1 2; do
            eval 'retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a'
          done
      - name: A loop in bash -c text
        timeout-minutes: 28
        run: |
          bash -c 'for pass in 1 2; do retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; done'
      - name: A function in bash -c text
        timeout-minutes: 28
        run: |
          bash -c 'fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }; fetch; fetch'
      - name: A loop in eval text
        timeout-minutes: 28
        run: |
          eval 'for pass in 1 2; do retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; done'
      - name: A loop in bash -c text that fetches nothing
        timeout-minutes: 28
        run: |
          bash -c 'for pass in 1 2; do echo "$pass"; done'
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for step in "A fetch in bash -c text inside a loop" "A fetch in eval text inside a loop"; do
  check "charges '$step' both passes, 3300 s" \
    "$(contains "$out" "step '$step' carries .timeout-minutes: 28., 1680 s, and its own retries take 3300 s to exhaust")"
done
for refusal in \
  "22	A loop in bash -c text" \
  "26	A function in bash -c text" \
  "30	A loop in eval text"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' by name at its line" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' runs .[a-z -]*. text, whose text holds a loop or a function, and a fetch")"
  check "tells '$step' where to write the loop or the function" \
    "$(contains "$out" "step '$step' of job 'build' runs .[a-z -]*. text.*write a loop or a function in the step itself or in a script under .github/scripts/ that the step runs")"
done
check "neither charges nor refuses the text that fetches nothing" \
  "$(is "$(contains "$out" "step 'A loop in bash -c text that fetches nothing'")" false)"
check "refuses the eval text in a loop by name for its form" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=17::step 'A fetch in eval text inside a loop' of job 'build' runs .eval. text, a form other than .bash -c., and a fetch is written in it")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 6)"

# A library one shell reaches along two paths of different products is charged at the larger,
# whichever of the two is written first (dispatch://INFRA-1545, round 37). ka-lib.sh defines a
# function retrying at 180 s and calls it nowhere, so it costs 1050 s once; ka-run.sh sources it
# and calls that function. Each step sources ka-lib.sh itself, a product of 1, and runs ka-run.sh
# three times, which reaches ka-lib.sh with a product of 3: the shell waits for three calls of
# the function, 3 x 1050 = 3150 s, and sourcing the library defines it and fetches nothing. A
# walk keeping the path found first charged the step sourcing first 1050 s; one keeping the path
# found last, or the path whose names sort first whatever its product, charged the other 1050 s.
echo "case: a library reached at two products is charged at the larger, in either order"
root=$(fixture ka-library-products)
cat > "$root/.github/scripts/ka-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
ka_fetch() {
  retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
}
SH
cat > "$root/.github/scripts/ka-run.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ka-lib.sh
ka_fetch
SH
chmod +x "$root/.github/scripts/ka-run.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source first, then the looped run
        timeout-minutes: 1
        run: |
          . .github/scripts/ka-lib.sh
          for pass in 1 2 3; do
            .github/scripts/ka-run.sh
          done
      - name: The looped run, then source
        timeout-minutes: 1
        run: |
          for pass in 1 2 3; do
            .github/scripts/ka-run.sh
          done
          . .github/scripts/ka-lib.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for charge in \
  "8	Source first, then the looped run" \
  "15	The looped run, then source"; do
  IFS=$'\t' read -r line step <<<"$charge"
  check "charges '$step' the looped run's product, 3150 s" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' carries .timeout-minutes: 1., 60 s, and its own retries take 3150 s to exhaust")"
  # The step sources the library and runs a script that sources it again, two paths, so it is
  # refused by name as well where no DECLARED_LIBRARIES entry covers it, though here the larger
  # product is the step's whole wait (dispatch://INFRA-1545, round 37).
  check "refuses '$step' by name for the library it reaches along two paths" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' reaches .github/scripts/ka-lib.sh, a library that fetches, along more than one path")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# A library reached along two paths of one product is charged through the path whose names sort
# first, whichever of the two is written first (dispatch://INFRA-1545, round 37). ka-z.sh retries
# at 180 s, 1050 s, and sources ka-lib.sh, which runs ka-z.sh once a job, the first time any
# shell sources it: the marker file it touches is seen by every later shell. Each step runs
# ka-z.sh and sources ka-lib.sh, so ka-lib.sh is reached at a product of 1 both from the step
# and through ka-z.sh. The shell runs ka-z.sh twice in either order, 2 x 1050 = 2100 s: the
# step's run, and ka-lib.sh's one run, from whichever shell sourced it first. The path the step
# sources it on, (ka-lib.sh), sorts before (ka-z.sh, ka-lib.sh), and through it ka-lib.sh's run
# of ka-z.sh is a run to charge, where through ka-z.sh it re-enters a file the path is inside and
# is charged nothing. A walk keeping the path it found first charged the step running ka-z.sh
# first 1050 s; one keeping the path it found last, the step sourcing first. Each script reaches
# itself through the other, and each fetches, so each is refused by name at its call of the
# other as well: the same shell under other names was charged 1050 s, and a guard each process
# sets for itself runs ka-z.sh three times, which no walk charges (dispatch://INFRA-1545, round
# 37).
echo "case: a library reached along two paths of one product is charged through the path that sorts first, in either order"
root=$(fixture ka-library-tie)
cat > "$root/.github/scripts/ka-lib.sh" <<'SH'
#!/usr/bin/env bash
[ -e "$RUNNER_TEMP/ka-z.done" ] || { touch "$RUNNER_TEMP/ka-z.done"; .github/scripts/ka-z.sh; }
SH
cat > "$root/.github/scripts/ka-z.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
. .github/scripts/ka-lib.sh
SH
chmod +x "$root/.github/scripts/ka-z.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run first, then source
        timeout-minutes: 1
        run: |
          .github/scripts/ka-z.sh
          . .github/scripts/ka-lib.sh
      - name: Source first, then run
        timeout-minutes: 1
        run: |
          . .github/scripts/ka-lib.sh
          .github/scripts/ka-z.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for charge in \
  "8	Run first, then source" \
  "13	Source first, then run"; do
  IFS=$'\t' read -r line step <<<"$charge"
  check "charges '$step' both runs of ka-z.sh, 2100 s" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' carries .timeout-minutes: 1., 60 s, and its own retries take 2100 s to exhaust")"
  check "refuses '$step' by name for the library it reaches along two paths" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' reaches .github/scripts/ka-lib.sh, a library that fetches, along more than one path")"
done
for refusal in \
  ".github/scripts/ka-z.sh	5	.github/scripts/ka-lib.sh" \
  ".github/scripts/ka-lib.sh	2	.github/scripts/ka-z.sh"; do
  IFS=$'\t' read -r script line target <<<"$refusal"
  check "refuses $script, which reaches itself, at its call of $target" \
    "$(contains "$out" "::error file=$script,line=$line::$script runs or sources $target, and that call reaches $script again")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 6)"

# Where a heredoc's consumer is read, and which line its refusal names (dispatch://INFRA-1545,
# round 37). A body fed to `bash -e -` is the shell's script, since the `-` after the options is
# standard input spelled out: its run of kb-c.sh, 1050 s, is the step's own and is charged against
# the key of 1 minute, 60 s. A body fed to a shell given `-O` is refused by name where it fetches,
# and the shell is the LAST command in front of the operator, whatever runs before it on the line
# (`cd … && bash -O extglob <<'EOF'`), and a shell written by its path (`/bin/bash`) is the same
# shell. The refusal names the operator's own line, which here is the run's second. Round 37's
# mutation pass found each of these read another way and the step then passed, its run charged
# nothing: the `-` looked up at the wrong word, the first command of the line taken as the
# consumer, `/bin/bash` read as the directory, and the line counted back from the `run:` key.
echo "case: a heredoc's shell is read after bash's options, as the line's last command and by its path, and refused at its operator"
root=$(fixture kb-heredoc-consumers)
cat > "$root/.github/scripts/kb-c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/kb-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Feed it to bash -e -
        timeout-minutes: 1
        run: |
          bash -e - <<'EOF'
          .github/scripts/kb-c.sh
          EOF
      - name: Feed it to bash -O extglob after a cd
        timeout-minutes: 1
        run: |
          cd "$RUNNER_TEMP" && bash -O extglob <<'EOF'
          .github/scripts/kb-c.sh
          EOF
      - name: Feed it to /bin/bash -O extglob
        timeout-minutes: 1
        run: |
          /bin/bash -O extglob <<'EOF'
          .github/scripts/kb-c.sh
          EOF
      - name: Feed it to bash -O extglob below a line
        timeout-minutes: 1
        run: |
          echo start
          bash -O extglob <<'EOF'
          .github/scripts/kb-c.sh
          EOF
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses 'Feed it to bash -e -' by name, its body charged nothing" \
  "$(contains "$out" "step 'Feed it to bash -e -' of job 'build' feeds a heredoc to .bash., and the body holds a fetch")"
for refusal in \
  "16	Feed it to bash -O extglob after a cd" \
  "22	Feed it to /bin/bash -O extglob" \
  "29	Feed it to bash -O extglob below a line"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' by name at its operator, line $line" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' feeds a heredoc to .[a-z/]*., and the body holds a fetch or names a script that fetches")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# A function rule 8 does not read is refused where a fetch is written on the last line its reach
# holds, that line included (dispatch://INFRA-1545, round 37). Each step calls its function twice,
# and each call makes the one `retry_fetch` at 300 s, 5 x 300 + 150 = 1650 s, so the step waits
# 3300 s; read as commands of the step, the fetch is charged once, 1650 s, which the key of
# 28 minutes, 1680 s, holds, so the refusal is all that stands between the step and a pass. The
# first function is spelled with a blank before its `()`, whose reach runs to the end of the text,
# and it is written on the last line of a `|-` block, which ends with no newline. The second is
# defined inside another function's body, whose reach ends at that body's `}`, on the same line as
# the fetch. Round 37's mutation pass found the reach ending a line short and both steps passing.
echo "case: a function rule 8 does not read is refused for a fetch on the last line of its reach"
root=$(fixture kb-function-reach)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A blank function on the last line of a stripped block
        timeout-minutes: 28
        run: |-
          . .github/scripts/ci-retry.sh
          fetch () { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }; fetch; fetch
      - name: A nested function whose fetch is on the outer closing line
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          outer() {
            inner() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }; }
          outer
          inner
          inner
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "11	A blank function on the last line of a stripped block	with a blank before or inside its .()." \
  "17	A nested function whose fetch is on the outer closing line	inside another function's body"; do
  IFS=$'\t' read -r line step shape <<<"$refusal"
  check "refuses '$step' by name at its definition" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' defines a function $shape, a form this check does not read as a function, and a fetch, or a call of a script that fetches, is in its reach: written on its line or after it")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A shape rule 8 refuses by name is refused for a fetch under `timeout 0`, which is no bound, and
# not for a fetch that waits for nothing (dispatch://INFRA-1545, round 37). The gate at d766bd4b
# read `timeout 0` as a bound of 0 s, so a `curl --retry 7 --retry-all-errors --retry-max-time 1`
# under it was charged 8 x 0 + 1 = 1 s a call, and round 37's mutation pass found each shape
# asking whether a call costs 1 s rather than 0 s. `timeout 0` sets no limit, so the curl's
# attempts are unbounded and have no figure: fed to `bash -O extglob`, the body is data to every
# other rule, so the refusal by name is the only thing that stops the step; in the credentialed
# publish job the curl is exempted from the other rules (exception (d)), so a function with the
# `function` keyword and a loop in `bash -c` text are each refused by name, and the curl in each
# is refused as an excepted site making unbounded attempts of its own. A bare `gh api` in a
# `bash -c` loop of the same job waits for nothing, so it is refused by nobody: a shape asking
# whether a call costs other than 1 s refused it.
echo "case: a shape refused by name is refused for a fetch under timeout 0, and not for one that waits for nothing"
root=$(fixture kb-one-second-fetch)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Feed a one-second curl to bash -O extglob
        timeout-minutes: 1
        run: |
          bash -O extglob <<'EOF'
          timeout 0 curl -fsSL --retry 7 --retry-all-errors --retry-max-time 1 -o b https://example.com/b
          EOF
YAML
cat >> "$root/.github/workflows/release.yaml" <<'YAML'
      - name: Read it in a function with the keyword
        timeout-minutes: 1
        run: |
          function latest { timeout 0 curl -fsSL --retry 7 --retry-all-errors --retry-max-time 1 -o l "https://api.github.com/repos/$GITHUB_REPOSITORY/releases/latest"; }
          latest
          latest
      - name: Read it in a bash -c loop
        timeout-minutes: 1
        run: |
          bash -c 'for pass in 1 2; do timeout 0 curl -fsSL --retry 7 --retry-all-errors --retry-max-time 1 -o l "https://api.github.com/repos/$GITHUB_REPOSITORY/releases/latest"; done'
      - name: Read it bare in a bash -c loop
        run: |
          bash -c 'for pass in 1 2; do gh api "repos/$GITHUB_REPOSITORY/releases/latest"; done'
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the heredoc fed to bash -O extglob by name at its operator" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Feed a one-second curl to bash -O extglob' of job 'build' feeds a heredoc to .[a-z/]*., and the body holds a fetch or names a script that fetches")"
check "refuses the function with the keyword by name at its definition" \
  "$(contains "$out" "::error file=.github/workflows/release.yaml,line=23::step 'Read it in a function with the keyword' of job 'publish' defines a function with the .function. keyword, a form this check does not read as a function, and a fetch, or a call of a script that fetches, is in its reach: written on its line or after it")"
check "refuses the loop in bash -c text by name at its line" \
  "$(contains "$out" "::error file=.github/workflows/release.yaml,line=29::step 'Read it in a bash -c loop' of job 'publish' runs .[a-z -]*. text, whose text holds a loop or a function, and a fetch")"
for refusal in "23	Read it in a function with the keyword" "29	Read it in a bash -c loop"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses the curl of '$step' as an excepted site making unbounded attempts" \
    "$(contains "$out" "::error file=.github/workflows/release.yaml,line=$line::.curl. in step '$step' is a site an exception above leaves unretried, and it makes attempts of its own with no bound this check finds")"
done
check "refuses nothing at the bare read in a bash -c loop" \
  "$(is "$(contains "$out" "Read it bare in a bash -c loop")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"

# Five readings of a function that round 37's mutation pass found no case holding
# (dispatch://INFRA-1545, round 37). Each step fetches through one `retry_fetch` at 300 s,
# 5 x 300 + 150 = 1650 s, which runs twice, 3300 s, under a key of 28 minutes, 1680 s, so a
# reading that charges the fetch once passes the key and only a refusal holds the step:
#   - a function defined inside another over several lines is refused at its definition, and
#     its reach runs to the outer function's closing brace, not to its own line alone;
#   - a function whose body is an `if` is refused by name though its test ends in a `{` word,
#     which is an argument of `[` and opens no group;
#   - a quoted command word that ends in `()`, `'quiet()' fetchzz`, is a command bash cannot find
#     and defines nothing, and the next word of that command names no function either: the
#     `fetch` defined before it is still the one its two calls run;
#   - of two functions written on one line, the first holds the fetch and is the one called, and
#     the step is charged both calls; the second definition starts on the line the first body
#     ends on, which round 37 refuses by name too, since with the fetch in the second the line
#     went to the first and the fetch was charged once (dispatch://INFRA-1545, round 37);
#   - `bash -c` text defining a function with the `function` keyword, which writes no `name()`,
#     is refused as text holding a function.
echo "case: five readings of a function the gate refuses or charges for both calls"
root=$(fixture kc-function-readings)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A nested function over several lines
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          outer() {
            inner() {
              retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
            }
          }
          outer
          inner
          inner
      - name: An if body whose test ends in a brace
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch()
          if [ -n { ]; then retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; fi
          fetch
          fetch
      - name: A command word quoted to end in parentheses
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() {
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          }
          fetch
          fetch
          'quiet()' fetchzz || true
      - name: Two functions on one line, the fetch in the first
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }; quiet() { :; }
          fetch
          fetch
      - name: A function with the keyword in bash -c text
        timeout-minutes: 28
        run: |
          bash -c 'function fetch { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }; fetch; fetch'
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the nested function at its definition, for the fetch on the line below it" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'A nested function over several lines' of job 'build' defines a function inside another function's body, a form this check does not read as a function, and a fetch, or a call of a script that fetches, is in its reach: written on its line or after it")"
check "refuses the if body at its name though its test ends in a brace" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=23::step 'An if body whose test ends in a brace' of job 'build' defines a function with a body that is not a .{ … }. group")"
# The test's `{` is a word of `[`, which this check read as a brace, so it is refused by name too
# since round 37 (dispatch://INFRA-1545, round 37).
check "refuses the brace the test writes as a word, by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=24::step 'An if body whose test ends in a brace' of job 'build' writes .{. where bash reads it as a word of its command")"
for step in "A command word quoted to end in parentheses" "Two functions on one line, the fetch in the first"; do
  check "charges '$step' both calls of the function holding the fetch, 3300 s" \
    "$(contains "$out" "step '$step' carries .timeout-minutes: 28., 1680 s, and its own retries take 3300 s to exhaust")"
done
check "refuses the bash -c text defining a function with the keyword" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=47::step 'A function with the keyword in bash -c text' of job 'build' runs .[a-z -]*. text, whose text holds a loop or a function, and a fetch")"
check "refuses the second definition on the first one's closing line by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=41::step 'Two functions on one line, the fetch in the first' of job 'build' defines a function on the line another function's body ends on")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 7)"

# A library function that calls itself inside a loop of no pass is called from outside the
# library alone (dispatch://INFRA-1545, round 37): the loop around its own call, `for ((i = 0;
# i < 0; i++))`, runs no pass, so the step that sources the library and calls the function once
# waits for one `retry_fetch` at 300 s, 1650 s, which its key of 27 minutes, 1620 s, does not
# exceed. A call inside the function's own body is no call of it: counted as one, its no passes
# multiply the library's fetch by 0, and the step is charged nothing. Round 37 refuses both
# readings of the library by name as well, since each can hide passes where it is written
# otherwise: the function that calls itself, at its definition, and the loop of no pass, at its
# line (dispatch://INFRA-1545, round 37).
echo "case: a library function's call of itself is no call of it"
root=$(fixture kc-library-recursion)
cat > "$root/.github/scripts/kc-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
fetch_asset() {
  retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
  for ((i = 0; i < 0; i++)); do
    fetch_asset
  done
}
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source the library and fetch once
        timeout-minutes: 27
        run: |
          . .github/scripts/kc-lib.sh
          fetch_asset
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "charges the library's fetch once, 1650 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Source the library and fetch once' carries .timeout-minutes: 27., 1620 s, and its own retries take 1650 s to exhaust")"
for refusal in \
  "3	defines a function that calls itself, directly or through another function"; do
  IFS=$'\t' read -r line shape <<<"$refusal"
  check "refuses kc-lib.sh at line $line by name" \
    "$(contains "$out" "::error file=.github/scripts/kc-lib.sh,line=$line::.github/scripts/kc-lib.sh $shape")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A step written as a flow mapping holds its `run:` on the key's own line, so a refusal inside it
# names that line, even where the step's name before the key holds the text `run: >`
# (dispatch://INFRA-1545, round 37): only a `|` or a `>` with nothing after it but an indentation
# figure, a chomping sign and a comment opens a block scalar, whose text starts a line below. The
# step defines a function with a blank before its `()` and calls it, which rule 8 refuses by name
# at the definition.
echo "case: a refusal in a flow-mapping step names the key's line, whatever the name before it holds"
root=$(fixture kc-flow-step)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - {name: "A run: > in a name", timeout-minutes: 28, run: ". .github/scripts/ci-retry.sh; fetch () { retry_fetch \"the asset\" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }; fetch"}
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the definition on the step's own line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=7::step 'A run: > in a name' of job 'build' defines a function with a blank before or inside its .().")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A function holding a loop runs what it is handed in the first loop its body writes, which is
# the loop rule 2 and rule 8 name (dispatch://INFRA-1545, round 37): here the `for` at line 10
# makes three passes of the curl the call hands it, and the `while` after it runs `:` alone. The
# curl's `--max-time` is `"$T"`, so rule 8 names the variable bound, and the passes of the `for`
# are read; a reading taking the function's last loop named the `while` at line 11 as the loop
# the curl runs in, in both refusals.
echo "case: a function's words run in the first loop its body writes"
root=$(fixture kc-handed-loops)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Hand the curl to the first of two loops
        timeout-minutes: 28
        run: |
          each() {
            for pass in 1 2 3; do "$@"; done
            while read -r tidy; do :; done < tidy.txt
          }
          each curl -fsSL --retry 7 --retry-all-errors --max-time "$T" -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the for loop as the copy of the retry" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::this loop retries .curl. itself")"
check "names the call's bound as a variable" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Hand the curl to the first of two loops' carries .timeout-minutes: 28. and this check can put no figure on it: it runs a retried call whose attempts are bounded by a variable rather than by a number — .curl. at .github/workflows/case.yaml:14\.")"
check "refuses nothing at the while loop's line" "$(is "$(contains "$out" 'line=12::')" false)"
check "does not name the while loop as the loop the curl runs in" "$(is "$(contains "$out" 'loop at line 12')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A call of a function written after a reserved word or a wrapper on its line is a call of it
# (dispatch://INFRA-1545, round 37): `do fetch`, `then fetch`, `GH_HOST=x fetch`, `! fetch` and
# `time fetch` each run the function's one `retry_fetch` at 300 s, 5 x 300 + 150 = 1650 s, so
# each step below waits 3300 s and its key of 28 minutes, 1680 s, is under that. The gate at
# d766bd4b read a call only where the function's name was its command's first word, charged
# each step 1650 s and passed it. This tree writes such calls (`if fn`, `! fn`, `elif ! fn`), so
# they are read, with the wrappers reduce_words strips from any command.
echo "case: a call of a function after a word this tree writes no call after is refused by name"
root=$(fixture u37-calls-after-word)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A call after do in a one-line loop
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          for pass in 1 2; do fetch; done
      - name: A call after then, and one more
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          if true; then fetch; fi; fetch
      - name: A call behind an assignment and one after a bang
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          GH_HOST=example.com fetch
          ! fetch
      - name: A call after time and one in a brace group
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          time fetch
          { fetch; }
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
# This tree calls a function after no word, `if`, `if !`, `!` or `elif !` alone, so `do f` and
# `then f` on one line, an assignment prefix and `time` are refused by name where the function
# fetches, and the calls read, `! fetch` and `{ fetch; }`, are charged (dispatch://INFRA-1545,
# round 37).
for refusal in "12	A call after do in a one-line loop	do" \
  "18	A call after then, and one more	then" \
  "24	A call behind an assignment and one after a bang	GH_HOST=example.com" \
  "31	A call after time and one in a brace group	time"; do
  IFS=$'\t' read -r line step prefix <<<"$refusal"
  check "refuses the call in '$step' after $prefix by name" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' calls .fetch. after .$prefix., a form this check does not read as a call of a function, and .fetch. fetches")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# Four readings of a function that hide a call of its fetch are refused by name, none of them
# written in this tree (dispatch://INFRA-1545, round 37). Each step's fetch is one `retry_fetch`
# at 300 s, 1650 s, which runs twice, 3300 s, and each is charged 1650 s, under its key of 28
# minutes, 1680 s, so the refusal is what holds it; the gate at d766bd4b passed all four. A name
# defined again after its calls kept the second body only, so the first body's two calls were
# charged once; a definition on the line another function's body ends on gave its line, and so
# its fetch, to that other function; a function calling itself was charged for its outside
# call alone; and a function this check does not read, here one with the `function` keyword,
# was refused for a fetch from its own line on, which missed the fetch in the function it
# calls, written above it.
echo "case: a function defined again, defined on a closing line, calling itself or called from a function this check does not read is refused by name"
root=$(fixture u37-function-readings)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A function defined again after its calls
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fetch
          fetch
          fetch() { :; }
          fetch
      - name: A definition on the line another body ends on
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          quiet() { :; }; fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fetch
          fetch
      - name: A function that calls itself
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; [ -e done ] || { touch done; fetch; }; }
          fetch
      - name: A function with the keyword calling one written above it
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          get() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          function twice { get; }
          twice
          twice
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "11	A function defined again after its calls	that the same text defines again under the same name.*Give each definition a name of its own" \
  "20	A definition on the line another body ends on	on the line another function's body ends on.*Start each definition on a line of its own" \
  "27	A function that calls itself	that calls itself, directly or through another function.*Write the repetition as a loop" \
  "34	A function with the keyword calling one written above it	with the .function. keyword"; do
  IFS=$'\t' read -r line step shape <<<"$refusal"
  check "refuses '$step' by name" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' defines a function $shape")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# Three loops this check reads by line are refused by name where the text fetches, none of them
# written in this tree (dispatch://INFRA-1545, round 37). Two loops on one line: the second's
# three passes priced the first's five, 3 x 1650 = 4950 s where the step waits 8250 s, under a
# key of 120 minutes, 7200 s. A loop after `then`: read as no loop, its two passes were charged
# once, 1650 s where the step waits 3300 s, under 28 minutes. A loop of no pass beside a fetch on
# its line: the fetch, which runs once, was multiplied by those no passes and charged nothing.
# The gate at d766bd4b passed all three.
echo "case: two loops on one line and a loop after a reserved word are refused by name, and a loop of no pass beside a fetch charges it once"
root=$(fixture u37-loop-readings)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Two loops on one line
        timeout-minutes: 120
        run: |
          . .github/scripts/ci-retry.sh
          for a in 1 2 3 4 5; do retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; done; for b in 1 2 3; do :; done
      - name: A loop after then
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          if true; then for a in 1 2; do retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; done; fi
      - name: A loop of no pass beside a fetch
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          for ((i = 0; i < 0; i++)); do :; done; retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "11	Two loops on one line	two loops on one line.*Write each loop on lines of its own" \
  "16	A loop after then	a loop after a reserved word on its line.*Open the loop on a line of its own"; do
  IFS=$'\t' read -r line step shape <<<"$refusal"
  check "refuses '$step' by name" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' writes $shape")"
done
# A loop of no pass beside a fetch is a loop whose `done` the fetch follows, so the fetch runs
# once, as charged, and the loop holds nothing priced (dispatch://INFRA-1545, round 37).
check "charges the fetch beside a loop of no pass once, and refuses nothing there" \
  "$(is "$(contains "$out" "step 'A loop of no pass beside a fetch'")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# An array whose words this check cannot count makes the loop over its length a loop whose
# passes it cannot read, which is refused (dispatch://INFRA-1545, round 37). `($names)` splits
# into as many words as the run finds, `lists[2]=c` adds a third word to two, and `mapfile` fills
# an array written empty. The gate at d766bd4b counted one, two and no passes, so each step's
# fetch at 1650 s a pass was charged 1650 s, 3300 s and nothing, each passing its key.
echo "case: a for (( )) over an array is refused for its passes, whatever the array holds"
root=$(fixture u37-array-counts)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: An array of an unquoted expansion
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          lists=($names)
          for ((i = 0; i < ${#lists[@]}; i++)); do
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          done
      - name: An array given an element after its words
        timeout-minutes: 56
        run: |
          . .github/scripts/ci-retry.sh
          lists=(a b)
          lists[2]=c
          for ((i = 0; i < ${#lists[@]}; i++)); do
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          done
      - name: An array mapfile fills
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          lists=()
          mapfile -t lists < lists.txt
          for ((i = 0; i < ${#lists[@]}; i++)); do
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "8	An array of an unquoted expansion	13	12" \
  "16	An array given an element after its words	22	21" \
  "25	An array mapfile fills	31	30"; do
  IFS=$'\t' read -r line step site loop <<<"$refusal"
  check "refuses '$step' for the loop's unread passes" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' carries .timeout-minutes: [0-9]*. and this check can put no figure on it: it runs a retried call inside a loop whose passes it cannot read — .curl. at .github/workflows/case.yaml:$site, inside the loop at line $loop\.")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# A `timeout` whose duration this check cannot read as seconds above 0, and a curl `--max-time
# 0`, bound nothing, and the fetch behind an option of `timeout` stays a fetch
# (dispatch://INFRA-1545, round 37). The gate at d766bd4b read `timeout 1.5`'s duration, `-k`'s
# value and `-s`'s signal as the command, so each bare curl behind them was no fetch at all and
# passed; read `--max-time 0` as a bound written as a variable; and read `timeout 0` as a bound
# of 0 s, so a curl in a credentialed publish job, which no other rule reads, retrying itself
# under it was charged 1 s and passed a key of 1 minute. The same job's bare read in a function
# with the `function` keyword waits for nothing, and is refused by nobody. The curls behind `-k`
# and `-s` are bounded at 60 s and read at curl's floor, 8 x 60 + 127 = 607 s, which their keys
# of 11 minutes hold, so rule 1 is all that refuses them.
echo "case: a timeout or a max-time this check cannot read as seconds above 0 bounds nothing, and hides no fetch"
root=$(fixture u37-timeout-bounds)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A curl under timeout 1.5
        run: timeout 1.5 curl -fsSL -o a https://example.com/a
      - name: A curl under timeout -k 5 60
        timeout-minutes: 11
        run: timeout -k 5 60 curl -fsSL -o a https://example.com/a
      - name: A curl under timeout -s KILL 60
        timeout-minutes: 11
        run: timeout -s KILL 60 curl -fsSL -o a https://example.com/a
      - name: A curl whose max-time is 0
        timeout-minutes: 28
        run: curl -fsSL --retry 7 --retry-all-errors --max-time 0 -o a https://example.com/a
YAML
cat >> "$root/.github/workflows/release.yaml" <<'YAML'
      - name: Read it under timeout 0
        timeout-minutes: 1
        run: timeout 0 curl -fsSL --retry 7 --retry-all-errors --retry-max-time 1 -o l "https://api.github.com/repos/$GITHUB_REPOSITORY/releases/latest"
      - name: Read it bare in a function with the keyword
        run: |
          function latest { gh api "repos/$GITHUB_REPOSITORY/releases/latest"; }
          latest
          latest
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for line in 8 11 14; do
  check "reads the curl at line $line as a fetch, and refuses it for the floor" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::.curl. in step .* fetches without the floor: it lacks .--retry 7.")"
done
check "refuses the curl whose max-time is 0 for a bound above 0" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=17::.curl. in step 'A curl whose max-time is 0' fetches without the floor: it lacks .--max-time <seconds>., which curl resets for each attempt, above 0, since .--max-time 0. sets no limit")"
check "refuses the curl under timeout 0 in the publish job as unbounded" \
  "$(contains "$out" "::error file=.github/workflows/release.yaml,line=22::.curl. in step 'Read it under timeout 0' is a site an exception above leaves unretried, and it makes attempts of its own with no bound this check finds")"
check "refuses nothing at the bare read in a function with the keyword" \
  "$(is "$(contains "$out" 'line=2[3-7]::')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"

# A heredoc fed to a shell given `-O`, `+O`, `--rcfile` or `--init-file` is refused by name for
# any fetch in its body, a bare one or a retried `mise x` included (dispatch://INFRA-1545, round
# 37): no other rule reads that body, so the rules that refuse a bare `wget`, a bare `docker
# pull` and a retried `mise x` written in a step see none of them there. The gate at d766bd4b
# refused such a body only where one call of a fetch in it cost more than nothing, and passed
# all three; the body holding a curl under `timeout 0`, which is no bound, is refused as well.
echo "case: a heredoc fed to a shell given an option rule 8 does not read is refused for any fetch in its body"
root=$(fixture u37-heredoc-any-fetch)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A bare wget fed to bash -O extglob
        run: |
          bash -O extglob <<'EOF'
          wget -q -O a https://example.com/a
          EOF
      - name: A bare docker pull fed to bash +O extglob
        run: |
          bash +O extglob <<'EOF'
          docker pull example.com/image:1
          EOF
      - name: A retried mise x fed to bash --rcfile
        run: |
          bash --rcfile /dev/null <<'EOF'
          . .github/scripts/ci-retry.sh
          retry_fetch "the tool" 60 -- mise x node@22 -- node --version
          EOF
      - name: A curl under timeout 0 fed to bash -O extglob
        timeout-minutes: 1
        run: |
          bash -O extglob <<'EOF'
          timeout 0 curl -fsSL --retry 7 --retry-all-errors --retry-max-time 1 -o b https://example.com/b
          EOF
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "9	A bare wget fed to bash -O extglob" \
  "14	A bare docker pull fed to bash +O extglob" \
  "19	A retried mise x fed to bash --rcfile" \
  "26	A curl under timeout 0 fed to bash -O extglob"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' by name at its operator" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' feeds a heredoc to .[a-z/]*., and the body holds a fetch or names a script that fetches")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# A run on the line after a backslash continuation is read against the loops of the line its
# command starts on (dispatch://INFRA-1545, round 37). u37-c.sh retries at 180 s, 1050 s. After
# `echo start \`, and after a double-quoted string the backslash continues, a three-pass loop
# opens on the next line and runs the script three times, 3150 s, over a key of 30 minutes,
# 1800 s; a `while read` opening there has no pass count. The gate at d766bd4b keyed each run
# to the line its logical line starts on, the line before the loop: it charged the first two
# 1050 s and refused nothing in the third, passing all three.
echo "case: a run behind a do after a continued line is refused by name at its line"
root=$(fixture u37-continued-lines)
cat > "$root/.github/scripts/u37-c.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/u37-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Bare continuation before a loop
        timeout-minutes: 30
        run: |
          echo start \
          && for pass in 1 2 3; do .github/scripts/u37-c.sh; done
      - name: Quoted continuation before a loop
        timeout-minutes: 30
        run: |
          echo "start\
          "; for pass in 1 2 3; do .github/scripts/u37-c.sh; done
      - name: Bare continuation before an uncounted loop
        timeout-minutes: 30
        run: |
          echo start \
          && while read -r id; do .github/scripts/u37-c.sh "$id"; done <<<"$ids"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
# A run behind `do` on its line, continued onto it or not, is no run this tree writes, so each is
# refused by name at the path's line and charged nothing (dispatch://INFRA-1545, round 37).
for refusal in "11	Bare continuation before a loop" "16	Quoted continuation before a loop" \
  "21	Bare continuation before an uncounted loop"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses the run behind do in '$step' by name at its line" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' names .github/scripts/u37-c.sh in a command behind .do., a place this check does not read as a call")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# A script that reaches itself and fetches is refused by name at the call that leads back
# (dispatch://INFRA-1545, round 37): a path stops at a script it is already inside, so the run
# the shell makes on entering it again was charged nothing. u37-self.sh retries at 180 s,
# 1050 s, and runs itself once more behind a guard, 2100 s; u37-a.sh retries the same and runs
# u37-b.sh, which runs u37-a.sh once a job. The gate at d766bd4b charged each step 1050 s,
# under its key of 30 minutes, and refused nothing.
echo "case: a script that reaches itself and fetches is refused by name"
root=$(fixture u37-self-reach)
cat > "$root/.github/scripts/u37-self.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
[ -n "${U37_INNER:-}" ] || U37_INNER=1 .github/scripts/u37-self.sh
SH
cat > "$root/.github/scripts/u37-a.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
.github/scripts/u37-b.sh
SH
cat > "$root/.github/scripts/u37-b.sh" <<'SH'
#!/usr/bin/env bash
[ -e "$RUNNER_TEMP/u37.done" ] || { touch "$RUNNER_TEMP/u37.done"; .github/scripts/u37-a.sh; }
SH
chmod +x "$root/.github/scripts/u37-self.sh" "$root/.github/scripts/u37-a.sh" \
  "$root/.github/scripts/u37-b.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the script that runs itself
        timeout-minutes: 30
        run: .github/scripts/u37-self.sh
      - name: Run the script that another runs again
        timeout-minutes: 30
        run: .github/scripts/u37-a.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  ".github/scripts/u37-self.sh	5	.github/scripts/u37-self.sh" \
  ".github/scripts/u37-a.sh	5	.github/scripts/u37-b.sh" \
  ".github/scripts/u37-b.sh	2	.github/scripts/u37-a.sh"; do
  IFS=$'\t' read -r script line target <<<"$refusal"
  check "refuses $script at its call of $target" \
    "$(contains "$out" "::error file=$script,line=$line::$script runs or sources $target, and that call reaches $script again")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# A call of a function through a variable, in a text that defines a function that fetches, is
# refused by name unless declared, and a declaration's figure enters the step's arithmetic
# (dispatch://INFRA-1545, round 37). The step calls `fetch`, one `retry_fetch` at 300 s,
# 5 x 300 + 150 = 1650 s, twice through `"$fn"`, 3300 s, which its key of 55 minutes, 3300 s,
# does not exceed; the site is charged once as written, 1650 s, and the gate at d766bd4b passed
# the step. A DECLARED_CALLS entry writing the second call's 1650 s puts the step at 3300 s,
# which that key is refused for and a key of 56 minutes passes; an entry matching no call fails.
echo "case: a call of a function through a variable is refused unless declared, and its declaration is charged"
root=$(fixture u37-variable-call)
write_variable_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through a variable
        timeout-minutes: $1
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=fetch
          "\$fn"
          "\$fn"
YAML
}
write_variable_case 55
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call through the variable by name, naming the fetch it can reach" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'Fetch through a variable' of job 'build' calls a function through a variable, .\\\$fn., and a function this text defines or sources fetches — .curl. at .github/workflows/case.yaml:11\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
declare_in "$root" DECLARED_CALLS '    DeclaredCall(".github/workflows/case.yaml", "$fn", 1650, "the second of two calls of fetch, the first being the site this check counts: 1 x (5 x 300 + 150) = 1650 s"),'
run_check "$root"
check "with a workflow's call declared, still refuses the call" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'Fetch through a variable' of job 'build' calls a function through a variable")"
check "refuses the declaration for naming a workflow" \
  "$(contains "$out" "::error file=.github/scripts/check-download-retry.sh,line=1::the declared call figure for .\\\$fn. in .github/workflows/case.yaml names a workflow or a composite action, and a declared call is read for a script's own calls alone")"
check "with the call declared, refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A library a step reaches along more than one path is refused by name unless declared
# (dispatch://INFRA-1545, round 37). u37-lib.sh defines u37_fetch, one `retry_fetch` at 300 s,
# 1650 s, and the step sources it and calls the function, and runs u37-run.sh, which sources it
# and calls the function again: 3300 s, which a key of 55 minutes does not exceed. The library
# is charged once, at its most expensive path, 1650 s, and the gate at d766bd4b passed the step.
# A DECLARED_LIBRARIES entry writing 3300 s replaces that charge, so the key of 55 minutes is
# refused and one of 56 passes; an entry matching no step fails.
echo "case: a library a step reaches along more than one path is refused unless declared, and its declaration is charged"
root=$(fixture u37-shared-library)
cat > "$root/.github/scripts/u37-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
u37_fetch() {
  retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
}
SH
cat > "$root/.github/scripts/u37-run.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/u37-lib.sh
u37_fetch
SH
chmod +x "$root/.github/scripts/u37-run.sh"
write_shared_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source the library and run a script that sources it too
        timeout-minutes: $1
        run: |
          . .github/scripts/u37-lib.sh
          u37_fetch
          .github/scripts/u37-run.sh
YAML
}
write_shared_case 55
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the step by name for the library it reaches twice" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Source the library and run a script that sources it too' reaches .github/scripts/u37-lib.sh, a library that fetches, along more than one path")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
library_entry='    DeclaredLibrary(".github/workflows/case.yaml", "Source the library and run a script that sources it too", ".github/scripts/u37-lib.sh", 3300, "one call of u37_fetch from the step and one from u37-run.sh: 2 x (5 x 300 + 150) = 3300 s"),'
declare_in "$root" DECLARED_LIBRARIES "$library_entry"
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/workflows/case.yaml", "No such step", ".github/scripts/u37-lib.sh", 1650, "a step this fixture does not write"),'
run_check "$root"
check "with the library declared, charges the step the declared 3300 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Source the library and run a script that sources it too' carries .timeout-minutes: 55., 3300 s, and its own retries take 3300 s to exhaust")"
check "refuses the declaration that matches no step" \
  "$(contains "$out" "::error file=.github/scripts/check-download-retry.sh,line=1::the declared library figure for .github/workflows/case.yaml step 'No such step' .github/scripts/u37-lib.sh matches no step")"
check "with the library declared, refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"
ln -sf "$check_script" "$root/.github/scripts/check-download-retry.sh"
declare_in "$root" DECLARED_LIBRARIES "$library_entry"
write_shared_case 56
run_check "$root"
check "with the library declared and a key above 3300 s, passes" "$(is "$status" 0)"

# A library's fetching function called more than once after sourcing it is refused by name
# unless declared, for the step that calls it or for the script whose calls they are
# (dispatch://INFRA-1545, round 37). The step calls u37_fetch in a two-pass loop, and
# u37-twice.sh, which a second step runs, calls it twice: each waits 3300 s under a key of 55
# minutes, and each is charged the library once, 1650 s, which the gate at d766bd4b passed. A
# DECLARED_LIBRARIES entry for the step, and one for the script with no step, each writing
# 3300 s, replace that charge, so each key of 55 minutes is refused and keys of 56 pass.
echo "case: a library's fetching function called more than once is refused unless declared, for the step or for the script"
root=$(fixture u37-library-calls)
cp "$work/u37-shared-library/.github/scripts/u37-lib.sh" "$root/.github/scripts/"
cat > "$root/.github/scripts/u37-twice.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/u37-lib.sh
u37_fetch
u37_fetch
SH
chmod +x "$root/.github/scripts/u37-twice.sh"
write_calls_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch twice through the library
        timeout-minutes: $1
        run: |
          . .github/scripts/u37-lib.sh
          for pass in 1 2; do
            u37_fetch
          done
      - name: Run the script that fetches twice
        timeout-minutes: $1
        run: .github/scripts/u37-twice.sh
YAML
}
write_calls_case 55
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the step's loop of calls by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Fetch twice through the library' of job 'build' calls .u37_fetch., which .github/scripts/u37-lib.sh defines and which fetch, more than once")"
check "refuses the script's two calls by name, naming the step that runs it" \
  "$(contains "$out" "::error file=.github/scripts/u37-twice.sh,line=4::.github/scripts/u37-twice.sh calls .u37_fetch., which .github/scripts/u37-lib.sh defines and which fetch, more than once.*step 'Run the script that fetches twice' of .github/workflows/case.yaml reaches it")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/workflows/case.yaml", "Fetch twice through the library", ".github/scripts/u37-lib.sh", 3300, "two passes of one call of u37_fetch: 2 x (5 x 300 + 150) = 3300 s"),'
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/scripts/u37-twice.sh", None, ".github/scripts/u37-lib.sh", 3300, "two calls of u37_fetch: 2 x (5 x 300 + 150) = 3300 s"),'
run_check "$root"
for step in "Fetch twice through the library" "Run the script that fetches twice"; do
  check "with the calls declared, charges '$step' the declared 3300 s" \
    "$(contains "$out" "step '$step' carries .timeout-minutes: 55., 3300 s, and its own retries take 3300 s to exhaust")"
done
check "with the calls declared, refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"
write_calls_case 56
run_check "$root"
check "with the calls declared and keys above 3300 s, passes" "$(is "$status" 0)"

# A heredoc whose shell the command in front of its operator does not name as this check reads
# one is refused by name where its body fetches, and a `-c` after `bash -o pipefail` is read as
# the text it runs (dispatch://INFRA-1545, round 37). u37-hc.sh retries at 180 s, 1050 s, and
# each step runs it, or the same retried curl, under a key of 1 minute. The gate at d766bd4b read
# each body as data and the `-c` text as no text, so it charged every step nothing and passed
# all six: a shell named by a variable, `ssh` and `/usr/bin/env` naming the shell among their
# words, a `cat` piped into `bash`, an unquoted body whose command substitution the step's own
# shell runs, and the `-c` text written after an option's value.
echo "case: a heredoc fed to a consumer this tree writes none to is refused for a fetch in its body, and -c text after -o is read and refused for its form"
root=$(fixture u37-heredoc-consumers)
cat > "$root/.github/scripts/u37-hc.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/u37-hc.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Feed it to a shell named by a variable
        timeout-minutes: 1
        run: |
          "$SHELL" <<'EOF'
          .github/scripts/u37-hc.sh
          EOF
      - name: Feed it to the shell ssh runs
        timeout-minutes: 1
        run: |
          ssh build-host bash <<'EOF'
          .github/scripts/u37-hc.sh
          EOF
      - name: Feed it to bash through env by its path
        timeout-minutes: 1
        run: |
          /usr/bin/env bash <<'EOF'
          .github/scripts/u37-hc.sh
          EOF
      - name: Pipe a printed copy into bash
        timeout-minutes: 1
        run: |
          cat <<'EOF' | bash
          .github/scripts/u37-hc.sh
          EOF
      - name: Substitute a run in an unquoted body
        timeout-minutes: 1
        run: |
          cat > out.txt <<EOF
          $(.github/scripts/u37-hc.sh)
          EOF
      - name: Run -c text after an option's value
        timeout-minutes: 1
        run: |
          . .github/scripts/ci-retry.sh
          bash -o pipefail -c 'retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b'
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
# Round 42 reads a wrapper by its name whatever directory names it, so `/usr/bin/env bash` feeds
# the heredoc to `bash`, the consumer the refusal at line 22 names (dispatch://INFRA-1545, round
# 42).
for refusal in \
  "10	Feed it to a shell named by a variable	.\\\$SHELL." \
  "16	Feed it to the shell ssh runs	.ssh." \
  "22	Feed it to bash through env by its path	.bash." \
  "28	Pipe a printed copy into bash	.cat., whose output a pipe on the operator's line hands to a shell" \
  "34	Substitute a run in an unquoted body	.cat. unquoted, and the body holds a command substitution, which the step's own shell runs before that command reads the body"; do
  IFS=$'\t' read -r line step consumer <<<"$refusal"
  check "refuses '$step' by name at its operator" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' feeds a heredoc to $consumer, and the body holds a fetch or names a script that fetches")"
done
check "charges the -c text after -o pipefail its retried curl, 1050 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=38::step 'Run -c text after an option's value' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
check "refuses the -c text after -o pipefail by name for its form" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=41::step 'Run -c text after an option's value' of job 'build' runs .bash -o pipefail -c. text, a form other than .bash -c., and a fetch is written in it")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 7)"

# A curl that writes its per-attempt bound twice is refused by name (dispatch://INFRA-1545, round
# 37): curl takes the last value, measured on curl 8.14.1 (`--max-time 1 --max-time 3` stopped
# after 3 s), and this tree writes one. Its eight attempts at 300 s and curl's own 127 s of waits
# are 2527 s, which no key of 7 minutes holds; the gate at d65e47ae read the first value, 8 x 30 +
# 127 = 367 s, and passed it under 7 minutes, 420 s.
echo "case: a curl writing its bound twice is refused by name"
root=$(fixture a37-max-time-twice)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Bound twice
        timeout-minutes: 7
        run: curl -fsSL --retry 7 --retry-all-errors --max-time 30 --max-time 300 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the curl for the bound it writes twice" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::.curl. in step 'Bound twice' writes its per-attempt bound 2 times, .30. and .300., and curl takes the last one")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# `-c` text after a shell option that takes a value, `-O <name>`, `+O <name>`, `--rcfile <file>`
# or `--init-file <file>`, is the text the shell runs, measured: `bash -O extglob -c 'echo ran'`
# prints `ran`. It is read for its sites, and refused by name as a form other than `bash -c`
# (dispatch://INFRA-1545, round 37). Each curl makes its own attempts at 120 s, 8 x 120 + 127 =
# 1087 s, under a key of 19 minutes, 1140 s, so the form is what each step is refused for. The
# gate at d65e47ae stopped at the option's value, read no text and no site, and passed all four,
# and so would have passed them under any key.
echo "case: -c text after a shell option taking a value is read and refused by name"
root=$(fixture a37-shell-options)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: After -O
        timeout-minutes: 19
        run: bash -O extglob -c 'curl -fsSL --retry 7 --retry-all-errors --max-time 120 -o b https://example.com/b'
      - name: After +O
        timeout-minutes: 19
        run: bash +O extglob -c 'curl -fsSL --retry 7 --retry-all-errors --max-time 120 -o b https://example.com/b'
      - name: After --rcfile
        timeout-minutes: 19
        run: bash --rcfile /dev/null -c 'curl -fsSL --retry 7 --retry-all-errors --max-time 120 -o b https://example.com/b'
      - name: After --init-file
        timeout-minutes: 19
        run: bash --init-file /dev/null -c 'curl -fsSL --retry 7 --retry-all-errors --max-time 120 -o b https://example.com/b'
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "9	After -O	bash -O extglob -c" "12	After +O	bash +O extglob -c" \
  "15	After --rcfile	bash --rcfile /dev/null -c" \
  "18	After --init-file	bash --init-file /dev/null -c"; do
  IFS=$'\t' read -r line step form <<<"$refusal"
  check "refuses '$step' by name for the form of its text" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' runs .$form. text, a form other than .bash -c., and a fetch is written in it")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# A `for ((…))` is a loop whose passes this check does not read, whatever its bound, and is
# refused unless a declaration writes its seconds (dispatch://INFRA-1545, round 37). Over an array
# `read -ra` fills, three words, or over `("${@}")` of a script run with two arguments, each loop
# runs a `retry_fetch` at 300 s, 1650 s, three and two times, which no key of 28 minutes holds.
# The gate at d65e47ae counted each array as one word, since its `read` pattern wanted `-a` as a
# word of its own and its `$@` test did not match `${@}`, charged each step 1650 s and passed it.
echo "case: a for (( )) over an array read -ra fills or over \"\${@}\" is refused for its passes"
root=$(fixture a37-arrays)
cat > "$root/.github/scripts/a37-braced.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
lists=("${@}")
for ((i = 0; i < ${#lists[@]}; i++)); do
  retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
done
SH
chmod +x "$root/.github/scripts/a37-braced.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: An array read -ra fills
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          lists=(a)
          read -ra lists <<<"a b c"
          for ((i = 0; i < ${#lists[@]}; i++)); do
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          done
      - name: An array of the braced arguments
        timeout-minutes: 28
        run: .github/scripts/a37-braced.sh a b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "8	An array read -ra fills	.github/workflows/case.yaml:14	13" \
  "17	An array of the braced arguments	.github/scripts/a37-braced.sh:6	5"; do
  IFS=$'\t' read -r line step site loop <<<"$refusal"
  check "refuses '$step' for the loop's unread passes" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' carries .timeout-minutes: 28. and this check can put no figure on it: it runs a retried call inside a loop whose passes it cannot read — .curl. at $site, inside the loop at line $loop\.")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A library's function that calls a function through a variable runs whatever its caller hands
# it, and the library's own reading cannot see what that is: a call of it from a text that can
# see a function that fetches is refused by name (dispatch://INFRA-1545, round 37). a37-run.sh
# defines `a37_run() { "$1"; }` and fetches nothing; the step defines `fetch`, one `retry_fetch`
# at 300 s, 1650 s, and runs it twice through `a37_run fetch`, 3300 s, under a key of 28 minutes.
# The gate at d65e47ae found no fetching function in the library and no call through a variable
# in the step, charged the site once, 1650 s, and passed it.
echo "case: a call of a library function that calls through a variable is refused by name"
root=$(fixture a37-library-runs-argument)
cat > "$root/.github/scripts/a37-run.sh" <<'SH'
#!/usr/bin/env bash
a37_run() { "$1"; }
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a function through the library
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          . .github/scripts/a37-run.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          a37_run fetch
          a37_run fetch
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for line in 13 14; do
  check "refuses the call at line $line by name, naming the fetch it can run" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step 'Run a function through the library' of job 'build' calls .a37_run., which .github/scripts/a37-run.sh defines and which calls a function through a variable, .\\\$1., and a function this text defines or sources fetches — .curl. at .github/workflows/case.yaml:12\.")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A name two libraries define, or the text and a library, is refused by name where either
# definition fetches, since bash runs the one it read last and this check reads one name as one
# function (dispatch://INFRA-1545, round 37). a37-dup.sh sources a37-d1.sh and then a37-d2.sh,
# which both define `a37_dup`, at 60 s and at 300 s; the first step sources a37-dup.sh and calls
# a37_dup twice, 2 x 1650 = 3300 s, and declares the calls at a37-d1.sh's 900 s, which the gate at
# d65e47ae took, charging 900 + 1650 = 2550 s under a key of 43 minutes, 2580 s, and passing it.
# The second step defines `a37_get`, which fetches nothing, and then sources a37-d3.sh, which
# defines it again at 300 s: bash runs a37-d3.sh's, twice, 3300 s, and that gate kept the text's
# own, charged the library once, 1650 s, and passed a key of 28 minutes.
echo "case: a name two libraries define, or the text and a library, is refused by name"
root=$(fixture a37-two-definitions)
for library in "a37-d1.sh a37_dup 60" "a37-d2.sh a37_dup 300" "a37-d3.sh a37_get 300"; do
  read -r file function bound <<<"$library"
  cat > "$root/.github/scripts/$file" <<SH
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
$function() { retry_fetch "the asset" $bound -- curl -fsSL --max-time $bound -o a https://example.com/a; }
SH
done
cat > "$root/.github/scripts/a37-dup.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/a37-d1.sh
. .github/scripts/a37-d2.sh
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call a name two libraries define
        timeout-minutes: 43
        run: |
          . .github/scripts/a37-dup.sh
          a37_dup
          a37_dup
      - name: Define a name a library defines again
        timeout-minutes: 28
        run: |
          a37_get() { echo none; }
          . .github/scripts/a37-d3.sh
          a37_get
          a37_get
YAML
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/workflows/case.yaml", "Call a name two libraries define", ".github/scripts/a37-d1.sh", 900, "two calls of a37_dup at 60 s: 2 x (5 x 60 + 150) = 900 s"),'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses a37-dup.sh for the name its two libraries define" \
  "$(contains "$out" "::error file=.github/scripts/a37-dup.sh,line=3::.github/scripts/a37-dup.sh reaches two definitions of .a37_dup., in .github/scripts/a37-d1.sh and .github/scripts/a37-d2.sh, and one of them fetches")"
check "refuses the second step for the name it and a library define" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=17::step 'Define a name a library defines again' of job 'build' reaches two definitions of .a37_get., in this text and .github/scripts/a37-d3.sh, and one of them fetches")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A DECLARED_CALLS entry names a script, the one body this tree declares a call through a
# variable in, and an entry naming a workflow is refused by name (dispatch://INFRA-1545, round
# 37). Jobs one and two each hold a step 'Fetch by name and through a variable', each calling
# `fetch`, 1650 s, by name and through `"$fn"`, 3300 s. One entry written for that step name
# was charged by the gate at d65e47ae to the first step alone, so job two's key of 28 minutes
# passed at 1650 s. Here the entry matches neither step: each call through the variable is
# refused, and the entry is refused for the body it names.
echo "case: a declared call naming a workflow step is refused, and the calls it named are refused"
root=$(fixture a37-declared-call-steps)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  one:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch by name and through a variable
        timeout-minutes: 56
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=fetch
          fetch
          "$fn"
  two:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch by name and through a variable
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=fetch
          fetch
          "$fn"
YAML
declare_in "$root" DECLARED_CALLS '    DeclaredCall(".github/workflows/case.yaml", "$fn", 1650, "the one call of fetch through the variable: 1 x (5 x 300 + 150) = 1650 s"),'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "14	one	11" "25	two	22"; do
  IFS=$'\t' read -r line job site <<<"$refusal"
  check "refuses job $job's call through the variable" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step 'Fetch by name and through a variable' of job '$job' calls a function through a variable, .\\\$fn., and a function this text defines or sources fetches — .curl. at .github/workflows/case.yaml:$site\.")"
done
check "refuses the entry for naming a workflow" \
  "$(contains "$out" "::error file=.github/scripts/check-download-retry.sh,line=1::the declared call figure for .\\\$fn. in .github/workflows/case.yaml names a workflow or a composite action, and a declared call is read for a script's own calls alone")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# A DECLARED_LIBRARIES entry keys a composite action by its file alone, so two steps of one action
# cannot take two entries for one library (dispatch://INFRA-1545, round 37). The action's steps
# call a37_fetch, 1650 s, three times and twice, 8250 s, under the using step's key of 120
# minutes, 7200 s. Entries keyed by the action's two step names, 4950 s and 3300 s, were taken by
# the gate at d65e47ae with the first one matched priced in both steps, 2 x 3300 = 6600 s, and
# the key passed. Here each entry is refused for its key, and each step for its calls.
echo "case: a declared library keyed by a composite action's step is refused for its key"
root=$(fixture a37-action-step-entries)
cat > "$root/.github/scripts/a37-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
a37_fetch() {
  retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
}
SH
mkdir -p "$root/.github/actions/a37-act"
cat > "$root/.github/actions/a37-act/action.yml" <<'YAML'
name: Fetch from two steps
description: Calls a37_fetch twice in one step and three times in the next.
runs:
  using: composite
  steps:
    - name: Twice
      shell: bash
      run: |
        . .github/scripts/a37-lib.sh
        a37_fetch
        a37_fetch
    - name: Thrice
      shell: bash
      run: |
        . .github/scripts/a37-lib.sh
        a37_fetch
        a37_fetch
        a37_fetch
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - name: Use the action
        timeout-minutes: 120
        uses: ./.github/actions/a37-act
YAML
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/actions/a37-act/action.yml", "Twice", ".github/scripts/a37-lib.sh", 3300, "two calls of a37_fetch: 2 x (5 x 300 + 150) = 3300 s"),'
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/actions/a37-act/action.yml", "Thrice", ".github/scripts/a37-lib.sh", 4950, "three calls of a37_fetch: 3 x (5 x 300 + 150) = 4950 s"),'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "10	Twice" "16	Thrice"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses step '$step' of the action for its calls" \
    "$(contains "$out" "::error file=.github/actions/a37-act/action.yml,line=$line::step '$step' of .github/actions/a37-act/action.yml calls .a37_fetch., which .github/scripts/a37-lib.sh defines and which fetch, more than once")"
  check "refuses the entry keyed by step '$step'" \
    "$(contains "$out" "::error file=.github/scripts/check-download-retry.sh,line=1::the declared library figure for .github/actions/a37-act/action.yml step '$step' .github/scripts/a37-lib.sh is keyed in a form this check does not read")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# `retry_fetch <what> <bound> --` is the helper's signature, and its bound is read in that place
# alone (dispatch://INFRA-1545, round 37). A <what> that is a number is a description: the helper
# bounds each attempt at 300 s here, 5 x 300 + 150 = 1650 s, which no key of 3 minutes holds. The
# gate at d65e47ae read the first number of the three words, 5 s, charged 5 x 5 + 150 = 175 s, and
# passed it under 180 s.
echo "case: retry_fetch's bound is its second word, whatever its first"
root=$(fixture a37-retry-fetch-bound)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A number for what
        timeout-minutes: 3
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch 5 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "charges the bound the helper reads, 1650 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'A number for what' carries .timeout-minutes: 3., 180 s, and its own retries take 1650 s to exhaust")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A `<<` inside a quoted word is a character of that word, not a heredoc operator, as bash reads
# it: the `echo "files<<FILES"` a step writes to $GITHUB_OUTPUT for a multi-line value
# (dispatch://INFRA-1545, round 37). The curl after it is bare and is refused by rule 1. The gate
# at d65e47ae read the echo's `<<FILES` as an operator that no line closes, blanked every line
# after it as a body, read no fetch and passed the step.
echo "case: a << inside a quoted word opens no heredoc, and a fetch after it is read"
root=$(fixture a37-quoted-heredoc)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Write a multi-line output, then fetch
        run: |
          {
            echo "files<<FILES"
            printf 'a\n'
            echo "FILES"
          } >> "$GITHUB_OUTPUT"
          curl -fsSL -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the bare curl after the echo" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=14::.curl. in step 'Write a multi-line output, then fetch' fetches without the floor")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# The refusal of a definition this check does not read says where the fetch is: in its reach,
# which holds the body of every function a call after it calls, wherever that body is written
# (dispatch://INFRA-1545, round 37). fb_get's fetch is written before the `function` keyword's
# definition that calls it; the gate at d65e47ae said the fetch "is written after it".
echo "case: a function-keyword refusal names the fetch as in its reach, not as written after it"
root=$(fixture a37-keyword-message)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call a function twice through the keyword
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fb_get() {
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          }
          function fb_twice { fb_get; }
          fb_twice
          fb_twice
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "says the fetch is in the definition's reach" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=14::step 'Call a function twice through the keyword' of job 'build' defines a function with the .function. keyword, a form this check does not read as a function, and a fetch, or a call of a script that fetches, is in its reach: written on its line or after it, or in the body of a function that a call written there calls\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A call of a function through a positional parameter, `"$1"`, is a call through a variable like
# `"$fn"`, and is refused by name where a function the text defines fetches (dispatch://INFRA-1545,
# round 37). The step calls `fetch`, one `retry_fetch` at 300 s, 5 x 300 + 150 = 1650 s, twice
# through `"$1"`, 3300 s, which its key of 55 minutes, 3300 s, does not exceed; the site is
# charged once as written, 1650 s. A reading of VARIABLE_CALL without its digit read `$1` as no
# variable and passed the step.
echo "case: a call of a function through a positional parameter is refused by name"
root=$(fixture fa-positional-call)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through a positional parameter
        timeout-minutes: 55
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          set -- fetch
          "$1"
          "$1"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call through \$1 by name, naming the fetch it can reach" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'Fetch through a positional parameter' of job 'build' calls a function through a variable, .\\\$1., and a function this text defines or sources fetches — .curl. at .github/workflows/case.yaml:11\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A curl `--max-time=0`, and a `--max-time 0` written as the curl's last two words, set no limit,
# as the `--max-time 0` of u37-timeout-bounds does, and each is refused for the missing bound and
# not as a bound written as a variable (dispatch://INFRA-1545, round 37). A reading that took the
# word after `--max-time=0` as its value, or that read no value for a `--max-time` with one word
# after it, read each as bounded by a variable, which is not what either curl writes.
echo "case: a curl --max-time=0, and a --max-time 0 at the end of the curl, bound nothing"
root=$(fixture fa-max-time-zero)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A curl whose max-time is =0
        timeout-minutes: 28
        run: curl -fsSL --retry 7 --retry-all-errors --max-time=0 -o a https://example.com/a
      - name: A curl ending in max-time 0
        timeout-minutes: 28
        run: curl -fsSL --retry 7 --retry-all-errors -o a https://example.com/a --max-time 0
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
# `--max-time=0` is a spelling curl rejects before any attempt, which round 37 refuses by name
# for what curl makes of it (dispatch://INFRA-1545, round 37).
check "refuses 'A curl whose max-time is =0' by name, as a spelling curl rejects" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::.curl. in step 'A curl whose max-time is =0' writes .--max-time=0., a spelling curl rejects as an unknown option")"
check "refuses 'A curl ending in max-time 0' for a bound above 0" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::.curl. in step 'A curl ending in max-time 0' fetches without the floor: it lacks .--max-time <seconds>., which curl resets for each attempt, above 0, since .--max-time 0. sets no limit")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# Three more arrays whose words this check cannot count, each making the loop over its length a
# loop whose passes it cannot read (dispatch://INFRA-1545, round 37): `("$@")` holds as many
# words as the script is given, `("${other[@]}")` as many as `other` holds, and a `read -r -a`
# refills an array written with one word. Run with two arguments and three words, each loop
# makes two or three passes of a fetch at 1650 s, which no key of 28 minutes holds; read as one
# word each, every step was charged one pass, 1650 s, and passed.
echo "case: a for (( )) over an array of the arguments, of another array's words, or of a read -a is refused for its passes"
root=$(fixture fa-array-words)
cat > "$root/.github/scripts/fa-arguments.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
lists=("$@")
for ((i = 0; i < ${#lists[@]}; i++)); do
  retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
done
SH
chmod +x "$root/.github/scripts/fa-arguments.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: An array of the arguments
        timeout-minutes: 28
        run: .github/scripts/fa-arguments.sh a b
      - name: An array of another array's words
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          other=(a b c)
          lists=("${other[@]}")
          for ((i = 0; i < ${#lists[@]}; i++)); do
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          done
      - name: An array a read -a fills
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          lists=(a)
          read -r -a lists <<<"a b c"
          for ((i = 0; i < ${#lists[@]}; i++)); do
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "8	An array of the arguments	.github/scripts/fa-arguments.sh:6	5" \
  "11	An array of another array's words	.github/workflows/case.yaml:17	16" \
  "20	An array a read -a fills	.github/workflows/case.yaml:26	25"; do
  IFS=$'\t' read -r line step site loop <<<"$refusal"
  check "refuses '$step' for the loop's unread passes" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' carries .timeout-minutes: 28. and this check can put no figure on it: it runs a retried call inside a loop whose passes it cannot read — .curl. at $site, inside the loop at line $loop\.")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# A DECLARED_CALLS entry is charged at the first call it matches, not the second
# (dispatch://INFRA-1545, round 37). fa-once.sh calls `fetch`, 1650 s, once by name and once
# through `"$fn"`: 3300 s, which a key of 55 minutes on the step running it does not exceed. The
# entry writes the call through the variable's 1650 s, so the step is charged 3300 s and refused,
# and a key of 56 passes. A charge made only at the second call it matched charged 1650 s.
echo "case: a declared call through a variable in a script is charged at its one call"
root=$(fixture fa-declared-call-once)
cat > "$root/.github/scripts/fa-once.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
fn=fetch
fetch
"$fn"
SH
chmod +x "$root/.github/scripts/fa-once.sh"
write_once_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the script
        timeout-minutes: $1
        run: .github/scripts/fa-once.sh
YAML
}
once_entry='    DeclaredCall(".github/scripts/fa-once.sh", "$fn", 1650, "the one call of fetch through the variable, beside the call by name this check counts: 1 x (5 x 300 + 150) = 1650 s"),'
write_once_case 55
declare_in "$root" DECLARED_CALLS "$once_entry"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "charges the step the site's call by name and the declared call, 3300 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Run the script' carries .timeout-minutes: 55., 3300 s, and its own retries take 3300 s to exhaust")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
write_once_case 56
run_check "$root"
check "with a key above 3300 s, passes" "$(is "$status" 0)"

# A DECLARED_CALLS entry for a script is charged to the script, so the step running it is charged
# it, and one for a composite action is refused, a script being the one body this tree declares a
# call through a variable in (dispatch://INFRA-1545, round 37). The action's step and the script each call `fetch`, 1650 s, once by name and once through
# `"$fn"`, and each entry writes the second call's 1650 s: each workflow step is charged 3300 s
# and refused under its key of 55 minutes. Charged to the other table, the entry's seconds
# reached neither step, and each passed at 1650 s.
echo "case: a declared call in a script is charged to the step running it, and one in a composite action is refused"
root=$(fixture fa-declared-call-files)
mkdir -p "$root/.github/actions/fa-act"
cat > "$root/.github/actions/fa-act/action.yml" <<'YAML'
name: Fetch by name and through a variable
description: Calls fetch once by name and once through a variable.
runs:
  using: composite
  steps:
    - name: Fetch by name and through a variable
      shell: bash
      run: |
        . .github/scripts/ci-retry.sh
        fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
        fn=fetch
        fetch
        "$fn"
YAML
cat > "$root/.github/scripts/fa-variable.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
fn=fetch
fetch
"$fn"
SH
chmod +x "$root/.github/scripts/fa-variable.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - name: Use the action
        timeout-minutes: 55
        uses: ./.github/actions/fa-act
      - name: Run the script
        timeout-minutes: 55
        run: .github/scripts/fa-variable.sh
YAML
declare_in "$root" DECLARED_CALLS '    DeclaredCall(".github/actions/fa-act/action.yml", "$fn", 1650, "the one call of fetch through the variable: 1 x (5 x 300 + 150) = 1650 s"),'
declare_in "$root" DECLARED_CALLS '    DeclaredCall(".github/scripts/fa-variable.sh", "$fn", 1650, "the one call of fetch through the variable: 1 x (5 x 300 + 150) = 1650 s"),'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the action's call through the variable" \
  "$(contains "$out" "::error file=.github/actions/fa-act/action.yml,line=13::step 'Fetch by name and through a variable' of .github/actions/fa-act/action.yml calls a function through a variable, .\\\$fn.")"
check "refuses the entry naming the action" \
  "$(contains "$out" "::error file=.github/scripts/check-download-retry.sh,line=1::the declared call figure for .\\\$fn. in .github/actions/fa-act/action.yml names a workflow or a composite action")"
check "charges the step running the script the declared call beside the site, 3300 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Run the script' carries .timeout-minutes: 55., 3300 s, and its own retries take 3300 s to exhaust")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# The refusal of a call through a variable names the fetches of the functions it can reach, and
# no fetch written outside them (dispatch://INFRA-1545, round 37). The step makes a retried call
# of its own, outside any function, and calls `fetch` through `"$fn"`: the refusal names
# fetch's curl alone, since the step's own call is charged as written and no call through a
# variable can run it again.
echo "case: a refusal of a call through a variable names only the fetches of functions"
root=$(fixture fa-variable-call-sites)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through a variable beside a fetch
        timeout-minutes: 120
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the first" 300 -- curl -fsSL --max-time 300 -o b https://example.com/b
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=fetch
          "$fn"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call through the variable, naming fetch's curl alone" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=14::step 'Fetch through a variable beside a fetch' of job 'build' calls a function through a variable, .\\\$fn., and a function this text defines or sources fetches — .curl. at .github/workflows/case.yaml:12\. This check reads")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# An option of `timeout` that takes no value, a `--` after its options, and a `--` that is only
# the last word of the command it runs leave the fetch behind them a fetch, read at the duration
# after them (dispatch://INFRA-1545, round 37). Each curl here is bare and bounded at 60 s, read
# at curl's floor, 8 x 60 + 127 = 607 s, which its key of 11 minutes holds, so rule 1 is all that
# refuses it. Round 37's mutation pass found gates that read the duration as the command — one
# popping the word after `--foreground` as its value, ones that left a `--` before the duration
# unread or read it only as the second or the last word, one stopping at a `--` that ends the
# curl — and so read no fetch at all, and passed each bare curl.
echo "case: a timeout option taking no value, a -- before the duration, and a -- ending the command leave the fetch a fetch"
root=$(fixture fb-timeout-options)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A curl under timeout -- 60
        timeout-minutes: 11
        run: timeout -- 60 curl -fsSL -o a https://example.com/a
      - name: A curl under timeout --foreground 60
        timeout-minutes: 11
        run: timeout --foreground 60 curl -fsSL -o a https://example.com/a
      - name: A curl under timeout --foreground -- 60
        timeout-minutes: 11
        run: timeout --foreground -- 60 curl -fsSL -o a https://example.com/a
      - name: A curl ending in -- under timeout -k 5 60
        timeout-minutes: 11
        run: timeout -k 5 60 curl -fsSL -o a https://example.com/a --
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "9	A curl under timeout -- 60" \
  "12	A curl under timeout --foreground 60" \
  "15	A curl under timeout --foreground -- 60" \
  "18	A curl ending in -- under timeout -k 5 60"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "reads the curl in '$step' as a fetch, and refuses it for the floor" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::.curl. in step '$step' fetches without the floor: it lacks .--retry 7.")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# A library a sourced library sources is read for its fetching functions too, so a step calling
# one of them twice is refused by name unless declared (dispatch://INFRA-1545, round 37). The step
# sources fb-outer.sh, which sources fb-inner.sh, and calls fb_get, 5 x 300 + 150 = 1650 s, twice:
# it waits 3300 s, and a library is charged once for the shell, 1650 s. Round 37's mutation pass
# found gates reading only the library the step sources, which found no fetching function in
# fb-outer.sh and passed the step under a key of 28 minutes; a DECLARED_LIBRARIES entry for the
# step writing 3300 s replaces that charge, so 28 minutes is refused and 56 passes.
echo "case: a library sourced through another library is read for the fetching functions a step calls twice"
root=$(fixture fb-nested-library)
cat > "$root/.github/scripts/fb-outer.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/fb-inner.sh
SH
cat > "$root/.github/scripts/fb-inner.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
fb_get() {
  retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
}
SH
write_nested_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch twice through a library's library
        timeout-minutes: $1
        run: |
          . .github/scripts/fb-outer.sh
          fb_get
          fb_get
YAML
}
write_nested_case 28
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the two calls by name, naming the library that defines the function" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Fetch twice through a library's library' of job 'build' calls .fb_get., which .github/scripts/fb-inner.sh defines and which fetch, more than once")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/workflows/case.yaml", "Fetch twice through a library'"'"'s library", ".github/scripts/fb-inner.sh", 3300, "two calls of fb_get: 2 x (5 x 300 + 150) = 3300 s"),'
run_check "$root"
check "with the calls declared, charges the step the declared 3300 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Fetch twice through a library's library' carries .timeout-minutes: 28., 1680 s, and its own retries take 3300 s to exhaust")"
check "with the calls declared, refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
write_nested_case 56
run_check "$root"
check "with the calls declared and a key above 3300 s, passes" "$(is "$status" 0)"

# A definition with the `function` keyword is refused by name for a fetch in the body of a
# function its own body calls, wherever that function is written (dispatch://INFRA-1545, round
# 37). fb_twice's body calls fb_get, whose fetch, 5 x 300 + 150 = 1650 s, sits on the line between
# its `fb_get() {` and its `}`, before fb_twice; fb_twice is called twice, so the step waits 3300
# s, and the shell's reading charges the call in fb_twice's body once, 1650 s. Round 37's
# mutation pass found gates adding only the first or only the last line of a called function to
# the reach, which missed the fetch between them and passed the step under a key of 28 minutes.
echo "case: a function-keyword definition is refused for a fetch in a multi-line function it calls"
root=$(fixture fb-keyword-reach)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call a function twice through the keyword
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fb_get() {
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          }
          function fb_twice { fb_get; }
          fb_twice
          fb_twice
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the keyword definition by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=14::step 'Call a function twice through the keyword' of job 'build' defines a function with the .function. keyword, a form this check does not read as a function, and a fetch")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A run is read against the loops around the command holding it, also where its word starts with
# the `/` this check reads a script's name after, and where that command starts on a line a
# backslash joined to the line before (dispatch://INFRA-1545, round 37). fb-a.sh retries at 180 s,
# 5 x 180 + 150 = 1050 s, and the loop runs it three times, 3150 s, which a key of 52 minutes,
# 3120 s, does not exceed and one of 53 does. Round 37's mutation pass found a gate that looked
# for the command holding `/fb-a.sh` only past the word's first character, so it read the run
# against the logical line, the `echo start \` line before the loop, and charged one pass,
# 1050 s, passing the key of 52 minutes.
echo "case: a run whose word starts with the slash, behind a do on a continued line, is refused by name"
root=$(fixture fb-root-path)
cat > "$root/.github/scripts/fb-a.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/fb-a.sh"
write_root_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the script three times on a continued line
        timeout-minutes: $1
        run: |
          echo start \\
            ; for pass in 1 2 3; do /fb-a.sh; done
YAML
}
write_root_case 52
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
# A run behind `do` is no run this tree writes, continued line or not, so it is refused by name
# at the path's line and charged nothing (dispatch://INFRA-1545, round 37).
check "refuses the run behind do by name at the path's line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Run the script three times on a continued line' of job 'build' names .github/scripts/fb-a.sh in a command behind .do., a place this check does not read as a call")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# Where a composite action reaches a library along two paths and one of the scripts it runs calls
# that library's function twice, the action's DECLARED_LIBRARIES entry, which writes the library's
# whole contribution, is the figure the step is charged, not the script's (dispatch://INFRA-1545,
# round 37). The action runs fb-twice.sh, two calls of fb_get at 1650 s, and fb-once.sh, one: the
# step waits 3 x 1650 = 4950 s, the action's entry, and fb-twice.sh's own entry writes its 3300 s.
# Round 37's mutation pass found a gate taking the script's entry first, which charged the step
# 3300 s and passed a key of 82 minutes, 4920 s; this tree's ci-ok report steps are the same
# shape, and that gate's --jobs charged them 1380 s where this one charges 2160 s.
echo "case: an action reaching a library along two paths is charged its own declaration, not a script's"
root=$(fixture fb-action-library)
cat > "$root/.github/scripts/fb-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
fb_get() {
  retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
}
SH
cat > "$root/.github/scripts/fb-twice.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/fb-lib.sh
fb_get
fb_get
SH
cat > "$root/.github/scripts/fb-once.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/fb-lib.sh
fb_get
SH
chmod +x "$root/.github/scripts/fb-twice.sh" "$root/.github/scripts/fb-once.sh"
mkdir -p "$root/.github/actions/fb-act"
cat > "$root/.github/actions/fb-act/action.yml" <<'YAML'
name: Fetch three times
description: Runs a script that fetches twice and one that fetches once.
runs:
  using: composite
  steps:
    - name: Run both scripts
      shell: bash
      run: |
        .github/scripts/fb-twice.sh
        .github/scripts/fb-once.sh
YAML
write_action_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Use the action
        timeout-minutes: $1
        uses: ./.github/actions/fb-act
YAML
}
write_action_case 82
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/actions/fb-act/action.yml", None, ".github/scripts/fb-lib.sh", 4950, "fb-twice.sh'"'"'s two calls of fb_get and fb-once.sh'"'"'s one: 3 x (5 x 300 + 150) = 4950 s"),'
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/scripts/fb-twice.sh", None, ".github/scripts/fb-lib.sh", 3300, "two calls of fb_get: 2 x (5 x 300 + 150) = 3300 s"),'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "charges the step the action's declared 4950 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Use the action' carries .timeout-minutes: 82., 4920 s, and its own retries take 4950 s to exhaust")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
write_action_case 83
run_check "$root"
check "with a key above 4950 s, passes" "$(is "$status" 0)"

# A DECLARED_LIBRARIES entry prices the library it names and no other: a step calling the
# fetching functions of two libraries twice each, with an entry for one of them only, is refused
# for the other (dispatch://INFRA-1545, round 37). fb_fa costs 5 x 60 + 150 = 450 s and fb_fb
# 1650 s, so the step waits 2 x 450 + 2 x 1650 = 4200 s. Round 37's mutation pass found a gate
# matching an entry by its file and step alone, which priced fb-lb.sh at fb-la.sh's 900 s,
# charged 1800 s and passed a key of 69 minutes, 4140 s; with both declared, 69 is refused for
# 4200 s and 71 passes.
echo "case: a library declaration prices only the library it names"
root=$(fixture fb-two-libraries)
for library in "fb-la.sh fb_fa 60" "fb-lb.sh fb_fb 300"; do
  read -r file fetcher seconds <<<"$library"
  cat > "$root/.github/scripts/$file" <<SH
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
$fetcher() {
  retry_fetch "the asset" $seconds -- curl -fsSL --max-time $seconds -o a https://example.com/a
}
SH
done
write_libraries_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch twice through each of two libraries
        timeout-minutes: $1
        run: |
          . .github/scripts/fb-la.sh
          . .github/scripts/fb-lb.sh
          fb_fa
          fb_fa
          fb_fb
          fb_fb
YAML
}
write_libraries_case 69
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/workflows/case.yaml", "Fetch twice through each of two libraries", ".github/scripts/fb-la.sh", 900, "two calls of fb_fa: 2 x (5 x 60 + 150) = 900 s"),'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the undeclared library's two calls by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=14::step 'Fetch twice through each of two libraries' of job 'build' calls .fb_fb., which .github/scripts/fb-lb.sh defines and which fetch, more than once")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/workflows/case.yaml", "Fetch twice through each of two libraries", ".github/scripts/fb-lb.sh", 3300, "two calls of fb_fb: 2 x (5 x 300 + 150) = 3300 s"),'
run_check "$root"
check "with both declared, charges the step 900 + 3300 = 4200 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Fetch twice through each of two libraries' carries .timeout-minutes: 69., 4140 s, and its own retries take 4200 s to exhaust")"
check "with both declared, refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
write_libraries_case 71
run_check "$root"
check "with both declared and a key above 4200 s, passes" "$(is "$status" 0)"

# A loop opened after `time`, a loop opened after `then` inside `bash -c` text, and `-c` text
# written after an option that takes no value are each read as bash reads them
# (dispatch://INFRA-1545, round 37). Each step fetches with one curl making its own attempts,
# `--retry 7` at `--max-time 120`, 1087 s a call. `time for pass in 1 2` runs it twice, 2174 s,
# and so does the `for` after `then` in the `bash -c` text; their keys of 30 minutes, 1800 s, sit
# between one call and two, so a reading that finds no loop there charges one call, passes the
# key, and only the refusal by name holds the step. `bash -e -c '…'` and zsh's
# `zsh --bsd-echo -c '…'` run the text behind `-c`: `-e` takes no value, and a `--` long option of
# zsh's is an option name, `--bsd-echo` setting BSD_ECHO, which takes none either, so each step
# is charged its 1087 s against a key of 1 minute. A reading that took `-e`, or a long option
# ending in `o`, as the `-o` that takes the next word read `-c` as that word, found no text, and
# charged each of those steps nothing.
echo "case: a loop after time or after then in -c text is refused, and -c text after -e or a zsh long option is charged"
root=$(fixture fc-shell-text)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Time a loop
        timeout-minutes: 30
        run: |
          time for pass in 1 2; do
            curl -fsSL --retry 7 --retry-all-errors --max-time 120 -o b https://example.com/b
          done
      - name: Open a loop after then in bash -c text
        timeout-minutes: 30
        run: |
          bash -c 'if true; then for pass in 1 2; do curl -fsSL --retry 7 --retry-all-errors --max-time 120 -o b https://example.com/b; done; fi'
      - name: Run -c text after bash -e
        timeout-minutes: 1
        run: |
          bash -e -c 'curl -fsSL --retry 7 --retry-all-errors --max-time 120 -o b https://example.com/b'
      - name: Run -c text after a zsh long option
        timeout-minutes: 1
        run: |
          zsh --bsd-echo -c 'curl -fsSL --retry 7 --retry-all-errors --max-time 120 -o b https://example.com/b'
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the loop after time by name at its line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Time a loop' of job 'build' writes a loop after a reserved word on its line, such as .then for x in 1 2; do., and a fetch")"
check "refuses the bash -c text holding a loop after then by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=16::step 'Open a loop after then in bash -c text' of job 'build' runs .bash -c. text, whose text holds a loop or a function, and a fetch is written in it")"
for refusal in "20	Run -c text after bash -e	bash -e -c" "24	Run -c text after a zsh long option	zsh --bsd-echo -c"; do
  IFS=$'\t' read -r line step form <<<"$refusal"
  check "refuses '$step' by name for the form of its text" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' runs .$form. text, a form other than .bash -c., and a fetch is written in it")"
done
for refusal in "18	Run -c text after bash -e" "22	Run -c text after a zsh long option"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "charges '$step' its curl, 1087 s" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' carries .timeout-minutes: 1., 60 s, and its own retries take 1087 s to exhaust")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 6)"

# A heredoc fed to a shell the check cannot name is refused for a fetch in its body whatever the
# words around the operator (dispatch://INFRA-1545, round 37): a shell named by a variable and
# given an option, `"$SHELL" -e`, is named by its first word and not its last, and a printed copy
# piped into `sh`, `zsh` or `dash`, or into `bash` with no blank after the `|`, is a pipe into a
# shell. fc-hc.sh retries at 180 s, 1050 s, and each step names it in the body under a key of 1
# minute; each shell named here runs what it reads on its standard input, so a reading that takes
# the body for data charges the step nothing and passes it.
echo "case: a heredoc fed to a shell named by a variable with an option, or piped into sh, zsh, dash or |bash, is refused"
root=$(fixture fc-heredoc-consumers)
cat > "$root/.github/scripts/fc-hc.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/fc-hc.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Feed it to a shell named by a variable given -e
        timeout-minutes: 1
        run: |
          "$SHELL" -e <<'EOF'
          .github/scripts/fc-hc.sh
          EOF
      - name: Pipe a printed copy into sh
        timeout-minutes: 1
        run: |
          cat <<'EOF' | sh
          .github/scripts/fc-hc.sh
          EOF
      - name: Pipe a printed copy into zsh
        timeout-minutes: 1
        run: |
          cat <<'EOF' | zsh
          .github/scripts/fc-hc.sh
          EOF
      - name: Pipe a printed copy into dash
        timeout-minutes: 1
        run: |
          cat <<'EOF' | dash
          .github/scripts/fc-hc.sh
          EOF
      - name: Pipe a printed copy into bash with no blank
        timeout-minutes: 1
        run: |
          cat <<'EOF' |bash
          .github/scripts/fc-hc.sh
          EOF
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the heredoc fed to \$SHELL -e by name, naming the variable" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Feed it to a shell named by a variable given -e' of job 'build' feeds a heredoc to .\\\$SHELL., and the body holds a fetch or names a script that fetches")"
for refusal in "16	Pipe a printed copy into sh" "22	Pipe a printed copy into zsh" \
  "28	Pipe a printed copy into dash" "34	Pipe a printed copy into bash with no blank"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' by name at its operator" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' feeds a heredoc to .cat., whose output a pipe on the operator's line hands to a shell, and the body holds a fetch or names a script that fetches")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"

# A call of a function through a variable is refused wherever bash runs it as a command
# (dispatch://INFRA-1545, round 37): last in a pipeline, `echo go | "$fn"`; behind an assignment
# whose value holds a `$(…)`, `echo go | TS=$(date) "$fn"`; last in a subshell written over lines;
# and alone in a subshell after a pipe elsewhere in the text. A `case` pattern, `$fn)` after `in`,
# `;;`, `;&` or `|`, is no call, and only a word that is one and nothing else before a `)` can be
# one. Each step calls `fetch`, one `retry_fetch` at 300 s, 1650 s, once by its name and once
# through `"$fn"`, 3300 s, under a key of 28 minutes, 1680 s: a reading that takes the call for a
# pattern charges the named call alone, 1650 s, and passes the step.
echo "case: a call of a function through a variable after a pipe, an assignment or in a subshell is refused"
root=$(fixture fc-variable-calls)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call it through a variable at a pipe's end
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=fetch
          fetch
          echo go | "$fn"
      - name: Call it through a variable after an assignment at a pipe's end
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=fetch
          fetch
          echo go | TS=$(date) "$fn"
      - name: Call it through a variable last in a subshell
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=fetch
          fetch
          (
            echo go
            "$fn"
          )
      - name: Call it through a variable in a subshell after a pipe
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=fetch
          echo go | fetch
          ( "$fn" )
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "14	11	Call it through a variable at a pipe's end" \
  "22	19	Call it through a variable after an assignment at a pipe's end" \
  "32	27	Call it through a variable last in a subshell" \
  "41	38	Call it through a variable in a subshell after a pipe"; do
  IFS=$'\t' read -r line fetch step <<<"$refusal"
  check "refuses '$step' by name at the call" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' calls a function through a variable, .\\\$fn., and a function this text defines or sources fetches — .curl. at .github/workflows/case.yaml:$fetch\.")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# An excepted site whose `retry_fetch` seconds are a variable makes attempts of its own that no
# figure this check reads bounds, and is refused for it whatever its verb (dispatch://INFRA-1545,
# round 37). In the credentialed publish job every site is excepted (exception (d)), so rule 4
# does not read this one and no figure prices it: `retry_fetch` runs five attempts of
# "$READ_SECONDS" each with 150 s of sleeps between them, at least 150 s, under a key of 1 minute.
# A refusal that read a curl's own `--retry` alone passed this `gh api`. The refusal says the
# seconds are that variable: until round 37's class search it said this check finds no bound on
# the attempts, which the helper bounds at what "$READ_SECONDS" holds.
echo "case: an excepted gh api under a retry_fetch whose seconds are a variable is refused for that variable"
root=$(fixture fc-excepted-unbounded)
cat >> "$root/.github/workflows/release.yaml" <<'YAML'
      - name: Read it under seconds the step sets
        timeout-minutes: 1
        run: |
          retry_fetch "the latest release" "$READ_SECONDS" -- gh api "repos/$GITHUB_REPOSITORY/releases/latest"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the gh api as an excepted site whose helper's seconds are a variable" \
  "$(contains "$out" "::error file=.github/workflows/release.yaml,line=23::.gh api. in step 'Read it under seconds the step sets' is a site an exception above leaves unretried, and it is retried by .retry_fetch. with its seconds written as .\\\$READ_SECONDS., which is no number this check reads")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A library a composite action's step reaches along more than one path is refused at that action,
# and declared for it, when another composite action uses it (dispatch://INFRA-1545, round 37).
# fc-inner's step sources fc-lib.sh and calls fc_fetch, one `retry_fetch` at 300 s, 1650 s, and
# runs fc-run.sh, which sources it and calls it again: 3300 s. fc-outer uses fc-inner and sources
# nothing, and the workflow step uses fc-outer. The refusal names fc-inner, the file whose step
# reaches the library twice, and a DECLARED_LIBRARIES entry for fc-inner prices it, 3300 s, which
# a key of 55 minutes is refused for and one of 56 passes. A reading that named the first action
# of the path, fc-outer, refused the wrong file and took a declaration written there.
echo "case: a library an action used by another action reaches twice is refused and declared at the action that reaches it"
root=$(fixture fc-nested-library)
mkdir -p "$root/.github/actions/fc-outer" "$root/.github/actions/fc-inner"
cat > "$root/.github/scripts/fc-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
fc_fetch() {
  retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
}
SH
cat > "$root/.github/scripts/fc-run.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/fc-lib.sh
fc_fetch
SH
chmod +x "$root/.github/scripts/fc-run.sh"
cat > "$root/.github/actions/fc-inner/action.yml" <<'YAML'
name: Fetch through the library twice
description: Sources the library and calls its function, and runs a script that does the same.
runs:
  using: composite
  steps:
    - name: Fetch through the library and through a script
      shell: bash
      run: |
        . .github/scripts/fc-lib.sh
        fc_fetch
        .github/scripts/fc-run.sh
YAML
cat > "$root/.github/actions/fc-outer/action.yml" <<'YAML'
name: Use the action that fetches twice
description: Uses fc-inner.
runs:
  using: composite
  steps:
    - uses: ./.github/actions/fc-inner
YAML
write_nested_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Use the outer action
        uses: ./.github/actions/fc-outer
        timeout-minutes: $1
YAML
}
write_nested_case 56
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the inner action by name for the library it reaches twice" \
  "$(contains "$out" "::error file=.github/actions/fc-inner/action.yml,line=1::.github/actions/fc-inner/action.yml reaches .github/scripts/fc-lib.sh, a library that fetches, along more than one path")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/actions/fc-inner/action.yml", None, ".github/scripts/fc-lib.sh", 3300, "one call of fc_fetch from the step and one from fc-run.sh: 2 x (5 x 300 + 150) = 3300 s"),'
write_nested_case 55
run_check "$root"
check "with the library declared for the inner action, charges the step the declared 3300 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::step 'Use the outer action' carries .timeout-minutes: 55., 3300 s, and its own retries take 3300 s to exhaust")"
check "with the library declared, refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
write_nested_case 56
run_check "$root"
check "with the library declared and a key above 3300 s, passes" "$(is "$status" 0)"


# A refusal names the line it reads, and in a `run: |` block the body's first line is the line
# after the `run:` key (dispatch://INFRA-1545, round 37). The bare curl is the first line of each
# body: line 9 of the workflow, under its `run: |` on line 8, and line 9 of the action, under its
# `run: |` on line 8. The gate at d65e47ae counted a block's lines from its key and named line 8
# for both, the key's own line, which holds no fetch.
echo "case: a refusal names the first line of a run: | block as that line, not the key's"
root=$(fixture a37-block-first-line)
mkdir -p "$root/.github/actions/a37-first"
cat > "$root/.github/actions/a37-first/action.yml" <<'YAML'
name: Fetch on the first line
description: A bare curl on the first line of its step's block.
runs:
  using: composite
  steps:
    - name: Fetch bare
      shell: bash
      run: |
        curl -fsSL -o b https://example.com/b
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch bare
        run: |
          curl -fsSL -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the workflow body's first line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::.curl. in step 'Fetch bare' fetches without the floor")"
check "names the action body's first line" \
  "$(contains "$out" "::error file=.github/actions/a37-first/action.yml,line=9::.curl. in step 'Fetch bare' fetches without the floor")"
check "names no key's line" "$(is "$(contains "$out" 'line=8::')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A run on a line a backslash continues is read against the loops of the line its own command
# starts on, and not the line its logical line starts on, whatever its word starts with
# (dispatch://INFRA-1545, round 37). za-c.sh retries at 180 s, 1050 s. After `echo start \`, a
# three-pass loop opens on the next line, and its body runs the script after a `true;`, as a
# command of its own and not behind `do`, three times: 3150 s over a key of 30 minutes, 1800 s.
# The gate reads any word ending in `/za-c.sh` as a run of the script, so the second step's
# `/za-c.sh`, a copy a step put at the root, is the same run to it. A reading that found the
# command holding the path only past the word's first character, or only at its first or its
# last, keyed the run to the `echo start` line, outside the loop, and charged one pass, 1050 s,
# passing the key of 30 minutes.
echo "case: a run on a continued line inside a loop opening there is charged for each pass"
root=$(fixture za-continued-loop)
cat > "$root/.github/scripts/za-c.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/za-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the script three times on a continued line
        timeout-minutes: 30
        run: |
          echo start \
            && for pass in 1 2 3; do true; .github/scripts/za-c.sh; done
      - name: Run the copy at the root three times on a continued line
        timeout-minutes: 30
        run: |
          echo start \
            && for pass in 1 2 3; do true; /za-c.sh; done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the run on a continued line for its three passes, 3150 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Run the script three times on a continued line' carries .timeout-minutes: 30., 1800 s, and its own retries take 3150 s to exhaust")"
check "refuses the run of the copy at the root for its three passes, 3150 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'Run the copy at the root three times on a continued line' carries .timeout-minutes: 30., 1800 s, and its own retries take 3150 s to exhaust")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A call of a function after `then` is refused by name where the function fetches, and the
# fetch it reaches can be two calls away (dispatch://INFRA-1545, round 37). za_fetch retries at
# 300 s, 1650 s; za_middle calls it and za_outer calls za_middle. The step calls za_outer twice
# after `then`, which bash runs twice, 3300 s, over a key of 28 minutes, 1680 s; this check
# counts no call after `then`, so it charges the fetch once, 1650 s, and the refusal is what
# stands for the second call. A reach that added one function's body and stopped reached
# za_middle and never za_fetch, refused nothing, and passed the key.
echo "case: an odd call of a function two calls above a fetch is refused by name"
root=$(fixture za-reach-two-deep)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call a function two calls above a fetch after then
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          za_fetch() {
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          }
          za_middle() {
            za_fetch
          }
          za_outer() {
            za_middle
          }
          if true; then za_outer; fi
          if true; then za_outer; fi
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for line in 20 21; do
  check "refuses the call of za_outer after then at line $line by name" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step 'Call a function two calls above a fetch after then' of job 'build' calls .za_outer. after .then., a form this check does not read as a call of a function, and .za_outer. fetches")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A library function whose one fetch costs nothing, an unretried `gh api` that exception (d)
# leaves bare, is no fetching function, so calling it twice is no reading a DeclaredLibrary entry
# answers, and an entry for it matches nothing (dispatch://INFRA-1545, round 37). za-lib.sh's
# own top level retries at 180 s, 1050 s, and release.yaml's credentialed publish job sources it,
# which makes its sites exception (d)'s. The case step sources it, which runs that top level once,
# 1050 s, over a key of one minute, and calls za_tag twice. A reading that took za_tag for a
# fetching function took its two calls for a reading the entry answers, replaced the library's
# 1050 s with the entry's 1 s and passed the key.
echo "case: a library function holding only a costless fetch is no reading a declaration answers"
root=$(fixture za-costless-library)
cat > "$root/.github/scripts/za-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
za_tag() {
  gh api "repos/$GITHUB_REPOSITORY/releases/latest"
}
SH
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
      - name: Source the library
        timeout-minutes: 18
        run: . .github/scripts/za-lib.sh
YAML
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source the library and read the tag twice
        timeout-minutes: 1
        run: |
          . .github/scripts/za-lib.sh
          za_tag
          za_tag
YAML
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/workflows/case.yaml", "Source the library and read the tag twice",
                    ".github/scripts/za-lib.sh", 1, "two reads of the tag, one attempt each"),'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the step's key at the library's 1050 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Source the library and read the tag twice' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust")"
check "refuses the declaration as matching nothing" \
  "$(contains "$out" "::error file=.github/scripts/check-download-retry.sh,line=1::the declared library figure for .github/workflows/case.yaml step 'Source the library and read the tag twice' .github/scripts/za-lib.sh matches no step that reaches that library along more than one path or through more than one call of its fetching functions")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A source behind an assignment is refused by name where the library fetches, since a source is
# read only with nothing before it (dispatch://INFRA-1545, round 37). za-lib.sh's own top level
# retries at 180 s, 1050 s. The step sources it and then sources it again behind `ZA_AGAIN=1`,
# and bash runs that top level twice, 2100 s, over a key of 18 minutes, 1080 s. A reading that
# let an assignment stand before a source read the second as a source of the same library, which
# is charged once for the shell, 1050 s, and passed the key.
echo "case: a source behind an assignment, after a source of the same library, is refused by name"
root=$(fixture za-source-behind-assignment)
cat > "$root/.github/scripts/za-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source the library, then again behind an assignment
        timeout-minutes: 18
        run: |
          . .github/scripts/za-lib.sh
          ZA_AGAIN=1 . .github/scripts/za-lib.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the source behind ZA_AGAIN=1 by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Source the library, then again behind an assignment' of job 'build' names .github/scripts/za-lib.sh in a command behind .ZA_AGAIN=1., a place this check does not read as a call")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# `sh -c` text is refused by name where it fetches, and is not read as `bash -c` text
# (dispatch://INFRA-1545, round 37). sh expands an alias in a shell that is not interactive (dash,
# and bash run as sh), and bash does not, so `alias nice='for pass in 1 2 3; do'` makes the next
# line's `nice curl …; done` a loop of three passes, which a bash reading of the text charges
# once: 8 x 300 + 127 = 2527 s read, 3 x 2527 = 7581 s waited, under a key of 43 minutes
# (2580 s). A reading that took `sh -c` for `bash -c` passed this step.
echo "case: zb sh -c text whose alias opens a loop is refused by name"
root=$(fixture zb-sh-alias)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  zb-sh-alias:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch in sh text through an alias
        timeout-minutes: 43
        run: |
          sh -c "alias nice='for pass in 1 2 3; do'
          nice curl -fsSL --retry 7 --retry-all-errors --max-time 300 -o a https://example.com/a; done"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the sh -c text by name at its line" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml,line=33::step 'Fetch in sh text through an alias' of job 'zb-sh-alias' runs .sh -c. text, a form other than .bash -c., and a fetch is written in it")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A block scalar's text starts on the line after its `run:` key, whatever the comment after the
# `|` says (dispatch://INFRA-1545, round 37). The header test reads the text after the key line's
# first `run:`; `run: |  # run: once, with no retry` holds a second one, and a reading of the text
# after that one found no `|`, so it numbered every refusal of the body one line early, the defect
# round 37 fixed for every block. The bare curl is refused at its own line, 32, not at the key's.
echo "case: zb a run key whose comment holds run: numbers its body from the next line"
root=$(fixture zb-run-comment)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  zb-run-comment:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch the asset bare
        run: |  # run: once, with no retry
          curl -fsSL -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the curl at its own line" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml,line=32::.curl. in step 'Fetch the asset bare' fetches without the floor")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# An excepted curl whose `--max-time` is a variable has a bound this check cannot read, and the
# step holding it is refused for that bound (dispatch://INFRA-1545, round 37). In the credentialed
# publish job every site is excepted (exception (d)). The excepted-site refusal is for attempts
# with no bound at all, and must not say that of this curl: its bound is written, as "$T", and a
# sentence telling the author to bound each attempt sends them to write what they wrote.
echo "case: zb an excepted curl bounded by a variable is refused for that bound and not as unbounded"
root=$(fixture zb-excepted-variable-bound)
cat >> "$root/.github/workflows/release.yaml" <<'YAML'
      - name: Fetch the asset at a bound the step sets
        timeout-minutes: 5
        run: |
          curl -fsSL --retry 7 --retry-all-errors --max-time "$T" -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the step at its key for a bound written as a variable" \
  "$(contains "$out" "::error file=.github/workflows/release.yaml,line=21::step 'Fetch the asset at a bound the step sets' carries .timeout-minutes: 5. and this check can put no figure on it: it runs a retried call whose attempts are bounded by a variable rather than by a number — .curl. at .github/workflows/release.yaml[:]23\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A library a step reaches along more than one path is refused at that step, whatever else the
# library runs (dispatch://INFRA-1545, round 37). In the credentialed publish job the step sources
# zb-twice-lib.sh and runs zb-twice-run.sh, which sources it again, so the library's own commands
# run twice. The library runs zb-twice-notes.sh, an excepted bare curl with no `--max-time`, whose
# attempt has no figure, and then zb-twice-asset.sh, 8 x 300 + 127 = 2527 s: 2 x 2527 = 5054 s at
# least, under a key of 5 minutes. The step's figure is None for the bare curl, so this refusal is
# the one that stands. A charge that stopped reading the library at the call with no figure priced
# it at 0 s, and a library priced at 0 s is not refused for its second path.
echo "case: zb a library reached twice whose first run has no figure is refused for its two paths"
root=$(fixture zb-twice-library)
cat > "$root/.github/scripts/zb-twice-lib.sh" <<'SH'
#!/usr/bin/env bash
.github/scripts/zb-twice-notes.sh
.github/scripts/zb-twice-asset.sh
SH
cat > "$root/.github/scripts/zb-twice-notes.sh" <<'SH'
#!/usr/bin/env bash
curl -fsSL -o notes https://example.com/notes
SH
cat > "$root/.github/scripts/zb-twice-asset.sh" <<'SH'
#!/usr/bin/env bash
curl -fsSL --retry 7 --retry-all-errors --max-time 300 -o asset https://example.com/asset
SH
cat > "$root/.github/scripts/zb-twice-run.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/zb-twice-lib.sh
SH
chmod +x "$root/.github/scripts/zb-twice-notes.sh" "$root/.github/scripts/zb-twice-asset.sh" \
  "$root/.github/scripts/zb-twice-run.sh"
cat >> "$root/.github/workflows/release.yaml" <<'YAML'
      - name: Source the library and run the script that sources it
        timeout-minutes: 5
        run: |
          . .github/scripts/zb-twice-lib.sh
          .github/scripts/zb-twice-run.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the step for reaching the library along more than one path" \
  "$(contains "$out" "::error file=.github/workflows/release.yaml,line=21::step 'Source the library and run the script that sources it' reaches .github/scripts/zb-twice-lib.sh, a library that fetches, along more than one path")"

# The same refusal, whatever order the step's libraries are charged in (dispatch://INFRA-1545,
# round 37). The credentialed step sources zb-order-b.sh, an excepted bare curl with no figure,
# then zb-order-a.sh, whose top-level curl is 8 x 300 + 127 = 2527 s, and runs zb-order-run.sh,
# which sources zb-order-a.sh again: 2 x 2527 = 5054 s at least, under a key of 5 minutes. The
# libraries are charged in the order their names sort, zb-order-a.sh first, so it is refused for
# its second path before zb-order-b.sh ends the step's figure. A charge in the reverse order, or
# in the order the step sources them, stopped at zb-order-b.sh and refused nothing.
echo "case: zb a library reached twice is refused before a library with no figure ends the charge"
root=$(fixture zb-order-library)
cat > "$root/.github/scripts/zb-order-a.sh" <<'SH'
#!/usr/bin/env bash
curl -fsSL --retry 7 --retry-all-errors --max-time 300 -o asset https://example.com/asset
SH
cat > "$root/.github/scripts/zb-order-b.sh" <<'SH'
#!/usr/bin/env bash
curl -fsSL -o notes https://example.com/notes
SH
cat > "$root/.github/scripts/zb-order-run.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/zb-order-a.sh
SH
chmod +x "$root/.github/scripts/zb-order-run.sh"
cat >> "$root/.github/workflows/release.yaml" <<'YAML'
      - name: Source both libraries and run the script that sources one
        timeout-minutes: 5
        run: |
          . .github/scripts/zb-order-b.sh
          . .github/scripts/zb-order-a.sh
          .github/scripts/zb-order-run.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the step for reaching zb-order-a.sh along more than one path" \
  "$(contains "$out" "::error file=.github/workflows/release.yaml,line=21::step 'Source both libraries and run the script that sources one' reaches .github/scripts/zb-order-a.sh, a library that fetches, along more than one path")"

# A heredoc opened with `<<-` ends at its terminator written after tabs, which bash strips from
# the body's lines and from the terminator's (dispatch://INFRA-1545, round 37). The body here
# holds an apostrophe and the curl after the heredoc is bare, which rule 1 refuses. A reading
# that took no `<<-` for an operator read the body as the step's own shell, and its apostrophe
# opened a quote that swallowed the curl; a reading that compared the terminator line unstripped
# ran the body to the end of the step and blanked the curl with it. Each read no fetch and
# passed the step, where bash closes the body at the tab and runs the curl.
echo "case: a <<- heredoc closes at its tab-indented terminator, and a fetch after it is read"
root=$(fixture zc-dash-heredoc)
tab=$'\t'
cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Write a note with an indented heredoc, then fetch
        run: |
          cat <<-EOF > note.txt
          ${tab}it's a note
          ${tab}EOF
          curl -fsSL -o a https://example.com/a
          echo 'ok'
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the bare curl after the heredoc" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::.curl. in step 'Write a note with an indented heredoc, then fetch' fetches without the floor")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# `--` ends timeout(1)'s options, and the word after it is the duration whatever it starts with
# (dispatch://INFRA-1545, round 37). GNU timeout takes `-0` there as a duration of 0, which sets
# no limit, and runs the curl, which is bare and refused by rule 1. A reading that went on
# taking options after `--` took `-0` for one and `curl` for the duration, read the curl's own
# options as the command, found no fetch and passed the step.
echo "case: the word after timeout's -- is its duration, and the fetch after that is read"
root=$(fixture zc-timeout-minus-zero)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch under a timeout of minus zero
        run: timeout -- -0 curl -fsSL -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the bare curl behind the timeout" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::.curl. in step 'Fetch under a timeout of minus zero' fetches without the floor")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A `timeout` inside a `retry_fetch` whose own bound is a variable is what bounds each attempt,
# and its seconds, in each spelling timeout(1) reads, are the figure rule 8 multiplies
# (dispatch://INFRA-1545, round 37): `timeout 300` gives 5 x 300 + 150 = 1650 s and `timeout
# 1m` 5 x 60 + 150 = 450 s, which keys of 28 and 8 minutes hold and keys of 27 and 7 do not. A
# reading of the duration as one digit, as no bound, or as no bound at 1 refused a step it should
# have priced, and under the short keys said instead that no attempt of the call is bounded in
# time, which is false.
echo "case: a timeout inside a retry with a variable bound bounds each attempt"
root=$(fixture zc-timeout-bound)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A timeout of 300 inside a retry with a variable bound
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the index" "$ZC_BOUND" -- timeout 300 gh api repos/o/r
      - name: A timeout of 300 inside a retry with a variable bound, under a short key
        timeout-minutes: 27
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the index" "$ZC_BOUND" -- timeout 300 gh api repos/o/r
      - name: A timeout of one minute inside a retry with a variable bound
        timeout-minutes: 8
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the index" "$ZC_BOUND" -- timeout 1m gh api repos/o/r
      - name: A timeout of one minute inside a retry with a variable bound, under a short key
        timeout-minutes: 7
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the index" "$ZC_BOUND" -- timeout 1m gh api repos/o/r
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "charges the timeout of 300 its 1650 s under the key of 27 minutes" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'A timeout of 300 inside a retry with a variable bound, under a short key' carries .timeout-minutes: 27., 1620 s, and its own retries take 1650 s to exhaust")"
check "charges the timeout of one minute its 450 s under the key of 7 minutes" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=23::step 'A timeout of one minute inside a retry with a variable bound, under a short key' carries .timeout-minutes: 7., 420 s, and its own retries take 450 s to exhaust")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A curl's own `--max-time` is its per-attempt bound wherever the curl writes it and at any
# number above 0 (dispatch://INFRA-1545, round 37). `--max-time 1` gives 8 x 1 + 127 = 135 s,
# above a key of 2 minutes; `--max-time 300` written as the curl's last two words gives
# 8 x 300 + 127 = 2527 s, above a key of 42. A reading that took no 1 for a bound refused the
# first as bounded by a variable, and one that read no value for a `--max-time` with one word
# after it refused the second as lacking `--max-time`: neither is what the curl writes.
echo "case: a curl's max-time of 1, and one written as its last two words, are its bound"
root=$(fixture zc-curl-bound-forms)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A one-second max-time under a short key
        timeout-minutes: 2
        run: curl -fsSL --retry 7 --retry-all-errors --max-time 1 -o a https://example.com/a
      - name: A max-time written last, under a short key
        timeout-minutes: 42
        run: curl -fsSL --retry 7 --retry-all-errors -o a https://example.com/a --max-time 300
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "charges the one-second max-time 135 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'A one-second max-time under a short key' carries .timeout-minutes: 2., 120 s, and its own retries take 135 s to exhaust")"
check "charges the max-time written last 2527 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'A max-time written last, under a short key' carries .timeout-minutes: 42., 2520 s, and its own retries take 2527 s to exhaust")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A curl that writes `--max-time 0` and then `--max-time 300` is refused by name for writing its
# bound twice, and curl takes the last, 300, so it does not lack a bound above 0
# (dispatch://INFRA-1545, round 37). A reading that asked the first value whether it is 0 said
# the curl lacks `--max-time <seconds>` above 0, which is false. The gate's other line on this
# step is not pinned here: it says the attempts are bounded by a variable, which no word of the
# curl is.
echo "case: a curl writing --max-time 0 and then --max-time 300 lacks no bound"
root=$(fixture zc-curl-zero-then-bound)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A max-time of 0 and then of 300
        timeout-minutes: 43
        run: curl -fsSL --retry 7 --retry-all-errors --max-time 0 --max-time 300 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the curl by name for writing its bound twice" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::.curl. in step 'A max-time of 0 and then of 300' writes its per-attempt bound 2 times, .0. and .300., and curl takes the last one")"
check "does not say the curl lacks a max-time above 0" \
  "$(is "$(contains "$out" "fetches without the floor: it lacks .--max-time <seconds>.")" false)"

# A `retry_fetch` whose bound no `--` follows is outside the helper's signature, wherever the
# command after it starts (dispatch://INFRA-1545, round 37): the helper stops on it with "no --
# separator" before any attempt, so nothing is retried, and the call is refused by name and
# charged nothing. The outer retry_fetch's third word is `x`, not `--`. A reading that took its
# 300 for a bound with no `--` after it charged 1650 s and passed the step, and the reading
# before round 37's refusal read the gh call as retried with no bounded attempt.
echo "case: a retry_fetch bound with no -- after it bounds nothing"
root=$(fixture zc-retry-fetch-twice)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A retry of a retry with no separator after its bound
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch retry_fetch 300 x -- gh api repos/o/r
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call by name as outside the helper's signature" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'A retry of a retry with no separator after its bound' of job 'build' calls .retry_fetch. in a form other than its signature")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A function name with no body after it, as the text's last command, is a definition this check
# does not read, refused by name where a fetch is written on its line (dispatch://INFRA-1545,
# round 37). Bash refuses the line too, as a syntax error, and runs nothing on it. A reading that
# never asked about a name left pending at the end of the text read no definition and passed the
# step.
echo "case: a function name with no body as the last command is refused by name"
root=$(fixture zc-name-last)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A function name with no body as the last command
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; quiet()
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the name with no body by name at its line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'A function name with no body as the last command' of job 'build' defines a function with a body that is not a .{ … }. group")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A run of a script is read against the line its command starts on, where a substitution opens
# that command or an assignment before it (dispatch://INFRA-1545, round 37). Each step runs
# zc-c.sh, one retry_fetch at 300 s, 1650 s, three times, 4950 s, as bash does, which a key of
# 28 minutes does not hold. The `(done)` case pattern inside a substitution is the reading's own
# end of the loop, on the line the command starts on; a reading keyed to the line where the path
# or a later substitution sits put the run outside the loop, charged 1650 s and passed the step.
# That `(done)` is a loop's own word bash reads as a pattern, which round 37 refuses by name
# (dispatch://INFRA-1545, round 37), so each step is refused for it too, at its line.
echo "case: a run whose command a substitution opens is read against the command's first line"
root=$(fixture zc-subst-lines)
cat > "$root/.github/scripts/zc-c.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a script whose path a substitution opens
        timeout-minutes: 28
        run: |
          for pass in 1 2 3; do
            $(case a in (done) ;; esac
            echo .github/scripts)/zc-c.sh
          done
      - name: Run a script behind two assignments of substitutions
        timeout-minutes: 28
        run: |
          for pass in 1 2 3; do
            ZC_A=$(case a in (done) ;; esac) \
            ZC_B=$(true) .github/scripts/zc-c.sh
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "8	Run a script whose path a substitution opens" "15	Run a script behind two assignments of substitutions"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "charges '$step' three runs of zc-c.sh, 4950 s" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' carries .timeout-minutes: 28., 1680 s, and its own retries take 4950 s to exhaust")"
done
for line in 11 18; do
  check "refuses the (done) pattern by name at line $line" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '.*' of job 'build' writes .done. as a .case. pattern or an extglob alternative")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# A heredoc's consumer is the last command in front of its operator on that line, the one the
# shell hands the body to: `cat /dev/null; bash <<'BODY'` feeds it to bash, which runs the
# body's `retry_fetch` (bash runs one curl), so the step is refused by name
# (dispatch://INFRA-1545, round 37). A reading that took the line's first command, `cat`, for the
# consumer read the body as data and passed a fetch charged nothing.
echo "case: a heredoc fed to a shell after another command on its line is refused by name"
root=$(fixture zd-heredoc-last-command)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Feed a heredoc to bash after a cat
        run: |
          cat /dev/null; bash <<'BODY'
          . .github/scripts/ci-retry.sh
          retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          BODY
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the heredoc fed to bash, naming bash" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::step 'Feed a heredoc to bash after a cat' of job 'build' feeds a heredoc to .bash., and the body holds a fetch or names a script that fetches\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A consumer is named by the last part of its path, so `./cat/sh` is a shell under a directory
# named `cat`, not `cat`, and a heredoc fed to it is refused by name where its body fetches
# (dispatch://INFRA-1545, round 37): with a `cat/sh` that is a shell, bash runs the body's curl.
# A reading of the path's first part took `./cat/sh` for `cat` and read the body as data.
echo "case: a heredoc fed to a program under a directory named cat is refused by name"
root=$(fixture zd-heredoc-consumer-path)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Feed a heredoc to a shell under cat/
        run: |
          ./cat/sh <<'BODY'
          . .github/scripts/ci-retry.sh
          retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          BODY
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the heredoc fed to ./cat/sh, naming ./cat/sh" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::step 'Feed a heredoc to a shell under cat/' of job 'build' feeds a heredoc to .\./cat/sh., and the body holds a fetch or names a script that fetches\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A pipe on a heredoc's operator line hands the printed body to a shell only where a shell is
# what it pipes into: `cat <<BODY | tee out.txt` pipes into tee (dispatch://INFRA-1545, round 37).
# The step is refused for what it does write, an unquoted body whose command substitution the
# step's own shell runs (bash runs its curl), and the refusal does not say a shell reads the
# output. A shell pattern with an empty alternative read every pipe as one into a shell, and
# named that instead.
echo "case: a heredoc printed into tee is refused for its substitution, not for a pipe into a shell"
root=$(fixture zd-heredoc-pipe-tee)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Print a heredoc into tee
        run: |
          . .github/scripts/ci-retry.sh
          cat <<BODY | tee out.txt
          $(retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 https://example.com/a)
          BODY
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the unquoted heredoc for its command substitution" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Print a heredoc into tee' of job 'build' feeds a heredoc to .cat. unquoted, and the body holds a command substitution, which the step's own shell runs before that command reads the body, and the body holds a fetch or names a script that fetches\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A heredoc's refusal names its consumer by the command's first word, so `cat - <<'BODY' | bash`
# and `python3 - <<BODY` name `cat` and `python3`, never their `-` (dispatch://INFRA-1545, round
# 37). Bash runs one curl in each: the first prints the body into a shell, the second's
# unquoted body holds a command substitution the step's shell runs. A refusal naming the
# command's last word said the heredoc was fed to `-`.
echo "case: a heredoc's refusal names the consumer, not the consumer's last word"
root=$(fixture zd-heredoc-consumer-name)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Pipe a printed heredoc into bash
        run: |
          cat - <<'BODY' | bash
          . .github/scripts/ci-retry.sh
          retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          BODY
      - name: Feed Python an unquoted heredoc
        run: |
          . .github/scripts/ci-retry.sh
          python3 - <<BODY
          print("$(retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 https://example.com/a)")
          BODY
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the printed heredoc piped into bash, naming cat" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::step 'Pipe a printed heredoc into bash' of job 'build' feeds a heredoc to .cat., whose output a pipe on the operator's line hands to a shell, and the body holds a fetch or names a script that fetches\.")"
check "refuses the unquoted heredoc fed to Python, naming python3" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=16::step 'Feed Python an unquoted heredoc' of job 'build' feeds a heredoc to .python3. unquoted, and the body holds a command substitution")"
check "names no consumer -" "$(is "$(contains "$out" "feeds a heredoc to .-.")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# Rule 6 reads which function a Python line sits in heredoc by heredoc, in the order the script
# writes them (dispatch://INFRA-1545, round 37). check-ci-images.sh's first heredoc here is a
# `<<-` whose tab-indented lines bash strips, so python3 runs a bare urlopen at top level
# (checked with a python3 that records urlopen: one read of https://example.com/a), and it is
# refused; the read_retrying() its second heredoc defines is another program. Reading the
# heredocs last first carried that definition into the first heredoc and passed its read.
echo "case: a bare urlopen before the heredoc that defines read_retrying is refused"
root=$(fixture zd-python-heredoc-order)
{
  printf '#!/usr/bin/env bash\n'
  printf "python3 - <<-'PY'\n\timport urllib.request\n"
  printf '\turllib.request.urlopen("https://example.com/a", timeout=30)\nPY\n'
  printf "python3 - <<'PY'\nimport urllib.request\ndef read_retrying(url):\n"
  printf '    return urllib.request.urlopen(url, timeout=30)\nPY\n'
} > "$root/.github/scripts/check-ci-images.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the bare read of the first heredoc" \
  "$(contains "$out" "::error file=.github/scripts/check-ci-images.sh,line=4::this read goes straight to urllib.request\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A call of a function through a variable is refused by name whatever its arguments hold
# (dispatch://INFRA-1545, round 37). Here each call's argument is a two-line string whose first
# line ends in `]]x`; bash calls `fetch` twice, 2 x (5 x 300 + 150) = 3300 s, which the key of 28
# minutes, 1680 s, does not exceed. A reading that dropped the character before each newline in
# a double-quoted string read that argument as `]]`, skipped the command as an operand of a
# `[[ … ]]`, and passed the step at the 1650 s of the body's one written fetch.
echo "case: a call through a variable with a two-line quoted argument is refused by name"
root=$(fixture zd-variable-call-two-lines)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through a variable with a two-line argument
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=fetch
          "$fn" "]]x
          "
          "$fn" "]]x
          "
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call through \$fn by name, naming the fetch it can reach" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'Fetch through a variable with a two-line argument' of job 'build' calls a function through a variable, .\\\$fn., and a function this text defines or sources fetches — .curl. at .github/workflows/case.yaml:11\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A backslash that is a text's last character is a character of its word, as bash keeps it
# (dispatch://INFRA-1545, round 37). The `run: |-` below ends in `"$fn" ]]\`, which bash runs as
# a call of `fetch` with the argument `]]\`; with the call by name before it, bash calls `fetch`
# twice, 3300 s, above the key's 1680 s, and the call through `$fn` is refused by name. A reading
# that dropped that backslash read the argument as `]]`, skipped the command as an operand of a
# `[[ … ]]`, and passed the step at 1650 s.
echo "case: a call through a variable ending the text in a backslash is refused by name"
root=$(fixture zd-variable-call-last-backslash)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through a variable at the end of the text
        timeout-minutes: 28
        run: |-
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=fetch
          fetch
          "$fn" ]]\
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call through \$fn by name, naming the fetch it can reach" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=14::step 'Fetch through a variable at the end of the text' of job 'build' calls a function through a variable, .\\\$fn., and a function this text defines or sources fetches — .curl. at .github/workflows/case.yaml:11\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A command's offset is where it starts, which is what tells a call through a variable from a
# `case` pattern (dispatch://INFRA-1545, round 37). `( $in"" )` runs `fetch`, the value of a
# variable named `in`, in a subshell; bash makes the call twice, 3300 s, above the key's 1680 s,
# and it is refused by name. A reading that moved the command's offset to its last opening
# quote saw `$in` before it as the `in` of a `case` and the `""` up to the `)` as a pattern,
# skipped both calls, and passed the step at 1650 s.
echo "case: a call through a variable named in, inside a subshell, is refused by name"
root=$(fixture zd-variable-call-named-in)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through a variable named in
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          in=fetch
          ( $in"" )
          ( $in"" )
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call through \$in by name, naming the fetch it can reach" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'Fetch through a variable named in' of job 'build' calls a function through a variable, .\\\$in., and a function this text defines or sources fetches — .curl. at .github/workflows/case.yaml:11\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# The command after a `|` is what the pipe hands a script's path to, and an empty arithmetic
# `$(())` among that command's words opens no command of its own (dispatch://INFRA-1545, round
# 37). `echo .github/scripts/zd-a.sh | bash -s $(())` runs zd-a.sh in the shell reading the pipe
# (bash runs its curl once), so the path is refused by name as a pipe into a shell. A reading
# that took `$(())`'s inner `()` for a command read that command as the pipe's, read the path as
# an argument of `echo` whose output no shell runs, and charged and refused nothing.
echo "case: a script echoed into a shell whose words hold \$(()) is refused by name"
root=$(fixture zd-pipe-shell-arithmetic)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Echo a script into a shell reading stdin
        run: |
          echo .github/scripts/zd-a.sh | bash -s $(())
YAML
cat > "$root/.github/scripts/zd-a.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
SH
chmod +x "$root/.github/scripts/zd-a.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the path piped into a shell by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::step 'Echo a script into a shell reading stdin' of job 'build' names .github/scripts/zd-a.sh in a pipe into a shell, a place this check does not read as a call, and that script fetches")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A step that defines its own `retry_fetch` runs that definition at every call this check reads
# as the helper's, so the definition is refused by name, whatever the calls write
# (dispatch://INFRA-1545, round 37). Here the call is `retry_fetch curl …`, outside the helper's
# signature, which the step's own function runs: bash runs that curl (one call; up to five, each
# unbounded, against a host that never answers). A reading that skipped two words after
# `retry_fetch` read `-fsSL` as the command and passed the step with no fetch, and the reading
# before round 37's refusal read the curl as a call of the helper's.
echo "case: the command after a retry_fetch outside the helper's signature is read"
root=$(fixture zd-retry-fetch-unsigned)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through a retry_fetch of the step's own
        run: |
          retry_fetch() {
            for attempt in 1 2 3 4 5; do
              "$@" && return 0
              sleep 30
            done
            return 1
          }
          retry_fetch curl -fsSL -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the step's own retry_fetch by name at its definition" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::step 'Fetch through a retry_fetch of the step's own' of job 'build' defines a function named .retry_fetch., the helper's own name")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A step whose own retries reach its key is refused with the arithmetic and nothing about scripts
# it does not run (dispatch://INFRA-1545, round 37). The refusal names the scripts behind the
# figure only where the step runs one; this step runs none, so the sentence goes straight from
# the figures to the race. A reading that tested an unsorted generator for truth named an empty
# list of scripts here: "that of , which it runs".
echo "case: ze a step running no script is refused for its key with no list of scripts"
root=$(fixture ze-no-scripts)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  ze-no-scripts:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch the asset under a short key
        timeout-minutes: 27
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the key with the figures and no list of scripts" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml,line=31::step 'Fetch the asset under a short key' carries .timeout-minutes: 27., 1620 s, and its own retries take 1650 s to exhaust, which that budget does not strictly exceed\. A budget its own retries reach is a race")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A job key is priced by a `-> N min` arrow and by nothing else (dispatch://INFRA-1545, round 37).
# This job's block derives 1200 + 1050 = 2250 s -> 38 min, and its key is 20, which the block
# also writes, but as `>20 min`, a measurement and no arrow. The key of 20 minutes, 1200 s, is
# under the 2250 s the block derives, so the comment and the key disagree and the block is
# refused. A reading that took any `>` before a number of minutes for an arrow paired the key
# with `>20 min`, and passed a job key under its own stated sum.
echo "case: ze a job key matching a figure written after a bare > is not priced by it"
root=$(fixture ze-bare-arrow)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # Budget: 1200 s measured, plus its slowest retried step alone — the large fetch,
    # 1050 s. 1200 + 1050 = 2250 s -> 38 min, though the October runs took >20 min.
    timeout-minutes: 20
    steps:
      - name: The large fetch
        # 5 x 180 + 150 = 1050 s -> 18 min.
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the block, whose one arrow is not the key" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=6::this comment prices what it sits above at 38 min, and the .timeout-minutes: 20. below it, the key of job 'build', carries 20\.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A call of a library function after a reserved word other than `if`, `if !`, `!` or `elif !` is
# refused by name, and not counted (dispatch://INFRA-1545, round 37). `until ze_ready; do sleep 10;
# done` runs ze_ready once for every pass, as many passes as the service takes to come up, and
# this check reads no count there: one call is 5 x 60 + 150 = 450 s, under a key of 8 minutes
# (480 s), and the step waits for as many of them as there are passes. A reading that counted
# the call once, or skipped it unrefused, passed this step.
echo "case: ze a library function polled by until is refused by name"
root=$(fixture ze-until)
cat > "$root/.github/scripts/ze-poll-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
ze_ready() {
  retry_fetch "the readiness probe" 60 -- curl -fsSL --max-time 60 -o r https://example.com/ready
}
SH
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  ze-until:
    runs-on: ubuntu-24.04
    steps:
      - name: Poll until the service is ready
        timeout-minutes: 8
        run: |
          . .github/scripts/ze-poll-lib.sh
          until ze_ready; do sleep 10; done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call after until by name" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml,line=34::step 'Poll until the service is ready' of job 'ze-until' calls .ze_ready. after .until., a form this check does not read as a call of a function, and .ze_ready. fetches")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A name the text and a library both define is refused where the text's definition fetches,
# whichever definition bash runs (dispatch://INFRA-1545, round 37). Here bash runs the library's
# ze_fetch, read last, which runs its argument three times, and the step hands it ze_get:
# 3 x (5 x 300 + 150) = 4950 s. The text's own ze_fetch calls ze_get once, 1650 s, which a key of
# 28 minutes passes. The library holds no fetch, so its reading finds no fetching function, and a
# function calling through a variable joins those only where there is one; the call of a library
# function through a variable is refused only where the text does not define the name. So only
# the text's definition says the name fetches, and a reading that asked the library's definition
# alone passed this step at 1650 s.
echo "case: ze a name the text and a library define is refused where the text's definition fetches"
root=$(fixture ze-scope-twice)
cat > "$root/.github/scripts/ze-thrice.sh" <<'SH'
#!/usr/bin/env bash
ze_fetch() {
  "$1"
  "$1"
  "$1"
}
SH
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  ze-scope-twice:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through the library function
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          ze_get() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          ze_fetch() { ze_get; }
          . .github/scripts/ze-thrice.sh
          ze_fetch ze_get
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the two definitions of ze_fetch at the source" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml,line=36::step 'Fetch through the library function' of job 'ze-scope-twice' reaches two definitions of .ze_fetch., in this text and .github/scripts/ze-thrice.sh, and one of them fetches")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# The functions a step calls after sourcing a library are that library's, and a script the step
# runs defines none of them in the step's shell (dispatch://INFRA-1545, round 37). ze-run.sh
# defines a ze_get of its own and calls it, in its own process; the step then sources ze-lib.sh
# and calls ze_get twice, which runs ze-lib.sh's ze_get. So the refusal of the calls names
# ze-lib.sh, and nothing says the step reaches two definitions of ze_get. A reading that took the
# names a run script defines for the step's own named ze-run.sh as the library and refused two
# definitions that never meet.
echo "case: ze a function defined by a script the step runs is not read as the step's"
root=$(fixture ze-run-first)
cat > "$root/.github/scripts/ze-run.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
ze_get() {
  retry_fetch "the run's asset" 300 -- curl -fsSL --max-time 300 -o r https://example.com/r
}
ze_get
SH
chmod +x "$root/.github/scripts/ze-run.sh"
cat > "$root/.github/scripts/ze-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
ze_get() {
  retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
}
SH
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  ze-run-first:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the script, then fetch twice through the library
        timeout-minutes: 56
        run: |
          .github/scripts/ze-run.sh
          . .github/scripts/ze-lib.sh
          ze_get
          ze_get
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the calls, naming the library that defines ze_get" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml,line=35::step 'Run the script, then fetch twice through the library' of job 'ze-run-first' calls .ze_get., which .github/scripts/ze-lib.sh defines and which fetch, more than once")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A library reached from an action's step and from a script the action runs is priced by the
# action's own DECLARED_LIBRARIES entry where both carry one (dispatch://INFRA-1545, round 37).
# ze-act's first step calls ze_get three times after sourcing ze-lib.sh, and its second runs
# ze-twice.sh, which calls it twice: 5 x (5 x 300 + 150) = 8250 s, which the action's entry
# writes, and ze-twice.sh's entry writes its own 3300 s. The step using ze-act is charged the
# action's figure, which its key of 120 minutes (7200 s) does not exceed. A reading that took
# the script's entry first priced the library at 3300 s in each shell, 6600 s, and passed a key
# under the 8250 s the step waits for.
echo "case: ze a library an action and its script both declare is priced by the action's entry"
root=$(fixture ze-action-script-entries)
mkdir -p "$root/.github/actions/ze-act"
cat > "$root/.github/scripts/ze-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
ze_get() {
  retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
}
SH
cat > "$root/.github/scripts/ze-twice.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ze-lib.sh
ze_get
ze_get
SH
chmod +x "$root/.github/scripts/ze-twice.sh"
cat > "$root/.github/actions/ze-act/action.yml" <<'YAML'
name: Fetch through the library five times
description: Calls the library's function three times, and runs a script that calls it twice.
runs:
  using: composite
  steps:
    - name: Fetch three times through the library
      shell: bash
      run: |
        . .github/scripts/ze-lib.sh
        ze_get
        ze_get
        ze_get
    - name: Run the script that fetches twice
      shell: bash
      run: .github/scripts/ze-twice.sh
YAML
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  ze-action-script-entries:
    runs-on: ubuntu-24.04
    steps:
      - name: Use the action
        timeout-minutes: 120
        uses: ./.github/actions/ze-act
YAML
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/actions/ze-act/action.yml", None, ".github/scripts/ze-lib.sh", 8250, "three calls of ze_get from the first step and two from ze-twice.sh, which the second runs: 5 x (5 x 300 + 150) = 8250 s"),'
declare_in "$root" DECLARED_LIBRARIES '    DeclaredLibrary(".github/scripts/ze-twice.sh", None, ".github/scripts/ze-lib.sh", 3300, "two calls of ze_get: 2 x (5 x 300 + 150) = 3300 s"),'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the step's key of 120 minutes" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml,line=31::step 'Use the action' carries .timeout-minutes: 120., 7200 s, and its own retries take")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A derivation is `<measured> + <step> = <total> s -> <key> min`, and a sum whose arrow writes no
# `min` after its figure is no derivation (dispatch://INFRA-1545, round 37). The block states
# 1200 + 1050 = 2250 s -> 37 min, which the key of 37 minutes, 2220 s, does not exceed, and then
# an older sum, 600 + 600 = 1200 s -> 37, with no unit after its arrow. The last derivation naming
# the key is the one read, so a reading that dropped the `min` read the older sum, 600 s measured,
# found 600 + 1050 = 1650 s under the key, and passed it.
echo "case: a sum whose arrow writes no min is no derivation, and the one before it is read"
root=$(fixture zf-derivation-without-min)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    # Budget: 1200 + 1050 = 2250 s -> 37 min. Round 12 wrote 600 + 600 = 1200 s -> 37 here.
    runs-on: ubuntu-24.04
    timeout-minutes: 37
    steps:
      - name: The large fetch
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the key under the sum its derivation states, 2250 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=7::job 'build' carries .timeout-minutes: 37., 2220 s, and its own comment derives 1200 + 1050 = 2250 s, which that key does not strictly exceed")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# The libraries a body reaches are the ones it sources and the ones those source in turn, all of
# them and only those (dispatch://INFRA-1545, round 37). zf-fetch-get.sh defines zf_get, one
# retry_fetch at 300 s, 1650 s. The first step sources zf-runs-then-sources.sh, which RUNS
# zf-quiet-get.sh, whose own zf_get fetches nothing, in a shell of its own, and then sources
# zf-fetch-get.sh; the second sources zf-sources-two.sh, which sources zf-quiet.sh and then
# zf-fetch-get.sh. Each step then calls zf_get twice, and bash runs the fetching definition both
# times, 3300 s, over a key of 28 minutes, 1680 s. Both are refused by name. A reading that took a
# library's runs for sources read zf_get as the quiet script's, and one that stopped at the first
# source never saw zf-fetch-get.sh: each charged the library once, 1650 s, and passed its step.
echo "case: a library's fetching function called twice is refused, behind a run of a namesake or a second source"
root=$(fixture zf-library-reach)
cat > "$root/.github/scripts/zf-quiet-get.sh" <<'SH'
#!/usr/bin/env bash
zf_get() {
  :
}
SH
chmod +x "$root/.github/scripts/zf-quiet-get.sh"
cat > "$root/.github/scripts/zf-fetch-get.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
zf_get() {
  retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
}
SH
cat > "$root/.github/scripts/zf-runs-then-sources.sh" <<'SH'
#!/usr/bin/env bash
.github/scripts/zf-quiet-get.sh
. .github/scripts/zf-fetch-get.sh
SH
cat > "$root/.github/scripts/zf-quiet.sh" <<'SH'
#!/usr/bin/env bash
zf_note() {
  :
}
SH
cat > "$root/.github/scripts/zf-sources-two.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/zf-quiet.sh
. .github/scripts/zf-fetch-get.sh
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call a library's fetch twice after a run of a quiet namesake
        timeout-minutes: 28
        run: |
          . .github/scripts/zf-runs-then-sources.sh
          zf_get
          zf_get
      - name: Call the second sourced library's fetch twice
        timeout-minutes: 28
        run: |
          . .github/scripts/zf-sources-two.sh
          zf_get
          zf_get
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "11	Call a library's fetch twice after a run of a quiet namesake" \
  "17	Call the second sourced library's fetch twice"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses the two calls of zf_get in '$step' by name" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' calls .zf_get., which .github/scripts/zf-fetch-get.sh defines and which fetch, more than once")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A library function that calls a function through a variable runs whatever its caller hands it,
# and so does every function of the library that calls it in turn, however many calls away
# (dispatch://INFRA-1545, round 37). zf-run.sh's zf_run calls `"$1"`, zf_twice calls zf_run twice
# and zf_four calls zf_twice twice. The step hands zf_four its zf_fetch, one retry_fetch at 300 s,
# which bash runs four times, 6600 s, over a key of 28 minutes, 1680 s; this check charges
# zf_fetch once, as a function the step defines and never calls by name, 1650 s, and the refusal
# naming zf_four and its variable stands for the rest. A reading that found no caller of a
# caller, or none past the first in a library, passed the step; one that took the library's path
# for the variable's word named `.github/scripts/zf-run.sh` as the variable.
echo "case: a library function two calls above a call through a variable is refused by name, with that variable"
root=$(fixture zf-variable-holders)
cat > "$root/.github/scripts/zf-run.sh" <<'SH'
#!/usr/bin/env bash
zf_run() {
  "$1"
}
zf_twice() {
  zf_run "$1"
  zf_run "$1"
}
zf_four() {
  zf_twice "$1"
  zf_twice "$1"
}
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Hand a fetch to a library function two calls above a call through a variable
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          . .github/scripts/zf-run.sh
          zf_fetch() {
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          }
          zf_four zf_fetch
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call of zf_four by name, with its variable \$1" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=15::step 'Hand a fetch to a library function two calls above a call through a variable' of job 'build' calls .zf_four., which .github/scripts/zf-run.sh defines and which calls a function through a variable, .\\\$1., and a function this text defines or sources fetches")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A key line is `timeout-minutes: <n>` with nothing after the number but blanks or a comment that
# a blank opens, as YAML reads one; `timeout-minutes: 20#keep` is text YAML reads as a string,
# and here it is a line of a heredoc (dispatch://INFRA-1545, round 37). The job runs no retried
# step, and its block states 1200 + 0 = 1200 s -> 20 min over its key of 20 minutes, 1200 s, which
# does not exceed that sum. A reading that took the heredoc's line for the key the block prices
# paired the block with it, left the job's own key unpaired, read no derivation for it and passed
# it.
echo "case: a heredoc line with a comment and no blank before it is no key a block prices"
root=$(fixture zf-keyline-comment-no-blank)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  quiet:
    # Budget: 1200 s measured, and no retried step: 1200 + 0 = 1200 s -> 20 min.
    runs-on: ubuntu-24.04
    steps:
      - name: Write a config
        run: |
          cat > cfg.yaml <<'EOF'
          timeout-minutes: 20#keep
          EOF
    timeout-minutes: 20
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the job's key at the sum its derivation states, 1200 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::job 'quiet' carries .timeout-minutes: 20., 1200 s, and its own comment derives 1200 + 0 = 1200 s, which that key does not strictly exceed")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A `timeout-minutes` key at the start of a line, with no blank before it, is a key line as well,
# and a block's figure that disagrees with it is refused (dispatch://INFRA-1545, round 37). Here
# it is a top-level key under the file's header block, which states 30 minutes over a key of 20.
# GitHub takes no `timeout-minutes` at the top of a workflow, so this key bounds no job; the case
# pins the reading of the key line, which a reading that asked for a blank before the key did not
# make, passing the disagreement.
echo "case: a key line with no blank before it is read, and a figure that disagrees with it refused"
root=$(fixture zf-top-level-key)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
# The nightly run -> 30 min.
name: Case
timeout-minutes: 20
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Echo
        run: echo hi
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the header's 30 minutes over the key of 20" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=1::this comment prices what it sits above at 30 min, and the .timeout-minutes: 20. below it carries 20")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# THE CASES OF ROUND 37'S LAST PASS: each input on which the gate at 9813ba64 was wrong, closed by
# a refusal by name, or, where the gate misread a form it lists, by the correction of that
# reading (dispatch://INFRA-1545, round 37). Each case names its finding of the round's list:
# B1-B4, A1-A4, F1-F4, D1-D7 and C2-C10. Each fails on the gate at 9813ba64, and fails again
# with its refusal or its correction taken out of this gate.

# B1. An alias can open a loop, which this check does not read: bash expands one once
# `shopt -s expand_aliases` is set, and sh always does. `nice` is a wrapper this check strips, so
# `nice curl …; done` read as one curl, 8 x 300 + 127 = 2527 s under a key of 43 minutes (2580 s),
# where the shell runs it three times, 7581 s. The alias is refused where the text defining it
# fetches, and where it is defined in a library, at the source of that library.
echo "case: r37 B1 an alias is refused by name, in the text and in a library it sources"
root=$(fixture r37-alias)
cat > "$root/.github/scripts/r37-aliases.sh" <<'SH'
#!/usr/bin/env bash
alias nice='for pass in 1 2 3; do'
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through an alias that opens a loop
        timeout-minutes: 43
        run: |
          shopt -s expand_aliases
          alias nice='for pass in 1 2 3; do'
          nice curl -fsSL --retry 7 --retry-all-errors --max-time 300 -o a https://example.com/a; done
      - name: Fetch after sourcing a library that defines an alias
        timeout-minutes: 43
        run: |
          shopt -s expand_aliases
          . .github/scripts/r37-aliases.sh
          nice curl -fsSL --retry 7 --retry-all-errors --max-time 300 -o a https://example.com/a; done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the alias by name at its line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Fetch through an alias that opens a loop' of job 'build' defines an alias, a form this check does not read")"
check "refuses the source of a library defining an alias at its line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=17::step 'Fetch after sourcing a library that defines an alias' of job 'build' sources .github/scripts/r37-aliases.sh, which defines an alias")"
# Round 41 refuses `shopt -s expand_aliases` by name, an option that makes an alias's body run in
# place of the command word, so each step's line 10 and 16 is refused too (dispatch://INFRA-1545,
# round 41).
check "refuses each step's expand_aliases by name" \
  "$(is "$(printf '%s\n' "$out" | grep -cE '^::error file=.github/workflows/case.yaml,line=(10|16)::step .* sets .expand_aliases. \(.shopt -s expand_aliases.\)')" 2)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# B2. A site an exception leaves unretried is priced at the floor its own options make, and a
# curl writing no `--max-time` has no figure: the step holding it was compared with nothing, so
# the 2527 s curl beside it passed under a key of 5 minutes. The unbounded curl is refused.
echo "case: r37 B2 an excepted curl with no --max-time is refused for its unpriced attempt"
root=$(fixture r37-excepted-unbounded)
cat >> "$root/.github/workflows/release.yaml" <<'YAML'
      - name: Fetch the notes with no bound beside a retried asset
        timeout-minutes: 5
        run: |
          curl -fsSL -o notes https://example.com/notes
          curl -fsSL --retry 7 --retry-all-errors --max-time 300 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the unbounded excepted curl at its line" \
  "$(contains "$out" "::error file=.github/workflows/release.yaml,line=23::.curl. in step 'Fetch the notes with no bound beside a retried asset' is a site an exception above leaves unretried, and it writes no .--max-time.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# B3. An excepted curl writing `--max-time` twice was priced at the first, 8 x 30 + 127 = 367 s
# under a key of 7 minutes, and curl takes the last, 2527 s. It is refused by name as any other
# curl writing two bounds is.
echo "case: r37 B3 an excepted curl writing two bounds is refused by name"
root=$(fixture r37-excepted-two-bounds)
cat >> "$root/.github/workflows/release.yaml" <<'YAML'
      - name: Fetch the asset under two bounds
        timeout-minutes: 7
        run: |
          curl -fsSL --retry 7 --retry-all-errors --max-time 30 --max-time 300 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the two bounds at the curl's line" \
  "$(contains "$out" "::error file=.github/workflows/release.yaml,line=23::.curl. in step 'Fetch the asset under two bounds' is a site an exception above leaves unretried, and it writes its per-attempt bound 2 times, .30. and .300., and curl takes the last one")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# B4. A library's function calls, by name, a function the sourcing text defines: bash runs the
# text's r37_get three times, 3 x 1650 = 4950 s, and the library's own reading saw no fetch, so
# the step was charged its one definition, 1650 s, under a key of 28 minutes (1680 s).
echo "case: r37 B4 a library calling a fetching function it does not define is refused at the source"
root=$(fixture r37-library-calls-text)
cat > "$root/.github/scripts/r37-thrice.sh" <<'SH'
#!/usr/bin/env bash
r37_fetch() { r37_get; r37_get; r37_get; }
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call a library function that calls the step's fetch
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          r37_get() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          . .github/scripts/r37-thrice.sh
          r37_fetch
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the source of the library at its line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Call a library function that calls the step's fetch' of job 'build' sources .github/scripts/r37-thrice.sh, and .github/scripts/r37-thrice.sh calls .r37_get. in its function .r37_fetch.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A1. A library one shell sources twice runs its own commands twice: r37-lib.sh's top-level
# retry_fetch at 180 s, 2 x 1050 = 2100 s, which a key of 18 minutes (1080 s) does not hold. Each
# step, the one sourcing it twice and the one running a script that does, is refused as reaching
# the library along more than one path.
echo "case: r37 A1 a library one shell sources twice is refused as reached along two paths"
root=$(fixture r37-library-twice)
cat > "$root/.github/scripts/r37-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/scripts/r37-twice.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/r37-lib.sh
. .github/scripts/r37-lib.sh
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source the library twice
        timeout-minutes: 18
        run: |
          . .github/scripts/r37-lib.sh
          . .github/scripts/r37-lib.sh
      - name: Run a script that sources the library twice
        timeout-minutes: 18
        run: .github/scripts/r37-twice.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "8	Source the library twice" "13	Run a script that sources the library twice"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' at its key for the library's two paths" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' reaches .github/scripts/r37-lib.sh, a library that fetches, along more than one path")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# A1's class search. A script that sources a library and that one step runs in more than one
# place is a shell of its own at each run, each of which can call the library's functions, and
# the library is charged once for the step (dispatch://INFRA-1545, round 37). zz-lib.sh defines
# zz_fetch, one `retry_fetch` at 180 s, 1050 s, and zz-a.sh sources it and calls zz_fetch once.
# The step running zz-a.sh on two lines waits 2100 s, and the gate at b5aba1e5 charged it 1050 s,
# since both runs give one path, and passed it under a key of 30 minutes, while the same two
# runs in a `for pass in 1 2` were charged 2100 s. So was a composite action whose step runs
# zz-a.sh twice. Each is refused by name, at the step and at the action, and a DECLARED_LIBRARIES
# entry writing 2100 s replaces the charge, so a key of 30 minutes is refused and one of 36
# passes. The step running zz-a.sh once and the loop of two passes are no such shape, and pass.
echo "case: r37 a script that sources a library, run twice by one step or one action step, is refused unless declared"
root=$(fixture r37-library-script-twice)
mkdir -p "$root/.github/actions/zz-twice"
cat > "$root/.github/scripts/zz-lib.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
zz_fetch() {
  retry_fetch "the index" 180 -- gh api repos/o/r
}
SH
cat > "$root/.github/scripts/zz-a.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/zz-lib.sh
zz_fetch
SH
chmod +x "$root/.github/scripts/zz-a.sh"
cat > "$root/.github/actions/zz-twice/action.yml" <<'YAML'
name: Run the caller twice
description: Runs a script that sources the library, twice.
runs:
  using: composite
  steps:
    - name: Run the caller twice
      shell: bash
      run: |
        .github/scripts/zz-a.sh
        .github/scripts/zz-a.sh
YAML
write_twice_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the caller twice
        timeout-minutes: $1
        run: |
          .github/scripts/zz-a.sh
          .github/scripts/zz-a.sh
      - name: Run the caller once
        timeout-minutes: 18
        run: .github/scripts/zz-a.sh
      - name: Run the caller in a loop of two passes
        timeout-minutes: 36
        run: |
          for pass in 1 2; do
            .github/scripts/zz-a.sh
          done
      - name: Use the action that runs the caller twice
        uses: ./.github/actions/zz-twice
        timeout-minutes: 36
YAML
}
write_twice_case 30
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the step by name for the library it reaches along one path twice" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Run the caller twice' reaches .github/scripts/zz-lib.sh, a library that fetches, along one path more than once, .github/scripts/zz-a.sh to .github/scripts/zz-lib.sh: it runs a script on that path, or sources one, in more than one place")"
check "refuses the action by name for the library its step reaches along one path twice" \
  "$(contains "$out" "::error file=.github/actions/zz-twice/action.yml,line=1::.github/actions/zz-twice/action.yml reaches .github/scripts/zz-lib.sh, a library that fetches, along one path more than once, .github/scripts/zz-a.sh to .github/scripts/zz-lib.sh")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"
twice_why='two runs of zz-a.sh, each one call of zz_fetch: 2 x (5 x 180 + 150) = 2100 s'
declare_in "$root" DECLARED_LIBRARIES "    DeclaredLibrary(\".github/workflows/case.yaml\", \"Run the caller twice\", \".github/scripts/zz-lib.sh\", 2100, \"$twice_why\"),"
declare_in "$root" DECLARED_LIBRARIES "    DeclaredLibrary(\".github/actions/zz-twice/action.yml\", None, \".github/scripts/zz-lib.sh\", 2100, \"$twice_why\"),"
run_check "$root"
check "with the library declared, charges the step the declared 2100 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Run the caller twice' carries .timeout-minutes: 30., 1800 s, and its own retries take 2100 s to exhaust")"
check "with the library declared, refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
write_twice_case 36
run_check "$root"
check "with the library declared and a key above 2100 s, passes" "$(is "$status" 0)"

# A2. A `}` written as an argument closed the function's body in this check's reading, so the
# fetch after it was read as the text's, charged once, 1650 s, under a key of 28 minutes, where
# two calls of the function run it twice, 3300 s. The brace is refused by name.
echo "case: r37 A2 a brace bash reads as a word is refused by name"
root=$(fixture r37-brace-word)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A function that writes a closing brace and then fetches, called twice
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          r37_write() {
            echo '}' >> out.json
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          }
          r37_write
          r37_write
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the quoted brace by name at its line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'A function that writes a closing brace and then fetches, called twice' of job 'build' writes .}. where bash reads it as a word of its command")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A2's correction. A parameter expansion is one word to its closing `}`, as bash reads one that
# removes all of `$1` up to and including its last blank: this check split it at its blank and
# took its `}` for the brace closing the function, so the fetch after it was charged once, 1650 s,
# where two calls run it twice, 3300 s, over a key of 28 minutes. Read whole, the function's body
# runs to its own `}`.
echo "case: r37 A2 a parameter expansion holding a blank is one word, and the body runs on"
root=$(fixture r37-expansion-word)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A function whose expansion holds a blank, called twice
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          r37_last() {
            last=${1##* }
            retry_fetch "the $last" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          }
          r37_last "one asset"
          r37_last "two assets"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "charges both calls of the function, 3300 s" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'A function whose expansion holds a blank, called twice' carries .timeout-minutes: 28., 1680 s, and its own retries take 3300 s to exhaust")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# A3. A call of a library's fetching function inside a function called after `then`, or defined
# with the `function` keyword, was neither a fetch nor a script call in the reach of that shape,
# so the shape passed and the library's fetch was charged once, 1650 s, where two calls run it
# twice. Each shape is refused by name.
echo "case: r37 A3 a library's fetch in the reach of an odd call or definition is refused"
root=$(fixture r37-library-reach)
cat > "$root/.github/scripts/r37-get.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
r37_get() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call a library's fetch twice through a function called after then
        timeout-minutes: 28
        run: |
          . .github/scripts/r37-get.sh
          r37_twice() {
            r37_get
          }
          if true; then r37_twice; fi
          if true; then r37_twice; fi
      - name: Call a library's fetch twice through a function defined with the keyword
        timeout-minutes: 28
        run: |
          . .github/scripts/r37-get.sh
          function r37_twice {
            r37_get
          }
          r37_twice
          r37_twice
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for line in 14 15; do
  check "refuses the call after then at line $line" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step 'Call a library's fetch twice through a function called after then' of job 'build' calls .r37_twice. after .then.")"
done
check "refuses the keyword definition at its line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=20::step 'Call a library's fetch twice through a function defined with the keyword' of job 'build' defines a function with the .function. keyword")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# A4. A function's lines end at its body's last command, so a run after a `}` that starts its
# line is the text's own, charged once, 1050 s, under a key of 18 minutes: the gate at 9813ba64
# read it as the function's, and refused the call after `then` saying the function fetches,
# which it does not. A command after the `}` on a line the body holds a command of is still
# read as the function's, and is refused by name.
echo "case: r37 A4 a command after a function's closing brace is the text's, or refused by name"
root=$(fixture r37-after-close)
cat > "$root/.github/scripts/r37-c.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a script after a closing brace that starts its line
        timeout-minutes: 18
        run: |
          r37_quiet() {
            :
          }; .github/scripts/r37-c.sh
          if true; then r37_quiet; fi
      - name: Run a script after a closing brace on a line of the body
        timeout-minutes: 18
        run: |
          r37_hush() {
            :; }; .github/scripts/r37-c.sh
          r37_hush
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "does not say the quiet function fetches" "$(is "$(contains "$out" 'r37_quiet. fetches')" false)"
check "refuses the command after the brace on the body's line" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=18::step 'Run a script after a closing brace on a line of the body' of job 'build' writes a command after the .}. that closes .r37_hush.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# F1. A script whose name holds an underscore is no name SCRIPT_CALL reads, so the step running
# it was charged nothing for its 1050 s under a key of 1 minute. The script is refused by name.
echo "case: r37 F1 a fetching script whose name this check cannot find is refused"
root=$(fixture r37-script-name)
cat > "$root/.github/scripts/r37_fetch_tools.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a script whose name holds an underscore
        timeout-minutes: 1
        run: .github/scripts/r37_fetch_tools.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the script by name" \
  "$(contains "$out" "::error file=.github/scripts/r37_fetch_tools.sh,line=1::.github/scripts/r37_fetch_tools.sh is a script under .github/scripts/ that fetches, and its name is one this check finds no call of")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# F1 holds a .sh name the call pattern misses. The four cases below are files under .github/scripts
# that the gate's own file list leaves out: a name with no .sh suffix, a .bash suffix, and a .sh
# script in a subdirectory. A step runs each one in a command word, as F1's step does. Each passed
# the gate at 0b924e3b928 under a key below what it waits for, and a bare curl in such a file
# passed with no floor refusal (dispatch://INFRA-1545, the tester's round-37 battery, W1, W2, W6
# and W9). Each case asks for a refusal that names the file, in whatever words the gate chooses.
echo "case: r37 W1 a fetching script with no .sh suffix is not passed under its retries"
root=$(fixture r37-w1-no-suffix)
cat > "$root/.github/scripts/r37-fetch-tools" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/r37-fetch-tools"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a script whose name has no suffix
        timeout-minutes: 17
        run: .github/scripts/r37-fetch-tools
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "names the script" "$(contains "$out" 'r37-fetch-tools')"

echo "case: r37 W2 a fetching script with a .bash suffix is not passed under its retries"
root=$(fixture r37-w2-bash-suffix)
cp "$work/r37-w1-no-suffix/.github/scripts/r37-fetch-tools" "$root/.github/scripts/r37-fetch-tools.bash"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a script whose name ends in .bash
        timeout-minutes: 17
        run: .github/scripts/r37-fetch-tools.bash
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "names the script" "$(contains "$out" 'r37-fetch-tools.bash')"

echo "case: r37 W6 a bare curl in a script in a subdirectory of .github/scripts is refused"
root=$(fixture r37-w6-subdirectory)
mkdir -p "$root/.github/scripts/lib"
cat > "$root/.github/scripts/lib/r37-bare.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
curl -fsSL -o b https://example.com/releases/download/v1/b
SH
chmod +x "$root/.github/scripts/lib/r37-bare.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a script in a subdirectory
        timeout-minutes: 5
        run: .github/scripts/lib/r37-bare.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "names the script" "$(contains "$out" 'lib/r37-bare.sh')"

# The subdirectory script below waits for two ladders, 2100 s, and a top-level script of the same
# name waits for one; the key, 34 minutes, 2040 s, is under the two.
echo "case: r37 W9 a script in a subdirectory is not charged as its top-level namesake"
root=$(fixture r37-w9-namesake)
mkdir -p "$root/.github/scripts/lib"
cat > "$root/.github/scripts/r37-namesake.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/scripts/lib/r37-namesake.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
retry_fetch "a second asset" 180 -- curl -fsSL --max-time 180 -o c https://example.com/c
SH
chmod +x "$root/.github/scripts/r37-namesake.sh" "$root/.github/scripts/lib/r37-namesake.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the subdirectory script
        timeout-minutes: 34
        run: .github/scripts/lib/r37-namesake.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "names the subdirectory script" "$(contains "$out" 'lib/r37-namesake.sh')"

# T1 and T3 below copy this fixture's r38-fetch.sh, a CI script that retries at 180 s.
root=$(fixture r38-x2-python-runs-script)
cat > "$root/.github/scripts/r38-fetch.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/r38-fetch.sh"

# T1 and T3. A bracket expression whose range runs backwards, such as `[z-c]`, is a word bash
# accepts and matches no file with. The gate turned it into a Python regular expression and
# stopped with re.PatternError and a traceback at e6de07881cdf, in pattern_words, on an argument
# of `ls` and on a command word (dispatch://INFRA-1545, the tester's round-38 battery). Each case
# asks for a refusal by name with no traceback.
echo "case: r38 T1 an argument holding a backwards bracket range is refused by name, not with a traceback"
root=$(fixture r38-t1-backwards-range-argument)
cp "$work/r38-x2-python-runs-script/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: List the scripts a backwards range names
        timeout-minutes: 1
        run: ls .github/scripts/[z-c]*.sh || true
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses by name" "$(contains "$out" '^::error file=.github/workflows/case.yaml,line=')"

echo "case: r38 T3 a command word holding a backwards bracket range is refused by name, not with a traceback"
root=$(fixture r38-t3-backwards-range-command)
cp "$work/r38-x2-python-runs-script/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a script a backwards range names
        timeout-minutes: 1
        run: .github/scripts/r38-[z-c]etch.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses by name" "$(contains "$out" '^::error file=.github/workflows/case.yaml,line=')"

# T4 to T7. A bracket expression bash matches, but the gate's pattern does not: a POSIX class
# (`[[:alpha:]]`), a `]` written first in the class (`[]t]`), a `]` escaped in it (`[t\]]`) and
# a negated `]` (`[!]]`). bash expands each of the words below to .github/scripts/r38-fetch.sh,
# which retries at 180 s (1050 s). At 98832fe87a80 each step passed under a 17-minute key, 1020 s,
# where the same words spelled `[!x]` are refused (dispatch://INFRA-1545, the tester's round-39
# battery, B02 to B17). Each word is literal text a static reader sees, not a path built at run
# time. Each case asks only that the gate exits 1 with no traceback, so a refusal by name or a
# charged figure both pass it.
for r39_case in "T4 a POSIX class:[[:alpha:]]" "T5 a ] written first:[]t]" "T6 an escaped ]:[t\\]]" "T7 a negated ]:[!]]"; do
  r39_id=${r39_case%% *}
  r39_what=${r39_case#* }
  r39_what=${r39_what%%:*}
  r39_class=${r39_case#*:}
  echo "case: r39 $r39_id a run after a cd whose word holds $r39_what is not passed under its retries"
  root=$(fixture "r39-$r39_id-run-after-cd")
  cp "$work/r38-x2-python-runs-script/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-fetch.sh"
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the fetching script through a bracket expression
        timeout-minutes: 17
        run: cd .github/scripts && ./r38-fe${r39_class}ch.sh
YAML
  run_check "$root"
  check "exits 1" "$(is "$status" 1)"
  check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

  echo "case: r39 $r39_id an argument of flock whose word holds $r39_what is not passed under its retries"
  root=$(fixture "r39-$r39_id-flock-argument")
  cp "$work/r38-x2-python-runs-script/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-fetch.sh"
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the fetching script under flock through a bracket expression
        timeout-minutes: 17
        run: flock /tmp/l .github/scripts/r38-fe${r39_class}ch.sh
YAML
  run_check "$root"
  check "exits 1" "$(is "$status" 1)"
  check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
done

# T8. A glob bash matches under `shopt -s nocaseglob`, which the gate's pattern, matched with
# case, does not. With that option set, bash expands `R38-FE[T]CH.SH` to r38-fetch.sh, which
# retries at 180 s (1050 s); without it the word names no file, and flock fails. At 046b589eb4ca
# each step passed under a 17-minute key, 1020 s, while the same words in a run's place, or
# after bash, are refused (dispatch://INFRA-1545, the tester's round-40 battery, D37 to D40).
# The option is literal text a static reader sees. Each case asks only that the gate exits 1
# with no traceback, so a refusal by name or a charged figure both pass it.
echo "case: r40 T8 a run after a cd whose word matches only under nocaseglob is not passed under its retries"
root=$(fixture r40-T8-run-after-cd)
cp "$work/r38-x2-python-runs-script/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the fetching script through a glob matched without case
        timeout-minutes: 17
        run: |
          shopt -s nocaseglob
          cd .github/scripts && ./R38-FE[T]CH.SH
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: r40 T8 an argument of flock whose word matches only under nocaseglob is not passed under its retries"
root=$(fixture r40-T8-flock-argument)
cp "$work/r38-x2-python-runs-script/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the fetching script under flock through a glob matched without case
        timeout-minutes: 17
        run: |
          shopt -s nocaseglob
          flock /tmp/l .github/scripts/R38-FE[T]CH.SH
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

# T9 to T12. Four ways a step sets nocaseglob that the gate does not read, so the T8 word
# `R38-FE[T]CH.SH` names .github/scripts/r38-fetch.sh, which retries at 180 s (1050 s), and the
# step passes under a 17-minute key, 1020 s (dispatch://INFRA-1545, the tester's round-41 drive at
# 1a3d8b825fc9). T9: `-O nocaseglob` after env written by its path, where `env bash -O` is
# refused. T10: the setting in a here-string that `.` sources from /dev/stdin, where `bash <<<` is
# refused. T11: BASHOPTS written to $GITHUB_ENV in a heredoc, which the runner hands to the next
# step's bash, where `echo BASHOPTS=… >> "$GITHUB_ENV"` is refused. T12: BASH_ENV naming a file
# the step writes, which bash sources before the step's text, where `bash <file written>` is
# refused. A runner emulation (bash --noprofile --norc -eo pipefail, with the step's env and the
# GITHUB_ENV lines carried) runs the script in each. Each case asks only that the gate exits 1
# with no traceback, so a refusal by name or a charged figure both pass it.
r41_steps=(
  "T9 an interpreter started by env written by its path|      - name: Run it under bash -O nocaseglob through /usr/bin/env
        timeout-minutes: 17
        run: /usr/bin/env bash -O nocaseglob -c 'flock /tmp/l .github/scripts/R38-FE[T]CH.SH'"
  "T10 a setting in a here-string a shell sources|      - name: Source the setting from a here-string, then run it
        timeout-minutes: 17
        run: |
          . /dev/stdin <<<'shopt -s nocaseglob'
          flock /tmp/l .github/scripts/R38-FE[T]CH.SH"
  "T11 BASHOPTS written to GITHUB_ENV in a heredoc|      - name: Write the setting for the next step
        timeout-minutes: 1
        run: |
          cat >> \"\$GITHUB_ENV\" <<'EOF'
          BASHOPTS=nocaseglob
          EOF
      - name: Run it under the setting the last step wrote
        timeout-minutes: 17
        run: flock /tmp/l .github/scripts/R38-FE[T]CH.SH"
  "T12 BASH_ENV naming a file the step writes|      - name: Write the setting into a startup file
        timeout-minutes: 1
        run: printf 'shopt -s nocaseglob\\n' > \"\$RUNNER_TEMP/settings\"
      - name: Run it under that startup file
        timeout-minutes: 17
        env:
          BASH_ENV: \${{ runner.temp }}/settings
        run: flock /tmp/l .github/scripts/R38-FE[T]CH.SH"
)
for r41_case in "${r41_steps[@]}"; do
  r41_name=${r41_case%%|*}
  r41_id=${r41_name%% *}
  echo "case: r41 $r41_id a fetching script named under nocaseglob, set by ${r41_name#* }, is not passed under its retries"
  root=$(fixture "r41-$r41_id")
  cp "$work/r38-x2-python-runs-script/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-fetch.sh"
  printf '%s\n' "name: Case" "on: [push]" "jobs:" "  build:" "    runs-on: ubuntu-24.04" "    steps:" \
    "${r41_case#*|}" > "$root/.github/workflows/case.yaml"
  run_check "$root"
  check "exits 1" "$(is "$status" 1)"
  check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
done

# R42. Round 42 closes the four routes T9 to T12 took as routes, not as spellings
# (dispatch://INFRA-1545, round 42), and each case below is a sibling of the tester's shapes that
# the route's rule implies, each running r38-fetch.sh's 1050 s under a 17-minute key with
# nocaseglob set: (a) a command word is read by its name whatever directory names it, so a
# wrapper behind `command`, or `timeout` written by its path, hands on a shell's option the gate
# reads; (b) a `.` or a `source` of anything but a literal path to a file of the tree is refused
# by name, /dev/fd/0 and /proc/self/fd/0 and a file an earlier step wrote among them; (c) a write
# into $GITHUB_ENV is read as the literal lines it holds and refused where a line names a
# variable of the list, and one whose lines cannot be read so is refused whatever it writes;
# (d) BASH_ENV and ENV are on that list, through each route the list is read by. Beside them, a
# literal source, a wrapper by its path with no setting and a $GITHUB_ENV heredoc of ordinary
# lines pass.
echo "case: r42 a wrapper named by its path is read by its name, its shell's option with it"
root=$(fixture r42-wrapper-path)
cp "$work/r38-x2-python-runs-script/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run it under bash -O nocaseglob through command and env by its path
        timeout-minutes: 17
        run: |
          command /usr/bin/env bash -O nocaseglob -c 'flock /tmp/l .github/scripts/R38-FE[T]CH.SH'
      - name: Run it under bash -O nocaseglob through timeout by its path
        timeout-minutes: 17
        run: |
          /usr/bin/timeout 600 bash -O nocaseglob -c 'flock /tmp/l .github/scripts/R38-FE[T]CH.SH'
      - name: Run a shell through env by its path with no setting
        timeout-minutes: 1
        run: |
          /usr/bin/env bash -c 'echo "nothing to fetch"'
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the option behind command and /usr/bin/env by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Run it under bash -O nocaseglob through command and env by its path' of job 'build' sets .nocaseglob. (.command /usr/bin/env bash -O nocaseglob -c ")"
check "refuses the option behind /usr/bin/timeout by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=14::step 'Run it under bash -O nocaseglob through timeout by its path' of job 'build' sets .nocaseglob. (./usr/bin/timeout 600 bash -O nocaseglob -c ")"
check "passes a shell through env by its path with no setting" \
  "$(is "$(contains "$out" "step 'Run a shell through env by its path with no setting'")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

echo "case: r42 a source that is no literal file of the tree is refused by name"
root=$(fixture r42-sources)
cp "$work/r38-x2-python-runs-script/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-fetch.sh"
printf '#!/usr/bin/env bash\necho "nothing to fetch"\n' > "$root/.github/scripts/r42-quiet.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Source the setting from /dev/fd/0
        timeout-minutes: 17
        run: |
          . /dev/fd/0 <<<'shopt -s nocaseglob'
          flock /tmp/l .github/scripts/R38-FE[T]CH.SH
      - name: Source the setting from /proc/self/fd/0
        timeout-minutes: 17
        run: |
          source /proc/self/fd/0 <<'EOS'
          shopt -s nocaseglob
          EOS
          flock /tmp/l .github/scripts/R38-FE[T]CH.SH
      - name: Source a file an earlier step wrote
        timeout-minutes: 17
        run: |
          . "$RUNNER_TEMP/settings"
          flock /tmp/l .github/scripts/R38-FE[T]CH.SH
      - name: Source a script of the tree by its literal path
        timeout-minutes: 1
        run: |
          . .github/scripts/r42-quiet.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "10	Source the setting from /dev/fd/0	sources ./dev/fd/0." \
  "15	Source the setting from /proc/self/fd/0	sources ./proc/self/fd/0." \
  "22	Source a file an earlier step wrote	sources .\\\$RUNNER_TEMP/settings."; do
  IFS=$'\t' read -r line step says <<<"$refusal"
  check "refuses '$step' by name, as no literal path to a file of the tree" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' $says, which is no literal path to a file of this tree\. A .\.. or a .source. runs the text it reads in this shell, and this check reads no shell text but what the files of this tree hold")"
done
check "tells each to source a library by its literal path, read a data file as data and write a here-string's commands in place" \
  "$(is "$(printf '%s\n' "$out" | grep -c "no literal path to a file of this tree.*Source a library of this repository by its literal path, .\. \.github/scripts/<name>\.sh., and read a data file as data.*; and write the commands a here-string or a heredoc feeds it in the text itself, where this check reads them")" 3)"
check "passes a script of the tree sourced by its literal path" \
  "$(is "$(contains "$out" "step 'Source a script of the tree by its literal path'")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

echo "case: r42 a write into GITHUB_ENV is read as its literal lines, and one it cannot read so is refused by name"
root=$(fixture r42-env-file)
cp "$work/r38-x2-python-runs-script/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Write GLOBIGNORE through a pipe into tee
        timeout-minutes: 1
        run: |
          cat <<'EOE' | tee -a "$GITHUB_ENV"
          GLOBIGNORE=x
          EOE
      - name: Write the setting from a group
        timeout-minutes: 1
        run: |
          { printf '%s=%s\n' BASHOPTS nocaseglob; } >> "$GITHUB_ENV"
      - name: Write a line whose name is built
        timeout-minutes: 1
        run: |
          echo "$name=nocaseglob" >> "$GITHUB_ENV"
      - name: Run it under the settings the steps above wrote
        timeout-minutes: 17
        run: |
          flock /tmp/l .github/scripts/R38-FE[T]CH.SH
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the GLOBIGNORE line a pipe hands tee by name, as a setting of the job's later steps" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Write GLOBIGNORE through a pipe into tee' of job 'build' sets .GLOBIGNORE. for every later step of the job, writing .GLOBIGNORE=x. into .\\\$GITHUB_ENV., which changes how bash expands a word in each of those steps, and this check reads every pattern as bash does with its default options, so it cannot tell which file a word of those steps names\. Write no line into .\\\$GITHUB_ENV. that sets any of ")"
check "refuses a group's output into GITHUB_ENV by name, as no literal lines" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=16::step 'Write the setting from a group' of job 'build' writes into .\\\$GITHUB_ENV. the output of a group or a subshell, .{ …; }. or .( … )., which this check cannot read as literal lines")"
check "refuses a line whose name is built by name, as no literal lines" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=20::step 'Write a line whose name is built' of job 'build' writes into .\\\$GITHUB_ENV. a line whose name is built as the step runs, .\\\$name=nocaseglob., which this check cannot read as literal lines")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

echo "case: r42 a write into GITHUB_ENV of ordinary lines passes"
root=$(fixture r42-env-file-ordinary)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Write ordinary lines into GITHUB_ENV
        timeout-minutes: 1
        run: |
          cat >> "$GITHUB_ENV" <<EOE
          IMAGE=ghcr.io/example/worker
          PLUGIN_DOWNLOAD_URL=$url
          NOTES<<EOV
          BASHOPTS=nocaseglob is a line of this value, which sets nothing
          EOV
          EOE
          echo "LEGION_TEST_UV=$dir/uv" >> "$GITHUB_ENV"
          printf '%s\n' 'no_proxy=' 'NO_PROXY=' >> "$GITHUB_ENV"
          printf 'TOOL_ROW=crane\n' >> "$GITHUB_ENV"
          echo "ELAN_HOME=$RUNNER_TEMP/elan" | tee -a "$GITHUB_ENV" > /dev/null
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: r42 BASH_ENV and ENV are refused by name through each route the list is read by"
root=$(fixture r42-startup-files)
cp "$work/r38-x2-python-runs-script/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    env:
      ENV: /tmp/settings
    steps:
      - name: Write the setting into a startup file
        timeout-minutes: 1
        run: printf 'shopt -s nocaseglob\n' > "$RUNNER_TEMP/settings"
      - name: Run it under sh with that startup file in ENV
        timeout-minutes: 17
        run: sh -c 'flock /tmp/l .github/scripts/R38-FE[T]CH.SH'
      - name: Export BASH_ENV naming that startup file
        timeout-minutes: 17
        run: |
          export BASH_ENV="$RUNNER_TEMP/settings"
          bash -c 'flock /tmp/l .github/scripts/R38-FE[T]CH.SH'
      - name: Write BASH_ENV into GITHUB_ENV in a heredoc
        timeout-minutes: 1
        run: |
          cat >> "$GITHUB_ENV" <<EOE
          BASH_ENV=$RUNNER_TEMP/settings
          EOE
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the job's ENV key by name, as a setting every step of the job takes" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=7::job 'build' sets .ENV. in its .env:., which reaches every step of the job, so it may change how bash expands a word there, and this check reads every pattern as bash does with its default options, so it cannot tell which file a word there names")"
check "refuses BASH_ENV through export by name, as a setting of this text or a shell it starts" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=18::step 'Export BASH_ENV naming that startup file' of job 'build' sets .BASH_ENV. (.export BASH_ENV=\\\$RUNNER_TEMP/settings.), which may change how bash expands a word, and this check reads every pattern as bash does with its default options, so it cannot tell which file a word of this text, or of a shell it starts, names")"
check "refuses BASH_ENV in a GITHUB_ENV heredoc by name, as a setting of the job's later steps" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=23::step 'Write BASH_ENV into GITHUB_ENV in a heredoc' of job 'build' sets .BASH_ENV. for every later step of the job, writing .BASH_ENV=\\\$RUNNER_TEMP/settings. into .\\\$GITHUB_ENV., which may change how bash expands a word in each of those steps, and this check reads every pattern as bash does with its default options, so it cannot tell which file a word of those steps names")"
check "tells the export to name no startup file, citing round 42" \
  "$(is "$(printf '%s\n' "$out" | grep -c "name no startup file in .BASH_ENV. or .ENV.: write the commands of the file it names in the step itself, where this check reads them (dispatch://INFRA-1545, round 42)")" 1)"
check "tells the job's key to set no such variable in an env: and a startup file's commands in the step, citing round 42" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=7::.*Set none of .* in an .env:.; .*; and where a step needs the commands a startup file would run, write them in that step, where this check reads them (dispatch://INFRA-1545, round 42)")"
check "tells the GITHUB_ENV heredoc to write no such line and a startup file's commands in the later step, citing round 42" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=23::.*Write no line into .\\\$GITHUB_ENV. that sets any of .*; and where a later step needs the commands a startup file would run, write them in that step, where this check reads them (dispatch://INFRA-1545, round 42)")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# W10 and W11. W1, W2, W6 and W9 above share one cure: the gate reads the `.sh` scripts directly
# in .github/scripts/ and no other file there, so every other file under that directory, at any
# depth, is refused by name at its line 1, whatever its name, its first line or its text, but a
# top-level harness, a top-level file of a suffix
# `.github/scripts/check-download-retry.sh#SCRIPT_DATA` names, and the bytecode Python writes for
# a `.py` into .github/scripts/__pycache__/ (dispatch://INFRA-1545, round 37). W10's refused
# files fetch nothing and no step runs them, so the file is what is refused, and not a fetch or a
# run: a `.zsh` script, a file with no suffix and no shebang, a `.txt` file, a `.sh` in a
# subdirectory and a `.txt` in the `__pycache__`, beside a top-level `.py` and `.jq` that pass.
# W11 holds only what is left alone.
echo "case: r37 W10 a shell program under .github/scripts that this check does not read is refused by name whatever it holds"
root=$(fixture r37-w10-unread-files)
mkdir -p "$root/.github/scripts/lib" "$root/.github/scripts/__pycache__"
printf '#!/usr/bin/env zsh\necho quiet\n' > "$root/.github/scripts/r37-quiet.zsh"
printf 'echo quiet\n' > "$root/.github/scripts/r37-data"
printf 'quiet\n' > "$root/.github/scripts/r37-notes.txt"
printf '#!/usr/bin/env bash\nset -euo pipefail\necho quiet\n' > "$root/.github/scripts/lib/r37-quiet.sh"
printf 'quiet\n' > "$root/.github/scripts/__pycache__/x.txt"
printf '#!/usr/bin/env python3\nprint("quiet")\n' > "$root/.github/scripts/r37-quiet.py"
printf '.name\n' > "$root/.github/scripts/r37-quiet.jq"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for file in r37-quiet.zsh r37-data r37-notes.txt lib/r37-quiet.sh __pycache__/x.txt; do
  check "refuses .github/scripts/$file by name at its line 1, with the move as its cure" \
    "$(contains "$out" "::error file=.github/scripts/$file,line=1::.github/scripts/$file is a file under .github/scripts/ this check does not read, whatever it holds: .*Move a shell script to .github/scripts/<name>.sh, its name lowercase letters, digits and .-. .*, and anything else out of .github/scripts/")"
done
check "passes the top-level .py" "$(is "$(contains "$out" 'r37-quiet.py')" false)"
check "passes the top-level .jq" "$(is "$(contains "$out" 'r37-quiet.jq')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"

echo "case: r37 W11 a top-level .py, .jq and .test.sh under .github/scripts pass"
root=$(fixture r37-w11-left-alone)
mkdir -p "$root/.github/scripts/__pycache__"
printf '#!/usr/bin/env python3\nprint("quiet")\n' > "$root/.github/scripts/x.py"
printf '.name\n' > "$root/.github/scripts/r37-quiet.jq"
printf '#!/usr/bin/env bash\necho quiet\n' > "$root/.github/scripts/r37-quiet.test.sh"
printf '\363\r\r\n' > "$root/.github/scripts/__pycache__/x.cpython-313.pyc"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses nothing" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 0)"

# W17. Main's #5308 puts two programs of other languages directly in .github/scripts/, the Go and
# TypeScript scanners ci_citations.py runs with `go run` and `bun`, and lint's formatting step
# names the Go one as an argument of `gofmt` (dispatch://INFRA-1545, round 66). A top-level `.go`
# and `.ts` pass where they stand, and so does `gofmt -l` given the `.go`, since gofmt reads a
# file's text and runs none of it. A shell given either file runs it as shell text and is refused
# at the run. A `.ts` whose first line runs a shell is refused at its line 1 and told its own
# interpreter, and is refused again where a step runs it as its command word. Until review round
# 39 of dispatch://INFRA-1545 no case wrote a `.ts` with a `#!` line, so the gate passed every
# case with its `.ts` entry of OWN_INTERPRETERS set to `sh`. A `.go` takes no `#!` line: Go
# cannot compile a file that starts with `#`. So a `.go` whose first line runs a shell, and one
# whose first line names `go`, are each refused at its line 1. A step that runs a `.go` as its
# command word is refused, with or without a `#!` line. Until round 68 a `.go` under
# `#!/usr/bin/env go` passed (dispatch://INFRA-1545, round 68). No row of this case checks the
# words of a refusal or of its cure. Each checks where the gate refuses, a line that opens
# `::error file=<file>,line=<n>::`, and literal names that line holds, each found on its own:
# the step and the file, the first line of a file refused at its line 1, the shell a step hands
# the `.go` to, `bun` for a `.ts` and `go run` for a `.go`. A refusal at a file's line 1 names the
# program its `#!` line runs, so the row of each such file also checks that the line names no path
# or `env` beyond that first line. The refusal of bash given the `.go` names `.go` only in the
# file's path, so its cure offers a `.go` no interpreter and no run by its path. A refusal names
# the program `.` as a lone dot, which no literal name finds apart from the dots of the paths, so
# the row of `.` given the `.ts` reads that refusal with its marks taken off: every backtick, quote
# and parenthesis deleted, and every comma, semicolon and colon made a space. There the program is
# a dot that stands alone between two spaces, whatever marks the gate writes round it, and the dots
# of a path, a suffix and a sentence's end stay joined to their words, so a refusal that names
# another program there names no lone dot (dispatch://INFRA-1545, rounds 69, 70 and 72).
echo "case: r66 W17 a top-level .go and .ts under .github/scripts pass, gofmt may name the .go, a shell given either is refused, and a .go opening with any #! line is refused"
root=$(fixture r66-w17-scanners)
printf '// a scanner\npackage main\n\nfunc main() {}\n' > "$root/.github/scripts/r66-scan.go"
printf '// a scanner\nconsole.log("quiet");\n' > "$root/.github/scripts/r66-scan.ts"
printf '#!/bin/sh\ncurl -fsSL -o b https://example.com/b\n' > "$root/.github/scripts/r66-shell.go"
printf '#!/bin/sh\ncurl -fsSL -o b https://example.com/b\n' > "$root/.github/scripts/r66-shell.ts"
printf '#!/usr/bin/env go\npackage main\n\nfunc main() {}\n' > "$root/.github/scripts/r68-env-go.go"
chmod +x "$root/.github/scripts/r66-shell.ts" "$root/.github/scripts/r66-scan.go" \
  "$root/.github/scripts/r68-env-go.go"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Check the scanner's formatting
        run: |
          unformatted=$(gofmt -l .github/scripts/r66-scan.go)
          test -z "$unformatted"
      - name: Run the Go scanner with bash
        run: bash .github/scripts/r66-scan.go
      - name: Source the TypeScript scanner
        run: . .github/scripts/r66-scan.ts
      - name: Run the TypeScript program whose first line runs a shell
        run: .github/scripts/r66-shell.ts
      - name: Run the Go scanner as its command word
        run: .github/scripts/r66-scan.go
      - name: Run the Go program whose first line names go as its command word
        run: .github/scripts/r68-env-go.go
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "passes the top-level .go and .ts where they stand" \
  "$(is "$(contains "$out" '::error file=.github/scripts/r66-scan\.\(go\|ts\),line=1::')" false)"
check "passes gofmt given the .go" \
  "$(is "$(contains "$out" "step 'Check the scanner's formatting'")" false)"
check "refuses bash given the .go at its run, naming the step, the file and bash" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=12::' \
    'Run the Go scanner with bash' .github/scripts/r66-scan.go bash)"
check "names .go to bash given the .go only in the file's path, offering a .go no interpreter and no run by its path" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=12::' \
    .github/scripts/r66-scan.go .github/scripts/r66-scan.go --not .go)"
# unmarked <text>: the text with those marks taken off. The row's location loses its marks the same
# way, so it still ends where the refusal's `::` did, and line 14 reads apart from line 140.
unmarked() { tr -d "\`'\"()" <<<"$1" | tr ',;:' '   '; }
check "refuses . given the .ts at its run, naming the step, the file and the program ., a word of its own" \
  "$(names_at "$(unmarked "$out")" "$(unmarked '::error file=.github/workflows/case.yaml,line=14::')" \
    'Source the TypeScript scanner' .github/scripts/r66-scan.ts ' . ')"
check "refuses the .go whose first line runs a shell at its line 1, naming that line and go run, and no path beyond it" \
  "$(names_at "$out" '::error file=.github/scripts/r66-shell.go,line=1::' \
    .github/scripts/r66-shell.go '#!/bin/sh' 'go run' --not /bin/sh)"
check "refuses the .go whose first line names go at its line 1, naming that line and go run, and no env beyond it" \
  "$(names_at "$out" '::error file=.github/scripts/r68-env-go.go,line=1::' \
    .github/scripts/r68-env-go.go '#!/usr/bin/env go' 'go run' --not env)"
check "refuses the .ts whose first line runs a shell at its line 1, naming that line and bun twice, and no path beyond it" \
  "$(names_at "$out" '::error file=.github/scripts/r66-shell.ts,line=1::' \
    .github/scripts/r66-shell.ts '#!/bin/sh' bun bun --not /bin/sh)"
check "refuses the .ts whose first line runs a shell where a step runs it as its command word, naming the step and the file" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=16::' \
    'Run the TypeScript program whose first line runs a shell' .github/scripts/r66-shell.ts)"
check "tells that run the interpreter a .ts names, bun" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=16::' bun)"
for refusal in "18	Run the Go scanner as its command word	r66-scan.go" \
  "20	Run the Go program whose first line names go as its command word	r68-env-go.go"; do
  IFS=$'\t' read -r line step name <<<"$refusal"
  check "refuses $name where a step runs it as its command word, naming the step, the file and go run" \
    "$(names_at "$out" "::error file=.github/workflows/case.yaml,line=$line::" \
      "$step" ".github/scripts/$name" 'go run')"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 8)"

# Review round 39 of dispatch://INFRA-1545. Each refusal that names the members of one of the
# gate's lists builds them from the list, so a member added to the list is named in the refusal
# too. Until that round the refusal of a `.go` or a `.ts` whose first line runs another program
# told it to keep a Python program or a jq filter under its own interpreter, a sentence written
# while SCRIPT_DATA held `.py` and `.jq` alone. The fixture runs a copy of the gate with one
# member added to each list: the suffix `.zz` to SCRIPT_DATA, with `zzrun` its interpreter in
# OWN_INTERPRETERS, the option `zzwrap --zz <zzword>` to WRAPPER_OPTIONS, the program `zzprint`
# to UNREAD_ARGUMENTS and the program `zzsay` to NONCALL_COMMANDS. Each refusal then names the
# member added to its list: the refusal of a `.zz` whose first line runs a shell, of a file the
# gate does not read, of a directory its walk skips, of a `.py` handed to bash, of an option of a
# wrapper the gate does not read, and of a script that fetches named where the gate reads no call.
# Since round 68 the cure of the `.py` handed to bash names the suffixes OWN_INTERPRETERS gives an
# interpreter, as the files a command word may run, so it names `.zz` too (dispatch://INFRA-1545,
# round 68). Since round 69 each row checks only where the gate refuses and the added member it
# names, not the words around it (dispatch://INFRA-1545, round 69).
echo "case: review r39 each refusal that names a list's members names a member added to that list"
root=$(fixture r39-listed-members)
copy="$root/.github/scripts/check-download-retry.sh"
rm "$copy"
sed -e 's/^SCRIPT_DATA = ($/&\n    ".zz",/' \
  -e 's/^OWN_INTERPRETERS = {/&".zz": {"zzrun"}, /' \
  -e 's/^WRAPPER_OPTIONS = {$/&\n    "zzwrap": {"--zz": ("<zzword>",)},/' \
  -e 's/^UNREAD_ARGUMENTS = NONCALL_COMMANDS | {$/&\n    "zzprint",/' \
  -e 's/^NONCALL_COMMANDS = {/&"zzsay", /' "$check_script" > "$copy"
chmod +x "$copy"
check "the copy of the gate carries the five added members" \
  "$(is "$(grep -c '"\.zz"\|"zz[a-z]*"' "$copy")" 5)"
printf '#!/bin/sh\necho quiet\n' > "$root/.github/scripts/r39-shell.zz"
printf 'quiet\n' > "$root/.github/scripts/r39-notes.txt"
mkdir -p "$root/.github/scripts/vendor"
printf 'quiet\n' > "$root/.github/scripts/vendor/r39-notes.txt"
printf '#!/usr/bin/env python3\nprint("quiet")\n' > "$root/.github/scripts/r39-tool.py"
cat > "$root/.github/scripts/r39-fetch.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/r39-fetch.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the Python program with bash
        run: bash .github/scripts/r39-tool.py
      - name: Run curl through env -S
        run: env -S 'curl -fsSL -o b https://example.com/releases/download/v1/b'
      - name: Run the fetching script through eval
        timeout-minutes: 1
        run: eval .github/scripts/r39-fetch.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "names zzrun, the added interpreter, twice to the .zz whose first line runs a shell" \
  "$(contains "$out" '::error file=.github/scripts/r39-shell.zz,line=1::.*`zzrun`.*`zzrun`')"
check "names .zz, the added suffix, to a file the gate does not read" \
  "$(contains "$out" '::error file=.github/scripts/r39-notes.txt,line=1::.*`\.zz`')"
check "names .zz to a directory its walk skips" \
  "$(contains "$out" '::error file=.github/scripts/vendor,line=1::.*`\.zz`')"
check "names zzrun for a .zz to the .py handed to bash" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=8::.*`zzrun`.*`\.zz`')"
check "names .zz twice to the .py handed to bash" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=8::.*`\.zz`.*`\.zz`')"
check "names zzprint, the added program, to the .py handed to bash" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=8::.*`zzprint`')"
check "names zzwrap --zz <zzword>, the added option, to an option of a wrapper the gate does not read" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=10::.*`zzwrap --zz <zzword>`')"
check "names zzsay, the added program, to a script that fetches named where the gate reads no call" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=13::.*`zzsay`')"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 6)"

# Review round 68 of dispatch://INFRA-1545. Round 67 built each refusal the review named from its
# list, and round 68 found more refusals that wrote a list's members by hand. Rule 7's refusal
# named the four members of MISE_SILENT. STARTUP_CURE named the two of STARTUP_VARIABLES. The
# refusals of a sleep's argument named the suffixes of SUFFIX. LOOP_WORD_REFUSAL named LOOP_WORDS.
# HEREDOC_UNREAD named HEREDOC_DATA. UNNAMED_RUN named two of the six shells of INTERPRETERS. Round
# 68 also adds NO_HASHBANG, whose refusals name its suffix and the command that compiles one. As in
# the case above, the fixture runs a copy of the gate with one member added to each list: the
# subcommand `zzx` to MISE_SILENT, the variable `ZZ_ENV` to STARTUP_VARIABLES, the suffix `w` to
# SUFFIX, the word `zzloop` to LOOP_WORDS, the consumer `zzdata` to HEREDOC_DATA, the shell `zzsh`
# to INTERPRETERS, and the suffix `.zq` to SCRIPT_DATA, with `zqc` its command in NO_HASHBANG.
# Each refusal or cure that lists one of those lists must then name the added member. Since round
# 69 each row checks only where the gate refuses and the added member it names, not the words
# around it. The refusals of `mise zzx` and `ZZ_ENV` also quote the step's own text, which holds
# the member, so their rows check that it is named twice (dispatch://INFRA-1545, round 69).
echo "case: review r68 each refusal that wrote a list's members by hand names a member added to that list"
root=$(fixture r68-listed-members)
copy="$root/.github/scripts/check-download-retry.sh"
rm "$copy"
sed -e 's/^MISE_SILENT = (/&"zzx", /' \
  -e 's/^STARTUP_VARIABLES = frozenset({/&"ZZ_ENV", /' \
  -e 's/^SUFFIX = {/&"w": 604800, /' \
  -e 's/^LOOP_WORDS = (/&"zzloop", /' \
  -e 's/^HEREDOC_DATA = (/&"zzdata", /' \
  -e 's/^INTERPRETERS = {/&"zzsh", /' \
  -e 's/^SCRIPT_DATA = ($/&\n    ".zq",/' \
  -e 's/^NO_HASHBANG = {/&".zq": "zqc", /' "$check_script" > "$copy"
chmod +x "$copy"
check "the copy of the gate carries the eight added members" \
  "$(is "$(grep -c '"zzx"\|"ZZ_ENV"\|"w": 604800\|"zzloop"\|"zzdata"\|"zzsh"\|"\.zq"' "$copy")" 8)"
printf '#!/bin/sh\necho quiet\n' > "$root/.github/scripts/r68-shell.zq"
chmod +x "$root/.github/scripts/r68-shell.zq"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Retry a mise subcommand that exits 0
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the tool" 60 -- mise zzx node@22 -- node --version
      - name: Export a startup file
        run: export ZZ_ENV="$RUNNER_TEMP/settings"
      - name: Sleep between fetches by an argument the gate does not read
        timeout-minutes: 30
        run: |
          . .github/scripts/ci-retry.sh
          for pass in 1 2 3; do
            retry_fetch "the asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
            sleep 5x
          done
      - name: Write a loop's word in quotes beside a fetch
        timeout-minutes: 10
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the asset" 60 -- curl -fsSL --max-time 60 -o a https://example.com/a
          "done"
      - name: Feed a fetch to bash in a heredoc
        run: |
          bash <<'EOF'
          curl -fsSL -o a https://example.com/a
          EOF
      - name: Run a script through a variable
        run: |
          p=.github/scripts/r68-quiet.sh; "$p"
      - name: Run the program of the added suffix as its command word
        run: .github/scripts/r68-shell.zq
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "names mise zzx, the added subcommand, twice to the step that retries it" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=10::.*`mise zzx`.*`mise zzx`')"
check "names ZZ_ENV, the added variable, twice to the step that exports it" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=12::.*`ZZ_ENV`.*`ZZ_ENV`')"
check "names w, the added suffix, twice to the sleep whose argument the gate does not read" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=19::.*`w`.*`w`')"
check "names zzloop, the added word, to the loop's word in quotes" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=26::.*`zzloop`')"
check "names zzdata, the added consumer, to the heredoc fed to bash" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=29::.*`zzdata`')"
check "names zzsh, the added shell, to the script run through a variable" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=34::.*`zzsh`')"
check "names zqc, the added command, twice to the .zq whose first line is a #! line" \
  "$(contains "$out" '::error file=.github/scripts/r68-shell.zq,line=1::.*`zqc`.*`zqc`')"
check "names zqc and .zq to the step that runs the .zq as its command word" \
  "$(contains "$out" '::error file=.github/workflows/case.yaml,line=36::.*`zqc`.*`\.zq`')"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 8)"

# W12. The two routes past that rule to shell text the gate does not read (dispatch://INFRA-1545,
# round 37). A shell, `.` or `source` handed a top-level `.py` or `.jq` runs it as shell text,
# and is refused at the run with the cure to run the program with `python3` or read the filter
# with `jq -f`: by a literal path, from the workspace's root, with `ksh` and `./`, by its name
# under a directory held in a variable, and from a step's working directory. And a directory the
# tree's walk does not enter hides every file in it from that rule, so it is refused by name: a
# symbolic link to a directory holding a bare curl, directly in .github/scripts/ and inside a
# subdirectory, and a directory named `vendor`, a name the walk skips. `python3` and `jq -f`
# given the same files pass, and so does a shell given a `.py` the tree does not hold there.
echo "case: r37 W12 a shell given a top-level .py or .jq, and a symlinked directory under .github/scripts, are refused by name"
root=$(fixture r37-w12-shell-data)
printf '#!/usr/bin/env python3\nprint("quiet")\n' > "$root/.github/scripts/r37-tool.py"
printf '.name\n' > "$root/.github/scripts/r37-filter.jq"
mkdir -p "$root/r37-elsewhere" "$root/.github/scripts/lib" "$root/.github/scripts/vendor"
printf '#!/usr/bin/env bash\ncurl -fsSL -o b https://example.com/b\n' > "$root/r37-elsewhere/r37-fetch.sh"
cp "$root/r37-elsewhere/r37-fetch.sh" "$root/.github/scripts/vendor/r37-fetch.sh"
ln -s ../../r37-elsewhere "$root/.github/scripts/r37-linked"
ln -s ../../../r37-elsewhere "$root/.github/scripts/lib/r37-deep"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the Python program with bash
        run: bash .github/scripts/r37-tool.py
      - name: Source the jq filter
        run: . .github/scripts/r37-filter.jq
      - name: Source the Python program from the workspace
        run: source "$GITHUB_WORKSPACE/.github/scripts/r37-tool.py"
      - name: Run the jq filter with ksh
        run: ksh ./.github/scripts/r37-filter.jq
      - name: Run the Python program by its name under a variable directory
        run: sh "$dir/r37-tool.py"
      - name: Run the Python program from a working directory
        working-directory: packages/envoy
        run: bash ../../.github/scripts/r37-tool.py
      - name: Run the Python program and read the jq filter as they are run
        run: |
          python3 .github/scripts/r37-tool.py
          jq -f .github/scripts/r37-filter.jq data.json
          bash "$RUNNER_TEMP/r37-other.py"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "8	Run the Python program with bash	writes ..github/scripts/r37-tool.py. as an argument of .bash., and it names .github/scripts/r37-tool.py" \
  "10	Source the jq filter	writes ..github/scripts/r37-filter.jq. as an argument of .\.., and it names .github/scripts/r37-filter.jq" \
  "12	Source the Python program from the workspace	writes .\$GITHUB_WORKSPACE/.github/scripts/r37-tool.py. as an argument of .source., and it names .github/scripts/r37-tool.py" \
  "14	Run the jq filter with ksh	writes ../.github/scripts/r37-filter.jq. as an argument of .ksh., and it names .github/scripts/r37-filter.jq" \
  "16	Run the Python program by its name under a variable directory	writes .\$dir/r37-tool.py. as an argument of .sh., and it names .github/scripts/r37-tool.py" \
  "19	Run the Python program from a working directory	writes .\.\./\.\./\.github/scripts/r37-tool.py. as an argument of .bash., and it names .github/scripts/r37-tool.py"; do
  IFS=$'\t' read -r line step says <<<"$refusal"
  check "refuses '$step' by name at its run" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' $says, a file under .github/scripts/ this check does not read as a script")"
done
check "tells each of the six to run the program with python3 or read the filter with jq -f" \
  "$(is "$(printf '%s\n' "$out" | grep -c "this check does not read as a script\. Such a file appears in a command only .*Run a Python program with .python3 .github/scripts/<name>.py .*. and read a jq filter with .jq -f .github/scripts/<name>.jq .*.; a shell script goes in .github/scripts/<name>.sh")" 6)"
check "passes python3 and jq -f given the same files, and a shell given a .py elsewhere" \
  "$(is "$(contains "$out" "step 'Run the Python program and read the jq filter as they are run'")" false)"
for directory in "r37-linked	it is a symbolic link" "lib/r37-deep	it is a symbolic link" \
  "vendor	that walk skips every directory named .vendor."; do
  IFS=$'\t' read -r name why <<<"$directory"
  check "refuses .github/scripts/$name by name, since $why" \
    "$(contains "$out" "::error file=.github/scripts/$name,line=1::.github/scripts/$name is a directory at or under .github/scripts/ that this check's walk of the tree does not enter, since $why, so no file in it is read for its fetches.*Replace it with the files it holds")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 9)"

# W13 and W14. Six routes round 38's class search drove past the gate at 172e244f, each passing a
# 1050 s script under a one-minute key (dispatch://INFRA-1545, round 38). W13 is the shell text
# under .github/scripts/ the gate reads nothing of: a top-level `.py` or `.jq` whose first line
# runs a shell, run as a command word (P7, P8), refused by name at its line 1 whatever runs it,
# by any path or through `env` with its options; a `.pyc` handed to bash (P9) and a `bash -c`
# text sourcing a `.py` (P15), each refused at the run; and a `.py` of shell commands with no
# `#!` line run as a command word, which the shell runs as shell text when the kernel will not
# execute it, refused at the run. A `python3` run of a `.py` and a run of a `.py` whose first
# line names python3, as a command word, pass beside them. Since W16's allow-list the first two
# are refused at the run as well, a command word whose `#!` line names no interpreter of its own,
# and each refusal at a run says where the file is written and where it may be.
echo "case: r37 W13 a shell program among the top-level .py and .jq files, a .pyc and a bash -c text, each run by a shell, are refused by name"
root=$(fixture r37-w13-shell-programs)
mkdir -p "$root/.github/scripts/__pycache__"
for program in "r37-shell.py	#!/bin/sh" "r37-shell.jq	#!/usr/bin/env -S bash -e" \
  "r37-env.py	#!/usr/bin/env -u HOME zsh"; do
  IFS=$'\t' read -r name shebang <<<"$program"
  printf '%s\n. .github/scripts/ci-retry.sh\nretry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b\n' \
    "$shebang" > "$root/.github/scripts/$name"
  chmod +x "$root/.github/scripts/$name"
done
printf '#!/usr/bin/env python3\nprint("quiet")\n' > "$root/.github/scripts/r37-tool.py"
chmod +x "$root/.github/scripts/r37-tool.py"
printf '. .github/scripts/ci-retry.sh\nretry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b\n' \
  > "$root/.github/scripts/__pycache__/r37-tool.cpython-313.pyc"
cp "$root/.github/scripts/__pycache__/r37-tool.cpython-313.pyc" "$root/.github/scripts/r37-plain.py"
chmod +x "$root/.github/scripts/r37-plain.py"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the shell program written as a .py
        timeout-minutes: 1
        run: .github/scripts/r37-shell.py
      - name: Run the shell program written as a .jq
        timeout-minutes: 1
        run: .github/scripts/r37-shell.jq
      - name: Run the bytecode with bash
        timeout-minutes: 1
        run: bash .github/scripts/__pycache__/r37-tool.cpython-313.pyc
      - name: Source the Python program in a bash -c text
        timeout-minutes: 1
        run: bash -c '. .github/scripts/r37-tool.py'
      - name: Run the plain Python file as a command
        timeout-minutes: 1
        run: .github/scripts/r37-plain.py
      - name: Run the Python program as Python
        run: |
          python3 .github/scripts/r37-tool.py
          .github/scripts/r37-tool.py
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for program in "r37-shell.py	#!/bin/sh	sh	.python3. or .python." \
  "r37-shell.jq	#!/usr/bin/env -S bash -e	bash	.jq." \
  "r37-env.py	#!/usr/bin/env -u HOME zsh	zsh	.python3. or .python."; do
  IFS=$'\t' read -r name shebang runs own <<<"$program"
  check "refuses .github/scripts/$name by name at its line 1, as the program of another interpreter its first line makes it" \
    "$(contains "$out" "::error file=.github/scripts/$name,line=1::.github/scripts/$name is a .\.${name##*.}. directly in .github/scripts/ whose first line, .$shebang., runs .$runs., not $own: this check reads no such file as a program of another interpreter.*Move a shell program to .github/scripts/<name>.sh")"
done
for run in "9	Run the shell program written as a .py	r37-shell.py" \
  "12	Run the shell program written as a .jq	r37-shell.jq" \
  "21	Run the plain Python file as a command	r37-plain.py"; do
  IFS=$'\t' read -r line step name <<<"$run"
  check "refuses '$step' at the run, a command word whose first line names no interpreter of its own" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' writes ..github/scripts/$name. as a command word that does not open with a .#!. line naming its own interpreter, and it names .github/scripts/$name, a file under .github/scripts/ this check does not read as a script")"
done
check "refuses the bytecode handed to bash at the run" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=15::step 'Run the bytecode with bash' of job 'build' writes ..github/scripts/__pycache__/r37-tool.cpython-313.pyc. as an argument of .bash., and it names .github/scripts/__pycache__/r37-tool.cpython-313.pyc, a file under .github/scripts/ this check does not read as a script")"
check "refuses the bash -c text that sources the Python program, at the run" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=18::step 'Source the Python program in a bash -c text' of job 'build' writes ..github/scripts/r37-tool.py. in the text .bash -c. runs, as an argument of .\.., and it names .github/scripts/r37-tool.py, a file under .github/scripts/ this check does not read as a script")"
check "tells each of the five runs to run the program with python3 or put a shell script in a .sh" \
  "$(is "$(printf '%s\n' "$out" | grep -c "this check does not read as a script\. Such a file appears in a command only .*Run a Python program with .python3 .github/scripts/<name>.py .*.; a shell script goes in .github/scripts/<name>.sh")" 5)"
check "passes python3 given the Python program, and the program run by its python3 first line" \
  "$(is "$(contains "$out" "step 'Run the Python program as Python'\|r37-tool.py,line=1")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 8)"

# W14 is the verb reader's wrappers (P11, P12): the words after a wrapper the gate drops were read
# as the command, so `nice -n 10 curl` ran a command named `-n`, and `busybox wget` a command
# named busybox, and neither fetch was read. The options the gate reads, those
# `.github/scripts/check-download-retry.sh#WRAPPER_OPTIONS` names, pass through to the command,
# so a bare fetch behind `nice -n <n>`, `env -u <name>`, `sudo -E` or `sudo -u <user>` is refused
# at the floor, a `psql` behind `sudo -u postgres`, which
# dispatch-unit-repeat.yaml writes since this branch's merge of main's #4267, fetches nothing and
# passes, a `retry_fetch` behind `env -i` is the helper behind a wrapper, which
# fetches nothing, and `env -i` inside the helper is a retried fetch that passes. Any other option
# of a wrapper, `env -S` and `nice --adjustment=5` here, and `busybox`, are refused by name in a
# step, in a `bash -c` text and in a Dockerfile RUN, and the
# `env -u CGO_ENABLED go env CGO_ENABLED` that worker.Dockerfile runs reads as `go env`, which
# fetches nothing.
echo "case: r37 W14 a wrapper option this check does not read, and busybox, are refused by name, and the options it reads pass through to the command"
root=$(fixture r37-w14-wrappers)
printf '%s\n' 'RUN nice --adjustment=5 curl -fsSL -o b https://example.com/b' \
  'RUN test "$(env -u CGO_ENABLED go env CGO_ENABLED)" = 0' \
  'RUN busybox --help > /tmp/busybox-help' >> "$root/packages/daemon/docker/worker.Dockerfile"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a bare curl under nice -n
        run: nice -n 10 curl -fsSL -o b https://example.com/releases/download/v1/b
      - name: Run a bare curl under env -u
        run: env -u HTTPS_PROXY curl -fsSL -o b https://example.com/releases/download/v1/b
      - name: Run a bare apt-get update under sudo -E
        run: sudo -E apt-get update -o APT::Update::Error-Mode=any
      - name: Run curl through env -S
        run: env -S 'curl -fsSL -o b https://example.com/releases/download/v1/b'
      - name: Run curl under nice --adjustment
        run: nice --adjustment=5 curl -fsSL -o b https://example.com/releases/download/v1/b
      - name: Run wget through busybox
        run: busybox wget -O b https://example.com/releases/download/v1/b
      - name: Run curl under an unread option in a bash -c text
        run: bash -c 'nice --adjustment=5 curl -fsSL -o b https://example.com/releases/download/v1/b'
      - name: Call the helper behind env -i
        run: env -i PATH=/x retry_fetch "the asset" 30 -- curl -fsSL --max-time 30 -o b https://example.com/b
      - name: Run env -i inside the helper
        timeout-minutes: 6
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the asset" 30 -- env -i PATH=/x curl -fsSL --max-time 30 -o b https://example.com/b
      - name: Print busybox's own help, which runs no applet
        run: busybox --help | head -n 1 && busybox --list
      - name: Run a bare curl under sudo -u
        run: sudo -u postgres curl -fsSL https://example.com/x -o x
      - name: Run psql as postgres under sudo -u
        run: sudo -u postgres psql -c 'select 1'
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for floor in "8	curl	Run a bare curl under nice -n	fetches without the floor" \
  "10	curl	Run a bare curl under env -u	fetches without the floor" \
  "12	apt-get update	Run a bare apt-get update under sudo -E	fetches with no retry" \
  "31	curl	Run a bare curl under sudo -u	fetches without the floor"; do
  IFS=$'\t' read -r line verb step says <<<"$floor"
  check "reads '$step' through to its $verb, and refuses it at the floor" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::.$verb. in step .$step. $says")"
done
for refusal in "14	Run curl through env -S	runs .env. with .-S., an option of it this check does not read" \
  "16	Run curl under nice --adjustment	runs .nice. with .--adjustment=5., an option of it this check does not read" \
  "18	Run wget through busybox	runs .busybox wget., a program named by an argument of .busybox." \
  "20	Run curl under an unread option in a bash -c text	runs .nice. with .--adjustment=5., an option of it this check does not read"; do
  IFS=$'\t' read -r line step says <<<"$refusal"
  check "refuses '$step' by name" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' $says")"
done
check "reads through env -i to the helper, which a wrapper cannot run" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=22::step 'Call the helper behind env -i' of job 'build' runs .retry_fetch. behind .env.")"
check "refuses the Dockerfile RUN's unread option by name" \
  "$(contains "$out" "::error file=packages/daemon/docker/worker.Dockerfile,line=3::this RUN runs .nice. with .--adjustment=5., an option of it this check does not read")"
check "passes env -i inside the helper, a retried fetch under its key" \
  "$(is "$(contains "$out" "Run env -i inside the helper")" false)"
check "passes the RUN that reads go env behind env -u" \
  "$(is "$(contains "$out" "worker.Dockerfile,line=4")" false)"
# `busybox --help`, `--list` and `--list-full` run no applet, as `command -v` runs no command, so
# they pass in a step and in a RUN; the worker image's version check writes the first
# (cybermonklabs/legion#4377).
check "passes a step that prints busybox's own help and list" \
  "$(is "$(contains "$out" "Print busybox's own help")" false)"
check "passes the RUN that prints busybox's own help" \
  "$(is "$(contains "$out" "worker.Dockerfile,line=5")" false)"
check "passes psql behind sudo -u postgres, which fetches nothing" \
  "$(is "$(contains "$out" "Run psql as postgres under sudo -u")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 10)"

# W54. ci-log-guard.sh's two guards, `ci_log_guard` and `ci_log_guard_stderr`, run the command
# after them inside a stop-commands block, so the gate reads through them as through the other
# PASSTHROUGH wrappers (dispatch://INFRA-1545, round 54). Behind each, a curl at the floor is a
# site: --list lists it, and --jobs charges its 8 x 30 + 127 = 367 s to the job's priced key; and
# a bare curl is refused at the floor. With the word out of PASSTHROUGH the guard was read as the
# command, so neither curl was read: the first was listed and charged nowhere, and the second
# passed.
for guard in ci_log_guard ci_log_guard_stderr; do
  echo "case: r54 W54 a curl behind $guard is read through it: at the floor it is listed and charged, bare it is refused"
  root=$(fixture "r54-w54-$guard-floor")
  ln -s "$script_dir/ci-log-guard.sh" "$root/.github/scripts/ci-log-guard.sh"
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # 60 s for the rest of the job, and the step's 367 s: 60 + 367 = 427 s -> 8 min.
    timeout-minutes: 8
    steps:
      - name: Retry a curl behind $guard
        # 8 x 30 + 127 = 367 s.
        timeout-minutes: 7
        run: |
          . .github/scripts/ci-log-guard.sh
          $guard curl -fsSL --retry 7 --retry-all-errors --max-time 30 -o a https://example.com/a
YAML
  run_check "$root"
  check "$guard: the curl at the floor passes" "$(is "$status" 0)"
  list_check "$root"
  check "$guard: --list lists it as a curl site of its step" \
    "$(contains "$out" "^.github/workflows/case.yaml	14	curl	Retry a curl behind $guard\$")"
  set +e
  run_gate null "$root" --jobs
  set -e
  check "$guard: --jobs charges its 367 s to the job's key" \
    "$(contains "$out" "^.github/workflows/case.yaml	build	7	8	367	.*	Retry a curl behind $guard\$")"
  root=$(fixture "r54-w54-$guard-bare")
  ln -s "$script_dir/ci-log-guard.sh" "$root/.github/scripts/ci-log-guard.sh"
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a bare curl behind $guard
        run: |
          . .github/scripts/ci-log-guard.sh
          $guard curl -fsSL https://example.com/a -o a
YAML
  run_check "$root"
  check "$guard: the bare curl is refused at the floor" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::.curl. in step .Run a bare curl behind $guard. fetches without the floor")"
  check "$guard: and nothing else" "$(is "$status|$(printf '%s\n' "$out" | grep -c '^::error')" "1|1")"
done

# W55. `.github/scripts/gh-read.sh#gh_wait_reset`, a shell function that runs the words after it,
# is the one wrapper a `retry_fetch` may stand behind, in exactly one form,
# `gh_wait_reset [--stderr <file>] retry_fetch …`, which the gate reads and charges as a bare
# call, one ladder (rule 6; dispatch://INFRA-1545, round 55). Each case writes gh-read.sh as a
# one-line stand-in, the shape check-workflow-files.test.sh gives it: the gate reads the words of
# the call, never the helper's body. Behind any other wrapper, alone behind it or with gh_wait_reset
# in front of that wrapper, `retry_fetch` stays refused by name; and a `gh` or a `curl` behind
# gh_wait_reset with no `retry_fetch` is a fetch the gate reads as one behind any other wrapper,
# refused at the floor.
# gh_read_stub <root>: that stand-in, in the fixture.
gh_read_stub() {
  printf '%s\n' '#!/usr/bin/env bash' 'gh_wait_reset() { "$@"; }' > "$1/.github/scripts/gh-read.sh"
}
for form in plain stderr; do
  echo "case: r55 W55 gh_wait_reset in front of retry_fetch ($form) passes, charged one ladder"
  root=$(fixture "r55-w55-$form")
  gh_read_stub "$root"
  if [ "$form" = plain ]; then
    call='gh_wait_reset retry_fetch "the pull request" 30 -- gh api repos/o/r/pulls/1 > pull.json'
  else
    call='gh_wait_reset --stderr "$RUNNER_TEMP/words" retry_fetch "the pull request" 30 -- gh api repos/o/r/pulls/1 > pull.json'
  fi
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # 60 s for the rest of the job, and the step's 300 s: 60 + 300 = 360 s -> 7 min.
    timeout-minutes: 7
    steps:
      - name: Read the pull request behind the rate-limit wait
        # 5 x 30 + 150 = 300 s -> 6 min.
        timeout-minutes: 6
        run: |
          . .github/scripts/ci-retry.sh
          source .github/scripts/gh-read.sh
          $call
YAML
  run_check "$root"
  check "$form: the call passes" "$(is "$status" 0)"
  check "$form: no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
  list_check "$root"
  check "$form: --list lists its gh api as a site of its step" \
    "$(contains "$out" "^.github/workflows/case.yaml	15	gh api	Read the pull request behind the rate-limit wait\$")"
  set +e
  run_gate null "$root" --jobs
  set -e
  check "$form: --jobs charges the step one ladder, 5 x 30 + 150 = 300 s, under the job's key" \
    "$(contains "$out" "^.github/workflows/case.yaml	build	7	7	300	.*	Read the pull request behind the rate-limit wait\$")"
done

echo "case: r55 W55 retry_fetch behind another wrapper, alone or behind gh_wait_reset, is refused by name"
root=$(fixture r55-w55-other-wrapper)
gh_read_stub "$root"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call the helper behind nice
        run: |
          . .github/scripts/ci-retry.sh
          nice -n 1 retry_fetch "the pull request" 30 -- gh api repos/o/r/pulls/1 > pull.json
      - name: Call the helper behind nice behind the wait
        run: |
          . .github/scripts/ci-retry.sh
          source .github/scripts/gh-read.sh
          gh_wait_reset nice -n 1 retry_fetch "the pull request" 30 -- gh api repos/o/r/pulls/1 > pull.json
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the helper behind nice by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Call the helper behind nice' of job 'build' runs .retry_fetch. behind .nice.")"
check "and behind nice behind gh_wait_reset, which is no form of the one gh_wait_reset may stand in" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=15::step 'Call the helper behind nice behind the wait' of job 'build' runs .retry_fetch. behind .nice.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

echo "case: r55 W55 a gh or a curl behind gh_wait_reset with no retry_fetch is refused at the floor"
root=$(fixture r55-w55-no-ladder)
gh_read_stub "$root"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the pull request through the wait alone
        run: |
          source .github/scripts/gh-read.sh
          gh_wait_reset gh api repos/o/r/pulls/1 > pull.json
      - name: Fetch an asset through the wait alone
        run: |
          source .github/scripts/gh-read.sh
          gh_wait_reset curl -fsSL https://example.com/a -o a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "reads through gh_wait_reset to its gh api, a fetch with no retry" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::.gh api. in step .Read the pull request through the wait alone. fetches with no retry")"
check "and to its curl, a fetch without the floor" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=14::.curl. in step .Fetch an asset through the wait alone. fetches without the floor")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# Review round 36 of dispatch://INFRA-1545. ci-log-guard.sh's two guards are shell functions
# that run the helper whole (that file runs `"$@"` in the shell), so the refusal of a
# `retry_fetch` behind either says why the form is refused all the same: retry_fetch already
# puts each attempt inside a block of its own, so the guard adds nothing and only hides the
# call. Under RETRY_BEHIND's sentence, which says the wrapper runs an executable and cannot run
# the helper, the two checks naming the guards and the check for that sentence fail.
echo "case: r36 a retry_fetch behind ci_log_guard or ci_log_guard_stderr is refused naming the guard as the shell function it is"
root=$(fixture r36-retry-behind-guard)
ln -s "$script_dir/ci-log-guard.sh" "$root/.github/scripts/ci-log-guard.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call the helper behind ci_log_guard
        run: |
          . .github/scripts/ci-retry.sh
          . .github/scripts/ci-log-guard.sh
          ci_log_guard retry_fetch "the pull request" 30 -- gh api repos/o/r/pulls/1
      - name: Call the helper behind ci_log_guard_stderr
        run: |
          . .github/scripts/ci-retry.sh
          . .github/scripts/ci-log-guard.sh
          ci_log_guard_stderr retry_fetch "the pull request" 30 -- gh api repos/o/r/pulls/1 > pull.json
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the helper behind ci_log_guard, naming it ci-log-guard.sh's shell function, which only hides the call" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Call the helper behind ci_log_guard' of job 'build' runs .retry_fetch. behind .ci_log_guard., ci-log-guard.sh's shell function, which runs the helper inside a stop-commands block of its own: retry_fetch already puts each attempt inside a block of its own, so the guard adds nothing and only hides the call from this reading")"
check "and behind ci_log_guard_stderr, in the same words" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=16::step 'Call the helper behind ci_log_guard_stderr' of job 'build' runs .retry_fetch. behind .ci_log_guard_stderr., ci-log-guard.sh's shell function, which runs the helper inside a stop-commands block of its own: retry_fetch already puts each attempt inside a block of its own, so the guard adds nothing and only hides the call from this reading")"
check "and its advice names the one composed form that passes" \
  "$(contains "$out" "behind .ci_log_guard.,.*Call .retry_fetch. as a command of its own, with nothing before it but variable assignments, or as .gh_wait_reset retry_fetch \"<what>\" <bound> -- …., the one composed form this check reads")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# Review round 37 of the pull request (dispatch://INFRA-1545). The refusal is chosen by the
# wrapper nearest the `retry_fetch`, so a `gh_wait_reset` with another wrapper in front of it gets
# RETRY_BEHIND, which until this round said `gh_wait_reset` "runs an executable and cannot run
# the helper": false of a shell function that runs its command in the shell. Behind `env` the
# call fails because `env` cannot run `gh_wait_reset`; behind `ci_log_guard` it runs and is only
# hidden. Under the old sentence the two refusal checks and the check of why each fails or hides
# fail.
echo "case: review r37 gh_wait_reset retry_fetch behind env or ci_log_guard is refused in words true of each"
root=$(fixture r37-review-wait-behind-wrapper)
gh_read_stub "$root"
ln -s "$script_dir/ci-log-guard.sh" "$root/.github/scripts/ci-log-guard.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call the wait behind env
        run: |
          . .github/scripts/ci-retry.sh
          source .github/scripts/gh-read.sh
          env gh_wait_reset retry_fetch "the pull request" 30 -- gh api repos/o/r/pulls/1 > pull.json
      - name: Call the wait behind ci_log_guard
        run: |
          . .github/scripts/ci-retry.sh
          source .github/scripts/gh-read.sh
          . .github/scripts/ci-log-guard.sh
          ci_log_guard gh_wait_reset retry_fetch "the pull request" 30 -- gh api repos/o/r/pulls/1 > pull.json
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the wait behind env, as a form this check does not read as the helper's call" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Call the wait behind env' of job 'build' runs .retry_fetch. behind .gh_wait_reset., a form this check does not read as a call of the helper, so it charges it nothing")"
check "and the wait behind ci_log_guard, in the same words" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=17::step 'Call the wait behind ci_log_guard' of job 'build' runs .retry_fetch. behind .gh_wait_reset., a form this check does not read as a call of the helper, so it charges it nothing")"
check "saying why each fails or hides: a program in front of gh_wait_reset cannot run it, a shell function only hides it" \
  "$(contains "$out" "behind one, directly or in front of .gh_wait_reset., the call fails before any attempt and fetches nothing; and a shell function in front of the call, or in front of .gh_wait_reset., only hides it from this reading")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# Review round 36 of dispatch://INFRA-1545. helper_bound_word reads the seconds word after
# `gh_wait_reset [--stderr <file>]` as it reads it after nothing, so a composed call whose seconds
# are a variable is refused naming that variable, as a bare call is (C8 below). Without the
# function's gh_wait_reset branch it reads no word there, and both refusals name `None`.
echo "case: r36 a gh_wait_reset retry_fetch call whose seconds are no number is refused naming them"
root=$(fixture r36-wait-bound-word)
gh_read_stub "$root"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read behind the wait with its seconds a variable
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          source .github/scripts/gh-read.sh
          gh_wait_reset retry_fetch "the pull request" "$READ_SECONDS" -- gh api repos/o/r/pulls/1 > pull.json
      - name: Read behind the wait and its stderr file with its seconds a variable
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          source .github/scripts/gh-read.sh
          gh_wait_reset --stderr "$RUNNER_TEMP/words" retry_fetch "the pull request" "$READ_SECONDS" -- gh api repos/o/r/pulls/1 > pull.json
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "names the seconds of gh_wait_reset retry_fetch" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::.gh api. in step 'Read behind the wait with its seconds a variable' is retried by .retry_fetch. with its seconds written as .\\\$READ_SECONDS.")"
check "and of gh_wait_reset --stderr <file> retry_fetch" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=18::.gh api. in step 'Read behind the wait and its stderr file with its seconds a variable' is retried by .retry_fetch. with its seconds written as .\\\$READ_SECONDS.")"
check "names no seconds as None" "$(is "$(contains "$out" 'written as .None.')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# W65. harness-pool.sh's two wrapper options, `setsid --wait` and `env --default-signal=INT,QUIT`,
# each run the command after them and take no word of their own, so the gate reads through them
# as through the options WRAPPER_OPTIONS held before (dispatch://INFRA-1545, round 65). Behind
# each, a curl at the floor is a site: --list lists it, and --jobs charges its 8 x 30 + 127 =
# 367 s to the job's priced key; and a bare curl is refused at the floor. With the option out of
# WRAPPER_OPTIONS the gate read no command after it and refused the option by name, so the first
# curl was refused and neither listed nor charged, and the second was refused for the option
# rather than at the floor.
for option in "setsid --wait" "env --default-signal=INT,QUIT"; do
  wrapper=${option%% *}
  echo "case: r65 W65 a curl behind $option is read through it: at the floor it is listed and charged, bare it is refused"
  root=$(fixture "r65-w65-$wrapper-floor")
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    # 60 s for the rest of the job, and the step's 367 s: 60 + 367 = 427 s -> 8 min.
    timeout-minutes: 8
    steps:
      - name: Retry a curl behind $option
        # 8 x 30 + 127 = 367 s.
        timeout-minutes: 7
        run: $option curl -fsSL --retry 7 --retry-all-errors --max-time 30 -o a https://example.com/a
YAML
  run_check "$root"
  check "$option: the curl at the floor passes" "$(is "$status" 0)"
  list_check "$root"
  check "$option: --list lists it as a curl site of its step" \
    "$(contains "$out" "^.github/workflows/case.yaml	12	curl	Retry a curl behind $option\$")"
  set +e
  run_gate null "$root" --jobs
  set -e
  check "$option: --jobs charges its 367 s to the job's key" \
    "$(contains "$out" "^.github/workflows/case.yaml	build	7	8	367	.*	Retry a curl behind $option\$")"
  root=$(fixture "r65-w65-$wrapper-bare")
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a bare curl behind $option
        run: $option curl -fsSL https://example.com/a -o a
YAML
  run_check "$root"
  check "$option: the bare curl is refused at the floor" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::.curl. in step .Run a bare curl behind $option. fetches without the floor")"
  check "$option: and nothing else" "$(is "$status|$(printf '%s\n' "$out" | grep -c '^::error')" "1|1")"
done

# W65 too. RUN_EXEMPTIONS excepts harness-pool.sh's three runs of the command a caller hands it
# from UNNAMED_RUN's refusal, keyed by the file and the exact text of the lines each command spans
# (dispatch://INFRA-1545, round 65). Every fixture carries the real harness-pool.sh, so the first
# row is every case's baseline as well. Each row fails on its mutant: with the lookup gone the
# listed runs are refused; with the file left out of the key the copy passes; with the text left
# out of it the changed run passes; and with the stale-entry rule gone the entry that matches
# nothing passes.
# pool_runs <file>: the first line of each of the three runs, as the file holds them now.
pool_runs() {
  grep -nE '"\$\{BASH_SOURCE\[0\]\}" run |^    HARNESS_POOL_DEADLINE_MS=|^    env --default-signal' "$1" |
    cut -d: -f1 | tr '\n' ' '
}
# unnamed_at <file>: the lines where the gate's output refuses a run of that file it cannot name.
unnamed_at() {
  printf '%s\n' "$out" | grep "^::error file=$1,line=[0-9]*::.* so this check cannot name the script that runs there" |
    sed -E 's/^::error file=[^,]*,line=([0-9]+)::.*/\1/' | sort -n | tr '\n' ' '
}

echo "case: r65 W65 the three runs RUN_EXEMPTIONS lists pass in harness-pool.sh"
root=$(fixture r65-w65-pool-listed)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "finds the three runs in the file" "$(is "$(pool_runs "$root/.github/scripts/harness-pool.sh" | wc -w)" 3)"
check "refuses nothing in harness-pool.sh" "$(is "$(contains "$out" 'harness-pool.sh')" false)"

echo "case: r65 W65 the same three runs in another file are refused"
root=$(fixture r65-w65-pool-copy)
cp "$script_dir/harness-pool.sh" "$root/.github/scripts/r65-pool-copy.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses each of the copy's three runs, which no entry names" \
  "$(is "$(unnamed_at .github/scripts/r65-pool-copy.sh)" "$(pool_runs "$root/.github/scripts/r65-pool-copy.sh")")"
check "and none in harness-pool.sh itself" "$(is "$(unnamed_at .github/scripts/harness-pool.sh)" "")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

echo "case: r65 W65 a listed run with one word changed is refused, and its entry, matching nothing, fails the check"
root=$(fixture r65-w65-pool-word)
rm "$root/.github/scripts/harness-pool.sh"
sed 's/"\${BASH_SOURCE\[0\]}" run "\$base"/"${BASH_SOURCE[0]}" run2 "$base"/' "$script_dir/harness-pool.sh" \
  > "$root/.github/scripts/harness-pool.sh"
run_check "$root"
changed=$(grep -n '"${BASH_SOURCE\[0\]}" run2 "$base"' "$root/.github/scripts/harness-pool.sh" | cut -d: -f1)
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "changes one line of the file" "$(is "$(diff "$script_dir/harness-pool.sh" "$root/.github/scripts/harness-pool.sh" | grep -c '^>')" 1)"
check "refuses the changed run alone" "$(is "$(unnamed_at .github/scripts/harness-pool.sh)" "$changed ")"
check "and fails the entry it no longer matches" \
  "$(contains "$out" '^::error file=.github/scripts/check-download-retry.sh,line=1::the exception for the run in .github/scripts/harness-pool.sh written .setsid --wait "${BASH_SOURCE\[0\]}" run "$base" .* matches no run this check cannot name there')"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

echo "case: r65 W65 a RUN_EXEMPTIONS entry that matches no run fails the check"
root=$(fixture r65-w65-pool-stale)
declare_in "$root" RUN_EXEMPTIONS "    RunExemption('.github/scripts/harness-pool.sh', ('  echo r65 a line no file holds',), 'a case of this harness'),"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "names the entry, its text and what it was written for" \
  "$(contains "$out" '^::error file=.github/scripts/check-download-retry.sh,line=1::the exception for the run in .github/scripts/harness-pool.sh written .echo r65 a line no file holds. matches no run this check cannot name there.* It was written for a case of this harness')"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# W65 as well: what the stated limit under THE REACH OF THIS CHECK ENDS says of a script the gate
# reads, handed to the pool. The pool's own run of its command is not read, but a script of
# .github/scripts/ named at a `harness-pool.sh start` call is a script named in a program's
# arguments, which the reach under rule 8's item 1 refuses by name where that script fetches, and
# passes where it fetches nothing (dispatch://INFRA-1545, round 65).
echo "case: r65 W65 a fetching script of .github/scripts handed to harness-pool.sh start is refused by name at the call"
root=$(fixture r65-w65-pool-fetching)
cat > "$root/.github/scripts/r65-fetch.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
printf '%s\n' '#!/usr/bin/env bash' 'echo r65 fetches nothing' > "$root/.github/scripts/r65-quiet.sh"
chmod +x "$root/.github/scripts/r65-fetch.sh" "$root/.github/scripts/r65-quiet.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Queue a fetching script in the harness pool
        run: .github/scripts/harness-pool.sh start r65-fetch .github/scripts/r65-fetch.sh
      - name: Queue a quiet script in the harness pool
        run: .github/scripts/harness-pool.sh start r65-quiet .github/scripts/r65-quiet.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the fetching script by name at its call" \
  "$(contains "$out" "^::error file=.github/workflows/case.yaml,line=8::step 'Queue a fetching script in the harness pool' of job 'build' names .github/scripts/r65-fetch.sh in an argument of .harness-pool.sh., a place this check does not read as a call, and that script fetches")"
check "passes the script that fetches nothing" "$(is "$(contains "$out" 'Queue a quiet script')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# W15. Four routes the Spec draft's review drove past the gate at a856bb35, each exit 0 under a
# one-minute key (dispatch://INFRA-1545, round 38). A literal path names a file of the tree from
# the repository's root, and a `cd`, a `pushd` or a working directory moves the shell away from
# it: `cd .github/scripts/lib && bash ../r38-tool.py`, the same `bash ../r38-tool.py` under a
# step's `working-directory` and under a job's default one, and `cd .github/scripts &&
# ./r38-plain.py`, a `.py` of shell commands with no `#!` line, each read a path no file of the
# tree has and passed. Where the directory may have moved, such a path is read by its name, as
# a variable directory is, and refused at the run. And `ash`, a shell the `#!` reading named and
# the run reading did not, now runs a file and reads a heredoc piped into it as every other
# shell does. A cd that leaves a path naming another file of the tree, and a run by its name of
# a `.py` whose first line names python3, pass.
echo "case: r37 W15 a run of an unread file after a cd or under a working-directory, and ash, are refused by name"
root=$(fixture r37-w15-moved)
mkdir -p "$root/scripts"
printf '#!/usr/bin/env python3\nprint("quiet")\n' > "$root/.github/scripts/r38-tool.py"
cp "$root/.github/scripts/r38-tool.py" "$root/scripts/r38-tool.py"
printf '. .github/scripts/ci-retry.sh\nretry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b\n' \
  > "$root/.github/scripts/r38-plain.py"
chmod +x "$root/.github/scripts/r38-tool.py" "$root/scripts/r38-tool.py" "$root/.github/scripts/r38-plain.py"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the Python program with bash after a cd
        timeout-minutes: 1
        run: cd .github/scripts/lib && bash ../r38-tool.py
      - name: Run the Python program with bash under a working directory
        timeout-minutes: 1
        working-directory: .github/scripts/lib
        run: bash ../r38-tool.py
      - name: Run the plain Python file as a command after a cd
        timeout-minutes: 1
        run: cd .github/scripts && ./r38-plain.py
      - name: Run the Python program with ash
        timeout-minutes: 1
        run: ash .github/scripts/r38-tool.py
      - name: Pipe a heredoc into ash
        timeout-minutes: 1
        run: |
          cat <<'EOF' | ash
          curl -fsSL -o b https://example.com/b
          EOF
      - name: Run what a cd leaves standing
        run: |
          cd .github/scripts && ./r38-tool.py
          cd /tmp && bash scripts/r38-tool.py
  nested:
    runs-on: ubuntu-24.04
    defaults:
      run:
        working-directory: .github/scripts/lib
    steps:
      - name: Run the Python program with bash under the job's working directory
        timeout-minutes: 1
        run: bash ../r38-tool.py
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "9	build	Run the Python program with bash after a cd	writes .\.\./r38-tool.py. as an argument of .bash." \
  "13	build	Run the Python program with bash under a working directory	writes .\.\./r38-tool.py. as an argument of .bash." \
  "19	build	Run the Python program with ash	writes ..github/scripts/r38-tool.py. as an argument of .ash." \
  "38	nested	Run the Python program with bash under the job's working directory	writes .\.\./r38-tool.py. as an argument of .bash."; do
  IFS=$'\t' read -r line job step says <<<"$refusal"
  check "refuses '$step' by name at its run" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job '$job' $says, and it names .github/scripts/r38-tool.py, a file under .github/scripts/ this check does not read as a script")"
done
check "refuses the plain Python file run as a command after a cd, at the run" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=16::step 'Run the plain Python file as a command after a cd' of job 'build' writes .\./r38-plain.py. as a command word that does not open with a .#!. line naming its own interpreter, and it names .github/scripts/r38-plain.py")"
check "refuses the heredoc piped into ash as one a shell runs" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=23::step 'Pipe a heredoc into ash' of job 'build' feeds a heredoc to .cat., whose output a pipe on the operator's line hands to a shell, and the body holds a fetch")"
check "tells each run to run the program with python3 or put a shell script in a .sh" \
  "$(is "$(printf '%s\n' "$out" | grep -c "this check does not read as a script.*Run a Python program with .python3 .github/scripts/<name>.py .*.; a shell script goes in .github/scripts/<name>.sh")" 5)"
check "passes a cd that leaves a path naming another file of the tree, and a python3 program run by its name" \
  "$(is "$(contains "$out" "step 'Run what a cd leaves standing'")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 6)"

# W16. The class as an allow-list (dispatch://INFRA-1545, round 38). Seven routes the class search
# drove past the gate at a856bb35, each exit 0 under a one-minute key, from a `.py` that opens
# `#!/usr/bin/env python3` and holds shell lines, or as named: a wrapper the gate does not know
# in front of `bash` (S6b `flock`, S7b `taskset`); a `.py` whose `#!` line names a shell the list
# of shells missed, run as a command word (S8 `#!/bin/busybox sh`, S9 `#!/usr/bin/env mksh`); a
# `.pyc` that opens `#!/usr/bin/env bash`, run as a command word (S10); the redirect that hands
# bash its program (S12); and a copy that runs (S13). So a file under .github/scripts/ this check
# does not read as a script may appear in a command only in the places
# `.github/scripts/check-download-retry.sh#unread_place` allows, and this case writes these of
# them beside the refusals, each of which passes: what `python3` runs, the filter `jq -f` or
# `--from-file` reads, a command word whose `#!` line names its own interpreter, an argument of
# `echo`, `printf` or `grep`, and the pattern `pkill -f` matches.
echo "case: r37 W16 a file under .github/scripts that this check does not read appears in a command only where the tree writes it"
root=$(fixture r37-w16-allow-list)
mkdir -p "$root/.github/scripts/__pycache__"
printf '%s\n. .github/scripts/ci-retry.sh\nretry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b\n' \
  '#!/usr/bin/env python3' > "$root/.github/scripts/zz-x.py"
printf '%s\necho quiet\n' '#!/bin/busybox sh' > "$root/.github/scripts/zz-busybox.py"
printf '%s\necho quiet\n' '#!/usr/bin/env mksh' > "$root/.github/scripts/zz-mksh.py"
printf '%s\necho quiet\n' '#!/usr/bin/env bash' > "$root/.github/scripts/__pycache__/zz-x.cpython-313.pyc"
printf '%s\n.name\n' '#!/usr/bin/env -S jq -f' > "$root/.github/scripts/zz-filter.jq"
chmod +x "$root/.github/scripts/zz-x.py" "$root/.github/scripts/zz-busybox.py" \
  "$root/.github/scripts/zz-mksh.py" "$root/.github/scripts/__pycache__/zz-x.cpython-313.pyc" \
  "$root/.github/scripts/zz-filter.jq"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the program through flock
        timeout-minutes: 1
        run: flock /tmp/zz.lock bash .github/scripts/zz-x.py
      - name: Run the program through taskset
        timeout-minutes: 1
        run: taskset -c 0 bash .github/scripts/zz-x.py
      - name: Run the busybox program
        timeout-minutes: 1
        run: .github/scripts/zz-busybox.py
      - name: Run the mksh program
        timeout-minutes: 1
        run: .github/scripts/zz-mksh.py
      - name: Run the bytecode as a command
        timeout-minutes: 1
        run: .github/scripts/__pycache__/zz-x.cpython-313.pyc
      - name: Feed the program to bash on its standard input
        timeout-minutes: 1
        run: bash /dev/stdin < .github/scripts/zz-x.py
      - name: Copy the program and run the copy
        timeout-minutes: 1
        run: |
          cp .github/scripts/zz-x.py "$RUNNER_TEMP/zz"
          "$RUNNER_TEMP/zz"
      - name: Write the files where this tree writes them
        run: |
          python3 .github/scripts/zz-x.py
          .github/scripts/zz-x.py
          jq -f .github/scripts/zz-filter.jq data.json
          jq --from-file .github/scripts/zz-filter.jq data.json
          .github/scripts/zz-filter.jq data.json
          echo .github/scripts/zz-x.py
          printf '%s\n' .github/scripts/zz-x.py
          grep -n retry_fetch .github/scripts/zz-x.py
          pkill -f zz-x.py || true
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "9	Run the program through flock	zz-x.py	as an argument of .flock." \
  "12	Run the program through taskset	zz-x.py	as an argument of .taskset." \
  "15	Run the busybox program	zz-busybox.py	as a command word that does not open with a .#!. line naming its own interpreter" \
  "18	Run the mksh program	zz-mksh.py	as a command word that does not open with a .#!. line naming its own interpreter" \
  "21	Run the bytecode as a command	__pycache__/zz-x.cpython-313.pyc	as a command word that does not open with a .#!. line naming its own interpreter" \
  "24	Feed the program to bash on its standard input	zz-x.py	as a file a redirect of .bash. names" \
  "28	Copy the program and run the copy	zz-x.py	as an argument of .cp."; do
  IFS=$'\t' read -r line step name where <<<"$refusal"
  check "refuses '$step' by name, the file written $where" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' writes ..github/scripts/$name. $where, and it names .github/scripts/$name, a file under .github/scripts/ this check does not read as a script\. Such a file appears in a command only where this tree writes one.*Run a Python program with .python3 .github/scripts/<name>.py .*.; a shell script goes in .github/scripts/<name>.sh")"
done
for program in "zz-busybox.py	#!/bin/busybox sh	busybox" "zz-mksh.py	#!/usr/bin/env mksh	mksh"; do
  IFS=$'\t' read -r name shebang runs <<<"$program"
  check "refuses .github/scripts/$name by name at its line 1, its first line naming $runs" \
    "$(contains "$out" "::error file=.github/scripts/$name,line=1::.github/scripts/$name is a .\.py. directly in .github/scripts/ whose first line, .$shebang., runs .$runs., not .python3. or .python.")"
done
check "refuses the bytecode that opens #! by name at its line 1" \
  "$(contains "$out" "::error file=.github/scripts/__pycache__/zz-x.cpython-313.pyc,line=1::.github/scripts/__pycache__/zz-x.cpython-313.pyc is Python's bytecode under .github/scripts/__pycache__/ by its name, and it opens .#!.")"
for line in "32	python3 runs it" "33	a command word whose first line names python3" \
  "34	jq -f reads it" "35	jq --from-file reads it" "36	a command word whose first line names jq" \
  "37	echo prints it" "38	printf prints it" "39	grep searches it" "40	pkill -f matches it"; do
  IFS=$'\t' read -r at form <<<"$line"
  check "passes the file where $form" \
    "$(is "$(contains "$out" "file=.github/workflows/case.yaml,line=$at::")" false)"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 10)"

# The command an `xargs` runs is one of those places, a command word (unread_uses): xargs runs it
# on the words it reads, through its `#!` line, as a shell runs a command word. Main's #5583 split
# the document service's race step into legs that write `… | xargs -r env GODEBUG=…
# RACE_MEM_LIMIT_KB=… ../../.github/scripts/race_memory.py go test …`, and until the forward merge
# of that change the gate read the program there as an argument of `xargs` and refused it. So a
# literal `.py` whose first line names python3 passes behind `xargs -r env A=b`, one with no `#!`
# line is refused there as the command word it is, and the same xargs handed a path built from
# data is still refused at the run (unnamed_runs reads what xargs runs, through the same
# xargs_command) and at the assignment that builds it.
echo "case: a program xargs runs behind env is read as a command word, and a built path there is refused"
root=$(fixture xargs-command-word)
printf '%s\nprint("quiet")\n' '#!/usr/bin/env python3' > "$root/.github/scripts/zz-wrap.py"
printf 'print("plain")\n' > "$root/.github/scripts/zz-plain.py"
chmod +x "$root/.github/scripts/zz-wrap.py"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Hand the program to xargs behind env
        run: |
          set -o pipefail
          printf '%s\n' a b | xargs -r env A=b C=d .github/scripts/zz-wrap.py --each
      - name: Hand a built path to xargs behind env
        run: |
          tool=.github/scripts/zz-wrap.py
          printf '%s\n' a b | xargs -r env A=b "$tool" --each
      - name: Hand a program with no first line to xargs behind env
        run: printf '%s\n' a b | xargs -r env A=b .github/scripts/zz-plain.py --each
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "passes the literal program whose first line names python3, run by xargs behind env" \
  "$(is "$(contains "$out" "step 'Hand the program to xargs behind env'")" false)"
check 'refuses the built path xargs runs at line 14, naming the step, the job, xargs and $tool' \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=14::' \
    'Hand a built path to xargs behind env' build xargs '$tool')"
check "and the assignment that builds it at line 13, naming the step, the job, the assignment and its path twice" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=13::' \
    'Hand a built path to xargs behind env' build 'tool=.github/scripts/zz-wrap.py' .github/scripts/zz-wrap.py)"
check "refuses at line 16 the program with no first line as the command word xargs runs, naming the step, the job, its path twice and #!" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=16::' \
    'Hand a program with no first line to xargs behind env' build .github/scripts/zz-plain.py \
    .github/scripts/zz-plain.py '#!')"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# R74 xargs. An option of xargs that xargs_command does not read is refused by name, at the
# commands odd_command reads and whatever xargs runs (dispatch://INFRA-1545, round 74): review
# 6085108423's three inputs, a cluster before a literal program, the same cluster before a shell
# that runs each word xargs reads, and a long option whose value is a word of its own before that
# shell. Until round 74 each such option was stepped past as one taking no value, so `1` read as
# the command xargs runs: both shells passed, and the program was refused as an argument of `1`.
# Now the option's refusal stands alone, citing round 74 in its own sentence: until round 75 the
# program was refused a second time, as an argument of `xargs` (dispatch://INFRA-1545, round 75).
# The same options as words of their own are read: the program, whose first line names python3,
# passes, and the shell is refused for the run it makes, which the gate read before round 74 too.
# Each refusal row is checked by its line, the program it names and the option, never by the
# words of the refusal, and each fails where the option is stepped past again.
echo "case: r74 an option of xargs this check does not read is refused by name, and the same options as words of their own are read"
root=$(fixture r74-xargs-options)
printf '%s\nprint("quiet")\n' '#!/usr/bin/env python3' > "$root/.github/scripts/zz-r74.py"
chmod +x "$root/.github/scripts/zz-r74.py"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A cluster before a literal program
        run: printf '%s\n' a b | xargs -rn 1 .github/scripts/zz-r74.py
      - name: A cluster before a shell
        run: xargs -rn 1 bash < names.txt
      - name: A long option and its value before a shell
        run: xargs --max-args 1 bash < names.txt
      - name: Separate options before a literal program
        run: printf '%s\n' a b | xargs -r -n 1 .github/scripts/zz-r74.py
      - name: Separate options before a shell
        run: xargs -r -n 1 bash < names.txt
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "8	-rn" "10	-rn" "12	--max-args"; do
  IFS=$'\t' read -r line option <<<"$refusal"
  check "refuses the option $option of xargs at line $line by name" \
    "$(names_at "$out" "::error file=.github/workflows/case.yaml,line=$line::" '`xargs`' "\`$option\`")"
  check "cites round 74 for the option $option at line $line, not round 37 or 38" \
    "$(names_at "$out" "::error file=.github/workflows/case.yaml,line=$line::" "\`$option\`" \
      'round 74' --not 'round 37' 'round 38')"
done
check "refuses the program at line 8 in no refusal of its own: the option's refusal stands alone" \
  "$(is "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' \
    '`.github/scripts/zz-r74.py`')" false)"
check "passes the program after the options written as words of their own, at line 14" \
  "$(is "$(contains "$out" 'case.yaml,line=14::')" false)"
check "reads the shell after the options written as words of their own, and refuses its run at line 16" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=16::' '`xargs`' '`bash`')"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# R74 class. The other readings that stepped past an option they did not know before the command,
# the operand or the interpreter they look for fail closed the same way, each refused by name
# (dispatch://INFRA-1545, round 74). timeout_option, the one reading of timeout's options that
# reduce_words and odd_command now share: `--kill`, an abbreviation of `--kill-after` that takes
# the next word, read 5 as the duration and 60 as the command, so the bare curl behind it was no
# fetch. shell_operand: `-eo` holds `o`, which takes the next word, so `pipefail` read as the
# script and the path built from data passed. interpreter_of: `-a` is an option of env that takes
# the next word, its zeroth argument, so the first line read as python3 where such an env runs
# bash. Each control is the form the reading reads: the joined `--signal=` and `--kill-after=`
# and `--foreground` the tree writes, read through to the curl, which is refused at the floor
# alone; `-o` as a word of its own, read through to the built path, which is refused as the run
# it is; and `-u <name>` before python3, which passes.
echo "case: r74 an option of timeout, a shell's group holding o and an option of env in a first line, each unread, are refused by name"
root=$(fixture r74-option-readers)
printf '%s\nprint("quiet")\n' '#!/usr/bin/env -S -a python3 bash' > "$root/.github/scripts/zz-r74-argv0.py"
printf '%s\nprint("quiet")\n' '#!/usr/bin/env -u HOME python3' > "$root/.github/scripts/zz-r74-unset.py"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch behind an abbreviated option that takes a value
        run: timeout --kill 5 60 curl -fsSL -o a https://example.com/a
      - name: Fetch behind the joined options the tree writes
        timeout-minutes: 11
        run: timeout --foreground --signal=TERM --kill-after=5 60 curl -fsSL -o a https://example.com/a
      - name: Run a built path after a group holding o
        run: bash -eo pipefail "$SCRIPT"
      - name: Run a built path after o as a word of its own
        run: bash -e -o pipefail "$SCRIPT"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the option --kill of timeout at line 8 by name" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' '`timeout`' '`--kill`')"
check "reads the curl at line 11 through the options the tree writes, and refuses it at the floor" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=11::' '`curl`' '`--retry 7`')"
check "refuses no option of timeout at line 11" \
  "$(is "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=11::' '`timeout`')" false)"
check "refuses the group -eo of bash at line 13 by name" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=13::' '`bash`' '`-eo`')"
for refusal in "8	--kill" "13	-eo"; do
  IFS=$'\t' read -r line option <<<"$refusal"
  check "cites round 74 for the option $option at line $line, not round 37 or 38" \
    "$(names_at "$out" "::error file=.github/workflows/case.yaml,line=$line::" "\`$option\`" \
      'round 74' --not 'round 37' 'round 38')"
done
check "reads the built path after -o written apart at line 15, and refuses its run" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=15::' '`$SCRIPT`' '`bash`')"
check "refuses zz-r74-argv0.py at its line 1, naming env and its option -a" \
  "$(names_at "$out" '::error file=.github/scripts/zz-r74-argv0.py,line=1::' '`env -a`')"
check "passes zz-r74-unset.py, whose env option it reads" \
  "$(is "$(contains "$out" 'zz-r74-unset.py')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"

# R74 verbs. The verb reading steps past an option before a tool's subcommand only where
# LEADING_OPTIONS reads it with its value, and refuses any other such option by name.
# The gate reads `go -C <dir> mod download` and refuses it without a retry.
# The same call under `retry_fetch` is charged 5 x 180 + 150 = 1050 s, so a one-minute key is
# refused and a key of 18 minutes passes. Each option the table does not read is refused by name,
# a real one of its tool each time: go's `-C=<dir>`, in a step and in a Dockerfile RUN; docker's
# `--config <dir>`, whose value read as the subcommand until round 74, in a step and in a
# `bash -c` text; and bunx's `--bun` before the program it runs, which read as the program.
# `go -C <dir> env` in a RUN, `docker --version` and `gh --version` pass. Each refusal row fails
# where the reading steps past the option as one taking no value again, and each row of
# `go -C <dir>`, the RUN's among them, fails where the table loses `-C`.
echo "case: r74 an option before a tool's subcommand is read only where LEADING_OPTIONS reads it, and refused by name otherwise"
root=$(fixture r74-verb-options)
printf '%s\n' 'RUN go -C=/src/packages/envoy mod download' \
  'RUN go -C /src/packages/envoy env GOMOD' >> "$root/packages/daemon/docker/worker.Dockerfile"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Download the Go modules with no retry
        run: go -C packages/envoy mod download
      - name: Download the Go modules under a key that cannot hold them
        timeout-minutes: 1
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the Go modules" 180 -- go -C packages/envoy mod download
      - name: Download the Go modules under a key that holds them
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the Go modules" 180 -- go -C packages/envoy mod download
      - name: Download the Go modules with the directory joined to its option
        run: go -C=packages/envoy mod download
      - name: Pull an image after a global option that takes a value
        run: docker --config "$RUNNER_TEMP/docker-config" pull postgres:16
      - name: Pull an image after that option in a bash -c text
        run: bash -c 'docker --config "$RUNNER_TEMP/docker-config" pull postgres:16'
      - name: Install a browser after an option of the package runner
        run: bunx --bun playwright install chromium
      - name: Ask the tools for their versions
        run: |
          docker --version
          gh --version
      - name: A leading option can take an informational word as its value
        run: docker --config --version pull postgres:16
      - name: A Go directory can look like an informational flag
        run: go -C --version mod download
      - name: Ask Bun for its binary revision
        run: bun --revision
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "reads the unretried go -C <dir> at line 8 through to go mod download, and refuses it for no retry" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=8::' '`go mod download`' \
    "'Download the Go modules with no retry'")"
check "charges the retried go -C <dir> its 1050 s, so its one-minute key at line 10 is refused" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=10::' \
    "'Download the Go modules under a key that cannot hold them'" '`timeout-minutes: 1`' '1050 s')"
check "passes the same retried call under a key of 18 minutes, which holds its 1050 s" \
  "$(is "$(contains "$out" 'Download the Go modules under a key that holds them')" false)"
for refusal in "20	go	-C=packages/envoy" "22	docker	--config" "24	docker	--config" \
  "26	bunx	--bun"; do
  IFS=$'\t' read -r line tool option <<<"$refusal"
  check "refuses the option $option before the subcommand of $tool at line $line by name" \
    "$(names_at "$out" "::error file=.github/workflows/case.yaml,line=$line::" "\`$tool\`" \
      "\`$option\`")"
  check "cites round 74 for the option $option of $tool at line $line, not round 37 or 38" \
    "$(names_at "$out" "::error file=.github/workflows/case.yaml,line=$line::" "\`$option\`" \
      'round 74' --not 'round 37' 'round 38')"
done
# The advice names a fix that works for the tool (dispatch://INFRA-1545, round 75): go takes `-C`
# before its subcommand alone, so the joined `-C=<dir>` is told to write `go -C <dir>`, the form
# the table reads, and is not told to write the option after the subcommand; docker's `--config`,
# which the table does not read, is still told either fix. The Go row names `go -C <dir>` twice,
# once in the list the table reads and once in the advice, and fails where the joined form gets
# the advice of an option the table does not read.
check "advises at line 20 the Go option as separate words, the form LEADING_OPTIONS reads, and not after the subcommand" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=20::' '`go -C <dir>`' \
    '`go -C <dir>`' 'LEADING_OPTIONS' --not 'after its subcommand')"
check "advises at line 22 docker's option after its subcommand or in LEADING_OPTIONS" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=22::' '`docker`' \
    '`--config`' 'after its subcommand' 'LEADING_OPTIONS')"
check "refuses the Dockerfile RUN's joined -C at its line 3 by name" \
  "$(names_at "$out" '::error file=packages/daemon/docker/worker.Dockerfile,line=3::' '`go`' \
    '`-C=/src/packages/envoy`')"
check "cites round 74 for the Dockerfile RUN's joined -C at its line 3, not round 37 or 38" \
  "$(names_at "$out" '::error file=packages/daemon/docker/worker.Dockerfile,line=3::' \
    '`-C=/src/packages/envoy`' 'round 74' --not 'round 37' 'round 38')"
check "advises the Dockerfile RUN's joined -C at its line 3 as separate words, and not after the subcommand" \
  "$(names_at "$out" '::error file=packages/daemon/docker/worker.Dockerfile,line=3::' \
    '`go -C <dir>`' '`go -C <dir>`' --not 'after its subcommand')"
check "passes the Dockerfile RUN that runs go env after -C <dir>" \
  "$(is "$(contains "$out" 'worker.Dockerfile,line=4::')" false)"
check "passes docker --version and gh --version, which answer about themselves" \
  "$(is "$(contains "$out" 'case.yaml,line=29::\|case.yaml,line=30::')" false)"
check "refuses the leading option even when its next argument looks like a version flag" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=32::' '`docker`' '`--config`')"
check "reads the Go download after a directory that looks like a version flag" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=34::' '`go mod download`')"
check "passes the revision read used by setup-bun" \
  "$(is "$(contains "$out" 'case.yaml,line=36::')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 9)"

# R74 verbs, the other two readings of a command's words. A call that hands its words to a loop
# that runs them is read for the verb among those words (verb_in), and where none is read, a tool
# among them that writes an option before its subcommand LEADING_OPTIONS does not read is refused
# by name (unread_in): until round 74 the value of docker's `--config` read as its subcommand
# there too, and the loop's pull as no fetch. The same pull with its option after the subcommand
# is read, and the loop around it is refused as a copy of the helper. A command in the body of a
# heredoc a shell runs is refused by name for such an option too, at its own line: the body's
# reading reads no site past it, so the heredoc's own question would pass it. Each refusal row
# fails where its reading drops the option again.
echo "case: r74 an option before a tool's subcommand that a call hands a loop, or that a heredoc's body writes, is refused by name"
root=$(fixture r74-verb-options-reach)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Hand the loop a pull after a global option that takes a value
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
          attempts 2 "the image" docker --config "$RUNNER_TEMP/docker-config" pull postgres:16
      - name: Hand the loop a pull with its option after the subcommand
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
          attempts 2 "the image" docker pull --quiet postgres:16
      - name: Pull an image after a global option in a heredoc a shell runs
        run: |
          bash <<'EOF'
          docker --debug pull postgres:16
          EOF
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses by name the option --config of docker that the call at line 19 hands the loop" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=19::' '`docker`' '`--config`')"
check "reads the pull whose option follows the subcommand, and refuses the loop at line 25 that retries it" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=25::' '`docker pull`')"
check "refuses the pull the call at line 32 hands the loop, which no retry_fetch holds" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=32::' '`docker pull`' \
    "'Hand the loop a pull with its option after the subcommand'")"
check "refuses by name the option --debug of docker at line 36, in the body of a heredoc a shell runs" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=36::' '`docker`' '`--debug`')"
for refusal in "19	--config" "36	--debug"; do
  IFS=$'\t' read -r line option <<<"$refusal"
  check "cites round 74 for the option $option at line $line, not round 37 or 38" \
    "$(names_at "$out" "::error file=.github/workflows/case.yaml,line=$line::" "\`$option\`" \
      'round 74' --not 'round 37' 'round 38')"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 4)"

# W17. Six glob shapes the Spec draft's review drove past the gate at bd06b2a2, each exit 0 under
# a one-minute key (dispatch://INFRA-1545, round 38): a glob or a brace expansion in a run's
# target names the file that runs from the names the file system holds as the step runs, as a
# variable names one from what it holds, and none of them was read as one. A `?`, a bracket and a
# brace in the target's last element, by bash, as a command word and in a `bash -c` text, are
# refused as a run built from data; a `*` in its directory names the file by its name. Beside them
# a glob character in quotes or after a backslash is that character, as bash reads it, so bash
# opens no such file and runs nothing, and passes; and `rm -rf "$RUNNER_TEMP"/*`, a glob in an
# argument with no literal `.github/`, and a `case` pattern or a piece of a `[[ … =~ … ]]` regex,
# which CallScanner reads as commands of their own, pass too. `ls .github/scripts/*.sh` passed
# here until the word reading of W18: it could name a CI script that fetches, so it is refused by
# name where it stands, as every such word is, and this case's count went from 11 to 12. Two
# quoting shapes of the same review passed the same way: `bash $'.github/scripts/x.py'`, whose
# `$'…'` this check read as a `$` and a quote, so it named no file, and a command word
# `.github/scripts/x."sh"`, whose name SCRIPT_CALL never found in the text as written, so the
# 1050 s of its script were charged nothing. A run's target is now read as bash reads it, its
# quotes removed and a `$'…'`'s escapes read: the first is refused by name as the same path
# unquoted is; the second is charged its script's 1050 s, so it is refused under its one-minute
# key and passes under a key of 18 that holds them, as the same path unquoted would, and so is a
# command word whose `$'…'` spells the dot as `\x2e`; and a backslash in a name in a `bash -c`
# text is read the same way, refused as that script's run in the text of a shell is. `python3`
# running the program written in `$'…'` with that escape passes, as `python3` running it plainly
# does.
echo "case: r37 W17 a path holding a glob or a brace expansion is read as built at run time and refused by name"
root=$(fixture r37-w17-globs)
printf '%s\n. .github/scripts/ci-retry.sh\nretry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b\n' \
  '#!/usr/bin/env bash' > "$root/.github/scripts/r38-fetch.sh"
printf '#!/usr/bin/env python3\nprint("quiet")\n' > "$root/.github/scripts/r38-tool.py"
chmod +x "$root/.github/scripts/r38-fetch.sh" "$root/.github/scripts/r38-tool.py"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the fetching script by a question mark
        timeout-minutes: 1
        run: bash .github/scripts/r38-fetc?.sh
      - name: Run the fetching script by a bracket as a command
        timeout-minutes: 1
        run: .github/scripts/r38-fetch.[s]h
      - name: Run the Python program by a question mark
        timeout-minutes: 1
        run: bash .github/scripts/r38-tool.p?
      - name: Run the Python program by a bracket
        timeout-minutes: 1
        run: bash .github/scripts/r38-tool.[p]y
      - name: Run the Python program by a brace expansion
        timeout-minutes: 1
        run: bash .github/scripts/r38-tool.{py,x}
      - name: Run the Python program through a star in its directory
        timeout-minutes: 1
        run: bash .github/scr*/r38-tool.py
      - name: Run the fetching script by a question mark in a bash -c text
        timeout-minutes: 1
        run: bash -c 'bash .github/scripts/r38-fetc?.sh'
      - name: Run the Python program in ANSI-C quotes
        timeout-minutes: 1
        run: bash $'.github/scripts/r38-tool.py'
      - name: Run the fetching script with a quoted part in its name as a command
        timeout-minutes: 1
        run: .github/scripts/r38-fetch."sh"
      - name: Run the fetching script with an escape in its name in a bash -c text
        timeout-minutes: 1
        run: bash -c '.github/scripts/r38-fetch\.sh'
      - name: Run the fetching script with an escape in ANSI-C quotes as a command
        timeout-minutes: 1
        run: $'.github/scripts/r38-fetch\x2esh'
      - name: Run the fetching script with a quoted part in its name under a key that holds it
        timeout-minutes: 18
        run: .github/scripts/r38-fetch."sh"
      - name: Write glob characters where no run is
        run: |
          bash ".github/scripts/r38-fetc?.sh"
          bash .github/scripts/r38-fetc\?.sh
          ls .github/scripts/*.sh
          rm -rf "$RUNNER_TEMP"/*
          python3 .github/scripts/r38-tool.py
          python3 $'.github/scripts/r38-tool\x2epy'
          case "$f" in
            # anything else
            *) echo other ;;
          esac
          [[ $n =~ ^([0-9]{1,6})$ ]] && echo number
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "9	Run the fetching script by a question mark	runs ..github/scripts/r38-fetc.\.sh. with .bash., which ends in .\.sh. and holds an expansion" \
  "12	Run the fetching script by a bracket as a command	runs ..github/scripts/r38-fetch\..s.h. as a command, which names a file under .github/scripts/ through an expansion" \
  "15	Run the Python program by a question mark	runs ..github/scripts/r38-tool\.p.. with .bash., which names a file under .github/scripts/ through an expansion" \
  "18	Run the Python program by a bracket	runs ..github/scripts/r38-tool\..p.y. with .bash., which names a file under .github/scripts/ through an expansion" \
  "21	Run the Python program by a brace expansion	runs ..github/scripts/r38-tool\..py,x.. with .bash., which names a file under .github/scripts/ through an expansion" \
  "27	Run the fetching script by a question mark in a bash -c text	runs .bash -c. on text that runs .bash .github/scripts/r38-fetc.\.sh., which ends in .\.sh. and holds an expansion"; do
  IFS=$'\t' read -r line step says <<<"$refusal"
  check "refuses '$step' by name at its run, as a run built from data" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' $says, so this check cannot name the script that runs there")"
done
check "refuses the star in the program's directory by the program's name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=24::step 'Run the Python program through a star in its directory' of job 'build' writes ..github/scr.*/r38-tool\.py. as an argument of .bash., and it names .github/scripts/r38-tool.py, a file under .github/scripts/ this check does not read as a script")"
check "refuses the Python program in ANSI-C quotes by name, as the same path unquoted is" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=30::step 'Run the Python program in ANSI-C quotes' of job 'build' writes ..github/scripts/r38-tool\.py. as an argument of .bash., and it names .github/scripts/r38-tool.py, a file under .github/scripts/ this check does not read as a script")"
check "charges the script a quoted part of its name runs its 1050 s, so its one-minute key is refused" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=32::step 'Run the fetching script with a quoted part in its name as a command' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust, which that budget does not strictly exceed\. That arithmetic is the step's own and that of .github/scripts/r38-fetch\.sh, which it runs")"
check "refuses the escaped name in a bash -c text where its script fetches" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=36::step 'Run the fetching script with an escape in its name in a bash -c text' of job 'build' names .github/scripts/r38-fetch\.sh in an argument of .bash., a place this check does not read as a call, and that script fetches")"
check "charges the script an escape in ANSI-C quotes names its 1050 s, so its one-minute key is refused" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=38::step 'Run the fetching script with an escape in ANSI-C quotes as a command' carries .timeout-minutes: 1., 60 s, and its own retries take 1050 s to exhaust, which that budget does not strictly exceed\. That arithmetic is the step's own and that of .github/scripts/r38-fetch\.sh, which it runs")"
check "passes the quoted part of a name under a key that holds its script's 1050 s" \
  "$(is "$(contains "$out" "step 'Run the fetching script with a quoted part in its name under a key that holds it'")" false)"
check "refuses the star in an argument of ls, which could name a CI script that fetches, and gives the argument its own reason and the advice that passes" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=47::step 'Write glob characters where no run is' of job 'build' writes ..github/scripts/.\.sh. as an argument of .ls., which could name .github/scripts/[a-z-]*\.sh, .* a script that fetches or a file this check does not read, since bash fills in its patterns and expansions from the files and the values the step holds as it runs, which this check does not see\. This check refuses such a word wherever its literal text holds .\.github/., but as an argument of .echo. or .printf. .*so it cannot tell whether those this word names run here\. List the directory with no pattern and match its names as text")"
for line in "45	a question mark in quotes" "46	a question mark after a backslash" \
  "48	a star in an argument of rm" "49	python3 runs it" \
  "50	python3 runs it in ANSI-C quotes" "53	a case pattern after a comment line" \
  "55	a piece of a [[ =~ ]] regex"; do
  IFS=$'\t' read -r at form <<<"$line"
  check "passes $form" "$(is "$(contains "$out" "file=.github/workflows/case.yaml,line=$at::")" false)"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 12)"

# W18. Seventeen shapes the class search drove past the gate at 548d180b, each exit 0 under a
# one-minute key (dispatch://INFRA-1545, round 38): a word that is no literal path, its globs,
# braces and expansions filled in as the step runs, named zz-c.sh, a CI script whose retries take
# 1050 s, or zz-x.py, a Python program holding shell text, through a patterned or expanded
# directory the run-target reading took for a tool's, through `. --`, behind a program this check
# does not read as a run (`xargs`, `find -exec`, `flock`, `taskset`, a set with no end), or inside
# a `$( … )` on a line a `[[` opens or a quoted `[[` stands on, which pattern_place read as a
# pattern. The rule is read by the word, not the program: a word that could name a CI script that
# fetches or a file this check does not read is refused by name wherever it stands, read at a
# run's target by its last element and anywhere else where its literal text holds `.github/`
# (pattern_words); and a command inside a `$( … )` is a run, and a quoted `[[` opens no test, so
# the three `[[` shapes are runs the target reading refuses as it refuses W17's. Beside them, three
# rows prove what the rule rests on, that a word with no literal `.github/` names a file there
# only through a variable, and that the variable is refused where the step sets it to such a
# path: `flock /tmp/l "$s"` and `timeout 5 "$s"` after `s=.github/scripts/zz-c.sh`, and
# `bash scripts/zz-c.sh` after `cd .github`, a name SCRIPT_CALL reads. The three were refused at
# 548d180b already; since the variable reading below, `flock /tmp/l "$s"` is refused at the run
# too. Round 38's next search found four more past 2eab0637, each exit 0 under a one-minute key:
# a redirect's file read as no word, `bash /dev/stdin < .github/scripts/zz-c.s?` and the same
# into r38-plain.py, a `.py` of shell text with no `#!` line; and a directory the step sets
# literally, `d=.github/scripts`, then `flock /tmp/l "$d"/zz-c.s[h]` and `cp
# "$d"/r38-plain.p? "$RUNNER_TEMP/zz"` before running the copy, a word with no literal
# `.github/`. A redirect's file is read as any other word is, and a variable the same text sets
# once to a literal value is read as that value. A literal tool path, a glob naming no CI script,
# a glob with no `.github/`, an argument of `echo` and a literal redirect's file pass.
echo "case: r37 W18 a word that could name a CI script that fetches is refused by name wherever it stands"
root=$(fixture r37-w18-words)
printf '%s\n. .github/scripts/ci-retry.sh\nretry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b\n' \
  '#!/usr/bin/env bash' > "$root/.github/scripts/zz-c.sh"
printf '%s\n. .github/scripts/ci-retry.sh\nretry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b\n' \
  '#!/usr/bin/env python3' > "$root/.github/scripts/zz-x.py"
printf '%s\n' '. .github/scripts/ci-retry.sh' \
  'retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b' \
  > "$root/.github/scripts/r38-plain.py"
chmod +x "$root/.github/scripts/zz-c.sh" "$root/.github/scripts/zz-x.py"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the script through a star in its directory with bash
        timeout-minutes: 1
        run: |
          bash .github/scr*/zz-c.s[h]
      - name: Run the script through a directory variable with bash
        timeout-minutes: 1
        run: |
          d=.github/scripts; bash "$d"/zz-c.s[h]
      - name: Run the script through a question mark with bash
        timeout-minutes: 1
        run: |
          bash .github/scr*/zz-c.s?
      - name: Run the script through a bracket in its directory as a command
        timeout-minutes: 1
        run: |
          .github/scr[i]pts/zz-c.s[h]
      - name: Source the script through stars
        timeout-minutes: 1
        run: |
          . .github/*/zz-c.s*
      - name: Run the script through a directory variable as a command
        timeout-minutes: 1
        run: |
          d=.github/scripts; "$d"/zz-c.s?
      - name: Run the Python program through a star with bash
        timeout-minutes: 1
        run: |
          bash .github/scr*/zz-x.p[y]
      - name: Source the script after a double dash
        timeout-minutes: 1
        run: |
          . -- .github/scripts/zz-c.s[h]
      - name: Run the script inside a test's substitution
        timeout-minutes: 1
        run: |
          [[ -n "$(.github/scripts/zz-c.s[h])" ]]
      - name: Run the script inside a second test's substitution
        timeout-minutes: 1
        run: |
          [[ -n "$(echo ok)" ]] && [[ -n "$(.github/scripts/zz-c.s[h])" ]]
      - name: Run the script after a quoted test opener
        timeout-minutes: 1
        run: |
          echo '[['; .github/scripts/zz-c.s[h]
      - name: Hand the script to xargs
        timeout-minutes: 1
        run: |
          true | xargs .github/scripts/zz-c.s[h]
      - name: Hand the script to find
        timeout-minutes: 1
        run: |
          find . -maxdepth 0 -exec .github/scripts/zz-c.s[h] \;
      - name: Hand the script to flock
        timeout-minutes: 1
        run: |
          flock /tmp/zz.lock .github/scripts/zz-c.s[h]
      - name: Hand the script to taskset
        timeout-minutes: 1
        run: |
          taskset -c 0 .github/scripts/zz-c.s[h]
      - name: Hand the script to xargs with a placeholder
        timeout-minutes: 1
        run: |
          echo x | xargs -I{} .github/scripts/zz-c.s[h] {}
      - name: Hand the script to flock with a backslash in its name
        timeout-minutes: 1
        run: |
          flock /tmp/zz.lock .github/scripts/zz-c.s\h
      - name: Hand a variable naming the script to flock
        timeout-minutes: 1
        run: |
          s=.github/scripts/zz-c.sh
          flock /tmp/l "$s"
      - name: Run a variable naming the script behind timeout
        timeout-minutes: 1
        run: |
          s=.github/scripts/zz-c.sh
          timeout 5 "$s"
      - name: Run the script by a relative path after a cd
        timeout-minutes: 1
        run: |
          s=.github/scripts/zz-c.sh
          cd .github && bash scripts/zz-c.sh
      - name: Feed the script to bash through a question mark in a redirect
        timeout-minutes: 1
        run: |
          bash /dev/stdin < .github/scripts/zz-c.s?
      - name: Feed the Python file to bash through a question mark in a redirect
        timeout-minutes: 1
        run: |
          bash /dev/stdin < .github/scripts/r38-plain.p?
      - name: Hand the script to flock through a directory the step sets
        timeout-minutes: 1
        run: |
          d=.github/scripts
          flock /tmp/l "$d"/zz-c.s[h]
      - name: Copy the Python file through a directory the step sets and run the copy
        timeout-minutes: 1
        run: |
          d=.github/scripts
          cp "$d"/r38-plain.p? "$RUNNER_TEMP/zz"
          "$RUNNER_TEMP/zz"
      - name: Write words that name no CI script
        timeout-minutes: 1
        run: |
          "$HOME/.local/bin/mise" --version
          ls .github/workflows/*.yaml
          ls packages/*/package.json
          echo .github/scripts/zz-c.s[h]
          wc -l < .github/workflows/ci.yaml
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
could="which could name .github/scripts/zz-c\.sh, a script that fetches or a file this check does not read"
for refusal in \
  "10	Run the script through a star in its directory with bash	runs ..github/scr./zz-c\.s.h.. with .bash., $could" \
  "14	Run the script through a directory variable with bash	runs .\$d/zz-c\.s.h.. with .bash., $could" \
  "18	Run the script through a question mark with bash	runs ..github/scr./zz-c\.s.. with .bash., $could" \
  "22	Run the script through a bracket in its directory as a command	runs ..github/scr.i.pts/zz-c\.s.h.. as a command, $could" \
  "26	Source the script through stars	sources ..github/./zz-c\.s.., $could" \
  "30	Run the script through a directory variable as a command	runs .\$d/zz-c\.s.. as a command, $could" \
  "34	Run the Python program through a star with bash	runs ..github/scr./zz-x\.p.y.. with .bash., which could name .github/scripts/zz-x\.py, a script that fetches or a file this check does not read" \
  "38	Source the script after a double dash	writes ..github/scripts/zz-c\.s.h.. as an argument of .\.., $could" \
  "54	Hand the script to xargs	writes ..github/scripts/zz-c\.s.h.. as an argument of .xargs., $could" \
  "58	Hand the script to find	writes ..github/scripts/zz-c\.s.h.. as an argument of .find., $could" \
  "62	Hand the script to flock	writes ..github/scripts/zz-c\.s.h.. as an argument of .flock., $could" \
  "66	Hand the script to taskset	writes ..github/scripts/zz-c\.s.h.. as an argument of .taskset., $could" \
  "70	Hand the script to xargs with a placeholder	writes ..github/scripts/zz-c\.s.h.. as an argument of .xargs., $could" \
  "74	Hand the script to flock with a backslash in its name	writes ..github/scripts/zz-c\.sh. as an argument of .flock., $could"; do
  IFS=$'\t' read -r line step says <<<"$refusal"
  check "refuses '$step' by name, as a word that could name a CI script that fetches" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' $says")"
done
for refusal in "42	Run the script inside a test's substitution" \
  "46	Run the script inside a second test's substitution" \
  "50	Run the script after a quoted test opener"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' at its run, which a test or a quoted test opener no longer hides" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' runs ..github/scripts/zz-c\.s.h.. as a command, which names a file under .github/scripts/ through an expansion, so this check cannot name the script that runs there")"
done
for refusal in \
  "78	Hand a variable naming the script to flock	names .github/scripts/zz-c\.sh in a variable assignment, a place this check does not read as a call, and that script fetches" \
  "79	Hand a variable naming the script to flock	writes .\$s. as an argument of .flock., $could" \
  "83	Run a variable naming the script behind timeout	names .github/scripts/zz-c\.sh in a variable assignment, a place this check does not read as a call, and that script fetches" \
  "84	Run a variable naming the script behind timeout	runs .\$s. as a command, whose last path element is an expansion, so this check cannot name the script that runs there" \
  "88	Run the script by a relative path after a cd	names .github/scripts/zz-c\.sh in a variable assignment, a place this check does not read as a call, and that script fetches" \
  "89	Run the script by a relative path after a cd	names .github/scripts/zz-c\.sh in an argument of .bash., a place this check does not read as a call, and that script fetches"; do
  IFS=$'\t' read -r line step says <<<"$refusal"
  check "refuses '$step' at line $line, where a word with no literal .github/ names the script" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' $says")"
done
plain="which could name .github/scripts/r38-plain\.py, a script that fetches or a file this check does not read"
for refusal in \
  "93	Feed the script to bash through a question mark in a redirect	writes ..github/scripts/zz-c\.s.. as a file a redirect of .bash. names, $could" \
  "97	Feed the Python file to bash through a question mark in a redirect	writes ..github/scripts/r38-plain\.p.. as a file a redirect of .bash. names, $plain" \
  "102	Hand the script to flock through a directory the step sets	writes .\$d/zz-c\.s.h.. as an argument of .flock., $could" \
  "107	Copy the Python file through a directory the step sets and run the copy	writes .\$d/r38-plain\.p.. as an argument of .cp., $plain"; do
  IFS=$'\t' read -r line step says <<<"$refusal"
  check "refuses '$step' by name, a redirect's file or a set directory read as a word" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' $says")"
done
for line in "112	a literal tool path" "113	a glob naming no CI script" \
  "114	a glob with no literal .github/" "115	an argument of echo" "116	a literal redirect's file"; do
  IFS=$'\t' read -r at form <<<"$line"
  check "passes $form" "$(is "$(contains "$out" "file=.github/workflows/case.yaml,line=$at::")" false)"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 27)"

# F2. A heredoc line reading `timeout-minutes: 20` is text, no key: it took the block's one key,
# so the job's own key went unpaired and the job passed under the 1200 s its block states. The
# line is refused by name, and the block is paired with the key below it.
echo "case: r37 F2 a text line that reads as a key is refused, and the block prices the job's key"
root=$(fixture r37-text-key)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  quiet:
    runs-on: ubuntu-24.04
    # The measured run, 1200 + 0 = 1200 s -> 20 min.
    steps:
      - name: Write a note
        run: |
          cat > note.txt <<'EOF'
          timeout-minutes: 20
          EOF
    timeout-minutes: 20
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the text line by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::this line reads .timeout-minutes: 20. below the comment at line 6, and it is no .timeout-minutes. key of the YAML")"
check "refuses the job's key under the sum its block states" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::job 'quiet' carries .timeout-minutes: 20., 1200 s, and its own comment derives 1200 + 0 = 1200 s")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# F3. `timeout-minutes: 08` is the string '08' to this check's YAML, so the job read as writing no
# key and was held at 360 minutes, where a YAML 1.2 reader takes 8, inside the step's 1050 s.
echo "case: r37 F3 a key with a leading zero is refused by name"
root=$(fixture r37-leading-zero)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    timeout-minutes: 08
    steps:
      - name: Fetch a large asset
        timeout-minutes: 18
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the job's key by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=6::job 'build' writes .timeout-minutes: 08., which is no whole number with no leading zero")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# F4. Two libraries define r37_go, each calling a function through a variable. The holder's
# refusal named the first definition, and bash runs the one it read last. The name defined twice
# is refused where both are reached, and no refusal names one of them as the one that runs.
echo "case: r37 F4 a holder function two libraries define is refused as defined twice"
root=$(fixture r37-two-holders)
cat > "$root/.github/scripts/r37-v1.sh" <<'SH'
#!/usr/bin/env bash
r37_go() { "$1"; }
. .github/scripts/r37-v2.sh
SH
cat > "$root/.github/scripts/r37-v2.sh" <<'SH'
#!/usr/bin/env bash
r37_go() { "$r37_fn"; }
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Two libraries defining one holder
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          . .github/scripts/r37-v1.sh
          r37_fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          r37_fn=r37_fetch
          r37_go r37_fetch
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the two definitions where they are reached" \
  "$(contains "$out" "::error file=.github/scripts/r37-v1.sh,line=3::.github/scripts/r37-v1.sh reaches two definitions of .r37_go., in this text and .github/scripts/r37-v2.sh, and one of them calls a function through a variable")"
check "names neither definition as the one the call runs" \
  "$(is "$(contains "$out" 'which .github/scripts/r37-v1.sh defines and which calls a function through a variable')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# D1. A loop's own word in quotes, after a backslash, or as a `case` pattern or an extglob
# alternative is no reserved word to bash, and this check ended the loop there, so the fetch
# after it was charged once where the loop runs it three or seven times. Each is refused by name.
echo "case: r37 D1 a loop's own word bash reads as another word is refused by name"
root=$(fixture r37-loop-words)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A loop with a quoted done in its body
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          for pass in 1 2 3; do
            "done" 2>/dev/null || true
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          done
      - name: A loop whose case pattern is done
        timeout-minutes: 10
        run: |
          . .github/scripts/ci-retry.sh
          for attempt in 1 2 3 4 5 6 7; do
            case "$state" in
              done) break ;;
            esac
            state=$(retry_fetch "the state" 60 -- gh api repos/o/r --jq .state)
          done
      - name: A loop whose extglob pattern holds done
        timeout-minutes: 10
        run: |
          . .github/scripts/ci-retry.sh
          for attempt in 1 2 3 4 5 6 7; do
            if [[ $state == @(done|failed) ]]; then break; fi
            state=$(retry_fetch "the state" 60 -- gh api repos/o/r --jq .state)
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "12	A loop with a quoted done in its body	in quotes or after a backslash" \
  "21	A loop whose case pattern is done	as a .case. pattern" \
  "30	A loop whose extglob pattern holds done	as a .case. pattern"; do
  IFS=$'\t' read -r line step how <<<"$refusal"
  check "refuses the word in '$step' by name" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' writes .done. $how")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# D1b. A `done)` written after `case … in` on that line opens no command: it is a word of the
# `case` command, which neither opens nor closes a loop, so it is not refused and the loop around
# it is read as written (dispatch://INFRA-1545, round 37). The loop runs its 1650 s call three
# times, 4950 s, under a key of 10 minutes, 600 s. Until round 37 the header said every `done)` was
# refused.
echo "case: r37 D1b a done) pattern on the case's own line is no loop word, and the loop is charged"
root=$(fixture r37-loop-word-inline)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A loop whose one-line case pattern is done
        timeout-minutes: 10
        run: |
          . .github/scripts/ci-retry.sh
          for attempt in 1 2 3; do
            case "$state" in done) break ;; esac
            retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
          done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "does not refuse the pattern" "$(is "$(contains "$out" 'writes .done.')" false)"
check "charges the loop's three passes, 4950 s" \
  "$(contains "$out" "step 'A loop whose one-line case pattern is done' carries .timeout-minutes: 10., 600 s, and its own retries take 4950 s to exhaust")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# D2. A call through a variable in a command holding the word `]]` is a call, `"$fn" ]]`: only an
# operand of a `[[ … ]]`, the command a `[[` opens and those `||` or `&&` split off up to its
# `]]`, is left out, so a test's operand beside a fetching function stays unrefused.
echo "case: r37 D2 a call through a variable holding ]] is read, and a test's operand is not"
root=$(fixture r37-variable-test-word)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call a fetch through a variable in a command holding ]]
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          r37_fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=r37_fetch
          "$fn" ]]
          "$fn" ]]
      - name: Test a variable in a split test beside a fetch
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          r37_fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          if [[ -n $x || $fn == r37_fetch ]]; then
            r37_fetch
          fi
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call through the variable" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'Call a fetch through a variable in a command holding ]]' of job 'build' calls a function through a variable, .\\\$fn.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# D3. A `$x)` after a `|` is a pattern's last alternative or the last command of a pipeline, and
# this check read every one as the pattern, so `got=$(: | "$fn")` ran a fetching function twice,
# 3300 s, charged nothing. It is refused by name where a function the text can see fetches.
echo "case: r37 D3 a variable after a pipe and before a closing parenthesis is refused by name"
root=$(fixture r37-variable-pipe-end)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call a fetch through a variable at the end of a pipeline
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          r37_fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          fn=r37_fetch
          got=$(: | "$fn")
          got=$(: | "$fn")
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the variable after the pipe by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=13::step 'Call a fetch through a variable at the end of a pipeline' of job 'build' writes .\\\$fn). after a .|.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# D4. `"$@"` and an array's `"${cmd[@]}"` as a command word run whatever they hold: each ran the
# step's fetching function twice, 3300 s, charged once. Each is a call through a variable.
echo "case: r37 D4 a call through \"\$@\" or an array is a call through a variable"
root=$(fixture r37-expansion-calls)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a fetch through its arguments
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          r37_fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          r37_run() { "$@"; }
          r37_run r37_fetch
          r37_run r37_fetch
      - name: Run a fetch through an array
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          r37_fetch() { retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; }
          cmd=(r37_fetch)
          "${cmd[@]}"
          "${cmd[@]}"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call through \"\$@\"" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Run a fetch through its arguments' of job 'build' calls a function through a variable, .\\\$@.")"
check "refuses the call through the array" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=21::step 'Run a fetch through an array' of job 'build' calls a function through a variable, .\\\${cmd\[@\]}.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# D5. A step's own `retry_fetch` runs at every call this check reads as the helper's: here it
# runs its command once, with no retry and no bound, and this check read a retried and bounded
# curl. The definition is refused by name.
echo "case: r37 D5 a definition of the helper's own name is refused by name"
root=$(fixture r37-own-helper)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch through a retry_fetch of the step's own
        timeout-minutes: 28
        run: |
          retry_fetch() { shift 3; "$@"; }
          retry_fetch "the asset" 300 -- curl -fsSL -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the definition by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=10::step 'Fetch through a retry_fetch of the step's own' of job 'build' defines a function named .retry_fetch., the helper's own name")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# D6. Rule 6 reads which `def` a Python read is in within one heredoc, a program of its own,
# and a line at the `def`'s own indentation ends its body. The read in the second heredoc, and
# the one after read_retrying's body at its own indentation, each make a bare read, and were read
# as read_retrying's. Each is refused; the reads inside read_retrying are not.
echo "case: r37 D6 rule 6 reads a def's body within its heredoc and its indentation"
root=$(fixture r37-python-heredocs)
{
  printf '%s\n' '#!/usr/bin/env bash' "python3 - <<'PY'" 'import urllib.request' \
    'def read_retrying(url):' '    return urllib.request.urlopen(url, timeout=30)' 'PY' \
    "python3 - <<-'PY'"
  printf '\t%s\n' 'import urllib.request' \
    'urllib.request.urlopen("https://example.com/a", timeout=30)' 'PY'
  printf '%s\n' "python3 - <<'PY'" 'import urllib.request' 'if True:' \
    '    def read_retrying(url):' '        return urllib.request.urlopen(url, timeout=30)' \
    '    urllib.request.urlopen("https://example.com/b", timeout=30)' 'PY'
} > "$root/.github/scripts/check-ci-images.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for line in 9 16; do
  check "refuses the bare read at line $line" \
    "$(contains "$out" "::error file=.github/scripts/check-ci-images.sh,line=$line::this read goes straight to urllib.request")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# D7. A `retry_fetch` outside its signature is refused by the helper with no attempt made:
# nothing is retried or bounded. The gate at 9813ba64 told this one to pass the `--max-time` its
# curl writes. It is refused by name, as what it is.
echo "case: r37 D7 a retry_fetch outside its signature is refused by name"
root=$(fixture r37-unsigned-helper)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Call the helper outside its signature
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch curl -fsSL --max-time 300 -o a https://example.com/a || true
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Call the helper outside its signature' of job 'build' calls .retry_fetch. in a form other than its signature")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# C2. `-m300` against its option is a bound curl takes, the last one it is given: 8 x 300 + 127 =
# 2527 s, read as the 1 s of the `--max-time 1` before it, 135 s, under a key of 3 minutes.
echo "case: r37 C2 a bound written as -m against its seconds is refused by name"
root=$(fixture r37-attached-bound)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch under an attached bound
        timeout-minutes: 3
        run: curl -fsSL --retry 7 --retry-all-errors --max-time 1 -m300 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the attached bound by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::.curl. in step 'Fetch under an attached bound' writes its per-attempt bound as .-m300.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# C3. A path an `echo` prints inside a `$( … )` that stands as the command is run by the shell:
# the step runs r37-c.sh, 1050 s, under a key of 1 minute, and was charged nothing as a non-call.
# A path printed into an assignment is printed, and stays a non-call.
echo "case: r37 C3 a path echo prints as the command is refused, and one printed into a variable is not"
root=$(fixture r37-printed-run)
cat > "$root/.github/scripts/r37-c.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the script a substitution prints
        timeout-minutes: 1
        run: $(echo .github/scripts/r37-c.sh)
      - name: Print the script's path into a variable
        timeout-minutes: 1
        run: path=$(echo .github/scripts/r37-c.sh)
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the printed run by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::step 'Run the script a substitution prints' of job 'build' names .github/scripts/r37-c.sh in an argument of .echo. inside a .*or backticks whose output the shell runs as a command")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# C4. A heredoc's terminator is its word with the quotes and backslashes removed: a word written
# `\EOF` or `"E"OF` ends the body at a line `EOF`. The gate at 9813ba64 read the first as no
# heredoc, whose apostrophe then swallowed the bare curl after it, and the second as ending at a
# line `E`, which blanked it.
echo "case: r37 C4 a heredoc terminator written escaped or quoted in part ends at its word"
root=$(fixture r37-terminator-words)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch after a heredoc whose terminator is escaped
        run: |
          cat <<\EOF > note.txt
          it's a note
          EOF
          curl -fsSL -o a https://example.com/a
          echo 'ok'
      - name: Fetch after a heredoc whose terminator is quoted in part
        run: |
          cat <<"E"OF > note.txt
          a note
          EOF
          curl -fsSL -o b https://example.com/b
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "12	Fetch after a heredoc whose terminator is escaped" \
  "19	Fetch after a heredoc whose terminator is quoted in part"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses the bare curl after the heredoc of '$step'" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::.curl. in step '$step' fetches without the floor")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# C5. `timeout -vk 5 300 curl …` takes 5 for `-k`'s value and 300 for the duration, and this check
# read 5 as the duration and 300 as the command, so the bare curl behind it was no fetch.
echo "case: r37 C5 a timeout whose short options cluster before a value is refused by name"
root=$(fixture r37-timeout-cluster)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch behind a timeout whose options cluster
        run: timeout -vk 5 300 curl -fsSL -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the cluster by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=8::step 'Fetch behind a timeout whose options cluster' of job 'build' runs .timeout. with .-vk.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# C6. `--max-time=300` is no option curl takes: after `-o` it is the output file, and alone curl
# rejects it before any attempt. This check read each as a bound of 300 s.
echo "case: r37 C6 a bound written --max-time= is refused by name, for what curl makes of it"
root=$(fixture r37-max-time-equals)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch with its bound after an output option
        timeout-minutes: 43
        run: curl -fsSL --retry 7 --retry-all-errors -o --max-time=300 https://example.com/a
      - name: Fetch with its bound written with an equals sign
        timeout-minutes: 43
        run: curl -fsSL --retry 7 --retry-all-errors --max-time=300 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the bound after -o for being -o's value" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::.curl. in step 'Fetch with its bound after an output option' writes .--max-time=300. right after .-o., an option that takes the next word as its value")"
check "refuses the bound with an equals sign as one curl rejects" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::.curl. in step 'Fetch with its bound written with an equals sign' writes .--max-time=300., a spelling curl rejects")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"

# C7. A bound this check cannot price is refused for what it is: `--max-time 1.5`, `--max-time 0
# --max-time 300` and a trailing `--max-time` were each told their bound was a variable, or that
# curl takes a value it stops on. A script holding such a curl, run in a loop this check cannot
# count, is told so too, and not that its bound is a variable.
echo "case: r37 C7 a bound that is no whole number of seconds is refused for what it is"
root=$(fixture r37-decimal-bound)
cat > "$root/.github/scripts/r37-decimal.sh" <<'SH'
#!/usr/bin/env bash
curl -fsSL --retry 7 --retry-all-errors --max-time 1.5 -o a https://example.com/a
SH
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch under a decimal bound
        timeout-minutes: 10
        run: curl -fsSL --retry 7 --retry-all-errors --max-time 1.5 -o a https://example.com/a
      - name: Fetch under a zero bound and then a whole one
        timeout-minutes: 43
        run: curl -fsSL --retry 7 --retry-all-errors --max-time 0 --max-time 300 -o a https://example.com/a
      - name: Fetch with a bound that has no value
        timeout-minutes: 43
        run: curl -fsSL --retry 7 --retry-all-errors --max-time 30 -o a https://example.com/a --max-time
      - name: Run the decimal script in a loop
        timeout-minutes: 43
        run: |
          while read -r row; do
            .github/scripts/r37-decimal.sh
          done < rows.txt
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the decimal bound for what it is" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::.curl. in step 'Fetch under a decimal bound' writes .--max-time 1.5., which curl takes as 1.5 s")"
check "refuses the two bounds" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::.curl. in step 'Fetch under a zero bound and then a whole one' writes its per-attempt bound 2 times, .0. and .300.")"
check "refuses the bound with no value for what curl does with it" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=15::.curl. in step 'Fetch with a bound that has no value' writes .--max-time. with no value after it")"
check "refuses the script's decimal bound" \
  "$(contains "$out" "::error file=.github/scripts/r37-decimal.sh,line=2::.curl. writes .--max-time 1.5.")"
check "refuses the loop around the script, naming the bound's form" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=17::step 'Run the decimal script in a loop' carries .timeout-minutes: 43. and this check can put no figure on it.*attempts whose bound is written in a form this check refuses by name")"
check "names at line 17 the script and its --max-time 1.5, and no variable among the kinds of its bound" \
  "$(names_at "$out" '::error file=.github/workflows/case.yaml,line=17::' .github/scripts/r37-decimal.sh \
    '--max-time 1.5' --not variable)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"

# C8. A `retry_fetch` whose seconds are a variable bounds each attempt at what the variable holds;
# the gate at 9813ba64 told its curl, which writes `--max-time 300`, to pass `--max-time`.
echo "case: r37 C8 a retry_fetch whose seconds are a variable is told so"
root=$(fixture r37-variable-helper-bound)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Retry under a bound the step sets
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the asset" "$R37_BOUND" -- curl -fsSL --max-time 300 -o a https://example.com/a
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "names the helper's seconds as a variable" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::.curl. in step 'Retry under a bound the step sets' is retried by .retry_fetch. with its seconds written as .\\\$R37_BOUND.")"
check "does not ask for a --max-time the curl writes" "$(is "$(contains "$out" 'Pass .--max-time')" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# C9. `timeout` runs an executable and cannot run the helper, a shell function: the call exits 127
# and fetches nothing, and the gate at 9813ba64 charged it 3 x 1050 = 3150 s.
echo "case: r37 C9 a retry_fetch behind timeout is refused by name and charged nothing"
root=$(fixture r37-helper-behind-timeout)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Retry behind a timeout
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          timeout 600 retry_fetch "the index" 300 -- gh api repos/o/r
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the call behind timeout by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'Retry behind a timeout' of job 'build' runs .retry_fetch. behind .timeout.")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# C10. A function's reach starts on its definition's own line, so a fetch written before the name
# on that line is in it: the refusal says "written on its line or after it", where the gate at
# 9813ba64 said "written after it" of a fetch written before it.
echo "case: r37 C10 a definition's refusal says a fetch on its line is in its reach"
root=$(fixture r37-reach-wording)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: A function name with no body after a fetch on its line
        timeout-minutes: 28
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a; quiet()
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "says the fetch is written on the definition's line or after it" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=11::step 'A function name with no body after a fetch on its line' of job 'build' defines a function with a body that is not a .{ … }. group.*is in its reach: written on its line or after it")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# E1. One refusal for a run this check cannot name (dispatch://INFRA-1545, round 37): the operand
# of `bash`, `sh`, `.` or `source`, or a command word, that could be a script under
# .github/scripts/ and is built from data or is a file the step wrote, and output piped into a
# shell or into an `xargs` that runs one. Each refused step runs r37-c.sh, 1050 s, under a key of
# 1 minute, through a path the gate cannot read: round 37's survey inputs 12a to 12d and 12f, the
# assignment the gate already refused (12e), whose run is refused now too, and printf, tee, a
# concatenated variable, sed piped into bash and a source through a variable beside them. The gate
# at 2fa366e0 refused none of those runs: it passed every step but 12e's assignment. A path whose
# last element is literal and no `.sh`, a tool; a variable path under a literal directory outside
# .github/scripts/, which this check does not read; and the arguments a wrapper hands on, which
# the reading of calls through a variable owns, pass. A data file a `.` sources,
# `"$state_dir/$name.state"`, passed too until round 42, which refuses a `.` or a `source` of any
# file but a literal path to one of the tree (dispatch://INFRA-1545, round 42).
echo "case: r37 E1 a run whose script this check cannot name is refused by name"
root=$(fixture r37-unnamed-runs)
cat > "$root/.github/scripts/r37-c.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
chmod +x "$root/.github/scripts/r37-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Cat it into a file and run the file
        timeout-minutes: 1
        run: |
          cat > run.sh <<'EOF'
          .github/scripts/r37-c.sh
          EOF
          bash run.sh
      - name: Echo it into a file and run the file
        timeout-minutes: 1
        run: |
          echo .github/scripts/r37-c.sh > run.sh
          bash run.sh
      - name: Read it into a variable and run the variable
        timeout-minutes: 1
        run: |
          read -r p <<'EOF'
          .github/scripts/r37-c.sh
          EOF
          "$p"
      - name: Echo it into a variable and run the variable
        timeout-minutes: 1
        run: |
          p=$(echo .github/scripts/r37-c.sh)
          "$p"
      - name: Assign it to a variable and run the variable
        timeout-minutes: 1
        run: |
          p=.github/scripts/r37-c.sh
          "$p"
      - name: Echo it into xargs running bash
        timeout-minutes: 1
        run: |
          echo .github/scripts/r37-c.sh | xargs bash
      - name: Print its parts into a file and run the file
        timeout-minutes: 1
        run: |
          printf '%s/%s\n' .github/scripts r37-c.sh > run.sh
          bash run.sh
      - name: Tee it into a file and run the file
        timeout-minutes: 1
        run: |
          echo .github/scripts/r37-c.sh | tee run.sh > /dev/null
          bash run.sh
      - name: Run it through a concatenated variable
        timeout-minutes: 1
        run: |
          dir=.github/scripts
          name=r37-c
          "$dir/$name.sh"
      - name: Pipe a line sed prints into bash
        timeout-minutes: 1
        run: |
          sed -n 1p list.txt | bash
      - name: Source a library named by a variable
        timeout-minutes: 1
        run: |
          . ".github/scripts/$library"
      - name: Run a tool by its literal path
        timeout-minutes: 1
        run: |
          "$HOME/.local/bin/mise" --version
          . "$state_dir/$name.state"
      - name: Run a script outside .github/scripts by a variable
        timeout-minutes: 1
        run: |
          bash "scripts/e2e/$SCRIPT" 2
      - name: Hand a wrapper its command
        timeout-minutes: 1
        run: |
          run() { "$@"; }
          run true
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "13	Cat it into a file and run the file	runs .run.sh. with .bash., a file this text writes before it runs it" \
  "18	Echo it into a file and run the file	runs .run.sh. with .bash., a file this text writes before it runs it" \
  "25	Read it into a variable and run the variable	runs .\$p. as a command, whose last path element is an expansion" \
  "30	Echo it into a variable and run the variable	runs .\$p. as a command, whose last path element is an expansion" \
  "35	Assign it to a variable and run the variable	runs .\$p. as a command, whose last path element is an expansion" \
  "39	Echo it into xargs running bash	runs .xargs., which runs .bash. on each word it reads" \
  "44	Print its parts into a file and run the file	runs .run.sh. with .bash., a file this text writes before it runs it" \
  "49	Tee it into a file and run the file	runs .run.sh. with .bash., a file this text writes before it runs it" \
  "55	Run it through a concatenated variable	runs .\$dir/\$name.sh. as a command, which ends in .\.sh. and holds an expansion" \
  "59	Pipe a line sed prints into bash	pipes the output of .sed. into .bash., which runs it as shell text" \
  "63	Source a library named by a variable	sources .\.github/scripts/\$library., whose last path element is an expansion"; do
  IFS=$'\t' read -r line step says <<<"$refusal"
  check "refuses '$step' by name at its run" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' $says, so this check cannot name the script that runs there")"
done
# The four runs of a file the step wrote get the advice that passes for them, to run no file
# the text writes; the seven others get the literal-path advice (round 37's class search).
check "tells each run whose target is data to write the script's literal path, or a tool's or a data file's" \
  "$(is "$(printf '%s\n' "$out" | grep -c "so this check cannot name the script that runs there.*Where a script of this repository runs there, write its literal path at the run")" 7)"
check "tells each run of a file the step wrote to run no file the text writes" \
  "$(is "$(printf '%s\n' "$out" | grep -c "a file this text writes before it runs it, so this check cannot name the script that runs there.*Run no file this text writes")" 4)"
check "still refuses 12e's path in its assignment" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=34::step 'Assign it to a variable and run the variable' of job 'build' names .github/scripts/r37-c.sh in a variable assignment")"
for step in "Run a script outside .github/scripts by a variable" "Hand a wrapper its command"; do
  check "passes '$step'" "$(is "$(contains "$out" "step '$step'")" false)"
done
check "refuses the data file 'Run a tool by its literal path' sources, which is no file of the tree" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=68::step 'Run a tool by its literal path' of job 'build' sources .\\\$state_dir/\\\$name\.state., which is no literal path to a file of this tree")"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 13)"

# The advice of a refusal of a run this check cannot name covers a tool as well as a script, and a
# file a `.` or a `source` reads (dispatch://INFRA-1545, rounds 37 and 42). The refusal stands
# where a script of this repository could run, and it told every run to write that script's
# literal path, where what runs here is a tool and a data file. It names a tool's literal path or
# its bare name, and the step written either way passes; and since round 42, which refuses a `.`
# or a `source` of any file but a literal path to one of the tree, it tells a source to name a
# library of this repository by its literal path and to read a data file as data, so a data file
# sourced by its literal path or its bare name is refused by name.
echo "case: a run of a tool named by a variable is told to write its literal path or bare name, and a sourced data file to be read as data"
root=$(fixture r37-unnamed-tool)
write_tool_case() {
  cat > "$root/.github/workflows/case.yaml" <<YAML
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run the installed tool
        run: |
          $1 --version
      - name: Read the state the last step wrote
        run: |
          . $2
YAML
}
write_tool_case '"$tool"' '"$state_file"'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the tool's run by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=9::step 'Run the installed tool' of job 'build' runs .\\\$tool. as a command, whose last path element is an expansion")"
check "refuses the data file's source by name" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Read the state the last step wrote' of job 'build' sources .\\\$state_file., whose last path element is an expansion")"
check "tells each run to write a tool's literal path or bare name, and a source to read a data file as data" \
  "$(is "$(printf '%s\n' "$out" | grep -c "so this check cannot name the script that runs there.*where a tool runs there, write its literal path, ...HOME/\.local/bin/mise.., or its bare name, .mise., so this check can see it is not one of this repository.s scripts; and where a .\.. or a .source. reads a file there, source a library of this repository by its literal path, .\. \.github/scripts/<name>\.sh., and read a data file as data")" 2)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 2)"
write_tool_case '"$HOME/.local/bin/mise"' '"$RUNNER_TEMP/build.state"'
run_check "$root"
check "with the literal paths written, exits 1" "$(is "$status" 1)"
check "refuses the data file's literal path by name, as no file of the tree" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Read the state the last step wrote' of job 'build' sources .\\\$RUNNER_TEMP/build\.state., which is no literal path to a file of this tree")"
check "and refuses nothing else, the tool's literal path passing" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"
write_tool_case mise build.state
run_check "$root"
check "with the bare names written, exits 1" "$(is "$status" 1)"
check "refuses the data file's bare name by name, as no file of the tree" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=12::step 'Read the state the last step wrote' of job 'build' sources .build\.state., which is no literal path to a file of this tree")"
check "and refuses nothing else, the tool's bare name passing" \
  "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 1)"

# E2. A function whose body runs one of its positional parameters runs the word its caller hands
# it in that place, so a call handing it a word built from data is a run this check cannot name,
# refused by name at the call (dispatch://INFRA-1545, round 37): `run "$p"` and `run
# "$(cmd)"`, the second word after a `shift`, a library's function, and a function handing a
# runner its own `"$@"`, `wrap() { run "$@"; }`, whose call `wrap "$p"` is the one refused, while
# `run "$@"` inside it is read at wrap's callers. Each runs r37-c.sh, 1050 s, under a key of
# 1 minute, and the gate before this refusal passed each step. A literal command, `run grep …`,
# and a literal path pass. The tree's seven calls of such a function,
# `.github/scripts/check-plugin-names.sh#run` and `.github/scripts/check-private-names.sh#run`,
# each hand it `git` or `grep`.
echo "case: r37 E2 a call handing a function the word it runs, built from data, is refused by name"
root=$(fixture r37-runner-calls)
cat > "$root/.github/scripts/r37-c.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/scripts/r37-quiet.sh" <<'SH'
#!/usr/bin/env bash
echo "nothing to fetch"
SH
cat > "$root/.github/scripts/r37-runlib.sh" <<'SH'
#!/usr/bin/env bash
r37_run() { "$1"; }
SH
chmod +x "$root/.github/scripts/r37-c.sh" "$root/.github/scripts/r37-quiet.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Hand a wrapper a variable
        timeout-minutes: 1
        run: |
          run() { "$@" >> out.txt; }
          p=$(echo .github/scripts/r37-c.sh)
          run "$p"
      - name: Hand a wrapper a substitution
        timeout-minutes: 1
        run: |
          run() { "$1"; }
          run "$(echo .github/scripts/r37-c.sh)"
      - name: Hand a wrapper a variable after the word it shifts off
        timeout-minutes: 1
        run: |
          run_as() { local what=$1; shift; "$@"; }
          p=$(echo .github/scripts/r37-c.sh)
          run_as "the asset" "$p"
      - name: Hand a library's wrapper a variable
        timeout-minutes: 1
        run: |
          . .github/scripts/r37-runlib.sh
          p=$(echo .github/scripts/r37-c.sh)
          r37_run "$p"
      - name: Hand a wrapper of a wrapper a variable
        timeout-minutes: 1
        run: |
          run() { "$@"; }
          wrap() { run "$@"; }
          p=$(echo .github/scripts/r37-c.sh)
          wrap "$p"
          wrap date
      - name: Hand a wrapper a literal command and a literal path
        timeout-minutes: 1
        run: |
          run() { "$@" >> out.txt; }
          run grep -c x notes.txt
          run .github/scripts/r37-quiet.sh
          run_as() { local what=$1; shift; "$@"; }
          run_as "$label" grep -c x notes.txt
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "12	Hand a wrapper a variable	calls .run., which runs the word it is handed, with .\$p., whose last path element is an expansion" \
  "17	Hand a wrapper a substitution	calls .run., which runs the word it is handed, with .\$(echo .github/scripts/r37-c.sh)., whose last path element is an expansion" \
  "23	Hand a wrapper a variable after the word it shifts off	calls .run_as., which runs the word it is handed, with .\$p., whose last path element is an expansion" \
  "29	Hand a library's wrapper a variable	calls .r37_run., which runs the word it is handed, with .\$p., whose last path element is an expansion" \
  "36	Hand a wrapper of a wrapper a variable	calls .wrap., which runs the word it is handed, with .\$p., whose last path element is an expansion"; do
  IFS=$'\t' read -r line step says <<<"$refusal"
  check "refuses '$step' by name at the call" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' $says, so this check cannot name the script that runs there")"
done
check "tells each call to hand the function a literal name" \
  "$(is "$(printf '%s\n' "$out" | grep -c "Hand .[a-z0-9_]*. the script or the command it runs by its literal name")" 5)"
check "passes the literal command and the literal path" \
  "$(is "$(contains "$out" "step 'Hand a wrapper a literal command and a literal path'")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 5)"

# E3. Round 37's probe at aaab38b5 found 14 routes by which text becomes a program that this
# check did not follow, 13a to 13n, and each is refused by its route (dispatch://INFRA-1545,
# round 37): `eval`, of anything (13a, 13b); a shell reading its program from a here-string, a
# file or a process substitution, and a `source` of one (13c, 13d, 13m, 13n); a file the step
# writes, run by a name with no `.sh` (13e), written by dd (13f), or run after a `cd` (13g); a
# function running a local copy of its argument (13h); `xargs` handing a wrapper what it reads
# (13i) and `find -exec {}` (13j); and a script named with no directory, found through the
# working directory or PATH (13k, 13l). Each step runs r37-c.sh or r37-here.sh, one retry_fetch
# at 180 s or 300 s, under a key of 1 minute, and the gate at aaab38b5 passed every one. Commands
# named literally pass: `xargs grep`, `find … -exec rm -rf {} +`, and a tool installed from a
# file named literally and then run.
echo "case: r37 E3 each route by which text becomes a program this check does not follow is refused by name"
root=$(fixture r37-routes)
cat > "$root/.github/scripts/r37-c.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/scripts/r37-here.sh" <<'SH'
#!/usr/bin/env bash
. "$(dirname "${BASH_SOURCE[0]}")/ci-retry.sh"
retry_fetch "the asset" 300 -- curl -fsSL --max-time 300 -o a https://example.com/a
SH
chmod +x "$root/.github/scripts/r37-c.sh" "$root/.github/scripts/r37-here.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: 13a eval a variable read from a heredoc
        timeout-minutes: 1
        run: |
          read -r p <<'EOF'
          .github/scripts/r37-c.sh
          EOF
          eval "$p"
      - name: 13b eval a file's text
        timeout-minutes: 1
        run: |
          echo .github/scripts/r37-c.sh > run.txt
          eval "$(< run.txt)"
      - name: 13c feed bash a here-string
        timeout-minutes: 1
        run: |
          read -r p <<'EOF'
          .github/scripts/r37-c.sh
          EOF
          bash <<< "$p"
      - name: 13d feed bash a file on its standard input
        timeout-minutes: 1
        run: |
          echo .github/scripts/r37-c.sh > run.txt
          bash < run.txt
      - name: 13e run a written file named with no .sh
        timeout-minutes: 1
        run: |
          echo .github/scripts/r37-c.sh > run
          chmod +x run
          ./run
      - name: 13f write the file with dd
        timeout-minutes: 1
        run: |
          echo .github/scripts/r37-c.sh | dd of=run.sh status=none
          bash run.sh
      - name: 13g run a written file after a cd
        timeout-minutes: 1
        run: |
          mkdir -p w
          echo 'cd .. && .github/scripts/r37-c.sh' > w/run.sh
          cd w
          bash run.sh
      - name: 13h hand a function that runs a local copy of its argument
        timeout-minutes: 1
        run: |
          r37_run() {
            local s=$1
            "$s"
          }
          read -r p <<'EOF'
          .github/scripts/r37-c.sh
          EOF
          r37_run "$p"
      - name: 13i pipe the path into xargs env
        timeout-minutes: 1
        run: |
          echo .github/scripts/r37-c.sh | xargs env
      - name: 13j run what find finds
        timeout-minutes: 1
        run: |
          find .github/scripts -name r37-c.sh -exec {} \;
      - name: 13k run a script by its name after a cd
        timeout-minutes: 1
        run: |
          cd .github/scripts && bash r37-here.sh
      - name: 13l run a script by its name through PATH
        timeout-minutes: 1
        run: |
          PATH="$PWD/.github/scripts:$PATH" r37-here.sh
      - name: 13m source a process substitution
        timeout-minutes: 1
        run: |
          source <(echo .github/scripts/r37-c.sh)
      - name: 13n run bash on a process substitution
        timeout-minutes: 1
        run: |
          bash <(echo .github/scripts/r37-c.sh)
      - name: Literal commands through xargs and find, and an installed tool
        timeout-minutes: 1
        run: |
          printf '%s\n' notes.txt list.txt | xargs grep -c x
          find . -name node_modules -prune -exec rm -rf {} +
          install -m 0755 "$RUNNER_TEMP/unpacked/bin/tool" "$RUNNER_TEMP/tool"
          "$RUNNER_TEMP/tool" --version
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in \
  "13	13a eval a variable read from a heredoc	runs .eval. on .\$p., which runs those words as shell text" \
  "18	13b eval a file's text	runs .eval. on .\$(< run.txt)., which runs those words as shell text" \
  "25	13c feed bash a here-string	runs .bash., which reads the program it runs from a here-string" \
  "30	13d feed bash a file on its standard input	runs .bash., which reads the program it runs from a file redirected into it" \
  "36	13e run a written file named with no .sh	runs .\./run. as a command, a file this text writes before it runs it" \
  "41	13f write the file with dd	runs .run\.sh. with .bash., a file this text writes before it runs it" \
  "48	13g run a written file after a cd	runs .run\.sh. with .bash., the name of a file this text writes elsewhere, which a .cd. or a .pushd. this check does not follow may make the file it runs" \
  "59	13h hand a function that runs a local copy of its argument	calls .r37_run., which runs the word it is handed, with .\$p., whose last path element is an expansion" \
  "63	13i pipe the path into xargs env	runs .xargs., which hands each word it reads to .env., which runs it as a command" \
  "67	13j run what find finds	runs .find. with .-exec., which runs each file it finds as a command" \
  "71	13k run a script by its name after a cd	runs .r37-here\.sh. with .bash., which names a script with no directory, so the shell finds it through the working directory or .PATH., neither of which this check follows" \
  "75	13l run a script by its name through PATH	runs .r37-here\.sh. as a command, which names a script with no directory, so the shell finds it through the working directory or .PATH., neither of which this check follows" \
  "79	13m source a process substitution	runs .source., which reads the program it runs from a process substitution" \
  "83	13n run bash on a process substitution	runs .bash., which reads the program it runs from a process substitution"; do
  IFS=$'\t' read -r line step says <<<"$refusal"
  check "refuses '$step' by name at its run" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' $says, so this check cannot name the script that runs there")"
done
check "passes the literal commands and the installed tool" \
  "$(is "$(contains "$out" "step 'Literal commands through xargs and find, and an installed tool'")" false)"
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 14)"
# The advice each of the three reasons gives is one a reader can follow and pass: the
# literal-path advice of the other unnamed runs would name a form still refused here
# (dispatch://INFRA-1545, round 37's class search).
check "tells a written file's run to run no file the text writes" \
  "$(is "$(printf '%s\n' "$out" | grep -c 'a file this text writes before it runs it, so this check cannot name the script that runs there.*Run no file this text writes: write its commands in the step itself')" 2)"
check "tells a run after a cd to run no written file from any directory" \
  "$(contains "$out" 'may make the file it runs, so this check cannot name the script that runs there.*Run no file this text writes, from any directory')"
check "tells a script named with no directory to write its path from the root" \
  "$(is "$(printf '%s\n' "$out" | grep -c 'neither of which this check follows, so this check cannot name the script that runs there.*Write the script.s path from the repository root')" 2)"
check "gives none of the three the literal-path advice" \
  "$(is "$(printf '%s\n' "$out" | grep -E 'writes before it runs it|writes elsewhere|names a script with no directory' | grep -c 'write its literal path at the run')" 0)"

# E4. A call through a variable inside a function's body whose value the function's callers do
# not hand it is a run like one at the text's top level (dispatch://INFRA-1545, round 37): a
# variable the body sets from a substitution or reads with `read`, and one the step sets before
# it defines the function, each run of r37-c.sh, 1050 s, under a key of 1 minute, and each left
# by the gate before this round to the readings of runners and holders, which follow callers. A
# body running a copy of its argument, handed a literal name, and a library function running a
# variable the library sets nowhere, whose value the sourcing step hands it, pass.
echo "case: r37 E4 a call through a variable a function's body or its text sets is refused by name"
root=$(fixture r37-body-variables)
cat > "$root/.github/scripts/r37-c.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
cat > "$root/.github/scripts/r37-holder.sh" <<'SH'
#!/usr/bin/env bash
r37_go() { "$r37_fn"; }
SH
chmod +x "$root/.github/scripts/r37-c.sh"
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Run a variable a function sets from a substitution
        timeout-minutes: 1
        run: |
          run_it() {
            local s
            s=$(echo .github/scripts/r37-c.sh)
            "$s"
          }
          run_it
      - name: Run a variable a function reads
        timeout-minutes: 1
        run: |
          run_it() {
            read -r s < list.txt
            "$s"
          }
          run_it
      - name: Run a variable the step sets before the function
        timeout-minutes: 1
        run: |
          s=$(echo .github/scripts/r37-c.sh)
          run_it() { "$s"; }
          run_it
      - name: Hand a function a literal name it copies and runs
        timeout-minutes: 1
        run: |
          run_it() { local s=$1; "$s"; }
          run_it date
      - name: Hand a library function a value it runs
        timeout-minutes: 1
        run: |
          . .github/scripts/r37-holder.sh
          r37_fn=date
          r37_go
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
for refusal in "13	Run a variable a function sets from a substitution" \
  "21	Run a variable a function reads" "28	Run a variable the step sets before the function"; do
  IFS=$'\t' read -r line step <<<"$refusal"
  check "refuses '$step' by name at its run" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' runs .\$s. as a command, whose last path element is an expansion, so this check cannot name the script that runs there")"
done
for step in "Hand a function a literal name it copies and runs" \
  "Hand a library function a value it runs"; do
  check "passes '$step'" "$(is "$(contains "$out" "step '$step'")" false)"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 3)"

# E5. A text that runs, outside any function, the words it is handed or sets itself, `"$@"`,
# `"$1"` or an array's `"${cmd[@]}"`, runs a program its callers or its own data name, which this
# check follows to no script (dispatch://INFRA-1545, round 37's last probe). Before the refusal,
# a path built from data, `.github/scripts/$n.sh`, handed to a script whose top level runs `"$@"`
# or `"$1"`, set as a step's parameters or put in an array the step then runs, each passed r37-c.sh,
# 1050 s, under a key of one minute. A function this text defines and calls with literal words is
# read at its callers, and passes. Since the word reading of W18 the built path itself,
# `.github/scripts/$n.sh`, is refused where each step writes it, a word that could name a CI script
# that fetches, whatever runs it, so the case's count went from 4 to 8 (dispatch://INFRA-1545,
# round 38).
echo "case: r37 E5 a run of the words a text is handed or sets itself, outside any function, is refused by name"
root=$(fixture r37-handed-on)
cat > "$root/.github/scripts/r37-c.sh" <<'SH'
#!/usr/bin/env bash
. .github/scripts/ci-retry.sh
retry_fetch "a large asset" 180 -- curl -fsSL --max-time 180 -o b https://example.com/b
SH
printf '#!/usr/bin/env bash\n"$@"\n' > "$root/.github/scripts/r37-wrap.sh"
printf '#!/usr/bin/env bash\n"$1"\n' > "$root/.github/scripts/r37-first.sh"
cat > "$root/.github/scripts/r37-run.sh" <<'SH'
#!/usr/bin/env bash
run() { "$@" > out.txt; }
run grep -c x in.txt
SH
chmod +x "$root"/.github/scripts/r37-*.sh
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Hand a built name to a script that runs its arguments
        timeout-minutes: 1
        run: |
          n=r37-c
          bash .github/scripts/r37-wrap.sh ".github/scripts/$n.sh"
      - name: Hand a built name to a script that runs its first argument
        timeout-minutes: 1
        run: |
          n=r37-c
          bash .github/scripts/r37-first.sh ".github/scripts/$n.sh"
      - name: Set a built name as the parameters and run them
        timeout-minutes: 1
        run: |
          n=r37-c
          set -- ".github/scripts/$n.sh"
          "$@"
      - name: Put a built name in an array and run it
        timeout-minutes: 1
        run: |
          n=r37-c
          cmd=(".github/scripts/$n.sh")
          "${cmd[@]}"
      - name: Run a function wrapper with literal words
        timeout-minutes: 1
        run: |
          bash .github/scripts/r37-run.sh
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "refuses the script that runs its arguments, at its run" \
  "$(contains "$out" '::error file=.github/scripts/r37-wrap.sh,line=2::.github/scripts/r37-wrap.sh runs `$@` as a command, which runs words its callers hand it or that it sets itself')"
check "refuses the script that runs its first argument, at its run" \
  "$(contains "$out" '::error file=.github/scripts/r37-first.sh,line=2::.github/scripts/r37-first.sh runs `$1` as a command, which runs words its callers hand it or that it sets itself')"
check "refuses the step that runs its own parameters, at the run" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=22::step 'Set a built name as the parameters and run them' of job 'build' runs \`\$@\` as a command")"
check "refuses the step that runs its array, at the run" \
  "$(contains "$out" "::error file=.github/workflows/case.yaml,line=28::step 'Put a built name in an array and run it' of job 'build' runs \`\${cmd\[@\]}\` as a command")"
check "tells each to name the command it runs literally" \
  "$(is "$(printf '%s\n' "$out" | grep -c 'follows neither to the program they name, so this check cannot name the script that runs there.*Name the command this text runs literally')" 4)"
check "passes the function wrapper called with literal words" \
  "$(is "$(contains "$out" "r37-run.sh")" false)"
for refusal in "11	Hand a built name to a script that runs its arguments	as an argument of .bash." \
  "16	Hand a built name to a script that runs its first argument	as an argument of .bash." \
  "21	Set a built name as the parameters and run them	as an argument of .set." \
  "27	Put a built name in an array and run it	as a word this check reads as a pattern rather than a run"; do
  IFS=$'\t' read -r line step place <<<"$refusal"
  check "refuses the built path '$step' writes, where it writes it" \
    "$(contains "$out" "::error file=.github/workflows/case.yaml,line=$line::step '$step' of job 'build' writes ..github/scripts/\$n\.sh. $place, which could name .github/scripts/r37-c\.sh, a script that fetches or a file this check does not read")"
done
check "refuses nothing else" "$(is "$(printf '%s\n' "$out" | grep -c '^::error')" 8)"

# The promise rule 8 exists to keep: no retried job whose key is below its own sum reaches main.
# Every case above proves one shape on a fixture written for it, which leaves open whether a shape
# this repository's own tree holds hides a key, so this case drives the promise on a copy of the
# tree itself. For each job --jobs lists, it lowers the key to the whole minutes of the job's own
# sum, which is at or under that sum, and asks the gate to refuse that job's key by name. The sum
# is the gate's: the largest of the slowest retried step's cost and, where the block that prices
# the key holds a derivation whose arrow names the key and adds up, the last such derivation's
# total and its measured term plus that cost. That block is the one --jobs names in its sixth and
# seventh columns: the case reads the gate's pairing and walks to no block of its own, since review
# round 29 of dispatch://INFRA-1545 found it re-implementing the gate's pairing walk. Its program,
# .github/scripts/download_retry_real_tree.py, reads the derivation inside that block, which is
# the case's oracle for the sum. A mutant of the gate is then judged by whether it can hide a real
# key, not by whether some fixture happened to hold the shape. The shapes are seven ways a lowered
# key can be written:
#   key           the key alone, its block left naming the old figure;
#   arrows        the key, and every arrow of its block naming the old key moved to the new one;
#   decoy-after   arrows, and a derivation whose arrow names another key appended to the block;
#   decoy-before  arrows, and a 2 s derivation naming the new key as the block's first line;
#   split         arrows, and the derivation the gate reads broken over two lines after its `=`;
#   neighbour     the job's own block deleted, and a step-depth comment naming the new key written
#                 directly above its id, which prices no job (every job but its file's first);
#   more-figures  the job's own block deleted, and the previous job's last comment block given
#                 arrows naming the new key, more than the keys left below it before this job's
#                 id, which ends its reach (only where nothing but YAML lies between that block
#                 and this job's key, since a comment line there ends the reach anyway).
# A refusal of one of the job's STEP keys does not count: the promise is about the job's key. Each
# shape prints how many jobs it lowered. `key`, `arrows` and both decoys lower every job --jobs
# lists and must lower exactly that many, and the other three must lower one at least, so a shape
# that silently lowers fewer is seen; and the list the case walks is first held to the tree, so it
# is not narrower than it: every workflow job that writes its own whole-number `timeout-minutes`
# and holds a site --list prints must be in --jobs. A key written as an expression is outside, as
# the gate's header says under A CEILING WRITTEN AS AN EXPRESSION
# (`.github/workflows/e2e.yaml#proof` and `.github/workflows/dispatch-unit-repeat.yaml#repeat`).
# THIS CASE FOUND A GAP THE FIXTURES DID NOT: at the head before round 35, `arrows`, both decoys
# and `split` left `download-retry`, `download-retry-images`, `re-record`, `embedder` and
# `visual` unnamed, because the derivation clause read only the comment block directly above the
# key and those five write their derivations above the job's id (dispatch://INFRA-1545, round 35).
# It is eight gate calls over the tree and one more for --list, each of which takes thirty to forty
# times a fixture's call, so each runs under GATE_TREE_BOUND rather than GATE_BOUND (see both).
GATE_BOUND=$GATE_TREE_BOUND
echo "case: every retried job of this repository's own tree, its key lowered below its own sum, is refused by name"
repository=$(cd "$script_dir/../.." && pwd)
tree="$work/real-tree"
mkdir -p "$tree/packages/daemon/docker" "$tree/packages/envoy/docker" \
  "$tree/packages/envoy/infra/host/heldportwrapper"
# The copy of .github brings the fourth Dockerfile DOCKERFILES names,
# .github/docker/browser.Dockerfile, with it; the other three live under packages/.
cp -R "$repository/.github" "$tree/.github"
for dockerfile in packages/daemon/docker/worker.Dockerfile packages/envoy/docker/Dockerfile \
  packages/envoy/infra/host/heldportwrapper/Dockerfile; do
  cp "$repository/$dockerfile" "$tree/$dockerfile"
done
real_tree="$script_dir/download_retry_real_tree.py"
set +e
run_gate null "$tree" --jobs
set -e
jobs_status=$status
check "--jobs over the tree exits 0" "$(is "$jobs_status" 0)"
if [ -n "$out" ]; then printf '%s\n' "$out"; fi > "$work/real-tree.jobs"
listed=$(python3 "$real_tree" rows "$work/real-tree.jobs")
check "--jobs lists at least one job (it lists $listed)" "$([ "$listed" -ge 1 ] && echo true || echo false)"
list_check "$tree"
check "--list over the tree exits 0" "$(is "$status" 0)"
if [ -n "$out" ]; then printf '%s\n' "$out"; fi > "$work/real-tree.sites"
omitted=$(python3 "$real_tree" cover "$tree" "$work/real-tree.jobs" "$work/real-tree.sites")
check "--jobs omits no workflow job that writes its own whole-number key and holds a site --list prints${omitted:+; it omits $omitted}" \
  "$(is "$omitted" "")"
# The shapes lower the jobs --jobs lists, so where it refused the tree or listed none there is
# nothing for them to lower, and the case says so in a check of its own rather than seven shapes
# that each lower nothing.
if [ "$jobs_status" -ne 0 ] || [ "$listed" -lt 1 ]; then
  check "the seven shapes are skipped: --jobs over the tree exited $jobs_status and listed $listed job(s), so there is no job for them to lower" false
else
  for shape in key arrows decoy-after decoy-before split neighbour more-figures; do
    copy="$work/real-tree-$shape"
    cp -R "$tree" "$copy"
    python3 "$real_tree" shape "$tree" "$work/real-tree.jobs" "$shape" "$copy" > "$copy.done"
    lowered=$(grep -c '^lowered' "$copy.done" || true)
    if [ "$shape" = split ]; then
      echo "  $shape: lowered $lowered job(s), split $(grep -c '^split' "$copy.done" || true) derivation(s)"
    else
      echo "  $shape: lowered $lowered job(s)"
    fi
    # These four shapes lower every job --jobs lists, so each is held to that number and cannot
    # shrink in silence; the other three lower only the jobs their shape fits, and are held to
    # one at least.
    case "$shape" in
      key | arrows | decoy-after | decoy-before)
        check "$shape: lowers every job --jobs lists ($lowered of $listed)" "$(is "$lowered" "$listed")"
        ;;
      *)
        check "$shape: lowers at least one job" "$([ "$lowered" -ge 1 ] && echo true || echo false)"
        ;;
    esac
    run_check "$copy"
    check "$shape: exits 1" "$(is "$status" 1)"
    printf '%s\n' "$out" > "$copy.out"
    unnamed=$(python3 "$real_tree" unnamed "$copy.done" "$copy.out")
    check "$shape: a refusal of its own key names every job it lowered${unnamed:+; none names $unnamed}" \
      "$(is "$unnamed" "")"
  done
fi

summary "check-download-retry.sh"
