# The daemon_contract_order_test filter's gate: what job test reads. The harness sources no
# test-lib.sh and builds its own scratch repository.
GATE = harness("daemon_contract_order_test", "Test the daemon contract order script (a scratch repository)",
        "scripts/daemon-contract-order.test.sh", [
    ("scripts/daemon-contract-order.sh", "the script under test (`scripts/daemon-contract-order.test.sh#scripts/daemon-contract-order.sh` names it)"),
], lib=False)
