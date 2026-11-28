"""The scripts the steps of ci.yaml's router run, each read by a person, and the one function that
says whether a step's run: is such a script and what it then runs.

A script's control flow is what decides what it runs, which a line-by-line reading cannot see, so
no tool that needs to know what a step runs reads such a script itself. Each entry here is a
person's reading of one script - the go test it runs for each argument list a step passes it, on
its own line or after `xargs` - pinned by the script's SHA-256. This is the one table of such
readings. Whether a step's run: is one of those forms, and which reading it then gets, is answered
once, by reading_of, and every tool that reads what a step runs calls it:
go-test-step-expansion.py's audited_step, which holds every pair a step runs to the rule that it
runs once and refuses a script whose text has moved past its digest; ci_filter_coverage.py's
step_go_lines, which check-ci-filters.sh's TAG_RUNS rule reads a step's go test lines through; and
go_test_steps.py's audited_step, which the test-reach search (check-test-reach.sh) reads every
step through and which refuses a moved script as the expansion does. A run: whose text holds an
audited script's file name is read as one of its entry's forms or raises Unreadable, which each of
them turns into a refusal naming the step, so none reads such a run: as if it named no script. A
run: that spells the name some other way - quotes inside it, or the path in a variable - holds no
file name and is read as naming no script: that is the limit go-test-step-expansion.py's header
records for every script it does not audit, and no step is written so. Whether a run: names an
audited script at all is names_audited's one test, which steps_of and reading_of both call.
go-test-step-expansion.test.sh holds the expansion and TAG_RUNS to the same reading of every step
of this head, and checks each digest; the test job runs it.

An entry the test-reach search alone reads (`search_only`) is invisible to the other two, which
read such a step from its run: alone, as they did before the search read it: forced-order.sh and
its test build their go test with -tags odatrace against the hooked nats-server the script builds
first, which go-test-step-expansion.py cannot compile for its listing, and their -run is that
tag's tests, which TAG_RUNS would hold to a job and filter of their own that nothing declares.

It imports nothing of this repository, so each reader can import it without the others.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field


@dataclass(frozen=True)
class AuditedScript:
    """A script a step runs, read by a person: the module it moves into before its tests, the one
    `go test` it runs there for each argument list a step passes it (the run:'s text after the
    script's path, "" for none), the one it runs for each argument list a legs step passes it
    after `xargs <path>` (`xargs`, each the go test the packages xargs appends are appended to),
    and the SHA-256 of the text that reading was of. `modules` names another module for an
    argument list that moves the script elsewhere (a `-C <module>`), and `search_only` marks an
    entry only the test-reach search reads (the module's header says why)."""

    module: str
    commands: dict[str, str]
    sha256: str
    xargs: dict[str, str] = field(default_factory=dict)
    modules: dict[str, str] = field(default_factory=dict)
    search_only: bool = False


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

# envoy-go's unit legs' -skip, quoted as ci_job_reads.UNIT_TEST_SCRIPT writes it from
# UNIT_SKIP_TESTS: the web display fixture's two tests, BACKEND_COUNT_TESTS and the rows whose
# bound is on time, which envoy-go-alone runs (ci_job_reads.UNIT_ALONE_TESTS). Which packages each
# leg runs is go_legs.py's, which the legs step's lister runs.
UNIT_SKIP = ("'^(TestABatchCutOffInFlightKeepsItsOrder|"
             "TestAClaimLeftByAnotherProcessIsTakenWhenItLapses|TestADeletedBucketFailsAWriteWithinTheBudget|"
             "TestAHolderWhoseRecordFindsTheHoldAnothersStopsItsRenewal|"
             "TestAProcessReportsASilentNATSCutWithinSeconds|"
             "TestARollingUpdatesSuccessorHandsEachMentionTheOldProcessLeftOnce|"
             "TestAStalledListingFailsWithinTheReadTimeout|"
             "TestAStandbyDoesNotTryTheHoldForEveryEventTheHolderWrites|"
             "TestAStandbyTakesAFreeHoldWithNoNotification|"
             "TestAStoppedOutboxHandsThePublisherToTheOtherAtOnce|TestAWriteIsEmbeddedOnItsNotification|"
             "TestAnEventNATSNeverAcknowledgesIsBackedOffAndRecorded|"
             "TestAnOutboxThatLosesNATSReleasesThePublisher|"
             "TestAnOutboxWhoseNATSGoesSilentGivesThePublisherToTheOther|"
             "TestClosingThePoolGivesUpOnALateConnectionOnceTheDatabaseStopsAnswering|"
             "TestEnrollThroughTheHelperPastTheBoundSaysTheOutcomeIsUnknown|"
             "TestEverySecretFormNamesTheSourceItUses|TestExecLearnsOfAGrantAfterAFullHold|"
             "TestExecLearnsOfTheGrantWithinASecond|TestGCPSecretMetadataSourceIsPresentWhenItYieldsAToken|"
             "TestHealthzAnswersWhilePostgresAndTheBucketBothStopAnswering|"
             "TestHealthzAnswersWhilePostgresStopsAnswering|TestHoldExpiryMatchesTheDatabaseClock|"
             "TestHoldReportsLossBeforeTheDatabaseExpiryWhenRenewalsFail|TestIdentity|"
             "TestNoSecretFormAsksTheBrokerForACredential|"
             "TestOneFillHoldsTheLeaseAndAnotherTakesOverWithinSixtySeconds|TestOnlyOneOutboxPublishesAtATime|"
             "TestReserveTokensSharesItsBudgetAcrossConnections|"
             "TestSIGTERMWhileARequestHoldsHTTPShutdownStillSettlesTheOwedDocument|"
             "TestSIGTERMWithOpenStreamsExitsPromptlyAndSettlesTheOwedDocument|"
             "TestSIGTERMWithTheDatabaseUnansweringExitsWithinTheShutdownBudget|"
             "TestSamplerHeapReadDelaysNoSample|TestScanPassReturnsWhenThePendingRowsFallDue|"
             "TestSettingsNameTheGoogleExchangeAudienceOnEitherStore|TestSyncDeadlineIsRecordedOnTheRow|"
             "TestTheChainSkipsAbsenceAndStopsOnABrokenSource|TestTheIdentityStepsRunOnceTheQueueFirstEmbeds|"
             "TestTheListenerReplacesAHalfOpenConnection|"
             "TestTheLostLineIsWrittenBeforeTheReleaseAndDatesTheStop|TestTheOutboxDoesNotScanWhileIdle|"
             "TestTheWebFixtureFindsItsFilesFromAnyDirectory|"
             "TestUseCommandOutsideASessionReachesTheMachineSource|TestWebDisplayFixture)$'")

# The forced-order run: every TestForcedOrder… test of the packages, with -tags odatrace and -race,
# under .github/scripts/go-test.sh.
FORCED_ORDER = "go test -tags odatrace -race -count=1 -timeout 10m -run '^TestForcedOrder'"

# Each script a step of ci.yaml's jobs runs as its whole run: line, or as what a legs step pipes
# its packages into, read by a person, with the one `go test` it runs for each argument list a step
# passes it: on the step's own line (commands), or after a legs step's `xargs <path>` (xargs).
# reading_of says which of them a step's run: is. A step that runs any other script is read by
# go_test_steps.py through the script's own lines where it can read each of them; a script it
# cannot read whole - one that lists its packages into an array, moves directory inside a
# function, or reads its arguments - is passed over, and an entry here is how such a script is
# read. A wrapper a Go step calls in place of `go test` (one that runs gotestsum for it) is read
# only once go_test_steps.py names its command (GOTESTSUM_COMMANDS) or its path (GO_WRAPPERS), or
# it has an entry here. Each entry's go test is the command the script hands go-test.sh, which
# runs it under gotestsum with the same packages and flags, so no entry writes the wrapper.
AUDITED_SCRIPTS = {
    # It moves into packages/envoy (`cd "$(dirname "$0")/.."`) and runs one `go test -count=1`
    # over its packages under .github/scripts/go-test.sh, then counts the skips in its jsonfile.
    # With no package argument it counts its DEFAULT_PACKAGES, ./internal/cipher/...,
    # ./cmd/cipherd/ and ./internal/holdrecord/, less the import paths its DEFAULT_EXCEPT takes out
    # of `go list` of them with `grep -Ev`, none today since that pattern is empty
    # (ci_job_reads.DATABASE_SKIPS_PATTERN, built from DATABASE_SKIPS_RACE_ONLY), with no -tags,
    # -race, -run or -skip, under -timeout "${GO_TEST_TIMEOUT:-15m}". A package argument replaces
    # those packages, `--race` adds -race, `--skip P` adds -skip P, `--timeout D` sets the
    # -timeout, and `--var NAME` decides only whether it runs at all. `--count-table` runs every
    # package its arguments name, in go list's order, and counts the table's among them; it changes
    # what is counted, never what runs. With `--report FILE` it reads a report and runs no test.
    # `-run=^(Name|...)$`, which a leg pipes in with its package, adds that -run.
    # envoy-go-race-api's legs and envoy-go's unit legs pass an xargs argument list below, each
    # leg's packages (and group) after it.
    "packages/envoy/scripts/database-skips.sh": AuditedScript(
        module="packages/envoy",
        commands={},
        xargs={
            f"--timeout 20m --skip {UNIT_SKIP} --count-table": f"go test -count=1 -skip {UNIT_SKIP} -timeout 20m",
            # envoy-go-race-api's legs: the API package, and the leg's group as `-run=^(Name|...)$`,
            # come from go_legs.py through xargs, so the command carries neither.
            "--var DISPATCH_TEST_DATABASE_URL --var BROKER_TEST_DATABASE_URL --race --timeout 70m":
                "go test -count=1 -race -timeout 70m",
        },
        sha256="a6dc9bc33eb56897d4012a3ed5a6dd8fe7517f9493fbec7df88ad81e3ca06bd3",
    ),
    # It moves into packages/envoy (`cd "$(dirname "$0")/.."`) and runs one `go test -count=1
    # -timeout "${GO_TEST_TIMEOUT:-5m}" -run '^(TEST|…)$'` under .github/scripts/go-test.sh over the
    # packages before its `--`, the -run built from the test names after it, then counts each
    # test's `aws` and `gcp` passes in the jsonfile's last attempts. GO_TEST_SKIP adds a -skip,
    # which no step sets, and envoy-go's job sets neither variable. envoy-go's count step passes
    # the argument list below.
    "packages/envoy/scripts/backend-count.sh": AuditedScript(
        module="packages/envoy",
        commands={
            "./cmd/cipher/ ./internal/cipher/api/ -- " + " ".join(BACKEND_COUNT_TESTS):
                f"go test -count=1 -timeout 5m -run '^({'|'.join(BACKEND_COUNT_TESTS)})$' "
                "./cmd/cipher/ ./internal/cipher/api/",
        },
        sha256="e8b0c1506f131aa80d4e7f8d2ccd9cc7220ae901e3f41b8bdd72f7fd3b7da8c7",
    ),
    # It moves into packages/envoy/infra (`cd "$(dirname "$0")/.."`) and, given `run`, runs one
    # `go test -count=1 -race` under .github/scripts/go-test.sh over the packages `go list ./...`
    # prints less those its EXCLUDE, '/internal/textcheck$' (ci_job_reads.INFRA_PLAIN_PATTERN),
    # matches, with no -tags, -run or -skip. Given `compile`, it builds those race test binaries
    # and runs none, which ci-caches.yaml's envoy-infra fill alone does, outside ci.yaml.
    # envoy-infra-build's unit step passes `run`. Until this entry the expansion skipped that step,
    # so neither of its verdicts saw the module's race run: every untagged test of infra/core,
    # infra/seed and infra/gcp read as compiled by the natslive step and run by no step, and
    # infra/host's untagged tests, which envoy-smoke's pulumilive step runs plain, as run once.
    "packages/envoy/infra/scripts/race-unit.sh": AuditedScript(
        module="packages/envoy/infra",
        commands={
            "run": "go list ./... | grep -Ev '/internal/textcheck$' | xargs go test -count=1 -race",
        },
        sha256="50c61cbc6bfd19697fe7c7741afbe268fa9105079868570a53f47c91d32a912d",
    ),
    # It moves into packages/envoy/infra (`cd "$(dirname "$0")/.."`), takes the packages to run as
    # its arguments and refuses none, reads each one's -run from go-tagged-tests.sh's natslive list
    # before any run starts, then runs one `go test -count=1 -timeout 25m -tags natslive -run
    # <that package's list> <package>` per package under .github/scripts/go-test.sh, each with a
    # jsonfile of its own (GO_TEST_JSONFILE), the three at once in the background, and waits for
    # each by process id. What it runs is the go tests below, one per package with its own list,
    # exactly as the step ran them one after another before it ran them at once; its output files,
    # its jsonfiles' names, its headings and its exit status carry no test. envoy-infra-go's
    # natslive step passes the argument list below (ci_job_reads.INFRA_NATSLIVE_PACKAGES), so a
    # package added or dropped moves the step's own run: line and this entry together.
    "packages/envoy/infra/scripts/natslive.sh": AuditedScript(
        module="packages/envoy/infra",
        commands={
            "./core ./seed ./gcp": "for pkg in ./core ./seed ./gcp; do\n"
                                   "  pattern=$(../../../.github/scripts/go-tagged-tests.sh natslive \"$pkg\")\n"
                                   "  go test -count=1 -timeout 25m -tags natslive -run \"$pattern\" \"$pkg\"\n"
                                   "done",
        },
        sha256="64265ade73fd295dcac927888b04c227375d2b88ed2a91e52695039e5b38d362",
    ),
    # It moves into the module its -C names, from the caller's directory (packages/envoy without
    # one), builds a hooked copy of nats-server, and runs one `go test -tags odatrace -race
    # -count=1 -timeout "${GO_TEST_TIMEOUT:-10m}" -run '^TestForcedOrder'` of the packages after
    # its flags against that copy, under .github/scripts/go-test.sh; -server picks the hooked
    # version and -drop cuts a hook point, and neither changes a flag of the command. Its `go test
    # -list` first runs no test. daemon-go's leg 2 passes the first argument list below, from the
    # root; envoy-go-race's leg 6, its last, the other two, from packages/envoy. The test-reach
    # search alone reads it (search_only, the module's header says why).
    "packages/envoy/scripts/forced-order.sh": AuditedScript(
        module="packages/envoy",
        commands={
            "-C packages/daemon ./internal/testnats/": f"{FORCED_ORDER} ./internal/testnats/",
            "./internal/testnats/hooktest/ ./cmd/listener/": f"{FORCED_ORDER} ./internal/testnats/hooktest/ ./cmd/listener/",
            "-server 2.10.29 ./internal/testnats/hooktest/": f"{FORCED_ORDER} ./internal/testnats/hooktest/",
        },
        modules={"-C packages/daemon ./internal/testnats/": "packages/daemon"},
        sha256="2d3891be59975ab79c860981e7f9b6fa7173fe0f013e079a546162c8d938e576",
        search_only=True,
    ),
    # forced-order.sh's own test, which envoy-go-race's leg 6 runs from packages/envoy with no
    # argument. Of its five cases one gets as far as a go test: forced-order.sh over the fixture
    # package ./internal/testnats/hooktest/skiponly/, whose one forced-order test skips itself,
    # the refusal that case holds; the other four are refused before anything is built
    # (dispatch://INFRA-3411's test-value-md, row 9.8). So the step runs that package's tests,
    # under forced-order.sh's command. The test-reach search alone reads it, as it does that
    # script.
    "packages/envoy/scripts/forced-order.test.sh": AuditedScript(
        module="packages/envoy",
        commands={"": f"{FORCED_ORDER} ./internal/testnats/hooktest/skiponly/"},
        sha256="7a00345dc6c667859f24c158b0ad58e58e7f6d58eb0b34fa4b23f6ea3b19abc1",
        search_only=True,
    ),
}


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


# A legs step's run: that hands its leg's packages to an audited script: `set -o pipefail`, then
# the lister piped to `xargs [-r] <the script's path> <an argument list>`. `-r` runs nothing on
# empty input (a refused leg), and changes nothing the command runs when it runs.
XARGS_AUDITED = re.compile(
    r"^set -o pipefail\n(?P<lister>[^\n|]+?) \| xargs (?P<empty>-r )?(?P<spelled>\S+) (?P<arguments>[^\n]+)$")


class Unreadable(Exception):
    """A run: that names an AUDITED_SCRIPTS script in a form no entry reads. Its text says which
    script and which forms an entry reads; a caller puts the step's name in front of it."""


@dataclass(frozen=True)
class Reading:
    """What a run: that is an audited script runs: the script, its entry, the entry's reading of
    the run: (`script`), and the directory a reader reads that shell text in (`module`): the
    entry's module, or the one its `modules` names for the run's argument list."""

    path: str
    entry: AuditedScript
    script: str
    module: str


def names_audited(script: str, search: bool = False) -> list[str]:
    """The AUDITED_SCRIPTS scripts whose file name a run:'s text holds, as plain text. This is the
    one test of whether a run: names an audited script: reading_of starts from it, and
    go-test-step-expansion.py's steps_of calls it to decide which steps reach reading_of at all, so
    a change to what counts as naming a script reaches both. A name spelled some other way - quotes
    inside it, or the path in a variable - is not held, and such a run: names no script here. A
    search_only entry's script is named only for the test-reach search (`search`)."""
    return [path for path, entry in AUDITED_SCRIPTS.items()
            if (search or not entry.search_only) and path.rsplit("/", 1)[-1] in script]


def reading_of(workdir: str, script: str, search: bool = False) -> Reading | None:
    """Whether a run: in workdir is an AUDITED_SCRIPTS script, and what it runs if so. None where
    the run's text holds no such script's file name (names_audited). A Reading where it is one of
    the two forms an entry reads: a run: that is the script's path as spelled from workdir
    (spellings), alone or followed by an argument list the entry's `commands` lists, from the
    repository root or from any directory on the script's own path, spelled from there, since the
    script moves into its module itself; or a legs step's `set -o pipefail` and lister piped to
    `xargs [-r] <path> <an argument list the entry's xargs lists>`, run from the entry's module,
    since the lister runs in the step's own directory and the packages it prints must name the same
    directories there. Unreadable for any other text that holds the file name - an operator, another
    command, a `$(…)`, a comment, another line, an argument list the entry does not list, or a run
    from a directory off the script's path, such as a `../` spelling from beside the module, which
    bash would run. Each of those fails closed, by name, on every reader, and that is the design:
    a reading is of a form a person read, and a form nobody read is refused rather than guessed at.
    The test is on the raw text, before any shell reading, so a quote or a substitution elsewhere in
    the run: does not hide a file name the text holds; a run: that spells the name itself some other
    way holds none, and is None here. `search` reads the search_only entries too, which the
    test-reach search alone asks for (go_test_steps.py's audited_step)."""
    named = names_audited(script, search)
    if not named:
        return None
    # Only the shell's own blanks and the line's end: Python's strip() also takes Unicode spaces,
    # which bash reads as part of the name.
    alone = script.strip(" \t\n")
    piped = XARGS_AUDITED.match(alone)
    if piped:
        spelled, arguments = piped.group("spelled"), piped.group("arguments")
        matches = [path for path in named if spelled in spellings(path, workdir)]
        readable = (len(named) == 1 and len(matches) == 1 and workdir == AUDITED_SCRIPTS[matches[0]].module
                    and arguments in AUDITED_SCRIPTS[matches[0]].xargs)
    else:
        # The path is the text up to the first space, and the arguments the text after it, compared
        # with each listed argument list as written: one space between words, nothing else.
        spelled, _, arguments = alone.partition(" ")
        # The literal spelling, compared with no path normalization: `path/` and `path/.` resolve to
        # the file in Python and name nothing the shell can run.
        matches = [path for path in named if spelled in spellings(path, workdir)]
        readable = (len(named) == 1 and len(matches) == 1 and not any(c.isspace() for c in spelled)
                    and arguments in AUDITED_SCRIPTS[matches[0]].commands)
    if not readable:
        raise Unreadable(f"names {', '.join(named)} in a form no AUDITED_SCRIPTS entry reads: an "
                         "entry reads a run: that is that one path, alone or followed by an argument "
                         "list the entry lists, or a legs step's `set -o pipefail` and lister piped to "
                         "`xargs <path> <an argument list its xargs lists>`, run from the entry's module")
    path = matches[0]
    entry = AUDITED_SCRIPTS[path]
    if piped:
        return Reading(path=path, entry=entry,
                       script=f"set -o pipefail\n{piped.group('lister')} | xargs {piped.group('empty') or ''}"
                              f"{entry.xargs[arguments]}",
                       module=entry.module)
    return Reading(path=path, entry=entry, script=entry.commands[arguments],
                   module=entry.modules.get(arguments, entry.module))


