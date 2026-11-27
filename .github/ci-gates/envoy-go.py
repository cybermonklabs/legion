# The envoy_go filter's gate: what job envoy-go reads. Its steps are in the jobs of the file it
# calls (ENVOY_GO_FILE): the unit step in the legs of envoy-go-legs, each over the packages
# .github/go-legs.yaml lists for it, the job's other steps beside them in envoy-go-alone,
# envoy-go-race-impossible, envoy-go-skips and envoy-go-natslive, and the PostgreSQL 16 job's step
# in envoy-go-postgres16; the job envoy-go itself only restates their result. So each citation
# names one of those jobs.
GATE = gate("envoy_go", "envoy-go", runs=[
    step_run(ENVOY_NATSLIVE_JOB, "Integration test envoy (a real NATS server, in the test process)",
             "go test ./internal/integration/ -timeout 300s -count=1", ENVOY, ["./internal/integration/"],
             workflow=ENVOY_GO_FILE),
    step_run(ENVOY_SKIPS_JOB, "Vet agentloop's live proof (no model call)", "go vet -tags modellive ./agentloop/", ENVOY,
             ["./agentloop/"], workflow=ENVOY_GO_FILE, tags="modellive", mode="vet"),
    step_run(ENVOY_SKIPS_JOB, "Vet agent-secrets' moto run (no moto)", "go vet -tags moto ./cmd/cipher/", ENVOY,
             ["./cmd/cipher/"], workflow=ENVOY_GO_FILE, tags="moto", mode="vet"),
    # ADOPTER_PACKAGES' tests, -v, after install-jj, in envoy-go-skips, built from the one list
    # that also puts the package with that job in the legs file (hold_envoy_legs).
    step_run(ENVOY_SKIPS_JOB, ADOPTER_STEP, ADOPTER_TEXT, ENVOY, ADOPTER_PKGS, workflow=ENVOY_GO_FILE),
    ceiling_run(ENVOY_LEGS_JOB, ENVOY_GO_FILE),
], exact=[
    # One go test per package of the plain run the race lane cannot make (RACE_IMPOSSIBLE_*),
    # each selecting that package's own tests with -run: the rows that measure no time here, and
    # those that do alone, in envoy-go-alone.
    pinned_step(ENVOY_RACE_IMPOSSIBLE_JOB, RACE_IMPOSSIBLE_STEP, ENVOY, RACE_IMPOSSIBLE_SCRIPT, [
        (ENVOY, [f"./{under}"], f"go test -run of what the race build cannot run in ./{under}", {})
        for under in RACE_IMPOSSIBLE_PACKAGES
    ], workflow=ENVOY_GO_FILE),
    pinned_step(ENVOY_ALONE_JOB, RACE_IMPOSSIBLE_ALONE_STEP, ENVOY, RACE_IMPOSSIBLE_ALONE_SCRIPT, [
        (ENVOY, [f"./{under}"], f"go test -run of what the race build cannot run in ./{under}, alone", {})
        for under in RACE_IMPOSSIBLE_ALONE_RUN
    ], workflow=ENVOY_GO_FILE),
    # The unit packages' rows whose bound is on time (UNIT_ALONE_TESTS), one go test -run per
    # package: the whole package compiles whichever tests the -run selects, so each Run is the
    # plain go test of its package.
    pinned_step(ENVOY_ALONE_JOB, UNIT_ALONE_STEP, ENVOY, UNIT_ALONE_SCRIPT, [
        (ENVOY, [f"./{package}/"], f"go test -parallel 1 -run of the rows of ./{package}/ whose bound is on time", {})
        for package in sorted(UNIT_ALONE_TESTS)
    ], workflow=ENVOY_GO_FILE),
    # The counter's test: its controls run database-skips.sh on DATABASE_SKIPS_TEST_PKGS, one
    # at a time, from packages/envoy (and the API package once more from a temporary checkout
    # of the same commit, which compiles the same files). Each run's -run moves no closure -
    # the whole package compiles whichever tests it selects - so each Run is the plain go test
    # of its package, which is why this job reads everything the API's tests read.
    pinned_step(ENVOY_SKIPS_JOB, DATABASE_SKIPS_TEST_STEP, ".", DATABASE_SKIPS_TEST, [
        (ENVOY, DATABASE_SKIPS_TEST_PKGS,
         "packages/envoy/scripts/database-skips.test.sh's runs of the counter on each package", {}),
    ], workflow=ENVOY_GO_FILE),
    # check-ci-filters.sh reduces only a one-line go test, so the legs' unit step is pinned whole:
    # its packages come from .github/scripts/go_legs.py through xargs into database-skips.sh
    # --count-table, which runs them in one go test -json and counts the skips of the broker's
    # tree, its command and the writer hold's client among them (DATABASE_SKIPS_PKGS less
    # DEFAULT_EXCEPT; hold_envoy_legs holds each of those to a leg here). The Run names every
    # package the unit legs run (ENVOY_UNIT_PACKAGES, from leg_run_packages): those go-legs.yaml
    # lists in them, and each package `go list ./...` prints that no place names, which leg 5, the
    # job `unlisted` names, runs - so a change to only such a package's test files starts this
    # job. Each leg runs the step over its own share, so the legs together compile and run those
    # packages, each once.
    # It names those and not the superset ./...: the module but the race lane's packages other
    # than those whose skips the legs count (the auth service's natscallout runs in a leg too),
    # internal/integration, which the integration step tests, the text checks, which the
    # text-check job runs, and the architecture-note and spec-file packages and
    # LISTENERWIRE_PACKAGES (listenerwire and internal/wirecheck), which envoy-go-build runs. A Run
    # saying more would credit this job with tests other jobs run, and the closure of ./... would
    # put their test files and the files only those tests read on this job's filter, so a change to
    # one of them would start a job that does not run the test it touches.
    # Its --skip takes WEB_FIXTURE_TESTS out of this step: without `--check`
    # TestWebDisplayFixture writes the committed web display fixture rather than checking it,
    # so a second run here asks nothing and can only pass, and
    # TestTheWebFixtureFindsItsFilesFromAnyDirectory stats that same file, a read this job
    # need not carry; the fixture step of envoy-go-build runs both, with the check that
    # makes the first a requirement. It also takes the backend count's tests of cmd/cipher
    # and internal/cipher/api out (BACKEND_COUNT_TESTS), which envoy-go-skips' backend count runs
    # in the same build and environment, and the rows whose bound is on time (UNIT_ALONE_TESTS),
    # which envoy-go-alone runs. The tests stay compiled into this step's binary, which is what a
    # skipped test is.
    pinned_step(ENVOY_LEGS_JOB, UNIT_TEST_STEP, ENVOY, UNIT_TEST_SCRIPT, [
        (ENVOY, [f"./{p}/" for p in ENVOY_UNIT_PACKAGES],
         f"database-skips.sh --count-table's go test -json -timeout {UNIT_TEST_TIMEOUT} over each unit leg's "
         f"packages of {GO_LEGS}", {}),
    ], workflow=ENVOY_GO_FILE),
    # Only what the natslive tag adds, one package at a time: the rig's own matrix, the one
    # test of internal/bus that needs a real server image, the auth service's check that the
    # deployed server lets an empty allow list through, which its guard refuses, and the edge
    # listener's grant and start under its rendered user. The rest of internal/testnats runs in a
    # unit leg; the rest of internal/bus runs in envoy-go-race, and the rest of
    # internal/cipher/natscallout there and in a unit leg, since the unit legs leave the race
    # lane's packages out but those whose skips they count, and the envoy_go entry of TAG_RUNS,
    # one entry per step, checks the testnats half alone.
    pinned_step(ENVOY_NATSLIVE_JOB, NATS_MATRIX_STEP, ENVOY, NATS_MATRIX_SCRIPT, [
        (ENVOY, ["./internal/testnats/"], "go test -tags natslive ./internal/testnats/", {"tags": "natslive"}),
        (ENVOY, ["./internal/bus/"], "go test -tags natslive ./internal/bus/", {"tags": "natslive"}),
        (ENVOY, ["./internal/cipher/natscallout/"], "go test -tags natslive ./internal/cipher/natscallout/", {"tags": "natslive"}),
        (ENVOY, ["./natsgrant/"], "go test -tags natslive ./natsgrant/", {"tags": "natslive"}),
        (ENVOY, ["./cmd/listener/"], "go test -tags natslive ./cmd/listener/", {"tags": "natslive"}),
    ], workflow=ENVOY_GO_FILE),
    # The PostgreSQL 16 job, a job of envoy-go's file beside the legs (POSTGRES16_JOB): its three
    # tests by name (POSTGRES16_TESTS). Its Run is the plain go test of the package, which compiles
    # every test file of internal/dispatch/delivery whichever three the -run selects, and which
    # envoy-go-alone's run of the package's xid window rows already puts on this gate's filter.
    pinned_step(POSTGRES16_JOB, POSTGRES16_STEP, ENVOY, POSTGRES16_SCRIPT, [
        (ENVOY, [POSTGRES16_PACKAGE], f"go test -run of three tests in {POSTGRES16_PACKAGE} on PostgreSQL 16", {}),
    ], workflow=ENVOY_GO_FILE),
], reads=reads(
    (".github/actions/pull-test-images/**", at(ENVOY_LEGS_JOB, "Pull test container images", ENVOY_GO_FILE)),
    (TAGGED_TESTS, f"{at(ENVOY_NATSLIVE_JOB, NATS_MATRIX_STEP, ENVOY_GO_FILE)} takes its -run from it"),
    (DATABASE_SKIPS_SCRIPT, f"{at(ENVOY_LEGS_JOB, UNIT_TEST_STEP, ENVOY_GO_FILE)} runs it, and "
                            f"{at(ENVOY_SKIPS_JOB, DATABASE_SKIPS_TEST_STEP, ENVOY_GO_FILE)} runs it and copies it"),
    (DATABASE_SKIPS_TEST, f"{at(ENVOY_SKIPS_JOB, DATABASE_SKIPS_TEST_STEP, ENVOY_GO_FILE)} runs it"),
    (GO_LEGS_SCRIPT, at(ENVOY_LEGS_JOB, UNIT_TEST_STEP, ENVOY_GO_FILE)),
    (GO_LEGS, f"{GO_LEGS_SCRIPT} reads each leg's packages from it"),
) + ceiling_reads(ENVOY_LEGS_JOB, ENVOY_GO_FILE) + service_settings_reads(ENVOY_LEGS_JOB, ENVOY_GO_FILE)
  + install_jj(ENVOY_SKIPS_JOB, ENVOY_GO_FILE)
  + setup_go(ENVOY_LEGS_JOB, f"{ENVOY}/go.mod", ENVOY_GO_FILE) + bun_job(ENVOY_LEGS_JOB, ENVOY_GO_FILE)
  + setup_go(POSTGRES16_JOB, f"{ENVOY}/go.mod", ENVOY_GO_FILE)
  + service_settings_reads(POSTGRES16_JOB, ENVOY_GO_FILE))
GATE = with_reads(GATE, [
    Read('packages/envoy/**', "filter envoy_go held packages/envoy/** by hand before the filters were computed: it selected 728 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
