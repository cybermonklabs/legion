#!/usr/bin/env bash
# project-privacy-measure.test.sh — Proves project-privacy-measure.sh's verdict over fixture logs,
# and its run over one fake process that serves both a listener's /metrics and the broker's
# GET /v1/project-privacy on 127.0.0.1. A run here takes a shortened minute
# (PROJECT_PRIVACY_MEASURE_MINUTE_MS=100, so its hour is 6 s); the verdict of each such run is
# taken under the same minute, and case (b9) shows a production verdict refuses it.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/project-privacy-measure.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly output_file="${temporary_dir}/output"
readonly server="${temporary_dir}/fake.py"
readonly token_file="${temporary_dir}/token"
readonly test_minute_ms=100
printf 'measure-token\n' >"$token_file"

server_pid=""
stop_server() {
  [[ -z "$server_pid" ]] || {
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  }
  server_pid=""
}
cleanup() {
  stop_server
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

command -v python3 >/dev/null || {
  printf 'ERR: python3 is required\n' >&2
  exit 4
}

# The fake answers GET /metrics as a listener does, its two counter families each rising by STEP
# at every request (METRICS: counting; missing leaves envoy_listener_requests_total out), and
# GET /v1/project-privacy as the broker does: 403 not_listener without the measure's bearer, else
# {"projects":[]}, except the read numbered AT, which PRIVACY makes slow (answers after SLOW_MS),
# 502 (dispatch_unavailable, the broker's answer when Dispatch does not answer) or null
# ({"projects":null}, no whole answer). GET /stats is its own count of privacy reads and the most
# it ever had in flight at once.
cat >"$server" <<'EOF'
import http.server
import json
import os
import sys
import threading
import time

port_file, metrics_mode, step, privacy_mode, at, slow_ms = sys.argv[1:7]
step, at, slow_ms = int(step), int(at), int(slow_ms)
lock = threading.Lock()
state = {"scrapes": 0, "session_scrapes": 0, "reads": 0, "inflight": 0, "inflight_max": 0}


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def answer(self, status, body, content_type="application/json"):
        data = body.encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        # /session/metrics is a second listener, the session listener, whose counters rise twice as
        # fast and are counted on their own.
        if self.path in ("/metrics", "/session/metrics"):
            key = "session_scrapes" if self.path == "/session/metrics" else "scrapes"
            with lock:
                state[key] += 1
                value = state[key] * step * (2 if key == "session_scrapes" else 1)
            lines = [
                "# HELP envoy_messages_received_total Total messages received by the listener",
                "# TYPE envoy_messages_received_total counter",
                f'envoy_messages_received_total{{source="nats",topic_prefix="notifications.dispatch"}} {value}',
                f'envoy_messages_received_total{{source="nats",topic_prefix="notifications.agent"}} {value}',
                "# HELP envoy_messages_received_totals_unrelated A family whose name starts with the counter's",
                "# TYPE envoy_messages_received_totals_unrelated counter",
                "envoy_messages_received_totals_unrelated 1000",
            ]
            if metrics_mode != "missing":
                lines += [
                    "# HELP envoy_listener_requests_total Requests /v1 admitted",
                    "# TYPE envoy_listener_requests_total counter",
                    f'envoy_listener_requests_total{{credential="broker",route="/v1/register"}} {value}',
                ]
            self.answer(200, "\n".join(lines) + "\n", "text/plain; version=0.0.4")
            return
        if self.path == "/stats":
            with lock:
                self.answer(200, f'reads={state["reads"]} inflight_max={state["inflight_max"]}\n', "text/plain")
            return
        if self.path != "/v1/project-privacy":
            self.answer(404, '{"error":"not_found"}')
            return
        if self.headers.get("Authorization") != "Bearer measure-token":
            self.answer(403, '{"error":"not_listener"}')
            return
        with lock:
            state["reads"] += 1
            number = state["reads"]
            state["inflight"] += 1
            state["inflight_max"] = max(state["inflight_max"], state["inflight"])
        try:
            if number == at and privacy_mode == "slow":
                time.sleep(slow_ms / 1000)
            if number == at and privacy_mode == "502":
                self.answer(502, '{"error":"dispatch_unavailable"}')
            elif number == at and privacy_mode == "null":
                self.answer(200, '{"projects":null}')
            else:
                self.answer(200, '{"projects":[]}')
        finally:
            with lock:
                state["inflight"] -= 1


httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
with open(port_file + ".tmp", "w") as f:
    f.write(f"{httpd.server_address[1]}\n")
os.rename(port_file + ".tmp", port_file)
httpd.serve_forever()
EOF

port=""
starts=0
# start_fake METRICS STEP PRIVACY AT SLOW_MS starts the fake and sets $port.
start_fake() {
  stop_server
  starts=$((starts + 1))
  local port_file="${temporary_dir}/port.${starts}"
  python3 "$server" "$port_file" "$@" &
  server_pid="$!"
  for _ in $(seq 100); do
    [[ ! -s "$port_file" ]] || break
    kill -0 "$server_pid" 2>/dev/null || {
      printf 'FAIL: the fake exited before it wrote its port\n' >&2
      exit 1
    }
    sleep 0.1
  done
  [[ -s "$port_file" ]] || {
    printf 'FAIL: the fake never wrote its port\n' >&2
    exit 1
  }
  read -r port <"$port_file"
}

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  cat "$output_file" >&2
  exit 1
}

status=0
expect_status() {
  [[ "$status" == "$1" ]] || fail "$2: exit $status, want $1"
}

expect_line() {
  grep -Fqx -- "$1" "$output_file" || fail "$2: no line $(printf '%q' "$1")"
}

expect_text() {
  grep -Fq -- "$1" "$output_file" || fail "$2: no $(printf '%q' "$1")"
}

expect_no_text() {
  ! grep -Fq -- "$1" "$output_file" || fail "$2: unexpected $(printf '%q' "$1")"
}

# fake_count NAME prints the fake's count NAME (reads or inflight_max).
fake_count() {
  local stats
  stats="$(curl -sS "http://127.0.0.1:${port}/stats")"
  [[ " ${stats}" =~ \ $1=([0-9]+) ]] || fail "the fake's /stats has no $1: ${stats}"
  printf '%s' "${BASH_REMATCH[1]}"
}

# run_case [ARG...] runs the script's run against the fake under the test minute, with its output
# in $output_file and its exit code in $status.
run_case() {
  status=0
  PROJECT_PRIVACY_MEASURE_MINUTE_MS="$test_minute_ms" bash "$driver" run \
    --source "http://127.0.0.1:${port}" --metrics-url "http://127.0.0.1:${port}/metrics" "$@" \
    >"$output_file" 2>&1 || status=$?
}

# verdict_case LOG [MINUTE_MS] judges LOG, under the production minute unless MINUTE_MS is given.
verdict_case() {
  status=0
  if (($# > 1)); then
    PROJECT_PRIVACY_MEASURE_MINUTE_MS="$2" bash "$driver" verdict "$1" >"$output_file" 2>&1 || status=$?
  else
    env -u PROJECT_PRIVACY_MEASURE_MINUTE_MS bash "$driver" verdict "$1" >"$output_file" 2>&1 || status=$?
  fi
}

# fixture NAME LONGEST_US [END_MS [END_WHY]] writes a log of an hour's run under the production
# minute: 99 answered reads of 1 to 99 ms, each serving 2 decisions, then one of LONGEST_US, and an
# end line at END_MS (3600000) with END_WHY (done), and prints its path.
fixture() {
  local log="${temporary_dir}/$1.log" longest="$2" end_ms="${3:-3600000}" why="${4:-done}" i
  {
    printf 'start\t60000\t2026-11-20T00:00:00Z\thttp://broker.test/v1/project-privacy\n'
    printf '# counters: envoy_messages_received_total envoy_listener_requests_total; listeners: http://listener.test:9020/metrics\n'
    printf 'batches\t1000\t200\n'
    for i in $(seq 99); do
      printf 'read\t%d\tok\t%d\t200\t2\n' "$((i * 30000))" "$((i * 1000))"
    done
    printf 'read\t3000000\tok\t%d\t200\t2\n' "$longest"
    printf 'minute\t3600000\t59\t0\t0\t0\n'
    [[ "$end_ms" == none ]] || printf 'end\t%s\t%s\n' "$end_ms" "$why"
  } >"$log"
  printf '%s' "$log"
}

# (a) A run shorter than an hour is refused before it reads anything.
start_fake counting 1 ok 0 0
run_case --token-file "$token_file" --minutes 59
expect_status 2 "a 59-minute run"
expect_text 'a run shorter than an hour sets no bound, so it is refused' "a 59-minute run"
[[ "$(fake_count reads)" == 0 ]] || fail "a 59-minute run: the fake saw privacy reads"
printf 'PASS: a run shorter than an hour is refused (exit 2)\n'

# (b1) The constant from a fixture log: the longest read is 100 ms, so twice it is 200 ms.
verdict_case "$(fixture hundred 100000)"
expect_status 0 "a 100 ms longest read"
expect_line 'reads: 100 answered, serving 200 of 200 decisions, over 60m0s (minute = 60000 ms)' "a 100 ms longest read"
expect_line 'read time: p50 50.000 ms, p99 99.000 ms, longest 100.000 ms' "a 100 ms longest read"
expect_line 'verdict: k7ReadTimeout = 200 * time.Millisecond (twice the longest read, 200.000 ms, rounded up to the next 100 ms)' "a 100 ms longest read"
printf 'PASS: a fixture whose longest read is 100 ms sets 200 ms (exit 0)\n'

# (b2) Control: the same fixture with its longest read one millisecond longer sets another constant.
verdict_case "$(fixture hundred-and-one 101000)"
expect_status 0 "a 101 ms longest read"
expect_line 'verdict: k7ReadTimeout = 300 * time.Millisecond (twice the longest read, 202.000 ms, rounded up to the next 100 ms)' "a 101 ms longest read"
printf 'PASS: control - one millisecond more on the longest read changes the constant to 300 ms\n'

# (b3) Twice the longest read at the cap exactly is the cap.
verdict_case "$(fixture at-cap 1000000)"
expect_status 0 "a 1 s longest read"
expect_line 'verdict: k7ReadTimeout = 2 * time.Second (twice the longest read, 2000.000 ms, rounded up to the next 100 ms)' "a 1 s longest read"
printf 'PASS: a longest read of half the cap sets the cap (exit 0)\n'

# (b4) Past the cap no constant is set.
verdict_case "$(fixture over-cap 1000001)"
expect_status 1 "a longest read past half the cap"
expect_line 'verdict: over the cap - twice the longest read is 2000.002 ms, above the 2000.000 ms k7ReadTimeout ships at; no constant is set' "a longest read past half the cap"
expect_no_text 'k7ReadTimeout =' "a longest read past half the cap"
printf 'PASS: twice the longest read past 2 s sets no constant (exit 1)\n'

# (b5) A failed read makes the hour UNKNOWN, whatever the answered reads say.
failed_log="$(fixture failed 100000)"
printf 'read\t3100000\tfailed\t40000\t502\t1\t{"error":"dispatch_unavailable"}\n' >>"$failed_log"
verdict_case "$failed_log"
expect_status 3 "a failed read"
expect_line 'UNKNOWN: a read failed at 51m40s after 40.000 ms: HTTP 502, {"error":"dispatch_unavailable"}' "a failed read"
expect_line 'verdict: UNKNOWN - a bound needs an hour of answered reads; no constant is set' "a failed read"
expect_no_text 'k7ReadTimeout =' "a failed read"
printf 'PASS: a failed read is UNKNOWN (exit 3)\n'

# (b6) A log with no read is UNKNOWN, not a bound of nothing.
no_reads_log="${temporary_dir}/no-reads.log"
printf 'start\t60000\t2026-11-20T00:00:00Z\thttp://broker.test/v1/project-privacy\nend\t3600000\tdone\n' >"$no_reads_log"
verdict_case "$no_reads_log"
expect_status 3 "no read"
expect_line 'UNKNOWN: no read happened: the counters counted no decision, so nothing was measured' "no read"
expect_no_text 'k7ReadTimeout =' "no read"
printf 'PASS: a run in which no read happened is UNKNOWN (exit 3)\n'

# (b7) Less than an hour, an unfinished run and a stopped run are each UNKNOWN.
verdict_case "$(fixture short 100000 3599999)"
expect_status 3 "a run of 59m59s"
expect_line 'UNKNOWN: the run covered 59m59s, less than an hour' "a run of 59m59s"
verdict_case "$(fixture unfinished 100000 none)"
expect_status 3 "an unfinished run"
expect_line 'UNKNOWN: the log has no end line: the run did not finish' "an unfinished run"
verdict_case "$(fixture stopped 100000 3700000 stopped)"
expect_status 3 "a stopped run"
expect_line 'UNKNOWN: the run was stopped at 61m40s' "a stopped run"
printf 'PASS: a run shorter than an hour, unfinished or stopped is UNKNOWN (exit 3)\n'

# (b8) A failed counter read is UNKNOWN.
scrape_log="$(fixture scrape 100000)"
printf 'scrape\t2000000\tfailed\thttp://listener.test:9020/metrics\tcurl: (7) Failed to connect\n' >>"$scrape_log"
verdict_case "$scrape_log"
expect_status 3 "a failed counter read"
expect_line 'UNKNOWN: a counter read failed at 33m20s: http://listener.test:9020/metrics: curl: (7) Failed to connect' "a failed counter read"
printf 'PASS: a failed counter read is UNKNOWN (exit 3)\n'

# (c1) A run with a planted slow read: the fifth read answers after 500 ms, the others at once. The
# run measures it and sets a constant from it; at most one read was ever in flight; and the
# verdict of the run's printed output, as a Job's log keeps it, is the run's own.
start_fake counting 1 slow 5 500
run_case --token-file "$token_file"
expect_status 0 "a planted slow read"
planted_verdict="$(grep '^verdict: ' "$output_file")" || fail "a planted slow read: no verdict line"
[[ "$planted_verdict" =~ k7ReadTimeout\ =\ ([0-9]+)\ \*\ time\.(Millisecond|Second) ]] ||
  fail "a planted slow read: no constant in ${planted_verdict}"
planted_ms="${BASH_REMATCH[1]}"
[[ "${BASH_REMATCH[2]}" == Millisecond ]] || planted_ms=$((planted_ms * 1000))
((planted_ms >= 1100)) || fail "a planted slow read: constant ${planted_ms} ms, want at least 1100 ms from a 500 ms read"
longest_line="$(grep -E $'^read\t[0-9]+\tok\t' "$output_file" | sort -t $'\t' -k4,4n | tail -n 1)"
IFS=$'\t' read -r _ _ _ longest_us _ <<<"$longest_line"
((longest_us >= 500000)) || fail "a planted slow read: the longest read line is ${longest_us} us"
[[ "$(fake_count reads)" -ge 5 ]] || fail "a planted slow read: the fake saw $(fake_count reads) reads, want 5 or more"
[[ "$(fake_count inflight_max)" == 1 ]] || fail "a planted slow read: $(fake_count inflight_max) reads were in flight at once"
served="$(awk -F '\t' '$1 == "read" { s += $6 } END { print s + 0 }' "$output_file")"
counted="$(awk -F '\t' '$1 == "batches" { s += $3 } END { print s + 0 }' "$output_file")"
((served > 0 && served <= counted)) || fail "a planted slow read: reads served ${served} of ${counted} decisions"
cp "$output_file" "${temporary_dir}/planted.log"
verdict_case "${temporary_dir}/planted.log" "$test_minute_ms"
expect_status 0 "the planted run's log judged again"
expect_line "$planted_verdict" "the planted run's log judged again"
printf 'PASS: a planted 500 ms read is measured and sets %d ms; one read in flight; its log judges the same\n' "$planted_ms"

# (b9) The same log under the production minute is UNKNOWN: a shortened run never sets a bound.
verdict_case "${temporary_dir}/planted.log"
expect_status 3 "a shortened run under the production minute"
expect_line "UNKNOWN: the run's minute is ${test_minute_ms} ms and this verdict's is 60000 ms" "a shortened run under the production minute"
printf 'PASS: a run on a shortened minute is UNKNOWN under the production minute (exit 3)\n'

# (c2) Control for (c1): the same run with no slow read sets a smaller constant.
start_fake counting 1 ok 0 0
run_case --token-file "$token_file"
expect_status 0 "no planted slow read"
[[ "$(grep '^verdict: ' "$output_file")" =~ k7ReadTimeout\ =\ ([0-9]+)\ \*\ time\.(Millisecond|Second) ]] ||
  fail "no planted slow read: no constant"
control_ms="${BASH_REMATCH[1]}"
[[ "${BASH_REMATCH[2]}" == Millisecond ]] || control_ms=$((control_ms * 1000))
((control_ms < planted_ms)) || fail "no planted slow read: constant ${control_ms} ms, not below the planted run's ${planted_ms} ms"
printf 'PASS: control - without the slow read the constant is %d ms, below %d ms\n' "$control_ms" "$planted_ms"

# (c3) The third read answers 502: the run stops there, UNKNOWN, naming the answer.
start_fake counting 1 502 3 0
run_case --token-file "$token_file"
expect_status 3 "a 502 at the third read"
expect_text $'\tfailed\t' "a 502 at the third read"
expect_text $'\t502\t' "a 502 at the third read"
expect_text 'UNKNOWN: a read failed at ' "a 502 at the third read"
expect_text 'HTTP 502, {"error":"dispatch_unavailable"}' "a 502 at the third read"
[[ "$(fake_count reads)" == 3 ]] || fail "a 502 at the third read: the fake saw $(fake_count reads) reads, want the run to stop at 3"
printf 'PASS: a failed read stops the run at that read, UNKNOWN (exit 3)\n'

# (c4) The second read answers {"projects":null}: no whole answer, UNKNOWN.
start_fake counting 1 null 2 0
run_case --token-file "$token_file"
expect_status 3 "a null answer"
expect_text 'not a whole answer: {"projects":null}' "a null answer"
[[ "$(fake_count reads)" == 2 ]] || fail "a null answer: the fake saw $(fake_count reads) reads, want 2"
printf 'PASS: an answer that is not whole stops the run, UNKNOWN (exit 3)\n'

# (c5) A token the broker refuses: the first read is 403, UNKNOWN.
start_fake counting 1 ok 0 0
printf 'another-token\n' >"${temporary_dir}/wrong-token"
run_case --token-file "${temporary_dir}/wrong-token"
expect_status 3 "a refused identity"
expect_text 'HTTP 403, {"error":"not_listener"}' "a refused identity"
printf 'PASS: a refused identity is UNKNOWN (exit 3)\n'

# (c6) A listener whose counters never move: no read happens, UNKNOWN, and the fake saw none.
start_fake counting 0 ok 0 0
run_case --token-file "$token_file"
expect_status 3 "counters that never move"
expect_line 'UNKNOWN: no read happened: the counters counted no decision, so nothing was measured' "counters that never move"
[[ "$(fake_count reads)" == 0 ]] || fail "counters that never move: the fake saw $(fake_count reads) reads"
printf 'PASS: a run in which no read happened is UNKNOWN (exit 3)\n'

# (c7) A counter the listener does not expose is a failed counter read, never a quiet zero.
start_fake missing 1 ok 0 0
run_case --token-file "$token_file"
expect_status 3 "a counter not exposed"
expect_text 'the counter envoy_listener_requests_total is not exposed there' "a counter not exposed"
[[ "$(fake_count reads)" == 0 ]] || fail "a counter not exposed: the fake saw $(fake_count reads) reads"
printf 'PASS: a counter the listener does not expose is UNKNOWN (exit 3)\n'

# check_both_listeners FILE prints why the run whose output is FILE does not name and count both
# listeners' pods, or nothing: a listener line for each, and in the verdict a count of each one's
# decisions, each above zero and together the run's.
check_both_listeners() {
  local file="$1" why="" line total listener sum=0
  for listener in envoy-listener-a envoy-nats-listener-0; do
    grep -Fq "$(printf 'listener\t0\t%s\thttp://' "$listener")" "$file" || why+="no listener line names ${listener}; "
  done
  line="$(grep '^decisions by listener: ' "$file")" || {
    printf 'no "decisions by listener" line; %s' "$why"
    return
  }
  for listener in envoy-listener-a envoy-nats-listener-0; do
    if [[ "$line" =~ (^|[:,]\ )${listener}\ ([0-9]+)(,|$) ]] && ((BASH_REMATCH[2] > 0)); then
      sum=$((sum + BASH_REMATCH[2]))
    else
      why+="${listener} counted no decision; "
    fi
  done
  total="$(sed -n 's/^reads: .* of \([0-9]*\) decisions.*/\1/p' "$file")"
  if [[ -z "$why" && "$sum" != "$total" ]]; then
    why+="the listeners' decisions add to ${sum}, not the run's ${total}; "
  fi
  printf '%s' "$why"
}

# both_listeners_run [NAME=URL...] runs the script against the fake's two listeners as the production
# command names them, envoy-listener-a's /metrics and the session listener envoy-nats-listener-0's,
# or against the ones given.
both_listeners_run() {
  local -a urls=("$@")
  ((${#urls[@]} > 0)) || urls=("envoy-listener-a=http://127.0.0.1:${port}/metrics" "envoy-nats-listener-0=http://127.0.0.1:${port}/session/metrics")
  local -a args=()
  local url
  for url in "${urls[@]}"; do
    args+=(--metrics-url "$url")
  done
  status=0
  PROJECT_PRIVACY_MEASURE_MINUTE_MS="$test_minute_ms" bash "$driver" run --source "http://127.0.0.1:${port}" \
    --token-file "$token_file" "${args[@]}" >"$output_file" 2>&1 || status=$?
}

# (c8) The production command reads both workloads' pods, the webhook listener's and the session
# listener's, which holds and decides frames in the required mode too: the run names each and counts
# each one's decisions.
start_fake counting 1 ok 0 0
both_listeners_run
expect_status 0 "a run over both listeners"
listeners_why="$(check_both_listeners "$output_file")"
[[ -z "$listeners_why" ]] || fail "a run over both listeners: ${listeners_why}"
printf 'PASS: a run over both listeners names each pod and counts each one'"'"'s decisions\n'

# (c9) Control: the same run with the session listener left out fails the check, naming it.
start_fake counting 1 ok 0 0
both_listeners_run "envoy-listener-a=http://127.0.0.1:${port}/metrics"
expect_status 0 "a run over the webhook listener alone"
listeners_why="$(check_both_listeners "$output_file")"
[[ "$listeners_why" == *"no listener line names envoy-nats-listener-0"* && "$listeners_why" == *"envoy-nats-listener-0 counted no decision"* ]] ||
  fail "control: a run without the session listener passed the check (${listeners_why:-no reason})"
printf 'PASS: control - a run without the session listener fails the check, naming it\n'

# check_job_account FILE prints why the manifest in FILE does not run under the measurement's own
# account, or nothing: the ServiceAccount project-privacy-measure and the Job, both in namespace
# envoy, the Job's pods running as that account and carrying the label the broker's ingress admits,
# and the listener's account and label on nothing (its pods' names in the /metrics URLs may name it).
check_job_account() {
  local file="$1" why=""
  grep -Fqx 'kind: ServiceAccount' "$file" || why+="no ServiceAccount; "
  grep -Fqx '  name: project-privacy-measure' "$file" || why+="no object is named project-privacy-measure; "
  [[ "$(grep -c -Fx '  namespace: envoy' "$file")" == 2 ]] || why+="the account and the Job are not both in namespace envoy; "
  grep -Fqx '      serviceAccountName: project-privacy-measure' "$file" || why+="the Job's pods do not run as project-privacy-measure; "
  grep -Fqx '      labels: {app.kubernetes.io/name: project-privacy-measure, project-privacy-measure/run: "r1"}' "$file" ||
    why+="the Job's pods do not carry app.kubernetes.io/name: project-privacy-measure; "
  ! grep -Eq 'serviceAccountName: envoy-listener|app: envoy-listener|name: envoy-listener$' "$file" ||
    why+="it runs as, or is named or labelled, envoy-listener; "
  printf '%s' "$why"
}

# (j1) The production run's manifest: an account of its own, which the broker answers as a privacy
# reader, never the listener's.
readonly job_file="${temporary_dir}/job.yaml"
status=0
bash "$driver" job --run r1 --image registry.test/envoy@sha256:0123 --source https://broker.test --audience broker.test \
  --metrics-url envoy-listener-a=http://10.0.0.5:9020/metrics --metrics-url envoy-nats-listener-0=http://10.0.0.7:9020/metrics \
  >"$job_file" 2>"$output_file" || status=$?
expect_status 0 "rendering the production run's manifest"
account_why="$(check_job_account "$job_file")"
[[ -z "$account_why" ]] || {
  cat "$job_file" >>"$output_file"
  fail "the production run's manifest: ${account_why}"
}
grep -Fq '"--metrics-url", "envoy-listener-a=http://10.0.0.5:9020/metrics", "--metrics-url", "envoy-nats-listener-0=http://10.0.0.7:9020/metrics"' "$job_file" || {
  cat "$job_file" >>"$output_file"
  fail "the production run's manifest does not pass both listeners' named /metrics to the run"
}
printf 'PASS: the production run runs as the ServiceAccount project-privacy-measure in namespace envoy\n'

# (j2) Control: the same manifest with its pods running as the listener's account fails the check,
# naming both the account and the listener.
sed 's/^      serviceAccountName: project-privacy-measure$/      serviceAccountName: envoy-listener/' "$job_file" >"${temporary_dir}/job-control.yaml"
account_why="$(check_job_account "${temporary_dir}/job-control.yaml")"
[[ "$account_why" == *"do not run as project-privacy-measure"* && "$account_why" == *"runs as, or is named or labelled, envoy-listener"* ]] ||
  fail "control: a manifest running as envoy-listener passed the account check (${account_why:-no reason})"
printf 'PASS: control - a manifest running as envoy-listener fails the account check\n'

# (j3) The manifest of a run shorter than an hour is refused, as the run is.
status=0
bash "$driver" job --run r1 --image registry.test/envoy@sha256:0123 --source https://broker.test \
  --audience broker.test --metrics-url http://10.0.0.5:9020/metrics --minutes 59 >"$output_file" 2>&1 || status=$?
expect_status 2 "the manifest of a 59-minute run"
expect_text 'a run shorter than an hour sets no bound, so it is refused' "the manifest of a 59-minute run"
printf 'PASS: the manifest of a run shorter than an hour is refused (exit 2)\n'

printf 'PASS: project-privacy-measure.sh\n'
