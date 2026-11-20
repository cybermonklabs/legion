"""The `go test` commands of ci.yaml's router, read off the workflow text: the module both
go-test-step-expansion.py and check-test-reach.sh import.

It loads `.github/workflows/ci.yaml` and every workflow a job of it calls (`documents`), picks
each step whose `run:` holds a `go test` in one of the Go modules (`steps_of`), and reads each
such step line by line into its `go test` commands (`commands_of`, `read_block`): packages,
`-tags`, `-race`, `-run` and `-skip`, through the variables a line sets before its command, the
wrappers GO_WRAPPERS names, `for` loops over packages, and the scripts AUDITED_SCRIPTS reads.

Two shapes of a line need an answer from outside the line: a `-run "$pattern"` assigned from a
`$(…)` call of `.github/scripts/go-tagged-tests.sh`, and the package list `go list … | grep -Ev
'…' | xargs go test …` pipes into its command. `read_block` asks its `answer` for both, with the
step and the text of the call or the lister. go-test-step-expansion.py answers by running the text
in the step's directory (`shell_answer`); check-test-reach.sh, which runs no `go` command,
answers by reading the source the way those two commands read it. A line that is neither, and
holds no command this module reads, is refused rather than skipped: a `go test` silently passed
over would leave both readers wrong about what a step runs.

It needs PyYAML, and the helpers of ci_filter_coverage.py beside it (`called_workflows`,
`script_lines`, `step_workdir`), so the readers and check-ci-filters.sh agree on which files a
job's steps live in, which lines a script holds and which directory a step runs in.
"""

from __future__ import annotations

import hashlib
import re
import shlex
import subprocess
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path

import yaml
from ci_filter_coverage import called_workflows, script_lines, step_workdir

# The repository holding this file: its own path, not resolved through a symlink, so a fixture
# tree that links this file in is the repository it reads.
ROOT = Path(__file__).absolute().parents[2]
# Repository-relative, as the documents below are keyed and as every message names them.
WORKFLOW = ".github/workflows/ci.yaml"
# The three Go modules whose steps are read. packages/daemon joined them with the first pair of
# steps in this repository that share a package through a `-run` and a `-skip` naming the same
# tests (ci-job-plugin-pack.yaml and ci-job-daemon-go.yaml's race step): the pairing is what makes
# each of them run exactly once, and a reader that cannot see both steps cannot see it.
MODULE_DIRS = ("packages/envoy", "packages/envoy/infra", "packages/daemon")

# A test function's name, as `go test -list` prints it and as a `-skip` alternation names it.
NAME = re.compile(r"^(Test|Fuzz|Example|Benchmark)[A-Za-z0-9_]*$")
# A shell variable, as the steps write it: $pkg or ${pkg}.
SHELL_VAR = re.compile(r"\$(?:([A-Za-z_][A-Za-z0-9_]*)|\{([A-Za-z_][A-Za-z0-9_]*)\})")
FOR_LINE = re.compile(r"^for\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\s+(.+?)\s*;\s*do$")
ASSIGNMENT = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)=\$\((.+)\)$")
XARGS = re.compile(r"^(?P<lister>.+?)\s*\|\s*xargs\s+(?P<test>go\s+test\b.*)$")
TAGGED_TESTS = ".github/scripts/go-tagged-tests.sh"


@dataclass(frozen=True)
class AuditedScript:
    """A script a step runs, read by a person: the module it moves into before its tests, the one
    `go test` it runs there for each argument list a step passes it (the run:'s text after the
    script's path, "" for none), and the SHA-256 of the text that reading was of."""

    module: str
    commands: dict[str, str]
    sha256: str


# The tests envoy-go's "Count the store-form rows on each backend" names, in the order its run:
# passes them to backend-count.sh; ci_job_reads.BACKEND_COUNT_TESTS, which the unit and
# database-skips steps' skips of them are built from, holds the same names by package.
BACKEND_COUNT_TESTS = (
    "TestEverySecretFormNamesTheSourceItUses",
    "TestNoSecretFormAsksTheBrokerForACredential",
    "TestUseCommandOutsideASessionReachesTheMachineSource",
    "TestTheChainSkipsAbsenceAndStopsOnABrokenSource",
    "TestSettingsNameTheGoogleExchangeAudienceOnEitherStore",
)

# Each script a step of ci.yaml's router runs as its whole run: line, read by a person, with the
# one `go test` it runs for each argument list a step passes it. A step that runs any other script
# is read from its own run: alone, so a `go test` inside that script is not seen: a step that runs
# a script from the repository root and holds no `go test` of its own is passed over (steps_of),
# which is how envoy-go's "Count the store-form rows on each backend" (backend-count.sh) was read
# as running nothing until its entry below, and its five tests' second run was seen by no reader.
AUDITED_SCRIPTS = {
    # It moves into packages/envoy (`cd "$(dirname "$0")/.."`) and runs one `go test -json
    # -count=1` over its packages, its stdout through tee into the report and through jq to the
    # log, then counts the skips in its report. With no package argument it
    # runs its DEFAULT_PACKAGES, ./internal/broker/... and ./cmd/broker/, less the import paths its
    # DEFAULT_EXCEPT's `grep -Ev '/internal/broker/natscallout$'` takes out of `go list` of them (the
    # race lane's packages under the tree, ci_job_reads.DATABASE_SKIPS_PATTERN), with no -tags,
    # -race, -run or -skip, under -timeout "${GO_TEST_TIMEOUT:-15m}". A package argument replaces
    # those packages, `--race` adds -race, `--skip P` adds -skip P, `--timeout D` sets the -timeout,
    # and `--var NAME` decides only whether it runs at all. With `--report FILE` it reads a report
    # and runs no test. envoy-go's count step passes the first argument list below, whose command
    # is that run in the listing form this module reads, and envoy-go-race-api's race step the
    # second. The first's --skip leaves out the backend count's test of ./internal/broker/api/.
    "packages/envoy/scripts/database-skips.sh": AuditedScript(
        module="packages/envoy",
        commands={
            "--timeout 20m --skip '^(TestSettingsNameTheGoogleExchangeAudienceOnEitherStore)$'":
                "go list ./internal/broker/... ./cmd/broker/ | grep -Ev '/internal/broker/natscallout$' | "
                "xargs go test -json -count=1 -skip '^(TestSettingsNameTheGoogleExchangeAudienceOnEitherStore)$' "
                "-timeout 20m",
            "--var DISPATCH_TEST_DATABASE_URL --var BROKER_TEST_DATABASE_URL --race --timeout 70m ./internal/dispatch/api/":
                "go test -json -count=1 -race -timeout 70m ./internal/dispatch/api/",
        },
        sha256="4200acaf42c44cc31bae4024e92930421961a0aefca4056f20eec0fc763c3f15",
    ),
    # It moves into packages/envoy (`cd "$(dirname "$0")/.."`) and runs one `go test -json
    # -count=1 -timeout "${GO_TEST_TIMEOUT:-5m}" -run '^(TEST|…)$'` over the packages before its
    # `--`, the -run built from the test names after it, its stdout through tee into its events
    # file and through jq to the log, then counts each test's `aws` and `gcp` passes. GO_TEST_SKIP
    # adds a -skip, which no step sets, and envoy-go's job sets neither variable. envoy-go's count
    # step passes the argument list below.
    "packages/envoy/scripts/backend-count.sh": AuditedScript(
        module="packages/envoy",
        commands={
            "./cmd/agent-secrets/ ./internal/broker/api/ -- " + " ".join(BACKEND_COUNT_TESTS):
                f"go test -json -count=1 -timeout 5m -run '^({'|'.join(BACKEND_COUNT_TESTS)})$' "
                "./cmd/agent-secrets/ ./internal/broker/api/",
        },
        sha256="315c12e15699049b9bc8583ddd1f6af4f21aa6ceddcc74216a3e052c2927f516",
    ),
}
# A variable a step sets before its command, as the steps write it: GODEBUG=checkfinalizers=1.
# A value holding `$` is left for the caller to refuse, since what it expands to is the shell's.
ENV_PREFIX = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=[^$]*$")
# The wrappers a step runs a go command under. Each bounds how the test binary runs and changes
# which tests it runs in no way, so the command behind it is read as though it stood alone; one
# this tuple does not name is refused rather than guessed at. Matched by the end of the word,
# since a step writes the path from its own working directory.
GO_WRAPPERS = (".github/scripts/race_memory.py",)
# go commands that run no test, so a step may hold one and the reader need not read it.
NO_TEST_GO = ("build", "vet", "list", "install", "mod", "work", "env", "generate", "tool", "run")
# The commands a line that names no `go` may still start one through, or move the directory a
# later `go test` reads its packages from with: such a line is refused like any other this module
# cannot read. Any other line that holds no `go` word - a `docker exec … createdb` that makes the
# tests' database, an `echo` - starts no go command the reader could see, and is passed over.
NOT_PASSED_OVER = ("cd", "pushd", "popd", "source", ".", "eval", "exec", "bash", "sh", "env", "make",
                   "timeout", "xargs", "nohup", "sudo")
GO_WORD = re.compile(r"(?<![\w./-])go(?![\w.-])")
# go test flags whose value is the next argument.
VALUE_FLAGS = ("-run", "-skip", "-tags", "-timeout", "-count", "-parallel", "-cpu", "-bench",
               "-o", "-fuzz", "-fuzztime", "-shuffle", "-exec", "-coverprofile", "-covermode",
               "-p")
# The one `-skip` shape this module reads: the anchored alternation of whole test names
# ci_job_reads.py builds for every step of this repository that carries one, `^(Name|Name|…)$`.
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

    # The file and the step's name, which is what an exception of go-test-step-expansion.py
    # declares: a step name is unique within the file that holds it, while a job id is ci.yaml's
    # and the name of the step may be another file's too (release-envoy-infra.yaml has its own
    # "Unit test envoy-infra").
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
    skip: str = ""  # the pattern as the step writes it; skip_names reads the names out of it
    resolved: list[str] = field(default_factory=list)  # import paths


# How a reader answers the two lines read_block cannot read off their own text: given the step
# and the text of the `$(…)` call or the `go list …` lister, what it prints.
Answer = Callable[[Step, str], str]


def indent(text: str) -> str:
    return "".join(f"    | {line}\n" for line in (text or "").strip().splitlines())


def shell_answer(step: Step, script: str) -> str:
    """A line of a step's own script, run in the step's own directory."""
    done = subprocess.run(["bash", "-c", f"set -euo pipefail\n{script}"],
                          cwd=step.directory, capture_output=True, text=True)
    if done.returncode != 0:
        raise Refused(f"{step.label}: {script} exited {done.returncode}\n"
                      f"    $ {script}\n{indent(done.stderr or done.stdout)}")
    return done.stdout


def expand(text: str, variables: dict[str, str]) -> str:
    def one(match: re.Match[str]) -> str:
        name = match.group(1) or match.group(2)
        if name not in variables:
            raise Refused(f"the shell variable ${name} is set outside the step, or not at all")
        return variables[name]

    return SHELL_VAR.sub(one, text)


def documents() -> dict:
    """ci.yaml and every workflow a job of it calls, keyed by repository-relative path. A job of
    ci.yaml keeps its id, its `needs: changes` and its whole `if:` there and holds its steps in the
    `ci-job-<id>.yaml` its `uses:` names, so ci.yaml alone holds none of the `go test` steps. The
    set is the one check-ci-filters.sh reads, derived with that check's own `called_workflows`;
    it is loaded here rather than through `ci_filter_coverage`'s `workflow_documents`, which
    roots itself at the current working directory and appends to the checker's problem list,
    because this module roots itself at its own path and refuses instead."""
    found = {WORKFLOW: yaml.safe_load((ROOT / WORKFLOW).read_text()) or {}}
    for path, job_id in called_workflows(found[WORKFLOW]).items():
        if not (ROOT / path).is_file():
            raise Refused(f"job {job_id} of {WORKFLOW} calls {path}, which is not in the "
                          f"repository, so the steps it runs are read by nothing here")
        found[path] = yaml.safe_load((ROOT / path).read_text()) or {}
    return found


def steps_of(found: dict) -> list[Step]:
    """Every step of those documents whose run: holds a `go test` and whose directory is one of
    the three modules, and every step that runs an AUDITED_SCRIPTS script. The directory and the
    comment-free lines are read with check-ci-filters.sh's own helpers
    (ci_filter_coverage.step_workdir, script_lines), so the readers and the gate agree on what a
    step runs and where."""
    steps = []
    for path, document in found.items():
        for job, body in (document.get("jobs") or {}).items():
            for index, step in enumerate((body or {}).get("steps", []) or []):
                script = step.get("run")
                if not script:
                    continue
                workdir = step_workdir(document, body, step)
                name = step.get("name") or f"step {index + 1}"
                audited = audited_step(path, job, name, workdir, script)
                if audited is not None:
                    steps.append(audited)
                    continue
                if workdir not in MODULE_DIRS:
                    continue
                if not re.search(r"\bgo\s+test\b", "\n".join(script_lines(script))):
                    continue
                steps.append(Step(workflow=path, job=job, name=name, workdir=workdir, script=script))
    return steps


def spellings(path: str, workdir: str) -> set[str]:
    """The ways a run: in workdir names path, a repository-relative file: its path from workdir
    behind `./`, and bare where that path holds a `/` (a bare name without one is looked up on
    PATH by the shell, not in the directory)."""
    if workdir in ("", "."):
        relative = path
    elif path.startswith(f"{workdir}/"):
        relative = path[len(workdir) + 1:]
    else:
        return set()
    return {f"./{relative}"} | ({relative} if "/" in relative else set())


def audited_step(workflow: str, job: str, name: str, workdir: str, script: str) -> Step | None:
    """The step a run: that is the path of an AUDITED_SCRIPTS script makes, alone or followed by
    an argument list its entry reads, read as that entry's command for those arguments in its
    module, or None where the run's text holds no such script's file name. A run whose text holds
    one anywhere else - an operator, another command, a `$(…)`, a comment, another line, an
    argument list the entry does not read - is refused rather than read or skipped: the test is
    on the raw text, before any shell reading, so no quoting or substitution hides the script
    from it. So is a script whose text no longer has its entry's SHA-256."""
    label = f"{workflow}'s {job} / {name}"
    named = [path for path in AUDITED_SCRIPTS if Path(path).name in script]
    if not named:
        return None
    # Only the shell's own blanks and the line's end: Python's strip() also takes Unicode spaces,
    # which bash reads as part of the name.
    alone = script.strip(" \t\n")
    # The path is the text up to the first space, and the arguments the text after it, compared
    # with each listed argument list as written: one space between words, nothing else.
    spelled, _, arguments = alone.partition(" ")
    # The literal spelling, compared with no path normalization: `path/` and `path/.` resolve to
    # the file in Python and name nothing the shell can run.
    matches = [path for path in named if spelled in spellings(path, workdir)]
    if (len(named) != 1 or len(matches) != 1 or any(c.isspace() for c in spelled)
            or arguments not in AUDITED_SCRIPTS[matches[0]].commands):
        raise Refused(f"{label}: names {', '.join(named)} in a form no AUDITED_SCRIPTS entry "
                      "reads: an entry reads a run: that is that one path, alone or followed by "
                      "an argument list the entry lists")
    path = matches[0]
    entry = AUDITED_SCRIPTS[path]
    digest = hashlib.sha256((ROOT / path).read_bytes()).hexdigest()
    if digest != entry.sha256:
        raise Refused(f"{label}: {path} has SHA-256 {digest}, and its AUDITED_SCRIPTS entry was "
                      f"read from {entry.sha256}; read the script again and update the entry's "
                      "commands and digest in .github/scripts/go_test_steps.py")
    return Step(workflow=workflow, job=job, name=name, workdir=entry.module, script=entry.commands[arguments])


def commands_of(step: Step, answer: Answer) -> list[GoTest]:
    """Every `go test` a step's script starts, each with its packages, tags, -race and -run."""
    found: list[GoTest] = []
    read_block(step, step.script.splitlines(), {}, found, answer)
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
            if TAGGED_TESTS not in inner:
                raise Refused(f"{step.label}: this reader reads only a $(…) call of "
                              f"{TAGGED_TESTS}, not `{inner}`")
            variables[name] = answer(step, inner).strip()
            continue

        piped = XARGS.match(line)
        if piped:
            lister = expand(piped.group("lister"), variables)
            packages = answer(step, lister).split()
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
        if (words and not GO_WORD.search(line) and words[0] not in NOT_PASSED_OVER
                and "/" not in words[0] and not words[0].endswith((".sh", ".py"))):
            continue
        raise Refused(f"{step.label}: this reader cannot read `{line}`")


def go_command(line: str) -> tuple[list[str], str]:
    """The command a line runs, with the variables it sets before it and the wrapper it runs it
    under dropped: its words, and its text with those words removed.

    A step writes `GODEBUG=checkfinalizers=1 go test …` and
    `RACE_MEM_LIMIT_KB=8388608 ../../.github/scripts/race_memory.py go test …`. Neither the
    variables nor the wrapper changes which test functions the command runs, which is all the
    readers read, so both are dropped and what is left is the go command. A value holding `$`,
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


def skip_names(pattern: str) -> list[str] | None:
    """The test names an anchored alternation leaves out, or None for a pattern of any other
    shape, which the caller refuses by name rather than guessing at."""
    match = SKIP_ALTERNATION.match(pattern)
    if not match:
        return None
    names = match.group(1).split("|")
    return names if all(NAME.match(name) for name in names) else None


def parse_go_test(step: Step, text: str, packages: list[str]) -> GoTest:
    """The packages, -tags, -race, -run and -skip of one `go test` command. The words after
    `-args` are the test binary's own, so the reading stops there."""
    command = GoTest(step=step, text=text, packages=list(packages))
    words = shlex.split(text)
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
            elif flag == "-skip":
                if skip_names(value) is None:
                    raise Refused(f"{step.label}: `{text}` skips by `{value}`, and this reader "
                                  "reads only the anchored alternation of whole test names, "
                                  "`^(Name|Name|…)$`, that ci_job_reads.py writes")
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
