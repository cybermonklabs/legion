# The main_bypass_check_test filter's gate: what job test reads.
GATE = harness("main_bypass_check_test", "Test the main bypass check's decision", f"{SCRIPTS}/main-bypass-check.test.sh", [
    (f"{SCRIPTS}/main-bypass-check.sh", "the script under test"),
    (".github/workflows/main-bypass-check.yaml",
     "`.github/scripts/main-bypass-check.test.sh#no_pipefail` holds every run: step of the workflow to a shell with pipefail, and "
     "`.github/scripts/main-bypass-check.test.sh#head_env` holds the decision's HEAD_SHA and HEAD_MESSAGE to the push payload's head"),
])
