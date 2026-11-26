# The release_push_test filter's gate: what job test reads.
GATE = harness("release_push_test", "Test release scripts", f"{SCRIPTS}/release-push.test.sh", [
    (f"{SCRIPTS}/release-push.sh", "the script under test"),
    (f"{SCRIPTS}/gh-read.sh", "`.github/scripts/release-push.sh#)/gh-read.sh`: restart, release and release-section source it for that "
                              "helper, which their gh calls go through; the stub gh answers them"),
    (f"{SCRIPTS}/release-bump.sh", "`.github/scripts/release-push.sh#prev_version`: its `version` command runs it on the previous version, and the version cases read its refusals"),
    (NOTICES_LOOPS, "release-push.test.sh's go-release-notices.sh case symlinks it into a fixture and runs it over an empty list"),
    (".github/workflows/release.yaml", "release-push.test.sh's worker_image_release and legion archive check cases read "
                                       "those steps out of it and run them"),
    (".github/workflows/release-envoy-listener.yaml",
     "release-push.test.sh's Determine latest case reads release_publish's step out of it and runs its image read"),
    ("packages/pi-legion/.changie.yaml", "release-push.test.sh's fold-notes case for the shipped configuration copies it into its fixture"),
    (".github/private-names.toml", "`.github/scripts/release-push.sh#leak_guard` finds it beside itself and runs gitleaks "
                                   "with it, and `.github/scripts/release-push.test.sh#leak_rules_into` copies it into each "
                                   "fixture checkout"),
])
