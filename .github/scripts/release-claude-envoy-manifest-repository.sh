#!/usr/bin/env bash
# The repository.url of packages/claude-envoy/package.json and of its Codex package's
# packages/claude-envoy/codex/package.json, rewritten to the repository this run publishes from,
# before the packages are packed. npm verifies a provenance publish against that field, so each
# tarball has to carry the right one.
#
# Who runs it, and what it takes:
#
#   .github/scripts/release-claude-envoy-manifest-repository.sh
#
# No arguments and no variables of its own: it asks .github/scripts/repo-paths.sh for this
# repository's url, which that script derives from $GITHUB_REPOSITORY, and rewrites the two
# manifests of the checkout in place. It names them by their paths from the repository root, so
# that is the working directory.
#
# Run from the repository root: .github/scripts/release-claude-envoy-manifest-repository.sh
# CI runs it in release.yaml's claude_envoy_build job, at the step "Point the manifest at the
# publishing repository".
set -euo pipefail

# As in pi_envoy_build, and for the reason .github/scripts/release-pi-envoy-manifest-repository.sh
# gives: npm refuses a publish with
# provenance whose repository.url is not the publishing repository's, case for case, and
# the publish job packs nothing of its own. The committed manifest keeps today's
# repository; this writes whichever one the job runs in, and this job's tree is
# discarded.
url=$(.github/scripts/repo-paths.sh repository_url)
for manifest in packages/claude-envoy/package.json packages/claude-envoy/codex/package.json; do
  # Two commands, not one `&&` list: `set -e` ignores the failure of any command of such a list
  # but the last, so a manifest jq cannot parse would leave the step green and that manifest
  # unwritten (packages/claude-envoy/tests/release-manifest-repository.test.ts).
  jq --arg u "$url" '.repository.url = $u' "$manifest" > tmp.json
  mv tmp.json "$manifest"
done
