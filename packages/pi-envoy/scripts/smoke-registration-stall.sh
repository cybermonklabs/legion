#!/usr/bin/env bash
# smoke-registration-stall.sh — the live check of the registration-stall fix: a pane whose
# listener record would outlive nothing but its own refreshes keeps that record past the
# listener's live window when one refresh is wedged, and puts it back when it is removed.
#
# No unit test can show a real pane's listener record surviving the listener's own expiry, so this
# drives a real headless `omp` pane, running the packed pi-envoy, against a real Envoy listener and
# watches the live registry. Two arms, each with a negative control that must fail the way the bug
# failed:
#
#   --arm stall    a pane whose registration post is wedged forever is detected, bounded, keeps
#                  refreshing past one live window, and says so exactly once. Its control
#                  SIGKILLs the pane at the wedge, as a pane whose refresh never comes back is: its
#                  record must leave the registry within the live window and a margin and never
#                  return, so the window the arm watches past is one this listener enforces.
#   --arm recover  a pane unregistered while its process runs re-registers itself, with no human
#                  and no write from this script beyond the one delete. Its control SIGKILLs the
#                  pane before the delete, and the record must stay gone.
#
# With no --arm it runs both arms and both controls, one pane each, which is what the
# agent-harness job's plugin-smokes part runs. The stall control shows the window is real, not
# that the fix is what keeps the record: a wedged pass that holds every later tick is what the
# plugin's unit tests hold off (extensions/envoy.test.ts, "a pass stuck on a listener call that
# never answers still lets later ticks refresh the registration" and "a whole stall says so once
# in the pane and once in the log").
#
# It takes the job's environment alone (scripts/e2e/lib/plugin-smoke.sh): Oh My Pi from
# LEGION_TEST_OMP, pi-envoy packed from this checkout, natsd and the Envoy listener of
# packages/envoy-client/test/envoy-stack.ts, and a model stand-in for the pane's one prompt. It prints
# one `CHECK <name>: PASS` or `CHECK <name>: FAIL: <why>` line per check and exits 1 after a FAIL; a
# bad argument is refused with exit 2. See scripts/README.md.
set -euo pipefail

readonly wedge_at=3
readonly poll_s=15
readonly fast_poll_s=5
readonly curl_connect_timeout=5
readonly curl_max_time=20
# How long a single check keeps retrying a list read that failed (the listener's refilling 503
# asks for a retry after 5 s) before it fails for want of a list.
readonly list_read_budget_s=60
# The pane's refresh interval unless --heartbeat-ms names another: short, so a run watches past
# the listener's live window in minutes rather than the deployment's five.
readonly default_refresh_ms=20000

refuse() {
  printf 'smoke-registration-stall: %s\n' "$1" >&2
  exit 2
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir

arm=""
control=0
plugin=""
heartbeat_ms=""
while (($# > 0)); do
  case "$1" in
    --arm)
      arm="${2:-}"
      shift 2 || refuse "--arm needs stall or recover"
      ;;
    --control)
      control=1
      shift
      ;;
    --plugin)
      plugin="${2:-}"
      shift 2 || refuse "--plugin needs a path"
      ;;
    --heartbeat-ms)
      heartbeat_ms="${2:-}"
      shift 2 || refuse "--heartbeat-ms needs a number"
      ;;
    *)
      refuse "unknown argument: $1"
      ;;
  esac
done

[[ -z "$arm" || "$arm" == "stall" || "$arm" == "recover" ]] ||
  refuse "--arm takes stall or recover"
((control == 0)) || [[ -n "$arm" ]] || refuse "--control needs --arm: the control of which arm"
[[ -z "$heartbeat_ms" || "$heartbeat_ms" =~ ^[1-9][0-9]*$ ]] ||
  refuse "--heartbeat-ms must be a whole number of milliseconds"
readonly heartbeat_ms
runs=()
if [[ -n "$arm" ]]; then
  runs=("${arm}:${control}")
else
  runs=(stall:0 stall:1 recover:0 recover:1)
fi
readonly runs

me=smoke-registration-stall
# shellcheck source-path=SCRIPTDIR source=../../../scripts/e2e/lib/plugin-smoke.sh
. "${script_dir}/../../../scripts/e2e/lib/plugin-smoke.sh"
readonly repo_root="$root"

# The pane's refresh interval and the listener's live window, the two numbers every budget below
# is measured in. Both are the run's own, kept in the arrangement @legion/contracts declares them
# in (packages/contracts/src/envoy-session.ts): there, three refreshes and one whole listener call
# fall short of the window, with the slack SESSION_LIVE_WINDOW_MS - 3 * SESSION_HEARTBEAT_MS
# leaves to spare. A shorter refresh keeps that same call cost and slack, so the window is three
# refreshes shorter for every millisecond each refresh is: SESSION_LIVE_WINDOW_MS - 3 *
# (SESSION_HEARTBEAT_MS - refresh). The two are read from the package, never copied here, so the
# run moves with them. The pane's pass deadline is its refresh interval, as a deployment's is (the
# deadline is never shorter than the heartbeat, and the heartbeat there is past its 30 s floor),
# and the listener is given the window with ENVOY_SESSION_LIVE_WINDOW.
contracts_timing="$(cd -- "${repo_root}/packages/pi-envoy" &&
  bun -e 'import { SESSION_HEARTBEAT_MS, SESSION_LIVE_WINDOW_MS } from "@legion/contracts";
console.log(SESSION_HEARTBEAT_MS, SESSION_LIVE_WINDOW_MS);')" ||
  fail "reading SESSION_HEARTBEAT_MS and SESSION_LIVE_WINDOW_MS from @legion/contracts failed (the reason is above)"
[[ "$contracts_timing" =~ ^[0-9]+[[:space:]][0-9]+$ ]] ||
  fail "@legion/contracts answered \"${contracts_timing}\" for SESSION_HEARTBEAT_MS and SESSION_LIVE_WINDOW_MS, which is not two whole numbers"
read -r contract_heartbeat_ms contract_window_ms <<<"$contracts_timing"
readonly refresh_ms="${heartbeat_ms:-$default_refresh_ms}"
readonly live_window_ms=$((contract_window_ms - 3 * (contract_heartbeat_ms - refresh_ms)))
# Rounded up: each is a budget the session must outlive, so a sub-second remainder must not be
# waited out short.
readonly refresh_s=$(((refresh_ms + 999) / 1000))
readonly live_window_s=$(((live_window_ms + 999) / 1000))
((live_window_ms > 3 * refresh_ms)) ||
  fail "a ${refresh_ms} ms refresh leaves a live window of ${live_window_ms} ms, which three refreshes do not fall short of"

smoke_setup --listener-env "ENVOY_SESSION_LIVE_WINDOW=${live_window_ms}ms"
[[ -n "$plugin" ]] || plugin="$plugin_js"
note "refresh interval ${refresh_s}s and live window ${live_window_s}s (SESSION_HEARTBEAT_MS ${contract_heartbeat_ms} ms and SESSION_LIVE_WINDOW_MS ${contract_window_ms} ms, three refreshes shorter)"
note "plugin ${plugin}"
pass

# One arm, run in a subshell of its own: its verdict ends the subshell, and the subshell's exit
# stops its pane, so the next arm starts on the same stack with nothing of this one left running.
run_arm() {
  local arm=$1 control=$2 arm_label scratch fixture_log sessions_json listener_gets=0 listener_deletes=0
  arm_label="$arm"
  ((control == 0)) || arm_label="${arm}-control"
  scratch="$work/arm-${arm_label}"
  fixture_log="${scratch}.fixture.log"
  sessions_json="${scratch}.sessions.json"
  : >"$fixture_log"
  begin "$arm_label"

  verdict_pass() {
    note "$1"
    pass
    exit 0
  }
  verdict_fail() {
    fail "$1"
  }
  arm_cleanup() {
    local status=$? fd=${omp_fd[$arm_label]:-}
    # Best effort: a pane the arm already killed answers no signal, which must not end the arm.
    set +e
    [[ -z "$fd" ]] || exec {fd}>&-
    [[ -z "${omp_pid[$arm_label]:-}" ]] || stop_group "${omp_pid[$arm_label]}" 5
    exit "$status"
  }
  trap arm_cleanup EXIT
  [[ -r "$plugin" ]] || verdict_fail "plugin (${plugin}) is not readable"

  # Every listener call this arm makes goes through these two, which keep their result in
  # `http_code` rather than on stdout: a command substitution would run them in a subshell and lose
  # the counters, and the recover arm's claim that nothing but one delete wrote is an assertion
  # about this script's own traffic, not a hope.
  http_code=""
  list_sessions() {
    listener_gets=$((listener_gets + 1))
    http_code="$(curl -sS -o "$sessions_json" -w '%{http_code}' \
      --connect-timeout "$curl_connect_timeout" --max-time "$curl_max_time" \
      "${envoy_url}/v1/sessions" 2>/dev/null)" || http_code="000"
  }

  delete_session() {
    listener_deletes=$((listener_deletes + 1))
    http_code="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE \
      --connect-timeout "$curl_connect_timeout" --max-time "$curl_max_time" \
      "${envoy_url}/v1/sessions/$1" 2>/dev/null)" || http_code="000"
  }

  # This arm's own row among the listener's: `dir` is the directory the pane was launched in,
  # which no other session runs in. own_row returns 0 when it read the list, leaving the row in
  # `row_json` (empty when it is not listed), and 1 when it could not: no answer, a non-200 such as
  # the listener's refilling 503, or a body that is not a list. A failed read says nothing about
  # the row, so no caller may take it for absence; `list_read_error` keeps the last such failure
  # for the message.
  row_json=""
  list_read_error=""
  own_row() {
    row_json=""
    list_sessions
    if [[ "$http_code" == "000" ]]; then
      list_read_error="GET /v1/sessions got no answer from ${envoy_url}"
    elif [[ "$http_code" != "200" ]]; then
      list_read_error="GET /v1/sessions returned ${http_code}: $(head -c 200 "$sessions_json" 2>/dev/null | tr '\n' ' ' || true)"
    elif ! row_json="$(jq -c --arg dir "$scratch" \
      'if type == "array" then map(select(.dir == $dir)) | first // empty else error("not a list") end' \
      "$sessions_json" 2>/dev/null)"; then
      row_json=""
      list_read_error="GET /v1/sessions answered a body that is not a session list: $(head -c 200 "$sessions_json" | tr '\n' ' ' || true)"
    else
      return 0
    fi
    note "$list_read_error"
    return 1
  }

  # read_own_row retries own_row until a read of the list lands, for at most $1 seconds: 0 with the
  # row in `row_json` once one does, 1 with `list_read_error` naming the last failure when none did.
  read_own_row() {
    local deadline=$((SECONDS + $1))
    until own_row; do
      ((SECONDS + fast_poll_s < deadline)) || return 1
      sleep "$fast_poll_s"
    done
  }

  last_registration() {
    awk '$1 == "registration" && NF == 2 { n = $2 } END { print n + 0 }' "$fixture_log"
  }

  stall_lines() {
    grep -c 'reconcile pass stalled' "$fixture_log" || true
  }

  pane_alive() {
    [[ -n "${omp_pid[$arm_label]:-}" ]] && omp_running "$arm_label"
  }

  pane_tail() {
    tail -n 5 "$logs/stderr-${arm_label}.log" 2>/dev/null | tr '\n' ' '
  }

  # One real headless pane, loading the fixture ahead of the plugin so the fixture's `fetch`
  # wrapper is the one the plugin captures, with no tools and no other extension. Its input is a
  # fifo this script holds open, so the pane is one process this script can signal exactly: the
  # recover control needs a SIGKILL that reaches `omp` itself, since a pane that exits cleanly
  # deletes its own record and there would then be nothing for the control to delete.
  start_pane() {
    local wedge="$1"
    smoke_start_omp "$arm_label" "$scratch" \
      STALL_FIXTURE_LOG="$fixture_log" STALL_FIXTURE_WEDGE_AT="$wedge" \
      ENVOY_HEARTBEAT_MS="$refresh_ms" ENVOY_PASS_DEADLINE_MS="$refresh_ms" -- \
      --no-tools -e "${script_dir}/stall-fixture.ts" -e "$plugin"
    smoke_prompt "$arm_label" "Reply with the single word ok."
    note "pane started (pid ${omp_pid[$arm_label]}, wedge ${wedge}, refresh ${refresh_s}s) in ${scratch}"
  }

  # Bounded wait for this arm's row to appear in `row_json`; any other outcome fails the arm, and a
  # window in which no read of the list landed fails as unknown, never as an unregistered pane.
  wait_for_row() {
    local deadline=$((SECONDS + $1)) read_any=0
    while ((SECONDS < deadline)); do
      if own_row; then
        read_any=1
        [[ -z "$row_json" ]] || return 0
      fi
      pane_alive || verdict_fail "the pane exited before it registered under ${scratch}: $(pane_tail)"
      sleep "$fast_poll_s"
    done
    ((read_any == 1)) ||
      verdict_fail "could not read the listener's session list within $1s, so whether the pane registered under ${scratch} is unknown: ${list_read_error:-no read finished}"
    verdict_fail "the pane never registered under ${scratch} within $1s: $(pane_tail)"
  }

  row_field() {
    jq -r --arg field "$2" '.[$field]' <<<"$1"
  }

  age_s() {
    printf '%s' "$((($(date +%s%3N) - $1) / 1000))"
  }

  if [[ "$arm" == "stall" ]]; then
    run_stall
  else
    run_recover
  fi
}

run_stall() {
  start_pane "$wedge_at"
  wait_for_row 120
  local row="$row_json"
  local session_id
  session_id="$(row_field "$row" session_id)"
  note "registered as ${session_id}"

  note "waiting for the fixture to wedge registration ${wedge_at} (every ${refresh_s}s)"
  local wedge_deadline=$((SECONDS + 8 * refresh_s + 180))
  until grep -q "^registration ${wedge_at} wedged\$" "$fixture_log"; do
    ((SECONDS < wedge_deadline)) ||
      verdict_fail "registration ${wedge_at} was never reached; the log holds $(last_registration) registrations"
    pane_alive || verdict_fail "the pane exited before registration ${wedge_at}: $(pane_tail)"
    sleep 2
  done
  local wedged_at
  wedged_at="$(date +%s)"
  read_own_row "$list_read_budget_s" ||
    verdict_fail "could not read the listener's session list within ${list_read_budget_s}s of registration ${wedge_at} wedging, so the session's row at the wedge is unknown: ${list_read_error}"
  row="$row_json"
  [[ -n "$row" ]] || verdict_fail "the session was already unlisted when registration ${wedge_at} wedged"
  local wedged_last_seen
  wedged_last_seen="$(row_field "$row" last_seen)"
  note "registration ${wedge_at} is wedged; last_seen at the wedge is ${wedged_last_seen}"
  if ((control == 1)); then
    # The pane whose refresh never comes back: SIGKILLed at the wedge, so nothing refreshes its
    # record again and no clean exit deletes it. The record must still be listed after the kill,
    # so that its leaving later is the listener's window and not the kill.
    note "SIGKILLing the pane at the wedge, so nothing refreshes its record again"
    kill -KILL "${omp_pid[$arm_label]}" 2>/dev/null || verdict_fail "could not SIGKILL the pane (pid ${omp_pid[$arm_label]})"
    wait "${omp_pid[$arm_label]}" 2>/dev/null || true
    if pane_alive; then verdict_fail "the pane is still running after SIGKILL"; fi
    read_own_row "$list_read_budget_s" ||
      verdict_fail "could not read the listener's session list within ${list_read_budget_s}s of the SIGKILL, so whether the killed pane's record outlived it is unknown: ${list_read_error}"
    [[ -n "$row_json" ]] ||
      verdict_fail "the killed pane's record was gone at once; the control needs a record only the window can remove"
    note "the killed pane's record is still listed"
  fi

  # One live window past the wedged post plus a margin: before that, a listener record that is
  # merely still inside the window proves nothing either way.
  local margin=60
  ((control == 0)) || margin=90
  local target=$((wedged_at + live_window_s + margin))
  local gone_at=0
  note "watching until $(date -u -d "@${target}" +%H:%M:%SZ) (${live_window_s}s live window + ${margin}s)"
  while (($(date +%s) < target)); do
    sleep "$poll_s"
    # A list this poll could not read says nothing either way; own_row has noted why.
    own_row || continue
    row="$row_json"
    local regs stalls
    regs="$(last_registration)"
    stalls="$(stall_lines)"
    if [[ -z "$row" ]]; then
      note "not listed; registrations=${regs} stall lines=${stalls} ($(($(date +%s) - wedged_at))s past the wedge)"
      ((control == 1)) ||
        verdict_fail "the session left the registry $(($(date +%s) - wedged_at))s after the wedged post; it must stay listed"
      ((gone_at != 0)) || gone_at="$(date +%s)"
      continue
    fi
    ((gone_at == 0)) ||
      verdict_fail "the killed pane's session returned to the registry after leaving it, with nothing left to refresh it"
    local seen
    seen="$(row_field "$row" last_seen)"
    note "listed; last_seen=${seen} (age $(age_s "$seen")s) registrations=${regs} stall lines=${stalls}"
    ((control == 1)) || pane_alive || verdict_fail "the pane exited during the watch: $(pane_tail)"
  done

  read_own_row "$list_read_budget_s" ||
    verdict_fail "could not read the listener's session list within ${list_read_budget_s}s of the watch ending, so whether the session outlived the ${live_window_s}s live window is unproven: ${list_read_error}"
  row="$row_json"
  local regs stalls last_seen
  regs="$(last_registration)"
  stalls="$(stall_lines)"
  if ((control == 1)); then
    [[ -z "$row" ]] ||
      verdict_fail "the killed pane's session is still listed $(($(date +%s) - wedged_at))s after the wedged post (last_seen $(row_field "$row" last_seen)), past the ${live_window_s}s live window; the record must lapse"
    ((gone_at != 0)) || gone_at="$(date +%s)"
    ((regs <= wedge_at - 1)) ||
      verdict_fail "the killed pane posted registration ${regs} after the wedged ${wedge_at}"
    verdict_pass "${session_id}'s pane was SIGKILLed at the wedged registration ${wedge_at} (last accepted ${regs}), its record was still listed after the kill, left the registry $((gone_at - wedged_at))s after the wedged post and never returned (watched to $(($(date +%s) - wedged_at))s) — so the ${live_window_s}s live window the stall arm's pane outlives is one this listener enforces"
  fi

  [[ -n "$row" ]] ||
    verdict_fail "the session is not listed $(($(date +%s) - wedged_at))s after the wedged post; expected it to outlive the ${live_window_s}s live window"
  last_seen="$(row_field "$row" last_seen)"
  ((last_seen > wedged_last_seen)) ||
    verdict_fail "last_seen did not advance past the wedged post: ${wedged_last_seen} at the wedge, ${last_seen} now"
  ((regs > wedge_at)) ||
    verdict_fail "no registration past the wedged ${wedge_at} was posted (last accepted ${regs}); the refresh did not resume"
  ((stalls == 1)) ||
    verdict_fail "expected exactly one stall line in the pane's log, saw ${stalls}"
  verdict_pass "${session_id} stayed listed $(($(date +%s) - wedged_at))s after registration ${wedge_at} wedged — past the ${live_window_s}s live window — last_seen advanced ${wedged_last_seen} → ${last_seen}, the fixture logged ${regs} accepted registrations (past the wedged ${wedge_at}, so the refresh kept going), and the pane said the stall exactly once"
}

run_recover() {
  start_pane 0
  wait_for_row 120
  local row="$row_json"
  local session_id before_seen regs_before
  session_id="$(row_field "$row" session_id)"
  before_seen="$(row_field "$row" last_seen)"
  regs_before="$(last_registration)"
  note "registered as ${session_id} (last_seen ${before_seen}, ${regs_before} registrations posted)"

  if ((control == 1)); then
    # A pane that exits cleanly deletes its own record, so the control's pane is SIGKILLed: its
    # row outlives it, which is the row the delete then removes.
    note "SIGKILLing the pane so its record outlives it"
    kill -KILL "${omp_pid[$arm_label]}" 2>/dev/null || verdict_fail "could not SIGKILL the pane (pid ${omp_pid[$arm_label]})"
    wait "${omp_pid[$arm_label]}" 2>/dev/null || true
    if pane_alive; then verdict_fail "the pane is still running after SIGKILL"; fi
    read_own_row "$list_read_budget_s" ||
      verdict_fail "could not read the listener's session list within ${list_read_budget_s}s of the SIGKILL, so whether the killed pane's record outlived it is unknown: ${list_read_error}"
    row="$row_json"
    [[ -n "$row" ]] ||
      verdict_fail "the killed pane's record was already gone before the delete; nothing to control"
    note "the dead pane's record is still listed"
  fi

  delete_session "$session_id"
  [[ "$http_code" == "200" ]] ||
    verdict_fail "DELETE /v1/sessions/${session_id} returned ${http_code}, want 200"
  local deleted_at
  deleted_at="$(date +%s)"
  note "deleted ${session_id}; confirming it is unlisted"
  local gone=0 read_any=0 confirm_deadline=$((SECONDS + 30))
  while ((SECONDS < confirm_deadline)); do
    if own_row; then
      read_any=1
      if [[ -z "$row_json" ]]; then
        gone=1
        break
      fi
    fi
    sleep 2
  done
  if ((gone == 0)); then
    ((read_any == 1)) ||
      verdict_fail "could not read the listener's session list in the 30s after the delete, so whether it took is unknown: ${list_read_error:-no read finished}"
    verdict_fail "${session_id} was still listed 30s after the delete; the delete did not take"
  fi
  note "unlisted $(($(date +%s) - deleted_at))s after the delete"

  # One refresh interval is what the pane's own heartbeat needs; the margin covers the round trip.
  local budget=$((refresh_s + 45))
  local deadline=$((SECONDS + budget))
  if ((control == 1)); then
    note "watching ${budget}s: a record with no process behind it must stay gone"
    while ((SECONDS < deadline)); do
      sleep "$fast_poll_s"
      # A list this poll could not read says nothing either way; own_row has noted why.
      own_row || continue
      [[ -z "$row_json" ]] ||
        verdict_fail "${session_id} came back $(($(date +%s) - deleted_at))s after the delete although its process was already dead; the return in the main arm would not be the pane's own refresh"
      note "still gone ($(($(date +%s) - deleted_at))s)"
    done
    # The verdict rests on a list read at the end of the window, never on polls that all failed.
    read_own_row "$list_read_budget_s" ||
      verdict_fail "could not read the listener's session list within ${list_read_budget_s}s of the ${budget}s watch ending, so that ${session_id} stayed gone is unproven: ${list_read_error}"
    [[ -z "$row_json" ]] ||
      verdict_fail "${session_id} came back $(($(date +%s) - deleted_at))s after the delete although its process was already dead; the return in the main arm would not be the pane's own refresh"
    verdict_pass "${session_id}'s process was SIGKILLed, its record deleted, and it stayed gone for ${budget}s (one ${refresh_s}s refresh interval and a margin) — so a return in the main arm is the live pane's own refresh, not the listener restoring anything; this arm made ${listener_gets} list polls and ${listener_deletes} delete, and no other request"
  fi

  note "watching up to ${budget}s for the live pane to re-register itself"
  local read_after_delete=0
  while ((SECONDS < deadline)); do
    sleep "$fast_poll_s"
    if own_row; then
      read_after_delete=1
      row="$row_json"
      if [[ -n "$row" ]]; then
        local back_id back_seen
        back_id="$(row_field "$row" session_id)"
        back_seen="$(row_field "$row" last_seen)"
        [[ "$back_id" == "$session_id" ]] ||
          verdict_fail "a different session (${back_id}) now holds ${scratch}, not ${session_id}"
        ((back_seen > before_seen)) ||
          verdict_fail "the returned row's last_seen ${back_seen} did not advance past ${before_seen}"
        local regs_after
        regs_after="$(last_registration)"
        ((regs_after > regs_before)) ||
          verdict_fail "the fixture logged no registration after the delete (${regs_before} before, ${regs_after} after); the row did not come back from the pane's own post"
        note "back after $(($(date +%s) - deleted_at))s with last_seen=${back_seen}"
        verdict_pass "${session_id} was deleted from the registry while its process ran and re-registered itself $(($(date +%s) - deleted_at))s later (budget ${budget}s: one ${refresh_s}s refresh interval and a margin), last_seen ${before_seen} → ${back_seen} and the pane's own registration count ${regs_before} → ${regs_after}, with no human and no envoy_subscribe: the only requests this arm made to the listener are ${listener_gets} GET /v1/sessions polls and the single DELETE /v1/sessions/${session_id}"
      fi
      note "not back yet ($(($(date +%s) - deleted_at))s)"
    fi
    pane_alive || verdict_fail "the pane exited before re-registering: $(pane_tail)"
  done
  ((read_after_delete == 1)) ||
    verdict_fail "could not read the listener's session list in the ${budget}s after the delete, so whether ${session_id} re-registered is unknown: ${list_read_error:-no read finished}"
  verdict_fail "${session_id} did not re-register within ${budget}s of the delete (one ${refresh_s}s refresh interval and a margin); the pane is still running: $(pane_tail)"
}

bad=0
for run in "${runs[@]}"; do
  (run_arm "${run%%:*}" "${run##*:}") || bad=1
done
if ((bad == 1)); then
  # Each failed arm printed its own FAIL line.
  failed=1
  exit 1
fi
ok=1
