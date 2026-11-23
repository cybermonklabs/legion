# shellcheck shell=bash
# What the legs of the outside agent's acceptance share: outside-agent-public.sh,
# outside-agent-tailnet.sh and outside-agent-codex.sh source this file from beside them. It is
# written for bash 3.2, as the public leg is. It defines functions alone. They read two variables
# of the leg that sources them: `allow_http`, OUTSIDE_ALLOW_HTTP's value, and `background`, the
# pids of the leg's background children, which `reap` stops.

# outside_issue_title: the title of the scratch project's issue that names the repository the edge
# relays: the tailnet leg finds or makes it (row issue), and the Codex leg comments on it (C3).
outside_issue_title() { printf '%s' 'Outside acceptance: the repository the edge relays to the project'; }

# text_of FILE: FILE's first 300 characters, on one line.
text_of() {
  local text
  text=$(tr '\n' ' ' <"$1" 2>/dev/null)
  printf '%s' "${text:0:300}"
}

# cell TEXT: TEXT on one line with no tab, so it stays one TSV field.
cell() {
  local text=$1 tab
  tab=$(printf '\t')
  text=${text//$tab/ }
  text=${text//$'\n'/ }
  text=${text//$'\r'/ }
  printf '%s' "$text"
}

now() { date -u '+%s'; }

# iso_seconds TIME: the Unix seconds of TIME, a `YYYY-MM-DDTHH:MM:SSZ` UTC time, read with GNU's
# date or BSD's (macOS ships BSD's); nothing when neither reads it.
iso_seconds() {
  date -u -d "$1" '+%s' 2>/dev/null || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$1" '+%s' 2>/dev/null
}

# json_field NAME LINE: the value of "NAME": in LINE, one compact JSON object: a string's text, or
# a number, true, false or null as written; nothing, with a false exit, when LINE has no NAME.
json_field() {
  local rest=${2#*\""$1"\":}
  [ "$rest" != "$2" ] || return 1
  case $rest in
    \"*)
      rest=${rest#\"}
      printf '%s' "${rest%%\"*}"
      ;;
    *) printf '%s' "${rest%%[,\}]*}" ;;
  esac
}

# json_string TEXT: TEXT as a JSON string, quotes included: a backslash, a double quote, a tab, a
# newline and a carriage return escaped. A leg's values hold no other control character.
json_string() {
  local text=$1
  text=${text//\\/\\\\}
  text=${text//\"/\\\"}
  text=${text//$'\t'/\\t}
  text=${text//$'\n'/\\n}
  text=${text//$'\r'/\\r}
  printf '"%s"' "$text"
}

# tool_problem NAME...: which of the named programs are not on PATH, or nothing.
tool_problem() {
  local name out=''
  for name in "$@"; do
    command -v "$name" >/dev/null 2>&1 || out="${out:+$out, }$name"
  done
  [ -z "$out" ] || printf '%s not on PATH' "$out"
}

# origin_problem NAME: why NAME's value is no origin a leg may use, or nothing: https://<host>, or
# http://127.0.0.1:<port> or http://localhost:<port> only with OUTSIDE_ALLOW_HTTP=1.
origin_problem() {
  local value
  eval "value=\${$1:-}"
  case $value in
    '') printf '%s is unset' "$1" ;;
    https://*/* | http://*/*) printf '%s must be an origin alone, with no path: %s' "$1" "$value" ;;
    https://?*) ;;
    http://127.0.0.1:* | http://localhost:*)
      [ "${allow_http:-}" = 1 ] || printf '%s may be http only with OUTSIDE_ALLOW_HTTP=1: %s' "$1" "$value"
      ;;
    *) printf '%s must be an https origin: %s' "$1" "$value" ;;
  esac
}

# lacking NAME...: the problems of the named inputs, joined with "; ": a name ending _URL must be
# an origin, any other must be set.
lacking() {
  local name value problem out=''
  for name in "$@"; do
    case $name in
      *_URL) problem=$(origin_problem "$name") ;;
      *)
        eval "value=\${$name:-}"
        problem=''
        [ -n "$value" ] || problem="$name is unset"
        ;;
    esac
    [ -z "$problem" ] || out="${out:+$out; }$problem"
  done
  printf '%s' "$out"
}

# real_tools DIR NAME...: a link in DIR to the real program behind each NAME. A version manager's
# shim (mise's, or a wrapper that calls mise) finds its program through the caller's HOME, and the
# children a leg starts under a HOME of their own have none, so they run the real programs.
real_tools() {
  local dir=$1 name found
  shift
  mkdir -p "$dir" || return
  for name in "$@"; do
    found=''
    if command -v mise >/dev/null 2>&1; then found=$(mise which "$name" 2>/dev/null </dev/null); fi
    [ -n "$found" ] || found=$(command -v "$name" 2>/dev/null)
    case $found in /*) ln -sf "$found" "$dir/$name" ;; esac
  done
}

# Every background child a leg starts is started so that `$!` is the child itself: a simple command
# (`env -i … cmd … &`, prefix assignments allowed), or a function whose last word is `exec`, started
# with `&`. A subshell that runs a command and then does anything else (`( cmd; echo $? >f ) &`)
# leaves `$!` naming the subshell, and a TERM to it leaves the command running; a child whose exit
# status a leg reads is waited for with `wait PID` and then forgotten.
#
# A pid is signalled only while bash still counts it as a running child of this shell (`jobs -rp`):
# once bash has reaped a child, its pid is free, and on a developer box, which forks thousands of
# processes a second, another session's process holds it within minutes. A zombie that bash has not
# reaped yet still holds its pid, so a signal to it reaches nothing else.

# running_child PID: whether PID is a child of this shell that bash still counts as running.
running_child() {
  local pid
  for pid in $(jobs -rp 2>/dev/null); do
    [ "$pid" != "$1" ] || return 0
  done
  return 1
}

# worst_row FILE NAME: FILE's row NAME (a TSV line whose first field is NAME and whose last is its
# verdict), whole, or nothing. Where FILE holds NAME more than once, the worst copy: FAIL, then any
# word but PASS, then PASS, the first of equals, so a later PASS never hides an earlier FAIL. Every
# row a leg reads from a file another program wrote is read through it.
worst_row() {
  local each worst='' rank best=-1 tab_=$'\t'
  [ -f "$1" ] || return 0
  while IFS= read -r each || [ -n "$each" ]; do
    case $each in "$2$tab_"*) ;; *) continue ;; esac
    case ${each##*"$tab_"} in
      FAIL) rank=2 ;;
      PASS) rank=0 ;;
      *) rank=1 ;;
    esac
    if [ "$rank" -gt "$best" ]; then
      best=$rank
      worst=$each
    fi
  done <"$1"
  printf '%s' "$worst"
}

# forget PID: takes PID out of `background`, once it is waited for or stopped.
forget() {
  local pid kept=''
  for pid in ${background:-}; do
    [ "$pid" = "$1" ] || kept="$kept $pid"
  done
  background=$kept
}

# shellcheck disable=SC2034 # child_exit is await_child's answer, which the leg that sources this file reads
# await_child PID SECONDS INTERVAL: waits up to SECONDS, looking every INTERVAL s, for the child PID
# to end. Once it has, waits for it, forgets it and sets child_exit to its exit status; while it
# still runs, child_exit is empty and the child stays on the list.
await_child() {
  local deadline
  deadline=$(($(now) + $2))
  child_exit=''
  while running_child "$1" && [ "$(now)" -lt "$deadline" ]; do sleep "$3"; done
  running_child "$1" && return 0
  wait "$1"
  child_exit=$?
  forget "$1"
}

# stop_child PID: stops one background child of the leg, if it still runs: TERM, up to 10 s for it
# to end, then KILL, and waits for it, so it writes nothing after the leg goes on; and forgets it.
stop_child() {
  local _
  forget "$1"
  running_child "$1" || return 0
  kill "$1" 2>/dev/null
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    running_child "$1" || break
    sleep 1
  done
  running_child "$1" && kill -9 "$1" 2>/dev/null
  wait "$1" 2>/dev/null
  return 0
}

# reap: stops every child `background` names that still runs, as stop_child stops one, all of them
# at once, and empties the list. A pid of the list that bash no longer counts as running is left
# alone.
reap() {
  local pid alive live='' _
  [ -n "${background:-}" ] || return 0
  for pid in $background; do
    running_child "$pid" && live="$live $pid"
  done
  background=''
  [ -n "$live" ] || return 0
  for pid in $live; do kill "$pid" 2>/dev/null; done
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    alive=''
    for pid in $live; do running_child "$pid" && alive=yes; done
    [ -n "$alive" ] || break
    sleep 1
  done
  for pid in $live; do
    running_child "$pid" && kill -9 "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
  done
}

# stop_tmux SOCKET: stops the tmux server at SOCKET and waits up to 10 s for its pane's process to
# exit, since an exiting Oh My Pi writes under its HOME and its session directory.
stop_tmux() {
  local pane _
  [ -S "$1" ] || return 0
  pane=$(tmux -S "$1" display-message -p '#{pane_pid}' 2>/dev/null)
  tmux -S "$1" kill-server 2>/dev/null
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    { [ -n "$pane" ] && kill -0 "$pane" 2>/dev/null; } || break
    sleep 1
  done
}

# omp_clear_screens SOCKET TARGET: OMP_SKIP_SETUP keeps Oh My Pi's setup wizard shut; any
# first-run screen a fresh HOME still opens (its splash) is skipped as a person who skips it
# would: a setup screen with Escape, the splash with Enter. One screen can follow another a
# moment later, so it returns only after three checks in a row, 2 s apart, show neither (at most
# 40 s).
omp_clear_screens() {
  local checks=0 clear=0
  while [ "$checks" -lt 20 ] && [ "$clear" -lt 3 ]; do
    checks=$((checks + 1))
    case $(tmux -S "$1" capture-pane -p -t "$2" 2>/dev/null) in
      *'exit setup'*)
        tmux -S "$1" send-keys -t "$2" Escape
        clear=0
        ;;
      *'to skip'*)
        tmux -S "$1" send-keys -t "$2" Enter
        clear=0
        ;;
      *) clear=$((clear + 1)) ;;
    esac
    sleep 2
  done
}
