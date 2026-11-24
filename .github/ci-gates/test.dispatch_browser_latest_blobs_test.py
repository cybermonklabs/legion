# The dispatch_browser_latest_blobs_test filter's gate: what job test reads.
GATE = harness("dispatch_browser_latest_blobs_test", "Test the browser lane's blob attempt selection",
        f"{SCRIPTS}/dispatch-browser-latest-blobs.test.sh", [
    (f"{SCRIPTS}/dispatch-browser-latest-blobs.sh", "the script under test"),
])
