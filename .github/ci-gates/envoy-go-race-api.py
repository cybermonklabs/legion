# The envoy_go_race_api filter's gate: what job envoy-go-race-api reads.
GATE = gate("envoy_go_race_api", "envoy-go-race-api", runs=[ceiling_run("envoy-go-race-api")], exact=[
    pinned_step("envoy-go-race-api", RACE_API.step, ENVOY, RACE_API_SCRIPT, [
        (ENVOY, race_pkgs(RACE_API),
         f"{DATABASE_SKIPS_SCRIPT}'s go test -json -race -timeout {RACE_API.timeout} of the package", {"race": True}),
    ]),
], reads=reads(
    (DATABASE_SKIPS_SCRIPT, f"{at('envoy-go-race-api', RACE_API.step)} runs the package through it"),
) + ceiling_reads("envoy-go-race-api") + service_settings_reads("envoy-go-race-api") + setup_go("envoy-go-race-api", f"{ENVOY}/go.mod")
  + go_modules("envoy-go-race-api"))
