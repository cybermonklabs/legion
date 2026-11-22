# The envoy_infra_go filter's gate: what job envoy-infra-go reads.
GATE = gate("envoy_infra_go", "envoy-infra-go", runs=[
    step_run("envoy-infra-go", "Vet envoy-infra, its live-test tags included", "go vet -tags pulumilive,natslive ./...", INFRA, ["./..."],
             tags="pulumilive,natslive", mode="vet"),
], exact=[
    # The module but the text checks, which the text-check job runs: the step runs
    # INFRA_RACE_UNIT_SCRIPT, which lists the packages, drops that one and runs go test -race
    # over the rest, and is pinned whole as the other two lanes' plain steps are; the script's
    # pattern is held to INFRA_PLAIN_PATTERN above. The vet step above still compiles the text
    # checks, and must: vetting them keeps them compiling, and a vet run reads none of the
    # files a test of theirs reads.
    pinned_step("envoy-infra-go", INFRA_PLAIN_STEP, INFRA, INFRA_PLAIN_SCRIPT, [
        (INFRA, packages_kept(INFRA, INFRA_PLAIN_PATTERN),
         f"{INFRA_RACE_UNIT_SCRIPT}'s go test -count=1 -race over the packages its EXCLUDE keeps",
         {"race": True}),
    ]),
    # One go test per package, each running only what the natslive tag adds to it.
    pinned_step("envoy-infra-go", INFRA_NATSLIVE_STEP, INFRA, INFRA_NATSLIVE_SCRIPT, [
        (INFRA, [pkg], f"go test -tags natslive {pkg}", {"tags": "natslive"})
        for pkg in INFRA_NATSLIVE_PACKAGES
    ]),
    darwin_vet("envoy-infra-go", INFRA_DARWIN_VET, INFRA),
], reads=reads(
    (f"{INFRA}/**", f"{at('envoy-infra-go', 'Check Go formatting')}: gofmt -l . in packages/envoy/infra reads every .go file under it"),
    (".github/actions/pull-test-images/**", at("envoy-infra-go", "Pull the NATS and Postgres test container images")),
    (f"{ENVOY}/internal/testnats/nats.go", f"{at('envoy-infra-go', 'Pull the NATS and Postgres test container images')}: consts Image, HubImage"),
    (f"{ENVOY}/internal/testpg/testpg.go", f"{at('envoy-infra-go', 'Pull the NATS and Postgres test container images')}: const Image"),
    (TAGGED_TESTS, f"{at('envoy-infra-go', INFRA_NATSLIVE_STEP)} takes each call's -run from it"),
) + setup_go("envoy-infra-go", f"{INFRA}/go.mod") + darwin_vet_reads("envoy-infra-go", INFRA_DARWIN_VET, INFRA))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/delivery/**', "filter envoy_infra_go held packages/envoy/internal/dispatch/delivery/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter envoy_infra_go held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 278 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/contracts/**', "filter envoy_infra_go held packages/envoy/internal/contracts/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
