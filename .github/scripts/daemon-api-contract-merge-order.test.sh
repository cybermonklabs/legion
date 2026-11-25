#!/usr/bin/env bash
# Two branches that each change the daemon API merge in either order, with no conflict on a
# contract value and no renumber round (dispatch://INFRA-1569, R1).
#
# The test copies HEAD's tree into a scratch repository as one base commit, and makes two
# branches from it. Branch a adds an entry with a new answer and its route; branch b adds an entry
# with a new state key and the band that serves the state without it below the entry's number.
# Each branch writes its entry file with scripts/daemon-contract-entry.sh and its entry fixture
# with `go test ./internal/api -update`, as an author does. Then, for each order of the two, it
# merges the second branch into the first and requires:
#
#   - `git merge` exits 0 with no conflict, and the merged tree is the same whichever order;
#   - no file changes after the merge: `go test ./internal/api -update` writes nothing;
#   - the contract check passes against the base;
#   - `go test ./internal/api` passes, the shape gate with it;
#   - `legion version --contracts` prints the base's number plus two.
#
# Run from the repository root: bash .github/scripts/daemon-api-contract-merge-order.test.sh
set -euo pipefail

root=$(git rev-parse --show-toplevel)
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
repo=$scratch/repo
mkdir -p "$repo"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

step() {
  echo "== $*"
}

# One scratch repository holding HEAD's tree. git's own identity is the test's, so the test needs
# no configuration of the machine it runs on.
step "the base: HEAD's tree as one commit"
git -C "$root" archive HEAD | tar -x -C "$repo"
cd "$repo"
git init -q -b main
git add -A
git -c user.name=merge-order-test -c user.email=merge-order-test@localhost commit -q -m base
commit() {
  git add -A
  git -c user.name=merge-order-test -c user.email=merge-order-test@localhost commit -q -m "$1"
}
base=$(git rev-parse HEAD)
base_number=$(bash scripts/daemon-contract.sh HEAD)
base_fixtures=$(git ls-tree -r --name-only HEAD -- packages/contracts/fixtures/daemon-api/contracts | wc -l)
echo "base $base declares contract $base_number and holds $base_fixtures entry fixtures"

api=packages/daemon/internal/api

# write_rows FILE ROWS replaces the entry file's registration, which the entry script writes with
# no rows, by one that carries ROWS (the fields of contractEntry), and appends CODE, the Go code the
# rows name, after it. @ENTRY@ in either is the entry's constant.
write_rows() {
  local file=$1 rows=$2 code=$3
  python3 - "$file" "$rows" "$code" <<'PY'
import re, sys
path, rows, code = sys.argv[1:4]
text = open(path).read()
match = re.search(r"registerEntry\(contractEntry\{id: (\w+), slug: \"([a-z0-9-]+)\"\}\)", text)
if not match:
    sys.exit(f"{path}: no registration of the form the entry script writes")
constant, slug = match.groups()
registration = "registerEntry(contractEntry{\n\t\tid: %s, slug: \"%s\",\n%s\n\t})" % (constant, slug, rows.replace("@ENTRY@", constant))
text = text[:match.start()] + registration + text[match.end():]
open(path, "w").write(text.rstrip("\n") + "\n\n" + code.replace("@ENTRY@", constant) + "\n")
PY
  gofmt -w "$file"
}

# write_test_rows ENTRY BODY writes the entry's test rows, the `_test.go` file of the entry file's
# name, with BODY as the fields of its entryTestRows. @ENTRY@ in BODY is the entry's constant.
write_test_rows() {
  local entry=$1 body=$2
  python3 - "$entry" "$body" <<'PY'
import re, sys
path, body = sys.argv[1:3]
constant = re.search(r"^const (\w+Contract) = \d{14}$", open(path).read(), re.M).group(1)
rows = path[: -len(".go")] + "_test.go"
open(rows, "w").write(
    "package api\n\nimport \"testing\"\n\n"
    "func init() {\n\tregisterEntryTestRows(entryTestRows{\n\t\tentry: %s,\n%s\n\t})\n}\n"
    % (constant, body.replace("@ENTRY@", constant)))
PY
  gofmt -w "${entry%.go}_test.go"
}

branch_a_test_rows() {
  write_test_rows "$1" '		answers: func(t *testing.T) map[AnswerID]answerFixture {
			return map[AnswerID]answerFixture{answerMergeOrderProbe: {
				roots:  typesOf(MergeOrderProbe{}),
				bodies: []any{MergeOrderProbe{OK: true}},
			}}
		},'
}

branch_b_test_rows() {
  write_test_rows "$1" '		states: func(t *testing.T) []State {
			state := stateCarryingTheRestOfTheShape(t)
			state.MergeOrderKey = "carried"
			return []State{state}
		},
		carry: func(state State) State {
			state.MergeOrderKey = "carried"
			return state
		},'
}

# branch_a_changes ENTRY: a new answer, the identity at every contract, and the GET route that
# answers it. The route is a line of server.go's route table that branch b leaves alone; the
# answer's row, its declaration and its handler are the entry file's.
branch_a_changes() {
  local entry=$1
  write_rows "$entry" '		answers:         []Answer{{ID: answerMergeOrderProbe, Routes: []string{"GET /legion/v1/merge-order-probe"}, Since: @ENTRY@}},
		identityAnswers: []AnswerID{answerMergeOrderProbe},' '// answerMergeOrderProbe is the answer of the route this entry adds.
const answerMergeOrderProbe AnswerID = "merge-order-probe"

// MergeOrderProbe is the body of the route this entry adds.
type MergeOrderProbe struct {
	OK bool `json:"ok"`
}

func (s *server) mergeOrderProbeRoute(w http.ResponseWriter, r *http.Request) {
	s.writeAnswer(w, r, answerMergeOrderProbe, http.StatusOK, MergeOrderProbe{OK: true})
}'
  sed -i '0,/^package api$/s//package api\n\nimport "net\/http"/' "$entry"
  python3 - "$api/server.go" <<'PY'
import sys
path = sys.argv[1]
text = open(path).read()
anchor = '\t\t{"GET /readyz", s.readyz},\n'
assert anchor in text, "server.go: no readyz route to add the probe's route after"
open(path, "w").write(text.replace(anchor, anchor + '\t\t{"GET /legion/v1/merge-order-probe", s.mergeOrderProbeRoute},\n', 1))
PY
  # Two tests hold every route of the table to a row of their own, as they do for any new route.
  python3 - "$api/fact_apply_test.go" "$api/storable_text_test.go" <<'PY'
import re, sys
rows = {
    sys.argv[1]: '\t\t"GET /legion/v1/merge-order-probe": {none: "the merge-order test\'s probe; " + noFact},\n',
    sys.argv[2]: '\t\t"GET /legion/v1/merge-order-probe": noBody,\n',
}
for path, row in rows.items():
    text = open(path).read()
    match = re.search(r'^\t\t"GET /readyz": .*\n', text, re.M)
    assert match, path + ": no GET /readyz row to add the probe's row after"
    open(path, "w").write(text[:match.end()] + row + text[match.end():])
PY
  gofmt -w "$api/fact_apply_test.go" "$api/storable_text_test.go"
  gofmt -w "$entry"
  branch_a_test_rows "$entry"
}

# branch_b_changes ENTRY: a new key of the state, written on every body, and the band that takes
# it off a caller whose strip is below the entry's number. The field is a line of state.go that
# branch a leaves alone; the band and its function are the entry file's.
branch_b_changes() {
  local entry=$1
  write_rows "$entry" '		bands: []stateNarrowing{{contract: @ENTRY@, state: withoutMergeOrderKey, dropped: "mergeOrderKey"}},' '// withoutMergeOrderKey is the state without the key this entry adds, for a caller below it.
func withoutMergeOrderKey(state State) (State, bool) {
	if state.MergeOrderKey == "" {
		return state, false
	}
	state.MergeOrderKey = ""
	return state, true
}'
  python3 - "$api/state.go" <<'PY'
import sys
path = sys.argv[1]
text = open(path).read()
anchor = '\tIntakeGiveUps IntakeGiveUpsView `json:"intakeGiveUps"`\n'
assert anchor in text, "state.go: no IntakeGiveUps field to add the probe's key after"
field = ('\t// MergeOrderKey is the key the merge-order test\'s entry adds, empty on every body the\n'
         '\t// daemon writes and set by the shape gate\'s fixture state alone.\n'
         '\tMergeOrderKey string `json:"mergeOrderKey,omitempty"`\n')
open(path, "w").write(text.replace(anchor, anchor + field, 1))
PY
  gofmt -w "$api/state.go"
  branch_b_test_rows "$entry"
}

step "branch a: an entry with a new answer and its route"
git checkout -q -b a main
entry_a=$(bash scripts/daemon-contract-entry.sh merge-order-answer)
[ -f "$entry_a" ] || fail "scripts/daemon-contract-entry.sh wrote no entry file (it printed '$entry_a')"
branch_a_changes "$entry_a"
(cd packages/daemon && go test -count=1 ./internal/api/ -update >"$scratch/update-a.txt" 2>&1) ||
  { cat "$scratch/update-a.txt"; fail "branch a: go test ./internal/api -update failed"; }
commit "a: an entry with a new answer and its route"
# Two entries made in one second take one id, which TestNoTwoEntriesShareAnId refuses; an author
# makes entries minutes apart, and the test waits a second for the same reason.
sleep 1

step "branch b: an entry with a new state key and its band"
git checkout -q -b b main
entry_b=$(bash scripts/daemon-contract-entry.sh merge-order-state-key)
[ -f "$entry_b" ] || fail "scripts/daemon-contract-entry.sh wrote no entry file (it printed '$entry_b')"
branch_b_changes "$entry_b"
(cd packages/daemon && go test -count=1 ./internal/api/ -update >"$scratch/update-b.txt" 2>&1) ||
  { cat "$scratch/update-b.txt"; fail "branch b: go test ./internal/api -update failed"; }
commit "b: an entry with a new state key and its band"

# Each branch holds the base's entry fixtures, however many HEAD's tree holds, and its own one.
for b in a b; do
  fixtures=$(git ls-tree -r --name-only "$b" -- packages/contracts/fixtures/daemon-api/contracts | wc -l)
  [ "$fixtures" -eq $((base_fixtures + 1)) ] ||
    fail "branch $b holds $fixtures entry fixtures, want the base's $base_fixtures and its own"
  echo "branch $b: $(git diff --name-only main "$b" | tr '\n' ' ')"
done

trees=()
for order in "a b" "b a"; do
  read -r first second <<<"$order"
  step "merge $second into $first"
  git checkout -q -B "merged-$first-$second" "$first"
  if ! git -c user.name=merge-order-test -c user.email=merge-order-test@localhost merge --no-edit "$second" >"$scratch/merge.txt" 2>&1; then
    cat "$scratch/merge.txt"
    git diff --name-only --diff-filter=U
    fail "the merge of $second into $first conflicts"
  fi
  if grep -q CONFLICT "$scratch/merge.txt"; then
    cat "$scratch/merge.txt"
    fail "the merge of $second into $first reports a conflict"
  fi
  trees+=("$(git rev-parse HEAD^{tree})")

  (cd packages/daemon && go test -count=1 ./internal/api/ -update >"$scratch/update.txt" 2>&1) ||
    { cat "$scratch/update.txt"; fail "$first+$second: go test ./internal/api -update failed"; }
  if [ -n "$(git status --porcelain)" ]; then
    git status --porcelain
    git diff --stat
    fail "$first+$second: -update changed files after the merge, which is a renumber round"
  fi

  DAEMON_API_CONTRACT_BASE=$base bash .github/scripts/check-daemon-api-contract.sh >"$scratch/check.txt" 2>&1 ||
    { cat "$scratch/check.txt"; fail "$first+$second: the contract check refuses the merge"; }

  (cd packages/daemon && go test -count=1 ./internal/api/ >"$scratch/test.txt" 2>&1) ||
    { grep -E '^(---|FAIL|ok|panic)|_test.go:[0-9]+:' "$scratch/test.txt" | head -40; fail "$first+$second: go test ./internal/api failed"; }

  contracts=$(cd packages/daemon && go run ./cmd/legion version --contracts)
  echo "$contracts"
  got=$(awk '$1 == "contract" { print $2 }' <<<"$contracts")
  [ "$got" = "$((base_number + 2))" ] ||
    fail "$first+$second: legion version --contracts prints contract '$got', want $((base_number + 2))"
  echo "$first+$second: no conflict, no change after the merge, contract $got"
done

[ "${trees[0]}" = "${trees[1]}" ] || fail "the two orders merge to two trees (${trees[0]} and ${trees[1]})"
echo "PASS: both orders merge with no conflict and no renumber, at contract $((base_number + 2))"
