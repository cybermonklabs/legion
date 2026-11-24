# The check_workflow_files_test filter's gate: what job test reads.
GATE = harness("check_workflow_files_test", "Test the workflow file check", f"{SCRIPTS}/check-workflow-files.test.sh", [
    (f"{SCRIPTS}/check-workflow-files.sh", "the script under test"),
    (f"{SCRIPTS}/workflow_paths.py", "check-workflow-files.sh imports repeated_keys from it, beside the "
                                     "script's own file that every fixture links in"),
    (".github/ci-notes/README.md", "the file both size refusals name, which the over-cap case holds to exist"),
])
