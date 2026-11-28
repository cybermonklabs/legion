# The go_cache_working_set_test filter's gate: what job test reads.
GATE = harness("go_cache_working_set_test", "Test the build cache prune of the cache fills",
        f"{SCRIPTS}/go-cache-working-set.test.sh", [
    (f"{SCRIPTS}/go-cache-working-set.sh", "the script under test"),
    (f"{DAEMON}/go.mod", "the test job's setup-go installs its Go, which the harness's real-go cases run: they "
                         "check that the Go the fills run still marks the cache entries a build uses"),
    (f"{ENVOY}/go.mod", "the go-envoy, go-envoy-test, go-envoy-smoke, go-envoy-race and go-envoy-darwin-vet fills install its Go, so a bump "
                        "of it changes the Go whose marking those fills' prunes rest on. The harness still runs "
                        "on the daemon's Go, the one the test job installs, so for a bump of this file alone the "
                        "check is those fills' own `go-cache-working-set.sh end`: from the second fill on the new "
                        "Go (the first restores nothing of it), a Go that marks none of the entries it reads "
                        "leaves no restored entry marked, and end prints its ::warning:: (the harness's "
                        "\"warns that the build marked none of the restored entries\" case)"),
])
