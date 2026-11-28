#!/usr/bin/env bash
# packages/envoy/scripts/legacy-subjects-purge.test.sh
#
# Verdict test for legacy-subjects-purge.sh, behind a fake kubectl that answers from a case
# directory and records every call. The Job the driver creates is run here: the fake kubectl's log of
# it is the output of the Job's own script (lib/legacy-subjects-purge-nats.sh), run under sh with
# the Job's environment behind a fake nats that keeps the stream's messages in $FAKE/stream (one
# "<subject> <sequence>" line each) and purges from that file what each purge's filter and sequence
# name, as the server does. So each case reads what the stream holds after the driver ran, not what
# the driver says of it.
#
# The cases: the driver purges every old-form subject and leaves every new-form message; it
# refuses, calling nothing, a reading whose (a1), (a2), (a3) or (b) did not read true, one that
# holds no (a3), a first reading, and another cluster's; it purges nothing when an old-form message
# was published after the reading, or when the stream is not the reading's; it keeps an old-form
# message published after the Job's check and before its purge; and it prints UNKNOWN when its
# read-back still finds an old-form message.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
driver="$here/legacy-subjects-purge.sh"
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
passed=0
failed=0

fail() {
  echo "FAIL: $case_name: $*" >&2
  case_failed=1
}

# --- the fakes ------------------------------------------------------------------------------------
mkdir -p "$root/bin" "$root/natsbin"
cat >"$root/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
# A fake kubectl that answers from $FAKE and records each call in $FAKE/calls. The Job's log is the
# output of its own script, run with its environment behind the fake nats.
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
    cp "$5" "$FAKE/created-job.json"
    echo "job.batch/$(jq -r .metadata.name "$5")" ;;
  "-n envoy get job "*" -o json") echo '{"status":{"succeeded":1}}' ;;
  "-n envoy logs job/"*)
    env_args=()
    while IFS= read -r -d '' pair; do env_args+=("$pair"); done < <(jq -j '.spec.template.spec.containers[0].env[] | select(.name != "WORK_DIR") | "\(.name)=\(.value)\u0000"' "$FAKE/created-job.json")
    env -i PATH="$FAKE_NATSBIN:$PATH" FAKE="$FAKE" WORK_DIR="$FAKE" "${env_args[@]}" \
      timeout 20 sh -c "$(jq -r '.spec.template.spec.containers[0].command[2]' "$FAKE/created-job.json")" ;;
  "-n envoy delete job "*) : ;;
  "-n envoy get job "*" --ignore-not-found -o name") : ;;
  "-n envoy get pods -l job-name="*" -o name") : ;;
  *) echo "fake kubectl: no answer for: $*" >&2; exit 99 ;;
esac
EOF
cat >"$root/natsbin/nats" <<'EOF'
#!/usr/bin/env bash
# A fake nats CLI for `nats req --raw <subject> <body> [--inbox-prefix <p>]`, answering from the
# stream in $FAKE/stream and recording each call, with the user it connects as, in $FAKE/nats.calls.
printf '%s nkey=%s\n' "$*" "${NATS_NKEY:-}" >>"$FAKE/nats.calls"
[ "$1 $2" = "req --raw" ] || { echo "fake nats: no answer for $*" >&2; exit 99; }
subject=$3
body=$4
case "$subject" in
  '$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS')
    filter=$(jq -r '.subjects_filter // ""' <<<"$body")
    if [ -z "$filter" ]; then
      jq -cn --arg id "$(cat "$FAKE/stream_id")" '{type: "io.nats.jetstream.api.v1.stream_info_response", config: {name: "ENVOY_NOTIFICATIONS", metadata: {envoy_stream_id: $id}}, state: {}}'
    else
      awk -v p="${filter%>}" 'index($1, p) == 1 {print $1}' "$FAKE/stream" | jq -cRs 'split("\n") | map(select(length > 0)) | group_by(.) | map({key: .[0], value: length}) | from_entries as $s
        | {type: "io.nats.jetstream.api.v1.stream_info_response", total: ($s | length), offset: 0, limit: 100000, state: {subjects: (if ($s | length) == 0 then null else $s end)}}'
    fi ;;
  '$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS')
    want=$(jq -r .last_by_subj <<<"$body")
    seq=$(awk -v s="$want" '$1 == s {print $2}' "$FAKE/stream" | sort -n | tail -n 1)
    if [ -z "$seq" ]; then echo '{"type":"io.nats.jetstream.api.v1.stream_msg_get_response","error":{"code":404,"err_code":10037,"description":"no message found"}}'
    else jq -cn --arg s "$want" --argjson q "$seq" '{type: "io.nats.jetstream.api.v1.stream_msg_get_response", message: {subject: $s, seq: $q, time: "2026-11-20T10:00:00Z", data: "eyJtZXNzYWdlIjoiYSBwcml2YXRlIG1lc3NhZ2UifQ=="}}'
    fi ;;
  '$JS.API.STREAM.PURGE.ENVOY_NOTIFICATIONS')
    # A message published after the Job read the subjects' last messages and before its first
    # purge: the case's $FAKE/late line joins the stream then.
    if [ -e "$FAKE/late" ]; then
      cat "$FAKE/late" >>"$FAKE/stream"
      rm "$FAKE/late"
    fi
    filter=$(jq -r '.filter // ""' <<<"$body")
    # The server's purge with a seq takes only the messages below it; without one, every message.
    keep_from=$(jq -r '.seq // 0' <<<"$body")
    before=$(wc -l <"$FAKE/stream")
    if [ ! -e "$FAKE/purge.keeps" ]; then
      awk -v s="$filter" -v k="$keep_from" '!((s == "" || $1 == s) && (k == 0 || $2 < k))' "$FAKE/stream" >"$FAKE/stream.new"
      mv "$FAKE/stream.new" "$FAKE/stream"
    fi
    jq -cn --argjson n "$((before - $(wc -l <"$FAKE/stream")))" '{type: "io.nats.jetstream.api.v1.stream_purge_response", success: true, purged: $n}' ;;
  *) echo "fake nats: no answer for $*" >&2; exit 99 ;;
esac
EOF
chmod +x "$root/bin/kubectl" "$root/natsbin/nats"
export FAKE_NATSBIN="$root/natsbin"

# --- the clean fixture ----------------------------------------------------------------------------
readonly old_issue=notifications.dispatch.issue.ACME-13.comment.created
readonly old_agent=notifications.agent.ses_old
readonly new_issue=notifications.dispatch.issue.ACME.ACME-12.comment.created
readonly new_agent=notifications.agent.ACME.ses_a

base="$root/base"
mkdir -p "$base"
printf '%s\n' "$old_issue 790" "$old_agent 800" "$old_issue 805" "$new_issue 810" "$new_agent 811" >"$base/stream"
echo stream-id-1 >"$base/stream_id"
jq -n '{spec: {template: {spec: {containers: [{name: "envoy-listener",
  env: [{name: "NATS_URLS", value: "nats://envoy-hub.envoy.svc.cluster.local:4222"}]}]}}}}' >"$base/deployment.json"
# The second reading the purge exists for: (a1), (a2), (a3) and (b) true, (c) false, the newest
# old-form message at stream sequence 805.
jq -n '{gate: "legacy-subjects-gate", version: 3, context: "example-cluster", verdict: "shut",
  compared_with: "2026-11-21T00:00:00Z", stream: {id: "stream-id-1", last_seq: 811},
  facts: {a1: "true", a2: "true", a3: "true", b: "true", c: "false", last_old_seq: 805}}' >"$base/reading.json"

# --- running a case -------------------------------------------------------------------------------
new_case() {
  case_name=$1
  case_failed=0
  FAKE="$root/case-$((passed + failed))-$RANDOM"
  cp -r "$base" "$FAKE"
  : >"$FAKE/calls"
  : >"$FAKE/nats.calls"
  export FAKE
}

# purge [ARG...] runs the driver on the case's reading, keeping its output and exit status.
purge() {
  set +e
  output=$(PATH="$root/bin:$PATH" bash "$driver" --context example-cluster --reading "$FAKE/reading.json" "$@" 2>&1)
  status=$?
  set -e
}

expect_status() {
  [ "$status" = "$1" ] || fail "exit status $status, want $1; output:"$'\n'"$output"
}

expect_line() {
  grep -qF -- "$1" <<<"$output" || fail "no line holds '$1'; output:"$'\n'"$output"
}

expect_last() {
  [ "$(tail -n 1 <<<"$output")" = "$1" ] || fail "the last line is '$(tail -n 1 <<<"$output")', want '$1'"
}

# stream_holds SUBJECT... fails the case unless the stream holds exactly these subjects.
stream_holds() {
  local want got
  want=$(printf '%s\n' "$@" | sort -u)
  got=$(awk '{print $1}' "$FAKE/stream" | sort -u)
  [ "$got" = "$want" ] || fail "the stream holds $(paste -sd' ' <<<"$got"), want $(paste -sd' ' <<<"$want")"
}

done_case() {
  if [ "$case_failed" = 0 ]; then
    passed=$((passed + 1))
    echo "ok: $case_name"
  else
    failed=$((failed + 1))
  fi
}

# --- the cases ------------------------------------------------------------------------------------
new_case "the driver purges every old-form subject and leaves every new-form message"
purge
expect_status 0
expect_last "PURGED"
expect_line "PURGED: $old_issue, 2 message(s)"
expect_line "PURGED: $old_agent, 1 message(s)"
expect_line "PASS: the stream holds no old-form subject after the purge of 2 subject(s)"
stream_holds "$new_issue" "$new_agent"
[ "$(grep -c 'STREAM.PURGE' "$FAKE/nats.calls")" = 2 ] || fail "the Job sent $(grep -c 'STREAM.PURGE' "$FAKE/nats.calls") purges, want 2, one per old-form subject"
! grep 'STREAM.PURGE' "$FAKE/nats.calls" | grep -qv -- '"filter":"notifications\.[a-z.A-Z0-9_-]*"' || fail "a purge named no literal subject: $(grep 'STREAM.PURGE' "$FAKE/nats.calls")"
[ "$(grep -c -v -- '--inbox-prefix _INBOX_legacy_cutover nkey=/var/run/secrets/legacy-cutover/nats-seed$' "$FAKE/nats.calls")" = 0 ] ||
  fail "a nats call took another prefix or credential than the legacy-cutover user's: $(grep -v -- '--inbox-prefix _INBOX_legacy_cutover' "$FAKE/nats.calls" | head -n 1)"
jq -e '.metadata.namespace == "envoy" and .metadata.labels["created-by"] == "legacy-subjects-purge"
  and .spec.backoffLimit == 0 and .spec.ttlSecondsAfterFinished == 600 and .spec.activeDeadlineSeconds == 300
  and .spec.template.spec.serviceAccountName == "legacy-cutover" and .spec.template.spec.automountServiceAccountToken == false
  and .spec.template.spec.securityContext.runAsUser == 65534
  and .spec.template.spec.containers[0].securityContext.capabilities.drop == ["ALL"]
  and .spec.template.spec.volumes[0] == {name: "legacy-cutover-seed", csi: {driver: "secrets-store-gke.csi.k8s.io", readOnly: true, volumeAttributes: {secretProviderClass: "legacy-cutover-nats-seed"}}}
  and ([.spec.template.spec.containers[0].env[] | select(.name == "STREAM_ID" or .name == "LAST_OLD_SEQ") | .value] == ["stream-id-1", "805"])' \
  "$FAKE/created-job.json" >/dev/null || fail "the Job is not the purge Job the driver declares: $(jq -c .spec "$FAKE/created-job.json")"
name=$(jq -r .metadata.name "$FAKE/created-job.json")
expect_line "JOB: deleted envoy/$name, and envoy holds no Job or pod of that name"
grep -qF -- "-n envoy get pods -l job-name=$name -o name" "$FAKE/calls" || fail "the Job's pods were not read back"
! grep -q 'eyJtZXNzYWdl' <<<"$output" || fail "the driver printed a message's content"
done_case

for fact in a1 a2 a3 b; do
  for verdict in false unknown; do
    new_case "the driver refuses, calling nothing, a reading whose ($fact) read $verdict"
    jq --arg f "$fact" --arg v "$verdict" '.facts[$f] = $v' "$base/reading.json" >"$FAKE/reading.json"
    purge
    expect_status 1
    expect_line "REFUSED: the reading $FAKE/reading.json read ($fact)"
    expect_line "$verdict, not true"
    expect_last "REFUSED"
    [ ! -s "$FAKE/calls" ] || fail "the refused driver called $(head -n 1 "$FAKE/calls")"
    stream_holds "$old_issue" "$old_agent" "$new_issue" "$new_agent"
    done_case
  done
done

# A reading recorded before the gate read (a3), or one that lost it, reads no publisher: refused.
new_case "the driver refuses, calling nothing, a reading that holds no (a3)"
jq 'del(.facts.a3)' "$base/reading.json" >"$FAKE/reading.json"
purge
expect_status 1
expect_line "REFUSED: the reading $FAKE/reading.json read (a3) the publishers unread, not true"
expect_last "REFUSED"
[ ! -s "$FAKE/calls" ] || fail "the refused driver called $(head -n 1 "$FAKE/calls")"
done_case

new_case "the driver refuses a first reading, which reads no fact, and another cluster's reading"
jq '.compared_with = null | .facts = null' "$base/reading.json" >"$FAKE/reading.json"
purge
expect_status 1
expect_line "REFUSED: the reading $FAKE/reading.json is a first reading, which reads no fact"
jq '.context = "another-cluster"' "$base/reading.json" >"$FAKE/reading.json"
purge
expect_status 1
expect_line "REFUSED: the reading $FAKE/reading.json read the cluster another-cluster, not example-cluster"
[ ! -s "$FAKE/calls" ] || fail "the refused driver called $(head -n 1 "$FAKE/calls")"
done_case

new_case "the driver purges nothing when an old-form message was published after the reading"
echo "$old_agent 812" >>"$FAKE/stream"
purge
expect_status 1
expect_line "REFUSED: $old_agent holds a message at stream sequence 812, after 805, the newest old-form message the reading found"
! grep -q 'STREAM.PURGE' "$FAKE/nats.calls" || fail "the Job purged: $(grep 'STREAM.PURGE' "$FAKE/nats.calls")"
stream_holds "$old_issue" "$old_agent" "$new_issue" "$new_agent"
done_case

new_case "the driver keeps an old-form message published after the Job's check, and prints UNKNOWN"
echo "$old_agent 812" >"$FAKE/late"
purge
expect_status 3
expect_line "PURGED: $old_issue, 2 message(s)"
expect_line "PURGED: $old_agent, 1 message(s)"
expect_line "UNKNOWN: the stream still holds 1 old-form subject(s) after the purge: $old_agent"
expect_last "UNKNOWN"
[ "$(sort "$FAKE/stream")" = "$(printf '%s\n' "$new_agent 811" "$old_agent 812" "$new_issue 810" | sort)" ] ||
  fail "the stream holds $(paste -sd' ' "$FAKE/stream"), want the new-form messages and $old_agent at 812, the one published after the check"
done_case

new_case "the driver purges nothing on a stream that is not the reading's"
echo stream-id-2 >"$FAKE/stream_id"
purge
expect_status 1
expect_line "REFUSED: the stream's envoy_stream_id is stream-id-2, not stream-id-1, the one the reading read"
! grep -q 'STREAM.PURGE' "$FAKE/nats.calls" || fail "the Job purged: $(grep 'STREAM.PURGE' "$FAKE/nats.calls")"
done_case

new_case "the driver prints UNKNOWN when its read-back finds an old-form message"
touch "$FAKE/purge.keeps"
purge
expect_status 3
expect_line "UNKNOWN: the stream still holds 2 old-form subject(s) after the purge: $old_agent, $old_issue"
expect_last "UNKNOWN"
done_case

new_case "a usage error calls nothing"
set +e
output=$(PATH="$root/bin:$PATH" bash "$driver" --context example-cluster --reading "$FAKE/reading.json" --seed-class envoy-purge-seed 2>&1)
status=$?
set -e
expect_status 2
[ ! -s "$FAKE/calls" ] || fail "the driver called $(head -n 1 "$FAKE/calls")"
done_case

echo "legacy-subjects-purge.test.sh: $passed cases passed, $failed failed"
[ "$failed" = 0 ]
