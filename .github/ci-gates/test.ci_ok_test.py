# The ci_ok_test filter's gate: what job test reads.
GATE = harness("ci_ok_test", "Test ci-ok's verdict", f"{SCRIPTS}/ci-ok.test.sh", [
    (f"{SCRIPTS}/ci-ok.sh", "the script under test"),
    (f"{SCRIPTS}/ci-ok.jq", "ci-ok.sh:173-232 runs its program with `jq -L \"$script_dir\"` and `include \"ci-ok\"`"),
    (f"{SCRIPTS}/runner-shutdown.sh",
     "ci-ok.sh:119 sources it, and the harness drives it with a stand-in gh: it is what reads "
     "a failure the platform caused off this run's check runs"),
    (f"{SCRIPTS}/gh-read.sh", "runner-shutdown.sh:104 sources it for its bounded gh reads"),
])
