#!/usr/bin/env bash
# Tests for package_installs.py: each form of package install it refuses in a required workflow,
# planted in a tree of its own beside the form it lets pass, and the reading it reports as UNKNOWN.
#
# Each case writes a repository of its own - a .github/workflows/ci.yaml, the workflows it calls,
# the local actions their steps use, and a workflow nothing calls - and runs the real rule over it
# as a program, so what is tested is the verdict and the annotation a run of the check prints. No
# network and nothing of this repository's own workflows are read.
#
# Run from anywhere: .github/scripts/package-installs.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
rule="$script_dir/package_installs.py"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# has <text> <fixed string>: true/false for check, the string read as itself; lacks is its negation.
has() { grep -qF -- "$2" <<<"$1" && echo true || echo false; }
lacks() { grep -qF -- "$2" <<<"$1" && echo false || echo true; }

# tree NAME: a fresh repository root, printed. write ROOT PATH: its file PATH, from stdin.
tree() {
  local root="$work/$1"
  rm -rf "$root"
  mkdir -p "$root/.github/workflows"
  echo "$root"
}
write() {
  mkdir -p "$(dirname "$1/$2")"
  cat > "$1/$2"
}

# run_rule ROOT: the rule over ROOT; its stdout in `out`, stderr in `err`, exit code in `rc`.
run_rule() {
  set +e
  PYTHONDONTWRITEBYTECODE=1 python3 "$rule" "$1" > "$work/stdout" 2> "$work/stderr"
  rc=$?
  set -e
  out=$(cat "$work/stdout")
  err=$(cat "$work/stderr")
}

# one_job ROOT STEP-YAML: a ci.yaml of one job, `build`, whose steps are STEP-YAML (indented to
# sit under `steps:`).
one_job() {
  write "$1" .github/workflows/ci.yaml <<YAML
name: CI
on: [pull_request]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
$2
YAML
}

# The cached path, as a required job takes it.
CACHED_STEPS='      - name: Read the runner release for the apt cache key
        id: apt-image
        run: .github/scripts/apt-cache.sh image >> "$GITHUB_OUTPUT"
      - uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9
        with:
          path: ${{ runner.temp }}/apt
          key: apt-${{ runner.os }}-${{ runner.arch }}-${{ steps.apt-image.outputs.image }}-${{ hashFiles('"'"'.github/apt-packages.txt'"'"', '"'"'.github/scripts/apt-cache.sh'"'"') }}
      - name: Install tmux
        timeout-minutes: 5
        run: .github/scripts/apt-cache.sh install .github/apt-packages.txt "$RUNNER_TEMP/apt" tmux'

echo "case: a bare sudo apt-get in ci.yaml is refused, naming the workflow, the job, the step and the line"
root=$(tree in-ci)
one_job "$root" '      - name: Install tmux
        run: sudo apt-get install -y --no-install-recommends tmux'
run_rule "$root"
check "exits 1" "$(is "$rc" 1)"
check "an annotation at the run line naming the job, the step and the command" \
  "$(has "$out" '::error file=.github/workflows/ci.yaml,line=9::.github/workflows/ci.yaml, job build, step "Install tmux" runs `apt-get install -y --no-install-recommends tmux`')"

echo "case: apt install in a workflow ci.yaml calls, and in one that file calls, is refused"
root=$(tree called)
write "$root" .github/workflows/ci.yaml <<'YAML'
on: [pull_request]
jobs:
  zsh-job:
    uses: ./.github/workflows/ci-job-zsh.yaml
YAML
write "$root" .github/workflows/ci-job-zsh.yaml <<'YAML'
on: workflow_call
jobs:
  zsh-job:
    runs-on: ubuntu-24.04
    steps:
      - name: Install zsh
        run: apt install -y zsh
  deeper:
    uses: ./.github/workflows/ci-job-deeper.yaml
YAML
write "$root" .github/workflows/ci-job-deeper.yaml <<'YAML'
on: workflow_call
jobs:
  deep:
    runs-on: ubuntu-24.04
    steps:
      - run: sudo apt-get update
YAML
run_rule "$root"
check "exits 1" "$(is "$rc" 1)"
check "names the called file, its job and its step" \
  "$(has "$out" '::error file=.github/workflows/ci-job-zsh.yaml,line=7::.github/workflows/ci-job-zsh.yaml, job zsh-job, step "Install zsh" runs `apt install -y zsh`')"
check "follows the called file to the one it calls, naming an unnamed step by its place" \
  "$(has "$out" '.github/workflows/ci-job-deeper.yaml, job deep, step 1 runs `apt-get update`')"

echo "case: apt-get in a local action a required job uses, and in one that action uses, is refused"
root=$(tree action)
one_job "$root" '      - uses: ./.github/actions/install-thing'
write "$root" .github/actions/install-thing/action.yml <<'YAML'
name: Install a thing
runs:
  using: composite
  steps:
    - name: Fetch the thing
      shell: bash
      run: sudo apt-get install -y thing
    - uses: ./.github/actions/inner
YAML
write "$root" .github/actions/inner/action.yaml <<'YAML'
name: Inner
runs:
  using: composite
  steps:
    - shell: bash
      run: apt-get update
YAML
run_rule "$root"
check "exits 1" "$(is "$rc" 1)"
check "names the action, the job that uses it and the step" \
  "$(has "$out" '::error file=.github/actions/install-thing/action.yml,line=7::.github/actions/install-thing/action.yml, the local action .github/actions/install-thing (used by .github/workflows/ci.yaml job build), step "Fetch the thing" runs `apt-get install -y thing`')"
check "follows the action to the action it uses" \
  "$(has "$out" '.github/actions/inner/action.yaml, the local action .github/actions/inner (used by .github/actions/install-thing/action.yml the local action')"

echo "case: apt-get after && and in a multi-line run are refused at their own lines"
root=$(tree multi)
one_job "$root" '      - name: Install tmux
        run: sudo apt-get update && sudo apt-get install -y tmux
      - name: Install zsh
        run: |
          echo "installing"
          # a comment installs nothing: apt-get update && apt-get install -y zsh
          sudo DEBIAN_FRONTEND=noninteractive apt-get \
            install -y zsh'
run_rule "$root"
check "the update before && is refused" "$(has "$out" 'step "Install tmux" runs `apt-get update`')"
check "and the install after it" "$(has "$out" 'step "Install tmux" runs `apt-get install -y tmux`')"
check "a continued line is one command, refused at its first line" \
  "$(has "$out" '::error file=.github/workflows/ci.yaml,line=14::.github/workflows/ci.yaml, job build, step "Install zsh" runs `apt-get install -y zsh`')"
check "the comment and the echo are no install" "$(is "$(grep -c '::error' <<<"$out")" 3)"

echo "case: Playwright's --with-deps and install-deps are refused"
root=$(tree with-deps)
one_job "$root" '      - name: Install Playwright Chromium
        timeout-minutes: 4
        run: bunx playwright install --with-deps chromium
      - name: Install the browsers host packages
        run: npx playwright@1.64.1 install-deps'
run_rule "$root"
check "--with-deps is refused" "$(has "$out" 'step "Install Playwright Chromium" runs `bunx playwright install --with-deps chromium`, whose host packages come from an apt mirror')"
check "install-deps is refused" "$(has "$out" 'step "Install the browsers host packages" runs `npx playwright@1.64.1 install-deps`, whose host packages come from an apt mirror')"
check "exits 1" "$(is "$rc" 1)"

echo "case: a Playwright browser install passes with a timeout-minutes, and is refused without one"
root=$(tree browsers)
one_job "$root" '      - name: Install Playwright Chromium
        timeout-minutes: 4
        run: bunx playwright install chromium'
run_rule "$root"
check "with timeout-minutes it passes" "$(is "$rc" 0)"
check "and is counted" "$(has "$out" '1 Playwright browser installs carry a timeout-minutes and no --with-deps')"
one_job "$root" '      - name: Install Playwright Chromium
        run: bunx playwright install chromium'
run_rule "$root"
check "without one it is refused" "$(has "$out" 'step "Install Playwright Chromium" runs `bunx playwright install chromium` with no timeout-minutes on its step')"
check "and exits 1" "$(is "$rc" 1)"

echo "case: the cached path passes"
root=$(tree cached)
one_job "$root" "$CACHED_STEPS"
run_rule "$root"
check "exits 0" "$(is "$rc" 0)"
check "and counts the install as cached" "$(has "$out" '1 package installs take the cached path')"

echo "case: the cached path without its restore, its key or its bound is refused"
root=$(tree cached-bare)
one_job "$root" '      - name: Install tmux
        timeout-minutes: 5
        run: .github/scripts/apt-cache.sh install .github/apt-packages.txt "$RUNNER_TEMP/apt" tmux'
run_rule "$root"
check "with no restore before it, it is refused naming the directory" \
  "$(has "$out" 'without an actions/cache restore of $RUNNER_TEMP/apt before it in the same job')"
check "and exits 1" "$(is "$rc" 1)"
one_job "$root" "$(grep -v 'timeout-minutes: 5' <<<"$CACHED_STEPS")"
run_rule "$root"
check "with no timeout-minutes, it is refused" "$(has "$out" 'without a timeout-minutes on its step')"
one_job "$root" "$(sed "s#, '.github/scripts/apt-cache.sh'##" <<<"$CACHED_STEPS")"
run_rule "$root"
check "with a key that does not hash the script, it is refused" \
  "$(has "$out" "a key on that restore that starts apt- and hashes .github/apt-packages.txt and .github/scripts/apt-cache.sh")"
one_job "$root" "$(sed 's#path: ${{ runner.temp }}/apt#path: ${{ runner.temp }}/other#' <<<"$CACHED_STEPS")"
run_rule "$root"
check "with a restore of another directory, it is refused" "$(has "$out" 'without an actions/cache restore of $RUNNER_TEMP/apt')"
one_job "$root" '      - name: Fill the cache
        timeout-minutes: 5
        run: .github/scripts/apt-cache.sh fill .github/apt-packages.txt "$RUNNER_TEMP/apt"'
run_rule "$root"
check "a fill in a required job is refused" "$(has "$out" 'runs `.github/scripts/apt-cache.sh fill .github/apt-packages.txt $RUNNER_TEMP/apt`, which fetches from the network on every run')"

echo "case: a workflow ci.yaml does not call is not read"
root=$(tree not-required)
one_job "$root" '      - run: echo hello'
write "$root" .github/workflows/nightly.yaml <<'YAML'
on:
  schedule:
    - cron: "41 4 * * *"
jobs:
  proof:
    runs-on: ubuntu-24.04
    steps:
      - run: sudo apt-get update && sudo apt-get install -y tmux
YAML
run_rule "$root"
check "exits 0" "$(is "$rc" 0)"
check "and reads one workflow" "$(has "$out" '1 required workflows and 0 local actions')"

echo "case: the forms a shell runs apt in are read, and the forms it does not run are not"
root=$(tree forms)
one_job "$root" "      - name: Shell forms
        run: |
          bash -c 'apt-get install -y a'
          echo \"\$(apt-get download b)\"
          timeout 60 apt-get update
          env -u HOME DEBIAN_FRONTEND=noninteractive apt-get install c
          sudo dpkg -i ./d.deb
          bash <<'EOF'
          apt-get install -y e
          EOF
      - name: Forms that install nothing
        run: |
          echo 'apt-get install -y f'
          cat > Dockerfile <<'EOF'
          RUN apt-get update && apt-get install -y g
          EOF
          apt list --installed
          apt-cache policy tmux
          dpkg --print-architecture
          dpkg -l tmux"
run_rule "$root"
check "bash -c's script is read" "$(has "$out" 'runs `apt-get install -y a`')"
check "a \$(...) inside double quotes is read" "$(has "$out" 'runs `apt-get download b`')"
check "timeout is passed over" "$(has "$out" 'runs `apt-get update`')"
check "env and its options and settings are passed over" "$(has "$out" 'runs `apt-get install c`')"
check "dpkg -i is refused" "$(has "$out" 'runs `dpkg -i ./d.deb`, which installs a .deb this rule cannot trace')"
check "a heredoc a shell reads is read as its script" "$(has "$out" 'runs `apt-get install -y e`')"
check "and nothing of the second step is" "$(lacks "$out" 'step "Forms that install nothing"')"
check "six refusals in all" "$(is "$(grep -c '::error' <<<"$out")" 6)"

echo "case: a script a shell reads from a pipe, through sudo or not, is read as that shell's script"
root=$(tree piped)
one_job "$root" "      - name: Piped forms
        run: |
          echo \"apt-get install -y p1\" | sudo bash
          printf 'apt-get install -y p2\\n' | sudo bash
          cat <<'EOF' | sudo bash
          apt-get install -y p3
          EOF
          echo \"apt-get install -y p4\" | bash
          sudo bash -c 'apt-get install -y p5'
          sudo sh <<'EOF'
          apt-get install -y p6
          EOF
          eval \"apt-get install -y p7\"
      - name: Pipes that run no install
        run: |
          echo 'apt-get install -y q1' | tee notes.txt
          printf 'apt-get install -y q2\\n' > install.sh"
run_rule "$root"
check "echo piped to sudo bash is refused at its line" \
  "$(has "$out" '::error file=.github/workflows/ci.yaml,line=10::.github/workflows/ci.yaml, job build, step "Piped forms" runs `apt-get install -y p1`')"
check "printf piped to sudo bash is refused" "$(has "$out" 'step "Piped forms" runs `apt-get install -y p2`')"
check "a cat of a heredoc piped to sudo bash is refused at the heredoc's line" \
  "$(has "$out" '::error file=.github/workflows/ci.yaml,line=13::.github/workflows/ci.yaml, job build, step "Piped forms" runs `apt-get install -y p3`')"
check "echo piped to bash is refused" "$(has "$out" 'step "Piped forms" runs `apt-get install -y p4`')"
check "sudo bash -c's script is read" "$(has "$out" 'step "Piped forms" runs `apt-get install -y p5`')"
check "a heredoc sudo sh reads is read at its line" \
  "$(has "$out" '::error file=.github/workflows/ci.yaml,line=18::.github/workflows/ci.yaml, job build, step "Piped forms" runs `apt-get install -y p6`')"
check "what eval joins is read" "$(has "$out" 'step "Piped forms" runs `apt-get install -y p7`')"
check "a pipe into no shell and a printf into a file are no install" "$(lacks "$out" 'step "Pipes that run no install"')"
check "seven refusals in all" "$(is "$(grep -c '::error' <<<"$out")" 7)"

echo "case: an install two shells deep, and one that an echo through sudo feeds to sudo bash, are read"
root=$(tree nested)
one_job "$root" "      - name: Nested forms
        run: |
          bash -c \"sudo bash -c 'apt-get install -y n1'\"
          sudo echo 'apt-get install -y n2' | sudo bash"
run_rule "$root"
check "a shell's -c inside a shell's -c is read" "$(has "$out" 'step "Nested forms" runs `apt-get install -y n1`')"
check "an echo through sudo that feeds sudo bash is read" "$(has "$out" 'step "Nested forms" runs `apt-get install -y n2`')"
check "two refusals in all" "$(is "$(grep -c '::error' <<<"$out")" 2)"

echo "case: a called file that is missing is refused, and a tree with nothing to read is UNKNOWN"
root=$(tree missing)
write "$root" .github/workflows/ci.yaml <<'YAML'
on: [pull_request]
jobs:
  gone:
    uses: ./.github/workflows/ci-job-gone.yaml
  here:
    runs-on: ubuntu-24.04
    steps:
      - run: echo hello
YAML
run_rule "$root"
check "the missing file is named" "$(has "$out" '.github/workflows/ci-job-gone.yaml is called by a required workflow and is not in the tree')"
check "and exits 1" "$(is "$rc" 1)"
root=$(tree no-ci)
run_rule "$root"
check "no ci.yaml is UNKNOWN, exit 2" "$(is "$rc" 2)"
check "and says what it could not read" "$(has "$err" 'UNKNOWN: no .github/workflows/ci.yaml under')"
root=$(tree no-run)
one_job "$root" ''
run_rule "$root"
check "a ci.yaml with no run step is UNKNOWN, exit 2" "$(is "$rc" 2)"
check "and says so" "$(has "$err" 'UNKNOWN: read no run step of the 1 required workflows')"

echo "case: check() answers as the program does, with (problems, summary)"
root=$(tree api)
one_job "$root" '      - name: Install tmux
        run: sudo apt-get install -y tmux'
api=$(cd "$script_dir" && PYTHONDONTWRITEBYTECODE=1 python3 -c '
import sys
import package_installs as rule
problems, summary = rule.check(sys.argv[1])
print(len(problems), problems[0][0], problems[0][1])
print(summary)' "$root")
check "check() gives (path, line, message)" "$(has "$api" '1 .github/workflows/ci.yaml 9')"
check "and the one line, with the count of refusals" \
  "$(has "$api" '1 package installs a required job makes off the cached path, over the 1 required workflows and 0 local actions, 1 jobs and 1 run steps read')"
root=$(tree api-no-run)
one_job "$root" ''
api=$(cd "$script_dir" && PYTHONDONTWRITEBYTECODE=1 python3 -c '
import sys
import package_installs as rule
problems, summary = rule.check(sys.argv[1])
print(len(problems), summary)' "$root")
check "check() of a tree with no run step gives no problem and an UNKNOWN summary" \
  "$(has "$api" '0 UNKNOWN: read no run step of the 1 required workflows')"

summary "package_installs.py"
