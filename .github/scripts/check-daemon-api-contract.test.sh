#!/usr/bin/env bash
# Tests for `check-daemon-api-contract.sh`: each of the six refusals of its header, with a change
# the rule passes beside each one it refuses; the two bases CI reads, a merge group's from its
# payload and a pull request's from the merge ref's first parent, each fetched without blobs into
# a checkout one commit deep, as CI's is; and a base, a head or an event the check cannot read.
#
# Each case builds a miniature repository under a temporary directory: a base commit holding two
# numbered entries, one entry with its fixture, the frozen records, the state goldens, two
# manifests and a fragment, then a head commit with the case's change. The real script is linked
# into it, untracked, so the script under test is the file CI runs and the test needs no network.
# Most cases name the base with DAEMON_API_CONTRACT_BASE; the CI cases clone the miniature over
# file:// one commit deep, as actions/checkout does, and let the check fetch the base itself.
#
# The numbers here are the fixtures' own (L is 2): no contract number of this repository is
# written into this file.
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

# Every git command here reads no configuration of the machine it runs on (a signing key, a hook
# path, a default branch), and writes commits under one name.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=contract-test GIT_AUTHOR_EMAIL=contract-test@example.invalid
export GIT_COMMITTER_NAME=contract-test GIT_COMMITTER_EMAIL=contract-test@example.invalid

# colon is the colon of a path:line that the check under test prints. It is built at run
# time so that this file's text holds no path followed by a colon and a line number, which
# ci_citations.py refuses wherever it stands under .github.
colon=':'

api=packages/daemon/internal/api
fixtures=packages/contracts/fixtures/daemon-api
entry=$api/contract_20261125010101_probe.go
fixture=$fixtures/contracts/20261125010101_probe.json
correction=$'//\n// Correction: the record named a key the daemon never served.'

# put <root> <path> <text>: writes <text> and a newline to <path> under <root>.
put() {
  mkdir -p "$(dirname "$1/$2")"
  printf '%s\n' "$3" > "$1/$2"
}

# version_go [<text under entry 1>] [<text under entry 2>]: a version.go of two numbered entries,
# laid out as the real one is: a doc comment that holds no number, then the entries, each a
# `// <n>: ` header and its prose. Entry 2's prose wraps at a time of day, which is no header.
version_go() {
  printf 'package api\n\n// The contract'"'"'s doc comment, which holds no number.\n//\n'
  printf '// 1: the first numbered entry.\n'
  if [ -n "${1:-}" ]; then printf '%s\n' "$1"; fi
  printf '//\n// 2: the second numbered entry, whose prose runs on to a time of day,\n'
  printf '// 22:48Z, which is no header.\n'
  if [ -n "${2:-}" ]; then printf '%s\n' "$2"; fi
}

# entry_go <id> <slug> [<text under its first line>]: an entry file as
# scripts/daemon-contract-entry.sh writes it.
entry_go() {
  printf 'package api\n\n// %s %s: an entry that changes no surface.\n' "$1" "$2"
  if [ -n "${3:-}" ]; then printf '%s\n' "$3"; fi
  printf 'const %sContract = %s\n\nfunc init() {\n\tregisterEntry(contractEntry{id: %sContract, slug: "%s"})\n}\n' \
    "$2" "$1" "$2" "$2"
}

# base_tree <root>: every file of the base commit.
base_tree() {
  put "$1" "$api/version.go" "$(version_go)"
  put "$1" "$entry" "$(entry_go 20261125010101 probe)"
  put "$1" "$fixture" '{"answers": []}'
  put "$1" "$fixtures/released-contracts.json" '[1, 2]'
  put "$1" "$fixtures/contract-shapes.json" \
    '[{"contract": 1, "answer": "state", "noBody": false, "keyPaths": ["a"], "domains": {}},
 {"contract": 2, "answer": "state", "noBody": false, "keyPaths": ["a", "b"], "domains": {}}]'
  put "$1" "$fixtures/request-shapes.json" '[{"contract": 2, "route": "GET /x", "fieldPaths": ["id"]}]'
  put "$1" "$fixtures/answers.json" \
    '[{"id": "state", "routes": ["GET /x"], "since": 1, "noBody": false, "typedSettings": [{"key": "k", "since": 1, "until": 2}]}]'
  put "$1" "$fixtures/enumerated-fields.json" \
    '[{"answer": "state", "keyPath": "p", "values": [{"value": "a", "since": 1}, {"value": "b", "since": 2}]}]'
  put "$1" "$fixtures/state.json" '{"settings": {}}'
  put "$1" "$fixtures/state-narrowed-repositories.json" '{"contract": {"caller": 1}}'
  put "$1" packages/pi-legion/package.json '{"name": "pi-legion", "legion": {"skills": []}}'
  # The word without the field: a pack step's script that writes it is no committed number.
  put "$1" packages/claude-envoy/package.json \
    '{"name": "claude-envoy", "scripts": {"pack": "jq .legion.daemonApiVersion=N package.json"}}'
  put "$1" packages/daemon/.changes/unreleased/base.yaml $'kind: Changed\nbody: The daemon serves contract 2.'
}

# repository <name>: a miniature repository holding the base commit, tagged `base`, with the real
# script linked in and left untracked. Echoes its root.
repository() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  git -c init.defaultBranch=main init -q "$root"
  mkdir -p "$root/.github/scripts"
  ln -s "$check_script" "$root/.github/scripts/check-daemon-api-contract.sh"
  printf '.github/\n' >> "$root/.git/info/exclude"
  base_tree "$root"
  git -C "$root" add -A
  git -C "$root" commit -q -m base
  git -C "$root" tag base
  echo "$root"
}

# commit <root> [<message>]: commits every change of the working tree as the head.
commit() {
  git -C "$1" add -A
  git -C "$1" commit -q --allow-empty -m "${2:-head}"
}

# run_check <root>: runs the real script over the miniature against the base commit. Sets out and
# status.
run_check() {
  set +e
  out=$(DAEMON_API_CONTRACT_BASE=base "$1/.github/scripts/check-daemon-api-contract.sh" 2>&1)
  status=$?
  set -e
}

# passes <root> <what>: the change passes, by the check's own line.
passes() {
  run_check "$1"
  check "$2: exits 0" "$(is "$status" 0)"
  check "$2: says it passes" "$(contains "$out" '^the daemon API contract check passes, compared with the base, DAEMON_API_CONTRACT_BASE (base): L = 2')"
  check "$2: no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
}

# refused <what>: the run that just ran refused the change, by the check's own summary line.
refused() {
  check "$1: exits 1" "$(is "$status" 1)"
  check "$1: says it refuses" "$(contains "$out" '^the daemon API contract check refuses this change')"
  check "$1: no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
}

echo "case: the base itself, a change that changes nothing"
root=$(repository nothing)
commit "$root"
passes "$root" "an empty change"
check "counts the one entry" "$(contains "$out" '1 entries with their fixtures')"

echo "rule 1: numbered entries"
root=$(repository entry-prose)
put "$root" "$api/version.go" "$(version_go $'//\n// More of the first entry, and a time 10:15Z that wraps\n// 10:16Z onto a line of its own.')"
commit "$root"
passes "$root" "prose added to a numbered entry, and a line that starts with a time of day"

root=$(repository entry-added)
put "$root" "$api/version.go" "$(version_go '' $'//\n// 3: a numbered entry added after L.')"
commit "$root"
run_check "$root"
refused "a numbered entry added"
check "names the file, the line and the header" \
  "$(contains "$out" "^::error file=packages/daemon/internal/api/version.go::packages/daemon/internal/api/version.go${colon}10: \`// 3:\` is a numbered entry that the base, DAEMON_API_CONTRACT_BASE (base), does not hold")"
check "says what to do instead" \
  "$(contains "$out" 'a contract change after them is an entry: .scripts/daemon-contract-entry.sh <slug>. writes its file')"
check "names L" "$(contains "$out" 'The numbered entries end at L = 2')"

root=$(repository entry-renumbered)
version_go | sed 's|^// 2: |// 3: |' > "$root/$api/version.go"
commit "$root"
run_check "$root"
refused "a numbered entry renumbered"
check "names the new number" "$(contains "$out" "version.go${colon}7: \`// 3:\` is a numbered entry that the base")"
# shellcheck disable=SC2016 # the backticks are the refusal's own text, not a substitution
check "names the number that went" \
  "$(contains "$out" 'the numbered entry `// 2:` (line 7 on the base, DAEMON_API_CONTRACT_BASE (base)) is gone from this file')"

root=$(repository entry-twice)
put "$root" "$api/version.go" "$(version_go '' $'//\n// 2: a second entry at the base\'s own number.')"
commit "$root"
run_check "$root"
refused "a second entry at a number the base holds"
check "names the second header" "$(contains "$out" "version.go${colon}10: \`// 2:\` is a numbered entry that the base")"

root=$(repository entry-reordered)
{
  printf 'package api\n\n// The contract'"'"'s doc comment, which holds no number.\n//\n'
  printf '// 2: the second numbered entry, moved first.\n//\n// 1: the first numbered entry.\n'
} > "$root/$api/version.go"
commit "$root"
run_check "$root"
refused "the numbered entries reordered"
check "says a numbered entry keeps its place" "$(contains "$out" 'is not where the base, DAEMON_API_CONTRACT_BASE (base), has it')"

echo "rule 2: an entry and its fixture stay"
root=$(repository entry-new)
put "$root" "$api/contract_20261126000000_second-entry.go" "$(entry_go 20261126000000 second-entry)"
put "$root" "$fixtures/contracts/20261126000000_second-entry.json" '{"answers": []}'
commit "$root"
passes "$root" "a new entry with its fixture"
check "counts both entries" "$(contains "$out" '2 entries with their fixtures')"

root=$(repository entry-deleted)
git -C "$root" rm -q "$entry" "$fixture"
commit "$root"
run_check "$root"
refused "an entry and its fixture deleted"
check "names the entry and says to put it back" \
  "$(contains "$out" "^::error file=$entry::$entry: this change deletes or renames an entry that the base, DAEMON_API_CONTRACT_BASE (base), holds.*put it back under this name")"
check "names the fixture" \
  "$(contains "$out" "^::error file=$fixture::$fixture: this change deletes or renames an entry fixture")"
check "says how to undo an entry" "$(contains "$out" 'add a new entry that undoes it')"

root=$(repository entry-renamed)
git -C "$root" mv "$entry" "$api/contract_20261125010101_renamed.go"
git -C "$root" mv "$fixture" "$fixtures/contracts/20261125010101_renamed.json"
commit "$root"
run_check "$root"
refused "an entry and its fixture renamed"
check "names the entry's old name" "$(contains "$out" "^::error file=$entry::")"
check "names the fixture's old name" "$(contains "$out" "^::error file=$fixture::")"
check "the new names pair, so rule 3 is silent" "$(is "$(contains "$out" 'has no fixture\|names no entry')" false)"

echo "rule 3: an entry has its fixture, and the fixtures' directory holds nothing else"
root=$(repository entry-without-fixture)
put "$root" "$api/contract_20261126000000_alone.go" "$(entry_go 20261126000000 alone)"
commit "$root"
run_check "$root"
refused "an entry with no fixture"
check "names the entry, its fixture and the command that writes it" \
  "$(contains "$out" "^::error file=$api/contract_20261126000000_alone.go::.*has no fixture, $fixtures/contracts/20261126000000_alone.json.*go test ./internal/api -update")"

root=$(repository fixture-without-entry)
put "$root" "$fixtures/contracts/20261126000000_orphan.json" '{"answers": []}'
commit "$root"
run_check "$root"
refused "a fixture with no entry"
check "names the fixture and the entry it lacks" \
  "$(contains "$out" "^::error file=$fixtures/contracts/20261126000000_orphan.json::.*names no entry: HEAD holds no $api/contract_20261126000000_orphan.go")"

# What is not a fixture by its name, or is not a file of the directory itself, no reader counts,
# so it pairs with no entry; TestEveryEntryHasOneFixture refuses it, and this check does not.
root=$(repository fixture-directory-other)
put "$root" "$fixtures/contracts/README.md" 'Not a fixture.'
put "$root" "$fixtures/contracts/more/20261126000000_nested.json" '{}'
commit "$root"
passes "$root" "a file and a subdirectory in the fixtures' directory that no reader counts"
check "counts the one entry" "$(contains "$out" '1 entries with their fixtures')"

echo "rule 4: no committed number"
root=$(repository number-manifest)
put "$root" packages/pi-legion/package.json '{"name": "pi-legion", "legion": {"daemonApiVersion": 3}}'
commit "$root"
run_check "$root"
refused "a manifest that commits the number"
check "names the manifest and says the pack writes it" \
  "$(contains "$out" '^::error file=packages/pi-legion/package.json::packages/pi-legion/package.json commits legion.daemonApiVersion (3).*each pack step writes the field')"
check "the word in a script of the other manifest is no number" \
  "$(is "$(contains "$out" 'file=packages/claude-envoy/package.json')" false)"

root=$(repository number-version-json)
put "$root" "$fixtures/version.json" '{"daemonApiVersion": 3}'
commit "$root"
run_check "$root"
refused "a version.json"
check "names it and what reads the number instead" \
  "$(contains "$out" "^::error file=$fixtures/version.json::.*scripts/daemon-contract.sh. prints. Delete it.")"

root=$(repository number-state)
put "$root" "$fixtures/state.json" '{"contract": {"daemon": 3, "caller": 3}, "settings": {}}'
commit "$root"
run_check "$root"
refused "state.json's contract mark"
check "names the golden and the command that writes it" \
  "$(contains "$out" "^::error file=$fixtures/state.json::$fixtures/state.json holds a .contract. mark.*TestStateGolden")"

root=$(repository number-narrowed)
put "$root" "$fixtures/state-narrowed-repositories.json" '{"contract": {"caller": 1, "daemon": 3}}'
commit "$root"
run_check "$root"
refused "the narrowed golden's daemon mark"
check "names the golden" \
  "$(contains "$out" "^::error file=$fixtures/state-narrowed-repositories.json::.*holds a .contract.daemon. mark")"

echo "rule 5: a fragment names no number above L"
root=$(repository fragment-below)
put "$root" packages/daemon/.changes/unreleased/new.yaml \
  $'kind: Added\nbody: |-\n  Entry 20261126000000 adds a key, which callers below contract 2 are not served.'
commit "$root"
passes "$root" "a fragment that names contract 2 and an entry id"

root=$(repository fragment-above)
put "$root" packages/daemon/.changes/unreleased/new.yaml $'kind: Added\nbody: |-\n  Declares daemon API\n  CONTRACT 3.'
commit "$root"
run_check "$root"
refused "a fragment that names contract 3, wrapped and in capitals"
check "names the file, the line, the number and L" \
  "$(contains "$out" '^::error file=packages/daemon/.changes/unreleased/new.yaml::packages/daemon/.changes/unreleased/new.yaml:4: names daemon API contract 3, above L = 2')"
check "says to name the entry id" "$(contains "$out" 'name the entry by its id')"

root=$(repository fragment-list)
put "$root" packages/pi-legion/.changes/unreleased/new.yaml $'kind: Fixed\nbody: Callers at contracts 1 and 3 read it.'
commit "$root"
run_check "$root"
refused "a fragment that lists contracts 1 and 3"
check "names 3 and not 1" \
  "$(contains "$out" 'new.yaml:2: names daemon API contract 3, above')"
check "  and not 1" "$(is "$(contains "$out" 'names daemon API contract 1,')" false)"

root=$(repository fragment-edited)
put "$root" packages/daemon/.changes/unreleased/base.yaml $'kind: Changed\nbody: The daemon serves contract 3.'
commit "$root"
run_check "$root"
refused "a fragment the base holds, edited to name contract 3"
check "names it" "$(contains "$out" '^::error file=packages/daemon/.changes/unreleased/base.yaml::')"

# The phrasings the repository's fragments say their number with besides `contract <n>`: a verb
# before it, and words before the verb that say which number it is, across a line break.
root=$(repository fragment-verbs)
put "$root" packages/daemon/.changes/unreleased/is.yaml $'kind: Changed\nbody: The daemon API contract is 3.'
put "$root" packages/daemon/.changes/unreleased/moves.yaml $'kind: Changed\nbody: The daemon API contract moves to 3.'
put "$root" packages/daemon/.changes/unreleased/stays.yaml $'kind: Fixed\nbody: The daemon API contract stays 3.'
put "$root" packages/pi-legion/.changes/unreleased/declares.yaml \
  $'kind: Changed\nbody: |-\n  The daemon API contract this plugin\n  declares is 3.'
commit "$root"
run_check "$root"
refused "fragments that say their contract is, moves to or stays 3"
check "names the contract that is 3" \
  "$(contains "$out" '^::error file=packages/daemon/.changes/unreleased/is.yaml::packages/daemon/.changes/unreleased/is.yaml:2: names daemon API contract 3, above L = 2')"
check "names the contract that moves to 3" "$(contains "$out" '^::error file=packages/daemon/.changes/unreleased/moves.yaml::.*moves.yaml:2: names daemon API contract 3, above')"
check "names the contract that stays 3" "$(contains "$out" '^::error file=packages/daemon/.changes/unreleased/stays.yaml::.*stays.yaml:2: names daemon API contract 3, above')"
check "names the number after the words that say which it is" \
  "$(contains "$out" '^::error file=packages/pi-legion/.changes/unreleased/declares.yaml::.*declares.yaml:4: names daemon API contract 3, above')"

root=$(repository fragment-verbs-below)
put "$root" packages/daemon/.changes/unreleased/new.yaml $'kind: Changed\nbody: The daemon API contract is 2.'
commit "$root"
passes "$root" "a fragment whose contract is 2"

echo "rule 6: a merged fixture or frozen record changes only as a correction"
root=$(repository fixture-corrected)
put "$root" "$fixture" '{"answers": [{"answer": "state", "addKeyPaths": ["c"]}]}'
put "$root" "$entry" "$(entry_go 20261125010101 probe "$correction")"
commit "$root"
passes "$root" "an entry fixture edited with a Correction paragraph in its entry"

root=$(repository fixture-edited)
put "$root" "$fixture" '{"answers": [{"answer": "state", "addKeyPaths": ["c"]}]}'
commit "$root"
run_check "$root"
refused "an entry fixture edited with no Correction paragraph"
check "names the fixture, the entry and the paragraph it lacks" \
  "$(contains "$out" "^::error file=$fixture::$fixture: this change edits the fixture of entry 20261125010101.*adds no paragraph that starts .Correction:. to that entry's text in $entry")"

root=$(repository fixture-edited-again)
put "$root" "$entry" "$(entry_go 20261125010101 probe "$correction")"
git -C "$root" commit -q -a --amend --no-edit
git -C "$root" tag -f base > /dev/null 2>&1
put "$root" "$fixture" '{"answers": [{"answer": "state", "addKeyPaths": ["d"]}]}'
commit "$root"
run_check "$root"
refused "an entry fixture edited again, under the Correction paragraph the base already holds"
check "names the fixture" "$(contains "$out" "^::error file=$fixture::")"

root=$(repository fixture-mid-paragraph)
put "$root" "$fixture" '{"answers": [{"answer": "state", "addKeyPaths": ["c"]}]}'
put "$root" "$entry" "$(entry_go 20261125010101 probe '// Correction: a line inside the first paragraph, which starts none.')"
commit "$root"
run_check "$root"
refused "an entry fixture edited under a Correction line that starts no paragraph"

root=$(repository record-corrected)
sed -i 's/"keyPaths": \["a", "b"\]/"keyPaths": ["a", "c"]/' "$root/$fixtures/contract-shapes.json"
put "$root" "$api/version.go" "$(version_go '' "$correction")"
commit "$root"
passes "$root" "a record of contract 2 edited with a Correction paragraph in entry 2"

root=$(repository record-edited)
sed -i 's/"keyPaths": \["a", "b"\]/"keyPaths": ["a", "c"]/' "$root/$fixtures/contract-shapes.json"
put "$root" "$api/version.go" "$(version_go "$correction")"
commit "$root"
run_check "$root"
refused "a record of contract 2 edited with the Correction paragraph in entry 1"
# shellcheck disable=SC2016 # the backticks are the refusal's own text, not a substitution
check "names the file, the contract and its entry" \
  "$(contains "$out" "^::error file=$fixtures/contract-shapes.json::$fixtures/contract-shapes.json: this change edits the records of contract 2.*the text of the numbered entry \`// 2:\` in $api/version.go")"
check "and not contract 1, whose record it leaves" "$(is "$(contains "$out" 'records of contract 1')" false)"

root=$(repository request-edited)
sed -i 's/"fieldPaths": \["id"\]/"fieldPaths": ["id", "name"]/' "$root/$fixtures/request-shapes.json"
commit "$root"
run_check "$root"
refused "a request record of contract 2 edited"
check "names contract 2" "$(contains "$out" "^::error file=$fixtures/request-shapes.json::.*records of contract 2")"

root=$(repository answer-edited)
sed -i 's/"noBody": false/"noBody": true/' "$root/$fixtures/answers.json"
commit "$root"
run_check "$root"
refused "an answer's row of answers.json edited"
check "names the answer's since" "$(contains "$out" "^::error file=$fixtures/answers.json::.*records of contract 1,")"
check "  and not its typed setting's" "$(is "$(contains "$out" 'records of contract 2')" false)"

root=$(repository setting-edited)
sed -i 's/"until": 2/"until": 1/' "$root/$fixtures/answers.json"
put "$root" "$api/version.go" "$(version_go "$correction" "$correction")"
commit "$root"
passes "$root" "a typed setting's until moved, with a Correction paragraph in both entries it names"

root=$(repository value-added)
sed -i 's/{"value": "b", "since": 2}/{"value": "b", "since": 2}, {"value": "c", "since": 2}/' \
  "$root/$fixtures/enumerated-fields.json"
commit "$root"
run_check "$root"
refused "a value of contract 2 added to enumerated-fields.json"
check "names contract 2 alone" \
  "$(contains "$out" "^::error file=$fixtures/enumerated-fields.json::.*records of contract 2,")"
check "  and not contract 1" "$(is "$(contains "$out" 'records of contract 1')" false)"

root=$(repository released-added)
put "$root" "$fixtures/released-contracts.json" '[1, 2, 3]'
commit "$root"
run_check "$root"
refused "a number added to released-contracts.json"
check "says no numbered entry holds it" \
  "$(contains "$out" "^::error file=$fixtures/released-contracts.json::.*records of contract 3, which no numbered entry of $api/version.go holds")"

root=$(repository record-above-l)
sed -i 's/^]$//; s/}]$/},/' "$root/$fixtures/contract-shapes.json"
printf '%s\n' ' {"contract": 3, "answer": "state", "noBody": false, "keyPaths": ["a", "b", "c"], "domains": {}}]' \
  >> "$root/$fixtures/contract-shapes.json"
commit "$root"
run_check "$root"
refused "a record of a contract above L written into contract-shapes.json"
check "says a new entry's records are its fixture" \
  "$(contains "$out" "records of contract 3, which no numbered entry.*a new entry's records are its fixture")"

root=$(repository record-reformatted)
python3 -c 'import json, sys; p = sys.argv[1]; d = json.load(open(p)); open(p, "w").write(json.dumps(d, indent=2) + "\n")' \
  "$root/$fixtures/contract-shapes.json"
commit "$root"
run_check "$root"
refused "a frozen file whose bytes change and whose records do not"
check "says to put it back" "$(contains "$out" 'changes no record in it.*Put it back as the base')"

root=$(repository record-deleted)
git -C "$root" rm -q "$fixtures/answers.json"
commit "$root"
run_check "$root"
refused "a frozen file deleted"
check "names it" "$(contains "$out" "^::error file=$fixtures/answers.json::.*deletes a frozen record file")"

echo "case: every refusal of one change named in one run"
root=$(repository all-at-once)
put "$root" "$api/version.go" "$(version_go '' $'//\n// 3: a numbered entry added after L.')"
put "$root" "$fixtures/version.json" '{"daemonApiVersion": 3}'
put "$root" "$fixtures/contracts/20261126000000_orphan.json" '{}'
commit "$root"
run_check "$root"
refused "three faults"
check "names each" "$(is "$(grep -c '^::error file=' <<<"$out")" 3)"

echo "case: L cannot be read at the head"
root=$(repository no-released)
git -C "$root" rm -q "$fixtures/released-contracts.json"
commit "$root"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the read that failed" \
  "$(contains "$out" "^::error file=$fixtures/released-contracts.json::could not read L at HEAD: $fixtures/released-contracts.json is missing")"

for released in '[]' '[1, "2"]' '[1, true]' '{"contracts": [1, 2]}'; do
  root=$(repository "released-form-$(printf %s "$released" | tr -c 'a-z0-9' -)")
  put "$root" "$fixtures/released-contracts.json" "$released"
  commit "$root"
  run_check "$root"
  check "released-contracts.json of $released: exits 1" "$(is "$status" 1)"
  check "released-contracts.json of $released: names the read that failed" \
    "$(contains "$out" "^::error file=$fixtures/released-contracts.json::could not read L at HEAD: $fixtures/released-contracts.json is not a non-empty JSON array of integers")"
done

echo "case: a base the check cannot resolve"
root=$(repository no-such-base)
commit "$root"
set +e
out=$(DAEMON_API_CONTRACT_BASE=no-such-rev "$root/.github/scripts/check-daemon-api-contract.sh" 2>&1)
status=$?
set -e
check "exits 1" "$(is "$status" 1)"
check "names the base it could not read" \
  "$(contains "$out" 'could not read the base, DAEMON_API_CONTRACT_BASE (no-such-rev): git rev-parse')"

echo "case: no event and no base"
root=$(repository no-event)
commit "$root"
set +e
out=$(env -u DAEMON_API_CONTRACT_BASE -u GITHUB_EVENT_NAME "$root/.github/scripts/check-daemon-api-contract.sh" 2>&1)
status=$?
set -e
check "exits 1" "$(is "$status" 1)"
check "says what it runs on" "$(contains "$out" 'this check runs on a pull request or a merge group.*or with DAEMON_API_CONTRACT_BASE=<rev>, and GITHUB_EVENT_NAME is unset')"

# checkout <root> <commit> <name>: what actions/checkout makes of <commit> of the miniature, one
# commit deep, with the real script linked in. The miniature serves a fetch without blobs and of
# any commit, as GitHub does. Echoes the checkout's root.
checkout() {
  local into="$work/$3"
  git -C "$1" config uploadpack.allowFilter true
  git -C "$1" config uploadpack.allowAnySHA1InWant true
  git -c init.defaultBranch=main init -q "$into"
  git -C "$into" remote add origin "file://$1"
  git -C "$into" fetch -q --no-tags --depth=1 origin "$2"
  git -C "$into" checkout -q --detach FETCH_HEAD
  mkdir -p "$into/.github/scripts"
  ln -s "$check_script" "$into/.github/scripts/check-daemon-api-contract.sh"
  echo "$into"
}

# run_event <checkout> <event name>: runs the real script in the checkout as CI runs it, with the
# payload at <checkout>.json. Sets out and status.
run_event() {
  set +e
  out=$(env -u DAEMON_API_CONTRACT_BASE GITHUB_EVENT_NAME="$2" GITHUB_EVENT_PATH="$1.json" \
    "$1/.github/scripts/check-daemon-api-contract.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a merge group, its base fetched into a checkout one commit deep"
root=$(repository group)
base=$(git -C "$root" rev-parse base)
put "$root" "$api/version.go" "$(version_go '' $'//\n// 3: a numbered entry added after L.')"
commit "$root" "the group's one change"
into=$(checkout "$root" "$(git -C "$root" rev-parse HEAD)" group-checkout)
printf '{"merge_group":{"base_sha":"%s"}}\n' "$base" > "$into.json"
check "the checkout lacks the base" \
  "$(is "$(git -C "$into" cat-file -e "$base^{commit}" 2>/dev/null && echo has || echo lacks)" lacks)"
run_event "$into" merge_group
refused "a merge group that adds a numbered entry"
check "reads the base's version.go, fetched, and names the group's base" \
  "$(contains "$out" "version.go${colon}10: \`// 3:\` is a numbered entry that the base, the commit the queue built this group on ($base), does not hold")"
check "fetched the base without blobs" "$(is "$(git -C "$into" config remote.origin.partialclonefilter)" blob:none)"

root=$(repository group-passing)
base=$(git -C "$root" rev-parse base)
put "$root" "$api/contract_20261126000000_second.go" "$(entry_go 20261126000000 second)"
put "$root" "$fixtures/contracts/20261126000000_second.json" '{}'
commit "$root" "the group's one change"
into=$(checkout "$root" "$(git -C "$root" rev-parse HEAD)" group-passing-checkout)
printf '{"merge_group":{"base_sha":"%s"}}\n' "$base" > "$into.json"
run_event "$into" merge_group
check "a merge group that adds an entry: exits 0" "$(is "$status" 0)"
check "a merge group that adds an entry: names its base" \
  "$(contains "$out" "passes, compared with the base, the commit the queue built this group on ($base)")"

root=$(repository group-unknown-base)
commit "$root"
into=$(checkout "$root" "$(git -C "$root" rev-parse HEAD)" group-unknown-base-checkout)
printf '{"merge_group":{"base_sha":"%s"}}\n' bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb > "$into.json"
run_event "$into" merge_group
check "a base origin does not hold: exits 1" "$(is "$status" 1)"
check "a base origin does not hold: names the fetch that failed" \
  "$(contains "$out" 'could not read the base, the commit the queue built this group on (bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb): git fetch')"

echo "case: a fetch from origin that fails, git's words holding the marker"
# Whatever answers the fetch in origin's place writes git's words, and the credential the checkout
# presents to origin can be among them, so the case puts the marker not-a-secret-token in them (in
# origin's URL, which git's refusal quotes), and no run of eight of its characters (the run the
# redaction rule's gate reads) may be in what the check prints. The twin: the file the refusal
# names holds the marker.
marker=not-a-secret-token
prints_no_marker() {
  local at
  for ((at = 0; at + 8 <= ${#marker}; at++)); do
    [[ "$1" == *"${marker:at:8}"* ]] && { echo false; return; }
  done
  echo true
}
root=$(repository group-fetch-words)
commit "$root"
into=$(checkout "$root" "$(git -C "$root" rev-parse HEAD)" group-fetch-words-checkout)
git -C "$into" remote set-url origin "file://$work/origin-$marker-gone"
printf '{"merge_group":{"base_sha":"%s"}}\n' cccccccccccccccccccccccccccccccccccccccc > "$into.json"
mkdir -p "$work/kept"
RUNNER_TEMP="$work/kept" run_event "$into" merge_group
check "a fetch git's words refuse: exits 1" "$(is "$status" 1)"
check "a fetch git's words refuse: prints no run of the marker" "$(prints_no_marker "$out")"
check "a fetch git's words refuse: names the fetch, its exit and the file that keeps git's words" \
  "$(contains "$out" "git fetch --no-tags --filter=blob:none exited [0-9][0-9]*; git's words are in $work/kept/")"
kept_file=$(sed -n "s/^.*git's words are in \([^ ;)]*\).*\$/\1/p" <<<"$out" | tail -n 1)
check "a fetch git's words refuse: twin, that file holds the marker" \
  "$([ -n "$kept_file" ] && grep -q -F "$marker" -- "$kept_file" && echo true || echo false)"

echo "case: a pull request's merge ref, the base branch moved on since GitHub built it"
root=$(repository pull)
base=$(git -C "$root" rev-parse base)
git -C "$root" checkout -q -b topic
put "$root" packages/daemon/.changes/unreleased/topic.yaml $'kind: Fixed\nbody: A fix that names no number.'
commit "$root" "the pull request's change"
git -C "$root" checkout -q main
git -C "$root" merge -q --no-ff --no-edit topic
merge=$(git -C "$root" rev-parse HEAD)
git -C "$root" update-ref refs/pull/1/merge "$merge"
git -C "$root" reset -q --hard "$base"
# Main takes another entry after GitHub built the merge ref; a re-run reuses that ref, which lacks
# the entry, and is not read as deleting it.
put "$root" "$api/contract_20261126000000_later.go" "$(entry_go 20261126000000 later)"
put "$root" "$fixtures/contracts/20261126000000_later.json" '{}'
commit "$root" "main moves on"
into=$(checkout "$root" "$merge" pull-checkout)
printf '{"pull_request":{"number":1,"base":{"ref":"main"}}}\n' > "$into.json"
run_event "$into" pull_request
check "exits 0" "$(is "$status" 0)"
check "names the commit the merge ref was built on" \
  "$(contains "$out" "passes, compared with the base, the commit GitHub built this pull request's merge ref on ($base)")"

root=$(repository pull-not-a-merge)
commit "$root"
into=$(checkout "$root" "$(git -C "$root" rev-parse HEAD)" pull-not-a-merge-checkout)
printf '{"pull_request":{"number":1,"base":{"ref":"main"}}}\n' > "$into.json"
run_event "$into" pull_request
check "a checkout that is no merge ref: exits 1" "$(is "$status" 1)"
check "a checkout that is no merge ref: says so" \
  "$(contains "$out" "HEAD names 1 parent(s), so this checkout is not the merge ref GitHub builds a pull request's run on")"

summary "check-daemon-api-contract.sh"
