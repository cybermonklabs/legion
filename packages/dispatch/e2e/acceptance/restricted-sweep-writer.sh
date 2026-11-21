# shellcheck shell=bash
# shellcheck disable=SC2154,SC2034 # restricted-sweep.sh sources this file: it sets what this reads, and reads what this sets
# The restricted sweep's writer mode (restricted-sweep.sh sources this file, beside it), beside a
# runner's run: the full member's own feed open, and every SWEEP_WRITE_EVERY s for
# SWEEP_WRITE_SECONDS one write, a comment on SWEEP_WRITE_ISSUE and a direct message that names no
# issue to the fills' session of the project (dm_session), each held to that feed. It defines
# run_writer alone, which ends the run.

# run_writer: one line a write, as soon as whether the full member's feed was sent both of its
# halves is known, so a writer stopped early has printed every write but the one in flight:
# `write <UTC> <comment id> <message id> delivered`, or `... not delivered` where the feed was
# sent either half alone or neither within the wait (SWEEP_WRITE_EVERY, at least 5 s and at most
# the delivery bound a runner's window leaves, delivery_seconds), or `write <UTC> - not written`
# where the server took neither. A write's time is read once the server answered both halves, so
# both were committed by then. Exit 0 only when every write was delivered.
run_writer() {
  local watcher_full ready_started writes=0 missed=0 wait_each next stamp comment_id message_id
  local outcome until_at write_started
  call full /api/v1/whoami "$work/body"
  [ "$status" = 200 ] || unknown "GET /api/v1/whoami with SWEEP_FULL_TOKEN answered $status: $(head_of "$work/body")"
  bun "$socket_probe" watch "$origin" "$work/full.h" "$work/ready-full" "$work/written" \
    "$((write_seconds + write_every + 15))" >"$work/live-full" 2>&1 &
  watcher_full=$!
  watchers=$watcher_full
  ready_started=$SECONDS
  until [ -e "$work/ready-full" ] || ! alive "$watcher_full"; do
    [ "$((SECONDS - ready_started))" -lt 30 ] || break
    sleep 0.2
  done
  if [ ! -e "$work/ready-full" ] || ! alive "$watcher_full"; then
    unknown "the full member's feed did not subscribe in 30 s: it was sent $(frames_of "$work/live-full")"
  fi
  : >"$work/written" || unknown 'could not signal the socket observation window'
  wait_each=$((write_every > 5 ? write_every : 5))
  wait_each=$((wait_each < delivery_seconds ? wait_each : delivery_seconds))
  printf 'writer  a comment on %s and a direct message to %s every %s s for %s s, each held to the full member'"'"'s own feed for %s s\n' \
    "$write_issue" "$dm_session" "$write_every" "$write_seconds" "$wait_each"
  write_started=$SECONDS
  while [ "$((SECONDS - write_started))" -lt "$write_seconds" ]; do
    next=$((SECONDS + write_every))
    comment_request "$work/comment.json"
    post_as full "/api/v1/issues/$write_issue/comments" "$work/comment.json"
    comment_id=$posted
    [ -n "$comment_id" ] || printf 'writer  the comment on %s answered %s: %s\n' "$write_issue" "$status" "$(head_of "$work/body" 120)"
    message_request "$work/message.json"
    post_as "$full_browser" "/api/v1/agents/$(uri "$dm_session")/messages" "$work/message.json"
    message_id=$posted
    [ -n "$message_id" ] || printf 'writer  the direct message to %s answered %s: %s\n' "$dm_session" "$status" "$(head_of "$work/body" 120)"
    stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    writes=$((writes + 1))
    if [ -z "$comment_id" ] && [ -z "$message_id" ]; then
      printf 'write %s - not written\n' "$stamp"
      missed=$((missed + 1))
    else
      outcome='not delivered'
      until_at=$((SECONDS + wait_each))
      while [ -n "$comment_id" ] && [ -n "$message_id" ]; do
        if grep -F -q -e "$comment_id" "$work/live-full" && grep -F -q -e "$message_id" "$work/live-full"; then
          outcome=delivered
          break
        fi
        if [ "$SECONDS" -ge "$until_at" ] || ! alive "$watcher_full"; then break; fi
        sleep 0.2
      done
      printf 'write %s %s %s %s\n' "$stamp" "${comment_id:--}" "${message_id:--}" "$outcome"
      [ "$outcome" = delivered ] || missed=$((missed + 1))
    fi
    if ! alive "$watcher_full"; then
      printf 'writer  the full member'"'"'s feed ended: it was sent %s\n' "$(frames_of "$work/live-full")"
      missed=$((missed + 1))
      break
    fi
    while [ "$SECONDS" -lt "$next" ] && [ "$((SECONDS - write_started))" -lt "$write_seconds" ]; do sleep 0.2; done
  done
  kill "$watcher_full" 2>/dev/null
  wait "$watcher_full" 2>/dev/null
  watchers=''
  if [ "$missed" -gt 0 ] || [ "$writes" -eq 0 ]; then
    printf 'WRITER: %s writes, %s not delivered to the full member'"'"'s own feed\n' "$writes" "$missed"
    exit 1
  fi
  printf 'WRITER: %s writes, each delivered to the full member'"'"'s own feed\n' "$writes"
  exit 0
}
