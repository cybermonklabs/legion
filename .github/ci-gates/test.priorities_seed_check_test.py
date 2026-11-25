# The one harness of the job that is not under .github/scripts and sources no test-lib.sh.
GATE = harness("priorities_seed_check_test", "Test the priorities seed check (a stand-in Dispatch)",
        "scripts/priorities-seed-check.test.sh", [
    ("scripts/priorities-seed-check.sh", "the script under test (`scripts/priorities-seed-check.test.sh#script` names it)"),
], lib=False)
