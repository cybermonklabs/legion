# The check_ci_images_test filter's gate: what job test reads.
GATE = harness("check_ci_images_test", "Test the CI image check", f"{SCRIPTS}/check-ci-images.test.sh", [
    (f"{SCRIPTS}/check-ci-images.sh", "the script under test"),
    (f"{SCRIPTS}/workflow_paths.py", "check-ci-images.test.sh:108 symlinks in the path model its sweep walks the tree with"),
    (f"{SCRIPTS}/ci_images.py", "check-ci-images.test.sh:109 symlinks in the reference grammar it reads a reference with"),
    (".github/ci-images.yaml", "its last case runs the package mode over this repository's own rows"),
])
