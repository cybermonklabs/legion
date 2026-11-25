# The dispatch_browser_reading_rows_test filter's gate: what job test reads.
GATE = harness("dispatch_browser_reading_rows_test", "Test the browser lane's reading of its rows",
        f"{SCRIPTS}/dispatch-browser-reading-rows.test.sh", [
    (f"{SCRIPTS}/dispatch-browser-reading-rows.sh", "the script under test"),
    (".github/workflows/dispatch-browser.yaml",
     "the harness runs the script through the merge job's own call, the run: line of the step "
     "\"Reading one, the rows\", and reads that the selection step carries the id it passes"),
])
