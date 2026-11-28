# The check_ts_filters_test filter's gate: what job test reads.
GATE = harness("check_ts_filters_test", "Test the TypeScript filter check", f"{SCRIPTS}/check-ts-filters.test.sh", [
    (f"{SCRIPTS}/check-ts-filters.sh", "the script under test, symlinked into every fixture (check-ts-filters.test.sh's fixture function)"),
    (f"{SCRIPTS}/ci_filter_coverage.py", "the engine it borrows the changes job's filters from, linked in beside it"),
    (f"{SCRIPTS}/audited_scripts.py", "the readings of the scripts a step runs, which the engine imports, linked in beside it"),
    (f"{SCRIPTS}/workflow_paths.py", "the path model both import, linked in beside it"),
    (f"{SCRIPTS}/go_build_tags.py", "the Go build-tag module the engine imports, linked in beside it"),
    (f"{SCRIPTS}/ci_ts_listing.py", "the listing both the check and the changes job's generation run, linked in beside it"),
    (f"{SCRIPTS}/ci_ts_programs.py", "the browser case imports the real table's dispatch_browser row"),
])
