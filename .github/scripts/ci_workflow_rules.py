"""The rules about ci.yaml that stand on their own: what no `if:` of it may say, what every job's
`if:` must say, what the cache fill workflows (CACHE_WORKFLOWS) must save
for the entries ci.yaml restores and which packages the daemon fill job's race build must compile, which workspaces
ci_job_reads.py's WORKSPACES must name,
that no other workflow of this repository runs on a merge group, and that no script or workflow
under .github fetches with a depth but through fetch-depth-if-shallow.sh.
None of them
reads a filter, a gate or the closure of a go command, so none belongs in the filter-coverage
engine beside them; check-ci-filters.sh's header numbers them 10, 12, 11, 13, 14, 16 and 26, and
ci_filter_coverage.main()
is what runs them, over the documents it already loaded where the rule reads only those - the
rerun-guard rule over ci.yaml alone, since the `if:` it reads is the caller job's; the cache
rule over ci.yaml and every workflow its jobs call, since a job's restore moves with its steps,
and over every other workflow of the directory, which it reads itself, for the key and paths,
since a release build's restore misses as silently; the push-condition rule over
ci.yaml and the called files that are ci.yaml's alone (ci_only_documents says why not the
others, and reads the directory to find them) - the workspace rule over the
root manifest and the WORKSPACES table main() hands it, and the race-fill rule over ci-caches.yaml
and the RACE_FILLS entries main() hands it the same way, since what this module imports is
the engine's names, never the data's, the merge-group rule over the workflow directory, which
it reads itself, and the shallow-fetch rule over the repository's file list main() hands it,
each file under .github/scripts, .github/actions and .github/workflows of which it reads itself.

It sits beside that engine (and workflow_paths.py, the path model) in the repository it checks,
and imports the engine's names for ci.yaml, ci-caches.yaml and the fill workflows rule 11 reads,
the repository root, the checks
main's ruleset requires, the reader of a job's `uses:` and the three
helpers that read an `if:`, and the path model's reader of a workflow's `on:`. main()
imports this module rather than the other way round, so the one import of the engine here is of
a module Python has finished loading by then.
"""

import json
import re
from collections import defaultdict
from functools import cache


from ci_filter_coverage import (
    CACHE_WORKFLOW,
    CACHE_WORKFLOWS,
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
    where_if,
)
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
PUSH_INSTEAD = (f"a cache that must be filled on a push to main belongs in {CACHE_WORKFLOW}, and ci-caches-changes.yaml "
                f"holds only the changes job's two entries, which must land once per commit; a job that must "
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
# The one suffix a save of a fill workflow may add to its restores' key: the commit the fill built.
# A family saved under it is filled again on every push, each entry under a key of its own, and a
# restore of the bare key takes the newest of them by prefix.
COMMIT_SUFFIX = "-${{ github.sha }}"
# A save's key that is the key a restore step of its job computed, as actions/cache/restore gives
# it in its cache-primary-key output: the key as that step evaluated it, before the build ran.
COMPUTED_KEY = re.compile(r"\$\{\{\s*steps\.([A-Za-z0-9_-]+)\.outputs\.cache-primary-key\s*\}\}")


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


# The composite actions of this repository, whose steps a job runs as its own.
ACTIONS_DIR = ".github/actions"


@cache
def local_actions() -> dict:
    """Every composite action of this repository, as {its action file's repository-relative path:
    the parsed document}, in path order."""
    directory = REPO / ACTIONS_DIR
    if not directory.is_dir():
        return {}
    return {path.relative_to(REPO).as_posix(): load_repository_yaml(path) or {}
            for path in sorted(directory.glob("*/action.y*ml"))}


def save_families(steps: list, where: str) -> tuple:
    """(The families the actions/cache/save steps among `steps` write, the saves among them whose
    family cannot be read), for one job or one composite action, `where` naming it. A save under
    `steps.<id>.outputs.cache-primary-key` writes the family of the actions/cache/restore of those
    steps with that id; one that names no such step (another job's output, or a step that is no
    restore) cannot be read, nor can a key whose text starts with an expression. A key's
    hashFiles() is read like any other expression after the family's text."""
    families, unread = set(), []
    for step in steps:
        if str((step or {}).get("uses", "")).split("@")[0] != "actions/cache/save":
            continue
        key = str((step.get("with") or {}).get("key", "")).strip()
        named = COMPUTED_KEY.fullmatch(key)
        if named:
            restore = next((s for s in steps if str((s or {}).get("id", "")) == named.group(1)
                            and str((s or {}).get("uses", "")).split("@")[0] == "actions/cache/restore"), None)
            if restore is None:
                unread.append(f"the save of {where} under `{key}`, which names no actions/cache/restore of its steps")
                continue
            key = str((restore.get("with") or {}).get("key", "")).strip()
        family = cache_family(key)
        if family:
            families.add(family)
        else:
            unread.append(f"the save of {where} under `{key}`, whose key starts with an expression")
    return families, unread


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


def saves_of(caches: dict, where: str, problems: list) -> list:
    """Every save of the fill workflow `where`, as (its job, its family, its key as its restores
    write it, its with:). A save under COMPUTED_KEY of a restore step of its own job takes that step's key text,
    the key the restore computed before the build; a save naming any other step is refused, and
    so is a save whose own key holds hashFiles(): the save evaluates it after the build, and the
    build can change the files it hashes (a go command may add the checksums it fetched to go.sum
    or go.work.sum), so the key it saves under need not be the one the restores ask for. On the
    repository, go-envoy-infra's save hashed its module files again and every push missed its
    lookup (run 45088325195: lookup ...-85ddfce1..., save ...-f2bbc22a...)."""
    saves = []
    for job_id, job in (caches.get("jobs") or {}).items():
        steps = (job or {}).get("steps") or []
        for step in steps:
            if str((step or {}).get("uses", "")).split("@")[0] != "actions/cache/save":
                continue
            given = step.get("with") or {}
            key = str(given.get("key", "")).rstrip()
            if "hashFiles(" in key:
                problems.append(error(where, f"job {job_id} saves under `{key}`, which holds hashFiles(): the "
                                             f"save evaluates it after the build, which can change a file it hashes, "
                                             f"so the entry can land under a key no restore asks for; save under "
                                             f"the key the lookup computed, `${{{{ steps.<id>.outputs.cache-primary-key }}}}`"))
                continue
            named = COMPUTED_KEY.fullmatch(key.strip())
            if named:
                restore = next((s for s in steps if str((s or {}).get("id", "")) == named.group(1)
                                and str((s or {}).get("uses", "")).split("@")[0] == "actions/cache/restore"), None)
                if restore is None:
                    problems.append(error(where, f"job {job_id} saves under `{key}`, but no "
                                                 f"actions/cache/restore of that job has the id {named.group(1)}, "
                                                 f"so nothing says which key the entry lands under"))
                    continue
                key = str((restore.get("with") or {}).get("key", "")).rstrip()
            saves.append((job_id, cache_family(key), key, given))
    return saves


def check_cache_round_trip(documents: dict, problems: list) -> None:
    """The fill workflows (CACHE_WORKFLOWS: ci-caches.yaml, and ci-caches-changes.yaml, which fills
    the changes job's entries in a concurrency group of its own) fill on a push to main the entries
    ci.yaml's run restores - its own jobs' restores, and those of the local workflows they call -
    so each family's save must carry
    exactly the key and the paths its restores name, or for the key, that key followed by
    COMMIT_SUFFIX: a key or a path that drifts apart makes every restore miss, and nothing fails -
    each job just rebuilds from nothing what the fill already built. A save under the key and the
    commit is a fill made again on every push: no entry then carries the restores' key itself, and
    GitHub gives each restore the newest entry whose key starts with it; any other suffix is
    drift, a restore key that itself names the commit is drift, and one family saved both ways is
    refused, in one fill workflow or across two, since its entry under the bare key would win every
    restore. A save's key is its own
    text, or the text of the restore step of its job whose computed key it names (saves_of says
    why a hashFiles() of its own is refused). A family is the key's text up to its first
    expression, and it is what
    pairs a save with the restores to hold it to; the keys are compared as text and the paths as
    the ordered lists of lines they name. A family ci.yaml restores that no fill workflow saves is
    no fault when another workflow of the directory saves it: docs-site.yaml saves go-docs.
    A family no save writes is one: its restore misses on every run and nothing fails, as a
    restore whose family name is misspelled does (`go-envoy-tests-` for go-envoy-test), so every
    restore of the directory names a family some save writes. The saves read for it are those of
    every workflow and of every composite action of .github/actions (save_families says how a
    key's family is read). A save whose family cannot be read leaves the refusal unable to say that
    no save writes the family, so the refusal then names each such save rather than saying none
    exists. The other way round is one too, a family nothing restores being a fill nobody reads.

    A fill workflow's own restores of a family it saves under the commit are what the fill builds
    on, and have one shape: no lookup-only at all (an expression can evaluate to true), so the
    newest entry downloads, and exactly its key followed by `-` as restore-keys, which no entry of
    another Go version starts with. Every other restore of a fill workflow is a lookup, which must
    be `lookup-only: true` with no
    restore-keys: the question it asks is whether that exact key is already there, the entry it
    would download is read by nothing in that workflow, and a prefix hit would answer for an older
    key and skip the build that fills this one. A fill workflow that is not in the repository saves
    nothing, and so has nothing to hold. Every refusal of a save or of a fill's restore names the
    fill workflow it is in.

    The restores are collected from every document of the set, ci.yaml and each workflow its
    jobs call, since a job that gives its steps to a called file takes its restore there with
    them and the entry it reads is the same one; each refusal names the file the restore is in.

    The key and the paths are held for every other workflow of the directory too: a release
    build (release.yaml's go-daemon, release-envoy-infra.yaml's go-envoy-infra), the nightly
    browser re-record (go-envoy) and the fill workflows' own restores read the entries they fill,
    and a restore there that drifts from the save misses as silently as one of ci.yaml's. Only
    the existence check above stays ci.yaml's: a fill is for ci.yaml's run, so a family only
    another workflow restores is still a fill ci.yaml does not read."""
    fills = {path: load_repository_yaml(REPO / path) or {} for path in CACHE_WORKFLOWS if (REPO / path).is_file()}
    if not fills:
        return
    restores = restores_of(documents)
    elsewhere = restores_of({path: document for path, document in directory_workflows().items()
                             if path not in documents})
    per_file = {where: saves_of(caches, where, problems) for where, caches in fills.items()}
    saves = [(where, *save) for where, file_saves in per_file.items() for save in file_saves]
    saved_anywhere, unread = set(), []
    for path, document in {**directory_workflows(), **documents}.items():
        for job_id, job in (document.get("jobs") or {}).items():
            families, missed = save_families((job or {}).get("steps") or [], f"job {job_id} of {path}")
            saved_anywhere |= families
            unread += missed
    for path, action in local_actions().items():
        families, missed = save_families(((action.get("runs") or {}).get("steps")) or [], path)
        saved_anywhere |= families
        unread += missed
    for path, document in {**directory_workflows(), **documents}.items():
        for job_id, family, _ in cache_steps(document, "restore"):
            if not family or family in saved_anywhere:
                continue
            if unread:
                why = (f"which no actions/cache/save this check can read writes. It cannot read the family "
                       f"of {'; '.join(unread)}, so one of those may write it: give such a save a key whose "
                       f"text names its family, or name a family a save writes "
                       f"({', '.join(sorted(saved_anywhere))})")
            else:
                why = (f"which no actions/cache/save of any workflow or local action of this repository "
                       f"writes: the restore misses on every run and nothing fails, the job building from "
                       f"nothing what it meant to restore; name a family a save writes "
                       f"({', '.join(sorted(saved_anywhere))})")
            problems.append(error(path, f"job {job_id} restores cache family {family}, {why}"))
    # One family saved both ways, in one fill workflow or across two: the hazard is the family's,
    # whichever files hold the two saves.
    refilled_in, bare_in = defaultdict(set), defaultdict(set)
    for where, _, family, key, _ in saves:
        (refilled_in if key.endswith(COMMIT_SUFFIX) else bare_in)[family].add(where)
    for family in sorted(refilled_in.keys() & bare_in.keys()):
        problems.append(error(sorted(bare_in[family])[0],
                              f"family {family} is saved both under `{COMMIT_SUFFIX}` (in "
                              f"{', '.join(sorted(refilled_in[family]))}) and without it (in "
                              f"{', '.join(sorted(bare_in[family]))}): the entry under the restores' own key wins every "
                              f"restore as an exact match, so the fills under the commit reach nobody"))
    for where, job_id, family, key, given in saves:
        paths = path_lines(given.get("path", ""))
        if family not in restores:
            problems.append(error(where, f"job {job_id} saves cache family {family}, which no "
                                         f"actions/cache/restore of {WORKFLOW} or of a workflow it calls "
                                         f"reads; this workflow exists to fill what {WORKFLOW} restores, so a "
                                         f"family nothing restores is a fill nobody reads: restore it there, "
                                         f"or drop the save"))
            continue
        for restore_where, restored in restores[family] + elsewhere.get(family, []):
            restored_key = str(restored.get("key", "")).rstrip()
            restored_paths = path_lines(restored.get("path", ""))
            expected = restored_key + COMMIT_SUFFIX if key.endswith(COMMIT_SUFFIX) else restored_key
            if key != expected:
                if restored_key and key.startswith(restored_key):
                    why = (f"that restore takes it only by prefix, and this workflow deletes the entries a fill "
                           f"replaced only once one under `{COMMIT_SUFFIX}` is there, so under any other suffix each "
                           f"fill's entry piles up beside the last")
                else:
                    why = ("a restore whose key is not the save's misses every time, and nothing fails - the job "
                           "rebuilds from nothing instead")
                problems.append(error(where, f"job {job_id} saves family {family} under key "
                                             f"`{key}`, but {restore_where} restores it under "
                                             f"`{restored_key}`: a save's key must be the restore's, or the "
                                             f"restore's followed by `{COMMIT_SUFFIX}`; {why}"))
            if paths != restored_paths:
                problems.append(error(where, f"job {job_id} saves family {family} from "
                                             f"{', '.join(paths) or '(no path)'}, but {restore_where} restores "
                                             f"it into {', '.join(restored_paths) or '(no path)'}: the "
                                             f"entry then holds other directories than the job reads, and the job rebuilds what "
                                             f"the fill already built"))
    for where, caches in fills.items():
        refilled = {family for _, family, key, _ in per_file[where] if key.endswith(COMMIT_SUFFIX)}
        check_fill_restores(where, caches, refilled, problems)


def check_fill_restores(where: str, caches: dict, refilled: set, problems: list) -> None:
    """The restores of one fill workflow, as check_cache_round_trip's second paragraph says: one of
    a family it saves under the commit (`refilled`) downloads the newest entry, and every other one
    is a lookup."""
    for job_id, family, given in cache_steps(caches, "restore"):
        given_lookup = given.get("lookup-only")
        lookup_text = str(given_lookup).strip().lower() if isinstance(given_lookup, bool) else str(given_lookup).strip()
        restore_keys = given.get("restore-keys")
        if family in refilled:
            prefix = str(given.get("key", "")).rstrip() + "-"
            # The fill's restore of such a family has one shape: no lookup-only at all (an
            # expression such as `${{ true }}` can evaluate to true, and this check does not
            # evaluate expressions), and exactly its key and a `-` as restore-keys.
            if given_lookup is not None:
                problems.append(error(where, f"job {job_id}'s actions/cache/restore of family "
                                             f"{family} is `lookup-only: {lookup_text}`, but this workflow saves the family "
                                             f"under `{COMMIT_SUFFIX}`, filling it again on every push: the fill builds "
                                             f"on the newest entry, so its restore must download it, or each fill "
                                             f"compiles everything from nothing; leave lookup-only out"))
            if path_lines(restore_keys or "") != [prefix]:
                problems.append(error(where, f"job {job_id}'s actions/cache/restore of family "
                                             f"{family} has restore-keys {', '.join(path_lines(restore_keys or '')) or '(none)'}, "
                                             f"but a family this workflow saves under `{COMMIT_SUFFIX}` takes exactly "
                                             f"`{prefix}`: the newest entry of the same Go, which a shorter prefix "
                                             f"would let an entry of another Go stand in for"))
            continue
        if lookup_text.lower() != "true":
            problems.append(error(where, f"job {job_id}'s actions/cache/restore of family "
                                         f"{family} is not `lookup-only: true`: it asks only whether that exact key is already "
                                         f"there, and the entry a restore downloads is read by no step of this workflow"))
        if restore_keys is not None:
            problems.append(error(where, f"job {job_id}'s actions/cache/restore of family "
                                         f"{family} has restore-keys: a prefix hit would answer for an older key, and the build "
                                         f"that fills this one would be skipped, leaving the key empty"))


# ------------------------------------------- the race builds that fill the race lanes' caches
# What makes a line of ci-caches.yaml a race build this rule holds: a `go test` running with
# -race, in a job ci_job_reads.py's RACE_FILLS names (the daemon's family's DAEMON_RACE_FILL_JOB
# and the envoy race lane's ENVOY_RACE_FILL_JOB). Spelled as the engine's tag-run triggers are, so
# `-racy` is not the flag and a go vet or go build line is no such command, and read through
# script_lines, so a comment is no trigger. The packages are the words beginning `./`, which is
# how a go command names one and how go_legs.py prints that same list for the race legs; the
# `-o /dev/null` of the build is no package. TABLE, the file the lists are in, is the workspaces
# section's constant below: both rules name that one file, so there is one spelling of it here. A
# race build of a job no entry names fills another family's cache, for another lane, and is not
# this rule's: the envoy-infra job's fills envoy-infra-build's unit step.
RACE_TEST = re.compile(r"\bgo\s+(?:-C\s+\S+\s+)?test\b")
RACE_FLAG = re.compile(r"(?<![\w-])-race\b")


def race_builds(document: dict, only_job: str) -> list:
    """Every race build of a workflow's job only_job: (the job, its line, the packages that line
    names)."""
    return [(job_id, line, [word for word in line.split() if word.startswith("./")])
            for job_id, job in (document.get("jobs") or {}).items() if job_id == only_job
            for step in ((job or {}).get("steps") or [])
            for line in script_lines(str((step or {}).get("run", "")))
            if RACE_TEST.search(line) and RACE_FLAG.search(line)]


def check_race_fill_packages(fills: list, problems: list) -> None:
    """Each entry of `fills` is (job, the name of the constant naming it, the package words its
    race build must name, the name of the list they come from, the step that restores the cache,
    the jobs that restore it, and the clause saying which steps follow the list): the race build
    of ci-caches.yaml's job `job` compiles exactly the packages of its lane. The workflow fills on
    a push to main the build cache the lane's race steps restore, so the two sides must name one
    set: a package the fill compiles and the lane does not leaves the entry holding a race binary
    no job ever restores, and a package the lane runs that the fill leaves out is linked from
    nothing in every run of that step. Neither fails anything - it is the silence
    check_cache_round_trip is about one level up, where the entry is found and holds the wrong
    objects - and the daemon's command drifted twice for want of this rule: the fill kept
    ./internal/record/ after #2867 took it out of the lane. The packages are held as a set and in
    order, each spelled as a command spells it
    (./internal/daemon/, ./internal/dispatch/delivery/...), since a command that reads in another
    order than the list is one a reader must sort before comparing, and an unreadable pair is
    where the next drift hides. A job with no race build at all fails too, rather than passing
    quietly: this rule exists to hold that one command, so a tree without it is a lane nothing
    fills, not a rule with nothing to do; and so does a workflow with no such job. A race build of
    any other job fills another family, and this rule does not read it. A ci-caches.yaml that is
    not in the repository fills nothing, and so has nothing to hold.

    The entries are an argument, as check_workspace_table's table is: main() holds
    ci_job_reads.py already, and nothing here imports it - the engine's names are all this module
    takes."""
    if not (REPO / CACHE_WORKFLOW).is_file():
        return
    caches = load_repository_yaml(REPO / CACHE_WORKFLOW) or {}
    for job, job_name, wanted, list_name, step, jobs, steps in fills:
        wanted = list(wanted)
        if job not in (caches.get("jobs") or {}):
            problems.append(error(CACHE_WORKFLOW, f"this workflow has no job {job}, which {TABLE}'s {job_name} names "
                                                  f"as the one that fills the cache {step} restores, so nothing "
                                                  f"fills it and that step links {', '.join(wanted) or '(no package)'} from nothing "
                                                  f"on every run; name the fill job there, or drop this rule with the lane it holds"))
            continue
        builds = race_builds(caches, job)
        if not builds:
            problems.append(error(CACHE_WORKFLOW, f"no step of job {job} runs a `go test` with -race, so "
                                                  f"nothing fills the build cache {step} restores and that step links "
                                                  f"{', '.join(wanted) or '(no package)'} from nothing on every run; put the race build "
                                                  f"back, or drop this rule with the lane it holds"))
            continue
        for job_id, line, named in builds:
            where = f"job {job_id}'s race build"
            extra = [package for package in named if package not in wanted]
            missing = [package for package in wanted if package not in named]
            for package in extra:
                problems.append(error(CACHE_WORKFLOW, f"{where} names {package}, which {TABLE}'s {list_name} "
                                                      f"does not ({', '.join(named)} against {', '.join(wanted) or '(no package)'}): this "
                                                      f"build fills the cache {step} restores, so it compiles a race binary "
                                                      f"no job of {jobs} restores, and nothing fails; either the command follows the "
                                                      f"list, or the package joins {list_name}, the one place the lane is named - "
                                                      f"{steps}"))
            for package in missing:
                problems.append(error(CACHE_WORKFLOW, f"{TABLE}'s {list_name} names {package}, which {where} "
                                                      f"does not ({', '.join(wanted)} against {', '.join(named) or '(no package)'}): the "
                                                      f"fill then misses a race binary {step} links from nothing on every "
                                                      f"run, and nothing fails; either the command follows the list, or the package leaves "
                                                      f"{list_name}, the one place the lane is named - {steps}"))
            if named != wanted and not extra and not missing:
                problems.append(error(CACHE_WORKFLOW, f"{where} names {', '.join(named)}, where {TABLE}'s "
                                                      f"{list_name} has {', '.join(wanted)}: the same packages in another order, so the "
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
    and a change to that package starts no job at all - which is how packages/cipher-client came
    to have its typecheck and its tests run nowhere. The other way round is a read every Bun job
    declares of a manifest no install reads. The table is an argument because main() holds it
    already, and because nothing here imports ci_job_reads: the engine's names are all this
    module takes.

    The manifest is read as `.github/scripts/check-bun-workspace-manifests.sh#root_manifest` reads it,
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


# ------------------------------------------------- no fetch with a depth outside the one helper
# A `git fetch --depth=<n>` in a whole clone makes that clone shallow: git writes the commits at
# the depth's edge into the clone's shallow file, and every later read treats them as having no
# parents. Every worktree of a clone shares that one file, so a fetch in one of them cuts the
# history all of them read, and git merge-base and an ancestor test then answer from the commits
# still in view. A script under .github runs in CI's shallow checkout and also in developers'
# clones, so its fetch goes through SHALLOW_FETCH_HELPER, which passes the depth only to a shallow
# repository.
SHALLOW_FETCH_DIRS = (".github/scripts/", ".github/actions/", ".github/workflows/")
SHALLOW_FETCH_HELPER = ".github/scripts/fetch-depth-if-shallow.sh"
DEPTH_OPTIONS = ("--depth", "--deepen", "--shallow-since", "--shallow-exclude")
_DEPTH_OPTION = r"(--depth|--deepen|--shallow-since|--shallow-exclude)(?=[=\s\"',)\]]|$)"
# A shell command, with its quoted words blanked (blank_quotes): git as a command word, then fetch
# or pull and a depth option within one command, so no `;`, `|` or `&&` between them, though a
# `2>&1` or `&>` redirection may be.
_SAME_COMMAND = r"(?:[^;|&\n]|>&|&>)*?"
SHELL_DEPTH_FETCH = re.compile(rf"(?:^|[\s;&|(`])git\s{_SAME_COMMAND}\b(?:fetch|pull)\b{_SAME_COMMAND}{_DEPTH_OPTION}")
# A quoted word: a Python or JavaScript argument list's element, or a command handed on as one string.
QUOTED_WORD = re.compile(r"\"((?:\\.|[^\"\\])*)\"|'([^']*)'")
# The repository a fetch writes to, when `git -C <dir>` names it.
GIT_DIR_OPERAND = re.compile(r"\bgit\s+-C\s+(\"[^\"]*\"|'[^']*'|\S+)")
# `name=$(mktemp ...)`: a directory the harness made for itself.
MKTEMP_VARIABLE = re.compile(r"(?:^|[\s;(])(\w+)=\"?\$\(mktemp\b", re.M)


def logical_lines(text: str) -> list:
    """(the number of its first line, the text) of each line of a script or workflow file, with a
    line that ends in a backslash joined to the next, as bash joins it; comment lines (`#`, and
    `//` for the TypeScript and Go beside the scripts) aside."""
    out, start, parts = [], 0, []
    for number, line in enumerate(text.split("\n"), 1):
        if not parts:
            start = number
            if line.lstrip().startswith(("#", "//")):
                continue
        if line.endswith("\\"):
            parts.append(line[:-1])
            continue
        parts.append(line)
        out.append((start, " ".join(parts)))
        parts = []
    if parts:
        out.append((start, " ".join(parts)))
    return out


def blank_quotes(line: str) -> str:
    """The line with the text inside each shell quote blanked, the quotes kept: the words a
    command passes as data (an echo's message, a printf's format) are not the command. A `$(`
    inside double quotes opens a command of its own, read as a new line is, to its `)`, so
    `"$(contains "$out" "...")"` blanks the inner strings too and keeps the command between."""
    out, i, n = [], 0, len(line)
    levels = [[None, 0]]  # per command substitution: the open quote, and the `(`s open in it
    while i < n:
        c = line[i]
        level = levels[-1]
        quote = level[0]
        if quote == "'":
            if c == "'":
                level[0] = None
            out.append(c if c == "'" else " ")
            i += 1
            continue
        if c == "\\" and i + 1 < n:
            out.append(line[i:i + 2] if quote is None else "  ")
            i += 2
            continue
        if line.startswith("$(", i):
            levels.append([None, 0])
            out.append("$(")
            i += 2
            continue
        if quote == '"':
            if c == '"':
                level[0] = None
            out.append(c if c == '"' else " ")
        else:
            if c in "'\"":
                level[0] = c
            elif c == "(":
                level[1] += 1
            elif c == ")":
                if level[1]:
                    level[1] -= 1
                elif len(levels) > 1:
                    levels.pop()
            out.append(c)
        i += 1
    return "".join(out)


def depth_fetch(path: str, line: str) -> str | None:
    """The depth option a git fetch or pull on this line passes, or None: as a shell command,
    outside a Python file, whose docstrings are prose; or as quoted words, in any file - an argument
    list whose "fetch" or "pull" follows git and comes before a depth option, or one quoted command
    string that starts with git."""
    if not path.endswith(".py"):
        found = SHELL_DEPTH_FETCH.search(blank_quotes(line))
        if found:
            return found.group(1)
    quoted = [(m.start(), m.group(1) if m.group(1) is not None else m.group(2)) for m in QUOTED_WORD.finditer(line)]
    for _, word in quoted:
        if word.startswith("git "):
            found = SHELL_DEPTH_FETCH.search(word)
            if found:
                return found.group(1)
    for at, (start, word) in enumerate(quoted):
        if word in ("fetch", "pull") and re.search(r"\bgit\b", line[:start]):
            for _, later in quoted[at + 1:]:
                option = next((o for o in DEPTH_OPTIONS if later == o or later.startswith(f"{o}=")), None)
                if option:
                    return option
    return None


def fixture_fetch(text: str, line: str) -> bool:
    """A harness's fetch into a fixture repository it made: the line names its repository with
    `git -C <dir>`, and <dir> is under a directory the harness made with mktemp - `$W/...` where the
    harness assigns `W=$(mktemp ...)`, or a variable the harness assigns `$W/...` or `$(mktemp ...)`.
    Such a repository is new, so a depth there cuts no clone anyone else reads."""
    operand = GIT_DIR_OPERAND.search(line)
    if not operand:
        return False
    word = operand.group(1).strip("\"'")
    made = set(MKTEMP_VARIABLE.findall(text))
    match = re.fullmatch(r"\$\{?(\w+)\}?(/.*)?", word)
    if not match:
        return False
    name, below = match.groups()
    if name in made:
        return True
    if below:
        return False
    assigned = re.findall(rf"(?:^|[\s;(]){re.escape(name)}=\"?\$\{{?(\w+)\}}?/", text, re.M)
    return any(parent in made for parent in assigned)


def check_shallow_fetches(files: list, problems: list) -> list:
    """Rule 26: no file under .github/scripts, .github/actions or .github/workflows runs a git fetch
    or git pull with one of DEPTH_OPTIONS, but SHALLOW_FETCH_HELPER itself, and, in a harness
    (`*.test.sh`), a fetch into a fixture repository the harness made (fixture_fetch). Each refusal
    names the file, the line and the command. The rule reads the words as written: a depth option
    handed to git through a variable is not seen, which is the review's to catch. Returns the
    report line."""
    paths = [f for f in files if f.startswith(SHALLOW_FETCH_DIRS) and f != SHALLOW_FETCH_HELPER]
    if not paths:
        problems.append(error(None, f"rule 26 found no file under {', '.join(SHALLOW_FETCH_DIRS)} to read, so whether "
                                    "a script there fetches with a depth is unknown"))
        return []
    calls = fixtures = 0
    for path in paths:
        text = (REPO / path).read_bytes().decode("utf-8", "replace")
        for number, line in logical_lines(text):
            option = depth_fetch(path, line)
            if option is None:
                continue
            calls += 1
            if path.endswith(".test.sh") and fixture_fetch(text, line):
                fixtures += 1
                continue
            command = " ".join(line.split())[:160]
            problems.append(error(path, f"{path} line {number} runs `{command}`: a git fetch or pull with "
                                        f"{option} makes a whole clone shallow, and every worktree of that clone then "
                                        f"reads a cut history. Fetch through {SHALLOW_FETCH_HELPER} --depth=<n> <git "
                                        f"fetch arguments>, which passes the depth only to a shallow repository; a "
                                        f"harness may fetch with a depth only into a fixture it made, named by git -C "
                                        f"<dir> under its mktemp directory"))
    return [f"rule 26: {len(paths)} files under {', '.join(SHALLOW_FETCH_DIRS)} read; {calls} git fetch or pull "
            f"line(s) with a depth, {fixtures} of them a harness's fetch into its own fixture, "
            f"{calls - fixtures} refused"]
