# The ci_ok_report_test filter's gate: what job test reads.
GATE = harness("ci_ok_report_test", "Test ci-ok's report and the check-run post (a stand-in gh)",
        f"{SCRIPTS}/ci-ok-report.test.sh", [
    (f"{SCRIPTS}/ci-ok-report.sh", "the script under test"),
    (f"{SCRIPTS}/ci-ok-hold.sh", "the ci-ok job's hold step, the other script under test"),
    (f"{SCRIPTS}/post-check-run.sh", "ci-ok-report.sh:292 posts with it, and ci-ok-report.test.sh:459,475 runs it on its own"),
    (f"{SCRIPTS}/gh-read.sh", "ci-ok-report.sh:121 and ci-ok-hold.sh:94 source it for their bounded reads"),
    (f"{SCRIPTS}/runner-shutdown.sh",
     "ci-ok-report.sh:123 sources it, and no early ci-ok is posted for a failure it reads as "
     "a runner shutdown"),
    (f"{SCRIPTS}/ci-ok.sh", "ci-ok-report.sh:268 reaches its verdict with it, and ci-ok-report.test.sh's "
                            "verdict helper runs it for the group behind the front of the queue"),
    (f"{SCRIPTS}/ci-ok.jq", "ci-ok.sh's program includes it"),
])
