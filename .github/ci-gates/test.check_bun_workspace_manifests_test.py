# The check_bun_workspace_manifests_test filter's gate: what job test reads.
GATE = harness("check_bun_workspace_manifests_test", "Test the frozen Bun workspace manifest check",
        f"{SCRIPTS}/check-bun-workspace-manifests.test.sh", [
    (f"{SCRIPTS}/check-bun-workspace-manifests.sh", "the script under test"),
])
