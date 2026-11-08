"""The engine of check-ci-filters.sh, whose header says what it proves and why. ci_job_reads.py,
beside it, is the data it reads: each ci.yaml gate's go commands and static reads, and the files
tests read outside their packages. ci_workflow_rules.py, also beside it, holds the rules that
read none of that - what no `if:` of ci.yaml may say, what every job's `if:` must say, what
ci-caches.yaml must save for the entries ci.yaml restores, and that ci.yaml is the only workflow
of this repository running on a merge group - and main() runs them.

check-ci-filters.sh runs it from the repository root with this directory on PYTHONPATH, through
`import ci_filter_coverage` rather than as a script, so ci_job_reads.py's `from ci_filter_coverage
import ...` gets these very classes.
"""

from __future__ import annotations

import atexit
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path

import yaml
from workflow_paths import PathsFilter, events, glob_regex, unread_filter_patterns

WORKFLOW = ".github/workflows/ci.yaml"
# The workflow that fills, on a push to main, the Go cache families WORKFLOW's jobs restore.
CACHE_WORKFLOW = ".github/workflows/ci-caches.yaml"
REPO = Path.cwd().resolve()
SHOWN = 20  # uncovered files printed per gate


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
    step holds exactly that script, every go line of it is claimed and the runs' closures are
    what the job reads; once it differs, every run counts as reading everything a go command can."""

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


def workflow_documents(problems: list) -> dict:
    """ci.yaml and every workflow its jobs call, keyed by repository-relative path, ci.yaml
    first and the called ones in the order the jobs name them. A `uses:` naming a file this
    repository does not have is a problem of its own: the steps of that job are then nowhere,
    and every rule below would pass over them in silence."""
    documents = {WORKFLOW: yaml.safe_load((REPO / WORKFLOW).read_text()) or {}}
    for path, job_id in called_workflows(documents[WORKFLOW]).items():
        if not (REPO / path).is_file():
            problems.append(f"::error file={WORKFLOW}::job {job_id} calls {path}, which is not in the repository, "
                            f"so the steps it runs are read by nothing here")
            continue
        documents[path] = yaml.safe_load((REPO / path).read_text()) or {}
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
    (check-ci-filters.sh's header says why)."""

    problems: list = field(default_factory=list)
    cache: dict = field(default_factory=dict)
    tmp: str = ""

    def __post_init__(self):
        self.tmp = tempfile.mkdtemp(prefix="check-ci-filters-")
        atexit.register(shutil.rmtree, self.tmp, True)
        os.mkdir(f"{self.tmp}/modcache")

    def go(self, run_: Run, args: list) -> str:
        env = dict(os.environ, GOPROXY="off", GOMODCACHE=f"{self.tmp}/modcache", GOFLAGS="", GOWORK="")
        env.update(dict(run_.env))
        if run_.race:
            env["CGO_ENABLED"] = "1"  # -race needs cgo
        result = subprocess.run(["go", *args], cwd=REPO / run_.workdir, env=env, capture_output=True, text=True)
        if result.returncode != 0:
            self.problems.append(f"::error::in {run_.workdir}, go {' '.join(args)} failed, so the closure of "
                                 f"{run_.why} is unknown: {' '.join(result.stderr.split())[:500]}")
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


def decode_stream(text: str) -> list:
    decoder, index, out = json.JSONDecoder(), 0, []
    while True:
        while index < len(text) and text[index].isspace():
            index += 1
        if index >= len(text):
            return out
        obj, index = decoder.raw_decode(text, index)
        out.append(obj)


def rel(path: str) -> str | None:
    try:
        return Path(path).resolve().relative_to(REPO).as_posix()
    except ValueError:
        return None


def embed_reads(dir_rel: str, patterns, why: str) -> list:
    reads = []
    for pattern in patterns or []:
        pattern = pattern.removeprefix("all:")
        if any(c in pattern for c in "*?["):
            parent = os.path.dirname(pattern)
            reads.append(Read(f"{dir_rel}/{parent}/*" if parent else f"{dir_rel}/*", f"{why} (embed {pattern})"))
        elif (REPO / dir_rel / pattern).is_dir():
            reads.append(Read(f"{dir_rel}/{pattern}/**", f"{why} (embed {pattern})"))
        else:
            reads.append(Read(f"{dir_rel}/{pattern}", f"{why} (embed {pattern})"))
    return reads


def go_skips_dir(parent: Path, name: str) -> bool:
    """Whether the go command passes over a directory of a `<root>/...` pattern: testdata, a name
    starting with `.` or `_`, node_modules, and a nested module. ci_job_reads.py walks the race
    lane's `/...` packages by this rule too, so the two walks cannot drift."""
    return (name == "testdata" or name.startswith((".", "_")) or name == "node_modules"
            or (parent / name / "go.mod").is_file())


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
    source - the non-Go ones, which no other read names, included - minus the files of that
    package's test build, which only a run that compiles them reads. It is `<dir>/*` while the
    package has no test file, and the names themselves once it has, since a glob cannot say
    "every file but these"."""
    if not tests:
        return [Read(f"{dir_rel}/*", why)]
    return [Read(f"{dir_rel}/{name}", why) for name in sorted(os.listdir(REPO / dir_rel))
            if name not in tests and (REPO / dir_rel / name).is_file()]


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
        error = (pkg.get("Error") or {}).get("Err", "")
        if error and "build constraints exclude" not in error:
            session.problems.append(f"::error::{d}: go list cannot load {pkg.get('ImportPath')} for {run_.why}, so its "
                                    f"imports are unknown: {' '.join(error.split())[:300]}")
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
        result = subprocess.run(["git", "ls-files", "-z", *extra], cwd=REPO, capture_output=True, text=True)
        if result.returncode != 0:
            raise SystemExit(f"::error::git ls-files failed in {REPO}: {result.stderr.strip()}")
        out += result.stdout.split("\0")
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
    15 (check_tag_runs) and rule 9's second direction, through rule_required_files, ask for the
    same TAG_RUNS filters, and a sweep is a covers() call per file of the repository. An answer
    is shared with every caller, so none of them may change the list it is given."""

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


def changes_filters(document: dict, problems: list) -> tuple:
    """The filters the changes job declares, with the quantifier each was declared under. It
    reads ci.yaml alone, and no workflow ci.yaml calls: the changes job and its paths-filter
    steps are ci.yaml's, and a called file is reached only through a caller job the changes job
    already gates."""
    filters, quantifiers = {}, {}
    steps = ((document.get("jobs") or {}).get("changes") or {}).get("steps") or []
    for step in steps:
        if not str(step.get("uses", "")).startswith("dorny/paths-filter"):
            continue
        options = step.get("with") or {}
        quantifier = str(options.get("predicate-quantifier", "some"))
        if quantifier not in PathsFilter.QUANTIFIERS:
            problems.append(f"::error file={WORKFLOW}::the changes job's paths-filter step {step.get('id')} has "
                            f"predicate-quantifier {quantifier}, which this check does not model")
            continue
        for name, value in (yaml.safe_load(str(options.get("filters", ""))) or {}).items():
            if name in filters:
                problems.append(f"::error file={WORKFLOW}::filter {name} is defined in two paths-filter steps")
            filters[name], quantifiers[name] = flatten(value), quantifier
    if not filters:
        problems.append(f"::error file={WORKFLOW}::no changes job with a dorny/paths-filter step")
    return filters, quantifiers


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


def input_expressions(documents: dict, problems: list) -> dict:
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
                    problems.append(f"::error file={WORKFLOW}::job {caller} passes {name}: {passed[name]} to {path}, "
                                    f"whose condition reads inputs.{name}: such a condition is read through the "
                                    f"caller's with:, so a value naming the changes job must be "
                                    f"${{{{ needs.changes.outputs.<filter> }}}} or ${{{{ needs.changes.result }}}}, "
                                    f"which this check can resolve")
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


def script_reason(text: str) -> str:
    """'' when a run: script holds only printable ASCII, tabs and newlines; else the first other
    character, which Python and bash may split, strip or read differently (a \\r, a NBSP)."""
    odd = next((c for c in text if c not in "\t\n" and not " " <= c <= "~"), None)
    return f"its step's script holds the character U+{ord(odd):04X}" if odd else ""


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
# reduced: names that cannot change what the command compiles. Any other name falls to everything,
# among them GO* and CGO_*, BASH_ENV and ENV (which bash sources before the script), HOME and
# XDG_CONFIG_HOME (go reads its env file, GOFLAGS among it, from $XDG_CONFIG_HOME/go/env or else
# $HOME/.config/go/env) and PATH (which chooses the go binary). Add a name here only when go
# compiles the same files with it set as without.
#
# GOPROXY is the one name go itself reads that is here. It says where a module's bytes come from,
# never which version: the module files this check already reads decide that, and go.sum holds
# each module's hash whichever source serves it. This check's own go commands run with
# GOPROXY=off over an empty module cache (Go.env below), so a workflow's value never reaches
# them either. CI_OK_SELECTED_BY, CI_OK_PULL_REQUEST_ONLY, CI_OK_ALWAYS_RUN and CI_OK_EXEMPT are
# ci.yaml's ci-ok classes: ci-ok-report.sh reads the first three out of that file and hands them
# to ci-ok.sh, and CI_OK_EXEMPT is read by this check alone. LEGION_RUN_FUZZ_ITERS and
# LEGION_KEPT_PROPERTY_RUNS are the counts the pull-request lane reduces: each is read at run
# time by the test it governs, so go compiles the same files with them set as without.
ENV_ALLOWED = frozenset({
    "AIRLOCK_MODEL",
    "BROKER_TEST_DATABASE_URL", "CI_OK_ALWAYS_RUN", "CI_OK_EXEMPT", "CI_OK_PULL_REQUEST_ONLY", "CI_OK_SELECTED_BY",
    "DATABASE_URL",
    "DISPATCH_E2E_FILE_STORE_BUCKET", "DISPATCH_E2E_FILE_STORE_ENDPOINT",
    "DISPATCH_TEST_DATABASE_URL", "ENVOY_INFRA_DOCKER_PLUGIN", "ENVOY_SMOKE_HELD_PORT_IMAGE", "ENVOY_SMOKE_IMAGE",
    "GOPROXY",
    "LEGION_E2E", "LEGION_E2E_ENVOY_STACK", "LEGION_E2E_PG_DSN", "LEGION_KEPT_PROPERTY_RUNS",
    "LEGION_RUN_FUZZ_ITERS", "LEGION_TEST_NATS_URL", "LEGION_TEST_PG_DSN",
    "TESTCONTAINERS_RYUK_DISABLED",
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
    whose comment names $GITHUB_ENV to say it writes none (dispatch-browser.yaml's install steps)
    writes none here either."""
    for step in job.get("steps") or []:
        for line in script_lines(str(step.get("run", ""))):
            written = ENV_WRITE.search(line)
            if written:
                return f'step "{step_label(step)}" runs `{written.group(0)}`, which every later step inherits'
    return ""


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


def around_job(units: list, owner: str) -> str:
    """The first thing around a ci.yaml job that can change what a go command it starts compiles,
    over its own steps and those of a workflow it calls, each read under the env:, defaults and
    container of the file that holds it; or ''."""
    for own, _, _, holder, job in units:
        reason = around_reason(holder, job, job.get("steps") or []) if own == owner else ""
        if reason:
            return reason
    return ""


# ---------------------------------------------------------------- the check
def check_table(documents: dict, data, filters: dict, files: set, problems: list) -> dict:
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
        problems.append(f"::error file=.github/scripts/ci_job_reads.py::{name} is both a gate and EXCLUDED")
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
                        problems.append(f"::error file={path}::{where} is gated on filter {name}, which "
                                        f"ci_job_reads.py neither lists in GATES nor EXCLUDES; add the gate with its reads")
    for g in data.GATES:
        if g.filter not in filters:
            problems.append(f"::error file={WORKFLOW}::{g.job} has no filter {g.filter} in the changes job")
        # The caller job is ci.yaml's, whichever file holds the steps: it keeps the job id and
        # the `if:` that tests the filter, so both are looked for there.
        if g.job not in caller_jobs:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::gate {g.filter} names job {g.job}, "
                            f"which {WORKFLOW} does not have")
            continue
        steps_document = documents.get(g.workflow)
        if steps_document is None:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::gate {g.filter} holds job {g.job}'s "
                            f"steps in {g.workflow}, which {WORKFLOW} neither is nor calls")
            continue
        job = (steps_document.get("jobs") or {}).get(g.job)
        if job is None:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::gate {g.filter} names job {g.job}, "
                            f"which {g.workflow} does not have")
            continue
        if g.step:
            steps = [s for s in job.get("steps") or [] if step_matches(s, g.step)]
            if len(steps) != 1 or g.filter not in GATE_REF.findall(
                    resolve_condition(str(steps[0].get("if", "")), inputs_of.get(g.workflow, {}))):
                problems.append(f"::error file={g.workflow}::gate {g.filter} names step \"{g.step}\" of {g.job}, but "
                                f"no single step of that name has an if: on needs.changes.outputs.{g.filter}")
        elif g.filter not in gated[(WORKFLOW, g.job)]:
            problems.append(f"::error file={WORKFLOW}::job {g.job}'s if: does not test needs.changes.outputs.{g.filter}")
    for name in excluded:
        if name not in filters:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::EXCLUDED names filter {name}, which "
                            f"the changes job does not have")

    # Every go command line of a run: step must be claimed: by a pinned Run of a gate on that job,
    # or by an Exact entry pinning the step's whole script. A line claimed by a Run is reduced only
    # in its one simple form, the whole script of its step, under an env, job, shell and directory
    # that cannot move it; its packages, -tags, -race and mode must then be exactly the Run's. Any
    # other claimed command is unreduced and falls to everything a go command can read, as does
    # every run of an Exact entry whose step's script, directory or surroundings differ.
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
                problems.append(f"::error file=.github/scripts/ci_job_reads.py::gate {g.filter}'s run "
                                f"{r.workdir} {' '.join(r.pkgs)} is pinned to no step or source text")
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
            around = around_reason(holder, job, [step]) or script_reason(script)
            entries = [fe for (j, ref), fes in exacts.items() if j == own and step_matches(step, ref) for fe in fes]
            for name, e in entries:
                reason = (around or ("" if script.rstrip() == e.script.rstrip() else "the step's script is not the pinned text")
                          or ("" if workdir == clean_dir(e.workdir) else f"its step runs in {workdir}, the entry's in {e.workdir}"))
                if reason:
                    where = f"{where_job(path, name_there, own)} step \"{step_label(step)}\"'s pinned script"
                    unreduced[name] += [(r, where, reason) for r in e.runs]
            if entries:
                continue  # every go line of the step is claimed by its Exact entries
            mine = [r for (j, ref), rs in claims.items() if j == own and step_matches(step, ref) for r in rs]
            for line in go_lines(script):
                where = f"{where_job(path, name_there, own)} step \"{step_label(step)}\" runs `{line}`"
                pinned = [r for r in mine if squash(r.text) in squash(line)]
                if not pinned:
                    problems.append(f"::error file={path}::{where}, which no Run in ci_job_reads.py lists; add it "
                                    f"to the job's gate so its closure is checked")
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
                        problems.append(f"::error file={path}::{where}, which is not the Run's "
                                        f"({' '.join(r.pkgs)}): {'; '.join(parts)}; make the Run in ci_job_reads.py "
                                        f"name exactly the packages, tags, -race and mode the line does, so the closure "
                                        f"checked against the job's filter is the one it compiles")
    for (job_id, ref), fes in exacts.items():
        steps = [s for s in owner_steps(units, job_id) if "run" in s and step_matches(s, ref)]
        if len(steps) != 1:
            for name, _ in fes:
                problems.append(f"::error file=.github/scripts/ci_job_reads.py::gate {name}'s exact entry names step "
                                f"\"{ref}\" of job {job_id}, which is not one run: step of it; update or remove the entry")
    for (job_id, ref), rs in claims.items():
        steps = [s for s in owner_steps(units, job_id) if step_matches(s, ref)]
        text = squash(" ".join(str(s.get("run", "")) for s in steps))
        for r in rs:
            if not steps or squash(r.text) not in text:
                problems.append(f"::error file=.github/scripts/ci_job_reads.py::job {job_id} step \"{ref}\" no longer "
                                f"runs `{r.text}`; update or remove the Run")
    # A script's go command is held exact rather than reduced: the line holding the pinned text is
    # that text alone, uncontinued, and no env:, shell: or defaults.run.shell of the workflow, the
    # job or any of its steps (the check does not know which starts the script), no container: and
    # no environment write of the job can move it. A job that calls a workflow runs the script from
    # that workflow's jobs, so each of them is read the same way. Its variables are the Run author's.
    for g in data.GATES:
        for r in g.runs:
            if not r.source:
                continue
            if not (r.source in files and squash(r.text) in squash((REPO / r.source).read_text())):
                problems.append(f"::error file={r.source}::{r.source} no longer holds `{r.text}`, the go command "
                                f"gate {g.filter} lists; update or remove the Run")
                continue
            lines = (REPO / r.source).read_text().split("\n")
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
    for source in data.RULES:
        if source not in files:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::RULES names {source}, which is not "
                            f"in the repository; update or remove the rule")
    # A citation names the workflow file that holds the step: ci.yaml, or a workflow one of its
    # jobs calls. The step must still be there, so a rename or a move fails here. The set already
    # holds every workflow ci.yaml calls, so only a citation naming another file is read here.
    cited_documents = dict(documents)
    for path, job_id, ref in CITED:
        if path not in cited_documents:
            cited_documents[path] = (yaml.safe_load((REPO / path).read_text()) or {}) if (REPO / path).is_file() else None
        cited = cited_documents[path]
        if cited is None:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::a why cites step \"{ref}\" of job "
                            f"{job_id} in {path}, which is not in the repository")
            continue
        if not any(step_matches(s, ref) for s in ((cited.get("jobs") or {}).get(job_id) or {}).get("steps") or []):
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::a why cites step \"{ref}\" of job "
                            f"{job_id}, which {path} does not have")
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


def check_ci_ok(documents: dict, problems: list) -> None:
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
    except json.JSONDecodeError as error:
        problems.append(f"::error file={WORKFLOW}::the env's CI_OK_SELECTED_BY, {EXEMPT_LIST}, {ALWAYS_RUN_LIST} "
                        f"or {' or '.join(EVENT_LISTS)} is missing or not JSON: {error}")
        return
    if not (isinstance(selected_by, dict) and all(isinstance(v, str) for v in selected_by.values())):
        problems.append(f"::error file={WORKFLOW}::the env's CI_OK_SELECTED_BY is not a JSON object of job to "
                        f"output name")
        return
    if not (isinstance(exempt, list) and all(isinstance(j, str) for j in exempt)):
        problems.append(f"::error file={WORKFLOW}::the env's {EXEMPT_LIST} is not a JSON array of job names")
        return
    if not (isinstance(always_run, list) and all(isinstance(j, str) for j in always_run)):
        problems.append(f"::error file={WORKFLOW}::the env's {ALWAYS_RUN_LIST} is not a JSON array of job names")
        return
    for name, jobs_listed in listed.items():
        if not (isinstance(jobs_listed, list) and all(isinstance(j, str) for j in jobs_listed)):
            problems.append(f"::error file={WORKFLOW}::the env's {name} is not a JSON array of job names")
            return
    for job_id in sorted(set(exempt) - set(jobs)):
        problems.append(f"::error file={WORKFLOW}::{EXEMPT_LIST} names {job_id}, which is no job of this file; "
                        f"drop it, or name the job it was meant for")
    for job_id in sorted(set(always_run) - set(jobs)):
        problems.append(f"::error file={WORKFLOW}::{ALWAYS_RUN_LIST} names {job_id}, which is no job of this file; "
                        f"drop it, or name the job it was meant for")
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
            problems.append(f"::error file={WORKFLOW}::job {job_id} is in none of ci-ok's three classes, so "
                            f"ci-ok.sh fails its result whatever the job did: {remedies}")
        else:
            problems.append(f"::error file={WORKFLOW}::job {job_id} is in {len(held)} of ci-ok's three classes "
                            f"({', '.join(held)}), which judge it differently, and it belongs in exactly one: "
                            f"{remedies}")
    for job_id in sorted(set(always_run) & set(jobs)):
        gate = GATE_REF.search(str((jobs.get(job_id) or {}).get("if", "")))
        if gate is not None:
            problems.append(f"::error file={WORKFLOW}::{ALWAYS_RUN_LIST} names {job_id}, whose if: tests "
                            f"needs.changes.outputs.{gate.group(1)}, so changes gates it rather than selecting "
                            f"every run of it, and ci-ok.sh would fail every run that rightly skipped it; map it "
                            f"in CI_OK_SELECTED_BY instead")
    for job_id, job in jobs.items():
        named = str((job or {}).get("name", job_id))
        if job_id in REQUIRED_CHECKS:
            if named != job_id:
                problems.append(f"::error file={WORKFLOW}::job {job_id} carries `name: {named}`, so its check run "
                                f"is called {named} and {job_id}, a check main's ruleset requires, is reported by "
                                f"nothing; drop the name:")
            continue
        if named in REQUIRED_CHECKS:
            problems.append(f"::error file={WORKFLOW}::job {job_id} is named {named}, a check main's ruleset "
                            f"requires, and is not the {named} job: a job posts its own check, and a skipped one "
                            f"posts a pass, so the job of that name is the only one that may carry it")
    for check in REQUIRED_CHECKS:
        if check != VERDICT_JOB and check not in jobs:
            problems.append(f"::error file={WORKFLOW}::this file has no {check} job, a check main's ruleset "
                            f"requires: its own check run is that check on a pull request and on a merge group, "
                            f"and the queue cannot merge a group nothing reports it on")
    verdict = jobs.get(VERDICT_JOB)
    if verdict is None:
        problems.append(f"::error file={WORKFLOW}::this file has no {VERDICT_JOB} job, the check main's ruleset "
                        f"requires: it needs every other job, fails when ci-ok.sh's verdict over their results "
                        f"is failure, and so carries that verdict in this run's own check suite, where a posted "
                        f"check run joins the first suite at the commit")
    else:
        needed = verdict.get("needs") or []
        needed = [needed] if isinstance(needed, str) else [str(job_id) for job_id in needed]
        want = set(jobs) - {VERDICT_JOB} - set(exempt)
        if VERDICT_JOB not in exempt:
            problems.append(f"::error file={WORKFLOW}::{EXEMPT_LIST} does not name {VERDICT_JOB}, which reports no "
                            f"verdict to itself: every other job ends with the report step and is judged through "
                            f"this one's needs:, so add it there")
        for job_id in sorted(want - set(needed)):
            problems.append(f"::error file={WORKFLOW}::job {VERDICT_JOB} does not need {job_id}, so it reaches the "
                            f"verdict while that job is still running and says nothing about it; add {job_id} to "
                            f"its needs:")
        for job_id in sorted(set(needed) - want):
            why = (f"{EXEMPT_LIST} holds it outside ci-ok's accounting, and ci-ok.sh fails a result for a job "
                   f"CI_OK_SELECTED_BY does not map" if job_id in exempt else "it is no job of this file")
            problems.append(f"::error file={WORKFLOW}::job {VERDICT_JOB} needs {job_id}, but {why}; drop it from "
                            f"its needs:")
    gated = sorted(job_id for job_id, job in jobs.items()
                   if job_id not in always_run and job_id not in exempt
                   and GATE_REF.search(str((job or {}).get("if", ""))))
    expected, list_expected = {}, {name: set() for name in EVENT_LISTS}
    for job_id in gated:
        condition = str((jobs.get(job_id) or {}).get("if", ""))
        outputs = sorted(set(GATE_REF.findall(condition)))
        if len(outputs) != 1:
            problems.append(f"::error file={WORKFLOW}::job {job_id}'s if: tests {len(outputs)} outputs of changes "
                            f"({', '.join(outputs)}), so CI_OK_SELECTED_BY cannot say what selects it")
            continue
        expected[job_id] = outputs[0]
        for name, (term, _, _, _) in EVENT_LISTS.items():
            if term.search(condition):
                list_expected[name].add(job_id)
    for job_id in sorted(set(exempt) & set(selected_by)):
        problems.append(f"::error file={WORKFLOW}::CI_OK_SELECTED_BY maps {job_id}, which {EXEMPT_LIST} holds "
                        f"outside ci-ok's accounting, so job {VERDICT_JOB} never needs it and ci-ok.sh fails on a "
                        f"job its map names and NEEDS lacks; drop it from one of the two")
    for job_id in sorted((set(gated) | set(selected_by)) - set(exempt)):
        want, have = expected.get(job_id), selected_by.get(job_id)
        if job_id not in gated:
            problems.append(f"::error file={WORKFLOW}::CI_OK_SELECTED_BY maps {job_id}, which is no job whose if: "
                            f"tests an output of changes; drop it")
        elif want is None or want == have:
            continue
        elif have is None:
            problems.append(f"::error file={WORKFLOW}::CI_OK_SELECTED_BY does not map {job_id}, whose if: tests "
                            f"needs.changes.outputs.{want}, so ci-ok.sh does not know what selects it and fails "
                            f"its result; add \"{job_id}\": \"{want}\"")
        else:
            problems.append(f"::error file={WORKFLOW}::CI_OK_SELECTED_BY maps {job_id} to {have}, but its if: tests "
                            f"needs.changes.outputs.{want}")
    for name, (_, admits, text, where) in EVENT_LISTS.items():
        for job_id in sorted(list_expected[name] - set(listed[name])):
            problems.append(f"::error file={WORKFLOW}::{name} lacks {job_id}, whose if: {admits}, so ci-ok.sh "
                            f"fails its skip on {where}")
        for job_id in sorted(set(listed[name]) - list_expected[name]):
            problems.append(f"::error file={WORKFLOW}::{name} lists {job_id}, whose if: has no {text} term, so "
                            f"ci-ok.sh accepts its skip on {where} even when changes selected it")
    for job_id, job in jobs.items():
        job = job or {}
        if job_id == "changes":
            continue
        if job_id in exempt:
            # An exempt job must report nothing: ci-ok.sh fails a result it cannot map to a
            # CI_OK_SELECTED_BY entry, and the job has none.
            if (str((job.get("with") or {}).get("ci-ok-changes", "")) != ""
                    or any((step or {}).get("uses") == REPORT_ACTION for step in job.get("steps") or [])):
                problems.append(f"::error file={WORKFLOW}::job {job_id} is in {EXEMPT_LIST}, yet it reports to "
                                f"ci-ok, which fails a result for a job CI_OK_SELECTED_BY does not map; drop the "
                                f"{REPORT_ACTION} step (and any `ci-ok-changes`), or drop {job_id} from "
                                f"{EXEMPT_LIST}")
            continue
        if "uses" in job:
            if str((job.get("with") or {}).get("ci-ok-changes", "")) != REPORT_CHANGES:
                problems.append(f"::error file={WORKFLOW}::job {job_id} calls {job['uses']} without "
                                f"`ci-ok-changes: {REPORT_CHANGES}`, so the called job cannot report to ci-ok")
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
                problems.append(f"::error file={WORKFLOW}::{job['uses']}, which job {job_id} calls, has no job "
                                f"whose last step is `uses: {REPORT_ACTION}` with an `if:` that starts with "
                                f"`always()`, `job: {job_id}`, `changes: ${{{{ inputs.ci-ok-changes }}}}` and "
                                f"`job-status: {REPORT_STATUS}`, so a merge-group run {job_id}'s own result "
                                f"decides gets no failing ci-ok until the {VERDICT_JOB} job has waited for every "
                                f"other job of the run")
            continue
        steps = job.get("steps") or []
        last = steps[-1] if steps else {}
        given = last.get("with") or {}
        if (last.get("uses") != REPORT_ACTION or str(last.get("if", "")) != "always()"
                or str(given.get("changes", "")) != REPORT_CHANGES or str(given.get("job-status", "")) != REPORT_STATUS):
            problems.append(f"::error file={WORKFLOW}::job {job_id}'s last step is not `uses: {REPORT_ACTION}` with "
                            f"`if: always()`, `changes: {REPORT_CHANGES}` and `job-status: {REPORT_STATUS}`, so a "
                            f"merge-group run {job_id}'s own result decides gets no failing ci-ok until the "
                            f"{VERDICT_JOB} job has waited for every other job of the run")


def check_push_workflows(data, filters: dict, problems: list) -> list:
    """A PUSH_WORKFLOWS workflow does on a push to main what its filter's job does on a pull request
    and a merge group, so its on.push.paths must name exactly the filter's patterns: a pattern only
    the filter has lets a change pass the pull request's build and never reach main's, and one only
    the paths have runs main's for a change no pull request built. Returns the report lines."""
    report = []
    for name, path in data.PUSH_WORKFLOWS.items():
        if name not in filters:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::PUSH_WORKFLOWS names filter {name}, which "
                            f"the changes job does not have")
            continue
        if not (REPO / path).is_file():
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::PUSH_WORKFLOWS names {path} for filter "
                            f"{name}, which is not in the repository")
            continue
        push = events(yaml.safe_load((REPO / path).read_text()) or {}).get("push")
        paths = push.get("paths") if isinstance(push, dict) else None
        if not (isinstance(paths, list) and paths and all(isinstance(p, str) for p in paths)):
            problems.append(f"::error file={path}::{path} has no on.push.paths list, which must equal {WORKFLOW}'s "
                            f"filter {name}")
            continue
        missing = [p for p in dict.fromkeys(filters[name]) if p not in paths]
        extra = [p for p in dict.fromkeys(paths) if p not in filters[name]]
        for p in missing:
            problems.append(f"::error file={path}::{path}'s on.push.paths lacks {p}, which {WORKFLOW}'s filter {name} "
                            f"has, so a push to main that changes only a file it matches never runs {path}")
        for p in extra:
            problems.append(f"::error file={path}::{path}'s on.push.paths has {p}, which {WORKFLOW}'s filter {name} "
                            f"lacks; add it to the filter or drop it here")
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


def check_tag_runs(documents: dict, data, filters: dict, quantifiers: dict, selection: Selection,
                   problems: list) -> list:
    """A step that runs a package under a build tag and passes `-run` runs only the tests the tag
    adds, so the package's untagged tests must run elsewhere in every run that step runs. The job
    that runs them is declared (TAG_RUNS), and its filter must cover every file the tag step's
    filter covers; otherwise a change to a file only the tag step's filter matches runs the tagged
    tests alone and the untagged ones nowhere. The table is no entry's to opt into: a `go test`
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
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::TAG_RUNS names filter {name} for {where}, "
                            f"which the changes job does not have")
        tagged_jobs = (documents.get(entry.workflow) or {}).get("jobs") or {}
        # The entry's own job is held to the file the entry names, which is where its step is;
        # the untagged job only has to be a job of the set, since a job that gave its steps to a
        # called file keeps its caller in ci.yaml, and one the set does not hold at all is no
        # job of ci.yaml either, which is the file that refusal names.
        gone_jobs = [(entry.job, entry.workflow)] if entry.job not in tagged_jobs else []
        if entry.untagged_job not in every_job:
            gone_jobs.append((entry.untagged_job, WORKFLOW))
        for name, found_in in gone_jobs:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::TAG_RUNS names job {name}, which "
                            f"{found_in} does not have")
        if gone_filters or gone_jobs:
            continue
        job = tagged_jobs[entry.job] or {}
        steps = [s for s in job.get("steps") or [] if "run" in s and step_matches(s, entry.step)]
        if len(steps) != 1:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::TAG_RUNS names step \"{entry.step}\" of "
                            f"job {entry.job}, which is not one run: step of it; update or remove the entry")
            continue
        if "-run" not in str(steps[0]["run"]):
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::TAG_RUNS names {where}, which passes no "
                            f"-run, so it runs the package's untagged tests itself; drop the entry")
            continue
        untagged = PathsFilter(filters[entry.untagged_filter], quantifiers[entry.untagged_filter])
        selected = selection.of(entry.filter)
        uncovered = [f for f in selected if not untagged.covers(f)]
        report.append(f"{where}: {len(selected)} files selected by filter {entry.filter}, {len(uncovered)} "
                      f"uncovered by {entry.untagged_job} (filter {entry.untagged_filter})")
        if uncovered:
            more = f", and {len(uncovered) - SHOWN} more" if len(uncovered) > SHOWN else ""
            problems.append(f"::error file={entry.workflow}::{where} runs only what its tag adds (-run), so {entry.untagged_job} "
                            f"must run that package's untagged tests in every run it runs ({entry.why}), but filter "
                            f"{entry.untagged_filter} does not match {' '.join(uncovered[:SHOWN])}{more}, which filter "
                            f"{entry.filter} matches: a change to one of them runs the tagged tests alone and the "
                            f"untagged ones nowhere")
    # The other side of the rule: an entry holds the step it names, and this holds every step no
    # entry names - a step added without one, or an entry deleted while its step stayed. A step
    # whose entry is itself stale is named all the same, so it keeps the refusal above rather
    # than drawing a second, confusing one.
    for path, name, step in tag_run_steps(documents):
        if any((entry.workflow, entry.job) == (path, name) and step_matches(step, entry.step) for entry in data.TAG_RUNS):
            continue
        problems.append(f"::error file={path}::{name} step \"{step_label(step)}\" runs only what its tag adds "
                        f"(-run), but no TAG_RUNS entry of .github/scripts/ci_job_reads.py names it: the job that "
                        f"runs that package's untagged tests must be declared there, so the check holds it to "
                        f"running in every run this step runs")
    return report


def declared_reads(data) -> list:
    """Every read pattern ci_job_reads.py declares, each with where it is: a gate's reads, or a
    RULES row's (a row's Runs declare go commands, not patterns)."""
    reads = [(r.glob, f"gate {g.filter} reads") for g in data.GATES for r in g.reads]
    return reads + [(read.glob, f"the RULES row for {source} reads")
                    for source, entries in data.RULES.items()
                    for read in entries if isinstance(read, Read)]


def check_dead_patterns(data, filters: dict, expander: Expander, problems: list) -> None:
    """Rule 9's first direction: a pattern that matches no file of the repository names nothing
    the run can select or cover. The other rules here pass over such a pair: a filter and a
    declared read that both name a path that does not exist agree with each other, and the gate's
    read expands to no file to hold the filter to, so a change to the files they mean starts no
    job, on the pull request and again on main. Judged over repo_files() with the Expander the
    rest of the check uses, so a pattern is held to exactly the files a dorny/paths-filter run
    sees. A `!` pattern is exempt: it names what to leave out, and leaving nothing out is no
    fault. It reads the filter block of ci.yaml alone, and no workflow ci.yaml calls: the filters
    are the changes job's, and a declared read is the table's rather than any workflow's.

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
            problems.append(f"::error file={WORKFLOW}::filter {name} has {pattern}, which matches no file in the "
                            f"repository, so no change ever selects on it; correct the path or drop the pattern")
    for pattern, where in dict.fromkeys(declared_reads(data)):
        if expander.expand(pattern):
            continue
        problems.append(f"::error file=.github/scripts/ci_job_reads.py::{where} {pattern}, which matches no file in "
                        f"the repository, so it holds its filter to nothing; correct the path or drop the read")


def rule_required_files(data, filters: dict, selection: Selection) -> dict:
    """Per filter, the files another rule of this check holds that filter to whatever its job
    reads, which rule 9's second direction has to pass over: refusing one of those patterns
    would be refusing the rule that put it there.

    Two rules do it. Rule 8 keeps a PUSH_WORKFLOWS workflow's on.push.paths equal to its filter,
    and a workflow that does not name itself never runs on a change to itself: the docs filter
    holds .github/workflows/docs-pages.yaml so that main's docs build runs when its own workflow
    changes, which is a run of that file rather than a read by ci.yaml's docs job. Rule 15 holds
    the filter of the job that runs a package's untagged tests to every file the tag step's
    filter covers, so a change that starts the tagged run starts the untagged one with it:
    envoy_go_race holds .github/scripts/go-tagged-tests.sh, which listener-server-change's soak
    takes its -run from and no step of envoy-go-race reads.

    This widens to nothing else. A pattern is passed over only when every file it matches is in
    this set, so one that reaches a single file outside it is judged like any other."""
    required: dict = defaultdict(set)
    for name, path in data.PUSH_WORKFLOWS.items():
        if name in filters:
            required[name].add(path)
    for entry in data.TAG_RUNS:
        if entry.filter in filters and entry.untagged_filter in filters:
            required[entry.untagged_filter].update(selection.of(entry.filter))
    return required


def check_dead_gate_patterns(data, filters: dict, gate_files: dict, expander: Expander,
                             selection: Selection, problems: list) -> None:
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
    pattern whose every file another rule of this check holds the filter to (rule_required_files
    above), which is the one case where a pattern matching no read is right rather than idle."""
    required = rule_required_files(data, filters, selection)
    for name in sorted(gate_files):
        reads, wheres = gate_files[name].reads, gate_files[name].wheres
        where = " and ".join(wheres) + (" read" if len(wheres) > 1 else " reads")
        for pattern in dict.fromkeys(filters[name]):
            matched = expander.expand(pattern) if not pattern.startswith("!") else []
            if not matched or not reads.isdisjoint(matched) or set(matched) <= required.get(name, frozenset()):
                continue
            problems.append(f"::error file={WORKFLOW}::filter {name} has {pattern}, which matches no file {where}, "
                            f"though it matches {len(matched)} of the repository's, so a change to one of them runs "
                            f"that job over a change no step of it sees; drop the pattern")


@dataclass
class GateFiles:
    """Per filter, what rule 9's second direction is judged over: the union of the files the
    gates on it read, and how the check names each of those gates."""

    reads: set = field(default_factory=set)
    wheres: list = field(default_factory=list)


def main() -> int:
    problems: list = []
    # ci.yaml and every workflow its jobs call: a job that calls one keeps its gate, its id and
    # its if: here and holds its steps there, so the rules that read steps read the set.
    documents = workflow_documents(problems)
    document = documents[WORKFLOW]
    import ci_job_reads as data  # imported here so its at() citations land in this module's CITED
    # The rules of ci.yaml that read no filter and no go command's closure, in a module of their
    # own. It is imported here, not at the top, because it imports this module's constants and
    # helpers: by the time main() runs, this module is loaded whole.
    import ci_workflow_rules as rules

    filters, quantifiers = changes_filters(document, problems)
    files = repo_files()
    unreduced = check_table(documents, data, filters, set(files), problems)
    check_ci_ok(documents, problems)
    rules.check_push_conditions(documents, problems)
    rules.check_rerun_guard(document, problems)
    rules.check_cache_round_trip(documents, problems)
    rules.check_race_fill_packages(data.DAEMON_RACE_PACKAGES, problems)
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
            problems.append(f"::error file={WORKFLOW}::filter {g.filter} has {negated[0]}, a `!` pattern under "
                            f"predicate-quantifier some, where it matches every file outside it")
        if unread_filter_patterns(patterns):
            problems.append(f"::error file={WORKFLOW}::filter {g.filter} has {unread_filter_patterns(patterns)[0]}, "
                            f"a brace picomatch expands and this check does not read")
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
            held.append(f"::error file={WORKFLOW}::{why}; filter {g.filter} must select them all on any Go change")
        matcher = PathsFilter(patterns, quantifiers[g.filter])
        uncovered = sorted(f for f in reason if not matcher.covers(f))
        where = g.job + (f' step "{g.step}"' if g.step else "")
        gate_files[g.filter].reads.update(reason)
        gate_files[g.filter].wheres.append(where)
        report.append(f"{where}: {len(reason)} files, {len(uncovered)} uncovered (filter {g.filter})")
        for f in uncovered[:SHOWN]:
            problems.append(f"::error file={f}::{where} reads {f}, which its filter {g.filter} does not match, so a "
                            f"change to it skips the job: {reason[f]}")
        if len(uncovered) > SHOWN:
            problems.append(f"::error file={WORKFLOW}::{where} reads {len(uncovered) - SHOWN} more files its filter "
                            f"{g.filter} does not match")
        if held and not set(fallback).isdisjoint(uncovered):
            problems += held
    check_dead_gate_patterns(data, filters, gate_files, expander, selection, problems)
    problems += session.problems
    report += check_push_workflows(data, filters, problems)
    report += check_tag_runs(documents, data, filters, quantifiers, selection, problems)
    for line in report:
        print(line)
    problems = list(dict.fromkeys(problems))
    for problem in problems:
        print(problem)
    if problems:
        print(f"::error::{len(problems)} CI filter problem(s)")
        return 1
    print(f"every ci.yaml filter matches every file its job reads ({len(data.GATES)} gates), and every "
          f"PUSH_WORKFLOWS workflow's on.push.paths equals its filter ({len(data.PUSH_WORKFLOWS)})")
    return 0
