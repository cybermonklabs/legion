# The merge_group_cancels_test filter's gate: what job test reads.
GATE = harness("merge_group_cancels_test", "Test the merge-group cancel decision", f"{SCRIPTS}/merge-group-cancels.test.sh", [
    (f"{SCRIPTS}/merge-group-cancels.sh", "the script under test"),
])
