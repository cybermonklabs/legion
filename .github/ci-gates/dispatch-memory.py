# The dispatch_memory filter's gate: what job dispatch-memory reads.
GATE = gate("dispatch_memory", "dispatch-memory", runs=[ceiling_run("dispatch-memory")], exact=[
    pinned_step("dispatch-memory", MEMORY_STEP, ENVOY, MEMORY_SCRIPT, [
        (ENVOY, ["./cmd/dispatch"], "go test -tags memory ./cmd/dispatch", {"tags": "memory"}),
    ]),
], reads=reads(
    (TAGGED_TESTS, f"{at('dispatch-memory', MEMORY_STEP)} takes its -run from it"),
) + ceiling_reads("dispatch-memory") + service_settings_reads("dispatch-memory") + setup_go("dispatch-memory", f"{ENVOY}/go.mod"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/**', "filter dispatch_memory held packages/envoy/internal/dispatch/** by hand before the filters were computed: it selected 892 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/contracts/**', "filter dispatch_memory held packages/envoy/internal/contracts/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
