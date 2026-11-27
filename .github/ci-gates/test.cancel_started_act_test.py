# The cancel_started_act_test filter's gate: what job test reads.
GATE = harness("cancel_started_act_test", "Test the cancel-started act's read of the run list (a stand-in gh)",
        f"{SCRIPTS}/cancel-started-act.test.sh", [
    (f"{SCRIPTS}/cancel-started-act.sh", "the script under test: the act ci.yaml's cancel-started job runs"),
    (f"{SCRIPTS}/gh-read.sh", "the act sources it: gh_read's ladder is the retry the harness tests"),
    (f"{SCRIPTS}/merge-group-cancels.sh", "the act runs it for the decision it acts on"),
])
