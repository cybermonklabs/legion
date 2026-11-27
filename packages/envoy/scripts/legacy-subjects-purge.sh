#!/usr/bin/env bash
# packages/envoy/scripts/legacy-subjects-purge.sh --context <kube-context> --reading <record>
#
# Purges the old-form messages ENVOY_NOTIFICATIONS still holds, once legacy-subjects-gate.sh's
# second reading has read four of its five facts true: (a1) every pinned build, (a2) every client
# connected to the hub, and (a3) every user of the hub that may publish a notification subject
# writes only new-form names, and (b) every consumer of the stream has acknowledged the last
# old-form message it takes. Then no client writes an old form and no consumer still needs one,
# and the old-form messages are only what keeps fact (c) false, which this driver makes true in
# place of waiting out the stream's retention. The gate's next second reading reads (c) again,
# with the other four.
#
# It refuses to purge anything (REFUSED, exit 1) unless --reading is a second reading of the gate,
# of the cluster --context names, whose record holds (a1), (a2), (a3) and (b) true; it reads that
# record before it calls anything. It then runs one Job in namespace envoy (lib/legacy-subjects-purge-nats.sh)
# that connects as the legacy-cutover user, at the NATS URL of the listener Deployment
# envoy/envoy-listener: the Job runs as the ServiceAccount legacy-cutover and mounts that user's
# nkey seed through the SecretProviderClass legacy-cutover-nats-seed, as the file nats-seed, and
# every call takes the user's inbox prefix, _INBOX_legacy_cutover (legacy-subjects-gate.sh's
# header says what the user may do; the deployment declares all four for it alone). The Job
# purges nothing when the stream is not the one the reading read (its envoy_stream_id), when its
# subjects cannot be read whole, or when an old-form subject holds a message after the newest
# old-form message the reading found; otherwise it purges each old-form subject by itself, with
# that literal subject as the purge's filter, so no new-form message goes, and the sequence after
# that newest one as its bound, so an old-form message published after the Job's check stays, and
# lists the stream's subjects again. It calls $JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS,
# $JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS and $JS.API.STREAM.PURGE.ENVOY_NOTIFICATIONS alone; no
# service grant of this repository holds the purge.
#
# It prints the Job's creation and deletion, a PURGED line for each subject purged, and the verdict
# alone on the last line: PURGED (exit 0) when the read-back finds no old-form subject, REFUSED
# (exit 1), or UNKNOWN (exit 3) when a call failed, the Job did not end, its log was cut short, or
# the read-back still finds an old-form subject. A usage error exits 2 and calls nothing. The Job,
# like the gate's, runs the pinned nats-box image as nobody with every capability dropped, no
# Kubernetes API token, backoffLimit 0, activeDeadlineSeconds 300 and ttlSecondsAfterFinished 600,
# labelled created-by legacy-subjects-purge; the driver deletes it and reads back that no Job or
# pod of its name is left, and names the command that deletes one it could not.
#
# It needs bash, kubectl (create, read, read the logs of and delete Jobs and pods in envoy, and
# read the envoy-listener Deployment), jq, base64 and od.
set -euo pipefail

usage() {
  [ -z "${1:-}" ] || echo "legacy-subjects-purge.sh: $1" >&2
  echo "usage: legacy-subjects-purge.sh --context <kube-context> --reading <record>" >&2
  echo "  <record> is a second reading's --record of legacy-subjects-gate.sh, which read (a1), (a2), (a3) and (b) true" >&2
  exit 2
}

context=""
reading=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) [ "$#" -ge 2 ] || usage "--context takes a value"; context=$2; shift 2 ;;
    --reading) [ "$#" -ge 2 ] || usage "--reading takes a value"; reading=$2; shift 2 ;;
    *) usage "unknown argument $1" ;;
  esac
done
[ -n "$context" ] || usage "--context <kube-context> is required"
[ -n "$reading" ] || usage "--reading <record> is required: the driver purges only on a reading that read (a1), (a2), (a3) and (b) true"
for tool in kubectl jq base64 od; do
  command -v "$tool" >/dev/null 2>&1 || usage "$tool is required and is not on PATH"
done
[ -r "$reading" ] || usage "--reading $reading cannot be read"
record=$(jq -c . "$reading" 2>/dev/null) || usage "--reading $reading is no JSON"

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly here context reading record
readonly job_script="$here/lib/legacy-subjects-purge-nats.sh"
# The legacy-cutover user's Job runs as this ServiceAccount, mounts its seed through this
# SecretProviderClass as this file, and takes this inbox prefix, as legacy-subjects-gate.sh's census
# Job does.
readonly cutover_account=legacy-cutover
readonly cutover_seed_class=legacy-cutover-nats-seed
readonly cutover_seed_file=nats-seed
readonly cutover_inbox=_INBOX_legacy_cutover
# The nats-box image legacy-subjects-gate.sh's Job runs, the one the deployment pins for its own
# NATS Jobs.
readonly nats_box="mirror.gcr.io/natsio/nats-box:0.19.0@sha256:00f10dd4156f28ece84bb3a52cf356ef220ebaa0494310caa90dec184d9e2b89"
readonly job_namespace=envoy
readonly job_deadline=300

# --- the reading: a second reading of this cluster whose (a1), (a2), (a3) and (b) read true -----
refusals=$(jq -r --arg context "$context" --arg path "$reading" '
  {a1: "(a1) the pins", a2: "(a2) the running clients", a3: "(a3) the publishers", b: "(b) the consumers"} as $names
  | if .gate != "legacy-subjects-gate" or .version != 3 then "the reading \($path) is no legacy-subjects-gate reading of version 3"
    elif .context != $context then "the reading \($path) read the cluster \(.context), not \($context)"
    elif .compared_with == null or (.facts | type) != "object" then "the reading \($path) is a first reading, which reads no fact"
    elif (.stream.id | type) != "string" then "the reading \($path) recorded no identity of the stream"
    else ($names | to_entries[] | . as $f | ($f.key as $k | $record_facts[$k]) // "unread" | select(. != "true") | "the reading \($path) read \($f.value) \(.), not true")
    end' --argjson record_facts "$(jq -c '.facts // {}' <<<"$record")" <<<"$record")
if [ -n "$refusals" ]; then
  while IFS= read -r line; do echo "REFUSED: $line"; done <<<"$refusals"
  echo "REFUSED"
  exit 1
fi
stream_id=$(jq -r .stream.id <<<"$record")
last_old_seq=$(jq -r '.facts.last_old_seq // "" | tostring' <<<"$record")

work=$(mktemp -d)
readonly work
job=""
job_deleted=0
job_delete_tried=0

kc() {
  kubectl --context "$context" --request-timeout=60s "$@"
}

first_line() {
  awk 'NF {print; exit}' "$1"
}

# delete_job deletes the Job and its pod, once, and reads back that envoy holds neither.
delete_job() {
  [ -n "$job" ] || return 0
  [ "$job_delete_tried" = 0 ] || return 0
  job_delete_tried=1
  local left_job="" left_pods=""
  kc -n "$job_namespace" delete job "$job" --cascade=foreground --wait=true --timeout=120s >/dev/null 2>"$work/delete.err" || true
  if left_job=$(kc -n "$job_namespace" get job "$job" --ignore-not-found -o name 2>"$work/get.err") &&
    left_pods=$(kc -n "$job_namespace" get pods -l "job-name=$job" -o name 2>>"$work/get.err") &&
    [ -z "$left_job$left_pods" ]; then
    job_deleted=1
    echo "JOB: deleted $job_namespace/$job, and $job_namespace holds no Job or pod of that name"
  else
    echo "JOB: $job_namespace/$job was not shown deleted${left_job:+ (still listed: $left_job)}"
  fi
}

# shellcheck disable=SC2329 # cleanup runs through the EXIT trap below
cleanup() {
  delete_job >&2 || true
  rm -rf "$work"
}
trap cleanup EXIT

verdict() {
  if [ -n "$job" ] && [ "$job_deleted" = 0 ]; then
    echo "UNKNOWN: the Job $job_namespace/$job was not shown deleted; delete it: kubectl --context $context -n $job_namespace delete job $job"
    [ "$1" = REFUSED ] || set -- UNKNOWN
  fi
  echo "$1"
  case "$1" in PURGED) exit 0 ;; REFUSED) exit 1 ;; *) exit 3 ;; esac
}

# --- the Job --------------------------------------------------------------------------------------
if ! kc -n "$job_namespace" get deployment envoy-listener -o json >"$work/deployment.json" 2>"$work/deployment.err"; then
  echo "UNKNOWN: the listener Deployment $job_namespace/envoy-listener, whose NATS_URLS the Job dials, could not be read: $(first_line "$work/deployment.err")"
  verdict UNKNOWN
fi
nats_url=$(jq -r '[.spec.template.spec.containers[]? | .env[]? | select(.name == "NATS_URLS") | .value // empty] | first // ""' "$work/deployment.json")
if [ -z "$nats_url" ]; then
  echo "UNKNOWN: the listener Deployment $job_namespace/envoy-listener names no NATS_URLS"
  verdict UNKNOWN
fi
name="legacy-subjects-purge-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
owner=$(id -un 2>/dev/null || echo unknown)
owner=$(printf '%s' "$owner" | tr -c 'A-Za-z0-9._-' '-' | cut -c1-63)
owner=${owner%-}
owner=${owner:-unknown}
jq -n --arg name "$name" --arg namespace "$job_namespace" --arg owner "$owner" --arg image "$nats_box" \
  --rawfile script "$job_script" --rawfile forms "$here/lib/legacy-subjects-gate-forms.jq" \
  --argjson deadline "$job_deadline" --arg url "$nats_url" --arg sa "$cutover_account" \
  --arg class "$cutover_seed_class" --arg file "$cutover_seed_file" --arg inbox "$cutover_inbox" \
  --arg stream_id "$stream_id" --arg last "$last_old_seq" '
  {"app.kubernetes.io/name": "legacy-subjects-purge", "created-by": "legacy-subjects-purge", owner: $owner} as $labels
  | {apiVersion: "batch/v1", kind: "Job",
     metadata: {name: $name, namespace: $namespace, labels: $labels},
     spec: {backoffLimit: 0, activeDeadlineSeconds: $deadline, ttlSecondsAfterFinished: 600,
       template: {metadata: {labels: $labels}, spec: {
         serviceAccountName: $sa, automountServiceAccountToken: false, restartPolicy: "Never",
         securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, fsGroup: 65534,
           seccompProfile: {type: "RuntimeDefault"}},
         containers: [{name: "purge", image: $image, command: ["sh", "-c", $script],
           env: [{name: "NATS_URL", value: $url}, {name: "NATS_NKEY", value: "/var/run/secrets/legacy-cutover/\($file)"},
                 {name: "WORK_DIR", value: "/work"}, {name: "FORMS_JQ", value: $forms},
                 {name: "STREAM_ID", value: $stream_id}, {name: "LAST_OLD_SEQ", value: $last},
                 {name: "NATS_INBOX_PREFIX", value: $inbox}],
           volumeMounts: [{name: "legacy-cutover-seed", mountPath: "/var/run/secrets/legacy-cutover", readOnly: true},
             {name: "legacy-subjects-purge-work", mountPath: "/work"}],
           resources: {requests: {cpu: "250m", memory: "256Mi"}, limits: {cpu: "250m", memory: "256Mi"}},
           securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}}}],
         volumes: [{name: "legacy-cutover-seed", csi: {driver: "secrets-store-gke.csi.k8s.io", readOnly: true, volumeAttributes: {secretProviderClass: $class}}},
           {name: "legacy-subjects-purge-work", emptyDir: {medium: "Memory", sizeLimit: "16Mi"}}]}}}}' >"$work/job.json"
if ! kc -n "$job_namespace" create -f "$work/job.json" -o name >"$work/create.out" 2>"$work/create.err"; then
  echo "UNKNOWN: the Job could not be created: $(first_line "$work/create.err")"
  verdict UNKNOWN
fi
job=$name
echo "JOB: created $job_namespace/$job at $(date -u +%Y-%m-%dT%H:%M:%SZ), owner $owner"
deadline=$((SECONDS + job_deadline + 60))
outcome=""
while :; do
  if kc -n "$job_namespace" get job "$job" -o json >"$work/job-status.json" 2>/dev/null; then
    outcome=$(jq -r 'if (.status.succeeded // 0) >= 1 then "succeeded" elif (.status.failed // 0) >= 1 then "failed" else "" end' "$work/job-status.json")
  fi
  [ -z "$outcome" ] || break
  if [ "$SECONDS" -ge "$deadline" ]; then
    outcome="not finished after $((job_deadline + 60)) s"
    break
  fi
  sleep 2
done
log_ok=1
kc -n "$job_namespace" logs "job/$job" >"$work/job.log" 2>"$work/logs.err" || log_ok=0
delete_job

# --- the judgement --------------------------------------------------------------------------------
if [ "$log_ok" = 0 ]; then
  echo "UNKNOWN: the Job ($outcome) left no log that could be read: $(first_line "$work/logs.err")"
  verdict UNKNOWN
fi
jq -R -c 'split("\t") | select(length == 5)
  | {leg: .[0], arg: .[1], rc: (.[2] | tonumber? // -1), out: (.[3] | @base64d), err: (.[4] | @base64d)}' \
  "$work/job.log" >"$work/lines.jsonl" 2>/dev/null || : >"$work/lines.jsonl"
jq -r -s -L "$here/lib" --arg outcome "$outcome" 'include "legacy-subjects-gate-forms";
  def lastline: split("\n") | map(select(length > 0)) | last // "no error text";
  def page: (.out | try fromjson catch null);
  . as $l
  | ($l | map(select(.leg == "refuse")) | first) as $refuse
  | ($l | map(select(.leg == "purge"))) as $purges
  | ($l | map(select(.leg == "after"))) as $after
  | if ($l | map(select(.leg == "end")) | length) == 0 then "UNKNOWN: the Job (\($outcome)) left a log that ends before its last call\nUNKNOWN"
    elif $refuse then "REFUSED: \($refuse.out | printable)\nREFUSED"
    else
      ([$purges[] | page as $p
        | if .rc == 0 and ($p | type) == "object" and $p.type == "io.nats.jetstream.api.v1.stream_purge_response" and $p.success == true
          then "PURGED: \(.arg | printable), \($p.purged // 0) message(s)"
          else "UNKNOWN: the purge of \(.arg | printable) was not acknowledged: \(if (.err | length) > 0 then .err | lastline | printable else (.out | printable) end)" end]) as $purgelines
      | ([$after[] | page as $p
          | if ($p | type) != "object" or $p.type != "io.nats.jetstream.api.v1.stream_info_response" or $p.error or (($p.state.subjects // {}) | length) != ($p.total // -1)
            then "the stream'"'"'s subjects under \(.arg | printable) were not read back whole" else empty end]) as $unread
      | ([$after[] | page | objects | .state.subjects // {} | keys[] | select(old_form)] | unique) as $left
      | ($purgelines | map(select(startswith("UNKNOWN")))) as $failed
      | ($purgelines | join("\n")) as $shown
      | (if $shown == "" then "" else $shown + "\n" end)
        + if ($after | length) != 4 then "UNKNOWN: the Job read the stream'"'"'s subjects back under \($after | length) of the 4 families\nUNKNOWN"
          elif ($unread | length) > 0 then ($unread | map("UNKNOWN: " + .) | join("\n")) + "\nUNKNOWN"
          elif ($left | length) > 0 then "UNKNOWN: the stream still holds \($left | length) old-form subject(s) after the purge: \($left[:5] | map(printable) | join(", "))\nUNKNOWN"
          elif ($failed | length) > 0 then "UNKNOWN"
          else "PASS: the stream holds no old-form subject after the purge of \($purges | length) subject(s)\nPURGED" end
    end' "$work/lines.jsonl" >"$work/judgement" 2>"$work/judge.err" ||
  printf 'UNKNOWN: the Job'"'"'s log could not be judged: %s\nUNKNOWN\n' "$(first_line "$work/judge.err")" >"$work/judgement"
sed '$d' "$work/judgement"
verdict "$(tail -n 1 "$work/judgement")"
