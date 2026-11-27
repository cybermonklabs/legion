# The go_legs_test filter's gate: what job test reads.
GATE = harness("go_legs_test", "Test the Go legs' package lists", f"{SCRIPTS}/go-legs.test.sh", [
    (f"{SCRIPTS}/go_legs.py", "the script under test, which go-legs.test.sh copies into each fixture tree"),
    (f"{SCRIPTS}/go_legs_groups.py", "the rebalancer the harness runs against that same fixture, so the file "
                                     "it writes and the file go_legs.py reads are tested together"),
    (f"{SCRIPTS}/go_build_tags.py", "go_legs.py imports its go_environment for the two go commands it runs, "
                                    "which go-legs.test.sh runs through the copy in each fixture tree"),
])
