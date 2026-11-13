#!/usr/bin/env bash
#
# check.sh is the gate of dispatch://INFRA-3134. It runs every command this reading
# recorded, reads the judgement for its shape, resolves every citation, asserts each
# landed step is in this tree with a passing proof, and proves its own bite.
#
#   bash docs/research/internet-of-cognition/check.sh --replay
#   bash docs/research/internet-of-cognition/check.sh --shape
#   bash docs/research/internet-of-cognition/check.sh --citations
#   bash docs/research/internet-of-cognition/check.sh --landed
#   bash docs/research/internet-of-cognition/check.sh --self-test
#   bash docs/research/internet-of-cognition/check.sh --published
#   bash docs/research/internet-of-cognition/check.sh --coverage
#   bash docs/research/internet-of-cognition/check.sh --help
#
# It prints one PASS or FAIL line per assertion and a last line reading
#   all N assertions passed
# N being the number it ran. Any other ending, no output at all included, is a failed
# check: a run that was killed prints nothing and exits non-zero for a reason that is not
# this deliverable's. Read the run by that last line and not by the exit status alone.
#
# Prerequisites, because nothing in CI runs this script and a gate nobody else can start
# passes by default:
#
#   git        resolves every citation into one of the projects and into this repository
#   curl       reads a published page's live address and the Dispatch API
#   jq         reads claims.jsonl and the Dispatch answers
#   python3    runs citations.py, the one extraction rule
#   uv         installs what a recorded command needs, under $TMPDIR
#   go         runs the two recorded commands that count rows of our own table
#   gh         reads the review bodies of our own repository, which one recorded search
#              reads; no citation of a project is ever resolved through it
#   dispatch   reads the judgement, the two records under check and this issue
#   $DISPATCH_TOKEN_FILE   the Dispatch bearer this pod already holds
#
# It reaches GitHub with git and the REST API and never with gh, which in a Legion pod is
# scoped to this project's owners and refuses ioc-project. It clones what it needs under
# the directory IOC_CLONES names, $TMPDIR/ioc-clones by default, at the commits
# sources.json pins. It writes nothing outside that directory and its own scratch.
#
# No recorded command may read the scratch of the pod this reading was made in, because
# that pod's disk died with it. --replay therefore sources env.sh once, before it runs the
# first recorded command, and env.sh prepares what those commands read and this tree
# cannot hold: the two Dispatch records under check, a python environment with
# sentence-transformers, torch and rdflib, the pinned all-MiniLM-L6-v2 snapshot checked
# against the checksum claims.jsonl carries, mas-lab installed from its clone, the SHACL
# shapes graph of the scope clone, and the review corpus one search reads. It caches all
# of that under IOC_CACHE, $TMPDIR/infra3134 by default, builds each thing once, and
# exports one variable per input, which every recorded command reads beside IOC_CLONES.
# env.sh's own header names each variable. A run whose environment env.sh cannot build
# stops there, with the line that names what it could not build.
#
# --shape and --landed read the history of this pull request: the commit that adds the
# question list and the commits of the landed steps. After the squash merge the default
# branch holds none of them, so both modes read IOC_HISTORY_REF in place of HEAD when it
# is set: a ref that holds the pull request's history, such as the one a reader makes with
#   git fetch origin refs/pull/3771/head:refs/remotes/origin/pr-3771
# and names as IOC_HISTORY_REF=refs/remotes/origin/pr-3771.
#
# --landed runs no text it reads from Dispatch. The proof of each landed step is committed
# in the landed list of sources.json, one entry per landed page with the page's title, its
# commit and its command. The mode runs the committed command, and fails a page whose
# Landed commit or Proven by command differs from the committed entry for its title, and a
# page carrying a Landed line that no committed entry names.
#
# The Dispatch bearer stands on no command line. A run writes it once as an HTTP header
# into a file under its own scratch, readable by its owner alone, exports that file's path
# as IOC_DISPATCH_HEADER, and every curl of the Dispatch API, the recorded ones included,
# reads it with -H @"$IOC_DISPATCH_HEADER". The scratch is removed when the run ends.
#
# The evidence it reads:
#
#   claims.jsonl   one line per claim, one JSON object. Keys: id, requirement, source,
#                  kind, claim, mark, evidence, command, expect, output, note.
#                  kind is one of project-code, our-code, behaviour, published, absence
#                  and quotation, which are the five forms of the specification with the
#                  quotation form marked as a claim of the project rather than of its
#                  code. mark is one of verified, false, unverified,
#                  not-externally-checkable, supported, unsupported and unreleased.
#                  expect holds exactly one of equals, contains, matches, exit, at_most,
#                  at_least and one_of. A predicate written as a sentence fails here.
#   questions.md   the question list, which every candidate page answers from
#   judgement.md   read live from Dispatch, the document the reader reads
#
# The one assertion table below is where every PASS line, every planted fault of
# --self-test and every line of --coverage comes from, so "a bad case of every kind the
# gate reads" is true by construction rather than by two lists kept in step by hand.

set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${IOC_ROOT:-$(cd "$HERE/../../.." && pwd)}"
CLONES="${IOC_CLONES:-${TMPDIR:-/tmp}/ioc-clones}"
EVIDENCE="${IOC_EVIDENCE:-$HERE}"
# This run's scratch, made fresh here and removed when the run ends, so that what one
# run reads from Dispatch is never read by the next.
RUN_SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/ioc-run.XXXXXX")"
trap 'rm -rf "$RUN_SCRATCH"' EXIT
PASSED=0
FAILED=0
FAILED_FAMILIES=""

# family | mode | what it asserts | the fault --self-test plants, or - for none
assertion_table() {
    cat <<'TABLE'
claim-form|replay|every claim carries one of the five forms of evidence the specification allows|a claim whose kind is none of them
claim-mark|replay|every claim carries one of the marks its requirement fixes|a claim with no mark
predicate-form|replay|every row that carries a command carries exactly one predicate over its output, and every predicate is a bound, a set membership or an exit status, never a sentence|a predicate written as a sentence
install-pinned|replay|every package a recorded command installs or runs names its version, or a constraints file, a pinned list or a local path|a recorded command that runs a package with no version
replay-command|replay|every recorded command runs again, no read inside it failed unless its predicate is an exit status, and its output meets the predicate the row carries|a recorded output changed by one character
absence-still-empty|replay|every absence search reads its corpus and still finds nothing, and a read that failed before the search is no empty search|an absence search that now finds something
quotation-marked|replay|every quotation of a project's prose is marked a claim of the project and not of its code|a quotation marked as evidence of the code
citation-row|replay|every citation of the two records under check has a row with a mark|a verification row deleted
mas-lab-directory|replay|each of the six named directories of mas-lab has an evidence file at the clone commit|the evidence file of one directory deleted
mas-lab-question|replay|each of the three questions of mas-lab is answered by a file naming its evidence|an answer with no evidence named
interview-fixed-row|replay|the four rows the specification fixes for the authors' account are present by identifier|one fixed row deleted
our-data-input|replay|each test against our own records names the Dispatch key or the commit its input came from|a test whose input is a fixture of the project
count-window|replay|the count of our own failures names its window as two instants, in the window.json the count was run over|the window deleted
count-class|replay|the count classes are the three the specification names|a fourth class
count-denominator|replay|every class of the count carries its denominator|a class with no denominator
count-zero-premise|replay|a class of zero over a denominator above zero is stated on the front page as a premise that failed|a class of zero with no failed premise
page-count|shape|the judgement carries at least three and at most seven candidate pages|an eighth candidate page
page-parts|shape|every candidate page carries all eleven of its parts|a page short of its reversal cost
page-verdict|shape|every candidate page carries exactly one verdict word of vendor, depend, port and reject|a page with two verdict words
verdict-commitment|shape|a depend names its upstream owner, a port names the change in our code, a reject names its reason|a reject with no reason
fixed-page|shape|the three pages the specification fixes exist and each answers by name the design already written for it|a fixed page that answers no named design
front-page|shape|the front page carries the ranking, the one thing to do first, the paragraph on where the two accounts differ, and what was not read|the paragraph deleted
question-list|shape|every page's question is on the question list, and every question is answered by a page or named on the front page|a page whose question is not on the list
triage-named|shape|every repository the triage dropped is named on the front page with its reason|a dropped repository the front page does not name
order-of-work|shape|the commit that adds the question list is an ancestor of every commit that adds a deep-read evidence file|a question-list commit that is not an ancestor
citation-our-code|citations|every citation into our own repository names a path that stands at the commit it names, and every part of its symbol that is an identifier, a line, a prefixed name or a file name stands in that path; a part written in words is reported as not checked|a cited symbol no file holds
citation-project-code|citations|every citation into one of the projects names a path the clone holds at the pinned commit, and every part of its symbol that is an identifier, a line, a prefixed name or a file name stands in that path; a part written in words is reported as not checked|a cited identifier the file does not hold
citation-published|citations|every citation of a published document names a committed copy whose checksum is the one sources.json recorded and whose text holds the sentence quoted|a byte appended to a committed copy
citation-dispatch|citations|every dispatch:// reference resolves to a node Dispatch holds|a reference that resolves to no node
citation-kinds|citations|the extraction rule reads a citation of every kind it names, a number written in words among them|an extractor that reads digits alone
landed-proof|landed|every landed page names the commit and the command sources.json commits for its title, the commit is one this pull request added and not one the base branch held, and the committed command passes|a page whose proof command differs from the committed one
landed-refutation|landed|a page that refutes a step carries a command with output, never a search that found nothing|a refutation that rests on a search alone
landed-order|landed|the commit of the shortfall rows is an ancestor of the commit of the combining rule|the two commits in the other order
landed-no-refusal|landed|no landed step's diff reaches a refusal path of the daemon or of Dispatch, and none adds a refusal constant|a landed step that touches a refusal path
landed-third-change|landed|the third change, a refusal of a reviewer completion with no pair result, is not landed in any form|the third change landed as prose in a skill
published-tree|published|the default branch carries the evidence directory, the committed transcript, this script and every file a page links to|-
published-delivery|published|the record of the delivery is on the issue: the message, the comment on dispatch://INFRA-1098 and the comment recording the publication|-
TABLE
}

usage() {
    # The header, down to the first line that is not a comment.
    awk 'NR == 1 { next } !/^#/ { exit } { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"
}

pass() {
    PASSED=$((PASSED + 1))
    printf 'PASS %s: %s\n' "$1" "$2"
}

fail() {
    FAILED=$((FAILED + 1))
    FAILED_FAMILIES="$FAILED_FAMILIES $1"
    printf 'FAIL %s: %s\n' "$1" "$2"
}

finish() {
    local total=$((PASSED + FAILED))
    if [ "$FAILED" -eq 0 ]; then
        printf 'all %d assertions passed\n' "$total"
        return 0
    fi
    printf '%d of %d assertions failed:%s\n' "$FAILED" "$total" "$FAILED_FAMILIES"
    return 1
}

need() {
    local missing=""
    for tool in "$@"; do
        command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
    done
    if [ -n "$missing" ]; then
        printf 'check.sh cannot run: this pod has no%s. --help lists what it needs.\n' "$missing"
        exit 2
    fi
}

claims_file() { printf '%s/claims.jsonl' "$EVIDENCE"; }

# Every row of claims.jsonl, as compact JSON, one per line.
claims() { grep -v '^[[:space:]]*$' "$(claims_file)"; }

clone_root() {
    # A row may name a repository as `scope` or as `ioc-project/scope`; both name the
    # same row of sources.json, which pins it by its last path element.
    local name="${1##*/}" url commit
    url="$(jq -r --arg n "$name" '.repositories[] | select(.name==$n) | .url' "$EVIDENCE/sources.json")"
    commit="$(jq -r --arg n "$name" '.repositories[] | select(.name==$n) | .commit' "$EVIDENCE/sources.json")"
    [ -n "$url" ] || return 1
    mkdir -p "$CLONES/$name"
    # One commit deep and no tags, which is the clone the triage read; env.sh clones the
    # same way, and the rows that read a repository's history read it from the committed
    # answers of GitHub's own API instead.
    if [ ! -d "$CLONES/$name/.git" ]; then
        git -C "$CLONES/$name" init --quiet >&2 || return 1
        git -C "$CLONES/$name" remote add origin "$url" 2>/dev/null || true
    fi
    git -C "$CLONES/$name" cat-file -e "$commit^{commit}" 2>/dev/null ||
        git -C "$CLONES/$name" fetch --quiet --depth 1 --no-tags origin "$commit" >&2 || true
    printf '%s/%s' "$CLONES" "$name"
}

our_commit() { jq -r '.our_repository.commit' "$EVIDENCE/sources.json"; }

# dispatch_header prints the path of a file holding the Dispatch bearer as one HTTP
# header, which curl reads with -H @<file>, so the token stands on no command line. The
# file is written once per run, readable by its owner alone, under this run's scratch,
# which the EXIT trap removes.
dispatch_header() {
    local file="$RUN_SCRATCH/dispatch.header"
    if [ ! -s "$file" ]; then
        [ -n "${DISPATCH_TOKEN_FILE:-}" ] && [ -r "$DISPATCH_TOKEN_FILE" ] || return 1
        (
            umask 077
            printf 'Authorization: Bearer %s\n' "$(tr -d '\r\n' <"$DISPATCH_TOKEN_FILE")" >"$file"
        ) || return 1
    fi
    printf '%s' "$file"
}

dispatch_get() {
    local path="$1" header
    header="$(dispatch_header)" || return 1
    curl -sS -H @"$header" "${DISPATCH_URL%/}$path"
}

# record_reader makes env.sh's reader of a Dispatch record, ioc_env_read_record, a function
# of this shell, so that check.sh and env.sh keep a record by one rule: the read exited 0
# and what it printed is not a refusal. env.sh read this way defines its functions and
# prepares nothing.
record_reader() {
    declare -F ioc_env_read_record >/dev/null && return 0
    [ -r "$EVIDENCE/env.sh" ] || return 1
    local IOC_ENV_FUNCTIONS_ONLY=1
    # shellcheck source=env.sh
    . "$EVIDENCE/env.sh"
}

# The judgement, read live from Dispatch once per run and held in this run's scratch, so
# the gate reads what a reader reads now rather than what an earlier run left behind.
judgement_text() {
    local cache="$RUN_SCRATCH/judgement.md"
    if [ -n "${IOC_JUDGEMENT:-}" ]; then
        cat "$IOC_JUDGEMENT"
        return 0
    fi
    if [ ! -s "$cache" ]; then
        { record_reader && ioc_env_read_record dispatch://INFRA-3134/artifact/judgement-md "$cache"; } 2>/dev/null || return 1
    fi
    cat "$cache"
}

record_text() {
    local label="$1" ref copy cache
    ref="$(jq -r --arg l "$label" '.records_under_check[] | select(.label==$l) | .ref' "$EVIDENCE/sources.json")"
    copy="$(jq -r --arg l "$label" '.records_under_check[] | select(.label==$l) | .copy // ""' "$EVIDENCE/sources.json")"
    if [ -n "$copy" ]; then
        # A record this directory carries a copy of, which is how the self-test's own
        # fixture holds one. The two records of this issue are read live from Dispatch.
        cat "$EVIDENCE/$copy"
        return 0
    fi
    # env.sh fetched the two records of this issue before the first recorded command ran
    # and exported where it put them; this reads those rather than fetching them twice.
    case "$label" in
    d) cache="${IOC_DESIGN:-}" ;;
    r) cache="${IOC_COMMENT:-}" ;;
    *) cache="" ;;
    esac
    if [ -n "$cache" ] && [ -s "$cache" ]; then
        cat "$cache"
        return 0
    fi
    cache="$RUN_SCRATCH/record-$label.md"
    if [ ! -s "$cache" ]; then
        { record_reader && ioc_env_read_record "$ref" "$cache"; } 2>/dev/null || return 1
    fi
    cat "$cache"
}

# history_ref names the commit whose history --shape and --landed read: IOC_HISTORY_REF
# when it is set, which a reader sets once the squash merge has left the pull request's
# own commits off the default branch, and HEAD otherwise.
history_ref() { printf '%s' "${IOC_HISTORY_REF:-HEAD}"; }

# pr_base prints the base this pull request's own diff is read against: the base branch's
# commit that the newest forward merge on the pull request's first-parent line brought in,
# or the commit our code was read at when no merge did. A range from our_commit alone would
# carry every change the base branch made since, which are no step of this pull request.
pr_base() {
    local merge
    merge="$(git -C "$ROOT" rev-list --first-parent --merges -1 "$(our_commit)..$(history_ref)")"
    if [ -n "$merge" ]; then
        git -C "$ROOT" rev-parse "$merge^2"
    else
        our_commit
    fi
}

# pr_commits prints the commits this pull request added, one per line: those of its history
# that are neither the commit our code was read at nor reachable from a base commit a
# forward merge brought in. A forward merge itself is one of them.
pr_commits() {
    local merges
    merges="$(git -C "$ROOT" rev-list --merges "$(our_commit)..$(history_ref)" | sed 's/$/^2/')"
    # shellcheck disable=SC2086
    git -C "$ROOT" rev-list "$(history_ref)" --not "$(our_commit)" $merges
}

# ---------------------------------------------------------------- the predicate


# bound <at_most|at_least> <value> <output file>
#
# A bound reads the number the output prints when the output is one number, which is how
# a score or a count is recorded, and the number of lines otherwise.
bound() {
    local key="$1" value="$2" out="$3" read_value
    read_value="$(tr -d '[:space:]' <"$out")"
    if ! printf '%s' "$read_value" | grep -qE '^-?[0-9]+(\.[0-9]+)?$'; then
        read_value="$(wc -l <"$out" | tr -d ' ')"
    fi
    python3 -c 'import sys; a=float(sys.argv[1]); b=float(sys.argv[2]); sys.exit(0 if (a <= b if sys.argv[3]=="at_most" else a >= b) else 1)' "$read_value" "$value" "$key"
}

# evaluate <expect-json> <output-file>; prints nothing, returns 0 when it holds.
evaluate() {
    local spec="$1" out="$2" key value candidate
    key="$(printf '%s' "$spec" | jq -r 'keys[0] // empty')"
    value="$(printf '%s' "$spec" | jq -r --arg k "$key" '.[$k] | tostring')"
    case "$key" in
    equals) [ "$(cat "$out")" = "$value" ] ;;
    contains) grep -qF -- "$value" "$out" ;;
    matches) grep -qE -- "$value" "$out" ;;
    exit) [ "$(cat "$out.status" 2>/dev/null || echo 1)" = "$value" ] ;;
    at_most | at_least) bound "$key" "$value" "$out" ;;
    one_of)
        # The candidates are read one per line, so a set of more than one holds when the
        # output carries any one of them.
        local found=1
        while IFS= read -r candidate; do
            [ -n "$candidate" ] || continue
            grep -qF -- "$candidate" "$out" && found=0
        done < <(printf '%s' "$spec" | jq -r --arg k "$key" '.[$k][]')
        [ "$found" -eq 0 ]
        ;;
    *) return 2 ;;
    esac
}

# A predicate is a bound, a set membership or an exit status. Anything else, and anything
# whose value reads as a sentence, fails: that is what the Verification document asks for.
predicate_is_a_sentence() {
    local spec="$1" key value
    key="$(printf '%s' "$spec" | jq -r 'keys[0] // empty')"
    case "$key" in
    equals | contains | matches | exit | at_most | at_least | one_of) ;;
    *) return 0 ;;
    esac
    case "$key" in
    at_most | at_least)
        value="$(printf '%s' "$spec" | jq -r --arg k "$key" '.[$k] | tostring')"
        printf '%s' "$value" | grep -qE '^-?[0-9]+(\.[0-9]+)?$' || return 0
        ;;
    exit)
        value="$(printf '%s' "$spec" | jq -r --arg k "$key" '.[$k] | tostring')"
        printf '%s' "$value" | grep -qE '^[0-9]+$' || return 0
        ;;
    esac
    return 1
}

# ---------------------------------------------------------------- --coverage

coverage_report() {
    printf 'What this gate reads, one line per assertion family.\n\n'
    printf '%-24s %-10s %s\n' 'family' 'mode' 'what it asserts'
    assertion_table | while IFS='|' read -r family mode asserts _planted; do
        printf '%-24s %-10s %s\n' "$family" "--$mode" "$asserts"
    done
    printf '\nWhat this gate does not read, and no run of it ever will:\n'
    cat <<'GAPS'
- whether the ranking is right. The pages carry a verdict, a cost and a default, and
  nothing here says the order is the order a reader should act in.
- whether the reconciliation is fair. It reads that the four fixed rows are present, not
  that the paragraph on where the two accounts differ is a fair reading of either.
- whether the reading is deep. It reads that each of the six mas-lab directories has an
  evidence file and that each of the three questions is answered; a file that answers
  shallowly passes.
- whether a cited text supports the claim beside it. It resolves the citation and reads
  the quoted words; the judgement of what they support is the reader's.
- whether a candidate that cannot be argued in one page was left off the list, and whether
  one fact is written once with the other half linking to it. Both are a reviewer's
  reading, and the plan of this issue names them so that nobody reads this gate as
  enforcing them.
- whether an engineer-day estimate is right. It reads the number on the page.
GAPS
}

# ---------------------------------------------------------------- --replay

# env.sh prepares every input a recorded command reads that this tree cannot hold and
# exports one variable per input. Sourcing it here puts those variables in this shell's
# environment, so that each recorded command reads them beside IOC_CLONES. The fixture of
# --self-test builds a deliverable whose rows need none of them and sets IOC_NO_ENV=1.
prepare_env() {
    [ "${IOC_NO_ENV:-0}" = 1 ] && return 0
    if [ ! -r "$EVIDENCE/env.sh" ]; then
        printf 'check.sh cannot run: %s is missing, and every recorded command needs what it prepares.\n' "$EVIDENCE/env.sh"
        exit 2
    fi
    # shellcheck source=env.sh
    . "$EVIDENCE/env.sh" || {
        printf 'check.sh cannot run: env.sh did not prepare what the recorded commands read.\n'
        exit 2
    }
}

run_recorded() {
    # run_recorded <command> <output-file>; the variables env.sh exported are already in
    # this shell's environment and reach the command through it.
    #
    # Beside the output and the status, the status of every stage of the command's last
    # pipeline is written to <output-file>.pipe. An absence search reads its corpus in the
    # first stage of such a pipeline, and a read that failed prints its refusal and leaves
    # the search with nothing to find, which is not the same fact as a search that found
    # nothing: the stage statuses are what tell the two apart.
    local command="$1" out="$2" script
    script="$out.sh"
    : >"$out.failed"
    {
        # Every command of the row that fails and that nothing guards writes its status and
        # its own text here, one line each, the status first. The status of a pipeline is
        # its last stage's, and the status of a brace group is its last command's, so
        # neither says that a read inside it failed; the ERR trap does, wherever the read
        # stands. The trap's first word expands "$?" before anything else runs, so the
        # status is the failing command's, and a command written over several lines is
        # written on one.
        printf '__ioc_failed=%q\n' "$out.failed"
        printf '%s\n' "__ioc_nl=\$'\\n'"
        printf '%s\n' 'set -o errtrace'
        printf '%s\n' "trap 'printf \"%s\\t%s\\n\" \"\$?\" \"\${BASH_COMMAND//\$__ioc_nl/ }\" >>\"\$__ioc_failed\"' ERR"
        printf '%s\n' "$command"
        printf '%s\n' '__ioc_stages=("${PIPESTATUS[@]}")'
        printf '%s\n' 'printf "%s\n" "${__ioc_stages[*]}" >&9'
        printf '%s\n' 'exit "${__ioc_stages[${#__ioc_stages[@]}-1]}"'
    } >"$script"
    (
        cd "$ROOT" || exit 1
        IOC_CLONES="$CLONES" EVIDENCE="$EVIDENCE" bash "$script" 9>"$out.pipe"
    ) >"$out" 2>&1
    printf '%s' "$?" >"$out.status"
}

# search_missed <command text> <status>; true when the command is a search that read its
# corpus and found nothing: its first word is grep, egrep, fgrep, rg or ag, or it is git
# with grep as its subcommand, and it exited exactly 1. A search that exits above 1 could
# not read what it searches, as a git grep over a missing clone exits 128, and any other
# command that failed is a failed read. This is the one rule every path below reads.
search_missed() {
    local status="$2" words i=0
    [ "$status" = 1 ] || return 1
    read -r -a words <<<"$1"
    # An assignment written before the command sets its environment and is no command.
    while [ "$i" -lt "${#words[@]}" ] && [[ "${words[$i]}" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]]; do
        i=$((i + 1))
    done
    case "${words[$i]:-}" in
    grep | egrep | fgrep | rg | ag) return 0 ;;
    git)
        # git's own options stand before its subcommand; -C and -c take a value.
        i=$((i + 1))
        while [ "$i" -lt "${#words[@]}" ]; do
            case "${words[$i]}" in
            -C | -c | --git-dir | --work-tree | --namespace) i=$((i + 2)) ;;
            -*) i=$((i + 1)) ;;
            *) break ;;
            esac
        done
        [ "${words[$i]:-}" = grep ]
        ;;
    *) return 1 ;;
    esac
}

# failed_read <output-file>; true when the ERR trap of the row's run recorded a command
# that failed and that is no search that missed, which is a read that failed wherever it
# stood in the row. It names that command and its status in IOC_FAILED_READ.
failed_read() {
    local out="$1" failed_status failed_command
    IOC_FAILED_READ=""
    [ -s "$out.failed" ] || return 1
    while IFS=$'\t' read -r failed_status failed_command; do
        search_missed "$failed_command" "$failed_status" && continue
        IOC_FAILED_READ="'$failed_command' exited $failed_status"
        return 0
    done <"$out.failed"
    return 1
}

# corpus_read_failed <output-file> <command>; true when a command of the row failed and
# was no search that missed, or when a stage before the last of the command's last
# pipeline did, which is a corpus this run never read.
#
# A search that finds nothing exits 1, and so does a read that could not reach what it
# reads, so the status alone does not tell them apart: the stage's own first word does.
# A search before the last stage may exit 1 and still have read its corpus; any other
# reader that exits non-zero read nothing, and the search after it found nothing because
# nothing was there to search.
corpus_read_failed() {
    local out="$1" command="$2" stages tail_pipeline
    IOC_CORPUS_READ_FAILED=""
    # Every unguarded failure the row's own commands made, whatever construct it stood in.
    if failed_read "$out"; then
        IOC_CORPUS_READ_FAILED="$IOC_FAILED_READ"
        return 0
    fi
    [ -s "$out.pipe" ] || return 1
    read -r -a stages <"$out.pipe" || return 1
    [ "${#stages[@]}" -gt 1 ] || return 1
    # The command's last pipeline, which is the one PIPESTATUS reports.
    tail_pipeline="$(printf '%s' "$command" | tr '\n' ';' | awk -F';' '{for (i = NF; i >= 1; i--) if ($i ~ /[^[:space:]]/) { print $i; exit } }')"
    local i stage
    for ((i = 0; i < ${#stages[@]} - 1; i++)); do
        [ "${stages[$i]}" = 0 ] && continue
        stage="$(printf '%s' "$tail_pipeline" | awk -F'[|]' -v n="$((i + 1))" '{print $n}')"
        search_missed "$stage" "${stages[$i]}" && continue
        IOC_CORPUS_READ_FAILED="the stage '$(printf '%s' "$stage" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')' of its last pipeline exited ${stages[$i]}"
        return 0
    done
    return 1
}


# github_budget
#
# About thirty-four recorded commands read api.github.com without a credential, which
# GitHub allows sixty of an hour for one address. A second --replay inside that hour runs
# out of budget, and every one of those rows then fails for a reason that is not this
# deliverable's: the gate would read GitHub's refusal as a claim that no longer holds. So
# the budget is read first, and a run that cannot carry those commands stops here, naming
# the budget and the moment it resets, rather than printing failures nobody should act on.
github_budget() {
    local answer remaining reset
    # The self-test's fixture reads no GitHub, and it sets IOC_NO_ENV as it does for env.sh.
    [ "${IOC_NO_ENV:-0}" = 1 ] && return 0
    answer="$(curl -sS https://api.github.com/rate_limit 2>/dev/null)" || return 0
    remaining="$(printf '%s' "$answer" | jq -r '.resources.core.remaining // empty' 2>/dev/null)"
    reset="$(printf '%s' "$answer" | jq -r '.resources.core.reset // empty' 2>/dev/null)"
    [ -n "$remaining" ] || return 0
    if [ "$remaining" -lt 40 ]; then
        printf 'check.sh cannot run --replay now: this address has %s of its 60 unauthenticated GitHub requests left and this mode makes about 34. The budget resets at %s.\n' \
            "$remaining" "$(date -u -d "@$reset" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || printf '%s' "$reset")"
        exit 2
    fi
}

# unpinned_installs; prints one line per package a recorded command installs or runs with
# no version, and per form this reader cannot read, as <id><TAB><what it runs>, over every
# row of claims.jsonl. It fails closed: what it does not recognise is refused and named,
# never passed.
#
# The rule. A package is pinned when it names a full release, nothing less:
#   - for Python (uvx, uv tool run, uv tool install, uv run --with, pipx run, pip install,
#     uv pip install), the name, then == and dotted digits only, such as pyshacl==0.40.2;
#   - for npm (bunx, npx, npm exec), the name, then @ and a full major.minor.patch, such
#     as tsx@4.1.0;
#   - for Go (go run, go install), the module, then @v and a full major.minor.patch, or a
#     Go pseudo-version, or a commit hash.
# A wildcard, a partial version, a tag such as latest or next, a comparison (>=, ~=, ^, ~)
# or any other spelling is no version. A local path, a variable of this deliverable that
# names one (IOC_...), a constraints or requirements file, and env.sh's pinned list
# IOC_ENV_PY_PINS are pinned. An option of a runner the reader does not know, a package
# written as any other variable, and a command it cannot open are refused.
#
# Every word in command position is read, and one this reader cannot resolve fails the
# row and is named. Command position is the first word of a command after its assignments
# and its keywords, the command a wrapper runs (env, nohup, nice, timeout, command, exec,
# sudo, stdbuf, time and xargs) and the command find runs after -exec. A command handed to
# a shell as a string (sh, bash, zsh, dash or ksh with -c, and eval) and the code of every
# substitution are read with this same reader, and the output of a substitution is no
# command word it can resolve. A command word is a literal, or one of a closed list of
# variables: $IOC_PY, which resolves to python, and $IOC_MASLAB_VENV,
# $IOC_MASLAB_WHEEL_VENV and $IOC_TC_WORK, each followed by a path whose last element is
# the program. A resolved word is held to every rule a literal is, so python code given
# to $IOC_PY -c that starts a process is refused as python -c is. Any other variable, a
# word built of several, and a command this reader cannot split are refused.
#
# A runner is known by the program a word names, not by how the word is spelled: the
# resolved word, its last path element, with a version after the name taken off, so
# /usr/bin/pip, "$IOC_MASLAB_VENV/bin/pip", pip3.12 and python3.12 -m pip are each pip and
# held to the rule for pip, and every runner above is matched that one way.
#
# What this reader cannot see. It compares text, and it guards against an install someone
# forgets to pin, not against one written to slip past it. A program that installs
# packages under a name no rule here names (a renamed copy of a runner, a wrapper script
# of the row's own, a script a row runs, or a binary that installs as a side effect) is
# outside what a reader of command text can see, and no further rule of this kind closes
# that. What covers it is a person reading every row at review, and env.sh, which pins
# the interpreters and environments the rows run.
unpinned_installs() {
    claims | python3 -c '
import json, re, shlex, sys

PY_PIN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*(\[[A-Za-z0-9,._-]+\])?==[0-9]+(\.[0-9]+)*$")
NPM_PIN = re.compile(r"^(@[A-Za-z0-9._-]+/)?[A-Za-z0-9][A-Za-z0-9._-]*@[0-9]+\.[0-9]+\.[0-9]+$")
GO_PIN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._~/-]*@(v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]*[0-9]{14}-[0-9a-f]{12})?|[0-9a-f]{7,40})$")
SHELLS = {"sh", "bash", "zsh", "dash", "ksh"}
PROCESS = re.compile(r"subprocess|os\.system|os\.popen|os\.exec|os\.spawn|pty\.spawn")

def local(arg):
    return arg.startswith((".", "/", "~")) or re.match(r"\$\{?IOC_[A-Z_]+\}?(/|$)", arg) is not None

def py_ok(arg):
    return local(arg) or PY_PIN.match(arg) is not None

def npm_ok(arg):
    return local(arg) or NPM_PIN.match(arg) is not None

def go_ok(arg):
    return local(arg) or GO_PIN.match(arg) is not None

OPERATORS = {";", "&", "&&", "|", "||", "(", ")", ";;", "|&", "&>", ">", "<", ">>", "<<", ">&", "<&", "2>", "<<<"}

def commands(text):
    # The commands of a line, split at the shell operators that end one, with quotes
    # respected, so an operator inside a quoted string splits nothing. None when the
    # text does not split, which the caller refuses.
    lexer = shlex.shlex(text, posix=True, punctuation_chars=";&|()<>")
    lexer.whitespace_split = True
    lexer.commenters = ""
    try:
        tokens = list(lexer)
    except ValueError:
        return None
    out, current, target = [], [], False
    for token in tokens:
        if target:
            # The word after a redirection is its target, a file and no command.
            target = False
            continue
        if (token in OPERATORS or set(token) <= set(";&|()<>")) and set(token) & set("<>"):
            # A redirection belongs to the command it stands in; a file descriptor
            # written before it, as in 2>, is part of it too.
            if current and current[-1].isdigit():
                current.pop()
            target = True
            continue
        if token in OPERATORS or set(token) <= set(";&|()<>"):
            if current:
                out.append(current)
            current = []
        else:
            current.append(token)
    if current:
        out.append(current)
    return out

# A command substitution, $(...) or a pair of backticks, that no backslash escapes; an
# escaped backtick is a literal one, as a quoted document holds.
SUBSTITUTION = re.compile(r"(?<!\\)[$<>]\(((?:[^()]|\([^()]*\))*)\)|(?<!\\)`((?:[^`\\]|\\.)*)`")
# Each substitution, once its code is read, stands in the line as this marker, inside the
# word it was written in, so an assignment from one stays an assignment and a
# command word that is the output of a substitution is refused as unresolved.
SUBST_WORD = "IOCSUBSTITUTION"

# The closed allow-list of variables a command word may be written with: the
# interpreters and environments env.sh builds and pins. Each is resolved to what env.sh
# makes it, and the word is then held to every rule a literal word is held to. A
# variable with no path after it resolves to the program named here; one with a path
# resolves to the last element of that path. No other variable may stand in command position.
RESOLVABLE = {
    "IOC_PY": "python",
    "IOC_MASLAB_VENV": None,
    "IOC_MASLAB_WHEEL_VENV": None,
    "IOC_TC_WORK": None,
}
KEYWORDS = {"do", "then", "else", "elif", "if", "while", "until", "!", "{", "}", "time", "done", "fi", "esac"}
CONTROL = {"for", "case", "select", "function", "in"}
ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?\+?=")
WRAPPERS = {"env", "nohup", "nice", "timeout", "command", "exec", "sudo", "stdbuf", "xargs", "time"}
WRAPPER_VALUE = {"nice": {"-n"}, "timeout": {"-s", "-k", "--signal", "--kill-after"},
                 "xargs": {"-n", "-I", "-L", "-P", "-s", "-d", "-E", "-a", "--max-args", "--replace",
                           "--max-procs", "--delimiter", "--arg-file"},
                 "sudo": {"-u", "-g", "-C"}, "stdbuf": {"-i", "-o", "-e"}, "env": {"-u", "-C", "-S"}}

def resolve(word):
    # The program a command word names, or None when this reader cannot resolve it.
    if SUBST_WORD in word or "`" in word:
        return None
    if "$" not in word:
        return word.rsplit("/", 1)[-1]
    m = re.match(r"^\$(\{)?([A-Z_]+)(?(1)\})(/[A-Za-z0-9._/-]*[A-Za-z0-9._-])?$", word)
    if not m or m.group(2) not in RESOLVABLE:
        return None
    if m.group(3):
        return m.group(3).rsplit("/", 1)[-1]
    return RESOLVABLE[m.group(2)]

def runner_name(word):
    # The program a word names, as every runner rule below matches it: the resolved
    # word, its last path element, with a version written after the name taken off, so
    # /usr/bin/pip, "$IOC_MASLAB_VENV/bin/pip", pip3 and pip3.12 are all pip. A word
    # this reader cannot resolve names no runner, and is refused in command position.
    base = resolve(word)
    if base is None:
        return ""
    return re.sub(r"[0-9][0-9.]*$", "", base)

def command_words(w):
    # The indexes of the words of w that stand in command position: the first word of the
    # command after its assignments and keywords, and the command each wrapper runs.
    i, out = 0, []
    while i < len(w):
        word = w[i]
        if ASSIGNMENT.match(word) or word in KEYWORDS:
            i += 1
            continue
        if word in CONTROL:
            return out
        break
    while i < len(w):
        out.append(i)
        base = resolve(w[i])
        if base not in WRAPPERS:
            # find runs the command after -exec or -execdir as well.
            if base == "find":
                for j in range(i + 1, len(w) - 1):
                    if w[j] in ("-exec", "-execdir", "-ok", "-okdir"):
                        out.append(j + 1)
            return out
        values = WRAPPER_VALUE.get(base, set())
        i += 1
        if base == "timeout" and i < len(w) and not w[i].startswith("-"):
            i += 1
        while i < len(w) and (w[i].startswith("-") or (base == "env" and ASSIGNMENT.match(w[i]))):
            if w[i].split("=", 1)[0] in values and "=" not in w[i]:
                i += 1
            i += 1
            if base == "timeout" and i < len(w) and not w[i].startswith("-") and re.match(r"^[0-9.]+[smhd]?$", w[i]):
                i += 1
    return out

# The options of each runner that take a value, and those that take none; any other
# option before the package is a form this reader does not know.
UV_TOOL_VALUE = {"--python", "-p", "--from", "--with", "--with-editable", "--with-requirements",
                 "--index", "--index-url", "--extra-index-url", "--default-index", "--cache-dir",
                 "--python-preference", "--constraints", "-c", "--overrides"}
UV_TOOL_FLAG = {"--isolated", "--offline", "--no-cache", "-q", "--quiet", "-v", "--verbose",
                "--refresh", "--no-project", "--no-config", "-n"}
PIPX_VALUE = {"--python", "--spec", "--index-url", "--pip-args"}
PIPX_FLAG = {"--no-cache", "-q", "--quiet", "-v", "--verbose"}
NODE_VALUE = {"--package", "-p", "--registry", "--cache"}
NODE_FLAG = {"-y", "--yes", "--no", "-q", "--quiet", "--silent", "--bun"}
GO_VALUE = {"-C", "-o", "-tags", "-ldflags", "-gcflags", "-mod", "-overlay", "-modfile", "-exec"}
GO_FLAG = {"-v", "-x", "-race", "-a", "-n", "-trimpath", "-buildvcs=false"}

def option_value(word):
    return word.split("=", 1)[1] if "=" in word else None

def read_runner(name, rest, value_opts, flag_opts, ok, pinned_by=()):
    problems, pins, i = [], {}, 0
    while i < len(rest):
        word = rest[i]
        if not word.startswith("-"):
            break
        key = word.split("=", 1)[0]
        if key in value_opts:
            value = option_value(word)
            if value is None:
                value = rest[i + 1] if i + 1 < len(rest) else ""
                i += 1
            pins.setdefault(key, []).append(value)
        elif word not in flag_opts and key not in flag_opts:
            return [name + " " + word + " (an option this reader does not know)"]
        i += 1
    for key in ("--with", "--with-editable"):
        for value in pins.get(key, []):
            if not py_ok(value):
                problems.append(name + " " + key + " " + value)
    package = rest[i] if i < len(rest) else ""
    source = None
    for key in pinned_by:
        if pins.get(key):
            source = pins[key][-1]
    target = source if source is not None else package
    if not target:
        problems.append(name + " (no package this reader can find)")
    elif target.startswith("$") and not local(target):
        problems.append(name + " " + target + " (a package this reader cannot read)")
    elif not ok(target):
        problems.append(name + " " + target)
    return problems

def read_command(command, depth=0):
    found = []
    if depth > 4:
        return ["a command nested too deep for this reader to open"]
    # No recorded command holds a heredoc, and its body is text this reader does not
    # split into commands, so a heredoc is refused rather than read as words.
    if re.search(r"<<-?\s*[\x27\x22]?[A-Za-z_]", command):
        return ["a heredoc, whose body this reader does not read"]
    # A command substitution runs whether or not it stands inside double quotes, and a
    # backtick pair may span two words, so it is read in the text before the split.
    for m in SUBSTITUTION.finditer(command):
        found += read_command(m.group(1) or m.group(2) or "", depth + 1)
    command = SUBSTITUTION.sub(SUBST_WORD, command)
    parts = commands(command)
    if parts is None:
        return found + ["a command this reader cannot split into words"]
    for w in parts:
        part = " ".join(w)
        for i in command_words(w):
            if resolve(w[i]) is None:
                what = "the output of a substitution" if SUBST_WORD in w[i] else w[i]
                found.append(what + " (a command word this reader cannot resolve)")
        for i, word in enumerate(w):
            nxt = w[i + 1] if i + 1 < len(w) else ""
            after = w[i + 1:]
            base = resolve(word) or word.rsplit("/", 1)[-1]
            name = runner_name(word)
            if name in SHELLS:
                j = i + 1
                while j < len(w) and w[j].startswith("-") and "c" not in w[j].lstrip("-"):
                    j += 1
                if j < len(w) and w[j].startswith("-") and "c" in w[j].lstrip("-"):
                    if j + 1 < len(w):
                        found += read_command(w[j + 1], depth + 1)
                    else:
                        found.append(base + " -c (no command this reader can find)")
            elif word == "eval":
                found += read_command(" ".join(after), depth + 1)
            elif name == "python" and nxt == "-c" and i + 2 < len(w):
                if PROCESS.search(w[i + 2]):
                    found.append(base + " -c (python code that starts a process, which this reader cannot open)")
            elif name == "uvx":
                found += read_runner("uvx", after, UV_TOOL_VALUE, UV_TOOL_FLAG, py_ok, ("--from",))
            elif name == "uv" and nxt == "tool" and i + 2 < len(w) and w[i + 2] in ("run", "install"):
                found += read_runner("uv tool " + w[i + 2], w[i + 3:], UV_TOOL_VALUE, UV_TOOL_FLAG, py_ok, ("--from",))
            elif name == "uv" and nxt == "tool":
                found.append("uv tool " + (w[i + 2] if i + 2 < len(w) else "") + " (a uv tool form this reader does not read)")
            elif name == "uv" and nxt == "run":
                # uv run fetches each --with, which every one of them must pin.
                rest, j = w[i + 2:], 0
                while j < len(rest):
                    key = rest[j].split("=", 1)[0]
                    if key in ("--with", "--with-editable"):
                        value = option_value(rest[j])
                        if value is None:
                            value = rest[j + 1] if j + 1 < len(rest) else ""
                            j += 1
                        if not py_ok(value):
                            found.append("uv run " + key + " " + value)
                    elif key == "--with-requirements":
                        j += 1
                    elif not rest[j].startswith("-"):
                        break
                    j += 1
            elif name == "uv" and nxt == "add":
                found.append("uv add (a form that installs and that this reader does not read)")
            elif name == "pipx" and nxt == "run":
                found += read_runner("pipx run", w[i + 2:], PIPX_VALUE, PIPX_FLAG, py_ok, ("--spec",))
            elif name == "pipx":
                found.append("pipx " + nxt + " (a pipx form this reader does not read)")
            elif name in ("bunx", "npx") or (name == "npm" and nxt == "exec"):
                label, rest = (name, after) if name != "npm" else ("npm exec", w[i + 2:])
                found += read_runner(label, rest, NODE_VALUE, NODE_FLAG, npm_ok, ("--package", "-p"))
            elif name in ("npm", "bun", "yarn", "pnpm") and nxt in ("install", "i", "add", "dlx", "x"):
                found.append(name + " " + nxt + " (a form that installs and that this reader does not read)")
            elif name == "go" and nxt in ("run", "install"):
                found += read_runner("go " + nxt, w[i + 2:], GO_VALUE, GO_FLAG, go_ok)
            elif name == "pip" and nxt == "install":
                rest = w[i + 2:]
                if any(a in ("-c", "--constraint", "-r", "--requirement") or a.startswith(("--constraint=", "--requirement=")) for a in rest) or "$IOC_ENV_PY_PINS" in part:
                    continue
                skip = False
                for a in rest:
                    if skip:
                        skip = False
                        continue
                    if a[:1] in (">", "<") or re.match(r"[0-9]*[<>]", a):
                        break
                    if a in ("--python", "-p", "--target", "--prefix", "--index-url", "--extra-index-url"):
                        skip = True
                        continue
                    if a.startswith("-"):
                        continue
                    if not py_ok(a):
                        found.append("pip install " + a)
    return found

for line in sys.stdin:
    row = json.loads(line)
    for what in read_command(row.get("command") or ""):
        print(row.get("id", "?") + "\t" + what)
'
}

mode_replay() {
    local work
    work="$(mktemp -d "${TMPDIR:-/tmp}/ioc-replay.XXXXXX")"
    # The scratch is removed at the end of this function rather than in a RETURN trap,
    # which bash would also fire in the functions this one calls. env.sh clones the three
    # repositories at the commits sources.json pins, so nothing clones them here.
    # The budget is read before the environment is prepared, so a run that cannot carry
    # those commands refuses at once rather than after an install.
    github_budget
    prepare_env
    # The header file every recorded curl of the Dispatch API reads; a pod with no bearer
    # leaves it unset, and those rows then fail on the read they could not make.
    IOC_DISPATCH_HEADER="$(dispatch_header)" && export IOC_DISPATCH_HEADER

    local kinds='project-code our-code behaviour published absence quotation'
    local marks='verified false unverified not-externally-checkable supported unsupported unreleased'
    local bad_form=0 bad_mark=0 bad_predicate=0 ran=0 command_failures="" absence_failures="" bad_quotation=0
    local row id kind mark command expect
    while IFS= read -r row; do
        id="$(printf '%s' "$row" | jq -r '.id')"
        kind="$(printf '%s' "$row" | jq -r '.kind // ""')"
        mark="$(printf '%s' "$row" | jq -r '.mark // ""')"
        case " $kinds " in *" $kind "*) ;; *)
            bad_form=$((bad_form + 1))
            fail claim-form "$id carries the kind '$kind', which is none of the five forms"
            ;;
        esac
        case " $marks " in *" $mark "*) ;; *)
            bad_mark=$((bad_mark + 1))
            fail claim-mark "$id carries the mark '$mark', which its requirement does not fix"
            ;;
        esac
        expect="$(printf '%s' "$row" | jq -c '.expect // empty')"
        if [ -n "$expect" ] && [ "$(printf '%s' "$expect" | jq 'if type == "object" then length else 0 end')" != 1 ]; then
            bad_predicate=$((bad_predicate + 1))
            fail predicate-form "$id carries $(printf '%s' "$expect" | jq -r 'if type == "object" then (keys | join(" and ")) else "no key" end') in one expect, and a row is held to exactly one predicate: $expect"
        elif [ -n "$expect" ] && predicate_is_a_sentence "$expect"; then
            bad_predicate=$((bad_predicate + 1))
            fail predicate-form "$id carries a predicate that is not a bound, a set membership or an exit status: $expect"
        fi
        if [ "$kind" = quotation ]; then
            printf '%s' "$row" | jq -e '.evidence.marked // empty' >/dev/null ||
                {
                    bad_quotation=$((bad_quotation + 1))
                    fail quotation-marked "$id quotes a project's prose and is not marked a claim of the project"
                }
        fi
        command="$(printf '%s' "$row" | jq -r '.command // ""')"
        [ -n "$command" ] || continue
        if [ -z "$expect" ]; then
            # A command with nothing to meet is a command nobody reads: the gate would run
            # it and compare nothing, so a recorded output the command no longer prints
            # would pass.
            bad_predicate=$((bad_predicate + 1))
            fail predicate-form "$id carries a command and no predicate, so nothing of its output is read"
            continue
        fi
        ran=$((ran + 1))
        run_recorded "$command" "$work/$id.out"
        if [ "$kind" = absence ]; then
            if corpus_read_failed "$work/$id.out" "$command"; then
                absence_failures="$absence_failures $id"
                fail absence-still-empty "$id searched a corpus this run never read: $IOC_CORPUS_READ_FAILED, so the search found nothing because nothing was read"
                continue
            fi
            if grep -q '[^[:space:]]' "$work/$id.out"; then
                if [ -n "$expect" ] && evaluate "$expect" "$work/$id.out"; then
                    :
                else
                    absence_failures="$absence_failures $id"
                    fail absence-still-empty "$id searched again and found something: $(head -c 160 "$work/$id.out" | tr '\n' ' ')"
                fi
            fi
            continue
        fi
        # A read that failed anywhere in the row leaves its output no reading of what the
        # row claims, whatever that output still meets: a traceback has enough lines for a
        # bound. A row whose predicate is an exit status claims the failure itself.
        if [ "$(printf '%s' "$expect" | jq -r 'keys[0]')" != exit ] && failed_read "$work/$id.out"; then
            command_failures="$command_failures $id"
            fail replay-command "$id ran its command and a read inside it failed: $IOC_FAILED_READ, so its output is no reading of what the row claims"
            continue
        fi
        if ! evaluate "$expect" "$work/$id.out"; then
            command_failures="$command_failures $id"
            fail replay-command "$id ran '$command' and its output does not meet $expect: $(head -c 160 "$work/$id.out" | tr '\n' ' ')"
        fi
    done < <(claims)

    local unpinned unpinned_id unpinned_what
    unpinned="$(unpinned_installs)"
    while IFS=$'\t' read -r unpinned_id unpinned_what; do
        [ -n "$unpinned_id" ] || continue
        fail install-pinned "$unpinned_id runs '$unpinned_what': a package with no version reads whatever release is newest, and a form this reader cannot read is refused rather than passed"
    done <<<"$unpinned"
    [ -z "$unpinned" ] && pass install-pinned "every package a recorded command installs or runs names its version, or is pinned by a constraints file, a pinned list or a local path"

    [ "$bad_form" -eq 0 ] && pass claim-form "every claim of $(claims | wc -l) carries one of the five forms"
    [ "$bad_mark" -eq 0 ] && pass claim-mark "every claim carries a mark its requirement fixes"
    [ "$bad_predicate" -eq 0 ] && pass predicate-form "every row with a command carries exactly one predicate, and each is a bound, a set membership or an exit status"
    [ "$bad_quotation" -eq 0 ] && pass quotation-marked "every quotation is marked a claim of the project and not of its code"
    [ -z "$command_failures" ] && pass replay-command "$ran recorded commands ran again, no read inside one failed, and each met the predicate its row carries"
    [ -z "$absence_failures" ] && pass absence-still-empty "every absence search still finds nothing"

    # The id list is read by three of the assertions below and built once here.
    claims | jq -r '.id' | sort -u >"$work/ids"
    replay_citation_rows "$work/ids"
    replay_mas_lab "$work/ids"
    replay_interview "$work/ids"
    replay_our_data
    replay_count
    rm -rf "$work"
}

replay_citation_rows() {
    # replay_citation_rows <id file>; the ids of claims.jsonl, one per line.
    local ids="$1" work label missing=0 total=0
    work="$(mktemp -d "${TMPDIR:-/tmp}/ioc-cit.XXXXXX")"
    for label in $(jq -r '.records_under_check[].label' "$EVIDENCE/sources.json"); do
        if ! record_text "$label" >"$work/record-$label.md"; then
            fail citation-row "Dispatch did not answer the read of the record labelled $label"
            return
        fi
        python3 "$EVIDENCE/citations.py" extract --input "$work/record-$label.md" --label "$label" >"$work/cit-$label.jsonl" || {
            fail citation-row "citations.py could not read the record labelled $label"
            return
        }
        while IFS= read -r cid; do
            total=$((total + 1))
            grep -qx "$cid" "$ids" || {
                missing=$((missing + 1))
                fail citation-row "the citation $cid of the record labelled $label has no row with a mark"
            }
        done < <(jq -r '.cid' "$work/cit-$label.jsonl")
    done
    [ "$missing" -eq 0 ] && pass citation-row "each of the $total citations of the two records under check has a row with a mark"
    rm -rf "$work"
}

replay_mas_lab() {
    # replay_mas_lab <id file>; the ids of claims.jsonl, one per line.
    local ids="$1" commit missing=0 directory
    commit="$(jq -r '.repositories[] | select(.name=="mas-lab") | .commit' "$EVIDENCE/sources.json")"
    for directory in library-standard ctl runtime library-eval library-telemetry library-kg; do
        if [ ! -s "$EVIDENCE/mas-lab/$directory.md" ]; then
            missing=$((missing + 1))
            fail mas-lab-directory "the directory $directory of mas-lab has no evidence file"
            continue
        fi
        grep -q "$commit" "$EVIDENCE/mas-lab/$directory.md" || {
            missing=$((missing + 1))
            fail mas-lab-directory "the evidence file of $directory does not name the clone commit $commit"
        }
    done
    [ "$missing" -eq 0 ] && pass mas-lab-directory "each of the six named directories of mas-lab has an evidence file at the clone commit"

    local unanswered=0 question
    for question in \
        "Question 1: can its specification format express Legion's phase workflow?" \
        "Question 2: could its experiment harness measure whether a change to a role's prompt or skill made Legion better?" \
        "Question 3: what its boundary enforcement does that our refusals do not"; do
        local file
        file="$(grep -rl -F "$question" "$EVIDENCE/mas-lab/" 2>/dev/null | head -1)"
        if [ -z "$file" ]; then
            unanswered=$((unanswered + 1))
            fail mas-lab-question "no evidence file answers: $question"
            continue
        fi
        # The answer names its evidence: at least one claim id of a row of claims.jsonl.
        local named=0 cid
        for cid in $(sed -n "/$(printf '%s' "$question" | sed 's/[][\.*^$\/]/\\&/g')/,/^## /p" "$file" | grep -o '\[[a-z0-9-]*\]' | tr -d '[]' | sort -u); do
            grep -qx "$cid" "$ids" && named=1
        done
        [ "$named" -eq 1 ] || {
            unanswered=$((unanswered + 1))
            fail mas-lab-question "the answer to '$question' in ${file##*/} names no row of claims.jsonl"
        }
    done
    [ "$unanswered" -eq 0 ] && pass mas-lab-question "each of the three questions is answered by a file that names its evidence"
}

replay_interview() {
    # replay_interview <id file>; the ids of claims.jsonl, one per line.
    local ids="$1" missing=0 id
    for id in i-scope-triggers i-three-tiers i-skills-drop-in; do
        grep -qx "$id" "$ids" || {
            missing=$((missing + 1))
            fail interview-fixed-row "the row $id, which the specification fixes, is missing"
        }
    done
    local measurements
    measurements="$(grep -c '^i-measure-' "$ids" || true)"
    [ "${measurements:-0}" -ge 1 ] || {
        missing=$((missing + 1))
        fail interview-fixed-row "no row records a measurement the interview states"
    }
    [ "$missing" -eq 0 ] && pass interview-fixed-row "the four rows the specification fixes are present, with $measurements measurement rows"
}

replay_our_data() {
    local bad=0 file
    for file in "$EVIDENCE"/tests/*.md; do
        [ -e "$file" ] || continue
        local input
        input="$(grep -m1 '^Input:' "$file" || true)"
        if [ -z "$input" ]; then
            bad=$((bad + 1))
            fail our-data-input "${file##*/} names no input"
            continue
        fi
        if ! printf '%s' "$input" | grep -qE 'dispatch://[A-Z]+-[0-9]+|[0-9a-f]{40}'; then
            bad=$((bad + 1))
            fail our-data-input "${file##*/} names an input that is neither a Dispatch key nor a commit of ours: $input"
        fi
        if printf '%s' "$input" | grep -qE 'tests/fixtures|/fixtures/|golden'; then
            bad=$((bad + 1))
            fail our-data-input "${file##*/} drives a fixture of the project rather than a record of ours: $input"
        fi
    done
    [ "$bad" -eq 0 ] && pass our-data-input "each test against our own records names the Dispatch key or the commit its input came from"
}

replay_count() {
    local window="$EVIDENCE/coordination/window.json" count="$EVIDENCE/coordination/count.md"
    if [ ! -s "$window" ]; then
        fail count-window "the count names no window"
        return
    fi
    local from to
    from="$(jq -r '.from // ""' "$window")"
    to="$(jq -r '.to // ""' "$window")"
    if printf '%s%s' "$from" "$to" | grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2}T.*[0-9]{4}-[0-9]{2}-[0-9]{2}T'; then
        pass count-window "the window is two instants, $from to $to"
    else
        fail count-window "the window is not two instants: from='$from' to='$to'"
    fi

    local classes='across sessions|between an agent and a person|inside one tree'
    local missing=0 class
    IFS='|' read -ra names <<<"$classes"
    for class in "${names[@]}"; do
        grep -qF "$class" "$count" || {
            missing=$((missing + 1))
            fail count-class "the count does not carry the class '$class'"
        }
    done
    [ "$missing" -eq 0 ] && pass count-class "the classes are the three the specification names"

    # Each class is a row of the table: the class, what it counted, and the denominator,
    # which is the records read in the window.
    local without=0 zero_classes="" class_row instances denominator
    # Every row of the table is read, not only the three classes: a sample counted beside
    # them carries a denominator of its own and may read zero over it.
    for class in "${names[@]}"; do
        grep -qE "^\| *$class *\| *[0-9]+ *\| *[0-9]+ *\|" "$count" || {
            without=$((without + 1))
            fail count-denominator "the class '$class' carries no row of the table with a count and a denominator"
        }
    done
    while IFS= read -r class_row; do
        class="$(printf '%s' "$class_row" | awk -F'|' '{gsub(/^ +| +$/,"",$2); print $2}')"
        instances="$(printf '%s' "$class_row" | awk -F'|' '{gsub(/[^0-9]/,"",$3); print $3}')"
        denominator="$(printf '%s' "$class_row" | awk -F'|' '{gsub(/[^0-9]/,"",$4); print $4}')"
        if [ "$instances" -eq 0 ] && [ "$denominator" -gt 0 ]; then
            zero_classes="$zero_classes '$class'"
        fi
    done < <(grep -E "^\|[^|]+\| *[0-9]+ *\| *[0-9]+ *\|" "$count")
    [ "$without" -eq 0 ] && pass count-denominator "every class carries its denominator, the records read in the window"

    local judgement
    judgement="$(judgement_text 2>/dev/null || true)"
    if [ -z "$zero_classes" ]; then
        pass count-zero-premise "no class reads zero over a denominator above zero"
    elif printf '%s' "$judgement" | grep -qF 'premise that failed'; then
        pass count-zero-premise "the class$zero_classes reads zero and the front page states that premise as failed"
    else
        fail count-zero-premise "the class$zero_classes reads zero over a denominator above zero and the front page states no failed premise"
    fi
}

# ---------------------------------------------------------------- --shape

page_blocks() {
    # Prints each candidate page's heading and its body, separated by a form feed.
    judgement_text | awk '/^### /{if(n)printf "\f"; n=1} n{print} END{if(n)printf "\f"}'
}

mode_shape() {
    local text
    text="$(judgement_text)" || {
        fail front-page "Dispatch did not answer the read of judgement.md"
        return
    }
    local pages
    pages="$(printf '%s\n' "$text" | grep -c '^### ' || true)"
    if [ "${pages:-0}" -ge 3 ] && [ "${pages:-0}" -le 7 ]; then
        pass page-count "the judgement carries $pages candidate pages, between the floor of three and the cap of seven"
    else
        fail page-count "the judgement carries $pages candidate pages, and the floor is three and the cap is seven"
    fi

    local front
    front="$(printf '%s\n' "$text" | sed -n '1,/^### /p')"
    local missing=0 part
    for part in "## The ranking" "## The one thing to do first" "## Where the authors' account and the checked reading differ" "## What was deliberately not read"; do
        printf '%s\n' "$front" | grep -qF "$part" || {
            missing=$((missing + 1))
            fail front-page "the front page has no section '$part'"
        }
    done
    [ "$missing" -eq 0 ] && pass front-page "the front page carries the ranking, the one thing to do first, the paragraph on where the two accounts differ, and what was not read"

    local parts=("**Question:**" "**Proposal:**" "**Verdict:**" "**Landing component:**" "**Cost:**" "**Deletes or replaces:**" "**Cost of reversing it:**" "**Default if nobody answers:**" "**Owner:**" "**Node model:**" "**Test:**")
    local bad_parts=0 bad_verdict=0 bad_commitment=0 page title verdict
    while IFS= read -r -d $'\f' page; do
        title="$(printf '%s\n' "$page" | head -1 | sed 's/^### //')"
        for part in "${parts[@]}"; do
            printf '%s\n' "$page" | grep -qF "$part" || {
                bad_parts=$((bad_parts + 1))
                fail page-parts "the page '$title' is short of $part"
            }
        done
        verdict="$(printf '%s\n' "$page" | grep -m1 -F '**Verdict:**' | sed 's/.*\*\*Verdict:\*\*[[:space:]]*//' | tr -d '`' | awk '{print $1}')"
        case "$verdict" in
        vendor | depend | port | reject) ;;
        *)
            bad_verdict=$((bad_verdict + 1))
            fail page-verdict "the page '$title' carries the verdict word '$verdict', which is none of vendor, depend, port and reject"
            ;;
        esac
        local others=0 word
        for word in vendor depend port reject; do
            [ "$word" = "$verdict" ] && continue
            printf '%s\n' "$page" | grep -m1 -F '**Verdict:**' | grep -qw "$word" && others=$((others + 1))
        done
        [ "$others" -eq 0 ] || {
            bad_verdict=$((bad_verdict + 1))
            fail page-verdict "the page '$title' carries more than one verdict word"
        }
        case "$verdict" in
        depend)
            printf '%s\n' "$page" | grep -qiE 'upstream (owner|stops)' || {
                bad_commitment=$((bad_commitment + 1))
                fail verdict-commitment "the depend page '$title' names no upstream owner and what we do when upstream stops"
            }
            ;;
        port)
            printf '%s\n' "$page" | grep -qiE 'the change in our code' || {
                bad_commitment=$((bad_commitment + 1))
                fail verdict-commitment "the port page '$title' names no change in our code"
            }
            ;;
        reject)
            printf '%s\n' "$page" | grep -qiE 'rejected because|the reason it is rejected' || {
                bad_commitment=$((bad_commitment + 1))
                fail verdict-commitment "the reject page '$title' names no reason"
            }
            ;;
        esac
        printf '%s\n' "$page" | grep -qE 'INFRA-1098/spec v[0-9]+' && printf '%s\n' "$page" | grep -qE 'INFRA-1253/spec v[0-9]+' || {
            bad_parts=$((bad_parts + 1))
            fail page-parts "the page '$title' does not name the versions of the two node-model specifications it was read against"
        }
    done < <(page_blocks)
    [ "$bad_parts" -eq 0 ] && pass page-parts "every candidate page carries all eleven of its parts"
    [ "$bad_verdict" -eq 0 ] && pass page-verdict "every candidate page carries exactly one verdict word"
    [ "$bad_commitment" -eq 0 ] && pass verdict-commitment "every verdict names what it commits us to"

    shape_fixed_pages "$text"
    shape_questions "$text"
    shape_triage "$text"
    shape_order
}

shape_fixed_pages() {
    local text="$1" bad=0
    # Each fixed page is found by the words its design is written in, and it answers that
    # design by name.
    local envoy definitions execution
    envoy="$(printf '%s\n' "$text" | awk '/^### /{p=0} /^### .*[Ee]nvoy/{p=1} p')"
    definitions="$(printf '%s\n' "$text" | awk '/^### /{p=0} /^### .*[Dd]efinitions/{p=1} p')"
    execution="$(printf '%s\n' "$text" | awk '/^### /{p=0} /^### .*[Ee]xecution record/{p=1} p')"
    if [ -z "$envoy" ] || ! printf '%s' "$envoy" | grep -qF 'quotes'; then
        bad=$((bad + 1))
        fail fixed-page "the page on structured metadata on an Envoy message does not answer the quotes field by name"
    fi
    if [ -z "$definitions" ] || ! printf '%s' "$definitions" | grep -qiE 'unit and its scale as values|unit and scale as values'; then
        bad=$((bad + 1))
        fail fixed-page "the page on one definitions record does not answer the typed definition carrying its unit and its scale as values"
    fi
    if [ -z "$execution" ] || ! printf '%s' "$execution" | grep -qF 'gen_ai.'; then
        bad=$((bad + 1))
        fail fixed-page "the page on an execution record does not answer the OpenTelemetry gen_ai names"
    fi
    [ "$bad" -eq 0 ] && pass fixed-page "the three pages the specification fixes exist and each answers by name the design already written for it"
}

shape_questions() {
    local text="$1" bad=0 number
    local listed
    listed="$(grep -c '^## [0-9]*\.' "$EVIDENCE/questions.md" || true)"
    while IFS= read -r number; do
        grep -qE "^## $number\." "$EVIDENCE/questions.md" || {
            bad=$((bad + 1))
            fail question-list "a page answers question $number, which the question list does not carry"
        }
    done < <(printf '%s\n' "$text" | grep -oE '\*\*Question:\*\*[[:space:]]*Q[0-9]+' | grep -oE '[0-9]+')
    for number in $(seq 1 "${listed:-0}"); do
        printf '%s\n' "$text" | grep -qE "\*\*Question:\*\*[[:space:]]*Q$number\b" && continue
        printf '%s\n' "$text" | grep -qE "[Qq]uestion $number( |,|\.|:)" || {
            bad=$((bad + 1))
            fail question-list "question $number is answered by no page and named on no front page"
        }
    done
    [ "$bad" -eq 0 ] && pass question-list "every page's question is on the list of $listed, and every question is answered by a page or named on the front page"
}

shape_triage() {
    local text="$1" bad=0 name
    for name in mas-lab scope cognition-protocols ioc-project.github.io dot-github; do
        [ -s "$EVIDENCE/triage/$name.md" ] || {
            bad=$((bad + 1))
            fail triage-named "the triage of $name is missing"
            continue
        }
    done
    # Every repository the triage dropped is named on the front page with its reason.
    local front
    front="$(printf '%s\n' "$text" | sed -n '1,/^### /p')"
    for name in mas-lab scope cognition-protocols ioc-project.github.io dot-github; do
        printf '%s' "$front" | grep -qF "$name" || {
            bad=$((bad + 1))
            fail triage-named "the front page does not name the repository $name"
        }
    done
    [ "$bad" -eq 0 ] && pass triage-named "every repository of the organisation is triaged and named on the front page"
}

shape_order() {
    local questions_commit history
    history="$(history_ref)"
    if ! git -C "$ROOT" rev-parse --verify --quiet "$history^{commit}" >/dev/null; then
        fail order-of-work "the history ref '$history' names no commit of this repository; IOC_HISTORY_REF names a ref that holds the pull request's history"
        return
    fi
    questions_commit="$(git -C "$ROOT" log --diff-filter=A --format='%H' "$history" -- docs/research/internet-of-cognition/questions.md | tail -1)"
    if [ -z "$questions_commit" ]; then
        fail order-of-work "no commit of this history adds the question list"
        return
    fi
    local bad=0 commit path
    # The question list lands in a commit of its own: the commit that adds it adds no
    # other file of this directory. A tree that added the evidence beside it would pass
    # the ancestry rule and break the order the specification fixes.
    local beside
    beside="$(git -C "$ROOT" show --name-only --format= "$questions_commit" | grep '^docs/research/internet-of-cognition/' | grep -v 'questions.md$' || true)"
    if [ -n "$beside" ]; then
        bad=$((bad + 1))
        fail order-of-work "the commit that adds the question list adds $(printf '%s' "$beside" | tr '\n' ' ') beside it"
    fi
    while IFS= read -r path; do
        commit="$(git -C "$ROOT" log --diff-filter=A --format='%H' "$history" -- "$path" | tail -1)"
        [ -n "$commit" ] || continue
        [ "$commit" = "$questions_commit" ] && continue
        git -C "$ROOT" merge-base --is-ancestor "$questions_commit" "$commit" || {
            bad=$((bad + 1))
            fail order-of-work "the commit that adds $path does not descend from the commit that adds the question list"
        }
    done < <(git -C "$ROOT" ls-tree -r --name-only "$history" -- docs/research/internet-of-cognition/mas-lab docs/research/internet-of-cognition/triage docs/research/internet-of-cognition/tests docs/research/internet-of-cognition/coordination docs/research/internet-of-cognition/similarity)
    [ "$bad" -eq 0 ] && pass order-of-work "the commit that adds the question list is an ancestor of every commit that adds a deep-read evidence file"
}

# ---------------------------------------------------------------- --citations

# paths_at <repo root> <commit> <pathspec>
#
# The paths a citation names, at one commit. A citation writes a path, a directory, a
# glob such as `*.py`, or several of those separated by a comma or by the word "and".
# Each is read here, so a citation of a set of files resolves when the commit holds at
# least one path of every part it names.
paths_at() {
    local root="$1" commit="$2" spec="$3" part regex names
    names="$(git -C "$root" ls-tree -r --name-only "$commit" 2>/dev/null)"
    [ -n "$names" ] || return 1
    while IFS= read -r part; do
        part="$(printf '%s' "$part" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
        [ -n "$part" ] || continue
        part="${part%%:*}"
        part="${part%/}"
        # A part written in words rather than as a path names a set a reader reads, so
        # the gate passes over it and holds the citation to the parts that are paths.
        case "$part" in
        *' '*) continue ;;
        esac
        case "$part" in
        *'*'*)
            regex="$(printf '%s' "$part" | sed 's/[.]/\\./g; s/[*]/[^\/]*/g')"
            case "$part" in
            */*) printf '%s\n' "$names" | grep -qE "^$regex(/|$)" || return 1 ;;
            *) printf '%s\n' "$names" | grep -qE "(^|/)$regex(/|$)" || return 1 ;;
            esac
            ;;
        *)
            printf '%s\n' "$names" | grep -qE "^$(printf '%s' "$part" | sed 's/[^a-zA-Z0-9_/-]/\\&/g')(/|$)" || return 1
            ;;
        esac
    done < <(printf '%s\n' "$spec" | sed 's/ and /\n/g; s/,/\n/g')
    # A citation whose every part is written in words, such as "20 pyproject.toml files
    # under backend/", names a set a reader reads rather than a path, and the gate holds
    # it to no path: every part that is a path has already been read above.
    return 0
}

# flatten_text reads standard input and prints it with every run of space taken out,
# including the spaces a page writes as a no-break space. Both sides of a quotation are
# read this way, because taking the tags out of a page moves where its spaces fall.
flatten_text() {
    python3 -c 'import re,sys; sys.stdout.write(re.sub(r"\s+", "", sys.stdin.read()))'
}

# copy_text <path>
#
# The readable text of a committed copy. A page fetched from a content system is HTML, so
# its sentences are read with the tags taken out and the runs of space collapsed; any
# other copy is read as it stands.
copy_text() {
    case "$1" in
    *.html | *.htm)
        python3 - "$1" <<'PYTHON'
import html, re, sys
raw = open(sys.argv[1], encoding="utf-8", errors="replace").read()
# A script element is kept: a page rendered by a client framework carries its own
# sentences inside one, and that is where a quotation of such a page is found. The
# JSON escapes of that script are read first, so the tags written inside it as
# \\u003cstrong\\u003e are tags here too and come out with the rest.
raw = re.sub(r"\\+u([0-9a-fA-F]{4})", lambda m: chr(int(m.group(1), 16)), raw)
text = html.unescape(re.sub(r"(?s)<[^>]+>", " ", raw))
print(re.sub(r"\s+", " ", text))
PYTHON
        ;;
    *) cat "$1" ;;
    esac
}

# resolve_symbol <id> <repo root> <commit> <pathspec> <symbol>
#
# How a symbol is read, which is a reading of its own and so is written here. A symbol
# ending in a line or a line range resolves when the file holds that line. Otherwise the
# symbol is split on commas and on the word "and", and each part is looked for in the
# paths the citation names: as literal text, as a line, as a file name, as a prefixed
# name, as a path, as a regular expression where the part holds `|` or `*`, and as the
# last element of a dotted name. A part written in words that none of those finds names a
# part of the file a reader judges, and this gate does not read it: it is written to
# $RUN_SCRATCH/unchecked-parts, which --citations prints, one line per part with its row,
# so that a part nothing checked is never counted as checked. Any other part that no
# search finds fails the citation.
resolve_symbol() {
    local id="$1" root="$2" commit="$3" file="$4" symbol="$5" family="${6:-citation-our-code}" part
    # A symbol that is a line or a line range, written `name.py:88-97` or as the bare
    # range the record writes beside a file, resolves when the file holds that line.
    local line_ref
    line_ref="$(printf '%s' "$symbol" | grep -oE '(^|:)[0-9]+(-[0-9]+)?[^0-9]*$' | grep -oE '[0-9]+' | head -1)"
    if [ -n "$line_ref" ] && [ -z "${file##*[*/]}" ]; then
        line_ref=""
    fi
    if [ -n "$line_ref" ]; then
        if [ "$(git -C "$root" show "$commit:${file%%:*}" 2>/dev/null | wc -l)" -ge "$line_ref" ]; then
            return 0
        fi
        fail "$family" "$id cites $symbol, and $file holds no such line at $commit"
        return 1
    fi
    # A citation may name more than one file. Each part of the symbol is looked for in
    # every path the citation names, because the record writes the two together.
    local pathspec=()
    local fpart
    while IFS= read -r fpart; do
        fpart="$(printf '%s' "$fpart" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
        fpart="${fpart%%:*}"
        fpart="${fpart%/}"
        case "$fpart" in '' | *' '*) continue ;; esac
        # A glob reaches across directories here, as the citation writes it, so git is
        # told to read it as one.
        case "$fpart" in *'*'*) fpart=":(glob)$fpart" ;; esac
        pathspec+=("$fpart")
    done < <(printf '%s\n' "$file" | sed 's/ and /\n/g; s/,/\n/g')
    # A citation whose file is written in words, such as "15 pyproject.toml files", names
    # a set a reader reads rather than a path, and the gate holds its symbol to no file:
    # the paths it does name were already read by paths_at.
    [ "${#pathspec[@]}" -gt 0 ] || return 0
    while IFS= read -r part; do
        part="$(printf '%s' "$part" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
        [ -n "$part" ] || continue
        if git -C "$root" grep -qF -- "$part" "$commit" -- "${pathspec[@]}" 2>/dev/null; then
            continue
        fi
        # A part that is a line or a line range, which a citation writes beside the names
        # it lists, resolves when one of the files it names holds that line.
        case "$part" in
        [0-9]*)
            local part_line held=1 path
            part_line="$(printf '%s' "$part" | grep -oE '^[0-9]+')"
            for path in "${pathspec[@]}"; do
                # A glob names many files and no line, so a number beside one is a figure
                # of the prose rather than a line of a file.
                case "$path" in *'*'*) held=0 ;; esac
                [ "$(git -C "$root" show "$commit:$path" 2>/dev/null | wc -l)" -ge "$part_line" ] && held=0
            done
            [ "$held" -eq 0 ] && continue
            ;;
        esac
        # A part that is a file name resolves as a path under the paths the citation names.
        case "$part" in
        *.[A-Za-z]*)
            # ls-tree does not read the glob magic git grep takes, so the whole listing is
            # filtered here, as paths_at filters it.
            git -C "$root" ls-tree -r --name-only "$commit" 2>/dev/null |
                grep -qE "(^|/)$(printf '%s' "$part" | sed 's/[^a-zA-Z0-9_/-]/\\&/g')$" && continue
            ;;
        esac
        # A prefixed name, such as a Turtle term `mas:sessionId`, resolves on its local
        # part too: the prefix names a namespace and the file declares the local name.
        case "$part" in
        *:*)
            git -C "$root" grep -qF -- "${part##*:}" "$commit" -- "${pathspec[@]}" 2>/dev/null && continue
            ;;
        esac
        # A symbol that is itself a path resolves as a path does.
        case "$part" in
        */*)
            paths_at "$root" "$commit" "$part" && continue
            ;;
        esac
        case "$part" in
        *'|'* | *'*'*)
            git -C "$root" grep -qE -- "$part" "$commit" -- "${pathspec[@]}" 2>/dev/null && continue
            ;;
        esac
        case "$part" in
        *' '*)
            # A part of a file named in words, which a reader judges and this gate does
            # not: it is reported as not checked.
            printf '%s\t%s\n' "$id" "$part" >>"$RUN_SCRATCH/unchecked-parts"
            continue
            ;;
        esac
        # A dotted name is read by its last element too, since a citation writes
        # `Envelope.Validate` where the file declares `Validate` on a receiver.
        case "$part" in
        *.*)
            git -C "$root" grep -qF -- "${part##*.}" "$commit" -- "${pathspec[@]}" 2>/dev/null && continue
            ;;
        esac
        fail "$family" "$id cites the symbol $part, which $file does not hold at $commit"
        return 1
    done < <(printf '%s\n' "$symbol" | sed 's/ and /\n/g; s/,/\n/g')
    return 0
}


# false_references
#
# A reference a row of claims.jsonl records as false is a reference the record under check
# wrote and Dispatch does not hold. It is that row's evidence rather than a citation of
# this directory, so --citations reads it there and not here.
false_references() {
    claims | jq -r 'select(.mark=="false") | [.claim, (.evidence|tostring), (.note // "")] | join(" ")' |
        grep -oE "$DISPATCH_REF" | sed 's/[.,;:)]*$//' | sort -u
}

# Every dispatch:// reference of this directory is read whole by this one pattern: its
# document, its block anchor after `#` and its version after `@`, so a bogus anchor is
# read as one rather than cut off at the `#`.
DISPATCH_REF='dispatch://[A-Za-z0-9][A-Za-z0-9/_.#@-]*'

# dispatch_ref_resolves <ref>; true when Dispatch holds the whole reference, its block
# anchor and its version included. dispatch read --ref reads a block and refuses a block
# it does not hold. It refuses a version of a whole document, which dispatch doc-read
# reads instead, and the shorthand KEY/spec@vN these pages write is the version N of the
# issue's primary document.
dispatch_ref_resolves() {
    local ref="$1" rest
    case "$ref" in
    *'#'*) dispatch read --ref "$ref" ;;
    dispatch://*/spec@v*)
        rest="${ref#dispatch://}"
        dispatch doc-read --issue "${rest%%/*}" --version "${ref##*@v}"
        ;;
    *@v*) dispatch doc-read --ref "$ref" ;;
    *) dispatch read --ref "$ref" ;;
    esac >/dev/null 2>&1
}


# report_live_pages
#
# What the Verification document asks of this mode beside the assertions: "where the live
# address answers, reports whether the live page still carries that sentence; a live page
# whose bytes have changed is reported as changed rather than failed, because a page
# served from a content system changes its bytes between any two fetches."
#
# So this is a report and no assertion: one line per published copy, naming its address
# and one of three readings. An address that does not answer is reported too, so the mode
# runs with no network, and a changed page fails nothing.
report_live_pages() {
    local address copy sentence live flat
    printf 'The live address of each published copy, read now. This is a report and no assertion: a page served from a content system changes its bytes between any two fetches.\n'
    while IFS=$'\t' read -r address copy; do
        [ -n "$address" ] && [ -n "$copy" ] || continue
        case "$copy" in */) continue ;; esac
        live="$RUN_SCRATCH/live-$(printf '%s' "$copy" | tr '/' '_')"
        if ! curl -sS -L --max-time 30 --max-filesize 20000000 -o "$live" "$address" 2>/dev/null || [ ! -s "$live" ]; then
            printf '  %s: unreachable, the address did not answer; the committed copy %s is what this reading rests on.\n' "$address" "$copy"
            continue
        fi
        if cmp -s "$live" "$EVIDENCE/$copy"; then
            printf '  %s: answered, and its bytes are the committed copy %s byte for byte.\n' "$address" "$copy"
            continue
        fi
        # The sentences this directory quotes from that copy, which is what a reader of a
        # changed page wants to know about.
        local missing=0 held=0
        while IFS= read -r sentence; do
            [ -n "$sentence" ] || continue
            flat="$(printf '%s' "$sentence" | flatten_text)"
            if copy_text "$live" | flatten_text | grep -qF -- "$flat"; then
                held=$((held + 1))
            else
                missing=$((missing + 1))
            fi
        done < <(claims | jq -r --arg c "$copy" 'select(.kind=="published") | select((.evidence.copy // "")==$c) | .evidence.quote // empty')
        if [ "$missing" -eq 0 ] && [ "$held" -eq 0 ]; then
            printf '  %s: answered and changed since it was fetched, and this reading quotes no sentence of it; the committed copy %s is what it rests on.\n' "$address" "$copy"
        elif [ "$missing" -eq 0 ]; then
            printf '  %s: answered and changed since it was fetched, and it still carries each of the %s sentences this reading quotes from %s.\n' "$address" "$held" "$copy"
        else
            printf '  %s: answered and changed since it was fetched, and it no longer carries %s of the %s sentences this reading quotes from %s, which the committed copy holds.\n' "$address" "$missing" "$((held + missing))" "$copy"
        fi
    done < <(jq -r '.published[] | select(.copy) | [.address, .copy] | @tsv' "$EVIDENCE/sources.json")
}

# report_unchecked_parts
#
# The symbol parts resolve_symbol did not read: each is written in words, as the name of
# a part of a file, and neither the literal search nor any other reading found it. This is
# a report and no assertion, one line per part with the row that cites it, and a count,
# so that a reader sees how much of the symbol column the citation assertions leave to them.
report_unchecked_parts() {
    local list="$RUN_SCRATCH/unchecked-parts" id part
    if [ ! -s "$list" ]; then
        printf 'No symbol part was left unchecked: every part is an identifier, a line, a prefixed name or a file name, or its text stands in the file.\n'
        return
    fi
    printf 'The symbol parts written in words that no search of the cited files found. These are not checked: each names a part of a file a reader judges.\n'
    while IFS=$'\t' read -r id part; do
        printf '  not checked: %s cites the part "%s"\n' "$id" "$part"
    done <"$list"
    printf '%d symbol parts were not checked.\n' "$(wc -l <"$list" | tr -d ' ')"
}

mode_citations() {
    local commit bad_ours=0 bad_theirs=0 bad_published=0 bad_dispatch=0 row id file symbol repo rcommit
    # The flattened text of each committed copy, read once: eight rows cite one page of
    # 400 KB, and reading it per row is the cost of the mode.
    local flat_dir="$RUN_SCRATCH/citations"
    mkdir -p "$flat_dir"
    commit="$(our_commit)"
    while IFS= read -r row; do
        id="$(printf '%s' "$row" | jq -r '.id')"
        case "$(printf '%s' "$row" | jq -r '.kind')" in
        our-code)
            file="$(printf '%s' "$row" | jq -r '.evidence.file // ""')"
            symbol="$(printf '%s' "$row" | jq -r '.evidence.symbol // ""')"
            file="${file%%:*}"
            # A path, a directory or a glob: git reads all three as a pathspec, and the
            # citation resolves when the commit holds at least one path it names. A file
            # this pull request adds is read at the head instead, since the base has none.
            if [ -z "$file" ] || ! paths_at "$ROOT" "$commit" "$file"; then
                if [ -n "$file" ] && paths_at "$ROOT" HEAD "$file"; then
                    :
                else
                    bad_ours=$((bad_ours + 1))
                    fail citation-our-code "$id cites $file, which this repository does not hold at $commit"
                    continue
                fi
            fi
            if [ -n "$symbol" ] && [ "$symbol" != "null" ]; then
                resolve_symbol "$id" "$ROOT" "$commit" "$file" "$symbol" || bad_ours=$((bad_ours + 1))
            fi
            ;;
        project-code)
            repo="$(printf '%s' "$row" | jq -r '.evidence.repo // ""')"
            rcommit="$(printf '%s' "$row" | jq -r '.evidence.commit // ""')"
            file="$(printf '%s' "$row" | jq -r '.evidence.file // ""')"
            file="${file%%:*}"
            local root
            root="$(clone_root "$repo")" || {
                bad_theirs=$((bad_theirs + 1))
                fail citation-project-code "$id cites the repository $repo, which could not be cloned"
                continue
            }
            if ! paths_at "$root" "$rcommit" "$file"; then
                bad_theirs=$((bad_theirs + 1))
                fail citation-project-code "$id cites $repo $file at $rcommit, which the clone does not hold"
            else
                symbol="$(printf '%s' "$row" | jq -r '.evidence.symbol // ""')"
                if [ -n "$symbol" ] && [ "$symbol" != "null" ]; then
                    resolve_symbol "$id" "$root" "$rcommit" "$file" "$symbol" citation-project-code ||
                        bad_theirs=$((bad_theirs + 1))
                fi
            fi
            ;;
        published)
            local copy address sentence
            copy="$(printf '%s' "$row" | jq -r '.evidence.copy // ""')"
            address="$(printf '%s' "$row" | jq -r '.evidence.address // ""')"
            sentence="$(printf '%s' "$row" | jq -r '.evidence.quote // ""')"
            [ -n "$copy" ] || copy="$(jq -r --arg a "$address" '.published[] | select(.address==$a) | .copy' "$EVIDENCE/sources.json")"
            if [ -z "$copy" ] || [ ! -s "$EVIDENCE/$copy" ]; then
                bad_published=$((bad_published + 1))
                fail citation-published "$id cites $address and names no committed copy this directory holds"
                continue
            fi
            # The checksum sources.json recorded of that copy, against the copy as it
            # stands: a page is evidence of what it said when it was fetched, so a copy
            # whose bytes moved since is no longer the page the claim was read at.
            local recorded_sum read_sum
            recorded_sum="$(jq -r --arg c "$copy" '.published[] | select(.copy==$c) | .sha256 // empty' "$EVIDENCE/sources.json")"
            if [ -z "$recorded_sum" ]; then
                bad_published=$((bad_published + 1))
                fail citation-published "$id cites the copy $copy, and sources.json records no checksum of it"
                continue
            fi
            read_sum="$(sha256sum "$EVIDENCE/$copy" | cut -d' ' -f1)"
            if [ "$read_sum" != "$recorded_sum" ]; then
                bad_published=$((bad_published + 1))
                fail citation-published "the copy $copy hashes $read_sum, and sources.json records $recorded_sum"
                continue
            fi
            # The sentence is looked for with the spaces taken out of both sides, because
            # taking the tags out of a page moves where its spaces fall.
            if [ -n "$sentence" ]; then
                local flat="$flat_dir/$(printf '%s' "$copy" | tr '/' '_')"
                [ -s "$flat" ] || copy_text "$EVIDENCE/$copy" | flatten_text >"$flat"
                if ! grep -qF -- "$(printf '%s' "$sentence" | flatten_text)" "$flat"; then
                    bad_published=$((bad_published + 1))
                    fail citation-published "$id quotes a sentence the committed copy $copy does not hold"
                fi
            fi
            ;;
        esac
    done < <(claims)
    [ "$bad_ours" -eq 0 ] && pass citation-our-code "every citation into our own repository names a path that stands at $commit, and every symbol part that is an identifier, a line, a prefixed name or a file name stands in it"
    [ "$bad_theirs" -eq 0 ] && pass citation-project-code "every citation into one of the projects names a path the clone holds at its pinned commit, and every symbol part that is an identifier, a line, a prefixed name or a file name stands in it"
    [ "$bad_published" -eq 0 ] && pass citation-published "every citation of a published document names a committed copy that holds the sentence quoted"

    report_unchecked_parts
    report_live_pages

    local ref
    while IFS= read -r ref; do
        dispatch_ref_resolves "$ref" || {
            bad_dispatch=$((bad_dispatch + 1))
            fail citation-dispatch "the reference $ref resolves to no node Dispatch holds"
        }
    done < <(grep -rhoE "$DISPATCH_REF" "$EVIDENCE" --include='*.md' --include='*.jsonl' |
        sed 's/[.,;:)]*$//' | sort -u | grep -vxF -f <(false_references) 2>/dev/null)
    [ "$bad_dispatch" -eq 0 ] && pass citation-dispatch "every dispatch:// reference of this directory resolves to a node Dispatch holds"

    if python3 "$EVIDENCE/citations.py" --self-test >/dev/null 2>&1; then
        pass citation-kinds "the extraction rule reads a citation of every kind it names, a number written in words among them"
    else
        fail citation-kinds "citations.py --self-test does not pass, so the extraction rule reads less than it names"
    fi
}

# ---------------------------------------------------------------- --landed

mode_landed() {
    local bad_proof=0 bad_refutation=0 page title commit command landed=0 history entry want_commit want_command named
    history="$(history_ref)"
    if ! git -C "$ROOT" rev-parse --verify --quiet "$history^{commit}" >/dev/null; then
        fail landed-proof "the history ref '$history' names no commit of this repository; IOC_HISTORY_REF names a ref that holds the pull request's history"
        return
    fi
    while IFS= read -r -d $'\f' page; do
        title="$(printf '%s\n' "$page" | head -1 | sed 's/^### //')"
        printf '%s\n' "$page" | grep -qE '\*\*Landed:\*\*' || continue
        landed=$((landed + 1))
        if printf '%s\n' "$page" | grep -qE '\*\*Landed:\*\*[[:space:]]*not made'; then
            # A step that is not made carries evidence in one of the five forms: a command
            # with its output, never a search that found nothing.
            printf '%s\n' "$page" | grep -qE '\*\*Refuted by:\*\*.*`.*`' || {
                bad_refutation=$((bad_refutation + 1))
                fail landed-refutation "the page '$title' says its step is not made and carries no command with output"
            }
            continue
        fi
        # The proof is the one this tree commits for the page's title, never the text of
        # the live page, which no reviewer of this pull request reads: the page must name
        # the committed commit and the committed command, and only the committed command
        # runs.
        entry="$(jq -c --arg t "$title" '[.landed // [] | .[] | select(.title == $t)] | first // empty' "$EVIDENCE/sources.json")"
        if [ -z "$entry" ]; then
            bad_proof=$((bad_proof + 1))
            fail landed-proof "the page '$title' carries a Landed line, and the landed list of sources.json commits no proof for that title"
            continue
        fi
        want_commit="$(printf '%s' "$entry" | jq -r '.commit // ""')"
        want_command="$(printf '%s' "$entry" | jq -r '.command // ""')"
        commit="$(printf '%s\n' "$page" | grep -m1 -oE '\*\*Landed:\*\*[[:space:]]*[0-9a-f]{7,40}' | grep -oE '[0-9a-f]{7,40}')"
        # The page may write the commit abbreviated; it names the committed one when git
        # reads the two as the same commit.
        named="$([ -n "$commit" ] && git -C "$ROOT" rev-parse --verify --quiet "$commit^{commit}" 2>/dev/null)"
        if [ -z "$named" ] || [ "$named" != "$(git -C "$ROOT" rev-parse --verify --quiet "$want_commit^{commit}" 2>/dev/null)" ]; then
            bad_proof=$((bad_proof + 1))
            fail landed-proof "the page '$title' names the commit '$commit', and sources.json commits '$want_commit' as the commit of that step"
            continue
        fi
        # A commit of this pull request is one this branch added (pr_commits): a commit of
        # the base branch, whether before the fork or brought in by a forward merge, is on
        # main and lands nothing here, so naming one is no proof of a step.
        if ! pr_commits | grep -qx "$named"; then
            bad_proof=$((bad_proof + 1))
            fail landed-proof "the page '$title' names the commit '$commit', which this pull request did not add: it is not on the pull request's own line of $history, past $(our_commit) and the base commits its forward merges brought in"
            continue
        fi
        command="$(printf '%s\n' "$page" | grep -m1 -oE '\*\*Proven by:\*\*[[:space:]]*`[^`]+`' | sed 's/.*`\(.*\)`/\1/')"
        if [ -z "$want_command" ] || [ "$command" != "$want_command" ]; then
            bad_proof=$((bad_proof + 1))
            fail landed-proof "the page '$title' names the proof command '$command', and sources.json commits '$want_command' as the proof of that step"
            continue
        fi
        if ! (cd "$ROOT" && bash -c "$want_command" >/dev/null 2>&1); then
            bad_proof=$((bad_proof + 1))
            fail landed-proof "the committed proof of the page '$title', '$want_command', exits non-zero"
        fi
    done < <(page_blocks)
    [ "$bad_proof" -eq 0 ] && pass landed-proof "each of the $landed landed pages names the commit and the command sources.json commits for it, the commit is one this pull request added, and the committed command passes"
    [ "$bad_refutation" -eq 0 ] && pass landed-refutation "a page that refutes a step carries a command with output"

    landed_order
    landed_no_refusal
    landed_third_change
}

landed_commit_of() {
    # shellcheck disable=SC2046
    git -C "$ROOT" log --no-walk --format='%H %s' $(pr_commits) | grep -m1 -F "$1" | cut -d' ' -f1
}

landed_order() {
    local rows rule
    rows="$(landed_commit_of 'two graders are recorded as two rows')"
    rule="$(landed_commit_of 'combines the reviewer')"
    if [ -z "$rows" ] || [ -z "$rule" ]; then
        fail landed-order "the two changes this issue commits to are not both in this pull request (rows='$rows' rule='$rule')"
        return
    fi
    if git -C "$ROOT" merge-base --is-ancestor "$rows" "$rule"; then
        pass landed-order "the commit of the shortfall rows is an ancestor of the commit of the combining rule"
    else
        fail landed-order "the commit of the shortfall rows is not an ancestor of the commit of the combining rule"
    fi
}

landed_no_refusal() {
    local touched
    touched="$(git -C "$ROOT" diff --name-only "$(pr_base)" "$(history_ref)" -- packages/daemon/internal/api packages/daemon/internal/workflow packages/envoy/internal/dispatch/api)"
    if [ -n "$touched" ]; then
        fail landed-no-refusal "a landed step touches a refusal path: $(printf '%s' "$touched" | tr '\n' ' ')"
        return
    fi
    local added
    added="$(git -C "$ROOT" diff "$(pr_base)" "$(history_ref)" -- '*.go' '*.ts' '*.sql' | grep -E '^\+' | grep -cE '"[A-Z][A-Z_]{4,}"' || true)"
    if [ "${added:-0}" -eq 0 ]; then
        pass landed-no-refusal "no landed step touches a refusal path of the daemon or of Dispatch, and none adds a refusal constant"
    else
        fail landed-no-refusal "a landed step adds what reads as a refusal constant ($added lines)"
    fi
}

landed_third_change() {
    # The third change the earlier reading proposed is a page with a cost and a default,
    # and the gate reads it by name rather than by the diff rule, because it could be
    # landed as prose in a skill and the diff rule alone would pass it.
    local hits
    hits="$(git -C "$ROOT" diff "$(pr_base)" "$(history_ref)" -- skills packages/daemon/internal/prompts |
        grep -E '^\+' | grep -ciE 'refus(e|ed|es|al).{0,80}(reviewer|review) completion|completion .{0,40}no pair result|REVIEWER_PAIR_REQUIRED' || true)"
    if [ "${hits:-0}" -eq 0 ]; then
        pass landed-third-change "the third change, a refusal of a reviewer completion with no pair result, is landed in no form"
    else
        fail landed-third-change "the third change is landed in this tree in $hits place(s), and it is a page with a cost and a default"
    fi
}

# ---------------------------------------------------------------- --published

mode_published() {
    local remote head work
    remote="$(jq -r '.our_repository.url' "$EVIDENCE/sources.json")"
    work="$(mktemp -d "${TMPDIR:-/tmp}/ioc-published.XXXXXX")"
    head="${IOC_PUBLISHED_COMMIT:-$(git ls-remote "$remote" refs/heads/main | cut -f1)}"
    if [ -z "$head" ]; then
        fail published-tree "GitHub did not answer the read of the default branch of $remote"
        rm -rf "$work"
        return
    fi
    # The default branch is fetched into a clone of this run's scratch and never into this
    # repository's own object store: the mode writes nothing outside its scratch.
    local clone="$work/clone"
    git init --quiet "$clone" 2>/dev/null &&
        git -C "$clone" fetch --quiet --depth 1 --no-tags "$remote" "$head" 2>/dev/null || {
        fail published-tree "the fetch of $head from $remote into the scratch clone failed"
        rm -rf "$work"
        return
    }
    local missing=0 path
    for path in questions.md check.sh citations.py sources.json claims.jsonl README.md interview/transcript.md; do
        git -C "$clone" cat-file -e "$head:docs/research/internet-of-cognition/$path" 2>/dev/null || {
            missing=$((missing + 1))
            fail published-tree "the default branch at $head does not carry docs/research/internet-of-cognition/$path"
        }
    done
    # The files each landed step changed stand there as the step left them, each read by
    # the two sentences its own step added and not by a word the file carried before.
    local sentence first second
    for path in skills/legion-worker/references/pr-body.md skills/legion-worker/references/merge-gate.md; do
        case "$path" in
        *pr-body.md)
            first='each pair row reads'
            second='not run (no runtime code)'
            ;;
        # The answered form, which is the rule this tree landed, and the clause that reads
        # it against the pair's own run condition. The `APPROVE` half stands in the
        # stricter form too, so reading it would pass on either.
        *)
            first='is answered in the review body'
            second='that touches no runtime code, both rows reading'
            ;;
        esac
        git -C "$clone" show "$head:$path" >"$work/published" 2>/dev/null || {
            missing=$((missing + 1))
            fail published-tree "the default branch at $head does not carry $path"
            continue
        }
        for sentence in "$first" "$second"; do
            grep -qF -- "$sentence" "$work/published" || {
                missing=$((missing + 1))
                fail published-tree "the published $path does not carry the landed step: '$sentence'"
            }
        done
    done
    [ "$missing" -eq 0 ] && pass published-tree "the default branch at $head carries the evidence, the transcript, this script and the files each landed step changed"

    local delivery=0
    local comments
    comments="$(dispatch_get '/api/v1/issues/INFRA-3134/comments?limit=200' 2>/dev/null || true)"
    printf '%s' "$comments" | grep -qF 'notifications.role.legion-infra-controller' || {
        delivery=$((delivery + 1))
        fail published-delivery "no comment of this issue records the publication of the ranked list to the project's controller"
    }
    local messages
    messages="$(dispatch_get '/api/v1/issues/INFRA-3134/messages?limit=200' 2>/dev/null || true)"
    printf '%s' "$messages" | grep -qiF 'ranking' || {
        delivery=$((delivery + 1))
        fail published-delivery "no message on this issue carries the ranking to the issue's assignee"
    }
    local upstream
    upstream="$(dispatch_get '/api/v1/issues/INFRA-1098/comments?limit=200' 2>/dev/null || true)"
    printf '%s' "$upstream" | grep -qF 'INFRA-3134' || {
        delivery=$((delivery + 1))
        fail published-delivery "no comment of dispatch://INFRA-1098 carries this issue's reading of the node model"
    }
    [ "$delivery" -eq 0 ] && pass published-delivery "the record of the delivery is on the issue: the message, the comment on dispatch://INFRA-1098 and the comment recording the publication"
    printf 'This mode reads the record of the publication to the controller, which is the comment above, and not the publication itself: an Envoy publication leaves no record a program can read.\n'
    rm -rf "$work"
}

# ---------------------------------------------------------------- --self-test

# The self-test builds one small deliverable of its own under $TMPDIR, proves the gate
# passes over it, then plants, one at a time, a bad case of every kind the gate reads and
# proves the gate fails and names the family. The planted list and the assertion table are
# one list: the table's fourth column is the fault, and a family whose fault is unplanted
# fails here.

fixture_build() {
    local dir="$1"
    local evidence="$dir/repo/docs/research/internet-of-cognition"
    mkdir -p "$evidence"/{mas-lab,triage,interview,tests,coordination,similarity,published}
    mkdir -p "$dir/repo/skills/legion-worker/references"
    mkdir -p "$dir/repo/packages/daemon/internal/api"
    cp "$EVIDENCE/citations.py" "$evidence/citations.py"
    cp "$EVIDENCE/check.sh" "$evidence/check.sh"

    cat >"$dir/repo/packages/daemon/internal/api/handoff.go" <<'GO'
package api

const TesterVerdictRequired = "TESTER_VERDICT_REQUIRED"
GO
    cat >"$dir/repo/skills/legion-worker/references/pr-body.md" <<'MD'
**Thermo:** the pair at the reviewed head, one row per half:
- `thermonuclear-deep-review`: <verdict>; <n> findings standing.
- `thermonuclear-code-quality`: <verdict>; <n> findings standing.
MD
    cat >"$dir/repo/skills/legion-worker/references/merge-gate.md" <<'MD'
The two halves of the pair decide one review between them: a `CHANGES` from either
`thermonuclear-deep-review` or `thermonuclear-code-quality` is a `REQUEST_CHANGES`.
MD

    cat >"$evidence/questions.md" <<'MD'
# The questions this reading answers

## 1. When a role writes a fact it read somewhere else, what carries the way back?

Raised by `packages/daemon/internal/api/handoff.go`.

## 2. What decides the round when the two graders disagree?

Raised by the merge gate.

## 3. A question no page answers.

Raised by nothing in particular.
MD

    local directory
    for directory in library-standard ctl runtime library-eval library-telemetry library-kg; do
        printf '# %s at COMMIT\n\nWhat it is [f-project-1].\n' "$directory" >"$evidence/mas-lab/$directory.md"
    done
    cat >>"$evidence/mas-lab/ctl.md" <<'MD'

## Question 1: can its specification format express Legion's phase workflow?

No, and the evidence is [f-project-1].
MD
    cat >>"$evidence/mas-lab/library-eval.md" <<'MD'

## Question 2: could its experiment harness measure whether a change to a role's prompt or skill made Legion better?

Not as it stands [f-project-1].
MD
    cat >>"$evidence/mas-lab/runtime.md" <<'MD'

## Question 3: what its boundary enforcement does that our refusals do not

It changes a call rather than refusing it [f-project-1].
MD
    for name in mas-lab scope cognition-protocols ioc-project.github.io dot-github; do
        printf '# %s\n\nTriaged [f-project-1].\n' "$name" >"$evidence/triage/$name.md"
    done
    printf '# The transcript\n\nHe said the third protocol is not released.\n' >"$evidence/interview/transcript.md"
    printf '# The rows\n\nThe three tiers [i-three-tiers].\n' >"$evidence/interview/claims.md"
    printf 'Input: dispatch://INFRA-3134\n\nIt ran [f-behaviour-1].\n' >"$evidence/tests/concord.md"
    printf 'Input: 9542c6fbbecd184ccee73523ab3defa39d6cf23a\n\nIt ran [f-behaviour-1].\n' >"$evidence/tests/scope.md"
    printf 'Input: dispatch://INFRA-3134\n\nIt ran [f-behaviour-1].\n' >"$evidence/tests/mas-lab.md"
    printf '{"from":"2026-10-29T00:00:00Z","to":"2026-11-12T00:00:00Z","project":"INFRA"}\n' >"$evidence/coordination/window.json"
    cat >"$evidence/coordination/count.md" <<'MD'
# The count

| class | instances | denominator |
| --- | --- | --- |
| across sessions | 10 | 100 |
| between an agent and a person | 5 | 100 |
| inside one tree | 20 | 100 |

Each class carries its denominator, the records read in the window.
MD
    printf '# The table\n\nNine pairs [f-behaviour-1].\n' >"$evidence/similarity/table.md"
    printf 'the quoted sentence of a published page\n' >"$evidence/published/page.txt"
    printf '# The fixture\n' >"$evidence/README.md"
    mkdir -p "$evidence/records"
    cat >"$evidence/records/under-check.md" <<'MD'
# The fixture record under check

It claims something about `packages/daemon/internal/api/handoff.go`, and it reports
thirteen normative invariants.
MD

    cat >"$evidence/claims.jsonl" <<'JSONL'
{"id":"f-project-1","requirement":"r3","source":"mas-lab","kind":"project-code","claim":"The runtime declares a boundary gate.","mark":"verified","evidence":{"repo":"mas-lab","commit":"COMMIT","file":"README.md","symbol":"README"},"note":"read again"}
{"id":"f-our-1","requirement":"r1","source":"this tree's measurement","kind":"our-code","claim":"The completion refusal names a tester verdict.","mark":"verified","evidence":{"file":"packages/daemon/internal/api/handoff.go","symbol":"TesterVerdictRequired","commit":"OURS"},"note":"read again"}
{"id":"f-behaviour-1","requirement":"r7","source":"this tree's measurement","kind":"behaviour","claim":"The script prints its own name.","mark":"verified","evidence":{"surface":"cli"},"command":"printf 'ran once\\n'","expect":{"contains":"ran once"},"output":"ran once","note":"ran again"}
{"id":"f-absence-1","requirement":"r5","source":"this tree's measurement","kind":"absence","claim":"Nothing names a pair refusal.","mark":"verified","evidence":{"scope":"the fixture tree"},"command":"grep -r 'REVIEWER_PAIR_REQUIRED' skills || true","expect":{"at_most":"0"},"output":"","note":"searched again"}
{"id":"f-quotation-1","requirement":"r2","source":"the interview","kind":"quotation","claim":"The authors say the third protocol is not released.","mark":"unreleased","evidence":{"source":"the interview","quote":"the third protocol is not released","marked":"a claim of the project, not of its code"},"note":"their words"}
{"id":"f-published-1","requirement":"r2","source":"a published page","kind":"published","claim":"The page carries the sentence.","mark":"supported","evidence":{"address":"https://example.org/page","fetched":"2026-11-12","section":"body","quote":"the quoted sentence of a published page","copy":"published/page.txt"},"note":"read in the committed copy"}
{"id":"i-three-tiers","requirement":"r2","source":"the interview","kind":"quotation","claim":"The three tiers exist and the third is unreleased.","mark":"unreleased","evidence":{"source":"the interview","quote":"the third protocol is not released","marked":"a claim of the project, not of its code"},"note":"their words"}
{"id":"i-scope-triggers","requirement":"r2","source":"the interview","kind":"absence","claim":"Nothing triggers the protocols.","mark":"unsupported","evidence":{"scope":"the fixture tree"},"command":"grep -r 'trigger-accord' skills || true","expect":{"at_most":"0"},"output":"","note":"searched again"}
{"id":"i-skills-drop-in","requirement":"r2","source":"the interview","kind":"project-code","claim":"The protocols ship as skills.","mark":"supported","evidence":{"repo":"mas-lab","commit":"COMMIT","file":"README.md","symbol":"README"},"note":"read again"}
{"id":"i-measure-one","requirement":"r2","source":"the interview","kind":"quotation","claim":"One misalignment took accuracy from 99% to 98%.","mark":"not-externally-checkable","evidence":{"source":"the interview","quote":"the third protocol is not released","marked":"a claim of the project, not of its code"},"note":"no published document of theirs repeats it"}
{"id":"u-1","requirement":"r1","source":"the fixture record","kind":"our-code","claim":"The record cites the completion refusal file.","mark":"verified","evidence":{"file":"packages/daemon/internal/api/handoff.go","symbol":"TesterVerdictRequired","commit":"OURS"},"note":"read again at the commit"}
{"id":"u-2","requirement":"r1","source":"the fixture record","kind":"project-code","claim":"The record counts thirteen normative invariants.","mark":"verified","evidence":{"repo":"mas-lab","commit":"COMMIT","file":"README.md","symbol":"README"},"note":"counted again"}
JSONL

    cat >"$evidence/sources.json" <<JSON
{
  "clone_root_env": "IOC_CLONES",
  "repositories": [
    {"name": "mas-lab", "url": "https://github.com/ioc-project/mas-lab", "commit": "COMMIT"}
  ],
  "our_repository": {"name": "legion", "url": "$dir/repo", "commit": "OURS"},
  "records_under_check": [
    {"ref": "the fixture record", "label": "u", "copy": "records/under-check.md"}
  ],
  "published": [
    {"id": "page", "address": "https://example.org/page", "fetched": "2026-11-12", "copy": "published/page.txt", "sha256": "PAGE_SHA"}
  ],
  "landed": [
    {"title": "An execution record under the gen_ai. names", "commit": "LANDED_ROWS", "command": "true"}
  ]
}
JSON

    cat >"$dir/judgement.md" <<'MD'
# The fixture judgement

## The ranking

1. Structured metadata on an Envoy message — port — 2 engineer-days.

## The one thing to do first

The Envoy field.

## Where the authors' account and the checked reading differ

The account describes the problem and the reading describes the artifacts.

## What was deliberately not read

- mas-lab: read in part.
- scope: read in part.
- cognition-protocols: read in part.
- ioc-project.github.io: the documentation site, read only where a claim cites a page.
- dot-github: the organisation profile page, which holds no code.
- Question 3 is answered by no page: it is named here, and nothing of theirs answers it.

## Candidate pages

### Structured metadata on an Envoy message

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** One optional field, `quotes: [{ref, token}]`.
- **Verdict:** port
- **Landing component:** Envoy
- **Cost:** 2 engineer-days
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** the Envoy tree.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- This is the change in our code it becomes.

### One definitions record

- **Question:** Q2 What decides the round when the two graders disagree?
- **Proposal:** A typed definition carrying its unit and its scale as values.
- **Verdict:** reject
- **Landing component:** Dispatch
- **Cost:** 4 engineer-days
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** the Dispatch tree.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because the measurement under it failed.

### An execution record under the gen_ai. names

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** Spans under the OpenTelemetry `gen_ai.` names.
- **Verdict:** port
- **Landing component:** Legion
- **Cost:** 6 engineer-days
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** the daemon tree.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- This is the change in our code it becomes.
- **Landed:** LANDED_ROWS
- **Proven by:** `true`
MD

    (
        cd "$dir/repo" || exit 1
        git init --quiet -b main .
        git config user.email fixture@example.org
        git config user.name fixture
        git add packages skills
        git commit --quiet -m "the base"
        printf '%s' "$(git rev-parse HEAD)" >"$dir/base"
        git add docs/research/internet-of-cognition/questions.md
        git commit --quiet -m "docs(research): the question list"
        git add docs
        git commit --quiet -m "docs(research): the evidence"
        git commit --quiet --allow-empty -m "feat(skills): each review round's two graders are recorded as two rows, not one verdict"
        printf '%s' "$(git rev-parse HEAD)" >"$dir/rows"
        git commit --quiet --allow-empty -m "feat(skills): the rule that combines the reviewer's two graders is written where the reviewer reads it"
    )
    local base rows mas_commit
    base="$(cat "$dir/base")"
    rows="$(cat "$dir/rows")"
    mas_commit="$(jq -r '.repositories[] | select(.name=="mas-lab") | .commit' "$EVIDENCE/sources.json")"
    sed -i "s/OURS/$base/g; s/COMMIT/$mas_commit/g" "$evidence/sources.json" "$evidence/claims.jsonl" "$evidence"/mas-lab/*.md
    sed -i "s/PAGE_SHA/$(sha256sum "$evidence/published/page.txt" | cut -d' ' -f1)/; s/LANDED_ROWS/$rows/" "$evidence/sources.json"
    # The commits above were made before those two names were resolved, so one more
    # commit leaves the fixture's working copy clean and a planted fault may check out.
    (cd "$dir/repo" && git add -A && git commit --quiet -m "docs(research): the pinned commits")
    sed -i "s/LANDED_ROWS/$rows/" "$dir/judgement.md"
}

fixture_run() {
    # fixture_run <dir> <mode>; prints the gate's own output. The fixture's own rows read
    # its own repository alone, so it needs nothing env.sh prepares and skips it.
    local dir="$1" mode="$2"
    IOC_ROOT="$dir/repo" \
        IOC_EVIDENCE="$dir/repo/docs/research/internet-of-cognition" \
        IOC_JUDGEMENT="$dir/judgement.md" \
        IOC_NO_ENV=1 \
        IOC_PUBLISHED_COMMIT="$(git -C "$dir/repo" rev-parse HEAD 2>/dev/null)" \
        bash "$dir/repo/docs/research/internet-of-cognition/check.sh" "$mode" 2>&1
}

fixture_plant() {
    # fixture_plant <dir> <family>; breaks the copy and returns 0, or 1 when the family
    # has no planted fault.
    local dir="$1" family="$2"
    local evidence="$dir/repo/docs/research/internet-of-cognition"
    case "$family" in
    claim-form) sed -i 's/"kind":"project-code"/"kind":"a reading"/' "$evidence/claims.jsonl" ;;
    claim-mark) sed -i 's/"mark":"verified"/"mark":""/' "$evidence/claims.jsonl" ;;
    predicate-form) sed -i 's/"expect":{"contains":"ran once"}/"expect":{"contains_a_sentence":"the output says it ran"}/' "$evidence/claims.jsonl" ;;
    install-pinned) sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || uvx pyshacl --version; printf 'ran once/" "$evidence/claims.jsonl" ;;
    replay-command) sed -i 's/"expect":{"contains":"ran once"}/"expect":{"contains":"ran twice"}/' "$evidence/claims.jsonl" ;;
    absence-still-empty) sed -i "s#grep -r 'REVIEWER_PAIR_REQUIRED' skills || true#grep -r 'thermonuclear' skills || true#" "$evidence/claims.jsonl" ;;
    quotation-marked) sed -i 's/"marked":"a claim of the project, not of its code"/"marked_as":"evidence of the code"/' "$evidence/claims.jsonl" ;;
    citation-row) sed -i '/"id":"u-1"/d' "$evidence/claims.jsonl" ;;
    mas-lab-directory) rm -f "$evidence/mas-lab/library-kg.md" ;;
    mas-lab-question) sed -i 's/\[f-project-1\]//' "$evidence/mas-lab/ctl.md" ;;
    interview-fixed-row) sed -i '/"id":"i-three-tiers"/d' "$evidence/claims.jsonl" ;;
    our-data-input) sed -i 's#^Input: dispatch://INFRA-3134#Input: tests/fixtures/golden-transcript.json#' "$evidence/tests/concord.md" ;;
    count-window) rm -f "$evidence/coordination/window.json" ;;
    count-class) sed -i 's/| inside one tree |/| inside one pod |/' "$evidence/coordination/count.md" ;;
    count-denominator) sed -i 's/| across sessions | 10 | 100 |/| across sessions | 10 |/' "$evidence/coordination/count.md" ;;
    count-zero-premise) sed -i 's/| inside one tree | 20 | 100 |/| inside one tree | 0 | 100 |/' "$evidence/coordination/count.md" ;;
    page-count) cat >>"$dir/judgement.md" <<'MD'

### An eighth candidate page

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** One more.
- **Verdict:** reject
- **Landing component:** Legion
- **Cost:** 1 engineer-day
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** nobody.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because nothing asks for it.

### A ninth candidate page

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** One more again.
- **Verdict:** reject
- **Landing component:** Legion
- **Cost:** 1 engineer-day
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** nobody.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because nothing asks for it.

### A tenth candidate page

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** One more and again.
- **Verdict:** reject
- **Landing component:** Legion
- **Cost:** 1 engineer-day
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** nobody.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because nothing asks for it.

### An eleventh candidate page

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** One more and again and again.
- **Verdict:** reject
- **Landing component:** Legion
- **Cost:** 1 engineer-day
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** nobody.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because nothing asks for it.

### A twelfth candidate page

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** The last one.
- **Verdict:** reject
- **Landing component:** Legion
- **Cost:** 1 engineer-day
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** nobody.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because nothing asks for it.
MD
        ;;
    page-parts) sed -i '0,/- \*\*Cost of reversing it:\*\* one commit./{/- \*\*Cost of reversing it:\*\* one commit./d}' "$dir/judgement.md" ;;
    page-verdict) sed -i '0,/- \*\*Verdict:\*\* port/{s/- \*\*Verdict:\*\* port/- **Verdict:** port and reject/}' "$dir/judgement.md" ;;
    verdict-commitment) sed -i 's/- It is rejected because the measurement under it failed./- It is not for us./' "$dir/judgement.md" ;;
    fixed-page) sed -i 's/- \*\*Proposal:\*\* One optional field, `quotes: \[{ref, token}\]`./- **Proposal:** Something on the message./' "$dir/judgement.md" ;;
    front-page) sed -i "/^## Where the authors' account and the checked reading differ$/d" "$dir/judgement.md" ;;
    question-list) sed -i '0,/\*\*Question:\*\* Q1/{s/\*\*Question:\*\* Q1/**Question:** Q9/}' "$dir/judgement.md" ;;
    triage-named) sed -i '/^- dot-github: /d' "$dir/judgement.md" ;;
    order-of-work)
        (
            cd "$dir/repo" || exit 1
            # The branch is moved without a checkout, so the planted tree keeps every
            # file and only its history changes: one commit adds the question list and
            # the evidence together.
            git branch -f replanted "$(cat "$dir/base")"
            git symbolic-ref HEAD refs/heads/replanted
            git reset --quiet
            git add -A
            git commit --quiet -m "docs(research): the evidence and the question list in one commit, out of order"
        )
        ;;
    citation-our-code) sed -i 's/"symbol":"TesterVerdictRequired"/"symbol":"NameNoFileHolds"/' "$evidence/claims.jsonl" ;;
    citation-project-code) sed -i 's#"file":"README.md","symbol":"README"#"file":"README.md","symbol":"NoTextThatFileHolds"#' "$evidence/claims.jsonl" ;;
    citation-published) printf 'a byte the fetched page did not carry\n' >>"$evidence/published/page.txt" ;;
    citation-dispatch) printf '\nSee dispatch://INFRA-99999999/artifact/no-such-document.\n' >>"$evidence/tests/concord.md" ;;
    citation-kinds) sed -i 's/^    if first in TENS:/    if False:/; s/^    if first in WORD_NUMBERS:/    if False:/' "$evidence/citations.py" ;;
    landed-proof) sed -i 's/- \*\*Proven by:\*\* `true`/- **Proven by:** `false`/' "$dir/judgement.md" ;;
    landed-refutation)
        cat >>"$dir/judgement.md" <<'MD'
- **Landed:** not made
- **Refuted by:** a reading of the page with no command behind it.
MD
        ;;
    landed-order)
        (
            cd "$dir/repo" || exit 1
            # Two more commits with the same two subjects, in the other order, so the
            # newest row commit is a descendant of the newest rule commit.
            git commit --quiet --allow-empty -m "feat(skills): the rule that combines the reviewer's two graders is written where the reviewer reads it"
            git commit --quiet --allow-empty -m "feat(skills): each review round's two graders are recorded as two rows, not one verdict"
        )
        ;;
    landed-no-refusal)
        mkdir -p "$dir/repo/packages/daemon/internal/workflow"
        printf 'package workflow\n\nconst ReviewerPairRequired = "REVIEWER_PAIR_REQUIRED"\n' >"$dir/repo/packages/daemon/internal/workflow/review.go"
        (
            cd "$dir/repo" || exit 1
            git add packages
            git commit --quiet -m "a landed step that reaches a refusal path"
        )
        ;;
    landed-third-change)
        printf '\nA reviewer completion with no pair result recorded at the head is refused.\n' >>"$dir/repo/skills/legion-worker/references/merge-gate.md"
        (
            cd "$dir/repo" || exit 1
            git add skills
            git commit --quiet -m "the third change, landed as prose in a skill"
        )
        ;;
    *) return 1 ;;
    esac
    return 0
}

mode_self_test() {
    need git jq python3
    local work baseline
    work="$(mktemp -d "${TMPDIR:-/tmp}/ioc-selftest.XXXXXX")"
    fixture_build "$work/base"
    local mode modes="replay shape citations landed"
    local broken=0
    for mode in $modes; do
        baseline="$(fixture_run "$work/base" "--$mode")"
        if printf '%s' "$baseline" | tail -1 | grep -q '^all [0-9]* assertions passed$'; then
            pass "self-test-baseline" "the fixture deliverable passes --$mode"
        else
            broken=$((broken + 1))
            fail "self-test-baseline" "the fixture deliverable does not pass --$mode: $(printf '%s' "$baseline" | grep '^FAIL' | head -3 | tr '\n' ' ')"
        fi
    done
    [ "$broken" -eq 0 ] || return 0

    local family planted
    while IFS='|' read -r family mode asserts fault; do
        [ "$fault" = "-" ] && continue
        planted="$work/$family"
        rm -rf "$planted"
        mkdir -p "$planted"
        cp -r "$work/base/repo" "$planted/repo"
        cp "$work/base/judgement.md" "$planted/judgement.md"
        cp "$work/base/base" "$planted/base"
        cp "$work/base/rows" "$planted/rows"
        if ! fixture_plant "$planted" "$family"; then
            fail "$family" "the self-test plants no fault of this kind, so nothing proves the gate reads it"
            continue
        fi
        local out
        out="$(fixture_run "$planted" "--$mode")"
        if printf '%s' "$out" | grep -q "^FAIL $family"; then
            pass "$family" "the gate fails the planted fault and names it: $fault"
        else
            fail "$family" "the gate passed the planted fault ($fault)"
        fi
        rm -rf "$planted"
    done < <(assertion_table)

    # The faults the tester of this issue found the gate passing are cases of their own,
    # in check.test.sh beside this script, because each of them is a second fault of a
    # family this table already plants once. The self-test runs them here, so one run
    # proves both lists.
    if [ -r "$EVIDENCE/check.test.sh" ]; then
        local cases
        cases="$(bash "$EVIDENCE/check.test.sh" 2>&1)"
        if printf '%s\n' "$cases" | tail -1 | grep -q '^all [0-9]* cases passed$'; then
            pass gate-cases "check.test.sh: $(printf '%s\n' "$cases" | tail -1)"
        else
            fail gate-cases "check.test.sh does not pass: $(printf '%s\n' "$cases" | grep '^FAIL' | head -3 | tr '\n' ' ')"
        fi
    else
        fail gate-cases "check.test.sh is missing, so the faults the tester found are proved by nothing"
    fi

    # --coverage names every family the gate holds, so a mode nothing runs is still read.
    local missing=0 coverage
    coverage="$(bash "$EVIDENCE/check.sh" --coverage)"
    while IFS='|' read -r family _mode _asserts _fault; do
        printf '%s\n' "$coverage" | grep -q "^$family " || {
            missing=$((missing + 1))
            fail coverage-complete "the coverage report does not name the family $family"
        }
    done < <(assertion_table)
    [ "$missing" -eq 0 ] && pass coverage-complete "the coverage report names every assertion family the gate holds"
    rm -rf "$work"
}

main() {
    local mode="${1:---help}"
    case "$mode" in
    --help | -h) usage ;;
    --coverage) coverage_report ;;
    --replay) need git jq python3 && { mode_replay; finish; } ;;
    --shape) need git jq && { mode_shape; finish; } ;;
    --citations) need git jq curl python3 && { mode_citations; finish; } ;;
    --landed) need git jq && { mode_landed; finish; } ;;
    --published) need git jq curl && { mode_published; finish; } ;;
    --self-test) need git jq python3 && { mode_self_test; finish; } ;;
    *)
        usage
        exit 2
        ;;
    esac
}

# Sourced, as check.test.sh sources it for the fixture builder and runner, the script
# defines its functions and runs no mode.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
    main "$@"
fi
