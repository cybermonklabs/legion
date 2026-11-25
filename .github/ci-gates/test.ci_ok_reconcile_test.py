# The ci_ok_reconcile_test filter's gate: what job test reads.
GATE = harness("ci_ok_reconcile_test", "Test the reconciliation of this head's other ci-ok check runs (a stand-in gh)",
        f"{SCRIPTS}/ci-ok-reconcile.test.sh", [
    (f"{SCRIPTS}/ci-ok-reconcile.sh", "the script under test"),
    *ci_retry("ci-ok-reconcile.sh sources it from its own directory for the retry its gh reads go through"),
    (f"{SCRIPTS}/gh-read.sh", "`.github/scripts/ci-ok-reconcile.sh#$script_dir/gh-read.sh` sources it from its own directory for "
                              "the wait for a rate limit its composed read and PATCH run through"),
])
