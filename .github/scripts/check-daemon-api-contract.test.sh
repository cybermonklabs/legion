#!/usr/bin/env bash
# Tests for `check-daemon-api-contract.sh`: a change that touches the daemon API contract's
# history or declaration declares a number above the one its base carries, the refusal names a
# free number and every file that holds it, the files that hold the number agree on it, an
# edit of the doc comment above the history touches no contract, a declared number needs its own
# `// <number>:` history entry, and a base, a tree or a file the check cannot read refuses the
# change.
#
# Each case builds a miniature repository under a temporary directory, symlinks the real script
# into it, and puts a stub `gh` first on PATH, so the script under test is the file CI runs and
# the test needs no network. The stub serves every GitHub read from files and counts its reads of
# the open pull requests, which the check makes only when it is about to fail. A case whose check
# reads what this tree was built on makes that checkout with git's own plumbing (built_on), since
# the check reads HEAD's parent line out of a real commit object.
#
# The numbers here are the fixtures' own: no contract number of this repository is written into
# this file, so a bump of the real declaration changes nothing in these cases.
#
# Run from anywhere: .github/scripts/check-daemon-api-contract.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-daemon-api-contract.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The interpreter python3 names, asked once in this harness's own environment. Each fixture's bin
# directory, first on the PATH a case runs the script under, holds a link to it as python3, so the
# script's `python3` is this interpreter and the gh its Python runs is the stub beside that link.
# The first python3 on the caller's PATH can be a version manager's shim that starts the
# interpreter with a PATH of its own: mise 2026.10.11's python3 shim, in a mise-activated
# environment, put that environment's PATH in front of the entries the caller had put first, so
# the script's Python ran that environment's gh, against GitHub, and not the stub.
# disk_watch.test.sh links its interpreter the same way.
if ! python=$(python3 -I -c 'import sys; print(sys.executable)'); then
  echo "could not resolve the python3 interpreter" >&2
  exit 1
fi
if [[ "$python" != /* || ! -x "$python" ]]; then
  echo "python3 did not report an absolute executable path: $python" >&2
  exit 1
fi

# declaration <number> [<extra comment>]: the text of a version.go declaring <number>, laid out as
# the real one is: a doc comment, then the history, here one entry at <number>, then the `const`
# line. The extra comment goes under that entry, which is how a case makes a tree that touches
# the contract's history without moving the number.
declaration() {
  printf 'package api\n\n'
  printf '// DaemonAPIVersion is the contract this daemon speaks with the plugin in its pods.\n'
  printf '//\n'
  printf '// %s: the shapes the plugin parses at this number.\n' "$1"
  if [ -n "${2:-}" ]; then
    printf '// %s\n' "$2"
  fi
  printf 'const DaemonAPIVersion = %s\n' "$1"
}

# The files beside version.go that hold the contract number, in the order the check names
# them.
other_sites=(
  packages/contracts/fixtures/daemon-api/version.json
  packages/contracts/fixtures/daemon-api/state.json
  packages/pi-legion/package.json
  packages/claude-envoy/package.json
)

# holding <site> <number>: the least text of one of other_sites that holds <number> where the
# check reads it.
holding() {
  case "$1" in
    */version.json) printf '{"daemonApiVersion": %s}\n' "$2" ;;
    */state.json) printf '{"contract": {"daemon": %s, "caller": %s}}\n' "$2" "$2" ;;
    */package.json) printf '{"legion": {"daemonApiVersion": %s}}\n' "$2" ;;
    *) echo "holding: no shape for $1" >&2; return 1 ;;
  esac
}

# sites <root> <number>: writes each of other_sites into the fixture, holding <number>. The check
# holds them to the number version.go declares before it compares that number with the base's,
# so every case that gets past reading the tree's own number writes them.
sites() {
  local site
  for site in "${other_sites[@]}"; do
    mkdir -p "$(dirname "$1/$site")"
    holding "$site" "$2" > "$1/$site"
  done
}

# fixture <name>: an empty repository with the real script symlinked in, a stub gh first on PATH,
# and the stub's own directory. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/daemon/internal/api" "$root/stub/refs" "$root/bin"
  ln -s "$check_script" "$root/.github/scripts/check-daemon-api-contract.sh"
  ln -s "$python" "$root/bin/python3"
  : > "$root/stub/pr-queries"
  cat > "$root/bin/gh" <<'SH'
#!/usr/bin/env bash
# The GitHub reads the check makes, served out of $STUB_DIR: refs/<ref, slashes as underscores>
# holds that ref's version.go, and open-prs.json is the one answer to the open-pull-request
# query, every read of which this stub counts a line of pr-queries. A ref with no file is a read
# that fails, which is how the cases that cannot read a base build it.
set -euo pipefail
[ "${1:-}" = "api" ] || { echo "stub gh: not an api call: $*" >&2; exit 2; }
shift
if [ "${1:-}" = "graphql" ]; then
  echo "read" >> "$STUB_DIR/pr-queries"
  if [ -f "$STUB_DIR/open-prs.json" ]; then
    cat "$STUB_DIR/open-prs.json"
    exit 0
  fi
  echo "stub gh: this case set no open pull request answer" >&2
  exit 1
fi
endpoint=${*: -1}
case "$endpoint" in
  *\?ref=*) ;;
  *) echo "stub gh: unexpected endpoint: $endpoint" >&2; exit 2 ;;
esac
ref=${endpoint##*\?ref=}
ref=${ref//%2F/\/}
file="$STUB_DIR/refs/${ref//\//_}"
if [ ! -f "$file" ]; then
  echo "gh: Not Found (HTTP 404)" >&2
  exit 1
fi
cat "$file"
SH
  chmod +x "$root/bin/gh"
  echo "$root"
}

# built_on <root> <contract number>: makes the checkout what GitHub checks a pull request out as
# — the merge ref it built when the event fired, a merge commit whose first parent is the commit
# that ref was built on — and serves that parent's declaration, declaring <contract number>, from
# the stub. Echoes the parent's commit id, which is what the check names in what it says.
#
# Built with git's plumbing, so a case needs no working tree of its own: every commit here carries
# the empty tree, and what the check reads out of the checkout it reads off the disk. CI checks
# out one commit, which hides a commit's parents from `git rev-parse HEAD^1`; the check reads
# HEAD's own object, where the parent lines stand whatever the checkout's depth.
built_on() {
  local root=$1 tree parent head merge
  local -a as=(-c user.name=contract-test -c user.email=contract-test@example.invalid)
  git -C "$root" -c init.defaultBranch=main init -q
  tree=$(git -C "$root" hash-object -w -t tree /dev/null)
  parent=$(git -C "$root" "${as[@]}" commit-tree "$tree" -m 'the commit the merge ref was built on')
  head=$(git -C "$root" "${as[@]}" commit-tree "$tree" -p "$parent" -m "the pull request's head")
  merge=$(git -C "$root" "${as[@]}" commit-tree "$tree" -p "$parent" -p "$head" -m 'the merge ref')
  git -C "$root" update-ref HEAD "$merge"
  declaration "$2" > "$root/stub/refs/$parent"
  echo "$parent"
}

# no_merge_ref <root>: a checkout that is no merge ref at all — HEAD a commit with no parent,
# which is what a job that checks out a branch tip rather than the merge ref GitHub built has.
no_merge_ref() {
  local root=$1 tree
  local -a as=(-c user.name=contract-test -c user.email=contract-test@example.invalid)
  git -C "$root" -c init.defaultBranch=main init -q
  tree=$(git -C "$root" hash-object -w -t tree /dev/null)
  git -C "$root" update-ref HEAD \
    "$(git -C "$root" "${as[@]}" commit-tree "$tree" -m 'a checkout that is no merge ref')"
}

# no_open_pull_requests <root>: the repository has none, so the free number is the base's plus one.
no_open_pull_requests() {
  printf '{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}\n' \
    > "$1/stub/open-prs.json"
}

# open_pull_request_declaring <root> <pull number> <contract number>: one open draft that changes
# the declaration and declares <contract number> at its head. A draft, because a draft holds the
# number it declares as firmly as any other open pull request.
open_pull_request_declaring() {
  cat > "$1/stub/open-prs.json" <<JSON
{"data":{"repository":{"pullRequests":{
  "pageInfo":{"hasNextPage":false,"endCursor":null},
  "nodes":[{"number":$2,"isDraft":true,"headRefOid":"1111111111111111111111111111111111111111",
    "files":{"pageInfo":{"hasNextPage":false},
      "nodes":[{"path":"packages/daemon/internal/api/version.go"}]}}]}}}}
JSON
  declaration "$3" > "$1/stub/refs/refs_pull_$2_head"
}

# pull_request_event <root> <base branch>: the payload a pull request run carries.
pull_request_event() {
  printf '{"pull_request":{"number":1,"base":{"ref":"%s"}}}\n' "$2" > "$1/event.json"
}

# merge_group_event <root> <base sha>: the payload a merge group run carries.
merge_group_event() {
  printf '{"merge_group":{"base_sha":"%s"}}\n' "$2" > "$1/event.json"
}

# run_check <root> <event name>: runs the real script over the fixture, with the stub gh first on
# PATH. Sets out, status, and queries — the stub's count of its open-pull-request reads.
run_check() {
  local root=$1
  set +e
  out=$(PATH="$root/bin:$PATH" STUB_DIR="$root/stub" GITHUB_EVENT_NAME="$2" \
    GITHUB_EVENT_PATH="$root/event.json" GITHUB_REPOSITORY="example-owner/example-repo" \
    "$root/.github/scripts/check-daemon-api-contract.sh" 2>&1)
  status=$?
  set -e
  queries=$(wc -l < "$root/stub/pr-queries" | tr -d ' ')
}

all_files="packages/daemon/internal/api/version.go$(printf ', %s' "${other_sites[@]}")"

# How a refusal for files that disagree says to write the number: the golden tests write the
# fixtures, and the manifests take it by hand. A pattern for contains, so `.` stands for the
# backquote.
update_command="go test ./internal/api -run '^(TestDaemonAPIVersionGolden|TestStateGolden)\$' -update., run in packages/daemon, writes the fixtures, and the manifests take the number in legion.daemonApiVersion"

echo "case: a tree equal to its base that touches the declaration"
root=$(fixture equal)
built_on "$root" 7 > /dev/null
declaration 7 > "$root/stub/refs/main"
declaration 7 "LEGION-1: a shape this change adds, on the number the base already carries" \
  > "$root/packages/daemon/internal/api/version.go"
sites "$root" 7
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 1" "$(is "$status" 1)"
check "names the declaration and its line" \
  "$(contains "$out" '^::error file=packages/daemon/internal/api/version.go::packages/daemon/internal/api/version.go:7:')"
check "names both numbers and the base it read" \
  "$(contains "$out" 'declares daemon API contract 7, which is not above the 7 the base carries (the tip of main)')"
check "names the free number" \
  "$(contains "$out" 'Take 8, the lowest number above the base.s that no open pull request declares,')"
check "names every file that holds the number" "$(contains "$out" "$all_files")"
check "reads the open pull requests once" "$(is "$queries" 1)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a tree below its base that touches the declaration"
root=$(fixture below)
built_on "$root" 9 > /dev/null
declaration 9 > "$root/stub/refs/main"
declaration 8 > "$root/packages/daemon/internal/api/version.go"
sites "$root" 8
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 1" "$(is "$status" 1)"
check "names both numbers" \
  "$(contains "$out" 'declares daemon API contract 8, which is not above the 9 the base carries')"
check "names the free number above the base, not above the tree" "$(contains "$out" 'Take 10,')"
check "names every file that holds the number" "$(contains "$out" "$all_files")"
check "reads the open pull requests once" "$(is "$queries" 1)"

echo "case: a tree above its base, the merge group of two entries that both picked that number"
root=$(fixture above)
base_sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
declaration 7 > "$root/stub/refs/$base_sha"
# The group's tree carries both entries' changes at 8: the declaration one of them bumped, and
# the other's edit beside it. The queue tests this tree, which is the pair that ships. Every
# file that holds the number holds 8, so they all agree.
declaration 8 > "$root/packages/daemon/internal/api/version.go"
sites "$root" 8
merge_group_event "$root" "$base_sha"
run_check "$root" merge_group
check "exits 0" "$(is "$status" 0)"
check "says what it covered, naming the group's own base" \
  "$(contains "$out" "daemon API contract 8 is above the 7 the base carries (the commit the queue built this group on ($base_sha))")"
check "reads no open pull request" "$(is "$queries" 0)"

echo "case: a tree that does not touch the declaration, whatever its base carries"
for carried in 7 99; do
  root=$(fixture "untouched-$carried")
  base=$(built_on "$root" "$carried")
  declaration "$carried" > "$root/stub/refs/main"
  declaration "$carried" > "$root/packages/daemon/internal/api/version.go"
  sites "$root" "$carried"
  pull_request_event "$root" main
  run_check "$root" pull_request
  check "a base at $carried: exits 0" "$(is "$status" 0)"
  check "a base at $carried: says it declares none of its own" \
    "$(contains "$out" "leaves the history and the declaration in packages/daemon/internal/api/version.go as the commit GitHub built this pull request's merge ref on ($base) has them (contract $carried), so it declares no daemon API contract of its own")"
  check "a base at $carried: reads no open pull request" "$(is "$queries" 0)"
done

echo "case: a tree equal to the commit it was built on, the base branch's tip moved on since"
root=$(fixture moved-on)
# GitHub built this merge ref when the event fired, on a commit carrying 7, and a re-run reuses
# that same ref: the tip of the base branch carrying 8 since says nothing about what this tree
# was built on. The change leaves the declaration alone, so it declares no contract of its own.
base=$(built_on "$root" 7)
declaration 8 > "$root/stub/refs/main"
declaration 7 > "$root/packages/daemon/internal/api/version.go"
sites "$root" 7
# The answer is there to be read, so a run that reads it is a run that was about to refuse.
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 0" "$(is "$status" 0)"
check "says it declares none of its own, naming the commit it was built on" \
  "$(contains "$out" "leaves the history and the declaration in packages/daemon/internal/api/version.go as the commit GitHub built this pull request's merge ref on ($base) has them (contract 7), so it declares no daemon API contract of its own")"
check "takes no number for a file it leaves alone" "$(is "$(contains "$out" 'Take ')" false)"
check "reads no open pull request" "$(is "$queries" 0)"

echo "case: a base the check cannot read"
root=$(fixture no-base)
declaration 7 > "$root/packages/daemon/internal/api/version.go"
merge_group_event "$root" bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
run_check "$root" merge_group
check "exits 1" "$(is "$status" 1)"
check "names the read that failed" \
  "$(contains "$out" 'could not read packages/daemon/internal/api/version.go on the base, the commit the queue built this group on (bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb): gh api exited 1')"
check "does not pass for want of a reading" "$(is "$(contains "$out" 'is above')" false)"
# The base's own read is the one that failed, so there is no number to search above.
check "reads no open pull request" "$(is "$queries" 0)"

echo "case: a tree the check cannot read"
root=$(fixture no-tree)
declaration 7 > "$root/stub/refs/main"
printf 'package api\n\n// the declaration this file is for is gone\n' \
  > "$root/packages/daemon/internal/api/version.go"
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 1" "$(is "$status" 1)"
check "names the read that failed" \
  "$(contains "$out" 'declares no .const DaemonAPIVersion = <number>. line')"
check "names the free number all the same" "$(contains "$out" 'Take 8,')"
check "reads the open pull requests once" "$(is "$queries" 1)"

root=$(fixture no-tree-file)
declaration 7 > "$root/stub/refs/main"
# This fixture writes no version.go at all, so the checkout holds no declaration to read.
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "a missing declaration file exits 1" "$(is "$status" 1)"
check "names it as missing" \
  "$(contains "$out" 'packages/daemon/internal/api/version.go is missing from this checkout')"
check "reads the open pull requests once" "$(is "$queries" 1)"
# The refusal is raised from the handler of the read's own error, which must reach the log as the
# one annotation line and nothing else.
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: the free number passes over the number an open pull request holds"
root=$(fixture held)
built_on "$root" 7 > /dev/null
declaration 7 > "$root/stub/refs/main"
declaration 7 "LEGION-2: another shape on the number the base carries" \
  > "$root/packages/daemon/internal/api/version.go"
sites "$root" 7
open_pull_request_declaring "$root" 11 8
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 1" "$(is "$status" 1)"
check "names the base's number plus two" "$(contains "$out" 'Take 9, the lowest number above')"
check "names the pull request holding the number it passed over" \
  "$(contains "$out" '(8 is declared by #11)')"
check "reads the open pull requests once" "$(is "$queries" 1)"

echo "case: a file list GitHub cut short is read rather than guessed at"
root=$(fixture cut-short)
built_on "$root" 7 > /dev/null
declaration 7 > "$root/stub/refs/main"
declaration 7 "LEGION-3: a shape that needs a number" \
  > "$root/packages/daemon/internal/api/version.go"
sites "$root" 7
# #12 changes more than 100 files, so the list GitHub answers with says nothing about whether
# the declaration is among them. Its head declares 8, which the free number must pass over.
cat > "$root/stub/open-prs.json" <<'JSON'
{"data":{"repository":{"pullRequests":{
  "pageInfo":{"hasNextPage":false,"endCursor":null},
  "nodes":[{"number":12,"isDraft":false,"headRefOid":"2222222222222222222222222222222222222222",
    "files":{"pageInfo":{"hasNextPage":true},"nodes":[{"path":"README.md"}]}}]}}}}
JSON
declaration 8 > "$root/stub/refs/refs_pull_12_head"
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 1" "$(is "$status" 1)"
check "counts the number that pull request declares" \
  "$(contains "$out" 'Take 9, the lowest number above')"
check "names it as the holder it passed over" "$(contains "$out" '(8 is declared by #12)')"
check "reads the open pull requests once" "$(is "$queries" 1)"

echo "case: a pull request's own declaration the check cannot read"
root=$(fixture unreadable-head)
built_on "$root" 7 > /dev/null
declaration 7 > "$root/stub/refs/main"
declaration 7 "LEGION-4: a shape that needs a number" \
  > "$root/packages/daemon/internal/api/version.go"
sites "$root" 7
open_pull_request_declaring "$root" 13 8
rm "$root/stub/refs/refs_pull_13_head"
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 1" "$(is "$status" 1)"
check "names the read that failed" \
  "$(contains "$out" 'could not read packages/daemon/internal/api/version.go at refs/pull/13/head (#13, head 1111111111111111111111111111111111111111)')"
check "names no free number it could not prove" "$(is "$(contains "$out" 'Take ')" false)"

echo "case: the commit this checkout was built on, which the check cannot read"
root=$(fixture no-built-on)
base=$(built_on "$root" 7)
# The declaration at that commit is what says whether this change touched the file, so a read of
# it that fails refuses the change rather than guessing either way.
rm "$root/stub/refs/$base"
declaration 7 > "$root/stub/refs/main"
declaration 7 > "$root/packages/daemon/internal/api/version.go"
sites "$root" 7
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 1" "$(is "$status" 1)"
check "names the read that failed" \
  "$(contains "$out" "could not read packages/daemon/internal/api/version.go on the commit GitHub built this pull request's merge ref on ($base): gh api exited 1")"
check "does not pass for want of a reading" \
  "$(is "$(contains "$out" 'declares no daemon API contract of its own')" false)"
check "reads no open pull request" "$(is "$queries" 0)"

root=$(fixture no-parent)
# A checkout that is no merge ref — a branch tip, or a tree with no history at all — carries no
# commit this change was built on, so the check reads none and refuses rather than comparing
# with a tip that has moved on.
no_merge_ref "$root"
declaration 7 > "$root/stub/refs/main"
declaration 7 > "$root/packages/daemon/internal/api/version.go"
sites "$root" 7
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "a checkout that is no merge ref exits 1" "$(is "$status" 1)"
check "names what it could not read" \
  "$(contains "$out" "could not read the commit GitHub built this pull request's merge ref on: HEAD names no parent")"
check "reads no open pull request" "$(is "$queries" 0)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

# The cases below lock the agreement of those files and the comparison of the history alone.
# Each serves an answer to the open pull request query, so a refusal that read the open pull
# requests would be counted rather than fail.

echo "case: a bump that leaves state.json at the number it bumps from"
root=$(fixture bump-forgets-state)
# INFRA-3621's bump: version.go and three of the other files at 8, and state.json still at 7.
# The number is above the base's, and the check refuses it all the same, before it passes one.
declaration 7 > "$root/stub/refs/main"
declaration 8 > "$root/packages/daemon/internal/api/version.go"
sites "$root" 8
holding packages/contracts/fixtures/daemon-api/state.json 7 \
  > "$root/packages/contracts/fixtures/daemon-api/state.json"
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 1" "$(is "$status" 1)"
check "names state.json as the file" \
  "$(contains "$out" '^::error file=packages/contracts/fixtures/daemon-api/state.json::')"
check "names both numbers it holds and the number version.go declares" \
  "$(contains "$out" 'packages/contracts/fixtures/daemon-api/state.json holds daemon API contract 7 at contract.daemon and 7 at contract.caller, and packages/daemon/internal/api/version.go declares 8\.')"
check "names every file that holds the number" "$(contains "$out" "$all_files")"
check "says how to write the number" "$(contains "$out" "$update_command")"
check "names no free number" "$(is "$(contains "$out" 'Take ')" false)"
check "does not pass the number above the base's" "$(is "$(contains "$out" 'is above')" false)"
check "reads no open pull request" "$(is "$queries" 0)"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a state.json whose caller's number alone is behind"
root=$(fixture state-caller-behind)
declaration 7 > "$root/stub/refs/main"
declaration 8 > "$root/packages/daemon/internal/api/version.go"
sites "$root" 8
printf '{"contract": {"daemon": 8, "caller": 7}}\n' \
  > "$root/packages/contracts/fixtures/daemon-api/state.json"
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 1" "$(is "$status" 1)"
check "names state.json and both numbers it holds" \
  "$(contains "$out" '^::error file=packages/contracts/fixtures/daemon-api/state.json::packages/contracts/fixtures/daemon-api/state.json holds daemon API contract 8 at contract.daemon and 7 at contract.caller, and packages/daemon/internal/api/version.go declares 8\.')"
check "names every file that holds the number" "$(contains "$out" "$all_files")"
check "reads no open pull request" "$(is "$queries" 0)"

echo "case: each other file one number behind version.go"
for site in "${other_sites[@]}"; do
  # state.json holds the number twice, and has cases of its own above.
  case "$site" in */state.json) continue ;; esac
  root=$(fixture "behind-${site//\//-}")
  declaration 7 > "$root/stub/refs/main"
  declaration 8 > "$root/packages/daemon/internal/api/version.go"
  sites "$root" 8
  holding "$site" 7 > "$root/$site"
  no_open_pull_requests "$root"
  pull_request_event "$root" main
  run_check "$root" pull_request
  check "$site at 7: exits 1" "$(is "$status" 1)"
  check "$site at 7: names that file, the number it holds and the number version.go declares" \
    "$(contains "$out" "^::error file=$site::$site holds daemon API contract 7 at [a-zA-Z.]*, and packages/daemon/internal/api/version.go declares 8\.")"
  check "$site at 7: names every file that holds the number" "$(contains "$out" "$all_files")"
  check "$site at 7: says how to write the number" "$(contains "$out" "$update_command")"
  check "$site at 7: reads no open pull request" "$(is "$queries" 0)"
done

# unreadable <name> <site> <text> <reason>: a bump from 7 to 8 whose <site> holds <text>, or is
# missing where <text> is empty, which the check refuses as a read that failed for <reason>.
unreadable() {
  local root
  root=$(fixture "unreadable-site-$1")
  declaration 7 > "$root/stub/refs/main"
  declaration 8 > "$root/packages/daemon/internal/api/version.go"
  sites "$root" 8
  if [ -n "$3" ]; then
    printf '%s\n' "$3" > "$root/$2"
  else
    rm "$root/$2"
  fi
  no_open_pull_requests "$root"
  pull_request_event "$root" main
  run_check "$root" pull_request
  check "$1: exits 1" "$(is "$status" 1)"
  check "$1: names the read that failed" \
    "$(contains "$out" "^::error file=$2::could not read $2 in this checkout: $4")"
  check "$1: does not pass for want of a reading" "$(is "$(contains "$out" 'is above')" false)"
  check "$1: reads no open pull request" "$(is "$queries" 0)"
  check "$1: no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
}

echo "case: a file beside version.go that the check cannot read"
unreadable missing packages/pi-legion/package.json '' 'it is missing$'
unreadable not-json packages/claude-envoy/package.json '{"legion": {"daemonApiVersion": 8' \
  'it is not JSON ('
unreadable no-field packages/contracts/fixtures/daemon-api/version.json '{"contractVersion": 8}' \
  'it holds no daemonApiVersion$'
unreadable string packages/contracts/fixtures/daemon-api/version.json '{"daemonApiVersion": "8"}' \
  'its daemonApiVersion is "8", not an integer$'
# JSON's true is a Python bool, which Python counts an int.
unreadable true packages/contracts/fixtures/daemon-api/state.json \
  '{"contract": {"daemon": true, "caller": 8}}' 'its contract.daemon is true, not an integer$'
# 8.0 equals 8 in Python, so a check that took it would pass a file that holds no integer.
unreadable float packages/pi-legion/package.json '{"legion": {"daemonApiVersion": 8.0}}' \
  'its legion.daemonApiVersion is 8.0, not an integer$'

echo "case: an edit of the doc comment above the history, the number left alone"
root=$(fixture doc-comment)
# The header above the first history entry holds no number, so an edit of it alone owes no bump,
# which would relaunch every registered claim onto a plugin that changed nothing.
base=$(built_on "$root" 7)
declaration 7 > "$root/stub/refs/main"
text=$(declaration 7)
printf '%s\n' "${text/in its pods./in its pods. Its bump rule names the files that hold it.}" \
  > "$root/packages/daemon/internal/api/version.go"
sites "$root" 7
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "its version.go is not the one it was built on" \
  "$(is "$(cmp -s "$root/packages/daemon/internal/api/version.go" "$root/stub/refs/$base" && echo same || echo differs)" differs)"
check "exits 0" "$(is "$status" 0)"
check "says it declares none of its own, naming the commit it was built on" \
  "$(contains "$out" "leaves the history and the declaration in packages/daemon/internal/api/version.go as the commit GitHub built this pull request's merge ref on ($base) has them (contract 7), so it declares no daemon API contract of its own")"
check "takes no number for an edit above the history" "$(is "$(contains "$out" 'Take ')" false)"
check "reads no open pull request" "$(is "$queries" 0)"

echo "case: an edit of a history entry, the number left alone"
root=$(fixture history-entry)
built_on "$root" 7 > /dev/null
declaration 7 > "$root/stub/refs/main"
text=$(declaration 7)
printf '%s\n' "${text/the shapes the plugin parses/the shapes and the flags the plugin parses}" \
  > "$root/packages/daemon/internal/api/version.go"
sites "$root" 7
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 1" "$(is "$status" 1)"
check "names the declaration and its line" \
  "$(contains "$out" '^::error file=packages/daemon/internal/api/version.go::packages/daemon/internal/api/version.go:6:')"
check "names both numbers" \
  "$(contains "$out" 'declares daemon API contract 7, which is not above the 7 the base carries (the tip of main)')"
check "names the free number" "$(contains "$out" 'Take 8, the lowest number above')"
check "names every file that holds the number" "$(contains "$out" "$all_files")"
check "reads the open pull requests once" "$(is "$queries" 1)"

echo "case: a declaration bump with no history entry at its number"
root=$(fixture const-without-history)
built_on "$root" 7 > /dev/null
declaration 7 > "$root/stub/refs/main"
declaration 7 > "$root/packages/daemon/internal/api/version.go"
sed -i 's/^const DaemonAPIVersion = 7$/const DaemonAPIVersion = 8/' \
  "$root/packages/daemon/internal/api/version.go"
sites "$root" 8
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "exits 1" "$(is "$status" 1)"
# shellcheck disable=SC2016 # the backticks are the refusal's own text, not a substitution
check "names the missing history entry" \
  "$(contains "$out" 'declares daemon API contract 8, but its history has no `// 8:` entry')"
check "names the free number" "$(contains "$out" 'Take 8, the lowest number above')"
check "reads the open pull requests once" "$(is "$queries" 1)"

echo "case: a declaration bump whose only line at its number is an entry's wrapped prose"
root=$(fixture const-with-prose-number)
built_on "$root" 7 > /dev/null
declaration 7 > "$root/stub/refs/main"
declaration 7 "a reading taken between 07:58Z and" > "$root/packages/daemon/internal/api/version.go"
sed -i -e 's/^const DaemonAPIVersion = 7$/const DaemonAPIVersion = 8/' \
  -e 's|^// a reading taken between 07:58Z and$|// a reading taken between 07:58Z and\n// 8:04Z, which the bump cites|' \
  "$root/packages/daemon/internal/api/version.go"
sites "$root" 8
no_open_pull_requests "$root"
pull_request_event "$root" main
run_check "$root" pull_request
check "the miniature file has the wrapped line" \
  "$(contains "$(cat "$root/packages/daemon/internal/api/version.go")" '^// 8:04Z')"
check "exits 1" "$(is "$status" 1)"
# shellcheck disable=SC2016 # the backticks are the refusal's own text, not a substitution
check "names the missing history entry" \
  "$(contains "$out" 'declares daemon API contract 8, but its history has no `// 8:` entry')"

echo "case: a python3 after the stub that starts the interpreter with a PATH of its own"
# A stricter form of what mise's shim did (the comment on $python above): this python3 starts the
# interpreter with the PATH this harness had and leaves the fixture's bin directory out, where mise
# put it behind its environment's own entries. So a script whose `python3` were this one would read
# the base with this harness's own gh, or with none, and never through the stub, whether or not
# this harness's PATH holds a gh. The fixture's link comes first, so the script never meets it. The
# two checks before the case show the resetting python3 does what the row needs, so the row can
# fail.
# The case is "a tree above its base" above, in a merge group, whose one GitHub read is the base.
resetting=$work/resetting-python
mkdir -p "$resetting/probe"
printf '%s' "$PATH" > "$resetting/path"
# sh_word <text>: <text> as one word that any POSIX sh reads back whole, for every byte but NUL:
# in single quotes, each ' in it written '\''. Each path goes into the stand-in so, as TMPDIR may
# hold a space, a quote or a newline. Not bash's printf %q: for a word holding a newline it writes
# $'…\n…', which the stand-in's /bin/sh (dash 0.5.12) reads as a $ and a backslash-n, so its cat
# reads no file.
sh_word() {
  local escaped="'\\''"
  printf "'%s'" "${1//\'/"$escaped"}"
}
quoted_path=$(sh_word "$resetting/path")
quoted_python=$(sh_word "$python")
cat > "$resetting/python3" <<EOF
#!/bin/sh
PATH=\$(cat $quoted_path) exec $quoted_python "\$@"
EOF
printf '#!/bin/sh\nexit 0\n' > "$resetting/probe/gh"
chmod +x "$resetting/python3" "$resetting/probe/gh"
check "without a link of its own, a PATH's python3 is the resetting one" \
  "$(is "$(PATH="$resetting/probe:$resetting:$PATH" command -v python3)" "$resetting/python3")"
check "  and the interpreter it starts does not find the gh before it" \
  "$(is "$(PATH="$resetting/probe:$resetting:$PATH" python3 -c 'import shutil; print(shutil.which("gh"))')" \
    "$(command -v gh || echo None)")"
root=$(fixture after-a-resetting-python3)
base_sha=cccccccccccccccccccccccccccccccccccccccc
declaration 7 > "$root/stub/refs/$base_sha"
declaration 8 > "$root/packages/daemon/internal/api/version.go"
sites "$root" 8
merge_group_event "$root" "$base_sha"
set +e
out=$(PATH="$root/bin:$resetting:$PATH" STUB_DIR="$root/stub" GITHUB_EVENT_NAME=merge_group \
  GITHUB_EVENT_PATH="$root/event.json" GITHUB_REPOSITORY="example-owner/example-repo" \
  "$root/.github/scripts/check-daemon-api-contract.sh" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "reads the base through the stub" \
  "$(contains "$out" "daemon API contract 8 is above the 7 the base carries (the commit the queue built this group on ($base_sha))")"

summary "check-daemon-api-contract.sh"
