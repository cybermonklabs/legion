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
#      workflow file, counting every tree of nested calls. This check follows only the calls to
#      this repository's own workflows (`./.github/workflows/…`) and counts a call to another
#      repository's workflow as one call. ci.yaml calls 45 today, the limit, which keeps five
#      below GitHub's cap for a surprise; a job split in two calls the file it was split from
#      (.github/ci-notes/ci.md#two-jobs-that-call-one-file);
#   6. a job whose `env:`, or one of whose steps' `env:`, sets JOB_TIMEOUT_MINUTES to anything but
#      the job's timeout-minutes (GitHub's default of 360 where the job sets none), and a step whose
#      `env:` sets STEP_TIMEOUT_MINUTES to anything but that step's own timeout-minutes.
#      .github/scripts/gh-read.sh waits for a spent API budget within those two numbers, measured
#      from the job's start and the step's, and GitHub cancels the job at its timeout-minutes, so a
#      larger number would let the wait outlive the job, and a smaller one would fail by name a wait
#      the job had time for. The values are compared as written, so an expression must be the same
#      expression. Three more ways to give the wait a wrong number are refused with it: either name
#      in the workflow's own `env:`, which stands for no one job's limit; STEP_TIMEOUT_MINUTES in a
#      job's `env:`, which stands for no one step's; and a step with a timeout-minutes of its own
#      that reaches gh-read.sh and sets no STEP_TIMEOUT_MINUTES, whose wait would see only the
#      job's limit. A step reaches gh-read.sh when its `run:` runs or sources it, or a script of
#      .github/scripts that does, followed through the scripts that run or source each other (test
#      files aside; COMMAND_PLACE below says what counts as a run), or when its `uses:` is an
#      action of .github/actions, at the checkout's root or under another checkout (`./base/`),
#      whose steps' `run:` does. A path named in a comment or echoed is no reach.
#
# A file that is not valid YAML is still measured for its bytes, and cannot be read for the other
# faults.
#
# Scope: every file whose parent directory is .github/workflows and whose suffix is .yaml or .yml.
# Any other file of that directory is passed over. Files are discovered by listing that directory,
# not by asking git, so an unsnapshotted change cannot read green locally.
#
# Run from anywhere: .github/scripts/check-workflow-files.sh
# CI runs it in ci.yaml's ci-filters job and in .github/workflows/workflow-files.yaml, and its tests
# in ci.yaml's test job.
set -euo pipefail

# The repeated-key test is workflow_paths.repeated_keys, the same one the loader every filter
# check reads the changes job's filters and the workflows with runs, imported from beside this script's own file: a
# test fixture links only this script in, and the link leads back to the module CI runs. Resolved
# before the cd, which would leave a relative $0 naming nothing. PYTHONDONTWRITEBYTECODE keeps the
# import from leaving a __pycache__ in the tree.
scripts=$(dirname "$(readlink -f "$0")")

cd "$(dirname "$0")/../.."

PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$scripts" python3 <<'PY'
import re
import sys
from pathlib import Path

import yaml
from workflow_paths import repeated_keys

WORKFLOWS = Path(".github/workflows")
SUFFIXES = {".yaml", ".yml"}
# The limit this check holds, and the cap GitHub holds. Every refusal names both: a reader who has
# only crossed the first needs to know what the second does to a run.
LIMIT = 64_000
GITHUB_CAP = 500_000
# Calls counted as fault 5 of the header says: a call to this repository's own workflow is
# followed, and a call to another repository's workflow counts as one.
CALL_LIMIT = 45
GITHUB_CALL_CAP = 50
# Where the rule the two size refusals enforce is written down: what a workflow file holds, and
# where the rest goes.
RULE = ".github/ci-notes/README.md"


def one_line(text: object) -> str:
    """A parser's message on one line: an annotation is one line, and a traceback is noise to the
    reader who has to fix the file."""
    return " ".join(str(text).split())


targets = (
    sorted(path for path in WORKFLOWS.iterdir() if not path.is_dir() and path.suffix in SUFFIXES)
    if WORKFLOWS.is_dir()
    else []
)

problems: list[str] = []
nodes: dict[Path, tuple[object, object]] = {}


def escape_message(value: str) -> str:
    """A workflow command's message, escaped as ci_filter_coverage.py's escape_annotation_message
    does: `%`, CR and LF, and the legacy `##[` command, which the runner recognises anywhere in a
    line. A message can hold YAML text (a repeated key, a `uses:` value), and the runner reads
    each line of a step's output as a command of its own, so a key holding a newline would start
    one. This is a copy rather than an import because this check runs when ci.yaml cannot, and
    importing the filter engine would tie it to the files the filter checks read."""
    return (
        value.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A").replace("##[", "%23%23%5B")
    )


def escape_property(value: str) -> str:
    """A property's value, escaped as ci_filter_coverage.py's escape_property does: the message
    escape above, then `:` and `,`, since the runner ends the properties at `::` and splits them
    on `,`."""
    return escape_message(value).replace(":", "%3A").replace(",", "%2C")


def problem(path: Path, message: str, line: int | None = None) -> None:
    where = escape_property(str(path)) + ("" if line is None else f",line={line}")
    problems.append(f"::error file={where}::{escape_message(message)}")


def limits(path: Path) -> str:
    """The two numbers a refusal for anything but the size names all the same, with the file's own
    bytes against them, so that every refusal says what the limit and the cap are."""
    try:
        size = f"{path.stat().st_size:,} bytes"
    except OSError:
        size = "of a size this check could not read"
    return (
        f" The file is {size}, against this check's limit of {LIMIT:,} bytes and GitHub's cap "
        f"of {GITHUB_CAP:,} bytes (500 KB), above which GitHub starts no run at all."
    )


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
    upper-casing are two keys GitHub's parser reads as one, and it refuses the file. Each node is
    visited once, so a mapping an alias repeats is reported once and a recursive alias ends."""
    stack = [root]
    seen: set[int] = set()
    while stack:
        node = stack.pop()
        if id(node) in seen:
            continue
        seen.add(id(node))
        if isinstance(node, yaml.MappingNode):
            for first, name in repeated_keys(node, lambda key: key.value.upper()):
                line = name.start_mark.line + 1
                problem(
                    path,
                    f"{path}:{line}: the mapping key '{name.value}' is the key "
                    f"'{first.value}' of line {first.start_mark.line + 1} again after "
                    f"upper-casing. GitHub's workflow parser compares keys without regard "
                    f"to case and refuses the file, while YAML keeps the last of the two "
                    f"and reports nothing." + limits(path),
                    line=line,
                )
            for name, value in node.value:
                if not isinstance(name, yaml.ScalarNode):
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


def scalar(node: object) -> str | None:
    return node.value.strip() if isinstance(node, yaml.ScalarNode) else None


HELPER = "gh-read.sh"
# The ways a line of shell runs or sources a script, or keeps its path in a variable to run later:
# the path as a command's first word (a line's start, or after `;`, `&`, `|`, `!`, `$(`, `( `, a
# shell keyword, exec, time, env's assignments, timeout's limit, or bash or sh with their flags,
# `bash -c '<path>'` among them), a `${{ ... }}` expression allowed as its first part
# (`${{ github.workspace }}/.github/scripts/<x>`); `source` or `.` followed, anywhere later on the
# line, by the path or the bare name, however a `$(...)` builds its directory; and a shell
# assignment (no space around the `=`) whose value ends in the path. A path named in a comment, a
# description or a Python string runs nothing and is no reach.
COMMAND_PLACE = (
    r"(?:^|[;&|!]|\$\(|\(\s|\b(?:then|do|else|if|while|until|exec|time)\s|\b(?:bash|sh)(?:\s+-\w+)*\s"
    r"|\btimeout\s+\S+\s|\benv(?:\s+\w+=\S*)*\s)\s*[\"']?(?:\$\{\{[^}]*\}\})?[^\s\"';&|()]*/"
)
SOURCED = r"(?:^|[\s;&|\"'])(?:source|\.)\s+[\"']?(?:.*/)?"
ASSIGNED = r"(?:^|[\s;&|])[A-Za-z_]\w*=.*/"
# A local action, at the root of the checkout or of another checkout inside it (`./base/`).
LOCAL_ACTION = re.compile(r"^\./(?:[^/]+/)*\.github/actions/([^/]+)/?$")


def runs_any(text: str, names: set[str]) -> list[str]:
    """The names among `names` that a line of shell text outside a comment runs or sources, by a
    path ending in that name in one of the places above."""
    found = set()
    for line in text.splitlines():
        if line.lstrip().startswith("#"):
            continue
        for name in names:
            end = re.escape(name) + r"(?![\w.-])"
            if any(re.search(place + end, line) for place in (COMMAND_PLACE, SOURCED, ASSIGNED)):
                found.add(name)
    return sorted(found)


def helper_reachers() -> tuple[set[str], set[str]]:
    """The basenames of the scripts of .github/scripts that reach HELPER, HELPER itself among
    them, and the names of the actions of .github/actions whose steps' `run:` reaches one: a
    script reaches it when a line of it runs or sources HELPER or a script that does
    (runs_any), followed until nothing more joins. Test files and their libraries run no step's
    API call and are left out."""
    texts: dict[str, str] = {}
    for script in Path(".github/scripts").glob("*.sh"):
        if script.name.endswith(".test.sh") or "test-lib" in script.name:
            continue
        try:
            texts[script.name] = script.read_text(errors="replace")
        except OSError:
            continue
    scripts = {HELPER}
    while True:
        joined = {name for name, text in texts.items() if name not in scripts and runs_any(text, scripts)}
        if not joined:
            break
        scripts |= joined
    actions: set[str] = set()
    for action in Path(".github/actions").glob("*/action.y*ml"):
        try:
            doc = yaml.safe_load(action.read_text(errors="replace"))
        except (OSError, yaml.YAMLError):
            continue
        runs = doc.get("runs") if isinstance(doc, dict) else None
        steps = runs.get("steps") if isinstance(runs, dict) else None
        if not isinstance(steps, list):
            continue
        text = "\n".join(str(step.get("run", "")) for step in steps if isinstance(step, dict))
        if runs_any(text, scripts):
            actions.add(action.parent.name)
    return scripts, actions


def check_timeout_variables(path: Path, root: object, reachers: tuple[set[str], set[str]]) -> None:
    """Rule 6 of the header: each JOB_TIMEOUT_MINUTES a job or its steps set is the job's
    timeout-minutes, each STEP_TIMEOUT_MINUTES a step sets is that step's, neither name stands in
    the workflow's env, STEP_TIMEOUT_MINUTES stands in no job's env, and a step with a limit of
    its own that reaches gh-read.sh passes that limit."""
    why = (
        " .github/scripts/gh-read.sh waits for a spent API budget within this number, measured from "
        "the job's start (STEP_TIMEOUT_MINUTES: the step's), and GitHub cancels the job at its "
        "timeout-minutes: a larger number lets the wait outlive the job, and a smaller one fails "
        "by name a wait the job had time for. Write the two as one number."
    )
    scripts, actions = reachers

    def refuse(where: object, who: str, name: str, value: str, limit: str) -> None:
        line = where.start_mark.line + 1
        problem(path, f"{path}:{line}: {who} sets {name} to {value}, but {limit}." + why, line=line)

    def misplaced(where: object, text: str) -> None:
        line = where.start_mark.line + 1
        problem(path, f"{path}:{line}: {text}", line=line)

    for name in ("JOB_TIMEOUT_MINUTES", "STEP_TIMEOUT_MINUTES"):
        value = mapping_get(mapping_get(root, "env"), name)
        if value is not None:
            misplaced(value, f"the workflow's env sets {name}, which stands for no one "
                             f"{'job' if name.startswith('JOB') else 'step'}'s timeout-minutes: set it "
                             f"in the env of each {'job' if name.startswith('JOB') else 'step'} it is "
                             f"that limit of, where this check holds it to that number.")
    jobs = mapping_get(root, "jobs")
    if not isinstance(jobs, yaml.MappingNode):
        return
    for job_name, job in jobs.value:
        if not isinstance(job, yaml.MappingNode):
            continue
        job_id = scalar(job_name)
        timeout = mapping_get(job, "timeout-minutes")
        if timeout is None:
            job_limit, job_said = "360", "its timeout-minutes is 360 (GitHub's default, as the job sets none)"
        else:
            job_limit = scalar(timeout)
            job_said = f"its timeout-minutes is {job_limit} (line {timeout.start_mark.line + 1})"
        value = mapping_get(mapping_get(job, "env"), "JOB_TIMEOUT_MINUTES")
        if value is not None and scalar(value) != job_limit:
            refuse(value, f"job {job_id}", "JOB_TIMEOUT_MINUTES", scalar(value), job_said)
        value = mapping_get(mapping_get(job, "env"), "STEP_TIMEOUT_MINUTES")
        if value is not None:
            misplaced(value, f"job {job_id}'s env sets STEP_TIMEOUT_MINUTES, which stands for no one "
                             f"step's timeout-minutes: set it in the env of the step it is that limit of.")
        steps = mapping_get(job, "steps")
        if not isinstance(steps, yaml.SequenceNode):
            continue
        for index, step in enumerate(steps.value, start=1):
            env = mapping_get(step, "env")
            own = mapping_get(step, "timeout-minutes")
            value = mapping_get(env, "JOB_TIMEOUT_MINUTES")
            if value is not None and scalar(value) != job_limit:
                refuse(value, f"job {job_id}", "JOB_TIMEOUT_MINUTES", scalar(value), job_said)
            value = mapping_get(env, "STEP_TIMEOUT_MINUTES")
            if value is None:
                if own is None:
                    continue
                run = scalar(mapping_get(step, "run")) or ""
                uses = scalar(mapping_get(step, "uses")) or ""
                via = runs_any(run, scripts)
                local = LOCAL_ACTION.match(uses)
                if local and local.group(1) in actions:
                    via.append(uses)
                if via:
                    misplaced(own, f"step {index} of job {job_id} sets timeout-minutes {scalar(own)} and "
                                   f"reaches gh-read.sh (through {', '.join(via)}), but sets no "
                                   f"STEP_TIMEOUT_MINUTES: its wait for a spent API budget would see only "
                                   f"the job's limit and could outlive the step's. Set STEP_TIMEOUT_MINUTES "
                                   f"to {scalar(own)} in the step's env.")
                continue
            if own is None:
                refuse(value, f"step {index} of job {job_id}", "STEP_TIMEOUT_MINUTES", scalar(value),
                       "the step sets no timeout-minutes")
            elif scalar(value) != scalar(own):
                refuse(value, f"step {index} of job {job_id}", "STEP_TIMEOUT_MINUTES", scalar(value),
                       f"its timeout-minutes is {scalar(own)} (line {own.start_mark.line + 1})")


REACHERS = helper_reachers()


for path in targets:
    size = path.stat().st_size
    if size > GITHUB_CAP:
        problem(
            path,
            f"{path} is {size:,} bytes, above GitHub's own cap of {GITHUB_CAP:,} bytes (500 KB): "
            f"GitHub starts no run at all for a workflow file this size, and signals it only as a "
            f"run with no jobs in it. It is above this check's limit of {LIMIT:,} bytes as well. "
            f"Move each longer shell body into a script under .github/scripts/ and each derivation "
            f"into the workflow's notes file, as {RULE} says.",
        )
    elif size > LIMIT:
        problem(
            path,
            f"{path} is {size:,} bytes, above this check's limit of {LIMIT:,} bytes. GitHub starts "
            f"no run at all for a workflow file above its own cap of {GITHUB_CAP:,} bytes (500 "
            f"KB), and signals it only as a run with no jobs in it, so this limit keeps every file "
            f"far enough below the cap that no edit crosses it unseen. Move each longer shell body "
            f"into a script under .github/scripts/ and each derivation into the workflow's notes "
            f"file, as {RULE} says.",
        )

    node, error = composed(path)
    if error is not None:
        if isinstance(error, yaml.YAMLError):
            problem(
                path,
                f"{path} is not valid YAML: {one_line(error)}. GitHub reads it no further than "
                f"this check does, and neither can read the file's keys or its "
                f"reusable-workflow calls until the syntax is fixed." + limits(path),
            )
        else:
            problem(path, f"{path} could not be read: {one_line(error)}." + limits(path))
        continue
    if node is None:
        continue

    check_folded_keys(path, node)
    check_timeout_variables(path, node, REACHERS)

    called = unique_callees(path)
    if len(called) > CALL_LIMIT:
        problem(
            path,
            f"{path} calls {len(called)} unique reusable workflows, above this check's limit of "
            f"{CALL_LIMIT}: {', '.join(sorted(called))}. GitHub allows {GITHUB_CALL_CAP} from a "
            f"single workflow file, counting every tree of nested calls, and a file over that cap "
            f"starts no run." + limits(path),
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
    f"{GITHUB_CAP:,}-byte cap, parse as YAML, repeat no mapping key after upper-casing, call "
    f"at most {CALL_LIMIT} unique reusable workflows, and set JOB_TIMEOUT_MINUTES and "
    f"STEP_TIMEOUT_MINUTES to the timeout-minutes they stand for"
)
PY
