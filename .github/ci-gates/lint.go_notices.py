# The go_notices filter's gate: what job lint reads.
GATE = gate("go_notices", "lint", step=NOTICES_STEP, runs=go_notices_runs(), reads=reads(
    (NOTICES_LOOPS, f"{at('lint', NOTICES_STEP)} runs it; it holds the loops"),
    (NOTICES, f"{NOTICES_LOOPS} runs it once per release target"),
    (GO_TARGETS, f"{NOTICES_LOOPS}: jq reads it"),
    (CLIENT_TARGETS, f"{NOTICES_LOOPS}: jq reads it"),
    (INFRA_TARGETS, f"{NOTICES_LOOPS}: jq reads it"),
))
GATE = with_reads(GATE, [
    Read('**/*.go', "filter go_notices held **/*.go by hand before the filters were computed: it selected 945 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('**/go.mod', "filter go_notices held **/go.mod by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares"),
    Read('**/go.sum', "filter go_notices held **/go.sum by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares"),
    Read('packages/daemon/internal/daemon/**', "filter go_notices held packages/daemon/internal/daemon/** by hand before the filters were computed: it selected 7 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter go_notices held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 24 file(s) no other read of this gate declares", but=('**/*_test.go', 'packages/envoy/internal/**/*.md')),
    Read('packages/envoy/infra/aws/**', "filter go_notices held packages/envoy/infra/aws/** by hand before the filters were computed: it selected 5 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
