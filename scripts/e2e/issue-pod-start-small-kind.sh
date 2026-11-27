#!/usr/bin/env bash
# The issue-pod start proof of issue-pod-start-kind.sh on a small repository, tmux/tmux (master):
# the same two arms, base and then head, on one runner, each a throwaway kind cluster with two
# issue pods under gVisor, and the same reads of each arm's second pod, with the verdict that the
# head's workspace-init run is at most the base's plus 10 s, the before-merge reading of "a pod of a
# small repository starts no slower". It runs issue-pod-start-kind.sh --small, whose header says
# what the run needs, reads and keeps.
#
# Run it from a checkout on an amd64 (x86_64) Linux host whose HEAD is pushed to
# cybermonklabs/legion, as
#   bash scripts/e2e/issue-pod-start-small-kind.sh
# with what issue-pod-start-kind.sh needs. .github/e2e-scripts.json marks it dispatch_only, as that
# script is: `gh workflow run e2e.yaml --ref <branch> -f script=issue-pod-start-small-kind`.
# ISSUE_POD_START_SMALL_EVIDENCE_DIR (default a fresh /tmp directory, kept and printed) keeps its
# transcript and each arm's evidence.
set -euo pipefail

exec bash "$(cd "$(dirname "$0")" && pwd)/issue-pod-start-kind.sh" --small
