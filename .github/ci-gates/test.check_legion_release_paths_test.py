# The check_legion_release_paths_test filter's gate: what job test reads.
GATE = harness("check_legion_release_paths_test", "Test the legion release path check and the legion build it asks",
        f"{SCRIPTS}/check-legion-release-paths.test.sh", [
    (f"{SCRIPTS}/check-legion-release-paths.sh",
     "the script under test, symlinked into every fixture (check-legion-release-paths.test.sh's fixture function)"),
    (f"{SCRIPTS}/release-push.sh",
     "symlinked into every fixture of both harnesses: the check runs its version-paths and legion-targets, and the "
     "build script its legion-targets"),
    (f"{SCRIPTS}/gh-read.sh", "release-push.sh sources it, so both harnesses reach it through the copy they "
                              "symlink into each fixture"),
    (f"{SCRIPTS}/release-bump.sh", "release-push.sh runs it for `version`, which the check's version-paths mode "
                                   "shares its parse with"),
    (f"{SCRIPTS}/release-build-legion.sh",
     "symlinked into every fixture beside it: the check runs its modes"),
    (f"{SCRIPTS}/release-build-legion.test.sh",
     "the step's second harness, which tests the build script's build and modes and sources test-lib.sh too"),
])
