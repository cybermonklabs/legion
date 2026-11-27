#!/usr/bin/env python3
"""Which legs of each Go legs job a change starts, and which of its packages they run.

    go_legs_select.py --base <commit>     the selection for the change from <commit> to the working tree
    go_legs_select.py --paths <path>...   the selection for a change to exactly those paths, with no base
    go_legs_select.py --every             every leg of every job, each with every package

Run from the repository's root, which go-legs-select.sh does; ci.yaml's affected job runs it. It
prints one JSON object, a key per job of .github/go-legs.yaml:

    {"daemon-go": {"legs": [2], "packages": ["cmd/legion", "internal/api"]},
     "envoy-go": {"legs": [1, 2, 3, 4, 5], "packages": "all"}, ...}

`legs` are the legs the job's matrix starts and `packages` the packages of the module (as
go-legs.yaml names them) those legs run, or `all`. go_legs.py reads it in each leg
(GO_LEGS_SELECTED) and prints only the leg's selected packages; a leg that runs a group of one
package's tests runs that group when the package is selected. Which jobs run at all is the changes
job's to say: this says, inside a job that runs, which of its packages the change reaches.
Everything but the JSON goes to standard error: per job what was selected, and per package the
first changed path that selected it.

WHAT SELECTS A PACKAGE of a job. The job's packages (the file's, and, in the job a module's
`unlisted` names, each package `go list ./...` prints and no place names, which go_legs.py runs in
that job's last leg) are listed once, `go list -deps -test` under the build flags its legs step
passes go_legs.py (-race) and the architecture of the runner its legs run on (GOARCH), and each
package's test binary is cut from that listing: the package, its test variants and everything
they import. A changed path selects
  * the package whose directory holds it (the nearest directory above it that go list knows as a
    package of a module), when it is a _test.go file directly in that directory: a test file is
    compiled into its own package's test binary alone;
  * otherwise every package whose test binary compiles that directory: the package itself, and
    every package that imports it, directly or not, in its test or its non-test build. A change to
    a shared package or a shared contract so selects its consumers, and a file of a package's
    testdata selects every package that imports it. A deleted or renamed file selects by its
    directory too;
  * every package whose test binary embeds it (//go:embed);
  * every package whose test binary compiles a Go file whose //ci: lines (the block right after its
    package clause, in the grammar `.github/scripts/ci_filter_sources.py#words` reads) declare a
    read of it: `//ci:reads <glob> [!<glob>...] <why>` reads the files the glob matches;
    `//ci:builds <dir> <package>... [NAME=value...] <why>` builds those packages, so it reads every
    directory their build compiles; `//ci:reads-set <name> <why>` reads a set
    .github/scripts/ci_job_reads.py's READ_SETS computes.
A test that reads a file outside its package must say so in a //ci: line, or the second rule may
not reach it. .github/scripts/go_legs_reads.py, which the ci-filters job runs, holds to such a line
each read it finds in a legs test's source: a path built out of `..` literals, and a climb from the
working directory or the source file to a go.mod, go.work, .git or .github, with what is walked or
read from where it ends. A read built any other way it does not see.

The rest is read from the change's base, as content rather than as a touched path:
  * go.work, or a go.mod change to anything but its requirements: every package of every job;
  * a go.mod requirement or a go.sum or go.work.sum line: every package whose test binary, or a
    //ci:builds build it starts, compiles a package of a module outside the repository. A module's
    own imports are not in the listing (third-party packages load with none under GOPROXY=off), so
    which of those a moved module reaches is not known, and each of them is selected;
  * a file a legs job runs or reads before its tests do (LegsJob.inputs: the files its steps name,
    the local actions they use and every file those actions' steps name, what each such script
    runs or sources in turn, go_legs.py, this script and go-legs-select.sh): every package of that
    job. A tool pin is one: install-jj reads the jj the worker image's Dockerfile names and the
    rows of .github/ci-tools.yaml, install-omp .omp-pin, and the tests run what they install;
  * a legs workflow file, and ci.yaml: every package of the job when the legs' definition changed
    (LegsJob.definitions, parsed, so a comment selects none): the legs' matrix job, the file's
    `on:`, `env:` and `defaults:`, and ci.yaml's affected job, the job that calls the file and
    CI_MERGE_GROUP_SELECTS;
  * go-legs.yaml: only the packages whose leg or group moved; a groups file its own package.
Anything else selects no package unless a rule above names it: a changelog fragment, a dashboard
file, another workflow. With no base (--paths), each of these rules selects every package.
A leg that carries a step of its own (`if: matrix.leg == N`: daemon-go's Stage 1 proof and stamped
shape gate, the forced-order tests) starts whenever its job runs, so that step runs; its go test
then runs the leg's selected packages, or none.

WHEN THE ANSWER IS EVERY LEG. --every; any failure (a go command that fails, a first-party package
go list cannot load, a deleted one among them, a //ci: line that cannot be read, a job of
go-legs.yaml no workflow step runs the legs of, an exception), the JSON being --every's and
standard error saying why.
"""

from __future__ import annotations

import argparse
import functools
import json
import os
import re
import subprocess
import sys
import tempfile
import traceback
from dataclasses import dataclass, field
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import yaml  # noqa: E402

import go_legs  # noqa: E402
from ci_filter_coverage import (REPO, WORKFLOW, Session, rel, repo_files, run, step_workdir,  # noqa: E402
                                workflow_callers, workflow_documents)
from ci_filter_sources import file_words  # noqa: E402
from workflow_paths import glob_regex  # noqa: E402

SCRIPT = ".github/scripts/go_legs_select.py"
WRAPPER = ".github/scripts/go-legs-select.sh"
WORKFLOWS = ".github/workflows"
# ci.yaml's job that runs this selection, and its switch for merge groups (go-legs-select.sh).
AFFECTED_JOB = "affected"
SWITCH = "CI_MERGE_GROUP_SELECTS"
# The call of go_legs.py in a legs step: the module, the job, and the build flags after the leg and
# the leg count, which its go test passes too.
LEGS_CALL = re.compile(r"go_legs\.py\s+(\S+)\s+(\S+)\s+\$\{\{[^}]*\}\}\s+\$\{\{[^}]*\}\}((?:\s+-[^\s|]+)*)\s*\|")
# A step's if: that runs it in one leg of its matrix alone.
ONE_LEG = re.compile(r"^\s*(?:\$\{\{)?\s*matrix\.leg\s*==\s*(\d+)\s*(?:\}\})?\s*$")
# The -skip (or a wrapper's --skip) the go test a legs step pipes into is given: the tests the legs
# leave to another job.
SKIP = re.compile(r"\|\s*xargs\b.*?\s--?skip[= ](?:'([^']*)'|(\S+))")
# A word of a step's text or a script's that may name a repository file: one only when it is the path
# of a file of the repository read from a directory it may be read from (named_files), so a bare
# name such as `.omp-pin` counts as `.github/ci-tools.yaml` does.
PATH_TOKEN = re.compile(r"[A-Za-z0-9_.@+/-]+")
# A local action's own directory, as its steps' text names it.
ACTION_PATH = re.compile(r"\$\{?GITHUB_ACTION_PATH\}?|\$\{\{\s*github\.action_path\s*\}\}")
# A runner label of an arm64 machine; any other runs x86_64.
ARM_RUNNER = re.compile(r"(?:^|[-_])(?:arm|arm64|aarch64)(?:$|[-_])")
# The fields of a package the selection reads off `go list -deps -test`.
LISTED = ("Dir,ImportPath,ForTest,Standard,Match,Imports,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles,"
          "EmbedFiles,TestEmbedFiles,XTestEmbedFiles,Error")
MODULE_LINE = re.compile(r"^\s*module\s+(\S+)", re.M)


def numbers(legs: dict) -> list:
    return sorted(int(number) for number in legs)


def legs_document(text: str | None) -> dict:
    """Each module of a go-legs.yaml text to its entry."""
    document = yaml.safe_load(text) if text else None
    return {module: entry for module, entry in (document or {}).items() if isinstance(entry, dict)}


def job_legs(entry: dict) -> dict:
    """Each job of a module's entry to its legs {leg number: entries}."""
    return {job: legs for job, legs in entry.items() if job not in go_legs.NOT_JOBS}


def whole(legs: dict) -> dict:
    """A job's whole run: every leg of it, each with every package."""
    return {"legs": numbers(legs), "packages": go_legs.ALL}


def every(document: dict) -> dict:
    return {job: whole(legs) for entry in document.values() for job, legs in job_legs(entry).items()}


def matches(glob: str, but: tuple, path: str) -> bool:
    """Whether a read of `glob`, less its `but` globs, names `path`, as dorny/paths-filter reads
    a glob (workflow_paths.glob_regex)."""
    return bool(glob_regex(glob).fullmatch(path)) and not any(glob_regex(b).fullmatch(path) for b in but)


def nearest(path: str, directories: set) -> str | None:
    parent = os.path.dirname(path)
    while parent:
        if parent in directories:
            return parent
        parent = os.path.dirname(parent)
    return None


def is_test_file_of(path: str, directory: str) -> bool:
    """Whether `path` is a _test.go file directly in `directory`, which go compiles into that
    package's own test binary and no other."""
    return path.endswith("_test.go") and os.path.dirname(path) == directory


def head_text(path: str) -> str | None:
    """A file of the tree the change leads to, the working tree."""
    try:
        return (REPO / path).read_text()
    except (OSError, UnicodeError):
        return None


@dataclass
class Base:
    """The tree a change is measured from, read file by file (`git show`); with no commit, nothing
    of it can be read, and each rule that needs it selects every package instead."""

    commit: str | None
    shown: dict = field(default_factory=dict)

    def text(self, path: str) -> str | None:
        if not self.commit:
            return None
        if path not in self.shown:
            out = subprocess.run(["git", "show", f"{self.commit}:{path}"], cwd=REPO, capture_output=True, text=True)
            self.shown[path] = out.stdout if out.returncode == 0 else None
        return self.shown[path]


# ---------------------------------------------------------------- where each job's legs run
@dataclass
class LegsJob:
    """Where a job of go-legs.yaml runs: the workflow that holds its legs step, the build flags that
    step passes, the tests its go test skips, the architecture of the runner its legs run on, the
    legs that carry a step of their own, the files the legs run or read, and the parts of the
    workflows whose definition decides how they run."""

    workflow: str
    flags: tuple
    skip: str  # a -skip pattern, or ""
    goarch: str
    own_steps: set = field(default_factory=set)  # the legs a step runs in alone
    inputs: set = field(default_factory=set)  # repository paths the legs run or read whole
    input_trees: set = field(default_factory=set)  # repository directories, each with all below it
    # (workflow file, the keys down to the part of it that defines the legs).
    definitions: list = field(default_factory=list)

    @property
    def race(self) -> bool:
        return "-race" in self.flags

    @property
    def tags(self) -> str:
        return next((flag.split("=", 1)[1] for flag in self.flags if flag.startswith("-tags=")), "")


def named_files(text: str, directories: list, files: set) -> set:
    """The repository files `text` names, each word read from each of `directories`."""
    found = set()
    for token in PATH_TOKEN.findall(text):
        token = token[2:] if token.startswith("./") else token
        for directory in directories:
            path = os.path.normpath(os.path.join(directory, token))
            if path in files:
                found.add(path)
    return found


def read_inputs(start: set, files: set) -> set:
    """`start` and every file of the repository a shell script among them names outside a comment,
    and every file those name, until no more: the scripts a step runs and what they run or source.
    A Go or Markdown file is never one of them: a test's package selects for its own files."""
    seen, queue = set(start), list(start)
    while queue:
        path = queue.pop()
        if os.path.splitext(path)[1] not in (".sh", ".bash", ""):
            continue
        text = head_text(path)
        if text is None:
            continue
        code = "\n".join(re.sub(r"(^|\s)#.*$", "", line) for line in text.splitlines())
        directory = os.path.dirname(path)
        for found in named_files(code, [directory, os.path.dirname(directory), "."], files):
            if found not in seen and not found.endswith((".go", ".md")):
                seen.add(found)
                queue.append(found)
    return seen


def action_files(action: str, files: set, trees: set) -> set:
    """The files the steps of the local action in directory `action` name in their run: and env:
    text, each word read from the action's own directory (what $GITHUB_ACTION_PATH names) and from
    the repository's root, where a composite action's steps run; and those of each local action it
    uses in turn. Each action's directory joins `trees`. A Go or Markdown file is never one of
    them: the action that reads a Go constant reads a package, which selects for its own files."""
    if action in trees:
        return set()
    trees.add(action)
    text = head_text(f"{action}/action.yml") or head_text(f"{action}/action.yaml")
    found: set = set()
    for step in (((yaml.safe_load(text) if text else None) or {}).get("runs") or {}).get("steps") or []:
        uses = str(step.get("uses", ""))
        if uses.startswith("./"):
            found |= action_files(os.path.normpath(uses), files, trees)
        said = " ".join(str(v) for v in [step.get("run", "")] + list((step.get("env") or {}).values()))
        found |= named_files(ACTION_PATH.sub(action, said), [action, step.get("working-directory") or "."], files)
    return {path for path in found if not path.endswith((".go", ".md"))}


def legs_jobs(document: dict, files: set) -> dict:
    """Each job of go-legs.yaml to the LegsJob that runs its legs: the one job of ci.yaml or of a
    workflow it calls with a step that pipes `go_legs.py <module> <job>`."""
    problems: list = []
    workflows = workflow_documents(problems)
    if problems:
        raise RuntimeError("; ".join(p.message for p in problems))
    callers = workflow_callers(workflows[WORKFLOW])
    found: dict = {}
    for path, workflow in workflows.items():
        for job_id, body in ((workflow or {}).get("jobs") or {}).items():
            steps = (body or {}).get("steps") or []
            for step in steps:
                call = LEGS_CALL.search(str(step.get("run", "")))
                if not call or call.group(2) not in job_legs(document.get(call.group(1)) or {}):
                    continue
                key = call.group(2)
                if key in found:
                    raise LookupError(f"two steps run the legs of {key}: in {found[key].workflow} and {path}")
                runner = str(body.get("runs-on", ""))
                skip = SKIP.search(str(step["run"]))
                legs = LegsJob(path, tuple(call.group(3).split()), (skip.group(1) or skip.group(2)) if skip else "",
                               "arm64" if ARM_RUNNER.search(runner) else "amd64")
                named: set = set()
                for other in steps:
                    one = ONE_LEG.match(str(other.get("if", "")))
                    if one:
                        legs.own_steps.add(int(one.group(1)))
                    uses = str(other.get("uses", ""))
                    if uses.startswith("./"):
                        named |= action_files(os.path.normpath(uses), files, legs.input_trees)
                    text = " ".join(str(v) for v in [other.get("run", "")] + list((other.get("env") or {}).values()))
                    named |= named_files(text, [step_workdir(workflow, body, other), "."], files)
                for values in ((body.get("env") or {}).values(), ((workflow or {}).get("env") or {}).values()):
                    named |= named_files(" ".join(str(v) for v in values), ["."], files)
                legs.inputs = {p for p in read_inputs(named | {".github/scripts/go_legs.py", SCRIPT, WRAPPER}, files)
                               if not p.startswith(f"{WORKFLOWS}/")}
                legs.definitions = [(path, ("jobs", job_id)), (path, ("env",)), (path, ("defaults",)),
                                    (path, ("on",)), (WORKFLOW, ("jobs", AFFECTED_JOB)), (WORKFLOW, ("env", SWITCH))]
                legs.definitions += [(WORKFLOW, ("jobs", caller)) for caller in callers.get(path, [])]
                found[key] = legs
    return found


def definition(text: str | None, keys: tuple):
    """The part of a workflow file the keys lead to, parsed, so a comment or a reflow is no change.
    YAML 1.1 reads a bare `on` key as True, so `on` is looked for under both."""
    value = yaml.safe_load(text) if text else None
    for key in keys:
        if not isinstance(value, dict):
            return None
        value = value.get(key, value.get(True)) if key == "on" else value.get(key)
    return value


def module_files(document: dict, files: set) -> set:
    """go.work and go.work.sum, and the go.mod and go.sum of each module go.work uses: they decide
    what every package of the workspace compiles."""
    found = {"go.work", "go.work.sum"} & files
    work = head_text("go.work") if "go.work" in files else ""
    uses = re.findall(r"^\s*(?:use\s+)?(\./\S+|\.)\s*$", work or "", re.M)
    for use in uses + list(document):
        directory = os.path.normpath(use)
        found |= {f"{directory}/go.mod", f"{directory}/go.sum"} & files
    return found


def module_paths(files: set) -> tuple:
    """The module path of each go.mod among `files` (the workspace's): a package whose import path
    is one of them or under one is first-party, and its directory is in this tree or nowhere."""
    found = []
    for path in sorted(f for f in files if os.path.basename(f) == "go.mod"):
        line = MODULE_LINE.search(head_text(path) or "")
        if line:
            found.append(line.group(1))
    return tuple(found)


# ---------------------------------------------------------------- go list
class Go:
    """The go commands of the selection, in ci_filter_coverage's Session: GOPROXY=off over an empty
    module cache, the module's own toolchain, CGO under -race. A command that fails, or a
    first-party package that cannot load, raises: what imports it is then unknown."""

    def __init__(self, first_party: tuple):
        self.session = Session()
        self.first_party = first_party
        self.checked: set = set()

    def failed(self) -> None:
        if self.session.problems:
            raise RuntimeError("; ".join(p.message for p in self.session.problems))

    @staticmethod
    def command(workdir: str, packages: tuple, test: bool, race: bool, tags: str, env: dict):
        return run(workdir, packages, f"the selection's listing in {workdir}", mode="test" if test else "build",
                   env=env, race=race, tags=tags)

    def is_first_party(self, path: str) -> bool:
        return any(path == module or path.startswith(module + "/") for module in self.first_party)

    def listing(self, workdir: str, packages: tuple, *, test: bool, race: bool = False, tags: str = "",
                env: dict) -> list:
        """`go list -deps` of the packages (with -test, of their test binaries), each with LISTED."""
        command = self.command(workdir, packages, test, race, tags, env)
        listing = self.session.listing(command, LISTED)
        self.failed()
        if command not in self.checked:
            for entry in listing:
                failure = " ".join(((entry.get("Error") or {}).get("Err", "")).split())[:300]
                directory = rel(entry["Dir"]) if entry.get("Dir") else None
                if directory and failure and "build constraints exclude" not in failure:
                    raise RuntimeError(f"{directory}: go list cannot load {entry.get('ImportPath')}, so what imports "
                                       f"it is unknown: {failure}")
                if not entry.get("Dir") and not entry.get("Standard") and self.is_first_party(entry["ImportPath"]):
                    raise RuntimeError(f"go list finds no directory for {entry['ImportPath']}, a package of this "
                                       f"repository's modules (deleted, or moved), so what imports it is unknown: "
                                       f"{failure or 'no error given'}")
            self.checked.add(command)
        return listing

    def named(self, workdir: str, packages: tuple, *, test: bool, race: bool = False, tags: str = "",
              env: dict) -> list:
        """The packages of `listing` the command names (Session.named)."""
        self.listing(workdir, packages, test=test, race=race, tags=tags, env=env)
        return self.session.named(self.command(workdir, packages, test, race, tags, env), LISTED)

    def go_mod(self, text: str) -> dict:
        """A go.mod's content as `go mod edit -json` reads it."""
        with tempfile.NamedTemporaryFile("w", suffix=".mod", dir=self.session.tmp, delete=False) as handle:
            handle.write(text)
        out = self.session.go(run(".", (), "the go.mod a change moves", mode="build"),
                              ["mod", "edit", "-json", handle.name])
        self.failed()
        return json.loads(out)


def cut(by_path: dict, named: dict) -> list:
    """The entries of one package's test binary out of a listing of many: its test main (or the
    package, when it has no test file), and what each imports, each once. The package itself is
    reached from its test main and walked as any other: a package with only external test files
    has no test variant, and its test binary compiles its plain imports."""
    path = named["ImportPath"]
    start = path + ".test" if path + ".test" in by_path else path
    order, seen, stack = [], {start}, [start]
    while stack:
        current = stack.pop()
        order.append(by_path[current])
        for imported in by_path[current].get("Imports") or []:
            if imported not in seen and imported in by_path:
                seen.add(imported)
                stack.append(imported)
    if named not in order:
        order.append(named)
    return order


@dataclass
class Reads:
    """What one package's test binary reads: the directories it compiles, those its //ci:builds
    and reads-set lines build, the files it embeds, the globs (each with its `!` globs) its //ci:
    lines declare, whether it or such a build compiles a package of a module outside the
    repository, and the Go files it compiles."""

    dirs: set = field(default_factory=set)
    built: set = field(default_factory=set)
    files: set = field(default_factory=set)
    globs: list = field(default_factory=list)
    third_party: bool = False
    sources: set = field(default_factory=set)

    def selects(self, path: str, own_dir: str, package_dirs: set) -> str | None:
        """Why a change to `path` selects the package in `own_dir` with these reads, or None. A
        package's own _test.go files are compiled into its own test binary alone, so another
        package's test file selects this one only through a //ci: glob that names it."""
        directory = nearest(path, package_dirs)
        if directory is not None and is_test_file_of(path, directory):
            if directory == own_dir:
                return "its own test file"
        elif directory in self.dirs:
            return f"under {directory}, which its test binary compiles"
        elif directory in self.built:
            return f"under {directory}, which a //ci:builds line of a file it compiles builds"
        if path in self.files:
            return "which its test binary embeds"
        glob = next((glob for glob, but in self.globs if matches(glob, but, path)), None)
        return None if glob is None else f"which a //ci: line's glob {glob} names"


def read_set(name: str, why: str) -> list:
    """The globs of a //ci:reads-set line: ci_job_reads.py's READ_SETS, the one table, read only
    when a line names a set (its import computes every gate)."""
    from ci_job_reads import READ_SETS
    if name not in READ_SETS:
        raise ValueError(f"names read set {name}, which ci_job_reads.py's READ_SETS does not have")
    return [(read.glob, tuple(read.but)) for read in READ_SETS[name](why)]


@functools.lru_cache(maxsize=None)
def directive_words(path: str) -> tuple:
    """The //ci: lines of a Go file as Words (ci_filter_sources.file_words, the engine's own scan);
    a line that cannot be read is a ValueError naming it, since what its test reads is then
    unknown."""
    try:
        return tuple(read for _, read in file_words(path))
    except ValueError as wrong:
        raise ValueError(f"{path}: {wrong}") from wrong


def package_reads(go: Go, entries: list, own_dir: str, goarch: str) -> Reads:
    """What the test binary of the package in `own_dir` reads, from its entries of the listing.
    A package's _test.go files are compiled into its own test binary alone. A //ci:builds line's
    build is listed for the runner's architecture unless the line names one."""
    reads = Reads()
    for entry in entries:
        if entry.get("Standard"):
            continue
        directory = rel(entry["Dir"]) if entry.get("Dir") else None
        if directory is None:
            reads.third_party = True
            continue
        reads.dirs.add(directory)
        names = (entry.get("GoFiles") or []) + (entry.get("CgoFiles") or [])
        embeds = entry.get("EmbedFiles") or []
        if directory == own_dir:
            names += (entry.get("TestGoFiles") or []) + (entry.get("XTestGoFiles") or [])
            embeds += (entry.get("TestEmbedFiles") or []) + (entry.get("XTestEmbedFiles") or [])
        reads.sources |= {f"{directory}/{name}" for name in names}
        reads.files |= {f"{directory}/{name}" for name in embeds}
    for source in sorted(reads.sources):
        for line in directive_words(source):
            if line.kind == "reads":
                reads.globs.append((line.globs[0], tuple(g[1:] for g in line.globs[1:])))
            elif line.kind == "reads-set":
                reads.globs += read_set(line.name, line.why)
            else:
                env = {"GOARCH": goarch, **dict(line.env)}
                for built in go.listing(line.directory, line.packages, test=False, env=env):
                    if built.get("Standard"):
                        continue
                    directory = rel(built["Dir"]) if built.get("Dir") else None
                    if directory is None:
                        reads.third_party = True
                    else:
                        reads.built.add(directory)
    return reads


# ---------------------------------------------------------------- what a change moves
def requirements_only(go: Go, path: str, base: Base) -> bool:
    """Whether a change to a workspace file moves module versions alone: a go.mod's requirements,
    or a go.sum or go.work.sum line. Not with no base, for go.work, or for a go.mod changed beyond
    its requirements, each of which decides how every package builds."""
    old, new = base.text(path), head_text(path)
    if old is None or new is None:
        return False
    if os.path.basename(path) == "go.mod":
        before, after = go.go_mod(old), go.go_mod(new)
        return {k: v for k, v in before.items() if k != "Require"} == {k: v for k, v in after.items() if k != "Require"}
    return os.path.basename(path) in ("go.sum", "go.work.sum")


def placements(document: dict, module: str, job: str) -> dict | None:
    """Each package of `job` in a parsed go-legs.yaml, to its places (leg, group) sorted, as
    go_legs.places reads them; None where it has no such job."""
    entry = document.get(module)
    if not entry or job not in job_legs(entry):
        return None
    found: dict = {}
    for place in go_legs.places(entry)[0]:
        if place.job == job:
            found.setdefault(place.package, []).append((str(place.leg), place.group or ""))
    return {package: sorted(places) for package, places in found.items()}


@dataclass
class JobPackages:
    """A job's packages and what each one's test binary reads."""

    module: str
    legs: dict
    packages: list  # as go-legs.yaml names them, the unlisted ones last
    unlisted: set  # those no place names, which the job's last leg runs
    reads: dict  # package -> (its directory, its Reads)


class Selector:
    """What each job's packages read, computed from the working tree and its go-legs.yaml
    (`document`, parsed), and the selection for a change to it (`choose`)."""

    def __init__(self, document: dict):
        self.document = document
        self.files = set(repo_files())
        self.legs_jobs = legs_jobs(self.document, self.files)
        missing = [job for entry in self.document.values() for job in job_legs(entry) if job not in self.legs_jobs]
        if missing:
            raise LookupError(f"no step of ci.yaml or a workflow it calls pipes go_legs.py for job(s) "
                              f"{', '.join(missing)} of {go_legs.LEGS}")
        self.workspace = module_files(self.document, self.files)
        self.go = Go(module_paths(self.workspace))
        self.package_dirs: set = set()
        module_packages = {module: go_legs.go_packages(module) for module in self.document}
        for module, packages in module_packages.items():
            self.package_dirs |= {module if p == "." else f"{module}/{p}" for p in packages}
        self.jobs: dict = {}
        for module, entry in self.document.items():
            home = entry.get(go_legs.UNLISTED)
            extra = go_legs.unlisted(entry, module_packages[module]) if home in entry else []
            for job, legs in job_legs(entry).items():
                self.jobs[job] = self.job_packages(module, job, legs, extra if job == home else [])

    def job_packages(self, module: str, job: str, legs: dict, extra: list) -> JobPackages:
        where = self.legs_jobs[job]
        packages = list(go_legs.job_packages(legs)) + [p for p in extra if p not in go_legs.job_packages(legs)]
        arguments = tuple(go_legs.package_argument(p) for p in packages)
        build = {"test": True, "race": where.race, "tags": where.tags, "env": {"GOARCH": where.goarch}}
        listing = self.go.listing(module, arguments, **build)
        self.package_dirs |= {d for e in listing if e.get("Dir") and not e.get("Standard")
                              for d in [rel(e["Dir"])] if d and d != "."}
        by_path = {entry["ImportPath"]: entry for entry in listing}
        found = {}
        for named in self.go.named(module, arguments, **build):
            own_dir = rel(named["Dir"])
            package = "." if own_dir == module else own_dir[len(module) + 1:]
            found[package] = (own_dir, package_reads(self.go, cut(by_path, named), own_dir, where.goarch))
        absent = [p for p in packages if p not in found]
        if absent:
            raise LookupError(f"go list of {job}'s packages lists no package {', '.join(absent)}")
        return JobPackages(module, legs, packages, set(extra), found)

    def choose(self, changed: list, said: list, base: Base) -> dict:
        """The selection for a change to `changed` from `base`, each reason appended to `said`."""
        said.append(f"{len(changed)} changed path(s)")
        third_party = False
        for path in (p for p in changed if p in self.workspace):
            if not requirements_only(self.go, path, base):
                said.append(f"every leg of every job and every package: {path} decides what every package compiles")
                return every(self.document)
            third_party = True
            said.append(f"{path} moves a module version: every package that compiles a module outside the repository")
        return {job: self.choose_job(job, changed, third_party, base, said) for job in self.jobs}

    def whole_job(self, job: str, changed: list, base: Base) -> str | None:
        """Why the change selects every package of `job`, or None: a file its legs run, or a part of
        a workflow that defines them."""
        where = self.legs_jobs[job]
        for path in changed:
            if path in where.inputs or any(path.startswith(tree + "/") for tree in where.input_trees):
                return f"{path} is a file its legs run"
        for path, keys in where.definitions:
            if path in changed and definition(base.text(path), keys) != definition(head_text(path), keys):
                return f"{path} changes {'.'.join(map(str, keys))}"
        return None

    def choose_job(self, job: str, changed: list, third_party: bool, base: Base, said: list) -> dict:
        found = self.jobs[job]
        reason = self.whole_job(job, changed, base)
        moved: dict = {}
        if reason is None and go_legs.LEGS in changed:
            old = placements(legs_document(base.text(go_legs.LEGS)), found.module, job)
            new = placements(self.document, found.module, job)
            if old is None or new is None:
                reason = f"{go_legs.LEGS} has no {job} to compare"
            else:
                moved = {package: f"{go_legs.LEGS}, which moves it to {new.get(package)} from {old.get(package)}"
                         for package in found.packages if old.get(package) != new.get(package)}
        if reason is not None:
            said.append(f"{job}: every leg and every package: {reason}")
            return whole(found.legs)
        why: dict = dict(moved)
        for package in found.packages:
            if package in why:
                continue
            own_dir, reads = found.reads[package]
            if go_legs.groups_path(found.module, package) in changed:
                why[package] = f"{go_legs.groups_path(found.module, package)}, its groups"
                continue
            for path in changed:
                selected = reads.selects(path, own_dir, self.package_dirs)
                if selected is not None:
                    why[package] = f"{path}, {selected}"
                    break
            if package not in why and third_party and reads.third_party:
                why[package] = "a module version moves, and it compiles a module outside the repository"
        chosen = [p for p in found.packages if p in why]
        own = self.legs_jobs[job].own_steps
        started = sorted({int(number) for number, listed in found.legs.items()
                          if any(go_legs.item(e)[0] in why for e in listed or [])}
                         | ({int(go_legs.last_leg(found.legs))} if found.unlisted & set(chosen) else set())
                         | (own & set(map(int, found.legs))))
        said.append(f"{job}: legs {started or 'none'} of {numbers(found.legs)}, {len(chosen)} of "
                    f"{len(found.packages)} packages" + (f" (legs {sorted(own)} carry steps of their own)" if own else ""))
        said += [f"  {package}: {why[package]}" for package in chosen]
        return {"legs": started, "packages": chosen}


def changed_paths(base: str) -> list:
    """The paths the working tree changes from `base`, NUL-separated so git quotes none."""
    out = subprocess.run(["git", "diff", "-z", "--no-renames", "--name-only", base], cwd=REPO, capture_output=True,
                         check=True).stdout
    return sorted({os.fsdecode(path) for path in out.split(b"\0") if path})


def main(argv: list) -> int:
    parser = argparse.ArgumentParser(prog="go_legs_select.py")
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--base", help="the commit the change is measured from, to the working tree (HEAD in CI)")
    group.add_argument("--paths", nargs="*", help="the changed paths, repository-relative, with no base to read")
    group.add_argument("--every", action="store_true", help="every leg of every job")
    options = parser.parse_args(argv)
    document = legs_document(head_text(go_legs.LEGS))
    if options.every:
        print(json.dumps(every(document), sort_keys=True, separators=(",", ":")))
        return 0
    said: list = []
    try:
        if options.base is not None:
            changed = changed_paths(options.base)
            said.append(f"the change is the working tree's from {options.base}")
        else:
            changed = sorted({path for path in options.paths if path})
        selection = Selector(document).choose(changed, said, Base(options.base))
    except Exception as failure:  # any failure selects every leg, and says why
        traceback.print_exc(file=sys.stderr)
        print(f"::warning::go_legs_select.py: every leg of every Go legs job runs every package, since the selection "
              f"could not be computed: {' '.join(str(failure).split())[:500]}", file=sys.stderr)
        selection = every(document)
    for line in said:
        print(line, file=sys.stderr)
    print(json.dumps(selection, sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
