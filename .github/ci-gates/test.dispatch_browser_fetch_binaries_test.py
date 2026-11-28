# The dispatch_browser_fetch_binaries_test filter's gate: what job test reads.
GATE = harness("dispatch_browser_fetch_binaries_test", "Test the browser lane's second read of its binaries (a stand-in GitHub)",
        f"{SCRIPTS}/dispatch-browser-fetch-binaries.test.sh", [
    (f"{SCRIPTS}/dispatch-browser-fetch-binaries.sh", "the script under test"),
    *ci_retry("dispatch-browser-fetch-binaries.sh sources it from its own directory for both of its "
              "reads, and the harness reads the waits of its ladder"),
])
