# shellcheck shell=bash
# shellcheck disable=SC2154 # the leg that sources this file sets work, tools, here, project, nonce, probe_env
# The peer of row P4, and the session outside the project row P6 must not find:
# outside-agent-tailnet.sh sources this file from beside it, after outside-agent-lib.sh. It defines
# functions alone. They read the leg's `work`, `tools`, `here`, `project`, `nonce`, `probe_env` and
# `background`, and its OUTSIDE_* inputs, and set:
#   peer_kind      `omp` or `probe` (OUTSIDE_PEER).
#   peer           the peer's session id, once it registered at the edge.
#   peer_problem   why there is no peer, or nothing.
#   peer_seen      a file with one line per ping and reply the peer was shown, `<word> <nonce>
#                  <sender's stamp> <this box's clock when it was first seen>`, in Unix ms.
#   peer_tmux      the tmux socket the Oh My Pi peer runs under.
#   foreign_problem, foreign_in_project   see foreign_start.
#
# The Oh My Pi peer's input is Envoy cards from any session the edge admits into the project, so
# it runs with nothing of this box but its model and its broker identity: `env -i`, a HOME of its
# own that holds only the model's configuration and credential (peer_home_make), no OUTSIDE_*
# variable, and no built-in tool (`--no-tools`), so sending Envoy messages is all it can do.

# The peer's instructions. It stamps nothing: the public leg times a card from its envelope's
# `at`, which the peer's Envoy client sets as it sends, and this leg times what the peer is shown
# by its own clock.
peer_prompt="You are the peer session of an automated check. Follow these instructions exactly and take no other action. Wait for Envoy messages. When a message 'outside-e2e ping <N> at <T>' arrives from a session, call envoy_send to send that session exactly 'outside-e2e pong <N>', and then call envoy_send again to send it exactly 'outside-e2e hello <N>'. When a message 'outside-e2e reply <N> at <T>' arrives from a session, call envoy_send to send that session exactly 'outside-e2e done <N>'. Then wait again."

# peer_setup: reads the peer's inputs and sets peer_kind, peer_seen, peer_tmux, peer_sessions and,
# for an Oh My Pi peer, its HOME (peer_home_make) and environment (peer_vars), or peer_problem.
peer_setup() {
  peer_kind=${OUTSIDE_PEER:-omp}
  peer_seen="$work/peer-seen.txt"
  : >"$peer_seen"
  peer_tmux="$work/peer-tmux.sock"
  peer_sessions="$work/peer-sessions"
  peer_problem=$(lacking OUTSIDE_LISTENER_URL)
  [ -n "$peer_problem" ] || peer_problem=$(tool_problem bun)
  case $peer_kind in
    omp)
      peer_plugin=${OUTSIDE_PEER_PLUGIN_DIR:-$here/../../../pi-envoy}
      [ -n "$peer_problem" ] || peer_problem=$(tool_problem omp tmux)
      [ -n "$peer_problem" ] || [ -f "$peer_plugin/dist/envoy.js" ] ||
        peer_problem="OUTSIDE_PEER_PLUGIN_DIR names $peer_plugin, which holds no built envoy extension (dist/envoy.js)"
      [ -n "$peer_problem" ] || peer_home_make
      [ -n "$peer_problem" ] || peer_env_make
      ;;
    # The scripted peer is P4's control: it answers with no model in the loop, so a model peer's
    # failure where it passes is the model's. Its own PASS proves no model turn, so the leg reads
    # it UNKNOWN.
    probe) ;;
    *) peer_problem="OUTSIDE_PEER must be omp or probe, not $peer_kind" ;;
  esac
}

# peer_home_make: the Oh My Pi peer's HOME, a scratch directory holding only the model's
# configuration and credential: the files OUTSIDE_PEER_AGENT_FILES names (default `config.yml
# models.yml agent.db`, Oh My Pi's roles, providers and stored logins) copied out of
# OUTSIDE_PEER_AGENT_DIR (default ~/.omp/agent) into its .omp/agent. A database is copied with
# sqlite3's .backup where sqlite3 is on PATH, which reads a live database whole. Sets peer_home,
# or peer_problem.
peer_home_make() {
  local source=${OUTSIDE_PEER_AGENT_DIR:-$HOME/.omp/agent} name copied=''
  peer_home="$work/peer-home"
  if ! mkdir -p "$peer_home/.omp/agent" || ! chmod 700 "$peer_home"; then
    peer_problem="the peer's HOME could not be made"
    return
  fi
  for name in ${OUTSIDE_PEER_AGENT_FILES:-config.yml models.yml agent.db}; do
    [ -f "$source/$name" ] || continue
    case $name in
      *.db)
        if command -v sqlite3 >/dev/null 2>&1; then
          sqlite3 "$source/$name" ".backup '$peer_home/.omp/agent/$name'" </dev/null 2>/dev/null
        else
          cp "$source/$name" "$peer_home/.omp/agent/$name" &&
            { [ ! -f "$source/$name-wal" ] || cp "$source/$name-wal" "$peer_home/.omp/agent/$name-wal"; }
        fi
        ;;
      *) cp "$source/$name" "$peer_home/.omp/agent/$name" ;;
    esac && copied="$copied $name"
  done
  # The copies hold the model's credential: readable by this account alone.
  for name in "$peer_home/.omp/agent"/*; do
    [ ! -f "$name" ] || chmod 600 "$name"
  done
  [ -n "$copied" ] ||
    peer_problem="OUTSIDE_PEER_AGENT_DIR ($source) holds none of ${OUTSIDE_PEER_AGENT_FILES:-config.yml models.yml agent.db}, the model's configuration and credential the peer runs on"
}

# peer_env_make: peer_vars, the Oh My Pi peer's whole environment: its HOME, the edge listener,
# the project its token names, and the broker client's identity settings of this box (every
# AGENT_SECRETS_* but the projects, GCE_METADATA_HOST and XDG_RUNTIME_DIR), with the directory the
# client reads a person's sign-in from pinned to this box's own, since the peer's HOME is another.
# OUTSIDE_PEER_ENV names any other variable the model needs, such as a provider's key, passed as
# it is.
peer_env_make() {
  local name
  peer_vars=(
    "PATH=$tools:$PATH"
    "HOME=$peer_home"
    "TERM=${TERM:-xterm-256color}"
    "TMPDIR=${TMPDIR:-/tmp}"
    "OMP_SKIP_SETUP=1"
    "ENVOY_URL=${OUTSIDE_LISTENER_URL:-}"
    "ENVOY_INBOUND=listener"
    "AGENT_SECRETS_SIGNIN_DIR=${AGENT_SECRETS_SIGNIN_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/signins}"
    "AGENT_SECRETS_PROJECTS=$project"
  )
  while IFS= read -r name; do
    case $name in
      AGENT_SECRETS_PROJECTS | AGENT_SECRETS_SIGNIN_DIR) ;;
      AGENT_SECRETS_* | GCE_METADATA_HOST | XDG_RUNTIME_DIR) peer_vars+=("$name=${!name}") ;;
    esac
  done <<EOF
$(compgen -e)
EOF
  for name in ${OUTSIDE_PEER_ENV:-}; do
    [ -z "${!name:-}" ] || peer_vars+=("$name=${!name}")
  done
}

# peer_text: what the peer has been shown so far: the Oh My Pi peer's transcript, since the TUI
# folds an Envoy card to its header, or the scripted peer's log.
peer_text() {
  if [ "$peer_kind" = omp ]; then
    cat "$peer_sessions"/*.jsonl "$peer_sessions"/*/*.jsonl 2>/dev/null
  else
    cat "$work/peer.log" 2>/dev/null
  fi
}

# peer_watch: runs until it is stopped, adding to `peer_seen` each ping and reply the peer is
# shown for the first time, with the time it was first seen.
# shellcheck disable=SC2329 # started in the background by peer_start
peer_watch() {
  local text rest word n stamped
  while :; do
    text=$(peer_text)
    for word in ping reply; do
      rest=$text
      while case $rest in *"outside-e2e $word "*) true ;; *) false ;; esac; do
        rest=${rest#*"outside-e2e $word "}
        n=${rest%% *}
        stamped=${rest#"$n at "}
        [ "$stamped" != "$rest" ] || continue
        stamped=${stamped%%[!0-9]*}
        [ "${#stamped}" -le 13 ] || stamped=${stamped:0:13}
        [ -n "$stamped" ] || continue
        case $(cat "$peer_seen") in *"$word $n "*) ;; *)
          printf '%s %s %s %s000\n' "$word" "$n" "$stamped" "$(now)" >>"$peer_seen"
          ;;
        esac
      done
    done
    sleep 1
  done
}

# peer_start: starts the peer peer_setup prepared and sets `peer`, its session id, or
# peer_problem; then starts peer_watch. The Oh My Pi peer runs in a tmux server of its own; the
# scripted one is outside-agent-probe.ts's `peer`.
peer_start() {
  local ready deadline _
  [ -z "$peer_problem" ] || return 0
  if [ "$peer_kind" = omp ]; then
    peer_dir="$work/outside-e2e-peer-$nonce"
    mkdir -p "$peer_dir" "$peer_sessions"
    peer_dir=$(cd "$peer_dir" && pwd -P)
    say "starting the peer, omp in tmux (tmux -S $peer_tmux attach -t outside-e2e-peer)"
    env -i "${peer_vars[@]}" tmux -S "$peer_tmux" new-session -d -s outside-e2e-peer -x 220 -y 50 \
      -c "$peer_dir" omp --no-extensions -e "$peer_plugin/dist/envoy.js" --no-skills --no-rules \
      --no-context-files --no-tools --session-dir "$peer_sessions" </dev/null
    ready=''
    deadline=$(($(now) + 60))
    while [ -z "$ready" ] && [ "$(now)" -lt "$deadline" ]; do
      case $(tmux -S "$peer_tmux" capture-pane -p -J -S -5000 -t outside-e2e-peer 2>/dev/null) in
        *"outside-e2e-peer-$nonce"*) ready=yes ;;
        *) sleep 1 ;;
      esac
    done
    omp_clear_screens "$peer_tmux" outside-e2e-peer
    peer=''
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      [ -n "$ready" ] || break
      peer=$(json_field at_dir "$(env -i "${probe_env[@]}" AGENT_SECRETS_PROJECTS="$project" \
        OUTSIDE_PROBE_SESSION="outside-e2e-peer-finder-$nonce" OUTSIDE_LISTENER_URL="$OUTSIDE_LISTENER_URL" \
        OUTSIDE_DISPATCH_URL="http://127.0.0.1:1" bun "$here/outside-agent-probe.ts" sessions "$peer_dir" \
        2>>"$work/probe.log" </dev/null)")
      [ -z "$peer" ] || [ "$peer" = null ] || break
      peer=''
      sleep 3
    done
    if [ -z "$ready" ]; then
      peer_problem="the peer's Oh My Pi TUI did not name its directory within 60 s"
    elif [ -z "$peer" ]; then
      peer_problem="the peer's Oh My Pi session is not on $OUTSIDE_LISTENER_URL's session list"
    else
      tmux -S "$peer_tmux" send-keys -t outside-e2e-peer -l "$peer_prompt"
      sleep 1
      tmux -S "$peer_tmux" send-keys -t outside-e2e-peer Enter
    fi
  else
    say "starting the scripted peer $peer at $OUTSIDE_LISTENER_URL"
    env -i "${probe_env[@]}" AGENT_SECRETS_PROJECTS="$project" OUTSIDE_PROBE_SESSION="$peer" \
      OUTSIDE_LISTENER_URL="$OUTSIDE_LISTENER_URL" OUTSIDE_DISPATCH_URL="http://127.0.0.1:1" \
      bun "$here/outside-agent-probe.ts" peer >"$work/peer.log" 2>&1 </dev/null &
    background="$background $!"
    deadline=$(($(now) + 60))
    while [ "$(now)" -lt "$deadline" ]; do
      case $(cat "$work/peer.log") in *'"ready":true'* | *'"unknown"'*) break ;; esac
      sleep 1
    done
    case $(cat "$work/peer.log") in
      *'"ready":true'*) ;;
      *) peer_problem="the peer $peer did not start: $(text_of "$work/peer.log")" ;;
    esac
  fi
  if [ -z "$peer_problem" ]; then
    peer_watch &
    background="$background $!"
  fi
}

# peer_stop: stops the Oh My Pi peer's tmux server and waits for the peer to exit. A server left
# running keeps the peer registered at the edge after the leg ends: an early edge-on run found
# ten such peers alive.
peer_stop() {
  [ -n "${peer_tmux:-}" ] || return 0
  PATH="${tools:+$tools:}$PATH" stop_tmux "$peer_tmux"
}

# foreign_start: the session outside the project, `<peer>-foreign`, which P6 must not find on the
# edge's list, registered at the private listener OUTSIDE_INTERNAL_LISTENER_URL. Its token names
# OUTSIDE_FOREIGN_PROJECT alone, a project of this box's person other than the edge's: a token
# naming none carries every project of the person, the edge's among them. That holds only where
# this box's clients trade as its person (a sign-in or a device login); a machine trading for
# itself names the projects its entry decides. So the probe also presents that token to the edge,
# whose admission (E1) is a check apart from the session list P6 reads: an edge that admits it
# holds that session in the project, and then P6's absence cannot be asked (foreign_in_project).
# Sets foreign_problem, empty when the session is listed at the private listener and refused at
# the edge, and foreign_in_project.
foreign_start() {
  local deadline
  foreign_in_project=''
  foreign_problem=$(lacking OUTSIDE_INTERNAL_LISTENER_URL OUTSIDE_FOREIGN_PROJECT OUTSIDE_LISTENER_URL)
  [ -n "$foreign_problem" ] || [ "${OUTSIDE_FOREIGN_PROJECT:-}" != "$project" ] ||
    foreign_problem="OUTSIDE_FOREIGN_PROJECT names $project, the edge's own project"
  [ -n "$foreign_problem" ] || foreign_problem=$(tool_problem bun)
  [ -n "$foreign_problem" ] || [ -n "$peer" ] || foreign_problem="no peer session id, which it is named after"
  [ -z "$foreign_problem" ] || return 0
  # The log exists before the probe starts, so the read below never meets a missing file.
  : >"$work/foreign.log"
  env -i "${probe_env[@]}" OUTSIDE_PROBE_SESSION="$peer" \
    OUTSIDE_LISTENER_URL="http://127.0.0.1:1" OUTSIDE_DISPATCH_URL="http://127.0.0.1:1" \
    OUTSIDE_FOREIGN_LISTENER_URL="$OUTSIDE_INTERNAL_LISTENER_URL" \
    OUTSIDE_FOREIGN_EDGE_URL="$OUTSIDE_LISTENER_URL" OUTSIDE_FOREIGN_PROJECT="$OUTSIDE_FOREIGN_PROJECT" \
    bun "$here/outside-agent-probe.ts" foreign >"$work/foreign.log" 2>&1 </dev/null &
  background="$background $!"
  deadline=$(($(now) + 60))
  while [ "$(now)" -lt "$deadline" ]; do
    case $(cat "$work/foreign.log") in *'"foreign":"'* | *'"unknown"'*) break ;; esac
    sleep 1
  done
  case $(cat "$work/foreign.log") in
    *'"foreign":"'*'"registered":2'*'"listed":true'*'"edge_admits":false'*) ;;
    *'"foreign":"'*'"registered":2'*'"listed":true'*'"edge_admits":true'*)
      foreign_in_project="the edge admits the token of $peer-foreign ($(text_of "$work/foreign.log")), so that session is in $project, and its absence from the list cannot be asked: its token names ${OUTSIDE_FOREIGN_PROJECT:-} only where this box trades as its person, not as a machine whose entry decides its projects"
      foreign_problem=$foreign_in_project
      ;;
    *) foreign_problem="the session $peer-foreign was not registered and listed at $OUTSIDE_INTERNAL_LISTENER_URL, with the edge's answer to its token read: $(text_of "$work/foreign.log")" ;;
  esac
}
