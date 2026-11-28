# A second gate on go_notices, lint's release-path check, so it runs exactly when the restore
# of the module cache it reads does: the go list of ./cmd/legion it runs for each legion target
# through release-build-legion.sh's list mode, and the files it reads to hold release.yaml's
# LEGION_VERSION_ARGS to that closure. Its go env GOWORK and go list -m compile nothing. The
# two gates claim different steps, so they do not collide; lint.go_notices.py holds the notices
# step's gate, and a file of .github/ci-gates binds one GATE.
GATE = gate("go_notices", "lint", step=RELEASE_PATHS_STEP, runs=legion_release_paths_runs(), reads=reads(
    (RELEASE_PATHS_CHECK, at("lint", RELEASE_PATHS_STEP)),
    (RELEASE_BUILD_LEGION, f"{RELEASE_PATHS_CHECK} runs its modes"),
    (f"{SCRIPTS}/release-push.sh", f"{RELEASE_PATHS_CHECK} runs its version-paths and legion-targets"),
    (".github/workflows/release.yaml", f"{RELEASE_PATHS_CHECK} reads its LEGION_VERSION_ARGS"),
    (GO_TARGETS, f"{SCRIPTS}/release-push.sh's legion-targets: jq reads it"),
))
