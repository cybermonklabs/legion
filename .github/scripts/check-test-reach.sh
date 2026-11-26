#!/usr/bin/env bash
# Every test of this repository runs in a CI job that `ci-ok` needs. This search reads the
# workflows and the tree, runs no `go`, `bun` or build command, and fails on each of the nine ways
# a test can be left out of every such job while each job stays green:
#
#   1. tag: a `_test.go` file whose `//go:build` constraint names a tag (no platform: `linux`,
#      `unix`, `race`, the other GOOS and GOARCH names, `cgo`, `goN.M` and their negations all
#      pass) that no required step's `go test` passes for that file's package;
#   2. skip: a test one required step's `-skip` leaves out and no other required step runs;
#   3. variable: a variable gate - a Go function, or a Bun `skip`/`skipIf`/`if` condition, that
#      reads an environment variable (by a literal name, through a constant of its own package,
#      or through a local that holds the read) and on that value skips the test or guards an
#      assertion (`t.Error*`, `t.Fatal*`, `t.Fail*`, `expect(`) - whose variable no row of
#      .github/test-variables.yaml names, or whose row's class does not hold for it; and a row
#      that names a job that does not set its variable, a job ci-ok does not need, a script that
#      does not set it, or a variable no gate reads. A row names one variable and its class, and
#      the search checks the class against each gate:
#        set and selector: each job the row names sets the variable (a workflow, job or step
#          `env:`, an inline `NAME=value command`, an `export`, an action that writes it into
#          $GITHUB_ENV), and some named job runs the gate's test in a step whose environment
#          holds it; or, where the row names `set_by_script`, that script assigns it or hands it
#          to a container (`docker run -e NAME=…`), and a step of a named job runs the script,
#          itself or through a repository script that runs it from the step's directory;
#        prerequisite: the gate calls testprereq.Require, requireOrSkip or skipWithout, or reads
#          GITHUB_ACTIONS and fails there;
#        tool: the gate is in a `Benchmark…` function or a file that carries the tool line;
#        opt-out: the gate skips only where the variable is set, a person's opt-out: some
#          required job runs the gate's test, and no required step that runs it sets the variable.
#      A gate in a tool holds under any row. A variable a test only sets is no gate, and neither
#      is one its package's own tests pass to a child process (`"NAME=value"`, `NAME + "="`) or a
#      test that its package's tests start again as a child (`-test.run=^TestXHelper$`) reads:
#      the value is the test's own;
#   4. script: a script (a `.sh`, `.bash` or `.py` file, or one that starts with `#!`) whose first
#      15 lines use the words proof, smoke, check or test, that no required step runs - directly,
#      through a script, an action or a `package.json` script it runs, or through a test it runs -
#      and whose first 15 lines carry neither the tool line (a comment that starts
#      `legion: tool - `) nor the not-a-test line (`legion: not a test - `); and a `*.test.sh`
#      harness, or a script whose header's first line of prose calls it a test or a proof (`A
#      proof that…`, `Tests for…`, `The smoke test of…`), that carries the not-a-test line, which
#      marks only a script that acts on GitHub, a release or a deployment and judges no code;
#   5. schedule: a step of a workflow, of a job, or of its own `if:`, that runs only on `schedule`
#      or `workflow_dispatch` and runs a test command (`go test`, `bun test`, `playwright test`, a
#      `bun run` script that comes to one, through a tool's runs lines too, a `.test.sh` harness)
#      that no required job runs with the same text in the same directory, unless its workflow
#      carries the tool line and a required job runs each test it runs in the same mode (rule 7);
#   6. unrun: a Go test function (Test, Fuzz, Example) that no required step's go test runs: no
#      step's packages hold its package (a Go module no step runs, a package `go list ./... |
#      grep -Ev` leaves out), no step that runs its package builds its file under that step's
#      GOOS, GOARCH, -tags and -race (a `//go:build` constraint or a `_GOOS`/`_GOARCH` file-name
#      suffix no test step meets; a go vet or a build for a platform is no run), or each step that
#      builds it leaves it out by its -run or -skip. A test that reads raceEnabled counts as run
#      only by a step without -race, and a `//go:build !race` file is built by such a step alone,
#      so either needs a plain step whose -run and -skip select it; a `//go:build race` file needs
#      a -race step;
#   7. outside: a step of a workflow ci-ok does not need (one run on a pull request only when its
#      own files change, on a push, by another event, or a workflow of this repository such a
#      workflow calls) that runs a test command no required job runs with the same text in the
#      same directory; where its workflow carries the tool line, it passes when a required job
#      runs each test it runs in the same mode (a Bun file with the same `--preload`s, a Go test
#      under the same -race, a harness), so a repeat count passes and a preload no required job
#      passes fails;
#   8. release: a step of a release workflow (.github/workflows/release*.yaml) that runs a test
#      command and that .github/release-checks.yaml does not list with the artifact it checks,
#      a `go test` over the tree's source above all; an entry of that list that names no step of
#      a release workflow; and a listed step running `go test` whose environment holds no
#      variable an earlier step of its own job built (wrote into $GITHUB_ENV, or named in its
#      `with:` or run:, as a build step names the image tag it loads);
#   9. runs: a tool (a source file a package script runs as `bun <file>`) that a workflow step
#      reaches through `bun run`, whose source names in a string a script of that package.json
#      that no `legion: runs - <script> [<argument>…]` line of its first 15 lines names: what a
#      tool runs is read through those lines.
#
# A shell step that runs no test command - a release step that checks a version it stamped, a
# scheduled step that reads the runner's disk - is not read.
#
# Each finding is one line, `check-test-reach: <kind>: <path>[:<line>]: <test, tag or variable>:
# <reason>`. It exits 1 when there is one, 0 when there is none, and 2 when it cannot read an
# input: a workflow, the registry, the list of release checks, a step whose `go test` it cannot
# read, a `-skip` or `-run` it cannot read.
#
# A required job is one that the ci-ok job of ci.yaml needs, and ci-ok and pr-title themselves; a job of
# a workflow such a job calls (ci.yaml's jobs call their .github/workflows/ci-job-<id>.yaml) is a
# part of it, whatever its own filter: ci-ok needs it, and a merge group runs it. A part whose
# `if:` admits only `schedule` or `workflow_dispatch`, a step whose own `if:` does, and a step
# that carries `continue-on-error`, run no required test.
#
# A finding .github/test-reach-unhomed.yaml names passes: a test dispatch://INFRA-3411's lists give
# a home the tree does not give it yet. An entry of that list that names no finding is a finding of
# its own, `listed`: a required job runs the test now, or it is gone.
#
# How it reads a step. Each job is read once for each leg of its matrix, with each
# `${{ matrix.<name> }}` its steps write replaced by the leg's value, and each job of a workflow
# of this repository a job calls under the inputs that job's `with:` gives, each
# `${{ inputs.<name> }}` replaced; a step that `uses:` a local composite action is read through
# the action's own run: steps, under the inputs its `with:` gives them. A job's GOOS and GOARCH
# are its runner's (darwin on a macos runner, arm64 on an arm one), and a step's env: or its
# command's own `GOOS=`/`GOARCH=` overrides them. A `go test` step is read with
# .github/scripts/go_test_steps.py in any Go module of the tree: its packages, `./...` and
# `go list | grep -Ev` (piped into `xargs` or assigned first, resolved here from the tree, the
# import paths the module's go.mod gives each directory), the packages a legs job's
# `.github/scripts/go_legs.py <module> <job> <leg> <legs>` lister prints (the union of the packages
# .github/go-legs.yaml lists for every leg of that job, each read whole), `for` loops, `-tags`,
# `-race`, `-run`, `-skip`, `-exec`, a gotestsum command (its `--packages`, and the `go test`
# flags after its `--`), a go command behind .github/scripts/race_memory.py or
# .github/scripts/go-test.sh, a script the step runs whose lines the reader reads, and the scripts
# its AUDITED_SCRIPTS reads (database-skips.sh, backend-count.sh, race-unit.sh and forced-order.sh,
# by their arguments); a -run of the pattern .github/scripts/go-tagged-tests.sh prints is the
# alternation that script prints, read off the
# source as it reads it. A script of TEST_BINARY_SCRIPTS (pmdoc's
# differential.sh) is read by each `<binary> -test.run '<pattern>'` it runs, with the variables it
# sets for that run. A test file is in a step's build when its constraint and its file name hold
# for the step's GOOS and GOARCH, with `cgo`, the step's `-tags` and, under `-race`, `race`. A
# `-run` and a `-skip` are matched against a test's name level by level, as `go test` splits
# them; only the top level decides whether the test runs. `bun test` reads its paths (a `./` path
# is a file or a directory, any other word a part of the path), `--path-ignore-patterns` and its
# `--preload`s; `-t` narrows the tests of a file it runs and is read as running the file.
# `bun run <script>` is read through the `package.json` of the step's directory, a tool a package
# script runs (`bun <file>`) through the package scripts its `legion: runs - ` lines name, and a
# shell script of the repository the step runs by its path (from the step's directory, or from the
# root through `$GITHUB_WORKSPACE`) through that script's own commands, with the variables it
# exports. A command these read is read past the words that only start it: the variables set
# before it, `env`, `exec`, and coreutils `timeout` with its options and duration
# (dispatch-unit-repeat.yaml's lane step runs `exec timeout … bun run test:repeat …`). A
# `playwright test` runs every spec under its `--config`'s directory that its file filters keep:
# its `--project`s and `--grep` choose among those specs and are not read, so a spec counts as run
# by every Playwright step of its config. The Go reader (go_test_steps.py) refuses a line that
# starts a `go` through such a word by name, rather than reading it as no command.
#
# How it reads a gate. A Go file of a test (a `_test.go` file, or one that imports `testing`) is
# read token by token: each function's reads of os.Getenv and os.LookupEnv, the names that hold a
# read (a local, a field of one, a package-level variable a function of the package assigns one
# to, a call of a function that returns one as a plain value), counted where the expression holds
# the value itself or a reshaping of it (strings, strconv, filepath, os.Stat) and not where it is
# the argument of another call; and each `if`, `switch` and `case` whose condition holds one, with
# the calls its branches make. A call of testprereq.Require whose condition holds one is a gate
# too, and so is each call that names a variable to a function that reads the one its caller
# names and skips or asserts on it. Two shapes are no gate: an `if` that is itself an assertion
# about the environment (its condition names nothing but reads, their holders, an error and
# constants, and its branches only assert: `if _, set := os.LookupEnv(X); set { t.Fatal(…) }`), and,
# inside a block a read guards, an `if err != nil` or such a check of the value, which is the
# block's own reading or writing of what the variable names. A read of a variable its package's
# TestMain sets for the whole test process (`os.Setenv(NAME, …)`) is no gate either: the test binary
# hands itself that value, from what its own reads gave it. A Bun or Playwright test file (one
# that imports `bun:test` or `@playwright/test`) is read for its `skipIf`, `if`, `skip` and `fixme`
# conditions and the `if` blocks that hold an `expect(` or a skip, each resolved through the
# file's own declarations and functions and those of a module of the same package it imports. A
# Go gate's test is its `Test` function, or each test of its package (and, for a helper package,
# of each package whose tests import it); a Bun gate's test is its file.
#
# How it reads a script's reach. A script counts as run when a required step names it, or a text
# a required step runs names it - an action, a `package.json` script, a script already counted,
# a test file a required step runs, or a module of the same package such a Bun test imports - by
# a path whose end is the script's path or by its bare name; a Python script reaches another only
# through an import.
#
# What it cannot see there: a mention is not a run. A test file that names a script counts as
# running it, so a test that reads a script or lifts its functions out without running the script
# makes the script read as reached. scripts/e2e/lib/nats-callout-omp-pin.test.ts is one: it reads
# nats-callout-sessions.sh's functions through script-functions.ts and starts no run of the script,
# whose own reach is the agent-harness job's nats-callout-sessions part.
#
# Files are read from `git ls-files` (tracked and untracked files git does not ignore) where the
# tree is a git checkout, and by a walk of the tree otherwise.
#
# Run from anywhere: .github/scripts/check-test-reach.sh
# CI runs it in ci.yaml's test-reach job, and its tests in ci-job-test.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# The step reader is .github/scripts/go_test_steps.py, beside this script in the repository it
# checks, which PYTHONPATH names, with go_build_tags.py, which this search imports.
# PYTHONDONTWRITEBYTECODE keeps the imports from leaving a __pycache__ in
# the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
from __future__ import annotations

import fnmatch
import json
import os
import re
import shlex
import subprocess
import sys
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path

import yaml

import go_test_steps as steps
from go_test_steps import script_lines, squash, step_workdir
from go_build_tags import EXTRA_ARCH, EXTRA_OS, go_skips_dir, is_tag

ROOT = Path.cwd()
WORKFLOWS = ".github/workflows"
CI = f"{WORKFLOWS}/ci.yaml"
REGISTRY = ".github/test-variables.yaml"
RELEASE_CHECKS = ".github/release-checks.yaml"
# The tests dispatch://INFRA-3411's lists give a home the tree does not give them yet, until that
# issue's last pull request: each passes while the search finds it, and fails once it does not.
UNHOMED = ".github/test-reach-unhomed.yaml"
VERDICT = "ci-ok"
# The jobs main's ruleset requires besides the verdict, which ci-ok does not need.
ALSO_REQUIRED = ("ci-ok", "pr-title")
CLASSES = ("set", "selector", "prerequisite", "tool", "opt-out")
JOB_CLASSES = ("set", "selector")
TOOL_LINE = re.compile(r"^\s*(?:#|//|--|;|\*|/\*)\s*legion: tool - \S")
NOT_A_TEST_LINE = re.compile(r"^\s*(?:#|//|--|;|\*|/\*)\s*legion: not a test - \S")
# The words of rule 4, with their inflections and no longer word: `check-…` and `_test` count,
# `checkout`, `latest` and `testdata` do not.
TEST_WORDS = re.compile(r"(?<![A-Za-z])(?:proof|smoke|check|test)(?:s|es|ed|ing|er|ers)?(?![A-Za-z])",
                        re.IGNORECASE)
HEAD_LINES = 15


class Unreadable(Exception):
    """An input this search cannot read, which it will not guess at: exit 2."""


@dataclass(frozen=True, order=True)
class Finding:
    kind: str
    path: str
    line: int
    subject: str
    reason: str

    def __str__(self) -> str:
        where = f"{self.path}:{self.line}" if self.line else self.path
        return f"check-test-reach: {self.kind}: {where}: {self.subject}: {self.reason}"


# ------------------------------------------------------------------------------------ the tree
SKIPPED_DIRS = {".git", ".jj", "node_modules"}


def tree_files() -> list[str]:
    if (ROOT / ".git").exists():
        done = subprocess.run(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
                              cwd=ROOT, capture_output=True)
        if done.returncode == 0:
            names = {name for name in os.fsdecode(done.stdout).split("\0") if name}
            return sorted(name for name in names
                          if not SKIPPED_DIRS.intersection(name.split("/")) and (ROOT / name).is_file())
    found = []
    for directory, subdirectories, files in os.walk(ROOT):
        subdirectories[:] = sorted(d for d in subdirectories if d not in SKIPPED_DIRS)
        for name in files:
            found.append(os.path.relpath(os.path.join(directory, name), ROOT).replace(os.sep, "/"))
    return sorted(found)


_texts: dict[str, str] = {}


def read_text(path: str) -> str:
    if path not in _texts:
        try:
            _texts[path] = (ROOT / path).read_text(encoding="utf-8", errors="replace")
        except OSError as error:
            raise Unreadable(f"{path}: {error.strerror}") from error
    return _texts[path]


def head(path: str) -> list[str]:
    return read_text(path).splitlines()[:HEAD_LINES]


def norm(path: str) -> str:
    return os.path.normpath(path).replace(os.sep, "/").removeprefix("./")


def join(directory: str, path: str) -> str:
    return norm(os.path.join(directory, path)) if directory not in ("", ".") else norm(path)


# --------------------------------------------------------------------------------- the workflows
class LineLoader(yaml.SafeLoader):
    """SafeLoader that keeps each mapping's line, so a finding names where to look."""


class Mapping(dict):
    line = 0


def construct_mapping(loader, node):
    mapping = Mapping(yaml.SafeLoader.construct_mapping(loader, node, deep=True))
    mapping.line = node.start_mark.line + 1
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, construct_mapping)
_loaded: dict[str, dict] = {}


def load_yaml(path: str) -> dict:
    if path not in _loaded:
        try:
            document = yaml.load(read_text(path), Loader=LineLoader) or {}
        except yaml.YAMLError as error:
            raise Unreadable(f"{path} is not valid YAML: {error}") from error
        if not isinstance(document, dict):
            raise Unreadable(f"{path} is not a mapping")
        _loaded[path] = document
    return _loaded[path]


def triggers(document: dict) -> set[str]:
    on = document.get("on", document.get(True))
    if isinstance(on, str):
        return {on}
    if isinstance(on, list):
        return {str(item) for item in on}
    return {str(key) for key in (on or {})}


SCHEDULED = {"schedule", "workflow_dispatch"}
EVENT_TERM = re.compile(r"github\.event_name\s*==\s*'([a-z_]+)'")


def scheduled_only(condition) -> bool:
    """Whether a job's or a step's `if:` admits only schedule and workflow_dispatch runs: a
    disjunction of `github.event_name == '…'` terms, each naming one of the two."""
    if condition is None:
        return False
    text = str(condition).strip()
    text = text.removeprefix("${{").removesuffix("}}").strip()
    terms = [term.strip().strip("()").strip() for term in text.split("||")]
    events = [EVENT_TERM.fullmatch(term) for term in terms]
    return bool(terms) and all(events) and {e.group(1) for e in events} <= SCHEDULED


# What GitHub sets in every job's environment.
RUNNER_ENV = frozenset({
    "CI", "GITHUB_ACTION", "GITHUB_ACTIONS", "GITHUB_ACTOR", "GITHUB_API_URL", "GITHUB_BASE_REF",
    "GITHUB_ENV", "GITHUB_EVENT_NAME", "GITHUB_EVENT_PATH", "GITHUB_GRAPHQL_URL", "GITHUB_HEAD_REF",
    "GITHUB_JOB", "GITHUB_OUTPUT", "GITHUB_PATH", "GITHUB_REF", "GITHUB_REF_NAME", "GITHUB_REPOSITORY",
    "GITHUB_REPOSITORY_OWNER", "GITHUB_RUN_ATTEMPT", "GITHUB_RUN_ID", "GITHUB_RUN_NUMBER",
    "GITHUB_SERVER_URL", "GITHUB_SHA", "GITHUB_STEP_SUMMARY", "GITHUB_WORKFLOW", "GITHUB_WORKSPACE",
    "HOME", "PATH", "RUNNER_ARCH", "RUNNER_NAME", "RUNNER_OS", "RUNNER_TEMP", "RUNNER_TOOL_CACHE",
})
GITHUB_ENV_WRITE = re.compile(r">>\s*\"?\$\{?GITHUB_ENV\b")
ENV_ENTRY = re.compile(r"(?:echo|printf)\s+(?:-[a-z]+\s+)?[\"']?([A-Za-z_][A-Za-z0-9_]*)(?:=|<<)")
EXPORT = re.compile(r"(?:^|[;&|]\s*)export\s+((?:[A-Za-z_][A-Za-z0-9_]*(?:=\S*)?\s*)+)")
INLINE = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)=")


def env_written(script: str) -> set[str]:
    """The variables a run: script writes into $GITHUB_ENV, which every later step of its job
    inherits."""
    names = set()
    for line in script_lines(script):
        if GITHUB_ENV_WRITE.search(line):
            names.update(ENV_ENTRY.findall(line))
    return names


def exported(script: str) -> set[str]:
    names = set()
    for line in script_lines(script):
        for match in EXPORT.finditer(line):
            names.update(INLINE.match(word).group(1) if "=" in word else word
                         for word in match.group(1).split() if re.match(r"[A-Za-z_]", word))
    return names


@dataclass
class Part:
    job: str  # ci.yaml's job id, or the id of the job of another workflow it starts from
    workflow: str  # the file holding the part's steps
    name: str  # the part's own job id in that file
    body: dict
    document: dict
    required: bool
    arch: str
    goos: str = "linux"  # the runner's: darwin on a macos runner, windows on a windows one
    leg: dict = field(default_factory=dict)  # the matrix values of this leg of the job
    inputs: dict = field(default_factory=dict)  # what the job that calls its workflow gives it

    @property
    def contexts(self) -> dict[str, dict[str, str]]:
        return {"matrix": self.leg, "inputs": self.inputs}


@dataclass
class RunStep:
    part: Part
    index: int
    name: str
    line: int
    workdir: str
    script: str
    env: frozenset[str]
    exported: frozenset[str]
    skipped: bool  # continue-on-error
    scheduled: bool  # its own `if:`, or its action step's, admits only schedule and workflow_dispatch
    values: dict = field(default_factory=dict)  # its env: blocks' variables, with their values
    own_name: str = ""  # the workflow step's own name, an action's step under it included
    keys: frozenset[str] = frozenset()  # the workflow step's name, id and uses, as a list names it

    @property
    def label(self) -> str:
        return f"{self.part.workflow}'s {self.part.name} / {self.name}"


def runner_platform(runs_on: str) -> tuple[str, str]:
    """The GOOS and GOARCH a job's `runs-on` gives its steps."""
    goos = "darwin" if "macos" in runs_on else "windows" if "windows" in runs_on else "linux"
    return goos, "arm64" if "arm" in runs_on else "amd64"


def parts_of(job_id: str, workflow: str, name: str, body: dict, document: dict, required: bool,
             seen: frozenset, inputs: dict | None = None) -> list[Part]:
    """The parts one job is: itself, once for each leg of its matrix, or each part of the
    workflow of this repository it calls, under the inputs its `with:` gives that workflow."""
    required = required and not scheduled_only(body.get("if"))
    inputs = inputs or {}
    uses = str(body.get("uses") or "")
    if uses.startswith(f"./{WORKFLOWS}/"):
        path = uses.removeprefix("./")
        if not (ROOT / path).is_file():
            raise Unreadable(f"{workflow}'s {name} calls {path}, which is not in the repository")
        if path in seen:
            return []
        called = load_yaml(path)
        found = []
        for leg in steps.matrix_legs(body):
            given = {**steps.input_defaults(called),
                     **{str(k): steps.substitute(steps.context_text(v), {"matrix": leg, "inputs": inputs})
                        for k, v in (body.get("with") or {}).items()}}
            for part, part_body in (called.get("jobs") or {}).items():
                found += parts_of(job_id, path, part, part_body or {}, called, required, seen | {path}, given)
        return found
    if uses:
        return []
    found = []
    for leg in steps.matrix_legs(body):
        goos, arch = runner_platform(steps.substitute(str(body.get("runs-on") or ""), {"matrix": leg, "inputs": inputs}))
        found.append(Part(job=job_id, workflow=workflow, name=name, body=body, document=document,
                          required=required, arch=arch, goos=goos, leg=leg, inputs=inputs))
    return found


def run_steps(part: Part) -> list[RunStep]:
    contexts = part.contexts
    values = {**steps.env_text(part.document.get("env"), contexts), **steps.env_text(part.body.get("env"), contexts)}
    base = set(RUNNER_ENV) | set(values)
    written: set[str] = set()
    found = []
    for index, step in enumerate(part.body.get("steps") or []):
        name = str(step.get("name") or f"step {index + 1}")
        keys = frozenset(str(step[key]) for key in ("name", "id", "uses") if step.get(key))
        step_values = {**values, **steps.env_text(step.get("env"), contexts)}
        skipped = bool(step.get("continue-on-error"))
        scheduled = scheduled_only(step.get("if"))
        line = getattr(step, "line", 0)
        # Each step of a composite action sees what the action's earlier steps wrote into
        # $GITHUB_ENV, as each later step of the job does.
        for run in step_runs(part, step, contexts):
            run_scheduled = scheduled or scheduled_only(run.condition)
            env = frozenset(base | written | set(step_values) | set(run.values))
            found.append(RunStep(part, index, f"{name} ({run.action})" if run.action else name, line, run.workdir,
                                 run.script, env, frozenset(exported(run.script)), skipped, run_scheduled,
                                 {**step_values, **run.values}, name, keys))
            if not run_scheduled:
                written |= env_written(run.script)
    return found


@dataclass
class StepRun:
    script: str
    workdir: str
    values: dict  # the variables an action's step's own env: sets
    action: str = ""  # the action file the step is of, or "" for a workflow's own run: step
    condition: object = None  # an action's step's own `if:`


def step_runs(part: Part, step: dict, contexts: dict) -> list[StepRun]:
    """What one workflow step runs, with the leg's matrix values and the caller's inputs read in:
    its own run:, or the run: of each step of the local composite action it uses, under the
    inputs its `with:` gives them."""
    if step.get("run"):
        return [StepRun(steps.substitute(str(step["run"]), contexts),
                        steps.substitute(step_workdir(part.document, part.body, step), contexts), {})]
    given = {str(k): steps.substitute(steps.context_text(v), contexts) for k, v in (step.get("with") or {}).items()}
    found = []
    for path, inner, inputs in steps.local_action_steps(str(step.get("uses") or ""), given):
        inner_contexts = {"inputs": inputs}
        found.append(StepRun(steps.substitute(str(inner["run"]), inner_contexts), steps.action_workdir(inner, inputs),
                             steps.env_text(inner.get("env"), inner_contexts), path, inner.get("if")))
    return found


@dataclass
class Workflows:
    parts: list[Part]
    steps: list[RunStep]
    required_jobs: set[str]


def workflows() -> Workflows:
    ci = load_yaml(CI)
    jobs = ci.get("jobs") or {}
    if VERDICT not in jobs:
        raise Unreadable(f"{CI} has no {VERDICT} job")
    needs = jobs[VERDICT].get("needs") or []
    required = set([needs] if isinstance(needs, str) else needs) | set(ALSO_REQUIRED)
    parts = []
    for job_id, body in jobs.items():
        parts += parts_of(job_id, CI, job_id, body or {}, ci, job_id in required, frozenset())
    found = [step for part in parts for step in run_steps(part)]
    return Workflows(parts, found, required)


def required_steps(flows: Workflows) -> list[RunStep]:
    return [step for step in flows.steps if step.part.required and not step.skipped and not step.scheduled]


# --------------------------------------------------------------------------------- the Go tree
GOOS = frozenset({"aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios", "js", "linux",
                  "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows"}) | EXTRA_OS
GOARCH = frozenset({"386", "amd64", "arm", "arm64", "loong64", "mips", "mipsle", "mips64", "mips64le",
                    "ppc64", "ppc64le", "riscv64", "s390x", "wasm"}) | EXTRA_ARCH
GO_RELEASES = frozenset(f"go1.{minor}" for minor in range(1, 40))


CONSTRAINT_TOKEN = re.compile(r"\s*(\(|\)|!|&&|\|\||[^\s()!&|]+)")


def constraint_names(expression: str) -> list[str]:
    return [token for token in CONSTRAINT_TOKEN.findall(expression) if token not in ("(", ")", "!", "&&", "||")]


def evaluate(expression: str, true: set[str]) -> bool:
    tokens = CONSTRAINT_TOKEN.findall(expression)
    position = 0

    def peek():
        return tokens[position] if position < len(tokens) else None

    def take():
        nonlocal position
        position += 1
        return tokens[position - 1]

    def disjunction():
        value = conjunction()
        while peek() == "||":
            take()
            value = conjunction() or value
        return value

    def conjunction():
        value = unary()
        while peek() == "&&":
            take()
            value = unary() and value
        return value

    def unary():
        token = take()
        if token == "!":
            return not unary()
        if token == "(":
            value = disjunction()
            take()
            return value
        return token in true

    return disjunction()


def filename_holds(name: str, goos: str, goarch: str) -> bool:
    stem = name.removesuffix(".go").removesuffix("_test")
    parts = stem.split("_")[1:]
    if len(parts) >= 2 and parts[-2] in GOOS and parts[-1] in GOARCH:
        return parts[-2] == goos and parts[-1] == goarch
    if parts and parts[-1] in GOOS:
        return parts[-1] == goos
    if parts and parts[-1] in GOARCH:
        return parts[-1] == goarch
    return True


GO_TEST_FUNC = re.compile(r"^func\s+((?:Test|Fuzz|Example|Benchmark)[A-Za-z0-9_]*)\s*\(", re.M)
CONSTRAINT_LINE = re.compile(r"^//go:build\s+(.+?)\s*$")


@dataclass(eq=False)
class GoFile:
    path: str
    directory: str
    module: str  # the module's directory
    is_test: bool
    package: str
    constraint: str | None
    constraint_line: int
    tests: list[tuple[str, int]]
    text: str
    _builds: dict = field(default_factory=dict, repr=False)

    def builds(self, true: frozenset[str], goos: str, goarch: str) -> bool:
        key = (true, goos, goarch)
        if key not in self._builds:
            self._builds[key] = (filename_holds(Path(self.path).name, goos, goarch)
                                 and (self.constraint is None or evaluate(self.constraint, true)))
        return self._builds[key]

    @property
    def tool(self) -> bool:
        return any(TOOL_LINE.match(line) for line in head(self.path))


def go_header(text: str) -> tuple[str | None, int, str]:
    """The `//go:build` expression of a file's leading comments, its line, and the package name."""
    constraint, line, package = None, 0, ""
    in_block = False
    for number, raw in enumerate(text.splitlines(), start=1):
        stripped = raw.strip()
        if in_block:
            if "*/" in stripped:
                in_block = False
            continue
        if not stripped or stripped.startswith("//"):
            found = CONSTRAINT_LINE.match(stripped)
            if found and constraint is None:
                constraint, line = found.group(1), number
            continue
        if stripped.startswith("/*"):
            in_block = "*/" not in stripped
            continue
        words = stripped.split()
        if words[0] == "package" and len(words) > 1:
            package = words[1]
        break
    return constraint, line, package


@dataclass
class GoTree:
    files: dict[str, GoFile]  # path -> file
    by_directory: dict[str, list[GoFile]]
    modules: dict[str, str]  # module directory -> module path

    def import_path(self, directory: str) -> str | None:
        module = self.module_of(directory)
        if module is None:
            return None
        relative = directory[len(module):].lstrip("/") if module != "." else directory
        return self.modules[module] + (f"/{relative}" if relative else "")

    def module_of(self, directory: str) -> str | None:
        best = None
        for module in self.modules:
            if directory == module or directory.startswith(f"{module}/") or module == ".":
                if best is None or len(module) > len(best):
                    best = module
        return best

    def directory_of(self, import_path: str) -> str | None:
        best = None
        for module, path in self.modules.items():
            if import_path == path or import_path.startswith(f"{path}/"):
                if best is None or len(path) > len(self.modules[best]):
                    best = module
        if best is None:
            return None
        rest = import_path[len(self.modules[best]):].lstrip("/")
        return join(best, rest) if rest else best


def go_tree(files: list[str]) -> GoTree:
    modules = {}
    for path in files:
        if Path(path).name == "go.mod":
            found = re.search(r"^module\s+(\S+)", read_text(path), re.M)
            if found:
                modules[norm(str(Path(path).parent))] = found.group(1)
    tree = GoTree({}, defaultdict(list), modules)
    for path in files:
        if not path.endswith(".go"):
            continue
        parts = path.split("/")
        if any(part == "testdata" or part == "vendor" or part.startswith((".", "_")) for part in parts):
            continue
        directory = "/".join(parts[:-1]) or "."
        module = tree.module_of(directory)
        if module is None:
            continue
        text = read_text(path)
        constraint, line, package = go_header(text)
        is_test = path.endswith("_test.go")
        tests = []
        if is_test:
            for match in GO_TEST_FUNC.finditer(text):
                name = match.group(1)
                if name == "TestMain":
                    continue
                suffix = re.sub(r"^(Test|Fuzz|Example|Benchmark)", "", name)
                if suffix and suffix[0].islower():
                    continue
                number = text.count("\n", 0, match.start()) + 1
                tests.append((name, number))
        go_file = GoFile(path, directory, module, is_test, package, constraint, line, tests, text)
        tree.files[path] = go_file
        tree.by_directory[directory].append(go_file)
    return tree


# The GOOS values the `unix` constraint holds for.
UNIX = frozenset({"aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "linux",
                  "netbsd", "openbsd", "solaris"})


def truths(tags: str, race: bool, goarch: str, goos: str = "linux") -> frozenset[str]:
    """The names a constraint reads as true in a step's build: its GOOS (linux unless the runner
    or the step says otherwise) and its architecture, cgo, every release, the step's -tags and,
    under -race, race."""
    true = {goos, goarch, "gc", "cgo"} | GO_RELEASES | ({"unix"} if goos in UNIX else set())
    true |= {tag for tag in re.split(r"[,\s]+", tags) if tag}
    if race:
        true.add("race")
    return frozenset(true)


@dataclass(eq=False)
class GoRun:
    """One `go test` of a step: the package directories it builds and how, and the variables in
    its environment."""

    step: RunStep
    text: str
    directories: list[str]
    tags: str
    race: bool
    run: str
    skip: list[str]
    goarch: str
    env: frozenset[str]
    goos: str = "linux"

    def __post_init__(self) -> None:
        self.true = truths(self.tags, self.race, self.goarch, self.goos)
        self.directory_set = set(self.directories)
        self._files: dict[str, list[GoFile]] = {}
        self._pattern = None
        if self.run:
            try:
                self._pattern = re.compile(split_levels(self.run)[0])
            except re.error as error:
                raise Unreadable(f"{self.step.label}: -run {self.run!r} is no pattern this search reads: {error}")

    def files(self, tree: GoTree, directory: str) -> list[GoFile]:
        if directory not in self._files:
            self._files[directory] = [f for f in tree.by_directory.get(directory, [])
                                      if f.builds(self.true, self.goos, self.goarch)]
        return self._files[directory]

    def builds(self, go_file: GoFile) -> bool:
        return go_file.directory in self.directory_set and go_file.builds(self.true, self.goos, self.goarch)

    def runs(self, name: str) -> bool:
        if name.startswith("Benchmark") or name in self.skip:
            return False
        return self._pattern is None or self._pattern.search(name) is not None


def split_levels(pattern: str) -> list[str]:
    """A -run or -skip pattern split at each `/` outside brackets and parentheses, as `go test`
    splits it into one pattern per level of subtests."""
    levels, current, depth, bracket, escaped = [], "", 0, False, False
    for char in pattern:
        if escaped:
            current += char
            escaped = False
            continue
        if char == "\\":
            current += char
            escaped = True
            continue
        if bracket:
            bracket = char != "]"
        elif char == "[":
            bracket = True
        elif char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
        elif char == "/" and depth == 0:
            levels.append(current)
            current = ""
            continue
        current += char
    levels.append(current)
    return levels


def buildable_directory(tree: GoTree, directory: str, true: set[str], goarch: str, goos: str = "linux") -> bool:
    return any(f.builds(true, goos, goarch) for f in tree.by_directory.get(directory, []))


def expand_pattern(tree: GoTree, workdir: str, argument: str, true: set[str], goarch: str,
                   goos: str = "linux") -> list[str]:
    """The package directories one argument of a go command names, from its directory."""
    if argument.endswith("/...") or argument == "...":
        base = argument[:-4].rstrip("/") if argument != "..." else "."
        if base.startswith((".", "/")):
            top = join(workdir, base)
        else:
            top = tree.directory_of(base)
            if top is None:
                raise Unreadable(f"{argument} names no package of this tree's modules")
        found = []
        for directory in sorted(tree.by_directory):
            if directory == top or directory.startswith(f"{top}/") or top == ".":
                relative = directory[len(top):].lstrip("/") if top != "." else directory
                parent, skipped = ROOT / top, False
                for part in [p for p in relative.split("/") if p]:
                    if go_skips_dir(parent, part):
                        skipped = True
                        break
                    parent = parent / part
                if not skipped and buildable_directory(tree, directory, true, goarch, goos):
                    found.append(directory)
        return found
    if argument.startswith((".", "/")):
        return [join(workdir, argument.rstrip("/") or ".")]
    directory = tree.directory_of(argument)
    if directory is None:
        raise Unreadable(f"{argument} names no package of this tree's modules")
    return [directory]


def tagged_tests(tree: GoTree, tag: str, directory: str) -> str:
    """What .github/scripts/go-tagged-tests.sh prints for a tag and a directory, read the way it
    reads the source, with its refusals."""
    names = set()
    tagged = 0
    for go_file in sorted(tree.by_directory.get(directory, []), key=lambda f: f.path):
        if not go_file.is_test:
            continue
        constraint = (go_file.constraint or "").strip()
        if constraint == tag:
            tagged += 1
            names |= {name for name, _ in go_file.tests if not name.startswith("Benchmark")}
        elif constraint and tag in constraint_names(constraint):
            raise steps.Refused(f"go-tagged-tests.sh refuses {go_file.path}: '//go:build {constraint}' "
                                f"names '{tag}' in a shape it will not read")
    if not tagged:
        raise steps.Refused(f"go-tagged-tests.sh refuses {directory}: no *_test.go file in it is "
                            f"constrained by '//go:build {tag}' alone")
    if not names:
        raise steps.Refused(f"go-tagged-tests.sh refuses {directory}: its '//go:build {tag}' files "
                            "declare no Test, Fuzz or Example function")
    return "^(" + "|".join(sorted(names)) + ")$"


GREP = re.compile(r"^grep\s+(?P<flags>-[A-Za-z]+)\s+(?P<pattern>.+)$")


def split_shell(text: str, operators: tuple[str, ...] = ("&&", "||", ";", "|")) -> list[str]:
    """A line split at each operator outside quotes, each part stripped."""
    parts, current, index, quote = [], "", 0, None
    while index < len(text):
        char = text[index]
        if quote:
            current += char
            if char == "\\" and quote == '"' and index + 1 < len(text):
                current += text[index + 1]
                index += 2
                continue
            if char == quote:
                quote = None
            index += 1
            continue
        if char in "'\"":
            quote = char
            current += char
            index += 1
            continue
        if char == "\\" and index + 1 < len(text):
            current += text[index:index + 2]
            index += 2
            continue
        operator = next((o for o in operators if text.startswith(o, index)), None)
        if operator:
            parts.append(current.strip())
            current = ""
            index += len(operator)
            continue
        current += char
        index += 1
    parts.append(current.strip())
    return parts


def static_answer(tree: GoTree, goarch: str, goos: str = "linux"):
    """The answer go_test_steps.read_block asks for, read off the tree: the alternation
    go-tagged-tests.sh prints, the packages a legs job's go_legs.py lister stands for (the union
    of the job's legs, go_test_steps.legs_packages), and the import paths `go list … | grep …`
    prints."""

    def answer(step: steps.Step, text: str) -> str:
        if steps.TAGGED_TESTS in text:
            words = shlex.split(text)
            at = next(i for i, word in enumerate(words) if word.endswith(steps.TAGGED_TESTS.split("/")[-1]))
            if len(words) != at + 3:
                raise steps.Refused(f"{step.label}: `{text}` is no call of go-tagged-tests.sh this search reads")
            return tagged_tests(tree, words[at + 1], join(step.workdir, words[at + 2].rstrip("/")))
        legs = steps.legs_packages(step, text)
        if legs is not None:
            return "\n".join(legs)
        stages = split_shell(text, ("|",))
        words = shlex.split(stages[0])
        if words[:2] != ["go", "list"] or any(word.startswith("-") for word in words[2:]):
            raise steps.Refused(f"{step.label}: `{text}` is no package lister this search reads")
        true = truths("", False, goarch, goos)
        directories = []
        for argument in words[2:] or ["."]:
            directories += expand_pattern(tree, step.workdir, argument, true, goarch, goos)
        paths = [tree.import_path(d) or d for d in dict.fromkeys(directories)]
        for stage in stages[1:]:
            grep = GREP.match(stage)
            if not grep or set(grep.group("flags")[1:]) - set("Ev"):
                raise steps.Refused(f"{step.label}: `{stage}` is no filter this search reads")
            pattern = shlex.split(grep.group("pattern"))[0]
            keep = "v" not in grep.group("flags")
            paths = [p for p in paths if (re.search(pattern, p) is not None) == keep]
        return "\n".join(paths)

    return answer


def go_runs(flows: Workflows, tree: GoTree) -> list[GoRun]:
    runs = []
    for step in required_steps(flows):
        runs += step_go_runs(tree, step)
        for line in commands(step.script):
            runs += binary_runs(tree, step, line)
    return runs


def step_go_runs(tree: GoTree, step: RunStep) -> list[GoRun]:
    """The `go test` commands one step starts, read by go_test_steps.step_of in any Go module of
    the tree: in its own run:, in the script it runs, through the composite action it is a step
    of, and with its matrix leg's values and its caller's inputs read in."""
    shared = steps.step_of(step.part.workflow, step.part.name, step.own_name, step.workdir, step.script,
                           tuple(sorted(tree.modules)))
    if shared is None:
        return []
    goos, goarch = platform(step, {})
    return [go_run(tree, step, command) for command in steps.commands_of(shared, static_answer(tree, goarch, goos))]


def platform(step: RunStep, command_env: dict) -> tuple[str, str]:
    """The GOOS and GOARCH a command builds for: its own line's, else its step's env:, else its
    runner's."""
    return (command_env.get("GOOS") or step.values.get("GOOS") or step.part.goos,
            command_env.get("GOARCH") or step.values.get("GOARCH") or step.part.arch)


def go_run(tree: GoTree, step: RunStep, command: steps.GoTest, extra: frozenset = frozenset()) -> GoRun:
    goos, goarch = platform(step, command.env)
    true = truths(command.tags, command.race, goarch, goos)
    directories = []
    for argument in command.packages:
        directories += expand_pattern(tree, command.step.workdir, argument, true, goarch, goos)
    return GoRun(step, command.text, list(dict.fromkeys(directories)), command.tags, command.race,
                 command.run, steps.skip_names(command.skip) or [], goarch,
                 frozenset(step.env | step.exported | inline_env(step.script) | set(command.env) | extra), goos)


# The scripts that build one package's test binary with `go test -c` and run tests of it with
# `-test.run`, read by their own text: each `<binary> -test.run '<pattern>'` line, with the
# variables it sets before it, is a run of that package's tests matching the pattern.
TEST_BINARY_SCRIPTS = {
    "packages/envoy/internal/dispatch/pmdoc/gen/differential.sh": "packages/envoy/internal/dispatch/pmdoc",
}
TEST_BINARY_RUN = re.compile(r"^\s*((?:[A-Za-z_][A-Za-z0-9_]*=\S*\s+)*)\S+\.test\"?\s+-test\.run\s+'([^']+)'", re.M)


def binary_runs(tree: GoTree, step: RunStep, line: str) -> list[GoRun]:
    words = words_of(line)
    while words and INLINE.match(words[0]):
        words = words[1:]
    script = join(step.workdir, words[0]) if words else ""
    if script not in TEST_BINARY_SCRIPTS:
        return []
    text = re.sub(r"\\\n\s*", " ", read_text(script))
    if "go test -c" not in text:
        raise Unreadable(f"{script} no longer builds its test binary with `go test -c`, as this search reads it")
    found = []
    for match in TEST_BINARY_RUN.finditer(text):
        names = frozenset(INLINE.match(word).group(1) for word in match.group(1).split())
        command = steps.GoTest(steps.Step(step.part.workflow, step.part.name, step.name, TEST_BINARY_SCRIPTS[script], line),
                               match.group(0), ["."], run=match.group(2))
        found.append(go_run(tree, step, command, names))
    if not found:
        raise Unreadable(f"{script} runs no `<binary> -test.run '<pattern>'` this search reads")
    return found


# ------------------------------------------------------------------------- rule 1: build tags
def tag_findings(tree: GoTree, runs: list[GoRun]) -> list[Finding]:
    found = []
    for go_file in tree.files.values():
        if not go_file.is_test or go_file.constraint is None:
            continue
        tags = [name for name in constraint_names(go_file.constraint) if is_tag(name, (GOOS, GOARCH))]
        if not tags:
            continue
        if any(run.builds(go_file) for run in runs):
            continue
        found.append(Finding("tag", go_file.path, go_file.constraint_line, ",".join(dict.fromkeys(tags)),
                             f"no required step's go test builds {go_file.directory} with "
                             f"`//go:build {go_file.constraint}` true"))
    return found


# ------------------------------------------------------------------------------- rule 2: -skip
def skip_findings(tree: GoTree, runs: list[GoRun]) -> list[Finding]:
    found = []
    for run in runs:
        for directory in run.directories:
            for go_file in run.files(tree, directory):
                for name, line in go_file.tests:
                    if name not in run.skip:
                        continue
                    if any(other.step is not run.step and other.builds(go_file) and other.runs(name)
                           for other in runs):
                        continue
                    found.append(Finding("skip", go_file.path, line, name,
                                         f"{run.step.label} skips it and no other required step runs it"))
    return found


# ------------------------------------------------------------------------------ rule 6: unrun
def race_readers(tree: GoTree) -> set[tuple[str, str]]:
    """The (file, test) pairs whose test function reads raceEnabled: a test that changes what it
    does under the race detector, so a -race step does not run it whole."""
    found = set()
    for go_file in tree.files.values():
        if not go_file.is_test or "raceEnabled" not in go_file.text:
            continue
        for function in go_functions(go_file, go_tokens(go_file.text)):
            if not function.receiver and any(t.kind == "ident" and t.text == "raceEnabled" for t in function.body):
                found.add((go_file.path, function.name))
    return found


def unrun_findings(tree: GoTree, runs: list[GoRun], reported: set) -> list[Finding]:
    """Each test function (Test, Fuzz, Example) that no required step's go test runs: no step's
    packages hold its package, no step that runs its package builds its file under that step's
    GOOS, GOARCH, -tags and -race, or each step that builds it leaves it out by its -run or
    -skip. A test that reads raceEnabled counts as run only by a step without -race. A file the
    tag rule names, and a test the skip rule names, are theirs (`reported`)."""
    by_directory: dict[str, list[GoRun]] = defaultdict(list)
    for run in runs:
        for directory in run.directory_set:
            by_directory[directory].append(run)
    readers = race_readers(tree)
    found = []
    for go_file in tree.files.values():
        if not go_file.is_test or go_file.path in reported:
            continue
        candidates = by_directory.get(go_file.directory, [])
        building = [run for run in candidates if run.builds(go_file)]
        for name, line in go_file.tests:
            if name.startswith("Benchmark") or (go_file.path, name) in reported:
                continue
            race_impossible = (go_file.path, name) in readers
            if any(run.runs(name) and not (race_impossible and run.race) for run in building):
                continue
            found.append(Finding("unrun", go_file.path, line, name,
                                 unrun_reason(go_file, name, candidates, building, race_impossible)))
    return found


def unrun_reason(go_file: GoFile, name: str, candidates: list[GoRun], building: list[GoRun],
                 race_impossible: bool) -> str:
    def labels(runs: list[GoRun]) -> str:
        named = sorted({run.step.label for run in runs})
        return ", ".join(named[:3]) + (f" and {len(named) - 3} more" if len(named) > 3 else "")

    if not candidates:
        return f"no required step's go test runs its package, {go_file.directory}"
    if not building:
        shape = (f"`//go:build {go_file.constraint}`" if go_file.constraint is not None
                 else f"its file name's `{Path(go_file.path).stem.removesuffix('_test').rsplit('_', 1)[-1]}`")
        return (f"{shape} holds for none of the GOOS, GOARCH, -tags and -race of the required steps that run "
                f"its package ({labels(candidates)}), and a go vet or a build is no run")
    if race_impossible and any(run.runs(name) for run in building):
        return (f"it reads raceEnabled, so only a step without -race runs it whole, and each required step "
                f"that runs it ({labels([run for run in building if run.runs(name)])}) runs it under -race")
    kept_out = " (its `//go:build " + go_file.constraint + "` keeps it out of each -race build)" if (
        go_file.constraint and "race" in constraint_names(go_file.constraint)) else ""
    return f"each required step that builds it{kept_out} ({labels(building)}) leaves it out by its -run or -skip"


def go_runners(tree: GoTree, runs: list[GoRun], go_file: GoFile, test: str | None) -> list[GoRun]:
    """The required go tests that run `test` of `go_file`, or any test of its package (and of each
    package whose tests import it, for a helper package) where `test` is None."""
    if test is not None:
        return [run for run in runs if run.builds(go_file) and run.runs(test)]
    directories = {go_file.directory}
    if not go_file.is_test:
        target = tree.import_path(go_file.directory)
        for other in tree.files.values():
            if other.is_test and target and f'"{target}"' in other.text:
                directories.add(other.directory)
    found = []
    for run in runs:
        for directory in directories & run.directory_set:
            if any(run.runs(name) for f in run.files(tree, directory) for name, _ in f.tests):
                found.append(run)
                break
    return found


# ------------------------------------------------------------------------- rule 3: Go gates
GO_TOKEN = re.compile(r"""
    (?P<nl>\n)
  | (?P<ws>[ \t\r\f]+)
  | (?P<comment>//[^\n]*|/\*.*?\*/)
  | (?P<raw>`[^`]*`)
  | (?P<str>"(?:[^"\\\n]|\\.)*")
  | (?P<rune>'(?:[^'\\\n]|\\.)*')
  | (?P<ident>[A-Za-z_][A-Za-z0-9_]*)
  | (?P<num>[0-9][A-Za-z0-9_.]*)
  | (?P<op>:=|==|!=|<=|>=|&&|\|\||<-|\+\+|--|\.\.\.|[-+*/%&|^<>=!(){}\[\],;.:~])
  | (?P<other>.)
""", re.S | re.X)


@dataclass(frozen=True)
class Tok:
    kind: str
    text: str
    line: int


def go_tokens(text: str) -> list[Tok]:
    tokens, line = [], 1
    for match in GO_TOKEN.finditer(text):
        kind = match.lastgroup
        value = match.group()
        if kind == "nl" or (kind == "comment" and "\n" in value):
            tokens.append(Tok("nl", "\n", line))
        elif kind not in ("ws", "comment", "other"):
            tokens.append(Tok(kind, value, line))
        if kind in ("nl", "comment", "raw"):
            line += value.count("\n")
    return tokens


def go_string(text: str) -> str | None:
    if text.startswith('"') and text.endswith('"') and len(text) >= 2:
        try:
            return json.loads(text)
        except ValueError:
            return text[1:-1]
    if text.startswith("`") and text.endswith("`") and len(text) >= 2:
        return text[1:-1]
    return None


CLOSING = {"(": ")", "[": "]", "{": "}"}


def match_close(tokens: list[Tok], start: int) -> int:
    """The index of the bracket that closes tokens[start]."""
    opening = tokens[start].text
    closing = CLOSING[opening]
    depth = 0
    for index in range(start, len(tokens)):
        text = tokens[index].text
        if text == opening:
            depth += 1
        elif text == closing:
            depth -= 1
            if depth == 0:
                return index
    return len(tokens) - 1


@dataclass(eq=False)
class GoFunc:
    file: GoFile
    name: str  # the function's name (methods: the method's)
    receiver: bool
    params: list[str]
    line: int
    body: list[Tok]
    results: list[str]  # the result type's tokens


def go_params(tokens: list[Tok]) -> list[str]:
    parts = split_commas(tokens)
    named = any(len(part) >= 2 and part[0].kind == "ident" and part[1].text != "." for part in parts)
    return [part[0].text if part and part[0].kind == "ident" else "" for part in parts] if named else []


def go_functions(go_file: GoFile, tokens: list[Tok]) -> list[GoFunc]:
    found, index, depth = [], 0, 0
    while index < len(tokens):
        token = tokens[index]
        if token.text in ("(", "{", "["):
            depth += 1
        elif token.text in (")", "}", "]"):
            depth -= 1
        elif token.text == "func" and depth == 0 and token.kind == "ident":
            cursor, receiver = index + 1, False
            if cursor < len(tokens) and tokens[cursor].text == "(":
                receiver = True
                cursor = match_close(tokens, cursor) + 1
            if cursor + 1 < len(tokens) and tokens[cursor].kind == "ident":
                name = tokens[cursor].text
                cursor += 1
                if tokens[cursor].text == "[":  # type parameters
                    cursor = match_close(tokens, cursor) + 1
                if cursor < len(tokens) and tokens[cursor].text == "(":
                    close = match_close(tokens, cursor)
                    params = go_params(tokens[cursor + 1:close])
                    body_start = close + 1
                    while (body_start < len(tokens) and tokens[body_start].text != "{"
                           and tokens[body_start].kind != "nl"):
                        if tokens[body_start].text in ("(", "["):
                            body_start = match_close(tokens, body_start)
                        body_start += 1
                    if body_start < len(tokens) and tokens[body_start].text == "{":
                        body_end = match_close(tokens, body_start)
                        found.append(GoFunc(go_file, name, receiver, params, token.line,
                                            tokens[body_start + 1:body_end],
                                            [t.text for t in tokens[close + 1:body_start] if t.kind != "nl"]))
                        index = body_end + 1
                        continue
        index += 1
    return found


GO_STRING = r'"(?:[^"\\\n]|\\.)*"|`[^`]*`'
CONST_SINGLE = re.compile(rf"^\s*(?:const|var)\s+([A-Za-z_]\w*)\s*(?:[A-Za-z_][\w.]*\s*)?=\s*({GO_STRING})\s*(?://.*)?$", re.M)
CONST_BLOCK = re.compile(r"^\s*(?:const|var)\s*\((.*?)^\s*\)", re.S | re.M)
CONST_ENTRY = re.compile(rf"^\s*([A-Za-z_]\w*)\s*(?:[A-Za-z_][\w.]*\s*)?=\s*({GO_STRING})\s*(?://.*)?$", re.M)
IMPORT_LINE = re.compile(r'^\s*(?:([A-Za-z_][A-Za-z0-9_]*|\.)\s+)?"([^"]+)"\s*$')
# What a package's own code hands a child process's environment: a whole `"NAME=value"` string,
# `NAME + "=…"`, and a `"%s=…"` format given NAME. An entry holds no space, so a message that
# names a variable and the value that turns it on (`t.Skip("runs only where " + NAME + "=1: …")`,
# launcher's TestControllerPodMemoryGuardLiveSandbox) hands no child anything and hides no gate.
CHILD_LITERAL = re.compile(r'"([A-Z_][A-Z0-9_]*)=[^"\s%]*"')
CHILD_CONCAT = re.compile(r'\b((?:[A-Za-z_]\w*\.)?[A-Za-z_]\w*)\s*\+\s*"=[^"\s]*"')
CHILD_FORMAT = re.compile(r'"%s=[^"\s]*"\s*,\s*((?:[A-Za-z_]\w*\.)?[A-Za-z_]\w*)')
ENV_READ_TEXT = re.compile(r"\bos\.(?:Getenv|LookupEnv)\(|\btestprereq\.Require\(")
PACKAGE_VAR = re.compile(r"^var\s+([A-Za-z_]\w*)", re.M)
PACKAGE_VAR_BLOCK = re.compile(r"^var\s*\((.*?)^\)", re.S | re.M)
PACKAGE_VAR_ENTRY = re.compile(r"^\s*([A-Za-z_]\w*)", re.M)
# A test a test of its own package starts again as a child process, `-test.run=^TestXHelper$`:
# what it reads is what its parent hands the child.
RERUN = re.compile(r"-test\.run[= ]\W{0,3}(Test[A-Za-z0-9_]+)")


def go_constants(text: str) -> dict[str, str]:
    found = {}
    for match in CONST_SINGLE.finditer(text):
        found[match.group(1)] = go_string(match.group(2))
    for block in CONST_BLOCK.finditer(text):
        for match in CONST_ENTRY.finditer(block.group(1)):
            found[match.group(1)] = go_string(match.group(2))
    return found


def go_imports(text: str) -> dict[str, str]:
    found = {}
    block = re.search(r"^import\s*\((.*?)^\)", text, re.S | re.M)
    lines = block.group(1).splitlines() if block else []
    lines += [m.group(1) for m in re.finditer(r"^import\s+((?:[A-Za-z_]\w*\s+)?\"[^\"]+\")", text, re.M)]
    for line in lines:
        match = IMPORT_LINE.match(line.split("//")[0])
        if match:
            alias = match.group(1) or match.group(2).rsplit("/", 1)[-1]
            found[alias] = match.group(2)
    return found


SKIP_CALLS = frozenset({"Skip", "Skipf", "SkipNow"})
ASSERT_CALLS = frozenset({"Error", "Errorf", "Fatal", "Fatalf", "Fail", "FailNow"})
ENV_READS = ("Getenv", "LookupEnv")
# The calls whose result still holds the value a read gave them: they reshape it (trim, parse,
# clean a path) or ask after what it names (a file, a program), and do nothing else with it.
VALUE_BUILTINS = frozenset({"len", "string", "bool", "int"})
VALUE_PACKAGES = frozenset({"strings", "strconv", "path/filepath", "path", "time", "net/url"})
VALUE_CALLS = frozenset({("os", "Stat"), ("os", "Lstat"), ("os/exec", "LookPath")})


@dataclass
class Gate:
    language: str
    path: str
    line: int
    variables: frozenset[str]
    file: GoFile | None  # a Go gate's file
    test: str | None  # a Go gate's Test function, None for a helper's gate
    prerequisite: bool  # calls testprereq.Require, requireOrSkip or skipWithout
    fails_on_actions: bool  # reads GITHUB_ACTIONS and fails there
    tool: bool  # in a Benchmark, or in a file that carries the tool line


@dataclass
class HelperGate:
    param: int
    prerequisite: bool
    fails_on_actions: bool


class GoGates:
    """Every variable gate of the Go tree. Only the files that read a variable, call
    testprereq.Require, or call a function that gates on or returns a read are read token by
    token; every package's constants, imports and child-process variables are read off its text."""

    def __init__(self, tree: GoTree):
        self.tree = tree
        self.constants: dict[str, dict[str, str]] = {}
        self.imports: dict[str, dict[str, str]] = {}
        for directory, files in tree.by_directory.items():
            constants: dict[str, str] = {}
            for go_file in files:
                constants.update(go_constants(go_file.text))
                self.imports[go_file.path] = go_imports(go_file.text)
            self.constants[directory] = constants
        self._child: dict[str, set[str]] = {}
        self._main: dict[str, set[str]] = {}
        self._package_taint: dict[str, dict[str, set]] = {}
        self._reruns: dict[str, set[str]] = {}
        self._functions: dict[str, list[GoFunc]] = {}
        self._taint: dict[GoFunc, dict[str, set]] = {}
        self._gates: dict[GoFunc, list] = {}
        self.helpers: dict[tuple[str, str], list[HelperGate]] = defaultdict(list)
        self.returning: dict[tuple[str, str], frozenset[str]] = {}
        self.gates: list[Gate] = []

    def functions(self, go_file: GoFile) -> list[GoFunc]:
        if go_file.path not in self._functions:
            self._functions[go_file.path] = go_functions(go_file, go_tokens(go_file.text))
        return self._functions[go_file.path]

    def constant(self, directory: str, path: str, spelled: str) -> str | None:
        """The string a literal, a name or a `pkg.Name` names, through a constant of its own
        package or of the package an import names."""
        literal = go_string(spelled)
        if literal is not None:
            return literal
        if "." not in spelled:
            return self.constants.get(directory, {}).get(spelled)
        alias, _, name = spelled.partition(".")
        imported = self.imports.get(path, {}).get(alias)
        target = self.tree.directory_of(imported) if imported else None
        return self.constants.get(target, {}).get(name) if target else None

    def child_env(self, directory: str) -> set[str]:
        if directory not in self._child:
            names = set()
            for go_file in self.tree.by_directory.get(directory, []):
                text = go_file.text
                if '=' not in text:
                    continue
                names.update(CHILD_LITERAL.findall(text))
                for marker, pattern in (('"=', CHILD_CONCAT), ('"%s=', CHILD_FORMAT)):
                    if marker not in text:
                        continue
                    for spelled in pattern.findall(text):
                        name = self.constant(directory, go_file.path, spelled)
                        if name:
                            names.add(name)
            self._child[directory] = names
        return self._child[directory]

    def main_env(self, directory: str) -> set[str]:
        """The variables a TestMain of the package sets for its whole test process, by
        `os.Setenv(NAME, …)` with NAME a literal or a constant."""
        if directory not in self._main:
            names = set()
            for go_file in self.tree.by_directory.get(directory, []):
                if not go_file.is_test or "TestMain" not in go_file.text:
                    continue
                for function in self.functions(go_file):
                    if function.name != "TestMain" or function.receiver:
                        continue
                    tokens = [t for t in function.body if t.kind != "nl"]
                    for index in range(len(tokens) - 3):
                        if not (tokens[index].text == "os" and tokens[index + 1].text == "."
                                and tokens[index + 2].text == "Setenv" and tokens[index + 3].text == "("):
                            continue
                        arguments = split_commas(tokens[index + 4:match_close(tokens, index + 3)])
                        name = self.constant(directory, go_file.path, self.spelled(arguments[0])) if arguments else None
                        if name:
                            names.add(name)
            self._main[directory] = names
        return self._main[directory]

    def spelled(self, tokens: list[Tok]) -> str:
        return "".join(t.text for t in tokens if t.kind != "nl")

    def read(self, function: GoFunc, tokens: list[Tok], index: int) -> tuple[set, int] | None:
        """At `os.Getenv(`/`os.LookupEnv(` the variables it reads and the index past it."""
        if not (tokens[index].text == "os" and index + 3 < len(tokens) and tokens[index + 1].text == "."
                and tokens[index + 2].text in ENV_READS and tokens[index + 3].text == "("):
            return None
        close = match_close(tokens, index + 3)
        argument = self.spelled(tokens[index + 4:close])
        name = self.constant(function.file.directory, function.file.path, argument)
        if name is not None:
            return {name}, close + 1
        if argument in function.params:
            return {("param", function.params.index(argument))}, close + 1
        return set(), close + 1

    def reads_in(self, function: GoFunc, tokens: list[Tok], tainted: dict) -> set:
        """The variables an expression holds: a read in it, a local that holds one, a call of a
        function that returns one, each at its own level or inside a call that only reshapes the
        value (VALUE_CALLS: strings.TrimSpace, strconv.Atoi, os.Stat, …). An argument of any other
        call is that call's, and the result holds what the call made of it, not the read: a store
        opened on a URL a variable names holds no variable."""
        found = set()
        index = 0
        imports = self.imports.get(function.file.path, {})
        while index < len(tokens):
            read = self.read(function, tokens, index)
            if read is not None:
                found |= read[0]
                index = read[1]
                continue
            token = tokens[index]
            if token.kind == "ident":
                after_dot = index > 0 and tokens[index - 1].text == "."
                calls = index + 1 < len(tokens) and tokens[index + 1].text == "("
                if token.text in tainted and not after_dot and not calls:
                    found |= tainted[token.text]
                if calls and not after_dot:
                    found |= self.returning.get((function.file.directory, token.text), frozenset())
                    if token.text not in VALUE_BUILTINS:
                        index = match_close(tokens, index + 1) + 1
                        continue
                if (index + 3 < len(tokens) and tokens[index + 1].text == "." and tokens[index + 3].text == "("
                        and not after_dot):
                    qualified = (imports.get(token.text, token.text), tokens[index + 2].text)
                    if token.text in imports:
                        target = self.tree.directory_of(imports[token.text])
                        if target:
                            found |= self.returning.get((target, tokens[index + 2].text), frozenset())
                    if token.text in tainted:
                        found |= tainted[token.text]
                    if qualified not in VALUE_CALLS and qualified[0] not in VALUE_PACKAGES:
                        index = match_close(tokens, index + 3) + 1
                        continue
                    index += 4
                    continue
            index += 1
        return found

    def taint(self, function: GoFunc) -> dict[str, set]:
        """The names that hold a read, with the variables each holds: the function's own locals,
        and the package-level variables a function of its package assigns a read to (a snapshot
        TestMain takes)."""
        if function not in self._taint:
            tainted = {name: set(held) for name, held in self.package_taint(function.file.directory).items()}
            self._taint[function] = self.local_taint(function, tainted)
        return self._taint[function]

    def local_taint(self, function: GoFunc, tainted: dict[str, set]) -> dict[str, set]:
        statements = split_statements(function.body)
        for _ in range(3):
            for statement in statements:
                assign = next((i for i, t in enumerate(statement) if t.text in (":=", "=")), None)
                if assign is None:
                    continue
                left = statement[:assign]
                if left and left[0].text == "var":
                    left = left[1:2]
                elif left and left[0].text in ("if", "for", "switch"):
                    left = left[1:]
                roots = []
                for operand in split_commas(left):
                    # `x` or a field of it, `x.y.z`: the root is what holds the read.
                    if (operand and operand[0].kind == "ident"
                            and all(t.text == "." if i % 2 else t.kind == "ident" for i, t in enumerate(operand))):
                        roots.append(operand[0].text)
                    else:
                        roots = []
                        break
                if not roots:
                    continue
                held = self.reads_in(function, statement[assign + 1:], tainted)
                for root in roots:
                    if held and root not in ("_", "err"):
                        tainted.setdefault(root, set()).update(held)
        return tainted

    def package_taint(self, directory: str) -> dict[str, set]:
        if directory not in self._package_taint:
            self._package_taint[directory] = {}
            globals_ = set()
            for go_file in self.tree.by_directory.get(directory, []):
                globals_ |= set(PACKAGE_VAR.findall(go_file.text))
                for block in PACKAGE_VAR_BLOCK.finditer(go_file.text):
                    globals_ |= set(PACKAGE_VAR_ENTRY.findall(block.group(1)))
            found: dict[str, set] = {}
            for go_file in self.tree.by_directory.get(directory, []):
                if go_file.path not in self._functions:
                    continue
                for function in self._functions[go_file.path]:
                    for name, held in self.local_taint(function, {}).items():
                        if name in globals_:
                            found.setdefault(name, set()).update(held)
            self._package_taint[directory] = found
        return self._package_taint[directory]

    def analyse(self) -> None:
        """Functions that return a plain value read off a variable (a bool, a string, a number: a
        `liveEnabled() bool`), then helpers that gate on a parameter, then every gate: two passes
        over the files that read a variable, so a function another file or another package calls
        is known before its callers are read. A function returning anything else - a store, a
        server - carries no variable to its callers: what it does with the read is a gate of its
        own, read where it is."""
        testing = [f for f in self.tree.files.values() if f.is_test or "testing" in self.imports[f.path].values()]
        readers = [f for f in testing if ENV_READ_TEXT.search(f.text)]
        everything = [function for go_file in readers for function in self.functions(go_file)]
        for _ in range(2):
            self._taint.clear()
            for function in everything:
                if function.receiver or not plain_result(function.results):
                    continue
                tainted = self.taint(function)
                returned = set()
                for statement in split_statements(function.body):
                    if statement and statement[0].text == "return":
                        returned |= self.reads_in(function, statement[1:], tainted)
                returned = {v for v in returned if isinstance(v, str)}
                if returned:
                    self.returning[(function.file.directory, function.name)] = frozenset(returned)
        self._taint.clear()
        for function in everything:
            for variables, _, prerequisite, fails in self.function_gates(function):
                for value in variables:
                    if isinstance(value, tuple):
                        self.helpers[(function.file.directory, function.name)].append(
                            HelperGate(value[1], prerequisite, fails))
        # The files that can call such a function: those of its own package that name it, and
        # those that import its package and name it.
        targets = {f.path: f for f in readers}
        by_package: dict[str, set[str]] = defaultdict(set)
        for directory, name in set(self.helpers) | set(self.returning):
            by_package[directory].add(name)
        for directory, names in by_package.items():
            calling = re.compile(r"\b(?:" + "|".join(sorted(map(re.escape, names))) + r")\s*\(")
            path = self.tree.import_path(directory)
            for go_file in testing:
                if go_file.directory != directory:
                    continue
                if calling.search(go_file.text):
                    targets[go_file.path] = go_file
            if path:
                for go_file in testing:
                    if f'"{path}"' in go_file.text and calling.search(go_file.text):
                        targets[go_file.path] = go_file
        for go_file in targets.values():
            for function in self.functions(go_file):
                for variables, line, prerequisite, fails in self.function_gates(function):
                    names = frozenset(v for v in variables if isinstance(v, str))
                    if names:
                        self.add(function, line, names, prerequisite, fails)
                self.helper_calls(function)

    def reruns(self, directory: str) -> set[str]:
        if directory not in self._reruns:
            self._reruns[directory] = {name for go_file in self.tree.by_directory.get(directory, [])
                                       for name in RERUN.findall(go_file.text)}
        return self._reruns[directory]

    def add(self, function: GoFunc, line: int, names: frozenset[str], prerequisite: bool, fails: bool) -> None:
        go_file = function.file
        if function.name in self.reruns(go_file.directory):
            return  # a helper process its own package's test starts, with the environment it hands it
        names = frozenset(n for n in names
                          if n not in self.child_env(go_file.directory) and n not in self.main_env(go_file.directory)
                          and n not in self.own_env(function))
        if not names:
            return
        test = function.name if (go_file.is_test and not function.receiver
                                 and re.match(r"^(Test|Fuzz|Example|Benchmark)", function.name)
                                 and function.name != "TestMain") else None
        self.gates.append(Gate("go", go_file.path, line, names, go_file, test, prerequisite,
                               fails and "GITHUB_ACTIONS" in names,
                               function.name.startswith("Benchmark") or go_file.tool))

    def setenv_names(self, function: GoFunc) -> set[str]:
        """The variables a function's own body sets with `<t>.Setenv(NAME, …)` or `os.Setenv(NAME,
        …)`, NAME a literal or a constant."""
        tokens = [t for t in function.body if t.kind != "nl"]
        names = set()
        for index in range(1, len(tokens) - 2):
            if not (tokens[index].text == "." and tokens[index + 1].text == "Setenv" and tokens[index + 2].text == "("):
                continue
            arguments = split_commas(tokens[index + 3:match_close(tokens, index + 2)])
            name = self.constant(function.file.directory, function.file.path, self.spelled(arguments[0])) if arguments else None
            if name:
                names.add(name)
        return names

    def own_env(self, function: GoFunc) -> set[str]:
        """The variables a test sets for itself before it reads them: those its own body sets, and
        those a function of its package it calls by name sets in its body (a fixture that writes a
        credentials file and points the variable at it with t.Setenv). The read is of the test's own
        value, so it gates on nothing a job must give."""
        names = self.setenv_names(function)
        called = {t.text for i, t in enumerate(function.body[:-1])
                  if t.kind == "ident" and function.body[i + 1].text == "(" and (i == 0 or function.body[i - 1].text != ".")}
        for go_file in self.tree.by_directory.get(function.file.directory, []):
            if ".Setenv(" not in go_file.text:
                continue
            for other in self.functions(go_file):
                if other.name in called and not other.receiver:
                    names |= self.setenv_names(other)
        return names

    def helper_calls(self, function: GoFunc) -> None:
        tokens = [t for t in function.body if t.kind != "nl"]
        imports = self.imports.get(function.file.path, {})
        for index, token in enumerate(tokens):
            if token.kind != "ident" or index + 1 >= len(tokens):
                continue
            key, open_at = None, None
            if tokens[index + 1].text == "(" and (index == 0 or tokens[index - 1].text != "."):
                key, open_at = (function.file.directory, token.text), index + 1
            elif (tokens[index + 1].text == "." and index + 3 < len(tokens) and tokens[index + 3].text == "("
                  and token.text in imports):
                key, open_at = (self.tree.directory_of(imports[token.text]), tokens[index + 2].text), index + 3
            if key is None or key not in self.helpers:
                continue
            arguments = split_commas(tokens[open_at + 1:match_close(tokens, open_at)])
            for helper in self.helpers[key]:
                if helper.param >= len(arguments):
                    continue
                name = self.constant(function.file.directory, function.file.path, self.spelled(arguments[helper.param]))
                if name is not None:
                    self.add(function, token.line, frozenset({name}), helper.prerequisite, helper.fails_on_actions)

    def function_gates(self, function: GoFunc) -> list[tuple[set, int, bool, bool]]:
        """(variables, line, prerequisite, fails there) of each gate in a function's body: an `if`
        whose condition holds a read and one of whose branches skips, asserts or calls
        testprereq.Require; a `switch` case the same; and a testprereq.Require whose condition
        holds a read."""
        if function in self._gates:
            return self._gates[function]
        tainted = self.taint(function)
        tokens = function.body
        found = []
        packages = frozenset(self.imports.get(function.file.path, {}))
        constants = self.constants.get(function.file.directory, {})
        fails = bool(calls_in(tokens, packages) & ASSERT_CALLS)
        index = 0
        while index < len(tokens):
            token = tokens[index]
            if token.kind == "ident" and token.text == "if":
                header_end = header_close(tokens, index + 1)
                if header_end >= len(tokens):
                    break
                close = match_close(tokens, header_end)
                branches = [tokens[header_end + 1:close]]
                after = close + 1
                if after + 1 < len(tokens) and tokens[after].text == "else" and tokens[after + 1].text == "{":
                    branches.append(tokens[after + 2:match_close(tokens, after + 1)])
                header = tokens[index + 1:header_end]
                variables = self.reads_in(function, header, tainted)
                if variables:
                    calls = set().union(*(self.branch_calls(function, branch, packages, tainted) for branch in branches))
                    if (calls & (SKIP_CALLS | {"Require"}) or (
                            calls & ASSERT_CALLS
                            and not self.checks_the_environment(header, branches, tainted, packages, constants))):
                        found.append((variables, token.line, "Require" in calls, fails))
            elif token.kind == "ident" and token.text == "switch":
                open_at = header_close(tokens, index + 1)
                if open_at >= len(tokens):
                    break
                tag_variables = self.reads_in(function, tokens[index + 1:open_at], tainted)
                for case_header, case_body in switch_cases(tokens[open_at + 1:match_close(tokens, open_at)]):
                    variables = tag_variables | self.reads_in(function, case_header, tainted)
                    calls = self.branch_calls(function, case_body, packages, tainted)
                    if variables and (calls & (SKIP_CALLS | {"Require"}) or (
                            calls & ASSERT_CALLS and not self.checks_the_environment(
                                tokens[index + 1:open_at] + case_header, [case_body], tainted, packages, constants))):
                        found.append((variables, token.line, "Require" in calls, fails))
            elif (token.text == "testprereq" and index + 3 < len(tokens) and tokens[index + 1].text == "."
                    and tokens[index + 2].text == "Require" and tokens[index + 3].text == "("):
                arguments = split_commas(tokens[index + 4:match_close(tokens, index + 3)])
                if len(arguments) >= 2:
                    variables = self.reads_in(function, arguments[1], tainted)
                    if variables:
                        found.append((variables, token.line, True, False))
            index += 1
        self._gates[function] = found
        return found

    def checks_the_environment(self, condition: list[Tok], branches: list[list[Tok]], tainted: dict,
                               packages: frozenset, constants: dict) -> bool:
        """Whether an `if` (or a `case`) is itself an assertion about the environment rather than a
        gate on one: its condition names nothing but reads, the names that hold them, an error
        and constants, and its branches do nothing but assert (and return). `if _, set :=
        os.LookupEnv(X); set { t.Fatal(…) }` checks that X is unset in every run, and `if err !=
        nil || n < 1 { t.Fatalf(…) }` after `n, err := strconv.Atoi(os.Getenv(X))` that its value
        is a count; `if os.Getenv(X) == "1" && elapsed > bound { t.Errorf(…) }` asserts on elapsed
        only where X is set, which is a gate."""
        tokens = [t for t in condition if t.kind != "nl"]
        index = 0
        while index < len(tokens):
            token = tokens[index]
            if token.text == "os" and index + 2 < len(tokens) and tokens[index + 2].text in ENV_READS:
                index = match_close(tokens, index + 3) + 1
                continue
            if token.kind == "ident" and not (index > 0 and tokens[index - 1].text == "."):
                if token.text in packages and index + 1 < len(tokens) and tokens[index + 1].text == ".":
                    index += 1
                    continue
                if token.text not in tainted and token.text not in GO_NEUTRAL_WORDS and token.text not in constants:
                    return False
            index += 1
        for branch in branches:
            for statement in split_statements(branch):
                if statement[0].text == "return":
                    continue
                if not (len(statement) >= 4 and statement[0].kind == "ident" and statement[1].text == "."
                        and statement[2].text in ASSERT_CALLS and statement[3].text == "("
                        and match_close(statement, 3) == len(statement) - 1):
                    return False
        return True

    def branch_calls(self, function: GoFunc, branch: list[Tok], packages: frozenset, tainted: dict) -> set[str]:
        """The calls a guarded block makes (calls_in), less those of an `if` inside it that only
        checks an operation's error (`if err != nil`, `if err := os.WriteFile(out, …); err != nil`)
        or checks the variable's own value (checks_the_environment): such a check is the block's own
        reading or writing of what the variable names, and asserts nothing of what the test
        proves."""
        constants = self.constants.get(function.file.directory, {})
        kept: list[Tok] = []
        index = 0
        while index < len(branch):
            token = branch[index]
            if token.kind == "ident" and token.text == "if":
                header_end = header_close(branch, index + 1)
                if header_end < len(branch):
                    header = [t for t in branch[index + 1:header_end] if t.kind != "nl"]
                    last = [t.text for t in header]
                    last = last[len(last) - 1 - last[::-1].index(";") + 1:] if ";" in last else last
                    close = match_close(branch, header_end)
                    if last == ["err", "!=", "nil"] or self.checks_the_environment(
                            header, [branch[header_end + 1:close]], tainted, packages, constants):
                        index = close + 1
                        continue
            kept.append(token)
            index += 1
        return calls_in(kept, packages)


GO_NEUTRAL_WORDS = frozenset({"nil", "true", "false", "_", "len", "iota", "err", "ok"})


def header_close(tokens: list[Tok], index: int) -> int:
    """The index of the `{` that opens the block of an `if` or `switch` whose header starts at
    index: the first one outside parentheses and brackets."""
    depth = 0
    while index < len(tokens):
        text = tokens[index].text
        if text in ("(", "["):
            depth += 1
        elif text in (")", "]"):
            depth -= 1
        elif text == "{" and depth == 0:
            return index
        index += 1
    return index


PLAIN_TYPES = frozenset({"bool", "string", "int", "int64", "uint64", "float64"})


def plain_result(results: list[str]) -> bool:
    names = [text for text in results if text not in ("(", ")", ",")]
    return bool(names) and all(name in PLAIN_TYPES for name in names)


def split_statements(tokens: list[Tok]) -> list[list[Tok]]:
    """The statements of a block, a function literal's body inside a call split into its own
    statements too (`t.Cleanup(func() { v, set := os.LookupEnv(X); … })`)."""
    statements, current, depth, outer = [], [], 0, []
    for token in tokens:
        text = token.text
        if text in ("(", "["):
            depth += 1
        elif text in (")", "]"):
            depth -= 1
        elif text == "{":
            outer.append(depth)
            depth = 0
        elif text == "}":
            depth = outer.pop() if outer else 0
        if text in ("{", "}") or (depth == 0 and (token.kind == "nl" or text == ";")):
            if current and token.kind == "nl" and current[-1].text in (",", "+", "&&", "||", "(", "=", ":="):
                continue
            if current:
                statements.append(current)
            current = []
            continue
        if token.kind != "nl":
            current.append(token)
    if current:
        statements.append(current)
    return statements


def split_commas(tokens: list[Tok]) -> list[list[Tok]]:
    parts, current, depth = [], [], 0
    for token in tokens:
        if token.kind == "nl":
            continue
        if token.text in ("(", "[", "{"):
            depth += 1
        elif token.text in (")", "]", "}"):
            depth -= 1
        if token.text == "," and depth == 0:
            parts.append(current)
            current = []
        else:
            current.append(token)
    if current:
        parts.append(current)
    return parts


def switch_cases(tokens: list[Tok]) -> list[tuple[list[Tok], list[Tok]]]:
    starts, colons, depth, pending = [], {}, 0, None
    for index, token in enumerate(tokens):
        text = token.text
        if text in ("(", "[", "{"):
            depth += 1
        elif text in (")", "]", "}"):
            depth -= 1
        elif depth == 0 and text in ("case", "default"):
            starts.append(index)
            pending = index
        elif depth == 0 and text == ":" and pending is not None:
            colons[pending] = index
            pending = None
    cases = []
    for number, start in enumerate(starts):
        end = starts[number + 1] if number + 1 < len(starts) else len(tokens)
        colon = colons.get(start, end)
        cases.append((tokens[start + 1:colon], tokens[colon + 1:end]))
    return cases


def calls_in(tokens: list[Tok], packages: frozenset = frozenset()) -> set[str]:
    """The method names `x.Name(` calls in tokens, `x` no imported package (`fmt.Errorf` is no
    assertion) and `x.Error()` with no argument (an error's text) left out."""
    found = set()
    for index, token in enumerate(tokens):
        if (token.text == "." and index + 2 < len(tokens) and tokens[index + 1].kind == "ident"
                and tokens[index + 2].text == "(" and not (index > 0 and tokens[index - 1].text in packages)
                and not (tokens[index + 1].text == "Error" and index + 3 < len(tokens)
                         and tokens[index + 3].text == ")")):
            found.add(tokens[index + 1].text)
    return found


# ------------------------------------------------------------------------ the Bun and Playwright tree
BUN_TEST = re.compile(r"(\.test|_test|\.spec|_spec)\.(ts|tsx|js|jsx|mjs|cjs|mts|cts)$")
SPEC = re.compile(r"\.(e2e|spec|test)\.(ts|tsx|js|mjs)$")
TS_SOURCE = re.compile(r"\.(ts|tsx|js|jsx|mjs|cjs|mts|cts)$")
TS_LEXEME = re.compile(r"\"(?:[^\"\\\n]|\\.)*\"|'(?:[^'\\\n]|\\.)*'|`(?:[^`\\]|\\.)*`|//[^\n]*|/\*.*?\*/", re.S)


def strip_comments(text: str) -> str:
    """The text with its // and /* */ comments blanked, strings and template literals kept, and
    each line where it was."""
    return TS_LEXEME.sub(lambda m: m.group() if m.group()[0] in "\"'`" else "\n" * m.group().count("\n"), text)


def balanced(text: str, start: int) -> int:
    """The index just past the bracket that closes text[start], strings skipped."""
    pairs = {"(": ")", "[": "]", "{": "}"}
    stack, index = [], start
    while index < len(text):
        char = text[index]
        if char in "\"'`":
            end = index + 1
            while end < len(text) and text[end] != char:
                end += 2 if text[end] == "\\" else 1
            index = end + 1
            continue
        if char in pairs:
            stack.append(pairs[char])
        elif char in ")]}":
            if stack and stack[-1] == char:
                stack.pop()
                if not stack:
                    return index + 1
        index += 1
    return len(text)


TS_ENV = re.compile(r"""(?:process\.env|Bun\.env|import\.meta\.env)(?:\??\.([A-Za-z_][A-Za-z0-9_]*)|\[\s*["']([A-Za-z_][A-Za-z0-9_]*)["']\s*\])""")
TS_DESTRUCTURE = re.compile(r"(?:const|let|var)\s*\{([^}]*)\}\s*=\s*(?:process\.env|Bun\.env)\b")
TS_DECLARATION = re.compile(r"(?:^|[\s;])(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=;]+)?=(?![=>])")
TS_FUNCTION = re.compile(r"(?:^|[\s;])(?:export\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)\s*[<(]")
TS_IMPORT = re.compile(r"import\s+(?:type\s+)?(?:[\w$]+\s*,\s*)?\{([^}]*)\}\s*from\s*[\"']([^\"']+)[\"']")
TS_WORD = re.compile(r"[A-Za-z_$][\w$]*")
TS_RETURN = re.compile(r"\breturn\b")
TS_STRING = re.compile(r"\"(?:[^\"\\\n]|\\.)*\"|'(?:[^'\\\n]|\\.)*'|`(?:[^`\\]|\\.)*`", re.S)
TS_PREREQUISITE = re.compile(r"\b(requireOrSkip|skipWithout)\s*\(")
TS_KEYWORDS = frozenset({"true", "false", "null", "undefined", "typeof", "new", "await", "async", "void",
                         "process", "Bun", "import", "return", "function", "const", "let", "var", "in",
                         "of", "instanceof", "this", "Number", "String", "Boolean", "JSON", "Object",
                         "Array", "Math", "Date", "RegExp", "Error", "Promise", "Set", "Map", "expect",
                         "test", "it", "describe", "as", "satisfies"})
# The calls whose result still holds the value a read gave them, read through: the gate helpers
# of @legion/test-fixtures/host-tools, the conversions, and the string and list methods that ask
# after the value. An argument of any other call is that call's.
TS_VALUE_CALLS = frozenset({"requireOrSkip", "skipWithout", "Number", "String", "Boolean", "parseInt",
                            "parseFloat", "existsSync", "statSync", "hostTool", "which", "trim", "trimStart",
                            "trimEnd", "includes", "startsWith", "endsWith", "toLowerCase", "toUpperCase",
                            "split", "test", "match", "has", "some", "every", "filter", "map", "join"})


def blank_strings(text: str) -> str:
    """The text with each string's contents blanked, quotes and length kept: what a scan of the
    code reads, so that no `if (` or name inside a string is taken for code."""
    return TS_STRING.sub(lambda m: m.group()[0] + re.sub(r"[^\n]", " ", m.group()[1:-1]) + m.group()[-1], text)


def statement_end(text: str, start: int) -> int:
    """The end of the TypeScript statement that starts at text[start]: a `;`, or a line end
    that no operator carries over, outside brackets and strings."""
    index = start
    while index < len(text):
        char = text[index]
        if char in "([{":
            index = balanced(text, index)
            continue
        if char in "\"'`":
            index = _string_end(text, index)
            continue
        if char in ";\n":
            # A line that ends in an operator continues.
            before = text[start:index].rstrip()
            if char == "\n" and before.endswith(("&&", "||", "??", "=", "?", ":", ",", "+", "(", "!")):
                index += 1
                continue
            if char == "\n" and text[index + 1:].lstrip().startswith(("&&", "||", "??", "?", ":", ".")):
                index += 1
                continue
            return index
        index += 1
    return len(text)


def _string_end(text: str, start: int) -> int:
    quote = text[start]
    index = start + 1
    while index < len(text) and text[index] != quote:
        index += 2 if text[index] == "\\" else 1
    return index + 1


@dataclass
class TsModule:
    path: str
    text: str  # comments stripped
    blank: str  # comments stripped and strings blanked, at the same offsets
    declarations: dict[str, tuple[int, int]]  # name -> span of its value
    functions: dict[str, tuple[int, int]]  # name -> span of its body
    imports: dict[str, tuple[str, str]]  # local name -> (source file, exported name)
    destructured: dict[str, str]  # local name -> variable


def package_root(path: str, packages: set[str]) -> str:
    directory = str(Path(path).parent)
    while directory not in ("", "."):
        if directory in packages:
            return directory
        directory = str(Path(directory).parent)
    return "."


class TsTree:
    def __init__(self, files: list[str]):
        self.files = set(files)
        self.packages = {str(Path(f).parent) for f in files if Path(f).name == "package.json"}
        self.modules: dict[str, TsModule] = {}

    def module(self, path: str) -> TsModule:
        if path not in self.modules:
            text = strip_comments(read_text(path))
            blank = blank_strings(text)
            declarations, functions, imports, destructured = {}, {}, {}, {}
            for match in TS_DECLARATION.finditer(blank):
                declarations.setdefault(match.group(1), (match.end(), statement_end(blank, match.end())))
            for match in TS_FUNCTION.finditer(blank):
                brace = blank.find("{", match.end())
                if brace >= 0:
                    functions.setdefault(match.group(1), (brace, balanced(blank, brace)))
            for match in TS_DESTRUCTURE.finditer(text):
                for entry in match.group(1).split(","):
                    entry = entry.strip()
                    if not entry:
                        continue
                    variable, _, local = entry.partition(":")
                    local = (local or variable).split("=")[0].strip()
                    destructured[local] = variable.strip()
            root = package_root(path, self.packages)
            for match in TS_IMPORT.finditer(text):
                source = match.group(2)
                if not source.startswith("."):
                    continue
                resolved = self.resolve(str(Path(path).parent), source)
                if resolved is None or package_root(resolved, self.packages) != root:
                    continue
                for entry in match.group(1).split(","):
                    entry = entry.strip().removeprefix("type ").strip()
                    if not entry:
                        continue
                    exported, _, local = entry.partition(" as ")
                    imports[(local or exported).strip()] = (resolved, exported.strip())
            self.modules[path] = TsModule(path, text, blank, declarations, functions, imports, destructured)
        return self.modules[path]

    def resolve(self, directory: str, source: str) -> str | None:
        base = join(directory, source)
        for candidate in (base, *(base + ext for ext in (".ts", ".tsx", ".js", ".mjs")),
                          *(f"{base}/index{ext}" for ext in (".ts", ".tsx", ".js"))):
            if candidate in self.files:
                return candidate
        if base.endswith(".js") and base[:-3] + ".ts" in self.files:
            return base[:-3] + ".ts"
        return None

    def chain(self, path: str, span: tuple[int, int], seen: set | None = None) -> tuple[set[str], str]:
        """The variables an expression of a module holds - a read in it, a name declared from one,
        the return of a function of the module that holds one, through the module's own
        declarations and those of the same package it imports - each at its own level or inside
        a call of TS_VALUE_CALLS, and the text it read them from."""
        seen = seen if seen is not None else set()
        module = self.module(path)
        start, end = span
        text, blank = module.text[start:end], module.blank[start:end]
        skipped: list[tuple[int, int]] = []
        words: list[tuple[str, bool]] = []  # (name, called)
        for match in TS_WORD.finditer(blank):
            if any(a <= match.start() < b for a, b in skipped):
                continue
            word = match.group()
            back = match.start() - 1
            while back >= 0 and blank[back] in " \t\n":
                back -= 1
            ahead = match.end()
            while ahead < len(blank) and blank[ahead] in " \t\n":
                ahead += 1
            member = back >= 0 and blank[back] == "."
            following = blank[ahead:ahead + 2]
            if following[:1] == "(":
                if word not in TS_VALUE_CALLS:
                    skipped.append((ahead, balanced(blank, ahead)))
                if not member and word not in TS_KEYWORDS:
                    words.append((word, True))
                continue
            key = following[:1] == ":" and back >= 0 and blank[back] in "{,"
            if not member and word not in TS_KEYWORDS and not key and following != "=>":
                words.append((word, False))
        names = {m.group(1) or m.group(2) for m in TS_ENV.finditer(text)
                 if not any(a <= m.start() < b for a, b in skipped)}
        texts = [text]
        for word, called in words:
            if (path, word) in seen:
                continue
            seen.add((path, word))
            found, inner = self.resolve_word(path, module, word, called, seen)
            names |= found
            texts.append(inner)
        return names, "\n".join(texts)

    def resolve_word(self, path: str, module: TsModule, word: str, called: bool, seen: set) -> tuple[set[str], str]:
        if word in module.destructured:
            return {module.destructured[word]}, ""
        if not called and word in module.declarations:
            return self.chain(path, module.declarations[word], seen)
        if called and word in module.functions:
            return self.returns(path, module, module.functions[word], seen)
        if called and word in module.declarations:
            return self.returns(path, module, module.declarations[word], seen)
        if word in module.imports:
            source, exported = module.imports[word]
            other = self.module(source)
            if exported in other.destructured:
                return {other.destructured[exported]}, ""
            if not called and exported in other.declarations:
                return self.chain(source, other.declarations[exported], seen)
            if called and exported in other.functions:
                return self.returns(source, other, other.functions[exported], seen)
        return set(), ""

    def returns(self, path: str, module: TsModule, span: tuple[int, int], seen: set) -> tuple[set[str], str]:
        """What a function's `return`s hold (an arrow function's body is its return)."""
        start, end = span
        blank = module.blank[start:end]
        arrow = blank.find("=>")
        if module.blank[start:start + 1] != "{" and arrow >= 0 and not blank[arrow + 2:].lstrip().startswith("{"):
            return self.chain(path, (start + arrow + 2, end), seen)
        names, texts = set(), []
        for match in TS_RETURN.finditer(blank):
            at = start + match.end()
            found, text = self.chain(path, (at, statement_end(module.blank, at)), seen)
            names |= found
            texts.append(text)
        return names, "\n".join(texts)


TS_GATE_CALL = re.compile(r"\b(?:test|it|describe)(?:\.(?:only|concurrent|serial|todo|failing))*\.(skipIf|if|skip|fixme)\s*\(")
TS_IF = re.compile(r"\bif\s*\(")
TS_THROW_ON_ACTIONS = re.compile(r"GITHUB_ACTIONS[\s\S]{0,200}?\bthrow\b")
# A block an `if` guards that asserts or skips: an `expect(`, or a skip of the test it is in.
TS_GUARDED = re.compile(r"\bexpect\s*\(|\.skip\s*\(\s*\)|\.skip\s*\(\s*true\b")

# A term of a gate's condition that reads only whether this run is GitHub Actions: `!onActions`
# where `onActions` (in the file or a module of its package it imports) is declared as
# `process.env.GITHUB_ACTIONS === "true"`, and that read itself, written out (`!==` for the
# negation). A `skipIf` whose condition holds such a negated term as one of its `&&` conjuncts, or
# a `test.if` that holds the read itself as one of its `||` disjuncts, never skips on GitHub
# Actions: there the test runs and fails without what it needs, which is the prerequisite's rule.
TS_ACTIONS_READ = r"""(?:process|Bun)\.env(?:\??\.GITHUB_ACTIONS|\[\s*["']GITHUB_ACTIONS["']\s*\])"""
TS_ON_ACTIONS = re.compile(rf"""^{TS_ACTIONS_READ}\s*===\s*["']true["']$""")
TS_OFF_ACTIONS = re.compile(rf"""^(?:{TS_ACTIONS_READ}\s*!==\s*["']true["']|!\s*{TS_ACTIONS_READ})$""")


def split_operator(text: str, operator: str) -> list[str]:
    """`text` split at each `operator` outside brackets and strings."""
    parts, depth, current, index = [], 0, "", 0
    while index < len(text):
        char = text[index]
        if char in "\"'`":
            end = _string_end(text, index)
            current += text[index:end]
            index = end
            continue
        if char in "([{":
            depth += 1
        elif char in ")]}":
            depth -= 1
        if depth == 0 and text.startswith(operator, index):
            parts.append(current)
            current = ""
            index += len(operator)
            continue
        current += char
        index += 1
    parts.append(current)
    return parts


def unwrap(term: str) -> str:
    term = term.strip()
    while term.startswith("(") and term.endswith(")") and balanced(term, 0) == len(term):
        term = term[1:-1].strip()
    return term


def declaration(tree: "TsTree", path: str, name: str) -> tuple[str, str] | None:
    """The module and the value text `name` is declared with, in the module at `path` or one of
    its package it imports, or None."""
    module = tree.module(path)
    if name in module.imports:
        path, name = module.imports[name]
        module = tree.module(path)
    span = module.declarations.get(name)
    return (path, unwrap(module.text[span[0]:span[1]].strip().rstrip(";"))) if span else None


TS_NAME = re.compile(r"[A-Za-z_$][\w$]*")


def never_skips_on_actions(tree: "TsTree", path: str, condition: str, kind: str, depth: int = 0) -> bool:
    """Whether a gate of `kind` (skipIf or if) with this condition text runs its test whenever
    GITHUB_ACTIONS is `true`. A condition that is one name is read through its declaration
    (`const skipped = omp === undefined && !onActions`)."""
    def on(term: str) -> bool:
        term = unwrap(term)
        if TS_ON_ACTIONS.match(term):
            return True
        declared = declaration(tree, path, term) if TS_NAME.fullmatch(term) else None
        return declared is not None and bool(TS_ON_ACTIONS.match(declared[1]))

    def off(term: str) -> bool:
        term = unwrap(term)
        return bool(TS_OFF_ACTIONS.match(term)) or (term.startswith("!") and on(term[1:]))

    condition = unwrap(condition)
    if TS_NAME.fullmatch(condition) and depth < 4:
        declared = declaration(tree, path, condition)
        if declared is not None and not TS_ON_ACTIONS.match(declared[1]):
            return never_skips_on_actions(tree, declared[0], declared[1], kind, depth + 1)
    if kind == "skipIf":
        return any(off(term) for term in split_operator(condition, "&&"))
    if kind == "if":
        return any(on(term) for term in split_operator(condition, "||"))
    return False


def skip_space(text: str, index: int) -> int:
    while index < len(text) and text[index] in " \t\n\r":
        index += 1
    return index


def bun_test_files(files: list[str]) -> list[str]:
    return [f for f in files if TS_SOURCE.search(f) and (BUN_TEST.search(f) or SPEC.search(f))]


TS_ENV_TEXT = re.compile(r"\b(?:process\.env|Bun\.env|import\.meta\.env)\b")
TS_RELATIVE_IMPORT = re.compile(r"""from\s*["'](\.[^"']*)["']""")


def ts_gates(tree: TsTree, files: list[str]) -> list[Gate]:
    """The gates of the test files that read a variable, or import a module of their own that
    does: no other file can hold one."""
    gates = []
    for path in files:
        raw = read_text(path)
        if not re.search(r"""from\s+["'](?:bun:test|@playwright/test)["']""", raw):
            continue
        if not TS_ENV_TEXT.search(raw) and not any(
                (source := tree.resolve(str(Path(path).parent), imported)) and TS_ENV_TEXT.search(read_text(source))
                for imported in TS_RELATIVE_IMPORT.findall(raw)):
            continue
        module = tree.module(path)
        blank = module.blank
        sites: list[tuple[tuple[int, int], int, str]] = []
        for match in TS_GATE_CALL.finditer(blank):
            open_at = match.end() - 1
            close = balanced(blank, open_at) - 1
            span = (open_at + 1, close)
            if match.group(1) in ("skip", "fixme"):
                first = split_top(blank[open_at + 1:close])[0]
                begin = open_at + 1 + len(first) - len(first.lstrip())
                head_text = first.strip()
                if (not head_text or head_text[0] in "\"'`" or "=>" in head_text.split("\n")[0]
                        or head_text.startswith(("async", "function", "{"))):
                    continue
                span = (begin, open_at + 1 + len(first))
            sites.append((span, blank.count("\n", 0, match.start()) + 1, match.group(1)))
        for match in TS_IF.finditer(blank):
            open_at = match.end() - 1
            close = balanced(blank, open_at)
            start = skip_space(blank, close)
            end = balanced(blank, start) if blank[start:start + 1] == "{" else statement_end(blank, start)
            body = blank[start:end]
            follow = skip_space(blank, end)
            if blank.startswith("else", follow):
                tail = skip_space(blank, follow + 4)
                if blank[tail:tail + 1] == "{":
                    body += blank[tail:balanced(blank, tail)]
            if TS_GUARDED.search(body):
                sites.append(((open_at + 1, close - 1), blank.count("\n", 0, match.start()) + 1, "block"))
        tool = any(TOOL_LINE.match(line) for line in head(path))
        for span, line, kind in sites:
            variables, chained = tree.chain(path, span)
            if not variables:
                continue
            on_actions = "GITHUB_ACTIONS" in variables and (
                bool(TS_THROW_ON_ACTIONS.search(chained))
                or never_skips_on_actions(tree, path, module.text[span[0]:span[1]], kind))
            gates.append(Gate("bun", path, line, frozenset(variables), None, None,
                              bool(TS_PREREQUISITE.search(chained)), on_actions, tool))
    return gates


def split_top(text: str) -> list[str]:
    parts, depth, current, index = [], 0, "", 0
    while index < len(text):
        char = text[index]
        if char in "\"'`":
            end = _string_end(text, index)
            current += text[index:end]
            index = end
            continue
        if char in "([{":
            depth += 1
        elif char in ")]}":
            depth -= 1
        if char == "," and depth == 0:
            parts.append(current)
            current = ""
        else:
            current += char
        index += 1
    parts.append(current)
    return parts


# --------------------------------------------------------------------- what a Bun step runs
@dataclass
class BunRun:
    step: RunStep
    files: set[str]
    env: frozenset[str]
    # The modules its `--preload`s load before each file: a flag that changes what the files
    # judge, so two runs of one file are one mode only where they preload the same.
    preloads: frozenset[str] = frozenset()


# A source file `bun` runs as a program, not a test: a tool, whose `legion: runs - <script>`
# lines name the package scripts it runs, `<script> [<argument>…]`.
TOOL_SOURCE = re.compile(r"\.(?:ts|tsx|js|mjs|cjs|mts|cts)$")
RUNS_LINE = re.compile(r"^\s*(?:#|//|--|;|\*|/\*\*?)\s*legion: runs - (\S+)(.*?)\s*(?:\*/)?\s*$")


def runs_lines(path: str) -> list[tuple[str, list[str], int]]:
    """The package scripts a tool's first 15 lines name in `legion: runs - ` lines, each with the
    arguments the line gives it and its line."""
    found = []
    for number, line in enumerate(head(path), start=1):
        match = RUNS_LINE.match(line)
        if match:
            found.append((match.group(1), words_of(match.group(2)), number))
    return found


BUN_VALUE_FLAGS = {"--timeout", "--preload", "-t", "--test-name-pattern", "--rerun-each", "--max-concurrency",
                   "--reporter", "--reporter-outfile", "--seed", "--coverage-reporter", "--coverage-dir", "-r",
                   "--path-ignore-patterns", "--bail"}
PLAYWRIGHT_VALUE_FLAGS = {"--config", "-c", "--project", "--grep", "-g", "--grep-invert", "--workers", "-j",
                          "--reporter", "--shard", "--timeout", "--retries", "--repeat-each", "--output",
                          "--max-failures", "--tsconfig"}
GH_EXPRESSION = re.compile(r"\$\{\{.*?\}\}")


def commands(script: str) -> list[str]:
    """The simple commands of a script's lines, split at `&&`, `||`, `;` and `|`, braces and
    subshell parentheses dropped, continuation lines joined."""
    text = GH_EXPRESSION.sub("GHEXPR", "\n".join(script_lines(script))).replace("\\\n", " ")
    found = []
    for line in text.split("\n"):
        for part in split_shell(line):
            part = part.strip().strip("{}()").strip()
            if part:
                found.append(part)
    return found


def words_of(command: str) -> list[str]:
    try:
        return shlex.split(command)
    except ValueError:
        return command.split()


# The checkout's root as a step's own script names it: a step that runs a repository script from
# another working directory names it `"$GITHUB_WORKSPACE/.github/scripts/<name>.sh"`.
WORKSPACE_PREFIXES = ("$GITHUB_WORKSPACE/", "${GITHUB_WORKSPACE}/")


def repository_script(cwd: str, words: list[str]) -> str | None:
    """The repository's shell script a command runs by its path - from the step's directory, or
    from the checkout's root through $GITHUB_WORKSPACE - alone or after `bash` or `sh`, or None."""
    if words[:1] in (["bash"], ["sh"]):
        words = words[1:]
    if not words:
        return None
    word = words[0]
    path = next((norm(word[len(prefix):]) for prefix in WORKSPACE_PREFIXES if word.startswith(prefix)), None)
    if path is None:
        if word.startswith("$") or "/" not in word:
            return None
        path = join(cwd, word)
    return path if path.endswith((".sh", ".bash")) and (ROOT / path).is_file() else None


# coreutils timeout's options whose value is the next word (`-s TERM`, `-k 60`). Each other option
# of it is one word: --foreground, --preserve-status, -v, and the `--signal=TERM` and
# `--kill-after=60` forms.
TIMEOUT_VALUE_OPTIONS = {"-s", "--signal", "-k", "--kill-after"}


def unwrapped(words: list[str], env: set[str]) -> list[str]:
    """The command a command's words run, after the words that only start it: the variables set
    before it (`NAME=value`, each name added to env), `env`, `exec`, and coreutils `timeout` with
    its options and its duration, in any order. dispatch-unit-repeat.yaml's lane step runs
    `exec timeout --foreground --signal=TERM --kill-after=60 <seconds> bun run test:repeat …`, so
    a reader that reads only words that start with `bun` reads that step as no test command."""
    while words:
        assignment = INLINE.match(words[0])
        if assignment:
            env.add(assignment.group(1))
            words = words[1:]
        elif words[0] in ("env", "exec"):
            words = words[1:]
        elif words[0] == "timeout":
            index = 1
            while index < len(words) and words[index].startswith("-"):
                index += 2 if words[index] in TIMEOUT_VALUE_OPTIONS else 1
            words = words[index + 1:]
        else:
            break
    return words


class BunReader:
    def __init__(self, files: list[str]):
        self.files = files
        self.file_set = set(files)
        self.tests = bun_test_files(files)
        self.packages: dict[str, dict] = {}
        # Each tool a package script runs (`bun <file>`), reached through `bun run` from a
        # workflow step, with the directory whose package.json holds that script.
        self.tools: dict[str, str] = {}

    def scripts(self, directory: str) -> dict[str, str]:
        path = join(directory, "package.json")
        if path not in self.packages:
            try:
                self.packages[path] = (json.loads(read_text(path)).get("scripts") or {}) if (ROOT / path).is_file() else {}
            except ValueError as error:
                raise Unreadable(f"{path} is not JSON: {error}") from error
        return self.packages[path]

    def runs(self, step: RunStep) -> list[BunRun]:
        found = []
        cwd = step.workdir
        for command in commands(step.script):
            words = words_of(command)
            if words[:1] == ["cd"] and len(words) == 2:
                cwd = join(cwd, words[1])
                continue
            found += self.command(step, cwd, words, frozenset(), set())
        return found

    def command(self, step: RunStep, cwd: str, words: list[str], inline: frozenset, seen: set) -> list[BunRun]:
        env = set(inline)
        words = unwrapped(words, env)
        environment = frozenset(step.env | step.exported | env)
        if words[:2] == ["bun", "test"]:
            files, preloads = self.bun_test(cwd, words[2:])
            return [BunRun(step, files, environment, preloads)]
        playwright = self.playwright_at(words)
        if playwright is not None:
            return [BunRun(step, self.playwright(cwd, words[playwright:]), environment)]
        tool = self.tool_at(cwd, words)
        if tool is not None and ("tool", tool) not in seen:
            # A tool a package script runs: what it runs is the package scripts its runs lines
            # name, each run as `bun run <script> <arguments>` from the same directory.
            if any(entry[0] not in ("script", "tool") for entry in seen):
                self.tools.setdefault(tool, cwd)
            found = []
            for script, arguments, _ in runs_lines(tool):
                found += self.command(step, cwd, ["bun", "run", script, *arguments], frozenset(env),
                                      seen | {("tool", tool)})
            return found
        if words[:2] == ["bun", "run"] and len(words) > 2:
            name, extra = words[2], words[3:]
            if extra[:1] == ["--"]:
                extra = extra[1:]
            script = self.scripts(cwd).get(name)
            if script is None or (cwd, name) in seen:
                return []
            found = []
            parts = commands(script)
            for index, part in enumerate(parts):
                inner = words_of(part)
                if index == len(parts) - 1:
                    inner += extra
                found += self.command(step, cwd, inner, frozenset(env), seen | {(cwd, name)})
            return found
        path = repository_script(cwd, words)
        if path is not None and ("script", path) not in seen:
            # A repository script the step runs by its path: its own Bun and Playwright commands
            # run in the step, in the step's directory until the script moves, under the
            # variables the script exports beside the step's own.
            text = read_text(path)
            inner_env = frozenset(env | exported(text))
            found, inner_cwd = [], cwd
            for part in commands(text):
                inner = words_of(part)
                if inner[:1] == ["cd"] and len(inner) == 2:
                    inner_cwd = join(inner_cwd, inner[1])
                    continue
                found += self.command(step, inner_cwd, inner, inner_env, seen | {("script", path)})
            return found
        return []

    @staticmethod
    def playwright_at(words: list[str]) -> int | None:
        for start in range(min(len(words), 3)):
            if words[start:start + 2] == ["playwright", "test"]:
                return start + 2
        return None

    def tool_at(self, cwd: str, words: list[str]) -> str | None:
        """The source file a `bun <file>` command runs as a program, or None."""
        if words[:1] != ["bun"] or len(words) < 2 or not TOOL_SOURCE.search(words[1]):
            return None
        path = join(cwd, words[1])
        return path if path in self.file_set else None

    def bun_test(self, cwd: str, arguments: list[str]) -> tuple[set[str], frozenset[str]]:
        filters, ignored, preloads, index = [], [], set(), 0
        while index < len(arguments):
            word = arguments[index]
            index += 1
            if word.startswith("-"):
                flag, _, value = word.partition("=")
                if flag in BUN_VALUE_FLAGS and not value and flag != "--bail":
                    value = arguments[index] if index < len(arguments) else ""
                    index += 1
                if flag == "--path-ignore-patterns":
                    ignored.append(value)
                elif flag == "--preload":
                    preloads.add(join(cwd, value))
                continue
            filters.append(word)
        under = [f for f in self.tests if cwd in (".", "") or f.startswith(f"{cwd}/")]
        chosen = set()
        if not filters:
            chosen = set(under)
        for word in filters:
            if word.startswith(("./", "/", "../")):
                target = join(cwd, word)
                if target in self.file_set:
                    chosen.add(target)
                else:
                    chosen |= {f for f in under if f.startswith(f"{target}/")}
            else:
                chosen |= {f for f in under if word in os.path.relpath(f, cwd if cwd else ".")}
        return {f for f in chosen
                if not any(fnmatch.fnmatch(os.path.relpath(f, cwd or "."), pattern.removeprefix("./")) for pattern in ignored)}, frozenset(preloads)

    def playwright(self, cwd: str, arguments: list[str]) -> set[str]:
        config, filters, index = None, [], 0
        while index < len(arguments):
            word = arguments[index]
            index += 1
            if word.startswith("-"):
                flag, _, value = word.partition("=")
                if flag in PLAYWRIGHT_VALUE_FLAGS and not value:
                    value = arguments[index] if index < len(arguments) else ""
                    index += 1
                if flag in ("--config", "-c"):
                    config = value
                continue
            filters.append(word)
        directory = join(cwd, str(Path(config).parent)) if config else cwd
        specs = [f for f in self.files if SPEC.search(f) and f.startswith(f"{directory}/")]
        if filters:
            specs = [f for f in specs if any(re.search(re.escape(word), f) for word in filters)]
        return set(specs)


# -------------------------------------------------------------------------------- the registry
@dataclass
class Row:
    variable: str
    klass: str
    jobs: list[str]
    script: str  # set_by_script: the script a named job runs that sets the variable, or ""
    line: int


def registry() -> dict[str, Row]:
    if not (ROOT / REGISTRY).is_file():
        raise Unreadable(f"{REGISTRY} is missing: it is the registry of each gate's variable")
    document = load_yaml(REGISTRY)
    variables = document.get("variables")
    if not isinstance(variables, dict):
        raise Unreadable(f"{REGISTRY} has no `variables:` mapping")
    rows = {}
    for variable, row in variables.items():
        line = getattr(row, "line", 0)
        if not isinstance(row, dict):
            raise Unreadable(f"{REGISTRY}:{line}: {variable}'s row is not a mapping")
        klass = row.get("class")
        if klass not in CLASSES:
            raise Unreadable(f"{REGISTRY}:{line}: {variable}'s class is {klass!r}, not one of {', '.join(CLASSES)}")
        jobs = row.get("jobs") or []
        script = str(row.get("set_by_script") or "")
        if klass in JOB_CLASSES and (not isinstance(jobs, list) or not jobs):
            raise Unreadable(f"{REGISTRY}:{line}: {variable} is of class {klass} and names no job")
        if klass not in JOB_CLASSES and (jobs or script):
            raise Unreadable(f"{REGISTRY}:{line}: {variable} is of class {klass}, which names no job and no script")
        if not str(row.get("why") or "").strip():
            raise Unreadable(f"{REGISTRY}:{line}: {variable}'s row says no `why`")
        unknown = set(row) - {"class", "jobs", "set_by_script", "why"}
        if unknown:
            raise Unreadable(f"{REGISTRY}:{line}: {variable}'s row holds {', '.join(sorted(unknown))}")
        rows[str(variable)] = Row(str(variable), klass, [str(job) for job in jobs], script, line)
    return rows


# What sets a variable in a set_by_script script: an assignment (`NAME=…`, `export NAME=…`), or
# its hand-on to a container, `docker run -e NAME=…` or `-e "NAME=…"`, which sets it for the
# command the container runs. `-e NAME` with no value forwards the caller's own and sets nothing.
SCRIPT_ASSIGNS = r"(?:^|[\s\"'])(?:export\s+)?{name}="


def script_runners(flows: Workflows, script: str) -> set[str]:
    """The required jobs a step of which runs `script`, by its path from the step's directory or
    from the checkout's root through $GITHUB_WORKSPACE (repository_script), or inside a repository
    script the step runs that runs it in turn, from the step's directory as the wrapper leaves it:
    dispatch-browser.yaml's visual step runs .github/scripts/dispatch-browser-visual-suite.sh,
    which runs e2e/visual-run.sh from packages/dispatch."""
    def runs(text: str, cwd: str, seen: frozenset) -> bool:
        for command in commands(text):
            words = words_of(command)
            if words[:1] == ["cd"] and len(words) == 2:
                cwd = join(cwd, words[1])
                continue
            while words and INLINE.match(words[0]):
                words = words[1:]
            path = repository_script(cwd, words)
            if path is None or path in seen:
                continue
            if path == script or runs(read_text(path), cwd, seen | {path}):
                return True
        return False

    return {step.part.job for step in required_steps(flows) if runs(step.script, step.workdir, frozenset())}


def variable_findings(rows: dict[str, Row], gates: list[Gate], flows: Workflows, tree: GoTree,
                      go: list[GoRun], bun: list[BunRun]) -> list[Finding]:
    found = []
    setters: dict[str, set[str]] = defaultdict(set)
    for step in required_steps(flows):
        for variable in step.env | step.exported | env_written(step.script) | inline_env(step.script):
            setters[variable].add(step.part.job)
    for run in [*bun, *go]:
        for variable in run.env:
            setters[variable].add(run.step.part.job)
    for row in rows.values():
        by_script = set()
        if row.script:
            if not (ROOT / row.script).is_file():
                found.append(Finding("variable", REGISTRY, row.line, row.variable,
                                     f"its row's set_by_script names {row.script}, which is not in the repository"))
            elif not re.search(SCRIPT_ASSIGNS.format(name=re.escape(row.variable)), read_text(row.script), re.M):
                found.append(Finding("variable", REGISTRY, row.line, row.variable,
                                     f"its row's set_by_script names {row.script}, which does not set it"))
            else:
                by_script = script_runners(flows, row.script)
                if not by_script & set(row.jobs):
                    found.append(Finding("variable", REGISTRY, row.line, row.variable,
                                         f"no step of a job its row names runs {row.script}, which sets it"))
        for job in row.jobs:
            if job not in flows.required_jobs:
                found.append(Finding("variable", REGISTRY, row.line, row.variable,
                                     f"its row names {job}, which is no job ci-ok needs"))
            elif job not in setters[row.variable] and row.variable not in RUNNER_ENV and job not in by_script:
                found.append(Finding("variable", REGISTRY, row.line, row.variable,
                                     f"its row names {job}, which does not set it"))
    read = set()
    for gate in gates:
        for variable in sorted(gate.variables):
            read.add(variable)
            row = rows.get(variable)
            if row is None:
                found.append(Finding("variable", gate.path, gate.line, variable,
                                     f"{what(gate)} gates on it and no row of {REGISTRY} names it"))
                continue
            reason = class_fails(row, gate, variable, tree, go, bun)
            if reason:
                found.append(Finding("variable", gate.path, gate.line, variable, reason))
    for row in rows.values():
        if row.variable not in read:
            found.append(Finding("variable", REGISTRY, row.line, row.variable,
                                 "its row names a variable no gate of the tree reads"))
    return found


def inline_env(script: str) -> set[str]:
    """The variables a script's commands set before themselves, `NAME=value command`."""
    names = set()
    for command in commands(script):
        for word in words_of(command):
            match = INLINE.match(word)
            if not match:
                break
            names.add(match.group(1))
    return names


def what(gate: Gate) -> str:
    if gate.language == "go":
        return gate.test or "a helper of its package's tests"
    return "the file's tests"


def class_fails(row: Row, gate: Gate, variable: str, tree: GoTree, go: list[GoRun], bun: list[BunRun]) -> str:
    """Why the row's class does not hold for this gate, or "". A gate in a tool (a Benchmark, a
    file that carries the tool line) holds under any row: a tool reads what its runner hands it,
    and the row says what the variable is to the tests that read it too (DISPATCH_TOKEN_CORPUS is
    a CI job's input of one test and a person's input of two benchmarks). So does a gate that never
    skips on GitHub Actions - one that fails there, or whose condition cannot hold there
    (never_skips_on_actions): there the test runs, and fails without what it needs."""
    if gate.tool or gate.fails_on_actions:
        return ""
    if row.klass in JOB_CLASSES or row.klass == "opt-out":
        if gate.language == "go":
            runners = [(r.step, r.env) for r in go_runners(tree, go, gate.file, gate.test)]
        else:
            runners = [(r.step, r.env) for r in bun if gate.path in r.files]
        ran = sorted({step.part.job for step, _ in runners})
        if row.klass == "opt-out":
            setting = sorted({step.part.job for step, env in runners if variable in env})
            if not runners:
                return f"its row's class is opt-out, and no required job runs {what(gate)}"
            if setting:
                return (f"its row's class is opt-out, and {', '.join(setting)} runs {what(gate)} with it set, "
                        "which opts that run out")
            return ""
        if row.script:
            # The variable is set inside the script, which the row's job runs; the check of the
            # script is the row's own (variable_findings).
            return ""
        if not any(step.part.job in row.jobs and variable in env for step, env in runners):
            return (f"its row's class is {row.klass}, and no job it names ({', '.join(row.jobs)}) runs "
                    f"{what(gate)} with it set" + (f"; {', '.join(ran)} runs it" if ran else "; no required job runs it"))
        return ""
    if row.klass == "prerequisite":
        if gate.prerequisite or gate.fails_on_actions:
            return ""
        return ("its row's class is prerequisite, and this gate neither calls testprereq.Require, "
                "requireOrSkip or skipWithout nor reads GITHUB_ACTIONS and fails there")
    return "its row's class is tool, and this gate is in no Benchmark and in no file that carries the tool line"


# ------------------------------------------------------------------------------ rule 4: scripts
SCRIPT_EXTENSIONS = (".sh", ".bash", ".py")
PY_IMPORT = re.compile(r"^\s*(?:from\s+([A-Za-z_][\w]*)\s+import|import\s+([A-Za-z_][\w]*(?:\s*,\s*[A-Za-z_][\w]*)*))", re.M)
PATH_CHARS = frozenset("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_./-")
# The computed filters' declarations: each file of GATES_DIR binds one GATE or EXCLUDED, and no
# file runs on its own. A Python module that calls load_gate_files at its top level (ci_job_reads.py
# calls ci_filter_sources.py's) runs every one of them; the line that defines the function calls
# nothing.
GATES_DIR = ".github/ci-gates"
LOADS_GATE_FILES = re.compile(r"^(?!def\s)\S.*\bload_gate_files\(", re.M)


def is_script(path: str) -> bool:
    if any(part in ("testdata", "node_modules") for part in path.split("/")):
        return False
    if path.endswith(SCRIPT_EXTENSIONS):
        return True
    try:
        with open(ROOT / path, "rb") as handle:
            return handle.read(2) == b"#!"
    except OSError:
        return False


def comment_free(path: str, text: str) -> str:
    if path.endswith((".go", ".ts", ".tsx", ".js", ".mjs", ".cjs")):
        return "\n".join(line for line in text.splitlines() if not line.lstrip().startswith(("//", "*", "/*")))
    return "\n".join(line for line in text.splitlines() if not line.lstrip().startswith("#") or line.startswith("#!"))


class Mentions:
    """The scripts a text names: by a path whose end is the script's path from the repository
    root (`../../.github/scripts/x.sh`, `"$script_dir/lib/x.sh"` for lib/x.sh), or by its bare
    name; and the Python modules a text imports."""

    def __init__(self, by_name: dict[str, list[str]]):
        self.by_name = by_name
        self.gate_files = sorted(path for paths in by_name.values() for path in paths
                                 if path.startswith(f"{GATES_DIR}/") and path.endswith(".py"))
        # Most names end in .sh, .bash or .py: one pattern finds every such word and the name set
        # keeps the scripts; the few other names (a shebang file with no extension, or a .ts one)
        # are spelled out.
        others = sorted((n for n in by_name if not n.endswith(SCRIPT_EXTENSIONS)), key=len, reverse=True)
        alternatives = [r"[A-Za-z0-9_-][A-Za-z0-9_.-]*\.(?:sh|bash|py)"] + [re.escape(n) for n in others]
        self.pattern = re.compile(r"(?<![A-Za-z0-9_.-])(" + "|".join(alternatives) + r")(?![A-Za-z0-9_-])")

    def of(self, path: str, text: str) -> set[str]:
        found = set()
        for match in PY_IMPORT.finditer(text):
            for module in re.split(r"\s*,\s*", match.group(1) or match.group(2)):
                found.update(self.by_name.get(f"{module}.py", []))
        if path.endswith(".py"):
            if LOADS_GATE_FILES.search(text):
                found.update(self.gate_files)
            return found  # a Python script reaches another script only through an import here
        for match in self.pattern.finditer(text):
            name = match.group(1)
            if name not in self.by_name:
                continue
            start = match.start()
            while start > 0 and text[start - 1] in PATH_CHARS:
                start -= 1
            spelled = text[start:match.end()]
            # A path that starts at a variable (`$dir/x.sh`, `${dir}/x.sh`, `$(dirname "$0")/x.sh`)
            # says nothing of the directories the variable holds.
            if start > 0 and text[start - 1] in "${}()" and not spelled.startswith("/"):
                spelled = spelled.split("/", 1)[1] if "/" in spelled else name
            tail = re.sub(r"^(?:\.\.?/)+", "", spelled.lstrip("/"))
            for candidate in self.by_name[name]:
                if tail == name or candidate == tail or candidate.endswith(f"/{tail}"):
                    found.add(candidate)
        return found


def script_findings(files: list[str], flows: Workflows, tree: GoTree, go: list[GoRun],
                    bun_reader: BunReader, bun: list[BunRun]) -> list[Finding]:
    candidates = [f for f in files if is_script(f)]
    by_name: dict[str, list[str]] = defaultdict(list)
    for path in candidates:
        by_name[Path(path).name].append(path)
    mentions = Mentions(by_name)
    texts: list[tuple[str, str]] = []
    for step in required_steps(flows):
        texts.append((step.part.workflow, step.script))
        for command in commands(step.script):
            texts += [(f"{step.workdir}/package.json", text)
                      for text in package_script_texts(bun_reader, step.workdir, words_of(command), set())]
    reached_files = set()
    for go_run in go:
        for directory in go_run.directories:
            reached_files.update(f.path for f in tree.by_directory.get(directory, []))
    # A Bun test's own modules (its relative imports, and theirs) run in its process too: a helper
    # module that spawns a script is that test spawning it.
    resolver = TsTree(files)
    pending_modules = [path for run in bun for path in run.files]
    while pending_modules:
        path = pending_modules.pop()
        if path in reached_files:
            continue
        reached_files.add(path)
        for source in TS_RELATIVE_IMPORT.findall(read_text(path)):
            resolved = resolver.resolve(str(Path(path).parent), source)
            if resolved and TS_SOURCE.search(resolved):
                pending_modules.append(resolved)
    texts += [(path, comment_free(path, read_text(path))) for path in sorted(reached_files)]
    reached: set[str] = set()
    pending = texts
    while pending:
        path, text = pending.pop()
        for found in mentions.of(path, text):
            if found not in reached:
                reached.add(found)
                pending.append((found, comment_free(found, read_text(found))))
    found = []
    for path in candidates:
        if path in reached:
            continue
        lines = head(path)
        if not any(TEST_WORDS.search(line) for line in lines):
            continue
        if any(TOOL_LINE.match(line) or NOT_A_TEST_LINE.match(line) for line in lines):
            continue
        found.append(Finding("script", path, 1, Path(path).name,
                             "it uses the words proof, smoke, check or test, no required step runs it, and it "
                             "carries neither `legion: tool - ` nor `legion: not a test - ` in its first 15 lines"))
    return found + marker_findings(candidates)


# A harness of a script it tests, by its name.
HARNESS_NAME = re.compile(r"[._]test\.(?:sh|bash)$")
# A header's first line of prose that says the script is a test or a proof (`A proof that…`,
# `Tests for…`, `The smoke test of…`), where the word is what the script is, not what a thing it
# starts or reads is (`A smoke stack.`, `Starts the stack the smoke checks run on.`).
CALLS_ITSELF = re.compile(r"^(?:(?:a|an|the|this(?:\s+script)?(?:\s+is)?(?:\s+an?)?)\s+)?(?:[\w-]+\s+){0,2}?"
                          r"(?:tests?|proofs?)\b|^(?:proves|smoke-tests)\b", re.IGNORECASE)
COMMENT_MARK = re.compile(r"^\s*(?:#!.*|#+|//+|/\*+|\*+/?|--|;)\s*")


def calls_itself_a_test(lines: list[str]) -> bool:
    """Whether a header's first line of prose, its shebang and its legion: lines aside, calls the
    script a test, a proof or a smoke test."""
    for line in lines:
        if line.startswith("#!") or TOOL_LINE.match(line) or NOT_A_TEST_LINE.match(line) or RUNS_LINE.match(line):
            continue
        prose = COMMENT_MARK.sub("", line, count=1).strip()
        if prose:
            return bool(CALLS_ITSELF.match(prose))
    return False


def marker_findings(candidates: list[str]) -> list[Finding]:
    """Each `*.test.sh` harness, and each script whose header calls it a test, a proof or a smoke
    test, that carries `legion: not a test - `: that line marks only a script that acts on
    GitHub, a release or a deployment and judges no code, so on a test it hides the test from
    rule 4 whether a job runs it or not."""
    found = []
    for path in candidates:
        lines = head(path)
        marked = next((number for number, line in enumerate(lines, start=1) if NOT_A_TEST_LINE.match(line)), 0)
        if not marked:
            continue
        if HARNESS_NAME.search(path):
            what = "it is a harness (*.test.sh)"
        elif calls_itself_a_test(lines):
            what = "its header calls it a test, a proof or a smoke test"
        else:
            continue
        found.append(Finding("script", path, marked, Path(path).name,
                             f"{what}, and it carries `legion: not a test - `, which marks only a script that acts "
                             "on GitHub, a release or a deployment and judges no code: give it the tool line, or "
                             "run it in a job ci-ok needs"))
    return found


def package_script_texts(reader: BunReader, cwd: str, words: list[str], seen: set) -> list[str]:
    """The text of the package script a `bun run <script>` command runs, and of each package
    script it runs in turn, a tool's runs lines' scripts among them."""
    words = unwrapped(words, set())
    tool = reader.tool_at(cwd, words)
    if tool is not None and ("tool", tool) not in seen:
        texts = []
        for script, arguments, _ in runs_lines(tool):
            texts += package_script_texts(reader, cwd, ["bun", "run", script, *arguments], seen | {("tool", tool)})
        return texts
    if words[:2] != ["bun", "run"] and words[:2] != ["npm", "run"] or len(words) < 3:
        return []
    script = reader.scripts(cwd).get(words[2])
    if script is None or (cwd, words[2]) in seen:
        return []
    texts = [script]
    for command in commands(script):
        texts += package_script_texts(reader, cwd, words_of(command), seen | {(cwd, words[2])})
    return texts


# ---------------------------------------------------- rules 5, 7 and 8: steps no merge waits for
# A `go test` is a test command unless it builds its binary alone (`-c`) or runs none of its tests
# (`-run '^$'`, as ci-caches.yaml's fill starts each test binary to link and cache it).
GO_RUNS_NONE = r"(?!.*\s-c\b)(?!.*\s-run[= ]?[\"']?\^\$[\"']?(?:\s|$))"
TEST_COMMAND = re.compile(rf"\bgo\s+(?:-C\s+\S+\s+)?test\b{GO_RUNS_NONE}|\bbun\s+test\b|\bplaywright\s+test\b|\.test\.sh\b|_test\.sh\b")
GO_TEST_COMMAND = re.compile(rf"\bgo\s+(?:-C\s+\S+\s+)?test\b{GO_RUNS_NONE}")
RELEASE_WORKFLOW = re.compile(rf"^{re.escape(WORKFLOWS)}/release[^/]*\.ya?ml$")


def is_test_command(reader: BunReader, cwd: str, line: str) -> bool:
    if TEST_COMMAND.search(line):
        return True
    # A `${{ }}` left in the line (a timeout's duration among them) is one word, as BunReader
    # reads it (commands).
    for command in re.split(r"&&|\|\||;|\|", GH_EXPRESSION.sub("GHEXPR", line)):
        if any(TEST_COMMAND.search(text) for text in package_script_texts(reader, cwd, words_of(command.strip()), set())):
            return True
    return False


@dataclass
class Outside:
    """A step no required job runs, with the workflow it starts in and how it is read."""
    top: str  # the workflow file a trigger starts, which holds the part or calls its workflow
    step: RunStep
    kind: str  # schedule (it runs only on schedule or workflow_dispatch), release, or outside


def outside_steps(files: list[str], flows: Workflows) -> list[Outside]:
    """Every step that runs in no job ci-ok needs: each step of ci.yaml's other jobs and of each
    other workflow (through each workflow of this repository its jobs call, but one a required
    job calls, whose steps are ci-ok's), and each step of a required job whose own `if:` admits
    only schedule and workflow_dispatch. A workflow that runs only when another calls it is read
    through its callers."""
    called_by_required = {part.workflow for part in flows.parts if part.required}
    found = []
    for step in flows.steps:
        if not step.part.required or step.scheduled:
            scheduled = step.scheduled or scheduled_only(step.part.body.get("if"))
            found.append(Outside(CI, step, "schedule" if scheduled else "outside"))
    for path in sorted(f for f in files if f.startswith(f"{WORKFLOWS}/") and f.endswith((".yaml", ".yml"))):
        if path == CI or path in called_by_required:
            continue
        document = load_yaml(path)
        events = triggers(document)
        if not events or events == {"workflow_call"}:
            continue
        scheduled_workflow = events <= SCHEDULED
        for name, body in (document.get("jobs") or {}).items():
            body = body or {}
            uses = str(body.get("uses") or "")
            if uses.removeprefix("./") in called_by_required:
                continue
            job_scheduled = scheduled_workflow or scheduled_only(body.get("if"))
            for part in parts_of(name, path, name, body, document, False, frozenset()):
                for step in run_steps(part):
                    if RELEASE_WORKFLOW.match(path):
                        kind = "release"
                    elif job_scheduled or step.scheduled or scheduled_only(part.body.get("if")):
                        kind = "schedule"
                    else:
                        kind = "outside"
                    found.append(Outside(path, step, kind))
    return found


def carries_tool_line(path: str) -> bool:
    return any(TOOL_LINE.match(line) for line in head(path))


def uncovered(step: RunStep, reader: BunReader, tree: GoTree, go: list[GoRun], bun: list[BunRun],
              harnesses: set[str]) -> list[str]:
    """What a step no merge waits for runs that no required job runs in the same mode: each test
    file of its Bun and Playwright runs that no required run runs with the same preloads, each Go
    test it runs that no required go test runs under the same -race (builds and selects), and each
    harness it runs that no required step runs. A step whose test commands this search reads as
    no run at all is uncovered too: nothing it read is what a required job runs."""
    missing, read = [], 0
    for run in reader.runs(step):
        read += 1
        for path in sorted(run.files):
            if not any(path in other.files and other.preloads == run.preloads for other in bun):
                missing.append(path + (f" with --preload {' '.join(sorted(run.preloads))}" if run.preloads else ""))
    try:
        outside_go = step_go_runs(tree, step)
    except (steps.Refused, Unreadable) as refusal:
        return missing + [f"a go test this search cannot read ({refusal})"]
    for run in outside_go:
        read += 1
        for directory in run.directories:
            for go_file in run.files(tree, directory):
                for name, _ in go_file.tests:
                    if run.runs(name) and not any(other.race == run.race and other.builds(go_file) and other.runs(name)
                                                  for other in go):
                        missing.append(f"{name} of {directory}" + (" under -race" if run.race else ""))
    cwd = step.workdir
    for command in commands(step.script):
        path = repository_script(cwd, words_of(command))
        if path is not None and HARNESS_NAME.search(path):
            read += 1
            if path not in harnesses:
                missing.append(path)
    if not read:
        missing.append("test commands this search reads as no run a required job makes")
    return missing


def outside_findings(outside: list[Outside], flows: Workflows, reader: BunReader, tree: GoTree,
                     go: list[GoRun], bun: list[BunRun], listed: set[tuple[str, str, str]]) -> list[Finding]:
    """Rules 5, 7 and 8: each step no merge waits for that runs a test command (`go test`, `bun
    test`, `playwright test`, a `*.test.sh` harness, a `bun run` script that comes to one,
    through a tool's runs lines too). A release step passes where .github/release-checks.yaml
    lists it. Any other passes where a required job runs that very line in that very directory,
    or where its workflow carries the tool line and every test it runs a required job runs in the
    same mode. A step that runs no test command is not read."""
    required_lines = {(step.workdir, squash(line)) for step in required_steps(flows) for line in script_lines(step.script)}
    harnesses = set()
    for step in required_steps(flows):
        for command in commands(step.script):
            path = repository_script(step.workdir, words_of(command))
            if path is not None:
                harnesses.add(path)
    found = []
    for entry in outside:
        step = entry.step
        tests = [line for line in script_lines(step.script) if is_test_command(reader, step.workdir, line)]
        if not tests:
            continue
        if entry.kind == "release":
            if any((step.part.workflow, step.part.name, key) in listed for key in step.keys):
                continue
            line = next((line for line in tests if GO_TEST_COMMAND.search(line)), tests[0])
            reason = (f"it runs `{squash(line)}` over the tree's source in a release workflow: a release checks "
                      "an artifact it built, and a test of the source runs in a job ci-ok needs"
                      if GO_TEST_COMMAND.search(line) else
                      f"it runs `{squash(line)}` in a release workflow")
            found.append(Finding("release", step.part.workflow, step.line, step.name,
                                 f"{reason}, and {RELEASE_CHECKS} lists no such step with the artifact it checks"))
            continue
        lines = [line for line in tests if (step.workdir, squash(line)) not in required_lines]
        if not lines:
            continue
        if carries_tool_line(entry.top):
            missing = uncovered(step, reader, tree, go, bun, harnesses)
            if not missing:
                continue
            more = f" and {len(missing) - 1} more" if len(missing) > 1 else ""
            found.append(Finding(entry.kind, step.part.workflow, step.line, step.name,
                                 f"its workflow carries the tool line, and it runs {missing[0]}{more}, which no "
                                 "required job runs in that mode"))
            continue
        for line in lines:
            where = ("only on schedule or workflow_dispatch" if entry.kind == "schedule"
                     else "in a workflow no merge waits for")
            found.append(Finding(entry.kind, step.part.workflow, step.line, step.name,
                                 f"it runs `{squash(line)}` {where}, and no required job runs that text there"))
    return found


# ------------------------------------------------------------------- rule 8: the release checks
@dataclass
class ReleaseCheck:
    workflow: str
    job: str
    step: str
    artifact: str
    line: int

    @property
    def subject(self) -> str:
        return f"{self.workflow} / {self.job} / {self.step}"


def release_checks() -> list[ReleaseCheck]:
    if not (ROOT / RELEASE_CHECKS).is_file():
        raise Unreadable(f"{RELEASE_CHECKS} is missing: it is the list of each release step that runs a check")
    entries = load_yaml(RELEASE_CHECKS).get("checks")
    if not isinstance(entries, list):
        raise Unreadable(f"{RELEASE_CHECKS} has no `checks:` list")
    found = []
    for entry in entries:
        line = getattr(entry, "line", 0)
        if not isinstance(entry, dict):
            raise Unreadable(f"{RELEASE_CHECKS}:{line}: an entry is not a mapping")
        unknown = set(entry) - {"workflow", "job", "step", "artifact"}
        missing = [key for key in ("workflow", "job", "step", "artifact") if not str(entry.get(key) or "").strip()]
        if unknown or missing:
            raise Unreadable(f"{RELEASE_CHECKS}:{line}: an entry holds {', '.join(sorted(unknown)) or 'no other key'} "
                             f"and lacks {', '.join(missing) or 'nothing'}; each names workflow, job, step and artifact")
        found.append(ReleaseCheck(str(entry["workflow"]), str(entry["job"]), str(entry["step"]),
                                  str(entry["artifact"]), line))
    return found


ENV_REFERENCE = re.compile(r"\benv\.([A-Za-z_][A-Za-z0-9_]*)|\$\{?([A-Za-z_][A-Za-z0-9_]*)")


def release_findings(entries: list[ReleaseCheck]) -> list[Finding]:
    """Each entry of .github/release-checks.yaml that names no step of a release workflow, and
    each that names a step running `go test` whose environment holds no variable an earlier step
    of its own job built: written into $GITHUB_ENV, or named by that step's `with:` or run: (the
    image tag a build step pushes or loads, the file it writes). Such a test reads the tree's
    source, which a job ci-ok needs tests, and not the artifact the release publishes."""
    found = []
    for entry in entries:
        def finding(reason: str) -> None:
            found.append(Finding("release", RELEASE_CHECKS, entry.line, entry.subject, reason))

        if not RELEASE_WORKFLOW.match(entry.workflow):
            finding(f"it names {entry.workflow}, which is no release workflow ({WORKFLOWS}/release*.yaml)")
            continue
        if not (ROOT / entry.workflow).is_file():
            finding(f"it names {entry.workflow}, which is not in the repository")
            continue
        document = load_yaml(entry.workflow)
        job = (document.get("jobs") or {}).get(entry.job)
        if not isinstance(job, dict):
            finding(f"{entry.workflow} has no job {entry.job}")
            continue
        job_steps = job.get("steps") or []
        index = next((i for i, step in enumerate(job_steps)
                      if entry.step in {str(step[key]) for key in ("name", "id", "uses") if step.get(key)}), None)
        if index is None:
            finding(f"job {entry.job} of {entry.workflow} has no step whose name, id or uses is {entry.step}")
            continue
        script = str(job_steps[index].get("run") or "")
        if not any(GO_TEST_COMMAND.search(line) for line in script_lines(script)):
            continue
        environment = (set(document.get("env") or {}) | set(job.get("env") or {}) | set(job_steps[index].get("env") or {})
                       | inline_env(script)) - RUNNER_ENV
        built = set()
        for earlier in job_steps[:index]:
            text = json.dumps(earlier.get("with") or {}) + "\n" + str(earlier.get("run") or "")
            built |= {a or b for a, b in ENV_REFERENCE.findall(text)} | env_written(str(earlier.get("run") or ""))
            environment |= env_written(str(earlier.get("run") or ""))
        if not (environment & built) - RUNNER_ENV:
            finding("it runs go test, and its environment holds no variable an earlier step of its job built, so it "
                    f"tests the tree's source and not the artifact it names ({entry.artifact})")
    return found


# -------------------------------------------------------------------- rule 9: a tool's runs lines
def runs_findings(reader: BunReader) -> list[Finding]:
    """Each tool a workflow step reaches through `bun run` whose source names, in a string, a
    script of its package.json that no `legion: runs - ` line of its first 15 lines names: what
    it runs is read through those lines alone."""
    found = []
    for tool, cwd in sorted(reader.tools.items()):
        scripts = reader.scripts(cwd)
        named = {script for script, _, _ in runs_lines(tool)}
        text = strip_comments(read_text(tool))
        for match in TS_STRING.finditer(text):
            value = match.group()[1:-1]
            if value in scripts and value not in named and "${" not in value:
                found.append(Finding("runs", tool, text.count("\n", 0, match.start()) + 1, value,
                                     f"the tool names the package script {value} of {join(cwd, 'package.json')} in "
                                     "a string, and no `legion: runs - ` line of its first 15 lines names it"))
    return found


# ------------------------------------------------------------------ the list of unhomed tests
@dataclass(frozen=True)
class Unhomed:
    finding: str  # `<kind>: <path>: <subject>`, as a finding names itself less its line and reason
    row: str  # the row of devbox-tests-md or test-value-md that gives the test its home
    line: int


def finding_key(finding: Finding) -> str:
    return f"{finding.kind}: {finding.path}: {finding.subject}"


def unhomed() -> list[Unhomed]:
    """The entries of UNHOMED, each a finding of a test that dispatch://INFRA-3411's two lists
    (devbox-tests-md and test-value-md) give a home that the tree does not give it yet."""
    if not (ROOT / UNHOMED).is_file():
        return []
    document = load_yaml(UNHOMED)
    listed = document.get("unhomed")
    if not isinstance(listed, list):
        raise Unreadable(f"{UNHOMED} has no `unhomed:` list")
    found, seen = [], set()
    for entry in listed:
        line = getattr(entry, "line", 0)
        if not isinstance(entry, dict) or set(entry) != {"finding", "row"}:
            raise Unreadable(f"{UNHOMED}:{line}: an entry is a mapping of `finding` and `row` alone")
        finding, row = str(entry["finding"]).strip(), str(entry["row"]).strip()
        kind = finding.split(": ", 1)[0]
        if kind not in KINDS or finding.count(": ") < 2 or not row:
            raise Unreadable(f"{UNHOMED}:{line}: `{finding}` is no `<kind>: <path>: <subject>` of a kind "
                             f"this search finds ({', '.join(KINDS)}), or its entry names no row")
        if finding in seen:
            raise Unreadable(f"{UNHOMED}:{line}: `{finding}` is listed twice")
        seen.add(finding)
        found.append(Unhomed(finding, row, line))
    return found


# --------------------------------------------------------------------------------------- main
KINDS = ("tag", "skip", "unrun", "variable", "script", "runs", "schedule", "outside", "release", "listed")


def main() -> int:
    files = tree_files()
    flows = workflows()
    rows = registry()
    entries = release_checks()
    tree = go_tree(files)
    go = go_runs(flows, tree)
    reader = BunReader(files)
    bun = [run for step in required_steps(flows) for run in reader.runs(step)]
    outside = outside_steps(files, flows)
    for entry in outside:
        reader.runs(entry.step)  # each tool a step no merge waits for reaches, for rule 9
    go_gates = GoGates(tree)
    go_gates.analyse()
    # The test files, and every other TypeScript file a required step's `bun test` runs by its
    # path (`bun test ./test/rig.ts`), which Bun runs as a test file whatever its name.
    ts_files = sorted(set(reader.tests) | {path for run in bun for path in run.files if TS_SOURCE.search(path)})
    gates = go_gates.gates + ts_gates(TsTree(files), ts_files)
    tags, skips = tag_findings(tree, go), skip_findings(tree, go)
    reported = {finding.path for finding in tags} | {(finding.path, finding.subject) for finding in skips}
    listed = {(entry.workflow, entry.job, entry.step) for entry in entries}
    findings = (tags + skips + unrun_findings(tree, go, reported)
                + variable_findings(rows, gates, flows, tree, go, bun)
                + script_findings(files, flows, tree, go, reader, bun)
                + outside_findings(outside, flows, reader, tree, go, bun, listed)
                + release_findings(entries)
                + runs_findings(reader))
    unique = sorted(set(findings), key=lambda f: (KINDS.index(f.kind), f.path, f.line, f.subject, f.reason))
    # A finding the list of unhomed tests names passes; an entry that names no finding is one.
    entries_unhomed = unhomed()
    names = {entry.finding for entry in entries_unhomed}
    kept = [finding for finding in unique if finding_key(finding) not in names]
    found_keys = {finding_key(finding) for finding in unique}
    stale = [Finding("listed", UNHOMED, entry.line, entry.finding,
                     f"the list names it as a test with no home yet ({entry.row}), and the search no longer "
                     "finds it: a job ci-ok needs runs it now, or it is gone, so take it off the list")
             for entry in entries_unhomed if entry.finding not in found_keys]
    failing = kept + stale
    for finding in failing:
        print(finding)
    if failing:
        print(f"check-test-reach: {len(failing)} finding(s); each is a test, a tag, a variable, a script, a "
              "tool or a step that no job ci-ok needs reaches, or an entry of the list of unhomed tests "
              "that no longer names one, as this search's header says", file=sys.stderr)
        return 1
    print(f"check-test-reach: every test reaches a job ci-ok needs, but the {len(entries_unhomed)} the list of "
          f"unhomed tests names: {len(go)} go test command(s), {len(bun)} Bun or Playwright run(s), "
          f"{len(gates)} variable gate(s), {len(rows)} registry row(s), {len(entries)} release check(s)")
    return 0


try:
    sys.exit(main())
except (Unreadable, steps.Refused) as refusal:
    print(f"check-test-reach: cannot read an input: {refusal}", file=sys.stderr)
    sys.exit(2)
PY
