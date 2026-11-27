# The go_test_within_job_test filter's gate: what job test reads.
GATE = harness("go_test_within_job_test", "Test the go test job-limit wrapper", f"{SCRIPTS}/go-test-within-job.test.sh", [
    (GO_WITHIN_JOB, "the script under test"),
])
