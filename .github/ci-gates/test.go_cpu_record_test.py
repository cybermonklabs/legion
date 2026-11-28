# The go_cpu_record_test filter's gate: what job test reads. The reader of the envoy-go jobs' spread,
# its factors and their CPU record runs over runs, jobs pages, log excerpts and artifacts captured
# from real runs, served by a stand-in gh, and over fixture tables, a module's go.mod and a
# package tree the harness writes; it reaches no network.
GATE = harness("go_cpu_record_test", "Test the Go CPU record's reader", f"{SCRIPTS}/go_cpu_record.test.sh", [
    (f"{SCRIPTS}/go_cpu_record.py", "the script under test"),
    (f"{SCRIPTS}/go-cpu-runner.py", "the row classes and the row reader go_cpu_record.py loads from it"),
    (f"{SCRIPTS}/go_build_tags.py", "the script imports go_skips_dir from it, the one copy of which directories go passes over"),
    (f"{SCRIPTS}/testdata/go-cpu/**", "the harness's fixtures: the runs, the jobs pages, the log excerpts and the artifacts its stand-in gh serves"),
])
