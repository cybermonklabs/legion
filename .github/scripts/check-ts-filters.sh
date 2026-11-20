#!/usr/bin/env bash
# ci.yaml's `changes` job selects each job through a dorny/paths-filter filter, and a change to a
# file a job reads but its filter misses skips that job, on the pull request and again on the
# merge group that lands it, after which nothing runs it. check-ci-filters.sh holds every filter
# to every file ci_job_reads.py says its job reads; for a Go job that set is derived from
# `go list`, so an import cannot slip past it, while for a Bun job the declared reads are
# hand-kept beside the filter, and the two go short together.
#
# This check derives the TypeScript half of them. For each pair of a changes filter and the tsc
# project the job that filter gates typechecks (.github/scripts/ci_ts_programs.py, PROGRAMS), it
# runs `bunx tsc -p <project> --noEmit --listFilesOnly`, keeps the files inside the repository
# and outside node_modules, makes each path relative to the repository root, and holds every one
# of them to that filter, failing with the filter, the file and the program it came from. The
# pairs are hand-kept and few; the file list is derived, which is the point: an import that
# reaches a new package fails this check on the pull request that writes it, with nobody
# remembering to widen a list. --listFilesOnly lists the program and stops without checking a
# type, so this is no second typecheck: the job's own step is what compiles it.
#
# The pairs cover every gate whose job runs a typecheck, each read from that job's own steps:
# contracts, envoy_client (packages/envoy-client and packages/broker-client), pi_shared,
# pi_envoy, pi_legion, claude_envoy, dispatch_ui (packages/dispatch and packages/proof-editor),
# docs (docs/site, in the docs-site.yaml job ci.yaml's docs job calls), conflicts_script
# (scripts/conflicts) and test (scripts/dispatch, scripts/github-apps, scripts/sessions and
# scripts/approval-eval).
# The gates left out are the ones whose job runs TypeScript but no typecheck, so there is no
# program to derive: `dispatch_browser`, whose jobs are the browser suite itself, sharded and in
# two-process mode, in the workflow ci.yaml's dispatch-browser job calls (the dashboard's and the
# proof editor's typechecks are dispatch_ui's, and
# .github/ci-notes/ci.md#the-dashboards-and-the-proof-editors-lint-typecheck-unit-tests-build-and
# says why they were split out), `envoy_session_stall`,
# whose job builds and runs the stall entries, `envoy_e2e_local`, whose live proof renders
# with @legion/envoy-client at run time, and `dispatch_two_builds` and `dispatch_two_builds_go`,
# the two-build job's gates, whose job runs the browser suite against two builds and whose Go
# step runs this tree's dispatch command. What such a job loads is a runtime closure rather than
# a tsc program, and stays a hand-kept read of ci_job_reads.py: envoy_client_dep for the
# dispatch_browser, envoy_e2e_local and two-build gates, a read of its own for the
# envoy_session_stall gate. Two parts of such closures this check does derive: every file the
# dispatch command the two-build Go step runs loads, from that command's bundle (the bundles,
# below), and the share of packages/contracts/src that dispatch_browser and the two two-build
# gates load, from dispatch_ui's programs and that bundle (the listed blocks, below).
#
# THE BUNDLES. A step that runs an entry with Bun loads that entry's runtime closure, which no
# tsc program of its job lists: the two-build job's Go step runs cmd/dispatch/twobuild_cli_test.go,
# which runs `bun packages/envoy-client/bin/dispatch.ts`. For each pair of a changes filter and
# such an entry (ci_ts_programs.py, BUNDLES), this check runs `bun build <entry> --target bun
# --outdir <scratch> --metafile=<scratch>/meta.json` in a scratch directory of its own, reads the
# metafile's `inputs` - every module the bundler loaded, resolved under the `bun` export
# condition `bun <entry>` resolves under - keeps the files inside the repository and outside
# node_modules, makes each path relative to the repository root, and holds every one of them to
# that filter as it holds a program's, failing with the filter, the file and the entry it came
# from. A bundle that does not build is a refusal naming the entry and the exit, never a bundle
# of no files, which every filter would cover; so is one that builds and leaves no metafile to
# read, or one whose `inputs` name no file inside the repository and outside node_modules, since
# a bundle loads at least its entry. Each such refusal shows the scratch directory as
# `<scratch>`, in the command and in what Bun or the metafile's read said, so it names no path
# that is gone by the time anyone reads it.
#
# THE WHOLE OUTPUT. Each command writes its output to a regular file of this run's own, which
# the check reads after the command exits; it never reads a command through a pipe. Where no
# node is on PATH, bunx runs tsc on Bun's own runtime, and Bun - every `bun build` too - writes
# to a pipe without blocking, keeps what a full pipe refuses in a queue, and drops that queue
# when the program exits. tsc exits with process.exit, so a reader the machine runs late gets
# the list cut at any byte, after an exit of 0. Measured with Bun 1.4.4, no node on PATH and a
# check that read through a pipe held to one CPU, so its reader ran late: one run passed holding
# 131 of the 230 files pi-envoy's program compiles, and another named a cut line as a file no
# filter selects, as a run on a machine at a load of about 550 named 'pa' and 'node_modul'. A
# write to a regular file waits until it is done, so the file holds every byte. Over that, a
# list is held whole: a program's output that is empty, or whose last line has no newline, is a
# refusal naming that line, and so is any line of a program's list or of a metafile's `inputs`
# that names no file this check can find, inside the repository or outside it, an empty line
# among them, since tsc lists only files it read and Bun only modules it loaded; and so is a
# program's list that keeps no file of the program's own directory, since a program holds at least
# its own sources. Each leaves its source unknown, as a command that fails does.
#
# Files a test reads by relative path - the JSON fixtures and corpora a test of one package
# reads out of another package's directory - are part of no tsc program, so they stay hand-kept
# too: the list of them is the tester's case in .github/scripts/check-ci-filters.test.sh, "every
# filter of a job that compiles or reads another package's files selects them", which measured
# each pair and holds it to the filter; ci_job_reads.py declares each of them as a read, and
# check-ci-filters.sh holds the filter to it.
#
# A file is held to the filter as workflow_paths.PathsFilter reads it, under the
# predicate-quantifier the changes job declares that filter under, over the filter's patterns
# with its anchors flattened into the list that names them - the patterns and the quantifier
# both from the engine of check-ci-filters.sh (ci_filter_coverage.changes_filters, which reads
# .github/ci-filters.yaml, the file the job's one paths-filter step hands the action, and the
# quantifier that step names) rather than this check reading that file a second way. It read
# the patterns as GitHub's `paths` instead
# until rule 17 of check-ci-filters.sh began writing into most filters the `!` patterns that
# keep a job off the Go test files it does not read, and refused a covered filter that had one,
# because GitHub's `paths` takes the last pattern that matches while dorny/paths-filter's
# `some-with-excludes` lets a `!` pattern exclude and its default `some` makes one match every
# file outside it. The one of the three that decides whether a job runs is the quantifier the
# step names, so that is the one read here: a `!` pattern excluding a file of a program or a
# bundle is still this check's refusal, since the job would then not run on a change that breaks
# its typecheck or the entry it runs.
#
# THE LISTED BLOCKS. A filter that names a whole tree selects its job for a change to any file of
# it, and where the job's programs compile only some of that tree - the Dispatch dashboard's two
# programs reached 42 of packages/contracts/src's 68 modules when this was written,
# packages/contracts/src/legion-api.ts not among them - the rest start a full run for a change
# no step of the job sees. Naming the files by hand
# would go stale the day one more module is imported, so for each entry of ci_ts_programs.py's
# LISTED this check derives them instead: every file under the entry's tree that the programs
# PROGRAMS pairs with the entry's source filters compile or the bundles BUNDLES pairs with them
# load - the union, where an entry names more than one source - in sorted order, one pattern
# each, in a listed block of the entry's filter - the shape of the generated `!` blocks rule 17 of
# check-ci-filters.sh writes, under a `# BEGIN listed` line naming the command that writes it and
# a `# END listed` line, so neither check takes the other's block for its own
# (ci_filter_coverage.TS_LISTED). It refuses a committed block that lacks a file its sources
# give, so a new import fails this check on the pull request that writes it, and one that
# holds a file they do not, so a module the job stops importing is dropped rather than kept; a
# block in a filter no entry names; a pattern of an entry's filter outside its block that
# matches a file under the tree, since which of those files the filter selects is the block's
# to say; and a file of the block that only another filter's programs or bundles give, which the
# filter does not select. Every refusal names the filter, the file and the command that writes
# the block. An entry's source is another filter where its job runs no program of its own and
# loads what that filter's programs compile or its bundles load. dispatch_browser takes the
# union of dispatch_ui's set and dispatch_two_builds_go's, the bundle of the dispatch command:
# its rows run the SPA and the e2e helpers that dispatch_ui's programs compile, and they spawn
# that command (packages/dispatch/e2e/dispatch-cli.ts:8,39). dispatch_two_builds takes the same
# union, for its browser half and for the command its Go step runs. LISTED says why for each.
# check-ci-filters.sh, which can run neither tsc nor `bun build`, holds no declared read to those
# files: rule 9's second direction passes over a listed block's patterns, since this check holds
# them. Its rule 15 reads a block's lines all the same, as what the tag step its filter gates
# loads: it holds the filter of the job that runs that package's untagged tests to them, envoy_go
# to dispatch_two_builds_go's block.
#
# THE TWO MODES, as check-ci-filters.sh has them. `check`, the default a bare call takes, is
# every refusal above. `write` lists the programs and builds the bundles, writes each listed
# block into .github/ci-filters.yaml - only that file, and only inside those blocks, one written
# where none was going after the filter's last item - and then runs the check over what it
# wrote, so every refusal that is not a listed block's still stands. A program that cannot be
# listed, or a bundle that cannot be built or whose metafile cannot be read or names no module,
# makes it write nothing, since the blocks would then be computed from a source that is
# unknown. Any other word, or more than one, fails naming both.
#
# Why ci.yaml's lint job and not ci-filters: the ci-filters job has no Bun and no node_modules -
# its steps are the checkout, actions/setup-go, check-ci-filters.sh, check-workflow-files.sh,
# the actionlint step and the ci-ok report - so
# `bunx tsc` there would install Bun and the dependency tree first, while lint already runs
# `bun install --frozen-lockfile` for the Biome pass; lint is gated by no `changes` output, so a
# change anywhere starts it, which is what a check of every filter needs; and the cost is small
# against that job's 30 minutes, 36.9 s measured over the nine package programs and 55 s over
# all fourteen programs this table held when that was measured, in a pod with node_modules
# already installed, and the bundle of the dispatch command builds in under a second (`bun
# build` reported 236 modules in 631 ms in such a pod).
#
# Run from anywhere: .github/scripts/check-ts-filters.sh
#                    .github/scripts/check-ts-filters.sh write
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# A bare call checks: that is the contract the lint job calls under, and a caller who says
# nothing is asking for the verdict, never for a rewrite of .github/ci-filters.yaml.
case "$#:${1:-}" in
  0: | 1:check | 1:write) ;;
  *)
    echo "::error file=.github/scripts/check-ts-filters.sh::this check takes one optional mode: \`check\` (the default) refuses a listed block of .github/ci-filters.yaml that is not the files its programs compile and its bundles load, and \`write\` writes those blocks instead. Run \`.github/scripts/check-ts-filters.sh\` or \`.github/scripts/check-ts-filters.sh write\`." >&2
    exit 1
    ;;
esac

# The pairs (ci_ts_programs.py), the path model (workflow_paths.py) and the engine this borrows
# the changes job's filters and the block helpers from (ci_filter_coverage.py) sit beside this
# script in the repository it checks. PYTHONDONTWRITEBYTECODE keeps the imports from leaving a
# __pycache__ in the tree.
TS_FILTERS_MODE="${1:-check}" PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

import ci_filter_coverage as engine
import workflow_paths as wp
from ci_ts_programs import BUNDLES, LISTED, PROGRAMS

TABLE = ".github/scripts/ci_ts_programs.py"
MODE = os.environ["TS_FILTERS_MODE"]
MARKS = engine.TS_LISTED
# Every pair of the two tables as (kind, filter, path, why): a tsc program, which PROGRAMS pairs
# with its project, and a bundle, which BUNDLES pairs with its entry. A refusal names one source
# of a kind with ONE's noun and verb, and a filter's set of them with ALL's.
PAIRS = ([("program", name, project, why) for name, project, why in PROGRAMS]
         + [("bundle", name, entry, why) for name, entry, why in BUNDLES])
ONE = {"program": ("tsc program", "compiles"), "bundle": ("bundle", "loads")}
ALL = {"program": ("tsc programs", "compile"), "bundle": ("bundles", "load")}

problems: list[engine.Annotation] = []


def kept(source: str, shown: str, lines: list, what: str, holds: str) -> list | None:
    """The files `lines` name inside the repository and outside node_modules, repository-relative
    and sorted. A relative path is read against the repository root, which is where both commands
    run and what each prints a relative path against. Every line must name a file this check can
    find, inside the repository or outside it: tsc lists only files it read and Bun's metafile
    only modules it loaded, so a line that names none is a cut line or no path at all, and read as
    a file it would hold a filter to a name no change can touch. Each such line is refused by
    name, and the source is then unknown: None. `what` counts the lines in the refusal, its `{}`
    taking their number (`printed {} line(s)`)."""
    files, nameless = set(), []
    for line in lines:
        path = Path(line.strip())
        if not path.is_absolute():
            path = engine.REPO / path
        if not line.strip() or not path.is_file():
            nameless.append(line)
            continue
        try:
            relative = path.resolve().relative_to(engine.REPO)
        except ValueError:  # tsc's own lib files and anything else outside this repository
            continue
        if "node_modules" in relative.parts:
            continue
        files.add(relative.as_posix())
    if nameless:
        named = ", ".join(repr(line) for line in nameless[:engine.SHOWN])
        more = f" and {len(nameless) - engine.SHOWN} more" if len(nameless) > engine.SHOWN else ""
        problems.append(engine.error(source, f"`{shown}` exited 0 and {what.format(len(nameless))} that name no file "
                                             f"this check can find: {named}{more}, so the list is cut or holds a name "
                                             f"that is no path, and the files its {holds} are unknown"))
        return None
    return sorted(files)


def run_whole(command: list) -> subprocess.CompletedProcess:
    """`command`'s exit and its whole output, run at the repository root: each stream goes to a
    regular file of this run's own, read once the command has exited, never to a pipe, for the
    reason THE WHOLE OUTPUT at the head of this file gives. The text is read as `text=True` reads
    it, in the locale's encoding with universal newlines."""
    with tempfile.TemporaryFile("w+") as out, tempfile.TemporaryFile("w+") as err:
        returncode = subprocess.run(command, cwd=engine.REPO, stdout=out, stderr=err).returncode
        out.seek(0)
        err.seek(0)
        return subprocess.CompletedProcess(command, returncode, out.read(), err.read())


def failed(path: str, command: str, result, holds: str, scratch: str = "") -> None:
    """The refusal of a command that could not list a source's files. The source is then
    unknown, which is a failure of its own rather than a source of no files that every filter
    covers. A build passes the scratch directory it gave Bun, which what Bun printed then shows as
    `<scratch>` (scrubbed)."""
    output = " / ".join(scrubbed(result.stdout + result.stderr, scratch).split()) or "no output"
    problems.append(engine.error(path, f"`{command}` failed (exit {result.returncode}), so the files its {holds} "
                                       f"are unknown: {output[:500]}"))


def scrubbed(text: str, scratch: str) -> str:
    """`text` with the scratch directory, as it was given and as it resolves, shown as
    `<scratch>`: the directory is this run's own and gone when it ends, so a refusal naming it
    would name a path nobody can read and read differently on every run. The longer form is
    replaced first, so neither leaves part of the other behind."""
    for form in sorted({scratch, os.path.realpath(scratch)} if scratch else (), key=len, reverse=True):
        text = text.replace(form, "<scratch>")
    return text


def program_files(project: str) -> list | None:
    """Every file of the project's tsc program that is inside the repository and outside
    node_modules, repository-relative. None when the command fails, or when its list is not
    whole: empty, ended by a line with no newline, holding a line that names no file (kept), or
    keeping no file of the program's own directory - the project itself, or the directory of the
    tsconfig file it names. A program holds at least its own sources, so a list cut at a line end
    after TypeScript's own library files, which no other guard sees, is refused by name rather
    than read as a program of no files, which every filter covers."""
    command = ["bunx", "tsc", "-p", project, "--noEmit", "--listFilesOnly"]
    shown = " ".join(command)
    result = run_whole(command)
    if result.returncode != 0:
        failed(project, shown, result, "program holds")
        return None
    if not result.stdout:
        problems.append(engine.error(project, f"`{shown}` exited 0 and printed nothing, so the files its program holds "
                                              f"are unknown: a program holds at least TypeScript's own library files"))
        return None
    if not result.stdout.endswith("\n"):
        last = result.stdout.rsplit("\n", 1)[-1]
        problems.append(engine.error(project, f"`{shown}` exited 0 and its last line, {last!r}, has no newline at its "
                                              f"end, so its output is cut and the files its program holds are unknown"))
        return None
    files = kept(project, shown, result.stdout.splitlines(), "printed {} line(s)", "program holds")
    if files is None:
        return None
    own = (Path(project).parent if project.endswith(".json") else Path(project)).as_posix()
    if not any(file.startswith(f"{own}/") for file in files):
        problems.append(engine.error(project, f"`{shown}` exited 0 and its list keeps {len(files)} file(s) of this "
                                              f"repository, none of them under {own}, the program's own directory, so "
                                              f"the list is cut or is another program's, and the files its program "
                                              f"holds are unknown: a program holds at least its own sources"))
        return None
    return files


def bundle_files(entry: str) -> list | None:
    """Every module `bun build` loads for the entry - the `inputs` of the metafile it writes,
    each a path relative to the directory Bun runs in - that is inside the repository and outside
    node_modules, repository-relative. The bundle and the metafile go to a scratch directory of
    this run's own, removed with them, and a refusal shows that directory as `<scratch>` - in the
    command, in what Bun printed and in the error a metafile it cannot read raised - so it reads
    the same on every run. None when the build fails, leaves no metafile to read, leaves one whose
    `inputs` hold a line that names no file (kept), or leaves one whose `inputs` name no module of
    the repository outside node_modules: a bundle loads at least its entry, so that metafile is no
    answer, and the empty set read from it would be one every filter covers and every listed
    block it gives would be written empty from."""
    with tempfile.TemporaryDirectory(prefix="check-ts-filters.") as scratch:
        command = ["bun", "build", entry, "--target", "bun", "--outdir", scratch, f"--metafile={scratch}/meta.json"]
        shown = scrubbed(" ".join(command), scratch)
        result = run_whole(command)
        if result.returncode != 0:
            failed(entry, shown, result, "bundle loads", scratch)
            return None
        try:
            inputs = list(json.loads((Path(scratch) / "meta.json").read_text())["inputs"])
        except (OSError, ValueError, KeyError, TypeError) as exc:
            problems.append(engine.error(entry, f"`{shown}` exited 0 and left no metafile whose `inputs` this check "
                                                f"can read ({type(exc).__name__}: {scrubbed(str(exc), scratch)}), so "
                                                f"the files its bundle loads are unknown"))
            return None
    files = kept(entry, shown, inputs, "the `inputs` of its metafile hold {} module(s)", "bundle loads")
    if files is None:
        return None
    if not files:
        problems.append(engine.error(entry, f"`{shown}` exited 0 and the `inputs` of its metafile name {len(inputs)} "
                                            f"module(s), none of them a file of this repository outside node_modules, "
                                            f"so the files its bundle loads are unknown: a bundle loads at least its "
                                            f"entry"))
        return None
    return files


def read_workflow() -> tuple:
    """The lines of .github/ci-filters.yaml and the changes job's filters with their quantifiers,
    all from the one read of that file changes_filters makes, under the quantifier ci.yaml's one
    paths-filter step names; whether a step may hand the action anything but that file is rule 21
    of check-ci-filters.sh, not this check's. Read again after the write mode writes. A file that
    is absent or refuses the open ends the run here, with the engine's annotation saying which."""
    try:
        filters, quantifiers, lines = engine.changes_filters(
            engine.load_repository_yaml(engine.REPO / engine.WORKFLOW) or {}, problems)
    except engine.RepositoryInputError as exc:
        print(engine.annotation_line(exc.annotation()), file=sys.stderr)
        raise SystemExit(1) from exc
    return lines, filters, quantifiers


def kinds(source: str) -> list:
    """The kinds of pair the two tables give filter `source`, programs first."""
    return [kind for kind in ("program", "bundle") if any(k == kind and pair == source for k, pair, _, _ in PAIRS)]


def named(sources) -> str:
    """Where a listed block's files come from, as its head and this check's report name it:
    `filter a's tsc programs and filter b's bundles`."""
    return " and ".join(f"filter {source}'s {' and '.join(ALL[kind][0] for kind in kinds(source))}"
                        for source in sources)


def give(givers) -> str:
    """The (source filter, kind) pairs that give a file, as a refusal's subject and verb: `the tsc
    programs of filter a compile and the bundles of filter b load`."""
    return " and ".join(f"the {ALL[kind][0]} of filter {source} {ALL[kind][1]}" for source, kind in givers)


def give_none(sources) -> str:
    """That no pair of `sources` gives a file: `no tsc program of filter a compiles and no bundle
    of filter b loads`."""
    return " and ".join(f"no {ONE[kind][0]} of filter {source} {ONE[kind][1]}"
                        for source in sources for kind in kinds(source))


def head(tree: str, sources) -> tuple:
    """A listed block's first lines: what it holds and the command that writes it, so a reader of
    .github/ci-filters.yaml knows not to edit one by hand. That command names this check as the
    block's owner: check-ci-filters.sh passes over the block's lines in its rule 9 only while the
    first line, in this `<begin> by `<script> write`` form, names a script a job of CI runs
    (ci_filter_coverage.listed_owners), which ci.yaml's lint job makes of this one. A block whose
    sources are tsc programs alone says they compile its files, and one with a bundle among them
    that they load them."""
    first = f"filter {sources[0]}'s"
    verb = "compile" if all(kind == "program" for source in sources for kind in kinds(source)) else "load"
    return (f"{MARKS.begin} by `{MARKS.write}`: the files of {tree} {first}",
            f"# {named(sources).removeprefix(first + ' ')} {verb}. An edit by hand is refused by that check, "
            f"which writes these.")


lines, filters, quantifiers = read_workflow()

# Each program is listed and each bundle built once, whichever pairs and listed blocks read it.
files_of: dict = {}
for kind, name, path, why in PAIRS:
    if (kind, path) not in files_of and name in filters and (engine.REPO / path).exists():
        files_of[kind, path] = program_files(path) if kind == "program" else bundle_files(path)

# Per LISTED entry's filter, the files its block must hold, or None where a source it is computed
# from is unknown, a refusal having been recorded then; and per file of the block, the (source
# filter, kind) pairs that give it, which a refusal names.
wanted: dict = {}
givers: dict = {}
for name, tree, sources, why in LISTED:
    if name in wanted:
        problems.append(engine.error(TABLE, f"LISTED names filter {name} twice; a filter has one listed block"))
        continue
    if not sources:
        problems.append(engine.error(TABLE, f"LISTED takes filter {name}'s files under {tree} from no filter, so the "
                                            f"block is computed from nothing"))
        wanted[name] = None
        continue
    known, gives = True, {}
    for source in sources:
        pairs = [(kind, path) for kind, pair, path, _ in PAIRS if pair == source]
        if not pairs:
            problems.append(engine.error(TABLE, f"LISTED takes filter {name}'s files under {tree} from filter {source}, "
                                                f"which PROGRAMS pairs with no tsc program and BUNDLES with no bundle, "
                                                f"so the block is computed from nothing"))
            known = False
        for kind, path in pairs:
            files = files_of.get((kind, path))
            if files is None:
                known = False
                continue
            for file in files:
                if file.startswith(f"{tree}/") and (source, kind) not in gives.setdefault(file, []):
                    gives[file].append((source, kind))
    wanted[name] = sorted(gives) if known else None
    givers[name] = gives

if MODE == "write":
    if any(files is None for files in wanted.values()) or problems:
        problems.append(engine.error(engine.FILTERS_FILE, "wrote no listed block: the files a block holds are "
                                                          "computed from programs and bundles this run could not "
                                                          "list, build or read"))
    else:
        rows = {name: (tree, sources) for name, tree, sources, _ in LISTED}
        engine.rewrite(engine.FILTERS_FILE, lines, engine.filter_blocks(lines, problems, MARKS), wanted,
                       heads=lambda name: head(*rows[name]), marks=MARKS)
        for name, tree, sources, _ in LISTED:
            print(f"filter {name}: wrote {len(wanted[name])} listed file(s) under {tree}, from {named(sources)}")
        lines, filters, quantifiers = read_workflow()

for kind, name, path, why in PAIRS:
    patterns = filters.get(name)
    if patterns is None:
        problems.append(engine.error(TABLE, f"{name} is no filter of the changes job, so the {kind} {path} ({why}) "
                                            f"is held to nothing"))
        continue
    if not (engine.REPO / path).exists():
        problems.append(engine.error(TABLE, f"{path} ({why}) is no path of this repository, so filter {name} is "
                                            f"held to no {kind}"))
        continue
    files = files_of.get((kind, path))
    if files is None:
        continue
    covers = wp.PathsFilter(patterns, quantifiers[name]).covers
    uncovered = [file for file in files if not covers(file)]
    shown = path if kind == "program" else f"bundle of {path}"
    print(f"{shown}: {len(files)} files, {len(uncovered)} uncovered (filter {name})")
    noun, verb = ONE[kind]
    for file in uncovered[:engine.SHOWN]:
        problems.append(engine.error(file, f"filter {name} does not select {file}, which the {noun} of {path} {verb} "
                                           f"({why}): the filter must select it, or the change that breaks that "
                                           f"{kind} starts no job"))
    if len(uncovered) > engine.SHOWN:
        problems.append(engine.error(engine.FILTERS_FILE, f"filter {name} misses "
                                                          f"{len(uncovered) - engine.SHOWN} more file(s) of {path}"))

blocks = engine.filter_blocks(lines, problems, MARKS)
for name in sorted(blocks):
    if blocks[name].begin >= 0 and name not in wanted:
        problems.append(engine.error(engine.FILTERS_FILE, f"{name} has a listed block, and LISTED of {TABLE} names "
                                                          f"no such filter, so nothing says what belongs in it; delete "
                                                          f"the block"))
for name, tree, sources, why in LISTED:
    files = wanted.get(name)
    if name not in filters:
        problems.append(engine.error(TABLE, f"LISTED names {name}, which is no filter of the changes job, so no "
                                            f"block holds its files under {tree}"))
        continue
    if files is None:
        continue
    gives = givers[name]
    block = blocks.get(name, engine.Block())
    committed = block.patterns
    where = f"filter {name}'s listed block" if block.begin >= 0 else f"filter {name}, which has no listed block,"
    regenerate = f"the block is generated from {named(sources)}, so run `{MARKS.write}`"
    for path in sorted(set(files) - set(committed)):
        problems.append(engine.error(engine.FILTERS_FILE, f"{where} lacks '{path}', which {give(gives[path])} "
                                                          f"({why}): {regenerate}"))
    for path in sorted(set(committed) - set(files)):
        problems.append(engine.error(engine.FILTERS_FILE, f"{where} has '{path}', which {give_none(sources)}, so "
                                                          f"it selects the job for a change no step of it sees: "
                                                          f"{regenerate}"))
    if set(committed) == set(files) and committed != files:
        problems.append(engine.error(engine.FILTERS_FILE, f"{where} holds its files out of sorted order or twice, "
                                                          f"and the same sources must write the same text on every "
                                                          f"run; run `{MARKS.write}`"))
    under = wp.walk(Path(tree)) if (engine.REPO / tree).is_dir() else []
    for pattern in dict.fromkeys(filters[name]):
        if pattern.startswith("!") or pattern in set(committed):
            continue
        regex = wp.glob_regex(pattern)
        hit = next((path for path in under if regex.fullmatch(path)), None)
        if hit:
            problems.append(engine.error(engine.FILTERS_FILE, f"filter {name} has '{pattern}' outside its listed "
                                                              f"block, and it matches {hit} under {tree}: which files "
                                                              f"of {tree} the filter selects is the block's to say, "
                                                              f"from {named(sources)}, so drop the pattern and run "
                                                              f"`{MARKS.write}`"))
    # A file that only another filter's pairs give is held to this filter here; one this filter's
    # own pairs give is held to it by the loop over the pairs above.
    foreign = [path for path in files if all(source != name for source, _ in gives[path])]
    if foreign:
        covers = wp.PathsFilter(filters[name], quantifiers[name]).covers
        for path in [path for path in foreign if not covers(path)][:engine.SHOWN]:
            problems.append(engine.error(path, f"filter {name} does not select {path}, which {give(gives[path])} "
                                               f"({why}): the filter must select it, or a change to it starts no job"))
    print(f"filter {name}: {len(files)} listed file(s) under {tree}, from {named(sources)}")

# Every problem, this check's own and the engine's (changes_filters and filter_blocks append
# theirs to the same list), is an engine Annotation, written as the engine writes one: a
# workflow command whose file and message are escaped, so no repository text can end it early.
for problem in problems:
    print(engine.annotation_line(problem), file=sys.stderr)
if problems:
    print(engine.annotation_line(engine.error(None, f"{len(problems)} problem(s) across {len(PROGRAMS)} tsc program(s), "
                                                    f"{len(BUNDLES)} bundle(s) and {len(LISTED)} listed block(s)")),
          file=sys.stderr)
    raise SystemExit(1)
print(f"every changes filter selects every file its job's typecheck compiles "
      f"({len(PROGRAMS)} programs, {len({name for name, _, _ in PROGRAMS})} filters) and every file the entries its "
      f"job runs load ({len(BUNDLES)} bundles, {len({name for name, _, _ in BUNDLES})} filters), and every listed "
      f"block holds exactly the files its programs compile and its bundles load ({len(LISTED)} blocks)")
PY
