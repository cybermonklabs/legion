# The check_private_names_keys_test filter's gate: what job test reads.
GATE = harness("check_private_names_keys_test", "Test the private-name check's issue-key rule", f"{SCRIPTS}/check-private-names-keys.test.sh", [
    (f"{SCRIPTS}/check-private-names-test-lib.sh", "`.github/scripts/check-private-names-keys.test.sh#pwd)/check-private-names-test-lib.sh` sources it for its fixtures and its runs of the check"),
    (TEST_LIB, "`.github/scripts/check-private-names-test-lib.sh#$script_dir/test-lib.sh`: the lib sources it for check/is/summary, which that function and the harnesses call"),
    (f"{SCRIPTS}/check-private-names.sh", "the script under test"),
    (f"{SCRIPTS}/check-pr-text.sh", "`.github/scripts/check-private-names-test-lib.sh#run_pr_text` runs it, over a pull request's new text"),
    (f"{SCRIPTS}/gh-read.sh", "`.github/scripts/check-pr-text.sh#$script_dir/gh-read.sh` sources it from its own directory for every gh read"),
], lib=False)
GATE = with_reads(GATE, reads(*ci_retry("check-pr-text.sh sources it from its own directory for every read of the API")))
