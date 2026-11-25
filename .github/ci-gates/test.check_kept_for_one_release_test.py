# The check_kept_for_one_release_test filter's gate: what job test reads.
GATE = harness("check_kept_for_one_release_test", "Test the kept-for-one-release list check",
        f"{SCRIPTS}/check-kept-for-one-release.test.sh", [
    (f"{SCRIPTS}/check-kept-for-one-release.sh", "the script under test"),
])
