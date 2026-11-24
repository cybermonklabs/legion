# The envoy_go_race_docs filter's gate: what job envoy-go-race-docs reads.
GATE = gate("envoy_go_race_docs", "envoy-go-race-docs", runs=[ceiling_run("envoy-go-race-docs")], exact=[
    pinned_step("envoy-go-race-docs", RACE_DOCS_STEP, ENVOY, RACE_DOCS_SCRIPT, [
        (ENVOY, ["./internal/dispatch/docs/"], "go test -race -count=1 -timeout 60m ./internal/dispatch/docs/", {"race": True}),
    ]),
], reads=reads(
    (".github/scripts/race_memory.py", f"{at('envoy-go-race-docs', RACE_DOCS_STEP)} runs the package under it"),
) + ceiling_reads("envoy-go-race-docs") + setup_go("envoy-go-race-docs", f"{ENVOY}/go.mod"))
