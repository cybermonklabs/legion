# The lane's three jobs: each step's text and packages come from ENVOY_RACE_LANE, which the
# envoy-go unit step and its plain companion are built from too. Two steps are not race_step's
# text, and each gate pins its whole line: envoy-go-race-api's runs its package through
# database-skips.sh (RACE_API_SCRIPT), which counts that run's skips, and envoy-go-race-docs'
# runs the package under .github/scripts/race_memory.py
# (dispatch://INFRA-1085), so its gate pins that whole line while the lane table still names
# the package, which is what keeps it out of the unit step.
GATE = gate("envoy_go_race", "envoy-go-race",
     runs=[race_step(RACE_JOBS["envoy-go-race"]), ceiling_run("envoy-go-race")]
     + [forced_order_run("envoy-go-race", ENVOY_FORCED_ORDER_TEST_STEP, ENVOY, ENVOY_FORCED_ORDER_TEST_PKGS)]
     + [forced_order_run("envoy-go-race", envoy_forced_order_step(server), ENVOY, pkgs)
        for server, pkgs in ENVOY_FORCED_ORDER_PKGS.items()],
     reads=ceiling_reads("envoy-go-race") + crash_safety_reads("envoy-go-race")
     + reads((FORCED_ORDER_TEST, f"{at('envoy-go-race', ENVOY_FORCED_ORDER_TEST_STEP)} runs it"))
     + forced_order_reads("envoy-go-race", ENVOY_FORCED_ORDER_TEST_STEP)
     + [read for server in ENVOY_FORCED_ORDER_PKGS
        for read in forced_order_reads("envoy-go-race", envoy_forced_order_step(server))]
     + setup_go("envoy-go-race", f"{ENVOY}/go.mod"))
GATE = with_reads(GATE, [
    Read('packages/envoy/**', "filter envoy_go_race held packages/envoy/** by hand before the filters were computed: it selected 1609 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
