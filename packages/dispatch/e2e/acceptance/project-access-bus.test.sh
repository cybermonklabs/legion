#!/usr/bin/env bash
# packages/dispatch/e2e/acceptance/project-access-bus.test.sh
#
# Proves project-access-bus.sh's decisions with no stack, as packages/envoy/scripts/dev-broker.test.sh
# proves its driver's: every decision is run against recorded inputs, and the driver itself is run
# against fake checkouts.
#
#   inputs     the driver's --inputs, run in a copy of it laid in fake checkouts: with no input it
#              names each missing input on the not-run line of each section that needs it, and
#              each stand-in on its stand-in line, and exits 1; with every input it prints every
#              section ready and exits 0; with every input but one, each section in that input's
#              way names that one input and no other; an input's expression held only by a
#              _test.go file does not count.
#   audit      audit_reads against recorded messages: a record holding no message of the project
#              passes; a subject of the project in the new form, the old form or its wake lane, and
#              a message whose data holds a marker of the project, each fail quoting the message; a
#              GitHub event is not judged; an empty, missing, unreadable or GitHub-only record is
#              UNKNOWN; and another project whose key begins with the same letters is not the
#              project.
#   privacy    judge_privacy_reads: a listener read answered 200 passes; none fails naming the
#              listener; a missing log is UNKNOWN.
#   verdict    bus_verdict passes only with nothing failed, nothing not run and no stand-in.
#   arguments  the driver refuses a control it does not know.
#
# Each case prints `ok <case>` or `FAIL <case>: <why>`; the last line counts them, and the run
# exits 1 when any failed. Run it as `bash packages/dispatch/e2e/acceptance/project-access-bus.test.sh`.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
checkout=$(cd "$here/../../../.." && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/project-access-bus-test.XXXXXX")
trap 'rm -rf -- "${scratch:?}"' EXIT

passed=0
failed=0
ok() {
  passed=$((passed + 1))
  echo "ok $1"
}
failed() {
  failed=$((failed + 1))
  echo "FAIL $1: $2"
}

# One line that matches each input's expression, keyed by its id. Each is checked against its row
# below before any fake checkout is laid, so a row whose expression changes fails here by name.
declare -A sample=(
  [required-mode]='const envPrivacyRequired = "ENVOY_PROJECT_PRIVACY_REQUIRED"'
  [listener-scope]='const projectNotListed = "project_not_listed"'
  [within-projects]='func WithinProjects(grant Permissions, scope Scope) (Permissions, error) {'
  [daemon-held]='	return natsgrant.WithinProjects(natsgrant.Daemon(), scope)'
  [wake-subjects]='	subject := contracts.LegionWakeSubject(token)'
  [outbox-subjects]='	subject := contracts.DispatchIssueSubject(event.Project, key, kind)'
  [hub-admission]='func NewHubAdmission(clients []ProofClient, broker Broker) *HubAdmission {'
  [private-hubs]='	{Name: "DISPATCH_PRIVATE_HUBS", Required: "no"},'
  [tagged-hub]='	ServerTags []string'
  [server-tag]='  server_tags: ["envoy-private-projects"]'
  [privacy-route]='	mux.Handle("GET /v1/project-privacy", route)'
)

# shellcheck source-path=SCRIPTDIR source=bus/decide.sh
. "$here/bus/decide.sh"

# The tables are the driver's own, read from bus/decide.sh above; every input has a sample, and each
# sample matches its row's expression.
for row in "${bus_inputs[@]}"; do
  IFS='|' read -r id _ _ pattern <<<"$row"
  if [ -z "${sample[$id]:-}" ]; then
    failed "samples" "the input $id has no sample line in this test"
  elif ! grep -qE -- "$pattern" <<<"${sample[$id]}"; then
    failed "samples" "the sample line of $id does not match its expression $pattern"
  fi
done
[ "$failed" -gt 0 ] || ok "samples"

# fake_checkout DIR [ID...] lays a checkout at DIR holding the driver, what it sources, and a file
# for each input ID given, at its glob with each * read as `x`, holding that input's sample.
fake_checkout() {
  local dir=$1 row id glob path
  shift
  mkdir -p "$dir/packages/dispatch/e2e/acceptance/bus" "$dir/scripts/e2e/lib"
  cp "$here/project-access-bus.sh" "$here/access-lib.sh" "$dir/packages/dispatch/e2e/acceptance/"
  cp "$here/bus/decide.sh" "$dir/packages/dispatch/e2e/acceptance/bus/"
  cp "$checkout"/scripts/e2e/lib/{two-builds.sh,rig.sh,loopback-dsn.sh,loopback-dsn.py,port-file.sh} "$dir/scripts/e2e/lib/"
  for id in "$@"; do
    row=$(bus_row bus_inputs "$id")
    IFS='|' read -r _ _ glob _ <<<"$row"
    path=$dir/${glob//\*/x}
    [ "${path##*/}" != x ] || path=${path%/x}/x.go
    case "$path" in *.go | *.sh | *.ts) ;; *) path=$path.go ;; esac
    mkdir -p "$(dirname "$path")"
    printf '%s\n' "${sample[$id]}" >>"$path"
  done
}

all_inputs=()
for row in "${bus_inputs[@]}"; do
  all_inputs+=("${row%%|*}")
done

# inputs RUN: the driver's --inputs in the fake checkout RUN, its output in $scratch/RUN.out and
# its status in inputs_status.
inputs_status=0
inputs() {
  inputs_status=0
  env -i PATH="$PATH" TMPDIR="$scratch" bash "$scratch/$1/packages/dispatch/e2e/acceptance/project-access-bus.sh" --inputs \
    >"$scratch/$1.out" 2>&1 || inputs_status=$?
}

# --- inputs: none merged.
fake_checkout "$scratch/none"
inputs none
case_name="inputs: a checkout with no input"
problem=''
[ "$inputs_status" = 1 ] || problem="exit $inputs_status, want 1"
grep -qx "ready dispatch" "$scratch/none.out" || problem+="; no 'ready dispatch'"
grep -qxF "not run: hub-tag ($(input_label server-tag))" "$scratch/none.out" ||
  problem+="; hub-tag's line does not name $(input_label server-tag) alone"
grep -qxF "not run: listener ($(input_label required-mode); $(input_label listener-scope))" "$scratch/none.out" ||
  problem+="; listener's line does not name t-c-8's two inputs"
# Each label below is named by one stand-in line alone, so finding it finds that line.
grep -q '^stand-in: privacy: ' "$scratch/none.out" && grep -qF ", until $(input_label privacy-route)" "$scratch/none.out" ||
  problem+="; no stand-in line for privacy naming $(input_label privacy-route)"
grep -q '^stand-in: private: ' "$scratch/none.out" && grep -qF "$(input_label hub-admission)" "$scratch/none.out" ||
  problem+="; no stand-in line for private naming $(input_label hub-admission)"
for section in writes pub-reads both-ways privacy-read; do
  grep -q "^not run: $section (" "$scratch/none.out" || problem+="; no not-run line for $section"
done
if [ -z "$problem" ]; then ok "$case_name"; else failed "$case_name" "${problem#; }: $(<"$scratch/none.out")"; fi

# --- inputs: every input merged.
fake_checkout "$scratch/all" "${all_inputs[@]}"
inputs all
case_name="inputs: a checkout with every input"
problem=''
[ "$inputs_status" = 0 ] || problem="exit $inputs_status, want 0"
for row in "${bus_sections[@]}"; do
  grep -qx "ready ${row%%|*}" "$scratch/all.out" || problem+="; no 'ready ${row%%|*}'"
done
! grep -qE '^(not run|stand-in):' "$scratch/all.out" || problem+="; a section is not run or stood in"
if [ -z "$problem" ]; then ok "$case_name"; else failed "$case_name" "${problem#; }: $(<"$scratch/all.out")"; fi

# --- inputs: every input but one, for each input a section needs.
for missing in required-mode within-projects daemon-held wake-subjects outbox-subjects server-tag; do
  rest=()
  for id in "${all_inputs[@]}"; do
    [ "$id" = "$missing" ] || rest+=("$id")
  done
  fake_checkout "$scratch/no-$missing" "${rest[@]}"
  inputs "no-$missing"
  case_name="inputs: every input but $missing"
  problem=''
  [ "$inputs_status" = 1 ] || problem="exit $inputs_status, want 1"
  label=$(input_label "$missing")
  lines=$(grep '^not run: ' "$scratch/no-$missing.out" || true)
  [ -n "$lines" ] || problem+="; no section is not run"
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    [ "${line#*(}" = "$label)" ] || problem+="; '$line' names more than, or other than, $label"
  done <<<"$lines"
  if [ -z "$problem" ]; then ok "$case_name"; else failed "$case_name" "${problem#; }: $(<"$scratch/no-$missing.out")"; fi
done

# --- inputs: an expression held by a test file alone is not the input.
fake_checkout "$scratch/test-only" "${all_inputs[@]}"
mv "$scratch/test-only/packages/envoy/cmd/listener/wake.go" "$scratch/test-only/packages/envoy/cmd/listener/wake_test.go"
case_name="inputs: an expression in a _test.go file alone"
work=$scratch/test-only-work
root=$scratch/test-only
mkdir -p "$work"
problem=''
! input_present wake-subjects || problem="input_present read wake_test.go as the input"
# Control: the same line in wake.go is the input.
cp "$root/packages/envoy/cmd/listener/wake_test.go" "$root/packages/envoy/cmd/listener/wake.go"
input_present wake-subjects || problem+="; the control, the line in wake.go, is not read as the input"
if [ -z "$problem" ]; then ok "$case_name"; else failed "$case_name" "${problem#; }"; fi

# --- audit_reads against recorded messages.
work=$scratch/audit
mkdir -p "$work"
printf '%s\n' PRIV-marker-0a1b2c PRIV-1 pab-priv-0001 >"$work/markers"
pub_lines='{"subject":"notifications.dispatch.issue.PUB.PUB-1.comment.created","data":"{\"issue\":\"PUB-1\",\"body\":\"PUB-marker-ffff\"}"}
{"subject":"notifications.agent.PUB.pab-pub-0001","data":{"body":"A direct word"}}
{"subject":"notifications.legion.pub.wake","data":"role_message"}'
# audit_case NAME WANT_STATUS WANT_TEXT LINES: audit_reads of LINES returns WANT_STATUS and prints a
# line holding WANT_TEXT.
audit_case() {
  local name=$1 want=$2 text=$3 status=0 line
  printf '%s\n' "$4" >"$work/record.jsonl"
  line=$(audit_reads reader "$work/record.jsonl" PRIV "$work/markers") || status=$?
  if [ "$status" = "$want" ] && grep -qF -- "$text" <<<"$line"; then
    ok "audit: $name"
  else
    failed "audit: $name" "status $status, want $want; printed '$line', want it to hold '$text'"
  fi
}
audit_case "a record of PUB alone passes" 0 "PASS reader: 3 messages read, none of PRIV" "$pub_lines"
audit_case "a new-form PRIV subject fails, quoted" 1 "notifications.dispatch.issue.PRIV.PRIV-1.comment.created" \
  "$pub_lines"$'\n''{"subject":"notifications.dispatch.issue.PRIV.PRIV-1.comment.created","data":"{}"}'
audit_case "an old-form PRIV subject fails, quoted" 1 "notifications.dispatch.issue.PRIV-1.comment.created" \
  "$pub_lines"$'\n''{"subject":"notifications.dispatch.issue.PRIV-1.comment.created","data":"{}"}'
audit_case "PRIV's role lane fails" 1 "notifications.role.PRIV.legion-priv-1-architect" \
  "$pub_lines"$'\n''{"subject":"notifications.role.PRIV.legion-priv-1-architect","data":"{}"}'
audit_case "PRIV's wake lane fails" 1 "notifications.legion.priv.wake" \
  "$pub_lines"$'\n''{"subject":"notifications.legion.priv.wake","data":"{}"}'
audit_case "a PUB subject carrying a PRIV marker fails" 1 "PRIV-marker-0a1b2c" \
  "$pub_lines"$'\n''{"subject":"notifications.legion.wake","data":"{\"kind\":\"role_message\",\"body\":\"PRIV-marker-0a1b2c\"}"}'
audit_case "a delivery naming the PRIV session fails" 1 "pab-priv-0001" \
  "$pub_lines"$'\n''{"subject":"","data":{"content":"from pab-priv-0001"}}'
audit_case "a GitHub event naming PRIV is not judged" 0 "1 GitHub events not judged" \
  "$pub_lines"$'\n''{"subject":"notifications.github.acme.priv.pull_request.4.opened","data":"PRIV-1 PRIV-marker-0a1b2c"}'
audit_case "another project whose key opens with PRIV is not PRIV" 0 "PASS reader: 4 messages read" \
  "$pub_lines"$'\n''{"subject":"notifications.dispatch.issue.PRIVATE.PRIVATE-1.comment.created","data":"{}"}'
audit_case "a record of GitHub events alone is UNKNOWN" 2 "UNKNOWN reader: read no message of any project" \
  '{"subject":"notifications.github.acme.pub.push","data":"{}"}'
audit_case "a record that is not JSON is UNKNOWN" 2 "is not one JSON object per line" 'not json'
: >"$work/empty.jsonl"
status=0
line=$(audit_reads reader "$work/empty.jsonl" PRIV "$work/markers") || status=$?
if [ "$status" = 2 ] && grep -q '^UNKNOWN reader: read nothing' <<<"$line"; then ok "audit: an empty record is UNKNOWN"; else failed "audit: an empty record is UNKNOWN" "status $status: $line"; fi
status=0
line=$(audit_reads reader "$work/absent.jsonl" PRIV "$work/markers") || status=$?
if [ "$status" = 2 ] && grep -q '^UNKNOWN reader: read nothing' <<<"$line"; then ok "audit: a missing record is UNKNOWN"; else failed "audit: a missing record is UNKNOWN" "status $status: $line"; fi

# --- judge_privacy_reads.
privacy_case() {
  local name=$1 want=$2 text=$3 status=0 line
  printf '%s' "$4" >"$work/privacy.jsonl"
  line=$(judge_privacy_reads "$work/privacy.jsonl" listener) || status=$?
  if [ "$status" = "$want" ] && grep -qF -- "$text" <<<"$line"; then
    ok "privacy: $name"
  else
    failed "privacy: $name" "status $status, want $want; printed '$line'"
  fi
}
privacy_case "a listener read answered 200 passes" 0 "PASS privacy reads: listener read the privacy answer 2 times" \
  $'{"caller":"listener","status":200}\n{"caller":"unknown","status":403}\n{"caller":"listener","status":200}\n'
privacy_case "no listener read fails, naming the listener" 1 "FAIL privacy reads: listener made no privacy read" \
  $'{"caller":"unknown","status":403}\n'
privacy_case "a listener read that failed is no read" 1 "FAIL privacy reads: listener" \
  $'{"caller":"listener","status":502}\n'
privacy_case "an empty log fails, naming the listener" 1 "FAIL privacy reads: listener" ''
status=0
line=$(judge_privacy_reads "$work/no-log.jsonl" listener) || status=$?
if [ "$status" = 2 ]; then ok "privacy: a missing log is UNKNOWN"; else failed "privacy: a missing log is UNKNOWN" "status $status: $line"; fi

# --- bus_verdict.
work=$scratch/verdict
mkdir -p "$work"
bus_files_reset
status=0
bus_verdict >/dev/null || status=$?
if [ "$status" = 0 ]; then ok "verdict: nothing failed, not run or stood in passes"; else failed "verdict: a clean run" "status $status"; fi
for file in unrun stood-in failed; do
  bus_files_reset
  echo "a line of $file" >"$work/$file"
  status=0
  said=$(bus_verdict) || status=$?
  if [ "$status" = 1 ] && grep -qF "a line of $file" <<<"$said"; then
    ok "verdict: a line in $file fails, printed"
  else
    failed "verdict: a line in $file fails, printed" "status $status: $said"
  fi
done

# --- arguments.
status=0
said=$(env -i PATH="$PATH" bash "$here/project-access-bus.sh" --control nonsense 2>&1) || status=$?
if [ "$status" = 1 ] && grep -qF "the controls are daemon-every-project and privacy-unread" <<<"$said"; then
  ok "arguments: an unknown control is refused"
else
  failed "arguments: an unknown control is refused" "status $status: $said"
fi

echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
