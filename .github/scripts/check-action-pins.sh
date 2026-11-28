#!/usr/bin/env bash
# Every remote action this repository calls is pinned to the commit a tag resolves to, not to the
# tag itself (a `v5` can move to a commit nobody reviewed; a 40-hex SHA cannot). This check is the
# enforcement half of that convention: nothing else in CI refuses a `uses:` that names a floating
# tag.
#
# A `uses:` is in scope when it names a remote action (`owner/repo[/path]@ref`); a local path
# (`./...`) or a Docker reference (`docker://...`) is not pinned by a commit and is out of scope.
# A scoped `uses:` passes only when its ref, the text after the last `@`, is exactly 40 lowercase
# hex characters — a tag, a branch, a short SHA, or an upper-case SHA all fail, so a pin cannot
# half-drift back toward a moving ref. .github/dependabot.yml is what keeps a passing pin current;
# this script only refuses a pin that has already drifted or was never made.
#
# It holds one more rule, about one action: every step that uses actions/setup-go carries
# `go-download-base-url: https://go.dev/dl` and `token: ''` under `with:`. Without them, setup-go
# reads its list of Go versions through the GitHub REST API (a tree and a blob of
# actions/go-versions, two calls) with the job's GITHUB_TOKEN. Every workflow run of this
# repository draws on that token's one budget of 15,000 calls an hour. On 2026-11-21 CI spent all
# of it between 16:24:15Z and 17:19:51Z, 1,712 setup-go runs made 3,424 of those calls, and the
# steps that read the API after that, CodeQL's upload and pr-title's read of the pull request
# among them, failed until the budget reset at 17:24:15Z (dispatch://INFRA-3904). With the base
# URL, setup-go lists the versions and downloads Go from go.dev and makes no GitHub API call. The
# empty token is needed too: setup-go sends its token with every download, so with the base URL
# alone the job's GITHUB_TOKEN would go to go.dev.
#
# Scope: every workflow (.github/workflows/*.yml, *.yaml) and every composite action
# (**/action.yml, **/action.yaml), read as YAML, every step's `uses:` and every job's `uses:`
# (a reusable-workflow call), and the `with:` of every step that uses actions/setup-go. Files are
# discovered by walking the tree, not by asking git, so an unsnapshotted change cannot read green
# locally. The walk skips only what holds no workflow or action of this repository: the version
# control metadata (.git, .jj) and node_modules, which .gitignore keeps out of every checkout, so
# GitHub can run no action from it. A `vendor` or `.venv` directory is read like any other, since a
# composite action committed there is one a workflow can call.
#
# Run from anywhere: .github/scripts/check-action-pins.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import re
import sys
from pathlib import Path

import yaml


class LineLoader(yaml.SafeLoader):
    """SafeLoader that records each mapping's line, so a problem names where to edit."""


class Mapping(dict):
    """A YAML mapping that remembers where each key was written."""

    lines: dict[str, int]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.lines = {
        key.value: key.start_mark.line + 1 for key, _ in node.value if hasattr(key, "value")
    }
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)

SKIP_DIRS = {".git", ".jj", "node_modules"}
SHA = re.compile(r"^[0-9a-f]{40}$")


def walk() -> list[Path]:
    found = []
    stack = [Path(".")]
    while stack:
        for entry in sorted(stack.pop().iterdir()):
            if entry.is_symlink() and entry.is_dir():
                continue
            if entry.is_dir():
                if entry.name not in SKIP_DIRS:
                    stack.append(entry)
            else:
                found.append(entry)
    return found


files = walk()
targets = [
    path
    for path in files
    if (path.parent == Path(".github/workflows") and path.suffix in {".yml", ".yaml"})
    or path.name in {"action.yml", "action.yaml"}
]

problems: list[str] = []
# The setup-go rule's refusals, apart from the pin refusals so the summary counts each kind, and
# the number of setup-go steps read, so a pass says how many it held to the rule.
setup_go_problems: list[str] = []
setup_go_steps = 0
# The action the second rule is about, and the two inputs it must carry, as the header says.
SETUP_GO = "actions/setup-go"
GO_DEV = "https://go.dev/dl"


def is_remote(uses: str) -> bool:
    """A local path or a Docker reference names no commit a tag could move; everything else
    (owner/repo[/path]@ref) is a pin this check covers."""
    return not (uses.startswith("./") or uses.startswith("../") or uses.startswith("docker://"))


def check_uses(path: Path, line: int, uses: str) -> None:
    if not is_remote(uses):
        return
    if "@" not in uses:
        problems.append(f"{path}:{line}: '{uses}' names no ref (no @) to pin")
        return
    ref = uses.rsplit("@", 1)[1]
    if not SHA.fullmatch(ref):
        problems.append(
            f"{path}:{line}: '{uses}' is pinned to '{ref}', not a 40-character commit SHA"
        )


def check_setup_go(path: Path, step: Mapping) -> None:
    """A step of actions/setup-go reads Go from go.dev and sends no token. The action's owner and
    name are compared without regard to case, as GitHub resolves them (`Actions/Setup-Go@<sha>`
    runs the same action) and as check-release-credentials.sh and check-bun-version.sh compare
    theirs. The input values are not printed: a YAML value can hold a newline, which would start a
    workflow command of its own."""
    global setup_go_steps
    uses = step["uses"]
    if not isinstance(uses, str) or uses.split("@", 1)[0].strip().lower() != SETUP_GO:
        return
    setup_go_steps += 1
    inputs = step.get("with")
    if not isinstance(inputs, Mapping):
        inputs = {}
    missing = []
    if inputs.get("go-download-base-url") != GO_DEV:
        missing.append(f"go-download-base-url: {GO_DEV}")
    if inputs.get("token") != "":
        missing.append("token: ''")
    if missing:
        setup_go_problems.append(
            f"{path}:{step.lines.get('uses', 0)}: '{uses}' lacks {' and '.join(missing)} under "
            f"with:, so it reads Go's version list through the GitHub API with the job's "
            f"GITHUB_TOKEN, from the one budget of 15,000 calls an hour every workflow run of this "
            f"repository draws on. With go-download-base-url: {GO_DEV} and token: '' it lists and "
            f"downloads Go from go.dev, makes no GitHub API call and sends no token there"
        )


def walk_steps(path: Path, steps: object) -> None:
    if not isinstance(steps, list):
        return
    for step in steps:
        if isinstance(step, Mapping) and "uses" in step:
            check_uses(path, step.lines.get("uses", 0), step["uses"])
            check_setup_go(path, step)


for path in targets:
    try:
        doc = yaml.load(path.read_text(), Loader=LineLoader)
    except yaml.YAMLError as error:
        problems.append(f"{path} is not valid YAML: {error}")
        continue
    if not isinstance(doc, Mapping):
        continue

    if path.name in {"action.yml", "action.yaml"}:
        runs = doc.get("runs")
        if isinstance(runs, Mapping):
            walk_steps(path, runs.get("steps"))
        continue

    jobs = doc.get("jobs")
    if not isinstance(jobs, Mapping):
        continue
    for job in jobs.values():
        if not isinstance(job, Mapping):
            continue
        if "uses" in job:
            check_uses(path, job.lines.get("uses", 0), job["uses"])
        walk_steps(path, job.get("steps"))

if problems or setup_go_problems:
    for problem in problems + setup_go_problems:
        print(f"::error::{problem}")
    print(
        f"{len(problems)} remote action(s) not pinned to a commit SHA, and "
        f"{len(setup_go_problems)} of {setup_go_steps} {SETUP_GO} step(s) not reading Go from go.dev",
        file=sys.stderr,
    )
    sys.exit(1)

setup_go_summary = (
    f"all {setup_go_steps} {SETUP_GO} steps read Go from go.dev"
    if setup_go_steps
    else f"no step uses {SETUP_GO}"
)
print(
    f"every remote action across {len(targets)} files is pinned to a commit SHA, and "
    f"{setup_go_summary}"
)
PY
