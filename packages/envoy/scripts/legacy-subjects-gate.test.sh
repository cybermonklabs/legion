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
# is not the first reading's, a second reading sooner than a counter line may be old, and an answer
# that is no JSON object. A first reading on a bus that still holds old-form messages, interests and
# consumer
# filters is a first reading that names each. A listener that dials the bus by another address
# than the listener Deployment's is skipped, and its durable unread, until --bus-alias names that
# address, and is then held to its restarts as any listener is, while a listener on another bus
# stays skipped, its NATS_URLS printed with any credential masked, and makes the reading UNKNOWN
# when it serves as the machine of a listener read (its machine read trimmed, as the listener reads
# it), or sets its machine as no literal value (a $(VAR) reference among them), and a container that
# lists ENVOY_MACHINE_ID or NATS_URLS twice makes it UNKNOWN too. At a second reading it reads the
# five facts, each with a case that shuts the gate or makes it UNKNOWN for that fact alone: a pin
# behind the new-form commit (a1), a census of pins that read none, a connected client at an older
# build (a2), a connection that names no build, a build's compare that gh could not make or that
# answered a status, a count or a text GitHub never gives, which prints no word of gh's, a hub
# server that does not answer the census, a census with no connection, a census whose list moved
# under it at an unchanged count, a user of
# the hub that may publish a notification subject mapped to no pin or to one the census of
# listener pins shut (a3), a hub with no such user, each shape of publish permission that reaches
# a notification subject and two that reach none, the auth service's entries behind an auth
# callout, an entry of a kind the gate's list does not classify and a list that classifies none,
# an auth service's Deployment that has not settled on its spec, an auth.conf line that holds more
# than its value, a user's object of another key or written otherwise than Render writes it,
# Render's own text, a count of ConfigMaps as BSD wc pads it, and a ConfigMap, an auth.conf, an
# auth service or a hub server it cannot read, a
# consumer that has not acknowledged the last old-form message it takes (b), a session's as well
# as a daemon's, an empty consumer list, a consumer the server could not read, a list shorter than
# the server's count and one with no daemon's consumer, a read of a family that saw nothing (b and
# c), and an old-form message still on the stream (c); new pins with one older client still
# connected keep the gate shut, and the purge driver refuses that reading and each reading whose
# (a3) or (b) is unknown; and a required-mode listener's connection, its build before its trailing
# privacy suffix, is judged by that build. Every case runs from the same clean
# fixture with one thing changed, so each fails for its own reason. The old-form rule itself is
# held row by row. The summary counts cases.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
# The gate runs from a copy of this directory's gate and lib/, beside a fake daemon-release-gate.sh
# (the census of pinned builds (a1) and of listener pins (a3) read), which adds its arguments as a
# line to $FAKE/release-gate.args and, with --listeners, prints $FAKE/release-gate-listeners.out
# and exits $FAKE/release-gate-listeners.rc, else prints $FAKE/release-gate.out and exits
# $FAKE/release-gate.rc (0 when absent).
mkdir -p "$root/gate/lib"
cp "$here/legacy-subjects-gate.sh" "$root/gate/"
cp "$here"/lib/legacy-subjects-gate-*.* "$root/gate/lib/"
cat >"$root/gate/daemon-release-gate.sh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE/release-gate.args"
case " $* " in
  *" --listeners "*)
    cat "$FAKE/release-gate-listeners.out"
    exit "$(cat "$FAKE/release-gate-listeners.rc" 2>/dev/null || echo 0)" ;;
esac
cat "$FAKE/release-gate.out"
exit "$(cat "$FAKE/release-gate.rc" 2>/dev/null || echo 0)"
EOF
chmod +x "$root/gate/daemon-release-gate.sh"
gate="$root/gate/legacy-subjects-gate.sh"
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
# fields ROW splits one tab-separated row of the Job log into leg, arg, rc, out and err with the
# shell's own read, a field left empty included: tab is a whitespace character to read, which joins
# runs of it, so the row is read with the unit separator, which no row holds, in its place. A cut
# per field would start four processes per row on every read of the log.
fields() {
  IFS=$'\x1f' read -r leg arg rc out err <<<"${1//$'\t'/$'\x1f'}"
}
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
  "-n envoy logs job/legacy-subjects-census-"*)
    # The census Job's log: the fixture's census rows, which name the census connection by the
    # name the gate drew for this reading and created the Job with (CENSUS_NAME), written
    # @CENSUS_NAME@ in the fixture, then its end line unless census.cut says the log was cut short.
    census=$(jq -r '[.spec.template.spec.containers[0].env[]? | select(.name == "CENSUS_NAME") | .value] | first // "no-census-name"' "$FAKE/created-census-job.json" 2>/dev/null)
    while IFS= read -r row; do
      fields "$row"
      [ "$leg" = census ] || continue
      out=$(printf '%s' "$out" | base64 -d | sed "s/@CENSUS_NAME@/${census:-no-census-name}/g" | base64 -w 0)
      printf '%s\t%s\t%s\t%s\t%s\n' census "$arg" "$rc" "$out" "$err"
    done <"$FAKE/job.log"
    [ -e "$FAKE/census.cut" ] || printf 'end\t-\t0\t\t\n' ;;
  "-n envoy logs job/"*)
    # The listener-identity Job's log: the fixture's rows but the census's.
    while IFS= read -r row; do
      [ "${row%%$'\t'*}" = census ] || printf '%s\n' "$row"
    done <"$FAKE/job.log" ;;
  "-n envoy get statefulset envoy-nats -o json")
    [ ! -e "$FAKE/statefulset.fail" ] || { echo 'Error from server (NotFound): statefulsets.apps "envoy-nats" not found' >&2; exit 1; }
    cat "$FAKE/statefulset.json" ;;
  "-n envoy get configmap "*" -o json")
    # A ConfigMap the hub's pods mount: $FAKE/configmaps/<name>.json, refused as the operator's
    # account may not read it when configmap.fail exists.
    [ ! -e "$FAKE/configmap.fail" ] || { echo "Error from server (Forbidden): configmaps \"$5\" is forbidden: User cannot get resource \"configmaps\" in API group \"\" in the namespace \"envoy\"" >&2; exit 1; }
    if [ -f "$FAKE/configmaps/$5.json" ]; then cat "$FAKE/configmaps/$5.json"
    else echo "Error from server (NotFound): configmaps \"$5\" not found" >&2; exit 1
    fi ;;
  "-n dispatch get deployment envoy-broker -o json")
    # The auth service's Deployment, refused when broker.fail exists.
    [ ! -e "$FAKE/broker.fail" ] || { echo 'Error from server (Forbidden): deployments.apps "envoy-broker" is forbidden: User cannot get resource "deployments" in API group "apps" in the namespace "dispatch"' >&2; exit 1; }
    cat "$FAKE/broker.json" ;;
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
    case "$(jq -r .metadata.name "$5")" in
      legacy-subjects-census-*) cp "$5" "$FAKE/created-census-job.json" ;;
      *) cp "$5" "$FAKE/created-job.json" ;;
    esac
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
cat >"$root/bin/gh" <<'EOF'
#!/usr/bin/env bash
# A fake gh for `gh api repos/cybermonklabs/legion/compare/<base>...<head> --jq <filter>`, answering
# from $FAKE/compare/<head> and recording the call in $FAKE/calls. $FAKE/compare/<head>.says makes
# the call fail, saying that file on stderr, and $FAKE/compare/<head>.raw is answered as it is.
set -u
printf 'gh %s\n' "$*" >>"$FAKE/calls"
[ "$1" = api ] && [ "$3" = --jq ] || { echo "fake gh: no answer for: $*" >&2; exit 99; }
head=${2##*...}
if [ -f "$FAKE/compare/$head.says" ]; then cat "$FAKE/compare/$head.says" >&2; exit 1
elif [ -f "$FAKE/compare/$head.raw" ]; then cat "$FAKE/compare/$head.raw"
elif [ -f "$FAKE/compare/$head" ]; then jq -c "$4" "$FAKE/compare/$head"
else echo "gh: Not Found (HTTP 404)" >&2; exit 1
fi
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
chmod +x "$root/bin/kubectl" "$root/bin/curl" "$root/bin/gh"

# --- the clean fixture ----------------------------------------------------------------------------
readonly envoy_image="ghcr.io/cybermonklabs/legion/envoy@sha256:1111111111111111111111111111111111111111111111111111111111111111"
readonly listener_start=1794900000
readonly hub_start=1794800000
readonly listener_nkey=UDAW25FEZ57VZNGE6FKAMUACIAK3LZRQHNVGUGPJS6L4INOXAQTX4AFQ
readonly durable_lapse_ns=604800000000000

readonly bus_url="nats://envoy-hub.envoy.svc.cluster.local:4222"

# at is the @ that ends a URL's userinfo. The fake users and passwords of the masking cases are
# joined to their hosts with it at run time, so no line of this file holds a URL with a user and a
# password in it, which a secret scanner takes for a leaked credential.
readonly at=@

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

# consumer NAME FILTERS(JSON) [PUSH_BOUND [INACTIVE_THRESHOLD_NS [ACK_FLOOR_STREAM_SEQ]]] prints one
# ConsumerInfo, its ack floor at stream sequence 900 unless ACK_FLOOR_STREAM_SEQ is given.
consumer() {
  jq -cn --arg name "$1" --argjson filters "$2" --argjson bound "${3:-null}" --argjson lapse "${4:-null}" --argjson floor "${5:-900}" '
    {stream_name: "ENVOY_NOTIFICATIONS", name: $name,
     config: ({durable_name: $name} + (if ($filters | length) == 1 then {filter_subject: $filters[0]} else {filter_subjects: $filters} end)
       + (if $lapse == null then {} else {inactive_threshold: $lapse} end)),
     delivered: {consumer_seq: 40, stream_seq: 900, last_active: "2026-11-20T23:59:00Z"},
     ack_floor: {consumer_seq: 40, stream_seq: $floor, last_active: "2026-11-20T23:59:00Z"}}
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

# The five facts' fixture. new_form is the commit from which every client writes only new-form
# names; newer is a later one. Each build a connection names has its compare with new_form in
# $FAKE/compare/<head>.
readonly new_form=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
readonly newer=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

# conn CID NAME [USER] prints one connection as the census Job prints it.
conn() {
  jq -cn --argjson cid "$1" --arg name "$2" --arg user "${3:-UCLIENT}" \
    '{cid: $cid, name: $name, account: "ENVOY", lang: "go", version: "1.55.0", authorized_user: $user}'
}

# census_reply SERVER [CONNECTION...] prints one server's census answer as the Job prints it.
census_reply() {
  local server=$1
  shift
  printf '%s\n' "$@" | jq -cs --arg s "$server" 'map(select(. != null)) as $c
    | {server: {name: $s, id: "N\($s)"}, error: null, data: {total: ($c | length), offset: 0, limit: 1024, connections: $c}}'
}

# clean_census prints the clean fleet's census: on each of the hub's three servers, connections
# that each name a build at or after new_form, the census connection's own among them.
clean_census() {
  census_reply envoy-nats-0 "$(conn 11 "envoy legion@$new_form")" "$(conn 12 "omp-ses_a @sjawhar/pi-envoy@21.0.0")" "$(conn 13 "@CENSUS_NAME@" UCENSUS)"
  census_reply envoy-nats-1 "$(conn 21 "legion-daemon ACME legion@$newer")" "$(conn 22 "dispatch legion@$new_form")"
  census_reply envoy-nats-2 "$(conn 31 "claude-envoy-channel-ses_b @sjawhar/claude-envoy@6.0.0")"
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
  line census 0 0 "$(clean_census)" ""
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
jq -n '{metadata: {name: "envoy-nats"},
  spec: {replicas: 3, template: {spec: {volumes: [{name: "config", configMap: {name: "envoy-nats-config"}}, {name: "pid", emptyDir: {}}]}}},
  status: {replicas: 3, currentRevision: "envoy-nats-7d9", updateRevision: "envoy-nats-7d9"}}' >"$base/statefulset.json"

# pyjson is the text Python's json.dumps writes with sort_keys and its default separators, as
# infra/core's PyJSON writes each user's object of Authorization.Render's auth.conf. It is this
# test's own, apart from the gate's reader, so the reader's check of a user's text is held against
# a writer it does not share; the golden row below holds both to Render's own text.
# shellcheck disable=SC2016 # jq's $-variables, not the shell's
readonly pyjson='def pyjson: if type == "object" then "{" + (to_entries | sort_by(.key) | map("\(.key | tojson): \(.value | pyjson)") | join(", ")) + "}"
  elif type == "array" then "[" + (map(pyjson) | join(", ")) + "]" else tojson end;'

# auth_conf [--callout] NAME=PERMISSIONS...: the hub's auth.conf as infra/core's Authorization.Render
# writes it, a user per NAME with the permissions object PERMISSIONS, the fifth its no_auth_user;
# --callout adds the auth callout's block and the ping lines it ends the text with.
auth_conf() {
  local callout=0 n=0 user
  if [ "${1:-}" = --callout ]; then
    callout=1
    shift
  fi
  printf '# Rendered by envoy-infra. Public keys only.\nauthorization {\n  users: [\n'
  for user in "$@"; do
    n=$((n + 1))
    printf '    # %s\n    %s\n' "${user%%=*}" "$(jq -rn --arg key "$(printf 'U%055d' "$n")" --argjson p "${user#*=}" "$pyjson"' {nkey: $key, permissions: $p} | pyjson')"
  done
  printf '  ]\n'
  if [ "$callout" = 1 ]; then
    printf '  auth_callout {\n    issuer: "AB2TCXFBYSIN24KFPVJARBN5CHCL4ZCV5SBWSONTZ2WYXS3XTAEK6S3G"\n    auth_users: ["%s"]\n    xkey: "XBVCTZK3PV5Y7TBIVYTDZBP3O2YGHUUL7MMENN4DTZIREJCPGLTAF6EL"\n  }\n' "$(printf 'U%055d' 1)"
  fi
  printf '}\n# members: a client presenting no credential becomes this user\nno_auth_user: "%s"\n' "$(printf 'U%055d' 5)"
  if [ "$callout" = 1 ]; then
    printf '%s\n' "# a replica of the auth service that answers nothing is dropped from the callout's queue group within 80 s" 'ping_interval: "20s"' 'ping_max: 2'
  fi
}

# hub_configmap DIR [auth_conf ARG...]: DIR's ConfigMap envoy-nats-config, which the hub's
# StatefulSet mounts: its nats.conf and the auth.conf auth_conf writes.
hub_configmap() {
  local dir=$1
  shift
  mkdir -p "$dir/configmaps"
  jq -n --arg auth "$(auth_conf "$@")" '{metadata: {name: "envoy-nats-config"}, data: {"nats.conf": "include auth.conf\n", "auth.conf": ($auth + "\n")}}' \
    >"$dir/configmaps/envoy-nats-config.json"
}

# The hub's users: Dispatch and the listener publish every notification subject, and the daemon,
# the legacy-cutover user and the sessions' fallback user none.
# shellcheck disable=SC2016 # $JS and $SYS begin NATS subjects, not the shell's expansions
readonly clean_users=(
  'dispatch={"publish": {"allow": ["notifications.>", "$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS"]}, "subscribe": {"allow": ["_INBOX.>"]}}'
  'daemon={"publish": {"allow": ["$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS", "$JS.ACK.ENVOY_NOTIFICATIONS.>"]}, "subscribe": {"allow": ["notifications.envoy.exceptions.notifications.role.>", "_INBOX.>"]}}'
  'listener={"publish": {"allow": ["notifications.>", "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.listener-gke"]}, "subscribe": {"allow": ["notifications.>", "_INBOX.>"]}}'
  'legacy-cutover={"publish": {"allow": ["$SYS.REQ.SERVER.PING.CONNZ", "$JS.API.STREAM.PURGE.ENVOY_NOTIFICATIONS"]}, "subscribe": {"allow": ["_INBOX_legacy_cutover.>"]}}'
  'members={"publish": {"allow": ["_INBOX.>", "agentstream.*.frames"]}, "subscribe": {"allow": ["notifications.envoy.deliver.*", "agentstream.*.control"]}}'
)
hub_configmap "$base" "${clean_users[@]}"
# The auth service's Deployment: its services, a listener's, Dispatch's, a daemon's, the
# measurement job's, an edge listener's, the operator's and the hold probe's, which a hub behind
# its callout admits, and a status settled on its spec, every replica of this generation and
# available, none terminating.
jq -n --arg services "listener/host-a/*=system:serviceaccount:envoy:host-a dispatch/example-dispatch/*=system:serviceaccount:dispatch:dispatch daemon/-/-=system:serviceaccount:legion:daemon measure/-/*=google:measure@example-project.iam.gserviceaccount.com edge-listener/edge-a/PRIV=system:serviceaccount:edge:edge-a operator/-/*=system:serviceaccount:envoy-operator:envoy-operator probe/-/*=system:serviceaccount:envoy-operator:envoy-hold-probe" \
  '{metadata: {name: "envoy-broker", generation: 7}, spec: {replicas: 2, template: {spec: {containers: [{name: "envoy-broker",
    env: [{name: "BROKER_NATS_URLS", value: "nats://envoy-nats.envoy.svc.cluster.local:4222"}, {name: "BROKER_NATS_SERVICES", value: $services}]}]}}},
    status: {observedGeneration: 7, replicas: 2, updatedReplicas: 2, readyReplicas: 2, availableReplicas: 2, terminatingReplicas: 0}}' >"$base/broker.json"
# The census of listener pins: no VM hub, and each pin at or after the commit.
printf '%s\n' "NO VM HUB: envoy/ at eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee declares no gcp:NatsHub" \
  "PASS: envoy-cluster runs $new_form (identical to $new_form)" "PASS: dispatch runs $newer (ahead of $new_form)" \
  "PASS: dispatch-server runs $newer (ahead of $new_form)" "GATE OPEN" >"$base/release-gate-listeners.out"
printf '%s\n' "PASS: ACME's image ghcr.io/example/worker@sha256:1111 runs pi-envoy and claude-envoy at $new_form" \
  "PASS: the sessions image ghcr.io/example/sessions@sha256:2222 runs pi-envoy and claude-envoy at $newer" "GATE OPEN" >"$base/release-gate.out"
mkdir -p "$base/compare"
echo '{"status":"identical","behind_by":0}' >"$base/compare/$new_form"
for head in "$newer" pi-envoy-v21.0.0 claude-envoy-v6.0.0; do
  echo '{"status":"ahead","behind_by":0}' >"$base/compare/$head"
done
echo '{"status":"behind","behind_by":12}' >"$base/compare/pi-envoy-v19.0.0"

# facts_default are the five facts' flags a second reading takes, the operator's map of the hub's
# publishers among them, and facts the ones a case's second readings take: new_case sets it to
# facts_default, and a case may change it before it reads.
readonly facts_default=(--deployment example/deploy --new-form-commit "$new_form" --hub-statefulset envoy-nats
  --broker dispatch/envoy-broker --publisher dispatch=dispatch --publisher dispatch=dispatch-server --publisher listener=envoy-cluster)
facts=("${facts_default[@]}")

# --- running a case -------------------------------------------------------------------------------
# new_case NAME copies the clean fixture into a fresh case directory, sets FAKE to it and names the
# case.
new_case() {
  case_name=$1
  case_failed=0
  FAKE="$root/case-$((passed + failed))-$RANDOM"
  cp -r "$base" "$FAKE"
  : >"$FAKE/calls"
  facts=("${facts_default[@]}")
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
# $FAKE/record.json; a second reading (--since) takes the five facts' flags (facts) too.
reading() {
  local extra=()
  case " $* " in *" --since "*) extra=("${facts[@]}") ;; esac
  run_gate --context example-cluster --record "$FAKE/record.json" --listener envoy-hub=https://envoy-hub.example-tailnet.ts.net:9020 "$@" ${extra[@]+"${extra[@]}"}
}

# replace_row LEG ARG [LINE...] replaces, in the case's Job log, the one row that is the read LEG of
# ARG with the lines given (none deletes it), failing the case unless exactly one row is that read.
replace_row() {
  local leg=$1 arg=$2 row row_leg row_arg matched=0
  shift 2
  while IFS= read -r row; do
    row_leg=${row%%$'\t'*}
    row_arg=${row#*$'\t'}
    row_arg=${row_arg%%$'\t'*}
    if [ "$row_leg" = "$leg" ] && [ "$row_arg" = "$arg" ]; then
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
  first_reading_with "$1" reading
}

# first_reading_with HOURS_AGO READER [ARG...] does the same with the reading READER [ARG...] takes.
# A first reading that wrote no record fails the case and leaves no $FAKE/earlier.json, so the
# case's second reading fails too and the test goes on to count the cases after it.
first_reading_with() {
  local hours=$1
  shift
  first_reading_secs "$((hours * 3600))" "$@"
}

# first_reading_secs SECONDS_AGO READER [ARG...] is first_reading_with, moved SECONDS_AGO seconds.
first_reading_secs() {
  local seconds=$1
  shift
  "$@"
  [ "$status" = 4 ] || fail "the first reading exited $status, want 4 (FIRST READING); output:"$'\n'"$output"
  if [ -f "$FAKE/record.json" ]; then
    jq --argjson s "$seconds" '.started_at -= $s | .finished_at -= $s | .read_at = (.started_at | todate)
      | .listeners |= map(.read_at -= $s)' "$FAKE/record.json" >"$FAKE/earlier.json"
  fi
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

new_case "a --bus-alias that is not a NATS_URLS is refused before anything is read"
run_gate --context example-cluster --record "$FAKE/record.json" --bus-alias envoy-nats.envoy.svc.cluster.local:4222
expect_status 2
expect_line "--bus-alias envoy-nats.envoy.svc.cluster.local:4222 is not a NATS_URLS"
[ ! -s "$FAKE/calls" ] || fail "the gate called $(head -n 1 "$FAKE/calls")"
# The other forms an alias is refused in: empty, two lines, a trailing comma, a user and password,
# a space after a comma, and a scheme in capitals. Each starts with no record and no call, so a form
# the gate took fails for itself and not for the one before.
for alias in "" $'nats://a:4222\nnats://b:4222' "nats://a:4222," "nats://alias-user:alias-pass${at}envoy-nats.envoy.svc.cluster.local:4222" \
  "nats://a:4222, nats://b:4222" NATS://envoy-nats.envoy.svc.cluster.local:4222; do
  rm -f "$FAKE/record.json"
  : >"$FAKE/calls"
  run_gate --context example-cluster --record "$FAKE/record.json" --bus-alias "$alias"
  [ "$status" = 2 ] || fail "--bus-alias $(printf %q "$alias"): exit status $status, want 2; output:"$'\n'"$output"
  grep -qF -- "is not a NATS_URLS: one or more scheme://host[:port], comma-separated" <<<"$output" ||
    fail "--bus-alias $(printf %q "$alias") is not refused as no NATS_URLS; output:"$'\n'"$output"
  [ ! -s "$FAKE/calls" ] || fail "--bus-alias $(printf %q "$alias"): the gate called $(head -n 1 "$FAKE/calls")"
done
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
expect_last "FIRST READING: $FAKE/record.json holds a first reading of every surface, with 0 FOUND line(s) naming old-form names still in use; read again at or after $(jq -r '(.finished_at + 180) | todate' "$FAKE/record.json") with --since $FAKE/record.json and the five facts' flags: that reading opens only if the stream then holds no old-form message (none after stream sequence 900), no listener's count moved, no interest record or consumer filter holds an old form, and the pins, the running clients, the publishers and the consumers are all past the old form"
[ "$(jq -c .facts "$FAKE/record.json")" = null ] || fail "a first reading recorded facts: $(jq -c .facts "$FAKE/record.json")"
[ ! -e "$FAKE/release-gate.args" ] || fail "a first reading ran the release gate: $(cat "$FAKE/release-gate.args")"
[ ! -e "$FAKE/created-census-job.json" ] || fail "a first reading created a census Job: $(jq -c .metadata "$FAKE/created-census-job.json")"
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

# No wait for the stream's retention: a second reading 200 s after the first, with the five facts
# true, opens.
new_case "a clean fleet read twice, 200 s apart, with the five facts true, opens"
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 0
expect_last "GATE OPEN"
expect_line "PASS: listener pod envoy/envoy-listener-7f9c container envoy-listener counted no old-form name between the readings"
expect_line "PASS: listener envoy-hub at https://envoy-hub.example-tailnet.ts.net:9020 counted no old-form name between the readings"
expect_line "PASS: (a1) the pins: daemon-release-gate.sh --clients $new_form read 2 pin(s) and client(s), each at or after it"
expect_line "PASS: (a2) the running clients: every one of the 5 connection(s) on the 3 hub server(s) (envoy-nats-0, envoy-nats-1, envoy-nats-2) names a build at or after $new_form, 4 build(s): @sjawhar/claude-envoy@6.0.0, @sjawhar/pi-envoy@21.0.0, legion@$new_form, legion@$newer"
expect_line "PASS: (b) the consumers: each of the stream's 7 consumer(s) has acknowledged every old-form message it takes (0 old-form subject(s) on the stream)"
expect_line "PASS: (c) the stream: the stream holds no old-form message under"
expect_line "PASS: (a3) the publishers: each of the 2 user(s) of the hub that may publish a notification subject runs a listener pin the census passed at or after $new_form: dispatch (dispatch, dispatch-server); listener (envoy-cluster)"
[ "$(jq -r .verdict "$FAKE/record.json")" = complete ] || fail "an open reading's record is $(jq -r .verdict "$FAKE/record.json"), want complete, a first reading for the next"
[ "$(jq -c '.facts | {a1, a2, a3, b, c, new_form_commit}' "$FAKE/record.json")" = "{\"a1\":\"true\",\"a2\":\"true\",\"a3\":\"true\",\"b\":\"true\",\"c\":\"true\",\"new_form_commit\":\"$new_form\"}" ] ||
  fail "the record's facts are $(jq -c .facts "$FAKE/record.json")"
# The record keeps the operator's map as given, each entry with its key's verdict.
[ "$(jq -c .facts.publishers "$FAKE/record.json")" = '{"map":[{"identity":"dispatch","key":"dispatch","verdict":"pass"},{"identity":"dispatch","key":"dispatch-server","verdict":"pass"},{"identity":"listener","key":"envoy-cluster","verdict":"pass"}],"users":["dispatch","listener"]}' ] ||
  fail "the record's publishers are $(jq -c .facts.publishers "$FAKE/record.json")"
[ "$(cat "$FAKE/release-gate.args" 2>/dev/null)" = "--deployment example/deploy --clients $new_form"$'\n'"--deployment example/deploy --listeners $new_form" ] ||
  fail "the release gate was run as $(cat "$FAKE/release-gate.args" 2>/dev/null)"
grep -qxF -- "--context example-cluster --request-timeout=60s -n envoy get configmap envoy-nats-config -o json" "$FAKE/calls" ||
  fail "the hub's auth.conf was not read from the ConfigMap its StatefulSet mounts"
# The census runs in a Job of its own, as the legacy-cutover user's ServiceAccount with that user's
# seed alone mounted; the listener-identity Job takes nothing of the census.
jq -e '.spec.template.spec as $s
  | .metadata.labels["created-by"] == "legacy-subjects-gate-census" and .spec.ttlSecondsAfterFinished == 600 and .spec.backoffLimit == 0
  and $s.serviceAccountName == "legacy-cutover" and $s.automountServiceAccountToken == false and $s.securityContext.runAsUser == 65534
  and $s.containers[0].securityContext.capabilities.drop == ["ALL"]
  and ([$s.containers[0].env[] | select(.name == "CENSUS_NKEY") | .value] == ["/var/run/secrets/legacy-cutover/nats-seed"])
  and ([$s.containers[0].env[] | select(.name == "CENSUS_INBOX_PREFIX") | .value] == ["_INBOX_legacy_cutover"])
  and ([$s.containers[0].env[] | select(.name == "NATS_URL") | .value] == ["nats://envoy-hub.envoy.svc.cluster.local:4222"])
  and ([$s.containers[0].env[] | select(.name == "CENSUS_NAME") | .value | test("^legacy-subjects-gate-census-[0-9a-f]{16}$")] == [true])
  and ($s.containers[0].volumeMounts == [{name: "legacy-cutover-seed", mountPath: "/var/run/secrets/legacy-cutover", readOnly: true}, {name: "legacy-subjects-census-work", mountPath: "/work"}])
  and ($s.volumes[0] == {name: "legacy-cutover-seed", csi: {driver: "secrets-store-gke.csi.k8s.io", readOnly: true, volumeAttributes: {secretProviderClass: "legacy-cutover-nats-seed"}}})' \
  "$FAKE/created-census-job.json" >/dev/null || fail "the census Job does not ask as the legacy-cutover user alone: $(jq -c .spec.template "$FAKE/created-census-job.json" 2>/dev/null)"
[ "$(jq -r '.spec.template.spec.containers[0].command[2]' "$FAKE/created-census-job.json")" = "$(cat "$here/lib/legacy-subjects-gate-census.sh")" ] ||
  fail "the census Job does not run lib/legacy-subjects-gate-census.sh"
jq -e '[.spec.template.spec.containers[0].env[] | select(.name | startswith("CENSUS_"))] | length == 0' "$FAKE/created-job.json" >/dev/null ||
  fail "the listener-identity Job takes something of the census"
for job in "$FAKE/created-job.json" "$FAKE/created-census-job.json"; do
  expect_line "JOB: deleted envoy/$(jq -r .metadata.name "$job"), and envoy holds no Job or pod of that name"
done
done_case

# The time a first reading names must open a clean fleet however old each listener's counter line
# is at the second reading: the first reading read each line at or before its own end, and the
# second takes a line up to three minutes old, so the time named is those three minutes after the
# first reading's end, and each listener's line then is later than the one the first reading read.
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
[ "${old%%|*}" = notifications.agent.ses_old ] && [ "$(($(wc -l <<<"$old")))" = 1 ] ||
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

# The census Job's own script, under sh, behind a fake nats that answers $SYS.REQ.SERVER.PING.CONNZ
# as one server holding 2000 connections does, cids 1 to 2000 in order, one page of the limit asked
# from the offset asked. Each page after the first is asked from the last connection of the page
# before, so the judge can read a list that moved between two pages.
mkdir -p "$root/censusbin"
cat >"$root/censusbin/nats" <<'EOF'
#!/usr/bin/env bash
subject=${*: -2:1}
body=${*: -1}
[ "$subject" = '$SYS.REQ.SERVER.PING.CONNZ' ] || { echo "fake nats: no answer for $*" >&2; exit 99; }
jq -cn --argjson q "$body" '($q.offset) as $o | ($q.limit) as $l
  | {server: {name: "envoy-nats-0", id: "N0"}, data: {total: 2000, offset: $o, limit: $l,
      connections: [range($o; [$o + $l, 2000] | min) | {cid: (. + 1), name: "c\(. + 1) legion@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}'
EOF
chmod +x "$root/censusbin/nats"
new_case "the census asks each next page from the last connection of the page before, and reads the list whole"
set +e
env CENSUS_NKEY=/dev/null CENSUS_NAME=legacy-subjects-gate-census-test CENSUS_INBOX_PREFIX=_INBOX_legacy_cutover WORK_DIR="$FAKE" \
  PATH="$root/censusbin:$PATH" timeout 60 sh -c "$(cat "$here/lib/legacy-subjects-gate-census.sh")" >"$FAKE/census.out" 2>"$FAKE/census.err"
status=$?
set -e
[ "$status" = 0 ] || fail "the census exited $status: $(cat "$FAKE/census.err")"
offsets=$(awk -F'\t' '$1 == "census" {print $2}' "$FAKE/census.out" | paste -sd, -)
[ "$offsets" = "0,1023" ] || fail "the census asked its pages at offsets $offsets, want 0,1023: each from the last connection of the page before"
read_cids=$(awk -F'\t' '$1 == "census" {print $4}' "$FAKE/census.out" | while IFS= read -r out; do base64 -d <<<"$out"; echo; done | jq -r 'objects | .data.connections[].cid' | sort -nu | wc -l)
[ "$((read_cids))" = 2000 ] || fail "the census read $((read_cids)) distinct connections, want 2000"
[ "$(tail -n 1 "$FAKE/census.out" | cut -f1)" = end ] || fail "the census did not reach its end line"
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
expect_line "read again while it runs, naming it with --listener laptop=<url> if it runs outside the cluster, or its NATS_URLS with --bus-alias if it runs in the cluster and dials the bus by another address"
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

new_case "a second reading sooner than a listener's counter line may be old shuts the gate"
first_reading_secs 100 reading
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "s passed between the first reading's end"
expect_line "less than the 180 s a listener's counter line may be old, so this reading may read a first reading's own line; read again at or after"
done_case

new_case "a listener whose counter line is no later than the one the first reading read shuts the gate"
first_reading_secs 200 reading
# The listener stopped logging after the first reading read its line: its newest line now is that
# same line, though it is younger than three minutes. One clock read gives both readings that line's
# time, so a second boundary between two reads cannot make the lines differ.
line_at=$(($(date -u +%s) - 30))
jq --argjson line_at "$line_at" '.listeners |= map(if .uid == "envoy-uid-1" then .read_at = $line_at else . end)' "$FAKE/earlier.json" >"$FAKE/earlier.tmp"
mv "$FAKE/earlier.tmp" "$FAKE/earlier.json"
echo "$line_at" >"$FAKE/log_at"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: listener pod envoy/envoy-listener-7f9c container envoy-listener's counter line at this reading was logged at"
expect_line "no later than the line the first reading read"
done_case

new_case "a stream made anew between the readings is UNKNOWN"
first_reading_of 73
replace_row stream_info - "$(line stream_info - 0 "$(stream_info "" stream-id-2)" "")"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: the stream is not the one the first reading read: its envoy_stream_id was stream-id-1 and is stream-id-2"
done_case

# The purge moves the stream's first message on, and the wait for the stream's retention is gone:
# a stream whose first message is ten hours old opens a clean second reading.
new_case "a stream whose first message is younger than its retention opens a clean second reading"
first_reading_secs 200 reading
replace_row stream_info - "$(line stream_info - 0 "$(stream_info "$(jq -rn 'now - 36000 | floor | todate')")" "")"
reading --since "$FAKE/earlier.json"
expect_status 0
expect_last "GATE OPEN"
done_case

# --- the five facts -------------------------------------------------------------------------------
new_case "a second reading given none of the five facts' flags is UNKNOWN, naming each"
first_reading_secs 200 reading
run_gate --context example-cluster --record "$FAKE/record.json" --listener envoy-hub=https://envoy-hub.example-tailnet.ts.net:9020 --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a1) the pins: the census of pinned builds was not read: name --deployment <owner/repo> and --new-form-commit <commit>"
expect_line "UNKNOWN: (a2) the running clients: no census of the hub's connections was read: name --new-form-commit <commit>"
expect_line "UNKNOWN: (a3) the publishers: the hub's publishers were not read: name --deployment <owner/repo> and --new-form-commit <commit> and --hub-statefulset <name>"
[ ! -e "$FAKE/created-census-job.json" ] || fail "a reading with no --new-form-commit created a census Job"
[ ! -e "$FAKE/release-gate.args" ] || fail "a reading with no --deployment ran the release gate: $(cat "$FAKE/release-gate.args")"
[ "$(jq -c '.facts | {a1, a2, a3, b, c}' "$FAKE/record.json")" = '{"a1":"unknown","a2":"unknown","a3":"unknown","b":"true","c":"true"}' ] ||
  fail "the record's facts are $(jq -c .facts "$FAKE/record.json")"
done_case

new_case "(a1) a pin that still writes an old form shuts the gate"
first_reading_secs 200 reading
printf '%s\n' "PASS: the sessions image ghcr.io/example/sessions@sha256:2222 runs pi-envoy and claude-envoy at $newer" \
  "GATE SHUT: ACME's image ghcr.io/example/worker@sha256:3333 runs pi-envoy at cccccccccccccccccccccccccccccccccccccccc, behind $new_form" "GATE SHUT" >"$FAKE/release-gate.out"
echo 1 >"$FAKE/release-gate.rc"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: (a1) the pins: daemon-release-gate.sh --clients $new_form shut: ACME's image ghcr.io/example/worker@sha256:3333 runs pi-envoy at cccccccccccccccccccccccccccccccccccccccc, behind $new_form"
expect_last "GATE SHUT"
done_case

new_case "(a1) an empty census of pins is UNKNOWN, whether the release gate says so or opens on nothing"
first_reading_secs 200 reading
printf '%s\n' "UNKNOWN: the deployment's Legion projects run no daemon, so no pin was read" "UNKNOWN" >"$FAKE/release-gate.out"
echo 3 >"$FAKE/release-gate.rc"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a1) the pins: daemon-release-gate.sh --clients $new_form did not read every pin (exit 3): the deployment's Legion projects run no daemon, so no pin was read"
rm -f "$FAKE/record.json" "$FAKE/release-gate.rc"
echo "GATE OPEN" >"$FAKE/release-gate.out"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a1) the pins: daemon-release-gate.sh --clients $new_form enumerated no pin (its verdict GATE OPEN, exit 0), and a census that read nothing passes nothing"
expect_last "UNKNOWN"
done_case

new_case "(a2) a connected client at an older build shuts the gate"
first_reading_secs 200 reading
replace_row census 0 "$(line census 0 0 "$(clean_census | sed '3d'; census_reply envoy-nats-2 "$(conn 31 "claude-envoy-channel-ses_b @sjawhar/claude-envoy@6.0.0")" "$(conn 32 "omp-ses_old @sjawhar/pi-envoy@19.0.0" UOLD)")" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: (a2) the running clients: 1 connection(s) run @sjawhar/pi-envoy@19.0.0, pi-envoy-v19.0.0 is behind $new_form (behind by 12): omp-ses_old @sjawhar/pi-envoy@19.0.0 (server envoy-nats-2, cid 32, user UOLD)"
[ "$(jq -r .facts.a2 "$FAKE/record.json")" = false ] || fail "the record's (a2) is $(jq -r .facts.a2 "$FAKE/record.json")"
# The control of the compare rows below: an answer of GitHub's own status and a count is printed
# and keeps nothing beside the record.
! compgen -G "$FAKE/record.json.compare.*" >/dev/null || fail "a compare GitHub answered with its own status and a count was kept beside the record"
done_case

# --- (a2) a build's compare prints no word of gh's ------------------------------------------------
# Whatever answers gh's compare in GitHub's place holds gh's token and writes gh's own words and its
# answer. Each case puts the marker not-a-secret-token in one of them for the compare of
# claude-envoy-v6.0.0, the release tag of a clean connection's build, and no run of eight of its
# characters (redacttest.Piece's run) may be in what the gate prints or in the record it writes,
# which legacy-subjects-purge.sh reads. The twin: the file the build's line names, beside the
# record, does hold the marker. The control: the fake gh is the one asked for that compare.
readonly marker=not-a-secret-token
readonly marked_head=claude-envoy-v6.0.0

# holds_marker TEXT: whether TEXT holds a run of eight characters of the marker.
holds_marker() {
  local start
  for ((start = 0; start + 8 <= ${#marker}; start++)); do
    [[ "$1" == *"${marker:start:8}"* ]] && return 0
  done
  return 1
}

# marked_compare WANT: a second reading of the case, whose answer to the compare of marked_head the
# case set. It must print WANT and then the words naming the one file beside the record that keeps
# gh's answer and its own words, and no run of the marker, in its lines or in its record.
marked_compare() {
  local kept
  reading --since "$FAKE/earlier.json"
  ! holds_marker "$output" || fail "the gate printed a run of the marker gh's compare of $marked_head held; output:"$'\n'"$output"
  [ -f "$FAKE/record.json" ] || fail "the reading wrote no record"
  ! holds_marker "$(cat "$FAKE/record.json" 2>/dev/null)" || fail "the record holds a run of the marker gh's compare of $marked_head held"
  grep -qxF -- "gh api repos/cybermonklabs/legion/compare/$new_form...$marked_head --jq {status, behind_by}" "$FAKE/calls" ||
    fail "the fake gh was not asked for the compare of $marked_head"
  kept=("$FAKE"/record.json.compare.*)
  if [ "${#kept[@]}" = 1 ] && [ -f "${kept[0]}" ]; then
    expect_line "$1; gh's answer and its own words are in ${kept[0]}"
    grep -qF -- "$marker" "${kept[0]}" || fail "the file ${kept[0]} does not hold the marker gh's compare held"
  else
    fail "the reading kept gh's compare in ${#kept[@]} file(s) beside the record, want 1: ${kept[*]}"
  fi
}

new_case "(a2) a compare gh could not make prints none of gh's words, and names the file that keeps them"
first_reading_secs 200 reading
printf 'gh: %s (HTTP 502)\n' "$marker" >"$FAKE/compare/$marked_head.says"
marked_compare "UNKNOWN: (a2) the running clients: 1 connection(s) run @sjawhar/claude-envoy@6.0.0, which could not be judged against $new_form: GitHub could not compare $marked_head"
expect_status 3
done_case

new_case "(a2) a compare answered with a status GitHub never gives prints none of it, and names the file that keeps it"
first_reading_secs 200 reading
printf '{"status":"%s","behind_by":0}\n' "$marker" >"$FAKE/compare/$marked_head"
marked_compare "UNKNOWN: (a2) the running clients: 1 connection(s) run @sjawhar/claude-envoy@6.0.0, which could not be judged against $new_form: GitHub compared $marked_head with $new_form as none of its statuses (ahead, behind, diverged, identical)"
expect_status 3
done_case

new_case "(a2) a compare answered behind with a behind_by that is no count prints none of it, and still shuts the gate"
first_reading_secs 200 reading
printf '{"status":"behind","behind_by":"%s"}\n' "$marker" >"$FAKE/compare/$marked_head"
marked_compare "GATE SHUT: (a2) the running clients: 1 connection(s) run @sjawhar/claude-envoy@6.0.0, $marked_head is behind $new_form (behind by a count GitHub did not give)"
expect_status 1
done_case

new_case "(a2) a compare answered with no JSON object names the file that keeps the answer"
first_reading_secs 200 reading
printf '%s\n' "$marker" >"$FAKE/compare/$marked_head.raw"
marked_compare "UNKNOWN: (a2) the running clients: 1 connection(s) run @sjawhar/claude-envoy@6.0.0, which could not be judged against $new_form: GitHub's compare answered no status"
expect_status 3
done_case

new_case "(a2) a connection that names no build is UNKNOWN"
first_reading_secs 200 reading
replace_row census 0 "$(line census 0 0 "$(clean_census | sed '3d'; census_reply envoy-nats-2 "$(conn 31 "claude-envoy-channel-ses_b @sjawhar/claude-envoy@6.0.0")" "$(conn 33 "omp-ses_custom" UCUSTOM)")" "")"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a2) the running clients: 1 connection(s) name no build: omp-ses_custom (server envoy-nats-2, cid 33, user UCUSTOM)"
done_case

# A listener in required privacy mode keeps natsgrant.PrivacyRequiredSuffix last, its build before
# it (internal/bus's connectionName), and (a2) reads that build from the word before the suffix.
new_case "(a2) a required-mode listener's connection is judged by the build before its privacy suffix"
first_reading_secs 200 reading
replace_row census 0 "$(line census 0 0 "$(census_reply envoy-nats-0 "$(conn 11 "envoy-listener legion@$new_form privacy-required")" "$(conn 12 "omp-ses_a @sjawhar/pi-envoy@21.0.0")" "$(conn 13 "@CENSUS_NAME@" UCENSUS)"; clean_census | sed '1d')" "")"
reading --since "$FAKE/earlier.json"
expect_status 0
expect_last "GATE OPEN"
expect_line "PASS: (a2) the running clients: every one of the 5 connection(s) on the 3 hub server(s) (envoy-nats-0, envoy-nats-1, envoy-nats-2) names a build at or after $new_form, 4 build(s): @sjawhar/claude-envoy@6.0.0, @sjawhar/pi-envoy@21.0.0, legion@$new_form, legion@$newer"
expect_no_line "name no build"
done_case

new_case "(a2) a hub server that does not answer the census is UNKNOWN"
first_reading_secs 200 reading
replace_row census 0 "$(line census 0 0 "$(clean_census | sed '3d')" "")"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a2) the running clients: hub server(s) envoy-nats-2 did not answer the request \$SYS.REQ.SERVER.PING.CONNZ at offset 0"
done_case

new_case "(a2) a census that enumerates no connection but its own is UNKNOWN"
first_reading_secs 200 reading
replace_row census 0 "$(line census 0 0 "$(census_reply envoy-nats-0 "$(conn 13 "@CENSUS_NAME@" UCENSUS)"; census_reply envoy-nats-1; census_reply envoy-nats-2)" "")"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a2) the running clients: the census of 3 hub server(s) (envoy-nats-0, envoy-nats-1, envoy-nats-2) enumerated no connection but its own, and a census that read nothing passes nothing"
done_case

new_case "(a2) a census request the grant refuses, and a hub StatefulSet that cannot be read, are each UNKNOWN"
first_reading_secs 200 reading
replace_row census 0 "$(line census 0 0 "" "04:10:18 Unexpected NATS error from server nats://envoy-hub.envoy.svc.cluster.local:4222: nats: permissions violation: Permissions Violation for Publish to \"\$SYS.REQ.SERVER.PING.CONNZ\"")"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a2) the running clients: the request \$SYS.REQ.SERVER.PING.CONNZ at offset 0: 04:10:18 Unexpected NATS error from server nats://envoy-hub.envoy.svc.cluster.local:4222: nats: permissions violation"
rm -f "$FAKE/record.json"
cp "$base/job.log" "$FAKE/job.log"
touch "$FAKE/statefulset.fail"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a2) the running clients: the hub servers that must each answer the census could not be read: the StatefulSet envoy/envoy-nats could not be read: Error from server (NotFound)"
done_case

new_case "(a2) a census Job whose log ends before its last read is UNKNOWN"
first_reading_secs 200 reading
touch "$FAKE/census.cut"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a2) the running clients: the census of the hub's connections, as the legacy-cutover user: the census Job's log ends before its last read, so what it read is not whole"
done_case

# The bus a second reading finds before the purge: every producer has moved and the old-form
# messages are still stored. ACME-13's old-form subject's last message is stream sequence 812.
# old_stream_rows replaces the case's issue subjects with one that still holds it.
old_stream_rows() {
  replace_row subjects "notifications.dispatch.issue.> 0" \
    "$(line subjects "notifications.dispatch.issue.> 0" 0 "$(subjects_page notifications.dispatch.issue.ACME.ACME-12.comment.created notifications.dispatch.issue.ACME-13.comment.created)" "")" \
    "$(last_message notifications.dispatch.issue.ACME-13.comment.created 812)"
}

new_case "(b) a daemon's consumer that has not acknowledged the last old-form message it takes shuts the gate"
first_reading_secs 200 reading
old_stream_rows
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers | grep -v '"legion-go-ACME-dispatch"')" "$(consumer legion-go-ACME-dispatch '["notifications.dispatch.issue.>"]' null null 812)" "$(consumer legion-go-ZED-dispatch '["notifications.dispatch.issue.>"]' null null 700)")" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: (b) the consumers: consumer legion-go-ZED-dispatch has acknowledged up to stream sequence 700, before the last old-form message it takes, notifications.dispatch.issue.ACME-13.comment.created at stream sequence 812"
# One that has acknowledged it, at its ack floor, is past it.
expect_no_line "consumer legion-go-ACME-dispatch has acknowledged"
[ "$(jq -r .facts.b "$FAKE/record.json")" = false ] || fail "the record's (b) is $(jq -r .facts.b "$FAKE/record.json")"
done_case

new_case "(b) a session's consumer that has not acknowledged the last old-form message it takes shuts the gate"
first_reading_secs 200 reading
replace_row subjects "notifications.agent.> 0" \
  "$(line subjects "notifications.agent.> 0" 0 "$(subjects_page notifications.agent.ACME.ses_a notifications.agent._.ses_b notifications.agent.ses_twin)" "")" \
  "$(last_message notifications.agent.ses_twin 850)"
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers | grep -v '"ses_twin"')" "$(consumer ses_twin '["notifications.agent.ses_twin","notifications.agent.*.ses_twin"]' null null 849)")" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: (b) the consumers: consumer ses_twin has acknowledged up to stream sequence 849, before the last old-form message it takes, notifications.agent.ses_twin at stream sequence 850"
done_case

new_case "(b) an empty consumer list is UNKNOWN"
first_reading_secs 200 reading
replace_row consumers 0 "$(line consumers 0 0 '{"type":"io.nats.jetstream.api.v1.consumer_list_response","total":0,"offset":0,"limit":256,"consumers":[]}' "")"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (b) the consumers: the stream lists no consumer, and a census that read nothing passes nothing"
done_case

new_case "(c) an old-form message still on the stream shuts the gate, every consumer past it"
first_reading_secs 200 reading
old_stream_rows
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: (c) the stream: the stream holds 1 old-form subject(s); once (a1), (a2), (a3) and (b) read true, legacy-subjects-purge.sh purges them"
expect_line "PASS: (b) the consumers: each of the stream's 7 consumer(s) has acknowledged every old-form message it takes (1 old-form subject(s) on the stream, the last at stream sequence 812)"
expect_line "PASS: (a2) the running clients:"
[ "$(jq -c '.facts | {a1, a2, a3, b, c, last_old_seq}' "$FAKE/record.json")" = '{"a1":"true","a2":"true","a3":"true","b":"true","c":"false","last_old_seq":812}' ] ||
  fail "the record's facts are $(jq -c .facts "$FAKE/record.json")"
done_case

# The reading the purge exists for, less one client: the pins are new and every consumer is past
# the stored old-form messages, but one older client is still connected and could publish more.
new_case "new pins with one older client still connected keep the gate shut, and the purge driver refuses that reading"
first_reading_secs 200 reading
old_stream_rows
replace_row census 0 "$(line census 0 0 "$(clean_census | sed '3d'; census_reply envoy-nats-2 "$(conn 31 "claude-envoy-channel-ses_b @sjawhar/claude-envoy@6.0.0")" "$(conn 32 "omp-ses_old @sjawhar/pi-envoy@19.0.0" UOLD)")" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "PASS: (a1) the pins:"
expect_line "GATE SHUT: (a2) the running clients: 1 connection(s) run @sjawhar/pi-envoy@19.0.0"
: >"$FAKE/calls"
set +e
output=$(PATH="$root/bin:$PATH" bash "$here/legacy-subjects-purge.sh" --context example-cluster --reading "$FAKE/record.json" 2>&1)
status=$?
set -e
expect_status 1
expect_line "REFUSED: the reading $FAKE/record.json read (a2) the running clients false, not true"
[ ! -s "$FAKE/calls" ] || fail "the refused purge called $(head -n 1 "$FAKE/calls")"
done_case

# purge_refuses FACT WORDS runs legacy-subjects-purge.sh on the case's record and fails the case
# unless it refuses, naming FACT read as WORDS, before any call.
purge_refuses() {
  : >"$FAKE/calls"
  set +e
  output=$(PATH="$root/bin:$PATH" bash "$here/legacy-subjects-purge.sh" --context example-cluster --reading "$FAKE/record.json" 2>&1)
  status=$?
  set -e
  expect_status 1
  expect_line "REFUSED: the reading $FAKE/record.json read $1 $2, not true"
  [ ! -s "$FAKE/calls" ] || fail "the refused purge called $(head -n 1 "$FAKE/calls")"
}

# The reading the purge exists for, with the consumer list the server answered not whole: (b)
# reads no consumer it could not read, so it is unknown, and the purge, which reads (b) alone of
# the consumers, refuses it.
new_case "(b) a consumer the server could not read is unknown, and the purge driver refuses that reading"
first_reading_secs 200 reading
old_stream_rows
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers)" | jq -c '.missing = ["legion-go-ZED-dispatch"]')" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "UNKNOWN: (b) the consumers: the server could not read 1 consumer(s) of the stream (legion-go-ZED-dispatch), so what they acknowledged is not known"
[ "$(jq -r .facts.b "$FAKE/record.json")" = unknown ] || fail "the record's (b) is $(jq -r .facts.b "$FAKE/record.json"), want unknown"
purge_refuses "(b) the consumers" unknown
done_case

new_case "(b) a consumer list shorter than the server's count is unknown, and the purge driver refuses that reading"
first_reading_secs 200 reading
old_stream_rows
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers)" | jq -c '.total += 1')" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "UNKNOWN: (b) the consumers: the list read 7 distinct consumer(s) of the 8 the server counts, so what the others acknowledged is not known"
[ "$(jq -r .facts.b "$FAKE/record.json")" = unknown ] || fail "the record's (b) is $(jq -r .facts.b "$FAKE/record.json"), want unknown"
purge_refuses "(b) the consumers" unknown
done_case

new_case "(b) a consumer list with no Legion daemon's consumer is unknown, and the purge driver refuses that reading"
first_reading_secs 200 reading
old_stream_rows
replace_row consumers 0 "$(line consumers 0 0 "$(consumers_page "$(clean_consumers | grep -v '"legion-go-')")" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "UNKNOWN: (b) the consumers: no Legion daemon consumer (legion-go-<project>-dispatch) is on the stream, so the daemons' consumers were not read"
[ "$(jq -r .facts.b "$FAKE/record.json")" = unknown ] || fail "the record's (b) is $(jq -r .facts.b "$FAKE/record.json"), want unknown"
purge_refuses "(b) the consumers" unknown
done_case

# A whole read of the agent family that lists no subject saw nothing (the surface line's rule), so
# neither fact that reads the stream holds: its old-form messages are not known.
new_case "(b) and (c) are unknown when a read of the agent family saw nothing, and the purge driver refuses that reading"
first_reading_secs 200 reading
old_stream_rows
replace_row subjects "notifications.agent.> 0" "$(line subjects "notifications.agent.> 0" 0 "$(subjects_page)" "")"
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "UNKNOWN: (b) the consumers: the stream's subjects under notifications.agent.> read none, so the read saw nothing and its old-form messages are not known"
expect_line "UNKNOWN: (c) the stream: the stream's subjects under notifications.agent.> read none, so the read saw nothing"
[ "$(jq -c '.facts | {b, c}' "$FAKE/record.json")" = '{"b":"unknown","c":"unknown"}' ] || fail "the record's facts are $(jq -c .facts "$FAKE/record.json")"
purge_refuses "(b) the consumers" unknown
done_case

# census_page SERVER OFFSET LIMIT TOTAL [CONNECTION...] prints one server's census answer for the
# page asked at OFFSET.
census_page() {
  local server=$1 offset=$2 limit=$3 total=$4
  shift 4
  printf '%s\n' "$@" | jq -cs --arg s "$server" --argjson o "$offset" --argjson l "$limit" --argjson t "$total" 'map(select(. != null)) as $c
    | {server: {name: $s, id: "N\($s)"}, error: null, data: {total: $t, offset: $o, limit: $l, connections: $c}}'
}

# A server's list moved under the census: between its first two pages, connections 12 and 13
# closed below the page read and 17 and 18 opened above it, so the count stayed 6 and every later
# connection moved down two places. Connection 14, an older client's, moved onto the page already
# read and is on no page; the second page, asked from the first's last connection, begins with 15
# and not 13, which is how the shift is read.
new_case "(a2) a census whose list moved under it at an unchanged count is UNKNOWN"
first_reading_secs 200 reading
replace_row census 0 \
  "$(line census 0 0 "$(census_page envoy-nats-0 0 3 6 "$(conn 11 "envoy legion@$new_form")" "$(conn 12 "omp-ses_a @sjawhar/pi-envoy@21.0.0")" "$(conn 13 "dispatch legion@$new_form")"; clean_census | sed '1d')" "")" \
  "$(line census 2 0 "$(census_page envoy-nats-0 2 3 6 "$(conn 15 "@CENSUS_NAME@" UCENSUS)" "$(conn 16 "legion-daemon ACME legion@$newer")" "$(conn 17 "omp-ses_c @sjawhar/pi-envoy@21.0.0")"; census_page envoy-nats-1 2 3 2; census_page envoy-nats-2 2 3 1)" "")" \
  "$(line census 4 0 "$(census_page envoy-nats-0 4 3 6 "$(conn 17 "omp-ses_c @sjawhar/pi-envoy@21.0.0")" "$(conn 18 "omp-ses_d @sjawhar/pi-envoy@21.0.0")"; census_page envoy-nats-1 4 3 2; census_page envoy-nats-2 4 3 1)" "")"
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a2) the running clients: hub server envoy-nats-0's census page at offset 2 begins with connection 15, not 13, the last of the page before, so the list moved between the two reads and a connection may have been passed over; read again"
[ "$(jq -r .facts.a2 "$FAKE/record.json")" = unknown ] || fail "the record's (a2) is $(jq -r .facts.a2 "$FAKE/record.json"), want unknown"
done_case

# --- (a3) the publishers --------------------------------------------------------------------------
# The reading the purge exists for, with the listener's user mapped to no pin: (a3) is unknown, and
# the purge driver refuses it. The record keeps the map as the operator gave it.
new_case "(a3) a hub user no --publisher maps is UNKNOWN, and the purge driver refuses that reading"
facts=(--deployment example/deploy --new-form-commit "$new_form" --hub-statefulset envoy-nats --broker dispatch/envoy-broker
  --publisher dispatch=dispatch --publisher dispatch=dispatch-server)
first_reading_secs 200 reading
old_stream_rows
reading --since "$FAKE/earlier.json"
expect_line "UNKNOWN: (a3) the publishers: listener may publish a notification subject, and no --publisher maps it to a listener pin"
[ "$(jq -c '.facts | {a1, a2, a3, b}' "$FAKE/record.json")" = '{"a1":"true","a2":"true","a3":"unknown","b":"true"}' ] ||
  fail "the record's facts are $(jq -c .facts "$FAKE/record.json")"
[ "$(jq -c '.facts.publishers.map' "$FAKE/record.json")" = '[{"identity":"dispatch","key":"dispatch","verdict":"pass"},{"identity":"dispatch","key":"dispatch-server","verdict":"pass"}]' ] ||
  fail "the record's map is $(jq -c .facts.publishers.map "$FAKE/record.json")"
purge_refuses "(a3) the publishers" unknown
done_case

new_case "(a3) a user mapped to a listener pin the census of listener pins shut is false"
printf '%s\n' "NO VM HUB: envoy/ at eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee declares no gcp:NatsHub" \
  "GATE SHUT: envoy-cluster runs cccccccccccccccccccccccccccccccccccccccc (behind $new_form by 4)" "PASS: dispatch runs $newer (ahead of $new_form)" \
  "PASS: dispatch-server runs $newer (ahead of $new_form)" "GATE SHUT" >"$FAKE/release-gate-listeners.out"
echo 1 >"$FAKE/release-gate-listeners.rc"
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 1
expect_line "GATE SHUT: (a3) the publishers: listener runs envoy-cluster, which the census of listener pins shut: GATE SHUT: envoy-cluster runs cccccccccccccccccccccccccccccccccccccccc (behind $new_form by 4)"
expect_last "GATE SHUT"
[ "$(jq -r .facts.a3 "$FAKE/record.json")" = false ] || fail "the record's (a3) is $(jq -r .facts.a3 "$FAKE/record.json"), want false"
[ "$(jq -c '[.facts.publishers.map[] | select(.identity == "listener")]' "$FAKE/record.json")" = '[{"identity":"listener","key":"envoy-cluster","verdict":"shut"}]' ] ||
  fail "the record's map is $(jq -c .facts.publishers.map "$FAKE/record.json")"
done_case

new_case "(a3) a hub none of whose users may publish a notification subject is UNKNOWN"
hub_configmap "$FAKE" "${clean_users[1]}" "${clean_users[3]}" "${clean_users[4]}"
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a3) the publishers: no user of the hub may publish a notification subject, by its auth.conf, and a census that read nothing passes nothing"
[ "$(jq -r .facts.a3 "$FAKE/record.json")" = unknown ] || fail "the record's (a3) is $(jq -r .facts.a3 "$FAKE/record.json"), want unknown"
done_case

# Each shape of publish permission that can reach a notification subject makes its user one of the
# hub's publishers, which the map must name: a user named extra, mapped by none, is UNKNOWN. The
# clean fixture's own users hold the first shape, notifications.>.
publisher_shapes=(
  'notifications.<x>.>|{"publish": {"allow": ["notifications.agent.>"]}, "subscribe": {"allow": ["_INBOX.>"]}}'
  '*.>|{"publish": {"allow": ["*.>"]}, "subscribe": {"allow": ["_INBOX.>"]}}'
  '>|{"publish": {"allow": [">"]}, "subscribe": {"allow": ["_INBOX.>"]}}'
  'missing, which allows every subject|{"subscribe": {"allow": ["_INBOX.>"]}}'
  '> with a deny of one family alone|{"publish": {"allow": [">"], "deny": ["notifications.agent.>"]}, "subscribe": {"allow": ["_INBOX.>"]}}'
)
for shape in "${publisher_shapes[@]}"; do
  new_case "(a3) a user whose publish allow list is ${shape%%|*} may publish a notification subject, and one no --publisher maps is UNKNOWN"
  hub_configmap "$FAKE" "${clean_users[@]}" "extra=${shape#*|}"
  first_reading_secs 200 reading
  reading --since "$FAKE/earlier.json"
  expect_status 3
  expect_line "UNKNOWN: (a3) the publishers: extra may publish a notification subject, and no --publisher maps it to a listener pin"
  done_case
done

# And the shapes that reach none: a deny of every notification subject beside an allow of every
# subject, and a one-token subject, which no notification subject is. The gate opens.
# shellcheck disable=SC2016 # $JS begins a NATS subject, not a shell expansion
silent_shapes=(
  '> with a deny of notifications.>|{"publish": {"allow": [">"], "deny": ["notifications.>"]}, "subscribe": {"allow": ["_INBOX.>"]}}'
  'the one token notifications|{"publish": {"allow": ["notifications", "$JS.API.>"]}, "subscribe": {"allow": ["_INBOX.>"]}}'
)
for shape in "${silent_shapes[@]}"; do
  new_case "(a3) a user whose publish allow list is ${shape%%|*} publishes no notification subject, and the gate opens"
  hub_configmap "$FAKE" "${clean_users[@]}" "extra=${shape#*|}"
  first_reading_secs 200 reading
  reading --since "$FAKE/earlier.json"
  expect_status 0
  expect_line "PASS: (a3) the publishers: each of the 2 user(s) of the hub that may publish a notification subject"
  expect_no_line "extra"
  done_case
done

# Behind an auth callout, each entry of the auth service whose kind's grant publishes a notification
# subject (lib/legacy-subjects-gate-callout-publishers.json) is a publisher too, by the name the
# service gives its user: the edge listener's among them, since natsgrant.Edge publishes its
# projects' agent and role subjects. The edge listener's entry is mapped to envoy-cluster, the
# stack the deployment names the edge listener's replicas under (the scripts README, (a3)).
new_case "(a3) behind an auth callout, the auth service's dispatch, edge listener and listener entries are publishers, and its daemon's, measurement job's, operator's and hold probe's are not"
hub_configmap "$FAKE" --callout "${clean_users[@]}"
facts+=(--publisher listener/host-a=envoy-cluster --publisher dispatch/example-dispatch=dispatch-server --publisher edge-listener/edge-a=envoy-cluster)
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 0
expect_last "GATE OPEN"
expect_line "PASS: (a3) the publishers: each of the 5 user(s) of the hub that may publish a notification subject runs a listener pin the census passed at or after $new_form: dispatch (dispatch, dispatch-server); dispatch/example-dispatch (dispatch-server); edge-listener/edge-a (envoy-cluster); listener (envoy-cluster); listener/host-a (envoy-cluster)"
grep -qxF -- "--context example-cluster --request-timeout=60s -n dispatch get deployment envoy-broker -o json" "$FAKE/calls" ||
  fail "the auth service's Deployment was not read"
done_case

new_case "(a3) behind an auth callout, an entry of the auth service no --publisher maps is UNKNOWN"
hub_configmap "$FAKE" --callout "${clean_users[@]}"
facts+=(--publisher dispatch/example-dispatch=dispatch-server)
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a3) the publishers: listener/host-a may publish a notification subject, and no --publisher maps it to a listener pin"
expect_no_line "daemon/system:serviceaccount:legion:daemon may publish"
expect_no_line "operator/system:serviceaccount:envoy-operator:envoy-operator may publish"
expect_no_line "probe/system:serviceaccount:envoy-operator:envoy-hold-probe may publish"
expect_line "UNKNOWN: (a3) the publishers: edge-listener/edge-a may publish a notification subject, and no --publisher maps it to a listener pin"
done_case

new_case "(a3) behind an auth callout with no --broker named is UNKNOWN"
hub_configmap "$FAKE" --callout "${clean_users[@]}"
facts=(--deployment example/deploy --new-form-commit "$new_form" --hub-statefulset envoy-nats
  --publisher dispatch=dispatch --publisher dispatch=dispatch-server --publisher listener=envoy-cluster)
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a3) the publishers: the hub runs an auth callout, whose services this gate reads from the auth service's Deployment: name it with --broker <namespace>/<deployment>"
done_case

# An auth service the operator's account cannot read is UNKNOWN, never a callout with no service.
new_case "(a3) an auth service's Deployment the operator cannot read is UNKNOWN, never passed over"
hub_configmap "$FAKE" --callout "${clean_users[@]}"
touch "$FAKE/broker.fail"
facts+=(--publisher listener/host-a=envoy-cluster --publisher dispatch/example-dispatch=dispatch-server)
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a3) the publishers: the hub runs an auth callout, and the services its auth service admits could not be read: the Deployment dispatch/envoy-broker could not be read: Error from server (Forbidden): deployments.apps \"envoy-broker\" is forbidden"
done_case

new_case "(a3) an auth service whose BROKER_NATS_SERVICES is set through valueFrom is UNKNOWN"
hub_configmap "$FAKE" --callout "${clean_users[@]}"
jq '.spec.template.spec.containers[0].env[1] = {name: "BROKER_NATS_SERVICES", valueFrom: {configMapKeyRef: {name: "envoy-broker", key: "services"}}}' \
  "$FAKE/broker.json" >"$FAKE/broker.tmp" && mv "$FAKE/broker.tmp" "$FAKE/broker.json"
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a3) the publishers: the hub runs an auth callout, and the services its auth service admits could not be read: the Deployment dispatch/envoy-broker: its BROKER_NATS_SERVICES is no literal value (set through valueFrom)"
done_case

# The auth service's replicas each admit by the list they read when they started, and all answer
# the callout in one queue group, so a Deployment that has not settled on its spec may still admit,
# through an older pod, an entry the spec no longer lists. A rollout whose new pod fails its
# readiness stalls with the older pods serving: UNKNOWN, and the purge driver refuses that reading.
new_case "(a3) an auth service's Deployment whose rollout stalled is UNKNOWN, and the purge driver refuses that reading"
hub_configmap "$FAKE" --callout "${clean_users[@]}"
facts+=(--publisher listener/host-a=envoy-cluster --publisher dispatch/example-dispatch=dispatch-server --publisher edge-listener/edge-a=envoy-cluster)
jq '.metadata.generation = 8 | .status = {observedGeneration: 8, replicas: 3, updatedReplicas: 1, readyReplicas: 2, availableReplicas: 2, unavailableReplicas: 1, terminatingReplicas: 0}' \
  "$FAKE/broker.json" >"$FAKE/broker.tmp" && mv "$FAKE/broker.tmp" "$FAKE/broker.json"
first_reading_secs 200 reading
old_stream_rows
reading --since "$FAKE/earlier.json"
expect_line "UNKNOWN: (a3) the publishers: the hub runs an auth callout, and the services its auth service admits could not be read: the Deployment dispatch/envoy-broker: it is rolling out, so an older pod may still admit an entry its spec no longer lists: generation 8, observed 8; 2 replica(s) wanted, 3 running, 1 updated, 1 unavailable, 0 terminating"
[ "$(jq -c '.facts | {a1, a2, a3, b}' "$FAKE/record.json")" = '{"a1":"true","a2":"true","a3":"unknown","b":"true"}' ] ||
  fail "the record's facts are $(jq -c .facts "$FAKE/record.json")"
purge_refuses "(a3) the publishers" unknown
done_case

# Each sign of a Deployment not settled on its spec, alone, from the settled fixture.
unsettled_brokers=(
  'its controller has not yet seen its newest spec|.metadata.generation = 8'
  'a replica of an older spec still runs|.status.updatedReplicas = 1'
  'more replicas run than it wants|.status.replicas = 3'
  'a replica is unavailable|.status.unavailableReplicas = 1 | .status.availableReplicas = 1 | .status.readyReplicas = 1'
  'a pod is still terminating|.status.terminatingReplicas = 1'
)
for unsettled in "${unsettled_brokers[@]}"; do
  new_case "(a3) an auth service's Deployment in which ${unsettled%%|*} is UNKNOWN"
  hub_configmap "$FAKE" --callout "${clean_users[@]}"
  facts+=(--publisher listener/host-a=envoy-cluster --publisher dispatch/example-dispatch=dispatch-server --publisher edge-listener/edge-a=envoy-cluster)
  jq "${unsettled#*|}" "$FAKE/broker.json" >"$FAKE/broker.tmp" && mv "$FAKE/broker.tmp" "$FAKE/broker.json"
  first_reading_secs 200 reading
  reading --since "$FAKE/earlier.json"
  expect_status 3
  expect_line "UNKNOWN: (a3) the publishers: the hub runs an auth callout, and the services its auth service admits could not be read: the Deployment dispatch/envoy-broker: it is rolling out"
  done_case
done

# An entry of a kind the gate's list does not classify, which a broker newer than this checkout
# can admit, is UNKNOWN: whether its grant publishes a notification subject is not known here.
new_case "(a3) behind an auth callout, an entry of a kind the gate's list does not classify is UNKNOWN, and the purge driver refuses that reading"
hub_configmap "$FAKE" --callout "${clean_users[@]}"
facts+=(--publisher listener/host-a=envoy-cluster --publisher dispatch/example-dispatch=dispatch-server --publisher edge-listener/edge-a=envoy-cluster)
jq '.spec.template.spec.containers[0].env[1].value += " relay/relay-a/*=system:serviceaccount:relay:relay-a"' \
  "$FAKE/broker.json" >"$FAKE/broker.tmp" && mv "$FAKE/broker.tmp" "$FAKE/broker.json"
first_reading_secs 200 reading
old_stream_rows
reading --since "$FAKE/earlier.json"
expect_line "UNKNOWN: (a3) the publishers: the hub runs an auth callout, and its auth service admits entries of the kind(s) relay, which this gate's list (lib/legacy-subjects-gate-callout-publishers.json) does not classify, so whether their grant publishes a notification subject cannot be read"
[ "$(jq -c '.facts | {a1, a2, a3, b}' "$FAKE/record.json")" = '{"a1":"true","a2":"true","a3":"unknown","b":"true"}' ] ||
  fail "the record's facts are $(jq -c .facts "$FAKE/record.json")"
purge_refuses "(a3) the publishers" unknown
done_case

# A list that names kinds without classifying each, and says nothing of a session, as the list did
# before it classified every kind, is UNKNOWN behind a callout. The gate's copy of the list is
# changed for this case alone and put back after its reading.
new_case "(a3) behind an auth callout, a list of kinds that does not classify each kind and a session is UNKNOWN"
hub_configmap "$FAKE" --callout "${clean_users[@]}"
facts+=(--publisher listener/host-a=envoy-cluster --publisher dispatch/example-dispatch=dispatch-server --publisher edge-listener/edge-a=envoy-cluster)
first_reading_secs 200 reading
echo '{ "kinds": ["dispatch", "edge-listener", "listener"] }' >"$root/gate/lib/legacy-subjects-gate-callout-publishers.json"
reading --since "$FAKE/earlier.json"
cp "$here/lib/legacy-subjects-gate-callout-publishers.json" "$root/gate/lib/"
expect_status 3
expect_line "UNKNOWN: (a3) the publishers: the hub runs an auth callout, and this gate's list of the auth service's kinds (lib/legacy-subjects-gate-callout-publishers.json) does not say, kind by kind and for a session, whether its grant publishes a notification subject"
done_case

new_case "(a3) a ConfigMap of the hub the operator cannot read is UNKNOWN"
touch "$FAKE/configmap.fail"
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a3) the publishers: the hub's users could not be read: the ConfigMap envoy/envoy-nats-config the StatefulSet mounts could not be read: Error from server (Forbidden)"
done_case

# An auth.conf of another shape than the rendering this gate reads, here one with an accounts
# block, is UNKNOWN: a user the reader passed over could be a publisher.
new_case "(a3) an auth.conf of another shape, with an accounts block, is UNKNOWN"
jq '.data["auth.conf"] |= sub("\nauthorization \\{"; "\naccounts {\n  CLIENTS: {}\n}\nauthorization {")' \
  "$FAKE/configmaps/envoy-nats-config.json" >"$FAKE/configmap.tmp" && mv "$FAKE/configmap.tmp" "$FAKE/configmaps/envoy-nats-config.json"
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a3) the publishers: the ConfigMap envoy-nats-config's auth.conf is not the rendering this gate reads, so a user it holds could be passed over: line 2, accounts {, comes before its authorization block"
done_case

# A line is read only whole: nats-server ends a value at `;` and reads on, so an accounts block
# after no_auth_user's value, on its line, holds users this reader would pass over. UNKNOWN, and
# the purge driver refuses that reading.
new_case "(a3) an auth.conf line after the block that holds an accounts block after its value is UNKNOWN, and the purge driver refuses that reading"
jq --arg hidden "$(printf 'U%055d' 9)" '.data["auth.conf"] |= sub("\nno_auth_user: (?<v>\"U[0-9]{55}\")\n"; "\nno_auth_user: \(.v); accounts { HIDDEN: { users: [ {nkey: \"\($hidden)\", permissions: {publish: {allow: [\">\"]}}} ] } }\n")' \
  "$FAKE/configmaps/envoy-nats-config.json" >"$FAKE/configmap.tmp" && mv "$FAKE/configmap.tmp" "$FAKE/configmaps/envoy-nats-config.json"
first_reading_secs 200 reading
old_stream_rows
reading --since "$FAKE/earlier.json"
expect_line "UNKNOWN: (a3) the publishers: the ConfigMap envoy-nats-config's auth.conf is not the rendering this gate reads, so a user it holds could be passed over: line 17, no_auth_user: \"$(printf 'U%055d' 5)\"; accounts { HIDDEN:"
[ "$(jq -c '.facts | {a1, a2, a3, b}' "$FAKE/record.json")" = '{"a1":"true","a2":"true","a3":"unknown","b":"true"}' ] ||
  fail "the record's facts are $(jq -c .facts "$FAKE/record.json")"
purge_refuses "(a3) the publishers" unknown
done_case

new_case "(a3) an auth_callout line that holds more than its value is UNKNOWN"
hub_configmap "$FAKE" --callout "${clean_users[@]}"
facts+=(--publisher listener/host-a=envoy-cluster --publisher dispatch/example-dispatch=dispatch-server --publisher edge-listener/edge-a=envoy-cluster)
jq '.data["auth.conf"] |= sub("\n    xkey: (?<v>\"X[A-Z0-9]{55}\")\n"; "\n    xkey: \(.v), timeout: 1\n")' \
  "$FAKE/configmaps/envoy-nats-config.json" >"$FAKE/configmap.tmp" && mv "$FAKE/configmap.tmp" "$FAKE/configmaps/envoy-nats-config.json"
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "could be passed over: line 18,     xkey: \"XBVCTZK3PV5Y7TBIVYTDZBP3O2YGHUUL7MMENN4DTZIREJCPGLTAF6EL\", timeout: 1, is no line of its auth_callout block"
done_case

# nats-server reads pub as publish, and of the two keeps the one it reads last; a user's object
# with any key Render does not write is UNKNOWN, never read by the key this reader knows. Here the
# user other may publish notifications.> by pub, and publish allows only inboxes. The purge
# driver refuses that reading.
new_case "(a3) a user's object that spells publish twice, as pub and as publish, is UNKNOWN, and the purge driver refuses that reading"
hub_configmap "$FAKE" "${clean_users[@]}" 'other={"pub": {"allow": ["notifications.>"]}, "publish": {"allow": ["_INBOX.>", "agentstream.*.frames"]}, "subscribe": {"allow": ["_INBOX.>"]}}'
first_reading_secs 200 reading
old_stream_rows
reading --since "$FAKE/earlier.json"
expect_line "UNKNOWN: (a3) the publishers: the ConfigMap envoy-nats-config's auth.conf is not the rendering this gate reads, so a user it holds could be passed over: line 15, "
expect_line "is no user's JSON object as the rendering writes it: the keys nkey and permissions, publish and subscribe, allow and deny, each once, sorted and unescaped"
[ "$(jq -c '.facts | {a1, a2, a3, b}' "$FAKE/record.json")" = '{"a1":"true","a2":"true","a3":"unknown","b":"true"}' ] ||
  fail "the record's facts are $(jq -c .facts "$FAKE/record.json")"
purge_refuses "(a3) the publishers" unknown
done_case

# A key written twice: this reader keeps the last, and whichever nats-server keeps, the two could
# be read apart. The daemon's object here first allows every subject, then its own list.
new_case "(a3) a user's object that names a key twice is UNKNOWN, whichever of the two a reader keeps"
jq '.data["auth.conf"] |= sub("\\{\"nkey\": \"(?<k>U0{54}2)\", \"permissions\": \\{\"publish\": "; "{\"nkey\": \"\(.k)\", \"permissions\": {\"publish\": {\"allow\": [\">\"]}, \"publish\": ")' \
  "$FAKE/configmaps/envoy-nats-config.json" >"$FAKE/configmap.tmp" && mv "$FAKE/configmap.tmp" "$FAKE/configmaps/envoy-nats-config.json"
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "could be passed over: line 7, "
expect_line "is no user's JSON object as the rendering writes it"
done_case

# Render's own text, infra/core's golden auth.conf with a callout (its non-ASCII subject among it),
# is read whole: its three users that may publish are read beside the auth service's entries.
new_case "(a3) Render's golden auth.conf is read whole, its users that may publish beside the auth service's"
jq -n --rawfile auth "$here/../infra/core/testdata/authorization-callout.golden" \
  '{metadata: {name: "envoy-nats-config"}, data: {"nats.conf": "include auth.conf\n", "auth.conf": $auth}}' >"$FAKE/configmaps/envoy-nats-config.json"
facts=(--deployment example/deploy --new-form-commit "$new_form" --hub-statefulset envoy-nats --broker dispatch/envoy-broker
  --publisher example-hub=envoy-cluster --publisher example-dispatch=dispatch-server --publisher example-sessions=envoy-cluster
  --publisher listener/host-a=envoy-cluster --publisher dispatch/example-dispatch=dispatch-server --publisher edge-listener/edge-a=envoy-cluster)
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 0
expect_last "GATE OPEN"
expect_line "PASS: (a3) the publishers: each of the 6 user(s) of the hub that may publish a notification subject runs a listener pin the census passed at or after $new_form: dispatch/example-dispatch (dispatch-server); edge-listener/edge-a (envoy-cluster); example-dispatch (dispatch-server); example-hub (envoy-cluster); example-sessions (envoy-cluster); listener/host-a (envoy-cluster)"
done_case

# BSD's and macOS's wc -l pad the count to eight columns: the one ConfigMap that holds auth.conf
# is still read as one.
new_case "(a3) a count of the ConfigMaps that hold auth.conf as BSD wc pads it is read as the number it is"
mkdir -p "$FAKE/bsdbin"
cat >"$FAKE/bsdbin/wc" <<'EOF'
#!/usr/bin/env bash
command -p wc "$@" | awk '{ $1 = sprintf("%8d", $1); print }'
EOF
chmod +x "$FAKE/bsdbin/wc"
[ "$("$FAKE/bsdbin/wc" -l <<<x)" = "       1" ] || fail "the fake BSD wc counts one line as '$("$FAKE/bsdbin/wc" -l <<<x)', not padded"
first_reading_secs 200 reading
PATH="$FAKE/bsdbin:$PATH" reading --since "$FAKE/earlier.json"
expect_status 0
expect_line "PASS: (a3) the publishers: each of the 2 user(s) of the hub that may publish a notification subject"
done_case

new_case "(a3) a hub server outside the StatefulSet, whose users cannot be read, is UNKNOWN"
facts+=(--hub-server envoy-vm)
first_reading_secs 200 reading
reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: (a3) the publishers: the hub server(s) envoy-vm that --hub-server names run outside the StatefulSet, whose users this gate cannot read"
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

# A NATS_URLS may carry a user and password, or a token, with a scheme or with none: the SKIP line
# prints each masked, the listener Deployment's as well as the skipped listener's. A nats CLI error
# names the server it is connected to by its URL, the Deployment's: that line masks it too.
new_case "every line masks a URL's user, password and token: a SKIP line's NATS_URLS and a nats CLI error's server"
bus_url_with_password="nats://bus-user:bus-pass-1e8f${at}envoy-hub.envoy.svc.cluster.local:4222"
jq --arg urls "$bus_url_with_password" \
  '(.spec.template.spec.containers[0].env[] | select(.name == "NATS_URLS") | .value) = $urls' "$base/deployment.json" >"$FAKE/deployment.json"
jq --arg old "$bus_url" --arg urls "$bus_url_with_password" \
  --argjson p1 "$(pod loadtest envoy-listener-lt1 lt-uid-1 envoy-listener "$envoy_image" null 0 running lt-1 "nats://loadtest-nats-a:4222,lt-user:lt-pass-7c1d${at}loadtest-nats.loadtest.svc.cluster.local:4222")" \
  --argjson p2 "$(pod loadtest envoy-listener-lt2 lt-uid-2 envoy-listener "$envoy_image" null 0 running lt-2 nats://lt-token-4b2a@loadtest-nats.loadtest.svc.cluster.local:4222)" \
  '(.items[].spec.containers[].env[] | select(.name == "NATS_URLS" and .value == $old) | .value) = $urls | .items += [$p1, $p2]' "$base/pods.json" >"$FAKE/pods.json"
replace_row user_info - "$(line user_info - 0 "" "04:10:18 Unexpected NATS error from server $bus_url_with_password: nats: permissions violation: Permissions Violation for Publish to \"\$SYS.REQ.USER.INFO\"")"
reading
expect_status 4
expect_line "SKIP: listener pod loadtest/envoy-listener-lt1 container envoy-listener is on another bus (NATS_URLS nats://loadtest-nats-a:4222,lt-user:***@loadtest-nats.loadtest.svc.cluster.local:4222), not the listener Deployment's nats://bus-user:***@envoy-hub.envoy.svc.cluster.local:4222, and is not read (machine lt-1;"
expect_line "SKIP: listener pod loadtest/envoy-listener-lt2 container envoy-listener is on another bus (NATS_URLS nats://***@loadtest-nats.loadtest.svc.cluster.local:4222), not the listener Deployment's nats://bus-user:***@envoy-hub.envoy.svc.cluster.local:4222, and is not read (machine lt-2;"
expect_line "IDENTITY: the server did not say whom the NATS reads ran as (the request \$SYS.REQ.USER.INFO: answered with nothing: 04:10:18 Unexpected NATS error from server nats://bus-user:***@envoy-hub.envoy.svc.cluster.local:4222: nats: permissions violation"
for secret in bus-pass-1e8f lt-pass-7c1d lt-token-4b2a; do
  expect_no_line "$secret"
  ! grep -qF -- "$secret" "$FAKE/record.json" || fail "the record holds $secret"
done
done_case

# A hub moved into the cluster is one bus by two names: Service envoy-hub, which the listener
# Deployment dials, selects the hub's own pods, and the hub's session listener, which serves as
# machine envoy-hub and is bound to its durable listener-envoy-hub, dials the hub's own Service.
# The listener outside the cluster that served as envoy-hub before has left the bus, so these
# readings name no --listener.
readonly hub_service_url="nats://envoy-nats.envoy.svc.cluster.local:4222"

# moved_cluster adds the hub's session listener, on the hub's own Service, to the case's cluster.
moved_cluster() {
  jq --argjson p "$(pod envoy envoy-nats-listener-5c7d hub-listener-uid-1 listener "$envoy_image" null 0 running envoy-hub "$hub_service_url")" \
    '.items += [$p]' "$base/pods.json" >"$FAKE/pods.json"
  metrics 7 0 "$hub_start" 0 envoy-hub >"$FAKE/metrics/envoy.envoy-nats-listener-5c7d"
}

# moved_reading [ARG...] runs the gate as a reading of the case, naming no listener outside the
# cluster, writing $FAKE/record.json.
moved_reading() {
  local extra=()
  case " $* " in *" --since "*) extra=("${facts[@]}") ;; esac
  run_gate --context example-cluster --record "$FAKE/record.json" "$@" ${extra[@]+"${extra[@]}"}
}

new_case "a listener that dials the bus by another name is skipped, its durable UNKNOWN, until --bus-alias names that name"
moved_cluster
moved_reading
expect_status 3
expect_line "SKIP: listener pod envoy/envoy-nats-listener-5c7d container listener is on another bus (NATS_URLS $hub_service_url), not the listener Deployment's $bus_url, and is not read (machine envoy-hub; if its NATS_URLS reaches the same NATS servers, name it with --bus-alias)"
expect_line "UNKNOWN: listener machine envoy-hub is bound to its durable listener-envoy-hub on the bus, and no listener this reading read serves as that machine: name it with --listener envoy-hub=<url> if it runs outside the cluster, or, if it runs in the cluster and dials the bus by another address, name that NATS_URLS with --bus-alias"
! grep -qF -- "-n envoy logs envoy-nats-listener-5c7d" "$FAKE/calls" || fail "the listener on the hub's own Service was read with no --bus-alias"
rm -f "$FAKE/record.json"
: >"$FAKE/calls"
moved_reading --bus-alias "$hub_service_url"
expect_status 4
expect_line "READ: listener pod envoy/envoy-nats-listener-5c7d container listener: {kind=\"interest\"} 7 since its start $(jq -rn --argjson s "$hub_start" '$s | todate')"
expect_no_line "SKIP: listener pod envoy/envoy-nats-listener-5c7d"
expect_no_line "listener machine envoy-hub"
grep -qF -- "-n envoy logs envoy-nats-listener-5c7d -c listener --since=5m" "$FAKE/calls" || fail "the listener on the hub's own Service was not read from its log"
[ "$(jq -c '[.listeners[].machine] | sort' "$FAKE/record.json")" = '["dispatch","envoy-hub","gke"]' ] ||
  fail "the record holds the listeners of machines $(jq -c '[.listeners[].machine] | sort' "$FAKE/record.json"), want dispatch, envoy-hub and gke"
done_case

new_case "--bus-alias reads no listener on a bus it does not name"
moved_cluster
jq --argjson p "$(pod loadtest envoy-listener-lt1 lt-uid-1 envoy-listener "$envoy_image" null 0 running lt-1 nats://loadtest-nats.loadtest.svc.cluster.local:4222)" \
  '.items += [$p]' "$FAKE/pods.json" >"$FAKE/pods.json.new"
mv "$FAKE/pods.json.new" "$FAKE/pods.json"
moved_reading --bus-alias "$hub_service_url"
expect_status 4
expect_line "SKIP: listener pod loadtest/envoy-listener-lt1 container envoy-listener is on another bus (NATS_URLS nats://loadtest-nats.loadtest.svc.cluster.local:4222), not the listener Deployment's $bus_url or a --bus-alias ($hub_service_url), and is not read (machine lt-1;"
! grep -qF -- "-n loadtest logs" "$FAKE/calls" || fail "the listener on another bus was read"
done_case

new_case "a moved cluster read twice, 73 hours apart, with --bus-alias naming the hub's own Service, opens"
moved_cluster
first_reading_with 73 moved_reading --bus-alias "$hub_service_url"
moved_reading --since "$FAKE/earlier.json" --bus-alias "$hub_service_url"
expect_status 0
expect_last "GATE OPEN"
expect_line "PASS: listener pod envoy/envoy-nats-listener-5c7d container listener counted no old-form name between the readings"
done_case

new_case "a second reading that drops the first reading's --bus-alias is UNKNOWN"
moved_cluster
first_reading_with 73 moved_reading --bus-alias "$hub_service_url"
moved_reading --since "$FAKE/earlier.json"
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-5c7d container listener served at the first reading and was not read now"
expect_last "UNKNOWN"
done_case

# A listener read through --bus-alias that restarted between the readings is caught as any
# listener in the cluster is: by its pod's restart count, its start-time gauge, or a new pod.
# hub_pod_set JQ sets the hub's session listener's pod in the case's cluster by the jq update JQ.
hub_pod_set() {
  jq "(.items[] | select(.metadata.name == \"envoy-nats-listener-5c7d\")) |= ($1)" "$FAKE/pods.json" >"$FAKE/pods.json.new"
  mv "$FAKE/pods.json.new" "$FAKE/pods.json"
}

new_case "a listener read through --bus-alias that restarted between the readings, as Kubernetes shows it, is UNKNOWN"
moved_cluster
first_reading_with 73 moved_reading --bus-alias "$hub_service_url"
hub_pod_set '.status.containerStatuses[0].restartCount = 1'
metrics 0 0 "$((hub_start + 100000))" 0 envoy-hub >"$FAKE/metrics/envoy.envoy-nats-listener-5c7d"
moved_reading --since "$FAKE/earlier.json" --bus-alias "$hub_service_url"
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-5c7d container listener restarted between the readings (restart count 0 then 1): its counter covers only the time since"
expect_last "UNKNOWN"
done_case

new_case "a listener read through --bus-alias whose restart count alone moved between the readings is UNKNOWN"
moved_cluster
first_reading_with 73 moved_reading --bus-alias "$hub_service_url"
hub_pod_set '.status.containerStatuses[0].restartCount = 1'
moved_reading --since "$FAKE/earlier.json" --bus-alias "$hub_service_url"
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-5c7d container listener restarted between the readings (restart count 0 then 1): its counter covers only the time since"
expect_last "UNKNOWN"
done_case

new_case "a listener read through --bus-alias whose start-time gauge alone moved between the readings is UNKNOWN"
moved_cluster
first_reading_with 73 moved_reading --bus-alias "$hub_service_url"
metrics 7 0 "$((hub_start + 100000))" 0 envoy-hub >"$FAKE/metrics/envoy.envoy-nats-listener-5c7d"
moved_reading --since "$FAKE/earlier.json" --bus-alias "$hub_service_url"
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-5c7d container listener restarted between the readings: its start-time gauge read $hub_start then $((hub_start + 100000))"
expect_no_line "restart count"
expect_last "UNKNOWN"
done_case

new_case "a listener read through --bus-alias that is a new pod at the second reading is UNKNOWN"
moved_cluster
first_reading_with 73 moved_reading --bus-alias "$hub_service_url"
hub_pod_set '.metadata.name = "envoy-nats-listener-8e2f" | .metadata.uid = "hub-listener-uid-2"'
mv "$FAKE/metrics/envoy.envoy-nats-listener-5c7d" "$FAKE/metrics/envoy.envoy-nats-listener-8e2f"
moved_reading --since "$FAKE/earlier.json" --bus-alias "$hub_service_url"
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-8e2f container listener was not read at the first reading"
expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-5c7d container listener served at the first reading and was not read now"
expect_last "UNKNOWN"
done_case

# A listener on another bus that serves as the machine of a listener read: an operator who names
# the load-test stack's bus with --bus-alias, and not the hub's own Service, while a load-test
# listener runs as machine envoy-hub, reads the load-test listener as envoy-hub, so the durable
# listener-envoy-hub looks read, and the hub's session listener, bound to it, is never read.
readonly loadtest_url="nats://loadtest-nats.loadtest.svc.cluster.local:4222"
new_case "a listener on another bus that serves as the machine of a listener read is UNKNOWN, naming both"
moved_cluster
jq --argjson p "$(pod loadtest envoy-listener-lt1 lt-uid-1 envoy-listener "$envoy_image" null 0 running envoy-hub "$loadtest_url")" \
  '.items += [$p]' "$FAKE/pods.json" >"$FAKE/pods.json.new"
mv "$FAKE/pods.json.new" "$FAKE/pods.json"
metrics 5 0 "$listener_start" 0 envoy-hub >"$FAKE/metrics/loadtest.envoy-listener-lt1"
moved_reading --bus-alias "$loadtest_url"
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-5c7d container listener, on another bus (NATS_URLS $hub_service_url) and not read, serves as machine envoy-hub, as listener pod loadtest/envoy-listener-lt1 container envoy-listener, which this reading read, does: the durable listener-envoy-hub on the bus can be either's"
expect_last "UNKNOWN"
# A second reading does not open on it either, whatever the first reading's record says: here it
# says complete, as a gate without this rule wrote it, and the hub's session listener, never read,
# counted an old-form name between the readings.
jq --argjson s "$((73 * 3600))" '.verdict = "complete" | .unknown = [] | .started_at -= $s | .finished_at -= $s
  | .read_at = (.started_at | todate) | .listeners |= map(.read_at -= $s)' "$FAKE/record.json" >"$FAKE/earlier.json"
rm -f "$FAKE/record.json"
metrics 8 0 "$hub_start" 0 envoy-hub >"$FAKE/metrics/envoy.envoy-nats-listener-5c7d"
moved_reading --since "$FAKE/earlier.json" --bus-alias "$loadtest_url"
expect_status 3
expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-5c7d container listener, on another bus (NATS_URLS $hub_service_url) and not read, serves as machine envoy-hub"
expect_no_line "GATE OPEN"
done_case

# A listener on another bus whose ENVOY_MACHINE_ID is no literal value: the gate cannot read its
# machine, so whether a listener the reading read serves as that machine cannot be told.
new_case "a listener on another bus whose ENVOY_MACHINE_ID is no literal value is UNKNOWN, naming it"
jq --argjson p1 "$(pod loadtest envoy-listener-lt1 lt-uid-1 envoy-listener "$envoy_image" null 0 running lt-1 "$loadtest_url" |
  jq '.spec.containers[0].env |= map(if .name == "ENVOY_MACHINE_ID" then {name, valueFrom: {fieldRef: {fieldPath: "metadata.name"}}} else . end)')" \
  --argjson p2 "$(pod loadtest envoy-listener-lt2 lt-uid-2 envoy-listener "$envoy_image" null 0 running "" "$loadtest_url" |
    jq '.spec.containers[0].envFrom = [{configMapRef: {name: "loadtest-listener-env"}}]')" \
  --argjson p3 "$(pod loadtest envoy-listener-lt3 lt-uid-3 envoy-listener "$envoy_image" null 0 running "" "$loadtest_url")" \
  '.items += [$p1, $p2, $p3]' "$base/pods.json" >"$FAKE/pods.json"
reading
expect_status 3
expect_line "UNKNOWN: listener pod loadtest/envoy-listener-lt1 container envoy-listener, on another bus (NATS_URLS $loadtest_url) and not read, sets ENVOY_MACHINE_ID through valueFrom, so its machine cannot be read and whether a listener this reading read serves as that machine cannot be told; set ENVOY_MACHINE_ID on it as a literal value"
expect_line "UNKNOWN: listener pod loadtest/envoy-listener-lt2 container envoy-listener, on another bus (NATS_URLS $loadtest_url) and not read, sets no ENVOY_MACHINE_ID of its own and takes envFrom, which may set it, so its machine cannot be read"
expect_line "UNKNOWN: listener pod loadtest/envoy-listener-lt3 container envoy-listener, on another bus (NATS_URLS $loadtest_url) and not read, sets no ENVOY_MACHINE_ID, so its machine cannot be read"
expect_last "UNKNOWN"
! grep -qF -- "-n loadtest logs" "$FAKE/calls" || fail "a listener on another bus was read"
done_case

# A listener on another bus whose ENVOY_MACHINE_ID, as the pod spec writes it, is not the machine
# it runs as: the listener trims the value (internal/config Load), and Kubernetes expands
# $(VAR_NAME) in it from the variables before it (EnvVar.value). Each form is the case above with
# the hub's session listener's machine written that way: it runs as envoy-hub all the same, so the
# load-test listener read as envoy-hub leaves it unread, and the reading must not count as a first.
new_case "a listener on another bus whose ENVOY_MACHINE_ID as written is not the machine it runs as is UNKNOWN"
# shellcheck disable=SC2016 # $(HUB_MACHINE) is Kubernetes' reference, not the shell's
for form in ' envoy-hub' 'envoy-hub ' '$(HUB_MACHINE)'; do
  rm -f "$FAKE/record.json"
  moved_cluster
  jq --arg form "$form" --argjson p "$(pod loadtest envoy-listener-lt1 lt-uid-1 envoy-listener "$envoy_image" null 0 running envoy-hub "$loadtest_url")" '
    .items += [$p]
    | (.items[] | select(.metadata.name == "envoy-nats-listener-5c7d") | .spec.containers[0].env)
      |= ([{name: "HUB_MACHINE", value: "envoy-hub"}] + map(if .name == "ENVOY_MACHINE_ID" then .value = $form else . end))' \
    "$FAKE/pods.json" >"$FAKE/pods.json.new"
  mv "$FAKE/pods.json.new" "$FAKE/pods.json"
  metrics 5 0 "$listener_start" 0 envoy-hub >"$FAKE/metrics/loadtest.envoy-listener-lt1"
  moved_reading --bus-alias "$loadtest_url"
  expect_status 3
  expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-5c7d container listener"
  expect_no_line "FIRST READING"
done
done_case

# The lines each form prints: a value with white space around it is read as the listener trims it,
# so it is the collision of the rule above; a value holding a $(VAR) reference cannot be read.
new_case "a machine written with white space is read trimmed, and one with a \$(VAR) reference is named as such"
# shellcheck disable=SC2016 # $(HUB_MACHINE) is Kubernetes' reference, not the shell's
for form in ' envoy-hub' '$(HUB_MACHINE)'; do
  rm -f "$FAKE/record.json"
  moved_cluster
  jq --arg form "$form" --argjson p "$(pod loadtest envoy-listener-lt1 lt-uid-1 envoy-listener "$envoy_image" null 0 running envoy-hub "$loadtest_url")" '
    .items += [$p]
    | (.items[] | select(.metadata.name == "envoy-nats-listener-5c7d") | .spec.containers[0].env)
      |= ([{name: "HUB_MACHINE", value: "envoy-hub"}] + map(if .name == "ENVOY_MACHINE_ID" then .value = $form else . end))' \
    "$FAKE/pods.json" >"$FAKE/pods.json.new"
  mv "$FAKE/pods.json.new" "$FAKE/pods.json"
  metrics 5 0 "$listener_start" 0 envoy-hub >"$FAKE/metrics/loadtest.envoy-listener-lt1"
  moved_reading --bus-alias "$loadtest_url"
  expect_status 3
  if [ "$form" = ' envoy-hub' ]; then
    expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-5c7d container listener, on another bus (NATS_URLS $hub_service_url) and not read, serves as machine envoy-hub, as listener pod loadtest/envoy-listener-lt1 container envoy-listener, which this reading read, does"
  else
    expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-5c7d container listener, on another bus (NATS_URLS $hub_service_url) and not read, sets ENVOY_MACHINE_ID with a \$(VAR) reference, which Kubernetes expands, so its machine cannot be read"
  fi
done
done_case

# A container that lists ENVOY_MACHINE_ID or NATS_URLS twice: the kubelet takes the last entry and
# the gate the first. With the machine listed as hub-old then envoy-hub, the hub's session
# listener runs as envoy-hub while the gate skipped it as hub-old, and the load-test listener read
# as envoy-hub stood in for it (the tester's M3); with NATS_URLS listed twice, the bus it dials is
# not the one the gate placed it on. Each is UNKNOWN, naming every value.
new_case "a listener that lists ENVOY_MACHINE_ID or NATS_URLS twice is UNKNOWN, naming both values"
for name in ENVOY_MACHINE_ID NATS_URLS; do
  rm -f "$FAKE/record.json"
  moved_cluster
  if [ "$name" = ENVOY_MACHINE_ID ]; then
    update='map(if .name == "ENVOY_MACHINE_ID" then .value = "hub-old" else . end) + [{name: "ENVOY_MACHINE_ID", value: "envoy-hub"}]'
    want="lists ENVOY_MACHINE_ID 2 times (hub-old, envoy-hub)"
  else
    # shellcheck disable=SC2016 # $lt is the jq program's variable, not the shell's
    update='. + [{name: "NATS_URLS", value: $lt}]'
    want="lists NATS_URLS 2 times ($hub_service_url, $loadtest_url)"
  fi
  jq --arg lt "$loadtest_url" --argjson p "$(pod loadtest envoy-listener-lt1 lt-uid-1 envoy-listener "$envoy_image" null 0 running envoy-hub "$loadtest_url")" "
    .items += [\$p]
    | (.items[] | select(.metadata.name == \"envoy-nats-listener-5c7d\") | .spec.containers[0].env) |= ($update)" \
    "$FAKE/pods.json" >"$FAKE/pods.json.new"
  mv "$FAKE/pods.json.new" "$FAKE/pods.json"
  metrics 5 0 "$listener_start" 0 envoy-hub >"$FAKE/metrics/loadtest.envoy-listener-lt1"
  moved_reading --bus-alias "$loadtest_url"
  expect_status 3
  expect_line "UNKNOWN: listener pod envoy/envoy-nats-listener-5c7d container listener $want: the kubelet takes the last entry of a name and this reading the first"
  expect_no_line "FIRST READING"
done
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
