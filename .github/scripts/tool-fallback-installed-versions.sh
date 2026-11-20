#!/usr/bin/env bash
# What the head's own installs under the fault left on this runner: every tool is held to the
# version its row pins, the docker provider plugin's tarball to its row's digest, the Bun install
# to the bunx beside it, and the fault proxy is asked how many requests to github.com it refused
# since the probe, which is the half a version and a digest cannot say.
#
# Who runs it: the step "Check every tool is the version its row pins" of
# .github/workflows/ci-job-tool-fallback.yaml's tool-fallback job; the comments below say what each of
# those readings answers for. Its one argument is the docker provider plugin's tarball, which the
# install-docker-plugin step hands back as `steps.docker-plugin.outputs.tarball`: a step's output
# is a workflow expression, which no script can read, so the step passes it. That job checks this
# repository out in its first step, above every step below it, so this script, the helper it
# sources and the rig it runs are all in the checkout when the step runs.
#
# Run from the repository root: LEGION_TEST_UV=<uv binary> .github/scripts/tool-fallback-installed-versions.sh <tarball>
# LEGION_TEST_UV is the uv binary the install-uv action fetched, which that action writes to
# GITHUB_ENV; the script runs it for uv's version. It reads it only inside a command
# substitution, so an unset one ends that subshell alone (`set -u`): uv's reading is empty, and
# the script ends naming uv among the tools that answered a version their rows do not pin.
# CI runs it in ci-job-tool-fallback.yaml's tool-fallback job, at the step "Check every tool is
# the version its row pins".
set -euo pipefail

# shellcheck source=.github/scripts/ci-tools.sh
. .github/scripts/ci-tools.sh
proof=.github/scripts/ci-tools-proof.sh
wrong=""
"$proof" version-answers crane "$(crane version)" || wrong="$wrong crane"
"$proof" version-answers changie "$(changie --version)" || wrong="$wrong changie"
"$proof" version-answers jj "$(jj --version)" || wrong="$wrong jj"
"$proof" version-answers bun-linux-x64 "$(bun --version)" || wrong="$wrong bun-linux-x64"
"$proof" version-answers elan "$(elan --version)" || wrong="$wrong elan"
"$proof" version-answers uv "$("$LEGION_TEST_UV" --version)" || wrong="$wrong uv"
"$proof" version-answers pulumi-cli "$(pulumi version)" || wrong="$wrong pulumi-cli"
# The docker provider plugin prints no version of its own: nothing is installed, and
# what the action hands back is the tarball the live tests are given. Its reading is
# therefore the row's own digest, read by the one digest reader of this repository.
# The name of that tarball is no reading at all: install-docker-plugin builds it
# from `ci_tools_field pulumi-docker version`, the field this step would compare it
# with, so the two cannot disagree. What holds the name to the constant the live
# tests read is .github/scripts/check-ci-tools.sh, in lint.
# A digest is no version, so this keeps its own word as the bunx reading does rather
# than joining the list below, whose sentence is about versions.
plugin='read'
tarball="$1"
echo "pulumi-docker is the tarball $tarball, and its row pins $(ci_tools_field pulumi-docker version)"
ci_tools_verify pulumi-docker "$tarball" "the docker provider plugin tarball this job installed" ||
  plugin=refused
# The name that Bun install must have left beside the bun one of those lines read. It
# is a reading of its own and not one more version-answers line because what can be
# missing here is the name rather than the version: npm serves the binary alone, so a
# faulted install that left no bunx behind answers every reading above and fails at
# the first bunx a later job runs. The verb prints which of the three things it reads
# — the name, the version, the bytes — was wrong, so this keeps its own word and does
# not join the list below, whose sentence is about versions.
bunx='read'
"$proof" installed-bunx || bunx=refused
# The other half of every reading above: how many requests to github.com the fault
# proxy refused since the probe, and which. A version says which tool is here and a
# digest says it is the pinned one; neither can say that the first source did not
# serve it, because a row's two sources are one binary by design. The 504s say it.
refusals='read'
"$proof" refusals github.com || refusals=refused
if [ -n "$wrong" ]; then
  echo "::error::$wrong answered a version its row does not pin, so the source that served it served something else" >&2
fi
if [ -n "$wrong" ] || [ "$plugin" = refused ] || [ "$bunx" = refused ] || [ "$refusals" = refused ]; then
  exit 1
fi
