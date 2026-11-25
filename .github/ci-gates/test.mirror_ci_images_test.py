# The mirror_ci_images_test filter's gate: what job test reads.
GATE = harness("mirror_ci_images_test", "Test the CI image mirror", f"{SCRIPTS}/mirror-ci-images.test.sh", [
    (f"{SCRIPTS}/mirror-ci-images.sh", "the script under test"),
    (f"{SCRIPTS}/check-ci-images.sh", "mirror-ci-images.test.sh:437 runs the gate over the registry a copy left"),
    (f"{SCRIPTS}/ci_images.py", "mirror-ci-images.test.sh:140 symlinks in the grammar both scripts read a row with"),
    (".github/ci-images.yaml", "mirror-ci-images.test.sh:362-419 builds its ordering fixture from this repository's own rows"),
    (f"{SCRIPTS}/workflow_paths.py", "check-ci-images.sh, which the harness runs, imports it"),
])
