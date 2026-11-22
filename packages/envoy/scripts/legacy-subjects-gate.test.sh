#!/usr/bin/env bash
# packages/envoy/scripts/legacy-subjects-gate.test.sh
#
# Verdict test for legacy-subjects-gate.sh, behind a fake kubectl and a fake curl that answer from
# a case directory and record every call, so it reads no cluster and no bus. The NATS Job's log is
# written here in the Job's own format (lib/legacy-subjects-gate-nats.sh: five tab-separated fields,
# the answers in base64, `end` last), its answers shaped as the nats CLI of the pinned nats-box
# printed them against nats-server 2.15.1 under the in-cluster listener's grant; and the Job's own
# script is run under sh behind a fake nats, so the log it writes is the log the gate is given.
#
# Each surface the gate exists to read has a case that shuts the gate or makes it UNKNOWN for that
# reason alone, beside the clean fleet that is the only case that opens it: at the second reading, a
# listener's count that rose between the readings, under any kind, a Legion daemon's consumer still
# on an old filter, an old-form message still on the stream, named apart when it came after the
# first reading's last stream sequence, and an interest record holding an old-form topic; at
# either, a consumer list that cannot be read
# at all, a listener whose counter cannot be read, a listener whose restart only its start-time
# gauge shows, a listener offline at the reading, a list that moved between its pages, a stream that
# is not the first reading's or does not hold the whole window, and an answer that is no JSON
# object. A first reading on a bus that still holds old-form messages, interests and consumer
# filters is a first reading that names each. Every case runs from the same clean fixture with one
# thing changed, so each fails for its own reason. The old-form rule itself is held row by row. The
# summary counts cases.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
gate="$here/legacy-subjects-gate.sh"
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
passed=0
failed=0

fail() {
  echo "FAIL: $case_name: $*" >&2
  case_failed=1
}

# --- the fakes ------------------------------------------------------------------------------------
mkdir -p "$root/bin"
cat >"$root/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
# A fake kubectl that answers from the case directory $FAKE and records each call in $FAKE/calls.
# A listener pod's log is its metrics file ($FAKE/metrics/<ns>.<pod>) written as the listener logs
# it, on a "listener legacy subjects" line logged at $FAKE/log_at (now when that file is absent),
# after a line of another message.
set -u
printf '%s\n' "$*" >>"$FAKE/calls"
args=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) shift 2 ;;
    --request-timeout=*) shift ;;
    *) args+=("$1"); shift ;;
  esac
done
set -- "${args[@]}"
case "$*" in
  "get pods --all-namespaces -o json")
    [ ! -e "$FAKE/pods.fail" ] || { echo "error: You must be logged in to the server (Unauthorized)" >&2; exit 1; }
    cat "$FAKE/pods.json" ;;
  "-n envoy logs job/"*) cat "$FAKE/job.log" ;;
  "-n "*" logs "*" -c "*" --since=5m")
    ns=$2
    pod=$4
    if [ -f "$FAKE/metrics/$ns.$pod" ]; then
      at=$(cat "$FAKE/log_at" 2>/dev/null || date -u +%s)
      echo '{"time":"2026-11-21T00:00:00Z","level":"INFO","msg":"role claims cache warm-up"}'
      jq -cn --rawfile m "$FAKE/metrics/$ns.$pod" --argjson at "$at" '{time: ($at | todate), level: "INFO", msg: "listener legacy subjects", at: $at, metrics: $m}'
    else echo "Error from server (Forbidden): pods \"$pod\" is forbidden: User cannot get resource \"pods/log\"" >&2; exit 1
    fi ;;
  "-n envoy get deployment envoy-listener -o json") cat "$FAKE/deployment.json" ;;
  "-n envoy create -f "*" -o name")
    [ ! -e "$FAKE/create.fail" ] || { echo "Error from server (Forbidden): jobs.batch is forbidden" >&2; exit 1; }
    cp "$5" "$FAKE/created-job.json"
    # A file made at the record's path while the gate runs, the path named in record.racer.
    [ ! -e "$FAKE/record.racer" ] || echo '{"made":"while the gate ran"}' >"$(cat "$FAKE/record.racer")"
    echo "job.batch/$(jq -r .metadata.name "$5")" ;;
  "-n envoy get job "*" -o json") cat "$FAKE/job-status.json" ;;
  "-n envoy delete job "*) : ;;
  "-n envoy get job "*" --ignore-not-found -o name")
    [ ! -e "$FAKE/job.stays" ] || echo "job.batch/$5" ;;
  "-n envoy get pods -l job-name="*" -o name") : ;;
  *) echo "fake kubectl: no answer for: $*" >&2; exit 99 ;;
esac
EOF
cat >"$root/bin/curl" <<'EOF'
#!/usr/bin/env bash
# A fake curl for `curl -sS --max-time N -o FILE -w '%{http_code}' URL`, answering URL from
# $FAKE/outside/<host> and recording the call in $FAKE/calls.
set -u
printf 'curl %s\n' "$*" >>"$FAKE/calls"
out=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    -w|--max-time) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
host=${url#*://}
host=${host%%[:/]*}
if [ -f "$FAKE/outside/$host" ]; then cp "$FAKE/outside/$host" "$out"; printf 200
else echo "curl: (7) Failed to connect to $host port 9020" >&2; exit 7
fi
EOF
chmod +x "$root/bin/kubectl" "$root/bin/curl"

# --- the clean fixture ----------------------------------------------------------------------------
readonly envoy_image="ghcr.io/cybermonklabs/legion/envoy@sha256:1111111111111111111111111111111111111111111111111111111111111111"
readonly listener_start=1794900000
readonly hub_start=1794800000
readonly listener_nkey=UDAW25FEZ57VZNGE6FKAMUACIAK3LZRQHNVGUGPJS6L4INOXAQTX4AFQ
readonly durable_lapse_ns=604800000000000

readonly bus_url="nats://envoy-hub.envoy.svc.cluster.local:4222"

# pod NS NAME UID CONTAINER IMAGE COMMAND(JSON) RESTARTS STATE MACHINE [NATS_URLS] prints one pod,
# its container on the listener Deployment's bus unless NATS_URLS names another.
pod() {
  jq -n --arg ns "$1" --arg name "$2" --arg uid "$3" --arg container "$4" --arg image "$5" \
    --argjson command "$6" --argjson restarts "$7" --arg state "$8" --arg machine "$9" --arg urls "${10-$bus_url}" '
    {metadata: {namespace: $ns, name: $name, uid: $uid},
     spec: {containers: [{name: $container, image: $image}
       + (if $command == null then {} else {command: $command} end)
       + {env: ([if $machine == "" then empty else {name: "ENVOY_MACHINE_ID", value: $machine} end]
           + [{name: "NATS_URLS", value: $urls}])}]},
     status: {phase: "Running", containerStatuses: [{name: $container, restartCount: $restarts,
       state: {($state): {}}}]}}'
}

# metrics INTEREST ROLE [START [ROLE_READ [MACHINE]]] prints a listener's metrics text: the
# counter's kinds, each when above zero, and the start-time gauge when START is given, under label
# machine when MACHINE is given.
metrics() {
  echo "# HELP envoy_listener_legacy_subjects_total Names a caller gave in their old form"
  echo "# TYPE envoy_listener_legacy_subjects_total counter"
  [ "$1" = 0 ] || echo "envoy_listener_legacy_subjects_total{kind=\"interest\"} $1"
  [ "$2" = 0 ] || echo "envoy_listener_legacy_subjects_total{kind=\"role\"} $2"
  [ "${4:-0}" = 0 ] || echo "envoy_listener_legacy_subjects_total{kind=\"role_read\"} $4"
  if [ -n "${3:-}" ]; then
    echo "# HELP envoy_listener_start_time_seconds When this listener process started"
    echo "# TYPE envoy_listener_start_time_seconds gauge"
    if [ -n "${5:-}" ]; then
      echo "envoy_listener_start_time_seconds{machine=\"$5\"} $3"
    else
      echo "envoy_listener_start_time_seconds $3"
    fi
  fi
}

# line LEG ARG STATUS OUT ERR prints one line of the Job's log.
line() {
  printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$(printf '%s' "$4" | base64 -w 0)" "$(printf '%s' "$5" | base64 -w 0)"
}

# consumer NAME FILTERS(JSON) [PUSH_BOUND [INACTIVE_THRESHOLD_NS]] prints one ConsumerInfo.
consumer() {
  jq -cn --arg name "$1" --argjson filters "$2" --argjson bound "${3:-null}" --argjson lapse "${4:-null}" '
    {stream_name: "ENVOY_NOTIFICATIONS", name: $name,
     config: ({durable_name: $name} + (if ($filters | length) == 1 then {filter_subject: $filters[0]} else {filter_subjects: $filters} end)
       + (if $lapse == null then {} else {inactive_threshold: $lapse} end)),
     delivered: {consumer_seq: 40, stream_seq: 900, last_active: "2026-11-20T23:59:00Z"}}
    + (if $bound == null then {} else {push_bound: $bound} end)'
}

# consumers_page CONSUMER... prints one consumer list answer holding them all.
consumers_page() {
  printf '%s\n' "$@" | jq -cs '{type: "io.nats.jetstream.api.v1.consumer_list_response", total: length, offset: 0, limit: 256, consumers: .}'
}

# subjects_page SUBJECT... prints one stream info answer listing the subjects, one message each.
subjects_page() {
  printf '%s\n' "$@" | jq -cRs 'split("\n") | map(select(length > 0)) as $s
    | {type: "io.nats.jetstream.api.v1.stream_info_response", total: ($s | length), offset: 0,
       limit: (if ($s | length) == 0 then 0 else 100000 end),
       config: {name: "ENVOY_NOTIFICATIONS", max_age: 259200000000000},
       state: {messages: 9, first_ts: "2026-11-18T00:00:00Z",
         subjects: (if ($s | length) == 0 then null else ($s | map({key: ., value: 1}) | from_entries) end)}}'
}

# interest SESSION TOPIC... prints one interest record as the Job prints it: its session id and its
# topics alone.
interest() {
  local session=$1
  shift
  printf '%s\n' "$@" | jq -cRs --arg s "$session" 'split("\n") | map(select(length > 0)) as $t
    | {session_id: $s, topics: $t}'
}

# stream_info [FIRST_TS [STREAM_ID [LAST_SEQ]]] prints the stream's info answer.
stream_info() {
  jq -cn --arg first "${1:-2026-11-18T00:00:00.123456789Z}" --arg id "${2-stream-id-1}" --argjson last "${3:-900}" '
    {type: "io.nats.jetstream.api.v1.stream_info_response", created: "2026-11-01T00:00:00Z",
     config: {name: "ENVOY_NOTIFICATIONS", max_age: 259200000000000,
       metadata: (if $id == "" then {} else {envoy_stream_id: $id} end)},
     state: {messages: 9, first_ts: $first, last_seq: $last}}'
}

# last_message SUBJECT SEQ prints the Job's line of the last message of SUBJECT, as the Job prints a
# stream message get's answer: its subject, sequence and time alone.
last_message() {
  line old_last "$1" 0 "$(jq -cn --arg s "$1" --argjson q "$2" '{type: "io.nats.jetstream.api.v1.stream_msg_get_response", error: null, message: {subject: $s, seq: $q, time: "2026-11-20T10:00:00Z"}}')" ""
}

# A request the grant refuses, as nats-box 0.19.0 answered it against nats-server 2.15.1 under the
# listener's grant: status 0, nothing on standard output, and the server's error logged on standard
# error with the time first.
readonly user_info_refused="04:10:18 Unexpected NATS error from server nats://envoy-hub.envoy.svc.cluster.local:4222: nats: permissions violation: Permissions Violation for Publish to \"\$SYS.REQ.USER.INFO\""
readonly consumer_list_refused="04:10:18 Unexpected NATS error from server nats://envoy-hub.envoy.svc.cluster.local:4222: nats: permissions violation: Permissions Violation for Publish to \"\$JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS\""

# clean_consumers prints the clean fleet's consumers: a Legion daemon's three, on today's filters;
# a session's consumer filtering both forms of its own agent subject; and the three listeners'
# durables, bound, which the server drops after 7 days unbound.
clean_consumers() {
  consumer legion-go-ACME-dispatch '["notifications.dispatch.issue.>"]'
  consumer legion-go-ACME-github '["notifications.github.>"]'
  consumer legion-go-ACME-wake '["notifications.legion.wake"]'
  consumer ses_twin '["notifications.agent.ses_twin","notifications.agent.*.ses_twin","notifications.dispatch.issue.ACME.ACME-12.>"]'
  consumer listener-gke '["notifications.agent.>","notifications.dispatch.>"]' true "$durable_lapse_ns"
  consumer listener-dispatch '["notifications.agent.>","notifications.dispatch.>"]' true "$durable_lapse_ns"
  consumer listener-envoy-hub '["notifications.agent.>","notifications.dispatch.>"]' true "$durable_lapse_ns"
}

# job_log CONSUMERS_PAGE ISSUE_SUBJECTS_PAGE [INTEREST_KEYS] prints a whole Job log.
job_log() {
  local keys=${3-$'ses_a\nses_b\n'}
  line credential nkey 0 "$listener_nkey" ""
  line user_info - 0 "" "$user_info_refused"
  line stream_info - 0 "$(stream_info)" ""
  line consumers 0 0 "$1" ""
  line subjects "notifications.dispatch.issue.> 0" 0 "$2" ""
  line subjects "notifications.agent.> 0" 0 "$(subjects_page notifications.agent.ACME.ses_a notifications.agent._.ses_b)" ""
  line subjects "notifications.envoy.exceptions.notifications.agent.> 0" 0 "$(subjects_page)" ""
  line subjects "notifications.envoy.kept.> 0" 0 "$(subjects_page notifications.envoy.kept.role._.lead-reviewer)" ""
  line old_last_skipped 1000 0 "0" ""
  line interest_keys - 0 "$keys" ""
  if [ -n "$keys" ]; then
    line interest ses_a 0 "$(interest ses_a notifications.agent.ses_a 'notifications.dispatch.issue.ACME.ACME-12.>')" ""
    line interest ses_b 0 "$(interest ses_b notifications.agent.ses_b notifications.team.deploy-squad)" ""
  fi
  printf 'end\t-\t0\t\t\n'
}

clean_issue_subjects() {
  subjects_page notifications.dispatch.issue.ACME.ACME-12.comment.created notifications.dispatch.issue.ACME.ACME-13.status.changed
}

base="$root/base"
mkdir -p "$base/metrics" "$base/outside"
{
  pod envoy envoy-listener-7f9c envoy-uid-1 envoy-listener "$envoy_image" null 0 running gke
  pod dispatch dispatch-listener-55d1 dispatch-uid-1 envoy-listener "$envoy_image" null 0 running dispatch
  # The Dispatch server and the broker run the same image with a command of their own: no listener.
  pod dispatch dispatch-6c2a dispatch-app-uid envoy-dispatch "$envoy_image" '["/usr/local/bin/envoy-dispatch"]' 0 running ""
  pod dispatch envoy-broker-1 broker-uid envoy-broker "$envoy_image" '["/usr/local/bin/envoy-broker"]' 0 running ""
  # Another program called envoy, from another image path: no listener.
  pod lean-build front-door-1 front-door-uid envoy "us-west1-docker.pkg.dev/example/lean-build/envoy@sha256:2222" '["envoy"]' 0 running ""
} | jq -s '{items: .}' >"$base/pods.json"
metrics 4 1 "$listener_start" 0 gke >"$base/metrics/envoy.envoy-listener-7f9c"
metrics 2 0 "$listener_start" 0 dispatch >"$base/metrics/dispatch.dispatch-listener-55d1"
metrics 7 0 "$hub_start" 0 envoy-hub >"$base/outside/envoy-hub.example-tailnet.ts.net"
jq -n '{spec: {template: {spec: {
  serviceAccountName: "envoy-listener",
  containers: [{name: "envoy-listener", image: "ghcr.io/cybermonklabs/legion/envoy@sha256:1111",
    env: [{name: "ENVOY_MACHINE_ID", value: "gke"}, {name: "NATS_URLS", value: "nats://envoy-hub.envoy.svc.cluster.local:4222"},
          {name: "NATS_NKEY_SEED_FILE", value: "/var/run/secrets/nats-seed/nats-seed"},
          {name: "ENVOY_API_TOKEN", valueFrom: {secretKeyRef: {name: "envoy-listener-api-token", key: "token"}}}],
    volumeMounts: [{name: "nats-seed", mountPath: "/var/run/secrets/nats-seed", readOnly: true}]}],
  volumes: [{name: "nats-seed", csi: {driver: "secrets-store-gke.csi.k8s.io", readOnly: true,
    volumeAttributes: {secretProviderClass: "envoy-listener-nats-seed"}}}]}}}}' >"$base/deployment.json"
echo '{"status":{"succeeded":1}}' >"$base/job-status.json"
job_log "$(consumers_page "$(clean_consumers)")" "$(clean_issue_subjects)" >"$base/job.log"

# --- running a case -------------------------------------------------------------------------------
# new_case NAME copies the clean fixture into a fresh case directory, sets FAKE to it and names the
# case.
new_case() {
  case_name=$1
  case_failed=0
  FAKE="$root/case-$((passed + failed))-$RANDOM"
  cp -r "$base" "$FAKE"
  : >"$FAKE/calls"
  export FAKE
}

# run_gate ARG... runs the gate on the case with the fakes first on PATH, keeping its output in
# $output and its exit status in $status.
run_gate() {
  set +e
  output=$(PATH="$root/bin:$PATH" bash "$gate" "$@" 2>&1)
  status=$?
  set -e
}

# reading [ARG...] runs the gate as a reading of the case, with the hub's listener named, writing
# $FAKE/record.json.
reading() {
  run_gate --context example-cluster --record "$FAKE/record.json" --listener envoy-hub=https://envoy-hub.example-tailnet.ts.net:9020 "$@"
}

# replace_row LEG ARG [LINE...] replaces, in the case's Job log, the one row that is the read LEG of
# ARG with the lines given (none deletes it), failing the case unless exactly one row is that read.
replace_row() {
  local leg=$1 arg=$2 row matched=0
  shift 2
  while IFS= read -r row; do
    if [ "$(cut -f1 <<<"$row")" = "$leg" ] && [ "$(cut -f2 <<<"$row")" = "$arg" ]; then
      matched=$((matched + 1))
      [ "$#" = 0 ] || printf '%s\n' "$@"
    else
      printf '%s\n' "$row"
    fi
  done <"$FAKE/job.log" >"$FAKE/job.log.new"
  mv "$FAKE/job.log.new" "$FAKE/job.log"
  [ "$matched" = 1 ] || fail "the Job log holds $matched rows of the read $leg $arg, want 1"
}

expect_status() {
  [ "$status" = "$1" ] || fail "exit status $status, want $1; output:"$'\n'"$output"
}

expect_line() {
  grep -qF -- "$1" <<<"$output" || fail "no line holds '$1'; output:"$'\n'"$output"
}

expect_no_line() {
  ! grep -qF -- "$1" <<<"$output" || fail "a line holds '$1', which it must not; output:"$'\n'"$output"
}

expect_last() {
  [ "$(tail -n 1 <<<"$output")" = "$1" ] || fail "the last line is '$(tail -n 1 <<<"$output")', want '$1'"
}

done_case() {
  if [ "$case_failed" = 0 ]; then
    passed=$((passed + 1))
    echo "ok: $case_name"
  else
    failed=$((failed + 1))
  fi
}

# first_reading_of HOURS_AGO takes a first reading of the case and writes it, moved HOURS_AGO hours
# into the past, to $FAKE/earlier.json, as a reading taken then would read.
first_reading_of() {
  local hours=$1
  reading
  [ "$status" = 4 ] || fail "the first reading exited $status, want 4 (FIRST READING); output:"$'\n'"$output"
  jq --argjson s "$((hours * 3600))" '.started_at -= $s | .finished_at -= $s | .read_at = (.started_at | todate)
    | .listeners |= map(.read_at -= $s)' "$FAKE/record.json" >"$FAKE/earlier.json"
  rm -f "$FAKE/record.json"
  : >"$FAKE/calls"
}

# --- the old-form rule, row by row ----------------------------------------------------------------
case_name="the old-form rule"
case_failed=0
while IFS=$'\t' read -r want topic; do
  got=$(jq -n -L "$here/lib" --arg t "$topic" 'include "legacy-subjects-gate-forms"; $t | old_form')
  [ "$got" = "$want" ] || fail "old_form($topic) = $got, want $want"
done <<'ROWS'
true	notifications.dispatch.issue.ACME-12.>
true	notifications.dispatch.issue.ACME-12.comment.created
true	notifications.dispatch.issue.ACME-12
true	notifications.dispatch.issue.A-1.>
false	notifications.dispatch.issue.ACME.ACME-12.>
false	notifications.dispatch.issue.>
true	notifications.dispatch.issue.*.comment.created
true	notifications.dispatch.issue.*.*.created
true	notifications.dispatch.issue.*.*.*
true	notifications.dispatch.issue.*.comment.>
true	notifications.envoy.exceptions.notifications.dispatch.issue.*.comment.created
false	notifications.dispatch.issue.*.*.comment.created
false	notifications.dispatch.issue.*.*.>
false	notifications.dispatch.issue.*.>
false	notifications.dispatch.issue.*.*.*.*
false	notifications.dispatch.issue.ACME.*.comment.created
false	notifications.dispatch.issue.*
false	notifications.dispatch.issue.*.ACME-12.>
true	notifications.dispatch.*.*.comment.created
true	notifications.dispatch.*.ACME-12.>
true	notifications.*.*.*.comment.created
true	*.*.issue.*.comment.created
true	notifications.envoy.exceptions.notifications.dispatch.*.*.comment.created
false	notifications.dispatch.*.*.*.comment.created
false	notifications.dispatch.*.>
false	notifications.dispatch.>
false	notifications.dispatch.document.*.spec.>
true	notifications.agent.ses_a
true	notifications.agent.*
false	notifications.agent.PRIV.ses_a
false	notifications.agent._.ses_a
false	notifications.agent.*.ses_a
false	notifications.agent.>
true	notifications.role.legion-priv-priv-1-architect
true	notifications.role.lead-reviewer
false	notifications.role.PRIV.lead-reviewer
false	notifications.role.>
true	notifications.envoy.exceptions.notifications.agent.ses_a
true	notifications.envoy.exceptions.notifications.role.lead-reviewer
false	notifications.envoy.exceptions.notifications.agent.ACME.ses_a
false	notifications.envoy.exceptions.notifications.agent.>
true	notifications.envoy.kept.role.lead-reviewer
true	notifications.envoy.kept.cursor.legion-priv-controller
false	notifications.envoy.kept.role._.lead-reviewer
false	notifications.envoy.kept.role.>
true	notifications.*.reviewer
true	*.role.reviewer
true	notifications.*.SES
true	notifications.envoy.*.role.x
true	notifications.envoy.kept.*.x
true	*.envoy.kept.role.x
true	notifications.envoy.exceptions.notifications.*.ses_a
false	notifications.*.PRIV.reviewer
false	notifications.*.>
false	notifications.envoy.kept.*.PRIV.x
false	notifications.envoy.*.>
false	notifications.legion.wake
false	notifications.legion.infra.wake
false	notifications.dispatch.document.PRIV.spec.>
false	notifications.github.acme.widgets.>
false	notifications.team.deploy-squad
ROWS
while IFS=$'\t' read -r want filter filters; do
  got=$(jq -n -L "$here/lib" --arg f "$filter" --argjson fs "$filters" 'include "legacy-subjects-gate-forms"; $f | has_twin($fs)')
  [ "$got" = "$want" ] || fail "has_twin($filter, $filters) = $got, want $want"
done <<'ROWS'
true	notifications.agent.ses_a	["notifications.agent.ses_a","notifications.agent.*.ses_a"]
true	notifications.dispatch.issue.ACME-12.>	["notifications.dispatch.issue.ACME-12.>","notifications.dispatch.issue.ACME.ACME-12.>"]
true	notifications.dispatch.issue.*.comment.created	["notifications.dispatch.issue.*.comment.created","notifications.dispatch.issue.*.*.comment.created"]
false	notifications.agent.ses_a	["notifications.agent.ses_a"]
false	notifications.agent.ses_a	["notifications.agent.ses_a","notifications.agent.*.ses_b"]
false	notifications.agent.ses_a	["notifications.agent.ses_a","notifications.agent.lower.ses_a"]
false	notifications.agent.ses_a	["notifications.agent.ses_a","notifications.agent.ACME.ses_a"]
false	notifications.dispatch.issue.*.comment.created	["notifications.dispatch.issue.*.comment.created","notifications.dispatch.issue.ACME.*.comment.created"]
true	notifications.dispatch.issue.ACME-12.>	["notifications.dispatch.issue.ACME-12.>","notifications.dispatch.issue.*.ACME-12.>"]
false	notifications.dispatch.issue.ACME-12.>	["notifications.dispatch.issue.ACME-12.>","notifications.dispatch.issue.ZED.ACME-12.>"]
true	notifications.dispatch.*.*.comment.created	["notifications.dispatch.*.*.comment.created","notifications.dispatch.*.*.*.comment.created"]
true	notifications.role.legion-priv-priv-1-architect	["notifications.role.legion-priv-priv-1-architect","notifications.role.PRIV.legion-priv-priv-1-architect"]
false	notifications.role.legion-priv-priv-1-architect	["notifications.role.legion-priv-priv-1-architect","notifications.role.ACME.legion-priv-priv-1-architect"]
false	notifications.envoy.kept.role.lead-reviewer	["notifications.envoy.kept.role.lead-reviewer","notifications.envoy.kept.role._.lead-reviewer"]
true	notifications.envoy.kept.role.lead-reviewer	["notifications.envoy.kept.role.lead-reviewer","notifications.envoy.kept.role.*.lead-reviewer"]
false	notifications.role.lead-reviewer	["notifications.role.lead-reviewer","notifications.role._.lead-reviewer"]
true	notifications.envoy.kept.role.legion-priv-priv-1-architect	["notifications.envoy.kept.role.legion-priv-priv-1-architect","notifications.envoy.kept.role.PRIV.legion-priv-priv-1-architect"]
true	notifications.envoy.exceptions.notifications.agent.ses_a	["notifications.envoy.exceptions.notifications.agent.ses_a","notifications.envoy.exceptions.notifications.agent.*.ses_a"]
true	notifications.*.reviewer	["notifications.*.reviewer","notifications.*.*.reviewer"]
false	notifications.*.reviewer	["notifications.*.reviewer","notifications.*._.reviewer"]
true	notifications.envoy.kept.*.x	["notifications.envoy.kept.*.x","notifications.envoy.kept.*.*.x"]
ROWS
[ "$(jq -n -L "$here/lib" 'include "legacy-subjects-gate-forms"; "a\u0007b\nc" | printable')" = '"a\\x07b\\x0ac"' ] ||
  fail "printable does not write control characters as \\x<hh>"
done_case

# --- usage ----------------------------------------------------------------------------------------
new_case "no --context reads nothing"
run_gate --record "$FAKE/record.json"
expect_status 2
[ ! -s "$FAKE/calls" ] || fail "the gate called $(head -n 1 "$FAKE/calls")"
done_case

new_case "an existing --record is never overwritten"
echo '{}' >"$FAKE/record.json"
run_gate --context example-cluster --record "$FAKE/record.json"
expect_status 2
[ "$(cat "$FAKE/record.json")" = '{}' ] || fail "the record was overwritten"
[ ! -s "$FAKE/calls" ] || fail "the gate called $(head -n 1 "$FAKE/calls")"
done_case

new_case "a --record in a directory that does not exist is refused before anything is read"
run_gate --context example-cluster --record "$FAKE/no-such-dir/record.json"
expect_status 2
expect_line "is no directory the gate can write"
[ ! -s "$FAKE/calls" ] || fail "the gate called $(head -n 1 "$FAKE/calls")"
done_case

new_case "a --token-file with a .. segment is refused"
run_gate --context example-cluster --record "$FAKE/record.json" --token-file /var/run/secrets/envoy/../../etc/token
expect_status 2
expect_line "holds a . or .. segment"
[ ! -s "$FAKE/calls" ] || fail "the gate called $(head -n 1 "$FAKE/calls")"
done_case

new_case "a first reading of another cluster is refused"
first_reading_of 73
jq '.context = "another-cluster"' "$FAKE/earlier.json" >"$FAKE/other.json"
reading --since "$FAKE/other.json"
expect_status 2
expect_line "read the cluster another-cluster"
[ ! -s "$FAKE/calls" ] || fail "the gate called $(head -n 1 "$FAKE/calls")"
done_case

new_case "a first reading of an earlier version of the gate is refused"
first_reading_of 73
jq '.version = 2' "$FAKE/earlier.json" >"$FAKE/old.json"
reading --since "$FAKE/old.json"
expect_status 2
expect_line "is no legacy-subjects-gate reading of this version (3)"
[ ! -s "$FAKE/calls" ] || fail "the gate called $(head -n 1 "$FAKE/calls")"
done_case

new_case "a --listener that is not <machine>=<url> is refused"
run_gate --context example-cluster --record "$FAKE/record.json" --listener envoy-hub
expect_status 2
done_case

new_case "a plain-http --listener is refused unless --insecure is given"
run_gate --context example-cluster --record "$FAKE/record.json" --listener envoy-hub=http://envoy-hub.example-tailnet.ts.net:9020
expect_status 2
expect_line "is plain http: anyone on the way could read or change the count the gate opens on"
[ ! -s "$FAKE/calls" ] || fail "the gate called $(head -n 1 "$FAKE/calls")"
run_gate --context example-cluster --record "$FAKE/record.json" --listener envoy-hub=http://envoy-hub.example-tailnet.ts.net:9020 --insecure
expect_status 4
grep -qF -- "curl -sS --max-time 15 -o" "$FAKE/calls" || fail "the http listener was not read with --insecure"
done_case

# --- the clean fleet ------------------------------------------------------------------------------
new_case "a clean fleet's first reading is a first reading, never GATE OPEN"
reading
expect_status 4
expect_last "FIRST READING: $FAKE/record.json holds a first reading of every surface, with 0 FOUND line(s) naming old-form names still in use; read again at or after $(jq -r '(.finished_at + 259200 + 180) | todate' "$FAKE/record.json") with --since $FAKE/record.json: that reading opens only if the stream then holds no old-form message (none after stream sequence 900), no listener's count moved, and no interest record or consumer filter holds an old form"
expect_no_line "GATE OPEN"
expect_line "PASS: the stream holds 2 subject(s) under notifications.dispatch.issue.>, none in an old form"
expect_line "PASS: no consumer of the stream's 7 filters a name only the old form matches (3 of them are Legion daemons')"
expect_line "PASS: no interest record of the 2 read holds an old-form topic beyond its session's own agent subject"
expect_line "READ: listener pod envoy/envoy-listener-7f9c container envoy-listener: {kind=\"interest\"} 4, {kind=\"role\"} 1 since its start"
expect_line "IDENTITY: the server did not say whom the NATS reads ran as (the request \$SYS.REQ.USER.INFO: answered with nothing: $user_info_refused); they ran with the listener's nkey seed, of the nkey user $listener_nkey"
expect_no_line "dispatch-6c2a"
expect_no_line "front-door"
grep -qF -- "-n dispatch logs dispatch-listener-55d1 -c envoy-listener --since=5m" "$FAKE/calls" || fail "Dispatch's listener was not read from its log"
! grep -qF -- "proxy" "$FAKE/calls" || fail "a listener was read through the pod proxy"
[ "$(jq -r .verdict "$FAKE/record.json")" = complete ] || fail "the record's verdict is $(jq -r .verdict "$FAKE/record.json"), want complete"
[ "$(jq '.listeners | length' "$FAKE/record.json")" = 3 ] || fail "the record holds $(jq '.listeners | length' "$FAKE/record.json") listeners, want 3"
[ "$(jq -r .stream.id "$FAKE/record.json")" = stream-id-1 ] || fail "the record holds the stream id $(jq -r .stream.id "$FAKE/record.json"), want stream-id-1"
[ "$(jq -r .identity.credential.names "$FAKE/record.json")" = "$listener_nkey" ] || fail "the record names the credential $(jq -c .identity "$FAKE/record.json")"
done_case

new_case "the Job connects as the listener, reads only, and is deleted and read back"
reading
expect_status 4
job_file="$FAKE/created-job.json"
if [ ! -f "$job_file" ]; then
  fail "no Job was created"
else
  jq -e --rawfile forms "$here/lib/legacy-subjects-gate-forms.jq" --arg image "mirror.gcr.io/natsio/nats-box:0.19.0@sha256:00f10dd4156f28ece84bb3a52cf356ef220ebaa0494310caa90dec184d9e2b89" '
    .metadata.namespace == "envoy" and (.metadata.name | startswith("legacy-subjects-gate-"))
    and .metadata.labels["created-by"] == "legacy-subjects-gate" and (.metadata.labels.owner | length) > 0
    and .spec.backoffLimit == 0 and .spec.activeDeadlineSeconds == 300 and .spec.ttlSecondsAfterFinished == 600
    and .spec.template.spec.serviceAccountName == "envoy-listener" and .spec.template.spec.automountServiceAccountToken == false
    and .spec.template.spec.securityContext.runAsUser == 65534 and .spec.template.spec.securityContext.runAsNonRoot == true
    and .spec.template.spec.containers[0].image == $image
    and .spec.template.spec.containers[0].securityContext.capabilities.drop == ["ALL"]
    and .spec.template.spec.containers[0].env == [{name: "NATS_URL", value: "nats://envoy-hub.envoy.svc.cluster.local:4222"}, {name: "NATS_NKEY", value: "/var/run/secrets/nats-seed/nats-seed"}, {name: "WORK_DIR", value: "/work"}, {name: "FORMS_JQ", value: $forms}]
    and .spec.template.spec.containers[0].volumeMounts == [{name: "nats-seed", mountPath: "/var/run/secrets/nats-seed", readOnly: true}, {name: "legacy-subjects-gate-work", mountPath: "/work"}]
    and .spec.template.spec.volumes[0].csi.volumeAttributes.secretProviderClass == "envoy-listener-nats-seed"
    and .spec.template.spec.volumes[1] == {name: "legacy-subjects-gate-work", emptyDir: {medium: "Memory", sizeLimit: "16Mi"}}' "$job_file" >/dev/null ||
    fail "the Job is not the listener-identity read Job the gate declares: $(jq -c . "$job_file")"
  [ "$(jq -r '.spec.template.spec.containers[0].command[2]' "$job_file")" = "$(cat "$here/lib/legacy-subjects-gate-nats.sh")" ] ||
    fail "the Job does not run lib/legacy-subjects-gate-nats.sh"
  name=$(jq -r .metadata.name "$job_file")
  grep -qF -- "-n envoy delete job $name" "$FAKE/calls" || fail "the Job was not deleted"
  grep -qF -- "-n envoy get pods -l job-name=$name -o name" "$FAKE/calls" || fail "the Job's pods were not read back"
  expect_line "JOB: deleted envoy/$name, and envoy holds no Job or pod of that name"
fi
done_case

new_case "with --token-file the Job connects with the listener's projected token, and with --inbox-prefix every call takes the prefix"
jq '.spec.template.spec.containers[0].env |= map(select(.name != "NATS_NKEY_SEED_FILE")) + [{name: "NATS_SERVICE_TOKEN_FILE", value: "/var/run/secrets/envoy/nats-token/token"}]
  | .spec.template.spec.containers[0].volumeMounts = [{name: "nats-token", mountPath: "/var/run/secrets/envoy/nats-token", readOnly: true}]
  | .spec.template.spec.volumes = [{name: "nats-token", projected: {sources: [{serviceAccountToken: {audience: "nats", expirationSeconds: 3600, path: "token"}}]}}]' \
  "$base/deployment.json" >"$FAKE/deployment.json"
replace_row credential nkey "$(line credential token 0 '{"sub":"system:serviceaccount:envoy:envoy-listener","aud":["nats"],"iss":"https://container.googleapis.com/x","exp":1}' "")"
reading --token-file /var/run/secrets/envoy/nats-token/token --inbox-prefix _INBOX_envoy_listener_gke
expect_status 4
jq -e --rawfile forms "$here/lib/legacy-subjects-gate-forms.jq" '.spec.template.spec.containers[0].env == [{name: "NATS_URL", value: "nats://envoy-hub.envoy.svc.cluster.local:4222"}, {name: "NATS_TOKEN_FILE", value: "/var/run/secrets/envoy/nats-token/token"}, {name: "WORK_DIR", value: "/work"}, {name: "FORMS_JQ", value: $forms}, {name: "NATS_INBOX_PREFIX", value: "_INBOX_envoy_listener_gke"}]
  and .spec.template.spec.containers[0].volumeMounts[0] == {name: "nats-token", mountPath: "/var/run/secrets/envoy/nats-token", readOnly: true}
  and .spec.template.spec.volumes[0].projected.sources[0].serviceAccountToken.audience == "nats"' "$FAKE/created-job.json" >/dev/null ||
  fail "the Job does not connect with the projected token and the prefix: $(jq -c .spec.template.spec "$FAKE/created-job.json")"
expect_line "they ran with the listener's projected token, for system:serviceaccount:envoy:envoy-listener (audience nats)"
done_case

new_case "a --token-file the listener Deployment mounts no volume for creates no Job and is UNKNOWN"
reading --token-file /var/run/secrets/envoy/nats-token/token
expect_status 3
expect_line "UNKNOWN: the NATS reads: the listener Deployment envoy/envoy-listener lacks a volume mount holding the token file /var/run/secrets/envoy/nats-token/token"
! grep -qF -- "create -f" "$FAKE/calls" || fail "a Job was created"
done_case

new_case "the server's own word on the identity is printed when it gives one"
# shellcheck disable=SC2016 # $G is the account's name in the server's answer, not a parameter
replace_row user_info - "$(line user_info - 0 '{"server":{"name":"hub"},"data":{"user":"UDAW25FEZ57VZNGE6FKAMUACIAK3LZRQHNVGUGPJS6L4INOXAQTX4AFQ","account":"$G","account_name":"$G"}}' "")"
reading
expect_status 4
expect_line "IDENTITY: the server answers that the NATS reads ran as user $listener_nkey, account \$G"
done_case

new_case "a clean fleet read twice, 73 hours apart, opens"
first_reading_of 73
reading --since "$FAKE/earlier.json"
expect_status 0
expect_last "GATE OPEN"
expect_line "PASS: listener pod envoy/envoy-listener-7f9c container envoy-listener counted no old-form name between the readings"
expect_line "PASS: listener envoy-hub at https://envoy-hub.example-tailnet.ts.net:9020 counted no old-form name between the readings"
[ "$(jq -r .verdict "$FAKE/record.json")" = complete ] || fail "an open reading's record is $(jq -r .verdict "$FAKE/record.json"), want complete, a first reading for the next window"
done_case

# The time a first reading names must open a clean fleet however old each listener's counter line
# is at the second reading: the first reading read each line at or before its own end, and the
# second takes a line up to three minutes old, so the time named is the window after the first
# reading's end and those three minutes besides. Read at the window alone, a listener whose line
# is 150 s old at the second reading was read less than the window apart, and the gate shut.
new_case "a second reading at the time the first reading names opens a clean fleet, its counter lines however old"
reading
expect_status 4
again=$(sed -n 's/.*read again at or after \([^ ]*\) with --since.*/\1/p' <<<"$output")
again_s=$(jq -rn --arg t "$again" '$t | fromdateiso8601')
now=$(date -u +%s)
jq --argjson s "$((again_s - now))" '.started_at -= $s | .finished_at -= $s | .read_at = (.started_at | todate) | .listeners |= map(.read_at -= $s)' \
  "$FAKE/record.json" >"$FAKE/earlier.json"
rm -f "$FAKE/record.json"
echo "$((now - 150))" >"$FAKE/log_at"
reading --since "$FAKE/earlier.json"
expect_status 0
expect_last "GATE OPEN"
done_case

# --- each reason the gate shuts or cannot say -----------------------------------------------------
new_case "a listener whose count rose between the readings shuts the gate"
first_reading_of 73
metrics 5 1 "$listener_start" >"$FAKE/metrics/envoy.envoy-listener-7f9c"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line 'GATE SHUT: listener pod envoy/envoy-listener-7f9c container envoy-listener counted old-form names since the first reading: {kind="interest"} 4 to 5'
expect_last "GATE SHUT"
done_case

new_case "a role a publisher named in the old form, counted under role_read alone, shuts the gate"
first_reading_of 73
metrics 2 0 "$listener_start" 1 >"$FAKE/metrics/dispatch.dispatch-listener-55d1"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line 'GATE SHUT: listener pod dispatch/dispatch-listener-55d1 container envoy-listener counted old-form names since the first reading: {kind="role_read"} 0 to 1'
expect_no_line '{kind="interest"} 2 to'
expect_last "GATE SHUT"
done_case

new_case "a Legion daemon's consumer still on an old filter shuts the gate"
first_reading_of 73
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers)" "$(consumer legion-go-ZED-dispatch '["notifications.dispatch.issue.ZED-7.>"]')")" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: consumer legion-go-ZED-dispatch filters notifications.dispatch.issue.ZED-7.>, which only an old-form name matches"
expect_no_line "consumer ses_twin"
done_case

# The bus a first reading takes once every producer has moved: the stream still holds old-form
# messages from before the move, and an interest record and a daemon's consumer filter are still in
# an old form. Under the rule that shut a first reading on any of them, no first reading could be
# taken on this bus until the stream's retention had passed and every reader had moved.
new_case "a first reading with old-form messages, interests and consumer filters prints FIRST READING, naming each"
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers)" "$(consumer legion-go-ZED-dispatch '["notifications.dispatch.issue.ZED-7.>"]')")" "")"
replace_row subjects "notifications.dispatch.issue.> 0" \
  "$(line subjects "notifications.dispatch.issue.> 0" 0 "$(subjects_page notifications.dispatch.issue.ACME.ACME-12.comment.created notifications.dispatch.issue.ACME-13.comment.created notifications.dispatch.issue.ZED-7.status.changed)" "")" \
  "$(last_message notifications.dispatch.issue.ACME-13.comment.created 812)" \
  "$(last_message notifications.dispatch.issue.ZED-7.status.changed 640)"
replace_row interest ses_b "$(line interest ses_b 0 "$(interest ses_b notifications.agent.ses_b notifications.role.lead-reviewer)" "")"
reading
expect_status 4
expect_line "FOUND: the stream holds 2 old-form subject(s) under notifications.dispatch.issue.>: notifications.dispatch.issue.ACME-13.comment.created, notifications.dispatch.issue.ZED-7.status.changed"
expect_line "FOUND: the newest old-form message on the stream is notifications.dispatch.issue.ACME-13.comment.created, stream sequence 812 at 2026-11-20T10:00:00Z; the stream's last sequence is 900"
expect_line "FOUND: consumer legion-go-ZED-dispatch filters notifications.dispatch.issue.ZED-7.>, which only an old-form name matches"
expect_line "FOUND: the interest record of session ses_b holds notifications.role.lead-reviewer, in an old form"
expect_line "with 4 FOUND line(s) naming old-form names still in use"
expect_line "none after stream sequence 900"
expect_no_line "GATE SHUT"
[ "$(jq -r '.verdict, .stream.last_seq, .stream.newest_old.seq' "$FAKE/record.json" | paste -sd, -)" = "complete,900,812" ] ||
  fail "the record holds $(jq -c '{verdict, stream}' "$FAKE/record.json"), want a complete reading at last sequence 900 whose newest old-form message is 812"
done_case

new_case "a consumer list that cannot be read is UNKNOWN, naming the refused request"
replace_row consumers 0 "$(line consumers 0 0 "" "$consumer_list_refused"$'\n')"
reading
expect_status 3
expect_line "UNKNOWN: the stream's consumers: the request \$JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS at offset 0 was answered with nothing: $consumer_list_refused"
expect_last "UNKNOWN"
done_case

# The Job's own script, run under sh behind a fake nats that answers as the hub does. The fake
# records every call, refuses the consumer list the way nats-server refuses a publish the grant
# lacks (status 0, no output, the violation on standard error), and answers every other read: the
# agent family lists one old-form subject, whose last message it answers whole, content and
# headers with it, as the server does.
mkdir -p "$root/natsbin"
cat >"$root/natsbin/nats" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$CALLS"
# The credential each call carries, as the nats CLI reads it from its environment: the user, and
# whether a password or a token is set, never their values.
printf 'user=%s password=%s token=%s\n' "${NATS_USER:-}" "${NATS_PASSWORD:+set}" "${NATS_TOKEN:+set}" >>"$CALLS.credentials"
case "$1 $2" in
  "req --raw")
    case "$3" in
      '$JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS')
        if [ -n "${REFUSE_LIST:-}" ]; then
          echo 'nats: permissions violation: Permissions Violation for Publish to "$JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS"' >&2
          exit 0
        fi
        offset=$(jq -r .offset <<<"$4")
        jq -cn --argjson o "$offset" '[range(0; 5) | "c\(.)"] as $all
          | {type: "io.nats.jetstream.api.v1.consumer_list_response", total: 5, offset: $o, limit: 2,
             consumers: [$all[$o:$o + 2][] | {name: ., config: {}}]}' ;;
      '$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS')
        if [ "$(printf '%s' "$4" | jq -r '.subjects_filter // ""' 2>/dev/null)" = 'notifications.agent.>' ]; then
          echo '{"type":"io.nats.jetstream.api.v1.stream_info_response","total":2,"offset":0,"limit":100000,"config":{"name":"ENVOY_NOTIFICATIONS"},"state":{"messages":3,"subjects":{"notifications.agent.ses_old":2,"notifications.agent.ACME.ses_a":1}}}'
        else
          echo '{"type":"io.nats.jetstream.api.v1.stream_info_response","total":0,"offset":0,"limit":0,"config":{"name":"ENVOY_NOTIFICATIONS"},"state":{"messages":0}}'
        fi ;;
      '$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS')
        if [ -n "${FAKE_MSG_RAW:-}" ]; then
          echo 'a private message, as no JSON'
        else
          jq -cn --argjson q "$4" '{type: "io.nats.jetstream.api.v1.stream_msg_get_response", message: {subject: $q.last_by_subj, seq: 77, time: "2026-11-20T10:00:00Z", hdrs: "TkFUUy8xLjANCg0K", data: "eyJtZXNzYWdlIjoiYSBwcml2YXRlIG1lc3NhZ2UifQ=="}}'
        fi ;;
      '$SYS.REQ.USER.INFO')
        echo 'nats: permissions violation: Permissions Violation for Publish to "$SYS.REQ.USER.INFO"' >&2 ;;
      *) echo "fake nats: no answer for $*" >&2; exit 99 ;;
    esac ;;
  "kv ls") echo ses_a ;;
  "kv get") echo '{"session_id":"ses_a","machine_id":"gke","dir":"/home/someone/private-work","title":"a title","topics":["notifications.agent.ses_a"]}' ;;
  *) echo "fake nats: no answer for $*" >&2; exit 99 ;;
esac
EOF
chmod +x "$root/natsbin/nats"
# A fake nsc that fails as nsc can, after echoing what it read: the seed in the key directory the
# Job's script gave it.
cat >"$root/natsbin/nsc" <<'EOF'
#!/usr/bin/env bash
dir=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --dir) dir=$2; shift 2 ;;
    *) shift ;;
  esac
done
cat "$dir"/* 2>/dev/null
echo "nsc: could not import the keys above"
exit 1
EOF
chmod +x "$root/natsbin/nsc"

# run_reads [ENV...] runs lib/legacy-subjects-gate-nats.sh as the gate's Job runs it (sh -c with the
# script's text), with the fake nats first on PATH, for at most 20 seconds, keeping its output in
# $FAKE/reads.out, its calls in $FAKE/reads.calls and its exit status in $status.
run_reads() {
  : >"$FAKE/reads.calls"
  : >"$FAKE/reads.calls.credentials"
  set +e
  env -u NATS_USER -u NATS_PASSWORD -u NATS_TOKEN "$@" FORMS_JQ="$(cat "$here/lib/legacy-subjects-gate-forms.jq")" CALLS="$FAKE/reads.calls" WORK_DIR="$FAKE" PATH="$root/natsbin:$PATH" timeout 20 sh -c "$(cat "$here/lib/legacy-subjects-gate-nats.sh")" >"$FAKE/reads.out" 2>"$FAKE/reads.err"
  status=$?
  set -e
}

# Taken in from Tester4688's red test (nats-reads-end-on-an-empty-answer.test.sh), its assertions
# unchanged: a refused list is read once and the reads go on to their end line, where the script
# once asked for it again until the Job's deadline; and the gate names the request refused.
new_case "the NATS reads end on a refused consumer list, and the gate names the refused request"
run_reads REFUSE_LIST=1
asks=$(grep -c 'CONSUMER.LIST' "$FAKE/reads.calls" || true)
if [ "$status" = 124 ]; then
  fail "the NATS reads did not end within 20 s after a refused consumer list: they asked for it $asks times and printed $(wc -l <"$FAKE/reads.out") lines"
fi
[ "$(tail -n 1 "$FAKE/reads.out" | cut -f1)" = end ] || fail "the NATS reads exited $status without their end line"
[ "$asks" = 1 ] || fail "a refused consumer list was asked for $asks times, want once"
cp "$FAKE/reads.out" "$FAKE/job.log"
reading
expect_status 3
expect_line "UNKNOWN: the stream's consumers: the request \$JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS at offset 0 was answered with nothing: nats: permissions violation"
done_case

new_case "the NATS reads page a list from the last item of the page before, print a record's session and topics and a message's sequence alone, and pass the inbox prefix to every call"
run_reads NATS_INBOX_PREFIX=_INBOX_envoy_listener_gke
[ "$status" = 0 ] || fail "the NATS reads exited $status: $(cat "$FAKE/reads.err")"
offsets=$(awk -F'\t' '$1 == "consumers" {print $2}' "$FAKE/reads.out" | paste -sd, -)
[ "$offsets" = "0,1,2,3" ] || fail "the consumer list was read at offsets $offsets, want 0,1,2,3: each page from the last item of the one before"
record=$(awk -F'\t' '$1 == "interest" {print $4}' "$FAKE/reads.out" | base64 -d)
[ "$record" = '{"session_id":"ses_a","topics":["notifications.agent.ses_a"]}' ] || fail "the Job printed the record $record, want its session id and topics alone"
old=$(awk -F'\t' '$1 == "old_last" {print $2 "|" $4}' "$FAKE/reads.out")
[ "${old%%|*}" = notifications.agent.ses_old ] && [ "$(wc -l <<<"$old")" = 1 ] ||
  fail "the Job read the last message of $old, want of notifications.agent.ses_old alone, the one old-form subject listed"
message=$(base64 -d <<<"${old#*|}")
[ "$message" = '{"type":"io.nats.jetstream.api.v1.stream_msg_get_response","error":null,"message":{"subject":"notifications.agent.ses_old","seq":77,"time":"2026-11-20T10:00:00Z"}}' ] ||
  fail "the Job printed the message $message, want its subject, sequence and time alone"
while IFS=$'\t' read -r _ _ _ out _; do printf '%s' "$out" | base64 -d; echo; done <"$FAKE/reads.out" >"$FAKE/reads.decoded"
! grep -q -- "private-work" "$FAKE/reads.decoded" || fail "the Job's log holds the record's directory"
! grep -q -- "eyJtZXNzYWdl\|TkFUUy8x" "$FAKE/reads.decoded" || fail "the Job's log holds a message's content or headers"
[ "$(grep -c -v -- '--inbox-prefix _INBOX_envoy_listener_gke' "$FAKE/reads.calls")" = 0 ] || fail "a nats call took no inbox prefix: $(grep -v -- '--inbox-prefix' "$FAKE/reads.calls" | head -n 1)"
[ "$(awk -F'\t' '$1 == "old_last_skipped" {print $2 "|" $4}' "$FAKE/reads.out")" = "1000|$(printf 0 | base64 -w 0)" ] ||
  fail "the Job printed no count of 0 old-form subjects skipped under its cap of 1000"
done_case

# The last-message reads decide no verdict, so the Job reads at most OLD_LAST_MAX of them and counts
# the rest: a stream full of old-form subjects cannot push the Job past its deadline.
new_case "the NATS reads read at most OLD_LAST_MAX last messages and count the rest"
run_reads OLD_LAST_MAX=0
[ "$status" = 0 ] || fail "the NATS reads exited $status: $(cat "$FAKE/reads.err")"
[ "$(grep -c -- 'MSG.GET' "$FAKE/reads.calls")" = 0 ] || fail "the Job asked for $(grep -c -- 'MSG.GET' "$FAKE/reads.calls") last messages under a cap of 0"
[ "$(awk -F'\t' '$1 == "old_last_skipped" {print $2 "|" $4}' "$FAKE/reads.out")" = "0|$(printf 1 | base64 -w 0)" ] ||
  fail "the Job printed no count of the 1 old-form subject it did not read: $(awk -F'\t' '$1 == "old_last_skipped"' "$FAKE/reads.out")"
[ "$(tail -n 1 "$FAKE/reads.out" | cut -f1)" = end ] || fail "the NATS reads did not reach their end line"
done_case

# A last message's answer that is no JSON object reaches the log as no answer, never as it came, so
# the log holds no message's content however the server answers.
new_case "a last-message answer that is no JSON object is logged as no answer"
run_reads FAKE_MSG_RAW=1
[ "$status" = 0 ] || fail "the NATS reads exited $status: $(cat "$FAKE/reads.err")"
[ "$(awk -F'\t' '$1 == "old_last" {print $4}' "$FAKE/reads.out")" = "" ] ||
  fail "the Job logged the answer $(awk -F'\t' '$1 == "old_last" {print $4}' "$FAKE/reads.out" | base64 -d), want nothing"
while IFS=$'\t' read -r _ _ _ out _; do printf '%s' "$out" | base64 -d; echo; done <"$FAKE/reads.out" >"$FAKE/reads.decoded"
! grep -q -- "a private message" "$FAKE/reads.decoded" || fail "the Job's log holds a message's content"
done_case

# A hub behind the auth service admits a service by CONNECT's user `service` and the token as its
# password (natscred.Service; natscallout's credentialKind), and reads a bare token as no
# credential at all.
new_case "with a projected token the NATS reads connect as the auth service's service user"
printf '%s' "eyJhbGciOiJSUzI1NiJ9.$(printf '{"sub":"system:serviceaccount:envoy:envoy-listener","aud":["nats"]}' | base64 -w 0 | tr '+/' '-_' | tr -d '=').c2ln" >"$FAKE/token"
run_reads NATS_TOKEN_FILE="$FAKE/token"
[ "$status" = 0 ] || fail "the NATS reads exited $status: $(cat "$FAKE/reads.err")"
shapes=$(sort -u "$FAKE/reads.calls.credentials")
[ "$shapes" = "user=service password=set token=" ] ||
  fail "the nats calls carried the credentials $(paste -sd'|' <<<"$shapes"), want user service with the token as its password on every call"
credential=$(awk -F'\t' '$1 == "credential" {print $2 "|" $3 "|" $4}' "$FAKE/reads.out")
[ "${credential%%|*}" = token ] && [ "$(cut -d'|' -f3 <<<"$credential" | base64 -d)" = '{"sub":"system:serviceaccount:envoy:envoy-listener","aud":["nats"],"iss":null,"exp":null}' ] ||
  fail "the Job named its credential $credential, want the token's claims"
done_case

new_case "an old-form message published after the first reading shuts the second"
first_reading_of 73
replace_row subjects "notifications.dispatch.issue.> 0" \
  "$(line subjects "notifications.dispatch.issue.> 0" 0 "$(subjects_page notifications.dispatch.issue.ACME.ACME-12.comment.created notifications.dispatch.issue.ACME-13.comment.created)" "")" \
  "$(last_message notifications.dispatch.issue.ACME-13.comment.created 950)"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: the stream holds 1 old-form subject(s) under notifications.dispatch.issue.> with a message published after the first reading's last stream sequence 900: notifications.dispatch.issue.ACME-13.comment.created (sequence 950 at 2026-11-20T10:00:00Z)"
expect_no_line "GATE SHUT: the stream still holds"
[ "$(jq -c .subjects_old "$FAKE/record.json")" = '["notifications.dispatch.issue.ACME-13.comment.created"]' ] ||
  fail "the record names $(jq -c .subjects_old "$FAKE/record.json") as old subjects"
done_case

# The second reading opens only on a stream that holds no old-form message at all: with the stream
# keeping the window, a clean second reading says the stream is wholly new-form when the code that
# reads the old form goes. An old-form message still stored at the second reading shuts it, though
# it came before the first reading's last sequence.
new_case "a stream still holding an old-form subject at the second reading shuts it, though its message came before the first reading"
first_reading_of 73
replace_row subjects "notifications.dispatch.issue.> 0" \
  "$(line subjects "notifications.dispatch.issue.> 0" 0 "$(subjects_page notifications.dispatch.issue.ACME.ACME-12.comment.created notifications.dispatch.issue.ACME-13.comment.created)" "")" \
  "$(last_message notifications.dispatch.issue.ACME-13.comment.created 850)"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: the stream still holds 1 old-form subject(s) under notifications.dispatch.issue.> at this second reading, none read as published after the first reading's last stream sequence 900: notifications.dispatch.issue.ACME-13.comment.created (sequence 850 at 2026-11-20T10:00:00Z)"
expect_no_line "with a message published after the first reading's last stream sequence"
expect_last "GATE SHUT"
done_case

# The last-message reads name what a reading found and decide nothing: a first reading whose
# old-form subjects' last messages were not read (beyond the Job's cap, or a read that failed) is a
# first reading that says so, never UNKNOWN; a second reading shuts on such a subject all the same.
new_case "an old-form subject whose last message was not read is named, at a first reading and a second"
replace_row subjects "notifications.dispatch.issue.> 0" \
  "$(line subjects "notifications.dispatch.issue.> 0" 0 "$(subjects_page notifications.dispatch.issue.ACME.ACME-12.comment.created notifications.dispatch.issue.ACME-13.comment.created notifications.dispatch.issue.ZED-7.comment.created)" "")" \
  "$(last_message notifications.dispatch.issue.ACME-13.comment.created 812)" \
  "$(line old_last notifications.dispatch.issue.ZED-7.comment.created 0 "" "nats: timeout")"
replace_row old_last_skipped 1000 "$(line old_last_skipped 1000 0 "2" "")"
reading
expect_status 4
expect_line "FOUND: the newest old-form message on the stream is notifications.dispatch.issue.ACME-13.comment.created, stream sequence 812 at 2026-11-20T10:00:00Z; the stream's last sequence is 900; the last message of 3 old-form subject(s) was not read (2 beyond the 1000 the Job reads, 1 whose read failed), so the newest named is the newest of those read"
expect_no_line "UNKNOWN"
jq '.started_at -= 262800 | .finished_at -= 262800 | .read_at = (.started_at | todate) | .listeners |= map(.read_at -= 262800)' "$FAKE/record.json" >"$FAKE/earlier.json"
rm -f "$FAKE/record.json"
cp "$base/job.log" "$FAKE/job.log"
replace_row subjects "notifications.dispatch.issue.> 0" \
  "$(line subjects "notifications.dispatch.issue.> 0" 0 "$(subjects_page notifications.dispatch.issue.ACME.ACME-12.comment.created notifications.dispatch.issue.ZED-7.comment.created)" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: the stream still holds 1 old-form subject(s) under notifications.dispatch.issue.> at this second reading, none read as published after the first reading's last stream sequence 900: notifications.dispatch.issue.ZED-7.comment.created (its last message not read)"
done_case

new_case "an interest record holding an old-form topic at the second reading shuts the gate; its own agent subject does not"
first_reading_of 73
replace_row interest ses_a "$(line interest ses_a 0 "$(interest ses_a notifications.agent.ses_a notifications.role.lead-reviewer)" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: the interest record of session ses_a holds notifications.role.lead-reviewer, in an old form"
expect_no_line "notifications.agent.ses_a, in an old form"
done_case

# An issue filter with a wildcard where the old form names the key, of the old form's fixed length,
# hears old-form names alone (notifications.dispatch.issue.*.comment.created: the new form of
# comment.created has one token more), so a reader on it goes deaf when Dispatch publishes the new
# form. As a consumer's filter it shuts the second reading unless the consumer also filters its
# new form; as a stored interest it shuts it too.
new_case "a consumer on an old-only wildcard issue filter shuts the second reading; one that also filters its new form does not"
first_reading_of 73
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers)" \
  "$(consumer ses_wild '["notifications.dispatch.issue.*.comment.created"]')" \
  "$(consumer ses_both '["notifications.dispatch.issue.*.comment.created","notifications.dispatch.issue.*.*.comment.created"]')")" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: consumer ses_wild filters notifications.dispatch.issue.*.comment.created, which only an old-form name matches"
expect_no_line "consumer ses_both"
expect_last "GATE SHUT"
done_case

new_case "an interest record holding an old-only wildcard issue filter shuts the second reading"
first_reading_of 73
replace_row interest ses_a "$(line interest ses_a 0 "$(interest ses_a notifications.agent.ses_a 'notifications.dispatch.issue.*.comment.created')" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: the interest record of session ses_a holds notifications.dispatch.issue.*.comment.created, in an old form"
expect_last "GATE SHUT"
done_case

# A filter reaching the issue family through a wildcard in the family's own tokens
# (notifications.dispatch.*.*.comment.created) is an issue filter all the same, and old-only for the
# same reason, so it shuts the second reading as a stored interest and as a consumer's filter.
new_case "an interest record holding an old-only filter that reaches the issue family through a wildcard shuts the second reading"
first_reading_of 73
replace_row interest ses_a "$(line interest ses_a 0 "$(interest ses_a notifications.agent.ses_a 'notifications.dispatch.*.*.comment.created')" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: the interest record of session ses_a holds notifications.dispatch.*.*.comment.created, in an old form"
expect_last "GATE SHUT"
done_case

new_case "a consumer on an old-only filter that reaches the issue family through a wildcard shuts the second reading"
first_reading_of 73
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers)" "$(consumer ses_fam '["notifications.dispatch.*.*.comment.created"]')")" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: consumer ses_fam filters notifications.dispatch.*.*.comment.created, which only an old-form name matches"
expect_last "GATE SHUT"
done_case

# A twin covers the old filter only when it covers every project the old filter's names carry:
# beside notifications.dispatch.issue.*.comment.created, whose names carry any project,
# notifications.dispatch.issue.ACME.*.comment.created hears ACME's alone, so the consumer still
# goes deaf to every other project's events when Dispatch publishes the new form.
new_case "a consumer whose new-form filter covers one project of an old-only filter's names shuts the second reading"
first_reading_of 73
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers)" \
  "$(consumer ses_one '["notifications.dispatch.issue.*.comment.created","notifications.dispatch.issue.ACME.*.comment.created"]')")" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: consumer ses_one filters notifications.dispatch.issue.*.comment.created, which only an old-form name matches"
expect_last "GATE SHUT"
done_case

new_case "a listener whose log cannot be read is UNKNOWN"
rm "$FAKE/metrics/dispatch.dispatch-listener-55d1"
reading
expect_status 3
expect_line "UNKNOWN: listener pod dispatch/dispatch-listener-55d1 container envoy-listener: its log could not be read: Error from server (Forbidden)"
done_case

new_case "a listener whose newest counter line is older than three minutes is UNKNOWN"
echo "$(($(date -u +%s) - 600))" >"$FAKE/log_at"
reading
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-listener-7f9c container envoy-listener: its newest \"listener legacy subjects\" log line was logged at"
expect_line "more than 3 minutes before this reading began"
done_case

new_case "a restart only the start-time gauge shows is caught through the gauge"
first_reading_of 73
# Same pod, same restart count; the process started again, so the counter began from nothing.
metrics 0 0 "$((listener_start + 100000))" >"$FAKE/metrics/envoy.envoy-listener-7f9c"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-listener-7f9c container envoy-listener restarted between the readings: its start-time gauge read $listener_start then $((listener_start + 100000))"
expect_no_line "restart count"
expect_no_line "count went down"
done_case

new_case "a restart the pod's restart count shows is caught through it"
first_reading_of 73
jq '(.items[] | select(.metadata.name == "envoy-listener-7f9c") | .status.containerStatuses[0].restartCount) = 1' "$base/pods.json" >"$FAKE/pods.json"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "restarted between the readings (restart count 0 then 1)"
done_case

# A listener's start-time gauge names its machine (label machine). An outside listener is named by
# the operator (--listener <machine>=<url>), and only its gauge shows the URL serves that machine:
# one that names none, or names another, leaves the named machine's counter unread.
new_case "a listener outside the cluster that names no machine on its start-time gauge is UNKNOWN"
metrics 7 0 >"$FAKE/outside/envoy-hub.example-tailnet.ts.net"
reading
expect_status 3
expect_line "UNKNOWN: listener envoy-hub at https://envoy-hub.example-tailnet.ts.net:9020 names no machine on its start-time gauge (label machine), so its URL is not shown to serve machine envoy-hub"
done_case

new_case "a listener outside the cluster whose start-time gauge names another machine is UNKNOWN, and its durable stays unread"
metrics 7 0 "$hub_start" 0 laptop >"$FAKE/outside/envoy-hub.example-tailnet.ts.net"
reading
expect_status 3
expect_line "UNKNOWN: listener envoy-hub at https://envoy-hub.example-tailnet.ts.net:9020 serves machine laptop, not envoy-hub: it reads another listener than the one named"
expect_line "UNKNOWN: listener machine envoy-hub is bound to its durable listener-envoy-hub on the bus, and no listener this reading read serves as that machine"
done_case

new_case "a listener pod whose start-time gauge names a machine other than its ENVOY_MACHINE_ID is UNKNOWN"
metrics 2 0 "$listener_start" 0 gke >"$FAKE/metrics/dispatch.dispatch-listener-55d1"
reading
expect_status 3
expect_line "UNKNOWN: listener pod dispatch/dispatch-listener-55d1 container envoy-listener serves machine gke, not dispatch: it reads another listener than the one named"
done_case

new_case "a listener bound on the bus that nobody named is UNKNOWN"
run_gate --context example-cluster --record "$FAKE/record.json"
expect_status 3
expect_line "UNKNOWN: listener machine envoy-hub is bound to its durable listener-envoy-hub on the bus, and no listener this reading read serves as that machine: name it with --listener envoy-hub=<url>"
done_case

new_case "a listener offline at the reading, its durable unbound, that nobody named is UNKNOWN"
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers)" "$(consumer listener-laptop '["notifications.agent.>","notifications.dispatch.>"]' false "$durable_lapse_ns")")" "")"
reading
expect_status 3
expect_line "UNKNOWN: listener machine laptop has its durable listener-laptop on the bus and is not bound to it now (offline, asleep or restarting; last active 2026-11-20T23:59:00Z)"
done_case

new_case "readings further apart than a listener's durable outlives its listener are UNKNOWN"
first_reading_of 170
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "h passed between the readings, longer than a listener's durable outlives its listener (168 h unbound, its inactive_threshold)"
done_case

new_case "a listener of the first reading that is gone is UNKNOWN"
first_reading_of 73
jq '.items[0].metadata.uid = "envoy-uid-2" | .items[0].metadata.name = "envoy-listener-8a01"' "$base/pods.json" >"$FAKE/pods.json"
cp "$FAKE/metrics/envoy.envoy-listener-7f9c" "$FAKE/metrics/envoy.envoy-listener-8a01"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-listener-8a01 container envoy-listener was not read at the first reading"
expect_line "UNKNOWN: listener pod envoy/envoy-listener-7f9c container envoy-listener served at the first reading and was not read now"
done_case

new_case "a second reading sooner than the stream's retention shuts the gate"
first_reading_of 10
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "h passed between the first reading's end"
expect_line "the stream keeps 72 h, so read again at or after"
done_case

new_case "a listener whose counter was read less than the window apart shuts the gate"
first_reading_of 73
# The listener's newest line at the second reading is a minute old, and its line at the first
# reading 71.83 hours before it: the readings are 73 hours apart, the listener's own two reads not.
log_at=$(($(date -u +%s) - 60))
echo "$log_at" >"$FAKE/log_at"
jq --argjson read "$((log_at - 259200 + 600))" '.listeners |= map(if .uid == "envoy-uid-1" then .read_at = $read else . end)' "$FAKE/earlier.json" >"$FAKE/earlier.tmp"
mv "$FAKE/earlier.tmp" "$FAKE/earlier.json"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: listener pod envoy/envoy-listener-7f9c container envoy-listener's counter was read"
expect_line "less than the 72 h window"
done_case

new_case "a stream made anew between the readings is UNKNOWN"
first_reading_of 73
replace_row stream_info - "$(line stream_info - 0 "$(stream_info "" stream-id-2)" "")"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: the stream is not the one the first reading read: its envoy_stream_id was stream-id-1 and is stream-id-2"
done_case

new_case "a stream that holds less than the window at the second reading is UNKNOWN"
first_reading_of 73
replace_row stream_info - "$(line stream_info - 0 "$(stream_info "$(jq -rn 'now - 36000 | floor | todate')")" "")"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: the stream holds messages only since"
expect_line "not the 72 h it keeps: it was purged or made anew within the window"
done_case

new_case "a stream whose configuration carries no identity is UNKNOWN at a first reading"
replace_row stream_info - "$(line stream_info - 0 "$(stream_info "" "")" "")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's info: its configuration carries no metadata envoy_stream_id"
done_case

new_case "a reading that shut the gate is no first reading"
first_reading_of 146
replace_row interest ses_a "$(line interest ses_a 0 "$(interest ses_a notifications.agent.ses_a notifications.role.lead-reviewer)" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
jq '.started_at -= 262800 | .finished_at -= 262800 | .listeners |= map(.read_at -= 262800)' "$FAKE/record.json" >"$FAKE/shut.json"
rm -f "$FAKE/record.json"
cp "$base/job.log" "$FAKE/job.log"
reading --since "$FAKE/shut.json"
expect_status 1
expect_line "GATE SHUT: the reading named with --since, taken"
expect_line "shut the gate, so it is no first reading"
done_case

new_case "a listener that is not running is UNKNOWN"
jq '(.items[] | select(.metadata.name == "dispatch-listener-55d1") | .status.containerStatuses[0].state) = {waiting: {reason: "CrashLoopBackOff"}}' "$base/pods.json" >"$FAKE/pods.json"
reading
expect_status 3
expect_line "UNKNOWN: listener pod dispatch/dispatch-listener-55d1 container envoy-listener is waiting, not running"
done_case

new_case "a listener build with no old-form counter is UNKNOWN"
grep -v legacy_subjects "$base/metrics/dispatch.dispatch-listener-55d1" >"$FAKE/metrics/dispatch.dispatch-listener-55d1"
reading
expect_status 3
expect_line "UNKNOWN: listener pod dispatch/dispatch-listener-55d1 container envoy-listener serves no envoy_listener_legacy_subjects_total"
done_case

new_case "no listener found in the cluster is UNKNOWN, not a clean fleet"
jq '.items |= map(select(.spec.containers[0].command != null))' "$base/pods.json" >"$FAKE/pods.json"
reading
expect_status 3
expect_line "UNKNOWN: no listener container runs in any namespace of the cluster"
done_case

new_case "pods that cannot be listed are UNKNOWN"
touch "$FAKE/pods.fail"
reading
expect_status 3
expect_line "UNKNOWN: the listeners in the cluster: the cluster's pods could not be listed: error: You must be logged in to the server (Unauthorized)"
done_case

new_case "no Legion daemon consumer on the stream is UNKNOWN"
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers | grep -v legion-go-ACME-dispatch)")" "")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's consumers: no Legion daemon consumer (legion-go-<project>-dispatch) is on the stream"
done_case

# Consumer pages, as the Job reads a list longer than one answer: each page from the last item of
# the page before. pages_of TOTAL OFFSET:NAME,NAME... prints one consumers row per page.
pages_of() {
  local total=$1 page offset names
  shift
  for page in "$@"; do
    offset=${page%%:*}
    names=${page#*:}
    line consumers "$offset" 0 "$(jq -cn --argjson total "$total" --argjson offset "$offset" --arg names "$names" --slurpfile all <(clean_consumers) '
      ($names | split(",")) as $n
      | {type: "io.nats.jetstream.api.v1.consumer_list_response", total: $total, offset: $offset, limit: 256,
         consumers: [$n[] | . as $name | (($all | map(select(.name == $name)) | first) // {name: $name, config: {filter_subject: "notifications.team.\($name)"}})]}')" ""
  done
}

new_case "a consumer list read over pages that join is read whole"
replace_row consumers 0 "$(pages_of 7 "0:legion-go-ACME-dispatch,legion-go-ACME-github,legion-go-ACME-wake,listener-dispatch" "3:listener-dispatch,listener-envoy-hub,listener-gke,ses_twin")"
reading
expect_status 4
expect_line "PASS: no consumer of the stream's 7 filters a name only the old form matches"
done_case

new_case "consumer pages that repeat a consumer, reading fewer than the server counts, are UNKNOWN"
replace_row consumers 0 "$(pages_of 8 "0:legion-go-ACME-dispatch,legion-go-ACME-github,legion-go-ACME-wake,listener-dispatch" "3:listener-dispatch,listener-envoy-hub,listener-gke,ses_twin,ses_twin")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's consumers: the list read 7 distinct of the 8 the server counts"
done_case

new_case "a consumer page that does not begin with the last of the page before is UNKNOWN"
# A consumer before the boundary went between the two reads and another came after it: the count
# holds, the second page starts one further on, and listener-dispatch's successor was never read.
replace_row consumers 0 "$(pages_of 7 "0:legion-go-ACME-dispatch,legion-go-ACME-github,legion-go-ACME-wake,listener-dispatch" "3:listener-envoy-hub,listener-gke,ses_twin,ses_zz")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's consumers: the page at offset 3 begins with listener-envoy-hub, not listener-dispatch, the last of the page before"
done_case

new_case "consumer pages whose counts differ are UNKNOWN"
replace_row consumers 0 "$(pages_of 7 "0:legion-go-ACME-dispatch,legion-go-ACME-github,legion-go-ACME-wake,listener-dispatch")" "$(pages_of 8 "3:listener-dispatch,listener-envoy-hub,listener-gke,ses_twin,ses_zz")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's consumers: the server's count changed between its pages (7, 8)"
done_case

new_case "subject pages whose counts differ are UNKNOWN"
replace_row subjects "notifications.dispatch.issue.> 0" \
  "$(line subjects "notifications.dispatch.issue.> 0" 0 "$(clean_issue_subjects | jq -c '.total = 3 | .limit = 2')" "")" \
  "$(line subjects "notifications.dispatch.issue.> 1" 0 "$(subjects_page notifications.dispatch.issue.ACME.ACME-13.status.changed notifications.dispatch.issue.ACME.ACME-14.status.changed | jq -c '.total = 4 | .offset = 1')" "")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's subjects under notifications.dispatch.issue.>: the server's count changed between its pages (3, 4)"
done_case

new_case "an answer that is a JSON value but no object is UNKNOWN, with its verdict and a record"
replace_row stream_info - "$(line stream_info - 0 "42" "")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's info: the request \$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS: the answer is a JSON number, not an object"
expect_last "UNKNOWN"
[ "$(jq -r .verdict "$FAKE/record.json" 2>/dev/null)" = unknown ] || fail "no record of verdict unknown was written"
done_case

new_case "an answer carrying the server's error is UNKNOWN, naming the error"
replace_row stream_info - "$(line stream_info - 0 '{"type":"io.nats.jetstream.api.v1.stream_info_response","error":{"code":404,"err_code":10059,"description":"stream not found"}}' "")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's info: the request \$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS: the server answered 404: stream not found"
done_case

# A consumer list answer that carries the server's error is no list, whatever else it holds: read
# as one, it would be an empty list. jq binds a function parameter's name as a filter too, so a
# judge whose parameter were named `type` would read `.error | type` as that parameter and take
# this answer for a list (jq 1.7.1, as CI runs it; jaq does not bind it so).
new_case "a consumer list answer carrying the server's error is UNKNOWN, naming the error"
replace_row consumers 0 "$(line consumers 0 0 '{"type":"io.nats.jetstream.api.v1.consumer_list_response","total":0,"offset":0,"limit":256,"consumers":[],"error":{"code":503,"err_code":10039,"description":"jetstream not enabled for account"}}' "")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's consumers: the request \$JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS at offset 0: the server answered 503: jetstream not enabled for account"
done_case

new_case "a judgement that fails is UNKNOWN, with its verdict and a record"
mkdir -p "$root/broken/lib"
cp "$gate" "$root/broken/"
cp "$here"/lib/legacy-subjects-gate-*.* "$root/broken/lib/"
echo 'error("the judge broke")' >"$root/broken/lib/legacy-subjects-gate-judge.jq"
set +e
output=$(PATH="$root/bin:$PATH" bash "$root/broken/legacy-subjects-gate.sh" --context example-cluster --record "$FAKE/record.json" --listener envoy-hub=https://envoy-hub.example-tailnet.ts.net:9020 2>&1)
status=$?
set -e
expect_status 3
expect_line "UNKNOWN: the judgement could not read what the gate read: jq: error"
expect_last "UNKNOWN"
[ "$(jq -r .verdict "$FAKE/record.json" 2>/dev/null)" = unknown ] || fail "no record of verdict unknown was written"
done_case

new_case "a stream with no issue subject at all is UNKNOWN"
replace_row subjects "notifications.dispatch.issue.> 0" "$(line subjects "notifications.dispatch.issue.> 0" 0 "$(subjects_page)" "")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's subjects under notifications.dispatch.issue.>: the stream holds none"
done_case

new_case "an empty interest store is UNKNOWN"
replace_row interest_keys - "$(line interest_keys - 0 "" "")"
replace_row interest ses_a
replace_row interest ses_b
reading
expect_status 3
expect_line "UNKNOWN: the interest store lists no record"
done_case

new_case "a record listed and not read is UNKNOWN; one deleted since the list is not"
replace_row interest_keys - "$(line interest_keys - 0 $'ses_a\nses_b\nses_gone\nses_unread\n' "")"
replace_row interest ses_b "$(line interest ses_b 0 "$(interest ses_b notifications.agent.ses_b notifications.team.deploy-squad)" "")" "$(line interest ses_gone 1 "" "nats: error: nats: key not found")"
reading
expect_status 3
expect_line "UNKNOWN: the interest store's record ses_unread was listed and not read"
expect_no_line "ses_gone"
done_case

new_case "a Job log cut short is UNKNOWN"
# The `end` line's place is what this case is about, so it cuts the log's last line.
sed '$d' "$base/job.log" >"$FAKE/job.log"
reading
expect_status 3
expect_line "UNKNOWN: the NATS reads: the Job's log ends before its last read"
done_case

new_case "a Job that failed is UNKNOWN"
echo '{"status":{"failed":1}}' >"$FAKE/job-status.json"
reading
expect_status 3
expect_line "UNKNOWN: the NATS reads: the Job failed"
done_case

new_case "a listener Deployment with no NATS seed file creates no Job and is UNKNOWN"
jq '.spec.template.spec.containers[0].env |= map(select(.name != "NATS_NKEY_SEED_FILE"))' "$base/deployment.json" >"$FAKE/deployment.json"
reading
expect_status 3
expect_line "UNKNOWN: the NATS reads: the listener Deployment envoy/envoy-listener lacks a NATS seed file (NATS_NKEY_SEED_FILE): name the listener's projected NATS token with --token-file once the hub is not static"
! grep -qF -- "create -f" "$FAKE/calls" || fail "a Job was created"
done_case

new_case "a Job that is not shown deleted is UNKNOWN and named"
touch "$FAKE/job.stays"
reading
expect_status 3
expect_line "was not shown deleted; delete it: kubectl --context example-cluster -n envoy delete job legacy-subjects-gate-"
expect_last "UNKNOWN"
done_case

# --- the operator's environment -------------------------------------------------------------------
# A uid with no passwd entry (a container run as an arbitrary uid) has no name for `id -un`: the
# reading goes on with the Job's owner `unknown`, and ends in a verdict and a record.
mkdir -p "$root/idbin"
cat >"$root/idbin/id" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = -un ]; then
  echo "id: cannot find name for user ID 4242" >&2
  exit 1
fi
exec /usr/bin/id "$@"
EOF
chmod +x "$root/idbin/id"

new_case "a uid with no name reads all the same, its Job's owner unknown"
set +e
output=$(PATH="$root/idbin:$root/bin:$PATH" bash "$gate" --context example-cluster --record "$FAKE/record.json" --listener envoy-hub=https://envoy-hub.example-tailnet.ts.net:9020 2>&1)
status=$?
set -e
expect_status 4
[ "$(jq -r .metadata.labels.owner "$FAKE/created-job.json" 2>/dev/null)" = unknown ] || fail "the Job's owner label is $(jq -r .metadata.labels.owner "$FAKE/created-job.json" 2>/dev/null), want unknown"
[ "$(jq -r .verdict "$FAKE/record.json" 2>/dev/null)" = complete ] || fail "no complete record was written"
done_case

# The gate deletes only a Job it made: one whose create was refused is never reported deleted.
new_case "a Job that could not be created is named so, and never reported deleted"
touch "$FAKE/create.fail"
reading
expect_status 3
expect_line "UNKNOWN: the NATS reads: the Job could not be created: Error from server (Forbidden): jobs.batch is forbidden"
expect_no_line "JOB: deleted"
expect_no_line "was not shown deleted"
! grep -qF -- "delete job" "$FAKE/calls" || fail "the gate deleted a Job it never made"
done_case

# The record is a new file: one made at its path while the gate ran is left as it is, and the
# reading, which cannot be kept, is UNKNOWN.
new_case "a record made at --record while the gate runs is left as it is, and the reading is UNKNOWN"
echo "$FAKE/record.json" >"$FAKE/record.racer"
reading
expect_status 3
expect_line "UNKNOWN: the record $FAKE/record.json could not be written, so this reading cannot be compared"
expect_last "UNKNOWN"
[ "$(jq -r .made "$FAKE/record.json" 2>/dev/null)" = "while the gate ran" ] || fail "the record made meanwhile was overwritten: $(cat "$FAKE/record.json")"
done_case

# A listener container on another bus than the listener Deployment's (a load-test stack on its own
# NATS) is no listener of the bus the reading is about: named, and not read.
new_case "a listener on another bus than the listener Deployment's is named and not read"
jq --argjson p "$(pod loadtest envoy-listener-lt1 lt-uid-1 envoy-listener "$envoy_image" null 0 running lt-1 nats://loadtest-nats.loadtest.svc.cluster.local:4222)" \
  '.items += [$p]' "$base/pods.json" >"$FAKE/pods.json"
reading
expect_status 4
expect_line "SKIP: listener pod loadtest/envoy-listener-lt1 container envoy-listener is on another bus (NATS_URLS nats://loadtest-nats.loadtest.svc.cluster.local:4222), not the listener Deployment's $bus_url, and is not read"
! grep -qF -- "-n loadtest logs" "$FAKE/calls" || fail "the listener on another bus was read"
done_case

# nsc's output on a failure may echo the seed it read: the Job logs a fixed line instead.
new_case "a failed nkey read logs a fixed line, never nsc's own output"
printf 'SUFAKESEEDFORTHEGATETESTONLY\n' >"$FAKE/seed"
run_reads NATS_NKEY="$FAKE/seed"
[ "$status" = 0 ] || fail "the NATS reads exited $status: $(cat "$FAKE/reads.err")"
[ "$(awk -F'\t' '$1 == "credential" {print $3}' "$FAKE/reads.out")" = 1 ] || fail "the credential read did not exit 1"
[ "$(awk -F'\t' '$1 == "credential" {print $5}' "$FAKE/reads.out" | base64 -d)" = "the nkey seed's public key could not be read (exit 1)" ] ||
  fail "the credential read's error is $(awk -F'\t' '$1 == "credential" {print $5}' "$FAKE/reads.out" | base64 -d), want the fixed line"
while IFS=$'\t' read -r _ _ _ out err; do { printf '%s' "$out" | base64 -d; echo; printf '%s' "$err" | base64 -d; echo; } 2>/dev/null; done <"$FAKE/reads.out" >"$FAKE/reads.decoded"
! grep -q -- "SUFAKESEED" "$FAKE/reads.decoded" || fail "the Job's log holds the seed"
done_case

# --- each UNKNOWN rule of the judge, on its own ---------------------------------------------------
new_case "subjects read fewer than the server counts are UNKNOWN"
replace_row subjects "notifications.dispatch.issue.> 0" "$(line subjects "notifications.dispatch.issue.> 0" 0 "$(clean_issue_subjects | jq -c '.total = 3')" "")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's subjects under notifications.dispatch.issue.>: the read holds 2 of the 3 the server counts"
done_case

new_case "consumers the server could not read are UNKNOWN"
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers)" | jq -c '.missing = ["ses_lost"]')" "")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's consumers: the server could not read ses_lost"
done_case

new_case "a key list that exits 0 with a violation is UNKNOWN"
replace_row interest_keys - "$(line interest_keys - 0 $'ses_a\nses_b\n' "nats: permissions violation: Permissions Violation for Subscribe to \"\$KV.envoy_interests.>\"")"
reading
expect_status 3
expect_line "UNKNOWN: the interest store's keys: nats kv ls envoy_interests: the nats CLI exited 0: nats: permissions violation: Permissions Violation for Subscribe to \"\$KV.envoy_interests.>\""
done_case

new_case "a listed name that is no key, and was not read, is UNKNOWN"
replace_row interest_keys - "$(line interest_keys - 0 $'ses_a\nses_b\nbad key\n' "")" "$(line interest_unread - 1 'bad key' "")"
reading
expect_status 3
expect_line "UNKNOWN: the interest store listed bad key, which is no key, and it was not read"
done_case

new_case "a record read that failed is UNKNOWN"
replace_row interest ses_b "$(line interest ses_b 1 "" "nats: error: nats: timeout")"
reading
expect_status 3
expect_line "UNKNOWN: the interest store's record ses_b: the nats CLI exited 1: nats: error: nats: timeout"
done_case

new_case "a first reading that did not read every surface is no first reading"
first_reading_of 73
jq '.verdict = "unknown"' "$FAKE/earlier.json" >"$FAKE/earlier.tmp"
mv "$FAKE/earlier.tmp" "$FAKE/earlier.json"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "did not read every surface; take a new first reading"
done_case

new_case "a first reading with no last stream sequence is no first reading"
first_reading_of 73
jq '.stream.last_seq = null' "$FAKE/earlier.json" >"$FAKE/earlier.tmp"
mv "$FAKE/earlier.tmp" "$FAKE/earlier.json"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: the first reading recorded no last stream sequence"
done_case

new_case "a counter line that cannot be read is UNKNOWN"
{ grep -v 'kind="interest"' "$base/metrics/envoy.envoy-listener-7f9c"; echo 'envoy_listener_legacy_subjects_total{kind="interest"} 4.5'; } >"$FAKE/metrics/envoy.envoy-listener-7f9c"
reading
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-listener-7f9c container envoy-listener: the line envoy_listener_legacy_subjects_total{kind=\"interest\"} 4.5 cannot be read"
done_case

new_case "two start-time samples are UNKNOWN"
{ cat "$base/metrics/envoy.envoy-listener-7f9c"; echo "envoy_listener_start_time_seconds{machine=\"gke\"} $((listener_start + 1))"; } >"$FAKE/metrics/envoy.envoy-listener-7f9c"
reading
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-listener-7f9c container envoy-listener serves 2 start-time samples"
done_case

new_case "a start-time gauge served at one reading only is UNKNOWN"
first_reading_of 73
metrics 4 1 >"$FAKE/metrics/envoy.envoy-listener-7f9c"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-listener-7f9c container envoy-listener served a start-time gauge at one reading and not the other"
done_case

echo "legacy-subjects-gate.test.sh: $passed cases passed, $failed failed"
[ "$failed" = 0 ]
