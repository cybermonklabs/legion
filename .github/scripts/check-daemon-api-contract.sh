#!/usr/bin/env bash
# `const DaemonAPIVersion` in packages/daemon/internal/api/version.go is the one number a daemon
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
# "Touches the file" is this checkout's copy differing from the base's. A pull request is checked
# out as its head merged into the base tip, and a merge group as the base with the group's entries
# on top, so a change that leaves the file alone holds the base's bytes exactly and one that
# changed it does not. A base, or a tree, this check cannot read refuses the change and names the
# read that failed: a check that passes when it cannot read proves nothing.
#
# What it does not check: whether a bump is owed. A change to a surface this contract names that
# leaves version.go alone passes here — that file's own bump rule and the review say when a bump
# is owed, and this check only keeps two changes from claiming one number. It does not read the
# number out of the other three files either: packages/daemon's golden test holds the fixture to
# the constant, and the plugins' tests hold their manifests to the fixture.
#
# Every GitHub read goes through `gh api`, and the base is read over the API rather than out of
# git: the checkout in CI is one commit deep and fetches no origin/main. The open pull requests
# are read only when the check is about to fail, so a passing run makes none of them: one
# paginated GraphQL query for every open pull request with the paths it changes, drafts counted,
# and then this file at refs/pull/<number>/head — a ref of this repository, so a fork's head too
# — for each that changes it. A file list GitHub cut short says nothing about the paths it left
# out, so that pull request's file is read as well rather than guessed at, and a read that fails
# refuses the change and names itself.
#
# Run from anywhere: .github/scripts/check-daemon-api-contract.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import json
import os
import re
import subprocess
import sys
from pathlib import Path
from urllib.parse import quote

DECLARATION = "packages/daemon/internal/api/version.go"
WORKFLOW = ".github/workflows/ci.yaml"
# Every file that holds the number. They move together, so a refusal names all of them.
SITES = (
    DECLARATION,
    "packages/contracts/fixtures/daemon-api/version.json",
    "packages/pi-legion/package.json",
    "packages/claude-envoy/package.json",
)
DECLARED = re.compile(r"^const\s+DaemonAPIVersion\s*=\s*(\d+)\s*$", re.M)
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
    """Every GitHub read goes through `gh api`, so one stub first on PATH serves them all."""
    try:
        done = subprocess.run(
            ["gh", "api", *args], capture_output=True, text=True, encoding="utf-8"
        )
    except OSError as error:
        fatal(DECLARATION, f"could not read {what}: gh could not be run ({one_line(error)})")
    if done.returncode != 0:
        fatal(
            DECLARATION,
            f"could not read {what}: gh api exited {done.returncode} "
            f"({one_line(done.stderr) or one_line(done.stdout) or 'no output'})",
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


def number_in(text: str, what: str) -> int:
    match = DECLARED.search(text)
    if match is None:
        fatal(
            DECLARATION,
            f"could not read {what}: it declares no `const DaemonAPIVersion = <number>` line",
        )
    return int(match.group(1))


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
            holders.setdefault(number_in(read_at(repository, ref, at), at), []).append(number)
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
base_number = number_in(base_text, f"{DECLARATION} on the base, {base_is}")

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
match = DECLARED.search(tree_text)
if match is None:
    fatal(
        DECLARATION,
        f"{DECLARATION} declares no `const DaemonAPIVersion = <number>` line, so what contract "
        f"this change declares cannot be read; the base, {base_is}, carries {base_number}. "
        f"{renumber(repository, base_number)}",
    )
tree_number = int(match.group(1))

if tree_number > base_number:
    print(
        f"daemon API contract {tree_number} is above the {base_number} the base carries "
        f"({base_is}): {DECLARATION}"
    )
    raise SystemExit(0)

# Trailing newlines are normalised: the API's body and the file on disk differ only there when
# nothing changed the file.
if tree_text.rstrip("\n") == base_text.rstrip("\n"):
    print(
        f"this change leaves {DECLARATION} as the base has it ({base_is}, contract "
        f"{base_number}), so it declares no daemon API contract of its own"
    )
    raise SystemExit(0)

# The line the declaration is on, so the refusal names where to edit: the newlines before the
# match the search above already holds.
line = tree_text.count("\n", 0, match.start()) + 1
fatal(
    DECLARATION,
    f"{DECLARATION}:{line}: this change touches {DECLARATION} and declares daemon "
    f"API contract {tree_number}, which is not above the {base_number} the base carries "
    f"({base_is}). A daemon and the plugin in its pods must agree on this number, so two changes "
    f"cannot both declare one: the second to land is renumbered after its review has run. "
    f"{renumber(repository, base_number)}",
)
PY
