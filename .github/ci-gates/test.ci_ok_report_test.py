# The ci_ok_report_test filter's gate: what job test reads.
GATE = harness("ci_ok_report_test", "Test ci-ok's report and the check-run post (a stand-in gh)",
        f"{SCRIPTS}/ci-ok-report.test.sh", [
    (f"{SCRIPTS}/ci-ok-report.sh", "the script under test"),
    (f"{SCRIPTS}/ci-ok-hold.sh", "the ci-ok job's hold step, the other script under test"),
    (f"{SCRIPTS}/post-check-run.sh", "ci-ok-report.sh's last command, `.github/scripts/ci-ok-report.sh#$script_dir/post-check-run.sh`, "
                                     "posts with it, and the cases "
                                     "`.github/scripts/ci-ok-report.test.sh#post-check-run.sh called with no marker` and "
                                     "`.github/scripts/ci-ok-report.test.sh#post-check-run.sh posts the marker it was given` "
                                     "run it on its own"),
    *ci_retry("ci-ok-report.sh and ci-ok-hold.sh source it from their own directory for the retry their gh and git reads go through"),
    (f"{SCRIPTS}/gh-read.sh", "`.github/scripts/ci-ok-report.sh#$script_dir/gh-read.sh`, "
                              "`.github/scripts/ci-ok-hold.sh#$script_dir/gh-read.sh`, "
                              "`.github/scripts/post-check-run.sh#$script_dir/gh-read.sh` and "
                              "`.github/scripts/runner-shutdown.sh#pwd)/gh-read.sh` source it from "
                              "their own directory for the wait for a rate limit their "
                              "composed calls run through, which the cases whose tip, run or refs GitHub "
                              "refuses run through"),
    (f"{SCRIPTS}/runner-shutdown.sh",
     "`.github/scripts/ci-ok-report.sh#$script_dir/runner-shutdown.sh` sources it from its own directory, and no early ci-ok is posted for a failure it reads as "
     "a runner shutdown"),
    (f"{SCRIPTS}/ci-ok.sh", "`.github/scripts/ci-ok-report.sh#verdict` reaches its verdict with it, and ci-ok-report.test.sh's "
                            "verdict helper runs it for the group behind the front of the queue"),
    (f"{SCRIPTS}/ci-ok.jq", "ci-ok.sh's program includes it"),
])
