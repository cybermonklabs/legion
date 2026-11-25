# shellcheck shell=bash
# shellcheck disable=SC2154,SC2034 # restricted-sweep.sh sources this file: it sets what this reads, and reads what this sets
# The restricted sweep's two sockets (restricted-sweep.sh sources this file, beside it), read as
# the member: the document socket's room of the project's document, and the live socket's event
# feed over the live window. It defines functions alone; they read the entry script's fills,
# `mode`, `live_seconds`, `member_session`, `member_issue`, `write_issue` and the run's files, and
# set `window_line`, which a runner's verdict names.

# doc_socket: the member is not admitted to the room of the project's document, while the same
# probe admits the member to a document of a project the member sees (`own`, read with the
# lookup), the control. The member's answer is read for rows before anything else of it, so a
# refusal that holds a row of the project FAILs whatever else the run saw, a failed probe
# included. A line names each probe's answer by its first words alone (`http 404`, `denied ...`,
# `closed 1011`), never the body a refusal holds: the log of a run on a public runner is public.
doc_socket() {
  local schema_version own own_doc='' own_exit answer_exit own_answer answer first marker
  call restricted /api/v1/schema/blocks "$work/body"
  schema_version=$(first_of "$work/body" '.version // .schema_version | tostring')
  while read -r own; do
    call restricted "/api/v1/projects/$own/artifacts" "$work/body"
    [ "$status" != 200 ] || own_doc=$(first_of "$work/body" '.[]? | .id')
    [ -z "$own_doc" ] || break
  done <"$work/own"
  if [ -z "$schema_version" ]; then
    unproven "/ws/doc/{room}: GET /api/v1/schema/blocks names no schema version"
    return
  elif [ -z "$doc" ]; then
    unproven "/ws/doc/{room}: the lookup found no document of $project"
    return
  elif [ -z "$own_doc" ]; then
    unproven "/ws/doc/{room}: the member sees no document of a project of theirs, so no control shows the probe admits them anywhere"
    return
  fi
  bun "$socket_probe" doc "$origin" "$work/restricted.h" "$own_doc" "$schema_version" >"$work/doc-own" 2>&1
  own_exit=$?
  bun "$socket_probe" doc "$origin" "$work/restricted.h" "$doc" "$schema_version" >"$work/doc-target" 2>&1
  answer_exit=$?
  own_answer=$(frames_of "$work/doc-own")
  answer=$(frames_of "$work/doc-target")
  first=$(head -n 1 "$work/doc-target")
  case $first in
    'authenticated '*)
      fail "/ws/doc/{room}: the member is admitted to the room of $project's document: $answer"
      return
      ;;
  esac
  refusal_body "$work/doc-target" "$work/doc-body"
  masked_marker "$work/doc-body" "$doc" || unknown 'could not mask the document refusal'
  if [ -n "$marker" ]; then
    fail "/ws/doc/{room}: the refusal holds $(shown "$marker"), a row of $project"
  elif [ "$own_exit" != 0 ] || [ "$answer_exit" != 0 ]; then
    unproven "/ws/doc/{room}: a probe failed (control $own_exit, member $answer_exit): $own_answer; $answer"
  else
    case $(head -n 1 "$work/doc-own") in
      'authenticated '*)
        case $first in
          'denied '* | 'http 403 '* | 'http 404 '*) printf 'ok      /ws/doc/{room}: %s (control: admitted to their own document)\n' "$answer" ;;
          *) unproven "/ws/doc/{room}: the member's answer for $project's document is no admission and no refusal: $answer" ;;
        esac
        ;;
      *) unproven "/ws/doc/{room}: the control did not hold: the member is not admitted to their own document: $own_answer" ;;
    esac
  fi
}

# live_writes: one run's writes while the feeds are open, before the window: the full member's
# comment on SWEEP_WRITE_ISSUE and direct message to dm_session, a session of the project alone,
# which the full member's own feed must be sent and the member's must not. Each failed write ends
# the check at once rather than waiting out a window that would prove nothing.
live_writes() {
  post_comment
  written=$posted
  if [ -z "$written" ]; then
    live_result="the full member's comment on $write_issue answered $status: $(head_of "$work/body")"
    return
  fi
  post_message full "$dm_session"
  written_message=$posted
  [ -n "$written_message" ] ||
    live_result="the full member's direct message to $dm_session answered $status: $(head_of "$work/body")"
}

# live_socket: the member's event feed, open for the whole of SWEEP_LIVE_SECONDS, is sent no event
# of the project while the project is written to, and is sent the member's own writes of the
# window (member_writes): the member's feed's own control, without which its silence proves
# nothing. One run writes the project's comment and direct message itself, with the full member's
# feed open over the same window, which must be sent both (its control). A runner
# holds no full member's credential: it reads the member's feed alone and prints the window it
# read (`live window <W0> <W1>`, UTC, whole seconds), and its silence is proven only by a
# writer's write delivered strictly inside that window, begun after W0 and ended before W1. W0 is
# read once the member's feed subscribed, so a write begun after it was committed after the feed
# subscribed. W1 leaves the writer's delivery bound (delivery_seconds) before the window's end, so
# a write the writer saw delivered inside the window reached the feeds while the member's was
# open; the entry script refuses a runner a window that, past that bound, holds fewer than two of
# the writer's writes (runner_live_floor). A refused upgrade is named by its status alone.
live_socket() {
  local watcher_restricted watcher_full='' ready_started observation_started window_start elapsed
  local restricted_exit full_exit feed pid refused_upgrade marker first_line frames own_message='' own_comment=''
  live_result='' written='' written_message='' window_line=''
  bun "$socket_probe" watch "$origin" "$work/restricted.h" "$work/ready-restricted" "$work/written" \
    "$live_seconds" >"$work/live-restricted" 2>&1 &
  watcher_restricted=$!
  watchers=$watcher_restricted
  if [ "$mode" = one ]; then
    bun "$socket_probe" watch "$origin" "$work/full.h" "$work/ready-full" "$work/written" \
      "$live_seconds" >"$work/live-full" 2>&1 &
    watcher_full=$!
    watchers="$watcher_restricted $watcher_full"
  fi
  ready_started=$SECONDS
  until { [ -e "$work/ready-restricted" ] || ! alive "$watcher_restricted"; } &&
    { [ "$mode" != one ] || [ -e "$work/ready-full" ] || ! alive "$watcher_full"; }; do
    [ "$((SECONDS - ready_started))" -lt 30 ] || break
    sleep 0.2
  done
  if [ "$mode" = one ] && { [ ! -e "$work/ready-full" ] || ! alive "$watcher_full"; }; then
    live_result="the full member's feed did not stay subscribed: it was sent $(frames_of "$work/live-full")"
  elif grep -q '^closed refused ' "$work/live-restricted"; then
    live_result="the member's events subscription was refused: $(frames_of "$work/live-restricted")"
  elif [ ! -e "$work/ready-restricted" ] && alive "$watcher_restricted"; then
    live_result="the member's feed did not start in 30 s"
  elif [ -e "$work/ready-restricted" ] && ! alive "$watcher_restricted"; then
    live_result="the member's feed ended before the observation window started"
  elif [ ! -e "$work/ready-restricted" ] && ! grep -E -q '^http (403|404) ' "$work/live-restricted"; then
    live_result="the member's probe ended without subscribing or refusing the upgrade: $(frames_of "$work/live-restricted")"
  elif [ "$mode" = one ]; then
    live_writes
  fi
  observation_started=$SECONDS
  window_start=$(date -u +%s)
  if [ -z "$live_result" ]; then
    : >"$work/written" || unknown 'could not signal the socket observation window'
    # The member's own writes, inside the window, to a feed that subscribed; a refused upgrade has
    # no feed to send them to.
    [ ! -e "$work/ready-restricted" ] || member_writes
  fi
  if [ -z "$live_result" ]; then
    while alive "$watcher_restricted" || alive "$watcher_full"; do
      elapsed=$((SECONDS - observation_started))
      if [ "$elapsed" -lt "$live_seconds" ] &&
        { { [ "$mode" = one ] && ! alive "$watcher_full"; } ||
          { [ -e "$work/ready-restricted" ] && ! alive "$watcher_restricted"; }; }; then
        live_result='a subscribed feed ended before the observation window finished'
        break
      fi
      if [ "$elapsed" -ge "$((live_seconds + 15))" ]; then
        live_result='a socket probe did not finish after the observation window'
        break
      fi
      sleep 0.2
    done
    if [ -z "$live_result" ] && [ "$((SECONDS - observation_started))" -lt "$live_seconds" ]; then
      live_result='the feeds did not cover the observation window'
    fi
  fi
  if [ -n "$live_result" ]; then
    for pid in $watchers; do kill "$pid" 2>/dev/null; done
  fi
  wait "$watcher_restricted"
  restricted_exit=$?
  full_exit=0
  if [ -n "$watcher_full" ]; then
    wait "$watcher_full"
    full_exit=$?
  fi
  watchers=''
  # The probe prints `closed <code> <reason>` for a feed that closed before its window ended, and
  # `closed ended <reason>` for a subscription the server ended on an open socket (it ends one
  # whose read failed, and the page subscribes again), either of which the whole-second clock above
  # can miss by a second: a subscribed feed that printed it did not cover the window. An ended
  # subscription is named, whatever else the run saw of it.
  for feed in restricted full; do
    if grep -q '^closed ended ' "$work/live-$feed" 2>/dev/null; then
      live_result="the server ended the $([ "$feed" = full ] && printf "full member's" || printf "member's") events subscription, its socket left open: $(grep -m 1 '^closed ended ' "$work/live-$feed")"
    elif [ -z "$live_result" ] && [ -e "$work/ready-$feed" ] && grep -q '^closed ' "$work/live-$feed"; then
      live_result='a subscribed feed ended before the observation window finished'
    fi
  done
  for marker in "$written" "$written_message"; do [ -z "$marker" ] || printf '%s\n' "$marker" >>"$work/values"; done
  write_markers
  # A refused upgrade is judged by the body the server wrote, without the probe's status prefix.
  refused_upgrade=''
  case $(head -n 1 "$work/live-restricted") in 'http '[0-9][0-9][0-9]' '*) refused_upgrade=1 ;; esac
  refusal_body "$work/live-restricted" "$work/live-body"
  marker=$(marker_in "$work/live-body")
  first_line=$(head -n 1 "$work/live-restricted")
  if [ -n "$marker" ] && [ -n "$refused_upgrade" ]; then
    fail "/ws/live: the member's refused upgrade holds $(shown "$marker"), a row of $project"
  elif [ -n "$marker" ]; then
    fail "/ws/live: the member is sent an event holding $(shown "$marker"), of $project"
  elif [ -n "$live_result" ]; then
    unproven "/ws/live: $live_result"
  elif [ "$mode" = one ] && { [ "$full_exit" != 0 ] || ! jq -s -e --arg c "$written" --arg m "$written_message" '
    length > 0 and .[0].type == "events_start"
    and all(.[]; type == "object" and (.type | type == "string"))
    and any(.[]; .type == "event" and any(.. | strings; . == $c))
    and any(.[]; .type == "event" and any(.. | strings; . == $m))' "$work/live-full" >/dev/null 2>&1; }; then
    unproven "/ws/live: the control did not hold: the full member's feed did not cover $live_seconds s and receive comment $written and direct message $written_message: it was sent $(frames_of "$work/live-full")"
  elif [ "$restricted_exit" != 0 ]; then
    unproven "/ws/live: the member's feed gave no answer: $(frames_of "$work/live-restricted")"
  else
    case $first_line in
      'http 403 '* | 'http 404 '*) printf 'ok      /ws/live: the member'"'"'s upgrade is refused: %s\n' "$(frames_of "$work/live-restricted")" ;;
      'http '*) unproven "/ws/live: the member's upgrade is answered $(frames_of "$work/live-restricted")" ;;
      *)
        if [ ! -e "$work/ready-restricted" ] || ! jq -s -e '
          length > 0 and .[0].type == "events_start"
          and all(.[]; type == "object" and (.type | type == "string"))' "$work/live-restricted" >/dev/null 2>&1; then
          unproven "/ws/live: the member's feed did not cover the observation window: $(frames_of "$work/live-restricted")"
        elif [ -n "$(own_unsent)" ]; then
          unproven "/ws/live: the control did not hold: the member was not sent $(own_unsent)"
        else
          frames=$(jq -s '[.[] | select(.type == "event")] | length' "$work/live-restricted")
          if [ "$mode" = one ]; then
            printf 'ok      /ws/live: the member is sent %s events in %s s, none of %s; the full member is sent the comment and the direct message, and the member %s (controls)\n' \
              "$frames" "$live_seconds" "$project" "$(own_writes)"
          else
            window_line="$(utc_of "$window_start") $(utc_of "$((window_start + live_seconds - delivery_seconds))")"
            printf 'ok      /ws/live: the member is sent %s events in %s s, none of %s, and %s; its control is a writer'"'"'s write delivered inside the live window\n' \
              "$frames" "$live_seconds" "$project" "$(own_writes)"
            printf 'live window %s\n' "$window_line"
          fi
        fi
        ;;
    esac
  fi
  [ -z "$own_message" ] || own_message_read "$own_message"
  [ -z "$written_message" ] || window_message_reads "$written_message"
}

# member_writes: the member's own writes of the live window, to a feed that subscribed: a direct
# message to SWEEP_MEMBER_SESSION, or, in its place, a comment and then a message on
# SWEEP_MEMBER_ISSUE, each a body alone, which a person who holds no session writes. It sets
# live_socket's own_message, and own_comment, to the ids they were answered; a write that answers
# none sets live_result, naming its status alone, and is the last.
member_writes() {
  if [ -z "$member_issue" ]; then
    post_message restricted "$member_session"
    own_message=$posted
    [ -n "$own_message" ] ||
      live_result="the member's direct message to their own session $member_session answered $status"
    return
  fi
  member_issue_write comments comment
  own_comment=$posted
  if [ -z "$own_comment" ]; then
    live_result="the member's comment on their own issue $member_issue answered $status"
    return
  fi
  member_issue_write messages message
  own_message=$posted
  [ -n "$own_message" ] ||
    live_result="the member's message on their own issue $member_issue answered $status"
}

# member_issue_write ROUTE WHAT: the member's WHAT (comment, message) on SWEEP_MEMBER_ISSUE, a body
# alone that says it is the sweep's probe and when it was written, POSTed to the issue's ROUTE
# (comments, messages), as post_as answers it.
member_issue_write() {
  jq -cn --arg b "restricted-sweep: a probe of whether the member is sent their own $2 live, written $(date -u +%Y-%m-%dT%H:%M:%SZ)." \
    '{body: $b}' >"$work/member-$2.json" || unknown "could not write the member's $2 request"
  post_as restricted "/api/v1/issues/$member_issue/$1" "$work/member-$2.json"
}

# own_writes: the member's own writes of the live window, as a line names them.
own_writes() {
  if [ -z "$member_issue" ]; then
    printf 'their own direct message'
  else
    printf 'their own comment and message on %s' "$member_issue"
  fi
}

# own_unsent: what of the member's own writes their feed was not sent, as a line names it, or
# nothing when it was sent every one; a write answered no id was sent nothing.
own_unsent() {
  local comment=unsent message=unsent
  [ -z "$own_message" ] || ! grep -F -q -e "$own_message" "$work/live-restricted" || message=''
  if [ -z "$member_issue" ]; then
    [ -z "$message" ] || printf 'their own direct message to %s' "$member_session"
    return 0
  fi
  [ -z "$own_comment" ] || ! grep -F -q -e "$own_comment" "$work/live-restricted" || comment=''
  if [ -n "$comment" ] && [ -n "$message" ]; then
    own_writes
  elif [ -n "$comment" ]; then
    printf 'their own comment on %s' "$member_issue"
  elif [ -n "$message" ]; then
    printf 'their own message on %s' "$member_issue"
  fi
}

# own_message_read ID: the member's own message, read by GET /api/v1/messages/{id} as the member:
# the control that the route answers the member a message they may read, beside the rows that must
# answer the project's session's direct message as missing. Those rows are held to a direct
# conversation's rule, its parties (access.PartyClause), which reads the member's own direct
# message to SWEEP_MEMBER_SESSION too; a message on SWEEP_MEMBER_ISSUE is read by its issue's
# project rule (access.MessageClause) instead, so in that mode the read shows that the route
# answers the member, and not that the party rule does.
own_message_read() {
  local what='own direct message'
  [ -z "$member_issue" ] || what="own message on $member_issue"
  call restricted "/api/v1/messages/$(uri "$1")" "$work/body"
  if [ "$status" = 200 ] && grep -F -q -e "$1" "$work/body"; then
    printf 'ok      GET /api/v1/messages/{id} (the member'"'"'s %s): 200 (control)\n' "$what"
  else
    unproven "GET /api/v1/messages/{id} (the member's $what): the control did not hold: the member is answered $status for their $what"
  fi
}

# window_message_reads ID: one run's direct message of the live window, to the project's session,
# by the routes that read one message and by its session's list, each read once the window ended.
window_message_reads() {
  local suffix
  for suffix in '' /blocks; do
    row filled "GET /api/v1/messages/{id}$suffix (the live window's direct message)" \
      "/api/v1/messages/$(uri "$1")$suffix" "/api/v1/messages/$none_uuid$suffix" 1 "$1" "$none_uuid"
  done
  row filled "GET /api/v1/agents/{session_id}/messages (after the live window)" \
    "/api/v1/agents/$(uri "$dm_session")/messages" "/api/v1/agents/ses-nosuchsession/messages" 0 \
    "$dm_session" ses-nosuchsession
}
