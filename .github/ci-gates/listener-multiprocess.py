# The listener_multiprocess filter's gate: what job listener-multiprocess reads
# (.github/ci-notes/ci.md#listener_multiprocess-the-two-process-and-two-build-tests-listener-multiprocess
# says why). Its steps are in ci-job-broker-multiprocess.yaml, beside broker-multiprocess's (SHARED_FILE).
GATE = gate("listener_multiprocess", "listener-multiprocess", exact=[
    pinned_step("listener-multiprocess", LISTENER_MP_STEP, ENVOY, LISTENER_MP_SCRIPT, [
        (ENVOY, ["./cmd/listener"], "go test -tags multiprocess ./cmd/listener", {"tags": "multiprocess"}),
        (ENVOY, ["./cmd/listener"], "cmdtest.Build of the listener under the envoy_twobuild tag", {"tags": "envoy_twobuild", "mode": "build"}),
    ]),
], reads=setup_go("listener-multiprocess", f"{ENVOY}/go.mod"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/cipherclient/**', "filter listener_multiprocess held packages/envoy/internal/cipherclient/** by hand (the branch's edit) before the filters were computed: it selected 1 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/delivery/**', "filter listener_multiprocess held packages/envoy/internal/dispatch/delivery/** by hand (the branch's edit) before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter listener_multiprocess held packages/envoy/internal/dispatch/pmdoc/** by hand (the branch's edit) before the filters were computed: it selected 281 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/text/**', "filter listener_multiprocess held packages/envoy/internal/dispatch/text/** by hand (the branch's edit) before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
