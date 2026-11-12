#!/usr/bin/env bash
#
# check.test.sh holds the faults the tester of dispatch://INFRA-3134 found the gate passing.
# Each case builds the self-test's own fixture deliverable, plants one fault the gate's
# assertion table does not plant, runs the mode that should refuse it, and fails when that
# mode still ends with its all-passed line.
#
#   bash docs/research/internet-of-cognition/check.test.sh
#
# It prints one PASS or FAIL line per case and a last line reading `all N cases passed`.
# It needs what check.sh --self-test needs: git, jq and python3.

set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export IOC_EVIDENCE="$HERE"
export IOC_ROOT="$(cd "$HERE/../../.." && pwd)"

# The fixture builder and runner are check.sh's own; every line but its last call to main is
# read, so nothing of the gate runs here except through fixture_run.
# shellcheck disable=SC1090
. <(sed '$d' "$HERE/check.sh")

CASES_PASSED=0
CASES_FAILED=0

case_result() {
    # case_result <name> <family the gate should name> <gate output>
    local name="$1" family="$2" out="$3"
    if printf '%s\n' "$out" | tail -1 | grep -q '^all [0-9]* assertions passed$'; then
        CASES_FAILED=$((CASES_FAILED + 1))
        printf 'FAIL %s: the gate passed it: %s\n' "$name" "$(printf '%s\n' "$out" | tail -1)"
    elif ! printf '%s\n' "$out" | grep -qE "^FAIL ($family)"; then
        CASES_FAILED=$((CASES_FAILED + 1))
        printf 'FAIL %s: the gate failed but named no %s: %s\n' "$name" "$family" "$(printf '%s\n' "$out" | grep '^FAIL' | head -2 | tr '\n' ' ')"
    else
        CASES_PASSED=$((CASES_PASSED + 1))
        printf 'PASS %s\n' "$name"
    fi
}

WORK="$(mktemp -d "${TMPDIR:-/tmp}/ioc-check-test.XXXXXX")"
trap 'rm -rf "$WORK" "$RUN_SCRATCH"' EXIT
fixture_build "$WORK/base" >/dev/null 2>&1

fresh() {
    # fresh <name>; a copy of the fixture to break, as --self-test copies it.
    local dir="$WORK/$1"
    mkdir -p "$dir"
    cp -r "$WORK/base/repo" "$dir/repo"
    cp "$WORK/base/judgement.md" "$WORK/base/base" "$WORK/base/rows" "$dir/"
    printf '%s' "$dir"
}

# 1. A row that carries a command and a recorded output but no predicate. claims.jsonl has
# eight such rows (tc-tol-4, tc-install-1 and tc-voc-1 among them): --replay runs the
# command and compares nothing, so a recorded output the command no longer prints passes.
dir="$(fresh no-predicate)"
printf '%s\n' '{"id":"f-behaviour-2","requirement":"r7","source":"this tree'"'"'s measurement","kind":"behaviour","claim":"The script prints ran once.","mark":"verified","evidence":{"surface":"cli"},"command":"printf '"'"'ran twice\\n'"'"'; exit 3","output":"ran once","note":"ran again"}' \
    >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
case_result "a row with a command and no predicate is held to its recorded output" 'replay-command|predicate-form' "$(fixture_run "$dir" --replay)"

# 2. An absence search whose corpus was never read. A Dispatch or GitHub read that fails
# prints its refusal on stdout and exits non-zero; the grep after it filters the refusal
# out and exits 1, so the row reads as "still finds nothing". n-no-finding-kind does
# exactly this with DISPATCH_URL unreachable: no output, exit 1.
dir="$(fresh absence-unread)"
sed -i 's#"command":"grep -r '"'"'REVIEWER_PAIR_REQUIRED'"'"' skills || true","expect":{"at_most":"0"}#"command":"{ printf '"'"'Dispatch is unreachable\\n'"'"'; exit 1; } | grep -n '"'"'REVIEWER_PAIR_REQUIRED'"'"'","expect":{"exit":1}#' \
    "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
grep -q 'Dispatch is unreachable' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
    printf 'FAIL the absence fault could not be planted\n'
    exit 1
}
case_result "an absence search whose corpus read failed does not pass as still empty" absence-still-empty "$(fixture_run "$dir" --replay)"

# 3. A landed page that names a commit from before the pull request. landed-proof asks
# `merge-base --is-ancestor <commit> HEAD`, which every commit of the base branch meets.
dir="$(fresh landed-base)"
sed -i "s/^- \*\*Landed:\*\* [0-9a-f]*/- **Landed:** $(cat "$dir/base")/" "$dir/judgement.md"
case_result "a landed page naming a commit of the base branch is not a commit of this pull request" landed-proof "$(fixture_run "$dir" --landed)"

# 4. A citation into one of the projects whose quoted text the file does not hold. The
# project-code branch of --citations asks only that the cited path exists at the pinned
# commit, so 266 rows are resolved by path alone; 102 of them carry no command either.
# i-scope-drift with its symbol changed to a name scope's processor.py does not hold
# passed --citations at fc100bb69a.
dir="$(fresh project-text)"
sed -i 's#"id":"f-project-1","requirement":"r3","source":"mas-lab","kind":"project-code","claim":"The runtime declares a boundary gate.","mark":"verified","evidence":{"repo":"mas-lab","commit":"\([0-9a-f]*\)","file":"README.md","symbol":"README"}#"id":"f-project-1","requirement":"r3","source":"mas-lab","kind":"project-code","claim":"The runtime declares a boundary gate.","mark":"verified","evidence":{"repo":"mas-lab","commit":"\1","file":"README.md","symbol":"NoTextTheReadmeHolds3134"}#' \
    "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
grep -q 'NoTextTheReadmeHolds3134' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
    printf 'FAIL the project-text fault could not be planted\n'
    exit 1
}
case_result "a project citation whose quoted text the file does not hold is refused" citation-project-code "$(fixture_run "$dir" --citations)"

# 5. A committed copy of a published page whose bytes changed outside the quoted sentence.
# The check reads "the script compares the copy's checksum"; at fc100bb69a sources.json
# recorded no checksum of any copy and --citations computed none, and the planted fault the
# self-test named "a committed copy whose checksum does not match" was a changed sentence.
dir="$(fresh published-bytes)"
printf 'a byte the fetched page did not carry\n' >>"$dir/repo/docs/research/internet-of-cognition/published/page.txt"
case_result "a committed copy whose bytes changed fails its checksum" citation-published "$(fixture_run "$dir" --citations)"

# 6. An absence search over two reads in one brace group, the first of which failed. The
# group's status is its last command's, so PIPESTATUS reports 0 for the stage that holds
# the failed read and the search after it reads only half its corpus. n-no-finding-kind
# has this shape: with its first read set to `--version 9999`, `dispatch doc-read` prints
# `not found` on stdout, the second read succeeds, the stages exit `0 1` and the row passed
# as still empty at bcc01d05ca.
dir="$(fresh absence-half-read)"
sed -i 's#"command":"grep -r '"'"'REVIEWER_PAIR_REQUIRED'"'"' skills || true","expect":{"at_most":"0"}#"command":"{ sh -c '"'"'echo not found; exit 1'"'"'; printf '"'"'a line of the second corpus\\n'"'"'; } | grep -n '"'"'REVIEWER_PAIR_REQUIRED'"'"'","expect":{"exit":1}#' \
    "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
grep -q 'a line of the second corpus' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
    printf 'FAIL the half-read absence fault could not be planted\n'
    exit 1
}
case_result "an absence search whose first of two reads failed does not pass as still empty" absence-still-empty "$(fixture_run "$dir" --replay)"

total=$((CASES_PASSED + CASES_FAILED))
if [ "$CASES_FAILED" -eq 0 ]; then
    printf 'all %d cases passed\n' "$total"
    exit 0
fi
printf '%d of %d cases failed\n' "$CASES_FAILED" "$total"
exit 1
