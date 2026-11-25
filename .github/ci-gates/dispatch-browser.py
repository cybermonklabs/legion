# The gate of ci.yaml's dispatch-browser job, the full lane, one of the two callers of
# dispatch-browser.yaml (dispatch-browser-pr.py holds the other): browser_gate
# (ci_job_reads.py) says what it reads.
GATE = browser_gate("dispatch_browser", BROWSER_JOB)
