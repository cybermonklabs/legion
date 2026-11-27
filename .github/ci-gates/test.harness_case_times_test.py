# The harness_case_times_test filter's gate: what job test reads. The reader of the harness
# pool's line record runs over fixture record directories the harness writes itself, and reaches
# no network and no artifact of a real run.
GATE = harness("harness_case_times_test", "Test the harness case time reader", f"{SCRIPTS}/harness_case_times.test.sh", [
    (f"{SCRIPTS}/harness_case_times.py", "the script under test"),
])
