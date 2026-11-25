#!/usr/bin/env bash
# Tests for check-log-guard.sh: a network client a step runs outside ci-log-guard.sh, itself or in a
# script it runs, fails by name unless .github/log-guard-allow.tsv names it with a reason; a routed
# one passes; a row naming no command, or giving no reason, fails; and a tree with no workflow fails
# rather than passes.
#
# The first cases run over a copy of this repository's own .github and of the scripts its steps
# run: unplanted it passes, with one unguarded step planted it fails naming that step, and with
# ci-filters-generate.sh's base fetch put back unguarded it fails naming that script. The rest run
# over fixture trees; the reader's forms that run no command are each run under bash too.
#
# Run from anywhere: .github/scripts/check-log-guard.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# run_check <root>: runs the check in <root>, leaving its output in $out and its exit in $status.
run_check() {
  status=0
  out=$("$1/.github/scripts/check-log-guard.sh" 2>&1) || status=$?
}

# fixture <name> <workflow run: body> [<allow rows>]: a tree holding the check, the guard library a
# step sources (a script the check reads, so a step that sources it names a file), one workflow
# whose one step runs the body, and an allow list of the rows given.
fixture() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/.github/actions"
  ln -s "$script_dir/check-log-guard.sh" "$root/.github/scripts/check-log-guard.sh"
  ln -s "$script_dir/shell_commands.py" "$root/.github/scripts/shell_commands.py"
  ln -s "$script_dir/ci-log-guard.sh" "$root/.github/scripts/ci-log-guard.sh"
  {
    printf 'on: push\njobs:\n  app:\n    runs-on: ubuntu-24.04\n    steps:\n      - name: Fetch\n        run: |\n'
    # shellcheck disable=SC2001 # every line of a multi-line body
    sed 's/^/          /' <<<"$2"
  } >"$root/.github/workflows/app.yaml"
  printf '# rows\n%s' "${3:-}" >"$root/.github/log-guard-allow.tsv"
  echo "$root"
}

echo "case: this repository's own tree passes, and an unguarded step planted in it fails by name"
repo=$(cd "$script_dir/../.." && pwd)
# copy_repo <dir>: this repository's .github and every other script the check reads, as the
# check's own `scripts` mode lists them, into <dir>.
copy_repo() {
  mkdir -p "$1"
  cp -r "$repo/.github" "$1/"
  local listed
  listed=$(cd "$repo" && .github/scripts/check-log-guard.sh scripts)
  while IFS= read -r script; do
    (cd "$repo" && cp --parents -- "$script" "$1")
  done <<<"$listed"
}
copy_repo "$work/repo"
run_check "$work/repo"
check "the tree as it is passes" "$(is "$status" 0)"
# The one case that holds this repository's own tree: when it fails, the reason is the check's.
[ "$status" = 0 ] || printf '%s\n' "$out"
check "and says how many commands it read" "$(contains "$out" 'unguarded network commands, every one on')"
check "and that it read the scripts the steps run" "$(contains "$out" ' files and [1-9][0-9]* scripts, ')"
check "and that every script name no file answers is on the reviewed list" \
  "$(contains "$out" ' and [0-9][0-9]* script names no file of this repository answers not read, every one on .github/log-guard-unread.tsv$')"
cat >>"$work/repo/.github/workflows/ci-job-test.yaml" <<'YAML'
  planted:
    runs-on: ubuntu-24.04
    steps:
      - name: Planted test step
        run: go test ./...
YAML
run_check "$work/repo"
check "a planted go test outside the guard fails" "$(is "$status" 1)"
check "naming the file, the job, the step and the command" \
  "$(contains "$out" '::error file=.github/workflows/ci-job-test.yaml::job planted step "Planted test step" runs .go test ./....')"

# The fetch this check could not see while it read only `run:` text: ci-filters-generate.sh's
# fetch of the base commit, which ci.yaml's changes job runs on every pull request and merge group.
echo "case: ci-filters-generate.sh's base fetch put back unguarded fails by name"
copy_repo "$work/repo-fetch"
generate="$work/repo-fetch/.github/scripts/ci-filters-generate.sh"
guarded='  { timeout -k 5 60 git fetch -q --no-tags --depth=1 origin "$base" 2>&1 | tail -3; } 2>&1 | ci_log_guard cat'
check "the copy holds the guarded fetch once" "$(is "$(grep -c -x -F -- "$guarded" "$generate")" 1)"
python3 - "$generate" "$guarded" <<'PY'
import sys
path, guarded = sys.argv[1:]
text = open(path).read()
open(path, "w").write(text.replace(guarded + "\n", '  timeout -k 5 60 git fetch -q --no-tags --depth=1 origin "$base" 2>&1 | tail -3\n'))
PY
run_check "$work/repo-fetch"
check "fails" "$(is "$status" 1)"
check "naming the script, the step that runs it and the fetch" \
  "$(contains "$out" '^::error file=.github/scripts/ci-filters-generate.sh::the script .github/scripts/ci-filters-generate.sh (job changes step "[^"]*" of .github/workflows/ci.yaml runs it) runs .timeout -k 5 60 git fetch -q --no-tags --depth=1 origin \$base 2., a git command')"
check "and counts that one problem" "$(contains "$out" '^::error::check-log-guard: 1 problem(s)$')"

# Two scripts CI runs that the check read only once it read the paths this repository's scripts
# name each other by (`"$(dirname "${BASH_SOURCE[0]}")/…"`, `"$script_dir/…"`): runner-shutdown.sh,
# which ci-ok.sh sources, and post-check-run.sh, which ci-ok-report.sh runs.
for planted in runner-shutdown.sh post-check-run.sh; do
  echo "case: a git fetch planted in $planted fails by name"
  copy_repo "$work/repo-$planted"
  printf '\ngit fetch origin main\n' >>"$work/repo-$planted/.github/scripts/$planted"
  run_check "$work/repo-$planted"
  check "fails" "$(is "$status" 1)"
  check "naming the script and the fetch" \
    "$(contains "$out" "^::error file=.github/scripts/$planted::the script .github/scripts/$planted (job [^ ]* step \"[^\"]*\" of [^ ]* runs it) runs .git fetch origin main., a git command")"
  check "and counts that one problem" "$(contains "$out" '^::error::check-log-guard: 1 problem(s)$')"
done

# A script is the first word of a command the step does not route, read through an interpreter,
# a runner or `.`, from the repository root ($GITHUB_WORKSPACE, ${{ github.workspace }}), the step's
# working directory, or a composite action's own ($GITHUB_ACTION_PATH); a script it runs is read
# too, and a script run under the guard is not.
echo "case: each script a step runs, and each script such a script runs, is read as a step is"
root=$(fixture scripts "$(cat <<'SH'
.github/scripts/fetch.sh
bash scripts/by-interpreter.sh
"$GITHUB_WORKSPACE/scripts/by-workspace.sh" x
timeout 60 ./scripts/behind-runner.sh
. scripts/sourced.sh
${{ github.workspace }}/scripts/by-expression.sh
ci_log_guard scripts/guarded-whole.sh
SH
)")
mkdir -p "$root/scripts" "$root/sub" "$root/.github/actions/act"
printf '#!/usr/bin/env bash\ngit fetch origin main\nscripts/nested.sh\n' >"$root/.github/scripts/fetch.sh"
printf 'curl -sS https://example.com/a\n' >"$root/scripts/by-interpreter.sh"
printf 'gh api repos/o/r\n' >"$root/scripts/by-workspace.sh"
printf 'go build ./x\n' >"$root/scripts/behind-runner.sh"
printf 'docker pull ghcr.io/o/i\n' >"$root/scripts/sourced.sh"
printf 'crane copy a b\n' >"$root/scripts/by-expression.sh"
printf 'go vet ./...\n' >"$root/scripts/guarded-whole.sh"
printf '#!/bin/sh\ngo test ./...\n' >"$root/scripts/nested.sh"
printf 'sudo apt-get install -y x\n' >"$root/sub/tool.sh"
printf 'npm install\n' >"$root/.github/scripts/action-script.sh"
printf 'on: push\njobs:\n  wd:\n    runs-on: ubuntu-24.04\n    steps:\n      - name: Tool\n        working-directory: sub\n        run: ./tool.sh\n' \
  >"$root/.github/workflows/wd.yaml"
printf 'runs:\n  using: composite\n  steps:\n    - name: Run\n      shell: bash\n      run: "\\"$GITHUB_ACTION_PATH/../../scripts/action-script.sh\\""\n' \
  >"$root/.github/actions/act/action.yml"
run_check "$root"
check "fails" "$(is "$status" 1)"
by_fetch='(job app step "Fetch" of .github/workflows/app.yaml runs it)'
for found in ".github/scripts/fetch.sh $by_fetch runs .git fetch origin main." \
  "scripts/nested.sh $by_fetch runs .go test ./..." \
  "scripts/by-interpreter.sh $by_fetch runs .curl -sS https://example.com/a." \
  "scripts/by-workspace.sh $by_fetch runs .gh api repos/o/r." \
  "scripts/behind-runner.sh $by_fetch runs .go build ./x." \
  "scripts/sourced.sh $by_fetch runs .docker pull ghcr.io/o/i." \
  "scripts/by-expression.sh $by_fetch runs .crane copy a b." \
  'sub/tool.sh (job wd step "Tool" of .github/workflows/wd.yaml runs it) runs .sudo apt-get install -y x.' \
  '.github/scripts/action-script.sh (job - step "Run" of .github/actions/act/action.yml runs it) runs .npm install.'; do
  check "names \`${found%% *}\`" "$(contains "$out" "::the script $found")"
done
check "reads no script the step runs under the guard" "$(is "$(contains "$out" 'guarded-whole')" false)"
check "and counts nine problems" "$(contains "$out" '^::error::check-log-guard: 9 problem(s)$')"

# A path after a variable or a substitution is read from the script's own directory, else from
# each directory above it; a variable set to a directory under another is read through it; a
# relative path after a literal `cd` is read from where the cd went. A script a harness writes at
# run time is no file of the tree, and is counted as not read, not refused.
echo "case: the paths this repository's scripts name each other by are read"
root=$(fixture siblings '.github/scripts/top.sh')
mkdir -p "$root/.github/scripts/sub" "$root/scripts" "$root/sub2"
cat >"$root/.github/scripts/top.sh" <<'SH'
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
"$script_dir/by-variable.sh"
bash "$(dirname "$0")/by-dirname.sh"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/by-root.sh"
readonly drivers="${script_dir}/sub"
"${drivers}/by-subdir.sh"
cd sub2 && ./after-cd.sh
SH
printf 'git fetch origin main\n' >"$root/.github/scripts/by-variable.sh"
printf 'curl -sS https://example.com/a\n' >"$root/.github/scripts/by-dirname.sh"
printf 'gh api repos/o/r\n' >"$root/scripts/by-root.sh"
printf 'go build ./x\n' >"$root/.github/scripts/sub/by-subdir.sh"
printf 'docker pull ghcr.io/o/i\n' >"$root/sub2/after-cd.sh"
run_check "$root"
check "fails" "$(is "$status" 1)"
for found in ".github/scripts/by-variable.sh $by_fetch runs .git fetch origin main." \
  ".github/scripts/by-dirname.sh $by_fetch runs .curl -sS https://example.com/a." \
  "scripts/by-root.sh $by_fetch runs .gh api repos/o/r." \
  ".github/scripts/sub/by-subdir.sh $by_fetch runs .go build ./x." \
  "sub2/after-cd.sh $by_fetch runs .docker pull ghcr.io/o/i."; do
  check "names \`${found%% *}\`" "$(contains "$out" "::the script $found")"
done
check "and counts five problems" "$(contains "$out" '^::error::check-log-guard: 5 problem(s)$')"
# A script a step writes at run time is no file of the tree. Unlisted, it fails by name, so a
# script the walk stops finding cannot pass unread; listed with its reason, it passes and is
# counted; and a listed name the check now reads, or one no step holds, fails as a stale row.
echo "case: a script name no file answers fails unless the unread list names it, and a stale row fails"
root=$(fixture made-at-run-time "$(printf '%s\n' 'work=$(mktemp -d)' '"$work/made.sh"')")
run_check "$root"
check "unlisted, it fails" "$(is "$status" 1)"
check "naming the file and the word" \
  "$(contains "$out" '^::error file=.github/workflows/app.yaml::.github/workflows/app.yaml runs .\$work/made.sh., a script no file of this repository answers')"
check "and counts that one problem" "$(contains "$out" '^::error::check-log-guard: 1 problem(s)$')"
check "which \`unresolved\` lists" \
  "$(contains "$("$root/.github/scripts/check-log-guard.sh" unresolved 2>&1)" $'^.github/workflows/app.yaml\t\\$work/made.sh$')"
printf '# rows\n.github/workflows/app.yaml\t$work/made.sh\tthe step writes it at run time\n' >"$root/.github/log-guard-unread.tsv"
run_check "$root"
check "listed with its reason, it passes" "$(is "$status" 0)"
check "counted as one name on the list" \
  "$(contains "$out" ' and 1 script names no file of this repository answers not read, every one on')"
printf '# rows\n.github/workflows/app.yaml\t$work/made.sh\t\n' >"$root/.github/log-guard-unread.tsv"
run_check "$root"
check "a row with no reason fails" "$(contains "$out" '^::error file=.github/log-guard-unread.tsv,line=2::row 2 is not file, script name and a reason')"
mkdir -p "$root/scripts"
printf 'echo made\n' >"$root/scripts/made.sh"
printf '# rows\n.github/workflows/app.yaml\tscripts/made.sh\tit used to be written at run time\n' >"$root/.github/log-guard-unread.tsv"
sed -i 's|"\$work/made.sh"|scripts/made.sh|' "$root/.github/workflows/app.yaml"
run_check "$root"
check "a listed name the check now reads fails" "$(is "$status" 1)"
check "naming the row" \
  "$(contains "$out" '^::error file=.github/log-guard-unread.tsv,line=2::row 2 names .scripts/made.sh. in .github/workflows/app.yaml, and no script name there now goes unread')"

# The two ways review 6054247075 found a script go unread: env handed an expansion of many words
# before the shell that runs it (`env "${unset_inputs[@]}" … bash x.sh`, as the airlock check's
# test runs it), and a literal path a script names from a directory above its own after
# `cd "$(dirname "$0")/.."` (`. scripts/compose-refusal.sh`, as the deploy script for Dispatch
# names it). Each script is read, and its client named.
echo "case: a script behind env and an expansion, and one named from above the script's own directory"
root=$(fixture env-and-parent "$(printf '%s\n' 'inputs=(-u A)' 'env "${inputs[@]}" PATH=/bin "$@" bash scripts/deep/run.sh')")
mkdir -p "$root/scripts/deep" "$root/scripts/lib"
printf 'cd "$(dirname "$0")/.."\n. lib/helper.sh\n' >"$root/scripts/deep/run.sh"
printf 'curl -sS https://example.com/helper\n' >"$root/scripts/lib/helper.sh"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "naming the client of the script the parent path names" \
  "$(contains "$out" '::the script scripts/lib/helper.sh .* runs .curl -sS https://example.com/helper.')"
check "and counts that one problem" "$(contains "$out" '^::error::check-log-guard: 1 problem(s)$')"

echo "case: a client behind flock, ionice, setsid or unshare fails by name"
root=$(fixture more-runners "$(cat <<'SH'
flock x git fetch origin main
ionice -c 3 git fetch origin a
setsid git fetch origin b
unshare --user -w /tmp git fetch origin c
flock -w 5 x -c 'curl -sS https://example.com/e'
SH
)")
run_check "$root"
check "fails" "$(is "$status" 1)"
for found in 'flock x git fetch origin main' 'ionice -c 3 git fetch origin a' 'setsid git fetch origin b' \
  'unshare --user -w /tmp git fetch origin c'; do
  check "names \`$found\`" "$(contains "$out" "step \"Fetch\" runs \`$found\`")"
done
check "reads flock's -c string" "$(contains "$out" 'step "Fetch" runs .flock -w 5 x -c curl -sS https://example.com/e.')"
check "and counts five problems" "$(contains "$out" '^::error::check-log-guard: 5 problem(s)$')"

# .github/scripts/harness-pool.sh queues a step's harness, whose command runs in the step's
# directory with its output in the log: the command, and the script it names, are read.
echo "case: a client in a script the harness pool queues fails by name"
root=$(fixture pooled "$(printf '%s\n' '.github/scripts/harness-pool.sh start pooled bash scripts/pooled.sh' \
  '.github/scripts/harness-pool.sh start --minutes 5 inline gh api repos/o/p')")
mkdir -p "$root/scripts"
printf 'case "$1" in start) ;; esac\n' >"$root/.github/scripts/harness-pool.sh"
printf 'curl -sS https://example.com/p\n' >"$root/scripts/pooled.sh"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the queued script's curl" "$(contains "$out" "::the script scripts/pooled.sh $by_fetch runs .curl -sS https://example.com/p.")"
check "names the queued command" "$(contains "$out" 'step "Fetch" runs .\.github/scripts/harness-pool.sh start --minutes 5 inline gh api repos/o/p.')"
check "and counts two problems" "$(contains "$out" '^::error::check-log-guard: 2 problem(s)$')"

echo "case: a stderr guard after an ANSI-C string holding an escaped quote fails by name"
root=$(fixture ansi-c "$(printf '%s\n' "v=\$'it\\'s'; ci_log_guard_stderr gh api repos/o/x")")
run_check "$root"
check "fails" "$(is "$status" 1)"
check "naming the call" "$(contains "$out" 'ci_log_guard_stderr gh api repos/o/x.: ci_log_guard_stderr blocks only stderr')"

echo "case: a row naming a script allows its command, and a row naming a client the script runs not fails"
root=$(fixture script-rows '.github/scripts/fetch.sh' \
  "$(printf '.github/scripts/fetch.sh\t-\t-\tgit\tit fetches a repository the job made on disk\n.github/scripts/fetch.sh\t-\t-\tcurl\tits curl is gone')")
printf 'git fetch origin main\n' >"$root/.github/scripts/fetch.sh"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "naming the row of the client the script does not run" \
  "$(contains "$out" 'row 3 names the script .github/scripts/fetch.sh running curl unguarded, and no script a step runs does: remove the row')"
check "and nothing else" "$(contains "$out" '^::error::check-log-guard: 1 problem(s)$')"

# The reader's forms, each proved against bash: the body runs under bash with a stand-in for each
# client on PATH that logs its name, and the check names exactly the clients bash ran. An ANSI-C
# string, a word after a `$(…)`, a case pattern, a quoted here-document's body, the name of a
# function `()` defines and arithmetic's `<<` run nothing; the command inside the case item does,
# and so does `command curl`, which skips the function. A here-document a shell reads as its
# script runs its lines, whether the shell stands before the `<<` or after a pipe, behind a runner
# or given /dev/stdin, and an unquoted one's substitutions run, as do arithmetic's. An array's
# values end at the first unquoted `)`, and a process substitution among them runs. A
# ci_log_guard_stderr call in a string that a line before opened is text, and one after the string
# closes is a call.
echo "case: the forms that run no command read as none, and those that run one as one, as bash runs them"
cat >"$work/forms.sh" <<'SH'
bash -c 'FOO=1 curl -sS https://example.com/E'
eval 'if curl -sS https://example.com/F; then :; fi'
( trap 'X=1 command curl -sS https://example.com/G' EXIT )
bash -c '! curl -sS https://example.com/H'
bash -c '{ curl -sS https://example.com/I; }'
eval 'while ! curl -sS https://example.com/J; do :; done'
sh -c 'GOFLAGS=-mod=mod go test ./...'
bash -c 'ci_log_guard curl -sS https://example.com/routed-in-string'
b2=(curl -sS https://example.com/K)
eval "eval '\"\${b2[@]}\"'"
eval 'a1=(curl -sS https://example.com/L)'
"${a1[@]}"
eval "a2=(curl -sS https://example.com/M)"
"${a2[@]}"
eval 'declare -a a3=(curl -sS https://example.com/N)'
"${a3[@]}"
eval 'a4=(curl -sS https://example.com/O)'
f4() { "${a4[@]}"; }
f4
bash -c 'eval "a5=(curl -sS https://example.com/P)"; "${a5[@]}"'
case x in "$(: # )
curl -sS https://example.com/Q)") : ;; esac
case "" in "$(: # "
)") curl -sS https://example.com/R ;; esac
(( $(: # )
curl -sS https://example.com/S; echo 1)))
env "${@:2}" curl -sS https://example.com/T
a6=()
env "${a6[@]:0}" curl -sS https://example.com/U
x=$(cat <<'EOF'
hello
EOF )
curl -sS https://example.com/y
y=$(cat <<'EOF'
hello
EOF	)
curl -sS https://example.com/z
z=`cat <<'EOF'
hello
EOF`
curl -sS https://example.com/A
b=(curl -sS https://example.com/B)
eval '"${b[@]}"'
((echo "))"; curl -sS https://example.com/C) || true)
case x in "$(echo ")"; curl -sS https://example.com/D)") : ;; esac
a=(curl -sS https://example.com/k)
"${a[@]}"
coproc curl -sS https://example.com/l
wait
coproc C { curl -sS https://example.com/m; }
wait
trap 'command curl -sS https://example.com/n' EXIT
function f { curl -sS https://example.com/o; }
f
bash <<< 'curl -sS https://example.com/p'
echo 'curl -sS https://example.com/q' | bash
cat <<'EOT' | xargs -I{} bash -c '{}'
curl -sS https://example.com/r
EOT
shopt -s expand_aliases
alias c='curl -sS'
c https://example.com/s
$'\x63url' -sS https://example.com/t
((curl -sS https://example.com/u) || true)
case x in "$(curl -sS https://example.com/v)") : ;; esac
args=()
env "${args[@]}" bash <<'EOT'
curl -sS https://example.com/w
EOT
env -S 'curl -sS https://example.com/x'
files=(<(curl -sS https://example.com/e))
wait $!
x=("(")
curl -sS https://example.com/f
# )
v=$'curl -sS https://example.com/a \' still quoted'
v=curl
echo "$(printf x)" gh api repos/o/r
case "$v" in
  curl | jq) gh api repos/o/s ;;
  *) ;;
esac
cat <<'EOF' >/dev/null
curl -sS https://example.com/b
EOF
curl() { echo "the function named curl"; }
n=$(( 1 << 2 ))
msg="a string a line opens
ci_log_guard_stderr gh api repos/o/t" ; ci_log_guard_stderr gh api repos/o/u
command curl -sS https://example.com/c
bash <<'EOT'
curl -sS https://example.com/d
EOT
cat <<EOF >/dev/null
$(gh api repos/o/v)
EOF
cat <<'EOF' >/dev/null
$(gh api repos/o/w)
EOF
n=$(( $(jq -n 1) + 1 ))
cat <<'EOT' | bash
curl -sS https://example.com/g
EOT
bash /dev/stdin <<'EOT'
curl -sS https://example.com/h
EOT
setsid bash <<'EOT'
curl -sS https://example.com/i
EOT
timeout --signal KILL 5m bash <<'EOT'
curl -sS https://example.com/j
EOT
SH
mkdir -p "$work/bin"
for client in curl gh jq go; do
  printf '#!/bin/sh\necho %s >>"%s/ran"\n' "$client" "$work" >"$work/bin/$client"
  chmod +x "$work/bin/$client"
done
: >"$work/ran"
(cd "$work" && PATH="$work/bin:$PATH" bash -c ". '$script_dir/ci-log-guard.sh'; . ./forms.sh" >/dev/null 2>&1)
check "bash runs curl forty-five times, go once, gh three times and jq once" \
  "$(is "$(sort "$work/ran" | uniq -c | tr -s ' \n' ' ')" " 45 curl 3 gh 1 go 1 jq ")"
root=$(fixture forms "$(cat "$work/forms.sh")")
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the case item's gh" "$(contains "$out" 'step "Fetch" runs .gh api repos/o/s.')"
check "names the command curl" "$(contains "$out" 'step "Fetch" runs .command curl -sS https://example.com/c.')"
check "names the curl of the body bash reads" "$(contains "$out" 'step "Fetch" runs .curl -sS https://example.com/d.')"
check "names the gh of the unquoted body's substitution" "$(contains "$out" 'step "Fetch" runs .gh api repos/o/v.')"
check "and not the quoted body's" "$(is "$(contains "$out" 'repos/o/w')" false)"
# e to j: the forms of review 6052147931; k to x: those of 5aef29db5a's two reviews, each a
# command word a shell builds rather than one written (an array, a coprocess, a trap, a one-line
# function, a here-string, a piped echo, xargs -I, an alias, an ANSI-C escape, two subshells, a
# case pattern, env with an expansion of many words, env -S); y to D: review 6058074395's (a body
# ended by `EOF )`, `EOF<tab>)` or a backtick close, an array set outside an eval string and run
# inside it, and a quoted paren in `((` and in a case pattern's substitution); E to U: review 6065555289's (a client behind an assignment or a keyword in
# a `-c`, eval or trap string, an array used in an eval inside an eval, an array an eval or a
# `bash -c` string assigns and the text after it runs, a `#` comment inside a `$(…)` in a case
# pattern or in arithmetic, and env handed a slice).
for form in e f g h i j k l m n o p q r s t u v w x y z A B C D E F G H I J K L M N O P Q R S T U; do
  check "names the curl of form $form" "$(contains "$out" "step \"Fetch\" runs .[^\`]*https://example.com/$form.")"
done
check "names the stderr guard after the string, and not the one inside it" \
  "$(contains "$out" 'runs .ci_log_guard_stderr gh api repos/o/t" ; ci_log_guard_stderr gh api repos/o/u.: ci_log_guard_stderr blocks only stderr')"
check "names the go behind an assignment in a sh -c string" \
  "$(contains "$out" 'step "Fetch" runs .go test \./\.\.\.., a go command')"
check "and not the curl a string runs under ci_log_guard" "$(is "$(contains "$out" 'routed-in-string')" false)"
check "and nothing else" "$(contains "$out" '^::error::check-log-guard: 49 problem(s)$')"

echo "case: each routed form passes"
root=$(fixture routed "$(cat <<'SH'
. .github/scripts/ci-log-guard.sh
ci_log_guard go test ./...
GOOS=darwin ci_log_guard go vet ./...
v=$(ci_log_guard_stderr gh api repos/o/r)
set -o pipefail
{ go list ./... | xargs go test; } 2>&1 | ci_log_guard cat
bun install --frozen-lockfile
git status
echo "go test is what runs"
# curl https://example.com
SH
)")
run_check "$root"
check "passes" "$(is "$status" 0)"
check "and reads no command as unguarded" "$(contains "$out" ' 0 unguarded network commands')"

echo "case: each unguarded form fails by name"
root=$(fixture unrouted "$(cat <<'SH'
GOOS=linux go build ./cmd/x
v=$(gh api repos/o/r)
sudo apt-get install -y zsh
bunx playwright install chromium
git fetch origin main
sh -c 'curl -sS https://example.com'
docker pull ghcr.io/o/i
SH
)")
run_check "$root"
check "fails" "$(is "$status" 1)"
for found in 'go build ./cmd/x' 'gh api repos/o/r' 'sudo apt-get install -y zsh' 'bunx playwright install chromium' \
  'git fetch origin main' 'curl -sS https://example.com' 'docker pull ghcr.io/o/i'; do
  check "names \`$found\`" "$(contains "$out" "step \"Fetch\" runs \`$found\`")"
done
check "and counts seven problems" "$(contains "$out" '^::error::check-log-guard: 7 problem(s)$')"

# The forms a review found the check passed: a client behind a command that runs it with options
# of its own (their values must not be read as the client), behind find's -execdir, between two
# brace groups on one line, and a ci_log_guard_stderr whose stdout goes to the log.
echo "case: each client behind a runner, a find, between two groups, or a loose stderr guard fails by name"
root=$(fixture runners "$(cat <<'SH'
nice -n 10 go test ./...
timeout -s KILL 5m go test ./a
xargs -n 1 go test
sudo -u runner apt-get install -y zsh
env -i PATH=/usr/bin go build ./x
command go build ./y
find . -name go.mod -execdir go build ./... \;
{ true; }; curl -sS https://example.com/x; { true; } 2>&1 | ci_log_guard cat
ci_log_guard_stderr gh api repos/o/r
command -v go
SH
)")
run_check "$root"
check "fails" "$(is "$status" 1)"
for found in 'nice -n 10 go test ./...' 'timeout -s KILL 5m go test ./a' 'xargs -n 1 go test' \
  'sudo -u runner apt-get install -y zsh' 'env -i PATH=/usr/bin go build ./x' 'command go build ./y' \
  'find . -name go.mod -execdir go build ./... ;' 'curl -sS https://example.com/x'; do
  check "names \`$found\`" "$(contains "$out" "step \"Fetch\" runs \`$found\`")"
done
check "names the stderr guard whose stdout reaches the log" \
  "$(contains "$out" 'step "Fetch" runs .ci_log_guard_stderr gh api repos/o/r.: ci_log_guard_stderr blocks only stderr')"
check "reads \`command -v go\` as no client, and counts nine problems" \
  "$(contains "$out" '^::error::check-log-guard: 9 problem(s)$')"

# The second review's forms. A ci_log_guard_stderr is judged by its own command: a redirection of
# another command on its line, or a `$(` closed before it, does not capture its stdout. A shell's
# -c script behind a runner, gh-read.sh's gh_wait_reset and `time -p` are read through.
echo "case: a stderr guard judged by its own command, and a client behind a runner's shell, fail by name"
root=$(fixture own-command "$(cat <<'SH'
ci_log_guard_stderr gh api repos/o/a || { echo "::error::x" >&2; exit 1; }
ci_log_guard_stderr gh api repos/o/b; echo ok >&2
ci_log_guard_stderr gh api repos/o/c && echo done > out.txt
v=$(date); ci_log_guard_stderr gh api repos/o/d
x=$(ci_log_guard_stderr gh api repos/o/e); ci_log_guard_stderr gh api repos/o/f
timeout 600 bash -c 'go test ./a'
sudo sh -c 'apt-get install -y x'
xargs -I{} sh -c 'go test {}'
find . -exec sh -c 'go vet ./...' \;
time -p go test ./b
gh_wait_reset --stderr err.txt gh api repos/o/g
SH
)")
run_check "$root"
check "fails" "$(is "$status" 1)"
for found in 'gh api repos/o/a' 'gh api repos/o/b' 'gh api repos/o/c' 'gh api repos/o/d' 'gh api repos/o/f'; do
  check "names the uncaptured stderr guard of \`$found\`" \
    "$(contains "$out" "runs .[^\`]*ci_log_guard_stderr ${found}[^\`]*.: ci_log_guard_stderr blocks only stderr")"
done
check "reads the captured call of its line as captured" \
  "$(is "$(grep -c 'ci_log_guard_stderr blocks only stderr' <<<"$out")" 5)"
for found in "timeout 600 bash -c go test ./a" "sudo sh -c apt-get install -y x" "xargs -I{} sh -c go test {}" \
  "find . -exec sh -c go vet ./... ;" "time -p go test ./b" "gh_wait_reset --stderr err.txt gh api repos/o/g"; do
  check "names \`$found\`" "$(contains "$out" "step \"Fetch\" runs \`$found\`")"
done

echo "case: a stderr guard whose own command captures or redirects its stdout passes"
root=$(fixture own-command-safe "$(cat <<'SH'
ci_log_guard_stderr gh api repos/o/a >&2 || { echo "::error::x" >&2; exit 1; }
v=$(ci_log_guard_stderr gh api repos/o/b) || exit 1
ci_log_guard_stderr gh api repos/o/c > out.json 2>/dev/null
ci_log_guard_stderr gh api repos/o/d &> both.txt
ci_log_guard_stderr gh api repos/o/e 2>&1 >&2
{ ci_log_guard_stderr gh api repos/o/p; } > out.json
( ci_log_guard_stderr gh api repos/o/q ) > out.json
ci_log_guard_stderr gh api repos/o/r | jq . > out.json
ci_log_guard_stderr gh api repos/o/t | sort >> sorted.txt 2>/dev/null
v="$(ci_log_guard_stderr gh api repos/o/s)"
SH
)")
run_check "$root"
check "passes" "$(is "$status" 0)"

# The third review's forms: a capture opened inside double quotes closes at its own `)`, so a call
# after it on the line is no part of it; a quoted capture holding another, and one split across a
# backslash continuation, close the same way. A comment's `>` redirects nothing, and stdout sent
# into `>(…)` reaches the log through the command there.
echo "case: a call after a quoted capture, after a comment's >, or into >(…), fails by name"
root=$(fixture quoted-capture "$(cat <<'SH'
v="$(date)"; ci_log_guard_stderr gh api repos/o/h
[ -n "$(git rev-parse HEAD)" ] && ci_log_guard_stderr gh api repos/o/i
cd "$(pwd)" && ci_log_guard_stderr gh api repos/o/j
v="$(echo "$(date)")"; ci_log_guard_stderr gh api repos/o/k
v="$(printf '%s' \
  x)"; ci_log_guard_stderr gh api repos/o/l
ci_log_guard_stderr gh api repos/o/m  # stdout -> the log
ci_log_guard_stderr gh api repos/o/n > >(tee out.txt)
SH
)")
run_check "$root"
check "fails" "$(is "$status" 1)"
for found in h i j k l m n; do
  check "names the stderr guard of \`gh api repos/o/$found\`" \
    "$(contains "$out" "runs .[^\`]*ci_log_guard_stderr gh api repos/o/${found}[^\`]*.: ci_log_guard_stderr blocks only stderr")"
done
check "and counts seven problems" "$(contains "$out" '^::error::check-log-guard: 7 problem(s)$')"

# The fourth and fifth reviews' forms: after a pipe the last stage writes after the guard's block
# has closed, so only a file keeps that output out of the log; a stage naming /dev/stderr, a dup to
# a file descriptor and a pipe into ci_log_guard (whose block interleaves with this one under the
# runner's one stop state) all reach it. The shell applies redirections left to right, so the last
# one of stdout decides: a file followed by a dup or /dev/stderr reaches the log too.
echo "case: a call piped into a stage that writes to the log, or into ci_log_guard, fails by name"
root=$(fixture piped "$(cat <<'SH'
ci_log_guard_stderr gh api repos/o/u | sort >&2
ci_log_guard_stderr gh api repos/o/v | tail -n 20 >&2
ci_log_guard_stderr gh api repos/o/w | jq . 1>&2
ci_log_guard_stderr gh api repos/o/x | tee /dev/stderr > out.json
ci_log_guard_stderr gh api repos/o/y | ci_log_guard cat
ci_log_guard_stderr gh api repos/o/z | jq . > /dev/stdout
ci_log_guard_stderr gh api repos/o/A | sort > out.json >&2
ci_log_guard_stderr gh api repos/o/B | sort > out.json 1>&2
ci_log_guard_stderr gh api repos/o/C | sort > out.json > /dev/stderr
ci_log_guard_stderr gh api repos/o/D > out.json >&1
SH
)")
run_check "$root"
check "fails" "$(is "$status" 1)"
for found in u v w x y z A B C D; do
  check "names the stderr guard of \`gh api repos/o/$found\`" \
    "$(contains "$out" "runs .[^\`]*ci_log_guard_stderr gh api repos/o/${found}[^\`]*.: ci_log_guard_stderr blocks only stderr")"
done
check "and counts ten problems" "$(contains "$out" '^::error::check-log-guard: 10 problem(s)$')"

echo "case: the last redirection of stdout decides, so a file after a dup keeps the stdout"
root=$(fixture last-redirect "$(cat <<'SH'
ci_log_guard_stderr gh api repos/o/E | sort >&2 > out.json
ci_log_guard_stderr gh api repos/o/F >&2 > out.json
ci_log_guard_stderr gh api repos/o/G > /dev/stderr 2>/dev/null
SH
)")
run_check "$root"
check "passes" "$(is "$status" 0)"

echo "case: a row with its reason allows its command, and only that one"
root=$(fixture allowed 'docker pull ghcr.io/o/i' \
  "$(printf '.github/workflows/app.yaml\tapp\tFetch\tdocker\tthe image is pulled by digest and its text was probed')")
run_check "$root"
check "passes" "$(is "$status" 0)"

echo "case: a row with no reason fails"
root=$(fixture no-reason 'docker pull ghcr.io/o/i' "$(printf '.github/workflows/app.yaml\tapp\tFetch\tdocker\t')")
run_check "$root"
check "fails" "$(is "$status" 1)"
check "naming the row" "$(contains "$out" 'row 2 is not file, job, step, client and a reason')"

echo "case: a row naming no unguarded command fails"
root=$(fixture stale 'ci_log_guard docker pull ghcr.io/o/i' \
  "$(printf '.github/workflows/app.yaml\tapp\tFetch\tdocker\tpulled by digest')")
run_check "$root"
check "fails" "$(is "$status" 1)"
check "naming the row and the step" "$(contains "$out" 'row 2 names job app step "Fetch" of .github/workflows/app.yaml running docker unguarded, and no such step does')"

echo "case: a tree with no workflow fails rather than passes"
root=$(fixture empty 'true')
rm "$root/.github/workflows/app.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "saying it checked nothing" "$(contains "$out" 'read no workflow file and no composite action')"

summary "check-log-guard.sh"
