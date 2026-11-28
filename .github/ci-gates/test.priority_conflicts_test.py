# The priority_conflicts_test filter's gate: what job test reads.
GATE = harness("priority_conflicts_test", "Test the priority pull request conflict check (a fixture git origin; a stand-in gh)",
        f"{SCRIPTS}/priority-conflicts.test.sh", [
    (f"{SCRIPTS}/priority-conflicts.sh", "the script under test"),
    *ci_retry("`.github/scripts/priority-conflicts.sh#pwd)/ci-retry.sh` sources it from its own directory for the ladder "
              "its label read, its fetch and its read of the queue's branches go through"),
    (f"{SCRIPTS}/gh-read.sh", "`.github/scripts/priority-conflicts.sh#pwd)/gh-read.sh` sources it for the wait for a rate limit "
                              "its composed reads run through"),
])
