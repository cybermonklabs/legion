"""The scripts a step of ci.yaml's router runs as its whole run: line, each read by a person.

A script's control flow is what decides what it runs, which a line-by-line reading cannot see, so
neither tool that needs to know what a step runs reads such a script itself. Each entry here is a
person's reading of one script - the go test commands it runs for each argument list a step passes
it - pinned by the script's SHA-256, and both readers take that reading from here:
go-test-step-expansion.py, which holds every pair a step runs to the rule that it runs once, and
refuses a script whose text has moved past its digest; and ci_filter_coverage.py's TAG_RUNS rule,
which reads a step's go test lines for -tags and -run (command_for). So the two read such a step
the same way, from one reading, and a change to a script that its entry does not follow fails
the expansion tool's digest check, in go-test-step-expansion.test.sh, which the test job runs.

It imports nothing of this repository, so each reader can import it without the other.
"""

from __future__ import annotations

from dataclasses import dataclass, field

@dataclass(frozen=True)
class AuditedScript:
    """A script a step runs, read by a person: the module it moves into before its tests, the one
    `go test` it runs there for each argument list a step passes it (the run:'s text after the
    script's path, "" for none), the one it runs for each argument list a legs step passes it
    after `xargs <path>` (`xargs`, each the go test the packages xargs appends are appended to),
    and the SHA-256 of the text that reading was of."""

    module: str
    commands: dict[str, str]
    sha256: str
    xargs: dict[str, str] = field(default_factory=dict)


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

# envoy-go's unit step's -skip, as ci_job_reads.UNIT_TEST_SCRIPT writes it: the web display
# fixture's two tests, BACKEND_COUNT_TESTS and the rows whose bound is on time, which
# envoy-go-alone runs (ci_job_reads.UNIT_ALONE_TESTS). Which packages each leg runs is
# go_legs.py's, which the legs step's lister runs.
UNIT_SKIP = ("'^(TestABatchCutOffInFlightKeepsItsOrder|TestABucketWaitWhoseLookupsAreHeldFailsByNameInTime|"
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
    # whether it runs at all. `--count-table` runs every package its arguments name, in go list's
    # order, and counts the table's among them; it changes what is counted, never what runs. With
    # `--report FILE` it reads a report and runs no test. `-run=^(Name|...)$`, which a leg pipes in
    # with its package, adds that -run. envoy-go-race-api's legs and envoy-go's unit legs pass an
    # xargs argument list below, each leg's packages (and group) after it.
    "packages/envoy/scripts/database-skips.sh": AuditedScript(
        module="packages/envoy",
        commands={},
        xargs={
            f"--timeout 20m --skip {UNIT_SKIP} --count-table": f"go test -json -count=1 -skip {UNIT_SKIP} -timeout 20m",
            # envoy-go-race-api's legs: the API package, and the leg's group as `-run=^(Name|...)$`,
            # come from go_legs.py through xargs, so the command carries neither.
            "--var DISPATCH_TEST_DATABASE_URL --var BROKER_TEST_DATABASE_URL --race --timeout 70m":
                "go test -json -count=1 -race -timeout 70m",
        },
        sha256="2e47aa7c214764170c3fc2e1f479e84df5674d673013c2470856ce1dfa46d2cc",
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
    # It moves into packages/envoy/infra (`cd "$(dirname "$0")/.."`), takes the packages to run as
    # its arguments and refuses none, reads each one's -run from go-tagged-tests.sh's natslive list
    # before any run starts, then runs one `go test -count=1 -timeout 25m -tags natslive -run
    # <that package's list> <package>` per package, the three at once in the background, and waits
    # for each by process id. What it runs is the go tests below, one per package with its own
    # list, exactly as the step ran them one after another before it ran them at once; its output
    # files, its headings and its exit status carry no test. envoy-infra-go's natslive step passes
    # the argument list below (ci_job_reads.INFRA_NATSLIVE_PACKAGES), so a package added or dropped
    # moves the step's own run: line and this entry together.
    "packages/envoy/infra/scripts/natslive.sh": AuditedScript(
        module="packages/envoy/infra",
        commands={
            "./core ./seed ./gcp": "for pkg in ./core ./seed ./gcp; do\n"
                                   "  pattern=$(../../../.github/scripts/go-tagged-tests.sh natslive \"$pkg\")\n"
                                   "  go test -count=1 -timeout 25m -tags natslive -run \"$pattern\" \"$pkg\"\n"
                                   "done",
        },
        sha256="52fa27ca8f9e67984a32009b75d5215f10b40b3f7ef9d87b556cf37d16dff8ec",
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


def command_for(workdir: str, script: str) -> str | None:
    """The reading of what a run: in workdir runs, where that run: is one line naming an
    AUDITED_SCRIPTS script with an argument list its entry reads: the entry's `go test` text for
    those arguments. None where the run: names no such script, which is every other step, and
    None too where it names one in a form no entry reads - a second line, an operator, another
    argument list - so that a caller reads such a step from its run: text rather than from an
    entry that does not describe it. go-test-step-expansion.py's audited_step refuses that case
    by name, and it is the reader that holds every pair a step runs; a caller that only looks for
    `go test` lines leaves it to that one.
    """
    alone = script.strip(" \t\n")
    if "\n" in alone:
        return None
    spelled, _, arguments = alone.partition(" ")
    for path, entry in AUDITED_SCRIPTS.items():
        if workdir == entry.module and spelled in spellings(path, workdir):
            return entry.commands.get(arguments)
    return None


