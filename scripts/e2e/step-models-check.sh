#!/usr/bin/env bash
# The comparison, after the release, of the step records of one or more issues with their agents'
# transcripts. It also reads the daemon's log lines of the reads each record used. It runs the
# check c-real word for word. Beside it, it runs live-at-both-reads, which is not c-real.
#
#   scripts/e2e/step-models-check.sh --issue <KEY> [--issue <KEY> …] --daemon-log <file> \
#     --agents <file> [--lower-one]
#
# It runs `dispatch steps --issue <KEY>` once for each --issue, in the order given, and prints
# what each run printed. lib/step-models-check.ts then judges the records. --daemon-log names the
# daemon's JSON log, whose lines "workflow: a step used a read of its agent's calls" it reads.
# --agents names a JSON file that gives each claim its harness (omp or claude), its transcript
# and its setModel. setModel is the model that the deployment's settings give the claim's role.
# Read it from those settings, never from a record, and keep a copy of the settings as evidence.
#
# For each harness of the agents file, c-real takes the first step that its agent ended. Both
# reads of that step must lie more than 2 s from any response of the transcript. The end read
# leaves out the response of the step's completion call, a call of the legion tool with the op
# handoff_complete or sign_off. The step's set model must be the settings'. Its counts must be
# known, at least 1, and equal to the transcript's responses between the two reads, with the
# completion's response. No record may name a model its transcript does not hold.
# live-at-both-reads holds a step that another fact ended to the same four things. --lower-one
# lowers the first count of each taken step by 1, the negative control. Both verdicts then FAIL.
#
# Its last two lines are `c-real: PASS|FAIL|UNKNOWN` and `live-at-both-reads: PASS|FAIL|UNKNOWN`.
# It exits with the c-real verdict's code: 0, 1 or 2. It answers UNKNOWN, exit 2, for a harness
# with no step to take. It answers UNKNOWN too when it could read nothing to judge. That is a
# failed command, a line of other than 16 words, or a log with no read line. So does a log line
# that holds the read line's message and is no JSON, and a record whose agent the agents file
# lacks. A usage error is exit 2 too, with the usage on stderr.
#
# It runs where the operator runs `dispatch` by hand, with the operator's own credential. That is
# the `dispatch` command on PATH (packages/envoy-client's bin), and the session the shell names
# (DISPATCH_HOST and DISPATCH_SESSION_ID). It reaches the Dispatch and token that envoy.json, or
# DISPATCH_URL and DISPATCH_TOKEN, name. It also needs `bun` on PATH for lib/step-models-check.ts.
# It sets no credential, reads none of its own, and writes only a scratch directory it removes.
set -euo pipefail

usage="usage: scripts/e2e/step-models-check.sh --issue <KEY> [--issue <KEY> …] --daemon-log <file> --agents <file> [--lower-one]"
case ${BASH_SOURCE[0]} in */*) here=${BASH_SOURCE[0]%/*} ;; *) here=. ;; esac
# shellcheck source-path=SCRIPTDIR source=lib/dispatch-read.sh
. "$here/lib/dispatch-read.sh"

# Each way out ends with both verdict lines. A run that could read nothing to judge is UNKNOWN for
# both. This replaces the read_unknown of lib/dispatch-read.sh, which its reads call.
read_unknown() {
  echo "UNKNOWN: $*"
  echo "c-real: UNKNOWN"
  echo "live-at-both-reads: UNKNOWN"
  exit 2
}
refuse_usage() {
  echo "$usage" >&2
  read_unknown "$1"
}

issues=()
daemon_log=
agents=
lower_one=()
while [ "$#" -gt 0 ]; do
  case $1 in
    --issue)
      [ "$#" -ge 2 ] && [[ $2 =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || refuse_usage "--issue takes the key of one issue, such as LEGION-1"
      for seen in "${issues[@]}"; do
        [ "$seen" != "$2" ] || refuse_usage "--issue $2 is given twice"
      done
      issues+=("$2")
      shift 2
      ;;
    --daemon-log | --agents)
      [ "$#" -ge 2 ] && [ -n "$2" ] || refuse_usage "$1 takes the path of a file"
      if [ "$1" = --daemon-log ]; then daemon_log=$2; else agents=$2; fi
      shift 2
      ;;
    --lower-one)
      lower_one=(--lower-one)
      shift
      ;;
    *) refuse_usage "it takes no argument $1" ;;
  esac
done
[ "${#issues[@]}" -gt 0 ] && [ -n "$daemon_log" ] && [ -n "$agents" ] ||
  refuse_usage "it needs at least one --issue, a --daemon-log and an --agents"

read_require_dispatch
command -v bun >/dev/null || read_unknown "no bun command is on PATH, which runs lib/step-models-check.ts"
answers=$(mktemp -d)
trap 'rm -rf "$answers"' EXIT
steps=()
for issue in "${issues[@]}"; do
  read_dispatch steps --issue "$issue"
  printf '%s\n' "$read_out" >"$answers/$issue.txt"
  steps+=(--steps "$issue" "$answers/$issue.txt")
done

# The comparison prints both verdicts last, and exits 0, 1 or 2. Any other end is a comparison
# that judged nothing.
code=0
judged=$(bun "$here/lib/step-models-check.ts" "${steps[@]}" --daemon-log "$daemon_log" --agents "$agents" "${lower_one[@]}") || code=$?
printf '%s\n' "$judged"
case $code:${judged##*$'\n'} in
  [012]:"live-at-both-reads: "*) exit "$code" ;;
esac
read_unknown "lib/step-models-check.ts exited $code and printed no verdict"
