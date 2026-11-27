# The check_gh_version_test filter's gate: what job test reads.
GATE = harness("check_gh_version_test", "Test the gh version check", f"{SCRIPTS}/check-gh-version.test.sh", [
    (f"{SCRIPTS}/check-gh-version.sh", "the script under test"),
])
