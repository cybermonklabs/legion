#!/usr/bin/env bash
# Capture the first model input of every Legion launch against one subject checkout.
# See scripts/e2e/README.md for the evidence, reference controls, and optional real-model run.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd -P)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"

subject=$root
base=ecd967b17a4e
refs=$root/packages/daemon/internal/daemon/testdata/prompt-inputs/refs-base.json
reads=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --subject|--base|--refs)
      [ "$#" -ge 2 ] && [ -n "$2" ] || refuse "$1 needs a value"
      case "$1" in
        --subject) subject=$2 ;;
        --base) base=$2 ;;
        --refs) refs=$2 ;;
      esac
      shift 2 ;;
    --reads) reads=1; shift ;;
    *) refuse "usage: $0 [--subject <checkout>] [--base <rev>] [--refs <file>] [--reads]" ;;
  esac
done

[[ ${LEGION_OMP_PATH:-} = /* && -x ${LEGION_OMP_PATH:-} ]] ||
  refuse 'set LEGION_OMP_PATH to an absolute Oh My Pi executable'
subject=$(cd "$subject" && pwd -P) || refuse 'cannot open the subject checkout'
case "$refs" in /*) ;; *) refs=$PWD/$refs ;; esac
[ -f "$refs" ] || refuse "no reference table at $refs"
[ -d "$subject/node_modules" ] || refuse 'run bun install --frozen-lockfile in the subject checkout'
if [ "$reads" = 1 ]; then
  [ -n "${ANTHROPIC_BASE_URL:-}" ] || refuse '--reads requires ANTHROPIC_BASE_URL'
fi
export LEGION_PROMPT_INPUTS_SUBJECT="$subject" LEGION_PROMPT_INPUTS_BASE="$base"
export LEGION_PROMPT_INPUTS_REFS="$refs" LEGION_PROMPT_INPUTS_READS="$reads"
export LEGION_E2E_REPO_ROOT="$root"
# Keep the caller's short scratch path for Claude Code's Unix socket. The wrapper adds a
# proof-specific component to TMPDIR before it starts the driver.
export LEGION_PROMPT_INPUTS_SOCKET_DIR="${TMPDIR:-/tmp}"
live_daemon_proof 'prompt inputs' prompt-inputs TestPromptInputsLive "${PROMPT_INPUTS_EVIDENCE_DIR:-}" 30m
