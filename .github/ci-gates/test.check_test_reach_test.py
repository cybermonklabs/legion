# The check_test_reach_test filter's gate: what job test reads.
GATE = harness("check_test_reach_test", "Test the test-reach search", f"{SCRIPTS}/check-test-reach.test.sh", [
    (f"{SCRIPTS}/check-test-reach.sh", "the script under test"),
    (f"{SCRIPTS}/go_test_steps.py", "the step reader the search imports, linked into each fixture"),
    (f"{SCRIPTS}/go_build_tags.py", "the tag reader the search imports, linked into each fixture"),
])
