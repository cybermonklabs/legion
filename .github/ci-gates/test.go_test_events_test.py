# The go_test_events_test filter's gate: what job test reads.
GATE = harness("go_test_events_test", "Test the go test events writer", f"{SCRIPTS}/go_test_events.test.sh", [
    (f"{SCRIPTS}/go_test_events.py", "the script under test"),
    ("packages/envoy/scripts/backend-count.sh", "its SIGTERM case runs this counter under the writer, and a copy of it "
                                                 "that appends its report only once go test has returned"),
    (f"{DAEMON}/go.mod", "the test job's setup-go installs its Go, which every case runs: each compares the writer's "
                         "log with plain go test's, and the writer follows the output rules of that Go's cmd/go and "
                         "testing, so a bump of it must run the cases again"),
])
