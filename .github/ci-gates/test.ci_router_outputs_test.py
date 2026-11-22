# The ci_router_outputs_test filter's gate: what job test reads.
GATE = harness("ci_router_outputs_test", "Test the router outputs' label terms", f"{SCRIPTS}/ci-router-outputs.test.sh", [
    (f"{SCRIPTS}/ci_router_outputs.py", "the script under test"),
])
