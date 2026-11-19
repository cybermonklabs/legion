# shellcheck shell=bash disable=SC2154 # the variables named below are the caller's to set
# lib/codex-host.sh — the Codex CLI sessions of nats-callout-sessions.sh: the Codex plugin packed
# as the release packs it (codex_pack), each session started, under the stand-in `codex` in the
# default mode and as the pinned Codex CLI with --real-hosts (codex_start), and stopped and kept
# for the evidence at the teardown (codex_stop, codex_clean). The script keeps the checks; this
# file keeps no table of its own: how each session is told its credential and what each control
# changes are the script's (`told`, `withholds`, `manifest_drops`), read here.
#
# Sourced, never run: it defines functions and starts nothing of its own.
#
# The caller sets root, work, scratch, evidence, drivers, hosts, heartbeat_ms, broker_env,
# ENVOY_URL, NATS_URLS, codex_labels, codex_session, codex_tmux_sockets, transcripts and
# scenario_pids, and with --real-hosts codex_bin, codex_base_url and codex_model; and defines
# refuse, note, wait_for, isolated, close_held_fifos, register_session, told, withholds and
# clients.sh's client_fifo, client_ready and send.

declare -A codex_transcript=()

# codex_pack: release.yaml's claude_envoy_build, scripts/build.ts --codex-out then `bun pm pack`
# of that directory, unpacked into a marketplace of the run's own: the repository's
# (.agents/plugins/marketplace.json) with the package's npm source swapped for the unpacked
# tarball, so each session installs the plugin under a release's name and keeps a release's state
# directory. The control's manifest_drops leave the installed manifest's env_vars. In the default
# mode it also compiles the stand-in `codex`, a binary of that name: the plugin's hooks find their
# codex process by its name, and the plugin runs `codex queue` with that process's own binary.
codex_pack() {
  local marketplace=$root/.agents/plugins/marketplace.json tarballs drop
  jq -e 'any(.plugins[]?; .name == "codex-envoy")' "$marketplace" >/dev/null ||
    refuse "$marketplace names no codex-envoy plugin"
  mkdir -p "$work/codex-pack" "$work/codex-market/.agents/plugins" "$work/codex-market/plugins/codex-envoy"
  (cd "$root/packages/claude-envoy" && bun scripts/build.ts --codex-out "$work/codex-envoy") \
    >"$work/logs/build-codex-envoy.log" 2>&1 || refuse "codex-envoy did not build; see $work/logs/build-codex-envoy.log"
  (cd "$work/codex-envoy" && bun pm pack --destination "$work/codex-pack") >"$work/logs/pack-codex-envoy.log" 2>&1 ||
    refuse "codex-envoy did not pack; see $work/logs/pack-codex-envoy.log"
  tarballs=("$work"/codex-pack/*.tgz)
  { [ "${#tarballs[@]}" = 1 ] && [ -f "${tarballs[0]}" ]; } ||
    refuse "packing codex-envoy left no one tarball in $work/codex-pack"
  codex_plugin=$work/codex-market/plugins/codex-envoy
  tar xzf "${tarballs[0]}" -C "$codex_plugin" --strip-components=1
  codex_manifest=$codex_plugin/.codex-plugin/plugin.json
  { [ -r "$codex_manifest" ] && [ -r "$codex_plugin/dist/envoy-channel.js" ]; } ||
    refuse "the packed codex-envoy holds no .codex-plugin/plugin.json and dist/envoy-channel.js"
  for drop in "${manifest_drops[@]}"; do
    jq --arg v "$drop" '.mcpServers.envoy.env_vars -= [$v]' "$codex_manifest" >"$work/codex-manifest.json"
    mv "$work/codex-manifest.json" "$codex_manifest"
  done
  codex_market_name=$(jq -r .name "$marketplace")
  jq '.plugins |= map(if .name == "codex-envoy" then .source = {source: "local", path: "./plugins/codex-envoy"} else . end)' \
    "$marketplace" >"$work/codex-market/.agents/plugins/marketplace.json"
  codex_plugin_version=$(jq -r .version "$codex_manifest")
  if jq -e '.mcpServers.envoy.env_vars | index("NATS_AUTH")' "$codex_manifest" >/dev/null; then
    codex_names_nats_auth=true
  else
    codex_names_nats_auth=false
  fi
  echo "   codex-envoy $codex_plugin_version packed; its manifest names NATS_AUTH among env_vars: $codex_names_nats_auth"
  [ "$hosts" = plugins ] || return 0
  (cd "$root" && bun build --compile "$drivers/codex-client.ts" --outfile "$work/codex-bin/codex") \
    >"$work/logs/build-codex-client.log" 2>&1 ||
    refuse "the stand-in codex did not build; see $work/logs/build-codex-client.log"
}

# codex_envoy LABEL HOME: the envoy.json of that session's HOME (~/.config/legion/envoy.json, which
# a Codex session reads where no ENVOY_CONFIG_FILE names another), naming natsAuth where `told`
# says the file tells it.
codex_envoy() {
  told "$1"
  mkdir -p "$2/.config/legion"
  jq -n --arg url "$ENVOY_URL" --arg nats "$NATS_URLS" --argjson named "$told_file" \
    '{envoy: ({enabled: true, url: $url, natsUrls: [$nats]} + (if $named then {natsAuth: "broker"} else {} end)),
      dispatch: {enabled: false}}' >"$2/.config/legion/envoy.json"
}

# codex_start: every Codex session up, its first prompt made, so its plugin has recorded and
# registered it.
codex_start() {
  if [ "$hosts" = plugins ]; then codex_start_standins; else codex_start_real; fi
}

# codex_start_standins: each session under the stand-in `codex`, the plugin copied where Codex
# installs it (<CODEX_HOME>/plugins/cache/<marketplace>/<plugin>/<version>), whose MCP server it
# starts with the manifest's env_vars alone, so a NATS_AUTH in its environment reaches the server
# only through the manifest. Each session's id is the one the script chose for it.
codex_start_standins() {
  local label base installed flags
  for label in "${codex_labels[@]}"; do
    base=$work/$label
    installed=$base/codex-home/plugins/cache/$codex_market_name/codex-envoy/$codex_plugin_version
    mkdir -p "$installed" "$base/home" "$base/cwd"
    cp -R "$codex_plugin/." "$installed/"
    codex_envoy "$label" "$base/home"
    flags=()
    ! withholds "$label" || flags=(--withhold-queue-answers true)
    client_fifo "$label"
    (close_held_fifos && cd -- "$base/cwd" &&
      exec env -i PATH="$PATH" HOME="$base/home" TMPDIR="$scratch" CODEX_HOME="$base/codex-home" \
        ENVOY_HEARTBEAT_MS="$heartbeat_ms" "${broker_env[@]}" "${told_env[@]}" \
        "$work/codex-bin/codex" --plugin "$installed" --cwd "$base/cwd" \
        --stderr "$work/logs/$label-server.err" --hook-log "$work/logs/$label-hooks.jsonl" "${flags[@]}") \
      <"$work/$label.stdin" >"${client_out[$label]}" 2>"$work/logs/$label.err" &
    scenario_pids+=($!)
  done
  # The session's first prompt: the plugin's SessionStart hook records its id for the server. Its
  # server's environment is kept first, while the server waits for that record: a server that then
  # fails to dial exits, and its environment with it.
  for label in "${codex_labels[@]}"; do
    wait_for 120 client_ready "$label" ||
      refuse "the $label Codex client did not start: $(tail -n 10 "$work/logs/$label.err") $(tail -n 10 "$work/logs/$label-server.err" 2>/dev/null)"
    codex_keep_server_env "$label"
    send "$label" "$(jq -cn --arg s "${codex_session[$label]}" '{op: "start", session: $s}')"
  done
}

# codex_start_real: the pinned Codex CLI's terminal in a tmux server of its own for each session,
# the plugin installed from the run's marketplace with Codex's own commands, into a Codex home
# outside TMPDIR, where Codex refuses its helper binaries. Codex calls the route the run was given
# through a provider of its own, whose key is in the variable it names (env_key), so the key is in
# Codex's environment and never in an argv or a file of Codex's. Each session's id is the one Codex
# mints, read from the record its SessionStart hook writes, and the pod is registered for it then.
codex_start_real() {
  local label base installed sock record codex_cache
  tmux_bin=$(command -v tmux)
  codex_cache=${XDG_CACHE_HOME:-$HOME/.cache}
  mkdir -p "$codex_cache"
  codex_root=$(mktemp -d "$codex_cache/ncs-codex.XXXXXX")
  case $codex_root/ in
    "${TMPDIR:-/tmp}"/* | /tmp/*)
      refuse "the Codex homes would be under $codex_root, a temporary directory, where Codex refuses its helper binaries: set XDG_CACHE_HOME to a directory outside it"
      ;;
  esac
  # The tmux sockets go in a short directory of their own: a unix socket path past 107 bytes is
  # refused.
  codex_tmux_dir=$(mktemp -d /tmp/ncs-codex-tmux.XXXXXX)
  for label in "${codex_labels[@]}"; do
    base=$codex_root/$label
    mkdir -p "$base/codex-home" "$base/home" "$base/cwd" "$base/tmp"
    codex_envoy "$label" "$base/home"
    jq -rn --arg model "$codex_model" --arg url "$codex_base_url" --arg cwd "$base/cwd" '
      "model = \($model | tojson)",
      "model_provider = \"ncs\"",
      "approval_policy = \"never\"",
      "sandbox_mode = \"read-only\"",
      "[model_providers.ncs]",
      "name = \"ncs\"",
      "base_url = \($url | tojson)",
      "wire_api = \"responses\"",
      "env_key = \"NCS_CODEX_MODEL_KEY\"",
      "requires_openai_auth = false",
      "[projects.\($cwd | tojson)]",
      "trust_level = \"trusted\""' >"$base/codex-home/config.toml"
    (isolated PATH="$PATH" HOME="$base/home" CODEX_HOME="$base/codex-home" TMPDIR="$base/tmp" -- \
      "$codex_bin" plugin marketplace add "$work/codex-market") >>"$work/logs/$label-install.log" 2>&1 ||
      refuse "codex plugin marketplace add failed for $label; see $work/logs/$label-install.log"
    (isolated PATH="$PATH" HOME="$base/home" CODEX_HOME="$base/codex-home" TMPDIR="$base/tmp" -- \
      "$codex_bin" plugin add "codex-envoy@$codex_market_name") >>"$work/logs/$label-install.log" 2>&1 ||
      refuse "codex plugin add failed for $label; see $work/logs/$label-install.log"
    installed=$base/codex-home/plugins/cache/$codex_market_name/codex-envoy/$codex_plugin_version
    cmp -s "$codex_manifest" "$installed/.codex-plugin/plugin.json" ||
      refuse "codex plugin add installed no copy of the packed manifest at $installed; see $work/logs/$label-install.log"
    sock=$codex_tmux_dir/$label
    (isolated PATH="$PATH" HOME="$base/home" CODEX_HOME="$base/codex-home" TMPDIR="$base/tmp" \
      TERM=xterm-256color LANG=C.UTF-8 NCS_CODEX_MODEL_KEY="$(<"$work/codex-model-key")" \
      ENVOY_HEARTBEAT_MS="$heartbeat_ms" "${broker_env[@]}" "${told_env[@]}" -- \
      "$tmux_bin" -S "$sock" -f /dev/null new-session -d -s codex -x 200 -y 50 -c "$base/cwd" \
      "$codex_bin" --dangerously-bypass-hook-trust) >>"$work/logs/$label-tmux.log" 2>&1 ||
      refuse "tmux did not start Codex for $label; see $work/logs/$label-tmux.log"
    codex_tmux_sockets+=("$sock")
  done
  for label in "${codex_labels[@]}"; do
    sock=$codex_tmux_dir/$label
    wait_for 120 codex_composer "$sock" ||
      refuse "Codex showed no prompt for $label: $("$tmux_bin" -S "$sock" capture-pane -p -t codex 2>&1 | tail -n 15)"
    # Codex has started the plugin's server, which waits for the record the first prompt writes.
    codex_keep_server_env "$label"
    # One turn of its own first: Codex runs SessionStart at a session's first prompt.
    "$tmux_bin" -S "$sock" send-keys -t codex -l 'Reply with the one word: ready.'
    sleep 0.5
    "$tmux_bin" -S "$sock" send-keys -t codex Enter
  done
  for label in "${codex_labels[@]}"; do
    wait_for 120 codex_record "$label" >/dev/null ||
      refuse "Codex's SessionStart hook recorded no session for $label: $("$tmux_bin" -S "$codex_tmux_dir/$label" capture-pane -p -t codex 2>&1 | tail -n 15)"
    record=$(codex_record "$label" | head -n 1)
    # shellcheck disable=SC2004 # codex_session is the script's associative array, keyed by label
    codex_session[$label]=$(jq -r '.session_id' "$record")
    codex_transcript[$label]=$(jq -r '.transcript_path' "$record")
    transcripts+=("${codex_transcript[$label]}")
    register_session "${codex_session[$label]}"
    note "Codex session ($label) ${codex_session[$label]}"
  done
}

# codex_composer SOCKET: Codex's terminal shows its prompt and has loaded its model, so it takes
# what is typed: a prompt typed while the header still says `model: loading` is lost (measured on
# 0.154.0).
codex_composer() {
  local pane
  pane=$("$tmux_bin" -S "$1" capture-pane -p -t codex 2>/dev/null) || return 1
  grep -q 'Ask Codex' <<<"$pane" && ! grep -qE 'model: +loading' <<<"$pane"
}

# codex_record LABEL: the record the plugin's SessionStart hook wrote for that real session.
codex_record() {
  compgen -G "$codex_root/$1/codex-home/plugins/data/codex-envoy-$codex_market_name/codex-sessions/*.json"
}

# codex_received LABEL MARKER: a real Codex session's own transcript names MARKER: the turn
# `codex queue` started, or the hook's text inside a running turn.
codex_received() {
  grep -qF -- "$2" "${codex_transcript[$1]}" 2>/dev/null
}

# codex_manifest_gap LABEL: for a Codex session told in its environment alone, that the installed
# manifest does not hand NATS_AUTH to the plugin's server, where it does not; nothing otherwise.
codex_manifest_gap() {
  case " ${codex_labels[*]} " in
    *" $1 "*) ;;
    *) return 0 ;;
  esac
  told "$1"
  { [ "${#told_env[@]}" -gt 0 ] && [ "${codex_names_nats_auth:-true}" = false ]; } || return 0
  printf '; the installed codex-envoy manifest names no NATS_AUTH among its env_vars, so Codex hands the plugin'"'"'s server none and it dials with no credential'
}

# codex_cwd LABEL: that session's working directory.
codex_cwd() {
  if [ "$hosts" = plugins ]; then printf '%s' "$work/$1/cwd"; else printf '%s' "$codex_root/$1/cwd"; fi
}

# codex_server_pid LABEL: the process of that session's plugin MCP server: the one the stand-in
# names in its ready line, or, for a real Codex, the installed plugin's envoy-channel.js running in
# that session's Codex home.
codex_server_pid() {
  local pid
  if [ "$hosts" = plugins ]; then
    jq -r 'select(.ready == true) | .serverPid' "${client_out[$1]}" 2>/dev/null | head -n 1
    return 0
  fi
  for pid in $(pgrep -f 'dist/envoy-channel\.js --host codex'); do
    case $(readlink "/proc/$pid/cwd" 2>/dev/null)/ in
      "$codex_root/$1/"*)
        echo "$pid"
        return 0
        ;;
    esac
  done
  return 1
}

# codex_keep_server_env LABEL: that session's plugin server's environment, read from its
# /proc/<pid>/environ while it waits for its session record, kept as $work/<label>.server-environ
# (0600) for the probe rows: what Codex, or the stand-in, handed the server, so as the manifest's
# env_vars filtered it. It refuses when no such server runs within 60 s.
codex_keep_server_env() {
  local pid=""
  wait_for 60 codex_server_pid "$1" >/dev/null && pid=$(codex_server_pid "$1")
  if [ -z "$pid" ] || ! (umask 077 && cat "/proc/$pid/environ" >"$work/$1.server-environ"); then
    refuse "no plugin server process of the $1 Codex session was found to read its environment from"
  fi
}

# codex_probe LABEL SESSION NATSPROBE-ARGS...: natsprobe in the environment that session's plugin
# server was started with (codex_keep_server_env), presenting the credential the plugin's own
# settings resolve there (role-takeover/codex-probe.ts): so a session whose server is told no
# credential, as under seedless or manifest-drops-nats-auth, probes with none.
codex_probe() {
  local label=$1 session=$2
  shift 2
  bun "$drivers/codex-probe.ts" --environ "$work/$label.server-environ" --session "$session" \
    --cwd "$(codex_cwd "$label")" -- "$(command -v natsprobe)" "$@"
}

# codex_transcripts_in_evidence: with --real-hosts, each Codex session's own transcript, the file
# its receive checks read, copied into the evidence before the teardown, and one check per session
# that the evidence holds it: a file whose first line names the session's id. (The stand-in's
# transcripts reach the evidence at the teardown, in codex_clean.)
codex_transcripts_in_evidence() {
  local label file kept
  [ "$hosts" = real ] || return 0
  mkdir -p "$evidence/sessions"
  for label in "${codex_labels[@]}"; do
    file=${codex_transcript[$label]:-}
    kept=$evidence/sessions/${file##*/}
    [ -n "$file" ] && [ -f "$file" ] && cp "$file" "$kept"
    if [ -n "$file" ] && [ -s "$kept" ] && head -n 1 "$kept" | grep -qF -- "${codex_session[$label]}"; then
      pass "transcript-in-evidence ($label): $kept, the transcript of Codex session ${codex_session[$label]}"
    else
      failed "transcript-in-evidence ($label)" "the evidence holds no transcript of Codex session ${codex_session[$label]} (its record named '$file')"
    fi
  done
}

# codex_stop: at the teardown, each real Codex's terminal kept whole for the evidence (Codex keeps
# no log of its own unless asked), then two ^Cs, which quit Codex and close its plugin's MCP
# server's stdin, so the server deregisters its session; a Codex still up 15 s later goes with its
# tmux server. A stand-in session stops with the other clients, when its input ends.
codex_stop() {
  local sock
  [ "${#codex_tmux_sockets[@]}" = 0 ] && return 0
  mkdir -p "$evidence/sessions"
  for sock in "${codex_tmux_sockets[@]}"; do
    "$tmux_bin" -S "$sock" capture-pane -p -S - -t codex >"$evidence/sessions/${sock##*/}-pane.txt" 2>&1
    "$tmux_bin" -S "$sock" send-keys -t codex C-c 2>/dev/null
    sleep 0.5
    "$tmux_bin" -S "$sock" send-keys -t codex C-c 2>/dev/null
  done
  for sock in "${codex_tmux_sockets[@]}"; do
    for _ in $(seq 1 75); do
      "$tmux_bin" -S "$sock" has-session -t codex 2>/dev/null || break
      sleep 0.2
    done
    "$tmux_bin" -S "$sock" kill-server 2>/dev/null
  done
}

# codex_clean: the stand-in sessions' transcripts, which the plugin read, kept in the evidence, and
# the real sessions' Codex homes and tmux sockets removed (each real transcript is in
# `transcripts`, which the script keeps).
codex_clean() {
  local transcript
  for transcript in "$work"/codex-*/codex-home/sessions/*/*/*/rollout-*.jsonl; do
    [ -f "$transcript" ] && cp "$transcript" "$evidence/client-logs/"
  done
  case ${codex_root:-} in
    */ncs-codex.??????) rm -rf -- "$codex_root" ;;
  esac
  case ${codex_tmux_dir:-} in
    /tmp/ncs-codex-tmux.*) rm -rf -- "$codex_tmux_dir" ;;
  esac
}
