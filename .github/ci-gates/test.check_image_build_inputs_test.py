# The check_image_build_inputs_test filter's gate: what job test reads.
GATE = harness("check_image_build_inputs_test", "Test the image build input check",
        f"{SCRIPTS}/check-image-build-inputs.test.sh", [
    (f"{SCRIPTS}/check-image-build-inputs.sh", "the script under test, symlinked into every fixture"),
    (f"{SCRIPTS}/workflow_paths.py", "`.github/scripts/check-image-build-inputs.test.sh#workflow_paths.py` symlinks in the path model the script imports"),
    (f"{SCRIPTS}/image_build_entries.py", "check-image-build-inputs.sh imports it for each image build's entries"),
])
