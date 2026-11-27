# The priority_conflicts_test filter's gate: what job test reads.
GATE = harness("priority_conflicts_test", "Test the priority pull request conflict check (a fixture git origin; a stand-in gh)",
        f"{SCRIPTS}/priority-conflicts.test.sh", [
    (f"{SCRIPTS}/priority-conflicts.sh", "the script under test"),
    (f"{SCRIPTS}/gh-read.sh", "`.github/scripts/priority-conflicts.sh#pwd)/gh-read.sh` sources it for its label read and its fetch"),
])
