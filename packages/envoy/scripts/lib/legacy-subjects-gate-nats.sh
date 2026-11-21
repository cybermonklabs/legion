# shellcheck shell=sh
# packages/envoy/scripts/lib/legacy-subjects-gate-nats.sh
#
# The NATS reads of legacy-subjects-gate.sh, run by the one-off Job that gate creates in the
# cluster: the gate hands this file to the Job's container as its `sh -c` script, so it runs in the
# nats-box image (busybox sh, the nats CLI, nsc, jq and base64), never on the box that runs the
# gate. It connects as the in-cluster listener does: NATS_URL is the listener's NATS_URLS, and the
# credential is either NATS_NKEY, the listener's seed file (NATS_NKEY_SEED_FILE), which the nats CLI
# reads itself, or NATS_TOKEN_FILE, the listener's projected NATS token, which this script presents
# as a service does to the auth service: user `service` (natscred.ServiceUser) and the token as its
# password, NATS_USER and NATS_PASSWORD for the CLI. NATS_INBOX_PREFIX, when set, is passed to every
# nats call as --inbox-prefix, for a grant that subscribes only its identity's own inbox prefix. It
# makes read requests only: who the server says the connection is, the stream's info, the stream's
# consumer list, the stream's subjects under each family the rename moved, the last message of each
# of those subjects that is in an old form (at most OLD_LAST_MAX of them, the rest counted), the
# interest bucket's keys and each key's record. It judges nothing; the gate does. Which subjects
# are in an old form is the gate's own rule, which the gate hands the Job in FORMS_JQ
# (lib/legacy-subjects-gate-forms.jq, old_form); without it no last message is read, and the gate
# names no newest old-form message.
#
# Each read is printed as one line of five tab-separated fields: the read's name, its argument,
# the nats CLI's exit status, and its standard output and standard error, each in base64 on one
# line. The nats CLI answers a request the server's permissions refuse with status 0, no output and
# the violation on standard error (measured with nats-box 0.19.0, the pinned image), so the status
# alone cannot say a read failed: the gate reads each answer's own type and error. A list read that
# answered nothing is read no further, so a refused list ends at once rather than at the Job's
# deadline. The last line is `end`, so a log cut short is told from one that is whole.
#
# What the log holds is what the gate judges: of an interest record only its session id and its
# topics, the record's directory, machine and every other field left out; of a stored message only
# its subject, its stream sequence and its time, never its headers or its content. The credential
# is named by its public part alone: the nkey's public key (nsc derives it in a directory of memory
# the gate's Job mounts at WORK_DIR, deleted with the pod), or the token's subject, audience,
# issuer and expiry, never the token.
#
# The JetStream API is called by its subjects, `$JS.API.CONSUMER.LIST.<stream>`,
# `$JS.API.STREAM.INFO.<stream>` and `$JS.API.STREAM.MSG.GET.<stream>`, because those are what the
# listener's grant holds (natsgrant.Listener: sessionConsumerCalls and listenerGrant); `nats
# consumer ls` asks `$JS.API.CONSUMER.NAMES.<stream>`, which it does not. A list longer than one
# answer is read page by page, each page starting at the last item of the page before (the server
# sorts both lists by name), so the gate can see that the list did not shift between two pages.
set -u

stream=ENVOY_NOTIFICATIONS
bucket=envoy_interests
work=$(mktemp -d "${WORK_DIR:-/tmp}/reads.XXXXXX")
trap 'rm -rf "$work"' EXIT

# n runs the nats CLI with the inbox prefix the gate named, if any.
n() {
  if [ -n "${NATS_INBOX_PREFIX:-}" ]; then
    nats "$@" --inbox-prefix "$NATS_INBOX_PREFIX"
  else
    nats "$@"
  fi
}

# emit LEG ARG STATUS prints one read's line from the output and error the read left in $work.
emit() {
  printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$(base64 -w 0 <"$work/out")" "$(base64 -w 0 <"$work/err")"
}

# request LEG ARG SUBJECT BODY makes one request and prints its line.
request() {
  n req --raw "$3" "$4" >"$work/out" 2>"$work/err"
  emit "$1" "$2" "$?"
}

# next_page prints the offset of the page after the list answer in $work/out, whose items the jq
# path $1 reads (`.consumers`, or `.state.subjects` for a subject list, an object), or `done` when
# that answer was the last page, held no item, or is no list answer at all: an empty answer (a
# refused request) included. The next page starts at the last item of this one, so the two pages
# share one item, unless this page held a single item.
next_page() {
  [ -s "$work/out" ] || {
    echo "done"
    return
  }
  page=$(jq -r "($1) as \$items
    | if (\$items | type) == \"array\" or (\$items | type) == \"object\"
      then (.offset + (\$items | length)) as \$end
        | if (.total | type) == \"number\" and (.offset | type) == \"number\" and (\$items | length) > 0 and \$end < .total
          then (if (\$items | length) > 1 then \$end - 1 else \$end end) else \"done\" end
      else \"done\" end" "$work/out" 2>/dev/null)
  case "$page" in
    '' | *[!0-9]*) echo "done" ;;
    *) echo "$page" ;;
  esac
}

# What the credential names, read first: a projected token is the CLI's credential once it is in
# NATS_PASSWORD beside NATS_USER=service, the shape the auth service admits a service by
# (natscred.Service: CONNECT's user and password; natscallout's credentialKind), never NATS_TOKEN,
# which it reads as no credential at all.
: >"$work/err"
if [ -n "${NATS_TOKEN_FILE:-}" ]; then
  if token=$(cat "$NATS_TOKEN_FILE" 2>"$work/err") && [ -n "$token" ]; then
    NATS_USER=service
    NATS_PASSWORD=$token
    export NATS_USER NATS_PASSWORD
    payload=$(printf '%s' "$token" | cut -d. -f2 | tr '_-' '/+')
    while [ $((${#payload} % 4)) != 0 ]; do payload="$payload="; done
    printf '%s' "$payload" | base64 -d 2>>"$work/err" | jq -c '{sub, aud, iss, exp}' >"$work/out" 2>>"$work/err"
    emit credential token "$?"
  else
    : >"$work/out"
    echo "the token file $NATS_TOKEN_FILE is empty or cannot be read" >>"$work/err"
    emit credential token 1
  fi
else
  mkdir -p "$work/key"
  cp "${NATS_NKEY:-}" "$work/key/listener.nk" 2>"$work/err" &&
    HOME="$work" nsc import keys --all-dirs "$work/nsc" --dir "$work/key" >"$work/nsc.out" 2>&1
  status=$?
  grep -oE 'U[A-Z2-7]{55}' "$work/nsc.out" 2>/dev/null | head -n 1 >"$work/out"
  [ "$status" = 0 ] || cat "$work/nsc.out" >>"$work/err" 2>/dev/null
  rm -rf "$work/key" "$work/nsc"
  emit credential nkey "$status"
fi

# Who the server says this connection is: answered only where the identity's grant holds the
# request, which the in-cluster listener's static grant does not.
request user_info - "\$SYS.REQ.USER.INFO" ""

request stream_info - "\$JS.API.STREAM.INFO.$stream" ""

offset=0
while :; do
  request consumers "$offset" "\$JS.API.CONSUMER.LIST.$stream" "{\"offset\":$offset}"
  offset=$(next_page .consumers)
  [ "$offset" = "done" ] && break
done

: >"$work/subject-pages"
for family in \
  'notifications.dispatch.issue.>' \
  'notifications.agent.>' \
  'notifications.envoy.exceptions.notifications.agent.>' \
  'notifications.envoy.kept.>'; do
  offset=0
  while :; do
    request subjects "$family $offset" "\$JS.API.STREAM.INFO.$stream" "{\"subjects_filter\":\"$family\",\"offset\":$offset}"
    jq -c . "$work/out" >>"$work/subject-pages" 2>/dev/null
    offset=$(next_page .state.subjects)
    [ "$offset" = "done" ] && break
  done
done

# The last message of each subject the pages list that is in an old form (FORMS_JQ, the gate's own
# rule), by `last_by_subj`: its subject, stream sequence and time alone, which the gate uses to name
# the newest old-form message and, at a second reading, the old-form subjects published after the
# first reading's last sequence. The reads decide no verdict, since any old-form subject the second
# reading finds shuts it, so they are bounded: at most OLD_LAST_MAX (1000, about 40 s at the Job's
# 250m) are read, and one line counts the rest, so a stream full of old-form subjects cannot push
# the Job past its deadline. A subject whose messages expired since the list answers no message
# found. An answer that is no JSON object is printed as no answer, never as it came.
: >"$work/old-subjects"
if [ -n "${FORMS_JQ:-}" ]; then
  printf '%s\n' "$FORMS_JQ" >"$work/legacy-subjects-gate-forms.jq"
  jq -r -L "$work" 'include "legacy-subjects-gate-forms"; try (.state.subjects | objects | keys[] | select(old_form)) catch empty' \
    "$work/subject-pages" 2>/dev/null | sort -u >"$work/old-subjects"
fi
old_last_max=${OLD_LAST_MAX:-1000}
head -n "$old_last_max" "$work/old-subjects" >"$work/old-subjects-read"
while IFS= read -r subject; do
  n req --raw "\$JS.API.STREAM.MSG.GET.$stream" "$(jq -cn --arg s "$subject" '{last_by_subj: $s}')" >"$work/msg" 2>"$work/err"
  status=$?
  jq -c 'if type == "object" then {type, error, message: (if (.message | type) == "object" then {subject: .message.subject, seq: .message.seq, time: .message.time} else null end)} else error("no object") end' \
    "$work/msg" >"$work/out" 2>/dev/null || : >"$work/out"
  emit old_last "$subject" "$status"
done <"$work/old-subjects-read"
printf '%s' "$(($(wc -l <"$work/old-subjects") - $(wc -l <"$work/old-subjects-read")))" >"$work/out"
: >"$work/err"
emit old_last_skipped "$old_last_max" 0

n kv ls "$bucket" >"$work/out" 2>"$work/err"
status=$?
emit interest_keys - "$status"
if [ "$status" = 0 ]; then
  cp "$work/out" "$work/keys"
  while IFS= read -r key; do
    # A NATS key is made of these characters alone; a line of any other is not a key the bucket
    # listed, and is printed as unread so the gate counts it, never left out.
    case "$key" in
      '') continue ;;
      *[!A-Za-z0-9_=./-]*)
        printf '%s' "$key" >"$work/out"
        : >"$work/err"
        emit interest_unread - 1
        continue
        ;;
    esac
    n kv get "$bucket" "$key" --raw >"$work/record" 2>"$work/err"
    status=$?
    # Only what the gate reads of a record: its session id and its topics. A record that is no
    # JSON object is printed as no answer, which the gate reads as no interest record.
    if [ "$status" = 0 ]; then
      jq -c 'if type == "object" then {session_id, topics} else error("the record is no JSON object") end' "$work/record" >"$work/out" 2>>"$work/err" || : >"$work/out"
    else
      : >"$work/out"
    fi
    emit interest "$key" "$status"
  done <"$work/keys"
fi

printf 'end\t-\t0\t\t\n'
