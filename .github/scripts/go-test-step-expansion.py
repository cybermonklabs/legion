#!/usr/bin/env python3
"""The Go test functions each `go test` step of ci.yaml's router runs, and every one two steps run.

This is the tool check k1 of dispatch://INFRA-1408 asks for (its A1): it expands every `go test`
step of `.github/workflows/ci.yaml`, and of every workflow a job of it calls, whose working
directory is `packages/envoy` or `packages/envoy/infra` into the test functions the step really
runs - through the step's own packages, `-tags`, `-race` and `-run`, with `go test -list` - and
prints every (package, function) pair more than one step runs. It exits 1 on a pair no declared
exception covers, and 0 when every such pair is declared with the requirement that makes it run
twice (EXCEPTIONS below).

No CI job runs it, and none should. It compiles every test binary of both modules, the `-race`
ones included: minutes of compile on a warm module cache, and tens of minutes on a cold one, for
an answer a person needs twice per change (at the head and at the merge base) rather than once per
run. The checks it serves run by hand, before the merge.

It starts no service and needs no Postgres, no NATS, no Docker and no Pulumi: it never runs a
test. `go test -list` builds the test binary and asks it for its names, and each `TestMain` of the
packages here returns before it starts its database, NATS server, container or Pulumi stack when
the run is a listing.

`-race` needs cgo, so run it with `CGO_ENABLED=1` and a C compiler on PATH; a race step otherwise
fails to build and this script stops on that step.

Run it at two commits and compare, which is what k1 asks for - the head in your workspace, the
merge base in a clone under a temporary directory:

    store="$(cd "$WORKSPACE/.jj" && realpath "$(cat repo)")/store"
    cd "$WORKSPACE"
    python3 .github/scripts/go-test-step-expansion.py > "$TMPDIR/head.txt"; echo "head: $?"
    git clone --no-local "$(cd "$store" && realpath "$(cat git_target)")" "$TMPDIR/base"
    git -C "$TMPDIR/base" checkout <merge base>
    cp .github/scripts/go-test-step-expansion.py "$TMPDIR/base/.github/scripts/"
    (cd "$TMPDIR/base" && python3 .github/scripts/go-test-step-expansion.py) > "$TMPDIR/base.txt"
    echo "base: $?"
    grep '^PAIR' "$TMPDIR/head.txt" > "$TMPDIR/head.pairs"
    grep '^PAIR' "$TMPDIR/base.txt" > "$TMPDIR/base.pairs"
    diff "$TMPDIR/base.pairs" "$TMPDIR/head.pairs"   # empty: the change loses no test

It reads the repository root off its own path, so the copy under the temporary directory checks
itself and not the workspace it was copied from, and it reads a step's directory and its
comment-free lines with `ci_filter_coverage`'s own helpers, which sit beside it in both trees.
Each job of ci.yaml holds its steps in a `ci-job-<id>.yaml` of its own, so the set of documents
it reads is ci.yaml and every workflow a job of it calls, which it derives with that module's
`called_workflows` (`documents()` below says why it loads them itself).
Keep `GOMODCACHE` and `GOCACHE` the same for both runs: the second run then reuses the first
run's compile.

How it reads a step:

  * the working directory is the step's own `working-directory`, else the job's, else the
    workflow's `defaults.run.working-directory`, else the repository root, as GitHub reads it and
    as check-ci-filters.sh reads it (`ci_filter_coverage.step_workdir`);
  * a `go vet`, `go build`, `go list` or `go install` line runs no test and is skipped; a line it
    cannot read stops the run, since silently skipping a `go test` would hide a duplicate;
  * `-run "$pattern"` assigned from a `$(…)` call of `.github/scripts/go-tagged-tests.sh` is run,
    in the step's own directory, and its output used; a step looping over packages
    (`for pkg in ./core ./seed ./gcp`) is one `go test` per package, each with its own call;
  * `go list … | grep -Ev '…' | xargs go test …` is run as written, in the step's own directory,
    and the packages it prints are the command's packages;
  * a line that sets variables before its command (`GODEBUG=checkfinalizers=1 go test …`) or runs
    it under one of the wrappers GO_WRAPPERS names (`race_memory.py go test …`) is the go command
    behind them: neither changes which test functions run. A value holding `$` and a wrapper that
    tuple does not name are refused rather than guessed at;
  * a `Benchmark…` name a listing prints is left out: no step passes `-bench`, so `go test` builds
    those functions and runs none of them.

What it prints: the steps it expanded, the pairs more than one step runs, the declared exceptions
it applied and the ones it did not need, and every pair, one per line behind a `PAIR` marker for
the diff above. Exit 0 when no pair is run twice undeclared, 1 when one is, 2 when it cannot read
a step or a `go` command fails.
"""

from __future__ import annotations

import re
import shlex
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

import yaml
from ci_filter_coverage import called_workflows, script_lines, step_workdir

ROOT = Path(__file__).resolve().parents[2]
# Repository-relative, as the documents below are keyed and as every message names them.
WORKFLOW = ".github/workflows/ci.yaml"
# The two Go modules this check covers. A step of packages/daemon is another job's business.
MODULE_DIRS = ("packages/envoy", "packages/envoy/infra")


@dataclass(frozen=True)
class Exception_:
    """A pair two steps may run, and the requirement that makes it run twice.

    `steps` must be exactly the steps that run it, each named as `Step.key` names it - the
    repository-relative workflow file that holds the step, and the step's `name:` - `package` its
    import path, `functions` the function names (None: every function of that package those steps
    share). An entry whose `requirement` is empty is refused: a duplicate with no requirement
    written beside it is a duplicate, not an exception.
    """

    steps: frozenset[tuple[str, str]]
    package: str
    functions: tuple[str, ...] | None
    requirement: str


EXCEPTIONS = (
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-smoke.yaml", "Live tests of envoy-infra's host:Listener"),
            (".github/workflows/ci-job-envoy-infra-go.yaml", "Unit test envoy-infra"),
        }),
        package="github.com/sjawhar/envoy/infra/host",
        functions=None,
        requirement=(
            "R3 (dispatch://INFRA-1408/spec): envoy-smoke's pulumilive step over infra/host keeps "
            "running the whole package, untagged tests included, because envoy-smoke runs on a "
            "Dispatch-only change where envoy-infra-go does not, and in such a run those untagged "
            "tests would otherwise run nowhere."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-go.yaml", "Test what the race build cannot run at all or at full size"),
            (".github/workflows/ci-job-envoy-go-race-docs.yaml", "Race-test the document service"),
            (".github/workflows/ci-job-envoy-go-docs-finalizers.yaml", "Test the document service under the cleanup detector"),
        }),
        package="github.com/sjawhar/envoy/internal/dispatch/docs",
        functions=(
            "TestRenderReuseARandomisedEditStorm",
            "TestRenderReuseAReplicaReplaysTheRoomsFormattingExactly",
        ),
        requirement=(
            "R2 (dispatch://INFRA-1408/spec): the two render-reuse storms run fewer seeds and "
            "fewer edits under the race detector (raceEnabled), so envoy-go's plain step is their "
            "full-size run and envoy-go-race-docs is the detector's run of the shrunk one. The "
            "third step is the cleanup detector's, declared with the rest of the package below."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-go-race-docs.yaml", "Race-test the document service"),
            (".github/workflows/ci-job-envoy-go-docs-finalizers.yaml", "Test the document service under the cleanup detector"),
        }),
        package="github.com/sjawhar/envoy/internal/dispatch/docs",
        functions=None,
        requirement=(
            "A3 (dispatch://INFRA-1413/artifact/verification-md#b-k3): envoy-go-docs-finalizers "
            "runs this package in a Go mode no other step runs it in, GODEBUG=checkfinalizers=1, "
            "and with -v, because the package's own count of the services its tests left alive is "
            "printed from TestMain after they have run and a package-list run drops it. The mode "
            "is what the job is for, and a cleanup the detector reports can be any test's, so the "
            "lane takes the package whole rather than a -run of part of it."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-go.yaml", "Test what the race build cannot run at all or at full size"),
            (".github/workflows/ci-job-envoy-go-docs-finalizers.yaml", "Test the document service under the cleanup detector"),
        }),
        package="github.com/sjawhar/envoy/internal/dispatch/docs",
        functions=(
            "TestARoomFedOnlyByAgentWritesKeepsMemoryWithinTheBound",
            "TestUploadsOverOpenAnchorsKeepMemoryWithinTheBound",
            "TestUploadsToARoomInUseKeepMemoryWithinTheBound",
        ),
        requirement=(
            "R2 (dispatch://INFRA-1408/spec) and A3 (dispatch://INFRA-1413): these three bound a "
            "room's memory, which the race build cannot measure, so the race listing does not "
            "hold them and envoy-go's plain step is their one measured run. The finalizers lane "
            "runs them as part of the package under its own mode, for the reason above."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-airlock-lean.yaml", "Compare the Go airlock with the model over the seed corpus"),
            (".github/workflows/ci-job-airlock-lean.yaml", "Fuzz the Go airlock against the model"),
            (".github/workflows/ci-job-envoy-go.yaml", "Unit test envoy Go package"),
        }),
        package="github.com/sjawhar/envoy/internal/broker/airlock",
        functions=("FuzzAirlock",),
        requirement=(
            "R3 (dispatch://INFRA-1408/spec): airlock-lean holds the Go airlock to its Lean model, "
            "and its filter takes files envoy_go's does not - its own job file, "
            ".github/actions/install-elan/** and packages/envoy/scripts/airlock-lean-axioms.sh - "
            "so on such a change envoy-go does not run and the seed corpus would run nowhere. Its "
            "own two steps are the corpus comparison and the 120-second fuzz, which seeds from "
            "that same corpus, so both name the function; envoy-go runs the corpus as part of the "
            "package."
        ),
    ),
)

# `go test -list` prints one name per line, then a status line per package.
NAME = re.compile(r"^(Test|Fuzz|Example|Benchmark)[A-Za-z0-9_]*$")
STATUS = re.compile(r"^(ok|FAIL|\?)\s+(\S+)")
# A shell variable, as the steps write it: $pkg or ${pkg}.
SHELL_VAR = re.compile(r"\$(?:([A-Za-z_][A-Za-z0-9_]*)|\{([A-Za-z_][A-Za-z0-9_]*)\})")
FOR_LINE = re.compile(r"^for\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\s+(.+?)\s*;\s*do$")
ASSIGNMENT = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)=\$\((.+)\)$")
XARGS = re.compile(r"^(?P<lister>.+?)\s*\|\s*xargs\s+(?P<test>go\s+test\b.*)$")
TAGGED_TESTS = ".github/scripts/go-tagged-tests.sh"
# A variable a step sets before its command, as the steps write it: GODEBUG=checkfinalizers=1.
# A value holding `$` is left for the caller to refuse, since what it expands to is the shell's.
ENV_PREFIX = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=[^$]*$")
# The wrappers a step runs a go command under. Each bounds how the test binary runs and changes
# which tests it runs in no way, so the command behind it is read as though it stood alone; one
# this tuple does not name is refused rather than guessed at. Matched by the end of the word,
# since a step writes the path from its own working directory.
GO_WRAPPERS = (".github/scripts/race_memory.py",)
# go commands that run no test, so a step may hold one and this script need not read it.
NO_TEST_GO = ("build", "vet", "list", "install", "mod", "work", "env", "generate", "tool", "run")
# go test flags whose value is the next argument.
VALUE_FLAGS = ("-run", "-tags", "-timeout", "-count", "-parallel", "-cpu", "-bench", "-o",
               "-fuzz", "-fuzztime", "-shuffle", "-exec", "-coverprofile", "-covermode", "-p")


class Refused(Exception):
    """A shape this script will not guess at."""


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

    # The file and the step's name, which is what an EXCEPTIONS entry declares: a step name is
    # unique within the file that holds it, while a job id is ci.yaml's and the name of the step
    # may be another file's too (release-envoy-infra.yaml has its own "Unit test envoy-infra").
    @property
    def key(self) -> tuple[str, str]:
        return (self.workflow, self.name)

    @property
    def directory(self) -> Path:
        return ROOT / self.workdir


@dataclass
class GoTest:
    """One `go test` command of one step."""

    step: Step
    text: str
    packages: list[str]
    tags: str = ""
    race: bool = False
    run: str = ""
    resolved: list[str] = field(default_factory=list)  # import paths


def shell(script: str, cwd: Path, what: str) -> str:
    """A line of a step's own script, run in the step's own directory."""
    done = subprocess.run(["bash", "-c", f"set -euo pipefail\n{script}"],
                          cwd=cwd, capture_output=True, text=True)
    if done.returncode != 0:
        raise Refused(f"{what} exited {done.returncode}\n"
                      f"    $ {script}\n{indent(done.stderr or done.stdout)}")
    return done.stdout


def go(args: list[str], cwd: Path, what: str) -> str:
    done = subprocess.run(args, cwd=cwd, capture_output=True, text=True)
    if done.returncode != 0:
        hint = ("\n    -race needs CGO_ENABLED=1 and a C compiler."
                if "-race" in args and "cgo" in (done.stderr or "") else "")
        raise Refused(f"{what} exited {done.returncode}\n"
                      f"    $ {shlex.join(args)} (in {cwd}){hint}\n{indent(done.stderr)}")
    return done.stdout


def indent(text: str) -> str:
    return "".join(f"    | {line}\n" for line in (text or "").strip().splitlines())


def expand(text: str, variables: dict[str, str]) -> str:
    def one(match: re.Match[str]) -> str:
        name = match.group(1) or match.group(2)
        if name not in variables:
            raise Refused(f"the shell variable ${name} is set outside the step, or not at all")
        return variables[name]

    return SHELL_VAR.sub(one, text)


def documents() -> dict:
    """ci.yaml and every workflow a job of it calls, keyed by repository-relative path. A job of
    ci.yaml keeps its id, its `needs: changes` and its whole `if:` there and holds its steps in
    the `ci-job-<id>.yaml` its `uses:` names, so ci.yaml alone holds none of the `go test` steps
    below. The set is the one check-ci-filters.sh reads, derived with that check's own
    `called_workflows`; it is loaded here rather than through `ci_filter_coverage`'s
    `workflow_documents`, which roots itself at the current working directory and appends to the
    checker's problem list, because this script roots itself at its own path (the module header
    says why) and refuses instead."""
    found = {WORKFLOW: yaml.safe_load((ROOT / WORKFLOW).read_text()) or {}}
    for path, job_id in called_workflows(found[WORKFLOW]).items():
        if not (ROOT / path).is_file():
            raise Refused(f"job {job_id} of {WORKFLOW} calls {path}, which is not in the "
                          f"repository, so the steps it runs are read by nothing here")
        found[path] = yaml.safe_load((ROOT / path).read_text()) or {}
    return found


def steps_of(found: dict) -> list[Step]:
    """Every step of those documents whose run: holds a `go test` and whose directory is one of
    the two modules. The directory and the comment-free lines are read with check-ci-filters.sh's
    own helpers (ci_filter_coverage.step_workdir, script_lines), so this tool and the gate agree
    on what a step runs and where."""
    steps = []
    for path, document in found.items():
        for job, body in (document.get("jobs") or {}).items():
            for index, step in enumerate((body or {}).get("steps", []) or []):
                script = step.get("run")
                if not script:
                    continue
                workdir = step_workdir(document, body, step)
                if workdir not in MODULE_DIRS:
                    continue
                if not re.search(r"\bgo\s+test\b", "\n".join(script_lines(script))):
                    continue
                name = step.get("name") or f"step {index + 1}"
                steps.append(Step(workflow=path, job=job, name=name, workdir=workdir, script=script))
    return steps


def commands_of(step: Step) -> list[GoTest]:
    """Every `go test` a step's script starts, each with its packages, tags, -race and -run."""
    found: list[GoTest] = []
    read_block(step, step.script.splitlines(), {}, found)
    if not found:
        raise Refused(f"{step.label}: its run: holds `go test` and no command this script can read")
    return found


def read_block(step: Step, lines: list[str], variables: dict[str, str],
               found: list[GoTest]) -> None:
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
                read_block(step, body, {**variables, loop.group(1): value}, found)
            continue

        assignment = ASSIGNMENT.match(line)
        if assignment:
            name, inner = assignment.group(1), expand(assignment.group(2), variables)
            if TAGGED_TESTS not in inner:
                raise Refused(f"{step.label}: this script runs only a $(…) call of "
                              f"{TAGGED_TESTS}, not `{inner}`")
            variables[name] = shell(inner, step.directory, f"{step.label}: {inner}").strip()
            continue

        piped = XARGS.match(line)
        if piped:
            lister = expand(piped.group("lister"), variables)
            packages = shell(lister, step.directory, f"{step.label}: {lister}").split()
            if not packages:
                raise Refused(f"{step.label}: `{lister}` printed no package")
            found.append(parse_go_test(step, expand(piped.group("test"), variables), packages))
            continue

        words, command = go_command(line)
        if len(words) > 1 and words[0] == "go" and words[1] in NO_TEST_GO:
            continue
        if len(words) > 1 and words[0] == "go" and words[1] == "test":
            found.append(parse_go_test(step, expand(command, variables), []))
            continue
        raise Refused(f"{step.label}: this script cannot read `{line}`")


def go_command(line: str) -> tuple[list[str], str]:
    """The command a line runs, with the variables it sets before it and the wrapper it runs it
    under dropped: its words, and its text with those words removed.

    A step writes `GODEBUG=checkfinalizers=1 go test …` and
    `RACE_MEM_LIMIT_KB=8388608 ../../.github/scripts/race_memory.py go test …`. Neither the
    variables nor the wrapper changes which test functions the command runs, which is all this
    script reads, so both are dropped and what is left is the go command. A value holding `$`,
    and a wrapper GO_WRAPPERS does not name, are left where they are, so the caller refuses the
    line rather than guessing. The text is cut word by word off the line itself, so the quoting
    of what is left - a `-run` pattern above all - is the line's own.
    """
    rest = line.strip()
    while True:
        head = rest.split(None, 1)
        if len(head) == 2 and ENV_PREFIX.match(head[0]):
            rest = head[1]
            continue
        break
    head = rest.split(None, 1)
    if len(head) == 2 and head[0].endswith(GO_WRAPPERS) and head[1].split(None, 1)[0] == "go":
        rest = head[1]
    return rest.split(), rest


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


def parse_go_test(step: Step, text: str, packages: list[str]) -> GoTest:
    """The packages, -tags, -race and -run of one `go test` command."""
    command = GoTest(step=step, text=text, packages=list(packages))
    words = shlex.split(text)
    position = words.index("test") + 1
    while position < len(words):
        word = words[position]
        position += 1
        if word == "-race":
            command.race = True
        elif word.startswith("-"):
            flag, _, value = word.partition("=")
            if not value and flag in VALUE_FLAGS:
                value = words[position]
                position += 1
            if flag == "-run":
                command.run = value
            elif flag == "-tags":
                command.tags = value
            elif flag == "-bench":
                raise Refused(f"{step.label}: `{text}` runs benchmarks, which this script "
                              "does not expand")
        else:
            command.packages.append(word)
    if not command.packages:
        raise Refused(f"{step.label}: `{text}` names no package")
    return command


def build_flags(command: GoTest) -> list[str]:
    flags = []
    if command.tags:
        flags += ["-tags", command.tags]
    if command.race:
        flags.append("-race")
    return flags


def resolve(command: GoTest) -> None:
    """The import path of each package argument, with ./... expanded under the step's flags."""
    out = go(["go", "list", "-f", "{{.ImportPath}}", *build_flags(command), *command.packages],
             command.step.directory, f"{command.step.label}: go list of `{command.text}`")
    command.resolved = out.split()
    if not command.resolved:
        raise Refused(f"{command.step.label}: `{command.text}` lists no package")


def list_tests(command: GoTest) -> dict[str, list[str]]:
    """What each package of one command runs: `go test -list` under the command's own flags."""
    pattern = command.run or ".*"
    out = go(["go", "test", "-count=1", "-list", pattern, *build_flags(command), *command.resolved],
             command.step.directory, f"{command.step.label}: go test -list of `{command.text}`")
    names: dict[str, list[str]] = {}
    pending: list[str] = []
    for line in out.splitlines():
        line = line.strip()
        if NAME.match(line):
            pending.append(line)
            continue
        status = STATUS.match(line)
        if not status:
            continue  # a test binary's own output, which names no test
        kind, package = status.group(1), status.group(2)
        if kind == "FAIL":
            raise Refused(f"{command.step.label}: `go test -list` failed on {package}\n{indent(out)}")
        names[package] = pending
        pending = []
    missing = [p for p in command.resolved if p not in names]
    if missing:
        raise Refused(f"{command.step.label}: `go test -list` printed no result line for "
                      f"{', '.join(missing)}\n{indent(out)}")
    return names


def expansion() -> tuple[list[Step], dict[tuple[str, str], list[Step]], list[str]]:
    """Every (package, function) pair, with the steps that run it, and a line per step."""
    found = documents()
    steps = steps_of(found)
    if not steps:
        raise Refused(f"{WORKFLOW} and the {len(found) - 1} workflow(s) its jobs call have no "
                      f"`go test` step in {' or '.join(MODULE_DIRS)}")
    pairs: dict[tuple[str, str], list[Step]] = {}
    report = []
    for step in steps:
        commands = commands_of(step)
        functions = 0
        packages: set[str] = set()
        for command in commands:
            resolve(command)
            for package, names in list_tests(command).items():
                packages.add(package)
                for name in names:
                    if name.startswith("Benchmark"):
                        continue  # no step passes -bench, so no step runs it
                    functions += 1
                    runners = pairs.setdefault((package, name), [])
                    if step not in runners:
                        runners.append(step)
        report.append(f"  {step.label}")
        report.append(f"      in {step.workdir}: {len(commands)} go test command(s), "
                      f"{len(packages)} package(s), {functions} function(s)")
        for command in commands:
            report.append(f"      -run {command.run or '(none)'}"
                          f"{' -tags ' + command.tags if command.tags else ''}"
                          f"{' -race' if command.race else ''}"
                          f": {len(command.resolved)} package(s)")
    return steps, pairs, report


def match_exception(package: str, name: str, runners: list[Step]) -> Exception_ | None:
    keys = frozenset(step.key for step in runners)
    for entry in EXCEPTIONS:
        if entry.steps != keys or entry.package != package:
            continue
        if entry.functions is None or name in entry.functions:
            return entry
    return None


def main() -> int:
    if len(sys.argv) > 1:
        print(f"usage: {Path(sys.argv[0]).name} (no arguments)", file=sys.stderr)
        return 2
    for entry in EXCEPTIONS:
        if not entry.requirement.strip():
            print(f"exception over {entry.package} names no requirement, so it is not one",
                  file=sys.stderr)
            return 2

    try:
        steps, pairs, report = expansion()
    except Refused as refusal:
        print(f"refused: {refusal}", file=sys.stderr)
        return 2

    print(f"# go test step expansion of {WORKFLOW} and the workflows its jobs call, under {ROOT}")
    print("# Every test function each go test step of packages/envoy and packages/envoy/infra")
    print("# runs, through go test -list over the step's own packages, -tags, -race and -run.")
    print("# Run it at two commits and compare the pair sets:")
    print("#   grep '^PAIR' head.txt > head.pairs; grep '^PAIR' base.txt > base.pairs")
    print("#   diff base.pairs head.pairs      (empty: the two commits run the same functions)")
    print("# Check k1 of dispatch://INFRA-1408. No CI job runs this script; the module header")
    print("# says why, and what it needs (CGO_ENABLED=1 for the -race steps, no service).")
    print()
    print(f"== steps expanded ({len(steps)}) ==")
    print("\n".join(report))
    print()

    duplicates = sorted((package, name, runners)
                        for (package, name), runners in pairs.items() if len(runners) > 1)
    numbered = {entry: number for number, entry in enumerate(EXCEPTIONS, start=1)}
    applied: dict[Exception_, list[str]] = {entry: [] for entry in EXCEPTIONS}
    undeclared = []
    print(f"== pairs more than one step runs ({len(duplicates)}) ==")
    if not duplicates:
        print("  none")
    for package, name, runners in duplicates:
        entry = match_exception(package, name, runners)
        print(f"  {package} {name}")
        for step in sorted(runners, key=lambda s: s.label):
            print(f"      {step.label}")
        if entry is None:
            undeclared.append((package, name))
            print("      undeclared: no exception covers this pair")
        else:
            applied[entry].append(f"{package} {name}")
            print(f"      declared by exception {numbered[entry]} below")
    print()

    print(f"== declared exceptions ({len(EXCEPTIONS)}) ==")
    for entry in EXCEPTIONS:
        names = ", ".join(entry.functions) if entry.functions else "every function both steps run"
        print(f"  exception {numbered[entry]}: {entry.package}: {names}")
        for job, step in sorted(entry.steps):
            print(f"      step: {job} / {step}")
        print(f"      requirement: {entry.requirement}")
        if applied[entry]:
            print(f"      applied to {len(applied[entry])} pair(s):")
            for pair in applied[entry]:
                print(f"        {pair}")
        else:
            print("      not needed: no pair of these steps matched it")
    print()

    print(f"== every pair ({len(pairs)}) ==")
    for package, name in sorted(pairs):
        print(f"PAIR\t{package}\t{name}")
    print()

    if undeclared:
        print(f"{len(undeclared)} pair(s) run by more than one step and declared by none; "
              "each is a test two steps of one run run.", file=sys.stderr)
        return 1
    print(f"No undeclared duplicate: {len(pairs)} pair(s) over {len(steps)} step(s), "
          f"{len(duplicates)} of them declared.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
