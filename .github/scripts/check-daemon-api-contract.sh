#!/usr/bin/env bash
# The daemon API contract number is the one number a daemon and the Legion plugin in its pods must
# agree on: the boot gate refuses a pair whose numbers differ, and each new number relaunches every
# registered claim in every deployment at its first boot, onto the plugin of that number. So one
# number must name one set of contract changes in every build of main, and no change may pick it:
# two changes that picked one number by hand collided, and the one that landed second was
# renumbered after its review had run (one was renumbered three times while its round ran, each
# time paying a forward merge, a test round, a review round and a thrown-away approval).
#
# Since dispatch://INFRA-1569 no branch picks a number. The numbered entries of
# packages/daemon/internal/api/version.go (each line that starts `// <n>: `) end at L, the
# greatest number of packages/contracts/fixtures/daemon-api/released-contracts.json. Each contract
# change after them is an entry: the file packages/daemon/internal/api/contract_<id>_<slug>.go,
# which scripts/daemon-contract-entry.sh writes, and its fixture
# packages/contracts/fixtures/daemon-api/contracts/<id>_<slug>.json, which
# `go test ./internal/api -update` writes. A build's number is L plus its count of entries, each
# entry ranked in main's merge order (scripts/daemon-contract-order.sh), so two changes add two
# files and edit no shared line. This check refuses each change that would undo that, naming the
# file and what to do. A change that:
#
# 1. adds a numbered entry to version.go, or changes, drops or reorders the number of one: the
#    `// <n>:` headers, in order, are the base's.
# 2. deletes or renames an entry file or an entry fixture its base holds: a number is a rank in
#    main's merge order, so an entry that main holds stays, and a revert adds a new entry that
#    undoes the change.
# 3. holds, at its head, an entry with no fixture, or a fixture with no entry: the count of
#    fixtures is the count of entries, which is how every reader outside the daemon counts them.
#    A fixture is what those readers count, a file of the fixtures' directory named
#    <14 digits>_<name>.json; anything else there is TestEveryEntryHasOneFixture's to refuse.
# 4. commits the number: `legion.daemonApiVersion` in a package.json (each pack step writes it),
#    packages/contracts/fixtures/daemon-api/version.json, a `contract` mark in state.json or a
#    `contract.daemon` mark in another state golden of that directory
#    (state-narrowed-repositories.json keeps its `caller`, which no entry moves).
# 5. adds or edits a changelog fragment (a file under a `.changes` directory) that names a contract
#    number above L: a new entry's number is unknown until main takes it, so its fragment names
#    the entry's id.
# 6. edits an entry fixture, or a frozen record (contract-shapes.json, request-shapes.json,
#    answers.json, enumerated-fields.json, released-contracts.json), that its base holds, and adds
#    no paragraph that starts `Correction:` to that entry's text: the comment of the entry file for
#    a fixture, and version.go's numbered entry n for a record of contract n. A change of what a
#    merged contract means is a new entry; an edit of a merged record corrects a record that was
#    wrong, and the reviewer reads that paragraph. A record's contract is its `contract`; an
#    answer's row and a typed setting of answers.json are their `since` (and `until`); a value of
#    enumerated-fields.json is its `since`; a number of released-contracts.json is itself. An edit
#    that changes no record (its bytes alone) is refused too, since it names no entry to correct.
#
# The base is the commit the change is compared with. For a merge group it is the commit the queue
# built the group on (the payload's merge_group.base_sha). For a pull request it is the commit
# GitHub built the merge ref this run checked out on: the tip of the base branch when the event
# fired, the merge ref's first parent. A re-run reuses that merge ref while main moves on under
# it, so the live tip of the branch holds entries and released fragments the merge ref never saw,
# which would read as this change deleting them. DAEMON_API_CONTRACT_BASE=<rev>, where set, is the
# base instead and no event is read: a test, or an author, checks a commit of a scratch
# repository with it. The head is HEAD, read as committed, not off the disk.
#
# Every read is of git objects. CI's checkout is one commit deep, so where the base commit is
# absent the check fetches it from origin, one commit deep and without blobs
# (`git fetch --depth=1 --filter=blob:none`), which carries its trees: the diff of the two trees
# needs no blob, and a base file's blob is fetched only where its rule reads it (version.go when
# the change edits it, a frozen file or an entry file when the change edits a fixture or a
# record). A read that fails refuses the change and names itself: a check that passes when it
# cannot read proves nothing. So L is read from released-contracts.json at the head, and a head
# without it, or whose file is not a non-empty JSON array of integers, is refused by name.
#
# What it does not check: whether a change owes an entry (the shape gate and the review say so),
# and whether a row of a frozen table names an entry (`TestNoFrozenRowNamesAnEntry` refuses that).
# .github/scripts/check-fragment-contracts.sh holds each fragment's own number to the numbers the
# commit that added it gave its entries.
#
# Run from anywhere: .github/scripts/check-daemon-api-contract.sh, with GITHUB_EVENT_NAME and
# GITHUB_EVENT_PATH of a pull request or a merge group, or with DAEMON_API_CONTRACT_BASE=<rev>.
# CI runs it in ci.yaml's lint job, and its tests (check-daemon-api-contract.test.sh) in ci.yaml's
# test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import json
import os
import re
import subprocess
import sys
from collections import Counter
from difflib import SequenceMatcher
from pathlib import Path

VERSION_GO = "packages/daemon/internal/api/version.go"
WORKFLOW = ".github/workflows/ci-job-lint.yaml"
SCRIPT = ".github/scripts/check-daemon-api-contract.sh"
DAEMON_API = "packages/contracts/fixtures/daemon-api/"
RELEASED = DAEMON_API + "released-contracts.json"
VERSION_JSON = DAEMON_API + "version.json"
STATE = DAEMON_API + "state.json"
# Every state golden of the fixtures' directory: state.json and each state-<name>.json.
STATE_GOLDEN = re.compile(r"^packages/contracts/fixtures/daemon-api/state(-[^/]+)?\.json$")
# The frozen records, each a file of DAEMON_API, which no -update writes again.
FROZEN = (
    "contract-shapes.json",
    "request-shapes.json",
    "answers.json",
    "enumerated-fields.json",
    "released-contracts.json",
)
SLUG = r"[a-z0-9]+(?:-[a-z0-9]+)*"
ENTRY_DIR = "packages/daemon/internal/api/"
ENTRY = re.compile(rf"^packages/daemon/internal/api/contract_(\d{{14}})_({SLUG})\.go$")
FIXTURE_DIR = DAEMON_API + "contracts/"
# An entry fixture, as every reader counts one: a file of FIXTURE_DIR itself named
# <14 digits>_<name>.json. Its entry is contract_<id>_<name>.go.
FIXTURE = re.compile(r"^packages/contracts/fixtures/daemon-api/contracts/(\d{14})_([^/]+)\.json$")
FRAGMENT = re.compile(r"(^|/)\.changes/")
# version.go's numbered entries: each line that starts `// <number>: `. The blank after the colon
# keeps out an entry's own prose that wraps at a time of day (`// 22:48Z`).
HEADER = re.compile(r"^// (\d+):[ \t]")
# A paragraph that starts `Correction:`: such a comment line, after a blank comment line.
CORRECTION = re.compile(r"^//\s+Correction:")
# A contract number a fragment names: `contract <n>`, or `contracts <n>, <n> and <n>` (a list, a
# range), whatever the case. The one number is also named with the verbs the repository's
# fragments use, `contract is <n>`, `contract stays <n>` and `contract moves to <n>`, and with up to
# three words before the verb that say which number it is (`contract fixture is <n>`, `contract
# this plugin declares is <n>`): a fragment that says its number so names it as surely.
# check-fragment-contracts.sh reads fragments with the same pattern.
NAMED = re.compile(
    r"\bcontract(?:(?:\s+[a-z]+){0,3}?\s+(?:is|stays|moves\s+to))?\s+(\d+)"
    r"|\bcontracts\s+(\d+(?:(?:\s*,\s*|\s*[-\u2013]\s*|\s+(?:and|or|to|through)\s+)\d+)*)",
    re.I,
)

refusals: list[tuple[str, str]] = []


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines, or it reaches the Actions log as a
    line of its own."""
    return " ".join(str(value).split())


def fatal(path: str, message: str) -> None:
    print(f"::error file={path}::{message}", file=sys.stderr)
    raise SystemExit(1)


def refuse(path: str, message: str) -> None:
    """A refusal of the change, kept until every rule has run, so one run names every fault."""
    refusals.append((path, message))


def git(args: list[str], what: str, where: str, ok: tuple[int, ...] = (0,)) -> bytes:
    """git's output, or a refusal naming what could not be read. GIT_TERMINAL_PROMPT=0, so a fetch
    that needs credentials fails by name rather than waiting on a prompt no one answers."""
    try:
        done = subprocess.run(
            ["git", *args], capture_output=True, env={**os.environ, "GIT_TERMINAL_PROMPT": "0"}
        )
    except OSError as error:
        fatal(where, f"could not read {what}: git could not be run ({one_line(error)})")
    if done.returncode not in ok:
        said = done.stderr.decode("utf-8", "replace") or done.stdout.decode("utf-8", "replace")
        fatal(
            where,
            f"could not read {what}: git {' '.join(args[:3])} exited {done.returncode} "
            f"({one_line(said)[:400] or 'no output'})",
        )
    return done.stdout


def text_at(rev: str, path: str, what: str) -> str:
    data = git(["cat-file", "blob", f"{rev}:{path}"], what, path)
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError as error:
        fatal(path, f"could not read {what}: it is not UTF-8 ({one_line(error)})")


def json_at(rev: str, path: str, what: str) -> object:
    try:
        return json.loads(text_at(rev, path, what))
    except json.JSONDecodeError as error:
        fatal(path, f"could not read {what}: it is not JSON ({one_line(error)})")


def payload_field(payload: dict, *path: str) -> str:
    value: object = payload
    for key in path:
        if not isinstance(value, dict) or key not in value:
            fatal(WORKFLOW, f"the event payload has no {'.'.join(path)}")
        value = value[key]  # type: ignore[index]
    if not isinstance(value, str) or not value:
        fatal(WORKFLOW, f"the event payload's {'.'.join(path)} is not a non-empty string")
    return value


def merge_ref_parent() -> str:
    """The commit GitHub built this pull request's merge ref on: HEAD's first parent.

    The checkout is one commit deep, which grafts HEAD's parents away: `git rev-parse HEAD^1`
    answers as if the merge ref had none. The commit object keeps them, and `git cat-file commit
    HEAD` prints it as it stands, header lines first. A merge ref is a merge commit, so a HEAD
    with fewer than two parents is no merge ref, and its first parent says nothing of a base."""
    what = "the commit GitHub built this pull request's merge ref on"
    parents = [
        line.removeprefix("parent ").strip()
        for line in git(["cat-file", "commit", "HEAD"], what, WORKFLOW)
        .decode("utf-8", "replace")
        .split("\n\n", 1)[0]
        .split("\n")
        if line.startswith("parent ")
    ]
    if len(parents) < 2:
        fatal(
            WORKFLOW,
            f"could not read {what}: HEAD names {len(parents)} parent(s), so this checkout is not "
            f"the merge ref GitHub builds a pull request's run on",
        )
    return parents[0]


def present(rev: str) -> bool:
    try:
        return subprocess.run(
            ["git", "cat-file", "-e", f"{rev}^{{commit}}"], capture_output=True
        ).returncode == 0
    except OSError as error:
        fatal(WORKFLOW, f"could not read the base: git could not be run ({one_line(error)})")


def fetched(sha: str, base_is: str) -> str:
    """The base commit, fetched from origin where this checkout lacks it: one commit deep in a
    shallow checkout, and in any case without blobs, so it carries the base's trees."""
    if not present(sha):
        shallow = git(["rev-parse", "--is-shallow-repository"], "whether this checkout is shallow", WORKFLOW)
        depth = ["--depth=1"] if shallow.strip() == b"true" else []
        git(
            ["fetch", "--no-tags", "--filter=blob:none", *depth, "origin", sha],
            f"the base, {base_is}",
            WORKFLOW,
        )
        if not present(sha):
            fatal(WORKFLOW, f"could not read the base, {base_is}: origin's fetch did not bring the commit")
    return sha


override = os.environ.get("DAEMON_API_CONTRACT_BASE", "")
if override:
    base_is = f"DAEMON_API_CONTRACT_BASE ({one_line(override)})"
    base = git(
        ["rev-parse", "--verify", "--end-of-options", f"{override}^{{commit}}"],
        f"the base, {base_is}",
        SCRIPT,
    ).decode().strip()
else:
    event_name = os.environ.get("GITHUB_EVENT_NAME", "")
    event_path = os.environ.get("GITHUB_EVENT_PATH", "")
    if event_name not in ("pull_request", "merge_group"):
        fatal(
            WORKFLOW,
            f"this check runs on a pull request or a merge group, which have a base to compare with, "
            f"or with DAEMON_API_CONTRACT_BASE=<rev>, and GITHUB_EVENT_NAME is "
            f"{one_line(event_name) or 'unset'}",
        )
    if event_name == "merge_group":
        try:
            payload = json.loads(Path(event_path).read_bytes().decode("utf-8"))
        except OSError as error:
            fatal(WORKFLOW, f"could not read the event payload at GITHUB_EVENT_PATH: {one_line(error)}")
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            fatal(WORKFLOW, f"the event payload at {event_path} is not JSON: {one_line(error)}")
        if not isinstance(payload, dict):
            fatal(WORKFLOW, f"the event payload at {event_path} is not a JSON object")
        sha = payload_field(payload, "merge_group", "base_sha")
        base_is = f"the commit the queue built this group on ({sha})"
    else:
        sha = merge_ref_parent()
        base_is = f"the commit GitHub built this pull request's merge ref on ({sha})"
    base = fetched(sha, base_is)
head = git(["rev-parse", "--verify", "HEAD^{commit}"], "HEAD", SCRIPT).decode().strip()

# What the change does, path by path: the two trees' diff, with no rename read (a rename is a
# deletion and an addition, which needs no blob), and every path the head holds.
raw = git(
    ["diff-tree", "-r", "-z", "--no-renames", "--name-status", base, head],
    f"the diff of HEAD with the base, {base_is}",
    SCRIPT,
)
fields = raw.decode("utf-8", "surrogateescape").split("\0")
changed: dict[str, str] = {fields[i + 1]: fields[i][:1] for i in range(0, len(fields) - 1, 2)}
held = set(
    git(["ls-tree", "-r", "-z", "--name-only", head], "the paths HEAD holds", SCRIPT)
    .decode("utf-8", "surrogateescape")
    .split("\0")
) - {""}

# L: the greatest number of released-contracts.json at the head, the last numbered entry.
released = json_at(head, RELEASED, f"{RELEASED} at HEAD") if RELEASED in held else None
if (
    not isinstance(released, list)
    or not released
    or any(isinstance(n, bool) or not isinstance(n, int) for n in released)
):
    fatal(
        RELEASED,
        f"could not read L at HEAD: {RELEASED} "
        f"{'is missing' if released is None else 'is not a non-empty JSON array of integers'}, and "
        f"L, the last numbered entry, is its greatest number",
    )
last = max(released)


def numbered_entries(text: str) -> tuple[list[tuple[int, int]], dict[int, list[str]]]:
    """version.go's numbered entries: their headers in order, each as (number, line), and each
    number's text, the comment lines from its header to the next header or the comment's end
    (a number with two headers, as 10, 22 and 23 have, holds both texts)."""
    headers: list[tuple[int, int]] = []
    texts: dict[int, list[str]] = {}
    current: int | None = None
    for number, line in enumerate(text.split("\n"), 1):
        header = HEADER.match(line)
        if header:
            current = int(header.group(1))
            headers.append((current, number))
            texts.setdefault(current, []).append(line)
        elif current is not None and line.startswith("//"):
            texts[current].append(line)
        else:
            current = None
    return headers, texts


def corrections(lines: list[str]) -> int:
    """The paragraphs of an entry's text that start `Correction:`."""
    return sum(1 for i in range(1, len(lines)) if CORRECTION.match(lines[i]) and lines[i - 1].strip() == "//")


def entry_text(text: str, entry_id: str) -> list[str]:
    """An entry file's text: its comment from the line `// <id> <slug>: ` to the comment's end."""
    lines = text.split("\n")
    for start, line in enumerate(lines):
        if line.startswith(f"// {entry_id} "):
            end = start + 1
            while end < len(lines) and lines[end].startswith("//"):
                end += 1
            return lines[start:end]
    return []


# version.go's numbered entries at the head and on the base, each read once and only where a rule
# needs them: the base's blob is fetched only where the change edits the file, since an unchanged
# file holds the head's entries.
Entries = tuple[list[tuple[int, int]], dict[int, list[str]]]
numbered: dict[str, Entries] = {}


def entries_at(rev: str) -> Entries:
    if rev not in numbered:
        status = changed.get(VERSION_GO)
        if rev == base and status is None:
            numbered[rev] = entries_at(head)
        elif (rev == head and VERSION_GO not in held) or (rev == base and status == "A"):
            numbered[rev] = ([], {})
        else:
            at = "at HEAD" if rev == head else f"on the base, {base_is}"
            numbered[rev] = numbered_entries(text_at(rev, VERSION_GO, f"{VERSION_GO} {at}"))
    return numbered[rev]


# 1. The numbered entries' headers, in order, are the base's.
if VERSION_GO in changed:
    head_headers, base_headers = entries_at(head)[0], entries_at(base)[0]
    added = Counter(n for n, _ in head_headers) - Counter(n for n, _ in base_headers)
    dropped = Counter(n for n, _ in base_headers) - Counter(n for n, _ in head_headers)
    steps = SequenceMatcher(
        a=[n for n, _ in base_headers], b=[n for n, _ in head_headers], autojunk=False
    ).get_opcodes()
    for step, base_from, base_to, head_from, head_to in steps:
        if step == "equal":
            continue
        for number, line in head_headers[head_from:head_to]:
            if added[number] > 0:
                added[number] -= 1
                refuse(
                    VERSION_GO,
                    f"{VERSION_GO}:{line}: `// {number}:` is a numbered entry that the base, "
                    f"{base_is}, does not hold. The numbered entries end at L = {last}, and a "
                    f"contract change after them is an entry: `scripts/daemon-contract-entry.sh "
                    f"<slug>` writes its file, which takes this text and its rows, and "
                    f"`go test ./internal/api -update`, run in packages/daemon, writes its "
                    f"fixture. Its number is L plus its rank in main's merge order, so no file "
                    f"holds it.",
                )
            else:
                refuse(
                    VERSION_GO,
                    f"{VERSION_GO}:{line}: the numbered entry `// {number}:` is not where the "
                    f"base, {base_is}, has it: a numbered entry keeps its number and its place. "
                    f"Put the numbered entries back in the base's order.",
                )
        for number, line in base_headers[base_from:base_to]:
            if dropped[number] > 0:
                dropped[number] -= 1
                refuse(
                    VERSION_GO,
                    f"{VERSION_GO}: the numbered entry `// {number}:` (line {line} on the base, "
                    f"{base_is}) is gone from this file: a numbered entry keeps its number and its "
                    f"header, since every build of main already carries it. Put `// {number}:` "
                    f"back as the base has it.",
                )

# 2. No entry file or entry fixture the base holds is deleted or renamed.
for path, status in sorted(changed.items()):
    if status != "D":
        continue
    if ENTRY.match(path):
        refuse(
            path,
            f"{path}: this change deletes or renames an entry that the base, {base_is}, holds. An "
            f"entry's number is its rank in main's merge order, so an entry main holds stays, file "
            f"and fixture: put it back under this name. To undo its change, add a new entry that "
            f"undoes it (`scripts/daemon-contract-entry.sh <slug>`).",
        )
    elif FIXTURE.match(path):
        refuse(
            path,
            f"{path}: this change deletes or renames an entry fixture that the base, {base_is}, "
            f"holds. An entry and its fixture stay once main holds them: put it back under this "
            f"name. To undo the entry's change, add a new entry that undoes it "
            f"(`scripts/daemon-contract-entry.sh <slug>`).",
        )

# 3. At the head, each entry has its fixture, and each fixture its entry. A fixture is what every
# reader counts: a file of FIXTURE_DIR named <14 digits>_<name>.json. Anything else in that
# directory is TestEveryEntryHasOneFixture's to refuse, and counts nowhere.
entries = {(m.group(1), m.group(2)): path for path in held if (m := ENTRY.match(path))}
fixtures = {(m.group(1), m.group(2)): path for path in held if (m := FIXTURE.match(path))}
for key, path in sorted(entries.items()):
    if key not in fixtures:
        refuse(
            path,
            f"{path}: this entry has no fixture, {FIXTURE_DIR}{key[0]}_{key[1]}.json. "
            f"`go test ./internal/api -update`, run in packages/daemon, writes it.",
        )
for key, path in sorted(fixtures.items()):
    if key not in entries:
        refuse(
            path,
            f"{path}: this fixture names no entry: HEAD holds no {ENTRY_DIR}contract_{key[0]}_"
            f"{key[1]}.go. An entry and its fixture come together, and a build counts its "
            f"fixtures: add the entry (`scripts/daemon-contract-entry.sh <slug>` writes one, with "
            f"a new id), or take out a fixture this change added for no entry.",
        )

# 4. No committed file holds the number.
listing = git(
    ["grep", "-l", "-z", "-F", "daemonApiVersion", head, "--", ":(glob)**/package.json"],
    "the package.json files HEAD holds",
    SCRIPT,
    ok=(0, 1),
)
for found in listing.decode("utf-8", "surrogateescape").split("\0"):
    if not found:
        continue
    path = found.removeprefix(f"{head}:")
    manifest = json_at(head, path, f"{path} at HEAD")
    legion = manifest.get("legion") if isinstance(manifest, dict) else None
    if isinstance(legion, dict) and "daemonApiVersion" in legion:
        refuse(
            path,
            f"{path} commits legion.daemonApiVersion "
            f"({one_line(json.dumps(legion['daemonApiVersion']))[:40]}). "
            f"No committed manifest holds the contract number: it is the greatest number of "
            f"{RELEASED} plus the count of entry fixtures, which `daemonContract()` reads in a "
            f"checkout, and each pack step writes the field into the manifest it packs. Take the "
            f"field out.",
        )
if VERSION_JSON in held:
    refuse(
        VERSION_JSON,
        f"{VERSION_JSON}: this file holds the contract number, and it goes: the number is the "
        f"greatest number of {RELEASED} plus the count of entry fixtures, which "
        f"`scripts/daemon-contract.sh` prints. Delete it.",
    )
for path in sorted(p for p in held if STATE_GOLDEN.match(p)):
    golden = json_at(head, path, f"{path} at HEAD")
    mark = golden.get("contract") if isinstance(golden, dict) else None
    if path == STATE and isinstance(golden, dict) and "contract" in golden:
        what = "a `contract` mark"
    elif isinstance(mark, dict) and "daemon" in mark:
        what = "a `contract.daemon` mark"
    else:
        continue
    refuse(
        path,
        f"{path} holds {what}, which is the daemon's own number: TestStateGolden takes out each "
        f"mark that equals DaemonAPIVersion() before it compares or writes, and holds the marks "
        f"to DaemonAPIVersion() apart. `go test ./internal/api -run '^TestStateGolden$' -update`, "
        f"run in packages/daemon, writes the golden without it.",
    )


def named(text: str) -> list[tuple[int, int, int]]:
    """Each contract number text names, as (number, its offset, its line)."""
    found = []
    for match in NAMED.finditer(text):
        group = 1 if match.group(1) is not None else 2
        for number in re.finditer(r"\d+", match.group(group)):
            at = match.start(group) + number.start()
            found.append((int(number.group()), at, text.count("\n", 0, at) + 1))
    return found


# 5. No fragment this change adds or edits names a number above L.
for path, status in sorted(changed.items()):
    if status not in ("A", "M", "T") or not FRAGMENT.search(path) or path not in held:
        continue
    for number, _, line in named(text_at(head, path, f"{path} at HEAD")):
        if number > last:
            refuse(
                path,
                f"{path}:{line}: names daemon API contract {number}, above L = {last}, the last "
                f"numbered entry ({RELEASED}). A new entry's number is unknown until main takes "
                f"it: name the entry by its id, the 14 digits of its file's name.",
            )

# 6. An entry fixture or a frozen record the base holds changes only as a correction.
for path, status in sorted(changed.items()):
    fixture = FIXTURE.match(path)
    if fixture is None or status not in ("M", "T"):
        continue
    entry_id, slug = fixture.group(1), fixture.group(2)
    entry = f"{ENTRY_DIR}contract_{entry_id}_{slug}.go"
    if entry not in held:
        continue  # rule 3 refuses a fixture with no entry, by name
    now = corrections(entry_text(text_at(head, entry, f"{entry} at HEAD"), entry_id))
    on_base = now  # an entry file this change leaves alone gains no paragraph
    if changed.get(entry) == "A":
        on_base = 0
    elif entry in changed:
        on_base = corrections(entry_text(text_at(base, entry, f"{entry} on the base, {base_is}"), entry_id))
    if now <= on_base:
        refuse(
            path,
            f"{path}: this change edits the fixture of entry {entry_id}, which the base, {base_is}, "
            f"holds, and adds no paragraph that starts `Correction:` to that entry's text in "
            f"{entry}. A change of what a merged contract means is a new entry "
            f"(`scripts/daemon-contract-entry.sh <slug>`); an edit of a merged fixture corrects a "
            f"record that was wrong, and says so in that paragraph, for the reviewer.",
        )


class Unreadable(Exception):
    pass


def number(value: object, where: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise Unreadable(f"{where} is {one_line(json.dumps(value))[:80]}, not an integer")
    return value


def records(name: str, document: object) -> dict[str, tuple[list[str], set[int]]]:
    """A frozen file's records, each keyed by what names it, as (its JSON, every copy, sorted; the
    contracts it is a record of)."""
    found: dict[str, tuple[list[str], set[int]]] = {}

    def add(key: list, value: object, contracts: set[int]) -> None:
        copies, numbers = found.setdefault(json.dumps(key), ([], set()))
        copies.append(json.dumps(value, sort_keys=True))
        numbers.update(contracts)

    if not isinstance(document, list):
        raise Unreadable("it is not a JSON array")
    for index, item in enumerate(document):
        where = f"its element {index}"
        if name == "released-contracts.json":
            released_number = number(item, where)
            add(["number", released_number], item, {released_number})
            continue
        if not isinstance(item, dict):
            raise Unreadable(f"{where} is not an object")
        if name in ("contract-shapes.json", "request-shapes.json"):
            contract = number(item.get("contract"), f"{where}'s contract")
            names = item.get("answer") if name == "contract-shapes.json" else item.get("route")
            add([contract, names], item, {contract})
        elif name == "answers.json":
            row = {k: v for k, v in item.items() if k != "typedSettings"}
            add(["answer", item.get("id")], row, {number(item.get("since"), f"{where}'s since")})
            settings = item.get("typedSettings", [])
            if not isinstance(settings, list):
                raise Unreadable(f"{where}'s typedSettings is not an array")
            for setting in settings:
                if not isinstance(setting, dict):
                    raise Unreadable(f"a typed setting of {where} is not an object")
                contracts = {number(setting.get("since"), f"a typed setting's since in {where}")}
                if "until" in setting:
                    contracts.add(number(setting["until"], f"a typed setting's until in {where}"))
                add(["setting", item.get("id"), setting.get("key")], setting, contracts)
        else:  # enumerated-fields.json
            values = item.get("values")
            if not isinstance(values, list):
                raise Unreadable(f"{where}'s values is not an array")
            sinces: set[int] = set()
            for value in values:
                if not isinstance(value, dict):
                    raise Unreadable(f"a value of {where} is not an object")
                since = number(value.get("since"), f"a value's since in {where}")
                sinces.add(since)
                add(["value", item.get("answer"), item.get("keyPath"), value.get("value")], value, {since})
            field = {k: v for k, v in item.items() if k != "values"}
            add(["field", item.get("answer"), item.get("keyPath")], field, sinces)
    for copies, _ in found.values():
        copies.sort()
    return found


for name in FROZEN:
    path = DAEMON_API + name
    status = changed.get(path)
    if status not in ("M", "T", "D"):
        continue
    if status == "D":
        refuse(
            path,
            f"{path}: this change deletes a frozen record file the base, {base_is}, holds: it holds "
            f"the records of the numbered entries, which every build of main already serves. Put it "
            f"back as the base has it.",
        )
        continue
    try:
        before = records(name, json_at(base, path, f"{path} on the base, {base_is}"))
    except Unreadable as error:
        fatal(path, f"could not read {path} on the base, {base_is}: {error}")
    try:
        after = records(name, json_at(head, path, f"{path} at HEAD"))
    except Unreadable as error:
        refuse(
            path,
            f"{path} at HEAD is not the frozen records' form: {error}. Put it back as the base, "
            f"{base_is}, has it.",
        )
        continue
    contracts: set[int] = set()
    for key in before.keys() | after.keys():
        was, was_of = before.get(key, ([], set()))
        now_, now_of = after.get(key, ([], set()))
        if was != now_:
            contracts |= was_of | now_of
    if not contracts:
        refuse(
            path,
            f"{path}: this change edits a frozen record file and changes no record in it, so it "
            f"names no entry to correct: a frozen file keeps the base's bytes. Put it back as the "
            f"base, {base_is}, has it.",
        )
        continue
    head_texts, base_texts = entries_at(head)[1], entries_at(base)[1]
    for contract in sorted(contracts):
        if contract not in head_texts:
            refuse(
                path,
                f"{path}: this change edits the records of contract {contract}, which no numbered "
                f"entry of {VERSION_GO} holds: a frozen file holds the records of the numbered "
                f"entries, and a new entry's records are its fixture "
                f"({FIXTURE_DIR}<id>_<slug>.json), which `go test ./internal/api -update` writes.",
            )
        elif corrections(head_texts[contract]) <= corrections(base_texts.get(contract, [])):
            refuse(
                path,
                f"{path}: this change edits the records of contract {contract}, which the base, "
                f"{base_is}, holds, and adds no paragraph that starts `Correction:` to the text of "
                f"the numbered entry `// {contract}:` in {VERSION_GO}. A change of what a merged "
                f"contract means is a new entry (`scripts/daemon-contract-entry.sh <slug>`); an edit "
                f"of a frozen record corrects a record that was wrong, and says so in that "
                f"paragraph, for the reviewer.",
            )

if refusals:
    for path, message in refusals:
        print(f"::error file={path}::{one_line(message)}", file=sys.stderr)
    print(
        f"the daemon API contract check refuses this change ({len(refusals)} refusal(s)), "
        f"compared with the base, {base_is}",
        file=sys.stderr,
    )
    raise SystemExit(1)
print(
    f"the daemon API contract check passes, compared with the base, {base_is}: L = {last}, "
    f"{len(entries)} entries with their fixtures, no numbered entry added or renumbered, and no "
    f"committed number"
)
PY
