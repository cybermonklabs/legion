# The architecture_check_test filter's gate: what job test reads.
GATE = harness("architecture_check_test", "Test the architecture-check action's go-path mode",
        ".github/actions/architecture-check/check.test.sh", [
    (".github/actions/architecture-check/check.sh", "the script under test"),
])
