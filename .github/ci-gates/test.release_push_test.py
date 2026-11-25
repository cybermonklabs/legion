# The release_push_test filter's gate: what job test reads.
GATE = harness("release_push_test", "Test release scripts", f"{SCRIPTS}/release-push.test.sh", [
    (f"{SCRIPTS}/release-push.sh", "the script under test"),
    (f"{SCRIPTS}/gh-read.sh", "`.github/scripts/release-push.sh#)/gh-read.sh`: restart, release and release-section source it for that "
                              "helper, which their gh calls go through; the stub gh answers them"),
    (f"{SCRIPTS}/release-bump.sh", "`.github/scripts/release-push.sh#prev_version`: its `version` command runs it on the previous version, and the version cases read its refusals"),
    (".github/workflows/release.yaml", "release-push.test.sh's worker_image_release case reads that job's step out of it and runs it"),
    (".github/workflows/release-envoy-listener.yaml",
     "release-push.test.sh's Determine latest case reads release_publish's step out of it and runs its image read"),
    ("packages/pi-legion/.changie.yaml", "release-push.test.sh's fold-notes case for the shipped configuration copies it into its fixture"),
])
