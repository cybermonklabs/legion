# The check_bun_version_test filter's gate: what job test reads.
GATE = harness("check_bun_version_test", "Test the Bun pin check", f"{SCRIPTS}/check-bun-version.test.sh", [
    (f"{SCRIPTS}/check-bun-version.sh", "the script under test"),
    (f"{SCRIPTS}/ci_images.py", "`.github/scripts/check-bun-version.test.sh#fixture` symlinks it in: the gate reads an image reference with it"),
])
