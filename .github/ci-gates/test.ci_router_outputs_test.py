# The ci_router_outputs_test filter's gate: what job test reads.
GATE = harness("ci_router_outputs_test", "Test the router outputs' label terms", f"{SCRIPTS}/ci-router-outputs.test.sh", [
    (f"{SCRIPTS}/ci_router_outputs.py", "the script under test"),
    (f"{SCRIPTS}/lane-verdicts.py", "the harness runs it over the env ci.yaml's statement step evaluates to and holds its "
                                    "decisions to the outputs"),
    (f"{SCRIPTS}/ci-filters-generate.sh", "the harness runs a copy of it to write the fallback file its fall-open "
                                          "cases evaluate the list over"),
    (".github/ci-gates/*.py", "the filter names the script reads (--filters) are the gates' names, which the changes "
                              "job computes the filters this step is handed from (CI_FILTERS_FILE)"),
    (f"{SCRIPTS}/check-ci-filters.sh", "the harness computes the filters with its generate mode when no "
                                       "CI_FILTERS_FILE is given"),
    (f"{SCRIPTS}/ci_filter_coverage.py", "check-ci-filters.sh's engine, which its generate mode runs, and whose "
                                         "GENERATED_HEAD lane-verdicts.py imports to check a computed header"),
    (f"{SCRIPTS}/ci_job_reads.py", "the engine's data, which loads the gates"),
    (f"{SCRIPTS}/ci_filter_sources.py", "the loader of the gate files and //ci: lines ci_job_reads.py imports"),
    (f"{SCRIPTS}/go_legs.py", "ci_job_reads.py imports it, and its import reads .github/go-legs.yaml"),
    (".github/go-legs.yaml", "the import of ci_job_reads.py reads the daemon's legs from it"),
    (f"{SCRIPTS}/ci_workflow_rules.py", "the rules check-ci-filters.sh imports"),
    (f"{SCRIPTS}/workflow_paths.py", "the path model the engine imports"),
    (f"{SCRIPTS}/go_build_tags.py", "the build-tag reader the engine imports"),
    (f"{SCRIPTS}/ci_ts_listing.py", "check-ci-filters.sh's generate mode imports it and starts the listings"),
    (f"{SCRIPTS}/ci_ts_programs.py", "ci_ts_listing.start() reads the pairs it lists"),
])
