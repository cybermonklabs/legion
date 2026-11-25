# The dispatch_multiprocess filter's gate: what job dispatch-multiprocess reads.
GATE = gate("dispatch_multiprocess", "dispatch-multiprocess", runs=[ceiling_run("dispatch-multiprocess")], exact=[
    pinned_step("dispatch-multiprocess", DISPATCH_MP_STEP, ENVOY, DISPATCH_MP_SCRIPT, [
        (ENVOY, ["./cmd/dispatch"], "go test -tags multiprocess ./cmd/dispatch", {"tags": "multiprocess"}),
    ]),
], reads=reads(
    (TAGGED_TESTS, f"{at('dispatch-multiprocess', DISPATCH_MP_STEP)} takes its -run from it"),
) + ceiling_reads("dispatch-multiprocess") + crash_safety_reads("dispatch-multiprocess") + setup_go("dispatch-multiprocess", f"{ENVOY}/go.mod"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/**', "filter dispatch_multiprocess held packages/envoy/internal/dispatch/** by hand before the filters were computed: it selected 893 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/contracts/**', "filter dispatch_multiprocess held packages/envoy/internal/contracts/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
