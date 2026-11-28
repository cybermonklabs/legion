# The cache_superseded_test filter's gate: what job test reads.
GATE = harness("cache_superseded_test", "Test the cache-superseded version mode's deletes (a stand-in gh)",
        f"{SCRIPTS}/cache-superseded.test.sh", [
    (f"{SCRIPTS}/cache-superseded.sh", "the script under test: the deletes ci-caches.yaml's bun-superseded job runs"),
    *ci_retry("cache-superseded.sh sources it from its own directory for the retry its list and its deletes go through"),
    (f"{SCRIPTS}/gh-read.sh", "cache-superseded.sh sources it from its own directory for the wait for a rate limit its "
                              "gh calls run through"),
])
