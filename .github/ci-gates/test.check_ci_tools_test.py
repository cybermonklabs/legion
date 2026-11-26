# main's #3179 gave the gate rule 3's own-repository test: it runs .github/scripts/repo-paths.sh
# for this repository's addresses, so that script is a second subject of this step.
GATE = harness("check_ci_tools_test", "Test the tool manifest check", f"{SCRIPTS}/check-ci-tools.test.sh", [
    (f"{SCRIPTS}/check-ci-tools.sh", "the script under test"),
    (f"{SCRIPTS}/repo-paths.sh", "`.github/scripts/check-ci-tools.sh#REPO_PATHS` runs it in bash for plugin_download_url, and "
                                 "`.github/scripts/check-ci-tools.test.sh#fixture` links it into every fixture"),
    (f"{SCRIPTS}/ci-tools.sh", "check-ci-tools.test.sh sources it for the helpers its cases call"),
])
