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
# PIPESTATUS of the row's last pipeline, so `ls /nonexistent-ioc && true` failed unseen;
# the function hook that wraps ls records it. A search that missed before && passes.
dir="$(fresh failed-before-and)"
plant_behaviour "$dir" "ls /nonexistent-ioc && true; printf 'ran once\\n'" '{"contains":"ran once"}'
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

# 27. A declared status is the one status a program may exit with. ls exits 2 on a path
# it cannot read, so IOC_EXPECT_STATUS=1 does not cover it and the read fails the row;
# IOC_EXPECT_STATUS=2 declares exactly that status and passes.
dir="$(fresh declared-status-differs)"
plant_behaviour "$dir" "IOC_EXPECT_STATUS=1 ls /nonexistent-ioc; printf 'ran once\\n'" '{"contains":"ran once"}'
case_result "a status other than the declared one fails the row" replay-command "$(fixture_run "$dir" --replay)"
dir="$(fresh declared-status-holds)"
plant_behaviour "$dir" "IOC_EXPECT_STATUS=2 ls /nonexistent-ioc; printf 'ran once\\n'" '{"contains":"ran once"}'
honest_result "the declared status passes" replay-command "$(fixture_run "$dir" --replay)"

# 28. An exit status beside one predicate over the output: both hold or the row fails.
# At 25c02ddfcf an expect held exactly one key, so a row could not claim a failure and the
# message it prints together. The honest row passes; the same status with another
# message fails under replay-command.
dir="$(fresh exit-and-message)"
plant_behaviour "$dir" "printf 'ran once\\n'; exit 1" '{"exit":1,"contains":"ran once"}'
out="$(fixture_run "$dir" --replay)"
honest_result "an exit status beside a message that both hold passes" 'replay-command|predicate-form' "$out"
dir="$(fresh exit-other-message)"
plant_behaviour "$dir" "printf 'other\\n'; exit 1" '{"exit":1,"contains":"ran once"}'
case_result "an exit status beside a message the output does not carry fails the row" replay-command "$(fixture_run "$dir" --replay)"

total=$((CASES_PASSED + CASES_FAILED))
if [ "$CASES_FAILED" -eq 0 ]; then
    printf 'all %d cases passed\n' "$total"
    exit 0
fi
printf '%d of %d cases failed\n' "$CASES_FAILED" "$total"
exit 1
