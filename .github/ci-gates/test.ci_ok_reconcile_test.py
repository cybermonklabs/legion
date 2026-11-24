# The ci_ok_reconcile_test filter's gate: what job test reads.
GATE = harness("ci_ok_reconcile_test", "Test the reconciliation of this head's other ci-ok check runs (a stand-in gh)",
        f"{SCRIPTS}/ci-ok-reconcile.test.sh", [
    (f"{SCRIPTS}/ci-ok-reconcile.sh", "the script under test"),
    (f"{SCRIPTS}/gh-read.sh", "ci-ok-reconcile.sh:80 sources it for its bounded gh reads"),
])
