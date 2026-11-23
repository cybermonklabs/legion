# The envoy-infra module's format check, its vets as linux and as darwin compile it, and its
# unit tests under the race detector: the half of the old envoy-infra-go job a pull request
# runs (ci-job-envoy-infra-go.yaml's envoy-infra-build job).
GATE = gate("envoy_infra_build", "envoy-infra-build", runs=[
    step_run("envoy-infra-build", "Vet envoy-infra, its live-test tags included", "go vet -tags pulumilive,natslive ./...", INFRA, ["./..."],
             tags="pulumilive,natslive", mode="vet"),
], exact=[
    # The module but the text checks, which the text-check job runs: the step runs
    # INFRA_RACE_UNIT_SCRIPT, which lists the packages, drops that one and runs go test -race
    # over the rest, and is pinned whole as the other two lanes' plain steps are; the script's
    # pattern is held to INFRA_PLAIN_PATTERN above. The vet step above still compiles the text
    # checks, and must: vetting them keeps them compiling, and a vet run reads none of the
    # files a test of theirs reads.
    pinned_step("envoy-infra-build", INFRA_PLAIN_STEP, INFRA, INFRA_PLAIN_SCRIPT, [
        (INFRA, packages_kept(INFRA, INFRA_PLAIN_PATTERN),
         f"{INFRA_RACE_UNIT_SCRIPT}'s go test -count=1 -race over the packages its EXCLUDE keeps",
         {"race": True}),
    ]),
    darwin_vet("envoy-infra-build", INFRA_DARWIN_VET, INFRA),
], reads=reads(
    (f"{INFRA}/**", f"{at('envoy-infra-build', 'Check Go formatting')}: gofmt -l . in packages/envoy/infra reads every .go file under it"),
) + setup_go("envoy-infra-build", f"{INFRA}/go.mod") + darwin_vet_reads("envoy-infra-build", INFRA_DARWIN_VET, INFRA))
