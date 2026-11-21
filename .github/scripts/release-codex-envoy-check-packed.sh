#!/usr/bin/env bash
# The packed @sjawhar/codex-envoy tarball, read back before the publish job is handed it: it names
# the publishing repository, it declares this release's version in both of its manifests, it holds
# the bundles and the hooks file the installed plugin runs from, it holds the `dispatch` command a
# terminal runs as its npm bin, and it holds the skills.
#
# Who runs it, and what it takes:
#
#   .github/scripts/release-codex-envoy-check-packed.sh
#
# No arguments. It reads two variables of the step's environment: $VERSION, the version this run
# releases, which the step's `env:` carries from the version step, and $RUNNER_TEMP, the runner's
# scratch directory, where the step "Build and pack the Codex plugin" left the tarball, in
# codex-envoy-tarball/. It asks .github/scripts/repo-paths.sh for this repository's url and runs
# packages/claude-envoy/scripts/check-packed-skills.sh, each named by its path from the repository
# root, so that is the working directory.
#
# Run from the repository root: VERSION=<version> RUNNER_TEMP=<dir> .github/scripts/release-codex-envoy-check-packed.sh
# CI runs it in release.yaml's claude_envoy_build job, at the step "Check the packed Codex tarball
# names the publishing repository and the release, and holds the build and the skills".
set -euo pipefail

# The Claude Code tarball's checks (release-claude-envoy-check-packed.sh), over the Codex package: its repository, the
# version npm and Codex read, the bundles its MCP server and its `dispatch` tool run, the hooks file
# Codex reads by default, and the skills as real files.
tgz=("$RUNNER_TEMP"/codex-envoy-tarball/*.tgz)
url=$(.github/scripts/repo-paths.sh repository_url)
tar -xzOf "${tgz[0]}" package/package.json | jq -e --arg u "$url" '.repository.url == $u'
for manifest in package/package.json package/.codex-plugin/plugin.json; do
  tar -xzOf "${tgz[0]}" "$manifest" | jq -e --arg v "$VERSION" '.version == $v'
done
members=$(tar -tzf "${tgz[0]}")
# A here-string and no `printf … | grep -q`, for the reason check-packed-skills.sh gives: grep -q
# exits at its first match, and under pipefail the printf it leaves writing fails the step.
for member in package/dist/envoy-channel.js package/dist/dispatch.js package/hooks/hooks.json; do
  if ! grep -qxF "$member" <<<"$members"; then
    echo "::error::the packed Codex tarball holds no $member, which the installed plugin runs from"
    exit 1
  fi
done
# bin/dispatch, the `dispatch` command a terminal runs: the plugin's `dispatch` tool ends a command
# after 120 s, which `dispatch login --device` outlasts while it waits for a person's approval. It
# is the installed plugin's own and the package's npm bin, so every user must be able to run it and
# the manifest must name it (packages/claude-envoy/tests/codex-packed.test.ts).
if ! grep -qxF package/bin/dispatch <<<"$members"; then
  echo "::error::the packed Codex tarball holds no package/bin/dispatch, the dispatch command a terminal runs"
  exit 1
fi
listing=$(tar -tvzf "${tgz[0]}" package/bin/dispatch)
if [[ $listing != -??x??x??x* ]]; then
  echo "::error::the packed Codex tarball's package/bin/dispatch is not executable by every user: ${listing%% *}"
  exit 1
fi
if ! tar -xzOf "${tgz[0]}" package/package.json | jq -e '.bin.dispatch == "bin/dispatch"' >/dev/null; then
  echo "::error::the packed Codex tarball's package.json does not name bin/dispatch as its dispatch bin"
  exit 1
fi
packages/claude-envoy/scripts/check-packed-skills.sh "${tgz[0]}"
