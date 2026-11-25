# The race_memory_test filter's gate: what job test reads.
GATE = harness("race_memory_test", "Test the race memory sampler", f"{SCRIPTS}/race_memory.test.sh", [
    (f"{SCRIPTS}/race_memory.py", "the module under test"),
])
