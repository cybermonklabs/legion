# The log_guard_test filter's first gate: the step that tests ci-log-guard.sh. The step that tests
# check-log-guard.sh is the filter's second gate (test.log_guard_test.check.py).
GATE = harness("log_guard_test", "Test the log guard that keeps a server's text from becoming a workflow command",
        f"{SCRIPTS}/ci-log-guard.test.sh", [
    (f"{SCRIPTS}/ci-log-guard.sh", "the library under test"),
    (".github/workflows/release.yaml", "ci-log-guard.test.sh reads every inline definition of the guard in it"),
    (".github/workflows/release-envoy-listener.yaml",
     "ci-log-guard.test.sh reads every inline definition of the guard in it"),
])
