# The cancel_started_act_test filter's gate: what job test reads.
GATE = harness("cancel_started_act_test", "Test the cancel-started act's read of the run list (a stand-in gh)",
        f"{SCRIPTS}/cancel-started-act.test.sh", [
    (f"{SCRIPTS}/cancel-started-act.sh", "the script under test: the act ci.yaml's cancel-started job runs"),
    *ci_retry("`.github/scripts/cancel-started-act.sh#. .github/scripts/ci-retry.sh` sources it: retry_fetch's ladder is the "
              "retry the harness tests"),
    (f"{SCRIPTS}/gh-read.sh", "the act sources it for the wait for a rate limit its composed calls run through"),
    (f"{SCRIPTS}/merge-group-cancels.sh", "the act runs it for the decision it acts on"),
])
