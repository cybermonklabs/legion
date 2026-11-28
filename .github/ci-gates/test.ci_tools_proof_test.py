# The ci_tools_proof_test filter's gate: what job test reads.
GATE = harness("ci_tools_proof_test", "Test the tool fallback rig", f"{SCRIPTS}/ci-tools-proof.test.sh", [
    (f"{SCRIPTS}/ci-tools-proof.sh", "the script under test"),
    (f"{SCRIPTS}/ci-tools.sh", "`.github/scripts/ci-tools-proof.sh#$root/.github/scripts/ci-tools.sh` sources the fetch helper at its top level, "
                               "before its first function, so every case of the harness reads it"),
])
