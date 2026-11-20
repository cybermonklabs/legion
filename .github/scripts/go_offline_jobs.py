#!/usr/bin/env python3
"""Every Go job of a required workflow reads the module cache alone.

The rule. Every step of a Go job of a required workflow runs with GOPROXY=off and
GOFLAGS=-mod=readonly, so a go command that needs a module the restored module cache lacks fails
the step at once, naming that module, and downloads nothing; the job's fetch step
(.github/scripts/go-fetch.sh, which takes its proxy from GO_FETCH_PROXY) fills the cache before.
A step that must go online says why.

What it reads.
- The required workflows: .github/workflows/ci.yaml and every local workflow a job of one of them
  calls (`uses: ./.github/workflows/<file>` at job level), followed transitively. Every job of
  them with `steps:` is read; no other workflow is.
- A Go job: a job with a step that uses actions/setup-go; or with a step that uses a local action
  (`uses: ./<directory>`) whose action.yml or action.yaml has such a step, or uses a local action
  that does; or with a run: step whose text, comment lines aside, starts a go command (`go`, or a
  path ending `/go`, then `-C <dir>` or not, then one of GO_SUBCOMMANDS) or runs go-fetch.sh. A
  job with none of these is not read. A local action under the path: of a second checkout of
  this repository (actions/checkout with no repository:) is read from this tree, and a called
  workflow, a local action or a file that cannot be read is refused.
- A Go job's settings: the workflow's env: and the job's env: over it. GOPROXY must be `off` and
  GOFLAGS must hold the word -mod=readonly. A job that lacks either is refused once, at the job's
  line, naming its first Go step and what it lacks. A value is read as the YAML text it is
  written as, so `off` and `"off"` read alike.
- Each step of a Go job sends a setting online when its own env: sets GOPROXY to anything but
  `off` or GOFLAGS to a value without -mod=readonly, or sets either to nothing; or when a
  non-comment line of its run: assigns or exports GOPROXY or GOFLAGS (GOPROXY=off, and a GOFLAGS
  that holds -mod=readonly, aside), removes one (`unset`, `env -u`), or writes one to
  $GITHUB_ENV. Such a step passes only with a comment line holding `go-offline: <why>` (some text
  after the colon) inside its own YAML block or in the REASON_WINDOW lines above its first line,
  below the step before it; it is refused at its line otherwise. The summary names each step that
  passes so.

What it does not see.
- A composite action's own env:, its run: lines, and a $GITHUB_ENV write inside one: an action
  makes a job a Go job, and nothing an action sets is read.
- A script's own exports: a step that runs a script which sets GOPROXY is read as its run: line.
- A job whose only go command runs inside a script its steps start, with no setup-go: no Go job.
- A workflow of another repository that a job calls, an env: written as one expression, a
  container's env, and `env -i`.
- A run: is read a line at a time, not parsed as a shell: a go command or an assignment inside a
  quoted string counts as one.

Interface. check(root) -> (problems, summary): problems is a list of (path, line, message), the
path relative to root and the line 1-based, of the job or step the message names; summary is one
line: `N Go jobs of M required workflows read, each with GOPROXY=off and GOFLAGS=-mod=readonly; K
online steps with a reason: <file job "step">; ...` (or `(none)`), `P problems over N Go jobs of M
required workflows read` when there are problems, and a line starting `UNKNOWN: ` that says what
it could not read when it read no required workflow or no Go job. The CLI,
`python3 .github/scripts/go_offline_jobs.py [ROOT]` (ROOT defaults to the repository this script
sits in), prints each problem as a GitHub error annotation and then the summary; it exits 1 on a
problem, 2 when the summary is UNKNOWN and 0 otherwise. Standard library and PyYAML alone.
"""

from __future__ import annotations

import os
import posixpath
import re
import sys
from collections import deque
from pathlib import Path

import yaml

CI = ".github/workflows/ci.yaml"
SETTINGS = ("GOPROXY", "GOFLAGS")
OFFLINE = {"GOPROXY": ("GOPROXY=off", 'GOPROXY: "off"'), "GOFLAGS": ("GOFLAGS=-mod=readonly", "GOFLAGS: -mod=readonly")}
# How many lines above a step's first line a go-offline reason may sit.
REASON_WINDOW = 5
GO_SUBCOMMANDS = ("build", "run", "list", "install", "get", "mod", "test", "vet", "generate", "work", "env", "tool")
NULL = "tag:yaml.org,2002:null"

# Where a shell word starts: the line's start, or after a character the shell splits words at.
WORD_START = r"(?:^|(?<=[\s;&|(){}`!]))"
SETUP_GO = re.compile(r"^actions/setup-go(?:@|$)", re.IGNORECASE)
CHECKOUT = re.compile(r"^actions/checkout(?:@|$)", re.IGNORECASE)
THIS_REPOSITORY = "${{github.repository}}"
GO_COMMAND = re.compile(
    WORD_START + r"""["']?(?:[^\s"';&|()`]*/)?go["']?(?:\s+-C(?:=|\s+)\S+)?\s+(?:"""
    + "|".join(GO_SUBCOMMANDS) + r""")(?=$|[\s;&|)`])""")
GO_FETCH = re.compile(r"go-fetch\.sh(?![\w.-])")
ASSIGNMENT = re.compile(WORD_START + r"""(GOPROXY|GOFLAGS)=("[^"]*"|'[^']*'|[^\s;&|)`]*)""")
UNSET = re.compile(WORD_START + r"unset((?:[ \t]+[^\s;&|)`]+)+)")
ENV_COMMAND = re.compile(WORD_START + r"env((?:[ \t]+[^\s;&|)`]+)+)")
ENV_UNSET = re.compile(r"(?:^|\s)(?:-u[ \t]*|--unset[= \t][ \t]*)(GOPROXY|GOFLAGS)(?!\w)")
SETTING = re.compile(r"(?<!\w)(GOPROXY|GOFLAGS)(?!\w)")
REASON = re.compile(r"^\s*#.*\bgo-offline:[ \t]*\S")


# ---------------------------------------------------------------- reading the YAML as nodes
def get(node, key: str):
    """(key node, value node) of `key` in a mapping node, the last if it repeats; (None, None)
    when it is absent or the node is no mapping."""
    found = (None, None)
    if isinstance(node, yaml.MappingNode):
        for key_node, value_node in node.value:
            if isinstance(key_node, yaml.ScalarNode) and key_node.value == key:
                found = (key_node, value_node)
    return found


def value(node, key: str):
    return get(node, key)[1]


def text(node) -> str | None:
    """A scalar's text as written, '' for a null; None for a mapping, a sequence or nothing."""
    if not isinstance(node, yaml.ScalarNode):
        return None
    return "" if node.tag == NULL else node.value


def env_of(node) -> dict:
    """{name: its text} of an env: mapping; '' for a value that is null or no scalar."""
    if not isinstance(node, yaml.MappingNode):
        return {}
    return {key.value: text(val) or "" for key, val in node.value if isinstance(key, yaml.ScalarNode)}


def content_end(node, lines: list) -> int:
    """The 0-based line after the last line of the node's own text. A collection ends with its
    last entry, so the comment lines between one step and the next are the next one's; a block
    scalar's end mark sits on the line after its text."""
    if isinstance(node, yaml.MappingNode) and node.value:
        return max(content_end(part, lines) for pair in node.value for part in pair)
    if isinstance(node, yaml.SequenceNode) and node.value:
        return content_end(node.value[-1], lines)
    mark = node.end_mark
    before = lines[mark.line][:mark.column] if mark.line < len(lines) else ""
    return mark.line if not before.strip() else mark.line + 1


def script_lines(run: str) -> list:
    """The lines of a run:, stripped, comment and blank lines aside."""
    return [line.strip() for line in run.splitlines() if line.strip() and not line.lstrip().startswith("#")]


def unquote(word: str) -> str:
    if len(word) >= 2 and word[0] == word[-1] and word[0] in "\"'":
        return word[1:-1]
    return word


def offline(name: str, given: str) -> bool:
    return given == "off" if name == "GOPROXY" else "-mod=readonly" in given.split()


def shown(env: dict, name: str) -> str:
    """A setting's value as a message gives it: quoted, or `unset` when no env: names it."""
    return f'"{env[name]}"' if name in env else "unset"


def runs_go(run: str) -> bool:
    return any(GO_COMMAND.search(line) or GO_FETCH.search(line) for line in script_lines(run))


def checkout_paths(steps: list) -> list:
    """The path: of each actions/checkout step of a job that checks out this repository (no
    repository:, or the run's own) into a directory of the workspace other than its root. A
    local action under one is read from this tree: the same repository, at this tree's commit
    rather than the one that checkout names."""
    paths = []
    for step in steps:
        given = value(step, "with")
        repository = re.sub(r"\s", "", text(value(given, "repository")) or THIS_REPOSITORY)
        path = posixpath.normpath((text(value(given, "path")) or ".").strip())
        if CHECKOUT.match((text(value(step, "uses")) or "").strip()) and repository == THIS_REPOSITORY and path != ".":
            paths.append(path)
    return paths


def step_name(step) -> str:
    """What the run's log calls a step: its name, else what it uses, else `Run <first line>`."""
    for key in ("name", "uses"):
        given = text(value(step, key))
        if given:
            return given
    first = next(iter(script_lines(text(value(step, "run")) or "")), "")
    return f"Run {first}" if first else f"the step at line {step.start_mark.line + 1}"


def run_overrides(line: str) -> list:
    """How one line of a run: sends GOPROXY or GOFLAGS online."""
    if "GITHUB_ENV" in line:
        return [f'writes {name} to $GITHUB_ENV in its run: "{line}"' for name in dict.fromkeys(SETTING.findall(line))]
    found = [f'sets {match.group(1)} in its run: "{line}"' for match in ASSIGNMENT.finditer(line)
             if not offline(match.group(1), unquote(match.group(2)))]
    removed = [word for match in UNSET.finditer(line) for word in match.group(1).split() if word in SETTINGS]
    removed += [name for match in ENV_COMMAND.finditer(line) for name in ENV_UNSET.findall(match.group(1))]
    return found + [f'removes {name} in its run: "{line}"' for name in dict.fromkeys(removed)]


def step_overrides(step) -> list:
    """How a step sends GOPROXY or GOFLAGS online: through its env:, or a line of its run:."""
    found = []
    env = env_of(value(step, "env"))
    for name in SETTINGS:
        if name not in env or offline(name, env[name]):
            continue
        found.append(f'sets {name} to "{env[name]}" in its env:' if env[name] else f"removes {name} in its env:")
    run = text(value(step, "run"))
    for line in script_lines(run or ""):
        found.extend(run_overrides(line))
    return list(dict.fromkeys(found))


def has_reason(lines: list, step, floor: int) -> bool:
    """Whether a go-offline reason sits in the step's own block, or in the REASON_WINDOW lines
    above its first line and at or below `floor`, the first line after the step before it."""
    start = step.start_mark.line
    window = range(max(start - REASON_WINDOW, floor), content_end(step, lines))
    return any(index < len(lines) and REASON.search(lines[index]) for index in window)


# ---------------------------------------------------------------- the rule
class Reader:
    def __init__(self, root: Path):
        self.root = root
        self.problems: list = []
        self.read: list = []
        self.go_jobs = 0
        self.reasoned: list = []
        self.actions: dict = {}

    def load(self, rel: str):
        """(the document node, its lines), or (None, lines) with a problem when it is no YAML."""
        source = (self.root / rel).read_text(encoding="utf-8", errors="replace")
        lines = source.splitlines()
        try:
            return yaml.compose(source, Loader=yaml.SafeLoader), lines
        except yaml.YAMLError as error:
            mark = getattr(error, "problem_mark", None) or getattr(error, "context_mark", None)
            problem = getattr(error, "problem", None) or str(error).splitlines()[0]
            self.problems.append((rel, mark.line + 1 if mark else 1,
                                  f"{rel} cannot be read as YAML ({problem}), so this rule cannot read its jobs"))
            return None, lines

    def action_sets_up_go(self, directory: str, where: tuple, said: str) -> bool:
        """Whether the local action in `directory` has a step that uses actions/setup-go, itself
        or through a local action it uses. `where` and `said` place a problem about it."""
        directory = posixpath.normpath(directory)
        if directory in self.actions:
            return self.actions[directory]
        self.actions[directory] = False  # an action that uses itself, round a cycle, adds no Go
        rel = next((f"{directory}/{name}" for name in ("action.yml", "action.yaml")
                    if (self.root / directory / name).is_file()), None)
        if rel is None:
            self.problems.append((*where, f"{said} uses ./{directory}, which has no action.yml or action.yaml in this "
                                          "tree, so this rule cannot read whether it sets up Go"))
            return False
        document, _ = self.load(rel)
        steps = value(value(document, "runs"), "steps")
        found = False
        for step in steps.value if isinstance(steps, yaml.SequenceNode) else []:
            uses = (text(value(step, "uses")) or "").strip()
            if SETUP_GO.match(uses) or (uses.startswith("./") and self.action_sets_up_go(
                    uses[2:], (rel, step.start_mark.line + 1), f'{rel}, step "{step_name(step)}",')):
                found = True
        self.actions[directory] = found
        return found

    def go_step(self, rel: str, job_id: str, step, checkouts: list) -> bool:
        uses = (text(value(step, "uses")) or "").strip()
        if SETUP_GO.match(uses):
            return True
        if uses.startswith("./"):
            directory = posixpath.normpath(uses[2:])
            for checkout in checkouts:
                if directory.startswith(checkout + "/"):
                    directory = directory[len(checkout) + 1:]
                    break
            return self.action_sets_up_go(directory, (rel, step.start_mark.line + 1),
                                          f'job {job_id} of {rel}, step "{step_name(step)}",')
        run = text(value(step, "run"))
        return run is not None and runs_go(run)

    def read_job(self, rel: str, lines: list, workflow_env: dict, job_key, job, steps_key, steps) -> None:
        job_id = job_key.value
        checkouts = checkout_paths(steps)
        go_steps = [step for step in steps if self.go_step(rel, job_id, step, checkouts)]
        if not go_steps:
            return
        self.go_jobs += 1
        env = {**workflow_env, **env_of(value(job, "env"))}
        lacks = [name for name in SETTINGS if not offline(name, env.get(name) or "")]
        if lacks:
            said = " and ".join(f"{OFFLINE[name][0]} (it is {shown(env, name)})" for name in lacks)
            remedy = " and ".join(OFFLINE[name][1] for name in lacks)
            self.problems.append((rel, job_key.start_mark.line + 1,
                                  f'job {job_id} of {rel} runs go (first at step "{step_name(go_steps[0])}") and its env '
                                  f"lacks {said}: add {remedy} to the env: of {rel} or of job {job_id}, so a module the "
                                  "restored cache lacks fails the step at once, naming it, and nothing downloads it"))
        floor = steps_key.start_mark.line + 1
        for step in steps:
            found = step_overrides(step)
            if found:
                name = step_name(step)
                if has_reason(lines, step, floor):
                    self.reasoned.append(f'{rel} {job_id} "{name}"')
                else:
                    self.problems.append((rel, step.start_mark.line + 1,
                                          f'job {job_id} of {rel}, step "{name}", {" and ".join(found)} with no go-offline '
                                          'reason: a step that must go online says why in a "# go-offline: <why>" comment '
                                          f"line inside its own block or in the {REASON_WINDOW} lines above it; any other "
                                          "step reads the restored module cache alone"))
            floor = content_end(step, lines)

    def run(self) -> str:
        if not (self.root / CI).is_file():
            return f"UNKNOWN: no {CI} here, so no workflow is known to be required"
        queue, seen = deque([CI]), {CI}
        while queue:
            rel = queue.popleft()
            document, lines = self.load(rel)
            if document is None:
                continue
            self.read.append(rel)
            workflow_env = env_of(value(document, "env"))
            jobs = value(document, "jobs")
            for job_key, job in jobs.value if isinstance(jobs, yaml.MappingNode) else []:
                uses_key, uses = get(job, "uses")
                called = (text(uses) or "").strip()
                if called.startswith("./"):
                    called = posixpath.normpath(called[2:])
                    if called not in seen:
                        seen.add(called)
                        if (self.root / called).is_file():
                            queue.append(called)
                        else:
                            self.problems.append((rel, uses_key.start_mark.line + 1,
                                                  f"job {job_key.value} of {rel} calls {called}, which is not in this "
                                                  "tree, so this rule cannot read its jobs"))
                steps_key, steps = get(job, "steps")
                if isinstance(steps, yaml.SequenceNode):
                    self.read_job(rel, lines, workflow_env, job_key, job, steps_key, steps.value)
        read = f"{self.go_jobs} Go jobs of {len(self.read)} required workflows read"
        if self.problems:
            return f"{len(self.problems)} problems over {read}"
        if not self.go_jobs:
            return (f"UNKNOWN: read {len(self.read)} required workflows ({', '.join(self.read)}) and found no Go job in "
                    "them: no step of them sets up Go, uses a local action that does, or runs go or go-fetch.sh")
        return (f"{read}, each with GOPROXY=off and GOFLAGS=-mod=readonly; {len(self.reasoned)} online steps with a "
                f"reason: {'; '.join(self.reasoned) or '(none)'}")


def check(root) -> tuple:
    """(problems, summary) of the tree at `root`: the module docstring says what each holds."""
    reader = Reader(Path(root))
    summary = reader.run()
    return reader.problems, summary


def escape(message: str) -> str:
    return message.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")


def main(argv: list) -> int:
    if len(argv) > 2:
        print("usage: go_offline_jobs.py [ROOT]", file=sys.stderr)
        return 2
    root = Path(argv[1]) if len(argv) == 2 else Path(os.path.abspath(__file__)).parents[2]
    problems, summary = check(root)
    for path, line, message in problems:
        print(f"::error file={path},line={line}::{escape(message)}")
    print(summary)
    if problems:
        return 1
    return 2 if summary.startswith("UNKNOWN: ") else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
