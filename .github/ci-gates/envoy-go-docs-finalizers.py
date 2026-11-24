# A second gate on envoy_go_race_docs: this job runs the same package, so it reads that
# closure and no other, and check-ci-filters.sh keys a claim by (job, step), so the two gates
# claim different steps and do not collide.
GATE = gate("envoy_go_race_docs", "envoy-go-docs-finalizers", runs=[ceiling_run("envoy-go-docs-finalizers")], exact=[
    pinned_step("envoy-go-docs-finalizers", FINALIZERS_STEP, ENVOY, FINALIZERS_SCRIPT, [
        (ENVOY, ["./internal/dispatch/docs/"], "go test -v -count=1 -timeout 34m ./internal/dispatch/docs/", {}),
    ]),
], reads=ceiling_reads("envoy-go-docs-finalizers") + setup_go("envoy-go-docs-finalizers", f"{ENVOY}/go.mod"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/pmdoc/schema/**', "filter envoy_go_race_docs held packages/envoy/internal/dispatch/pmdoc/schema/** by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares"),
    Read('packages/envoy/internal/dispatch/pmdoc/testdata/**', "filter envoy_go_race_docs held packages/envoy/internal/dispatch/pmdoc/testdata/** by hand before the filters were computed: it selected 259 file(s) no other read of this gate declares"),
])
