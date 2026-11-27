# The find_pr_payload_test filter's gate: what job test reads.
GATE = harness("find_pr_payload_test", "Test the pull request payload both pr-title checks read (a stand-in gh)",
        f"{SCRIPTS}/find-pr-payload.test.sh", [
    (f"{SCRIPTS}/find-pr-payload.sh", "the script under test"),
    (f"{SCRIPTS}/gh-read.sh", "find-pr-payload.sh:38 sources it for its gh read"),
])
