# The check_private_names_keys_test filter's gate: what job test reads.
GATE = harness("check_private_names_keys_test", "Test the private-name check's issue-key rule", f"{SCRIPTS}/check-private-names-keys.test.sh", [
    (f"{SCRIPTS}/check-private-names-test-lib.sh", "check-private-names-keys.test.sh:26 sources it for its fixtures"),
    (TEST_LIB, "check-private-names-test-lib.sh:37 sources it for check/is/summary"),
    (f"{SCRIPTS}/check-private-names.sh", "the script under test"),
    (f"{SCRIPTS}/check-pr-text.sh", "check-private-names-test-lib.sh:417 runs it, over a pull request's new text"),
    (f"{SCRIPTS}/gh-read.sh", "check-pr-text.sh:72 sources it for every gh read"),
], lib=False)
