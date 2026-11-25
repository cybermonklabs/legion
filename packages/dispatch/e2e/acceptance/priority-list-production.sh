#!/usr/bin/env bash
# Measures in production the time the server takes to answer the issue list filtered by a priority
# (the check c-both-plans-production): the p90 of the access log's `elapsed_ms` over 200 reads of
# each of three requests this script sends itself, read back from the deployment's container logs.
#
# Inputs: DISPATCH_URL (the deployed Dispatch), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent
# bearer, read here and never printed), MERGE_SHA (the merge commit the fleet must carry) and
# LOGGING_PROJECT (the project that holds the deployment's container logs). An earlier look's
# values, each printed on the line that ends that look: FLEET_SINCE and FLEET_COMMIT; and PROBE_ID,
# PROBE_AT and PROBE_STOPPED, all three or none, with which the look sends nothing and reads back
# that probe's lines. BEFORE_DEPLOY=1, which only the check's negative control sets, turns each test
# of a commit around and skips the three-hour wait.
#
# A look: /healthz's `fleet_commit` must carry MERGE_SHA by GitHub's compare (`identical` or
# `ahead`), or with PROBE_ID the commit inside the probe id must; the fleet must have served that
# commit for three hours; then 200 reads of each form, one at a time, of the priority with the most
# open issues - the page of 50 (`page`), `&open=true` (`open`), the whole list (`whole`) - each with
# the User-Agent `priority-list-production/<probe id>/<form>`, until 21 reads of one form took over
# 1,000 ms by curl's `time_total`; then the probe's lines from `gcloud logging read`, read again
# every 30 s for at most 10 minutes; and each form's p90, the value at rank ceil(0.9 n).
#
# Output: `priority <id>: <n> open issues`, a line per form with curl's p90, `PROBE_ID=<id>
# PROBE_AT=<time> PROBE_STOPPED=<none or form:reads>`, and a line per form from the log: `<form>:
# p90 N ms over M reads, at most 200 ms` (only with all 200 lines) or `..., over 200 ms`, `<form>:
# not read`, or `<form>: stopped after M reads, K of them over 200 ms on the server`. A look that
# waits ends on `read again at or after <time> with ...`, naming what the next look takes.
#
# Exit codes: 0 every form at most 200 ms; 1 a form over 200 ms, a line whose status is not 200,
# or a first answer that holds no issue; 2 the look cannot measure (an input unset or malformed, a
# first answer that is not 200, no line of the probe after 10 minutes, a probe id of another build);
# 3 a look that waits. Exit 1 comes before exit 3.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=<deployed Dispatch> DISPATCH_TOKEN=<agent bearer> MERGE_SHA=<merge commit> \
         LOGGING_PROJECT=<project of the container logs> [FLEET_SINCE=<time> FLEET_COMMIT=<commit>] \
         [PROBE_ID=<id> PROBE_AT=<time> PROBE_STOPPED=<value>] [BEFORE_DEPLOY=1] \
         priority-list-production.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN; each time as
       this script prints one, YYYY-MM-DDTHH:MM:SSZ)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL MERGE_SHA LOGGING_PROJECT
read_bearer
require_commands curl gcloud gh jq od sleep sort

# cannot_measure WHY: the look cannot measure (exit 2).
cannot_measure() {
  printf '%s\n' "$1" >&2
  exit 2
}

# shellcheck disable=SC2031 # the DISPATCH_URL `cli` exports is its own; this one is the caller's
readonly base="${DISPATCH_URL%/}"
readonly merge="$MERGE_SHA"
[[ "$merge" =~ ^[0-9a-f]{40}$ ]] || cannot_measure "MERGE_SHA is $merge, not a commit's 40 hex digits"
case "${BEFORE_DEPLOY:-}" in
  '') before_deploy=0 ;;
  1) before_deploy=1 ;;
  *) cannot_measure "BEFORE_DEPLOY is $BEFORE_DEPLOY: only 1 sets it" ;;
esac
readonly before_deploy

readonly forms=(page open whole)
declare -rA title=([page]='the page of 50' [open]='the open list' [whole]='the whole list')
readonly reads_per_form=200 stop_after=21 slow_read_us=1000000 bound_ms=200
readonly fleet_wait_s=10800 log_window_s=7200 log_pause_s=30 log_pauses=20
readonly agent_prefix=priority-list-production
readonly time_pattern='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'

work_dir
trap 'rm -rf "$work"' EXIT
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

now() { date -u +%s; }
# stamp SECONDS: the time SECONDS after the epoch, as this script prints a time.
stamp() { jq -rn --argjson s "$1" '$s | todateiso8601'; }
# epoch NAME: sets `epoch_s` to the time the variable NAME holds, in seconds since the epoch. A
# time this script does not print cannot be measured against.
epoch() {
  [[ "${!1}" =~ $time_pattern ]] ||
    cannot_measure "$1 is ${!1}, not a time as this script prints one (YYYY-MM-DDTHH:MM:SSZ)"
  epoch_s="$(jq -rn --arg t "${!1}" '$t | fromdateiso8601 | floor')"
}

# fetch OUT URL [CURL-ARGS...]: one GET of URL, its body in OUT. Sets `code`, 000 where no answer
# came, and `elapsed_us`, curl's time_total in microseconds, the whole --max-time where curl wrote
# none.
fetch() {
  local out="$1" url="$2" total=''
  shift 2
  curl --compressed --max-time 60 -sS -o "$out" -w '%{http_code} %{time_total}\n' "$@" "$url" \
    >"$work/written" 2>"$work/curl" </dev/null || true
  read -r code total <"$work/written" || true
  [[ "$code" =~ ^[0-9]{3}$ ]] || code=000
  if [[ "$total" =~ ^([0-9]+)\.([0-9]+)$ ]]; then
    local fraction="${BASH_REMATCH[2]}000000"
    elapsed_us=$((10#${BASH_REMATCH[1]} * 1000000 + 10#${fraction:0:6}))
  elif [[ "$total" =~ ^[0-9]+$ ]]; then
    elapsed_us=$((10#$total * 1000000))
  else
    elapsed_us=60000000
  fi
}

# p90 FILE: the p90 of the numbers FILE holds, one a line: the value at rank ceil(0.9 n) of them in
# ascending order.
p90() {
  local -a sorted
  mapfile -t sorted < <(sort -n "$1")
  local rank=$(((9 * ${#sorted[@]} + 9) / 10))
  printf '%s' "${sorted[rank - 1]}"
}

# carries COMMIT: GitHub's compare of MERGE_SHA with COMMIT. Sets `compared`, its status, and
# `carried`, yes where COMMIT carries MERGE_SHA (`identical` or `ahead`) and no where it does not
# (`behind` or `diverged`). A compare GitHub does not answer ends the look, which waits.
carries() {
  if ! compared="$(gh api "repos/cybermonklabs/legion/compare/$merge...$1" --jq .status \
    2>"$work/gh" </dev/null)"; then
    printf 'GitHub did not answer whether %s carries %s: %s\n' "$1" "$merge" \
      "$(head -c 300 "$work/gh" | tr '\n' ' ')"
    exit 3
  fi
  case "$compared" in
    identical | ahead) carried=yes ;;
    behind | diverged) carried=no ;;
    *)
      printf "GitHub's compare of %s with %s answered %s, which says neither way whether it carries it\n" \
        "$merge" "$1" "${compared:-nothing}"
      exit 3
      ;;
  esac
}

# A look holds that the commit it measures carries MERGE_SHA, and BEFORE_DEPLOY=1 that it does not.
if ((before_deploy)); then wanted=no; else wanted=yes; fi
readonly wanted

if [[ -n "${PROBE_ID:-}${PROBE_AT:-}${PROBE_STOPPED:-}" ]]; then
  # A look that reads back an earlier probe: it sends nothing.
  [[ -n "${PROBE_ID:-}" && -n "${PROBE_AT:-}" && -n "${PROBE_STOPPED:-}" ]] ||
    cannot_measure "PROBE_ID, PROBE_AT and PROBE_STOPPED come together, as the line that ends a look prints them"
  [[ "$PROBE_ID" =~ ^([0-9a-f]{40})/[0-9a-f]{16}$ ]] ||
    cannot_measure "PROBE_ID is $PROBE_ID, not a commit's 40 hex digits, a slash and 16 hex digits"
  probe_commit="${BASH_REMATCH[1]}"
  probe_id="$PROBE_ID"
  probe_at="$PROBE_AT"
  epoch PROBE_AT
  probe_at_s="$epoch_s"
  stopped="$PROBE_STOPPED"
  if [[ "$stopped" =~ ^(page|open|whole):([0-9]+)$ ]]; then
    ((10#${BASH_REMATCH[2]} >= stop_after && 10#${BASH_REMATCH[2]} <= reads_per_form)) ||
      cannot_measure "PROBE_STOPPED is $stopped: a probe stops after $stop_after to $reads_per_form reads of a form"
  elif [[ "$stopped" != none ]]; then
    cannot_measure "PROBE_STOPPED is $stopped, not none or a form and the reads sent of it, as page:21"
  fi
  carries "$probe_commit"
  if [[ "$carried" != "$wanted" ]]; then
    if [[ "$carried" == yes ]]; then relation='carries'; else relation='does not carry'; fi
    cannot_measure "probe $probe_id read $probe_commit, which $relation $merge (GitHub's compare: $compared)${BEFORE_DEPLOY:+ under BEFORE_DEPLOY=1}: it read another build"
  fi
  printf 'PROBE_ID given: this look sends nothing and reads back the lines of probe %s\n' "$probe_id"
else
  # First: the commit every live server process serves, and whether it carries the merge.
  fetch "$work/healthz" "$base/healthz"
  [[ "$code" != 000 ]] ||
    cannot_measure "GET $base/healthz got no answer: $(head -c 300 "$work/curl" | tr '\n' ' ')"
  fleet="$(jq -r '.fleet_commit // empty' "$work/healthz" 2>/dev/null)" || fleet=''
  if [[ ! "$fleet" =~ ^[0-9a-f]{40}$ ]]; then
    printf "production's fleet serves no one commit: GET /healthz answered HTTP %s with fleet_commit %s\n" \
      "$code" "${fleet:-null}"
    exit 3
  fi
  readonly fleet
  carries "$fleet"
  if [[ "$carried" != "$wanted" ]]; then
    if [[ "$carried" == yes ]]; then
      printf "production's fleet serves %s, which carries %s (GitHub's compare: %s), and BEFORE_DEPLOY=1 measures a fleet that does not\n" \
        "$fleet" "$merge" "$compared"
    else
      printf "production's fleet serves %s, which does not carry %s (GitHub's compare: %s)\n" \
        "$fleet" "$merge" "$compared"
    fi
    exit 3
  fi

  # Second: the fleet has served that commit for three hours, which BEFORE_DEPLOY=1 skips.
  if ((before_deploy)); then
    printf "production's fleet serves %s, which does not carry %s (GitHub's compare: %s): BEFORE_DEPLOY=1 measures it\n" \
      "$fleet" "$merge" "$compared"
  else
    now_s="$(now)"
    if [[ -z "${FLEET_SINCE:-}" || "${FLEET_COMMIT:-}" != "$fleet" ]]; then
      printf "production's fleet serves %s, which carries the merge: read again at or after %s with FLEET_SINCE=%s FLEET_COMMIT=%s\n" \
        "$fleet" "$(stamp $((now_s + fleet_wait_s)))" "$(stamp "$now_s")" "$fleet"
      exit 3
    fi
    epoch FLEET_SINCE
    since_s="$epoch_s"
    if ((now_s < since_s + fleet_wait_s)); then
      printf "production's fleet serves %s, which carries the merge: read again at or after %s with FLEET_SINCE=%s FLEET_COMMIT=%s\n" \
        "$fleet" "$(stamp $((since_s + fleet_wait_s)))" "$FLEET_SINCE" "$fleet"
      exit 3
    fi
  fi

  # Third: the priority with the most open issues, and the probe's reads of it.
  fetch "$work/priorities" "$base/api/v1/priorities" -H "@$work/auth"
  [[ "$code" == 200 ]] ||
    cannot_measure "GET /api/v1/priorities answered HTTP $code: $(head -c 300 "$work/priorities" 2>/dev/null)"
  largest="$(jq -r '[.priorities[]? | {id, open: ([.work.by_status[]?] | add // 0)}]
    | sort_by(-.open) | .[0] // empty | "\(.id) \(.open)"' "$work/priorities" 2>/dev/null)" || largest=''
  read -r priority open_issues <<<"$largest" || true
  [[ -n "${priority:-}" && "${open_issues:-0}" =~ ^[1-9][0-9]*$ ]] ||
    cannot_measure "no live priority has an open issue: $(head -c 300 "$work/priorities")"
  readonly priority open_issues
  printf 'priority %s: %s open issues\n' "$priority" "$open_issues"

  probe_id="$fleet/$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  [[ "$probe_id" =~ ^[0-9a-f]{40}/[0-9a-f]{16}$ ]] || cannot_measure "no probe id could be made: $probe_id"
  quoted="$(q "$priority")"
  declare -rA path=(
    [page]="/api/v1/issues?advances=$quoted&limit=50"
    [open]="/api/v1/issues?advances=$quoted&open=true"
    [whole]="/api/v1/issues?advances=$quoted"
  )
  probe_at_s=$(($(now) - 60))
  probe_at="$(stamp "$probe_at_s")"
  stopped=none
  declare -A client_reads=()
  for form in "${forms[@]}"; do
    [[ "$stopped" == none ]] || break
    slow=0
    : >"$work/client-$form"
    for ((i = 1; i <= reads_per_form; i++)); do
      fetch "$work/body" "$base${path[$form]}" -H "@$work/auth" -A "$agent_prefix/$probe_id/$form"
      client_reads[$form]=$i
      if ((i == 1)); then
        [[ "$code" == 200 ]] ||
          cannot_measure "${title[$form]}: the first answer of GET ${path[$form]} is HTTP $code, not 200: $(head -c 300 "$work/body" 2>/dev/null)"
        issues="$(jq 'if type == "array" then length else (.issues | length) end' "$work/body" 2>/dev/null)" ||
          issues=0
        if [[ ! "$issues" =~ ^[0-9]+$ ]] || ((issues == 0)); then
          printf '%s: the first answer of GET %s holds no issue, so its time shows nothing\n' \
            "${title[$form]}" "${path[$form]}"
          exit 1
        fi
      fi
      printf '%d\n' $((elapsed_us / 1000)) >>"$work/client-$form"
      ((elapsed_us <= slow_read_us)) || slow=$((slow + 1))
      if ((slow >= stop_after)); then
        stopped="$form:$i"
        break
      fi
    done
  done
  for form in "${forms[@]}"; do
    if [[ -n "${client_reads[$form]:-}" ]]; then
      printf "%s by curl's clock: p90 %s ms over %d reads\n" \
        "${title[$form]}" "$(p90 "$work/client-$form")" "${client_reads[$form]}"
    else
      printf "%s by curl's clock: not read\n" "${title[$form]}"
    fi
  done
  printf 'PROBE_ID=%s PROBE_AT=%s PROBE_STOPPED=%s\n' "$probe_id" "$probe_at" "$stopped"
fi
readonly probe_id probe_at probe_at_s stopped

# What the probe sent of each form: 200 of each before the one that stopped, the reads sent of that
# one, and none after it.
declare -A sent=()
stopped_form=''
[[ "$stopped" == none ]] || stopped_form="${stopped%%:*}"
readonly stopped_form
after_stop=0
for form in "${forms[@]}"; do
  if ((after_stop)); then
    sent[$form]=0
  elif [[ "$form" == "$stopped_form" ]]; then
    sent[$form]="$((10#${stopped#*:}))"
    after_stop=1
  else
    sent[$form]=$reads_per_form
  fi
done

# Fourth: the probe's lines in the container logs, from PROBE_AT to two hours after it.
printf -v filter '%s AND %s AND %s AND %s AND %s AND %s' \
  'resource.type="k8s_container"' \
  'resource.labels.namespace_name="dispatch"' \
  'textPayload:"msg=\"dispatch: request\""' \
  "textPayload:\"user_agent=$agent_prefix/$probe_id/\"" \
  "timestamp>=\"$probe_at\"" \
  "timestamp<=\"$(stamp $((probe_at_s + log_window_s)))\""
readonly filter
readonly agent_field=" user_agent=$agent_prefix/$probe_id/(page|open|whole) "
readonly status_field=' status=([0-9]+) ' elapsed_field=' elapsed_ms=([0-9]+) '

# read_log: the probe's lines the log holds now. For each form it sets `held` (its lines), `over`
# (those over 200 ms), `failed_status` (those whose status is not 200) and `statuses` (those
# statuses), and writes each line's elapsed_ms to $work/elapsed-<form>.
declare -A held=() over=() failed_status=() statuses=()
read_log() {
  local line form status elapsed
  local -a lines
  local -A values=()
  gcloud logging read "$filter" --project "$LOGGING_PROJECT" --format='value(textPayload)' \
    >"$work/log" 2>"$work/gcloud" </dev/null ||
    cannot_measure "gcloud logging read failed: $(head -c 600 "$work/gcloud" | tr '\n' ' ')"
  for form in "${forms[@]}"; do
    held[$form]=0 over[$form]=0 failed_status[$form]=0 statuses[$form]='' values[$form]=''
  done
  mapfile -t lines <"$work/log"
  for line in "${lines[@]}"; do
    line=" $line "
    [[ "$line" == *' msg="dispatch: request" '* && "$line" == *' method=GET '* &&
      "$line" == *' route=/api/v1/issues '* && "$line" =~ $agent_field ]] || continue
    form="${BASH_REMATCH[1]}"
    [[ "$line" =~ $status_field ]] || cannot_measure "a line of probe $probe_id holds no status:$line"
    status=$((10#${BASH_REMATCH[1]}))
    [[ "$line" =~ $elapsed_field ]] || cannot_measure "a line of probe $probe_id holds no elapsed_ms:$line"
    elapsed=$((10#${BASH_REMATCH[1]}))
    values[$form]+="$elapsed"$'\n'
    held[$form]=$((held[$form] + 1))
    ((elapsed <= bound_ms)) || over[$form]=$((over[$form] + 1))
    if ((status != 200)); then
      failed_status[$form]=$((failed_status[$form] + 1))
      statuses[$form]="${statuses[$form]:+${statuses[$form]} }$status"
    fi
  done
  for form in "${forms[@]}"; do
    printf '%s' "${values[$form]}" >"$work/elapsed-$form"
  done
}

# complete: whether the log holds as many lines of each form as the probe sent.
complete() {
  local form
  for form in "${forms[@]}"; do
    ((held[$form] >= sent[$form])) || return 1
  done
}

read_log
pauses=0
until complete || ((pauses >= log_pauses)); do
  sleep "$log_pause_s"
  pauses=$((pauses + 1))
  read_log
done
((held[page] + held[open] + held[whole] > 0)) ||
  cannot_measure "no line of probe $probe_id reached the log in $((log_pause_s * log_pauses / 60)) minutes: its User-Agent header did not reach it"
printf 'the log holds lines of probe %s: %d of the page of 50, %d of the open list, %d of the whole list\n' \
  "$probe_id" "${held[page]}" "${held[open]}" "${held[whole]}"

# Fifth: each form's p90 by the server's clock.
failed=0 missing=0 restart=0
for form in "${forms[@]}"; do
  name="${title[$form]}"
  if ((sent[$form] == 0)); then
    printf '%s: not read\n' "$name"
    continue
  fi
  if ((held[$form] == 0)); then
    printf '%s: none of the %d reads sent is in the log\n' "$name" "${sent[$form]}"
    missing=1
    continue
  fi
  value="$(p90 "$work/elapsed-$form")"
  if [[ "$form" == "$stopped_form" ]]; then
    # 21 lines over 200 ms put the p90 of any 200 reads over 200 ms; fewer say the network was
    # slow and the server was not, and a new probe is owed.
    if ((over[$form] >= stop_after)); then
      printf '%s: p90 %d ms over %d reads, over 200 ms\n' "$name" "$value" "${held[$form]}"
      failed=1
    elif ((held[$form] < sent[$form])); then
      printf '%s: %d of the %d reads sent are in the log\n' "$name" "${held[$form]}" "${sent[$form]}"
      missing=1
    else
      printf '%s: stopped after %d reads, %d of them over 200 ms on the server\n' \
        "$name" "${sent[$form]}" "${over[$form]}"
      restart=1
    fi
  elif ((held[$form] >= sent[$form])); then
    if ((value > bound_ms)); then
      printf '%s: p90 %d ms over %d reads, over 200 ms\n' "$name" "$value" "${held[$form]}"
      failed=1
    else
      printf '%s: p90 %d ms over %d reads, at most 200 ms\n' "$name" "$value" "${held[$form]}"
    fi
  elif ((over[$form] >= stop_after)); then
    # The lines still missing cannot bring the p90 of 200 reads back to 200 ms.
    printf '%s: p90 %d ms over %d reads, over 200 ms\n' "$name" "$value" "${held[$form]}"
    failed=1
  else
    printf '%s: %d of the %d reads sent are in the log\n' "$name" "${held[$form]}" "${sent[$form]}"
    missing=1
  fi
  if ((failed_status[$form] > 0)); then
    printf '%s: %d of its lines answered a status other than 200 (%s)\n' \
      "$name" "${failed_status[$form]}" "${statuses[$form]}"
    failed=1
  fi
done

((failed == 0)) || exit 1
if ((missing)); then
  now_s="$(now)"
  if ((now_s < probe_at_s + log_window_s)); then
    printf 'read again at or after %s with PROBE_ID=%s PROBE_AT=%s PROBE_STOPPED=%s\n' \
      "$(stamp $((now_s + 600)))" "$probe_id" "$probe_at" "$stopped"
  else
    printf 'the lines of probe %s the log lacks are lost: two hours have passed since PROBE_AT\n' "$probe_id"
    printf 'read again at or after %s with no PROBE_ID\n' "$(stamp $((now_s + 3600)))"
  fi
  exit 3
fi
if ((restart)); then
  printf 'read again at or after %s with no PROBE_ID\n' "$(stamp $(($(now) + 3600)))"
  exit 3
fi
