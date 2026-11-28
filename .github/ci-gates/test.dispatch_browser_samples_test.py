# The dispatch_browser_samples_test filter's gate: what job test reads.
GATE = harness("dispatch_browser_samples_test", "Test the browser lane's memory and CPU sampler",
        f"{SCRIPTS}/dispatch-browser-samples.test.sh", [
    (f"{SCRIPTS}/dispatch-browser-samples.sh", "the script under test"),
])
