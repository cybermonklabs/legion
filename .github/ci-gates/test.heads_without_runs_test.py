# The heads_without_runs_test filter's gate: what job test reads.
GATE = harness("heads_without_runs_test", "Test the check that names open pull request heads no workflow ran and queued entries that conflict with main (a fixture git origin; a stand-in gh)",
        f"{SCRIPTS}/heads-without-runs.test.sh", [
    (f"{SCRIPTS}/heads-without-runs.sh", "the script under test"),
    (f"{SCRIPTS}/gh-read.sh", "`.github/scripts/heads-without-runs.sh#pwd)/gh-read.sh` sources it for its list reads, its pull request reads, its activity read and its fetch"),
])
