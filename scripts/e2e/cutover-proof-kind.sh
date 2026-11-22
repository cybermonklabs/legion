#!/usr/bin/env bash
# The listener's replacement under a hold, on a throwaway kind cluster (the consumer-names cutover,
# step 3, and its rollback). A 1-replica envoy-listener Deployment whose startup probe asks /v1
# runs this tree's listener in the fixed naming mode on a NATS server in the cluster. The hold is
# that server refusing the listener's user: its configuration is swapped for one that lists the
# operator's user alone and reloaded (SIGHUP), which closes the listener's connection and refuses
# every reconnect, as the auth service does to every client but the operator while the hold row
# reads held. Then, each check printing `== <name>`, what it observed and `ok <name> (<s>s)`, the
# first that fails ending the run non-zero, naming it:
#
#   rolling-update-control  under the hold, the Deployment is set to ENVOY_CONSUMER_NAMES=fresh
#                           with today's RollingUpdate: the new pod's /v1 never opens, so the old
#                           pod stays, Ready, and the mode check reads it and stops.
#   recreate-fresh          the same template with strategy Recreate: no pod of a previous
#                           ReplicaSet is left, and the new pod's start line names `fresh` under
#                           the hold, before any convert, since every build logs its mode before
#                           its first NATS connect; the mode check passes.
#   rollback-control        after a release and under a second (rollback) hold, the Deployment is
#                           set to `fixed` with RollingUpdate: the `fresh` pod stays, and the mode
#                           check reads `fresh` and stops.
#   recreate-fixed          the rollback with Recreate: no pod of a previous ReplicaSet is left,
#                           and the pod's spec and start line read `fixed`; the mode check passes.
#   rolling-update-restored after the release, RollingUpdate is put back (the restore pull
#                           request), and a later replacement keeps the old pod Ready until the new
#                           one is Ready.
#
# The mode check is this script's own (mode_check below), the rule of the cutover's step 4 that
# the window's check applies: list the Deployment's pods by name, require that none is of a
# previous ReplicaSet, and require the wanted mode from each pod's spec and its start line
# (`session consumer naming mode`, consumer_names). A pod that logged no such line is UNKNOWN,
# which stops the check as a refusal does. Each hold and release is checked on the server itself
# before the row goes on: while held the server refuses a connection as the listener's user and
# holds none of its connections (/connz), and admits the operator's; while released it admits both.
#
# Run it from a checkout on a Linux host as
#   bash scripts/e2e/cutover-proof-kind.sh
# with docker, go, kubectl, jq and kind (LEGION_E2E_KIND names the binary when it is not on PATH)
# installed. It builds this tree's envoy-listener and the nats-server packages/envoy/go.mod pins,
# statically, into one image on busybox, and loads it into the cluster; fixturebuckets, which it
# also builds, runs from the host through a port-forward. Every kubectl call names the run's
# kubeconfig and context, so the caller's kubeconfig is
# never read, and the cluster is deleted on any exit. CUTOVER_PROOF_EVIDENCE_DIR (default a fresh
# /tmp directory, kept and printed) keeps the transcript and, on a failure, the cluster's pods,
# events and logs.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
evidence=${CUTOVER_PROOF_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e-cutover-proof-evidence.XXXXXXXX)}
mkdir -p "$evidence"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"

kind_bin=${LEGION_E2E_KIND:-kind}
cluster=legion-cutover-proof-$$
work=$(mktemp -d "/tmp/legion-e2e-cutover-proof.$$.XXXXXXXX")
kubeconfig=$work/kubeconfig
context=kind-$cluster
namespace=cutover-proof
image=cutover-proof:$$
cluster_made=
forward=
port=
check=setup
started=$SECONDS
check_started=$SECONDS

begin() { check=$1; check_started=$SECONDS; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }
k() { kubectl --kubeconfig "$kubeconfig" --context "$context" -n "$namespace" "$@"; }

cleanup() {
  local status=$?
  [ -z "$forward" ] || kill "$forward" 2>/dev/null || true
  if [ -n "$cluster_made" ]; then
    if [ "$status" -ne 0 ]; then
      {
        k get deployments,replicasets,pods -o wide
        k describe pods
        k get events --sort-by=.lastTimestamp
        k logs --all-containers --prefix --tail=400 -l 'app in (listener,nats)'
      } >"$evidence/cluster-at-failure.txt" 2>&1 || true
      echo "the cluster's pods, events and logs at the failure: $evidence/cluster-at-failure.txt" >&2
    fi
    "$kind_bin" delete cluster --name "$cluster" --kubeconfig "$kubeconfig" >/dev/null 2>&1 ||
      echo "could not delete kind cluster $cluster; delete it with: $kind_bin delete cluster --name $cluster" >&2
  fi
  docker image rm "$image" >/dev/null 2>&1 || true
  rm -rf "$work"
  if [ "$status" -eq 0 ]; then
    echo "cutover proof kind e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "cutover proof kind e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
}
trap cleanup EXIT

# The users of the NATS server: the operator's, which a hold admits, and the listener's, which it
# refuses. The listener presents its token as natscred.Service does, user "service" with the token
# as the password (NATS_SERVICE_TOKEN_FILE), so a nats-server user of that name and password is
# the listener's entry.
operator_password=operator-$$-$RANDOM
listener_token=listener-$$-$RANDOM
api_token=api-$$-$RANDOM

# nats_config FILE USERS writes a nats-server configuration with JetStream on /data, the monitor
# on 8222 (connz is how the row reads who is connected) and the users USERS, a list of
# `{user: ..., password: ...}` entries.
nats_config() {
  cat >"$1" <<EOF
listen: 0.0.0.0:4222
http_port: 8222
jetstream { store_dir: /data }
authorization { users = [ $2 ] }
EOF
}

# connections USER answers how many connections of USER the server holds, read from its /connz.
connections() {
  local connz
  connz=$(k exec deploy/nats -- wget -qO- 'http://127.0.0.1:8222/connz?auth=1&limit=1024') ||
    fail "the NATS server's /connz did not answer"
  [[ $connz == *'"connections"'* ]] || fail "the NATS server's /connz answered no connection list: $connz"
  { grep -o "\"authorized_user\": *\"$1\"" <<<"$connz" || true; } | wc -l
}

# admits USER PASSWORD reports whether the server admits a connection of USER now: fixturebuckets
# connects with it through the run's port-forward and, on a server it has provisioned already,
# changes nothing (bustest.Provision), so it is a connection and nothing else.
admits() {
  "$work/fixturebuckets" -url "nats://$1:$2@127.0.0.1:$port" >/dev/null 2>&1
}

# set_hold held|released swaps the server's configuration and reloads it, then waits until the
# server refuses the listener's user (user "service") and holds no connection of it (held), or
# admits it (released). The operator's user must be admitted either way, as the hold admits it.
set_hold() {
  local want=$1 deadline n
  k exec deploy/nats -- sh -c "cp /etc/nats/$want.conf /tmp/nats.conf && kill -HUP 1"
  deadline=$((SECONDS + 120))
  while :; do
    n=$(connections service)
    if [ "$want" = held ] && [ "$n" -eq 0 ] && ! admits service "$listener_token"; then break; fi
    if [ "$want" = released ] && admits service "$listener_token"; then break; fi
    [ $SECONDS -lt $deadline ] || fail "the server never read $want: it holds $n connections of the listener's user"
    sleep 2
  done
  admits operator "$operator_password" || fail "the server refuses the operator's user while $want"
  note "the server reads $want: it $( [ "$want" = held ] && echo refuses || echo admits) the listener's user and holds $n of its connections; it admits the operator's"
}

# current_hash answers the pod-template-hash of the listener Deployment's current ReplicaSet, the
# one whose revision is the Deployment's.
current_hash() {
  local revision rs
  revision=$(k get deployment listener -o jsonpath='{.metadata.annotations.deployment\.kubernetes\.io/revision}')
  [ -n "$revision" ] || fail "the listener Deployment carries no revision"
  rs=$(k get replicasets -l app=listener -o json |
    jq -r --arg r "$revision" '.items[] | select(.metadata.annotations["deployment.kubernetes.io/revision"] == $r) | .metadata.labels["pod-template-hash"]')
  [ -n "$rs" ] || fail "no ReplicaSet of the listener Deployment is at its revision $revision"
  echo "$rs"
}

# listener_pods prints `<pod> <pod-template-hash> <ready>` for each pod of the Deployment.
listener_pods() {
  k get pods -l app=listener -o json |
    jq -r '.items[] | select(.metadata.deletionTimestamp == null) | "\(.metadata.name) \(.metadata.labels["pod-template-hash"]) \([.status.conditions[]? | select(.type == "Ready") | .status][0] // "False")"'
}

# pod_mode POD answers the mode POD's start line names (`session consumer naming mode`,
# consumer_names), from its running container's log or, after a restart, its previous one's;
# nothing when neither holds the line.
pod_mode() {
  local logs
  logs=$(k logs "$1" 2>/dev/null || true)
  if [[ $logs != *'"msg":"session consumer naming mode"'* ]]; then
    logs=$(k logs --previous "$1" 2>/dev/null || true)
  fi
  # A log with no such line answers nothing rather than failing: under set -e a failed grep in a
  # caller's assignment would end the run without naming the check.
  { grep -F '"msg":"session consumer naming mode"' <<<"$logs" || true; } | head -n 1 | sed -n 's/.*"consumer_names":"\([a-z]*\)".*/\1/p'
}

# mode_check WANT is the window's mode check over the listener Deployment: every pod listed by
# name, none of a previous ReplicaSet, and WANT read from each pod's spec and its start line. It
# prints one line a pod and a verdict, and answers 0 when it passes, 1 when it stops on a pod that
# is of a previous ReplicaSet or reads another mode, and 2 when it cannot read a pod's mode or
# lists no pod at all (UNKNOWN, which stops the window as a refusal does).
mode_check() {
  local want=$1 hash pods pod pod_hash ready spec mode verdict=0
  hash=$(current_hash)
  pods=$(listener_pods)
  if [ -z "$pods" ]; then
    echo "   mode check: UNKNOWN: the listener Deployment lists no pod"
    return 2
  fi
  while read -r pod pod_hash ready; do
    spec=$(k get pod "$pod" -o jsonpath='{.spec.containers[0].env[?(@.name=="ENVOY_CONSUMER_NAMES")].value}')
    mode=$(pod_mode "$pod")
    if [ "$pod_hash" != "$hash" ]; then
      echo "   mode check: stop: pod $pod (Ready $ready) is of a previous ReplicaSet ($pod_hash, current $hash) and reads spec '${spec:-unset}', start line '${mode:-none}'"
      verdict=1
    elif [ -z "$mode" ]; then
      echo "   mode check: UNKNOWN: pod $pod logged no session consumer naming mode line"
      [ "$verdict" -ne 0 ] || verdict=2
    elif [ "$spec" != "$want" ] || [ "$mode" != "$want" ]; then
      echo "   mode check: stop: pod $pod reads spec '${spec:-unset}', start line '$mode'; want $want"
      verdict=1
    else
      echo "   mode check: pod $pod reads $want, spec and start line"
    fi
  done <<<"$pods"
  return "$verdict"
}

# set_listener MODE STRATEGY sets the Deployment's naming mode and its strategy, which makes a
# new ReplicaSet when the mode changes.
set_listener() {
  local strategy
  if [ "$2" = Recreate ]; then
    strategy='{"type":"Recreate","rollingUpdate":null}'
  else
    strategy='{"type":"RollingUpdate","rollingUpdate":{"maxSurge":1,"maxUnavailable":0}}'
  fi
  k patch deployment listener --type=strategic -p "{\"spec\":{\"strategy\":$strategy,\"template\":{\"spec\":{\"containers\":[{\"name\":\"envoy-listener\",\"env\":[{\"name\":\"ENVOY_CONSUMER_NAMES\",\"value\":\"$1\"}]}]}}}}" >/dev/null
  note "the Deployment is set to ENVOY_CONSUMER_NAMES=$1 with strategy $2"
}

# only_current_pods waits up to 180 s until the Deployment lists at least one pod and every pod it
# lists is of its current ReplicaSet, and fails naming what is left otherwise.
only_current_pods() {
  local hash pods deadline=$((SECONDS + 180))
  while :; do
    hash=$(current_hash)
    pods=$(listener_pods)
    if [ -n "$pods" ] && ! awk -v h="$hash" '$2 != h { found = 1 } END { exit !found }' <<<"$pods"; then
      note "no pod of a previous ReplicaSet is left: $(tr '\n' ';' <<<"$pods")"
      return 0
    fi
    [ $SECONDS -lt $deadline ] || fail "pods of a previous ReplicaSet are left after 180 s: $(tr '\n' ';' <<<"$pods") (current $hash)"
    sleep 2
  done
}

# start_line MODE waits up to 120 s for the current ReplicaSet's pod to log its start line naming
# MODE.
start_line() {
  local hash pod mode deadline=$((SECONDS + 120))
  while :; do
    hash=$(current_hash)
    pod=$(listener_pods | awk -v h="$hash" '$2 == h { print $1; exit }')
    mode=
    [ -z "$pod" ] || mode=$(pod_mode "$pod")
    if [ "$mode" = "$1" ]; then
      note "pod $pod's start line names $1"
      return 0
    fi
    [ $SECONDS -lt $deadline ] || fail "pod ${pod:-of the current ReplicaSet} logged no start line naming $1 in 120 s (it names '${mode:-nothing}')"
    sleep 2
  done
}

begin prerequisites
for tool in docker go kubectl jq "$kind_bin"; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
pass

begin image
# This tree's listener and the nats-server go.mod pins (the listener refuses a server version that
# can skip a message, internal/natsversion), each static, in one image; fixturebuckets, built here
# too, runs from the host.
mkdir -p "$work/image"
(cd "$root/packages/envoy" &&
  CGO_ENABLED=0 go build -o "$work/image/envoy-listener" ./cmd/listener &&
  CGO_ENABLED=0 go build -o "$work/fixturebuckets" ./internal/testnats/cmd/fixturebuckets &&
  CGO_ENABLED=0 go build -o "$work/image/nats-server" github.com/nats-io/nats-server/v2) ||
  fail "go build of the listener, fixturebuckets or nats-server failed"
cat >"$work/image/Dockerfile" <<'EOF'
FROM mirror.gcr.io/library/busybox:1.37
COPY envoy-listener nats-server /usr/local/bin/
EOF
docker build -q -t "$image" "$work/image" >/dev/null
note "image $image: $("$work/image/nats-server" --version), this tree's envoy-listener at $(git -C "$root" rev-parse --short HEAD)"
pass

begin cluster
"$kind_bin" create cluster --name "$cluster" --kubeconfig "$kubeconfig" --wait 180s
cluster_made=1
"$kind_bin" load docker-image "$image" --name "$cluster" >/dev/null
kubectl --kubeconfig "$kubeconfig" --context "$context" create namespace "$namespace" >/dev/null
note "kind $("$kind_bin" version)"
pass

begin nats
nats_config "$work/released.conf" "{user: operator, password: \"$operator_password\"}, {user: service, password: \"$listener_token\"}"
nats_config "$work/held.conf" "{user: operator, password: \"$operator_password\"}"
k create configmap nats --from-file=released.conf="$work/released.conf" --from-file=held.conf="$work/held.conf" >/dev/null
k create secret generic listener --from-literal=token="$listener_token" --from-literal=api="$api_token" >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: nats, labels: {app: nats}}
spec:
  replicas: 1
  selector: {matchLabels: {app: nats}}
  template:
    metadata: {labels: {app: nats}}
    spec:
      containers:
        - name: nats
          image: $image
          imagePullPolicy: Never
          command: [sh, -c, "cp /etc/nats/released.conf /tmp/nats.conf && exec nats-server -c /tmp/nats.conf"]
          ports: [{containerPort: 4222}, {containerPort: 8222}]
          volumeMounts: [{name: config, mountPath: /etc/nats}, {name: data, mountPath: /data}]
          readinessProbe: {tcpSocket: {port: 4222}, periodSeconds: 2}
      volumes: [{name: config, configMap: {name: nats}}, {name: data, emptyDir: {}}]
---
apiVersion: v1
kind: Service
metadata: {name: nats}
spec: {selector: {app: nats}, ports: [{name: client, port: 4222}]}
EOF
k rollout status deployment/nats --timeout=180s >/dev/null
# The buckets a listener copies, which no listener creates: fixturebuckets provisions only a
# server on its own loopback, so it reaches this one through a port-forward, as the operator. The
# port-forward stays for the run: each hold and release is checked through it (admits).
k port-forward deployment/nats :4222 >"$work/port-forward.log" 2>&1 &
forward=$!
deadline=$((SECONDS + 60))
until port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) -> 4222$/\1/p' "$work/port-forward.log") && [ -n "$port" ]; do
  [ $SECONDS -lt $deadline ] || fail "the port-forward to the NATS server never listened: $(cat "$work/port-forward.log")"
  sleep 1
done
"$work/fixturebuckets" -url "nats://operator:$operator_password@127.0.0.1:$port" ||
  fail "fixturebuckets could not provision the NATS server"
note "the NATS server holds the buckets a listener copies"
pass

begin listener
# The probes: the startup probe asks /v1 and passes only once /v1 has opened (GET /v1/sessions
# answers 200 to the bearer; the starting gate answers 503), which needs the listener's NATS
# connection; the readiness probe asks /livez, which answers while the process runs, so a pod the
# hold cut off from NATS stays Ready, as a running pod does.
k apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: listener, labels: {app: listener}}
spec:
  replicas: 1
  selector: {matchLabels: {app: listener}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxSurge: 1, maxUnavailable: 0}}
  template:
    metadata: {labels: {app: listener}}
    spec:
      terminationGracePeriodSeconds: 10
      containers:
        - name: envoy-listener
          image: $image
          imagePullPolicy: Never
          command: [envoy-listener]
          env:
            - {name: ENVOY_CONSUMER_NAMES, value: fixed}
            - {name: PORT, value: "9020"}
            - {name: ENVOY_LISTEN_HOST, value: 0.0.0.0}
            - {name: ENVOY_MACHINE_ID, value: cutover-proof}
            - {name: NATS_URLS, value: "nats://nats:4222"}
            - {name: ENVOY_ALLOW_REMOTE_NATS, value: "1"}
            - {name: NATS_SERVICE_TOKEN_FILE, value: /var/run/envoy/token}
            - {name: ENVOY_API_TOKEN, valueFrom: {secretKeyRef: {name: listener, key: api}}}
          volumeMounts: [{name: token, mountPath: /var/run/envoy, readOnly: true}]
          startupProbe:
            exec: {command: [sh, -c, 'wget -q -O /dev/null --header "Authorization: Bearer \$ENVOY_API_TOKEN" http://127.0.0.1:9020/v1/sessions']}
            periodSeconds: 5
            failureThreshold: 60
          readinessProbe:
            exec: {command: [sh, -c, 'wget -q -O /dev/null http://127.0.0.1:9020/livez']}
            periodSeconds: 5
      volumes: [{name: token, secret: {secretName: listener, items: [{key: token, path: token}]}}]
EOF
k rollout status deployment/listener --timeout=300s >/dev/null || fail "the fixed listener never became Ready before the hold"
mode_check fixed || fail "the mode check refused the fixed listener before the hold"
note "listener user connections: $(connections service)"
pass

begin hold
set_hold held
pass

begin rolling-update-control
set_listener fresh RollingUpdate
sleep 60
old_ready=$(listener_pods | awk -v h="$(current_hash)" '$2 != h { print $3; exit }')
[ "$old_ready" = True ] || fail "RollingUpdate did not keep the old pod Ready under the hold: $(tr '\n' ';' <<<"$(listener_pods)")"
note "RollingUpdate keeps the old pod Ready: $(tr '\n' ';' <<<"$(listener_pods)")"
if mode_check fresh; then
  fail "the mode check passed with a pod of the previous ReplicaSet left"
fi
pass

begin recreate-fresh
set_listener fresh Recreate
only_current_pods
start_line fresh
mode_check fresh || fail "the mode check refused the fresh listener Recreate left alone"
pass

begin release
set_hold released
pass

begin rollback-hold
set_hold held
pass

begin rollback-control
set_listener fixed RollingUpdate
sleep 60
kept=$(listener_pods | awk -v h="$(current_hash)" '$2 != h { print $1; exit }')
[ -n "$kept" ] || fail "RollingUpdate did not keep the fresh pod under the rollback hold: $(tr '\n' ';' <<<"$(listener_pods)")"
[ "$(pod_mode "$kept")" = fresh ] || fail "the pod RollingUpdate kept, $kept, does not read fresh"
if mode_check fixed; then
  fail "the mode check passed with the fresh pod left"
fi
pass

begin recreate-fixed
set_listener fixed Recreate
only_current_pods
start_line fixed
mode_check fixed || fail "the mode check refused the fixed listener the rollback's Recreate left alone"
pass

begin rollback-release
set_hold released
# A listener the hold refused may be in its crash back-off, up to 5 minutes, when the release lands.
k rollout status deployment/listener --timeout=420s >/dev/null || fail "the fixed listener never became Ready after the release"
pass

begin rolling-update-restored
set_listener fixed RollingUpdate
old=$(listener_pods | awk '{ print $1; exit }')
k patch deployment listener -p '{"spec":{"template":{"metadata":{"annotations":{"cutover-proof/replacement":"after-restore"}}}}}' >/dev/null
deadline=$((SECONDS + 300))
while :; do
  hash=$(current_hash)
  pods=$(listener_pods)
  new_ready=$(awk -v h="$hash" '$2 == h && $3 == "True" { print $1; exit }' <<<"$pods")
  [ -z "$new_ready" ] || break
  awk -v o="$old" '$1 == o && $3 == "True" { found = 1 } END { exit !found }' <<<"$pods" ||
    fail "the old pod $old was not Ready while the new one was not: $(tr '\n' ';' <<<"$pods")"
  [ $SECONDS -lt $deadline ] || fail "the replacement never became Ready: $(tr '\n' ';' <<<"$pods")"
  sleep 1
done
note "the old pod $old stayed Ready until $new_ready was Ready"
k rollout status deployment/listener --timeout=120s >/dev/null
mode_check fixed || fail "the mode check refused the listener after the restored RollingUpdate"
pass
