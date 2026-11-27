# Notes on `.github/workflows/release-envoy-listener.yaml`

## The inline stop-commands block

Four steps of this workflow define `ci_log_guard_stderr` inline: "Create the image index", "Determine latest", "Advance latest image" and "Fill the cache of the earlier Dispatch builds with this release".
Each runs in a job that holds `packages: write` or `contents: write`, and `.github/scripts/check-release-credentials.sh` keeps such a job from running any script of this repository, so the step cannot source `.github/scripts/ci-log-guard.sh`.
The inline copy writes the same block as that library: a fresh token from `/dev/urandom`, both markers on stderr, and the end marker on a line of its own after the command exits.
So a registry's or GitHub's reply that `docker buildx imagetools`, `git` or `gh` copies to the log stays text.
Each step's own workflow commands after a block go to stderr, where they stay in order with the block.
`.github/scripts/ci-log-guard.test.sh` reads each inline definition out of `.github/workflows/release-envoy-listener.yaml` and drives it with the forms a server can plant.
