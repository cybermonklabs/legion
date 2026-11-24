# The release_push_test filter's gate: what job test reads.
GATE = harness("release_push_test", "Test release scripts", f"{SCRIPTS}/release-push.test.sh", [
    (f"{SCRIPTS}/release-push.sh", "the script under test"),
    (f"{SCRIPTS}/gh-read.sh", "release-push.sh:1036,1115,1202 source it for restart's, release's and "
                              "release-section's gh calls, which the stub gh answers"),
    (f"{SCRIPTS}/release-bump.sh", "release-push.sh:966 runs it for `version`, and the version cases read its refusals"),
    (".github/workflows/release.yaml", "release-push.test.sh's worker_image_release case reads that job's step out of it and runs it"),
    (".github/workflows/release-envoy-listener.yaml",
     "release-push.test.sh's Determine latest case reads release_publish's step out of it and runs its image read"),
    ("packages/pi-legion/.changie.yaml", "release-push.test.sh's fold-notes case for the shipped configuration copies it into its fixture"),
])
