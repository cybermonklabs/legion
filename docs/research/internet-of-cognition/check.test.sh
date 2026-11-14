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
# honest form beside it, a local path installed under the same constraints file, passes.
dir="$(fresh constraint-beside-package)"
sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || uv pip install -c c.txt requests; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
grep -qF 'uv pip install -c c.txt requests' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
    printf 'FAIL the form uv pip install -c c.txt requests could not be planted\n'
    exit 1
}
case_result "a package beside a constraints file is held to its own version" install-pinned "$(fixture_run "$dir" --replay)"
dir="$(fresh constraint-beside-local)"
sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || uv pip install -c c.txt .\/local; printf 'ran once/" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
grep -qF 'uv pip install -c c.txt ./local' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
    printf 'FAIL the form uv pip install -c c.txt ./local could not be planted\n'
    exit 1
}
out="$(fixture_run "$dir" --replay)"
if printf '%s\n' "$out" | grep -q '^FAIL install-pinned'; then
    CASES_FAILED=$((CASES_FAILED + 1))
    printf 'FAIL %s: %s\n' "a local path beside a constraints file passes install-pinned" "$(printf '%s\n' "$out" | grep '^FAIL install-pinned' | head -1)"
else
    CASES_PASSED=$((CASES_PASSED + 1))
    printf 'PASS %s\n' "a local path beside a constraints file passes install-pinned"
fi

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

# honest_result <name> <family> <gate output>; the honest form beside a planted fault,
# which passes when the gate names no failure of that family.
honest_result() {
    local name="$1" family="$2" out="$3"
    if printf '%s\n' "$out" | grep -qE "^FAIL ($family)"; then
        CASES_FAILED=$((CASES_FAILED + 1))
        printf 'FAIL %s: %s\n' "$name" "$(printf '%s\n' "$out" | grep -E "^FAIL ($family)" | head -1)"
    else
        CASES_PASSED=$((CASES_PASSED + 1))
        printf 'PASS %s\n' "$name"
    fi
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
case_result "an entry of ADMITTED that no row uses fails --admitted, named" 'admitted-form: until is used by no row' "$(bash "$GATE" --admitted 2>&1)"

# 42. The table is the classifier's only admission: with the entry && taken out of
# ADMITTED, every row that joins two pipelines with && is refused, and --admitted fails
# naming a refused row and the form.
plant_gate admitted-missing '/^    "&&": /d' '    "&&": ' lacks
case_result "an entry the rows use, taken out of ADMITTED, refuses those rows" 'admitted-rows: the row [^ ]+ is refused: the operator &&' "$(bash "$GATE" --admitted 2>&1)"

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
# [python|evid] plants a command into a fresh fixture and sets PLANTED_OUT to the gate's
# --replay output, planting in this shell so that a command that cannot be planted stops
# the run; with python the run sets IOC_PY, and with evid IOC_EVID, to the fixture's own
# directory, each of which env.sh sets and the fixture does not.
# refused_read <name> <command> <refusal, an extended regular expression> [python|evid]
# plants one fault; admitted_read <name> <command> [python|evid] plants the honest form
# beside it, which passes.
planted_run() {
    local dir
    dir="$(fresh "$1")"
    plant_behaviour "$dir" "$2" '{"contains":"ran once"}'
    case "${3:-}" in
    python) PLANTED_OUT="$(IOC_PY="$(command -v python3)" fixture_run "$dir" --replay)" ;;
    evid) PLANTED_OUT="$(IOC_EVID="$dir/repo/docs/research/internet-of-cognition" fixture_run "$dir" --replay)" ;;
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
refused_read xargs-env "printf 'x\\n' | xargs env sh -c 'cat /nonexistent-ioc 2>/dev/null; true'; printf 'ran once\\n'" 'env running a program'
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
case_result "a program variable taken out of PROGRAM_VARIABLES fails --admitted" 'admitted-reserved: the header' "$(bash "$GATE" --admitted 2>&1)"

# 51. A name of the runner that starts with no prefix of RUNNER_PREFIXES is one no row
# is kept from touching, and --admitted fails it by name.
plant_gate runner-name-unreserved 's/__ioc_stages/__gate_stages/g' '__gate_stages' holds
case_result "a name of the runner that no reserved prefix covers fails --admitted" 'admitted-reserved: the runner or the shims record with a name no prefix of RUNNER_PREFIXES reserves: __gate_stages' "$(bash "$GATE" --admitted 2>&1)"

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
# env now takes no option; the one row that runs env, with none, passes.
refused_read env-split-string "env \"-Ssh -c 'cat /nonexistent-ioc 2>/dev/null; true'\"; printf 'ran once\\n'" 'env -Ssh -c .cat /nonexistent-ioc 2>/dev/null; true., an option of env'
admitted_read env-alone "env | grep -c . >/dev/null; printf 'ran once\\n'"

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
# tc-run rows have. An unpinned --with in that shape is now refused; the real form, a
# local path under $IOC_CLONES, passes.
dir="$(fresh uv-run-python-with)"
sed -i "s#\"command\":\"printf 'ran once#\"command\":\"true || uv run --no-project --python 3.12 --with requests python -c 1; printf 'ran once#" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
case_result "an unpinned --with after uv run --python is refused" install-pinned "$(fixture_run "$dir" --replay)"
dir="$(fresh uv-run-python-with-local)"
sed -i "s#\"command\":\"printf 'ran once#\"command\":\"true || uv run --no-project --python 3.12 --with \$IOC_CLONES/cognition-protocols/packages/ioc-concord python -c 1; printf 'ran once#" "$dir/repo/docs/research/internet-of-cognition/claims.jsonl"
grep -qF 'uv run --no-project --python 3.12 --with $IOC_CLONES/' "$dir/repo/docs/research/internet-of-cognition/claims.jsonl" || {
    printf 'FAIL the honest uv run --with form could not be planted\n'
    exit 1
}
honest_result "a local path after uv run --python passes install-pinned" install-pinned "$(fixture_run "$dir" --replay)"

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

total=$((CASES_PASSED + CASES_FAILED))
if [ "$CASES_FAILED" -eq 0 ]; then
    printf 'all %d cases passed\n' "$total"
    exit 0
fi
printf '%d of %d cases failed\n' "$CASES_FAILED" "$total"
exit 1
