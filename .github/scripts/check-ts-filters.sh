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
# proof editor's typechecks are dispatch_ui's, and ci.yaml's comment on dispatch-ui says why they
# were split out), `envoy_session_stall`,
# whose job builds and runs the stall entries, and `envoy_e2e_local`, whose live proof renders
# with @legion/envoy-client at run time. What such a job loads is a runtime closure rather than a
# tsc program, and stays a hand-kept read of ci_job_reads.py: envoy_client_dep for the
# dispatch_browser and envoy_e2e_local gates, a read of its own for the envoy_session_stall gate.
# The one part of such a closure this check does derive is dispatch_browser's share of
# packages/contracts/src, from dispatch_ui's programs (the listed blocks, below).
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
# every paths-filter step of the job, and the job declares its filters in two of them) rather
# than this check reading those steps a second way. It read the patterns as GitHub's `paths`
# instead
# until rule 17 of check-ci-filters.sh began writing into most filters the `!` patterns that
# keep a job off the Go test files it does not read, and refused a covered filter that had one,
# because GitHub's `paths` takes the last pattern that matches while dorny/paths-filter's
# `some-with-excludes` lets a `!` pattern exclude and its default `some` makes one match every
# file outside it. The one of the three that decides whether a job runs is the quantifier the
# step names, so that is the one read here: a `!` pattern excluding a file of the program is
# still this check's refusal, since the job would then not run on a change that breaks its
# typecheck.
#
# THE LISTED BLOCKS. A filter that names a whole tree selects its job for a change to any file of
# it, and where the job's programs compile only some of that tree - the Dispatch dashboard's two
# programs reached 42 of packages/contracts/src's 68 modules when this was written,
# packages/contracts/src/legion-api.ts not among them - the rest start a full run for a change
# no step of the job sees. Naming the files by hand
# would go stale the day one more module is imported, so for each entry of ci_ts_programs.py's
# LISTED this check derives them instead: every file under the entry's tree that the programs
# PROGRAMS pairs with the entry's source filter compile, in sorted order, one pattern each, in a
# listed block of the entry's filter - the shape of the generated `!` blocks rule 17 of
# check-ci-filters.sh writes, under a `# BEGIN listed` line naming the command that writes it and
# a `# END listed` line, so neither check takes the other's block for its own
# (ci_filter_coverage.TS_LISTED). It refuses a committed block that lacks a file the programs
# compile, so a new import fails this check on the pull request that writes it, and one that
# holds a file they do not, so a module the job stops importing is dropped rather than kept; a
# block in a filter no entry names; a pattern of an entry's filter outside its block that
# matches a file under the tree, since which of those files the filter selects is the block's
# to say; and, for an entry whose source is another filter, a file of the block the filter does
# not select. Every refusal names the filter, the file and the command that writes the block. An
# entry's source is another filter where its job runs no program of its own and loads what
# that filter's programs compile: dispatch_browser takes dispatch_ui's set, and LISTED says why.
# check-ci-filters.sh, which cannot run tsc, holds no declared read to those files: rule 9's
# second direction passes over a listed block's patterns, since this check holds them.
#
# THE TWO MODES, as check-ci-filters.sh has them. `check`, the default a bare call takes, is
# every refusal above. `write` lists the programs, writes each listed block into ci.yaml - only
# ci.yaml, and only inside those blocks, one written where none was going after the filter's
# last item - and then runs the check over what it wrote, so every refusal that is not a
# listed block's still stands. A program that cannot be listed makes it write nothing, since
# the blocks would then be computed from a program that is unknown. Any other word, or more than
# one, fails naming both.
#
# Why ci.yaml's lint job and not ci-filters: the ci-filters job has no Bun and no node_modules -
# its steps are the checkout, actions/setup-go, check-ci-filters.sh and the ci-ok report - so
# `bunx tsc` there would install Bun and the dependency tree first, while lint already runs
# `bun install --frozen-lockfile` for the Biome pass; lint is gated by no `changes` output, so a
# change anywhere starts it, which is what a check of every filter needs; and the cost is small
# against that job's 30 minutes, 36.9 s measured over the nine package programs and 55 s over
# all fourteen programs this table held when that was measured, in a pod with node_modules
# already installed.
#
# Run from anywhere: .github/scripts/check-ts-filters.sh
#                    .github/scripts/check-ts-filters.sh write
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# A bare call checks: that is the contract the lint job calls under, and a caller who says
# nothing is asking for the verdict, never for a rewrite of ci.yaml.
case "$#:${1:-}" in
  0: | 1:check | 1:write) ;;
  *)
    echo "::error file=.github/scripts/check-ts-filters.sh::this check takes one optional mode: \`check\` (the default) refuses a listed block of ci.yaml that is not the files its programs compile, and \`write\` writes those blocks instead. Run \`.github/scripts/check-ts-filters.sh\` or \`.github/scripts/check-ts-filters.sh write\`." >&2
    exit 1
    ;;
esac

# The pairs (ci_ts_programs.py), the path model (workflow_paths.py) and the engine this borrows
# the changes job's filters and the block helpers from (ci_filter_coverage.py) sit beside this
# script in the repository it checks. PYTHONDONTWRITEBYTECODE keeps the imports from leaving a
# __pycache__ in the tree.
TS_FILTERS_MODE="${1:-check}" PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import os
import subprocess
import sys
from pathlib import Path

import yaml

import ci_filter_coverage as engine
import workflow_paths as wp
from ci_ts_programs import LISTED, PROGRAMS

TABLE = ".github/scripts/ci_ts_programs.py"
MODE = os.environ["TS_FILTERS_MODE"]
MARKS = engine.TS_LISTED

problems: list[str] = []


def listed(project: str) -> list | None:
    """Every file of the project's tsc program that is inside the repository and outside
    node_modules, repository-relative. None when the command fails: the program is then unknown,
    which is a failure of its own rather than a program of no files that every filter covers."""
    command = ["bunx", "tsc", "-p", project, "--noEmit", "--listFilesOnly"]
    result = subprocess.run(command, cwd=engine.REPO, capture_output=True, text=True)
    if result.returncode != 0:
        output = " / ".join((result.stdout + result.stderr).split()) or "no output"
        problems.append(f"::error file={project}::`{' '.join(command)}` failed (exit {result.returncode}), so the "
                        f"files its program holds are unknown: {output[:500]}")
        return None
    files = set()
    for line in result.stdout.splitlines():
        if not line.strip():
            continue
        path = Path(line.strip())
        if not path.is_absolute():
            path = engine.REPO / path
        try:
            relative = path.resolve().relative_to(engine.REPO)
        except ValueError:  # tsc's own lib files and anything else outside this repository
            continue
        if "node_modules" in relative.parts:
            continue
        files.add(relative.as_posix())
    return sorted(files)


def read_workflow() -> tuple:
    """ci.yaml's text and the changes job's filters with their quantifiers. changes_filters reads
    every dorny/paths-filter step of the job, which declares its filters in two of them (ci.yaml's
    `changes` job says why), so both strings are covered here; the steps a filter is declared in
    are rule 19's, not this check's. Read again after the write mode writes."""
    text = (engine.REPO / engine.WORKFLOW).read_text()
    filters, quantifiers, _steps = engine.changes_filters(yaml.safe_load(text), problems)
    return text.splitlines(), filters, quantifiers


def head(tree: str, source: str) -> tuple:
    """A listed block's first lines: what it holds and the command that writes it, so a reader of
    ci.yaml knows not to edit one by hand."""
    return (f"{MARKS.begin} by `{MARKS.write}`: the files of {tree} filter {source}'s",
            "# tsc programs compile. An edit by hand is refused by that check, which writes these.")


lines, filters, quantifiers = read_workflow()

# Each program is listed once, whichever pairs and listed blocks read it.
programs: dict = {}
for name, project, why in PROGRAMS:
    if project not in programs and name in filters and (engine.REPO / project).exists():
        programs[project] = listed(project)

# Per LISTED entry's filter, the files its block must hold, or None where a program it is computed
# from is unknown; a refusal has then been recorded already.
wanted: dict = {}
for name, tree, source, why in LISTED:
    projects = [project for pair, project, _ in PROGRAMS if pair == source]
    if name in wanted:
        problems.append(f"::error file={TABLE}::LISTED names filter {name} twice; a filter has one listed block")
        continue
    if not projects:
        problems.append(f"::error file={TABLE}::LISTED takes filter {name}'s files under {tree} from filter {source}, "
                        f"which PROGRAMS pairs with no tsc program, so the block is computed from nothing")
        wanted[name] = None
        continue
    compiled = [programs.get(project) for project in projects]
    if any(files is None for files in compiled):
        wanted[name] = None
        continue
    wanted[name] = sorted({f for files in compiled for f in files if f.startswith(f"{tree}/")})

if MODE == "write":
    if any(files is None for files in wanted.values()) or problems:
        problems.append(f"::error file={engine.WORKFLOW}::wrote no listed block: the files a block holds are "
                        f"computed from programs this run could not list or read")
    else:
        sources = {name: (tree, source) for name, tree, source, _ in LISTED}
        engine.rewrite(engine.WORKFLOW, lines, engine.filter_blocks(lines, problems, MARKS), wanted,
                       heads=lambda name: head(*sources[name]), marks=MARKS)
        for name, tree, source, _ in LISTED:
            print(f"filter {name}: wrote {len(wanted[name])} listed file(s) under {tree}, from filter {source}'s "
                  f"programs")
        lines, filters, quantifiers = read_workflow()

for name, project, why in PROGRAMS:
    patterns = filters.get(name)
    if patterns is None:
        problems.append(f"::error file={TABLE}::{name} is no filter of the changes job, so the program "
                        f"{project} ({why}) is held to nothing")
        continue
    if not (engine.REPO / project).exists():
        problems.append(f"::error file={TABLE}::{project} ({why}) is no path of this repository, so filter {name} "
                        f"is held to no program")
        continue
    files = programs.get(project)
    if files is None:
        continue
    covers = wp.PathsFilter(patterns, quantifiers[name]).covers
    uncovered = [path for path in files if not covers(path)]
    print(f"{project}: {len(files)} files, {len(uncovered)} uncovered (filter {name})")
    for path in uncovered[:engine.SHOWN]:
        problems.append(f"::error file={path}::filter {name} does not select {path}, which the tsc program of "
                        f"{project} compiles ({why}): the filter must select it, or the change that breaks that "
                        f"program starts no job")
    if len(uncovered) > engine.SHOWN:
        problems.append(f"::error file={engine.WORKFLOW}::filter {name} misses "
                        f"{len(uncovered) - engine.SHOWN} more file(s) of {project}")

blocks = engine.filter_blocks(lines, problems, MARKS)
for name in sorted(blocks):
    if blocks[name].begin >= 0 and name not in wanted:
        problems.append(f"::error file={engine.WORKFLOW}::{name} has a listed block, and LISTED of {TABLE} names no "
                        f"such filter, so nothing says what belongs in it; delete the block")
for name, tree, source, why in LISTED:
    files = wanted.get(name)
    if name not in filters:
        problems.append(f"::error file={TABLE}::LISTED names {name}, which is no filter of the changes job, so no "
                        f"block holds its files under {tree}")
        continue
    if files is None:
        continue
    block = blocks.get(name, engine.Block())
    committed = block.patterns
    where = f"filter {name}'s listed block" if block.begin >= 0 else f"filter {name}, which has no listed block,"
    for path in sorted(set(files) - set(committed)):
        problems.append(f"::error file={engine.WORKFLOW}::{where} lacks '{path}', which the tsc programs of filter "
                        f"{source} compile ({why}): the block is generated from those programs, so run "
                        f"`{MARKS.write}`")
    for path in sorted(set(committed) - set(files)):
        problems.append(f"::error file={engine.WORKFLOW}::{where} has '{path}', which no tsc program of filter "
                        f"{source} compiles, so it selects the job for a change no step of it sees: the block is "
                        f"generated from those programs, so run `{MARKS.write}`")
    if set(committed) == set(files) and committed != files:
        problems.append(f"::error file={engine.WORKFLOW}::{where} holds its files out of sorted order or twice, and "
                        f"the same programs must write the same text on every run; run `{MARKS.write}`")
    under = wp.walk(Path(tree)) if (engine.REPO / tree).is_dir() else []
    for pattern in dict.fromkeys(filters[name]):
        if pattern.startswith("!") or pattern in set(committed):
            continue
        regex = wp.glob_regex(pattern)
        hit = next((path for path in under if regex.fullmatch(path)), None)
        if hit:
            problems.append(f"::error file={engine.WORKFLOW}::filter {name} has '{pattern}' outside its listed block, "
                            f"and it matches {hit} under {tree}: which files of {tree} the filter selects is the "
                            f"block's to say, from the tsc programs of filter {source}, so drop the pattern and run "
                            f"`{MARKS.write}`")
    if source != name:
        covers = wp.PathsFilter(filters[name], quantifiers[name]).covers
        for path in [path for path in files if not covers(path)][:engine.SHOWN]:
            problems.append(f"::error file={path}::filter {name} does not select {path}, which the tsc programs of "
                            f"filter {source} compile ({why}): the filter must select it, or a change to it starts "
                            f"no job")
    print(f"filter {name}: {len(files)} listed file(s) under {tree}, from filter {source}'s programs")

for problem in problems:
    print(problem, file=sys.stderr)
if problems:
    print(f"::error::{len(problems)} problem(s) across {len(PROGRAMS)} tsc program(s) and {len(LISTED)} listed "
          f"block(s)", file=sys.stderr)
    raise SystemExit(1)
print(f"every changes filter selects every file its job's typecheck compiles "
      f"({len(PROGRAMS)} programs, {len({name for name, _, _ in PROGRAMS})} filters), and every listed block holds "
      f"exactly the files its programs compile ({len(LISTED)} blocks)")
PY
