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
# every 15 s comments on the write issue and sends a direct message to the fills' session, prints
# `write <began> <ended> <comment id> <message id> delivered` for each the full member's own feed
# was sent (`began` read before the comment's POST, `ended` after the message's POST answered),
# and so is the live socket's control. Fills mode writes the full member's half of the GitHub proxy
# rows (a `can_fail` line for each read that can fail) only with the full member's browser session,
# since the proxy reads no token, so the sweep needs OUTSIDE_FULL_COOKIE_FILE.
#
# The contract this file reads is #4608's at ca10a682ba, fills version 4: `routes`, the SHA-256 of
# the routes.txt the fills were made from, `deployment`, of the member's own projects' keys and
# creation times, and `origin_sha256`, of the origin they were made at. A runner refuses fills of
# another version, of another routes.txt than its own checkout's, or of another deployment, and
# refuses a SWEEP_LIVE_SECONDS below 45; each such refusal is its UNKNOWN, and the sweep row's. So
# the workflow's checkout must hold this checkout's routes.txt, and this box sends the fills only
# when they are version 4.

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
  local first='' version=4
  [ -z "$sweep_problem" ] || return 0
  say "the sweep's fills mode"
  if ! (sweep_exec fills "$work/sweep-fills.log") || [ ! -s "$work/sweep-fills" ]; then
    sweep_problem="the sweep's fills mode made no file: $(text_of "$work/sweep-fills.log")"
    return 0
  fi
  IFS= read -r first <"$work/sweep-fills"
  if [ "$first" != "restricted-sweep-fills$tab$version" ]; then
    sweep_problem="the sweep beside this leg wrote fills of another version than $version, which a runner of #4608's contract refuses: their first line is ${first:0:60}"
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
# least one of the full member's writes (a comment and a direct message) delivered inside the
# window the sweep read the member's feed over, with every write of the writer delivered, which is
# what makes its "nothing reached the member" mean something, and the
# proxy control, which is what makes its proxy rows' "answered as a repository the App is not
# installed on" mean something. A FAIL of the sweep stands without either.
sweep_row() {
  local want="restricted-sweep.sh on the runner PASS, with every write of the writer (a comment and a direct message) delivered and one inside its live window, and the full member's browser answered 200 by the GitHub proxy for the repository its proxy rows read"
  local log="$results/restricted-sweep.log" sweep_exit window='' w0='' w1='' inside='' line verdict_line=''
  local word began ended comment message state rest unread='' dropped=''
  local stamp='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'
  local id='^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
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
  # The window's two ends, as `date -u +%Y-%m-%dT%H:%M:%SZ` prints them; any other window is none.
  read -r w0 w1 rest <<<"$window"
  [[ $w0 =~ $stamp && $w1 =~ $stamp && -z $rest && ! $w0 > $w1 ]] || window=''
  # The writer prints one line a write: `write <began> <ended> <comment id> <message id>
  # delivered`, `... not delivered` when the full member's feed was not sent both, or `write
  # <began> <ended> - not written`. As #4608's own join reads them, every line must be readable and
  # delivered: a write not delivered or not written anywhere, inside the window or across one of its
  # ends or before it, means the feed lost one, and a line whose stamps are not of that form, or that
  # began after it ended, or a delivered one whose ids are not of the lowercase 8-4-4-4-12 form
  # Dispatch gives them, cannot be read; either holds the row UNKNOWN. Then one delivered write
  # inside the window is the control: one that began after the window opened and ended before it
  # closed (UTC whole seconds, compared as strings), so the whole write fell inside the window the
  # sweep read the member's feed over.
  while IFS= read -r line; do
    read -r word began ended comment message state rest <<<"$line"
    [ "$word" = write ] || continue
    if ! [[ $began =~ $stamp && $ended =~ $stamp ]] || [[ $began > $ended ]]; then
      unread=${unread:-$line}
      continue
    fi
    case $comment:$message:$state:$rest in
      -:not:written: | ?*:?*:not:delivered) dropped=${dropped:-$line} ;;
      -:*) unread=${unread:-$line} ;;
      ?*:?*:delivered:)
        if ! [[ $comment =~ $id && $message =~ $id ]]; then
          unread=${unread:-$line}
        elif [[ -n $window && $began > $w0 && $ended < $w1 ]]; then
          inside="from $began to $ended (comment $comment, message $message)"
        fi
        ;;
      *) unread=${unread:-$line} ;;
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
        row sweep "$want" "the sweep passed and printed no readable live window, so the writer's control cannot be held to it" UNKNOWN
      elif [ -n "$unread" ]; then
        row sweep "$want" "the sweep passed, and the writer printed a line this leg cannot read (a stamp not of the form date -u +%Y-%m-%dT%H:%M:%SZ prints, one that began after it ended, or ids not of Dispatch's lowercase 8-4-4-4-12 form), so whether it dropped a write is not known: ${unread:0:200}" UNKNOWN
      elif [ -n "$dropped" ]; then
        row sweep "$want" "the sweep passed, and a write of the writer was not delivered, so the member's feed lost one and its live check proved nothing: ${dropped:0:200}" UNKNOWN
      elif [ -z "$inside" ]; then
        row sweep "$want" "the sweep passed, and no write of the writer was delivered inside its live window $window, so its live check proved nothing" UNKNOWN
      else
        row sweep "$want" "the sweep passed; the writer's write $inside was delivered inside its live window $window; $proxy_control" PASS
      fi
      ;;
    *) row sweep "$want" "the sweep exited ${sweep_exit:-with no code read}: $verdict_line" UNKNOWN ;;
  esac
}
