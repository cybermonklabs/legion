# shellcheck shell=sh
# packages/envoy/scripts/lib/legacy-subjects-purge-nats.sh
#
# The NATS calls of legacy-subjects-purge.sh, run by the one-off Job that driver creates in the
# cluster: the driver hands this file to the Job's container as its `sh -c` script, so it runs in
# the nats-box image (busybox sh, the nats CLI, jq and base64). It connects as the nkey user of
# NATS_NKEY (the seed file the driver's SecretProviderClass mounts) at NATS_URL, every call taking
# NATS_INBOX_PREFIX as its inbox prefix when set, and:
#
#   1. reads the stream's info, and purges nothing unless its envoy_stream_id is STREAM_ID, the
#      stream the gate's reading read;
#   2. lists the stream's subjects under each family the rename moved, one page each, and purges
#      nothing unless each was read whole; the old-form ones are the gate's own rule (FORMS_JQ,
#      lib/legacy-subjects-gate-forms.jq, old_form);
#   3. reads each old-form subject's last message (its subject, stream sequence and time alone),
#      and purges nothing if any is after LAST_OLD_SEQ, the newest old-form message the reading
#      found: one published since was published after (a1), (a2), (a3) and (b) were read, by a
#      client the reading did not see, and a consumer may not have it yet;
#   4. purges each old-form subject by itself, $JS.API.STREAM.PURGE.ENVOY_NOTIFICATIONS with the
#      subject as the filter, a literal subject and never a wildcard, so no new-form message goes,
#      and LAST_OLD_SEQ + 1 as its sequence, so it takes the messages up to LAST_OLD_SEQ alone: one
#      published after step 3 read the subject is one a consumer may not have yet, and it stays;
#   5. lists the stream's subjects again, for the driver to read back that no old-form one is left.
#
# Each call is printed as one line of five tab-separated fields, as legacy-subjects-gate-nats.sh
# prints its reads: the call's name, its argument, the nats CLI's exit status, and its standard
# output and standard error, each in base64 on one line. A refusal to purge is the line `refuse`
# naming why. The last line is `end`, so a log cut short is told from one that is whole.
set -u

stream=ENVOY_NOTIFICATIONS
work=$(mktemp -d "${WORK_DIR:-/tmp}/purge.XXXXXX")
trap 'rm -rf "$work"' EXIT

# n runs the nats CLI with the inbox prefix the driver named, if any.
n() {
  if [ -n "${NATS_INBOX_PREFIX:-}" ]; then
    nats "$@" --inbox-prefix "$NATS_INBOX_PREFIX"
  else
    nats "$@"
  fi
}

# emit LEG ARG STATUS prints one call's line from the output and error the call left in $work.
emit() {
  printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$(base64 -w 0 <"$work/out")" "$(base64 -w 0 <"$work/err")"
}

# request LEG ARG SUBJECT BODY makes one request and prints its line.
request() {
  n req --raw "$3" "$4" >"$work/out" 2>"$work/err"
  emit "$1" "$2" "$?"
}

# refuse WHY prints the refusal and the end line, and stops: nothing more is called.
refuse() {
  printf '%s' "$1" >"$work/out"
  : >"$work/err"
  emit refuse - 1
  printf 'end\t-\t0\t\t\n'
  exit 0
}

printf '%s\n' "${FORMS_JQ:-}" >"$work/legacy-subjects-gate-forms.jq"

# subjects LEG lists each family's subjects, one page each, printing each page, and writes the
# old-form ones to $work/old; it returns 1 when a page was not read whole (a refused or failed
# call, an answer that is no subject list, or fewer subjects than the server counts).
subjects() {
  : >"$work/old"
  whole=0
  for family in \
    'notifications.dispatch.issue.>' \
    'notifications.agent.>' \
    'notifications.envoy.exceptions.notifications.agent.>' \
    'notifications.envoy.kept.>'; do
    request "$1" "$family" "\$JS.API.STREAM.INFO.$stream" "{\"subjects_filter\":\"$family\"}"
    jq -e 'type == "object" and .type == "io.nats.jetstream.api.v1.stream_info_response" and (.error | not)
      and ((.state.subjects // {}) | type) == "object" and ((.state.subjects // {}) | length) == (.total // -1)' \
      "$work/out" >/dev/null 2>&1 || whole=1
    jq -r -L "$work" 'include "legacy-subjects-gate-forms"; .state.subjects // {} | keys[] | select(old_form)' \
      "$work/out" >>"$work/old" 2>/dev/null || whole=1
  done
  return "$whole"
}

request stream_info - "\$JS.API.STREAM.INFO.$stream" ""
id=$(jq -r 'if type == "object" then .config.metadata.envoy_stream_id // "" else "" end' "$work/out" 2>/dev/null)
[ -n "${STREAM_ID:-}" ] && [ "$id" = "$STREAM_ID" ] ||
  refuse "the stream's envoy_stream_id is ${id:-none}, not ${STREAM_ID:-none}, the one the reading read"

subjects before || refuse "the stream's subjects were not read whole, so no old-form subject is known"
sort -u "$work/old" >"$work/old-subjects"

while IFS= read -r subject; do
  n req --raw "\$JS.API.STREAM.MSG.GET.$stream" "$(jq -cn --arg s "$subject" '{last_by_subj: $s}')" >"$work/msg" 2>"$work/err"
  status=$?
  jq -c 'if type == "object" then {type, error, message: (if (.message | type) == "object" then {subject: .message.subject, seq: .message.seq, time: .message.time} else null end)} else error("no object") end' \
    "$work/msg" >"$work/out" 2>/dev/null || : >"$work/out"
  emit old_last "$subject" "$status"
  seq=$(jq -r '.message.seq // empty' "$work/out" 2>/dev/null)
  case "$seq" in
    '' | *[!0-9]*)
      # A subject whose messages expired since the list answers no message found, and holds none.
      jq -e '.error.err_code == 10037 or .error.code == 404' "$work/out" >/dev/null 2>&1 ||
        refuse "the last message of $subject was not read, so whether it came after the reading is not known"
      ;;
    *)
      case "${LAST_OLD_SEQ:-}" in
        '' | *[!0-9]*) refuse "$subject holds a message at stream sequence $seq, and the reading found no old-form message: one was published after it" ;;
      esac
      [ "$seq" -le "$LAST_OLD_SEQ" ] ||
        refuse "$subject holds a message at stream sequence $seq, after $LAST_OLD_SEQ, the newest old-form message the reading found: one was published after it"
      ;;
  esac
done <"$work/old-subjects"

# The purge's seq keeps every message from that sequence on. With no LAST_OLD_SEQ, step 3 found no
# old-form subject holding a message, so nothing is the reading's to purge.
case "${LAST_OLD_SEQ:-}" in
  '' | *[!0-9]*) : >"$work/old-subjects" ;;
  *) keep_from=$((LAST_OLD_SEQ + 1)) ;;
esac
while IFS= read -r subject; do
  request purge "$subject" "\$JS.API.STREAM.PURGE.$stream" "$(jq -cn --arg s "$subject" --argjson keep "$keep_from" '{filter: $s, seq: $keep}')"
done <"$work/old-subjects"

subjects after || :
printf 'end\t-\t0\t\t\n'
