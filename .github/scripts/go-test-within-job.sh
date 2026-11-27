#!/usr/bin/env bash
# go-test-within-job.sh go test <flags and packages>: runs that go test with a -timeout that ends
# inside the CI job running it: what is left of JOB_TIMEOUT_MINUTES when the go test starts, less
# 60 s for go to print the test it stops and for the job's steps after it. A test whose own waits
# end only near the binary's deadline (testwait) then fails by name, where the job's limit would
# cancel the job naming no test.
#
# The job's start is read off the runner's per-job process, Runner.Worker, among this script's
# parents, as .github/scripts/gh-read.sh reads it: `ps -o etimes=` of that process is how long the
# job has run. JOB_TIMEOUT_MINUTES is the job's timeout-minutes, which check-workflow-files.sh
# holds equal to it.
#
# It refuses, exit 2 and nothing run: a command that is not `go test`, one that sets -timeout
# itself, a JOB_TIMEOUT_MINUTES that is not a whole number, a process tree with no Runner.Worker.
# It fails, exit 1 and nothing run, when less than 60 s would be left for the go test. Otherwise
# it says the -timeout it gives and runs the go test in its place (exec), so the go test's own
# status is the step's.
#
# go-test-step-expansion.py reads a line run under this script as the go test behind it
# (GO_WRAPPERS): the -timeout changes no test the command runs.
#
# Run from anywhere; its test is .github/scripts/go-test-within-job.test.sh.
set -euo pipefail

# What the go test leaves the job: the time go takes to print the test its alarm stops, and the
# steps after it (the report step took 0 s in each of envoy-infra-go's seven green merge-group runs
# of 2026-11-22 15:41Z to 16:48Z).
spare=60

if [ "$#" -lt 2 ] || [ "$1" != go ] || [ "$2" != test ]; then
  echo "::error::go-test-within-job.sh runs a go test: give it \`go test <flags and packages>\`, not \`$*\`" >&2
  exit 2
fi
shift 2
for arg in "$@"; do
  case $arg in
    -timeout | -timeout=* | --timeout | --timeout=* | -test.timeout | -test.timeout=*)
      echo "::error::go-test-within-job.sh gives the go test its -timeout; drop \`$arg\`" >&2
      exit 2
      ;;
  esac
done
limit=${JOB_TIMEOUT_MINUTES:-}
if ! [[ $limit =~ ^[0-9]+$ ]]; then
  echo "::error::go-test-within-job.sh: JOB_TIMEOUT_MINUTES is '$limit', not the job's timeout-minutes as a whole number" >&2
  exit 2
fi

worker=$$
until [ "$(ps -o comm= -p "$worker")" = Runner.Worker ]; do
  worker=$(ps -o ppid= -p "$worker" | tr -d ' ')
  if [ "${worker:-1}" -le 1 ]; then
    echo "::error::go-test-within-job.sh: no Runner.Worker among this process's parents, so the job's start cannot be read" >&2
    exit 2
  fi
done
ran=$(ps -o etimes= -p "$worker" | tr -d ' ')
left=$((limit * 60 - ran - spare))
if [ "$left" -lt 60 ]; then
  echo "::error::go-test-within-job.sh: the job has run ${ran} s of its ${limit} min, which leaves ${left} s after ${spare} s for what follows the go test: too few to run it in" >&2
  exit 1
fi
echo "go-test-within-job.sh: the job has run ${ran} s of its ${limit} min; go test -timeout ${left}s"
exec go test -timeout "${left}s" "$@"
