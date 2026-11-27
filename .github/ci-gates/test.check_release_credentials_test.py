# The check_release_credentials_test filter's gate: what job test reads.
GATE = harness("check_release_credentials_test", "Test the release credential check",
        f"{SCRIPTS}/check-release-credentials.test.sh", [
    (f"{SCRIPTS}/check-release-credentials.sh", "the script under test"),
    (f"{SCRIPTS}/shell_commands.py", "check-release-credentials.sh imports it to find command position, "
                                     "and the harness links it into each fixture"),
])
