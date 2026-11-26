# The fetch_depth_if_shallow_test filter's gate: what job test reads.
GATE = harness("fetch_depth_if_shallow_test", "Test the fetch that gives a depth to a shallow checkout alone (fixture git remotes; no GitHub)",
        f"{SCRIPTS}/fetch-depth-if-shallow.test.sh", [
    (f"{SCRIPTS}/fetch-depth-if-shallow.sh", "the script under test"),
])
