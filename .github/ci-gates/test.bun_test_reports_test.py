# The bun_test_reports_test filter's gate: what job test reads.
GATE = harness("bun_test_reports_test", "Test the wrapper and the reader of the bun test reports",
        f"{SCRIPTS}/bun_test_reports.test.sh", [
    (f"{SCRIPTS}/bun_test_reports.py", "the reader under test"),
    (f"{SCRIPTS}/bun_test_record.py", "the record's names and its rule, which the reader and the wrapper import"),
    (f"{SCRIPTS}/bun_test_run.py", "the wrapper under test, which writes every record the reader reads"),
    (".bun-version", "the harness runs the bun setup-bun installs, whose report format and exits the two are held to"),
])
