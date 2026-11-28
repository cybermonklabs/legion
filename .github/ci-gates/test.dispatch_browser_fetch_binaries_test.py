# The dispatch_browser_fetch_binaries_test filter's gate: what job test reads.
GATE = harness("dispatch_browser_fetch_binaries_test", "Test the browser lane's second read of its binaries (a stand-in GitHub)",
        f"{SCRIPTS}/dispatch-browser-fetch-binaries.test.sh", [
    (f"{SCRIPTS}/dispatch-browser-fetch-binaries.sh", "the script under test"),
])
