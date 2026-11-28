# The gh_read_test filter's gate: what job test reads.
GATE = harness("gh_read_test", "Test the wait for a spent API budget and the retry ladder (a stand-in GitHub)",
        f"{SCRIPTS}/gh-read.test.sh", [
    (f"{SCRIPTS}/gh-read.sh", "the script under test"),
    *ci_retry("gh-read.test.sh's composed cases source it beside the code they drive, for the ladder "
              "gh_wait_reset runs"),
    (f"{SCRIPTS}/gh-read-stand-in.py", "the stand-in GitHub gh-read.test.sh starts, which every case calls"),
])
