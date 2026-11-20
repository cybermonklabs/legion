#!/usr/bin/env bash
# The public leg of the outside agent's acceptance, rows P1 to P9: what an outside person's agent
# does and is refused from the public internet, off the private network, through the three edge
# origins alone. A workflow on a runner on the public internet runs it after its own browser has
# signed the test person in. outside-agent-acceptance.md, beside this file, says what each row
# reads, how the tailnet leg starts this one, and the gate both legs make.
#
# It is written for bash 3.2 (as macOS ships it) and later. It drives what the outside person
# installs and runs: Oh My Pi (`omp`) in tmux, as packages/pi-envoy/scripts/smoke-delivery.sh
# runs it, the `dispatch` command the plugin ships, and curl. Calls that carry the agent's broker
# credential, which only the holder of its key can sign, go through outside-agent-probe.ts beside
# this file, run with bun.
#
# Every process it starts runs with `env -i` and section (c)'s settings alone, under a HOME of
# its own in a `mktemp -d` directory, so nothing of the machine it runs on reaches them:
#   ENVOY_URL=<OUTSIDE_LISTENER_URL> ENVOY_INBOUND=listener DISPATCH_URL=<OUTSIDE_DISPATCH_URL>
#   DISPATCH_TOKEN_BROKER=1 AGENT_SECRETS_URL=<OUTSIDE_BROKER_URL>
#   AGENT_SECRETS_MACHINE_IDENTITY=device-login AGENT_SECRETS_PROJECTS=<OUTSIDE_PROJECT>
# beside PATH, HOME, TERM, TMPDIR and OMP_SKIP_SETUP=1, Oh My Pi's own switch that skips its
# first-run setup wizard, which a fresh HOME would otherwise open. No model credential,
# ENVOY_NATS_URL, ENVOY_TOKEN or DISPATCH_TOKEN reaches any of them.
#
# Inputs, each named by the row that cannot run without it:
#   OUTSIDE_DISPATCH_URL, OUTSIDE_BROKER_URL, OUTSIDE_LISTENER_URL
#                             the three edge origins, https://<host>; http://127.0.0.1:<port>
#                             only with OUTSIDE_ALLOW_HTTP=1, for a local stack and this file's
#                             tests.
#   OUTSIDE_PROJECT           the scratch project's key.
#   OUTSIDE_PERSON            the test person's address.
#   OUTSIDE_PEER_SESSION      our peer session (the tailnet leg runs it: outside-agent-probe.ts
#                             peer).
#   OUTSIDE_APPROVE_COMMAND   a command line, split on blanks, that approves one device login's
#                             code as the test person: run with the code as its last word, it
#                             prints `APPROVED <code>` and exits 0, or exits 1 (refused) or 2
#                             (could not tell).
#   OUTSIDE_PI_ENVOY_VERSION  the @sjawhar/pi-envoy release P1 installs; OUTSIDE_PI_ENVOY_PACKAGE
#                             names a package path or tarball instead, for a local stack.
#   OUTSIDE_REPOSITORY        owner/name of the repository the scratch project's issue resolves
#                             to, whose events P5 must be admitted to.
#   OUTSIDE_PATH_TARGETS      the target list outside-path-reach.sh reads (P9).
# Optional: OUTSIDE_OTHER_PROJECT (default EXAMPLE) and OUTSIDE_OTHER_REPOSITORY (default
# example-owner/example-repo), the project and repository P5 must be refused;
# OUTSIDE_AGENT_PUBLIC_TSV, a file that gets a copy of the TSV; OUTSIDE_GITHUB_EVENT_SECONDS
# (default 900), how long P5 waits for an event.
#
# Output: on stdout a TSV, the header `row want got verdict`, one line per row P1 to P9, and a
# last `verdict` line. Progress goes to stderr. A row reads PASS, FAIL or UNKNOWN: UNKNOWN when it
# could not run, its `got` naming what it could not read, never PASS. Exit 0 when every row
# passed, 1 when any failed, 2 otherwise. A row that needs another row's result reads UNKNOWN when
# that row did not pass.
#
# Usage: OUTSIDE_DISPATCH_URL=https://dispatch.edge.example ... bash outside-agent-public.sh

if [ -z "${BASH_VERSION:-}" ]; then
  echo 'UNKNOWN: run this with bash: bash outside-agent-public.sh'
  exit 2
fi
set -u
LC_ALL=C
export LC_ALL

case $0 in */*) here=${0%/*} ;; *) here=. ;; esac
here=$(cd "$here" && pwd)
tab=$(printf '\t')
newline='
'

tsv=${OUTSIDE_AGENT_PUBLIC_TSV:-}
if [ -n "$tsv" ]; then
  : >"$tsv" || tsv=''
fi

# emit LINE: one line of the TSV, on stdout and in OUTSIDE_AGENT_PUBLIC_TSV.
emit() {
  printf '%s\n' "$1"
  [ -z "$tsv" ] || printf '%s\n' "$1" >>"$tsv"
}

# cell TEXT: TEXT on one line with no tab, so it stays one TSV field.
cell() {
  local text=$1
  text=${text//$tab/ }
  text=${text//$newline/ }
  text=${text//$'\r'/ }
  printf '%s' "$text"
}

fails=0
unknowns=0
# row NAME WANT GOT VERDICT: prints the row and keeps its verdict as verdict_<NAME>, a dash in
# NAME read as an underscore.
row() {
  emit "$1$tab$(cell "$2")$tab$(cell "$3")$tab$4"
  printf -v "verdict_${1//-/_}" '%s' "$4"
  case $4 in
    FAIL) fails=$((fails + 1)) ;;
    UNKNOWN) unknowns=$((unknowns + 1)) ;;
  esac
}
passed() {
  local name="verdict_${1//-/_}"
  [ "${!name:-}" = PASS ]
}
say() { printf 'outside-agent-public: %s\n' "$1" >&2; }
# text_of FILE: FILE's first 300 characters, on one line.
text_of() {
  local text
  text=$(tr '\n' ' ' <"$1" 2>/dev/null)
  printf '%s' "${text:0:300}"
}
# stamp: this runner's clock in Unix ms, to the second, which bash 3.2's date can give.
stamp() { printf '%s000' "$(date -u '+%s')"; }

emit "row${tab}want${tab}got${tab}verdict"

# --- Inputs. `missing` collects what each row lacks, by name. ---
allow_http=${OUTSIDE_ALLOW_HTTP:-}
# origin_problem NAME: why NAME's value is no origin this leg may use, or nothing.
origin_problem() {
  local value
  eval "value=\${$1:-}"
  case $value in
    '') printf '%s is unset' "$1" ;;
    https://*/* | http://*/*) printf '%s must be an origin alone, with no path: %s' "$1" "$value" ;;
    https://?*) ;;
    http://127.0.0.1:* | http://localhost:*)
      [ "$allow_http" = 1 ] || printf '%s may be http only with OUTSIDE_ALLOW_HTTP=1: %s' "$1" "$value"
      ;;
    *) printf '%s must be an https origin: %s' "$1" "$value" ;;
  esac
}
unset_problem() {
  local value
  eval "value=\${$1:-}"
  [ -n "$value" ] || printf '%s is unset' "$1"
}
# lacking NAME...: the problems of the named inputs, joined with "; ".
lacking() {
  local name problem out=''
  for name in "$@"; do
    case $name in
      *_URL) problem=$(origin_problem "$name") ;;
      *) problem=$(unset_problem "$name") ;;
    esac
    [ -z "$problem" ] || out="${out:+$out; }$problem"
  done
  printf '%s' "$out"
}
# tool_problem NAME...: which of the named programs are not on PATH.
tool_problem() {
  local name out=''
  for name in "$@"; do
    command -v "$name" >/dev/null 2>&1 || out="${out:+$out, }$name"
  done
  [ -z "$out" ] || printf '%s not on PATH' "$out"
}

dispatch=${OUTSIDE_DISPATCH_URL:-}
broker=${OUTSIDE_BROKER_URL:-}
listener=${OUTSIDE_LISTENER_URL:-}
project=${OUTSIDE_PROJECT:-}
person=${OUTSIDE_PERSON:-}
peer=${OUTSIDE_PEER_SESSION:-}
repository=${OUTSIDE_REPOSITORY:-}
other_project=${OUTSIDE_OTHER_PROJECT:-EXAMPLE}
[ "$other_project" != "$project" ] || other_project="${project}X"
other_repository=${OUTSIDE_OTHER_REPOSITORY:-example-owner/example-repo}
event_seconds=${OUTSIDE_GITHUB_EVENT_SECONDS:-900}

# --- The scratch directory, its HOME and its cleanup. ---
work=''
tmux_socket=''
background=''
# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  local pid
  for pid in $background; do kill "$pid" 2>/dev/null; done
  if [ -n "$tmux_socket" ] && [ -S "$tmux_socket" ]; then
    tmux -S "$tmux_socket" kill-server 2>/dev/null
  fi
  [ -z "$work" ] || rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
nonce=$(od -An -N6 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n')
[ -n "$nonce" ] || nonce=$$
probe_session="outside-e2e-probe-$nonce"
work=$(mktemp -d "${TMPDIR:-/tmp}/outside-agent-public.XXXXXX") || work=''
home="$work/home"
# The Oh My Pi session's directory, by its real path, which is what the session registers.
session_dir=''
if [ -n "$work" ]; then
  mkdir -p "$home" "$work/outside-e2e-$nonce"
  session_dir=$(cd "$work/outside-e2e-$nonce" && pwd -P)
fi

# real_tools DIR NAME...: a link in DIR to the real program behind each NAME. A version manager's
# shim (mise's, or a wrapper that calls mise) finds its program through the caller's HOME, and the
# children this leg starts under a HOME of their own have none, so they run the real programs.
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
[ -z "$work" ] || real_tools "$work/tools" bun omp tmux

session_env=(
  "PATH=$work/tools:$PATH"
  "HOME=$home"
  "TERM=${TERM:-xterm-256color}"
  "TMPDIR=${TMPDIR:-/tmp}"
  "OMP_SKIP_SETUP=1"
  "ENVOY_URL=$listener"
  "ENVOY_INBOUND=listener"
  "DISPATCH_URL=$dispatch"
  "DISPATCH_TOKEN_BROKER=1"
  "AGENT_SECRETS_URL=$broker"
  "AGENT_SECRETS_MACHINE_IDENTITY=device-login"
  "AGENT_SECRETS_PROJECTS=$project"
)

# pane: what the Oh My Pi session's tmux pane shows, with its history.
pane() { tmux -S "$tmux_socket" capture-pane -p -J -S -3000 -t outside-e2e 2>/dev/null; }

# probe COMMAND [ARGS...]: outside-agent-probe.ts as the probe's own session, its one JSON line.
probe() {
  env -i "${session_env[@]}" OUTSIDE_LISTENER_URL="$listener" OUTSIDE_DISPATCH_URL="$dispatch" \
    OUTSIDE_PROBE_SESSION="${probe_as:-$probe_session}" \
    bun "$here/outside-agent-probe.ts" "$@" 2>>"$work/probe.log" </dev/null
}

# json_field NAME LINE: the value of "NAME": in LINE, one compact JSON object the probe printed:
# a string's text, or a number, true, false or null as written; nothing when LINE has no NAME.
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

# unknown_of LINE: the probe's `unknown` text, or nothing when it read an answer.
unknown_of() { json_field unknown "$1"; }

now() { date -u '+%s'; }

# --- P1: the plugin, installed from npm as the outside person installs it. ---
plugin_dir="$home/.omp/plugins/node_modules/@sjawhar/pi-envoy"
target=${OUTSIDE_PI_ENVOY_PACKAGE:-}
version=${OUTSIDE_PI_ENVOY_VERSION:-}
want1="@sjawhar/pi-envoy ${version:-<OUTSIDE_PI_ENVOY_VERSION>} installed by omp, under section (c)'s settings alone"
if [ -z "$target" ] && [ -z "$version" ]; then
  row P1 "$want1" "OUTSIDE_PI_ENVOY_VERSION is unset" UNKNOWN
elif [ -z "$work" ]; then
  row P1 "$want1" "mktemp -d made no scratch directory" UNKNOWN
elif problem=$(tool_problem omp bun) && [ -n "$problem" ]; then
  row P1 "$want1" "$problem" UNKNOWN
else
  [ -n "$target" ] || target="@sjawhar/pi-envoy@$version"
  say "P1: omp plugin install $target"
  env -i "${session_env[@]}" omp plugin install "$target" >"$work/p1.log" 2>&1 </dev/null
  installed=$?
  got=''
  if [ -f "$plugin_dir/package.json" ]; then
    got=$(json_field version "$(tr -d ' \n' <"$plugin_dir/package.json")")
  fi
  if [ "$installed" -ne 0 ] || [ -z "$got" ]; then
    last=$(tr '\n' ' ' <"$work/p1.log")
    row P1 "$want1" "omp plugin install $target exited $installed, no package.json read: ${last:0:300}" UNKNOWN
  elif [ -n "$version" ] && [ "$got" != "$version" ]; then
    row P1 "$want1" "installed $got" FAIL
  else
    row P1 "$want1" "installed $got from $target" PASS
  fi
fi

# --- P2: the device login, approved by the test person's browser. ---
want2="dispatch login --device --person ${person:-<OUTSIDE_PERSON>} prints a code, the approve command approves it, the login exits 0 and keeps ~/.config/legion/device-login.json mode 0600"
login_file="$home/.config/legion/device-login.json"
problem=$(lacking OUTSIDE_BROKER_URL OUTSIDE_PERSON OUTSIDE_APPROVE_COMMAND)
if [ -n "$problem" ]; then
  row P2 "$want2" "$problem" UNKNOWN
elif ! passed P1; then
  row P2 "$want2" "not run: P1 did not pass, so the plugin's dispatch command is not installed" UNKNOWN
else
  say "P2: dispatch login --device --person $person"
  (
    env -i "${session_env[@]}" "$plugin_dir/bin/dispatch" login --device --person "$person" \
      >"$work/p2.log" 2>&1 </dev/null
    echo "$?" >"$work/p2.exit"
  ) &
  background="$background $!"
  code=''
  deadline=$(($(now) + 60))
  while [ -z "$code" ] && [ "$(now)" -lt "$deadline" ] && [ ! -f "$work/p2.exit" ]; do
    while IFS= read -r line; do
      case $line in *'and enter the code '*) code=${line##*and enter the code } ;; esac
    done <"$work/p2.log"
    [ -n "$code" ] || sleep 1
  done
  if [ -z "$code" ]; then
    last=$(tr '\n' ' ' <"$work/p2.log")
    row P2 "$want2" "dispatch login printed no code within 60 s: ${last:0:300}" UNKNOWN
  else
    set -f
    # shellcheck disable=SC2086 # a command line, split on blanks on purpose
    set -- $OUTSIDE_APPROVE_COMMAND
    set +f
    "$@" "$code" >"$work/p2.approve" 2>&1 </dev/null
    approved=$?
    p2_code=$code
    answer=$(tr '\n' ' ' <"$work/p2.approve")
    deadline=$(($(now) + 300))
    while [ ! -f "$work/p2.exit" ] && [ "$(now)" -lt "$deadline" ]; do sleep 1; done
    exited=$(cat "$work/p2.exit" 2>/dev/null)
    mode=''
    # The mode as ls prints it: stat's flags differ between GNU and BSD, and macOS ships BSD's.
    # shellcheck disable=SC2012 # one file this run named, so no name can confuse the read
    [ ! -f "$login_file" ] || mode=$(ls -ln "$login_file" | { read -r perms _ && printf '%s' "$perms"; })
    case $approved in
      0) ;;
      1) row P2 "$want2" "the approve command refused code $code: ${answer:0:300}" FAIL ;;
      *) row P2 "$want2" "the approve command could not tell (exit $approved): ${answer:0:300}" UNKNOWN ;;
    esac
    if [ "$approved" -eq 0 ]; then
      case $answer in *"APPROVED $code"*) ;; *)
        row P2 "$want2" "the approve command exited 0 without printing APPROVED $code: ${answer:0:300}" UNKNOWN
        ;;
      esac
    fi
    if [ -z "${verdict_P2:-}" ]; then
      if [ -z "$exited" ]; then
        row P2 "$want2" "code $code approved; dispatch login had not ended 300 s later" UNKNOWN
      elif [ "$exited" -ne 0 ]; then
        last=$(tr '\n' ' ' <"$work/p2.log")
        row P2 "$want2" "code $code approved; dispatch login exited $exited: ${last:0:300}" FAIL
      elif [ "$mode" != -rw------- ]; then
        row P2 "$want2" "code $code approved, login exit 0, device-login.json mode ${mode:-absent}" FAIL
      else
        row P2 "$want2" "code $code approved, login exit 0, device-login.json mode $mode" PASS
      fi
    fi
  fi
fi

# probe_problem: why the probe cannot run, or nothing: it needs P2's device login, and bun.
probe_problem() {
  if ! passed P2; then
    printf 'not run: P2 did not pass, so there is no device login to trade'
  else
    tool_problem bun
  fi
}

# --- P3: Dispatch's whoami names the person, the project and no scope. ---
person_lower=$(printf '%s' "$person" | tr '[:upper:]' '[:lower:]')
want3="GET /api/v1/whoami: kind agent, owner ${person_lower:-<OUTSIDE_PERSON>}, project ${project:-<OUTSIDE_PROJECT>}, scopes []"
problem=$(lacking OUTSIDE_DISPATCH_URL OUTSIDE_PROJECT)
[ -n "$problem" ] || problem=$(probe_problem)
if [ -n "$problem" ]; then
  row P3 "$want3" "$problem" UNKNOWN
else
  answer=$(probe whoami)
  if [ -n "$(unknown_of "$answer")" ] || [ "$(json_field status "$answer")" != 200 ]; then
    row P3 "$want3" "${answer:0:400}" UNKNOWN
  else
    case $answer in
      *'"kind":"agent"'*'"owner":"'"$person_lower"'"'*'"project":"'"$project"'"'*'"scopes":[]'*)
        row P3 "$want3" "$answer" PASS
        ;;
      *) row P3 "$want3" "$answer" FAIL ;;
    esac
  fi
fi

# --- P5's event wait starts now, in the background, so its 15 minutes overlap P4 and P8. ---
owner=${repository%%/*}
name=${repository#*/}
sanitize() {
  local value=$1
  value=${value//./_}
  value=${value// /_}
  value=${value//\*/_}
  value=${value//>/_}
  printf '%s' "$value"
}
admitted_topic="notifications.github.$(sanitize "$owner").$(sanitize "$name").>"
[ -n "$repository" ] || admitted_topic="notifications.github.<OUTSIDE_REPOSITORY>.>"
problem5=$(lacking OUTSIDE_LISTENER_URL OUTSIDE_PROJECT OUTSIDE_REPOSITORY)
[ -n "$problem5" ] || problem5=$(probe_problem)
case $repository in */*) ;; '') ;; *) problem5="${problem5:+$problem5; }OUTSIDE_REPOSITORY is not owner/name: $repository" ;; esac
if [ -z "$problem5" ]; then
  say "P5: waiting up to $event_seconds s for an event of $admitted_topic"
  (probe_as="outside-e2e-events-$nonce" probe github-event "$admitted_topic" "$event_seconds" >"$work/p5.event") &
  event_pid=$!
  background="$background $event_pid"
fi

# --- P4: an Oh My Pi session at the edge, its relay socket, and a message each way. ---
# The session receives through its own relay socket, and its pane shows what the peer sends it.
# Its two sends are scripted: outside-agent-probe.ts sends them as the session's own session id,
# through the same device login and the same edge, since this runner holds no model credential.
# The peer, a session of ours on a developer box that the tailnet leg runs, answers with its
# model: a ping with a pong and then a hello, and the reply to the hello with a done. Each message
# carries its sender's clock, `at <Unix ms>`, and a lag here is this runner's clock when the pane
# shows a message, less that stamp; the tailnet leg reads the peer's side the same way.
want4="the session at ${listener:-<OUTSIDE_LISTENER_URL>} pings ${peer:-<OUTSIDE_PEER_SESSION>}, and shows its pong and its hello within 10 s of the peer's sends; it replies to the hello, and shows the peer's done within 10 s"
problem=$(lacking OUTSIDE_LISTENER_URL OUTSIDE_PEER_SESSION OUTSIDE_PROJECT)
[ -n "$problem" ] || problem=$(tool_problem tmux omp)
[ -n "$problem" ] || problem=$(probe_problem)
session_id=''
p4_started=''
if [ -n "$problem" ]; then
  row P4 "$want4" "$problem" UNKNOWN
else
  tmux_socket="$work/tmux.sock"
  say "P4: starting omp in tmux (tmux -S $tmux_socket attach -t outside-e2e)"
  env -i "${session_env[@]}" tmux -S "$tmux_socket" new-session -d -s outside-e2e -x 220 -y 50 \
    -c "$session_dir" omp </dev/null
  p4_started=$(now)
  # p4_send TEXT: the session sends TEXT to the peer. False when it was not sent, with
  # `send_problem` saying why and `send_verdict` FAIL for an edge that refused the send, UNKNOWN
  # for a send that got no answer.
  send_problem=''
  send_verdict=UNKNOWN
  p4_send() {
    local answer status
    answer=$(probe_as=$session_id probe send "$peer" "$1")
    status=$(json_field status "$answer")
    case $status in
      2??) return 0 ;;
      4??) send_verdict=FAIL ;;
      *) send_verdict=UNKNOWN ;;
    esac
    send_problem="the send of '$1' as $session_id: ${answer:0:200}"
    return 1
  }
  # seen WORD: the peer's stamp on `outside-e2e WORD <nonce> at <ms>` once the pane shows it.
  seen() {
    local text stamped
    text=$(pane)
    case $text in *"outside-e2e $1 $nonce at "*) ;; *) return 1 ;; esac
    stamped=${text#*"outside-e2e $1 $nonce at "}
    stamped=${stamped%%[!0-9]*}
    printf '%s' "$stamped"
  }
  # wait_seen WORD SECONDS: the lag in whole seconds from the peer's stamp to this pane showing it.
  wait_seen() {
    local deadline stamped
    deadline=$(($(now) + $2))
    while [ "$(now)" -lt "$deadline" ]; do
      if stamped=$(seen "$1") && [ -n "$stamped" ]; then
        printf '%s' $(($(now) - stamped / 1000))
        return 0
      fi
      sleep 1
    done
    return 1
  }
  # The TUI names its directory once it runs; the session registers from then on.
  ready=''
  deadline=$(($(now) + 60))
  while [ -z "$ready" ] && [ "$(now)" -lt "$deadline" ]; do
    case $(pane) in *"outside-e2e-$nonce"*) ready=yes ;; *) sleep 1 ;; esac
  done
  # OMP_SKIP_SETUP keeps the setup wizard shut; any first-run screen a fresh HOME still opens (its
  # splash) is skipped as a person who skips it would: a setup screen with Escape, the splash with
  # Enter. One screen can follow another a moment later, so the leg goes on only after three
  # checks in a row, 2 s apart, show neither (at most 40 s).
  checks=0
  clear=0
  while [ "$checks" -lt 20 ] && [ "$clear" -lt 3 ]; do
    checks=$((checks + 1))
    case $(tmux -S "$tmux_socket" capture-pane -p -t outside-e2e 2>/dev/null) in
      *'exit setup'*)
        tmux -S "$tmux_socket" send-keys -t outside-e2e Escape
        clear=0
        ;;
      *'to skip'*)
        tmux -S "$tmux_socket" send-keys -t outside-e2e Enter
        clear=0
        ;;
      *) clear=$((clear + 1)) ;;
    esac
    sleep 2
  done
  # Whether the edge serves a relay socket at all, asked as the probe's own session so that the
  # Oh My Pi session's registration is left as it is. The session opens its socket before it
  # registers (pi-envoy's envoy.ts, `ENVOY_INBOUND=listener`), so with no socket it never registers,
  # and its registration is not waited for.
  served=$(json_field granted "$(probe relay-sub "notifications.envoy.deliver.$probe_session")")
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    [ "$served" = true ] || break
    session_id=$(json_field at_dir "$(probe sessions "$session_dir")")
    [ -z "$session_id" ] || [ "$session_id" = null ] || break
    session_id=''
    sleep 3
  done
  pong_lag=''
  hello_lag=''
  done_lag=''
  ping_at=''
  reply_at=''
  ping_sent=''
  reply_sent=''
  if [ -n "$ready" ] && [ -n "$session_id" ] && [ "$served" = true ]; then
    ping_at=$(stamp)
    if p4_send "outside-e2e ping $nonce at $ping_at"; then
      ping_sent=yes
      pong_lag=$(wait_seen pong 240) || pong_lag=''
      [ -z "$pong_lag" ] || { hello_lag=$(wait_seen hello 240) || hello_lag=''; }
      if [ -n "$hello_lag" ]; then
        reply_at=$(stamp)
        if p4_send "outside-e2e reply $nonce at $reply_at"; then
          reply_sent=yes
          done_lag=$(wait_seen 'done' 240) || done_lag=''
        fi
      fi
    fi
  fi
  # The tailnet leg reads these from the row to find the peer's side of the same exchange.
  marks="nonce=$nonce ping_at=${ping_at:-none} reply_at=${reply_at:-none}"
  if [ -z "$ready" ]; then
    row P4 "$want4" "the Oh My Pi TUI did not name its directory within 60 s" UNKNOWN
  elif [ "$served" = false ]; then
    row P4 "$want4" "the relay socket refused the probe's session its own relay subject" FAIL
  elif [ "$served" != true ]; then
    row P4 "$want4" "$listener serves no relay socket, which the Oh My Pi session opens before it registers" UNKNOWN
  elif [ -z "$session_id" ]; then
    shown=$(pane | tr -s ' \n' '  ')
    [ "${#shown}" -le 300 ] || shown=${shown:$((${#shown} - 300))}
    row P4 "$want4" "the Oh My Pi session is not on $listener's session list (GET /v1/sessions by its directory); its pane ends: $shown" UNKNOWN
  elif [ -z "$ping_sent" ]; then
    row P4 "$want4" "session $session_id registered; $send_problem; $marks" "$send_verdict"
  elif [ -z "$pong_lag" ]; then
    row P4 "$want4" "session $session_id: no pong from $peer shown within 240 s of the ping; $marks" FAIL
  elif [ -z "$hello_lag" ]; then
    row P4 "$want4" "session $session_id: pong shown ${pong_lag} s after the peer sent it; no hello within 240 s; $marks" FAIL
  elif [ -z "$reply_sent" ]; then
    row P4 "$want4" "session $session_id: pong and hello shown; $send_problem; $marks" "$send_verdict"
  elif [ -z "$done_lag" ]; then
    row P4 "$want4" "session $session_id: pong and hello shown; no done from $peer within 240 s of the reply; $marks" FAIL
  elif [ "$pong_lag" -gt 10 ] || [ "$hello_lag" -gt 10 ] || [ "$done_lag" -gt 10 ]; then
    row P4 "$want4" "session $session_id: pong ${pong_lag} s, hello ${hello_lag} s and done ${done_lag} s after the peer sent them; $marks" FAIL
  else
    row P4 "$want4" "session $session_id: pong ${pong_lag} s, hello ${hello_lag} s and done ${done_lag} s after the peer sent them; $marks" PASS
  fi
fi

# --- P5: the interests and the publish the edge refuses, and the one it admits. ---
want5="403 for $other_project's Dispatch topic; topic_not_in_project for slack, whatsapp and team; repository_not_in_project for $other_repository and every repository; topic_not_published for a publish of a ${project:-<OUTSIDE_PROJECT>} Dispatch event; $admitted_topic admitted and one of its events delivered within $event_seconds s"
if [ -n "$problem5" ]; then
  row P5 "$want5" "$problem5" UNKNOWN
else
  wrong=''
  unread=''
  # refused KIND TOPIC CODE: the probe's answer to KIND (interest or publish) of TOPIC is 403 and,
  # where CODE is given, names it.
  refused() {
    local answer status
    answer=$(probe "$1" "$2")
    status=$(json_field status "$answer")
    if [ -n "$(unknown_of "$answer")" ] || [ -z "$status" ]; then
      unread="${unread:+$unread; }$1 $2: ${answer:0:200}"
    elif [ "$status" != 403 ]; then
      wrong="${wrong:+$wrong; }$1 $2 answered $status"
    elif [ -n "$3" ]; then
      case $answer in *"$3"*) ;; *) wrong="${wrong:+$wrong; }$1 $2 answered 403 without $3: ${answer:0:200}" ;; esac
    fi
  }
  refused interest "notifications.dispatch.issue.$other_project.$other_project-1.>" ''
  refused interest 'notifications.slack.>' topic_not_in_project
  refused interest 'notifications.whatsapp.>' topic_not_in_project
  refused interest 'notifications.team.>' topic_not_in_project
  refused interest "notifications.github.$(sanitize "${other_repository%%/*}").$(sanitize "${other_repository#*/}").>" repository_not_in_project
  refused interest 'notifications.github.>' repository_not_in_project
  refused publish "notifications.dispatch.issue.$project.$project-1.comment.created" topic_not_published
  say "P5: waiting for the event probe"
  wait "$event_pid" 2>/dev/null
  event=$(cat "$work/p5.event" 2>/dev/null)
  status=$(json_field status "$event")
  delivered=$(json_field event "$event")
  if [ -n "$(unknown_of "$event")" ] || [ -z "$status" ]; then
    unread="${unread:+$unread; }the admitted interest: ${event:0:200}"
  elif [ "${status#2}" = "$status" ]; then
    wrong="${wrong:+$wrong; }$admitted_topic answered $status"
  elif [ "$delivered" = null ] || [ -z "$delivered" ]; then
    unread="${unread:+$unread; }$admitted_topic admitted, and no event of it arrived within $event_seconds s"
  fi
  if [ -n "$wrong" ]; then
    row P5 "$want5" "$wrong${unread:+; $unread}" FAIL
  elif [ -n "$unread" ]; then
    row P5 "$want5" "$unread" UNKNOWN
  else
    row P5 "$want5" "every refusal by its code; $admitted_topic admitted and its event $delivered delivered" PASS
  fi
fi

# --- P6: the session list holds no session outside the project; the peer's subject is refused. ---
foreign="$peer-foreign"
want6="GET /v1/sessions lists ${peer:-<OUTSIDE_PEER_SESSION>} (the control) and not ${peer:-<OUTSIDE_PEER_SESSION>}-foreign, a session outside ${project:-<OUTSIDE_PROJECT>}; a relay sub to notifications.envoy.deliver.${peer:-<OUTSIDE_PEER_SESSION>} is subject_not_granted"
problem=$(lacking OUTSIDE_LISTENER_URL OUTSIDE_PEER_SESSION)
[ -n "$problem" ] || problem=$(probe_problem)
if [ -n "$problem" ]; then
  row P6 "$want6" "$problem" UNKNOWN
else
  listed=$(probe sessions)
  socket=$(probe relay-sub "notifications.envoy.deliver.$peer")
  wrong=''
  unread=''
  if [ -n "$(unknown_of "$listed")" ] || [ "$(json_field status "$listed")" != 200 ]; then
    unread="GET /v1/sessions: ${listed:0:200}"
  else
    case $listed in *"\"$foreign\""*) wrong="the list holds $foreign" ;; esac
    case $listed in *"\"$peer\""*) ;; *) unread="the list does not hold $peer, so its absences prove nothing" ;; esac
  fi
  granted=$(json_field granted "$socket")
  if [ -n "$(unknown_of "$socket")" ]; then
    unread="${unread:+$unread; }the relay socket: ${socket:0:200}"
  elif [ -z "$granted" ]; then
    unread="${unread:+$unread; }the listener serves no relay socket (status $(json_field status "$socket"))"
  elif [ "$granted" = true ]; then
    wrong="${wrong:+$wrong; }a relay sub to $peer's subject was granted"
  else
    case $socket in *subject_not_granted*) ;; *) wrong="${wrong:+$wrong; }the sub was refused with $(json_field code "$socket")" ;; esac
  fi
  if [ -n "$wrong" ]; then
    row P6 "$want6" "$wrong${unread:+; $unread}" FAIL
  elif [ -n "$unread" ]; then
    row P6 "$want6" "$unread" UNKNOWN
  else
    row P6 "$want6" "$peer listed, $foreign not; the sub refused subject_not_granted" PASS
  fi
fi

# --- P7: the agent's token mints no agent token and approves nothing. ---
want7="POST /api/v1/me/agent-tokens and POST /api/v1/credential-requests/<id>/approve with the agent's token: 403 each"
problem=$(lacking OUTSIDE_DISPATCH_URL)
[ -n "$problem" ] || problem=$(probe_problem)
if [ -n "$problem" ]; then
  row P7 "$want7" "$problem" UNKNOWN
else
  wrong=''
  unread=''
  for call in agent-token approve; do
    answer=$(probe "$call")
    status=$(json_field status "$answer")
    if [ -n "$(unknown_of "$answer")" ] || [ -z "$status" ]; then
      unread="${unread:+$unread; }$call: ${answer:0:200}"
    else
      case $status in
        403) ;;
        401 | 5*) unread="${unread:+$unread; }$call answered $status, so the token was not read" ;;
        *) wrong="${wrong:+$wrong; }$call answered $status: ${answer:0:200}" ;;
      esac
    fi
  done
  if [ -n "$wrong" ]; then
    row P7 "$want7" "$wrong${unread:+; $unread}" FAIL
  elif [ -n "$unread" ]; then
    row P7 "$want7" "$unread" UNKNOWN
  else
    row P7 "$want7" "both 403" PASS
  fi
fi

# --- P8: a socket closes 4401 at its token's expiry, and the session reopens and receives. ---
want8="a relay socket closes 4401 within 25 s after its token's exp, at most 300 s after the trade; the Oh My Pi session reopens and shows a message sent after that"
problem=$(lacking OUTSIDE_LISTENER_URL)
[ -n "$problem" ] || problem=$(probe_problem)
if [ -n "$problem" ]; then
  row P8 "$want8" "$problem" UNKNOWN
elif ! passed P4; then
  row P8 "$want8" "not run: P4 did not pass, so there is no open session socket to watch" UNKNOWN
else
  say "P8: waiting for a token to expire (up to 300 s)"
  closed=$(probe_as="outside-e2e-expiry-$nonce" probe expiry 60 300)
  wrong=''
  unread=''
  traded=$(json_field traded_at "$closed")
  expires=$(json_field expires_at "$closed")
  closed_at=$(json_field closed_at "$closed")
  code=$(json_field code "$closed")
  case $traded$expires in *[!0-9]* | '') unread="the expiry probe: ${closed:0:200}" ;; esac
  if [ -z "$unread" ]; then
    if [ $((expires - traded)) -gt 301000 ]; then
      wrong="the token lasts $(((expires - traded) / 1000)) s from the trade, past 300 s"
    elif [ "$code" = null ]; then
      wrong="${wrong:+$wrong; }the socket was still open 60 s after its token's exp"
    elif [ "$code" != 4401 ] || [ $((closed_at - expires)) -lt 0 ] || [ $((closed_at - expires)) -gt 25000 ]; then
      wrong="${wrong:+$wrong; }closed $code $(((closed_at - expires) / 1000)) s after the token's exp"
    fi
  fi
  # Every token the Oh My Pi session traded at or after its socket opened has expired 325 s after,
  # and the socket with it; what it shows after that came through a socket it opened again.
  while [ "$(now)" -lt $((p4_started + 330)) ]; do sleep 5; done
  sent=$(probe send "$session_id" "outside-e2e after-reopen $nonce")
  sent_at=$(now)
  shown=''
  while [ -z "$shown" ] && [ "$(now)" -le $((sent_at + 10)) ]; do
    case $(pane) in *"outside-e2e after-reopen $nonce"*) shown=$(($(now) - sent_at)) ;; *) sleep 1 ;; esac
  done
  sent_status=$(json_field status "$sent")
  if [ -n "$(unknown_of "$sent")" ] || [ -z "$sent_status" ]; then
    unread="${unread:+$unread; }the send after the reopen: ${sent:0:200}"
  elif [ "${sent_status#2}" = "$sent_status" ]; then
    wrong="${wrong:+$wrong; }the send after the reopen answered $sent_status"
  elif [ -z "$shown" ]; then
    wrong="${wrong:+$wrong; }a message sent $(($(now) - p4_started)) s after the session opened was not shown within 10 s"
  fi
  if [ -n "$wrong" ]; then
    row P8 "$want8" "$wrong${unread:+; $unread}" FAIL
  elif [ -n "$unread" ]; then
    row P8 "$want8" "$unread" UNKNOWN
  else
    row P8 "$want8" "closed 4401 $(((closed_at - expires) / 1000)) s after exp, $(((expires - traded) / 1000)) s after the trade; a message after the reopen shown in $shown s" PASS
  fi
fi

# --- P9: the TCP reach check finds every internal address unreachable. ---
want9="outside-path-reach.sh from ${dispatch:-<OUTSIDE_DISPATCH_URL>}: PASS, every target unreachable"
problem=$(lacking OUTSIDE_DISPATCH_URL OUTSIDE_PATH_TARGETS)
if [ -n "$problem" ]; then
  row P9 "$want9" "$problem" UNKNOWN
elif [ ! -f "$here/outside-path-reach.sh" ]; then
  row P9 "$want9" "outside-path-reach.sh is not beside this file in $here" UNKNOWN
else
  reach=$(OUTSIDE_ORIGIN="$dispatch" OUTSIDE_PATH_TARGETS="$OUTSIDE_PATH_TARGETS" \
    OUTSIDE_PATH_ALLOW_HTTP="$allow_http" bash "$here/outside-path-reach.sh" 2>&1 </dev/null)
  status=$?
  printf '%s\n' "$reach" >&2
  last=${reach##*"$newline"}
  case $status in
    0) row P9 "$want9" "$last" PASS ;;
    1) row P9 "$want9" "$last" FAIL ;;
    *) row P9 "$want9" "exit $status: $last" UNKNOWN ;;
  esac
fi

# --- revocations: before the window, while the person's cookie still reads their own list, the
# workflow's OUTSIDE_REVOKE_COMMAND (a command line, split on blanks) revokes every device login
# the run approved but P2's, which it is told as OUTSIDE_EDGE_REVOKE_KEEP. After the owner's removal
# that cookie gets 401 (T5), so a revocation after the window could read nothing. P2's login stays
# live into the window, where the removal stops it (T5); its key lives only in this leg's scratch
# HOME, which the leg removes when it ends. ---
want_revoke="every device login the run approved but P2's revoked before the T5 window"
if [ -z "${OUTSIDE_REVOKE_COMMAND:-}" ]; then
  row revocations "$want_revoke" "OUTSIDE_REVOKE_COMMAND is unset" UNKNOWN
else
  set -f
  # shellcheck disable=SC2086 # a command line, split on blanks on purpose
  set -- $OUTSIDE_REVOKE_COMMAND
  set +f
  OUTSIDE_EDGE_REVOKE_KEEP=${p2_code:-} "$@" >"$work/revoke.log" 2>&1 </dev/null
  revoked=$?
  case $revoked in
    0) row revocations "$want_revoke" "the revoke command exited 0, keeping ${p2_code:-no code}: $(text_of "$work/revoke.log")" PASS ;;
    1) row revocations "$want_revoke" "the revoke command failed: $(text_of "$work/revoke.log")" FAIL ;;
    *) row revocations "$want_revoke" "the revoke command exited $revoked: $(text_of "$work/revoke.log")" UNKNOWN ;;
  esac
fi

# --- The T5 window: the owner removes the test person from the project's access list, and the
# person's live agent and browser stop. This leg opens the window by telling the peer, whose
# tailnet leg then has the owner make the removal and keeps its time; the tailnet leg judges T5
# from that time and the rows below, each of which carries the times it read (Unix seconds). For
# up to OUTSIDE_T5_SECONDS (default 1200) the leg polls a fresh trade of P2's device login every
# 10 s, and keeps a relay socket open with the newest token, opening another when one closes, as
# the agent would; the window ends once a trade is refused and the socket open then has closed.
# OUTSIDE_T5_COMMAND, the workflow's browser, runs beside it with OUTSIDE_T5_DEADLINE (Unix s) and
# OUTSIDE_T5_TSV: it polls the person's cookie until it gets 401 and then tries the passkey sign-in,
# writing rows T5-cookie and T5-passkey there, which are copied into this leg's TSV. ---
t5_seconds=${OUTSIDE_T5_SECONDS:-1200}
want_t5a="once the owner removes ${person:-<OUTSIDE_PERSON>}: a fresh trade of the device login is refused, and the relay socket open at that moment closes"
want_t5c="the person's cookie answered 401 after the removal"
want_t5p="the person's passkey sign-in refused after the removal"
problem=$(lacking OUTSIDE_LISTENER_URL OUTSIDE_PEER_SESSION)
[ -n "$problem" ] || problem=$(probe_problem)
if [ -n "$problem" ]; then
  row T5-agent "$want_t5a" "$problem" UNKNOWN
  row T5-cookie "$want_t5c" "not run: the window did not open: $problem" UNKNOWN
  row T5-passkey "$want_t5p" "not run: the window did not open: $problem" UNKNOWN
else
  opened=$(now)
  deadline=$((opened + t5_seconds))
  told=$(probe send "$peer" "outside-e2e t5-open $nonce at $(stamp)")
  say "T5: window open until $(date -u -d "@$deadline" '+%H:%M:%SZ' 2>/dev/null || echo "$deadline"): $told"
  t5_driver=''
  if [ -n "${OUTSIDE_T5_COMMAND:-}" ]; then
    : >"$work/t5.tsv"
    set -f
    # shellcheck disable=SC2086 # a command line, split on blanks on purpose
    set -- $OUTSIDE_T5_COMMAND
    set +f
    OUTSIDE_T5_DEADLINE=$deadline OUTSIDE_T5_TSV="$work/t5.tsv" "$@" >"$work/t5-driver.log" 2>&1 </dev/null &
    t5_driver=$!
    background="$background $t5_driver"
  fi
  refused_at=''
  refusal=''
  socket_pid=''
  socket_log=''
  sockets=0
  socket_unread=''
  while [ "$(now)" -lt "$deadline" ]; do
    answer=$(probe trade)
    if [ "$(json_field traded "$answer")" = false ]; then
      refused_at=$(now)
      refusal=$answer
      break
    fi
    # A socket that ended is opened again with a fresh token, as the agent's own would be.
    if [ -z "$socket_unread" ] && { [ -z "$socket_pid" ] || ! kill -0 "$socket_pid" 2>/dev/null; }; then
      sockets=$((sockets + 1))
      socket_log="$work/t5-socket-$sockets.log"
      probe_as="outside-e2e-t5-$nonce" probe hold "$t5_seconds" >"$socket_log" &
      socket_pid=$!
      background="$background $socket_pid"
      sleep 3
      case $(cat "$socket_log") in *'"unknown"'*) socket_unread=$(text_of "$socket_log") ;; esac
    fi
    sleep 10
  done
  # The socket open at the refusal: its close, within 325 s of it.
  closed_at=''
  close_code=''
  if [ -n "$refused_at" ] && [ -z "$socket_unread" ] && [ -n "$socket_pid" ]; then
    while kill -0 "$socket_pid" 2>/dev/null && [ "$(now)" -le $((refused_at + 325)) ]; do sleep 2; done
    held=$(cat "$socket_log")
    closed_at=$(json_field closed_at "$held")
    close_code=$(json_field code "$held")
    case $closed_at in '' | null) closed_at='' ;; *) closed_at=$((closed_at / 1000)) ;; esac
  fi
  marks="opened_at=$opened refused_at=${refused_at:-none} socket_closed_at=${closed_at:-none} code=${close_code:-none} sockets=$sockets"
  if [ -z "$refused_at" ]; then
    row T5-agent "$want_t5a" "no trade refused in the ${t5_seconds} s window, so no removal was seen; $marks" UNKNOWN
  elif [ -n "$socket_unread" ]; then
    row T5-agent "$want_t5a" "the trade was refused; the socket part was not read: $socket_unread; $marks" UNKNOWN
  elif [ -z "$closed_at" ]; then
    row T5-agent "$want_t5a" "the trade was refused, and the socket open then was still open 325 s later; $marks" FAIL
  else
    row T5-agent "$want_t5a" "the trade was refused (status $(json_field status "$refusal")) and the socket open then closed $close_code; $marks" PASS
  fi
  if [ -z "$t5_driver" ]; then
    row T5-cookie "$want_t5c" "OUTSIDE_T5_COMMAND is unset, so the browser was not read" UNKNOWN
    row T5-passkey "$want_t5p" "OUTSIDE_T5_COMMAND is unset, so the browser was not read" UNKNOWN
  else
    while kill -0 "$t5_driver" 2>/dev/null && [ "$(now)" -le $((deadline + 30)) ]; do sleep 2; done
    for name in T5-cookie T5-passkey; do
      line=''
      while IFS= read -r each; do
        case $each in "$name$tab"*) line=$each ;; esac
      done <"$work/t5.tsv"
      if [ -z "$line" ]; then
        want=$want_t5c
        [ "$name" = T5-cookie ] || want=$want_t5p
        row "$name" "$want" "OUTSIDE_T5_COMMAND wrote no $name row: $(text_of "$work/t5-driver.log")" UNKNOWN
        continue
      fi
      IFS="$tab" read -r _ want got verdict <<EOF
$line
EOF
      case $verdict in PASS | FAIL | UNKNOWN) row "$name" "$want" "$got" "$verdict" ;; *)
        row "$name" "$want" "the browser's row reads $verdict: $got" UNKNOWN ;;
      esac
    done
  fi
fi

# --- The verdict. ---
if [ "$fails" -gt 0 ]; then
  emit "verdict${tab}every row PASS${tab}$fails FAIL, $unknowns UNKNOWN${tab}FAIL"
  exit 1
elif [ "$unknowns" -gt 0 ]; then
  emit "verdict${tab}every row PASS${tab}$unknowns UNKNOWN${tab}UNKNOWN"
  exit 2
fi
emit "verdict${tab}every row PASS${tab}every row PASS${tab}PASS"
exit 0
