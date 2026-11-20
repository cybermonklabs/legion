"""The engine of check-ci-filters.sh, whose header says what it proves and why. ci_job_reads.py,
beside it, is the data it reads: each ci.yaml gate's go commands and static reads, and the files
tests read outside their packages. ci_workflow_rules.py, also beside it, holds the rules that
read none of that - what no `if:` of ci.yaml may say, what every job's `if:` must say, what
ci-caches.yaml must save for the entries ci.yaml restores, and that ci.yaml is the only workflow
of this repository running on a merge group - and main() runs them.

check-ci-filters.sh runs it from the repository root with this directory on PYTHONPATH, through
`import ci_filter_coverage` rather than as a script, so ci_job_reads.py's `from ci_filter_coverage
import ...` gets these very classes.

A step an exact entry pins (Exact) is refused, never read as reading everything, when it is not
what the entry pins (refuse_off_pin): its script or directory differs, named by the first line
that differs (pin_difference, name_list_difference) or by both directories, or something around
it (an env name outside ENV_ALLOWED, a shell, a container, an environment write that reaches it)
can change what it compiles or which tests it runs. The pin is also the table's word for which
tests the step runs, and reading everything says nothing where a filter selects every Go file
already. What surrounds a go command is read over the steps whose writes reach it: a pinned step
the steps before it, a claimed simple command the steps up to and including its own (job_up_to),
and a script's go command each job's steps up to the last one whose run: starts the script
(names_script, SCRIPT_START), or every step of a job that names it nowhere (around_job).
check-ci-filters.sh's header says why, gives the remedies - the step's own script line, or
ENV_ALLOWED for a name with which go compiles the same files and runs the same tests - and gives
the recipe that runs it as CI does, GOROOT unset.
"""

from __future__ import annotations

import atexit
import itertools
import json
import os
import re
import shutil
import secrets
import subprocess
import sys
import tempfile
import traceback
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path

import yaml
from go_build_tags import constraint_names, decode_stream, go_files, go_skips_dir, is_tag, platforms
from workflow_paths import PathsFilter, UniqueKeyLoader, events, glob_regex, unread_filter_patterns

WORKFLOW = ".github/workflows/ci.yaml"
# The workflow that fills, on a push to main, the Go cache families WORKFLOW's jobs restore.
CACHE_WORKFLOW = ".github/workflows/ci-caches.yaml"
REPO = Path.cwd().resolve()
SHOWN = 20  # uncovered files printed per gate
# A Go test file, by name: what `go list` reports as a package's TestGoFiles or XTestGoFiles
# under the build it was asked about, and what a build constraint keeps out of that listing.
TEST_SUFFIX = "_test.go"


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
        """The refusal this error is reported as, by this check and by the checks that borrow its
        readers: the file, and why it could not be read."""
        return error(self.path, f"cannot read repository input: {self.cause}")


def repository_path(path: Path) -> str:
    """The file property for a repository path, relative where that is possible."""
    try:
        return path.relative_to(REPO).as_posix()
    except ValueError:
        return str(path)


def warning_path(filename: str) -> str:
    """The annotation file property for a warning emitted while importing repository code."""
    path = Path(filename)
    return repository_path(path) if path.is_absolute() else filename


def read_repository_text(path: Path) -> str:
    """Read a repository file, retaining its path if the read or decoding fails."""
    try:
        return path.read_text()
    except Exception as exc:
        raise RepositoryInputError(repository_path(path), exc) from exc


def load_repository_yaml(path: Path):
    """Parse a repository YAML file, retaining its path if the parser refuses it. A mapping that
    holds one key twice is refused (UniqueKeyLoader), as js-yaml and GitHub's workflow parser
    refuse it, rather than read as its last copy."""
    try:
        return yaml.load(read_repository_text(path), Loader=UniqueKeyLoader)
    except RepositoryInputError:
        raise
    except yaml.YAMLError as exc:
        raise RepositoryInputError(repository_path(path), exc) from exc


def load_repository_yaml_text(path: str, text: str):
    """Parse YAML obtained from a repository file field, retaining that file's path. A repeated
    key is refused, as load_repository_yaml refuses it."""
    try:
        return yaml.load(text, Loader=UniqueKeyLoader)
    except yaml.YAMLError as exc:
        raise RepositoryInputError(path, exc) from exc




def annotation_line(annotation: Annotation) -> str:
    """The workflow command an annotation is written as, its file and message escaped."""
    location = f" file={escape_property(annotation.file)}" if annotation.file else ""
    return f"::{annotation.severity}{location}::{escape_annotation_message(annotation.message)}"


def print_annotation(annotation: Annotation) -> None:
    print(annotation_line(annotation))


def print_import_warning(caught) -> None:
    """Emit an import warning without letting its formatter print repository source text."""
    line = f"line {caught.lineno}: " if caught.lineno else ""
    print_annotation(warning(warning_path(caught.filename), f"{line}{caught.category.__name__}: {caught.message}"))


@dataclass(frozen=True)
class Read:
    """A file or glob a gate reads, and why."""

    glob: str
    why: str


@dataclass(frozen=True)
class Run:
    """One go command. `mode` "test" runs the tests (closure with -test, testdata, and RULES
    applied to the files compiled in), "vet" compiles them (closure with -test, nothing runs),
    "build" compiles the packages only; `race` compiles them under -race (go list -race, with
    CGO_ENABLED=1). A gate's own runs are pinned to the text that starts them: `step`, a step of
    the gate's job whose run: holds `text`, or `source`, a file that holds it; or they are an
    Exact entry's, which pins the step's whole script instead."""

    workdir: str
    pkgs: tuple
    why: str
    tags: str = ""
    mode: str = "test"
    env: tuple = ()  # (("GOOS", "linux"), ...)
    overlay: tuple = ()  # ((replaced file, its replacement), ...), repository-relative
    step: str = ""
    source: str = ""
    text: str = ""
    race: bool = False


def run(workdir, pkgs, why, *, tags="", mode="test", env=None, overlay=None, step="", source="", text="", race=False):
    if mode not in ("test", "vet", "build"):
        raise ValueError(f"run mode {mode!r}")
    return Run(workdir, tuple(pkgs), why, tags, mode, tuple(sorted((env or {}).items())),
               tuple(sorted((overlay or {}).items())), step, source, text, race)


@dataclass(frozen=True)
class Exact:
    """A run: step of the gate's job whose go commands are not in the simple form, pinned to its
    whole script (compared as text, after trailing whitespace, and never parsed) and to the
    directory it runs in. Its `runs`, unpinned, stand for what the script compiles: while the
    step holds exactly that script there, every go line of it is claimed and the runs' closures
    are what the job reads. Since the pin is also the table's word for which tests the step runs,
    a step whose script or directory differs is refused, naming the first line that differs or
    both directories, and so is one whose env, shell, container or job could move what it compiles
    or which tests it runs, naming what does (refuse_off_pin)."""

    step: str
    workdir: str
    script: str
    runs: tuple


def exact(step, workdir, script, runs):
    return Exact(step, workdir, script, tuple(runs))


@dataclass(frozen=True)
class Gate:
    """A dorny/paths-filter filter of the changes job and what the job (or, with `step`, the one
    step) it gates reads. `workflow` is the file that holds that job's steps: ci.yaml, or a
    workflow ci.yaml calls, since a job that calls a reusable workflow keeps its id, its
    `needs: changes` and the whole `if:` that tests the filter in ci.yaml and holds its steps in
    the called file under the same id."""

    filter: str
    job: str
    runs: tuple = ()
    reads: tuple = ()
    step: str = ""
    exact: tuple = ()
    workflow: str = WORKFLOW


def gate(filter, job, runs=(), reads=(), step="", exact=(), workflow=WORKFLOW):
    return Gate(filter, job, tuple(runs), tuple(reads), step, tuple(exact), workflow)


def gate_runs(g: Gate) -> list:
    """Every go command of the gate: its pinned runs and its Exact entries' runs."""
    return list(g.runs) + [r for e in g.exact for r in e.runs]


@dataclass(frozen=True)
class TagRun:
    """A step of ci.yaml, or of a workflow it calls, that runs a package under a build tag and
    selects with `-run` only the
    tests the tag adds, paired with the job that runs that package's untagged tests: `filter`,
    `job` and `step` are the tag step's gate, job and step name, `workflow` the file that holds
    that job and that step, `untagged_filter` and
    `untagged_job` the other job's filter and name, and `why` names the step that runs the
    untagged tests - the judgement check_tag_runs cannot derive and this table records."""

    filter: str
    job: str
    step: str
    untagged_filter: str
    untagged_job: str
    why: str
    workflow: str = WORKFLOW


# Every workflow step a why cites through at(), checked to exist so a renamed step fails here.
# Each entry is (the workflow file that holds the step, the job, the step).
CITED: list = []


def at(job: str, step: str, workflow: str = WORKFLOW) -> str:
    """A citation of a step by its name, its `uses:` (without the @ref) or its whole run: text.
    `workflow` is the file that holds the job. It is ci.yaml, or a workflow ci.yaml calls: a job
    that calls a reusable workflow keeps its gate here and its steps there."""
    CITED.append((workflow, job, step))
    return f'{os.path.basename(workflow)} {job} "{step}"'


# ---------------------------------------------------------------- the workflow documents
# The prefix of a `uses:` that names a reusable workflow of this repository. A job of ci.yaml
# that carries one keeps its id, its `needs: changes` and its whole `if:` in ci.yaml and holds
# its steps in the called file, which runs them inside the caller's own run. So the rules that
# read a job's steps, its go commands or its cache restores read a set of documents - ci.yaml
# and every file its jobs call - rather than ci.yaml alone, and each of them names the file it
# found a job in rather than the constant WORKFLOW.
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
    """ci.yaml and every workflow its jobs call, keyed by repository-relative path, ci.yaml
    first and the called ones in the order the jobs name them. A `uses:` naming a file this
    repository does not have is a problem of its own: the steps of that job are then nowhere,
    and every rule below would pass over them in silence."""
    documents = {WORKFLOW: load_repository_yaml(REPO / WORKFLOW) or {}}
    for path, job_id in called_workflows(documents[WORKFLOW]).items():
        if not (REPO / path).is_file():
            problems.append(error(WORKFLOW, f"job {job_id} calls {path}, which is not in the repository, "
                                            f"so the steps it runs are read by nothing here"))
            continue
        documents[path] = load_repository_yaml(REPO / path) or {}
    return documents


# ---------------------------------------------------------------- go list
GO_COMMAND = re.compile(r"\bgo\s+(?:-C\s+\S+\s+)?(?:test|vet|build|run|list|install|generate)\b")
FIELDS = ("Dir,ImportPath,ForTest,Standard,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles,EmbedPatterns,"
          "TestEmbedPatterns,XTestEmbedPatterns,Error")


@dataclass
class Session:
    """The go commands' environment and results. GOPROXY=off over an empty module cache: no
    download, no rewrite of a go.work.sum, and the same answer on a warm machine as on a cold
    runner. A third-party package then fails to load, which loses nothing first-party
    (check-ci-filters.sh's header says why). The toolchain is the exception that cache cannot
    hold: `toolchain()` resolves it first, under the machine's own cache and proxy."""

    problems: list[Annotation] = field(default_factory=list)
    cache: dict = field(default_factory=dict)
    toolchains: dict = field(default_factory=dict)
    tmp: str = ""

    def __post_init__(self):
        self.tmp = tempfile.mkdtemp(prefix="check-ci-filters-")
        atexit.register(shutil.rmtree, self.tmp, True)
        os.mkdir(f"{self.tmp}/modcache")

    def toolchain(self, workdir: str) -> str:
        """The go binary the module in `workdir` pins, resolved under the ambient environment.

        GOTOOLCHAIN=auto satisfies a `go` directive above the installed go by fetching that
        toolchain into GOMODCACHE, so the empty cache below would make every go command fetch it
        again and GOPROXY=off refuse: `toolchain not available`, and every closure unknown. CI
        never sees it, since setup-go installs the pinned version itself, but a developer's or an
        agent's machine on an older go does. So the toolchain is resolved once per module
        directory, with this machine's own GOMODCACHE and GOPROXY, and its own binary runs every
        go command below under GOTOOLCHAIN=local: the empty cache then holds modules alone, and a
        module pinning a toolchain this machine cannot resolve fails saying which go it wants
        rather than which download it could not make.
        """
        if workdir not in self.toolchains:
            root = subprocess.run(["go", "env", "GOROOT"], cwd=REPO / workdir,
                                  capture_output=True, text=True).stdout.strip()
            exe = Path(root) / "bin" / "go" if root else None
            self.toolchains[workdir] = str(exe) if exe and exe.is_file() else "go"
        return self.toolchains[workdir]

    def go(self, run_: Run, args: list) -> str:
        env = dict(os.environ, GOPROXY="off", GOMODCACHE=f"{self.tmp}/modcache", GOFLAGS="",
                   GOWORK="", GOTOOLCHAIN="local")
        env.update(dict(run_.env))
        if run_.race:
            env["CGO_ENABLED"] = "1"  # -race needs cgo
        result = subprocess.run([self.toolchain(run_.workdir), *args], cwd=REPO / run_.workdir,
                                env=env, capture_output=True, text=True)
        if result.returncode != 0:
            self.problems.append(error(None, f"in {run_.workdir}, go {' '.join(args)} failed, so the closure of "
                                       f"{run_.why} is unknown: {' '.join(result.stderr.split())[:500]}"))
        return result.stdout

    def overlay_args(self, run_: Run) -> list:
        if not run_.overlay:
            return []
        path = Path(self.tmp) / f"overlay-{len(self.cache)}.json"
        path.write_text(json.dumps({"Replace": {str(REPO / a): str(REPO / b) for a, b in run_.overlay}}))
        return ["-overlay", str(path)]

    def listing(self, run_: Run, deps: bool) -> list:
        key = ("list", run_.workdir, run_.pkgs, run_.tags, run_.mode, run_.race, run_.env, run_.overlay, deps)
        if key not in self.cache:
            args = ["list", "-e", f"-json={FIELDS}"]
            if deps:
                args.append("-deps")
                if run_.mode in ("test", "vet"):
                    args.append("-test")
            if run_.tags:
                args += ["-tags", run_.tags]
            if run_.race:
                args.append("-race")
            args += self.overlay_args(run_)
            self.cache[key] = decode_stream(self.go(run_, args + list(run_.pkgs)))
        return self.cache[key]

    def module_files(self, run_: Run) -> list:
        """go.work, go.work.sum and each bound module's go.mod and go.sum, or the one module's."""
        key = ("mod", run_.workdir, run_.env)
        if key not in self.cache:
            info = json.loads(self.go(run_, ["env", "-json", "GOWORK", "GOMOD"]) or "{}")
            reads = []
            gowork = info.get("GOWORK", "")
            if gowork and gowork != "off":
                why = f"the workspace of go commands in {run_.workdir} (go env GOWORK)"
                reads += [Read(rel(gowork), why), Read(rel(gowork + ".sum"), why)]
                work = json.loads(self.go(run_, ["work", "edit", "-json", gowork]) or "{}")
                for use in work.get("Use") or []:
                    moddir = (Path(gowork).parent / use["DiskPath"]).resolve()
                    reads += [Read(rel(str(moddir / name)), f"{why}, use {use['DiskPath']}") for name in ("go.mod", "go.sum")]
            elif info.get("GOMOD") and info["GOMOD"] != os.devnull:
                why = f"the module of go commands in {run_.workdir} (go env GOMOD)"
                reads += [Read(rel(info["GOMOD"]), why), Read(rel(str(Path(info["GOMOD"]).parent / "go.sum")), why)]
            self.cache[key] = [r for r in reads if r.glob]
        return self.cache[key]


def rel(path: str) -> str | None:
    try:
        return Path(path).resolve().relative_to(REPO).as_posix()
    except ValueError:
        return None


def embed_reads(dir_rel: str, patterns, why: str) -> list:
    """The files a package's //go:embed patterns read. A pattern without a wildcard reads the
    file it names, or the whole tree of the directory it names. A wildcard pattern - one holding
    `*`, `?`, `[` or `\\`, path.Match's metacharacters - reads what the go command embeds for it
    (cmd/go's resolveEmbed): each path it matches in the package's directory (embed_matches), a
    file alone and a directory as the files of its tree go:embed takes (embedded_tree). Read as
    `<dir>/*` it would count every file beside the embedded ones as read, the package's test files
    among them: a change to one of those alone would then select every job whose closure holds the
    package, and rule 17 could write no `!` pattern keeping a filter off it."""
    reads = []
    for pattern in patterns or []:
        everything = pattern.startswith("all:")
        pattern = pattern.removeprefix("all:")
        if any(c in pattern for c in "*?[\\"):
            for match in embed_matches(REPO / dir_rel, pattern):
                files = embedded_tree(match, everything) if match.is_dir() else [match]
                reads += [Read(f.relative_to(REPO).as_posix(), f"{why} (embed {pattern})") for f in files]
        elif (REPO / dir_rel / pattern).is_dir():
            reads.append(Read(f"{dir_rel}/{pattern}/**", f"{why} (embed {pattern})"))
        else:
            reads.append(Read(f"{dir_rel}/{pattern}", f"{why} (embed {pattern})"))
    return reads


def embed_matches(package: Path, pattern: str) -> list:
    """The paths a wildcard //go:embed pattern matches under a package's directory, as the go
    command finds them with filepath.Glob: one element of the pattern at a time, each matched
    against the sorted entries of every directory the elements before it matched - by path.Match
    (go_match_regex) where the element holds a metacharacter, and as that one name where it holds
    none. A `*` therefore never reaches into a subdirectory, where fnmatch's would. A pattern
    path.Match refuses matches nothing here; the go command refuses it too, so go list has already
    reported it and the check fails on that."""
    matched = [package]
    for element in pattern.split("/"):
        literal = not any(c in element for c in "*?[\\")
        regex = None if literal else go_match_regex(element)
        if not literal and regex is None:
            return []
        found = []
        for parent in matched:
            if not parent.is_dir():
                continue
            if literal:
                found += [parent / element] if os.path.lexists(parent / element) else []
            else:
                found += [parent / name for name in sorted(os.listdir(parent)) if regex.fullmatch(name)]
        matched = found
    return matched


def go_match_regex(element: str) -> re.Pattern | None:
    """One element of a //go:embed pattern as path.Match reads it, a regex over one name: `*` is
    any run of characters and `?` any one, `[...]` one character of the class and `[^...]` one
    outside it, each member a character or a `lo-hi` range (one whose ends are reversed holds
    nothing), and `\\` makes the character after it literal, in a class too. fnmatch reads these
    otherwise: its `*` crosses a `/`, `[!...]` is its negation and it has no escape. None for an
    element path.Match refuses."""
    out, index = "", 0
    while index < len(element):
        char, index = element[index], index + 1
        if char == "*":
            out += "[^/]*"
        elif char == "?":
            out += "[^/]"
        elif char == "\\":
            if index == len(element):
                return None
            out += re.escape(element[index])
            index += 1
        elif char == "[":
            negated = element.startswith("^", index)
            if negated:
                index += 1
            members, count = "", 0
            while not (element.startswith("]", index) and count):
                low, index = class_char(element, index)
                high = low
                if low is not None and element.startswith("-", index):
                    high, index = class_char(element, index + 1)
                if low is None or high is None:
                    return None
                if low <= high:
                    members += f"{re.escape(low)}-{re.escape(high)}"
                count += 1
            index += 1
            if members:
                out += f"[{'^' if negated else ''}{members}]"
            else:
                out += "[^/]" if negated else "(?!)"
        else:
            out += re.escape(char)
    return re.compile(out)


def class_char(element: str, index: int) -> tuple:
    """The character a member of a path.Match class starts with at index, a `\\` escaping it, and
    the index after it (path.Match's getEsc); None where path.Match refuses the class: a member
    that is a bare `-` or `]`, or a class the element ends inside."""
    if index >= len(element) or element[index] in "-]":
        return None, index
    if element[index] == "\\":
        index += 1
    if index + 1 >= len(element):
        return None, index
    return element[index], index + 1


# The names a module never carries, which go:embed passes over in a directory it walks whatever
# the pattern's prefix (cmd/go's isBadEmbedName).
VCS_DIRS = (".bzr", ".git", ".hg", ".svn")


def embedded_tree(top: Path, everything: bool) -> list:
    """The files go:embed takes from a directory a pattern matched: every regular file of its tree
    but, at every level below top, a name starting with `.` or `_` unless the pattern carries the
    `all:` prefix, a version-control directory, and a directory holding a go.mod, which is another
    module's. A symbolic link is no regular file, and the walk does not follow one. The go command
    also passes over a name module.CheckFilePath refuses, which this reads: that can only widen
    what a filter is held to, never narrow it."""
    def embeddable(name: str) -> bool:
        return name not in VCS_DIRS and (everything or not name.startswith((".", "_")))

    files = []
    for current, subdirs, names in os.walk(top):
        here = Path(current)
        subdirs[:] = sorted(s for s in subdirs if embeddable(s) and not os.path.exists(here / s / "go.mod"))
        files += [here / n for n in sorted(names)
                  if embeddable(n) and (here / n).is_file() and not (here / n).is_symlink()]
    return files


def test_build_files(listing: list) -> dict:
    """Per first-party package directory, the names of the files of its test build: what go list
    reports as TestGoFiles and XTestGoFiles, unioned over every entry naming that directory,
    since -test lists a package again as the variant its test binary compiles and as that
    binary's own main package. go list fills both fields from the directory scan whether or not
    the command asked for -test, so a build run's listing answers this too."""
    names: dict = defaultdict(set)
    for pkg in listing:
        d = rel(pkg["Dir"]) if pkg.get("Dir") else None
        if d is not None:
            names[d].update(pkg.get("TestGoFiles") or [], pkg.get("XTestGoFiles") or [])
    return names


def directory_reads(dir_rel: str, tests: set, why: str) -> list:
    """What a go command reads of a first-party package's directory: every file beside the
    source - the non-Go ones, which no other read names, included - minus the package's test
    files, which only a run that compiles them reads. It is `<dir>/*` while the package has
    none, and the names themselves once it has, since a glob cannot say "every file but these".

    The files left out are the test build's, which go list reports as TestGoFiles and
    XTestGoFiles, and every other `_test.go` file of the directory: one a build constraint keeps
    out of this listing is a test file of some other run's build - a `//go:build race` test of a
    package the race lane runs, a `natslive` one of the matrix - and go list reads its first
    lines here only to find that constraint. Counting it as a read of this run would say a job
    that compiles it nowhere must run on a change to it, which rule 17 could then not exclude:
    the 97 test files under a build constraint here would hold their unconstrained siblings in
    the gate filters that way, 4,566 generated patterns where the check today writes a few
    hundred, the number it prints on each run."""
    names = sorted(os.listdir(REPO / dir_rel))
    left_out = {name for name in names if name in tests or name.endswith(TEST_SUFFIX)}
    if not left_out:
        return [Read(f"{dir_rel}/*", why)]
    return [Read(f"{dir_rel}/{name}", why) for name in names
            if name not in left_out and (REPO / dir_rel / name).is_file()]


def pattern_dirs(run_: Run, tests: dict) -> list:
    """A `<root>/...` pattern reads every directory under root that holds .go files, to evaluate
    their build constraints, the packages it then excludes included; go_skips_dir says which are
    passed over. A directory's test files are left out here as they are everywhere outside a run
    that compiles them (closure below), and the run that does puts them back."""
    reads = []
    for pattern in run_.pkgs:
        if not pattern.endswith("..."):
            continue
        root = (REPO / run_.workdir / pattern.removesuffix("...").rstrip("/")).resolve()
        for current, subdirs, names in os.walk(root):
            here = Path(current)
            subdirs[:] = [s for s in subdirs if not go_skips_dir(here, s)]
            if any(n.endswith(".go") for n in names):
                d = rel(current)
                reads += directory_reads(d, tests[d],
                                         f"{run_.why} (the pattern {pattern} reads the directory's .go files)")
    return reads


def closure(session: Session, rules: dict, run_: Run, seen: set) -> list:
    """Every Read the go command reads, with the closures of the go commands its tests start.

    A package's test files - what go list reports as TestGoFiles and XTestGoFiles, their
    TestEmbedPatterns and XTestEmbedPatterns, and, for a run that runs them, its testdata and
    the RULES rows keyed on them - are read by a run whose go list ran over that package itself
    in test or vet mode, which are the modes that compile them. A run that merely holds the
    package in its closure, as a dependency or as something it builds, reads the files of its
    non-test build instead: its GoFiles, its CgoFiles, its EmbedPatterns and whatever else sits
    beside them in the directory. So a change to a _test.go file selects the jobs that compile
    that file and no others."""
    if run_ in seen:
        return []
    seen.add(run_)
    listed = session.listing(run_, deps=True)
    tests = test_build_files(listed)
    reads = list(session.module_files(run_)) + pattern_dirs(run_, tests)
    reads += [Read(b, f"{run_.why} (the overlay's replacement for {a})") for a, b in run_.overlay]
    dirs = set()
    for pkg in listed:
        if pkg.get("Standard") or not pkg.get("Dir"):
            continue
        d = rel(pkg["Dir"])
        if d is None:
            continue  # the module cache: third-party code
        load_error = (pkg.get("Error") or {}).get("Err", "")
        if load_error and "build constraints exclude" not in load_error:
            session.problems.append(error(None, f"{d}: go list cannot load {pkg.get('ImportPath')} for {run_.why}, so "
                                              f"its imports are unknown: {' '.join(load_error.split())[:300]}"))
        if (REPO / d).is_dir() and d not in dirs:
            dirs.add(d)
            reads += directory_reads(d, tests[d], f"{run_.why} (go list -deps)")
        reads += embed_reads(d, pkg.get("EmbedPatterns"), run_.why)
        if run_.mode == "test" and not pkg.get("ForTest"):
            for name in (pkg.get("GoFiles") or []) + (pkg.get("CgoFiles") or []):
                reads += apply_rules(session, rules, f"{d}/{name}", seen)
    if run_.mode in ("test", "vet"):
        # The packages the command names, which are the ones whose test files it compiles: a
        # package reached only as a dependency is in the listing above and not in this one.
        for pkg in session.listing(run_, deps=False):
            d = rel(pkg["Dir"]) if pkg.get("Dir") else None
            if d is None:
                continue
            test_files = (pkg.get("TestGoFiles") or []) + (pkg.get("XTestGoFiles") or [])
            reads += [Read(f"{d}/{name}", f"{run_.why} (tests)") for name in test_files]
            embeds = (pkg.get("TestEmbedPatterns") or []) + (pkg.get("XTestEmbedPatterns") or [])
            reads += embed_reads(d, embeds, f"{run_.why} (tests)")
            if run_.mode == "test":
                if (REPO / d / "testdata").is_dir():
                    reads.append(Read(f"{d}/testdata/**", f"{run_.why} (the testdata of a package it tests)"))
                for name in test_files:
                    reads += apply_rules(session, rules, f"{d}/{name}", seen)
    return reads


def apply_rules(session: Session, rules: dict, source: str, seen: set) -> list:
    reads = []
    for action in rules.get(source, ()):
        reads += closure(session, rules, action, seen) if isinstance(action, Run) else [action]
    return reads


GO_FILE_NAMES = ("go.mod", "go.sum", "go.work", "go.work.sum")


def module_trees_and_go_files(files: list) -> list:
    """What a claimed command this check does not reduce counts as reading: every file under the
    directory of every Go module (each directory holding a go.mod, nested ones included: testdata,
    embedded files and all), every .go file anywhere, and every go.mod, go.sum, go.work and
    go.work.sum, the root's included. A cd, `go -C` or any other move of the command can then
    only widen what it reads, never narrow it."""
    modules = {os.path.dirname(f) for f in files if os.path.basename(f) == "go.mod"}

    def in_module(path: str) -> bool:
        parent = os.path.dirname(path)
        while parent not in modules:
            if not parent:
                return False
            parent = os.path.dirname(parent)
        return True

    return [f for f in files if f.endswith(".go") or os.path.basename(f) in GO_FILE_NAMES or in_module(f)]


# ---------------------------------------------------------------- files and filters
def repo_files() -> list:
    """Tracked files and untracked ones git does not ignore, so an uncommitted new file counts
    and a build output, which no diff ever carries, does not."""
    out = []
    for extra in ([], ["--others", "--exclude-standard"]):
        result = subprocess.run(["git", "ls-files", "-z", *extra], cwd=REPO, capture_output=True)
        if result.returncode != 0:
            raise RepositoryInputError(
                repository_path(REPO),
                RuntimeError(f"git ls-files failed in {REPO}: {os.fsdecode(result.stderr).strip()}"),
            )
        out += os.fsdecode(result.stdout).split("\0")
    return sorted({f for f in out if f and (REPO / f).is_file()})



class Expander:
    """The files each glob of a filter or a declared read names. A glob's answer is kept: the
    repository's files do not change during a run, and the three rules that expand patterns -
    check_dead_patterns, the per-gate loop and check_dead_gate_patterns - ask for the same ones.
    An answer is shared with every caller, so none of them may change the list it is given."""

    def __init__(self, files: list):
        self.files = files
        self.known = set(files)
        self.by_parent = defaultdict(list)
        for f in files:
            self.by_parent[os.path.dirname(f)].append(f)
        self.expanded: dict = {}

    def expand(self, glob: str) -> list:
        found = self.expanded.get(glob)
        if found is None:
            found = self.expanded[glob] = self.sweep(glob)
        return found

    def sweep(self, glob: str) -> list:
        if glob.endswith("/**") and not any(c in glob[:-3] for c in "*?["):
            return [f for f in self.files if f.startswith(glob[:-2])]
        if glob.endswith("/*") and not any(c in glob[:-2] for c in "*?["):
            return list(self.by_parent.get(glob[:-2], []))
        if not any(c in glob for c in "*?["):
            return [glob] if glob in self.known else []
        regex = glob_regex(glob)
        return [f for f in self.files if regex.fullmatch(f)]


class Selection:
    """The files a filter selects, in the repository's own order, computed once per filter: rule
    15 (check_tag_runs), rule 9's second direction and rule 17 ask, through tag_step_files, for
    the same TAG_RUNS filters this check computes no read set for, and a sweep is a covers() call
    per file of the repository. An answer is shared with every caller, so none of them may change
    the list it is given."""

    def __init__(self, filters: dict, quantifiers: dict, files: list):
        self.filters, self.quantifiers, self.files = filters, quantifiers, files
        self.selected: dict = {}

    def of(self, name: str) -> list:
        found = self.selected.get(name)
        if found is None:
            matcher = PathsFilter(self.filters[name], self.quantifiers[name])
            found = self.selected[name] = [f for f in self.files if matcher.covers(f)]
        return found


def flatten(value) -> list:
    """A filter's patterns, an anchor's nested list flattened into it as dorny/paths-filter does."""
    if isinstance(value, str):
        return [value]
    if isinstance(value, list):
        return [p for item in value for p in flatten(item)]
    if isinstance(value, dict):  # the change-type form: {added|modified|...: pattern(s)}
        return [p for item in value.values() for p in flatten(item)]
    return []


# Where every filter of the changes job is. ci.yaml's one dorny/paths-filter step hands the action
# this path rather than a filters string of its own (rule 21 refuses any other input), and the
# action reads the file off the runner's disk, so every rule here reads that file too.
FILTERS_FILE = ".github/ci-filters.yaml"


def read_filters_file() -> str:
    """The text of .github/ci-filters.yaml. Three answers and no fourth: the text; a refusal
    naming the file as absent; and a refusal naming it as a path that is there and refuses the
    open - a directory, a file this process may not read, bytes that are not UTF-8. Each refusal
    is a RepositoryInputError, which ends the run with that one annotation: never an empty
    mapping, a default or a warning, since every job of ci.yaml is selected by a filter of this
    file and a run that read none would hold every filter to nothing."""
    path = REPO / FILTERS_FILE
    try:
        return path.read_text()
    except FileNotFoundError as exc:
        raise RepositoryInputError(FILTERS_FILE, RuntimeError(
            f"{FILTERS_FILE} is absent, and it is the file every filter of ci.yaml's changes job is read "
            f"from, so no job could be selected by one")) from exc
    except (OSError, UnicodeError) as exc:
        why = exc.strerror if isinstance(exc, OSError) and exc.strerror else str(exc)
        raise RepositoryInputError(FILTERS_FILE, RuntimeError(
            f"{FILTERS_FILE} is there and refuses the open ({type(exc).__name__}: {why}), so no filter of "
            f"ci.yaml's changes job can be read from it")) from exc


def filter_input_steps(job: dict) -> list:
    """The dorny/paths-filter steps of one job."""
    return [step for step in (job.get("steps") or [])
            if isinstance(step, dict) and str(step.get("uses", "")).startswith("dorny/paths-filter")]


def reads_filters_file(step: dict) -> bool:
    """Whether a paths-filter step hands the action exactly FILTERS_FILE, as a plain scalar.
    dorny/paths-filter v4.0.3 takes its `filters` input as the path of a file when the text holds
    no newline and no colon, and as inline YAML otherwise (src/main.ts, isPathInput); the path
    written alone is the one form that is a path to every reader, so it is the one admitted."""
    return (step.get("with") or {}).get("filters") == FILTERS_FILE


def changes_filters(document: dict, problems: list[Annotation]) -> tuple:
    """The filters of .github/ci-filters.yaml, with the quantifier the changes job reads them
    under, and the file's lines, which the rule 17 writer and the listed blocks are read from: the
    file is read once, and both come from that one read. The steps are read from ci.yaml alone,
    and no workflow ci.yaml calls: the changes job and its paths-filter step are ci.yaml's, and a
    called file is reached only through a caller job the changes job already gates.

    A paths-filter step that does not hand the action exactly that file is rule 21's refusal
    (check_filter_inputs), and no filter is read from it here: every rule of this check reads the
    file, so a filter declared anywhere else is one none of them has seen. With no step reading
    the file, the job has no filter this check knows of. Two steps that both read it under
    different quantifiers would make one filter mean two things, which is refused."""
    text = read_filters_file()
    lines = text.splitlines()
    steps = filter_input_steps((document.get("jobs") or {}).get("changes") or {})
    if not steps:
        problems.append(error(WORKFLOW, "no changes job with a dorny/paths-filter step"))
        return {}, {}, lines
    declared = set()
    for step in steps:
        if not reads_filters_file(step):
            continue
        quantifier = str((step.get("with") or {}).get("predicate-quantifier", "some"))
        if quantifier not in PathsFilter.QUANTIFIERS:
            problems.append(error(WORKFLOW, f"the changes job's paths-filter step {step.get('id')} has "
                                             f"predicate-quantifier {quantifier}, which this check does not model"))
            continue
        declared.add(quantifier)
    if len(declared) != 1:
        if declared:
            problems.append(error(WORKFLOW, f"the changes job's paths-filter steps read {FILTERS_FILE} under "
                                            f"predicate-quantifier {' and '.join(sorted(declared))}, so one filter of "
                                            f"it means two things; give every such step the same quantifier"))
        return {}, {}, lines
    quantifier = declared.pop()
    loaded = load_repository_yaml_text(FILTERS_FILE, text)
    if not isinstance(loaded, dict) or not loaded:
        problems.append(error(FILTERS_FILE, "this file holds no filter: dorny/paths-filter reads it as a mapping of "
                                            "filter name to patterns, and the changes job selects every job by it"))
        return {}, {}, lines
    filters = {str(name): flatten(value) for name, value in loaded.items()}
    return filters, {name: quantifier for name in filters}, lines


def check_filter_inputs(workflows: dict, problems: list[Annotation]) -> None:
    """Rule 21: every dorny/paths-filter step of every workflow of .github/workflows hands the
    action exactly .github/ci-filters.yaml. Every rule of this check reads that file, so a filter
    written into a workflow's own text instead would be read by none of them: it would gate its
    job on patterns no coverage rule here has seen, and nothing would say so."""
    for path, document in workflows.items():
        jobs = document.get("jobs") if isinstance(document, dict) else None
        for job_id, job in (jobs if isinstance(jobs, dict) else {}).items():
            for step in filter_input_steps(job if isinstance(job, dict) else {}):
                if reads_filters_file(step):
                    continue
                problems.append(error(path, f"the paths-filter step {step_label(step)} of job {job_id} passes a "
                                            f"`filters` input that is not exactly {FILTERS_FILE}; every filter is read "
                                            f"from that file, so one declared anywhere else is covered by no rule of "
                                            f"this check and gates its job on whatever it says, unread"))


# A `steps.<id>.outputs.<name>` of an output of the changes job: which step's verdict it hands on.
STEP_OUTPUT_REF = re.compile(r"steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)")


def check_output_steps(document: dict, filters: dict, problems: list[Annotation]) -> None:
    """Rule 19: every `steps.<id>.outputs.<name>` an output of the changes job hands on must name
    the job's dorny/paths-filter step and a filter .github/ci-filters.yaml holds. Which step
    declares a name was the other half of this rule, and the filters being one file has left it
    no subject: the step hands that file over whole, so a name is a filter of the job exactly when
    the file holds it. A reference to a paths-filter step that reads no such file is passed over
    here, since rule 21 refuses that step and no filter of it was read to hold the name to; so is
    every name while no filter was read at all, since each way that happens is refused on its own
    (changes_filters, rule 21) and holding the names to nothing would refuse every output again."""
    job = (document.get("jobs") or {}).get("changes") or {}
    steps = job.get("steps") or []
    ids = [str(s.get("id", "")) for s in steps if s.get("id")]
    filter_steps = {str(s.get("id", "")): s for s in filter_input_steps(job)}
    for output, expression in (job.get("outputs") or {}).items():
        for step_id, name in STEP_OUTPUT_REF.findall(str(expression)):
            if step_id not in ids:
                problems.append(error(WORKFLOW, f"the changes job's output {output} reads "
                                                f"steps.{step_id}.outputs.{name}, and the job has no step {step_id}, "
                                                f"so the output is empty on every run"))
            elif step_id not in filter_steps:
                problems.append(error(WORKFLOW, f"the changes job's output {output} reads "
                                                f"steps.{step_id}.outputs.{name}, and step {step_id} is no "
                                                f"dorny/paths-filter step of the job, so it writes no filter's verdict "
                                                f"and the output is empty on every run"))
            elif filters and reads_filters_file(filter_steps[step_id]) and name not in filters:
                problems.append(error(WORKFLOW, f"the changes job's output {output} reads "
                                                f"steps.{step_id}.outputs.{name}, and {FILTERS_FILE} holds no filter "
                                                f"{name}, so the output is empty on every run"))


GATE_REF = re.compile(r"needs\.changes\.outputs\.([A-Za-z0-9_-]+)")


def step_label(step: dict) -> str:
    return str(step.get("name") or step.get("id") or step.get("uses") or str(step.get("run", "")).strip())


def conditions(job: dict) -> list:
    """A job's `if:` and each of its steps', as (the step, or None for the job's, the text)."""
    return [(None, str(job.get("if", "")))] + [(s, str((s or {}).get("if", ""))) for s in job.get("steps") or []]


def where_if(job_id: str, step) -> str:
    """Where such a condition is: the job, and the step too when the condition is a step's."""
    return f"job {job_id}" + (f' step "{step_label(step)}"' if step else "")


# A job or a step of a called workflow cannot test `needs.changes.outputs.<name>`: inside a
# reusable workflow the only context carrying a caller's value is `inputs`, so the gate is
# written `inputs.<name>` there and what it gates on is whatever the calling job passes for that
# input. The reference is resolved back to the caller's expression before GATE_REF reads it,
# which is what lets the rules below read a gate that lives in a called file exactly as they
# read one written out in ci.yaml: a step with `if: inputs.go_notices == 'true'` under a caller
# passing `go_notices: ${{ needs.changes.outputs.go_notices }}` is a gate on the filter
# go_notices.
INPUT_REF = re.compile(r"\binputs\.([A-Za-z0-9_-]+)")
# The `with:` values a condition may be resolved through, and the expression each stands for.
# An output of the changes job carries the one filter it names; `needs.changes.result` carries
# none, so `inputs.changes-result != 'success' || inputs.go_notices == 'true'` reads as exactly
# what ci.yaml's `needs.changes.result != 'success' || needs.changes.outputs.go_notices ==
# 'true'` reads as today - one gate, on go_notices. The third is ci.yaml's ci-ok payload, the
# whole changes job as JSON: docs-site.yaml's report step reads it (`if: always() &&
# inputs.ci-ok-changes != ''`, which asks whether ci.yaml is the caller rather than what
# changed), and since it names no output it can carry no filter into a condition and hide none.
WITH_CHANGES = re.compile(r"^\$\{\{\s*(needs\.changes\.outputs\.[A-Za-z0-9_-]+|needs\.changes\.result"
                          r"|toJSON\(needs\.changes\))\s*\}\}$")
# A value that names the changes job at all, in any shape: what the refusal below is about. One
# that does not cannot carry a filter, so the condition reading it gates on something other than
# the paths filter and this rule leaves it alone - dispatch-browser.yaml's `lane`, which ci.yaml
# builds from the event and the pull request's labels and whose two jobs test with
# `if: inputs.lane == 'full'`, chooses which projects the browser suite runs, never whether it
# runs; the `dispatch_browser` output decides that, on the caller job's own `if:`.
NAMES_CHANGES = re.compile(r"\bneeds\.changes\b")


def input_expressions(documents: dict, problems: list[Annotation]) -> dict:
    """Per workflow ci.yaml calls, {an input a condition of that workflow reads: the expression
    ci.yaml's calling job passes for it}. The resolution may never be guessed: an input a
    condition reads whose `with:` value names the changes job in any other shape - an output
    inside a larger expression, a value built from one - is refused here, naming the input, the
    calling job and both files, since reading such a condition would silently drop the filter
    gating that job or step, or invent one.

    A value that names the changes job nowhere carries no filter and is admitted, unresolved: the
    condition then gates on something the paths filters do not decide, which is no business of
    this rule. Where such a value stands in for a gate the table declares, the gate's own check
    says so - rule 2 refuses a gate whose step has no `if:` on its filter - so nothing is hidden
    by admitting it here.

    That boundary is the conditions', and no wider. A `with:` value no condition of the called
    file reads gates nothing, whatever it holds, and is none of this rule's business; nor is an
    input the caller passes no value for, which the called workflow's own default decides and
    the caller states no gate on (ci.yaml passes neither docs-site.yaml's upload-pages-artifact
    nor its save-cache, and a step's `if:` there reads both)."""
    expressions = {}
    for path, caller in called_workflows(documents[WORKFLOW]).items():
        document = documents.get(path)
        if document is None:
            continue  # workflow_documents has already refused a `uses:` naming no file of the repository
        calling = (documents[WORKFLOW].get("jobs") or {}).get(caller) or {}
        # Keyed by the text of each name, since that is what a condition's `inputs.<name>` is:
        # a `with:` key YAML read as something else (a bare `on`, a bare `true`) is still a name
        # here rather than a lookup that raises.
        passed = {str(key): value for key, value in (calling.get("with") or {}).items()}
        read = {name for job in (document.get("jobs") or {}).values()
                for _, condition in conditions(job or {}) for name in INPUT_REF.findall(condition)}
        resolved = {}
        for name in sorted(read & set(passed)):
            value = WITH_CHANGES.match(str(passed[name]))
            if value is None:
                if NAMES_CHANGES.search(str(passed[name])):
                    problems.append(error(WORKFLOW, f"job {caller} passes {name}: {passed[name]} to {path}, "
                                                    f"whose condition reads inputs.{name}: such a condition is read "
                                                    f"through the caller's with:, so a value naming the changes job "
                                                    f"must be ${{{{ needs.changes.outputs.<filter> }}}} or "
                                                    f"${{{{ needs.changes.result }}}}, which this check can resolve"))
                continue
            resolved[name] = value.group(1)
        expressions[path] = resolved
    return expressions


def resolve_condition(condition: str, expressions: dict) -> str:
    """A condition as its caller's values make it: every `inputs.<name>` the caller passes a
    value for becomes the expression that value holds, and every other reference stands. The
    reference is matched whole, so a name holding a `-` (changes-result) resolves, and a longer
    identifier that merely starts with an input's name (inputs.go_notices_extra beside an input
    go_notices) is left alone rather than rewritten."""
    return INPUT_REF.sub(lambda m: expressions.get(m.group(1), m.group(0)), condition)


def step_matches(step: dict, ref: str) -> bool:
    return ref in (step.get("name"), step.get("id"), str(step.get("uses", "")).split("@")[0],
                   str(step.get("run", "")).strip())


def squash(text: str) -> str:
    return " ".join(text.split())


def script_lines(text: str) -> list:
    """The lines of a run: script as bash splits them (at \\n alone), stripped of spaces and tabs,
    comments and blank lines aside."""
    stripped = (line.strip(" \t") for line in text.split("\n"))
    return [line for line in stripped if line and not line.startswith("#")]


def odd_character(text: str) -> str | None:
    """The first character of text outside printable ASCII, tabs and newlines, which Python and
    bash may split, strip or read differently (a \\r, a NBSP); None when it holds none."""
    return next((c for c in text if c not in "\t\n" and not " " <= c <= "~"), None)


def script_reason(text: str) -> str:
    """'' when a run: script holds only printable ASCII, tabs and newlines; else the first other
    character (odd_character)."""
    odd = odd_character(text)
    return f"its step's script holds the character U+{ord(odd):04X}" if odd else ""


# The anchored alternation of whole test names ci_job_reads.py writes into the -run or -skip of a
# pinned script, `-run '^(Name|Name)$'`, each name a Go identifier. Where those lists are all a
# step's line and its pin differ in, the refusal names the tests either side lacks rather than
# quoting the line, which can run past a thousand characters: the text-check step's -run holds
# fifteen names. A list holding any other pattern (`TestA.*`) is quoted like any other line,
# since which tests a pattern selects is go's to say, not a set difference of its text.
NAME_LIST = re.compile(r"(-run|-skip) '\^\(([A-Za-z_][A-Za-z0-9_]*(?:\|[A-Za-z_][A-Za-z0-9_]*)*)\)\$'")
# A run of what bash splits words at, which the "spaces or tabs alone" reading compares lines by:
# not Python's str.split(), which also splits at a no-break space bash reads as part of a word.
BASH_BLANKS = re.compile(r"[ \t]+")
# How much of a differing line a refusal quotes on each side of the difference.
DIFFERENCE_CONTEXT = 40


def excerpt(line: str, start: int, end: int) -> str:
    """line[start:end], the part of a line that differs, with DIFFERENCE_CONTEXT characters on each
    side and `...` where that cuts the line."""
    low, high = max(0, start - DIFFERENCE_CONTEXT), min(len(line), end + DIFFERENCE_CONTEXT)
    return f"{'...' if low else ''}{line[low:high]}{'...' if high < len(line) else ''}"


def quoted(line: str) -> str:
    """A line of a script as a refusal quotes it whole: in backticks, or `a blank line` for one
    that holds nothing but spaces and tabs, which backticks would show as nothing."""
    return f"`{line}`" if line.strip(" \t") else "a blank line"


def held_character(part: str) -> str:
    """What a refusal adds after quoting `part` of a step's line: the first character of it
    outside printable ASCII and tab (odd_character), or ''."""
    odd = odd_character(part)
    return f", and holds the character U+{ord(odd):04X} there" if odd else ""


def name_list_difference(line: str, pin: str) -> str:
    """What the anchored name lists (NAME_LIST) of a step's line do that its pin's do not, where
    those lists, the same flags in the same order, are all the two lines differ in: per list, each
    test a -run leaves out or names besides the pinned -run, each test a -skip no longer skips or
    skips besides the pinned -skip, or that a list names the pinned one's tests in another order or
    more than once. '' where anything else differs, and where the lists are the pin's but stand
    elsewhere in a line that reads the same without them."""
    lists, pins = NAME_LIST.findall(line), NAME_LIST.findall(pin)
    if (not lists or [flag for flag, _ in lists] != [flag for flag, _ in pins]
            or NAME_LIST.sub("", line) != NAME_LIST.sub("", pin)):
        return ""
    parts = []
    for (flag, got), (_, wanted) in zip(lists, pins):
        got_names, wanted_names = set(got.split("|")), set(wanted.split("|"))
        gone = ", ".join(sorted(wanted_names - got_names))
        extra = ", ".join(sorted(got_names - wanted_names))
        if gone:
            parts.append(f"its -run leaves out {gone}, which the pinned -run names" if flag == "-run"
                         else f"its -skip no longer skips {gone}, which the pinned -skip skips")
        if extra:
            parts.append(f"its -run names {extra}, which the pinned -run does not" if flag == "-run"
                         else f"its -skip also skips {extra}, which the pinned -skip does not")
        if got != wanted and not gone and not extra:
            parts.append(f"its {flag} names the pinned {flag}'s tests in another order or more than once")
    return "; ".join(parts)


def pin_difference(script: str, pinned: str) -> str:
    """'' when a step's run: script is the text its Exact entry pins, compared as the entry
    compares them (after trailing whitespace, nothing parsed). Else the first line that differs, by
    its number in the script, in one of seven forms:
      * `its line N, <the line>, is past the pinned text's last line`: the step's script goes on
        where the pin has ended;
      * `it ends before the pinned text's line N, <that line>`: the pin goes on where the script
        has ended;
      * `on its line N, <what its name lists do>` (name_list_difference), where all that differs
        on the line is its anchored -run or -skip lists of test names;
      * `its line N differs from the pinned text's in spaces or tabs alone`, quoting neither;
      * `its line N is a blank line where the pinned text's reads <its start>`, and
      * `its line N reads <its start> where the pinned text's line is a blank line`, a blank line
        being one of spaces and tabs alone, which backticks would show as nothing, and its start
        the line's first 2 * DIFFERENCE_CONTEXT characters;
      * else `its line N reads <part> where the pinned text's reads <part>`: both lines from the
        first character that differs to the last, DIFFERENCE_CONTEXT characters either side.
    The last two forms go on to name the first character of the step's part they quote outside
    printable ASCII and tab, when it holds one (held_character). A line quoted whole is quoted as
    quoted() quotes it."""
    have, want = script.rstrip().split("\n"), pinned.rstrip().split("\n")
    if have == want:
        return ""
    i = next((i for i, (a, b) in enumerate(zip(have, want)) if a != b), min(len(have), len(want)))
    if i == len(want):
        return f"its line {i + 1}, {quoted(have[i])}, is past the pinned text's last line"
    if i == len(have):
        return f"it ends before the pinned text's line {i + 1}, {quoted(want[i])}"
    line, pin = have[i], want[i]
    lists = name_list_difference(line, pin)
    if lists:
        return f"on its line {i + 1}, {lists}"
    if BASH_BLANKS.split(line.strip(" \t")) == BASH_BLANKS.split(pin.strip(" \t")):
        return f"its line {i + 1} differs from the pinned text's in spaces or tabs alone"
    if not line.strip(" \t"):
        return (f"its line {i + 1} is a blank line where the pinned text's reads "
                f"`{excerpt(pin, 0, DIFFERENCE_CONTEXT)}`")
    if not pin.strip(" \t"):
        return (f"its line {i + 1} reads `{excerpt(line, 0, DIFFERENCE_CONTEXT)}` where the pinned text's line "
                f"is a blank line" + held_character(line[:2 * DIFFERENCE_CONTEXT]))
    start = len(os.path.commonprefix([line, pin]))
    same_end = len(os.path.commonprefix([line[start:][::-1], pin[start:][::-1]]))
    return (f"its line {i + 1} reads `{excerpt(line, start, len(line) - same_end)}` where the pinned text's reads "
            f"`{excerpt(pin, start, len(pin) - same_end)}`" + held_character(line[start:len(line) - same_end]))


def go_lines(text: str) -> list:
    """The lines of a run: script that hold a go command, stripped, comments aside."""
    return [line for line in script_lines(text) if GO_COMMAND.search(line)]


def continued(lines: list, i: int) -> bool:
    return lines[i].rstrip().endswith("\\") or (i > 0 and lines[i - 1].rstrip().endswith("\\"))


# The forms of a go command this check reduces: a leading run of ENV_ALLOWED assignments, then
# `go test|vet|build` or `go -C <directory>` and one of those three, then relative package paths
# and only these flags. Any other claimed command counts as reading every Go file of the
# repository and every file under a Go module's directory.
NOT_SIMPLE = "$'\"`|&;()<>*?\\"
ASSIGNMENT = re.compile(r"([A-Za-z_][A-Za-z0-9_]*)=(\S*)")
PACKAGE = re.compile(r"\.\.?(/[A-Za-z0-9_./-]*)?")
FLAG = re.compile(r"-v|-race|-count=[0-9]+|-timeout=[0-9]+[smh]|-buildvcs=false")
DURATION = re.compile(r"[0-9]+[smh]")
TAGS = re.compile(r"[A-Za-z0-9_,]+")


def reduce_line(line: str) -> tuple:
    """A go line in one of the forms this check reduces as (packages, tags, mode, whether it runs
    with -race, the directory its `go -C` names or "", ""), any other as (None, None, None, None,
    None, the first reason it is not one of them)."""
    unreduced = (None, None, None, None, None)
    special = next((c for c in line if c in NOT_SIMPLE), None)
    if special:
        return *unreduced, f"it has `{special}`"
    words = line.split()
    # A leading NAME=value run is dropped: an ENV_ALLOWED name cannot change what go compiles (the
    # list says why of each), so the command compiles what the bare line does and the closure is
    # the one go list gives under the Run's own env. Any other name - GOFLAGS, GOOS, GOARCH,
    # CGO_ENABLED - chooses the files go compiles, and refuses the line here exactly as it refuses
    # a step's env: in env_reason, naming the name rather than the whole assignment.
    while words and (assignment := ASSIGNMENT.fullmatch(words[0])):
        if assignment.group(1) not in ENV_ALLOWED:
            return *unreduced, f"it sets {assignment.group(1)}, which can change what go compiles"
        words = words[1:]
    if not words or words[0] != "go":
        return *unreduced, f"it starts with `{words[0] if words else line}`, not go"
    # `go -C <directory>` is reduced where a `cd` is not because it names the directory outright,
    # on the line the check already reads: the caller joins it to the step's working directory and
    # holds the result to the Run's, so a command that moves to another module is refused rather
    # than read in the wrong one. Go takes -C only as the first flag, so only the first word here.
    chdir = ""
    if len(words) > 1 and words[1] == "-C":
        if len(words) < 3 or words[2].startswith("-"):
            return *unreduced, "`go -C` names no directory"
        chdir, words = words[2], ["go", *words[3:]]
    if len(words) < 2 or words[1] not in ("test", "vet", "build"):
        return *unreduced, f"`{' '.join(words[:2])}` is not go test, vet or build"
    pkgs, tags, index = set(), None, 2
    while index < len(words):
        word, value = words[index], words[index + 1] if index + 1 < len(words) else ""
        if PACKAGE.fullmatch(word):
            pkgs.add(word)
        elif word == "-timeout" and DURATION.fullmatch(value):
            index += 1
        elif word == "-tags" or word.startswith("-tags="):
            given = value if word == "-tags" else word.removeprefix("-tags=")
            if tags is not None or not TAGS.fullmatch(given):
                return *unreduced, f"`-tags {given}` is not its one plain -tags"
            tags, index = given, index + (word == "-tags")
        elif not FLAG.fullmatch(word):
            return *unreduced, f"`{word}` is neither a relative package path nor a flag this check reduces"
        index += 1
    return pkgs, tag_set(tags or ""), words[1], "-race" in words, chdir, ""


# The only env: names a step, its job or the workflow may set while a go command of the step stays
# reduced, or a step an exact entry pins stays unrefused: names that cannot change what the command
# compiles or which tests it runs. Any other name falls to everything, or refuses the pinned step,
# among them GO* and CGO_*, BASH_ENV and ENV (which bash sources before the script), HOME and
# XDG_CONFIG_HOME (go reads its env file, GOFLAGS among it, from $XDG_CONFIG_HOME/go/env or else
# $HOME/.config/go/env) and PATH (which chooses the go binary). Add a name here only when go
# compiles the same files with it set as without, and runs the same tests.
#
# GOPROXY is the one name go itself reads that is here. It says where a module's bytes come from,
# never which version: the module files this check already reads decide that, and go.sum holds
# each module's hash whichever source serves it. This check's own go commands run with
# GOPROXY=off over an empty module cache (Go.env below), so a workflow's value never reaches
# them either. CI_OK_SELECTED_BY, CI_OK_PULL_REQUEST_ONLY, CI_OK_ALWAYS_RUN and CI_OK_EXEMPT are
# ci.yaml's ci-ok classes: ci-ok-report.sh reads the first three out of that file and hands them
# to ci-ok.sh, and CI_OK_EXEMPT is read by this check alone. LEGION_RUN_FUZZ_ITERS and
# LEGION_KEPT_PROPERTY_RUNS are the counts the pull-request lane reduces: each is read at run
# time by the test it governs, so go compiles the same files with them set as without. So are
# the variables that turn on a test's own input or bound: the corpus directories the pmdoc and
# document-service corpus tests read (PMDOC_ANCHOR_CORPUS, DISPATCH_RENDER_CORPUS,
# PMDOC_WALK_CORPUS, DISPATCH_TOKEN_CORPUS, DISPATCH_TEST_SPEC_CORPUS), the seeded database the
# statement timings measure on (DISPATCH_BENCH_DATABASE_URL), the outbox load steps' switch
# (DISPATCH_OUTBOX_LOAD_STEPS), the intake drain's bound
# (LEGION_TEST_DRAIN_MEASURE), the kind rig's worker image, its archive and the directory a failed
# rig's state goes to (LEGION_TEST_KIND_IMAGE, LEGION_TEST_KIND_IMAGE_ARCHIVE,
# LEGION_TEST_KIND_LOG_DIR, which internal/kindtest reads at run time), and the base commit
# envoy-go's pmdoc differential step hands its script (DIFFERENTIAL_BASE), which no go command reads.
ENV_ALLOWED = frozenset({
    "AIRLOCK_MODEL",
    "BROKER_TEST_DATABASE_URL", "CI_OK_ALWAYS_RUN", "CI_OK_EXEMPT", "CI_OK_PULL_REQUEST_ONLY", "CI_OK_SELECTED_BY",
    "DATABASE_URL", "DIFFERENTIAL_BASE",
    "DISPATCH_BENCH_DATABASE_URL",
    "DISPATCH_E2E_FILE_STORE_BUCKET", "DISPATCH_E2E_FILE_STORE_ENDPOINT", "DISPATCH_FIRST_ROLLOUT_BUILD",
    "DISPATCH_OUTBOX_LOAD_STEPS", "DISPATCH_PREVIOUS_BUILD", "DISPATCH_RENDER_CORPUS", "DISPATCH_TEST_DATABASE_URL",
    "DISPATCH_TEST_SPEC_CORPUS", "DISPATCH_TOKEN_CORPUS",
    "ENVOY_INFRA_DOCKER_PLUGIN", "ENVOY_SMOKE_HELD_PORT_IMAGE",
    "ENVOY_SMOKE_IMAGE", "FIRST_ROLLOUT_COMMIT",
    # The token gh reads. A step of the lint job sets it for the daemon API contract check, and
    # around_job reads the env of every step up to the one that runs go-release-notices.sh, that
    # step among them, so without this name here every go command of that script would count as
    # unreduced and go_notices would have to match every Go file of the repository again. go reads
    # it nowhere.
    "GH_TOKEN",
    "GOPROXY",
    "LEGION_E2E", "LEGION_E2E_DISPATCH_STACK", "LEGION_E2E_ENVOY_STACK", "LEGION_E2E_PG_DSN", "LEGION_KEPT_PROPERTY_RUNS",
    "LEGION_RUN_FUZZ_ITERS", "LEGION_TEST_DAEMON_LOG_DIR", "LEGION_TEST_DRAIN_MEASURE",
    "LEGION_TEST_KIND_IMAGE", "LEGION_TEST_KIND_IMAGE_ARCHIVE", "LEGION_TEST_KIND_LOG_DIR",
    "LEGION_TEST_NATS_URL", "LEGION_TEST_PG_DSN",
    # The commit envoy-go-checks' edited-migrations step compares a bypass run's tree with:
    # .github/scripts/edited-migrations-base.sh, which that step runs, reads it and hands the base
    # it picks to the command as an argument, and go reads it nowhere.
    "MIGRATION_BASE",
    "PMDOC_ANCHOR_CORPUS", "PMDOC_WALK_CORPUS", "PREVIOUS_COMMIT",
    "TESTCONTAINERS_RYUK_DISABLED",
    # The deployment's term list .github/scripts/check-private-names.sh reads. The private-names
    # jobs of ci-job-lint.yaml and docs-site.yaml map it from the secret of that name, and this
    # check reads every step of a called file beside the go commands its other job runs. go reads
    # it nowhere.
    "PRIVATE_NAME_TERMS",
})


def env_reason(document: dict, job: dict, steps: list) -> str:
    """The first env: block of the workflow, the job or one of the steps that can change what a go
    command compiles (an expression, or a name outside ENV_ALLOWED), or ''."""
    blocks = [("the workflow's", document.get("env")), ("the job's", job.get("env"))]
    blocks += [(f'step "{step_label(s)}"\'s', s.get("env")) for s in steps]
    for where, env in blocks:
        if env is not None and not isinstance(env, dict):
            return f"{where} env is an expression"
        for name in env or {}:
            if name not in ENV_ALLOWED:
                return f"{where} env sets {name}"
    return ""


def run_default(holder: dict, key: str):
    """The workflow's or a job's defaults.run.<key>, or None."""
    return ((holder.get("defaults") or {}).get("run") or {}).get(key)


def shell_reason(document: dict, job: dict, steps: list) -> str:
    """What runs the job's run: scripts other than GitHub's default bash does, unseen in the
    script: a container: on the job, a defaults.run.shell of the workflow or the job, or a shell:
    of one of the steps; or ''."""
    if job.get("container") is not None:
        return "the job runs in a container:"
    for where, holder in (("the workflow's", document), ("the job's", job)):
        if run_default(holder, "shell") is not None:
            return f"{where} defaults.run.shell is `{run_default(holder, 'shell')}`"
    for step in steps:
        if step.get("shell") is not None:
            return f'step "{step_label(step)}" runs under shell: `{step["shell"]}`'
    return ""


# A run: step that changes every later step's environment, which a go command reads beyond its line.
ENV_WRITE = re.compile(r"\$\{?GITHUB_(?:ENV|PATH)\b|\bgo\s+env\s+-w\b")


def job_reason(job: dict) -> str:
    """The first step of the job whose run: writes $GITHUB_ENV or $GITHUB_PATH or runs `go env -w`,
    which reach every later go command of the job without showing on its line, or ''. Read line by
    line through script_lines, as a step's go commands are: a `#` line is not a command, so a step
    whose comment names $GITHUB_ENV to say it writes none writes none here either."""
    for step in job.get("steps") or []:
        for line in script_lines(str(step.get("run", ""))):
            written = ENV_WRITE.search(line)
            if written:
                return f'step "{step_label(step)}" runs `{written.group(0)}`, which every later step inherits'
    return ""


def job_up_to(job: dict, step: dict, including: bool = True) -> dict:
    """The job as `step` sees it: its steps before that one, and with `including` that one too. A
    step's write of $GITHUB_ENV or $GITHUB_PATH reaches only the steps after it, so a step later in
    the job moves nothing `step` runs: reading the whole job would refuse a pinned step, or widen a
    claimed command, for a write that comes after it. `step`'s own script is read with it where
    that script is no pin: its `go env -w` reaches its later go commands. A pinned step's own
    script is what its entry pins, so refuse_off_pin reads only the steps before it. The whole job
    where `step` is none of its steps."""
    steps = job.get("steps") or []
    at = next((i for i, s in enumerate(steps) if s is step), None)
    return job if at is None else {**job, "steps": steps[:at + 1] if including else steps[:at]}


# What may come before a script's path where a step starts it: the start of a line, a space, a
# quote, an `=`, a `(` or a backtick; `./` after one of those; or a `/` after a variable
# (`$NAME`), a `}` or a `)`, which end `${NAME}`, `${{ ... }}` and `$(...)`. A `/` after anything
# else ends a directory of another path, so `tools/scripts/e2e-local.sh` is not
# `packages/envoy/scripts/e2e-local.sh` run from `packages/envoy`.
SCRIPT_START = r"(?:^|[\s'\"=(`]|(?:^|[\s'\"=(`])\./|(?:\$[A-Za-z_][A-Za-z0-9_]*|[})])/)"


def names_script(holder: dict, job: dict, step: dict, source: str) -> bool:
    """Whether a run: step names the repository script `source`: by its path from the repository
    root, or from the directory the step runs in, as a whole word of its run: text after one of
    SCRIPT_START's starts - `"$GITHUB_WORKSPACE/<script>"`, `${{ github.workspace }}/<script>`,
    `$(<script> ...)`, a backticked call, `$(git rev-parse --show-toplevel)/<script>` and
    `./<script>` among them. The script's own parent directories before its path from the step's
    directory make its path from the root, which is read as that; any other directory before it
    names another script. Nothing that goes on the path after it (`.bak`, `-old`, a longer name)
    counts."""
    run = str(step.get("run", ""))
    workdir = step_workdir(holder, job, step)
    paths = {source, os.path.relpath(source, workdir)} if workdir != "." else {source}
    return any(re.search(rf"{SCRIPT_START}{re.escape(p)}(?![\w.-])", run) for p in paths)


def around_reason(document: dict, job: dict, steps: list) -> str:
    """The first thing outside a go command's line that can change what it compiles: an env:,
    a step writing the job's environment, a shell or a container; or ''. `steps` are the steps
    that may start it."""
    return env_reason(document, job, steps) or job_reason(job) or shell_reason(document, job, steps)


def step_workdir(document: dict, job: dict, step: dict) -> str:
    """The directory a run: step runs in: its own working-directory, else the job's or the
    workflow's defaults.run.working-directory, else the repository root."""
    given = step.get("working-directory") or run_default(job, "working-directory") or run_default(document, "working-directory") or "."
    return clean_dir(str(given))


def clean_dir(path: str) -> str:
    return path.strip().removeprefix("./").rstrip("/") or "."


def tag_set(tags: str) -> frozenset:
    return frozenset(t for t in tags.split(",") if t)


# A ci.yaml job that calls a local reusable workflow gives it its steps: the go commands, the
# env: blocks, the shells and the directories that decide what the job compiles are in that file,
# not here. So a gate on such a job is read over the jobs of the file it calls, each under that
# file's own env: and defaults (a called workflow inherits neither from its caller).
def step_holders(documents: dict) -> list:
    """Where every ci.yaml job's steps are, as (the ci.yaml job a gate names, the workflow file
    holding the steps, the job holding them there, that file's document, that job): the job
    itself, and, when it calls a local workflow, each job of that workflow. The documents are
    workflow_documents' set, which holds each called file once however many jobs name it, so no
    job of one is listed twice."""
    ci = documents[WORKFLOW]
    units = [(job_id, WORKFLOW, job_id, ci, job or {}) for job_id, job in (ci.get("jobs") or {}).items()]
    for path, caller in called_workflows(ci).items():
        called = documents.get(path) or {}
        units += [(caller, path, name, called, called_job or {})
                  for name, called_job in (called.get("jobs") or {}).items()]
    return units


def where_job(path: str, name: str, owner: str) -> str:
    """How an error names the job a step is in. A job of ci.yaml, and a job of a called file
    carrying its caller's own id (.github/workflows/ci-job-<id>.yaml, which holds that one job's
    steps), is named by that id alone: the two are one job to a reader, and the `::error file=`
    of every refusal here names the file either way. A called file that gives the job an id of
    its own (dispatch-browser.yaml's shard, alone, multiprocess and merge) is named with the
    ci.yaml job that calls it, which a reader would otherwise look for in ci.yaml."""
    return f"job {name}" if path == WORKFLOW or name == owner else f"job {name} of {path}, which job {owner} calls"


def owner_steps(units: list, owner: str) -> list:
    """Every step a ci.yaml job runs: its own, and those of the jobs of a workflow it calls."""
    return [s for own, _, _, _, job in units if own == owner for s in job.get("steps") or []]


def around_job(units: list, owner: str, source: str) -> str:
    """The first thing around a ci.yaml job that can change what a go command its script `source`
    holds compiles, over its own steps and those of a workflow it calls, each read under the env:,
    defaults and container of the file that holds it; or ''. A job some step of which names the
    script (names_script) is read up to and including the last such step, since a step after it
    runs after the script. A job that names it nowhere is read whole, since it may start the
    script some way this check does not see - in a form names_script does not read, by another
    script, or by a test it compiles."""
    for own, _, _, holder, job in units:
        if own != owner:
            continue
        steps = job.get("steps") or []
        naming = [i for i, s in enumerate(steps) if "run" in s and names_script(holder, job, s, source)]
        read = {**job, "steps": steps[:naming[-1] + 1]} if naming else job
        reason = around_reason(holder, read, read.get("steps") or [])
        if reason:
            return reason
    return ""


def exact_entries(names) -> str:
    """How a refusal names the gates whose Exact entries pin one step."""
    names = sorted(set(names))
    return f"gate {names[0]}'s exact entry" if len(names) == 1 else f"the exact entries of gates {', '.join(names)}"


def refuse_off_pin(path: str, where: str, step: dict, holder: dict, job: dict, workdir: str, entries: list,
                   problems: list[Annotation]) -> None:
    """Refuse a run: step its Exact entries pin (`entries`, each (the gate's filter, Exact)) where
    the step is not what they pin: a script or a directory other than an entry's, named by the
    first line that differs (pin_difference) or by both directories, and an env, shell, container
    or step of the job before this one (around_reason over job_up_to) that can change what its go
    commands compile or which tests they run. Its own script's writes are the pin's to hold.

    Refused rather than read as reading everything, which a step off its pin once counted as. The
    pin is also the table's word for which tests the step runs - ci_job_reads.py builds the
    text-check step's -run from TEXT_CHECK_TESTS, the names envoy-go's step of what the race build
    cannot run leaves out - and reading everything says nothing where the filter selects every Go
    file already, as the text_checks filter does: a guard test dropped from that -run ran in no job
    while this check passed, and so did one a `GOFLAGS: -skip=` in the step's env skipped. No pinned
    step runs under such an env, shell or container. Entries of two gates pinning one text make one
    refusal, and the gates' entries of one step one refusal of what surrounds it."""
    label = step_label(step)
    script = str(step["run"])
    pins = defaultdict(set)  # (the script an entry pins, as pin_difference compares it; its directory) -> gates
    for name, e in entries:
        pins[(e.script.rstrip(), clean_dir(e.workdir))].add(name)
    off_pin = False
    for (pinned_script, directory), names in pins.items():
        drift = [d for d in (pin_difference(script, pinned_script),
                             "" if workdir == directory else f"it runs in {workdir}, the entry's in {directory}") if d]
        if drift:
            off_pin = True
            pin = "pins" if len(names) == 1 else "pin"
            problems.append(error(path, f"{where} step \"{label}\" is off the script and directory "
                                        f"{exact_entries(names)} {pin} it to: {'; '.join(drift)}; make the step run "
                                        f"that text there again, or change {'the entry' if len(names) == 1 else 'the entries'}, "
                                        f"and any list of ci_job_reads.py its text is built from, with the step"))
    # A step already refused as off its pin is not refused again for a character its script holds.
    moved = (around_reason(holder, job_up_to(job, step, including=False), [step])
             or ("" if off_pin else script_reason(script)))
    if moved:
        problems.append(error(path, f"{where} step \"{label}\" is pinned whole by {exact_entries(n for n, _ in entries)}, "
                                    f"but {moved}, and that can change what its go commands compile, or which tests "
                                    f"they run, where no pin holds it; remove it, or put what it sets on the step's "
                                    f"own script, where the pin holds it, or, for a name with which go compiles the "
                                    f"same files and runs the same tests, add it to ENV_ALLOWED"))



# ---------------------------------------------------------------- the check
def check_table(documents: dict, data, filters: dict, files: set, problems: list[Annotation]) -> dict:
    """The table must say what ci.yaml says: every gate a real filter on a real job, every go
    command a job's run: steps start listed, every pin and citation still found. `documents` is
    ci.yaml and every workflow its jobs call (workflow_documents), since a gate's job may hold
    its steps in a called file: the caller job in ci.yaml keeps the id and the `if:`, so the
    filter test is read off ci.yaml, and the steps are read off that file, which step_holders
    pairs back with the ci.yaml job that runs them. Returns, per
    filter, each claimed command it does not reduce: (Run, the command as the job runs it, why)."""
    caller_jobs = documents[WORKFLOW].get("jobs") or {}
    gates = {g.filter: g for g in data.GATES}
    excluded = dict(data.EXCLUDED)
    for name in sorted(set(gates) & set(excluded)):
        problems.append(error(".github/scripts/ci_job_reads.py", f"{name} is both a gate and EXCLUDED"))
    # What each called file's conditions gate on, which is the caller's to say: a condition of
    # ci.yaml names its filter outright, one of a called file names an input the caller passes
    # that filter's output in.
    inputs_of = input_expressions(documents, problems)
    # Rule 3 over every document of the set: a called file's job or step is gated through an
    # input of its own, resolved to the caller's expression here, and the refusal names the file
    # the condition is in.
    gated = defaultdict(set)  # (workflow, job) -> filters its job-level if: names
    for path, document in documents.items():
        for job_id, job in (document.get("jobs") or {}).items():
            for step, condition in conditions(job or {}):
                for name in GATE_REF.findall(resolve_condition(condition, inputs_of.get(path, {}))):
                    if step is None:
                        gated[(path, job_id)].add(name)
                    if name not in gates and name not in excluded:
                        where = where_if(job_id, step)
                        problems.append(error(path, f"{where} is gated on filter {name}, which ci_job_reads.py "
                                                    f"neither lists in GATES nor EXCLUDES; add the gate with its reads"))
    for g in data.GATES:
        if g.filter not in filters:
            problems.append(error(WORKFLOW, f"{g.job} has no filter {g.filter} in the changes job"))
        # The caller job is ci.yaml's, whichever file holds the steps: it keeps the job id and
        # the `if:` that tests the filter, so both are looked for there.
        if g.job not in caller_jobs:
            problems.append(error(".github/scripts/ci_job_reads.py", f"gate {g.filter} names job {g.job}, "
                                                                      f"which {WORKFLOW} does not have"))
            continue
        steps_document = documents.get(g.workflow)
        if steps_document is None:
            problems.append(error(".github/scripts/ci_job_reads.py", f"gate {g.filter} holds job {g.job}'s "
                                                                      f"steps in {g.workflow}, which {WORKFLOW} "
                                                                      f"neither is nor calls"))
            continue
        job = (steps_document.get("jobs") or {}).get(g.job)
        if job is None:
            problems.append(error(".github/scripts/ci_job_reads.py", f"gate {g.filter} names job {g.job}, "
                                                                      f"which {g.workflow} does not have"))
            continue
        if g.step:
            steps = [s for s in job.get("steps") or [] if step_matches(s, g.step)]
            if len(steps) != 1 or g.filter not in GATE_REF.findall(
                    resolve_condition(str(steps[0].get("if", "")), inputs_of.get(g.workflow, {}))):
                problems.append(error(g.workflow, f"gate {g.filter} names step \"{g.step}\" of {g.job}, but "
                                                  f"no single step of that name has an if: on "
                                                  f"needs.changes.outputs.{g.filter}"))
        elif g.filter not in gated[(WORKFLOW, g.job)]:
            problems.append(error(WORKFLOW, f"job {g.job}'s if: does not test needs.changes.outputs.{g.filter}"))
    for name in excluded:
        if name not in filters:
            problems.append(error(".github/scripts/ci_job_reads.py", f"EXCLUDED names filter {name}, which "
                                                                      f"the changes job does not have"))

    # Every go command line of a run: step must be claimed: by a pinned Run of a gate on that job,
    # or by an Exact entry pinning the step's whole script. A line claimed by a Run is reduced only
    # in its one simple form, the whole script of its step, under an env, job, shell and directory
    # that cannot move it; its packages, -tags, -race and mode must then be exactly the Run's. Any
    # other claimed command is unreduced and falls to everything a go command can read. A step an
    # Exact entry pins is refused instead when its script or directory is not the entry's, naming
    # the first line that differs or both directories, and when its env, shell, container or job
    # could move what it compiles, naming what does.
    # Each index is keyed by the ci.yaml job the gate names, which is the job that runs the
    # steps wherever they are: a job that calls a reusable workflow keeps its id, its
    # `needs: changes` and its whole `if:` in ci.yaml and holds its steps in the called file,
    # whose own job ids are its to choose. The caller job has no run: step of its own, so the
    # two never answer for each other.
    skipped = {key for key, names in gated.items() if names and names <= set(excluded)}
    # A called file's jobs run inside the caller job's run and under its `if:`, so a caller whose
    # gate is EXCLUDED hands its skip to them: the go commands of those steps are the other
    # check's too, exactly as the caller's own would be.
    for path, caller in called_workflows(documents[WORKFLOW]).items():
        if (WORKFLOW, caller) in skipped:
            skipped |= {(path, job_id) for job_id in (documents.get(path) or {}).get("jobs") or {}}
    claims = defaultdict(list)  # (the ci.yaml job, step label) -> pinned texts
    exacts = defaultdict(list)  # (the ci.yaml job, step label) -> (the gate's filter, Exact)
    owner = {}  # id(Run) -> the gate's filter
    unreduced = defaultdict(list)  # filter -> (Run, the command as the job runs it, why it is not reduced)
    for g in data.GATES:
        for r in g.runs:
            if not (r.step or r.source) or not r.text:
                problems.append(error(".github/scripts/ci_job_reads.py", f"gate {g.filter}'s run "
                                                                          f"{r.workdir} {' '.join(r.pkgs)} is pinned to no step or source text"))
            if r.step:
                claims[(g.job, r.step)].append(r)
                owner[id(r)] = g.filter
        for e in g.exact:
            exacts[(g.job, e.step)].append((g.filter, e))
    units = step_holders(documents)
    for own, path, name_there, holder, job in units:
        if (path, name_there) in skipped:
            continue
        for step in job.get("steps") or []:
            if "run" not in step:
                continue
            script = str(step["run"])
            workdir = step_workdir(holder, job, step)
            entries = [fe for (j, ref), fes in exacts.items() if j == own and step_matches(step, ref) for fe in fes]
            if entries:
                refuse_off_pin(path, where_job(path, name_there, own), step, holder, job, workdir, entries, problems)
                continue  # every go line of the step is claimed by its Exact entries
            around = around_reason(holder, job_up_to(job, step), [step]) or script_reason(script)
            mine = [r for (j, ref), rs in claims.items() if j == own and step_matches(step, ref) for r in rs]
            for line in go_lines(script):
                where = f"{where_job(path, name_there, own)} step \"{step_label(step)}\" runs `{line}`"
                pinned = [r for r in mine if squash(r.text) in squash(line)]
                if not pinned:
                    problems.append(error(path, f"{where}, which no Run in ci_job_reads.py lists; add it "
                                                 f"to the job's gate so its closure is checked"))
                    continue
                reason = around or ("" if script_lines(script) == [line] else
                                    "it is not its step's whole run: script, comments and blank lines aside")
                named, tags, mode, race, chdir, reason = (reduce_line(line) if not reason else
                                                          (None, None, None, None, None, reason))
                # The directory the command itself runs in: the step's, moved by its `go -C`.
                here = clean_dir(os.path.normpath(os.path.join(workdir, chdir))) if chdir else workdir
                for r in pinned:
                    if here != clean_dir(r.workdir):
                        unreduced[owner[id(r)]].append((r, where, reason or f"its step runs in {here}, the Run in {r.workdir}"))
                        continue
                    if reason:
                        unreduced[owner[id(r)]].append((r, where, reason))
                        continue
                    parts = []
                    extra = sorted(named - set(r.pkgs))
                    missing = sorted(set(r.pkgs) - named)
                    if extra:
                        parts.append(f"it also names {' '.join(extra)}")
                    if missing:
                        parts.append(f"it no longer names {' '.join(missing)}")
                    if tags != tag_set(r.tags):
                        parts.append(f"it builds with -tags {','.join(sorted(tags)) or '(none)'}, the Run with "
                                     f"{','.join(sorted(tag_set(r.tags))) or '(none)'}")
                    if race != r.race:
                        parts.append("it runs with -race, the Run without" if race else "it runs without -race, the Run with it")
                    if mode != r.mode:
                        parts.append(f"its go command is a {mode} run, the Run a {r.mode} run")
                    if parts:
                        problems.append(error(path, f"{where}, which is not the Run's "
                                                    f"({' '.join(r.pkgs)}): {'; '.join(parts)}; make the Run in ci_job_reads.py "
                                                    f"name exactly the packages, tags, -race and mode the line does, so the closure "
                                                    f"checked against the job's filter is the one it compiles"))
    for (job_id, ref), fes in exacts.items():
        steps = [s for s in owner_steps(units, job_id) if "run" in s and step_matches(s, ref)]
        if len(steps) != 1:
            for name, _ in fes:
                problems.append(error(".github/scripts/ci_job_reads.py", f"gate {name}'s exact entry names step "
                                                                          f"\"{ref}\" of job {job_id}, which is not one run: step of it; update or remove the entry"))
    for (job_id, ref), rs in claims.items():
        steps = [s for s in owner_steps(units, job_id) if step_matches(s, ref)]
        text = squash(" ".join(str(s.get("run", "")) for s in steps))
        for r in rs:
            if not steps or squash(r.text) not in text:
                problems.append(error(".github/scripts/ci_job_reads.py", f"job {job_id} step \"{ref}\" no longer "
                                                                          f"runs `{r.text}`; update or remove the Run"))
    # A script's go command is held exact rather than reduced: the line holding the pinned text is
    # that text alone, uncontinued, and no env:, shell: or defaults.run.shell of the workflow, the
    # job or its steps, no container: and no environment write of the job can move it - each job's
    # steps up to the last one naming the script, or all of them where it names the script nowhere
    # (around_job), since the check does not otherwise know which step starts it. A job that calls
    # a workflow runs the script from that workflow's jobs, so each of them is read the same way.
    # Its variables are the Run author's.
    for g in data.GATES:
        for r in g.runs:
            if not r.source:
                continue
            if r.source not in files:
                problems.append(error(r.source, f"{r.source} no longer holds `{r.text}`, the go command "
                                                f"gate {g.filter} lists; update or remove the Run"))
                continue
            source_text = read_repository_text(REPO / r.source)
            if squash(r.text) not in squash(source_text):
                problems.append(error(r.source, f"{r.source} no longer holds `{r.text}`, the go command "
                                                f"gate {g.filter} lists; update or remove the Run"))
                continue
            lines = source_text.split("\n")
            held = [i for i, line in enumerate(lines) if r.text in line]
            reason = around_job(units, g.job, r.source) or ("" if held else "its text spans lines")
            for i in held:
                line = lines[i].strip(" \t")
                if not reason and line != r.text:
                    reason = f"its line is `{line}`, not the text alone"
                elif not reason and continued(lines, i):
                    reason = "it is part of a backslash continuation"
            if reason:
                unreduced[g.filter].append((r, f"{r.source}, which job {g.job} runs, runs `{r.text}`", reason))
    for source in data.RULES:
        if source not in files:
            problems.append(error(".github/scripts/ci_job_reads.py", f"RULES names {source}, which is not "
                                                                      f"in the repository; update or remove the rule"))
    # A citation names the workflow file that holds the step: ci.yaml, or a workflow one of its
    # jobs calls. The step must still be there, so a rename or a move fails here. The set already
    # holds every workflow ci.yaml calls, so only a citation naming another file is read here.
    cited_documents = dict(documents)
    for path, job_id, ref in CITED:
        if path not in cited_documents:
            cited_documents[path] = load_repository_yaml(REPO / path) or {} if (REPO / path).is_file() else None
        cited = cited_documents[path]
        if cited is None:
            problems.append(error(".github/scripts/ci_job_reads.py", f"a why cites step \"{ref}\" of job "
                                                                      f"{job_id} in {path}, which is not in the repository"))
            continue
        if not any(step_matches(s, ref) for s in ((cited.get("jobs") or {}).get(job_id) or {}).get("steps") or []):
            problems.append(error(".github/scripts/ci_job_reads.py", f"a why cites step \"{ref}\" of job "
                                                                      f"{job_id}, which {path} does not have"))
    return unreduced

# ci.yaml's env entry naming the jobs that run on every change: no `changes` output gates one, so
# CI_OK_SELECTED_BY maps none of them, and ci-ok.sh holds each to success — a skipped, cancelled
# or absent one fails the verdict. ci-ok.sh reads the list rather than knowing `changes` and
# `lint` by name, so a third such job is declared here and judged like them.
ALWAYS_RUN_LIST = "CI_OK_ALWAYS_RUN"
# ci.yaml's env entry naming the jobs the merge queue never runs.
PULL_REQUEST_ONLY_LIST = "CI_OK_PULL_REQUEST_ONLY"
# ci-ok.sh's lists of CI_OK_SELECTED_BY jobs whose if: admits only some events, as ci.yaml's env
# names them: per list, the if: term that puts a job on it, what that term admits, and the events
# where ci-ok.sh accepts its skip.
EVENT_LISTS = {
    PULL_REQUEST_ONLY_LIST: (re.compile(r"github\.event_name\s*==\s*'pull_request'"),
                             "admits only pull_request", "github.event_name == 'pull_request'", "every other event"),
}
# The step every job but `changes` and the exempt ones ends with (.github/actions/ci-ok-report):
# it posts the failing ci-ok of a merge-group run its own job's result decides, so its inputs are
# fixed.
REPORT_ACTION = "./.github/actions/ci-ok-report"
REPORT_CHANGES = "${{ toJSON(needs.changes) }}"
REPORT_STATUS = "${{ job.status }}"
# The checks main's ruleset requires, each carried by the ci.yaml job of that name: a job's own
# check run is in its run's own check suite, where one POSTed with POST /repos/.../check-runs
# joins the Actions App's first suite at that commit. ci-ok is the verdict over every other job,
# pr-title runs check-pr-text.sh on a pull request and a merge group. No other job of ci.yaml may
# carry one of these names, and neither of the two may be renamed by a `name:`, since a job posts
# the check its display name gives and a skipped job posts a passing one.
REQUIRED_CHECKS = ("ci-ok", "pr-title")
VERDICT_JOB = "ci-ok"
# ci.yaml's env entry naming the jobs outside ci-ok's accounting: the job that exists to stop a
# doomed merge-group run, which runs only on merge_group, holds `actions: write`, runs no
# dependency code, reports no result to ci-ok and is no job a verdict waits for, VERDICT_JOB,
# which reports no verdict to itself and judges the jobs its needs: names, and the other
# REQUIRED_CHECKS job, whose own check run is a required check of its own: the queue reads it
# rather than the verdict, so it carries no report step and the verdict waits for nothing of it.
EXEMPT_LIST = "CI_OK_EXEMPT"
# ENV_ALLOWED above and ci-ok-report.sh's yq read write the same keys again.


def check_ci_ok(documents: dict, problems: list[Annotation]) -> None:
    """ci-ok.sh fails a job CI_OK_SELECTED_BY says `changes` selected unless the job succeeded, and
    ci-ok-report.sh reads every selected job's result for the verdict it posts, so CI_OK_SELECTED_BY
    must be exactly
    {job: the output its if: tests} for every job whose if: tests an output of changes, and each
    EVENT_LISTS list exactly the jobs whose if: holds its term. Every job but `changes` must end
    with the report step, or a merge-group run that job's failure decides posts no ci-ok until
    the verdict job has waited for every other job, and a job that calls
    a workflow must hand that workflow `ci-ok-changes` for its job's report step. Each
    REQUIRED_CHECKS name is a job of this file and no other job carries it, and VERDICT_JOB must
    need every job of the file but itself and
    the exempt ones: a job outside its needs: ends after the verdict that says nothing about it.
    A job EXEMPT_LIST names is outside all of that: it is no job of
    CI_OK_SELECTED_BY, it needs no report step, and it must carry none, since ci-ok.sh fails a
    result for a job CI_OK_SELECTED_BY cannot map. Every job of the file is in exactly one of
    ci-ok's three classes — CI_OK_SELECTED_BY maps it, ALWAYS_RUN_LIST names it, or EXEMPT_LIST
    does: a job in none is one ci-ok.sh fails on every run whatever the job did, and a job in two
    is read by whichever class ci-ok.sh reaches first. A job ALWAYS_RUN_LIST names is gated by no
    output of changes, since ci-ok.sh holds it to success on a run that rightly skipped it.

    It judges ci.yaml's jobs alone, and no workflow ci.yaml calls: the three class lists are
    ci.yaml's env, the jobs they must account for are ci.yaml's jobs, and a job that gave its
    steps to a called file keeps its caller here, which is the job ci-ok's verdict needs and the
    one the report step of the called file names. What that called file owes is checked from here
    all the same, through the caller's `uses:`, read out of the document set main() loaded: the
    report step below."""
    document = documents[WORKFLOW]
    jobs = document.get("jobs") or {}
    env = document.get("env") or {}
    try:
        selected_by = json.loads(str(env.get("CI_OK_SELECTED_BY", "")))
        exempt = json.loads(str(env.get(EXEMPT_LIST, "[]")) or "[]")
        always_run = json.loads(str(env.get(ALWAYS_RUN_LIST, "")))
        listed = {name: json.loads(str(env.get(name, ""))) for name in EVENT_LISTS}
    except json.JSONDecodeError as json_error:
        problems.append(error(WORKFLOW, f"the env's CI_OK_SELECTED_BY, {EXEMPT_LIST}, {ALWAYS_RUN_LIST} "
                                        f"or {' or '.join(EVENT_LISTS)} is missing or not JSON: {json_error}"))
        return
    if not (isinstance(selected_by, dict) and all(isinstance(v, str) for v in selected_by.values())):
        problems.append(error(WORKFLOW, "the env's CI_OK_SELECTED_BY is not a JSON object of job to "
                                        "output name"))
        return
    if not (isinstance(exempt, list) and all(isinstance(j, str) for j in exempt)):
        problems.append(error(WORKFLOW, f"the env's {EXEMPT_LIST} is not a JSON array of job names"))
        return
    if not (isinstance(always_run, list) and all(isinstance(j, str) for j in always_run)):
        problems.append(error(WORKFLOW, f"the env's {ALWAYS_RUN_LIST} is not a JSON array of job names"))
        return
    for name, jobs_listed in listed.items():
        if not (isinstance(jobs_listed, list) and all(isinstance(j, str) for j in jobs_listed)):
            problems.append(error(WORKFLOW, f"the env's {name} is not a JSON array of job names"))
            return
    for job_id in sorted(set(exempt) - set(jobs)):
        problems.append(error(WORKFLOW, f"{EXEMPT_LIST} names {job_id}, which is no job of this file; "
                                        f"drop it, or name the job it was meant for"))
    for job_id in sorted(set(always_run) - set(jobs)):
        problems.append(error(WORKFLOW, f"{ALWAYS_RUN_LIST} names {job_id}, which is no job of this file; "
                                        f"drop it, or name the job it was meant for"))
    remedies = (f"map it in CI_OK_SELECTED_BY to the changes output its if: tests, name it in {ALWAYS_RUN_LIST} "
                f"when it runs on every change and ci-ok must hold it to success, or name it in {EXEMPT_LIST} "
                f"when it is outside ci-ok's accounting and out of job {VERDICT_JOB}'s needs:")
    for job_id in sorted(jobs):
        held = [name for name, yes in (("CI_OK_SELECTED_BY", job_id in selected_by),
                                       (ALWAYS_RUN_LIST, job_id in always_run),
                                       (EXEMPT_LIST, job_id in exempt)) if yes]
        if len(held) == 1:
            continue
        if not held:
            problems.append(error(WORKFLOW, f"job {job_id} is in none of ci-ok's three classes, so "
                                            f"ci-ok.sh fails its result whatever the job did: {remedies}"))
        else:
            problems.append(error(WORKFLOW, f"job {job_id} is in {len(held)} of ci-ok's three classes "
                                            f"({', '.join(held)}), which judge it differently, and it belongs in exactly one: "
                                            f"{remedies}"))
    for job_id in sorted(set(always_run) & set(jobs)):
        gate = GATE_REF.search(str((jobs.get(job_id) or {}).get("if", "")))
        if gate is not None:
            problems.append(error(WORKFLOW, f"{ALWAYS_RUN_LIST} names {job_id}, whose if: tests "
                                            f"needs.changes.outputs.{gate.group(1)}, so changes gates it rather than selecting "
                                            f"every run of it, and ci-ok.sh would fail every run that rightly skipped it; map it "
                                            f"in CI_OK_SELECTED_BY instead"))
    for job_id, job in jobs.items():
        named = str((job or {}).get("name", job_id))
        if job_id in REQUIRED_CHECKS:
            if named != job_id:
                problems.append(error(WORKFLOW, f"job {job_id} carries `name: {named}`, so its check run "
                                                f"is called {named} and {job_id}, a check main's ruleset requires, is reported by "
                                                f"nothing; drop the name:"))
            continue
        if named in REQUIRED_CHECKS:
            problems.append(error(WORKFLOW, f"job {job_id} is named {named}, a check main's ruleset "
                                            f"requires, and is not the {named} job: a job posts its own check, and a skipped one "
                                            f"posts a pass, so the job of that name is the only one that may carry it"))
    for check in REQUIRED_CHECKS:
        if check != VERDICT_JOB and check not in jobs:
            problems.append(error(WORKFLOW, f"this file has no {check} job, a check main's ruleset "
                                            f"requires: its own check run is that check on a pull request and on a merge group, "
                                            f"and the queue cannot merge a group nothing reports it on"))
    verdict = jobs.get(VERDICT_JOB)
    if verdict is None:
        problems.append(error(WORKFLOW, f"this file has no {VERDICT_JOB} job, the check main's ruleset "
                                        f"requires: it needs every other job, fails when ci-ok.sh's verdict over their results "
                                        f"is failure, and so carries that verdict in this run's own check suite, where a posted "
                                        f"check run joins the first suite at the commit"))
    else:
        needed = verdict.get("needs") or []
        needed = [needed] if isinstance(needed, str) else [str(job_id) for job_id in needed]
        want = set(jobs) - {VERDICT_JOB} - set(exempt)
        if VERDICT_JOB not in exempt:
            problems.append(error(WORKFLOW, f"{EXEMPT_LIST} does not name {VERDICT_JOB}, which reports no "
                                            f"verdict to itself: every other job ends with the report step and is judged through "
                                            f"this one's needs:, so add it there"))
        for job_id in sorted(want - set(needed)):
            problems.append(error(WORKFLOW, f"job {VERDICT_JOB} does not need {job_id}, so it reaches the "
                                            f"verdict while that job is still running and says nothing about it; add {job_id} to "
                                            f"its needs:"))
        for job_id in sorted(set(needed) - want):
            why = (f"{EXEMPT_LIST} holds it outside ci-ok's accounting, and ci-ok.sh fails a result for a job "
                   f"CI_OK_SELECTED_BY does not map" if job_id in exempt else "it is no job of this file")
            problems.append(error(WORKFLOW, f"job {VERDICT_JOB} needs {job_id}, but {why}; drop it from "
                                            f"its needs:"))
    gated = sorted(job_id for job_id, job in jobs.items()
                   if job_id not in always_run and job_id not in exempt
                   and GATE_REF.search(str((job or {}).get("if", ""))))
    expected, list_expected = {}, {name: set() for name in EVENT_LISTS}
    for job_id in gated:
        condition = str((jobs.get(job_id) or {}).get("if", ""))
        outputs = sorted(set(GATE_REF.findall(condition)))
        if len(outputs) != 1:
            problems.append(error(WORKFLOW, f"job {job_id}'s if: tests {len(outputs)} outputs of changes "
                                            f"({', '.join(outputs)}), so CI_OK_SELECTED_BY cannot say what selects it"))
            continue
        expected[job_id] = outputs[0]
        for name, (term, _, _, _) in EVENT_LISTS.items():
            if term.search(condition):
                list_expected[name].add(job_id)
    for job_id in sorted(set(exempt) & set(selected_by)):
        problems.append(error(WORKFLOW, f"CI_OK_SELECTED_BY maps {job_id}, which {EXEMPT_LIST} holds "
                                        f"outside ci-ok's accounting, so job {VERDICT_JOB} never needs it and ci-ok.sh fails on a "
                                        f"job its map names and NEEDS lacks; drop it from one of the two"))
    for job_id in sorted((set(gated) | set(selected_by)) - set(exempt)):
        want, have = expected.get(job_id), selected_by.get(job_id)
        if job_id not in gated:
            problems.append(error(WORKFLOW, f"CI_OK_SELECTED_BY maps {job_id}, which is no job whose if: "
                                            f"tests an output of changes; drop it"))
        elif want is None or want == have:
            continue
        elif have is None:
            problems.append(error(WORKFLOW, f"CI_OK_SELECTED_BY does not map {job_id}, whose if: tests "
                                            f"needs.changes.outputs.{want}, so ci-ok.sh does not know what selects it and fails "
                                            f"its result; add \"{job_id}\": \"{want}\""))
        else:
            problems.append(error(WORKFLOW, f"CI_OK_SELECTED_BY maps {job_id} to {have}, but its if: tests "
                                            f"needs.changes.outputs.{want}"))
    for name, (_, admits, text, where) in EVENT_LISTS.items():
        for job_id in sorted(list_expected[name] - set(listed[name])):
            problems.append(error(WORKFLOW, f"{name} lacks {job_id}, whose if: {admits}, so ci-ok.sh "
                                            f"fails its skip on {where}"))
        for job_id in sorted(set(listed[name]) - list_expected[name]):
            problems.append(error(WORKFLOW, f"{name} lists {job_id}, whose if: has no {text} term, so "
                                            f"ci-ok.sh accepts its skip on {where} even when changes selected it"))
    for job_id, job in jobs.items():
        job = job or {}
        if job_id == "changes":
            continue
        if job_id in exempt:
            # An exempt job must report nothing: ci-ok.sh fails a result it cannot map to a
            # CI_OK_SELECTED_BY entry, and the job has none.
            if (str((job.get("with") or {}).get("ci-ok-changes", "")) != ""
                    or any((step or {}).get("uses") == REPORT_ACTION for step in job.get("steps") or [])):
                problems.append(error(WORKFLOW, f"job {job_id} is in {EXEMPT_LIST}, yet it reports to "
                                                f"ci-ok, which fails a result for a job CI_OK_SELECTED_BY does not map; drop the "
                                                f"{REPORT_ACTION} step (and any `ci-ok-changes`), or drop {job_id} from "
                                                f"{EXEMPT_LIST}"))
            continue
        if "uses" in job:
            if str((job.get("with") or {}).get("ci-ok-changes", "")) != REPORT_CHANGES:
                problems.append(error(WORKFLOW, f"job {job_id} calls {job['uses']} without "
                                                f"`ci-ok-changes: {REPORT_CHANGES}`, so the called job cannot report to ci-ok"))
            # The called file is the set's: main() parsed every workflow ci.yaml calls, and a
            # `uses:` the set does not hold is either a workflow of another repository or a path
            # this repository has not got, which workflow_documents has already refused.
            called = documents.get(str(job["uses"]).removeprefix("./")) or {}
            called_jobs = called.get("jobs") or {}
            # The job that reports is the called workflow's job whose last step is the report:
            # always() first, so it runs after a failed step too, and ended only by the call's own
            # condition (docs-site.yaml's input, worker-image.yaml's event).
            reports = [((j or {}).get("steps") or [])[-1] for j in called_jobs.values() if (j or {}).get("steps")]
            if not any(s.get("uses") == REPORT_ACTION and str(s.get("if", "")).startswith("always()")
                       and str((s.get("with") or {}).get("job", "")) == job_id
                       and str((s.get("with") or {}).get("changes", "")) == "${{ inputs.ci-ok-changes }}"
                       and str((s.get("with") or {}).get("job-status", "")) == REPORT_STATUS for s in reports):
                problems.append(error(WORKFLOW, f"{job['uses']}, which job {job_id} calls, has no job "
                                                f"whose last step is `uses: {REPORT_ACTION}` with an `if:` that starts with "
                                                f"`always()`, `job: {job_id}`, `changes: ${{{{ inputs.ci-ok-changes }}}}` and "
                                                f"`job-status: {REPORT_STATUS}`, so a merge-group run {job_id}'s own result "
                                                f"decides gets no failing ci-ok until the {VERDICT_JOB} job has waited for every "
                                                f"other job of the run"))
            continue
        steps = job.get("steps") or []
        last = steps[-1] if steps else {}
        given = last.get("with") or {}
        if (last.get("uses") != REPORT_ACTION or str(last.get("if", "")) != "always()"
                or str(given.get("changes", "")) != REPORT_CHANGES or str(given.get("job-status", "")) != REPORT_STATUS):
            problems.append(error(WORKFLOW, f"job {job_id}'s last step is not `uses: {REPORT_ACTION}` with "
                                            f"`if: always()`, `changes: {REPORT_CHANGES}` and `job-status: {REPORT_STATUS}`, so a "
                                            f"merge-group run {job_id}'s own result decides gets no failing ci-ok until the "
                                            f"{VERDICT_JOB} job has waited for every other job of the run"))


def check_push_workflows(data, filters: dict, problems: list[Annotation]) -> list:
    """A PUSH_WORKFLOWS workflow does on a push to main what its filter's job does on a pull request
    and a merge group, so its on.push.paths must name exactly the filter's patterns: a pattern only
    the filter has lets a change pass the pull request's build and never reach main's, and one only
    the paths have runs main's for a change no pull request built. Returns the report lines."""
    report = []
    for name, path in data.PUSH_WORKFLOWS.items():
        if name not in filters:
            problems.append(error(".github/scripts/ci_job_reads.py", f"PUSH_WORKFLOWS names filter {name}, which "
                                                                      f"the changes job does not have"))
            continue
        if not (REPO / path).is_file():
            problems.append(error(".github/scripts/ci_job_reads.py", f"PUSH_WORKFLOWS names {path} for filter "
                                                                      f"{name}, which is not in the repository"))
            continue
        push = events(load_repository_yaml(REPO / path) or {}).get("push")
        paths = push.get("paths") if isinstance(push, dict) else None
        if not (isinstance(paths, list) and paths and all(isinstance(p, str) for p in paths)):
            problems.append(error(path, f"{path} has no on.push.paths list, which must equal {FILTERS_FILE}'s "
                                        f"filter {name}"))
            continue
        missing = [p for p in dict.fromkeys(filters[name]) if p not in paths]
        extra = [p for p in dict.fromkeys(paths) if p not in filters[name]]
        for p in missing:
            problems.append(error(path, f"{path}'s on.push.paths lacks {p}, which {FILTERS_FILE}'s filter {name} "
                                        f"has, so a push to main that changes only a file it matches never runs {path}"))
        for p in extra:
            problems.append(error(path, f"{path}'s on.push.paths has {p}, which {FILTERS_FILE}'s filter {name} "
                                        f"lacks; add it to the filter or drop it here"))
        verdict = "differs from" if missing or extra else "equals"
        report.append(f"{path}: on.push.paths {verdict} filter {name} ({len(set(paths))} patterns)")
    return report


# A line of a run: script that runs `go test` passing both -tags and -run: it runs only the tests
# its tag adds to the package, so another job must run that package's untagged tests. A `go vet`
# or `go build` line is no such step, and `-runtime` or `-tagsomething` is not the flag.
TAG_RUN_TEST = re.compile(r"\bgo\s+(?:-C\s+\S+\s+)?test\b")
TAG_RUN_FLAGS = (re.compile(r"(?<![\w-])-tags\b"), re.compile(r"(?<![\w-])-run\b"))


def tag_run_steps(documents: dict) -> list:
    """Every run: step of ci.yaml and the workflows it calls whose script holds a line running
    `go test` with both -tags and
    -run, as (the workflow file, the job, the step). Read with script_lines, so a comment is no
    trigger and both
    flags must be on the go test line itself, as every such step of ci.yaml writes them."""
    found = []
    for path, document in documents.items():
        for name, job in (document.get("jobs") or {}).items():
            for step in (job or {}).get("steps") or []:
                if any(TAG_RUN_TEST.search(line) and all(flag.search(line) for flag in TAG_RUN_FLAGS)
                       for line in script_lines(str(step.get("run", "")))):
                    found.append((path, name, step))
    return found


def check_tag_runs(documents: dict, data, filters: dict, quantifiers: dict, gate_files: dict,
                   selection: Selection, listed: dict, expander: Expander, problems: list[Annotation]) -> list:
    """A step that runs a package under a build tag and passes `-run` runs only the tests the tag
    adds, so the package's untagged tests must run elsewhere in every run that step runs. The job
    that runs them is declared (TAG_RUNS), and its filter must cover every file the tag step reads
    (tag_step_files) and every file of the tag step's filter's listed block (tag_listed_files,
    given `listed`, the listed blocks listed_owners accepts); otherwise a change to a file only the
    tag step reads runs the tagged tests alone and the untagged ones nowhere. The table is no
    entry's to opt into: a `go test`
    step of ci.yaml or of a workflow it calls passing both -tags and -run that no entry names is
    a failure of its own, and
    so is a stale entry - one naming a filter the changes job does not have, a job its own
    workflow does
    not have, a step that job does not have, or a step whose run: holds no -run. The tagged job
    and its step are looked for in the file the entry's `workflow` names; the job that runs the
    untagged tests is looked for in every document of the set, since it may hold its steps in a
    called file of its own. Returns the
    report lines."""
    report = []
    # Every job id the set holds, in any of its documents: what the untagged job is held to.
    every_job = {job_id for document in documents.values() for job_id in (document.get("jobs") or {})}
    for entry in data.TAG_RUNS:
        where = f'{entry.job} step "{entry.step}"'
        gone_filters = [name for name in (entry.filter, entry.untagged_filter) if name not in filters]
        for name in gone_filters:
            problems.append(error(".github/scripts/ci_job_reads.py", f"TAG_RUNS names filter {name} for {where}, "
                                                                      f"which the changes job does not have"))
        tagged_jobs = (documents.get(entry.workflow) or {}).get("jobs") or {}
        # The entry's own job is held to the file the entry names, which is where its step is;
        # the untagged job only has to be a job of the set, since a job that gave its steps to a
        # called file keeps its caller in ci.yaml, and one the set does not hold at all is no
        # job of ci.yaml either, which is the file that refusal names.
        gone_jobs = [(entry.job, entry.workflow)] if entry.job not in tagged_jobs else []
        if entry.untagged_job not in every_job:
            gone_jobs.append((entry.untagged_job, WORKFLOW))
        for name, found_in in gone_jobs:
            problems.append(error(".github/scripts/ci_job_reads.py", f"TAG_RUNS names job {name}, which "
                                                                      f"{found_in} does not have"))
        if gone_filters or gone_jobs:
            continue
        job = tagged_jobs[entry.job] or {}
        steps = [s for s in job.get("steps") or [] if "run" in s and step_matches(s, entry.step)]
        if len(steps) != 1:
            problems.append(error(".github/scripts/ci_job_reads.py", f"TAG_RUNS names step \"{entry.step}\" of "
                                                                      f"job {entry.job}, which is not one run: step of it; update or remove the entry"))
            continue
        if "-run" not in str(steps[0]["run"]):
            problems.append(error(".github/scripts/ci_job_reads.py", f"TAG_RUNS names {where}, which passes no "
                                                                      f"-run, so it runs the package's untagged tests itself; drop the entry"))
            continue
        untagged = PathsFilter(filters[entry.untagged_filter], quantifiers[entry.untagged_filter])
        held, counted, named = tag_step_files(entry, gate_files, selection)
        read = set(held)
        block = [f for f in tag_listed_files(entry, listed, expander) if f not in read]
        # Per way a refusal names a file: the files the tag step reads, then the ones only its
        # filter's listed block holds.
        uncovered = {named: [f for f in held if not untagged.covers(f)],
                     f"filter {entry.filter}'s listed block holds": [f for f in block if not untagged.covers(f)]}
        of_block = f" and {len(block)} of its listed block" if block else ""
        report.append(f"{where}: {len(held)} files {counted}{of_block}, "
                      f"{sum(len(files) for files in uncovered.values())} uncovered by {entry.untagged_job} "
                      f"(filter {entry.untagged_filter})")
        for which, files in uncovered.items():
            if not files:
                continue
            more = f", and {len(files) - SHOWN} more" if len(files) > SHOWN else ""
            problems.append(error(entry.workflow, f"{where} runs only what its tag adds (-run), so {entry.untagged_job} "
                                                  f"must run that package's untagged tests in every run it runs ({entry.why}), "
                                                  f"but filter {entry.untagged_filter} does not match "
                                                  f"{' '.join(files[:SHOWN])}{more}, which {which}: a change to one of "
                                                  f"them runs the tagged tests alone and the untagged ones nowhere"))
    # The other side of the rule: an entry holds the step it names, and this holds every step no
    # entry names - a step added without one, or an entry deleted while its step stayed. A step
    # whose entry is itself stale is named all the same, so it keeps the refusal above rather
    # than drawing a second, confusing one.
    for path, name, step in tag_run_steps(documents):
        if any((entry.workflow, entry.job) == (path, name) and step_matches(step, entry.step) for entry in data.TAG_RUNS):
            continue
        problems.append(error(path, f"{name} step \"{step_label(step)}\" runs only what its tag adds "
                                    f"(-run), but no TAG_RUNS entry of .github/scripts/ci_job_reads.py names it: the job that "
                                    f"runs that package's untagged tests must be declared there, so the check holds it to "
                                    f"running in every run this step runs"))
    return report


def declared_reads(data) -> list:
    """Every read pattern ci_job_reads.py declares, each with where it is: a gate's reads, or a
    RULES row's (a row's Runs declare go commands, not patterns)."""
    reads = [(r.glob, f"gate {g.filter} reads") for g in data.GATES for r in g.reads]
    return reads + [(read.glob, f"the RULES row for {source} reads")
                    for source, entries in data.RULES.items()
                    for read in entries if isinstance(read, Read)]


def check_dead_patterns(data, filters: dict, expander: Expander, problems: list[Annotation]) -> None:
    """Rule 9's first direction: a pattern that matches no file of the repository names nothing
    the run can select or cover. The other rules here pass over such a pair: a filter and a
    declared read that both name a path that does not exist agree with each other, and the gate's
    read expands to no file to hold the filter to, so a change to the files they mean starts no
    job, on the pull request and again on main. Judged over repo_files() with the Expander the
    rest of the check uses, so a pattern is held to exactly the files a dorny/paths-filter run
    sees. A `!` pattern is exempt: it names what to leave out, and leaving nothing out is no
    fault. It reads .github/ci-filters.yaml alone, and no workflow: the filters are the changes
    job's, which its one paths-filter step reads from that file, and a declared read is the
    table's rather than any workflow's.

    The rule's second direction - a filter pattern that matches files none of its job reads - is
    check_dead_gate_patterns below. The two halves are judged in different places because they
    are judged over different sets: this one needs only the repository's files, which main() has
    before it starts the go work, while the other needs the files each gate reads, which exist
    only once every go command's closure has been computed. Judging this half first means a
    mistyped path is named even when the go work that follows cannot run."""
    for name in sorted(filters):
        for pattern in dict.fromkeys(filters[name]):
            if pattern.startswith("!") or expander.expand(pattern):
                continue
            problems.append(error(FILTERS_FILE, f"filter {name} has {pattern}, which matches no file in the "
                                                f"repository, so no change ever selects on it; correct the path or drop "
                                                f"the pattern"))
    for pattern, where in dict.fromkeys(declared_reads(data)):
        if expander.expand(pattern):
            continue
        problems.append(error(".github/scripts/ci_job_reads.py", f"{where} {pattern}, which matches no file in "
                                                                  f"the repository, so it holds its filter to nothing; correct the path or drop the read"))


def tag_step_files(entry, gate_files: dict, selection: Selection) -> tuple:
    """What the tag step of a TAG_RUNS entry reads, which rule 15 holds the filter of the job that
    runs the package's untagged tests to, as (the files, sorted; how the report counts them; how a
    refusal names them). A change that starts the tagged run and that the tag step reads must
    start the untagged one with it: the tag's own test files, the package's untagged ones the
    tagged build compiles too, and what the tag step's gate declares besides - the
    go-tagged-tests.sh a soak takes its -run from, and the gate's own workflow files. They are
    the read set main() builds for the gates on the entry's filter (gate_files), the set rule 1
    holds that filter to; the entry names that filter as the tag step's gate.

    The read set and never what the filter selects: a filter's selection is its patterns net of
    its generated block, so an entry pairing a filter with itself (envoy_go, envoy_infra_go) would
    hold the filter to every file its own patterns reach. Rule 17 would then want no `!` pattern
    of it, so a generated line deleted by hand would pass, and rule 9's second direction would
    refuse none of its patterns. Only a tag step's filter this check computes no read set for (an
    EXCLUDED one, which carries no generated block) is stood for by what it selects, since that
    is all the check knows of what its step reads."""
    if entry.filter in gate_files:
        files = gate_files[entry.filter]
        whose = " and ".join(files.wheres)
        return (sorted(files.reads), f"read by {whose} (filter {entry.filter})",
                f"{whose} {'read' if len(files.wheres) > 1 else 'reads'}")
    return (selection.of(entry.filter), f"selected by filter {entry.filter}", f"filter {entry.filter} matches")


def tag_listed_files(entry, listed: dict, expander: Expander) -> list:
    """The files of the listed block of a TAG_RUNS entry's tag filter, which rule 15 holds the
    filter of the job that runs the package's untagged tests to beside what the tag step reads
    (tag_step_files), sorted. Such a block (TS_LISTED) is the files of a tree that the tag step
    loads and this check cannot compute: .github/scripts/check-ts-filters.sh derives them, from
    the tsc programs or the `bun build` of an entry its table pairs with the filter, writes them
    and holds the block to them. They are no declared read of the tag step's gate, so its read set
    leaves them out: dispatch_two_builds_go's block is the contracts modules of the dispatch
    command its step runs. A change to one of them starts the tagged run all the same, so it must
    start the untagged one with it, whatever patterns of its own the untagged filter takes that
    tree in by. Read from the block's own lines in .github/ci-filters.yaml, among the blocks
    listed_owners accepts (`listed`), each pattern expanded to the files it matches: a block whose
    marker names no check CI runs is refused there and holds nothing here, since no check holds its
    lines true."""
    block = listed.get(entry.filter)
    if block is None or block.begin < 0:
        return []
    return sorted({f for pattern in block.patterns for f in expander.expand(pattern)})


def tag_held_files(data, filters: dict, gate_files: dict, selection: Selection, listed: dict,
                   expander: Expander) -> dict:
    """Per filter, the files rule 15 holds it to: every file a TAG_RUNS step reads
    (tag_step_files) and every file of its filter's listed block (tag_listed_files), held on the
    filter of the job that runs that package's untagged tests, so a change that starts the tagged
    run starts the untagged one with it. Rule 9's second direction passes over them
    (rule_required_files) and rule 17 writes no `!` pattern for one (wanted_negations): either
    would be refusing the rule that put the file there. A listed block's files are what tsc
    programs compile and bundles load, never a Go test file, so rule 17, which reads Go test files
    alone, finds none of them here. An entry pairing a filter with itself adds nothing here, since
    what its tag step reads is already what that filter's gate reads, and its listed block is its
    own."""
    held: dict = defaultdict(set)
    for entry in data.TAG_RUNS:
        if entry.filter in filters and entry.untagged_filter in filters:
            held[entry.untagged_filter].update(tag_step_files(entry, gate_files, selection)[0])
            held[entry.untagged_filter].update(tag_listed_files(entry, listed, expander))
    return held


def rule_required_files(data, filters: dict, gate_files: dict, selection: Selection, listed: dict,
                        expander: Expander) -> dict:
    """Per filter, the files another rule holds that filter to whatever this check computes its
    job reads, which rule 9's second direction has to pass over: refusing one of those patterns
    would be refusing the rule that put it there.

    Two rules of this check do it. Rule 8 keeps a PUSH_WORKFLOWS workflow's on.push.paths equal
    to its filter, and a workflow that does not name itself never runs on a change to itself: the
    docs filter holds .github/workflows/docs-pages.yaml so that main's docs build runs when its
    own workflow changes, which is a run of that file rather than a read by ci.yaml's docs job.
    Rule 15 holds the filter of the job that runs a package's untagged tests to every file the
    tag step reads and every file of the tag step filter's listed block (tag_held_files), so a
    change that starts the tagged run starts the untagged one with it: envoy_go_race holds
    .github/scripts/go-tagged-tests.sh, which listener-server-change's soak takes its -run from
    and no step of envoy-go-race reads.

    And .github/scripts/check-ts-filters.sh does it from outside: it writes into a filter a
    listed block (TS_LISTED below) of the files of a tree that a job's tsc programs compile or the
    entry it runs with Bun loads, and refuses a committed block that is not exactly that, in
    ci.yaml's lint job, since this one has no Bun to run tsc or `bun build` with. Those files are
    reads of the job this check cannot compute, so the patterns of such a block, one file each,
    are passed over here; the filter's coverage of them is that check's to prove
    (ci_ts_programs.py, LISTED).

    This widens to nothing else. A pattern is passed over only when every file it matches is in
    this set, so one that reaches a single file outside it is judged like any other."""
    required: dict = defaultdict(set)
    for name, path in data.PUSH_WORKFLOWS.items():
        if name in filters:
            required[name].add(path)
    for name, held in tag_held_files(data, filters, gate_files, selection, listed, expander).items():
        required[name].update(held)
    for name, block in listed.items():
        if name in filters and block.begin >= 0:
            for pattern in block.patterns:
                required[name].update(expander.expand(pattern))
    return required


def check_dead_gate_patterns(data, filters: dict, gate_files: dict, expander: Expander,
                             selection: Selection, listed: dict, problems: list[Annotation]) -> None:
    """Rule 9's second direction: a pattern of a gate's filter that matches files of the
    repository but none its job reads. Such a pattern selects the job for a change no step of it
    can see, which costs a full run of that job every time one of those files changes - the fault
    rule 1 cannot see, since it reads the other way round, from the files a job reads to the
    patterns that must match them.

    `gate_files` is, per filter, the union of the files the gates on it read and how the check
    names those gates. The read set is the keys of the `reason` dict main() builds per gate,
    which is exactly the set rule 1 holds the filter to. The union is what a shared filter must
    be judged over - two gates name envoy_go_race_docs today - since a pattern matching only
    what the second job reads does start a job that reads the change. The names are the job and,
    for a gate that is a step's, the step.

    Four kinds of pattern are passed over. A `!` pattern, as in the first direction: it names what
    to leave out, so it reads no file by design and matching none of a job's reads is what it is
    for. A pattern that matches no file of the repository at all, which is the first direction's
    refusal: naming it here as well would refuse one pattern twice for one fault. Every
    pattern of an EXCLUDED filter, which reaches here in no gate: the check computes no read set
    for one (worker_image today), so it has nothing to judge the patterns against, and
    .github/scripts/check-image-trigger-paths.sh proves that filter's coverage instead. And a
    pattern whose every file another rule holds the filter to (rule_required_files above, given
    `listed`, the changes job's listed blocks), which is the one case where a pattern matching no
    read is right rather than idle."""
    required = rule_required_files(data, filters, gate_files, selection, listed, expander)
    for name in sorted(gate_files):
        reads, wheres = gate_files[name].reads, gate_files[name].wheres
        where = " and ".join(wheres) + (" read" if len(wheres) > 1 else " reads")
        for pattern in dict.fromkeys(filters[name]):
            matched = expander.expand(pattern) if not pattern.startswith("!") else []
            if not matched or not reads.isdisjoint(matched) or set(matched) <= required.get(name, frozenset()):
                continue
            problems.append(error(FILTERS_FILE, f"filter {name} has {pattern}, which matches no file {where}, "
                                                f"though it matches {len(matched)} of the repository's, so a change to "
                                                f"one of them runs that job over a change no step of it sees; drop the "
                                                f"pattern"))


@dataclass
class GateFiles:
    """Per filter, what rule 9's second direction is judged over: the union of the files the
    gates on it read, and how the check names each of those gates."""

    reads: set = field(default_factory=set)
    wheres: list = field(default_factory=list)


# ---------------------------------------- rule 17: the test files a gate's filter must not select
SCRIPT = ".github/scripts/check-ci-filters.sh"
WRITE = f"{SCRIPT} write"
# A generated block's first and last line, written at the indentation of the filter's own items.
# The first names the command that writes it, so a reader of the filters file knows not to edit
# one by hand; those two prefixes are how both modes find the block again.
BEGIN = "# BEGIN generated"
END = "# END generated"
BLOCK_HEAD = (
    f"{BEGIN} by `{WRITE}`: a change to a Go test file the job this filter gates does not read",
    "# must not select it. An edit by hand is refused (rule 17 of that check), which writes these.",
)


@dataclass(frozen=True)
class Marks:
    """One kind of generated block: its first and last line's prefixes and the command that
    writes it, which a refusal names."""

    begin: str
    end: str
    write: str


GENERATED = Marks(BEGIN, END, WRITE)
# The other generated block a gate filter may hold: one pattern per file of a tree that the tsc
# programs paired with some filters compile or the bundles paired with them load
# (.github/scripts/ci_ts_programs.py, LISTED), which .github/scripts/check-ts-filters.sh writes in
# its own write mode and refuses when it differs, since only that check, in ci.yaml's lint job,
# has the Bun to run tsc and `bun build` with. Its own two lines mark it, so neither check takes
# the other's block for its own: rule 17 reads and rewrites only the lines between BEGIN and END,
# a listed block's lines are items of the list to it, and rule 9's second direction passes over a
# listed block's patterns (rule_required_files) while its BEGIN line names that check, which a job
# of CI runs (listed_owners below). Rule 15 reads such a block as what the tag step its filter
# gates loads, and holds that step's untagged job's filter to it (tag_listed_files).
TS_LISTED = Marks("# BEGIN listed", "# END listed", ".github/scripts/check-ts-filters.sh write")
KINDS = (GENERATED, TS_LISTED)

# A listed block's first line names the command that writes it, `<script> write`, which is also
# the check that holds its lines true: `# BEGIN listed by `<script> write`: ...`, as
# check-ts-filters.sh's head() writes it.
LISTED_OWNER = re.compile(r"^# BEGIN listed by `(\S+) write`")
# The programs a run: line may hand a script to rather than run it itself.
INTERPRETERS = frozenset({"bash", "sh", "python3"})


def ci_commands(documents: dict) -> set:
    """What CI runs: the command each line of every run: step of ci.yaml and the workflows its
    jobs call starts - the line's first word, or its second where the first is an interpreter -
    as a repository-relative path where it is one. ci.yaml's lint job runs check-ts-filters.sh
    this way (.github/workflows/ci-job-lint.yaml), which makes it an owner listed_owners accepts."""
    commands = set()
    for _, _, _, _, job in step_holders(documents):
        for step in job.get("steps") or []:
            for line in script_lines(str(step.get("run", ""))):
                words = line.split()
                if len(words) > 1 and words[0] in INTERPRETERS:
                    words = words[1:]
                if words:
                    commands.add(words[0].removeprefix("./"))
    return commands


def listed_owners(documents: dict, lines: list, problems: list[Annotation]) -> dict:
    """The listed blocks of .github/ci-filters.yaml that rule 9's second direction may pass over,
    and rule 15 reads for a tag step's filter: each one whose BEGIN line names, as the command that
    writes it, a script a step of CI runs (ci_commands) other than this check. Passing over a
    block's patterns is right only because another check holds them to what the job reads, so a
    block whose marker names no such check - a script no job runs, this check itself, or nothing -
    holds its filter to nothing: it is refused, naming the filter and the marker, and its patterns
    are judged like any other."""
    runs = ci_commands(documents)
    owned = {}
    for name, block in filter_blocks(lines, [], TS_LISTED).items():
        if block.begin < 0:
            continue
        marker = lines[block.begin].strip()
        owner = LISTED_OWNER.match(marker)
        script = owner.group(1).removeprefix("./") if owner else ""
        if script and script != SCRIPT and script in runs:
            owned[name] = block
            continue
        if not script:
            what = "no check, in the `# BEGIN listed by `<script> write`` form a listed block's first line takes"
        elif script == SCRIPT:
            what = f"`{script} write`, this check, which passes over a listed block's lines rather than holding them"
        else:
            what = f"`{script} write`, a command no step of {WORKFLOW} or of a workflow it calls runs"
        problems.append(error(FILTERS_FILE, f"filter {name}'s listed block opens with `{marker}`, which names "
                                            f"{what}, so no check of CI holds its lines and rule 9 does not pass over "
                                            f"them; name the check that writes and holds the block and that a job of CI "
                                            f"runs, as `{TS_LISTED.write}` does"))
    return owned



def selected(patterns: list, expander: Expander) -> set:
    """The files a `some-with-excludes` filter selects: a pattern without `!` matches it and no
    `!` pattern does. Expanded through the Expander, whose answers every other rule here shares,
    rather than a covers() call per file of the repository per filter."""
    yes, no = set(), set()
    for pattern in patterns:
        (no if pattern.startswith("!") else yes).update(expander.expand(pattern.removeprefix("!")))
    return yes - no


def negation_patterns(excluded: set, read: set, tests: list) -> list:
    """The `!` patterns that keep a filter off every file of `excluded` while excluding none of
    `read`, in the two forms a reader can check by eye: `!<dir>/**/*_test.go`, where no file
    `read` is anywhere under <dir>, and `!<path>`, where one is. The choice is the fewest of
    those two forms the sets allow, and the same text on every run over one tree: the walk takes
    the highest directory whose whole tree may go, names one by one the excluded files of a
    directory that holds a read file too, and reads directories and files in sorted order.

    `excluded` is the Go test files the filter selects and the gates on it do not read: a file
    the filter does not select needs no pattern to keep it unselected, and writing one anyway
    would fill every filter with the test files its patterns never reach. A tree pattern
    excludes the files under it that are in neither set too, which is why a directory may take
    one only when nothing `read` is under it, and which is what makes the block stable: a test
    file added to a package the job does not read lands inside a pattern that is already there
    or adds one line, and a source file added anywhere changes nothing here."""
    if not excluded:
        return []
    kids: dict = defaultdict(set)  # directory -> the directories directly under it
    here: dict = defaultdict(list)  # directory -> the test files directly in it
    read_dirs, excluded_dirs = set(), set()  # the directories holding one, at any depth
    for path in tests:
        node = os.path.dirname(path)
        here[node].append(path)
        marks = [s for s, members in ((read_dirs, read), (excluded_dirs, excluded)) if path in members]
        while True:
            for mark in marks:
                mark.add(node)
            if not node:
                break
            parent = os.path.dirname(node)
            kids[parent].add(node)
            node = parent
    out: list = []

    def emit(node: str) -> None:
        if node not in excluded_dirs:
            return
        if node not in read_dirs:
            out.append(f"!{node}/**/*{TEST_SUFFIX}" if node else f"!**/*{TEST_SUFFIX}")
            return
        out.extend(f"!{path}" for path in sorted(here[node]) if path in excluded)
        for kid in sorted(kids[node]):
            emit(kid)

    emit("")
    return out


@dataclass
class Block:
    """One pattern list in a file's text - a filter of .github/ci-filters.yaml, or a workflow's
    on.push.paths: where its generated block is and what it holds, or where one would go. The
    write mode replaces exactly those lines, so the text is read here rather than the parsed
    document, which keeps neither comments nor order."""

    indent: str = ""  # the indentation of the list's items
    after: int = -1  # the list's last item line outside a block: where a new block goes
    begin: int = -1
    end: int = -1
    patterns: list = field(default_factory=list)


PATHS_KEY = re.compile(r"^(\s*)paths:\s*$")
# A filter of .github/ci-filters.yaml: a key at the top level of that file, its value the list
# below it, with or without an anchor on the key.
FILTER_KEY = re.compile(r"^([A-Za-z0-9_]+):\s*(?:&\S+\s*)?$")
FILTER_ITEM = re.compile(r"^(\s*)-\s+(.*?)\s*$")


def indent_of(line: str) -> int:
    return len(line) - len(line.lstrip())


def list_block(lines: list, body: range, block: Block, path: str, marks: Marks = GENERATED) -> Block:
    """`block` filled in from the lines of one pattern list of `path`'s text: its items'
    indentation, its last item outside a block of `marks`' kind, and that block's own two lines and
    patterns. A line closing a block of another kind counts as an item, so a block written where
    none was goes after that whole block rather than inside it."""
    others = tuple(kind.end for kind in KINDS if kind != marks)
    for i in body:
        line, text = lines[i], lines[i].strip()
        if not text:
            continue
        inside = block.begin >= 0 and block.end < 0
        item = FILTER_ITEM.match(line)
        if item:
            block.indent = item.group(1)
            if inside:
                block.patterns.append(load_repository_yaml_text(path, item.group(2)))
            else:
                block.after = i
        elif text.startswith(marks.begin):
            block.begin, block.indent = i, line[: indent_of(line)]
        elif text.startswith(marks.end):
            block.end = i
        elif text.startswith(others) and not inside:
            block.after = i
    return block


def closed(blocks: dict, path: str, problems: list[Annotation], marks: Marks = GENERATED) -> dict:
    """`blocks` with every list whose BEGIN line has no END line after it left as a list with no
    block: the write mode would otherwise replace lines nobody meant it to."""
    for name, block in blocks.items():
        if block.begin >= 0 and block.end < block.begin:
            problems.append(error(path, f"{name} opens a generated block and never closes it with a "
                                        f"`{marks.end}` line; run `{marks.write}`"))
            block.begin = -1
    return blocks


def filter_blocks(lines: list, problems: list[Annotation], marks: Marks = GENERATED) -> dict:
    """Per filter of .github/ci-filters.yaml - every gate filter and every group an anchor names -
    where its list ends in that file's text and which of its lines are a generated block of
    `marks`' kind. A filter's name is a key at the top level of the file, so each such key opens
    a list and the next one closes it. The comment that stands at column 0 above a key falls in
    the list before it, and is no line of that list either, since list_block reads items and block
    marks alone."""
    starts = [i for i, line in enumerate(lines) if FILTER_KEY.match(line)]
    if not starts:
        problems.append(error(FILTERS_FILE, "no filter at the top level of this file, where this check reads the "
                                            "lists the changes job's paths-filter step selects its jobs by"))
        return {}
    blocks = {FILTER_KEY.match(lines[start]).group(1): list_block(lines, range(start + 1, end), Block(),
                                                                  FILTERS_FILE, marks)
              for start, end in zip(starts, starts[1:] + [len(lines)])}
    return closed(blocks, FILTERS_FILE, problems, marks)


def push_paths_block(lines: list, name: str, path: str, problems: list[Annotation]) -> dict:
    """The `on.push.paths` list of a PUSH_WORKFLOWS workflow, keyed by the filter rule 8 holds it
    equal to: the write mode writes that filter's block here as well, since a generated pattern
    the two lists do not share is rule 8's refusal and no hand's to cure."""
    keys = [i for i, line in enumerate(lines) if PATHS_KEY.match(line)]
    if len(keys) != 1:
        problems.append(error(path, f"{len(keys)} `paths:` lists, where this check writes filter {name}'s "
                                    f"generated block into the one on.push.paths"))
        return {}
    body = range(keys[0] + 1, len(lines))
    outer = indent_of(lines[keys[0]])
    stop = next((i for i in body if lines[i].strip() and indent_of(lines[i]) <= outer), len(lines))
    return closed({name: list_block(lines, range(keys[0] + 1, stop), Block(), path)}, path, problems)


def wanted_negations(filters: dict, quantifiers: dict, gate_files: dict, blocks: dict, expander: Expander,
                     tests: list, held: dict) -> dict:
    """Per gate filter, the `!` patterns its list must hold: the ones that keep it off every Go
    test file it selects and the gates on it do not read. What the filter selects is read from
    its patterns without its own generated block, so two runs over one tree write one text;
    every other `!` pattern of the list counts, since one of those narrows what is left to
    exclude. A filter the changes job declares under `some` gets none: a `!` pattern there
    matches every file outside it, which rule 6 refuses, and these patterns would then mean the
    opposite of what they say. A test file rule 15 holds the filter to (`held`, tag_held_files)
    is kept as a read file is: listener-server-change's soak reads cmd/listener's tagged test,
    which envoy-go-race does not compile, and rule 15 holds envoy_go_race to selecting it so
    that a change to it runs that package's untagged tests too. What rule 15 holds a filter to is
    what a tag step reads (tag_step_files) and the files of its filter's listed block
    (tag_listed_files), never a selection a generated block narrows, so this set does not grow
    when a hand deletes a `!` pattern."""
    wanted = {}
    for name in sorted(gate_files):
        if quantifiers[name] != "some-with-excludes":
            continue
        generated = set(blocks[name].patterns) if name in blocks else set()
        base = [p for p in filters[name] if p not in generated]
        read = {f for f in gate_files[name].reads | held.get(name, set()) if f.endswith(TEST_SUFFIX)}
        picked = {f for f in selected(base, expander) if f.endswith(TEST_SUFFIX)}
        wanted[name] = negation_patterns(picked - read, read, tests)
    return wanted


def check_negations(blocks: dict, wanted: dict, filters: dict, expander: Expander,
                    problems: list[Annotation]) -> list:
    """Rule 17: the committed block of each gate filter must be exactly the patterns above, and
    no `!` pattern outside a generated block may match a Go test file. The refusal names the
    filter, the first pattern that differs and the command that writes the block. Returns the
    report lines."""
    for name in sorted(blocks):
        if blocks[name].begin >= 0 and name not in wanted:
            problems.append(error(FILTERS_FILE, f"{name} has a generated block, and it is no filter of a gate "
                                                f"this check computes a read set for, so nothing says what belongs in "
                                                f"it; delete the block"))
    for name in sorted(wanted):
        patterns, block = wanted[name], blocks.get(name, Block())
        committed = block.patterns
        for i in range(max(len(patterns), len(committed))):
            if i < len(committed) and i < len(patterns) and committed[i] == patterns[i]:
                continue
            if i >= len(committed):
                says = f"lacks '{patterns[i]}'"
            elif i >= len(patterns):
                says = f"has '{committed[i]}', which keeps it off no test file its job leaves unread"
            else:
                says = f"has '{committed[i]}' where '{patterns[i]}' belongs"
            head = (f"filter {name}'s generated block" if block.begin >= 0
                    else f"filter {name}, which has no generated block,")
            problems.append(error(FILTERS_FILE, f"{head} {says}: the `!` patterns that keep a "
                                                f"filter off the Go test files its job does not read are generated, so "
                                                f"a change to what the job reads rewrites them; run `{WRITE}`"))
            break
        loose = [p for p in dict.fromkeys(filters[name])
                 if p.startswith("!") and p not in set(committed)
                 and any(f.endswith(TEST_SUFFIX) for f in expander.expand(p.removeprefix("!")))]
        for pattern in loose:
            problems.append(error(FILTERS_FILE, f"filter {name} has {pattern} outside its generated block, and "
                                                f"it matches a Go test file: the test files a filter excludes are this "
                                                f"rule's to write, over the set its gates read, so move the exclusion "
                                                f"there by running `{WRITE}`"))
    return [f"{FILTERS_FILE}: {sum(len(p) for p in wanted.values())} generated `!` patterns keep "
            f"{sum(1 for p in wanted.values() if p)} of {len(wanted)} gate filters off the Go test files their jobs "
            f"do not read"]


def rewrite(path: str, lines: list, blocks: dict, wanted: dict, heads=lambda name: BLOCK_HEAD,
            marks: Marks = GENERATED) -> None:
    """`path` written again with each list's generated block of `marks`' kind replaced by its
    patterns in `wanted`, under the head `heads` gives for it, and left alone when that changes
    nothing. A list with no pattern to write keeps no block, and one with no block yet takes it
    after its last item, so the block belongs to the filter whose list holds it and never to a
    group an anchor shares."""
    out = list(lines)
    for name in sorted(blocks, key=lambda n: -max(blocks[n].begin, blocks[n].after)):
        block, patterns = blocks[name], wanted.get(name, [])
        written = ([block.indent + line for line in heads(name)]
                   + [f"{block.indent}- '{pattern}'" for pattern in patterns]
                   + [block.indent + marks.end]) if patterns else []
        if block.begin >= 0:
            out[block.begin : block.end + 1] = written
        elif written and block.after >= 0:
            out[block.after + 1 : block.after + 1] = written
    text = "".join(line + "\n" for line in out)
    if text != read_repository_text(REPO / path):
        (REPO / path).write_text(text)


# Rule 20 searches every choice of a file's own tags for one go builds it under; a file naming
# more tags than this is refused by name rather than searched.
MAX_TRIAL_TAGS = 6


def check_vet_tags(session: Session, data, problems: list) -> list:
    """Rule 20: the vets for another platform of one step - the Runs of mode vet whose env sets
    GOOS, one per tag set the step loops over - compile between them every file of their
    packages that go builds for that platform under some choice of tags. Such a step is its
    module's one compile for that platform, and a file outside every set it vets under is a file
    it never reads while it still passes: the miss a hand-kept tag list makes the day a tag
    lands, and the one a single set makes beside a file under a negated tag.

    The files are those on disk under the Runs' patterns (go_build_tags.go_files, the directories
    `./...` walks), never go list's own package list, which leaves out a package whose every file
    a missing tag excludes. go list then says, under each Run's platform and tags, which of them
    it compiles. For each file none compiles that names a tag (go_build_tags.is_tag: no platform
    or architecture feature name, none the toolchain sets), every choice of its own tags is
    tried, and go list says again whether the file is compiled under it: one that is, is a miss,
    named with the tags that build it. go decides each answer; this rule's own reading of a
    constraint only chooses which tags to try, and it reads a name in any script Go accepts."""
    groups: dict = defaultdict(list)
    for g in data.GATES:
        for r, step in [(r, r.step) for r in g.runs] + [(r, e.step) for e in g.exact for r in e.runs]:
            env = dict(r.env)
            if r.mode == "vet" and env.get("GOOS"):
                groups[(g.job, step, r.workdir, env["GOOS"], env.get("GOARCH", ""), r.pkgs)].append(r)
    report = []
    for (job, step, workdir, goos, goarch, pkgs), runs in groups.items():
        where = f'job {job} step "{step}"' if step else f"job {job}"
        target = f"{goos}/{goarch}" if goarch else goos
        try:
            known = platforms(REPO / workdir)
        except (OSError, subprocess.CalledProcessError) as failure:
            problems.append(error(None, f"{where}: go tool dist list failed in {workdir}, so which names of a "
                                        f"build constraint are platforms is unknown: {failure}"))
            continue
        disk = set()
        for pattern in pkgs:
            base = REPO / workdir / pattern.removesuffix("...").rstrip("/")
            found = go_files(base) if pattern.endswith("...") else (
                p for p in sorted(base.glob("*.go")) if not p.name.startswith(("_", ".")))
            disk |= {rel(str(path)) for path in found}
        if not disk:
            problems.append(error(None, f"{where}: no .go file is on disk under {' '.join(pkgs)} in {workdir}, "
                                        f"so rule 20 checked nothing there"))
            continue
        dirs = sorted({path.rsplit("/", 1)[0] for path in disk})
        sets = [r.tags for r in runs]
        compiled = set().union(*(compiled_files(session, r, r.tags, dirs) for r in runs))
        tagged = misses = 0
        trials: dict = {}
        for path in sorted(disk - compiled):
            names = sorted(n for n in constraint_names(REPO / path) if is_tag(n, known))
            if not names:
                continue
            tagged += 1
            if len(names) > MAX_TRIAL_TAGS:
                problems.append(error(path, f"{where}: {path} names {len(names)} tags, more than the "
                                            f"{MAX_TRIAL_TAGS} rule 20 tries, so whether a vet of {target} "
                                            f"misses it is unknown"))
                continue
            directory = path.rsplit("/", 1)[0]
            for bits in itertools.product((True, False), repeat=len(names)):
                tags = ",".join(n for n, on in zip(names, bits) if on)
                if (directory, tags) not in trials:
                    trials[directory, tags] = compiled_files(session, runs[0], tags, [directory])
                if path in trials[directory, tags]:
                    misses += 1
                    problems.append(error(path, f"{where} vets {workdir} for {target} under -tags "
                                                f"{' and '.join(s or '(none)' for s in sets)}, and none of them "
                                                f"compiles {path}, which go builds for {target} with -tags "
                                                f"{tags or '(none)'}: the vet passes without reading the file. The "
                                                f"vet steps take their tag sets from .github/scripts/"
                                                f"go_build_tags.py, which must print a set that builds every such "
                                                f"file"))
                    break
        report.append(f"{where}: vets {workdir} for {target} under -tags "
                      f"{' and '.join(s or '(none)' for s in sets)}; go list compiles {len(compiled & disk)} of "
                      f"the {len(disk)} .go files on disk there, {tagged} of the rest name a tag, and "
                      f"{misses} of those build for {target} under tags no set holds")
    return report


def compiled_files(session: Session, r: Run, tags: str, dirs: list) -> set:
    """The files go list compiles in `dirs`, each named as a directory and not as a pattern, under
    the Run's platform and `tags`, as repository paths: GoFiles, CgoFiles and the test files,
    which a vet reads too. A directory whose every file is excluded is still listed, with an
    error -e keeps. `-find`, as go_build_tags.compiled runs it too, keeps go list from loading the
    packages the listed ones import (Imports, Deps and DepsErrors stay empty), though in module
    mode go still resolves each import: the file lists are the same, and the rule reads nothing
    else."""
    args = ["list", "-e", "-find", "-json=Dir,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles"]
    if tags:
        args += ["-tags", tags]
    patterns = [f"./{Path(d).relative_to(r.workdir).as_posix()}" if d != r.workdir else "." for d in dirs]
    out = set()
    for pkg in decode_stream(session.go(r, args + patterns)):
        d = rel(pkg["Dir"]) if pkg.get("Dir") else None
        if d is not None:
            for field in ("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles"):
                out |= {f"{d}/{name}" for name in pkg.get(field) or []}
    return out


def check() -> int:
    # The mode check-ci-filters.sh was called in: `check`, which refuses a generated block that
    # is not what rule 17 computes, or `write`, which writes those blocks into
    # .github/ci-filters.yaml instead. Every other rule runs in both, since the blocks are computed
    # from the read sets the rest of this check builds.
    mode = os.environ.get("CI_FILTERS_MODE", "check")
    problems: list[Annotation] = []
    # ci.yaml and every workflow its jobs call: a job that calls one keeps its gate, its id and
    # its if: here and holds its steps there, so the rules that read steps read the set.
    documents = workflow_documents(problems)
    document = documents[WORKFLOW]
    import ci_job_reads as data  # imported here so its at() citations land in this module's CITED
    # The rules of ci.yaml that read no filter and no go command's closure, in a module of their
    # own. It is imported here, not at the top, because it imports this module's constants and
    # helpers: by the time main() runs, this module is loaded whole.
    import ci_workflow_rules as rules

    # The filters, and the text of .github/ci-filters.yaml they were read from, which rule 17 and
    # the listed blocks read below: one read of the file serves all three.
    filters, quantifiers, lines = changes_filters(document, problems)
    files = repo_files()
    unreduced = check_table(documents, data, filters, set(files), problems)
    check_ci_ok(documents, problems)
    check_output_steps(document, filters, problems)
    check_filter_inputs(rules.directory_workflows(), problems)
    rules.check_push_conditions(documents, problems)
    rules.check_rerun_guard(document, problems)
    rules.check_cache_round_trip(documents, problems)
    rules.check_race_fill_packages(data.DAEMON_RACE_PACKAGES, data.DAEMON_RACE_FILL_JOB, problems)
    rules.check_race_fill_scripts(data.RACE_FILL_SCRIPTS, problems)
    rules.check_workspace_table(data.WORKSPACES, problems)
    rules.check_one_merge_group_workflow(problems)
    expander, selection = Expander(files), Selection(filters, quantifiers, files)
    # Rule 9's first direction, over the repository's files alone: it needs no closure, so a
    # mistyped path is named even when the go work below cannot run. Its second direction is
    # after that work, where the files each gate reads exist.
    check_dead_patterns(data, filters, expander, problems)
    session = Session()
    report = []
    # Per filter, the files the gates on it read and how the check names those gates: what rule
    # 9's second direction is judged over, collected as each gate's set is built rather than
    # computed a second time.
    gate_files: dict = defaultdict(GateFiles)
    fallback = None  # module_trees_and_go_files(files), once a gate needs it
    for g in data.GATES:
        if g.filter not in filters:
            continue
        patterns = filters[g.filter]
        negated = [p for p in patterns if p.startswith("!")]
        if negated and quantifiers[g.filter] == "some":
            problems.append(error(FILTERS_FILE, f"filter {g.filter} has {negated[0]}, a `!` pattern under "
                                                f"predicate-quantifier some, where it matches every file outside it"))
        if unread_filter_patterns(patterns):
            problems.append(error(FILTERS_FILE, f"filter {g.filter} has {unread_filter_patterns(patterns)[0]}, "
                                                f"a brace picomatch expands and this check does not read"))
        whys = defaultdict(list)
        seen: set = set()
        for r in gate_runs(g):
            for read in closure(session, data.RULES, r, seen):
                whys[read.glob].append(read.why)
        for read in g.reads:
            whys[read.glob].append(read.why)
        reason = {}
        for glob, why in whys.items():
            for f in expander.expand(glob):
                reason.setdefault(f, why[0])
        # An unreduced command counts as reading every Go file of the repository and every file
        # under a Go module's directory, and fails naming the command when its filter misses one.
        held = []
        for r, command, why_not in unreduced.get(g.filter, []):
            if fallback is None:
                fallback = module_trees_and_go_files(files)
            why = (f"{command}, which this check does not reduce ({why_not}), so it counts as reading every Go "
                   f"file of the repository and every file under a Go module's directory")
            for path in fallback:
                reason.setdefault(path, why)
            held.append(error(FILTERS_FILE, f"{why}; filter {g.filter} must select them all on any Go change"))
        matcher = PathsFilter(patterns, quantifiers[g.filter])
        uncovered = sorted(f for f in reason if not matcher.covers(f))
        where = g.job + (f' step "{g.step}"' if g.step else "")
        gate_files[g.filter].reads.update(reason)
        gate_files[g.filter].wheres.append(where)
        report.append(f"{where}: {len(reason)} files, {len(uncovered)} uncovered (filter {g.filter})")
        for f in uncovered[:SHOWN]:
            problems.append(error(f, f"{where} reads {f}, which its filter {g.filter} does not match, so a "
                                     f"change to it skips the job: {reason[f]}"))
        if len(uncovered) > SHOWN:
            problems.append(error(FILTERS_FILE, f"{where} reads {len(uncovered) - SHOWN} more files its filter "
                                                f"{g.filter} does not match"))
        if held and not set(fallback).isdisjoint(uncovered):
            problems += held
    # Rule 9's second direction passes over the patterns of the listed blocks whose marker names
    # the check that writes and holds them, and refuses a block whose marker names no check CI
    # runs (listed_owners); whether an owned block is well formed is its owner's to refuse. Rule
    # 15 reads the same blocks, holding a tag step's untagged job to its filter's one. Both read
    # the lines of .github/ci-filters.yaml changes_filters read above.
    listed = listed_owners(documents, lines, problems)
    check_dead_gate_patterns(data, filters, gate_files, expander, selection, listed, problems)
    # Rule 20: a vet for another platform passes over no file a build tag would compile.
    report += check_vet_tags(session, data, problems)
    problems += session.problems
    report += check_push_workflows(data, filters, problems)
    report += check_tag_runs(documents, data, filters, quantifiers, gate_files, selection, listed, expander,
                             problems)
    # Rule 17, last of the rules that read a filter: it needs the files each gate reads, which
    # the loop above has just finished, and the text of .github/ci-filters.yaml, read above.
    blocks = filter_blocks(lines, problems)
    tests = [f for f in files if f.endswith(TEST_SUFFIX)]
    wanted = wanted_negations(filters, quantifiers, gate_files, blocks, expander, tests,
                              tag_held_files(data, filters, gate_files, selection, listed, expander))
    if mode == "write":
        rewrite(FILTERS_FILE, lines, blocks, wanted)
        # Rule 8 holds a PUSH_WORKFLOWS workflow's on.push.paths equal to its filter, so the
        # block goes into both lists or the next run of this check refuses the pair.
        for name, path in data.PUSH_WORKFLOWS.items():
            if name in wanted and (REPO / path).is_file():
                pushed = read_repository_text(REPO / path).splitlines()
                rewrite(path, pushed, push_paths_block(pushed, name, path, problems), wanted)
        for name in sorted(wanted):
            if wanted[name]:
                report.append(f"filter {name}: {len(wanted[name])} generated `!` pattern(s)")
        report.append(f"{FILTERS_FILE}: wrote {sum(len(p) for p in wanted.values())} generated `!` patterns over "
                      f"{sum(1 for p in wanted.values() if p)} of {len(wanted)} gate filters")
    else:
        report += check_negations(blocks, wanted, filters, expander, problems)
    if report:
        report_token = secrets.token_hex(16)
        print(f"::stop-commands::{report_token}")
        for line in report:
            print("report: " + escape_message(line))
        print(f"::{report_token}::")
    problems = list(dict.fromkeys(problems))
    for problem in problems:
        print_annotation(problem)
    if problems:
        print_annotation(error(None, f"{len(problems)} CI filter problem(s)"))
        return 1
    print(f"every ci.yaml filter matches every file its job reads ({len(data.GATES)} gates), and every "
          f"PUSH_WORKFLOWS workflow's on.push.paths equals its filter ({len(data.PUSH_WORKFLOWS)})")
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
