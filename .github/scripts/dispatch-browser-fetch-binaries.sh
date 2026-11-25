#!/usr/bin/env bash
# The second read of the binaries artifact in the shard and alone jobs of
# .github/workflows/dispatch-browser.yaml, run when actions/download-artifact's first read did
# not complete within its step's limit.
#
# Who runs it, and what it takes:
#
#   GH_TOKEN=<token> .github/scripts/dispatch-browser-fetch-binaries.sh <out-dir> <job>
#
# <out-dir> is the directory the artifact's files are unzipped into, which the step after it
# copies from. <job> is the id of the job that calls the workflow, its `${{ inputs.job }}`: the
# artifact is `<job>-binaries`, as the build job names it, so a run of ci.yaml that runs both of
# its callers (a pull request labelled full-ci) reads each caller's own artifact and never the
# other's. GH_TOKEN is the run's own `${{ github.token }}`, a key of the step's `env:` and
# never an argument: an argument is on the process's command line, which every process of the
# runner can read in /proc, so the token reaches curl through a header file this script writes
# with mode 0600. The runner sets the other three names it reads for every step:
# GITHUB_API_URL, GITHUB_REPOSITORY and GITHUB_RUN_ID.
#
# Why a script and not the action a second time:
# .github/ci-notes/dispatch-browser.md#why-the-second-read-of-the-binaries-is-a-script. In short:
# a stalled read is the runner's own connection to blob storage going slow or silent while the
# same blob serves other runners in seconds, and the action (v8.0.1 and v8.0.2 alike) ends a
# connection only after 30 s with no byte at all, starts again from byte 0, and takes no input
# that changes either. This script reads the same artifact by its REST address, ends an attempt
# whose connection stays below DISPATCH_BROWSER_FETCH_SPEED_LIMIT bytes a second for
# DISPATCH_BROWSER_FETCH_SPEED_TIME seconds (curl's --speed-limit and --speed-time), and resumes
# the next attempt at the byte the last one reached (curl's --continue-at), on a new connection.
#
# Both of its reads go through retry_fetch, .github/scripts/ci-retry.sh, the one retry every
# download of this repository's CI goes through (dispatch://INFRA-1545, R2): the read of the
# run's artifact list, and the attempts at the zip. So the zip is read in at most five attempts,
# with the helper's waits of 10, 20, 40 and 80 s between them. Each attempt is one run of the
# `bash -c` text below, which holds one curl and no loop.
#
# Each attempt prints one line: curl's exit status, the HTTP status, the bytes it read and at
# what speed, the bytes on disk against the artifact's size, and the TCP segments this network
# namespace retransmitted during it (/proc/net/snmp), so a stalled read says whether the
# connection was silent or slow and whether packets were lost. The line goes to standard error,
# because retry_fetch withholds a failed attempt's standard output, and a failed attempt's line
# is the one a stalled read needs. The finished zip is held to the artifact's own sha256 digest
# before it is unzipped. An attempt that cannot be continued (curl's exit 33, or more bytes on
# disk than the artifact's size) and an attempt whose whole zip fails the digest delete the zip,
# so the next attempt starts again from byte 0.
#
# The step's `timeout-minutes` holds the ladders of both reads, and the step's own comment
# derives it. DISPATCH_BROWSER_FETCH_SPEED_LIMIT (1048576) and DISPATCH_BROWSER_FETCH_SPEED_TIME
# (10) are read for .github/scripts/dispatch-browser-fetch-binaries.test.sh, which runs them at a
# stand-in server's scale; the workflow sets neither.
#
# Every line this script prints goes to standard error, where it stays in order with the lines
# and the stop-commands blocks of retry_fetch (ci-retry.sh's header, its third stated limit).
#
# Run from anywhere: .github/scripts/dispatch-browser-fetch-binaries.sh <out-dir> <job>
# CI runs it in dispatch-browser.yaml's shard and alone jobs, at the step "Download the binaries
# again, after a first attempt that did not complete", and its tests in ci.yaml's test job.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "::error::dispatch-browser-fetch-binaries.sh takes the directory to unzip into and the calling job's" \
    "id, and was given $# argument(s): .github/scripts/dispatch-browser-fetch-binaries.sh <out-dir> <job>" >&2
  exit 1
fi
out=$1
name=$2-binaries
api=${GITHUB_API_URL:?GITHUB_API_URL must name the REST API}
repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must name the repository as owner/repo}
run=${GITHUB_RUN_ID:?GITHUB_RUN_ID must name this run}
: "${GH_TOKEN:?GH_TOKEN must hold the token of this run, which reads its artifacts}"
speed_limit=${DISPATCH_BROWSER_FETCH_SPEED_LIMIT:-1048576}
speed_time=${DISPATCH_BROWSER_FETCH_SPEED_TIME:-10}

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/ci-retry.sh
. "$script_dir/ci-retry.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
(umask 077 && printf 'Authorization: Bearer %s\n' "$GH_TOKEN" > "$work/auth")
mkdir -p "$out"

# TCP segments this network namespace has retransmitted so far, or `unknown` where /proc gives
# no count: an awk program over /proc/net/snmp, which each attempt runs before and after its
# curl. A job in a container has a namespace of its own, so the count is the leg's.
# shellcheck disable=SC2016 # awk's fields, not the shell's
retransmitted='$1 == "Tcp:" { if (!seen) { for (i = 2; i <= NF; i++) if ($i == "RetransSegs") at = i; seen = 1 }
    else if (at) { print $at; found = 1 } }
  END { if (!found) print "unknown" }'

# The artifact actions/download-artifact reads under the name: the one with the highest id among
# the run's artifacts of that name. A re-run uploads the binaries again under the same name, and
# the first read, given a name and no run id, lists the run's artifacts of that name and takes
# the highest id (@actions/artifact's getArtifactInternal, "returning newest"); run 45306887784
# held three by its attempt 3. This read takes that same artifact, and its address, size and
# digest all come from that one selection. One read of the run's artifact list, at 30 s an
# attempt: .github/ci-budgets.md's Retries note gives that bound to a `gh api` read, and this is
# the same read of the same REST API.
retry_fetch "run $run's list of artifacts named $name" 30 -- \
  curl -sS --fail --connect-timeout 10 -H @"$work/auth" -H 'Accept: application/vnd.github+json' \
  -o "$work/artifacts.json" \
  "$api/repos/$repository/actions/runs/$run/artifacts?name=$name&per_page=100"
jq --arg name "$name" '[.artifacts[] | select(.name == $name)] | sort_by(.id) | last // empty' \
  "$work/artifacts.json" > "$work/artifact.json"
if [ ! -s "$work/artifact.json" ]; then
  echo "::error::run $run holds no artifact named $name, so there is nothing to read again" >&2
  exit 1
fi
held=$(jq --arg name "$name" '[.artifacts[] | select(.name == $name)] | length' "$work/artifacts.json")
id=$(jq -r '.id' "$work/artifact.json")
if [ "$(jq -r '.expired' "$work/artifact.json")" != false ]; then
  echo "::error::the newest artifact named $name in run $run, id $id, has expired, so there is" \
    "nothing to read again" >&2
  exit 1
fi
url=$(jq -r '.archive_download_url' "$work/artifact.json")
size=$(jq -r '.size_in_bytes' "$work/artifact.json")
digest=$(jq -r '.digest // ""' "$work/artifact.json")
echo "artifact $name, id $id, the highest of the $held the run holds under that name: $size bytes," \
  "digest ${digest:-none given}" >&2

# The attempts at the zip. retry_fetch runs the `bash -c` text below as its program, so the
# attempts are the helper's five and no more. The text gets the work directory, the zip's path,
# the artifact's address, size and digest, the two low-speed settings and the awk program above
# as $1 to $8. It counts its own attempt in $work/attempts, because retry_fetch tells its command
# nothing of which attempt it is. --location follows the API's redirect to the blob's signed
# address; curl sends a header given with -H to the host it was given for and to no other, so the
# token stays with the API. --continue-at - starts where the file on disk ends, which is byte 0
# on the first attempt and after an attempt that deleted the zip. The text writes nothing on
# standard output: its line goes to standard error, and curl's answer to the zip and to files of
# the work directory.
#
# THE BOUND, 150 s AN ATTEMPT, IS READ OFF THE ARTIFACT'S SIZE. The zip is about 107 MB. A read
# held just above the speed limit of 1 MiB a second, which the low-speed bound never ends, takes
# 107,000,000 / 1,048,576 = 102 s. curl opens two connections, to the API and then to the blob's
# host, and each may spend its 10 s connect timeout, so such a read takes 122 s at most. curl's
# own --max-time 140 ends the transfer above that, so an attempt that reaches it still prints its
# line and leaves its bytes on disk. retry_fetch's 150 s is the backstop 10 s above it, for the
# attempt's own work after curl, the digest of the zip and the second read of /proc, as
# dispatch-browser-run-reads.sh keeps its ladder's bound 10 s above its inner one. An attempt that
# either bound ends loses nothing, since the next attempt resumes at the byte it reached.
printf '0' > "$work/attempts"
zip="$work/$name.zip"
# shellcheck disable=SC2016 # the shell retry_fetch runs expands $1 to $8 and its own variables
if ! retry_fetch "the $name artifact" 150 -- bash -c '
  attempt=$(( $(cat "$1/attempts") + 1 ))
  printf "%s" "$attempt" > "$1/attempts"
  before=$(awk "$8" /proc/net/snmp 2> /dev/null) || before=unknown
  status=0
  curl -sS --fail --location --connect-timeout 10 --max-time 140 \
    --speed-limit "$6" --speed-time "$7" --continue-at - -H @"$1/auth" \
    -w "%{http_code} %{size_download} %{speed_download} %{time_total}\n" \
    -o "$2" "$3" > "$1/written" 2> "$1/error" || status=$?
  after=$(awk "$8" /proc/net/snmp 2> /dev/null) || after=unknown
  read -r code got speed took < "$1/written"
  have=$(stat -c %s "$2" 2> /dev/null || echo 0)
  lost=unknown
  [ "$before" = unknown ] || [ "$after" = unknown ] || lost=$((after - before))
  echo "attempt $attempt: curl exit $status, HTTP ${code:-none}, ${got:-0} bytes in ${took:-0} s" \
    "(${speed:-0} bytes/s), $have of $4 bytes on disk, $lost TCP segments retransmitted" \
    "$(head -c 300 "$1/error" | tr "\n" " ")" >&2
  failed=$status
  [ "$failed" -ne 0 ] || failed=1
  if [ "$have" -gt "$4" ] || [ "$status" -eq 33 ]; then
    echo "attempt $attempt cannot be continued from byte $have;" \
      "the next attempt starts again from byte 0" >&2
    rm -f "$2"
    exit "$failed"
  fi
  [ "$have" -eq "$4" ] || exit "$failed"
  if [ -n "$5" ] && [ "sha256:$(sha256sum "$2" | cut -d " " -f 1)" != "$5" ]; then
    echo "attempt $attempt read all $4 bytes, and their sha256 is not the one the artifact" \
      "names, $5; the next attempt starts again from byte 0" >&2
    rm -f "$2"
    exit 1
  fi
' fetch-attempt "$work" "$zip" "$url" "$size" "$digest" "$speed_limit" "$speed_time" \
  "$retransmitted"; then
  echo "::error::the $name artifact was not read whole in $(cat "$work/attempts") attempts" \
    "($(stat -c %s "$zip" 2> /dev/null || echo 0) of $size bytes on disk, each attempt's line" \
    "above says how it ended): a failed read of the run's artifact, not a failed browser row" >&2
  exit 1
fi
unzip -q -o "$zip" -d "$out"
echo "unzipped $name into $out: $(find "$out" -type f | wc -l) files" >&2
