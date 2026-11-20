#!/usr/bin/env bash
# Tests for `go_offline_jobs.py`, over trees it writes: every Go job of a required workflow runs
# with GOPROXY=off and GOFLAGS=-mod=readonly. A Go job of ci.yaml, of a workflow ci.yaml calls and
# of one that workflow calls is refused once when its env leaves both settings out, either one
# out, or holds an online proxy, and passes with both in the workflow's env, in the job's, or one
# in each; a job's env wins over its workflow's. A step that sends either setting online in its
# env:, in a line of its run: (an assignment, an export, unset, env -u) or through $GITHUB_ENV is
# refused with no `# go-offline: <why>` comment line in its block or in the 5 lines above it, and
# passes, named in the summary, with one; a reason six lines up, an empty one and one in the step
# before are refused, and one right after the step before's block run: is this step's. A workflow
# outside the required set and a job with no Go are not read. A job is a Go job by setup-go, by a
# local action that runs setup-go (itself, through another local action, or under the path of a
# second checkout of this repository), and by a run: line that starts go (bare, with -C, as a path
# ending /go, after a cd) or go-fetch.sh, and not by go version or a comment. A called workflow or
# action that is not in the tree (an action of another repository's checkout among them), and a
# file that is no YAML, are refused. A tree with no ci.yaml, and one whose required jobs run no
# Go, give UNKNOWN and exit 2.
#
# Each case writes a tree of its own under this harness's temporary directory and runs the rule's
# CLI on it; the rule reads only text, so nothing a fixture names runs. The line a refusal must
# name is read from the planted file with grep, not from the rule's YAML reading. It needs a
# python3 with PyYAML, as the rule does.
#
# Run from anywhere: .github/scripts/go-offline-jobs.test.sh
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
rule="$script_dir/go_offline_jobs.py"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export PYTHONDONTWRITEBYTECODE=1

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The case count: a case passes when no check of it failed.
CASES=0
CASES_PASSED=0
case_fails=0
case_open=""
close_case() {
  if [ -n "$case_open" ]; then
    if [ "$FAIL" -eq "$case_fails" ]; then
      CASES_PASSED=$((CASES_PASSED + 1))
    fi
    case_open=""
  fi
}
case_() { # case_ DESCRIPTION
  close_case
  CASES=$((CASES + 1))
  case_fails=$FAIL
  case_open=1
  echo "case $CASES: $1"
}

# tree <name>: an empty tree for one case. Echoes its root.
tree() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the tree name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/workflows"
  echo "$root"
}

# put <root> <path>: writes this harness's stdin to <path> under <root>, making its directory.
put() {
  mkdir -p "$(dirname "$1/$2")"
  cat >"$1/$2"
}

# The steps of a job that sets up Go and runs its tests.
GO_STEPS='      - uses: actions/checkout@08eba0b27e820071cde6df949e0beb9ba4906955 # v4.3.0
      - name: Set up Go
        uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: go.mod
          cache: false
      - name: Test
        run: go test ./...'
# Both settings, as a workflow's env: holds them and as a job's does.
WORKFLOW_OFFLINE='  GOPROXY: "off"
  GOFLAGS: -mod=readonly'
JOB_OFFLINE='      GOPROXY: "off"
      GOFLAGS: -mod=readonly'

# go_workflow <root> <path> <on> <workflow env> <job env>: a workflow at <path>, run on <on>, with
# one job, vet, whose steps are GO_STEPS and then the steps on this harness's stdin. Each env is
# the lines of an env: block at its indentation, or empty for no env: block.
go_workflow() {
  local root=$1 path=$2 on=$3 workflow_env=$4 job_env=$5
  {
    printf 'name: %s\non: %s\n' "$(basename "$path")" "$on"
    [ -z "$workflow_env" ] || printf 'env:\n%s\n' "$workflow_env"
    printf 'jobs:\n  vet:\n    runs-on: ubuntu-24.04\n'
    [ -z "$job_env" ] || printf '    env:\n%s\n' "$job_env"
    printf '    steps:\n%s\n' "$GO_STEPS"
    cat
  } | put "$root" "$path"
}

# calls <root> <path> <on> <called>: a workflow at <path> whose one job, go, calls <called>.
calls() {
  printf 'name: %s\non: %s\njobs:\n  go:\n    uses: ./%s\n' "$(basename "$2")" "$3" "$4" | put "$1" "$2"
}

run_rule() { # run_rule <root>
  set +e
  out=$(python3 "$rule" "$1" 2>&1)
  status=$?
  set -e
}

# line_of <file> <regex>: the line number of the first line of <file> that matches <regex>.
line_of() { grep -n -m1 -- "$2" "$1" | cut -d: -f1; }

# count_of <text> <regex>: how many lines of <text> match <regex>.
count_of() { grep -c -- "$2" <<<"$1" || true; }

# refused_job <root> <path> <job> <first Go step> <lacks>: exit 1, and job <job> of <path> is
# refused at the line of its key, naming its first Go step and what its env lacks (<lacks>, a
# regex that starts right after "lacks ").
refused_job() {
  local line
  line=$(line_of "$1/$2" "^  $3:$")
  check "exits 1" "$(is "$status" 1)"
  check "refuses job $3 of $2 at its line, $line, naming its first Go step" \
    "$(contains "$out" "^::error file=$2,line=$line::job $3 of $2 runs go (first at step \"$4\") and its env lacks $5")"
  check "refuses that job once" "$(is "$(count_of "$out" "::job $3 of $2 runs go")" 1)"
}

# refused_step <root> <path> <job> <step> <how>: exit 1, and step <step> of job <job> of <path> is
# refused at the line of its `- name:`, naming how it sends a setting online (<how>, a regex).
refused_step() {
  local line
  line=$(line_of "$1/$2" "^ *- name: $4$")
  check "exits 1" "$(is "$status" 1)"
  check "refuses step \"$4\" at its line, $line: $5" \
    "$(contains "$out" "^::error file=$2,line=$line::job $3 of $2, step \"$4\", .*$5")"
}

# passes <Go jobs> <required workflows> <online steps>: exit 0, and the summary counts what was read
# (<online steps> is the summary's text from the count of online steps on, a regex).
passes() {
  check "exits 0" "$(is "$status" 0)"
  check "reads $1 Go jobs of $2 required workflows, all offline" \
    "$(contains "$out" "^$1 Go jobs of $2 required workflows read, each with GOPROXY=off and GOFLAGS=-mod=readonly; $3$")"
}

# --- The job's settings, in each required workflow ---------------------------------------------

case_ "a job of ci.yaml that sets up Go with neither setting is refused once, naming both"
root=$(tree ci-neither)
go_workflow "$root" .github/workflows/ci.yaml pull_request '' '' </dev/null
run_rule "$root"
refused_job "$root" .github/workflows/ci.yaml vet "Set up Go" \
  'GOPROXY=off (it is unset) and GOFLAGS=-mod=readonly (it is unset): add GOPROXY: "off" and GOFLAGS: -mod=readonly to the env: of .github/workflows/ci.yaml or of job vet'
check "refuses it once, though two of its steps run Go" "$(is "$(count_of "$out" '^::error ')" 1)"
check "counts the problem" "$(contains "$out" '^1 problems over 1 Go jobs of 1 required workflows read$')"
check "check() gives (path, line, message), the path relative to the root" \
  "$(is "$(python3 -c 'import sys; sys.path.insert(0, sys.argv[1]); import go_offline_jobs as rule; print(*rule.check(sys.argv[2])[0][0][:2])' \
    "$script_dir" "$root")" ".github/workflows/ci.yaml 4")"

case_ "the same job in a workflow ci.yaml calls is refused"
root=$(tree called-neither)
calls "$root" .github/workflows/ci.yaml pull_request .github/workflows/ci-job-go.yaml
go_workflow "$root" .github/workflows/ci-job-go.yaml workflow_call '' '' </dev/null
run_rule "$root"
refused_job "$root" .github/workflows/ci-job-go.yaml vet "Set up Go" 'GOPROXY=off (it is unset) and GOFLAGS=-mod=readonly'
check "counts both workflows" "$(contains "$out" '^1 problems over 1 Go jobs of 2 required workflows read$')"

case_ "the same job in a workflow called by a workflow ci.yaml calls is refused"
root=$(tree called-twice-neither)
calls "$root" .github/workflows/ci.yaml pull_request .github/workflows/ci-job-docs.yaml
calls "$root" .github/workflows/ci-job-docs.yaml workflow_call .github/workflows/docs-site.yaml
go_workflow "$root" .github/workflows/docs-site.yaml workflow_call '' '' </dev/null
run_rule "$root"
refused_job "$root" .github/workflows/docs-site.yaml vet "Set up Go" 'GOPROXY=off (it is unset) and GOFLAGS=-mod=readonly'
check "counts the three workflows" "$(contains "$out" '^1 problems over 1 Go jobs of 3 required workflows read$')"

case_ "a job with GOPROXY=off alone is refused, naming GOFLAGS alone"
root=$(tree goproxy-only)
go_workflow "$root" .github/workflows/ci.yaml pull_request '  GOPROXY: "off"' '' </dev/null
run_rule "$root"
refused_job "$root" .github/workflows/ci.yaml vet "Set up Go" 'GOFLAGS=-mod=readonly (it is unset): add GOFLAGS: -mod=readonly to'
check "does not name GOPROXY" "$(is "$(count_of "$out" 'GOPROXY=off (it is')" 0)"

case_ "a job with GOFLAGS=-mod=readonly alone is refused, naming GOPROXY alone"
root=$(tree goflags-only)
go_workflow "$root" .github/workflows/ci.yaml pull_request '  GOFLAGS: -mod=readonly' '' </dev/null
run_rule "$root"
refused_job "$root" .github/workflows/ci.yaml vet "Set up Go" 'GOPROXY=off (it is unset): add GOPROXY: "off" to'
check "does not name GOFLAGS" "$(is "$(count_of "$out" 'GOFLAGS=-mod=readonly (it is')" 0)"

case_ "an online GOPROXY and a GOFLAGS without -mod=readonly, as main's workflows set them, are refused with their values"
root=$(tree online-values)
go_workflow "$root" .github/workflows/ci.yaml pull_request '  GOPROXY: https://proxy.golang.org|direct
  GOFLAGS: -mod=mod -p=4' '' </dev/null
run_rule "$root"
refused_job "$root" .github/workflows/ci.yaml vet "Set up Go" \
  'GOPROXY=off (it is "https://proxy.golang.org|direct") and GOFLAGS=-mod=readonly (it is "-mod=mod -p=4")'

case_ "both settings in the workflow's env pass, an unquoted off and -mod=readonly among other flags included"
root=$(tree workflow-both)
go_workflow "$root" .github/workflows/ci.yaml pull_request '  GOPROXY: off
  GOFLAGS: -p=4 -mod=readonly' '' </dev/null
run_rule "$root"
passes 1 1 '0 online steps with a reason: (none)'

case_ "both settings in the job's env pass, and so does one in each"
root=$(tree job-both)
go_workflow "$root" .github/workflows/ci.yaml pull_request '' "$JOB_OFFLINE" </dev/null
run_rule "$root"
passes 1 1 '0 online steps with a reason: (none)'
root=$(tree one-in-each)
go_workflow "$root" .github/workflows/ci.yaml pull_request '  GOPROXY: "off"' '      GOFLAGS: -mod=readonly' </dev/null
run_rule "$root"
passes 1 1 '0 online steps with a reason: (none)'

case_ "a job's env wins over its workflow's: an online GOPROXY there is refused"
root=$(tree job-wins)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '      GOPROXY: direct' </dev/null
run_rule "$root"
refused_job "$root" .github/workflows/ci.yaml vet "Set up Go" 'GOPROXY=off (it is "direct"): add GOPROXY: "off" to'

# --- A step that goes online, and its reason ----------------------------------------------------

case_ "a step whose env sets GOPROXY to the fetch proxy, with no reason, is refused"
root=$(tree step-proxy)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      - name: Fetch the plugin
        env:
          GOPROXY: ${{ env.GO_FETCH_PROXY }}
        run: go run ./cmd/fetch-plugin
EOF
run_rule "$root"
refused_step "$root" .github/workflows/ci.yaml vet "Fetch the plugin" \
  'sets GOPROXY to "${{ env.GO_FETCH_PROXY }}" in its env: with no go-offline reason'
check "refuses that step alone" "$(is "$(count_of "$out" '^::error ')" 1)"

case_ "the same step with a go-offline reason passes, named as an online step; so does one inside the step or 5 lines up"
root=$(tree step-reason-above)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      # go-offline: the plugin's own installer downloads it, and no module cache holds it.
      - name: Fetch the plugin
        env:
          GOPROXY: ${{ env.GO_FETCH_PROXY }}
        run: go run ./cmd/fetch-plugin
EOF
run_rule "$root"
passes 1 1 '1 online steps with a reason: .github/workflows/ci.yaml vet "Fetch the plugin"'
root=$(tree step-reason-inside)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      - name: Fetch the plugin
        # go-offline: the plugin's own installer downloads it, and no module cache holds it.
        env:
          GOPROXY: ${{ env.GO_FETCH_PROXY }}
        run: go run ./cmd/fetch-plugin
EOF
run_rule "$root"
passes 1 1 '1 online steps with a reason: .github/workflows/ci.yaml vet "Fetch the plugin"'
root=$(tree step-reason-five-up)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      # go-offline: the plugin's own installer downloads it, and no module cache holds it.
      #
      # The installer reads its version from the plugin's manifest.
      #
      # It retries on its own.
      - name: Fetch the plugin
        env:
          GOPROXY: ${{ env.GO_FETCH_PROXY }}
        run: go run ./cmd/fetch-plugin
EOF
run_rule "$root"
passes 1 1 '1 online steps with a reason: .github/workflows/ci.yaml vet "Fetch the plugin"'

case_ "a reason six lines above the step is too far"
root=$(tree step-reason-six-up)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      # go-offline: the plugin's own installer downloads it, and no module cache holds it.
      #
      # The installer reads its version from the plugin's manifest.
      #
      # It retries on its own.
      #
      - name: Fetch the plugin
        env:
          GOPROXY: ${{ env.GO_FETCH_PROXY }}
        run: go run ./cmd/fetch-plugin
EOF
run_rule "$root"
refused_step "$root" .github/workflows/ci.yaml vet "Fetch the plugin" 'sets GOPROXY to ".*" in its env: with no go-offline reason'

case_ "a reason inside the step before is that step's, not this one's"
root=$(tree step-reason-before)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      - name: List the plugin's packages
        run: |
          # go-offline: this is the listing step's comment.
          go list ./cmd/fetch-plugin
      - name: Fetch the plugin
        env:
          GOPROXY: ${{ env.GO_FETCH_PROXY }}
        run: go run ./cmd/fetch-plugin
EOF
run_rule "$root"
refused_step "$root" .github/workflows/ci.yaml vet "Fetch the plugin" 'sets GOPROXY to ".*" in its env: with no go-offline reason'

case_ "a reason between a step whose run: is a block and the next step is the next step's"
root=$(tree step-reason-after-block)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      - name: List the plugin's packages
        run: |
          go list ./cmd/fetch-plugin
      # go-offline: the plugin's own installer downloads it, and no module cache holds it.
      - name: Fetch the plugin
        env:
          GOPROXY: ${{ env.GO_FETCH_PROXY }}
        run: go run ./cmd/fetch-plugin
EOF
run_rule "$root"
passes 1 1 '1 online steps with a reason: .github/workflows/ci.yaml vet "Fetch the plugin"'

case_ "a go-offline comment with nothing after the colon is no reason"
root=$(tree step-reason-empty)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      # go-offline:
      - name: Fetch the plugin
        env:
          GOPROXY: ${{ env.GO_FETCH_PROXY }}
        run: go run ./cmd/fetch-plugin
EOF
run_rule "$root"
refused_step "$root" .github/workflows/ci.yaml vet "Fetch the plugin" 'sets GOPROXY to ".*" in its env: with no go-offline reason'

case_ "a step whose env sets GOFLAGS without -mod=readonly, or empties GOPROXY, with no reason, is refused"
root=$(tree step-goflags)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      - name: Tidy the module
        env:
          GOFLAGS: -mod=mod
        run: go mod tidy
      - name: Clear the proxy
        env:
          GOPROXY: ""
        run: go list -m all
EOF
run_rule "$root"
refused_step "$root" .github/workflows/ci.yaml vet "Tidy the module" 'sets GOFLAGS to "-mod=mod" in its env: with no go-offline reason'
refused_step "$root" .github/workflows/ci.yaml vet "Clear the proxy" 'removes GOPROXY in its env: with no go-offline reason'

case_ "a step whose env keeps both settings offline is no online step"
root=$(tree step-offline-env)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      - name: Race
        env:
          GOPROXY: "off"
          GOFLAGS: -mod=readonly -race
        run: go test ./...
EOF
run_rule "$root"
passes 1 1 '0 online steps with a reason: (none)'

case_ "a run line GOPROXY=direct go test, with no reason, is refused"
root=$(tree run-assign)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      - name: Test online
        run: GOPROXY=direct go test ./...
EOF
run_rule "$root"
refused_step "$root" .github/workflows/ci.yaml vet "Test online" 'sets GOPROXY in its run: "GOPROXY=direct go test ./..." with no go-offline reason'

case_ "an export, an unset and an env -u in a run line are refused; GOPROXY=off and a GOFLAGS holding -mod=readonly are not"
root=$(tree run-forms)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      - name: Export the flags
        run: |
          export GOFLAGS=-mod=mod
          go mod tidy
      - name: Unset the proxy
        run: |
          unset GOPROXY
          go list -m all
      - name: Run without the flags
        run: env -u GOFLAGS go test ./...
EOF
run_rule "$root"
refused_step "$root" .github/workflows/ci.yaml vet "Export the flags" 'sets GOFLAGS in its run: "export GOFLAGS=-mod=mod"'
refused_step "$root" .github/workflows/ci.yaml vet "Unset the proxy" 'removes GOPROXY in its run: "unset GOPROXY"'
refused_step "$root" .github/workflows/ci.yaml vet "Run without the flags" 'removes GOFLAGS in its run: "env -u GOFLAGS go test ./..."'
check "refuses the three steps alone" "$(is "$(count_of "$out" '^::error ')" 3)"
root=$(tree run-offline)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      - name: Test offline
        run: |
          GOPROXY=off go test ./...
          GOFLAGS="-mod=readonly -race" go test ./...
EOF
run_rule "$root"
passes 1 1 '0 online steps with a reason: (none)'

case_ "a run line writing GOPROXY=direct to \$GITHUB_ENV, with no reason, is refused"
root=$(tree run-github-env)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      - name: Go online for the rest of the job
        run: echo "GOPROXY=direct" >> "$GITHUB_ENV"
EOF
run_rule "$root"
refused_step "$root" .github/workflows/ci.yaml vet "Go online for the rest of the job" \
  'writes GOPROXY to $GITHUB_ENV in its run: "echo "GOPROXY=direct" >> "$GITHUB_ENV"" with no go-offline reason'

# --- What is not read ---------------------------------------------------------------------------

case_ "a Go job of a workflow no required workflow calls is not read"
root=$(tree not-required)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' </dev/null
go_workflow "$root" .github/workflows/nightly.yaml schedule '' '' </dev/null
run_rule "$root"
passes 1 1 '0 online steps with a reason: (none)'

case_ "a job of a required workflow with no Go is not read"
root=$(tree no-go-job)
go_workflow "$root" .github/workflows/ci.yaml pull_request '' "$JOB_OFFLINE" <<'EOF'
  ts:
    runs-on: ubuntu-24.04
    steps:
      - uses: ./.github/actions/setup-bun
      - run: bun install --frozen-lockfile
      - name: Test
        run: bun test # a cargo test, a go-to-definition
EOF
put "$root" .github/actions/setup-bun/action.yml <<'EOF'
name: Set up Bun
runs:
  using: composite
  steps:
    - shell: bash
      run: echo "installing bun"
EOF
run_rule "$root"
passes 1 1 '0 online steps with a reason: (none)'

# --- What makes a job a Go job ------------------------------------------------------------------

case_ "a job whose Go comes from a local action that runs setup-go, itself or through another local action, is refused"
root=$(tree action-go)
put "$root" .github/workflows/ci.yaml <<'EOF'
name: CI
on: pull_request
jobs:
  notes:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@08eba0b27e820071cde6df949e0beb9ba4906955 # v4.3.0
      - name: Check the notes
        uses: ./.github/actions/check-notes
  specs:
    runs-on: ubuntu-24.04
    steps:
      - name: Check the specs
        uses: ./.github/actions/check-specs/
EOF
put "$root" .github/actions/check-notes/action.yml <<'EOF'
name: Check the notes
runs:
  using: composite
  steps:
    - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
      with:
        go-version-file: go.mod
    - shell: bash
      run: echo checked
EOF
put "$root" .github/actions/check-specs/action.yaml <<'EOF'
name: Check the specs
runs:
  using: composite
  steps:
    - uses: ./.github/actions/check-notes
EOF
run_rule "$root"
refused_job "$root" .github/workflows/ci.yaml notes "Check the notes" 'GOPROXY=off (it is unset) and GOFLAGS'
refused_job "$root" .github/workflows/ci.yaml specs "Check the specs" 'GOPROXY=off (it is unset) and GOFLAGS'

case_ "a local action under a second checkout of this repository is read from this tree; one of another repository's checkout is not"
root=$(tree checkout-action)
put "$root" .github/workflows/ci.yaml <<'EOF'
name: CI
on: pull_request
jobs:
  base:
    runs-on: ubuntu-24.04
    steps:
      - name: Check out the merge base
        uses: actions/checkout@08eba0b27e820071cde6df949e0beb9ba4906955 # v4.3.0
        with:
          path: base
      - name: Check the base's notes
        uses: ./base/.github/actions/check-notes
  other:
    runs-on: ubuntu-24.04
    steps:
      - name: Check out another repository
        uses: actions/checkout@08eba0b27e820071cde6df949e0beb9ba4906955 # v4.3.0
        with:
          repository: example/other
          path: other
      - name: Check the other repository's notes
        uses: ./other/.github/actions/check-notes
EOF
put "$root" .github/actions/check-notes/action.yml <<'EOF'
name: Check the notes
runs:
  using: composite
  steps:
    - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
EOF
run_rule "$root"
refused_job "$root" .github/workflows/ci.yaml base "Check the base's notes" 'GOPROXY=off (it is unset) and GOFLAGS'
check "does not call the base's action missing" "$(is "$(count_of "$out" 'uses ./base/.github/actions/check-notes, which has no')" 0)"
check "calls the other repository's action unreadable, at its step's line" \
  "$(contains "$out" "^::error file=.github/workflows/ci.yaml,line=$(line_of "$root/.github/workflows/ci.yaml" "- name: Check the other repository's notes")::job other of .github/workflows/ci.yaml, step \"Check the other repository's notes\", uses ./other/.github/actions/check-notes, which has no action.yml or action.yaml in this tree")"

case_ "a job with no setup-go whose run step runs go test is refused"
root=$(tree run-go)
put "$root" .github/workflows/ci.yaml <<'EOF'
name: CI
on: pull_request
jobs:
  unit:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@08eba0b27e820071cde6df949e0beb9ba4906955 # v4.3.0
      - name: Test
        run: go test ./...
EOF
run_rule "$root"
refused_job "$root" .github/workflows/ci.yaml unit "Test" 'GOPROXY=off (it is unset) and GOFLAGS'

case_ "go -C, a path ending /go, go after a cd and go-fetch.sh make a Go job; go version and a comment do not"
root=$(tree go-forms)
put "$root" .github/workflows/ci.yaml <<'EOF'
name: CI
on: pull_request
env:
  GOPROXY: https://proxy.golang.org|direct
jobs:
  chdir:
    runs-on: ubuntu-24.04
    steps:
      - name: Vet with -C
        run: go -C packages/envoy vet ./...
  path:
    runs-on: ubuntu-24.04
    steps:
      - name: Build with a path
        run: /usr/local/go/bin/go build ./cmd/listener
  after-cd:
    runs-on: ubuntu-24.04
    steps:
      - name: Build after a cd
        run: |
          cd packages/envoy && go build ./cmd/...
  fetch:
    runs-on: ubuntu-24.04
    steps:
      - name: Fetch
        run: .github/scripts/go-fetch.sh file .github/go-fetch/test.txt
  version:
    runs-on: ubuntu-24.04
    steps:
      - run: go version
  prose:
    runs-on: ubuntu-24.04
    steps:
      - run: |
          # go test ./... runs in the chdir job, not here
          echo cargo test
EOF
run_rule "$root"
refused_job "$root" .github/workflows/ci.yaml chdir "Vet with -C" 'GOPROXY=off (it is "https://proxy.golang.org|direct")'
refused_job "$root" .github/workflows/ci.yaml path "Build with a path" 'GOPROXY=off'
refused_job "$root" .github/workflows/ci.yaml after-cd "Build after a cd" 'GOPROXY=off'
refused_job "$root" .github/workflows/ci.yaml fetch "Fetch" 'GOPROXY=off'
check "reads go version as no Go" "$(is "$(count_of "$out" 'job version of')" 0)"
check "reads a comment as no Go" "$(is "$(count_of "$out" 'job prose of')" 0)"
check "counts the four Go jobs" "$(contains "$out" '^4 problems over 4 Go jobs of 1 required workflows read$')"

# --- What cannot be read ------------------------------------------------------------------------

case_ "a called workflow or a local action that is not in the tree, and a workflow that is no YAML, are refused"
root=$(tree missing-workflow)
calls "$root" .github/workflows/ci.yaml pull_request .github/workflows/ci-job-gone.yaml
run_rule "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job that calls it, at the uses: line" \
  "$(contains "$out" '^::error file=.github/workflows/ci.yaml,line=5::job go of .github/workflows/ci.yaml calls .github/workflows/ci-job-gone.yaml, which is not in this tree')"
root=$(tree missing-action)
go_workflow "$root" .github/workflows/ci.yaml pull_request "$WORKFLOW_OFFLINE" '' <<'EOF'
      - name: Check the notes
        uses: ./.github/actions/gone
EOF
run_rule "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step that uses it, at its line" \
  "$(contains "$out" "^::error file=.github/workflows/ci.yaml,line=$(line_of "$root/.github/workflows/ci.yaml" '- name: Check the notes')::job vet of .github/workflows/ci.yaml, step \"Check the notes\", uses ./.github/actions/gone, which has no action.yml or action.yaml in this tree")"
root=$(tree not-yaml)
calls "$root" .github/workflows/ci.yaml pull_request .github/workflows/ci-job-broken.yaml
printf 'name: broken\non: workflow_call\njobs:\n  vet:\n    steps: [\n' | put "$root" .github/workflows/ci-job-broken.yaml
run_rule "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file that is no YAML" \
  "$(contains "$out" '^::error file=.github/workflows/ci-job-broken.yaml,line=[0-9]*::.github/workflows/ci-job-broken.yaml cannot be read as YAML')"

case_ "a tree with no ci.yaml gives UNKNOWN, read from the root above the script when none is named"
root=$(tree no-ci)
go_workflow "$root" .github/workflows/nightly.yaml schedule '' '' </dev/null
run_rule "$root"
check "exits 2" "$(is "$status" 2)"
check "says no workflow is known to be required" "$(contains "$out" '^UNKNOWN: no .github/workflows/ci.yaml here')"
mkdir -p "$root/.github/scripts"
ln -s "$rule" "$root/.github/scripts/go_offline_jobs.py"
set +e
out=$(python3 "$root/.github/scripts/go_offline_jobs.py" 2>&1)
status=$?
set -e
check "with no root named, reads the tree the script sits in: exits 2" "$(is "$status" 2)"
check "and says the same" "$(contains "$out" '^UNKNOWN: no .github/workflows/ci.yaml here')"

case_ "a ci.yaml whose jobs run no Go gives UNKNOWN"
root=$(tree no-go)
put "$root" .github/workflows/ci.yaml <<'EOF'
name: CI
on: pull_request
jobs:
  ts:
    runs-on: ubuntu-24.04
    steps:
      - run: bun test
EOF
run_rule "$root"
check "exits 2" "$(is "$status" 2)"
check "says what it read and found no Go in" \
  "$(contains "$out" '^UNKNOWN: read 1 required workflows (.github/workflows/ci.yaml) and found no Go job in them')"

close_case

# --- The repository's own tree ------------------------------------------------------------------
# Run for its verdict and not judged: the repository's workflows are still being moved to this
# shape, so a case that held them to it would fail until they are. Only the verdict's last line is
# printed, so a run of this harness in CI makes no annotation of its own.
repository=$(cd "$script_dir/../.." && pwd)
run_rule "$repository"
echo "the repository's own tree (not judged here): exit $status: $(tail -n 1 <<<"$out")"

echo
echo "checks: $PASS passed, $FAIL failed"
echo "go-offline-jobs.test.sh: $CASES_PASSED of $CASES cases passed"
[ "$CASES_PASSED" -eq "$CASES" ] || exit 1
