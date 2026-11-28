# The check_workflow_files_test filter's gate: what job test reads.
GATE = harness("check_workflow_files_test", "Test the workflow file check", f"{SCRIPTS}/check-workflow-files.test.sh", [
    (f"{SCRIPTS}/check-workflow-files.sh", "the script under test"),
    (f"{SCRIPTS}/workflow_paths.py", "check-workflow-files.sh imports repeated_keys, REQUIRED_CHECKS, one_line "
                                     "and the node readers mapping_get and scalar from beside its own file, which "
                                     "each fixture links to. Rules 5, 6 and 7 use those readers, and rule 10 "
                                     "(bun_test_steps.py) imports them itself."),
    (f"{SCRIPTS}/bun_test_steps.py", "check-workflow-files.sh imports its rule 10 from it, beside the script's own "
                                     "file, and the settings case copies it"),
    (f"{SCRIPTS}/bun_test_record.py", "bun_test_steps.py imports the names it holds steps to from it, and the "
                                      "settings case copies it"),
    (f"{SCRIPTS}/postgres-service-settings.sh", "check-workflow-files.sh runs its --command from beside "
                                                "its own file for rule 7, and the harness runs it for the "
                                                "command line its fixtures carry"),
    (".github/ci-notes/README.md", "the file both size refusals name, which the over-cap case holds to exist"),
])
