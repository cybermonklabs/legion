#!/usr/bin/env bash
# `.github/workflows/ci.yaml#changes` job selects each job through a dorny/paths-filter filter, and a change to a
# file a job reads but its filter misses skips that job, on the pull request and again on the
# merge group that lands it, after which nothing runs it. The changes job computes every filter
# from what ci_job_reads.py says its job reads (`check-ci-filters.sh generate`); for a Go job that
# set is derived from `go list`, so an import cannot slip past it, while for a Bun job the declared
# reads are hand-kept in its file of .github/ci-gates, and the filter goes short with them.
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
# This check lists with tsc, the compiler every typecheck job runs, and the changes job's
# generation lists with tsgo, the native compiler pinned in the root package.json, which it runs
# for speed (.github/scripts/ci_ts_listing.py#COMPILER). So this check is also the cross-check of
# that compiler: a listed block the generation wrote from tsgo's answer that is not exactly the
# files tsc lists fails here, on the pull request whose tree makes the two differ, naming the file,
# as a bump of the pinned tsgo that changes a listing does.
#
# The pairs cover every gate whose job runs a typecheck, each read from that job's own steps:
# contracts, envoy_client (packages/envoy-client and packages/cipher-client), pi_shared,
# pi_envoy, pi_legion_build (packages/pi-legion, in the pi-legion-build job), claude_envoy,
# dispatch_ui (packages/dispatch and packages/proof-editor), docs (docs/site, in the
# docs-site.yaml job ci.yaml's docs job calls), conflicts_script (scripts/conflicts) and
# tooling_checks (scripts/dispatch, scripts/github-apps, scripts/sessions, scripts/approval-eval
# and scripts/e2e/lib, in the tooling-checks job).
# The gates left out are the ones whose job runs TypeScript but no typecheck, so there is no
# program to derive: `dispatch_browser` and `dispatch_browser_pr`, whose jobs are the browser
# suite itself, sharded and in two-process mode, in the workflow ci.yaml's dispatch-browser and
# dispatch-browser-pr jobs call (the dashboard's and the
# proof editor's typechecks are dispatch_ui's, and
# .github/ci-notes/ci.md#the-dashboards-and-the-proof-editors-lint-typecheck-unit-tests-build-and
# says why they were split out), `envoy_session_stall`,
# whose job builds and runs the stall entries, `envoy_e2e_local`, whose live proof renders
# with @legion/envoy-client at run time, and `dispatch_two_builds` and `dispatch_two_builds_go`,
# the two-build job's gates, whose job runs the browser suite against two builds and whose Go
# step runs this tree's dispatch command. What such a job loads is a runtime closure rather than
# a tsc program, and stays a hand-kept read of ci_job_reads.py: envoy_client_dep for the
# dispatch_browser, dispatch_browser_pr, envoy_e2e_local and two-build gates, a read of its own
# for the envoy_session_stall gate. Two parts of such closures this check does derive: every file
# the dispatch command the two-build Go step runs loads, from that command's bundle (the bundles,
# below), and the share of packages/contracts/src that the two browser gates and the two
# two-build gates load, from dispatch_ui's programs and that bundle (the listed blocks, below).
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
# that is gone by the time anyone reads it. The listing is .github/scripts/ci_ts_listing.py's,
# which the changes job's generation runs too.
#
# Files a test reads by relative path - the JSON fixtures and corpora a test of one package
# reads out of another package's directory - are part of no tsc program, so they stay hand-kept
# too: each is a declared read of its gate's file of .github/ci-gates, which the filter is
# computed from.
#
# THE FILTERS this check holds are the ones the run's changes job computed and its paths-filter
# step read: the file CI_FILTERS_FILE names, which ci.yaml's lint job fetches from that job (its
# `ci-filters` artifact). With no CI_FILTERS_FILE, this check computes them itself with
# `.github/scripts/check-ci-filters.sh generate`, which needs go and the workspace's node_modules,
# and is the changes job's whole computation: about a minute on a CI runner and several in a busy
# pod, so a local run that has a filters file at hand names it in CI_FILTERS_FILE.
# A file written because the filters could not be computed (its first line `# FAIL-OPEN:`) is
# refused: it selects every job, and holds nothing a program could be checked against. A file is
# held to a filter as workflow_paths.PathsFilter reads it, under the predicate-quantifier ci.yaml's
# paths-filter step declares (ci_filter_coverage.changes_quantifier): `some-with-excludes`, where a
# `!` pattern excludes, so a `!` pattern excluding a file of a program or a bundle is a refusal,
# since the job would then not run on a change that breaks its typecheck or the entry it runs.
#
# THE LISTED BLOCKS. A filter that names a whole tree selects its job for a change to any file of
# it, and where the job's programs compile only some of that tree - the Dispatch dashboard's two
# programs reached 42 of packages/contracts/src's 68 modules when this was written,
# packages/contracts/src/legion-api.ts not among them - the rest start a full run for a change
# no step of the job sees. So for each entry of ci_ts_programs.py's LISTED the changes job's
# generation writes into the entry's filter the files under the entry's tree that the programs
# PROGRAMS pairs with the entry's source filters compile or the bundles BUNDLES pairs with them
# load - the union, where an entry names more than one source - in sorted order, one pattern
# each, between a `# BEGIN listed` and a `# END listed` line (ci_filter_coverage.TS_LISTED). This
# check lists the same sources again, in the lint job, and refuses a block that lacks a file its
# sources give or holds one they do not, or holds them out of order or twice: the two jobs list one
# tree, so a difference is one of them listing it wrong. It refuses a block in a filter no entry
# names; a pattern of an entry's filter outside its block that matches a file under the tree,
# since which of those files the filter selects is the block's to say, and such a pattern is a
# read a gate declares over the tree; and a file of the block that only another filter's programs
# or bundles give, which the filter does not select. An entry's source is another filter where
# its job runs no program of its own and loads what that filter's programs compile or its bundles
# load. dispatch_browser and dispatch_browser_pr take the union of dispatch_ui's set and
# dispatch_two_builds_go's, the bundle of the dispatch command: their rows run the SPA and the e2e
# helpers that dispatch_ui's
# programs compile, and they spawn that command (`packages/dispatch/e2e/dispatch-cli.ts#dispatchEntry`).
# dispatch_two_builds takes the same union, for its browser half and for the command its Go step
# runs. LISTED says why for each. check-ci-filters.sh's rule 15 reads a block's files as what the
# tag step its filter gates loads: it holds the filter of the job that runs that package's
# untagged tests to them, envoy_go to dispatch_two_builds_go's block.
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
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# A bare call checks, which is the one thing this check does: the listed blocks are written by the
# changes job's generation, never by a hand or by this check.
case "$#:${1:-}" in
  0: | 1:check) ;;
  *)
    echo "::error file=.github/scripts/check-ts-filters.sh::this check takes one optional mode: \`check\` (the default), which holds the changes job's filters to the files their programs compile and their bundles load; the listed blocks are written by \`.github/scripts/check-ci-filters.sh generate\`, never here. Run \`.github/scripts/check-ts-filters.sh\`." >&2
    exit 1
    ;;
esac

# The changes job's filters: the file the lint job hands this check, or one computed here.
if [ -z "${CI_FILTERS_FILE:-}" ]; then
  scratch=$(mktemp -d)
  trap 'rm -rf "$scratch"' EXIT
  if ! .github/scripts/check-ci-filters.sh generate "$scratch/ci-filters.yaml" >&2; then
    echo "::error file=.github/scripts/check-ts-filters.sh::no CI_FILTERS_FILE is given, and \`.github/scripts/check-ci-filters.sh generate\` could not compute the changes job's filters, so no filter can be held to a program" >&2
    exit 1
  fi
  export CI_FILTERS_FILE="$scratch/ci-filters.yaml"
fi

# The pairs (ci_ts_programs.py), the listing (ci_ts_listing.py), the path model (workflow_paths.py)
# and the engine this borrows the quantifier and the block reader from (ci_filter_coverage.py) sit
# beside this script in the repository it checks. PYTHONDONTWRITEBYTECODE keeps the imports from
# leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import os
import sys
from pathlib import Path

import ci_filter_coverage as engine
import ci_ts_listing as listing
import workflow_paths as wp
from ci_ts_programs import BUNDLES, LISTED, PROGRAMS

TABLE = ".github/scripts/ci_ts_programs.py"
MARKS = engine.TS_LISTED
GENERATED = os.environ["CI_FILTERS_FILE"]
# A refusal about the computed filters names no file: they are no file of the repository, and the
# message says which filter. The YAML reader and the block reader name them as GENERATED_FILE.
SHOWN = None
# Every pair of the two tables as (kind, filter, path, why). A refusal names one source of a kind
# with ONE's noun and verb, and a filter's set of them with ALL's.
PAIRS = listing.pairs()
ONE = {"program": ("tsc program", "compiles"), "bundle": ("bundle", "loads")}
ALL = {"program": ("tsc programs", "compile"), "bundle": ("bundles", "load")}

problems: list[engine.Annotation] = []


def read_filters() -> tuple:
    """The lines of the generated filters and the filters with the quantifier ci.yaml's paths-filter
    step reads them under. A file that is absent, refuses the open, holds no mapping or was written
    to fail open ends the run here, naming which."""
    try:
        text = Path(GENERATED).read_text()
    except (OSError, UnicodeError) as exc:
        print(engine.annotation_line(engine.error(SHOWN, f"cannot read the changes job's filters at {GENERATED}, the "
                                                         f"file CI_FILTERS_FILE names: {type(exc).__name__}: {exc}")),
              file=sys.stderr)
        raise SystemExit(1) from exc
    if text.startswith(engine.FAIL_OPEN_HEAD):
        print(engine.annotation_line(engine.error(SHOWN, f"the changes job could not compute its filters and selected "
                                                         f"every job: {text.splitlines()[0]}")), file=sys.stderr)
        raise SystemExit(1)
    try:
        loaded = engine.load_repository_yaml_text(engine.GENERATED_FILE, text)
        quantifier = engine.changes_quantifier(engine.load_repository_yaml(engine.REPO / engine.WORKFLOW) or {},
                                               problems)
    except engine.RepositoryInputError as exc:
        print(engine.annotation_line(exc.annotation()), file=sys.stderr)
        raise SystemExit(1) from exc
    if not isinstance(loaded, dict) or not loaded:
        print(engine.annotation_line(engine.error(SHOWN, "the generated filters hold no filter")), file=sys.stderr)
        raise SystemExit(1)
    filters = {str(name): engine.flatten(value) for name, value in loaded.items()}
    return text.splitlines(), filters, {name: quantifier or "some" for name in filters}


def kinds(source: str) -> list:
    """The kinds of pair the two tables give filter `source`, programs first."""
    return [kind for kind in ("program", "bundle") if any(k == kind and pair == source for k, pair, _, _ in PAIRS)]


def named(sources) -> str:
    """Where a listed block's files come from: `filter a's tsc programs and filter b's bundles`."""
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


lines, filters, quantifiers = read_filters()

# Each program is listed and each bundle built once, whichever pairs and listed blocks read it.
files_of: dict = {}
for kind, name, path, why in PAIRS:
    if (kind, path) not in files_of and name in filters and (engine.REPO / path).exists():
        files_of[kind, path] = listing.source_files(kind, path, problems)

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
                                           f"{kind} starts no job; declare it a read of the filter's gate"))
    if len(uncovered) > engine.SHOWN:
        problems.append(engine.error(SHOWN, f"filter {name} misses {len(uncovered) - engine.SHOWN} more file(s) of "
                                            f"{path}"))

blocks = engine.filter_blocks(lines, problems, MARKS, engine.GENERATED_FILE)
for name in sorted(blocks):
    if blocks[name].begin >= 0 and name not in wanted:
        problems.append(engine.error(SHOWN, f"{name} has a listed block, and LISTED of {TABLE} names no such filter, "
                                            f"so nothing says what belongs in it"))
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
    differs = (f"the changes job computed the block from {named(sources)}, as this check lists them, so the two "
               f"jobs listed one tree differently")
    for path in sorted(set(files) - set(committed)):
        problems.append(engine.error(SHOWN, f"{where} lacks '{path}', which {give(gives[path])} ({why}): {differs}"))
    for path in sorted(set(committed) - set(files)):
        problems.append(engine.error(SHOWN, f"{where} has '{path}', which {give_none(sources)}, so it selects the "
                                            f"job for a change no step of it sees: {differs}"))
    if set(committed) == set(files) and committed != files:
        problems.append(engine.error(SHOWN, f"{where} holds its files out of sorted order or twice, and the same "
                                            f"sources must write the same text on every run"))
    under = wp.walk(Path(tree)) if (engine.REPO / tree).is_dir() else []
    for pattern in dict.fromkeys(filters[name]):
        if pattern.startswith("!") or pattern in set(committed):
            continue
        regex = wp.glob_regex(pattern)
        hit = next((path for path in under if regex.fullmatch(path)), None)
        if hit:
            problems.append(engine.error(SHOWN, f"filter {name} has '{pattern}' outside its listed block, and it "
                                                f"matches {hit} under {tree}: which files of {tree} the filter "
                                                f"selects is the block's to say, from {named(sources)}, so drop the "
                                                f"read of the filter's gate that names them"))
    # A file that only another filter's pairs give is held to this filter here; one this filter's
    # own pairs give is held to it by the loop over the pairs above.
    foreign = [path for path in files if all(source != name for source, _ in gives[path])]
    if foreign:
        covers = wp.PathsFilter(filters[name], quantifiers[name]).covers
        for path in [path for path in foreign if not covers(path)][:engine.SHOWN]:
            problems.append(engine.error(path, f"filter {name} does not select {path}, which {give(gives[path])} "
                                               f"({why}): the filter must select it, or a change to it starts no job"))
    print(f"filter {name}: {len(files)} listed file(s) under {tree}, from {named(sources)}")

# Every problem, this check's own and the engine's (changes_quantifier and filter_blocks append
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
