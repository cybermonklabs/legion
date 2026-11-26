# The ci_ok_retries_test filter's gate: what job test reads.
GATE = harness("ci_ok_retries_test", "Test the report of the Go tests that passed only on their retry",
               f"{SCRIPTS}/ci-ok-retries.test.sh", [
    (f"{SCRIPTS}/ci-ok-retries.sh", "the script under test"),
    (f"{SCRIPTS}/go-test.jq", "the jsonfile reader ci-ok-retries.sh includes"),
    (f"{SCRIPTS}/testdata/ci-ok-retries/**", "the jsonfiles and rerun reports go-test.test.sh's runs wrote, which every case reads"),
])
