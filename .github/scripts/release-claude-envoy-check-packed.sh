#!/usr/bin/env bash
# The packed @sjawhar/claude-envoy tarball, read back before the publish job is handed it: it names
# the publishing repository, it declares this release's version in all three of its manifests, it
# holds the two generated directories the installed plugin runs from, and it holds the skills.
#
# Who runs it, and what it takes:
#
#   .github/scripts/release-claude-envoy-check-packed.sh
#
# No arguments. It reads one variable of the step's environment, $VERSION, the version this run
# releases, which the step's `env:` carries from the version step, and asks
# .github/scripts/repo-paths.sh for this repository's url. The tarball it reads is
# packages/claude-envoy/*.tgz, the one the pack step left there, named by its path from the
# repository root, so that is the working directory.
#
# Run from the repository root: .github/scripts/release-claude-envoy-check-packed.sh
# CI runs it in release.yaml's claude_envoy_build job, at the step "Check the packed tarball names
# the publishing repository and the release, and holds the build and the skills".
set -euo pipefail

# pi_envoy_build's check, over this package's tarball, and four more: the version
# Claude Code reads off .claude-plugin/plugin.json, without which every install is
# `unknown` and no update is ever seen; the version Oh My Pi reads off
# .omp-plugin/plugin.json, which tests/envoy-channel-server.test.ts holds equal to
# package.json's; the two generated directories, which the plugin runs from and
# .gitignore names, so a packer that started honouring git would publish a plugin that
# cannot start; and the skills, which only the pack's prepack turns into files. All of
# them are read out of the tarball the publish job will hand npm, never out of the
# working tree.
url=$(.github/scripts/repo-paths.sh repository_url)
tar -xzOf packages/claude-envoy/*.tgz package/package.json \
  | jq -e --arg u "$url" '.repository.url == $u'
for manifest in package/package.json package/.claude-plugin/plugin.json \
  package/.omp-plugin/plugin.json; do
  tar -xzOf packages/claude-envoy/*.tgz "$manifest" \
    | jq -e --arg v "$VERSION" '.version == $v'
done
members=$(tar -tzf packages/claude-envoy/*.tgz)
for member in package/dist/envoy-channel.js package/legion/.claude-plugin/plugin.json; do
  if ! printf '%s\n' "$members" | grep -qxF "$member"; then
    echo "::error::the packed tarball holds no $member, which the installed plugin runs from"
    exit 1
  fi
done
# The skills, read back out of the same tarball: the rules are in
# packages/claude-envoy/scripts/check-packed-skills.sh, which tests/packed-skills.test.ts
# runs over tarballs of its own. Without them every install loses all ten skills and
# the dispatch-first hook hooks/hooks.json runs on every session throws.
packages/claude-envoy/scripts/check-packed-skills.sh packages/claude-envoy/*.tgz
