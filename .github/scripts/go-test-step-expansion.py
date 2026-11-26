#!/usr/bin/env python3
"""The test functions each `go test` step of ci.yaml's router runs, which two run and which none.

This is the tool check k1 of dispatch://INFRA-1408 asks for (its A1): it expands every `go test`
step of `.github/workflows/ci.yaml`, and of every workflow a job of it calls, whose working
directory is `packages/envoy`, `packages/envoy/infra` or `packages/daemon` into the test
functions the step really runs - through the step's own packages, `-tags`, `-race`, `-run` and
`-skip`, with `go test -list` - and prints two verdicts over them:

  * every (package, function) pair more than one step runs;
  * every pair some expanded step compiles and no expanded step runs: the package is in that
    step's resolved package list, so the step's test binary holds the function, and no step runs
    it. A `-skip` naming a test no other step's `-run` covers leaves exactly that behind, and
    nothing else here looks for it.

The second verdict is over the packages the expansion already touches, never over every test of
the three modules. A package no step lists at all is outside both verdicts: a tag-gated live
proof no job runs would read as unrun, which is true and tells a reader nothing. A test that is
unrun because its package is only ever listed under a `-run` that leaves it out is the ordinary
case this prints.

It exits 1 when either verdict holds a pair nothing accounts for, and 0 when every pair two
steps run is declared with the requirement that makes it run twice (EXCEPTIONS below) and no
pair is unrun. EXCEPTIONS covers the first verdict alone: an entry is keyed on exactly the steps
that run the pair, and its requirement answers what makes it run twice, so neither field has an
answer to give about a pair no step runs. An unrun pair is a hole to close where it is - give
the function to some step's `-run`, or take its package off the step that only builds it - and
the tree held none of them when this verdict was written.

No CI job runs it, and none should. It compiles every test binary of all three modules, the
`-race` ones included. Measured in a Legion pod on a warm module and build cache, the daemon
module alone costs about six minutes of that: `go test -count=1 -list '.*' ./...` over
packages/daemon takes 135 s and prints 4,644 names, and the race listing over
`./internal/daemon/ ./internal/sessionproxy/ ./internal/launcher/` takes 234 s and prints 933.
Five whole runs in that pod took 648 s, 402 s, 387 s, 461 s and 430 s, and a cold cache costs
tens of minutes, for an answer a person needs twice per change (at the head and at the merge
base) rather than once per run. The checks it serves run by hand, before the merge.

Between those hand runs CI holds the plugin-pack part of the property itself. The plugin-pack
step's `-run` and the daemon-go race step's `-skip` are both built from one list,
PLUGIN_PACK_TESTS in ci_job_reads.py, and check-ci-filters.sh pins each step's whole script, so an
edit to either step's text is refused (a step whose script is not its pinned text fails, naming
the step and the first line that differs, and on that line each test its -run or -skip names and
the pin does not, or the other way round) and the two cannot come to name different tests.
ci_job_reads.py's hold_plugin_pack_tests() refuses, at import, a name in that list that the
package no longer declares and a test of agentmodels_omp_test.go the list does not name. And
go-test-step-expansion.test.sh runs this script's step reader, with no `go` command, over every
`go test` step of the head, so a step this script would refuse fails the pull request that makes
it unreadable rather than the next hand run. What CI does not see is what this script decides: a
(package, function) pair two steps of one run run without a declared reason, and one some step
compiles while no step runs it.

It starts no service and needs no Postgres, no NATS, no Docker and no Pulumi: it never runs a
test. `go test -list` builds the test binary and asks it for its names, and each `TestMain` of the
packages here returns before it starts its database, NATS server, container or Pulumi stack when
the run is a listing.

`-race` needs cgo, so run it with `CGO_ENABLED=1` and a C compiler on PATH; a race step otherwise
fails to build and this script stops on that step.

Run it at two commits and compare, which is what k1 asks for - the head in your workspace, the
merge base in a clone under a temporary directory:

    store="$(cd "$WORKSPACE/.jj" && realpath "$(cat repo)")/store"
    cd "$WORKSPACE"
    python3 .github/scripts/go-test-step-expansion.py > "$TMPDIR/head.txt"; echo "head: $?"
    git clone --no-local "$(cd "$store" && realpath "$(cat git_target)")" "$TMPDIR/base"
    git -C "$TMPDIR/base" checkout <merge base>
    cp .github/scripts/go-test-step-expansion.py "$TMPDIR/base/.github/scripts/"
    (cd "$TMPDIR/base" && python3 .github/scripts/go-test-step-expansion.py) > "$TMPDIR/base.txt"
    echo "base: $?"
    grep '^PAIR' "$TMPDIR/head.txt" > "$TMPDIR/head.pairs"
    grep '^PAIR' "$TMPDIR/base.txt" > "$TMPDIR/base.pairs"
    diff "$TMPDIR/base.pairs" "$TMPDIR/head.pairs"   # empty: the change loses no test

It reads the repository root off its own path, so the copy under the temporary directory checks
itself and not the workspace it was copied from, and it reads a step's directory and its
comment-free lines with `ci_filter_coverage`'s own helpers, which sit beside it in both trees.
Each job of ci.yaml holds its steps in a `ci-job-<id>.yaml` of its own, so the set of documents
it reads is ci.yaml and every workflow a job of it calls, which it derives with that module's
`called_workflows` (`documents()` below says why it loads them itself).
Keep `GOMODCACHE` and `GOCACHE` the same for both runs: the second run then reuses the first
run's compile.

How it reads a step:

  * the working directory is the step's own `working-directory`, else the job's, else the
    workflow's `defaults.run.working-directory`, else the repository root, as GitHub reads it and
    as check-ci-filters.sh reads it (`ci_filter_coverage.step_workdir`);
  * a `go vet`, `go build`, `go list` or `go install` line runs no test and is skipped; a line it
    cannot read stops the run, since silently skipping a `go test` would hide both a duplicate
    and a function that step is the only one to compile;
  * `-run "$pattern"` assigned from a `$(…)` call of `.github/scripts/go-tagged-tests.sh` is run,
    in the step's own directory, and its output used; a step looping over packages
    (`for pkg in ./core ./seed ./gcp`) is one `go test` per package, each with its own call;
  * a `-skip '^(Name|Name|…)$'` names tests the step leaves out, which are subtracted from what
    the step runs and left in what it compiles, since the binary holds a skipped test and never
    runs it; every other `-skip` pattern is refused rather than read (SKIP_ALTERNATION says why);
  * what a step compiles is a second `go test -list` over the same packages under the same build
    flags and the pattern `.*`, run where the step carries a `-run`, because `-list` answers the
    pattern it is given and a `-run` listing names only the functions the step runs. The second
    listing reuses the first's build, so it costs the test binaries' own run and no compile;
  * `go list … | grep -Ev '…' | xargs go test …` is run as written, in the step's own directory,
    and the packages it prints are the command's packages;
  * a line that sets variables before its command (`GODEBUG=checkfinalizers=1 go test …`) or runs
    it under one of the wrappers GO_WRAPPERS names (`race_memory.py go test …`) is the go command
    behind them: neither changes which test functions run. A value holding `$` and a wrapper that
    tuple does not name are refused rather than guessed at;
  * a step of a matrix job that writes `${{ matrix.<key> }}` in its run: is one step per row of
    the job's matrix, each with the row's value written in, since each row is a job of its own:
    a legs job's go test step (`go_legs.py … ${{ matrix.leg }} ${{ strategy.job-total }} |
    xargs go test …`) runs each leg's packages in that leg alone, and `${{ strategy.job-total }}`
    is the number of rows. A matrix of lists is read, and nothing else: an
    `include`, an `exclude` or a matrix an expression computes is refused, as is a key no row
    has;
  * a step whose run: is the path of a script AUDITED_SCRIPTS names
    (`packages/envoy/scripts/database-skips.sh` and `packages/envoy/scripts/backend-count.sh`), alone
    or followed by one of the argument lists
    its entry reads, is read as the `go test` command that entry gives for those arguments, in
    the module it names. A step whose run: holds that script's file name anywhere else, or with
    arguments its entry does not list, is refused, since each entry reads its script only as
    called with what it lists; the test is on the run's raw text, so no quoting, substitution or
    comment hides the script from it. A
    name the shell builds at run time (from a variable, or quoted in pieces) is not in the text,
    and no reading of the text sees it; This script reads no shell script itself: what a script runs
    depends on its control flow, which a line-by-line reading cannot see (a `go test` under
    `if false`, a `cd` in a branch, a test run inside `$(…)`), so each entry is a person's reading
    of the script, pinned by the script's SHA-256. An edit to the script makes the entry stale,
    and this script refuses the step, naming the file, until someone reads the script again and
    updates the entry. A step that runs any other script is read as before, from its own run:
    alone, so a `go test` inside that script is not seen: a step that runs a script from the
    repository root and holds no `go test` of its own is skipped, which is how envoy-go's
    "Count the store-form rows on each backend" (backend-count.sh) was read as running nothing
    until its entry below, and its five tests' second run was in neither verdict;
  * a `Benchmark…` name a listing prints is left out of both verdicts: no step passes `-bench`,
    so `go test` builds those functions and runs none of them, and reading every one of them as
    compiled and unrun would say that over and over and name no hole anyone can close.

What it prints: the steps it expanded, the pairs more than one step runs, the pairs some step
compiles and no step runs, the declared exceptions it applied and the ones it did not need, and
every pair a step runs, one per line behind a `PAIR` marker for the diff above. Exit 0 when no
pair is run twice undeclared and none is unrun, 1 when one is, 2 when it cannot read a step or a
`go` command fails.
"""

from __future__ import annotations

import hashlib
import itertools
import re
import shlex
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

import yaml
from ci_filter_coverage import called_workflows, script_lines, step_workdir

ROOT = Path(__file__).resolve().parents[2]
# Repository-relative, as the documents below are keyed and as every message names them.
WORKFLOW = ".github/workflows/ci.yaml"
# The three Go modules this check covers. packages/daemon joined them with the first pair of
# steps in this repository that share a package through a `-run` and a `-skip` naming the same
# three tests (ci-job-plugin-pack.yaml and ci-job-daemon-go.yaml's race step): the pairing is
# what makes each of the three run exactly once, and a check that cannot see both steps cannot
# see it.
MODULE_DIRS = ("packages/envoy", "packages/envoy/infra", "packages/daemon")


@dataclass(frozen=True)
class Exception_:
    """A pair two steps may run, and the requirement that makes it run twice.

    `steps` must be exactly the steps that run it, each named as `Step.key` names it - the
    repository-relative workflow file that holds the step, and the step's `name:` - `package` its
    import path, `functions` the function names (None: every function of that package those steps
    share). An entry whose `requirement` is empty is refused: a duplicate with no requirement
    written beside it is a duplicate, not an exception.

    Nothing here declares an unrun pair. Both fields above ask after the steps that run the
    pair, and no step runs that one; the answer to a function a step compiles and none runs is a
    change to the step, not a line here.
    """

    steps: frozenset[tuple[str, str]]
    package: str
    functions: tuple[str, ...] | None
    requirement: str


EXCEPTIONS = (
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-smoke.yaml", "Live tests of envoy-infra's host:Listener"),
            (".github/workflows/ci-job-envoy-infra-go.yaml", "Unit test envoy-infra"),
        }),
        package="github.com/sjawhar/envoy/infra/host",
        functions=None,
        requirement=(
            "R3 (dispatch://INFRA-1408/spec): envoy-smoke's pulumilive step over infra/host keeps "
            "running the whole package, untagged tests included, because envoy-smoke runs on a "
            "Dispatch-only change where envoy-infra-build does not, and in such a run those untagged "
            "tests would otherwise run nowhere. A -run of the tagged tests alone would make "
            "check-ci-filters.sh's rule 15 hold envoy_infra_build's filter to every file envoy-smoke "
            "reads, the listener image's inputs among them, and so start envoy-infra-build on each "
            "change to the image to spare the twenty untagged tests' 0.17 s in envoy-smoke (job "
            "137289139653)."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-go.yaml", "Test what the race build cannot run at all or at full size"),
            (".github/workflows/ci-job-envoy-go-race-docs.yaml", "Race-test the document service"),
        }),
        package="github.com/sjawhar/envoy/internal/dispatch/docs",
        functions=(
            "TestRenderReuseARandomisedEditStorm",
            "TestRenderReuseAReplicaReplaysTheRoomsFormattingExactly",
        ),
        requirement=(
            "R2 (dispatch://INFRA-1408/spec): the two render-reuse storms run fewer seeds and "
            "fewer edits under the race detector (raceEnabled), so envoy-go's plain step is their "
            "full-size run and envoy-go-race-docs is the detector's run of the shrunk one."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-go.yaml", "Test what the race build cannot run at all or at full size"),
            (".github/workflows/ci-job-envoy-go-race.yaml", "Race-test delivery, the store, the bus, the listener, the registry store, the session cache, kvwatch, the NATS credentials, the NATS auth service and the sign-in code flow"),
            (".github/workflows/ci-job-envoy-go.yaml", "Unit test envoy Go package"),
        }),
        package="github.com/sjawhar/envoy/internal/cipher/natscallout",
        functions=("TestACalloutStormAfterAServerRestartAdmitsEveryConnection",),
        requirement=(
            "c-callout-available (the NATS auth service's verification): five hundred "
            "connections reconnect at once after a server restart and every one is admitted. The "
            "restart storm opens two hundred under the race detector (raceEnabled), whose cost per "
            "in-process connect fails the setup at five hundred on a loaded runner, so envoy-go's "
            "plain step is its full-size run and envoy-go-race the detector's run of the smaller one. "
            "The unit step runs the package whole, for the reason the next entry gives."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-go-race.yaml", "Race-test delivery, the store, the bus, the listener, the registry store, the session cache, kvwatch, the NATS credentials, the NATS auth service and the sign-in code flow"),
            (".github/workflows/ci-job-envoy-go.yaml", "Unit test envoy Go package"),
        }),
        package="github.com/sjawhar/envoy/internal/cipher/natscallout",
        functions=None,
        requirement=(
            "The writer hold's rows of the NATS auth service read the broker's hold row from "
            "Postgres and skip without BROKER_TEST_DATABASE_URL, and envoy-go-race's step runs "
            "without -v, where a skip prints nothing. envoy-go's step \"Unit test envoy Go package\" "
            "runs the package through packages/envoy/scripts/database-skips.sh, "
            "which counts that run's skips and fails on any, so a hold row that stopped running "
            "fails the pull request; envoy-go-race keeps the package's race run. The count takes "
            "the package whole, since go test -json of a -run would leave a skip it did not select "
            "uncounted."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-dispatch-two-builds.yaml", "Two-build tests of the Dispatch server"),
            (".github/workflows/ci-job-dispatch-two-builds.yaml", "Two-build tests of the Dispatch server against its own build"),
        }),
        package="github.com/sjawhar/envoy/cmd/dispatch",
        functions=None,
        requirement=(
            "R1 (dispatch://INFRA-4330/spec): in a merge-group run, the twobuild suite runs twice "
            "on purpose, against the newest release's build (dispatch-two-builds) and against the "
            "tree's own build (dispatch-two-builds-same), so a row whose assertion holds only "
            "across a version boundary fails in the merge group of the change that breaks it. The "
            "two runs answer different questions, and the second runs in no pull-request or "
            "dispatched run."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-airlock-lean.yaml", "Compare the Go airlock with the model over the seed corpus"),
            (".github/workflows/ci-job-airlock-lean.yaml", "Fuzz the Go airlock against the model"),
            (".github/workflows/ci-job-envoy-go.yaml", "Unit test envoy Go package"),
        }),
        package="github.com/sjawhar/envoy/internal/cipher/proxyrules",
        functions=("FuzzAirlock",),
        requirement=(
            "R3 (dispatch://INFRA-1408/spec): airlock-lean holds the Go airlock to its Lean model, "
            "and its filter takes files envoy_go's does not - its own job file, "
            ".github/actions/install-elan/** and packages/envoy/scripts/airlock-lean-axioms.sh - "
            "so on such a change envoy-go does not run and the seed corpus would run nowhere. Its "
            "own two steps are the corpus comparison and the 120-second fuzz, which seeds from "
            "that same corpus, so both name the function. envoy-go's step \"Unit test envoy Go "
            "package\" runs every broker package through "
            "packages/envoy/scripts/database-skips.sh, so a listing counts the function run there "
            "too, but it skips there for want of AIRLOCK_MODEL, as that script's EXPECTED_SKIPS "
            "records, so the corpus runs only in airlock-lean."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-airlock-lean.yaml", "Compare the Go airlock with the model over the seed corpus"),
            (".github/workflows/ci-job-envoy-go.yaml", "Unit test envoy Go package"),
        }),
        package="github.com/sjawhar/envoy/internal/cipher/proxyrules",
        functions=("TestModelAgreesOnHostShapes",),
        requirement=(
            "The same reason as FuzzAirlock above: airlock-lean's corpus comparison runs the host "
            "shapes a rules file may hold through both the Go validation and the Lean model's, "
            "with AIRLOCK_MODEL set, and runs on changes that do not start envoy-go. envoy-go's "
            "step \"Unit test envoy Go package\" runs every broker package through "
            "packages/envoy/scripts/database-skips.sh, so a listing counts "
            "the function run there too, but it skips there for want of AIRLOCK_MODEL, as that "
            "script's EXPECTED_SKIPS records, so the comparison runs only in airlock-lean. The fuzz "
            "step runs FuzzAirlock alone and does not name it."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-airlock-lean.yaml", "Compare the Go airlock with the model over the seed corpus"),
            (".github/workflows/ci-job-envoy-go.yaml", "Unit test envoy Go package"),
        }),
        package="github.com/sjawhar/envoy/internal/cipher/proxyrules",
        functions=("TestTheModelRefusesAHostlessEntry",),
        requirement=(
            "The same reason as TestModelAgreesOnHostShapes above: airlock-lean's corpus "
            "comparison runs a host-less entry carrying guidance, a field the generator never "
            "writes, through both the Go validation and the Lean model's, which each refuse it, "
            "with AIRLOCK_MODEL set. envoy-go's step \"Unit test envoy Go package\" "
            "lists it too, and it skips there for want of AIRLOCK_MODEL, as that script's "
            "EXPECTED_SKIPS records, so the comparison runs only in airlock-lean."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-go.yaml", "Unit test envoy Go package"),
            (".github/workflows/ci-job-envoy-go-build.yaml", "Test pmdoc's tables in a build without inlining"),
        }),
        package="github.com/sjawhar/envoy/internal/dispatch/pmdoc",
        functions=None,
        requirement=(
            "pmdoc's readers must read a table in every build, whatever its inlining. "
            "envoy-go-build runs pmdoc's table tests and its display fixture test in a build made "
            "with -gcflags=all=-l, a mode no other step builds pmdoc in: goldmark builds pmdoc's "
            "readers through an interface Go's initialization order does not look through, so a "
            "reader that copied a variable of pmdoc was handed nil in that build alone, and each "
            "table failed to read there while it passed in envoy-go's unit step. The two runs "
            "answer different questions."
        ),
    ),
)

# `go test -list` prints one name per line, then a status line per package.
NAME = re.compile(r"^(Test|Fuzz|Example|Benchmark)[A-Za-z0-9_]*$")
STATUS = re.compile(r"^(ok|FAIL|\?)\s+(\S+)")
# A shell variable, as the steps write it: $pkg or ${pkg}.
SHELL_VAR = re.compile(r"\$(?:([A-Za-z_][A-Za-z0-9_]*)|\{([A-Za-z_][A-Za-z0-9_]*)\})")
FOR_LINE = re.compile(r"^for\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\s+(.+?)\s*;\s*do$")
ASSIGNMENT = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)=\$\((.+)\)$")
XARGS = re.compile(r"^(?P<lister>.+?)\s*\|\s*xargs\s+(?P<test>go\s+test\b.*)$")
# A matrix value as a run: writes it, which GitHub replaces before the shell reads the line.
MATRIX_VALUE = re.compile(r"\$\{\{\s*matrix\.([A-Za-z_][A-Za-z0-9_-]*)\s*\}\}")
# How many jobs the matrix makes, as a run: writes it (a legs step hands it to go_legs.py).
JOB_TOTAL = re.compile(r"\$\{\{\s*strategy\.job-total\s*\}\}")
TAGGED_TESTS = ".github/scripts/go-tagged-tests.sh"


@dataclass(frozen=True)
class AuditedScript:
    """A script a step runs, read by a person: the module it moves into before its tests, the one
    `go test` it runs there for each argument list a step passes it (the run:'s text after the
    script's path, "" for none), and the SHA-256 of the text that reading was of."""

    module: str
    commands: dict[str, str]
    sha256: str


# The tests envoy-go's "Count the store-form rows on each backend" names, in the order its run:
# passes them to backend-count.sh; ci_job_reads.BACKEND_COUNT_TESTS, which the unit step's skip of
# them is built from, holds the same names by package.
BACKEND_COUNT_TESTS = (
    "TestEverySecretFormNamesTheSourceItUses",
    "TestNoSecretFormAsksTheBrokerForACredential",
    "TestUseCommandOutsideASessionReachesTheMachineSource",
    "TestTheChainSkipsAbsenceAndStopsOnABrokenSource",
    "TestSettingsNameTheGoogleExchangeAudienceOnEitherStore",
)

# envoy-go's unit step's -skip and --module-except, as ci_job_reads.UNIT_TEST_RUN writes them: the
# web display fixture's two tests and BACKEND_COUNT_TESTS, and the race lane's packages but those
# whose skips the step counts, the text checks, internal/integration and the packages
# envoy-go-build runs.
UNIT_SKIP = ("'^(TestEverySecretFormNamesTheSourceItUses|TestNoSecretFormAsksTheBrokerForACredential|"
             "TestSettingsNameTheGoogleExchangeAudienceOnEitherStore|TestTheChainSkipsAbsenceAndStopsOnABrokenSource|"
             "TestTheWebFixtureFindsItsFilesFromAnyDirectory|TestUseCommandOutsideASessionReachesTheMachineSource|"
             "TestWebDisplayFixture)$'")
UNIT_EXCEPT = ("'/integration|/internal/textcheck$|/internal/dispatch/architecture/notes$|/internal/runnerlog$|"
               "/internal/dispatch/specgraph$|/cmd/architecture-check$|/cmd/spec-check$|/listenerwire$|"
               "/internal/wirecheck$|/internal/dispatch/delivery($|/)|/internal/dispatch/store($|/)|/internal/bus$|"
               "/cmd/listener$|/internal/store$|/internal/session$|/internal/kvwatch($|/)|/natscred$|"
               "/internal/oidc$|/internal/dispatch/api$|/internal/dispatch/docs$'")

# Each script a step of ci.yaml's router runs as its whole run: line, read by a person, with the
# one `go test` it runs for each argument list a step passes it.
AUDITED_SCRIPTS = {
    # It moves into packages/envoy (`cd "$(dirname "$0")/.."`) and runs one `go test -json
    # -count=1` over its packages, then counts the skips in its report. With no package argument it
    # counts its DEFAULT_PACKAGES, ./internal/cipher/..., ./cmd/cipherd/ and ./internal/holdrecord/,
    # less the import paths its DEFAULT_EXCEPT takes out of `go list` of them with `grep -Ev`, none
    # today since that pattern is empty (ci_job_reads.DATABASE_SKIPS_PATTERN, built from
    # DATABASE_SKIPS_RACE_ONLY), with no -tags, -race, -run or -skip, under -timeout
    # "${GO_TEST_TIMEOUT:-15m}". A package argument replaces those packages, `--race` adds -race,
    # `--skip P` adds -skip P, `--timeout D` sets the -timeout, and `--var NAME` decides only
    # whether it runs at all. `--module-except P` adds to the run every package of
    # `go list ./... | grep -Ev P`, in that order, and then each counted package P took out; P
    # keeps every counted package here (ci_job_reads.hold_counted_in_unit_run), so the run is the
    # listing alone. With `--report FILE` it reads a report and runs no test. envoy-go's unit step
    # passes the first argument list below, whose command is that run in the listing form this
    # script reads, and envoy-go-race-api's race step the second.
    "packages/envoy/scripts/database-skips.sh": AuditedScript(
        module="packages/envoy",
        commands={
            f"--timeout 20m --skip {UNIT_SKIP} --module-except {UNIT_EXCEPT}":
                f"go list ./... | grep -Ev {UNIT_EXCEPT} | xargs go test -json -count=1 -skip {UNIT_SKIP} -timeout 20m",
            "--var DISPATCH_TEST_DATABASE_URL --var BROKER_TEST_DATABASE_URL --race --timeout 70m ./internal/dispatch/api/":
                "go test -json -count=1 -race -timeout 70m ./internal/dispatch/api/",
        },
        sha256="4bdb145f86204a40263a655be0f8e49500f47ec17e7caffc63ef7f2672ef86a2",
    ),
    # It moves into packages/envoy (`cd "$(dirname "$0")/.."`) and runs one `go test -json
    # -count=1 -timeout "${GO_TEST_TIMEOUT:-5m}" -run '^(TEST|…)$'` over the packages before its
    # `--`, the -run built from the test names after it, then counts each test's `aws` and `gcp`
    # passes. GO_TEST_SKIP adds a -skip, which no step sets, and envoy-go's job sets neither
    # variable. envoy-go's count step passes the argument list below.
    "packages/envoy/scripts/backend-count.sh": AuditedScript(
        module="packages/envoy",
        commands={
            "./cmd/cipher/ ./internal/cipher/api/ -- " + " ".join(BACKEND_COUNT_TESTS):
                f"go test -json -count=1 -timeout 5m -run '^({'|'.join(BACKEND_COUNT_TESTS)})$' "
                "./cmd/cipher/ ./internal/cipher/api/",
        },
        sha256="bea62eaf32791a70c86e0350053bda35bae7dfa81685ad3338022849f0587a0e",
    ),
    # It moves into packages/envoy/infra (`cd "$(dirname "$0")/.."`) and, given `run`, runs one
    # `go test -count=1 -race` over the packages `go list ./...` prints less those its EXCLUDE,
    # '/internal/textcheck$' (ci_job_reads.INFRA_PLAIN_PATTERN), matches, with no -tags, -run or
    # -skip. envoy-infra-build's unit step passes `run`. Until this entry the reader skipped that
    # step, so neither verdict saw the module's race run: every untagged test of infra/core,
    # infra/seed and infra/gcp read as compiled by the natslive step and run by no step, and
    # infra/host's untagged tests, which envoy-smoke's pulumilive step runs plain, as run once.
    "packages/envoy/infra/scripts/race-unit.sh": AuditedScript(
        module="packages/envoy/infra",
        commands={
            "run": "go list ./... | grep -Ev '/internal/textcheck$' | xargs go test -count=1 -race",
        },
        sha256="3e9946d3482859bdab444ee99341955f982e70e86cb6e93c2138040cd015753b",
    ),
}
# A variable a step sets before its command, as the steps write it: GODEBUG=checkfinalizers=1.
# A value holding `$` is left for the caller to refuse, since what it expands to is the shell's.
ENV_PREFIX = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=[^$]*$")
# The wrappers a step runs a go command under. Each bounds how the test binary runs and changes
# which tests it runs in no way, so the command behind it is read as though it stood alone; one
# this tuple does not name is refused rather than guessed at. Matched by the end of the word,
# since a step writes the path from its own working directory.
GO_WRAPPERS = (".github/scripts/race_memory.py",)
# go commands that run no test, so a step may hold one and this script need not read it.
NO_TEST_GO = ("build", "vet", "list", "install", "mod", "work", "env", "generate", "tool", "run")
# go test flags whose value is the next argument.
VALUE_FLAGS = ("-run", "-skip", "-tags", "-timeout", "-count", "-parallel", "-cpu", "-bench",
               "-o", "-fuzz", "-fuzztime", "-shuffle", "-exec", "-coverprofile", "-covermode",
               "-p")
# The one `-skip` shape this script reads: the anchored alternation of whole test names
# ci_job_reads.py builds for both steps of this repository that carry one, `^(Name|Name|…)$`.
# Its names are read off the pattern's own text, and every other pattern is refused by name,
# because `go test` runs the real `-skip` through Go's regexp: a Python `re` matched against a
# listing would be a second engine, and two engines that agree on every pattern anyone wrote
# down disagree on the one nobody tried, leaving this script wrong about what a step runs.
SKIP_ALTERNATION = re.compile(r"^\^\(([^()|]+(?:\|[^()|]+)*)\)\$$")


class Refused(Exception):
    """A shape this script will not guess at."""


@dataclass
class Step:
    workflow: str  # repository-relative: the file that holds this step
    job: str
    name: str
    workdir: str  # repository-relative, as the file writes it
    script: str

    @property
    def label(self) -> str:
        return f"{self.workflow}'s {self.job} / {self.name}"

    # The file and the step's name, which is what an EXCEPTIONS entry declares: a step name is
    # unique within the file that holds it, while a job id is ci.yaml's and the name of the step
    # may be another file's too (release-envoy-infra.yaml has its own "Unit test envoy-infra").
    @property
    def key(self) -> tuple[str, str]:
        return (self.workflow, self.name)

    @property
    def directory(self) -> Path:
        return ROOT / self.workdir


@dataclass
class GoTest:
    """One `go test` command of one step."""

    step: Step
    text: str
    packages: list[str]
    tags: str = ""
    race: bool = False
    run: str = ""
    skip: str = ""  # the pattern as the step writes it; skip_names reads the names out of it
    resolved: list[str] = field(default_factory=list)  # import paths


def shell(script: str, cwd: Path, what: str) -> str:
    """A line of a step's own script, run in the step's own directory."""
    done = subprocess.run(["bash", "-c", f"set -euo pipefail\n{script}"],
                          cwd=cwd, capture_output=True, text=True)
    if done.returncode != 0:
        raise Refused(f"{what} exited {done.returncode}\n"
                      f"    $ {script}\n{indent(done.stderr or done.stdout)}")
    return done.stdout


def go(args: list[str], cwd: Path, what: str) -> str:
    done = subprocess.run(args, cwd=cwd, capture_output=True, text=True)
    if done.returncode != 0:
        hint = ("\n    -race needs CGO_ENABLED=1 and a C compiler."
                if "-race" in args and "cgo" in (done.stderr or "") else "")
        raise Refused(f"{what} exited {done.returncode}\n"
                      f"    $ {shlex.join(args)} (in {cwd}){hint}\n{indent(done.stderr)}")
    return done.stdout


def indent(text: str) -> str:
    return "".join(f"    | {line}\n" for line in (text or "").strip().splitlines())


def expand(text: str, variables: dict[str, str]) -> str:
    def one(match: re.Match[str]) -> str:
        name = match.group(1) or match.group(2)
        if name not in variables:
            raise Refused(f"the shell variable ${name} is set outside the step, or not at all")
        return variables[name]

    return SHELL_VAR.sub(one, text)


def documents() -> dict:
    """ci.yaml and every workflow a job of it calls, keyed by repository-relative path. A job of
    ci.yaml keeps its id, its `needs: changes` and its whole `if:` there and holds its steps in
    the `ci-job-<id>.yaml` its `uses:` names, so ci.yaml alone holds none of the `go test` steps
    below. The set is the one check-ci-filters.sh reads, derived with that check's own
    `called_workflows`; it is loaded here rather than through `ci_filter_coverage`'s
    `workflow_documents`, which roots itself at the current working directory and appends to the
    checker's problem list, because this script roots itself at its own path (the module header
    says why) and refuses instead."""
    found = {WORKFLOW: yaml.safe_load((ROOT / WORKFLOW).read_text()) or {}}
    for path, job_id in called_workflows(found[WORKFLOW]).items():
        if not (ROOT / path).is_file():
            raise Refused(f"job {job_id} of {WORKFLOW} calls {path}, which is not in the "
                          f"repository, so the steps it runs are read by nothing here")
        found[path] = yaml.safe_load((ROOT / path).read_text()) or {}
    return found


def steps_of(found: dict) -> list[Step]:
    """Every step of those documents whose run: holds a `go test` and whose directory is one of
    the three modules. The directory and the comment-free lines are read with
    check-ci-filters.sh's own helpers (ci_filter_coverage.step_workdir, script_lines), so this
    tool and the gate agree on what a step runs and where."""
    steps = []
    for path, document in found.items():
        for job, body in (document.get("jobs") or {}).items():
            for index, step in enumerate((body or {}).get("steps", []) or []):
                script = step.get("run")
                if not script:
                    continue
                workdir = step_workdir(document, body, step)
                name = step.get("name") or f"step {index + 1}"
                audited = audited_step(path, job, name, workdir, script)
                if audited is not None:
                    steps.append(audited)
                    continue
                if workdir not in MODULE_DIRS:
                    continue
                if not re.search(r"\bgo\s+test\b", "\n".join(script_lines(script))):
                    continue
                if not MATRIX_VALUE.search(script):
                    steps.append(Step(workflow=path, job=job, name=name, workdir=workdir, script=script))
                    continue
                rows = matrix_rows(f"{path}'s {job} / {name}", body)
                for row in rows:
                    def value(match: re.Match[str], row: dict = row) -> str:
                        if match.group(1) not in row:
                            raise Refused(f"{path}'s {job} / {name}: its run: writes matrix.{match.group(1)}, "
                                          f"which no row of the job's matrix has")
                        return str(row[match.group(1)])
                    label = ", ".join(f"{key} {row[key]}" for key in row)
                    steps.append(Step(workflow=path, job=job, name=f"{name} ({label})", workdir=workdir,
                                      script=JOB_TOTAL.sub(str(len(rows)), MATRIX_VALUE.sub(value, script))))
    return steps


def matrix_rows(label: str, body: dict) -> list[dict]:
    """The rows of a job's matrix of lists, each the values one of its jobs gets, in GitHub's
    order. Any other shape is refused rather than guessed at: an `include` or `exclude` changes the
    rows in ways this reading does not follow, and a matrix an expression computes is known only
    when the run starts."""
    matrix = ((body or {}).get("strategy") or {}).get("matrix")
    if not isinstance(matrix, dict) or not matrix:
        raise Refused(f"{label}: its run: writes a matrix value, and the job has no matrix of lists to read it from")
    if "include" in matrix or "exclude" in matrix or not all(isinstance(v, list) and v for v in matrix.values()):
        raise Refused(f"{label}: its job's matrix is not a matrix of lists alone, which is the one shape read here")
    keys = list(matrix)
    return [dict(zip(keys, values)) for values in itertools.product(*(matrix[key] for key in keys))]


def spellings(path: str, workdir: str) -> set[str]:
    """The ways a run: in workdir names path, a repository-relative file: its path from workdir
    behind `./`, and bare where that path holds a `/` (a bare name without one is looked up on
    PATH by the shell, not in the directory)."""
    if workdir in ("", "."):
        relative = path
    elif path.startswith(f"{workdir}/"):
        relative = path[len(workdir) + 1:]
    else:
        return set()
    return {f"./{relative}"} | ({relative} if "/" in relative else set())


def audited_step(workflow: str, job: str, name: str, workdir: str, script: str) -> Step | None:
    """The step a run: that is the path of an AUDITED_SCRIPTS script makes, alone or followed by
    an argument list its entry reads, read as that entry's command for those arguments in its
    module, or None where the run's text holds no such script's file name. A run whose text holds
    one anywhere else - an operator, another command, a `$(…)`, a comment, another line, an
    argument list the entry does not read - is refused rather than read or skipped: the test is
    on the raw text, before any shell reading, so no quoting or substitution hides the script
    from it. So is a script whose text no longer has its entry's SHA-256."""
    label = f"{workflow}'s {job} / {name}"
    named = [path for path in AUDITED_SCRIPTS if Path(path).name in script]
    if not named:
        return None
    # Only the shell's own blanks and the line's end: Python's strip() also takes Unicode spaces,
    # which bash reads as part of the name.
    alone = script.strip(" \t\n")
    # The path is the text up to the first space, and the arguments the text after it, compared
    # with each listed argument list as written: one space between words, nothing else.
    spelled, _, arguments = alone.partition(" ")
    # The literal spelling, compared with no path normalization: `path/` and `path/.` resolve to
    # the file in Python and name nothing the shell can run.
    matches = [path for path in named if spelled in spellings(path, workdir)]
    if (len(named) != 1 or len(matches) != 1 or any(c.isspace() for c in spelled)
            or arguments not in AUDITED_SCRIPTS[matches[0]].commands):
        raise Refused(f"{label}: names {', '.join(named)} in a form no AUDITED_SCRIPTS entry "
                      "reads: an entry reads a run: that is that one path, alone or followed by "
                      "an argument list the entry lists")
    path = matches[0]
    entry = AUDITED_SCRIPTS[path]
    digest = hashlib.sha256((ROOT / path).read_bytes()).hexdigest()
    if digest != entry.sha256:
        raise Refused(f"{label}: {path} has SHA-256 {digest}, and its AUDITED_SCRIPTS entry was "
                      f"read from {entry.sha256}; read the script again and update the entry's "
                      "commands and digest")
    return Step(workflow=workflow, job=job, name=name, workdir=entry.module, script=entry.commands[arguments])


def commands_of(step: Step) -> list[GoTest]:
    """Every `go test` a step's script starts, each with its packages, tags, -race and -run."""
    found: list[GoTest] = []
    read_block(step, step.script.splitlines(), {}, found)
    if not found:
        raise Refused(f"{step.label}: its run: holds `go test` and no command this script can read")
    return found


def read_block(step: Step, lines: list[str], variables: dict[str, str],
               found: list[GoTest]) -> None:
    index = 0
    while index < len(lines):
        line = lines[index].strip()
        index += 1
        if not line or line.startswith("#") or line.startswith("set "):
            continue

        loop = FOR_LINE.match(line)
        if loop:
            body, index = loop_body(step, lines, index)
            for value in shlex.split(expand(loop.group(2), variables)):
                read_block(step, body, {**variables, loop.group(1): value}, found)
            continue

        assignment = ASSIGNMENT.match(line)
        if assignment:
            name, inner = assignment.group(1), expand(assignment.group(2), variables)
            if TAGGED_TESTS not in inner:
                raise Refused(f"{step.label}: this script runs only a $(…) call of "
                              f"{TAGGED_TESTS}, not `{inner}`")
            variables[name] = shell(inner, step.directory, f"{step.label}: {inner}").strip()
            continue

        piped = XARGS.match(line)
        if piped:
            lister = expand(piped.group("lister"), variables)
            packages = shell(lister, step.directory, f"{step.label}: {lister}").split()
            if not packages:
                raise Refused(f"{step.label}: `{lister}` printed no package")
            found.append(parse_go_test(step, expand(piped.group("test"), variables), packages))
            continue

        words, command = go_command(line)
        if len(words) > 1 and words[0] == "go" and words[1] in NO_TEST_GO:
            continue
        if len(words) > 1 and words[0] == "go" and words[1] == "test":
            found.append(parse_go_test(step, expand(command, variables), []))
            continue
        raise Refused(f"{step.label}: this script cannot read `{line}`")


def go_command(line: str) -> tuple[list[str], str]:
    """The command a line runs, with the variables it sets before it and the wrapper it runs it
    under dropped: its words, and its text with those words removed.

    A step writes `GODEBUG=checkfinalizers=1 go test …` and
    `RACE_MEM_LIMIT_KB=8388608 ../../.github/scripts/race_memory.py go test …`. Neither the
    variables nor the wrapper changes which test functions the command runs, which is all this
    script reads, so both are dropped and what is left is the go command. A value holding `$`,
    and a wrapper GO_WRAPPERS does not name, are left where they are, so the caller refuses the
    line rather than guessing. The text is cut word by word off the line itself, so the quoting
    of what is left - a `-run` pattern above all - is the line's own.
    """
    rest = line.strip()
    while True:
        head = rest.split(None, 1)
        if len(head) == 2 and ENV_PREFIX.match(head[0]):
            rest = head[1]
            continue
        break
    head = rest.split(None, 1)
    if len(head) == 2 and head[0].endswith(GO_WRAPPERS) and head[1].split(None, 1)[0] == "go":
        rest = head[1]
    return rest.split(), rest


def loop_body(step: Step, lines: list[str], index: int) -> tuple[list[str], int]:
    body, depth = [], 1
    while index < len(lines):
        line = lines[index].strip()
        index += 1
        if FOR_LINE.match(line) or line.startswith("while ") or line.startswith("until "):
            depth += 1
        elif line == "done":
            depth -= 1
            if depth == 0:
                return body, index
        body.append(line)
    raise Refused(f"{step.label}: a `for` loop with no `done`")


def skip_names(pattern: str) -> list[str] | None:
    """The test names an anchored alternation leaves out, or None for a pattern of any other
    shape, which the caller refuses by name rather than guessing at."""
    match = SKIP_ALTERNATION.match(pattern)
    if not match:
        return None
    names = match.group(1).split("|")
    return names if all(NAME.match(name) for name in names) else None


def parse_go_test(step: Step, text: str, packages: list[str]) -> GoTest:
    """The packages, -tags, -race, -run and -skip of one `go test` command."""
    command = GoTest(step=step, text=text, packages=list(packages))
    words = shlex.split(text)
    position = words.index("test") + 1
    while position < len(words):
        word = words[position]
        position += 1
        if word == "-race":
            command.race = True
        elif word.startswith("-"):
            flag, _, value = word.partition("=")
            if not value and flag in VALUE_FLAGS:
                value = words[position]
                position += 1
            if flag == "-run":
                command.run = value
            elif flag == "-skip":
                if skip_names(value) is None:
                    raise Refused(f"{step.label}: `{text}` skips by `{value}`, and this script "
                                  "reads only the anchored alternation of whole test names, "
                                  "`^(Name|Name|…)$`, that ci_job_reads.py writes")
                command.skip = value
            elif flag == "-tags":
                command.tags = value
            elif flag == "-bench":
                raise Refused(f"{step.label}: `{text}` runs benchmarks, which this script "
                              "does not expand")
        else:
            command.packages.append(word)
    if not command.packages:
        raise Refused(f"{step.label}: `{text}` names no package")
    return command


def build_flags(command: GoTest) -> list[str]:
    flags = []
    if command.tags:
        flags += ["-tags", command.tags]
    if command.race:
        flags.append("-race")
    return flags


def resolve(command: GoTest) -> None:
    """The import path of each package argument, with ./... expanded under the step's flags."""
    out = go(["go", "list", "-f", "{{.ImportPath}}", *build_flags(command), *command.packages],
             command.step.directory, f"{command.step.label}: go list of `{command.text}`")
    command.resolved = out.split()
    if not command.resolved:
        raise Refused(f"{command.step.label}: `{command.text}` lists no package")


def listing(command: GoTest, pattern: str) -> dict[str, list[str]]:
    """The names each package of one command prints for one `go test -list` pattern, under the
    command's own build flags."""
    out = go(["go", "test", "-count=1", "-list", pattern, *build_flags(command), *command.resolved],
             command.step.directory,
             f"{command.step.label}: go test -list {pattern} of `{command.text}`")
    names: dict[str, list[str]] = {}
    pending: list[str] = []
    for line in out.splitlines():
        line = line.strip()
        if NAME.match(line):
            pending.append(line)
            continue
        status = STATUS.match(line)
        if not status:
            continue  # a test binary's own output, which names no test
        kind, package = status.group(1), status.group(2)
        if kind == "FAIL":
            raise Refused(f"{command.step.label}: `go test -list` failed on {package}\n{indent(out)}")
        names[package] = pending
        pending = []
    missing = [p for p in command.resolved if p not in names]
    if missing:
        raise Refused(f"{command.step.label}: `go test -list` printed no result line for "
                      f"{', '.join(missing)}\n{indent(out)}")
    return names


def list_tests(command: GoTest) -> tuple[dict[str, list[str]], dict[str, list[str]]]:
    """What each package of one command runs, and what it compiles.

    `go test -list` answers the pattern it is given, so what the step runs is a listing under the
    step's own `-run` and what its test binary holds is a listing under `.*`: the same packages
    under the same build flags, and a second run of binaries the first listing already built. A
    command carrying no `-run` lists once, and both answers are that one listing.

    A `-skip` prints in either listing, since `-list` names the tests a `-skip` would keep from
    running, so it is subtracted from what the step runs and left in what it compiles - which is
    what a skipped test is: built into the binary and never run by it.
    """
    compiled = listing(command, ".*")
    skipped = set(skip_names(command.skip) or ())  # the parse refused every other shape
    run = compiled if not command.run else listing(command, command.run)
    return ({package: [name for name in names if name not in skipped]
             for package, names in run.items()}, compiled)


Expansion = tuple[list[Step], dict[tuple[str, str], list[Step]],
                  dict[tuple[str, str], list[Step]], list[str]]


def expansion() -> Expansion:
    """Every (package, function) pair, the steps that run it, the steps whose test binary holds
    it whether they run it or not, and a line per step. A step runs no function it does not
    compile, so the second map holds every key of the first and the pairs no step runs besides."""
    found = documents()
    steps = steps_of(found)
    if not steps:
        raise Refused(f"{WORKFLOW} and the {len(found) - 1} workflow(s) its jobs call have no "
                      f"`go test` step in {' or '.join(MODULE_DIRS)}")
    pairs: dict[tuple[str, str], list[Step]] = {}
    built: dict[tuple[str, str], list[Step]] = {}
    report = []
    for step in steps:
        commands = commands_of(step)
        functions = 0
        compiled_count = 0
        packages: set[str] = set()
        for command in commands:
            resolve(command)
            run, compiled = list_tests(command)
            for package, names in compiled.items():
                packages.add(package)
                for name in names:
                    if name.startswith("Benchmark"):
                        continue  # no step passes -bench, so no step runs it
                    compiled_count += 1
                    holders = built.setdefault((package, name), [])
                    if step not in holders:
                        holders.append(step)
            for package, names in run.items():
                for name in names:
                    if name.startswith("Benchmark"):
                        continue
                    functions += 1
                    runners = pairs.setdefault((package, name), [])
                    if step not in runners:
                        runners.append(step)
        report.append(f"  {step.label}")
        report.append(f"      in {step.workdir}: {len(commands)} go test command(s), "
                      f"{len(packages)} package(s), {functions} function(s) run of "
                      f"{compiled_count} compiled")
        for command in commands:
            report.append(f"      -run {command.run or '(none)'}"
                          f"{' -skip ' + command.skip if command.skip else ''}"
                          f"{' -tags ' + command.tags if command.tags else ''}"
                          f"{' -race' if command.race else ''}"
                          f": {len(command.resolved)} package(s)")
    return steps, pairs, built, report


def match_exception(package: str, name: str, runners: list[Step]) -> Exception_ | None:
    keys = frozenset(step.key for step in runners)
    for entry in EXCEPTIONS:
        if entry.steps != keys or entry.package != package:
            continue
        if entry.functions is None or name in entry.functions:
            return entry
    return None


def main() -> int:
    if len(sys.argv) > 1:
        print(f"usage: {Path(sys.argv[0]).name} (no arguments)", file=sys.stderr)
        return 2
    for entry in EXCEPTIONS:
        if not entry.requirement.strip():
            print(f"exception over {entry.package} names no requirement, so it is not one",
                  file=sys.stderr)
            return 2

    try:
        steps, pairs, built, report = expansion()
    except Refused as refusal:
        print(f"refused: {refusal}", file=sys.stderr)
        return 2

    print(f"# go test step expansion of {WORKFLOW} and the workflows its jobs call, under {ROOT}")
    print("# Every test function each go test step of packages/envoy, packages/envoy/infra and")
    print("# packages/daemon runs, through go test -list over the step's own packages, -tags,")
    print("# -race, -run and -skip, and every function such a step compiles and no step runs.")
    print("# Run it at two commits and compare the pair sets:")
    print("#   grep '^PAIR' head.txt > head.pairs; grep '^PAIR' base.txt > base.pairs")
    print("#   diff base.pairs head.pairs      (empty: the two commits run the same functions)")
    print("# Check k1 of dispatch://INFRA-1408. No CI job runs this script; the module header")
    print("# says why, and what it needs (CGO_ENABLED=1 for the -race steps, no service).")
    print()
    print(f"== steps expanded ({len(steps)}) ==")
    print("\n".join(report))
    print()

    duplicates = sorted((package, name, runners)
                        for (package, name), runners in pairs.items() if len(runners) > 1)
    numbered = {entry: number for number, entry in enumerate(EXCEPTIONS, start=1)}
    applied: dict[Exception_, list[str]] = {entry: [] for entry in EXCEPTIONS}
    undeclared = []
    print(f"== pairs more than one step runs ({len(duplicates)}) ==")
    if not duplicates:
        print("  none")
    for package, name, runners in duplicates:
        entry = match_exception(package, name, runners)
        print(f"  {package} {name}")
        for step in sorted(runners, key=lambda s: s.label):
            print(f"      {step.label}")
        if entry is None:
            undeclared.append((package, name))
            print("      undeclared: no exception covers this pair")
        else:
            applied[entry].append(f"{package} {name}")
            print(f"      declared by exception {numbered[entry]} below")
    print()

    unrun = sorted(pair for pair in built if pair not in pairs)
    print(f"== pairs some step compiles and no step runs ({len(unrun)}) ==")
    if not unrun:
        print("  none")
    for package, name in unrun:
        print(f"  {package} {name}")
        for step in sorted(built[(package, name)], key=lambda s: s.label):
            print(f"      compiled by {step.label}")
    print()

    print(f"== declared exceptions ({len(EXCEPTIONS)}) ==")
    for entry in EXCEPTIONS:
        names = ", ".join(entry.functions) if entry.functions else "every function both steps run"
        print(f"  exception {numbered[entry]}: {entry.package}: {names}")
        for job, step in sorted(entry.steps):
            print(f"      step: {job} / {step}")
        print(f"      requirement: {entry.requirement}")
        if applied[entry]:
            print(f"      applied to {len(applied[entry])} pair(s):")
            for pair in applied[entry]:
                print(f"        {pair}")
        else:
            print("      not needed: no pair of these steps matched it")
    print()

    print(f"== every pair a step runs ({len(pairs)}) ==")
    for package, name in sorted(pairs):
        print(f"PAIR\t{package}\t{name}")
    print()

    if undeclared or unrun:
        if undeclared:
            print(f"{len(undeclared)} pair(s) run by more than one step and declared by none; "
                  "each is a test two steps of one run run.", file=sys.stderr)
        if unrun:
            print(f"{len(unrun)} pair(s) some step compiles and no step runs; each is a test a "
                  "step of this run builds into its binary and no step of this run calls.",
                  file=sys.stderr)
        return 1
    print(f"No undeclared duplicate and no unrun pair: {len(pairs)} pair(s) run over "
          f"{len(steps)} step(s), {len(duplicates)} of them declared.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
