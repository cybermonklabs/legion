# The go_tagged_tests_test filter's gate: what job test reads.
GATE = harness("go_tagged_tests_test", "Test the build-tag test lister", f"{SCRIPTS}/go-tagged-tests.test.sh", [
    (f"{SCRIPTS}/go-tagged-tests.sh", "the script under test"),
    ("packages/envoy/cmd/listener/*_test.go",
     "`.github/scripts/go-tagged-tests.test.sh#run_lister serverchange` runs the lister over this repository's own packages/envoy/cmd/listener "
     "and holds its answer to `^(TestServerChange)$`, and the lister reads every *_test.go of the "
     "directory it is given (`.github/scripts/go-tagged-tests.sh#constraint_of`), so a second serverchange test, a rename of that "
     "function or a compound //go:build line there changes what this step prints"),
])
