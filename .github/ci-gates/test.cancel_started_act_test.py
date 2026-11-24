# The cancel_started_act_test filter's gate: what job test reads.
GATE = harness("cancel_started_act_test", "Test the merge-group cancel act (a stand-in gh)", f"{SCRIPTS}/cancel-started-act.test.sh", [
    (f"{SCRIPTS}/cancel-started-act.sh", "the script under test"),
    (f"{SCRIPTS}/merge-group-cancels.sh", "the act runs it over the run list and acts on its lines"),
    (f"{SCRIPTS}/gh-read.sh", "the act makes both its calls through gh_wait_reset"),
])
