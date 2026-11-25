# The workflow_paths_test filter's gate: what job test reads.
GATE = harness("workflow_paths_test", "Test the workflow path model the path checks import",
        f"{SCRIPTS}/workflow_paths.test.sh", [
    (f"{SCRIPTS}/workflow_paths.py", "the module under test, symlinked in at workflow_paths.test.sh:29"),
])
