# The dispatch_browser_pr filter's gate: what job dispatch-browser-pr reads.
GATE = browser_gate("dispatch_browser_pr", BROWSER_PR_JOB)
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/delivery/**', "filter dispatch_browser_pr held packages/envoy/internal/dispatch/delivery/** by hand (the branch's edit) before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter dispatch_browser_pr held packages/envoy/internal/dispatch/pmdoc/** by hand (the branch's edit) before the filters were computed: it selected 277 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/cipher/policy/**', "filter dispatch_browser_pr held packages/envoy/internal/cipher/policy/** by hand (the branch's edit) before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
