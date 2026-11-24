# The dispatch_browser_pr filter's gate: what job dispatch-browser-pr reads, browser_gate
# (ci_job_reads.py) as the full lane's gate is.
GATE = browser_gate("dispatch_browser_pr", BROWSER_PR_JOB)
