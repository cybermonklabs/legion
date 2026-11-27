"""The engine of check-ci-filters.sh, whose header says what it proves and why. ci_job_reads.py,
beside it, is the data it reads: each ci.yaml gate's go commands and static reads, and the files
tests read outside their packages. ci_workflow_rules.py, also beside it, holds the rules that
read none of that - what no if: condition of ci.yaml may say, what every job's `if:` must say, what
the cache fill workflows must save for the entries ci.yaml restores, and that ci.yaml is the only workflow
of this repository running on a merge group - and main() runs them.

check-ci-filters.sh runs it from the repository root with this directory on PYTHONPATH, through
`import ci_filter_coverage` rather than as a script, so ci_job_reads.py gets these very classes
through its `from ci_filter_coverage import ...`.

A step an exact entry pins (Exact) is refused, never read as reading everything, when it is not
what the entry pins (refuse_off_pin): its script or directory differs, named by the first line
that differs (pin_difference, name_list_difference) or by both directories, or something around
it (an env name outside ENV_ALLOWED, a shell, a container, an environment write that reaches it)
can change what it compiles or which tests it runs. The pin is also the table's word for which
tests the step runs, and reading everything says nothing where a filter selects every Go file
already. What surrounds a go command is read over the steps whose writes may reach it: a pinned
step the steps before it, a claimed simple command the steps up to and including its own
(job_up_to), and a script's go command every step of each job that may run the script
(around_job), since which step starts a script is nothing this check can prove.
check-ci-filters.sh's header says why, gives the remedies - the step's own script line, or
ENV_ALLOWED for a name with which go compiles the same files and runs the same tests - and gives
the recipe that runs it as CI does, GOROOT unset.
"""

from __future__ import annotations

import atexit
import bisect
import functools
import hashlib
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
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path

import yaml
from go_build_tags import constraint_names, decode_stream, go_files, go_skips_dir, is_tag, platforms
from workflow_paths import PathsFilter, UniqueKeyLoader, events, glob_regex, unread_filter_patterns

WORKFLOW = ".github/workflows/ci.yaml"
# The workflow that fills, on a push to main, the Go cache families WORKFLOW's jobs restore.
CACHE_WORKFLOW = ".github/workflows/ci-caches.yaml"
# The fill workflows rule 11 (ci_workflow_rules.check_cache_round_trip) holds to WORKFLOW's
# restores: CACHE_WORKFLOW, one run at a time, and the changes job's two entries, one run per commit,
# so that no commit's entry waits for the Go fills or is lost to a newer push that replaced its run
# (.github/ci-notes/ci-caches.md#the-two-entries-the-changes-job-restores). Not every workflow that
# fills a cache on a push: docs-pages.yaml saves go-docs from the build job it shares with the docs
# job.
CACHE_WORKFLOWS = (CACHE_WORKFLOW, ".github/workflows/ci-caches-changes.yaml")
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
    """A file or glob a gate reads, and why; `but` names globs whose files it leaves out (a
    //ci:reads line's `!` globs, or `**/*_test.go` where the Go tests a job compiles are go list's
    to say)."""

    glob: str
    why: str
    but: tuple = ()


def read_files(read: Read, expander) -> set:
    """The files a Read reads: those its glob matches and none of its `but` globs does."""
    files = set(expander.expand(read.glob))
    for glob in read.but:
        files -= set(expander.expand(glob))
    return files


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
# that carries one keeps its id, its `needs: changes` and its whole if: condition and holds
# its steps in the called file, which runs them inside the caller's own run. So the rules that
# read a job's steps, its go commands or its cache restores read a set of documents - ci.yaml
# and every file its jobs call - rather than ci.yaml alone, and each of them names the file it
# found a job in rather than the constant WORKFLOW.
LOCAL_WORKFLOW = "./.github/workflows/"


def workflow_callers(document: dict) -> dict:
    """Every reusable workflow of this repository a job of `document` calls, as {its
    repository-relative path: every job that calls it, in the file's order}, the files in the
    order the jobs name them."""
    callers: dict = {}
    for job_id, job in (document.get("jobs") or {}).items():
        uses = str((job or {}).get("uses", ""))
        if uses.startswith(LOCAL_WORKFLOW):
            callers.setdefault(uses.removeprefix("./"), []).append(job_id)
    return callers


def called_workflows(document: dict) -> dict:
    """Every reusable workflow of this repository a job of `document` calls, as {its
    repository-relative path: the first job that calls it}, in the order the jobs name them."""
    return {path: jobs[0] for path, jobs in workflow_callers(document).items()}


# The input a workflow several jobs of ci.yaml call reads to run, under each caller, the job of it
# named for that caller and no other: ci-job-test.yaml holds `test` and `tooling-checks`, the
# two halves of what was one job, and ci.yaml's job of each id passes its own id. A file that
# several jobs call and that holds no job named for any of them (dispatch-browser.yaml, which
# dispatch-browser, dispatch-browser-pr and dispatch-references call with a lane each) runs every
# job under every caller.
CALLER_INPUT = "job"


def called_job_owners(callers: list, name: str) -> list:
    """The ci.yaml jobs among `callers` that run job `name` of the workflow they call. A file one
    job calls runs every job of it under that one. A file several call runs a job named for one of
    them under that one alone, since its `if:` admits only that caller's CALLER_INPUT
    (check_called_jobs holds both halves of that), and any other job under every caller."""
    return [name] if len(callers) > 1 and name in callers else list(callers)


def check_called_jobs(documents: dict, problems: list[Annotation]) -> None:
    """A workflow several ci.yaml jobs call is read job by job: each job named for one of its
    callers is read as that caller's alone (called_job_owners), so the rules that hold a job's go
    commands, its environment and its gated steps to its own gate hold it to that caller's. That
    reading is sound only while the job really runs under that caller alone. So, once the file
    holds a job named for any of its callers: each caller must have a job of the file named for
    it, else no job of the file runs under that caller for this check to hold to its gate; that
    job's `if:` must be exactly `inputs.job == '<its id>'`; and the caller must pass
    `job: <its own id>`. A job that ran under a second caller would run go commands and read files
    that caller's filter is never held to."""
    for path, callers in workflow_callers(documents[WORKFLOW]).items():
        if len(callers) < 2:
            continue
        called_jobs = (documents.get(path) or {}).get("jobs") or {}
        named = [name for name in callers if name in called_jobs]
        if not named:
            continue
        for name in callers:
            if name not in called_jobs:
                problems.append(error(WORKFLOW, f"job {name} calls {path}, which holds jobs named for its other "
                                                f"callers ({', '.join(named)}) and none named {name}, so no job of it "
                                                f"runs under {name} that this check can hold to {name}'s gate; give the "
                                                f"file a job named {name}, or call another file"))
                continue
            want_if = f"inputs.{CALLER_INPUT} == '{name}'"
            have_if = squash(str((called_jobs[name] or {}).get("if", "")))
            if have_if != want_if:
                problems.append(error(path, f"job {name} of {path}, which jobs {', '.join(callers)} of {WORKFLOW} all "
                                            f"call, carries `if: {have_if}`: give it `if: {want_if}`, so it runs under "
                                            f"job {name} alone, which is the one caller this check holds its steps to"))
            passed = str((((documents[WORKFLOW].get("jobs") or {}).get(name) or {}).get("with") or {}).get(CALLER_INPUT, ""))
            if passed != name:
                problems.append(error(WORKFLOW, f"job {name} calls {path}, which other jobs of this file call too, and "
                                                f"passes `{CALLER_INPUT}: {passed}`: pass `{CALLER_INPUT}: {name}`, so the "
                                                f"job of {path} named {name} is the one that runs under it"))


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
          "TestEmbedPatterns,XTestEmbedPatterns,Error,Match")


@dataclass
class Session:
    """The go commands' environment and results. GOPROXY=off over an empty module cache: no
    download, no rewrite of a go.sum or go.work.sum, and the same answer on a warm machine as on a
    cold runner. A third-party package then fails to load, which loses nothing first-party
    (check-ci-filters.sh's header says why). The toolchain is the exception that cache cannot
    hold: `toolchain()` resolves it first, under the machine's own cache and proxy, with a `go env
    GOROOT` that writes nothing. GOGC=400: a listing's heap lives only as long as its command, and
    collecting it a quarter as often takes a sixth off the generation's processor time (48 to 40
    s on 4 processors) for a peak of about 230 MB in the largest listing, against 95."""

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
                   GOWORK="", GOTOOLCHAIN="local", GOGC="400")
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
        """The -overlay flag of a run, its file named by what it replaces and written once:
        prefetch writes every one before its pool starts, so no two threads write one file."""
        if not run_.overlay:
            return []
        name = hashlib.sha256(repr(run_.overlay).encode()).hexdigest()[:16]
        path = Path(self.tmp) / f"overlay-{name}.json"
        if not path.exists():
            path.write_text(json.dumps({"Replace": {str(REPO / a): str(REPO / b) for a, b in run_.overlay}}))
        return ["-overlay", str(path)]

    def listing(self, run_: Run) -> list:
        """`go list -deps` of the run, with -test for a run that compiles tests (test, vet). Kept
        by the command it runs, so Runs that differ only in a mode that asks the same listing (a
        test and a vet, a build and a run) share one. `--` ends go's flags before the packages, so
        go reads a package that starts with `-` (a spec's selector can name one) as an import path,
        which it refuses, and never as a flag such as -toolexec, which names a program go runs."""
        args = ["list", "-e", f"-json={FIELDS}", "-deps"]
        if run_.mode in ("test", "vet"):
            args.append("-test")
        if run_.tags:
            args += ["-tags", run_.tags]
        if run_.race:
            args.append("-race")
        args += self.overlay_args(run_) + ["--", *run_.pkgs]
        key = ("list", run_.workdir, run_.env, tuple(args))
        if key not in self.cache:
            self.cache[key] = decode_stream(self.go(run_, args))
        return self.cache[key]

    def named(self, run_: Run) -> list:
        """The packages the run's command names, out of its listing: each entry go list matched to
        one of its patterns (Match), but the test variants -test adds of them (ForTest). These are
        the ones whose test files a test or vet compiles; a package reached only as a dependency
        matches no pattern. The same entries a `go list` without -deps prints, field for field,
        since every field read of them comes from the package's directory and build context."""
        return [pkg for pkg in self.listing(run_) if pkg.get("Match") and not pkg.get("ForTest")]

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

    def prefetch(self, runs) -> int:
        """Run, at once, the go commands closure() asks of each of `runs` - its listings and its
        module files - so the walk that follows reads them from the cache. Each command is
        independent of the others and its answer is keyed by what it asks, so the cache holds
        what a run one after another would; the problems they raise are put in the order of
        their messages, which no scheduling moves. CI_FILTERS_JOBS bounds the pool (default: the
        machine's processors, at most 8; 1 runs them one after another). Returns the pool size."""
        jobs = int(os.environ.get("CI_FILTERS_JOBS") or min(8, os.cpu_count() or 1))
        tasks = []
        for r in dict.fromkeys(runs):
            tasks.append(lambda r=r: self.listing(r))
            tasks.append(lambda r=r: self.module_files(r))
        for r in dict.fromkeys(runs):
            self.toolchain(r.workdir)  # resolved once per directory, before any thread asks
            self.overlay_args(r)  # and each overlay written once, before any thread reads it
        before = len(self.problems)
        if jobs > 1:
            with ThreadPoolExecutor(max_workers=jobs) as pool:
                for future in [pool.submit(task) for task in tasks]:
                    future.result()
        else:
            for task in tasks:
                task()
        self.problems[before:] = sorted(dict.fromkeys(self.problems[before:]), key=lambda a: (str(a.file), a.message))
        return jobs


def rel(path: str) -> str | None:
    """A path as repository-relative text, or None outside the repository. The walk asks this of
    a few hundred package directories tens of thousands of times, so each answer is kept, keyed by
    the repository and, for a relative path, the directory it is read from: the tree does not
    change during a run (check-ci-filters.sh fails one that does)."""
    return resolved_rel(REPO, "" if os.path.isabs(path) else os.getcwd(), path)


@functools.lru_cache(maxsize=None)
def resolved_rel(repo: Path, cwd: str, path: str) -> str | None:
    real, root = os.path.realpath(os.path.join(cwd, path)), str(repo)
    if real == root:
        return "."
    return real[len(root) + 1:] if real.startswith(root + "/") else None


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
    names, files = directory_entries(REPO / dir_rel)
    left_out = {name for name in names if name in tests or name.endswith(TEST_SUFFIX)}
    if not left_out:
        return [Read(f"{dir_rel}/*", why)]
    return [Read(f"{dir_rel}/{name}", why) for name in names if name not in left_out and name in files]


@functools.lru_cache(maxsize=None)
def directory_entries(directory: Path) -> tuple:
    """(the sorted names in a directory, those of them that are files), kept per directory: the
    walk reads one package directory once for each run that holds it, and the tree does not
    change during a run."""
    names = sorted(os.listdir(directory))
    return names, frozenset(name for name in names if (directory / name).is_file())


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
        for d in go_directories(root):
            reads += directory_reads(d, tests[d], f"{run_.why} (the pattern {pattern} reads the directory's .go files)")
    return reads


@functools.lru_cache(maxsize=None)
def go_directories(root: Path) -> tuple:
    """The directories a `<root>/...` pattern reads, repository-relative: each under root that
    holds a .go file, but those go_skips_dir passes over. Kept per root, as directory_entries is."""
    found = []
    for current, subdirs, names in os.walk(root):
        here = Path(current)
        subdirs[:] = [s for s in subdirs if not go_skips_dir(here, s)]
        if any(n.endswith(".go") for n in names):
            found.append(rel(current))
    return tuple(found)



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
    listed = session.listing(run_)
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
        # package reached only as a dependency is in the listing above and not among these.
        for pkg in session.named(run_):
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


TIE_WILD = re.compile(r"[*?\\[]")


def tie_entry(glob: str) -> str | None:
    """The narrowest Tie entry that contains every repository path glob can select: the glob
    itself when it holds no wildcard; the directory before its first wildcard part with `*` when
    that part is the last one and holds no `**` and no `[` class, which stays inside one directory;
    and that directory with `**` otherwise, since a later part, a `**` or a class reaches below it:
    glob_regex reads `**` as any run of characters, `/` included, so `a/b**` selects `a/bc/d`, and
    copies a class into its regex, where `[^x]` matches `/`, so `a/b[^x]c` selects `a/b/c`. None
    when the first part holds a wildcard, which leaves no directory before it."""
    parts = glob.split("/")
    wild = next((i for i, part in enumerate(parts) if TIE_WILD.search(part)), None)
    if wild is None:
        return glob
    if wild == 0:
        return None
    if wild == len(parts) - 1 and "**" not in parts[wild] and "[" not in parts[wild]:
        return "/".join(parts[:wild] + ["*"])
    return "/".join(parts[:wild] + ["**"])


def test_reads(workdir: str, pkgs: list, env: dict) -> tuple:
    """Read the closure of `go test <pkgs>` in workdir for spec-check citations."""
    # ci_job_reads imports this module, so import its rules only after this module loads.
    import ci_job_reads as data

    session = Session()
    why = f"go test {' '.join(pkgs)} in {workdir}"
    tested = run(workdir, pkgs, why, env=env)
    reads = closure(session, data.RULES, tested, set())
    for pkg in session.named(tested):
        if not pkg.get("Dir") and not pkg.get("Standard"):
            cause = " ".join((pkg.get("Error") or {}).get("Err", "").split())[:300]
            session.problems.append(error(None, f"go list finds no directory of {pkg.get('ImportPath')} for {why}, so "
                                                f"its closure is unknown: {cause}"))
    entries = set()
    for read in reads:
        if not read.glob:
            continue
        entry = tie_entry(read.glob)
        if entry is None:
            session.problems.append(error(None, f"{read.glob} has no form of a tie, so the closure of {why} is unknown"))
        else:
            entries.add(entry)
    return sorted(entries), [problem.message for problem in session.problems]


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
        # A glob's every match starts with its text before the first `*`, `?` or `[`, which
        # glob_regex reads as itself, and the files that start with it are one run of these.
        self.ordered = sorted(files)
        self.position = {f: i for i, f in enumerate(files)}
        self.expanded: dict = {}

    def expand(self, glob: str) -> list:
        found = self.expanded.get(glob)
        if found is None:
            found = self.expanded[glob] = self.sweep(glob)
        return found

    def sweep(self, glob: str) -> list:
        if glob.endswith("/*") and not any(c in glob[:-2] for c in "*?["):
            return list(self.by_parent.get(glob[:-2], []))
        if not any(c in glob for c in "*?["):
            return [glob] if glob in self.known else []
        regex, prefix = glob_regex(glob), re.split(r"[*?\[]", glob, maxsplit=1)[0]
        found = []
        for index in range(bisect.bisect_left(self.ordered, prefix), len(self.ordered)):
            f = self.ordered[index]
            if not f.startswith(prefix):
                break
            if regex.fullmatch(f):
                found.append(f)
        return sorted(found, key=self.position.__getitem__)


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


# Where every filter of the changes job is. No file of the repository holds them: the changes job
# computes them when it runs (`check-ci-filters.sh generate`, through GENERATE_SCRIPT, which falls
# open to selecting every job when that fails) into the runner's temporary directory, and its one
# dorny/paths-filter step hands the action that path (rule 21 refuses any other input). This check
# computes the same filters in the same run of the engine and reads them from memory, so the file
# and every rule here are one computation. GENERATED_FILE is how this check's refusals name it.
GENERATED_INPUT = "${{ runner.temp }}/ci-filters.yaml"
GENERATED_FILE = "the generated filters (check-ci-filters.sh generate)"
GENERATE_SCRIPT = ".github/scripts/ci-filters-generate.sh"
GENERATE_RUN = f'{GENERATE_SCRIPT} "$RUNNER_TEMP/ci-filters.yaml"'
# The first line of a filters file GENERATE_SCRIPT wrote because the computation failed: every
# filter `'**'`, so every job runs. A reader of the file (check-ts-filters.sh,
# check-image-trigger-paths.sh) refuses one, since it holds nothing to check a filter against.
FAIL_OPEN_HEAD = "# FAIL-OPEN:"


def filter_input_steps(job: dict) -> list:
    """The dorny/paths-filter steps of one job."""
    return [step for step in (job.get("steps") or [])
            if isinstance(step, dict) and str(step.get("uses", "")).startswith("dorny/paths-filter")]


def reads_generated_file(step: dict) -> bool:
    """Whether a paths-filter step hands the action exactly GENERATED_INPUT, as a plain scalar.
    dorny/paths-filter v4.0.3 takes its `filters` input as the path of a file when the text holds
    no newline and no colon, and as inline YAML otherwise (src/main.ts, isPathInput); the
    expression evaluates to a path with neither, so it is the one form admitted."""
    return (step.get("with") or {}).get("filters") == GENERATED_INPUT


def changes_quantifier(document: dict, problems: list[Annotation]) -> str:
    """The predicate-quantifier the changes job's paths-filter step reads the generated filters
    under. A step that does not hand the action the generated file is rule 21's refusal and is
    passed over here; two that read it under different quantifiers would make one filter mean two
    things, which is refused. "" when no step reads it under one quantifier this check models."""
    steps = filter_input_steps((document.get("jobs") or {}).get("changes") or {})
    if not steps:
        problems.append(error(WORKFLOW, "no changes job with a dorny/paths-filter step"))
        return ""
    declared = set()
    for step in steps:
        if not reads_generated_file(step):
            continue
        quantifier = str((step.get("with") or {}).get("predicate-quantifier", "some"))
        if quantifier not in PathsFilter.QUANTIFIERS:
            problems.append(error(WORKFLOW, f"the changes job's paths-filter step {step.get('id')} has "
                                             f"predicate-quantifier {quantifier}, which this check does not model"))
            continue
        declared.add(quantifier)
    if len(declared) != 1:
        if declared:
            problems.append(error(WORKFLOW, f"the changes job's paths-filter steps read {GENERATED_FILE} under "
                                            f"predicate-quantifier {' and '.join(sorted(declared))}, so one filter of "
                                            f"it means two things; give every such step the same quantifier"))
        return ""
    return declared.pop()


def check_filter_inputs(workflows: dict, problems: list[Annotation]) -> None:
    """Rule 21: every dorny/paths-filter step of every workflow of .github/workflows hands the
    action exactly GENERATED_INPUT, and the job that holds it writes that file first, in a step
    whose run: is exactly GENERATE_RUN. Every filter is computed into that file, so a filter
    written into a workflow's own text, or a file nothing wrote, would gate its job on patterns no
    rule here computed, and nothing would say so."""
    for path, document in workflows.items():
        jobs = document.get("jobs") if isinstance(document, dict) else None
        for job_id, job in (jobs if isinstance(jobs, dict) else {}).items():
            job = job if isinstance(job, dict) else {}
            steps = [s for s in job.get("steps") or [] if isinstance(s, dict)]
            for step in filter_input_steps(job):
                if not reads_generated_file(step):
                    problems.append(error(path, f"the paths-filter step {step_label(step)} of job {job_id} passes a "
                                                f"`filters` input that is not exactly {GENERATED_INPUT}; every filter is "
                                                f"computed into that file, so one declared anywhere else is computed by "
                                                f"no rule of this check and gates its job on whatever it says, unread"))
                    continue
                before = steps[:steps.index(step)]
                if not any(str(s.get("run", "")).strip() == GENERATE_RUN for s in before):
                    problems.append(error(path, f"the paths-filter step {step_label(step)} of job {job_id} reads "
                                                f"{GENERATED_INPUT}, and no step before it runs `{GENERATE_RUN}`, which "
                                                f"writes that file, so the step reads a file nothing wrote"))


# A `steps.<id>.outputs.<name>` of an output of the changes job: which step's verdict it hands on.
STEP_OUTPUT_REF = re.compile(r"steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)")
# The every-job term every output carries
# (.github/ci-notes/ci.md#what-a-dispatched-run-of-a-branch-selects-and-when-to-ask-for-every-job):
# the changes job's scope step writes every_job, which is no filter's verdict and the one other
# step output an output may read. ci-router-outputs.test.sh runs that step and holds what it
# selects; this rule holds that the job has the step and that every output reads it.
SCOPE_STEP_ID = "scope"
SCOPE_OUTPUT = "every_job"
# How every output of the changes job but REF_OUTPUT opens; then `'true' ||` its own term, or
# filter_verdicts' `'{}' ||` the filter step's outputs as JSON.
EVERY_JOB_TERM = f"${{{{ steps.{SCOPE_STEP_ID}.outputs.{SCOPE_OUTPUT} && "


def handed_on(document: dict) -> set:
    """The filters an output of the changes job hands on: every `<name>` of a
    `steps.<a paths-filter step>.outputs.<name>` its outputs read."""
    job = (document.get("jobs") or {}).get("changes") or {}
    filter_steps = {str(s.get("id", "")) for s in filter_input_steps(job)}
    return {name for expression in (job.get("outputs") or {}).values()
            for step_id, name in STEP_OUTPUT_REF.findall(str(expression)) if step_id in filter_steps}


def check_output_steps(document: dict, filters: dict, problems: list[Annotation]) -> None:
    """Rule 19: every `steps.<id>.outputs.<name>` an output of the changes job hands on must name
    the job's dorny/paths-filter step and a filter of the generated file. Such an output is empty
    on every run otherwise, which reads as a filter that never matched. A reference to a
    paths-filter step that reads no generated file is passed over here, since rule 21 refuses that
    step. REF_OUTPUT is no filter's verdict: check_ci_ok holds it and the one step that writes it.
    Nor is the every-job term's SCOPE_STEP_ID.SCOPE_OUTPUT, which a step of the job must write,
    and which an output may read only beside a filter's verdict. The job must have that step, one
    of id SCOPE_STEP_ID that is no paths-filter step, and every output but REF_OUTPUT opens with
    EVERY_JOB_TERM: an output that does not, an older `github.event_name == 'workflow_dispatch' &&
    'true'` term among them, selects its job on every dispatch of a branch, or leaves it off
    main's, and a job without the step has no term to open its outputs with. Both are refused
    here; no other rule of this check sees them.

    LIST_OUTPUT reads `steps.filter.outputs.changes`, the action's own list of the filters that
    matched, which no filter names: it is held here to exactly LIST_OUTPUT_VALUE instead, since a
    list that is not `all` on a run whose filter step is skipped, or on one whose filters fell
    open, skips there every step it gates, and one that is `all` on every dispatch (#5309's
    `github.event_name == 'workflow_dispatch'` term) runs every step on a dispatch of a branch
    whose diff reaches few. The fall-open term reads the generate step's every_filter output, so
    the job must have that step: one of id GENERATE_STEP_ID that runs GENERATE_RUN.

    No filter may have a name the action writes another output under: `changes`, its list, and
    `<filter>_count` and `<filter>_files`, each other filter's count and (under list-files) files
    (src/main.ts, exportResults, at the pinned v4.0.3). The action would write two values under one
    name, and the output would hand on whichever it wrote last. GENERATE_SCRIPT's fallback file
    also leaves out `changes`, so a job gated on a filter of that name would be skipped there."""
    job = (document.get("jobs") or {}).get("changes") or {}
    steps = job.get("steps") or []
    ids = [str(s.get("id", "")) for s in steps if s.get("id")]
    filter_steps = {str(s.get("id", "")): s for s in filter_input_steps(job)}
    if job and (SCOPE_STEP_ID not in ids or SCOPE_STEP_ID in filter_steps):
        problems.append(error(WORKFLOW, f"the changes job has no step of id {SCOPE_STEP_ID} that writes "
                                        f"{SCOPE_OUTPUT}, the every-job term `{EVERY_JOB_TERM}` every output but "
                                        f"{REF_OUTPUT} opens with: without it a dispatch of main, a tag's or one "
                                        f"given every-job selects no job by that term, and an output written "
                                        f"instead with `github.event_name == 'workflow_dispatch'` selects every job "
                                        f"on every dispatch of a branch, whatever its diff touches; give the job "
                                        f"the step \"Read whether this run selects every job\""))
    outputs = job.get("outputs") or {}
    for name in sorted(filters):
        if name == LIST_SOURCE:
            problems.append(error(GATES_DIR, f"a file of {GATES_DIR} declares a filter named {LIST_SOURCE}, and "
                                             f"dorny/paths-filter writes its list of the filters that matched "
                                             f"under that name only while no filter has it, so the changes job's "
                                             f"output {LIST_OUTPUT} would hand on that one filter's verdict, and the "
                                             f"fallback file of {GENERATE_SCRIPT} leaves the name out"))
        for other in sorted(filters):
            for suffix in ("_count", "_files"):
                if other != name and name == other + suffix:
                    problems.append(error(GATES_DIR, f"a file of {GATES_DIR} declares a filter named {name}, and "
                                                     f"dorny/paths-filter writes filter {other}'s {suffix[1:]} under "
                                                     f"that name too, so an output reading "
                                                     f"steps.filter.outputs.{name} hands on whichever it wrote last"))
    if LIST_OUTPUT in outputs:
        value = " ".join(str(outputs[LIST_OUTPUT]).split())
        if value != LIST_OUTPUT_VALUE:
            problems.append(error(WORKFLOW, f"the changes job's output {LIST_OUTPUT} is {value}, and it must be "
                                            f"exactly {LIST_OUTPUT_VALUE}: the paths filter's list of the filters "
                                            f"that matched, all on a run that selects every job, whose filter step "
                                            f"is skipped, and all on a run whose filters fell open, whose file holds "
                                            f"no filter a step reads out of the list, so that every step it gates "
                                            f"runs there, while a dispatch of a branch hands on its own list"))
        generate = [s for s in steps if str(s.get("id", "")) == GENERATE_STEP_ID]
        if len(generate) != 1 or str(generate[0].get("run", "")).strip() != GENERATE_RUN:
            problems.append(error(WORKFLOW, f"the changes job's output {LIST_OUTPUT} reads "
                                            f"steps.{GENERATE_STEP_ID}.outputs.{EVERY_FILTER_OUTPUT}, and the job has "
                                            f"no one step of id {GENERATE_STEP_ID} that runs `{GENERATE_RUN}`, which "
                                            f"writes it, so the list is not all on a run whose filters fell open"))
        # A step reads its filter out of the list as `contains(<list>, '"<name>"')`, which holds
        # only while JSON.stringify writes the name as it is and no other name is it in another
        # case: GitHub's `contains` ignores case, so `"App"` would run app's steps.
        for name in sorted(filters):
            if not re.fullmatch(r"[A-Za-z0-9_-]+", name):
                problems.append(error(GATES_DIR, f"a file of {GATES_DIR} declares a filter named {name!r}: a step "
                                                 f"reads its filter out of {LIST_OUTPUT} by its name in quotes, so a "
                                                 f"name is letters, digits, _ and - alone, which JSON writes as they are"))
        by_case = defaultdict(list)
        for name in filters:
            by_case[name.lower()].append(name)
        for names in by_case.values():
            if len(names) > 1:
                problems.append(error(GATES_DIR, f"the files of {GATES_DIR} declare filters {', '.join(sorted(names))}, "
                                                 f"one name in different cases: GitHub's contains() ignores case, so a "
                                                 f"step reading one of them out of {LIST_OUTPUT} runs when any matched"))
    for output, expression in outputs.items():
        if output == REF_OUTPUT:
            continue
        text = str(expression).strip()
        if not text.startswith(EVERY_JOB_TERM):
            problems.append(error(WORKFLOW, f"the changes job's output {output} does not open with the every-job "
                                            f"term `{EVERY_JOB_TERM}`, so a dispatch of main or one given every-job "
                                            f"does not select its job by that step, and a term of its own (an older "
                                            f"`github.event_name == 'workflow_dispatch' && 'true'`) selects it on "
                                            f"every dispatch of a branch whatever its diff touches; open it with "
                                            f"that term, as every other output does"))
        refs = STEP_OUTPUT_REF.findall(str(expression))
        # A filter step's outputs read whole (filter_verdicts' toJSON(steps.filter.outputs)) are its verdicts too.
        read_steps = re.findall(r"steps\.([A-Za-z0-9_-]+)\.outputs\b", str(expression))
        if ((SCOPE_STEP_ID, SCOPE_OUTPUT) in refs and SCOPE_STEP_ID not in filter_steps
                and not any(step_id in filter_steps for step_id in read_steps)):
            problems.append(error(WORKFLOW, f"the changes job's output {output} reads "
                                            f"steps.{SCOPE_STEP_ID}.outputs.{SCOPE_OUTPUT} and no filter's verdict, so "
                                            f"it is true only on a run that selects every job and false on every "
                                            f"pull request, merge group and dispatch of a branch; give it the "
                                            f"filter it stands for after the every-job term"))
        for step_id, name in refs:
            if output == LIST_OUTPUT and name == LIST_SOURCE and step_id in filter_steps:
                continue  # the action's list, held above
            if output == LIST_OUTPUT and step_id == GENERATE_STEP_ID and name == EVERY_FILTER_OUTPUT:
                continue  # the fall-open term, held above with the step that writes it
            if step_id not in ids:
                problems.append(error(WORKFLOW, f"the changes job's output {output} reads "
                                                f"steps.{step_id}.outputs.{name}, and the job has no step {step_id}, "
                                                f"so the output is empty on every run"))
            elif step_id == SCOPE_STEP_ID and name == SCOPE_OUTPUT and step_id not in filter_steps:
                continue
            elif step_id not in filter_steps:
                problems.append(error(WORKFLOW, f"the changes job's output {output} reads "
                                                f"steps.{step_id}.outputs.{name}, and step {step_id} is no "
                                                f"dorny/paths-filter step of the job, so it writes no filter's verdict "
                                                f"and the output is empty on every run"))
            elif reads_generated_file(filter_steps[step_id]) and name not in filters:
                problems.append(error(WORKFLOW, f"the changes job's output {output} reads "
                                                f"steps.{step_id}.outputs.{name}, and {GENERATED_FILE} hold no filter "
                                                f"{name}, since no file of .github/ci-gates declares one, so the output "
                                                f"is empty on every run"))


GATE_REF = re.compile(r"needs\.changes\.outputs\.([A-Za-z0-9_-]+)")

# The one output of the changes job that carries a list rather than one filter's verdict:
# dorny/paths-filter's own `changes` output, the JSON array of every filter that matched
# (`["app","app_step"]`, written by JSON.stringify), or `all` on a run that selects every job,
# whose filter step is skipped. A step reads it in one shape alone, the whole of its
# condition: `needs.changes.result != 'success' || <list> == 'all' || contains(<list>,
# '"<filter>"')`, with more `|| contains(<list>, '"<filter>"')` terms for a step that runs when any
# of several filters matched (the test job's fetch of the computed filters, for the two harnesses
# that read them). The quotes make the `contains` match one whole name of the array, never a name
# another name ends or begins with. It lets one `with:` line hand a called file every step gate of
# a job, where an output and a `with:` line per gate cost ci.yaml about 197 bytes each
# (.github/ci-notes/ci.md#one-list-for-the-test-jobs-step-gates).
LIST_OUTPUT = "matched_filters"
# The step of the changes job that runs GENERATE_RUN, and the output it writes `true` to when it
# hands the paths filter a fallback file (every filter `'**'`, written by GENERATE_SCRIPT's
# every_filter on FAIL-OPEN and FORCED), which holds no filter a step reads out of the list.
GENERATE_STEP_ID = "generate"
EVERY_FILTER_OUTPUT = "every_filter"
# The list, opened with the every-job term as every other output is: a dispatch of main, a tag's or
# one given every-job hands on `all`, as does a run whose filters fell open, while a dispatch of a
# branch hands on the list its filter step wrote over its diff with its merge base with main, as a
# merge group's does
# (.github/ci-notes/ci.md#what-a-dispatched-run-of-a-branch-selects-and-when-to-ask-for-every-job).
LIST_OUTPUT_VALUE = (f"{EVERY_JOB_TERM}'all' || steps.{GENERATE_STEP_ID}.outputs.{EVERY_FILTER_OUTPUT} "
                     f"== 'true' && 'all' || steps.filter.outputs.changes }}}}")
# The name dorny/paths-filter writes its list under, which it writes only while no filter has it.
LIST_SOURCE = "changes"
# A condition that names the list at all, in any spelling (an input's `matched-filters`, an
# index, another case), before or after its caller's values are put in.
LIST_MENTION = re.compile(r"matched[_-]filters", re.I)
# Each filter a `contains` reads out of the list: the one pattern the shape below and
# gated_filters, which rules 2 and 3 and every other reader of a step's gate call, both read the
# filter's name with. Only the spaces and the spelling of `contains`, which GitHub reads in any
# case, are free.
LIST_GATE_TEXT = (rf"(?i:contains)\s*\(\s*needs\.changes\.outputs\.{LIST_OUTPUT}\s*,\s*"
                  r"'\"([A-Za-z0-9_-]+)\"'\s*\)")
LIST_GATE = re.compile(LIST_GATE_TEXT)
# The one shape, the whole condition, after its caller's values are put in: one `contains` or more,
# joined by `||`.
LIST_SHAPE = re.compile(
    rf"^\s*(?:\$\{{\{{)?\s*needs\.changes\.result\s*!=\s*'success'\s*\|\|\s*"
    rf"needs\.changes\.outputs\.{LIST_OUTPUT}\s*==\s*'all'\s*\|\|\s*"
    rf"{LIST_GATE_TEXT}(?:\s*\|\|\s*{LIST_GATE_TEXT})*\s*(?:\}}\}})?\s*$")


def gated_filters(condition: str) -> list:
    """The filters a condition, resolved to ci.yaml's expressions, gates on: each output of the
    changes job it tests, which carries the one filter of its name, and each filter a `contains`
    reads out of LIST_OUTPUT. LIST_OUTPUT itself is no filter, and is not one of them. The one
    reader of a gate's filter for every rule here: a rule that reads a step's or a job's gate
    reads it through this, so the two shapes a gate takes are read the same everywhere."""
    return [name for name in GATE_REF.findall(condition) if name != LIST_OUTPUT] + LIST_GATE.findall(condition)


def list_refusals(condition: str, path: str, job_id: str, step) -> list:
    """Rule 23, its first half: a condition, resolved to ci.yaml's expressions, that names
    LIST_OUTPUT is a step's, and the whole of it is LIST_SHAPE. Any other condition holding the
    list gates on what no rule here can read: `<list> != ''` is true on every run, a `!` before the
    `contains` or an `&& false` after it runs the step when its filter did not match or never, a
    `contains` without its quotes matches a name that holds the filter's, a condition without the
    `all` term is skipped on every run that selects every job, and one without the result term on
    every run whose changes failed. A job-level read is refused too: ci-ok's CI_OK_SELECTED_BY maps
    a job to the output that is 'true' when changes selects it, and the list is never 'true'."""
    if not LIST_MENTION.search(condition):
        return []
    where = where_if(job_id, step)
    if step is None:
        return [error(path, f"{where}'s if: reads needs.changes.outputs.{LIST_OUTPUT}, a list that gates steps: "
                            f"ci-ok's CI_OK_SELECTED_BY maps a job to the changes output that is 'true' when it "
                            f"selects the job, and the list is never 'true', so gate the job on an output of its own")]
    if LIST_SHAPE.match(condition):
        return []
    return [error(path, f"{where} reads needs.changes.outputs.{LIST_OUTPUT} outside its one shape, the whole "
                        f"condition needs.changes.result != 'success' || needs.changes.outputs.{LIST_OUTPUT} == "
                        f"'all' || contains(needs.changes.outputs.{LIST_OUTPUT}, '\"<filter>\"'), with more "
                        f"`|| contains(...)` terms or none, so neither which filter gates it nor that it runs "
                        f"exactly when that filter matched can be read")]


# Every `needs` and `inputs` a condition or a `with:` value names, in any case and in either
# GitHub spelling, `.name` or `['name']`: what spelling_refusals reads.
NEEDS_TERM = re.compile(r"(?<![A-Za-z0-9_.-])needs\s*(?:\.\s*([A-Za-z0-9_-]+)|\[\s*['\"]([^'\"]*)['\"]\s*\])?", re.I)
INPUTS_TERM = re.compile(r"(?<![A-Za-z0-9_.-])inputs\s*(\.\s*[A-Za-z0-9_-]+|\[)?", re.I)
# The spellings of the changes job this check reads: its result, an output by name, and the job
# whole (ci.yaml's ci-ok payload, `toJSON(needs.changes)`).
CHANGES_READ = re.compile(r"needs\.changes(?:\.result|\.outputs\.[A-Za-z0-9_-]+)?(?![A-Za-z0-9_.\[-])")


def spelling_refusals(text: str, path: str, where: str) -> list:
    """Rule 23, its second half: a condition, a `with:` value or any other `${{ }}` of a job names
    the changes job only as `needs.changes.result`, `needs.changes.outputs.<name>` or
    `needs.changes` whole, and an input only as `inputs.<name>`. GitHub reads
    `needs['changes'].outputs['x']`, `NEEDS.Changes`, `INPUTS.x` and `inputs['x']` as the same
    values, and `toJSON(inputs)` as every input at once, and no rule here reads any of them: a gate
    so spelt would be held to nothing, and a list read so spelt would escape the rest of rule 23."""
    refusals = []

    def spelt(term) -> str:
        # The term and the chain of indexes and properties after it, so the refusal quotes the
        # read as written (`needs.changes.outputs['x']`, not `needs.changes`).
        return term.group(0) + re.match(r"[^\s,)}|&!=]*", text[term.end():]).group(0)

    for term in NEEDS_TERM.finditer(text):
        job = (term.group(1) or term.group(2) or "").lower()
        if job != "changes":
            continue
        if not CHANGES_READ.match(text, term.start()):
            refusals.append(error(path, f"{where} names the changes job as {spelt(term)}: this check reads it spelt "
                                        f"needs.changes.result, needs.changes.outputs.<name> or needs.changes, so a "
                                        f"read spelt otherwise is held to nothing"))
    for term in INPUTS_TERM.finditer(text):
        # Exactly `inputs.<name>`: no space about the dot, which GitHub reads past and INPUT_REF,
        # the reference resolve_condition rewrites, does not.
        if not re.fullmatch(r"inputs\.[A-Za-z0-9_-]+", term.group(0)):
            refusals.append(error(path, f"{where} reads an input as {spelt(term)}: this check resolves an input "
                                        f"spelt inputs.<name> alone, so a read spelt otherwise is held to nothing"))
    return refusals


# A read of a value carrying more than one filter's verdict, once its caller's values are put
# in: the list of matched filters, the paths filter's verdict object (filter_verdicts), the
# changes job whole (ci.yaml's ci-ok payload, `toJSON(needs.changes)`, which holds both), every
# job's outputs at once (`toJSON(needs)`) and every job's outputs by the `*` filter
# (`join(needs.*.outputs.matched_filters, ',')`), which a job's result alone (`needs.*.result`)
# is not.
CARRIER = re.compile(
    rf"needs\.changes\.outputs\.(?:{LIST_OUTPUT}|filter_verdicts)(?![A-Za-z0-9_-])"
    r"|needs\.changes(?![A-Za-z0-9_.\[-])"
    r"|(?<![A-Za-z0-9_.-])needs(?![A-Za-z0-9_.\[-])"
    r"|(?<![A-Za-z0-9_.-])needs\s*\.\s*\*(?!\s*\.\s*result(?![A-Za-z0-9_-]))", re.I)
# The one test of the payload a condition may make: whether ci.yaml handed it on at all, which
# docs-site.yaml's and dispatch-browser.yaml's report steps ask, `inputs.ci-ok-changes != ''`.
PAYLOAD_PRESENCE = re.compile(r"(?i:tojson)\s*\(\s*needs\.changes\s*\)\s*(?:!=|==)\s*''")
# The one reader of the payload outside a condition: the ci-ok report action, which posts the
# verdict of a merge-group run from it (`with: changes:`).
CI_OK_REPORT_ACTION = "./.github/actions/ci-ok-report"
EXPRESSION = re.compile(r"\$\{\{(.*?)\}\}", re.S)


def carrier_condition_refusals(condition: str, path: str, job_id: str, step) -> list:
    """Rule 23, its third half, over conditions: a condition, resolved to ci.yaml's expressions,
    that reads a CARRIER other than the list (which list_refusals holds to its shape) reads only
    the payload, and only as PAYLOAD_PRESENCE. The payload and the verdict object name every
    filter on every run, so `contains(inputs.ci-ok-changes, 'apx')` gates a step on a filter no
    rule reads, and is true on every run besides."""
    if LIST_MENTION.search(condition) or not CARRIER.search(PAYLOAD_PRESENCE.sub("", condition)):
        return []
    return [error(path, f"{where_if(job_id, step)} reads the changes job's verdicts other than as the list of "
                        f"matched filters in its one shape: the ci-ok payload (toJSON(needs.changes)), "
                        f"filter_verdicts and toJSON(needs) name every filter on every run, so a condition reading "
                        f"them for a filter gates on one no rule reads; test the payload only as != ''")]


def texts(value, at: tuple = ()):
    """Every string in a workflow's value, with the keys that lead to it."""
    if isinstance(value, dict):
        for key, inner in value.items():
            yield from texts(inner, at + (str(key),))
    elif isinstance(value, list):
        for index, inner in enumerate(value):
            yield from texts(inner, at + (index,))
    elif isinstance(value, str):
        yield at, value


def carrier_text_refusals(documents: dict, units: list) -> list:
    """Rule 23, its third half, outside conditions: no `run:`, `env:`, `with:` or other text of a
    job of ci.yaml or of a workflow it calls reads a CARRIER, once its caller's `with:` values are
    put in for its inputs (in any case, as GitHub reads them), but in three places: ci.yaml's own
    `with:` to a workflow it calls, which hands the values on to the conditions read there; the
    ci-ok report action's `changes:`, which posts ci-ok's verdict from the payload; and ci.yaml's
    ci-ok job, which is that verdict. A script that reads the list or the payload can test it
    for any filter, so its work is gated on filters no rule reads (`case '${{
    inputs.matched-filters }}' in *apx*)`)."""
    ci_jobs = documents[WORKFLOW].get("jobs") or {}
    refusals, said = [], set()
    scanned = []  # (path, owner, the job's name or None for the file's own env/defaults, value)
    for owner, path, job_id, document, job in units:
        if path == WORKFLOW and job_id == VERDICT_JOB:
            continue
        body = {key: value for key, value in job.items() if key != "if" and not (path == WORKFLOW and key == "with")}
        scanned.append((path, owner, job_id, body))
        if path != WORKFLOW:
            scanned.append((path, owner, None, {key: document.get(key) for key in ("env", "defaults") if key in document}))
    for path, owner, job_id, body in scanned:
        passed = {str(key).lower(): str(value) for key, value in ((ci_jobs.get(owner) or {}).get("with") or {}).items()}

        def put_in(match):
            value = passed.get(match.group(1).lower())
            if value is None:
                return match.group(0)
            inner = EXPRESSION.fullmatch(value.strip())
            return f"({inner.group(1)})" if inner else value

        steps = body.get("steps") or []
        for at, text in texts(body):
            if at and at[-1] == "if":
                continue  # a condition: list_refusals and carrier_condition_refusals read it
            step = steps[at[1]] if len(at) > 2 and at[0] == "steps" and isinstance(at[1], int) else None
            where = (f"{where_if(job_id, step)}'s {'.'.join(str(k) for k in at[2:] if not isinstance(k, int))}"
                     if step is not None else
                     f"{f'job {job_id}' if job_id else 'the file'}'s {'.'.join(str(k) for k in at if not isinstance(k, int))}")
            for expression in EXPRESSION.findall(text):
                # Rule 23's second half over every scanned expression: a carrier read under a spelling
                # this check does not read (`inputs['x']`, `needs['changes']`) would escape the
                # carrier test below as it would escape a condition's.
                messages = [refusal.message for refusal in spelling_refusals(expression, path, where)]
                resolved = INPUT_REF.sub(put_in, expression) if path != WORKFLOW else expression
                exempt = (step is not None and at[2:] == ("with", "changes")
                          and str(step.get("uses", "")).split("@")[0] == CI_OK_REPORT_ACTION)
                if CARRIER.search(resolved) and not exempt:
                    messages.append(f"{where} reads ${{{{ {expression.strip()} }}}}, which carries every filter's "
                                    f"verdict: a script or value that reads the list of matched filters, "
                                    f"filter_verdicts or the ci-ok payload can test it for any filter, so its work is "
                                    f"gated on filters no rule reads; gate the step in its if: (rule 23's one shape) "
                                    f"instead")
                for message in messages:
                    if (path, message) not in said:
                        said.add((path, message))
                        refusals.append(error(path, message))
    return refusals


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
# An output of the changes job carries the one filter it names, but for LIST_OUTPUT, which
# carries every filter that matched and is read one filter at a time (gated_filters);
# `needs.changes.result` carries none, so `inputs.changes-result != 'success' || inputs.go_notices
# == 'true'` reads as exactly what the same test of `.github/workflows/ci.yaml#needs.changes.result`
# and `.github/workflows/ci.yaml#needs.changes.outputs.go_notices` would read as - one gate, on
# go_notices - and the test job's `inputs.changes-result != 'success' || inputs.matched-filters ==
# 'all' || contains(inputs.matched-filters, '"release_push_test"')` as one gate, on
# release_push_test. The third is ci.yaml's ci-ok payload, the whole changes job as JSON:
# docs-site.yaml's report step reads it (`if: always() && inputs.ci-ok-changes != ''`, which asks
# whether ci.yaml is the caller rather than what changed). It carries the list of matched filters
# and filter_verdicts, so it names every filter: carrier_condition_refusals holds a condition to
# reading it only as `!= ''`, and carrier_text_refusals holds every other reader to the ci-ok
# report action (rule 23).
WITH_CHANGES = re.compile(r"^\$\{\{\s*(needs\.changes\.outputs\.[A-Za-z0-9_-]+|needs\.changes\.result"
                          r"|toJSON\(needs\.changes\))\s*\}\}$")
# A value that names the changes job at all, in any shape: what the refusal below is about. One
# that does not cannot carry a filter, so the condition reading it gates on something other than
# the paths filter and this rule leaves it alone - `.github/workflows/dispatch-browser.yaml#lane`, which each of
# its three callers in ci.yaml passes as a literal and three of whose jobs run only when it reads
# `full`, chooses which projects the browser suite runs, never whether it runs; the
# `dispatch_browser`, `dispatch_browser_pr` and `dispatch_references` outputs decide that, on the
# caller jobs' own `if:`.
NAMES_CHANGES = re.compile(r"\bneeds\.changes\b")


def input_expressions(documents: dict, problems: list[Annotation]) -> dict:
    """Per workflow ci.yaml calls and per job of ci.yaml that calls it, {an input a condition of
    that workflow reads: the expression that calling job passes for it}, keyed (the workflow, the
    calling job): a file several jobs call (ci-job-test.yaml, dispatch-browser.yaml) is read under
    each caller's own `with:`. The resolution may never be guessed: an input a
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
    for path, callers in workflow_callers(documents[WORKFLOW]).items():
        document = documents.get(path)
        if document is None:
            continue  # workflow_documents has already refused a `uses:` naming no file of the repository
        # Names lowered: GitHub reads an input name in any case, so `inputs.GATES` reads the input
        # a caller passes as `gates`, and is resolved here as that input.
        read = {name.lower() for job in (document.get("jobs") or {}).values()
                for _, condition in conditions(job or {}) for name in INPUT_REF.findall(condition)}
        for caller in callers:
            calling = (documents[WORKFLOW].get("jobs") or {}).get(caller) or {}
            # Keyed by the text of each name, since that is what a condition's `inputs.<name>` is:
            # a `with:` key YAML read as something else (a bare `on`, a bare `true`) is still a
            # name here rather than a lookup that raises.
            passed = {str(key).lower(): value for key, value in (calling.get("with") or {}).items()}
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
            expressions[(path, caller)] = resolved
    return expressions


def resolve_condition(condition: str, expressions: dict) -> str:
    """A condition as its caller's values make it: every `inputs.<name>` the caller passes a
    value for becomes the expression that value holds, its name read in any case as GitHub reads
    it, and every other reference stands. The reference is matched whole, so a name holding a `-`
    (changes-result) resolves, and a longer identifier that merely starts with an input's name
    (inputs.go_notices_extra beside an input go_notices) is left alone rather than rewritten."""
    return INPUT_REF.sub(lambda m: expressions.get(m.group(1).lower(), m.group(0)), condition)


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
# eighteen names. A list holding any other pattern (`TestA.*`) is quoted like any other line,
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
# among them every other GO* name and CGO_*, BASH_ENV and ENV (which bash sources before the
# script), HOME and XDG_CONFIG_HOME (go reads its env file, GOFLAGS among it, from
# $XDG_CONFIG_HOME/go/env or else $HOME/.config/go/env) and PATH (which chooses the go binary).
# Add a name here only when go compiles the same files with it set as without, and runs the same
# tests.
#
# GOPROXY and GOCACHE are the two names go itself reads that are here. GOCACHE says where go
# keeps what it built (its line below says why that changes nothing go compiles). GOPROXY says
# where a module's bytes come from, never which version: the module files this check already
# reads decide that, and go.sum holds each module's hash whichever source serves it. The go
# commands that compute closures run with GOPROXY=off over an empty module cache (Session.go), so
# a workflow's value never reaches them either. CI_OK_SELECTED_BY, CI_OK_PULL_REQUEST_ONLY,
# CI_OK_ALWAYS_RUN and CI_OK_EXEMPT are
# ci.yaml's ci-ok classes: ci-ok-report.sh reads the first three out of that file and hands them
# to ci-ok.sh, and CI_OK_EXEMPT is read by this check alone. LEGION_RUN_FUZZ_ITERS and
# LEGION_KEPT_PROPERTY_RUNS are the counts the pull-request lane reduces: each is read at run
# time by the test it governs, so go compiles the same files with them set as without.
ENV_ALLOWED = frozenset({
    "AIRLOCK_MODEL",
    "BROKER_TEST_DATABASE_URL", "CI_OK_ALWAYS_RUN", "CI_OK_EXEMPT", "CI_OK_PULL_REQUEST_ONLY", "CI_OK_SELECTED_BY",
    # The file of the filters this run's changes job computed, which the lint job hands
    # check-image-trigger-paths.sh and check-ts-filters.sh. around_job reads the env of every step
    # of the job that runs go-release-notices.sh, so without this name here every go command of
    # that script would count as unreduced and go_notices would have to match every Go file of the
    # repository again. go reads it nowhere.
    "CI_FILTERS_FILE",
    "DATABASE_URL",
    "DISPATCH_E2E_FILE_STORE_BUCKET", "DISPATCH_E2E_FILE_STORE_ENDPOINT", "DISPATCH_FIRST_ROLLOUT_BUILD",
    "DISPATCH_PREVIOUS_BUILD", "DISPATCH_TEST_DATABASE_URL",
    # Read at run time by storetest's xid window rig (XidRequireGapsEnv): it turns a run that
    # cannot judge gaps from shape alone into a failure, and go compiles the same files and runs
    # the same tests with it set as without.
    "DISPATCH_XID_REQUIRE_GAP_JUDGMENT",
    "ENVOY_INFRA_DOCKER_PLUGIN", "ENVOY_SMOKE_HELD_PORT_IMAGE",
    "ENVOY_SMOKE_IMAGE", "FIRST_ROLLOUT_COMMIT",
    # Where go keeps what it compiled and the test results it caches, both found by a hash of
    # their inputs, so go compiles the same files and runs the same tests whatever directory it
    # names. envoy-go-build's darwin vet sets it in its step's env, so that vet's darwin objects
    # land in a directory of their own, which the go-envoy-darwin-vet cache family holds.
    "GOCACHE",
    "GOPROXY",
    # The job's state, success, failure or cancelled, which the step "Wait for the script
    # harnesses and judge them" of the test job and envoy-go-checks hands .github/scripts/
    # harness-pool.sh to choose --allow-none and --now. go reads it nowhere.
    "JOB_STATUS",
    # The job's timeout-minutes, which .github/scripts/gh-read.sh's wait for a spent API budget
    # stays within. Every job whose steps call GitHub's API sets it in its own env, the jobs that
    # run go commands among them, and around_job reads a job's env. go reads it nowhere.
    "JOB_TIMEOUT_MINUTES",
    "LEGION_E2E", "LEGION_E2E_DISPATCH_STACK", "LEGION_E2E_ENVOY_STACK", "LEGION_E2E_PG_DSN", "LEGION_KEPT_PROPERTY_RUNS",
    "LEGION_RUN_FUZZ_ITERS", "LEGION_TEST_NATS_URL", "LEGION_TEST_PG_DSN",
    # The commit envoy-go-build's edited-migrations step compares a bypass run's tree with:
    # .github/scripts/edited-migrations-base.sh, which that step runs, reads it and hands the base
    # it picks to the command as an argument, and go reads it nowhere.
    "MIGRATION_BASE",
    "PREVIOUS_COMMIT",
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
    itself, and, when it calls a local workflow, each job of that workflow it runs
    (called_job_owners). The documents are workflow_documents' set, which holds each called file
    once however many jobs name it; a job of a file several jobs call is listed once per caller
    that runs it, since each caller's gate is held to it."""
    ci = documents[WORKFLOW]
    units = [(job_id, WORKFLOW, job_id, ci, job or {}) for job_id, job in (ci.get("jobs") or {}).items()]
    for path, callers in workflow_callers(ci).items():
        called = documents.get(path) or {}
        units += [(owner, path, name, called, called_job or {})
                  for name, called_job in (called.get("jobs") or {}).items()
                  for owner in called_job_owners(callers, name)]
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


def around_job(units: list, owner: str) -> str:
    """The first thing around a ci.yaml job that can change what a go command of a script it runs
    compiles, over every step of the job and of each job of a workflow it calls, each read under
    the env:, defaults and container of the file that holds it; or ''. Every step, since which one
    starts the script is nothing this check can prove: a step can start it through another
    script, from another directory or by a test it compiles, and a step that names its path may
    only chmod or lint it. So a write after the last step that shows a start still counts: the
    check errs wide, never narrow."""
    for own, _, _, holder, job in units:
        reason = around_reason(holder, job, job.get("steps") or []) if own == owner else ""
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
GATES_DIR = ".github/ci-gates"


def declared(g: Gate) -> str:
    """The file of .github/ci-gates that declares a gate, which every refusal about the gate
    names: the loader appends that file to the gate's reads last (ci_filter_sources.load_gate_files),
    after any read of the directory the gate declares itself."""
    return next((r.glob for r in reversed(g.reads) if r.glob.startswith(f"{GATES_DIR}/")
                 and r.glob.endswith(".py") and "*" not in r.glob), ".github/scripts/ci_job_reads.py")


def check_table(documents: dict, data, files: set, problems: list[Annotation]) -> dict:
    """The table must say what ci.yaml says: every gate a filter the changes job hands on to a
    real job, every go command a job's run: steps start listed, every pin and citation still
    found. `documents` is
    ci.yaml and every workflow its jobs call (workflow_documents), since a gate's job may hold
    its steps in a called file: the caller job in ci.yaml keeps the id and the `if:`, so the
    filter test is read off ci.yaml, and the steps are read off that file, which step_holders
    pairs back with the ci.yaml job that runs them. Returns, per
    filter, each claimed command it does not reduce: (Run, the command as the job runs it, why)."""
    caller_jobs = documents[WORKFLOW].get("jobs") or {}
    gates = {g.filter: g for g in data.GATES}
    excluded = dict(data.EXCLUDED)
    outputs = handed_on(documents[WORKFLOW])
    for name in sorted(set(gates) & set(excluded)):
        problems.append(error(GATES_DIR, f"{name} is both a gate and EXCLUDED"))
    # What each called file's conditions gate on, which is the caller's to say: a condition of
    # ci.yaml names its filter outright, one of a called file names an input the caller passes
    # that filter's output in.
    inputs_of = input_expressions(documents, problems)
    # Rule 3 over every document of the set: a called file's job or step is gated through an
    # input of its own, resolved to the expression the caller that runs it passes, and the
    # refusal names the file the condition is in. REF_OUTPUT is no filter: the condition reading
    # it is check_ci_ok's. Rule 23 over the same conditions: one reading LIST_OUTPUT reads it in
    # its one shape, each refusal said once however many callers run the file.
    units = step_holders(documents)
    gated = defaultdict(set)  # (workflow, job) -> filters its job-level if: names
    refused = set()
    for own, path, job_id, _, job in units:
        for step, condition in conditions(job):
            resolved = resolve_condition(condition, inputs_of.get((path, own), {}))
            for refusal in (spelling_refusals(condition, path, where_if(job_id, step))
                            + list_refusals(resolved, path, job_id, step)
                            + carrier_condition_refusals(resolved, path, job_id, step)):
                if (path, refusal.message) not in refused:
                    refused.add((path, refusal.message))
                    problems.append(refusal)
            for name in gated_filters(resolved):
                if name == REF_OUTPUT:
                    continue
                if step is None:
                    gated[(path, job_id)].add(name)
                where = where_if(job_id, step)
                if name not in gates and name not in excluded and (path, where, name) not in refused:
                    refused.add((path, where, name))
                    problems.append(error(path, f"{where} is gated on filter {name}, which no file of "
                                                f"{GATES_DIR} declares as a gate or as EXCLUDED; add the gate "
                                                f"with its reads"))
    # Rule 23's spelling half over the values ci.yaml's jobs hand the workflows they call, which
    # are what a called file's conditions are resolved through.
    for job_id, job in caller_jobs.items():
        for name, value in ((job or {}).get("with") or {}).items():
            for refusal in spelling_refusals(str(value), WORKFLOW, f"job {job_id}'s with: {name}"):
                if (WORKFLOW, refusal.message) not in refused:
                    refused.add((WORKFLOW, refusal.message))
                    problems.append(refusal)
    # Rule 23's third half outside conditions: no script, value or input of a step reads the list,
    # the verdict object or the payload but in the places that hand them on or report from them.
    problems.extend(carrier_text_refusals(documents, units))
    # A step that reads its filter out of the list of matched filters has it handed on by that
    # list, which names every filter of the generated file that matched (rule 19 holds the output
    # to its one value): only a gate read as an output of its own needs that output.
    list_output = LIST_OUTPUT in (((caller_jobs.get("changes") or {}).get("outputs")) or {})
    for g in data.GATES:
        via_list = list_output and bool(g.step) and any(
            step_matches(s, g.step) and g.filter in LIST_GATE.findall(
                resolve_condition(str(s.get("if", "")), inputs_of.get((g.workflow, g.job), {})))
            for s in (((documents.get(g.workflow) or {}).get("jobs") or {}).get(g.job) or {}).get("steps") or [])
        if g.filter not in outputs and not via_list:
            problems.append(error(WORKFLOW, f"{g.job} has no filter {g.filter} in the changes job: no output of it "
                                            f"hands on the paths-filter step's {g.filter}, so the job's if: reads an "
                                            f"empty output"))
        # The caller job is ci.yaml's, whichever file holds the steps: it keeps the job id and
        # the `if:` that tests the filter, so both are looked for there.
        if g.job not in caller_jobs:
            problems.append(error(declared(g), f"gate {g.filter} names job {g.job}, which {WORKFLOW} does not have"))
            continue
        steps_document = documents.get(g.workflow)
        if steps_document is None:
            problems.append(error(declared(g), f"gate {g.filter} holds job {g.job}'s steps in {g.workflow}, which "
                                               f"{WORKFLOW} neither is nor calls"))
            continue
        job = (steps_document.get("jobs") or {}).get(g.job)
        if job is None:
            problems.append(error(declared(g), f"gate {g.filter} names job {g.job}, which {g.workflow} does not have"))
            continue
        if g.step:
            steps = [s for s in job.get("steps") or [] if step_matches(s, g.step)]
            if len(steps) != 1 or g.filter not in gated_filters(
                    resolve_condition(str(steps[0].get("if", "")), inputs_of.get((g.workflow, g.job), {}))):
                problems.append(error(g.workflow, f"gate {g.filter} names step \"{g.step}\" of {g.job}, but "
                                                  f"no single step of that name has an if: on "
                                                  f"needs.changes.outputs.{g.filter}, nor one reading "
                                                  f"{g.filter} out of needs.changes.outputs.{LIST_OUTPUT}"))
        elif g.filter not in gated[(WORKFLOW, g.job)]:
            problems.append(error(WORKFLOW, f"job {g.job}'s if: does not test needs.changes.outputs.{g.filter}"))
    for name, kept in excluded.items():
        if name not in outputs:
            problems.append(error(kept.source or GATES_DIR,
                                  f"EXCLUDED names filter {name}, which no output of the changes job hands on"))

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
    # `needs: changes` and its whole if: condition and holds its steps in the called file,
    # whose own job ids are its to choose. The caller job has no run: step of its own, so the
    # two never answer for each other.
    skipped = {key for key, names in gated.items() if names and names <= set(excluded)}
    # A called file's jobs run inside the caller job's run and under its `if:`, so a caller whose
    # gate is EXCLUDED hands its skip to them: the loop below passes over a step of a called file
    # whose caller is skipped, and the go commands of those steps, read as that caller's, are the
    # other check's too, exactly as the caller's own would be. A job a caller with a gate of this
    # table also runs is read as that caller's alone (dispatch-browser.yaml's jobs, which
    # dispatch-references runs beside dispatch-browser and dispatch-browser-pr).
    claims = defaultdict(list)  # (the ci.yaml job, step label) -> pinned texts
    exacts = defaultdict(list)  # (the ci.yaml job, step label) -> (the gate's filter, Exact)
    owner = {}  # id(Run) -> the gate's filter
    unreduced = defaultdict(list)  # filter -> (Run, the command as the job runs it, why it is not reduced)
    for g in data.GATES:
        for r in g.runs:
            if not (r.step or r.source) or not r.text:
                problems.append(error(declared(g), f"gate {g.filter}'s run {r.workdir} {' '.join(r.pkgs)} is pinned "
                                                   f"to no step or source text"))
            if r.step:
                claims[(g.job, r.step)].append(r)
                owner[id(r)] = g.filter
        for e in g.exact:
            exacts[(g.job, e.step)].append((g.filter, e))
    for own, path, name_there, holder, job in units:
        if (path, name_there) in skipped or (WORKFLOW, own) in skipped:
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
                    problems.append(error(path, f"{where}, which no Run of a gate in {GATES_DIR} lists; add it "
                                                 f"to the job's gate so its closure is computed"))
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
                                                    f"({' '.join(r.pkgs)}): {'; '.join(parts)}; make the Run of the gate in {GATES_DIR} "
                                                    f"name exactly the packages, tags, -race and mode the line does, so the closure "
                                                    f"the job's filter is computed from is the one it compiles"))
    for (job_id, ref), fes in exacts.items():
        steps = [s for s in owner_steps(units, job_id) if "run" in s and step_matches(s, ref)]
        if len(steps) != 1:
            for name, _ in fes:
                problems.append(error(GATES_DIR, f"gate {name}'s exact entry names step \"{ref}\" of job {job_id}, "
                                                 f"which is not one run: step of it; update or remove the entry"))
    for (job_id, ref), rs in claims.items():
        steps = [s for s in owner_steps(units, job_id) if step_matches(s, ref)]
        text = squash(" ".join(str(s.get("run", "")) for s in steps))
        for r in rs:
            if not steps or squash(r.text) not in text:
                problems.append(error(GATES_DIR, f"job {job_id} step \"{ref}\" no longer runs `{r.text}`; update or "
                                                 f"remove the Run"))
    # A script's go command is held exact rather than reduced: the line holding the pinned text is
    # that text alone, uncontinued, and no env:, shell: or defaults.run.shell of the workflow, the
    # job or its steps, no container: and no environment write of the job can move it - over every
    # step of the job (around_job), since the check cannot prove which step starts the script. A
    # job that calls a workflow runs the script from that workflow's jobs, so each of them is read
    # the same way. Its variables are the Run author's.
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
            reason = around_job(units, g.job) or ("" if held else "its text spans lines")
            for i in held:
                line = lines[i].strip(" \t")
                if not reason and line != r.text:
                    reason = f"its line is `{line}`, not the text alone"
                elif not reason and continued(lines, i):
                    reason = "it is part of a backslash continuation"
            if reason:
                unreduced[g.filter].append((r, f"{r.source}, which job {g.job} runs, runs `{r.text}`", reason))
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
# A report step's `job:` that reads an input of its file rather than naming the caller outright,
# resolved through the calling job's `with:`.
CALLER_VALUE = re.compile(r"\$\{\{\s*inputs\.([A-Za-z0-9_-]+)\s*\}\}")
# The checks main's ruleset requires, each carried by the ci.yaml job of that name: a job's own
# check run is in its run's own check suite, where one POSTed with POST /repos/.../check-runs
# joins the Actions App's first suite at that commit. ci-ok is the verdict over every other job,
# pr-title runs check-pr-text.sh on a pull request and a merge group. No other job of ci.yaml may
# carry one of these names, since a job posts the check its display name gives and a skipped job
# posts a passing one.
#
# Each of the two carries the one `name:` DISPATCH_NAMES gives it, and no other. GitHub evaluates
# a job's name only when the job runs. ci-ok runs on every event: it is `ci-ok` on a pull
# request, a merge group and a dispatch of exactly refs/heads/main, and `ci-ok (dispatch)` on a
# dispatch of any other ref. pr-title runs on a pull request that is not a draft, on a merge group
# and on a dispatch, where its steps check nothing, and is named pr-title on the first two and
# `pr-title (dispatch)` on the third (PR_TITLE_IF). A draft's run skips it, leaves the name
# unevaluated, and its check run carries the expression's text, which is no pr-title either; a
# dispatch once did the same, and its check run was named by the expression's literal text
# (#5231's head 1bca361a). A dispatch of a branch tests the branch's own
# tree, not its merge with the base, and its check runs land on the branch's head, which is a
# pull request's head: under the plain name that commit carried a second ci-ok with another
# tree's verdict, and a skipped pr-title. Pull request #3513 at a368687e carried both on
# 2026-11-21 from workflow_dispatch run 44860519340, a ci-ok success beside its pull_request
# run's failure. GitHub's rollup of the pull request read no check run of the dispatched run,
# but a reader by name (the commit's check-runs list, or a record keeping the higher check run
# id per name) takes the dispatched run's.
#
# Whether a dispatch is of main is read by a shell test in the changes job (REF_STEP), not by an
# expression: GitHub's `==` and `!=` ignore case, so `github.ref != 'refs/heads/main'` is false
# for the branches refs/heads/Main and refs/heads/MAIN, which git accepts. The step writes
# is_main=true only for exactly refs/heads/main, and the changes job hands it on as
# REF_OUTPUT; a dispatch whose changes job wrote nothing (it failed) is no dispatch of main
# either. pr-title needs no job, so it cannot read that output, and its name carries the event
# alone: on a dispatch of main too it is `pr-title (dispatch)`, which main's ruleset does not read.
# What keeps a skipped pr-title from posting a pass named pr-title is that its name is an
# expression at all.
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
# pr-title's one `if:`: every event this file runs on, a draft's pull request apart, so its name is
# evaluated on a dispatch too.
PR_TITLE_IF = "${{ !github.event.pull_request.draft }}"
# The verdict job's step that sets every other ci-ok on the head to its conclusion, and the one
# `if:` it carries: it runs after a failed verdict (always()), on no dispatch but one of exactly
# refs/heads/main, since every other ci-ok on such a head is a pull request's or the queue's, and
# not where the hold step runs (HOLD_TERM): a merge group's first attempt whose verdict failed,
# where every ci-ok posted on the head is failure already and the hold must be the job's last
# step (.github/scripts/ci-ok-hold.sh's header).
RECONCILE_STEP = "Set every other ci-ok on this head to this job's conclusion"
HOLD_TERM = "steps.verdict.outcome == 'failure' && github.event_name == 'merge_group' && github.run_attempt == '1'"
RECONCILE_IF = (f"${{{{ always() && (github.event_name != 'workflow_dispatch' || "
                f"needs.changes.outputs.{REF_OUTPUT} == 'true') && !({HOLD_TERM}) }}}}")


def dispatch_name(job_id: str) -> str:
    """The one `name:` a REQUIRED_CHECKS job carries: `<job id> (dispatch)` on a dispatch of any
    ref but exactly refs/heads/main, and the job id on every other run that evaluates it. A run
    that skips the job evaluates no name, and its check run carries this expression's text."""
    return DISPATCH_NAMES[job_id]


def quoted_names(named: str) -> set:
    """The single-quoted strings of a `name:` that is an expression, which are the names it can
    evaluate to, and nothing for a plain name."""
    return set(re.findall(r"'([^']*)'", named)) if "${{" in named else set()


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
    REQUIRED_CHECKS name is a job of this file that carries the name dispatch_name gives it, no
    other job carries it or a name expression that can evaluate to it, VERDICT_JOB's reconcile
    step (RECONCILE_STEP) carries RECONCILE_IF, and VERDICT_JOB must need every job of the file
    but itself and
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
            want_name = dispatch_name(job_id)
            if named != want_name:
                carried = "carries no `name:`" if named == job_id else f"carries `name: {named}`"
                problems.append(error(WORKFLOW, f"job {job_id} {carried}, and {job_id} is a check main's "
                                                f"ruleset requires: give it `name: {want_name}`, so that a "
                                                f"workflow_dispatch run of any ref but exactly refs/heads/main, which "
                                                f"tests that ref's tree rather than a merge with the base, reports "
                                                f"under a name of its own and never sits beside the pull request's "
                                                f"{job_id} on the branch's head, while every other run that runs the job "
                                                f"reports {job_id}"))
            continue
        if named in REQUIRED_CHECKS:
            problems.append(error(WORKFLOW, f"job {job_id} is named {named}, a check main's ruleset "
                                            f"requires, and is not the {named} job: a job posts its own check, and a skipped one "
                                            f"posts a pass, so the job of that name is the only one that may carry it"))
            continue
        for check in sorted(quoted_names(named) & set(REQUIRED_CHECKS)):
            problems.append(error(WORKFLOW, f"job {job_id} carries `name: {named}`, which evaluates to {check} on "
                                            f"some run, a check main's ruleset requires, and is not the {check} job: a "
                                            f"job posts its own check, and a skipped one posts a pass, so the job of "
                                            f"that name is the only one that may carry it"))
    verdict_steps = (jobs.get(VERDICT_JOB) or {}).get("steps") or []
    for step in verdict_steps:
        if isinstance(step, dict) and step.get("name") == RECONCILE_STEP and str(step.get("if", "")) != RECONCILE_IF:
            problems.append(error(WORKFLOW, f"job {VERDICT_JOB}'s step \"{RECONCILE_STEP}\" carries `if: "
                                            f"{squash(str(step.get('if', '')))}`: give it `if: {RECONCILE_IF}`, so that "
                                            f"it runs after a failed verdict, on no workflow_dispatch run but one of "
                                            f"exactly refs/heads/main, since any other dispatch's verdict is no pull "
                                            f"request's and must set none of a pull request's ci-ok check runs, and not "
                                            f"after the hold step, which must end the job"))
    # The output the name and the reconcile step read, and the one step that writes it.
    changes = jobs.get("changes") or {}
    have_output = (changes.get("outputs") or {}).get(REF_OUTPUT)
    why_shell = ("the shell's `=` is case-sensitive, where an expression's `==` and `!=` ignore case and read "
                 "refs/heads/Main as main")
    if str(have_output) != REF_OUTPUT_VALUE:
        carried = f"has no output {REF_OUTPUT}" if have_output is None else \
            f"carries `{REF_OUTPUT}: {squash(str(have_output))}`"
        problems.append(error(WORKFLOW, f"the changes job {carried}: give it `{REF_OUTPUT}: {REF_OUTPUT_VALUE}`, the "
                                        f"output job {VERDICT_JOB}'s name and its step \"{RECONCILE_STEP}\" read to tell "
                                        f"a dispatch of exactly refs/heads/main from one of any other ref"))
    ref_steps = [s for s in changes.get("steps") or [] if isinstance(s, dict) and s.get("id") == REF_STEP_ID]
    exact = (len(ref_steps) == 1 and ref_steps[0].get("name") == REF_STEP_NAME and "if" not in ref_steps[0]
             and str(ref_steps[0].get("run", "")).strip() == REF_STEP_RUN)
    if not exact:
        problems.append(error(WORKFLOW, f"the changes job holds {len(ref_steps)} step(s) with id {REF_STEP_ID}, and "
                                        f"must hold exactly one, named \"{REF_STEP_NAME}\", with no `if:` and `run: "
                                        f"{REF_STEP_RUN}`: it is what writes {REF_OUTPUT}, and {why_shell}"))
    for check in REQUIRED_CHECKS:
        if check != VERDICT_JOB and check not in jobs:
            problems.append(error(WORKFLOW, f"this file has no {check} job, a check main's ruleset "
                                            f"requires: its own check run is that check on a pull request and on a merge group, "
                                            f"and the queue cannot merge a group nothing reports it on"))
    title = jobs.get("pr-title")
    if isinstance(title, dict) and str(title.get("if", "")) != PR_TITLE_IF:
        problems.append(error(WORKFLOW, f"job pr-title carries `if: {squash(str(title.get('if', '')))}`: give it "
                                        f"`if: {PR_TITLE_IF}`, so that it runs on a dispatch too and its check run is "
                                        f"named pr-title (dispatch), not the name expression's literal text, which "
                                        f"GitHub writes for a job it skips"))
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
        # LIST_OUTPUT selects no job, and rule 23 refuses a job-level read of it by name, so it is
        # no output CI_OK_SELECTED_BY could map a job to.
        outputs = sorted(set(GATE_REF.findall(condition)) - {LIST_OUTPUT})
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
            path = str(job["uses"]).removeprefix("./")
            called = documents.get(path) or {}
            callers = workflow_callers(document).get(path, [job_id])
            # Only the jobs of that file this caller runs (called_job_owners): a file several
            # jobs call holds one job per caller, each reporting under its own caller's id.
            called_jobs = {name: j for name, j in (called.get("jobs") or {}).items()
                           if job_id in called_job_owners(callers, name)}
            # The job that reports is the called workflow's job whose last step is the report:
            # always() first, so it runs after a failed step too, and ended only by the call's own
            # condition (docs-site.yaml's input, worker-image.yaml's event). Its `job:` is the
            # caller's id, written out or read from an input this caller passes it, which is how
            # dispatch-browser.yaml's merge job reports for whichever of its callers ran it.
            passed = job.get("with") or {}

            def reported_job(value: str) -> str:
                named = CALLER_VALUE.fullmatch(value.strip())
                return str(passed.get(named.group(1), "")) if named else value

            reports = [((j or {}).get("steps") or [])[-1] for j in called_jobs.values() if (j or {}).get("steps")]
            if not any(s.get("uses") == REPORT_ACTION and str(s.get("if", "")).startswith("always()")
                       and reported_job(str((s.get("with") or {}).get("job", ""))) == job_id
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


def check_push_workflows(data, filters: dict, mode: str, problems: list[Annotation]) -> list:
    """Rule 8: a PUSH_WORKFLOWS workflow does on a push to main what its filter's job does on a
    pull request and a merge group, so its on.push.paths must name exactly the generated filter's
    patterns: a pattern only the filter has lets a change pass the pull request's build and never
    reach main's, and one only the paths have runs main's for a change no pull request built. A
    push trigger is read by GitHub before any job runs, so it is the one copy of a filter the
    repository keeps; the write mode writes it rather than refusing it. Returns the report lines."""
    report = []
    for name, path in data.PUSH_WORKFLOWS.items():
        if name not in filters:
            problems.append(error(".github/scripts/ci_job_reads.py", f"PUSH_WORKFLOWS names filter {name}, which "
                                                                      f"no file of {GATES_DIR} declares"))
            continue
        if not (REPO / path).is_file():
            problems.append(error(".github/scripts/ci_job_reads.py", f"PUSH_WORKFLOWS names {path} for filter "
                                                                      f"{name}, which is not in the repository"))
            continue
        if mode == "write":
            rewrite_push_paths(path, name, filters[name], problems)
        push = events(load_repository_yaml(REPO / path) or {}).get("push")
        paths = push.get("paths") if isinstance(push, dict) else None
        if not (isinstance(paths, list) and paths and all(isinstance(p, str) for p in paths)):
            problems.append(error(path, f"{path} has no on.push.paths list, which must equal the generated filter "
                                        f"{name}"))
            continue
        missing = [p for p in dict.fromkeys(filters[name]) if p not in paths]
        extra = [p for p in dict.fromkeys(paths) if p not in filters[name]]
        for p in missing:
            problems.append(error(path, f"{path}'s on.push.paths lacks {p}, which the generated filter {name} has, "
                                        f"so a push to main that changes only a file it matches never runs {path}; "
                                        f"run `{WRITE}`"))
        for p in extra:
            problems.append(error(path, f"{path}'s on.push.paths has {p}, which the generated filter {name} lacks; "
                                        f"run `{WRITE}`"))
        verdict = "differs from" if missing or extra else "equals"
        report.append(f"{path}: on.push.paths {verdict} filter {name} ({len(set(paths))} patterns)")
    return report


PATHS_KEY = re.compile(r"^(\s*)paths:\s*$")


def rewrite_push_paths(path: str, name: str, patterns: list, problems: list[Annotation]) -> None:
    """`path` written again with its one `paths:` list replaced by `patterns`, at the indentation
    of its items, and left alone when that changes nothing."""
    lines = read_repository_text(REPO / path).splitlines()
    keys = [i for i, line in enumerate(lines) if PATHS_KEY.match(line)]
    if len(keys) != 1:
        problems.append(error(path, f"{len(keys)} `paths:` lists, where this check writes filter {name} into the "
                                    f"one on.push.paths"))
        return
    outer = indent_of(lines[keys[0]])
    body = range(keys[0] + 1, len(lines))
    stop = next((i for i in body if lines[i].strip() and indent_of(lines[i]) <= outer), len(lines))
    item = next((FILTER_ITEM.match(lines[i]).group(1) for i in range(keys[0] + 1, stop) if FILTER_ITEM.match(lines[i])),
                " " * (outer + 2))
    lines[keys[0] + 1:stop] = [f"{item}- {quote(p)}" for p in patterns]
    text = "".join(line + "\n" for line in lines)
    if text != read_repository_text(REPO / path):
        (REPO / path).write_text(text)


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
                   selection: Selection, listed: dict, problems: list[Annotation]) -> list:
    """A step that runs a package under a build tag and passes `-run` runs only the tests the tag
    adds, so the package's untagged tests must run elsewhere in every run that step runs. The job
    that runs them is declared (TAG_RUNS), and its filter must cover every file the tag step reads
    (tag_step_files) and every file of the tag step's filter's listed block (`listed`); the
    generation puts them in that filter (tag_held_files), so this holds the generation to it. The
    table is no entry's to opt into: a `go test`
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
        block = [f for f in listed.get(entry.filter, ()) if f not in read]
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


# ---------------------------------------------------------------- the references lane's spec files
# The `references` lane of dispatch-browser.yaml runs the spec files that one shell variable of the
# script its shard job's browser step runs lists, and the dispatch_references filter, an EXCLUDED
# one whose patterns its file of .github/ci-gates keeps by hand, is what starts the job calling
# that lane. The arm is read from that script with comments and blank lines aside (script_lines),
# so a comment that spells an assignment is no entry.
BROWSER_WORKFLOW = ".github/workflows/dispatch-browser.yaml"
LANE_SCRIPT = ".github/scripts/dispatch-browser-shard-suite.sh"
REFERENCES_FILTER = "dispatch_references"
REFERENCES_ARM = re.compile(r"^references\)$(.*?)^;;$", re.M | re.S)
LANE_FILES = re.compile(r'^lane_files="([^"]*)"', re.M)


# Where a run: step's text may start the lane script's path: the start of a line or a word, a
# quote, an `=`, a `(` or a backtick, each also before `./`, and a variable's or an expression's
# end before a `/` (`"$GITHUB_WORKSPACE/<script>"`, `${{ github.workspace }}/<script>`,
# `$(git rev-parse --show-toplevel)/<script>`).
LANE_SCRIPT_START = r"(?:^|[\s'\"=(`]|(?:^|[\s'\"=(`])\./|(?:\$[A-Za-z_][A-Za-z0-9_]*|[})])/)"


def runs_lane_script(holder: dict, job: dict, step: dict) -> bool:
    """Whether a run: step names LANE_SCRIPT as a whole word of its run: text after one of
    LANE_SCRIPT_START's starts, by its path from the repository root or from the directory the
    step runs in; nothing that goes on the path after it (`.old`, a longer name) counts. This
    reads only where the references lane's spec files are read from: which step starts a script
    is not proven by its text (around_job), so a lane started some other way is read as run from
    no step, and rule 24 fails rather than holding the filter to nothing."""
    run = str(step.get("run", ""))
    workdir = step_workdir(holder, job, step)
    paths = {LANE_SCRIPT, os.path.relpath(LANE_SCRIPT, workdir)} if workdir != "." else {LANE_SCRIPT}
    return any(re.search(rf"{LANE_SCRIPT_START}{re.escape(path)}(?![\w.-])", run) for path in paths)


def check_references_lane(data, quantifier: str, problems: list[Annotation]) -> list:
    """Rule 24: every spec file the references lane runs is one the dispatch_references filter
    matches. That filter names less than its job reads on purpose (EXCLUDED holds it to no gate's
    reads), but the lane's own spec files are the least it must name: a spec file it misses is a
    change to the lane's own rows that starts no run of the lane, and nothing fails. The entries
    are the lane_files LANE_SCRIPT sets in its `references)` arm. Each is a path from the directory
    of the step of BROWSER_WORKFLOW that runs that script, which is where Playwright reads it, and
    is held to the filter as the repository path it names there, under the changes job's
    quantifier. A tree without that workflow holds no such lane; one whose script is absent, sets
    lane_files in no `references)` arm, in more than one, or in an arm that lists no file, or whose
    workflow runs the script from no step or from steps of two directories, or where no file of
    .github/ci-gates declares the filter as EXCLUDED, fails, rather than holding the filter to
    nothing. Returns the report lines."""
    if not (REPO / BROWSER_WORKFLOW).is_file():
        return []
    kept = data.EXCLUDED.get(REFERENCES_FILTER)
    source = kept.source if kept is not None else GATES_DIR
    held = f"so the filter {REFERENCES_FILTER} cannot be held to the spec files the references lane runs"
    if not (REPO / LANE_SCRIPT).is_file():
        problems.append(error(BROWSER_WORKFLOW, f"{LANE_SCRIPT}, the script whose `references)` arm lists the spec "
                                                f"files {BROWSER_WORKFLOW}'s references lane runs, is absent, {held}"))
        return []
    arms = REFERENCES_ARM.findall("\n".join(script_lines(read_repository_text(REPO / LANE_SCRIPT))))
    lists = [listed.group(1).split() for listed in (LANE_FILES.search(arm) for arm in arms) if listed]
    if not lists:
        problems.append(error(LANE_SCRIPT, f"{LANE_SCRIPT} sets lane_files=\"...\" in no `references)` arm of its "
                                           f"script, {held}"))
        return []
    if len(lists) > 1:
        problems.append(error(LANE_SCRIPT, f"{LANE_SCRIPT} sets lane_files in {len(lists)} `references)` arms, "
                                           f"where this check reads one, {held}"))
        return []
    entries = lists[0]
    if not entries:
        problems.append(error(LANE_SCRIPT, f"{LANE_SCRIPT} sets lane_files to no spec file in its `references)` "
                                           f"arm, {held}"))
        return []
    browser = load_repository_yaml(REPO / BROWSER_WORKFLOW) or {}
    runs = [(f'job {job_id} step "{step_label(step or {})}"', step_workdir(browser, job or {}, step or {}))
            for job_id, job in (browser.get("jobs") or {}).items()
            for step in (job or {}).get("steps") or []
            if runs_lane_script(browser, job or {}, step or {})]
    if not runs:
        problems.append(error(BROWSER_WORKFLOW, f"no step of {BROWSER_WORKFLOW} runs {LANE_SCRIPT}, so the "
                                                f"directory its references lane's spec files are read from is "
                                                f"unknown, {held}"))
        return []
    if len({workdir for _, workdir in runs}) > 1:
        problems.append(error(BROWSER_WORKFLOW, f"{len(runs)} steps of {BROWSER_WORKFLOW} run {LANE_SCRIPT} from "
                                                f"more than one directory "
                                                f"({'; '.join(f'{where} from {workdir}' for where, workdir in runs)}), "
                                                f"where this check reads one, {held}"))
        return []
    where, workdir = runs[0]
    if kept is None:
        problems.append(error(GATES_DIR, f"no file of {GATES_DIR} declares the filter {REFERENCES_FILTER} "
                                         f"(`EXCLUDED = excluded({REFERENCES_FILTER!r}, ...)`), which must match "
                                         f"every spec file the references lane of {LANE_SCRIPT} runs ({where} "
                                         f"of {BROWSER_WORKFLOW} runs it)"))
        return []
    matcher = PathsFilter(list(kept.patterns), quantifier or "some-with-excludes")
    unmatched = 0
    for entry in entries:
        path = os.path.normpath(os.path.join(workdir, entry))
        if matcher.covers(path):
            continue
        unmatched += 1
        problems.append(error(LANE_SCRIPT, f"{LANE_SCRIPT} runs {entry} in the references lane (lane_files, run "
                                           f"from {workdir} by {where} of {BROWSER_WORKFLOW}), which the filter "
                                           f"{REFERENCES_FILTER} ({source}) does not match as {path}, so a change "
                                           f"to that spec file starts no run of the lane that runs it; add '{path}' "
                                           f"to the patterns of {source}, or take the entry out of lane_files"))
    return [f"{LANE_SCRIPT} references lane: {len(entries)} spec files, {unmatched} unmatched by filter "
            f"{REFERENCES_FILTER}"]


def declared_reads(data) -> list:
    """Every pattern written by hand, each with the file it is in and how a refusal names it: a
    gate's reads, a //ci: line's glob, and an EXCLUDED filter's patterns but its `!` ones."""
    found = [(r.glob, declared(g), f"gate {g.filter} reads") for g in data.GATES for r in g.reads]
    found += [(d.globs[0], d.path, f"the //ci:{d.kind} line at {d.path}:{d.line} reads")
              for d in getattr(data, "DIRECTIVES", ()) if d.globs]
    found += [(p, kept.source or GATES_DIR, f"filter {name} has")
              for name, kept in data.EXCLUDED.items() for p in kept.patterns if not p.startswith("!")]
    return found


# What a harness under .github/scripts reaches of the scripts beside it, read from its text and
# from the text of every shell script it reaches in turn: a `$<folder>/<name>` (linked into a
# fixture, sourced or run), where <folder> is `script_dir` or any variable assigned from `dirname`
# or BASH_SOURCE, or `$(dirname ...)/<name>`; a `$<folder>/$<var>` whose var any `for <var> in`
# loop of the script takes from names or from an array of names; a library it sources (`source` or
# `.`); and a Python module beside it - every name of an `import a, b`, a `from <module> import`,
# a `python3 -m <module>` and an `importlib.import_module("<module>")`, at the start of a line of a
# heredoc or right after a quote or a `;`. A path a script only names in a string (an expected
# message, a line it parses) is no reach. A module reaches each module its top-level code imports
# (an import inside a function runs only when that function does), and one whose top-level code
# calls load_gate_files (ci_job_reads.py) reaches every gate file.
FOLDER_VARIABLE = re.compile(r"^\s*(?:local\s+|readonly\s+|declare\s+\S*\s*)?(\w+)=.*\b(?:dirname|BASH_SOURCE)\b", re.M)
FOLDER_SUBSHELL = re.compile(r"(?:\$\((?:cd\s+)?\"?\$\(dirname[^)]*\)\"?(?:\s*&&\s*pwd)?\)|\$\(dirname[^)]*\))/([\w.-]+)")
# A variable that names the repository's root (`repo_root`, `REPO_ROOT`, `repo`), and
# `<var>/.github/scripts/<name>` below it. A fixture's own root (`$root/.github/scripts/...`) is
# the fixture's copy, no reach.
REPOSITORY_SCRIPT = re.compile(r"\$\{?(?:repo_root|REPO_ROOT|repo)\}?\"?/\.github/scripts/([\w.-]+)")
HARNESS_FOR = re.compile(r"\bfor\s+(\w+)\s+in\s+([^;\n]*?)\s*;\s*do\b", re.S)
HARNESS_ARRAY = re.compile(r"^\s*(?:local\s+|declare\s+-a\s+)?(\w+)=\(([^)]*)\)", re.M)
IMPORT_ITEM = r"[\w.]+(?:\s+as\s+\w+)?"
IMPORT_NAMES = re.compile(rf"(?:^\s*|[\"';]\s*)import\s+({IMPORT_ITEM}(?:\s*,\s*{IMPORT_ITEM})*)", re.M)
FROM_IMPORT = re.compile(r"(?:^\s*|[\"';]\s*)from\s+([A-Za-z_]\w*)[\w.]*\s+import\b", re.M)
MODULE_RUN = re.compile(r"(?:-m\s+|import_module\(\s*[\"'])([A-Za-z_]\w*)")
TOP_LEVEL_IMPORT_NAMES = re.compile(rf"^import\s+({IMPORT_ITEM}(?:\s*,\s*{IMPORT_ITEM})*)", re.M)
TOP_LEVEL_FROM = re.compile(r"^from\s+([A-Za-z_]\w*)[\w.]*\s+import\b", re.M)
SOURCED = re.compile(r"^\s*(?:source|\.)\s+.*/([\w.-]+\.sh)\"?\s*$", re.M)
LOADS_GATE_FILES = re.compile(r"^\S.*\bload_gate_files\(", re.M)
HEREDOC = re.compile(r"<<(-?)\s*(['\"]?)(\w+)\2")


def names_of(lists) -> list:
    """The first component of each name of each `import a, b.c as n` list."""
    return [name.split()[0].split(".")[0] for names in lists for name in names.split(",") if name.strip()]


def shell_unquoted(text: str) -> str:
    """`text` with the contents of each shell single-quoted string blanked: text a fixture writes
    (`printf '... "$(dirname "$0")/x.sh"'`) is no reach. Read as bash reads quotes: a `'` inside a
    double-quoted string is a literal character, a single-quoted string may span lines, and a
    heredoc's body is kept whole, since a quote there is the heredoc's own text."""
    out, i, n = [], 0, len(text)
    # A stack of quoting contexts, one per `$(` open: [state, open parentheses]. A `$(` inside a
    # double-quoted string starts a fresh context, where quotes pair anew, as bash reads them.
    stack = [[None, 0]]
    pending = []  # the heredoc words to close, in order
    while i < n:
        c = text[i]
        top = stack[-1]
        state = top[0]
        if len(stack) == 1 and state is None and pending and (i == 0 or text[i - 1] == "\n"):
            # Inside a heredoc body: copy lines until the closing word.
            word, strip = pending[0]
            end = text.find("\n", i)
            line = text[i:] if end < 0 else text[i:end]
            out.append(line + ("" if end < 0 else "\n"))
            i = n if end < 0 else end + 1
            if (line.lstrip("\t") if strip else line) == word:
                pending.pop(0)
            continue
        if state == "'":
            if c == "'":
                top[0] = None
                out.append(c)
            elif c == "\n":
                out.append(c)
            i += 1
            continue
        if c == "\\" and i + 1 < n:
            out.append(text[i:i + 2])
            i += 2
            continue
        if text.startswith("$(", i):
            stack.append([None, 0])
            out.append("$(")
            i += 2
            continue
        if state == '"':
            if c == '"':
                top[0] = None
            out.append(c)
            i += 1
            continue
        if c == "#" and (i == 0 or text[i - 1] in " \t\n;&|("):
            # A comment runs to the end of its line, and a quote inside it is text: dropped, so an
            # apostrophe there (`f() { # the filter's verdicts`) opens no string.
            end = text.find("\n", i)
            i = n if end < 0 else end
            continue
        if c == "(":
            top[1] += 1
        elif c == ")":
            if top[1]:
                top[1] -= 1
            elif len(stack) > 1:
                stack.pop()
        elif c in "'\"":
            top[0] = c
        elif c == "<" and text.startswith("<<", i) and not text.startswith("<<<", i):
            match = HEREDOC.match(text, i)
            if match:
                pending.append((match.group(3), bool(match.group(1))))
                out.append(match.group(0))
                i = match.end()
                continue
        out.append(c)
        i += 1
    return "".join(out)


def harness_reaches(harness: str) -> dict:
    """{a file of the repository: how the harness reaches it} for a harness script under
    .github/scripts, read as the patterns above say. Comment lines are no reach. Every shell script
    reached is read as the harness is, so a library a script under test sources is reached too."""
    scripts = Path(harness).parent.as_posix()
    found: dict = {}

    def code(path: str) -> str:
        text = read_repository_text(REPO / path).replace("\\\n", " ")
        return "\n".join(line for line in text.split("\n") if not line.lstrip().startswith("#"))

    def reach(path: str, how: str) -> None:
        if path in found or not (REPO / path).is_file():
            return
        found[path] = how
        if path.endswith(".py"):
            text = code(path)
            for module in names_of(TOP_LEVEL_IMPORT_NAMES.findall(text)) + TOP_LEVEL_FROM.findall(text):
                reach(f"{scripts}/{module}.py", f"{path} imports it")
            if LOADS_GATE_FILES.search(text):
                for gate_file in sorted((REPO / GATES_DIR).glob("*.py")):
                    reach(gate_file.relative_to(REPO).as_posix(), f"{path} loads every gate file")
        elif path.endswith(".sh") or "." not in Path(path).name:
            scan(path)

    def scan(path: str) -> None:
        """A shell script: what it links, runs, sources and imports. A path inside a shell
        single-quoted string is text a fixture writes (`printf '... source "$(dirname "$0")/x.sh"'`),
        no reach (shell_unquoted); a Python module named inside one is the code of a
        `python3 -c '...'`, which runs."""
        text = code(path)
        unquoted = shell_unquoted(text)
        arrays: dict = {}
        for var, words in HARNESS_ARRAY.findall(unquoted):
            arrays.setdefault(var, []).extend(w.strip("\"'") for w in words.split())
        loops: dict = {}
        for var, words in HARNESS_FOR.findall(unquoted):
            for word in words.split():
                array = re.fullmatch(r"\"?\$\{(\w+)\[@\]\}\"?", word)
                loops.setdefault(var, []).extend(arrays.get(array.group(1), []) if array else [word.strip("\"'")])
        for name in SOURCED.findall(unquoted):
            reach(f"{scripts}/{name}", f"{path} sources it")
        # A folder variable is `script_dir`, one assigned from dirname or BASH_SOURCE, or one
        # assigned from another folder variable (`dir=$script_dir`).
        folders = {"script_dir"} | set(FOLDER_VARIABLE.findall(unquoted))
        while True:
            copies = {var for var, source in re.findall(r"^\s*(?:local\s+)?(\w+)=\"?\$\{?(\w+)\}?\"?\s*(?:;|$)",
                                                        unquoted, re.M) if source in folders}
            if copies <= folders:
                break
            folders |= copies
        for folder in sorted(folders):
            for name in re.findall(rf"\$\{{?{folder}\}}?\"?/(\$\{{?\w+\}}?|[\w.-]+)", unquoted):
                var = name.strip("${}")
                for each in loops.get(var, []) if name.startswith("$") else [name]:
                    reach(f"{scripts}/{each}", f"{path} reaches it as ${folder}/{each}")
        for name in FOLDER_SUBSHELL.findall(unquoted):
            reach(f"{scripts}/{name}", f"{path} reaches it beside itself")
        for name in REPOSITORY_SCRIPT.findall(unquoted):
            reach(f"{scripts}/{name}", f"{path} reaches it under the repository's root")
        for module in names_of(IMPORT_NAMES.findall(text)) + FROM_IMPORT.findall(text) + MODULE_RUN.findall(text):
            reach(f"{scripts}/{module}.py", f"{path} imports it")

    found[harness] = "the harness"
    scan(harness)
    found.pop(harness, None)
    return found


def check_harness_reads(data, gate_files: dict, problems: list[Annotation]) -> list:
    """Rule 22: a gate that reads a harness script (`*.test.sh` under .github/scripts) reads every
    file that harness reaches (harness_reaches): a harness that links a script into its fixtures, or
    imports a module, tests it, so a change to it alone must start the harness's step. Each file the
    gate's read set lacks is refused by name. Returns the report lines."""
    report = []
    for g in data.GATES:
        harnesses = [r.glob for r in g.reads
                     if r.glob.endswith(".test.sh") and r.glob.startswith(".github/scripts/") and "*" not in r.glob]
        reads = gate_files[g.filter].reads if g.filter in gate_files else set()
        for harness in harnesses:
            if not (REPO / harness).is_file():
                continue
            reached = harness_reaches(harness)
            missing = sorted(path for path in reached if path not in reads)
            report.append(f"{harness}: reaches {len(reached)} files, {len(missing)} unread by gate {g.filter}")
            for path in missing:
                problems.append(error(declared(g), f"gate {g.filter} runs {harness}, which reaches {path} "
                                                   f"({reached[path]}), and the gate does not read it, so a "
                                                   f"change to that file alone starts no run of the harness; "
                                                   f"add it to the gate's reads"))
    return report


def check_dead_patterns(data, expander: Expander, problems: list[Annotation]) -> None:
    """Rule 9: a pattern written by hand that matches no file of the repository names nothing: a
    gate's read or a //ci: line's glob then holds its filter to nothing, and a change to the files
    it means starts no job, on the pull request and again on main. So is a //ci:reads line whose
    `!` globs leave none of its glob's files. Judged over repo_files() with the Expander the rest
    of the check uses. A `!` pattern of an EXCLUDED filter is exempt: it names what to leave out,
    and leaving nothing out is no fault. The filters the changes job reads are computed from the
    files the repository holds, so no generated pattern can match none, which is why the rule's
    other halves - a filter pattern of a hand copy that matched nothing, or nothing its job
    reads - have no subject left."""
    for pattern, path, where in dict.fromkeys(declared_reads(data)):
        if expander.expand(pattern):
            continue
        problems.append(error(path, f"{where} {pattern}, which matches no file in the repository, so it holds "
                                    f"its filter to nothing; correct the path or drop the read"))
    for d in getattr(data, "DIRECTIVES", ()):
        if d.kind == "reads" and len(d.globs) > 1 and expander.expand(d.globs[0]) \
                and not read_files(d.entries[0], expander):
            problems.append(error(d.path, f"the //ci:reads line at {d.path}:{d.line} reads {' '.join(d.globs)}, "
                                          f"whose `!` globs leave none of the files its glob matches, so it holds its "
                                          f"filter to nothing; correct the globs or drop the line"))


def tag_step_files(entry, gate_files: dict, selection: Selection) -> tuple:
    """What the tag step of a TAG_RUNS entry reads, which rule 15 holds the filter of the job that
    runs the package's untagged tests to, as (the files, sorted; how the report counts them; how a
    refusal names them). A change that starts the tagged run and that the tag step reads must
    start the untagged one with it: the tag's own test files, the package's untagged ones the
    tagged build compiles too, and what the tag step's gate declares besides - the
    go-tagged-tests.sh a soak takes its -run from, and the gate's own workflow files. They are
    the read set the generation computes for the gates on the entry's filter (gate_files); the
    entry names that filter as the tag step's gate. Only a tag step's filter this check computes
    no read set for (an EXCLUDED one) is stood for by what it selects, since that is all the
    check knows of what its step reads."""
    if entry.filter in gate_files:
        files = gate_files[entry.filter]
        whose = " and ".join(files.wheres)
        return (sorted(files.reads), f"read by {whose} (filter {entry.filter})",
                f"{whose} {'read' if len(files.wheres) > 1 else 'reads'}")
    return (selection.of(entry.filter), f"selected by filter {entry.filter}", f"filter {entry.filter} matches")


def tag_held_files(data, gate_files: dict, selection: Selection, listed: dict) -> dict:
    """Per filter, the files rule 15 holds it to: every file a TAG_RUNS step reads
    (tag_step_files) and every file of its filter's listed block, held on the filter of the job
    that runs that package's untagged tests, so a change that starts the tagged run starts the
    untagged one with it. The generation puts them in that filter as it puts its own job's reads
    (compute_filters): listener-server-change's soak reads cmd/listener's tagged test and
    .github/scripts/go-tagged-tests.sh, which envoy-go-race does not, and envoy-go-race's filter
    selects them. An entry pairing a filter with itself adds nothing here, since what its tag step
    reads is already what that filter's gate reads, and its listed block is its own."""
    held: dict = defaultdict(set)
    for entry in data.TAG_RUNS:
        if entry.untagged_filter in gate_files and (entry.filter in gate_files or entry.filter in data.EXCLUDED):
            held[entry.untagged_filter].update(tag_step_files(entry, gate_files, selection)[0])
            held[entry.untagged_filter].update(listed.get(entry.filter, ()))
    return held


@dataclass
class GateFiles:
    """Per filter, the union of the files the gates on it read, the globs those gates declare
    that its filter carries as written (verbatim_globs), and how the check names each gate."""

    reads: set = field(default_factory=set)
    globs: set = field(default_factory=set)
    wheres: list = field(default_factory=list)


def verbatim_globs(g: Gate) -> set:
    """The globs a gate declares that its filter carries as written, beside the files they match
    now: each wildcard read the gate itself declares whose `but` leaves out Go test files alone.
    Such a glob also selects a file a change deletes or adds under it, which the files of the
    tree the changes job computes on cannot name, and the Go test files it matches that the job
    does not compile are kept out by the filter's computed `!` patterns, as `but` says."""
    return {r.glob for r in g.reads
            if any(c in r.glob for c in "*?[") and set(r.but) <= {f"**/*{TEST_SUFFIX}"}}


# ---------------------------------------- the filters, computed from what each job reads
SCRIPT = ".github/scripts/check-ci-filters.sh"
WRITE = f"{SCRIPT} write"
GENERATE = f"{SCRIPT} generate"
# Every file Python opened in this run, which check-ci-filters.sh's audit hook records.
OPENED: list = []


def engine_inputs() -> list:
    """The files of the repository the filters are computed with rather than from: the two scripts
    that run the computation and every file this run's Python opened, the engine's own modules
    among them, but the Go files (whose //ci: lines and imports a filter of the jobs that compile
    them selects) and the files of .github/ci-gates (each selected by the filter it declares).
    A change to one of these can change what many filters select without any of them selecting
    it, which the changes job holds against the filters of the base tree (ci-filters-generate.sh)."""
    found = {SCRIPT, GENERATE_SCRIPT}
    for opened in OPENED:
        path = rel(opened)
        if path and not path.endswith(".go") and not path.startswith(".github/ci-gates/") \
                and (REPO / path).is_file():
            found.add(path)
    # What tsc and bun resolved the listed blocks with: the tsconfig chains, the package.json
    # files, the lockfile (ci_ts_listing.RESOLVED), which no Python open sees.
    listing = sys.modules.get("ci_ts_listing")
    if listing is not None:
        found |= set(listing.RESOLVED)
    return sorted(found)


@dataclass(frozen=True)
class Marks:
    """One kind of block a generated filter holds: its first and last line's prefixes and the
    command that writes it, which a refusal names."""

    begin: str
    end: str
    write: str


# The block of a filter that holds one pattern per file of a tree that the tsc programs paired
# with some filters compile or the bundles paired with them load (.github/scripts/ci_ts_programs.py,
# LISTED), listed with Bun (ci_ts_listing.py) when the changes job computes its filters. Its two
# lines mark it so .github/scripts/check-ts-filters.sh, which holds every filter to its programs,
# can tell those patterns from the rest of the filter: a pattern outside the block that matches a
# file of the tree defeats the narrowing the block is for.
TS_LISTED = Marks("# BEGIN listed", "# END listed", GENERATE)
KINDS = (TS_LISTED,)


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

    `excluded` is the Go test files the filter's other patterns select and the gates on it do not
    read: a file the filter does not select needs no pattern to keep it unselected. A tree pattern
    excludes the files under it that are in neither set too, which is why a directory may take
    one only when nothing `read` is under it."""
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


# The characters a picomatch glob reads as more than themselves, which a path the generation
# writes as itself may not hold: dorny/paths-filter would read such a pattern as a wider one.
GLOB_SPECIAL = re.compile(r"[*?\[\]{}()!'\\\x00-\x1f]|^\s|\s$")


class Tree:
    """The repository's files by directory, built once and read by every filter's compression."""

    def __init__(self, files: list):
        self.direct: dict = defaultdict(list)  # directory -> its files
        self.kids: dict = defaultdict(set)  # directory -> the directories directly under it
        for f in files:
            node = os.path.dirname(f)
            self.direct[node].append(f)
            while node:
                parent = os.path.dirname(node)
                if node in self.kids[parent]:
                    break
                self.kids[parent].add(node)
                node = parent


def compressed(read: set, tree: Tree) -> list:
    """Patterns that select every file of `read` and, among the repository's files, no other but a
    Go test file, in three forms: `<dir>/**` for a directory whose tree holds a file of `read` and
    every file of it that is not a Go test file is in `read`, `<dir>/*` for such a directory's own
    files, and the path itself otherwise. The directory forms also select a file a change deletes
    there, which a filter must: the change that deletes a source of a package a job compiles
    starts that job. A Go test file the patterns select and `read` lacks is negation_patterns'
    to leave out. Sorted by the walk, a directory's own files before the directories under it,
    so two runs over one tree write one text. A path holding a character a glob reads (GLOB_SPECIAL)
    is a ValueError: no pattern of these forms says it alone."""
    full: dict = {}  # directory -> whether every file of its tree but the Go test files is read
    touched: dict = {}  # directory -> whether its tree holds a file of `read`

    def walk(node: str) -> None:
        own = tree.direct.get(node, [])
        is_full = all(f in read for f in own if not f.endswith(TEST_SUFFIX))
        is_touched = any(f in read for f in own)
        for kid in tree.kids.get(node, ()):
            walk(kid)
            is_full = is_full and full[kid]
            is_touched = is_touched or touched[kid]
        full[node], touched[node] = is_full, is_touched

    walk("")
    out: list = []

    def emit(node: str) -> None:
        if node and full[node]:
            out.append(f"{node}/**")
            return
        own = tree.direct.get(node, [])
        mine = sorted(f for f in own if f in read)
        if mine:
            if node and all(f in read for f in own if not f.endswith(TEST_SUFFIX)):
                out.append(f"{node}/*")
            else:
                out.extend(mine)
        for kid in sorted(tree.kids.get(node, ())):
            if touched[kid]:
                emit(kid)

    emit("")
    for pattern in out:
        if GLOB_SPECIAL.search(pattern.removesuffix("/**").removesuffix("/*")):
            raise ValueError(f"{pattern} holds a character a glob reads as more than itself, so no generated "
                             f"pattern names it alone")
    return out


@dataclass(frozen=True)
class Computed:
    """One generated filter: the patterns of what its jobs read, its listed block, and the `!`
    patterns that keep it off the Go test files the others select and its jobs do not read. An
    EXCLUDED filter's patterns are its file's, as written."""

    positives: tuple = ()
    listed: tuple = ()
    negations: tuple = ()

    @property
    def patterns(self) -> list:
        return [*self.positives, *self.listed, *self.negations]


def gate_read_sets(session: Session, data, files: list, expander: Expander, unreduced: dict) -> tuple:
    """(per filter, the GateFiles of the gates on it; the report lines). A gate reads the
    closure of each go command it runs, the RULES of the files those compile, and its declared
    reads; a go command this check does not reduce counts as reading every Go file of the
    repository and every file under a Go module's directory (module_trees_and_go_files), which
    the report names. A gate on a job's own `if:` reads what each gate on one of that job's steps
    reads too: a step runs only in a run its job's filter selected, so a change only the step
    reads must start the job, which is why the test job's filter holds every harness its steps
    test."""
    gate_files: dict = defaultdict(GateFiles)
    own: dict = defaultdict(set)  # id of a gate -> the files it reads
    report = []
    # The go commands the walk below asks, run at once first: each gate's own and each one a
    # //ci:builds line starts. A command only a listing below reveals runs when the walk asks it.
    runs = [r for g in data.GATES for r in gate_runs(g)]
    runs += [action for actions in data.RULES.values() for action in actions if isinstance(action, Run)]
    jobs = session.prefetch(runs)
    report.append(f"go commands: {len(session.cache)} answers prefetched for {len(set(runs))} commands, "
                  f"{jobs} at a time")
    fallback = None
    for g in data.GATES:
        found: dict = {}  # (glob, but) -> one Read of it; whys are kept on the Reads themselves
        seen: set = set()
        for r in gate_runs(g):
            for read in closure(session, data.RULES, r, seen):
                found.setdefault((read.glob, read.but), read)
        for read in g.reads:
            found.setdefault((read.glob, read.but), read)
        reads = set().union(*(read_files(read, expander) for read in found.values()))
        widened = []
        for r, command, why_not in unreduced.get(g.filter, []):
            if fallback is None:
                fallback = module_trees_and_go_files(files)
            reads.update(fallback)
            widened.append(f"{command}, which this check does not reduce ({why_not})")
        own[id(g)] = reads
        where = g.job + (f' step "{g.step}"' if g.step else "")
        gate_files[g.filter].reads.update(reads)
        gate_files[g.filter].globs.update(verbatim_globs(g))
        gate_files[g.filter].wheres.append(where)
        note = (f"; every Go file and every file under a Go module's directory, since {'; '.join(widened)}"
                if widened else "")
        report.append(f"{where}: {len(reads)} files (filter {g.filter}){note}")
    jobs = {(g.workflow, g.job): g.filter for g in data.GATES if not g.step}
    for g in data.GATES:
        name = jobs.get((g.workflow, g.job))
        if g.step and name and name != g.filter:
            gate_files[name].reads.update(own[id(g)])
            gate_files[name].globs.update(verbatim_globs(g))
    return gate_files, report


def compute_filters(data, gate_files: dict, listed: dict, files: list, expander: Expander,
                    selection: Selection) -> dict:
    """Every filter of the changes job, by name: a gate filter from the files its gates read
    (gate_files), the files rule 15 holds it to (tag_held_files), the PUSH_WORKFLOWS workflow rule
    8 pairs it with (a workflow that does not name itself never runs on a change to itself), and
    its listed block (`listed`); an EXCLUDED filter as its file writes it. `selection` gives the
    files an EXCLUDED filter selects, which a TAG_RUNS entry on one stands for. Each gate filter is
    checked against its own read set before it is returned: what its patterns select among the
    repository's files must be exactly those files, or this raises naming the first difference."""
    tree = Tree(files)
    tests = [f for f in files if f.endswith(TEST_SUFFIX)]
    known = set(files)
    held = tag_held_files(data, gate_files, selection, listed)
    computed = {}
    for name in sorted(gate_files):
        read = (gate_files[name].reads | held.get(name, set())) & known
        if name in data.PUSH_WORKFLOWS and data.PUSH_WORKFLOWS[name] in known:
            read.add(data.PUSH_WORKFLOWS[name])
        block = tuple(sorted(set(listed.get(name, ())) - read))
        # The gates' own globs as written, then the files no such glob selects, compressed.
        globs = sorted(gate_files[name].globs)
        covered = set().union(*(expander.expand(g) for g in globs))
        positives = globs + [p for p in compressed(read, tree) if not set(expander.expand(p)) <= covered]
        picked = selected([*positives, *block], expander)
        read_tests = {f for f in read if f.endswith(TEST_SUFFIX)}
        negations = negation_patterns({f for f in picked if f.endswith(TEST_SUFFIX)} - read_tests, read_tests, tests)
        result = Computed(tuple(positives), block, tuple(negations))
        got = selected(result.patterns, expander)
        if got != read | set(block):
            stray = sorted(got - read - set(block))[:3]
            lost = sorted(read | set(block) - got)[:3]
            raise ValueError(f"the generated filter {name} selects {stray or 'nothing more'} beyond what its jobs "
                             f"read and misses {lost or 'nothing'} of it")
        computed[name] = result
    for name, kept in data.EXCLUDED.items():
        computed[name] = Computed(tuple(kept.patterns))
    return computed


def quote(pattern: str) -> str:
    """A pattern as a single-quoted YAML scalar."""
    return "'" + pattern.replace("'", "''") + "'"


GENERATED_HEAD = (
    f"# Every filter of .github/workflows/ci.yaml's changes job, computed by `{GENERATE}` from",
    "# what each job reads: the files of .github/ci-gates, the //ci: lines of the Go tests and go list.",
    "# The changes job writes this when it runs, and its paths-filter step reads it. No file of the",
    "# repository holds it, and no hand edits it: change what a job is declared to read instead.",
)


def render_filters(computed: dict) -> str:
    """The text of the generated filters file: each filter a key of its own, sorted, its patterns
    single-quoted, its listed block between the two lines check-ts-filters.sh finds it by."""
    lines = list(GENERATED_HEAD)
    for name in sorted(computed):
        c = computed[name]
        lines.append(f"{name}:")
        lines += [f"  - {quote(p)}" for p in c.positives]
        if c.listed:
            lines.append(f"  {TS_LISTED.begin} by `{TS_LISTED.write}`: the files of its tree the tsc programs and the "
                         f"bundles paired with it compile and load")
            lines += [f"  - {quote(p)}" for p in c.listed]
            lines.append(f"  {TS_LISTED.end}")
        lines += [f"  - {quote(p)}" for p in c.negations]
    return "".join(line + "\n" for line in lines)


@dataclass
class Block:
    """One pattern list in the generated file's text: where its listed block is and what it holds."""

    indent: str = ""  # the indentation of the list's items
    after: int = -1  # the list's last item line outside a block
    begin: int = -1
    end: int = -1
    patterns: list = field(default_factory=list)


# A filter of the generated file: a key at the top level of it, its value the list below it.
FILTER_KEY = re.compile(r"^([A-Za-z0-9_]+):\s*(?:&\S+\s*)?$")
FILTER_ITEM = re.compile(r"^(\s*)-\s+(.*?)\s*$")


def indent_of(line: str) -> int:
    return len(line) - len(line.lstrip())


def list_block(lines: list, body: range, block: Block, path: str, marks: Marks = TS_LISTED) -> Block:
    """`block` filled in from the lines of one pattern list of `path`'s text: its items'
    indentation, its last item outside a block of `marks`' kind, and that block's own two lines and
    patterns."""
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
    return block


def filter_blocks(lines: list, problems: list[Annotation], marks: Marks = TS_LISTED,
                  path: str = GENERATED_FILE) -> dict:
    """Per filter of a generated filters file's `lines`, where its list ends and which of its
    lines are a block of `marks`' kind. A filter's name is a key at the top level of the file, so
    each such key opens a list and the next one closes it. A block whose BEGIN line has no END line
    after it is refused and read as no block."""
    starts = [i for i, line in enumerate(lines) if FILTER_KEY.match(line)]
    if not starts:
        problems.append(error(path, "no filter at the top level of this file, where the changes job's paths-filter "
                                    "step reads the lists it selects its jobs by"))
        return {}
    blocks = {FILTER_KEY.match(lines[start]).group(1): list_block(lines, range(start + 1, end), Block(), path, marks)
              for start, end in zip(starts, starts[1:] + [len(lines)])}
    for name, block in blocks.items():
        if block.begin >= 0 and block.end < block.begin:
            problems.append(error(path, f"{name} opens a block and never closes it with a `{marks.end}` line"))
            block.begin = -1
    return blocks


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
    or architecture feature name, none of its NOT_TAGS, which the toolchain or only a patched
    build sets), every choice of its own tags is tried, and go list says again whether the file
    is compiled under it: one that is, is a miss, named with the tags that build it. go decides
    each answer; this rule's own reading of a constraint only chooses which tags to try, and it
    reads a name in any script Go accepts."""
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
    # `--` ends go's flags, as listing passes it, so a pattern is never read as a flag.
    for pkg in decode_stream(session.go(r, args + ["--", *patterns])):
        d = rel(pkg["Dir"]) if pkg.get("Dir") else None
        if d is not None:
            for field in ("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles"):
                out |= {f"{d}/{name}" for name in pkg.get(field) or []}
    return out


def check() -> int:
    # The mode check-ci-filters.sh was called in: `check`, every refusal; `write`, the same run with
    # rule 8's refusal replaced by its remedy, writing each PUSH_WORKFLOWS workflow's on.push.paths;
    # or `generate`, which computes the filters and writes them to CI_FILTERS_OUT, the changes job's
    # step: it runs only what the filters are computed from, and fails, writing nothing, on any
    # problem of that work, since the filters would then be unknown.
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

    quantifier = changes_quantifier(document, problems)
    files = repo_files()
    expander = Expander(files)
    # Every rule that decides what a filter is computed from runs in each mode: a go command no
    # Run claims, or a pin its step no longer holds, leaves what its job reads unknown.
    unreduced = check_table(documents, data, set(files), problems)
    session = Session()
    # The listed blocks need the workspace's node_modules and Bun, which the changes job installs
    # and the ci-filters job does not: only the generation lists them. No rule of the check reads
    # them, and rule 8's docs filter holds none. Which blocks it lists depends on no read the walk
    # below finds, only on which filters a gate is on, so tsc and Bun list them while the walk's go
    # commands run, rather than after them.
    listed, listed_problems = {}, []
    with ThreadPoolExecutor(max_workers=1) as beside:
        if mode == "generate":
            import ci_ts_listing
            listing = beside.submit(ci_ts_listing.listed_files, {g.filter for g in data.GATES}, listed_problems)
        gate_files, report = gate_read_sets(session, data, files, expander, unreduced)
        problems += session.problems
        session.problems = []
        # A branch made before the filters were computed keeps the committed filters file when it
        # merges main; nothing reads it, so what the branch added there selects nothing.
        if (REPO / ".github/ci-filters.yaml").exists():
            problems.append(error(".github/ci-filters.yaml", "no job reads this file any more: the changes job "
                                                             "computes the filters. Run .github/scripts/ci-reads-convert, "
                                                             "which moves what a branch added here into its gates and "
                                                             "deletes it"))
        if mode == "generate":
            listed = listing.result()
            problems += listed_problems
    excluded = {name: list(kept.patterns) for name, kept in data.EXCLUDED.items()}
    selection = Selection(excluded, {name: quantifier or "some-with-excludes" for name in excluded}, files)
    try:
        computed = compute_filters(data, gate_files, listed, files, expander, selection)
    except ValueError as exc:
        problems.append(error(None, f"cannot compute the filters: {exc}"))
        computed = {}
    filters = {name: c.patterns for name, c in computed.items()}
    quantifiers = {name: quantifier for name in filters}
    # Rule 6: the generated filters exclude with `!`, which only `some-with-excludes` reads as an
    # exclusion, and a brace in a pattern written by hand is one picomatch expands and this check
    # does not read.
    for name in sorted(filters):
        negated = [p for p in filters[name] if p.startswith("!")]
        if negated and quantifier == "some":
            problems.append(error(WORKFLOW, f"filter {name} has {negated[0]}, a `!` pattern under "
                                            f"predicate-quantifier some, where it matches every file outside it"))
        if unread_filter_patterns(filters[name]):
            problems.append(error(GATES_DIR, f"filter {name} has {unread_filter_patterns(filters[name])[0]}, "
                                             f"a brace picomatch expands and this check does not read"))
    check_dead_patterns(data, expander, problems)
    if mode == "generate":
        out = os.environ.get("CI_FILTERS_OUT", "")
        problems = list(dict.fromkeys(problems))
        for problem in problems:
            print_annotation(problem)
        if problems or not out:
            print_annotation(error(None, f"{len(problems)} problem(s): no filters written"
                                         if problems else "generate takes the path to write the filters to"))
            return 1
        Path(out).write_text(render_filters(computed))
        Path(out + ".inputs").write_text("".join(f"{path}\n" for path in engine_inputs()))
        print(f"wrote {len(computed)} filters to {out}: {sum(len(c.positives) for c in computed.values())} "
              f"patterns, {sum(len(c.listed) for c in computed.values())} listed files and "
              f"{sum(len(c.negations) for c in computed.values())} `!` patterns")
        return 0

    check_ci_ok(documents, problems)
    # Rule 22: a harness's gate reads every script the harness links, sources or imports.
    report += check_harness_reads(data, gate_files, problems)
    check_called_jobs(documents, problems)
    check_output_steps(document, filters, problems)
    check_filter_inputs(rules.directory_workflows(), problems)
    rules.check_push_conditions(documents, problems)
    rules.check_rerun_guard(document, problems)
    rules.check_cache_round_trip(documents, problems)
    rules.check_race_fill_packages(data.RACE_FILLS, problems)
    rules.check_race_fill_scripts(data.RACE_FILL_SCRIPTS, problems)
    rules.check_workspace_table(data.WORKSPACES, problems)
    rules.check_one_merge_group_workflow(problems)
    # Rule 20: a vet for another platform passes over no file a build tag would compile.
    report += check_vet_tags(session, data, problems)
    problems += session.problems
    report += check_push_workflows(data, filters, mode, problems)
    report += check_tag_runs(documents, data, filters, quantifiers, gate_files, Selection(filters, quantifiers, files),
                             listed, problems)
    # Rule 24: the references lane's spec files are ones its EXCLUDED filter matches.
    report += check_references_lane(data, quantifier, problems)
    report.append(f"{GENERATED_FILE}: {len(computed)} filters, {sum(len(c.positives) for c in computed.values())} "
                  f"patterns and {sum(len(c.negations) for c in computed.values())} `!` patterns")
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
    print(f"every ci.yaml filter is computed from what its job reads ({len(data.GATES)} gates), and every "
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
