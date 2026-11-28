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
#      repeated key and a key differing only in case (actions/languageservices'
#      `is already defined` errors, https://github.com/actions/languageservices/blob/00d83b8521a6a179cc55ce9c93f5564035de0c5c/workflow-parser/src/templates/template-reader.ts#L212
#      and #L359, actions/runner
#      TemplateReader.cs with StringComparer.OrdinalIgnoreCase), while the YAML layer passes them
#      through on purpose (yaml-object-reader.ts parses with uniqueKeys: false). A load cannot find
#      it — yaml.safe_load keeps the last of a repeated key and reports nothing — so this walks the
#      composed nodes instead;
#   5. a file calling more than 46 unique reusable workflows. GitHub allows 50 from a single
#      workflow file, counting every tree of nested calls. This check follows only the calls to
#      this repository's own workflows (`./.github/workflows/…`) and counts a call to another
#      repository's workflow as one call. ci.yaml calls 44 today, and large-pr-lanes.yaml, which
#      calls ci.yaml, 45: ci.yaml itself and its whole tree. That is the limit, which keeps four
#      below GitHub's cap for a surprise and holds ci.yaml to 45, since its caller counts one more;
#      a job split in two calls the file it was split from, and a new job calls the file of a job
#      like it
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
#   7. a Postgres service (a `services:` entry whose key, or whose `image:`, holds `postgres` or
#      `pgvector`) whose `command:` is not the line `.github/scripts/postgres-service-settings.sh
#      --command` prints, the words compared as written, or whose job has no step whose `run:`
#      runs .github/scripts/postgres-service-settings.sh, in a place COMMAND_PLACE below counts as
#      a run, with that service's container id (`${{ job.services.<service id>.id }}`) as its
#      first argument on the same line: the id in a comment or another command reads nothing. The
#      command line is the server's settings (crash safety off, max_connections,
#      max_prepared_transactions), and the step reads them back from the running server; that
#      script's header says what each setting costs or buys. A service the check cannot tell is
#      Postgres by its key or its image is not its business.
#   8. a job that calls a reusable workflow of this repository and grants less than it declares:
#      for each scope that workflow's own `permissions` or any of its jobs' `permissions` names,
#      the calling job's `permissions` (the caller workflow's where the job sets none) must grant
#      it at that level or above (`read` below `write`; `write-all` and `read-all` as GitHub reads
#      them). GitHub refuses a run whose calling job grants less than the called workflow's jobs
#      declare, whatever their `if:`, and refuses it at startup, with no job and no check run, so
#      a scope added to a called workflow alone stops every run of its caller with no signal on the
#      head: .github/workflows/large-pr-lanes.yaml, which calls ci.yaml on an approval, is the
#      caller this was written for. A caller with no `permissions` anywhere passes GitHub's
#      default, which this check cannot read, so it is refused too.
#   9. a job that a check main's ruleset requires is named for, read from its `name:` or, where it
#      has none, its job id, as GitHub names it: named so as a plain word while it carries an
#      `if:` (where the `if:` skips it, its check run, of that name, reads as a pass); named so by
#      a `<condition> && '<check>' || '<other>'` expression whose condition is not its own `if:`
#      word for word (GitHub evaluates the name only where the job runs); named by an expression
#      that can yield the check's name in any other form, which this rule cannot read; and, in its
#      file, each job that waits for nothing whose `if:` is not that job's. A job is selected by
#      the name it can carry, not by its condition's text, so no drift of a condition, a name or
#      an `if:` takes it out of the rule; an expression that yields the check's name only where
#      its condition fails (the `ci-ok (dispatch)` name of the main CI workflow) is evaluated wherever its job runs, and
#      a skipped one posts the expression's text, so it is held to nothing here. And a workflow
#      that calls ci.yaml must name a job for ci-ok: its call posts `<job> / ci-ok`, which no
#      ruleset reads, so only a job of its own holds the queue while the call runs.
#      .github/workflows/large-pr-lanes.yaml is the file this was written for, where a `lead` that
#      admits an approval its ci-ok does not runs every slow suite with no ci-ok holding the queue.
#  10. in ci.yaml and every workflow of this repository it reaches through its calls (the run a
#      pull request and the merge queue get), a line of a step's `run:` that runs `bun test`, or
#      `bun run` of a script whose name starts with `test`, other than under
#      .github/scripts/bun_test_run.py, which keeps bun's JUnit report with a marker of the run's
#      start and bun's exit status beside it; a step that uses `harness-pool.sh start` to start
#      the wrapper with no later step that runs `harness-pool.sh wait`; a job with no step that
#      uses ./.github/actions/bun-test-reports-upload after its last bun test ends (after the
#      pool's next wait step for a run the pool starts); the first such upload step if its name
#      is not "Keep the bun test reports", its `if:` is not `always()`, or its `check-run-id` is
#      not `${{ job.check_run_id }}`; and, once such a job exists, that action's own
#      upload-artifact step when it does not upload `bun-test-reports/` as
#      `bun-test-reports-<check-run-id>` with its hidden files. A step's refusal names the file,
#      job, step number and name, and line. An action's refusal names its file and line, or only
#      its file if it cannot be read. The rule is
#      .github/scripts/bun_test_steps.py, whose head lists the forms it does not read; the names
#      it holds steps and the action to are bun_test_record.py's, which every reader of the
#      record imports, so they have one spelling.
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
# test fixture links only this script in, and the link leads back to the module CI runs. Rule 7's
# command line is read the same way, from the postgres-service-settings.sh beside this file, and
# rule 10 is bun_test_steps.py, imported from there the same way. Resolved before the cd, which
# would leave a relative $0 naming nothing. PYTHONDONTWRITEBYTECODE keeps the import from leaving a
# __pycache__ in the tree.
scripts=$(dirname "$(readlink -f "$0")")

cd "$(dirname "$0")/../.."

PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$scripts" WORKFLOW_CHECK_SCRIPTS="$scripts" python3 <<'PY'
import copy
import os
import re
import subprocess
import sys
from pathlib import Path

import yaml
from bun_test_steps import Census, check_bun_test_steps, check_upload_action
from workflow_paths import REQUIRED_CHECKS, mapping_get, one_line, repeated_keys, scalar

WORKFLOWS = Path(".github/workflows")
SUFFIXES = {".yaml", ".yml"}
# The limit this check holds, and the cap GitHub holds. Every refusal names both: a reader who has
# only crossed the first needs to know what the second does to a run.
LIMIT = 64_000
GITHUB_CAP = 500_000
# Calls counted as fault 5 of the header says: a call to this repository's own workflow is
# followed, and a call to another repository's workflow counts as one.
CALL_LIMIT = 46
GITHUB_CALL_CAP = 50
# Where the rule the two size refusals enforce is written down: what a workflow file holds, and
# where the rest goes.
RULE = ".github/ci-notes/README.md"


targets = (
    sorted(path for path in WORKFLOWS.iterdir() if not path.is_dir() and path.suffix in SUFFIXES)
    if WORKFLOWS.is_dir()
    else []
)

problems: list[str] = []
nodes: dict[Path, tuple[object, object]] = {}
documents: dict[Path, object] = {}


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


def loaded(path: Path) -> object:
    """The file's document as yaml.safe_load builds it, constructed from composed()'s node tree
    rather than read and parsed again, or None where there is no tree or no constructor takes one
    of its tags. Cached beside that tree. The constructor rewrites the merge keys (`<<`) of the
    tree it builds from, so it builds from a copy: every other rule reads the tree as written."""
    if path not in documents:
        node, _ = composed(path)
        try:
            documents[path] = None if node is None else yaml.SafeLoader("").construct_document(copy.deepcopy(node))
        except yaml.YAMLError:
            documents[path] = None
    return documents[path]


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


# Rule 8. The scopes GitHub's `permissions` names, and their levels in order.
SCOPES = (
    "actions", "attestations", "checks", "contents", "deployments", "discussions", "id-token",
    "issues", "models", "packages", "pages", "pull-requests", "repository-projects",
    "security-events", "statuses",
)
LEVELS = {"none": 0, "read": 1, "write": 2}
LEVEL_NAMES = {level: name for name, level in LEVELS.items()}


def granted(block: object) -> dict[str, int]:
    """A `permissions` value as {scope: level}: a mapping what it names, `read-all` and
    `write-all` every scope, and anything else, `{}` and a value GitHub would not take, nothing.
    A scope written at no level GitHub takes is left out, so a caller is refused for it and a
    callee declares it not: this check holds nothing to a level it cannot read."""
    if isinstance(block, str):
        level = {"read-all": LEVELS["read"], "write-all": LEVELS["write"]}.get(block)
        return {} if level is None else {scope: level for scope in SCOPES}
    if isinstance(block, dict):
        return {str(scope): LEVELS[level] for scope, level in block.items() if isinstance(level, str) and level in LEVELS}
    return {}


def check_call_permissions(path: Path, document: dict, jobs: dict) -> None:
    """Rule 8: each job of `path` that calls a workflow of this repository grants, scope by scope,
    at least what that workflow's `permissions` and its jobs' `permissions` declare."""
    for job_id, job in jobs.items():
        uses = job.get("uses") if isinstance(job, dict) else None
        callee = local_callee(uses.strip()) if isinstance(uses, str) else None
        if callee is None or not callee.is_file():
            continue
        # A callee that does not load is named by that file's own read. One that is no mapping (a
        # file of comments alone loads as None, a list as a list) declares no scope; rule 3 lets
        # such a file through, so it is read as declaring nothing.
        called = loaded(callee)
        if not isinstance(called, dict):
            continue
        need: dict[str, int] = {}
        blocks = [called.get("permissions")]
        called_jobs = called.get("jobs")
        bodies = called_jobs.values() if isinstance(called_jobs, dict) else ()
        blocks += [body.get("permissions") for body in bodies if isinstance(body, dict)]
        for block in blocks:
            for scope, level in granted(block).items():
                need[scope] = max(need.get(scope, 0), level)
        if not any(need.values()):
            continue
        declared = ", ".join(f"{s}: {LEVEL_NAMES[n]}" for s, n in sorted(need.items()) if n)
        block = job["permissions"] if "permissions" in job else document.get("permissions")
        if block is None:
            problem(path, f"{path}: job '{job_id}' calls {callee}, which declares {declared}, and "
                          f"names no `permissions`, nor does its workflow, so GitHub hands it the "
                          f"repository's default, which this check cannot read. Name the scopes "
                          f"on the job." + limits(path))
            continue
        have = granted(block)
        short = sorted(scope for scope, level in need.items() if have.get(scope, 0) < level)
        if short:
            lacking = ", ".join(f"{s}: {LEVEL_NAMES[need[s]]} (granted {LEVEL_NAMES[have.get(s, 0)]})" for s in short)
            problem(path, f"{path}: job '{job_id}' calls {callee}, and grants less than that "
                          f"workflow declares: {lacking}. GitHub refuses such a run at startup, "
                          f"with no job and no check run, so every run of this caller would stop "
                          f"with no signal on the head." + limits(path))


# Rule 9. A name that yields a check only where a condition holds: `<condition> && '<check>' ||
# '<other>'`, the whole of it in one `${{ }}` or in none, a quote in a literal written twice.
NAMED_CHECK = re.compile(r"^\s*(?:\$\{\{)?\s*(?P<condition>.*?)\s*&&\s*'(?P<check>(?:[^']|'')*)'\s*\|\|\s*"
                         r"'(?P<other>(?:[^']|'')*)'\s*(?:\}\})?\s*$", re.S)
QUOTED_LITERAL = re.compile(r"'((?:[^']|'')*)'")
# The workflow whose call a caller must hold the queue beside (rule 9's last clause), and the check
# it posts.
CI_WORKFLOW, CI_VERDICT = "./.github/workflows/ci.yaml", "ci-ok"


def condition_text(value: object) -> str:
    """An `if:`, or the condition a name holds, as its words: GitHub reads the expression, so a
    line break or a run of spaces is no difference."""
    text = str(value).strip()
    if text.startswith("${{") and text.endswith("}}"):
        text = text[3:-2]
    return one_line(text)


def unquote(literal: str) -> str:
    return literal.replace("''", "'")


def check_one_condition(path: Path, jobs: dict) -> None:
    """Rule 9, which the header says in full. Every job is read: its name is its `name:`, or its job
    id where it has none, as GitHub names it; a plain name that is a required check, or an
    expression any literal of which is one, selects the job. The first selected job's own `if:`
    is the condition every job of the file that waits for nothing must write, whether or not its
    name agrees with it, so a drift of the name is reported beside a drift of the others."""
    held = holder = None
    verdict_named = False
    for job_id, job in jobs.items():
        if not isinstance(job, dict):
            continue
        own = condition_text(job.get("if", ""))
        name = str(job.get("name", job_id)).strip()
        given = "its `name:`" if "name" in job else "its job id, which GitHub names a job by when it has no `name:`"
        if "${{" not in name:
            if name in REQUIRED_CHECKS and own:
                problem(path, f"{path}: job '{job_id}' is named {name} by {given}, a check main's "
                              f"ruleset requires, as a plain word, and carries an `if:`, so where "
                              f"that `if:` skips the job its check run {name} reads as a pass. Write "
                              f"the `if:` into the name: `${{{{ <if> && '{name}' || '<another "
                              f"name>' }}}}`." + limits(path))
            continue
        yields = {unquote(q) for q in QUOTED_LITERAL.findall(name)} & set(REQUIRED_CHECKS)
        if not yields:
            continue
        match = NAMED_CHECK.match(name)
        if match is None:
            problem(path, f"{path}: job '{job_id}' carries `name: {name}`, which can evaluate to "
                          f"{', '.join(sorted(yields))}, a check main's ruleset requires, in a form "
                          f"this rule does not read. Write it as `${{{{ <if> && '<check>' || "
                          f"'<another name>' }}}}`, with the job's `if:` as its condition."
                          + limits(path))
            continue
        check = unquote(match.group("check"))
        if check not in REQUIRED_CHECKS:
            continue  # it yields the check only where its condition fails (the header's rule 9)
        if check == CI_VERDICT:
            verdict_named = True
        wanted = condition_text(match.group("condition"))
        if wanted != own:
            problem(path, f"{path}: job '{job_id}' is named {check}, a check main's ruleset "
                          f"requires, where `{wanted}` holds, and runs where "
                          f"{f'`{own}` holds' if own else 'nothing skips it (it has no `if:`)'}. "
                          f"GitHub evaluates the name only where the job runs, so the two must be "
                          f"one text, or the check is posted where it should not be, or "
                          f"missing where it should." + limits(path))
        if holder is None:
            held, holder = own, str(job_id)
    calls_ci = any(isinstance(job, dict) and str(job.get("uses", "")).strip() == CI_WORKFLOW for job in jobs.values())
    if calls_ci and not verdict_named:
        problem(path, f"{path}: a job of this workflow calls {CI_WORKFLOW[2:]}, whose own verdict is "
                      f"posted as `<job> / {CI_VERDICT}`, which main's ruleset does not read, and no "
                      f"job of this workflow is named for {CI_VERDICT}, so nothing holds the pull "
                      f"request out of the queue while the call runs. Name the job that holds it "
                      f"`${{{{ <if> && '{CI_VERDICT}' || '<another name>' }}}}`." + limits(path))
    if holder is None:
        return
    for job_id, job in jobs.items():
        if not isinstance(job, dict) or str(job_id) == holder or "needs" in job:
            continue
        own = condition_text(job.get("if", ""))
        if own != held:
            problem(path, f"{path}: job '{job_id}' waits for no job and writes "
                          f"`{own or 'no if:'}` where job '{holder}', which a required check is named "
                          f"for, writes `{held or 'no if:'}` in its `if:`. The jobs of this file that "
                          f"the event alone decides must write one condition, or one of them runs "
                          f"where the job named for the check does not." + limits(path))


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


SETTINGS = "postgres-service-settings.sh"
POSTGRES_WORDS = ("postgres", "pgvector")


def service_command() -> str | None:
    """The command line every Postgres service starts with, as SETTINGS --command prints it from
    beside this file, its words joined by one space. None, with a problem saying why, when it
    cannot be read: rule 7 then holds each job to its read-back step alone."""
    script = Path(os.environ["WORKFLOW_CHECK_SCRIPTS"]) / SETTINGS
    try:
        done = subprocess.run(["bash", str(script), "--command"], capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.TimeoutExpired) as error:
        why = one_line(error)
    else:
        line = " ".join(done.stdout.split())
        if done.returncode == 0 and line:
            return line
        why = f"it exited {done.returncode} and printed '{one_line(done.stdout + done.stderr)}'"
    problem(
        Path(".github/scripts") / SETTINGS,
        f"`.github/scripts/{SETTINGS} --command` gives no command line ({why}), so no Postgres "
        f"service's command: line can be held to it.",
    )
    return None


def reads_back(run: str, container: str) -> bool:
    """Whether a line of `run` outside a comment runs SETTINGS, in a place runs_any counts, with
    `${{ <container> }}` as the script's first argument. The container's id elsewhere in the step (a
    comment, an echo, another command) is no read-back, and neither is the `--command` form."""
    argument = re.compile(
        re.escape(SETTINGS) + r"[\"']?\s+[\"']?\$\{\{\s*" + re.escape(container) + r"\s*\}\}[\"']?(?=\s|$)"
    )
    return any(runs_any(line, {SETTINGS}) and argument.search(line) for line in run.splitlines())


def check_postgres_services(path: Path, root: object, command: str | None) -> None:
    """Rule 7 of the header: each Postgres service starts on `command`, and its job reads the
    settings back with SETTINGS and the service's container id."""
    jobs = mapping_get(root, "jobs")
    if not isinstance(jobs, yaml.MappingNode):
        return
    for job_name, job in jobs.value:
        services = mapping_get(job, "services")
        if not isinstance(services, yaml.MappingNode):
            continue
        postgres = [
            (name, service) for name, service in services.value
            if any(word in (scalar(name) or "").lower() or word in (scalar(mapping_get(service, "image")) or "").lower()
                   for word in POSTGRES_WORDS)
        ]
        if not postgres:
            continue
        steps = mapping_get(job, "steps")
        runs = [scalar(mapping_get(step, "run")) or "" for step in steps.value] if isinstance(steps, yaml.SequenceNode) else []
        for name, service in postgres:
            service_id = scalar(name)
            line = name.start_mark.line + 1
            where = f"{path}:{line}: job {scalar(job_name)} runs the Postgres service '{service_id}'"
            given = mapping_get(service, "command")
            words = None if given is None else " ".join((scalar(given) or "").split())
            if command is not None and words != command:
                problem(
                    path,
                    f"{where} " + ("with no command: line" if given is None else f"on the command: line '{words}'")
                    + f", not the one `.github/scripts/{SETTINGS} --command` prints, so its server starts "
                    f"without the settings the tests here are written for (crash safety off, the "
                    f"connections and the prepared transactions they open; that script's header says "
                    f"what each buys). Write `command: {command}`.",
                    line=line,
                )
            container = f"job.services.{service_id}.id"
            if not any(reads_back(run, container) for run in runs):
                problem(
                    path,
                    f"{where} and no step of it runs .github/scripts/{SETTINGS} with that service's "
                    f"container id, so nothing reads its settings back from the running server, and a "
                    f"command: line the runner did not pass would leave the tests on the image's "
                    f"defaults with nothing that names the cause. Add the step every such job has, "
                    f"before its tests: `run: .github/scripts/{SETTINGS} \"${{{{ {container} }}}}\" {service_id}`.",
                    line=line,
                )


REACHERS = helper_reachers()
COMMAND = service_command()
# Rule 10 of the header (.github/scripts/bun_test_steps.py) reads ci.yaml and every workflow of
# this repository it reaches through its calls, or nothing when the directory holds no ci.yaml.
ROUTER = WORKFLOWS / "ci.yaml"
SCOPE = ({ROUTER} | {Path(called) for called in unique_callees(ROUTER) if Path(called).is_file()}
         if ROUTER.is_file() else set())
census = Census()


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
    check_postgres_services(path, node, COMMAND)
    if path in SCOPE:
        check_bun_test_steps(path, node, problem, census)

    called = unique_callees(path)
    if len(called) > CALL_LIMIT:
        problem(
            path,
            f"{path} calls {len(called)} unique reusable workflows, above this check's limit of "
            f"{CALL_LIMIT}: {', '.join(sorted(called))}. GitHub allows {GITHUB_CALL_CAP} from a "
            f"single workflow file, counting every tree of nested calls, and a file over that cap "
            f"starts no run." + limits(path),
        )
    # Rules 8 and 9 read the file's jobs as values, loaded once; the file composed above, so a
    # load that still fails (a tag no constructor takes) leaves them nothing to read.
    document = loaded(path)
    jobs = document.get("jobs") if isinstance(document, dict) else None
    if isinstance(jobs, dict):
        check_call_permissions(path, document, jobs)
        check_one_condition(path, jobs)

# Rule 10's last refusal, once: the action every such job keeps its reports with.
if census.jobs:
    check_upload_action(problem)

if problems:
    for annotation in problems:
        print(annotation)
    print(
        f"{len(problems)} problem(s) across the {len(targets)} workflow file(s) under {WORKFLOWS}",
        file=sys.stderr,
    )
    sys.exit(1)

kept = (
    f"run each of {census.commands} bun test command(s) in {census.steps} step(s) of {census.jobs} "
    f"job(s) of ci.yaml's run under bun_test_run.py, each job keeping its reports" if SCOPE else
    f"keep bun test reports UNKNOWN: there is no {ROUTER}, so rule 10 read no step"
)
print(
    f"all {len(targets)} workflow files under {WORKFLOWS} are within {LIMIT:,} bytes and GitHub's "
    f"{GITHUB_CAP:,}-byte cap, parse as YAML, repeat no mapping key after upper-casing, call "
    f"at most {CALL_LIMIT} unique reusable workflows, set JOB_TIMEOUT_MINUTES and "
    f"STEP_TIMEOUT_MINUTES to the timeout-minutes they stand for, start every Postgres service on "
    f"{SETTINGS}'s command line with a step that reads it back, grant each workflow they call "
    f"every scope it declares, write one condition in a job's name and in every `if:` beside it, "
    f"and {kept}"
)
PY
