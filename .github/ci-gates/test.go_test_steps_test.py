# The go_test_steps_test filter's gate: what job test reads.
GATE = harness("go_test_steps_test", "Test the workflow file check's go test events rule",
        f"{SCRIPTS}/go_test_steps.test.sh", [
    (f"{SCRIPTS}/go_test_steps.py", "the module under test"),
    (f"{SCRIPTS}/go_test_shell.py", "the module's reader of shell, which it imports from beside its own file"),
    (f"{SCRIPTS}/check-workflow-files.sh", "every case runs it, linked into the case's fixture: it imports the module "
                                          "from beside its own file and reports what the module refuses"),
    (f"{SCRIPTS}/go_test_events.py", "the module imports GoArgs, ENV_FILE and TRUE from it, and the generated "
                                     "forms' kept steps run it"),
    (f"{SCRIPTS}/workflow_paths.py", "the module imports PathsFilter, EVENTS_WRITER and the node readers from it, "
                                     "and the check imports repeated_keys"),
    (f"{SCRIPTS}/go_test_step_forms.py", "one case runs the generated forms, and two controls of them"),
    (".github/go-test-step-misses.tsv", "the generated forms' list of known misses, which a row of it that matches "
                                        "no miss fails"),
])
