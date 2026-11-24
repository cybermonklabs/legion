# The cancel_acts_test filter's gate: what job test reads.
GATE = harness("cancel_acts_test", "Test the two cancel acts' reads of GitHub (a stand-in gh)",
        f"{SCRIPTS}/cancel-acts.test.sh", [
    (f"{SCRIPTS}/cancel-started-act.sh", "a script under test: the act ci.yaml's cancel-started job runs"),
    (f"{SCRIPTS}/cancel-dequeued-act.sh",
     "the other script under test: the act dequeued.yaml's cancel-dequeued job runs"),
    (f"{SCRIPTS}/gh-read.sh",
     "both acts source it for their bounded reads, and gh_read_transient decides there which "
     "refusal leaves an act green"),
    (f"{SCRIPTS}/merge-group-cancels.sh", "cancel-started-act.sh runs it for the decision it acts on"),
])
