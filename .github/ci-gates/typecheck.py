# The typecheck filter's gate: what job typecheck reads.
GATE = gate("typecheck", "typecheck", runs=[
    step_run("typecheck", "Vet the Go daemon, its e2e-tagged live proofs included", "go vet -tags e2e ./...", DAEMON, ["./..."],
             tags="e2e", mode="vet"),
], exact=[
    darwin_vet("typecheck", DAEMON_DARWIN_VET, DAEMON),
], reads=setup_go("typecheck", f"{DAEMON}/go.mod") + darwin_vet_reads("typecheck", DAEMON_DARWIN_VET, DAEMON))
GATE = with_reads(GATE, [
    Read('packages/daemon/**', "filter typecheck held packages/daemon/** by hand before the filters were computed: it selected 808 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/cipher/policy/**', "filter typecheck held packages/envoy/internal/cipher/policy/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/cipherclient/**', "filter typecheck held packages/envoy/internal/cipherclient/** by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter typecheck held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 278 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/text/**', "filter typecheck held packages/envoy/internal/dispatch/text/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
