# The check_readers_place_test filter's gate: what job test reads.
GATE = harness("check_readers_place_test", "Test the reader's-place check",
        f"{SCRIPTS}/check-readers-place.test.sh", [
    (f"{SCRIPTS}/check-readers-place.sh", "the script under test"),
])
