# main's #3179 gave the gate rule 3's own-repository test: it runs .github/scripts/repo-paths.sh
# for this repository's addresses, so that script is a second subject of this step.
GATE = harness("check_ci_tools_test", "Test the tool manifest check", f"{SCRIPTS}/check-ci-tools.test.sh", [
    (f"{SCRIPTS}/check-ci-tools.sh", "the script under test"),
    (f"{SCRIPTS}/repo-paths.sh", "check-ci-tools.sh:981-982 runs it in bash for plugin_download_url, and "
                                 "check-ci-tools.test.sh:103 links it into every fixture"),
    (f"{SCRIPTS}/ci-tools.sh", "check-ci-tools.test.sh sources it for the helpers its cases call"),
])
