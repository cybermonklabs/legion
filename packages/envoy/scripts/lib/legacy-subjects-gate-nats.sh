# shellcheck shell=sh
# packages/envoy/scripts/lib/legacy-subjects-gate-nats.sh
#
# The NATS reads of legacy-subjects-gate.sh, run by the one-off Job that gate creates in the
# cluster: the gate hands this file to the Job's container as its `sh -c` script, so it runs in the
# nats-box image (busybox sh, the nats CLI, jq and base64), never on the box that runs the gate. It
# connects as the in-cluster listener does, NATS_URL and NATS_NKEY being the listener's NATS_URLS
# and NATS_NKEY_SEED_FILE, and makes read requests only: the stream's info, the stream's consumer
# list, the stream's subjects under each family the rename moved, the interest bucket's keys and
# each key's record. It judges nothing; the gate does.
#
# Each read is printed as one line of five tab-separated fields: the read's name, its argument,
# the nats CLI's exit status, and its standard output and standard error, each in base64 on one
# line. The nats CLI answers a request the server's permissions refuse with status 0, no output and
# the violation on standard error (measured with nats-box 0.19.0, the pinned image), so the status
# alone cannot say a read failed: the gate reads each answer's own type and error. The last line is
# `end`, so a log cut short is told from one that is whole.
#
# The JetStream API is called by its subjects, `$JS.API.CONSUMER.LIST.<stream>` and
# `$JS.API.STREAM.INFO.<stream>`, because those are what the listener's grant holds
# (natsgrant.Listener: sessionConsumerCalls and listenerGrant); `nats consumer ls` asks
# `$JS.API.CONSUMER.NAMES.<stream>`, which it does not. A list longer than one answer is read page
# by page from the offset each answer gives.
set -u

stream=ENVOY_NOTIFICATIONS
bucket=envoy_interests
work=$(mktemp -d)

# emit LEG ARG STATUS prints one read's line from the output and error the read left in $work.
emit() {
  printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$(base64 -w 0 <"$work/out")" "$(base64 -w 0 <"$work/err")"
}

# request LEG ARG SUBJECT BODY makes one JetStream API request and prints its line.
request() {
  nats req --raw "$3" "$4" >"$work/out" 2>"$work/err"
  emit "$1" "$2" "$?"
}

# next_page prints the offset of the page after the list answer in $work/out, whose items the jq
# path $1 reads (`.consumers`, or `.state.subjects` for a subject list, an object), or `done` when
# that answer was the last page, held no item, or is no list answer at all.
next_page() {
  jq -r "($1) as \$items
    | if (\$items | type) == \"array\" or (\$items | type) == \"object\"
      then (.offset + (\$items | length)) as \$next
        | if (.total | type) == \"number\" and (.offset | type) == \"number\" and (\$items | length) > 0 and \$next < .total
          then \$next else \"done\" end
      else \"done\" end" "$work/out" 2>/dev/null || echo done
}

request stream_info - "\$JS.API.STREAM.INFO.$stream" ""

offset=0
while :; do
  request consumers "$offset" "\$JS.API.CONSUMER.LIST.$stream" "{\"offset\":$offset}"
  offset=$(next_page .consumers)
  [ "$offset" = done ] && break
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
    [ "$offset" = done ] && break
  done
done

nats kv ls "$bucket" >"$work/out" 2>"$work/err"
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
    nats kv get "$bucket" "$key" --raw >"$work/out" 2>"$work/err"
    emit interest "$key" "$?"
  done <"$work/keys"
fi

printf 'end\t-\t0\t\t\n'
