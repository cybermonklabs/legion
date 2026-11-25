# The ci_ok_test filter's gate: what job test reads.
GATE = harness("ci_ok_test", "Test ci-ok's verdict", f"{SCRIPTS}/ci-ok.test.sh", [
    (f"{SCRIPTS}/ci-ok.sh", "the script under test"),
    (f"{SCRIPTS}/ci-ok.jq", "`.github/scripts/ci-ok.sh#program` and `.github/scripts/ci-ok.sh#rows` runs its program with `jq -L \"$script_dir\"` and `include \"ci-ok\"`"),
    (f"{SCRIPTS}/runner-shutdown.sh",
     "`.github/scripts/ci-ok.sh#$script_dir/runner-shutdown.sh` sources it, and the harness drives it with a stand-in gh: it is what reads "
     "a failure the platform caused off this run's check runs"),
    *ci_retry("runner-shutdown.sh sources it from its own directory for the retry its gh reads go through"),
    (f"{SCRIPTS}/gh-read.sh", "`.github/scripts/runner-shutdown.sh#pwd)/gh-read.sh` sources it from its own directory for "
                              "the wait for a rate limit its composed reads run through"),
    (".github/workflows/ci-job-daemon-go.yaml",
     "its last case reads the two steps that restate their legs' result, which must write the words "
     "`.github/scripts/runner-shutdown.sh#RUNNER_SHUTDOWN_RESTATED` holds"),
])
