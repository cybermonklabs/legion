# shellcheck shell=bash
# shellcheck disable=SC2154,SC2034 # the leg that sources this file sets what it reads, and reads what it sets
# The read sweep's full-member half and its controls (restricted-sweep.sh, beside this file, row
# sweep): outside-agent-tailnet.sh sources this file from beside it, after outside-agent-lib.sh. It
# defines functions alone. They read the leg's `work`, `here`, `person`, `results`, `tab`,
# `newline`, `background`, `browser_problem` and its `browser` function, and set `sweep_problem`,
# `sweep_fills` (the fills file, base64, for the workflow), `writer_pid`, `proxy_control` and
# `proxy_control_problem`.
#
# A full member has no cookie at the outside origin, and no full member's credential goes on the
# runner. So this box runs the sweep's fills mode, which makes the full member's half of every
# row, and passes the file to the workflow as `sweep_fills`; and it runs the sweep's writer, which
# comments on the write issue every 15 s and reads the full member's own feed, as the live
# socket's control. Fills mode writes the full member's half of the GitHub proxy rows (a
# `can_fail` line for each read that can fail) only with the full member's browser session, since
# the proxy reads no token, so the sweep needs OUTSIDE_FULL_COOKIE_FILE.

# sweep_setup: sets sweep_script and sweep_problem, from the inputs and the browser's reading.
sweep_setup() {
  sweep_script="$here/restricted-sweep.sh"
  sweep_fills=''
  writer_pid=''
  sweep_problem=$(lacking OUTSIDE_SWEEP_PROJECT OUTSIDE_SWEEP_FULL_TOKEN_FILE OUTSIDE_SWEEP_CONVERSATION_FILE OUTSIDE_SWEEP_WRITE_ISSUE OUTSIDE_FULL_COOKIE_FILE)
  [ -n "$sweep_problem" ] || [ -f "$sweep_script" ] || sweep_problem="restricted-sweep.sh is not beside this file in $here"
  [ -n "$sweep_problem" ] || [ -r "${OUTSIDE_SWEEP_FULL_TOKEN_FILE:-}" ] ||
    sweep_problem="OUTSIDE_SWEEP_FULL_TOKEN_FILE names $OUTSIDE_SWEEP_FULL_TOKEN_FILE, which cannot be read"
  [ -n "$sweep_problem" ] || [ -z "$browser_problem" ] ||
    sweep_problem="the sweep's fills mode writes its GitHub proxy rows only with the full member's browser session, which was not read: $browser_problem"
}

# sweep_exec MODE OUT: becomes the sweep in MODE with the box's full member, its output in OUT.
# Its last word is `exec`, so a call started with `&` leaves `$!` naming the sweep itself, and a
# call that must return runs in a subshell: `( sweep_exec fills OUT )`. The token and the cookie
# reach the sweep through its environment alone, never its command line.
sweep_exec() {
  SWEEP_FULL_TOKEN=$(tr -d '\r\n' <"$OUTSIDE_SWEEP_FULL_TOKEN_FILE")
  SWEEP_FULL_COOKIE=$(tr -d '\r\n' <"$OUTSIDE_FULL_COOKIE_FILE")
  export SWEEP_FULL_TOKEN SWEEP_FULL_COOKIE
  SWEEP_MODE=$1 SWEEP_ORIGIN=$OUTSIDE_TAILNET_DISPATCH_URL SWEEP_PROJECT=$OUTSIDE_SWEEP_PROJECT \
    SWEEP_MEMBER=$person SWEEP_CONVERSATION_FILE=$OUTSIDE_SWEEP_CONVERSATION_FILE \
    SWEEP_FILLS_FILE="$work/sweep-fills" \
    SWEEP_WRITE_ISSUE=$OUTSIDE_SWEEP_WRITE_ISSUE SWEEP_WRITE_SECONDS=3600 SWEEP_WRITE_EVERY=15 \
    exec bash "$sweep_script" >"$2" 2>&1 </dev/null
}

# sweep_prepare: the fills, and then the writer, which comments every 15 s on a scratch issue
# (OUTSIDE_SWEEP_WRITE_ISSUE), about four comments a minute, until sweep_writer_stop.
sweep_prepare() {
  [ -z "$sweep_problem" ] || return 0
  say "the sweep's fills mode"
  if ! (sweep_exec fills "$work/sweep-fills.log") || [ ! -s "$work/sweep-fills" ]; then
    sweep_problem="the sweep's fills mode made no file: $(text_of "$work/sweep-fills.log")"
  elif [ "$(wc -c <"$work/sweep-fills")" -gt 40960 ]; then
    sweep_problem="the sweep's fills file is $(wc -c <"$work/sweep-fills") bytes, past the 40 KiB one workflow input holds"
  else
    sweep_fills=$(base64 <"$work/sweep-fills" | tr -d '\n')
    sweep_exec writer "$work/sweep-writer.log" &
    writer_pid=$!
    background="$background $writer_pid"
  fi
}

# sweep_writer_stop: the public leg has ended, so the writer has nothing more to show it.
sweep_writer_stop() {
  [ -z "${writer_pid:-}" ] || stop_child "$writer_pid"
}

# sweep_proxy_control: the sweep's proxy rows' positive control. The runner reads the GitHub proxy
# as the test person for the repository the fills name as one only the sweep's project's issues
# resolve to, and must be answered as for a repository the App is not installed on. That answer
# means something only where the proxy answers the same repository to a person who may see it.
# So the fills must hold the full member's answer that can fail for each proxy read the runner
# makes (a `can_fail` line each), and this box's browser reads, through the proxy, the workflow
# runs of the commit the sweep's own proxy rows name, and must get 200 with a list of workflow
# runs. Without either, the sweep row reads UNKNOWN.
sweep_proxy_control() {
  local proxy_sha=0123456789abcdef0123456789abcdef01234567 proxy_repo='' can_fail blind='' name value each status answer
  local proxy_reads='GET /api/github/rest/repos/{owner}/{repo}/pulls/{number}
GET /api/github/rest/repos/{owner}/{repo}/issues/{number}
GET /api/github/rest/repos/{owner}/{repo}/commits/{sha}/check-runs
GET /api/github/rest/repos/{owner}/{repo}/actions/runs?head_sha={sha}
GET /api/v1/github/app-installation/{owner}/{repo}'
  proxy_control=''
  proxy_control_problem=$browser_problem
  [ -n "$proxy_control_problem" ] || [ -z "$sweep_problem" ] ||
    proxy_control_problem="the sweep's fills, which name the repository, were not made: $sweep_problem"
  [ -z "$proxy_control_problem" ] || return 0
  can_fail=$newline
  while IFS="$tab" read -r name value; do
    case $name in
      proxy_repo) proxy_repo=$value ;;
      can_fail) can_fail="$can_fail$value$newline" ;;
    esac
  done <"$work/sweep-fills"
  while IFS= read -r each; do
    case $can_fail in *"$newline$each$newline"*) ;; *) blind="${blind:+$blind, }$each" ;; esac
  done <<EOF
$proxy_reads
EOF
  if [ -n "$blind" ]; then
    proxy_control_problem="the sweep's fills hold no full member's answer that can fail for $blind"
    return 0
  fi
  case $proxy_repo in
    '') proxy_control_problem="the sweep's fills name no repository that only $OUTSIDE_SWEEP_PROJECT's issues resolve to" ;;
    *[!A-Za-z0-9_./-]* | */*/* | /* | */) proxy_control_problem="the sweep's fills name no owner/name repository: $proxy_repo" ;;
    */*)
      status=$(browser GET "/api/github/rest/repos/$proxy_repo/actions/runs?head_sha=$proxy_sha" "$work/proxy-control.json")
      answer=$(tr -d ' \n' <"$work/proxy-control.json" 2>/dev/null)
      case $status:$answer in
        200:*'"workflow_runs":'*) proxy_control="the full member's browser read $proxy_repo's workflow runs through the proxy: 200" ;;
        *) proxy_control_problem="the full member's browser read of $proxy_repo through the proxy answered $status: ${answer:0:200} $(text_of "$work/browser.err")" ;;
      esac
      ;;
    *) proxy_control_problem="the sweep's fills name no owner/name repository: $proxy_repo" ;;
  esac
}

# sweep_row: the sweep row: the read sweep on the runner, held to this box's two controls: at
# least one of the full member's comments delivered strictly inside the window the sweep read the
# member's feed over, which is what makes its "nothing reached the member" mean something, and the
# proxy control, which is what makes its proxy rows' "answered as a repository the App is not
# installed on" mean something. A FAIL of the sweep stands without either.
sweep_row() {
  local want="restricted-sweep.sh on the runner PASS, with a full member's comment delivered inside its live window, and the full member's browser answered 200 by the GitHub proxy for the repository its proxy rows read"
  local log="$results/restricted-sweep.log" sweep_exit window='' w0 w1 inside='' line at verdict_line=''
  if [ -n "$sweep_problem" ]; then
    row sweep "$want" "$sweep_problem" UNKNOWN
    return
  fi
  if [ ! -f "$log" ]; then
    row sweep "$want" "the run left no restricted-sweep.log${public_problem:+: $public_problem}" UNKNOWN
    return
  fi
  sweep_exit=$(tr -d ' \n' <"$results/restricted-sweep.exit" 2>/dev/null)
  while IFS= read -r line; do
    case $line in 'live window '*) window=${line#live window } ;; esac
  done <"$log"
  w0=${window%% *}
  w1=${window#* }
  while IFS= read -r line; do
    case $line in 'write '*' delivered')
      at=${line#write }
      at=${at%% *}
      [[ -n "$w0" && "$at" > "$w0" && "$at" < "$w1" ]] && inside=$at
      ;;
    esac
  done <"$work/sweep-writer.log"
  # The verdict is the sweep's last line; the log's first is the workflow's own note.
  while IFS= read -r line; do
    [ -z "$line" ] || verdict_line=$line
  done <"$log"
  verdict_line=${verdict_line:0:300}
  case $sweep_exit in
    1) row sweep "$want" "the sweep failed: $verdict_line" FAIL ;;
    0)
      if [ -n "$proxy_control_problem" ]; then
        row sweep "$want" "the sweep passed, and its GitHub proxy rows have no positive control: $proxy_control_problem" UNKNOWN
      elif [ -z "$window" ]; then
        row sweep "$want" "the sweep passed and printed no live window, so the writer's control cannot be held to it" UNKNOWN
      elif [ -z "$inside" ]; then
        row sweep "$want" "the sweep passed, and no comment of the writer was delivered inside its live window $window, so its live check proved nothing" UNKNOWN
      else
        row sweep "$want" "the sweep passed; the writer's comment at $inside was delivered inside its live window $window; $proxy_control" PASS
      fi
      ;;
    *) row sweep "$want" "the sweep exited ${sweep_exit:-with no code read}: $verdict_line" UNKNOWN ;;
  esac
}
