#!/usr/bin/env bash
# packages/pi-legion/package.json's omp.extensions, pointed at the bundle the published tarball
# ships (dist/legion.js) instead of the source the committed manifest names, before the package is
# packed.
#
# Who runs it, and what it takes:
#
#   .github/scripts/release-pi-legion-extensions.sh
#
# No arguments and no variables: it rewrites that one manifest of the checkout in place, named by
# its path from the repository root, so that is the working directory.
#
# Run from the repository root: .github/scripts/release-pi-legion-extensions.sh
# CI runs it in release.yaml's pi_legion_build job, at the step "Point extensions at the packed
# bundle".
set -euo pipefail

# The committed manifest keeps the source path (extensions/legion.ts) so repo checkouts
# (dev sessions) load the source without a build. The published tarball ships only the
# self-contained dist bundle, and scripts/pi-plugin-prepack.sh refuses to pack any other
# omp.extensions. legion.ts is inert without LEGION_TREE/LEGION_ROLE/LEGION_CONTROLLER in
# the environment, so every installed session loading it is safe; the Legion daemon
# relies on this single installed copy instead of a repo checkout. Nothing restores the
# manifest: this job's tree is discarded, and no job commits a version, so the committed
# manifest keeps whatever it last held.
jq '.omp.extensions = ["dist/legion.js"]' \
  packages/pi-legion/package.json > tmp.json \
  && mv tmp.json packages/pi-legion/package.json
