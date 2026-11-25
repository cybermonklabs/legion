# The dispatch_browser_run_reads_test filter's gate: what job test reads.
GATE = harness("dispatch_browser_run_reads_test", "Test the browser lane's run reads",
        f"{SCRIPTS}/dispatch-browser-run-reads.test.sh", [
    (f"{SCRIPTS}/dispatch-browser-run-reads.sh", "the script under test"),
    (f"{SCRIPTS}/gh-read.sh",
     "`.github/scripts/dispatch-browser-run-reads.sh#$script_dir/gh-read.sh` sources it, and the harness drives its retry ladder with a "
     "stand-in gh that refuses a case's first calls"),
])
