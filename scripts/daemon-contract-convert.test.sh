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
# read; on the stacked branch, the first branch, a base branch that has not merged the cutover yet,
# and so with that branch's refusal where the two have two merge bases (the crossed branch: the
# stacked branch with a commit from before the cutover merged, which the first branch merges too);
# and a branch whose entry declares no constant, over a main where another change's entry file
# declares the one the script would name it.
# After the first branch is converted and its row moved, the stacked branch is refused, the same
# way, over a main that took the first branch as a squash: over its merge base with that main the
# stacked branch reads N+1 as its own, and the refusal names the entry file that main holds of it;
# over one that took the first branch with N+1's text reworded, where that text opens no entry file
# and the refusal names the entry's constant, which the entry file declares too;
# and the crossed branch with two merge bases with the first branch, converted, where the refusal
# names both and the way to one: rebased onto the commit it names, with the commit that came in
# through a merge dropped, the crossed branch converts N+2 alone.
# Then the script run on the stacked branch with --base naming the first branch converts N+2 alone
# and merges the first branch, in a commit that names it: the stacked branch then differs from the
# first branch by N+2's entry file and fixture, so N+1, which the first branch converted, is not
# converted a second time under another id. A branch from the same commit whose edit of N+1's
# row of lineKinds makes the merge of the first branch conflict gets its conversion committed
# alone, and the script names the first branch and its commit, for the author to merge by hand.
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
branch_crossed=convert-test-crossed-$$
branch_conflict=convert-test-conflict-$$
branch_rebased=convert-test-rebased-$$
branch_named=convert-test-named-$$
cleanup() {
  git -C "$root" worktree remove --force "$repo" >/dev/null 2>&1 || true
  git -C "$root" worktree remove --force "$repo_two" >/dev/null 2>&1 || true
  git -C "$root" worktree remove --force "$repo_stacked" >/dev/null 2>&1 || true
  git -C "$root" branch -D -q "$branch" "$branch_two" "$branch_stacked" "$branch_crossed" "$branch_conflict" "$branch_rebased" "$branch_named" >/dev/null 2>&1 || true
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

# numbered_text NUMBER LINE... adds numbered entry NUMBER's text to version.go of the branch of the
# current directory as an author did before the cutover: a `// NUMBER: ` paragraph of the LINEs
# above the const line, and the const line raised to NUMBER.
numbered_text() {
  python3 - "$api/version.go" "$@" <<'PY'
import re, sys

path, number, *lines = sys.argv[1:]
with open(path) as f:
    text = f.read()
paragraph = "".join(f"// {line}\n" for line in [f"{number}: {lines[0]}"] + lines[1:])
new, count = re.subn(r"\nconst DaemonAPIVersion = \d+\n",
                     lambda m: f"\n//\n{paragraph}const DaemonAPIVersion = {number}\n", text, count=1)
if count != 1:
    sys.exit(f"{path}: no const line to raise to {number}")
with open(path, "w") as f:
    f.write(new)
PY
}

# numbered_entry NUMBER WORD [PAUSE_FIELD] adds numbered entry NUMBER to the branch of the current
# directory as an author did before the cutover: its text (numbered_text); its constant
# convert<Word>Contract after the last contract of shaping.go, appended to SupportedContracts;
# where PAUSE_FIELD is given, that field of the body of `POST /legion/v1/project/pause`; its
# manifests; and its goldens. The caller commits it.
numbered_entry() {
  local change="which changes nothing of the contract."
  [ -z "${3:-}" ] || change="the field \`$3\` of the body of \`POST /legion/v1/project/pause\`."
  numbered_text "$1" "dispatch://INFRA-1569 conversion test -- the entry \`$2\` of the conversion test," "$change"
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

# added_entry_files REF prints, one a line in name order, each entry file that HEAD adds over REF.
# With no rename detection: an entry converted again differs from REF's file of it by its id alone,
# which git would read as a rename.
added_entry_files() {
  git diff --no-renames --name-only --diff-filter=A "$1" HEAD -- "$api" | grep -E '/contract_[0-9]{14}_[a-z0-9-]+\.go$' | sort || true
}

# stacked_alone REF WHAT prints the entry file that HEAD adds over REF, and fails, naming WHAT,
# unless that is the one file HEAD adds and the stacked entry's (convert-stacked).
stacked_alone() {
  local added
  mapfile -t added < <(added_entry_files "$1")
  [ "${#added[@]}" -eq 1 ] && [[ ${added[0]} == */contract_*_convert-stacked.go ]] ||
    fail "$2 adds ${#added[@]} entry files over $1, want 1, entry $((k + 1))'s: ${added[*]}"
  echo "${added[0]}"
}

# fixture_of ENTRY_FILE prints the path of the entry's fixture.
fixture_of() {
  sed -E "s|.*/contract_([0-9]{14})_(.*)\\.go\$|$fixtures/contracts/\\1_\\2.json|" <<<"$1"
}

# tree_with REV ARG... prints the tree of REV with `git update-index ARG...` applied to it, in an
# index of its own, so the worktree and its index stay as they were.
tree_with() {
  local rev=$1
  shift
  GIT_INDEX_FILE=$scratch/index git read-tree "$rev"
  GIT_INDEX_FILE=$scratch/index git update-index "$@"
  GIT_INDEX_FILE=$scratch/index git write-tree
  rm -f "$scratch/index"
}

# fails WHAT TEXT... -- OPTION... runs the script with OPTION... on the branch of the current
# directory, its output in $scratch/fails.txt, and fails unless it exits non-zero and prints each
# TEXT.
fails() {
  local what=$1 status=0 text texts=()
  shift
  while [ "$1" != -- ]; do texts+=("$1"); shift; done
  shift
  bash "$root/scripts/daemon-contract-convert.sh" "$@" >"$scratch/fails.txt" 2>&1 || status=$?
  [ "$status" != 0 ] || { cat "$scratch/fails.txt"; fail "$what exits 0"; }
  for text in "${texts[@]}"; do
    grep -qF -- "$text" "$scratch/fails.txt" || { cat "$scratch/fails.txt"; fail "$what fails, but not with: $text"; }
  done
  echo "exit $status: $(cat "$scratch/fails.txt")"
}

# refused WHAT TEXT... -- OPTION... runs fails, and fails unless the branch is as it was too.
refused() {
  local at
  at=$(git rev-parse HEAD)
  fails "$@"
  [ "$(git rev-parse HEAD)" = "$at" ] && [ -z "$(git status --porcelain)" ] ||
    fail "$1 changed the branch: HEAD $(git rev-parse --short HEAD), want ${at:0:10}"
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
numbered_text "$k" "dispatch://INFRA-1569 conversion test -- the probe entry the conversion test adds:" \
  "a band that takes nothing off the state, an answer no route writes, and the word" \
  "\`convert_probe\` of the waiting line's \`kind\`."
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
stacked_commit=$(git rev-parse HEAD)
# A commit from before the cutover, which the stacked branch merges apart from the first branch, as
# a branch merges main: the crossed branch. Where the first branch merges it too, the two have two
# merge bases, that commit and the first branch's commit of entry $k.
side_blob=$(printf 'convert-test: a commit both branches merge\n' | git hash-object -w --stdin)
side_file=(--add --cacheinfo "100644,$side_blob,convert-side.txt")
side=$(git commit-tree "$(tree_with "$base" "${side_file[@]}")" -p "$base" -m "convert-test: a commit before the cutover")
crossed=$(git commit-tree "$(tree_with "$stacked_commit" "${side_file[@]}")" -p "$stacked_commit" -p "$side" -m "convert-test: the stacked branch merges it")
git branch "$branch_crossed" "$crossed"
cd "$repo"

step "a main from before the cutover is refused"
# The base is such a main: its version.go declares `const DaemonAPIVersion`, and its tree defines
# no registerEntry, which the entry files of the conversion call.
refused "the conversion against main at ${base:0:10}, which declares const DaemonAPIVersion," \
  "$base (${base:0:10}) declares \`const DaemonAPIVersion\`" "it has not merged the cutover" -- --base "$base"

step "a ref whose version.go git cannot read is refused"
# This change with no version.go: the script cannot tell whether it holds the cutover, so it
# refuses it, where it would otherwise read it as after the cutover.
no_version=$(git commit-tree "$(tree_with "$this" --force-remove "$api/version.go")" -p "$this" -m "convert-test: this change with no version.go")
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

step "a base branch that has not merged the cutover is refused so, whatever its merge bases"
# The first branch, not converted yet, merges the commit the crossed branch merges: the two have
# two merge bases. What the author does first is to convert the first branch, so that is the
# refusal; the advice for two merge bases names the first branch as it was before it merged the
# cutover, which it has not done.
cd "$repo_stacked"
side_base=$(git commit-tree "$(tree_with "$entry_commit" "${side_file[@]}")" -p "$entry_commit" -p "$side" -m "convert-test: the first branch, not converted yet, merges it")
git checkout -q "$branch_crossed"
refused "the conversion of the crossed branch over the first branch, not converted yet, with two merge bases," \
  "$side_base (${side_base:0:10}) declares \`const DaemonAPIVersion\`" "it has not merged the cutover" \
  -- --base "$side_base"
git checkout -q "$branch_stacked"
cd "$repo"

step "an entry with no constant is refused where an entry file of the ref declares the one it would get"
# A branch from the base adds numbered entry $k with no constant of its own, whose text opens with
# dispatch://INFRA-1569, so the script would name its constant infra1569Contract. Over a main where
# another change's entry file declares that name, the second declaration would not compile.
cd "$repo_stacked"
git checkout -q -b "$branch_named" "$base"
numbered_text "$k" "dispatch://INFRA-1569 conversion test -- an entry with no constant of its own."
git commit -q -am "feat(daemon): the conversion test's entry $k, with no constant"
named_file=$api/contract_20000101000000_infra-1569.go
named_blob=$(printf '%s\n' "package api" "" \
  "// 20000101000000 infra-1569: dispatch://INFRA-1569 conversion test -- another change's entry, which took the name." \
  "const infra1569Contract = 20000101000000" "" "func init() {" \
  '	registerEntry(contractEntry{id: infra1569Contract, slug: "infra-1569"})' "}" | git hash-object -w --stdin)
named_main=$(git commit-tree "$(tree_with "$this" --add --cacheinfo "100644,$named_blob,$named_file")" -p "$this" \
  -m "convert-test: this change, with another change's entry file that declares infra1569Contract")
refused "the conversion of an entry with no constant over a main whose entry file declares the one it would get" \
  "entry $k of this branch declares no constant, and the one this script names it, infra1569Contract, $named_file of $named_main declares" \
  "Or another change took that name: give the entry a constant of another name in the branch" -- --base "$named_main"
git checkout -q "$branch_stacked"
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
mapfile -t entries < <(added_entry_files "$this")
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

step "a stacked branch whose base branch main took as a squash is refused"
# main takes the first branch, converted, as a squash, and GitHub retargets the stacked branch to
# main. Over its merge base with that main, from before the cutover, the stacked branch would read
# entry $k as its own, though that main holds it as the first branch's entry file.
cd "$repo_stacked"
squash=$(git commit-tree "$branch^{tree}" -p "$this" -m "convert-test: the first branch, taken as a squash")
refused "the conversion of the stacked branch over a main that took its base branch as a squash" \
  "entry $k of this branch is $entry of $squash" "--base naming the base branch's last commit before the squash" \
  -- --base "$squash"

step "a stacked branch whose base branch main took reworded as a squash is refused by the entry's constant"
# The first branch's entry text changes after its conversion, in review, and main takes it so as a
# squash. The stacked branch, which merged the first branch before that, still carries the old
# text, which then opens no entry file of that main; the constant convertProbeContract, which its
# entry declares, is the one the entry file declares. A constant main declares can also be another
# change's, which took the same name, so the refusal names both causes and what to do for each.
old_text=$(git rev-parse "$branch:$entry")
new_text=$(git show "$branch:$entry" | sed '0,/the probe entry the conversion test adds/s//the probe entry, reworded in review, that the conversion test adds/' | git hash-object -w --stdin)
[ "$new_text" != "$old_text" ] || fail "the reword changed nothing of $entry"
reworded=$(git commit-tree "$(tree_with "$branch" --cacheinfo "100644,$new_text,$entry")" -p "$this" -m "convert-test: the first branch, reworded and taken as a squash")
refused "the conversion of the stacked branch over a main that took its base branch reworded as a squash" \
  "entry $k of this branch declares convertProbeContract, which $entry of $reworded declares too" \
  "Either $reworded took that entry from a base branch as a squash after the cutover" \
  "--base naming the base branch's last commit before the squash" \
  "Or another change took that name: rename the entry's constant in the branch" -- --base "$reworded"

step "a branch with two merge bases with its base branch is refused"
# The first branch, converted, merges the commit the crossed branch merges: the crossed branch's
# merge bases with it are then that commit and the first branch's commit of entry $k, and the one
# git prints would decide which entries the crossed branch reads as its own.
base_crossed=$(git commit-tree "$(tree_with "$branch" "${side_file[@]}")" -p "$branch" -p "$side" -m "convert-test: the first branch merges it")
git checkout -q "$branch_crossed"
refused "the conversion of the crossed branch with two merge bases with its base branch" \
  "2 merge bases" "${entry_commit:0:10}" "${side:0:10}" \
  "git rebase -i" "drop each commit that $base_crossed holds" \
  "cherry-pick the branch's own commits onto a branch made at that commit" -- --base "$base_crossed"

step "the way out of two merge bases that the refusal names leads to one"
# The refusal names the first branch as it was before it merged the cutover, the first parent of
# its conversion: its commit of entry $k, which the crossed branch holds already, so a merge of it
# leaves the two merge bases. `git rebase -i` onto it, with the commit that came in through the
# merge dropped, leaves the crossed branch its own commit alone on it; that commit is then the one
# merge base, and the conversion over the first branch converts entry $((k + 1)) alone.
git checkout -q -b "$branch_rebased" "$branch_crossed"
GIT_SEQUENCE_EDITOR="sed -i '/convert-test: a commit before the cutover\$/s/^pick /drop /'" git rebase -q -i "$entry_commit" ||
  fail "the rebase of the crossed branch onto ${entry_commit:0:10}, its merged commit dropped, failed"
[ "$(git merge-base --all HEAD "$base_crossed")" = "$entry_commit" ] ||
  fail "the rebased crossed branch has merge bases $(git merge-base --all HEAD "$base_crossed" | tr '\n' ' ')with the first branch, want ${entry_commit:0:10} alone"
bash "$root/scripts/daemon-contract-convert.sh" --base "$base_crossed" >"$scratch/convert-rebased.txt" 2>&1 ||
  { cat "$scratch/convert-rebased.txt"; fail "scripts/daemon-contract-convert.sh --base ${base_crossed:0:10} failed on the rebased crossed branch"; }
rebased=$(stacked_alone "$base_crossed" "the rebased crossed branch")
echo "PASS: rebased onto ${entry_commit:0:10} with the merged commit dropped, the crossed branch has one merge base, and converts entry $((k + 1)) alone to $rebased"
git checkout -q "$branch_stacked"

step "the conversion of the stacked branch over its base branch"
cd "$repo_stacked"
bash "$root/scripts/daemon-contract-convert.sh" --base "$branch" >"$scratch/convert-stacked.txt" 2>&1 ||
  { cat "$scratch/convert-stacked.txt"; fail "scripts/daemon-contract-convert.sh --base $branch failed on the stacked branch"; }
cat "$scratch/convert-stacked.txt"

step "the stacked branch adds its own entry alone over its base branch"
stacked=$(stacked_alone "$branch" "the stacked branch")
git merge-base --is-ancestor "$branch" HEAD || fail "the stacked branch does not hold its base branch $branch after the conversion"
[[ $(git log -1 --format=%s) == *", and merge $branch" ]] ||
  fail "the stacked branch's conversion commit names no merge of its base branch $branch: $(git log -1 --format=%s)"
# The body names the ref merged and its commit, the base branch's, which is not main's.
body=$(git log -1 --format=%b | tr '\n' ' ')
[[ $body == *"and merged $branch ($(git rev-parse "$branch")) into the branch."* ]] ||
  fail "the stacked branch's conversion commit does not say it merged $branch at $(git rev-parse --short "$branch"): ${body:0:300}"
stacked_fixture=$(fixture_of "$stacked")
[ "$(git diff --no-renames --name-only "$branch" HEAD | sort)" = "$(printf '%s\n' "$stacked" "$stacked_fixture" | sort)" ] ||
  { git diff --stat "$branch" HEAD; fail "the stacked branch differs from its base branch $branch by more than $stacked and $stacked_fixture"; }
echo "PASS: the stacked branch converts entry $((k + 1)) alone to $stacked, and differs from $branch by that entry's file and fixture"

step "a merge of the base branch that conflicts is left to the author, with the ref and its commit"
# A branch made from the first branch's commit of entry $k adds numbered entry $((k + 1)) and edits
# the row of lineKinds that the first branch moved into its entry file after its conversion, so
# the merge of the first branch conflicts in answers.go. The script commits the conversion alone,
# and says which ref to merge by hand and at which commit, the one it read.
git checkout -q -b "$branch_conflict" "$entry_commit"
numbered_entry "$((k + 1))" conflict
sed -i 's/{Value: "convert_probe", Since: convertProbeContract},/{Value: "convert_probe_edited", Since: convertProbeContract},/' "$api/answers.go"
grep -qF '"convert_probe_edited"' "$api/answers.go" || fail "the conflicting branch: the edit of lineKinds' row found nothing"
git add -A
git commit -q -m "feat(daemon): the conversion test's entry $((k + 1)), which edits the row entry $k moved"
conflict_commit=$(git rev-parse HEAD)
branch_at=$(git rev-parse "$branch")
fails "the conversion whose merge of $branch conflicts" \
  "the merge of $branch conflicts in:" "  $api/answers.go" "merge $branch (${branch_at:0:10}) by hand" -- --base "$branch"
[ "$(git rev-parse HEAD^)" = "$conflict_commit" ] && ! git rev-parse -q --verify HEAD^2 >/dev/null && [ -z "$(git status --porcelain)" ] ||
  fail "the conversion whose merge of $branch conflicts is not committed alone on the branch"
echo "PASS: the merge of $branch conflicts in $api/answers.go; the conversion is committed alone, and the script says: $(grep -F 'by hand' "$scratch/fails.txt")"
git checkout -q "$branch_stacked"

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
mapfile -t pair < <(added_entry_files "$this")
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
