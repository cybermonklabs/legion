"""The `go test` commands of ci.yaml's jobs, read off the workflow text: the step reader
check-test-reach.sh imports.

Its caller walks the workflows and hands `step_of` each run: a step makes, with the directory it
runs in and the Go modules to read. `step_of` keeps a run: that runs a script AUDITED_SCRIPTS
reads, one whose script `script_step` reads, and one that holds a `go test` (or a gotestsum
command) in one of those modules; `commands_of` reads such a step line by line into its
`go test` commands (`read_block`): packages, `-tags`, `-race`, `-run` and `-skip`, through the
variables a line sets before its command, the wrappers GO_WRAPPERS names and `for` loops over
packages.

The caller reads a step in each form the workflows write it in, with the helpers here. A matrix
job's step is read once for each leg of its matrix (`matrix_legs`), with each
`${{ matrix.<name> }}` it writes replaced by the leg's value, and a called workflow's step once
for each job that calls it, with each `${{ inputs.<name> }}` replaced by what that job's `with:`
gives over the defaults the workflow declares (`input_defaults`, `substitute`). A step that
`uses:` a local composite action is read through the action's own `run:` steps, under the inputs
its `with:` gives them (`local_action_steps`). A step whose run: is one command that runs a shell
script of the repository is read through the script's own lines (`script_step`), where the reader
can read every line of it. A gotestsum command is read as the
`go test` it wraps: its `--packages` list and the `go test` flags after its `--`
(`parse_gotestsum`). The variables a line sets before its command, and those a
`-exec 'env NAME=value'` hands the test binary, are the command's own environment (`GoTest.env`).

Three shapes of a line need an answer from outside the line: a `-run "$pattern"` assigned from a
`$(…)` call of `.github/scripts/go-tagged-tests.sh`; the package list `go list … | grep -Ev '…'`
gives a command, piped into `xargs go test …` or assigned to a variable first; and what
`.github/scripts/go_legs.py <module> <job> <leg> <legs>` prints for one leg of a legs job, piped
into `xargs go test …` or into `xargs` and an AUDITED_SCRIPTS script. `read_block` asks its
`answer` for each, with the step and the text of the call or the lister. A reader that runs no
`go` command answers the third with `legs_packages`: the union of the packages
`.github/go-legs.yaml` lists for every leg of that job, whole. go_legs.py fails a leg unless the
file runs each package of the module in exactly one place and each test of a split package in one
of its groups, so the legs of a job between them run those packages whole, and a leg's step is read
as running them. A line that is none of these, and holds no command this module reads, is refused
rather than skipped: a `go test` silently passed over would leave a reader wrong about what a step
runs.

It needs PyYAML, and audited_scripts.py beside it, which holds the readings of the scripts a step
runs (AUDITED_SCRIPTS) for every reader of what a step runs. The helpers that say which files a
job's steps live in (`called_workflows`), which lines a script holds (`script_lines`) and which
directory a step runs in (`step_workdir`) are here.
"""

from __future__ import annotations

import hashlib
import os
import re
import shlex
from collections.abc import Callable
from dataclasses import dataclass, field
from functools import cache
from pathlib import Path

import yaml
from audited_scripts import Unreadable, reading_of

# The repository holding this file: its own path, not resolved through a symlink, so a fixture
# tree that links this file in is the repository it reads.
ROOT = Path(__file__).absolute().parents[2]
# Repository-relative, as every message names it.
WORKFLOW = ".github/workflows/ci.yaml"
# A job's `uses:` that names a reusable workflow of this repository.
LOCAL_WORKFLOW = "./.github/workflows/"

# A test function's name, as `go test -list` prints it and as a `-skip` alternation names it.
NAME = re.compile(r"^(Test|Fuzz|Example|Benchmark)[A-Za-z0-9_]*$")
# A shell variable, as the steps write it: $pkg or ${pkg}.
SHELL_VAR = re.compile(r"\$(?:([A-Za-z_][A-Za-z0-9_]*)|\{([A-Za-z_][A-Za-z0-9_]*)\})")
FOR_LINE = re.compile(r"^for\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\s+(.+?)\s*;\s*do$")
ASSIGNMENT = re.compile(r'^([A-Za-z_][A-Za-z0-9_]*)="?\$\((.+)\)"?$')
# A lister piped into xargs and the command xargs runs with what it printed. `xargs -r`
# (`--no-run-if-empty`) runs nothing when its lister prints nothing, and with something printed
# runs the command plain xargs runs, so the reader reads it as plain xargs. The command may be
# `env NAME=value …` and a GO_WRAPPERS wrapper before its `go test` (envoy-go-race-docs' legs run
# `env RACE_MEM_LIMIT_KB=… go-test.sh --race-memory go test … -exec 'env GODEBUG=…' …`);
# read_block reads it through go_command, as it reads the same command written on a line of its
# own, and refuses a command that is no go test once those are read past (`xargs timeout 60 go
# test`, `xargs go vet`): xargs is among the commands NOT_PASSED_OVER names.
XARGS = re.compile(r"^(?P<lister>.+?)\s*\|\s*xargs\s+(?:(?:-r|--no-run-if-empty)\s+)?(?P<command>.+)$")
TAGGED_TESTS = ".github/scripts/go-tagged-tests.sh"
# The script that prints what one leg of a legs job runs, from LEGS_FILE: given `<module> <job>
# <leg> <legs> [<build flag>…]`, the leg's packages, or a split package's group of tests as
# `-run=^(…)$` and the package. A legs step pipes what it prints into xargs.
LEGS_SCRIPT = ".github/scripts/go_legs.py"
LEGS_FILE = ".github/go-legs.yaml"
# The keys of a module's entry in LEGS_FILE that name no job: the packages another job's own steps
# run (`elsewhere`), why a package runs in the legs of two jobs (`twice`), and the job whose last
# leg runs each package no place names (`unlisted`). They are go_legs.py's ELSEWHERE, TWICE,
# UNLISTED and NOT_JOBS, written here since this module imports nothing of the repository.
LEGS_ELSEWHERE = "elsewhere"
LEGS_TWICE = "twice"
LEGS_UNLISTED = "unlisted"
LEGS_NOT_JOBS = (LEGS_ELSEWHERE, LEGS_TWICE, LEGS_UNLISTED)
# A run: that starts a go test or a gotestsum, by its comment-free lines.
GO_TEST_TEXT = re.compile(r"\b(?:go\s+test|gotestsum)\b")
# The commands read as gotestsum (parse_gotestsum), by the last part of the word that names them.
GOTESTSUM_COMMANDS = ("gotestsum",)
# gotestsum's own flags that take a value, as `--flag value` or `--flag=value`; every other flag
# before its `--` is a switch. `--raw-command` makes the words after `--` a whole command and
# `--watch` runs no fixed set, so a command with either is refused rather than read.
GOTESTSUM_VALUE_FLAGS = ("--format", "-f", "--jsonfile", "--junitfile", "--packages",
                         "--rerun-fails-report", "--rerun-fails-max-failures", "--format-icons",
                         "--junitfile-testsuite-name", "--junitfile-testcase-classname",
                         "--junitfile-project-name", "--post-run-command", "--jsonfile-timing-events",
                         "--hide-summary")
GOTESTSUM_REFUSED = ("--raw-command", "--watch")
# The wrapper a step runs a go command under, matched by the end of the word, since a step writes
# the path from its own working directory: go-test.sh, which runs the go test command its words
# after its options are under gotestsum, with the same packages and flags, and whose merge-group
# rerun of a failed test runs no test the command does not, so the command behind it is read as
# though it stood alone. A wrapper this tuple does not name is refused rather than guessed at.
# GO_WRAPPER_OPTIONS are the options a wrapper takes before the command.
GO_WRAPPERS = (".github/scripts/go-test.sh",)
GO_WRAPPER_OPTIONS = ("--race-memory",)
# The expressions a step's text is read through before its lines are: a matrix value, and an input
# the job that calls the workflow, or the step that uses the composite action, gives. Any other
# expression is left as written.
CONTEXT_EXPRESSION = re.compile(r"\$\{\{\s*(matrix|inputs)\.([A-Za-z0-9_-]+)\s*\}\}")
# A composite action's step that names the checkout's root as its directory.
WORKSPACE_DIRECTORY = re.compile(r"^\$\{\{\s*github\.workspace\s*\}\}/?")
# The one move a script may make before its commands that script_step reads: into a directory
# named from the script's own, `cd "$(dirname "$0")/<path>"`.
SCRIPT_CD = re.compile(r'^cd\s+"\$\(dirname\s+"(?:\$0|\$\{BASH_SOURCE\[0\]\})"\)(?:/(?P<path>[^"]*))?"$')
# A positional parameter, which a script read without the arguments a step passes cannot expand.
POSITIONAL = re.compile(r"\$(?:[1-9@*#]|\{[1-9@*#])")
# The harnesses of the repository's own scripts, which run `go test` over fixture modules they
# write, not over the tree.
HARNESS_NAME = re.compile(r"[._]test\.(?:sh|bash)$")
# What script_step reads `$GITHUB_WORKSPACE/` as before it splits a run: into words, the checkout's
# root a step names a script by from another directory: a text no path holds.
WORKSPACE_MARK = "\0workspace\0/"


def called_workflows(document: dict) -> dict:
    """Every reusable workflow of this repository a job of `document` calls, as {its
    repository-relative path: the first job that calls it}, in the order the jobs name them."""
    called: dict = {}
    for job_id, job in (document.get("jobs") or {}).items():
        uses = str((job or {}).get("uses", ""))
        if uses.startswith(LOCAL_WORKFLOW):
            called.setdefault(uses.removeprefix("./"), job_id)
    return called


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


def step_workdir(document: dict, job: dict, step: dict) -> str:
    """The directory a run: step runs in: its own working-directory, else the job's or the
    workflow's defaults.run.working-directory, else the repository root."""
    given = (step.get("working-directory") or run_default(job, "working-directory")
             or run_default(document, "working-directory") or ".")
    return str(given).strip().removeprefix("./").rstrip("/") or "."


# The scripts a step runs that the reader cannot read whole, each read by a person with the one
# `go test` it runs for each argument list a step passes it, are audited_scripts.AUDITED_SCRIPTS:
# the one table of such readings, which go-test-step-expansion.py and check-ci-filters.sh's
# TAG_RUNS rule read a step through too. audited_step below reads a step through it, the entries
# only this search reads (search_only) among them. A wrapper script a Go step calls in place of
# `go test` (one that runs gotestsum for it) is read only once its command name is in
# GOTESTSUM_COMMANDS above, its path is in GO_WRAPPERS above (so that the go command it is handed
# is read in its place), or it has an entry there; until then the tests its steps run read as run
# by no step, and the search fails on each of them.

# A variable a step sets before its command, as the steps write it: RACE_MEM_LIMIT_KB=8388608
# behind `xargs env`.
# A value holding `$` is left for the caller to refuse, since what it expands to is the shell's.
ENV_PREFIX = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=[^$]*$")
# go commands that run no test, so a step may hold one and the reader need not read it: alone on
# its line, or with only commands the reader passes over chained after it (CHAINS). A line that
# chains anything else to one (`go vet ./... && go test ./...`) is refused.
NO_TEST_GO = ("build", "vet", "list", "install", "mod", "work", "env", "generate", "tool", "run")
# The commands a line that names no `go` may still start one through, or move the directory a
# later `go test` reads its packages from with: such a line is refused like any other this module
# cannot read. Any other line that holds no `go` word - a `docker exec … createdb` that makes the
# tests' database, an `echo` - starts no go command the reader could see, and is passed over.
NOT_PASSED_OVER = ("cd", "pushd", "popd", "source", ".", "eval", "exec", "bash", "sh", "env", "make",
                   "timeout", "xargs", "nohup", "sudo")
GO_WORD = re.compile(r"(?<![\w./-])go(?![\w.-])")
# The operators that end one command of a line and start the next, as shlex's punctuation tokens
# spell them.
CHAINS = ("&&", "||", ";", "|", "|&", "&")
# The go build and test flags that take the next argument as their value when written without
# `=`: go-test.sh's GO_VALUE_FLAGS, in its order, which Python cannot read out of the bash
# (`go help build`, `go help testflag`). Every other flag is a boolean, or written `-flag=value`.
VALUE_FLAGS = ("-asmflags", "-bench", "-benchtime", "-blockprofile", "-blockprofilerate", "-buildmode",
               "-compiler", "-count", "-covermode", "-coverpkg", "-coverprofile", "-cpu", "-cpuprofile",
               "-exec", "-fuzz", "-fuzzcachedir", "-fuzzminimizetime", "-fuzztime", "-gccgoflags",
               "-gcflags", "-installsuffix", "-ldflags", "-list", "-memprofile", "-memprofilerate", "-mod",
               "-modfile", "-mutexprofile", "-mutexprofilefraction", "-o", "-outputdir", "-overlay", "-p",
               "-parallel", "-pgo", "-pkgdir", "-run", "-shuffle", "-skip", "-tags", "-timeout",
               "-toolexec", "-trace", "-vet")
# The one `-skip` shape this module reads: the anchored alternation of whole test names
# every step of this repository that carries a -skip writes, `^(Name|Name|…)$`.
# Its names are read off the pattern's own text, and every other pattern is refused by name,
# because `go test` runs the real `-skip` through Go's regexp: a Python `re` matched against a
# listing would be a second engine, and two engines that agree on every pattern anyone wrote
# down disagree on the one nobody tried, leaving a reader wrong about what a step runs.
SKIP_ALTERNATION = re.compile(r"^\^\(([^()|]+(?:\|[^()|]+)*)\)\$$")


class Refused(Exception):
    """A shape this module will not guess at."""


@dataclass
class Step:
    workflow: str  # repository-relative: the file that holds this step
    job: str
    name: str
    workdir: str  # repository-relative, as the file writes it
    script: str

    @property
    def label(self) -> str:
        return f"{self.workflow}'s {self.job} / {self.name}"


@dataclass
class GoTest:
    """One `go test` command of one step."""

    step: Step
    text: str
    packages: list[str]
    tags: str = ""
    race: bool = False
    run: str = ""
    skip: str = ""  # the pattern as the step writes it; skip_names reads the names out of it
    # The variables the command's own line sets before it, and those a `-exec 'env NAME=value'`
    # hands the test binary, which is how the steps give it GODEBUG=checkfinalizers=1.
    env: dict[str, str] = field(default_factory=dict)


# How a reader answers the lines read_block cannot read off their own text: given the step and the
# text of the `$(…)` call or the lister, what it prints.
Answer = Callable[[Step, str], str]


def expand(text: str, variables: dict[str, str]) -> str:
    def one(match: re.Match[str]) -> str:
        name = match.group(1) or match.group(2)
        if name not in variables:
            raise Refused(f"the shell variable ${name} is set outside the step, or not at all")
        return variables[name]

    return SHELL_VAR.sub(one, text)


def step_of(workflow: str, job: str, name: str, workdir: str, script: str,
            modules: tuple[str, ...]) -> Step | None:
    """The step one run: makes, or None where it starts no `go test` this module reads: an
    AUDITED_SCRIPTS script's entry; the lines of a script script_step reads; or the run: itself,
    where it holds a `go test` or a gotestsum command and runs in one of `modules`
    (check-test-reach.sh's every module of the tree)."""
    audited = audited_step(workflow, job, name, workdir, script)
    if audited is not None:
        return audited
    step = Step(workflow=workflow, job=job, name=name, workdir=workdir, script=script)
    followed = script_step(step, modules)
    if followed is not None:
        return followed
    if workdir not in modules or not GO_TEST_TEXT.search("\n".join(script_lines(script))):
        return None
    return step


def context_text(value) -> str:
    """A matrix value, an input or an env value as the expression that reads it writes it into
    the text: a YAML boolean as `true` or `false`, nothing as the empty string."""
    if isinstance(value, bool):
        return "true" if value else "false"
    return "" if value is None else str(value)


def substitute(text: str, contexts: dict[str, dict[str, str]]) -> str:
    """The text with each `${{ matrix.<name> }}` and `${{ inputs.<name> }}` the contexts hold
    replaced by its value; any other expression, and one naming a value they lack, as written."""
    def one(match: re.Match[str]) -> str:
        values = contexts.get(match.group(1)) or {}
        return values.get(match.group(2), match.group(0))

    return CONTEXT_EXPRESSION.sub(one, text)


def matrix_legs(job: dict) -> list[dict[str, str]]:
    """The legs a job's `strategy.matrix` makes, each as the values its `${{ matrix.<name> }}`
    reads, expanded as GitHub expands a matrix: every combination of its lists, less each
    `exclude` entry matches, then each `include` entry added to every combination whose original
    values it does not change, or made a leg of its own where it changes those of each. A job with
    no matrix, or with one written as an expression, is one leg with no value, whose
    `${{ matrix.<name> }}` is then left as written."""
    matrix = ((job or {}).get("strategy") or {}).get("matrix")
    if not isinstance(matrix, dict):
        return [{}]
    keys = [key for key in matrix if key not in ("include", "exclude")]
    legs: list[dict] = [{}] if keys else []
    for key in keys:
        values = matrix[key]
        if not isinstance(values, list):
            return [{}]
        legs = [{**leg, key: value} for leg in legs for value in values]
    for excluded in matrix.get("exclude") or []:
        if isinstance(excluded, dict):
            legs = [leg for leg in legs if any(leg.get(k) != v for k, v in excluded.items())]
    originals = len(legs)
    for included in matrix.get("include") or []:
        if not isinstance(included, dict):
            continue
        added = False
        for leg in legs[:originals]:
            if all(leg.get(k) == v for k, v in included.items() if k in keys):
                leg.update({k: v for k, v in included.items() if k not in keys})
                added = True
        if not added:
            legs.append(dict(included))
    return [{str(k): context_text(v) for k, v in leg.items()} for leg in legs] or [{}]


def input_defaults(document: dict) -> dict[str, str]:
    """The default of each input a reusable workflow's `on.workflow_call` declares one for."""
    on = document.get("on", document.get(True)) or {}
    call = on.get("workflow_call") if isinstance(on, dict) else None
    declared = (call or {}).get("inputs") if isinstance(call, dict) else None
    return {str(name): context_text(spec.get("default")) for name, spec in (declared or {}).items()
            if isinstance(spec, dict) and "default" in spec}


def env_text(block, contexts: dict[str, dict[str, str]]) -> dict[str, str]:
    """An `env:` block's variables, each value as text with the contexts read in."""
    return {str(k): substitute(context_text(v), contexts) for k, v in (block or {}).items()} if isinstance(block, dict) else {}


@cache
def local_action(base: str) -> tuple[str, dict] | None:
    """The action file of the local directory `base`, action.yml or action.yaml, and what it
    holds, or None where it has neither: read once for each directory, since every leg of a job
    and every job that calls its workflow reads the same file."""
    path = next((f"{base}/{name}" for name in ("action.yml", "action.yaml") if (ROOT / base / name).is_file()), None)
    if path is None:
        return None
    return path, yaml.safe_load((ROOT / path).read_text()) or {}


def local_action_steps(uses: str, given: dict[str, str],
                       seen: frozenset = frozenset()) -> list[tuple[str, dict, dict[str, str]]]:
    """The run: steps of the local composite action a step's `uses: ./<directory>` names, with
    those of each local action it uses in turn, as (its action file, the step, the inputs that
    step reads: the `with:` its user gives over the defaults its action declares). An action that
    is no local directory with an action.yml or action.yaml has none here."""
    if not uses.startswith("./") or uses in seen:
        return []
    loaded = local_action(uses.removeprefix("./").rstrip("/"))
    if loaded is None:
        return []
    path, action = loaded
    declared = action.get("inputs") or {}
    inputs = {**{str(k): context_text(v.get("default")) for k, v in declared.items()
                 if isinstance(v, dict) and "default" in v}, **given}
    found = []
    for step in ((action.get("runs") or {}).get("steps") or []):
        if step.get("run"):
            found.append((path, step, inputs))
        elif step.get("uses"):
            inner = {str(k): substitute(context_text(v), {"inputs": inputs}) for k, v in (step.get("with") or {}).items()}
            found += local_action_steps(str(step["uses"]), inner, seen | {uses})
    return found


def action_workdir(step: dict, inputs: dict[str, str]) -> str:
    """The directory a composite action's run: step runs in: its own working-directory, the
    checkout's root where that names `${{ github.workspace }}` or is not given."""
    given = substitute(context_text(step.get("working-directory")), {"inputs": inputs})
    return WORKSPACE_DIRECTORY.sub("", given).strip() or "."


def script_step(step: Step, modules: tuple[str, ...]) -> Step | None:
    """The step a run: that is one command running a shell script of the repository makes: the
    script's own lines, read in the directory it runs them in - the step's, or the one its first
    command moves to from its own (`cd "$(dirname "$0")/<path>"`) - where that is one of
    `modules`. None where the run: is no such command, the script is one of the harnesses beside
    the scripts they test (`*.test.sh`), its lines hold no `go test` or gotestsum, it reads the
    arguments a step passes it, or the reader cannot read each of its lines: such a script is
    passed over, and an AUDITED_SCRIPTS entry is how a person's reading of it joins the reader."""
    lines = script_lines(step.script)
    if len(lines) != 1:
        return None
    line = lines[0]
    for prefix in ("$GITHUB_WORKSPACE/", "${GITHUB_WORKSPACE}/"):
        line = line.replace(prefix, WORKSPACE_MARK)
    if any(c in line for c in "|&;<>`$("):
        return None
    try:
        words = shlex.split(line)
    except ValueError:
        return None
    while words and ENV_PREFIX.match(words[0]):
        words = words[1:]
    if words[:1] in (["bash"], ["sh"]):
        words = words[1:]
    if not words or "/" not in words[0]:
        return None
    if words[0].startswith(WORKSPACE_MARK):
        relative = os.path.normpath(words[0][len(WORKSPACE_MARK):])
    else:
        relative = os.path.normpath(os.path.join(step.workdir, words[0]))
    if (relative.startswith("..") or os.path.isabs(relative) or not relative.endswith((".sh", ".bash"))
            or HARNESS_NAME.search(relative) or not (ROOT / relative).is_file()):
        return None
    text = (ROOT / relative).read_text(encoding="utf-8", errors="replace")
    body = [line for line in script_lines(text) if not line.startswith(("set ", "shopt "))]
    if not GO_TEST_TEXT.search("\n".join(body)) or (len(words) > 1 and POSITIONAL.search(text)):
        return None
    workdir = step.workdir
    moved = SCRIPT_CD.match(body[0]) if body else None
    if moved:
        workdir = os.path.normpath(os.path.join(os.path.dirname(relative), moved.group("path") or "."))
        body = body[1:]
    if workdir not in modules:
        return None
    followed = Step(workflow=step.workflow, job=step.job, name=step.name, workdir=workdir,
                    script="\n".join(body))
    try:
        commands_of(followed, lambda _step, _text: "(read by the reader's own answer)")
    except Refused:
        return None
    return followed


def audited_step(workflow: str, job: str, name: str, workdir: str, script: str) -> Step | None:
    """The step a run: that is an AUDITED_SCRIPTS script makes: audited_scripts.reading_of's
    reading of it, its search_only entries included, in the directory that reading names - the one
    reading of such a step, which go-test-step-expansion.py and check-ci-filters.sh's TAG_RUNS rule
    take too - or None where the run's text holds no such script's file name. A run: that names one
    in a form no entry reads (reading_of's Unreadable) is refused by step rather than read or
    skipped, as is a script whose text no longer has its entry's SHA-256. For a legs step the
    reading is the lister piped into the entry's go test, which read_block's xargs branch then
    reads, with the packages the lister prints."""
    label = f"{workflow}'s {job} / {name}"
    try:
        reading = reading_of(workdir, script, search=True)
    except Unreadable as unreadable:
        raise Refused(f"{label}: {unreadable}") from None
    if reading is None:
        return None
    digest = hashlib.sha256((ROOT / reading.path).read_bytes()).hexdigest()
    if digest != reading.entry.sha256:
        raise Refused(f"{label}: {reading.path} has SHA-256 {digest}, and its entry in "
                      f".github/scripts/audited_scripts.py's AUDITED_SCRIPTS was read from "
                      f"{reading.entry.sha256}; read the script again and update the entry's "
                      "commands and digest")
    return Step(workflow=workflow, job=job, name=name, workdir=reading.module, script=reading.script)


@cache
def module_packages(module: str) -> tuple[str, ...]:
    """The directories of `module` that hold a Go package, as the go list over ./... that
    LEGS_SCRIPT runs finds them, read off the tree with no `go` command: each directory under the
    module holding a `.go` file whose name starts with neither `.` nor `_`, outside a `testdata`,
    `vendor`, `.`- or `_`-named directory and outside a nested module; `.` for the module's own
    root. A directory whose every file a build tag excludes is read as a package here, which errs
    wide; the tag rule holds such files on its own. Walked once for each module."""
    base = ROOT / module
    found: list[str] = []
    for directory, subdirectories, files in os.walk(base):
        here = Path(directory)
        subdirectories[:] = sorted(name for name in subdirectories
                                   if not name.startswith((".", "_")) and name not in ("testdata", "vendor")
                                   and not (here / name / "go.mod").exists())
        if any(name.endswith(".go") and not name.startswith((".", "_")) for name in files):
            relative = here.relative_to(base).as_posix()
            found.append("." if relative == "." else relative)
    return tuple(found)


@cache
def legs_document() -> dict:
    """LEGS_FILE as it parses, read once: every leg of every legs step reads it."""
    return yaml.safe_load((ROOT / LEGS_FILE).read_text()) or {}


def package_argument(package: str) -> str:
    """A package of LEGS_FILE as a go command names it, from the module's root: go_legs.py's
    package_argument."""
    return "./" if package == "." else f"./{package}/"


def legs_packages(step: Step, text: str) -> list[str] | None:
    """The packages a `go_legs.py <module> <job> <leg> <legs> [<flag>…]` lister stands for, read
    off LEGS_FILE with no `go` command: each package any leg of that job lists, once, whole, as
    `./<directory>/` from the module, in the file's order, and, for the job the module's
    `unlisted` names, each package of the module (module_packages) that no place of the file names,
    which go_legs.py runs in that job's last leg; or None where `text` calls no LEGS_SCRIPT. A split
    package is whole here: go_legs.py fails a leg unless each of its tests is in one group the
    job's legs run. A call this reading cannot place - a module or job the file lacks, a step that
    runs outside the module the packages are relative to - is refused."""
    words = shlex.split(text)
    at = next((index for index, word in enumerate(words) if word.endswith(Path(LEGS_SCRIPT).name)), None)
    if at is None:
        return None
    if len(words) < at + 5:
        raise Refused(f"{step.label}: `{text}` is no call of {LEGS_SCRIPT} this reader reads: it takes "
                      "<module> <job> <leg> <legs>")
    module, job = words[at + 1], words[at + 2]
    if module != step.workdir:
        raise Refused(f"{step.label}: `{text}` lists the packages of {module} as paths from that module, "
                      f"and the step runs in {step.workdir}")
    entry = legs_document().get(module) or {}
    legs = entry.get(job) if job not in LEGS_NOT_JOBS else None
    if not isinstance(legs, dict) or not legs:
        raise Refused(f"{step.label}: {LEGS_FILE} lists no legs of {job} under {module}")

    def listed_packages(of: dict) -> list[str]:
        packages = []
        for listed in of.values():
            for written in listed or []:
                package = (written if isinstance(written, str)
                           else next(iter(written), None) if isinstance(written, dict) else None)
                if not isinstance(package, str):
                    raise Refused(f"{step.label}: {LEGS_FILE} lists `{written}` in a leg of {module}, which "
                                  "is no package and no `<package>: <group>`")
                packages.append(package)
        return packages

    found: list[str] = []
    for package in listed_packages(legs):
        argument = package_argument(package)
        if argument not in found:
            found.append(argument)
    if entry.get(LEGS_UNLISTED) == job:
        named = set(entry.get(LEGS_ELSEWHERE) or {}) | set(entry.get(LEGS_TWICE) or {})
        for other, other_legs in entry.items():
            if other not in LEGS_NOT_JOBS and isinstance(other_legs, dict):
                named.update(listed_packages(other_legs))
        for package in module_packages(module):
            argument = package_argument(package)
            if package not in named and argument not in found:
                found.append(argument)
    return found


def commands_of(step: Step, answer: Answer) -> list[GoTest]:
    """Every `go test` a step's script starts, each with its packages, tags, -race and -run. A
    line a `\\` continues is read as one line with the next, as bash reads it."""
    found: list[GoTest] = []
    read_block(step, step.script.replace("\\\n", " ").splitlines(), {}, found, answer)
    if not found:
        raise Refused(f"{step.label}: its run: holds `go test` and no command this reader can read")
    return found


def read_block(step: Step, lines: list[str], variables: dict[str, str],
               found: list[GoTest], answer: Answer) -> None:
    index = 0
    while index < len(lines):
        line = lines[index].strip()
        index += 1
        if not line or line.startswith("#") or line.startswith("set "):
            continue

        loop = FOR_LINE.match(line)
        if loop:
            body, index = loop_body(step, lines, index)
            for value in shlex.split(expand(loop.group(2), variables)):
                read_block(step, body, {**variables, loop.group(1): value}, found, answer)
            continue

        assignment = ASSIGNMENT.match(line)
        if assignment:
            name, inner = assignment.group(1), expand(assignment.group(2), variables)
            if TAGGED_TESTS in inner or re.match(r"go\s+list\b", inner):
                variables[name] = answer(step, inner).strip()
            else:
                raise Refused(f"{step.label}: this reader reads only a $(…) call of {TAGGED_TESTS}, "
                              f"or a `go list … | grep …` lister, not `{inner}`")
            continue

        piped = XARGS.match(line)
        if piped:
            written = piped.group("command").strip()
            command_line = written
            if command_line.split(None, 1)[0] == "env" and len(command_line.split(None, 1)) == 2:
                command_line = command_line.split(None, 1)[1]
            words, command_text = go_command(command_line)
            if words[:2] != ["go", "test"]:
                raise Refused(f"{step.label}: `{written}`, what `{line}` pipes into xargs, is no go test "
                              "this reader reads")
            lister = expand(piped.group("lister"), variables)
            appended = answer(step, lister).split()
            if not appended:
                raise Refused(f"{step.label}: `{lister}` printed no package")
            command = parse_go_test(step, expand(command_text, variables), appended)
            command.env = {**line_env(command_line), **command.env}
            found.append(command)
            continue

        words, command_text = go_command(line)
        if len(words) > 1 and words[0] == "go" and words[1] in NO_TEST_GO:
            if not all(passed_over(command) for command in chained(step, line)):
                raise Refused(f"{step.label}: `{line}` chains to its `go {words[1]}` a command that may "
                              "start a go test, which this reader does not read")
            continue
        if len(words) > 1 and words[0] == "go" and words[1] == "test":
            command = parse_go_test(step, expand(command_text, variables), [])
            command.env = {**line_env(line), **command.env}
            found.append(command)
            continue
        if words and words[0].rsplit("/", 1)[-1] in GOTESTSUM_COMMANDS:
            command = parse_gotestsum(step, expand(command_text, variables))
            command.env = {**line_env(line), **command.env}
            found.append(command)
            continue
        if passed_over(line):
            continue
        raise Refused(f"{step.label}: this reader cannot read `{line}`")


def passed_over(command: str) -> bool:
    """Whether a command starts no go command the reader could see, so the reader passes it over:
    it holds no `go` word, and the word that names it once go_command has read past its variables
    is no gotestsum, no NOT_PASSED_OVER command and no path to a program or a script."""
    words, _ = go_command(command)
    return (bool(words) and not GO_WORD.search(command) and words[0].rsplit("/", 1)[-1] not in GOTESTSUM_COMMANDS
            and words[0] not in NOT_PASSED_OVER and "/" not in words[0] and not words[0].endswith((".sh", ".py")))


def chained(step: Step, line: str) -> list[str]:
    """The commands a line runs after its first, each as its words joined by spaces: what follows
    each CHAINS operator outside quotes and comments, as bash splits the line. A line whose quoting
    does not close, which bash would refuse to run, is refused."""
    lexer = shlex.shlex(line, posix=True, punctuation_chars=True)
    lexer.whitespace_split = True
    commands: list[list[str]] = []
    try:
        for token in lexer:
            if token in CHAINS:
                commands.append([])
            elif commands:
                commands[-1].append(token)
    except ValueError as error:
        raise Refused(f"{step.label}: this reader cannot split `{line}` into its commands: {error}") from None
    return [" ".join(words) for words in commands]


def go_command(line: str) -> tuple[list[str], str]:
    """The command a line runs, with the variables it sets before it and the wrapper it runs it
    under dropped: its words, and its text with those words removed.

    Behind an xargs a step writes `env RACE_MEM_LIMIT_KB=8388608 ../../.github/scripts/go-test.sh
    --race-memory go test … -exec 'env GODEBUG=checkfinalizers=1' …`, which read_block hands here
    without its `env`. Neither the variables nor the wrapper changes which test functions the
    command runs, which is all the reader reads, so both are dropped and what is left is the go
    command, or the gotestsum command that runs it. A
    value holding `$`, and a wrapper GO_WRAPPERS does not name, are left where they are, so the
    caller refuses the line rather than guessing. The text is cut word by word off the line
    itself, so the quoting of what is left - a `-run` pattern above all - is the line's own. The
    variables themselves are line_env's.
    """
    rest = line.strip()
    while True:
        head = rest.split(None, 1)
        if len(head) == 2 and ENV_PREFIX.match(head[0]):
            rest = head[1]
            continue
        break
    head = rest.split(None, 1)
    if len(head) == 2 and head[0].endswith(GO_WRAPPERS):
        behind = head[1]
        while behind.split(None, 1)[0] in GO_WRAPPER_OPTIONS and len(behind.split(None, 1)) == 2:
            behind = behind.split(None, 1)[1]
        if behind.split(None, 1)[0] == "go":
            rest = behind
    return rest.split(), rest


def line_env(line: str) -> dict[str, str]:
    """The variables a line sets before its command, `NAME=value command`, which go_command
    drops: the command's environment, its test binary's included."""
    found = {}
    for word in line.strip().split():
        if not ENV_PREFIX.match(word):
            break
        name, _, value = word.partition("=")
        found[name] = value.strip("'\"")
    return found


def parse_gotestsum(step: Step, text: str) -> GoTest:
    """The `go test` a gotestsum command runs: the packages its `--packages` lists and the `go
    test` flags (and any package) after its `--`, read as parse_go_test reads a `go test`, with
    the command's text kept as the line writes it. gotestsum's own flags - `--format`,
    `--jsonfile`, the `--rerun-fails` family - choose how it reports and whether it runs a
    failed test again, never which tests the first run runs."""
    words = shlex.split(text)
    position, packages = 1, []
    while position < len(words):
        word = words[position]
        position += 1
        if word == "--":
            break
        flag, equals, value = word.partition("=")
        if not word.startswith("-"):
            raise Refused(f"{step.label}: `{text}` gives gotestsum `{word}` before its `--`, and "
                          "this reader reads packages from --packages and go test flags after `--`")
        if flag in GOTESTSUM_REFUSED:
            raise Refused(f"{step.label}: `{text}` runs gotestsum with {flag}, whose run this "
                          "reader cannot read off the command")
        if flag in GOTESTSUM_VALUE_FLAGS and not equals:
            if position >= len(words):
                raise Refused(f"{step.label}: `{text}` gives gotestsum's {flag} no value")
            value = words[position]
            position += 1
        if flag == "--packages":
            packages += value.split()
    command = parse_go_test(step, shlex.join(["go", "test", *words[position:]]), packages)
    command.text = text
    return command


def loop_body(step: Step, lines: list[str], index: int) -> tuple[list[str], int]:
    body, depth = [], 1
    while index < len(lines):
        line = lines[index].strip()
        index += 1
        if FOR_LINE.match(line) or line.startswith("while ") or line.startswith("until "):
            depth += 1
        elif line == "done":
            depth -= 1
            if depth == 0:
                return body, index
        body.append(line)
    raise Refused(f"{step.label}: a `for` loop with no `done`")


def skip_names(pattern: str) -> list[str] | None:
    """The test names an anchored alternation leaves out, or None for a pattern of any other
    shape, which the caller refuses by name rather than guessing at."""
    match = SKIP_ALTERNATION.match(pattern)
    if not match:
        return None
    names = match.group(1).split("|")
    return names if all(NAME.match(name) for name in names) else None


def parse_go_test(step: Step, text: str, appended: list[str]) -> GoTest:
    """The packages, -tags, -race, -run and -skip of one `go test` command, read with the words
    `appended` after its own, as xargs appends what its lister printed, and the variables a
    `-exec 'env NAME=value'` hands its test binary. The words after `-args` are the test binary's
    own, so the reading stops there."""
    command = GoTest(step=step, text=text, packages=[])
    words = shlex.split(text) + list(appended)
    position = words.index("test") + 1
    while position < len(words):
        word = words[position]
        position += 1
        if word == "-args":
            break
        if word == "-race":
            command.race = True
        elif word.startswith("-"):
            flag, _, value = word.partition("=")
            if not value and flag in VALUE_FLAGS:
                value = words[position]
                position += 1
            if flag == "-run":
                command.run = value
            elif flag == "-exec":
                exec_words = shlex.split(value)
                if exec_words[:1] == ["env"]:
                    command.env.update(line_env(" ".join(exec_words[1:])))
            elif flag == "-skip":
                if skip_names(value) is None:
                    raise Refused(f"{step.label}: `{text}` skips by `{value}`, and this reader "
                                  "reads only the anchored alternation of whole test names, "
                                  "`^(Name|Name|…)$`, that every -skip of this repository writes")
                command.skip = value
            elif flag == "-tags":
                command.tags = value
            elif flag == "-bench":
                raise Refused(f"{step.label}: `{text}` runs benchmarks, which this reader "
                              "does not expand")
        else:
            command.packages.append(word)
    if not command.packages:
        raise Refused(f"{step.label}: `{text}` names no package")
    return command
