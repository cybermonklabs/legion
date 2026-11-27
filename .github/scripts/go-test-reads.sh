#!/usr/bin/env bash
# go-test-reads.sh [--env NAME=VALUE]... <workdir> <package>...
# What `go test <package>...` run in <workdir> reads, as check-ci-filters.sh holds a gate to it:
# ci_filter_coverage.py's closure, with ci_job_reads.py's RULES. Prints one JSON object,
# {"reads": [<path or glob>...], "problems": [<message>...]}, and exits 1 when a problem leaves
# the closure unknown, 2 on a usage fault. packages/envoy/cmd/spec-check reads it as the tie of a
# check of The Spec (specgraph.ScriptTestReads). It needs go and a python3 with PyYAML.
set -euo pipefail
cd "$(dirname "$0")/../.."
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts exec python3 -c '
import json
import sys

import ci_filter_coverage

args, env = sys.argv[1:], {}
while len(args) >= 2 and args[0] == "--env":
    name, _, value = args[1].partition("=")
    env[name] = value
    args = args[2:]
if len(args) < 2:
    print("usage: go-test-reads.sh [--env NAME=VALUE]... <workdir> <package>...", file=sys.stderr)
    sys.exit(2)
reads, problems = ci_filter_coverage.test_reads(args[0], args[1:], env)
print(json.dumps({"reads": reads, "problems": problems}))
sys.exit(1 if problems else 0)
' "$@"
