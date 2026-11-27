# The repo_paths_test filter's gate: what job test reads.
GATE = harness("repo_paths_test", "Test the repository path derivation", f"{SCRIPTS}/repo-paths.test.sh", [
    (f"{SCRIPTS}/repo-paths.sh", "the script under test"),
])
