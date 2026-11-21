#!/usr/bin/env bash
# packages/envoy/scripts/cutover-read.sh --context <kube-context> --image <worker image@sha256:…>
#   (drain|filters) --projects <KEY,…>
#
# Runs `legion cutover-read drain|filters --projects <KEY,…>` (plan C's drain read and filter read,
# packages/daemon/internal/intake/cutover_read.go) in the cluster under the in-cluster listener's
# NATS grant: the reads need the hub's NATS, which a developer box does not reach, and the
# listener's credential, which only its pod mounts. So the script runs a one-off Job in namespace
# envoy that takes the ServiceAccount, NATS_URLS, NATS_NKEY_SEED_FILE and the volume holding that
# file from the listener Deployment envoy/envoy-listener as deployed, with --image, the worker image
# of the build whose reads are wanted (deploy 4's head for the drain read, pinned by digest), as
# nobody (65534) with every capability dropped, backoffLimit 0, activeDeadlineSeconds 600 and
# ttlSecondsAfterFinished 600, labelled created-by cutover-read and owner <your login>. The command
# reads the stream's info, its consumer list and stored messages by sequence, and makes no call on a
# daemon's durable.
#
# It prints the Job's creation and deletion, then the command's own lines, and exits as the command
# did: 0 on GATE OPEN, 1 on GATE SHUT, 3 on UNKNOWN. A Job that cannot be made, fails without a
# verdict, leaves no log or is not shown deleted prints UNKNOWN and exits 3. A usage error exits 2
# and reads nothing. It needs bash, kubectl (a context for the cluster that runs the listener,
# signed in as an account that may read the envoy-listener Deployment and create, read, read the
# logs of and delete Jobs and pods in envoy) and jq.
set -euo pipefail

usage() {
  [ -z "${1:-}" ] || echo "cutover-read.sh: $1" >&2
  echo "usage: cutover-read.sh --context <kube-context> --image <worker image@sha256:…> (drain|filters) --projects <KEY,…>" >&2
  exit 2
}

context=""
image=""
mode=""
projects=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) [ "$#" -ge 2 ] || usage "--context takes a value"; context=$2; shift 2 ;;
    --image) [ "$#" -ge 2 ] || usage "--image takes a value"; image=$2; shift 2 ;;
    --projects) [ "$#" -ge 2 ] || usage "--projects takes a value"; projects=$2; shift 2 ;;
    drain | filters) [ -z "$mode" ] || usage "one of drain or filters"; mode=$1; shift ;;
    *) usage "unknown argument $1" ;;
  esac
done
[ -n "$context" ] || usage "--context <kube-context> is required"
[[ $image =~ ^[A-Za-z0-9][A-Za-z0-9._/:-]*@sha256:[0-9a-f]{64}$ ]] || usage "--image must name the worker image by digest (<repository>@sha256:<64 hex>)"
[ -n "$mode" ] || usage "drain or filters is required"
[[ $projects =~ ^[A-Z][A-Z0-9]*(,[A-Z][A-Z0-9]*)*$ ]] || usage "--projects must be project keys separated by commas"
for tool in kubectl jq; do
  command -v "$tool" >/dev/null 2>&1 || usage "$tool is required and is not on PATH"
done
readonly context image mode projects
readonly job_namespace=envoy listener_deployment=envoy-listener job_deadline=600

work=$(mktemp -d)
readonly work
job=""
job_delete_tried=0
job_deleted=0

kc() {
  kubectl --context "$context" --request-timeout=60s "$@"
}

first_line() {
  awk 'NF {print; exit}' "$1"
}

unknown() {
  echo "UNKNOWN: $1"
  echo "UNKNOWN"
  exit 3
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
    echo "JOB: $job_namespace/$job was not shown deleted; delete it: kubectl --context $context -n $job_namespace delete job $job"
  fi
}

cleanup() {
  delete_job >&2 || true
  rm -rf "$work"
}
trap cleanup EXIT

if ! kc -n "$job_namespace" get deployment "$listener_deployment" -o json >"$work/deployment.json" 2>"$work/deployment.err"; then
  unknown "the listener Deployment $job_namespace/$listener_deployment could not be read: $(first_line "$work/deployment.err")"
fi
jq -c '.spec.template.spec as $s
  | [$s.containers[] | select(.image | test("(^|/)legion/envoy([@:]|$)"))
      | select((.command // []) as $c | ($c | length) == 0 or ($c[0] | split("/") | last) == "envoy-listener")] as $listeners
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
    if .seed == null then "a NATS seed file (NATS_NKEY_SEED_FILE)" else empty end,
    if .seed != null and .mount == null then "a volume mount holding \(.seed)" else empty end,
    if .mount != null and .volume == null then "the volume \(.mount.name)" else empty end] | join("; ")' "$work/identity.json")
[ -z "$missing" ] || unknown "the listener Deployment $job_namespace/$listener_deployment lacks $missing"

job="cutover-read-${mode}-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
owner=$(id -un 2>/dev/null | tr -c 'A-Za-z0-9._-' '-' | cut -c1-63)
owner=${owner%-}
jq -n --arg name "$job" --arg namespace "$job_namespace" --arg owner "${owner:-unknown}" --arg image "$image" \
  --arg mode "$mode" --arg projects "$projects" --argjson deadline "$job_deadline" --slurpfile identity "$work/identity.json" '
  $identity[0] as $id
  | {"app.kubernetes.io/name": "cutover-read", "created-by": "cutover-read", owner: $owner} as $labels
  | {apiVersion: "batch/v1", kind: "Job",
     metadata: {name: $name, namespace: $namespace, labels: $labels},
     spec: {backoffLimit: 0, activeDeadlineSeconds: $deadline, ttlSecondsAfterFinished: 600,
       template: {metadata: {labels: $labels}, spec: {
         serviceAccountName: $id.sa, restartPolicy: "Never", automountServiceAccountToken: false,
         securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, fsGroup: 65534,
           seccompProfile: {type: "RuntimeDefault"}},
         containers: [{name: "read", image: $image, command: ["legion", "cutover-read", $mode, "--projects", $projects],
           env: [{name: "NATS_URLS", value: $id.urls}, {name: "NATS_NKEY_SEED_FILE", value: $id.seed}],
           volumeMounts: [$id.mount + {readOnly: true}],
           resources: {requests: {cpu: "250m", memory: "256Mi"}, limits: {cpu: "250m", memory: "256Mi"}},
           securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}}}],
         volumes: [$id.volume]}}}}' >"$work/job.json"
if ! kc -n "$job_namespace" create -f "$work/job.json" -o name >"$work/create.out" 2>"$work/create.err"; then
  job=""
  unknown "the Job could not be created: $(first_line "$work/create.err")"
fi
echo "JOB: created $job_namespace/$job at $(date -u +%Y-%m-%dT%H:%M:%SZ), owner ${owner:-unknown}, image $image"
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
[ "$job_deleted" = 1 ] || unknown "the Job was not shown deleted"
[ "$log_ok" = 1 ] || unknown "the Job ($outcome) left no log that could be read: $(first_line "$work/logs.err")"
cat "$work/job.log"
verdict=$(awk 'NF {last = $0} END {print last}' "$work/job.log")
case "$outcome:$verdict" in
  "succeeded:GATE OPEN") exit 0 ;;
  "failed:GATE SHUT") exit 1 ;;
  "failed:UNKNOWN") exit 3 ;;
esac
unknown "the Job $outcome and its log ends with $(printf '%q' "$verdict"), which is no verdict of the read"
