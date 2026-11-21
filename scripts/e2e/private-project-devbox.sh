#!/usr/bin/env bash
# c-private-legion (private projects, verification): a private project's Legion daemon works on the
# shared hub and listener behind the auth service, and no message crosses between it and a public
# project. A live proof on a devbox, as stage3-devbox-workflow.sh is for a public project. It runs
# against the deployment the operator names, after private-project support is deployed there, and
# writes only to the two scratch projects it is given and the scratch repository.
#
# What it starts, on this devbox, from this checkout:
#   two proof daemons   this checkout's `legion`, each on a scratch Postgres of its own: the private
#                       one for the private scratch project, connected to the shared hub as the
#                       private project's own daemon identity (its token's entry is held to that
#                       project by natsgrant.WithinProjects, so its consumers, legion-go-<P>-*, are
#                       this proof's alone and no deployed daemon's durable is touched); and the
#                       control, the same for the public scratch project, which must record none of
#                       the private project's events while it records its own.
# What it reads of the deployment, taking no message:
#   each deployed daemon's consumers, with that daemon's own credential, by CONSUMER.INFO (filter
#   subjects, delivered, pending), before and after the run; the listener's
#   envoy_listener_project_withheld_total{family}, before and after; the private controller's pods
#   in its namespace; and a watch of the pods deleted there during the run.
#
# The run: the private daemon drives one scratch issue of the private project through the workflow
# to done (its architect, its phase workers, a pull request on the scratch repository, review, the
# merge), the operator approving its spec when Dispatch asks. Meanwhile the public project gets one
# comment on a public issue and one message to another session of the public project, so the private
# daemon has a public message to be refused. The scratch repository's GitHub events reach both
# daemons and are not judged.
#
# Then lib/private-project-audit.ts judges:
#   - what each proof daemon received, from its own record: the events its store took
#     (processed_events, each named with the project Dispatch's event log gives it) and the issues it
#     admitted; neither holds the other project's;
#   - each deployed daemon's consumers: its own project's filters, and delivered counts that moved;
#   - the controller's pod runs as legion-<p>-controller in legion-<p>, and it deleted a finished
#     issue pod of its project during the run.
# A read whose identity the run does not hold prints `not run: <read> (<the identity it needs>)` and
# fails the run, as does any judgement that failed or read nothing (UNKNOWN).
#
# Inputs (each required unless a default is named):
#   LEGION_E2E_PRIVATE_PROJECT         the private scratch project's key, provisioned private with
#                                      Open: true, so the deployment's quota lets its pods run
#   LEGION_E2E_PUBLIC_PROJECT          a public scratch project's key
#   LEGION_E2E_PUBLIC_PEER_SESSION     a live session of the public project, sent the public message
#   LEGION_E2E_KUBE_CONTEXT            the operator's context of the deployment's cluster
#   LEGION_E2E_NATS_URL                the shared hub
#   LEGION_E2E_PRIVATE_DAEMON_TOKEN_FILE  the private project's daemon identity's platform token for
#                                      the hub's NATS audience
#   LEGION_E2E_CONTROL_DAEMON_TOKEN_FILE  the same, for the public scratch project's identity
#   LEGION_E2E_DAEMON_CONFIG           a legion.yaml template for a proof daemon, in which the run
#                                      fills @PROJECT@, @DATABASE_URL@, @STATE_DIR@, @NATS_URL@ and
#                                      @NATS_TOKEN_FILE@ (the deployment's own keys, which this
#                                      public repository does not hold)
#   LEGION_E2E_SCRATCH_REPO            the scratch repository (default sjawhar/legion-smoke)
#   LEGION_E2E_DEPLOYED_DAEMON_TOKEN_FILE  optional: a deployed public daemon's own credential, for
#                                      its consumers' CONSUMER.INFO; without it that read is not run
#   LEGION_E2E_LISTENER_METRICS_URL    optional: the listener's /metrics; without it the withheld
#                                      count is not read
# The operator's Dispatch is read as every command reads it (lib/operator-dispatch.sh).
#
#   LEGION_E2E_PRIVATE_PROJECT=… LEGION_E2E_PUBLIC_PROJECT=… … bash scripts/e2e/private-project-devbox.sh
set -Eeuo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d "/tmp/private-project-devbox.$$.XXXXXXXX")
evidence=${PRIVATE_PROJECT_EVIDENCE_DIR:-$(mktemp -d /tmp/private-project-devbox-evidence.XXXXXXXX)}
mkdir -p "$evidence/records" "$work/logs"
check=setup
started=$SECONDS
note() { echo "   $*"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}
begin() {
  check=$1
  echo "== $check"
}
pass() { echo "ok $check"; }
# shellcheck disable=SC2034 # read by lib/rig.sh's until_true
timeout_hook=''
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/operator-dispatch.sh
. "$root/scripts/e2e/lib/operator-dispatch.sh"

: >"$evidence/unrun"
not_run() { echo "not run: $1 ($2)" | tee -a "$evidence/unrun"; }

private_daemon_pid='' control_daemon_pid='' watch_pid=''
containers=()
cleanup() {
  local rc=$1 name
  trap - EXIT INT TERM
  set +e
  stop_pid "$private_daemon_pid"
  stop_pid "$control_daemon_pid"
  stop_tree "$watch_pid"
  for name in "${containers[@]}"; do docker rm -f -v "$name" >/dev/null 2>&1; done
  rm -rf -- "${work:?}"
  echo "private project devbox e2e: $([ "$rc" -eq 0 ] && echo PASS || echo "FAIL at $check") after $((SECONDS - started))s (evidence: $evidence)"
  exit "$rc"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

begin prerequisites
for tool in go docker jq curl kubectl gh bun psql; do command -v "$tool" >/dev/null || fail "$tool is required"; done
for name in LEGION_E2E_PRIVATE_PROJECT LEGION_E2E_PUBLIC_PROJECT LEGION_E2E_PUBLIC_PEER_SESSION LEGION_E2E_KUBE_CONTEXT \
  LEGION_E2E_NATS_URL LEGION_E2E_PRIVATE_DAEMON_TOKEN_FILE LEGION_E2E_CONTROL_DAEMON_TOKEN_FILE LEGION_E2E_DAEMON_CONFIG; do
  [ -n "${!name:-}" ] || fail "$name is required (the header of this script says what it names)"
done
private=$LEGION_E2E_PRIVATE_PROJECT public=$LEGION_E2E_PUBLIC_PROJECT
[ "$private" != "$public" ] || fail "the private and the public scratch project are one project, $private"
repo=${LEGION_E2E_SCRATCH_REPO:-sjawhar/legion-smoke}
context=$LEGION_E2E_KUBE_CONTEXT
# The private project's namespace and its controller's account, as the deployment names them
# (legion.ProjectNamespace and the controller account of the deployment repository's legion package).
namespace=legion-${private,,}
controller_account=$namespace-controller
for file in "$LEGION_E2E_PRIVATE_DAEMON_TOKEN_FILE" "$LEGION_E2E_CONTROL_DAEMON_TOKEN_FILE" "$LEGION_E2E_DAEMON_CONFIG"; do
  [ -r "$file" ] || fail "$file cannot be read"
done
operator_dispatch
[ -n "$operator_dispatch_url" ] || fail "the operator's Dispatch: $operator_dispatch_notice"
(umask 077 && printf 'Authorization: Bearer %s\n' "$operator_dispatch_token" >"$work/dispatch-header")
dispatch() { curl -fsS --max-time 30 -H "@$work/dispatch-header" -H 'Content-Type: application/json' "$@"; }
visibility() { dispatch "$operator_dispatch_url/api/v1/projects/$1" | jq -r '.visibility // empty'; }
[ "$(visibility "$private")" = private ] || fail "$private is not private in the operator's Dispatch"
[ "$(visibility "$public")" = public ] || fail "$public is not public in the operator's Dispatch"
note "$private is private and $public public in $operator_dispatch_url; the scratch repository is $repo"
pass

begin deployment
kubectl --context "$context" get namespace "$namespace" >/dev/null 2>&1 ||
  fail "the deployment has no namespace $namespace: the private project's namespaces are not deployed (#t-c-12)"
kubectl --context "$context" -n "$namespace" get serviceaccount "$controller_account" >/dev/null 2>&1 ||
  fail "the deployment has no account $controller_account in $namespace (#t-c-12)"
pass

begin build
go -C "$root/packages/daemon" build -o "$work/legion" ./cmd/legion || fail "this checkout's legion did not build"
pass

# consumer_info LABEL: the deployed public daemon's consumers, read with its own credential.
consumer_info() {
  bun "$root/scripts/e2e/lib/private-project-audit.ts" consumer-info --servers "$LEGION_E2E_NATS_URL" \
    --token-file "$LEGION_E2E_DEPLOYED_DAEMON_TOKEN_FILE" --out "$evidence/records/consumers-$1.jsonl" \
    --durable "legion-go-$public-dispatch" --durable "legion-go-$public-github" \
    --durable "legion-go-$public-wake" --durable "legion-go-$public-ready"
}
# withheld LABEL: the listener's withheld counts, by family.
withheld() {
  curl -fsS --max-time 20 "$LEGION_E2E_LISTENER_METRICS_URL" | grep '^envoy_listener_project_withheld_total' \
    >"$evidence/records/withheld-$1.txt" || true
}

begin baseline
if [ -n "${LEGION_E2E_DEPLOYED_DAEMON_TOKEN_FILE:-}" ]; then
  consumer_info before || fail "the deployed $public daemon's consumers could not be read with its credential"
else
  not_run "deployed daemon consumer info" "LEGION_E2E_DEPLOYED_DAEMON_TOKEN_FILE, the deployed $public daemon's own credential"
fi
if [ -n "${LEGION_E2E_LISTENER_METRICS_URL:-}" ]; then
  withheld before
else
  not_run "listener withheld count" "LEGION_E2E_LISTENER_METRICS_URL, the listener's /metrics"
fi
baseline_event=$(dispatch "$operator_dispatch_url/api/v1/events/log?after=0&limit=1&order=desc" | jq -r '.[0].id // 0')
run_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# Every pod of the private namespace deleted from here on, with the object as it was.
(kubectl --context "$context" -n "$namespace" get pods --watch --output-watch-events -o json |
  jq -c --unbuffered . >"$evidence/records/pod-watch.jsonl") 2>"$work/logs/pod-watch.log" &
watch_pid=$!
pass

# start_daemon LABEL PROJECT TOKEN: a proof daemon for PROJECT on a scratch Postgres of its own,
# connected to the shared hub with the identity TOKEN holds; sets <LABEL>_daemon_pid and
# <LABEL>_database_url.
start_daemon() {
  local label=$1 project=$2 token=$3 container port config
  container="private-project-devbox-$$-$label"
  # A scratch server bound to loopback alone, trusting its one user, so no URL carries a password.
  docker run -d --name "$container" --cpus 2 --memory 2g --mount type=tmpfs,destination=/var/lib/postgresql \
    -e POSTGRES_USER=legion -e POSTGRES_HOST_AUTH_METHOD=trust -e POSTGRES_DB=legion -p 127.0.0.1::5432 \
    mirror.gcr.io/pgvector/pgvector@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11 >/dev/null ||
    fail "the $label daemon's Postgres did not start"
  containers+=("$container")
  port=$(docker port "$container" 5432/tcp | sed -n '1s/.*://p')
  printf -v "${label}_database_url" 'postgres://legion@127.0.0.1:%s/legion?sslmode=disable' "$port"
  local url_var=${label}_database_url
  until_true 60 "the $label daemon's Postgres to accept connections" psql "${!url_var}" -Atc 'select 1'
  mkdir -p "$work/state-$label"
  config=$(<"$LEGION_E2E_DAEMON_CONFIG")
  config=${config//@PROJECT@/$project}
  config=${config//@DATABASE_URL@/${!url_var}}
  config=${config//@STATE_DIR@/$work/state-$label}
  config=${config//@NATS_URL@/$LEGION_E2E_NATS_URL}
  config=${config//@NATS_TOKEN_FILE@/$token}
  printf '%s\n' "$config" >"$work/legion-$label.yaml"
  start_process "${label}_daemon" "$work/legion" start --config "$work/legion-$label.yaml"
}

begin daemons
start_daemon private "$private" "$LEGION_E2E_PRIVATE_DAEMON_TOKEN_FILE"
start_daemon control "$public" "$LEGION_E2E_CONTROL_DAEMON_TOKEN_FILE"
until_true 120 "the private daemon to be ready" grep -q '"readyz"\|ready' "$evidence/logs/private_daemon.log"
until_true 120 "the control daemon to be ready" grep -q '"readyz"\|ready' "$evidence/logs/control_daemon.log"
pass

begin private-issue
marker_private="$private-marker-$(openssl rand -hex 6)"
issue=$(dispatch -X POST "$operator_dispatch_url/api/v1/issues" -d "$(jq -cn --arg p "$private" --arg m "$marker_private" \
  '{project: $p, title: ("Private project proof " + $m), spec: ("# Spec\n\nAppend the line `" + $m + "` to README.md.\n")}')" |
  jq -r .key) || fail "the operator's Dispatch did not open the private scratch issue"
note "the private scratch issue is $issue: approve its spec in Dispatch when asked ($operator_dispatch_url)"
status_of() { dispatch "$operator_dispatch_url/api/v1/issues/$1" | jq -r '.status // empty'; }
issue_done() { [ "$(status_of "$issue")" = "done" ]; }
pass

begin public-writes
marker_public="$public-marker-$(openssl rand -hex 6)"
public_issue=$(dispatch -X POST "$operator_dispatch_url/api/v1/issues" -d "$(jq -cn --arg p "$public" --arg m "$marker_public" \
  '{project: $p, title: ("Public project proof " + $m), spec: "# Spec\n\nA public issue of the private project proof.\n"}')" |
  jq -r .key) || fail "the operator's Dispatch did not open the public scratch issue"
dispatch -X POST "$operator_dispatch_url/api/v1/issues/$public_issue/comments" \
  -d "$(jq -cn --arg m "$marker_public" '{body: ("A public remark (" + $m + ").")}')" >/dev/null ||
  fail "the comment on $public_issue was refused"
dispatch -X POST "$operator_dispatch_url/api/v1/agents/$LEGION_E2E_PUBLIC_PEER_SESSION/messages" \
  -d "$(jq -cn --arg m "$marker_public" '{body: ("A public word (" + $m + ")."), delivery: "btw"}')" >/dev/null ||
  fail "the message to $LEGION_E2E_PUBLIC_PEER_SESSION was refused"
pass

begin workflow
until_true 14400 "$issue to be done: its architect, its phase workers, its pull request, review and the merge" issue_done
merged=$(gh -R "$repo" pr list --state merged --head "legion/$issue" --json number --jq 'length')
[ "$merged" -ge 1 ] || fail "no pull request of legion/$issue was merged into $repo"
pass

begin receipts
# What each proof daemon received, from its own record: the events its store took, each named with
# the project Dispatch's event log gives it, and the issues it admitted.
for label in private control; do
  url_var=${label}_database_url
  psql "${!url_var}" -Atc "select coalesce(json_agg(json_build_object('source', source, 'event_id', event_id)), '[]')
    from processed_events where processed_at >= '$run_started'" >"$work/processed-$label.json"
  psql "${!url_var}" -Atc "select coalesce(json_agg(json_build_object('key', key, 'project', project)), '[]') from issues" \
    >"$work/issues-$label.json"
  : >"$evidence/records/receipts-$label.jsonl"
  while IFS=$'\t' read -r source event; do
    project=''
    if [[ $event =~ ([0-9]+)$ ]]; then
      project=$(dispatch "$operator_dispatch_url/api/v1/events/log?after=$((BASH_REMATCH[1] - 1))&limit=1" |
        jq -r --argjson id "${BASH_REMATCH[1]}" '.[] | select(.id == $id) | (.project // .issue_key // "")') || project=''
    fi
    jq -cn --arg s "$source" --arg e "$event" --arg p "$project" \
      '{subject: ("processed." + $s + "." + ($p | split("-")[0]) + "." + $e), data: {source: $s, event_id: $e, project: $p}}' \
      >>"$evidence/records/receipts-$label.jsonl"
  done < <(jq -r '.[] | [.source, .event_id] | @tsv' "$work/processed-$label.json")
  jq -c '.[] | {subject: ("issue." + .project + "." + .key), data: .}' "$work/issues-$label.json" \
    >>"$evidence/records/receipts-$label.jsonl"
done
printf '%s\n' "$marker_private" "$issue" >"$evidence/records/markers-private"
printf '%s\n' "$marker_public" "$public_issue" "$LEGION_E2E_PUBLIC_PEER_SESSION" >"$evidence/records/markers-public"
pass

begin deployment-reads
consumer_args=()
if [ -n "${LEGION_E2E_DEPLOYED_DAEMON_TOKEN_FILE:-}" ]; then
  consumer_info after || fail "the deployed $public daemon's consumers could not be read again"
  consumer_args=(--consumers "deployed $public daemon's consumers,$evidence/records/consumers-before.jsonl,$evidence/records/consumers-after.jsonl,$public,$private")
fi
[ -z "${LEGION_E2E_LISTENER_METRICS_URL:-}" ] || withheld after
kubectl --context "$context" -n "$namespace" get pods -o json |
  jq --arg a "$controller_account" '{items: [.items[] | select(.spec.serviceAccountName == $a or (.metadata.name | test("controller")))]}' \
    >"$evidence/records/controller-pods.json"
stop_tree "$watch_pid"
watch_pid=''
# A deletion's principal, read from the controller's own log, which names each pod it deletes; a
# deletion its log does not name is no deletion of the controller's.
controller_pod=$(jq -r '.items[0].metadata.name // empty' "$evidence/records/controller-pods.json")
: >"$work/controller.log"
[ -z "$controller_pod" ] || kubectl --context "$context" -n "$namespace" logs "$controller_pod" --since-time="$run_started" \
  >"$work/controller.log" 2>/dev/null || true
jq -c --arg by "system:serviceaccount:$namespace:$controller_account" --rawfile log "$work/controller.log" \
  'select(.type == "DELETED") | . + {deleted_by: (if ($log | contains(.object.metadata.name)) then $by else "" end)}' \
  "$evidence/records/pod-watch.jsonl" >"$evidence/records/deletions.jsonl" || true
pass

begin audit
audit_status=0
bun "$root/scripts/e2e/lib/private-project-audit.ts" \
  --connection "private daemon's receipts,$evidence/records/receipts-private.jsonl,$public,$evidence/records/markers-public" \
  --connection "control daemon's receipts,$evidence/records/receipts-control.jsonl,$private,$evidence/records/markers-private" \
  --controller-pods "$evidence/records/controller-pods.json" --deletions "$evidence/records/deletions.jsonl" \
  --namespace "$namespace" --controller-account "$controller_account" "${consumer_args[@]}" | tee "$evidence/audit.txt" ||
  audit_status=$?
[ -z "${LEGION_E2E_LISTENER_METRICS_URL:-}" ] ||
  note "withheld before: $(tr '\n' ' ' <"$evidence/records/withheld-before.txt"); after: $(tr '\n' ' ' <"$evidence/records/withheld-after.txt")"
note "events from $baseline_event on; private issue $issue, public issue $public_issue"
[ "$audit_status" -eq 0 ] || fail "the audit failed; see $evidence/audit.txt"
[ ! -s "$evidence/unrun" ] || fail "$(wc -l <"$evidence/unrun") reads did not run: $(tr '\n' ' ' <"$evidence/unrun")"
pass
