# The broker_multiprocess filter's gate: what job broker-multiprocess reads.
GATE = gate("broker_multiprocess", "broker-multiprocess", exact=[
    pinned_step("broker-multiprocess", BROKER_MP_STEP, ENVOY, BROKER_MP_SCRIPT, [
        (ENVOY, ["./cmd/cipherd"], "go test -tags multiprocess ./cmd/cipherd", {"tags": "multiprocess"}),
    ]),
], reads=reads(
    (TAGGED_TESTS, f"{at('broker-multiprocess', BROKER_MP_STEP)} takes its -run from it"),
) + service_settings_reads("broker-multiprocess") + setup_go("broker-multiprocess", f"{ENVOY}/go.mod") + go_modules("broker-multiprocess"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/cipher/policy/**', "filter broker_multiprocess held packages/envoy/internal/cipher/policy/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/cipherclient/**', "filter broker_multiprocess held packages/envoy/internal/cipherclient/** by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/contracts/**', "filter broker_multiprocess held packages/envoy/internal/contracts/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
