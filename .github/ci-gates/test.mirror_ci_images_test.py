# The mirror_ci_images_test filter's gate: what job test reads.
GATE = harness("mirror_ci_images_test", "Test the CI image mirror", f"{SCRIPTS}/mirror-ci-images.test.sh", [
    (f"{SCRIPTS}/mirror-ci-images.sh", "the script under test"),
    (f"{SCRIPTS}/check-ci-images.sh", "`.github/scripts/mirror-ci-images.test.sh#gate_script`: its rule 9 case runs the gate over the registry a copy left"),
    (f"{SCRIPTS}/ci_images.py", "`.github/scripts/mirror-ci-images.test.sh#fixture` symlinks in the grammar both scripts read a row with"),
    (".github/ci-images.yaml", "`.github/scripts/mirror-ci-images.test.sh#held_back`: the rule 9 case builds its ordering fixture from this repository's own rows"),
    (f"{SCRIPTS}/workflow_paths.py", "check-ci-images.sh, which the harness runs, imports it"),
])
