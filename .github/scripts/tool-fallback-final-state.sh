#!/usr/bin/env bash
# What this runner holds after the sixteen corrupted payloads: crane, changie and jj each answer
# the version its row pins, the jj on the PATH is held to that row's own digest, and neither the
# bun nor the bunx a refusal took away is back, so not one of those payloads was installed.
#
# Who runs it: the step "Print what every tool answers now" of
# .github/workflows/ci-job-tool-fallback.yaml's tool-fallback job;
# .github/ci-notes/ci-job-tool-fallback.md#what-this-runner-holds-after-the-sixteen-refusals says
# what each install left behind and why. It takes no argument. That job checks this repository
# out in its
# first step, above every step below it, so this script, the helper it sources and the rig it runs
# are all in the checkout when the step runs.
#
# Run from the repository root: .github/scripts/tool-fallback-final-state.sh
# CI runs it in ci-job-tool-fallback.yaml's tool-fallback job, at the step "Print what every tool
# answers now".
set -euo pipefail

# shellcheck source=.github/scripts/ci-tools.sh
. .github/scripts/ci-tools.sh
proof=.github/scripts/ci-tools-proof.sh
wrong=""
"$proof" version-answers crane "$(crane version)" || wrong="$wrong crane"
"$proof" version-answers changie "$(changie --version)" || wrong="$wrong changie"
"$proof" version-answers jj "$(jj --version)" || wrong="$wrong jj"
ci_tools_verify jj "$HOME/.local/bin/jj" "the tool this job leaves on the runner" || wrong="$wrong jj"
for taken in "$HOME/.bun/bin/bun" "$HOME/.bun/bin/bunx"; do
  if [ -e "$taken" ]; then
    echo "::error::$taken is still here, and no case of this job may leave one: the bun-primary case's refusal takes the binary it refused away, and the bun-second-source case's refusal writes none in its place, so a bun here is a refused binary on the PATH of every later step" >&2
    wrong="$wrong $taken"
  else
    echo "$taken is gone: the bun-primary case's refusal took it away, the case after it took the pair away again before its own install, and that case refused the corrupted package before it wrote anything there"
  fi
done
if [ -n "$wrong" ]; then
  echo "::error::$wrong is not what this job must leave on this runner, so one of the sixteen refusals did not hold; the lines above say which reading was wrong" >&2
  exit 1
fi
echo "every tool on this runner is its row's pinned bytes and the refused bun is gone: not one of the sixteen corrupted payloads was installed"
