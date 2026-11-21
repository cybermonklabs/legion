# shellcheck shell=bash
# shellcheck disable=SC2034 # the three tables are read by name, through bus_row's nameref
# shellcheck disable=SC2154 # root and work are the caller's (see below)
# packages/dispatch/e2e/acceptance/bus/decide.sh
#
# What project-access-bus.sh decides from what it read, kept apart from what it starts, so that
# project-access-bus.test.sh runs every decision against recorded inputs, with no stack:
#
#   which of the check's inputs this checkout holds, and so which sections run (section_gate),
#     which run on a stand-in (stand_in), and the line each prints;
#   whether a reader's record names a project it must not hold (audit_reads);
#   whether the listener read the privacy answer during the run (judge_privacy_reads);
#   and the run's verdict (bus_verdict).
#
# Sourced, never run. The caller sets
#   root   the checkout whose inputs are read
#   work   the run's directory, where the verdict's three files are kept: unrun (a section that did
#          not run, with why), stood-in (a section that ran on a stand-in for an input this
#          checkout lacks) and failed (a judgement that failed or saw nothing)
# and calls bus_files_reset once work exists.

# The inputs of check c-private-bus that a checkout may not hold yet, one row each, `|`-separated:
#   id | what a not-run or stand-in line names: the symbol, and the task that writes it |
#   the files, a glob under root (a _test.go file never counts) |
#   an extended regular expression only the input's own code holds
# An input is merged when some file of the glob holds a line its expression matches. A glob names
# internal/*/ where main renamed the broker's packages (internal/broker to internal/cipher), so a
# merge of main forward finds the same symbol under either name.
bus_inputs=(
  "required-mode|the listener's required privacy mode, ENVOY_PROJECT_PRIVACY_REQUIRED (#t-c-8)|packages/envoy/internal/config/config.go|ENVOY_PROJECT_PRIVACY_REQUIRED"
  "listener-scope|the listener's project scope, cmd/listener/project_scope.go (#t-c-8)|packages/envoy/cmd/listener/project_scope.go|project_not_listed"
  "within-projects|natsgrant.WithinProjects (#t-c-9)|packages/envoy/natsgrant/*.go|^func WithinProjects\\("
  "daemon-held|the auth service holding a daemon to its exchange projects, natsgrant.WithinProjects in natscallout's scope.go (plan E's t-e-16, #4197)|packages/envoy/internal/*/natscallout/scope.go|WithinProjects\\("
  "wake-subjects|the listener's per-project wake subject, contracts.LegionWakeSubject in cmd/listener/wake.go (#t-c-10)|packages/envoy/cmd/listener/wake.go|LegionWakeSubject\\("
  "outbox-subjects|the outbox writing each event's project, contracts.DispatchIssueSubject(event.Project, ...) (#t-c-6)|packages/envoy/internal/dispatch/outbox/publisher.go|DispatchIssueSubject\\(event\\.Project"
  "hub-admission|the admission proof, busprojects.NewHubAdmission (#t-c-11)|packages/envoy/internal/dispatch/busprojects/*.go|^func NewHubAdmission\\("
  "private-hubs|Dispatch's DISPATCH_PRIVATE_HUBS, which starts that proof (plan A's t-a-20)|packages/envoy/cmd/dispatch/settings.go|DISPATCH_PRIVATE_HUBS"
  "tagged-hub|the auth service's tagged-hub rules, natscallout's Request.ServerTags (plan E's t-e-57)|packages/envoy/internal/*/natscallout/callout.go|ServerTags"
  "server-tag|the private hub's server tag in the start script, server_tags (#t-c-24)|packages/envoy/infra/core/nats_callout.go|server_tags"
  "privacy-route|the broker's GET /v1/project-privacy (plan E's t-e-63)|packages/envoy/internal/*/api/project_privacy.go|project-privacy"
)

# The sections of the run, in order, one row each, `|`-separated:
#   section | the inputs it needs, comma-separated ids of bus_inputs | the sections it needs
# A section runs when every input it needs is merged and every section it needs ran (or ran on a
# stand-in); otherwise it prints `not run: <section> (<each missing input>)`, naming the inputs
# behind every section in its way, so the line names what is missing and never only that an
# earlier section did not run. An input a section can stand in for (stand_in) is not listed here.
bus_sections=(
  "prerequisites||"
  "build||prerequisites"
  "hub||build"
  "hub-tag|server-tag|hub"
  "auth||hub"
  "dispatch||auth"
  "tokens||auth"
  "private||dispatch"
  "privacy||private"
  "listener|required-mode,listener-scope|privacy,tokens"
  "sessions||listener"
  "daemon|within-projects,daemon-held,wake-subjects|auth,tokens"
  "writes|outbox-subjects|sessions,daemon"
  "pub-reads||writes"
  "both-ways||writes"
  "privacy-read||writes"
)

# bus_files_reset empties the verdict's three files.
bus_files_reset() {
  : >"$work/unrun"
  : >"$work/stood-in"
  : >"$work/failed"
}

# bus_row TABLE KEY prints the row of TABLE (bus_inputs or bus_sections) whose first field is KEY.
bus_row() {
  local -n _table=$1
  local row
  for row in "${_table[@]}"; do
    [ "${row%%|*}" != "$2" ] || {
      printf '%s\n' "$row"
      return 0
    }
  done
  echo "bus_row: $1 holds no row $2" >&2
  return 1
}

# input_label ID prints what a line names the input ID by.
input_label() {
  local row
  row=$(bus_row bus_inputs "$1") || return 1
  IFS='|' read -r _ label _ _ <<<"$row"
  printf '%s\n' "$label"
}

# input_present ID succeeds when this checkout holds the input ID: some file of its glob that is no
# test holds a line its expression matches. bus_read_inputs reads every input once for root; until
# it has, and for any other root, each call reads the files itself.
declare -A bus_present=()
bus_present_root=''
input_present() {
  local row glob pattern file
  if [ "$bus_present_root" = "${root:-}" ] && [ -n "${bus_present[$1]:-}" ]; then
    [ "${bus_present[$1]}" = yes ]
    return
  fi
  row=$(bus_row bus_inputs "$1") || return 2
  IFS='|' read -r _ _ glob pattern <<<"$row"
  # The glob is unquoted on purpose: it is the pattern, expanded under root.
  # shellcheck disable=SC2086
  for file in "$root"/$glob; do
    [ -f "$file" ] || continue
    case "$file" in *_test.go) continue ;; esac
    grep -qE -- "$pattern" "$file" && return 0
  done
  return 1
}

# bus_read_inputs reads whether root holds each input, once, so every later decision in this shell
# and the subshells it starts answers from that one reading.
bus_read_inputs() {
  local row id
  bus_present=()
  bus_present_root=''
  for row in "${bus_inputs[@]}"; do
    id=${row%%|*}
    if input_present "$id"; then bus_present[$id]=yes; else bus_present[$id]=no; fi
  done
  bus_present_root=$root
}

# section_missing SECTION prints, one per line and each once, the label of every input SECTION
# needs that this checkout lacks, its own and those of every section in its way.
section_missing() {
  local row inputs needs input need
  row=$(bus_row bus_sections "$1") || return 2
  IFS='|' read -r _ inputs needs <<<"$row"
  {
    if [ -n "$needs" ]; then
      for need in ${needs//,/ }; do
        section_missing "$need"
      done
    fi
    if [ -n "$inputs" ]; then
      for input in ${inputs//,/ }; do
        input_present "$input" || input_label "$input"
      done
    fi
  } | awk '!seen[$0]++'
}

# section_gate SECTION succeeds when SECTION can run. Otherwise it prints
# `not run: SECTION (<missing input>; <missing input>)`, keeps that line for the verdict, and fails.
section_gate() {
  local missing line
  missing=$(section_missing "$1") || return 2
  [ -n "$missing" ] || return 0
  line="not run: $1 (${missing//$'\n'/; })"
  printf '%s\n' "$line" | tee -a "$work/unrun"
  return 1
}

# The sections that run on a stand-in while this checkout lacks an input, one row each,
# `|`-separated: section | the inputs it stands in for, comma-separated ids of bus_inputs | what
# stands in. Each stand-in is a harness piece the check names, used only until its input merges;
# the run then takes the real path with no change here.
bus_stand_ins=(
  "private|hub-admission,private-hubs,tagged-hub,server-tag|PRIV is made private by the store write the visibility route makes, after the route refuses it 409 PRIVATE_PROJECTS_UNAVAILABLE with its other preconditions met"
  "privacy|privacy-route|bus/project-privacy.ts serves GET /v1/project-privacy from Dispatch's rows, as the broker's route does"
)

# stand_in_missing SECTION prints, `; `-separated, the label of each input bus_stand_ins lists for
# SECTION that this checkout lacks, and nothing when it holds them all or SECTION has no stand-in.
stand_in_missing() {
  local row inputs input missing=''
  row=$(bus_row bus_stand_ins "$1" 2>/dev/null) || return 0
  IFS='|' read -r _ inputs _ <<<"$row"
  for input in ${inputs//,/ }; do
    input_present "$input" || missing+="${missing:+; }$(input_label "$input")"
  done
  printf '%s' "$missing"
}

# stand_in SECTION succeeds when SECTION runs on its stand-in: the checkout lacks an input
# bus_stand_ins lists for it. It then prints `stand-in: SECTION: <what stands in>, until <each
# missing input>` and keeps the line for the verdict, which a stand-in fails. It fails, printing
# nothing, when the checkout holds every such input or SECTION has no stand-in.
stand_in() {
  local row what missing line
  missing=$(stand_in_missing "$1")
  [ -n "$missing" ] || return 1
  row=$(bus_row bus_stand_ins "$1")
  IFS='|' read -r _ _ what <<<"$row"
  line="stand-in: $1: $what, until $missing"
  printf '%s\n' "$line" | tee -a "$work/stood-in"
  return 0
}

# bus_fail LINE prints LINE and keeps it for the verdict.
bus_fail() {
  printf '%s\n' "$1" | tee -a "$work/failed"
}

# audit_reads READER FILE PROJECT MARKERS judges what READER received, FILE holding one JSON object
# per message: its `subject` and its `data` (a string, or any JSON value, read as its text). A
# message names PROJECT when its subject holds PROJECT as a token, a token that opens with
# `PROJECT-` (an issue key), or `notifications.legion.<project in lower case>.` (its wake lane), or
# when its subject or data holds any line of MARKERS, the file of what the run wrote in PROJECT (the
# texts, the ids, the session) that no message of another project can hold. A GitHub event
# (`notifications.github.`) is a message of no project and is not judged. It prints one line and
# returns 0 for PASS, 1 for FAIL, quoting the first message that names PROJECT, or 2 for UNKNOWN,
# when the reader read no message it could judge: an empty read never passes.
audit_reads() {
  local reader=$1 file=$2 project=$3 markers=$4 answer judged skipped named first
  if [ ! -s "$file" ]; then
    echo "UNKNOWN $reader: read nothing (no record at $file)"
    return 2
  fi
  answer=$(jq -rs --arg p "$project" --rawfile m "$markers" '
    def text: if type == "string" then . elif . == null then "" else tojson end;
    def names($p; $lower; $marks):
      (.subject | text) as $s | (.data | text) as $d
      | any(($s | split("."))[]; . == $p or startswith($p + "-"))
        or ($s | startswith("notifications.legion." + $lower + "."))
        or any($marks[]; . as $mark | ($s | contains($mark)) or ($d | contains($mark)));
    ($m | split("\n") | map(select(length > 0))) as $marks
    | [.[] | select((.subject | text) | startswith("notifications.github.") | not)] as $judged
    | (length - ($judged | length)) as $skipped
    | [$judged[] | select(names($p; $p | ascii_downcase; $marks))] as $named
    | "\($judged | length)\t\($skipped)\t\($named | length)\t"
      + (if ($named | length) > 0 then
          ($named[0] | ((.subject | text) + " " + ((.data | text)[0:200])) | gsub("[\t\n\r]"; " "))
        else "" end)
  ' "$file" 2>&1) || {
    echo "UNKNOWN $reader: its record at $file is not one JSON object per line: ${answer:0:300}"
    return 2
  }
  IFS=$'\t' read -r judged skipped named first <<<"$answer"
  if [ "$judged" -eq 0 ]; then
    echo "UNKNOWN $reader: read no message of any project ($skipped GitHub events, which name none)"
    return 2
  fi
  if [ "$named" -gt 0 ]; then
    echo "FAIL $reader: received $named messages of $project, the first: $first"
    return 1
  fi
  echo "PASS $reader: $judged messages read, none of $project ($skipped GitHub events not judged)"
  return 0
}

# judge_privacy_reads LOG CALLER judges the privacy route's request log, LOG holding one JSON object
# per request it answered (`caller`, `status`): PASS when CALLER read it and was answered 200 at
# least once during the run, else FAIL naming CALLER. A log that is missing is UNKNOWN.
judge_privacy_reads() {
  local log=$1 caller=$2 reads
  if [ ! -e "$log" ]; then
    echo "UNKNOWN privacy reads: the privacy route kept no request log at $log"
    return 2
  fi
  reads=$(jq -s --arg c "$caller" '[.[] | select(.caller == $c and .status == 200)] | length' "$log" 2>/dev/null) || {
    echo "UNKNOWN privacy reads: the request log at $log is not one JSON object per line"
    return 2
  }
  if [ "$reads" -eq 0 ]; then
    echo "FAIL privacy reads: $caller made no privacy read that was answered during the run"
    return 1
  fi
  echo "PASS privacy reads: $caller read the privacy answer $reads times"
  return 0
}

# bus_verdict prints every section that did not run, ran on a stand-in or failed a judgement, and
# fails when there is any; a run passes only when every section ran on its real inputs and every
# judgement passed.
bus_verdict() {
  local unrun stood failed
  unrun=$(wc -l <"$work/unrun")
  stood=$(wc -l <"$work/stood-in")
  failed=$(wc -l <"$work/failed")
  if [ "$unrun" -eq 0 ] && [ "$stood" -eq 0 ] && [ "$failed" -eq 0 ]; then
    echo "c-private-bus: every section ran on its real inputs and every judgement passed"
    return 0
  fi
  echo "c-private-bus: not passed: $failed failed, $unrun not run, $stood on a stand-in"
  cat "$work/failed" "$work/unrun" "$work/stood-in"
  return 1
}
