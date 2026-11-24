#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR
# The public leg of the outside agent's acceptance, rows P1 to P10 and the Codex leg's C0 to C7:
# what an outside person's agent does and is refused from the public internet, off the private
# network, through the three edge origins alone. A workflow on a runner on the public internet runs
# it after its own browser has signed the test person in. outside-agent-acceptance.md, beside this
# file, says what each row reads, how the tailnet leg starts this one, and the gate both legs make.
#
# It is written for bash 3.2 (as macOS ships it) and later, and sources outside-agent-lib.sh from
# beside it, what both legs share. It drives what the outside person installs and runs: Oh My Pi
# (`omp`) in tmux, as packages/pi-envoy/scripts/smoke-delivery.sh runs it, the `dispatch` command
# the plugin ships, and curl; and, after P10, outside-agent-codex.sh beside it, which runs the
# Codex CLI with the codex-envoy plugin under P2's device login (rows C0-C7). Calls that carry the
# agent's broker credential, which only the holder of its key can sign, go through
# outside-agent-probe.ts beside this file, run with bun.
#
# Every process it starts runs with `env -i` and section (c)'s settings alone, under a HOME of
# its own in a `mktemp -d` directory, so nothing of the machine it runs on reaches them:
#   ENVOY_URL=<OUTSIDE_LISTENER_URL> ENVOY_INBOUND=listener DISPATCH_URL=<OUTSIDE_DISPATCH_URL>
#   DISPATCH_TOKEN_BROKER=1 AGENT_SECRETS_URL=<OUTSIDE_BROKER_URL>
#   AGENT_SECRETS_MACHINE_IDENTITY=device-login AGENT_SECRETS_PROJECTS=<OUTSIDE_PROJECT>
# beside PATH, HOME, TERM, TMPDIR and OMP_SKIP_SETUP=1, Oh My Pi's own switch that skips its
# first-run setup wizard, which a fresh HOME would otherwise open. No model credential,
# ENVOY_NATS_URL, ENVOY_TOKEN or DISPATCH_TOKEN reaches any of them: the Oh My Pi session runs on
# the probe's keyless scripted model at 127.0.0.1 (P4).
#
# Inputs, each named by the row that cannot run without it:
#   OUTSIDE_DISPATCH_URL, OUTSIDE_BROKER_URL, OUTSIDE_LISTENER_URL
#                             the three edge origins, https://<host>; http://127.0.0.1:<port>
#                             only with OUTSIDE_ALLOW_HTTP=1, for a local stack and this file's
#                             tests.
#   OUTSIDE_PROJECT           the scratch project's key.
#   OUTSIDE_PERSON            the test person's address.
#   OUTSIDE_PEER_SESSION      our peer session, which the tailnet leg runs.
#   OUTSIDE_APPROVE_COMMAND   a command line, split on blanks, that approves one device login's
#                             code as the test person: run with the code as its last word, it
#                             prints `APPROVED <code>` and exits 0, or exits 1 (refused) or 2
#                             (could not tell).
#   OUTSIDE_PI_ENVOY_VERSION  the @sjawhar/pi-envoy release P1 installs; OUTSIDE_PI_ENVOY_PACKAGE
#                             names a package path or tarball instead, for a local stack.
#   OUTSIDE_REPOSITORY        owner/name of the repository the scratch project's issue resolves
#                             to, whose events P5 must be admitted to.
#   OUTSIDE_PATH_TARGETS      the target list outside-path-reach.sh reads (P9).
#   OUTSIDE_REVOKE_COMMAND    the workflow's revoke step (row revocations), and OUTSIDE_T5_COMMAND
#                             its browser in the T5 window (rows T5-cookie and T5-passkey).
#   OUTSIDE_INSIDE_DISPATCH_URL
#                             Dispatch's private origin, which C2's control points the Codex
#                             plugin's DISPATCH_URL at (outside-agent-codex.sh). No default: the
#                             workflow passes it as it passes the edge origins; without it C2 reads
#                             UNKNOWN.
#   OUTSIDE_CODEX_ENVOY_VERSION, OUTSIDE_CODEX_ENVOY_COMMIT, OUTSIDE_DEVICE_LOGIN_COMMIT
#                             the exact @sjawhar/codex-envoy release the Codex leg installs from npm,
#                             the commit its provenance must name, and the device login's merge
#                             commit that commit must hold (row C0). Without the version every C row
#                             reads UNKNOWN; without either commit, C0 does.
# Optional: OUTSIDE_CODEX_ENVOY_SOURCE=checkout (a development run of the Codex leg, whose C0 reads
# UNKNOWN); OUTSIDE_OTHER_PROJECT (default EXAMPLE) and OUTSIDE_OTHER_REPOSITORY (default
# example-owner/example-repo), the project and repository P5 must be refused;
# OUTSIDE_AGENT_PUBLIC_TSV, a file that gets a copy of the TSV; OUTSIDE_GITHUB_EVENT_SECONDS
# (default 900), how long P5 waits for an event; OUTSIDE_T5_SECONDS (default 1200), the longest
# the T5 window stays open.
#
# Output: on stdout a TSV, the header `row want got verdict`, one line per row P1 to P10, the
# Codex leg's C0 to C7, the rows revocations, T5-agent, T5-cookie and T5-passkey, and a last
# `verdict` line. Progress goes to
# stderr. A row reads PASS, FAIL or UNKNOWN: UNKNOWN when it could not run, its `got` naming what
# it could not read, never PASS. A row quotes no output of an operator's command, which may print
# a cookie, a code or an invite token: only its exit status and a fixed phrase. Exit 0 when every
# row passed, 1 when any failed, 2 otherwise. A row that needs another row's result reads UNKNOWN
# when that row did not pass.
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
if [ ! -r "$here/outside-agent-lib.sh" ]; then
  echo "UNKNOWN: outside-agent-lib.sh is not beside this file in $here"
  exit 2
fi
# shellcheck source=outside-agent-lib.sh
. "$here/outside-agent-lib.sh"
tab=$(printf '\t')
newline='
'
allow_http=${OUTSIDE_ALLOW_HTTP:-}

tsv=${OUTSIDE_AGENT_PUBLIC_TSV:-}
if [ -n "$tsv" ]; then
  : >"$tsv" || tsv=''
fi

# emit LINE: one line of the TSV, on stdout and in OUTSIDE_AGENT_PUBLIC_TSV.
emit() {
  printf '%s\n' "$1"
  [ -z "$tsv" ] || printf '%s\n' "$1" >>"$tsv"
}

fails=0
unknowns=0
# row NAME WANT GOT VERDICT: prints the row and keeps its verdict as verdict_<NAME>, a dash in
# NAME read as an underscore. A verdict that is none of PASS, FAIL and UNKNOWN counts as UNKNOWN.
row() {
  emit "$1$tab$(cell "$2")$tab$(cell "$3")$tab$4"
  printf -v "verdict_${1//-/_}" '%s' "$4"
  case $4 in
    PASS) ;;
    FAIL) fails=$((fails + 1)) ;;
    *) unknowns=$((unknowns + 1)) ;;
  esac
}
passed() {
  local name="verdict_${1//-/_}"
  [ "${!name:-}" = PASS ]
}
say() { printf 'outside-agent-public: %s\n' "$1" >&2; }
# stamp: this runner's clock in Unix ms, to the second, which bash 3.2's date can give.
stamp() { printf '%s000' "$(date -u '+%s')"; }

emit "row${tab}want${tab}got${tab}verdict"

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
[ "$other_repository" != "$repository" ] || other_repository="${repository}-other"
event_seconds=${OUTSIDE_GITHUB_EVENT_SECONDS:-900}

# --- The scratch directory, its HOME and its cleanup: every background child, stopped and waited
# for, then the Oh My Pi session's tmux server, then the directory, which the session writes under
# its HOME as it exits. ---
work=''
tmux_socket=''
background=''
# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  reap
  [ -z "$tmux_socket" ] || stop_tmux "$tmux_socket"
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
# probe_exec COMMAND [ARGS...]: probe for a background child. Its last word is exec, so `$!` of
# `probe_exec ... &` names outside-agent-probe.ts itself, which reap stops.
probe_exec() {
  exec env -i "${session_env[@]}" OUTSIDE_LISTENER_URL="$listener" OUTSIDE_DISPATCH_URL="$dispatch" \
    OUTSIDE_PROBE_SESSION="${probe_as:-$probe_session}" \
    bun "$here/outside-agent-probe.ts" "$@" 2>>"$work/probe.log" </dev/null
}

# unknown_of LINE: the probe's `unknown` text, or nothing when it read an answer.
unknown_of() { json_field unknown "$1"; }

# refused KIND TOPIC CODE: the probe's answer to KIND (interest or publish) of TOPIC, as the session
# `probe_as` names (the probe's own by default), is 403 and, where CODE is given, its `code` is CODE.
# A 5xx is the edge not deciding (repositories_unknown), neither a refusal nor an admission, so it
# is unread, as is no readable answer. Adds to the row's `wrong` or `unread`.
refused() {
  local answer status code
  answer=$(probe "$1" "$2")
  status=$(json_field status "$answer")
  code=$(json_field code "$answer")
  if [ -n "$(unknown_of "$answer")" ] || [ -z "$status" ]; then
    unread="${unread:+$unread; }$1 $2: ${answer:0:200}"
  elif [ "${status#5}" != "$status" ]; then
    unread="${unread:+$unread; }$1 $2 answered $status, so the edge could not decide it: ${answer:0:200}"
  elif [ "$status" != 403 ]; then
    wrong="${wrong:+$wrong; }$1 $2 answered $status"
  elif [ -n "$3" ] && [ "$code" != "$3" ]; then
    wrong="${wrong:+$wrong; }$1 $2 answered 403 ${code:-with no code}, not $3"
  fi
}

# sub_refused LINE WHAT: the probe's relay-socket `sub` answer LINE, for the subject WHAT names, is
# the edge's refusal, subject_not_granted. Sets `sub` (refused, granted, unread, or the other code)
# and adds to the row's `wrong` or `unread`: a granted sub, or another code, is wrong.
sub_refused() {
  local granted
  granted=$(json_field granted "$1")
  if [ -n "$(unknown_of "$1")" ]; then
    sub=unread
    unread="${unread:+$unread; }the relay sub of $2: ${1:0:200}"
  elif [ -z "$granted" ]; then
    sub=unread
    unread="${unread:+$unread; }the listener serves no relay socket (status $(json_field status "$1"))"
  elif [ "$granted" = true ]; then
    sub=granted
    wrong="${wrong:+$wrong; }a relay sub of $2 was granted"
  else
    case $1 in
      *subject_not_granted*) sub=refused ;;
      *)
        sub=$(json_field code "$1")
        wrong="${wrong:+$wrong; }the relay sub of $2 was refused with ${sub:-no code}"
        ;;
    esac
  fi
}

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
    row P1 "$want1" "omp plugin install $target exited $installed, and no package.json was read" UNKNOWN
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
  : >"$work/p2.log"
  env -i "${session_env[@]}" "$plugin_dir/bin/dispatch" login --device --person "$person" \
    >"$work/p2.log" 2>&1 </dev/null &
  p2_pid=$!
  background="$background $p2_pid"
  code=''
  p2_ended=''
  deadline=$(($(now) + 60))
  # Whether the login still runs is read before its log: a login that has ended has written all it
  # printed, so a code it printed as it ended is still read, and the wait ends on that read.
  while [ -z "$code" ] && [ -z "$p2_ended" ] && [ "$(now)" -lt "$deadline" ]; do
    running_child "$p2_pid" || p2_ended=yes
    while IFS= read -r line; do
      case $line in *'and enter the code '*) code=${line##*and enter the code } ;; esac
    done <"$work/p2.log"
    [ -n "$code" ] || [ -n "$p2_ended" ] || pause
  done
  if [ -z "$code" ]; then
    last=$(tr '\n' ' ' <"$work/p2.log")
    if [ -n "$p2_ended" ]; then
      row P2 "$want2" "dispatch login ended and printed no code: ${last:0:300}" UNKNOWN
    else
      row P2 "$want2" "dispatch login printed no code within 60 s: ${last:0:300}" UNKNOWN
    fi
  else
    set -f
    # shellcheck disable=SC2086 # a command line, split on blanks on purpose
    set -- $OUTSIDE_APPROVE_COMMAND
    set +f
    "$@" "$code" >"$work/p2.approve" 2>&1 </dev/null
    approved=$?
    p2_code=$code
    answer=$(tr '\n' ' ' <"$work/p2.approve")
    await_child "$p2_pid" 300
    exited=$child_exit
    mode=''
    # The mode as ls prints it: stat's flags differ between GNU and BSD, and macOS ships BSD's.
    # shellcheck disable=SC2012 # one file this run named, so no name can confuse the read
    [ ! -f "$login_file" ] || mode=$(ls -ln "$login_file" | { read -r perms _ && printf '%s' "$perms"; })
    case $approved in
      0) ;;
      1) row P2 "$want2" "the approve command refused code $code (exit 1)" FAIL ;;
      *) row P2 "$want2" "the approve command could not tell (exit $approved)" UNKNOWN ;;
    esac
    if [ "$approved" -eq 0 ]; then
      case $answer in *"APPROVED $code"*) ;; *)
        row P2 "$want2" "the approve command exited 0 without printing APPROVED $code" UNKNOWN
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

# --- P5's event wait starts now, in the background, so its 15 minutes overlap P4 and P8. The probe
# trades a fresh token and opens its socket again whenever the edge closes one, as a session's
# client does, so the wait holds for the whole time and not for one token's life. ---
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
  probe_as="outside-e2e-events-$nonce" probe_exec github-event "$admitted_topic" "$event_seconds" >"$work/p5.event" &
  event_pid=$!
  background="$background $event_pid"
fi

# --- P4: an Oh My Pi session at the edge, its relay socket, and a message each way. ---
# The session receives through its own relay socket, and its transcript holds what the peer sends
# it. Its two sends are scripted: outside-agent-probe.ts sends them as the session's own session
# id, through the same device login and the same edge, since this runner holds no model
# credential. The session still needs a model, since an Envoy card is handed to it as a turn and a
# session with no model refuses the card ("Extension sendMessage failed: No model configured"),
# holding nothing; so it runs on the probe's keyless scripted model (`outside-agent-probe.ts
# model`) at 127.0.0.1, which answers every turn with one fixed line. The peer, a session of ours on
# a developer box that the tailnet leg runs, answers with its model: a ping with a pong and then a
# hello, and the reply to the hello with a done. A lag here is this runner's clock when the
# session holds a message, less the time its sender sent it, the card's envelope `at`; the tailnet
# leg reads the peer's side by the stamps this leg's messages carry, `at <Unix ms>`.
want4="the session at ${listener:-<OUTSIDE_LISTENER_URL>} pings ${peer:-<OUTSIDE_PEER_SESSION>}, and holds its pong and its hello within 10 s of the peer's sends; it replies to the hello, and holds the peer's done within 10 s"
problem=$(lacking OUTSIDE_LISTENER_URL OUTSIDE_PEER_SESSION OUTSIDE_PROJECT)
[ -n "$problem" ] || problem=$(tool_problem tmux omp)
[ -n "$problem" ] || problem=$(probe_problem)
session_id=''
session_seen_at=''
model_port=''
if [ -z "$problem" ]; then
  env -i "${session_env[@]}" bun "$here/outside-agent-probe.ts" model >"$work/model.log" 2>&1 </dev/null &
  background="$background $!"
  deadline=$(($(now) + 30))
  while [ -z "$model_port" ] && [ "$(now)" -lt "$deadline" ]; do
    model_port=$(json_field port "$(cat "$work/model.log")")
    [ -n "$model_port" ] || pause
  done
  if [ -n "$model_port" ]; then
    mkdir -p "$home/.omp/agent"
    printf '%s\n' 'providers:' '  outside-e2e:' "    baseUrl: http://127.0.0.1:$model_port/v1" \
      '    auth: none' '    api: openai-completions' '    models:' '      - id: scripted' \
      '        name: outside-e2e scripted reply' '        reasoning: false' '        input: [text]' \
      '        contextWindow: 128000' '        maxTokens: 1024' >"$home/.omp/agent/models.yml"
  else
    problem="the scripted model did not listen within 30 s: $(text_of "$work/model.log")"
  fi
fi
if [ -n "$problem" ]; then
  row P4 "$want4" "$problem" UNKNOWN
else
  tmux_socket="$work/tmux.sock"
  say "P4: starting omp in tmux (tmux -S $tmux_socket attach -t outside-e2e)"
  env -i "${session_env[@]}" tmux -S "$tmux_socket" new-session -d -s outside-e2e -x 220 -y 50 \
    -c "$session_dir" omp --model outside-e2e/scripted --session-dir "$work/omp-sessions" </dev/null
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
  # shown_text: every message the session holds, read from its transcript (its session directory
  # is $work/omp-sessions), since the TUI folds an Envoy card to its header and the pane never
  # shows the card's text. A card is in the transcript once the host takes it into the session.
  shown_text() { cat "$work"/omp-sessions/*.jsonl "$work"/omp-sessions/*/*.jsonl 2>/dev/null; }
  # seen WORD: once the session holds `outside-e2e WORD <nonce>`, the time in Unix ms its sender
  # sent it: the card's envelope `at` (whole seconds), which the sender's Envoy client sets as it
  # sends; `unread` for a card held with no envelope `at`; false while the session holds none.
  seen() {
    local text line sent
    text=$(shown_text)
    case $text in *"outside-e2e $1 $nonce"*) ;; *) return 1 ;; esac
    while IFS= read -r line; do
      case $line in *"outside-e2e $1 $nonce"*) break ;; esac
    done <<EOF
$text
EOF
    if [[ $line =~ at:\ \\?\"([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z) ]] &&
      sent=$(iso_seconds "${BASH_REMATCH[1]}") && [ -n "$sent" ]; then
      printf '%s' "$((sent * 1000))"
    else
      printf unread
    fi
  }
  # wait_seen WORD SECONDS: the lag in whole seconds from the peer's send to the session holding
  # it, or `unread`; false when the session held none within SECONDS.
  wait_seen() {
    local deadline stamped
    deadline=$(($(now) + $2))
    while [ "$(now)" -lt "$deadline" ]; do
      if stamped=$(seen "$1"); then
        case $stamped in
          unread) printf unread ;;
          *) printf '%s' $(($(now) - stamped / 1000)) ;;
        esac
        return 0
      fi
      pause
    done
    return 1
  }
  # The TUI names its directory once it runs; the session registers from then on.
  ready=''
  deadline=$(($(now) + 60))
  while [ -z "$ready" ] && [ "$(now)" -lt "$deadline" ]; do
    case $(pane) in *"outside-e2e-$nonce"*) ready=yes ;; *) pause ;; esac
  done
  omp_clear_screens "$tmux_socket" outside-e2e
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
  # When the session is first read on the list: its socket opened before it registered, so every
  # token that socket rode was traded before this (P8).
  [ -z "$session_id" ] || session_seen_at=$(now)
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
    row P4 "$want4" "session $session_id: no pong from $peer held within 240 s of the ping; $marks" FAIL
  elif [ -z "$hello_lag" ]; then
    row P4 "$want4" "session $session_id: pong held ${pong_lag} s after the peer sent it; no hello within 240 s; $marks" FAIL
  elif [ -z "$reply_sent" ]; then
    row P4 "$want4" "session $session_id: pong and hello held; $send_problem; $marks" "$send_verdict"
  elif [ -z "$done_lag" ]; then
    row P4 "$want4" "session $session_id: pong and hello held; no done from $peer within 240 s of the reply; $marks" FAIL
  elif [ "$pong_lag" = unread ] || [ "$hello_lag" = unread ] || [ "$done_lag" = unread ]; then
    row P4 "$want4" "session $session_id: pong, hello and done held, and the transcript gave no send time for one of them (pong $pong_lag, hello $hello_lag, done $done_lag); $marks" UNKNOWN
  elif [ "$pong_lag" -gt 10 ] || [ "$hello_lag" -gt 10 ] || [ "$done_lag" -gt 10 ]; then
    row P4 "$want4" "session $session_id: pong ${pong_lag} s, hello ${hello_lag} s and done ${done_lag} s after the peer sent them; $marks" FAIL
  elif [ "$pong_lag" -lt -2 ] || [ "$hello_lag" -lt -2 ] || [ "$done_lag" -lt -2 ]; then
    # Held before it was sent, by more than the 2 s whole seconds round off: the peer's clock runs
    # ahead of this runner's, so a lag within 10 s says nothing.
    row P4 "$want4" "session $session_id: pong ${pong_lag} s, hello ${hello_lag} s and done ${done_lag} s after the peer sent them, one before it was sent, so the two clocks disagree; $marks" UNKNOWN
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
  refused interest "notifications.dispatch.issue.$other_project.$other_project-1.>" ''
  refused interest 'notifications.slack.>' topic_not_in_project
  refused interest 'notifications.whatsapp.>' topic_not_in_project
  refused interest 'notifications.team.>' topic_not_in_project
  refused interest "notifications.github.$(sanitize "${other_repository%%/*}").$(sanitize "${other_repository#*/}").>" repository_not_in_project
  refused interest 'notifications.github.>' repository_not_in_project
  refused publish "notifications.dispatch.issue.$project.$project-1.comment.created" topic_not_published
  say "P5: waiting for the event probe"
  await_child "$event_pid" $((event_seconds + 120))
  event=$(cat "$work/p5.event" 2>/dev/null)
  status=$(json_field status "$event")
  delivered=$(json_field event "$event")
  sockets=$(json_field sockets "$event")
  if [ -n "$(unknown_of "$event")" ] || [ -z "$status" ]; then
    unread="${unread:+$unread; }the admitted interest: ${event:0:200}"
  elif [ "${status#5}" != "$status" ]; then
    unread="${unread:+$unread; }$admitted_topic answered $status, so the edge could not decide it: ${event:0:200}"
  elif [ "${status#2}" = "$status" ]; then
    wrong="${wrong:+$wrong; }$admitted_topic answered $status"
  elif [ "$delivered" = null ] || [ -z "$delivered" ]; then
    unread="${unread:+$unread; }$admitted_topic admitted, and no event of it arrived within $event_seconds s (${sockets:-?} sockets, each opened again on a fresh token when the edge closed the one before)"
  fi
  if [ -n "$wrong" ]; then
    row P5 "$want5" "$wrong${unread:+; $unread}" FAIL
  elif [ -n "$unread" ]; then
    row P5 "$want5" "$unread" UNKNOWN
  else
    row P5 "$want5" "every refusal by its code; $admitted_topic admitted and its event $delivered delivered (socket ${sockets:-?})" PASS
  fi
fi

# --- P6: the session list holds no session outside the project, a send to one is answered as to a
# session no listener knows, and the peer's subject is refused. The got ends with marks the
# tailnet leg reads, each part's reading: peer_listed (the control) and foreign_listed yes, no or
# unread; foreign_send refused (404), accepted, unread or the status it answered; sub refused,
# granted, unread or the code it was refused with. ---
foreign="$peer-foreign"
want6="GET /v1/sessions lists ${peer:-<OUTSIDE_PEER_SESSION>} (the control) and not ${peer:-<OUTSIDE_PEER_SESSION>}-foreign, a session outside ${project:-<OUTSIDE_PROJECT>}; a send to that session answered 404, as to a session no listener knows; a relay sub to notifications.envoy.deliver.${peer:-<OUTSIDE_PEER_SESSION>} is subject_not_granted"
problem=$(lacking OUTSIDE_LISTENER_URL OUTSIDE_PEER_SESSION)
[ -n "$problem" ] || problem=$(probe_problem)
if [ -n "$problem" ]; then
  row P6 "$want6" "$problem" UNKNOWN
else
  listed=$(probe sessions)
  socket=$(probe relay-sub "notifications.envoy.deliver.$peer")
  sent=$(probe send "$foreign" "outside-e2e foreign-send $nonce")
  wrong=''
  unread=''
  peer_listed=unread
  foreign_listed=unread
  if [ -n "$(unknown_of "$listed")" ] || [ "$(json_field status "$listed")" != 200 ]; then
    unread="GET /v1/sessions: ${listed:0:200}"
  else
    foreign_listed=no
    peer_listed=no
    case $listed in *"\"$foreign\""*) foreign_listed=yes wrong="the list holds $foreign" ;; esac
    case $listed in *"\"$peer\""*) peer_listed=yes ;; *) unread="the list does not hold $peer, so its absences prove nothing" ;; esac
  fi
  sent_status=$(json_field status "$sent")
  if [ -n "$(unknown_of "$sent")" ] || [ -z "$sent_status" ]; then
    foreign_send=unread
    unread="${unread:+$unread; }the send to $foreign: ${sent:0:200}"
  elif [ "$sent_status" = 404 ]; then
    foreign_send=refused
  elif [ "${sent_status#5}" != "$sent_status" ]; then
    foreign_send=unread
    unread="${unread:+$unread; }the send to $foreign answered $sent_status"
  elif [ "${sent_status#2}" != "$sent_status" ]; then
    foreign_send=accepted
    wrong="${wrong:+$wrong; }a send to $foreign was accepted ($sent_status)"
  else
    foreign_send=$sent_status
    wrong="${wrong:+$wrong; }a send to $foreign answered $sent_status, not the 404 of a session no listener knows: ${sent:0:200}"
  fi
  sub_refused "$socket" "$peer's subject"
  marks="peer_listed=$peer_listed foreign_listed=$foreign_listed foreign_send=$foreign_send sub=${sub:-unread}"
  if [ -n "$wrong" ]; then
    row P6 "$want6" "$wrong${unread:+; $unread}; $marks" FAIL
  elif [ -n "$unread" ]; then
    row P6 "$want6" "$unread; $marks" UNKNOWN
  else
    row P6 "$want6" "$peer listed, $foreign not; the send to it answered 404; the sub refused subject_not_granted; $marks" PASS
  fi
fi

# --- P7: the agent's token mints no agent token and approves nothing: each answered 403
# OAUTH_SCOPE_REQUIRED, the refusal of a credential that lacks the route's scope. A 403 with
# another code says nothing of that rule, so it reads FAIL too. ---
want7="POST /api/v1/me/agent-tokens and POST /api/v1/credential-requests/<id>/approve with the agent's token: 403 OAUTH_SCOPE_REQUIRED each"
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
        403)
          case $(json_field code "$answer") in
            OAUTH_SCOPE_REQUIRED) ;;
            *) wrong="${wrong:+$wrong; }$call answered 403 $(json_field code "$answer"), not OAUTH_SCOPE_REQUIRED: ${answer:0:200}" ;;
          esac
          ;;
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
    row P7 "$want7" "both 403 OAUTH_SCOPE_REQUIRED" PASS
  fi
fi

# --- P8: a socket closes 4401 at its token's expiry, and the session reopens and receives. ---
want8="a relay socket closes 4401 within 25 s after its token's exp, at most 300 s after the trade; the Oh My Pi session reopens and holds a message sent after that"
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
      wrong="the socket was still open 60 s after its token's exp"
    elif [ $((closed_at - expires)) -lt 0 ]; then
      # A close before the expiry (a dropped connection, a listener that restarted) shows nothing
      # of whether the edge closes a socket at the expiry, which is what this part reads.
      unread="the socket closed $code $(((expires - closed_at) / 1000)) s before its token's exp, so its close at the exp was not read"
    elif [ "$code" != 4401 ] || [ $((closed_at - expires)) -gt 25000 ]; then
      wrong="closed $code $(((closed_at - expires) / 1000)) s after the token's exp"
    fi
  fi
  # Every token the Oh My Pi session traded before it was first read on the session list had
  # expired 300 s after that read at the latest, and the socket it opened with that token closed
  # 25 s after: the session opens its socket before it registers. What it holds after that came
  # through a socket it opened again. The leg sleeps until then, and no longer.
  left=$((session_seen_at + 330 - $(now)))
  [ "$left" -le 0 ] || sleep "$left"
  sent=$(probe send "$session_id" "outside-e2e after-reopen $nonce")
  sent_at=$(now)
  shown=''
  while [ -z "$shown" ] && [ "$(now)" -le $((sent_at + 10)) ]; do
    case $(shown_text) in *"outside-e2e after-reopen $nonce"*) shown=$(($(now) - sent_at)) ;; *) pause ;; esac
  done
  sent_status=$(json_field status "$sent")
  if [ -n "$(unknown_of "$sent")" ] || [ -z "$sent_status" ]; then
    unread="${unread:+$unread; }the send after the reopen: ${sent:0:200}"
  elif [ "${sent_status#2}" = "$sent_status" ]; then
    wrong="${wrong:+$wrong; }the send after the reopen answered $sent_status"
  elif [ -z "$shown" ]; then
    wrong="${wrong:+$wrong; }a message sent $(($(now) - session_seen_at)) s after the session was first listed was not held within 10 s"
  fi
  if [ -n "$wrong" ]; then
    row P8 "$want8" "$wrong${unread:+; $unread}" FAIL
  elif [ -n "$unread" ]; then
    row P8 "$want8" "$unread" UNKNOWN
  else
    row P8 "$want8" "closed 4401 $(((closed_at - expires) / 1000)) s after exp, $(((expires - traded) / 1000)) s after the trade; a message after the reopen held in $shown s" PASS
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

# --- P10: no live view of the outside session reaches Dispatch. A session's live view is its
# agentstream frames, which Dispatch reads for its own people's sessions. As the Oh My Pi session's
# own id at the edge, a publish of agentstream.<session>.frames is refused topic_not_published, an
# interest in agentstream.> topic_not_in_project, and a relay sub of agentstream.<session>.>
# subject_not_granted. It runs after P8, since its interest and its relay socket, made as that
# session, replace the session's registration and socket. ---
want10="as ${session_id:-<the Oh My Pi session>} at ${listener:-<OUTSIDE_LISTENER_URL>}: publish agentstream.<session>.frames refused topic_not_published; interest agentstream.> refused topic_not_in_project; relay sub agentstream.<session>.> refused subject_not_granted"
problem=$(lacking OUTSIDE_LISTENER_URL)
[ -n "$problem" ] || problem=$(probe_problem)
[ -n "$problem" ] || [ -n "$session_id" ] ||
  problem="not run: P4 found no Oh My Pi session at the edge, whose live view this row asks for"
if [ -n "$problem" ]; then
  row P10 "$want10" "$problem" UNKNOWN
else
  wrong=''
  unread=''
  probe_as=$session_id refused publish "agentstream.$session_id.frames" topic_not_published
  probe_as=$session_id refused interest 'agentstream.>' topic_not_in_project
  sub_refused "$(probe_as=$session_id probe relay-sub "agentstream.$session_id.>")" "agentstream.$session_id.>"
  if [ -n "$wrong" ]; then
    row P10 "$want10" "$wrong${unread:+; $unread}" FAIL
  elif [ -n "$unread" ]; then
    row P10 "$want10" "$unread" UNKNOWN
  else
    row P10 "$want10" "the publish, the interest and the relay sub each refused by its code" PASS
  fi
fi

# --- C0-C7: the outside person's Codex agent, under P2's device login. outside-agent-codex.sh,
# beside this file, runs it with this leg's HOME, where P2's login is, so no second login is
# approved; it installs the pinned Codex and the plugin's npm release itself. Its rows join this
# leg's TSV, each read through worst_row, so a row it wrote twice reads as its worst copy. ---
codex_rows='C0 C1 C2 C3 C4 C5 C6 C7'
problem=$(probe_problem)
[ -n "$problem" ] || [ -f "$here/outside-agent-codex.sh" ] ||
  problem="outside-agent-codex.sh is not beside this file in $here"
if [ -n "$problem" ]; then
  for name in $codex_rows; do row "$name" "the Codex leg's row $name" "$problem" UNKNOWN; done
else
  say "C0-C7: outside-agent-codex.sh"
  : >"$work/codex.tsv"
  env -i "PATH=$work/tools:$PATH" "HOME=$home" "TERM=${TERM:-xterm-256color}" \
    "TMPDIR=${TMPDIR:-/tmp}" OUTSIDE_DISPATCH_URL="$dispatch" OUTSIDE_BROKER_URL="$broker" \
    OUTSIDE_LISTENER_URL="$listener" OUTSIDE_INSIDE_DISPATCH_URL="${OUTSIDE_INSIDE_DISPATCH_URL:-}" \
    OUTSIDE_PROJECT="$project" OUTSIDE_PERSON="$person" OUTSIDE_PEER_SESSION="$peer" \
    OUTSIDE_DEVICE_LOGIN_HOME="$home" OUTSIDE_ALLOW_HTTP="$allow_http" \
    OUTSIDE_CODEX_ENVOY_VERSION="${OUTSIDE_CODEX_ENVOY_VERSION:-}" \
    OUTSIDE_CODEX_ENVOY_COMMIT="${OUTSIDE_CODEX_ENVOY_COMMIT:-}" \
    OUTSIDE_DEVICE_LOGIN_COMMIT="${OUTSIDE_DEVICE_LOGIN_COMMIT:-}" \
    OUTSIDE_CODEX_ENVOY_SOURCE="${OUTSIDE_CODEX_ENVOY_SOURCE:-}" \
    OUTSIDE_CODEX_TSV="$work/codex.tsv" bash "$here/outside-agent-codex.sh" >/dev/null </dev/null
  codex_exit=$?
  for name in $codex_rows; do
    line=$(worst_row "$work/codex.tsv" "$name")
    if [ -z "$line" ]; then
      row "$name" "the Codex leg's row $name" "outside-agent-codex.sh wrote no $name row (exit $codex_exit)" UNKNOWN
      continue
    fi
    IFS="$tab" read -r _ want got verdict <<EOF
$line
EOF
    case $verdict in PASS | FAIL | UNKNOWN) row "$name" "$want" "$got" "$verdict" ;; *)
      row "$name" "$want" "the Codex leg's row reads $(cell "$verdict"), which is no verdict: $got" UNKNOWN ;;
    esac
  done
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
    0) row revocations "$want_revoke" "the revoke command exited 0, keeping ${p2_code:-no code}" PASS ;;
    1) row revocations "$want_revoke" "the revoke command failed (exit 1)" FAIL ;;
    *) row revocations "$want_revoke" "the revoke command could not tell (exit $revoked)" UNKNOWN ;;
  esac
fi

# --- The T5 window: the owner removes the test person from the project's access list, and the
# person's live agent and browser stop. This leg opens the window by telling the peer, whose
# tailnet leg then has the owner make the removal and keeps its time; the tailnet leg judges T5
# from that time and the rows below, each of which carries the times it read (Unix seconds). For
# up to OUTSIDE_T5_SECONDS (default 1200) the leg trades P2's device login afresh every 10 s, and
# keeps a relay socket open with the newest token, opening another when one closes, as the agent
# would. Once a trade is refused, it watches the socket open then until it closes or 325 s pass,
# and goes on trading every 10 s meanwhile and once more as it closes: a trade answered after the
# refusal reads FAIL, since the removal stops every trade and not one. The socket's close passes
# only with the edge's own codes, 4401 (its token expired) or 4403 (refused before its expiry): any
# other close says nothing of the removal. OUTSIDE_T5_COMMAND, the workflow's browser, runs beside
# it with OUTSIDE_T5_DEADLINE (Unix s) and OUTSIDE_T5_TSV: it polls the person's cookie until it
# gets 401 and then tries the passkey sign-in, writing rows T5-cookie and T5-passkey there, which
# are copied into this leg's TSV. ---
t5_seconds=${OUTSIDE_T5_SECONDS:-1200}
want_t5a="once the owner removes ${person:-<OUTSIDE_PERSON>}: a fresh trade of the device login is refused and no later trade answered, and the relay socket open at that moment closes 4401 or 4403"
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
  say "T5: window open for up to $t5_seconds s: $told"
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
  refusals=0
  answered_after=''
  unread_after=0
  last_answered_at=''
  socket_pid=''
  socket_log=''
  sockets=0
  socket_unread=''
  while [ "$(now)" -lt "$deadline" ]; do
    answer=$(probe trade)
    case $(json_field traded "$answer") in
      false)
        refused_at=$(now)
        refusal=$answer
        refusals=1
        break
        ;;
      # Each answered trade's time: the tailnet leg holds the last one to the removal's bound.
      true) last_answered_at=$(now) ;;
    esac
    # A socket that ended is opened again with a fresh token, as the agent's own would be.
    if [ -z "$socket_unread" ] && { [ -z "$socket_pid" ] || ! running_child "$socket_pid"; }; then
      sockets=$((sockets + 1))
      socket_log="$work/t5-socket-$sockets.log"
      probe_as="outside-e2e-t5-$nonce" probe_exec hold "$t5_seconds" >"$socket_log" &
      socket_pid=$!
      background="$background $socket_pid"
      # Its first line, `opened_at` or `unknown`, looked at every pause for up to 3 s; a socket
      # that ended has written all it will.
      socket_until=$(($(now) + 3))
      while [ ! -s "$socket_log" ] && running_child "$socket_pid" && [ "$(now)" -lt "$socket_until" ]; do
        pause
      done
      case $(cat "$socket_log") in *'"unknown"'*) socket_unread=$(text_of "$socket_log") ;; esac
    fi
    sleep 10
  done
  # The socket open at the refusal, watched until it closes or 325 s pass, the trades going on. A
  # later trade that got no readable answer (no status, a malformed line, no broker reached) is
  # neither a refusal nor an answer, so "no later trade is answered" is not read while one stands.
  socket_open=''
  socket_opened_at=''
  closed_at=''
  close_code=''
  if [ -n "$refused_at" ]; then
    if [ -z "$socket_unread" ] && [ -n "$socket_pid" ] && running_child "$socket_pid"; then
      socket_open=yes
    fi
    while [ "$(now)" -le $((refused_at + 325)) ]; do
      # The 10 s between two trades, cut short by the close of the socket this watch is for.
      if [ -n "$socket_open" ]; then end_within "$socket_pid" 10; else sleep 10; fi
      answer=$(probe trade)
      case $(json_field traded "$answer") in
        false) refusals=$((refusals + 1)) ;;
        true) [ -n "$answered_after" ] || answered_after=$(now) ;;
        *) unread_after=$((unread_after + 1)) ;;
      esac
      if [ -z "$socket_open" ] || ! running_child "$socket_pid"; then break; fi
    done
    if [ -n "$socket_open" ]; then
      held=$(cat "$socket_log")
      socket_opened_at=$(json_field opened_at "$held")
      case $socket_opened_at in '' | null) socket_opened_at='' ;; *) socket_opened_at=$((socket_opened_at / 1000)) ;; esac
      closed_at=$(json_field closed_at "$held")
      close_code=$(json_field code "$held")
      case $closed_at in '' | null) closed_at='' ;; *) closed_at=$((closed_at / 1000)) ;; esac
    fi
  fi
  marks="opened_at=$opened last_answered_at=${last_answered_at:-none} refused_at=${refused_at:-none} socket_opened_at=${socket_opened_at:-none} socket_closed_at=${closed_at:-none} code=${close_code:-none} sockets=$sockets refusals=$refusals unread_after=$unread_after"
  if [ -z "$refused_at" ]; then
    row T5-agent "$want_t5a" "no trade refused in the ${t5_seconds} s window, the last answered at ${last_answered_at:-none}; $marks" UNKNOWN
  elif [ -n "$answered_after" ]; then
    row T5-agent "$want_t5a" "a trade was answered at $answered_after, $((answered_after - refused_at)) s after the first refusal; $marks" FAIL
  elif [ "$unread_after" -gt 0 ]; then
    row T5-agent "$want_t5a" "after the first refusal, $unread_after later trade(s) got no readable answer, so whether every later trade was refused is not read; $marks" UNKNOWN
  elif [ -n "$socket_unread" ]; then
    row T5-agent "$want_t5a" "the trades were refused; the socket part was not read: $socket_unread; $marks" UNKNOWN
  elif [ -z "$socket_open" ]; then
    row T5-agent "$want_t5a" "the trades were refused; no relay socket was open at the first refusal (the removal came before one opened, or as one had closed), so the socket part was not read; $marks" UNKNOWN
  elif [ -z "$closed_at" ]; then
    row T5-agent "$want_t5a" "the trades were refused, and the socket open at the first refusal was still open 325 s later; $marks" FAIL
  else
    case $close_code in
      4401 | 4403) row T5-agent "$want_t5a" "the trades were refused (status $(json_field status "$refusal")) and the socket open then closed $close_code; $marks" PASS ;;
      *) row T5-agent "$want_t5a" "the trades were refused; the socket open then closed $close_code, not the edge's own 4401 or 4403, so its close says nothing of the removal; $marks" UNKNOWN ;;
    esac
  fi
  if [ -z "$t5_driver" ]; then
    row T5-cookie "$want_t5c" "OUTSIDE_T5_COMMAND is unset, so the browser was not read" UNKNOWN
    row T5-passkey "$want_t5p" "OUTSIDE_T5_COMMAND is unset, so the browser was not read" UNKNOWN
  else
    await_child "$t5_driver" $((deadline + 30 - $(now)))
    driver_end="${child_exit:+exit $child_exit}"
    driver_end=${driver_end:-still running}
    for name in T5-cookie T5-passkey; do
      line=$(worst_row "$work/t5.tsv" "$name")
      if [ -z "$line" ]; then
        want=$want_t5c
        [ "$name" = T5-cookie ] || want=$want_t5p
        row "$name" "$want" "OUTSIDE_T5_COMMAND wrote no $name row ($driver_end)" UNKNOWN
        continue
      fi
      IFS="$tab" read -r _ want got verdict <<EOF
$line
EOF
      case $verdict in PASS | FAIL | UNKNOWN) row "$name" "$want" "$got" "$verdict" ;; *)
        row "$name" "$want" "the browser's row reads $(cell "$verdict"), which is no verdict: $got" UNKNOWN ;;
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
