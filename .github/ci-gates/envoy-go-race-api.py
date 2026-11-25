# Every step here is in the legs job of the file envoy-go-race-api calls, whose id is not ci.yaml's, so each
# citation names that file.
ENVOY_GO_RACE_API_FILE = job_file("envoy-go-race-api")
# The envoy_go_race_api filter's gate: what job envoy-go-race-api reads.
# The step runs in three legs (RACE_API_LEGS_JOB), each over the group of the package's tests
# .github/go-legs/packages/envoy/internal/dispatch/api.yaml gives it; the legs together run the
# package once, so what this Run claims is the package.
# why: .github/ci-notes/ci-job-envoy-go-race-api.md#why-the-apis-tests-run-in-three-legs
GATE = gate("envoy_go_race_api", "envoy-go-race-api", runs=[ceiling_run(RACE_API_LEGS_JOB, ENVOY_GO_RACE_API_FILE)], exact=[
    pinned_step(RACE_API_LEGS_JOB, RACE_API.step, ENVOY, RACE_API_SCRIPT, [
        (ENVOY, race_pkgs(RACE_API),
         f"{DATABASE_SKIPS_SCRIPT}'s go test -json -race -timeout {RACE_API.timeout} of the package", {"race": True}),
    ], workflow=ENVOY_GO_RACE_API_FILE),
], reads=reads(
    (DATABASE_SKIPS_SCRIPT, f"{at(RACE_API_LEGS_JOB, RACE_API.step, ENVOY_GO_RACE_API_FILE)} runs the package through it"),
    (GO_LEGS_SCRIPT, f"{at(RACE_API_LEGS_JOB, RACE_API.step, ENVOY_GO_RACE_API_FILE)} runs it for the leg's group and package"),
    (GO_LEGS, f"{GO_LEGS_SCRIPT} reads each leg's packages from it"),
    (f"{GO_LEGS_GROUPS}/**", f"{GO_LEGS_SCRIPT} reads the groups of a split package's tests from it"),
) + ceiling_reads(RACE_API_LEGS_JOB, ENVOY_GO_RACE_API_FILE) + crash_safety_reads(RACE_API_LEGS_JOB, ENVOY_GO_RACE_API_FILE)
  + setup_go(RACE_API_LEGS_JOB, f"{ENVOY}/go.mod", ENVOY_GO_RACE_API_FILE))
