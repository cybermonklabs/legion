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
# copies from. <job> is the id of the caller whose binaries job built the artifact: the
# artifact is `<job>-binaries`, as the build job names it, and the step passes the name its
# first read takes, `${{ inputs.build-job || inputs.job }}` in the shard job and
# `${{ inputs.job }}` in the alone job. So each caller reads its own build, even when several
# callers share a run, but the references lane where it takes the pull-request lane's, which
# both reads then name. GH_TOKEN is the run's own `${{ github.token }}`, a key
# of the step's `env:` and
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
# Each attempt prints one line: curl's exit status, the HTTP status, the bytes it read and at
# what speed, the bytes on disk against the artifact's size, and the TCP segments this network
# namespace retransmitted during it (/proc/net/snmp), so a stalled read says whether the
# connection was silent or slow and whether packets were lost. The finished zip is held to the
# artifact's own sha256 digest before it is unzipped.
#
# The step's `timeout-minutes` bounds the whole read; DISPATCH_BROWSER_FETCH_ATTEMPTS (8) attempts
# of at most DISPATCH_BROWSER_FETCH_SPEED_TIME (10) seconds of a slow connection fit in it. The
# three names are read for .github/scripts/dispatch-browser-fetch-binaries.test.sh, which runs
# them at a stand-in server's scale; the workflow sets none of them.
#
# Run from anywhere: .github/scripts/dispatch-browser-fetch-binaries.sh <out-dir> <job>
# CI runs it in dispatch-browser.yaml's shard and alone jobs, at the step "Download the binaries
# again, after a first attempt that did not complete", and its tests in ci.yaml's test job.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "::error::dispatch-browser-fetch-binaries.sh takes the directory to unzip into and the calling job's" \
    "id, and was given $# argument(s): .github/scripts/dispatch-browser-fetch-binaries.sh <out-dir> <job>"
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
attempts=${DISPATCH_BROWSER_FETCH_ATTEMPTS:-8}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
(umask 077 && printf 'Authorization: Bearer %s\n' "$GH_TOKEN" > "$work/auth")
mkdir -p "$out"

# TCP segments this network namespace has retransmitted so far, or `unknown` where /proc gives
# no count. A job in a container has a namespace of its own, so the count is the leg's.
retransmitted() {
  awk '$1 == "Tcp:" { if (!seen) { for (i = 2; i <= NF; i++) if ($i == "RetransSegs") at = i; seen = 1 }
         else if (at) { print $at; found = 1 } }
       END { if (!found) print "unknown" }' /proc/net/snmp 2>/dev/null || echo unknown
}

# The artifact actions/download-artifact reads under the name: the one with the highest id among
# the run's artifacts of that name. A re-run uploads the binaries again under the same name, and
# the first read, given a name and no run id, lists the run's artifacts of that name and takes
# the highest id (@actions/artifact's getArtifactInternal, "returning newest"); run 45306887784
# held three by its attempt 3. This read takes that same artifact, and its address, size and
# digest all come from that one selection. One read of the run's artifact list, with curl's own
# retries for a refused connection or a 5xx.
curl -sS --fail --connect-timeout 10 --max-time 30 --retry 3 --retry-all-errors \
  -H @"$work/auth" -H 'Accept: application/vnd.github+json' \
  -o "$work/artifacts.json" \
  "$api/repos/$repository/actions/runs/$run/artifacts?name=$name&per_page=100"
jq --arg name "$name" '[.artifacts[] | select(.name == $name)] | sort_by(.id) | last // empty' \
  "$work/artifacts.json" > "$work/artifact.json"
if [ ! -s "$work/artifact.json" ]; then
  echo "::error::run $run holds no artifact named $name, so there is nothing to read again"
  exit 1
fi
held=$(jq --arg name "$name" '[.artifacts[] | select(.name == $name)] | length' "$work/artifacts.json")
id=$(jq -r '.id' "$work/artifact.json")
if [ "$(jq -r '.expired' "$work/artifact.json")" != false ]; then
  echo "::error::the newest artifact named $name in run $run, id $id, has expired, so there is" \
    "nothing to read again"
  exit 1
fi
url=$(jq -r '.archive_download_url' "$work/artifact.json")
size=$(jq -r '.size_in_bytes' "$work/artifact.json")
digest=$(jq -r '.digest // ""' "$work/artifact.json")
echo "artifact $name, id $id, the highest of the $held the run holds under that name: $size bytes," \
  "digest ${digest:-none given}"

zip="$work/$name.zip"
done=false
for attempt in $(seq 1 "$attempts"); do
  before=$(retransmitted)
  # --location follows the API's redirect to the blob's signed address; curl sends a header
  # given with -H to the host it was given for and to no other, so the token stays with the
  # API. --continue-at - starts where the file on disk ends, which is 0 on the first attempt.
  status=0
  curl -sS --fail --location --connect-timeout 10 \
    --speed-limit "$speed_limit" --speed-time "$speed_time" \
    --continue-at - -H @"$work/auth" \
    -w '%{http_code} %{size_download} %{speed_download} %{time_total}\n' \
    -o "$zip" "$url" > "$work/written" 2> "$work/error" || status=$?
  after=$(retransmitted)
  read -r code got speed took < "$work/written" || true
  have=$(stat -c %s "$zip" 2>/dev/null || echo 0)
  if [ "$before" = unknown ] || [ "$after" = unknown ]; then
    lost=unknown
  else
    lost=$((after - before))
  fi
  echo "attempt $attempt: curl exit $status, HTTP ${code:-none}, ${got:-0} bytes in ${took:-0} s" \
    "(${speed:-0} bytes/s), $have of $size bytes on disk, $lost TCP segments retransmitted" \
    "$(head -c 300 "$work/error" | tr '\n' ' ')"
  # curl's 33 is a server that answered a resumed read with the whole object rather than the
  # range asked for: what is on disk cannot be continued, so the next attempt starts from 0.
  if [ "$have" -gt "$size" ] || [ "$status" -eq 33 ]; then
    echo "attempt $attempt cannot be continued from byte $have; the next attempt starts again from byte 0"
    rm -f "$zip"
    continue
  fi
  [ "$have" -eq "$size" ] || continue
  if [ -n "$digest" ] && [ "sha256:$(sha256sum "$zip" | cut -d' ' -f1)" != "$digest" ]; then
    echo "attempt $attempt read all $size bytes, and their sha256 is not the artifact's $digest;" \
      "the next attempt starts again from byte 0"
    rm -f "$zip"
    continue
  fi
  done=true
  break
done
if [ "$done" != true ]; then
  echo "::error::the $name artifact was not read whole in $attempts attempts" \
    "($(stat -c %s "$zip" 2>/dev/null || echo 0) of $size bytes, each attempt's line above says" \
    "how it ended): a failed read of the run's artifact, not a failed browser row"
  exit 1
fi
unzip -q -o "$zip" -d "$out"
echo "unzipped $name into $out: $(find "$out" -type f | wc -l) files"
