#!/usr/bin/env bash
# packages/envoy/scripts/legacy-subjects-gate.sh --context <kube-context> --record <file>
#   [--since <earlier record>] [--listener <machine>=<url>]... [--insecure]
#   [--bus-alias <nats-urls>]... [--token-file <path>] [--inbox-prefix <prefix>]
#
# Says whether any reader or writer of the bus still uses the old form of a name the rename moved
# (an issue event named by its key alone, an agent's or a role's name with no project, a kept lane
# with no project, and a failed-delivery report of one), the reading a pull request quotes before it
# refuses the old form and deletes the code that reads it.
# One reading cannot say that nothing moved, so the gate is read twice, at least the stream's
# retention apart (its max_age, 72 hours): the first reading is written to --record, and the second
# names it with --since. It prints GATE OPEN only at a second reading, when both readings read every
# surface, neither found an old-form name, and no listener counted one in between.
#
# What it reads, all of it read-only:
#
#   - Each listener's envoy_listener_legacy_subjects_total samples (every label set: an interest a
#     registration named, a role a claim named, and a valid role a publish, the role lane or a
#     holder lookup named) and its start-time gauge envoy_listener_start_time_seconds, whose one
#     label, machine, is its ENVOY_MACHINE_ID. The listeners in the cluster are found by what they
#     run, not by name: every container, in any namespace, of an image whose path ends legion/envoy
#     that runs the image's own entrypoint (envoy-listener; a command of envoy-listener counts too,
#     and any other command is the Dispatch server, the broker or a helper), on the bus the reading
#     is about: the NATS_URLS of the envoy-listener Deployment, or another NATS_URLS the operator
#     names with --bus-alias as an address of the same NATS servers (a hub in the cluster that the
#     Service envoy-listener dials and a Service of the hub's own both select, so a listener that
#     dials the second is on the bus). A listener container on another bus (a load-test stack's, on
#     its own NATS) is named (SKIP), with its machine and its NATS_URLS, any credential in it
#     masked, and not read. Each is read from its pod's log: the listener logs the two metrics, as
#     /metrics serves them, on a line "listener legacy subjects" every minute (cmd/listener
#     startLegacySubjectsLog), and the gate takes the newest from the last five minutes, which must
#     be at most three minutes older than the
#     reading. The log and not /metrics, because the operator may read every listener pod's log and
#     may not reach Dispatch's listener's /metrics: the API server's pod proxy is refused in that
#     namespace, and the listener's NetworkPolicy admits its port from Dispatch's own pods alone. A
#     listener outside the cluster, such as the VM hub's, is named with --listener <machine>=<url>
#     and read at <url>/metrics, over https unless --insecure is given. Each reading is held to the
#     machine it was read as: the start-time gauge of a listener outside the cluster must name the
#     machine the operator gave, and a pod's must name its container's ENVOY_MACHINE_ID when it names
#     one, so a URL serving another listener is UNKNOWN, never that machine's count. Every listener
#     with its durable on the bus (listener-<machine>, bus.ListenerDurable), bound now or not, must
#     be one of those: one that is not is UNKNOWN by name, so a listener nobody named, or one offline
#     at the reading, is never left out of the count while its durable lasts.
#   - Over NATS, from a one-off Job in namespace envoy that connects as the in-cluster listener
#     does (lib/legacy-subjects-gate-nats.sh): who the server says the connection is, the stream's
#     info, every consumer of the stream, the stream's subjects under notifications.dispatch.issue.>,
#     notifications.agent.>, notifications.envoy.exceptions.notifications.agent.> and
#     notifications.envoy.kept.>, the last message (subject, stream sequence and time) of each of
#     those subjects in an old form, and every record of the interest store envoy_interests.
#
# The first reading shuts on no old-form name: the stream still holds the old-form messages
# published before every producer moved, for its retention, and interest records and consumer
# filters may still be in an old form. It names each one it finds (FOUND), and records the stream's
# last sequence, the newest old-form message and every old-form counter. What holds the gate shut at
# the second reading, at least the stream's retention later:
#
#   - any old-form message the stream still holds: the stream keeps the window, so a clean second
#     reading says it is wholly new-form; one published after the first reading's last stream
#     sequence (its subject's last message, read by last_by_subj, is later) is named apart;
#   - a listener whose count rose since the first reading, under any kind;
#   - a consumer one of whose filters only an old-form name matches, and whose new form the same
#     consumer does not also filter under every project its names carry (a session consumer filters
#     both forms of each topic until the old form is refused, so its old filter loses nothing; a
#     filter that is the old form alone, a Legion daemon's legion-go-<project>-<kind> or any other,
#     would go deaf once publishers stop, and so would one beside a new form under one project of
#     names that carry any); an issue filter is one that reaches the issue family, through a
#     wildcard in the family's own tokens too (notifications.dispatch.*.*.comment.created);
#   - an interest record holding an old-form topic other than its session's own agent subject,
#     which the listener writes into every record itself until the old form is refused;
#   - a reading named with --since that shut the gate, or less than the retention between the two
#     readings (for each listener, between the two moments its counter was read).
#
# The old form is the gate's own rule (lib/legacy-subjects-gate-forms.jq, old_form), so the gate
# stays right once the code that reads the old form is deleted.
#
# What it cannot see is UNKNOWN, by name, and UNKNOWN is never GATE OPEN: a surface it could not
# read or that answered nothing (no listener, no Legion daemon consumer, no issue or agent subject
# on the stream, no interest record: a production bus always holds them), a refused read named by
# its request, a list that changed between the pages it was read in, an answer that is no JSON
# object, a listener that is not running, a listener build that logs or serves no old-form counter,
# a reading whose start-time gauge names another machine than the one read, or none for a listener
# outside the cluster, a listener that restarted between the readings (its pod's restart count or
# its start-time gauge moved, or it is a new pod), a listener of the first reading that is gone, a
# listener on another bus that serves as the machine of a listener read (that machine's durable can
# be either's) or whose ENVOY_MACHINE_ID is no literal value (its machine cannot be read), a
# start-time gauge served at one reading only, a record that cannot be written, and
# at a second reading: a first reading that did not read every surface, a stream that is not the
# first reading's (its envoy_stream_id changed), a stream that does not hold the whole window (its
# first message is younger than the window, less an hour), and readings further apart than a
# listener's durable outlives its listener.
#
# The Job: namespace envoy, the ServiceAccount, NATS URL and credential of the listener Deployment
# envoy/envoy-listener as it is deployed (read at each run, so the Job connects with whatever
# credential the listener holds): its NATS seed file (NATS_NKEY_SEED_FILE) and the volume holding
# it, or, with --token-file <path>, the listener's projected NATS token at that path in the
# Deployment and the volume holding it, which a hub that is not static takes in place of a seed;
# with --inbox-prefix <prefix>, every nats call of the Job takes that inbox prefix, the only one
# such a hub's grant lets the identity subscribe. It runs the nats-box image the deployment pins
# for its own NATS Jobs, as nobody (65534) with every capability dropped, no Kubernetes API token
# mounted, a scratch directory in memory, backoffLimit 0, activeDeadlineSeconds 300 and
# ttlSecondsAfterFinished 600; labels app.kubernetes.io/name and created-by legacy-subjects-gate
# and owner <your login> (unknown for a uid with no name). The gate waits for it to finish, reads
# its log, deletes it and reads back that no Job or pod of its name is left; one it could not
# delete is UNKNOWN and named with the command that deletes it, and one it could not create is
# never deleted. The Job's log holds what the gate judges and no more: of an interest record, its
# session id and its topics; of a stored message, its subject, stream sequence and time; of a seed
# nsc could not read, a fixed line. The cluster keeps a container's log in its logging backend as
# it keeps the listener's own, which logs every topic it delivers, so the Job adds no kind of text
# the namespace does not already log.
#
# It prints the identity the NATS reads ran as (`IDENTITY`), one line per surface, `READ`, `SKIP` (a
# listener on another bus, with its machine), `PASS`, `FOUND` (an old-form name a first reading
# found), `GATE SHUT` or `UNKNOWN`, the Job's creation and
# deletion, and the verdict alone on the last line: `GATE OPEN` (exit 0) at a clean second reading,
# `GATE SHUT` (exit 1) when anything shut it, `UNKNOWN` (exit 3) otherwise when anything was not
# read, and at a first reading that read every surface `FIRST READING`, naming how many old-form
# names it found, the stream sequence the second reading reads after, and when to read again (exit
# 4). It writes the reading to --record in every case, as a new file in a directory that exists
# when the gate starts: a file made at that path meanwhile is left as it is, and the reading is
# UNKNOWN. A second reading that opened is a first reading for the next window. A usage error exits
# 2 and reads nothing. No line of a reading prints a URL's user, password or token (user:***@,
# ***@ in its place), whether a NATS_URLS or a nats error naming its server holds it.
#
# It needs bash, kubectl (a context for the cluster that runs the listeners and the hub bridge,
# signed in as an account that may list pods in every namespace, read the logs of the listener
# pods in each namespace that runs one, read the envoy-listener Deployment, and create, read, read
# the logs of and delete Jobs and pods in envoy: a developer box's account holds list through its
# container.viewer role, and the rest through its edit role in envoy and its pods/log read in
# dispatch), jq, curl, base64 and date.
set -euo pipefail

usage() {
  [ -z "${1:-}" ] || echo "legacy-subjects-gate.sh: $1" >&2
  echo "usage: legacy-subjects-gate.sh --context <kube-context> --record <file> [--since <earlier record>] [--listener <machine>=<url>]... [--insecure] [--bus-alias <nats-urls>]... [--token-file <path>] [--inbox-prefix <prefix>]" >&2
  echo "  <kube-context> is the kubectl context of the cluster that runs the listeners and the hub bridge" >&2
  echo "  <file> is where this reading is written; it must not exist" >&2
  echo "  <earlier record> is the first reading's --record, read at least the stream's retention (72 h) before" >&2
  echo "  <machine>=<url> names a listener outside the cluster: its ENVOY_MACHINE_ID and its https://host[:port]" >&2
  echo "  --insecure takes an http:// listener URL, whose answer anyone on the way could read or change" >&2
  echo "  <nats-urls> is a NATS_URLS that reaches the NATS servers the listener Deployment's NATS_URLS reaches, by another name: a listener container that dials it is read as one of the bus's" >&2
  echo "  <path> is where the listener Deployment mounts its projected NATS token, for a hub that is not static" >&2
  echo "  <prefix> is the inbox prefix the listener identity's grant subscribes, for a hub that is not static" >&2
  exit 2
}

context=""
record=""
since=""
insecure=0
token_file=""
inbox_prefix=""
outside=()
aliases=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) [ "$#" -ge 2 ] || usage "--context takes a value"; context=$2; shift 2 ;;
    --record) [ "$#" -ge 2 ] || usage "--record takes a value"; record=$2; shift 2 ;;
    --since) [ "$#" -ge 2 ] || usage "--since takes a value"; since=$2; shift 2 ;;
    --listener) [ "$#" -ge 2 ] || usage "--listener takes a value"; outside+=("$2"); shift 2 ;;
    --bus-alias) [ "$#" -ge 2 ] || usage "--bus-alias takes a value"; aliases+=("$2"); shift 2 ;;
    --insecure) insecure=1; shift ;;
    --token-file) [ "$#" -ge 2 ] || usage "--token-file takes a value"; token_file=$2; shift 2 ;;
    --inbox-prefix) [ "$#" -ge 2 ] || usage "--inbox-prefix takes a value"; inbox_prefix=$2; shift 2 ;;
    *) usage "unknown argument $1" ;;
  esac
done
[ -n "$context" ] || usage "--context <kube-context> is required: the gate reads no cluster it was not named"
[ -n "$record" ] || usage "--record <file> is required: a reading the gate does not keep cannot be compared"
[ ! -e "$record" ] || usage "--record $record exists; the gate writes a new file and overwrites none"
[ -d "$(dirname -- "$record")" ] && [ -w "$(dirname -- "$record")" ] ||
  usage "--record $record: its directory $(dirname -- "$record") is no directory the gate can write"
for entry in ${outside[@]+"${outside[@]}"}; do
  [[ $entry =~ ^[A-Za-z0-9._-]+=https?://[A-Za-z0-9.-]+(:[0-9]+)?/?$ ]] || usage "--listener $entry is not <machine>=https://host[:port]"
  [[ $entry =~ =https:// ]] || [ "$insecure" = 1 ] ||
    usage "--listener $entry is plain http: anyone on the way could read or change the count the gate opens on; give its https URL, or --insecure to take it as it is"
done
for alias in ${aliases[@]+"${aliases[@]}"}; do
  [[ $alias =~ ^[a-z][a-z0-9+.-]*://[A-Za-z0-9.-]+(:[0-9]+)?(,[a-z][a-z0-9+.-]*://[A-Za-z0-9.-]+(:[0-9]+)?)*$ ]] ||
    usage "--bus-alias $alias is not a NATS_URLS: one or more scheme://host[:port], comma-separated"
done
[ -z "$token_file" ] || [[ $token_file =~ ^/[A-Za-z0-9._/-]+$ ]] || usage "--token-file $token_file is not an absolute path"
case "$token_file/" in
  */../* | */./*) usage "--token-file $token_file holds a . or .. segment; name the token's own path" ;;
esac
[ -z "$inbox_prefix" ] || [[ $inbox_prefix =~ ^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$ ]] || usage "--inbox-prefix $inbox_prefix is not a NATS subject of literal tokens"
for tool in kubectl jq curl base64 date; do
  command -v "$tool" >/dev/null 2>&1 || usage "$tool is required and is not on PATH"
done

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly here context record since insecure token_file inbox_prefix
readonly judge="$here/lib/legacy-subjects-gate-judge.jq"
readonly job_script="$here/lib/legacy-subjects-gate-nats.sh"
# The nats-box image the deployment pins for its own NATS Jobs (envoy-cluster/Pulumi.prod.yaml
# moveImage), by digest, so the Job runs the nats CLI the reads were measured with.
readonly nats_box="mirror.gcr.io/natsio/nats-box:0.19.0@sha256:00f10dd4156f28ece84bb3a52cf356ef220ebaa0494310caa90dec184d9e2b89"
readonly job_namespace=envoy
readonly listener_deployment=envoy-listener
readonly job_deadline=300
# The listener logs its counter every minute; the gate reads this much of each listener's log.
readonly log_window=5m

# listener_container selects the containers that run the listener: an image whose path ends
# legion/envoy, running the image's own entrypoint or a command of envoy-listener. The pods and the
# listener Deployment are read by the one rule.
readonly listener_container='def listener_container: select(.image | test("(^|/)legion/envoy([@:]|$)"))
  | select(.command // [] | length == 0 or (.[0] | split("/") | last) == "envoy-listener");'

earlier=null
if [ -n "$since" ]; then
  [ -r "$since" ] || usage "--since $since cannot be read"
  earlier=$(jq -c . "$since" 2>/dev/null) || usage "--since $since is no JSON"
  jq -e '.gate == "legacy-subjects-gate" and .version == 3 and (.finished_at | type) == "number"' <<<"$earlier" >/dev/null ||
    usage "--since $since is no legacy-subjects-gate reading of this version (3)"
  [ "$(jq -r .context <<<"$earlier")" = "$context" ] ||
    usage "--since $since read the cluster $(jq -r .context <<<"$earlier"), not $context"
fi

work=$(mktemp -d)
readonly work
job=""
job_deleted=0
job_delete_tried=0

kc() {
  kubectl --context "$context" --request-timeout=60s "$@"
}

# first_line prints the first line of a file that is not blank, or nothing.
first_line() {
  awk 'NF {print; exit}' "$1"
}

# delete_job deletes the gate's Job and its pod, once, and reads back that envoy holds neither; it
# sets job_deleted to 1 when it does, and prints the line saying which.
delete_job() {
  [ -n "$job" ] || return 0
  local left_job="" left_pods=""
  [ "$job_delete_tried" = 0 ] || return 0
  job_delete_tried=1
  kc -n "$job_namespace" delete job "$job" --cascade=foreground --wait=true --timeout=120s >/dev/null 2>"$work/delete.err" || true
  if left_job=$(kc -n "$job_namespace" get job "$job" --ignore-not-found -o name 2>"$work/get.err") &&
    left_pods=$(kc -n "$job_namespace" get pods -l "job-name=$job" -o name 2>>"$work/get.err") &&
    [ -z "$left_job$left_pods" ]; then
    job_deleted=1
    echo "JOB: deleted $job_namespace/$job, and $job_namespace holds no Job or pod of that name"
  else
    echo "JOB: $job_namespace/$job was not shown deleted${left_job:+ (still listed: $left_job)}$(first_line "$work/delete.err" | sed 's/^/ (/; s/$/)/')"
  fi
}

# cleanup deletes the Job when the gate stops before its own deletion ran (an error, a signal).
# shellcheck disable=SC2329 # cleanup runs through the EXIT trap below
cleanup() {
  delete_job >&2 || true
  rm -rf "$work"
}
trap cleanup EXIT

started_at=$(date -u +%s)

# --- the listener Deployment, whose identity the Job takes and whose bus the reading is about -----
deployment_error=""
if ! kc -n "$job_namespace" get deployment "$listener_deployment" -o json >"$work/deployment.json" 2>"$work/deployment.err"; then
  deployment_error="the listener Deployment $job_namespace/$listener_deployment could not be read: $(first_line "$work/deployment.err")"
  echo '{}' >"$work/identity.json"
else
  jq -c --arg token "$token_file" "$listener_container"'
    .spec.template.spec as $s
    | [$s.containers[] | listener_container] as $listeners
    | ($listeners | first // {}) as $c
    | ([$c.env[]? | select(.name == "NATS_URLS") | .value // empty] | first // null) as $urls
    | (if $token != "" then $token else ([$c.env[]? | select(.name == "NATS_NKEY_SEED_FILE") | .value // empty] | first // null) end) as $file
    | (if $file == null then null else ([$c.volumeMounts[]? | . as $m | select($file | startswith(($m.mountPath | rtrimstr("/")) + "/"))] | first) end) as $mount
    | (if $mount == null then null else ([$s.volumes[]? | select(.name == $mount.name)] | first) end) as $volume
    | {listeners: ($listeners | length), sa: ($s.serviceAccountName // null), urls: $urls,
       credential: (if $token != "" then "token" else "seed" end), file: $file, mount: $mount, volume: $volume}' \
    "$work/deployment.json" >"$work/identity.json"
fi
# The bus the reading is about: the listener Deployment's NATS_URLS, and each NATS_URLS --bus-alias
# names as another address of the same NATS servers. A listener container on another bus (a
# load-test stack's listeners on their own NATS) is named and not read; one on this bus that is
# missed, by an address no --bus-alias named, is named by its durable instead (the judge's census),
# so nothing opens on it.
bus_urls=$(jq -r '.urls // ""' "$work/identity.json")
bus_aliases=$(jq -cn '$ARGS.positional' --args ${aliases[@]+"${aliases[@]}"})

# --- the listeners in the cluster -----------------------------------------------------------------
pods_error=""
if ! { kc get pods --all-namespaces -o json >"$work/pods.json" 2>"$work/pods.err" &&
  jq -c --arg urls "$bus_urls" --argjson aliases "$bus_aliases" "$listener_container"'
    def on_bus: .urls as $u | $urls == "" or $u == $urls or any($aliases[]; . == $u);
    [.items[] | . as $p | ($p.status.containerStatuses // []) as $statuses
      | $p.spec.containers[]
      | listener_container
      | . as $c
      | (($statuses | map(select(.name == $c.name)) | first) // {}) as $s
      | {ns: $p.metadata.namespace, pod: $p.metadata.name, uid: $p.metadata.uid, container: $c.name,
         running: ($s.state.running != null), state: ((($s.state // {}) | keys | first) // "not started"),
         restarts: ($s.restartCount // null),
         machine: ([$c.env[]? | select(.name == "ENVOY_MACHINE_ID") | .value // empty] | first // null),
         machine_from: ([$c.env[]? | select(.name == "ENVOY_MACHINE_ID") | if .valueFrom != null then "valueFrom" else empty end] | first
           // (if ($c.envFrom // []) | length > 0 then "envFrom" else null end)),
         urls: ([$c.env[]? | select(.name == "NATS_URLS") | .value // empty] | first // null)}]
    | {bus: map(select(on_bus)), other: map(select(on_bus | not))}' \
    "$work/pods.json" >"$work/listener-sets.json" 2>"$work/pods.err" &&
  jq -c '.bus' "$work/listener-sets.json" >"$work/listeners.json" &&
  jq -c '.other' "$work/listener-sets.json" >"$work/other-bus.json"; }; then
  pods_error="the cluster's pods could not be listed: $(first_line "$work/pods.err")"
  echo '[]' >"$work/listeners.json"
  echo '[]' >"$work/other-bus.json"
fi

# Each running listener's newest "listener legacy subjects" log line: {ok, text, at}, the metrics
# text and the second it was read at, or {ok: false, error}.
: >"$work/pod-reads.jsonl"
count=$(jq length "$work/listeners.json")
for ((i = 0; i < count; i++)); do
  entry=$(jq -c ".[$i]" "$work/listeners.json")
  ns=$(jq -r .ns <<<"$entry")
  pod=$(jq -r .pod <<<"$entry")
  container=$(jq -r .container <<<"$entry")
  read_json='{"ok": false, "error": "not running"}'
  if [ "$(jq -r .running <<<"$entry")" = true ]; then
    if kc -n "$ns" logs "$pod" -c "$container" --since="$log_window" >"$work/log" 2>"$work/log.err"; then
      read_json=$(jq -R -c 'fromjson? | select(type == "object" and .msg == "listener legacy subjects")' "$work/log" |
        jq -cs --arg window "$log_window" 'if length == 0
          then {ok: false, error: "its log holds no \"listener legacy subjects\" line from the last \($window): its build predates that line, or it is not logging it"}
          else last | {ok: true, text: (.metrics | strings // ""), at: (.at | numbers // null)} end')
    else
      read_json=$(jq -cn --arg error "$(first_line "$work/log.err")" '{ok: false, error: "its log could not be read: \($error)"}')
    fi
  fi
  jq -c --argjson read "$read_json" '. + {read: $read}' <<<"$entry" >>"$work/pod-reads.jsonl"
done

# --- the listeners outside it ---------------------------------------------------------------------
: >"$work/outside-reads.jsonl"
for entry in ${outside[@]+"${outside[@]}"}; do
  name=${entry%%=*}
  url=${entry#*=}
  url=${url%/}
  if status=$(curl -sS --max-time 15 -o "$work/metrics" -w '%{http_code}' "$url/metrics" 2>"$work/metrics.err") && [ "$status" = 200 ]; then
    read_json=$(jq -cn --rawfile text "$work/metrics" '{ok: true, text: $text}')
  else
    read_json=$(jq -cn --arg error "${status:+HTTP $status }$(first_line "$work/metrics.err")" '{ok: false, error: "its /metrics could not be read: \($error)"}')
  fi
  jq -cn --arg name "$name" --arg url "$url" --argjson read "$read_json" '{name: $name, url: $url, read: $read}' >>"$work/outside-reads.jsonl"
done

# --- the NATS reads, from a Job that connects as the in-cluster listener --------------------------
job_ok=0
job_error=""
: >"$work/job-lines.jsonl"
if [ -n "$deployment_error" ]; then
  job_error=$deployment_error
else
  missing=$(jq -r '[if .listeners != 1 then "one listener container (it has \(.listeners))" else empty end,
      if .sa == null then "a ServiceAccount" else empty end,
      if .urls == null then "NATS_URLS" else empty end,
      if .file == null then "a NATS seed file (NATS_NKEY_SEED_FILE): name the listener'"'"'s projected NATS token with --token-file once the hub is not static" else empty end,
      if .file != null and .mount == null then "a volume mount holding \(if .credential == "token" then "the token file" else "the seed file" end) \(.file)" else empty end,
      if .mount != null and .volume == null then "the volume \(.mount.name)" else empty end] | join("; ")' "$work/identity.json")
  if [ -n "$missing" ]; then
    job_error="the listener Deployment $job_namespace/$listener_deployment lacks $missing"
  else
    name="legacy-subjects-gate-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
    # Whose reading this is, for the Job's owner label: a uid with no passwd entry (a container run
    # as an arbitrary uid) has no name, and is `unknown` rather than the end of the reading.
    owner=$(id -un 2>/dev/null || echo unknown)
    owner=$(printf '%s' "$owner" | tr -c 'A-Za-z0-9._-' '-' | cut -c1-63)
    owner=${owner%-}
    owner=${owner:-unknown}
    # The pod mounts no Kubernetes API token: the NATS credential is the seed file's secret volume
    # (its CSI driver takes the pod's token from the kubelet, tokenRequests) or a projected volume,
    # and neither path needs the API.
    jq -n --arg name "$name" --arg namespace "$job_namespace" --arg owner "$owner" \
      --arg image "$nats_box" --rawfile script "$job_script" --argjson deadline "$job_deadline" \
      --rawfile forms "$here/lib/legacy-subjects-gate-forms.jq" \
      --arg inbox "$inbox_prefix" --slurpfile identity "$work/identity.json" '
      $identity[0] as $id
      | {"app.kubernetes.io/name": "legacy-subjects-gate", "created-by": "legacy-subjects-gate", owner: $owner} as $labels
      | "legacy-subjects-gate-work" as $scratch
      | {apiVersion: "batch/v1", kind: "Job",
         metadata: {name: $name, namespace: $namespace, labels: $labels},
         spec: {backoffLimit: 0, activeDeadlineSeconds: $deadline, ttlSecondsAfterFinished: 600,
           template: {metadata: {labels: $labels}, spec: {
             serviceAccountName: $id.sa, automountServiceAccountToken: false, restartPolicy: "Never",
             securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, fsGroup: 65534,
               seccompProfile: {type: "RuntimeDefault"}},
             containers: [{name: "read", image: $image, command: ["sh", "-c", $script],
               env: ([{name: "NATS_URL", value: $id.urls},
                      (if $id.credential == "token" then {name: "NATS_TOKEN_FILE", value: $id.file} else {name: "NATS_NKEY", value: $id.file} end),
                      {name: "WORK_DIR", value: "/work"}, {name: "FORMS_JQ", value: $forms}]
                     + (if $inbox == "" then [] else [{name: "NATS_INBOX_PREFIX", value: $inbox}] end)),
               volumeMounts: [$id.mount + {readOnly: true}, {name: $scratch, mountPath: "/work"}],
               resources: {requests: {cpu: "250m", memory: "512Mi"}, limits: {cpu: "250m", memory: "512Mi"}},
               securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}}}],
             volumes: [$id.volume, {name: $scratch, emptyDir: {medium: "Memory", sizeLimit: "16Mi"}}]}}}}' >"$work/job.json"
    if ! kc -n "$job_namespace" create -f "$work/job.json" -o name >"$work/create.out" 2>"$work/create.err"; then
      job_error="the Job could not be created: $(first_line "$work/create.err")"
    else
      # The Job is the gate's to delete from here on, and not before: a Job never made is never
      # reported deleted.
      job=$name
      echo "JOB: created $job_namespace/$job at $(date -u +%Y-%m-%dT%H:%M:%SZ), owner $owner"
      deadline=$((SECONDS + job_deadline + 60))
      outcome=""
      while :; do
        if kc -n "$job_namespace" get job "$job" -o json >"$work/job-status.json" 2>"$work/job-status.err"; then
          outcome=$(jq -r 'if (.status.succeeded // 0) >= 1 then "succeeded" elif (.status.failed // 0) >= 1 then "failed" else "" end' "$work/job-status.json")
        fi
        [ -z "$outcome" ] || break
        if [ "$SECONDS" -ge "$deadline" ]; then
          outcome="not finished after $((job_deadline + 60)) s"
          break
        fi
        sleep 2
      done
      if ! kc -n "$job_namespace" logs "job/$job" >"$work/job.log" 2>"$work/logs.err"; then
        job_error="the Job ($outcome) left no log that could be read: $(first_line "$work/logs.err")"
      elif ! jq -R -c 'split("\t") | select(length == 5)
          | {leg: .[0], arg: .[1], rc: (.[2] | tonumber? // -1), out: (.[3] | @base64d), err: (.[4] | @base64d)}' \
        "$work/job.log" >"$work/job-lines.jsonl" 2>"$work/parse.err"; then
        job_error="the Job's log could not be read: $(first_line "$work/parse.err")"
      elif [ "$outcome" = succeeded ]; then
        job_ok=1
      else
        job_error="the Job $outcome; its log ends: $(tail -n 1 "$work/job.log" | cut -c1-200)"
      fi
      delete_job
    fi
  fi
fi

finished_at=$(date -u +%s)

# --- the judgement --------------------------------------------------------------------------------
jq -n \
  --arg context "$context" \
  --argjson started "$started_at" \
  --argjson finished "$finished_at" \
  --argjson earlier "$earlier" \
  --arg pods_error "$pods_error" \
  --slurpfile pods "$work/pod-reads.jsonl" \
  --slurpfile other_bus "$work/other-bus.json" \
  --arg bus "$bus_urls" \
  --argjson bus_aliases "$bus_aliases" \
  --slurpfile outside "$work/outside-reads.jsonl" \
  --slurpfile lines "$work/job-lines.jsonl" \
  --argjson job_ok "$job_ok" \
  --arg job_error "$job_error" \
  '{context: $context, started_at: $started, finished_at: $finished, earlier: $earlier,
    pods_error: (if $pods_error == "" then null else $pods_error end),
    pods: $pods, other_bus: ($other_bus[0] // []), bus: $bus, bus_aliases: $bus_aliases, outside: $outside,
    job: {ok: ($job_ok == 1), error: (if $job_error == "" then null else $job_error end), lines: $lines}}' >"$work/input.json"
# A judgement that fails on what it was given (a fault in the judge itself) is a reading that could
# not be judged: UNKNOWN, with a line and a record naming the failure, never a bare jq exit.
if ! jq -L "$here/lib" -f "$judge" "$work/input.json" >"$work/judgement.json" 2>"$work/judge.err"; then
  jq -n --arg context "$context" --argjson started "$started_at" --argjson finished "$finished_at" \
    --argjson earlier "$earlier" --arg error "$(first_line "$work/judge.err")" '
    "the judgement could not read what the gate read: \($error | gsub("[[:cntrl:]]"; "?"))" as $why
    | {verdict: "unknown", window: 259200, lines: ["UNKNOWN: " + $why],
       record: {gate: "legacy-subjects-gate", version: 3, context: $context, started_at: $started,
         finished_at: $finished, read_at: ($started | todate), verdict: "unknown",
         compared_with: (if $earlier == null then null else $earlier.read_at end),
         identity: null, stream: null, listeners: [], consumers_old: [], subjects_old: [], interests_old: [],
         shut: [], unknown: [$why]}}' >"$work/judgement.json"
fi

jq -r '.lines[]' "$work/judgement.json"
if [ "$job" != "" ] && [ "$job_deleted" = 0 ]; then
  echo "UNKNOWN: the Job $job_namespace/$job was not shown deleted; delete it: kubectl --context $context -n $job_namespace delete job $job"
  jq '.record.unknown += ["the Job was not shown deleted"] | (.record.verdict, .verdict) |= (if . == "shut" then . else "unknown" end)' \
    "$work/judgement.json" >"$work/judgement.tmp" && mv "$work/judgement.tmp" "$work/judgement.json"
fi
# The record is written as a new file (noclobber): one made since the gate began is left as it is,
# and a record not kept is a reading that cannot be compared.
if ! (set -o noclobber && jq '.record' "$work/judgement.json" >"$record") 2>"$work/record.err"; then
  echo "UNKNOWN: the record $record could not be written, so this reading cannot be compared: $(first_line "$work/record.err")"
  echo "UNKNOWN"
  exit 3
fi

verdict=$(jq -r .verdict "$work/judgement.json")
case "$verdict" in
  open) echo "GATE OPEN"; exit 0 ;;
  shut) echo "GATE SHUT"; exit 1 ;;
  unknown) echo "UNKNOWN"; exit 3 ;;
  clean)
    again=$(jq -r '.again | todate' "$work/judgement.json")
    found=$(jq -r .found "$work/judgement.json")
    last_seq=$(jq -r .last_seq "$work/judgement.json")
    echo "FIRST READING: $record holds a first reading of every surface, with $found FOUND line(s) naming old-form names still in use; read again at or after $again with --since $record: that reading opens only if the stream then holds no old-form message (none after stream sequence $last_seq), no listener's count moved, and no interest record or consumer filter holds an old form"
    exit 4
    ;;
esac
echo "UNKNOWN"
exit 3
