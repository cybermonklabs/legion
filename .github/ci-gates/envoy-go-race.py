# The lane's jobs: each step's text and packages come from ENVOY_RACE_LANE, which the envoy-go unit
# step and its plain companion are built from too. envoy-go-race's file runs two jobs, each on a
# runner of its own: envoy-go-race and envoy-go-race-store, the Dispatch store alone (RACE_STORE),
# whose race step is this gate's second Run, since ci.yaml's job of this filter calls that file and
# both jobs run under it. Three steps are not race_step's text, and each gate pins its whole line:
# envoy-go-race's counted step runs the NATS auth service through database-skips.sh
# (RACE_COUNTED_SCRIPT) and envoy-go-race-api's its package (RACE_API_SCRIPT), each counting that
# run's skips, and envoy-go-race-docs' runs the package under .github/scripts/race_memory.py
# (dispatch://INFRA-1085), so its gate pins that whole line while the lane table still names
# the package, which is what keeps it out of the unit step.
GATE = gate("envoy_go_race", "envoy-go-race",
     runs=[race_step(RACE), race_step(RACE_STORE), ceiling_run("envoy-go-race")]
     + [forced_order_run("envoy-go-race", ENVOY_FORCED_ORDER_TEST_STEP, ENVOY, ENVOY_FORCED_ORDER_TEST_PKGS)]
     + [forced_order_run("envoy-go-race", envoy_forced_order_step(server), ENVOY, pkgs)
        for server, pkgs in ENVOY_FORCED_ORDER_PKGS.items()],
     exact=[
         pinned_step("envoy-go-race", RACE_COUNTED_STEP, ENVOY, RACE_COUNTED_SCRIPT, [
             (ENVOY, race_pkgs(RACE, counted=True),
              f"{DATABASE_SKIPS_SCRIPT}'s go test -json -race -timeout {RACE_COUNTED_TIMEOUT} of the package", {"race": True}),
         ]),
     ],
     reads=reads((DATABASE_SKIPS_SCRIPT, f"{at('envoy-go-race', RACE_COUNTED_STEP)} runs the package through it"))
     + ceiling_reads("envoy-go-race") + service_settings_reads("envoy-go-race")
     + reads((FORCED_ORDER_TEST, f"{at('envoy-go-race', ENVOY_FORCED_ORDER_TEST_STEP)} runs it"))
     + forced_order_reads("envoy-go-race", ENVOY_FORCED_ORDER_TEST_STEP)
     + [read for server in ENVOY_FORCED_ORDER_PKGS
        for read in forced_order_reads("envoy-go-race", envoy_forced_order_step(server))]
     + setup_go("envoy-go-race", f"{ENVOY}/go.mod"))
GATE = with_reads(GATE, [
    Read('packages/envoy/**', "filter envoy_go_race held packages/envoy/** by hand before the filters were computed: it selected 1609 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
