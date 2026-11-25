# The check_gitleaks_version_test filter's gate: what job test reads.
GATE = harness("check_gitleaks_version_test", "Test the gitleaks version check",
        f"{SCRIPTS}/check-gitleaks-version.test.sh", [
    (f"{SCRIPTS}/check-gitleaks-version.sh",
     "the script under test, symlinked into every fixture (check-gitleaks-version.test.sh:31)"),
])
