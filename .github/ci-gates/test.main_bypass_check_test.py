# The main_bypass_check_test filter's gate: what job test reads.
GATE = harness("main_bypass_check_test", "Test the main bypass check's decision", f"{SCRIPTS}/main-bypass-check.test.sh", [
    (f"{SCRIPTS}/main-bypass-check.sh", "the script under test"),
    (".github/workflows/main-bypass-check.yaml",
     "main-bypass-check.test.sh:198-210 holds every run: step of the workflow to a shell with pipefail, and "
     ":216-227 holds the decision's HEAD_SHA and HEAD_MESSAGE to the push payload's head"),
])
