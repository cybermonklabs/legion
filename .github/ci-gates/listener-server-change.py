# The listener_server_change filter's gate: what job listener-server-change reads.
GATE = gate("listener_server_change", "listener-server-change", exact=[
    pinned_step("listener-server-change", SOAK_STEP, ENVOY, SOAK_SCRIPT, [
        (ENVOY, ["./cmd/listener"], "go test -tags serverchange ./cmd/listener", {"tags": "serverchange"}),
    ]),
], reads=reads(
    (TAGGED_TESTS, f"{at('listener-server-change', SOAK_STEP)} takes its -run from it"),
) + setup_go("listener-server-change", f"{ENVOY}/go.mod"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/cipherclient/**', "filter listener_server_change held packages/envoy/internal/cipherclient/** by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/delivery/**', "filter listener_server_change held packages/envoy/internal/dispatch/delivery/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter listener_server_change held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 278 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/text/**', "filter listener_server_change held packages/envoy/internal/dispatch/text/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
