#!/usr/bin/env bash
# packages/envoy/scripts/jetstream-account-probe.sh --context <kube-context> [--token-file <path>]
#   [--inbox-prefix <prefix>] [--remover-seed-secret <secret>] [--image <image>]
#
# Says whether a client of the clustered hub can still have a JetStream answer stored on a subject
# a stream captures, the reading t-c-29's move closes for good once its switch places every client
# in ENVOY, the account that reaches JetStream only through imports. It
# runs `jetstream-account-move probe` (packages/envoy/cmd/jetstream-account-move/probe.go) from a
# one-off Job in namespace envoy that connects as the in-cluster listener does, and prints what the
# probe printed:
#
#   1. its ordinary request, $JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS, must come back with the
#      stream's configuration, else UNKNOWN: an identity refused everything stores nothing and
#      would read clean;
#   2. its control, a plain publish to $KV.envoy_session_consumers.probe, a KV key the listener may
#      not write, must be refused, else UNKNOWN;
#   3. the same request with its reply aimed at notifications.dispatch.issue.ZZPROBE.1, a subject
#      the stream captures and no project uses, must leave nothing stored there.
#
# The last line is the verdict: GATE OPEN (exit 0) when the ordinary request answered, the control
# was refused and nothing was stored; GATE SHUT (exit 1) when anything was stored, each stored
# sequence removed by the JetStream account's operator when --remover-seed-secret names the Secret
# in namespace envoy holding its nkey seed under the key seed (a REMOVED line each), and named as
# left otherwise (a LEFT line each); UNKNOWN (exit 3) when the probe could not read what it needs, or
# the Job could not be made, read or shown deleted. A usage error exits 2.
#
# The Job: namespace envoy, the ServiceAccount, NATS URLs and credential of the listener
# Deployment envoy/envoy-listener as it is deployed (read at each run): its NATS seed file
# (NATS_NKEY_SEED_FILE) and the volume holding it, or, with --token-file <path>, the listener's
# projected NATS token at that path in the Deployment and the volume holding it, which a hub
# behind the auth service takes in place of a seed; with --inbox-prefix <prefix>, the probe takes
# that inbox prefix, the only one such a grant lets the identity subscribe. It runs the listener
# container's own image (the envoy image, which carries jetstream-account-move), or --image, as
# nobody (65534) with every capability dropped, no Kubernetes API token mounted, backoffLimit 0 (a
# probe run again would store again), activeDeadlineSeconds 120 and ttlSecondsAfterFinished 600;
# labels app.kubernetes.io/name and created-by jetstream-account-probe and owner <your login>. The
# script waits for it, reads its log, deletes it and reads back that no Job of its name is left; one
# it could not delete is UNKNOWN and named with the command that deletes it.
#
# It needs bash, kubectl (a context for the cluster running the hub, signed in as an account that
# may read the envoy-listener Deployment and create, read, read the logs of and delete Jobs in
# envoy), jq and od.
set -euo pipefail

usage() {
  [ -z "${1:-}" ] || echo "jetstream-account-probe.sh: $1" >&2
  echo "usage: jetstream-account-probe.sh --context <kube-context> [--token-file <path>] [--inbox-prefix <prefix>] [--remover-seed-secret <secret>] [--image <image>]" >&2
  exit 2
}

context=""
token_file=""
inbox_prefix=""
remover_secret=""
image=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) [ "$#" -ge 2 ] || usage "--context needs a value"; context=$2; shift 2 ;;
    --token-file) [ "$#" -ge 2 ] || usage "--token-file needs a value"; token_file=$2; shift 2 ;;
    --inbox-prefix) [ "$#" -ge 2 ] || usage "--inbox-prefix needs a value"; inbox_prefix=$2; shift 2 ;;
    --remover-seed-secret) [ "$#" -ge 2 ] || usage "--remover-seed-secret needs a value"; remover_secret=$2; shift 2 ;;
    --image) [ "$#" -ge 2 ] || usage "--image needs a value"; image=$2; shift 2 ;;
    *) usage "unknown argument $1" ;;
  esac
done
[ -n "$context" ] || usage "--context is required"
for tool in kubectl jq od; do
  command -v "$tool" >/dev/null || usage "$tool is not on PATH"
done

readonly job_namespace=envoy
readonly listener_deployment=envoy-listener
readonly job_deadline=120
readonly remover_mount=/var/run/jetstream-account-probe/remover

work=$(mktemp -d "${TMPDIR:-/tmp}/jetstream-account-probe.XXXXXX")
job=""
job_deleted=0
cleanup() { rm -rf "$work"; }
trap cleanup EXIT

kc() { kubectl --context "$context" --request-timeout=30s "$@"; }
first_line() { sed -n 1p "$1" | cut -c1-300; }

# unknown prints the reason and the verdict, and exits 3.
unknown() {
  echo "UNKNOWN $1"
  echo "UNKNOWN"
  exit 3
}

# delete_job deletes the Job and reads back that it is gone.
delete_job() {
  [ -n "$job" ] || return 0
  kc -n "$job_namespace" delete job "$job" --cascade=foreground --wait=true >/dev/null 2>"$work/delete.err" || true
  if [ -z "$(kc -n "$job_namespace" get job "$job" --ignore-not-found -o name 2>/dev/null)" ]; then
    job_deleted=1
    echo "JOB: deleted $job_namespace/$job"
  fi
}

# --- the listener's identity, as it is deployed ---------------------------------------------------
if ! kc -n "$job_namespace" get deployment "$listener_deployment" -o json >"$work/deployment.json" 2>"$work/deployment.err"; then
  unknown "the listener Deployment $job_namespace/$listener_deployment could not be read: $(first_line "$work/deployment.err")"
fi
if ! jq --arg token "$token_file" '
  .spec.template.spec as $pod
  | [$pod.containers[] | select(.image | test("(^|/)legion/envoy([@:]|$)"))] as $listeners
  | ($listeners[0] // {}) as $c
  | (($c.env // []) | map({key: .name, value: .value}) | from_entries) as $env
  | (if $token != "" then $token else $env.NATS_NKEY_SEED_FILE end) as $file
  | ([($c.volumeMounts // [])[] | . as $m | select($file != null and ($file | startswith($m.mountPath + "/")))] | first) as $mount
  | {listeners: ($listeners | length), sa: $pod.serviceAccountName, image: $c.image, urls: $env.NATS_URLS,
     credential: (if $token != "" then "token" else "seed" end), file: $file, mount: $mount,
     volume: (if $mount == null then null else ([($pod.volumes // [])[] | select(.name == $mount.name)] | first) end)}' \
  "$work/deployment.json" >"$work/identity.json" 2>"$work/identity.err"; then
  unknown "the listener Deployment $job_namespace/$listener_deployment could not be read for its identity: $(first_line "$work/identity.err")"
fi
missing=$(jq -r '[if .listeners != 1 then "one listener container (it has \(.listeners))" else empty end,
    if .sa == null then "a ServiceAccount" else empty end,
    if .urls == null then "NATS_URLS" else empty end,
    if .file == null then "a NATS seed file (NATS_NKEY_SEED_FILE): name the listener'"'"'s projected NATS token with --token-file on a hub behind the auth service" else empty end,
    if .file != null and .mount == null then "a volume mount holding \(.file)" else empty end,
    if .mount != null and .volume == null then "the volume \(.mount.name)" else empty end] | join("; ")' "$work/identity.json")
[ -z "$missing" ] || unknown "the listener Deployment $job_namespace/$listener_deployment lacks $missing"
[ -n "$image" ] || image=$(jq -r .image "$work/identity.json")

# --- the Job --------------------------------------------------------------------------------------
job="jetstream-account-probe-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
owner=$(id -un 2>/dev/null || echo unknown)
owner=$(printf '%s' "$owner" | tr -c 'A-Za-z0-9._-' '-' | cut -c1-63)
owner=${owner%-}
owner=${owner:-unknown}
jq -n --arg name "$job" --arg namespace "$job_namespace" --arg owner "$owner" --arg image "$image" \
  --argjson deadline "$job_deadline" --arg inbox "$inbox_prefix" --arg remover "$remover_secret" \
  --arg remover_mount "$remover_mount" --slurpfile identity "$work/identity.json" '
  $identity[0] as $id
  | {"app.kubernetes.io/name": "jetstream-account-probe", "created-by": "jetstream-account-probe", owner: $owner} as $labels
  | (["jetstream-account-move", "probe", "--servers", $id.urls,
      (if $id.credential == "token" then "--token-file" else "--nkey-seed-file" end), $id.file]
     + (if $inbox == "" then [] else ["--inbox-prefix", $inbox] end)
     + (if $remover == "" then [] else ["--remover-seed-file", ($remover_mount + "/seed")] end)) as $command
  | {apiVersion: "batch/v1", kind: "Job",
     metadata: {name: $name, namespace: $namespace, labels: $labels},
     spec: {backoffLimit: 0, activeDeadlineSeconds: $deadline, ttlSecondsAfterFinished: 600,
       template: {metadata: {labels: $labels}, spec: {
         serviceAccountName: $id.sa, automountServiceAccountToken: false, restartPolicy: "Never",
         securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, fsGroup: 65534,
           seccompProfile: {type: "RuntimeDefault"}},
         containers: [{name: "probe", image: $image, command: $command,
           volumeMounts: ([$id.mount + {readOnly: true}]
             + (if $remover == "" then [] else [{name: "remover", mountPath: $remover_mount, readOnly: true}] end)),
           resources: {requests: {cpu: "100m", memory: "128Mi"}, limits: {cpu: "100m", memory: "128Mi"}},
           securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}}}],
         volumes: ([$id.volume]
           + (if $remover == "" then [] else [{name: "remover", secret: {secretName: $remover, items: [{key: "seed", path: "seed"}]}}] end))}}}}' \
  >"$work/job.json"
if ! kc -n "$job_namespace" create -f "$work/job.json" -o name >"$work/create.out" 2>"$work/create.err"; then
  job=""
  unknown "the Job could not be created: $(first_line "$work/create.err")"
fi
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

verdict=UNKNOWN
if [ "$log_ok" = 1 ]; then
  cat "$work/job.log"
  last=$(sed -n '$p' "$work/job.log")
  case "$last" in
    "GATE OPEN" | "GATE SHUT" | UNKNOWN) verdict=$last ;;
    *) echo "UNKNOWN the probe's log ends with no verdict (the Job $outcome): $(printf '%s' "$last" | cut -c1-200)" ;;
  esac
else
  echo "UNKNOWN the Job ($outcome) left no log that could be read: $(first_line "$work/logs.err")"
fi
if [ "$job_deleted" = 0 ]; then
  echo "UNKNOWN the Job $job_namespace/$job was not shown deleted; delete it: kubectl --context $context -n $job_namespace delete job $job"
  [ "$verdict" = "GATE SHUT" ] || verdict=UNKNOWN
fi
echo "$verdict"
case "$verdict" in
  "GATE OPEN") exit 0 ;;
  "GATE SHUT") exit 1 ;;
  *) exit 3 ;;
esac
