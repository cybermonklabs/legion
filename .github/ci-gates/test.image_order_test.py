# The image_order_test filter's gate: what job test reads. The harness sources no
# test-lib.sh and builds its own scratch repository.
GATE = harness("image_order_test", "Test the worker image's contract order step (a scratch repository)",
        f"{SCRIPTS}/worker-image-contract-order.test.sh", [
    (".github/workflows/worker-image.yaml",
     "the workflow whose build job's contract_order step the harness reads out and runs"),
], lib=False)
