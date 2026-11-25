# The mirror_ci_tools_test filter's gate: what job test reads.
GATE = harness("mirror_ci_tools_test", "Test the tool copy filler", f"{SCRIPTS}/mirror-ci-tools.test.sh", [
    (f"{SCRIPTS}/mirror-ci-tools.sh", "the script under test"),
    (f"{SCRIPTS}/ci-tools.sh", "`.github/scripts/mirror-ci-tools.test.sh#fixture` symlinks in the fetch helper the filler sources"),
])
