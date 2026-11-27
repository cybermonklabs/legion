# The check_private_names_test filter's gate: what job test reads.
GATE = harness("check_private_names_test", "Test the private-name check", f"{SCRIPTS}/check-private-names.test.sh", [
    (f"{SCRIPTS}/check-private-names-test-lib.sh", "check-private-names.test.sh:23 sources it for its fixtures"),
    (TEST_LIB, "check-private-names-test-lib.sh:37 sources it for check/is/summary"),
    (f"{SCRIPTS}/check-private-names.sh", "the script under test"),
    (f"{SCRIPTS}/check-pr-text.sh", "check-private-names-test-lib.sh:417 runs it too: the pull request's text is the guard's other surface"),
    (f"{SCRIPTS}/gh-read.sh", "check-pr-text.sh:72 sources it for every gh read"),
], lib=False)
