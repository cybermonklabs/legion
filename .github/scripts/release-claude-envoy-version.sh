#!/usr/bin/env bash
# The version this run releases, written into all five manifests of packages/claude-envoy and its
# Codex package, and the tree's daemon API contract, written into its package.json, before the
# packages are built and packed.
#
# Who runs it, and what it takes:
#
#   .github/scripts/release-claude-envoy-version.sh
#
# No arguments. It reads one variable of the step's environment, $VERSION, the version this run
# releases, which the step's `env:` carries from the version step, and rewrites the five manifests
# of the checkout in place. It names them, and scripts/daemon-contract.sh, by their paths from the
# repository root, so that is the working directory.
#
# Run from the repository root: .github/scripts/release-claude-envoy-version.sh
# CI runs it in release.yaml's claude_envoy_build job, at the step "Set release version".
set -euo pipefail

# All five manifests, and for different readers. package.json's version is what npm
# publishes and what the MCP server reports. .claude-plugin/plugin.json's is what Claude
# Code calls the installed plugin's version: it reads the manifest, never the registry,
# and an install whose manifest declares none lands in a plugin-cache directory named
# `unknown` and never updates, since one `unknown` is indistinguishable from the next
# (measured on Claude Code 2.1.311). The Codex CLI names its plugin cache directory by
# codex/.codex-plugin/plugin.json's and reinstalls when it changes (measured on Codex CLI
# 0.154.0), and the Codex package's package.json is what npm publishes it
# at. .omp-plugin/plugin.json's is what Oh My Pi reports,
# and tests/envoy-channel-server.test.ts holds it equal to package.json's, so a release
# that rewrote the first two alone would publish a plugin naming two versions. The
# committed .claude-plugin manifest declares none, which is what keeps a checkout from
# pinning every install at one string, and this job's tree is discarded, as with the
# rewrite of the job's "Point the manifest at the publishing repository" step.
for manifest in packages/claude-envoy/package.json \
  packages/claude-envoy/.claude-plugin/plugin.json \
  packages/claude-envoy/.omp-plugin/plugin.json \
  packages/claude-envoy/codex/package.json \
  packages/claude-envoy/codex/.codex-plugin/plugin.json; do
  # Two commands, not one `&&` list: `set -e` ignores the failure of any command of such a list
  # but the last, so a manifest jq cannot parse would leave the step green and that manifest
  # unwritten (packages/claude-envoy/tests/release-version.test.ts).
  jq --arg v "$VERSION" '.version = $v' "$manifest" > tmp.json
  mv tmp.json "$manifest"
done

# Claude Envoy's manifest takes `legion.daemonApiVersion`, the contract of this tree
# (scripts/daemon-contract.sh), which no committed manifest names: the daemon built from the same
# commit requires it of the installed manifest, and verify-build.sh, which runs next, bundles this
# manifest into dist/, so the code the plugin runs states the same number. The other four
# manifests carry no contract.
contract=$(bash scripts/daemon-contract.sh)
jq --argjson contract "$contract" '.legion.daemonApiVersion = $contract' \
  packages/claude-envoy/package.json > tmp.json
mv tmp.json packages/claude-envoy/package.json
