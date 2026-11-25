"""The engine of check-ci-layout.sh, whose header numbers the rules and says why each holds.

It reads ci.yaml, every workflow a job of it calls, and every local composite action those run
(.github/actions/<job-or-leg>/action.yml), and refuses a layout that drifts from what it holds:
the jobs ci-ok judges and how it judges them (rule 7), what a job's `if:` may test (rules 2, 12,
23 and 28), the names a skipped job's record carries (rules 25 and 30), the carriers of a split
job's old name (rule 32) and the indexes its legs name (rule 34), how a Go step runs its tests
(rules 22, 24 and 27), the bounds a timed step keeps (rule 31), the matrices (rule 29), the runner
a job proves (rule 26), the darwin vets (rule 20), the notes a `why:` link names (rule 33) and the
pins of the scripts go_test_steps.py's AUDITED_SCRIPTS reads (rule 35).
ci_workflow_rules.py, beside it, holds rules 10, 11, 12, 14 and 16, which read the workflow
directory; main() runs both. ci_pins.py holds the facts a person pins by reading the layout.
go_test_steps.py reads the `go test` commands of the steps, as go-test-step-expansion.py and
check-test-reach.sh read them.

check-ci-layout.sh runs it from the repository root with this directory on PYTHONPATH. Every
annotation escapes its message, and the report lines print inside a stop-commands section, so
no repository text becomes a workflow command.
"""

from __future__ import annotations

import atexit
import functools
import hashlib
import itertools
import json
import os
import re
import secrets
import shlex
import shutil
import subprocess
import sys
import tempfile
import traceback
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path

import yaml
from go_build_tags import constraint_names, decode_stream, go_files, is_tag, platforms
from workflow_paths import UniqueKeyLoader

WORKFLOW = ".github/workflows/ci.yaml"
# The workflow that fills, on a push to main, the Go cache families WORKFLOW's jobs restore.
CACHE_WORKFLOW = ".github/workflows/ci-caches.yaml"
REPO = Path.cwd().resolve()


# ---------------------------------------------------------------- annotations
def escape_message(value: str) -> str:
    return value.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")


def escape_annotation_message(value: str) -> str:
    """Also escape legacy runner commands, which recognize ##[ anywhere in a line."""
    return escape_message(value).replace("##[", "%23%23%5B")


def escape_property(value: str) -> str:
    return escape_annotation_message(value).replace(":", "%3A").replace(",", "%2C")


@dataclass(frozen=True)
class Annotation:
    severity: str
    file: str | None
    message: str


def error(file: str | None, message: str) -> Annotation:
    return Annotation("error", file, message)


def warning(file: str | None, message: str) -> Annotation:
    return Annotation("warning", file, message)


class RepositoryInputError(Exception):
    """A repository file could not be read or parsed, with its path kept for the annotation."""

    def __init__(self, path: str, cause: Exception):
        super().__init__(str(cause))
        self.path = path
        self.cause = cause

    def annotation(self) -> Annotation:
        return error(self.path, f"cannot read repository input: {self.cause}")


def repository_path(path: Path) -> str:
    """The file property for a repository path, relative where that is possible."""
    try:
        return path.relative_to(REPO).as_posix()
    except ValueError:
        return str(path)


def read_repository_text(path: Path) -> str:
    """Read a repository file, retaining its path if the read or decoding fails."""
    try:
        return path.read_text()
    except Exception as exc:
        raise RepositoryInputError(repository_path(path), exc) from exc


def load_repository_yaml(path: Path):
    """Parse a repository YAML file, retaining its path if the parser refuses it. A mapping that
    holds one key twice is refused (UniqueKeyLoader), as GitHub's workflow parser refuses it."""
    try:
        return yaml.load(read_repository_text(path), Loader=UniqueKeyLoader)
    except RepositoryInputError:
        raise
    except yaml.YAMLError as exc:
        raise RepositoryInputError(repository_path(path), exc) from exc


def annotation_line(annotation: Annotation) -> str:
    """The workflow command an annotation is written as, its file and message escaped."""
    location = f" file={escape_property(annotation.file)}" if annotation.file else ""
    return f"::{annotation.severity}{location}::{escape_annotation_message(annotation.message)}"


def print_annotation(annotation: Annotation) -> None:
    print(annotation_line(annotation))


# ---------------------------------------------------------------- reading the workflows
# The prefix of a `uses:` that names a reusable workflow of this repository.
LOCAL_WORKFLOW = "./.github/workflows/"


def called_workflows(document: dict) -> dict:
    """Every reusable workflow of this repository a job of `document` calls, as {its
    repository-relative path: the first job that calls it}, in the order the jobs name them."""
    called: dict = {}
    for job_id, job in (document.get("jobs") or {}).items():
        uses = str((job or {}).get("uses", ""))
        if uses.startswith(LOCAL_WORKFLOW):
            called.setdefault(uses.removeprefix("./"), job_id)
    return called


def workflow_documents(problems: list[Annotation]) -> dict:
    """ci.yaml and every workflow its jobs call, keyed by repository-relative path, ci.yaml first.
    A `uses:` naming a file this repository does not have is a problem of its own."""
    documents = {WORKFLOW: load_repository_yaml(REPO / WORKFLOW) or {}}
    for path, job_id in called_workflows(documents[WORKFLOW]).items():
        if not (REPO / path).is_file():
            problems.append(error(WORKFLOW, f"job {job_id} calls {path}, which is not in the repository, "
                                            f"so the steps it runs are read by nothing here"))
            continue
        documents[path] = load_repository_yaml(REPO / path) or {}
    return documents


def squash(text: str) -> str:
    return " ".join(text.split())


def script_lines(text: str) -> list:
    """The lines of a run: script as bash splits them (at \\n alone), stripped of spaces and tabs,
    comments and blank lines aside."""
    stripped = (line.strip(" \t") for line in text.split("\n"))
    return [line for line in stripped if line and not line.startswith("#")]


def run_default(holder: dict, key: str):
    """The workflow's or a job's defaults.run.<key>, or None."""
    return ((holder.get("defaults") or {}).get("run") or {}).get(key)


def clean_dir(path: str) -> str:
    return path.strip().removeprefix("./").rstrip("/") or "."


def step_workdir(document: dict, job: dict, step: dict) -> str:
    """The directory a run: step runs in: its own working-directory, else the job's or the
    workflow's defaults.run.working-directory, else the repository root."""
    given = (step.get("working-directory") or run_default(job, "working-directory")
             or run_default(document, "working-directory") or ".")
    return clean_dir(str(given))


def repo_files(*patterns: str) -> list:
    """The repository's files matching git pathspecs (`*.sh`, `.github`): tracked ones and
    untracked ones git does not ignore, so an uncommitted new file counts. Outside a git work tree,
    as in a test's fixture, every file under the root that matches."""
    listed = subprocess.run(["git", "ls-files", "--cached", "--others", "--exclude-standard", "--", *patterns],
                            cwd=REPO, capture_output=True, text=True)
    if listed.returncode == 0:
        return sorted(set(listed.stdout.split()))
    found = []
    for current, dirs, names in os.walk(REPO):
        dirs[:] = [d for d in dirs if d not in (".git", ".jj", "node_modules")]
        for name in names:
            path = Path(current, name).relative_to(REPO).as_posix()
            if any(Path(path).match(p) or path.startswith(p.rstrip("/") + "/") for p in patterns):
                found.append(path)
    return sorted(found)


def step_label(step: dict) -> str:
    return str(step.get("name") or step.get("id") or step.get("uses") or str(step.get("run", "")).strip())


def conditions(job: dict) -> list:
    """A job's `if:` and each of its steps', as (the step, or None for the job's, the text)."""
    return [(None, str(job.get("if", "")))] + [(s, str((s or {}).get("if", ""))) for s in job.get("steps") or []]


def where_if(job_id: str, step) -> str:
    """Where such a condition is: the job, and the step too when the condition is a step's."""
    return f"job {job_id}" + (f' step "{step_label(step)}"' if step else "")


def action_file(uses: str) -> str | None:
    """The action.yml of a local composite action a step's `uses: ./<directory>` names, or None."""
    if not uses.startswith("./"):
        return None
    base = uses.removeprefix("./").rstrip("/")
    for name in ("action.yml", "action.yaml"):
        if (REPO / base / name).is_file():
            return f"{base}/{name}"
    return None


_ACTIONS: dict = {}


def load_action(path: str) -> dict:
    if path not in _ACTIONS:
        _ACTIONS[path] = load_repository_yaml(REPO / path) or {}
    return _ACTIONS[path]


def action_steps(uses: str, seen: frozenset = frozenset()) -> list:
    """Every step of the local composite action `uses` names, with those of each local action it
    uses in turn, as (its action file, the step)."""
    path = action_file(uses)
    if path is None or path in seen:
        return []
    found = []
    for step in ((load_action(path).get("runs") or {}).get("steps") or []):
        found.append((path, step or {}))
        if str((step or {}).get("uses", "")).startswith("./"):
            found += action_steps(str(step["uses"]), seen | {path})
    return found


def actions_of_job(job: dict) -> list:
    """Every local action file a job's steps reach, directly or through another action."""
    found = []
    for step in (job or {}).get("steps") or []:
        for path, _ in action_steps(str((step or {}).get("uses", ""))):
            if path not in found:
                found.append(path)
        path = action_file(str((step or {}).get("uses", "")))
        if path and path not in found:
            found.insert(0, path)
    return found


def expression(text: str) -> str:
    """An `if:` or a value as the expression it holds: the text inside `${{ }}` when the whole
    text is one, the text itself otherwise (an `if:` may leave the brackets out)."""
    found = re.fullmatch(r"\s*\$\{\{(.*)\}\}\s*", text, re.S)
    return (found.group(1) if found else text).strip()


GATE_REF = re.compile(r"needs\.changes\.outputs\.([A-Za-z0-9_-]+)")
REPORT_ACTION = "./.github/actions/ci-ok-report"
REPORT_CHANGES = "${{ toJSON(needs.changes) }}"
REPORT_STATUS = "${{ job.status }}"


# ---------------------------------------------------------------- GitHub expressions
# A small reader of GitHub's expression language, for the rules that ask what an `if:` does on an
# event: literals, property paths (`github.event_name`, `matrix.leg`, `labels.*.name`), calls, `!`,
# comparisons, `&&` and `||`. A value it cannot know is UNKNOWN, and `&&` and `||` keep what is
# certain: `false && x` is false whatever x is.
class _Unknown:
    def __repr__(self):
        return "UNKNOWN"


UNKNOWN = _Unknown()
TOKEN = re.compile(r"\s*(?:(?P<str>'(?:[^']|'')*')|(?P<num>-?\d+(?:\.\d+)?)|(?P<op>&&|\|\||==|!=|<=|>=|[<>!(),.\[\]*])"
                   r"|(?P<id>[A-Za-z_][A-Za-z0-9_-]*))")


def tokens(text: str) -> list:
    out, index = [], 0
    while index < len(text):
        if text[index:].strip() == "":
            break
        found = TOKEN.match(text, index)
        if not found:
            raise ValueError(f"cannot read `{text[index:]}`")
        kind = found.lastgroup
        out.append((kind, found.group(kind)))
        index = found.end()
    return out


class Evaluator:
    """Evaluates one expression in a context: a function from a property path (`github.event_name`)
    to its value, or UNKNOWN."""

    def __init__(self, text: str, lookup):
        self.items = tokens(text)
        self.at = 0
        self.lookup = lookup

    def peek(self):
        return self.items[self.at] if self.at < len(self.items) else (None, None)

    def take(self, value=None):
        kind, got = self.peek()
        if value is not None and got != value:
            raise ValueError(f"expected {value}, found {got}")
        self.at += 1
        return kind, got

    def value(self):
        result = self.or_()
        if self.at != len(self.items):
            raise ValueError(f"cannot read past `{self.peek()[1]}`")
        return result

    def or_(self):
        left = self.and_()
        while self.peek()[1] == "||":
            self.take()
            right = self.and_()
            seen = truth(left)
            left = left if seen is True else right if seen is False else (right if truth(right) is True else UNKNOWN)
        return left

    def and_(self):
        left = self.compare()
        while self.peek()[1] == "&&":
            self.take()
            right = self.compare()
            seen = truth(left)
            left = left if seen is False else right if seen is True else (right if truth(right) is False else UNKNOWN)
        return left

    def compare(self):
        left = self.unary()
        while self.peek()[1] in ("==", "!=", "<", ">", "<=", ">="):
            _, op = self.take()
            right = self.unary()
            if left is UNKNOWN or right is UNKNOWN or op not in ("==", "!="):
                left = UNKNOWN
                continue
            same = (str(left).lower() == str(right).lower()) if isinstance(left, str) and isinstance(right, str) \
                else left == right
            left = same if op == "==" else not same
        return left

    def unary(self):
        if self.peek()[1] == "!":
            self.take()
            seen = truth(self.unary())
            return UNKNOWN if seen is UNKNOWN else not seen
        return self.primary()

    def primary(self):
        kind, got = self.take()
        if kind == "str":
            return got[1:-1].replace("''", "'")
        if kind == "num":
            return float(got)
        if got == "(":
            inner = self.or_()
            self.take(")")
            return inner
        if kind != "id":
            raise ValueError(f"cannot read `{got}`")
        if got in ("true", "false"):
            return got == "true"
        if got == "null":
            return None
        if self.peek()[1] == "(":
            self.take()
            args = []
            while self.peek()[1] != ")":
                args.append(self.or_())
                if self.peek()[1] == ",":
                    self.take()
            self.take(")")
            return call(got, args)
        path = got
        while self.peek()[1] in (".", "["):
            if self.take()[1] == ".":
                path += "." + self.take()[1]
            else:
                self.or_()
                self.take("]")
                path += ".[]"
        return self.lookup(path)


def truth(value):
    if value is UNKNOWN:
        return UNKNOWN
    return not (value is None or value is False or value == "" or value == 0)


def call(name: str, args: list):
    name = name.lower()
    if name == "always":
        return True
    if name == "cancelled":
        return False
    if any(a is UNKNOWN for a in args) or name not in ("contains", "startswith", "endswith"):
        return UNKNOWN
    haystack, needle = (args + [None, None])[:2]
    if name == "contains":
        if isinstance(haystack, list):
            return any(str(item).lower() == str(needle).lower() for item in haystack)
        return str(needle).lower() in str(haystack).lower()
    if name == "startswith":
        return str(haystack).lower().startswith(str(needle).lower())
    return str(haystack).lower().endswith(str(needle).lower())


# The runs a gate on the event or a label may skip a test on: a pull request without the full-ci
# label and with it, and a merge group, none of them a draft.
SCENARIOS = (("pull_request", []), ("pull_request", ["full-ci"]), ("merge_group", []))
EVENT_REFERENCE = re.compile(r"github\.event_name|\.labels\b")


def scenario_lookup(event: str, labels: list):
    def lookup(path: str):
        if path == "github.event_name":
            return event
        if path == "github.event.pull_request.draft":
            return False
        if path == "github.event.pull_request.labels.*.name":
            return labels if event == "pull_request" else []
        return UNKNOWN
    return lookup


def skips_on(text: str) -> list:
    """The scenarios on which a condition is certainly false, the event and the labels known and
    everything else not: the runs its gate holds back for what they are."""
    found = []
    for event, labels in SCENARIOS:
        if truth(Evaluator(expression(text), scenario_lookup(event, labels)).value()) is False:
            found.append(event + (" with full-ci" if labels else ""))
    return found


# ---------------------------------------------------------------- ci-ok's name reader
def jq_names(document: dict, judged: list | None = None) -> tuple:
    """ci-ok.jq's readings of a workflow's names: (each job's legs as (job, leg, the name ci-ok.jq
    reads, or None where it reads none), and ci-ok's own map check_run_jobs gives, or the error
    jq stopped on)."""
    program = ('include "ci-ok"; [(.jobs // {}) | to_entries[] | .key as $k | .value as $v '
               '| ($v | matrix_legs[]) as $leg | {job: $k, leg: $leg, named: ($v | has("name")), '
               'name: (if ($v | has("name")) then ($v.name | leg_name($leg)) else null end)}]')
    text = json.dumps(document, default=str)
    legs = subprocess.run(["jq", "-c", "-L", str(REPO / ".github/scripts"), program], input=text,
                          capture_output=True, text=True)
    if legs.returncode != 0:
        raise RepositoryInputError(".github/scripts/ci-ok.jq", RuntimeError(legs.stderr.strip()))
    mapped = None
    if judged is not None:
        done = subprocess.run(["jq", "-c", "-L", str(REPO / ".github/scripts"), "--argjson", "judged",
                               json.dumps(judged), 'include "ci-ok"; check_run_jobs($judged)'],
                              input=text, capture_output=True, text=True)
        mapped = json.loads(done.stdout) if done.returncode == 0 else done.stderr.strip()
    return json.loads(legs.stdout), mapped


def leg_values(leg: dict) -> str:
    return ", ".join(str(v).lower() if isinstance(v, bool) else str(v) for v in leg.values())


def leg_check_names(document: dict) -> list:
    """Each leg of each job of a workflow as (its job, the check-run name it gives): the name
    ci-ok.jq reads, a leg whose job carries no name named as GitHub names it, `<job> (<values>)`,
    and None where the job's name is no form ci-ok.jq reads."""
    legs, _ = jq_names(document)
    out = []
    for entry in legs:
        name = entry["name"]
        if name is None and not entry["named"]:
            name = entry["job"] + (f" ({leg_values(entry['leg'])})" if entry["leg"] else "")
        out.append((entry["job"], name))
    return out


def called_workflow_of(document: dict, job_id: str, documents: dict) -> str | None:
    """The workflow of this repository a job of `document` calls, as a key of `documents`, or None."""
    called = str((((document.get("jobs") or {}).get(job_id)) or {}).get("uses", "")).removeprefix("./")
    return called if called in documents else None


def check_run_names(documents: dict) -> dict:
    """Every check-run name the layout's jobs give, each to where it comes from: a job of ci.yaml
    and each leg of its matrix, and a job of a workflow it calls as `<caller> / <job>`. A leg
    whose job carries no name is named as GitHub names it, `<job> (<values>)`."""
    names: dict = {}
    top = documents[WORKFLOW]
    for job_id, name in leg_check_names(top):
        if job_id in REQUIRED_CHECKS and name is None:
            name = job_id
        called = called_workflow_of(top, job_id, documents)
        if called is not None:
            for inner, inner_name in leg_check_names(documents[called]):
                names[f"{name or job_id} / {inner_name or inner}"] = f"{called} job {inner}"
        elif name is not None:
            names[name] = f"{WORKFLOW} job {job_id}"
    return names


# ---------------------------------------------------------------- rule 2 (half) and rule 19
FILTER_STEP = "dorny/paths-filter"
UNNAMED_STEP_ID = "unnamed"
UNNAMED_OUTPUT = "found"
STEP_OUTPUT_REF = re.compile(r"steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)")


def changes_filters(document: dict, problems: list) -> dict:
    """The filters the changes job's dorny/paths-filter step reads, by name: the file its `filters`
    input names, or the inline text it holds."""
    job = (document.get("jobs") or {}).get("changes") or {}
    names: dict = {}
    for step in job.get("steps") or []:
        if not str((step or {}).get("uses", "")).startswith(FILTER_STEP):
            continue
        given = str((step.get("with") or {}).get("filters", ""))
        if "\n" in given or ":" in given:
            parsed = yaml.safe_load(given) or {}
        elif (REPO / given).is_file():
            parsed = load_repository_yaml(REPO / given) or {}
        else:
            problems.append(error(WORKFLOW, f"the changes job's step {step.get('id')} reads filters from {given}, "
                                            f"which is not in the repository"))
            continue
        names[str(step.get("id", ""))] = set(parsed) if isinstance(parsed, dict) else set()
    return names


def check_gate_outputs(document: dict, problems: list) -> None:
    """Rule 2, its kept half: a job of ci.yaml whose `if:` tests an output the changes job does not
    declare is skipped on every run, since the output is empty, and nothing else says so."""
    outputs = set(((document.get("jobs") or {}).get("changes") or {}).get("outputs") or {})
    for job_id, job in (document.get("jobs") or {}).items():
        for name in sorted(set(GATE_REF.findall(str((job or {}).get("if", ""))))):
            if name not in outputs:
                problems.append(error(WORKFLOW, f"job {job_id}'s if: tests needs.changes.outputs.{name}, which "
                                                f"the changes job does not declare, so the output is empty and the job "
                                                f"is skipped on every run"))


def check_output_steps(document: dict, filters: dict, problems: list) -> None:
    """Rule 19: every `steps.<id>.outputs.<name>` an output of the changes job hands on names the
    job's dorny/paths-filter step and a filter of the file it reads, the unnamed-file step and its
    `found` beside a filter, or the step ref_is_main reads; any other reference is empty on every
    run, which reads as a filter that never matched."""
    job = (document.get("jobs") or {}).get("changes") or {}
    ids = [str(s.get("id", "")) for s in job.get("steps") or [] if s.get("id")]
    for output, value in (job.get("outputs") or {}).items():
        references = STEP_OUTPUT_REF.findall(str(value))
        for step_id, name in references:
            if output == REF_OUTPUT and (step_id, name) == (REF_STEP_ID, "is_main"):
                continue
            if step_id not in ids:
                problems.append(error(WORKFLOW, f"the changes job's output {output} reads steps.{step_id}.outputs."
                                                f"{name}, and the job has no step {step_id}, so the output is empty on "
                                                f"every run"))
            elif step_id == UNNAMED_STEP_ID:
                if name != UNNAMED_OUTPUT:
                    problems.append(error(WORKFLOW, f"the changes job's output {output} reads steps.{step_id}.outputs."
                                                    f"{name}, and the unnamed-file step writes {UNNAMED_OUTPUT} alone"))
                elif output != "unnamed_files" and not any(other in filters for other, _ in references):
                    problems.append(error(WORKFLOW, f"the changes job's output {output} reads steps.{step_id}.outputs."
                                                    f"{name} and no filter, so its job runs only for a file no filter "
                                                    f"names"))
            elif step_id not in filters:
                problems.append(error(WORKFLOW, f"the changes job's output {output} reads steps.{step_id}.outputs."
                                                f"{name}, and step {step_id} is neither its {FILTER_STEP} step, nor the "
                                                f"unnamed-file step, nor the step {REF_OUTPUT} reads, so the output is "
                                                f"empty on every run"))
            elif name not in filters[step_id]:
                problems.append(error(WORKFLOW, f"the changes job's output {output} reads steps.{step_id}.outputs."
                                                f"{name}, and the file that step reads holds no filter {name}, so the "
                                                f"output is empty on every run"))


# ---------------------------------------------------------------- rule 7: ci-ok's classes
ALWAYS_RUN_LIST = "CI_OK_ALWAYS_RUN"
PULL_REQUEST_ONLY_LIST = "CI_OK_PULL_REQUEST_ONLY"
EXEMPT_LIST = "CI_OK_EXEMPT"
SELECTED_BY = "CI_OK_SELECTED_BY"
PULL_REQUEST_TERM = re.compile(r"github\.event_name\s*==\s*'pull_request'")
REQUIRED_CHECKS = ("ci-ok", "pr-title")
VERDICT_JOB = "ci-ok"
REF_OUTPUT = "ref_is_main"
REF_OUTPUT_VALUE = "${{ steps.ref.outputs.is_main }}"
REF_STEP_ID = "ref"
REF_STEP_NAME = "Read whether this run's ref is exactly refs/heads/main"
REF_STEP_RUN = ('if [ "$GITHUB_REF" = refs/heads/main ]; then echo is_main=true; else echo is_main=false; fi '
                '>> "$GITHUB_OUTPUT"')
DISPATCH_OFF_MAIN = f"github.event_name == 'workflow_dispatch' && needs.changes.outputs.{REF_OUTPUT} != 'true'"
DISPATCH_NAMES = {
    "ci-ok": f"${{{{ {DISPATCH_OFF_MAIN} && 'ci-ok (dispatch)' || 'ci-ok' }}}}",
    "pr-title": "${{ github.event_name == 'workflow_dispatch' && 'pr-title (dispatch)' || 'pr-title' }}",
}
# The verdict job's step that holds a failing verdict of a merge group's first attempt until the
# group is the front of the queue, and the one `if:` it carries (HOLD_TERM, after the verdict step
# failed, whatever a later step does); and its step that sets every other ci-ok on the head to its
# conclusion, and the one `if:` that carries: it runs after a failed verdict (always()), on no
# dispatch but one of exactly refs/heads/main, since every other ci-ok on such a head is a pull
# request's or the queue's, and not where the hold step runs (HOLD_TERM): a merge group's first
# attempt whose verdict failed, where every ci-ok posted on the head is failure already and the hold
# must be the job's last step (.github/scripts/ci-ok-hold.sh's header). The job holds each step
# once, so renaming or dropping either is refused rather than passed over, and the one HOLD_TERM
# is what both read.
HOLD_STEP = "Hold a failing verdict until this group is the front of the queue"
RECONCILE_STEP = "Set every other ci-ok on this head to this job's conclusion"
HOLD_TERM = "steps.verdict.outcome == 'failure' && github.event_name == 'merge_group' && github.run_attempt == '1'"
HOLD_IF = f"${{{{ !cancelled() && {HOLD_TERM} }}}}"
RECONCILE_IF = (f"${{{{ always() && (github.event_name != 'workflow_dispatch' || "
                f"needs.changes.outputs.{REF_OUTPUT} == 'true') && !({HOLD_TERM}) }}}}")


def json_env(env: dict, name: str, kind, problems: list):
    try:
        value = json.loads(str(env.get(name, "")))
    except json.JSONDecodeError as json_error:
        problems.append(error(WORKFLOW, f"the env's {name} is missing or not JSON: {json_error}"))
        return None
    if not isinstance(value, kind) or not all(isinstance(v, str) for v in (value.values() if isinstance(value, dict)
                                                                           else value)):
        problems.append(error(WORKFLOW, f"the env's {name} is not a JSON {kind.__name__} of job names"))
        return None
    return value


def check_ci_ok(documents: dict, problems: list) -> None:
    """Rule 7. ci-ok.sh judges each job by its class: a job its `if:` gates on an output of
    changes is selected by that output, the job's own id unless CI_OK_SELECTED_BY names the
    exception; CI_OK_ALWAYS_RUN names the jobs no output gates, which must succeed; CI_OK_EXEMPT
    the jobs outside its accounting. Every job is in exactly one class, every job but changes and
    the exempt ones ends with the report step, ci-ok needs every job but itself and the exempt
    ones, the two checks main's ruleset requires keep their names and pr-title needs no job, and
    ci-ok.jq reads the name of every job ci-ok judges, the carriers' and the legs' included."""
    document = documents[WORKFLOW]
    jobs = document.get("jobs") or {}
    env = document.get("env") or {}
    selected_by = json_env(env, SELECTED_BY, dict, problems)
    always_run = json_env(env, ALWAYS_RUN_LIST, list, problems)
    exempt = json_env(env, EXEMPT_LIST, list, problems)
    pull_request_only = json_env(env, PULL_REQUEST_ONLY_LIST, list, problems)
    if None in (selected_by, always_run, exempt, pull_request_only):
        return
    for name, listed in ((EXEMPT_LIST, exempt), (ALWAYS_RUN_LIST, always_run), (SELECTED_BY, list(selected_by)),
                         (PULL_REQUEST_ONLY_LIST, pull_request_only)):
        for job_id in sorted(set(listed) - set(jobs)):
            problems.append(error(WORKFLOW, f"{name} names {job_id}, which is no job of this file"))
    outputs = {}
    for job_id, job in jobs.items():
        found = sorted(set(GATE_REF.findall(str((job or {}).get("if", "")))) - {REF_OUTPUT})
        if len(found) > 1:
            problems.append(error(WORKFLOW, f"job {job_id}'s if: tests {len(found)} outputs of changes "
                                            f"({', '.join(found)}), and a job is selected by one"))
        if found:
            outputs[job_id] = found[0]
    for job_id in sorted(jobs):
        held = [name for name, yes in ((SELECTED_BY, job_id in outputs), (ALWAYS_RUN_LIST, job_id in always_run),
                                       (EXEMPT_LIST, job_id in exempt)) if yes]
        if not held:
            problems.append(error(WORKFLOW, f"job {job_id} is in none of ci-ok's three classes, so ci-ok.sh fails its "
                                            f"result whatever the job did: gate it on the changes output that selects "
                                            f"it, name it in {ALWAYS_RUN_LIST} when it runs on every change, or name it "
                                            f"in {EXEMPT_LIST} when it is outside ci-ok's accounting"))
        elif len(held) > 1:
            problems.append(error(WORKFLOW, f"job {job_id} is in {len(held)} of ci-ok's three classes "
                                            f"({', '.join(held)}), which judge it differently"))
    for job_id in sorted(set(always_run) & set(outputs)):
        problems.append(error(WORKFLOW, f"{ALWAYS_RUN_LIST} names {job_id}, whose if: tests needs.changes.outputs."
                                        f"{outputs[job_id]}, so ci-ok.sh would fail every run that rightly skipped it; "
                                        f"a job ci-ok holds to success on every run tests no output of changes"))
    for job_id, output in sorted(selected_by.items()):
        if job_id in exempt:
            problems.append(error(WORKFLOW, f"{SELECTED_BY} maps {job_id}, which {EXEMPT_LIST} holds outside ci-ok's "
                                            f"accounting"))
        elif job_id in jobs and outputs.get(job_id) != output:
            problems.append(error(WORKFLOW, f"{SELECTED_BY} maps {job_id} to {output}, but its if: tests "
                                            + (f"needs.changes.outputs.{outputs[job_id]}" if job_id in outputs
                                               else "no output of changes")))
        elif output == job_id:
            problems.append(error(WORKFLOW, f"{SELECTED_BY} maps {job_id} to its own id, which ci-ok.jq reads without "
                                            f"an entry: the map holds the exceptions alone"))
    for job_id, output in sorted(outputs.items()):
        if job_id not in selected_by and output != job_id and job_id not in exempt:
            problems.append(error(WORKFLOW, f"job {job_id}'s if: tests needs.changes.outputs.{output}, which is not its "
                                            f"own id, and {SELECTED_BY} does not map it, so ci-ok.sh reads it as selected "
                                            f"by an output changes does not have; add \"{job_id}\": \"{output}\""))
    want_pr = {job_id for job_id, job in jobs.items() if job_id in outputs
               and PULL_REQUEST_TERM.search(str((job or {}).get("if", "")))}
    for job_id in sorted(want_pr - set(pull_request_only)):
        problems.append(error(WORKFLOW, f"{PULL_REQUEST_ONLY_LIST} lacks {job_id}, whose if: admits only "
                                        f"pull_request, so ci-ok.sh fails its skip on every other event"))
    for job_id in sorted(set(pull_request_only) - want_pr):
        problems.append(error(WORKFLOW, f"{PULL_REQUEST_ONLY_LIST} lists {job_id}, whose if: has no "
                                        f"github.event_name == 'pull_request' term, so ci-ok.sh accepts its skip on "
                                        f"every other event even when changes selected it"))
    for job_id in REQUIRED_CHECKS:
        job = jobs.get(job_id)
        if job is None:
            problems.append(error(WORKFLOW, f"this file has no {job_id} job, a check main's ruleset requires"))
            continue
        if str(job.get("name", job_id)) != DISPATCH_NAMES[job_id]:
            problems.append(error(WORKFLOW, f"job {job_id} must carry `name: {DISPATCH_NAMES[job_id]}`, so a dispatch "
                                            f"of any ref but exactly refs/heads/main reports under a name of its own"))
    if (jobs.get("pr-title") or {}).get("needs"):
        problems.append(error(WORKFLOW, "job pr-title needs a job: it must need none, so its check runs on every pull "
                                        "request and merge group whatever another job concluded"))
    for job_id, job in jobs.items():
        if job_id in REQUIRED_CHECKS:
            continue
        named = str((job or {}).get("name", job_id))
        for check in sorted(set(REQUIRED_CHECKS) & ({named} | set(re.findall(r"'([^']*)'", named)))):
            problems.append(error(WORKFLOW, f"job {job_id} carries a name that can be {check}, a check main's ruleset "
                                            f"requires, and is not the {check} job"))
    verdict = jobs.get(VERDICT_JOB) or {}
    for name, condition in ((HOLD_STEP, HOLD_IF), (RECONCILE_STEP, RECONCILE_IF)):
        held = [step for step in verdict.get("steps") or [] if (step or {}).get("name") == name]
        if len(held) != 1:
            problems.append(error(WORKFLOW, f"job {VERDICT_JOB} holds {len(held)} step(s) named \"{name}\", where it "
                                            f"holds one, with `if: {condition}`"))
        elif str(held[0].get("if", "")) != condition:
            problems.append(error(WORKFLOW, f"job {VERDICT_JOB}'s step \"{name}\" must carry `if: {condition}`"))
    changes = jobs.get("changes") or {}
    if str((changes.get("outputs") or {}).get(REF_OUTPUT)) != REF_OUTPUT_VALUE:
        problems.append(error(WORKFLOW, f"the changes job must carry the output `{REF_OUTPUT}: {REF_OUTPUT_VALUE}`"))
    ref_steps = [s for s in changes.get("steps") or [] if (s or {}).get("id") == REF_STEP_ID]
    if not (len(ref_steps) == 1 and ref_steps[0].get("name") == REF_STEP_NAME and "if" not in ref_steps[0]
            and str(ref_steps[0].get("run", "")).strip() == REF_STEP_RUN):
        problems.append(error(WORKFLOW, f"the changes job must hold exactly one step with id {REF_STEP_ID}, named "
                                        f"\"{REF_STEP_NAME}\", with no if: and `run: {REF_STEP_RUN}`"))
    needed = verdict.get("needs") or []
    needed = [needed] if isinstance(needed, str) else [str(j) for j in needed]
    want = set(jobs) - {VERDICT_JOB} - set(exempt)
    if VERDICT_JOB not in exempt:
        problems.append(error(WORKFLOW, f"{EXEMPT_LIST} does not name {VERDICT_JOB}, which reports no verdict to "
                                        f"itself"))
    for job_id in sorted(want - set(needed)):
        problems.append(error(WORKFLOW, f"job {VERDICT_JOB} does not need {job_id}, so its verdict says nothing about "
                                        f"it; add {job_id} to its needs:"))
    for job_id in sorted(set(needed) - want):
        problems.append(error(WORKFLOW, f"job {VERDICT_JOB} needs {job_id}, which is "
                                        + ("outside ci-ok's accounting" if job_id in exempt else "no job of this file")))
    for job_id, job in jobs.items():
        job = job or {}
        if job_id == "changes":
            continue
        reports = any((s or {}).get("uses") == REPORT_ACTION for s in job.get("steps") or []) \
            or bool((job.get("with") or {}).get("ci-ok-changes"))
        if job_id in exempt:
            if reports:
                problems.append(error(WORKFLOW, f"job {job_id} is in {EXEMPT_LIST}, yet it reports to ci-ok, which "
                                                f"fails a result for a job it does not judge"))
            continue
        if "uses" in job:
            if str((job.get("with") or {}).get("ci-ok-changes", "")) != REPORT_CHANGES:
                problems.append(error(WORKFLOW, f"job {job_id} calls {job['uses']} without `ci-ok-changes: "
                                                f"{REPORT_CHANGES}`, so the called job cannot report to ci-ok"))
            called = (documents.get(str(job["uses"]).removeprefix("./")) or {}).get("jobs") or {}
            lasts = [((j or {}).get("steps") or [])[-1] for j in called.values() if (j or {}).get("steps")]
            # A file two jobs call (dispatch-browser.yaml) reports for the job its `job` input names.
            reported = {job_id} | ({"${{ inputs.job }}"} if str((job.get("with") or {}).get("job", "")) == job_id
                                    else set())
            if not any((s or {}).get("uses") == REPORT_ACTION and str(s.get("if", "")).startswith("always()")
                       and str((s.get("with") or {}).get("job", "")) in reported
                       and str((s.get("with") or {}).get("changes", "")) == "${{ inputs.ci-ok-changes }}"
                       and str((s.get("with") or {}).get("job-status", "")) == REPORT_STATUS for s in lasts):
                problems.append(error(WORKFLOW, f"{job['uses']}, which job {job_id} calls, has no job whose last step "
                                                f"reports to ci-ok for {job_id}"))
            continue
        steps = job.get("steps") or []
        last = steps[-1] if steps else {}
        given = (last or {}).get("with") or {}
        if ((last or {}).get("uses") != REPORT_ACTION or str(last.get("if", "")) != "always()"
                or str(given.get("changes", "")) != REPORT_CHANGES or str(given.get("job-status", "")) != REPORT_STATUS):
            problems.append(error(WORKFLOW, f"job {job_id}'s last step is not `uses: {REPORT_ACTION}` with `if: "
                                            f"always()`, `changes: {REPORT_CHANGES}` and `job-status: {REPORT_STATUS}`, "
                                            f"so a merge-group run {job_id}'s own result decides gets no failing ci-ok "
                                            f"until the {VERDICT_JOB} job has waited for every other job"))
    _, mapped = jq_names(document, needed)
    if isinstance(mapped, str):
        problems.append(error(WORKFLOW, f"ci-ok.jq cannot read the check-run names of the jobs ci-ok judges: {mapped}"))


# ---------------------------------------------------------------- rule 22: split lists
NAME_LIST = re.compile(r"^\^\(?([A-Za-z_][A-Za-z0-9_]*(?:\|[A-Za-z_][A-Za-z0-9_]*)*)\)?\$$")


def top_level(pattern: str) -> str:
    """The first element of a -run or -skip pattern, the part that names root tests: go splits a
    pattern at each `/` outside brackets and parentheses."""
    depth = 0
    for index, char in enumerate(pattern):
        if char in "([":
            depth += 1
        elif char in ")]":
            depth -= 1
        elif char == "/" and depth == 0:
            return pattern[:index]
    return pattern


def named_tests(pattern: str) -> list | None:
    """The root tests an anchored -run or -skip list names (`^(A|B)$`, `^A$`), or None for any
    other pattern."""
    found = NAME_LIST.match(top_level(pattern.strip("'\"")))
    return found.group(1).split("|") if found else None


def pattern_selects(pattern: str, test: str):
    """Whether a -run pattern selects a root test: True, False, or None where it cannot be read."""
    if not pattern:
        return True
    try:
        return re.search(top_level(pattern.strip("'\"")), test) is not None
    except re.error:
        return None


def module_root(directory: Path) -> Path | None:
    for candidate in (directory, *directory.parents):
        if (candidate / "go.mod").is_file():
            return candidate
        if candidate == REPO:
            return None
    return None


def module_path(root: Path) -> str:
    for line in read_repository_text(root / "go.mod").splitlines():
        if line.startswith("module "):
            return line.split()[1]
    return ""


@functools.cache
def package_dirs(workdir: str, pattern: str) -> list:
    """The repository directories a package argument names, from the step's directory: a relative
    path, a `/...` pattern walked as go walks it, or an import path of the module."""
    base = REPO / workdir
    root = module_root(base)
    if not pattern.startswith((".", "/")) and root is not None:
        prefix = module_path(root)
        if pattern == prefix or pattern.startswith(prefix + "/"):
            pattern = "./" + pattern[len(prefix):].lstrip("/") if pattern != prefix else "."
            base = root
        else:
            return []
    if pattern.endswith("/...") or pattern == "./..." or pattern == "...":
        top = (base / pattern.removesuffix("...").rstrip("/")).resolve()
        if not top.is_dir():
            return []
        return sorted({repository_path(path.parent) for path in go_files(top)})
    return [repository_path((base / pattern).resolve())]


def static_answer(step, text: str) -> str:
    """The answer go_test_steps asks for, read off the tree: what go-tagged-tests.sh prints for
    a tag and a directory, and the import paths `go list … | grep …` prints."""
    import go_test_steps as steps

    if steps.TAGGED_TESTS in text:
        words = shlex.split(text)
        at = next(i for i, word in enumerate(words) if word.endswith("go-tagged-tests.sh"))
        tag, directory = words[at + 1], REPO / step.workdir / words[at + 2]
        names = set()
        for path in sorted(directory.glob("*_test.go")):
            source = path.read_text(encoding="utf-8", errors="replace")
            if re.search(rf"^//go:build {re.escape(tag)}\s*$", source, re.M):
                names |= set(re.findall(r"^func ((?:Test|Fuzz|Example)\w*)\(", source, re.M))
        return "^(" + "|".join(sorted(names)) + ")$"
    stages = [stage.strip() for stage in re.findall(r"(?:'[^']*'|\"[^\"]*\"|[^|'\"])+", text)]
    words = shlex.split(stages[0])
    if words[:2] != ["go", "list"]:
        raise steps.Refused(f"{step.label}: `{text}` is no package lister this rule reads")
    root = module_root(REPO / step.workdir)
    prefix = module_path(root) if root else ""
    paths = []
    for argument in [w for w in words[2:] if not w.startswith("-")] or ["."]:
        for directory in package_dirs(step.workdir, argument):
            relative = Path(REPO / directory).resolve().relative_to(root.resolve()).as_posix() if root else directory
            paths.append(prefix if relative == "." else f"{prefix}/{relative}")
    for stage in stages[1:]:
        grep = re.match(r"^grep\s+-([A-Za-z]+)\s+(.+)$", stage)
        if not grep:
            raise steps.Refused(f"{step.label}: `{stage}` is no filter this rule reads")
        pattern = shlex.split(grep.group(2))[0]
        keep = "v" not in grep.group(1)
        paths = [p for p in paths if (re.search(pattern, p) is not None) == keep]
    return "\n".join(dict.fromkeys(paths))


@dataclass
class GoRun:
    """One `go test` command a job runs, as the rules below read it."""

    job: str
    where: str
    dirs: list
    run: str
    skip: str
    split: bool
    command: object


def go_runs(problems: list) -> list:
    """Every `go test` command of the layout, read by go_test_steps through matrix legs, composite
    actions, called workflows and the scripts a step runs, with where it is and the directories
    of the packages it runs."""
    import go_test_steps as steps

    runs = []
    try:
        found = steps.documents()
        read = steps.steps_of(found)
    except steps.Refused as refused:
        problems.append(error(WORKFLOW, f"the step reader refuses the workflows: {refused}"))
        return runs
    for step in read:
        try:
            commands = steps.commands_of(step, static_answer)
        except steps.Refused as refused:
            problems.append(error(step.workflow, f"the step reader cannot read {step.label}: {refused}"))
            continue
        for command in commands:
            dirs = sorted({d for p in command.packages for d in package_dirs(step.workdir, p)})
            runs.append(GoRun(step.job, step.label, dirs, command.run, command.skip, command.split, command))
    return runs


BUILD_LINE = re.compile(r"^//go:build (.+)$")
GO_FUNC = "^func {}\\("
# The names a build constraint is true for on a Linux runner, beside a command's own -tags and
# -race: the platform, the families GOOS sets, cgo and the gc toolchain.
RUNNER_NAMES = frozenset({"linux", "unix", "cgo", "gc", "amd64", "arm64"})


def compiled_test(directory: str, test: str, tags: set) -> bool | None:
    """Whether a command whose build is true for `tags` compiles the test function `test` of a
    package directory: None where no test file of it declares the function, else whether the
    `//go:build` line of the file that does holds under those tags."""
    for path in sorted((REPO / directory).glob("*_test.go")):
        source = path.read_text(encoding="utf-8", errors="replace")
        if not re.search(GO_FUNC.format(re.escape(test)), source, re.M):
            continue
        constraint = next((m.group(1) for m in map(BUILD_LINE.match, source.splitlines()) if m), "")
        if not constraint:
            return True
        python = re.sub(r"[A-Za-z0-9_.]+", lambda m: str(m.group(0) in tags), constraint)
        python = python.replace("&&", " and ").replace("||", " or ").replace("!", " not ")
        try:
            return bool(eval(python, {"__builtins__": {}}, {}))
        except SyntaxError:
            return True
    return None


def build_names(command) -> set:
    return set(RUNNER_NAMES) | set(re.split(r"[ ,]+", command.tags or "")) - {""} | ({"race"} if command.race else set())


def check_split_lists(runs: list, problems: list) -> None:
    """Rule 22: where one job's -run list names a test of a package and another job's -skip list
    of that package names one of the same tests, the two lists split the package between the two
    jobs, and the -skip list names every test of the -run list that its command compiles: a name
    it loses runs the test in both jobs. Lists that share no name split nothing - a test one job
    runs by name may run again elsewhere under -race, or under the Lean model, on purpose - and a
    test a -skip list names that no job runs is the search's (check-test-reach.sh) to name."""
    for first in runs:
        names = named_tests(first.run) if first.run and not first.split else None
        if not names:
            continue
        for second in runs:
            shared = sorted(set(second.dirs) & set(first.dirs))
            skipped = named_tests(second.skip) if second.skip else None
            if second.job == first.job or not shared or not skipped or not set(skipped) & set(names):
                continue
            tags = build_names(second.command)
            for test in names:
                if test in skipped:
                    continue
                if second.run and not second.split and pattern_selects(second.run, test) is False:
                    continue
                held = [d for d in shared if compiled_test(d, test, tags)]
                if held:
                    problems.append(error(WORKFLOW, f"job {first.job} runs {test} of {held[0]} by name "
                                                    f"({first.where}), and job {second.job}'s -skip list of that package "
                                                    f"names the other tests of that -run list and not {test} "
                                                    f"({second.where}), so it runs in both jobs: name it in that -skip "
                                                    f"list"))


# ---------------------------------------------------------------- rule 24: the retry
GO_TEST_SCRIPT = "go-test.sh"
JSONFILE = re.compile(r"--jsonfile[ =]\S")
RETRY_OFF = re.compile(r"\bGO_TEST_RETRY=[\"']?off\b")
NOT_A_RUN = re.compile(r"(?<![\w-])-(?:list|c)\b|-run[ =]['\"]?\^\$")


def check_retry(runs: list, pins, problems: list) -> None:
    """Rule 24: every `go test` command that runs tests goes through .github/scripts/go-test.sh,
    which reruns a failed root test once on a merge group (--rerun-fails-run-root-test) and reads
    the jsonfile its --jsonfile names after gotestsum ends; and none turns that retry off, but
    kind's runtime legs, whose -run selects one group of LIVE_GROUPS' test, and the script whose
    runs against a mutant take none (NO_RETRY_SCRIPT, which turns it off once for every run)."""
    live = pins.LIVE_GROUPS
    from go_test_steps import AUDITED_SCRIPTS

    for run in runs:
        command = run.command
        text = command.text
        if NOT_A_RUN.search(text):
            continue
        audited = next((path for path, entry in AUDITED_SCRIPTS.items()
                        if any(text in body for body in entry.commands.values())), None)
        if audited is not None:
            # A script a person read (go_test_steps.AUDITED_SCRIPTS) holds its own go-test.sh call.
            lines = script_lines(read_repository_text(REPO / audited)) if (REPO / audited).is_file() else []
            if not any(GO_TEST_SCRIPT in line and JSONFILE.search(line) for line in lines):
                problems.append(error(audited, f"{audited}, which {run.where} runs, runs its tests with no "
                                               f".github/scripts/go-test.sh call that names a --jsonfile, so a merge "
                                               f"group gives a failed test no retry and nothing reads the jsonfile"))
        elif GO_TEST_SCRIPT not in text:
            problems.append(error(command.step.workflow, f"{run.where} runs `{squash(text)[:160]}` outside "
                                                         f".github/scripts/go-test.sh, so a merge group gives a failed "
                                                         f"test no retry and nothing reads its jsonfile"))
            continue
        elif not JSONFILE.search(text):
            problems.append(error(command.step.workflow, f"{run.where} runs go-test.sh with no --jsonfile, so nothing "
                                                         f"reads the jsonfile after gotestsum ends and a package that "
                                                         f"failed with no test failing passes"))
        off = RETRY_OFF.search(" ".join(f"{k}={v}" for k, v in {**command.step.env, **command.env}.items()))
        runtime = run.job == live["job"] and top_level(command.run.strip("'\"")).strip("^$") == live["test"]
        if off and not runtime:
            problems.append(error(command.step.workflow, f"{run.where} sets GO_TEST_RETRY=off, so a merge group "
                                                         f"retries none of its tests: only kind's runtime legs and "
                                                         f"{pins.NO_RETRY_SCRIPT}'s runs against a mutant take no retry"))
        if runtime and not off:
            problems.append(error(command.step.workflow, f"{run.where} runs one group of {live['test']} and takes the "
                                                         f"retry, whose rerun of the root test runs every group"))
    script = REPO / pins.NO_RETRY_SCRIPT
    if not script.is_file() or "export GO_TEST_RETRY=off" not in script_lines(read_repository_text(script)):
        problems.append(error(pins.NO_RETRY_SCRIPT, f"{pins.NO_RETRY_SCRIPT} does not `export GO_TEST_RETRY=off`, so "
                                                    f"its runs against a mutant, which must fail, take the retry"))
    for path in repo_files("*.sh"):
        if path == pins.NO_RETRY_SCRIPT or path.endswith(".test.sh") or path.endswith("/go-test.sh"):
            continue
        if (REPO / path).is_file() and any(RETRY_OFF.search(line) for line in
                                           script_lines((REPO / path).read_text(errors="replace"))):
            problems.append(error(path, f"{path} sets GO_TEST_RETRY=off, so the go tests it runs take no retry on a "
                                        f"merge group"))


# ---------------------------------------------------------------- rule 27: liveGroups
LIVE_GROUP = re.compile(r"^\t\{name: \"([^\"]+)\"", re.M)


def check_live_groups(document: dict, pins, problems: list) -> None:
    """Rule 27: each group of liveGroups is in exactly one kind leg's -run, so each runs once, on
    a leg the matrix runs."""
    live = pins.LIVE_GROUPS
    source = read_repository_text(REPO / live["file"]) if (REPO / live["file"]).is_file() else ""
    body = re.search(r"^var liveGroups = \[\]liveGroup\{\n(.*?)^\}", source, re.M | re.S)
    if body is None:
        problems.append(error(live["file"], f"{live['file']} declares no liveGroups, which kind's legs split by "
                                            f"name"))
        return
    groups = LIVE_GROUP.findall(body.group(1))
    legs = {str(leg.get("leg", "")) for leg in _python_legs((document.get("jobs") or {}).get(live["job"]) or {})}
    runs: dict = defaultdict(list)
    for step in ((load_action(live["action"]).get("runs") or {}).get("steps") or []):
        leg = re.fullmatch(r"\s*inputs\.leg\s*==\s*'([^']+)'\s*", expression(str((step or {}).get("if", ""))))
        found = re.search(rf"-run '\^{re.escape(live['test'])}\$/\^\(?([^)'$]+)\)?\$'", str(step.get("run", "")))
        if found and leg and leg.group(1) in legs:
            for group in found.group(1).split("|"):
                runs[group].append(leg.group(1))
    for group in groups:
        if len(runs.get(group, [])) != 1:
            problems.append(error(live["action"], f"liveGroups entry {group} of {live['file']} is in "
                                                  f"{len(runs.get(group, []))} {live['job']} legs' -run "
                                                  f"({', '.join(runs.get(group, [])) or 'none'}), and must be in "
                                                  f"exactly one"))
    for group in sorted(set(runs) - set(groups)):
        problems.append(error(live["action"], f"a {live['job']} leg runs group {group}, which liveGroups of "
                                              f"{live['file']} does not declare, so its -run selects nothing"))


def _python_legs(job: dict) -> list:
    import go_test_steps as steps

    return steps.matrix_legs(job)


# ---------------------------------------------------------------- rule 23: changes outputs
def check_changes_tests(documents: dict, problems: list) -> None:
    """Rule 23: a job's own `if:` is the one place an output of changes selects anything, beside
    the report step at its end. A step, a leg, an input of a called workflow or an action that
    tests one would skip a test a selected job owes, so each other place that names
    needs.changes.outputs fails, ci-ok's own steps aside."""
    for job_id, job in (documents[WORKFLOW].get("jobs") or {}).items():
        if job_id in ("changes", VERDICT_JOB):
            continue
        job = dict(job or {})
        job.pop("if", None)
        job.pop("needs", None)
        if str((job.get("with") or {}).get("ci-ok-changes", "")) == REPORT_CHANGES:
            job["with"] = {k: v for k, v in job["with"].items() if k != "ci-ok-changes"}
        job["steps"] = [s for s in job.get("steps") or [] if (s or {}).get("uses") != REPORT_ACTION]
        for index, step in enumerate(job["steps"]):
            found = GATE_REF.search(json.dumps(step, default=str)) or re.search(r"toJSON\(needs\.changes\)",
                                                                               json.dumps(step, default=str))
            if found:
                problems.append(error(WORKFLOW, f"job {job_id} step \"{step_label(step or {}) or index + 1}\" tests "
                                                f"`{found.group(0)}`: only the job's own if: and its report step may, "
                                                f"since a selected job runs every leg and every step"))
        rest = {k: v for k, v in job.items() if k != "steps"}
        found = GATE_REF.search(json.dumps(rest, default=str))
        if found:
            problems.append(error(WORKFLOW, f"job {job_id} names `{found.group(0)}` outside its if: (in its matrix, "
                                            f"env, name or the inputs of the workflow it calls): only the job's own "
                                            f"if: and its report step may test an output of changes"))


# ---------------------------------------------------------------- rule 28: event and label gates
def is_remote(step: dict) -> bool:
    uses = str((step or {}).get("uses", ""))
    return bool(uses) and not uses.startswith("./")


def gate_places(documents: dict) -> list:
    """Every condition of the layout that can skip a job, a leg, a step or an inner job: (the file,
    the job or None for an action's step, the step's name or None for a job's own if:, the text).
    Each local action a step reaches is read once, its steps under its own file; a step that uses
    another repository's action runs no test or check of this repository and is passed over."""
    places, actions = [], []
    for path, document in documents.items():
        for job_id, job in (document.get("jobs") or {}).items():
            job = job or {}
            if job.get("if") is not None:
                places.append((path, job_id, None, str(job["if"])))
            matrix = (job.get("strategy") or {}).get("matrix")
            if isinstance(matrix, dict):
                for key in ("exclude", "include"):
                    for entry in matrix.get(key) or []:
                        for value in (entry or {}).values() if isinstance(entry, dict) else []:
                            if "${{" in str(value):
                                places.append((path, job_id, f"strategy.matrix.{key}", expression(str(value))))
            for step in job.get("steps") or []:
                if (step or {}).get("if") is not None and not is_remote(step) and step.get("uses") != REPORT_ACTION:
                    places.append((path, job_id, step_label(step), str(step["if"])))
            for action in actions_of_job(job):
                if action not in actions:
                    actions.append(action)
    for action in actions:
        for step in ((load_action(action).get("runs") or {}).get("steps") or []):
            if (step or {}).get("if") is not None and not is_remote(step) and step.get("uses") != REPORT_ACTION:
                places.append((action, None, step_label(step), str(step["if"])))
    return places


def check_event_gates(documents: dict, pins, problems: list) -> None:
    """Rule 28: on a pull request and a merge group, no gate on the event or a label skips a job, a
    leg, a step or an inner job that runs a test or a check, but those ci_pins' EVENT_GATES
    allows. A condition that names the event or a label and is certainly false on one of
    those runs is such a gate; one that keeps a step to the events it serves, true on both, is
    not."""
    allowed = [(path, job, step, squash(term)) for path, job, step, term in pins.EVENT_GATES]
    for path, job, step, text in gate_places(documents):
        if not EVENT_REFERENCE.search(text):
            continue
        try:
            skipped = skips_on(text)
        except ValueError as unreadable:
            problems.append(error(path, f"{'job ' + job if job else path} {('step ' + repr(step)) if step else ''} "
                                        f"has `{squash(text)}`, which this rule cannot read: {unreadable}"))
            continue
        if not skipped:
            continue
        if any(p == path and j == job and s == step and term in squash(text) for p, j, s, term in allowed):
            continue
        where = (f"job {job}" if job else path) + (f" {step}" if step and step.startswith("strategy.")
                                                   else f' step "{step}"' if step else "")
        problems.append(error(path, f"{where} has `{squash(text)}`, which skips it on {' and on '.join(skipped)} for "
                                    f"the event or a label: the layout lets no such gate skip a test or a check but "
                                    f"those ci_pins.EVENT_GATES names, so select the job by its changes output instead"))


# ---------------------------------------------------------------- rule 25: expression names
CANNOT_SKIP = re.compile(r"^\s*(?:\$\{\{)?\s*(?:always\(\)|!\s*cancelled\(\))\s*(?:\}\})?\s*$")
NAME_EXPRESSION = re.compile(r"\$\{\{(.*?)\}\}", re.S)
STRING_LITERAL = re.compile(r"'(?:[^']|'')*'")
# A name an expression reads outside its string literals that is neither a function's (followed
# by `(`) nor a literal word: a context, such as github, matrix or needs.
CONTEXT_NAME = re.compile(r"(?<![\w.-])[A-Za-z_][\w-]*(?![\w-]|\s*\()")
LITERAL_WORDS = {"true", "false", "null", "NaN", "Infinity"}


def reads_context(name: str) -> bool:
    """Whether an expression of a `name:` reads a context, rather than string literals, operators
    and functions of them alone."""
    return any(word not in LITERAL_WORDS
               for found in NAME_EXPRESSION.finditer(name)
               for word in CONTEXT_NAME.findall(STRING_LITERAL.sub("''", found.group(1))))


def check_expression_names(documents: dict, pins, problems: list) -> None:
    """Rule 25: every job of ci.yaml, and every job of a workflow it calls, that a run can skip
    carries a `name:` that is an expression reading a context, so its skipped record carries the
    expression's text rather than a name a reader takes for a check that ran. GitHub evaluates a
    name that reads no context even for a job it skips: pull request run 45440587594 wrote check
    run 138611450495, skipped, as `kind / image` for `${{ 'kind / image' }}`, while pr-title's
    name, which reads github.event_name, left its own text on check run 138610203753.

    A job of ci.yaml that calls a reusable workflow of this repository is the exception: GitHub
    writes its skipped record under the name it evaluates, although the name reads a context. Run
    45523492636 wrote job 138901138215, skipped, as `worker-image` for
    `${{ github.event_name && 'worker-image' }}`, and job 138901137695 as `dispatch-browser-pr` for
    its sibling's name of the same form. The text cannot keep such a record apart from a check
    that ran, so the rule holds the evaluated name instead: the name ci-ok.jq reads for each leg of
    a caller is no check-run name the layout writes (check_run_names, which holds each called
    job's `<caller> / <job>`) and none a check reads (ci_pins.MAIN_CHECK_RUN_NAMES, the names rule
    30 keeps for other readers). A skipped caller then writes a name that no reader takes for a
    check that ran."""
    for path, document in documents.items():
        for job_id, job in (document.get("jobs") or {}).items():
            condition = (job or {}).get("if")
            if condition is None or CANNOT_SKIP.match(str(condition)):
                continue
            name = str((job or {}).get("name", ""))
            if "${{" in name and reads_context(name):
                continue
            problems.append(error(path, f"job {job_id} of {path} has an if: that can skip it and "
                                        + ("no name:" if "name" not in (job or {}) else
                                           f"the literal name `{name}`" if "${{" not in name else
                                           f"the name `{name}`, an expression that reads no context, which "
                                           f"GitHub evaluates even for a skipped job")
                                        + ", so its skipped record carries a plain name; give it a name that is an "
                                          f"expression reading a context, such as "
                                          f"`${{{{ github.event_name && '{job_id}' }}}}`"))
    top = documents[WORKFLOW]
    written = check_run_names(documents)
    read = set(pins.MAIN_CHECK_RUN_NAMES)
    for job_id, name in leg_check_names(top):
        called = called_workflow_of(top, job_id, documents)
        if called is None:
            continue
        if name is None:
            text = (((top.get("jobs") or {}).get(job_id)) or {}).get("name")
            problems.append(error(WORKFLOW, f"job {job_id} of {WORKFLOW} calls {called}, and its name `{text}` is no "
                                            "form ci-ok.jq reads, so this check cannot tell the name GitHub writes "
                                            "for its skipped record"))
            continue
        holders = ([f"{written[name]} writes"] if name in written else []) + (
            ["a check reads (ci_pins.MAIN_CHECK_RUN_NAMES)"] if name in read else [])
        if holders:
            problems.append(error(WORKFLOW, f"job {job_id} of {WORKFLOW} calls {called}, so GitHub writes its skipped "
                                            f"record under its evaluated name `{name}`, which is the check-run name "
                                            f"`{name}` that {' and that '.join(holders)}; a reader takes that skipped "
                                            "record for the check, so give the job a name that no called job writes "
                                            "and no check reads"))


# ---------------------------------------------------------------- rule 26: runners
def check_runners(document: dict, pins, problems: list) -> None:
    """Rule 26: a job whose runner is part of what it proves runs there."""
    for job_id, runner in pins.ARM_JOBS.items():
        have = str(((document.get("jobs") or {}).get(job_id) or {}).get("runs-on", ""))
        if have != runner:
            problems.append(error(WORKFLOW, f"job {job_id} runs on `{have or '(no runner)'}`, and what it proves is "
                                            f"proved on {runner}"))


# ---------------------------------------------------------------- rule 29: fail-fast
def check_fail_fast(documents: dict, problems: list) -> None:
    """Rule 29: every matrix sets fail-fast: false, so one failed leg cancels no other leg's tests."""
    for path, document in documents.items():
        for job_id, job in (document.get("jobs") or {}).items():
            strategy = (job or {}).get("strategy") or {}
            if "matrix" in strategy and strategy.get("fail-fast") is not False:
                problems.append(error(path, f"job {job_id}'s matrix does not set fail-fast: false, so one failed leg "
                                            f"cancels the tests of every other leg"))


# ---------------------------------------------------------------- rule 30: main's names
def check_main_names(documents: dict, pins, problems: list) -> list:
    """Rule 30: each check-run name of ci.yaml at main but ci-filters / ci-filters is still a name
    of the layout, so a reader of a name (another issue's check, a ruleset) still finds it."""
    names = check_run_names(documents)
    for name in pins.MAIN_CHECK_RUN_NAMES:
        if name not in names and not any(other.startswith(f"{name} (") for other in names):
            problems.append(error(WORKFLOW, f"no job, leg or called job of the layout carries the check-run name "
                                            f"`{name}`, which ci.yaml carried at main; give the job or leg that runs "
                                            f"its tests that name"))
    return [f"check-run names: {len(names)} in the layout, {len(pins.MAIN_CHECK_RUN_NAMES)} of main's held"]


# ---------------------------------------------------------------- rule 31: timed steps
def check_timed_steps(documents: dict, pins, problems: list) -> None:
    """Rule 31: each timed step of main's ci-job files, and of the jobs the layout added after them,
    keeps its own bound: a uses: step of ci.yaml its own timeout-minutes, a run: body moved into an
    action a `timeout <minutes>m` command in its body; and there are as many of each as there were."""
    for path, job_id, picks, minutes, count in pins.TIMED_STEPS:
        if job_id is not None:
            steps = (((documents.get(path) or {}).get("jobs") or {}).get(job_id) or {}).get("steps") or []
        else:
            steps = (load_action(path).get("runs") or {}).get("steps") or [] if (REPO / path).is_file() else []
        matching = [s for s in steps if all(str((s or {}).get(k, "")).split("@")[0] == v for k, v in picks.items())]
        bounded = [s for s in matching
                   if (job_id is not None and str(s.get("timeout-minutes", "")) == str(minutes))
                   or (job_id is None and re.search(rf"(?<![\w-])timeout {minutes}m ", str(s.get("run", ""))))]
        if len(bounded) != count:
            what = ", ".join(f"{k} {v}" for k, v in picks.items())
            problems.append(error(path, f"{'job ' + job_id + ' of ' if job_id else ''}{path} holds {len(bounded)} "
                                        f"step(s) with {what} bound to {minutes} minutes, where {count} kept that bound "
                                        f"at main: " + ("a timed uses: step keeps its own timeout-minutes" if job_id
                                                        else "a timed run: body keeps a `timeout` command in its body")))


# ---------------------------------------------------------------- rule 32: the carriers
LEG_VERDICT = ".github/scripts/leg-verdict.sh"
SPLIT_LEG = re.compile(r"^(?P<base>.+) \((?P<part>\d+)/(?P<legs>\d+)\)$")


def is_carrier(job: dict) -> bool:
    return any(LEG_VERDICT in str((s or {}).get("run", "")) for s in (job or {}).get("steps") or [])


def split_legs(legs: list) -> dict:
    """The legs of each split job, by (job, base): each leg jq_names reads whose name is
    `<base> (<i>/<n>)`, as (i, n, its jq_names entry)."""
    splits: dict = defaultdict(list)
    for entry in legs:
        found = SPLIT_LEG.match(str(entry["name"] or ""))
        if found:
            splits[(entry["job"], found.group("base"))].append(
                (int(found.group("part")), int(found.group("legs")), entry))
    return splits


def check_carriers(document: dict, legs: list, splits: dict, pins, problems: list) -> None:
    """Rule 32: a job split into legs named `<name> (<i>/<n>)` has one carrier, a job named
    `<name> / <name>` that needs the split job, runs under its gate and !cancelled(), and holds
    exactly three steps: a sparse checkout of .github, leg-verdict.sh over exactly those legs, and
    the report to ci-ok. A carrier for a job that is not split carries a name nothing splits.
    `legs` is what jq_names reads off the workflow, and `splits` its split_legs."""
    jobs = document.get("jobs") or {}
    leg_names_of = {key: [entry["name"] for _, _, entry in entries] for key, entries in splits.items()}
    carriers = {job_id: job for job_id, job in jobs.items() if is_carrier(job)}
    names = {job_id: next((e["name"] for e in legs if e["job"] == job_id), None) for job_id in carriers}
    named = pins.CARRIER_NAMES
    def carried(base: str) -> str:
        return named.get(base, f"{base} / {base}")
    for (split_job, base), leg_names in sorted(leg_names_of.items()):
        holders = [job_id for job_id, name in names.items() if name == carried(base)]
        if len(holders) != 1:
            problems.append(error(WORKFLOW, f"job {split_job} splits {base} into {len(leg_names)} legs, and "
                                            f"{len(holders)} carrier(s) carry `{carried(base)}`, where one must"))
    for job_id, job in carriers.items():
        steps = job.get("steps") or []
        name = names.get(job_id) or ""
        base = next((b for b, n in named.items() if n == name), name.split(" / ")[0])
        needs = job.get("needs") or []
        needs = [needs] if isinstance(needs, str) else needs
        split = next(((sj, ln) for (sj, b), ln in leg_names_of.items() if b == base and sj in needs), None)
        if name != carried(base) or split is None:
            problems.append(error(WORKFLOW, f"job {job_id} runs {LEG_VERDICT}, a carrier, and carries `{name}`, which "
                                            f"is the old name of no job it needs that splits into legs: a carrier "
                                            f"carries the name of a split job alone"))
            continue
        split_job, leg_names = split
        checkout = steps[0] if steps else {}
        verdict = steps[1] if len(steps) > 1 else {}
        sparse = str(((checkout or {}).get("with") or {}).get("sparse-checkout", "")).strip()
        given = shlex.split(str((verdict or {}).get("run", "")).strip())
        if (len(steps) != 3 or not str((checkout or {}).get("uses", "")).startswith("actions/checkout@")
                or sparse != ".github" or given[:1] != [LEG_VERDICT] or (steps[2] or {}).get("uses") != REPORT_ACTION):
            problems.append(error(WORKFLOW, f"carrier {job_id} holds {len(steps)} step(s), and a carrier holds exactly "
                                            f"three: a sparse checkout of .github, {LEG_VERDICT} over its legs, and the "
                                            f"report to ci-ok"))
        elif sorted(given[1:]) != sorted(leg_names):
            problems.append(error(WORKFLOW, f"carrier {job_id} reads the legs {', '.join(given[1:])}, and job "
                                            f"{split_job} runs {', '.join(sorted(leg_names))}"))
        condition = str(job.get("if", ""))
        gate = GATE_REF.findall(str((jobs.get(split_job) or {}).get("if", "")))
        if "!cancelled()" not in condition or not gate or gate[0] not in GATE_REF.findall(condition):
            problems.append(error(WORKFLOW, f"carrier {job_id}'s if: must hold !cancelled() and test "
                                            f"needs.changes.outputs.{gate[0] if gate else '<its legs gate>'}, the gate "
                                            f"of the legs it carries"))


# ---------------------------------------------------------------- rule 34: the shard legs
def whole(value):
    """A count as a step's `with:` or a matrix gives it (3, 3.0 or '3'), or None for anything
    else, an expression the reader cannot know among them."""
    if isinstance(value, bool) or value is UNKNOWN or value is None:
        return None
    if isinstance(value, (int, float)):
        return int(value) if value == int(value) else None
    return int(value) if str(value).strip().isdigit() else None


def leg_value(text: str, leg: dict):
    """What an `if:` or a `with:` value is in one leg of a matrix: the leg's own values known
    (`matrix.<key>`, null where the leg sets none, as GitHub reads it) and every other context
    not, so UNKNOWN where the answer rests on one, and UNKNOWN for a text the reader cannot read."""
    def lookup(path: str):
        return leg.get(path[len("matrix."):]) if path.startswith("matrix.") else UNKNOWN
    try:
        return Evaluator(expression(text), lookup).value()
    except ValueError:
        return UNKNOWN


def check_shard_legs(document: dict, splits: dict, problems: list) -> None:
    """Rule 34: the legs of a split job, each named `<name> (<i>/<n>)`, name one count n and each
    index from 1 to n once, a leg that hands its steps a matrix `part` hands them its own index,
    and each step that runs in such a leg and hands it a `with: legs`, written out (`legs: 3`) or
    read off the leg's matrix (`${{ matrix.legs }}`), hands it n. A leg reads its shard of a corpus
    by its index and the count (CORPUS_SHARD=<i>/<n>), so the legs read each document once between
    them only when every index is one leg's and every step of every leg divides by the same n.
    (The envoy-go unit step's `go-test-split.sh "$LEG" 3` is no `with:`: go_test_steps'
    AUDITED_SCRIPTS holds that run: to its text, so another count is refused there.) `splits` is
    split_legs over what jq_names reads off the workflow."""
    jobs = document.get("jobs") or {}
    for (job, base), entries in sorted(splits.items(), key=lambda item: item[0]):
        counts = sorted({legs_count for _, legs_count, _ in entries})
        if len(counts) != 1:
            problems.append(error(WORKFLOW, f"job {job}'s legs of {base} name {len(counts)} counts of legs "
                                            f"({', '.join(map(str, counts))}), where every leg names the same count"))
            continue
        count = counts[0]
        indexes = [index for index, _, _ in entries]
        for index in range(1, count + 1):
            held = indexes.count(index)
            if held != 1:
                which = "no leg is" if held == 0 else f"{held} legs are"
                problems.append(error(WORKFLOW, f"job {job} splits {base} into {count} legs, and {which} "
                                                f"`{base} ({index}/{count})`, where each index from 1 to {count} "
                                                f"names one leg"))
        for index in sorted({index for index in indexes if not 1 <= index <= count}):
            problems.append(error(WORKFLOW, f"job {job} splits {base} into {count} legs, and a leg is "
                                            f"`{base} ({index}/{count})`, an index outside 1 to {count}"))
        for index, _, entry in entries:
            leg = entry["leg"] or {}
            part = leg.get("part")
            if part is not None and str(part) != str(index):
                problems.append(error(WORKFLOW, f"leg `{entry['name']}` of job {job} hands its steps part {part}, "
                                                f"not its own index {index}"))
            for step in (jobs.get(job) or {}).get("steps") or []:
                step = step or {}
                if "if" in step and truth(leg_value(str(step["if"]), leg)) is False:
                    continue
                given = (step.get("with") or {}).get("legs")
                handed = whole(leg_value(given, leg) if isinstance(given, str) and "${{" in given else given)
                if handed is not None and handed != count:
                    problems.append(error(WORKFLOW, f"leg `{entry['name']}` of job {job} runs step "
                                                    f"\"{step_label(step)}\", which hands it legs {handed}, where its "
                                                    f"name counts {count}"))


# ---------------------------------------------------------------- rule 35: the audited scripts' pins
def check_audited_pins(problems: list) -> None:
    """Rule 35: each script go_test_steps.AUDITED_SCRIPTS reads still has the SHA-256 its entry
    was read from. The entry is a person's reading of what the script runs, so a script edited
    past it is read wrong by every reader of the steps, and go-test-step-expansion.py, which a
    person runs by hand, stops on it with no verdict (#2856). This check runs in the lint job on
    every pull request, so the pull request that edits an audited script is the one it fails."""
    import go_test_steps as steps

    for path, entry in steps.AUDITED_SCRIPTS.items():
        try:
            digest = hashlib.sha256((REPO / path).read_bytes()).hexdigest()
        except OSError as unread:
            problems.append(error(path, f"{path}, which .github/scripts/go_test_steps.py's AUDITED_SCRIPTS reads, "
                                        f"cannot be read: {unread}"))
            continue
        if digest != entry.sha256:
            problems.append(error(path, f"{path} has SHA-256 {digest}, and its AUDITED_SCRIPTS entry in "
                                        f".github/scripts/go_test_steps.py was read from {entry.sha256}: read the "
                                        f"script again and update the entry's commands and digest"))



# ---------------------------------------------------------------- rule 20: darwin vets
@dataclass
class Go:
    """The go commands of rule 20: GOPROXY=off over an empty module cache, so nothing is fetched
    and a cold runner answers as a warm machine does, with the module's own toolchain resolved
    first under the machine's own environment."""

    tmp: str = ""

    def __post_init__(self):
        self.tmp = tempfile.mkdtemp(prefix="check-ci-layout-")
        atexit.register(shutil.rmtree, self.tmp, True)
        os.mkdir(f"{self.tmp}/modcache")
        self.toolchains: dict = {}

    def binary(self, workdir: str) -> str:
        if workdir not in self.toolchains:
            root = subprocess.run(["go", "env", "GOROOT"], cwd=REPO / workdir, capture_output=True,
                                  text=True).stdout.strip()
            exe = Path(root) / "bin" / "go" if root else None
            self.toolchains[workdir] = str(exe) if exe and exe.is_file() else "go"
        return self.toolchains[workdir]

    def env(self, workdir: str, **extra) -> dict:
        binary = self.binary(workdir)
        path = os.environ.get("PATH", "")
        if binary != "go":
            path = f"{Path(binary).parent}:{path}"
        return {**os.environ, "PATH": path, "GOPROXY": "off", "GOMODCACHE": f"{self.tmp}/modcache", "GOFLAGS": "",
                "GOTOOLCHAIN": "local", **extra}

    def compiled(self, workdir: str, tags: str, dirs: list) -> set:
        args = [self.binary(workdir), "list", "-e", "-find", "-json=Dir,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles"]
        if tags:
            args += ["-tags", tags]
        patterns = [f"./{Path(d).relative_to(workdir).as_posix()}" if d != workdir else "." for d in dirs]
        done = subprocess.run(args + patterns, cwd=REPO / workdir, env=self.env(workdir, GOOS="darwin", GOARCH="arm64"),
                              capture_output=True, text=True)
        out = set()
        for package in decode_stream(done.stdout):
            directory = package.get("Dir")
            if directory:
                directory = repository_path(Path(directory).resolve())
                for field in ("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles"):
                    out |= {f"{directory}/{name}" for name in package.get(field) or []}
        return out


MAX_TRIAL_TAGS = 6


def check_vet_tags(pins, problems: list) -> list:
    """Rule 20: each darwin vet step (ci_pins.DARWIN_VETS) runs DARWIN_VET_SCRIPT, which takes its
    tag sets from go_build_tags.py, and the sets that script prints for the module compile between
    them every file of the module darwin builds under some choice of tags. go list decides each
    answer; a file none of the sets compiles that names a tag is tried under every choice of its
    own tags, and one go list then compiles is a file the vet passes over."""
    go = Go()
    report = []
    for path, name, workdir in pins.DARWIN_VETS:
        steps = [s for s in ((load_action(path).get("runs") or {}).get("steps") or [])
                 if (s or {}).get("name") == name] if (REPO / path).is_file() else []
        want = pins.darwin_vet_script(workdir)
        if len(steps) != 1 or str(steps[0].get("run", "")).rstrip() != want or \
                clean_dir(str(steps[0].get("working-directory", "."))) != workdir:
            problems.append(error(path, f"{path} holds no single step \"{name}\" in {workdir} whose run: is the darwin "
                                        f"vet that takes its tag sets from .github/scripts/go_build_tags.py:\n{want}"))
            continue
        printed = subprocess.run([sys.executable, str(REPO / ".github/scripts/go_build_tags.py"), ".", "darwin",
                                  "arm64"], cwd=REPO / workdir, env=go.env(workdir), capture_output=True, text=True)
        if printed.returncode != 0:
            problems.append(error(path, f"go_build_tags.py failed in {workdir}, so the sets \"{name}\" vets under are "
                                        f"unknown: {squash(printed.stderr)[:400]}"))
            continue
        sets = [line.removeprefix("-tags=") for line in printed.stdout.splitlines() if line.startswith("-tags=")]
        known = platforms(REPO / workdir)
        disk = {repository_path(p.resolve()) for p in go_files(REPO / workdir)}
        dirs = sorted({p.rsplit("/", 1)[0] for p in disk})
        compiled = set().union(*(go.compiled(workdir, tags, dirs) for tags in sets)) if sets else set()
        misses = 0
        trials: dict = {}
        for file in sorted(disk - compiled):
            names = sorted(n for n in constraint_names(REPO / file) if is_tag(n, known))
            if not names:
                continue
            if len(names) > MAX_TRIAL_TAGS:
                problems.append(error(file, f"{file} names {len(names)} tags, more than the {MAX_TRIAL_TAGS} rule 20 "
                                            f"tries"))
                continue
            directory = file.rsplit("/", 1)[0]
            for bits in itertools.product((True, False), repeat=len(names)):
                tags = ",".join(n for n, on in zip(names, bits) if on)
                if (directory, tags) not in trials:
                    trials[directory, tags] = go.compiled(workdir, tags, [directory])
                if file in trials[directory, tags]:
                    misses += 1
                    problems.append(error(file, f"\"{name}\" ({path}) vets {workdir} for darwin under -tags "
                                                f"{' and '.join(s or '(none)' for s in sets)}, and none of them compiles "
                                                f"{file}, which darwin builds with -tags {tags or '(none)'}: "
                                                f".github/scripts/go_build_tags.py must print a set that builds it"))
                    break
        report.append(f"{path} \"{name}\": {len(sets)} tag set(s), {len(compiled & disk)} of {len(disk)} files "
                      f"compiled, {misses} missed")
    return report


# ---------------------------------------------------------------- rule 16: the race fill
def race_step_packages(pins, problems: list) -> list | None:
    """The packages the daemon's race step runs, as its go-test.sh call's --packages lists them."""
    path, name = pins.DAEMON_RACE_STEP
    steps = [s for s in ((load_action(path).get("runs") or {}).get("steps") or [])
             if (s or {}).get("name") == name] if (REPO / path).is_file() else []
    for line in script_lines(str(steps[0].get("run", ""))) if len(steps) == 1 else []:
        found = re.search(r"go-test\.sh\b.*--packages\s+\"([^\"]+)\".*\s-race\b", line)
        if found:
            return found.group(1).split()
    problems.append(error(path, f"{path} holds no single step \"{name}\" running go-test.sh with --packages and "
                                f"-race, which ci_pins.DAEMON_RACE_STEP names as the daemon's race step"))
    return None


# ---------------------------------------------------------------- rule 33: the notes
NOTES = ".github/ci-notes"
# A why: link is a comment line of its own, `# why: <notes file>#<anchor>`; the same text inside a
# string, a test's fixture among them, is no link.
WHY_LINK = re.compile(r"^\s*#\s*why: (\.github/ci-notes/[\w.-]+\.md)#([\w-]+)")
NAME_HEADING = re.compile(r"^([a-z0-9_-]+): ")


def anchors(path: Path) -> dict:
    """Each `## ` heading of a notes file by the anchor GitHub gives it, with its line: lowercased,
    each character but a letter, a digit, `-`, `_` and a space dropped, spaces made `-`, and a
    repeated anchor numbered."""
    found, seen = {}, defaultdict(int)
    for number, line in enumerate(read_repository_text(path).splitlines(), 1):
        if not line.startswith("## "):
            continue
        heading = line[3:].strip()
        base = re.sub(r"[^\w\- ]", "", heading.lower()).replace(" ", "-")
        slug = f"{base}-{seen[base]}" if seen[base] else base
        seen[base] += 1
        found[slug] = (number, heading)
    return found


def check_notes(document: dict, problems: list) -> list:
    """Rule 33: each `## ` section of .github/ci-notes/*.md is the target of a `why:` link from a
    workflow, an action, a script or another file of .github, each such link names a section that
    exists, and each heading of the form `## <name>: ` names an output of the changes job or a job
    of ci.yaml. A section nothing links describes what no file holds; a link to no section sends
    its reader nowhere."""
    sections = {}
    for path in sorted((REPO / NOTES).glob("*.md")):
        for slug, (number, heading) in anchors(path).items():
            sections[(repository_path(path), slug)] = (number, heading)
    linked = set()
    tracked = repo_files(".github")
    for path in tracked:
        if path.startswith(NOTES + "/") or not (REPO / path).is_file():
            continue
        try:
            text = (REPO / path).read_text()
        except UnicodeDecodeError:
            continue
        for number, line in enumerate(text.splitlines(), 1):
            for found in WHY_LINK.finditer(line):
                target = (found.group(1), found.group(2))
                linked.add(target)
                if target not in sections:
                    problems.append(error(path, f"{path}:{number} links {found.group(1)}#{found.group(2)}, a section "
                                                f"that file does not hold"))
    names = set(document.get("jobs") or {}) | set(((document.get("jobs") or {}).get("changes") or {}).get("outputs")
                                                  or {})
    for (path, slug), (number, heading) in sorted(sections.items()):
        if (path, slug) not in linked:
            problems.append(error(path, f"{path}:{number} `## {heading}` is the target of no why: link, so it describes "
                                        f"what no file of the layout holds: link it from the step it explains, or drop "
                                        f"it"))
        named = NAME_HEADING.match(heading)
        if named and named.group(1) not in names:
            problems.append(error(path, f"{path}:{number} `## {heading}` names {named.group(1)}, which is neither an "
                                        f"output of the changes job nor a job of {WORKFLOW}"))
    return [f"notes: {len(sections)} sections, {len(linked)} linked"]


# ---------------------------------------------------------------- the check
def check() -> int:
    import ci_pins as pins
    import ci_workflow_rules as rules

    problems: list[Annotation] = []
    report: list[str] = []
    documents = workflow_documents(problems)
    document = documents[WORKFLOW]
    filters = changes_filters(document, problems)
    check_gate_outputs(document, problems)
    check_output_steps(document, filters, problems)
    check_ci_ok(documents, problems)
    rules.check_push_conditions(documents, problems)
    rules.check_rerun_guard(document, problems)
    rules.check_cache_round_trip(documents, problems)
    packages = race_step_packages(pins, problems)
    if packages is not None:
        rules.check_race_fill_packages(packages, pins.DAEMON_RACE_STEP, pins.DAEMON_RACE_FILL_JOB, problems)
    rules.check_race_fill_scripts(pins.RACE_FILL_SCRIPTS, problems)
    rules.check_one_merge_group_workflow(problems)
    check_audited_pins(problems)
    runs = go_runs(problems)
    check_split_lists(runs, problems)
    check_changes_tests(documents, problems)
    check_retry(runs, pins, problems)
    check_expression_names(documents, pins, problems)
    check_runners(document, pins, problems)
    check_live_groups(document, pins, problems)
    check_event_gates(documents, pins, problems)
    check_fail_fast(documents, problems)
    report += check_main_names(documents, pins, problems)
    check_timed_steps(documents, pins, problems)
    legs, _ = jq_names(document)
    splits = split_legs(legs)
    check_carriers(document, legs, splits, pins, problems)
    check_shard_legs(document, splits, problems)
    report += check_notes(document, problems)
    report += check_vet_tags(pins, problems)
    report.append(f"go test commands read: {len(runs)}")
    token = secrets.token_hex(16)
    print(f"::stop-commands::{token}")
    for line in report:
        print("report: " + escape_message(line))
    print(f"::{token}::")
    problems = list(dict.fromkeys(problems))
    for problem in problems:
        print_annotation(problem)
    if problems:
        print_annotation(error(None, f"{len(problems)} CI layout problem(s)"))
        return 1
    print("the CI layout holds every rule of check-ci-layout.sh")
    return 0


def main() -> int:
    """Run the checker without allowing repository text into an unprotected workflow log."""
    try:
        return check()
    except RepositoryInputError as exc:
        print_annotation(exc.annotation())
        return 1
    except BaseException:
        token = secrets.token_hex(16)
        print(f"::stop-commands::{token}", flush=True)
        traceback.print_exc(file=sys.stdout)
        print(f"::{token}::", flush=True)
        return 1
