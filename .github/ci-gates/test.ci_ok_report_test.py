# The ci_ok_report_test filter's gate: what job test reads.
GATE = harness("ci_ok_report_test", "Test ci-ok's report and the check-run post (a stand-in gh)",
        f"{SCRIPTS}/ci-ok-report.test.sh", [
    (f"{SCRIPTS}/ci-ok-report.sh", "the script under test"),
    (f"{SCRIPTS}/ci-ok-hold.sh", "the ci-ok job's hold step, the other script under test"),
    (f"{SCRIPTS}/post-check-run.sh", "`.github/scripts/ci-ok-report.sh#$script_dir/post-check-run.sh` posts with it, and "
                                     "`.github/scripts/ci-ok-report.test.sh#post-check-run.sh called with no marker` and `.github/scripts/ci-ok-report.test.sh#post-check-run.sh posts the marker it was given` "
                                     "runs it on its own"),
    (f"{SCRIPTS}/gh-read.sh", "`.github/scripts/ci-ok-report.sh#$script_dir/gh-read.sh` and `.github/scripts/ci-ok-hold.sh#$script_dir/gh-read.sh` source it for their bounded reads"),
    (f"{SCRIPTS}/runner-shutdown.sh",
     "`.github/scripts/ci-ok-report.sh#$script_dir/runner-shutdown.sh` sources it, and no early ci-ok is posted for a failure it reads as "
     "a runner shutdown"),
    (f"{SCRIPTS}/ci-ok.sh", "`.github/scripts/ci-ok-report.sh#verdict` reaches its verdict with it, and ci-ok-report.test.sh's "
                            "verdict helper runs it for the group behind the front of the queue"),
    (f"{SCRIPTS}/ci-ok.jq", "ci-ok.sh's program includes it"),
])
