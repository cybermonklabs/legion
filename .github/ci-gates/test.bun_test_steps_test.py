# The bun_test_steps_test filter's gate: what job test reads.
GATE = harness("bun_test_steps_test", "Test the workflow file check's bun test reports rule",
        f"{SCRIPTS}/bun_test_steps.test.sh", [
    (f"{SCRIPTS}/bun_test_steps.py", "the module under test"),
    (f"{SCRIPTS}/check-workflow-files.sh", "every case runs it, linked into the case's fixture: it imports the module "
                                          "from beside its own file and reports what the module refuses"),
    (f"{SCRIPTS}/bun_test_record.py", "the module imports the wrapper's path, the report name and the upload step's "
                                      "action and name from it"),
    (f"{SCRIPTS}/workflow_paths.py", "the module imports the node readers from it, and the check imports "
                                     "repeated_keys"),
    (f"{SCRIPTS}/postgres-service-settings.sh", "the check runs its --command from beside its own file before any "
                                                "rule"),
    (".github/actions/bun-test-reports-upload/action.yml", "every fixture copies it, and the action case holds its "
                                                           "upload's name, path and hidden files to the record's"),
])
