#!/usr/bin/env bash
# packages/pi-envoy/package.json's repository.url, rewritten to the repository this run publishes
# from, before the package is packed. npm verifies a provenance publish against that field, so the
# tarball has to carry the right one.
#
# Who runs it, and what it takes:
#
#   .github/scripts/release-pi-envoy-manifest-repository.sh
#
# No arguments and no variables of its own: it asks .github/scripts/repo-paths.sh for this
# repository's url, which that script derives from $GITHUB_REPOSITORY, and rewrites the manifest of
# the checkout in place. It names both by their paths from the repository root, so that is the
# working directory.
#
# Run from the repository root: .github/scripts/release-pi-envoy-manifest-repository.sh
# CI runs it in release.yaml's pi_envoy_build job, at the step "Point the manifest at the
# publishing repository".
set -euo pipefail

# npm matches a package's repository.url against the repository publishing it, case for
# case, and refuses a publish with provenance whose url is not that repository's (npm
# docs, "Generating provenance statements", Prerequisites; the registry answers E422
# "Error verifying sigstore provenance bundle: Failed to validate repository
# information"). The publish job hands npm this job's tarball exactly as packed and
# never repacks it, so the field has to be right here, before the pack below.
#
# The committed manifest is left alone: it holds today's repository, which is true
# today, and this step writes whichever repository the job runs in, so moving the
# repository to another owner or name needs no commit. Nothing restores it, as with the
# rewrites above: this job's tree is discarded.
url=$(.github/scripts/repo-paths.sh repository_url)
jq --arg u "$url" '.repository.url = $u' \
  packages/pi-envoy/package.json > tmp.json \
  && mv tmp.json packages/pi-envoy/package.json
