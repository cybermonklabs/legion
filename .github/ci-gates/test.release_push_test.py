# The release_push_test filter's gate: what job test reads.
GATE = harness("release_push_test", "Test release scripts", f"{SCRIPTS}/release-push.test.sh", [
    (f"{SCRIPTS}/release-push.sh", "the script under test"),
    (f"{SCRIPTS}/gh-read.sh", "release-push.sh:1074,1131,1219 source it for restart's, release's and "
                              "release-section's gh calls, which the stub gh answers"),
    (f"{SCRIPTS}/release-bump.sh", "release-push.sh:995 runs it for `version`, and the version cases read its refusals"),
    (NOTICES_LOOPS, "release-push.test.sh's go-release-notices.sh case symlinks it into a fixture and runs it over an empty list"),
    (f"{SCRIPTS}/legion_targets.py", "release-push.test.sh's two-readers case gives it each file it gives legion-targets"),
    (".github/workflows/release.yaml", "release-push.test.sh's worker_image_release and legion archive check cases read "
                                       "those steps out of it and run them"),
    (".github/workflows/release-envoy-listener.yaml",
     "release-push.test.sh's Determine latest case reads release_publish's step out of it and runs its image read"),
    ("packages/pi-legion/.changie.yaml", "release-push.test.sh's fold-notes case for the shipped configuration copies it into its fixture"),
])
