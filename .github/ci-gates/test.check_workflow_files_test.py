# The check_workflow_files_test filter's gate: what job test reads.
GATE = harness("check_workflow_files_test", "Test the workflow file check", f"{SCRIPTS}/check-workflow-files.test.sh", [
    (f"{SCRIPTS}/check-workflow-files.sh", "the script under test"),
    (f"{SCRIPTS}/workflow_paths.py", "check-workflow-files.sh imports repeated_keys and the node readers every rule "
                                     "reads a workflow with from it, beside the script's own file that every fixture "
                                     "links in"),
    (f"{SCRIPTS}/postgres-service-settings.sh", "check-workflow-files.sh runs its --command from beside "
                                                "its own file for rule 7, and the harness runs it for the "
                                                "command line its fixtures carry"),
    (f"{SCRIPTS}/go_test_steps.py", "check-workflow-files.sh imports its rule 8 from it, beside the script's own file"),
    (f"{SCRIPTS}/go_test_shell.py", "go_test_steps.py imports its reader of shell from it"),
    (f"{SCRIPTS}/go_test_events.py", "go_test_steps.py imports GoArgs, ENV_FILE and TRUE from it"),
    (".github/ci-notes/README.md", "the file both size refusals name, which the over-cap case holds to exist"),
])
