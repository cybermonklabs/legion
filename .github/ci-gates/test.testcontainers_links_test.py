# The testcontainers_links_test filter's gate: what job test reads.
GATE = harness("testcontainers_links_test", "Test the testcontainers link check", f"{SCRIPTS}/testcontainers_links.test.sh", [
    (f"{SCRIPTS}/testcontainers_links.py", "the script under test"),
])
