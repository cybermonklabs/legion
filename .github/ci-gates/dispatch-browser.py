# The gate of ci.yaml's dispatch-browser job, the full lane, one of the three callers of
# dispatch-browser.yaml (dispatch-browser-pr.py holds the second, and dispatch-references.py the
# third, kept by hand): browser_gate (ci_job_reads.py) says what it reads.
GATE = browser_gate("dispatch_browser", BROWSER_JOB)
