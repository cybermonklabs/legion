#!/usr/bin/env bash
# Tests for `fetch-depth-if-shallow.sh`: a fetch with a depth deepens a shallow checkout, as
# actions/checkout leaves a run's tree, and leaves a whole clone whole.
#
# Each case clones a fixture origin over file://, so git honours --depth: one commit deep for
# CI's checkout, whole for a developer's clone. The origin then gets commits the clone lacks, and
# the case fetches the newest through the script. A control runs the same fetch with plain git in
# a whole clone and sees the clone made shallow, so the whole-clone cases can see the cut they
# test for. Other cases give the script a first argument that is no depth option, a directory that
# is no repository, and a git whose shallow answer is neither true nor false.
#
# Run from anywhere: .github/scripts/fetch-depth-if-shallow.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

# The fixtures are judged on git's defaults wherever the harness runs, so every git variable the
# environment sets goes before any git runs, and every git runs with no configuration file of the
# machine or of the user (GIT_CONFIG_GLOBAL, GIT_CONFIG_NOSYSTEM): a Legion pod sets configuration
# through GIT_CONFIG_COUNT, and GIT_DIR would point every case at another repository.
while read -r name; do
  unset "$name"
done < <(compgen -e GIT_)
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$script_dir/fetch-depth-if-shallow.sh"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# No case may find a repository above its directory: a directory that is no repository has to
# read as none, not as the checkout this harness runs in.
export GIT_CEILING_DIRECTORIES=$work

# git_fixture <args>: git with an identity for the fixture's commits.
git_fixture() { git -c user.name='CI test' -c user.email=ci-test@example.com "$@"; }

# The origin: 10 commits on main. GitHub serves any commit reachable from a ref by its sha; a bare
# repository serves only its tips unless told otherwise.
origin="$work/origin.git"
git_fixture init -q --bare -b main "$origin"
git_fixture -C "$origin" config uploadpack.allowReachableSHA1InWant true
seed="$work/seed"
git_fixture init -q -b main "$seed"
# add_commits <count>: <count> more commits on the seed's main, pushed to the origin.
add_commits() {
  local i
  for ((i = 1; i <= $1; i++)); do
    git_fixture -C "$seed" commit -q --allow-empty -m "commit $i of $1"
  done
  git_fixture -C "$seed" push -q "$origin" main
}
add_commits 10

# clone <name> [<clone option>...]: a clone of the origin's main in a directory of its own;
# prints the directory.
clone() {
  local dir="$work/$1"
  shift
  git_fixture clone -q --no-tags --single-branch --branch main "$@" "file://$origin" "$dir"
  echo "$dir"
}
whole=$(clone whole)
whole_control=$(clone whole-control)
refused=$(clone refused)
shallow=$(clone shallow --depth=1)
deepen=$(clone deepen --depth=1)
# Three commits land on the origin's main after the clones, as main moves on after a clone.
add_commits 3
tip=$(git -C "$seed" rev-parse HEAD)

# fetch <dir> <argument>...: runs the script in <dir> with those arguments; sets out (stdout and
# stderr) and status. Under `timeout`, so a fetch that hung fails its case instead of the job.
fetch() {
  local dir=$1
  shift
  set +e
  out=$(cd "$dir" && timeout 60 "$script" "$@" 2>&1)
  status=$?
  set -e
}

# holds <dir> <sha>: true when the commit is in the repository, for the check to read.
holds() { git -C "$1" cat-file -e "$2^{commit}" 2> /dev/null && echo true || echo false; }

echo "fixture: the clones lack the origin's three newest commits"
check "the whole clone holds 10 commits" "$(is "$(git -C "$whole" rev-list --count HEAD)" 10)"
check "the whole clone is not shallow" "$(is "$(git -C "$whole" rev-parse --is-shallow-repository)" false)"
check "the shallow clone is shallow" "$(is "$(git -C "$shallow" rev-parse --is-shallow-repository)" true)"
check "the whole clone lacks the origin's tip" "$(is "$(holds "$whole" "$tip")" false)"

echo "control: plain git fetch --depth=1 in a whole clone makes it shallow"
(cd "$whole_control" && git fetch -q --depth=1 --no-tags origin "$tip")
check "the clone is now shallow" \
  "$(is "$(git -C "$whole_control" rev-parse --is-shallow-repository)" true)"
check "and its history is cut to the one commit fetched, though it holds the 10 below it" \
  "$(is "$(git -C "$whole_control" rev-list --count "$tip")" 1)"

echo "case: a whole clone is fetched into with no depth and stays whole"
fetch "$whole" --depth=1 -q --no-tags origin "$tip"
check "it exits 0" "$(is "$status" 0)"
check "it prints nothing, as git fetch -q prints nothing" "$(is "$out" "")"
check "the commit is fetched" "$(holds "$whole" "$tip")"
check "the clone is still not shallow" "$(is "$(git -C "$whole" rev-parse --is-shallow-repository)" false)"
check "the commit's whole history reads: 13 commits" "$(is "$(git -C "$whole" rev-list --count "$tip")" 13)"

echo "case: a shallow checkout is fetched into with the depth"
fetch "$shallow" --depth=3 -q --no-tags origin "$tip"
check "it exits 0" "$(is "$status" 0)"
check "the commit is fetched" "$(holds "$shallow" "$tip")"
check "the checkout is still shallow" "$(is "$(git -C "$shallow" rev-parse --is-shallow-repository)" true)"
check "the commit's history is the 3 commits the depth names" "$(is "$(git -C "$shallow" rev-list --count "$tip")" 3)"

echo "case: a shallow checkout is deepened by --deepen"
deepen_head=$(git -C "$deepen" rev-parse HEAD)
fetch "$deepen" --deepen=2 -q --no-tags origin "$deepen_head"
check "it exits 0" "$(is "$status" 0)"
check "the checkout's history is its 1 commit and the 2 below it" \
  "$(is "$(git -C "$deepen" rev-list --count HEAD)" 3)"

echo "case: a fetch that git refuses exits with git's status"
fetch "$whole" --depth=1 -q --no-tags origin 1111111111111111111111111111111111111111
check "it exits 128, git's status for a commit the origin does not serve" "$(is "$status" 128)"
check "it prints git's message" "$(contains "$out" 'fatal: ')"

echo "case: a first argument that is no depth option is refused, and nothing is fetched"
fetch "$refused" -q --no-tags origin "$tip"
check "it exits 2" "$(is "$status" 2)"
check "it names what the first argument must be" \
  "$(contains "$out" "^::error file=.github/scripts/fetch-depth-if-shallow.sh::the first argument must be --depth=<n>, --deepen=<n>, --shallow-since=<date> or --shallow-exclude=<ref>, not '-q'\$")"
check "the commit is not fetched" "$(is "$(holds "$refused" "$tip")" false)"
fetch "$refused" --depth= origin "$tip"
check "a depth option with no value is refused too" "$(is "$status" 2)"
fetch "$refused"
check "and so is no argument at all" "$(is "$status" 2)"

echo "case: a directory that is no repository fails with git's status before any fetch"
mkdir "$work/none"
fetch "$work/none" --depth=1 origin "$tip"
check "it exits 128, the status of git rev-parse outside a repository" "$(is "$status" 128)"
check "it prints git's message" "$(contains "$out" 'not a git repository')"

echo "case: a shallow answer that is neither true nor false is refused, and nothing is fetched"
# A git older than 2.15 has no --is-shallow-repository, and git rev-parse prints an option it
# does not know as it was given. The stand-in git answers so, and records any fetch it is asked.
mkdir "$work/old-git"
cat > "$work/old-git/git" <<'SH'
#!/usr/bin/env bash
if [ "$1" = rev-parse ]; then
  printf '%s\n' "$2"
  exit 0
fi
echo "$*" >> "$(dirname "$0")/calls"
SH
chmod +x "$work/old-git/git"
set +e
out=$(cd "$refused" && PATH="$work/old-git:$PATH" timeout 60 "$script" --depth=1 origin "$tip" 2>&1)
status=$?
set -e
check "it exits 2" "$(is "$status" 2)"
check "it names git's answer" \
  "$(contains "$out" "^::error file=.github/scripts/fetch-depth-if-shallow.sh::git rev-parse --is-shallow-repository printed '--is-shallow-repository', not true or false, so whether a depth would make this repository shallow is unknown\$")"
check "it asks the stand-in git for no fetch" "$(is "$([ -e "$work/old-git/calls" ] && echo yes || echo no)" no)"

summary "a fetch with a depth that never makes a whole clone shallow"
