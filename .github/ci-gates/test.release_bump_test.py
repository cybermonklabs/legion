# The release_bump_test filter's gate: what job test reads.
GATE = harness("release_bump_test", "Test the release version bump", f"{SCRIPTS}/release-bump.test.sh", [
    (f"{SCRIPTS}/release-bump.sh", "the script under test"),
])
