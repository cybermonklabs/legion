#!/usr/bin/env bash
# A changelog fragment of a daemon API contract change says which contract its change is, and an
# operator reads that number against the `legion version --contracts` of the build they run. Until
# dispatch://INFRA-1569 a change picked its number by hand and renumbered while it waited, and a
# fragment written before the last renumber kept the old number: the three
# pr2751-admission-counts-issues.yaml fragments named contract 47 for the change that
# packages/daemon/internal/api/version.go numbers 51, and declares no 47.
#
# So this check reads each fragment, every file under a `.changes` directory, with no regard to
# case. A fragment declares its own number with the words `daemon API contract <n>`, or with the
# verbs fragments say it with (`daemon API contract is <n>`, `... stays <n>`, `... moves to <n>`,
# each also after up to three words that say which number it is). The check finds the commit of
# main's first-parent line that added the fragment (`git log --first-parent --diff-filter=A`), and
# the numbers that commit gave its own entries: the `// <n>: ` headers its diff adds to version.go,
# less any it takes out. It refuses a declared number that is not one of them, and a declared
# number above L, the greatest number of
# packages/contracts/fixtures/daemon-api/released-contracts.json: an entry made after the numbered
# ones has no number until main takes it, so its fragment names the entry's id.
#
# A change whose commit gave no entry a number has no number of its own, and neither has a change
# no commit of main adds yet: such a fragment's `daemon API contract <n>` names another change's
# contract (pr3206-slot-until-work-ends.yaml says "the `waiting-for-slot` notice that daemon API
# contract 58 declared"), so it is listed with the other numbers below, and refused only above L.
#
# Every other contract number a fragment names (`contract <n>`, `contract is <n>` and the other
# verbs, or `contracts <n> and <n>`, the pattern check-daemon-api-contract.sh reads fragments with)
# is listed on stdout, one line each: the file and line, the number and the words around it. The
# reviewer sorts each: a number of the fragment's own change must be one of the numbers its commit
# added, and any other number is context. The check exits 0 when it refuses nothing, whatever it
# lists.
#
# It reads git objects only: commits and trees for the log, and version.go's blobs at the commits
# that added a declaring fragment, so a blobless fetch of main is enough (git fetches each blob
# where a diff reads it). A shallow clone grafts main's history, and the commit at the graft reads
# as adding every file it holds, so the check refuses to run in one.
#
# Run from anywhere: .github/scripts/check-fragment-contracts.sh [--main <ref>], after a blobless
# fetch of main (`git fetch --filter=blob:none origin refs/heads/main:refs/remotes/origin/main`).
# <ref> defaults to refs/remotes/origin/main. Its tests: check-fragment-contracts.test.sh.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 - "$@" <<'PY'
import json
import os
import re
import subprocess
import sys
from collections import Counter
from pathlib import Path

SCRIPT = ".github/scripts/check-fragment-contracts.sh"
VERSION_GO = "packages/daemon/internal/api/version.go"
RELEASED = "packages/contracts/fixtures/daemon-api/released-contracts.json"
# A header of version.go that a commit's diff adds or takes out: `// <n>: `, the blank after the
# colon keeping out prose that wraps at a time of day (`// 22:48Z`).
HEADER = re.compile(r"^([+-])// (\d+):[ \t]")
# A fragment declares its own number with the words `daemon API contract <n>`, or with the verbs
# NAMED reads beside them (`daemon API contract is <n>`, `daemon API contract moves to <n>`).
DECLARED = re.compile(
    r"\bdaemon\s+api\s+contract(?:(?:\s+[a-z]+){0,3}?\s+(?:is|stays|moves\s+to))?\s+(\d+)", re.I
)
# The pattern check-daemon-api-contract.sh reads fragments with: `contract <n>`, the same with the
# verbs its fragments use (`contract is <n>`, `contract stays <n>`, `contract moves to <n>`, after
# up to three words that say which number it is), or `contracts <n>, <n> and <n>` (a list, a
# range).
NAMED = re.compile(
    r"\bcontract(?:(?:\s+[a-z]+){0,3}?\s+(?:is|stays|moves\s+to))?\s+(\d+)"
    r"|\bcontracts\s+(\d+(?:(?:\s*,\s*|\s*[-\u2013]\s*|\s+(?:and|or|to|through)\s+)\d+)*)",
    re.I,
)
COMMIT = "\x01"


def one_line(value: object) -> str:
    return " ".join(str(value).split())


def fatal(path: str, message: str) -> None:
    print(f"::error file={path}::{message}", file=sys.stderr)
    raise SystemExit(1)


def git(args: list[str], what: str) -> str:
    try:
        done = subprocess.run(
            ["git", *args], capture_output=True, env={**os.environ, "GIT_TERMINAL_PROMPT": "0"}
        )
    except OSError as error:
        fatal(SCRIPT, f"could not read {what}: git could not be run ({one_line(error)})")
    if done.returncode != 0:
        said = done.stderr.decode("utf-8", "replace") or done.stdout.decode("utf-8", "replace")
        fatal(
            SCRIPT,
            f"could not read {what}: git {' '.join(args[:3])} exited {done.returncode} "
            f"({one_line(said)[:400] or 'no output'})",
        )
    return done.stdout.decode("utf-8", "surrogateescape")


arguments = sys.argv[1:]
main_ref = "refs/remotes/origin/main"
if arguments[:1] == ["--main"] and len(arguments) == 2:
    main_ref = arguments[1]
elif arguments:
    fatal(SCRIPT, f"usage: {SCRIPT} [--main <ref>], and it was given {one_line(' '.join(arguments))}")

fetch = "git fetch --filter=blob:none origin refs/heads/main:refs/remotes/origin/main"
if git(["rev-parse", "--is-shallow-repository"], "whether this clone is shallow").strip() == "true":
    fatal(
        SCRIPT,
        f"this clone is shallow, so the commit at its graft reads as adding every fragment it "
        f"holds: fetch main's whole history, without blobs ({fetch} --unshallow)",
    )
try:
    main = subprocess.run(
        ["git", "rev-parse", "--verify", "--quiet", "--end-of-options", f"{main_ref}^{{commit}}"],
        capture_output=True, text=True,
    ).stdout.strip()
except OSError as error:
    fatal(SCRIPT, f"could not read {main_ref}: git could not be run ({one_line(error)})")
if not main:
    fatal(
        SCRIPT,
        f"{main_ref} names no commit in this clone: fetch main without blobs ({fetch}), or name it "
        f"with --main <ref>",
    )

try:
    released = json.loads(Path(RELEASED).read_bytes().decode("utf-8"))
except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
    fatal(RELEASED, f"could not read L: {RELEASED}: {one_line(error)}")
if (
    not isinstance(released, list)
    or not released
    or any(isinstance(n, bool) or not isinstance(n, int) for n in released)
):
    fatal(RELEASED, f"could not read L: {RELEASED} is not a non-empty JSON array of integers")
last = max(released)


def line_of(text: str, at: int) -> int:
    return text.count("\n", 0, at) + 1


def context(text: str, start: int, end: int) -> str:
    """The words around text[start:end], on one line."""
    before, after = max(0, start - 60), min(len(text), end + 60)
    words = one_line(text[before:after])
    return f"{'...' if before else ''}{words}{'...' if after < len(text) else ''}"


# Each fragment the working tree holds, tracked or not yet added (ignored files aside).
paths = sorted(
    p
    for p in git(
        ["ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", ":(glob)**/.changes/**"],
        "the fragments this clone holds",
    ).split("\0")
    if p and Path(p).is_file()
)
# Each number a fragment names, in its order: (its line, the number, whether DECLARED's words
# declare it, the words around it).
named: dict[str, list[tuple[int, int, bool, str]]] = {}
for path in paths:
    try:
        text = Path(path).read_bytes().decode("utf-8")
    except (OSError, UnicodeDecodeError) as error:
        fatal(path, f"could not read {path}: {one_line(error)}")
    own = {match.start(1) for match in DECLARED.finditer(text)}
    found = []
    for match in NAMED.finditer(text):
        group = 1 if match.group(1) is not None else 2
        for number in re.finditer(r"\d+", match.group(group)):
            at = match.start(group) + number.start()
            words = context(text, match.start(), match.end())
            found.append((at, line_of(text, at), int(number.group()), at in own, words))
    if found:
        named[path] = [occurrence[1:] for occurrence in sorted(found)]
declarations = sorted(path for path, found in named.items() if any(declared for _, _, declared, _ in found))

# The commit of main's first-parent line that added each declaring fragment: the newest that adds
# its path, read in one log over every such path.
adder: dict[str, str] = {}
if declarations:
    current = ""
    for line in git(
        ["-c", "core.quotePath=false", "--literal-pathspecs", "log", "--first-parent", "--diff-filter=A",
         "--name-only", f"--format={COMMIT}%H", main, "--", *declarations],
        f"the commits of {main_ref} that added the fragments",
    ).split("\n"):
        if line.startswith(COMMIT):
            current = line[1:]
        elif line and current:
            adder.setdefault(line, current)

# The numbers each of those commits gave its own entries: the headers its diff of version.go,
# against its first parent, adds, less those it takes out (a header it rewrote is no number it
# gave).
given: dict[str, list[int]] = {}
subjects: dict[str, str] = {}
if adder:
    commits = sorted(set(adder.values()))
    plus: dict[str, Counter] = {c: Counter() for c in commits}
    minus: dict[str, Counter] = {c: Counter() for c in commits}
    current = ""
    for line in git(
        ["log", "--no-walk=unsorted", "--diff-merges=first-parent", "-p", "-U0", "--no-renames",
         "--no-ext-diff", "--no-textconv", "--no-color", f"--format={COMMIT}%H %s", *commits,
         "--", VERSION_GO],
        f"{VERSION_GO} at the commits that added the fragments",
    ).split("\n"):
        if line.startswith(COMMIT):
            current, _, subjects_line = line[1:].partition(" ")
            subjects[current] = subjects_line
            continue
        header = HEADER.match(line)
        if header and current in plus:
            (plus if header.group(1) == "+" else minus)[current][int(header.group(2))] += 1
    for commit in commits:
        given[commit] = sorted((plus[commit] - minus[commit]).elements())
        if commit not in subjects:
            subjects[commit] = git(["log", "-1", "--format=%s", commit], f"the subject of {commit}").strip()

refusals: list[tuple[str, str]] = []
listed: list[str] = []
held = 0
for path, found in sorted(named.items()):
    commit = adder.get(path)
    numbers = given.get(commit, []) if commit else []
    for line, number, declared, words in found:
        where = f"{path}:{line}: declares daemon API contract {number}"
        if declared and number > last:
            refusals.append((path, (
                f"{where}, above L = {last}, the last numbered entry ({RELEASED}). An entry made "
                f"after the numbered ones has no number until main takes it, so its fragment names "
                f"the entry's id, the 14 digits of its file's name."
            )))
        elif declared and numbers and number not in numbers:
            refusals.append((path, (
                f"{where}, and the commit of main that added this fragment, {commit[:10]} "
                f"({one_line(subjects[commit])[:120]}), gave its own entries "
                f"{', '.join(str(n) for n in numbers)} in {VERSION_GO}. Write the number "
                f"{VERSION_GO} gives this change."
            )))
        elif declared and numbers:
            held += 1
        else:
            listed.append(f"{path}:{line}: contract {number}: {words}")

for line in listed:
    print(line)
for path, message in refusals:
    print(f"::error file={path}::{one_line(message)}", file=sys.stderr)
print(
    f"{len(paths)} fragments read, L = {last}: {held} declarations of a fragment's own number are "
    f"each one its commit gave its entries, {len(refusals)} refused; {len(listed)} other contract "
    f"numbers listed above for the reviewer",
    file=sys.stderr,
)
if refusals:
    raise SystemExit(1)
PY
