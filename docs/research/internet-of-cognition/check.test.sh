#!/usr/bin/env bash
#
# check.test.sh holds the faults the tester of dispatch://INFRA-3134 found the gate passing.
# Each case builds the self-test's own fixture deliverable, plants one fault the gate's
# assertion table does not plant, runs the mode that should refuse it, and fails when that
# mode still ends with its all-passed line.
#
#   bash docs/research/internet-of-cognition/check.test.sh
#
# It prints one PASS or FAIL line per case, or COULD NOT RUN for a case whose run of the
# gate could not run, and ends by the three endings check.sh's header defines: `all N cases
# passed`, exit 0; `F of N cases failed`, exit 1, naming after the count each case that
# could not run; or, with no failure, `check.sh cannot run check.test.sh: ...`, exit 2,
# naming each case that could not run and its run's reason. N counts every case, the ones
# that could not run among them. Like every run of a mode, it writes its ending record
# (check.sh's header, "The ending record"), the cases being what it declares, through
# check.sh's run_ending, and --self-test reads it by that record.
# It needs what check.sh --self-test needs, which `check.sh --help` prints: the lines of
# --self-test and of the five modes it runs over its fixture, --replay, --shape, --citations,
# --landed and --published. A case that needs uv or go prints SKIP when that tool is missing.

set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export IOC_EVIDENCE="$HERE"
export IOC_ROOT="$(cd "$HERE/../../.." && pwd)"

# The fixture builder and runner are check.sh's own; sourcing check.sh defines them and runs
# nothing, because its call to main runs only when it is executed, so nothing of the gate runs
# here except through fixture_run.
# shellcheck disable=SC1090
. "$HERE/check.sh"

# Every case reads the run of the gate it judges in one way, case_read, which every helper
# below calls first, by the one rule of check.sh's header ("The ending record"): from the
# run's own ending record, read by check.sh's ending_read, never from its exit status alone
# or its text. fixture_run and gate_run print the directory of the run they made, which
# holds its text, its record and its status, and a case hands that directory, its own
# run's, to its helper, so no two runs share one record. Each case ends by case_passed,
# case_failed or case_unrun, which print its line and write it into this file's own ending
# record; the summary at the end writes the record by run_ending.
#
# case_passed <name>; case_failed <name> [<why>]; case_unrun <name> <kind> <reason>.
case_passed() {
    printf 'PASS %s\n' "$1"
    ending_add declared "$1"
    ending_add passed "$1"
}
case_failed() {
    printf 'FAIL %s%s\n' "$1" "${2:+: $2}"
    ending_add declared "$1"
    ending_add failed "$1"
}
case_unrun() {
    printf 'COULD NOT RUN %s: %s\n' "$1" "$3"
    ending_add declared "$1"
    ending_add unrun "$1" "$2" "$3"
}
# case_read <name> <run> [<planted reason, an extended regular expression>]; reads the run
# by ending_read, which leaves RUN_* set, and settles the case there, returning non-zero,
# when the run crashed, which fails the case, or when an assertion or a case of the run
# could not run for a reason that does not match <planted>: the case could not run when
# that reason is from outside the tree, of the kind prerequisite, environment or source,
# whatever the run's exit status, and failed when it is of the kind gate or evidence, the
# tree's own.
case_read() {
    local name kind reason
    ending_read "$2"
    if [ "$RUN_CRASH" = 1 ]; then
        case_failed "$1" "$RUN_LINE"
        return 1
    fi
    while IFS=$'\t' read -r name kind reason; do
        [ -n "$name" ] || continue
        [ -n "${3:-}" ] && printf '%s\n' "$reason" | grep -qE -- "$3" && continue
        case "$kind" in
        prerequisite | environment | source) case_unrun "$1" "$kind" "$reason" ;;
        *) case_failed "$1" "its run could not run for a reason of the tree it reads, of the kind ${kind:-none}, and no reason from outside it: $reason" ;;
        esac
        return 1
    done <<<"$RUN_UNRUN"
    return 0
}

# Each helper below decides pass, fail or could not run from the run's record alone, by
# case_read and RUN_*, and reads the run's text only for the detail of a FAIL, which the
# record does not hold: the FAIL lines it quotes in its own failure, the detail a case
# names after a family and a colon, and the number of FAIL lines one_failure_result asks
# for.
#
# case_result <name> <family the gate should name> <run> [<family> <run>]...; a planted
# fault, which passes when the run's record names that family among the assertions that
# failed (RUN_FAILED); a fault two modes each refuse gives each run after the family its
# record must name, and the runs are read in their order, the first that does not name its
# family deciding the case. <family> is an extended regular expression over the names, as
# 'replay-command|predicate-form'; a case that also asks what the FAIL said writes that
# after the name and a colon, as "replay-command: f-behaviour-1 ran ...", and that detail
# is read from the run's text alone, as a FAIL line of the family that matches it.
case_result() {
    local name="$1" family detail run
    shift
    while [ "$#" -ge 2 ]; do
        family="${1%%:*}" detail="$1" run="$2"
        shift 2
        case_read "$name" "$run" || return 0
        if [ "$RUN_ENDING" = passed ]; then
            case_failed "$name" "the gate passed it: $RUN_LINE"
            return 0
        elif ! printf '%s' "$RUN_FAILED" | grep -qxE -- "$family"; then
            case_failed "$name" "the gate failed but named no $family, only $(printf '%s' "$RUN_FAILED" | tr '\n' ' ')$(grep '^FAIL' "$run/text" | head -2 | tr '\n' ' ')$RUN_LINE"
            return 0
        elif [ "$detail" != "$family" ] && ! grep -qE -- "^FAIL ($detail)" "$run/text"; then
            case_failed "$name" "the gate failed $family, and no FAIL line of it reads $detail: $(grep -E "^FAIL ($family)" "$run/text" | head -2 | tr '\n' ' ')$RUN_LINE"
            return 0
        fi
    done
    case_passed "$name"
}

# honest_result <name> <family> <run>; the honest form beside a planted fault, which passes
# when the gate ran and its record names no failure of that family (RUN_FAILED); the text
# gives the FAIL line its failure quotes.
honest_result() {
    local name="$1" family="$2" run="$3"
    case_read "$name" "$run" || return 0
    if printf '%s' "$RUN_FAILED" | grep -qxE -- "$family"; then
        case_failed "$name" "$(grep -E "^FAIL ($family)" "$run/text" | head -1)"
    else
        case_passed "$name"
    fi
}

# unchecked_result <name> <symbol> <listed> <run>; a symbol part of the row f-our-1 that
# fails nothing, which passes when the gate ran, its record names no failure of
# citation-our-code, and its text lists the part <symbol> as not checked <listed> times, 0 or
# 1, by the line `not checked: f-our-1 cites the part "<symbol>"`; the text gives the FAIL line
# its failure quotes.
unchecked_result() {
    local name="$1" symbol="$2" listed="$3" run="$4" times
    case_read "$name" "$run" || return 0
    times="$(grep -cxF "  not checked: f-our-1 cites the part \"$symbol\"" "$run/text")"
    if printf '%s' "$RUN_FAILED" | grep -qx citation-our-code; then
        case_failed "$name" "$(grep '^FAIL citation-our-code' "$run/text" | head -1)"
    elif [ "$times" != "$listed" ]; then
        case_failed "$name" "its run lists the part as not checked $times times, not $listed"
    else
        case_passed "$name"
    fi
}

# passed_result <name> <run>; an honest form that passes only when the gate ends with its
# all-passed line, with no failure of any family; the text gives the FAIL lines its
# failure quotes.
passed_result() {
    local name="$1" run="$2"
    case_read "$name" "$run" || return 0
    if [ "$RUN_ENDING" = passed ]; then
        case_passed "$name"
    else
        case_failed "$name" "$(grep '^FAIL' "$run/text" | head -2 | tr '\n' ' ')$RUN_LINE"
    fi
}

# ending_result <name> <status> <last line, an extended regular expression> <run> [<planted
# reason, an extended regular expression>]; passes when the run's record holds that status
# and that last line. A case that asks for exit 2 plants the reason its last line names,
# unless it names another as its fifth argument, as a case over a run of check.test.sh
# names the reason of the case inside it; one that asks for exit 1 beside a reason that
# could not run names that reason. Any other reason is read by case_read's one rule.
ending_result() {
    local name="$1" want="$2" last="$3" run="$4" planted="${5:-}"
    [ "$want" = 2 ] && [ -z "$planted" ] && planted="$last"
    case_read "$name" "$run" "$planted" || return 0
    if [ "$RUN_STATUS" = "$want" ] && printf '%s\n' "$RUN_LINE" | grep -qE -- "$last"; then
        case_passed "$name"
    else
        case_failed "$name" "exit $RUN_STATUS, last line: $RUN_LINE"
    fi
}

# one_failure_result <name> <family> <the run's last line> <its one FAIL> <run>; the third
# and fourth are extended regular expressions. It asks whether exactly the planted
# assertion failed: it passes when the run's record holds exit 1, <family> alone among
# the assertions that failed and that last line, and its text one FAIL line, that one. The
# number of FAIL lines and what the one says are read from the text alone, since the record
# names a failed assertion once, however many times it failed, and holds no detail.
one_failure_result() {
    local name="$1" family="$2" summary="$3" failure="$4" run="$5"
    case_read "$name" "$run" || return 0
    if [ "$RUN_STATUS" = 1 ] && [ "$RUN_FAILED" = "$family"$'\n' ] && printf '%s\n' "$RUN_LINE" | grep -qE -- "$summary" &&
        [ "$(grep -c '^FAIL ' "$run/text")" = 1 ] && grep -qE -- "$failure" "$run/text"; then
        case_passed "$name"
    else
        case_failed "$name" "exit $RUN_STATUS, $(grep '^FAIL' "$run/text" | head -2 | tr '\n' ' '), last line: $RUN_LINE"
    fi
}

# path_without <tool>; makes $WORK/path-without-<tool>, a directory to set PATH to that
# holds a link to each program of this PATH, the first of each name, but <tool>, so that a
# run with it lacks that one tool. It stops when the tool is still found there.
path_without() {
    local bin="$WORK/path-without-$1" d
    if [ ! -d "$bin" ]; then
        mkdir -p "$bin"
        while IFS= read -r d; do
            [ -d "$d" ] && ln -s "$d"/* "$bin/" 2>/dev/null
        done < <(printf '%s\n' "$PATH" | tr ':' '\n')
        rm -f "$bin/$1"
    fi
    if (PATH="$bin" && command -v "$1" >/dev/null 2>&1); then
        printf 'FAIL %s could not be taken off PATH\n' "$1"
        exit 1
    fi
}

# The run's own ending (check.sh's header, "The run's own scratch and record"). check.sh,
# when this file sourced it, made and checked this run's scratch, and this file checks what
# its fixture builder needs before its first case, the lines of --self-test alone
# (prerequisite_rows cases): one that is missing ends it at once, exit 2, by early_end;
# each run a case makes checks the lines of its own mode. The cases' scratch, WORK, is
# under the run's own. A run that ends before its summary, at a case that stops it (a plant
# that could not be made exits 1) or at a crash, writes no record, which its reader reads
# as the failure it is; but when the run's scratch takes no write as it ends, that stop is
# the environment's, and cases_exit, the EXIT trap, ends the run by early_end, exit 2,
# naming the scratch. The summary sets CASES_SUMMARY, and run_ending reads the scratch
# there.
CASES_MISSING="$(prerequisites_missing cases)" && early_end prerequisite "$CASES_MISSING"
WORK="$RUN_SCRATCH/cases"
CASES_WHY="$({ mkdir -- "$WORK"; } 2>&1)" || early_end environment "the scratch of its cases, $WORK, cannot be made: ${CASES_WHY##*: }"
cases_exit() {
    local status=$? lost=""
    [ -n "${CASES_SUMMARY:-}" ] || lost="$(write_probe "$RUN_SCRATCH")" || :
    rm -rf "$RUN_SCRATCH"
    [ -z "$lost" ] || early_end environment "its own scratch $RUN_SCRATCH takes no write as it stops before its summary: $lost, so the case it stopped at stopped for that"
    exit "$status"
}
trap cases_exit EXIT
fixture_build "$WORK/base" >/dev/null 2>&1

fresh() {
    # fresh <name>; a copy of the fixture to break, as --self-test copies it.
    local dir="$WORK/$1"
    fixture_copy "$WORK/base" "$dir"
    printf '%s' "$dir"
}

# not_landed_plant <dir> <line>; commits the fixture's one step of sources.json's landed list,
# the page 'An execution record under the gen_ai. names', as not landed, for the condition
# "needs a decision of the owner's" and two records, NOT_LANDED_RECORD, the owner's ruling,
# and cybermonklabs/legion#4544, and writes <line> on that page in place of its Landed and
# Proven by lines. It returns non-zero when either could not be planted.
NOT_LANDED_RECORD='dispatch://INFRA-1484/ask/b426938b-404d-4044-8231-49e7147aba8f'
not_landed_plant() {
    local sources="$1/repo/docs/research/internet-of-cognition/sources.json"
    jq --arg r "$NOT_LANDED_RECORD" '.landed = [{"title": "An execution record under the gen_ai. names", "state": "not_landed", "condition": "needs a decision of the owner'"'"'s", "record": [$r, "cybermonklabs/legion#4544"]}]' \
        "$sources" >"$1/sources.json" && mv "$1/sources.json" "$sources" || return 1
    sed -i "/^- \*\*Proven by:\*\* \`true\`\$/d; s|^- \*\*Landed:\*\* [0-9a-f]*\$|$2|" "$1/judgement.md"
    grep -qxF -- "$2" "$1/judgement.md" && ! grep -qE '\*\*(Landed|Proven by):\*\*' "$1/judgement.md"
}

# symbol_plant <name> <file field> <symbol> [<path> <text>]...; prints a fresh copy of the
# fixture named <name> (fresh) into whose repository each <path>, where any is given, is
# committed holding <text>, that commit named as our repository's in sources.json, and whose
# row f-our-1 cites <file field> with <symbol>, in place of handoff.go and
# TesterVerdictRequired. It prints nothing and returns 1 when any of it could not be planted,
# and its caller stops.
symbol_plant() {
    local dir evidence named="" file="$2" symbol="$3" paths=()
    dir="$(fresh "$1")" || return 1
    evidence="$dir/repo/docs/research/internet-of-cognition"
    shift 3
    while [ "$#" -ge 2 ]; do
        mkdir -p "$dir/repo/$(dirname -- "$1")" && printf '%s' "$2" >"$dir/repo/$1" || return 1
        paths+=("$1")
        shift 2
    done
    if [ "${#paths[@]}" -gt 0 ]; then
        named="$(cd "$dir/repo" && git add -- "${paths[@]}" && git commit --quiet -m "the files a cited symbol names" && git rev-parse HEAD)" &&
            jq --arg c "$named" '.our_repository.commit = $c' "$evidence/sources.json" >"$dir/sources.json" &&
            mv "$dir/sources.json" "$evidence/sources.json" || return 1
        [ -n "$named" ] && [ "$(jq -r '.our_repository.commit' "$evidence/sources.json")" = "$named" ] || return 1
    fi
    sed -i "/\"id\":\"f-our-1\"/s#\"file\":\"packages/daemon/internal/api/handoff.go\",\"symbol\":\"TesterVerdictRequired\"#\"file\":\"$file\",\"symbol\":\"$symbol\"#" \
        "$evidence/claims.jsonl"
    grep -F '"id":"f-our-1"' "$evidence/claims.jsonl" | grep -qF "\"file\":\"$file\",\"symbol\":\"$symbol\"" || return 1
    printf '%s' "$dir"
}

# dir_copy <name> <check.test.sh>; a scratch copy of this directory that --self-test can
# run, whose check.test.sh is the file <check.test.sh> names.
dir_copy() {
    local copy="$WORK/$1"
    mkdir -p "$copy/evidence"
    cp "$HERE/check.sh" "$HERE/env.sh" "$HERE/citations.py" "$HERE/sources.json" "$HERE/claims.jsonl" "$copy/"
    cp "$HERE/evidence/ioc-venv.lock" "$HERE/evidence/ioc-venv.in" "$HERE/evidence/tm-constraints.txt" \
        "$HERE/evidence/build-constraints.txt" "$copy/evidence/"
    mkdir -p "$copy/tests" && cp -r "$HERE/tests/omp-stub" "$copy/tests/"
    cp "$2" "$copy/check.test.sh"
    printf '%s' "$copy"
}

# missing_result <name> <mode> <what is missing> <what provides it> <run>; the second and
# third are extended regular expressions. It passes when the mode cannot run, exit 2, and
# its last line names what is missing and ends with what provides it.
missing_result() {
    ending_result "$1" 2 "^check\\.sh cannot run --$2: $3; provided by: $4\$" "$5"
}

missing_tool() {
    # missing_tool <mode> <tool> <what provides it, an extended regular expression>
    local dir out
    path_without "$2"
    dir="$(fresh "missing-$2-$1")"
    if [ "$1" = admitted ]; then
        out="$(PATH="$WORK/path-without-$2" gate_run "$HERE/check.sh" --admitted)"
    else
        out="$(PATH="$WORK/path-without-$2" fixture_run "$dir" "--$1")"
    fi
    missing_result "--$1 with no $2 on PATH cannot run, naming $2 and what provides it" "$1" "the tool $2 is not on PATH" "$3" "$out"
}

# ==== the cases of check.test.sh begin below this line, where cases_copy cuts it ====

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
if ! case_read "--citations reports the live state of each published copy" "$out"; then
    :
elif grep -F 'https://example.org/page' "$out/text" | grep -qiE 'still carries|changed|unreachable|does not answer|no longer'; then
    case_passed "--citations reports the live state of each published copy"
else
    case_failed "--citations reports the live state of each published copy" "no line of the run names https://example.org/page and what its live page holds"
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
# 77b8e9234f as `uvx --python`, which would stop a pinned command from being recorded. The
# form reads the hashed lock with -c, since an install from an index reads a lock (case 62).
dir="$(fresh uvx-option-pinned)"
sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || uvx --python 3.12 -c \$IOC_EVID\/ioc-venv.lock pyshacl==0.40.2 --version; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
grep -qF 'uvx --python 3.12 -c $IOC_EVID/ioc-venv.lock pyshacl==0.40.2' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
    printf 'FAIL the form uvx --python 3.12 -c $IOC_EVID/ioc-venv.lock could not be planted\n'
    exit 1
}
out="$(fixture_run "$dir" --replay)"
honest_result "a pinned uvx with an option before its package passes install-pinned" install-pinned "$out"

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

# 19. The class, not three spellings of it: a word in command position that the reader
# cannot resolve fails the row, however it is written. A word built of two variables, a
# substitution's output standing as the command, and a variable after a wrapper each
# name a program the reader never sees, and none of them is a form cases 16 to 18 plant.
for form in '${A}${B} pyshacl' '$(printf uvx) pyshacl' 'env $RUNNER pyshacl'; do
    dir="$(fresh "class-$(printf '%s' "$form" | tr -c 'a-z0-9' '-' | cut -c1-24)")"
    sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || $form; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    grep -qF "$(printf '%s' "$form" | cut -c1-6)" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
        printf 'FAIL the form %s could not be planted\n' "$form"
        exit 1
    }
    case_result "a command word the reader cannot resolve is refused: $form" install-pinned "$(fixture_run "$dir" --replay)"
done

# 20. A runner named by its path, or by a versioned name, is still that runner. The
# header says a resolved command word "is held to every rule a literal is", yet the runner
# rules match the bare words pip, pip3 and uvx alone, so at 69fecb2dc2 each of these passed
# install-pinned: pip through an allow-listed venv path, pip through an absolute path, pip
# by its versioned name, and uvx through an allow-listed path. `python3 -m pip install`
# is refused there, so the gap is the path and the name, not pip.
for form in '\\"$IOC_MASLAB_VENV\/bin\/pip\\" install rdflib' '\/usr\/bin\/pip install rdflib' \
    'pip3.12 install rdflib' '\\"$IOC_MASLAB_VENV\/bin\/uvx\\" pyshacl'; do
    dir="$(fresh "path-$(printf '%s' "$form" | tr -c 'a-z0-9' '-' | cut -c1-24)")"
    sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || $form; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    grep -qF 'install rdflib' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" ||
        grep -qF 'uvx\" pyshacl' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
        printf 'FAIL the form %s could not be planted\n' "$form"
        exit 1
    }
    case_result "a runner named by its path or versioned name is held to its rule: $(printf '%s' "$form" | tr -d '\\')" install-pinned "$(fixture_run "$dir" --replay)"
done

# 21. The same rule for runners case 20 does not name: pip reached through a versioned
# interpreter's -m, and npx through a path of its own. Each runner is matched by the one
# rule that takes the program a word names, so a form no case lists is held to it too.
for form in 'python3.12 -m pip install pkg' '\/opt\/x\/bin\/npx pkg'; do
    dir="$(fresh "named-$(printf '%s' "$form" | tr -c 'a-z0-9' '-' | cut -c1-24)")"
    sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || $form; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    grep -qF "$(printf '%s' "$form" | tr -d '\\')" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
        printf 'FAIL the form %s could not be planted\n' "$form"
        exit 1
    }
    case_result "a runner is known by the program it names: $(printf '%s' "$form" | tr -d '\\')" install-pinned "$(fixture_run "$dir" --replay)"
done

# 22. A dispatch:// reference is read whole, its block anchor and its version included, by
# the one definition citations.py holds. A reference to a block Dispatch holds, at a
# version of the document that does not exist, resolves to no node; a reader that cut the
# reference at its `@`, as citations.py's pattern did at 25c02ddfcf, would read the block
# alone and pass it.
dir="$(fresh dispatch-version)"
printf '\nSee dispatch://INFRA-3134/spec#b-61117349-45b4-496f-af67-24b0d4063953@v9999.\n' \
    >>"$dir/repo/docs/research/internet-of-cognition/tests/concord.md"
case_result "a reference to a block at a version Dispatch does not hold is refused" citation-dispatch "$(fixture_run "$dir" --citations)"

# 23. A constraints file pins what it lists and nothing beside it. At 25c02ddfcf one -c,
# --constraint, -r or --requirement anywhere in a pip install exempted the whole command,
# so `uv pip install -c c.txt requests` passed install-pinned with requests unpinned. The
# honest form beside it, a local path installed under the same constraints file, passes;
# it names the hashed build lock with -b, since a local source path is built (case 61).
dir="$(fresh constraint-beside-package)"
sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || uv pip install -c c.txt requests; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
grep -qF 'uv pip install -c c.txt requests' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
    printf 'FAIL the form uv pip install -c c.txt requests could not be planted\n'
    exit 1
}
case_result "a package beside a constraints file is held to its own version" install-pinned "$(fixture_run "$dir" --replay)"
dir="$(fresh constraint-beside-local)"
sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || uv pip install -c c.txt -b \$IOC_EVID\/build-constraints.txt .\/local; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
grep -qF 'uv pip install -c c.txt -b $IOC_EVID/build-constraints.txt ./local' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
    printf 'FAIL the form uv pip install -c c.txt -b $IOC_EVID/build-constraints.txt ./local could not be planted\n'
    exit 1
}
out="$(fixture_run "$dir" --replay)"
honest_result "a local path beside a constraints file passes install-pinned" install-pinned "$out"

# 24-28 plant into the fixture row f-behaviour-1, whose command is `printf 'ran once\n'`
# and whose predicate is {"contains":"ran once"}, a command and a predicate of their own.
# plant_behaviour <dir> <command> <predicate> rewrites both by sed, as the cases above
# plant theirs, and stops when the planted command is not in claims.jsonl afterwards.
BEHAVIOUR_ROW='"command":"printf '"'"'ran once\\\\n'"'"'","expect":{"contains":"ran once"}'
plant_behaviour() {
    local claims="$1/repo/docs/research/internet-of-cognition/claims.jsonl" planted
    planted="$(jq -cn --arg c "$2" --argjson e "$3" '{command: $c, expect: $e}')"
    planted="${planted#\{}"
    planted="${planted%\}}"
    sed -i "s#$BEHAVIOUR_ROW#$(printf '%s' "$planted" | sed 's/[\\&#]/\\&/g')#" "$claims"
    grep -qF -- "$planted" "$claims" || {
        printf 'FAIL the command %s could not be planted\n' "$2"
        exit 1
    }
}

# 24. A read that failed in a stage of the final pipeline before its last. The pipeline's
# status is its last stage's, so `{ printf 'ran once\n'; false; } | cat` exits 0 and its
# output still meets the predicate. At 25c02ddfcf the ERR trap inside the group saw that
# `false`, but it does not see a command of an && list, so with `false && true` as the
# group's last command nothing recorded the failure and the row passed; the statuses of
# the final pipeline's stages hold it. A search that read its corpus and found nothing in
# such a stage is no failed read, so the honest form beside it passes.
for form in "{ printf 'ran once\\n'; false; } | cat" "{ printf 'ran once\\n'; false && true; } | cat"; do
    dir="$(fresh "failed-stage-$(printf '%s' "$form" | cksum | cut -d' ' -f1)")"
    plant_behaviour "$dir" "$form" '{"contains":"ran once"}'
    case_result "a failed stage before the last of the final pipeline fails the row: $form" replay-command "$(fixture_run "$dir" --replay)"
done
dir="$(fresh missed-search-stage)"
plant_behaviour "$dir" "printf 'ran once\\n'; printf 'x\\n' | grep zzz | cat" '{"contains":"ran once"}'
honest_result "a search that found nothing in a stage before the last passes" replay-command "$(fixture_run "$dir" --replay)"

# 25. A read that failed before &&. A command before && or || sets no ERR trap and no
# PIPESTATUS of the row's last pipeline, so `cat /nonexistent-ioc && true` failed unseen;
# the function hook that wraps cat records it. A search that missed before && passes.
dir="$(fresh failed-before-and)"
plant_behaviour "$dir" "cat /nonexistent-ioc && true; printf 'ran once\\n'" '{"contains":"ran once"}'
case_result "a failed read before && fails the row" replay-command "$(fixture_run "$dir" --replay)"
dir="$(fresh missed-before-and)"
plant_behaviour "$dir" "grep -q zzz /dev/null && true; printf 'ran once\\n'" '{"contains":"ran once"}'
honest_result "a search that found nothing before && passes" replay-command "$(fixture_run "$dir" --replay)"

# 26. A bound reads one number. At 25c02ddfcf a bound over output that was not a number
# counted its lines, so two lines of text met at_least 1.
dir="$(fresh bound-not-a-number)"
plant_behaviour "$dir" "printf 'ran once\\nsecond line\\n'" '{"at_least":"1"}'
case_result "a bound over output that is not one number fails the row" replay-command "$(fixture_run "$dir" --replay)"
# Two numbers on two lines are no one number either: a bound that took the whitespace out
# of its output read 1 and 2 as 12 and met at_least 5.
dir="$(fresh bound-two-numbers)"
plant_behaviour "$dir" "printf '1\\n2\\n'" '{"at_least":"5"}'
case_result "a bound over two numbers on two lines fails the row" replay-command "$(fixture_run "$dir" --replay)"

# 27. A declared status is the one status a program may exit with. cat exits 1 on a path
# it cannot read, so IOC_EXPECT_STATUS=2 does not cover it and the read fails the row;
# IOC_EXPECT_STATUS=1 declares exactly that status and passes.
dir="$(fresh declared-status-differs)"
plant_behaviour "$dir" "IOC_EXPECT_STATUS=2 cat /nonexistent-ioc; printf 'ran once\\n'" '{"contains":"ran once"}'
case_result "a status other than the declared one fails the row" replay-command "$(fixture_run "$dir" --replay)"
dir="$(fresh declared-status-holds)"
plant_behaviour "$dir" "IOC_EXPECT_STATUS=1 cat /nonexistent-ioc; printf 'ran once\\n'" '{"contains":"ran once"}'
honest_result "the declared status passes" replay-command "$(fixture_run "$dir" --replay)"

# 28. An exit status beside one predicate over the output: both hold or the row fails.
# At 25c02ddfcf an expect held exactly one key, so a row could not claim a failure and the
# message it prints together. The honest row passes; the same status with another
# message fails under replay-command. The failing command declares its status, the one
# form in which a row claims a failure (exit, which these cases first planted, ends the
# shell and is a form the evaluator does not admit).
dir="$(fresh exit-and-message)"
plant_behaviour "$dir" "printf 'ran once\\n'; IOC_EXPECT_STATUS=1 cat /nonexistent-ioc 2>/dev/null" '{"exit":1,"contains":"ran once"}'
out="$(fixture_run "$dir" --replay)"
honest_result "an exit status beside a message that both hold passes" 'replay-command|predicate-form' "$out"
dir="$(fresh exit-other-message)"
plant_behaviour "$dir" "printf 'other\\n'; IOC_EXPECT_STATUS=1 cat /nonexistent-ioc 2>/dev/null" '{"exit":1,"contains":"ran once"}'
case_result "an exit status beside a message the output does not carry fails the row" replay-command "$(fixture_run "$dir" --replay)"

# 29. A group in a stage of the final pipeline exits with its last command's status, so
# the stage is read as that command. A group whose last command is a search that found
# nothing read its corpus, and passes; the same group whose last command is a read that
# failed fails the row. The first form is the shape c-terms-7 takes, { git grep ...; } |
# wc -l, which a stage named by its opening brace took for a failed read.
dir="$(fresh group-ends-in-miss)"
plant_behaviour "$dir" "{ printf 'ran once\\n'; grep zzz /dev/null; } | cat" '{"contains":"ran once"}'
honest_result "a group whose last command is a search that found nothing passes" replay-command "$(fixture_run "$dir" --replay)"
dir="$(fresh group-ends-in-failure)"
plant_behaviour "$dir" "{ printf 'ran once\\n'; cat /nonexistent-ioc; } | cat" '{"contains":"ran once"}'
case_result "a group whose last command is a read that failed fails the row" replay-command "$(fixture_run "$dir" --replay)"

# 30. A program named by its path, or run through the command builtin, is still a program
# the command runs. The header's first rule refuses "a non-zero status of any program the
# command runs, wherever it stands", but only a bare word is wrapped in a recording
# function and only the five program variables point at a shim. So at c1052427bf a read
# that failed before || passed when its program was named /bin/cat, /usr/bin/python3 or
# ./a-script, or came after `command`, which runs the program itself and not the
# function. The same row with a bare `cat` fails, so the gap is how the program is named.
for form in '/bin/cat /nonexistent-ioc' '/usr/bin/python3 -c "import sys; sys.exit(4)"' \
    'command cat /nonexistent-ioc' './no-such-ioc-script.sh'; do
    dir="$(fresh "named-$(printf '%s' "$form" | tr -c 'a-z0-9' '-' | cut -c1-24)")"
    plant_behaviour "$dir" "$form 2>/dev/null || printf 'ran once\\n'" '{"contains":"ran once"}'
    case_result "a program named by its path or after command is refused as unread: $form" replay-command "$(fixture_run "$dir" --replay)"
done

# 31. A program the evaluator cannot record is a program the reader refuses, whatever
# form names it. builtin command runs cat past its function, and neither builtin nor
# command is wrapped, so the gate at c1052427bf passed the failed read; the row is now
# refused by name rather than read. (exec, the form first chosen here, replaces the shell,
# so the row never printed its output and failed its predicate at either gate.) The
# honest form beside it, a search named by its bare word that finds nothing, is read and
# passes.
dir="$(fresh builtin-command-unread)"
plant_behaviour "$dir" "builtin command cat /nonexistent-ioc 2>/dev/null || true; printf 'ran once\\n'" '{"contains":"ran once"}'
case_result "a program run by builtin command is refused as unread" replay-command "$(fixture_run "$dir" --replay)"
dir="$(fresh bare-search-misses)"
plant_behaviour "$dir" "grep -q zzz /dev/null || true; printf 'ran once\\n'" '{"contains":"ran once"}'
honest_result "a search named by its bare word that finds nothing passes" replay-command "$(fixture_run "$dir" --replay)"

# 32. The evaluator admits only the forms the rows use, and for each admitted form a read
# that fails in it fails the row while the honest form beside it, a search that finds
# nothing in the same place, passes. One pair per form of the header's admitted list.
pair() {
    # pair <name> <failing command> <honest command> [<expect>]
    local name="$1" bad="$2" good="$3" expect="${4:-}"
    [ -n "$expect" ] || expect='{"contains":"ran once"}'
    dir="$(fresh "pair-bad-$(printf '%s' "$name" | tr -c 'a-z0-9' '-')")"
    plant_behaviour "$dir" "$bad" "$expect"
    case_result "$name: a failed read fails the row" replay-command "$(fixture_run "$dir" --replay)"
    dir="$(fresh "pair-good-$(printf '%s' "$name" | tr -c 'a-z0-9' '-')")"
    plant_behaviour "$dir" "$good" "$expect"
    honest_result "$name: the honest form passes" replay-command "$(fixture_run "$dir" --replay)"
}
pair "a simple command before ;" \
    "cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" \
    "grep -q zzz /dev/null; printf 'ran once\\n'"
pair "an element before ||" \
    "cat /nonexistent-ioc 2>/dev/null || printf 'ran once\\n'" \
    "grep -q zzz /dev/null || printf 'ran once\\n'"
pair "a command on the line before" \
    "true
cat /nonexistent-ioc 2>/dev/null || printf 'ran once\\n'" \
    "true
grep -q zzz /dev/null || printf 'ran once\\n'"
pair "a subshell" \
    "(cat /nonexistent-ioc 2>/dev/null); printf 'ran once\\n'" \
    "(grep -q zzz /dev/null); printf 'ran once\\n'"
pair "a for loop" \
    "for f in a; do cat /nonexistent-ioc 2>/dev/null; done; printf 'ran once\\n'" \
    "for f in a; do grep -q zzz /dev/null; done; printf 'ran once\\n'"
pair "a command substitution" \
    "o=\"\$(cat /nonexistent-ioc 2>/dev/null)\"; printf 'ran once\\n'" \
    "o=\"\$(grep zzz /dev/null)\"; printf 'ran once\\n'"
pair "xargs running a program" \
    "printf '/nonexistent-ioc\\n' | xargs cat 2>/dev/null; printf 'ran once\\n'" \
    "printf 'ran once\\n' | xargs echo"
pair "the last stage of the final pipeline" \
    "printf 'ran once\\n'; printf 'x\\n' > /nonexistent-ioc-dir/f" \
    "printf 'ran once\\n'; printf 'x\\n' | grep -c zzz"
pair "a row with an exit status" \
    "cat /nonexistent-ioc 2>/dev/null | printf 'ran once\\n'" \
    "IOC_EXPECT_STATUS=1 cat /nonexistent-ioc 2>/dev/null | printf 'ran once\\n'" \
    '{"exit":0,"contains":"ran once"}'
pair "a program whose name starts with a digit" \
    "7z x /nonexistent-ioc 2>/dev/null || printf 'ran once\\n'" \
    "grep -q zzz /dev/null || printf 'ran once\\n'"
pair "a search whose options hide its subcommand" \
    "{ printf 'ran once\\n'; git -C . -c core.abc=1 -c core.abd=2 show no-such-ioc-ref 2>/dev/null; } | cat" \
    "{ printf 'ran once\\n'; git -C . -c core.abc=1 -c core.abd=2 grep zzz; } | cat"

# 33. A program variable whose program is not there when the shim runs it, as a row that
# installs the program itself finds on a cold cache, is a failed read the shim records.
dir="$(fresh program-variable-missing)"
plant_behaviour "$dir" "\"\$IOC_MASLAB_WHEEL_CTL\" validate x 2>/dev/null || true; printf 'ran once\\n'" '{"contains":"ran once"}'
case_result "a program variable whose program is missing records its failed read" replay-command "$(IOC_MASLAB_WHEEL_CTL=/nonexistent-ioc/bin/mas-ctl fixture_run "$dir" --replay)"

# 34. One refusal per class the classifier does not admit: each is refused by name, so a
# program it runs is never passed unread.
for form in "f() { cat /nonexistent-ioc; }; f" "function f { cat /nonexistent-ioc; }; f" \
    "time -p cat /nonexistent-ioc" "find / -maxdepth 0 -exec cat /nonexistent-ioc \\\\;" \
    "bash -c 'cat /nonexistent-ioc; true'" "env A=1 cat /nonexistent-ioc" \
    "{ls,-d,/nonexistent-ioc}" "cat < /nonexistent-ioc" "if true; then cat /nonexistent-ioc; fi" \
    "while false; do true; done" "IOC_EXPECT_STATUS=2; ls /nonexistent-ioc" "eval 'cat /nonexistent-ioc'"; do
    dir="$(fresh "refused-$(printf '%s' "$form" | tr -c 'a-z0-9' '-' | cut -c1-28)")"
    plant_behaviour "$dir" "$form 2>/dev/null || true; printf 'ran once\\n'" '{"contains":"ran once"}'
    case_result "a form the evaluator does not admit is refused: $form" replay-command "$(fixture_run "$dir" --replay)"
done

# 35. Every key of a predicate is read: an unknown key beside an exit status, and an exit
# status that is no number, are each no predicate.
for expect in '{"exit":1,"zz":"ran once"}' '{"contains":"ran once","exit":"one"}'; do
    dir="$(fresh "keys-$(printf '%s' "$expect" | tr -c 'a-z0-9' '-' | cut -c1-24)")"
    plant_behaviour "$dir" "printf 'ran once\\n'" "$expect"
    case_result "a predicate with a key of the wrong kind is refused: $expect" predicate-form "$(fixture_run "$dir" --replay)"
done

# 36. A row whose only program is a program variable names no program to wrap, and the
# runner reads no empty name for a program it cannot wrap: the row runs its shim and
# passes. Four rows of claims.jsonl have this shape (s-model-install among them), and a
# runner that refused the empty name refused all four on a cold replay.
dir="$(fresh program-variable-alone)"
plant_behaviour "$dir" "\"\$IOC_PY\" -c \"print('ran once')\"" '{"contains":"ran once"}'
honest_result "a row whose only program is a program variable passes" replay-command "$(IOC_PY="$(command -v python3)" fixture_run "$dir" --replay)"

# 37. Four forms the header's admitted list does not name are admitted at 27859400d7:
# the arithmetic command (( ... )), the arithmetic expansion $(( ... )), and the process
# substitutions <( ... ) and >( ... ). The header says every form it does not list is
# refused by name. The last form also hides a failed read: a >( ... ) runs alongside the
# row, so a program in it that fails after the row ends is recorded by nobody, and
# `echo | tee >(sleep 2; cat /nonexistent-ioc)` passed. No row of claims.jsonl uses any
# of the four.
for form in "(( 1 + 1 ))" "echo \$(( 2 + 3 ))" "cat <(printf x)" \
    "printf x | tee >(sleep 2; cat /nonexistent-ioc 2>/dev/null) >/dev/null"; do
    dir="$(fresh "unlisted-$(printf '%s' "$form" | tr -c 'a-z0-9' '-' | cut -c1-28)")"
    plant_behaviour "$dir" "$form; printf 'ran once\\n'" '{"contains":"ran once"}'
    case_result "a form the admitted list does not name is refused: $form" replay-command "$(fixture_run "$dir" --replay)"
done

# 38. Arithmetic in its other spelling. afdaf9863e refuses $(( )) by its opening text, but
# bash also reads $[ ] as an arithmetic expansion, and `echo $[2+3]` and `X=$[0]` were
# admitted at that head. The header refuses arithmetic as a form the evaluator does not
# admit, so each spelling of it is refused. No row of claims.jsonl holds `$[`.
for form in "echo \$[2+3]" "X=\$[0]"; do
    dir="$(fresh "arith-$(printf '%s' "$form" | tr -c 'a-z0-9' '-' | cut -c1-24)")"
    plant_behaviour "$dir" "$form; printf 'ran once\\n'" '{"contains":"ran once"}'
    case_result "arithmetic written as \$[ ] is refused: $form" replay-command "$(fixture_run "$dir" --replay)"
done

# 39. The classifier admits a $ expansion only in the forms the rows use: a parameter,
# $NAME or ${NAME}, the one special parameter $?, a command substitution
# and a literal $. Any other $ form is refused by name, so a spelling nobody named lands on
# a refusal: here a parameter with a default, ${o:-y}. The honest form beside it, the same
# parameter as ${o}, is read and passes. The parameter is o, a name the rows assign.
dir="$(fresh dollar-default-refused)"
plant_behaviour "$dir" "o=once; printf 'ran %s\\n' \"\${o:-y}\"" '{"contains":"ran once"}'
case_result "a \$ form the classifier does not list is refused: \${o:-y}" replay-command "$(fixture_run "$dir" --replay)"
dir="$(fresh dollar-parameter-admitted)"
plant_behaviour "$dir" "o=once; printf 'ran %s\\n' \"\${o}\"" '{"contains":"ran once"}'
honest_result "a parameter written as \${o} is admitted and passes" replay-command "$(fixture_run "$dir" --replay)"

# 40. The rows use one special parameter, $? (6 rows), and check.sh's header lists only
# that one, but dollar_forms admits every one-character special (SPECIAL holds
# ?$!#@*-0123456789). So at a201de0b62 $1, $@, $#, $$, $! and $0 were each admitted,
# none of which a row uses, against the rule that a $ form is admitted only where the rows
# use it. $? beside them is the honest form and passes.
special=0
for form in '"$1"' '"$@"' '"$#"' '"$$"' '"$!"' '"$0"'; do
    special=$((special + 1))
    dir="$(fresh "special-parameter-$special")"
    plant_behaviour "$dir" "printf '%s' $form >/dev/null; printf 'ran once\\n'" '{"contains":"ran once"}'
    case_result "a special parameter no row uses is refused: $form" replay-command "$(fixture_run "$dir" --replay)"
done
dir="$(fresh special-status-admitted)"
plant_behaviour "$dir" "true; printf 'ran %s\\n' \"once\$?\"" '{"contains":"ran once0"}'
honest_result "the special parameter \$?, which the rows use, is admitted and passes" replay-command "$(fixture_run "$dir" --replay)"

# 41-42 break a scratch copy of check.sh, never the gate itself, and run --admitted on the
# copy over this directory's claims.jsonl. plant_gate <name> <sed program> <text>
# <holds|lacks> makes the copy at $GATE and stops when, after the change, the copy does not
# hold the text it should hold, or still holds the text it should lack.
plant_gate() {
    local copy="$WORK/$1"
    mkdir -p "$copy"
    cp "$HERE/check.sh" "$copy/check.sh"
    sed -i "$2" "$copy/check.sh"
    if { [ "$4" = holds ] && ! grep -qF -- "$3" "$copy/check.sh"; } ||
        { [ "$4" = lacks ] && grep -qF -- "$3" "$copy/check.sh"; }; then
        printf 'FAIL the change %s could not be planted in the copy of check.sh\n' "$2"
        exit 1
    fi
    GATE="$copy/check.sh"
}

# 41. The audit's own bite: an entry of ADMITTED that no row uses is admitted unchecked, so
# --admitted fails it and names it. The copy adds the keyword until, which no row writes.
plant_gate admitted-unused 's/^ADMITTED = {$/&\n    "until": "a keyword no row writes, planted by check.test.sh",/' '"until": "a keyword no row writes' holds
case_result "an entry of ADMITTED that no row uses fails --admitted, named" 'admitted-form: until is used by no row' "$(gate_run "$GATE" --admitted)"

# 42. The table is the classifier's only admission: with the entry && taken out of
# ADMITTED, every row that joins two pipelines with && is refused, and --admitted fails
# naming a refused row and the form.
plant_gate admitted-missing '/^    "&&": /d' '    "&&": ' lacks
case_result "an entry the rows use, taken out of ADMITTED, refuses those rows" 'admitted-rows: the row [^ ]+ is refused: the operator &&' "$(gate_run "$GATE" --admitted)"

# 43. $_ is bash's special parameter, the last argument of the last command, but at
# c2ab1b7c56 the name pattern matched it, so it was admitted as the entry "$NAME or
# ${NAME}" that no row uses it through (the tester's note of test round 16). $_ and ${_}
# are refused by name; a name that starts with _, such as $_x, is a parameter and passes.
# No row assigns _x, so the honest form reads it unset, which expands to nothing.
for form in '"$_"' '"${_}"'; do
    dir="$(fresh "special-underscore-$(printf '%s' "$form" | tr -dc 'a-z{}' | tr '{}' 'bb')")"
    plant_behaviour "$dir" "true x; printf '%s' $form >/dev/null; printf 'ran once\\n'" '{"contains":"ran once"}'
    case_result "the special parameter \$_ is refused: $form" replay-command "$(fixture_run "$dir" --replay)"
done
dir="$(fresh underscore-name-admitted)"
plant_behaviour "$dir" "printf 'ran once%s\\n' \"\$_x\"" '{"contains":"ran once"}'
honest_result "a name that starts with _, \$_x, is a parameter and passes" replay-command "$(fixture_run "$dir" --replay)"

# 44-49 come from the fifth review of the pull request. Each fault plants into
# f-behaviour-1 a failed read that the gate must catch, and each passed the gate at
# 6f3da565d1, which ran the program or the form unrecorded; each is now refused, and the
# case asks for the refusal by name. Where the review wrote the read with ls, which no row
# runs and the gate now refuses, the review's form is planted as it wrote it and again
# with cat, so that the case tests the rule it plants. planted_run <name> <command>
# [python|evid|tcwork] plants a command into a fresh fixture and sets PLANTED_OUT to the
# gate's --replay output, planting in this shell so that a command that cannot be planted
# stops the run; with python the run sets IOC_PY, with evid IOC_EVID, to the fixture's own
# directory, each of which env.sh sets and the fixture does not, and with tcwork IOC_EVID
# and the variables of an empty tc-work environment and its clone, which freeze_world
# (case 66) builds.
# refused_read <name> <command> <refusal, an extended regular expression> [python|evid|tcwork]
# plants one fault; admitted_read <name> <command> [python|evid|tcwork] plants the honest
# form beside it, which passes.
planted_run() {
    local dir
    dir="$(fresh "$1")"
    plant_behaviour "$dir" "$2" '{"contains":"ran once"}'
    case "${3:-}" in
    python) PLANTED_OUT="$(IOC_PY="$(command -v python3)" fixture_run "$dir" --replay)" ;;
    evid) PLANTED_OUT="$(IOC_EVID="$dir/repo/docs/research/internet-of-cognition" fixture_run "$dir" --replay)" ;;
    tcwork) PLANTED_OUT="$(IOC_EVID="$dir/repo/docs/research/internet-of-cognition/evidence" IOC_TC_WORK="$FREEZE/empty" IOC_CLONES="$FREEZE/clones" fixture_run "$dir" --replay)" ;;
    *) PLANTED_OUT="$(fixture_run "$dir" --replay)" ;;
    esac
}
refused_read() {
    planted_run "$1" "$2" "${4:-}"
    case_result "a failed read is refused by name: $2" "replay-command: f-behaviour-1 runs $3" "$PLANTED_OUT"
}
admitted_read() {
    planted_run "$1" "$2" "${3:-}"
    honest_result "the admitted form passes: $2" replay-command "$PLANTED_OUT"
}

# 44. The program xargs runs is held to every rule a command word is held to. At
# 6f3da565d1 it was read by its name alone, so a shell given a string, a shell other than
# sh, env running a program and find -exec each ran after xargs with no rule read, and
# the read inside each was recorded by nobody: sh, bash and find exit 0, and so does
# xargs. The honest form, xargs running cat over a file that exists, passes.
refused_read xargs-sh-ls "printf 'x\\n' | xargs sh -c 'ls /nonexistent-ioc 2>/dev/null; true'; printf 'ran once\\n'" 'sh -c, a command given to a shell'
refused_read xargs-sh "printf 'x\\n' | xargs sh -c 'cat /nonexistent-ioc 2>/dev/null; true'; printf 'ran once\\n'" 'sh -c, a command given to a shell'
refused_read xargs-bash "printf 'x\\n' | xargs bash -c 'cat /nonexistent-ioc 2>/dev/null; true'; printf 'ran once\\n'" 'bash, a shell other than sh'
refused_read xargs-env "printf 'x\\n' | xargs env sh -c 'cat /nonexistent-ioc 2>/dev/null; true'; printf 'ran once\\n'" 'env, a wrapper, which runs a program past the function that records it'
refused_read xargs-find "printf '' | xargs find . -maxdepth 0 -exec cat /nonexistent-ioc 2>/dev/null \\; ; printf 'ran once\\n'" 'find, whose -exec runs a program'
admitted_read xargs-cat "printf '/dev/null\\n' | xargs cat; printf 'ran once\\n'"

# 45. xargs's options are read from the closed table of the options the rows use, -0 and
# -n. At 6f3da565d1 an option of xargs it did not know was taken for one with no value,
# so after --max-args the value 1 was read as the program and sh -c ran unread, and -I{}
# was stepped over with no rule read after it. Each option the table does not hold is
# now refused by name; the rows' own options pass.
refused_read xargs-replace "printf 'x\\n' | xargs -I{} sh -c 'cat /nonexistent-ioc 2>/dev/null; true'; printf 'ran once\\n'" 'xargs -I[{][}], an option of xargs the evaluator does not admit'
refused_read xargs-max-args "printf 'x\\n' | xargs --max-args 1 sh -c 'cat /nonexistent-ioc 2>/dev/null; true'; printf 'ran once\\n'" 'xargs --max-args, an option of xargs the evaluator does not admit'
admitted_read xargs-rows-options "printf '/dev/null\\0' | xargs -0 -n 1 cat; printf 'ran once\\n'"

# 46. A shell whose script is a stream under /dev/ or /proc/ reads its commands from it,
# as a shell with no script reads its input. sh /dev/stdin was admitted at 6f3da565d1 as
# a shell running a script file, and the read in the commands it read from the pipe
# was recorded by nobody. A script file the row writes and runs passes.
refused_read sh-dev-stdin "printf 'cat /nonexistent-ioc 2>/dev/null; true\\n' | sh /dev/stdin; printf 'ran once\\n'" 'sh /dev/stdin, a script under /dev/ or /proc/'
refused_read sh-proc-fd "printf 'cat /nonexistent-ioc 2>/dev/null; true\\n' | sh /proc/self/fd/0; printf 'ran once\\n'" 'sh /proc/self/fd/0, a script under /dev/ or /proc/'
admitted_read sh-script-file "printf 'true\\n' > ioc-script.sh; sh ioc-script.sh; printf 'ran once\\n'"

# 47. The declared status is written only before one command. At 6f3da565d1 export was
# a program like any other, so `export IOC_EXPECT_STATUS=2` set the declared status for
# every later program, and the failed read after it was exempted. export is refused as a
# program the table does not hold; the prefix the header allows passes.
refused_read export-status-ls "export IOC_EXPECT_STATUS=2; ls /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'export, which changes the shell'
refused_read export-status "export IOC_EXPECT_STATUS=1; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'export, which changes the shell'
refused_read for-status "for IOC_EXPECT_STATUS in 1; do cat /nonexistent-ioc 2>/dev/null; done; printf 'ran once\\n'" 'for IOC_EXPECT_STATUS, a loop that assigns IOC_EXPECT_STATUS'
admitted_read prefix-status "IOC_EXPECT_STATUS=1 cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'"

# 48. No row touches a name the gate records with. At 6f3da565d1 unset deleted the
# function that records cat, an assignment, a redirection or printf -v pointed the record
# at /dev/null or emptied it, and an assignment to a program variable, alone or before
# the command, ran python past its shim or sent the shim's record to /dev/null; each read
# after it failed unrecorded. Each is refused, naming the name; a program variable used
# as a command word, as the rows use it, passes.
refused_read unset-ls "unset -f ls; ls /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'unset, which changes the shell'
refused_read unset-cat "unset -f cat; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'unset, which changes the shell'
refused_read record-assigned "__ioc_failed=/dev/null; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" '__ioc_failed=/dev/null, an assignment to __ioc_failed'
refused_read record-redirected "cat /nonexistent-ioc 2>/dev/null; true >\"\$__ioc_failed\"; printf 'ran once\\n'" '.__ioc_failed, a redirection target that reads __ioc_failed'
refused_read record-printf-v "printf -v __ioc_failed /dev/null; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'printf -v, which assigns a variable'
refused_read program-variable-assigned "IOC_PY=python3; \$IOC_PY -c 'import sys; sys.exit(3)'; printf 'ran once\\n'" 'IOC_PY=python3, an assignment to IOC_PY' python
refused_read shim-record-prefix "__IOC_FAILED=/dev/null \$IOC_PY -c 'import sys; sys.exit(3)'; printf 'ran once\\n'" '__IOC_FAILED=/dev/null, an assignment to __IOC_FAILED' python
admitted_read program-variable-word "\$IOC_PY -c 'print(1)'; printf 'ran once\\n'" python

# 49. A program is admitted by its entry of the table, one per program name the rows run,
# and every other name is refused. At 6f3da565d1 a program named by a bare word was
# admitted unless a list of refused words held it, so hash, which no row runs, pointed
# cat at true and the read of cat passed. head, which no row runs either, is refused by
# name (6f3da565d1 wrapped it, so it caught head's own failed read).
refused_read hash-cat "hash -p /bin/true cat; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'hash, a program the evaluator does not admit'
refused_read head-unused "head -c 0 /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'head, a program the evaluator does not admit'

# 50-51 break a scratch copy of check.sh, as 41-42 do. The names the gate records with are
# held in one place each: PROGRAM_VARIABLES, from which the shims, the classifier's
# entries, install-pinned's resolve() and the reserved names are built, and
# RUNNER_PREFIXES, which every name of the runner starts with.
#
# 50. A program variable taken out of PROGRAM_VARIABLES leaves the header's list of
# reserved names, and --admitted fails the list by name. At 6f3da565d1 the variable left
# only the shims, and the classifier still admitted it as a command word with no shim.
plant_gate program-variable-dropped '/^PROGRAM_VARIABLES=/s/ IOC_TC_PY\(:python\)\{0,1\}"/" # IOC_TC_PY dropped by check.test.sh/' '" # IOC_TC_PY dropped by check.test.sh' holds
case_result "a program variable taken out of PROGRAM_VARIABLES fails --admitted" 'admitted-reserved: the header' "$(gate_run "$GATE" --admitted)"

# 51. A name of the runner that starts with no prefix of RUNNER_PREFIXES is one no row
# is kept from touching, and --admitted fails it by name.
plant_gate runner-name-unreserved 's/__ioc_stages/__gate_stages/g' '__gate_stages' holds
case_result "a name of the runner that no reserved prefix covers fails --admitted" 'admitted-reserved: the runner or the shims record with a name no prefix of RUNNER_PREFIXES reserves: __gate_stages' "$(gate_run "$GATE" --admitted)"

# 52. A script path a variable builds could name a stream as well: o=/dev/stdin and then
# sh "$o" reads its commands from the pipe. A script's path is held to a literal file,
# or one under $IOC_EVID, ending in .sh, so the path built from $o is refused by name.
# The one real row runs sh "$IOC_EVID/concord-build-run.sh"; a script under $IOC_EVID
# that the row writes passes (the fixture sources no env.sh, so the run sets IOC_EVID).
# The expected text is an extended regular expression, so each $ of the message is a dot.
refused_read sh-variable-path "o=/dev/stdin; printf 'cat /nonexistent-ioc 2>/dev/null; true\\n' | sh \"\$o\"; printf 'ran once\\n'" 'sh .o, a script path that is no literal file and not under .IOC_EVID'
admitted_read sh-evid-script "printf 'true\\n' > \"\$IOC_EVID/ioc-script.sh\"; sh \"\$IOC_EVID/ioc-script.sh\"; printf 'ran once\\n'" evid

# 53-57 come from the sixth review of the pull request. Each fault plants a failed read
# the gate must catch, and each is now refused by name; each but the second form of 56
# passed the gate at 0da660be6e.
#
# 53. printf -v assigns a variable, and 0da660be6e refused it only when -v stood as a
# literal first argument. A format that expands to -v, from a variable or a substitution,
# pointed the record at /dev/null or set the declared status for the rest of the row. A
# format is now a literal word that starts with no - and holds no $ or substitution; the
# substitution's own printf -- is refused by the same rule, before the format it makes.
refused_read printf-variable-option "o=-v; printf \$o __ioc_failed /dev/null; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'printf .o, a format that is no literal word'
refused_read printf-variable-joined "o=-v__ioc_failed; printf \$o /dev/null; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'printf .o, a format that is no literal word'
refused_read printf-substitution-option "printf \"\$(printf -- -v)\" __ioc_failed /dev/null; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'printf --, a format that is no literal word'
refused_read printf-variable-status "o=-v; printf \$o IOC_EXPECT_STATUS 1; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'printf .o, a format that is no literal word'
admitted_read printf-literal-format "printf '%s %s\\n' ran once"

# 54. env -S hands env a command line of its own, which ran a shell string past every
# function at 0da660be6e, since every word that started with - was read as an option.
# The one row that ran env, c-session-route-1, read the environment of whatever pod ran
# it, and since round 31 it reads the checks the reading recorded, so no row runs env and
# env is refused in every form, as a wrapper, alone as well as with an option.
refused_read env-split-string "env \"-Ssh -c 'cat /nonexistent-ioc 2>/dev/null; true'\"; printf 'ran once\\n'" 'env, a wrapper, which runs a program past the function that records it'
refused_read env-alone "env | grep -c . >/dev/null; printf 'ran once\\n'" 'env, a wrapper, which runs a program past the function that records it'

# 55. uv run runs the program its arguments name, and at 0da660be6e uv was admitted with
# any arguments, so uv run ran a shell string unread. Its program is now held to every
# rule a command word is held to, its options are read from the closed table of those
# the rows use, and its program is one of those the rows run with it, python alone. The
# honest form runs python through uv run, as the four rows do.
refused_read uv-run-sh "uv run --no-project sh -c 'cat /nonexistent-ioc 2>/dev/null; true'; printf 'ran once\\n'" 'sh -c, a command given to a shell'
refused_read uv-run-option "uv run --no-project --directory . sh -c 'cat /nonexistent-ioc 2>/dev/null; true'; printf 'ran once\\n'" 'uv run --directory, an option of uv run the evaluator does not admit'
refused_read uv-run-wrapper "uv run --no-project nohup sh -c 'cat /nonexistent-ioc 2>/dev/null; true'; printf 'ran once\\n'" 'uv run nohup, a program uv run runs that the evaluator does not admit'
admitted_read uv-run-python "uv run --no-project python -c \"print('ran once')\""

# 56. bash splits words at space, tab and newline alone. The tokenizer of 0da660be6e
# split at a carriage return too, so cat followed by one was admitted as cat, while bash
# ran a program named cat and a carriage return, which no function wraps, and its 127
# was recorded by nobody. A command holding a character bash keeps inside a word, a
# control character or a space Python counts and bash does not, such as U+00A0, is now
# refused by name. (0da660be6e split at no U+00A0, so it refused the second form as a
# program named by a path; it is planted for a reader that splits where Python's
# whitespace does.)
refused_read carriage-return "cat"$'\r'" /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'the character U.000D, which bash reads as part of a word'
refused_read no-break-space "cat"$'\xc2\xa0'"/nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'the character U.00A0, which bash reads as part of a word'

# 57. Each function finds the program it wraps on PATH, and at 0da660be6e a row could
# set PATH, so cat found a copy of true and the read of cat passed. The row now stops at
# its first assignment, to d, a name no row assigns; case 59 plants PATH again without it.
refused_read path-assigned "d=\$(mktemp -d); cp /bin/true \"\$d/cat\"; PATH=\"\$d:\$PATH\"; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'd=IOCSUBSTITUTION, an assignment to d, a name the evaluator does not admit'

# 58-60 come from the seventh review of the pull request. Each fault plants a failed read
# the gate must catch.
#
# 58. install-pinned reads uv run's options from UV_RUN_OPTIONS, the classifier's own
# table. At e9e39c35e5 its own walk stepped one word past --python, read 3.12 as the
# program and stopped, so the --with after it was never read, in the shape all four
# tc-run rows have. An unpinned --with in that shape is now refused; the real form, the
# wheel env.sh builds, $IOC_CONCORD_WHEEL, passes.
dir="$(fresh uv-run-python-with)"
sed -i "s#\"command\":\"printf 'ran once#\"command\":\"true || uv run --no-project --python 3.12 --with requests python -c 1; printf 'ran once#" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
case_result "an unpinned --with after uv run --python is refused" install-pinned "$(fixture_run "$dir" --replay)"
dir="$(fresh uv-run-python-with-local)"
sed -i "s#\"command\":\"printf 'ran once#\"command\":\"true || uv run --no-project --python 3.12 --with \$IOC_CONCORD_WHEEL python -c 1; printf 'ran once#" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
grep -qF 'uv run --no-project --python 3.12 --with $IOC_CONCORD_WHEEL' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
    printf 'FAIL the honest uv run --with form could not be planted\n'
    exit 1
}
honest_result "a built wheel after uv run --python passes install-pinned" install-pinned "$(fixture_run "$dir" --replay)"

# 59. A row assigns only a name the rows assign: an assignment, alone or as a prefix, and
# a for loop's variable are admitted to a name of ASSIGNED_NAMES alone, and every other
# name is refused by name. At e9e39c35e5 FUNCNEST=1 made each function's call to the
# record go past the limit, so bash ended that command and nothing was recorded, alone,
# in a subshell or as a loop's variable; BASH_ENV pointed a shim at a file that emptied
# its record. IFS and LC_ALL are names an author could write by accident, and PATH is
# planted here without case 57's d. The honest forms assign o and f, names the rows use.
refused_read funcnest-alone "FUNCNEST=1; cat /nonexistent-ioc 2>/dev/null
printf 'ran once\\n'" 'FUNCNEST=1, an assignment to FUNCNEST, a name the evaluator does not admit'
refused_read funcnest-subshell "( FUNCNEST=1; cat /nonexistent-ioc 2>/dev/null )
printf 'ran once\\n'" 'FUNCNEST=1, an assignment to FUNCNEST, a name the evaluator does not admit'
refused_read funcnest-loop "for FUNCNEST in 1; do cat /nonexistent-ioc 2>/dev/null; done
printf 'ran once\\n'" 'for FUNCNEST, a loop that assigns FUNCNEST, a name the evaluator does not admit'
refused_read bash-env-prefix "printf '__IO%s=\\n' C_FAILED > ioc-env.sh; BASH_ENV=./ioc-env.sh \$IOC_PY -c 'import sys; sys.exit(3)'; printf 'ran once\\n'" 'BASH_ENV=./ioc-env.sh, an assignment to BASH_ENV, a name the evaluator does not admit' python
refused_read ifs-alone "IFS=x; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'IFS=x, an assignment to IFS, a name the evaluator does not admit'
refused_read lc-all-prefix "LC_ALL=C cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'LC_ALL=C, an assignment to LC_ALL, a name the evaluator does not admit'
refused_read path-alone "mkdir -p ioc-bin; cp /bin/true ioc-bin/cat; PATH=\"\$PWD/ioc-bin:\$PATH\"; cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'" 'PATH=.PWD/ioc-bin:.PATH, an assignment to PATH, a name the evaluator does not admit'
admitted_read assigned-name "o=once; printf 'ran %s\\n' \"\$o\""
admitted_read assigned-loop "for f in once; do printf 'ran %s\\n' \"\$f\"; done"

# 60. The declared status covers the one command it prefixes and nothing a prefix
# substitution runs. bash expands prefixes from left to right, so at e9e39c35e5 a
# substitution in a prefix after IOC_EXPECT_STATUS=1 ran with the status declared, and
# its failed read was taken for the declared one. IOC_EXPECT_STATUS is now the last
# prefix of its command or refused; written alone before its command, it passes.
refused_read expect-status-before-prefix "IOC_EXPECT_STATUS=1 X=\$(cat /nonexistent-ioc 2>/dev/null) cat /dev/null; printf 'ran once\\n'" 'IOC_EXPECT_STATUS=1, IOC_EXPECT_STATUS before another prefix'
admitted_read expect-status-last-prefix "IOC_EXPECT_STATUS=1 cat /nonexistent-ioc 2>/dev/null; printf 'ran once\\n'"

# 61. A build backend is an install. Every local package the replay builds declares
# build-system requires = ["hatchling"] with no version, so at 5466ea605d a uv pip install
# of a local source path fetched whatever hatchling was newest, and so did a uv run --with
# of one, which takes no build constraint at all. A uv pip install of a local source path
# is now pinned only with -b of the hashed build lock on the same command, and a uv run
# --with of one is refused; a built wheel needs no build and passes. install_refused
# <name> <install> and install_passes <name> <install> plant the install after true ||
# with planted_run, so that nothing of it runs and only the reading of the command is
# read, and ask install-pinned for its verdict.
install_refused() {
    planted_run "$1" "true || $2; printf 'ran once\\n'"
    case_result "install-pinned refuses: $2" install-pinned "$PLANTED_OUT"
}
install_passes() {
    planted_run "$1" "true || $2; printf 'ran once\\n'"
    honest_result "install-pinned passes: $2" install-pinned "$PLANTED_OUT"
}
install_refused build-unpinned "uv pip install -c c.txt ./local"
install_passes build-pinned "uv pip install -c c.txt -b \"\$IOC_EVID/build-constraints.txt\" ./local"
install_refused run-source "uv run --no-project --with ./local python -c 1"
install_passes run-wheel "uv run --no-project --with ./local/pkg-0.1.0-py3-none-any.whl python -c 1"

# 62. A version is not enough: an install that resolves a package from an index reads a
# committed hashed lock. At cb6bb33c25 a pinned registry package passed install-pinned on
# its version alone, though the index may serve another file under that version and the
# package's own dependencies float. A pinned install that names no lock, or names a file
# under $IOC_EVID whose requirements carry no hash, is refused; the same install reading
# evidence/ioc-venv.lock, which the fixture carries beside the unhashed ioc-venv.in,
# passes.
install_refused no-lock "uv pip install --python x rdflib==7.6.0"
install_refused unhashed-lock "uv pip install --python x -r \"\$IOC_EVID/ioc-venv.in\""
install_passes hashed-lock "uv pip install --python x -c \"\$IOC_EVID/ioc-venv.lock\" rdflib==7.6.0"

# 63-65 come from the eighth review of the pull request. Each fault of 63 and 64 passed
# install-pinned at 8bad53ad40. The assignments of 65 were refused there too, by the closed
# list of assigned names, under another message, and its honest form is new.
#
# 63. Naming a lock exempts nothing it does not hold. At 8bad53ad40 one hashed -c or -r
# anywhere on the command passed every registry package beside it, so a package the lock
# never listed was fetched from today's index with no hash. Each package a command names
# is now looked up in the locks it names, at its exact version; uvx, which checks no hash
# of a -c file, is held to the same lookup. The honest forms name packages their lock
# holds at that version.
install_refused tm-lock-plus-package "uv pip install -c \"\$IOC_EVID/tm-constraints.txt\" -b \"\$IOC_EVID/build-constraints.txt\" ./runtime requests==2.32.3"
install_refused tm-lock-package "uv pip install -c \"\$IOC_EVID/tm-constraints.txt\" requests==2.32.3"
install_refused venv-lock-package "uv pip install -r \"\$IOC_EVID/ioc-venv.lock\" requests==2.32.3"
install_refused build-lock-package "uv pip install -c \"\$IOC_EVID/build-constraints.txt\" requests==2.32.3"
install_refused uvx-lock-package "uvx -c \"\$IOC_EVID/ioc-venv.lock\" pyflakes==3.2.0 --version"
install_passes tm-lock-held "uv pip install -c \"\$IOC_EVID/tm-constraints.txt\" -b \"\$IOC_EVID/build-constraints.txt\" ./runtime click==8.5.1"
install_passes venv-lock-held "uv pip install -r \"\$IOC_EVID/ioc-venv.lock\" rdflib==7.6.0"
install_passes build-lock-held "uv pip install -c \"\$IOC_EVID/build-constraints.txt\" hatchling==1.33.0"
install_passes tool-lock-held "uv tool run -c \"\$IOC_EVID/ioc-venv.lock\" --from pyshacl==0.40.2 pyshacl --version"

# 64. A build is held to the hashed build lock in every spelling. At 8bad53ad40 any -b
# passed, uv build was not read, a local source under uv tool run, uvx or pipx run was
# passed, and --editable=PATH and -ePATH were read as options; each built with whatever
# hatchling was newest. -b is now the hashed $IOC_EVID/build-constraints.txt itself, uv
# build names it, a local source under a runner that checks no hash of its build is
# refused, and an option under which uv would check no hash is refused by name.
install_refused build-tm-lock "uv pip install -b \"\$IOC_EVID/tm-constraints.txt\" ./local"
install_refused build-requirements "uv pip install -b requirements.txt ./local"
install_refused uv-build-bare "uv build --wheel ./local"
install_refused uv-build-other-b "uv build --wheel -b b.txt ./local"
install_refused tool-run-source "uv tool run --from \"\$IOC_CLONES/cognition-protocols/packages/ioc-concord\" python -c 1"
install_refused uvx-source "uvx --from ./pkg pkg"
install_refused pipx-source "pipx run --spec ./pkg pkg"
install_refused editable-equals "uv pip install --editable=./ctl"
install_refused editable-joined "uv pip install -e./ctl"
install_refused no-require-hashes "uv pip install --no-require-hashes -c \"\$IOC_EVID/ioc-venv.lock\" rdflib==7.6.0"
install_passes build-lock "uv pip install -b \"\$IOC_EVID/build-constraints.txt\" ./local"
install_passes uv-build-lock "uv build --wheel -b \"\$IOC_EVID/build-constraints.txt\" ./local"
install_passes uvx-wheel "uvx --from ./pkg-0.1.0-py3-none-any.whl pkg"
install_passes editable-equals-lock "uv pip install -b \"\$IOC_EVID/build-constraints.txt\" --editable=./ctl"
install_passes editable-joined-lock "uv pip install -b \"\$IOC_EVID/build-constraints.txt\" -e./ctl"

# 65. uv enforces the lock itself: env.sh exports UV_REQUIRE_HASHES=1, UV_BUILD_CONSTRAINT
# and PIP_REQUIRE_HASHES=1 into every row's environment, and the three are reserved, so no
# row assigns, unsets or re-points them. uv refuses every local source path under
# UV_REQUIRE_HASHES=1, so one form clears it, visibly, as UV_REQUIRE_HASHES=0, in the one
# install the header above reader_py in check.sh states. Each of its conditions broken,
# one at a time, is refused by name, and so are another value, a statement of its own,
# and an assignment to the other two names; the four real rows' form passes. Since the ninth
# review (case 67) the form also names the target environment's own lock with -c and
# passes --require-build-hashes, so the forms here that reach those conditions carry them.
EXEMPT='UV_REQUIRE_HASHES=0, which clears uv.s hash check, admitted only before the one install the header above reader_py states: '
refused_read exempt-registry "true || UV_REQUIRE_HASHES=0 uv pip install -c \"\$IOC_EVID/tm-constraints.txt\" -b \"\$IOC_EVID/build-constraints.txt\" ./local requests==2.32.3; printf 'ran once\\n'" "${EXEMPT}a package it installs is no local source path"
refused_read exempt-no-build "true || UV_REQUIRE_HASHES=0 uv pip install -c \"\$IOC_EVID/tm-constraints.txt\" ./local; printf 'ran once\\n'" "${EXEMPT}it names no -b .IOC_EVID/build-constraints.txt"
refused_read exempt-no-lock "true || UV_REQUIRE_HASHES=0 uv pip install --python \"\$IOC_TC_WORK/venv/bin/python\" -c c.txt -b \"\$IOC_EVID/build-constraints.txt\" --require-build-hashes ./local; printf 'ran once\\n'" "${EXEMPT}it names -c c.txt, which is not .IOC_EVID/ioc-venv.lock, the lock of IOC_TC_WORK"
refused_read exempt-other-value "true || UV_REQUIRE_HASHES=false uv pip install -c \"\$IOC_EVID/tm-constraints.txt\" -b \"\$IOC_EVID/build-constraints.txt\" ./local; printf 'ran once\\n'" 'UV_REQUIRE_HASHES=false, which clears uv.s hash check, admitted only .*: its value is not 0'
refused_read exempt-statement "UV_REQUIRE_HASHES=0; printf 'ran once\\n'" 'UV_REQUIRE_HASHES=0, which clears uv.s hash check, admitted only .*: it is no prefix'
refused_read build-constraint-assigned "true || UV_BUILD_CONSTRAINT=b.txt uv pip install -c \"\$IOC_EVID/tm-constraints.txt\" ./local; printf 'ran once\\n'" 'UV_BUILD_CONSTRAINT=b.txt, an assignment to UV_BUILD_CONSTRAINT, a name the gate records with'
refused_read pip-hashes-assigned "true || PIP_REQUIRE_HASHES=0 pip install ./local; printf 'ran once\\n'" 'PIP_REQUIRE_HASHES=0, an assignment to PIP_REQUIRE_HASHES, a name the gate records with'
refused_read exempt-no-environment "true || UV_REQUIRE_HASHES=0 uv pip install -c \"\$IOC_EVID/tm-constraints.txt\" -b \"\$IOC_EVID/build-constraints.txt\" ./local; printf 'ran once\\n'" "${EXEMPT}it installs into no environment env.sh builds"

# freeze_world; builds, once and offline, what an install into a checked environment
# needs, under $FREEZE: wheels/, holding ioc-freeze-dep 1.0, a wheel built here;
# clones/cognition-protocols, a git repository whose packages/ioc-freeze-pkg declares
# ioc-freeze-pkg 0.1.0, which requires ioc-freeze-dep==1.0 and is built by a backend in its
# own tree that needs no package; and empty/venv, an environment that holds nothing. It
# sets FREEZE_DEP_HASH, the sha256 of the wheel, and returns non-zero when uv is missing
# or a step fails, which the cases that need it report as a skip, never as a pass.
FREEZE="$WORK/freeze"
FREEZE_DEP_HASH=""
freeze_world() {
    [ -n "$FREEZE_DEP_HASH" ] && return 0
    command -v uv >/dev/null 2>&1 || return 1
    local clone="$FREEZE/clones/cognition-protocols" pkg
    pkg="$clone/packages/ioc-freeze-pkg"
    mkdir -p "$FREEZE/wheels" "$pkg" || return 1
    cat >"$pkg/backend.py" <<'PY'
import base64, hashlib, os, sys, zipfile

def wheel(directory, name, version, requires):
    dist = name.replace("-", "_")
    info = "%s-%s.dist-info" % (dist, version)
    files = {dist + "/__init__.py": "",
             info + "/METADATA": "Metadata-Version: 2.1\nName: %s\nVersion: %s\n%s" % (
                 name, version, "".join("Requires-Dist: %s\n" % r for r in requires)),
             info + "/WHEEL": "Wheel-Version: 1.0\nGenerator: check.test.sh\nRoot-Is-Purelib: true\nTag: py3-none-any\n"}
    filename = "%s-%s-py3-none-any.whl" % (dist, version)
    record = []
    with zipfile.ZipFile(os.path.join(directory, filename), "w") as z:
        for path, text in files.items():
            data = text.encode()
            z.writestr(zipfile.ZipInfo(path, (2020, 1, 1, 0, 0, 0)), data)
            digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
            record.append("%s,sha256=%s,%d" % (path, digest, len(data)))
        record.append(info + "/RECORD,,")
        z.writestr(zipfile.ZipInfo(info + "/RECORD", (2020, 1, 1, 0, 0, 0)), "\n".join(record) + "\n")
    return filename

def get_requires_for_build_wheel(config_settings=None):
    return []

def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    return wheel(wheel_directory, "ioc-freeze-pkg", "0.1.0", ["ioc-freeze-dep==1.0"])

if __name__ == "__main__":
    print(wheel(sys.argv[1], "ioc-freeze-dep", "1.0", []))
PY
    cat >"$pkg/pyproject.toml" <<'TOML'
[build-system]
requires = []
build-backend = "backend"
backend-path = ["."]

[project]
name = "ioc-freeze-pkg"
version = "0.1.0"
dependencies = ["ioc-freeze-dep==1.0"]
TOML
    git -C "$clone" init -q && git -C "$clone" add -A &&
        git -C "$clone" -c user.name=check.test.sh -c user.email=check.test.sh@invalid commit -qm fixture || return 1
    python3 "$pkg/backend.py" "$FREEZE/wheels" >/dev/null || return 1
    freeze_venv empty || return 1
    FREEZE_DEP_HASH="$(sha256sum "$FREEZE/wheels/ioc_freeze_dep-1.0-py3-none-any.whl" | cut -d' ' -f1)"
}
# freeze_venv <name>; an environment that holds nothing, $FREEZE/<name>/venv.
freeze_venv() {
    UV_CACHE_DIR="$FREEZE/cache" UV_OFFLINE=1 uv venv -q --python "$(command -v python3)" "$FREEZE/$1/venv" >/dev/null 2>&1
}
# freeze_run <dir> <environment name>; the gate's --replay of a fixture whose row installs
# into $FREEZE/<environment name>/venv, offline: uv reads no index and finds the wheel of
# ioc-freeze-dep under UV_FIND_LINKS, three of the inputs fixture_run passes on, and keeps
# its cache under the fixture's own IOC_CACHE, where prepare_env puts it.
freeze_run() {
    UV_OFFLINE=1 UV_NO_INDEX=1 UV_FIND_LINKS="$FREEZE/wheels" \
        IOC_EVID="$1/repo/docs/research/internet-of-cognition/evidence" IOC_TC_WORK="$FREEZE/$2" IOC_CLONES="$FREEZE/clones" \
        fixture_run "$1" --replay
}
if freeze_world; then
    admitted_read exempt-honest "true || UV_REQUIRE_HASHES=0 uv pip install --python \"\$IOC_TC_WORK/venv/bin/python\" -c \"\$IOC_EVID/ioc-venv.lock\" -b \"\$IOC_EVID/build-constraints.txt\" --require-build-hashes ./local; printf 'ran once\\n'" tcwork
else
    printf 'SKIP the admitted form of UV_REQUIRE_HASHES=0: uv, or an environment it builds here offline, is missing\n'
fi

# 66. The installed result is checked, not only the command. Under UV_REQUIRE_HASHES=0 a
# dependency the local package fetches that its -c leaves out installs unchecked, and at
# da7af429a2 nothing read what the environment held afterwards. The gate now lists every
# package of each environment env.sh builds that a row installs into, after the row, and
# refuses one its lock does not hold at that version by name. The case plants a local
# package of a pinned clone, ioc-freeze-pkg, whose dependency ioc-freeze-dep 1.0 the
# fixture's ioc-venv.lock, the tc-work environment's own lock, leaves out, installed with
# the visible prefix, -c of that lock, -b of the build lock and --require-build-hashes, as
# tc-install-1 installs ioc-concord; the honest form is the same install after the
# fixture's lock holds ioc-freeze-dep 1.0 with its hash, which uv checks. Both run
# offline: the dependency is a wheel built here, found under UV_FIND_LINKS with no index,
# and the package's backend needs no package.
FREEZE_INSTALL="UV_REQUIRE_HASHES=0 uv pip install --python \"\$IOC_TC_WORK/venv/bin/python\" -c \"\$IOC_EVID/ioc-venv.lock\" -b \"\$IOC_EVID/build-constraints.txt\" --require-build-hashes \"\$IOC_CLONES/cognition-protocols/packages/ioc-freeze-pkg\"; printf 'ran once\\n'"
if freeze_world && freeze_venv left-out && freeze_venv held; then
    dir="$(fresh freeze-left-out)"
    plant_behaviour "$dir" "$FREEZE_INSTALL" '{"contains":"ran once"}'
    case_result "a dependency the -c of a local install leaves out is refused by the check of the environment" \
        'install-pinned: f-behaviour-1 installed into IOC_TC_WORK, and what IOC_TC_WORK holds after it is refused: IOC_TC_WORK holds ioc-freeze-dep==1.0, which ioc-venv.lock does not hold at that version' \
        "$(freeze_run "$dir" left-out)"
    dir="$(fresh freeze-held)"
    plant_behaviour "$dir" "$FREEZE_INSTALL" '{"contains":"ran once"}'
    printf 'ioc-freeze-dep==1.0 \\\n    --hash=sha256:%s\n' "$FREEZE_DEP_HASH" >>"$dir/repo/docs/research/internet-of-cognition/evidence/ioc-venv.lock"
    passed_result "the same install with its dependency in the lock passes" "$(freeze_run "$dir" held)"
else
    printf 'SKIP the check of what an environment holds after a local install: uv, or an environment it builds here offline, is missing\n'
fi

# 67-68 come from the ninth review of the pull request; each fault passed the gate at
# fe34a1bada.
#
# 67. The exemption reads the target environment's own lock. At fe34a1bada any hashed -c
# passed, so the review installed mas-lab into a mas-lab environment with -c of
# ioc-venv.lock, and 13 of its 28 packages came from the index unhashed, at versions the
# check of the environment, which reads names and versions, then passed. -c is now the
# lock IOC_ENV_FROZEN binds the environment to, -r is refused, and --require-build-hashes is
# required, so uv checks the hash of a build dependency -b leaves out; each is refused by
# name beside the honest form of case 65. The review's form is planted as it ran.
refused_read exempt-other-lock "true || { cd \"\$IOC_CLONES/mas-lab\" && UV_REQUIRE_HASHES=0 VIRTUAL_ENV=\"\$IOC_MASLAB_WHEEL_VENV\" uv pip install -c \"\$IOC_EVID/ioc-venv.lock\" -b \"\$IOC_EVID/build-constraints.txt\" ./runtime ./ctl ./library-standard; }; printf 'ran once\\n'" "${EXEMPT}it names -c .IOC_EVID/ioc-venv.lock, which is not .IOC_EVID/tm-constraints.txt, the lock of IOC_MASLAB_WHEEL_VENV"
refused_read exempt-requirement "true || UV_REQUIRE_HASHES=0 uv pip install --python \"\$IOC_TC_WORK/venv/bin/python\" -r \"\$IOC_EVID/ioc-venv.lock\" -b \"\$IOC_EVID/build-constraints.txt\" --require-build-hashes ./local; printf 'ran once\\n'" "${EXEMPT}it names -r .IOC_EVID/ioc-venv.lock, and the lock of IOC_TC_WORK is read with -c alone"
refused_read exempt-no-build-hashes "true || UV_REQUIRE_HASHES=0 uv pip install --python \"\$IOC_TC_WORK/venv/bin/python\" -c \"\$IOC_EVID/ioc-venv.lock\" -b \"\$IOC_EVID/build-constraints.txt\" ./local; printf 'ran once\\n'" "${EXEMPT}it does not pass --require-build-hashes"

# 68. No assertion passes by its silence. At fe34a1bada the check of an environment
# crashed on an empty dist-info, which an interrupted install leaves, and install-pinned
# then reported neither PASS nor FAIL while the run still ended all N assertions passed.
# The check now refuses the dist-info by name, and every mode's verdict counts the
# assertions it declares: one that reports nothing, here order-of-work after an exit
# planted in its code in the fixture's copy of the gate, is a FAIL naming it.
if freeze_world && freeze_venv broken; then
    mkdir -p "$(set -- "$FREEZE/broken/venv"/lib/python*/site-packages && printf '%s' "$1")/broken-1.0.dist-info"
    dir="$(fresh freeze-broken-dist-info)"
    plant_behaviour "$dir" "true || $FREEZE_INSTALL" '{"contains":"ran once"}'
    case_result "an empty dist-info in an environment a row installs into fails install-pinned by name" \
        'install-pinned: f-behaviour-1 installed into IOC_TC_WORK, and what IOC_TC_WORK holds after it is refused: IOC_TC_WORK holds .*broken-1.0.dist-info, a dist-info with no name or version' \
        "$(freeze_run "$dir" broken)"
else
    printf 'SKIP the check of an environment holding an empty dist-info: uv, or an environment it builds here offline, is missing\n'
fi
dir="$(fresh shape-assertion-exits)"
sed -i '/pass order-of-work "the commit that adds the question list is an ancestor/s/^\( *\)/\1exit 3; /' "$dir/repo/docs/research/internet-of-cognition/check.sh"
grep -q 'exit 3; \[ "$bad" -eq 0 \] && pass order-of-work' "$dir/repo/docs/research/internet-of-cognition/check.sh" || {
    printf 'FAIL the exit could not be planted in the fixture copy of check.sh\n'
    exit 1
}
case_result "an assertion whose code exits before its verdict fails, named" 'order-of-work: it reported no verdict' "$(fixture_run "$dir" --shape)"

# 69-71 come from the tenth review of the pull request and the tester's note of round 23.
#
# 69. An assertion prints PASS only after all its parts have run. At 987017b05f
# admitted-reserved printed PASS after its first part, so an exit or an unbound variable
# in its second part left that PASS standing and the run ended all 4 assertions passed.
# A scratch copy of check.sh plants each before the second part, and --admitted fails
# admitted-reserved by name.
plant_gate reserved-second-exit 's/^    local name prefix stray=""$/    exit 0 # planted by check.test.sh\n&/' 'exit 0 # planted by check.test.sh' holds
case_result "an exit before the second part of admitted-reserved fails it, named" 'admitted-reserved: it reported no verdict' "$(gate_run "$GATE" --admitted)"
plant_gate reserved-second-unbound 's/^    local name prefix stray=""$/&\n    : "$NO_SUCH_VARIABLE"/' ': "$NO_SUCH_VARIABLE"' holds
case_result "an unbound variable in the second part of admitted-reserved fails it, named" 'admitted-reserved: it reported no verdict' "$(gate_run "$GATE" --admitted)"

# 70. Three endings, kept apart to the end. At 987017b05f a mode that could not run ended
# as every declared assertion failing, exit 1, so a spent GitHub budget read as the
# deliverable failing. A mode that cannot run now ends exit 2 with its reason as the last
# line and no assertion counted; one that ran and failed ends exit 1 with the count; one
# that passed ends exit 0. Each form is read by ending_result, at the top of this file.
# The budget's form fakes GitHub's answer to the read of its rate limit in a copy of
# check.sh twice, with fewer requests left than --replay makes: once an address's
# unauthenticated budget, a limit of 60, and once a token's. Each run must end exit 2
# naming which budget it is, how many requests of how many are left, how many the mode
# makes and when the budget resets.
dir="$(fresh ending-no-env)"
rm "$dir/repo/docs/research/internet-of-cognition/env.sh"
out="$(fixture_run "$dir" --replay)"
ending_result "a mode whose env.sh is missing cannot run: exit 2, the reason last" 2 '^check\.sh cannot run: env\.sh' "$out"
dir="$(fresh ending-budget-spent)"
sed -i '/^github_budget() {$/,/^}$/{/IOC_NO_ENV/d;s#curl -sS -H @"\$IOC_GITHUB_AUTH_LEGION" https://api.github.com/rate_limit#cat "$IOC_FAKE_RATE_LIMIT"#}' "$dir/repo/docs/research/internet-of-cognition/check.sh"
grep -qF 'cat "$IOC_FAKE_RATE_LIMIT"' "$dir/repo/docs/research/internet-of-cognition/check.sh" || {
    printf 'FAIL the budget read could not be faked in the fixture copy of check.sh\n'
    exit 1
}
budget_made="this mode makes $((GITHUB_CORPUS_REQUESTS + GITHUB_ROW_REQUESTS)): $GITHUB_CORPUS_REQUESTS to read env\\.sh's review corpus and $GITHUB_ROW_REQUESTS for the rows\\. The budget resets at 2026-11-15T[0-9:]+Z\\.$"
printf '{"resources":{"core":{"limit":60,"remaining":3,"reset":1794700800}}}\n' >"$dir/budget.json"
out="$(IOC_FAKE_RATE_LIMIT="$dir/budget.json" fixture_run "$dir" --replay)"
ending_result "a spent unauthenticated GitHub budget cannot run: exit 2, naming the address's budget, what is left of it, what the mode makes and the reset" 2 \
    "^check\\.sh cannot run --replay now: this address's unauthenticated GitHub budget, the one a read with no token draws on, has 3 of its 60 requests left, and $budget_made" "$out"
printf '{"resources":{"core":{"limit":5000,"remaining":200,"reset":1794700800}}}\n' >"$dir/budget-token.json"
out="$(IOC_FAKE_RATE_LIMIT="$dir/budget-token.json" fixture_run "$dir" --replay)"
ending_result "a token's spent GitHub budget cannot run: exit 2, naming the token's budget, what is left of it, what the mode makes and the reset" 2 \
    "^check\\.sh cannot run --replay now: the GitHub budget of the token git credential fill gave for cybermonklabs/legion has 200 of its 5000 requests left, and $budget_made" "$out"
dir="$(fresh ending-failed)"
sed -i 's/"expect":{"contains":"ran once"}/"expect":{"contains":"ran twice"}/' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(fixture_run "$dir" --replay)"
ending_result "an ordinary failed assertion still ends exit 1 with the count" 1 '^1 of 18 assertions failed: replay-command$' "$out"
dir="$(fresh ending-passed)"
out="$(fixture_run "$dir" --replay)"
ending_result "a mode whose assertions all pass ends exit 0" 0 '^all 18 assertions passed$' "$out"

# 71. The exemption reads every spelling of its files (the tester's note of round 23). At
# 987017b05f the exemption read -c, --constraint, -r and --requirement alone, and held
# only one -b to the build lock, so --constraints of another lock, --requirements, and a
# second --build-constraints that is not the build lock passed beside the honest form.
# Each is refused by name; the same install without it passes, and so does the target's
# own lock written --constraints=, a spelling the exemption now reads.
refused_read exempt-constraints-other "true || UV_REQUIRE_HASHES=0 uv pip install --python \"\$IOC_TC_WORK/venv/bin/python\" -c \"\$IOC_EVID/ioc-venv.lock\" --constraints \"\$IOC_EVID/tm-constraints.txt\" -b \"\$IOC_EVID/build-constraints.txt\" --require-build-hashes ./local; printf 'ran once\\n'" "${EXEMPT}it names --constraints .IOC_EVID/tm-constraints.txt, which is not .IOC_EVID/ioc-venv.lock, the lock of IOC_TC_WORK"
refused_read exempt-constraints-equals "true || UV_REQUIRE_HASHES=0 uv pip install --python \"\$IOC_TC_WORK/venv/bin/python\" -c \"\$IOC_EVID/ioc-venv.lock\" --constraints=\"\$IOC_EVID/tm-constraints.txt\" -b \"\$IOC_EVID/build-constraints.txt\" --require-build-hashes ./local; printf 'ran once\\n'" "${EXEMPT}it names --constraints .IOC_EVID/tm-constraints.txt, which is not .IOC_EVID/ioc-venv.lock, the lock of IOC_TC_WORK"
refused_read exempt-requirements "true || UV_REQUIRE_HASHES=0 uv pip install --python \"\$IOC_TC_WORK/venv/bin/python\" -c \"\$IOC_EVID/ioc-venv.lock\" --requirements \"\$IOC_EVID/ioc-venv.lock\" -b \"\$IOC_EVID/build-constraints.txt\" --require-build-hashes ./local; printf 'ran once\\n'" "${EXEMPT}it names --requirements .IOC_EVID/ioc-venv.lock, and the lock of IOC_TC_WORK is read with -c alone"
refused_read exempt-second-build "true || UV_REQUIRE_HASHES=0 uv pip install --python \"\$IOC_TC_WORK/venv/bin/python\" -c \"\$IOC_EVID/ioc-venv.lock\" -b \"\$IOC_EVID/build-constraints.txt\" --build-constraints \"\$IOC_EVID/tm-constraints.txt\" --require-build-hashes ./local; printf 'ran once\\n'" "${EXEMPT}it names --build-constraints .IOC_EVID/tm-constraints.txt, which is not the hashed build lock"
if freeze_world; then
    admitted_read exempt-constraints-own "true || UV_REQUIRE_HASHES=0 uv pip install --python \"\$IOC_TC_WORK/venv/bin/python\" --constraints=\"\$IOC_EVID/ioc-venv.lock\" --build-constraints \"\$IOC_EVID/build-constraints.txt\" --require-build-hashes ./local; printf 'ran once\\n'" tcwork
else
    printf 'SKIP the exemption with --constraints= and --build-constraints: uv, or an environment it builds here offline, is missing\n'
fi

# 72. --landed holds each step this tree commits to a page, not only the pages it finds.
# At 8f53f0c8ca the gate looked only at the pages that carry a Landed line, so a judgement
# whose landed pages had lost that line, or a judgement Dispatch did not answer the read of
# (an empty text), ended "each of the 0 landed pages ... all 5 assertions passed". Each
# title of sources.json's landed list needs a page that carries its Landed line, and a read
# of the judgement that gives nothing cannot be judged.
dir="$(fresh landed-lines-gone)"
sed -i '/^- \*\*Landed:\*\*/d' "$dir/judgement.md"
grep -q '\*\*Landed:\*\*' "$dir/judgement.md" && { printf 'FAIL the Landed lines could not be deleted\n'; exit 1; }
case_result "a judgement whose landed pages lost their Landed line fails --landed" landed-proof "$(fixture_run "$dir" --landed)"
dir="$(fresh landed-empty-judgement)"
: >"$dir/judgement.md"
out="$(fixture_run "$dir" --landed)"
if ! case_read "an empty judgement is no pass of --landed" "$out"; then
    :
elif [ "$RUN_ENDING" = passed ]; then
    case_failed "an empty judgement is no pass of --landed" "the gate passed it: $(grep '^PASS landed-proof' "$out/text" | head -1)"
else
    case_passed "an empty judgement is no pass of --landed"
fi
# A Dispatch that does not answer is a mode that cannot run, as case 70 says of a missing
# env.sh and a spent budget. At 8f53f0c8ca, with no Dispatch, --citations ended exit 1
# with one FAIL per reference "resolves to no node Dispatch holds", blaming the evidence
# for the outage; the empty judgement above is what --landed read with no Dispatch.
dir="$(fresh citations-no-dispatch)"
out="$(DISPATCH_URL=http://127.0.0.1:1 fixture_run "$dir" --citations)"
ending_result "--citations with no Dispatch to resolve against cannot run: exit 2, the reason last" 2 '^check\.sh cannot run --citations: Dispatch, the read of dispatch://[^ ]* did not answer' "$out"

# 73-75 come from test round 24 and the architect's decision on it: no check passes on no
# input, and a live source that does not answer is a mode that cannot run (the header of
# check.sh states both). Each fault passed at 8f53f0c8ca, or ended there as the
# deliverable failing; each honest form beside it is the fixture as it is built.
#
# 73. An empty judgement ends each mode that reads it in a FAIL naming the assertion. At
# 8f53f0c8ca --shape failed page-count alone and passed each assertion over the pages on
# none of them, --landed passed every assertion, and --replay read the judgement only
# after its other assertions had judged.
dir="$(fresh empty-judgement-shape)"
: >"$dir/judgement.md"
case_result "an empty judgement fails the assertions over its pages, named" 'page-parts: the judgement carries 0 candidate pages' "$(fixture_run "$dir" --shape)"
dir="$(fresh empty-judgement-landed)"
: >"$dir/judgement.md"
case_result "an empty judgement fails --landed, naming the step it lost" 'landed-proof: the step .An execution record under the gen_ai. names., which sources.json.s landed list commits, has no page' "$(fixture_run "$dir" --landed)"
dir="$(fresh empty-judgement-replay)"
sed -i 's/| inside one tree | 20 | 100 |/| inside one tree | 0 | 100 |/' "$dir/repo/docs/research/internet-of-cognition/coordination/count.md"
: >"$dir/judgement.md"
case_result "an empty judgement fails --replay's zero class, named" 'count-zero-premise' "$(fixture_run "$dir" --replay)"
dir="$(fresh judgement-honest)"
out="$(fixture_run "$dir" --shape)"
ending_result "the fixture's judgement passes --shape" 0 '^all 9 assertions passed$' "$out"
out="$(fixture_run "$dir" --landed)"
ending_result "the fixture's judgement passes --landed" 0 '^all 7 assertions passed$' "$out"

# 74. A step of sources.json's landed list that no page carries fails by its title. At
# 8f53f0c8ca the gate read only the pages it found, so a renamed page left the step its
# list commits unread. Other inputs of a committed set are held the same way: a test
# against our own records whose file is gone, and a record under check read as nothing.
dir="$(fresh landed-title-missing)"
sed -i 's/^### An execution record under the gen_ai\. names$/### An execution record, renamed/' "$dir/judgement.md"
grep -q '^### An execution record, renamed$' "$dir/judgement.md" || { printf 'FAIL the landed page could not be renamed\n'; exit 1; }
case_result "a landed step whose page is gone fails --landed, naming it" 'landed-proof: the step .An execution record under the gen_ai. names., which sources.json.s landed list commits, has no page of the judgement that carries its Landed line' "$(fixture_run "$dir" --landed)"
dir="$(fresh test-file-missing)"
rm "$dir/repo/docs/research/internet-of-cognition/tests/scope.md"
case_result "a test against our own records whose file is gone fails, named" 'our-data-input: the test scope against our own records has no file' "$(fixture_run "$dir" --replay)"
dir="$(fresh record-read-empty)"
: >"$dir/repo/docs/research/internet-of-cognition/records/under-check.md"
case_result "a record under check read as nothing fails citation-row by its rows" 'citation-row: the row u-[0-9]+ is no citation the record labelled u carries' "$(fixture_run "$dir" --replay)"

# 75. A live source that does not answer is a mode that cannot run, in every mode that
# reads one, with the source named last. At 8f53f0c8ca, with no Dispatch, --landed passed
# on an empty judgement and --published failed its delivery; a record read live from
# Dispatch failed citation-row. The fixture's own judgement is a file, so its read that
# does not answer is a file that is not there; the tree's own --landed reads Dispatch.
dir="$(fresh judgement-unreadable)"
rm "$dir/judgement.md"
out="$(fixture_run "$dir" --landed)"
ending_result "--landed whose judgement does not answer cannot run" 2 '^check\.sh cannot run --landed: the judgement file .* did not answer' "$out"
out="$(fixture_run "$dir" --shape)"
ending_result "--shape whose judgement does not answer cannot run" 2 '^check\.sh cannot run --shape: the judgement file .* did not answer' "$out"
out="$(DISPATCH_URL=http://127.0.0.1:1 gate_run "$HERE/check.sh" --landed)"
ending_result "--landed with no Dispatch cannot run, naming Dispatch" 2 '^check\.sh cannot run --landed: Dispatch, the read of dispatch://INFRA-3134/artifact/judgement-md did not answer' "$out"
dir="$(fresh record-no-dispatch)"
jq -c '.records_under_check |= map(del(.copy) | .ref = "dispatch://INFRA-3134/artifact/judgement-md")' "$dir/repo/docs/research/internet-of-cognition/sources.json" >"$dir/sources.json" &&
    mv "$dir/sources.json" "$dir/repo/docs/research/internet-of-cognition/sources.json"
out="$(DISPATCH_URL=http://127.0.0.1:1 fixture_run "$dir" --replay)"
ending_result "--replay whose record under check Dispatch does not answer cannot run" 2 '^check\.sh cannot run --replay: Dispatch, the read of dispatch://INFRA-3134/artifact/judgement-md did not answer' "$out"
dir="$(fresh published-no-dispatch)"
out="$(DISPATCH_URL=http://127.0.0.1:1 fixture_run "$dir" --published)"
ending_result "--published with no Dispatch cannot run, naming Dispatch" 2 '^check\.sh cannot run --published: Dispatch, the read of the comments of INFRA-3134 did not answer' "$out"
dir="$(fresh citations-honest)"
out="$(fixture_run "$dir" --citations)"
ending_result "the fixture passes --citations when Dispatch answers" 0 '^all 6 assertions passed$' "$out"
out="$(fixture_run "$dir" --replay)"
ending_result "the fixture passes --replay" 0 '^all 18 assertions passed$' "$out"

# 76. A recorded command reproduces on a machine that never ran it. At 9959ca5563 the row
# m-ctl-our-row-count kept its Go module cache at $TMPDIR/gomod, outside IOC_CACHE and
# IOC_CLONES. So a replay at new roots on a machine that had run it before still read a warm
# cache. On a machine that had not, go printed one "go: downloading" line per module before
# "rows 38", and the row failed its {"equals":"rows 38"}. Test round 25's cold replay met it
# on a pod that had just been replaced. The case runs that row's own command, read from
# claims.jsonl, through the gate's replay, with a fresh TMPDIR and IOC_CACHE and our tree at
# the commit sources.json pins, and holds it to its own predicate. It names no Go cache of
# its own: the fixture's --replay runs prepare_env, which exports every cache of env.sh's
# table under the IOC_CACHE given here, GOMODCACHE and GOCACHE among them, so the row reads
# go's caches from that empty directory, and a fixture takes no Go cache the caller's
# environment names. It needs go, git and the Go module proxy, and prints SKIP when go is
# missing.
if command -v go >/dev/null 2>&1; then
    row="$(jq -c 'select(.id=="m-ctl-our-row-count")' "$HERE/claims.jsonl")"
    if [ -z "$row" ]; then
        case_failed "the row m-ctl-our-row-count is not in claims.jsonl, so nothing replays the count it proves"
    else
        cold="$WORK/cold-go"
        mkdir -p "$cold/tmp" "$cold/cache" "$cold/our"
        git -C "$IOC_ROOT" archive "$(jq -r '.our_repository.commit' "$HERE/sources.json")" go.work go.work.sum packages/daemon packages/envoy |
            tar -x -C "$cold/our"
        dir="$(fresh cold-go-cache)"
        plant_behaviour "$dir" "$(printf '%s' "$row" | jq -r '.command')" "$(printf '%s' "$row" | jq -c '.expect')"
        out="$(TMPDIR="$cold/tmp" IOC_CACHE="$cold/cache" IOC_OUR_TREE="$cold/our" fixture_run "$dir" --replay)"
        # go writes its module cache read-only, and the EXIT trap removes $WORK.
        chmod -R u+w "$cold"
        honest_result "m-ctl-our-row-count meets its predicate on a machine with no Go module cache" replay-command "$out"
    fi
else
    printf 'SKIP the replay of m-ctl-our-row-count with no Go module cache: go is missing\n'
fi

# 77. --landed on a fresh checkout judges the step, with nothing installed. At 9959ca5563 the
# committed proof of a landed step was (cd packages/pi-legion && bun test ...), which needs
# the packages `bun install` writes, and a checkout with no node_modules ended --landed exit 1
# with "FAIL landed-proof: the committed proof of the page ... exits non-zero" (bun: 'Could
# not resolve: "zod". Maybe you need to "bun install"?'), which blamed the landed step for a
# step of setup; test round 25 met it on a pod that had just been replaced, and --landed's
# table then named bun and that install, so such a checkout could not run. Both proofs now
# read fcc746a3 with git show (review 6003758567), so --landed needs nothing installed and
# its table names neither. The case clones this checkout's own history at its HEAD, with no
# node_modules, and runs the gate under test, its own check.sh and evidence, over that clone
# as the root of the tree, which must pass every assertion of --landed. It needs git and
# Dispatch.
fresh_checkout="$WORK/fresh-checkout"
git clone --quiet --shared --no-checkout "$(git -C "$IOC_ROOT" rev-parse --path-format=absolute --git-common-dir)" "$fresh_checkout" &&
    git -C "$fresh_checkout" checkout --quiet --detach "$(git -C "$IOC_ROOT" rev-parse HEAD)" &&
    [ ! -e "$fresh_checkout/node_modules" ] && [ ! -e "$fresh_checkout/packages/pi-legion/node_modules" ] || {
    printf 'FAIL the fresh checkout of HEAD with no node_modules could not be made\n'
    exit 1
}
ending_result "--landed on a checkout with no node_modules passes, its proofs installing nothing" 0 '^all 7 assertions passed$' \
    "$(IOC_ROOT="$fresh_checkout" gate_run "$HERE/check.sh" --landed)"

# 78-83 come from test round 25 and the architect's decision on it. Each mode declares what
# it needs in one table, prerequisite_table, which one step checks before the mode reads or
# judges anything, so a prerequisite that is missing ends the mode exit 2, naming it and,
# at the end of the line, what provides it, rather than reading as a failure of the
# deliverable. The gate makes the replay cold itself: every tool cache is under IOC_CACHE by
# env.sh's one table of them, and each recorded command runs with an empty TMPDIR of its
# own. A predicate reads the measured line, not what a tool prints of its progress. Each
# fault passed at 533506542a, or ended there in a verdict on the deliverable or in a reason
# that named nothing that provides what was missing; each honest form beside it passes
# there and here.

# 78. A tool missing from PATH. At 533506542a --replay, --shape and --landed read no tool
# their recorded commands, their judgement or their proof need before they judged, so each
# went on to a verdict, and --citations, --published, --admitted and --self-test named the
# tool with no word of what provides it. Each mode runs here, over the fixture or, for
# --admitted, over this tree, with PATH lacking one tool its table names, and cannot run,
# naming the tool and, last, what provides it. The honest form is the fixture with PATH
# whole, which passes each mode it judges.
missing_tool replay go 'curl -fsSL https://go\.dev/dl/go1\.27\.3\.linux-amd64\.tar\.gz \| tar -xzC /usr/local, then PATH=/usr/local/go/bin:\$PATH'
missing_tool shape dispatch 'the dispatch command of a Legion pod, /opt/legion/pi-envoy/bin/dispatch on its PATH'
missing_tool citations python3 'apt-get install python3'
missing_tool landed dispatch 'the dispatch command of a Legion pod, /opt/legion/pi-envoy/bin/dispatch on its PATH'
missing_tool published curl 'apt-get install curl'
missing_tool admitted python3 'apt-get install python3'
missing_tool self-test jq 'apt-get install jq'
dir="$(fresh tools-whole)"
out="$(fixture_run "$dir" --replay)"
ending_result "--replay with every tool of its table on PATH passes the fixture" 0 '^all 18 assertions passed$' "$out"
out="$(fixture_run "$dir" --shape)"
ending_result "--shape with every tool of its table on PATH passes the fixture" 0 '^all 9 assertions passed$' "$out"
out="$(fixture_run "$dir" --landed)"
ending_result "--landed with every tool of its table on PATH passes the fixture" 0 '^all 7 assertions passed$' "$out"

# 79. An environment variable missing. At 533506542a --replay with no DISPATCH_TOKEN_FILE
# exported no header, so every recorded read of the Dispatch API would fail as a row of
# the deliverable, and --published with a token file that is not there ended as Dispatch
# not answering. Each now cannot run, naming the variable and how to set it. The honest
# form is the fixture's --replay with the variable as this pod sets it.
dir="$(fresh variable-missing)"
out="$(DISPATCH_TOKEN_FILE='' fixture_run "$dir" --replay)"
missing_result "--replay with DISPATCH_TOKEN_FILE unset cannot run, naming it" replay 'the variable DISPATCH_TOKEN_FILE is not set' 'export DISPATCH_TOKEN_FILE=<a file holding the Dispatch bearer>, which a Legion pod sets' "$out"
out="$(DISPATCH_TOKEN_FILE="$dir/no-such-token" fixture_run "$dir" --published)"
missing_result "--published whose DISPATCH_TOKEN_FILE names no file cannot run, naming it" published 'the variable DISPATCH_TOKEN_FILE names .*/no-such-token, which is no readable file' 'export DISPATCH_TOKEN_FILE=<a file holding the Dispatch bearer>, which a Legion pod sets' "$out"
out="$(fixture_run "$dir" --replay)"
ending_result "--replay with DISPATCH_TOKEN_FILE naming a readable file passes the fixture" 0 '^all 18 assertions passed$' "$out"

# 80. --landed needs nothing installed, and a proof that fails is the step failing. Until
# round 45 --landed's table named the packages packages/pi-legion/package.json lists, so the
# mode could not run while node_modules lacked one of them, and judged the step only once all
# were there; both proofs now read fcc746a3 with git show (case 77), and the table names no
# install. Case 77 runs this tree's own --landed on a checkout with no node_modules; this
# case holds the rule on the fixture, offline. The fixture gains
# packages/pi-legion/package.json naming two packages, with no node_modules: the mode judges
# the step and passes; with the committed proof `false` it fails landed-proof, exit 1.
dir="$(fresh landed-nothing-installed)"
mkdir -p "$dir/repo/packages/pi-legion"
printf '{"name": "@legion/pi-legion", "private": true, "devDependencies": {"typescript": "^5.3.0", "zod": "4.3.6"}}\n' >"$dir/repo/packages/pi-legion/package.json"
out="$(fixture_run "$dir" --landed)"
ending_result "--landed over a package.json with no node_modules judges the step" 0 '^all 7 assertions passed$' "$out"
sed -i 's/"command": "true"/"command": "false"/' "$dir/repo/docs/research/internet-of-cognition/sources.json"
sed -i 's/^- \*\*Proven by:\*\* `true`$/- **Proven by:** `false`/' "$dir/judgement.md"
grep -qF '"command": "false"' "$dir/repo/docs/research/internet-of-cognition/sources.json" && grep -qF '**Proven by:** `false`' "$dir/judgement.md" || {
    printf 'FAIL the failing proof could not be planted\n'
    exit 1
}
out="$(fixture_run "$dir" --landed)"
ending_result "--landed over a package.json with no node_modules and a proof that fails is a FAIL of the step" 1 '^1 of 7 assertions failed: landed-proof$' "$out"

# 81. The history and the clone root. At 533506542a --shape with an IOC_HISTORY_REF that
# names no commit failed order-of-work, blaming the deliverable for a fetch the reader had
# not made, and --landed and --citations ended without naming what provides it. Each now
# cannot run, naming the ref or the directory and what provides it. The honest forms are
# the fixture's own branch and the default clone root.
dir="$(fresh history-missing)"
out="$(IOC_HISTORY_REF=refs/heads/no-such-history fixture_run "$dir" --shape)"
missing_result "--shape whose IOC_HISTORY_REF names no commit cannot run, naming it" shape "the history ref 'refs/heads/no-such-history' names no commit of this repository" 'git fetch origin refs/pull/3771/head:refs/remotes/origin/pr-3771, then export IOC_HISTORY_REF=refs/remotes/origin/pr-3771' "$out"
out="$(IOC_HISTORY_REF=refs/heads/no-such-history fixture_run "$dir" --landed)"
missing_result "--landed whose IOC_HISTORY_REF names no commit cannot run, naming it" landed "the history ref 'refs/heads/no-such-history' names no commit of this repository" 'git fetch origin refs/pull/3771/head:refs/remotes/origin/pr-3771, then export IOC_HISTORY_REF=refs/remotes/origin/pr-3771' "$out"
out="$(IOC_HISTORY_REF=refs/heads/main fixture_run "$dir" --shape)"
ending_result "--shape whose IOC_HISTORY_REF names the fixture's branch passes" 0 '^all 9 assertions passed$' "$out"
out="$(IOC_CLONES="$dir/judgement.md/clones" fixture_run "$dir" --citations)"
missing_result "--citations whose IOC_CLONES cannot be made cannot run, naming it" citations 'IOC_CLONES names .*/judgement\.md/clones, which this run cannot make' 'export IOC_CLONES=<a directory this user can write>, or leave it unset for \$TMPDIR/ioc-clones' "$out"
out="$(fixture_run "$dir" --citations)"
ending_result "--citations with the default clone root passes the fixture" 0 '^all 6 assertions passed$' "$out"

# 82. Each recorded command runs with an empty TMPDIR of its own. At 533506542a a row ran
# with the caller's TMPDIR, so a row that read what an earlier run had left there, as
# m-ctl-our-row-count read go's caches at $TMPDIR/gomod, passed on the machine that left it
# and nowhere else. The fault reads a file the caller's TMPDIR holds and fails; the honest
# form writes into its own TMPDIR and reads it back.
dir="$(fresh tmpdir-left-behind)"
mkdir -p "$dir/outer-tmp"
printf 'ran once\n' >"$dir/outer-tmp/left-behind"
plant_behaviour "$dir" 'cat "$TMPDIR/left-behind"' '{"contains":"ran once"}'
case_result "a row that reads what the caller's TMPDIR holds fails" "replay-command: f-behaviour-1 ran its command and a read inside it failed: 'cat [^']*/left-behind ?' exited 1" "$(TMPDIR="$dir/outer-tmp" fixture_run "$dir" --replay)"
dir="$(fresh tmpdir-own)"
mkdir -p "$dir/outer-tmp"
plant_behaviour "$dir" "printf 'ran once\\n' >\"\$TMPDIR/own\" && cat \"\$TMPDIR/own\"" '{"contains":"ran once"}'
honest_result "a row that writes into its own TMPDIR and reads it back passes" replay-command "$(TMPDIR="$dir/outer-tmp" fixture_run "$dir" --replay)"

# 83. A row whose tool writes progress into the stream on a cold cache is held to its
# measured line, and the gate makes the cache cold. At 533506542a a row's tool read the
# caller's module cache, so a cache a machine had filled before stayed warm at a fresh
# IOC_CACHE and go printed "rows 38" alone; a predicate over the whole output passed there
# and failed on a machine that never ran it. The case serves a module from a file proxy
# offline and fills go's own default module cache with it, $HOME/go/pkg/mod under a HOME of
# its own, as a machine that ran the row before would hold it. fixture_run carries HOME and
# passes on no GOMODCACHE, GOCACHE or GOPATH of its caller's, so a go given no cache
# variable would read that warm default; what the case proves is that the gate gives it
# one, by env.sh's table, under the fresh IOC_CACHE the row runs at. So go downloads the
# module and prints "go: downloading" before "rows 38": the predicate {"equals":"rows 38"}
# fails, by name, and the module stands under IOC_CACHE afterwards; {"matches":"^rows
# 38$"}, the measured line, passes. It needs go, and prints SKIP when go is missing.
if command -v go >/dev/null 2>&1; then
    gocold="$WORK/go-cold"
    mkdir -p "$gocold/src/example.com/ioccount@v1.0.0" "$gocold/proxy/example.com/ioccount/@v"
    printf 'module example.com/ioccount\n\ngo 1.21\n' >"$gocold/src/example.com/ioccount@v1.0.0/go.mod"
    printf 'package ioccount\n\n// Rows is the count the row prints.\nconst Rows = 38\n' >"$gocold/src/example.com/ioccount@v1.0.0/count.go"
    (cd "$gocold/src" && python3 -c 'import sys, zipfile
z = zipfile.ZipFile(sys.argv[1], "w")
for name in sys.argv[2:]:
    z.write(name)
z.close()' "$gocold/proxy/example.com/ioccount/@v/v1.0.0.zip" example.com/ioccount@v1.0.0/go.mod example.com/ioccount@v1.0.0/count.go)
    cp "$gocold/src/example.com/ioccount@v1.0.0/go.mod" "$gocold/proxy/example.com/ioccount/@v/v1.0.0.mod"
    printf 'v1.0.0\n' >"$gocold/proxy/example.com/ioccount/@v/list"
    printf '{"Version":"v1.0.0"}\n' >"$gocold/proxy/example.com/ioccount/@v/v1.0.0.info"
    # GOPROXY, GOSUMDB and GOTOOLCHAIN are carried into the fixture, so its go reads the
    # file proxy and no other; the cache variables set here fill the warm default alone.
    export GOPROXY="file://$gocold/proxy" GOSUMDB=off GOTOOLCHAIN=local
    (cd "$gocold" && HOME="$gocold/home" GOMODCACHE="$gocold/home/go/pkg/mod" GOCACHE="$gocold/home/.cache/go-build" \
        go mod download example.com/ioccount@v1.0.0) || {
        printf 'FAIL the module cache under the HOME of the case could not be filled from the file proxy\n'
        exit 1
    }
    for form in fault honest; do
        dir="$(fresh "go-cold-$form")"
        mkdir -p "$dir/repo/gocount/cmd/count"
        printf 'module example.com/gocount\n\ngo 1.21\n\nrequire example.com/ioccount v1.0.0\n' >"$dir/repo/gocount/go.mod"
        printf 'package main\n\nimport (\n\t"fmt"\n\n\t"example.com/ioccount"\n)\n\nfunc main() { fmt.Println("rows", ioccount.Rows) }\n' >"$dir/repo/gocount/cmd/count/main.go"
        if [ "$form" = fault ]; then
            plant_behaviour "$dir" 'cd gocount && GOFLAGS=-mod=mod go run ./cmd/count' '{"equals":"rows 38"}'
            out="$(HOME="$gocold/home" IOC_CACHE="$dir/cache" fixture_run "$dir" --replay)"
            case_result "a row whose go downloads on a cold cache fails a predicate over the whole output" 'replay-command: f-behaviour-1 ran .* does not meet \{"equals":"rows 38"\}: go: downloading example\.com/ioccount v1\.0\.0' "$out"
            if case_read "the module a row downloads stands under IOC_CACHE, in the module cache env.sh names" "$out"; then
                if [ -d "$dir/cache/cache/go/mod/example.com/ioccount@v1.0.0" ]; then
                    case_passed "the module a row downloads stands under IOC_CACHE, in the module cache env.sh names"
                else
                    case_failed "the module a row downloads stands under IOC_CACHE, in the module cache env.sh names" "$dir/cache/cache/go/mod holds no example.com/ioccount@v1.0.0"
                fi
            fi
        else
            plant_behaviour "$dir" 'cd gocount && GOFLAGS=-mod=mod go run ./cmd/count' '{"matches":"^rows 38$"}'
            out="$(HOME="$gocold/home" IOC_CACHE="$dir/cache" fixture_run "$dir" --replay)"
            ending_result "a row whose go downloads on a cold cache meets the measured line, ^rows 38\$" 0 '^all 18 assertions passed$' "$out"
        fi
    done
    unset GOPROXY GOSUMDB GOTOOLCHAIN
    # go writes its module cache read-only, and the EXIT trap removes $WORK.
    chmod -R u+w "$WORK"
else
    printf 'SKIP the replay of a go row on a cold cache: go is missing\n'
fi

# 84-87 come from the eleventh review of the pull request and the architect's ruling on
# it, and the header of check.sh states each rule they hold: a run that cannot run ends
# exit 2 and is never counted as failed assertions, a fixture runs in an environment of
# its own, and nothing a killed run leaves is trusted. Each fault ended at 0fdbb24ddf in a
# count of failed assertions, in a fixture that read its caller's environment, or in a
# pass on what a killed run left; each honest form beside it passes there and here.
#
# 84. A fixture runs in an environment of its own, not its caller's. At 0fdbb24ddf
# fixture_run passed every variable of its caller on, so an IOC_HISTORY_REF exported as
# the history line of `check.sh --help` says, which names a commit of this tree and none
# of the fixture's, left the fixture's --shape and --landed unable to run. A shell that
# sources check.sh, as this file does, with this tree's HEAD exported as IOC_HISTORY_REF,
# now runs each over the fixture and passes, and so does the same shell with nothing
# exported, the honest form. A variable the header names nowhere does not reach a
# fixture's row either: a row that prints one prints nothing of it.
head_commit="$(git -C "$IOC_ROOT" rev-parse HEAD)"
dir="$(fresh history-exported)"
for mode in shape:9 landed:7; do
    out="$(IOC_HISTORY_REF="$head_commit" bash -c '. "$1"; fixture_run "$2" "$3"' _ "$HERE/check.sh" "$dir" "--${mode%:*}")"
    ending_result "--${mode%:*} over the fixture passes from a shell that exported IOC_HISTORY_REF, a commit of this tree" 0 "^all ${mode#*:} assertions passed\$" "$out"
    out="$(bash -c '. "$1"; fixture_run "$2" "$3"' _ "$HERE/check.sh" "$dir" "--${mode%:*}")"
    ending_result "--${mode%:*} over the fixture passes from a shell that exported no IOC_HISTORY_REF" 0 "^all ${mode#*:} assertions passed\$" "$out"
done
dir="$(fresh caller-variable)"
plant_behaviour "$dir" "printf 'ran once%s\\n' \"\$IOC_CALLER_PROBE\"" '{"equals":"ran once"}'
out="$(IOC_CALLER_PROBE=' and a variable of the caller' fixture_run "$dir" --replay)"
ending_result "a variable of the caller's that the header does not name does not reach a fixture's row" 0 '^all 18 assertions passed$' "$out"

# 85. A run the self-test makes that cannot run is a self-test that cannot run. At
# 0fdbb24ddf mode_self_test looked only for the all-passed line at the end of each run it
# made, so a fixture run that ended "check.sh cannot run" became FAIL self-test-baseline,
# and the self-test ended "39 of 39 assertions failed", exit 1: with a Dispatch that does
# not answer, through the fixture's --citations, and with IOC_HISTORY_REF exported,
# through its --shape and --landed (the eleventh review, item 3). The case runs the
# --self-test of a scratch copy of this directory whose check.test.sh is a stub that
# passes, so that it does not run this file again. With a Dispatch that does not answer it
# ends exit 2, naming the fixture's --citations and, last, Dispatch; with IOC_HISTORY_REF
# exported beside it, it ends the same way, so the fixture's --shape and --replay, which
# run first, read no history of their caller's. The honest form is the same copy's
# --self-test, which passes.
#
# A check.test.sh that runs no case: it sources the copy's check.sh and writes its ending
# record, all 0 cases passed, by run_ending, as this file's summary does.
CASES_STUB='. "$(dirname "$0")/check.sh" && run_ending cases'
printf '%s\n' "$CASES_STUB" >"$WORK/cases-stub.sh"
self_copy="$(dir_copy self-test-copy "$WORK/cases-stub.sh")"
SELF_CANNOT='^check\.sh cannot run --self-test: --citations over the fixture deliverable could not run, so the self-test stops at self-test-baseline: check\.sh cannot run --citations: Dispatch, the read of dispatch://[^ ]* did not answer'
out="$(DISPATCH_URL=http://127.0.0.1:1 IOC_EVIDENCE="$self_copy" gate_run "$self_copy/check.sh" --self-test)"
ending_result "--self-test whose fixture's --citations has no Dispatch to read cannot run, naming the run and Dispatch" 2 "$SELF_CANNOT" "$out"
out="$(IOC_HISTORY_REF="$head_commit" DISPATCH_URL=http://127.0.0.1:1 IOC_EVIDENCE="$self_copy" gate_run "$self_copy/check.sh" --self-test)"
ending_result "--self-test with IOC_HISTORY_REF exported runs its fixture on the fixture's own history, and ends naming Dispatch" 2 "$SELF_CANNOT" "$out"
out="$(IOC_EVIDENCE="$self_copy" gate_run "$self_copy/check.sh" --self-test)"
ending_result "--self-test of the copy passes when every run it makes can run" 0 '^all 46 assertions passed$' "$out"

# 86. --admitted reads the status of the install reader. At 0fdbb24ddf it read the
# classifier's lines through a process substitution, so no status of python3 reached it:
# one line of claims.jsonl that is no JSON object, the eleventh review's
# {"id": "broken-row", "kind": , stopped the reader with a traceback and the mode ended
# "4 of 4 assertions failed", as it did with env.sh missing and with a reader that does not
# start. A line that is no row is the evidence being wrong: the one parse fails it in
# admitted-rows alone, exit 1, naming the line by its number. A reader that cannot load is
# a mode that cannot run, exit 2. Each form runs --admitted over a scratch copy of
# claims.jsonl and env.sh, or, for the reader that does not start, a scratch copy of
# check.sh, as cases 41 and 42 do; the honest form is the copy as it stands.
admitted_copy() {
    # admitted_copy <name>; prints a scratch directory holding this directory's
    # claims.jsonl, env.sh and evidence/, which --admitted reads as its IOC_EVIDENCE.
    local copy="$WORK/$1"
    mkdir -p "$copy"
    cp "$HERE/claims.jsonl" "$HERE/env.sh" "$copy/"
    ln -s "$HERE/evidence" "$copy/evidence"
    printf '%s' "$copy"
}
copy="$(admitted_copy admitted-broken-line)"
printf '%s\n' '{"id": "broken-row", "kind": ' >>"$copy/claims.jsonl"
out="$(IOC_EVIDENCE="$copy" gate_run "$HERE/check.sh" --admitted)"
ending_result "a line of claims.jsonl that is no JSON object fails admitted-rows alone, exit 1" 1 '^1 of 4 assertions failed: admitted-rows$' "$out"
case_result "a line of claims.jsonl that is no JSON object is named by its number" "admitted-rows: line $(wc -l <"$copy/claims.jsonl") of claims.jsonl is no row" "$out"
copy="$(admitted_copy admitted-no-env)"
rm "$copy/env.sh"
out="$(IOC_EVIDENCE="$copy" gate_run "$HERE/check.sh" --admitted)"
ending_result "--admitted whose env.sh is missing cannot run: exit 2, the reason last" 2 '^check\.sh cannot run --admitted: env\.sh, which holds .* did not load\.$' "$out"
plant_gate admitted-reader-start 's/^import json, os, re, shlex, sys$/&, ioc_no_such_module_3134/' 'import json, os, re, shlex, sys, ioc_no_such_module_3134' holds
out="$(gate_run "$GATE" --admitted)"
ending_result "--admitted whose install reader does not start cannot run: exit 2, the reason last" 2 "^check\\.sh cannot run --admitted: the install reader cannot load: ModuleNotFoundError: No module named 'ioc_no_such_module_3134'\$" "$out"
copy="$(admitted_copy admitted-honest)"
out="$(IOC_EVIDENCE="$copy" gate_run "$HERE/check.sh" --admitted)"
ending_result "--admitted over a copy of claims.jsonl and env.sh passes" 0 '^all 4 assertions passed$' "$out"

# 87. Nothing a killed run leaves is trusted. A pod replacement kills a replay part-way.
# At 0fdbb24ddf env.sh wrote the shapes graph of the scope clone under its own name, so a
# generator killed after its first line left a file the next run reused as the whole
# graph, and the replay left env.sh's mark of a whole environment on one a row had
# installed into. The case builds a scope clone of its own, offline, whose generator
# writes one line and then, when asked, kills its own process group, as a pod replacement
# kills the run; env.sh builds the graph once killed and once more, and the graph must then
# hold the generator's second line. The honest form is a build nobody kills. Then, offline,
# in an environment case 66's world builds, a fixture's --replay of a row that installs
# into IOC_TC_WORK must find no stamp of env.sh's on it while the row runs, so that a kill
# during the install leaves none, and none after it, so the next run builds it again; a
# row that installs into nothing leaves the stamp, the honest form. At 9c82069807 the
# stamp half's row ran no step that read the stamp while it ran, so it passed with the
# removal before the row taken out.
killed="$WORK/killed-shapes"
mkdir -p "$killed/clones/scope/ontology/scripts" "$killed/clones/scope/ontology/src/scope_ontology" "$killed/cache" "$killed/honest-cache"
for name in mas-ontology metrics-ontology semantic-ontology analysis-ontology insight-ontology mas-shapes-custom; do
    printf '# %s\n' "$name" >"$killed/clones/scope/ontology/src/scope_ontology/$name.ttl"
done
cat >"$killed/clones/scope/ontology/scripts/generate_shacl_shapes.py" <<'PY'
import os, signal, sys
with open(sys.argv[1], "w") as out:
    out.write("# the first line of the generated shapes\n")
    out.flush()
    if os.environ.get("IOC_KILL_AFTER_FIRST_LINE"):
        os.killpg(0, signal.SIGKILL)
    out.write("# the second line of the generated shapes\n")
PY
# shapes_build <cache> [kill]; env.sh's build of the shapes graph under <cache>, in a
# process group of its own, whose generator kills that group after its first line when
# asked; the group around it keeps bash's report of the kill out of this file's output.
# shapes_whole <cache> is true when both files of the graph hold the second line.
shapes_build() {
    {
        IOC_CACHE="$1" IOC_CLONES="$killed/clones" IOC_PY="$(command -v python3)" IOC_KILL_AFTER_FIRST_LINE="${2:-}" \
            setsid -w bash -c 'IOC_ENV_FUNCTIONS_ONLY=1 . "$1" && ioc_env_scope_shapes' _ "$HERE/env.sh" >/dev/null
    } 2>/dev/null
}
shapes_whole() {
    grep -qxF '# the second line of the generated shapes' "$1/mas-shapes.ttl" 2>/dev/null &&
        grep -qxF '# the second line of the generated shapes' "$1/scope-shapes.ttl" 2>/dev/null
}
if command -v setsid >/dev/null 2>&1; then
    shapes_build "$killed/cache" kill
    shapes_build "$killed/cache"
    if shapes_whole "$killed/cache"; then
        case_passed "the shapes graph a killed build left part-written is built whole by the next run"
    else
        case_failed "the shapes graph a killed build left part-written is built whole by the next run" "the graph holds $(head -c 160 "$killed/cache/scope-shapes.ttl" 2>/dev/null | tr '\n' ' ')"
    fi
    shapes_build "$killed/honest-cache"
    if shapes_whole "$killed/honest-cache"; then
        case_passed "the shapes graph of a build nobody killed is whole"
    else
        case_failed "the shapes graph of a build nobody killed is whole" "the graph holds $(head -c 160 "$killed/honest-cache/scope-shapes.ttl" 2>/dev/null | tr '\n' ' ')"
    fi
else
    printf 'SKIP a killed build of the shapes graph: setsid is missing\n'
fi
# stamp_result <name> <gone|stands> <stamp file> <run>; passes when the replay passed every
# assertion and, after it, the stamp is gone, or stands, as named.
stamp_result() {
    case_read "$1" "$4" || return 0
    if [ "$RUN_ENDING" = passed ] && { { [ "$2" = gone ] && [ ! -e "$3" ]; } || { [ "$2" = stands ] && [ -e "$3" ]; }; }; then
        case_passed "$1"
    else
        case_failed "$1" "the stamp is to be $2 after the run, and the run ended $RUN_LINE, $(grep '^FAIL' "$4/text" | head -1)"
    fi
}
# The planted row installs into IOC_TC_WORK as the install reader reads it, and, while it
# runs, searches IOC_TC_WORK's environment for the stamp, printing the stamp's path when it
# is there, and then ran once. Its predicate {"equals":"ran once"} holds only when the
# stamp was already gone while the row ran, which the removal before the row does and the
# removal after it cannot; the stamp gone after the run is the second half.
if freeze_world && freeze_venv stamped && freeze_venv kept; then
    printf 'built by env.sh\n' >"$FREEZE/stamped/venv/.ioc-built-from"
    printf 'built by env.sh\n' >"$FREEZE/kept/venv/.ioc-built-from"
    dir="$(fresh stamp-install)"
    plant_behaviour "$dir" "true || UV_REQUIRE_HASHES=0 uv pip install --python \"\$IOC_TC_WORK/venv/bin/python\" -c \"\$IOC_EVID/ioc-venv.lock\" -b \"\$IOC_EVID/build-constraints.txt\" --require-build-hashes ./local; grep -rlF 'built by env.sh' \"\$IOC_TC_WORK/venv\"; printf 'ran once\\n'" '{"equals":"ran once"}'
    out="$(IOC_EVID="$dir/repo/docs/research/internet-of-cognition/evidence" IOC_TC_WORK="$FREEZE/stamped" IOC_CLONES="$FREEZE/clones" fixture_run "$dir" --replay)"
    stamp_result "a replay whose row installs into IOC_TC_WORK removes env.sh's stamp of it before the row runs, and it stays gone" gone "$FREEZE/stamped/venv/.ioc-built-from" "$out"
    dir="$(fresh stamp-no-install)"
    out="$(IOC_EVID="$dir/repo/docs/research/internet-of-cognition/evidence" IOC_TC_WORK="$FREEZE/kept" IOC_CLONES="$FREEZE/clones" fixture_run "$dir" --replay)"
    stamp_result "a replay whose rows install into nothing leaves env.sh's stamp of IOC_TC_WORK" stands "$FREEZE/kept/venv/.ioc-built-from" "$out"
else
    printf 'SKIP the stamp of an environment a row installs into: uv, or an environment it builds here offline, is missing\n'
fi

# 88-89 come from the twelfth review of the pull request and the architect's rulings on it
# of 2026-11-15T22:45Z and 23:10Z, and the header of check.sh states each rule they hold:
# the one parse of claims.jsonl, and the three endings, read in one way from each run's
# ending record. Each fault ended at 9c82069807 in a mode that could not run, in a pass, or
# in a failure read for a run that could not run; each honest form beside it passes there
# and here.

# 88. One parse of claims.jsonl, before any environment is built or any reader starts. At
# 9c82069807 --replay read claims.jsonl through the install reader, which stopped on a
# line that is no JSON and on a row whose command is a list, so the mode ended exit 2 as if
# it could not run; --citations read the rows with jq, which passed over both lines with
# no word, all 5 assertions passed; --admitted's classifier stopped on a row whose command
# is a list, exit 2, and counted a row whose id is a number into its table, so that
# admitted-header failed and no line was named; and --self-test took that --admitted for a
# run that could not run, exit 2. Each mode that reads the file now fails one assertion for
# the line, naming it, and ends exit 1 with its declared assertions counted: --replay
# claim-form, --citations citation-kinds, --admitted admitted-rows, and --self-test, which
# reads the file through its run of --admitted, admitted-table. Each form appends one line
# to the fixture's claims.jsonl, or to a copy of this directory's; the honest form is the
# same mode over the same file without it.
# A case that holds a run to failing, where the gate of 9c82069807 said it could not run,
# reads the run by case_read's one rule: a reason from outside the tree makes the case one
# that could not run, and any other is the fault the case plants, and fails it.
BROKEN_LINE='{"id": "broken-row", "kind": '
LIST_COMMAND='{"id":"command-list","requirement":"r7","source":"the case","kind":"behaviour","claim":"A command that is a list.","mark":"verified","evidence":{"surface":"cli"},"command":["ls"],"expect":{"contains":"ls"},"note":"planted"}'
NUMBER_ID='{"id":7,"requirement":"r7","source":"the case","kind":"behaviour","claim":"An id that is a number.","mark":"verified","evidence":{"surface":"cli"},"command":"printf ran","expect":{"contains":"ran"},"note":"planted"}'
for mode in replay:18:claim-form citations:6:citation-kinds; do
    family="${mode##*:}"
    count="${mode#*:}"
    count="${count%%:*}"
    mode="${mode%%:*}"
    for form in 'BROKEN_LINE:a line cut short' 'LIST_COMMAND:a row whose command is a list'; do
        planted="${form%%:*}"
        dir="$(fresh "parse-$mode-$planted")"
        claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
        printf '%s\n' "${!planted}" >>"$claims_copy"
        out="$(fixture_run "$dir" "--$mode")"
        one_failure_result "--$mode over a claims.jsonl holding ${form#*:} fails $family alone, naming the line, exit 1" "$family" \
            "^1 of $count assertions failed: $family\$" "^FAIL $family: line $(wc -l <"$claims_copy") of claims\\.jsonl is no row" "$out"
    done
    dir="$(fresh "parse-$mode-honest")"
    out="$(fixture_run "$dir" "--$mode")"
    ending_result "--$mode over the fixture's claims.jsonl as it stands passes" 0 "^all $count assertions passed\$" "$out"
done
for form in 'LIST_COMMAND:a row whose command is a list' 'NUMBER_ID:a row whose id is a number'; do
    planted="${form%%:*}"
    copy="$(admitted_copy "parse-admitted-$planted")"
    printf '%s\n' "${!planted}" >>"$copy/claims.jsonl"
    out="$(IOC_EVIDENCE="$copy" gate_run "$HERE/check.sh" --admitted)"
    one_failure_result "--admitted over a claims.jsonl holding ${form#*:} fails admitted-rows alone, naming the line, exit 1" admitted-rows \
        '^1 of 4 assertions failed: admitted-rows$' "^FAIL admitted-rows: line $(wc -l <"$copy/claims.jsonl") of claims\\.jsonl is no row" "$out"
done
copy="$(dir_copy parse-self-test "$WORK/cases-stub.sh")"
printf '%s\n' "$LIST_COMMAND" >>"$copy/claims.jsonl"
out="$(IOC_EVIDENCE="$copy" gate_run "$copy/check.sh" --self-test)"
one_failure_result "--self-test over a claims.jsonl holding a row whose command is a list fails admitted-table alone, exit 1" admitted-table \
    '^1 of 46 assertions failed: admitted-table$' "^FAIL admitted-table: .*admitted-rows: line $(wc -l <"$copy/claims.jsonl") of claims\\.jsonl is no row" "$out"

# 89. check.test.sh ends the three ways the gate does, and --self-test reads it by its
# ending record. At 9c82069807 case_result, passed_result and ending_result read a fixture
# run that could not run as a failed case and honest_result read it as a passed one, so a
# Dispatch that did not answer during one case ended this file exit 1, or exit 0, and
# --self-test then failed gate-cases, or passed it; and --self-test took any exit 2 of this
# file, a bash syntax error's among them, for a run that could not run. The case writes
# copies of this file from its own text: its helpers, then one case, then its summary. The
# first copy holds case 4 with a Dispatch that does not answer given to its --citations,
# and ends exit 2 naming the case and Dispatch; the second holds an honest form, read by
# honest_result, with the same Dispatch, and ends the same way; the third holds case 4 and
# then a syntax error, which ends it with bash's own line and no ending record.
# --self-test over a copy of this directory whose check.test.sh is the first ends exit 2,
# naming the case and Dispatch, and over one whose check.test.sh is the third ends exit 1
# on gate-cases, since a crash is a failure. The honest form is the copy of case 4 with
# Dispatch answering, which passes. Two forms hold the precedence: a copy holding the
# first copy's case and a case that fails ends exit 1, naming the failure and then the case
# that could not run; and a mode, the fixture's --citations with a line cut short in its
# claims.jsonl and the same Dispatch, ends exit 1, naming the assertion that failed and
# then the four that could not run, where at 9c82069807 it ended exit 2 and named neither.
# cases_copy <name> <case text>; a copy of this directory, by dir_copy, whose check.test.sh
# is this file down to the marker line its cases begin below, then <case text>, then its
# summary, from the marker line the summary begins below; it prints the copy's path. Each
# marker stands once in this file, or no copy is cut, so a copy can never hold this case
# and run it again inside itself.
CASES_MARK='# ==== the cases of check.test.sh begin below this line, where cases_copy cuts it ===='
SUMMARY_MARK='# ==== the summary of check.test.sh begins below this line, where cases_copy cuts it ===='
cases_copy() {
    local mark
    for mark in "$CASES_MARK" "$SUMMARY_MARK"; do
        [ "$(grep -cxF -- "$mark" "$HERE/check.test.sh")" = 1 ] || {
            printf 'FAIL check.test.sh holds the line "%s" other than once, so no copy of it can be cut\n' "$mark"
            exit 1
        }
    done
    {
        awk -v mark="$CASES_MARK" '$0 == mark { exit } { print }' "$HERE/check.test.sh"
        printf '%s\n' "$2"
        awk -v mark="$SUMMARY_MARK" 'found { print } $0 == mark { found = 1 }' "$HERE/check.test.sh"
    } >"$WORK/$1.cases"
    dir_copy "$1" "$WORK/$1.cases"
}
case_four="$(sed -n '/^# 4\. /,/^# 5\. /p' "$HERE/check.test.sh" | sed '$d')"
[ "$(grep -c '^# 4\. ' "$HERE/check.test.sh")" = 1 ] && [ "$(grep -c '^# 5\. ' "$HERE/check.test.sh")" = 1 ] &&
    [ "$(printf '%s\n' "$case_four" | grep -c '^case_result ')" = 1 ] || {
    printf 'FAIL case 4 could not be cut from check.test.sh: its first line and the first line of case 5 are each to stand once, with one case_result between them\n'
    exit 1
}
outage="$(printf '%s\n' "$case_four" | sed 's#"$(fixture_run "$dir" --citations)"$#"$(DISPATCH_URL=http://127.0.0.1:1 fixture_run "$dir" --citations)"#')"
printf '%s\n' "$outage" | grep -qF 'DISPATCH_URL=http://127.0.0.1:1 fixture_run "$dir" --citations' || {
    printf 'FAIL the Dispatch outage could not be planted in case 4 of a copy of check.test.sh\n'
    exit 1
}
CASE_FOUR_NAME='a project citation whose quoted text the file does not hold is refused'
DISPATCH_DOWN='check\.sh cannot run --citations: Dispatch, the read of dispatch://[^ ]* did not answer'
copy="$(cases_copy cases-outage "$outage")"
out="$(gate_run "$copy/check.test.sh")"
ending_result "a case whose --citations has no Dispatch could not run, and check.test.sh ends exit 2 naming it and Dispatch" 2 \
    "^check\\.sh cannot run check\\.test\\.sh: 1 of 1 cases could not run: $CASE_FOUR_NAME: $DISPATCH_DOWN" "$out" "$DISPATCH_DOWN"
out="$(IOC_EVIDENCE="$copy" gate_run "$copy/check.sh" --self-test)"
ending_result "--self-test whose check.test.sh meets no Dispatch in one case ends exit 2, naming the case and Dispatch" 2 \
    "^check\\.sh cannot run --self-test: check\\.test\\.sh could not run, so the self-test stops at gate-cases: check\\.sh cannot run check\\.test\\.sh: 1 of 1 cases could not run: $CASE_FOUR_NAME: $DISPATCH_DOWN" "$out"
copy="$(cases_copy cases-honest-outage "dir=\"\$(fresh honest-no-dispatch)\"
honest_result \"an honest --citations names no failure of citation-dispatch\" citation-dispatch \"\$(DISPATCH_URL=http://127.0.0.1:1 fixture_run \"\$dir\" --citations)\"")"
out="$(gate_run "$copy/check.test.sh")"
ending_result "an honest form whose --citations has no Dispatch could not run, never passed, and check.test.sh ends exit 2" 2 \
    "^check\\.sh cannot run check\\.test\\.sh: 1 of 1 cases could not run: an honest --citations names no failure of citation-dispatch: $DISPATCH_DOWN" "$out" "$DISPATCH_DOWN"
copy="$(cases_copy cases-honest "$case_four")"
out="$(gate_run "$copy/check.test.sh")"
ending_result "the copy of case 4 with Dispatch answering passes" 0 '^all 1 cases passed$' "$out"
copy="$(cases_copy cases-precedence "$outage
case_result \"a planted case that the gate passes\" replay-command \"\$(fixture_run \"\$(fresh precedence-passes)\" --replay)\"")"
out="$(gate_run "$copy/check.test.sh")"
ending_result "check.test.sh with a failed case and a case that could not run ends exit 1, naming the failure first" 1 \
    "^1 of 2 cases failed; 1 could not run: $CASE_FOUR_NAME: $DISPATCH_DOWN" "$out" "$DISPATCH_DOWN"
dir="$(fresh precedence-citations)"
printf '%s\n' "$BROKEN_LINE" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(DISPATCH_URL=http://127.0.0.1:1 fixture_run "$dir" --citations)"
ending_result "a mode that failed an assertion and then met no Dispatch ends exit 1, naming the failure and then what could not run" 1 \
    "^1 of 6 assertions failed: citation-kinds; 5 could not run: citation-our-code citation-project-code citation-published citation-dispatch citation-figures, since $DISPATCH_DOWN" "$out" "$DISPATCH_DOWN"
copy="$(cases_copy cases-syntax "$case_four
fi")"
out="$(gate_run "$copy/check.test.sh")"
ending_read "$out"
if [ "$RUN_CRASH" != 1 ] || [ -e "$out/ending" ] || ! grep -q "syntax error near unexpected token \`fi'" "$out/text"; then
    printf 'FAIL the syntax error could not be planted in a copy of check.test.sh: %s\n' "$RUN_LINE"
    exit 1
fi
out="$(IOC_EVIDENCE="$copy" gate_run "$copy/check.sh" --self-test)"
one_failure_result "--self-test whose check.test.sh ends in a bash syntax error, exit 2 with no ending record, fails gate-cases" gate-cases \
    '^1 of 46 assertions failed: gate-cases$' '^FAIL gate-cases: check\.test\.sh does not pass, exit 2: ' "$out"

# 90-92 come from the thirteenth review of the pull request and the architect's rulings on
# it of 2026-11-16T06:40Z and 07:08Z, and the header of check.sh states each rule they hold:
# one ending record of each run, which every case and --self-test read the run by; env.sh
# reading a value of a row from its evidence alone; and an id naming one row. Each fault
# ended at beda0a6c32 in a failed case for a run that could not run, in a pass for a run
# that wrote no record, in env.sh refusing or keeping a corpus of any length, or in a pass;
# each honest form beside it passes there and here.
#
# 90. A case reads its run by the run's own record (thread PRRT_kwDORFy7ds63u3C5). At
# beda0a6c32 one_failure_result excused a run only when it ended exit 2, and ending_result
# held a run to the one reason it planted, so an outage from outside the tree that a case
# did not plant counted it as failed: with GitHub cut and an empty clone root, case 88's
# --citations forms failed citation-kinds, could not fetch mas-lab and ended exit 1, and a
# form that plants a Dispatch that does not answer met GitHub first. Each form is a copy of
# this file holding one such case, which now could not run, so the copy ends exit 2 naming
# it and the source: case 88's two --citations forms with GitHub cut; a form that plants a
# Dispatch that does not answer, with GitHub cut; and case 88's line cut short beside a
# Dispatch that does not answer, an outage it does not plant. And a run whose text ends
# with the all-passed line, exit 0, but that wrote no record, is no pass: the fixture's
# gate planted to print that line and stop reads as a crash. The honest form is the copy of
# case 88's two --citations forms with GitHub answering, which passes.
GITHUB_DOWN='check\.sh cannot run --citations: GitHub, the fetch of the clone of mas-lab did not answer'
# github_cut <command...>, defined in each copy below: the command run with GitHub cut, as
# the review cut it, an empty clone root and every https read sent to a port nobody
# answers; the honest copy defines it as the command alone.
CUT_DEFINED="$(printf 'BROKEN_LINE=%q\nLIST_COMMAND=%q\nDISPATCH_DOWN=%q\n' "$BROKEN_LINE" "$LIST_COMMAND" "$DISPATCH_DOWN")"
GITHUB_CUT='github_cut() { IOC_CLONES="$WORK/no-clones" https_proxy=http://127.0.0.1:1 HTTPS_PROXY=http://127.0.0.1:1 no_proxy= NO_PROXY= "$@"; }'
GITHUB_WHOLE='github_cut() { "$@"; }'
PARSE_CITATIONS="$(cat <<'CASE'
for planted in BROKEN_LINE LIST_COMMAND; do
    dir="$(fresh "cut-$planted")"
    claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    printf '%s\n' "${!planted}" >>"$claims_copy"
    one_failure_result "--citations over a claims.jsonl holding $planted fails citation-kinds alone, naming the line, exit 1" citation-kinds \
        '^1 of 6 assertions failed: citation-kinds$' "^FAIL citation-kinds: line $(wc -l <"$claims_copy") of claims\\.jsonl is no row" \
        "$(github_cut fixture_run "$dir" --citations)"
done
CASE
)"
copy="$(cases_copy cut-parse "$CUT_DEFINED
$GITHUB_CUT
$PARSE_CITATIONS")"
ending_result "case 88's --citations forms with GitHub cut could not run, and check.test.sh ends exit 2 naming each and GitHub" 2 \
    "^check\\.sh cannot run check\\.test\\.sh: 2 of 2 cases could not run: [^;]*BROKEN_LINE[^;]*: $GITHUB_DOWN.*; [^;]*LIST_COMMAND[^;]*: $GITHUB_DOWN" "$(gate_run "$copy/check.test.sh")" "$GITHUB_DOWN"
copy="$(cases_copy cut-parse-honest "$CUT_DEFINED
$GITHUB_WHOLE
$PARSE_CITATIONS")"
ending_result "case 88's --citations forms in a copy with GitHub answering pass" 0 '^all 2 cases passed$' "$(gate_run "$copy/check.test.sh")"
copy="$(cases_copy cut-dispatch-planted "$CUT_DEFINED
$GITHUB_CUT
$(cat <<'CASE'
dir="$(fresh cut-dispatch-planted)"
ending_result "--citations with no Dispatch to read cannot run, naming Dispatch" 2 "^$DISPATCH_DOWN" "$(DISPATCH_URL=http://127.0.0.1:1 github_cut fixture_run "$dir" --citations)"
CASE
)")"
ending_result "a case that plants a Dispatch that does not answer, with GitHub cut, could not run, and check.test.sh ends exit 2 naming it and GitHub" 2 \
    "^check\\.sh cannot run check\\.test\\.sh: 1 of 1 cases could not run: --citations with no Dispatch to read cannot run, naming Dispatch: $GITHUB_DOWN" "$(gate_run "$copy/check.test.sh")" "$GITHUB_DOWN"
copy="$(cases_copy dispatch-beside-failure "$CUT_DEFINED
$(cat <<'CASE'
dir="$(fresh dispatch-beside-failure)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
printf '%s\n' "$BROKEN_LINE" >>"$claims_copy"
one_failure_result "--citations over a claims.jsonl holding a line cut short fails citation-kinds alone, beside a Dispatch that does not answer" citation-kinds \
    '^1 of 6 assertions failed: citation-kinds$' "^FAIL citation-kinds: line $(wc -l <"$claims_copy") of claims\\.jsonl is no row" \
    "$(DISPATCH_URL=http://127.0.0.1:1 fixture_run "$dir" --citations)"
CASE
)")"
ending_result "a planted failure beside an outage it does not plant could not run, neither failed nor passed, and check.test.sh ends exit 2" 2 \
    "^check\\.sh cannot run check\\.test\\.sh: 1 of 1 cases could not run: --citations over a claims\\.jsonl holding a line cut short fails citation-kinds alone, beside a Dispatch that does not answer: $DISPATCH_DOWN" "$(gate_run "$copy/check.test.sh")" "$DISPATCH_DOWN"
dir="$(fresh no-record)"
sed -i "s/^    --replay) run_mode replay ;;\$/    --replay) printf 'all 18 assertions passed\\\\n' ;;/" "$dir/repo/docs/research/internet-of-cognition/check.sh"
grep -qxF "    --replay) printf 'all 18 assertions passed\\n' ;;" "$dir/repo/docs/research/internet-of-cognition/check.sh" || {
    printf 'FAIL the gate that prints the all-passed line and writes no record could not be planted in the fixture copy of check.sh\n'
    exit 1
}
out="$(fixture_run "$dir" --replay)"
ending_read "$out"
if [ "$RUN_ENDING" = failed ] && [ "$RUN_CRASH" = 1 ] && [ "$(tail -1 "$out/text")" = 'all 18 assertions passed' ]; then
    case_passed "a run whose text ends with the all-passed line, exit 0, but that wrote no ending record, reads as a crash"
else
    case_failed "a run whose text ends with the all-passed line, exit 0, but that wrote no ending record, reads as a crash" "it reads as $RUN_ENDING: $RUN_LINE"
fi

# 91. env.sh reads a value of a row from its evidence alone (the thirteenth review, item
# 3). At beda0a6c32 env.sh read the checksum of s-model-checksum from its {"contains": ...}
# and the floor of tc-corpus-1 from its {"at_least": ...}, so a row recorded again with
# another predicate, {"matches": ...} as the header's measured-line rule recommends, left
# env.sh no checksum, and --replay ended exit 2 with no FAIL before it, or no floor, and
# env.sh kept a corpus of any length. Each value is now a named field of the row's
# evidence, sha256 and corpus_floor, which the one parse holds to its type. The case runs
# env.sh's two reads as --replay's env.sh step makes them, ioc_env_model and
# ioc_env_reviews, over a copy of claims.jsonl whose two rows are recorded with
# {"matches": ...}: env.sh is to prepare the snapshot the row's sha256 pins, which the case
# plants under a sha256 of its own, and a corpus of at least the row's floor, fetched by a
# curl of the case's own that answers 30 reviews for each pull request over a corpus the case
# plants short, with an empty header file, no token, as IOC_GITHUB_AUTH_LEGION. The steps
# around the two reads that build an environment, which need uv and
# the package index, are taken as built. The honest form is the same copy with both
# predicates as committed. A field of the wrong type is no row of the one parse: a row the
# case appends to the fixture's claims.jsonl fails --replay's claim-form alone, naming the
# line. A row env.sh reads that is gone, or whose field differs from its predicate, is
# case 95.
MODEL_BYTES='the weights of the snapshot case 91 plants'
MODEL_SHA="$(printf '%s\n' "$MODEL_BYTES" | sha256sum | cut -d' ' -f1)"
# env_copy <name> <committed|matches>; a copy of claims.jsonl whose s-model-checksum pins
# the snapshot this case plants, its two rows carrying their predicates as committed, or
# recorded again with {"matches": ...}.
env_copy() {
    local copy="$WORK/$1.jsonl"
    jq -c --arg sha "$MODEL_SHA" --arg form "$2" '
        if .id == "s-model-checksum" then .evidence.sha256 = $sha
            | .expect = (if $form == "matches" then {matches: ("^" + $sha + "  ")} else {contains: $sha} end)
        elif .id == "tc-corpus-1" and $form == "matches" then .expect = {matches: "^[0-9]+$"}
        else . end' "$HERE/claims.jsonl" >"$copy"
    printf '%s' "$copy"
}
# env_reads <name> <claims copy>; runs env.sh's ioc_env_model and ioc_env_reviews over the
# copy, as --replay's env.sh step does, in a shell of their own with a cache of the case's
# own; ends 0 when both prepared what the rows pin, and prints env.sh's refusals.
env_reads() {
    local cache="$WORK/$1-cache" snapshot
    snapshot="$cache/cache/hf/hub/models--sentence-transformers--all-MiniLM-L6-v2/snapshots/1110a243fdf4706b3f48f1d95db1a4f5529b4d41"
    mkdir -p "$snapshot" "$cache/bin" "$cache/tc-work"
    printf '%s\n' "$MODEL_BYTES" >"$snapshot/model.safetensors"
    printf '1110a243fdf4706b3f48f1d95db1a4f5529b4d41 %s\n' "$MODEL_SHA" >"$cache/model.built-from"
    seq 10 >"$cache/tc-work/reviews.jsonl"
    printf '#!/bin/sh\njq -n '"'"'[range(30) | {id: ., state: "COMMENTED", commit_id: "c", user: {login: "u"}, submitted_at: "2026-11-01T00:00:00Z", body: "b"}]'"'"'\n' >"$cache/bin/curl"
    chmod +x "$cache/bin/curl"
    : >"$cache/legion.header"
    (
        IOC_ENV_FUNCTIONS_ONLY=1
        # shellcheck source=env.sh
        . "$HERE/env.sh" || exit 1
        ioc_env_built() { return 0; }
        ioc_env_check_frozen() { return 0; }
        ioc_env_stamp_write() { return 0; }
        export IOC_CACHE="$cache" IOC_EVID="$HERE/evidence" IOC_ENV_HERE="$HERE" IOC_ENV_CLAIMS="$2" IOC_PY=/nonexistent-ioc/python IOC_GITHUB_AUTH_LEGION="$cache/legion.header" PATH="$cache/bin:$PATH"
        ioc_env_caches
        ioc_env_model && ioc_env_reviews || exit 1
        [ "$(wc -l <"$IOC_REVIEWS")" -ge "$(ioc_env_row_field tc-corpus-1 corpus_floor)" ] || {
            printf 'env.sh kept a corpus of %s review bodies, fewer than the %s of tc-corpus-1\n' "$(wc -l <"$IOC_REVIEWS")" "$(ioc_env_row_field tc-corpus-1 corpus_floor)"
            exit 1
        }
    ) 2>&1
}
for form in matches committed; do
    name="env.sh prepares the snapshot and the corpus its rows pin, with both rows' predicates as committed"
    [ "$form" = matches ] && name="env.sh prepares the snapshot and the corpus its rows pin, with both rows recorded again with {\"matches\": ...}"
    if why="$(env_reads "env-rows-$form" "$(env_copy "env-rows-$form" "$form")")"; then
        case_passed "$name"
    else
        case_failed "$name" "$(printf '%s\n' "$why" | tail -1)"
    fi
done
for form in 'sha256:"sha256:53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db"' 'corpus_floor:"5000"'; do
    field="${form%%:*}"
    dir="$(fresh "typed-$field")"
    claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    jq -cn --arg field "$field" --argjson value "${form#*:}" \
        '{id: ("typed-" + $field), requirement: "r7", source: "the case", kind: "behaviour", claim: "An evidence field of the wrong type.", mark: "verified", evidence: {surface: "cli", ($field): $value}, note: "planted"}' >>"$claims_copy"
    one_failure_result "--replay over a claims.jsonl whose evidence holds $field as ${form#*:} fails claim-form alone, naming the line" claim-form \
        '^1 of 18 assertions failed: claim-form$' "^FAIL claim-form: line $(wc -l <"$claims_copy") of claims\\.jsonl is no row, so nothing of it is read: its evidence's $field is " \
        "$(fixture_run "$dir" --replay)"
done

# 92. An id names one row (the architect's note of 2026-11-16T06:35Z). At beda0a6c32 the one
# parse read a second row of an id it had read as a row of its own, so a row recorded twice
# ran twice and each passed; claims.jsonl held none at 134143b4fb, 731 rows and 731 ids. The second is
# now no row, one FAIL naming both lines, in --replay's claim-form and --admitted's
# admitted-rows, exit 1. The honest form is each mode over the file as it stands, which
# cases 88 and 86 run.
dir="$(fresh duplicate-id)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
row="$(grep -F '"id":"f-behaviour-1"' "$claims_copy")"
printf '%s\n' "$row" >>"$claims_copy"
one_failure_result "--replay over a claims.jsonl holding a second row of one id fails claim-form alone, naming both lines, exit 1" claim-form \
    '^1 of 18 assertions failed: claim-form$' "^FAIL claim-form: line $(wc -l <"$claims_copy") of claims\\.jsonl is no row, so nothing of it is read: its id \"f-behaviour-1\" is the id of line 3 too" \
    "$(fixture_run "$dir" --replay)"
copy="$(admitted_copy duplicate-id-admitted)"
row="$(head -1 "$copy/claims.jsonl")"
printf '%s\n' "$row" >>"$copy/claims.jsonl"
one_failure_result "--admitted over a claims.jsonl holding a second row of one id fails admitted-rows alone, naming both lines, exit 1" admitted-rows \
    '^1 of 4 assertions failed: admitted-rows$' "^FAIL admitted-rows: line $(wc -l <"$copy/claims.jsonl") of claims\\.jsonl is no row, so nothing of it is read: its id \"[^\"]*\" is the id of line 1 too" \
    "$(IOC_EVIDENCE="$copy" gate_run "$HERE/check.sh" --admitted)"

# 93. A row reads a fact fixed in time, never the summary of a node as it stands now (test
# round 29, the architect's note of 2026-11-16T12:58Z). At 134143b4fb n-read-1098 held the
# whole output of `dispatch doc-read --version 97` to its checksum and counted its line
# `Approval: approved v97 by person@example.invalid`, written here with this case's
# placeholder for the approver's address. After the version's content and its Content:
# line the read prints the document as it stands now, its Approval: line and its open
# anchored threads, so when an approval of version 99 was requested the checksum moved and
# the row failed, while the approval of version 97 stood. The row now reads the approval
# event by its id and the first Content: line of the pinned read. The case runs the row as
# claims.jsonl holds it, its command and its predicate, over a fixture whose Dispatch is a
# stub of the case's own first on PATH, a dispatch and a curl that answer from files and
# reach no server: the approval event and the version's Content: line as measured, beside a
# present state of the document that has moved on from the one the row was recorded
# against, its approval awaiting a request for version 99 and an anchored thread whose reply
# quotes another Content: line. The row passes there. The old form, the checksum of the
# whole pinned read, and the count of its Approval: line, each recorded against the stub as
# the document stood when the row was first recorded, pass there and fail once the present
# state moves.
PINNED="$WORK/pinned-read"
PINNED_SHA=f6c88506ba193386bf9b644aeab9b8b0e2d69f7935f17b2fade89fcac3838194
mkdir -p "$PINNED/recorded/bin" "$PINNED/moved/bin"
printf 'case-93-token\n' >"$PINNED/token"
printf '%s\n' '[{"id":405554,"issue_key":"INFRA-1098","project":"INFRA","seq":424,"type":"artifact.approved","actor":{"kind":"user","id":"person@example.invalid"},"created_at":"2026-11-12T00:45:28.848044Z","payload":{"kind":"person","name":"spec.md","version":97,"artifact_id":"89c8e344-c6ad-4fa0-89c5-d252856dc817","ask_id":"b5d11b18-84de-494f-af2f-9669f338882e","content_sha256":"'"$PINNED_SHA"'"}}]' >"$PINNED/event.json"
PINNED_CONTENT='## Summary

Version 97 of the spec this case stands in for.
'
printf '%s\n\nContent: sha256:%s\nApproval: approved v97 by person@example.invalid\n' "$PINNED_CONTENT" "$PINNED_SHA" >"$PINNED/recorded/doc-read.txt"
{
    printf '%s\n\nContent: sha256:%s\n' "$PINNED_CONTENT" "$PINNED_SHA"
    printf 'Approval: awaiting, waiting on human (requested by 01a1bb6a-b605-779a-879f-16af577b2512, ask 2ae7547f-3262-4f02-998a-e950aea19501)\n\nOpen anchored threads:\n\n'
    printf 'comment 93930000-0000-4000-8000-000000000093 · user person@example.invalid · 2026-11-16T12:00:00Z\n> Version 97 of the spec this case stands in for.\nBody: Which version did I approve?\n'
    printf 'reply 93930000-0000-4000-8000-000000000094 · session 01a1bb6a-b605-779a-879f-16af577b2512 · 2026-11-16T12:01:00Z\nBody: Version 97, whose read prints\nContent: sha256:%s\n' "$(printf '0%.0s' $(seq 64))"
} >"$PINNED/moved/doc-read.txt"
for state in recorded moved; do
    cat >"$PINNED/$state/bin/dispatch" <<STUB
#!/bin/sh
[ "\$*" = 'doc-read --issue INFRA-1098 --artifact spec --version 97' ] && exec cat '$PINNED/$state/doc-read.txt'
printf 'the stub dispatch of case 93 answers no %s\n' "\$*" >&2
exit 2
STUB
    cat >"$PINNED/$state/bin/curl" <<STUB
#!/bin/sh
for url; do :; done
case "\$url" in
*'/api/v1/issues/INFRA-1098/events?ids=405554') exec cat '$PINNED/event.json' ;;
esac
printf 'curl: (7) the stub curl of case 93 answers no %s\n' "\$url" >&2
exit 7
STUB
    chmod +x "$PINNED/$state/bin/dispatch" "$PINNED/$state/bin/curl"
done
# pinned_run <name> <recorded|moved> <command> <predicate>; plants the form into a fresh
# fixture and sets PINNED_OUT to its --replay over that state of the stub Dispatch.
pinned_run() {
    local dir
    dir="$(fresh "pinned-$1-$2")"
    plant_behaviour "$dir" "$3" "$4"
    PINNED_OUT="$(PATH="$PINNED/$2/bin:$PATH" DISPATCH_URL=http://dispatch.case-93.invalid DISPATCH_TOKEN_FILE="$PINNED/token" fixture_run "$dir" --replay)"
}
PINNED_MISSED="replay-command: f-behaviour-1 ran '.*' and its output does not meet"
pinned_run row moved "$(jq -r 'select(.id == "n-read-1098") | .command' "$HERE/claims.jsonl")" "$(jq -c 'select(.id == "n-read-1098") | .expect' "$HERE/claims.jsonl")"
honest_result "n-read-1098 as claims.jsonl holds it passes over a Dispatch whose document moved on while version 97 and its approval stand" replay-command "$PINNED_OUT"
# pinned_old <name> <what it reads> <command> <predicate>; the old form recorded against the
# stub as the document stood, which passes there, and the same form once it moved on.
pinned_old() {
    pinned_run "$1" recorded "$3" "$4"
    honest_result "the old form of n-read-1098, $2, passes over the Dispatch it was recorded against" replay-command "$PINNED_OUT"
    pinned_run "$1" moved "$3" "$4"
    case_result "the old form of n-read-1098, $2, fails once the document's approval and threads move on" "$PINNED_MISSED" "$PINNED_OUT"
}
pinned_old checksum "the checksum of the whole pinned read" \
    "dispatch doc-read --issue INFRA-1098 --artifact spec --version 97 | sha256sum && dispatch doc-read --issue INFRA-1098 --artifact spec --version 97 | grep -c '^Approval: approved v97 by person@example.invalid'" \
    "{\"contains\":\"$(sha256sum <"$PINNED/recorded/doc-read.txt" | cut -d' ' -f1)\"}"
pinned_old approval "the count of the read's Approval: line" \
    "dispatch doc-read --issue INFRA-1098 --artifact spec --version 97 | grep -c '^Approval: approved v97 by person@example.invalid'" '{"equals":"1"}'

# 94. A predicate holds every part its claim names (the architect's note of
# 2026-11-16T19:54Z). At f63516e5dbe tm-validate claimed that mas-ctl validate prints OK for
# the Agent manifest and for the Workflow manifest and exits 0, and held the exit status
# alone, while its own tm-schema-wheel shows mas-ctl answering OK, exit 0, when it finds no
# schema and so checks nothing. The case runs the row as claims.jsonl holds it, its command
# and its predicate, over a fixture whose mas-ctl is a stub of the case's own, as
# IOC_MASLAB_CTL: one prints both OK lines and lists the agent and workflow schemas, and
# passes; the other prints both OK lines with a warning under each that no schema file was
# found, exits 0 and lists no schema, and fails. The old predicate, the exit status alone,
# passes that second stub.
NOSCHEMA="$WORK/no-schema"
mkdir -p "$NOSCHEMA/found" "$NOSCHEMA/missing"
cat >"$NOSCHEMA/found/mas-ctl" <<'STUB'
#!/bin/sh
case "$1" in
validate) printf 'OK  legion-workflow.yaml  kind=agent\nOK  legion-workflow-kind.yaml  kind=workflow\n' ;;
schemas) printf 'schema root: /case-94/docs/schemas\n  agent\n  mas\n  workflow\n' ;;
*) exit 2 ;;
esac
STUB
cat >"$NOSCHEMA/missing/mas-ctl" <<'STUB'
#!/bin/sh
case "$1" in
validate) printf "OK  legion-workflow.yaml  kind=agent\n  [warning] no schema file for kind 'agent'\nOK  legion-workflow-kind.yaml  kind=workflow\n  [warning] no schema file for kind 'workflow'\n" ;;
schemas) printf 'schema root: /case-94/no-schemas\n' ;;
*) exit 2 ;;
esac
STUB
chmod +x "$NOSCHEMA/found/mas-ctl" "$NOSCHEMA/missing/mas-ctl"
# schema_run <name> <found|missing> <command> <predicate>; plants the form into a fresh
# fixture and sets SCHEMA_OUT to its --replay with that stub as mas-ctl.
schema_run() {
    local dir
    dir="$(fresh "schema-$1-$2")"
    plant_behaviour "$dir" "$3" "$4"
    SCHEMA_OUT="$(IOC_EVID="$dir/repo/docs/research/internet-of-cognition" IOC_MASLAB_CTL="$NOSCHEMA/$2/mas-ctl" fixture_run "$dir" --replay)"
}
SCHEMA_COMMAND="$(jq -r 'select(.id == "tm-validate") | .command' "$HERE/claims.jsonl")"
SCHEMA_PREDICATE="$(jq -c 'select(.id == "tm-validate") | .expect' "$HERE/claims.jsonl")"
schema_run row found "$SCHEMA_COMMAND" "$SCHEMA_PREDICATE"
honest_result "tm-validate as claims.jsonl holds it passes over a mas-ctl that finds both schemas" replay-command "$SCHEMA_OUT"
schema_run row missing "$SCHEMA_COMMAND" "$SCHEMA_PREDICATE"
case_result "tm-validate as claims.jsonl holds it fails over a mas-ctl that answers OK, exit 0, with no schema found" replay-command "$SCHEMA_OUT"
schema_run old missing 'cd "$IOC_EVID" && "$IOC_MASLAB_CTL" validate legion-workflow.yaml legion-workflow-kind.yaml' '{"exit":0}'
honest_result "the old form of tm-validate, the exit status alone, passes a mas-ctl that answers OK with no schema found" replay-command "$SCHEMA_OUT"

# 95. A pin env.sh reads and the value its row records are one fact (the fourteenth
# review, the Minor at check.sh:3366). At f63516e5db a row env.sh reads that was gone only
# failed claim-form, and --replay went on to read GitHub and Dispatch and to source env.sh,
# which cloned and built the python environment before it refused, so the 15 other
# assertions could not run for the environment when the cause was the evidence; and
# nothing held s-model-checksum's sha256 equal to its predicate, or tc-corpus-1's
# corpus_floor to its, so a pin with one digit wrong ended exit 2 at env.sh with no FAIL.
# Each form runs --replay over a copy of this directory's claims.jsonl and env.sh with
# GitHub and Dispatch cut, as case 90 cuts GitHub: s-model-checksum gone, its sha256 one
# digit off the line its predicate records, and tc-corpus-1's corpus_floor one below its
# predicate's count. Each fails claim-form alone, naming the row, and stops there, before
# it reads a live source or builds anything, exit 1: claim-marks-agree, which reads the
# rows alone, is judged before it stops, and each of the other 16 assertions could not run
# for the evidence. The honest form is env_rows_judge over claims.jsonl as it stands,
# which reports nothing.
# evidence_stop_result <name> <the FAIL's detail, an extended regular expression> <run>;
# passes when the record holds exit 1, claim-form alone among the assertions that failed,
# and the 16 others but claim-marks-agree as ones that could not run, of the kind
# evidence, for the reason evidence_stops gives, and the text a FAIL of claim-form with
# that detail. A run that stops there reads no live source and builds nothing, so the only
# reasons from outside the tree that can end it first, a missing prerequisite or its own
# scratch and record (case 96), make the case one that could not run; any other ending
# fails it.
EVIDENCE_STOP='the evidence stops --replay at claim-form: '
evidence_stop_result() {
    local name="$1" detail="$2" run="$3" first
    ending_read "$run"
    first="$(printf '%s' "$RUN_UNRUN" | head -1)"
    if [ "$RUN_CRASH" != 1 ] && { [ "$(printf '%s' "$first" | cut -f2)" = prerequisite ] || [ "$(printf '%s' "$first" | cut -f1)" = --replay ]; }; then
        case_unrun "$name" "$(printf '%s' "$first" | cut -f2)" "$(printf '%s' "$first" | cut -f3)"
    elif [ "$RUN_STATUS" = 1 ] && [ "$RUN_FAILED" = claim-form$'\n' ] && [ "$(printf '%s' "$RUN_UNRUN" | wc -l)" = 16 ] &&
        [ "$(printf '%s' "$RUN_UNRUN" | STOP="$EVIDENCE_STOP" awk -F'\t' '$2 == "evidence" && index($3, ENVIRON["STOP"]) == 1' | wc -l)" = 16 ] &&
        grep -qE -- "^FAIL claim-form: $detail" "$run/text"; then
        case_passed "$name"
    else
        case_failed "$name" "exit $RUN_STATUS, $(grep '^FAIL' "$run/text" | head -2 | tr '\n' ' ')last line: $RUN_LINE"
    fi
}
for form in 'gone|select(.id != "s-model-checksum")|claims\.jsonl holds no row s-model-checksum whose evidence names sha256, which env\.sh reads as ' \
    'sha256|if .id == "s-model-checksum" then .evidence.sha256 |= .[0:63] + (if .[63:] == "0" then "1" else "0" end) else . end|the row s-model-checksum names sha256 [0-9a-f]{64} in its evidence, which env\.sh reads as .*, and its predicate holds no line [0-9a-f]{64}, so the pin and the value the row is held to differ' \
    'corpus_floor|if .id == "tc-corpus-1" then .evidence.corpus_floor -= 1 else . end|the row tc-corpus-1 names corpus_floor [0-9]+ in its evidence, which env\.sh reads as .*, and its predicate holds no line [0-9]+, so the pin and the value the row is held to differ'; do
    planted="${form%%|*}"
    detail="${form##*|}"
    program="${form#*|}"
    program="${program%|*}"
    copy="$(admitted_copy "env-row-$planted")"
    # claim-marks-agree, judged before the stop, reads which records are under check from
    # sources.json, so the copy carries it beside claims.jsonl and env.sh.
    cp "$HERE/sources.json" "$copy/"
    jq -c "$program" "$HERE/claims.jsonl" >"$copy/claims.jsonl"
    cmp -s "$copy/claims.jsonl" "$HERE/claims.jsonl" && {
        printf 'FAIL the row env.sh reads could not be planted %s in a copy of claims.jsonl\n' "$planted"
        exit 1
    }
    out="$(IOC_EVIDENCE="$copy" IOC_DEPLOYMENT_REPO="$FIXTURE_DEPLOYMENT_REPO" DISPATCH_URL=http://127.0.0.1:1 https_proxy=http://127.0.0.1:1 HTTPS_PROXY=http://127.0.0.1:1 no_proxy= NO_PROXY= gate_run "$HERE/check.sh" --replay)"
    evidence_stop_result "--replay whose row env.sh reads is $planted fails claim-form alone, naming the row, and stops before it reads a source or builds anything, exit 1" "$detail" "$out"
done
out="$(bash -c '. "$1" && claims_parsed && env_functions && env_rows_judge claim-form && printf "it reported nothing\n"' _ "$HERE/check.sh" 2>&1)"
if [ "$out" = 'it reported nothing' ]; then
    case_passed "env_rows_judge over claims.jsonl as it stands reports nothing: each row env.sh reads names its field, and the field is a line of its predicate"
else
    case_failed "env_rows_judge over claims.jsonl as it stands reports nothing: each row env.sh reads names its field, and the field is a line of its predicate" "$(printf '%s\n' "$out" | head -2 | tr '\n' ' ')"
fi

# 96. A run's ending needs its own scratch, its record and the tools it is written with,
# and each is checked before anything else runs (the fourteenth review, item 2, and the
# endings audit of round 31). At f63516e5db a TMPDIR that is missing, unwritable or full
# left the run no scratch, so cannot_run could not record its reason and every assertion
# was counted as failed: `TMPDIR=/nonexistent-rv bash check.sh --admitted` ended "4 of 4
# assertions failed", exit 1, its record holding status 1. The same held for a scratch that
# stops taking writes while the run goes on, for a record that cannot be written, which
# ended exit 0 with no record, for a missing tool the ending is written with, for a mode the
# gate does not have, which printed its help and exited 2 with no record, for a run of
# check.test.sh that stopped before its summary with its scratch shut, and for a missing
# prerequisite of check.test.sh, which built no fixture and passed 0 cases. Each form now
# ends exit 2 naming what failed, its record holding the run, or each unit it declared, as
# one that could not run, of the kind environment, prerequisite or gate. A record that
# cannot be written is read by its reader as a run that could not run when the directory
# it goes to takes no write, and as a crash when it does, since then the caller named a
# place it cannot make, and so is a run that was never made. A full TMPDIR is a tmpfs of
# 16 KiB mounted in a user namespace of its own, and the form prints SKIP where the pod
# allows none; an unwritable one prints SKIP for a user who writes it anyway, as root does.
# The honest forms are --admitted with all of them whole, which passes, and a run of
# check.test.sh that stops before its summary with its scratch whole, which wrote no record
# and reads as a crash.
# unrun_result <name> <kind> <count> <last line, an extended regular expression> <run>; a
# run the case stops for a reason it plants, from outside the tree: it passes when the
# run's record holds exit 2 and that last line, no failure, and <count> units that could
# not run, each of <kind> and for that last line. The reason is the one the case plants,
# so case_read's rule for a reason from outside the tree is not read here.
unrun_result() {
    local name="$1" kind="$2" count="$3" last="$4" run="$5"
    ending_read "$run"
    if [ "$RUN_CRASH" != 1 ] && [ "$RUN_STATUS" = 2 ] && [ -z "$RUN_FAILED" ] && printf '%s\n' "$RUN_LINE" | grep -qE -- "$last" &&
        [ "$(printf '%s' "$RUN_UNRUN" | wc -l)" = "$count" ] &&
        [ "$(printf '%s' "$RUN_UNRUN" | KIND="$kind" LINE="$RUN_LINE" awk -F'\t' '$2 == ENVIRON["KIND"] && $3 == ENVIRON["LINE"]' | wc -l)" = "$count" ]; then
        case_passed "$name"
    else
        case_failed "$name" "exit $RUN_STATUS, last line: $RUN_LINE; could not run: $(printf '%s' "$RUN_UNRUN" | cut -f1,2 | tr '\t\n' ': ')"
    fi
}
out="$(TMPDIR=/nonexistent-rv gate_run "$HERE/check.sh" --admitted)"
unrun_result "TMPDIR=/nonexistent-rv bash check.sh --admitted cannot run, exit 2, naming the scratch, and its record holds the run as one that could not run for the environment" environment 1 \
    '^check\.sh cannot run --admitted: its own scratch cannot be made under TMPDIR, /nonexistent-rv: No such file or directory$' "$out"
mkdir -p "$WORK/tmp-unwritable"
chmod a-w "$WORK/tmp-unwritable"
if (: >"$WORK/tmp-unwritable/probe") 2>/dev/null; then
    printf 'SKIP the run whose TMPDIR cannot be written: this user writes a directory whose mode forbids it, as root does\n'
else
    out="$(TMPDIR="$WORK/tmp-unwritable" gate_run "$HERE/check.sh" --admitted)"
    unrun_result "--admitted whose TMPDIR cannot be written cannot run, exit 2, naming the scratch" environment 1 \
        "^check\\.sh cannot run --admitted: its own scratch cannot be made under TMPDIR, .*/tmp-unwritable: Permission denied\$" "$out"
fi
chmod u+w "$WORK/tmp-unwritable"
mkdir -p "$WORK/tmp-full"
if command -v unshare >/dev/null 2>&1 && unshare -rm sh -c 'mount -t tmpfs -o size=16k tmpfs "$1"' _ "$WORK/tmp-full" 2>/dev/null; then
    cat >"$WORK/in-full-tmp.sh" <<'SH'
# in-full-tmp.sh <mode>; runs the gate IOC_CASE_GATE names in that mode with TMPDIR on a
# tmpfs of 16 KiB, IOC_CASE_MOUNT, that is full, in a user namespace of its own.
exec unshare -rm sh -c 'mount -t tmpfs -o size=16k tmpfs "$1" && { head -c 40000 /dev/zero >"$1/fill" 2>/dev/null; TMPDIR="$1" exec bash "$2" "$3"; }' _ "$IOC_CASE_MOUNT" "$IOC_CASE_GATE" "$1"
SH
    out="$(IOC_CASE_MOUNT="$WORK/tmp-full" IOC_CASE_GATE="$HERE/check.sh" gate_run "$WORK/in-full-tmp.sh" --admitted)"
    unrun_result "--admitted whose TMPDIR is full cannot run, exit 2, naming the scratch" environment 1 \
        "^check\\.sh cannot run --admitted: its own scratch .*/tmp-full/ioc-run\\.[^ ]*, made under TMPDIR, takes no write: No space left on device\$" "$out"
else
    printf 'SKIP the run whose TMPDIR is full: this pod mounts no tmpfs in a user namespace of its own\n'
fi
cat >"$WORK/record-elsewhere.sh" <<'SH'
# record-elsewhere.sh <mode>; runs the gate IOC_CASE_GATE names in that mode with its
# record at IOC_CASE_RECORD, in place of the one gate_run names.
IOC_ENDING="$IOC_CASE_RECORD" exec bash "$IOC_CASE_GATE" "$1"
SH
out="$(IOC_CASE_RECORD="$WORK/no-such-directory/ending" IOC_CASE_GATE="$HERE/check.sh" gate_run "$WORK/record-elsewhere.sh" --admitted)"
ending_read "$out"
if [ "$RUN_CRASH" = 1 ] && [ "$(cat "$out/status")" = 2 ] && [ ! -e "$WORK/no-such-directory" ] &&
    tail -1 "$out/text" | grep -qE '^check\.sh cannot run --admitted: its ending record, .*/no-such-directory/ending, which IOC_ENDING names, cannot be written: No such file or directory$'; then
    case_passed "--admitted whose record IOC_ENDING names cannot be written ends exit 2 naming it, and its reader, whose directory takes writes, reads a crash"
else
    case_failed "--admitted whose record IOC_ENDING names cannot be written ends exit 2 naming it, and its reader, whose directory takes writes, reads a crash" "exit $(cat "$out/status"), it reads as $RUN_ENDING, last line: $(tail -1 "$out/text")"
fi
path_without awk
out="$(PATH="$WORK/path-without-awk" gate_run "$HERE/check.sh" --admitted)"
unrun_result "--admitted with no awk, a tool its ending is written with, cannot run, exit 2, naming it and what provides it" prerequisite 1 \
    '^check\.sh cannot run --admitted: the tools its ending record is written and read with are not on PATH: awk; provided by: apt-get install coreutils grep gawk$' "$out"
plant_gate scratch-shut 's/^mode_admitted() {$/&\n    chmod a-w "$RUN_SCRATCH" # planted by check.test.sh/' 'chmod a-w "$RUN_SCRATCH" # planted by check.test.sh' holds
mkdir -p "$WORK/tmp-shut"
out="$(TMPDIR="$WORK/tmp-shut" gate_run "$GATE" --admitted)"
unrun_result "--admitted whose scratch stops taking writes while it runs could not run, exit 2, naming the scratch, whatever it judged" environment 4 \
    '^check\.sh cannot run --admitted: its own scratch .*/tmp-shut/ioc-run\.[^ ]* takes no write as it ends: Permission denied, so nothing it judged is read' "$out"
chmod -R u+w "$WORK/tmp-shut"
plant_gate record-shut 's/^mode_admitted() {$/&\n    chmod a-w "${ENDING%\/*}" # planted by check.test.sh/' 'chmod a-w "${ENDING%/*}" # planted by check.test.sh' holds
# gate_run's own write of the run's status fails there too, as the case plants.
out="$(gate_run "$GATE" --admitted 2>/dev/null)"
unrun_result "--admitted whose record's directory stops taking writes is read as a run that could not run, naming the directory" environment 1 \
    '^check\.sh cannot run: the directory of the run, .*, takes no write: Permission denied, so the run could write no ending record there$' "$out"
chmod u+w "$out"
if tail -1 "$out/text" | grep -qE '^check\.sh cannot run --admitted: its ending record, .*, cannot be written: Permission denied, so no reader reads this run, which ended: all 4 assertions passed$'; then
    case_passed "--admitted whose record cannot be written as it ends ends naming the record, then the line the record would have held"
else
    case_failed "--admitted whose record cannot be written as it ends ends naming the record, then the line the record would have held" "last line: $(tail -1 "$out/text")"
fi
# read_unmade <scratch>; how ending_read reads a run that was never made, its directory
# empty, by a reader whose scratch is <scratch>: the ending, the crash and the kinds.
read_unmade() { (RUN_SCRATCH="$1" && ending_read "" && printf '%s|%s|%s' "$RUN_ENDING" "$RUN_CRASH" "$(printf '%s' "$RUN_UNRUN" | cut -f2)"); }
if [ "$(read_unmade "$WORK/no-such-scratch")" = 'cannot-run|0|environment' ]; then
    case_passed "a run never made, read by a reader whose scratch takes no write, could not run, of the kind environment"
else
    case_failed "a run never made, read by a reader whose scratch takes no write, could not run, of the kind environment" "it reads as $(read_unmade "$WORK/no-such-scratch")"
fi
if [ "$(read_unmade "$RUN_SCRATCH")" = 'failed|1|' ]; then
    case_passed "a run never made, read by a reader whose scratch takes writes, is a crash"
else
    case_failed "a run never made, read by a reader whose scratch takes writes, is a crash" "it reads as $(read_unmade "$RUN_SCRATCH")"
fi
out="$(gate_run "$HERE/check.sh" --no-such-mode)"
unrun_result "check.sh in a mode it does not have cannot run, exit 2, naming the mode, of the kind gate" gate 1 \
    '^check\.sh cannot run --no-such-mode: it has no mode --no-such-mode; its modes are the ones the header above names$' "$out"
STOP_CASE="$(cat <<'CASE'
printf 'FAIL a plant of this case could not be made\n'
exit 1
CASE
)"
copy="$(cases_copy stop-scratch-shut "chmod a-w \"\$RUN_SCRATCH\"
$STOP_CASE")"
mkdir -p "$WORK/tmp-stop"
out="$(TMPDIR="$WORK/tmp-stop" gate_run "$copy/check.test.sh")"
unrun_result "a check.test.sh that stops before its summary while its scratch takes no write could not run, exit 2, naming the scratch" environment 1 \
    '^check\.sh cannot run check\.test\.sh: its own scratch .*/tmp-stop/ioc-run\.[^ ]* takes no write as it stops before its summary: Permission denied, so the case it stopped at stopped for that$' "$out"
chmod -R u+w "$WORK/tmp-stop"
copy="$(cases_copy stop-scratch-whole "$STOP_CASE")"
out="$(gate_run "$copy/check.test.sh")"
ending_read "$out"
if [ "$RUN_CRASH" = 1 ] && [ "$RUN_STATUS" = 1 ]; then
    case_passed "a check.test.sh that stops before its summary with its scratch whole wrote no record and reads as a crash"
else
    case_failed "a check.test.sh that stops before its summary with its scratch whole wrote no record and reads as a crash" "it reads as $RUN_ENDING, exit $RUN_STATUS: $RUN_LINE"
fi
path_without jq
copy="$(cases_copy cases-without-jq '')"
out="$(PATH="$WORK/path-without-jq" gate_run "$copy/check.test.sh")"
unrun_result "check.test.sh with no jq on PATH cannot run before its first case, naming jq and what provides it" prerequisite 1 \
    '^check\.sh cannot run check\.test\.sh: the tool jq is not on PATH; provided by: apt-get install jq$' "$out"
mkdir -p "$WORK/tmp-whole"
out="$(TMPDIR="$WORK/tmp-whole" gate_run "$HERE/check.sh" --admitted)"
ending_result "--admitted with its scratch, its record and every tool whole passes" 0 '^all 4 assertions passed$' "$out"

# 97. A case reads whether an assertion failed from the run's record, never from its text
# (the fourteenth review, the Minor at check.test.sh:102). At f63516e5db case_result and
# honest_result searched the run's text for a FAIL line of the family, so a line the gate
# printed that no assertion reported was read as that assertion failing: honest_result
# failed a run whose every assertion passed, and case_result passed a run that failed
# another family. The fixture's gate is planted to print a FAIL line of
# absence-still-empty that no assertion reports. Over it as built, every assertion passes,
# and honest_result passes; over it with the planted fault of predicate-form, the run fails
# predicate-form alone, and case_result, asked for absence-still-empty, fails, which the
# case reads from case_result's own line, made in a subshell so that it is no case of this
# file.
PRINTED_FAIL="    printf 'FAIL absence-still-empty: a line the gate printed and no assertion reported, planted by check.test.sh\\\\n'"
printed_fixture() {
    # printed_fixture <name>; a fresh fixture whose gate prints that line as --replay starts.
    local dir
    dir="$(fresh "$1")"
    sed -i "s/^mode_replay() {\$/&\n$PRINTED_FAIL/" "$dir/repo/docs/research/internet-of-cognition/check.sh"
    grep -qF 'a line the gate printed and no assertion reported, planted by check.test.sh' "$dir/repo/docs/research/internet-of-cognition/check.sh" || {
        printf 'FAIL the printed FAIL line could not be planted in the fixture copy of check.sh\n'
        exit 1
    }
    printf '%s' "$dir"
}
out="$(fixture_run "$(printed_fixture printed-fail-passes)" --replay)"
honest_result "a run that printed a FAIL line of absence-still-empty no assertion reported, and passed, passes honest_result" absence-still-empty "$out"
dir="$(printed_fixture printed-fail-other)"
fixture_plant "$dir" predicate-form
out="$(fixture_run "$dir" --replay)"
verdict="$(case_result "probe" absence-still-empty "$out")"
if [ "${verdict%%:*}" = 'FAIL probe' ]; then
    case_passed "case_result asked for absence-still-empty fails a run whose record names only predicate-form, though its text prints a FAIL line of absence-still-empty"
else
    case_failed "case_result asked for absence-still-empty fails a run whose record names only predicate-form, though its text prints a FAIL line of absence-still-empty" "case_result printed: $verdict"
fi

# 98. Dispatch answering that it holds no such node is an answer, in every wording its CLI
# prints. The CLI of the worker image the issue pod moved to on 2026-11-17 prints the
# refusal's code before its text, "NOT_FOUND: not found", where it printed "not found"
# alone. At a829d6d5e8, LIVE_ANSWER_NO read that wording as no answer, so the planted fault
# of citation-dispatch, a reference to an issue Dispatch does not hold, ended --self-test
# as a run that could not run instead of failing citation-dispatch. live_read returns 1 on
# each wording of a no, with the code or without it, and ends the mode as could-not-run on
# a Dispatch that did not answer or that refused for another reason. Each read runs in a
# subshell over a scratch of its own, so cannot_run writes nothing into this run's scratch.
live_answer() {
    # live_answer <line>; how live_read ends over a source that prints <line> and exits 1:
    # 0 answered, 1 answered no, 2 could not run.
    local scratch
    scratch="$(mktemp -d "$WORK/live.XXXXXX")" || return 3
    (
        RUN_SCRATCH="$scratch"
        MODE=citations
        live_read "Dispatch, a stub" "$scratch/out" sh -c 'printf "%s\n" "$1"; exit 1' sh "$1"
    ) >/dev/null 2>&1
}
for answer in 'not found' 'NOT_FOUND: not found' 'No block nosuchblock in dispatch://INFRA-3134/artifact/plan-md.' 'document "no-such-document" not found by slug; this issue'"'"'s 11 documents and files include: plan-md (plan.md)'; do
    live_answer "$answer"
    status=$?
    if [ "$status" = 1 ]; then
        case_passed "live_read reads Dispatch's '$answer' as an answer that it holds no such node"
    else
        case_failed "live_read reads Dispatch's '$answer' as an answer that it holds no such node" "live_read ended $status"
    fi
done
for answer in 'Dispatch at http://127.0.0.1:1 is unreachable' 'UNAUTHORIZED: the token is not valid' 'NOT_FOUND: no route POST /api/v1/x'; do
    live_answer "$answer"
    status=$?
    if [ "$status" = 2 ]; then
        case_passed "live_read reads Dispatch's '$answer' as no answer, so the mode cannot run"
    else
        case_failed "live_read reads Dispatch's '$answer' as no answer, so the mode cannot run" "live_read ended $status"
    fi
done

# 99. A row that compares the two backends reads the model's verdicts from a run in which
# the model loaded (test round 31). At 9a8d707031 s-backends-disagree claims that the two
# backends return different verdict words for p4, p5 and p6, and its command runs
# similarity.py without ACCORD_MODEL_DIR, so the model backend never loads and prints
# model=n/a model_verdict=n/a for every pair. Its predicate, widened to the whole of what
# the command prints, holds 'p1 p2 p3 p4 p5 p6 p7 p8 p9 ', the output of that run, which the
# claim contradicts; with the model loaded the command prints 'p4 p5 p6 ' and the row fails.
# s-sum-model's note records the same lost assignment, fixed there. The case runs the row as
# claims.jsonl holds it, its command and its predicate, over a fixture whose $IOC_PY is a
# stub of the case's own that prints similarity.py's pair lines as measured on 2026-11-17 in
# this tree's replay environment: one with the model loaded, over which the row must pass,
# and one with no model, over which it must fail. The old form, at f63516e5db, contains
# 'p4 p5 p6', passes the stub with no model.
BACKENDS="$WORK/backends"
mkdir -p "$BACKENDS/loaded" "$BACKENDS/unloaded"
cat >"$BACKENDS/loaded/python" <<'STUB'
#!/bin/sh
cat <<'OUT'
p1 truth=differs by 100x difflib=0.923077 difflib_verdict=agreed model=0.794183 model_verdict=agreed from=named in the task
p2 truth=differs by 1000x difflib=0.897959 difflib_verdict=agreed model=0.936863 model_verdict=agreed from=named in the task
p3 truth=inverted difflib=0.901961 difflib_verdict=agreed model=0.944179 model_verdict=agreed from=named in the task
p4 truth=different acts difflib=0.579710 difflib_verdict=clarify model=0.757076 model_verdict=agreed from=named in the task
p5 truth=the same difflib=0.466667 difflib_verdict=flagged model=0.950298 model_verdict=agreed from=named in the task
p6 truth=different events difflib=0.458333 difflib_verdict=flagged model=0.660460 model_verdict=clarify from=ours: skills/dispatch/SKILL.md:45 against skills/legion-controller/SKILL.md:50-51
p7 truth=inverted difflib=0.781609 difflib_verdict=agreed model=0.906882 model_verdict=agreed from=ours: skills/envoy/SKILL.md:49
p8 truth=inverted difflib=0.785714 difflib_verdict=agreed model=0.852597 model_verdict=agreed from=ours: skills/legion-architect/SKILL.md:45
p9 truth=inverted difflib=0.411765 difflib_verdict=flagged model=0.480545 model_verdict=flagged from=ours: skills/envoy/SKILL.md:35
p5 is the same pair: difflib verdict flagged, model verdict agreed
OUT
STUB
cat >"$BACKENDS/unloaded/python" <<'STUB'
#!/bin/sh
cat <<'OUT'
p1 truth=differs by 100x difflib=0.923077 difflib_verdict=agreed model=n/a model_verdict=n/a from=named in the task
p2 truth=differs by 1000x difflib=0.897959 difflib_verdict=agreed model=n/a model_verdict=n/a from=named in the task
p3 truth=inverted difflib=0.901961 difflib_verdict=agreed model=n/a model_verdict=n/a from=named in the task
p4 truth=different acts difflib=0.579710 difflib_verdict=clarify model=n/a model_verdict=n/a from=named in the task
p5 truth=the same difflib=0.466667 difflib_verdict=flagged model=n/a model_verdict=n/a from=named in the task
p6 truth=different events difflib=0.458333 difflib_verdict=flagged model=n/a model_verdict=n/a from=ours: skills/dispatch/SKILL.md:45 against skills/legion-controller/SKILL.md:50-51
p7 truth=inverted difflib=0.781609 difflib_verdict=agreed model=n/a model_verdict=n/a from=ours: skills/envoy/SKILL.md:49
p8 truth=inverted difflib=0.785714 difflib_verdict=agreed model=n/a model_verdict=n/a from=ours: skills/legion-architect/SKILL.md:45
p9 truth=inverted difflib=0.411765 difflib_verdict=flagged model=n/a model_verdict=n/a from=ours: skills/envoy/SKILL.md:35
p5 is the same pair: difflib verdict flagged
OUT
STUB
chmod +x "$BACKENDS/loaded/python" "$BACKENDS/unloaded/python"
# backends_run <name> <loaded|unloaded> <command> <predicate>; plants the form into a fresh
# fixture, with the similarity directory its command enters, and sets BACKENDS_OUT to its
# --replay with that stub as $IOC_PY.
backends_run() {
    local dir
    dir="$(fresh "backends-$1-$2")"
    mkdir -p "$dir/repo/docs/research/internet-of-cognition/similarity"
    plant_behaviour "$dir" "$3" "$4"
    BACKENDS_OUT="$(IOC_PY="$BACKENDS/$2/python" fixture_run "$dir" --replay)"
}
BACKENDS_COMMAND="$(jq -r 'select(.id == "s-backends-disagree") | .command' "$HERE/claims.jsonl")"
BACKENDS_PREDICATE="$(jq -c 'select(.id == "s-backends-disagree") | .expect' "$HERE/claims.jsonl")"
backends_run row loaded "$BACKENDS_COMMAND" "$BACKENDS_PREDICATE"
honest_result "s-backends-disagree as claims.jsonl holds it passes over similarity.py's output with the model loaded" replay-command "$BACKENDS_OUT"
backends_run row unloaded "$BACKENDS_COMMAND" "$BACKENDS_PREDICATE"
case_result "s-backends-disagree as claims.jsonl holds it fails over similarity.py's output with no model loaded" replay-command "$BACKENDS_OUT"
backends_run old unloaded "$BACKENDS_COMMAND" '{"contains":"p4 p5 p6"}'
honest_result "the old form of s-backends-disagree, contains p4 p5 p6, passes similarity.py's output with no model loaded" replay-command "$BACKENDS_OUT"

# 100. A row's verdict does not turn on what the reader's own python holds (test round 31).
# At 9a8d707031 m-rt-no-execution claims that their runtime could not be run in the pod of
# the reading, because pydantic was absent and its install failed, and its command imports
# pydantic in the reader's own python3. That reads the machine that replays the row, not the
# pod the claim is about. A reader whose python3 finds pydantic, as one installed under
# ~/.local does, fails the row though nothing the reading recorded changed; and the replay's
# own IOC_MASLAB_VENV installs pydantic 2.14.3 from tm-constraints.txt and runs mas-lab's
# code through it, so the import no longer tests what the claim is about. The case runs the
# row as claims.jsonl holds it, its command and its predicate, over the fixture twice: with
# HOME at a directory whose ~/.local holds a module named pydantic, over which the row must
# pass, and with HOME at an empty directory.
PYDANTIC_HOME="$WORK/pydantic-home"
PYDANTIC_SITE="$PYDANTIC_HOME/.local/lib/python$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])')/site-packages/pydantic"
mkdir -p "$PYDANTIC_SITE" "$WORK/empty-home"
printf 'VERSION = "2.14.3"\n' >"$PYDANTIC_SITE/__init__.py"
(HOME="$PYDANTIC_HOME" python3 -c 'import pydantic') 2>/dev/null || {
    printf 'FAIL the case could not give python3 a pydantic under ~/.local\n'
    exit 1
}
pydantic_run() {
    local dir
    dir="$(fresh "pydantic-$1")"
    plant_behaviour "$dir" "$(jq -r 'select(.id == "m-rt-no-execution") | .command' "$HERE/claims.jsonl")" "$(jq -c 'select(.id == "m-rt-no-execution") | .expect' "$HERE/claims.jsonl")"
    PYDANTIC_OUT="$(HOME="$2" fixture_run "$dir" --replay)"
}
pydantic_run reader-has-it "$PYDANTIC_HOME"
honest_result "m-rt-no-execution as claims.jsonl holds it passes for a reader whose python3 finds pydantic under ~/.local" replay-command "$PYDANTIC_OUT"
pydantic_run reader-lacks-it "$WORK/empty-home"
honest_result "m-rt-no-execution as claims.jsonl holds it passes for a reader whose python3 finds no pydantic" replay-command "$PYDANTIC_OUT"

# 101. The replay's uv and pip read no setting of the caller's, and no row reads an optional
# input of mas-lab's programs the caller set (the tester's note of test round 31, and the
# side-by-side audit of round 32). At f7a6cb36ba a caller's shell that exported
# UV_EXTRA_INDEX_URL, as a torch user's shell often does, reached every install the replay
# makes: uv looked for a package at that index first and failed, and the replay failed as
# though the evidence were wrong. The audit of round 32 found five optional inputs that
# mas-lab's programs and tm-load.py read and no row sets, MAS_MANIFEST_VALIDATE,
# MAS_MANIFEST_RESOLVE_REFS, MAS_MANIFEST_STRICT, OPENAI_API_KEY and MAS_LLM_API_KEY, of
# which MAS_MANIFEST_VALIDATE=0 turns mas-ctl validate into a skip that exits 0; and env.sh
# unset UV_NO_VERIFY_HASHES without check.sh reserving it. env.sh now sets or clears each of
# these itself (IOC_ENV_ENFORCED): it unsets each of the eighteen variables of
# CALLER_SETTINGS, twelve that point uv or pip at another source of packages, change which
# index a package is taken from, keep them from their index or name a configuration file,
# UV_NO_VERIFY_HASHES, which keeps uv from checking a hash, and the five inputs, and it
# exports UV_NO_CONFIG=1 and PIP_CONFIG_FILE=/dev/null; check.sh reserves each of those
# names. The case runs env.sh's ioc_env_main, as --replay's env.sh step runs it, offline,
# from a shell that exports each of the eighteen, the twelve at an address that does not
# exist or at a configuration file of the case's own, UV_NO_VERIFY_HASHES=1,
# MAS_MANIFEST_VALIDATE=0, MAS_MANIFEST_RESOLVE_REFS=0, MAS_MANIFEST_STRICT=1 and the two
# keys at a value of the case's own, beside UV_NO_CONFIG=0 and a PIP_CONFIG_FILE of its own,
# with every other step of ioc_env_main, each of which clones, reads a source or builds,
# taken as done. The environment it leaves, which every row inherits, holds none of the
# eighteen, and holds UV_NO_CONFIG=1 and PIP_CONFIG_FILE=/dev/null. And a row that assigns
# UV_EXTRA_INDEX_URL before an install is refused by name, as a name the gate records with.
# At f7a6cb36ba the environment held every one of the eighteen but UV_NO_VERIFY_HASHES, and
# the row was refused only as an assignment to a name the evaluator does not admit; at
# 81bed05e14 the environment still held the five inputs.
CALLER_SETTINGS='UV_INDEX_URL UV_EXTRA_INDEX_URL UV_INDEX UV_DEFAULT_INDEX UV_FIND_LINKS UV_INDEX_STRATEGY UV_NO_INDEX UV_CONFIG_FILE PIP_INDEX_URL PIP_EXTRA_INDEX_URL PIP_FIND_LINKS PIP_NO_INDEX UV_NO_VERIFY_HASHES MAS_MANIFEST_VALIDATE MAS_MANIFEST_RESOLVE_REFS MAS_MANIFEST_STRICT OPENAI_API_KEY MAS_LLM_API_KEY'
CALLER_INDEX=http://ioc-case-101.invalid/simple
# caller_settings_env; prints the environment env.sh's ioc_env_main leaves, run from a shell
# that exports each of CALLER_SETTINGS, UV_NO_CONFIG=0 and a PIP_CONFIG_FILE of the case's
# own, with every step it runs but ioc_env_caches and ioc_env_enforce taken as done, and
# ends with ioc_env_main's status.
caller_settings_env() {
    local dir="$WORK/caller-settings"
    mkdir -p "$dir/cache"
    printf '[global]\nindex-url = %s\n' "$CALLER_INDEX" >"$dir/pip.conf"
    printf 'index-url = "%s"\n' "$CALLER_INDEX" >"$dir/uv.toml"
    (
        for name in $CALLER_SETTINGS; do
            case "$name" in
            UV_INDEX) value="case-101=$CALLER_INDEX" ;;
            UV_FIND_LINKS | PIP_FIND_LINKS) value="$CALLER_INDEX/links" ;;
            UV_INDEX_STRATEGY) value=unsafe-best-match ;;
            UV_NO_INDEX | PIP_NO_INDEX | UV_NO_VERIFY_HASHES | MAS_MANIFEST_STRICT) value=1 ;;
            MAS_MANIFEST_VALIDATE | MAS_MANIFEST_RESOLVE_REFS) value=0 ;;
            OPENAI_API_KEY | MAS_LLM_API_KEY) value=ioc-case-101-key ;;
            UV_CONFIG_FILE) value="$dir/uv.toml" ;;
            *) value="$CALLER_INDEX" ;;
            esac
            export "$name=$value"
        done
        export UV_NO_CONFIG=0 PIP_CONFIG_FILE="$dir/pip.conf" IOC_CACHE="$dir/cache" IOC_CLONES="$dir/clones"
        IOC_ENV_FUNCTIONS_ONLY=1
        # shellcheck source=env.sh
        . "$HERE/env.sh" || exit 1
        for step in $(declare -f ioc_env_main | grep -oE '\bioc_env_[a-z_]+' | sort -u); do
            case "$step" in
            ioc_env_main | ioc_env_caches | ioc_env_enforce | ioc_env_refuse) ;;
            *) eval "$step() { return 0; }" ;;
            esac
        done
        ioc_env_main || exit 1
        env
    )
}
settings="$(caller_settings_env 2>&1)"
status=$?
held=""
for name in $CALLER_SETTINGS; do
    printf '%s\n' "$settings" | grep -q "^$name=" && held="$held $name"
done
if [ "$status" = 0 ] && [ -z "$held" ] && printf '%s\n' "$settings" | grep -qxF UV_NO_CONFIG=1 &&
    printf '%s\n' "$settings" | grep -qxF PIP_CONFIG_FILE=/dev/null; then
    case_passed "the environment env.sh gives a row holds none of the caller's uv and pip settings or mas-lab's optional inputs, and holds UV_NO_CONFIG=1 and PIP_CONFIG_FILE=/dev/null"
else
    case_failed "the environment env.sh gives a row holds none of the caller's uv and pip settings or mas-lab's optional inputs, and holds UV_NO_CONFIG=1 and PIP_CONFIG_FILE=/dev/null" \
        "ioc_env_main ended $status; the caller's settings it still holds:${held:- none}; $(printf '%s\n' "$settings" | grep -E '^(UV_NO_CONFIG|PIP_CONFIG_FILE)=' | tr '\n' ' ')$(printf '%s\n' "$settings" | grep '^env\.sh' | head -1)"
fi
planted_run caller-index-assigned "true || UV_EXTRA_INDEX_URL=$CALLER_INDEX uv pip install -r \"\$IOC_EVID/ioc-venv.lock\"; printf 'ran once\\n'"
case_result "a row that assigns UV_EXTRA_INDEX_URL before an install is refused by name, as a name the gate records with" \
    "replay-command: f-behaviour-1 runs UV_EXTRA_INDEX_URL=[^ ]*, an assignment to UV_EXTRA_INDEX_URL, a name the gate records with" "$PLANTED_OUT"

# 102. A contains predicate over several lines holds all of them (test round 32). At
# a657fd7db7 evaluate reads contains with grep -qF, and grep takes a value holding a newline
# as a list of patterns, any one of which is a match. So a multi-line contains passes on
# output that holds one of its lines, and a value holding an empty line, as d-75's does
# between two quoted blocks, passes on any output with a line at all. Measured with the
# gate's own evaluate: d-75's predicate holds over output that is the one line 0. The case
# plants a two-line contains over output that holds only its first line, which must fail,
# d-75's predicate as claims.jsonl holds it over output that is the one line 0, which must
# fail, and the same two-line contains over output that holds both lines, which passes.
dir="$(fresh contains-first-line-only)"
plant_behaviour "$dir" "printf 'ran once\\n'" '{"contains":"ran once\nran twice"}'
case_result "a contains over two lines fails on output that holds only its first line" replay-command "$(fixture_run "$dir" --replay)"
dir="$(fresh contains-blank-line)"
plant_behaviour "$dir" "printf '0\\n'" "$(jq -c 'select(.id == "d-75") | .expect' "$HERE/claims.jsonl")"
case_result "d-75's contains, as claims.jsonl holds it, fails on output that is the one line 0" replay-command "$(fixture_run "$dir" --replay)"
dir="$(fresh contains-both-lines)"
plant_behaviour "$dir" "printf 'ran once\\nran twice\\n'" '{"contains":"ran once\nran twice"}'
honest_result "a contains over two lines passes on output that holds both" replay-command "$(fixture_run "$dir" --replay)"

# 103-109 come from the reading of every operator of expect after test round 32, whose
# fault, case 102, was a value grep read as a list of patterns: each case is a key whose
# value a tool read as something its rule does not say, and check.sh's header now states
# what each key holds and the shape of its value.
#
# 103. A one_of is a set membership: the output is one of its candidates. At 6f7143ab
# evaluate read each candidate with grep -qF, so a candidate held when it was a substring of
# any line of the output, 'agreed' inside 'disagreed', and jq printed a candidate holding a
# newline as two candidates, either of which held. The output without its trailing newlines
# now equals one whole candidate, and predicate-form refuses a candidate that holds a
# newline, and an empty one, which 6f7143ab skipped and which would now hold over empty
# output. The case plants 'agreed' over output that is 'disagreed', which must fail, the
# two forms predicate-form must refuse, and two candidates over output that is the second,
# which passes. The nine one_of rows of claims.jsonl each print exactly one candidate.
dir="$(fresh op-one-of-substring)"
plant_behaviour "$dir" "printf 'disagreed\\n'" '{"one_of":["agreed"]}'
case_result "a one_of over 'agreed' fails on output that is 'disagreed'" replay-command "$(fixture_run "$dir" --replay)"
dir="$(fresh op-one-of-two-lines)"
plant_behaviour "$dir" "printf 'flagged\\n'" '{"one_of":["agreed\nflagged"]}'
case_result "a one_of candidate that holds a newline is refused" 'predicate-form: f-behaviour-1 .*its one_of holds a candidate that is empty or holds a newline' "$(fixture_run "$dir" --replay)"
dir="$(fresh op-one-of-empty)"
plant_behaviour "$dir" "printf '\\n'" '{"one_of":[""]}'
case_result "an empty one_of candidate is refused" 'predicate-form: f-behaviour-1 .*its one_of holds a candidate that is empty or holds a newline' "$(fixture_run "$dir" --replay)"
dir="$(fresh op-one-of-member)"
plant_behaviour "$dir" "printf 'flagged\\n'" '{"one_of":["agreed","flagged"]}'
honest_result "a one_of passes on output that is one of its candidates" 'replay-command|predicate-form' "$(fixture_run "$dir" --replay)"

# 104. A matches is one extended regular expression over one line. At 6f7143ab evaluate
# read it with grep -qE, which takes a value holding a newline as one expression per line,
# any one of which is a match, and an empty expression, which matches every line. The
# case plants a matches over two lines over output that holds only its first, and an empty
# matches, which predicate-form must refuse, and an anchored matches of one line, which
# passes.
dir="$(fresh op-matches-two-lines)"
plant_behaviour "$dir" "printf 'ran once\\n'" '{"matches":"ran once\nran twice"}'
case_result "a matches that holds a newline is refused" 'predicate-form: f-behaviour-1 .*its matches holds a newline' "$(fixture_run "$dir" --replay)"
dir="$(fresh op-matches-empty)"
plant_behaviour "$dir" "printf 'ran once\\n'" '{"matches":""}'
case_result "an empty matches is refused" 'predicate-form: f-behaviour-1 .*its matches is empty' "$(fixture_run "$dir" --replay)"
dir="$(fresh op-matches-one-line)"
plant_behaviour "$dir" "printf 'ran once\\n'" '{"matches":"^ran once$"}'
honest_result "a matches of one line passes on output a line of which it matches" 'replay-command|predicate-form' "$(fixture_run "$dir" --replay)"

# 105. A contains is not empty. The empty value is a substring of any output, and so is a
# value of newlines alone once its trailing newlines are off, so at 6f7143ab each held over
# anything. The case plants both, which predicate-form must refuse, beside a contains of
# one word of the output, which passes.
dir="$(fresh op-contains-empty)"
plant_behaviour "$dir" "printf 'ran once\\n'" '{"contains":""}'
case_result "an empty contains is refused" 'predicate-form: f-behaviour-1 .*its contains is empty' "$(fixture_run "$dir" --replay)"
dir="$(fresh op-contains-newline)"
plant_behaviour "$dir" "printf 'ran once\\n'" '{"contains":"\n"}'
case_result "a contains of one newline is refused" 'predicate-form: f-behaviour-1 .*its contains is empty' "$(fixture_run "$dir" --replay)"
dir="$(fresh op-contains-word)"
plant_behaviour "$dir" "printf 'ran once\\n'" '{"contains":"ran"}'
honest_result "a contains of one word of the output passes" 'replay-command|predicate-form' "$(fixture_run "$dir" --replay)"

# 106. An equals, a contains and a matches take a string. At 6f7143ab evaluate read every
# value through jq's tostring, which gives null the text 'null' and true the text 'true',
# and a number the text jq prints for it, which differs between jq and jaq for 1.0; so
# {"contains":null} held over output that prints null. The case plants a contains of null
# and an equals of true over output that is that word, which predicate-form must refuse,
# beside the equals of the string "true", which passes.
dir="$(fresh op-type-null)"
plant_behaviour "$dir" "printf 'null\\n'" '{"contains":null}'
case_result "a contains of null is refused" 'predicate-form: f-behaviour-1 .*its contains is a JSON null, not a string' "$(fixture_run "$dir" --replay)"
dir="$(fresh op-type-boolean)"
plant_behaviour "$dir" "printf 'true\\n'" '{"equals":true}'
case_result "an equals of true is refused" 'predicate-form: f-behaviour-1 .*its equals is a JSON boolean, not a string' "$(fixture_run "$dir" --replay)"
dir="$(fresh op-type-string)"
plant_behaviour "$dir" "printf 'true\\n'" '{"equals":"true"}'
honest_result "an equals of the string true passes on output that is true" 'replay-command|predicate-form' "$(fixture_run "$dir" --replay)"

# 107. The value of a bound and of an exit status is one number, read whole. At 6f7143ab
# predicate_is_a_sentence held each to its form with grep -qE, which passes a value any
# one line of which is a number, so a bound of '0' and a line 'zero' after it, and an exit
# status the same, were predicates of the right shape; the row then failed only at its
# run, as an output that does not meet it. The case plants both, which predicate-form must
# refuse, beside a bound of '0' and an exit status of 0, which pass.
dir="$(fresh op-bound-two-lines)"
plant_behaviour "$dir" "printf '0\\n'" '{"at_most":"0\nzero"}'
case_result "a bound whose value is a number and a line after it is refused" 'predicate-form: f-behaviour-1 .*its at_most is no one number' "$(fixture_run "$dir" --replay)"
dir="$(fresh op-exit-two-lines)"
plant_behaviour "$dir" "printf 'ran once\\n'" '{"contains":"ran once","exit":"0\nzero"}'
case_result "an exit status whose value is a number and a line after it is refused" 'predicate-form: f-behaviour-1 .*its exit is no one whole number' "$(fixture_run "$dir" --replay)"
dir="$(fresh op-bound-one-number)"
plant_behaviour "$dir" "printf '0\\n'" '{"at_most":"0"}'
honest_result "a bound of one number passes on output that meets it" 'replay-command|predicate-form' "$(fixture_run "$dir" --replay)"
dir="$(fresh op-exit-one-number)"
plant_behaviour "$dir" "printf 'ran once\\n'" '{"contains":"ran once","exit":0}'
honest_result "an exit status of one whole number passes beside a contains both of which hold" 'replay-command|predicate-form' "$(fixture_run "$dir" --replay)"

# 108. A bound compares the two numbers exactly. At 6f7143ab bound compared them as python
# floats, which hold whole numbers exactly only up to 2^53, so 9007199254740993 was at most
# 9007199254740992. They are now compared as the decimals they are written as. The case
# plants that bound, which must fail, beside the bound at the number itself, which passes.
dir="$(fresh op-bound-past-float)"
plant_behaviour "$dir" "printf '9007199254740993\\n'" '{"at_most":"9007199254740992"}'
case_result "9007199254740993 is not at most 9007199254740992" replay-command "$(fixture_run "$dir" --replay)"
dir="$(fresh op-bound-exact)"
plant_behaviour "$dir" "printf '9007199254740993\\n'" '{"at_most":"9007199254740993"}'
honest_result "9007199254740993 is at most 9007199254740993" 'replay-command|predicate-form' "$(fixture_run "$dir" --replay)"

# 109. An exit status that was never recorded meets no exit predicate. At 6f7143ab evaluate
# read a missing status file as the status 1, so {"exit":1}, the exit status of 47 rows,
# held for a command whose status nothing recorded. run_recorded writes the status of
# every row --replay runs, so no fixture row reaches that path, and the case calls the
# gate's own evaluate: over an output with no status recorded beside it, {"exit":1} must
# not hold, and over the same output with the status 1 recorded, it holds.
mkdir -p "$WORK/op-status"
printf 'ran once\n' >"$WORK/op-status/out"
if evaluate '{"exit":1}' "$WORK/op-status/out"; then
    case_failed "an exit status that was never recorded meets no exit predicate" "evaluate held {\"exit\":1} over an output with no status recorded"
else
    case_passed "an exit status that was never recorded meets no exit predicate"
fi
printf '1' >"$WORK/op-status/out.status"
if evaluate '{"exit":1}' "$WORK/op-status/out"; then
    case_passed "a recorded exit status of 1 meets {\"exit\":1}"
else
    case_failed "a recorded exit status of 1 meets {\"exit\":1}" "evaluate did not hold {\"exit\":1} over an output with the status 1 recorded"
fi

# 110. Two rows that judge one claim of a record under check give it one mark, or the one
# that is not the claim's own row names the part it holds (round 34). At c1520338 tc-voc-2
# said that the record's sentence at d-17 is false as stated while d-17 was marked
# verified, and nothing linked the two rows, so nothing compared their marks. A row now
# names each record claim it judges in its checks, with the mark it gives it, and
# --replay's claim-marks-agree holds that mark to the named row's; the fixture's f-our-1
# checks u-1 with u-1's own mark. The case plants into the fixture's claims.jsonl a row
# that gives u-2, marked verified, the mark false and names no part it holds, which fails
# claim-marks-agree alone, naming both rows; u-2 marked false and split by two rows, each
# naming the part it holds in a refines, one part verified and one false, which passes;
# and a row that marks a part of u-1, marked verified, unverified with a refines, which
# fails claim-marks-agree alone, since a part of a claim marked verified is verified too.
# checks_row <id> <checks, a JSON array>; a row of the case's own carrying those checks.
checks_row() {
    jq -cn --arg id "$1" --argjson checks "$2" \
        '{id: $id, requirement: "r7", source: "the case", kind: "behaviour", claim: "A row that judges a claim of the fixture record.", mark: "verified", evidence: {surface: "cli"}, note: "planted", checks: $checks}'
}
dir="$(fresh marks-disagree)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
checks_row f-check-disagrees '[{"row":"u-2","mark":"false"}]' >>"$claims_copy"
one_failure_result "a row that gives u-2's claim, marked verified, the mark false and names no part it holds fails claim-marks-agree alone, naming both rows" claim-marks-agree \
    '^1 of 18 assertions failed: claim-marks-agree$' "^FAIL claim-marks-agree: f-check-disagrees gives u-2's claim the mark false, u-2 carries verified, and f-check-disagrees names no part it holds, so the two rows disagree on one claim\$" \
    "$(fixture_run "$dir" --replay)"
dir="$(fresh marks-split)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
sed -i '/"id":"u-2"/s/"mark":"verified"/"mark":"false"/' "$claims_copy"
grep -F '"id":"u-2"' "$claims_copy" | grep -qF '"mark":"false"' || {
    printf 'FAIL u-2 could not be marked false in the fixture copy of claims.jsonl\n'
    exit 1
}
checks_row f-check-part-held '[{"row":"u-2","mark":"verified","refines":"This row holds the count of thirteen, which the file prints."}]' >>"$claims_copy"
checks_row f-check-part-false '[{"row":"u-2","mark":"false","refines":"This row holds the word normative, which the file never uses of them."}]' >>"$claims_copy"
ending_result "u-2 marked false and split by two rows that each name the part they hold, one part verified and one false, passes" 0 '^all 18 assertions passed$' "$(fixture_run "$dir" --replay)"
dir="$(fresh marks-verified-part)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
checks_row f-check-part-unverified '[{"row":"u-1","mark":"unverified","refines":"This row holds the symbol the record names, which no command of it reads."}]' >>"$claims_copy"
one_failure_result "a row that marks a part of u-1's claim, marked verified, unverified with a refines fails claim-marks-agree alone, naming both rows" claim-marks-agree \
    '^1 of 18 assertions failed: claim-marks-agree$' "^FAIL claim-marks-agree: f-check-part-unverified gives a part of u-1's claim the mark unverified, and u-1 carries verified, " \
    "$(fixture_run "$dir" --replay)"

# 111. A claim with a part marked false is false (test round 34). claim-marks-agree holds a part
# of a claim marked verified to verified, since a part of a verified claim is verified too.
# The same rule read the other way is that a claim one of whose parts is false is false, so a
# row that gives a part of a claim the mark false disagrees with that claim's row unless the
# row is marked false. At 54cb2498 the gate passed a row that gives a part of u-2 the mark
# false with a refines while u-2 is marked unverified, and while u-2 is marked
# not-externally-checkable, so two rows gave one claim two verdicts and nothing failed. The
# case plants each of the two, which must fail claim-marks-agree alone, naming both rows, and
# the same part under u-2 marked false, which passes.
for theirs in unverified not-externally-checkable; do
    dir="$(fresh "false-part-under-$theirs")"
    claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    sed -i "/\"id\":\"u-2\"/s/\"mark\":\"verified\"/\"mark\":\"$theirs\"/" "$claims_copy"
    grep -F '"id":"u-2"' "$claims_copy" | grep -qF "\"mark\":\"$theirs\"" || {
        printf 'FAIL u-2 could not be marked %s in the fixture copy of claims.jsonl\n' "$theirs"
        exit 1
    }
    checks_row f-check-part-false '[{"row":"u-2","mark":"false","refines":"This row holds the count of thirteen, which the file does not print."}]' >>"$claims_copy"
    one_failure_result "a row that marks a part of u-2's claim, marked $theirs, false with a refines fails claim-marks-agree alone, naming both rows" claim-marks-agree \
        '^1 of 18 assertions failed: claim-marks-agree$' "^FAIL claim-marks-agree: .*f-check-part-false.*u-2" \
        "$(fixture_run "$dir" --replay)"
done
dir="$(fresh false-part-under-false)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
sed -i '/"id":"u-2"/s/"mark":"verified"/"mark":"false"/' "$claims_copy"
checks_row f-check-part-false '[{"row":"u-2","mark":"false","refines":"This row holds the count of thirteen, which the file does not print."}]' >>"$claims_copy"
ending_result "a row that marks a part of u-2's claim, marked false, false with a refines passes" 0 '^all 18 assertions passed$' "$(fixture_run "$dir" --replay)"

# 112. The count of our own failures runs again (the architect's reading of round 36
# against the approved check c-evidence-replays). At 8ab6c1a7 --replay judged the count by
# coordination/count.md alone, the committed text, and ran no query of it, so a committed
# count its walker no longer prints passed. --replay now runs coordination/count.py through
# live_read and holds what it prints to evidence/count.json, every figure but
# feed.events_read, the rows of the whole feed read up to the run, which grows with the
# feed and is no figure of the window. The fixture's count.py prints a fixed count and
# reads nothing, and its evidence/count.json is what it prints. The case plants an
# evidence/count.json one figure of its classes off what count.py prints, which fails
# count-reruns alone, naming the key; a count.py that raises on its read of the feed, as
# count.py's fetch does when Dispatch does not answer, and so exits non-zero with nothing
# on stdout, over which --replay cannot run, naming the count's source; and an
# evidence/count.json whose feed names another last event of the window, which fails
# count-reruns alone, naming feed; beside the honest fixture, which passes, and an
# evidence/count.json whose feed.events_read alone is below what count.py prints, as a
# feed grown since leaves it, which passes, its PASS line printing both values.
# count_plant <dir> <sed program> <text>; changes the fixture's evidence/count.json by the
# program, and stops when the count does not hold the text afterwards.
count_plant() {
    local count_json="$1/repo/docs/research/internet-of-cognition/evidence/count.json"
    sed -i "$2" "$count_json"
    grep -qF -- "$3" "$count_json" || {
        printf 'FAIL the count could not be planted to hold %s in the fixture copy of evidence/count.json\n' "$3"
        exit 1
    }
}
dir="$(fresh count-class-off)"
count_plant "$dir" 's/"inside_one_tree": 20,/"inside_one_tree": 21,/' '"inside_one_tree": 21,'
one_failure_result "an evidence/count.json one figure of its classes off what count.py prints fails count-reruns alone, naming the key and both values" count-reruns \
    '^1 of 18 assertions failed: count-reruns$' '^FAIL count-reruns: coordination/count\.py ran again over the window 2026-10-29T00:00:00Z to 2026-11-12T00:00:00Z and printed classes \{.*"inside_one_tree":20.*\}, and evidence/count\.json holds classes \{.*"inside_one_tree":21.*\}$' \
    "$(fixture_run "$dir" --replay)"
dir="$(fresh count-read-fails)"
cat >"$dir/repo/docs/research/internet-of-cognition/coordination/count.py" <<'PY'
# A walker whose read of the feed fails, as count.py's fetch raises when Dispatch does not
# answer: a traceback on stderr, nothing on stdout, exit 1.
import urllib.error

raise urllib.error.URLError(ConnectionRefusedError(111, "Connection refused"))
PY
ending_result "a count.py that exits non-zero with nothing on stdout, as a failed read of the feed leaves it, makes --replay cannot run, naming the count's source" 2 \
    "^check\\.sh cannot run --replay: Dispatch, the feed of the count's window did not answer: " "$(fixture_run "$dir" --replay)"
dir="$(fresh count-feed-off)"
count_plant "$dir" 's/"last_event_id_in_window": 900/"last_event_id_in_window": 901/' '"last_event_id_in_window": 901'
one_failure_result "an evidence/count.json whose feed names another last event of the window fails count-reruns alone, naming feed" count-reruns \
    '^1 of 18 assertions failed: count-reruns$' '^FAIL count-reruns: coordination/count\.py ran again over the window [^ ]* to [^ ]* and printed a count that differs from evidence/count\.json at feed$' \
    "$(fixture_run "$dir" --replay)"
dir="$(fresh count-honest)"
ending_result "the fixture whose evidence/count.json is what its count.py prints passes --replay" 0 '^all 18 assertions passed$' "$(fixture_run "$dir" --replay)"
dir="$(fresh count-feed-grown)"
count_plant "$dir" 's/"events_read": 1000,/"events_read": 900,/' '"events_read": 900,'
out="$(fixture_run "$dir" --replay)"
count_case="an evidence/count.json whose feed.events_read alone is below what count.py prints passes --replay, its PASS line printing both values"
if ! case_read "$count_case" "$out"; then
    :
elif [ "$RUN_ENDING" = passed ] && grep -qE '^PASS count-reruns: .*feed\.events_read.*: 1000 now, 900 in evidence/count\.json$' "$out/text"; then
    case_passed "$count_case"
else
    case_failed "$count_case" "$(grep -E '^(PASS|FAIL) count-reruns' "$out/text" | head -2 | tr '\n' ' ')$RUN_LINE"
fi

# 113. A landed step stands in the tree (the architect's brief of round 40). Before: from test
# round 36, this case took the two pair rows the page 'Each review round's shortfall recorded
# as two grader rows' landed out of a fresh checkout's pr-body.md and asked that the step's
# proof, the guard test, then fail. It changed because the forward merge 51c5860680 took
# main's pr-body.md and guard test whole, so that step is in this tree no longer and
# sources.json commits it as not landed, and no proof of it runs. At cc62bd3b --landed read
# that a landed page names the committed commit, that this pull request added it and that
# the committed proof passes, and never that the change stands in the tree, so it passed both
# steps once the merge had taken them out. The case commits the fixture's landed step with
# the sentence it published, "one row per half", in skills/legion-worker/references/pr-body.md,
# takes that sentence out of the fixture's pr-body.md, and runs --landed, which must fail
# landed-proof, naming the step, the file and the sentence.
dir="$(fresh landed-not-in-tree)"
jq '.landed[0].state = "landed" | .landed[0].published = [{"path": "skills/legion-worker/references/pr-body.md", "sentence": "one row per half"}]' \
    "$dir/repo/docs/research/internet-of-cognition/sources.json" >"$dir/sources.json" &&
    mv "$dir/sources.json" "$dir/repo/docs/research/internet-of-cognition/sources.json"
sed -i 's/, one row per half:$/:/' "$dir/repo/skills/legion-worker/references/pr-body.md"
grep -qF 'one row per half' "$dir/repo/skills/legion-worker/references/pr-body.md" && {
    printf 'FAIL the published sentence could not be taken out of the fixture'"'"'s pr-body.md\n'
    exit 1
}
case_result "a landed step whose published sentence its file no longer carries fails --landed, naming the file and the sentence" \
    "landed-proof: the landed step .An execution record under the gen_ai. names. published .one row per half. in skills/legion-worker/references/pr-body\\.md, and this tree.s skills/legion-worker/references/pr-body\\.md does not carry it" \
    "$(fixture_run "$dir" --landed)"

# 114. A page not landed says the reason sources.json commits (the architect's brief of round
# 40). Before: from test round 37, this case took the later-round half of the combining rule
# out of a fresh checkout's merge-gate.md and asked that the step's proof then fail. It changed
# because the forward merge 51c5860680 took main's merge-gate.md whole, so that step is in this
# tree no longer and sources.json commits it as not landed, for the condition "needs a
# decision of the owner's" and two records, and no proof of it runs. A step not landed is held
# instead to a page whose Not landed line says that condition word for word and names each
# record, and to each dispatch:// record resolving. At cc62bd3b --landed read no Not landed
# line and no state of sources.json's list. The case commits the fixture's one step as not
# landed and writes its page's Not landed line in three faulty forms: another condition of
# R12 with both records; the committed condition with no record; and both records, with a
# third committed beside them on the list and the page, a dispatch:// reference Dispatch does
# not hold. --landed must fail landed-proof for each, naming the page and the condition, the
# record it does not name, or the record that resolves to nothing.
for form in condition no-record unresolved; do
    dir="$(fresh "not-landed-$form")"
    case "$form" in
    condition) line="- **Not landed:** is more than one engineer-day ($NOT_LANDED_RECORD, cybermonklabs/legion#4544)" ;;
    no-record) line="- **Not landed:** needs a decision of the owner's" ;;
    unresolved) line="- **Not landed:** needs a decision of the owner's ($NOT_LANDED_RECORD, cybermonklabs/legion#4544, dispatch://INFRA-1484/ask/00000000-0000-0000-0000-000000000000)" ;;
    esac
    not_landed_plant "$dir" "$line" || {
        printf 'FAIL the %s form of the Not landed line could not be planted in the fixture\n' "$form"
        exit 1
    }
    if [ "$form" = unresolved ]; then
        jq '.landed[0].record += ["dispatch://INFRA-1484/ask/00000000-0000-0000-0000-000000000000"]' \
            "$dir/repo/docs/research/internet-of-cognition/sources.json" >"$dir/sources.json" &&
            mv "$dir/sources.json" "$dir/repo/docs/research/internet-of-cognition/sources.json"
    fi
    out="$(fixture_run "$dir" --landed)"
    case "$form" in
    condition)
        case_result "a page not landed for another condition than the one sources.json commits fails --landed, naming both" \
            "landed-proof: the page .An execution record under the gen_ai. names. says its step is not landed because it .is more than one engineer-day., and sources.json commits the condition .needs a decision of the owner.s." "$out"
        ;;
    no-record)
        case_result "a page not landed that names no record fails --landed, naming the record sources.json commits" \
            "landed-proof: the page .An execution record under the gen_ai. names. says its step is not landed, and its Not landed line does not name the record $NOT_LANDED_RECORD" "$out"
        ;;
    unresolved)
        case_result "a step not landed whose dispatch:// record Dispatch does not hold fails --landed, naming the record" \
            "landed-proof: the record dispatch://INFRA-1484/ask/00000000-0000-0000-0000-000000000000 of the step .An execution record under the gen_ai. names., which is not landed, resolves to no node Dispatch holds" "$out"
        ;;
    esac
done

# 115. A row that reads a private repository cannot run where git cannot read it (test round
# 37, and R11 of round 38, under which the gate reads GitHub through git and the REST API and
# never through gh). Rows read the deployment repository, a private repository
# IOC_DEPLOYMENT_REPO names: most from the clone env.sh fetches with git, and a few its pull
# requests through the REST API. A reader whose git cannot read that repository would see
# env.sh refuse its clone, or each row's read fail, a FAIL of replay-command that is no
# verdict on the deliverable. --replay reads the repository with git ls-remote through
# live_read before the GitHub budget and before env.sh builds anything
# (private_repository_read), after it read GitHub's own name for it
# (deployment_repository_read), so such a reader gets check.sh cannot run, exit 2, naming the
# input, IOC_DEPLOYMENT_REPO, never its value, and the rows. The case runs --replay over this
# directory with IOC_DEPLOYMENT_REPO the placeholder FIXTURE_DEPLOYMENT_REPO and a git of
# the case's own first on PATH, which refuses ls-remote of that repository with exit 128, as
# git does for a repository its credential cannot read, and runs the real git for everything
# else; the run must end exit 2, its last line naming the input and the rows, and quoting
# git's own line with $IOC_DEPLOYMENT_REPO where git wrote the placeholder in the address,
# with no FAIL line. A
# curl of the case's own, also first on PATH, answers the gate's read of the repository's
# name, the address it is handed as its config on its input, with the placeholder as
# GitHub's full_name, and answers nothing else, so a gate that read the repository after the
# budget, or not at all, would end at the budget's read, which names neither, and never start
# the whole replay. Both reads are the case's own plants, GitHub's, so the run's reason is
# read as planted when it is either, and a run that ends at the budget's read fails the case
# rather than reading as one that could not run.
PRIVATE="$WORK/private-repository"
mkdir -p "$PRIVATE/bin"
cat >"$PRIVATE/bin/git" <<'STUB'
#!/bin/sh
case " $* " in
*" ls-remote "*THE_DEPLOYMENT_REPO*)
    printf 'remote: Repository not found.\nfatal: repository '\''https://github.com/THE_DEPLOYMENT_REPO.git/'\'' not found\n' >&2
    exit 128
    ;;
esac
exec REAL_GIT "$@"
STUB
sed -i -e "s|REAL_GIT|'$(command -v git)'|" -e "s|THE_DEPLOYMENT_REPO|$FIXTURE_DEPLOYMENT_REPO|g" "$PRIVATE/bin/git"
cat >"$PRIVATE/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" --config - "*)
    if grep -q 'url = "https://api\.github\.com/repos/THE_DEPLOYMENT_REPO"'; then
        printf '{"full_name": "THE_DEPLOYMENT_REPO"}\n200'
        exit 0
    fi
    ;;
esac
printf 'curl: (7) the stub curl of case 115 answers no %s\n' "$*" >&2
exit 7
STUB
sed -i -e "s|THE_DEPLOYMENT_REPO|$FIXTURE_DEPLOYMENT_REPO|g" "$PRIVATE/bin/curl"
chmod +x "$PRIVATE/bin/git" "$PRIVATE/bin/curl"
out="$(PATH="$PRIVATE/bin:$PATH" IOC_DEPLOYMENT_REPO="$FIXTURE_DEPLOYMENT_REPO" gate_run "$HERE/check.sh" --replay)"
private_case="--replay whose git cannot read the deployment repository cannot run, exit 2, naming the input and the rows that read it, and prints no FAIL"
if grep -q '^FAIL' "$out/text"; then
    case_failed "$private_case" "$(grep '^FAIL' "$out/text" | head -2 | tr '\n' ' ')"
else
    ending_result "$private_case" 2 \
        "^check\\.sh cannot run --replay: GitHub's answer to git ls-remote of the deployment repository IOC_DEPLOYMENT_REPO names, which the rows n-collector-keep-keys, [^,]*(, [a-z0-9-]+)* read, did not answer: remote: Repository not found\\. fatal: repository 'https://github\\.com/\\\$IOC_DEPLOYMENT_REPO\\.git/' not found" "$out" \
        '^check\.sh cannot run --replay: GitHub'
fi

# 116. No gh anywhere in the gate (R11 of round 38): the gate reads GitHub through git and the
# REST API and never through gh, which in a Legion pod is scoped to this project's owners
# alone. ADMITTED holds no entry for gh, so --admitted refuses a row whose command runs it:
# the case plants a row that runs `gh api …` into a copy of claims.jsonl and env.sh, and
# --admitted must fail admitted-rows alone, naming that row and gh. And no file the gate or a
# row runs calls gh: gh_calls reads every .sh and .py file under this directory but
# check.test.sh, check.sh, env.sh, citations.py, coordination/count.py, similarity/ and the
# scripts under evidence/ that rows run among them, for a line outside comments that calls gh
# as a word, and the case fails naming the file and the line of each it finds. A line is a
# comment when its first character that is not a space is #. gh_calls is shown to bite first:
# over a directory holding one script with gh in a comment and gh called on the next line, it
# names that line alone.
NO_GH="$WORK/no-gh"
mkdir -p "$NO_GH"
cp "$HERE/claims.jsonl" "$HERE/env.sh" "$NO_GH/"
ln -s "$HERE/evidence" "$NO_GH/evidence"
printf '%s\n' '{"claim": "The deployment repository names its commit 6908f072 by its id.", "command": "gh api repos/example-owner/deployment/commits/6908f07257629dfb202b91c35c2fc2c70753b3a7 --jq .sha", "evidence": {"surface": "cli"}, "expect": {"equals": "6908f07257629dfb202b91c35c2fc2c70753b3a7"}, "id": "f-gh-read", "kind": "behaviour", "mark": "verified", "note": "planted by case 116", "output": "6908f07257629dfb202b91c35c2fc2c70753b3a7", "requirement": "r7", "source": "the case"}' \
    >>"$NO_GH/claims.jsonl"
one_failure_result "--admitted over a row that runs gh fails admitted-rows alone, naming the row and gh" admitted-rows \
    '^1 of 4 assertions failed: admitted-rows$' '^FAIL admitted-rows: the row f-gh-read is refused: gh, ' \
    "$(IOC_EVIDENCE="$NO_GH" gate_run "$HERE/check.sh" --admitted)"
# gh_calls <directory>; prints <file>:<line>:<text> for each line of a .sh or .py file under
# <directory>, check.test.sh aside, that calls gh as a word outside a comment.
gh_calls() {
    find "$1" -type f \( -name '*.sh' -o -name '*.py' \) ! -name check.test.sh -print0 | sort -z |
        xargs -0 grep -nE '(^|[^a-z_.-])gh( |$)' /dev/null | grep -vE '^[^:]*:[0-9]+:[[:space:]]*#'
}
mkdir -p "$WORK/gh-planted"
printf '#!/bin/sh\n# gh api in a comment is no call\ngh api repos/example-owner/deployment --jq .full_name\n' >"$WORK/gh-planted/planted.sh"
calls="$(gh_calls "$WORK/gh-planted")"
if [ "$calls" = "$WORK/gh-planted/planted.sh:3:gh api repos/example-owner/deployment --jq .full_name" ]; then
    case_passed "gh_calls names a script's call of gh by its file and line, and not gh in a comment"
else
    case_failed "gh_calls names a script's call of gh by its file and line, and not gh in a comment" "it printed: $(printf '%s' "$calls" | head -3 | tr '\n' ' ')"
fi
calls="$(gh_calls "$HERE")"
if [ -z "$calls" ]; then
    case_passed "no script the gate or a row runs calls gh outside a comment"
else
    case_failed "no script the gate or a row runs calls gh outside a comment" "$(printf '%s\n' "$calls" | cut -c1-200 | head -5 | tr '\n' ' ')"
fi

# 117. The GitHub tokens stand on no command line, in no output and in no file the run leaves
# (the architect's conditions of round 38 on R11's header files). --replay writes one header
# file per repository under its own scratch with umask 077 (github_headers), every recorded
# curl of GitHub's REST API sends the file with -H @<file>, and the scratch goes when the run
# ends. The case replays, over the fixture, the rows of claims.jsonl that send a header file,
# with a git of the case's own first on PATH that answers git credential fill with a token of
# the case's own, whose prefix ghs_IOCFAKETOKEN117 stands nowhere else, and runs the real git
# for everything else. Two rows of the case's own follow them: one prints the size of each
# header file, so the case reads that each held that token while the run went on, and one
# copies the replay's recorded files, every row's output, command, failed reads and statuses,
# out of the run's scratch before the run removes it. The run is bounded: the fixture builds
# nothing with env.sh and reads no budget and no private repository (IOC_NO_ENV), and a curl of
# the case's own, first on PATH, answers each read of api.github.com with 200 and an empty list,
# so the replay walks every row and reads no network; the rows' predicates then fail, which the
# case does not judge. Since round 39 a refusal of the credential ends the mode cannot-run at the
# first row that sends it, so a case that let GitHub refuse its token would end before its copy
# row ran. That curl records its arguments and whether the header file it was handed held the
# case's token, and writes nothing of the token: so the token on a command line would stand in
# that record, and a read that sent no header file shows there. Its clone root, IOC_CLONES, is a
# directory of the case's own beside its TMPDIR, since the gate keeps clones there across runs,
# so whatever the run leaves under its TMPDIR is its own. The case fails when the token's prefix
# stands in the run's whole log, its stdout and stderr, in any recorded file, in the curl's own
# record, or in any file under the fixture, the clone root or the TMPDIR, when a header file did
# not hold the token, when a read of api.github.com was sent without it, or when the run leaves
# any file under its TMPDIR, the header files among them.
TOKEN_CASE="$WORK/github-token"
TOKEN_PREFIX=ghs_IOCFAKETOKEN117
mkdir -p "$TOKEN_CASE/bin" "$TOKEN_CASE/tmp" "$TOKEN_CASE/clones" "$TOKEN_CASE/outputs"
cat >"$TOKEN_CASE/bin/git" <<'STUB'
#!/bin/sh
case " $* " in
*" credential fill "*)
    cat >/dev/null
    printf 'protocol=https\nhost=github.com\nusername=x-access-token\npassword=ghs_IOCFAKETOKEN117c4s3n0tAr34lt0k3n\n'
    exit 0
    ;;
esac
exec REAL_GIT "$@"
STUB
sed -i "s|REAL_GIT|'$(command -v git)'|" "$TOKEN_CASE/bin/git"
chmod +x "$TOKEN_CASE/bin/git"
cat >"$TOKEN_CASE/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" https://api.github.com/"*) ;;
*) exec REAL_CURL "$@" ;;
esac
printf '%s\n' "$*" >>CASE_DIR/outputs/curl-argv
held=header-lacks-token
prev=
for arg in "$@"; do
    if [ "$prev" = -H ]; then
        case "$arg" in
        @*) grep -q '^Authorization: Bearer ghs_IOCFAKETOKEN117' "${arg#@}" 2>/dev/null && held=header-holds-token ;;
        esac
    fi
    prev="$arg"
done
printf '%s\n' "$held" >>CASE_DIR/outputs/curl-reads
printf '[]\n'
exit 0
STUB
sed -i "s|REAL_CURL|'$(command -v curl)'|; s|CASE_DIR|'$TOKEN_CASE'|g" "$TOKEN_CASE/bin/curl"
chmod +x "$TOKEN_CASE/bin/curl"
dir="$(fresh github-token-fixture)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
jq -c 'select((.command // "") | test("IOC_GITHUB_AUTH_"))' "$HERE/claims.jsonl" >"$TOKEN_CASE/rows"
[ -s "$TOKEN_CASE/rows" ] || {
    printf 'FAIL no row of claims.jsonl sends a GitHub header file, so case 117 has no row to replay\n'
    exit 1
}
cat "$TOKEN_CASE/rows" >>"$claims_copy"
jq -cn '{id: "f-header-size", requirement: "r7", source: "the case", kind: "behaviour", claim: "Each GitHub header file holds one header line.", mark: "verified", evidence: {surface: "cli"}, command: "wc -c \"$IOC_GITHUB_AUTH_LEGION\" \"$IOC_GITHUB_AUTH_PRIVATE\"", expect: {exit: 0}, output: "", note: "planted by case 117"}' >>"$claims_copy"
jq -cn --arg to "$TOKEN_CASE/outputs/replay" '{id: "f-copy-outputs", requirement: "r7", source: "the case", kind: "behaviour", claim: "The recorded files of the rows before it are copied.", mark: "verified", evidence: {surface: "cli"}, command: ("cp -r \"$TMPDIR/..\" " + $to), expect: {exit: 0}, output: "", note: "planted by case 117"}' >>"$claims_copy"
out="$(PATH="$TOKEN_CASE/bin:$PATH" TMPDIR="$TOKEN_CASE/tmp" IOC_CLONES="$TOKEN_CASE/clones" fixture_run "$dir" --replay)"
token_case="a replay of the rows that send a GitHub header file prints the token in no line and no recorded output, and leaves no header file behind"
header_bytes="$(printf 'Authorization: Bearer %s\n' "${TOKEN_PREFIX}c4s3n0tAr34lt0k3n" | wc -c)"
if case_read "$token_case" "$out"; then
    leaks="$(grep -rlF "$TOKEN_PREFIX" "$dir" "$TOKEN_CASE/outputs" "$TOKEN_CASE/clones" "$TOKEN_CASE/tmp" 2>/dev/null)"
    sizes="$(awk '$2 != "total" { print $1 }' "$TOKEN_CASE/outputs/replay/f-header-size.out" 2>/dev/null | sort -u)"
    left="$(find "$TOKEN_CASE/tmp" -mindepth 1 2>/dev/null | head -3)"
    if [ -n "$leaks" ]; then
        case_failed "$token_case" "the token's prefix stands in $(printf '%s' "$leaks" | head -3 | tr '\n' ' ')"
    elif [ ! -s "$TOKEN_CASE/outputs/replay/$(jq -r '.id' "$TOKEN_CASE/rows" | head -1).out.command" ]; then
        case_failed "$token_case" "the recorded files of the rows that send a header were not copied, so none was read: $RUN_LINE"
    elif [ ! -s "$TOKEN_CASE/outputs/curl-reads" ]; then
        case_failed "$token_case" "no row's read reached the case's curl of api.github.com, so no read was seen to send a header file: $RUN_LINE"
    elif grep -qxF header-lacks-token "$TOKEN_CASE/outputs/curl-reads"; then
        case_failed "$token_case" "$(grep -cxF header-lacks-token "$TOKEN_CASE/outputs/curl-reads") read(s) of api.github.com sent no header file holding the case's token"
    elif [ "$sizes" != "$header_bytes" ]; then
        case_failed "$token_case" "the header files did not each hold the case's token while the run went on: their sizes were $(printf '%s' "$sizes" | tr '\n' ' ')where $header_bytes was asked"
    elif [ -n "$left" ]; then
        case_failed "$token_case" "the run left files under its TMPDIR: $(printf '%s' "$left" | tr '\n' ' ')"
    else
        case_passed "$token_case"
    fi
fi

# 118. The GitHub header files are written again while the run goes on (the clean-clone
# replay at 882b668c). A Legion pod's git credential fill gives a GitHub App installation
# token, which lives one hour, and that replay ran 3723 s: env.sh's read of the review corpus
# at its start passed with the file github_headers wrote, and about an hour later the five
# rows that sent the same file failed together, each a curl of GitHub's REST API exiting 22.
# --replay now writes the files again before a row when they are GITHUB_HEADER_AGE seconds
# old (github_headers_fresh), into a directory beside them, and renames each over its old
# file. The case sets that age to 0 by IOC_GITHUB_HEADER_AGE, so that every row meets files
# written again, and puts first on PATH a git of the case's own whose git credential fill
# answers each call with a token one character longer than the call before, ghs_IOCFAKE118_
# and then one x per call so far, and runs the real git for everything else. Two rows of the
# case's own each print the size of the file IOC_GITHUB_AUTH_LEGION names and nothing of
# it, and a third copies the replay's recorded files out of the run's scratch before the run
# removes it, as case 117's does. The run is bounded as case 117's is: the fixture builds
# nothing with env.sh and reads no budget and no private repository (IOC_NO_ENV), and its
# clone root is a directory of the case's own beside its TMPDIR. The case fails when the
# second row's file is no longer than the first row's, so it was not written again between
# them, when the first holds no token of the case's, when the token's prefix stands in the
# run's whole log, in any recorded file, or in any file under the fixture, the clone root
# or the TMPDIR, or when the run leaves any file under its TMPDIR, the header files among
# them.
RENEW_CASE="$WORK/github-header-renewal"
RENEW_PREFIX=ghs_IOCFAKE118
mkdir -p "$RENEW_CASE/bin" "$RENEW_CASE/tmp" "$RENEW_CASE/clones" "$RENEW_CASE/outputs"
cat >"$RENEW_CASE/bin/git" <<'STUB'
#!/bin/sh
case " $* " in
*" credential fill "*)
    cat >/dev/null
    calls=$(($(cat CALLS 2>/dev/null || printf 0) + 1))
    printf '%s\n' "$calls" >CALLS
    printf 'protocol=https\nhost=github.com\nusername=x-access-token\npassword=ghs_IOCFAKE118_%s\n' "$(printf "%${calls}s" '' | tr ' ' x)"
    exit 0
    ;;
esac
exec REAL_GIT "$@"
STUB
sed -i -e "s|REAL_GIT|'$(command -v git)'|" -e "s|CALLS|'$RENEW_CASE/calls'|g" "$RENEW_CASE/bin/git"
chmod +x "$RENEW_CASE/bin/git"
dir="$(fresh github-header-renewal-fixture)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
for row in first second; do
    jq -cn --arg id "f-header-$row" --arg claim "The GitHub header file of cybermonklabs/legion holds one header line when the $row row reads it." \
        '{id: $id, requirement: "r7", source: "the case", kind: "behaviour", claim: $claim, mark: "verified", evidence: {surface: "cli"}, command: "wc -c \"$IOC_GITHUB_AUTH_LEGION\"", expect: {exit: 0}, output: "", note: "planted by case 118"}' >>"$claims_copy"
done
jq -cn --arg to "$RENEW_CASE/outputs/replay" '{id: "f-copy-outputs", requirement: "r7", source: "the case", kind: "behaviour", claim: "The recorded files of the rows before it are copied.", mark: "verified", evidence: {surface: "cli"}, command: ("cp -r \"$TMPDIR/..\" " + $to), expect: {exit: 0}, output: "", note: "planted by case 118"}' >>"$claims_copy"
out="$(PATH="$RENEW_CASE/bin:$PATH" TMPDIR="$RENEW_CASE/tmp" IOC_CLONES="$RENEW_CASE/clones" IOC_GITHUB_HEADER_AGE=0 fixture_run "$dir" --replay)"
renew_case="a replay writes the GitHub header files again before a row once they reach their age, and prints the token in no line and no recorded output, and leaves no header file behind"
base_bytes="$(printf 'Authorization: Bearer %s_\n' "$RENEW_PREFIX" | wc -c)"
if case_read "$renew_case" "$out"; then
    leaks="$(grep -rlF "$RENEW_PREFIX" "$dir" "$RENEW_CASE/outputs" "$RENEW_CASE/clones" "$RENEW_CASE/tmp" 2>/dev/null)"
    first="$(awk '{ print $1; exit }' "$RENEW_CASE/outputs/replay/f-header-first.out" 2>/dev/null)"
    second="$(awk '{ print $1; exit }' "$RENEW_CASE/outputs/replay/f-header-second.out" 2>/dev/null)"
    left="$(find "$RENEW_CASE/tmp" -mindepth 1 2>/dev/null | head -3)"
    if [ -n "$leaks" ]; then
        case_failed "$renew_case" "the token's prefix stands in $(printf '%s' "$leaks" | head -3 | tr '\n' ' ')"
    elif ! [[ "$first" =~ ^[0-9]+$ && "$second" =~ ^[0-9]+$ ]]; then
        case_failed "$renew_case" "the sizes the two rows printed were not copied, so none was read: $RUN_LINE"
    elif [ "$first" -le "$base_bytes" ]; then
        case_failed "$renew_case" "the header file the first row read held no token of the case's: $first bytes, where more than $base_bytes were asked"
    elif [ "$second" -le "$first" ]; then
        case_failed "$renew_case" "the second row read the file the first row read, $first bytes and then $second, so the file was not written again between them"
    elif [ -n "$left" ]; then
        case_failed "$renew_case" "the run left files under its TMPDIR: $(printf '%s' "$left" | tr '\n' ' ')"
    else
        case_passed "$renew_case"
    fi
fi

# 119. A failed read of GitHub's REST API names what GitHub answered, and a refusal for
# GitHub's rate limit is no verdict on the deliverable (the architect's ruling on the replay
# at 882b668c). There the five rows that sent a dead token each failed with "'curl -sS
# --fail-with-body -H @<file> https://api.github.com/<path>' exited 22" and nothing more,
# since each hands GitHub's body to jq and the row's output holds only curl's own line, so
# that GitHub had answered 401 was an inference. When a row's failed read is a curl of
# GitHub's REST API, the gate now reads that curl's URL once more itself, with the header
# file the curl sent (source_answer): its FAIL of replay-command names GitHub's status and
# the first line of its message, and a 403 or 429 for GitHub's rate limit ends the mode,
# cannot run, of the kind source, naming the limit. The case plants into each of two
# fixtures one row of the case's own that runs tc-input-1's command, a curl of GitHub's
# REST API piped into jq, and puts first on PATH a git that answers git credential fill
# with a token of the case's own, as case 117's does, and a curl of the case's own, which
# answers a read of api.github.com as GitHub does, with the body and the headers of the
# answer its directory holds: to the row, the body on stdout, curl's line on stderr and
# exit 22, and to a read with -o, the body and the headers into the files it names and the
# status, recording what that read sent. It runs the real curl for any other address. Its
# first answer is 401 Bad credentials, an answer about the reader's credential and not about
# the deliverable: the run must end exit 2, cannot run, with no FAIL line, its last line
# naming 401 and the message, after one read of the gate's own of the row's URL with the
# header file IOC_GITHUB_AUTH_LEGION names. This form changed by the architect's order of
# 2026-11-19T15:35Z (round 39), that an expired token is the reader's and must never read
# as a wrong deliverable; until then the run had to fail replay-command, naming 401 and the
# message. Its second is a 403 whose message names the rate limit and whose
# x-ratelimit-remaining is 0: the run must end exit 2 with no FAIL line, its last line
# naming the limit. In neither may the token's prefix stand in the run's log.
ANSWER_CASE="$WORK/github-answer"
ANSWER_PREFIX=ghs_IOCFAKETOKEN119
mkdir -p "$ANSWER_CASE/bin"
cat >"$ANSWER_CASE/bin/git" <<'STUB'
#!/bin/sh
case " $* " in
*" credential fill "*)
    cat >/dev/null
    printf 'protocol=https\nhost=github.com\nusername=x-access-token\npassword=ghs_IOCFAKETOKEN119c4s3n0tAr34lt0k3n\n'
    exit 0
    ;;
esac
exec REAL_GIT "$@"
STUB
sed -i "s|REAL_GIT|'$(command -v git)'|" "$ANSWER_CASE/bin/git"
chmod +x "$ANSWER_CASE/bin/git"
for answer in 401 403; do
    mkdir -p "$ANSWER_CASE/$answer/bin"
    cat >"$ANSWER_CASE/$answer/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" https://api.github.com/"*) ;;
*) exec REAL_CURL "$@" ;;
esac
body='' dump='' format='' previous=''
for word in "$@"; do
    case "$previous" in
    -o) body=$word ;;
    -D) dump=$word ;;
    -w) format=$word ;;
    esac
    previous=$word
done
status=$(cat ANSWER/status)
if [ -n "$body" ]; then
    printf '%s\n' "$*" >>ANSWER/reads
    cat ANSWER/body >"$body"
    [ -z "$dump" ] || cat ANSWER/headers >"$dump"
    [ -z "$format" ] || printf '%s' "$status"
    exit 0
fi
cat ANSWER/body
printf 'curl: (22) The requested URL returned error: %s\n' "$status" >&2
exit 22
STUB
    sed -i -e "s|REAL_CURL|'$(command -v curl)'|" -e "s|ANSWER|'$ANSWER_CASE/$answer'|g" "$ANSWER_CASE/$answer/bin/curl"
    chmod +x "$ANSWER_CASE/$answer/bin/curl"
    printf '%s\n' "$answer" >"$ANSWER_CASE/$answer/status"
done
printf '{\n  "message": "Bad credentials",\n  "documentation_url": "https://docs.github.com/rest",\n  "status": "401"\n}\n' >"$ANSWER_CASE/401/body"
printf 'HTTP/2 401\r\ncontent-type: application/json; charset=utf-8\r\nx-ratelimit-limit: 60\r\nx-ratelimit-remaining: 59\r\n\r\n' >"$ANSWER_CASE/401/headers"
printf '{\n  "message": "API rate limit exceeded for installation ID 119.",\n  "documentation_url": "https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api",\n  "status": "403"\n}\n' >"$ANSWER_CASE/403/body"
printf 'HTTP/2 403\r\ncontent-type: application/json; charset=utf-8\r\nx-ratelimit-limit: 5000\r\nx-ratelimit-remaining: 0\r\nx-ratelimit-reset: 1794700800\r\nx-ratelimit-resource: core\r\n\r\n' >"$ANSWER_CASE/403/headers"
jq -c 'select(.id == "tc-input-1") | {id: "f-github-read", requirement: "r7", source: "the case", kind: "behaviour", claim: "The input driven through the library is review 5854791039 of pull request cybermonklabs/legion#3522.", mark: "verified", evidence: {surface: "cli"}, command, expect, output: "", note: "planted by case 119"}' "$HERE/claims.jsonl" >"$ANSWER_CASE/row"
[ -s "$ANSWER_CASE/row" ] && grep -qF 'https://api.github.com/' "$ANSWER_CASE/row" || {
    printf 'FAIL tc-input-1 is no row of claims.jsonl that curls GitHub'"'"'s REST API, so case 119 has no command to plant\n'
    exit 1
}
dir="$(fresh github-answer-401)"
cat "$ANSWER_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(PATH="$ANSWER_CASE/401/bin:$ANSWER_CASE/bin:$PATH" fixture_run "$dir" --replay)"
answer_case="a row whose curl of GitHub's REST API is answered 401 cannot run, exit 2, naming the status and GitHub's message, read once more with the row's header file, and prints no FAIL"
reads="$(cat "$ANSWER_CASE/401/reads" 2>/dev/null)"
if grep -q '^FAIL' "$out/text"; then
    case_failed "$answer_case" "$(grep '^FAIL' "$out/text" | head -2 | tr '\n' ' ')"
elif grep -qF "$ANSWER_PREFIX" "$out/text"; then
    case_failed "$answer_case" "the token's prefix stands in the run's log"
elif [ "$(printf '%s\n' "$reads" | grep -c .)" != 1 ] ||
    ! printf '%s\n' "$reads" | grep -qE -- '-H @[^ ]*/IOC_GITHUB_AUTH_LEGION\.header https://api\.github\.com/repos/cybermonklabs/legion/pulls/3522/reviews$'; then
    case_failed "$answer_case" "the gate did not read the row's URL once with the row's header file; it read: $(printf '%s' "$reads" | head -3 | tr '\n' ' ')"
else
    ending_result "$answer_case" 2 \
        "^check\\.sh cannot run --replay now: GitHub refused the credential of f-github-read's read of its REST API, so the row's failure is no verdict on the deliverable: .*GitHub answered 401: Bad credentials\$" "$out"
fi
dir="$(fresh github-answer-403)"
cat "$ANSWER_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(PATH="$ANSWER_CASE/403/bin:$ANSWER_CASE/bin:$PATH" fixture_run "$dir" --replay)"
answer_case="a row whose curl of GitHub's REST API meets GitHub's rate limit cannot run, exit 2, naming the limit, and prints no FAIL"
if grep -q '^FAIL' "$out/text"; then
    case_failed "$answer_case" "$(grep '^FAIL' "$out/text" | head -2 | tr '\n' ' ')"
elif grep -qF "$ANSWER_PREFIX" "$out/text"; then
    case_failed "$answer_case" "the token's prefix stands in the run's log"
else
    ending_result "$answer_case" 2 \
        "^check\\.sh cannot run --replay now: GitHub's rate limit refused f-github-read's read of its REST API, so the row's failure is no verdict on the deliverable: GitHub answered 403: API rate limit exceeded for installation ID 119\\.; its core rate limit has 0 of its 5000 requests left until [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z\$" "$out"
fi

# 120. Each candidate page's landing component is Legion, Envoy or Dispatch (test round 38).
# The specification's R9 lists the eleven things a page carries, among them "the landing
# component, which is Legion, Envoy or Dispatch". At 3df528a3 the page 'An execution record
# under the gen_ai. names' reads "Legion, for the phase key, and the spend collector, for its
# keep-list and a view", and the spend collector is a program of the deployment repository,
# none of the three; --shape passed it, since page-parts holds only that the line is there.
# The case writes that line into the first page of the fixture's judgement whose landing
# component is Legion, and runs --shape, which must fail, naming the page and its landing
# component.
dir="$(fresh landing-outside-r9)"
sed -i '0,/^- \*\*Landing component:\*\* Legion$/s//- **Landing component:** Legion, for the phase key, and the spend collector, for its keep-list and a view./' "$dir/judgement.md"
grep -qF 'and the spend collector, for its keep-list' "$dir/judgement.md" || {
    printf 'FAIL the landing component could not be planted in the fixture judgement\n'
    exit 1
}
case_result "a page whose landing component names one outside Legion, Envoy and Dispatch fails --shape, naming the page" \
    'page-[a-z-]+: .*[Ll]anding component' "$(fixture_run "$dir" --shape)"

# 121. A row whose read of GitHub's REST API gets no answer at all is no verdict on the
# deliverable (test round 38). Case 119 holds a refusal for GitHub's rate limit as a mode
# that cannot run, since that failure is GitHub's and not the deliverable's, and live_read
# holds a curl transport error the same way for the gate's own reads. A row's curl that
# cannot reach api.github.com, and the gate's own read of the same URL that source_answer
# makes after it, which gets no status either, is that same outage. At 3df528a3 it ended
# exit 1 with 'FAIL replay-command: f-github-read ran its command and a read inside it
# failed: ... exited 7, and GitHub answered nothing when the gate read ... again', a verdict
# on the deliverable during an outage. The tester's run of n-arms-precedent on 2026-11-19
# met one: 'curl: (28) Failed to connect to api.github.com port 443 after 127038 ms'. The
# case plants case 119's row with a curl of the case's own first on PATH that cannot
# connect to api.github.com, and expects --replay to end exit 2, cannot run, naming GitHub,
# with no FAIL line.
OUTAGE_CASE="$WORK/github-outage"
mkdir -p "$OUTAGE_CASE/bin"
cat >"$OUTAGE_CASE/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" https://api.github.com/"*)
    printf 'curl: (7) Failed to connect to api.github.com port 443 after 3 ms: Could not connect to server\n' >&2
    exit 7
    ;;
esac
exec REAL_CURL "$@"
STUB
sed -i "s|REAL_CURL|'$(command -v curl)'|" "$OUTAGE_CASE/bin/curl"
chmod +x "$OUTAGE_CASE/bin/curl"
dir="$(fresh github-outage)"
cat "$ANSWER_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(PATH="$OUTAGE_CASE/bin:$ANSWER_CASE/bin:$PATH" fixture_run "$dir" --replay)"
outage_case="a row whose curl of GitHub's REST API reaches no GitHub cannot run, exit 2, naming GitHub, and prints no FAIL"
if grep -q '^FAIL' "$out/text"; then
    case_failed "$outage_case" "$(grep '^FAIL' "$out/text" | head -2 | cut -c1-300 | tr '\n' ' ')"
else
    ending_result "$outage_case" 2 '^check\.sh cannot run --replay.*GitHub' "$out"
fi

# 122. A row whose own curl of GitHub's REST API got no HTTP answer is no verdict on the
# deliverable, whatever the gate's read of it again gets (the architect's rule of round 39:
# only an answer from GitHub judges the deliverable). Every row that reads GitHub's REST API
# runs curl with --fail-with-body, which exits 22 when GitHub answered with an HTTP error;
# any other exit of the row's curl, as 6, 7, 28, 35, 52 or 56, means its read got no HTTP
# answer. An outage that ends between the row's read and the gate's read again
# (source_answer) left a FAIL naming an answer the row never got: with case 121 held, a row
# whose curl exited 28 and whose read again got 200 ended exit 1, 'FAIL replay-command:
# f-github-read ran its command and a read inside it failed: ... exited 28, and GitHub
# answered 200'. The case plants case 119's row with a curl of the case's own first on PATH
# that answers the row's read with no body, curl's line of a timeout on stderr and exit 28,
# and the gate's read with -o, as case 119's does, with a JSON body and 200, and expects
# --replay to end exit 2, cannot run, naming the row's exit 28 and the 200, with no FAIL
# line.
LATE_CASE="$WORK/github-late-answer"
mkdir -p "$LATE_CASE/bin"
cat >"$LATE_CASE/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" https://api.github.com/"*) ;;
*) exec REAL_CURL "$@" ;;
esac
body='' dump='' format='' previous=''
for word in "$@"; do
    case "$previous" in
    -o) body=$word ;;
    -D) dump=$word ;;
    -w) format=$word ;;
    esac
    previous=$word
done
if [ -n "$body" ]; then
    printf '[{"id": 5854791039, "body": "planted by case 122"}]\n' >"$body"
    [ -z "$dump" ] || printf 'HTTP/2 200\r\ncontent-type: application/json; charset=utf-8\r\n\r\n' >"$dump"
    [ -z "$format" ] || printf '200'
    exit 0
fi
printf 'curl: (28) Failed to connect to api.github.com port 443 after 127038 ms: Timeout was reached\n' >&2
exit 28
STUB
sed -i "s|REAL_CURL|'$(command -v curl)'|" "$LATE_CASE/bin/curl"
chmod +x "$LATE_CASE/bin/curl"
dir="$(fresh github-late-answer)"
cat "$ANSWER_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(PATH="$LATE_CASE/bin:$ANSWER_CASE/bin:$PATH" fixture_run "$dir" --replay)"
late_case="a row whose own curl of GitHub's REST API got no HTTP answer cannot run, exit 2, naming its exit 28, though the gate's read again is answered 200, and prints no FAIL"
if grep -q '^FAIL' "$out/text"; then
    case_failed "$late_case" "$(grep '^FAIL' "$out/text" | head -2 | cut -c1-300 | tr '\n' ' ')"
else
    ending_result "$late_case" 2 '^check\.sh cannot run --replay.*GitHub.*f-github-read.*exited 28.*200' "$out"
fi

# 123. A row whose dispatch command got no answer from Dispatch, a server error or an answer
# about the reader's credential is no verdict on the deliverable (the architect's rule of round
# 39, that only an answer from the source judges the deliverable, and his order of
# 2026-11-19T15:35Z, that an answer about the reader's credential judges it no more). Of the
# rows of claims.jsonl, 76 run the dispatch command, and each pipes what it prints into sed,
# grep or awk, so the row's output holds nothing of a refusal, which the dispatch command
# prints on stdout before it exits 1. Measured in this pod on 2026-11-19, `dispatch read
# --issue INFRA-3134` printed 'Dispatch at http://127.0.0.1:1 is unreachable: ECONNREFUSED: ...'
# with DISPATCH_URL at an address that does not answer, 'HTTP_502: GET ... answered 502 Bad
# Gateway with a body that is not Dispatch's error JSON ...' against a server answering 502
# with an HTML page, and 'UNAUTHORIZED: invalid bearer token' with a bearer Dispatch does not
# hold, and at 6e885f7e each ended FAIL replay-command. The case plants one row of the case's
# own that runs n-1098-gaps-posted's command, a dispatch read piped into sed and grep, and puts
# first on PATH a dispatch of the case's own that prints the one line its directory holds and
# exits 1. With the unreachable line, the HTTP_502 line and the UNAUTHORIZED line, --replay must
# end exit 2, cannot run, naming the row, Dispatch and the line, with no FAIL line. The honest
# form is 'NOT_FOUND: not found', Dispatch's answer that it holds no such node: the run must
# fail replay-command, naming the row and the line.
DCLI_CASE="$WORK/dispatch-cli"
mkdir -p "$DCLI_CASE"
jq -c 'select(.id == "n-1098-gaps-posted") | {id: "f-dispatch-read", requirement: "r7", source: "the case", kind: "behaviour", claim: "The comment of INFRA-1098 names four gaps.", mark: "verified", evidence: {surface: "cli"}, command, expect, output: "", note: "planted by case 123"}' "$HERE/claims.jsonl" >"$DCLI_CASE/row"
[ -s "$DCLI_CASE/row" ] && grep -qF '"command":"dispatch read ' "$DCLI_CASE/row" || {
    printf 'FAIL n-1098-gaps-posted is no row of claims.jsonl that runs dispatch read, so case 123 has no command to plant\n'
    exit 1
}
for form in unreachable gateway credential not-found; do
    mkdir -p "$DCLI_CASE/$form/bin"
    cat >"$DCLI_CASE/$form/bin/dispatch" <<'STUB'
#!/bin/sh
cat ANSWER/line
exit 1
STUB
    sed -i "s|ANSWER|'$DCLI_CASE/$form'|" "$DCLI_CASE/$form/bin/dispatch"
    chmod +x "$DCLI_CASE/$form/bin/dispatch"
done
printf '%s\n' 'Dispatch at http://127.0.0.1:1 is unreachable: ECONNREFUSED: Unable to connect. Is the computer able to access the url?. If the Dispatch URL changed, restart this agent process so it picks up the new configuration.' >"$DCLI_CASE/unreachable/line"
printf '%s\n' 'HTTP_502: GET http://127.0.0.1:18765/api/v1/issues/INFRA-3134 answered 502 Bad Gateway with a body that is not Dispatch'"'"'s error JSON ("Bad Gateway"), which looks like a proxy or gateway page rather than Dispatch'"'"'s own answer, so a retry may succeed.' >"$DCLI_CASE/gateway/line"
printf '%s\n' 'UNAUTHORIZED: invalid bearer token' >"$DCLI_CASE/credential/line"
printf '%s\n' 'NOT_FOUND: not found' >"$DCLI_CASE/not-found/line"
for form in unreachable gateway credential; do
    case "$form" in
    unreachable)
        cli_case="a row whose dispatch command prints that Dispatch is unreachable cannot run, exit 2, naming the row, Dispatch and the line, and prints no FAIL"
        want="Dispatch gave no answer that judges f-dispatch-read's read .*Dispatch at http://127\\.0\\.0\\.1:1 is unreachable: "
        ;;
    gateway)
        cli_case="a row whose dispatch command prints HTTP_502, a gateway's server error, cannot run, exit 2, naming the row, Dispatch and the line, and prints no FAIL"
        want="Dispatch gave no answer that judges f-dispatch-read's read .*HTTP_502: GET "
        ;;
    credential)
        cli_case="a row whose dispatch command prints UNAUTHORIZED, Dispatch's answer about the reader's bearer, cannot run, exit 2, naming the row, Dispatch and the line, and prints no FAIL"
        want="Dispatch refused the credential of f-dispatch-read's read .*UNAUTHORIZED: invalid bearer token"
        ;;
    esac
    dir="$(fresh "dispatch-cli-$form")"
    cat "$DCLI_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    out="$(PATH="$DCLI_CASE/$form/bin:$PATH" fixture_run "$dir" --replay)"
    if grep -q '^FAIL' "$out/text"; then
        case_failed "$cli_case" "$(grep '^FAIL' "$out/text" | head -2 | cut -c1-300 | tr '\n' ' ')"
    else
        ending_result "$cli_case" 2 "^check\\.sh cannot run --replay now: $want" "$out"
    fi
done
dir="$(fresh dispatch-cli-not-found)"
cat "$DCLI_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
case_result "a row whose dispatch command prints NOT_FOUND, Dispatch's answer about the record, fails replay-command, naming the row and the line" \
    "replay-command: f-dispatch-read ran its command and a read inside it failed: .*NOT_FOUND: not found" "$(PATH="$DCLI_CASE/not-found/bin:$PATH" fixture_run "$dir" --replay)"

# 124. A row whose read of the Dispatch API got no answer, a server error or an answer about
# the reader's credential is no verdict on the deliverable, as case 123 holds for the dispatch
# command (the architect's rule of round 39 and his order of 2026-11-19T15:35Z). Of the 29 rows
# of claims.jsonl that read DISPATCH_URL, 25 curl it and 4 read it through python3's urllib.
# At 6e885f7e no curl of DISPATCH_URL ran with --fail-with-body, and measured in this pod on
# 2026-11-19 a plain curl of a server answering 502 with an HTML page exits 0 and hands the
# page to the row's pipeline as if Dispatch had answered. Each such curl now runs with
# --fail-with-body, which exits 22 on an HTTP error, and the gate reads that curl's URL once
# more itself with the Dispatch header file the curl sent (source_answer), as it reads a curl
# of GitHub's REST API again (case 119). The case plants one row of the case's own that runs
# ts-issue-events's command, a curl of the Dispatch API piped into jq, with DISPATCH_URL at an
# address of the case's own, http://dispatch.case124.invalid, and puts first on PATH a curl of
# the case's own that answers that address with the status, body and headers its directory
# holds, as case 119's answers GitHub's, recording what the gate's read sent. A 502 with an
# HTML page, and a 401 with the error JSON Dispatch answered a bearer it does not hold on
# 2026-11-19, {"code":"UNAUTHORIZED","error":"invalid bearer token"}, must each end exit 2,
# cannot run, naming the row, Dispatch and the status, with no FAIL line, after one read of
# the gate's own of the row's URL with the row's header file. The honest form is a 404 with the
# error JSON Dispatch answered GET /api/v1/issues/NO-1 on 2026-11-19,
# {"code":"NOT_FOUND","error":"not found"}: the run must fail replay-command, naming 404 and the
# message. A row whose python3 reads DISPATCH_URL is read by the last urllib error line of its
# output: the case plants a row whose python3 prints the traceback of a URLError, which urllib
# raises when it reaches no server, and exits 1, which must end exit 2, cannot run, naming the
# row, Dispatch and the line, with no FAIL line; and one whose traceback ends
# 'urllib.error.HTTPError: HTTP Error 404: Not Found', which must fail replay-command, naming
# 404 and the message.
DAPI_CASE="$WORK/dispatch-api"
DAPI_URL=http://dispatch.case124.invalid
mkdir -p "$DAPI_CASE"
jq -c 'select(.id == "ts-issue-events") | {id: "f-dispatch-api-read", requirement: "r7", source: "the case", kind: "behaviour", claim: "The issue holds 45 events up to 2026-11-12T07:00:00Z.", mark: "verified", evidence: {surface: "cli"}, command, expect, output: "", note: "planted by case 124"}' "$HERE/claims.jsonl" >"$DAPI_CASE/row"
[ -s "$DAPI_CASE/row" ] && grep -qF '"command":"curl -sS --fail-with-body -H @\"$IOC_DISPATCH_HEADER\" \"$DISPATCH_URL/api/v1/' "$DAPI_CASE/row" || {
    printf 'FAIL ts-issue-events is no row of claims.jsonl that curls the Dispatch API with --fail-with-body, so case 124 has no command to plant\n'
    exit 1
}
for answer in 502 401 404; do
    mkdir -p "$DAPI_CASE/$answer/bin"
    cat >"$DAPI_CASE/$answer/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" DISPATCH_ADDRESS/"*) ;;
*) exec REAL_CURL "$@" ;;
esac
body='' dump='' format='' previous=''
for word in "$@"; do
    case "$previous" in
    -o) body=$word ;;
    -D) dump=$word ;;
    -w) format=$word ;;
    esac
    previous=$word
done
status=$(cat ANSWER/status)
if [ -n "$body" ]; then
    printf '%s\n' "$*" >>ANSWER/reads
    cat ANSWER/body >"$body"
    [ -z "$dump" ] || cat ANSWER/headers >"$dump"
    [ -z "$format" ] || printf '%s' "$status"
    exit 0
fi
cat ANSWER/body
printf 'curl: (22) The requested URL returned error: %s\n' "$status" >&2
exit 22
STUB
    sed -i -e "s|REAL_CURL|'$(command -v curl)'|" -e "s|DISPATCH_ADDRESS|$DAPI_URL|" -e "s|ANSWER|'$DAPI_CASE/$answer'|g" "$DAPI_CASE/$answer/bin/curl"
    chmod +x "$DAPI_CASE/$answer/bin/curl"
    printf '%s\n' "$answer" >"$DAPI_CASE/$answer/status"
done
printf '<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n<body>\r\n<center><h1>502 Bad Gateway</h1></center>\r\n</body>\r\n</html>\r\n' >"$DAPI_CASE/502/body"
printf 'HTTP/1.1 502 Bad Gateway\r\ncontent-type: text/html\r\n\r\n' >"$DAPI_CASE/502/headers"
printf '{"code":"UNAUTHORIZED","error":"invalid bearer token"}\n' >"$DAPI_CASE/401/body"
printf 'HTTP/1.1 401 Unauthorized\r\ncontent-type: application/json\r\n\r\n' >"$DAPI_CASE/401/headers"
printf '{"code":"NOT_FOUND","error":"not found"}\n' >"$DAPI_CASE/404/body"
printf 'HTTP/1.1 404 Not Found\r\ncontent-type: application/json\r\n\r\n' >"$DAPI_CASE/404/headers"
for answer in 502 401; do
    case "$answer" in
    502)
        api_case="a row whose curl of the Dispatch API exits 22 and whose read again is answered 502 cannot run, exit 2, naming the row, Dispatch and the status, and prints no FAIL"
        want="Dispatch gave no answer that judges f-dispatch-api-read's read of its API, .*exited 22.*Dispatch answered 502, a server error"
        ;;
    401)
        api_case="a row whose curl of the Dispatch API exits 22 and whose read again is answered 401 UNAUTHORIZED cannot run, exit 2, naming the row, Dispatch and the status, and prints no FAIL"
        want="Dispatch refused the credential of f-dispatch-api-read's read of its API, .*exited 22.*Dispatch answered 401 UNAUTHORIZED: invalid bearer token"
        ;;
    esac
    dir="$(fresh "dispatch-api-$answer")"
    cat "$DAPI_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    out="$(DISPATCH_URL="$DAPI_URL" PATH="$DAPI_CASE/$answer/bin:$PATH" fixture_run "$dir" --replay)"
    reads="$(cat "$DAPI_CASE/$answer/reads" 2>/dev/null)"
    if grep -q '^FAIL' "$out/text"; then
        case_failed "$api_case" "$(grep '^FAIL' "$out/text" | head -2 | cut -c1-300 | tr '\n' ' ')"
    elif [ "$(printf '%s\n' "$reads" | grep -c .)" != 1 ] ||
        ! printf '%s\n' "$reads" | grep -qE -- '-H @[^ ]*/dispatch\.header http://dispatch\.case124\.invalid/api/v1/issues/INFRA-3134/events\?after=0&limit=200$'; then
        case_failed "$api_case" "the gate did not read the row's URL once with the row's header file; it read: $(printf '%s' "$reads" | head -3 | tr '\n' ' ')"
    else
        ending_result "$api_case" 2 "^check\\.sh cannot run --replay now: $want" "$out"
    fi
done
dir="$(fresh dispatch-api-404)"
cat "$DAPI_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
case_result "a row whose curl of the Dispatch API exits 22 and whose read again is answered 404 NOT_FOUND fails replay-command, naming the status and Dispatch's message" \
    "replay-command: f-dispatch-api-read ran its command and a read inside it failed: .*, and Dispatch answered 404 NOT_FOUND: not found, so its output" \
    "$(DISPATCH_URL="$DAPI_URL" PATH="$DAPI_CASE/404/bin:$PATH" fixture_run "$dir" --replay)"
cat >"$DAPI_CASE/urlerror.command" <<'CMD'
python3 -c 'import os,sys;os.environ.get("DISPATCH_URL");sys.exit("Traceback (most recent call last):\n  File \"<string>\", line 1, in <module>\nurllib.error.URLError: <urlopen error [Errno 111] Connection refused>")'
CMD
cat >"$DAPI_CASE/httperror.command" <<'CMD'
python3 -c 'import os,sys;os.environ.get("DISPATCH_URL");sys.exit("Traceback (most recent call last):\n  File \"<string>\", line 1, in <module>\nurllib.error.HTTPError: HTTP Error 404: Not Found")'
CMD
cat >"$DAPI_CASE/timeout.command" <<'CMD'
python3 -c 'import os,sys;os.environ.get("DISPATCH_URL");sys.exit("Traceback (most recent call last):\n  File \"<string>\", line 1, in <module>\nTimeoutError: The read operation timed out")'
CMD
for form in urlerror httperror timeout; do
    jq -cn --arg c "$(cat "$DAPI_CASE/$form.command")" '{id: "f-dispatch-python-read", requirement: "r7", source: "the case", kind: "behaviour", claim: "The script reads the feed of the window.", mark: "verified", evidence: {surface: "cli"}, command: $c, expect: {contains: "ran"}, output: "", note: "planted by case 124"}' >"$DAPI_CASE/$form.row"
done
dir="$(fresh dispatch-python-urlerror)"
cat "$DAPI_CASE/urlerror.row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(DISPATCH_URL="$DAPI_URL" fixture_run "$dir" --replay)"
python_case="a row whose python3 reads DISPATCH_URL and ends on urllib's URLError cannot run, exit 2, naming the row, Dispatch and the line, and prints no FAIL"
if grep -q '^FAIL' "$out/text"; then
    case_failed "$python_case" "$(grep '^FAIL' "$out/text" | head -2 | cut -c1-300 | tr '\n' ' ')"
else
    ending_result "$python_case" 2 '^check\.sh cannot run --replay now: Dispatch gave no answer that judges f-dispatch-python-read.*urllib\.error\.URLError: <urlopen error \[Errno 111\] Connection refused>' "$out"
fi
dir="$(fresh dispatch-python-httperror)"
cat "$DAPI_CASE/httperror.row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
case_result "a row whose python3 reads DISPATCH_URL and ends on urllib's HTTPError 404 fails replay-command, naming the status and the message" \
    "replay-command: f-dispatch-python-read ran its command and a read inside it failed: .*, and Dispatch answered 404: Not Found, so its output" \
    "$(DISPATCH_URL="$DAPI_URL" fixture_run "$dir" --replay)"
# A read that times out after the connection raises the socket's own TimeoutError, which urllib
# does not wrap in a URLError: no answer either.
dir="$(fresh dispatch-python-timeout)"
cat "$DAPI_CASE/timeout.row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(DISPATCH_URL="$DAPI_URL" fixture_run "$dir" --replay)"
timeout_case="a row whose python3 reads DISPATCH_URL and ends on an unwrapped TimeoutError cannot run, exit 2, naming the row, Dispatch and the line, and prints no FAIL"
if grep -q '^FAIL' "$out/text"; then
    case_failed "$timeout_case" "$(grep '^FAIL' "$out/text" | head -2 | cut -c1-300 | tr '\n' ' ')"
else
    ending_result "$timeout_case" 2 '^check\.sh cannot run --replay now: Dispatch gave no answer that judges f-dispatch-python-read.*TimeoutError: The read operation timed out' "$out"
fi

# 125. A row whose curl of GitHub's REST API exits 22 and whose read again is answered with a
# server error is no verdict on the deliverable (the architect's rule of round 39: a 5xx says
# the server failed, for GitHub as for Dispatch, and judges nothing of the record). At 6e885f7e
# such a row ended 'FAIL replay-command: ... exited 22, and GitHub answered 502'. The case
# plants case 119's row with a curl of the case's own first on PATH that answers the row's read
# and the gate's read again of api.github.com with 502 and an HTML page, as a gateway before
# GitHub does, and expects --replay to end exit 2, cannot run, naming the row, GitHub, the
# row's exit 22 and the 502, with no FAIL line.
GATEWAY_CASE="$WORK/github-gateway"
mkdir -p "$GATEWAY_CASE/bin"
cat >"$GATEWAY_CASE/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" https://api.github.com/"*) ;;
*) exec REAL_CURL "$@" ;;
esac
body='' dump='' format='' previous=''
for word in "$@"; do
    case "$previous" in
    -o) body=$word ;;
    -D) dump=$word ;;
    -w) format=$word ;;
    esac
    previous=$word
done
if [ -n "$body" ]; then
    cat ANSWER/body >"$body"
    [ -z "$dump" ] || cat ANSWER/headers >"$dump"
    [ -z "$format" ] || printf '502'
    exit 0
fi
cat ANSWER/body
printf 'curl: (22) The requested URL returned error: 502\n' >&2
exit 22
STUB
sed -i -e "s|REAL_CURL|'$(command -v curl)'|" -e "s|ANSWER|'$GATEWAY_CASE'|g" "$GATEWAY_CASE/bin/curl"
chmod +x "$GATEWAY_CASE/bin/curl"
printf '<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n<body>\r\n<center><h1>502 Bad Gateway</h1></center>\r\n</body>\r\n</html>\r\n' >"$GATEWAY_CASE/body"
printf 'HTTP/2 502\r\ncontent-type: text/html\r\n\r\n' >"$GATEWAY_CASE/headers"
dir="$(fresh github-gateway)"
cat "$ANSWER_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(PATH="$GATEWAY_CASE/bin:$ANSWER_CASE/bin:$PATH" fixture_run "$dir" --replay)"
gateway_case="a row whose curl of GitHub's REST API exits 22 and whose read again is answered 502 cannot run, exit 2, naming the row, GitHub and the status, and prints no FAIL"
if grep -q '^FAIL' "$out/text"; then
    case_failed "$gateway_case" "$(grep '^FAIL' "$out/text" | head -2 | cut -c1-300 | tr '\n' ' ')"
else
    ending_result "$gateway_case" 2 '^check\.sh cannot run --replay now: GitHub gave no answer that judges f-github-read.*exited 22.*GitHub answered 502, a server error' "$out"
fi

# 126. A row whose curl of GitHub's REST API exits 22 and whose read again is answered 404 Not
# Found fails replay-command (test round 39). The architect's rule of round 39 makes a 4xx that
# is not about the reader's credential, and is no rate limit, an answer about the record: GitHub
# holds no such pull request, review or commit, which is a verdict on the row. Cases 119, 121,
# 122 and 125 hold the endings that cannot run, and case 124 holds Dispatch's 404 as a FAIL, but
# no case held GitHub's, so a gate that read every GitHub 4xx as an outage passed them all. The
# case plants case 119's row with a curl of the case's own first on PATH that answers the row's
# read and the gate's read again of api.github.com with 404 and GitHub's error JSON, and expects
# --replay to fail replay-command, naming the row, its exit 22, the 404 and GitHub's message.
NOTFOUND_CASE="$WORK/github-not-found"
mkdir -p "$NOTFOUND_CASE/bin"
cat >"$NOTFOUND_CASE/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" https://api.github.com/"*) ;;
*) exec REAL_CURL "$@" ;;
esac
body='' dump='' format='' previous=''
for word in "$@"; do
    case "$previous" in
    -o) body=$word ;;
    -D) dump=$word ;;
    -w) format=$word ;;
    esac
    previous=$word
done
if [ -n "$body" ]; then
    cat ANSWER/body >"$body"
    [ -z "$dump" ] || cat ANSWER/headers >"$dump"
    [ -z "$format" ] || printf '404'
    exit 0
fi
cat ANSWER/body
printf 'curl: (22) The requested URL returned error: 404\n' >&2
exit 22
STUB
sed -i -e "s|REAL_CURL|'$(command -v curl)'|" -e "s|ANSWER|'$NOTFOUND_CASE'|g" "$NOTFOUND_CASE/bin/curl"
chmod +x "$NOTFOUND_CASE/bin/curl"
printf '{\n  "message": "Not Found",\n  "documentation_url": "https://docs.github.com/rest/pulls/reviews#list-reviews-for-a-pull-request",\n  "status": "404"\n}\n' >"$NOTFOUND_CASE/body"
printf 'HTTP/2 404\r\ncontent-type: application/json; charset=utf-8\r\nx-ratelimit-limit: 5000\r\nx-ratelimit-remaining: 4990\r\nx-ratelimit-resource: core\r\n\r\n' >"$NOTFOUND_CASE/headers"
dir="$(fresh github-not-found)"
cat "$ANSWER_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(PATH="$NOTFOUND_CASE/bin:$ANSWER_CASE/bin:$PATH" fixture_run "$dir" --replay)"
notfound_case="a row whose curl of GitHub's REST API exits 22 and whose read again is answered 404 Not Found fails replay-command, naming the row, its exit 22, the status and GitHub's message"
if [ "$(cat "$out/status" 2>/dev/null)" = 2 ]; then
    # A cannot-run ending is the defect this case pins, so it fails the case rather than
    # leaving it unrun: the gate took GitHub's answer about the record for an outage.
    case_failed "$notfound_case" "the gate read GitHub's 404 as no verdict: $(tail -1 "$out/text" | cut -c1-300)"
else
    case_result "$notfound_case" \
        "replay-command: f-github-read ran its command and a read inside it failed: .*exited 22, and GitHub answered 404: Not Found" "$out"
fi

# 127. A candidate page names one landing component (test round 39). R9's page carries "the
# landing component, which is Legion, Envoy or Dispatch", one of the three. At 672d71db --shape
# reads a page's first Landing component line alone (grep -m1), so a page whose first line
# names Legion and whose second names Envoy passed every assertion, although it names two
# components, as the single line 'Legion and Envoy' that case 120's rule refuses does. The case
# writes a second line naming Envoy under the first page of the fixture's judgement whose
# landing component is Legion, and runs --shape, which must fail, naming the page and its
# landing component.
dir="$(fresh landing-two-lines)"
sed -i '0,/^- \*\*Landing component:\*\* Legion$/s//- **Landing component:** Legion\n- **Landing component:** Envoy/' "$dir/judgement.md"
[ "$(grep -c '^- \*\*Landing component:\*\* Envoy$' "$dir/judgement.md")" -ge 2 ] || {
    printf 'FAIL the second landing component could not be planted in the fixture judgement\n'
    exit 1
}
case_result "a page with two Landing component lines naming two components fails --shape, naming the page" \
    'page-[a-z-]+: .*[Ll]anding component' "$(fixture_run "$dir" --shape)"

# 128. A candidate page carries one verdict (the architect's brief of round 40, on case 127's
# fault in the Verdict field). R9's page carries one verdict, one word of vendor, depend,
# port and reject. At 51c58606 --shape read a page's first Verdict line alone (grep -m1), for
# its verdict and for a second verdict word beside it, so a page whose first Verdict line
# says port and whose second says reject passed every assertion, although it carries two
# verdicts. The case writes a second line saying reject under the first Verdict line of the
# fixture's judgement that says port, and runs --shape, which must fail, naming the page and
# its verdict.
dir="$(fresh verdict-two-lines)"
sed -i '0,/^- \*\*Verdict:\*\* port$/s//- **Verdict:** port\n- **Verdict:** reject/' "$dir/judgement.md"
[ "$(grep -c '^- \*\*Verdict:\*\* reject$' "$dir/judgement.md")" -ge 2 ] || {
    printf 'FAIL the second verdict could not be planted in the fixture judgement\n'
    exit 1
}
case_result "a page with two Verdict lines naming two verdicts fails --shape, naming the page" \
    'page-[a-z-]+: .*[Vv]erdict' "$(fixture_run "$dir" --shape)"

# 129. A figure the judgement states beside a citation is one the rows it cites print (the
# architect's brief of round 40). A reader found, over six review rounds, sentences of the
# judgement whose figures no row they cite prints. At 51c58606 --citations read no sentence
# of the judgement, so nothing held a figure to the rows beside it. The case writes, after
# the fixture's sentence on the one thing to do first, a sentence that cites f-behaviour-1
# and f-figure-1 and states 12,618 rows where f-figure-1 prints 12617, and runs --citations,
# which must fail citation-figures, naming the figure and the rows; the honest form is the
# same sentence stating 12,617, which f-figure-1 prints without its thousands separator,
# and --citations passes it.
for figure in 12,618 12,617; do
    dir="$(fresh "figure-$figure")"
    sed -i "s/^The Envoy field\\.\$/The Envoy field. Its count reads $figure rows [f-behaviour-1, f-figure-1]./" "$dir/judgement.md"
    grep -qF "Its count reads $figure rows [f-behaviour-1, f-figure-1]." "$dir/judgement.md" || {
        printf 'FAIL the sentence stating %s could not be planted in the fixture judgement\n' "$figure"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    if [ "$figure" = 12,618 ]; then
        case_result "a sentence that cites rows and states a figure none of them prints fails --citations, naming the figure and the rows" \
            'citation-figures: line [0-9]+ of the judgement states 12,618 in a sentence that cites f-behaviour-1, f-figure-1' "$out"
    else
        passed_result "the same sentence stating the figure its row prints passes --citations" "$out"
    fi
done

# 130. A figure marked as an estimate is exempt by name (the architect's rule of round 40):
# an estimate is no measurement, and the judgement marks each one with "by our estimate".
# At 4ea5f7b1 citation-figures held every figure of a sentence that cites a row to that
# row, so the judgement's own Cost line "by our estimate, about 7 engineer-days ... against
# about 2 [m-eval-cost]" failed it. The case writes, after the fixture's sentence on the
# one thing to do first, a sentence that cites f-figure-1, which prints 12617 rows and no 9,
# and states about 9 engineer-days, in four forms, and runs --citations over each: with
# "by our estimate" in the figure's clause it passes citation-figures; with the words alone
# between commas after the figure's clause it passes too; with no marking it fails
# citation-figures, naming 9; and with the marked clause beside one that states 12,618 rows
# it fails citation-figures once, naming 12,618 and not 9, so the marking reaches no
# further than its clause.
for form in marked aside unmarked beside; do
    case "$form" in
    marked) sentence='It costs about 9 engineer-days by our estimate [f-figure-1].' ;;
    aside) sentence='It costs about 9 engineer-days, by our estimate, and its count reads 12,617 rows [f-figure-1].' ;;
    unmarked) sentence='It costs about 9 engineer-days [f-figure-1].' ;;
    beside) sentence='It costs about 9 engineer-days by our estimate, and its count reads 12,618 rows [f-figure-1].' ;;
    esac
    dir="$(fresh "estimate-$form")"
    sed -i "s/^The Envoy field\\.\$/The Envoy field. $sentence/" "$dir/judgement.md"
    grep -qxF "The Envoy field. $sentence" "$dir/judgement.md" || {
        printf 'FAIL the %s sentence could not be planted in the fixture judgement\n' "$form"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    marked) honest_result "a figure its clause marks by our estimate passes citation-figures, though no row it cites prints it" citation-figures "$out" ;;
    aside) honest_result "a figure whose clause the words by our estimate follow alone between commas passes citation-figures" citation-figures "$out" ;;
    unmarked)
        case_result "the same figure with no marking fails citation-figures, naming it" \
            'citation-figures: line [0-9]+ of the judgement states 9 in a sentence that cites f-figure-1' "$out"
        ;;
    beside)
        one_failure_result "a marked clause beside a clause stating a figure no cited row prints fails citation-figures once, naming that figure alone" citation-figures \
            '^1 of 6 assertions failed: citation-figures$' '^FAIL citation-figures: line [0-9]+ of the judgement states 12,618 in a sentence that cites f-figure-1' "$out"
        ;;
    esac
done

# 131. A candidate page carries one state of its step (the architect's brief of round 40):
# landed, with its commit and its proof; not made, with its refutation; or not landed, with
# the condition of R12 that stops it and its record. At cc62bd3b --landed read a page by its
# Landed line alone, so a page carrying a Not landed line beside its Landed and Proven by
# lines passed, although it says both that its step is landed and that it is not. The case
# writes a Not landed line under the fixture's landed page and runs --landed, which must fail
# landed-proof, naming the page and its two states.
dir="$(fresh landed-two-states)"
sed -i "s|^- \*\*Proven by:\*\* \`true\`\$|&\n- **Not landed:** needs a decision of the owner's ($NOT_LANDED_RECORD, cybermonklabs/legion#4544)|" "$dir/judgement.md"
[ "$(grep -c '^- \*\*Not landed:\*\* ' "$dir/judgement.md")" = 1 ] && grep -q '^- \*\*Landed:\*\* [0-9a-f]' "$dir/judgement.md" || {
    printf 'FAIL the Not landed line could not be planted beside the Landed line in the fixture judgement\n'
    exit 1
}
case_result "a page carrying a Landed line and a Not landed line fails --landed, naming the page and its two states" \
    'landed-proof: the page .An execution record under the gen_ai. names. carries two states' "$(fixture_run "$dir" --landed)"

# 132. A candidate page carries each line of its state once (the architect's brief of round
# 40). At cc62bd3b --landed read a page's commit and its proof command from its first Landed
# and its first Proven by line (grep -m1), so a page whose second Landed line names a commit
# of the base branch, or whose second Proven by line names a command that fails, passed with
# the first line read and the second never. The case writes each second line under the
# fixture's landed page, one run each, and runs --landed, which must fail landed-proof each
# time, naming the page.
for kind in Landed 'Proven by'; do
    dir="$(fresh "state-line-twice-${kind%% *}")"
    case "$kind" in
    Landed) second="- **Landed:** $(cat "$dir/base")" ;;
    *) second='- **Proven by:** `false`' ;;
    esac
    sed -i "s|^- \*\*Proven by:\*\* \`true\`\$|&\n$second|" "$dir/judgement.md"
    grep -qxF -- "$second" "$dir/judgement.md" || {
        printf 'FAIL the second %s line could not be planted in the fixture judgement\n' "$kind"
        exit 1
    }
    case_result "a page carrying its $kind line twice fails --landed, naming the page" \
        'landed-proof: the page .An execution record under the gen_ai. names. carries a line of its state more than once' "$(fixture_run "$dir" --landed)"
done

# 133. A landed list with no step is no pass of --landed, in any assertion over it (the
# architect's acceptance item of round 40: --landed "does not pass on an empty set in
# silence"). At cc62bd3b landed-proof failed an empty list, but landed-order judged the
# order of two commits it found by their subjects, whatever the list held, and passed. The
# case empties the fixture's landed list and takes the Landed and Proven by lines off its
# page, so neither the list nor the judgement names a step, and runs --landed, which must
# fail both landed-proof and landed-order, each naming the empty list.
dir="$(fresh landed-no-step)"
jq '.landed = []' "$dir/repo/docs/research/internet-of-cognition/sources.json" >"$dir/sources.json" &&
    mv "$dir/sources.json" "$dir/repo/docs/research/internet-of-cognition/sources.json"
sed -i '/^- \*\*Landed:\*\* /d; /^- \*\*Proven by:\*\* /d' "$dir/judgement.md"
[ "$(jq '.landed | length' "$dir/repo/docs/research/internet-of-cognition/sources.json")" = 0 ] && ! grep -qE '\*\*(Landed|Proven by):\*\*' "$dir/judgement.md" || {
    printf 'FAIL the landed list could not be emptied in the fixture\n'
    exit 1
}
out="$(fixture_run "$dir" --landed)"
no_step_case="a landed list with no step fails --landed, landed-proof and landed-order each naming the empty list"
if ! case_read "$no_step_case" "$out"; then
    :
elif grep -qE '^FAIL landed-proof: sources\.json.s landed list names no step' "$out/text" &&
    grep -qE '^FAIL landed-order: sources\.json.s landed list names no step' "$out/text"; then
    case_passed "$no_step_case"
else
    case_failed "$no_step_case" "$(grep -E '^(PASS|FAIL) landed-(proof|order)' "$out/text" | head -3 | cut -c1-300 | tr '\n' ' ')$RUN_LINE"
fi

# 134. A landed list whose every step is not landed passes --landed and says so in its own
# words (the architect's brief of round 40). After the forward merge 51c5860680 both steps
# of this tree's list are not landed, and a pass on them must name how many steps are
# landed and how many not, each step not landed with its condition, and that the order of
# the two steps is not judged for that reason. At cc62bd3b --landed read only pages with a
# Landed line, so it failed every step not landed as a page that lost its Landed line. The
# case commits the fixture's one step as not landed, writes its page's Not landed line with
# the condition and both records, and runs --landed, which must pass, its landed-proof line
# naming 0 landed and 1 not landed and the step with its condition, and its landed-order
# line naming the step and the order not judged.
dir="$(fresh landed-none-landed)"
not_landed_plant "$dir" "- **Not landed:** needs a decision of the owner's ($NOT_LANDED_RECORD, cybermonklabs/legion#4544)" || {
    printf 'FAIL the step could not be planted as not landed in the fixture\n'
    exit 1
}
out="$(fixture_run "$dir" --landed)"
none_landed_case="a landed list whose every step is not landed passes --landed, naming the counts, each step with its condition, and the order not judged"
if ! case_read "$none_landed_case" "$out"; then
    :
elif [ "$RUN_ENDING" = passed ] &&
    grep -qE "^PASS landed-proof: the steps of sources\\.json.s landed list, 1 in all, are 0 landed and 1 not landed, .*: .An execution record under the gen_ai\\. names., not landed because it needs a decision of the owner.s\$" "$out/text" &&
    grep -qE "^PASS landed-order: .* is not judged, because .*: .An execution record under the gen_ai\\. names.\$" "$out/text"; then
    case_passed "$none_landed_case"
else
    case_failed "$none_landed_case" "$(grep -E '^(PASS|FAIL) landed-(proof|order)' "$out/text" | head -3 | cut -c1-400 | tr '\n' ' ')$RUN_LINE"
fi

# 135. A test against our own records names one input, and it is a record of ours (test
# round 40, case 127's class in another field reader). R7 gives each candidate one test
# "against our own records, never against the project's own fixtures", and our-data-input
# holds each of tests/concord.md, scope.md and mas-lab.md to an Input line naming a Dispatch
# key or a commit of ours and no fixture. At a4e9fb5a it reads a file's first Input line alone
# (grep -m1 '^Input:'), so a file whose first Input line names dispatch://INFRA-3134 and whose
# second names the project's tests/fixtures/golden-transcript.json passed, although the test
# names a fixture of the project as an input. The case writes that second line under the
# fixture's tests/concord.md and runs --replay, which must fail our-data-input, naming the file.
dir="$(fresh input-two-lines)"
sed -i '0,/^Input: dispatch:\/\/INFRA-3134$/s//Input: dispatch:\/\/INFRA-3134\nInput: tests\/fixtures\/golden-transcript.json/' "$dir/repo/docs/research/internet-of-cognition/tests/concord.md"
[ "$(grep -c '^Input:' "$dir/repo/docs/research/internet-of-cognition/tests/concord.md")" -eq 2 ] || {
    printf 'FAIL the second Input line could not be planted in the fixture tests/concord.md\n'
    exit 1
}
case_result "a test file whose second Input line names a project fixture fails --replay, naming the file" \
    'our-data-input: concord\.md' "$(fixture_run "$dir" --replay)"

# 136. A page whose first step is one engineer-day or less states whether it is landed (test
# round 40). The reading of R12 and of c-landed-steps recorded beside the spec
# (dispatch://INFRA-3134/comment/de485c44-6b40-4018-8ac5-9cde3e5cdd65) holds each page whose
# first step is one engineer-day or less to "landed" with its commit and its proof, or "not
# landed" with the R12 condition that stops it and its record. At a4e9fb5a --landed holds only
# the steps sources.json's landed list names, so a page whose Cost is 1 engineer-day and that
# carries no Landed, Refuted by or Not landed line passed every assertion (judgement v17 with
# the refusal page's cost written 'about 1 engineer-day' passed all 5). The case writes the
# fixture's Envoy page's Cost as 1 engineer-day, leaves the page with no state line, and runs
# --landed, which must fail, naming the page.
dir="$(fresh one-day-no-state)"
sed -i '0,/^- \*\*Cost:\*\* 2 engineer-days$/s//- **Cost:** 1 engineer-day/' "$dir/judgement.md"
awk '/^### Structured metadata on an Envoy message/{f=1; next} /^### /{f=0} f && /\*\*(Landed|Not landed|Refuted by|Proven by):\*\*/{n++} f && /^- \*\*Cost:\*\* 1 engineer-day$/{c++} END{exit !(c == 1 && n == 0)}' "$dir/judgement.md" || {
    printf 'FAIL the fixture Envoy page could not be given a one-day first step with no state line\n'
    exit 1
}
case_result "a page whose first step is 1 engineer-day and that states no landing fails --landed, naming the page" \
    "landed-[a-z-]+: .*Structured metadata on an Envoy message" "$(fixture_run "$dir" --landed)"

# 137. A landed list names each step once (the architect's order of round 41, on the readers
# that take a first match). Until round 41 mode_landed read the entry for a page's title with
# jq select(.title == $t) | head -1, so a list that named one title twice held the page to
# its first entry alone: the second entry's commit and proof command were never compared
# with the page, and the list's own step loop passed it, since the page carries the Landed
# line and the sentence it published stands in the tree. The case appends to the fixture's
# landed list a second entry of its one step's title, landed, with the base commit and the
# proof `false`, and runs --landed, which must fail landed-proof, naming the title and that
# the list names it twice.
dir="$(fresh landed-title-twice)"
sources="$dir/repo/docs/research/internet-of-cognition/sources.json"
jq --arg base "$(cat "$dir/base")" '.landed += [.landed[0] | .commit = $base | .command = "false"]' "$sources" >"$dir/sources.json" &&
    mv "$dir/sources.json" "$sources"
[ "$(jq '[.landed[] | select(.title == "An execution record under the gen_ai. names")] | length' "$sources")" = 2 ] || {
    printf 'FAIL the second entry of the step could not be planted in the fixture landed list\n'
    exit 1
}
case_result "a landed list naming one step's title twice fails --landed, naming the title" \
    "landed-proof: sources\\.json.s landed list names the step .An execution record under the gen_ai\\. names. 2 times" "$(fixture_run "$dir" --landed)"

# 138. A question of mas-lab is answered once, in one evidence file (the architect's order of
# round 41: a second of what a file's shape allows one of is a defect of the file, refused by
# name, and no reader learns to read several). Until round 41 mas-lab-question read only the
# first file grep -rl found holding a question (head -1), in the order the file system lists
# the directory, so a second file answering the same question was never read. The case
# appends to the fixture's library-standard.md, which sorts after ctl.md, the file that
# answers Question 1, a second answer to Question 1, which cites the row ctl.md's does, so
# that only the second answer itself is the fault, and runs --replay, which must fail
# mas-lab-question, naming the question and both files.
dir="$(fresh mas-lab-second-answer)"
printf '\n## Question 1: can its specification format express Legion%ss phase workflow?\n\nYes, by a reading of its schema [f-project-1].\n' "'" \
    >>"$dir/repo/docs/research/internet-of-cognition/mas-lab/library-standard.md"
[ "$(grep -rlF "Question 1: can its specification format express Legion's phase workflow?" "$dir/repo/docs/research/internet-of-cognition/mas-lab" | wc -l)" = 2 ] || {
    printf 'FAIL the second answer to Question 1 could not be planted in the fixture mas-lab/library-standard.md\n'
    exit 1
}
case_result "a question of mas-lab that a second file answers fails --replay, naming the question and both files" \
    "mas-lab-question: .Question 1: can its specification format express Legion.s phase workflow\\?. stands on 2 lines of the evidence files of mas-lab, ctl\\.md \\(1\\), library-standard\\.md \\(1\\)" "$(fixture_run "$dir" --replay)"

# 139. A line range a citation names is held to its file by both its ends, a bound (the
# architect's order of round 41). Until round 41 resolve_symbol read the first number of a
# symbol's line range alone (head -1), so 'handoff.go:3-9' resolved in a file of three
# lines, and so did a range whose end stands before its start. The case writes the symbol
# of the fixture's row f-our-1, which cites packages/daemon/internal/api/handoff.go, a file
# of three lines, in three forms and runs --citations over each: 3-9, which runs past the
# file's end, must fail citation-our-code, naming the row, the symbol and the file's length;
# 3-1 must fail it, naming the range as one that ends before it starts; and 1-3, the honest
# form, passes it.
for form in past-end reversed honest; do
    case "$form" in
    past-end) symbol='handoff.go:3-9' ;;
    reversed) symbol='handoff.go:3-1' ;;
    honest) symbol='handoff.go:1-3' ;;
    esac
    dir="$(fresh "line-range-$form")"
    claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    sed -i "/\"id\":\"f-our-1\"/s/\"symbol\":\"TesterVerdictRequired\"/\"symbol\":\"$symbol\"/" "$claims_copy"
    grep -F '"id":"f-our-1"' "$claims_copy" | grep -qF "\"symbol\":\"$symbol\"" || {
        printf 'FAIL the %s range could not be planted in the fixture row f-our-1\n' "$form"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    past-end)
        case_result "a line range that runs past its file's end fails --citations, naming the row, the range and the file's length" \
            "citation-our-code: f-our-1 cites handoff\\.go:3-9, and packages/daemon/internal/api/handoff\\.go holds 3 lines" "$out"
        ;;
    reversed)
        case_result "a line range whose end stands before its start fails --citations, naming the row and the range" \
            "citation-our-code: f-our-1 cites handoff\\.go:3-1, a line range that ends before it starts" "$out"
        ;;
    honest) honest_result "a line range inside its file passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 140. The order of the two steps is judged on one commit of each (the architect's order of
# round 41). Until round 41 landed_commit_of took the newest commit of the pull request whose
# subject carries the phrase it reads (grep -m1), so with two commits of the shortfall rows'
# subject the order was judged on one of them and the other was never read: a first rows
# commit made after the combining rule would stand unjudged beside a later one. The case
# writes into the fixture's history, whose one landed step leaves the order judged, a second
# commit with the shortfall rows' subject between the first and the combining rule's commit,
# so either rows commit is an ancestor of the rule's, and runs --landed, which must fail
# landed-order, naming the two commits, rather than judge the order on a guess.
dir="$(fresh rows-subject-twice)"
(
    cd "$dir/repo" || exit 1
    rule="$(git log --format=%H -1 --grep='combines the reviewer')"
    head="$(git rev-parse HEAD)"
    git checkout --quiet --detach "$rule^" &&
        git commit --quiet --allow-empty -m "feat(skills): each review round's two graders are recorded as two rows, not one verdict" &&
        git cherry-pick --allow-empty "$rule" "$head" >/dev/null &&
        git branch -f main HEAD && git checkout --quiet main
) || {
    printf 'FAIL the second commit of the shortfall rows could not be planted in the fixture history\n'
    exit 1
}
[ "$(git -C "$dir/repo" log --format=%s | grep -c 'two graders are recorded as two rows')" = 2 ] &&
    [ -z "$(git -C "$dir/repo" status --porcelain)" ] || {
    printf 'FAIL the fixture history does not hold two commits of the shortfall rows with a clean tree\n'
    exit 1
}
case_result "two commits of the pull request carrying the shortfall rows' subject fail --landed, naming both" \
    "landed-order: 2 commits of this pull request carry the subject of the shortfall rows" "$(fixture_run "$dir" --landed)"

# 141. A page gives its first step one cost, on one Cost line, in --landed too (the
# architect's rule of round 41: a second of what a page allows one of is a defect of the
# page, refused by name, and no reader learns to read several). As round 41 first wrote it,
# --landed read every Cost line of a page and held the page to a state when any of them
# was one engineer-day or less, so a page with two Cost lines, each above a day, passed
# --landed with neither refused. The case writes a second Cost line, 3 engineer-days, under
# the fixture's Envoy page, whose Cost is 2 engineer-days and which carries no state, and
# runs --landed, which must fail landed-proof, naming the page and its two Cost lines.
dir="$(fresh cost-twice)"
sed -i '0,/^- \*\*Cost:\*\* 2 engineer-days$/s//- **Cost:** 2 engineer-days\n- **Cost:** 3 engineer-days/' "$dir/judgement.md"
awk '/^### Structured metadata on an Envoy message/{f=1; next} /^### /{f=0} f && /\*\*(Landed|Not landed|Refuted by|Proven by):\*\*/{n++} f && /^- \*\*Cost:\*\* /{c++} END{exit !(c == 2 && n == 0)}' "$dir/judgement.md" || {
    printf 'FAIL the second Cost line could not be planted on the fixture Envoy page\n'
    exit 1
}
case_result "a page carrying its Cost on two lines fails --landed, naming the page" \
    "landed-proof: the page .Structured metadata on an Envoy message. carries its Cost on 2 lines" "$(fixture_run "$dir" --landed)"

# 142. A page whose first step is one engineer-day or less states its state, whatever way
# its Cost writes the figure (test round 41). Case 136 made --landed read each page's Cost,
# but only a figure in digits before "engineer-day": a Cost of "about one engineer-day" or
# "half an engineer-day", which the judgement's own ranking writes costs like ("About three
# engineer-days by our estimate"), reads as no cost, so such a page with no state line passed
# every assertion, the PASS line saying only that one page "give no cost in engineer-days
# written in digits, which is not read". Judgement v19 with the refusal page's Cost written
# 'about one engineer-day, by our estimate' passed all 5. An author can write that by
# accident. The case writes the fixture's Envoy page's Cost as "about one engineer-day",
# leaves the page with no state line, and runs --landed, which must fail, naming the page,
# whether it reads the words or refuses a Cost it cannot read.
dir="$(fresh one-day-in-words)"
sed -i '0,/^- \*\*Cost:\*\* 2 engineer-days$/s//- **Cost:** about one engineer-day/' "$dir/judgement.md"
awk '/^### Structured metadata on an Envoy message/{f=1; next} /^### /{f=0} f && /\*\*(Landed|Not landed|Refuted by|Proven by):\*\*/{n++} f && /^- \*\*Cost:\*\* about one engineer-day$/{c++} END{exit !(c == 1 && n == 0)}' "$dir/judgement.md" || {
    printf 'FAIL the fixture Envoy page could not be given a cost of one engineer-day in words with no state line\n'
    exit 1
}
case_result "a page whose Cost is about one engineer-day in words and that states no landing fails --landed, naming the page" \
    "landed-[a-z-]+: .*Structured metadata on an Envoy message" "$(fixture_run "$dir" --landed)"

# 143. A page gives its first step a cost in engineer-days (test round 42). R9 gives each
# page "the cost in engineer-days", and case 142's fix reads one written in digits or in
# words, and fails a Cost line that names engineer-days with a figure it cannot read. A Cost
# line that names no engineer-days at all, as "about half a day" or "about 4 hours", reads as
# no cost: --landed passed such a page with no state line, its PASS line saying "1 give their
# cost in no engineer-days on their Cost line", and --shape passed it too (judgement v22 with
# the refusal page's Cost written 'about half a day, by our estimate' passed both). An author
# can write that by accident, and the page then holds neither R9's cost nor, for a step of
# half a day, the state R12 asks of it. The case writes the fixture's Envoy page's Cost as
# "about half a day", leaves the page with no state line, and runs --shape and then --landed:
# one of them must fail, naming the page; each run is read by its record (case_read) before
# its text.
dir="$(fresh cost-in-no-engineer-days)"
sed -i '0,/^- \*\*Cost:\*\* 2 engineer-days$/s//- **Cost:** about half a day/' "$dir/judgement.md"
awk '/^### Structured metadata on an Envoy message/{f=1; next} /^### /{f=0} f && /\*\*(Landed|Not landed|Refuted by|Proven by):\*\*/{n++} f && /^- \*\*Cost:\*\* about half a day$/{c++} END{exit !(c == 1 && n == 0)}' "$dir/judgement.md" || {
    printf 'FAIL the fixture Envoy page could not be given a cost in no engineer-days with no state line\n'
    exit 1
}
no_unit_case="a page whose Cost names no engineer-days fails --shape or --landed, naming the page"
shape_out="$(fixture_run "$dir" --shape)"
landed_out="$(fixture_run "$dir" --landed)"
if ! case_read "$no_unit_case" "$shape_out"; then
    :
elif shape_line="$RUN_LINE" && ! case_read "$no_unit_case" "$landed_out"; then
    :
elif grep -qE '^FAIL (page-[a-z-]+|landed-[a-z-]+): .*Structured metadata on an Envoy message' "$shape_out/text" "$landed_out/text"; then
    case_passed "$no_unit_case"
else
    case_failed "$no_unit_case" "no FAIL of either run names the page: --shape $shape_line, --landed $RUN_LINE"
fi

# 144. A step another pull request of this tree made and main merged stands in its commit
# (the architect's brief of round 43, on
# dispatch://INFRA-3134/comment/0c3c9fb7-7127-43c6-a37d-1b52be4e9760: a landed step names a
# commit of this pull request or the merge commit on main of another pull request of this
# tree). R12's two steps were made in cybermonklabs/legion#4636 and merged as fcc746a3,
# which this pull request's history does not hold, so until round 43 --landed
# read a landed step only as a commit this pull request added, with its sentences in this
# tree. A step that carries merged_in is held instead to its merge commit, fetched from
# our_repository.url into the run's scratch, with each sentence it published in its file at
# that commit. The case makes the fixture's landed list R12's two steps as fcc746a3 made them
# (check.sh's fixture_merged), their commit on a branch beside the fixture's own line, and
# runs --landed twice: with the shortfall rows' first sentence written in sources.json as
# one the commit's pr-body.md does not carry, which must fail landed-proof, naming the step,
# the sentence and the commit; and, the honest form, as fixture_merged plants it, which
# must pass every assertion.
for form in absent honest; do
    dir="$(fresh "merged-$form")"
    merged="$(fixture_merged "$dir" "docs(skills): the reviewer pair is two rows (#4636)")" || {
        printf 'FAIL the merged-in steps could not be planted in the fixture (%s)\n' "$form"
        exit 1
    }
    if [ "$form" = absent ]; then
        sources="$dir/repo/docs/research/internet-of-cognition/sources.json"
        jq '(.landed[] | select(.title == "An execution record under the gen_ai. names") | .published[0].sentence) = "a sentence the merge never wrote"' \
            "$sources" >"$dir/sources.json" && mv "$dir/sources.json" "$sources" || {
            printf 'FAIL the absent sentence could not be planted in the fixture sources.json\n'
            exit 1
        }
    fi
    out="$(fixture_run "$dir" --landed)"
    case "$form" in
    absent)
        case_result "a merged-in step whose published sentence its commit does not carry fails --landed, naming the sentence and the commit" \
            "landed-proof: the landed step .An execution record under the gen_ai\\. names. published .a sentence the merge never wrote. in skills/legion-worker/references/pr-body\\.md, and skills/legion-worker/references/pr-body\\.md at $merged, which cybermonklabs/legion#4636 merged, does not carry it" "$out"
        ;;
    honest) passed_result "merged-in steps whose commit carries every sentence they publish, each named on its page with its pull request and held by its proof, pass --landed" "$out" ;;
    esac
done

# 145. The merge commit of a merged-in step is the one its pull request made (the architect's
# brief of round 43). A squash merge on main writes the pull request's number at the end of
# its subject, "(#4636)", a fixed fact of an immutable commit, so a step that says
# cybermonklabs/legion#4636 merged it is held to a commit whose subject ends "(#4636)". The
# case plants the merged-in steps of case 144's honest form with a commit whose subject ends
# "(#4637)", and runs --landed, which must fail landed-proof, naming the commit's subject and
# the pull request sources.json names.
dir="$(fresh merged-other-pr)"
fixture_merged "$dir" "docs(skills): the reviewer pair is two rows (#4637)" >/dev/null || {
    printf 'FAIL the merged-in steps could not be planted in the fixture with another pull request in their subject\n'
    exit 1
}
case_result "a merged-in step whose commit's subject names another pull request fails --landed, naming both" \
    "landed-proof: the landed step .An execution record under the gen_ai\\. names. commits [0-9a-f]{40}, whose subject .docs\\(skills\\): the reviewer pair is two rows \\(#4637\\). does not end .\\(#4636\\)., and sources\\.json says cybermonklabs/legion#4636 merged it" \
    "$(fixture_run "$dir" --landed)"

# 146. A merged-in step's commit that the fetch cannot reach leaves --landed unable to run (the
# architect's brief of round 43: the fetch is a live read, and a live read that gets no answer
# is a mode that cannot run, never a verdict on the deliverable). The case plants the
# merged-in steps of case 144's honest form, then writes into sources.json and onto the pages
# a commit the fixture's repository does not hold, and runs --landed, which must end exit 2,
# cannot run, naming the fetch of that commit.
UNREACHABLE=0123456789abcdef0123456789abcdef01234567
dir="$(fresh merged-unreachable)"
merged="$(fixture_merged "$dir" "docs(skills): the reviewer pair is two rows (#4636)")" &&
    sed -i "s/$merged/$UNREACHABLE/g" "$dir/judgement.md" "$dir/repo/docs/research/internet-of-cognition/sources.json" &&
    grep -qF "$UNREACHABLE" "$dir/judgement.md" && [ "$(jq -r '.landed[0].commit' "$dir/repo/docs/research/internet-of-cognition/sources.json")" = "$UNREACHABLE" ] &&
    ! git -C "$dir/repo" cat-file -e "$UNREACHABLE" 2>/dev/null || {
    printf 'FAIL the merged-in steps could not be planted in the fixture with a commit its repository does not hold\n'
    exit 1
}
ending_result "a merged-in step whose commit the fetch cannot reach makes --landed cannot run, naming the fetch" 2 \
    "^check\\.sh cannot run --landed: .*the fetch of $UNREACHABLE, .*did not answer" "$(fixture_run "$dir" --landed)"

# 147. A merged-in step's published list holds every line its merge commit added to the files
# it names (the architect's brief of round 44). Until round 44 --landed read that each
# published sentence stands in its file at the commit and ran the page's proof there, and
# read nothing of what the commit added beside them, so the tester cut pr-body.md's sentence
# "Where the diff the round reads touches no runtime code, each row reads ..." from a copy
# of fcc746a3, which neither step's list named, and both proofs still passed. The case plants
# R12's two steps as fcc746a3 made them (fixture_merged), takes that not-run sentence out of
# the combining rule's published list, on the fixture's page 'One definitions record', and
# writes its proof again from the list, so list and proof agree, and runs --landed, which must
# fail landed-coverage, naming the line the commit added to pr-body.md that no list holds.
dir="$(fresh merged-list-short)"
fixture_merged "$dir" "docs(skills): the reviewer pair is two rows (#4636)" >/dev/null &&
    sources="$dir/repo/docs/research/internet-of-cognition/sources.json" &&
    jq '(.landed[] | select(.title == "One definitions record") | .published) |= map(select(.sentence | startswith("Where the diff the round reads") | not))' \
        "$sources" >"$dir/sources.json" && mv "$dir/sources.json" "$sources" &&
    [ "$(jq '[.landed[].published[] | select(.sentence | startswith("Where the diff the round reads"))] | length' "$sources")" = 0 ] &&
    fixture_merged_prove "$dir" "One definitions record" || {
    printf 'FAIL the not-run sentence could not be taken out of the fixture combining rule'"'"'s list\n'
    exit 1
}
case_result "a merged-in step's published list that leaves out a sentence its commit added fails --landed, naming the line" \
    "landed-coverage: the line .Where the diff the round reads touches no runtime code, each row reads .not run \\(<the whole change .*, which [0-9a-f]{40} added to skills/legion-worker/references/pr-body\\.md, is not covered" \
    "$(fixture_run "$dir" --landed)"

# 148. The proof of a merged-in step holds every sentence the step publishes (the architect's
# brief of round 44). A proof that greps for less than its step publishes still passes with a
# sentence gone from the commit. The case plants R12's two steps as fcc746a3 made them, the
# combining rule's published list carrying the not-run sentence, and writes the combining
# rule's proof, on its page and in sources.json alike, without the grep of that sentence, and
# runs --landed, which must fail landed-proof-holds, naming the step and the sentence.
dir="$(fresh merged-proof-short)"
fixture_merged "$dir" "docs(skills): the reviewer pair is two rows (#4636)" >/dev/null &&
    fixture_merged_prove "$dir" "One definitions record" "$(fixture_merged_steps | jq -r '.[1].published[] | select(.sentence | startswith("Where the diff the round reads")) | .sentence')" &&
    ! grep -qF 'Where the diff the round reads' <(jq -r '.landed[] | select(.title == "One definitions record") | .command' "$dir/repo/docs/research/internet-of-cognition/sources.json") || {
    printf 'FAIL the combining rule'"'"'s proof could not be written without the not-run sentence in the fixture\n'
    exit 1
}
case_result "a merged-in step whose proof does not grep for a sentence the step publishes fails --landed, naming it" \
    "landed-proof-holds: the proof the page of the merged-in step .One definitions record. writes does not hold the sentence .Where the diff the round reads touches no runtime code, each row reads .not run \\(<the whole change" \
    "$(fixture_run "$dir" --landed)"

# 149. A row whose own curl of GitHub's REST API was answered with a server error is no verdict
# on the deliverable, whatever the gate's read of it again gets (the architect's rule of round
# 45, on item 1 of review 6003758567: the status the row got decides, and the gate's read
# again only where the row's record holds none). At b1b431ca the gate decided a curl that
# exited 22 by its own read of the URL again alone, so a row answered 503 whose read again was
# answered 200 ended exit 1, 'FAIL replay-command: f-github-read ran its command and a read
# inside it failed: ... exited 22, and GitHub answered 200', though the row's output held
# curl's line 'curl: (22) The requested URL returned error: 503'. curl's function now keeps
# that line in the row's record. The case plants case 119's row with a curl of the case's own
# first on PATH that answers the row's read with a gateway's page, that line on stderr and
# exit 22, and the gate's read with -o, as case 122's does, with a JSON body and 200, and
# runs --replay, which must end exit 2, cannot run, naming the row's 503 as a server error
# and the 200 beside it, with no FAIL.
SERVER_CASE="$WORK/github-row-server-error"
mkdir -p "$SERVER_CASE/bin"
cat >"$SERVER_CASE/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" https://api.github.com/"*) ;;
*) exec REAL_CURL "$@" ;;
esac
body='' dump='' format='' previous=''
for word in "$@"; do
    case "$previous" in
    -o) body=$word ;;
    -D) dump=$word ;;
    -w) format=$word ;;
    esac
    previous=$word
done
if [ -n "$body" ]; then
    printf '[{"id": 5854791039, "body": "planted by case 149"}]\n' >"$body"
    [ -z "$dump" ] || printf 'HTTP/2 200\r\ncontent-type: application/json; charset=utf-8\r\n\r\n' >"$dump"
    [ -z "$format" ] || printf '200'
    exit 0
fi
printf '<html><body><h1>503 Service Unavailable</h1></body></html>\n'
printf 'curl: (22) The requested URL returned error: 503\n' >&2
exit 22
STUB
sed -i "s|REAL_CURL|'$(command -v curl)'|" "$SERVER_CASE/bin/curl"
chmod +x "$SERVER_CASE/bin/curl"
dir="$(fresh github-row-server-error)"
cat "$ANSWER_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
ending_result "a row whose own curl of GitHub's REST API was answered 503 cannot run, exit 2, naming the 503 as a server error and the 200 the gate's read again got, and fails no assertion" 2 \
    "^check\\.sh cannot run --replay now: GitHub gave no answer that judges f-github-read's read of its REST API, .*exited 22, since GitHub answered 503, a server error, and when the gate read it again GitHub answered 200\$" \
    "$(PATH="$SERVER_CASE/bin:$ANSWER_CASE/bin:$PATH" fixture_run "$dir" --replay)"

# 150. A read inside a script a row runs is a row's read too (the architect's rule of round 45,
# on item 2 of review 6003758567). tc-rng-2 runs evidence/concord-build-run.sh with sh, and the
# script reads GitHub's REST API twice under set -e, each read a curl piped into jq and grep.
# At b1b431ca a read that got no answer left the grep nothing to match, so the script exited 1,
# the row's record held only that sh exited 1, and source_answer, which read no line of sh,
# ended the run exit 1, 'FAIL replay-command: f-concord-script ran its command and a read inside
# it failed: 'sh .../concord-build-run.sh' exited 1'. The script now writes into the row's
# record the line curl's function writes for a row's own curl, and ends with curl's status.
# The case copies the script into the fixture's evidence and plants a row that runs tc-rng-2's
# command, with stubs of the case's own for what the script runs before its reads (uv, the
# venv's python and the files of the clone it reads with sed) and a curl first on PATH that
# cannot connect to api.github.com, and runs --replay, which must end exit 2, cannot run,
# naming the row and the curl's exit 7, with no FAIL; and the script run by hand under the
# same stubs must end with curl's 7, not 1.
SCRIPT_CASE="$WORK/script-read"
SCRIPT_CLONE="$SCRIPT_CASE/clones/cognition-protocols"
mkdir -p "$SCRIPT_CASE/bin" "$SCRIPT_CASE/work/venv/bin" "$SCRIPT_CLONE/packages/ioc-concord/src/ioc_concord" "$SCRIPT_CLONE/protocols/concord/spec"
: >"$SCRIPT_CLONE/packages/ioc-concord/src/ioc_concord/wire.py"
: >"$SCRIPT_CLONE/packages/ioc-concord/src/ioc_concord/engine.py"
: >"$SCRIPT_CLONE/protocols/concord/spec/CONCORD-SPEC.md"
: >"$SCRIPT_CASE/work/reviews.jsonl"
printf '#!/bin/sh\nexit 0\n' >"$SCRIPT_CASE/bin/uv"
printf '#!/bin/sh\n[ "$1" != - ] || cat >/dev/null\nexit 0\n' >"$SCRIPT_CASE/work/venv/bin/python"
cat >"$SCRIPT_CASE/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" https://api.github.com/"*)
    printf 'curl: (7) Failed to connect to api.github.com port 443 after 0 ms: Could not connect to server\n' >&2
    exit 7
    ;;
esac
exec REAL_CURL "$@"
STUB
sed -i "s|REAL_CURL|'$(command -v curl)'|" "$SCRIPT_CASE/bin/curl"
chmod +x "$SCRIPT_CASE/bin/uv" "$SCRIPT_CASE/bin/curl" "$SCRIPT_CASE/work/venv/bin/python"
jq -c 'select(.id == "tc-rng-2") | {id: "f-concord-script", requirement: "r7", source: "the case", kind: "behaviour", claim: "The library accepts a raw score without complaint.", mark: "verified", evidence: {surface: "cli"}, command, expect, output: "", note: "planted by case 150"}' "$HERE/claims.jsonl" >"$SCRIPT_CASE/row"
[ -s "$SCRIPT_CASE/row" ] && grep -qF 'sh \"$IOC_EVID/concord-build-run.sh\"' "$SCRIPT_CASE/row" || {
    printf 'FAIL tc-rng-2 is no row of claims.jsonl that runs evidence/concord-build-run.sh with sh, so case 150 has no command to plant\n'
    exit 1
}
dir="$(fresh script-read)"
SCRIPT_EVID="$dir/repo/docs/research/internet-of-cognition/evidence"
cp "$HERE/evidence/concord-build-run.sh" "$SCRIPT_EVID/"
cat "$SCRIPT_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(IOC_EVID="$SCRIPT_EVID" IOC_TC_WORK="$SCRIPT_CASE/work" IOC_CLONES="$SCRIPT_CASE/clones" PATH="$SCRIPT_CASE/bin:$PATH" fixture_run "$dir" --replay)"
script_status="$(cd "$SCRIPT_CASE/work" && IOC_CLONES="$SCRIPT_CASE/clones" PATH="$SCRIPT_CASE/bin:$PATH" sh "$SCRIPT_EVID/concord-build-run.sh" >/dev/null 2>&1; printf '%s' "$?")"
script_case="a read of GitHub inside the script tc-rng-2 runs that reaches no GitHub cannot run, exit 2, naming the row and the curl's exit, fails no assertion, and the script ends with curl's status"
script_want="^check\\.sh cannot run --replay now: GitHub gave no answer that judges f-concord-script's read of its REST API, so the row's failure is no verdict on the deliverable: the row's curl of https://api\\.github\\.com/repos/cybermonklabs/legion/pulls/3522/reviews exited 7 with no HTTP answer"
if ! case_read "$script_case" "$out" "$script_want"; then
    :
elif [ "$RUN_STATUS" = 2 ] && printf '%s\n' "$RUN_LINE" | grep -qE -- "$script_want" && [ "$script_status" = 7 ]; then
    case_passed "$script_case"
else
    case_failed "$script_case" "exit $RUN_STATUS, $(grep '^FAIL' "$out/text" | head -2 | cut -c1-300 | tr '\n' ' ')last line: $RUN_LINE; the script run by hand exited $script_status"
fi

# 151. A row whose curl of the Dispatch API hands its body to python3 is read by the curl's
# record, whichever of the two wrote its line first (the Minor of review 6003758567 at
# check.sh:2392). Three rows of claims.jsonl, c-types-2, c-handoff-feed-1 and c-terms-3, pipe a
# curl of DISPATCH_URL into python3. At b1b431ca source_answer took the first line of the
# record that names a source, and took python3's for a read of Dispatch whenever the row's
# command holds DISPATCH_URL, so a python3 that failed on the body before curl exited decided
# the row by its own output, which names no urllib error: a gateway's 502 ended exit 1, 'FAIL
# replay-command: ... python3 ... exited 1'. The case plants a row that pipes a curl of the
# Dispatch API, at an address of the case's own, into a python3 that reads the first line of
# the body as JSON, and puts first on PATH a curl of the case's own that answers the row's
# read with the first line of a gateway's page, waits until python3's line stands in the
# row's record, prints curl's line of a 502 and exits 22, and answers the gate's read with -o
# with that page and 502. --replay must end exit 2, cannot run, naming the row, Dispatch and
# the 502 as a server error, with no FAIL.
PIPE_CASE="$WORK/dispatch-pipe"
PIPE_URL=http://dispatch.case151.invalid
mkdir -p "$PIPE_CASE/bin"
cat >"$PIPE_CASE/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" DISPATCH_ADDRESS/"*) ;;
*) exec REAL_CURL "$@" ;;
esac
body='' dump='' format='' previous=''
for word in "$@"; do
    case "$previous" in
    -o) body=$word ;;
    -D) dump=$word ;;
    -w) format=$word ;;
    esac
    previous=$word
done
if [ -n "$body" ]; then
    printf '<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n</html>\r\n' >"$body"
    [ -z "$dump" ] || printf 'HTTP/1.1 502 Bad Gateway\r\ncontent-type: text/html\r\n\r\n' >"$dump"
    [ -z "$format" ] || printf '502'
    exit 0
fi
printf '<html>\r\n'
tries=0
while [ "$tries" -lt 100 ] && ! grep -q "$(printf '\t')python3 " "${__IOC_FAILED:-/nonexistent-ioc}" 2>/dev/null; do
    sleep 0.1
    tries=$((tries + 1))
done
printf 'curl: (22) The requested URL returned error: 502\n' >&2
exit 22
STUB
sed -i -e "s|REAL_CURL|'$(command -v curl)'|" -e "s|DISPATCH_ADDRESS|$PIPE_URL|" "$PIPE_CASE/bin/curl"
chmod +x "$PIPE_CASE/bin/curl"
cat >"$PIPE_CASE/command" <<'CMD'
curl -sS --fail-with-body -H @"$IOC_DISPATCH_HEADER" "$DISPATCH_URL/api/v1/references?to=dispatch://INFRA-460/artifact/glossary-md" | python3 -c 'import json,sys;print(json.loads(sys.stdin.readline())["edges"][0]["node"]["ref"])'
CMD
jq -cn --arg c "$(cat "$PIPE_CASE/command")" '{id: "f-dispatch-pipe-read", requirement: "r7", source: "the case", kind: "behaviour", claim: "The glossary is cited by a node.", mark: "verified", evidence: {surface: "cli"}, command: $c, expect: {contains: "dispatch://"}, output: "", note: "planted by case 151"}' >"$PIPE_CASE/row"
dir="$(fresh dispatch-pipe)"
cat "$PIPE_CASE/row" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
ending_result "a row whose curl of the Dispatch API, answered 502, feeds a python3 that failed first is read by the curl: it cannot run, exit 2, naming the row, Dispatch and the 502 as a server error, and fails no assertion" 2 \
    "^check\\.sh cannot run --replay now: Dispatch gave no answer that judges f-dispatch-pipe-read's read of its API, .*exited 22, since Dispatch answered 502, a server error\$" \
    "$(DISPATCH_URL="$PIPE_URL" PATH="$PIPE_CASE/bin:$PATH" fixture_run "$dir" --replay)"

# 152. The figure of a Cost is the whole word before engineer-day, and a word the reader
# cannot read fails (review 6003758567, its third item, and the architect's change of round
# 45). At a6c85d0a cost_read turned the text before the unit into words of letters, digits
# and points, so "1/2 engineer-day" read as the words 1 and 2, and the last of them, 2, put
# the page above one engineer-day: a copy of judgement v25 whose Envoy page's Cost read
# 'about 1/2 engineer-day, by our estimate' passed all 7 of --landed, with that page held to
# no state, and all 9 of --shape. The case writes the fixture's Envoy page's Cost so, leaves
# the page with no state line, and runs --shape, which must fail page-parts, and --landed,
# which must fail landed-proof, each naming the page and its Cost line as a figure the gate
# cannot read and saying that a figure in digits is a whole number or one with a decimal
# point.
dir="$(fresh cost-slash-half)"
sed -i '0,/^- \*\*Cost:\*\* 2 engineer-days$/s//- **Cost:** about 1\/2 engineer-day, by our estimate/' "$dir/judgement.md"
awk '/^### Structured metadata on an Envoy message/{f=1; next} /^### /{f=0} f && /\*\*(Landed|Not landed|Refuted by|Proven by):\*\*/{n++} f && /^- \*\*Cost:\*\* about 1\/2 engineer-day, by our estimate$/{c++} END{exit !(c == 1 && n == 0)}' "$dir/judgement.md" || {
    printf 'FAIL the fixture Envoy page could not be given a cost of 1/2 engineer-day with no state line\n'
    exit 1
}
case_result "a page whose Cost is about 1/2 engineer-day fails --shape and --landed, each naming the page and its Cost line as a figure the gate cannot read" \
    "page-parts: the page .Structured metadata on an Envoy message. gives its first step a cost whose figure this gate cannot read, .about 1/2 engineer-day, by our estimate. on its Cost line: .*in digits, a whole number or one with a decimal point" "$(fixture_run "$dir" --shape)" \
    "landed-proof: the page .Structured metadata on an Envoy message. gives its first step a cost whose figure this gate cannot read, .about 1/2 engineer-day, by our estimate. on its Cost line: .*in digits, a whole number or one with a decimal point" "$(fixture_run "$dir" --landed)"

# 153. A figure written with a decimal comma is no figure the reader reads (the item and the
# change of case 152). At a6c85d0a "0,5 engineer-days" read as the words 0 and 5, and the
# last of them, 5, put the page above one engineer-day, so a page whose first step takes
# half a day, written with a decimal comma, passed --landed with no state, and --shape. The
# case writes the fixture's Envoy page's Cost as "0,5 engineer-days", leaves the page with
# no state line, and runs --shape, which must fail page-parts, and --landed, which must fail
# landed-proof, each naming the page and its Cost line as a figure the gate cannot read and
# saying that a figure in digits is a whole number or one with a decimal point.
dir="$(fresh cost-decimal-comma)"
sed -i '0,/^- \*\*Cost:\*\* 2 engineer-days$/s//- **Cost:** 0,5 engineer-days/' "$dir/judgement.md"
awk '/^### Structured metadata on an Envoy message/{f=1; next} /^### /{f=0} f && /\*\*(Landed|Not landed|Refuted by|Proven by):\*\*/{n++} f && /^- \*\*Cost:\*\* 0,5 engineer-days$/{c++} END{exit !(c == 1 && n == 0)}' "$dir/judgement.md" || {
    printf 'FAIL the fixture Envoy page could not be given a cost of 0,5 engineer-days with no state line\n'
    exit 1
}
case_result "a page whose Cost is 0,5 engineer-days fails --shape and --landed, each naming the page and its Cost line as a figure the gate cannot read" \
    "page-parts: the page .Structured metadata on an Envoy message. gives its first step a cost whose figure this gate cannot read, .0,5 engineer-days. on its Cost line: .*in digits, a whole number or one with a decimal point" "$(fixture_run "$dir" --shape)" \
    "landed-proof: the page .Structured metadata on an Envoy message. gives its first step a cost whose figure this gate cannot read, .0,5 engineer-days. on its Cost line: .*in digits, a whole number or one with a decimal point" "$(fixture_run "$dir" --landed)"

# 154. A sentence that holds only citations cites the sentence before it (review 6003758567,
# its Minor at check.sh's citation_figures). A sentence ends at a full stop before a space, so
# in "... 6484 acts. [r-a]" the bracket was a sentence of its own: at ee5bd0b7 the figure
# before the full stop stood in a sentence that cites no row, and the bracket in one that
# states no figure, so a row r-a that does not print 6484 passed citation-figures. The case
# writes after the fixture's one cited sentence 'It also counts 6484 acts. [f-figure-1]',
# which f-figure-1 does not print, and runs --citations, which must fail citation-figures,
# naming 6484 and that sentence with its bracket. The honest form writes the fixture's
# sentence as 'The fixture's count reads 12,617 rows. [f-figure-1]', whose figure the row
# prints, which passes citation-figures; at ee5bd0b7 it failed it, since the judgement then
# stated no figure in a sentence that cites a row.
dir="$(fresh citation-alone-fault)"
sed -i 's/^The fixture.s count reads 12,617 rows \[f-figure-1\]\.$/& It also counts 6484 acts. [f-figure-1]/' "$dir/judgement.md"
grep -qxF "The fixture's count reads 12,617 rows [f-figure-1]. It also counts 6484 acts. [f-figure-1]" "$dir/judgement.md" || {
    printf 'FAIL the sentence whose citation stands alone after it could not be planted in the fixture judgement\n'
    exit 1
}
case_result "a figure whose citation stands alone after its sentence is held to the row it cites" \
    'citation-figures: line [0-9]+ of the judgement states 6484 in a sentence that cites f-figure-1, and none of them prints it in its output or its command: .It also counts 6484 acts\. \[f-figure-1\].' \
    "$(fixture_run "$dir" --citations)"
dir="$(fresh citation-alone-honest)"
sed -i 's/^The fixture.s count reads 12,617 rows \[f-figure-1\]\.$/The fixture'"'"'s count reads 12,617 rows. [f-figure-1]/' "$dir/judgement.md"
grep -qxF "The fixture's count reads 12,617 rows. [f-figure-1]" "$dir/judgement.md" || {
    printf 'FAIL the citation could not be set alone after the fixture sentence\n'
    exit 1
}
honest_result "a citation alone after a sentence whose figure its row prints passes citation-figures" citation-figures \
    "$(fixture_run "$dir" --citations)"

# 155. A predicate of one of the seven keys with a value of the wrong type is named as that
# (review 6003758567, its Minor at check.sh's predicate_is_a_sentence). At ee5bd0b7 the FAIL
# of predicate-form said of {"equals": true} that it 'carries a predicate that is not a
# bound, a set membership or an exit status', though equals is one of the seven keys the
# header's rule for expect gives: what is wrong is that its value is a JSON boolean, and
# equals takes a string. The case plants that predicate, and --replay must fail
# predicate-form with a line that names the predicate equals, the type its value has and the
# type equals takes.
dir="$(fresh op-type-named)"
plant_behaviour "$dir" "printf 'true\\n'" '{"equals":true}'
case_result "an equals of true fails predicate-form, named as an equals whose value is a JSON boolean and not a string" \
    'predicate-form: f-behaviour-1 carries the predicate equals with a value of another shape than the header.s rule for expect gives it, since its equals is a JSON boolean, not a string: [{]"equals":true[}]$' \
    "$(fixture_run "$dir" --replay)"

# 156. A citing sentence that writes a number as a fraction or with a decimal comma fails
# citation-figures (the sibling of item 3 of review 6003758567, the Cost reader that read
# 1/2 as 2). The figure reader reads digits with an optional decimal point, or a number
# grouped by thousands: at ee5bd0b7 it read a token holding a slash as a place, so '1/2'
# was no figure and its row was held to none, and it read '0,5' as the figures 0 and 5, and
# citations.py's extraction reads it as 5. The case writes after the fixture's one cited
# sentence 'It holds 1/2 of them [f-figure-1].', and in another copy 'It reads 0,5 more
# [f-figure-1].', and runs --citations over each, which must fail citation-figures, naming
# the line, the form and the forms the reader reads. The honest form is the fixture as
# built, '12,617 rows [f-figure-1]', a number grouped by thousands, which passes
# citation-figures.
for form in '1/2 of them' '0,5 more'; do
    dir="$(fresh "citation-form-$(printf '%s' "$form" | tr -c 'a-z0-9' '-')")"
    sed -i "s|^The fixture.s count reads 12,617 rows \\[f-figure-1\\]\\.\$|& It holds $form [f-figure-1].|" "$dir/judgement.md"
    grep -qxF "The fixture's count reads 12,617 rows [f-figure-1]. It holds $form [f-figure-1]." "$dir/judgement.md" || {
        printf 'FAIL the sentence writing %s could not be planted in the fixture judgement\n' "$form"
        exit 1
    }
    case "$form" in
    1/2*) shown='1/2' written='1/2, a fraction' ;;
    *) shown='0,5' written='0,5, a decimal comma' ;;
    esac
    case_result "a sentence citing f-figure-1 that writes $shown fails citation-figures, naming the line and the form" \
        "citation-figures: line [0-9]+ of the judgement writes $written, in a sentence that cites f-figure-1, and the gate reads a figure as digits with an optional decimal point, or as a thousands-grouped number" \
        "$(fixture_run "$dir" --citations)"
done
dir="$(fresh citation-form-grouped)"
grep -qxF "The fixture's count reads 12,617 rows [f-figure-1]." "$dir/judgement.md" || {
    printf 'FAIL the fixture judgement does not carry its sentence of 12,617 rows\n'
    exit 1
}
honest_result "a sentence citing f-figure-1 that writes 12,617, grouped by thousands, passes citation-figures" citation-figures \
    "$(fixture_run "$dir" --citations)"

# 157. A count.py the machine ends by a signal is no source that did not answer (the
# architect's item of round 45 after the fresh-clone --replay of e54dbfa0). The pod's memory
# guard killed that run's count.py at 09:08Z on 2026-11-21, and --replay ended 'check.sh
# cannot run --replay: Dispatch, the feed of the count's window did not answer: ... Killed
# python3 ... count.py', of the kind source, though no read of Dispatch had failed. The case
# plants a count.py that kills its own process with SIGKILL before it prints anything, and
# runs --replay, which must end exit 2, cannot run, every assertion it leaves of the kind
# environment, its last line saying count.py was ended by signal 9 (SIGKILL) and that no
# read of Dispatch failed. Either stop of --replay at the count is the case's own plant,
# so a stop of the kind source there fails the case rather than leaving it unrun.
KILLED_LINE='^check\.sh cannot run --replay: coordination/count\.py was ended by signal 9 \(SIGKILL\), so the count was not taken again, and no read of Dispatch failed$'
dir="$(fresh count-killed)"
cat >"$dir/repo/docs/research/internet-of-cognition/coordination/count.py" <<'PY'
# A walker the machine ends by SIGKILL, as the pod's memory guard ended count.py: it kills its
# own process before it reads or prints anything.
import os
import signal

os.kill(os.getpid(), signal.SIGKILL)
PY
out="$(fixture_run "$dir" --replay)"
killed_case="a count.py the machine ends by SIGKILL makes --replay cannot run, of the kind environment, saying no read of Dispatch failed"
if ! case_read "$killed_case" "$out" "^check\\.sh cannot run --replay: (coordination/count\\.py was ended by signal|Dispatch, the feed of the count's window did not answer)"; then
    :
elif [ "$RUN_STATUS" = 2 ] && printf '%s\n' "$RUN_LINE" | grep -qE -- "$KILLED_LINE" &&
    [ -n "$RUN_UNRUN" ] && ! printf '%s' "$RUN_UNRUN" | cut -f2 | grep -qvx environment; then
    case_passed "$killed_case"
else
    case_failed "$killed_case" "exit $RUN_STATUS, kinds $(printf '%s' "$RUN_UNRUN" | cut -f2 | sort -u | tr '\n' ' ')last line: $RUN_LINE"
fi

# 158. A reader the machine ends by a signal while the replay prepares a row is no row that
# passed (test round 45). The tester's fresh-clone --replay of e7518312 stopped at 13:18:01Z
# on 2026-11-21 in run_recorded of row d-4636-merged, after it wrote .out.failed and
# .out.unread and before .out.sh: the `python3 "$READER" names` of the row was gone, the
# gate's shell had no child left, and it waited 2 h 58 min with no line printed until the
# tester stopped it. A child the gate waits on that dies must end the run cannot-run, of the
# kind environment, naming what died and the signal, as case 157 holds for count.py; so the
# names step's status is read, and a reader that printed nothing does not run the row with
# no function recording its programs' statuses. The case puts a python3 first on PATH that
# runs the real one, except that a call of reader.py with the word names kills its own
# process with SIGKILL before it prints anything, and runs --replay, which must end exit 2,
# cannot run, every assertion it leaves of the kind environment, its last line naming the
# reader and signal 9.
READER_KILLED_LINE='^check\.sh cannot run --replay: .*reader.*signal 9'
dir="$(fresh reader-killed)"
mkdir -p "$dir/reader-killed-bin"
cat >"$dir/reader-killed-bin/python3" <<SH
#!/usr/bin/env bash
# A python3 whose reader.py names call the machine ends by SIGKILL, as the pod ended one.
for ((i = 1; i < \$#; i++)); do
    j=\$((i + 1))
    case "\${!i}" in */reader.py) [ "\${!j}" = names ] && kill -9 \$\$ ;; esac
done
exec $(command -v python3) "\$@"
SH
chmod +x "$dir/reader-killed-bin/python3"
out="$(PATH="$dir/reader-killed-bin:$PATH" fixture_run "$dir" --replay)"
reader_case="a reader.py the machine ends by SIGKILL while --replay prepares a row makes --replay cannot run, of the kind environment, naming the reader and the signal"
if ! case_read "$reader_case" "$out" "^check\\.sh cannot run --replay: .*reader"; then
    :
elif [ "$RUN_STATUS" = 2 ] && printf '%s\n' "$RUN_LINE" | grep -qE -- "$READER_KILLED_LINE" &&
    [ -n "$RUN_UNRUN" ] && ! printf '%s' "$RUN_UNRUN" | cut -f2 | grep -qvx environment; then
    case_passed "$reader_case"
else
    case_failed "$reader_case" "exit $RUN_STATUS, kinds $(printf '%s' "$RUN_UNRUN" | cut -f2 | sort -u | tr '\n' ' ')last line: $RUN_LINE"
fi

# 159. A process a bounded child leaves holding the pipe the gate reads is ended with the
# child (the architect's class of round 46, after test round 45, and his fifth note of round
# 48). The tester's --replay of e7518312 slept 2 h 58 min in run_recorded's
# `names="$(printf '%s' "$command" | python3 "$READER" names)"` with no child left: a
# substitution reads its pipe to its end before it waits for anything, so a process that
# still holds the pipe's write end keeps the gate waiting with nothing it waits for, and
# reads whatever that process writes there. bounded_run kills what is left of the group of
# the program it ran once the program has ended. Until round 48 this case planted the held
# pipe in the names step, which has written into a file since round 46, so with that kill
# taken out the case still passed: it drove nothing. It now plants it where a substitution
# still reads a bounded child to its end, unpinned_installs, `claims | python3 "$READER"
# installs` read by `unpinned="$(unpinned_installs ...)"`: the case puts a python3 first on
# PATH whose call of reader.py with the word installs leaves a process holding its output,
# which waits until that python3 has ended, then 5 s more, and writes there a line of its
# own, an install no row runs, and runs --replay, which must pass every assertion, that line
# never read. With the kill taken out of bounded_run the substitution waits for the process,
# reads its line, and --replay fails install-pinned naming it.
# bound_case <name> <last line, an extended regular expression> <run> [<seconds> <limit>];
# passes when the run's record holds exit 2 and that last line, which is the case's own
# plant, and every assertion it leaves could not run, of the kind environment. It holds the
# run to no time: each run a case makes ends at its own bound at the latest, BOUND_RUN, or
# BOUND_CASES for a run of a check.test.sh, and each plant ends before that, so no case
# waits forever and the last line alone decides, on a machine of any speed. It reads its
# first three arguments alone: a caller that still passes the seconds its run took and a
# limit, as the tester's case 176 does as it was committed, passes two it takes no notice of,
# so that case stands unchanged. Cases 160 to 164, 176, 179 and 182 read their runs by it.
bound_case() {
    if ! case_read "$1" "$3" "$2"; then
        :
    elif [ "$RUN_STATUS" = 2 ] && printf '%s\n' "$RUN_LINE" | grep -qE -- "$2" &&
        [ -n "$RUN_UNRUN" ] && ! printf '%s' "$RUN_UNRUN" | cut -f2 | grep -qvx environment; then
        case_passed "$1"
    else
        case_failed "$1" "exit $RUN_STATUS, kinds $(printf '%s' "$RUN_UNRUN" | cut -f2 | sort -u | tr '\n' ' ')last line: $RUN_LINE"
    fi
}
dir="$(fresh installs-held)"
mkdir -p "$dir/installs-held-bin"
cat >"$dir/installs-held-bin/python3" <<SH
#!/usr/bin/env bash
# A python3 whose reader.py installs leaves a process holding its output, which writes a
# line of its own there 5 s after this python3 has ended, unless something ends it first.
for ((i = 1; i < \$#; i++)); do
    j=\$((i + 1))
    case "\${!i}" in */reader.py) [ "\${!j}" = installs ] && { (while kill -0 \$\$ 2>/dev/null; do sleep 0.2; done; sleep 5; printf 'f-held-pipe\tan install a process the reader left wrote into its pipe\n') & } ;; esac
done
exec $(command -v python3) "\$@"
SH
chmod +x "$dir/installs-held-bin/python3"
out="$(PATH="$dir/installs-held-bin:$PATH" fixture_run "$dir" --replay)"
passed_result "a reader.py installs that leaves a process holding its pipe, which writes a line there after the reader has ended, leaves --replay passing every assertion, the process ended with the reader and its line never read" "$out"

# 160. A child that never ends ends the replay at its bound (the architect's class of round
# 46). The case puts a python3 first on PATH whose first call of reader.py with the word
# names sleeps 300 s and prints nothing, and runs --replay with IOC_CHILD_BOUND=20, which
# lowers every bound of a child to 20 s; it must end exit 2, cannot run, every assertion it
# leaves of the kind environment, its last line naming the reader and its bound of 20 s,
# which only a run its bound ended prints. At 78256590 the replay waited the whole sleep, ran
# the row with no function recording its programs' statuses and passed.
dir="$(fresh reader-endless)"
mkdir -p "$dir/reader-endless-bin"
cat >"$dir/reader-endless-bin/python3" <<SH
#!/usr/bin/env bash
# A python3 whose first call of reader.py names does not end within its bound.
for ((i = 1; i < \$#; i++)); do
    j=\$((i + 1))
    case "\${!i}" in */reader.py) [ "\${!j}" = names ] && mkdir "$dir/reader-endless-bin/fired" 2>/dev/null && exec sleep 300 ;; esac
done
exec $(command -v python3) "\$@"
SH
chmod +x "$dir/reader-endless-bin/python3"
out="$(PATH="$dir/reader-endless-bin:$PATH" IOC_CHILD_BOUND=20 fixture_run "$dir" --replay)"
bound_case "a reader.py that does not end makes --replay cannot run at its bound, of the kind environment, naming the reader and the bound" \
    "^check\\.sh cannot run --replay: the install reader's reading of the programs of the row [^ ]+ was ended by its bound of 20 s, [0-9]+ s after it started\$" \
    "$out"

# 161. A row's recorded command the machine ends by SIGKILL is no row that failed (the
# architect's rule of round 46: a row's command that a signal ends ends the run cannot run,
# of the kind environment, naming the row, the signal and how long it ran). The case adds a
# row whose python3 kills its own process with SIGKILL, as the pod's memory guard kills the
# largest process, and runs --replay, which must end exit 2, cannot run, every assertion it
# leaves of the kind environment, its last line naming the row and signal 9. At 78256590
# the row failed replay-command, "'python3 -c ...' exited 137", exit 1.
dir="$(fresh row-killed)"
jq -cn '{id: "f-killed-row", requirement: "r7", source: "this tree'"'"'s measurement", kind: "behaviour", claim: "The script prints ran once.", mark: "verified", evidence: {surface: "cli"}, command: "python3 -c '"'"'import os, signal; os.kill(os.getpid(), signal.SIGKILL)'"'"'", expect: {equals: "ran once"}, output: "ran once", note: "a row the machine ends by SIGKILL"}' \
    >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(fixture_run "$dir" --replay)"
bound_case "a row whose command the machine ends by SIGKILL makes --replay cannot run, of the kind environment, naming the row and the signal" \
    "^check\\.sh cannot run --replay: the recorded command of the row f-killed-row was ended by signal 9 \\(SIGKILL\\), [0-9]+ s after it started\$" \
    "$out"

# 162. A row's recorded command that runs past its bound is ended there (the architect's
# rule of round 46). The case adds a row whose python3 sleeps 300 s and runs --replay with
# IOC_CHILD_BOUND=20, which must end exit 2, cannot run, every assertion it leaves of the
# kind environment, its last line naming the row and its bound of 20 s. At 78256590 the row
# ran its whole sleep and failed replay-command, its output empty, exit 1.
dir="$(fresh row-endless)"
jq -cn '{id: "f-endless-row", requirement: "r7", source: "this tree'"'"'s measurement", kind: "behaviour", claim: "The script prints ran once.", mark: "verified", evidence: {surface: "cli"}, command: "python3 -c '"'"'import time; time.sleep(300)'"'"'", expect: {equals: "ran once"}, output: "ran once", note: "a row that runs past its bound"}' \
    >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
out="$(IOC_CHILD_BOUND=20 fixture_run "$dir" --replay)"
bound_case "a row whose command runs past its bound makes --replay cannot run at the bound, of the kind environment, naming the row and the bound" \
    "^check\\.sh cannot run --replay: the recorded command of the row f-endless-row was ended by its bound of 20 s, [0-9]+ s after it started\$" \
    "$out"

# 163. A local text tool the machine ends by a signal is never read as an empty value (the
# architect's form of round 46: a tool runs with no bound of its own, and its status is read
# where it runs). evaluate reads the value of a row's predicate with `jq -r --arg k "$key"
# '.[$k] | tostring'` in a substitution, and a jq killed there leaves the value empty, which
# a contains predicate holds over any output. The case puts a jq first on PATH that kills
# its own process with SIGKILL when it is given that filter with -r, as evaluate gives it
# and predicate_is_a_sentence, which gives it with -j, does not, and runs --replay, which must
# end exit 2, cannot run, every assertion it leaves of the kind environment, its last line
# naming jq, evaluate and signal 9. At 78256590 every row's predicate held over the empty
# value and the replay passed.
dir="$(fresh jq-killed-predicate)"
mkdir -p "$dir/jq-killed-bin"
cat >"$dir/jq-killed-bin/jq" <<'SH'
#!/usr/bin/env bash
# A jq the machine ends by SIGKILL when it reads the value of a predicate.
case " $* " in *" -r "*) for word in "$@"; do [ "$word" = '.[$k] | tostring' ] && kill -9 $$; done ;; esac
exec REAL_JQ "$@"
SH
sed -i "s|REAL_JQ|'$(command -v jq)'|" "$dir/jq-killed-bin/jq"
chmod +x "$dir/jq-killed-bin/jq"
out="$(PATH="$dir/jq-killed-bin:$PATH" fixture_run "$dir" --replay)"
bound_case "a jq the machine ends by SIGKILL while it reads a row's predicate makes --replay cannot run, of the kind environment, naming jq and the step" \
    "^check\\.sh cannot run --replay: the program 'jq .*', which evaluate ran, was ended by signal 9 \\(SIGKILL\\)\$" \
    "$out"

# 164. A local text tool the machine ends by a signal in a list that feeds a loop is never
# read as a short list (the architect's form of round 46). --citations reads the
# repositories its project-code rows cite with `claims | jq -r 'select(.kind=="project-code")
# | .evidence.repo // empty' | sort -u` into the loop that clones each, and a jq killed there
# leaves the list empty or short. The case puts a jq first on PATH that kills its own
# process with SIGKILL when it is given that filter, and runs --citations, which must end
# exit 2, cannot run, every assertion it leaves of the kind environment, its last line
# naming jq, mode_citations and signal 9. At 78256590 the loop read the empty list and the
# mode went on to its verdicts.
dir="$(fresh jq-killed-list)"
mkdir -p "$dir/jq-killed-bin"
cat >"$dir/jq-killed-bin/jq" <<'SH'
#!/usr/bin/env bash
# A jq the machine ends by SIGKILL when it lists the repositories project-code rows cite.
for word in "$@"; do [ "$word" = 'select(.kind=="project-code") | .evidence.repo // empty' ] && kill -9 $$; done
exec REAL_JQ "$@"
SH
sed -i "s|REAL_JQ|'$(command -v jq)'|" "$dir/jq-killed-bin/jq"
chmod +x "$dir/jq-killed-bin/jq"
out="$(PATH="$dir/jq-killed-bin:$PATH" fixture_run "$dir" --citations)"
bound_case "a jq the machine ends by SIGKILL while it lists what a loop reads makes --citations cannot run, of the kind environment, naming jq and the step" \
    "^check\\.sh cannot run --citations: the program 'jq .*project-code.*', which mode_citations ran, was ended by signal 9 \\(SIGKILL\\)\$" \
    "$out"

# 165. The commit a Landed line names is the whole word after its label, and a word the
# reader cannot read fails (the architect's class of round 46: a reader of what an author
# writes matches the whole token or fails by name, as cost_read does since case 152). At
# 63caeeea --landed took as the commit the first run of seven to forty hexadecimal digits
# after the label (grep -o), so a commit with a mistyped character was read as the digits
# before it, which git resolves to the committed commit when they are a prefix of it: the
# fixture's Landed line written as its commit with its thirteenth digit typed as the letter
# o passed all of --landed. The case writes it so and runs --landed, which must fail
# landed-proof, naming the page and the word and saying what a commit is.
dir="$(fresh landed-commit-typo)"
rows="$(cat "$dir/rows")"
typo="${rows:0:12}o${rows:13}"
sed -i "s/^- \*\*Landed:\*\* $rows\$/- **Landed:** $typo/" "$dir/judgement.md"
grep -qxF -- "- **Landed:** $typo" "$dir/judgement.md" || {
    printf 'FAIL the Landed line could not be written with a mistyped commit in the fixture judgement\n'
    exit 1
}
case_result "a Landed line whose commit holds a mistyped character fails --landed, naming the page and the word, and is not read as the digits before it" \
    "landed-proof: the page .An execution record under the gen_ai\\. names. names its commit as .$typo. on its Landed line, which is no commit this gate reads: a commit is seven to forty lowercase hexadecimal digits" \
    "$(fixture_run "$dir" --landed)"

# 166. A citing sentence that writes a fraction of decimals fails citation-figures, as one of
# whole numbers does (the architect's class of round 46; the sibling of case 156). The reader
# read a fraction as digits, a slash and digits alone, so at 63caeeea '0.72/0.55', two
# thresholds written as one token, held a slash and was read as a place, no figure, and the
# row its sentence cites was held to none: the sentence passed citation-figures. The case
# writes after the fixture's one cited sentence 'It holds 0.72/0.55 of them [f-figure-1].',
# which f-figure-1 does not print, and runs --citations, which must fail citation-figures,
# naming the line and the form.
dir="$(fresh citation-form-decimal-fraction)"
sed -i "s|^The fixture.s count reads 12,617 rows \\[f-figure-1\\]\\.\$|& It holds 0.72/0.55 of them [f-figure-1].|" "$dir/judgement.md"
grep -qxF "The fixture's count reads 12,617 rows [f-figure-1]. It holds 0.72/0.55 of them [f-figure-1]." "$dir/judgement.md" || {
    printf 'FAIL the sentence writing 0.72/0.55 could not be planted in the fixture judgement\n'
    exit 1
}
case_result "a sentence citing f-figure-1 that writes 0.72/0.55 fails citation-figures, naming the line and the form" \
    "citation-figures: line [0-9]+ of the judgement writes 0\\.72/0\\.55, a fraction, in a sentence that cites f-figure-1, and the gate reads a figure as digits with an optional decimal point, or as a thousands-grouped number" \
    "$(fixture_run "$dir" --citations)"

# 167. A part of a symbol that starts with a digit is read whole, as a line or a line range,
# and any other such part fails by name (the architect's class of round 46). resolve_symbol
# read such a part by its first digits (grep -o '^[0-9]+'), so at 63caeeea the range 3-9
# written beside a name, and 3–9 written with a dash, each read as line 3 of a file of three
# lines and passed. The case writes the symbol of the fixture's row f-our-1, which cites
# packages/daemon/internal/api/handoff.go, a file of three lines, as 'TesterVerdictRequired,
# 3-9' and as 'TesterVerdictRequired, 3–9', and runs --citations over each: the first must
# fail citation-our-code, naming the part 3-9, whose end the file does not hold; the second
# must fail it, naming 3–9 as no line or line range the gate reads. The honest form,
# 'TesterVerdictRequired, 1-3', passes it.
for form in hyphen dash honest; do
    case "$form" in
    hyphen) part='3-9' ;;
    dash) part='3–9' ;;
    honest) part='1-3' ;;
    esac
    dir="$(fresh "symbol-part-$form")"
    claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    sed -i "/\"id\":\"f-our-1\"/s/\"symbol\":\"TesterVerdictRequired\"/\"symbol\":\"TesterVerdictRequired, $part\"/" "$claims_copy"
    grep -F '"id":"f-our-1"' "$claims_copy" | grep -qF "\"symbol\":\"TesterVerdictRequired, $part\"" || {
        printf 'FAIL the symbol part %s could not be planted in the fixture row f-our-1\n' "$part"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    hyphen)
        case_result "a symbol part 3-9 beside a name is read as a line range, both ends, and fails --citations in a file of three lines" \
            "citation-our-code: f-our-1 cites 3-9, and packages/daemon/internal/api/handoff\\.go holds 3 lines at [0-9a-f]+, so the range 3-9 runs past its end" "$out"
        ;;
    dash)
        case_result "a symbol part 3–9 written with a dash fails --citations, named as no line or line range the gate reads" \
            "citation-our-code: f-our-1 cites 3–9, which starts with a digit and is no line or line range this gate reads" "$out"
        ;;
    honest) honest_result "a symbol part 1-3 beside a name, a line range inside its file, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 168. Every path a citation's file field names is read, and the field is never cut at its
# first colon (the architect's class of round 46). mode_citations took the field up to its
# first colon (${file%%:*}) before it read its paths, so at 63caeeea a field that names a
# first path with its line and a second path, 'packages/daemon/internal/api/handoff.go:1 and
# packages/daemon/internal/api/no_such.go', was read as handoff.go alone, and the second
# path, which the fixture does not hold, passed unread, as the live row s-pair-p6-source's
# second path was never read. The case writes the file field of the fixture's row f-our-1 so
# and runs --citations, which must fail citation-our-code, naming the row and the field.
dir="$(fresh citation-file-two-paths)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
sed -i '/"id":"f-our-1"/s#"file":"packages/daemon/internal/api/handoff.go"#"file":"packages/daemon/internal/api/handoff.go:1 and packages/daemon/internal/api/no_such.go"#' "$claims_copy"
grep -F '"id":"f-our-1"' "$claims_copy" | grep -qF '"file":"packages/daemon/internal/api/handoff.go:1 and packages/daemon/internal/api/no_such.go"' || {
    printf 'FAIL the file field of two paths could not be planted in the fixture row f-our-1\n'
    exit 1
}
case_result "a citation whose file field names a path after a first path with its line fails --citations when that path is missing" \
    "citation-our-code: f-our-1 cites packages/daemon/internal/api/handoff\\.go:1 and packages/daemon/internal/api/no_such\\.go, which this repository does not hold" \
    "$(fixture_run "$dir" --citations)"

# 169. A page's question is the whole word after its label, and a word the reader cannot read
# fails (the architect's class of round 46). shape_questions took the first Q and digits after
# the label (grep -o), so at 63caeeea a page whose Question line read 'Q1/Q3', two questions,
# was read as Q1 alone and passed question-list, Q3 being named on the front page. The case
# writes the question of the fixture's page 'An execution record under the gen_ai. names' so
# and runs --shape, which must fail question-list, naming the line and the word.
dir="$(fresh question-two-words)"
awk '/^### /{title = substr($0, 5)} title == "An execution record under the gen_ai. names" && /^- \*\*Question:\*\* Q1 /{sub(/ Q1 /, " Q1/Q3 ")} {print}' "$dir/judgement.md" >"$dir/judgement.md.new" &&
    mv "$dir/judgement.md.new" "$dir/judgement.md"
[ "$(grep -c '^- \*\*Question:\*\* Q1/Q3 When a role writes' "$dir/judgement.md")" = 1 ] || {
    printf 'FAIL the question Q1/Q3 could not be planted on the fixture page of the gen_ai. names\n'
    exit 1
}
case_result "a page whose question is Q1/Q3 fails --shape's question-list, naming the line and the word, and is not read as Q1" \
    "question-list: line [0-9]+ of the judgement names its question as .Q1/Q3., which is no question this gate reads: a question is Q and its number" \
    "$(fixture_run "$dir" --shape)"

# 170. The repository a project-code row names is read whole, and one the reader cannot read
# fails by name (the architect's class of round 46). mode_citations read the repository by
# its last path element (${repo##*/}), so at 63caeeea a row whose repository's owner was
# mistyped, 'ioc-projects/mas-lab', read as mas-lab, the clone sources.json pins for
# ioc-project/mas-lab, and passed. The case writes the repository of the fixture's row
# f-project-1 so and runs --citations, which must fail citation-project-code, naming the row
# and the repository and saying how a repository is written.
dir="$(fresh citation-repository-owner)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
sed -i '/"id":"f-project-1"/s#"repo":"mas-lab"#"repo":"ioc-projects/mas-lab"#' "$claims_copy"
grep -F '"id":"f-project-1"' "$claims_copy" | grep -qF '"repo":"ioc-projects/mas-lab"' || {
    printf 'FAIL the repository ioc-projects/mas-lab could not be planted in the fixture row f-project-1\n'
    exit 1
}
case_result "a project-code row whose repository's owner is mistyped fails --citations, naming the row and the repository, and is not read as the clone of its last path element" \
    "citation-project-code: f-project-1 cites the repository .ioc-projects/mas-lab., which names no repository sources.json pins a clone of: a repository is written whole" \
    "$(fixture_run "$dir" --citations)"

# 171. Each part of a symbol is read against its own file, and a part that names no file
# against the file before it (the architect's ruling of round 46 on check.sh:5632). At
# 2b46c850 resolve_symbol read only the last line a symbol names, after its last colon, and
# held it to the first path of the citation's file field, so a symbol naming two files read
# the second file's line in the first, and the first file's line not at all, as the live row
# d-28's 'merge-gate.md:81, pr-body.md:33-34' read as lines 33-34 of merge-gate.md. The case
# writes the fixture's row f-our-1 as citing skills/legion-worker/references/pr-body.md, a
# file of three lines, with the symbol 'pr-body.md:1, merge-gate.md:3', whose second part
# names line 3 of merge-gate.md, a file of two lines beside it; at 2b46c850 line 3 was read
# in pr-body.md and passed. --citations must fail citation-our-code, naming the part and
# merge-gate.md.
dir="$(fresh symbol-two-files)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
sed -i '/"id":"f-our-1"/s#"file":"packages/daemon/internal/api/handoff.go","symbol":"TesterVerdictRequired"#"file":"skills/legion-worker/references/pr-body.md","symbol":"pr-body.md:1, merge-gate.md:3"#' "$claims_copy"
grep -F '"id":"f-our-1"' "$claims_copy" | grep -qF '"file":"skills/legion-worker/references/pr-body.md","symbol":"pr-body.md:1, merge-gate.md:3"' &&
    [ "$(wc -l <"$dir/repo/skills/legion-worker/references/pr-body.md")" = 3 ] &&
    [ "$(wc -l <"$dir/repo/skills/legion-worker/references/merge-gate.md")" = 2 ] || {
    printf 'FAIL the two-file symbol could not be planted in the fixture row f-our-1\n'
    exit 1
}
case_result "a symbol whose second part names a line its own file does not hold fails --citations, read against that file and not the first" \
    "citation-our-code: f-our-1 cites merge-gate\\.md:3, and skills/legion-worker/references/merge-gate\\.md holds no such line at [0-9a-f]+, 2 lines" \
    "$(fixture_run "$dir" --citations)"

# 172. A qualified name its file holds only by its local part resolves only when each
# component of its prefix stands in the file too (the architect's rule of round 46 on
# resolve_symbol's local-part readings). resolve_symbol read a prefixed name by the text
# after its last colon, and a dotted name by the text after its last dot, when its file did
# not hold the part whole, and read nothing of the prefix, so at a1062b2e 'Engin.match'
# resolved in a file holding `class Engine` and `def match`, on match alone, as any prefix
# with a mistyped component did. The case commits packages/daemon/internal/api/engine.py into
# the fixture, a class Engine with a method match, names that commit as our repository's in
# sources.json, and writes the fixture's row f-our-1 as citing that file with the symbol
# 'Engin.match', then 'Engine.match', running --citations over each: the first must fail
# citation-our-code, naming the row, the part and the component Engin, which the file holds
# only inside Engine; the honest form passes it.
for form in mistyped honest; do
    case "$form" in
    mistyped) owner='Engin' ;;
    honest) owner='Engine' ;;
    esac
    dir="$(fresh "symbol-prefix-$form")"
    evidence_copy="$dir/repo/docs/research/internet-of-cognition"
    printf 'class Engine:\n    def match(self, row):\n        return row\n' >"$dir/repo/packages/daemon/internal/api/engine.py"
    engine="$(
        cd "$dir/repo" &&
            git add packages/daemon/internal/api/engine.py &&
            git commit --quiet -m "the engine" &&
            git rev-parse HEAD
    )"
    jq --arg c "$engine" '.our_repository.commit = $c' "$evidence_copy/sources.json" >"$dir/sources.json" &&
        mv "$dir/sources.json" "$evidence_copy/sources.json"
    sed -i "/\"id\":\"f-our-1\"/s#\"file\":\"packages/daemon/internal/api/handoff.go\",\"symbol\":\"TesterVerdictRequired\"#\"file\":\"packages/daemon/internal/api/engine.py\",\"symbol\":\"$owner.match\"#" \
        "$evidence_copy/claims.jsonl"
    [ -n "$engine" ] && [ "$(jq -r '.our_repository.commit' "$evidence_copy/sources.json")" = "$engine" ] &&
        grep -F '"id":"f-our-1"' "$evidence_copy/claims.jsonl" | grep -qF "\"file\":\"packages/daemon/internal/api/engine.py\",\"symbol\":\"$owner.match\"" || {
        printf 'FAIL the symbol %s.match and its file could not be planted in the fixture row f-our-1\n' "$owner"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    mistyped)
        case_result "a dotted name whose file holds its local part but not the component of its prefix fails --citations, naming the row, the part and the component" \
            "citation-our-code: f-our-1 cites the symbol Engin\\.match, which packages/daemon/internal/api/engine\\.py holds only by its local part 'match', and the component 'Engin' of its prefix 'Engin' does not stand in it" "$out"
        ;;
    honest) honest_result "a dotted name whose file holds each component of its prefix and its local part, Engine.match, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 173. The deployment repository is an input of --replay, never a name in this tree (the
# architect's ruling of round 46 on AGENTS.md's rule for this public repository: name
# neither the repository Legion is deployed from nor the company that runs it, and make a
# value a live script needs a required input). At c7ba7ee3 check.sh, env.sh, sources.json
# and the rows wrote that repository's owner and name; now IOC_DEPLOYMENT_REPO holds it as
# <owner>/<name>, a variable of --replay's prerequisites whose form the prerequisite step
# reads, a / between two words of letters, digits, ".", "_" and "-" (REPOSITORY_FORM). The
# case runs --replay over the fixture twice, with the variable unset and with it holding
# not-a-repo (FIXTURE_DEPLOYMENT_REPO, which fixture_run passes as IOC_DEPLOYMENT_REPO, set
# empty and then to that word); each must end exit 2, cannot run, every assertion of the
# kind prerequisite and none failed, its last line naming IOC_DEPLOYMENT_REPO, what is
# wrong with it and what provides it. At c7ba7ee3 each passed every assertion.
for form in unset malformed; do
    case "$form" in
    unset) value='' missing='the variable IOC_DEPLOYMENT_REPO is not set' ;;
    malformed) value='not-a-repo' missing="the variable IOC_DEPLOYMENT_REPO holds 'not-a-repo', which is no GitHub repository written <owner>/<name>" ;;
    esac
    dir="$(fresh "deployment-repo-$form")"
    out="$(FIXTURE_DEPLOYMENT_REPO="$value" fixture_run "$dir" --replay)"
    deployment_case="--replay with IOC_DEPLOYMENT_REPO $form cannot run, exit 2, of the kind prerequisite, naming the variable and what provides it"
    ending_read "$out"
    if [ "$RUN_CRASH" != 1 ] && [ "$RUN_STATUS" = 2 ] && [ -z "$RUN_FAILED" ] &&
        printf '%s\n' "$RUN_LINE" | grep -qxF -- "check.sh cannot run --replay: $missing; provided by: export IOC_DEPLOYMENT_REPO=<owner>/<name>, the repository the deployment applies from" &&
        [ "$(printf '%s' "$RUN_UNRUN" | cut -f2 | sort -u)" = prerequisite ]; then
        case_passed "$deployment_case"
    else
        case_failed "$deployment_case" "exit $RUN_STATUS, last line: $RUN_LINE; could not run, by kind: $(printf '%s' "$RUN_UNRUN" | cut -f2 | sort | uniq -c | tr -s ' \n' ' ')"
    fi
done

# 174. The local part of a qualified name is read as a whole word too (test round 46, the
# class of the architect's round 46: a reader of what an author writes matches the whole
# token or fails by name). resolve_symbol holds each component of a qualified name's prefix
# to its file as a whole word (prefix_held, case 172), but finds the local part with git grep
# -F, which matches it inside a longer word, so at 919871f2 'Engine.matc', its method name
# typed short, resolved in a file of `class Engine` and `def match` and passed
# citation-our-code. The case commits that file into the fixture as case 172 does and writes
# the fixture's row f-our-1 as citing it with the symbol 'Engine.matc', then 'Engine.match',
# running --citations over each: the first must fail citation-our-code, naming the row and the
# part; the honest form passes it.
for form in short honest; do
    case "$form" in
    short) local_part='matc' ;;
    honest) local_part='match' ;;
    esac
    dir="$(fresh "symbol-local-$form")"
    evidence_copy="$dir/repo/docs/research/internet-of-cognition"
    printf 'class Engine:\n    def match(self, row):\n        return row\n' >"$dir/repo/packages/daemon/internal/api/engine.py"
    engine="$(
        cd "$dir/repo" &&
            git add packages/daemon/internal/api/engine.py &&
            git commit --quiet -m "the engine" &&
            git rev-parse HEAD
    )"
    jq --arg c "$engine" '.our_repository.commit = $c' "$evidence_copy/sources.json" >"$dir/sources.json" &&
        mv "$dir/sources.json" "$evidence_copy/sources.json"
    sed -i "/\"id\":\"f-our-1\"/s#\"file\":\"packages/daemon/internal/api/handoff.go\",\"symbol\":\"TesterVerdictRequired\"#\"file\":\"packages/daemon/internal/api/engine.py\",\"symbol\":\"Engine.$local_part\"#" \
        "$evidence_copy/claims.jsonl"
    [ -n "$engine" ] && [ "$(jq -r '.our_repository.commit' "$evidence_copy/sources.json")" = "$engine" ] &&
        grep -F '"id":"f-our-1"' "$evidence_copy/claims.jsonl" | grep -qF "\"file\":\"packages/daemon/internal/api/engine.py\",\"symbol\":\"Engine.$local_part\"" || {
        printf 'FAIL the symbol Engine.%s and its file could not be planted in the fixture row f-our-1\n' "$local_part"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    short)
        case_result "a dotted name whose local part its file holds only inside a longer word, Engine.matc beside def match, fails --citations, naming the row and the part" \
            "citation-our-code: f-our-1 cites .*Engine\\.matc" "$out"
        ;;
    honest) honest_result "a dotted name whose local part its file holds as a whole word, Engine.match, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 175. A symbol part read as text is read as a whole word where it starts or ends with a
# letter, a digit or `_` (test round 46, the class of case 174). resolve_symbol looks for a
# part with git grep -F, which finds it inside a longer word, so at 919871f2 the fixture's row
# f-our-1 written with the symbol 'TesterVerdictRequire', its last letter dropped, resolved
# in packages/daemon/internal/api/handoff.go, which holds TesterVerdictRequired, and passed
# citation-our-code. The case writes that symbol and runs --citations, which must fail
# citation-our-code, naming the row and the part; the fixture's own symbol, the honest form,
# passes in every other case of --citations.
dir="$(fresh symbol-bare-short)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
sed -i '/"id":"f-our-1"/s/"symbol":"TesterVerdictRequired"/"symbol":"TesterVerdictRequire"/' "$claims_copy"
grep -F '"id":"f-our-1"' "$claims_copy" | grep -qF '"symbol":"TesterVerdictRequire"' || {
    printf 'FAIL the symbol TesterVerdictRequire could not be planted in the fixture row f-our-1\n'
    exit 1
}
case_result "a symbol its file holds only inside a longer word, TesterVerdictRequire beside TesterVerdictRequired, fails --citations, naming the row and the part" \
    "citation-our-code: f-our-1 cites .*TesterVerdictRequire([^d]|$)" \
    "$(fixture_run "$dir" --citations)"

# 176. A local text tool the machine ends by a signal while finish reads the verdict is never
# read as a verdict (test round 46, the architect's class of round 46: every local text tool's
# status is read where it runs). finish runs in the gate's own shell after the mode's
# subshell, where bound_mode's functions do not stand, so its tools run bare: at 919871f2 a
# grep killed in `grep -qxF "PASS $family" "$REPORTED"` read as no PASS, and the honest
# fixture's --citations ended 'FAIL citation-our-code: it reported no verdict, so its code
# crashed', exit 1; and the awk of declared_assertions killed there left no assertion
# declared, and every assertion the mode had passed failed as one the table does not
# declare, '6 of 6 assertions failed', exit 1. The case puts first on PATH, in turn, a grep
# that kills its own process with SIGKILL when it is asked for the line 'PASS
# citation-our-code', and an awk that does so when it is given declared_assertions'
# program, and runs --citations over the honest fixture with each; each must end exit 2,
# cannot run, every assertion it leaves of the kind environment, its last line naming the
# tool and signal 9, and never a FAIL.
for tool in grep awk; do
    dir="$(fresh "finish-$tool-killed")"
    mkdir -p "$dir/finish-bin"
    case "$tool" in
    grep) condition='[ "$1" = -qxF ] && [ "$2" = "PASS citation-our-code" ]' ;;
    awk) condition='case "$*" in *'"'"'mode == "self-test"'"'"'*) true ;; *) false ;; esac' ;;
    esac
    {
        printf '#!/usr/bin/env bash\n'
        printf '# A %s the machine ends by SIGKILL while finish reads the verdict.\n' "$tool"
        printf '%s && kill -9 $$\n' "$condition"
        printf 'exec %q "$@"\n' "$(command -v "$tool")"
    } >"$dir/finish-bin/$tool"
    chmod +x "$dir/finish-bin/$tool"
    finish_start="$EPOCHSECONDS"
    out="$(PATH="$dir/finish-bin:$PATH" fixture_run "$dir" --citations)"
    bound_case "the $tool of finish, ended by SIGKILL while it reads the verdict of an honest --citations, makes it cannot run, of the kind environment, naming $tool and the signal, and fails nothing" \
        "^check\\.sh cannot run --citations: .*$tool.*signal 9 \\(SIGKILL\\)" \
        "$out" "$((EPOCHSECONDS - finish_start))" 150
done

# 177. A line a symbol part names is 1 or more, and so is each end of a line range (test
# round 46, the tester's note on line_part). resolve_symbol read a part of digits as a line
# and held it to a file that has at least that many lines, so at 32a62b2d a part 0 resolved
# in any file, as did the range 0-3 in a file of three lines, and passed citation-our-code.
# The case writes the symbol of the fixture's row f-our-1, which cites
# packages/daemon/internal/api/handoff.go, a file of three lines, as
# 'TesterVerdictRequired, 0' and as 'TesterVerdictRequired, 0-3', and runs --citations over
# each, which must fail citation-our-code, naming the row, the part and line 0. Case 167's
# honest form, 'TesterVerdictRequired, 1-3', passes it.
for part in 0 0-3; do
    dir="$(fresh "symbol-line-zero-$part")"
    claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    sed -i "/\"id\":\"f-our-1\"/s/\"symbol\":\"TesterVerdictRequired\"/\"symbol\":\"TesterVerdictRequired, $part\"/" "$claims_copy"
    grep -F '"id":"f-our-1"' "$claims_copy" | grep -qF "\"symbol\":\"TesterVerdictRequired, $part\"" || {
        printf 'FAIL the symbol part %s could not be planted in the fixture row f-our-1\n' "$part"
        exit 1
    }
    case_result "a symbol part $part names line 0, which no file holds, and fails --citations, naming the row and the part" \
        "citation-our-code: f-our-1 cites $part, which names line 0: a file's lines are numbered from 1" \
        "$(fixture_run "$dir" --citations)"
done

# 178. A git that reads no Perl-compatible regular expression is a prerequisite --citations
# lacks, never a deliverable whose tokens no file holds (the architect's addition of round 47
# to the whole-token reading). resolve_symbol reads each symbol part, its local part and
# each component of its prefix by git grep -P (whole_token, token_held), and a git built
# without PCRE refuses every such search, 'fatal: cannot use Perl-compatible regexes when
# not compiled with USE_LIBPCRE', so each part would read as one its file does not hold and
# --citations would fail the deliverable for the machine. The case puts first on PATH a git
# that refuses -P with that line and runs everything else by this machine's git, and runs
# --citations over the fixture, which must end exit 2, cannot run, every assertion of the
# kind prerequisite and none failed, its last line naming git, what git grep -P answered and
# what provides it. At 32a62b2d, which read no part by -P, the run passed every assertion.
# The honest form, this machine's git, is every other case of --citations.
pcre_less="$WORK/git-without-pcre"
mkdir -p "$pcre_less"
cat >"$pcre_less/git" <<STUB
#!/bin/sh
# A git that reads no Perl-compatible regular expression, as a git built without PCRE.
for a in "\$@"; do
    case "\$a" in
    --) break ;;
    -P | --perl-regexp | -[!-]*P*)
        echo 'fatal: cannot use Perl-compatible regexes when not compiled with USE_LIBPCRE' >&2
        exit 128
        ;;
    esac
done
exec '$(type -P git)' "\$@"
STUB
chmod +x "$pcre_less/git"
"$pcre_less/git" --version >/dev/null 2>&1 &&
    { "$pcre_less/git" -C "$WORK" grep --no-index -qP -- git "$pcre_less/git" 2>/dev/null; [ "$?" = 128 ]; } || {
    printf 'FAIL the git that refuses -P could not be planted at %s\n' "$pcre_less/git"
    exit 1
}
dir="$(fresh citations-git-without-pcre)"
out="$(PATH="$pcre_less:$PATH" fixture_run "$dir" --citations)"
pcre_case="--citations with a git that reads no Perl-compatible regular expression cannot run, exit 2, of the kind prerequisite, naming git, its answer and what provides it"
ending_read "$out"
if [ "$RUN_CRASH" != 1 ] && [ "$RUN_STATUS" = 2 ] && [ -z "$RUN_FAILED" ] &&
    printf '%s\n' "$RUN_LINE" | grep -qxF -- "check.sh cannot run --citations: the tool git reads no Perl-compatible regular expression, which each search of a token an author wrote reads it by (whole_token): git grep -P ended exit 128, 'fatal: cannot use Perl-compatible regexes when not compiled with USE_LIBPCRE'; provided by: a git built with PCRE (USE_LIBPCRE), as the distribution's git is: apt-get install git" &&
    [ "$(printf '%s' "$RUN_UNRUN" | cut -f2 | sort -u)" = prerequisite ]; then
    case_passed "$pcre_case"
else
    case_failed "$pcre_case" "exit $RUN_STATUS, last line: $RUN_LINE; could not run, by kind: $(printf '%s' "$RUN_UNRUN" | cut -f2 | sort | uniq -c | tr -s ' \n' ' ')"
fi

# 179. A tool the machine ends by a signal in the ending's other places is no verdict and
# leaves no run without its record (the architect's ruling of round 47 on case 176: the
# rule holds in every shell the ending runs in, and a stop there writes the environment
# ending through the one record). Case 176 reaches finish; the three places it does not
# reach are run_ending's own write, check.test.sh's summary, which run_ending writes, and
# run_mode's own reading of a mode its bound or a signal ended. At 32a62b2d each tool there
# ran with its status unread: an mv killed as it renamed the record into place left the run
# no record, so its reader read a crash, in a mode and in the summary alike; and an awk
# killed as run_mode read what a mode a signal ended declares left nothing declared, so the
# run ended 'all 0 assertions passed', exit 0. The case puts first on PATH, in turn: an mv
# that kills itself with SIGKILL as it renames a record, <x>.new to <x>, into place, under
# the honest fixture's --citations; the same mv under a summary of check.test.sh's own form,
# one case that passed and run_ending cases, run by gate_run; and a jq that, at its first
# call, kills the run of the mode run_mode made, the process its environment marks
# IOC_MODE_INNER=1 under timeout, beside an awk that kills itself when it is given
# declared_assertions' program, under --citations. Each must end exit 2, cannot run, every
# unit it leaves of the kind environment, its last line naming the stop: the mv and
# run_ending, written again by printf alone; the mv and run_ending in the summary; and the
# mode ended by signal 9, the run itself (--citations) the one unit, since what the mode
# declares was not read whole.
ending_bin="$WORK/ending-bin"
mkdir -p "$ending_bin/mv" "$ending_bin/mode"
{
    printf '#!/usr/bin/env bash\n'
    printf '# An mv the machine ends by SIGKILL as it renames a record into place.\n'
    printf '[ "$1" = -f ] && [ "$2" = -- ] && [ "$3" = "$4.new" ] && case "$4" in */ending) kill -9 $$ ;; esac\n'
    printf 'exec %q "$@"\n' "$(command -v mv)"
} >"$ending_bin/mv/mv"
{
    printf '#!/usr/bin/env bash\n'
    printf '# A jq that, at its first call, ends the run of the mode run_mode made by SIGKILL.\n'
    printf 'if mkdir %q 2>/dev/null; then\n' "$ending_bin/mode/fired"
    printf '    pid=$PPID\n'
    printf '    while [ "${pid:-1}" -gt 1 ]; do\n'
    printf '        parent="$(sed -n "s/^PPid:[[:space:]]*//p" "/proc/$pid/status")"\n'
    printf '        if [ "$(cat "/proc/$parent/comm" 2>/dev/null)" = timeout ] && grep -qzx IOC_MODE_INNER=1 "/proc/$pid/environ" 2>/dev/null; then\n'
    printf '            kill -9 "$pid"\n'
    printf '            break\n'
    printf '        fi\n'
    printf '        pid="$parent"\n'
    printf '    done\n'
    printf 'fi\n'
    printf 'exec %q "$@"\n' "$(command -v jq)"
} >"$ending_bin/mode/jq"
{
    printf '#!/usr/bin/env bash\n'
    printf '# An awk the machine ends by SIGKILL as it reads what a mode declares.\n'
    printf 'case "$*" in *%q*) kill -9 $$ ;; esac\n' 'mode == "self-test"'
    printf 'exec %q "$@"\n' "$(command -v awk)"
} >"$ending_bin/mode/awk"
chmod +x "$ending_bin/mv/mv" "$ending_bin/mode/jq" "$ending_bin/mode/awk"
mkdir -p "$WORK/ending-summary"
printf '. %q && ending_add declared %q && ending_add passed %q && run_ending cases\n' "$HERE/check.sh" 'a case that passed' 'a case that passed' >"$WORK/ending-summary/check.test.sh"
dir="$(fresh ending-write-killed)"
out="$(PATH="$ending_bin/mv:$PATH" fixture_run "$dir" --citations)"
bound_case "the mv of run_ending, ended by SIGKILL as it renames the record of an honest --citations into place, makes it cannot run, of the kind environment, its record written again" \
    "^check\\.sh cannot run --citations: the program 'mv -f -- .*', which run_ending ran, was ended by signal 9 \\(SIGKILL\\)\$" \
    "$out"
out="$(PATH="$ending_bin/mv:$PATH" gate_run "$WORK/ending-summary/check.test.sh")"
bound_case "the mv of check.test.sh's summary, ended by SIGKILL as it renames the record into place, makes it cannot run, of the kind environment, its record written again" \
    "^check\\.sh cannot run check\\.test\\.sh: the program 'mv -f -- .*', which run_ending ran, was ended by signal 9 \\(SIGKILL\\)\$" \
    "$out"
dir="$(fresh ending-mode-killed)"
out="$(PATH="$ending_bin/mode:$PATH" fixture_run "$dir" --citations)"
mode_case="a mode a signal ended, whose declared list run_mode's awk cannot read, makes --citations cannot run, of the kind environment, the run itself the one unit"
ending_read "$out"
if [ "$(printf '%s' "$RUN_UNRUN" | cut -f1)" = --citations ]; then
    bound_case "$mode_case" \
        "^check\\.sh cannot run --citations: the mode was ended by signal 9 \\(SIGKILL\\), [0-9]+ s after it started\$" \
        "$out"
else
    case_failed "$mode_case" "exit $RUN_STATUS, the units that could not run: $(printf '%s' "$RUN_UNRUN" | cut -f1 | tr '\n' ' ')last line: $RUN_LINE"
fi

# 180. The deployment repository is read as GitHub names it, whatever case its input is
# written in (the architect's fourth finding of round 47). GitHub reads an owner and a name
# without case, so at 32a62b2d a value written with capitals passed the prerequisite step,
# git ls-remote and env.sh's clone, and five rows that compare it with what GitHub and the
# repository write failed replay-command on evidence that holds. --replay now reads GitHub's
# own full_name for the value before the budget, env.sh or any row, and exports
# IOC_DEPLOYMENT_REPO as that name (deployment_repository_read); a 404 ends it cannot run,
# of the kind prerequisite, naming the variable and never the value. The case runs --replay
# over this directory twice, with curl and git of its own first on PATH, as case 115 does:
# with IOC_DEPLOYMENT_REPO the placeholder FIXTURE_DEPLOYMENT_REPO written in capitals, a
# curl that answers the read of that repository, in any case, with the placeholder as
# GitHub writes it, and a git that refuses every ls-remote: of the placeholder's address as
# GitHub writes it with git's own two lines, and of any other with a line of its own, so the
# run must end exit 2 quoting git's own lines, with $IOC_DEPLOYMENT_REPO in the address,
# which only a gate that read the repository by GitHub's name reaches, and no line of its
# text may hold the value in any case; and with a curl that answers 404, so the run must
# end exit 2, of the kind prerequisite, naming the variable, and no line of its text may
# hold the value. At 32a62b2d the first quoted the stub's own line, naming the capitals, and
# the second read no repository's name.
deployment="$WORK/deployment-name"
mkdir -p "$deployment/found" "$deployment/missing"
cat >"$deployment/found/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" --config - "*)
    if grep -qi 'url = "https://api\.github\.com/repos/THE_DEPLOYMENT_REPO"'; then
        printf '{"full_name": "THE_DEPLOYMENT_REPO"}\n200'
        exit 0
    fi
    ;;
esac
printf 'curl: (7) the stub curl of case 180 answers no %s\n' "$*" >&2
exit 7
STUB
cat >"$deployment/found/git" <<'STUB'
#!/bin/sh
case " $* " in
*" ls-remote "*)
    for a in "$@"; do
        case "$a" in
        https://github.com/THE_DEPLOYMENT_REPO.git) printf "remote: Repository not found.\nfatal: repository '%s/' not found\n" "$a" >&2 ;;
        https://*) printf 'fatal: the stub git of case 180 was given the address %s, not as GitHub writes it\n' "$a" >&2 ;;
        esac
    done
    exit 128
    ;;
esac
exec REAL_GIT "$@"
STUB
cat >"$deployment/missing/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" --config - "*)
    printf '{"message": "Not Found", "status": "404"}\n404'
    printf 'curl: (22) The requested URL returned error: 404\n' >&2
    exit 22
    ;;
esac
printf 'curl: (7) the stub curl of case 180 answers no %s\n' "$*" >&2
exit 7
STUB
sed -i -e "s|THE_DEPLOYMENT_REPO|$FIXTURE_DEPLOYMENT_REPO|g" "$deployment/found/curl"
sed -i -e "s|REAL_GIT|'$(command -v git)'|" -e "s|THE_DEPLOYMENT_REPO|$FIXTURE_DEPLOYMENT_REPO|g" "$deployment/found/git"
cp "$deployment/found/git" "$deployment/missing/git"
chmod +x "$deployment/found/curl" "$deployment/found/git" "$deployment/missing/curl" "$deployment/missing/git"
capitals="${FIXTURE_DEPLOYMENT_REPO^^}"
[ "$capitals" != "$FIXTURE_DEPLOYMENT_REPO" ] && [[ "$capitals" =~ $REPOSITORY_FORM ]] || {
    printf 'FAIL the placeholder could not be written in capitals\n'
    exit 1
}
out="$(PATH="$deployment/found:$PATH" IOC_DEPLOYMENT_REPO="$capitals" gate_run "$HERE/check.sh" --replay)"
capitals_case="--replay whose IOC_DEPLOYMENT_REPO is written in capitals reads the repository by the name GitHub gives it, and git ls-remote reads that name"
if grep -q '^FAIL' "$out/text"; then
    case_failed "$capitals_case" "$(grep '^FAIL' "$out/text" | head -2 | tr '\n' ' ')"
elif grep -qiF -- "$FIXTURE_DEPLOYMENT_REPO" "$out/text"; then
    case_failed "$capitals_case" "its text names the value: $(grep -ciF -- "$FIXTURE_DEPLOYMENT_REPO" "$out/text") lines"
else
    ending_result "$capitals_case" 2 \
        "^check\\.sh cannot run --replay: GitHub's answer to git ls-remote of the deployment repository IOC_DEPLOYMENT_REPO names, .* not found\\. fatal: repository 'https://github\\.com/\\\$IOC_DEPLOYMENT_REPO\\.git/' not found" "$out" \
        '^check\.sh cannot run --replay: GitHub'
fi
out="$(PATH="$deployment/missing:$PATH" IOC_DEPLOYMENT_REPO="$FIXTURE_DEPLOYMENT_REPO" gate_run "$HERE/check.sh" --replay)"
missing_case="--replay whose IOC_DEPLOYMENT_REPO names a repository GitHub answers 404 for cannot run, of the kind prerequisite, naming the variable and not the value"
if grep -qiF -- "$FIXTURE_DEPLOYMENT_REPO" "$out/text"; then
    case_failed "$missing_case" "its text names the value: $(grep -ciF -- "$FIXTURE_DEPLOYMENT_REPO" "$out/text") lines"
else
    ending_read "$out"
    if [ "$RUN_CRASH" != 1 ] && [ "$RUN_STATUS" = 2 ] && [ -z "$RUN_FAILED" ] &&
        printf '%s\n' "$RUN_LINE" | grep -qxF -- "check.sh cannot run --replay: GitHub answered 404 to the read of the repository the variable IOC_DEPLOYMENT_REPO names, so it holds no such repository, or the token this machine's git credential helper gives for it cannot read it; provided by: export IOC_DEPLOYMENT_REPO=<owner>/<name>, the repository the deployment applies from" &&
        [ "$(printf '%s' "$RUN_UNRUN" | cut -f2 | sort -u)" = prerequisite ]; then
        case_passed "$missing_case"
    else
        case_failed "$missing_case" "exit $RUN_STATUS, last line: $RUN_LINE; could not run, by kind: $(printf '%s' "$RUN_UNRUN" | cut -f2 | sort | uniq -c | tr -s ' \n' ' ')"
    fi
fi

# 181. An outage of the read of the deployment repository's name is a source that did not
# answer, never a verdict, and its line names the input and not the value (the architect's
# condition of round 47 on deployment_repository_read). The read goes through live_read, so
# an answer that is neither the repository nor a 404 ends --replay cannot run, of the kind
# source, as every read of a live source the gate makes itself does, and the line, written
# by cannot_run, names IOC_DEPLOYMENT_REPO and quotes curl's own line. The case runs --replay
# over this directory twice, as case 180 does, with IOC_DEPLOYMENT_REPO the placeholder
# FIXTURE_DEPLOYMENT_REPO and a curl of its own first on PATH that answers that read with a
# 503, then with no answer at all, exit 7, beside a git that refuses every ls-remote, so a
# gate that went on past the read would end naming git instead; each run must end exit 2,
# every assertion it leaves of the kind source, its last line naming IOC_DEPLOYMENT_REPO and
# curl's line, and no line of its text may hold the value in any case. ba71a485 already
# ends each run so; the case keeps that proven.
outage="$WORK/deployment-outage"
mkdir -p "$outage/503" "$outage/none"
cat >"$outage/503/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" --config - "*)
    printf '{"message": "Service Unavailable"}\n503'
    printf 'curl: (22) The requested URL returned error: 503\n' >&2
    exit 22
    ;;
esac
printf 'curl: (7) the stub curl of case 181 answers no read but the one of the repository\n' >&2
exit 7
STUB
cat >"$outage/none/curl" <<'STUB'
#!/bin/sh
printf "curl: (7) Failed to connect to api.github.com port 443 after 0 ms: Couldn't connect to server\n" >&2
exit 7
STUB
cat >"$outage/503/git" <<'STUB'
#!/bin/sh
case " $* " in
*" ls-remote "*)
    printf 'fatal: the stub git of case 181 refuses every ls-remote\n' >&2
    exit 128
    ;;
esac
exec REAL_GIT "$@"
STUB
sed -i -e "s|REAL_GIT|'$(command -v git)'|" "$outage/503/git"
cp "$outage/503/git" "$outage/none/git"
chmod +x "$outage/503/curl" "$outage/none/curl" "$outage/503/git" "$outage/none/git"
for form in 503 none; do
    case "$form" in
    503) said='curl: \(22\) The requested URL returned error: 503' what='a 503' ;;
    none) said="curl: \\(7\\) Failed to connect to api\\.github\\.com port 443 after 0 ms: Couldn't connect to server" what='no answer' ;;
    esac
    out="$(PATH="$outage/$form:$PATH" IOC_DEPLOYMENT_REPO="$FIXTURE_DEPLOYMENT_REPO" gate_run "$HERE/check.sh" --replay)"
    outage_case="--replay whose read of the deployment repository's name gets $what cannot run, of the kind source, naming IOC_DEPLOYMENT_REPO and not the value"
    if grep -qiF -- "$FIXTURE_DEPLOYMENT_REPO" "$out/text"; then
        case_failed "$outage_case" "its text names the value: $(grep -ciF -- "$FIXTURE_DEPLOYMENT_REPO" "$out/text") lines"
        continue
    fi
    ending_read "$out"
    if [ "$RUN_CRASH" != 1 ] && [ "$RUN_STATUS" = 2 ] && [ -z "$RUN_FAILED" ] &&
        printf '%s\n' "$RUN_LINE" | grep -qE -- "^check\\.sh cannot run --replay: GitHub, the read of the repository IOC_DEPLOYMENT_REPO names, did not answer: $said ?\$" &&
        [ "$(printf '%s' "$RUN_UNRUN" | cut -f2 | sort -u)" = source ]; then
        case_passed "$outage_case"
    else
        case_failed "$outage_case" "exit $RUN_STATUS, last line: $RUN_LINE; could not run, by kind: $(printf '%s' "$RUN_UNRUN" | cut -f2 | sort | uniq -c | tr -s ' \n' ' ')"
    fi
done

# 182. A program a row's xargs runs that a signal ends is no row that failed (the
# architect's second item of round 47, the class of case 161). A program xargs runs has no
# function of its own, since xargs runs it, and GNU xargs exits 125 when a signal ended
# that program, so at ba71a485 xargs's function recorded 125 and the row failed
# replay-command, "'xargs ...' exited 125", a verdict on the deliverable for the machine.
# run_recorded now reads an xargs that recorded 125, at a status the row did not declare,
# as a program a signal ended. The case adds a row whose command is
# `printf 'a\n' | xargs python3 -c '...'`, whose python3 kills its own process with
# SIGKILL, and runs --replay, which must end exit 2, cannot run, every assertion it leaves
# of the kind environment, its last line naming the row and saying that xargs exited 125.
dir="$(fresh xargs-killed-row)"
jq -cn '{id: "f-xargs-killed", requirement: "r7", source: "this tree'"'"'s measurement", kind: "behaviour", claim: "The script prints ran once.", mark: "verified", evidence: {surface: "cli"}, command: "printf '"'"'a\\n'"'"' | xargs python3 -c '"'"'import os, signal; os.kill(os.getpid(), signal.SIGKILL)'"'"'", expect: {equals: "ran once"}, output: "ran once", note: "a row whose xargs runs a program the machine ends by SIGKILL"}' \
    >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
grep -qF '"id":"f-xargs-killed"' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
    printf 'FAIL the row whose xargs runs a killed program could not be planted in the fixture\n'
    exit 1
}
out="$(fixture_run "$dir" --replay)"
bound_case "a row whose xargs runs a program the machine ends by SIGKILL makes --replay cannot run, of the kind environment, naming the row and xargs's status 125" \
    "^check\\.sh cannot run --replay: the program 'xargs python3 -c .*' the row f-xargs-killed ran exited 125, which xargs exits with when a program it ran was ended by a signal, and the row ran [0-9]+ s\$" \
    "$out"

# 183. A symbol part written as a file's name and a name it holds, `file.go:Symbol`, is read
# as that file and that name (review 6011626086, its thread at check.sh's resolve_symbol). A
# part its file does not hold whole reaches the reading of a prefixed name, which held the
# local part to the paths the citation names and read the prefix as a namespace, each
# component of it a word the file must hold (prefix_held), so at cf2e23db
# `spendattrs.go:SpendRoleAttribute` failed because that file's text writes neither
# spendattrs nor go as a word, and `count.py:quote_matches` held only because count.py's
# text says count and py; a line after a file's name, `name.py:88-97`, was already read
# against that file. The case commits two files into the fixture,
# packages/daemon/internal/api/a.go and b.go, declaring the functions Alpha and Beta, neither
# writing a, b or go as a word, names that commit as our repository's in sources.json, and
# writes the fixture's row f-our-1 as citing both files, with the symbol
# 'a.go:Alpha, b.go:Beta', then its dotted form 'a.go.Alpha, b.go.Beta', and then
# 'a.go:Beta', running --citations over each: the first two must pass citation-our-code, and
# the third, whose local part a.go does not hold though b.go does, must fail it, naming the
# part and a.go. At cf2e23db the first two failed, each naming the component 'a' of the
# prefix 'a.go'.
for form in colon dot other; do
    case "$form" in
    colon) symbol='a.go:Alpha, b.go:Beta' ;;
    dot) symbol='a.go.Alpha, b.go.Beta' ;;
    other) symbol='a.go:Beta' ;;
    esac
    dir="$(fresh "symbol-file-prefix-$form")"
    evidence_copy="$dir/repo/docs/research/internet-of-cognition"
    api="$dir/repo/packages/daemon/internal/api"
    printf 'package alphapkg\n\nfunc Alpha() int {\n\treturn 1\n}\n' >"$api/a.go"
    printf 'package betapkg\n\nfunc Beta() int {\n\treturn 2\n}\n' >"$api/b.go"
    named="$(
        cd "$dir/repo" &&
            git add packages/daemon/internal/api/a.go packages/daemon/internal/api/b.go &&
            git commit --quiet -m "two files of one function each" &&
            git rev-parse HEAD
    )"
    jq --arg c "$named" '.our_repository.commit = $c' "$evidence_copy/sources.json" >"$dir/sources.json" &&
        mv "$dir/sources.json" "$evidence_copy/sources.json"
    sed -i "/\"id\":\"f-our-1\"/s#\"file\":\"packages/daemon/internal/api/handoff.go\",\"symbol\":\"TesterVerdictRequired\"#\"file\":\"packages/daemon/internal/api/a.go, packages/daemon/internal/api/b.go\",\"symbol\":\"$symbol\"#" \
        "$evidence_copy/claims.jsonl"
    [ -n "$named" ] && [ "$(jq -r '.our_repository.commit' "$evidence_copy/sources.json")" = "$named" ] &&
        grep -F '"id":"f-our-1"' "$evidence_copy/claims.jsonl" | grep -qF "\"file\":\"packages/daemon/internal/api/a.go, packages/daemon/internal/api/b.go\",\"symbol\":\"$symbol\"" &&
        ! grep -qwE 'a|b|go' "$api/a.go" "$api/b.go" || {
        printf 'FAIL the symbol %s and its two files could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    other)
        case_result "a symbol part a.go:Beta, whose prefix names a.go and whose local part only b.go holds, fails --citations, naming the part and a.go" \
            "citation-our-code: f-our-1 cites the symbol a\\.go:Beta, whose prefix names the file packages/daemon/internal/api/a\\.go, and packages/daemon/internal/api/a\\.go does not hold its local part 'Beta'" "$out"
        ;;
    *) honest_result "symbol parts each written as a file and a name that file holds, $symbol, in files that write neither a, b nor go as a word, pass citation-our-code" citation-our-code "$out" ;;
    esac
done

# 184. No PASS or FAIL line names the deployment repository (the architect's second note of
# round 48). A row's command, the URL its curl read among it, reached fail() as it stood: a
# deployment REST row, a curl of https://api.github.com/repos/$IOC_DEPLOYMENT_REPO/..., whose
# read GitHub answered 404 failed replay-command quoting that curl with the value in its URL,
# though every stop of a run wrote $IOC_DEPLOYMENT_REPO in its place (deployment_hidden).
# Each PASS and FAIL line is now printed by deployment_hidden too. The case plants into the
# fixture a row that reads a pull request of the deployment repository through that URL,
# with a curl of its own first on PATH that answers every read of api.github.com 404 Not
# Found as GitHub does: to the row, the body on stdout, curl's line on stderr and exit 22,
# and to the gate's read again with -o, the body and the headers into the files it names
# and the status. It runs --replay, whose IOC_DEPLOYMENT_REPO fixture_run sets to the
# placeholder FIXTURE_DEPLOYMENT_REPO: no line of the run's text may hold the placeholder,
# in any case, and the run must fail replay-command, naming the row, its curl with
# $IOC_DEPLOYMENT_REPO in the URL, its exit 22 and GitHub's 404. At cf2e23db the FAIL line
# held the placeholder.
deployment_404="$WORK/deployment-404"
mkdir -p "$deployment_404/bin"
cat >"$deployment_404/bin/curl" <<'STUB'
#!/bin/sh
case " $* " in
*" https://api.github.com/"*) ;;
*) exec REAL_CURL "$@" ;;
esac
body='' dump='' format='' previous=''
for word in "$@"; do
    case "$previous" in
    -o) body=$word ;;
    -D) dump=$word ;;
    -w) format=$word ;;
    esac
    previous=$word
done
answer='{"message": "Not Found", "documentation_url": "https://docs.github.com/rest/pulls/pulls#get-a-pull-request", "status": "404"}'
if [ -n "$body" ]; then
    printf '%s\n' "$answer" >"$body"
    [ -z "$dump" ] || printf 'HTTP/2 404\r\ncontent-type: application/json; charset=utf-8\r\n\r\n' >"$dump"
    [ -z "$format" ] || printf '404'
    exit 0
fi
printf '%s\n' "$answer"
printf 'curl: (22) The requested URL returned error: 404\n' >&2
exit 22
STUB
sed -i "s|REAL_CURL|'$(command -v curl)'|" "$deployment_404/bin/curl"
chmod +x "$deployment_404/bin/curl"
dir="$(fresh deployment-read-404)"
claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
jq -cn '{id: "f-deployment-read", requirement: "r7", source: "the case", kind: "behaviour", claim: "The deployment repository holds pull request 1.", mark: "verified", evidence: {surface: "cli"}, command: "curl -sS --fail-with-body https://api.github.com/repos/$IOC_DEPLOYMENT_REPO/pulls/1 | jq -r .title", expect: {equals: "the first pull request"}, output: "the first pull request", note: "planted by case 184"}' \
    >>"$claims_copy"
grep -F '"id":"f-deployment-read"' "$claims_copy" | grep -qF 'https://api.github.com/repos/$IOC_DEPLOYMENT_REPO/pulls/1' || {
    printf 'FAIL the row that reads the deployment repository could not be planted in the fixture\n'
    exit 1
}
out="$(PATH="$deployment_404/bin:$PATH" fixture_run "$dir" --replay)"
hidden_case="a deployment REST row whose read GitHub answers 404 fails replay-command, naming the row, its curl with \$IOC_DEPLOYMENT_REPO in the URL and the 404, and no line of the run names the value"
if grep -qiF -- "$FIXTURE_DEPLOYMENT_REPO" "$out/text"; then
    case_failed "$hidden_case" "its text names the value: $(grep -ciF -- "$FIXTURE_DEPLOYMENT_REPO" "$out/text") lines"
else
    case_result "$hidden_case" \
        'replay-command: f-deployment-read ran its command and a read inside it failed: .curl -sS --fail-with-body https://api\.github\.com/repos/\$IOC_DEPLOYMENT_REPO/pulls/1 ?. exited 22, and GitHub answered 404: Not Found' "$out"
fi

# 185. git's 129 is git's own exit and no signal (the architect's third note of round 48).
# git exits 129 on a usage error, and 129 is 128 and SIGHUP's number too, so bounded_run,
# reading the status of a row's script, and run_recorded, reading the status a row's git
# recorded, each took a git given an option it does not know for a program signal 1 ended:
# at cf2e23db such a row ended --replay cannot run, of the kind environment, naming signal 1
# (SIGHUP), where any other program's failure fails the row. A git's 129 is now read as
# git's own exit (GIT_USAGE), wherever it stands. The case plants into the fixture a row
# whose command is `git --no-such-option`, its git the last command, so the row's script
# exits 129 too, and then one whose command is `git --no-such-option; printf 'ran once\n'`,
# its git's 129 recorded before a command that exits 0, and runs --replay over each, which
# must fail replay-command, naming the row and git's exit 129, and not end cannot run.
for form in last first; do
    case "$form" in
    last) command='git --no-such-option' ;;
    first) command="git --no-such-option; printf 'ran once\\n'" ;;
    esac
    dir="$(fresh "git-usage-$form")"
    claims_copy="$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
    jq -cn --arg id "f-git-usage-$form" --arg c "$command" '{id: $id, requirement: "r7", source: "this tree'"'"'s measurement", kind: "behaviour", claim: "The script prints ran once.", mark: "verified", evidence: {surface: "cli"}, command: $c, expect: {equals: "ran once"}, output: "ran once", note: "a row whose git is given an option it does not know"}' \
        >>"$claims_copy"
    grep -qF "\"id\":\"f-git-usage-$form\"" "$claims_copy" || {
        printf 'FAIL the row whose git is given an option it does not know could not be planted in the fixture\n'
        exit 1
    }
    out="$(fixture_run "$dir" --replay)"
    git_case="a row whose git is given an option it does not know, git standing $form in its command, fails replay-command on git's exit 129 and does not end --replay cannot run"
    if ! case_read "$git_case" "$out" 'signal 1 \(SIGHUP\)'; then
        :
    elif [ "$RUN_ENDING" = cannot-run ]; then
        case_failed "$git_case" "the gate read git's 129 as signal 1: $RUN_LINE"
    else
        case_result "$git_case" "replay-command: f-git-usage-$form ran its command and a read inside it failed: 'git --no-such-option ?' exited 129" "$out"
    fi
done

# 186. A machine with no timeout lacks a prerequisite, named, and never ends 127 (the
# architect's fourth note of round 48). run_mode runs the mode under GNU timeout by
# bounded_run before the mode's prerequisites, timeout's line of prerequisite_table among
# them, are read, so at cf2e23db a PATH with no timeout ended every mode exit 127, with no
# ending record and no line naming what was missing. The mode's line for timeout is now read
# before that run. The case runs --citations and --shape over the fixture with a PATH that
# holds every program of this one but timeout (path_without), and each must end exit 2,
# cannot run, every assertion it declares of the kind prerequisite and none failed, its last
# line naming timeout and, as the table's line does, what provides it.
path_without timeout
for mode in citations shape; do
    dir="$(fresh "missing-timeout-$mode")"
    out="$(PATH="$WORK/path-without-timeout" fixture_run "$dir" "--$mode")"
    timeout_case="--$mode with no timeout on PATH cannot run, exit 2, of the kind prerequisite, naming timeout and what provides it"
    ending_read "$out"
    if [ "$RUN_CRASH" != 1 ] && [ "$RUN_STATUS" = 2 ] && [ -z "$RUN_FAILED" ] &&
        printf '%s\n' "$RUN_LINE" | grep -qxF -- "check.sh cannot run --$mode: the tool timeout is not on PATH; provided by: apt-get install coreutils" &&
        [ "$(printf '%s' "$RUN_UNRUN" | cut -f2 | sort -u)" = prerequisite ]; then
        case_passed "$timeout_case"
    else
        case_failed "$timeout_case" "exit $RUN_STATUS, last line: $RUN_LINE; could not run, by kind: $(printf '%s' "$RUN_UNRUN" | cut -f2 | sort | uniq -c | tr -s ' \n' ' ')"
    fi
done

# 187. A dotted name whose prefix names its file as a module does, `engine.match` for the
# function match of engine.py, is read as that file and that name (test round 48, the class
# of case 183: a symbol part whose prefix names its file is read in that file, not as a
# namespace the file's text must write). Case 183 reads a prefix that is the file's name
# with its extension, `a.go:Alpha` or `a.go.Alpha`; Python names a module-level function by
# its module, the file's name without its extension, and a package path before it, as
# `engine.match` or `api.engine.match`, and a module's own text seldom writes its name. At
# e0845e61 such a prefix reached prefix_held, which asks each component to stand in the file
# as a word, so both failed citation-our-code ("the component 'engine' of its prefix 'engine'
# does not stand in it"), though engine.py declares match. The case commits
# packages/daemon/internal/api/engine.py into the fixture, a module-level `def match`, its
# text writing neither engine nor api as a word, names that commit as our repository's in
# sources.json, and writes the fixture's row f-our-1 as citing that file with the symbol
# 'engine.match', then 'api.engine.match', then 'engin.match', running --citations over
# each: the first two must pass citation-our-code; the third, whose prefix names no file
# and is no word of the file, must fail it, naming the part.
for form in module package mistyped; do
    case "$form" in
    module) symbol='engine.match' ;;
    package) symbol='api.engine.match' ;;
    mistyped) symbol='engin.match' ;;
    esac
    dir="$(fresh "symbol-module-prefix-$form")"
    evidence_copy="$dir/repo/docs/research/internet-of-cognition"
    api="$dir/repo/packages/daemon/internal/api"
    printf 'def match(row):\n    return row\n' >"$api/engine.py"
    named="$(
        cd "$dir/repo" &&
            git add packages/daemon/internal/api/engine.py &&
            git commit --quiet -m "a module of one function" &&
            git rev-parse HEAD
    )"
    jq --arg c "$named" '.our_repository.commit = $c' "$evidence_copy/sources.json" >"$dir/sources.json" &&
        mv "$dir/sources.json" "$evidence_copy/sources.json"
    sed -i "/\"id\":\"f-our-1\"/s#\"file\":\"packages/daemon/internal/api/handoff.go\",\"symbol\":\"TesterVerdictRequired\"#\"file\":\"packages/daemon/internal/api/engine.py\",\"symbol\":\"$symbol\"#" \
        "$evidence_copy/claims.jsonl"
    [ -n "$named" ] && [ "$(jq -r '.our_repository.commit' "$evidence_copy/sources.json")" = "$named" ] &&
        grep -F '"id":"f-our-1"' "$evidence_copy/claims.jsonl" | grep -qF "\"file\":\"packages/daemon/internal/api/engine.py\",\"symbol\":\"$symbol\"" &&
        ! grep -qwE 'engine|api|engin' "$api/engine.py" || {
        printf 'FAIL the symbol %s and its module could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    mistyped)
        case_result "a dotted name engin.match, whose prefix names no file and is no word of engine.py, fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol engin\\.match" "$out"
        ;;
    *) honest_result "a dotted name $symbol, whose prefix names engine.py as a module, in a file that writes neither engine nor api as a word, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 188. A module path is the file's stem after the directories above it, each matched whole
# from the file upward, and a prefix whose directory is not the file's names no file (the
# architect's note of round 49 on case 187). module_file reads a dotted prefix as a module
# of a file the citation names: its last component the file's stem, its name without its
# extension, and each component before it the next directory up. So does a colon prefix,
# the object reference `pkg.mod:func` that Python's entry points write for the function
# func of pkg/mod.py. The case commits pkg/mod.py into the fixture, a module-level
# `def func`, its text writing neither pkg, mod nor other as a word, names that commit as our
# repository's in sources.json, and writes the fixture's row f-our-1 as citing that file
# with the symbol 'pkg.mod.func', then 'pkg.mod:func', then 'other.mod.func', running
# --citations over each (symbol_plant): the first two must pass citation-our-code; the
# third, whose directory is not pkg, names no file as a module and is read as a qualified
# name, and must fail it, naming the part and the component other. At ca8b6531 the first two
# failed, each naming the component 'pkg' of the prefix 'pkg.mod'.
for form in dotted colon other; do
    case "$form" in
    dotted) symbol='pkg.mod.func' ;;
    colon) symbol='pkg.mod:func' ;;
    other) symbol='other.mod.func' ;;
    esac
    dir="$(symbol_plant "symbol-module-path-$form" pkg/mod.py "$symbol" pkg/mod.py $'def func(row):\n    return row\n')" &&
        ! grep -qwE 'pkg|mod|other' "$dir/repo/pkg/mod.py" || {
        printf 'FAIL the symbol %s and its module could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    other)
        case_result "a dotted name other.mod.func, whose directory is not pkg/mod.py's, names no file as a module and fails --citations, naming the part and the component other" \
            "citation-our-code: f-our-1 cites the symbol other\\.mod\\.func, which pkg/mod\\.py holds only by its local part 'func', and the component 'other' of its prefix 'other\\.mod'" "$out"
        ;;
    *) honest_result "a symbol $symbol, whose prefix names pkg/mod.py as a module, in a file that writes neither pkg nor mod as a word, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 189. A class or an attribute after the module, `engine.Engine.match`, is read in the
# module's file (the architect's note (a) of round 49, the class of cases 183, 187 and 188: a
# correct Python reference read as a namespace). Python names a method by its module, its
# class and its name, so module_file takes the longest leading run of the prefix's dotted
# components that names a file as a module, and module_held asks each name after that run,
# the class and then the method, to stand in that file as a whole token. At ca8b6531 such a
# prefix reached prefix_held, which asks the module's own name to stand in its text, so
# 'engine.Engine.match' failed citation-our-code on the component engine, though engine.py
# declares the class Engine and its method match. The case commits
# packages/daemon/internal/api/engine.py, a class Engine with a method match, its text writing
# neither engine nor api as a word, and writes the row f-our-1 as citing that file with the
# symbol 'engine.Engine.match, api.engine.Engine.match', then 'engine.Engin.match', running
# --citations over each: the first must pass citation-our-code; the second, a class the file
# holds only inside Engine, must fail it, naming Engin.
for form in honest mistyped; do
    case "$form" in
    honest) symbol='engine.Engine.match, api.engine.Engine.match' ;;
    mistyped) symbol='engine.Engin.match' ;;
    esac
    dir="$(symbol_plant "symbol-module-class-$form" packages/daemon/internal/api/engine.py "$symbol" \
        packages/daemon/internal/api/engine.py $'class Engine:\n    def match(self, row):\n        return row\n')" &&
        ! grep -qwE 'engine|api|Engin' "$dir/repo/packages/daemon/internal/api/engine.py" || {
        printf 'FAIL the symbol %s and its module could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    mistyped)
        case_result "a dotted name engine.Engin.match, whose class engine.py holds only inside Engine, fails --citations, naming Engin" \
            "citation-our-code: f-our-1 cites the symbol engine\\.Engin\\.match, whose prefix names the file packages/daemon/internal/api/engine\\.py as the module engine, and packages/daemon/internal/api/engine\\.py does not hold the name 'Engin'" "$out"
        ;;
    honest) honest_result "dotted names $symbol, a class and its method after the module engine.py names, in a file that writes neither engine nor api as a word, pass citation-our-code" citation-our-code "$out" ;;
    esac
done

# 190. A package's __init__.py is the module its directory names (the architect's note (b) of
# round 49). Python names a function of a package's __init__.py by the package, `pkg.func`,
# and a package path before it, `a.pkg.func`, so module_names reads a file named __init__.py
# by the directory that holds it, and each component before that as the next directory up.
# At ca8b6531 'pkg.func' failed citation-our-code on the component pkg, which the file's text
# does not write. The case commits a/pkg/__init__.py, a function func, its text writing none
# of a, pkg and other as a word, and writes the row f-our-1 as citing that file with the
# symbol 'pkg.func, a.pkg.func', then 'other.pkg.func', running --citations over each: the
# first must pass citation-our-code; the second, whose directory is not a, names no module and
# is read as a qualified name, and must fail it, naming the component other.
for form in honest other; do
    case "$form" in
    honest) symbol='pkg.func, a.pkg.func' ;;
    other) symbol='other.pkg.func' ;;
    esac
    dir="$(symbol_plant "symbol-module-package-$form" a/pkg/__init__.py "$symbol" a/pkg/__init__.py $'def func(row):\n    return row\n')" &&
        ! grep -qwE 'a|pkg|other' "$dir/repo/a/pkg/__init__.py" || {
        printf 'FAIL the symbol %s and its package could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    other)
        case_result "a dotted name other.pkg.func, whose directory is not a/pkg/__init__.py's, names no module and fails --citations, naming the component other" \
            "citation-our-code: f-our-1 cites the symbol other\\.pkg\\.func, which a/pkg/__init__\\.py holds only by its local part 'func', and the component 'other' of its prefix 'other\\.pkg'" "$out"
        ;;
    honest) honest_result "dotted names $symbol, a function of the package a/pkg/__init__.py names, in a file that writes none of a and pkg as a word, pass citation-our-code" citation-our-code "$out" ;;
    esac
done

# 191. A module below a directory the citation names is read in its file (the architect's
# note (c) of round 49). A citation whose file field names a directory, lib, cites a module
# below it by the path from that directory down, `core.verifier.verify` for the function
# verify of lib/core/verifier.py, or `core.setup` for one of the package lib/core/__init__.py,
# and Python's full reference writes the directory's own name first, `lib.core.verifier.verify`,
# or names the directory's own package alone, `lib.hook` for lib/__init__.py; module_below
# reads each so. At ca8b6531 each failed citation-our-code on a component the files' text does
# not write. The case commits lib/core/verifier.py, a function verify under a comment that
# says core, lib/core/__init__.py, a function setup, and lib/__init__.py, a function hook, the
# text writing none of lib, verifier and verifer as a word, and writes the row f-our-1 as
# citing the directory lib with the symbol
# 'core.verifier.verify, lib.core.verifier.verify, core.setup, lib.hook', then
# 'core.verifer.verify', running --citations over each: the first must pass
# citation-our-code; the second, whose longest run that names a module is the package core,
# must fail it, naming verifer, which lib/core/__init__.py does not hold.
for form in honest mistyped; do
    case "$form" in
    honest) symbol='core.verifier.verify, lib.core.verifier.verify, core.setup, lib.hook' ;;
    mistyped) symbol='core.verifer.verify' ;;
    esac
    dir="$(symbol_plant "symbol-module-below-$form" lib "$symbol" \
        lib/core/verifier.py $'# the core checks\ndef verify(row):\n    return row\n' \
        lib/core/__init__.py $'def setup(row):\n    return row\n' \
        lib/__init__.py $'def hook(row):\n    return row\n')" &&
        ! grep -rqwE 'lib|verifier|verifer' "$dir/repo/lib" || {
        printf 'FAIL the symbol %s and the modules below lib could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    mistyped)
        case_result "a dotted name core.verifer.verify, cited in the directory lib, whose longest module run is the package core, fails --citations, naming verifer" \
            "citation-our-code: f-our-1 cites the symbol core\\.verifer\\.verify, whose prefix names the file lib/core/__init__\\.py as the module core, and lib/core/__init__\\.py does not hold the name 'verifer'" "$out"
        ;;
    honest) honest_result "dotted names $symbol, modules and packages below the directory lib the citation names, pass citation-our-code" citation-our-code "$out" ;;
    esac
done

# 192. A name and a colon before a line, `Symbol:line`, is read as text its file holds and as
# that file's line (the architect's class of round 49: every symbol form the gate reads is
# stated once, in check.sh's header above resolve_symbol, each pinned by a case that passes and
# one that fails). The live row m-eval-our-scorers writes `askOnMessage:306` so, and no case
# held the form. The case writes the fixture's row f-our-1, which cites
# packages/daemon/internal/api/handoff.go, a file of three lines declaring
# TesterVerdictRequired, with the symbol 'TesterVerdictRequired:3', then
# 'TesterVerdictRequird:3', then 'TesterVerdictRequired:4', running --citations over each
# (symbol_plant): the first must pass citation-our-code; the second, whose name is neither a
# file nor text the file holds, must fail it, naming the part and the name; and the third, a
# line past the file's end, must fail it, naming the file's three lines.
for form in honest mistyped past-end; do
    case "$form" in
    honest) symbol='TesterVerdictRequired:3' ;;
    mistyped) symbol='TesterVerdictRequird:3' ;;
    past-end) symbol='TesterVerdictRequired:4' ;;
    esac
    dir="$(symbol_plant "symbol-name-line-$form" packages/daemon/internal/api/handoff.go "$symbol")" || {
        printf 'FAIL the symbol %s could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    mistyped)
        case_result "a symbol part TesterVerdictRequird:3, whose name is neither a file nor text handoff.go holds, fails --citations, naming the part and the name" \
            "citation-our-code: f-our-1 cites TesterVerdictRequird:3, and TesterVerdictRequird is neither a file beside the paths the citation names nor text the file before it" "$out"
        ;;
    past-end)
        case_result "a symbol part TesterVerdictRequired:4, a line past the end of handoff.go, fails --citations, naming the file's three lines" \
            "citation-our-code: f-our-1 cites TesterVerdictRequired:4, and packages/daemon/internal/api/handoff\\.go holds no such line at [0-9a-f]+, 3 lines" "$out"
        ;;
    honest) honest_result "a symbol part TesterVerdictRequired:3, a name handoff.go holds and a line it holds, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 193. A part written in words is read as text its file holds, and one that no reading of its
# form finds is reported as not checked, never passed unread (the architect's class of round
# 49). resolve_symbol read any part holding a `/` as a path, by paths_at, which passes over a
# part written in words and returns 0, so at ca8b6531 a part in words holding a slash passed
# citation-our-code and was not listed as not checked, though nothing read it: twelve live
# parts so, `GET /api/v1/delivery/pipeline` of n-2918-routes and
# `skills/thermonuclear-code-quality/SKILL.md lines 22-23` of tc-our-2 among them. The case
# writes the fixture's row f-our-1 as citing skills/legion-worker/references/pr-body.md with
# the symbol 'one row per half', text that file holds, then 'the rows of
# skills/legion-worker/references/merge-gate.md', text it does not hold, running --citations
# over each: each must pass citation-our-code; the first must not be listed as not checked,
# and the second must be, by the line `not checked: f-our-1 cites the part "..."`.
for form in held unread; do
    case "$form" in
    held)
        symbol='one row per half' listed=0
        words_case="a symbol part in words that pr-body.md holds, 'one row per half', passes citation-our-code and is not listed as not checked"
        ;;
    unread)
        symbol='the rows of skills/legion-worker/references/merge-gate.md' listed=1
        words_case="a symbol part in words holding a path, which pr-body.md does not hold, is listed as not checked and fails nothing, never passed unread"
        ;;
    esac
    dir="$(symbol_plant "symbol-words-$form" skills/legion-worker/references/pr-body.md "$symbol")" || {
        printf 'FAIL the symbol %s could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    unchecked_result "$words_case" "$symbol" "$listed" "$out"
done

# 194. A quoted string is read as text its file holds, its quotes among it (the architect's
# class of round 49). No case held the form. The case commits
# packages/daemon/internal/api/mode.py, `MODE = 'strict'`, and writes the fixture's row f-our-1
# as citing that file with the symbol "'strict'", then "'MODE'", a name the file writes
# without quotes, running --citations over each: the first must pass citation-our-code; the
# second must fail it, naming the part.
for form in honest unquoted; do
    case "$form" in
    honest) symbol="'strict'" ;;
    unquoted) symbol="'MODE'" ;;
    esac
    dir="$(symbol_plant "symbol-quoted-$form" packages/daemon/internal/api/mode.py "$symbol" \
        packages/daemon/internal/api/mode.py "MODE = 'strict'"$'\n')" || {
        printf 'FAIL the symbol %s and its file could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    unquoted)
        case_result "a quoted symbol part 'MODE', a name mode.py writes without its quotes, fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol 'MODE', which packages/daemon/internal/api/mode\\.py does not hold" "$out"
        ;;
    honest) honest_result "a quoted symbol part 'strict', which mode.py holds with its quotes, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 195. A part that is a file's name beside the paths the citation names is that file, and the
# parts after it are read in it (the architect's class of round 49). Case 171 holds a file's
# name before a line's colon, `merge-gate.md:3`; no case held a file's name written as a part
# of its own, as the live row t-ml-cross-reference-docs writes `contracts.md`. The case writes
# the fixture's row f-our-1 as citing skills/legion-worker/references/pr-body.md, a file of
# three lines, with the symbol 'merge-gate.md, 2', then 'merge-gate.md, 3', merge-gate.md
# being a file of two lines beside it, running --citations over each: the first must pass
# citation-our-code; the second must fail it, naming line 3 and merge-gate.md, so line 3 is
# read in merge-gate.md and not in pr-body.md, which holds it.
for form in honest past-end; do
    case "$form" in
    honest) symbol='merge-gate.md, 2' ;;
    past-end) symbol='merge-gate.md, 3' ;;
    esac
    dir="$(symbol_plant "symbol-file-name-$form" skills/legion-worker/references/pr-body.md "$symbol")" &&
        [ "$(wc -l <"$dir/repo/skills/legion-worker/references/pr-body.md")" = 3 ] &&
        [ "$(wc -l <"$dir/repo/skills/legion-worker/references/merge-gate.md")" = 2 ] || {
        printf 'FAIL the symbol %s could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    past-end)
        case_result "a symbol 'merge-gate.md, 3' cited in pr-body.md reads line 3 in merge-gate.md, a file of two lines, and fails --citations, naming it" \
            "citation-our-code: f-our-1 cites 3, and skills/legion-worker/references/merge-gate\\.md holds no such line at [0-9a-f]+, 2 lines" "$out"
        ;;
    honest) honest_result "a symbol 'merge-gate.md, 2' cited in pr-body.md, a file's name beside it and a line that file holds, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 196. A part that is a file's name is read as a path at or below the paths the citation names
# that is the name or ends with it after a slash (the architect's class of round 49; case 201
# holds that no path elsewhere in the tree is read). Twelve live parts pass only so, as
# `engine.py` of t-cp-drop-packages, cited in the directory packages/ioc-concord/, and no
# case held the reading. The case writes the fixture's row f-our-1 as citing the directory
# packages/daemon with the symbol 'handoff.go', the name of
# packages/daemon/internal/api/handoff.go, which no file of the directory writes, then
# 'handof.go', running --citations over each: the first must pass citation-our-code; the
# second must fail it, naming the part and the directory.
for form in honest mistyped; do
    case "$form" in
    honest) symbol='handoff.go' ;;
    mistyped) symbol='handof.go' ;;
    esac
    dir="$(symbol_plant "symbol-tree-name-$form" packages/daemon "$symbol")" &&
        ! grep -rqF 'handoff.go' "$dir/repo/packages/daemon" || {
        printf 'FAIL the symbol %s could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    mistyped)
        case_result "a symbol part handof.go, cited in packages/daemon, the name of no file of the tree, fails --citations, naming the part and the directory" \
            "citation-our-code: f-our-1 cites the symbol handof\\.go, which packages/daemon does not hold" "$out"
        ;;
    honest) honest_result "a symbol part handoff.go, cited in packages/daemon, the name of a file below it that no file writes, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 197. A prefixed name whose prefix is a namespace, `Namespace:Symbol`, holds where its file
# holds the local part and each component of the prefix, and a prefix that names the file or
# a module holds the part to that file (the architect's class of round 49). The live rows
# ts-hold-key and ts-hold-actors write Turtle terms, `mas:sessionId`, and no case held that
# form; cases 183 and 188 hold `file.ext.Symbol` and `module:attr` passing, and no case held
# either failing. The case commits ontology/terms.ttl, a Turtle file that declares
# <https://example.org/mas#sessionId> under `@prefix mas:` and never writes mas:sessionId whole,
# and pkg/mod.py, a function func, and writes the fixture's row f-our-1 as citing both with
# the symbol 'mas:sessionId, pkg.mod:func, terms.ttl.sessionId', then 'mass:sessionId', then
# 'pkg.mod:fun', then 'terms.ttl.session', running --citations over each: the first must pass
# citation-our-code; each other must fail it, naming the component mass, the name fun in
# pkg/mod.py, or the local part session in ontology/terms.ttl.
for form in honest namespace module file; do
    case "$form" in
    honest) symbol='mas:sessionId, pkg.mod:func, terms.ttl.sessionId' ;;
    namespace) symbol='mass:sessionId' ;;
    module) symbol='pkg.mod:fun' ;;
    file) symbol='terms.ttl.session' ;;
    esac
    dir="$(symbol_plant "symbol-prefixed-$form" 'ontology/terms.ttl, pkg/mod.py' "$symbol" \
        ontology/terms.ttl $'@prefix mas: <https://example.org/mas#> .\n<https://example.org/mas#sessionId> a owl:DatatypeProperty .\n' \
        pkg/mod.py $'def func(row):\n    return row\n')" &&
        ! grep -qwE 'mass|fun|session' "$dir/repo/ontology/terms.ttl" "$dir/repo/pkg/mod.py" &&
        ! grep -qF 'mas:sessionId' "$dir/repo/ontology/terms.ttl" || {
        printf 'FAIL the symbol %s and its two files could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    namespace)
        case_result "a prefixed name mass:sessionId, whose namespace terms.ttl does not write, fails --citations, naming the component mass" \
            "citation-our-code: f-our-1 cites the symbol mass:sessionId, which ontology/terms\\.ttl, pkg/mod\\.py holds only by its local part 'sessionId', and the component 'mass' of its prefix 'mass'" "$out"
        ;;
    module)
        case_result "a prefixed name pkg.mod:fun, whose prefix names pkg/mod.py as a module that holds fun only inside func, fails --citations, naming fun" \
            "citation-our-code: f-our-1 cites the symbol pkg\\.mod:fun, whose prefix names the file pkg/mod\\.py as the module pkg\\.mod, and pkg/mod\\.py does not hold the name 'fun'" "$out"
        ;;
    file)
        case_result "a dotted name terms.ttl.session, whose prefix names ontology/terms.ttl, which holds session only inside sessionId, fails --citations, naming session" \
            "citation-our-code: f-our-1 cites the symbol terms\\.ttl\\.session, whose prefix names the file ontology/terms\\.ttl, and ontology/terms\\.ttl does not hold its local part 'session'" "$out"
        ;;
    honest) honest_result "symbol parts $symbol, a namespace, a module and a file each with a name its file holds, pass citation-our-code" citation-our-code "$out" ;;
    esac
done

# 198. A path is read as a path, a directory or a glob the commit holds (the architect's class
# of round 49). The live rows r-12, d-codeowners-skills and t-ml-drop-libraries cite
# `protocols/accord`, `skills/` and `src/mas/library/*`, and no case held the form. The case
# writes the fixture's row f-our-1, which cites packages/daemon/internal/api/handoff.go, with
# the symbol 'skills/legion-worker/references, skills/*/references', a directory and a glob
# the fixture holds, then 'skills/legion-reviewer/references', which it does not, running
# --citations over each: the first must pass citation-our-code; the second must fail it,
# naming the part.
for form in honest missing; do
    case "$form" in
    honest) symbol='skills/legion-worker/references, skills/*/references' ;;
    missing) symbol='skills/legion-reviewer/references' ;;
    esac
    dir="$(symbol_plant "symbol-path-$form" packages/daemon/internal/api/handoff.go "$symbol")" &&
        [ -d "$dir/repo/skills/legion-worker/references" ] && [ ! -e "$dir/repo/skills/legion-reviewer" ] || {
        printf 'FAIL the symbol %s could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    missing)
        case_result "a symbol part skills/legion-reviewer/references, a path the fixture does not hold, fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol skills/legion-reviewer/references, which packages/daemon/internal/api/handoff\\.go does not hold" "$out"
        ;;
    honest) honest_result "symbol parts $symbol, a directory and a glob the fixture holds, pass citation-our-code" citation-our-code "$out" ;;
    esac
done

# 199. A pattern holding `|`, a part that is no path, is read as an extended regular
# expression a line of its file matches (the architect's class of round 49; one holding `(`,
# `[` or `\` is one too, case 207, and any other holding `*` or `?` is a glob of names, cases
# 204, 205 and 206). The live row d-29 cites `consensus|majority|quorum`, and no case held
# the form. The case writes the fixture's row f-our-1, which cites
# packages/daemon/internal/api/handoff.go, with the symbol 'TesterVerdict(Required|Missing)',
# then 'TesterVerdict(Absent|Missing)', running --citations over each: the first must pass
# citation-our-code; the second must fail it, naming the part.
for form in honest unmatched; do
    case "$form" in
    honest) symbol='TesterVerdict(Required|Missing)' ;;
    unmatched) symbol='TesterVerdict(Absent|Missing)' ;;
    esac
    dir="$(symbol_plant "symbol-pattern-$form" packages/daemon/internal/api/handoff.go "$symbol")" || {
        printf 'FAIL the symbol %s could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    unmatched)
        case_result "a symbol part TesterVerdict(Absent|Missing), a pattern no line of handoff.go matches, fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol TesterVerdict\\(Absent\\|Missing\\), which packages/daemon/internal/api/handoff\\.go does not hold" "$out"
        ;;
    honest) honest_result "a symbol part TesterVerdict(Required|Missing), a pattern a line of handoff.go matches, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 200. A part in no form of the symbol grammar fails by name and is never read as text (the
# architect's class of round 49: a form the gate does not state is refused, not guessed at).
# resolve_symbol read every part that no other reading took as text its file holds, so at
# ca8b6531 `match(row)`, a call no form reads, passed citation-our-code in a file that writes
# `def match(row):`. The case commits packages/daemon/internal/api/engine.py, that function,
# pyproject.toml, the table [project.optional-dependencies], and lab.yaml, a list scenarios
# whose item holds overlays, and writes the fixture's row f-our-1 as citing the three with the
# symbol 'match, [project.optional-dependencies], scenarios[].overlays', a name, a table head
# (form 8; a key path of form 5 before round 50) and a key path written with brackets, then
# 'match(row)', then 'Engine::match', running --citations over each: the first must pass
# citation-our-code; each other must fail it, naming the part as not a symbol form the gate
# reads.
for form in honest call scope; do
    case "$form" in
    honest) symbol='match, [project.optional-dependencies], scenarios[].overlays' ;;
    call) symbol='match(row)' ;;
    scope) symbol='Engine::match' ;;
    esac
    dir="$(symbol_plant "symbol-no-form-$form" 'packages/daemon/internal/api/engine.py, pyproject.toml, lab.yaml' "$symbol" \
        packages/daemon/internal/api/engine.py $'def match(row):\n    return row\n' \
        pyproject.toml $'[project.optional-dependencies]\nneo4j = []\n' \
        lab.yaml $'scenarios:\n  - id: baseline\n    overlays: []\n')" &&
        grep -qF 'match(row)' "$dir/repo/packages/daemon/internal/api/engine.py" || {
        printf 'FAIL the symbol %s and its three files could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    call)
        case_result "a symbol part match(row), which engine.py writes but which is in no form of the symbol grammar, fails --citations, named as no symbol form the gate reads" \
            "citation-our-code: f-our-1 cites the symbol part 'match\\(row\\)', which is not a symbol form the gate reads \\(check\\.sh's header, the symbol grammar\\)" "$out"
        ;;
    scope)
        case_result "a symbol part Engine::match, in no form of the symbol grammar, fails --citations, named as no symbol form the gate reads" \
            "citation-our-code: f-our-1 cites the symbol part 'Engine::match', which is not a symbol form the gate reads \\(check\\.sh's header, the symbol grammar\\)" "$out"
        ;;
    honest) honest_result "symbol parts $symbol, a name, a table head and a key path written with brackets, pass citation-our-code" citation-our-code "$out" ;;
    esac
done

# 201. A file's name is read at or below the paths the citation names, and never anywhere else
# in the tree (the architect's ruling of round 49 on the readings the grammar kept: a part
# stands where the row cites it, not anywhere in the repository). At 1a24ac7d a part that is a
# file's name passed when any path of the tree ended with it, so 'handoff.go' cited in the
# directory skills passed citation-our-code on packages/daemon/internal/api/handoff.go, a file
# below no path the citation names. The case writes the fixture's row f-our-1 as citing the
# directories skills and packages/daemon with the symbol 'handoff.go', then the directory
# skills alone with the same symbol, running --citations over each: the first must pass
# citation-our-code; the second must fail it, naming the part and skills.
for form in honest elsewhere; do
    case "$form" in
    honest) cited='skills, packages/daemon' ;;
    elsewhere) cited='skills' ;;
    esac
    dir="$(symbol_plant "symbol-name-below-$form" "$cited" handoff.go)" &&
        [ -f "$dir/repo/packages/daemon/internal/api/handoff.go" ] &&
        [ -z "$(find "$dir/repo/skills" -name handoff.go)" ] &&
        ! grep -rqwE 'handoff|go' "$dir/repo/skills" || {
        printf 'FAIL the symbol handoff.go cited in %s could not be planted in the fixture row f-our-1\n' "$cited"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    elsewhere)
        case_result "a symbol part handoff.go cited in skills, a name only a file below packages/daemon holds, fails --citations, naming the part and skills" \
            "citation-our-code: f-our-1 cites the symbol handoff\\.go, which skills does not hold" "$out"
        ;;
    honest) honest_result "a symbol part handoff.go cited in skills and packages/daemon, a name a file below packages/daemon holds, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 202. A path's glob is read as paths the commit holds, from the root or below a directory the
# citation names, and never as a pattern over a file's text (the architect's ruling of round
# 49). At 1a24ac7d a path holding `*` that named no path was read as an extended regular
# expression over its file, so 'thermonuclear-deep-review/*', which names no path of the
# fixture, passed citation-our-code in skills/legion-worker/references/pr-body.md, whose text
# writes thermonuclear-deep-review, as the live row t-ml-drop-libraries's `src/mas/library/*`
# passed on the text of its files while it names paths below the directories it cites. The
# case writes the fixture's row f-our-1 as citing the directory skills with the symbol
# 'legion-worker/*, skills/*/references', a glob below skills and one from the root, then
# pr-body.md with the symbol 'thermonuclear-deep-review/*', running --citations over each: the
# first must pass citation-our-code; the second must fail it, naming the part.
for form in honest text; do
    case "$form" in
    honest) cited='skills' symbol='legion-worker/*, skills/*/references' ;;
    text) cited='skills/legion-worker/references/pr-body.md' symbol='thermonuclear-deep-review/*' ;;
    esac
    dir="$(symbol_plant "symbol-path-glob-$form" "$cited" "$symbol")" &&
        grep -qF 'thermonuclear-deep-review' "$dir/repo/skills/legion-worker/references/pr-body.md" &&
        [ -z "$(find "$dir/repo" -path '*thermonuclear-deep-review*')" ] || {
        printf 'FAIL the symbol %s cited in %s could not be planted in the fixture row f-our-1\n' "$symbol" "$cited"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    text)
        case_result "a symbol part thermonuclear-deep-review/*, a glob no path matches whose text pr-body.md writes, fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol thermonuclear-deep-review/\\*, which skills/legion-worker/references/pr-body\\.md does not hold" "$out"
        ;;
    honest) honest_result "symbol parts $symbol, a glob below the directory skills and one from the root, pass citation-our-code" citation-our-code "$out" ;;
    esac
done

# 203. A bare file's name is a file beside the paths the citation names or none, and only a
# full path, holding `/`, is read from the root of the tree (the architect's ruling of round
# 49: a part stands where the row cites it). At 1a24ac7d symbol_file read any name, last, as
# a path from the root, so 'NOTES.md:1' cited in skills/legion-worker/references/pr-body.md
# read line 1 of the root's NOTES.md, a file beside no path the citation names, and passed
# citation-our-code, as 'NOTES.md' did. The case commits NOTES.md and guide/NOTES.md, a line
# each, and writes the fixture's row f-our-1 as citing pr-body.md and the directory guide with
# the symbol 'guide/NOTES.md:1, NOTES.md', a full path from the root and a name in a cited
# directory, then as citing pr-body.md alone with 'NOTES.md:1', then 'NOTES.md', running
# --citations over each: the first must pass citation-our-code; each other must fail it,
# naming the part.
for form in honest line name; do
    case "$form" in
    honest) cited='skills/legion-worker/references/pr-body.md, guide' symbol='guide/NOTES.md:1, NOTES.md' ;;
    line) cited='skills/legion-worker/references/pr-body.md' symbol='NOTES.md:1' ;;
    name) cited='skills/legion-worker/references/pr-body.md' symbol='NOTES.md' ;;
    esac
    dir="$(symbol_plant "symbol-root-name-$form" "$cited" "$symbol" NOTES.md $'one note\n' guide/NOTES.md $'one guide note\n')" &&
        ! grep -qwE 'NOTES|md' "$dir/repo/skills/legion-worker/references/pr-body.md" || {
        printf 'FAIL the symbol %s cited in %s could not be planted in the fixture row f-our-1\n' "$symbol" "$cited"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    line)
        case_result "a symbol part NOTES.md:1 cited in pr-body.md, a name only the root and guide hold, fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites NOTES\\.md:1, and NOTES\\.md is neither a file beside the paths the citation names nor text the file before it" "$out"
        ;;
    name)
        case_result "a symbol part NOTES.md cited in pr-body.md, a name only the root and guide hold, fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol NOTES\\.md, which skills/legion-worker/references/pr-body\\.md does not hold" "$out"
        ;;
    honest) honest_result "symbol parts $symbol, a full path from the root and a name in the cited directory guide, pass citation-our-code" citation-our-code "$out" ;;
    esac
done

# 204. A pattern written as a glob of names holds only where a name it covers stands (test
# round 49, judged against the symbol grammar, form 8). The grammar's own example of a
# pattern, `GEN_AI_*`, and the live rows m-tel-semconv-module's `GEN_AI_*` and r-21's
# `gen_ai.*`, each name a set of names by a glob: the names that start with GEN_AI_, the
# attributes under gen_ai. The gate reads a pattern as an extended regular expression a line
# matches, as written, so at eb8cf706 `GEN_AI_*` (GEN_AI, then any number of `_`) held in a
# file whose only such text is GEN_AIRPORT, and `gen_ai.*` (gen_ai, then anything) in one
# whose only such text is gen_aix_total: in each the file holds no name the glob covers, and
# --citations passed. The case commits packages/daemon/internal/api/x.go into the fixture,
# names that commit as our repository's in sources.json, and writes the fixture's row
# f-our-1 as citing that file with each pattern, over a file that holds only the longer word
# and over one that holds a name the glob covers: the first two must fail citation-our-code,
# naming the part; the last two pass it.
for form in gen-ai-other gen-ai-dot-other gen-ai-honest gen-ai-dot-honest; do
    case "$form" in
    gen-ai-other) symbol='GEN_AI_*' body='package x\n\nconst GEN_AIRPORT = 1\n' ;;
    gen-ai-dot-other) symbol='gen_ai.*' body='package x\n\nconst metric = "gen_aix_total"\n' ;;
    gen-ai-honest) symbol='GEN_AI_*' body='package x\n\nconst GEN_AI_MODEL = 1\n' ;;
    gen-ai-dot-honest) symbol='gen_ai.*' body='package x\n\nconst attribute = "gen_ai.request.model"\n' ;;
    esac
    dir="$(fresh "symbol-glob-$form")"
    evidence_copy="$dir/repo/docs/research/internet-of-cognition"
    printf "$body" >"$dir/repo/packages/daemon/internal/api/x.go"
    named="$(
        cd "$dir/repo" &&
            git add packages/daemon/internal/api/x.go &&
            git commit --quiet -m "a file for a glob" &&
            git rev-parse HEAD
    )"
    jq --arg c "$named" '.our_repository.commit = $c' "$evidence_copy/sources.json" >"$dir/sources.json" &&
        mv "$dir/sources.json" "$evidence_copy/sources.json"
    jq -c --arg s "$symbol" 'if .id == "f-our-1" then .evidence.file = "packages/daemon/internal/api/x.go" | .evidence.symbol = $s else . end' \
        "$evidence_copy/claims.jsonl" >"$dir/claims.jsonl" && mv "$dir/claims.jsonl" "$evidence_copy/claims.jsonl"
    [ -n "$named" ] && jq -e --arg s "$symbol" 'select(.id == "f-our-1") | .evidence.symbol == $s and .evidence.file == "packages/daemon/internal/api/x.go"' "$evidence_copy/claims.jsonl" >/dev/null || {
        printf 'FAIL the pattern %s and its file could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    gen-ai-other)
        case_result "a glob GEN_AI_* cited in a file whose only such text is GEN_AIRPORT fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites .*GEN_AI_\\*" "$out"
        ;;
    gen-ai-dot-other)
        case_result "a glob gen_ai.* cited in a file whose only such text is gen_aix_total fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites .*gen_ai\\.\\*" "$out"
        ;;
    *) honest_result "a glob $symbol cited in a file that holds a name it covers passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 205. A glob with a `*` inside it covers a name that starts and ends as the glob does, a whole
# token (the tester's finding of round 49 on form 8, case 204). Read as an extended regular
# expression, `a*_total` is any number of a and then _total, which every name ending in
# _total holds, so at d75ea1dc it held in a file whose only such name is ba_requests_total,
# whose a follows a letter, or a_requests_totals, whose _total runs on: no name the glob
# covers. The case commits packages/daemon/internal/api/metrics.go, one name in a string, and
# writes the fixture's row f-our-1 as citing that file with the symbol 'a*_total' (symbol_plant)
# over a file holding a_requests_total, then ba_requests_total, then a_requests_totals, running
# --citations over each: the first must pass citation-our-code; the other two must fail it,
# naming the part.
for form in honest start end; do
    case "$form" in
    honest) name='a_requests_total' ;;
    start) name='ba_requests_total' ;;
    end) name='a_requests_totals' ;;
    esac
    dir="$(symbol_plant "symbol-glob-middle-$form" packages/daemon/internal/api/metrics.go 'a*_total' \
        packages/daemon/internal/api/metrics.go "package api"$'\n\n'"const metric = \"$name\""$'\n')" &&
        [ "$(grep -c '_total' "$dir/repo/packages/daemon/internal/api/metrics.go")" = 1 ] &&
        grep -qF "\"$name\"" "$dir/repo/packages/daemon/internal/api/metrics.go" || {
        printf 'FAIL the glob a*_total over %s could not be planted in the fixture row f-our-1\n' "$name"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    honest) honest_result "a glob a*_total cited in a file that holds a_requests_total, a name it covers, passes citation-our-code" citation-our-code "$out" ;;
    *)
        case_result "a glob a*_total cited in a file whose only name ending in _total is $name, which it does not cover, fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol a\\*_total, which packages/daemon/internal/api/metrics\\.go does not hold" "$out"
        ;;
    esac
done

# 206. A glob's `?` stands for exactly one character of a name (the architect's condition of
# round 50 on form 8: the part's text alone decides which patterns are globs and which are
# extended regular expressions). At d75ea1dc a part holding `?` and neither `|` nor `*`, as
# `v?_total`, was in no form of the grammar and failed by name as no symbol form, whatever
# its file held. The case commits packages/daemon/internal/api/metrics.go, one name in a
# string, and writes the fixture's row f-our-1 as citing that file with the symbol 'v?_total'
# (symbol_plant) over a file holding v1_total, then v10_total, whose run between v and _total
# is two characters, then xv1_total, whose v follows a letter, running --citations over
# each: the first must pass citation-our-code; the other two must fail it, naming the part.
for form in honest long start; do
    case "$form" in
    honest) name='v1_total' ;;
    long) name='v10_total' ;;
    start) name='xv1_total' ;;
    esac
    dir="$(symbol_plant "symbol-glob-one-$form" packages/daemon/internal/api/metrics.go 'v?_total' \
        packages/daemon/internal/api/metrics.go "package api"$'\n\n'"const metric = \"$name\""$'\n')" &&
        [ "$(grep -c '_total' "$dir/repo/packages/daemon/internal/api/metrics.go")" = 1 ] &&
        grep -qF "\"$name\"" "$dir/repo/packages/daemon/internal/api/metrics.go" || {
        printf 'FAIL the glob v?_total over %s could not be planted in the fixture row f-our-1\n' "$name"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    honest) honest_result "a glob v?_total cited in a file that holds v1_total, a name it covers, passes citation-our-code" citation-our-code "$out" ;;
    *)
        case_result "a glob v?_total cited in a file whose only name ending in _total is $name, which it does not cover, fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol v\\?_total, which packages/daemon/internal/api/metrics\\.go does not hold" "$out"
        ;;
    esac
done

# 207. A pattern holding `(`, `[` or `\` is an extended regular expression, read as written,
# and no glob, though it holds `*` or `?` (the architect's condition of round 50 on form 8).
# `v(1)?_total` and `v[0-9]?_total` each name a v, an optional digit and _total; read as
# globs, they would ask for the text `v(1)` or `v[0-9]` and hold in no file that writes only
# v1_total. `\bv1_total\b` holds neither `|`, `*` nor `?`, and is a pattern by its `\`. At
# d75ea1dc each was in no form of the grammar and failed by name as no symbol form. The case
# commits packages/daemon/internal/api/metrics.go, one name in a string, and writes the
# fixture's row f-our-1 as citing that file (symbol_plant; jq writes the symbol, whose `\`
# the plant's sed would read) with 'v(1)?_total, v[0-9]?_total, \bv1_total\b' over a file
# holding v1_total, then with 'v(1)?_total' and with 'v[0-9]?_total' over one holding
# vx_total, then with '\bv1_total\b' over one holding xv1_total, whose v follows a letter,
# running --citations over each: the first must pass citation-our-code; each other must fail
# it, naming the part.
for form in honest paren bracket backslash; do
    case "$form" in
    honest) symbol='v(1)?_total, v[0-9]?_total, \bv1_total\b' name='v1_total' ;;
    paren) symbol='v(1)?_total' name='vx_total' detail='v\(1\)\?_total' ;;
    bracket) symbol='v[0-9]?_total' name='vx_total' detail='v\[0-9\]\?_total' ;;
    backslash) symbol='\bv1_total\b' name='xv1_total' detail='\\bv1_total\\b' ;;
    esac
    dir="$(symbol_plant "symbol-pattern-ere-$form" packages/daemon/internal/api/metrics.go metric \
        packages/daemon/internal/api/metrics.go "package api"$'\n\n'"const metric = \"$name\""$'\n')" &&
        evidence_copy="$dir/repo/docs/research/internet-of-cognition" &&
        jq -c --arg s "$symbol" 'if .id == "f-our-1" then .evidence.symbol = $s else . end' \
            "$evidence_copy/claims.jsonl" >"$dir/claims.jsonl" && mv "$dir/claims.jsonl" "$evidence_copy/claims.jsonl" &&
        jq -e --arg s "$symbol" 'select(.id == "f-our-1") | .evidence.symbol == $s' "$evidence_copy/claims.jsonl" >/dev/null &&
        [ "$(grep -c '_total' "$dir/repo/packages/daemon/internal/api/metrics.go")" = 1 ] &&
        grep -qF "\"$name\"" "$dir/repo/packages/daemon/internal/api/metrics.go" || {
        printf 'FAIL the pattern %s over %s could not be planted in the fixture row f-our-1\n' "$symbol" "$name"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    honest) honest_result "patterns $symbol, extended regular expressions a line holding v1_total matches, pass citation-our-code" citation-our-code "$out" ;;
    *)
        case_result "a pattern $symbol cited in a file whose only name ending in _total is $name, which it does not match, fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol $detail, which packages/daemon/internal/api/metrics\\.go does not hold" "$out"
        ;;
    esac
done

# 208. A part that begins with `[` and ends with `]` is a table head, and holds only on a line
# that is that head, its inside read as a glob (the architect's ruling of round 50: `[tool.*]`
# was a false pass). An author cites the tables under [tool] of a pyproject.toml as
# `[tool.*]`; at aec374fe the part held `[`, so it was read as an extended regular
# expression, a bracket expression that matches any line holding t, o, l, `.` or `*`, and it
# passed in a file with no such head. The case commits pyproject.toml and writes the
# fixture's row f-our-1 as citing it with the symbol '[tool.*]' (symbol_plant) over a file
# holding the head [tool.ruff], then over one whose only head is [project], then over one
# whose only such text is the dotted key tool.x, with no brackets, running --citations over
# each: the first must pass citation-our-code; the other two must fail it, naming the part.
for form in honest no-head dotted; do
    case "$form" in
    honest) text=$'[project]\nname = "x"\n\n[tool.ruff]\nline-length = 100\n' ;;
    no-head) text=$'[project]\nname = "x"\n' ;;
    dotted) text=$'tool.x = 1\n' ;;
    esac
    dir="$(symbol_plant "symbol-table-head-glob-$form" pyproject.toml '[tool.*]' pyproject.toml "$text")" &&
        case "$form" in
        honest) grep -qx '\[tool\.ruff\]' "$dir/repo/pyproject.toml" ;;
        no-head) ! grep -q '^\[tool' "$dir/repo/pyproject.toml" ;;
        dotted) ! grep -q '\[' "$dir/repo/pyproject.toml" && grep -qF 'tool.x' "$dir/repo/pyproject.toml" ;;
        esac || {
        printf 'FAIL the table head [tool.*] could not be planted in the fixture row f-our-1 over a %s pyproject.toml\n' "$form"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    honest) honest_result "a table head [tool.*] cited in a pyproject.toml whose line [tool.ruff] is a head it covers passes citation-our-code" citation-our-code "$out" ;;
    no-head)
        case_result "a table head [tool.*] cited in a pyproject.toml whose only head is [project] fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol \\[tool\\.\\*\\], which pyproject\\.toml does not hold" "$out"
        ;;
    dotted)
        case_result "a table head [tool.*] cited in a pyproject.toml whose only such text is tool.x, with no brackets, fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol \\[tool\\.\\*\\], which pyproject\\.toml does not hold" "$out"
        ;;
    esac
done

# 209. A table head with no glob in it holds only on a line that is that head, after spaces or
# tabs and before spaces, tabs and a comment, and never as text inside a line (the
# architect's ruling of round 50). At aec374fe `[project]` was a key path of form 5, read as
# text its file holds, so it passed in a file that writes `[project]` only in a comment. The
# case commits pyproject.toml and writes the fixture's row f-our-1 as citing it with the
# symbol '[project]' (symbol_plant) over a file holding the head [project], then the head
# indented with a comment after it, then the head [projects] alone, then [project] only
# inside the comment `# the [project] table is below`, running --citations over each: the
# first two must pass citation-our-code; the other two must fail it, naming the part.
for form in honest commented longer inline; do
    case "$form" in
    honest) text=$'[project]\nname = "x"\n' ;;
    commented) text=$'  [project]  # metadata\nname = "x"\n' ;;
    longer) text=$'[projects]\nname = "x"\n' ;;
    inline) text=$'# the [project] table is below\n[tool.ruff]\nline-length = 100\n' ;;
    esac
    dir="$(symbol_plant "symbol-table-head-$form" pyproject.toml '[project]' pyproject.toml "$text")" &&
        case "$form" in
        honest | commented) grep -qE '^[[:space:]]*\[project\]' "$dir/repo/pyproject.toml" ;;
        *) ! grep -qE '^[[:space:]]*\[project\]' "$dir/repo/pyproject.toml" ;;
        esac || {
        printf 'FAIL the table head [project] could not be planted in the fixture row f-our-1 over a %s pyproject.toml\n' "$form"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    honest) honest_result "a table head [project] cited in a pyproject.toml whose line [project] is that head passes citation-our-code" citation-our-code "$out" ;;
    commented) honest_result "a table head [project] cited in a pyproject.toml whose line is that head indented, with a comment after it, passes citation-our-code" citation-our-code "$out" ;;
    longer)
        case_result "a table head [project] cited in a pyproject.toml whose only head is [projects] fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol \\[project\\], which pyproject\\.toml does not hold" "$out"
        ;;
    inline)
        case_result "a table head [project] cited in a pyproject.toml that writes it only inside a comment fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol \\[project\\], which pyproject\\.toml does not hold" "$out"
        ;;
    esac
done

# 210. A module below a directory the citation names is a Python file, a .py file or a .pyi
# stub, and never a file of another extension whose stem the run names (review 6031558230,
# its thread at check.sh's module_below). module_below took the first file below the
# directory whose name without its last extension was the run's last component, so at
# ef564aae, with lib/core/schema.json beside lib/core/schema.py, `core.schema.load` was read
# in schema.json, which git ls-tree lists first, and failed though schema.py declares load,
# and `core.schema.notes` passed on schema.json's text. The case commits lib/core/schema.py,
# a function load, lib/core/schema.json, a key notes, and lib/core/stub.pyi, the stub of a
# function check, their text writing none of lib, core, schema and stub as a word, and writes
# the row f-our-1 as citing the directory lib with the symbol 'core.schema.load,
# core.stub.check', then 'core.schema.notes', then, over lib/core/schema.json alone, a key
# load, 'core.schema.load', running --citations over each (symbol_plant): the first must pass
# citation-our-code; the second, read in schema.py, which does not hold notes, must fail it,
# naming notes and schema.py; and the third, whose run names no Python file, is read as a
# qualified name and must fail it, naming the component core. At ef564aae the first failed,
# naming load and schema.json, and the other two passed.
for form in honest json-name json-only; do
    case "$form" in
    honest | json-name)
        module_files=(lib/core/schema.py $'def load(row):\n    return row\n' lib/core/schema.json '{"notes": 1}'
            lib/core/stub.pyi $'def check(row: int) -> int: ...\n')
        ;;
    json-only) module_files=(lib/core/schema.json '{"load": 1}') ;;
    esac
    case "$form" in
    honest) symbol='core.schema.load, core.stub.check' ;;
    json-name) symbol='core.schema.notes' ;;
    json-only) symbol='core.schema.load' ;;
    esac
    dir="$(symbol_plant "symbol-module-extension-$form" lib "$symbol" "${module_files[@]}")" &&
        ! grep -rqwE 'lib|core|schema|stub' "$dir/repo/lib" || {
        printf 'FAIL the symbol %s and the files below lib could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    json-name)
        case_result "a dotted name core.schema.notes, cited in lib, where only lib/core/schema.json beside lib/core/schema.py holds notes, is read in schema.py and fails --citations, naming notes" \
            "citation-our-code: f-our-1 cites the symbol core\\.schema\\.notes, whose prefix names the file lib/core/schema\\.py as the module core\\.schema, and lib/core/schema\\.py does not hold the name 'notes'" "$out"
        ;;
    json-only)
        case_result "a dotted name core.schema.load, cited in lib, where lib/core/schema.json is the only file of that stem, names no module and fails --citations, naming the component core" \
            "citation-our-code: f-our-1 cites the symbol core\\.schema\\.load, which lib holds only by its local part 'load', and the component 'core' of its prefix 'core\\.schema'" "$out"
        ;;
    honest) honest_result "dotted names $symbol, cited in lib, read in lib/core/schema.py beside lib/core/schema.json and in the stub lib/core/stub.pyi, pass citation-our-code" citation-our-code "$out" ;;
    esac
done

# 211. A file the citation names is a module only when it is a Python file, a .py file or a
# .pyi stub, and a .py comes before a .pyi of its name (review 6031558230, the class of case
# 210: a reader that picks a file by its stem holds the extension its form names).
# module_names read any file's name without its last extension as a module, so at ef564aae
# `schema.load`, cited in pkg/schema.json, a file holding only {"load": 1}, read schema.json
# as the module schema and passed citation-our-code on the key load, though nothing in the
# file is schema; and with pkg/schema.pyi named before pkg/schema.py, `schema.notes` was read
# in the stub, which Python's import does not take, and passed on a name schema.py does not
# hold. The case writes the row f-our-1 as citing pkg/schema.json holding {"load": 1} with the
# symbol 'schema.load', then pkg/schema.json holding {"schema": {"load": 1}} with the same
# symbol, then pkg/schema.py, a function load, with the same symbol, then 'pkg/schema.pyi,
# pkg/schema.py', the stub declaring load and notes and the module load alone, with
# 'schema.notes', running --citations over each (symbol_plant): the first, read as a
# qualified name, must fail citation-our-code, naming the component schema; the second and
# the third must pass it; and the fourth, read in schema.py, must fail it, naming notes. At
# ef564aae the first and the fourth passed.
for form in json json-namespace module stub-first; do
    case "$form" in
    json) cited='pkg/schema.json' symbol='schema.load' module_files=(pkg/schema.json '{"load": 1}') ;;
    json-namespace) cited='pkg/schema.json' symbol='schema.load' module_files=(pkg/schema.json '{"schema": {"load": 1}}') ;;
    module) cited='pkg/schema.py' symbol='schema.load' module_files=(pkg/schema.py $'def load(row):\n    return row\n') ;;
    stub-first)
        cited='pkg/schema.pyi, pkg/schema.py' symbol='schema.notes'
        module_files=(pkg/schema.pyi $'def load(row: int) -> int: ...\ndef notes() -> str: ...\n' pkg/schema.py $'def load(row):\n    return row\n')
        ;;
    esac
    dir="$(symbol_plant "symbol-module-named-$form" "$cited" "$symbol" "${module_files[@]}")" &&
        case "$form" in
        json-namespace) grep -qw schema "$dir/repo/pkg/schema.json" ;;
        *) ! grep -rqw schema "$dir/repo/pkg" ;;
        esac || {
        printf 'FAIL the symbol %s cited in %s could not be planted in the fixture row f-our-1\n' "$symbol" "$cited"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    json)
        case_result "a dotted name schema.load, cited in pkg/schema.json, a file holding only the key load, names no module and fails --citations, naming the component schema" \
            "citation-our-code: f-our-1 cites the symbol schema\\.load, which pkg/schema\\.json holds only by its local part 'load', and the component 'schema' of its prefix 'schema'" "$out"
        ;;
    stub-first)
        case_result "a dotted name schema.notes, cited in pkg/schema.pyi and pkg/schema.py, where only the stub holds notes, is read in schema.py and fails --citations, naming notes" \
            "citation-our-code: f-our-1 cites the symbol schema\\.notes, whose prefix names the file pkg/schema\\.py as the module schema, and pkg/schema\\.py does not hold the name 'notes'" "$out"
        ;;
    *) honest_result "a dotted name schema.load, cited in $cited, which holds load under schema or is the module schema, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 212. A part that starts with a digit and is no line is read by every reading of its form that
# reads it as a file or as text its file holds, and fails as no line only when none holds it
# (review 6031558230, its thread at check.sh's resolve_symbol). resolve_symbol failed such a
# part as no line right after it read the part as text, before it read it as a file's name at
# or below the cited paths (5c) or as a path the commit holds (7), so at ef564aae, cited in
# docs, `2024-report.md` failed though docs/deep/r/2024-report.md stands, while
# `y2024-report.md` in its place held, and `2024/` failed though docs/2024/ stands. The case
# commits notes/deep/r/2024-report.md and notes/2024/x.md, neither writing 2024, in the
# directory notes, which holds nothing else (the fixture's docs holds the gate, whose text
# names these parts), and writes the row f-our-1 as citing notes with the symbol
# '2024-report.md', then '2024/', then '2024-missing.md', a name no path below notes ends
# with, running --citations over each (symbol_plant): the first two must pass
# citation-our-code; the third must fail it, naming the part as no line or line range the gate
# reads. At ef564aae the first two failed so.
for form in name directory missing; do
    case "$form" in
    name) symbol='2024-report.md' named='notes/deep/r/2024-report.md' ;;
    directory) symbol='2024/' named='notes/2024/' ;;
    missing) symbol='2024-missing.md' named='' ;;
    esac
    dir="$(symbol_plant "symbol-digit-path-$form" notes "$symbol" notes/deep/r/2024-report.md $'# The yearly report\n' notes/2024/x.md $'one note\n')" &&
        ! git -C "$dir/repo" grep -qF 2024 HEAD -- notes || {
        printf 'FAIL the symbol %s cited in notes could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    missing)
        case_result "a symbol part 2024-missing.md cited in notes, a name no path below notes ends with, fails --citations, named as no line or line range the gate reads" \
            "citation-our-code: f-our-1 cites 2024-missing\\.md, which starts with a digit and is no line or line range this gate reads" "$out"
        ;;
    *) honest_result "a symbol part $symbol cited in notes, which names $named, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 213. A part that starts with a digit is read as a file or as a module by a prefixed name and
# by a qualified name, as any other part is (review 6031558230, the class of case 212: no
# refusal runs before a reading that can hold the part). At ef564aae such a part failed as no
# line before 6 read it as `file:Symbol` or `module:attr` and before 5d read it as
# `file.ext.Symbol` or `module.symbol`, so `2024-report.md:Title`, `2024-report.md.Title`,
# `2024_report:main` and `2024_report.main`, cited in notes, each failed, though
# notes/2024-report.md holds Title and notes/2024_report.py declares main. The case commits
# those two files, neither writing 2024, in the directory notes, which holds nothing else, and
# writes the row f-our-1 as citing notes with each of the four symbols, then with
# '2024-missing.md:Title', whose prefix names no file and no module, running --citations over
# each (symbol_plant): the four must pass citation-our-code; the last must fail it, naming the
# part as no line or line range the gate reads, since a namespace reading never holds such a
# part (case 215). At ef564aae the four failed so.
for form in file-colon file-dot module-colon module-dot missing; do
    case "$form" in
    file-colon) symbol='2024-report.md:Title' ;;
    file-dot) symbol='2024-report.md.Title' ;;
    module-colon) symbol='2024_report:main' ;;
    module-dot) symbol='2024_report.main' ;;
    missing) symbol='2024-missing.md:Title' ;;
    esac
    dir="$(symbol_plant "symbol-digit-file-$form" notes "$symbol" notes/2024-report.md $'# Title\n' notes/2024_report.py $'def main():\n    return 1\n')" &&
        ! git -C "$dir/repo" grep -qF 2024 HEAD -- notes || {
        printf 'FAIL the symbol %s cited in notes could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    missing)
        case_result "a symbol part 2024-missing.md:Title cited in notes, whose prefix names no file and no module, fails --citations, named as no line or line range the gate reads" \
            "citation-our-code: f-our-1 cites 2024-missing\\.md:Title, which starts with a digit and is no line or line range this gate reads" "$out"
        ;;
    *) honest_result "a symbol part $symbol cited in notes, whose prefix names notes/2024-report.md or notes/2024_report.py, which holds its name, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 214. A pattern that starts with a digit is read by the rules of 8, as any other pattern is
# (review 6031558230, the class of case 212). At ef564aae such a part failed as no line before
# 8 read it, so `2024-*`, a glob of names, failed in a file that holds 2024-11-23, a name it
# covers, and so did `2023|2024`, an extended regular expression a line of that file matches.
# The case commits docs/notes.md and writes the row f-our-1 as citing it with the symbol
# '2024-*', then '2023|2024', over a file holding 2024-11-23, then '2024-*' over one whose only
# such text is x2024-11, whose 2024 follows a letter, running --citations over each
# (symbol_plant): the first two must pass citation-our-code; the last must fail it, naming the
# part as no line or line range the gate reads. At ef564aae the first two failed so.
for form in glob regex glob-other; do
    case "$form" in
    glob) symbol='2024-*' text=$'released on 2024-11-23\n' ;;
    regex) symbol='2023|2024' text=$'released on 2024-11-23\n' ;;
    glob-other) symbol='2024-*' text=$'build x2024-11\n' ;;
    esac
    dir="$(symbol_plant "symbol-digit-pattern-$form" docs/notes.md "$symbol" docs/notes.md "$text")" &&
        [ "$(grep -c 2024 "$dir/repo/docs/notes.md")" = 1 ] || {
        printf 'FAIL the pattern %s could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    glob-other)
        case_result "a glob 2024-* cited in a file whose only such text is x2024-11 fails --citations, named as no line or line range the gate reads" \
            "citation-our-code: f-our-1 cites 2024-\\*, which starts with a digit and is no line or line range this gate reads" "$out"
        ;;
    *) honest_result "a pattern $symbol cited in a file that holds 2024-11-23 passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 215. A namespace reading never holds a part that starts with a digit (the architect's ruling
# on review 6031558230: such a part is read by every reading of its form that reads it as a
# file or as text its file holds, and a namespace reading reads it as neither). The namespace
# reading of a qualified name holds where its file holds the local part and each component of
# its prefix as words (prefix_held), so read so, `3.14`, a version, would hold in a file that
# holds 3 and 14 apart, and `12a:b` in one that holds 12a and b apart. The case commits
# docs/notes.md and writes the row f-our-1 as citing it with the symbol '3.14' over a file
# holding `version 3 lists 14 items`, then '12a:b' over one holding `row 12a of table b`, then
# '3.14' over one holding `version 3.14`, running --citations over each (symbol_plant): the
# first two must fail citation-our-code, naming the part as no line or line range the gate
# reads; the last must pass it. ef564aae did the same, since it failed such a part before any
# reading after its text; the case holds that the readings after its text, which never read
# such a part as a namespace, hold neither of the first two.
for form in version prefixed honest; do
    case "$form" in
    version) symbol='3.14' text=$'version 3 lists 14 items\n' detail='3\.14' ;;
    prefixed) symbol='12a:b' text=$'row 12a of table b\n' detail='12a:b' ;;
    honest) symbol='3.14' text=$'version 3.14\n' detail='' ;;
    esac
    dir="$(symbol_plant "symbol-digit-namespace-$form" docs/notes.md "$symbol" docs/notes.md "$text")" &&
        case "$form" in
        version) grep -qw 3 "$dir/repo/docs/notes.md" && grep -qw 14 "$dir/repo/docs/notes.md" && ! grep -qF 3.14 "$dir/repo/docs/notes.md" ;;
        prefixed) grep -qw 12a "$dir/repo/docs/notes.md" && grep -qw b "$dir/repo/docs/notes.md" && ! grep -qF 12a:b "$dir/repo/docs/notes.md" ;;
        honest) grep -qF 3.14 "$dir/repo/docs/notes.md" ;;
        esac || {
        printf 'FAIL the symbol %s could not be planted in the fixture row f-our-1 over a %s docs/notes.md\n' "$symbol" "$form"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    honest) honest_result "a symbol part 3.14 cited in a file that holds 3.14 passes citation-our-code" citation-our-code "$out" ;;
    *)
        case_result "a symbol part $symbol cited in a file that holds each component apart and not $symbol fails --citations, named as no line or line range the gate reads" \
            "citation-our-code: f-our-1 cites $detail, which starts with a digit and is no line or line range this gate reads" "$out"
        ;;
    esac
done

# 216. A glob in a file field or in a symbol part names a path whose characters are the glob's,
# its `*` any run within one path element (found while the class of review 6031558230 was
# fixed: a reader that turns a cited path into a pattern quotes every character it does not
# read). paths_at turned a glob into an extended regular expression with only `.` quoted, so
# `+`, `?`, `(`, `[`, `{`, `|`, `^` and `$` acted as its own characters: at ef564aae
# `src/c++/*`, read as src/, one c or more, then a path element, named src/cc/x.h and not
# src/c++/x.h. The case commits src/c++/x.h, then, in another fixture, src/cc/x.h alone, each
# declaring CPP_HEADER, and writes the row f-our-1 as citing the file field 'src/c++/*' with
# no symbol, as live rows cite a file alone, then as citing
# skills/legion-worker/references/pr-body.md with the symbol 'src/c++/*' (symbol_plant),
# running --citations over each: over src/c++/x.h both must pass citation-our-code; over
# src/cc/x.h both must fail it, the first naming the field as a path the repository does not
# hold, the second naming the part. At ef564aae each form read the wrong path: the two over
# src/c++/x.h failed and the two over src/cc/x.h passed.
for form in field part field-other part-other; do
    case "$form" in
    field | field-other) cited='src/c++/*' symbol='' ;;
    part | part-other) cited='skills/legion-worker/references/pr-body.md' symbol='src/c++/*' ;;
    esac
    case "$form" in
    field | part) header='src/c++/x.h' ;;
    *) header='src/cc/x.h' ;;
    esac
    dir="$(symbol_plant "symbol-path-glob-chars-$form" "$cited" "$symbol" "$header" $'#define CPP_HEADER 1\n')" &&
        [ -f "$dir/repo/$header" ] && [ "$(find "$dir/repo/src" -name x.h | wc -l)" = 1 ] &&
        ! grep -qF 'src/c++' "$dir/repo/skills/legion-worker/references/pr-body.md" || {
        printf 'FAIL the citation of %s with the symbol %s over %s could not be planted in the fixture row f-our-1\n' "$cited" "$symbol" "$header"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    field-other)
        case_result "a file field src/c++/* over a tree whose only header is src/cc/x.h fails --citations, naming the field" \
            "citation-our-code: f-our-1 cites src/c\\+\\+/\\*, which this repository does not hold" "$out"
        ;;
    part-other)
        case_result "a symbol part src/c++/* over a tree whose only header is src/cc/x.h fails --citations, naming the part" \
            "citation-our-code: f-our-1 cites the symbol src/c\\+\\+/\\*, which skills/legion-worker/references/pr-body\\.md does not hold" "$out"
        ;;
    *) honest_result "a citation of src/c++/* as its $form over a tree holding src/c++/x.h passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 217. A part in words holding a colon is read by the readings of a prefixed name (6), which
# can hold it and never fail it (the architect's ruling on review 6031558230, the class of case
# 212: no refusal runs before a reading that can hold the part). Form 2 reads such a part by 6,
# else by 8, else not at all, but resolve_symbol failed it wherever the reading of 6 that
# applied did not hold it, so at ef564aae `deploy: the replica count`, cited in a directory that
# holds deploy.py, failed through the module reading, while the same part where no deploy.py
# stands is not checked; `deploy.py: the replica count` failed naming the file; and
# `cluster: the replica count` failed naming the component cluster. The case commits
# notes/deploy.py, a function whose text writes ` replicas`, and notes/values.md, which holds
# ` the replica count` and neither file writes cluster, and writes the row f-our-1 as citing
# notes with each of those three symbols, then with 'deploy: replicas', which the module
# deploy.py holds, running --citations over each (symbol_plant): each must pass
# citation-our-code; the first three must be listed as not checked, by the line
# `not checked: f-our-1 cites the part "..."`, and the last must not be. At ef564aae the first
# three failed citation-our-code.
for form in module file namespace held; do
    case "$form" in
    module) symbol='deploy: the replica count' listed=1 ;;
    file) symbol='deploy.py: the replica count' listed=1 ;;
    namespace) symbol='cluster: the replica count' listed=1 ;;
    held) symbol='deploy: replicas' listed=0 ;;
    esac
    case "$listed" in
    1) words_case="a symbol part in words '$symbol', cited in notes, whose reading of a prefixed name does not hold it, is listed as not checked and fails nothing" ;;
    *) words_case="a symbol part in words '$symbol', cited in notes, which the module notes/deploy.py holds, passes citation-our-code and is not listed as not checked" ;;
    esac
    dir="$(symbol_plant "symbol-words-prefixed-$form" notes "$symbol" notes/deploy.py $'def scale(replicas):\n    return replicas\n' notes/values.md $'# the replica count is three\n')" &&
        ! grep -rqw cluster "$dir/repo/notes" && ! grep -qF 'the replica count' "$dir/repo/notes/deploy.py" &&
        grep -qF ' the replica count' "$dir/repo/notes/values.md" && grep -qF ' replicas' "$dir/repo/notes/deploy.py" || {
        printf 'FAIL the symbol %s cited in notes could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    unchecked_result "$words_case" "$symbol" "$listed" "$out"
done

# 218. A cited path is read as its characters stand, and a glob's characters but `*` too, by
# every reading of the citation's files (found while the class of review 6031558230 was fixed:
# paths_at and paths_below agree with git on what a cited path names). resolve_symbol handed git
# the paths of the file field as pathspecs, which git reads with `?` and `[` as wildcards, and
# a glob by git's glob magic, which reads `?`, `[` and `\` so too, so at ef564aae, with
# notes/a[bc].md beside notes/ab.md, a name only ab.md holds passed in a citation of
# notes/a[bc].md, as it did in one of notes/a?.md, and a name only a[bc].md holds failed in a
# citation of the glob notes/a[bc]*.md, while one only ab.md holds passed there. The case
# commits notes/a[bc].md, notes/a?.md and notes/a[bc]1.md, each holding ALPHA_X, and
# notes/ab.md and notes/ab1.md, each holding BETA_Y, and writes the row f-our-1 as citing
# notes/a[bc].md with the symbol 'ALPHA_X', then 'BETA_Y', then notes/a?.md with 'BETA_Y', then
# notes/a[bc]*.md with 'ALPHA_X', then 'BETA_Y', running --citations over each (symbol_plant):
# the two citing ALPHA_X must pass citation-our-code; each citing BETA_Y must fail it, naming
# the part and the cited path. At ef564aae every form but the first did the opposite.
for form in plain plain-other question-other glob glob-other; do
    case "$form" in
    plain) cited='notes/a[bc].md' symbol='ALPHA_X' ;;
    plain-other) cited='notes/a[bc].md' symbol='BETA_Y' detail='notes/a\[bc\]\.md' ;;
    question-other) cited='notes/a?.md' symbol='BETA_Y' detail='notes/a\?\.md' ;;
    glob) cited='notes/a[bc]*.md' symbol='ALPHA_X' ;;
    glob-other) cited='notes/a[bc]*.md' symbol='BETA_Y' detail='notes/a\[bc\]\*\.md' ;;
    esac
    dir="$(symbol_plant "symbol-path-literal-$form" "$cited" "$symbol" 'notes/a[bc].md' $'ALPHA_X = 1\n' 'notes/a?.md' $'ALPHA_X = 1\n' \
        'notes/a[bc]1.md' $'ALPHA_X = 1\n' notes/ab.md $'BETA_Y = 2\n' notes/ab1.md $'BETA_Y = 2\n')" &&
        [ "$(git -C "$dir/repo" ls-tree -r --name-only HEAD -- notes | wc -l)" = 5 ] &&
        [ "$(grep -rl BETA_Y "$dir/repo/notes" | wc -l)" = 2 ] || {
        printf 'FAIL the citation of %s with the symbol %s could not be planted in the fixture row f-our-1\n' "$cited" "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    *-other)
        case_result "a symbol part BETA_Y cited in $cited, a name only notes/ab.md and notes/ab1.md hold, fails --citations, naming the part and the path" \
            "citation-our-code: f-our-1 cites the symbol BETA_Y, which $detail does not hold" "$out"
        ;;
    *) honest_result "a symbol part ALPHA_X cited in $cited, a name the file of that name holds, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 219. A glob names in git what paths_at reads it to name: each `*`, or run of `*`, within one
# path element, and every path below a directory it names (found while the class of review
# 6031558230 was fixed). paths_at reads `notes/*` as every path below notes, a directory among
# them, and `notes/**/x.md` as notes/<one element>/x.md, while git's glob magic matched no file
# below a directory `notes/*` names and read `**/` as any number of directories, so at
# ef564aae, with notes/a/x.md holding ALPHA_X, notes/a/b/x.md holding BETA_Y and other/y.md
# holding DELTA_Z, ALPHA_X failed in a citation of notes/*, though paths_at found that citation
# by notes/a/x.md, and BETA_Y passed in one of notes/**/x.md, which paths_at reads as never
# naming notes/a/b/x.md, as the live row t-cp-drop-scripts's protocols/*/skills/*/scripts/
# searched no file of the directories it names. The case commits those three files and writes
# the row f-our-1 as citing notes/* with the symbol 'ALPHA_X', then 'DELTA_Z', then
# notes/**/x.md with 'ALPHA_X', then 'BETA_Y', running --citations over each (symbol_plant):
# each citing ALPHA_X must pass citation-our-code; the other two must fail it, naming the part
# and the cited glob. At ef564aae the first failed and the last passed.
for form in directory directory-other star star-other; do
    case "$form" in
    directory) cited='notes/*' symbol='ALPHA_X' ;;
    directory-other) cited='notes/*' symbol='DELTA_Z' detail='notes/\*' ;;
    star) cited='notes/**/x.md' symbol='ALPHA_X' ;;
    star-other) cited='notes/**/x.md' symbol='BETA_Y' detail='notes/\*\*/x\.md' ;;
    esac
    dir="$(symbol_plant "symbol-glob-depth-$form" "$cited" "$symbol" notes/a/x.md $'ALPHA_X = 1\n' notes/a/b/x.md $'BETA_Y = 2\n' other/y.md $'DELTA_Z = 3\n')" &&
        [ "$(git -C "$dir/repo" ls-tree --name-only HEAD notes/)" = notes/a ] || {
        printf 'FAIL the citation of %s with the symbol %s could not be planted in the fixture row f-our-1\n' "$cited" "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    *-other)
        case_result "a symbol part $symbol cited in $cited, a name only a path the glob does not name holds, fails --citations, naming the part and the glob" \
            "citation-our-code: f-our-1 cites the symbol $symbol, which $detail does not hold" "$out"
        ;;
    *) honest_result "a symbol part ALPHA_X cited in $cited, a name notes/a/x.md holds, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 220. A glob holding no `/` names its paths at any depth, by every reading of the citation's
# files (found while the class of review 6031558230 was fixed). paths_at reads `*.cfg` as every
# path whose last elements match it, at any depth, as the live row r-23's `*.py` names every
# Python file of mas-lab, while git's glob magic and paths_below read it from the root alone,
# so at ef564aae, with notes/deep/x.cfg holding ALPHA_X and no .cfg file at the root, a
# citation of *.cfg passed paths_at, and then ALPHA_X failed in it, and so did `x.cfg`, a
# file's name at or below the cited glob (5c). The case commits notes/deep/x.cfg and
# notes/deep/y.txt, which holds DELTA_Z, and writes the row f-our-1 as citing *.cfg with the
# symbol 'ALPHA_X', then 'x.cfg', then 'DELTA_Z', running --citations over each (symbol_plant):
# the first two must pass citation-our-code; the last must fail it, naming the part and the
# glob. At ef564aae the first two failed.
for form in text name other; do
    case "$form" in
    text) symbol='ALPHA_X' ;;
    name) symbol='x.cfg' ;;
    other) symbol='DELTA_Z' ;;
    esac
    dir="$(symbol_plant "symbol-glob-anywhere-$form" '*.cfg' "$symbol" notes/deep/x.cfg $'ALPHA_X = 1\n' notes/deep/y.txt $'DELTA_Z = 3\n')" &&
        [ "$(git -C "$dir/repo" ls-tree -r --name-only HEAD | grep -c '\.cfg$')" = 1 ] &&
        ! grep -rqF 'x.cfg' "$dir/repo/notes" || {
        printf 'FAIL the citation of *.cfg with the symbol %s could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    other)
        case_result "a symbol part DELTA_Z cited in *.cfg, a name only notes/deep/y.txt holds, fails --citations, naming the part and the glob" \
            "citation-our-code: f-our-1 cites the symbol DELTA_Z, which \\*\\.cfg does not hold" "$out"
        ;;
    *) honest_result "a symbol part $symbol cited in *.cfg, which names notes/deep/x.cfg, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 221. A path part below a cited glob holding no `/` is read at any depth, as the glob itself is
# (the architect's ruling: the rule above paths_at holds in every reading of a cited path).
# Form 7 read a path part below each directory or glob the citation names by paths_at over the
# glob and the part joined, `pkg*/sub/tools`, which holds a `/` and so was read from the root
# alone, while the glob `pkg*` names its paths at any depth, and 5c read a file's name below a
# glob from the root alone too. So at ef564aae, with a/pkg1/sub/x.py and
# a/pkg1/sub/tools/run.sh committed, `sub/x.py` and `sub/tools`, cited in pkg*, each failed,
# though a/pkg1 is a path the glob names and each stands below it. The case commits
# a/pkg1/sub/x.py and a/pkg1/sub/tools/run.sh, then, in another fixture, a/pkg1/other.py
# alone, and writes the row f-our-1 as citing the file field 'pkg*' with the symbol
# 'sub/x.py', then 'sub/tools', over each tree, running --citations over each (symbol_plant):
# over the first tree both must pass citation-our-code; over the second both must fail it,
# naming the part and pkg*. At ef564aae the two over the first tree failed.
for form in name directory name-other directory-other; do
    case "$form" in
    name | name-other) symbol='sub/x.py' detail='sub/x\.py' ;;
    directory | directory-other) symbol='sub/tools' detail='sub/tools' ;;
    esac
    case "$form" in
    *-other) tree=(a/pkg1/other.py $'OTHER = 1\n') ;;
    *) tree=(a/pkg1/sub/x.py $'X = 1\n' a/pkg1/sub/tools/run.sh $'echo run\n') ;;
    esac
    dir="$(symbol_plant "symbol-path-below-glob-$form" 'pkg*' "$symbol" "${tree[@]}")" &&
        [ "$(git -C "$dir/repo" ls-tree -r --name-only HEAD | grep -cE '(^|/)pkg')" = "$((${#tree[@]} / 2))" ] &&
        ! grep -rqF "$symbol" "$dir/repo/a" || {
        printf 'FAIL the citation of pkg* with the symbol %s could not be planted in the fixture row f-our-1\n' "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    *-other)
        case_result "a path part $symbol cited in pkg*, over a tree whose only path the glob names is a/pkg1/other.py, fails --citations, naming the part and the glob" \
            "citation-our-code: f-our-1 cites the symbol $detail, which pkg\\* does not hold" "$out"
        ;;
    *) honest_result "a path part $symbol cited in pkg*, which stands below a/pkg1, a path the glob names, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 222. A package's __init__.pyi stub is the package where no __init__.py stands, as a .pyi is the
# module where no .py of its name stands (the simplification pass of round 51, the class of
# cases 190, 191 and 211: a reader that names a module by its file holds every file Python's
# import takes for it). module_names read __init__.pyi as the module __init__, and
# module_below took a directory for a package only where it held __init__.py, so at ef564aae
# `pkg.func`, cited in pkg/__init__.pyi, which declares func, failed citation-our-code on the
# component pkg, which the stub's text does not write, and so did `core.setup` and `lib.hook`,
# cited in lib, where lib/core/__init__.pyi declares setup and lib/__init__.pyi declares hook,
# on the component core. The case commits those three stubs, lib/core/__init__.pyi declaring
# extra too, their text writing none of pkg, lib, core and check as a word, and writes the row
# f-our-1 as citing pkg/__init__.pyi with the symbol 'pkg.func', then lib with 'core.setup,
# lib.hook', then lib with 'core.check', a name the stub does not declare, then, over a tree
# that also holds lib/core/__init__.py, which declares setup alone, lib with 'core.extra',
# running --citations over each (symbol_plant): the first two must pass citation-our-code; the
# third must fail it, naming check and lib/core/__init__.pyi; and the fourth, read in
# lib/core/__init__.py, as Python's import takes the .py, must fail it, naming extra and
# lib/core/__init__.py. At ef564aae the first two failed, and the third failed naming no file.
for form in named below missing py-first; do
    case "$form" in
    named) cited='pkg/__init__.pyi' symbol='pkg.func' ;;
    below) cited='lib' symbol='core.setup, lib.hook' ;;
    missing) cited='lib' symbol='core.check' ;;
    py-first) cited='lib' symbol='core.extra' ;;
    esac
    case "$form" in
    py-first) init=(lib/core/__init__.py $'def setup(row):\n    return row\n') ;;
    *) init=() ;;
    esac
    dir="$(symbol_plant "symbol-package-stub-$form" "$cited" "$symbol" pkg/__init__.pyi $'def func(row: int) -> int: ...\n' \
        lib/core/__init__.pyi $'def setup(row: int) -> int: ...\ndef extra() -> None: ...\n' lib/__init__.pyi $'def hook(row: int) -> int: ...\n' "${init[@]}")" &&
        ! grep -rqwE 'pkg|lib|core|check' "$dir/repo/pkg" "$dir/repo/lib" || {
        printf 'FAIL the symbol %s cited in %s and the package stubs could not be planted in the fixture row f-our-1\n' "$symbol" "$cited"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    missing)
        case_result "a dotted name core.check, cited in lib, where the package stub lib/core/__init__.pyi does not declare check, fails --citations, naming check and the stub" \
            "citation-our-code: f-our-1 cites the symbol core\\.check, whose prefix names the file lib/core/__init__\\.pyi as the module core, and lib/core/__init__\\.pyi does not hold the name 'check'" "$out"
        ;;
    py-first)
        case_result "a dotted name core.extra, cited in lib, where only lib/core/__init__.pyi beside lib/core/__init__.py declares extra, is read in __init__.py and fails --citations, naming extra" \
            "citation-our-code: f-our-1 cites the symbol core\\.extra, whose prefix names the file lib/core/__init__\\.py as the module core, and lib/core/__init__\\.py does not hold the name 'extra'" "$out"
        ;;
    *) honest_result "dotted names $symbol, cited in $cited, functions of packages whose only package file is an __init__.pyi stub, pass citation-our-code" citation-our-code "$out" ;;
    esac
done

# 223. A cited path is read as its characters stand whatever they are, by every reading of the
# paths the commit tracks (the simplification pass of round 51: a reader of git's list of paths
# reads each path as git holds it). paths_at, paths_below and the reading of a path below a
# cited directory read git ls-tree's list without -z, in which git quotes a path holding a byte
# above 127, a `\` or a `"`, while here_grep hands git the cited path itself. So at ef564aae a
# file field docs/café.md failed as a path the repository does not hold, though the commit
# holds that file, and so did docs/a\b.md and docs/say"hi".md; and `menu.md`, cited in notes,
# which holds notes/index.md too, failed though notes/café/menu.md stands. The case commits
# docs/café.md, docs/a\b.md and docs/say"hi".md, each holding ALPHA_X, and notes/index.md and
# notes/café/menu.md, whose text does not write menu.md, and writes the row f-our-1
# (symbol_plant, the file field set by jq, which writes a `\` and a `"` as JSON does) as citing
# each of the three files with the symbol 'ALPHA_X', then docs/cafe.md, which the tree does
# not hold, with 'ALPHA_X', then notes with 'menu.md', a file's name only a path below notes
# ends with (5c), running --citations over each: each must pass citation-our-code but
# docs/cafe.md, which must fail it, naming the field. At ef564aae each of the four that must
# pass failed, the last on 5c, its file field held by notes/index.md.
for form in field backslash quote field-other name; do
    case "$form" in
    field) cited='docs/café.md' symbol='ALPHA_X' ;;
    backslash) cited='docs/a\b.md' symbol='ALPHA_X' ;;
    quote) cited='docs/say"hi".md' symbol='ALPHA_X' ;;
    field-other) cited='docs/cafe.md' symbol='ALPHA_X' ;;
    name) cited='notes' symbol='menu.md' ;;
    esac
    dir="$(symbol_plant "symbol-path-unquoted-$form" docs/café.md "$symbol" docs/café.md $'ALPHA_X = 1\n' 'docs/a\b.md' $'ALPHA_X = 1\n' \
        'docs/say"hi".md' $'ALPHA_X = 1\n' notes/index.md $'# the notes\n' notes/café/menu.md $'# the menu of the day\n')" &&
        claims="$dir/repo/docs/research/internet-of-cognition/claims.jsonl" &&
        jq -c --arg f "$cited" 'if .id == "f-our-1" then .evidence.file = $f else . end' "$claims" >"$dir/claims.jsonl" &&
        mv "$dir/claims.jsonl" "$claims" && [ "$(jq -r 'select(.id == "f-our-1") | .evidence.file' "$claims")" = "$cited" ] &&
        [ "$(git -C "$dir/repo" ls-tree -r -z --name-only HEAD | grep -zcxE 'docs/café\.md|docs/a\\b\.md|docs/say"hi"\.md|notes/index\.md|notes/café/menu\.md')" = 5 ] &&
        ! git -C "$dir/repo" grep -qF menu.md HEAD -- notes || {
        printf 'FAIL the citation of %s with the symbol %s could not be planted in the fixture row f-our-1\n' "$cited" "$symbol"
        exit 1
    }
    out="$(fixture_run "$dir" --citations)"
    case "$form" in
    field-other)
        case_result "a file field docs/cafe.md over a tree whose only such file is docs/café.md fails --citations, naming the field" \
            "citation-our-code: f-our-1 cites docs/cafe\\.md, which this repository does not hold" "$out"
        ;;
    name) honest_result "a symbol part menu.md cited in notes, a file's name only notes/café/menu.md ends with, passes citation-our-code" citation-our-code "$out" ;;
    *) honest_result "a symbol part ALPHA_X cited in $cited, a file the tree holds that holds ALPHA_X, passes citation-our-code" citation-our-code "$out" ;;
    esac
done

# 224. A measurement the interview states is a row of R2 and not of R1 (R1 of the
# specification; the architect's acceptance at 3e1e50ce, item 7). citations.py read every
# number of a record under check as a citation of it, so at 3e1e50ce the reconciliation's
# rows r-1 to r-10 carried the interview's figures as rows of R1, beside the i-measure-* rows
# of R2 that are this issue's only rows for those numbers, and citation-row passed them. The
# case writes into the fixture's record under check a sentence of his measured claims, its
# opening words broken across a line as the reconciliation breaks them, holding 85%, and
# writes the row u-3 of R1 for that number, u-2's row with its claim changed, running --replay
# over it: citation-row must fail, naming u-3 as no citation the record carries. Beside it the
# same record with no row u-3 must pass citation-row, since the number is no citation of R1
# and so no citation without its row. At 3e1e50ce the first passed citation-row and the
# second failed it, naming the citation u-3 as one with no row.
for form in row no-row; do
    dir="$(fresh "interview-measurement-$form")"
    evidence="$dir/repo/docs/research/internet-of-cognition"
    printf '\nHis measured\nclaims: 85%% of his sessions failed.\n' >>"$evidence/records/under-check.md"
    if [ "$form" = row ]; then
        jq -c 'select(.id == "u-2") | .id = "u-3" | .claim = "The record repeats his measurement that 85% of his sessions failed."' \
            "$evidence/claims.jsonl" >"$dir/u-3.jsonl" && cat "$dir/u-3.jsonl" >>"$evidence/claims.jsonl"
    fi
    grep -qx 'claims: 85% of his sessions failed.' "$evidence/records/under-check.md" &&
        [ "$(grep -c '"id":"u-3"' "$evidence/claims.jsonl")" = "$([ "$form" = row ] && echo 1 || echo 0)" ] || {
        printf 'FAIL the interview measurement could not be planted in the fixture record, form %s\n' "$form"
        exit 1
    }
    out="$(fixture_run "$dir" --replay)"
    case "$form" in
    row)
        case_result "a measurement the interview states, planted as the row u-3 of R1, fails citation-row as no citation of the record" \
            'citation-row: the row u-3 is no citation the record labelled u carries' "$out"
        ;;
    no-row) honest_result "a measurement the interview states with no row of R1 passes citation-row" citation-row "$out" ;;
    esac
done

# 225. The models file of the row i-skills-run is a required input of --replay, as the deployment
# repository is (the architect's condition 1 on the row, ruled 2026-11-24T23:59Z). The row runs
# one headless omp session on a model that IOC_OMP_MODELS, the path of an omp models.yml,
# reaches, so a run with the variable unset ends cannot run, naming it and what provides it,
# before it reads anything, and never as a FAIL of the row. At 3e1e50ce the prerequisite table
# named neither the variable nor omp, so --replay went on to the row, whose script ended 2, and
# the row failed replay-command, exit 1. The case plants the row as claims.jsonl holds it into
# the fixture and runs --replay with the stub omp of tests/omp-stub first on PATH and
# IOC_OMP_MODELS empty (FIXTURE_OMP_STUB, FIXTURE_OMP_MODELS): it must end exit 2, every
# assertion it leaves of the kind prerequisite and none failed, its last line naming the
# variable and what provides it. Case 228 is the honest form.
#
# Cases 225 to 230 run the row on the stub (check.sh's FIXTURE_OMP_STUB), never on a model. The
# stub reads its mode from a file beside itself, so each case copies it (accord_stub) and
# writes the mode. The script reads a clone of cognition-protocols at the commit sources.json
# pins, which is made once here, under the cases' scratch, by env.sh's rule; where it cannot be
# made, each case that needs it could not run, of the kind source.
ACCORD_CASE="$WORK/accord-skills-run"
ACCORD_CLONES="$ACCORD_CASE/clones"
mkdir -p "$ACCORD_CLONES" "$ACCORD_CASE/home" "$ACCORD_CASE/tmp"
ACCORD_WHY=""
if ! (
    IOC_ENV_FUNCTIONS_ONLY=1
    . "$HERE/env.sh" && IOC_ENV_HERE="$HERE" IOC_CLONES="$ACCORD_CLONES" ioc_env_clone cognition-protocols
) >"$ACCORD_CASE/clone.log" 2>&1; then
    ACCORD_WHY="the clone of cognition-protocols at the commit sources.json pins could not be made: $(tail -1 "$ACCORD_CASE/clone.log" | head -c 240)"
fi
# accord_unrun <case>; ends the case as one that could not run, of the kind source, and returns
# 0, when the clone could not be made; it returns 1 when the case can run.
accord_unrun() {
    [ -n "$ACCORD_WHY" ] || return 1
    case_unrun "$1" source "$ACCORD_WHY"
}
# accord_stub <mode>; prints a copy of the stub omp, tests/omp-stub, whose mode file holds <mode>.
accord_stub() {
    local copy="$ACCORD_CASE/stub-$1"
    rm -rf "$copy" && cp -r "$HERE/tests/omp-stub" "$copy" && printf '%s\n' "$1" >"$copy/omp-stub-mode" || return 1
    printf '%s' "$copy"
}
# accord_plant <name>; prints a fresh copy of the fixture whose claims.jsonl holds the row
# i-skills-run as this directory's holds it, its command and its predicate among them, and
# stops when it could not be planted.
accord_plant() {
    local dir
    dir="$(fresh "$1")" &&
        jq -c 'select(.id == "i-skills-run")' "$HERE/claims.jsonl" >>"$dir/repo/docs/research/internet-of-cognition/claims.jsonl" &&
        [ "$(grep -c '"id":"i-skills-run"' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl")" = 1 ] &&
        printf '%s' "$dir" && return 0
    printf 'FAIL the row i-skills-run could not be planted into the fixture\n' >&2
    exit 1
}
# accord_run <stub dir> [<argument>...]; the script run by hand, under env -i, with the stub first on
# PATH and its models.yml as IOC_OMP_MODELS.
accord_run() {
    local stub="$1"
    shift
    env -i PATH="$stub:$PATH" HOME="$ACCORD_CASE/home" TMPDIR="$ACCORD_CASE/tmp" IOC_CLONES="$ACCORD_CLONES" IOC_OMP_MODELS="$stub/models.yml" \
        sh "$HERE/evidence/accord-skills-run.sh" "$@"
}
dir="$(accord_plant accord-models-unset)" || exit 1
out="$(FIXTURE_OMP_STUB="$(accord_stub locked)" FIXTURE_OMP_MODELS='' IOC_EVID="$HERE/evidence" IOC_CLONES="$ACCORD_CLONES" fixture_run "$dir" --replay)"
models_case="--replay with IOC_OMP_MODELS unset cannot run, exit 2, of the kind prerequisite, naming the variable and what provides it, and the row i-skills-run runs no session"
ending_read "$out"
if [ "$RUN_CRASH" != 1 ] && [ "$RUN_STATUS" = 2 ] && [ -z "$RUN_FAILED" ] &&
    printf '%s\n' "$RUN_LINE" | grep -qxF -- 'check.sh cannot run --replay: the variable IOC_OMP_MODELS is not set; provided by: export IOC_OMP_MODELS=<the path of an omp models.yml that reaches a model>' &&
    [ "$(printf '%s' "$RUN_UNRUN" | cut -f2 | sort -u)" = prerequisite ]; then
    case_passed "$models_case"
else
    case_failed "$models_case" "exit $RUN_STATUS, last line: $RUN_LINE; could not run, by kind: $(printf '%s' "$RUN_UNRUN" | cut -f2 | sort | uniq -c | tr -s ' \n' ' ')"
fi

# 226. A session whose protocol does not lock is the row's FAIL, naming the row and what it
# printed, never a read that failed. The script ends 1 and prints "result: not locked" when the
# model answered and the state did not reach INTENT-LOCKED, and the row declares that status
# (IOC_EXPECT_STATUS=1), so run_recorded's record of it is no failed read and the predicate,
# one_of ["locked"], judges the output. This case guards that reading rather than bites: a
# 3e1e50ce whose fixture carried the variable passes it, since the declared status was read the
# same way there; as the fixture stood, with no IOC_OMP_MODELS, the script ended 2 and the row
# failed as a read inside it, with no word of the predicate or of what the session printed. The
# case plants the row and runs --replay with the stub in its stalled mode,
# which ends the session after the first round: exactly one assertion, replay-command, must
# fail, its one line naming the row, its command, the predicate and "not locked".
stalled_case="a session that does not lock fails replay-command alone, naming the row i-skills-run, the predicate and what the run printed"
if ! accord_unrun "$stalled_case"; then
    dir="$(accord_plant accord-stalled)" || exit 1
    out="$(FIXTURE_OMP_STUB="$(accord_stub stalled)" IOC_EVID="$HERE/evidence" IOC_CLONES="$ACCORD_CLONES" fixture_run "$dir" --replay)"
    one_failure_result "$stalled_case" replay-command '^1 of 18 assertions failed: replay-command$' \
        "^FAIL replay-command: i-skills-run ran '.*accord-skills-run\\.sh\" --max-cost 2\\.15 .*' and its output does not meet \\{\"one_of\":\\[\"locked\"\\]\\}: not locked" "$out"
fi

# 227. A model gateway that does not answer is a source that did not, never a verdict on the
# deliverable (check.sh's live-read rule, which the model gateway joins). The script ends 3 and
# prints "result: model not reached (<cause>)" when the model gave no answer, HTTP 429, HTTP 5xx
# or a refusal of the credential, and writes into the row's record the line check.sh's function of a
# program writes, as evidence/concord-build-run.sh does for its curl, so that source_answer reads
# the row's failure as the gateway's. At 3e1e50ce source_answer knew GitHub and Dispatch alone,
# so the script's line was read as any failed read, and the row failed replay-command, exit 1:
# "'omp -p --mode json --max-cost 2' exited 3, so its output is no reading of what the row
# claims". The case plants the row and runs
# --replay with the stub answering the first request with 429, 503 and 401 in turn: each run must
# end exit 2, cannot run, every assertion it leaves of the kind source and none failed, its last
# line naming the row, the model gateway, the rate limit, the server error or the credential,
# and the cause the script printed.
for gateway in 429 503 401; do
    case "$gateway" in
    429) refused="the model gateway's rate limit refused i-skills-run's read of its model through omp" said='HTTP 429' ;;
    503) refused="the model gateway gave no answer that judges i-skills-run's read of its model through omp" said='HTTP 5xx' ;;
    401) refused="the model gateway refused the credential of i-skills-run's read of its model through omp" said='credential refused' ;;
    esac
    gateway_case="a model gateway that answers $gateway cannot run --replay, exit 2, of the kind source, naming the row i-skills-run and the model gateway, and fails no assertion"
    accord_unrun "$gateway_case" && continue
    dir="$(accord_plant "accord-gateway-$gateway")" || exit 1
    out="$(FIXTURE_OMP_STUB="$(accord_stub "$gateway")" IOC_EVID="$HERE/evidence" IOC_CLONES="$ACCORD_CLONES" fixture_run "$dir" --replay)"
    ending_read "$out"
    if [ "$RUN_CRASH" != 1 ] && [ "$RUN_STATUS" = 2 ] && [ -z "$RUN_FAILED" ] && ! grep -q '^FAIL' "$out/text" &&
        printf '%s\n' "$RUN_LINE" | grep -qxF -- "check.sh cannot run --replay now: $refused, so the row's failure is no verdict on the deliverable: 'omp -p --mode json --max-cost 2.15' exited 3 and printed 'result: model not reached ($said)'" &&
        [ "$(printf '%s' "$RUN_UNRUN" | cut -f2 | sort -u)" = source ]; then
        case_passed "$gateway_case"
    else
        case_failed "$gateway_case" "exit $RUN_STATUS, $(grep '^FAIL' "$out/text" | head -2 | cut -c1-300 | tr '\n' ' ')last line: $RUN_LINE; could not run, by kind: $(printf '%s' "$RUN_UNRUN" | cut -f2 | sort | uniq -c | tr -s ' \n' ' ')"
    fi
done

# 228. The honest form of cases 225 to 227: with the stub in its locked mode the session runs the
# protocol to INTENT-LOCKED, the script prints "result: locked", and --replay over the fixture and
# the row passes every assertion it declares. At 3e1e50ce the same --replay did not pass: the
# fixture's run carried no IOC_OMP_MODELS, so the script ended 2 and the row failed.
locked_case="--replay over the fixture and the row i-skills-run on the stub in its locked mode passes every assertion"
if ! accord_unrun "$locked_case"; then
    dir="$(accord_plant accord-locked)" || exit 1
    ending_result "$locked_case" 0 '^all 18 assertions passed$' \
        "$(FIXTURE_OMP_STUB="$(accord_stub locked)" IOC_EVID="$HERE/evidence" IOC_CLONES="$ACCORD_CLONES" fixture_run "$dir" --replay)"
fi

# 229. omp is a prerequisite of --replay, and the models file is one that is read, not only set. A
# run with no omp on PATH, or with IOC_OMP_MODELS naming no readable file, ends cannot run, exit 2,
# of the kind prerequisite, naming it and what provides it, and no line the gate prints names the
# path of the models file, which holds a credential. At 3e1e50ce the table held neither line, so
# each run passed all 18 assertions, exit 0. The
# case runs the fixture's --replay with FIXTURE_OMP_STUB empty and a PATH without omp
# (path_without), then with FIXTURE_OMP_MODELS naming a file that is not there.
dir="$(fresh accord-omp-missing)"
FIXTURE_OMP_STUB='' missing_tool replay omp 'the omp coding agent, with its print mode, on PATH'
out="$(FIXTURE_OMP_MODELS="$dir/no-such-models.yml" fixture_run "$dir" --replay)"
missing_result "--replay whose IOC_OMP_MODELS names no readable file cannot run, naming the variable" replay 'the variable IOC_OMP_MODELS names no readable file' 'export IOC_OMP_MODELS=<the path of an omp models\.yml that reaches a model>' "$out"
if grep -qF -- 'no-such-models' "$out/text"; then
    case_failed "--replay whose IOC_OMP_MODELS names no readable file prints no line that names its path" "$(grep -F -- 'no-such-models' "$out/text" | head -1 | cut -c1-300)"
else
    case_passed "--replay whose IOC_OMP_MODELS names no readable file prints no line that names its path"
fi

# 230. The cost cap of the session is the row's, and the script writes none of its own (the
# architect's condition 2 on the row): the row's command carries --max-cost and its number, the
# flag omp --help prints, the script reads it from its arguments and hands it to omp, and a script
# run with none ends cannot run, naming the flag, as does one whose omp prints no such flag, or
# prints it and not another flag the session passes, so a renamed flag is never read as a model
# that did not answer. The case reads the row's command, runs the script by hand on the stub with
# --max-cost 7, which the stub writes into the arguments of its session, with no flag, over an
# omp whose help prints no flag, and over one whose help prints --max-cost alone. It judges the
# script and the row's command by hand and not the gate. The last form was added in round 52:
# the script of e13cc01c read the help for --max-cost alone, so over that omp it ran a session
# and ended 3, "result: model not reached (no answer)", a model gateway that did not answer.
cap_case="the cost cap of the row i-skills-run is the --max-cost its command passes: the script hands it to omp, and ends cannot run with none, or with an omp that prints no such flag or no other flag the session passes"
if ! accord_unrun "$cap_case"; then
    stub="$(accord_stub locked)"
    mkdir -p "$ACCORD_CASE/old-omp"
    printf '#!/bin/sh\necho "usage: omp"\n' >"$ACCORD_CASE/old-omp/omp"
    cp "$HERE/tests/omp-stub/models.yml" "$ACCORD_CASE/old-omp/models.yml"
    chmod +x "$ACCORD_CASE/old-omp/omp"
    mkdir -p "$ACCORD_CASE/cap-only-omp"
    printf '#!/bin/sh\necho "      --max-cost=<value>    Stop sending model requests once the spend reaches this many USD"\n' >"$ACCORD_CASE/cap-only-omp/omp"
    cp "$HERE/tests/omp-stub/models.yml" "$ACCORD_CASE/cap-only-omp/models.yml"
    chmod +x "$ACCORD_CASE/cap-only-omp/omp"
    flag="$(jq -r 'select(.id == "i-skills-run") | .command' "$HERE/claims.jsonl" | grep -oE -- '--max-cost [0-9]+(\.[0-9]+)?')"
    without="$(accord_run "$stub" 2>&1)"
    without_status=$?
    old="$(accord_run "$ACCORD_CASE/old-omp" --max-cost 2 2>&1)"
    old_status=$?
    caponly="$(accord_run "$ACCORD_CASE/cap-only-omp" --max-cost 2 2>&1)"
    caponly_status=$?
    with="$(accord_run "$stub" --max-cost 7 2>&1)"
    with_status=$?
    handed="$(awk '$0 == "--max-cost" { getline; print; exit }' "$stub/omp-stub-args" 2>/dev/null)"
    if [ -n "$flag" ] && [ "$without_status" = 2 ] && [ "$(printf '%s\n' "$without" | head -1)" = 'cannot run: --max-cost is not given; pass the flag omp --help prints, as --max-cost 2' ] &&
        [ "$old_status" = 2 ] && [ "$(printf '%s\n' "$old" | head -1)" = 'cannot run: omp --help prints no --max-cost flag' ] &&
        [ "$caponly_status" = 2 ] && [ "$(printf '%s\n' "$caponly" | head -1)" = 'cannot run: omp --help prints no --max-time flag' ] &&
        [ "$with_status" = 0 ] && [ "$(printf '%s\n' "$with" | tail -1)" = 'result: locked' ] && [ "$handed" = 7 ]; then
        case_passed "$cap_case"
    else
        case_failed "$cap_case" "the row's flag is '$flag'; with none the script exited $without_status, '$(printf '%s\n' "$without" | head -1 | cut -c1-120)'; over an omp with no flag, $old_status, '$(printf '%s\n' "$old" | head -1 | cut -c1-120)'; over an omp with --max-cost alone, $caponly_status, '$(printf '%s\n' "$caponly" | head -1 | cut -c1-120)'; with 7, $with_status and handed '$handed'"
    fi
fi

# 231. The message to the assignee carries the ranking, every item of the list under "## The
# ranking", in its order (R13 and c-published; the architect's acceptance at 3e1e50ce, item
# 6). At 3e1e50ce published-delivery read the messages of the issue for the word "ranking"
# alone, so a message under the heading "## The ranking" that left out an item of the list,
# or gave the items in another order, passed, exit 0. Cases 231 to 237 run --published over
# the fixture's own Dispatch (check.sh's fixture_published), whose delivery fixture_delivery
# writes with one part missing. Here the message holds the first of the fixture's two items
# alone, and then the two in the other order: each must fail published-delivery alone, exit
# 1, its one FAIL naming the newest message and item 2 of the ranking.
DELIVERY_NEWEST="^FAIL published-delivery: the newest message on this issue to its assignee, dispatch://INFRA-3134/message/[^ ]+ of [^ ]+, does not carry "
# delivery_plant <name> <part>; prints a fresh copy of the fixture whose delivery
# fixture_delivery writes without <part>, and stops when it could not be written.
delivery_plant() {
    local dir
    dir="$(fresh "$1")" && fixture_delivery "$dir" "$2" && printf '%s' "$dir" && return 0
    printf 'FAIL the delivery without its %s could not be planted into the fixture\n' "$2" >&2
    exit 1
}
for form in ranking order; do
    case "$form" in
    ranking) form_case="a delivery message whose ranking leaves out an item of the list fails published-delivery alone, naming the ranking and the item" ;;
    order) form_case="a delivery message whose ranking gives the items in another order fails published-delivery alone, naming the ranking and the item" ;;
    esac
    dir="$(delivery_plant "delivery-$form" "$form")" || exit 1
    one_failure_result "$form_case" published-delivery '^1 of 2 assertions failed: published-delivery$' \
        "${DELIVERY_NEWEST}the ranking: item 2 of the list under '## The ranking' is not in it, one after another in the list's order$" "$(fixture_published "$dir")"
done

# 232. The message carries the paragraph on where the two accounts differ, the text under
# "## Where the authors' account and the checked reading differ" (R13, the paragraph R2
# requires). At 3e1e50ce a message holding the word "ranking" passed whatever else it held,
# so a message with one sentence in place of the paragraph passed, exit 0. It must now fail
# published-delivery alone, naming the paragraph. --self-test plants the same fault.
dir="$(delivery_plant delivery-differ differ)" || exit 1
one_failure_result "a delivery message without the paragraph on where the two accounts differ fails published-delivery alone, naming the paragraph" published-delivery \
    '^1 of 2 assertions failed: published-delivery$' "${DELIVERY_NEWEST}the paragraph on where the two accounts differ, " "$(fixture_published "$dir")"

# 233. The message carries each candidate above one engineer-day with its default, the text
# of the page's "Default if nobody answers" line after the page's title (R13). At 3e1e50ce a
# message that named the candidate 'One definitions record' with another default passed,
# exit 0. It must now fail published-delivery alone, naming the candidate and its default.
# The second form gives two candidates each other's default. At e6a13e4e the text after the
# first place a title stands, which is its item of the ranking, was searched for the default,
# so a default under any title passed, exit 0. The text is now read after the last place the
# title stands, up to the title of another candidate: each of the two candidates must fail,
# naming its default.
dir="$(delivery_plant delivery-default default)" || exit 1
one_failure_result "a delivery message that names a candidate above one engineer-day without its default fails published-delivery alone, naming the candidate" published-delivery \
    '^1 of 2 assertions failed: published-delivery$' "${DELIVERY_NEWEST}the candidate 'One definitions record', above one engineer-day, with its default: its default does not follow its title$" "$(fixture_published "$dir")"
swapped_case="a delivery message that gives two candidates each other's default fails published-delivery alone, naming each candidate and that its default does not follow its title"
dir="$(fresh delivery-swapped)"
jq '.[0].message.body |= sub("(?<a>- \\*\\*Structured metadata on an Envoy message:\\*\\* )(?<x>[^\n]*)\n(?<b>- \\*\\*One definitions record:\\*\\* )(?<y>[^\n]*)"; "\(.a)\(.y)\n\(.b)\(.x)")' "$dir/dispatch/messages.json" >"$dir/messages.json" &&
    mv "$dir/messages.json" "$dir/dispatch/messages.json" &&
    jq -e '.[0].message.body | contains("- **Structured metadata on an Envoy message:** terms stay prose in each issue.\n- **One definitions record:** the envelope keeps no field of the record a fact came from.")' "$dir/dispatch/messages.json" >/dev/null || {
    printf 'FAIL the delivery message with two defaults swapped could not be planted into the fixture\n'
    exit 1
}
out="$(fixture_published "$dir")"
if case_read "$swapped_case" "$out"; then
    if [ "$RUN_STATUS" = 1 ] && [ "$RUN_FAILED" = published-delivery$'\n' ] && [ "$(grep -c '^FAIL ' "$out/text")" = 2 ] &&
        grep -qE -- "${DELIVERY_NEWEST}the candidate 'Structured metadata on an Envoy message', above one engineer-day, with its default: its default does not follow its title\$" "$out/text" &&
        grep -qE -- "${DELIVERY_NEWEST}the candidate 'One definitions record', above one engineer-day, with its default: its default does not follow its title\$" "$out/text"; then
        case_passed "$swapped_case"
    else
        case_failed "$swapped_case" "exit $RUN_STATUS, $(grep '^FAIL' "$out/text" | head -3 | cut -c1-300 | tr '\n' ' ')last line: $RUN_LINE"
    fi
fi

# 234. The comment on dispatch://INFRA-1098 records the gaps R8 found, each by the row
# node-model.md's section "## 3. The gaps" cites for it, or, where that section cites none,
# the judgement holds R8's sentence (the architect's ruling of 2026-11-25T02:09Z on item 6).
# At 3e1e50ce published-delivery passed any comment of INFRA-1098 that held the word
# INFRA-3134, so each of the two ways the part can fail passed, exit 0: a comment naming
# this issue that cites one of the fixture's two gap rows, and a node-model.md whose section
# of the gaps cites none beside a judgement without R8's sentence. Each must now fail
# published-delivery alone, naming what is missing. The honest form of the second way, R8's
# sentence in the judgement and no comment on INFRA-1098 at all, passes both assertions; at
# 3e1e50ce it failed, since no comment held the word.
dir="$(fresh delivery-gap-uncited)"
jq '.[0].body |= sub("\n2\\. [^\n]*"; "")' "$dir/dispatch/upstream.json" >"$dir/upstream.json" &&
    mv "$dir/upstream.json" "$dir/dispatch/upstream.json" && ! grep -q 'n-gap-evidence-kind' "$dir/dispatch/upstream.json" || {
    printf 'FAIL the comment on INFRA-1098 citing one gap could not be planted into the fixture\n'
    exit 1
}
one_failure_result "a comment on INFRA-1098 that names this issue and cites one of node-model.md's two gaps fails published-delivery alone, naming the gap it leaves out" published-delivery \
    '^1 of 2 assertions failed: published-delivery$' '^FAIL published-delivery: no comment of dispatch://INFRA-1098 that names INFRA-3134 cites each gap .*: the newest, dispatch://INFRA-1098/comment/[^ ]+, does not cite n-gap-evidence-kind$' \
    "$(fixture_published "$dir")"
for form in unsaid said; do
    dir="$(fresh "delivery-no-gap-$form")"
    sed -i '/^\*\*/d' "$dir/repo/docs/research/internet-of-cognition/node-model.md"
    if [ "$form" = unsaid ]; then
        one_failure_result "a node-model.md that records no gap beside a judgement without R8's sentence fails published-delivery alone, naming the sentence" published-delivery \
            '^1 of 2 assertions failed: published-delivery$' "^FAIL published-delivery: node-model\\.md's section of the gaps cites no gap's row, and the judgement does not say 'The node model holds a finding with its evidence and its verdict, and no gap was found\\.'" \
            "$(fixture_published "$dir")"
    else
        printf '\n%s\n' "$R8_NO_GAP" >>"$dir/judgement.md"
        printf '[]\n' >"$dir/dispatch/upstream.json"
        ending_result "a node-model.md that records no gap beside a judgement holding R8's sentence passes --published with no comment on INFRA-1098" 0 '^all 2 assertions passed$' \
            "$(fixture_published "$dir")"
    fi
done

# 235. The publication of the ranked list to the controller is judged by its record, a
# comment of this issue that names the topic notifications.role.legion-infra-controller, gives
# the time of the publication as a UTC time in RFC 3339 form, and carries the ranking as
# published (the approved departure of the plan: "its topic, its time and the ranking as
# published"). At 3e1e50ce any comment that held the topic passed, so a record whose list
# leaves out the second item passed, exit 0, and so did a record that gives its time in words,
# "this morning", and no UTC time. Each must now fail published-delivery alone, naming the
# comment and what it lacks: item 2 of the ranking, or the time of the publication.
for form in publication time; do
    case "$form" in
    publication)
        form_case="a record of the publication whose ranked list leaves out an item fails published-delivery alone, naming the item"
        lacks="the ranked list as published: item 2 of the list under '## The ranking' is not in it"
        ;;
    time)
        form_case="a record of the publication that gives no UTC time fails published-delivery alone, naming the time of the publication"
        lacks='the time of the publication, a UTC time in RFC 3339 form, as 2026-11-25T02:09:09Z$'
        ;;
    esac
    dir="$(delivery_plant "delivery-$form" "$form")" || exit 1
    one_failure_result "$form_case" published-delivery '^1 of 2 assertions failed: published-delivery$' \
        "^FAIL published-delivery: the comment dispatch://INFRA-3134/comment/[^ ]+, the newest of this issue that names the topic notifications\\.role\\.legion-infra-controller, does not carry $lacks" \
        "$(fixture_published "$dir")"
done

# 236. The message is one to the issue's assignee: the issue names its assignee, and a message
# reaches the assignee when the assignee did not write it and it is addressed to no session or
# role. At 3e1e50ce the gate read neither, so the fixture's delivery message, holding every
# part, passed as one the assignee wrote, its author's address in capitals where the issue's is
# not, since an address is read without case, and as one addressed to a session, exit 0. Each
# must now fail published-delivery alone: no message reaches the assignee.
for form in author target; do
    case "$form" in
    author) edit='.[0].message.author = {kind: "user", id: "Assignee@Example.org"}' form_case="a delivery message the assignee wrote reaches no assignee, and fails published-delivery alone" ;;
    target) edit='.[0].message.target = {kind: "session", id: "fixture-session"}' form_case="a delivery message addressed to a session reaches no assignee, and fails published-delivery alone" ;;
    esac
    dir="$(fresh "delivery-$form")"
    jq "$edit" "$dir/dispatch/messages.json" >"$dir/messages.json" && mv "$dir/messages.json" "$dir/dispatch/messages.json" || {
        printf 'FAIL the delivery message could not be planted into the fixture\n'
        exit 1
    }
    one_failure_result "$form_case" published-delivery '^1 of 2 assertions failed: published-delivery$' \
        '^FAIL published-delivery: no message on this issue reaches its assignee: ' "$(fixture_published "$dir")"
done

# 237. The honest delivery of cases 231 to 236: the fixture's message holds every part, the
# comment on INFRA-1098 cites both gaps and the record of the publication carries the
# ranking, so --published passes both its assertions, exit 0, as it did at 3e1e50ce, and its
# text says it reads the record of the publication and not the publication.
dir="$(fresh delivery-honest)"
out="$(fixture_published "$dir")"
ending_result "the fixture's honest delivery passes --published" 0 '^all 2 assertions passed$' "$out"
if grep -qF 'This mode reads the record of the publication to the controller' "$out/text"; then
    case_passed "--published says it reads the record of the publication to the controller and not the publication"
else
    case_failed "--published says it reads the record of the publication to the controller and not the publication" "$(tail -2 "$out/text" | tr '\n' ' ')"
fi

# 238. The default branch carries every path under docs/research/internet-of-cognition/ the
# judgement names, front page and pages (the architect's ruling of 2026-11-25T02:09Z). At
# 3e1e50ce published-tree read seven fixed files and the files of the landed list and no path
# of the judgement, so a page naming coordination/no-such-count.md, which the fixture's head
# does not carry, passed, exit 0. The case plants --self-test's fault of published-tree
# (fixture_plant): it must fail published-tree alone, naming the path.
dir="$(fresh published-page-path)"
fixture_plant "$dir" published-tree || {
    printf 'FAIL the page naming a file the head lacks could not be planted into the fixture\n'
    exit 1
}
one_failure_result "a page that names a file of this directory the default branch's commit lacks fails published-tree alone, naming the path" published-tree \
    '^1 of 2 assertions failed: published-tree$' '^FAIL published-tree: the judgement names docs/research/internet-of-cognition/coordination/no-such-count\.md, and the default branch at [0-9a-f]+ does not carry it$' \
    "$(fixture_published "$dir")"

# 239. The default branch carries the files each landed step changed. The case removes
# skills/legion-worker/references/pr-body.md, the file of the fixture's landed step, in a
# commit of the fixture's own head, and names the fixture's own repository as our
# repository's url, so the mode fetches that head: published-tree must fail alone, naming the
# file. The gate of 3e1e50ce failed it the same way, so this case guards the read and does not
# bite.
dir="$(fresh published-landed-file)"
(cd "$dir/repo" && git rm --quiet skills/legion-worker/references/pr-body.md && git commit --quiet -m "the landed step's file removed") &&
    jq --arg url "$dir/repo" '.our_repository.url = $url' "$dir/repo/docs/research/internet-of-cognition/sources.json" >"$dir/sources.json" &&
    mv "$dir/sources.json" "$dir/repo/docs/research/internet-of-cognition/sources.json" || {
    printf "FAIL the landed step's file could not be removed from the fixture's head\n"
    exit 1
}
one_failure_result "a landed step's file the default branch's commit lacks fails published-tree alone, naming the file" published-tree \
    '^1 of 2 assertions failed: published-tree$' '^FAIL published-tree: the default branch at [0-9a-f]+ does not carry skills/legion-worker/references/pr-body\.md$' \
    "$(fixture_published "$dir")"

# ==== the summary of check.test.sh begins below this line, where cases_copy cuts it ====
# The summary, by the three endings and their precedence (check.sh's header): run_ending
# reads every case this file counted, the ones that could not run among them, and this
# run's scratch, prints the last line, writes the ending record and returns the status
# this file ends with.
CASES_SUMMARY=1
run_ending cases
exit "$?"
