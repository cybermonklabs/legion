# shellcheck shell=bash
# The run's RPC session (launch_rpc), which rig.sh and rig.test.ts's rpcRun both source: the
# label's Oh My Pi in RPC mode, the prompts the rig sends it, and the facts its run ends on.
# rpc-sent.jsonl holds each command sent and rpc-events.jsonl each frame Oh My Pi printed, each line
# `{at, ms, sent|got}`, `ms` its epoch milliseconds, and rpc.jsonl both in time order once the
# session ends.
#
# What it takes from the script that sources it: R, the run's directory; pin, the label's Oh My Pi;
# omp_pid and rpc_fd, which launch_rpc sets and rpc_end clears; rig.sh's fail and rig_omp; and
# scripts/e2e/lib/rig.sh's start_process and stop_tree. A test sets rpc_poll and rpc_ready_bound
# again after it sources this file.

# launch_rpc's clock: seconds between two looks at a run's end, and the bound on the claim's
# claims/ready.
rpc_poll=10
rpc_ready_bound=300

# rpc_now prints the time now as `<UTC instant, microseconds> <epoch milliseconds>`.
rpc_now() {
  local t=$EPOCHREALTIME
  local s=${t%.*} u=${t#*.}
  printf '%(%Y-%m-%dT%H:%M:%S)T.%sZ %s\n' "$s" "$u" "$s${u:0:3}"
}

# rpc_send COMMAND writes one RPC command to the session's stdin and records it.
rpc_send() {
  local now
  now=$(rpc_now)
  printf '%s\n' "$1" >&"$rpc_fd"
  printf '{"at":"%s","ms":%s,"sent":%s}\n' "${now% *}" "${now#* }" "$1" >>"$R/rpc-sent.jsonl"
}

# rpc_answer ID FIELD prints FIELD of the session's response to the command ID, waiting up to 30 s,
# or nothing when none came.
rpc_answer() {
  local i answer
  for ((i = 0; i < 120; i++)); do
    answer=$(jq -rs --arg id "$1" --arg field "$2" \
      '[.[] | .got | objects | select(.type == "response" and .id == $id)] | last
       | if . == null then "" else (getpath($field | split(".")) | tostring) end' "$R/rpc-events.jsonl")
    [ -z "$answer" ] || break
    sleep 0.25
  done
  printf '%s\n' "$answer"
}

# rpc_state prints get_state's isStreaming, true or false, or `unanswered`.
rpc_state() {
  local id=state-${EPOCHREALTIME/./} answer
  rpc_send "$(jq -cn --arg id "$id" '{type: "get_state", id: $id}')"
  answer=$(rpc_answer "$id" data.isStreaming)
  printf '%s\n' "${answer:-unanswered}"
}

# rpc_ms is the jq definition of an instant's epoch milliseconds, for the stand-in's `at`.
rpc_ms='def ms: capture("^(?<s>[^.Z]+)(\\.(?<f>[0-9]+))?Z$") | ((.s + "Z") | fromdateiso8601) * 1000 + ((.f // "") + "000" | .[0:3] | tonumber);'

# rpc_turn SINCE prints the session's turn as `<state> <its agent_end's ms> <its agent_start's ms>`:
# none (no turn yet), running (agent_start last), ended (agent_end last, and both at or after
# SINCE, epoch milliseconds: the turn ran after SINCE), or before (its last turn ran before SINCE).
rpc_turn() {
  jq -rs --argjson since "$1" '
    [.[] | select((.got | type) == "object" and (.got.type == "agent_start" or .got.type == "agent_end"))] as $turns
    | ($turns | last) as $last | ([$turns[] | select(.got.type == "agent_start")] | last) as $start
    | if $last == null then "none 0 0"
      elif $last.got.type == "agent_start" then "running 0 \($start.ms)"
      elif $last.ms >= $since and $start.ms >= $since then "ended \($last.ms) \($start.ms)"
      else "before \($last.ms) \($start.ms // 0)" end' "$R/rpc-events.jsonl"
}

# rpc_activity AFTER prints the session's newest claims/activity report in the stand-in's log at or
# after AFTER (epoch milliseconds) as `<running> <nextReminderAt or null> <kinds, comma-separated>`,
# or `none`.
rpc_activity() {
  [ -f "$R/daemon.log" ] || { echo none && return 0; }
  jq -rRs --argjson after "$1" "$rpc_ms"'
    [split("\n")[] | select(. != "") | fromjson? | objects
     | select(.path == "/legion/v1/claims/activity" and (.body | type) == "object" and (.at | ms) >= $after)] | last
    | if . == null then "none"
      else "\(.body.backgroundWork.running) \(.body.phase.nextReminderAt // "null") \((.body.backgroundWork.kinds // []) | join(", "))" end' \
    "$R/daemon.log"
}

# rpc_settled SINCE succeeds when the session's run has ended on these facts:
# its last turn began and ended after SINCE, get_state answers that no turn runs, and the session's
# newest activity report since that turn began says no background work runs and names no reminder.
# The plugin sends that report at session_stop, just before the run's last agent_end, so the
# report is read from the turn's start, not its end.
rpc_settled() {
  local state started activity
  read -r state _ started < <(rpc_turn "$1")
  [ "$state" = ended ] || return 1
  [ "$(rpc_state)" = false ] || return 1
  activity=$(rpc_activity "$started")
  [[ $activity == "false null"* ]]
}

# rpc_ready waits, bounded (rpc_ready_bound), for the stand-in's log to show the claim's
# claims/ready, and fails when Oh My Pi exits first.
rpc_ready() {
  local i
  for ((i = 0; i < rpc_ready_bound * 2; i++)); do
    [ -f "$R/daemon.log" ] &&
      jq -e -s 'any(.[]; .path == "/legion/v1/claims/ready" and .status < 300)' "$R/daemon.log" >/dev/null 2>&1 && return 0
    kill -0 "$omp_pid" 2>/dev/null || fail "Oh My Pi exited before its claim was ready; see $R/omp-stderr.txt"
    sleep 0.5
  done
  fail "the session's claim was not ready within ${rpc_ready_bound}s: $R/daemon.log holds no claims/ready; see $R/omp-stderr.txt"
}

# rpc_prompt FILE DEADLINE sends FILE's text, unchanged, as one RPC prompt. A prompt Oh My Pi
# refuses while a turn runs is sent again once get_state answers that none runs; past DEADLINE
# (bash's SECONDS) it fails.
rpc_prompt() {
  local id answer
  while :; do
    id=prompt-${EPOCHREALTIME/./}
    rpc_send "$(jq -cn --arg id "$id" --rawfile message "$1" '{type: "prompt", id: $id, message: $message}')"
    answer=$(rpc_answer "$id" success)
    case $answer in
    true) return 0 ;;
    false) ;;
    *) fail "Oh My Pi did not answer the prompt of $1 within 30 s; see $R/rpc-events.jsonl" ;;
    esac
    while [ "$(rpc_state)" != false ]; do
      [ "$SECONDS" -lt "$2" ] || return 1
      sleep "$rpc_poll"
    done
  done
}

# rpc_wait SINCE DEADLINE waits, a look every rpc_poll seconds, until the run has ended
# (rpc_settled SINCE), and fails past DEADLINE (bash's SECONDS), or when Oh My Pi exits first.
rpc_wait() {
  while :; do
    rpc_settled "$1" && return 0
    kill -0 "$omp_pid" 2>/dev/null || fail "Oh My Pi exited before the run ended; see $R/omp-stderr.txt"
    [ "$SECONDS" -lt "$2" ] || return 1
    sleep "$rpc_poll"
  done
}

# rpc_reminders prints how many reminders of Legion's the run's transcripts hold
# (`legion-phase-reminder` messages).
rpc_reminders() {
  find "$R/sessions" -name '*.jsonl' -type f -exec cat {} + 2>/dev/null |
    jq -R 'fromjson? | objects | select(.customType == "legion-phase-reminder")' | jq -s length
}

# rpc_bound BOUND SINCE ends a run at its bound: held.txt says what held it (the turn, the
# background work, or the reminder and the count of reminders), and out.txt ends `exit=bound`.
rpc_bound() {
  local state started activity running at kinds reason
  read -r state _ started < <(rpc_turn "$2")
  activity=none
  [ "$state" = none ] || activity=$(rpc_activity "$started")
  read -r running at kinds <<<"$activity"
  if [ "$state" = running ] || [ "$(rpc_state)" = true ]; then
    reason="a turn running"
  elif [ "$running" = true ]; then
    reason="background work running: ${kinds:-of no kind it names}"
  elif [ -n "${at:-}" ] && [ "$at" != null ]; then
    reason="a reminder armed for $at, after $(rpc_reminders) reminders"
  elif [ "$state" = none ] || [ "$state" = before ]; then
    reason="no turn started"
  else
    reason="no activity report of the session since its turn began"
  fi
  printf 'the run did not end within %s seconds: %s\n' "$1" "$reason" >"$R/held.txt"
  echo exit=bound >>"$R/out.txt"
  rpc_end
}

# rpc_end closes the session's stdin, waits up to 15 s for Oh My Pi to exit, stops what is left,
# and writes rpc.jsonl.
rpc_end() {
  local i
  [ -z "$rpc_fd" ] || { exec {rpc_fd}>&- && rpc_fd=''; }
  for ((i = 0; i < 30; i++)); do
    kill -0 "$omp_pid" 2>/dev/null || break
    sleep 0.5
  done
  stop_tree "$omp_pid" || true
  wait "$omp_pid" 2>/dev/null || true
  omp_pid=''
  jq -cs 'sort_by(.ms)[]' "$R/rpc-sent.jsonl" "$R/rpc-events.jsonl" >"$R/rpc.jsonl"
}

# launch_rpc BOUND [START...] -- [THEN...] runs the label's Oh My Pi as the runtime starts a
# pane: `--model @default --mode rpc --append-system-prompt $R/system-prompt.md`, with the run's
# --session-dir, in $R/cwd under $R/pane.env alone. The pane's variables reach Oh My Pi through the
# launch file's environment, never through process arguments: an owner's pane can carry the launch
# answer's NATS URL, including a credential. The file starts itself again with an empty environment,
# exports each pane line, and execs Oh My Pi with the pane's lines as its whole environment. The
# frame loop takes TZ=UTC for itself alone. Once the stand-in's log shows claims/ready, it sends
# each START file's text as one RPC prompt, and then each THEN file's once the turn before it ended.
# A run with no start task waits for a notice turn. Each turn has BOUND seconds. Its last out.txt
# line is `exit=0` once the last turn ended, or `exit=bound` (rpc_bound).
launch_rpc() {
  local bound=$1 starts=() thens=() omp file since launched
  local -x TZ=UTC
  shift
  while [ $# -gt 0 ] && [ "$1" != -- ]; do
    starts+=("$1")
    shift
  done
  [ $# = 0 ] || shift
  thens=("$@")
  omp=$(rig_omp "$pin") || exit 1
  : >"$R/rpc-events.jsonl"
  : >"$R/rpc-sent.jsonl"
  : >"$R/out.txt"
  rm -f "$R/rpc.in"
  # The session's stdin carries every prompt the rig sends; it is its owner's alone, as the pane is.
  mkfifo -m 0600 "$R/rpc.in"
  # The rig's end of the session's stdin. Every child inherits it, so the script closes it first:
  # while any process held it open, rpc_end's close would give Oh My Pi no end of input, and it
  # would be killed rather than exit.
  exec {rpc_fd}<>"$R/rpc.in"
  cat >"$R/launch-rpc.sh" <<EOF
#!/usr/bin/env bash
[ "\${1:-}" = clean ] || exec env -i "\$BASH" "\$0" clean
exec $rpc_fd>&-
cd $(printf %q "$R/cwd") || exit 1
export -n PWD OLDPWD
mapfile -t vars <$(printf %q "$R/pane.env")
for var in "\${vars[@]}"; do export "\$var"; done
{ exec env -u SHLVL $(printf %q "$omp") --model @default --mode rpc --append-system-prompt $(printf %q "$R/system-prompt.md") \
  --session-dir $(printf %q "$R/sessions"); } <$(printf %q "$R/rpc.in") 2>>$(printf %q "$R/omp-stderr.txt") |
  {
    export TZ=UTC
    while IFS= read -r frame || [ -n "\$frame" ]; do
      [ -n "\$frame" ] || continue
      t=\$EPOCHREALTIME s=\${t%.*} u=\${t#*.}
      case \$frame in '{'*) ;; *) frame=\$(jq -Rn --arg f "\$frame" '\$f') ;; esac
      printf '{"at":"%(%Y-%m-%dT%H:%M:%S)T.%sZ","ms":%s,"got":%s}\n' "\$s" "\$u" "\$s\${u:0:3}" "\$frame"
    done
  } >>$(printf %q "$R/rpc-events.jsonl")
EOF
  launched=$(rpc_now)
  launched=${launched#* }
  start_process omp bash "$R/launch-rpc.sh"
  rpc_ready
  if [ ${#starts[@]} = 0 ]; then
    rpc_wait "$launched" $((SECONDS + bound)) || {
      rpc_bound "$bound" "$launched"
      return 0
    }
  fi
  for file in "${starts[@]}" "${thens[@]}"; do
    since=$(rpc_now)
    since=${since#* }
    local deadline=$((SECONDS + bound))
    if ! rpc_prompt "$file" "$deadline" || ! rpc_wait "$since" "$deadline"; then
      rpc_bound "$bound" "$since"
      return 0
    fi
  done
  rpc_end
  echo "exit=0" >>"$R/out.txt"
}
