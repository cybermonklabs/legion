# The envoy_go filter's gate: what job envoy-go reads.
GATE = gate("envoy_go", "envoy-go", runs=[
    step_run("envoy-go", "Integration test envoy (a real NATS server, in the test process)",
             "go test ./internal/integration/ -timeout 300s -count=1", ENVOY, ["./internal/integration/"]),
    step_run("envoy-go", "Vet agentloop's live proof (no model call)", "go vet -tags modellive ./agentloop/", ENVOY,
             ["./agentloop/"], tags="modellive", mode="vet"),
    step_run("envoy-go", "Vet agent-secrets' moto run (no moto)", "go vet -tags moto ./cmd/cipher/", ENVOY,
             ["./cmd/cipher/"], tags="moto", mode="vet"),
    ceiling_run("envoy-go"),
], exact=[
    # One go test per package of the plain run the race lane cannot make (RACE_IMPOSSIBLE_*),
    # each selecting that package's own tests with -run.
    pinned_step("envoy-go", RACE_IMPOSSIBLE_STEP, ENVOY, RACE_IMPOSSIBLE_SCRIPT, [
        (ENVOY, [f"./{under}"], f"go test -run of what the race build cannot run in ./{under}", {})
        for under in RACE_IMPOSSIBLE_PACKAGES
    ]),
    # The counter's test: its controls run database-skips.sh on DATABASE_SKIPS_TEST_PKGS, one
    # at a time, from packages/envoy (and the API package once more from a temporary checkout
    # of the same commit, which compiles the same files). Each run's -run moves no closure -
    # the whole package compiles whichever tests it selects - so each Run is the plain go test
    # of its package, which is why this job reads everything the API's tests read.
    pinned_step("envoy-go", DATABASE_SKIPS_TEST_STEP, ".", DATABASE_SKIPS_TEST, [
        (ENVOY, DATABASE_SKIPS_TEST_PKGS,
         "packages/envoy/scripts/database-skips.test.sh's runs of the counter on each package", {}),
    ]),
    # The module but the race lane's packages other than those under the broker's tree whose
    # skips the step counts (the auth service's natscallout runs here), internal/integration,
    # which the integration step tests, the text checks, which the text-check job runs, and the
    # architecture-note and spec-file packages and LISTENERWIRE_PACKAGES (listenerwire and
    # internal/wirecheck), which envoy-go-build runs, in one go test -json through
    # database-skips.sh, which counts the skips of the broker's tree, its command and the writer
    # hold's client (DATABASE_SKIPS_PKGS less DEFAULT_EXCEPT, all kept by the pattern:
    # hold_counted_in_unit_run()).
    # The Run names the packages the step's own --module-except keeps (packages_kept over
    # UNIT_TEST_PATTERN), so it is read from the pattern the step runs rather than claimed
    # beside it. A Run saying more would credit this job with tests other jobs run: the race
    # lane's are envoy-go-race's, envoy-go-race-api's and envoy-go-race-docs', the text checks
    # are the text-check job's, those packages are envoy-go-build's, and the closure of ./...
    # would put their test files and the files only those tests read on this job's filter, so a
    # change to one of them would start a job that does not run the test it touches.
    # Its --skip takes WEB_FIXTURE_TESTS out of this step: without `--check`
    # TestWebDisplayFixture writes the committed web display fixture rather than checking it,
    # so a second run here asks nothing and can only pass, and
    # TestTheWebFixtureFindsItsFilesFromAnyDirectory stats that same file, a read this job
    # need not carry; the fixture step of envoy-go-build runs both, with the check that
    # makes the first a requirement. It also takes the backend count's tests of cmd/cipher
    # and internal/cipher/api out (BACKEND_COUNT_TESTS), which the backend count runs in this
    # same job and environment. The tests stay compiled into this step's binary, which is what
    # a skipped test is.
    pinned_step("envoy-go", UNIT_TEST_STEP, ".", UNIT_TEST_RUN, [
        (ENVOY, packages_kept(ENVOY, UNIT_TEST_PATTERN),
         f"packages/envoy/scripts/database-skips.sh's go test -json -timeout {UNIT_TEST_TIMEOUT} of the "
         "packages its --module-except keeps", {}),
    ]),
    # Only what the natslive tag adds, one package at a time: the rig's own matrix, the one
    # test of internal/bus that needs a real server image, the auth service's check that the
    # deployed server lets an empty allow list through, which its guard refuses, and the edge
    # listener's grant and start under its rendered user. The rest of internal/testnats runs in
    # the unit step; the rest of internal/bus runs in envoy-go-race, and the rest of
    # internal/cipher/natscallout there and in the unit step above, since UNIT_TEST_PATTERN
    # leaves the race lane's packages out of the unit step but those whose skips it counts,
    # and the envoy_go entry of TAG_RUNS, one entry per step, checks the testnats half alone.
    pinned_step("envoy-go", NATS_MATRIX_STEP, ENVOY, NATS_MATRIX_SCRIPT, [
        (ENVOY, ["./internal/testnats/"], "go test -tags natslive ./internal/testnats/", {"tags": "natslive"}),
        (ENVOY, ["./internal/bus/"], "go test -tags natslive ./internal/bus/", {"tags": "natslive"}),
        (ENVOY, ["./internal/cipher/natscallout/"], "go test -tags natslive ./internal/cipher/natscallout/", {"tags": "natslive"}),
        (ENVOY, ["./natsgrant/"], "go test -tags natslive ./natsgrant/", {"tags": "natslive"}),
        (ENVOY, ["./cmd/listener/"], "go test -tags natslive ./cmd/listener/", {"tags": "natslive"}),
    ]),
    # The PostgreSQL 16 job, the second job of envoy-go's file (POSTGRES16_JOB): its three tests
    # by name (POSTGRES16_TESTS). Its Run is the plain go test of the package, which compiles
    # every test file of internal/dispatch/delivery whichever three the -run selects, and
    # which the unit step's run of the package already puts on this gate's filter.
    pinned_step(POSTGRES16_JOB, POSTGRES16_STEP, ENVOY, POSTGRES16_SCRIPT, [
        (ENVOY, [POSTGRES16_PACKAGE], f"go test -run of three tests in {POSTGRES16_PACKAGE} on PostgreSQL 16", {}),
    ], workflow=job_file("envoy-go")),
], reads=reads(
    (".github/actions/pull-test-images/**", at("envoy-go", "Pull test container images")),
    (TAGGED_TESTS, f"{at('envoy-go', NATS_MATRIX_STEP)} takes its -run from it"),
    (DATABASE_SKIPS_SCRIPT, f"{at('envoy-go', UNIT_TEST_STEP)} runs it, and "
                            f"{at('envoy-go', DATABASE_SKIPS_TEST_STEP)} runs it and copies it"),
    (DATABASE_SKIPS_TEST, f"{at('envoy-go', DATABASE_SKIPS_TEST_STEP)} runs it"),
) + ceiling_reads("envoy-go") + crash_safety_reads("envoy-go") + setup_go("envoy-go", f"{ENVOY}/go.mod")
  + setup_go(POSTGRES16_JOB, f"{ENVOY}/go.mod", workflow=job_file("envoy-go")) + bun_job("envoy-go"))
GATE = with_reads(GATE, [
    Read('packages/envoy/**', "filter envoy_go held packages/envoy/** by hand before the filters were computed: it selected 728 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
