#!/usr/bin/env bash
# Every bounded retry a CI step runs must end, and print its named error, before its job's
# timeout-minutes cancels the job. A cancelled job's log says only "The operation was canceled",
# which is what job 136053067213 of run 44665276896 said when its apt-get hung. So this check holds
# the budget of each such call to its job's timeout, and fails on every call that does not fit.
#
# The calls it reads are the two scripts that take a budget and divide it:
#   .github/scripts/apt-install.sh --budget <seconds> <package>...
#   .github/scripts/changed-files.sh --budget <seconds> <directory>
# Each script bounds its own worst case - every call or attempt running to its bound, and then
# ignoring TERM until the KILL 10 s later - by the budget it is given (their headers show the
# division). What a script cannot know is its job's timeout, and that is what this check reads.
#
# The rule: a call's budget is at most half of the timeout it runs under, the smaller of its job's
# timeout-minutes and its step's own timeout-minutes. The other half is the margin, for every step
# that runs before the call and for the error line. The steps before the six calls of this tree took
# at most 81 s (dispatch-ui, of 900 s) in the pull request run that measured them.
#
# Fails, naming the workflow, the job and the step, on:
#   * a call whose budget is more than half the timeout it runs under, naming both;
#   * a call that names no --budget <seconds> right after the script;
#   * a job that holds such a call and carries no timeout-minutes, or one this check cannot
#     read: a literal number of minutes is read, and `${{ matrix.<key> }}` over the job's own
#     `strategy.matrix.include` rows or over the rows .github/e2e-scripts.json gives e2e.yaml's
#     proof job (select-e2e-proofs.sh emits them, each row's `apt` list joined by spaces), the
#     rows narrowed by a step `if:` of the form matrix.<key> == '<value>' or matrix.<key> != '',
#     and the smallest timeout of the rows left taken; anything else is refused, not guessed;
#   * a `run:` that calls apt or apt-get itself (update, install, upgrade): no budget bounds it;
#   * a composite action under .github/actions that runs either script or apt: an action's step
#     has no job timeout of its own to hold a budget to;
#   * no call of either script found at all, which would make every rule above pass over nothing.
#
# Run from anywhere: .github/scripts/check-retry-budgets.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import json
import re
import sys
from pathlib import Path

import yaml

SCRIPTS = ("apt-install.sh", "changed-files.sh")
CALL = re.compile(r"\.github/scripts/(apt-install\.sh|changed-files\.sh)\b([^\n]*)")
BUDGET = re.compile(r"^\s+--budget\s+(\d+)(?:\s|$)")
APT = re.compile(r"(?:^|[\s;&|(`])apt(?:-get)?\s+(?:-\S+\s+)*(?:update|install|upgrade|dist-upgrade|full-upgrade)\b")
MATRIX = re.compile(r"^\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}$")
IF_EQUALS = re.compile(r"^(?:\$\{\{\s*)?matrix\.([A-Za-z0-9_-]+)\s*==\s*'([^']*)'(?:\s*\}\})?$")
IF_NOT_EMPTY = re.compile(r"^(?:\$\{\{\s*)?matrix\.([A-Za-z0-9_-]+)\s*!=\s*''(?:\s*\}\})?$")
# The one matrix whose rows a job reads from a file: e2e.yaml's proof job takes its include list
# from the select job, which emits .github/e2e-scripts.json's `hosted` rows.
EMITTED_ROWS = {(".github/workflows/e2e.yaml", "proof"): ".github/e2e-scripts.json"}

problems: list[str] = []
reports: list[str] = []
counts = {name: 0 for name in SCRIPTS}


def problem(path, where, text):
    problems.append(f"::error file={path}::{where}: {text}")


def emitted_rows(source):
    rows = []
    for row in json.loads(Path(source).read_text()).get("hosted", []):
        row = dict(row)
        if isinstance(row.get("apt"), list):
            row["apt"] = " ".join(row["apt"])
        rows.append(row)
    return rows


def matrix_rows(path, job_id, job):
    """The rows a matrix job runs, each a dict of its keys, or None when they cannot be read."""
    if (str(path), job_id) in EMITTED_ROWS:
        return emitted_rows(EMITTED_ROWS[(str(path), job_id)])
    include = ((job.get("strategy") or {}).get("matrix") or {}).get("include")
    if isinstance(include, list) and all(isinstance(row, dict) for row in include):
        return include
    return None


def narrowed(rows, condition):
    """The rows a step `if:` admits, or None when the condition names the matrix in a form this
    check does not read."""
    if condition is None or "matrix." not in str(condition):
        return rows
    text = str(condition).strip()
    found = IF_EQUALS.match(text)
    if found:
        return [row for row in rows if str(row.get(found.group(1), "")) == found.group(2)]
    found = IF_NOT_EMPTY.match(text)
    if found:
        return [row for row in rows if str(row.get(found.group(1), "")) != ""]
    return None


def minutes(value, rows, what):
    """Each row's minutes for a timeout-minutes value, or a refusal's text."""
    if isinstance(value, int) and not isinstance(value, bool):
        return [value], None
    found = MATRIX.match(str(value).strip())
    if not found:
        return None, f"its {what} is {value!r}, which this check cannot read"
    if rows is None:
        return None, f"its {what} is {value!r}, and this check cannot read the job's matrix rows"
    key = found.group(1)
    values = [row.get(key) for row in rows]
    if not values or not all(isinstance(v, int) and not isinstance(v, bool) for v in values):
        return None, f"its {what} is {value!r}, and not every matrix row that runs the step gives {key} a number"
    return values, None


def check_step(path, job_id, job, step, index):
    run = step.get("run")
    if not isinstance(run, str):
        return
    where = f"job {job_id}, step {step.get('name') or step.get('id') or index + 1}"
    if APT.search(run):
        problem(path, where, "runs apt itself, which no budget bounds; run .github/scripts/apt-install.sh --budget <seconds> <package>... instead")
    for found in CALL.finditer(run):
        name, rest = found.group(1), found.group(2)
        counts[name] += 1
        budget = BUDGET.match(rest)
        if not budget:
            problem(path, where, f"calls {name} with no --budget <seconds> right after it")
            continue
        budget = int(budget.group(1))
        rows = matrix_rows(path, job_id, job)
        admitted = narrowed(rows, step.get("if")) if rows is not None else None
        if rows is not None and admitted is None:
            problem(path, where, f"its if: {step.get('if')!r} names the matrix in a form this check cannot read")
            continue
        if "timeout-minutes" not in job:
            problem(path, where, f"calls {name} in a job with no timeout-minutes, so its {budget} s budget fits nothing")
            continue
        job_minutes, refusal = minutes(job["timeout-minutes"], admitted, "job's timeout-minutes")
        if refusal:
            problem(path, where, refusal)
            continue
        limit = min(job_minutes)
        bound_by = f"its job's timeout-minutes of {limit}"
        if "timeout-minutes" in step:
            step_minutes, refusal = minutes(step["timeout-minutes"], admitted, "step's timeout-minutes")
            if refusal:
                problem(path, where, refusal)
                continue
            if min(step_minutes) < limit:
                limit = min(step_minutes)
                bound_by = f"its step's timeout-minutes of {limit}"
        seconds = limit * 60
        if budget * 2 > seconds:
            problem(path, where, f"calls {name} with a {budget} s budget, more than half of {bound_by} ({seconds} s), so a call that spends its budget can be cancelled with the job before its error prints; give it at most {seconds // 2} s")
        else:
            reports.append(f"report: {path} {where}: {name} --budget {budget}, at most half of {bound_by} ({seconds} s)")


workflows = sorted(p for p in Path(".github/workflows").iterdir() if p.suffix in {".yml", ".yaml"})
for path in workflows:
    document = yaml.safe_load(path.read_text()) or {}
    for job_id, job in (document.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        for index, step in enumerate(job.get("steps") or []):
            if isinstance(step, dict):
                check_step(path, job_id, job, step, index)

for path in sorted(Path(".github/actions").glob("*/action.y*ml")):
    document = yaml.safe_load(path.read_text()) or {}
    for index, step in enumerate((document.get("runs") or {}).get("steps") or []):
        run = step.get("run") if isinstance(step, dict) else None
        if isinstance(run, str) and (APT.search(run) or CALL.search(run)):
            problem(path, f"step {step.get('name') or index + 1}", "a composite action runs apt or a budgeted script, and an action's step has no job timeout to hold a budget to; run it from the workflow's own step")

for name, count in counts.items():
    if count == 0:
        problems.append(f"::error::found no call of .github/scripts/{name} in .github/workflows: UNKNOWN, there was nothing to hold to a timeout, so this is no pass")

for line in reports:
    print(line)
if problems:
    for line in problems:
        print(line)
    print(f"{len(problems)} problem(s) with the budgets of bounded retries", file=sys.stderr)
    sys.exit(1)
print(f"every bounded retry fits half the timeout it runs under: {counts['apt-install.sh']} call(s) of apt-install.sh and {counts['changed-files.sh']} of changed-files.sh, across {len(workflows)} workflow(s)")
PY
