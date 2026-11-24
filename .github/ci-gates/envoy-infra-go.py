# The envoy-infra module's live tests against nats-server and Postgres, the merge-queue half
# of the old envoy-infra-go job; envoy_infra_build (envoy-infra-build.py) is the half a pull request runs.
GATE = gate("envoy_infra_go", "envoy-infra-go", exact=[
    # One go test per package, each running only what the natslive tag adds to it: the
    # untagged tests of the three are envoy-infra-build's unit step's (TAG_RUNS).
    pinned_step("envoy-infra-go", INFRA_NATSLIVE_STEP, INFRA, INFRA_NATSLIVE_SCRIPT, [
        (INFRA, [pkg], f"go test -tags natslive {pkg}", {"tags": "natslive"})
        for pkg in INFRA_NATSLIVE_PACKAGES
    ]),
], reads=reads(
    (".github/actions/pull-test-images/**", at("envoy-infra-go", "Pull the NATS and Postgres test container images")),
    (f"{ENVOY}/internal/testnats/nats.go", f"{at('envoy-infra-go', 'Pull the NATS and Postgres test container images')}: consts Image, HubImage"),
    (f"{ENVOY}/internal/testpg/testpg.go", f"{at('envoy-infra-go', 'Pull the NATS and Postgres test container images')}: const Image"),
    (TAGGED_TESTS, f"{at('envoy-infra-go', INFRA_NATSLIVE_STEP)} takes each call's -run from it"),
) + setup_go("envoy-infra-go", f"{INFRA}/go.mod"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/delivery/**', "filter envoy_infra_go held packages/envoy/internal/dispatch/delivery/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter envoy_infra_go held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 278 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/contracts/**', "filter envoy_infra_go held packages/envoy/internal/contracts/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/infra/**', "filter envoy_infra_go holds packages/envoy/infra/** by hand: its gofmt step moved to envoy-infra-build, and the filter kept the module; it selects 19 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
