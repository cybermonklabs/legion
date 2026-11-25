# The go_legs_test filter's gate: what job test reads.
GATE = harness("go_legs_test", "Test the Go legs' package lists", f"{SCRIPTS}/go-legs.test.sh", [
    (f"{SCRIPTS}/go_legs.py", "the script under test, which go-legs.test.sh copies into each fixture tree"),
])
