#!/usr/bin/env bash
# Measures in production the time the server takes to answer the priorities read in each of its two
# forms (the check c-both-forms-production): the p90 of the access log's `elapsed_ms` over three
# hours of production's own reads of GET /api/v1/priorities, read back from the deployment's
# container logs. It sends no read of the priorities itself.
#
# Inputs: DISPATCH_URL (the deployed Dispatch), MERGE_SHA (the merge commit the fleet must carry)
# and LOGGING_PROJECT (the project that holds the deployment's container logs), all three
# required; and FLEET_SINCE, the time an earlier look printed, from which the three hours run.
# WINDOW_START, which only the check's negative control sets, is the start of the three hours: the
# look then skips the test of the fleet's commit and the wait for it.
#
# A look: /healthz's `fleet_commit`, the commit every live server process serves, must carry
# MERGE_SHA by GitHub's compare (`identical` or `ahead`); the three hours from FLEET_SINCE must have
# passed; then `gcloud logging read` gives the access log lines of route /api/v1/priorities in those
# hours, and the look keeps those of `method=GET` and `status=200`. A line carries no query string,
# so its `bytes`, the bytes sent after compression, tell the two forms apart: a line below 2,048
# bytes is the summary (`?summary=true`, two counts and no row), and one at or above it is the full
# read. Each form's p90 is the value at rank ceil(0.9 n) of its lines' `elapsed_ms` in ascending
# order.
#
# Output: for each form, `the full read: p90 N ms over M reads, at most 200 ms` or `..., over 200
# ms` and `the full read: smallest S bytes, largest L bytes`, the same two for `the summary`, and
# `<form>: M reads, fewer than 200` for a form the three hours hold fewer than 200 reads of. A look
# that waits for the three hours to pass, or for a form's 200 reads, ends on `read again at or
# after <time> with FLEET_SINCE=<time>` (WINDOW_START=<time> under WINDOW_START), which names the
# value the next look takes. A look that waits for the fleet ends on a line of its own that says
# which: the fleet serves no one commit, it does not carry MERGE_SHA, GitHub did not answer, or
# GitHub's compare says neither way. That line names no value: the next look takes the inputs this
# one took.
#
# Exit codes: 0 both forms at most 200 ms over at least 200 reads each; 1 a form over 200 ms; 2 the
# look cannot measure (an input unset or malformed, no answer from /healthz, a log read that
# failed, a line of the route without its fields); 3 a look that waits (a fleet that does not carry
# the merge or serves no one commit, a compare GitHub does not answer, three hours still to come,
# or a form with fewer than 200 reads). Exit 1 comes before exit 3.

set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=<deployed Dispatch> MERGE_SHA=<merge commit> \
         LOGGING_PROJECT=<project of the container logs> [FLEET_SINCE=<time>] \
         priorities-production.sh
       (FLEET_SINCE as an earlier look printed it, YYYY-MM-DDTHH:MM:SSZ; WINDOW_START=<time> in
       its place is for the check's negative control alone)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL MERGE_SHA LOGGING_PROJECT
require_commands curl date gcloud gh jq sort

# shellcheck disable=SC2031 # the DISPATCH_URL `cli` exports is its own; this one is the caller's
readonly base="${DISPATCH_URL%/}"
readonly merge="$MERGE_SHA"
[[ "$merge" =~ ^[0-9a-f]{40}$ ]] || cannot_measure "MERGE_SHA is $merge, not a commit's 40 hex digits"
[[ -z "${FLEET_SINCE:-}" || -z "${WINDOW_START:-}" ]] ||
  cannot_measure "FLEET_SINCE and WINDOW_START both name the start of the three hours: set one"

readonly route=/api/v1/priorities
readonly forms=(full summary)
declare -rA title=([full]='the full read' [summary]='the summary')
readonly summary_below=2048 bound_ms=200 reads_wanted=200 window_s=10800 max_time_s=60

work_dir
trap 'rm -rf "$work"' EXIT

# now: the host's clock, in seconds since the epoch.
now() {
  local seconds
  seconds="$(date -u +%s)"
  [[ "$seconds" =~ ^[0-9]+$ ]] || cannot_measure "date -u +%s answered $seconds, not a count of seconds"
  printf '%s' "$seconds"
}

if [[ -n "${WINDOW_START:-}" ]]; then
  # The negative control: the three hours it names, whatever commit the fleet serves.
  epoch WINDOW_START
  start_s="$epoch_s"
  since_name=WINDOW_START
  printf "WINDOW_START=%s: this look reads the three hours from that time and skips the test of the fleet's commit\n" \
    "$WINDOW_START"
  now_s="$(now)"
  if ((now_s < start_s + window_s)); then
    printf 'the three hours from %s end at %s, which is still to come: read again at or after that time with WINDOW_START=%s\n' \
      "$WINDOW_START" "$(stamp $((start_s + window_s)))" "$WINDOW_START"
    exit 3
  fi
else
  [[ -z "${FLEET_SINCE:-}" ]] || epoch FLEET_SINCE

  # First: the commit every live server process serves, and whether it carries the merge.
  code="$(curl --compressed --max-time "$max_time_s" -sS -o "$work/healthz" -w '%{http_code}' \
    "$base/healthz" 2>"$work/curl" </dev/null)" || code=000
  fleet_commit "$code"
  readonly fleet
  carries "$fleet"
  if [[ "$carried" == no ]]; then
    printf "production's fleet serves %s, which does not carry %s (GitHub's compare: %s)\n" \
      "$fleet" "$merge" "$compared"
    exit 3
  fi

  # Second: the three hours from FLEET_SINCE, or from now where no earlier look named it.
  now_s="$(now)"
  if [[ -n "${FLEET_SINCE:-}" ]]; then
    start_s="$epoch_s"
  else
    start_s="$now_s"
  fi
  since_name=FLEET_SINCE
  if ((now_s < start_s + window_s)); then
    printf "production's fleet serves %s, which carries the merge: read again at or after %s with FLEET_SINCE=%s\n" \
      "$fleet" "$(stamp $((start_s + window_s)))" "$(stamp "$start_s")"
    exit 3
  fi
  printf "production's fleet serves %s, which carries %s (GitHub's compare: %s): this look reads the three hours from %s\n" \
    "$fleet" "$merge" "$compared" "$(stamp "$start_s")"
fi
readonly start_s since_name
readonly end_s=$((start_s + window_s))
start="$(stamp "$start_s")"
end="$(stamp "$end_s")"
readonly start end

# Third: the access log lines of the route in those three hours.
access_log "textPayload:\"route=$route \"" "timestamp>=\"$start\"" "timestamp<\"$end\""

# Fourth: the lines of GET answered 200, each form told by its bytes. The fields before `path` are
# the first a line holds, and `status`, `elapsed_ms` and `bytes` the last but `credential` and
# `aborted`, so each is read where the handler writes it and never out of a quoted user agent.
readonly head_fields=' msg="dispatch: request" method=([^ ]*) route=([^ ]*) path='
readonly tail_fields='^.* status=([0-9]+) elapsed_ms=([0-9]+) bytes=([0-9]+) '
declare -A reads=() smallest=() largest=()
for form in "${forms[@]}"; do
  reads[$form]=0 smallest[$form]='' largest[$form]=''
  : >"$work/elapsed-$form"
done
lines=0
kept=0
while IFS= read -r line || [[ -n "$line" ]]; do
  line=" $line "
  [[ "$line" =~ $head_fields && "${BASH_REMATCH[2]}" == "$route" ]] || continue
  method="${BASH_REMATCH[1]}"
  lines=$((lines + 1))
  if [[ ! "$line" =~ $tail_fields ]]; then
    at=unknown
    [[ ! "$line" =~ ^\ time=([^ ]+)\  ]] || at="${BASH_REMATCH[1]}"
    cannot_measure "the line of $route at $at holds no status, elapsed_ms and bytes"
  fi
  status=$((10#${BASH_REMATCH[1]}))
  elapsed=$((10#${BASH_REMATCH[2]}))
  bytes=$((10#${BASH_REMATCH[3]}))
  if [[ "$method" != GET ]] || ((status != 200)); then continue; fi
  kept=$((kept + 1))
  if ((bytes < summary_below)); then form=summary; else form=full; fi
  printf '%d\n' "$elapsed" >>"$work/elapsed-$form"
  reads[$form]=$((reads[$form] + 1))
  [[ -n "${smallest[$form]}" ]] && ((smallest[$form] <= bytes)) || smallest[$form]=$bytes
  [[ -n "${largest[$form]}" ]] && ((largest[$form] >= bytes)) || largest[$form]=$bytes
done <"$work/log"
printf 'the log from %s to %s holds %d lines of route %s, %d of them GET answered 200\n' \
  "$start" "$end" "$lines" "$route" "$kept"

# Fifth: each form's p90 by the server's clock.
failed=0 short=0
for form in "${forms[@]}"; do
  name="${title[$form]}"
  if ((reads[$form] > 0)); then
    value="$(p90 "$work/elapsed-$form")"
    if ((value > bound_ms)); then
      verdict=over
      failed=1
    else
      verdict='at most'
    fi
    printf '%s: p90 %d ms over %d reads, %s %d ms\n' "$name" "$value" "${reads[$form]}" "$verdict" "$bound_ms"
    printf '%s: smallest %d bytes, largest %d bytes\n' "$name" "${smallest[$form]}" "${largest[$form]}"
  fi
  if ((reads[$form] < reads_wanted)); then
    printf '%s: %d reads, fewer than %d\n' "$name" "${reads[$form]}" "$reads_wanted"
    short=1
  fi
done

((failed == 0)) || exit 1
if ((short)); then
  printf 'read again at or after %s with %s=%s\n' "$(stamp $((end_s + window_s)))" "$since_name" "$end"
  exit 3
fi
