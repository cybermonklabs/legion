#!/usr/bin/env bash
# daemon-contract-order.test.sh - Drives scripts/daemon-contract-order.sh over a scratch
# repository of this test's own, whose history holds each shape a build reads the merge order
# from (dispatch://INFRA-1569/spec, "How a build reads the merge order"). Its main adds entry B,
# then entry A, whose id is earlier; a branch adds C and then forward-merges main; a merge group's
# line past main adds D, then C again with an id earlier than D's, and holds no merge commit;
# another line adds two entries in one commit. Every case reads a clone of that repository, as a
# build does, so main is refs/remotes/origin/main there, the default the script reads. One case
# runs the script from the clone's packages/daemon, where ci-job-daemon-go.yaml's stamped shape
# gate runs it, and requires the list the root prints. The last case reads a clone that holds
# main's history and only the merge group's newest commit, and requires the script to fail naming
# the commit before it, which that clone lacks.
#
# Run from the repository root: bash scripts/daemon-contract-order.test.sh
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly project_root
readonly script="${project_root}/scripts/daemon-contract-order.sh"
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/daemon-contract-order-test.XXXXXX")"
readonly temporary_dir
trap 'rm -rf -- "$temporary_dir"' EXIT

# git reads no configuration of the machine or the user, so a signing key, a hook or a default
# branch name of theirs changes nothing here, and every commit carries one fixed identity.
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
export GIT_AUTHOR_NAME='Order test' GIT_AUTHOR_EMAIL=order-test@example.invalid
export GIT_COMMITTER_NAME='Order test' GIT_COMMITTER_EMAIL=order-test@example.invalid

readonly api=packages/daemon/internal/api
readonly B=20261102000000 A=20261101000000 C=20261103000000 D=20261104000000
# The two entries of one commit: the earlier id has the later slug, so an order by file name's
# slug, or by the order the commit wrote them, is not taken for the id order.
readonly Y=20261105000000 Z=20261106000000

readonly origin="${temporary_dir}/origin"
git init -q -b main "$origin"

# commit <message> <file>...: writes each file under the api directory and commits them.
commit() {
  local message=$1 file
  shift
  for file in "$@"; do
    mkdir -p "$origin/$api"
    printf 'package api\n' >"$origin/$api/$file"
    git -C "$origin" add "$api/$file"
  done
  git -C "$origin" commit -q --allow-empty -m "$message"
}

# A file of the directory that is no entry, beside each entry: the package's own source, an
# entry's `_test.go` rows, and names an entry's pattern must not take.
commit 'the package' contracts.go contract_shapes_test.go
root=$(git -C "$origin" rev-parse HEAD)
commit 'entry B' "contract_${B}_bravo.go" "contract_${B}_bravo_test.go"
git -C "$origin" branch feature
commit 'entry A' "contract_${A}_alpha.go" contract_20261101_short.go contract_2026110100000x_alpha.go

# The branch: C, then main forward-merged into it.
git -C "$origin" checkout -q feature
commit 'entry C' "contract_${C}_charlie.go"
git -C "$origin" merge -q --no-ff -m 'merge main into feature' main

# The merge group's line past main: the squash commit of another pull request, adding D, then the
# branch's squash commit, adding C.
git -C "$origin" checkout -q -b merge-group main
commit 'entry D' "contract_${D}_delta.go"
commit 'entry C, squashed' "contract_${C}_charlie.go"

# Two entries one commit adds.
git -C "$origin" checkout -q -b two-in-one main
commit 'entries Y and Z' "contract_${Z}_yankee.go" "contract_${Y}_zulu.go"
git -C "$origin" checkout -q main

readonly clone="${temporary_dir}/clone"
git clone -q --no-local "$origin" "$clone"

failures=0
case_name=''
status=0
output=''
errors=''

# run <argument>...: the script over the clone, run from its root.
run() {
  run_in "$clone" "$@"
}

# run_in <directory> <argument>...: the script run from the directory, its exit status, standard
# output and error.
run_in() {
  local directory=$1
  shift
  status=0
  output="$(cd "$directory" && bash "$script" "$@" 2>"$temporary_dir/stderr")" || status=$?
  errors="$(cat "$temporary_dir/stderr")"
}

fail() {
  echo "FAIL ${case_name}: $*"
  echo "  exit ${status}; stdout:"
  printf '    %s\n' "$output"
  echo "  stderr:"
  printf '    %s\n' "$errors"
  failures=$((failures + 1))
}

# expect <list>: the case printed exactly these lines and exited 0.
expect() {
  local want
  want="$(printf '%s\n' "$@")"
  if [[ $status != 0 ]]; then
    fail "expected exit 0 and $(printf '%s' "$want" | tr '\n' ' ')"
  elif [[ $output != "$want" ]]; then
    fail "expected $(printf '%s' "$want" | tr '\n' ' ')"
  else
    echo "ok ${case_name}"
  fi
}

case_name='main prints B, then A, whose id is earlier: the order main added them'
run origin/main
expect "$B" "$A"

case_name="main's commit before A prints B alone, though main holds A now"
run origin/main~1
expect "$B"

case_name='a branch that adds C and forward-merges main prints B, A, C'
run origin/feature
expect "$B" "$A" "$C"

case_name="a merge group's line that adds D, then C with an earlier id, prints B, A, D, C"
run origin/merge-group
expect "$B" "$A" "$D" "$C"

case_name='two entries one commit adds print in id order'
run origin/two-in-one
expect "$B" "$A" "$Y" "$Z"

case_name='--stamp prints the list in the stamp form'
run --stamp origin/merge-group
expect "v1:$B,$A,$D,$C"

case_name='--stamp of a commit of no entry prints v1: alone'
run --stamp "$root"
expect 'v1:'

# The clone's checkout is main, which holds B and A. Every path the script reads is the
# repository's, so the directory it runs from changes nothing it prints.
case_name='--stamp HEAD run from packages/daemon prints the list the root prints'
run_in "$clone/packages/daemon" --stamp HEAD
expect "v1:$B,$A"

# With the branch named as main, its own first-parent line is the history read: C, which the
# branch added, comes before A, which its merge of main added.
case_name='--main names the ref whose first-parent line is read as main'
run --main refs/remotes/origin/feature origin/feature
expect "$B" "$C" "$A"

# A clone of main whose merge group line is fetched one commit deep: it holds the group's newest
# commit, which adds C, and lacks the one before it, which adds D.
case_name="a clone that holds main's history and not the merge group's fails, naming the commit it lacks"
readonly lacking="${temporary_dir}/lacking"
git clone -q --no-local --single-branch --branch main "$origin" "$lacking"
git -C "$lacking" fetch -q --depth=1 origin refs/heads/merge-group:refs/remotes/origin/merge-group
lacked=$(git -C "$origin" rev-parse merge-group~1)
run_in "$lacking" origin/merge-group
if [[ $status == 0 ]]; then
  fail 'expected a failure'
elif [[ $errors != *"$lacked"* ]]; then
  fail "expected the error to name ${lacked}"
else
  echo "ok ${case_name}"
fi

if [[ $failures -gt 0 ]]; then
  echo "FAIL daemon-contract-order.test.sh: ${failures} case(s) failed"
  exit 1
fi
echo 'ok daemon-contract-order.test.sh: every case passed'
