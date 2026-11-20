"""The rules about the Go module caches: which fetch files each Go module cache fill of
ci-caches.yaml must run (check_go_fetch_fills), and which files each Go module cache key must hash
(check_go_cache_keys). check-ci-filters.sh's header numbers them 22 and 23. Neither reads a filter,
a gate or the closure of a go command, so neither belongs in the filter-coverage engine; each reads
ci.yaml and every workflow it calls, followed through each call (required_workflows), the key rule
with ci-caches.yaml and docs-pages.yaml beside them.

It sits beside that engine (ci_filter_coverage.py) and ci_workflow_rules.py in the repository it
checks, and imports the engine's names for the workflow files, the repository root and the readers
of a step, and ci_workflow_rules.py's reading of a cache family, of a cache step's paths and of the
workflow directory. ci_filter_coverage.main() imports this module and runs both rules, as it runs
ci_workflow_rules.py's, so the import of the engine here is of a module Python has finished loading
by then.
"""

import posixpath
import re
from collections import Counter, defaultdict
from dataclasses import dataclass
from functools import cache

from ci_filter_coverage import (
    CACHE_WORKFLOW,
    REPO,
    WORKFLOW,
    called_workflows,
    error,
    load_repository_yaml,
    read_repository_text,
    script_lines,
    step_label,
    step_workdir,
)
from ci_workflow_rules import cache_family, directory_workflows, path_lines


# ------------------------------------------- the Go module cache fills and the fetch files they serve
# A Go family is a cache family whose path: holds GO_MODULE_CACHE. A job reads its entry when it
# restores it (actions/cache/restore, or actions/cache, which restores before the job's later
# steps and saves after them), and the entry is there so that the job's fetch downloads nothing.
# That fetch is each `go-fetch.sh file <fetch file>` line of the job's steps after the restore; a
# fetch file is the list of go-fetch.sh's own `deps`, `tools` and `tidy` commands kept under
# .github/go-fetch/ (FETCH_COMMAND below). A go-fetch.sh line is a line of a step's run: script
# that names GO_FETCH, read through script_lines, so a comment is none, and its fetch file is
# resolved against the step's working directory (step_workdir: its own, else the job's or the
# workflow's defaults.run, else the root), since the same words run in another directory name
# another file. go-fetch.sh's `deps` and `tools` modes take their arguments on the line itself,
# in no file a cache key hashes: a change to them changes no key, and the entry found is one
# without the modules they now name. So a line in either mode after a restore, or in a fill, is
# refused. Its `pulumi-consumer` mode builds a Pulumi consumer program in a directory of its own
# at run time: it is allowed, and it is no fetch of the entry.
GO_MODULE_CACHE = "~/go/pkg/mod"
GO_FETCH = ".github/scripts/go-fetch.sh"
FILE_MODE = "file"
ARGUMENT_MODES = ("deps", "tools")
RUNTIME_MODES = ("pulumi-consumer",)
RESTORES = ("actions/cache/restore", "actions/cache")
SAVES = ("actions/cache/save", "actions/cache")
# A save that names its restore's key instead of writing it again:
# `key: ${{ steps.<id>.outputs.cache-primary-key }}`, as docs-site.yaml's save of go-docs does.
PRIMARY_KEY = re.compile(r"\$\{\{\s*steps\.([\w-]+)\.outputs\.cache-primary-key\s*\}\}")
# The workflow that saves go-docs on a push to main, through the docs-site.yaml build job it calls.
DOCS_PAGES = ".github/workflows/docs-pages.yaml"


@dataclass(frozen=True)
class FetchLine:
    """A go-fetch.sh line of a step: the step's directory, the line, its mode and, for a `file`
    line, the fetch file as a repository path. `fault` says why the line cannot be read, else ''."""
    workdir: str
    line: str
    mode: str
    file: str = ""
    fault: str = ""


@dataclass(frozen=True)
class Reader:
    """A restore of a cache family: the workflow and the job that hold it, and the go-fetch.sh
    lines of that job's steps after it."""
    path: str
    job: str
    lines: tuple

    @property
    def where(self) -> str:
        return f"job {self.job} of {self.path}"

    @property
    def files(self) -> list:
        """The fetch files of its readable `file` lines, in order."""
        return [line.file for line in self.lines if line.mode == FILE_MODE and not line.fault]


def action(step) -> str:
    """The action a step uses, without its @ref."""
    return str((step or {}).get("uses", "")).split("@")[0]


def holds_module_cache(step) -> bool:
    """Whether a cache step's path: names the Go module cache."""
    return GO_MODULE_CACHE in path_lines(((step or {}).get("with") or {}).get("path", ""))


def step_key(job: dict, step: dict) -> str:
    """A cache step's key; a save keyed by its restore's primary key reads as that restore's key."""
    key = str(((step or {}).get("with") or {}).get("key", "")).strip()
    named = PRIMARY_KEY.fullmatch(key)
    for other in (job.get("steps") or []) if named else []:
        if (other or {}).get("id") == named.group(1):
            return str(((other or {}).get("with") or {}).get("key", "")).strip()
    return key


def repository_path(*parts: str) -> str:
    """The parts joined and normalised as a repository-relative path; '' is the root."""
    path = posixpath.normpath(posixpath.join(*parts))
    return "" if path == "." else path


def fetch_line(workdir: str, line: str) -> FetchLine:
    """One go-fetch.sh line, read: the word after the script's is its mode, and a `file` line's
    one argument is its fetch file, resolved against the step's directory."""
    words = line.split()
    at = next(index for index, word in enumerate(words) if GO_FETCH in word)
    mode = words[at + 1] if at + 1 < len(words) else ""
    arguments = words[at + 2:]
    if mode in ARGUMENT_MODES or mode in RUNTIME_MODES:
        return FetchLine(workdir, line, mode)
    if mode != FILE_MODE:
        return FetchLine(workdir, line, mode, fault=f"its mode is {mode or '(none)'}, none of go-fetch.sh's "
                                                    f"{FILE_MODE}, {', '.join(ARGUMENT_MODES + RUNTIME_MODES)}")
    if len(arguments) != 1:
        return FetchLine(workdir, line, mode, fault=f"its {FILE_MODE} mode takes one fetch file, and the line gives "
                                                    f"{len(arguments)} arguments")
    named = arguments[0].strip("'\"")
    path = repository_path(workdir, named)
    if "$" in named or named.startswith("/") or path == ".." or path.startswith("../"):
        return FetchLine(workdir, line, mode, fault=f"its fetch file {named}, read from {workdir}, is no path "
                                                    f"of this repository")
    return FetchLine(workdir, line, mode, file=path)


def fetch_lines(document: dict, job: dict, steps) -> tuple:
    """Every go-fetch.sh line of `steps`, in order, each read with its step's directory."""
    return tuple(fetch_line(step_workdir(document, job, step or {}), line)
                 for step in steps
                 for line in script_lines(str((step or {}).get("run", "")))
                 if GO_FETCH in line)


def required_workflows(documents: dict, problems: list) -> dict:
    """`documents` and every workflow of this repository that a job of one of them calls,
    followed through each call to the files that file calls in turn: a reusable workflow may
    call another, and a restore and a fetch move with the steps that hold them. A call by
    ci.yaml itself of a file the repository does not have is workflow_documents' refusal; a call
    further down is refused here, since the restores it runs are then read by nothing."""
    found = dict(documents)
    pending = list(documents.items())
    workflows = directory_workflows()
    while pending:
        path, document = pending.pop()
        for called, job_id in called_workflows(document).items():
            if called in found:
                continue
            if called not in workflows:
                if path != WORKFLOW:
                    problems.append(error(path, f"job {job_id} calls {called}, which is not in the repository, so "
                                                f"the restores and fetches it runs are read by nothing here"))
                continue
            found[called] = workflows[called]
            pending.append((called, workflows[called]))
    return found


def go_cache_readers(documents: dict) -> dict:
    """Every restore of the given workflows, as family -> [Reader], each with the go-fetch.sh
    lines its job runs after that restore: a line before it runs before the entry is there, so
    it is no fetch the entry serves."""
    readers = defaultdict(list)
    for path, document in documents.items():
        for job_id, job in (document.get("jobs") or {}).items():
            job = job or {}
            steps = job.get("steps") or []
            for index, step in enumerate(steps):
                if action(step) in RESTORES:
                    readers[cache_family(step_key(job, step))].append(
                        Reader(path, job_id, fetch_lines(document, job, steps[index + 1:])))
    return readers


def go_fills(caches: dict) -> list:
    """Every save of a Go family in ci-caches.yaml, as (its job, the family, the go-fetch.sh
    lines of its job before it). An actions/cache/save saves what the steps before it left, and
    an actions/cache saves at the end of its job, after every step: so a job that saves two
    families fills the second with what the first one's fetch downloaded too."""
    fills = []
    for job_id, job in (caches.get("jobs") or {}).items():
        job = job or {}
        steps = job.get("steps") or []
        for index, step in enumerate(steps):
            if action(step) in SAVES and holds_module_cache(step):
                before = steps if action(step) == "actions/cache" else steps[:index]
                fills.append((job_id, cache_family(step_key(job, step)), fetch_lines(caches, job, before)))
    return fills


def listed(files) -> str:
    """A list of fetch files as a refusal shows it."""
    return ", ".join(files) or "(none)"


def reader_line_problems(reader: Reader, family: str, problems: list) -> None:
    """A reader's go-fetch.sh lines after its restore that no key can hold: a `deps` or `tools`
    line, whose arguments are in no hashed file, and a line this rule cannot read."""
    for line in reader.lines:
        if line.mode in ARGUMENT_MODES:
            problems.append(error(reader.path, f"{reader.where} runs `{line.line}` after restoring Go family "
                                               f"{family}, a go-fetch.sh {line.mode} line: its arguments are in no file "
                                               f"the family's key hashes, so a change to them changes no key and the entry "
                                               f"restored lacks what they name; keep the command in a fetch file under "
                                               f".github/go-fetch/ and run `{GO_FETCH} {FILE_MODE} <that file>`"))
        elif line.fault:
            problems.append(error(reader.path, f"{reader.where} runs `{line.line}` after restoring Go family "
                                               f"{family}, which this rule cannot read: {line.fault}"))


def check_go_fetch_fills(documents: dict, problems: list) -> list:
    """Each Go family ci-caches.yaml saves is filled with exactly the fetch files its readers run.
    The entry exists so that each reader's fetch downloads nothing: the fill runs, on a fresh
    runner, the fetch files its readers run, and saves the module cache they leave. A fill that
    drifts from its readers - a fetch file a reader now runs and the fill does not - saves an entry
    that lacks what that reader then downloads on every run, and nothing fails: the silence
    check_cache_round_trip is about, one level down, where the key and the paths match and the
    entry holds the wrong modules.

    So for every family whose save (actions/cache/save, or actions/cache) names GO_MODULE_CACHE -
    every such family, its builds' too - the `go-fetch.sh file` lines its fill job runs before the
    save must be the union of the fetch files of every job that restores the family, each file
    once, in ascending order of its repository path, so that two fills of one set read alike and
    a difference shows as one. Its readers are the jobs of ci.yaml and of every workflow it
    calls, followed through each call (`documents` is ci.yaml and the files it calls itself;
    required_workflows follows the rest), and a reader's fetch files are its `file` lines after
    its restore. A missing file, an extra one, a repeat and another order are each refused,
    naming the family, the fill job, the readers and both lists; so are a fill with no `file`
    line, a reader with none after its restore, a `deps` or `tools` line in either, and a family
    no reader restores. A family ci-caches.yaml does not save (go-docs, which docs-pages.yaml
    saves) has no fill here to hold.

    It returns its report line on a pass: how many families and readers it compared. A
    ci-caches.yaml that saves no Go family, or whose Go families no job restores, is refused as
    UNKNOWN rather than passed: a rule that compared nothing has not passed. A ci-caches.yaml that
    is not in the repository fills nothing, and so has nothing to hold."""
    if not (REPO / CACHE_WORKFLOW).is_file():
        return []
    start = len(problems)
    caches = load_repository_yaml(REPO / CACHE_WORKFLOW) or {}
    fills = go_fills(caches)
    if not fills:
        problems.append(error(CACHE_WORKFLOW, f"rule 22 UNKNOWN: no actions/cache/save or actions/cache step of this "
                                              f"workflow names {GO_MODULE_CACHE} in its path:, so this rule found no Go "
                                              f"family to hold a fill of and compared nothing; a ci-caches.yaml without one "
                                              f"is a module cache nothing fills, not a rule with nothing to do"))
        return []
    readers = go_cache_readers(required_workflows(documents, problems))
    families = list(dict.fromkeys(family for _, family, _ in fills))
    read = sum(len(readers.get(family, [])) for family in families)
    if not read:
        problems.append(error(CACHE_WORKFLOW, f"rule 22 UNKNOWN: no job of {WORKFLOW} or of a workflow it calls, "
                                              f"followed through every call, restores a Go family this workflow saves "
                                              f"({', '.join(families)}), so this rule read no reader's fetch files to hold "
                                              f"a fill to and compared nothing"))
    for job_id, family, filled in fills:
        check_fill(job_id, family, filled, readers.get(family, []), problems)
    if len(problems) > start:
        return []
    return [f"rule 22: {len(families)} Go families {posixpath.basename(CACHE_WORKFLOW)} saves, {read} readers, each "
            f"fill runs its readers' fetch files"]


def check_fill(job_id: str, family: str, filled: tuple, readers: list, problems: list) -> None:
    """One save of a Go family held to its readers, as check_go_fetch_fills says."""
    files = [line.file for line in filled if line.mode == FILE_MODE and not line.fault]
    for line in filled:
        if line.mode in ARGUMENT_MODES:
            problems.append(error(CACHE_WORKFLOW, f"job {job_id} fills Go family {family} with `{line.line}`, a "
                                                  f"go-fetch.sh {line.mode} line: its arguments are in no file the family's "
                                                  f"key hashes, so a change to them changes no key and the entry is found "
                                                  f"without the modules they name; run the readers' fetch files instead"))
        elif line.fault:
            problems.append(error(CACHE_WORKFLOW, f"job {job_id} fills Go family {family} with `{line.line}`, which "
                                                  f"this rule cannot read: {line.fault}"))
    if not readers:
        problems.append(error(CACHE_WORKFLOW, f"job {job_id} saves Go family {family}, which no job of {WORKFLOW} or "
                                              f"of a workflow it calls, followed through every call, restores, so no "
                                              f"reader's fetch holds what its fill runs: restore it there, or drop the save"))
        return
    wanted_by = defaultdict(list)
    for reader in readers:
        reader_line_problems(reader, family, problems)
        if not reader.files:
            problems.append(error(reader.path, f"{reader.where} restores Go family {family} and runs no "
                                               f"`{GO_FETCH} {FILE_MODE} <fetch file>` line after it, so the entry job "
                                               f"{job_id} of {CACHE_WORKFLOW} fills serves no fetch of that job: fetch "
                                               f"after the restore, or restore another family"))
        for file in reader.files:
            wanted_by[file].append(reader.where)
    wanted = sorted(wanted_by)
    names = "; ".join(dict.fromkeys(reader.where for reader in readers))
    if not files:
        problems.append(error(CACHE_WORKFLOW, f"job {job_id} saves Go family {family} and runs no "
                                              f"`{GO_FETCH} {FILE_MODE} <fetch file>` line before it, so it fills nothing "
                                              f"its readers fetch ({names} run {listed(wanted)})"))
        return
    if not wanted:
        return
    both = f"the fill runs {listed(files)}, and {names} run {listed(wanted)}"
    missing = [file for file in wanted if file not in files]
    extra = [file for file in dict.fromkeys(files) if file not in wanted_by]
    repeated = [file for file, count in Counter(files).items() if count > 1]
    for file in missing:
        problems.append(error(CACHE_WORKFLOW, f"job {job_id} fills Go family {family} without "
                                              f"`{GO_FETCH} {FILE_MODE} {file}`, which {' and '.join(wanted_by[file])} "
                                              f"run after restoring it ({both}): the entry then lacks what that fetch "
                                              f"downloads on every run, and nothing fails; run the file in the fill"))
    for file in extra:
        problems.append(error(CACHE_WORKFLOW, f"job {job_id} fills Go family {family} with "
                                              f"`{GO_FETCH} {FILE_MODE} {file}`, which no job that restores it runs "
                                              f"({both}): the entry then holds modules no reader fetches; drop the line, "
                                              f"or run the file in the reader that needs it"))
    for file in repeated:
        problems.append(error(CACHE_WORKFLOW, f"job {job_id} runs `{GO_FETCH} {FILE_MODE} {file}` "
                                              f"{Counter(files)[file]} times to fill Go family {family} ({both}): each "
                                              f"fetch file is run once"))
    if not (missing or extra or repeated) and files != wanted:
        problems.append(error(CACHE_WORKFLOW, f"job {job_id} fills Go family {family} with its readers' fetch files "
                                              f"in another order than the ascending one of their repository paths "
                                              f"({both}): the fill is right, and only a reader who sorts both lists can "
                                              f"see it; run them in that order, so the next difference shows as one"))


# ------------------------------------------------------------ the keys of the Go module caches
# A Go family's entry is right for a run only while it holds what that run's fetches download,
# and files decide that: go-fetch.sh itself, the fetch files the family's readers run, each tools
# pin file those name, the module files of each deps line's workspace, and those each tidy line's
# module reads. A key that hashes them all changes when any of them does, so a push to main that
# changes one fills a fresh entry. A key that leaves one out is found unchanged after that file
# moves, and every reader then downloads the difference on each run with nothing failing. A fetch
# file's line is one command (`#` starts a comment anywhere on it, and a blank line is none):
#   [GOOS=<v>] [GOARCH=<v>] [CGO_ENABLED=<0|1>] [GOWORK=off] deps <dir> [go list flags and packages...]
#   tools <file>
#   tidy <dir>
# with every path from the repository root, which FETCH_COMMAND spells. The hashFiles list is
# compared as the literal paths it names, as every key of this repository names them; a glob in
# it covers no file here, so such a key is refused, never passed.
HASH_FILES = re.compile(r"-\$\{\{\s*hashFiles\((?P<files>[^)]*)\)\s*\}\}$")
FETCH_COMMAND = re.compile(r"(?:GOOS=\S+\s+)?(?:GOARCH=\S+\s+)?(?:CGO_ENABLED=[01]\s+)?(?:GOWORK=off\s+)?deps\s+(?P<dir>[^\s-]\S*)(?:\s.*)?"
                           r"|tools\s+(?P<file>\S+)"
                           r"|tidy\s+(?P<module>[^\s-]\S*)")


@cache
def fetch_file_commands(path: str):
    if not (REPO / path).is_file():
        return None
    commands, faults = [], []
    for number, raw in enumerate(read_repository_text(REPO / path).split("\n"), 1):
        text = raw.split("#", 1)[0].strip()
        if not text:
            continue
        found = FETCH_COMMAND.fullmatch(text)
        named = repository_path(found["dir"] or found["file"] or found["module"]) if found else ""
        if not found or named == ".." or named.startswith("../") or named.startswith("/"):
            faults.append((number, raw.strip()))
        else:
            commands.append((number, "deps" if found["dir"] else "tidy" if found["module"] else "tools", named))
    return commands, faults


def work_uses(text: str) -> list:
    """The directories a go.work's use directives name, in the order they name them."""
    uses, block = [], False
    for raw in text.split("\n"):
        line = raw.split("//", 1)[0].strip()
        if block:
            if line == ")":
                block = False
            elif line:
                uses.append(line.strip('"`'))
        elif re.match(r"use\b", line):
            rest = line[3:].strip()
            if rest == "(":
                block = True
            elif rest.startswith("(") and rest.endswith(")"):
                uses += [word.strip('"`') for word in rest[1:-1].split()]
            elif rest:
                uses.append(rest.strip('"`'))
    return uses


def module_files(directory: str) -> list:
    """The module files a go command run in `directory` reads to choose its modules: the nearest
    go.work at or above it up to the root, with the go.work.sum beside it and the go.mod and
    go.sum of each directory its use directives name (resolved against its own directory); with
    no go.work, the nearest go.mod at or above it and the go.sum beside it; [] with neither."""
    chain = [directory]
    while chain[-1]:
        chain.append(posixpath.dirname(chain[-1]))
    for here in chain:
        if (REPO / here / "go.work").is_file():
            files = [repository_path(here, "go.work"), repository_path(here, "go.work.sum")]
            for use in work_uses(read_repository_text(REPO / here / "go.work")):
                module = repository_path(here, use)
                files += [repository_path(module, "go.mod"), repository_path(module, "go.sum")]
            return files
    for here in chain:
        if (REPO / here / "go.mod").is_file():
            return [repository_path(here, "go.mod"), repository_path(here, "go.sum")]
    return []


def relative_replace_dirs(text: str) -> list:
    dirs, block = [], False
    for raw in text.split("\n"):
        line = raw.split("//", 1)[0].strip()
        if block:
            if line == ")":
                block = False
                continue
            entry = line
        elif re.match(r"replace\b", line):
            entry = line[len("replace"):].strip()
            if entry == "(":
                block = True
                continue
        else:
            continue
        target = entry.split("=>", 1)[1].split() if "=>" in entry else []
        if len(target) == 1 and re.match(r"\.\.?(/|$)", target[0].strip('"`')):
            dirs.append(target[0].strip('"`'))
    return dirs


def tidy_files(directory: str) -> list:
    """The module files `go mod tidy` in `directory` reads to choose its modules: the go.mod there
    and the go.sum beside it, and the go.mod of each directory a replace directive of that go.mod
    names (resolved against its own directory). Tidy reads no go.work, and go applies the main
    module's replace directives alone. [] when the directory holds no go.mod, which go-fetch.sh
    refuses too."""
    if not (REPO / directory / "go.mod").is_file():
        return []
    files = [repository_path(directory, "go.mod"), repository_path(directory, "go.sum")]
    for target in relative_replace_dirs(read_repository_text(REPO / directory / "go.mod")):
        files.append(repository_path(directory, target, "go.mod"))
    return files


def family_files(family: str, readers: list, filled: bool, problems: list) -> set:
    files = {GO_FETCH}
    for reader in readers:
        if not filled:
            reader_line_problems(reader, family, problems)
        for fetch in reader.files:
            files.add(fetch)
            parsed = fetch_file_commands(fetch)
            if parsed is None:
                problems.append(error(reader.path, f"{reader.where} runs `{GO_FETCH} {FILE_MODE} {fetch}` after "
                                                   f"restoring Go family {family}, and the repository has no {fetch}: "
                                                   f"go-fetch.sh fails on it, and this rule cannot read which files the "
                                                   f"family's keys must hash"))
                continue
            commands, faults = parsed
            for number, text in faults:
                problems.append(error(fetch, f"line {number} of {fetch} is `{text}`, which is neither "
                                             f"`[GOOS=<v>] [GOARCH=<v>] [CGO_ENABLED=<0|1>] [GOWORK=off] deps <dir> ...`, "
                                             f"`tools <file>` nor `tidy <dir>` with its path from the repository "
                                             f"root: go-fetch.sh's "
                                             f"{FILE_MODE} mode cannot run it, and this rule cannot read which files the "
                                             f"keys of the families that fetch it must hash"))
            for number, kind, named in commands:
                if kind == "tools":
                    files.add(named)
                    continue
                modules = module_files(named) if kind == "deps" else tidy_files(named)
                if not modules and kind == "deps":
                    problems.append(error(fetch, f"line {number} of {fetch} runs deps in {named}, with no go.work or "
                                                 f"go.mod at or above it up to the repository root, so this rule cannot "
                                                 f"name the module files the keys of the families that fetch it must hash"))
                elif not modules:
                    problems.append(error(fetch, f"line {number} of {fetch} runs tidy in {named}, which holds no go.mod, "
                                                 f"so this rule cannot name the module files the keys of the families "
                                                 f"that fetch it must hash"))
                files.update(modules)
    return files


def check_go_cache_keys(documents: dict, problems: list) -> list:
    """Every restore and save of a Go family (a cache step whose path: names GO_MODULE_CACHE) in
    ci.yaml, in every workflow it calls (followed through each call), in ci-caches.yaml and in
    docs-pages.yaml (with the docs-site.yaml build job it calls, which restores and saves
    go-docs) has a key that ends `-${{ hashFiles(<files>) }}`, and its list names every file
    family_files gives that family: go-fetch.sh, each fetch file the family's readers run (its
    readers read as check_go_fetch_fills reads them, over those workflows but ci-caches.yaml,
    whose restores are lookups), each tools file those name, the module files of each deps line's
    workspace and those each tidy line's module reads (tidy_files). A key with no hashFiles, or a
    list missing a file, is refused naming the workflow, the job, the step, the family and each
    missing file; a file the list names beyond those is allowed. A save keyed
    `${{ steps.<id>.outputs.cache-primary-key }}` is held as the
    key of that restore. It returns its report line on a pass that read at least one key."""
    start = len(problems)
    workflows = directory_workflows()
    reading = dict(documents)
    if DOCS_PAGES in workflows:
        reading[DOCS_PAGES] = workflows[DOCS_PAGES]
    reading = required_workflows(reading, problems)
    readers = go_cache_readers(reading)
    held = dict(reading)
    if CACHE_WORKFLOW in workflows:
        held[CACHE_WORKFLOW] = workflows[CACHE_WORKFLOW]
    filled = {family for _, family, _ in go_fills(workflows.get(CACHE_WORKFLOW) or {})}
    wanted: dict = {}
    keys = 0
    for path, document in held.items():
        for job_id, job in (document.get("jobs") or {}).items():
            job = job or {}
            for step in job.get("steps") or []:
                if action(step) not in RESTORES + SAVES or not holds_module_cache(step):
                    continue
                keys += 1
                key = step_key(job, step)
                family = cache_family(key)
                if family not in wanted:
                    wanted[family] = family_files(family, readers.get(family, []), family in filled, problems)
                where = f'job {job_id} of {path} step "{step_label(step)}"'
                found = HASH_FILES.search(key)
                if found is None:
                    problems.append(error(path, f"{where} keys Go family {family} with `{key}`, which does not end "
                                                f"`-${{{{ hashFiles(<files>) }}}}`: the entry is right only while it "
                                                f"holds what its readers' fetches download, and a key that hashes none of "
                                                f"the files deciding that is found unchanged after they move; hash "
                                                f"{listed(sorted(wanted[family]))}"))
                    continue
                hashed = {repository_path(name.strip()) for name in re.findall(r"'([^']*)'", found["files"])}
                missing = sorted(wanted[family] - hashed)
                if missing:
                    problems.append(error(path, f"{where} keys Go family {family} with `{key}`, whose hashFiles list "
                                                f"lacks {listed(missing)}: a key that does not hash a file deciding what "
                                                f"the entry must hold is found unchanged after that file moves, so every "
                                                f"reader downloads the difference on each run and nothing fails; add "
                                                f"them to the list"))
    if len(problems) > start or not keys:
        return []
    return [f"rule 23: {keys} Go module cache keys, each hashing go-fetch.sh, its family's fetch files and the "
            f"files they name"]
