# The go_test_test filter's gate: what job test reads.
GATE = harness("go_test_test", "Test the Go test runner and its retry", f"{SCRIPTS}/go-test.test.sh", [
    (f"{SCRIPTS}/go-test.sh", "the script under test, which go-test.test.sh copies into its fixture repository"),
    (f"{SCRIPTS}/go-test.jq", "the jsonfile reader go-test.sh includes, copied beside it"),
    (f"{SCRIPTS}/race_memory.py", "the sampler go-test.sh --race-memory runs gotestsum under, copied beside it"),
    (f"{SCRIPTS}/testdata/go-test/**", "the fixture module every case runs the real go and gotestsum over"),
    (".github/actions/install-gotestsum/action.yml", "the install of the gotestsum the harness runs"),
    (".github/ci-tools.yaml", "the gotestsum row that install reads"),
])
