#!/usr/bin/env bash
# Every file under .github/workflows stays small enough for GitHub to read it, and plain enough for
# GitHub's own parser to accept it.
#
# A workflow file above GitHub's cap of 500 KB starts no run at all, and it fails silently: the run
# appears with no jobs in it, no annotation and no error, so the change that crossed the cap reads
# as a change that had nothing to do. .github/workflows/ci.yaml crossed it, which is why this check
# exists (dispatch://INFRA-2507). So the limit held here is 64,000 bytes, far below the cap: a
# workflow file is a list of jobs, and filter data, inline shell and prose belong in a file of their
# own that a job reads, where a reviewer can read them too.
#
# What it refuses, reporting every fault of every file in one run:
#
#   1. a file above 64,000 bytes;
#   2. a file above 500,000 bytes, GitHub's own cap, which is the same fault with the consequence
#      named, because such a file starts no run whatever this check says;
#   3. a file that is not valid YAML, which GitHub reads no further than either;
#   4. two keys of one mapping that are equal after upper-casing. GitHub's parsers refuse both a
#      repeated key and a key differing only in case (actions/languageservices
#      workflow-parser/src/templates/template-reader.ts:212 and :359, actions/runner
#      TemplateReader.cs with StringComparer.OrdinalIgnoreCase), while the YAML layer passes them
#      through on purpose (yaml-object-reader.ts parses with uniqueKeys: false). A load cannot find
#      it — yaml.safe_load keeps the last of a repeated key and reports nothing — so this walks the
#      composed nodes instead;
#   5. a file calling more than 45 unique reusable workflows. GitHub allows 50 from a single
#      workflow file, counting every tree of nested calls, and ci.yaml calls 42 today, so 45 leaves
#      room for a few more calls and none for a surprise.
#
# A file that is not valid YAML is still measured for its bytes, and cannot be read for the other
# two faults.
#
# Scope: every file whose parent directory is .github/workflows and whose suffix is .yaml or .yml.
# Any other file of that directory is passed over. Files are discovered by walking the tree, not by
# asking git, so an unsnapshotted change cannot read green locally.
#
# Run from anywhere: .github/scripts/check-workflow-files.sh
# CI runs it in ci.yaml's ci-filters job and in .github/workflows/workflow-files.yaml, and its tests
# in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import sys
from pathlib import Path

import yaml

SKIP_DIRS = {".git", ".jj", "node_modules", ".venv", "vendor"}
WORKFLOWS = Path(".github/workflows")
SUFFIXES = {".yaml", ".yml"}
# The limit this check holds, and the cap GitHub holds. Every refusal names both: a reader who has
# only crossed the first needs to know what the second does to a run.
LIMIT = 64_000
GITHUB_CAP = 500_000
# Calls counted as GitHub counts them, over the whole tree of nested calls.
CALL_LIMIT = 45
GITHUB_CALL_CAP = 50


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


def one_line(text: object) -> str:
    """A parser's message on one line: an annotation is one line, and a traceback is noise to the
    reader who has to fix the file."""
    return " ".join(str(text).split())


targets = sorted(
    path
    for path in walk()
    if path.parent == WORKFLOWS and path.suffix in SUFFIXES
)

problems: list[str] = []
nodes: dict[Path, tuple[object, object]] = {}


def problem(path: Path, message: str, line: int | None = None) -> None:
    where = f"{path}" if line is None else f"{path},line={line}"
    problems.append(f"::error file={where}::{message}")


def composed(path: Path) -> tuple[object, object]:
    """The file's YAML node tree, or the error that stopped it. Cached, since a callee is read once
    however many jobs call it."""
    if path not in nodes:
        try:
            nodes[path] = (yaml.compose(path.read_text()), None)
        except (OSError, UnicodeDecodeError, yaml.YAMLError) as error:
            nodes[path] = (None, error)
    return nodes[path]


def mapping_get(node: object, key: str) -> object:
    if not isinstance(node, yaml.MappingNode):
        return None
    for name, value in node.value:
        if isinstance(name, yaml.ScalarNode) and name.value == key:
            return value
    return None


def check_folded_keys(path: Path, root: object) -> None:
    """Every mapping reachable from the document, sequences included: two keys equal after
    upper-casing are two keys GitHub's parser reads as one, and it refuses the file."""
    stack = [root]
    while stack:
        node = stack.pop()
        if isinstance(node, yaml.MappingNode):
            first_seen: dict[str, yaml.ScalarNode] = {}
            for name, value in node.value:
                if isinstance(name, yaml.ScalarNode):
                    folded = name.value.upper()
                    first = first_seen.get(folded)
                    if first is None:
                        first_seen[folded] = name
                    else:
                        line = name.start_mark.line + 1
                        problem(
                            path,
                            f"{path}:{line}: the mapping key '{name.value}' is the key "
                            f"'{first.value}' of line {first.start_mark.line + 1} again after "
                            f"upper-casing. GitHub's workflow parser compares keys without regard "
                            f"to case and refuses the file, while YAML keeps the last of the two "
                            f"and reports nothing.",
                            line=line,
                        )
                else:
                    stack.append(name)
                stack.append(value)
        elif isinstance(node, yaml.SequenceNode):
            stack.extend(node.value)


def local_callee(uses: str) -> Path | None:
    """A call this repository holds the file for, which is followed; anything else (an
    owner/repo/.github/workflows/x.yml@ref) counts as one call and is not followed."""
    if not uses.startswith("./"):
        return None
    return Path(uses[2:])


def job_uses(node: object) -> list[str]:
    """A job-level `uses:` is a reusable-workflow call. A step-level `uses:` is an action, and
    GitHub counts no action against this limit."""
    found = []
    jobs = mapping_get(node, "jobs")
    if isinstance(jobs, yaml.MappingNode):
        for _, job in jobs.value:
            uses = mapping_get(job, "uses")
            if isinstance(uses, yaml.ScalarNode) and uses.value.strip():
                found.append(uses.value.strip())
    return found


def unique_callees(start: Path) -> list[str]:
    """Every reusable workflow the file reaches, each counted once however many jobs call it and
    however deep the tree that reaches it. The file itself is not one of them."""
    called: dict[str, None] = {}
    queue = [start]
    read = {start}
    while queue:
        node, error = composed(queue.pop(0))
        if error is not None:
            continue
        for uses in job_uses(node):
            callee = local_callee(uses)
            if callee == start:
                continue
            called[uses if callee is None else str(callee)] = None
            if callee is not None and callee not in read:
                read.add(callee)
                queue.append(callee)
    return list(called)


for path in targets:
    size = path.stat().st_size
    if size > GITHUB_CAP:
        problem(
            path,
            f"{path} is {size:,} bytes, above GitHub's own cap of {GITHUB_CAP:,} bytes (500 KB): "
            f"GitHub starts no run at all for a workflow file this size, and signals it only as a "
            f"run with no jobs in it. It is above this check's limit of {LIMIT:,} bytes as well. "
            f"Move filter data, inline shell and prose into files of their own that a job reads.",
        )
    elif size > LIMIT:
        problem(
            path,
            f"{path} is {size:,} bytes, above this check's limit of {LIMIT:,} bytes. GitHub starts "
            f"no run at all for a workflow file above its own cap of {GITHUB_CAP:,} bytes (500 "
            f"KB), and signals it only as a run with no jobs in it, so this limit keeps every file "
            f"far enough below the cap that no edit crosses it unseen. Move filter data, inline "
            f"shell and prose into files of their own that a job reads.",
        )

    node, error = composed(path)
    if error is not None:
        if isinstance(error, yaml.YAMLError):
            problem(
                path,
                f"{path} is not valid YAML: {one_line(error)}. GitHub reads it no further than "
                f"this check does, and neither can read the file's keys or its "
                f"reusable-workflow calls until the syntax is fixed.",
            )
        else:
            problem(path, f"{path} could not be read: {one_line(error)}")
        continue
    if node is None:
        continue

    check_folded_keys(path, node)

    called = unique_callees(path)
    if len(called) > CALL_LIMIT:
        problem(
            path,
            f"{path} calls {len(called)} unique reusable workflows, above this check's limit of "
            f"{CALL_LIMIT}: {', '.join(sorted(called))}. GitHub allows {GITHUB_CALL_CAP} from a "
            f"single workflow file, counting every tree of nested calls, and a file over that cap "
            f"starts no run.",
        )

if problems:
    for annotation in problems:
        print(annotation)
    print(
        f"{len(problems)} problem(s) across the {len(targets)} workflow file(s) under {WORKFLOWS}",
        file=sys.stderr,
    )
    sys.exit(1)

print(
    f"all {len(targets)} workflow files under {WORKFLOWS} are within {LIMIT:,} bytes and GitHub's "
    f"{GITHUB_CAP:,}-byte cap, parse as YAML, repeat no mapping key after upper-casing, and call "
    f"at most {CALL_LIMIT} unique reusable workflows"
)
PY
