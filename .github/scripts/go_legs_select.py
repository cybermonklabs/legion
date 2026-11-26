#!/usr/bin/env python3
"""Which legs of each Go legs job a change starts, and which of its packages they run.

    go_legs_select.py --base <commit>     the selection for the change from <commit> to HEAD
    go_legs_select.py --paths <path>...   the selection for a change to exactly those paths
    go_legs_select.py --every             every leg of every job, each with every package

Run from anywhere; ci.yaml's affected job runs it through go-legs-select.sh. It prints one JSON
object, a key per job of .github/go-legs.yaml:

    {"daemon-go": {"legs": [2], "packages": ["cmd/legion", "internal/api"]},
     "envoy-go": {"legs": [1, 2, 3, 4, 5], "packages": "all"}, ...}

`legs` are the legs the job's matrix starts and `packages` the packages of the module (as
go-legs.yaml names them) those legs run, or `all`. go_legs.py reads it in each leg
(GO_LEGS_SELECTED) and prints only the leg's selected packages; a leg that runs a group of one
package's tests runs that group when the package is selected. Which jobs run at all is the changes
job's filters' to say: this says, inside a job that runs, which of its packages the change reaches.
Everything but the JSON goes to standard error: per job what was selected, and per package the
first changed path that selected it.

WHAT SELECTS A PACKAGE of a job. The job's packages (the file's, and, in the job a module's
`unlisted` names, each package `go list ./...` prints and no place names, which go_legs.py runs in
that job's last leg) are listed once, `go list -deps -test` under the build flags its legs step
passes go_legs.py (-race), and each package's test binary is cut from that listing: the package,
its test variants and everything they import. A changed path selects
  * the package whose directory holds it (the nearest directory above it that go list knows as a
    package of a module), when it is a _test.go file of that package or under a testdata directory
    below it: only that package's own test binary reads it;
  * otherwise every package whose test binary compiles that directory: the package itself, and
    every package that imports it, directly or not, in its test or its non-test build. So a change
    to a shared package or a shared contract selects its consumers. This holds for a path the head
    no longer has (a deleted or renamed file) too, and for any file beside the source;
  * every package whose test binary embeds it (//go:embed);
  * every package whose test binary compiles a Go file whose //ci: lines (the block right after its
    package clause; .github/ci-notes/ci.md#which-packages-a-go-leg-runs) declare a read of it:
    `//ci:reads <glob> [!<glob>...] <why>` reads the files the glob matches; `//ci:builds <dir>
    <package>... [NAME=value...] <why>` builds those packages, so it reads every directory their
    build compiles; `//ci:reads-set <name> <why>` reads a set READ_SETS names.
The rest is read from the change's base (Base), as content rather than as a touched path:
  * go.work, or a go.mod change to anything but its requirements: every package of every job;
  * a go.mod requirement or a go.sum or go.work.sum line: the packages whose test binaries import a
    module whose version moves (moved_modules); a moved module no package imports itself, every
    package, since the listing cannot say which third-party package imports it;
  * a file a legs job runs (LegsJob.inputs: the scripts its steps name and what those run or
    source, its local actions, go_legs.py, this script and go-legs-select.sh): every package of
    that job;
  * a legs workflow file, and ci.yaml: every package of the job only when the legs' definition
    changed (LegsJob.definitions, parsed, so a comment selects none);
  * go-legs.yaml: only the packages whose leg or group moved; a groups file its own package.
Anything else selects no package unless a rule above names it: a changelog fragment, a dashboard
file, another workflow. With no base (--paths), each of these rules selects every package.
A leg that carries a step of its own (`if: matrix.leg == N`: daemon-go's Stage 1 proof, the
forced-order tests) starts whenever its job runs, so that step runs; its go test then runs the
leg's selected packages, or none.

WHEN THE ANSWER IS EVERY LEG. --every; any failure (a go command that fails, a first-party package
go list cannot load, a job of go-legs.yaml no workflow step runs the legs of, an exception), the
JSON being --every's and standard error saying why.

--check holds every //ci: line of the repository to the files it names, and exits 1 naming each
line that cannot be read, names no file, leaves none past its `!` globs, or builds what does not
compile: the affected job runs it, since a line held to nothing selects nothing.
"""

from __future__ import annotations

import argparse
import functools
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import traceback
from dataclasses import dataclass, field
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import yaml  # noqa: E402

import go_legs  # noqa: E402

ROOT = go_legs.ROOT
SCRIPT = ".github/scripts/go_legs_select.py"
WRAPPER = ".github/scripts/go-legs-select.sh"
WORKFLOWS = ".github/workflows"
CI = f"{WORKFLOWS}/ci.yaml"
ALL = "all"
# ci.yaml's job that runs this selection, and its switch for merge groups (go-legs-select.sh).
AFFECTED_JOB = "affected"
SWITCH = "CI_MERGE_GROUP_SELECTS"
# The call of go_legs.py in a legs step: the module, the job, and the build flags after the leg and
# the leg count, which its go test passes too.
LEGS_CALL = re.compile(r"go_legs\.py\s+(\S+)\s+(\S+)\s+\$\{\{[^}]*\}\}\s+\$\{\{[^}]*\}\}((?:\s+-[^\s|]+)*)\s*\|")
# A step's if: that runs it in one leg of its matrix alone.
ONE_LEG = re.compile(r"^\s*(?:\$\{\{)?\s*matrix\.leg\s*==\s*(\d+)\s*(?:\}\})?\s*$")
# A //ci: line, as .github/scripts/ci_filter_sources.py documented the format.
DIRECTIVE = re.compile(r"^\s*//ci:(\S*)(?:[ \t]+(.*?))?\s*$")
ENV_WORD = re.compile(r"^[A-Z_][A-Z0-9_]*=\S*$")
# What a //ci:reads-set line reads: the sets the read table computed, each as globs or as the
# directories a build of some packages compiles (READ_SETS). A set not named here reads every file
# of the repository, so a line naming a new set selects its package on any change until it is
# added here.
READ_SETS = {
    # cmd/cipher-refgen parses the API package and the approval page's, and each package of the
    # module they import (`packages/envoy/cmd/cipher-refgen/source.go#importDir`).
    "refgen_api": {"builds": [("packages/envoy", ("./internal/cipher/api", "./internal/cipher/approval"), ())]},
    # cmd/dispatch's two-build CLI test runs packages/envoy-client's bin/dispatch.ts.
    "envoy_client_dep": {"reads": ["packages/envoy-client/**"]},
}
# A token of a step's text or a script's that may name a repository file.
PATH_TOKEN = re.compile(r"[A-Za-z0-9_.@+-]*(?:/[A-Za-z0-9_.@+-]+)+|[A-Za-z0-9_.@+-]+\.(?:sh|py|yaml|yml|json)")


def legs_file() -> dict:
    """Each module of .github/go-legs.yaml to its jobs, each job to its legs {leg number: entries}."""
    document = yaml.safe_load((ROOT / go_legs.LEGS).read_text()) or {}
    return {module: {job: legs for job, legs in entry.items() if job not in go_legs.NOT_JOBS}
            for module, entry in document.items() if isinstance(entry, dict)}


def numbers(legs: dict) -> list:
    return sorted(int(number) for number in legs)


def every(modules: dict) -> dict:
    return {job: {"legs": numbers(legs), "packages": ALL} for jobs in modules.values() for job, legs in jobs.items()}


@functools.lru_cache(maxsize=None)
def glob_regex(glob: str) -> re.Pattern:
    """A repository glob as a regex over a repository-relative path: `**` spans directories, `*` and
    `?` stay inside one, as the //ci:reads globs and dorny/paths-filter read them."""
    out, i = [], 0
    while i < len(glob):
        if glob.startswith("**/", i):
            out.append("(?:.*/)?")
            i += 3
        elif glob.startswith("**", i):
            out.append(".*")
            i += 2
        elif glob[i] == "*":
            out.append("[^/]*")
            i += 1
        elif glob[i] == "?":
            out.append("[^/]")
            i += 1
        else:
            out.append(re.escape(glob[i]))
            i += 1
    return re.compile("".join(out))


def matches(globs: list, path: str) -> bool:
    """Whether a read of `globs` (the first a glob, each later one an `!` glob it leaves out)
    names `path`."""
    first, *but = globs
    return bool(glob_regex(first).fullmatch(path)) and not any(glob_regex(b).fullmatch(path) for b in but)


def nearest(path: str, directories: set) -> str | None:
    parent = os.path.dirname(path)
    while parent:
        if parent in directories:
            return parent
        parent = os.path.dirname(parent)
    return None


def is_own_test_file(path: str, directory: str) -> bool:
    """A path under a package's directory that only that package's own test binary reads: a _test.go
    file of the package, or anything under a testdata directory below it."""
    below = path[len(directory) + 1:]
    return (path.endswith("_test.go") and "/" not in below) or "testdata" in below.split("/")[:-1]


def repository_files() -> set:
    listed = subprocess.run(["git", "ls-files", "-z"], cwd=ROOT, capture_output=True, check=True).stdout
    return {os.fsdecode(path) for path in listed.split(b"\0") if path}


# ---------------------------------------------------------------- where each job's legs run
@dataclass
class LegsJob:
    """Where a job of go-legs.yaml runs: the workflow, its matrix job and the legs step, the build
    flags that step passes, the legs that carry a step of their own, the files the legs run, and
    the parts of the workflows whose definition decides how they run."""

    module: str
    job: str
    workflow: str
    matrix_job: str
    flags: tuple
    own_steps: set = field(default_factory=set)  # the legs a step runs in alone
    inputs: set = field(default_factory=set)  # repository paths the legs run or read whole
    input_trees: set = field(default_factory=set)  # repository directories, each with all below it
    # (workflow file, the keys down to the part of it that defines the legs): the legs' matrix job,
    # and in ci.yaml the job that calls the file, the affected job and the switch.
    definitions: list = field(default_factory=list)


def named_files(text: str, directories: list, files: set) -> set:
    """The repository files `text` names, each token read from each of `directories`."""
    found = set()
    for token in PATH_TOKEN.findall(text):
        token = token.lstrip("./") if token.startswith("./") else token
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
        try:
            text = (ROOT / path).read_text(errors="replace")
        except OSError:
            continue
        code = "\n".join(re.sub(r"(^|\s)#.*$", "", line) for line in text.splitlines())
        directory = os.path.dirname(path)
        for found in named_files(code, [directory, os.path.dirname(directory), "."], files):
            if found not in seen and not found.endswith((".go", ".md")):
                seen.add(found)
                queue.append(found)
    return seen


def legs_jobs(modules: dict, files: set) -> dict:
    """Each job of go-legs.yaml to the LegsJob that runs its legs: the one job of a workflow of
    .github/workflows with a step that pipes `go_legs.py <module> <job>`."""
    found: dict = {}
    for path in sorted(p for p in files if p.startswith(f"{WORKFLOWS}/") and p.endswith((".yaml", ".yml"))):
        document = yaml.safe_load((ROOT / path).read_text()) or {}
        for job_id, body in (document.get("jobs") or {}).items():
            steps = (body or {}).get("steps") or []
            for step in steps:
                call = LEGS_CALL.search(str(step.get("run", "")))
                if not call or call.group(2) not in modules.get(call.group(1), {}):
                    continue
                key = call.group(2)
                if key in found:
                    raise LookupError(f"two steps run the legs of {key}: in {found[key].workflow} and {path}")
                legs = LegsJob(call.group(1), key, path, job_id, tuple(call.group(3).split()))
                named: set = set()
                for other in steps:
                    one = ONE_LEG.match(str(other.get("if", "")))
                    if one:
                        legs.own_steps.add(int(one.group(1)))
                    uses = str(other.get("uses", ""))
                    if uses.startswith("./"):
                        legs.input_trees.add(os.path.normpath(uses))
                    workdir = other.get("working-directory") or ((body.get("defaults") or {}).get("run") or {}).get(
                        "working-directory") or "."
                    text = " ".join(str(v) for v in [other.get("run", "")] + list((other.get("env") or {}).values()))
                    named |= named_files(text, [workdir, "."], files)
                named |= named_files(" ".join(str(v) for v in (body.get("env") or {}).values()), ["."], files)
                legs.inputs = {p for p in read_inputs(named | {".github/scripts/go_legs.py", SCRIPT, WRAPPER}, files)
                               if not p.startswith(f"{WORKFLOWS}/")}
                caller = [job for job, value in ((yaml.safe_load((ROOT / CI).read_text()) or {}).get("jobs") or {}).items()
                          if str((value or {}).get("uses", "")).removeprefix("./") == path]
                legs.definitions = [(path, ("jobs", job_id)), (CI, ("jobs", AFFECTED_JOB)), (CI, ("env", SWITCH))]
                legs.definitions += [(CI, ("jobs", job)) for job in caller]
                found[key] = legs
    return found


def module_files(modules: dict, files: set) -> set:
    """go.work and go.work.sum, and the go.mod and go.sum of each module go.work uses: they decide
    what every package of the workspace compiles."""
    found = {"go.work", "go.work.sum"} & files
    work = (ROOT / "go.work").read_text() if "go.work" in files else ""
    uses = re.findall(r"^\s*(?:use\s+)?(\./\S+|\.)\s*$", work, re.M)
    for use in uses + list(modules):
        directory = os.path.normpath(use)
        found |= {f"{directory}/go.mod", f"{directory}/go.sum"} & files
    return found


# ---------------------------------------------------------------- go list
class Go:
    """The go commands of the selection: GOPROXY=off over an empty module cache, so nothing is
    downloaded and a cold runner answers as a warm machine does (a third-party package then fails
    to load, which no first-party package's place in the listing depends on), under the toolchain
    the module pins, resolved once with this machine's own cache."""

    FIELDS = ("Dir,ImportPath,ForTest,Standard,Match,Imports,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles,"
              "EmbedFiles,TestEmbedFiles,XTestEmbedFiles,Error")

    def __init__(self):
        self.tmp = tempfile.mkdtemp(prefix="go-legs-select-")
        os.mkdir(f"{self.tmp}/modcache")
        root = subprocess.run(["go", "env", "GOROOT"], cwd=ROOT, capture_output=True, text=True).stdout.strip()
        self.go = str(Path(root) / "bin" / "go") if root and (Path(root) / "bin" / "go").is_file() else "go"
        self.cache: dict = {}

    def close(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def listing(self, workdir: str, args: tuple, env: tuple = (), deps: bool = True) -> list:
        key = (workdir, args, env, deps)
        if key not in self.cache:
            environment = dict(os.environ, GOPROXY="off", GOMODCACHE=f"{self.tmp}/modcache", GOFLAGS="",
                               GOWORK="", GOTOOLCHAIN="local")
            environment.update(dict(env))
            if "-race" in args:
                environment["CGO_ENABLED"] = "1"
            command = [self.go, "list", "-e", f"-json={self.FIELDS}", *(["-deps"] if deps else []), *args]
            result = subprocess.run(command, cwd=ROOT / workdir, env=environment, capture_output=True, text=True)
            if result.returncode != 0:
                raise RuntimeError(f"in {workdir}, {' '.join(command[1:])} failed: {' '.join(result.stderr.split())[:500]}")
            listing, decoder, text, at = [], json.JSONDecoder(), result.stdout, 0
            while True:
                while at < len(text) and text[at].isspace():
                    at += 1
                if at >= len(text):
                    break
                entry, at = decoder.raw_decode(text, at)
                listing.append(entry)
            for entry in listing:
                directory = rel(entry.get("Dir"))
                failure = (entry.get("Error") or {}).get("Err", "")
                if directory and failure and "build constraints exclude" not in failure:
                    raise RuntimeError(f"{directory}: go list cannot load {entry.get('ImportPath')}, so what imports "
                                       f"it is unknown: {' '.join(failure.split())[:300]}")
            self.cache[key] = listing
        return self.cache[key]

    def built_dirs(self, workdir: str, packages: tuple, env: tuple) -> set:
        """The repository directories a `go build` of `packages` in `workdir` compiles."""
        return {d for entry in self.listing(workdir, tuple(packages), env)
                if not entry.get("Standard") for d in [rel(entry.get("Dir"))] if d}


@functools.lru_cache(maxsize=None)
def rel(path: str | None) -> str | None:
    """A path as repository-relative text, or None outside the repository (the module cache)."""
    if not path:
        return None
    real, root = os.path.realpath(path), os.path.realpath(ROOT)
    return os.path.relpath(real, root) if real.startswith(root + os.sep) else None


def cut(listing: list, by_path: dict, named: dict) -> list:
    """The entries of one package's test binary out of a listing of many: its test main (or the
    package, when it has no test file), and what each imports, each once."""
    path = named["ImportPath"]
    start = path + ".test" if path + ".test" in by_path else path
    # The package itself is reached from its test main and walked as any other: a package with only
    # external test files has no test variant, and its test binary compiles its plain imports.
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


@dataclass(frozen=True)
class Directive:
    """One //ci: line: `reads` with its glob and its `!` globs, `builds` with its directory,
    packages and env, or `reads-set` with the set's name."""

    path: str
    line: int
    kind: str
    globs: tuple = ()
    directory: str = ""
    packages: tuple = ()
    env: tuple = ()
    name: str = ""


def parse_directive(path: str, line: int, kind: str, rest: str) -> Directive:
    """One //ci: line, in the grammar the read table wrote and its check held (ci_filter_sources.py's
    parse_directive), or a ValueError saying what is wrong with it."""
    words = rest.split()
    if kind == "reads":
        globs = [words[0]] if words else []
        while len(words) > len(globs) and words[len(globs)].startswith("!"):
            globs.append(words[len(globs)])
        if not globs or globs[0].startswith("!") or len(words) == len(globs):
            raise ValueError("a reads line is `//ci:reads <glob> [!<glob>...] <why>`")
        return Directive(path, line, kind, globs=(globs[0], *(g[1:] for g in globs[1:])))
    if kind == "reads-set":
        if len(words) < 2:
            raise ValueError("a reads-set line is `//ci:reads-set <name> <why>`")
        if words[0] not in READ_SETS:
            raise ValueError(f"names read set {words[0]}, which {SCRIPT}'s READ_SETS does not have "
                             f"({', '.join(sorted(READ_SETS))})")
        return Directive(path, line, kind, name=words[0])
    if kind == "builds":
        directory, i, packages, env = (words[0] if words else ""), 1, [], []
        while i < len(words) and (words[i] == "." or words[i].startswith("./")):
            packages.append(words[i])
            i += 1
        while i < len(words) and ENV_WORD.match(words[i]):
            env.append(tuple(words[i].split("=", 1)))
            i += 1
        if not packages or i == len(words) or os.path.isabs(directory) or not (ROOT / directory).is_dir():
            raise ValueError("a builds line is `//ci:builds <directory> <package>... [NAME=value...] <why>`, the "
                             "directory relative to the repository root and each package `.` or `./...`")
        return Directive(path, line, kind, directory=directory, packages=tuple(packages), env=tuple(sorted(env)))
    raise ValueError(f"//ci:{kind} is no kind of line this selection reads (reads, reads-set, builds)")


@functools.lru_cache(maxsize=None)
def directives(path: str) -> tuple:
    """The //ci: lines of a Go file, each parsed; a line that cannot be read is a ValueError naming
    it, since what its test reads is then unknown."""
    try:
        text = (ROOT / path).read_text(encoding="utf-8")
    except (OSError, UnicodeError):
        return ()
    if "//ci:" not in text:
        return ()
    found = []
    for number, line in enumerate(text.split("\n"), 1):
        match = DIRECTIVE.match(line)
        if match:
            try:
                found.append(parse_directive(path, number, match.group(1), match.group(2) or ""))
            except ValueError as wrong:
                raise ValueError(f"{path}:{number}: {wrong}") from wrong
    return tuple(found)


@dataclass
class Reads:
    """What one package's test binary reads: the directories it compiles, the files it embeds, the
    globs and build closures its //ci: lines declare, and the import paths outside the repository
    it compiles (a module's packages, which a go.mod or go.sum change can move)."""

    dirs: set = field(default_factory=set)  # what the test binary compiles
    built: set = field(default_factory=set)  # what its //ci:builds and reads-set lines build
    files: set = field(default_factory=set)
    globs: list = field(default_factory=list)
    outside: set = field(default_factory=set)


def package_reads(go: Go, entries: list, own_dir: str) -> Reads:
    """What the test binary of the package in `own_dir` reads, from its entries of the listing: every
    directory it compiles, every file it embeds, and what the //ci: lines of every Go file it
    compiles declare (a package's _test.go files are compiled into its own test binary alone)."""
    reads = Reads()
    sources = set()
    for entry in entries:
        if entry.get("Standard"):
            continue
        directory = rel(entry.get("Dir"))
        if not directory:
            reads.outside.add(str(entry.get("ImportPath", "")).split(" ")[0])
            continue
        reads.dirs.add(directory)
        names = (entry.get("GoFiles") or []) + (entry.get("CgoFiles") or [])
        embeds = entry.get("EmbedFiles") or []
        if directory == own_dir:
            names += (entry.get("TestGoFiles") or []) + (entry.get("XTestGoFiles") or [])
            embeds += (entry.get("TestEmbedFiles") or []) + (entry.get("XTestEmbedFiles") or [])
        sources |= {f"{directory}/{name}" for name in names}
        reads.files |= {f"{directory}/{name}" for name in embeds}
    for source in sorted(sources):
        for directive in directives(source):
            if directive.kind == "reads":
                reads.globs.append(directive.globs)
            elif directive.kind == "builds":
                reads.built |= go.built_dirs(directive.directory, directive.packages, directive.env)
            else:
                known = READ_SETS[directive.name]
                reads.globs += [(glob,) for glob in known.get("reads", [])]
                for workdir, packages, env in known.get("builds", []):
                    reads.built |= go.built_dirs(workdir, packages, env)
    return reads


# ---------------------------------------------------------------- what a change moves
class Base:
    """The tree a change is measured from, read file by file (`git show`); with no commit, nothing
    of it can be read, and each rule that needs it selects every package instead."""

    def __init__(self, commit: str | None):
        self.commit = commit

    @functools.lru_cache(maxsize=None)
    def text(self, path: str) -> str | None:
        if not self.commit:
            return None
        shown = subprocess.run(["git", "show", f"{self.commit}:{path}"], cwd=ROOT, capture_output=True, text=True)
        return shown.stdout if shown.returncode == 0 else None


class WorkingTree:
    """The tree a change leads to, read off the files: the head in CI."""

    @staticmethod
    def text(path: str) -> str | None:
        try:
            return (ROOT / path).read_text()
        except OSError:
            return None


def go_mod(go: Go, text: str) -> dict:
    """A go.mod's content as `go mod edit -json` reads it."""
    with tempfile.NamedTemporaryFile("w", suffix=".mod", dir=go.tmp, delete=False) as handle:
        handle.write(text)
    read = subprocess.run([go.go, "mod", "edit", "-json", handle.name], capture_output=True, text=True,
                          env=dict(os.environ, GOTOOLCHAIN="local", GOFLAGS=""))
    if read.returncode != 0:
        raise RuntimeError(f"go mod edit -json cannot read a go.mod: {' '.join(read.stderr.split())[:300]}")
    return json.loads(read.stdout)


def moved_modules(go: Go, path: str, base: Base, head) -> tuple:
    """(the module paths a change to a workspace file moves, or None for every package; why). A
    go.sum or go.work.sum: each module a line of which the change adds or removes. A go.mod: each
    module whose requirement changed, when nothing else of it did (its go version, toolchain, a
    replace, an exclude, a godebug decide what every package compiles). go.work: every package."""
    old, new = base.text(path), head.text(path)
    if old is None or new is None:
        return None, f"{path} has no {'base' if old is None else 'head'} to compare"
    name = os.path.basename(path)
    if name in ("go.sum", "go.work.sum"):
        lines = {tuple(line.split()[:2]) for line in old.splitlines() if line.strip()} ^ \
                {tuple(line.split()[:2]) for line in new.splitlines() if line.strip()}
        return {module for module, _ in lines}, f"{path} changes the sums of {len(lines)} module version(s)"
    if name == "go.mod":
        before, after = go_mod(go, old), go_mod(go, new)
        if {k: v for k, v in before.items() if k != "Require"} != {k: v for k, v in after.items() if k != "Require"}:
            return None, f"{path} changes more than its requirements"
        required = lambda document: {(r["Path"], r["Version"]) for r in document.get("Require") or []}  # noqa: E731
        return {module for module, _ in required(before) ^ required(after)}, f"{path} changes requirements"
    return None, f"{path} decides how every module of the workspace builds"


def definition(text: str | None, keys: tuple):
    """The part of a workflow file the keys lead to, parsed, so a comment or a reflow is no change."""
    value = yaml.safe_load(text) if text else None
    for key in keys:
        value = value.get(key) if isinstance(value, dict) else None
    return value


def placements(text: str | None, module: str, job: str) -> dict | None:
    """Each package of `job` in a go-legs.yaml text, to its places: (leg, group) sorted."""
    document = yaml.safe_load(text) if text else None
    legs = ((document or {}).get(module) or {}).get(job) if isinstance(document, dict) else None
    if not isinstance(legs, dict):
        return None
    found: dict = {}
    for number, listed in legs.items():
        for written in listed or []:
            package, group = go_legs.item(written)
            found.setdefault(package, []).append((str(number), group or 0))
    return {package: sorted(places) for package, places in found.items()}


def check() -> list:
    """What keeps the //ci: lines from saying what the tests read: a line that cannot be read, a
    glob that names no file of the repository or whose `!` globs leave none, and a build whose go
    command fails. Such a line holds its package to nothing, so a change to what its test really
    reads would select no package for it."""
    files = repository_files()
    problems, go = [], Go()
    try:
        for path in sorted(p for p in files if p.endswith(".go")):
            try:
                found = directives(path)
            except ValueError as wrong:
                problems.append(str(wrong))
                continue
            for directive in found:
                where = f"{path}:{directive.line}"
                if directive.kind == "reads":
                    first, *but = directive.globs
                    named = [f for f in files if glob_regex(first).fullmatch(f)]
                    if not named:
                        problems.append(f"{where}: //ci:reads {first} names no file of the repository")
                    elif but and not [f for f in named if not any(glob_regex(b).fullmatch(f) for b in but)]:
                        problems.append(f"{where}: //ci:reads {first}'s `!` globs leave none of the files it names")
                elif directive.kind == "builds":
                    try:
                        go.listing(directive.directory, directive.packages, directive.env, deps=False)
                    except RuntimeError as failed:
                        problems.append(f"{where}: //ci:builds {directive.directory} "
                                        f"{' '.join(directive.packages)}: {failed}")
    finally:
        go.close()
    return problems


# ---------------------------------------------------------------- the selection
class Selector:
    """What each job's packages read, computed once from the head tree, and the selection for any
    change to it (`choose`): the backtest asks it for many."""

    def __init__(self, go: Go):
        self.go = go
        self.modules = legs_file()
        files = repository_files()
        self.jobs = legs_jobs(self.modules, files)
        missing = [job for jobs in self.modules.values() for job in jobs if job not in self.jobs]
        if missing:
            raise LookupError(f"no step of a workflow pipes go_legs.py for job(s) {', '.join(missing)} of "
                              f"{go_legs.LEGS}")
        self.workspace = module_files(self.modules, files)
        self.package_dirs = {f"{m}/{p}" if p != "." else m for m in self.modules for p in go_legs.go_packages(m)}
        # Each job's packages: the file's, and in the job a module's `unlisted` names, each package
        # `go list ./...` prints and no place names, which go_legs.py runs in that job's last leg.
        document = yaml.safe_load((ROOT / go_legs.LEGS).read_text()) or {}
        self.packages: dict = {}  # job -> its packages
        self.unlisted: dict = {}  # job -> (the leg that runs the packages no place names, those packages)
        for module, entries in self.modules.items():
            entry = document[module]
            home = entry.get(go_legs.UNLISTED)
            extra = go_legs.unlisted(entry, go_legs.go_packages(module)) if home in entries else []
            for job, legs in entries.items():
                self.packages[job] = list(go_legs.job_packages(legs))
                if job == home and extra:
                    self.unlisted[job] = (int(go_legs.last_leg(legs)), set(extra))
                    self.packages[job] += [p for p in extra if p not in self.packages[job]]
        listings = {}
        for module, entries in self.modules.items():
            for job, legs in entries.items():
                wanted = tuple(go_legs.package_argument(p) for p in self.packages[job])
                listings[job] = go.listing(module, ("-test", *self.jobs[job].flags, *wanted))
                self.package_dirs |= {d for e in listings[job] if not e.get("Standard")
                                      for d in [rel(e.get("Dir"))] if d}
        self.reads: dict = {}  # job -> {package: (its directory, its Reads)}
        for module, entries in self.modules.items():
            for job, legs in entries.items():
                by_path = {entry.get("ImportPath"): entry for entry in listings[job]}
                found = {}
                for named in listings[job]:
                    if named.get("Match") and not named.get("ForTest"):
                        own_dir = rel(named["Dir"])
                        package = "." if own_dir == module else own_dir[len(module) + 1:]
                        found[package] = (own_dir, package_reads(go, cut(listings[job], by_path, named), own_dir))
                absent = [p for p in self.packages[job] if p not in found]
                if absent:
                    raise LookupError(f"go list of {job}'s packages lists no package {', '.join(absent)}")
                self.reads[job] = found

    def choose(self, changed: list, said: list, base: Base | None = None, head=None) -> dict:
        """The selection for a change to `changed` from `base` to `head` (the working tree when not
        given), each reason appended to `said`."""
        base, head = base or Base(None), head or WorkingTree()
        said.append(f"{len(changed)} changed path(s)")
        modules: set = set()
        for path in (p for p in changed if p in self.workspace):
            moved, why = moved_modules(self.go, path, base, head)
            said.append(why)
            if moved is None:
                said.append(f"every leg of every job and every package: {why}")
                return every(self.modules)
            modules |= moved
        known = {module for module in modules
                 if any(imports_module(reads.outside, module) for packages in self.reads.values()
                        for _, reads in packages.values())}
        if modules - known:
            said.append(f"every leg of every job and every package: the change moves module(s) "
                        f"{', '.join(sorted(modules - known))}, which no package imports itself, so which "
                        f"packages compile them is not in this listing")
            return every(self.modules)
        return {job: self.choose_job(module, job, legs, changed, modules, base, head, said)
                for module, entries in self.modules.items() for job, legs in entries.items()}

    def choose_job(self, module: str, job: str, legs: dict, changed: list, modules: set, base: Base, head,
                   said: list) -> dict:
        where = self.jobs[job]
        read = next((path for path in changed if path in where.inputs
                     or any(path.startswith(tree + "/") for tree in where.input_trees)), None)
        if not read:
            for path, keys in where.definitions:
                if path in changed and definition(base.text(path), keys) != definition(head.text(path), keys):
                    read = f"{path} ({'.'.join(keys)}{', with no base to compare' if base.text(path) is None else ''})"
                    break
        if read:
            said.append(f"{job}: every leg and every package: {read} is what its legs run")
            return {"legs": numbers(legs), "packages": ALL}
        packages = self.packages[job]
        why: dict = {}
        if go_legs.LEGS in changed:
            old, new = placements(base.text(go_legs.LEGS), module, job), placements(head.text(go_legs.LEGS), module, job)
            if old is None or new is None:
                said.append(f"{job}: every leg and every package: {go_legs.LEGS} has no {job} to compare")
                return {"legs": numbers(legs), "packages": ALL}
            for package in packages:
                if old.get(package) != new.get(package):
                    why[package] = f"{go_legs.LEGS}, which moves it to {new.get(package)} from {old.get(package)}"
        for package in packages:
            groups = go_legs.groups_path(module, package)
            if package not in why and groups in changed:
                why[package] = f"{groups}, its groups"
        for package in packages:
            if package in why:
                continue
            own_dir, reads = self.reads[job][package]
            for path in changed:
                directory = nearest(path, self.package_dirs)
                if directory is not None and is_own_test_file(path, directory):
                    hit = "its own test file or testdata" if directory == own_dir else ""
                else:
                    hit = (f"under {directory}, which its test binary compiles" if directory in reads.dirs else
                           f"under {directory}, which a //ci:builds line of a file it compiles builds"
                           if directory in reads.built else "")
                hit = hit or ("which its test binary embeds" if path in reads.files else "")
                hit = hit or next((f"which a //ci:reads {globs[0]} of a file it compiles names"
                                   for globs in reads.globs if matches(globs, path)), "")
                if hit:
                    why[package] = f"{path}, {hit}"
                    break
            moved = next((m for m in sorted(modules) if imports_module(reads.outside, m)), None)
            if package not in why and moved:
                why[package] = f"its test binary compiles module {moved}, whose version the change moves"
        chosen = [p for p in packages if p in why]
        unlisted_leg, unlisted = self.unlisted.get(job, (None, set()))
        started = sorted({int(number) for number, listed in legs.items()
                          if any(go_legs.item(e)[0] in why for e in listed or [])}
                         | ({unlisted_leg} if unlisted & set(chosen) else set())
                         | (where.own_steps & set(map(int, legs))))
        said.append(f"{job}: legs {started or 'none'} of {numbers(legs)}, {len(chosen)} of {len(packages)} packages"
                    + (f" (legs {sorted(where.own_steps)} carry steps of their own)" if where.own_steps else ""))
        said += [f"  {package}: {why[package]}" for package in chosen]
        return {"legs": started, "packages": chosen}


def imports_module(paths: set, module: str) -> bool:
    """Whether one of the import paths is a package of `module` (or of a module under its path)."""
    return any(path == module or path.startswith(module + "/") for path in paths)


def select(changed: list, said: list, base: Base) -> dict:
    """The selection for a change to `changed` from `base`, each reason appended to `said`."""
    go = Go()
    try:
        return Selector(go).choose(changed, said, base)
    finally:
        go.close()


def main(argv: list) -> int:
    parser = argparse.ArgumentParser(prog="go_legs_select.py")
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--base", help="the commit the change is measured from, to the working tree (HEAD in CI)")
    group.add_argument("--paths", nargs="*", help="the changed paths, repository-relative, with no base to read")
    group.add_argument("--every", action="store_true", help="every leg of every job")
    group.add_argument("--check", action="store_true", help="hold every //ci: line to the files it names")
    options = parser.parse_args(argv)
    if options.check:
        problems = check()
        for problem in problems:
            print(f"::error::{problem}", file=sys.stderr)
        print(f"go_legs_select.py: {len(problems)} problem(s) with the //ci: lines" if problems else
              "go_legs_select.py: every //ci: line reads, and names files of the repository", file=sys.stderr)
        return 1 if problems else 0
    modules = legs_file()
    if options.every:
        print(json.dumps(every(modules), sort_keys=True, separators=(",", ":")))
        return 0
    said: list = []
    try:
        if options.base is not None:
            changed = subprocess.run(["git", "diff", "--no-renames", "--name-only", options.base], cwd=ROOT,
                                     capture_output=True, text=True, check=True).stdout.split("\n")
            said.append(f"the change is the working tree's from {options.base}")
        else:
            changed = options.paths
        selection = select(sorted({path for path in changed if path}), said, Base(options.base))
    except Exception as failure:  # any failure selects every leg, and says why
        traceback.print_exc(file=sys.stderr)
        print(f"::warning::go_legs_select.py: every leg of every Go legs job runs every package, since the selection "
              f"could not be computed: {' '.join(str(failure).split())[:500]}", file=sys.stderr)
        selection = every(modules)
    for line in said:
        print(line, file=sys.stderr)
    print(json.dumps(selection, sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
