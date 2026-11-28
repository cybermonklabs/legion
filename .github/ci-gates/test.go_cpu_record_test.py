# The go_cpu_record_test filter's gate: what job test reads. The reader of the envoy-go jobs' spread
# and its factors runs over runs, jobs pages and log excerpts captured from real runs, served by a
# stand-in gh, and over fixture tables and a package tree the harness writes; it reaches no
# network.
GATE = harness("go_cpu_record_test", "Test the Go CPU record's reader", f"{SCRIPTS}/go_cpu_record.test.sh", [
    (f"{SCRIPTS}/go_cpu_record.py", "the script under test"),
    (f"{SCRIPTS}/testdata/go-cpu/**", "the harness's fixtures: the runs, the jobs pages and the log excerpts its stand-in gh serves"),
])
