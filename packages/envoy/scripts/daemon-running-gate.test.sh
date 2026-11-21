#!/usr/bin/env bash
# packages/envoy/scripts/daemon-running-gate.test.sh
#
# Verdict test for daemon-running-gate.sh behind fakes of gh (the deployment repository's files),
# kubectl (Deployments, ReplicaSets, pods and `legion version` in a pod), crane (image configs) and
# git (which revisions the clone holds and which are at or after the commit), each answering from a
# case directory and recording its calls. Every case changes one thing of the clean fixture, four
# running daemons and SMOKE at `replicas: 0`, so each fails for its own reason.
#
# Control: a copy of the gate that reads the Deployment's pod template image, the pin, in place of
# the pods' imageIDs. With one pod still on an older digest while the pin names the new one, the
# copy opens where the gate shuts; the test fails naming the control if the copy does not open.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
gate="$here/daemon-running-gate.sh"
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
passed=0
failed=0

fail() {
  echo "FAIL: $case_name: $*" >&2
  failed=$((failed + 1))
  case_failed=1
}

readonly commit=1111111111111111111111111111111111111111
readonly new_rev=2222222222222222222222222222222222222222
readonly old_rev=3333333333333333333333333333333333333333
readonly new_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
readonly old_digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
readonly repo=ghcr.io/cybermonklabs/legion/worker

mkdir -p "$root/bin"
cat >"$root/bin/gh" <<'EOF'
#!/usr/bin/env bash
# A fake gh: `gh api repos/<dep>/contents/<path> --jq …` answers from $FAKE/repo/<path>.
set -u
printf 'gh %s\n' "$*" >>"$FAKE/calls"
path=${2#repos/*/*/contents/}
if [ "$path" = legion/projects ]; then
  for file in "$FAKE/repo/legion/projects/"*; do printf 'file\turl\t%s\n' "$(basename "$file")"; done
  exit 0
fi
[ -f "$FAKE/repo/$path" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
printf 'base64\n%s\n' "$(base64 -w 0 <"$FAKE/repo/$path")"
EOF
cat >"$root/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
# A fake kubectl answering from $FAKE.
set -u
printf 'kubectl %s\n' "$*" >>"$FAKE/calls"
args=()
while [ "$#" -gt 0 ]; do
  case "$1" in --context) shift 2 ;; --request-timeout=*) shift ;; *) args+=("$1"); shift ;; esac
done
set -- "${args[@]}"
ns=$2
case "$3 $4" in
  "get deployment") file="$FAKE/deploy-$ns-$5.json" ;;
  "get replicasets") file="$FAKE/rs-$ns.json" ;;
  "get pods") file="$FAKE/pods-$ns-${6#app=}.json" ;;
  "exec "*)
    pod=$4
    [ -f "$FAKE/version-$pod" ] || { echo "error: pods \"$pod\" is forbidden: cannot exec" >&2; exit 1; }
    cat "$FAKE/version-$pod"; exit 0 ;;
  *) echo "fake kubectl: no answer for $*" >&2; exit 99 ;;
esac
[ -f "$file" ] || { echo "Error from server (Forbidden): cannot read $3 $4" >&2; exit 1; }
cat "$file"
EOF
cat >"$root/bin/crane" <<'EOF'
#!/usr/bin/env bash
# A fake crane: `crane config <repo>@<digest>` answers from $FAKE/config-<digest without sha256:>.
set -u
printf 'crane %s\n' "$*" >>"$FAKE/calls"
digest=${2#*@sha256:}
[ -f "$FAKE/config-$digest" ] || { echo "Error: MANIFEST_UNKNOWN" >&2; exit 1; }
cat "$FAKE/config-$digest"
EOF
cat >"$root/bin/git" <<'EOF'
#!/usr/bin/env bash
# A fake git for `git -C <dir> cat-file -e <rev>^{commit}` and `merge-base --is-ancestor <a> <b>`.
set -u
printf 'git %s\n' "$*" >>"$FAKE/calls"
[ "$1" = -C ] && shift 2
case "$1" in
  cat-file) rev=${3%^\{commit\}}; ! grep -qxF "$rev" "$FAKE/git-missing" ;;
  merge-base) grep -qxF "$3 $4" "$FAKE/git-ancestors" ;;
  *) echo "fake git: no answer for $*" >&2; exit 99 ;;
esac
EOF
chmod +x "$root/bin/"*

# daemon_fixture KEY NAMESPACE DIGEST: one running daemon's Deployment, its current ReplicaSet and
# its pod on DIGEST, in $base.
daemon_fixture() {
  local key=$1 ns=$2 digest=$3 lower name
  lower=$(tr 'A-Z' 'a-z' <<<"$key")
  name="legion-${lower}-daemon"
  jq -n --arg name "$name" --arg ns "$ns" --arg pin "$repo@$new_digest" --arg uid "uid-$lower" '
    {metadata: {name: $name, namespace: $ns, uid: $uid, generation: 4, annotations: {"deployment.kubernetes.io/revision": "7"}},
     spec: {replicas: 1, selector: {matchLabels: {app: $name}}, template: {spec: {containers: [{name: "legion", image: $pin}]}}},
     status: {observedGeneration: 4, updatedReplicas: 1, readyReplicas: 1, replicas: 1}}' >"$base/deploy-$ns-$name.json"
  jq -n --arg name "$name" --arg uid "uid-$lower" '{metadata: {name: "\($name)-7f", annotations: {"deployment.kubernetes.io/revision": "7"},
     ownerReferences: [{uid: $uid}]}, status: {replicas: 1}}' >>"$base/rs-$ns.items"
  jq -n --arg name "$name" --arg image "$repo@$digest" '{items: [{metadata: {name: "\($name)-pod"}, status: {phase: "Running",
     containerStatuses: [{name: "legion", imageID: $image}]}}]}' >"$base/pods-$ns-$name.json"
  local revision=$new_rev
  [ "$digest" = "$new_digest" ] || revision=$old_rev
  printf 'legion v1.2.3 commit %s\n' "$revision" >"$base/version-$name-pod"
}

base="$root/base"
mkdir -p "$base/repo/legion/projects" "$base/repo/shared/legion/privateprojects"
printf 'name: legion\n' >"$base/repo/legion/Pulumi.yaml"
printf 'config: {}\n' >"$base/repo/legion/Pulumi.prod.yaml"
for entry in 10:LINUX 20:BET 30:INFRA 40:OSSV; do
  printf 'key: %s\nimage: %s@%s\n' "${entry#*:}" "$repo" "$new_digest" >"$base/repo/legion/projects/${entry%%:*}-${entry#*:}.yaml"
done
printf 'key: SMOKE\nreplicas: 0\n' >"$base/repo/legion/projects/90-SMOKE.yaml"
printf 'package privateprojects\n\nvar projects = []Project{}\n' >"$base/repo/shared/legion/privateprojects/lists.go"
for key in LINUX BET INFRA OSSV; do daemon_fixture "$key" legion "$new_digest"; done
jq -s '{items: .}' "$base/rs-legion.items" >"$base/rs-legion.json"
for digest in "$new_digest" "$old_digest"; do
  revision=$new_rev
  [ "$digest" = "$new_digest" ] || revision=$old_rev
  jq -n --arg rev "$revision" '{config: {Labels: {"org.opencontainers.image.source": "https://github.com/cybermonklabs/legion", "org.opencontainers.image.revision": $rev}}}' \
    >"$base/config-${digest#sha256:}"
done
: >"$base/git-missing"
printf '%s %s\n%s %s\n' "$commit" "$new_rev" "$commit" "$commit" >"$base/git-ancestors"

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
  output=$(PATH="$root/bin:$PATH" bash "${1:-$gate}" --deployment example/deployment "$commit" 2>&1)
  status=$?
  set -e
}

expect_status() { [ "$status" = "$1" ] || fail "exit status $status, want $1; output:"$'\n'"$output"; }
expect_line() { grep -qF -- "$1" <<<"$output" || fail "no line holds '$1'; output:"$'\n'"$output"; }
expect_last() { [ "$(tail -n 1 <<<"$output")" = "$1" ] || fail "the last line is '$(tail -n 1 <<<"$output")', want '$1'"; }
done_case() {
  if [ "$case_failed" = 0 ]; then
    passed=$((passed + 1))
    echo "ok: $case_name"
  fi
}

new_case "four running daemons current and SMOKE at replicas 0 open"
run
expect_status 0
expect_last "GATE OPEN"
expect_line "SKIPPED: SMOKE runs no daemon (replicas: 0)"
for key in LINUX BET INFRA OSSV; do expect_line "CURRENT: ${key} (legion/legion-$(tr 'A-Z' 'a-z' <<<"$key")-daemon)"; done
grep -qF -- "-n legion exec legion-infra-daemon-pod -c legion -- legion version" "$FAKE/calls" || fail "the daemon's own version was not read"
done_case

new_case "one pod on an older digest shuts, naming it"
jq --arg image "$repo@$old_digest" '.items[0].status.containerStatuses[0].imageID = $image' "$base/pods-legion-legion-bet-daemon.json" >"$FAKE/pods-legion-legion-bet-daemon.json"
printf 'legion v1.2.2 commit %s\n' "$old_rev" >"$FAKE/version-legion-bet-daemon-pod"
run
expect_status 1
expect_line "GATE SHUT: BET (legion/legion-bet-daemon): pod legion-bet-daemon-pod container legion runs $repo@$old_digest at revision $old_rev, which is not at or after $commit"
expect_last "GATE SHUT"
done_case

new_case "control: a gate reading the pod template's pin passes the stale pod"
jq --arg image "$repo@$old_digest" '.items[0].status.containerStatuses[0].imageID = $image' "$base/pods-legion-legion-bet-daemon.json" >"$FAKE/pods-legion-legion-bet-daemon.json"
mutant="$root/mutant/daemon-running-gate.sh"
mkdir -p "$root/mutant/lib"
cp "$here/lib/legion-deployment-projects.sh" "$root/mutant/lib/"
awk '/^  jq -r .\.items\[\] \| select\(\.status\.phase == "Running"\)/ {
    print "  jq -r --arg pod \"${name}-pod\" '\''.spec.template.spec.containers[] | [$pod, .name, .image] | @tsv'\'' \"$work/deployment.json\" >\"$work/containers\""
    getline; next }
  { print }' "$gate" >"$mutant"
if cmp -s "$gate" "$mutant"; then
  fail "the control copy is the gate unchanged, so it shows nothing"
fi
run "$mutant"
[ "$status" = 0 ] || fail "the control, reading the pin, did not pass the stale pod (exit $status), so the test shows nothing:"$'\n'"$output"
done_case

new_case "an updated count below the replicas shuts"
jq '.status.updatedReplicas = 0' "$base/deploy-legion-legion-ossv-daemon.json" >"$FAKE/deploy-legion-legion-ossv-daemon.json"
run
expect_status 1
expect_line "GATE SHUT: OSSV (legion/legion-ossv-daemon): the rollout is not complete: status.updatedReplicas 0, spec.replicas 1"
done_case

new_case "an old ReplicaSet still holding a pod shuts"
jq '.items += [{metadata: {name: "legion-linux-daemon-6e", annotations: {"deployment.kubernetes.io/revision": "6"}, ownerReferences: [{uid: "uid-linux"}]}, status: {replicas: 1}}]' "$base/rs-legion.json" >"$FAKE/rs-legion.json"
run
expect_status 1
expect_line "GATE SHUT: LINUX (legion/legion-linux-daemon): an old ReplicaSet still holds a pod: legion-linux-daemon-6e (revision 6) holds 1 pod(s)"
done_case

new_case "legion version naming another commit than the image label shuts"
printf 'legion v1.2.3 commit %s\n' "$old_rev" >"$FAKE/version-legion-infra-daemon-pod"
run
expect_status 1
expect_line "GATE SHUT: INFRA (legion/legion-infra-daemon): legion version in pod legion-infra-daemon-pod names commit $old_rev, not its image's revision $new_rev"
done_case

for failure in "deploy:the Deployment" "pods:the pods" "crane:the image config" "git:which this clone does not hold" "exec:was refused or failed" "version:names no commit" "rs:the ReplicaSets"; do
  new_case "an unreadable read is UNKNOWN: ${failure#*:}"
  case "${failure%%:*}" in
    deploy) rm "$FAKE/deploy-legion-legion-bet-daemon.json" ;;
    pods) rm "$FAKE/pods-legion-legion-bet-daemon.json" ;;
    crane) rm "$FAKE/config-${new_digest#sha256:}" ;;
    git) echo "$new_rev" >"$FAKE/git-missing" ;;
    exec) rm "$FAKE/version-legion-bet-daemon-pod" ;;
    version) echo "legion (devel)" >"$FAKE/version-legion-bet-daemon-pod" ;;
    rs) rm "$FAKE/rs-legion.json" ;;
  esac
  run
  expect_status 3
  expect_line "${failure#*:}"
  expect_last "UNKNOWN"
  done_case
done

new_case "an unreadable private-project list is UNKNOWN"
rm "$FAKE/repo/shared/legion/privateprojects/lists.go"
run
expect_status 3
expect_line "UNKNOWN: cannot read example/deployment shared/legion/privateprojects/lists.go"
done_case

new_case "a private project's daemon is read in its own namespace"
printf 'package privateprojects\n\nvar projects = []Project{\n\t{Key: "BET", Person: "someone"},\n}\n' >"$FAKE/repo/shared/legion/privateprojects/lists.go"
mv "$FAKE/deploy-legion-legion-bet-daemon.json" "$FAKE/deploy-legion-bet-legion-bet-daemon.json"
mv "$FAKE/pods-legion-legion-bet-daemon.json" "$FAKE/pods-legion-bet-legion-bet-daemon.json"
jq '{items: [.items[] | select(.metadata.name | startswith("legion-bet"))]}' "$base/rs-legion.json" >"$FAKE/rs-legion-bet.json"
run
expect_status 0
expect_line "CURRENT: BET (legion-bet/legion-bet-daemon)"
done_case

new_case "a usage error reads nothing"
set +e
output=$(PATH="$root/bin:$PATH" bash "$gate" "$commit" 2>&1)
status=$?
set -e
expect_status 2
[ ! -s "$FAKE/calls" ] || fail "the gate called $(head -n 1 "$FAKE/calls")"
done_case

echo "daemon-running-gate.test.sh: $passed passed, $failed failed"
[ "$failed" = 0 ]
