#!/usr/bin/env bash
# packages/envoy/scripts/cutover-read.test.sh
#
# Verdict test for cutover-read.sh behind a fake kubectl that answers from a case directory and
# records every call: the Job it makes runs `legion cutover-read` with the listener's identity and
# the image named, its verdict is the command's own, and every way the Job can fail to give one is
# UNKNOWN. The reads themselves are held by packages/daemon/internal/intake's
# TestTheDrainReadRefusesAnUnreadSharedWake and TestTheFilterReadRefusesAConsumerLeftOnItsOldFilter.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$here/cutover-read.sh"
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
passed=0
failed=0
readonly image="ghcr.io/cybermonklabs/legion/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

fail() {
  echo "FAIL: $case_name: $*" >&2
  failed=$((failed + 1))
  case_failed=1
}

mkdir -p "$root/bin"
cat >"$root/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >>"$FAKE/calls"
args=()
while [ "$#" -gt 0 ]; do
  case "$1" in --context) shift 2 ;; --request-timeout=*) shift ;; *) args+=("$1"); shift ;; esac
done
set -- "${args[@]}"
case "$*" in
  "-n envoy get deployment envoy-listener -o json") cat "$FAKE/deployment.json" ;;
  "-n envoy create -f "*" -o name") cp "$5" "$FAKE/created-job.json"; echo "job.batch/$(jq -r .metadata.name "$5")" ;;
  "-n envoy get job "*" -o json") cat "$FAKE/job-status.json" ;;
  "-n envoy logs job/"*) [ -f "$FAKE/job.log" ] || { echo "error: no log" >&2; exit 1; }; cat "$FAKE/job.log" ;;
  "-n envoy delete job "*) : ;;
  "-n envoy get job "*" --ignore-not-found -o name") [ ! -e "$FAKE/job.stays" ] || echo "job.batch/$5" ;;
  "-n envoy get pods -l job-name="*" -o name") : ;;
  *) echo "fake kubectl: no answer for: $*" >&2; exit 99 ;;
esac
EOF
chmod +x "$root/bin/kubectl"

base="$root/base"
mkdir -p "$base"
jq -n '{spec: {template: {spec: {
  serviceAccountName: "envoy-listener",
  containers: [{name: "envoy-listener", image: "ghcr.io/cybermonklabs/legion/envoy@sha256:1111",
    env: [{name: "NATS_URLS", value: "nats://envoy-hub.envoy.svc.cluster.local:4222"},
          {name: "NATS_NKEY_SEED_FILE", value: "/var/run/secrets/nats-seed/nats-seed"}],
    volumeMounts: [{name: "nats-seed", mountPath: "/var/run/secrets/nats-seed", readOnly: true}]}],
  volumes: [{name: "nats-seed", csi: {driver: "secrets-store-gke.csi.k8s.io", readOnly: true,
    volumeAttributes: {secretProviderClass: "envoy-listener-nats-seed"}}}]}}}}' >"$base/deployment.json"
echo '{"status":{"succeeded":1}}' >"$base/job-status.json"
printf 'DRAINED INFRA: 3 listener_ready past legion-go-INFRA-wake'"'"'s ack floor 40, to the stream'"'"'s last sequence 99\nGATE OPEN\n' >"$base/job.log"

new_case() {
  case_name=$1
  case_failed=0
  FAKE="$root/case-$((passed + failed))-$RANDOM"
  cp -r "$base" "$FAKE"
  : >"$FAKE/calls"
  export FAKE
}
run() {
  set +e
  output=$(PATH="$root/bin:$PATH" bash "$script" "$@" 2>&1)
  status=$?
  set -e
}
read_drain() { run --context example-cluster --image "$image" drain --projects INFRA,BET; }
expect_status() { [ "$status" = "$1" ] || fail "exit status $status, want $1; output:"$'\n'"$output"; }
expect_line() { grep -qF -- "$1" <<<"$output" || fail "no line holds '$1'; output:"$'\n'"$output"; }
expect_last() { [ "$(tail -n 1 <<<"$output")" = "$1" ] || fail "the last line is '$(tail -n 1 <<<"$output")', want '$1'"; }
done_case() {
  if [ "$case_failed" = 0 ]; then
    passed=$((passed + 1))
    echo "ok: $case_name"
  fi
}

new_case "a drain read that opens passes its verdict through, from a Job with the listener's identity"
read_drain
expect_status 0
expect_last "GATE OPEN"
expect_line "DRAINED INFRA"
jq -e --arg image "$image" '.metadata.namespace == "envoy" and .metadata.labels["created-by"] == "cutover-read"
  and .spec.backoffLimit == 0 and .spec.template.spec.serviceAccountName == "envoy-listener"
  and .spec.template.spec.securityContext.runAsUser == 65534
  and .spec.template.spec.containers[0].image == $image
  and .spec.template.spec.containers[0].command == ["legion", "cutover-read", "drain", "--projects", "INFRA,BET"]
  and .spec.template.spec.containers[0].env == [{name: "NATS_URLS", value: "nats://envoy-hub.envoy.svc.cluster.local:4222"}, {name: "NATS_NKEY_SEED_FILE", value: "/var/run/secrets/nats-seed/nats-seed"}]
  and .spec.template.spec.containers[0].securityContext.capabilities.drop == ["ALL"]
  and .spec.template.spec.volumes[0].csi.volumeAttributes.secretProviderClass == "envoy-listener-nats-seed"' "$FAKE/created-job.json" >/dev/null ||
  fail "the Job is not the listener-identity read Job: $(jq -c . "$FAKE/created-job.json")"
expect_line "JOB: deleted envoy/cutover-read-drain-"
done_case

new_case "a filter read that shuts exits 1 with the command's lines"
echo '{"status":{"failed":1}}' >"$FAKE/job-status.json"
printf 'GATE SHUT INFRA: legion-go-INFRA-dispatch on notifications.dispatch.issue.>, want notifications.dispatch.issue.INFRA.>\nGATE SHUT\n' >"$FAKE/job.log"
run --context example-cluster --image "$image" filters --projects INFRA
expect_status 1
expect_line "GATE SHUT INFRA: legion-go-INFRA-dispatch"
expect_last "GATE SHUT"
[ "$(jq -c '.spec.template.spec.containers[0].command' "$FAKE/created-job.json")" = '["legion","cutover-read","filters","--projects","INFRA"]' ] || fail "the Job does not run the filter read"
done_case

new_case "a read that is UNKNOWN exits 3"
echo '{"status":{"failed":1}}' >"$FAKE/job-status.json"
printf 'UNKNOWN INFRA: the stream holds no wake durable legion-go-INFRA-wake\nUNKNOWN\n' >"$FAKE/job.log"
read_drain
expect_status 3
expect_last "UNKNOWN"
done_case

new_case "a Job that fails with no verdict is UNKNOWN"
echo '{"status":{"failed":1}}' >"$FAKE/job-status.json"
printf 'exec: "legion": executable file not found in $PATH\n' >"$FAKE/job.log"
read_drain
expect_status 3
expect_line "UNKNOWN: the Job failed and its log ends with"
done_case

new_case "a Job that succeeds while its log says GATE SHUT is UNKNOWN"
printf 'GATE SHUT\n' >"$FAKE/job.log"
read_drain
expect_status 3
done_case

new_case "a Job whose log cannot be read is UNKNOWN"
rm "$FAKE/job.log"
read_drain
expect_status 3
expect_line "left no log that could be read"
done_case

new_case "a Job not shown deleted is UNKNOWN"
touch "$FAKE/job.stays"
read_drain
expect_status 3
expect_line "was not shown deleted; delete it"
done_case

new_case "a listener Deployment with no seed file creates no Job"
jq '.spec.template.spec.containers[0].env |= map(select(.name != "NATS_NKEY_SEED_FILE"))' "$base/deployment.json" >"$FAKE/deployment.json"
read_drain
expect_status 3
expect_line "lacks a NATS seed file"
! grep -qF -- "create -f" "$FAKE/calls" || fail "a Job was created"
done_case

for bad in "--image ghcr.io/cybermonklabs/legion/worker:latest drain --projects INFRA" "--image $image --projects INFRA" "--image $image drain --projects infra"; do
  new_case "a usage error reads nothing: ${bad}"
  # shellcheck disable=SC2086
  run --context example-cluster $bad
  expect_status 2
  [ ! -s "$FAKE/calls" ] || fail "the script called $(head -n 1 "$FAKE/calls")"
  done_case
done

echo "cutover-read.test.sh: $passed passed, $failed failed"
[ "$failed" = 0 ]
