# The check_image_trigger_paths_test filter's gate: what job test reads.
GATE = harness("check_image_trigger_paths_test", "Test the image trigger path check",
        f"{SCRIPTS}/check-image-trigger-paths.test.sh", [
    (f"{SCRIPTS}/check-image-trigger-paths.sh", "the script under test"),
    (f"{SCRIPTS}/workflow_paths.py", "`.github/scripts/check-image-trigger-paths.test.sh#fixture` symlinks in the path model the script imports"),
])
