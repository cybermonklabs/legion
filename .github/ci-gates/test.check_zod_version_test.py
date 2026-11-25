# The check_zod_version_test filter's gate: what job test reads.
GATE = harness("check_zod_version_test", "Test the zod version check", f"{SCRIPTS}/check-zod-version.test.sh", [
    (f"{SCRIPTS}/check-zod-version.sh", "the script under test"),
])
