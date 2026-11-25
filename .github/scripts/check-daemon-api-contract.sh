#!/usr/bin/env bash
# `packages/daemon/internal/api/version.go#const DaemonAPIVersion` is the one number a daemon
# and the Legion plugin in its pods must agree on: the boot gate refuses a pair whose numbers
# differ. So two changes that declare the same number cannot both be right, and the one that lands
# second has to take another number after its review has already run. Nothing caught that before
# the merge, so it was found after it: one change was renumbered three times while its round ran
# (49, then 50, then 52, landing at 53), each renumber costing a forward merge, a test round, a
# review round and a thrown-away approval, and two changes still declared 53 at once.
#
# So a change that touches that file must declare a number above the one its base carries. The
# base is the commit the queue built the group on, for a merge group, and the tip of the base
# branch, for a pull request. A change that touches the file and declares no higher number is
# refused, and the refusal names a free number — the lowest above the base's that no open pull
# request of this repository declares, naming the pull requests holding the numbers it passed
# over — and every file that holds the number, since they move together.
#
# Two entries of one merge group that both picked the same next number are not refused: the
# group's tree carries both changes at that number, which is above the base's, and that tree is
# the pair that ships.
#
# "Touches the file" is this checkout's copy of the contract's history and declaration differing
# from the copy the commit this checkout was built on carries. That part runs from the file's
# first history entry (the first line that starts `// <number>:`) to its end: the history and
# the `const` line. A collision shows there, as a second entry at one number or in the `const`
# line. The doc comment above the first entry holds no number, and an edit of it alone needs no
# contract bump: a bump relaunches every registered claim in every deployment onto the new
# plugin, and a comment is not worth that. What this gives up: an edit of that header alone is
# no longer refused, even one that adds a surface to what the contract names. A copy with no
# history entry is compared whole. The number the `const` line declares must also have its own
# `// <number>:` entry, so a bump cannot move the declaration and its fixtures while the history
# stays silent.
#
# The commit this checkout was built on is not always the base: a merge group is the base with
# the group's entries on top, so there the two are one commit, but a pull request is checked out
# as the merge ref GitHub built when the event fired, and a re-run reuses that ref. Main moves on
# under it, so a comparison with the tip of the base branch read a pull request that had left
# the file alone as one that had touched it, and refused it for declaring the number its own
# base carries. What the tree was built on is that merge ref's first parent, which a depth-1
# checkout carries in HEAD's parent line (built_on). The number is still compared with the
# base's, so a change that does touch the file and declares a number the base has reached since
# is refused as before. A base, a commit this checkout was built on, or a tree this check cannot
# read refuses the change and names the read that failed: a check that passes when it cannot
# read proves nothing.
#
# What it does not check: whether a bump is owed. A change to a surface this contract names that
# leaves version.go alone passes here — that file's own bump rule and the review say when a bump
# is owed, and this check only keeps two changes from claiming one number.
#
# It does check that the files that hold the number agree. Each file after version.go must hold
# the number version.go declares, as an integer: the fixture
# packages/contracts/fixtures/daemon-api/version.json at daemonApiVersion, the fixture
# packages/contracts/fixtures/daemon-api/state.json at both contract.daemon and contract.caller,
# and the manifests packages/pi-legion/package.json and packages/claude-envoy/package.json at
# legion.daemonApiVersion. A file this check cannot read there is refused as a read that failed.
# This rule runs before the pass for a number above the base's, because a bump is where a file
# is left behind: INFRA-3621's bump forgot state.json, and that pass would have let it through.
# It reads only this checkout, so its refusal reads no open pull request and names no free
# number: the files disagree, and the number is not what is wrong.
#
# Every GitHub read goes through `gh api`, and the base is read over the API rather than out of
# git: the checkout in CI is one commit deep and fetches no origin/main. The one git read is of
# HEAD's own object, whose parent lines a shallow checkout keeps whatever `git rev-parse HEAD^1`
# says of them, and it is made only where the declared number is not above the base's, so a
# change that bumps the number reads git not at all. The open pull requests are read only when
# the check is about to fail, so a passing run makes none of them: one
# paginated GraphQL query for every open pull request with the paths it changes, drafts counted,
# and then this file at refs/pull/<number>/head — a ref of this repository, so a fork's head too
# — for each that changes it. A file list GitHub cut short says nothing about the paths it left
# out, so that pull request's file is read as well rather than guessed at, and a read that fails
# refuses the change and names itself. Each `gh api` runs through gh-read.sh's gh_wait_reset,
# which waits out a spent API budget within the job's JOB_TIMEOUT_MINUTES and fails by name past
# it; its own lines reach the step's log as they happen, and gh's words reach the refusal.
#
# Run from anywhere: .github/scripts/check-daemon-api-contract.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

# gh-read.sh beside this script's own file, resolved before the cd: a test fixture links only
# this script in, and the link leads back to the directory CI runs from.
GH_READ="$(dirname "$(readlink -f "$0")")/gh-read.sh"
export GH_READ
cd "$(dirname "$0")/../.."

python3 <<'PY'
import json
import os
import re
import subprocess
import sys
import tempfile
from collections.abc import Callable
from pathlib import Path
from urllib.parse import quote

DECLARATION = "packages/daemon/internal/api/version.go"
WORKFLOW = ".github/workflows/ci.yaml"
# Where each file after version.go holds the number, which held_in reads: dotted key paths into
# its JSON, each an integer. state.json holds it twice, since the state route writes the number
# as both the daemon's and the caller's for a caller at this contract, which internal/daemon's
# TestTheStateFixturesAreTheDaemonsCompositionOfTheirSeeds writes the body of. version.go's reader
# is number_in.
FIELDS = {
    "packages/contracts/fixtures/daemon-api/version.json": ("daemonApiVersion",),
    "packages/contracts/fixtures/daemon-api/state.json": ("contract.daemon", "contract.caller"),
    "packages/pi-legion/package.json": ("legion.daemonApiVersion",),
    "packages/claude-envoy/package.json": ("legion.daemonApiVersion",),
}
# Every file that holds the number: version.go, then the files of FIELDS in their order. They move
# together, so a refusal names all of them, and each file after version.go must hold the number
# version.go declares.
SITES = (DECLARATION, *FIELDS)
DECLARED = re.compile(r"^const\s+DaemonAPIVersion\s*=\s*(\d+)\s*$", re.M)
# version.go's history entries: each line that starts `// <number>: `, the number its group. The
# blank after the colon keeps out an entry's own prose that wraps at a time of day (`// 22:48Z`).
HISTORY = re.compile(r"^// (\d+):[ \t]", re.M)
# One page of the open pull requests, each with its number, its draft mark, its head commit and
# the paths it changes. Drafts are counted: a draft holds the number it declares as firmly as any
# other open pull request, and one of the collisions this check exists for was a draft's.
PULL_REQUESTS = """
query($owner: String!, $name: String!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, first: 50, after: $cursor) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        isDraft
        headRefOid
        files(first: 100) {
          pageInfo { hasNextPage }
          nodes { path }
        }
      }
    }
  }
}
"""


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines, or it reaches the Actions log as a
    line of its own."""
    return " ".join(str(value).split())


def fatal(path: str, message: str) -> None:
    print(f"::error file={path}::{message}", file=sys.stderr)
    raise SystemExit(1)


def gh(args: list[str], what: str) -> str:
    """Every GitHub read goes through `gh api`, so one stub first on PATH serves them all, and
    through gh-read.sh's gh_wait_reset: its own lines (a wait, or a budget spent past the limit)
    go straight to this step's stderr, and the words gh printed on the call it ended on go to a
    file, which the refusal quotes."""
    with tempfile.NamedTemporaryFile(prefix="gh-words.") as words:
        try:
            done = subprocess.run(
                ["bash", "-c", 'source "$0"; gh_wait_reset --stderr "$1" gh api "${@:2}"',
                 os.environ["GH_READ"], words.name, *args],
                stdout=subprocess.PIPE, text=True, encoding="utf-8",
            )
        except OSError as error:
            fatal(DECLARATION, f"could not read {what}: gh could not be run ({one_line(error)})")
        said = Path(words.name).read_text(encoding="utf-8", errors="replace")
    if done.returncode != 0:
        fatal(
            DECLARATION,
            f"could not read {what}: gh api exited {done.returncode} "
            f"({one_line(said) or one_line(done.stdout) or 'no output'})",
        )
    return done.stdout


def read_at(repository: str, ref: str, what: str) -> str:
    """The declaration file as one ref of the base repository carries it."""
    return gh(
        [
            "-H",
            "Accept: application/vnd.github.raw",
            f"repos/{repository}/contents/{DECLARATION}?ref={quote(ref, safe='')}",
        ],
        what,
    )


def built_on(what: str) -> str:
    """The commit this checkout was built on: HEAD's first parent.

    A pull request is checked out as the merge ref GitHub built when the event fired, whose first
    parent is the commit of the base branch it was built on. That is the tree's own base: the tip
    of the branch moves on under a ref a re-run reuses, and a change compared with the tip reads
    as touching a file it never opened.

    The checkout is one commit deep, which grafts HEAD's parents away: `git rev-parse HEAD^1` and
    `git rev-list --parents HEAD` both answer as if the merge ref had no parent at all. The commit
    object itself keeps them, and `git cat-file commit HEAD` prints it as it stands, header lines
    first and the message after the blank line that ends them."""
    try:
        done = subprocess.run(
            ["git", "cat-file", "commit", "HEAD"], capture_output=True, text=True, encoding="utf-8"
        )
    except OSError as error:
        fatal(DECLARATION, f"could not read {what}: git could not be run ({one_line(error)})")
    if done.returncode != 0:
        fatal(
            DECLARATION,
            f"could not read {what}: git cat-file commit HEAD exited {done.returncode} "
            f"({one_line(done.stderr) or one_line(done.stdout) or 'no output'})",
        )
    for line in done.stdout.split("\n"):
        if not line:
            break
        if line.startswith("parent "):
            return line.removeprefix("parent ").strip()
    fatal(
        DECLARATION,
        f"could not read {what}: HEAD names no parent, so this checkout is not the merge ref "
        f"GitHub builds a pull request's run on",
    )


def number_in(text: str, what: str, advice: Callable[[], str] | None = None) -> tuple[int, int]:
    """The contract number text declares and the line it is declared on, so the one pattern is
    applied once wherever a number is read: the line is what a refusal names to say where to
    edit. advice is called for what a refusal adds after its own sentence, the free number to
    take where the caller already knows the base's; it is called only when the refusal is made,
    since reading it costs this check its reads of the open pull requests."""
    match = DECLARED.search(text)
    if match is None:
        fatal(
            DECLARATION,
            f"could not read {what}: it declares no `const DaemonAPIVersion = <number>` line"
            f"{advice() if advice else ''}",
        )
    return int(match.group(1)), text.count("\n", 0, match.start()) + 1


def held_in(site: str) -> dict[str, int]:
    """The number site, a file of FIELDS, holds at each of the fields FIELDS names for it. A file
    that is missing, is not JSON, lacks a field or holds anything but an integer there is refused
    by name, as a read that failed: a check that passes when it cannot read proves nothing."""
    what = f"{site} in this checkout"
    try:
        document = json.loads(Path(site).read_bytes().decode("utf-8"))
    except FileNotFoundError:
        fatal(site, f"could not read {what}: it is missing")
    except (OSError, UnicodeDecodeError) as error:
        fatal(site, f"could not read {what}: {one_line(error)}")
    except json.JSONDecodeError as error:
        fatal(site, f"could not read {what}: it is not JSON ({one_line(error)})")
    held: dict[str, int] = {}
    for field in FIELDS[site]:
        value: object = document
        for key in field.split("."):
            if not isinstance(value, dict) or key not in value:
                fatal(site, f"could not read {what}: it holds no {field}")
            value = value[key]  # type: ignore[index]
        # JSON's true is a Python bool, which is an int, and 8.0 equals 8: neither is a contract
        # number, so neither is held to the declared one.
        if isinstance(value, bool) or not isinstance(value, int):
            fatal(
                site,
                f"could not read {what}: its {field} is {one_line(json.dumps(value))[:200]}, "
                f"not an integer",
            )
        held[field] = value
    return held


def history_and_declaration(text: str) -> str:
    """The contract's history and declaration in a copy of version.go: the text from its first
    history entry to its end, trailing newlines normalised, since the API's body and the file on
    disk differ only there when nothing changed the file. The doc comment above the first entry
    holds no number, so an edit of it alone touches no contract. A copy with no history entry is
    returned whole."""
    entry = HISTORY.search(text)
    return text[entry.start() if entry else 0 :].rstrip("\n")


def payload_field(payload: dict, *path: str) -> str:
    value: object = payload
    for key in path:
        if not isinstance(value, dict) or key not in value:
            fatal(WORKFLOW, f"the event payload has no {'.'.join(path)}")
        value = value[key]  # type: ignore[index]
    if not isinstance(value, str) or not value:
        fatal(WORKFLOW, f"the event payload's {'.'.join(path)} is not a non-empty string")
    return value


def declared_by_open_pull_requests(repository: str) -> dict[int, list[int]]:
    """Each contract number an open pull request declares, and which pull requests declare it.

    Read only when the check is about to fail, so a passing run makes none of these reads."""
    owner, _, name = repository.partition("/")
    holders: dict[int, list[int]] = {}
    cursor: str | None = None
    while True:
        what = "the open pull requests"
        args = ["graphql", "-f", f"query={PULL_REQUESTS}", "-F", f"owner={owner}", "-F", f"name={name}"]
        if cursor is not None:
            args += ["-F", f"cursor={cursor}"]
        answer = gh(args, what)
        try:
            page = json.loads(answer)
        except json.JSONDecodeError as error:
            fatal(DECLARATION, f"could not read {what}: the answer is not JSON ({one_line(error)})")
        listing = page.get("data", {}) if isinstance(page, dict) else {}
        for key in ("repository", "pullRequests"):
            listing = listing.get(key, {}) if isinstance(listing, dict) else {}
        if not isinstance(listing, dict) or not isinstance(listing.get("nodes"), list):
            fatal(
                DECLARATION,
                f"could not read {what}: the answer carries no "
                f"data.repository.pullRequests.nodes ({one_line(answer)[:200]})",
            )
        for node in listing["nodes"]:
            number = node.get("number")
            files = node.get("files") or {}
            paths = [entry.get("path") for entry in files.get("nodes") or []]
            # A file list GitHub cut short says nothing about the paths it left out, so the file
            # itself is read for that pull request rather than guessed at. Measured on this
            # repository (2026-11-07, 138 open pull requests): 12 of the first 50 carry more than
            # 100 files, so refusing on a cut list would have made every refusal a cannot-read one
            # and no refusal would ever have named a free number.
            cut_short = bool((files.get("pageInfo") or {}).get("hasNextPage"))
            if DECLARATION not in paths and not cut_short:
                continue
            # refs/pull/<number>/head is a ref of this repository, so it serves a fork's head too.
            ref = f"refs/pull/{number}/head"
            head = node.get("headRefOid")
            at = f"{DECLARATION} at {ref} (#{number}, head {head})"
            holders.setdefault(number_in(read_at(repository, ref, at), at)[0], []).append(number)
        info = listing.get("pageInfo") or {}
        if not info.get("hasNextPage"):
            return holders
        cursor = info.get("endCursor")
        if not isinstance(cursor, str) or not cursor:
            fatal(DECLARATION, f"could not read {what}: a page says it has a next one but names no cursor")


def take(repository: str, base_number: int) -> str:
    """The lowest number above the base's that no open pull request declares, with the pull
    requests holding the numbers it passed over."""
    holders = declared_by_open_pull_requests(repository)
    free = base_number + 1
    while free in holders:
        free += 1
    passed = "; ".join(
        f"{number} is declared by " + ", ".join(f"#{pull}" for pull in sorted(holders[number]))
        for number in range(base_number + 1, free)
    )
    if passed:
        return (
            f"Take {free}, the lowest number above the base's that no open pull request declares "
            f"({passed})"
        )
    return f"Take {free}, the lowest number above the base's that no open pull request declares"


def renumber(repository: str, base_number: int) -> str:
    """How every refusal ends: the number to take (take), and the files to write it in, which
    hold the number together."""
    return (
        f"{take(repository, base_number)}, and write it in each of the {len(SITES)} files that "
        f"hold the number: {', '.join(SITES)}"
    )


event_name = os.environ.get("GITHUB_EVENT_NAME", "")
event_path = os.environ.get("GITHUB_EVENT_PATH", "")
repository = os.environ.get("GITHUB_REPOSITORY", "")
if not repository:
    fatal(WORKFLOW, "GITHUB_REPOSITORY is unset, so this check cannot name the repository to read")
if event_name not in ("pull_request", "merge_group"):
    fatal(
        WORKFLOW,
        f"this check runs on a pull request or a merge group, which have a base to compare with, "
        f"and GITHUB_EVENT_NAME is {one_line(event_name) or 'unset'}",
    )
try:
    payload = json.loads(Path(event_path).read_bytes().decode("utf-8"))
except OSError as error:
    fatal(WORKFLOW, f"could not read the event payload at GITHUB_EVENT_PATH: {one_line(error)}")
except (UnicodeDecodeError, json.JSONDecodeError) as error:
    fatal(WORKFLOW, f"the event payload at {event_path} is not JSON: {one_line(error)}")
if not isinstance(payload, dict):
    fatal(WORKFLOW, f"the event payload at {event_path} is not a JSON object")

if event_name == "merge_group":
    base_ref = payload_field(payload, "merge_group", "base_sha")
    base_is = f"the commit the queue built this group on ({base_ref})"
else:
    base_ref = payload_field(payload, "pull_request", "base", "ref")
    base_is = f"the tip of {base_ref}"

base_text = read_at(repository, base_ref, f"{DECLARATION} on the base, {base_is}")
base_number, _ = number_in(base_text, f"{DECLARATION} on the base, {base_is}")

# Every refusal from here on knows the base's number, so each one names a free number.
try:
    tree_text = Path(DECLARATION).read_bytes().decode("utf-8")
except FileNotFoundError:
    fatal(
        DECLARATION,
        f"{DECLARATION} is missing from this checkout, so what contract this change declares "
        f"cannot be read; the base, {base_is}, carries {base_number}. "
        f"{renumber(repository, base_number)}",
    )
except (OSError, UnicodeDecodeError) as error:
    fatal(
        DECLARATION,
        f"could not read {DECLARATION} in this checkout: {one_line(error)}; the base, {base_is}, "
        f"carries {base_number}. {renumber(repository, base_number)}",
    )
tree_number, line = number_in(
    tree_text,
    f"{DECLARATION} in this checkout",
    lambda: f"; the base, {base_is}, carries {base_number}. {renumber(repository, base_number)}",
)

if str(tree_number) not in HISTORY.findall(tree_text):
    fatal(
        DECLARATION,
        f"{DECLARATION}:{line}: declares daemon API contract {tree_number}, but its history has "
        f"no `// {tree_number}:` entry. A bump changes the daemon and plugin together, so record "
        f"the number in that history before the declaration. The base, {base_is}, carries "
        f"{base_number}. {renumber(repository, base_number)}",
    )

# The agreement rule of the header: it runs before the pass for a number above the base's.
for site in FIELDS:
    held = held_in(site)
    if any(number != tree_number for number in held.values()):
        holds = " and ".join(f"{number} at {field}" for field, number in held.items())
        fatal(
            site,
            f"{site} holds daemon API contract {holds}, and {DECLARATION} declares {tree_number}. "
            f"The {len(SITES)} files that hold the number must agree on it: {', '.join(SITES)}. "
            f"`go test ./internal/api -run '^TestDaemonAPIVersionGolden$' -update` and "
            f"`go test ./internal/daemon -run '^TestTheStateFixturesAreTheDaemonsCompositionOfTheirSeeds$' "
            f"-update`, run in packages/daemon, write the fixtures, and the manifests take the "
            f"number in legion.daemonApiVersion.",
        )

if tree_number > base_number:
    print(
        f"daemon API contract {tree_number} is above the {base_number} the base carries "
        f"({base_is}): {DECLARATION}"
    )
    raise SystemExit(0)

# What this tree was built on. For a merge group it is the base itself, the commit the queue
# built the group on, which is already read; for a pull request it is the merge ref's first
# parent, read here rather than above so a change that bumps the number costs neither read.
if event_name == "merge_group":
    built_is, built_text = base_is, base_text
else:
    parent = built_on("the commit GitHub built this pull request's merge ref on")
    built_is = f"the commit GitHub built this pull request's merge ref on ({parent})"
    built_text = read_at(repository, parent, f"{DECLARATION} on {built_is}")

# Only the history and the declaration are compared (history_and_declaration).
if history_and_declaration(tree_text) == history_and_declaration(built_text):
    print(
        f"this change leaves the history and the declaration in {DECLARATION} as {built_is} has "
        f"them (contract {tree_number}), so it declares no daemon API contract of its own"
    )
    raise SystemExit(0)

fatal(
    DECLARATION,
    f"{DECLARATION}:{line}: this change touches {DECLARATION} and declares daemon "
    f"API contract {tree_number}, which is not above the {base_number} the base carries "
    f"({base_is}). A daemon and the plugin in its pods must agree on this number, so two changes "
    f"cannot both declare one: the second to land is renumbered after its review has run. "
    f"{renumber(repository, base_number)}",
)
PY
