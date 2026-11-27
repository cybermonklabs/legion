# The lane_packages_test filter's gate: what job test reads.
GATE = harness("lane_packages_test", "Test the slow suites' own packages and ci-ok's lane check",
               f"{SCRIPTS}/lane-packages.test.sh", [
    (f"{SCRIPTS}/lane_packages.py", "the script under test"),
    (".github/lane-packages.yaml", "the table the script reads, which the harness holds to ci.yaml and to the tree"),
    (".github/go-legs.yaml", "the script reads the packages of each slow suite's legs out of it"),
    (f"{SCRIPTS}/workflow_paths.py", "the script's gate matches the table's globs with its PathsFilter, and the "
                                     "harness evaluates the written filters with it"),
    (".github/workflows/ci-job-test.yaml", "the script reads the test job's harnesses out of it"),
    (".github/workflows/ci-job-envoy-go-checks.yaml", "the script reads envoy-go-checks' harnesses out of it"),
    (".github/workflows/ci-job-*.yaml", "the harness holds every go test package a slow suite's workflow names to "
                                        "that suite's own packages"),
    ("**/go.mod", "the harness holds the table's nested modules to the go.mod files inside a legs module"),
])
