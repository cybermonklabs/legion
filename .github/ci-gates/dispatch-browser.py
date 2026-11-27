# The gate of ci.yaml's dispatch-browser job, the full lane, one of the three callers of
# dispatch-browser.yaml (dispatch-browser-pr.py holds the second, and dispatch-references.py the
# third, kept by hand): browser_gate (ci_job_reads.py) says what it reads.
GATE = browser_gate("dispatch_browser", BROWSER_JOB)
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/cipher/store/**', "filter dispatch_browser held packages/envoy/internal/cipher/store/** by hand before the filters were computed, and the branch's edit kept it while it moved the gate read behind it: it selected 1 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
