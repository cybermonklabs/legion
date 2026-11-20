"""The rules about ci.yaml that stand on their own: what no `if:` of it may say, what every job's
`if:` must say, what ci-caches.yaml must save
for the entries ci.yaml restores and which packages the daemon fill job's race build must compile, which workspaces
ci_job_reads.py's WORKSPACES must name, and
that no other workflow of this repository runs on a merge group.
None of them
reads a filter, a gate or the closure of a go command, so none belongs in the filter-coverage
engine beside them; check-ci-filters.sh's header numbers them 10, 12, 11, 13, 14 and 16, and
ci_filter_coverage.main()
is what runs them, over the documents it already loaded where the rule reads only those - the
rerun-guard rule over ci.yaml alone, since the `if:` it reads is the caller job's; the cache
rule over ci.yaml and every workflow its jobs call, since a job's restore moves with its steps,
and over every other workflow of the directory, which it reads itself, for the key and paths,
since a release build's restore misses as silently; the push-condition rule over
ci.yaml and the called files that are ci.yaml's alone (ci_only_documents says why not the
others, and reads the directory to find them) - the workspace rule over the
root manifest and the WORKSPACES table main() hands it, and the race-fill rule over ci-caches.yaml
and the DAEMON_RACE_PACKAGES list and DAEMON_RACE_FILL_JOB job main() hands it the same way, since what this module imports is
the engine's names, never the data's, and the merge-group rule over the workflow directory, which
it reads itself.

It sits beside that engine (and workflow_paths.py, the path model) in the repository it checks,
and imports the engine's names for the two workflow files, the repository root, the checks
main's ruleset requires, the reader of a job's `uses:` and the three
helpers that read an `if:`, and the path model's reader of a workflow's `on:`. main()
imports this module rather than the other way round, so the one import of the engine here is of
a module Python has finished loading by then. Rule 22 reads a go command's words with
private_proc.py's read_go_args, the wrapper's own reader of them, and the words bash reads before a
command with shell_commands.py's SKIP_WORDS, both beside it.
"""

import json
import posixpath
import re
import shlex
from collections import defaultdict
from functools import cache


from ci_filter_coverage import (
    CACHE_WORKFLOW,
    REPO,
    REQUIRED_CHECKS,
    WORKFLOW,
    RepositoryInputError,
    called_workflows,
    conditions,
    error,
    load_repository_yaml,
    read_repository_text,
    script_lines,
    squash,
    step_workdir,
    where_if,
)
from private_proc import FALSE, VALUE_FLAGS, read_go_args
from shell_commands import SKIP_WORDS
from workflow_paths import events


# ---------------------------------------------------------------- the conditions ci.yaml cannot hold
# ci.yaml runs on a pull request, a merge group and a workflow_dispatch, and on no push, so an if:
# testing for a push event, or comparing github.ref with a branch ref, holds its job or step back
# on every run there is. Either way round, as a GitHub expression may be written; its strings are
# single-quoted, a double-quoted one being no expression at all. github.ref_name, and github.ref
# in anything but such a comparison - startsWith against refs/heads/gh-readonly-queue/, the branch
# a merge_group run does carry, among them - are left alone.
def comparison(left: str, right: str) -> str:
    """The regex of a GitHub expression comparing `left` with `right`, either operand first."""
    return rf"{left}\s*[=!]=\s*{right}|{right}\s*[=!]=\s*{left}"


PUSH_CONDITION = re.compile("|".join((comparison(r"github\.event_name", "'push'"),
                                      comparison(r"github\.ref", r"'refs/heads/[^']*'"))))
# Where the work such a condition used to gate lives now.
PUSH_INSTEAD = (f"a cache that must be filled on a push to main belongs in {CACHE_WORKFLOW}, and a job that must "
                f"run at main's tip is started by the workflow_dispatch "
                f".github/workflows/main-bypass-check.yaml sends")
# The one `on:` a workflow this rule reads beyond ci.yaml may declare, and the directory and the
# two suffixes GitHub reads it in, which this rule and the merge-group rule at the foot of this
# module both walk.
WORKFLOW_CALL = "workflow_call"
WORKFLOW_DIR = ".github/workflows"
WORKFLOW_SUFFIXES = (".yaml", ".yml")


@cache
def directory_workflows() -> dict:
    """Every workflow of this repository, as {its repository-relative path: the parsed document},
    in path order. The three rules that walk the directory - the push-condition rule (through
    ci_only_documents), the cache rule and the merge-group rule - read this one answer: parsing
    each file for each of them is a second read of every workflow in the repository."""
    try:
        paths = sorted(p for p in (REPO / WORKFLOW_DIR).iterdir() if p.suffix in WORKFLOW_SUFFIXES)
    except Exception as exc:
        raise RepositoryInputError(WORKFLOW_DIR, exc) from exc
    return {f"{WORKFLOW_DIR}/{path.name}": load_repository_yaml(path) or {} for path in paths}


def ci_only_documents(documents: dict) -> dict:
    """Of the document set, the files whose every run is a run of ci.yaml: ci.yaml itself, and
    each workflow it calls that no other workflow of this repository calls and whose `on:`
    declares workflow_call and nothing else.

    The rule below is about conditions that are dead because the workflow they are in has no
    push trigger, and that reason carries to a called file only when ci.yaml is the only way
    into it. A reusable workflow runs inside its caller's run and under its caller's event, so a
    file two workflows call is live under each caller's events, and a file with a trigger of its
    own has runs no caller starts at all. Both cases are in this repository today:
    .github/workflows/docs-site.yaml is called by docs-pages.yaml as well, which runs on a push
    to main, and docs-site.yaml's cache save step carries `github.event_name == 'push'` for that
    caller; .github/workflows/worker-image.yaml is called by release.yaml twice. The
    .github/workflows/ci-job-*.yaml files, which hold the steps of one ci.yaml job each, are the
    other case: nothing else calls them and they run on workflow_call alone, so a push condition
    in one of them is as dead as it would be in ci.yaml."""
    called_elsewhere: set = set()
    for path, document in directory_workflows().items():
        if path != WORKFLOW:
            called_elsewhere |= set(called_workflows(document))
    return {path: document for path, document in documents.items()
            if path == WORKFLOW
            or (path not in called_elsewhere and set(events(document)) == {WORKFLOW_CALL})}


def check_push_conditions(documents: dict, problems: list) -> None:
    """A job or a step of ci.yaml, or of a workflow only ci.yaml calls, gated on a push event,
    or on github.ref being a branch ref, is
    dead: the workflow has no push trigger, and on its three events github.ref is a pull request's
    merge ref or the merge queue's own branch, never the ref such a condition names. Dead code
    that reads as live is worse than none - a step held back by `if: github.event_name ==
    'push'`, or a job by `if: github.ref == 'refs/heads/main'`, runs in no run of this workflow
    while its text says it runs on main - so a later pull
    request may not bring one back. Read off the if: text, one comparison at a time: a committer
    who writes the same test as a function call is the review's to catch. Which files it reads,
    and why it is not every file ci.yaml calls, is ci_only_documents above; each refusal names
    the file the condition is in."""
    for path, document in ci_only_documents(documents).items():
        for job_id, job in (document.get("jobs") or {}).items():
            job = job or {}
            refs = conditions(job)
            for step, condition in refs:
                for found in dict.fromkeys(m.group(0) for m in PUSH_CONDITION.finditer(condition)):
                    where = where_if(job_id, step)
                    runs = (f"{WORKFLOW} runs" if path == WORKFLOW
                            else f"{path} runs only as part of {WORKFLOW}, which runs")
                    problems.append(error(path, f"{where} has `if: {squash(condition)}`, which tests "
                                                f"`{found}`: {runs} on a pull request, a merge group and a "
                                                f"workflow_dispatch, never on a push to main, so nothing that condition "
                                                f"gates ever runs; {PUSH_INSTEAD}"))


# ------------------------------------------------------- the re-run of a merge-group run
# A group is tested once, so every job of a re-run attempt of a merge-group run must skip. The
# term that does it is a job's own: `gh run rerun --failed` leaves out a `changes` that
# succeeded and hands the attempt that job's outputs again, so an `if:` testing only
# `needs.changes.outputs.<x>` runs the job in full on an attempt the group has no use for, and
# its report step posts a ci-ok on the dead group's head. A job that runs on merge_group alone
# (`cancel-started`) needs only the attempt half, so each job is asked for one term and never
# for both.
#
# A job named for a required check (REQUIRED_CHECKS) is asked for neither, and refused one,
# because a skip is no safe outcome for it: a job an `if:` holds back is skipped, and a skipped
# job's check run is a passing required check, so the term that saves every other job from a dead
# group would hand that group the check it merges on, concluded by nothing. Each of those jobs
# runs on every attempt and refuses an attempt above the first in a step, which fails and makes
# the check a failure; .github/ci-notes/ci.md#why-pr-title-refuses-a-re-run-attempt-in-a-step-of-its-own
# and .github/ci-notes/ci.md#why-a-re-run-attempt-of-a-merge-group-run-is-refused-in-a-step say so.
RERUN_GUARD = ("(github.event_name != 'merge_group' || github.run_attempt == '1')",
               re.compile(r"\(\s*github\.event_name\s*!=\s*'merge_group'\s*\|\|"
                          r"\s*github\.run_attempt\s*==\s*'1'\s*\)"))
MERGE_GROUP_ONLY = re.compile(r"github\.event_name\s*==\s*'merge_group'")
ATTEMPT_ONE = re.compile(r"github\.run_attempt\s*==\s*'1'")


def check_rerun_guard(document: dict, problems: list) -> None:
    """Every job of ci.yaml but the REQUIRED_CHECKS ones must hold, in its own `if:`, the term
    that skips it on a re-run attempt of a merge-group run: the disjunction RERUN_GUARD names,
    or, for a job whose `if:` admits merge_group alone, `github.run_attempt == '1'` by itself. A
    job named for a required check must hold neither, for the reason above: its skip is a passing
    required check. Read off the if: text, as check_push_conditions is: the same test written
    another way is the review's to catch.

    It reads ci.yaml alone, and no workflow ci.yaml calls: the `if:` this rule is about is the
    caller job's, which stays in ci.yaml whatever file holds that job's steps, and a called
    file's own jobs run or skip with the caller that started them."""
    wanted, found = RERUN_GUARD
    for job_id, job in (document.get("jobs") or {}).items():
        condition = str((job or {}).get("if", ""))
        if job_id in REQUIRED_CHECKS:
            if found.search(condition) or ATTEMPT_ONE.search(condition):
                problems.append(error(WORKFLOW, f"job {job_id} is named for a check main's ruleset "
                                                f"requires and its if: holds a term that skips it on a re-run attempt of a "
                                                f"merge-group run: a skipped job's check run is a passing required check, so that "
                                                f"attempt would merge on {job_id} concluded by nothing; drop the term and refuse "
                                                f"the attempt in a step of the job instead"))
            continue
        if found.search(condition):
            continue
        if MERGE_GROUP_ONLY.search(condition):
            if not ATTEMPT_ONE.search(condition):
                problems.append(error(WORKFLOW, f"job {job_id} runs on merge_group and its if: has no "
                                                f"`github.run_attempt == '1'`, so a re-run attempt of a merge-group run runs it "
                                                f"again on a group that is tested already"))
            continue
        problems.append(error(WORKFLOW, f"job {job_id}'s if: has no `{wanted}`, so a re-run attempt of a "
                                        f"merge-group run runs it again on a group that is tested already; `gh run rerun --failed` "
                                        f"does not re-run the `changes` that succeeded, so no job may lean on that job's skip for "
                                        f"this"))


# ---------------------------------------------------------------- the cache round trip
def cache_family(key: str) -> str:
    """The family a cache key names: its text up to the first expression - the Go version the
    entry is keyed on - without the separator that joins the two."""
    return key.split("${{")[0].strip().rstrip("-")


def path_lines(value) -> list:
    """A cache step's path:, as the ordered list of directories it names."""
    return [line.strip() for line in str(value).splitlines() if line.strip()]


def job_cache_steps(job: dict, action: str) -> list:
    """Every actions/cache/<action> step of one job, as (its family, its with:)."""
    return [(cache_family(str((step.get("with") or {}).get("key", ""))), step.get("with") or {})
            for step in ((job or {}).get("steps") or [])
            if str((step or {}).get("uses", "")).split("@")[0] == f"actions/cache/{action}"]


def cache_steps(document: dict, action: str) -> list:
    """Every actions/cache/<action> step of a workflow, as (its job, its family, its with:)."""
    return [(job_id, family, given)
            for job_id, job in (document.get("jobs") or {}).items()
            for family, given in job_cache_steps(job or {}, action)]


def restores_of(documents: dict) -> dict:
    """Every restore of the given workflows, as family -> [(where it is, its with:)]. Given the
    document set the engine loads - ci.yaml, and each local workflow one of its jobs calls:
    dispatch-browser.yaml's binaries, multiprocess, embedder and visual jobs restore go-envoy and
    go-envoy-test for ci.yaml's dispatch-browser job - it is every restore of ci.yaml's run;
    paired against ci.yaml alone, a save that only a called workflow restores would read as a fill
    nobody wants. Given the rest of the directory, it is the restores of the release builds and
    the other workflows. The set holds each file once however many of ci.yaml's jobs name it, so
    each restore is taken once."""
    restores = defaultdict(list)
    for path, document in documents.items():
        for job_id, family, given in cache_steps(document, "restore"):
            restores[family].append((f"job {job_id} of {path}", given))
    return restores


def check_cache_round_trip(documents: dict, problems: list) -> None:
    """ci-caches.yaml fills on a push to main the entries ci.yaml's run restores - its own jobs'
    restores, and those of the local workflows they call - so each family's save must carry
    exactly the key and the paths its restores name: a key or a path that drifts apart makes
    every restore miss, and nothing fails - each job just rebuilds from nothing what the fill
    already built. A family is the key's text up to its first expression, and it is what
    pairs a save with the restores to hold it to; the keys are compared as text and the paths as
    the ordered lists of lines they name. A family ci.yaml restores that this workflow does not
    save is no fault: docs-pages.yaml saves go-docs. The other way round is one, a family nothing
    restores being a fill nobody reads. Each restore here is a lookup, which must be
    `lookup-only: true` with no restore-keys: the question it asks is whether that exact key is
    already there, the entry it would download is read by nothing in this workflow, and a prefix
    hit would answer for an older key and skip the build that fills this one. A ci-caches.yaml
    that is not in the repository saves nothing, and so has nothing to hold.

    The restores are collected from every document of the set, ci.yaml and each workflow its
    jobs call, since a job that gives its steps to a called file takes its restore there with
    them and the entry it reads is the same one; each refusal names the file the restore is in.

    The key and the paths are held for every other workflow of the directory too: a release
    build (release.yaml's go-daemon, release-envoy-infra.yaml's go-envoy-infra), the nightly
    browser re-record (go-envoy) and this workflow's own lookups restore the entries it fills,
    and a restore there that drifts from the save misses as silently as one of ci.yaml's. Only
    the existence check above stays ci.yaml's: a fill is for ci.yaml's run, so a family only
    another workflow restores is still a fill ci.yaml does not read."""
    if not (REPO / CACHE_WORKFLOW).is_file():
        return
    caches = load_repository_yaml(REPO / CACHE_WORKFLOW) or {}
    restores = restores_of(documents)
    elsewhere = restores_of({path: document for path, document in directory_workflows().items()
                             if path not in documents})
    for job_id, family, given in cache_steps(caches, "save"):
        key, paths = str(given.get("key", "")).rstrip(), path_lines(given.get("path", ""))
        if family not in restores:
            problems.append(error(CACHE_WORKFLOW, f"job {job_id} saves cache family {family}, which no "
                                                  f"actions/cache/restore of {WORKFLOW} or of a workflow it calls "
                                                  f"reads; this workflow exists to fill what {WORKFLOW} restores, so a "
                                                  f"family nothing restores is a fill nobody reads: restore it there, "
                                                  f"or drop the save"))
            continue
        for restore_where, restored in restores[family] + elsewhere.get(family, []):
            restored_key = str(restored.get("key", "")).rstrip()
            restored_paths = path_lines(restored.get("path", ""))
            if key != restored_key:
                problems.append(error(CACHE_WORKFLOW, f"job {job_id} saves family {family} under key "
                                                      f"`{key}`, but {restore_where} restores it under "
                                                      f"`{restored_key}`: a restore whose key is not the save's "
                                                      f"misses every time, and nothing fails - the job rebuilds from nothing instead"))
            if paths != restored_paths:
                problems.append(error(CACHE_WORKFLOW, f"job {job_id} saves family {family} from "
                                                      f"{', '.join(paths) or '(no path)'}, but {restore_where} restores "
                                                      f"it into {', '.join(restored_paths) or '(no path)'}: the "
                                                      f"entry then holds other directories than the job reads, and the job rebuilds what "
                                                      f"the fill already built"))
    for job_id, family, given in cache_steps(caches, "restore"):
        if str(given.get("lookup-only", "")).strip().lower() != "true":
            problems.append(error(CACHE_WORKFLOW, f"job {job_id}'s actions/cache/restore of family "
                                                  f"{family} is not `lookup-only: true`: it asks only whether that exact key is already "
                                                  f"there, and the entry a restore downloads is read by no step of this workflow"))
        if given.get("restore-keys") is not None:
            problems.append(error(CACHE_WORKFLOW, f"job {job_id}'s actions/cache/restore of family "
                                                  f"{family} has restore-keys: a prefix hit would answer for an older key, and the build "
                                                  f"that fills this one would be skipped, leaving the key empty"))


# ------------------------------------------- the race build that fills the race lane's cache
# What makes a line of ci-caches.yaml the race build this rule holds: a `go test` running with
# -race, in the job that fills the daemon's family (RACE_JOB, the job id ci_job_reads.py's
# DAEMON_RACE_FILL_JOB names). Spelled as the engine's tag-run triggers are, so `-racy` is not the
# flag and a go vet or go build line is no such command, and read through script_lines, so a
# comment is no trigger. The packages are the words beginning `./`, which is how a go command
# names one and how ci_job_reads.py's DAEMON_RACE_PKGS spells that same list; the `-o /dev/null`
# of the build is no package. TABLE, the file the list is in, is the workspaces section's constant
# below: both rules name that one file, so there is one spelling of it here. A race build of
# another job fills another family's cache, for another lane, and is not this rule's: the
# envoy-infra job's fills envoy-infra-go's unit step.
RACE_TEST = re.compile(r"\bgo\s+(?:-C\s+\S+\s+)?test\b")
RACE_FLAG = re.compile(r"(?<![\w-])-race\b")
RACE_LIST = "DAEMON_RACE_PACKAGES"
RACE_JOB = "DAEMON_RACE_FILL_JOB"


def race_builds(document: dict, only_job: str) -> list:
    """Every race build of a workflow's job only_job: (the job, its line, the packages that line
    names)."""
    return [(job_id, line, [word for word in line.split() if word.startswith("./")])
            for job_id, job in (document.get("jobs") or {}).items() if job_id == only_job
            for step in ((job or {}).get("steps") or [])
            for line in script_lines(str((step or {}).get("run", "")))
            if RACE_TEST.search(line) and RACE_FLAG.search(line)]


def check_race_fill_packages(packages: list, job: str, problems: list) -> None:
    """The race build of ci-caches.yaml's job `job`, the one that fills the daemon's family,
    compiles exactly the packages the daemon's race lane runs. The workflow fills on a push to main
    the build cache ci.yaml's race step restores, so the two sides must name one set: a package the
    fill compiles and the lane does not leaves the entry holding a race binary no job ever
    restores, and a package the lane runs that the fill leaves out is linked from nothing in every
    run of that step. Neither fails anything - it is the silence check_cache_round_trip is about
    one level up, where the entry is found and holds the wrong objects - and the command has
    drifted twice for want of this rule: the fill kept ./internal/record/ after #2867 took it out
    of the lane. The packages are held as a set and in order, each spelled as a command spells it
    (./internal/daemon/), since a command that reads in another order than the list is one a
    reader must sort before comparing, and an unreadable pair is where the next drift hides. A job
    with no race build at all fails too, rather than passing quietly: this rule exists to hold that
    one command, so a tree without it is a lane nothing fills, not a rule with nothing to do; and
    so does a workflow with no such job. A race build of any other job fills another family, and
    this rule does not read it. A ci-caches.yaml that is not in the repository fills nothing, and
    so has nothing to hold.

    The list and the job are arguments, as check_workspace_table's table is: main() holds
    ci_job_reads.py already, and nothing here imports it - the engine's names are all this module
    takes."""
    if not (REPO / CACHE_WORKFLOW).is_file():
        return
    caches = load_repository_yaml(REPO / CACHE_WORKFLOW) or {}
    wanted = [f"./{package}/" for package in packages]
    if job not in (caches.get("jobs") or {}):
        problems.append(error(CACHE_WORKFLOW, f"this workflow has no job {job}, which {TABLE}'s {RACE_JOB} names "
                                              f"as the one that fills the cache {WORKFLOW}'s race step restores, so nothing "
                                              f"fills it and that step links {', '.join(wanted) or '(no package)'} from nothing "
                                              f"on every run; name the fill job there, or drop this rule with the lane it holds"))
        return
    builds = race_builds(caches, job)
    if not builds:
        problems.append(error(CACHE_WORKFLOW, f"no step of job {job} runs a `go test` with -race, so "
                                              f"nothing fills the build cache {WORKFLOW}'s race step restores and that step links "
                                              f"{', '.join(wanted) or '(no package)'} from nothing on every run; put the race build "
                                              f"back, or drop this rule with the lane it holds"))
        return
    for job_id, line, named in builds:
        where = f"job {job_id}'s race build"
        extra = [package for package in named if package not in wanted]
        missing = [package for package in wanted if package not in named]
        for package in extra:
            problems.append(error(CACHE_WORKFLOW, f"{where} names {package}, which {TABLE}'s {RACE_LIST} "
                                                  f"does not ({', '.join(named)} against {', '.join(wanted) or '(no package)'}): this "
                                                  f"build fills the cache {WORKFLOW}'s race step restores, so it compiles a race binary "
                                                  f"no job of {WORKFLOW} restores, and nothing fails; either the command follows the "
                                                  f"list, or the package joins {RACE_LIST}, the one place the lane is named - "
                                                  f"{WORKFLOW}'s two race steps follow it already"))
        for package in missing:
            problems.append(error(CACHE_WORKFLOW, f"{TABLE}'s {RACE_LIST} names {package}, which {where} "
                                                  f"does not ({', '.join(wanted)} against {', '.join(named) or '(no package)'}): the "
                                                  f"fill then misses a race binary {WORKFLOW}'s race step links from nothing on every "
                                                  f"run, and nothing fails; either the command follows the list, or the package leaves "
                                                  f"{RACE_LIST}, the one place the lane is named - {WORKFLOW}'s two race steps follow "
                                                  f"it already"))
        if named != wanted and not extra and not missing:
            problems.append(error(CACHE_WORKFLOW, f"{where} names {', '.join(named)}, where {TABLE}'s "
                                                  f"{RACE_LIST} has {', '.join(wanted)}: the same packages in another order, so the "
                                                  f"fill is right and only a reader who sorts both lists can see that it is; spell the "
                                                  f"command as the list reads, so the next change to either shows as a difference"))


def check_race_fill_scripts(fills: list, problems: list) -> None:
    """Each (fill job, script, its workdir, its compile call) of `fills` is a ci-caches.yaml job
    that fills a family whose race step runs a script: the job's steps in that workdir run the
    script's compile call, exactly as written, and no `go test` with -race of their own. The
    script holds the package list and -race the race step runs, so the fill and the step cannot
    name other packages or flags only while the fill reaches them through that script; a race
    build the fill wrote itself is the copy this rule exists to stop, and nothing fails when it
    drifts. A line that names the script with any other arguments fails too, naming the line, as
    does a fill job that is gone or never calls the script.

    The table is an argument, as check_race_fill_packages' list is: main() holds ci_job_reads.py,
    and nothing here imports it."""
    if not (REPO / CACHE_WORKFLOW).is_file():
        return
    jobs = (load_repository_yaml(REPO / CACHE_WORKFLOW) or {}).get("jobs") or {}
    for job_id, script, workdir, call in fills:
        name = script.rsplit("/", 1)[-1]
        if job_id not in jobs:
            problems.append(error(CACHE_WORKFLOW, f"this workflow has no job {job_id}, which {TABLE} names as the one "
                                                  f"that fills the race objects {script} compiles; name the fill job there, "
                                                  f"or drop the entry with the step it serves"))
            continue
        calls = 0
        for step in (jobs[job_id] or {}).get("steps") or []:
            if (step or {}).get("working-directory", "") != workdir:
                continue
            for line in script_lines(str((step or {}).get("run", ""))):
                if name in line:
                    if line == call:
                        calls += 1
                    else:
                        problems.append(error(CACHE_WORKFLOW, f"job {job_id} runs `{line}`, where the fill runs "
                                                              f"`{call}` and nothing else of {script}: the step that "
                                                              f"restores this cache runs the script's other mode, so any "
                                                              f"other call compiles objects the step does not link"))
                elif RACE_TEST.search(line) and RACE_FLAG.search(line):
                    problems.append(error(CACHE_WORKFLOW, f"job {job_id} runs `{line}`, a race build of its own: "
                                                          f"its race objects come from `{call}`, which takes the "
                                                          f"packages and flags from {script}, so a copy of them here "
                                                          f"can drift from the step and nothing fails"))
        if calls != 1:
            problems.append(error(CACHE_WORKFLOW, f"job {job_id} runs `{call}` {calls} times in {workdir}, where it "
                                                  f"fills the race objects {script} compiles once: with none the step "
                                                  f"links them from nothing on every run"))


# ---------------------------------------------------------------- the workspaces and the table
# The root manifest, whose workspaces ci_job_reads.py's WORKSPACES must name one for one, and the
# file that table is in. Both are named in the errors below, each in the one it is about.
MANIFEST = "package.json"
TABLE = ".github/scripts/ci_job_reads.py"


def check_workspace_table(workspaces: list, problems: list) -> None:
    """The root manifest's workspaces and ci_job_reads.py's WORKSPACES name the same packages.
    A frozen `bun install` reads every workspace manifest, so bun_job() declares each of
    WORKSPACES as a read of every Bun job, and the engine holds every filter gating such a job to
    all of them. A workspace the table leaves out is therefore a manifest no filter has to select,
    and a change to that package starts no job at all - which is how packages/broker-client came
    to have its typecheck and its tests run nowhere. The other way round is a read every Bun job
    declares of a manifest no install reads. The table is an argument because main() holds it
    already, and because nothing here imports ci_job_reads: the engine's names are all this
    module takes.

    The manifest is read as .github/scripts/check-bun-workspace-manifests.sh:101-109 reads it,
    at every shape, since the two checks are about the same list and a list one of them cannot
    read is not a list the other may pass over: a root manifest that is missing or is not valid
    JSON, and a `workspaces` that is not a list or holds anything but non-empty strings, are
    named rather than taken for an empty list, which would hold the table to nothing - and would
    do it exactly where every read WORKSPACES feeds is wrong anyway, since a tree with no root
    manifest runs no `bun install` the table could describe. An empty array is accepted, as that
    script accepts it: a root that declares no workspace is consistent with a table that names
    none, and a table that names one is caught below as an entry the manifest does not list. A
    trailing slash is stripped on both sides, since `packages/x/` and `packages/x` are one
    workspace to bun, and a rule that stripped it on one side alone would report the table as
    missing a workspace it names in the other spelling."""
    if not (REPO / MANIFEST).is_file():
        problems.append(error(MANIFEST, f"the root manifest is missing, so {TABLE}'s WORKSPACES is held to "
                                        f"nothing while every read it gives each Bun job names a manifest this tree does not have"))
        return
    try:
        manifest = json.loads(read_repository_text(REPO / MANIFEST))
    except json.JSONDecodeError as json_error:
        problems.append(error(MANIFEST, f"the root manifest is not valid JSON ({json_error.msg} at line "
                                        f"{json_error.lineno}), so its workspaces cannot be held to {TABLE}'s WORKSPACES"))
        return
    found = manifest.get("workspaces") if isinstance(manifest, dict) else None
    if not isinstance(found, list) or not all(isinstance(w, str) and w for w in found):
        problems.append(error(MANIFEST, "workspaces must be an array of workspace paths for "
                                        f"{TABLE}'s WORKSPACES to be held to it"))
        return
    declared = [workspace.removesuffix("/") for workspace in found]
    table = [workspace.removesuffix("/") for workspace in workspaces]
    for workspace in declared:
        if workspace in table:
            continue
        problems.append(error(MANIFEST, f"workspaces names {workspace}, which {TABLE}'s WORKSPACES does "
                                        f"not: a frozen `bun install` reads that manifest in every Bun job, so until the table "
                                        f"names it no filter has to select it, and a change to the package starts no job at all"))
    for workspace in table:
        if workspace in declared:
            continue
        problems.append(error(TABLE, f"WORKSPACES names {workspace}, which the root manifest's workspaces "
                                     f"does not: every Bun job then declares a read of {workspace}/package.json that no install "
                                     f"of this repository reads"))


# ------------------------------------------------- the one workflow a merge group runs
MERGE_GROUP = "merge_group"


def check_one_merge_group_workflow(problems: list) -> None:
    """ci.yaml is the only workflow of this repository that runs on a merge group.

    What holds it there is ci-ok-report.sh: it POSTs the failing ci-ok of a merge-group run the
    reporting job's own result decides, rather than waiting for the ci-ok job, so the queue
    learns the group is doomed while the rest of the run is still going. A POSTed check run
    joins the Actions App's first check suite at that commit rather than the posting run's
    (measured on pull request #2860 at 57182d42), and a ci-ok in a suite the queue does not read
    is a ci-ok the queue does not have: #2847 was refused with `Required status check "ci-ok" is
    expected` while a green ci-ok sat in PR Title's suite. That POST therefore lands where the
    queue reads it only because ci.yaml is
    the one workflow a merge group starts, which makes its suite the first one. A second
    merge_group workflow puts the early POST back in another suite, and nothing in ci.yaml would
    say so: the workflow that broke it is the new file. So the rule is about every other
    workflow, and names the file it found."""
    for name, document in directory_workflows().items():
        if name == WORKFLOW or MERGE_GROUP not in events(document):
            continue
        problems.append(error(name, f"{name} runs on {MERGE_GROUP}, which only {WORKFLOW} may: "
                                    f"ci-ok-report.sh POSTs the failing ci-ok of a merge-group run the reporting job's own "
                                    f"result decides, and a POSTed check run joins the Actions App's first check suite at that "
                                    f"commit rather than the posting run's, so a second {MERGE_GROUP} workflow can hold that "
                                    f"first suite and the queue never reads the POST - measured on pull request #2847, which "
                                    f"the queue refused to enqueue with `Required status check \"ci-ok\" is expected` while a "
                                    f"green ci-ok sat in another workflow's check suite. Run this work on pull_request, or "
                                    f"move it into {WORKFLOW}"))


# ------------------------------------------------- every daemon test binary in a /proc of its own
# The module whose test binaries CI runs in a PID namespace of their own, and the wrapper that puts
# them there, as a step writes it from that module's directory, with its own options, which come
# before the go command it runs.
DAEMON_MODULE = "packages/daemon"
PRIVATE_PROC = ".github/scripts/private_proc.py"
PRIVATE_PROC_OPTIONS = frozenset({"--sudo"})
# Each separator of a script line this rule follows: bash's &&, ||, ;, | and &, and the
# parentheses of a subshell. shell_commands reads a line into quote-aware words first (shlex), so
# a `|` inside a quoted -run selector stays a word rather than a pipeline separator, and splits
# each punctuation word shlex makes, such as `);` or `&&`, into its separators with this pattern
# alone: a punctuation word it does not split whole is a word of a command. A command
# substitution's `$(` and the `)` that closes it are read as spaces, so a go test it runs is read
# as a command too.
SEPARATOR = re.compile(r"(&&|\|\||[;|&()])")
# The words bash reads before a command and runs nothing for (`if`, `then`, `do`, `{`, `}`, `!`,
# `exec`, `time`, ...), as shell_commands.py lists them for check-release-credentials.sh, so a cd
# after one moves the directory as a cd alone does. env stays a word of the command: it is a runner
# whose `-C` this rule follows (env_directory).
SHELL_KEYWORDS = SKIP_WORDS - {"env"}
# The commands that read a test binary's name without running it. Any other command that names
# one, however it takes its options (env, timeout, setsid, stdbuf, nohup, bash -c, ...), runs it.
NAME_READERS = frozenset({"rm", "cp", "mv", "ls", "ln", "chmod", "chown", "mkdir", "touch", "cat", "file", "stat",
                          "du", "test", "[", "[[", "echo", "printf", "tee", "install", "readlink", "realpath",
                          "basename", "dirname", "wc", "sha256sum"})
# The characters a word holds when it is a script a command runs (`bash -c '…'`, `eval "…"`,
# `su -c '…'`, `flock lock -c '…'`): a blank or a separator, which a quoted word alone can hold.
SCRIPT_CHARACTERS = set(" \t\n;&|()")
# A directory a script names by anything but a plain path: a variable, a substitution, a home, a glob.
NOT_PLAIN = set("$`~*?[{")
UNKNOWN_DIRECTORY = "a directory a cd made unknown"


def in_daemon_module(directory: str) -> bool:
    return directory == DAEMON_MODULE or directory.startswith(DAEMON_MODULE + "/")


@cache
def workspace_modules(directory: str) -> frozenset | None:
    """The module directories the go.work that governs directory uses, read from the repository:
    go's own lookup, the nearest go.work from directory up to the root; None when none does."""
    path = posixpath.normpath(directory)
    while not path.startswith(".."):
        work = REPO / path / "go.work"
        if work.is_file():
            uses, block = set(), False
            for line in read_repository_text(work).splitlines():
                words = line.split("//", 1)[0].split()
                if words[:2] == ["use", "("]:
                    block = True
                elif block and words == [")"]:
                    block = False
                elif block and words or words[:1] == ["use"] and len(words) == 2:
                    uses.add(posixpath.normpath(posixpath.join(path, words[-1])))
            return frozenset(uses)
        if path == ".":
            return None
        path = posixpath.dirname(path) or "."
    return None


def reaches_daemon(directory: str) -> bool:
    """Whether a go command run in directory can name a package of the daemon module: it runs in
    that module, or the workspace that governs it uses that module. Elsewhere no package argument,
    not even one the rule cannot read, is the daemon's: go resolves none outside its workspace."""
    if in_daemon_module(directory):
        return True
    modules = workspace_modules(directory)
    return modules is not None and DAEMON_MODULE in modules


@cache
def daemon_import_path() -> str:
    """The daemon module's path, as its go.mod names it; "" in a tree with no such module, where no
    import path names it."""
    go_mod = REPO / DAEMON_MODULE / "go.mod"
    if not go_mod.is_file():
        return ""
    for line in read_repository_text(go_mod).splitlines():
        if line.startswith("module "):
            return line.split()[1]
    return ""


def moved(directory: str | None, target: str) -> str | None:
    """directory moved to target, a plain path; None for an unknown directory or target."""
    if directory is None or not target or target == "-" or set(target) & NOT_PLAIN:
        return None
    return posixpath.normpath(posixpath.join(directory, target))


def without_substitutions(text: str) -> str:
    """text with each `$(` and the `)` that closes it read as spaces, so the command a substitution
    runs is read as words of the command it stands in."""
    out, depth, opened = [], 0, []
    index = 0
    while index < len(text):
        if text.startswith("$(", index):
            opened.append(depth)
            depth += 1
            out.append("  ")
            index += 2
            continue
        char = text[index]
        if char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
            if opened and opened[-1] == depth:
                opened.pop()
                char = " "
        out.append(char)
        index += 1
    return "".join(out)


def shell_commands(text: str) -> list[str | list[str]]:
    """text split into its commands, each the list of its words, and the separators between them,
    each a string SEPARATOR names. shlex keeps a quoted word whole, so `go test -run 'Test|Example'
    ../daemon` is one command rather than two split at the selector's `|`, and the script of `bash
    -c 'go vet ./... && go test ./...'` is one word of its command, which script_commands reads as
    a script of its own; punctuation_chars holds only the shell separators this rule follows."""
    try:
        lexer = shlex.shlex(without_substitutions(text), posix=True, punctuation_chars=";&|()")
        lexer.whitespace_split, lexer.commenters = True, ""
        words = list(lexer)
    except ValueError:
        # A script Bash itself cannot parse runs no test. Read its words whole here rather than
        # pretend an unquoted punctuation character made commands it did not.
        return [text.split()] if text.strip() else []
    commands: list[str | list[str]] = []
    words_of_one: list[str] = []
    for word in words:
        separators = shell_separators(word)
        if separators is None:
            words_of_one.append(word)
            continue
        if words_of_one:
            commands.append(words_of_one)
            words_of_one = []
        commands.extend(separators)
    if words_of_one:
        commands.append(words_of_one)
    return commands


def shell_separators(word: str) -> list[str] | None:
    """The separators of one shlex punctuation word, as SEPARATOR splits it, or None when word is
    no punctuation word or SEPARATOR does not split the whole of it. shlex returns a run such as
    `);` as one word, while Bash executes its close and its next-command separator apart."""
    if not word or set(word) - set(";&|()"):
        return None
    separated = SEPARATOR.findall(word)
    return separated if "".join(separated) == word else None


def names_daemon(directory: str, package: str) -> bool | None:
    """Whether a package argument of a go test run in directory names a package of the daemon
    module; None when the rule cannot read it (a variable, a substitution, an absolute path, which
    is the runner's checkout). A relative pattern is read from directory, and one ending in `/...`
    or `...` takes every package below it, so `./...` from the repository root, whose go.work holds
    the daemon module, names it; an import path names it by the module's path (`all` among them,
    which names every package of the workspace)."""
    if set(package) & set("$`") or package.startswith("/"):
        return None
    if package in ("std", "cmd"):
        return False
    if package == "all":
        return True
    head, dots, _ = package.partition("...")
    if package.startswith("."):
        base = posixpath.normpath(posixpath.join(directory, head or "."))
        if not dots:
            return in_daemon_module(base)
        if not head or head.endswith("/") or head in (".", ".."):
            return in_daemon_module(base) or base == "." or DAEMON_MODULE.startswith(base + "/")
        return in_daemon_module(base) or DAEMON_MODULE.startswith(base)
    module = daemon_import_path()
    if not module:
        return False
    if not dots:
        return package == module or package.startswith(module + "/")
    return module.startswith(head) or head.startswith(module + "/")


def go_test_packages(args: list[str], value_flags: frozenset = VALUE_FLAGS) -> tuple[list[str], bool, bool, list[str]]:
    """The package arguments of a go test's words after its subcommand, as read_go_args reads them
    (private_proc.py's reader, the wrapper's own), whether it builds with -c or lists with -list and
    so runs no test, and its -o."""
    flags, packages = read_go_args(args, value_flags)
    builds = any(name == "-c" and value not in FALSE for name, value, _ in flags)
    lists = any(name == "-list" for name, _, _ in flags)
    return packages, builds, lists, [value or "" for name, value, _ in flags if name == "-o"]


# go list's flags whose value is the next word, beside the build flags go test shares with it.
GO_LIST_VALUE_FLAGS = VALUE_FLAGS | {"-f"}
# go's subcommands that run no test binary, so a command of one runs none here.
GO_RUNS_NO_TEST = frozenset({"build", "vet", "list", "mod", "env", "version", "fmt", "install", "work", "doc", "clean"})


def is_go(word: str) -> bool:
    """Whether a word runs go: one whose base name is go (a path to it), or a variable that may
    hold it (`"$GO"`, `$GOROOT/bin/go`)."""
    return posixpath.basename(word) == "go" or word.startswith("$")


def check_daemon_tests_in_private_proc(documents: dict, problems: list) -> None:
    """Every go test of packages/daemon that ci.yaml or a workflow it calls runs goes through
    private_proc.py, which runs each test binary beneath a supervisor at PID 1 of a PID and mount
    namespace of its own, with a new /proc that lists none of the runner's processes, and fails the
    step when a package with tests ran in none. A step that runs one outside it runs the module's
    tests on the runner's /proc, where nothing refuses a listing no scanner of the module sees: a
    raw read of the directory, a glob, a program such as pgrep. Nothing fails then; the tests pass on
    a runner whose processes happen to be harmless and fail on a machine several users share
    (docs/testing.md, "A test that lists the machine's /proc").

    It reads a step's script as bash does lines (a line that ends in `\\` goes on to the next), and
    each command of a line on its own (shell_commands, SEPARATOR), past the words bash reads before
    a command (SHELL_KEYWORDS: `if`, `then`, `do`, `{`, `}`, `!`, ...). A word of a command that
    holds a blank or a separator is a script that command may run (`bash -c '…'`, `sh -ec "…"`,
    `eval "…"`, `su -c '…'`, `flock lock -c '…'`, behind any runner), and is read as a script of
    its own, in the directory the command runs in, unless the command only reads names
    (NAME_READERS: `echo`, `printf`, ...). It follows the directory each command runs in: the
    step's working directory (step_workdir), moved by each `cd` and `pushd` of the script before
    it, their options aside, put back by `popd` and at the end of a subshell, and not moved by a `cd`
    of a pipeline or a background command, which runs in a subshell of its own, or of a script a
    command runs, which runs in a shell of its own; then by the command's own `env -C` (or
    `--chdir`) and its go's `-C`. A go is any word whose base name is `go` or a variable, and every
    go of a command is read, so a `go env` before a `go test` (`$(go env GOROOT)/bin/go test`) hides
    nothing. A `go test` whose words hold `-c` or `-list` builds or lists and runs no test; any other
    is judged by the packages it names (go_test_packages), each read from its directory
    (names_daemon), and by the packages `go list` feeds it through `xargs`. A test binary run by a
    command that is no reader of names, whatever runner it is behind, is refused unless a `go test
    -c` of the same step built it from another module, and so is a command that gives a word
    `-test.` flags with no test binary named, since the rule cannot place either. After a `cd` to a
    directory the script names by anything but a plain path the directory is unknown, and a go test
    there is refused, for the reason a step outside the module cannot say it is outside; so is a
    package the rule cannot read. The documents are ci.yaml and every workflow its jobs call, keyed
    by repository-relative path. What no text shows the rule cannot read: a script a workflow runs
    from a file of its own (scripts/e2e's proofs enroll their drivers in the wrapper themselves:
    live-daemon-proof.sh), and a command a variable holds. internal/hostproc holds those at run
    time: in CI it ends a test binary that has no record of the wrapper's (refuseUnwrapped)."""
    for path, document in documents.items():
        for job_id, job in (document.get("jobs") or {}).items():
            for step in job.get("steps") or []:
                script = step.get("run")
                if not isinstance(script, str):
                    continue
                for line, where in script_commands(script, step_workdir(document, job, step), {}):
                    problems.append(error(path, f"job {job_id} step \"{step.get('name') or line}\" runs `{line}` in "
                                                f"{where} outside {PRIVATE_PROC}: its test binaries then read the "
                                                f"runner's /proc, where a listing no scanner of the module checks sees "
                                                f"every process of the machine; write the go command behind the wrapper, "
                                                f"as `../../{PRIVATE_PROC} go test …` from the module's directory"))


def script_commands(script: str, directory: str | None, built: dict[str, bool | None]) -> list[tuple[str, str]]:
    """Each line of script that runs a test binary of packages/daemon outside the wrapper, with
    where it does (unwrapped_daemon_test), read as check_daemon_tests_in_private_proc says from
    directory, where the script starts. built is the step's test binaries (unwrapped_daemon_test),
    which a script a command of it runs shares; a line of such a script is said as the line of the
    step that holds it."""
    found: list[tuple[str, str]] = []
    pushed: list[str | None] = []
    subshells: list[str | None] = []
    for line in script_lines(script.replace("\\\n", " ")):
        tokens = shell_commands(line)
        listed: list[str] | None = None
        for at, token in enumerate(tokens):
            if token == "(":
                subshells.append(directory)
                continue
            if token == ")":
                directory = subshells.pop() if subshells else directory
                continue
            if isinstance(token, str):
                if token != "|":
                    listed = None
                continue
            words = token
            while words and words[0] in SHELL_KEYWORDS:
                words = words[1:]
            if not words or not words[0]:
                # Keywords alone, or a quoted empty argument, which is a word Bash passes but no
                # command: neither moves the directory, runs a test or is a test binary.
                continue
            before = tokens[at - 1] if at else ""
            after = tokens[at + 1] if at + 1 < len(tokens) else ""
            if words[0] in ("cd", "pushd", "popd"):
                if "|" in (before, after) or after == "&":
                    continue
                if words[0] == "popd":
                    directory = pushed.pop() if pushed else None
                    continue
                options = 1
                while options < len(words) and words[options].startswith("-") and words[options] != "-":
                    options += 1
                    if words[options - 1] == "--":
                        break
                if words[0] == "pushd":
                    pushed.append(directory)
                directory = moved(directory, words[options] if options < len(words) else "")
                continue
            if posixpath.basename(command_word(words)) not in NAME_READERS:
                place = env_directory(words, directory)
                for word in words[1:]:
                    if set(word) & SCRIPT_CHARACTERS:
                        found += [(line, where) for _, where in script_commands(word, place, built)]
            where = unwrapped_daemon_test(words, directory, built, listed)
            if words[:2] == ["go", "list"]:
                listed = read_go_args(words[2:], GO_LIST_VALUE_FLAGS, test=False)[1] or ["."]
            if where:
                found.append((line, where))
    return found


def command_word(words: list[str]) -> str:
    """The word a command runs: its first that is no `NAME=value` assignment."""
    return next((w for w in words if "=" not in w or w.startswith(("-", "/", "."))), "")


def env_directory(words: list[str], directory: str | None) -> str | None:
    """directory moved by the command's own `env -C DIR` (`-CDIR`, `--chdir DIR`, `--chdir=DIR`)
    or `sudo -D DIR` (`--chdir`), which run the command after them there."""
    for at, word in enumerate(words):
        runner = posixpath.basename(word)
        if runner not in ("env", "sudo"):
            continue
        short = "-C" if runner == "env" else "-D"
        for index in range(at + 1, len(words)):
            option = words[index]
            if option == short or option == "--chdir":
                directory = moved(directory, words[index + 1] if index + 1 < len(words) else "")
            elif option.startswith("--chdir="):
                directory = moved(directory, option.removeprefix("--chdir="))
            elif option.startswith(short) and len(option) > 2:
                directory = moved(directory, option[2:])
    return directory


def unwrapped_daemon_test(words: list[str], directory: str | None, built: dict[str, bool | None],
                          listed: list[str] | None) -> str:
    """Where one command runs a test binary of packages/daemon outside the wrapper, or "", in a
    form check_daemon_tests_in_private_proc reads. directory is where the command runs, None when a
    cd made it unknown; built maps each test binary a `go test -c -o` of the step built, by its path
    from the root, to whether it holds the daemon's packages, and takes this command's builds;
    listed is the packages a `go list` before it in its pipeline printed, for an `xargs go test`."""
    directory = env_directory(words, directory)
    ran_go = False
    for at, word in enumerate(words):
        if not is_go(word):
            continue
        rest, place = words[at + 1:], directory
        while rest and rest[0].startswith("-"):
            flag = rest[0].replace("--", "-", 1)
            if flag == "-C":
                place, rest = moved(place, rest[1] if len(rest) > 1 else ""), rest[2:]
            elif flag.startswith("-C="):
                place, rest = moved(place, flag.removeprefix("-C=")), rest[1:]
            else:
                rest = rest[1:]
        if rest[:1] and rest[0] in GO_RUNS_NO_TEST:
            ran_go = True
            continue
        if rest[:1] != ["test"]:
            continue
        wrapper = at - 1
        while wrapper >= 0 and words[wrapper] in PRIVATE_PROC_OPTIONS:
            wrapper -= 1
        if wrapper >= 0 and words[wrapper].endswith(PRIVATE_PROC):
            return ""
        packages, builds, lists, out = go_test_packages(rest[1:])
        fed = "xargs" in (posixpath.basename(w) for w in words[:at])
        if lists:
            return ""
        if place is None:
            return "" if builds else UNKNOWN_DIRECTORY
        reaches = reaches_daemon(place)
        if fed and listed is None and not builds and reaches:
            return f"{place}, whose packages xargs reads from a command the rule cannot read,"
        packages = packages + (listed or [] if fed else []) or ["."]
        named = [names_daemon(place, package) if reaches else False for package in packages]
        daemon = True if True in named else None if None in named else False
        if builds:
            names = out or ([f"{posixpath.basename(posixpath.normpath(posixpath.join(place, packages[0])))}.test"]
                            if len(packages) == 1 else [])
            for name in names:
                if name and name != "/dev/null":
                    built[posixpath.normpath(posixpath.join(place, name))] = daemon
            return ""
        if daemon is None:
            unread = next(package for package, named_it in zip(packages, named) if named_it is None)
            return f"{place}, whose package {unread} it cannot read,"
        if daemon:
            return place if in_daemon_module(place) else f"{'the root' if place == '.' else place}, whose packages are the daemon's,"
        return ""
    if ran_go or posixpath.basename(command_word(words)) in NAME_READERS:
        # A go that runs no test reads the words after it as its own (`go build -o launcher.test`).
        return ""
    placed = False
    for word in words:
        path = moved(directory, word) if directory is not None else None
        if path in built or posixpath.basename(word).endswith(".test"):
            if path in built and built[path] is False:
                placed = True
                continue
            return f"{directory or UNKNOWN_DIRECTORY}, the test binary {word} run by its own name,"
    if not placed and any(word.startswith("-test.") for word in words):
        return f"{directory or UNKNOWN_DIRECTORY}, a test binary given -test. flags under a name it cannot read,"
    return ""
