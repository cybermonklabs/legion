# TestAReadingIsAChildNodeOfTheDecisionItReads runs this tree's dispatch command under bun, so
# the job installs Bun and the workspace, and reads bin/dispatch.ts and its imports:
# envoy-client's sources and broker-client's with them, which are reads here, and some of
# contracts' modules, which are not: ci_ts_programs.py's BUNDLES builds that entry for this
# filter too, and its LISTED row adds the contracts modules the bundle loads to the filter the
# changes job computes, as it does for dispatch_two_builds_go's.
GATE = gate("envoy_go_race_api", "envoy-go-race-api", runs=[ceiling_run("envoy-go-race-api")], exact=[
    pinned_step("envoy-go-race-api", RACE_API.step, ENVOY, RACE_API_SCRIPT, [
        (ENVOY, race_pkgs(RACE_API),
         f"{DATABASE_SKIPS_SCRIPT}'s go test -json -race -timeout {RACE_API.timeout} of the package", {"race": True}),
    ]),
], reads=reads(
    (DATABASE_SKIPS_SCRIPT, f"{at('envoy-go-race-api', RACE_API.step)} runs the package through it"),
    ("packages/envoy-client/bin/dispatch.ts",
     f"{ENVOY}/internal/dispatch/api/decision_entries_test.go's runDispatchCLI runs "
     "`bun ../../../../envoy-client/bin/dispatch.ts`, this tree's dispatch command"),
) + envoy_client_dep(f"packages/envoy-client/bin/dispatch.ts, which {ENVOY}/internal/dispatch/api/decision_entries_test.go's "
                     "runDispatchCLI runs, imports src/dispatch-cli.ts")
  + ceiling_reads("envoy-go-race-api") + setup_go("envoy-go-race-api", f"{ENVOY}/go.mod") + bun_job("envoy-go-race-api"))
