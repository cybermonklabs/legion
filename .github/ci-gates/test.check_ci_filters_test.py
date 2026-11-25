# The check_ci_filters_test filter's gate: what job test reads.
GATE = harness("check_ci_filters_test", "Test the CI filter check", f"{SCRIPTS}/check-ci-filters.test.sh", [
    (f"{SCRIPTS}/check-ci-filters.sh", "the script under test, symlinked into every fixture (check-ci-filters.test.sh's fixture function)"),
    (f"{SCRIPTS}/ci_filter_coverage.py", "the engine the fixtures link in beside it"),
    (f"{SCRIPTS}/ci_workflow_rules.py", "the rules the fixtures link in beside it"),
    (f"{SCRIPTS}/workflow_paths.py", "the path model the fixtures link in beside it"),
    (f"{SCRIPTS}/go_build_tags.py", "the build-tag reader rule 20 imports, linked in beside it"),
    (f"{SCRIPTS}/ci_job_reads.py", "check-ci-filters.test.sh's derived function imports this table to derive the plain step's -run lists"),
    (f"{SCRIPTS}/ci_filter_sources.py", "the loader of the gate files and //ci: lines the fixtures link in beside it"),
    (f"{SCRIPTS}/ci-filters-generate.sh", "the changes job's wrapper the fixtures link in, which the fail-open and "
                                          "base-tree cases run"),
    (f"{SCRIPTS}/ci_ts_listing.py", "the TypeScript listing the fixtures link in, which the generate mode imports"),
    (f"{SCRIPTS}/ci-reads-convert", "the converter cases load it and run its gate and filter steps"),
    (f"{SCRIPTS}/ci_ts_programs.py", "the converter's filters step imports it for each filter's listed trees"),
    (".github/ci-gates/*.py", "the derived function's import of ci_job_reads.py loads every gate file"),
    (".github/workflows/ci-job-typecheck.yaml", "check-ci-filters.test.sh runs its darwin vet step's own run text "
                                                "over a module with a negated tag"),
    (FORCED_ORDER, "check-ci-filters.test.sh reads its go flags, which must pass -tags odatrace, the one name "
                   "beyond the toolchain's that go_build_tags.py leaves out of every vet"),
])
