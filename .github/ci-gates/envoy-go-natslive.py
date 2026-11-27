# The envoy_go_natslive filter's gate: what job envoy-go-natslive reads, the container-backed NATS
# matrix, the integration suite and the callout check. They ran beside envoy-go's unit legs, under
# the envoy_go filter, until the merge group stopped running envoy-go's package tests; this job
# runs in the merge group and on every pull request its paths select
# (.github/ci-notes/ci.md#what-the-merge-group-runs-and-what-only-a-pull-request-runs).
GATE = gate("envoy_go_natslive", "envoy-go-natslive", runs=[
    step_run(ENVOY_NATSLIVE_JOB, "Integration test envoy (a real NATS server, in the test process)",
             "go test ./internal/integration/ -timeout 300s -count=1", ENVOY, ["./internal/integration/"]),
    ceiling_run(ENVOY_NATSLIVE_JOB),
], exact=[
    # Only what the natslive tag adds, one package at a time: the rig's own matrix, the one
    # test of internal/bus that needs a real server image, the auth service's check that the
    # deployed server lets an empty allow list through, which its guard refuses, and the edge
    # listener's grant and start under its rendered user. The rest of internal/testnats runs in a
    # unit leg of envoy-go; the rest of internal/bus runs in envoy-go-race, and the rest of
    # internal/cipher/natscallout there and in a unit leg, since the unit legs leave the race
    # lane's packages out but those whose skips they count, and the envoy_go_natslive entry of
    # TAG_RUNS, one entry per step, checks the testnats half alone.
    pinned_step(ENVOY_NATSLIVE_JOB, NATS_MATRIX_STEP, ENVOY, NATS_MATRIX_SCRIPT, [
        (ENVOY, ["./internal/testnats/"], "go test -tags natslive ./internal/testnats/", {"tags": "natslive"}),
        (ENVOY, ["./internal/bus/"], "go test -tags natslive ./internal/bus/", {"tags": "natslive"}),
        (ENVOY, ["./internal/cipher/natscallout/"], "go test -tags natslive ./internal/cipher/natscallout/", {"tags": "natslive"}),
        (ENVOY, ["./natsgrant/"], "go test -tags natslive ./natsgrant/", {"tags": "natslive"}),
        (ENVOY, ["./cmd/listener/"], "go test -tags natslive ./cmd/listener/", {"tags": "natslive"}),
    ]),
], reads=reads(
    (".github/actions/pull-test-images/**", at(ENVOY_NATSLIVE_JOB, "Pull test container images")),
    (TAGGED_TESTS, f"{at(ENVOY_NATSLIVE_JOB, NATS_MATRIX_STEP)} takes its -run from it"),
) + ceiling_reads(ENVOY_NATSLIVE_JOB) + service_settings_reads(ENVOY_NATSLIVE_JOB)
  + setup_go(ENVOY_NATSLIVE_JOB, f"{ENVOY}/go.mod") + bun_job(ENVOY_NATSLIVE_JOB))
GATE = with_reads(GATE, [
    Read('packages/envoy/**', "callout-check.sh builds and starts the brokers, the listener, Dispatch and "
                              "nats-server through dev-stack.sh --callout from the module's sources, which no "
                              "go command of this gate lists; envoy-go held it so before the job was its own",
         but=('**/*_test.go',)),
])
