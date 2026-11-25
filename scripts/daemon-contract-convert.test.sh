#!/usr/bin/env bash
# A branch made before dispatch://INFRA-1569 that adds a numbered entry converts to an entry
# file with one script, and then merges that change with no conflict (R8, check c-convert).
#
# The test makes a scratch branch, in a worktree of this checkout, from the commit this change
# branched from: the commit given as its one argument, else the merge base of HEAD with
# refs/remotes/origin/main. That commit declares `const DaemonAPIVersion = N`. The branch adds
# numbered entry N+1 as an author did before the cutover:
#
#   - its text, a `// N+1: ` paragraph above the const line of version.go, and the const line
#     raised to N+1;
#   - its constant, `const convertProbeContract = N+1` beside the other contracts of shaping.go,
#     appended to SupportedContracts;
#   - its band, a row of stateNarrowings naming the constant, whose strip function is in a new
#     file of its own;
#   - a new answer, a row of Answers that arrives at the constant, with its AnswerID and its
#     place in shapeAnswer's identity case, and its fixture in contract_shapes_test.go;
#   - one row the script does not move: a value of the waiting line's `kind`, an existing
#     enumerated field, arriving at the constant in answers.go's lineKinds, which
#     TestNoFrozenRowNamesAnEntry reads (enumeratedFields() values' Since);
#   - its edits of the manifests and fixtures: `legion.daemonApiVersion` of both manifests by
#     hand, and every golden by `go test ./internal/api -update` as the bump rule said, among them
#     version.json, the state marks, the records of N+1 in contract-shapes.json and
#     request-shapes.json, the `since: N+1` row of answers.json and the `since: N+1` value of
#     enumerated-fields.json.
#
# Then it runs scripts/daemon-contract-convert.sh on the branch, with this change (HEAD) as the
# main it merges, and requires:
#
#   - the merge of this change into the branch exits 0 with no conflict;
#   - the branch holds one entry file, with the entry's text and its band;
#   - no edit of a derived copy remains: version.go, shaping.go, the manifests, the state goldens
#     and the four record files are this change's, and answers.go differs from it by the one
#     row left behind;
#   - the script names the file and line of that row;
#   - TestNoFrozenRowNamesAnEntry fails until the test moves that row into the entry file;
#   - after the move and the `-update` that then writes the entry's fixture, with the request part
#     of the entry that the script kept (`-entry-requests`, no change here), the fixture holds the
#     two `since: N+1` rows (the answer's and the value's), and the contract check and the shape
#     gate pass.
#
# Without the conversion the merge conflicts in version.go and answers.go, and the test fails
# there (the control: this file with the line that runs the script taken out).
#
# A second scratch branch, from the same commit, adds two numbered entries, N+1 and N+2, each in a
# commit of its own as the rule before the cutover had it: its text, its constant in
# SupportedContracts, one field of the body of `POST /legion/v1/project/pause` (convertFirst, then
# convertSecond), its manifests, and its goldens by `-update`, which writes the records of its own
# number in request-shapes.json and keeps those of the numbers below it. So the branch's record of
# N+1 holds convertFirst alone, and its record of N+2 both fields. After the conversion, the
# fixture of N+1's entry records convertFirst alone and the fixture of N+2's convertSecond alone:
# the request types of the converted branch read both fields at every number, so only the
# branch's records of each number, which the script keeps before it puts request-shapes.json back,
# tell one entry's change from the other's. TestEntryFixtures and the request gate then pass over
# the two fixtures.
#
# More rows hold the script to the ref the branch merges, --base, and to a stacked branch: one made
# from the first branch's commit of N+1, which adds numbered entry N+2 (its text, its constant in
# SupportedContracts, its manifests and its goldens by `-update`). Before the first branch's
# conversion, the script refuses each ref it cannot merge, exits non-zero naming the ref, and
# leaves the branch as it was: on the first branch, the first branch's base, a main from before the
# cutover whose tree defines no registerEntry, and this change with no version.go, which it cannot
# read; on the stacked branch, the first branch, a base branch that has not merged the cutover yet.
# After the first branch is converted and its row moved, the script run on the stacked branch with
# --base naming the first branch converts N+2 alone and merges the first branch, in a commit that
# names it: the stacked branch then differs from the first branch by N+2's entry file and fixture,
# so N+1, which the first branch converted, is not converted a second time under another id.
#
# Run from the repository root: bash scripts/daemon-contract-convert.test.sh [<base>]
set -euo pipefail

root=$(git rev-parse --show-toplevel)
cd "$root"
this=$(git rev-parse HEAD)
base=${1:-$(git merge-base HEAD refs/remotes/origin/main)}
base=$(git rev-parse --verify "$base^{commit}")
scratch=$(mktemp -d)
branch=convert-test-$$
repo=$scratch/repo
branch_two=convert-test-two-$$
repo_two=$scratch/repo-two
branch_stacked=convert-test-stacked-$$
repo_stacked=$scratch/repo-stacked
cleanup() {
  git -C "$root" worktree remove --force "$repo" >/dev/null 2>&1 || true
  git -C "$root" worktree remove --force "$repo_two" >/dev/null 2>&1 || true
  git -C "$root" worktree remove --force "$repo_stacked" >/dev/null 2>&1 || true
  git -C "$root" branch -D -q "$branch" "$branch_two" "$branch_stacked" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

step() {
  echo "== $*"
}

# excerpt FILE [N] prints the result lines of a go test log, at most N (40).
excerpt() {
  grep -E '^(---|FAIL|ok|panic)|_test.go:[0-9]+:' "$1" | head -"${2:-40}" || true
}

# The goldens a numbered entry wrote with `-update`, as the rule before the cutover said.
goldens='^(TestDaemonAPIVersionGolden|TestStateGolden|TestStateNarrowedRepositoriesGolden|TestAnswersGolden|TestEnumeratedFieldsGolden|TestContractShapesFixture|TestRequestShapesFixture)$'

# write_goldens WHAT writes them in the branch of the current directory, and fails naming WHAT
# where they are not written.
write_goldens() {
  (cd packages/daemon && go test -count=1 ./internal/api/ -update -run "$goldens" >"$scratch/goldens.txt" 2>&1) ||
    { cat "$scratch/goldens.txt"; fail "$1: the goldens were not written"; }
}

# numbered_entry NUMBER WORD [PAUSE_FIELD] adds numbered entry NUMBER to the branch of the current
# directory as an author did before the cutover: its text, a `// NUMBER: ` paragraph above the
# const line of version.go, and the const line raised to NUMBER; its constant
# convert<Word>Contract after the last contract of shaping.go, appended to SupportedContracts;
# where PAUSE_FIELD is given, that field of the body of `POST /legion/v1/project/pause`; its
# manifests; and its goldens. The caller commits it.
numbered_entry() {
  python3 - "$@" <<'PY'
import re, sys

number, word = sys.argv[1:3]
field = sys.argv[3] if len(sys.argv) > 3 else ""
api = "packages/daemon/internal/api"
const = "convert" + word.capitalize() + "Contract"


def edit(path, change):
    with open(path) as f:
        text = f.read()
    new = change(text)
    if new == text:
        sys.exit(f"{path}: the edit found nothing to change")
    with open(path, "w") as f:
        f.write(new)


def once(pattern, replacement, text):
    new, count = re.subn(pattern, replacement, text, count=1)
    if count != 1:
        sys.exit(f"no match of {pattern!r}")
    return new


# The entry's text, below the one before it, and the const line raised to it.
change = f"the field `{field}` of the body of `POST /legion/v1/project/pause`." if field else \
    "which changes nothing of the contract."
edit(f"{api}/version.go", lambda t: once(
    r"\nconst DaemonAPIVersion = \d+\n",
    lambda m: f"\n//\n// {number}: dispatch://INFRA-1569 conversion test -- the entry `{word}` of the conversion test,\n"
              f"// {change}\n"
              f"const DaemonAPIVersion = {number}\n", t))


# The constant, after the last contract of shaping.go, and in SupportedContracts.
def shaping(text):
    last = list(re.finditer(r"^const \w+Contract = \d+\n", text, re.M))[-1]
    return (text[:last.end()] + f"\n// {const} is the daemon contract of version.go's {number}, the entry\n"
            f"// `{word}` of the conversion test.\nconst {const} = {number}\n" + text[last.end():])
edit(f"{api}/shaping.go", shaping)
edit(f"{api}/answers.go", lambda t: once(r"(\nvar SupportedContracts = \[\]int\{[^}]*)\}", rf"\1, {const}}}", t))


# The field, the last of the request's.
def pause(text):
    start = text.index("\ntype ProjectPauseRequest struct {\n")
    close = text.index("\n}\n", start)
    return text[:close + 1] + f'\t{field[0].upper() + field[1:]} string `json:"{field},omitempty"`' + text[close:]
if field:
    edit(f"{api}/pause.go", pause)

for manifest in ("packages/pi-legion/package.json", "packages/claude-envoy/package.json"):
    edit(manifest, lambda t: once(r'("daemonApiVersion": )\d+', r"\g<1>" + number, t))
PY
  gofmt -w "$api/version.go" "$api/shaping.go" "$api/answers.go" ${3:+"$api/pause.go"}
  write_goldens "entry $1"
}

# fixture_of ENTRY_FILE prints the path of the entry's fixture.
fixture_of() {
  sed -E "s|.*/contract_([0-9]{14})_(.*)\\.go\$|$fixtures/contracts/\\1_\\2.json|" <<<"$1"
}

# refused WHAT TEXT... -- OPTION... runs the script with OPTION... on the branch of the current
# directory, and fails unless it exits non-zero, leaves the branch as it was, and prints each TEXT.
refused() {
  local what=$1 at status=0 text texts=()
  shift
  while [ "$1" != -- ]; do texts+=("$1"); shift; done
  shift
  at=$(git rev-parse HEAD)
  bash "$root/scripts/daemon-contract-convert.sh" "$@" >"$scratch/refused.txt" 2>&1 || status=$?
  [ "$status" != 0 ] || { cat "$scratch/refused.txt"; fail "$what exits 0"; }
  [ "$(git rev-parse HEAD)" = "$at" ] && [ -z "$(git status --porcelain)" ] ||
    { cat "$scratch/refused.txt"; fail "$what changed the branch: HEAD $(git rev-parse --short HEAD), want ${at:0:10}"; }
  for text in "${texts[@]}"; do
    grep -qF -- "$text" "$scratch/refused.txt" || { cat "$scratch/refused.txt"; fail "$what is refused, but not with: $text"; }
  done
  echo "refused, exit $status: $(cat "$scratch/refused.txt")"
}

# git's identity is the test's own, so the test needs no configuration of the machine it runs on.
export GIT_AUTHOR_NAME=convert-test GIT_AUTHOR_EMAIL=convert-test@localhost
export GIT_COMMITTER_NAME=convert-test GIT_COMMITTER_EMAIL=convert-test@localhost

api=packages/daemon/internal/api
fixtures=packages/contracts/fixtures/daemon-api
n=$(git show "$base:$api/version.go" | sed -n 's/^const DaemonAPIVersion = \([0-9][0-9]*\)$/\1/p')
[ -n "$n" ] || fail "the base ${base:0:10} declares no const DaemonAPIVersion: give the test a commit from before the cutover"
k=$((n + 1))
echo "this change ${this:0:10}, branched from ${base:0:10}, which declares contract $n"

step "the branch: numbered entry $k by the rule before the cutover"
git worktree add -q -b "$branch" "$repo" "$base"
cd "$repo"
python3 - "$k" <<'PY'
import re, sys

k = sys.argv[1]
api = "packages/daemon/internal/api"


def edit(path, change):
    with open(path) as f:
        text = f.read()
    new = change(text)
    if new == text:
        sys.exit(f"{path}: the edit found nothing to change")
    with open(path, "w") as f:
        f.write(new)


def before_close(text, opener, line):
    """text with line added as the last row of the literal that opens on the line opener starts."""
    start = text.index(opener)
    close = text.index("\n}\n", start)
    return text[:close + 1] + line + "\n" + text[close + 1:]


def once(pattern, replacement, text, flags=0):
    new, count = re.subn(pattern, replacement, text, count=1, flags=flags)
    if count != 1:
        sys.exit(f"no match of {pattern!r}")
    return new


# The entry's text, and the const line raised to it.
edit(f"{api}/version.go", lambda t: once(
    r"\nconst DaemonAPIVersion = \d+\n",
    f"\n//\n// {k}: dispatch://INFRA-1569 conversion test -- the probe entry the conversion test adds:\n"
    "// a band that takes nothing off the state, an answer no route writes, and the word\n"
    "// `convert_probe` of the waiting line's `kind`.\n"
    f"const DaemonAPIVersion = {k}\n", t))

# The constant, after the last contract of shaping.go, and the band.
def shaping(text):
    last = list(re.finditer(r"^const \w+Contract = \d+\n", text, re.M))[-1]
    text = (text[:last.end()] + "\n// convertProbeContract is the daemon contract of version.go's "
            f"{k}, which the conversion test adds.\nconst convertProbeContract = {k}\n" + text[last.end():])
    text = before_close(text, "var stateNarrowings = []stateNarrowing{",
                        '\t{contract: convertProbeContract, state: withoutConvertProbe, dropped: "nothing"},')
    # The new answer is the identity, as each answer that arrived whole is: one more id of the
    # case that lists them.
    return once(r"(\n\tcase AnswerHealth,[^:]*?)(:\n)", r"\1, AnswerConvertProbe\2", text, re.S)
edit(f"{api}/shaping.go", shaping)

with open(f"{api}/convert_probe_band.go", "w") as f:
    f.write("package api\n\n"
            "// withoutConvertProbe is the state as the conversion test's band serves it below its entry: the\n"
            "// entry adds no key, so the band takes nothing and reports no move.\n"
            "func withoutConvertProbe(state State) (State, bool) { return state, false }\n")

def answers(text):
    # The AnswerID, the last of the block that declares every answer's.
    block = text.index("\n\tAnswerState ")
    close = text.index("\n)\n", block)
    text = text[:close + 1] + '\tAnswerConvertProbe AnswerID = "convert-probe"' + text[close:]
    text = before_close(text, "var Answers = []Answer{",
                        f"\t// version.go's {k}: the conversion test's answer, which no route writes and which carries no body.\n"
                        "\t{ID: AnswerConvertProbe, Routes: []string{}, Since: convertProbeContract, NoBody: true},")
    text = once(r"(\nvar SupportedContracts = \[\]int\{[^}]*)\}", r"\1, convertProbeContract}", text)
    # The value, in its sorted place among the waiting line's kinds.
    start = text.index("var lineKinds = []EnumeratedValue{\n")
    close = text.index("\n}\n", start)
    rows = text[start:close].split("\n")[1:]
    row = '\t{Value: "convert_probe", Since: convertProbeContract},'
    at = next(i for i, r in enumerate(rows) if re.search(r'Value: "([^"]*)"', r).group(1) > "convert_probe")
    rows.insert(at, row)
    return text[:start] + "var lineKinds = []EnumeratedValue{\n" + "\n".join(rows) + text[close:]
edit(f"{api}/answers.go", answers)

# The answer's fixture, in the middle of the map: a row of the entry's own, which the conversion
# leaves where the branch wrote it.
edit(f"{api}/contract_shapes_test.go", lambda t: once(
    r"\n(\t\t// The register golden's own value \(TestRegisterResponseGolden\)\.\n)",
    "\n\t\t// The conversion test's answer, which carries no body.\n\t\tAnswerConvertProbe: {},\n\\1", t))

for manifest in ("packages/pi-legion/package.json", "packages/claude-envoy/package.json"):
    edit(manifest, lambda t: once(r'("daemonApiVersion": )\d+', r"\g<1>" + k, t))
PY
gofmt -w "$api/version.go" "$api/shaping.go" "$api/answers.go" "$api/convert_probe_band.go" "$api/contract_shapes_test.go"
write_goldens "the branch's entry $k"
# The branch holds every copy the rule before the cutover asked of an entry.
[ "$(jq '.daemonApiVersion' "$fixtures/version.json")" = "$k" ] || fail "the branch: version.json holds no $k"
[ "$(jq -c '.contract' "$fixtures/state.json")" = "{\"daemon\":$k,\"caller\":$k}" ] || fail "the branch: state.json's marks are not $k"
jq -e --argjson k "$k" 'any(.[]; .since == $k)' "$fixtures/answers.json" >/dev/null || fail "the branch: answers.json has no row since $k"
jq -e --argjson k "$k" 'any(.[].values[]; .since == $k)' "$fixtures/enumerated-fields.json" >/dev/null ||
  fail "the branch: enumerated-fields.json has no value since $k"
jq -e --argjson k "$k" 'any(.[]; .contract == $k)' "$fixtures/contract-shapes.json" >/dev/null || fail "the branch: contract-shapes.json has no record of $k"
jq -e --argjson k "$k" 'any(.[]; .contract == $k)' "$fixtures/request-shapes.json" >/dev/null || fail "the branch: request-shapes.json has no record of $k"
git add -A
git commit -q -m "feat(daemon): the conversion test's entry $k"
echo "branch: $(git diff --name-only "$base" HEAD | tr '\n' ' ')"
# The commit of entry $k, which the stacked branch below starts from.
entry_commit=$(git rev-parse HEAD)

step "the stacked branch: numbered entry $((k + 1)) on the first branch's commit of entry $k"
cd "$root"
git worktree add -q -b "$branch_stacked" "$repo_stacked" "$entry_commit"
cd "$repo_stacked"
numbered_entry "$((k + 1))" stacked
git add -A
git commit -q -m "feat(daemon): the conversion test's entry $((k + 1)), stacked on entry $k"
cd "$repo"

step "a main from before the cutover is refused"
# The base is such a main: its version.go declares `const DaemonAPIVersion`, and its tree defines
# no registerEntry, which the entry files of the conversion call.
refused "the conversion against main at ${base:0:10}, which declares const DaemonAPIVersion," \
  "$base (${base:0:10}) declares \`const DaemonAPIVersion\`" "it has not merged the cutover" -- --base "$base"

step "a ref whose version.go git cannot read is refused"
# This change with no version.go: the script cannot tell whether it holds the cutover, so it
# refuses it, where it would otherwise read it as after the cutover.
GIT_INDEX_FILE=$scratch/index git read-tree "$this"
GIT_INDEX_FILE=$scratch/index git update-index --force-remove "$api/version.go"
no_version=$(git commit-tree "$(GIT_INDEX_FILE=$scratch/index git write-tree)" -p "$this" -m "convert-test: this change with no version.go")
rm -f "$scratch/index"
refused "the conversion against this change with no version.go" \
  "$no_version (${no_version:0:10}) holds no $api/version.go that git can read" -- --base "$no_version"

step "a base branch that has not merged the cutover is refused"
# The first branch, not converted yet, still declares `const DaemonAPIVersion`: the stacked branch
# converts after it.
cd "$repo_stacked"
refused "the conversion of the stacked branch over $branch, not converted yet," \
  "$branch (${entry_commit:0:10}) declares \`const DaemonAPIVersion\`" "it has not merged the cutover" \
  "convert it if it adds a numbered entry, else merge main into it" -- --base "$branch"
cd "$repo"

step "the conversion"
# The control takes out the one line that runs the script, and leaves the branch as it was.
: >"$scratch/convert.txt"
bash "$root/scripts/daemon-contract-convert.sh" --base "$this" >"$scratch/convert.txt" 2>&1 || { cat "$scratch/convert.txt"; fail "scripts/daemon-contract-convert.sh failed"; }
cat "$scratch/convert.txt"

step "the merge of this change"
if ! git merge --no-edit "$this" >"$scratch/merge.txt" 2>&1; then
  cat "$scratch/merge.txt"
  fail "the merge of this change conflicts in: $(git diff --name-only --diff-filter=U | tr '\n' ' ')"
fi
! grep -q CONFLICT "$scratch/merge.txt" || { cat "$scratch/merge.txt"; fail "the merge of this change reports a conflict"; }
git merge-base --is-ancestor "$this" HEAD || fail "the branch does not hold this change after the merge"
echo "merged with no conflict"

step "one entry file, with the entry's text and its band"
mapfile -t entries < <(git diff --name-only --diff-filter=A "$this" HEAD -- "$api" | grep -E '/contract_[0-9]{14}_[a-z0-9-]+\.go$' || true)
[ "${#entries[@]}" -eq 1 ] || fail "the branch adds ${#entries[@]} entry files, want 1: ${entries[*]}"
entry=${entries[0]}
id=$(sed -E 's|.*/contract_([0-9]{14})_.*|\1|' <<<"$entry")
slug=$(sed -E 's|.*/contract_[0-9]{14}_(.*)\.go$|\1|' <<<"$entry")
echo "entry $entry"
grep -qF "// $id $slug: dispatch://INFRA-1569 conversion test -- the probe entry the conversion test adds" "$entry" ||
  fail "$entry does not hold the entry's text"
grep -qE "^const convertProbeContract = $id\$" "$entry" || fail "$entry does not declare convertProbeContract = $id"
grep -qF '{contract: convertProbeContract, state: withoutConvertProbe, dropped: "nothing"},' "$entry" ||
  fail "$entry does not hold the band"
grep -qF '{ID: AnswerConvertProbe, Routes: []string{}, Since: convertProbeContract, NoBody: true},' "$entry" ||
  fail "$entry does not hold the answer"
# The row left in lineKinds is the entry's, and its fixture records it, so TestEntryFixtures
# writes no fixture of the entry while that row is left: the test writes it after the move.
fixture=$(fixture_of "$entry")

step "no edit of a derived copy remains"
for path in "$api/version.go" "$api/shaping.go" packages/pi-legion/package.json packages/claude-envoy/package.json \
  "$fixtures/version.json" "$fixtures/state.json" "$fixtures/state-narrowed-repositories.json" \
  "$fixtures/contract-shapes.json" "$fixtures/request-shapes.json" "$fixtures/answers.json" "$fixtures/enumerated-fields.json"; do
  git diff --quiet "$this" HEAD -- "$path" || { git diff --stat "$this" HEAD -- "$path"; fail "$path differs from this change's"; }
done
left_row='{Value: "convert_probe", Since: convertProbeContract},'
answers_diff=$(git diff -U0 "$this" HEAD -- "$api/answers.go" | grep -E '^[-+][^-+]' || true)
[ "$answers_diff" = "+	$left_row" ] || { echo "$answers_diff"; fail "answers.go differs from this change's by more than the row left behind"; }
echo "each derived copy is this change's"

step "the script names the row it could not move"
named=$(grep -E "^$api/answers\.go:[0-9]+: " "$scratch/convert.txt" | grep -F "$left_row" || true)
[ -n "$named" ] || fail "the script names no file and line of the row it left: $left_row"
line=$(cut -d: -f2 <<<"$named")
[ "$(sed -n "${line}p" "$api/answers.go" | sed 's/^[[:space:]]*//')" = "$left_row" ] ||
  fail "the script names $api/answers.go:$line, which does not hold the row"
echo "$named"

step "TestNoFrozenRowNamesAnEntry fails until the row is in the entry file"
if (cd packages/daemon && go test -count=1 ./internal/api/ -run '^TestNoFrozenRowNamesAnEntry$' >"$scratch/frozen.txt" 2>&1); then
  fail "TestNoFrozenRowNamesAnEntry passes with the row left in answers.go's lineKinds"
fi
excerpt "$scratch/frozen.txt" 5
python3 - "$api/answers.go" "$entry" "$left_row" <<'PY'
import sys

answers, entry, row = sys.argv[1:4]
with open(answers) as f:
    lines = f.read().split("\n")
kept = [line for line in lines if line.strip() != row]
if len(kept) != len(lines) - 1:
    sys.exit(f"{answers}: the row is not there once")
with open(answers, "w") as f:
    f.write("\n".join(kept))
with open(entry) as f:
    text = f.read()
close = text.rindex("\n\t})\n")
moved = ('\t\tenumeratedValues: []enumeratedValues{{answer: AnswerState, keyPath: "issues.*.waitingLine.kind",\n'
         '\t\t\tvalues: []EnumeratedValue{{Value: "convert_probe", Since: convertProbeContract}}}},')
with open(entry, "w") as f:
    f.write(text[:close + 1] + moved + text[close:])
PY
gofmt -w "$api/answers.go" "$entry"
# The fixture: written now, where the conversion wrote none while the row was left, else again,
# with the request part of each entry that the script kept, as the script tells the author to.
requests=$(sed -n 's/^kept the request part of each entry, read from .* at its number, in \(\/.*\)$/\1/p' "$scratch/convert.txt")
[ -f "$requests" ] || fail "the script names no file it kept of the entries' request parts: ${requests:-none}"
jq -e --arg id "$id" '.[$id] == []' "$requests" >/dev/null ||
  fail "$requests gives entry $id, which changes no request, a request part: $(jq -c . "$requests")"
if [ -f "$fixture" ]; then update=-update-entry=$id; else update=-update; fi
(cd packages/daemon && go test -count=1 ./internal/api/ -run '^TestEntryFixtures$' "$update" -entry-requests="$requests" \
  >"$scratch/update-entry.txt" 2>&1) || { cat "$scratch/update-entry.txt"; fail "$update failed after the move"; }
echo "go test ./internal/api -run '^TestEntryFixtures\$' $update -entry-requests=$requests"
git add -A
git commit -q -m "chore(daemon): move the waiting line's convert_probe into entry $id"
(cd packages/daemon && go test -count=1 ./internal/api/ -run '^TestNoFrozenRowNamesAnEntry$' >"$scratch/frozen.txt" 2>&1) ||
  { excerpt "$scratch/frozen.txt" 20; fail "TestNoFrozenRowNamesAnEntry fails after the move"; }
echo "TestNoFrozenRowNamesAnEntry passes after the move"

step "the entry's fixture holds the two since rows"
[ -f "$fixture" ] || fail "no entry fixture $fixture after the move"
jq -e '.answerRows | any(.id == "convert-probe")' "$fixture" >/dev/null ||
  { cat "$fixture"; fail "$fixture holds no row of answers.json for the answer convert-probe"; }
jq -e '.enumeratedRows | any(.answer == "state" and .keyPath == "issues.*.waitingLine.kind" and (.values | index("convert_probe")))' "$fixture" >/dev/null ||
  { cat "$fixture"; fail "$fixture holds no row of enumerated-fields.json for the value convert_probe"; }
echo "$fixture holds the answer's row of answers.json and the value's row of enumerated-fields.json"

step "the contract check and the shape gate pass"
DAEMON_API_CONTRACT_BASE=$this bash .github/scripts/check-daemon-api-contract.sh >"$scratch/check.txt" 2>&1 ||
  { cat "$scratch/check.txt"; fail "the contract check refuses the converted branch"; }
(cd packages/daemon && go test -count=1 ./internal/api/ \
  -run '^(TestEveryAnswerAtEveryContractIsTheShapeItsRecordDescribes|TestTheRecordSetIsOneRecordPerAnswerPerContract|TestEntryFixtures)$' \
  >"$scratch/gate.txt" 2>&1) || { excerpt "$scratch/gate.txt"; fail "the shape gate fails on the converted branch"; }
echo "PASS: entry $k converts to entry $id, merges this change with no conflict, and passes the contract check and the shape gate"

step "the conversion of the stacked branch over its base branch"
cd "$repo_stacked"
bash "$root/scripts/daemon-contract-convert.sh" --base "$branch" >"$scratch/convert-stacked.txt" 2>&1 ||
  { cat "$scratch/convert-stacked.txt"; fail "scripts/daemon-contract-convert.sh --base $branch failed on the stacked branch"; }
cat "$scratch/convert-stacked.txt"

step "the stacked branch adds its own entry alone over its base branch"
# With no rename detection: an entry of the base branch converted again differs from the base
# branch's file by its id alone, which git would read as a rename.
mapfile -t stacked < <(git diff --no-renames --name-only --diff-filter=A "$branch" HEAD -- "$api" | grep -E '/contract_[0-9]{14}_[a-z0-9-]+\.go$' || true)
[ "${#stacked[@]}" -eq 1 ] && [[ ${stacked[0]} == */contract_*_convert-stacked.go ]] ||
  fail "the stacked branch adds ${#stacked[@]} entry files over its base branch $branch, want 1, entry $((k + 1))'s: ${stacked[*]}"
git merge-base --is-ancestor "$branch" HEAD || fail "the stacked branch does not hold its base branch $branch after the conversion"
[[ $(git log -1 --format=%s) == *", and merge $branch" ]] ||
  fail "the stacked branch's conversion commit names no merge of its base branch $branch: $(git log -1 --format=%s)"
stacked_fixture=$(fixture_of "${stacked[0]}")
[ "$(git diff --no-renames --name-only "$branch" HEAD | sort)" = "$(printf '%s\n' "${stacked[0]}" "$stacked_fixture" | sort)" ] ||
  { git diff --stat "$branch" HEAD; fail "the stacked branch differs from its base branch $branch by more than ${stacked[0]} and $stacked_fixture"; }
echo "PASS: the stacked branch converts entry $((k + 1)) alone to ${stacked[0]}, and differs from $branch by that entry's file and fixture"

step "the second branch: numbered entries $k and $((k + 1)), each adding one field to one request"
cd "$root"
git worktree add -q -b "$branch_two" "$repo_two" "$base"
cd "$repo_two"
route='POST /legion/v1/project/pause'
number=$k
for word in first second; do
  numbered_entry "$number" "$word" "convert${word^}"
  git add -A
  git commit -q -m "feat(daemon): the conversion test's entry $number, the $word of two"
  number=$((number + 1))
done
# The branch's records of the request, as `-update` wrote them at each number: the probe's fields.
recorded() {
  jq -c --argjson contract "$1" --arg route "$route" \
    '[.[] | select(.contract == $contract and .route == $route) | .fieldPaths[] | select(startswith("convert"))]' \
    "$fixtures/request-shapes.json"
}
[ "$(recorded "$k")" = '["convertFirst"]' ] || fail "the second branch: its record of $k at $route holds $(recorded "$k"), want convertFirst alone"
[ "$(recorded "$((k + 1))")" = '["convertFirst","convertSecond"]' ] ||
  fail "the second branch: its record of $((k + 1)) at $route holds $(recorded "$((k + 1))"), want convertFirst and convertSecond"
echo "the second branch records convertFirst at $k, and convertFirst and convertSecond at $((k + 1)), at $route"

step "the conversion of the second branch"
bash "$root/scripts/daemon-contract-convert.sh" --base "$this" >"$scratch/convert-two.txt" 2>&1 ||
  { cat "$scratch/convert-two.txt"; fail "scripts/daemon-contract-convert.sh failed on the second branch"; }
cat "$scratch/convert-two.txt"
git merge-base --is-ancestor "$this" HEAD || fail "the second branch does not hold this change after the conversion"

step "each entry's fixture records its own request change alone"
mapfile -t pair < <(git diff --name-only --diff-filter=A "$this" HEAD -- "$api" | grep -E '/contract_[0-9]{14}_[a-z0-9-]+\.go$' | sort || true)
[ "${#pair[@]}" -eq 2 ] || fail "the second branch adds ${#pair[@]} entry files, want 2: ${pair[*]}"
# The ids follow the numbers, so the first file of the two in id order is entry $k's.
[[ ${pair[0]} == */contract_*_convert-first.go && ${pair[1]} == */contract_*_convert-second.go ]] ||
  fail "the entry files are ${pair[*]}: want convert-first's, entry $k's, then convert-second's"
# holds_alone ENTRY_FILE FIELD NUMBER fails unless the fixture of the entry records one request
# change: FIELD added to the route, the change of version.go's NUMBER.
holds_alone() {
  local path
  path=$(fixture_of "$1")
  [ -f "$path" ] || fail "no entry fixture $path after the conversion"
  jq -e --arg route "$route" --arg field "$2" '.routes == [{route: $route, addFieldPaths: [$field]}]' "$path" >/dev/null ||
    fail "$path records the request changes $(jq -c .routes "$path"), want entry $3's own alone: $2 added to $route"
  echo "$path records $2 added to $route, and no other request change"
}
holds_alone "${pair[0]}" convertFirst "$k"
holds_alone "${pair[1]}" convertSecond "$((k + 1))"

step "TestEntryFixtures and the request gate pass over the two fixtures"
(cd packages/daemon && go test -count=1 ./internal/api/ -run '^(TestEntryFixtures|TestEveryRecordedRequestFieldIsReadByName)$' \
  >"$scratch/gate-two.txt" 2>&1) || { excerpt "$scratch/gate-two.txt"; fail "TestEntryFixtures or the request gate fails on the converted second branch"; }
echo "PASS: entries $k and $((k + 1)) convert to two entries, and each one's fixture records its own request change alone"
