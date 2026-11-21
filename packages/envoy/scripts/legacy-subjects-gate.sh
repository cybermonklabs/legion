#!/usr/bin/env bash
# packages/envoy/scripts/legacy-subjects-gate.sh --context <kube-context> --record <file>
#   [--since <earlier record>] [--listener <machine>=<url>]...
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
#   - Each listener's /metrics, its envoy_listener_legacy_subjects_total samples (every label set)
#     and its start-time gauge envoy_listener_start_time_seconds. The listeners in the cluster are
#     found by what they run, not by name: every container, in any namespace, of an image whose path
#     ends legion/envoy that runs the image's own entrypoint (envoy-listener; a command of
#     envoy-listener counts too, and any other command is the Dispatch server, the broker or a
#     helper). Each is read through the API server's pod proxy,
#     /api/v1/namespaces/<ns>/pods/<pod>:<PORT, else 9020>/proxy/metrics. A listener outside the
#     cluster, such as the VM hub's, is named with --listener <machine>=<url> and read at
#     <url>/metrics. Every listener bound to its durable on the bus (listener-<machine>,
#     bus.ListenerDurable) must be one of those: one that is not is UNKNOWN by name, so a listener
#     nobody named is never left out of the count.
#   - Over NATS, from a one-off Job in namespace envoy that connects as the in-cluster listener
#     does (lib/legacy-subjects-gate-nats.sh): the stream's info, every consumer of the stream, the
#     stream's subjects under notifications.dispatch.issue.>, notifications.agent.>,
#     notifications.envoy.exceptions.notifications.agent.> and notifications.envoy.kept.>, and every
#     record of the interest store envoy_interests.
#
# What holds the gate shut:
#
#   - a consumer one of whose filters only an old-form name matches, and whose new form the same
#     consumer does not also filter (a session consumer filters both forms of each topic until the
#     old form is refused, so its old filter loses nothing; a filter that is the old form alone,
#     a Legion daemon's legion-go-<project>-<kind> or any other, would go deaf once publishers stop);
#   - an old-form subject the stream still holds;
#   - an interest record holding an old-form topic other than its session's own agent subject,
#     which the listener writes into every record itself until the old form is refused;
#   - at a second reading: a listener whose count rose since the first reading, a first reading
#     that was not clean, or less than the retention between the two.
#
# The old form is the gate's own rule (lib/legacy-subjects-gate-forms.jq, old_form), so the gate
# stays right once the code that reads the old form is deleted.
#
# What it cannot see is UNKNOWN, by name, and UNKNOWN is never GATE OPEN: a surface it could not
# read or that answered nothing (no listener, no Legion daemon consumer, no issue or agent subject
# on the stream, no interest record: a production bus always holds them), a listener that is not
# running, a listener build that serves no old-form counter, a listener that restarted between the
# readings (its pod's restart count or its start-time gauge moved, or it is a new pod), a listener
# of the first reading that is gone, and a listener outside the cluster that serves no start-time
# gauge, since nothing else shows its restart.
#
# The Job: namespace envoy, the ServiceAccount, NATS URL, NATS seed file and the volume holding that
# file of the listener Deployment envoy/envoy-listener as it is deployed (read at each run, so the
# Job connects with whatever credential the listener holds), the nats-box image the deployment pins
# for its own NATS Jobs, run as nobody (65534) with every capability dropped, backoffLimit 0,
# activeDeadlineSeconds 300 and ttlSecondsAfterFinished 600; labels app.kubernetes.io/name and
# created-by legacy-subjects-gate and owner <your login>. The gate waits for it to finish, reads its
# log, deletes it and reads back that no Job or pod of its name is left; one it could not delete is
# UNKNOWN and named with the command that deletes it. A listener Deployment that connects with no
# NATS seed file (NATS_NKEY_SEED_FILE) is UNKNOWN: the gate dials NATS only with the listener's seed.
#
# It prints one line per surface, `READ`, `PASS`, `GATE SHUT` or `UNKNOWN`, the Job's creation and
# deletion, and the verdict alone on the last line: `GATE OPEN` (exit 0) at a clean second reading,
# `GATE SHUT` (exit 1) when anything shut it, `UNKNOWN` (exit 3) otherwise when anything was not
# read, and at a clean first reading `FIRST READING`, naming when to read again (exit 4). It writes
# the reading to --record in every case, a file that must not exist yet; a clean second reading's
# record is a clean first reading for the next window. A usage error exits 2 and reads nothing.
#
# It needs bash, kubectl (a context for the cluster that runs the listeners and the hub bridge,
# signed in as an account that may list pods in every namespace, read pods/proxy, read the
# envoy-listener Deployment, and create, read, read the logs of and delete Jobs and pods in envoy; a
# developer box's account holds those), jq, curl, base64 and date.
set -euo pipefail

usage() {
  [ -z "${1:-}" ] || echo "legacy-subjects-gate.sh: $1" >&2
  echo "usage: legacy-subjects-gate.sh --context <kube-context> --record <file> [--since <earlier record>] [--listener <machine>=<url>]..." >&2
  echo "  <kube-context> is the kubectl context of the cluster that runs the listeners and the hub bridge" >&2
  echo "  <file> is where this reading is written; it must not exist" >&2
  echo "  <earlier record> is the first reading's --record, read at least the stream's retention (72 h) before" >&2
  echo "  <machine>=<url> names a listener outside the cluster: its ENVOY_MACHINE_ID and its http(s)://host[:port]" >&2
  exit 2
}

context=""
record=""
since=""
outside=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) [ "$#" -ge 2 ] || usage "--context takes a value"; context=$2; shift 2 ;;
    --record) [ "$#" -ge 2 ] || usage "--record takes a value"; record=$2; shift 2 ;;
    --since) [ "$#" -ge 2 ] || usage "--since takes a value"; since=$2; shift 2 ;;
    --listener) [ "$#" -ge 2 ] || usage "--listener takes a value"; outside+=("$2"); shift 2 ;;
    *) usage "unknown argument $1" ;;
  esac
done
[ -n "$context" ] || usage "--context <kube-context> is required: the gate reads no cluster it was not named"
[ -n "$record" ] || usage "--record <file> is required: a reading the gate does not keep cannot be compared"
[ ! -e "$record" ] || usage "--record $record exists; the gate writes a new file and overwrites none"
for entry in ${outside[@]+"${outside[@]}"}; do
  [[ $entry =~ ^[A-Za-z0-9._-]+=https?://[A-Za-z0-9.-]+(:[0-9]+)?/?$ ]] || usage "--listener $entry is not <machine>=http(s)://host[:port]"
done
for tool in kubectl jq curl base64 date; do
  command -v "$tool" >/dev/null 2>&1 || usage "$tool is required and is not on PATH"
done

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly here context record since
readonly judge="$here/lib/legacy-subjects-gate-judge.jq"
readonly job_script="$here/lib/legacy-subjects-gate-nats.sh"
# The nats-box image the deployment pins for its own NATS Jobs (envoy-cluster/Pulumi.prod.yaml
# moveImage), by digest, so the Job runs the nats CLI the reads were measured with.
readonly nats_box="mirror.gcr.io/natsio/nats-box:0.19.0@sha256:00f10dd4156f28ece84bb3a52cf356ef220ebaa0494310caa90dec184d9e2b89"
readonly job_namespace=envoy
readonly listener_deployment=envoy-listener
readonly job_deadline=300

# listener_container selects the containers that run the listener: an image whose path ends
# legion/envoy, running the image's own entrypoint or a command of envoy-listener. The pods and the
# listener Deployment are read by the one rule.
readonly listener_container='def listener_container: select(.image | test("(^|/)legion/envoy([@:]|$)"))
  | select(.command // [] | length == 0 or (.[0] | split("/") | last) == "envoy-listener");'

earlier=null
if [ -n "$since" ]; then
  [ -r "$since" ] || usage "--since $since cannot be read"
  earlier=$(jq -c . "$since" 2>/dev/null) || usage "--since $since is no JSON"
  jq -e '.gate == "legacy-subjects-gate" and .version == 1 and (.finished_at | type) == "number"' <<<"$earlier" >/dev/null ||
    usage "--since $since is no legacy-subjects-gate reading"
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

# --- the listeners in the cluster -----------------------------------------------------------------
pods_error=""
if ! { kc get pods --all-namespaces -o json >"$work/pods.json" 2>"$work/pods.err" &&
  jq -c "$listener_container"'
    [.items[] | . as $p | ($p.status.containerStatuses // []) as $statuses
      | $p.spec.containers[]
      | listener_container
      | . as $c
      | (($statuses | map(select(.name == $c.name)) | first) // {}) as $s
      | {ns: $p.metadata.namespace, pod: $p.metadata.name, uid: $p.metadata.uid, container: $c.name,
         running: ($s.state.running != null), state: ((($s.state // {}) | keys | first) // "not started"),
         restarts: ($s.restartCount // null),
         machine: ([$c.env[]? | select(.name == "ENVOY_MACHINE_ID") | .value // empty] | first // null),
         port: ([$c.env[]? | select(.name == "PORT") | .value // empty] | first // "9020")}]' \
    "$work/pods.json" >"$work/listeners.json" 2>"$work/pods.err"; }; then
  pods_error="the cluster's pods could not be listed: $(first_line "$work/pods.err")"
  echo '[]' >"$work/listeners.json"
fi

: >"$work/pod-reads.jsonl"
count=$(jq length "$work/listeners.json")
for ((i = 0; i < count; i++)); do
  entry=$(jq -c ".[$i]" "$work/listeners.json")
  ns=$(jq -r .ns <<<"$entry")
  pod=$(jq -r .pod <<<"$entry")
  port=$(jq -r .port <<<"$entry")
  read_json='{"ok": false, "error": "not running"}'
  if [ "$(jq -r .running <<<"$entry")" = true ]; then
    if ! [[ $port =~ ^[0-9]{1,5}$ ]]; then
      read_json=$(jq -cn --arg port "$port" '{ok: false, error: "its PORT \($port) is no port number"}')
    elif kc get --raw "/api/v1/namespaces/$ns/pods/$pod:$port/proxy/metrics" >"$work/metrics" 2>"$work/metrics.err"; then
      read_json=$(jq -cn --rawfile text "$work/metrics" '{ok: true, text: $text}')
    else
      read_json=$(jq -cn --arg error "$(first_line "$work/metrics.err")" '{ok: false, error: $error}')
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
    read_json=$(jq -cn --arg error "${status:+HTTP $status }$(first_line "$work/metrics.err")" '{ok: false, error: $error}')
  fi
  jq -cn --arg name "$name" --arg url "$url" --argjson read "$read_json" '{name: $name, url: $url, read: $read}' >>"$work/outside-reads.jsonl"
done

# --- the NATS reads, from a Job that connects as the in-cluster listener --------------------------
job_ok=0
job_error=""
: >"$work/job-lines.jsonl"
if ! kc -n "$job_namespace" get deployment "$listener_deployment" -o json >"$work/deployment.json" 2>"$work/deployment.err"; then
  job_error="the listener Deployment $job_namespace/$listener_deployment could not be read: $(first_line "$work/deployment.err")"
else
  jq -c "$listener_container"'
    .spec.template.spec as $s
    | [$s.containers[] | listener_container] as $listeners
    | ($listeners | first // {}) as $c
    | ([$c.env[]? | select(.name == "NATS_URLS") | .value // empty] | first // null) as $urls
    | ([$c.env[]? | select(.name == "NATS_NKEY_SEED_FILE") | .value // empty] | first // null) as $seed
    | (if $seed == null then null else ([$c.volumeMounts[]? | . as $m | select($seed | startswith(($m.mountPath | rtrimstr("/")) + "/"))] | first) end) as $mount
    | (if $mount == null then null else ([$s.volumes[]? | select(.name == $mount.name)] | first) end) as $volume
    | {listeners: ($listeners | length), sa: ($s.serviceAccountName // null), urls: $urls, seed: $seed, mount: $mount, volume: $volume}' \
    "$work/deployment.json" >"$work/identity.json"
  missing=$(jq -r '[if .listeners != 1 then "one listener container (it has \(.listeners))" else empty end,
      if .sa == null then "a ServiceAccount" else empty end,
      if .urls == null then "NATS_URLS" else empty end,
      if .seed == null then "a NATS seed file (NATS_NKEY_SEED_FILE): the gate dials NATS only with the listener seed" else empty end,
      if .seed != null and .mount == null then "a volume mount holding \(.seed)" else empty end,
      if .mount != null and .volume == null then "the volume \(.mount.name)" else empty end] | join("; ")' "$work/identity.json")
  if [ -n "$missing" ]; then
    job_error="the listener Deployment $job_namespace/$listener_deployment lacks $missing"
  else
    job="legacy-subjects-gate-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
    owner=$(id -un 2>/dev/null | tr -c 'A-Za-z0-9._-' '-' | cut -c1-63)
    owner=${owner%-}
    jq -n --arg name "$job" --arg namespace "$job_namespace" --arg owner "${owner:-unknown}" \
      --arg image "$nats_box" --rawfile script "$job_script" --argjson deadline "$job_deadline" \
      --slurpfile identity "$work/identity.json" '
      $identity[0] as $id
      | {"app.kubernetes.io/name": "legacy-subjects-gate", "created-by": "legacy-subjects-gate", owner: $owner} as $labels
      | {apiVersion: "batch/v1", kind: "Job",
         metadata: {name: $name, namespace: $namespace, labels: $labels},
         spec: {backoffLimit: 0, activeDeadlineSeconds: $deadline, ttlSecondsAfterFinished: 600,
           template: {metadata: {labels: $labels}, spec: {
             serviceAccountName: $id.sa, restartPolicy: "Never",
             securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, fsGroup: 65534,
               seccompProfile: {type: "RuntimeDefault"}},
             containers: [{name: "read", image: $image, command: ["sh", "-c", $script],
               env: [{name: "NATS_URL", value: $id.urls}, {name: "NATS_NKEY", value: $id.seed}],
               volumeMounts: [$id.mount + {readOnly: true}],
               resources: {requests: {cpu: "250m", memory: "512Mi"}, limits: {cpu: "250m", memory: "512Mi"}},
               securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}}}],
             volumes: [$id.volume]}}}}' >"$work/job.json"
    if ! kc -n "$job_namespace" create -f "$work/job.json" -o name >"$work/create.out" 2>"$work/create.err"; then
      job_error="the Job could not be created: $(first_line "$work/create.err")"
      job=""
    else
      echo "JOB: created $job_namespace/$job at $(date -u +%Y-%m-%dT%H:%M:%SZ), owner ${owner:-unknown}"
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
  --slurpfile outside "$work/outside-reads.jsonl" \
  --slurpfile lines "$work/job-lines.jsonl" \
  --argjson job_ok "$job_ok" \
  --arg job_error "$job_error" \
  '{context: $context, started_at: $started, finished_at: $finished, earlier: $earlier,
    pods_error: (if $pods_error == "" then null else $pods_error end),
    pods: $pods, outside: $outside,
    job: {ok: ($job_ok == 1), error: (if $job_error == "" then null else $job_error end), lines: $lines}}' >"$work/input.json"
jq -L "$here/lib" -f "$judge" "$work/input.json" >"$work/judgement.json"

jq -r '.lines[]' "$work/judgement.json"
if [ "$job" != "" ] && [ "$job_deleted" = 0 ]; then
  echo "UNKNOWN: the Job $job_namespace/$job was not shown deleted; delete it: kubectl --context $context -n $job_namespace delete job $job"
  jq '.record.unknown += ["the Job was not shown deleted"] | (.record.verdict, .verdict) |= (if . == "shut" then . else "unknown" end)' \
    "$work/judgement.json" >"$work/judgement.tmp" && mv "$work/judgement.tmp" "$work/judgement.json"
fi
jq '.record' "$work/judgement.json" >"$record"

verdict=$(jq -r .verdict "$work/judgement.json")
case "$verdict" in
  open) echo "GATE OPEN"; exit 0 ;;
  shut) echo "GATE SHUT"; exit 1 ;;
  unknown) echo "UNKNOWN"; exit 3 ;;
  clean)
    again=$(jq -r '(.record.finished_at + .window) | todate' "$work/judgement.json")
    echo "FIRST READING: $record holds a clean reading; read again at or after $again with --since $record"
    exit 4
    ;;
esac
echo "UNKNOWN"
exit 3
