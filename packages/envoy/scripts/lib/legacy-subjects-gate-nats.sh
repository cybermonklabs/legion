# shellcheck shell=sh
# packages/envoy/scripts/lib/legacy-subjects-gate-nats.sh
#
# The NATS reads of legacy-subjects-gate.sh, run by the one-off Job that gate creates in the
# cluster: the gate hands this file to the Job's container as its `sh -c` script, so it runs in the
# nats-box image (busybox sh, the nats CLI, nsc, jq and base64), never on the box that runs the
# gate. It connects as the in-cluster listener does: NATS_URL is the listener's NATS_URLS, and the
# credential is either NATS_NKEY, the listener's seed file (NATS_NKEY_SEED_FILE), which the nats CLI
# reads itself, or NATS_TOKEN_FILE, the listener's projected NATS token, which this script reads
# into NATS_TOKEN for the CLI. NATS_INBOX_PREFIX, when set, is passed to every nats call as
# --inbox-prefix, for a grant that subscribes only its identity's own inbox prefix. It makes read
# requests only: who the server says the connection is, the stream's info, the stream's consumer
# list, the stream's subjects under each family the rename moved, the interest bucket's keys and
# each key's record. It judges nothing; the gate does.
#
# Each read is printed as one line of five tab-separated fields: the read's name, its argument,
# the nats CLI's exit status, and its standard output and standard error, each in base64 on one
# line. The nats CLI answers a request the server's permissions refuse with status 0, no output and
# the violation on standard error (measured with nats-box 0.19.0, the pinned image), so the status
# alone cannot say a read failed: the gate reads each answer's own type and error. A list read that
# answered nothing is read no further, so a refused list ends at once rather than at the Job's
# deadline. The last line is `end`, so a log cut short is told from one that is whole.
#
# What the log holds is what the gate judges, and of an interest record only its session id and
# its topics: the record's directory, machine and every other field are left out. The credential is
# named by its public part alone: the nkey's public key (nsc derives it in a directory of memory
# the gate's Job mounts at WORK_DIR, deleted with the pod), or the token's subject, audience,
# issuer and expiry, never the token.
#
# The JetStream API is called by its subjects, `$JS.API.CONSUMER.LIST.<stream>` and
# `$JS.API.STREAM.INFO.<stream>`, because those are what the listener's grant holds
# (natsgrant.Listener: sessionConsumerCalls and listenerGrant); `nats consumer ls` asks
# `$JS.API.CONSUMER.NAMES.<stream>`, which it does not. A list longer than one answer is read page
# by page, each page starting at the last item of the page before (the server sorts both lists by
# name), so the gate can see that the list did not shift between two pages.
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

# What the credential names, read first: a token is the CLI's credential once it is in NATS_TOKEN.
: >"$work/err"
if [ -n "${NATS_TOKEN_FILE:-}" ]; then
  if NATS_TOKEN=$(cat "$NATS_TOKEN_FILE" 2>"$work/err") && [ -n "$NATS_TOKEN" ]; then
    export NATS_TOKEN
    payload=$(printf '%s' "$NATS_TOKEN" | cut -d. -f2 | tr '_-' '/+')
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
    NKEYS_PATH="$work/nkeys" NSC_HOME="$work/nsc" HOME="$work" nsc import keys --dir "$work/key" >"$work/nsc.out" 2>&1
  status=$?
  grep -oE 'U[A-Z2-7]{55}' "$work/nsc.out" 2>/dev/null | head -n 1 >"$work/out"
  [ "$status" = 0 ] || cat "$work/nsc.out" >>"$work/err" 2>/dev/null
  rm -rf "$work/key" "$work/nkeys" "$work/nsc"
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

for family in \
  'notifications.dispatch.issue.>' \
  'notifications.agent.>' \
  'notifications.envoy.exceptions.notifications.agent.>' \
  'notifications.envoy.kept.>'; do
  offset=0
  while :; do
    request subjects "$family $offset" "\$JS.API.STREAM.INFO.$stream" "{\"subjects_filter\":\"$family\",\"offset\":$offset}"
    offset=$(next_page .state.subjects)
    [ "$offset" = "done" ] && break
  done
done

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
