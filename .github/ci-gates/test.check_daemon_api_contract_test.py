# The check_daemon_api_contract_test filter's gate: what job test reads.
GATE = harness("check_daemon_api_contract_test", "Test the daemon API contract number check",
        f"{SCRIPTS}/check-daemon-api-contract.test.sh", [
    (f"{SCRIPTS}/check-daemon-api-contract.sh", "the script under test"),
    (f"{SCRIPTS}/gh-read.sh", "check-daemon-api-contract.sh:80 names it, and each gh api read runs through it"),
])
