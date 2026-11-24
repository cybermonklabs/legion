# The disk_watch_test filter's gate: what job test reads.
GATE = harness("disk_watch_test", "Test the machine watch", f"{SCRIPTS}/disk_watch.test.sh", [
    (f"{SCRIPTS}/disk_watch.py", "the module under test"),
])
