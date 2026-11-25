# The tool_fallback_nowhere_controls_test filter's gate: what job test reads.
GATE = harness("tool_fallback_nowhere_controls_test", "Test the negative control's hold on the base tree",
               f"{SCRIPTS}/tool-fallback-nowhere-controls.test.sh", [
    (f"{SCRIPTS}/tool-fallback-nowhere-controls.sh", "the script under test"),
    (f"{SCRIPTS}/ci-tools-proof.sh", "the script reads its install bound from `ci-tools-proof.sh install-bound`"),
])
