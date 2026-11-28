# The select_e2e_proofs_test filter's gate: what job test reads.
GATE = harness("select_e2e_proofs_test", "Test the e2e proof selector", f"{SCRIPTS}/select-e2e-proofs.test.sh", [
    (f"{SCRIPTS}/select-e2e-proofs.sh", "the script under test"),
    (".github/e2e-scripts.json", "its last case runs the selector over this repository's own list"),
    ("scripts/e2e/*.sh", "that same case: the selector names one row per scripts/e2e/*.sh (`.github/scripts/select-e2e-proofs.sh#present` globs them)"),
])
