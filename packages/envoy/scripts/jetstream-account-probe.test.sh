#!/usr/bin/env bash
# packages/envoy/scripts/jetstream-account-probe.test.sh
#
# Verdict test for jetstream-account-probe.sh, behind a fake kubectl that answers from a case
# directory and runs the Job it is handed here: the Job's own command, the real
# jetstream-account-move built from this tree, against a real nats-server running a layout of the
# move (cmd/jetstream-account-move/testhub: core's render as its goldens pin it, with a stand-in
# auth service), with the paths the Job mounts mapped into the case directory. So each verdict is
# the probe's own reading of that layout, and the script's of the probe's log.
#
# It asserts that: (a) the private render with JetStream in an account of its own, under a
# listener's identity, reads GATE OPEN, exit 0, from a Job built as the script promises (the
# listener's ServiceAccount, its token mount, no API token, backoffLimit 0) and deleted after;
# (b) the merge base's layout, every user in $G, reads GATE SHUT, exit 1, and with the remover's
# Secret removes each sequence it stored (REMOVED); (c) without the remover it names each
# sequence it left (LEFT); (d) an identity denied the JetStream API (a session's grant) reads
# UNKNOWN, exit 3, though nothing is stored; (e) a layout missing ENVOY's $JS.API.> import reads
# UNKNOWN, exit 3, its ordinary request answered "JetStream not enabled for account"; (f) a Job
# that cannot be created reads UNKNOWN; and (g) a Job not shown deleted reads UNKNOWN, naming the
# command that deletes it. Neither (d) nor (e) may print GATE OPEN.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
envoy=$(cd "$here/.." && pwd)
probe="$here/jetstream-account-probe.sh"
root=$(mktemp -d "${TMPDIR:-/tmp}/jetstream-account-probe-test.XXXXXX")
hub_pid=""
cleanup() {
  if [ -n "$hub_pid" ] && kill -0 "$hub_pid" 2>/dev/null; then
    kill "$hub_pid" 2>/dev/null || true
    wait "$hub_pid" 2>/dev/null || true
  fi
  rm -rf "$root"
}
trap cleanup EXIT
failed=0
passed=0

mkdir -p "$root/bin"
(cd "$envoy" && go build -o "$root/bin/jetstream-account-move" ./cmd/jetstream-account-move &&
  go build -o "$root/bin/testhub" ./cmd/jetstream-account-move/testhub)

# --- the fake kubectl -----------------------------------------------------------------------------
cat >"$root/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
# A fake kubectl that answers from the case directory $FAKE, records each call in $FAKE/calls, and
# runs a created Job's command here, each path under /var/run/ mapped into $FAKE/fs, the remover
# Secret's seed copied from $FAKE/secrets/<name>.
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
  "-n envoy get deployment envoy-listener -o json") cat "$FAKE/deployment.json" ;;
  "-n envoy create -f "*" -o name")
    [ ! -e "$FAKE/create.fail" ] || { echo "Error from server (Forbidden): jobs.batch is forbidden" >&2; exit 1; }
    cp "$5" "$FAKE/created-job.json"
    secret=$(jq -r '.spec.template.spec.volumes[] | select(.name == "remover") | .secret.secretName' "$5")
    if [ -n "$secret" ]; then
      mkdir -p "$FAKE/fs/var/run/jetstream-account-probe/remover"
      cp "$FAKE/secrets/$secret" "$FAKE/fs/var/run/jetstream-account-probe/remover/seed"
    fi
    command=()
    while IFS= read -r arg; do
      case "$arg" in
        jetstream-account-move) arg="$FAKE/bin/jetstream-account-move" ;;
        /var/run/*) arg="$FAKE/fs$arg" ;;
      esac
      command+=("$arg")
    done < <(jq -r '.spec.template.spec.containers[0].command[]' "$5")
    [ "${#command[@]}" -gt 0 ] || { echo "fake kubectl: the Job names no command" >&2; exit 1; }
    if "${command[@]}" >"$FAKE/job.log" 2>&1; then
      echo '{"status": {"succeeded": 1}}' >"$FAKE/job-status.json"
    else
      echo '{"status": {"failed": 1}}' >"$FAKE/job-status.json"
    fi
    echo "job.batch/$(jq -r .metadata.name "$5")" ;;
  "-n envoy get job "*" -o json") cat "$FAKE/job-status.json" ;;
  "-n envoy logs job/"*) cat "$FAKE/job.log" ;;
  "-n envoy delete job "*) [ -e "$FAKE/delete.fail" ] || : >"$FAKE/deleted" ;;
  "-n envoy get job "*" --ignore-not-found -o name") [ -e "$FAKE/deleted" ] || echo "job.batch/$5" ;;
  *) echo "fake kubectl: unexpected call: $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$root/bin/kubectl"

# start_hub runs the test hub on layout into a fresh case directory, $FAKE, and waits for it.
start_hub() {
  if [ -n "$hub_pid" ] && kill -0 "$hub_pid" 2>/dev/null; then
    kill "$hub_pid" 2>/dev/null || true
    wait "$hub_pid" 2>/dev/null || true
  fi
  FAKE=$(mktemp -d "$root/case.XXXXXX")
  export FAKE
  mkdir -p "$FAKE/hub" "$FAKE/fs/var/run/secrets/envoy-nats" "$FAKE/secrets"
  ln -s "$root/bin" "$FAKE/bin"
  "$root/bin/testhub" --testdata "$envoy/infra/core/testdata" --layout "$1" --dir "$FAKE/hub" >"$FAKE/hub.log" 2>&1 &
  hub_pid=$!
  for _ in $(seq 1 300); do
    [ -e "$FAKE/hub/ready" ] && break
    kill -0 "$hub_pid" 2>/dev/null || break
    sleep 0.1
  done
  [ -e "$FAKE/hub/ready" ] || { cat "$FAKE/hub.log" >&2; echo "the test hub on $1 never came up" >&2; exit 1; }
  cp "$FAKE/hub/remover-seed" "$FAKE/secrets/jetstream-operator"
  jq -n --arg urls "$(cat "$FAKE/hub/url")" '{spec: {template: {spec: {
    serviceAccountName: "envoy-listener",
    containers: [{name: "listener", image: "ghcr.io/example/legion/envoy@sha256:0123",
      env: [{name: "NATS_URLS", value: $urls}],
      volumeMounts: [{name: "envoy-nats", mountPath: "/var/run/secrets/envoy-nats"}]}],
    volumes: [{name: "envoy-nats", projected: {sources: [{serviceAccountToken: {audience: "envoy-nats", path: "token"}}]}}]}}}}' \
    >"$FAKE/deployment.json"
}

# as presents token as the listener Deployment's projected NATS token.
as() { cp "$FAKE/hub/$1-token" "$FAKE/fs/var/run/secrets/envoy-nats/token"; }

output=""
status=0
run_probe() {
  status=0
  output=$(PATH="$root/bin:$PATH" bash "$probe" --context example-cluster --token-file /var/run/secrets/envoy-nats/token "$@" 2>&1) || status=$?
}
last() { printf '%s\n' "$output" | sed -n '$p'; }
case_name=""
fail() {
  echo "FAIL: $case_name: $*" >&2
  printf 'the probe printed:\n%s\n' "$output" >&2
  failed=$((failed + 1))
}
pass() {
  echo "PASS: $case_name"
  passed=$((passed + 1))
}

# (a) The private render: GATE OPEN.
case_name="the private render reads GATE OPEN under the listener's identity"
start_hub accounts
as listener
run_probe --remover-seed-secret jetstream-operator
job=$FAKE/created-job.json
if [ "$status" -ne 0 ] || [ "$(last)" != "GATE OPEN" ]; then
  fail "exit $status, last line '$(last)'"
elif ! grep -q '^NOTHING STORED on notifications.dispatch.issue.ZZPROBE.1' <<<"$output" ||
  ! grep -q '^ANSWERED \$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS' <<<"$output" ||
  ! grep -q '^REFUSED the control publish to \$KV.envoy_session_consumers.probe' <<<"$output"; then
  fail "the answered, refused and nothing-stored lines are not all there"
elif ! grep -q '^JOB: deleted envoy/jetstream-account-probe-' <<<"$output"; then
  fail "the Job was not shown deleted"
elif [ "$(jq -r '.spec.template.spec | [.serviceAccountName, .automountServiceAccountToken, .containers[0].image] | @tsv' "$job")" != $'envoy-listener\tfalse\tghcr.io/example/legion/envoy@sha256:0123' ] ||
  [ "$(jq -r .spec.backoffLimit "$job")" != 0 ] ||
  [ "$(jq -c '.spec.template.spec.containers[0].command[4:6]' "$job")" != '["--token-file","/var/run/secrets/envoy-nats/token"]' ]; then
  fail "the Job is not the listener's identity, its own image, no API token and no retry: $(jq -c .spec "$job")"
else
  pass
fi

# (b) The merge base's layout: GATE SHUT, and the remover removes what it stored.
case_name="the merge base's layout reads GATE SHUT and the remover removes what it stored"
start_hub merge-base
as listener
run_probe --remover-seed-secret jetstream-operator
if [ "$status" -ne 1 ] || [ "$(last)" != "GATE SHUT" ]; then
  fail "exit $status, last line '$(last)'"
elif ! grep -q '^STORED 1 on notifications.dispatch.issue.ZZPROBE.1' <<<"$output" ||
  ! grep -Eq '^REMOVED notifications.dispatch.issue.ZZPROBE.1 seq=[0-9]+ from ENVOY_NOTIFICATIONS$' <<<"$output"; then
  fail "no STORED and REMOVED lines"
else
  pass
fi

# (c) Without a remover: GATE SHUT, naming the sequence it left.
case_name="without a remover the merge base's layout reads GATE SHUT naming what it left"
start_hub merge-base
as listener
run_probe
if [ "$status" -ne 1 ] || [ "$(last)" != "GATE SHUT" ]; then
  fail "exit $status, last line '$(last)'"
elif ! grep -Eq '^LEFT notifications.dispatch.issue.ZZPROBE.1 seq=[0-9]+ in ENVOY_NOTIFICATIONS' <<<"$output"; then
  fail "no LEFT line"
else
  pass
fi

# (d) An identity denied the JetStream API: UNKNOWN, never GATE OPEN.
case_name="an identity denied the JetStream API reads UNKNOWN"
start_hub accounts
as session
run_probe --remover-seed-secret jetstream-operator
if [ "$status" -ne 3 ] || [ "$(last)" != "UNKNOWN" ] || grep -q '^GATE OPEN$' <<<"$output"; then
  fail "exit $status, last line '$(last)'"
elif ! grep -q '^UNKNOWN the ordinary request \$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS was not answered' <<<"$output"; then
  fail "no UNKNOWN line naming the ordinary request"
else
  pass
fi

# (e) A layout missing ENVOY's $JS.API.> import: UNKNOWN, never GATE OPEN.
case_name="a layout missing the \$JS.API.> import reads UNKNOWN"
start_hub accounts-without-api-import
as listener
run_probe --remover-seed-secret jetstream-operator
if [ "$status" -ne 3 ] || [ "$(last)" != "UNKNOWN" ] || grep -q '^GATE OPEN$' <<<"$output"; then
  fail "exit $status, last line '$(last)'"
elif ! grep -q 'JetStream not enabled for account' <<<"$output"; then
  fail "the ordinary request's answer is not named"
else
  pass
fi

# (f) A Job that cannot be created.
case_name="a Job that cannot be created reads UNKNOWN"
start_hub accounts
as listener
: >"$FAKE/create.fail"
run_probe
if [ "$status" -ne 3 ] || [ "$(last)" != "UNKNOWN" ] || ! grep -q '^UNKNOWN the Job could not be created' <<<"$output"; then
  fail "exit $status, last line '$(last)'"
else
  pass
fi

# (g) A Job not shown deleted.
case_name="a Job not shown deleted reads UNKNOWN naming the delete"
start_hub accounts
as listener
: >"$FAKE/delete.fail"
run_probe
if [ "$status" -ne 3 ] || [ "$(last)" != "UNKNOWN" ] ||
  ! grep -q '^UNKNOWN the Job envoy/jetstream-account-probe-[0-9a-f]* was not shown deleted; delete it: kubectl --context example-cluster -n envoy delete job ' <<<"$output"; then
  fail "exit $status, last line '$(last)'"
else
  pass
fi

echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
