# The go_work_sum_test filter's gate: what job test reads.
GATE = harness("go_work_sum_test", "Test the go.work.sum check over fixture workspaces and a file proxy",
        f"{SCRIPTS}/check-go-work-sum.test.sh", [
    (f"{SCRIPTS}/check-go-work-sum.sh", "the script under test, which the harness copies into each fixture tree"),
    *ci_retry("the harness copies it into each fixture tree beside check-go-work-sum.sh, which sources it from its "
              "own directory for the retry its go mod download all goes through"),
    (f"{DAEMON}/go.mod", "the test job's setup-go installs its Go, which every case runs: they check that the Go "
                         "the check runs still writes each hash a workspace download reads, an unselected "
                         "version's go.mod hash among them"),
])
