# The browser suite's two gates, one per caller of dispatch-browser.yaml in ci.yaml:
# browser_gate (ci_job_reads.py) says what each reads.
GATE = browser_gate("dispatch_browser", BROWSER_JOB)
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/delivery/**', "filter dispatch_browser held packages/envoy/internal/dispatch/delivery/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter dispatch_browser held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 276 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/cipher/policy/**', "filter dispatch_browser held packages/envoy/internal/cipher/policy/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
