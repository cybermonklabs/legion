# The harness_pool_test filter's gate: what job test reads. The pool's own test runs as a step of
# its own, not through the pool it tests.
GATE = harness("harness_pool_test", "Test the harness pool", f"{SCRIPTS}/harness-pool.test.sh", [
    (f"{SCRIPTS}/harness-pool.sh", "the script under test"),
    (f"{SCRIPTS}/harness_line_times.py", "the stamper that script starts beside each harness, whose record three cases read"),
])
