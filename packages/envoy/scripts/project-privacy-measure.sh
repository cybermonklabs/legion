#!/usr/bin/env bash
# project-privacy-measure.sh — Measures the listener's privacy read on the path a listener's own
# read crosses, and derives the listener's read timeout from it. See
# packages/envoy/scripts/README.md for usage.
#
# A listener in the required privacy mode reads the secrets broker's GET /v1/project-privacy once
# per decision batch (cmd/listener's k7Reader): the broker reads Dispatch, so each read is one
# listener -> broker -> Dispatch round trip, bounded by k7ReadTimeout. This script makes that read,
# with the listener's own platform identity, at the rate the production listener makes decisions,
# and records every read's wall time.
#
#   run      For at least an hour: each tick (a sixtieth of a minute) it reads the listener's
#            counters at each --metrics-url, and spreads one decision for each increment over the
#            next tick. A read serves every decision due when it starts, so at most one read is in
#            flight and a decision that arrives during a read waits for the next one, as the
#            listener's reader does. It prints one line per tick that counted decisions, one per
#            read and one per minute, and ends with the verdict below. The first read that fails,
#            or the first counter read that fails, ends the run.
#   verdict  Judges a log a run printed (kubectl logs of the run's Job, say), with no network.
#   job      Prints the manifest of one production run: the ServiceAccount project-privacy-measure
#            and a Job that runs this script's `run` under it, in namespace envoy. The Job reads
#            the script from a ConfigMap named as the Job, which the caller creates from the
#            committed script. The broker answers that account's reads as its own kind, so the run
#            reads as no listener does, and its pods carry app.kubernetes.io/name:
#            project-privacy-measure, the label the broker's ingress admits and that no
#            listener's Service selects.
#
# Verdict: the count of reads, their p50 and p99 (nearest rank) and the longest. The bound is
# twice the longest read, rounded up to the next 100 ms; where twice the longest read exceeds the
# 2 s cap the listener ships with, no bound is set. The verdict is UNKNOWN when any read or counter
# read failed, when no read happened, when the run covered less than an hour, or when the log is
# not a whole run: a bound needs an hour of answered reads.
#
# Needs bash 5 (EPOCHREALTIME), curl, sort and sleep, so it runs in the listener's own image.
#
# Exit codes:
#   0 — a bound: the verdict prints the constant.
#   1 — twice the longest read exceeds the 2 s cap: no constant is set.
#   2 — usage error.
#   3 — UNKNOWN: the verdict names why.
#   4 — a required tool is missing.
set -euo pipefail
export TZ=UTC LC_ALL=C

usage() {
  cat <<'EOF'
Usage:
  project-privacy-measure.sh run --source <broker base URL> --token-file <file>
      --metrics-url <listener /metrics URL> [--metrics-url <URL>]...
      [--counter <name>]... [--minutes <n>] [--max-time <seconds>]
  project-privacy-measure.sh verdict <log>
  project-privacy-measure.sh job --run <id> --image <listener image> --source <broker base URL>
      --audience <broker token audience> --metrics-url <URL> [--metrics-url <URL>]... [--minutes <n>]

run:
  --source       the broker's base URL; each read is GET <source>/v1/project-privacy
  --token-file   the listener's platform identity, a projected service-account token of the
                 broker's audience, read again before every read
  --metrics-url  [NAME=]URL of a listener's /metrics, NAME its pod's name (a lower-case DNS
                 label); give one per listener pod. The output names each, and counts each
                 one's decisions
  --counter      a counter family whose increments are decisions; repeatable. Default:
                 envoy_messages_received_total and envoy_listener_requests_total
  --minutes      how long to run; at least 60 (default 60)
  --max-time     the longest one read may take before it counts as failed (default 30)

verdict:
  <log>          the output of a run, whole

job:
  --run          the run's id, lower-case letters and digits; names the Job and its ConfigMap
  --image        the image the Job runs, the listener's own, which carries bash and curl
  --audience     the audience of the projected token the broker verifies
  --source, --metrics-url, --minutes   as for run
EOF
}

die_usage() {
  printf 'ERR: %s\n\n' "$1" >&2
  usage >&2
  exit 2
}

# The cap k7ReadTimeout ships at (cmd/listener), in microseconds: a bound above it is not set.
readonly cap_us=2000000
readonly hour_minutes=60
# The bound's step: twice the longest read is rounded up to a whole number of these.
readonly step_us=100000
# A minute's length in milliseconds. PROJECT_PRIVACY_MEASURE_MINUTE_MS shortens it for the
# script's own test alone; a run records its minute, and the verdict refuses a log whose minute
# is not its own, so a production log is never judged on a shortened minute.
minute_ms="${PROJECT_PRIVACY_MEASURE_MINUTE_MS:-60000}"
if ! [[ "$minute_ms" =~ ^[1-9][0-9]*$ ]] || ((minute_ms < 60)); then
  die_usage "PROJECT_PRIVACY_MEASURE_MINUTE_MS must be a whole number of milliseconds >= 60, got $minute_ms"
fi
readonly minute_ms

# now_us prints the wall clock in microseconds.
now_us() {
  local now="$EPOCHREALTIME"
  printf '%s' "${now/[.,]/}"
}

# ms US prints US microseconds as milliseconds with three decimals.
ms() {
  printf '%d.%03d' "$(($1 / 1000))" "$(($1 % 1000))"
}

# duration MS prints MS milliseconds as minutes and seconds.
duration() {
  printf '%dm%ds' "$(($1 / 60000))" "$((($1 % 60000) / 1000))"
}

# one_line TEXT prints TEXT on one line with no tabs, at most 200 characters.
one_line() {
  local text="${1//[$'\t\r\n']/ }"
  printf '%s' "${text:0:200}"
}

# verdict LOG judges a run's log and exits with the verdict's code.
verdict() {
  local log="$1"
  [[ -r "$log" ]] || die_usage "cannot read the log $log"
  local kind elapsed a b c d e pair
  local starts=0 log_minute="" ended="" end_why="" reads=0 served=0 batches=0
  local -a times=() unknown=() listeners=()
  local -A by_listener=()
  while IFS=$'\t' read -r kind elapsed a b c d e; do
    case "$kind" in
    start)
      starts=$((starts + 1))
      log_minute="$elapsed"
      ;;
    listener)
      listeners+=("$a")
      by_listener[$a]=0
      ;;
    batches)
      batches=$((batches + a))
      # b is each listener's share, name=count separated by commas.
      local -a shares=()
      IFS=, read -r -a shares <<<"$b"
      for pair in "${shares[@]}"; do
        [[ "$pair" =~ ^([^=]+)=([0-9]+)$ ]] || continue
        by_listener[${BASH_REMATCH[1]}]=$((${by_listener[${BASH_REMATCH[1]}]:-0} + BASH_REMATCH[2]))
      done
      ;;
    read)
      if [[ "$a" == ok ]]; then
        reads=$((reads + 1))
        served=$((served + d))
        times+=("$b")
      else
        unknown+=("a read failed at $(duration "$elapsed") after $(ms "$b") ms: HTTP $c, $e")
      fi
      ;;
    scrape) unknown+=("a counter read failed at $(duration "$elapsed"): $b: $c") ;;
    end)
      ended="$elapsed"
      end_why="$a"
      ;;
    esac
  done <"$log"

  if ((starts != 1)); then
    unknown+=("the log holds $starts start lines: it is not one run of this script")
  elif [[ "$log_minute" != "$minute_ms" ]]; then
    unknown+=("the run's minute is ${log_minute} ms and this verdict's is ${minute_ms} ms")
  fi
  if [[ -z "$ended" ]]; then
    unknown+=("the log has no end line: the run did not finish")
  elif [[ "$end_why" == stopped ]]; then
    unknown+=("the run was stopped at $(duration "$ended")")
  elif ((ended < hour_minutes * minute_ms)); then
    unknown+=("the run covered $(duration "$ended"), less than an hour")
  fi
  ((reads > 0)) || unknown+=("no read happened: the counters counted no decision, so nothing was measured")

  printf 'reads: %d answered, serving %d of %d decisions, over %s (minute = %d ms)\n' \
    "$reads" "$served" "$batches" "$(duration "${ended:-0}")" "$minute_ms"
  if ((${#listeners[@]} > 0)); then
    local name line=""
    for name in "${listeners[@]}"; do
      line+="${line:+, }${name} ${by_listener[$name]}"
    done
    printf 'decisions by listener: %s\n' "$line"
  fi
  if ((reads > 0)); then
    local -a sorted
    mapfile -t sorted < <(printf '%s\n' "${times[@]}" | sort -n)
    local p50="${sorted[$(((reads * 50 + 99) / 100 - 1))]}"
    local p99="${sorted[$(((reads * 99 + 99) / 100 - 1))]}"
    local longest="${sorted[$((reads - 1))]}"
    printf 'read time: p50 %s ms, p99 %s ms, longest %s ms\n' "$(ms "$p50")" "$(ms "$p99")" "$(ms "$longest")"
  fi
  if ((${#unknown[@]} > 0)); then
    local reason
    for reason in "${unknown[@]}"; do
      printf 'UNKNOWN: %s\n' "$reason"
    done
    printf 'verdict: UNKNOWN - a bound needs an hour of answered reads; no constant is set\n'
    exit 3
  fi
  local twice=$((2 * longest))
  if ((twice > cap_us)); then
    printf 'verdict: over the cap - twice the longest read is %s ms, above the %s ms k7ReadTimeout ships at; no constant is set\n' \
      "$(ms "$twice")" "$(ms "$cap_us")"
    exit 1
  fi
  local bound_ms=$((((twice + step_us - 1) / step_us) * step_us / 1000)) constant
  if ((bound_ms % 1000 == 0)); then
    constant="$((bound_ms / 1000)) * time.Second"
  else
    constant="${bound_ms} * time.Millisecond"
  fi
  printf 'verdict: k7ReadTimeout = %s (twice the longest read, %s ms, rounded up to the next 100 ms)\n' \
    "$constant" "$(ms "$twice")"
  exit 0
}

# --- run ---

source_url=""
token_file=""
metrics_urls=()
counters=()
minutes="$hour_minutes"
max_time=30

# emit FIELD... prints one tab-separated log line, to standard output, where the run's Job keeps it
# for a later verdict, and to the run's own log, which its verdict reads.
emit() {
  local IFS=$'\t'
  printf '%s\n' "$*"
  printf '%s\n' "$*" >>"$log_file"
}

# scrape URL sets scraped to the sum of every sample of each counter family at URL, or returns 1
# with scrape_error naming why: the request failed, or a family is not exposed there.
scrape() {
  local url="$1" line name total=0 found pattern
  scraped=""
  if ! curl -sS -f --max-time 5 -o "$scrape_file" "$url" 2>"$err_file"; then
    scrape_error="$(one_line "$(<"$err_file")")"
    scrape_error="${scrape_error:-curl gave no reason}"
    return 1
  fi
  for name in "${counters[@]}"; do
    found=0
    pattern="^${name}(\\{.*\\})?[[:space:]]+([0-9]+)$"
    while IFS= read -r line; do
      if [[ "$line" == "# TYPE ${name} counter" ]]; then
        found=1
      elif [[ "$line" =~ $pattern ]]; then
        total=$((total + 10#${BASH_REMATCH[2]}))
      fi
    done <"$scrape_file"
    if ((!found)); then
      scrape_error="the counter ${name} is not exposed there"
      return 1
    fi
  done
  scraped="$total"
}

# read_once makes one read and sets read_code, read_us and read_error (empty for a whole answer);
# it returns 1 for a read that failed.
read_once() {
  local token rc=0 written body
  read_code="000"
  read_us=0
  read_error=""
  token="$(<"$token_file")"
  token="${token//[$'\r\n\t ']/}"
  if [[ -z "$token" ]]; then
    read_error="the token file ${token_file} is empty"
    return 1
  fi
  printf 'Authorization: Bearer %s\n' "$token" >"$header_file"
  rm -f "$body_file"
  written="$(curl -sS -o "$body_file" -w '%{http_code} %{time_total}' --max-time "$max_time" \
    --max-filesize 1048576 -H @"$header_file" "$read_url" 2>"$err_file")" || rc=$?
  # curl writes "<status> <seconds>" whatever the outcome, 000 for no answer.
  local seconds="${written##* }" whole fraction
  read_code="${written%% *}"
  [[ "$read_code" =~ ^[0-9]{3}$ ]] || read_code="000"
  whole="${seconds%%[.,]*}"
  fraction="${seconds#*[.,]}000000"
  [[ "$seconds" == *[.,]* ]] || fraction="000000"
  [[ "$whole" =~ ^[0-9]+$ && "${fraction:0:6}" =~ ^[0-9]{6}$ ]] || {
    whole=0
    fraction="000000"
  }
  read_us=$((10#${whole} * 1000000 + 10#${fraction:0:6}))
  if ((rc != 0)); then
    case "$rc" in
    28) read_error="no answer within ${max_time} s" ;;
    7) read_error="no connection" ;;
    *) read_error="curl exit ${rc}: $(one_line "$(<"$err_file")")" ;;
    esac
    return 1
  fi
  body=""
  [[ ! -f "$body_file" ]] || body="$(<"$body_file")"
  if [[ "$read_code" != 200 ]]; then
    read_error="$(one_line "$body")"
    read_error="${read_error:-no body}"
    return 1
  fi
  if ! [[ "${body//[[:space:]]/}" =~ ^\{\"projects\":\[.*\]\}$ ]]; then
    read_error="not a whole answer: $(one_line "$body")"
    return 1
  fi
}

run() {
  while (($#)); do
    case "$1" in
    --source) source_url="${2:-}" ;;
    --token-file) token_file="${2:-}" ;;
    --metrics-url) metrics_urls+=("${2:-}") ;;
    --counter) counters+=("${2:-}") ;;
    --minutes) minutes="${2:-}" ;;
    --max-time) max_time="${2:-}" ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) die_usage "unknown argument $1" ;;
    esac
    (($# >= 2)) || die_usage "$1 needs a value"
    shift 2
  done
  [[ "$source_url" =~ ^https?://[^[:space:]]+$ ]] || die_usage "--source must be an http(s) URL, got ${source_url:-nothing}"
  [[ -n "$token_file" ]] || die_usage "--token-file is required"
  [[ -r "$token_file" ]] || die_usage "cannot read the token file $token_file"
  ((${#metrics_urls[@]} > 0)) || die_usage "--metrics-url is required"
  # Each --metrics-url is NAME=URL, the name a listener's pod (any lower-case DNS label), or a bare
  # URL, which names itself. The run names each in its output, and counts each one's decisions.
  local url name entry i
  local -a names=() urls=()
  for entry in "${metrics_urls[@]}"; do
    name=""
    url="$entry"
    if [[ "$entry" =~ ^([a-z0-9][a-z0-9.-]{0,62})=(.*)$ ]]; then
      name="${BASH_REMATCH[1]}"
      url="${BASH_REMATCH[2]}"
    fi
    [[ "$url" =~ ^https?://[^[:space:],=]+$ ]] || die_usage "--metrics-url must be [NAME=]an http(s) URL with no comma or =, got ${entry:-nothing}"
    name="${name:-$url}"
    for i in "${!names[@]}"; do
      [[ "${names[i]}" != "$name" ]] || die_usage "--metrics-url names ${name} twice"
    done
    names+=("$name")
    urls+=("$url")
  done
  ((${#counters[@]} > 0)) || counters=(envoy_messages_received_total envoy_listener_requests_total)
  for name in "${counters[@]}"; do
    [[ "$name" =~ ^[a-zA-Z_:][a-zA-Z0-9_:]*$ ]] || die_usage "--counter is not a metric name: ${name:-nothing}"
  done
  [[ "$minutes" =~ ^[0-9]+$ ]] || die_usage "--minutes must be a whole number, got $minutes"
  ((minutes >= hour_minutes)) || die_usage "--minutes is $minutes: a run shorter than an hour sets no bound, so it is refused"
  [[ "$max_time" =~ ^[1-9][0-9]*$ ]] || die_usage "--max-time must be a whole number of seconds >= 1, got $max_time"
  local cmd
  for cmd in curl sort sleep; do
    command -v "$cmd" >/dev/null || {
      printf 'ERR: %s is required\n' "$cmd" >&2
      exit 4
    }
  done

  work_dir="$(mktemp -d)"
  trap 'rm -rf "$work_dir"' EXIT
  # The bearer goes to curl in a header file, never on its command line, where ps shows it.
  umask 077
  log_file="${work_dir}/log"
  header_file="${work_dir}/header"
  body_file="${work_dir}/body"
  scrape_file="${work_dir}/scrape"
  err_file="${work_dir}/err"
  read_url="${source_url%/}/v1/project-privacy"

  local stop=0
  trap 'stop=1' INT TERM
  local start
  start="$(now_us)"
  local -r tick_us=$((minute_ms * 1000 / 60)) run_us=$((minutes * minute_ms * 1000))
  emit start "$minute_ms" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$read_url"
  printf '# counters: %s\n' "${counters[*]}"
  for i in "${!names[@]}"; do
    emit listener 0 "${names[i]}" "${urls[i]}"
  done

  # The decisions one tick counted are spread evenly over the next: the k-th (from 0) is due at
  # sched_start + k * sched_step. overdue counts decisions due and not yet served from a schedule a
  # newer tick replaced.
  local -A previous=()
  local sched_start=0 sched_step=0 sched_count=0 sched_next=0 overdue=0
  local next_tick="$start" counted=0 now due due_k wake delta total elapsed shares
  local minute=0 minute_batches=0 minute_reads=0 minute_longest=0 why="done" code=0
  while true; do
    now="$(now_us)"
    elapsed=$(((now - start) / 1000))
    if ((stop)); then
      why=stopped
      break
    fi
    if ((now - start >= run_us)); then
      break
    fi
    if (((now - start) / 1000 / minute_ms > minute)); then
      emit minute "$elapsed" "$minute" "$minute_batches" "$minute_reads" "$minute_longest"
      minute=$(((now - start) / 1000 / minute_ms))
      minute_batches=0
      minute_reads=0
      minute_longest=0
    fi
    if ((now >= next_tick)); then
      total=0
      shares=""
      for i in "${!urls[@]}"; do
        if ! scrape "${urls[i]}"; then
          emit scrape "$elapsed" failed "${names[i]} (${urls[i]})" "$scrape_error"
          why=failed
          break 2
        fi
        if ((counted)); then
          delta=$((scraped - previous[$i]))
          # A counter below its last reading restarted with its listener: all of it is new.
          ((delta >= 0)) || delta="$scraped"
          total=$((total + delta))
          ((delta == 0)) || shares+="${shares:+,}${names[i]}=${delta}"
        fi
        previous[$i]="$scraped"
      done
      if ((counted && total > 0)); then
        emit batches "$elapsed" "$total" "$shares"
        minute_batches=$((minute_batches + total))
      fi
      counted=1
      overdue=$((overdue + sched_count - sched_next))
      sched_start="$now"
      sched_count="$total"
      sched_next=0
      sched_step=0
      ((total == 0)) || sched_step=$((tick_us / total))
      next_tick=$((next_tick + tick_us))
      ((next_tick > now)) || next_tick=$((now + tick_us))
      # A read due now is made before the next tick's counter read, so counter reads slower than
      # a tick never starve the reads.
      now="$(now_us)"
      elapsed=$(((now - start) / 1000))
    fi
    due_k=0
    if ((sched_count > 0)); then
      if ((sched_step == 0)); then
        due_k="$sched_count"
      else
        due_k=$(((now - sched_start) / sched_step + 1))
        ((due_k <= sched_count)) || due_k="$sched_count"
      fi
    fi
    due=$((overdue + due_k - sched_next))
    if ((due > 0)); then
      overdue=0
      sched_next="$due_k"
      if read_once; then
        emit read "$elapsed" ok "$read_us" "$read_code" "$due"
        minute_reads=$((minute_reads + 1))
        ((read_us <= minute_longest)) || minute_longest="$read_us"
      else
        emit read "$elapsed" failed "$read_us" "$read_code" "$due" "$read_error"
        why=failed
        break
      fi
      continue
    fi
    wake="$next_tick"
    if ((sched_next < sched_count && sched_start + sched_next * sched_step < wake)); then
      wake=$((sched_start + sched_next * sched_step))
    fi
    ((start + run_us >= wake)) || wake=$((start + run_us))
    if ((wake > now)); then
      sleep "$(((wake - now) / 1000000)).$(printf '%06d' "$(((wake - now) % 1000000))")" || true
    fi
  done
  now="$(now_us)"
  emit end "$(((now - start) / 1000))" "$why"
  (verdict "$log_file") || code=$?
  exit "$code"
}

# The production run's identity and place (job): an account of its own, which the broker answers
# as a privacy reader, in the listener's namespace, whose pods the broker's ingress admits by
# measure_label.
readonly measure_account=project-privacy-measure
readonly measure_namespace=envoy
readonly measure_label=app.kubernetes.io/name

# job ARG... prints the ServiceAccount and the Job of one production run.
job() {
  local run_id="" image="" source="" audience="" minutes="$hour_minutes" url
  local -a metrics=()
  while (($#)); do
    (($# >= 2)) || die_usage "$1 needs a value"
    case "$1" in
    --run) run_id="$2" ;;
    --image) image="$2" ;;
    --source) source="$2" ;;
    --audience) audience="$2" ;;
    --metrics-url) metrics+=("$2") ;;
    --minutes) minutes="$2" ;;
    *) die_usage "unknown argument $1" ;;
    esac
    shift 2
  done
  [[ "$run_id" =~ ^[0-9a-z]{1,30}$ ]] || die_usage "--run must be 1 to 30 lower-case letters and digits, got ${run_id:-nothing}"
  [[ "$image" =~ ^[^[:space:]\"\\]+$ ]] || die_usage "--image is required and holds no space, quote or backslash"
  [[ "$source" =~ ^https?://[^[:space:]\"\\]+$ ]] || die_usage "--source must be an http(s) URL, got ${source:-nothing}"
  [[ "$audience" =~ ^[^[:space:]\"\\]+$ ]] || die_usage "--audience is required and holds no space, quote or backslash"
  ((${#metrics[@]} > 0)) || die_usage "--metrics-url is required"
  for url in "${metrics[@]}"; do
    [[ "$url" =~ ^([a-z0-9][a-z0-9.-]{0,62}=)?https?://[^[:space:]\",=\\]+$ ]] ||
      die_usage "--metrics-url must be [NAME=]an http(s) URL, got ${url:-nothing}"
  done
  if ! [[ "$minutes" =~ ^[0-9]+$ ]] || ((minutes < hour_minutes)); then
    die_usage "--minutes is ${minutes}: a run shorter than an hour sets no bound, so it is refused"
  fi
  local name="project-privacy-measure-${run_id}" args
  args="\"run\", \"--source\", \"${source}\", \"--token-file\", \"/var/run/secrets/envoy-broker/token\", \"--minutes\", \"${minutes}\""
  for url in "${metrics[@]}"; do
    args+=", \"--metrics-url\", \"${url}\""
  done
  # The deadline is the run's minutes and a quarter of an hour for the image's start.
  cat <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${measure_account}
  namespace: ${measure_namespace}
  labels: {${measure_label}: ${measure_account}, project-privacy-measure/run: "${run_id}"}
automountServiceAccountToken: false
---
apiVersion: batch/v1
kind: Job
metadata:
  name: ${name}
  namespace: ${measure_namespace}
  labels: {${measure_label}: ${measure_account}, project-privacy-measure/run: "${run_id}"}
spec:
  backoffLimit: 0
  activeDeadlineSeconds: $(((minutes + 15) * 60))
  template:
    metadata:
      labels: {${measure_label}: ${measure_account}, project-privacy-measure/run: "${run_id}"}
    spec:
      serviceAccountName: ${measure_account}
      automountServiceAccountToken: false
      restartPolicy: Never
      securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: measure
          image: "${image}"
          command: ["bash", "/measure/project-privacy-measure.sh", ${args}]
          env: [{name: TMPDIR, value: /tmp}]
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          resources: {requests: {cpu: 50m, memory: 32Mi}, limits: {memory: 64Mi}}
          volumeMounts:
            - {name: script, mountPath: /measure, readOnly: true}
            - {name: broker-token, mountPath: /var/run/secrets/envoy-broker, readOnly: true}
            - {name: tmp, mountPath: /tmp}
      volumes:
        - {name: script, configMap: {name: ${name}}}
        - {name: broker-token, projected: {sources: [{serviceAccountToken: {audience: "${audience}", expirationSeconds: 3600, path: token}}]}}
        - {name: tmp, emptyDir: {}}
EOF
}

case "${1:-}" in
run)
  shift
  run "$@"
  ;;
verdict)
  (($# == 2)) || die_usage "verdict takes one log"
  verdict "$2"
  ;;
job)
  shift
  job "$@"
  ;;
-h | --help)
  usage
  exit 0
  ;;
*) die_usage "the first argument is run, verdict or job" ;;
esac
