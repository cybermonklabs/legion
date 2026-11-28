# The lane's three jobs: each step's text and packages come from ENVOY_RACE_LANE, which the
# envoy-go unit legs and its plain companion are built from or held to too. No step is a plain
# go test line any more, and each gate pins its whole line: envoy-go-race's runs in the legs of
# envoy-go-race-legs, each over the packages .github/go-legs.yaml lists for it (ENVOY_RACE_SCRIPT,
# whose packages hold_envoy_legs holds to the lane's), the forced-order steps in the last leg;
# envoy-go-race-api's runs its package through database-skips.sh (RACE_API_SCRIPT), which counts
# that run's skips, and envoy-go-race-docs' runs the package under .github/scripts/race_memory.py
# (dispatch://INFRA-1085), so its gate pins that whole line while the lane table still names
# the package, which is what keeps it out of the unit legs. The job envoy-go-race itself only
# restates its legs' result, so each citation names envoy-go-race-legs (ENVOY_RACE_FILE).
GATE = gate("envoy_go_race", "envoy-go-race",
     runs=[ceiling_run(ENVOY_RACE_LEGS_JOB, ENVOY_RACE_FILE)]
     + [forced_order_run(ENVOY_RACE_LEGS_JOB, ENVOY_FORCED_ORDER_TEST_STEP, ENVOY, ENVOY_FORCED_ORDER_TEST_PKGS,
                         ENVOY_RACE_FILE)]
     + [forced_order_run(ENVOY_RACE_LEGS_JOB, envoy_forced_order_step(server), ENVOY, pkgs, ENVOY_RACE_FILE)
        for server, pkgs in ENVOY_FORCED_ORDER_PKGS.items()],
     exact=[
         # check-ci-filters.sh reduces only a one-line go test, so the legs' race step is pinned
         # whole: its packages come from .github/scripts/go_legs.py through xargs. The Run names
         # every package of every race leg, as go-legs.yaml lists them: each leg runs the step over
         # its own share, so the legs together compile and run those packages, each once.
         pinned_step(ENVOY_RACE_LEGS_JOB, ENVOY_RACE_STEP, ENVOY, ENVOY_RACE_SCRIPT, [
             (ENVOY, [f"./{p}/" for p in ENVOY_RACE_LEG_PACKAGES],
              f"go test -race over each race leg's packages of {GO_LEGS}", {"race": True}),
         ], workflow=ENVOY_RACE_FILE),
     ],
     reads=ceiling_reads(ENVOY_RACE_LEGS_JOB, ENVOY_RACE_FILE) + service_settings_reads(ENVOY_RACE_LEGS_JOB, ENVOY_RACE_FILE)
     + reads((FORCED_ORDER_TEST, f"{at(ENVOY_RACE_LEGS_JOB, ENVOY_FORCED_ORDER_TEST_STEP, ENVOY_RACE_FILE)} runs it"),
             (GO_LEGS_SCRIPT, at(ENVOY_RACE_LEGS_JOB, ENVOY_RACE_STEP, ENVOY_RACE_FILE)),
             (GO_LEGS, f"{GO_LEGS_SCRIPT} reads each leg's packages from it"),
             (f"{GO_LEGS_GROUPS}/**", f"{GO_LEGS_SCRIPT} reads the groups of a split package's tests from "
                                      f"{GO_LEGS_GROUPS}/<module>/<package>.yaml, one per leg that runs a share of it "
                                      f"({at(ENVOY_RACE_LEGS_JOB, ENVOY_RACE_STEP, ENVOY_RACE_FILE)}, cmd/listener)"))
     + forced_order_reads(ENVOY_RACE_LEGS_JOB, ENVOY_FORCED_ORDER_TEST_STEP, ENVOY_RACE_FILE)
     + [read for server in ENVOY_FORCED_ORDER_PKGS
        for read in forced_order_reads(ENVOY_RACE_LEGS_JOB, envoy_forced_order_step(server), ENVOY_RACE_FILE)]
     + setup_go(ENVOY_RACE_LEGS_JOB, f"{ENVOY}/go.mod", ENVOY_RACE_FILE) + go_test_runner(ENVOY_RACE_LEGS_JOB, ENVOY_RACE_FILE))
GATE = with_reads(GATE, [
    Read('packages/envoy/**', "filter envoy_go_race held packages/envoy/** by hand before the filters were computed: it selected 1609 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
