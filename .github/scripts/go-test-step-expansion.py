#!/usr/bin/env python3
# legion: tool - lists every test function each `go test` step of ci.yaml's router runs, with `go test -list` over the step's own packages and flags, and names each one more than one step runs that no exception declares, for the author of a change to a Go step's packages, `-run` or `-skip`, who runs it at the head and at the merge base before the merge and compares the two pair lists.
"""The test functions each `go test` step of ci.yaml's router runs, and which of them two run.

It expands every `go test` step of `.github/workflows/ci.yaml`, and of every workflow a job of it
calls, whose working directory is `packages/envoy`, `packages/envoy/infra` or `packages/daemon`
into the test functions the step really runs - through the step's own packages, `-tags`,
`-race`, `-run` and `-skip`, with `go test -list` - and prints one verdict over them: every
(package, function) pair more than one step runs.

It exits 1 when such a pair is one nothing accounts for, and 0 when every pair two steps run is
declared with the requirement that makes it run twice (EXCEPTIONS below). An entry is keyed on
exactly the steps that run the pair, and its requirement answers what makes it run twice.

It had a second verdict, every pair some step compiles and no step runs, which
.github/scripts/check-test-reach.sh replaced: that search runs in ci.yaml's lint job on every
change and reads the same steps through the same reader (.github/scripts/go_test_steps.py), with
no `go` command, and names a test one step's `-skip` leaves out and no other step runs, a build
tag no step passes, and the other ways a test can be left out of every required job.

No CI job runs this script, and none should. It compiles every test binary of all three modules,
the `-race` ones included. Measured in a Legion pod on a warm module and build cache, the daemon
module alone costs about six minutes of that: `go test -count=1 -list '.*' ./...` over
packages/daemon takes 135 s and prints 4,644 names, and the race listing over
`./internal/daemon/ ./internal/sessionproxy/ ./internal/launcher/` takes 234 s and prints 933.
Five whole runs in that pod took 648 s, 402 s, 387 s, 461 s and 430 s, and a cold cache costs
tens of minutes, for an answer a person needs twice per change (at the head and at the merge
base) rather than once per run.

Between those hand runs CI holds the plugin-pack part of the property itself. The plugin-pack
step's `-run` and the daemon-go race step's `-skip` are both built from one list,
PLUGIN_PACK_TESTS in ci_job_reads.py, and check-ci-filters.sh pins each step's whole script, so an
edit to either step's text is refused (a step whose script is not its pinned text fails, naming
the step and the first line that differs, and on that line each test its -run or -skip names and
the pin does not, or the other way round) and the two cannot come to name different tests.
ci_job_reads.py's hold_plugin_pack_tests() refuses, at import, a name in that list that the
package no longer declares and a test of agentmodels_omp_test.go the list does not name. And
go-test-step-expansion.test.sh runs the step reader this script reads the steps with
(.github/scripts/go_test_steps.py), with no `go` command, over every `go test` step of the head,
so a step this script would refuse fails the pull request that makes it unreadable rather than
the next hand run. What CI does not see is what this script decides: a (package, function) pair
two steps of one run run without a declared reason.

It starts no service and needs no Postgres, no NATS, no Docker and no Pulumi: it never runs a
test. `go test -list` builds the test binary and asks it for its names, and each `TestMain` of the
packages here returns before it starts its database, NATS server, container or Pulumi stack when
the run is a listing.

`-race` needs cgo, so run it with `CGO_ENABLED=1` and a C compiler on PATH; a race step otherwise
fails to build and this script stops on that step.

Run it at two commits and compare - the head in your workspace, the merge base in a clone under a
temporary directory:

    store="$(cd "$WORKSPACE/.jj" && realpath "$(cat repo)")/store"
    cd "$WORKSPACE"
    python3 .github/scripts/go-test-step-expansion.py > "$TMPDIR/head.txt"; echo "head: $?"
    git clone --no-local "$(cd "$store" && realpath "$(cat git_target)")" "$TMPDIR/base"
    git -C "$TMPDIR/base" checkout <merge base>
    cp .github/scripts/go-test-step-expansion.py .github/scripts/go_test_steps.py "$TMPDIR/base/.github/scripts/"
    (cd "$TMPDIR/base" && python3 .github/scripts/go-test-step-expansion.py) > "$TMPDIR/base.txt"
    echo "base: $?"
    grep '^PAIR' "$TMPDIR/head.txt" > "$TMPDIR/head.pairs"
    grep '^PAIR' "$TMPDIR/base.txt" > "$TMPDIR/base.pairs"
    diff "$TMPDIR/base.pairs" "$TMPDIR/head.pairs"   # empty: the change loses no test

It reads the repository root off its own path, so the copy under the temporary directory checks
itself and not the workspace it was copied from. It reads the steps with
.github/scripts/go_test_steps.py, which sits beside it in both trees and says how a step is read:
the documents, the step's directory and its comment-free lines (through `ci_filter_coverage`'s
own helpers), `for` loops, the variables and wrappers before a command, `-skip`, and the scripts
its AUDITED_SCRIPTS reads. Two lines are answered by running them in the step's own directory
(`go_test_steps.shell_answer`): a `-run "$pattern"` assigned from a `$(…)` call of
`.github/scripts/go-tagged-tests.sh`, and the package list of `go list … | grep -Ev '…' | xargs`.
Keep `GOMODCACHE` and `GOCACHE` the same for both runs: the second run then reuses the first
run's compile.

A `-skip '^(Name|Name|…)$'` names tests a step leaves out, which are subtracted from what the step
runs. A `Benchmark…` name a listing prints is left out of the verdict: no step passes `-bench`, so
`go test` builds those functions and runs none of them.

What it prints: the steps it expanded, the pairs more than one step runs, the declared exceptions
it applied and the ones it did not need, and every pair a step runs, one per line behind a
`PAIR` marker for the diff above. Exit 0 when no pair is run twice undeclared, 1 when one is, 2
when it cannot read a step or a `go` command fails.
"""

from __future__ import annotations

import re
import shlex
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

from go_test_steps import (MODULE_DIRS, NAME, ROOT, WORKFLOW, GoTest, Refused, Step, commands_of,
                           documents, indent, shell_answer, skip_names, steps_of)


@dataclass(frozen=True)
class Exception_:
    """A pair two steps may run, and the requirement that makes it run twice.

    `steps` must be exactly the steps that run it, each named as `Step.key` names it - the
    repository-relative workflow file that holds the step, and the step's `name:` - `package` its
    import path, `functions` the function names (None: every function of that package those steps
    share). An entry whose `requirement` is empty is refused: a duplicate with no requirement
    written beside it is a duplicate, not an exception.
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
            "Dispatch-only change where envoy-infra-go does not, and in such a run those untagged "
            "tests would otherwise run nowhere."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-go.yaml", "Test what the race build cannot run at all or at full size"),
            (".github/workflows/ci-job-envoy-go-race-docs.yaml", "Race-test the document service"),
            (".github/workflows/ci-job-envoy-go-docs-finalizers.yaml", "Test the document service under the cleanup detector"),
        }),
        package="github.com/sjawhar/envoy/internal/dispatch/docs",
        functions=(
            "TestRenderReuseARandomisedEditStorm",
            "TestRenderReuseAReplicaReplaysTheRoomsFormattingExactly",
        ),
        requirement=(
            "R2 (dispatch://INFRA-1408/spec): the two render-reuse storms run fewer seeds and "
            "fewer edits under the race detector (raceEnabled), so envoy-go's plain step is their "
            "full-size run and envoy-go-race-docs is the detector's run of the shrunk one. The "
            "third step is the cleanup detector's, declared with the rest of the package below."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-go.yaml", "Test what the race build cannot run at all or at full size"),
            (".github/workflows/ci-job-envoy-go-race.yaml", "Race-test delivery, the store, the bus, the listener, the registry store, the session cache, kvwatch, the NATS credentials and the NATS auth service"),
        }),
        package="github.com/sjawhar/envoy/internal/broker/natscallout",
        functions=("TestACalloutStormAfterAServerRestartAdmitsEveryConnection",),
        requirement=(
            "c-callout-available (the NATS auth service's verification): five hundred "
            "connections reconnect at once after a server restart and every one is admitted. The "
            "restart storm opens two hundred under the race detector (raceEnabled), whose cost per "
            "in-process connect fails the setup at five hundred on a loaded runner, so envoy-go's "
            "plain step is its full-size run and envoy-go-race the detector's run of the smaller one."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-go-race-docs.yaml", "Race-test the document service"),
            (".github/workflows/ci-job-envoy-go-docs-finalizers.yaml", "Test the document service under the cleanup detector"),
        }),
        package="github.com/sjawhar/envoy/internal/dispatch/docs",
        functions=None,
        requirement=(
            "A3 (dispatch://INFRA-1413/artifact/verification-md#b-k3): envoy-go-docs-finalizers "
            "runs this package in a Go mode no other step runs it in, GODEBUG=checkfinalizers=1, "
            "and with -v, because the package's own count of the services its tests left alive is "
            "printed from TestMain after they have run and a package-list run drops it. The mode "
            "is what the job is for, and a cleanup the detector reports can be any test's, so the "
            "lane takes the package whole rather than a -run of part of it."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-envoy-go.yaml", "Test what the race build cannot run at all or at full size"),
            (".github/workflows/ci-job-envoy-go-docs-finalizers.yaml", "Test the document service under the cleanup detector"),
        }),
        package="github.com/sjawhar/envoy/internal/dispatch/docs",
        functions=(
            "TestARoomFedOnlyByAgentWritesKeepsMemoryWithinTheBound",
            "TestUploadsOverOpenAnchorsKeepMemoryWithinTheBound",
            "TestUploadsToARoomInUseKeepMemoryWithinTheBound",
        ),
        requirement=(
            "R2 (dispatch://INFRA-1408/spec) and A3 (dispatch://INFRA-1413): these three bound a "
            "room's memory, which the race build cannot measure, so the race listing does not "
            "hold them and envoy-go's plain step is their one measured run. The finalizers lane "
            "runs them as part of the package under its own mode, for the reason above."
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
            (".github/workflows/ci-job-envoy-go.yaml", "Count the database broker suites' skips"),
        }),
        package="github.com/sjawhar/envoy/internal/broker/airlock",
        functions=("FuzzAirlock",),
        requirement=(
            "R3 (dispatch://INFRA-1408/spec): airlock-lean holds the Go airlock to its Lean model, "
            "and its filter takes files envoy_go's does not - its own job file, "
            ".github/actions/install-elan/** and packages/envoy/scripts/airlock-lean-axioms.sh - "
            "so on such a change envoy-go does not run and the seed corpus would run nowhere. Its "
            "own two steps are the corpus comparison and the 120-second fuzz, which seeds from "
            "that same corpus, so both name the function. envoy-go's step \"Count the database "
            "broker suites' skips\" runs every broker package but the race lane's through "
            "packages/envoy/scripts/database-skips.sh, so a listing counts the function run there "
            "too, but it skips there for want of AIRLOCK_MODEL, as that script's EXPECTED_SKIPS "
            "records, so the corpus runs only in airlock-lean."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-airlock-lean.yaml", "Compare the Go airlock with the model over the seed corpus"),
            (".github/workflows/ci-job-envoy-go.yaml", "Count the database broker suites' skips"),
        }),
        package="github.com/sjawhar/envoy/internal/broker/airlock",
        functions=("TestModelAgreesOnHostShapes",),
        requirement=(
            "The same reason as FuzzAirlock above: airlock-lean's corpus comparison runs the host "
            "shapes a rules file may hold through both the Go validation and the Lean model's, "
            "with AIRLOCK_MODEL set, and runs on changes that do not start envoy-go. envoy-go's "
            "step \"Count the database broker suites' skips\" runs every broker package but the "
            "race lane's through packages/envoy/scripts/database-skips.sh, so a listing counts "
            "the function run there too, but it skips there for want of AIRLOCK_MODEL, as that "
            "script's EXPECTED_SKIPS records, so the comparison runs only in airlock-lean. The fuzz "
            "step runs FuzzAirlock alone and does not name it."
        ),
    ),
    Exception_(
        steps=frozenset({
            (".github/workflows/ci-job-airlock-lean.yaml", "Compare the Go airlock with the model over the seed corpus"),
            (".github/workflows/ci-job-envoy-go.yaml", "Count the database broker suites' skips"),
        }),
        package="github.com/sjawhar/envoy/internal/broker/airlock",
        functions=("TestTheModelReadsAHostlessEntryWithGuidance",),
        requirement=(
            "The same reason as TestModelAgreesOnHostShapes above: airlock-lean's corpus "
            "comparison runs a host-less entry carrying guidance, a field the generator never "
            "writes, through both the Go airlock and the Lean model's, with AIRLOCK_MODEL set. "
            "envoy-go's step \"Count the database broker suites' skips\" lists it too, and it "
            "skips there for want of AIRLOCK_MODEL, as that script's EXPECTED_SKIPS records, so "
            "the comparison runs only in airlock-lean."
        ),
    ),
)

# `go test -list` prints one name per line (go_test_steps.NAME), then a status line per package.
STATUS = re.compile(r"^(ok|FAIL|\?)\s+(\S+)")


def go(args: list[str], cwd: Path, what: str) -> str:
    done = subprocess.run(args, cwd=cwd, capture_output=True, text=True)
    if done.returncode != 0:
        hint = ("\n    -race needs CGO_ENABLED=1 and a C compiler."
                if "-race" in args and "cgo" in (done.stderr or "") else "")
        raise Refused(f"{what} exited {done.returncode}\n"
                      f"    $ {shlex.join(args)} (in {cwd}){hint}\n{indent(done.stderr)}")
    return done.stdout


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


def list_tests(command: GoTest) -> dict[str, list[str]]:
    """What each package of one command runs: a `go test -list` under the step's own `-run`, or
    under `.*` where it carries none.

    A `-skip` prints in the listing, since `-list` names the tests a `-skip` would keep from
    running, so it is subtracted from what the step runs.
    """
    skipped = set(skip_names(command.skip) or ())  # the parse refused every other shape
    run = listing(command, command.run or ".*")
    return {package: [name for name in names if name not in skipped]
            for package, names in run.items()}


Expansion = tuple[list[Step], dict[tuple[str, str], list[Step]], list[str]]


def expansion() -> Expansion:
    """Every (package, function) pair, the steps that run it, and a line per step."""
    found = documents()
    steps = steps_of(found)
    if not steps:
        raise Refused(f"{WORKFLOW} and the {len(found) - 1} workflow(s) its jobs call have no "
                      f"`go test` step in {' or '.join(MODULE_DIRS)}")
    pairs: dict[tuple[str, str], list[Step]] = {}
    report = []
    for step in steps:
        commands = commands_of(step, shell_answer)
        functions = 0
        packages: set[str] = set()
        for command in commands:
            resolve(command)
            for package, names in list_tests(command).items():
                packages.add(package)
                for name in names:
                    if name.startswith("Benchmark"):
                        continue  # no step passes -bench, so no step runs it
                    functions += 1
                    runners = pairs.setdefault((package, name), [])
                    if step not in runners:
                        runners.append(step)
        report.append(f"  {step.label}")
        report.append(f"      in {step.workdir}: {len(commands)} go test command(s), "
                      f"{len(packages)} package(s), {functions} function(s) run")
        for command in commands:
            report.append(f"      -run {command.run or '(none)'}"
                          f"{' -skip ' + command.skip if command.skip else ''}"
                          f"{' -tags ' + command.tags if command.tags else ''}"
                          f"{' -race' if command.race else ''}"
                          f": {len(command.resolved)} package(s)")
    return steps, pairs, report


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
        steps, pairs, report = expansion()
    except Refused as refusal:
        print(f"refused: {refusal}", file=sys.stderr)
        return 2

    print(f"# go test step expansion of {WORKFLOW} and the workflows its jobs call, under {ROOT}")
    print("# Every test function each go test step of packages/envoy, packages/envoy/infra and")
    print("# packages/daemon runs, through go test -list over the step's own packages, -tags,")
    print("# -race, -run and -skip, and every one more than one step runs.")
    print("# Run it at two commits and compare the pair sets:")
    print("#   grep '^PAIR' head.txt > head.pairs; grep '^PAIR' base.txt > base.pairs")
    print("#   diff base.pairs head.pairs      (empty: the two commits run the same functions)")
    print("# No CI job runs this script; the module header says why, and what it needs")
    print("# (CGO_ENABLED=1 for the -race steps, no service).")
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

    if undeclared:
        print(f"{len(undeclared)} pair(s) run by more than one step and declared by none; "
              "each is a test two steps of one run run.", file=sys.stderr)
        return 1
    print(f"No undeclared duplicate: {len(pairs)} pair(s) run over "
          f"{len(steps)} step(s), {len(duplicates)} of them declared.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
