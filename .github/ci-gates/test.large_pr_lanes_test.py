# The large_pr_lanes_test filter's gate: what job test reads.
GATE = harness("large_pr_lanes_test", "Test the large pull request's label and its approval-time ci-ok (a stand-in gh)",
        f"{SCRIPTS}/large-pr-lanes.test.sh", [
    (f"{SCRIPTS}/large-pr-lanes.sh", "the script under test"),
    (f"{SCRIPTS}/gh-read.sh", "large-pr-lanes.sh sources it for its bounded gh reads and writes"),
])
