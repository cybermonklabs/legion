# The go_work_sum_test filter's gate: what job test reads.
GATE = harness("go_work_sum_test", "Test the go.work.sum check over fixture workspaces and a file proxy",
        f"{SCRIPTS}/check-go-work-sum.test.sh", [
    (f"{SCRIPTS}/check-go-work-sum.sh", "the script under test, which the harness copies into each fixture tree"),
])
