# Every step here is in the legs job of the file envoy-go-race-docs calls, whose id is not ci.yaml's, so each
# citation names that file.
ENVOY_GO_RACE_DOCS_FILE = job_file("envoy-go-race-docs")
# The envoy_go_race_docs filter's gate: what job envoy-go-race-docs reads.
# The step runs in three legs (RACE_DOCS_LEGS_JOB), each over the group of the package's tests
# .github/go-legs/packages/envoy/internal/dispatch/docs.yaml gives it; the legs together run the
# package once, so what this Run claims is the package.
# why: .github/ci-notes/ci-job-envoy-go-race-docs.md#why-the-document-services-tests-run-in-three-legs
GATE = gate("envoy_go_race_docs", "envoy-go-race-docs", runs=[ceiling_run(RACE_DOCS_LEGS_JOB, ENVOY_GO_RACE_DOCS_FILE)], exact=[
    pinned_step(RACE_DOCS_LEGS_JOB, RACE_DOCS_STEP, ENVOY, RACE_DOCS_SCRIPT, [
        (ENVOY, ["./internal/dispatch/docs/"], "go test -race -count=1 -timeout 60m ./internal/dispatch/docs/", {"race": True}),
    ], workflow=ENVOY_GO_RACE_DOCS_FILE),
], reads=reads(
    (".github/scripts/race_memory.py", f"{at(RACE_DOCS_LEGS_JOB, RACE_DOCS_STEP, ENVOY_GO_RACE_DOCS_FILE)} runs the package under it"),
    (GO_LEGS_SCRIPT, f"{at(RACE_DOCS_LEGS_JOB, RACE_DOCS_STEP, ENVOY_GO_RACE_DOCS_FILE)} runs it for the leg's group and package"),
    (GO_LEGS, f"{GO_LEGS_SCRIPT} reads each leg's packages from it"),
    (f"{GO_LEGS_GROUPS}/**", f"{GO_LEGS_SCRIPT} reads the groups of a split package's tests from it"),
) + ceiling_reads(RACE_DOCS_LEGS_JOB, ENVOY_GO_RACE_DOCS_FILE) + service_settings_reads(RACE_DOCS_LEGS_JOB, ENVOY_GO_RACE_DOCS_FILE)
  + setup_go(RACE_DOCS_LEGS_JOB, f"{ENVOY}/go.mod", ENVOY_GO_RACE_DOCS_FILE)
  + go_modules(RACE_DOCS_LEGS_JOB, ENVOY_GO_RACE_DOCS_FILE))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/pmdoc/schema/**', "filter envoy_go_race_docs held packages/envoy/internal/dispatch/pmdoc/schema/** by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares"),
    Read('packages/envoy/internal/dispatch/pmdoc/testdata/**', "filter envoy_go_race_docs held packages/envoy/internal/dispatch/pmdoc/testdata/** by hand before the filters were computed: it selected 259 file(s) no other read of this gate declares"),
])
