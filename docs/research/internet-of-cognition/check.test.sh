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

# The fixture builder and runner are check.sh's own; sourcing check.sh defines them and runs
# nothing, because its call to main runs only when it is executed, so nothing of the gate runs
# here except through fixture_run.
# shellcheck disable=SC1090
. "$HERE/check.sh"

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

# 7. The live address of a published copy is never read. The check reads: "where the live
# address answers, reports whether the live page still carries that sentence; a live page
# whose bytes have changed is reported as changed rather than failed", and --help says curl
# "reads a published page's live address". At bcc01d05ca no line of check.sh fetches one,
# so --citations says nothing of any live page. The case asks only that the run names the
# fixture's address with what it found there, whatever that is: answered, changed or
# unreachable all count, so it needs no network.
dir="$(fresh live-address)"
out="$(fixture_run "$dir" --citations)"
if printf '%s\n' "$out" | grep -F 'https://example.org/page' | grep -qiE 'still carries|changed|unreachable|does not answer|no longer'; then
    CASES_PASSED=$((CASES_PASSED + 1))
    printf 'PASS %s\n' "--citations reports the live state of each published copy"
else
    CASES_FAILED=$((CASES_FAILED + 1))
    printf 'FAIL %s: no line of the run names https://example.org/page and what its live page holds\n' "--citations reports the live state of each published copy"
fi

# 8. A landed page that names a commit of the base branch which a forward merge brought in.
# Such a commit is an ancestor of the head and not of the commit our code was read at, so a
# range from that commit alone takes it for a commit of this pull request; a forward merge
# of main into the branch of dispatch://INFRA-3134 made every commit main gained one.
dir="$(fresh landed-merged-base)"
merged="$(
    cd "$dir/repo" &&
        git checkout --quiet -b moved "$(cat "$dir/base")" &&
        git commit --quiet --allow-empty -m "a commit the base branch gained" &&
        git rev-parse HEAD &&
        git checkout --quiet - &&
        git merge --quiet --no-edit moved >/dev/null
)"
sed -i "s/^- \*\*Landed:\*\* [0-9a-f]*/- **Landed:** $merged/" "$dir/judgement.md"
sed -i "s/$(cat "$dir/rows")/$merged/" "$dir/repo/docs/research/internet-of-cognition/sources.json"
case_result "a landed page naming a base commit a forward merge brought in is not a commit of this pull request" landed-proof "$(fixture_run "$dir" --landed)"

# 9. A recorded command that runs a package with no version. The row ts-shapes-graph ran
# `uv run --no-project --with rdflib`, which reads whatever rdflib release is newest while
# env.sh pins rdflib==7.6.0, and its run passed because the newest release was the pinned
# one that day. The command here never runs the package, so only the gate's reading of the
# command can catch it.
dir="$(fresh unpinned-runner)"
sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || uv run --no-project --with rdflib python -c 1; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
case_result "a recorded command that runs a package with no version is refused" install-pinned "$(fixture_run "$dir" --replay)"

# 10. `@latest` is no version. install-pinned takes any `@` after a uvx or go run package
# for a version, so `uvx pyshacl@latest` and `go run <module>@latest`, the two spellings
# that ask for the newest release by name, passed it at 77b8e9234f.
dir="$(fresh latest-is-no-version)"
sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || uvx pyshacl@latest --version; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
case_result "uvx <package>@latest is refused as a package with no version" install-pinned "$(fixture_run "$dir" --replay)"
dir="$(fresh go-latest-is-no-version)"
sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || go run golang.org\\/x\\/tools\\/cmd\\/stringer@latest; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
case_result "go run <module>@latest is refused as a package with no version" install-pinned "$(fixture_run "$dir" --replay)"

# 11. `uv tool run` is the command uvx is an alias of, and install-pinned does not read it,
# so `uv tool run pyshacl` passed it at 77b8e9234f.
dir="$(fresh uv-tool-run)"
sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || uv tool run pyshacl --version; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
case_result "uv tool run <package> with no version is refused" install-pinned "$(fixture_run "$dir" --replay)"

# 12. A pinned uvx with an option before its package is no fault. install-pinned reads the
# word after uvx as the package, so `uvx --python 3.12 pyshacl==0.40.2` was refused at
# 77b8e9234f as `uvx --python`, which would stop a pinned command from being recorded.
dir="$(fresh uvx-option-pinned)"
sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || uvx --python 3.12 pyshacl==0.40.2 --version; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(fixture_run "$dir" --replay)"
if printf '%s\n' "$out" | grep -q '^FAIL install-pinned'; then
    CASES_FAILED=$((CASES_FAILED + 1))
    printf 'FAIL %s: %s\n' "a pinned uvx with an option before its package passes install-pinned" "$(printf '%s\n' "$out" | grep '^FAIL install-pinned' | head -1)"
else
    CASES_PASSED=$((CASES_PASSED + 1))
    printf 'PASS %s\n' "a pinned uvx with an option before its package passes install-pinned"
fi

# 13. A runner inside a shell string is not read. unpinned_installs splits a command into
# words, so `sh -c 'uvx pyshacl --version'` is the two words `sh -c` and one quoted word,
# and the uvx inside it is never seen; it passed install-pinned at 828d080931, against the
# header's own rule that what the reader cannot read it refuses.
dir="$(fresh runner-in-shell-string)"
sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || sh -c 'uvx pyshacl --version'; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
case_result "a runner inside sh -c is read, or refused as unread" install-pinned "$(fixture_run "$dir" --replay)"

# 14. A range is no concrete version. The header says a version is a concrete one, and
# `==0.*` (a Python wildcard), `@1` (an npm major range) and `@v0` (a Go module query for
# the newest v0) each name the newest release inside a range; all three passed
# install-pinned at 828d080931.
for form in 'uvx pyshacl==0.* --version' 'npx cowsay@1' 'go run golang.org\/x\/tools\/cmd\/stringer@v0'; do
    dir="$(fresh "range-$(printf '%s' "$form" | tr -c 'a-z0-9' '-' | cut -c1-24)")"
    sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || $form; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    case_result "a range is refused as no version: $(printf '%s' "$form" | tr -d '\\')" install-pinned "$(fixture_run "$dir" --replay)"
done

# 15. The reader fails closed: a form no case above names is refused, not passed. A
# compatible-release clause, a comparison, an npm caret range and a dist-tag are none of
# the versions the rule takes, and a runner inside a command substitution inside a shell
# string is still a runner. A reader that refuses only a list of known faults passes each
# of these; one that takes a version only when it is a full release refuses them all.
# The last three passed the gate at 828d080931: a wildcard after a full prefix, an npm x
# range and a string eval runs.
for form in 'uvx pkg~=1.2' 'uvx \\"pkg>=1.2\\"' 'npx cowsay@^1.2.3' 'bunx tsx@next' "sh -c \\\\\"echo \$(npx cowsay)\\\\\"" \
    'uvx pkg==1.2.*' 'npx cowsay@1.x' 'eval \\"uvx pyshacl\\"'; do
    dir="$(fresh "unnamed-$(printf '%s' "$form" | tr -c 'a-z0-9' '-' | cut -c1-24)")"
    sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || $form; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    case_result "a form no rule names is refused: $(printf '%s' "$form" | tr -d '\\')" install-pinned "$(fixture_run "$dir" --replay)"
done

# 16-18. A command the reader cannot resolve passes it at 03bd226bb0, against the header's
# rule that any form it does not recognise fails the row. Each one runs code the reader
# never sees:
#   16. a shell given a variable as its command, `sh -c "$CMD"`: the reader reads the
#       string `$CMD` and finds no runner in it;
#   17. python started through the deliverable's own interpreter variable, as hundreds of
#       rows start it, `$IOC_PY -c '... os.system(...)'`: the python -c rule reads only a
#       word whose name is python, so code that starts a process passes;
#   18. a runner named by a variable, `{ R=uvx; $R pyshacl; }`: the reader looks for the
#       word uvx and finds `$R`. The group keeps the `true ||` in front of all of it, so
#       nothing of it runs.
for form in 'sh -c \\"$CMD\\"' "\$IOC_PY -c 'import os; os.system(0)'" '{ R=uvx; $R pyshacl; }'; do
    dir="$(fresh "unresolved-$(printf '%s' "$form" | tr -c 'a-z0-9' '-' | cut -c1-24)")"
    sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || $form; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    grep -qF "$(printf '%s' "$form" | tr -d '\\' | cut -c1-6)" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
        printf 'FAIL the form %s could not be planted\n' "$form"
        exit 1
    }
    case_result "a command the reader cannot resolve is refused: $(printf '%s' "$form" | tr -d '\\')" install-pinned "$(fixture_run "$dir" --replay)"
done

total=$((CASES_PASSED + CASES_FAILED))
if [ "$CASES_FAILED" -eq 0 ]; then
    printf 'all %d cases passed\n' "$total"
    exit 0
fi
printf '%d of %d cases failed\n' "$CASES_FAILED" "$total"
exit 1
