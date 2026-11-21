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
# reason alone, beside the clean fleet that is the only case that opens it: a listener's count that
# rose between the readings, under any kind, a Legion daemon's consumer still on an old filter, a
# consumer list that cannot be read at all, a stream still holding an old-form subject, an interest
# record holding one, a listener whose counter cannot be read, a listener whose restart only its
# start-time gauge shows, a listener offline at the reading, a list that moved between its pages, a
# stream that is not the first reading's or does not hold the whole window, and an answer that is no
# JSON object. Every case runs from the same clean fixture with one thing changed, so each fails for
# its own reason. The old-form rule itself is held row by row. The summary counts cases.
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

# pod NS NAME UID CONTAINER IMAGE COMMAND(JSON) RESTARTS STATE MACHINE prints one pod.
pod() {
  jq -n --arg ns "$1" --arg name "$2" --arg uid "$3" --arg container "$4" --arg image "$5" \
    --argjson command "$6" --argjson restarts "$7" --arg state "$8" --arg machine "$9" '
    {metadata: {namespace: $ns, name: $name, uid: $uid},
     spec: {containers: [{name: $container, image: $image}
       + (if $command == null then {} else {command: $command} end)
       + {env: [if $machine == "" then empty else {name: "ENVOY_MACHINE_ID", value: $machine} end]}]},
     status: {phase: "Running", containerStatuses: [{name: $container, restartCount: $restarts,
       state: {($state): {}}}]}}'
}

# metrics INTEREST ROLE [START [ROLE_READ]] prints a listener's metrics text: the counter's kinds,
# each when above zero, and the start-time gauge when START is given.
metrics() {
  echo "# HELP envoy_listener_legacy_subjects_total Names a caller gave in their old form"
  echo "# TYPE envoy_listener_legacy_subjects_total counter"
  [ "$1" = 0 ] || echo "envoy_listener_legacy_subjects_total{kind=\"interest\"} $1"
  [ "$2" = 0 ] || echo "envoy_listener_legacy_subjects_total{kind=\"role\"} $2"
  [ "${4:-0}" = 0 ] || echo "envoy_listener_legacy_subjects_total{kind=\"role_read\"} $4"
  if [ -n "${3:-}" ]; then
    echo "# HELP envoy_listener_start_time_seconds When this listener process started"
    echo "# TYPE envoy_listener_start_time_seconds gauge"
    echo "envoy_listener_start_time_seconds $3"
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

# stream_info [FIRST_TS [STREAM_ID]] prints the stream's info answer.
stream_info() {
  jq -cn --arg first "${1:-2026-11-18T00:00:00.123456789Z}" --arg id "${2-stream-id-1}" '
    {type: "io.nats.jetstream.api.v1.stream_info_response", created: "2026-11-01T00:00:00Z",
     config: {name: "ENVOY_NOTIFICATIONS", max_age: 259200000000000,
       metadata: (if $id == "" then {} else {envoy_stream_id: $id} end)},
     state: {messages: 9, first_ts: $first}}'
}

readonly user_info_refused="nats: permissions violation: Permissions Violation for Publish to \"\$SYS.REQ.USER.INFO\""

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
metrics 4 1 "$listener_start" >"$base/metrics/envoy.envoy-listener-7f9c"
metrics 2 0 "$listener_start" >"$base/metrics/dispatch.dispatch-listener-55d1"
metrics 7 0 "$hub_start" >"$base/outside/envoy-hub.example-tailnet.ts.net"
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
false	notifications.dispatch.issue.*.comment.created
false	notifications.dispatch.issue.*.ACME-12.>
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
false	notifications.agent.ses_a	["notifications.agent.ses_a"]
false	notifications.agent.ses_a	["notifications.agent.ses_a","notifications.agent.*.ses_b"]
false	notifications.agent.ses_a	["notifications.agent.ses_a","notifications.agent.lower.ses_a"]
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
jq '.version = 1' "$FAKE/earlier.json" >"$FAKE/old.json"
reading --since "$FAKE/old.json"
expect_status 2
expect_line "is no legacy-subjects-gate reading of this version (2)"
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
expect_last "FIRST READING: $FAKE/record.json holds a clean reading; read again at or after $(jq -r '(.finished_at + 259200) | todate' "$FAKE/record.json") with --since $FAKE/record.json"
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
[ "$(jq -r .verdict "$FAKE/record.json")" = clean ] || fail "the record's verdict is $(jq -r .verdict "$FAKE/record.json"), want clean"
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
  jq -e --arg image "mirror.gcr.io/natsio/nats-box:0.19.0@sha256:00f10dd4156f28ece84bb3a52cf356ef220ebaa0494310caa90dec184d9e2b89" '
    .metadata.namespace == "envoy" and (.metadata.name | startswith("legacy-subjects-gate-"))
    and .metadata.labels["created-by"] == "legacy-subjects-gate" and (.metadata.labels.owner | length) > 0
    and .spec.backoffLimit == 0 and .spec.activeDeadlineSeconds == 300 and .spec.ttlSecondsAfterFinished == 600
    and .spec.template.spec.serviceAccountName == "envoy-listener"
    and .spec.template.spec.securityContext.runAsUser == 65534 and .spec.template.spec.securityContext.runAsNonRoot == true
    and .spec.template.spec.containers[0].image == $image
    and .spec.template.spec.containers[0].securityContext.capabilities.drop == ["ALL"]
    and .spec.template.spec.containers[0].env == [{name: "NATS_URL", value: "nats://envoy-hub.envoy.svc.cluster.local:4222"}, {name: "NATS_NKEY", value: "/var/run/secrets/nats-seed/nats-seed"}, {name: "WORK_DIR", value: "/work"}]
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
jq -e '.spec.template.spec.containers[0].env == [{name: "NATS_URL", value: "nats://envoy-hub.envoy.svc.cluster.local:4222"}, {name: "NATS_TOKEN_FILE", value: "/var/run/secrets/envoy/nats-token/token"}, {name: "WORK_DIR", value: "/work"}, {name: "NATS_INBOX_PREFIX", value: "_INBOX_envoy_listener_gke"}]
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
[ "$(jq -r .verdict "$FAKE/record.json")" = clean ] || fail "an open reading's record is $(jq -r .verdict "$FAKE/record.json"), want clean, a first reading for the next window"
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
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers)" "$(consumer legion-go-ZED-dispatch '["notifications.dispatch.issue.ZED-7.>"]')")" "")"
reading
expect_status 1
expect_line "GATE SHUT: consumer legion-go-ZED-dispatch filters notifications.dispatch.issue.ZED-7.>, which only an old-form name matches"
expect_no_line "consumer ses_twin"
done_case

new_case "a consumer list that cannot be read is UNKNOWN, naming the refused request"
replace_row consumers 0 "$(line consumers 0 0 "" "nats: permissions violation: Permissions Violation for Publish to \"\$JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS\"")"
reading
expect_status 3
expect_line "UNKNOWN: the stream's consumers: the request \$JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS at offset 0 was answered with nothing: nats: permissions violation: Permissions Violation for Publish to \"\$JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS\""
expect_last "UNKNOWN"
done_case

# The Job's own script, run under sh behind a fake nats that answers as the hub does. The fake
# records every call, refuses the consumer list the way nats-server refuses a publish the grant
# lacks (status 0, no output, the violation on standard error), and answers every other read.
mkdir -p "$root/natsbin"
cat >"$root/natsbin/nats" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$CALLS"
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
        echo '{"type":"io.nats.jetstream.api.v1.stream_info_response","total":0,"offset":0,"limit":0,"config":{"name":"ENVOY_NOTIFICATIONS"},"state":{"messages":0}}' ;;
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

# run_reads [ENV...] runs lib/legacy-subjects-gate-nats.sh as the gate's Job runs it (sh -c with the
# script's text), with the fake nats first on PATH, for at most 20 seconds, keeping its output in
# $FAKE/reads.out, its calls in $FAKE/reads.calls and its exit status in $status.
run_reads() {
  : >"$FAKE/reads.calls"
  set +e
  env "$@" CALLS="$FAKE/reads.calls" WORK_DIR="$FAKE" PATH="$root/natsbin:$PATH" timeout 20 sh -c "$(cat "$here/lib/legacy-subjects-gate-nats.sh")" >"$FAKE/reads.out" 2>"$FAKE/reads.err"
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

new_case "the NATS reads page a list from the last item of the page before, print a record's session and topics alone, and pass the inbox prefix to every call"
run_reads NATS_INBOX_PREFIX=_INBOX_envoy_listener_gke
[ "$status" = 0 ] || fail "the NATS reads exited $status: $(cat "$FAKE/reads.err")"
offsets=$(awk -F'\t' '$1 == "consumers" {print $2}' "$FAKE/reads.out" | paste -sd, -)
[ "$offsets" = "0,1,2,3" ] || fail "the consumer list was read at offsets $offsets, want 0,1,2,3: each page from the last item of the one before"
record=$(awk -F'\t' '$1 == "interest" {print $4}' "$FAKE/reads.out" | base64 -d)
[ "$record" = '{"session_id":"ses_a","topics":["notifications.agent.ses_a"]}' ] || fail "the Job printed the record $record, want its session id and topics alone"
while IFS=$'\t' read -r _ _ _ out _; do printf '%s' "$out" | base64 -d; echo; done <"$FAKE/reads.out" >"$FAKE/reads.decoded"
! grep -q -- "private-work" "$FAKE/reads.decoded" || fail "the Job's log holds the record's directory"
[ "$(grep -c -v -- '--inbox-prefix _INBOX_envoy_listener_gke' "$FAKE/reads.calls")" = 0 ] || fail "a nats call took no inbox prefix: $(grep -v -- '--inbox-prefix' "$FAKE/reads.calls" | head -n 1)"
done_case

new_case "a stream still holding an old-form subject shuts the gate"
replace_row subjects "notifications.dispatch.issue.> 0" "$(line subjects "notifications.dispatch.issue.> 0" 0 "$(subjects_page notifications.dispatch.issue.ACME.ACME-12.comment.created notifications.dispatch.issue.ACME-13.comment.created)" "")"
reading
expect_status 1
expect_line "GATE SHUT: the stream holds 1 old-form subject(s) under notifications.dispatch.issue.>: notifications.dispatch.issue.ACME-13.comment.created"
[ "$(jq -c .subjects_old "$FAKE/record.json")" = '["notifications.dispatch.issue.ACME-13.comment.created"]' ] ||
  fail "the record names $(jq -c .subjects_old "$FAKE/record.json") as old subjects"
done_case

new_case "an interest record holding an old-form topic shuts the gate; its own agent subject does not"
replace_row interest ses_a "$(line interest ses_a 0 "$(interest ses_a notifications.agent.ses_a notifications.role.lead-reviewer)" "")"
reading
expect_status 1
expect_line "GATE SHUT: the interest record of session ses_a holds notifications.role.lead-reviewer, in an old form"
expect_no_line "notifications.agent.ses_a, in an old form"
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

new_case "a listener outside the cluster with no start-time gauge cannot be compared"
metrics 7 0 >"$FAKE/outside/envoy-hub.example-tailnet.ts.net"
first_reading_of 73
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: listener envoy-hub at https://envoy-hub.example-tailnet.ts.net:9020 runs in no pod and serves no start-time gauge"
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

new_case "a first reading that was not clean cannot open the gate"
replace_row subjects "notifications.dispatch.issue.> 0" "$(line subjects "notifications.dispatch.issue.> 0" 0 "$(subjects_page notifications.dispatch.issue.ACME-13.comment.created)" "")"
: >"$FAKE/calls"
reading
expect_status 1
jq '.started_at -= 262800 | .finished_at -= 262800 | .listeners |= map(.read_at -= 262800)' "$FAKE/record.json" >"$FAKE/earlier.json"
rm -f "$FAKE/record.json"
cp "$base/job.log" "$FAKE/job.log"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: the first reading, taken"
expect_line "was not clean: it held an old-form name"
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

echo "legacy-subjects-gate.test.sh: $passed cases passed, $failed failed"
[ "$failed" = 0 ]
