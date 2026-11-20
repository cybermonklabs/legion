#!/usr/bin/env python3
"""Every go command an image build of a required job runs reads the job's module cache alone.

A go command inside a Docker build reaches the module proxy as a test does, and fails whenever
the proxy does, on a change that touched nothing it builds: in run 44589958875 (attempt 1),
envoy-smoke's build of packages/envoy/infra/host/heldportwrapper/Dockerfile printed `go:
downloading golang.org/x/sys` and failed. The rule that ends that is the job's own: the job fills
its module cache in its fetch step, before the build, and hands the build that cache as a named
build context; each go command of the build reads it with the module proxy off.

What it reads.
- The required workflows: .github/workflows/ci.yaml and every local workflow a job of one of them
  calls, followed transitively, as go_offline_jobs.py reads them. A called workflow that is not in
  the tree, and a file that is no YAML, are refused.
- Each image build of a job of them: a step that uses docker/build-push-action, its `file:`
  (`<context>/Dockerfile` when it gives none), `target:` and `build-contexts:` (NAME=PATH a line,
  or comma-separated); and a `docker build`, `docker buildx build` (or `b`), `docker image build`
  or `docker builder build` command in a step's run:, with its -f/--file, --target and
  --build-context. A Dockerfile a build names that is not in the tree is refused.
- The stages that build builds: its target (the last stage when it names none) and every stage
  that one reaches through FROM <stage>, COPY or ADD --from=<stage> and RUN --mount from=<stage>,
  by name or by index. A stage whose name the build hands as a named context is not built (the
  context replaces it, as BuildKit does), and nothing is followed from it. A stage no build of a
  required job builds is not read, so a Dockerfile only another workflow builds (a release) keeps
  whatever its builds need.
- Each RUN of a built stage, its shell form read a simple command at a time as
  check-go-offline.sh reads a shell file (go_offline_sites.shell_sites: the go binary followed by
  a subcommand that can load a module, in a $(...) and a `bash -c` too), and its exec form as an
  argv. A command whose program is a shell script of this repository (by its name: an image COPYs
  it to a directory on PATH) that starts a go command of its own is one too
  (scripts/go-third-party-notices.sh among them). Comment lines inside a RUN are dropped, as
  Docker drops them.

What a go command of such a RUN must have, each read from the stage's ENV lines (and those of
the stage it is FROM), then the RUN's own: `export`s, NAME=value prefixes and `env` settings, the
last one deciding:
- GOPROXY=off, or a file:// proxy at a directory a mount below puts the job's cache at;
- GOFLAGS holding the word -mod=readonly;
- GOTOOLCHAIN=local, so a go line of a go.mod never fetches a toolchain;
- a `--mount=type=bind,from=<NAME>,target=<dir>` on the RUN, where NAME is a named context the
  build hands and <dir> is the stage's module cache (GOMODCACHE, else the first GOPATH entry's
  pkg/mod, else /go/pkg/mod, the golang images' GOPATH) or its cache/download directory.
A go command that lacks any of these is refused, naming the Dockerfile, its line, the stage, and
the workflow, job and step that build it, unless a comment line holding `go-offline: <why>` sits
inside the RUN or in the REASON_WINDOW lines above its first line, saying why it reads no
module or why its build fails no change it does not touch; the summary names each such RUN.

What it does not see: a build inside a script a step runs (check-image-trigger-paths.sh refuses
every build form but docker/build-push-action and a pinned run: build in a workflow, so none is
there today); a compose file's build; an ARG or a build-arg that sets one of the four (an ARG of
one of them reads as unknown, so its go commands are refused); a RUN's heredoc, read as its first
line; the base image's own ENV, which no file here holds.

Interface. check(root) -> (problems, summary), as go_offline_jobs.py's: problems is a list of
(path, line, message), the line 1-based; summary is one line naming what was read, `P problems
over ...` when there are problems, and a line starting `UNKNOWN: ` when it read no required
workflow. The CLI, `python3 .github/scripts/go_offline_images.py [ROOT]`, prints each problem as
a GitHub error annotation and then the summary, and exits 1 on a problem, 2 when the summary is
UNKNOWN and 0 otherwise. check-go-offline.sh calls check(); its test is check-go-offline.test.sh,
with check-go-offline-cases-images.sh.
"""

from __future__ import annotations

import json
import os
import posixpath
import re
import shlex
import sys
from collections import deque
from pathlib import Path

import yaml

from go_offline_jobs import get, step_name, text, value
from go_offline_sites import SUBCOMMANDS, argv_program, goish_text, shell_sites
from source_scan import CLEAR, SKIP_DIRS
from source_shell import command_word, shell_script

CI = ".github/workflows/ci.yaml"
BUILD_ACTION = re.compile(r"^docker/build-push-action(?:@|$)", re.IGNORECASE)
SETTINGS = ("GOPROXY", "GOFLAGS", "GOTOOLCHAIN")
# How many lines above a RUN's first line a go-offline reason may sit, as check-go-offline.sh's.
REASON_WINDOW = 5
REASON = re.compile(r"^\s*#.*\bgo-offline:[ \t]*\S")
GO_PATH = "/go"  # GOPATH of the golang images every Go stage here builds on
# `docker <these> build`: the forms of a docker command that build an image.
DOCKER_BUILDS = (["build"], ["buildx", "build"], ["buildx", "b"], ["image", "build"], ["builder", "build"])
# The options of those that take the next word as their value.
DOCKER_VALUED = {"-f", "--file", "-t", "--tag", "--target", "--build-arg", "--build-context", "--platform",
                 "--label", "--cache-from", "--cache-to", "-o", "--output", "--secret", "--ssh", "--progress",
                 "--network", "--iidfile", "--metadata-file", "--builder", "--attest", "--annotation",
                 "--shm-size", "--ulimit", "--add-host", "--allow", "--call", "--sbom", "--provenance"}


# ---------------------------------------------------------------- a Dockerfile
class Instruction:
    def __init__(self, line: int, lines: list, comments: list):
        self.line = line  # 1-based line of its first line
        self.lines = lines  # (1-based line, text) of each physical line, comment lines aside
        self.comments = comments  # the comment lines inside it
        joined = " ".join(t.rstrip()[:-1] if t.rstrip().endswith("\\") else t for _, t in lines)
        self.keyword, _, self.rest = joined.strip().partition(" ")
        self.keyword = self.keyword.upper()
        self.rest = self.rest.strip()


def instructions(source: str) -> list:
    """Each instruction of a Dockerfile, its continued lines joined: a line that starts with `#` is
    a comment, inside an instruction too, and a line ending in a backslash continues."""
    found, current, comments = [], None, []
    for number, raw in enumerate(source.splitlines(), 1):
        stripped = raw.strip()
        if current is None:
            if not stripped or stripped.startswith("#"):
                continue
            current, comments = [(number, raw)], []
        elif stripped.startswith("#"):
            comments.append(raw)
            continue
        elif not stripped:
            continue
        else:
            current.append((number, raw))
        if not raw.rstrip().endswith("\\"):
            found.append(Instruction(current[0][0], current, comments))
            current = None
    if current:
        found.append(Instruction(current[0][0], current, comments))
    return found


class Stage:
    def __init__(self, index: int, name: str, base: str, line: int):
        self.index, self.name, self.base, self.line = index, name, base, line
        self.items: list = []  # its instructions after FROM, in order
        self.uses: set = set()  # the stage names and indexes it reads (COPY --from, RUN --mount from)

    def label(self) -> str:
        return self.name or f"#{self.index}"


def flags_of(rest: str) -> tuple:
    """(the --flag=value words at the start of an instruction's text, the text after them)."""
    flags = []
    while True:
        found = re.match(r"--([A-Za-z-]+)(?:=(\S*))?\s*", rest)
        if not found:
            return flags, rest
        flags.append((found.group(1), found.group(2) or ""))
        rest = rest[found.end():]


def mount_of(spec: str) -> dict:
    parts = {}
    for item in spec.split(","):
        key, eq, val = item.partition("=")
        parts[key.strip()] = val.strip() if eq else "true"
    return parts


def stages_of(source: str) -> list:
    stages = []
    for item in instructions(source):
        if item.keyword == "FROM":
            _, rest = flags_of(item.rest)
            words = rest.split()
            name = words[2].lower() if len(words) >= 3 and words[1].upper() == "AS" else ""
            stages.append(Stage(len(stages), name, words[0].lower() if words else "", item.line))
            continue
        if not stages:
            continue  # an ARG before the first FROM
        stage = stages[-1]
        stage.items.append(item)
        flags, _ = flags_of(item.rest)
        for flag, given in flags:
            if flag == "from" and item.keyword in ("COPY", "ADD"):
                stage.uses.add(given.lower())
            elif flag == "mount" and item.keyword == "RUN" and mount_of(given).get("from"):
                stage.uses.add(mount_of(given)["from"].lower())
    return stages


def find_stage(stages: list, ref: str):
    for stage in stages:
        if ref == stage.name or ref == str(stage.index):
            return stage
    return None


def built_stages(stages: list, target: str, contexts: set) -> list:
    """The stages a build of `target` (the last stage when empty) builds, `contexts` replacing the
    stages they name: a stage a named context replaces is not built, whether the target or a
    stage reaches it, and nothing is followed from it."""
    start = find_stage(stages, target.lower()) if target else (stages[-1] if stages else None)
    built, todo = [], deque([start] if start else [])
    while todo:
        stage = todo.popleft()
        if stage in built or stage.name in contexts:
            continue
        built.append(stage)
        for ref in [stage.base, *sorted(stage.uses)]:
            found = find_stage(stages[:stage.index], ref) if ref == stage.base else find_stage(stages, ref)
            if found is not None:
                todo.append(found)
    return sorted(built, key=lambda s: s.index)


def env_words(rest: str) -> list:
    """(name, value) of an ENV line: NAME=value words, or the legacy `ENV NAME value`. A value
    that expands a variable is None: this reads no ARG and no base image's env."""
    try:
        words = shlex.split(rest, posix=True)
    except ValueError:
        return []
    if words and "=" not in words[0]:
        pairs = [(words[0], " ".join(words[1:]))]
    else:
        pairs = [w.partition("=")[::2] for w in words if "=" in w]
    return [(name, None if "$" in val else val) for name, val in pairs]


def stage_env(stages: list, stage: Stage, upto=None, seen=()) -> list:
    """The environment elements ENV and ARG lines give a RUN of `stage` before instruction
    `upto`: the stage it is FROM's first, then its own."""
    elements = []
    base = find_stage(stages[:stage.index], stage.base)
    if base is not None and base not in seen:
        elements += stage_env(stages, base, None, (*seen, stage))
    for item in stage.items:
        if item is upto:
            break
        if item.keyword == "ENV":
            elements += [("set", name, val) for name, val in env_words(item.rest)]
        elif item.keyword == "ARG":
            elements += [("set", word.partition("=")[0], None) for word in item.rest.split()
                         if word.partition("=")[0] in (*SETTINGS, "GOMODCACHE", "GOPATH")]
    return elements


def last(elements: list, name: str):
    """The value the last element that can set `name` gives it; None for unset or unknown."""
    for element in reversed(elements):
        if element == CLEAR:
            return None
        if element[0] == "set" and element[1] == name:
            return element[2]
        if element[0] != "set":
            return None
    return None


def module_cache(elements: list) -> str:
    cache = last(elements, "GOMODCACHE")
    if cache:
        return cache.rstrip("/")
    gopath = last(elements, "GOPATH")
    return (gopath.split(":")[0].rstrip("/") if gopath else GO_PATH) + "/pkg/mod"


# ---------------------------------------------------------------- repository scripts that start go
class Scripts:
    """Which program names are shell scripts of this repository that start a go command."""

    def __init__(self, root: Path):
        self.root, self.paths, self.answers = root, None, {}

    def starts_go(self, name: str) -> bool:
        base = name.rsplit("/", 1)[-1]
        if not base.endswith((".sh", ".bash")):
            return False
        if base not in self.answers:
            if self.paths is None:
                self.paths = {}
                for directory, dirs, files in os.walk(self.root):
                    dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
                    for file in files:
                        if file.endswith((".sh", ".bash")):
                            self.paths.setdefault(file, []).append(Path(directory) / file)
            self.answers[base] = any(shell_sites(path.read_text(errors="replace"))
                                     for path in self.paths.get(base, []))
        return self.answers[base]


# ---------------------------------------------------------------- the rule
class Reader:
    def __init__(self, root: Path):
        self.root = root
        self.problems: list = []
        self.read: list = []
        self.steps = 0
        self.builds = 0
        self.dockerfiles: set = set()
        self.go_commands = 0
        self.mounted = 0
        self.reasoned: list = []
        self.scripts = Scripts(root)

    def load(self, rel: str):
        source = (self.root / rel).read_text(encoding="utf-8", errors="replace")
        try:
            return yaml.compose(source, Loader=yaml.SafeLoader)
        except yaml.YAMLError as error:
            mark = getattr(error, "problem_mark", None)
            self.problems.append((rel, mark.line + 1 if mark else 1,
                                  f"{rel} cannot be read as YAML, so this rule cannot read its image builds"))
            return None

    def builds_of(self, step) -> list:
        """(Dockerfile, target, named contexts) of each image build a step makes."""
        uses = (text(value(step, "uses")) or "").strip()
        given = value(step, "with")
        if BUILD_ACTION.match(uses):
            context = (text(value(given, "context")) or ".").strip()
            dockerfile = (text(value(given, "file")) or posixpath.join(context, "Dockerfile")).strip()
            listed = re.split(r"[\n,]", text(value(given, "build-contexts")) or "")
            names = {entry.partition("=")[0].strip().lower() for entry in listed if "=" in entry}
            return [(dockerfile, (text(value(given, "target")) or "").strip(), names)]
        run = text(value(step, "run"))
        if run is None:
            return []
        workdir = (text(value(step, "working-directory")) or ".").strip()
        found = []
        for command in shell_script(run):
            values = command.values()
            k = command_word(values)
            if k is None or (values[k] or "").rsplit("/", 1)[-1] != "docker":
                continue
            words = [w for w in values[k + 1:] if w is not None]
            while words and words[0].startswith("-"):
                words = words[1:]  # docker's global options: none here takes a separate value
            form = next((f for f in DOCKER_BUILDS if words[:len(f)] == f), None)
            if form is None:
                continue
            dockerfile = target = None
            names, positional, rest = set(), [], words[len(form):]
            i = 0
            while i < len(rest):
                word = rest[i]
                option, eq, inline = word.partition("=")
                takes = option in DOCKER_VALUED and not eq
                given_value = rest[i + 1] if takes and i + 1 < len(rest) else inline
                if option in ("-f", "--file"):
                    dockerfile = given_value
                elif option == "--target":
                    target = given_value
                elif option == "--build-context":
                    names.add(given_value.partition("=")[0].lower())
                elif not word.startswith("-"):
                    positional.append(word)
                i += 2 if takes else 1
            context = positional[-1] if positional else "."
            dockerfile = posixpath.normpath(posixpath.join(workdir, dockerfile or posixpath.join(context, "Dockerfile")))
            found.append((dockerfile, target or "", names))
        return found

    def read_build(self, where: str, rel: str, step, dockerfile: str, target: str, contexts: set) -> None:
        self.builds += 1
        path = self.root / dockerfile
        if not path.is_file():
            self.problems.append((rel, step.start_mark.line + 1, f"{where} builds {dockerfile}, which is not in this "
                                                               "tree, so this rule cannot read its go commands"))
            return
        self.dockerfiles.add(dockerfile)
        source = path.read_text(encoding="utf-8", errors="replace")
        raw = source.splitlines()
        stages = stages_of(source)
        if target and find_stage(stages, target.lower()) is None:
            self.problems.append((rel, step.start_mark.line + 1, f"{where} builds target {target} of {dockerfile}, "
                                                               "which names no such stage"))
            return
        for stage in built_stages(stages, target, contexts):
            for item in stage.items:
                if item.keyword == "RUN":
                    self.read_run(where, dockerfile, raw, stages, stage, item, contexts)

    def read_run(self, where, dockerfile, raw, stages, stage, item, contexts) -> None:
        flags, command = flags_of(item.rest)
        base = stage_env(stages, stage, item)
        cache = module_cache(base)
        mounts = [mount_of(given) for flag, given in flags if flag == "mount"]
        targets = [m.get("target") or m.get("dst") or m.get("destination") or "" for m in mounts
                   if m.get("type", "bind") == "bind" and m.get("from", "").lower() in contexts]
        cache_targets = [t.rstrip("/") for t in targets if t.rstrip("/") in (cache, cache + "/cache/download")]

        def judge(elements):
            lacks = []
            proxy = last(elements, "GOPROXY")
            if not (proxy == "off" or any(proxy and re.fullmatch(r"file://" + re.escape(t) + r"(?:/.*)?", proxy)
                                          for t in cache_targets)):
                lacks.append(f"GOPROXY=off (it is {proxy or 'unset or unknown'})")
            flags_value = last(elements, "GOFLAGS")
            if "-mod=readonly" not in (flags_value or "").split():
                lacks.append(f"GOFLAGS=-mod=readonly (it is {flags_value or 'unset or unknown'})")
            if last(elements, "GOTOOLCHAIN") != "local":
                lacks.append(f"GOTOOLCHAIN=local (it is {last(elements, 'GOTOOLCHAIN') or 'unset or unknown'})")
            if not cache_targets:
                lacks.append(f"a --mount=type=bind,from=<a named context the build hands>,target={cache} (or its "
                             f"cache/download)")
            return lacks

        sites = []
        if command.startswith("["):
            try:
                argv = json.loads(command)
            except ValueError:
                argv = None
            if isinstance(argv, list) and all(isinstance(a, str) for a in argv):
                k, settings = argv_program(argv)
                if k < len(argv) and goish_text(argv[k]) and argv[k + 1:k + 2] and argv[k + 1] in SUBCOMMANDS:
                    sites.append((item.line, judge([*base, *settings])))
        else:
            # The RUN's text as its physical lines, the keyword and flags blanked, so a site's line
            # is the Dockerfile's.
            numbers = [number for number, _ in item.lines]
            body = [t for _, t in item.lines]
            keep = len(body[0]) - len(body[0].lstrip())
            first = body[0].lstrip()[3:]  # past RUN
            consumed = len(first) - len(first.lstrip())
            remaining = first.lstrip()
            while True:
                found = re.match(r"--[A-Za-z-]+(?:=\S*)?\s*", remaining)
                if not found:
                    break
                consumed += found.end()
                remaining = remaining[found.end():]
            body[0] = " " * (keep + 3 + consumed) + remaining
            joined = "\n".join(body)
            programs = {c.values()[k] for c in shell_script(joined)
                        for k in [command_word(c.values())] if k is not None and c.values()[k]}
            scripts = frozenset(p for p in programs if self.scripts.starts_go(p))
            sites = [(numbers[min(line, len(numbers) - 1)], lacks)
                     for line, lacks in shell_sites(joined, 0, base, 0, SETTINGS + ("GOMODCACHE",), judge, scripts)]
        if not sites:
            return
        self.go_commands += len(sites)
        window = raw[max(0, item.line - 1 - REASON_WINDOW):item.line - 1] + item.comments
        if any(REASON.search(line) for line in window):
            self.reasoned.append(f"{dockerfile}:{item.line}")
            return
        by_line: dict = {}
        for line, lacks in sites:
            if not lacks:
                self.mounted += 1
                continue
            merged = by_line.setdefault(line, [])
            merged += [lack for lack in lacks if lack not in merged]
        for line, lacks in sorted(by_line.items()):
            self.problems.append((dockerfile, line, f"a go command of stage {stage.label()} of {dockerfile}, which "
                                  f"{where} builds, runs without {'; '.join(lacks)}: hand the build the job's module "
                                  "cache as a named context (build-contexts: gomodcache=<its GOMODCACHE>/cache/download, "
                                  "filled by the job's fetch step before), mount it there and run go offline, so a "
                                  "module the cache lacks fails the build at once, naming it, and nothing downloads "
                                  "it while the image builds; or say why it need not in a `# go-offline: <why>` line "
                                  f"within the {REASON_WINDOW} lines above the RUN: "
                                  + (raw[line - 1].strip() if 0 < line <= len(raw) else "")))

    def run(self) -> str:
        if not (self.root / CI).is_file():
            return f"UNKNOWN: no {CI} here, so no workflow is known to be required"
        queue, seen = deque([CI]), {CI}
        while queue:
            rel = queue.popleft()
            document = self.load(rel)
            if document is None:
                continue
            self.read.append(rel)
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
                                                  "tree, so this rule cannot read its image builds"))
                steps = value(job, "steps")
                for step in steps.value if isinstance(steps, yaml.SequenceNode) else []:
                    self.steps += 1
                    for dockerfile, target, contexts in self.builds_of(step):
                        where = f'job {job_key.value} of {rel} (step "{step_name(step)}")'
                        self.read_build(where, rel, step, dockerfile, target, contexts)
        read = (f"{self.builds} image builds of {len(self.read)} required workflows read ({len(self.dockerfiles)} "
                f"Dockerfiles, {self.go_commands} go commands in the stages they build)")
        if self.problems:
            return f"{len(self.problems)} problems over {read}"
        if not self.steps:
            return (f"UNKNOWN: read {len(self.read)} required workflows and no step of them, so their image builds "
                    "are unseen")
        return (f"{read}: {self.mounted} read the job's module cache alone, and {len(self.reasoned)} RUNs carry a "
                f"reason: {', '.join(self.reasoned) or '(none)'}")


def check(root) -> tuple:
    """(problems, summary) of the tree at `root`: the module docstring says what each holds."""
    reader = Reader(Path(root))
    summary = reader.run()
    return reader.problems, summary


def escape(message: str) -> str:
    return message.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")


def main(argv: list) -> int:
    if len(argv) > 2:
        print("usage: go_offline_images.py [ROOT]", file=sys.stderr)
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
