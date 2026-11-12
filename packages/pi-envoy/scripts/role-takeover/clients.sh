# shellcheck shell=bash disable=SC2154 # the variables named below are the caller's to set
# clients.sh — the client processes smoke-role-takeover.sh drives its rows with, and what each
# one sent: a recording proxy and a process per client (start_proxy, client_fifo, client_ready,
# start_omp, start_claude), one command answered on a client's stdout (send), the readers of
# that client's proxy log — the claims it made (claim_status, claim_holder, first_claim_status,
# first_claim_holder, made_claim, claim_count), the registrations it sent (instance_id,
# registered_at), its deregistration (delete_answer) and its hand-backs (release_status,
# release_instance, released) — and the two ways a row makes a holder answer for its claim
# (renew_omp, renew_claude).
#
# smoke-role-takeover.sh sources it, as it sources local-stack.sh and listener-api.sh beside it:
# what that smoke knows about the stack is in the first file, what it knows about the listener's
# answers is in the second, and what it knows about the clients themselves is in this one.
#
# Sourced, never run: it defines functions and the tables they keep, and starts no process of
# its own.
#
# The caller sets
#   work                   the scratch directory the proxy logs, fifos and client output live in
#   drivers                the directory of the Bun drivers (role-takeover/*.ts)
#   repo_root              this checkout, which every client runs out of
#   channel_server         packages/claude-envoy/bin/envoy-channel.ts
#   envoy_url              the listener each proxy forwards to
#   nats_url               natsd, which each client subscribes through
#   omp_heartbeat_ms       the Oh My Pi client's heartbeat period
#   omp_pass_deadline_ms   how long one of its passes may take
#   client_reply_timeout   how long a client may take to answer one command
#   listener_wait_timeout  how long a renewal may take to reach the listener
#   run_id                 this run's id, which renew_claude's topic carries
# and defines refuse, wait_for, pick_port and port_open, and the scenario_pids and scenario_fds
# its teardown kills and closes, which every process and descriptor started here is added to.

# ---- the clients ---------------------------------------------------------------------------------
declare -A proxy_url proxy_log client_fd client_out client_pid
next_command_id=0

# start_proxy LABEL: this client's own HTTP path to the listener, recording every request.
start_proxy() {
  local label="$1" port
  pick_port port
  proxy_log["$label"]="${work}/proxies/${label}.jsonl"
  : >"${proxy_log[$label]}"
  (exec env -i PATH="$PATH" HOME="${work}/bun-home" TMPDIR="${TMPDIR:-/tmp}" \
    bun "${drivers}/listener-proxy.ts" --port "$port" --upstream "$envoy_url" \
    --log "${proxy_log[$label]}" --label "$label") >>"${work}/logs/proxy-${label}.log" 2>&1 &
  scenario_pids+=($!)
  proxy_url["$label"]="http://127.0.0.1:${port}"
  wait_for 30 port_open "$port" ||
    refuse "the ${label} proxy did not listen on 127.0.0.1:${port}: $(tail -n 5 "${work}/logs/proxy-${label}.log")"
}

# client_fifo LABEL: the stdin this shell holds open for the client, so it never sees end of input.
client_fifo() {
  local label="$1" fifo="${work}/${1}.stdin" fd
  mkfifo "$fifo"
  exec {fd}<>"$fifo"
  client_fd["$label"]="$fd"
  scenario_fds+=("$fd")
  client_out["$label"]="${work}/${label}.out"
  : >"${client_out[$label]}"
}

# client_ready LABEL: the client's first line, which it writes once it is up.
client_ready() {
  grep -q '"ready":true' "${client_out[$1]}" 2>/dev/null
}

# start_omp LABEL: one Oh My Pi Envoy extension instance in a process of its own, pointed at this
# label's proxy and at natsd. `env -i` keeps the operator's own ENVOY_*, NATS_* and DISPATCH_*
# variables out of it, so the configuration is this script's alone.
start_omp() {
  local label="$1" home="${work}/${1}-home"
  start_proxy "$label"
  mkdir -p "${home}/cwd"
  jq -n --arg url "${proxy_url[$label]}" --arg nats "$nats_url" \
    '{envoy: {url: $url, natsUrls: [$nats]}, dispatch: {enabled: false}, askgate: {enabled: false}}' \
    >"${home}/envoy.json"
  client_fifo "$label"
  (cd -- "${repo_root}/packages/pi-envoy" &&
    exec env -i PATH="$PATH" HOME="$home" TMPDIR="${TMPDIR:-/tmp}" \
      ENVOY_CONFIG_FILE="${home}/envoy.json" ENVOY_HEARTBEAT_MS="$omp_heartbeat_ms" \
      ENVOY_PASS_DEADLINE_MS="$omp_pass_deadline_ms" ROLE_TAKEOVER_CWD="${home}/cwd" \
      ROLE_TAKEOVER_CLIENT_LOG="${work}/logs/${label}-notices.log" \
      bun --preload "${drivers}/host-stub.ts" "${drivers}/omp-client.ts") \
    <"${work}/${label}.stdin" >"${client_out[$label]}" 2>"${work}/logs/${label}.err" &
  client_pid["$label"]=$!
  scenario_pids+=($!)
  wait_for 90 client_ready "$label" ||
    refuse "the ${label} Oh My Pi client did not start: $(tail -n 10 "${work}/logs/${label}.err")"
}

# start_claude LABEL SESSION STATE_DIR HEARTBEAT_MS: one `claude` process and the real channel
# server under it. Two labels given one STATE_DIR are two `claude` processes of one user, which
# share one plugin data directory, as they do on a real machine.
start_claude() {
  local label="$1" session="$2" state="$3" heartbeat="$4" config="${work}/${1}-claude-config"
  start_proxy "$label"
  mkdir -p "$config" "$state"
  jq -n --arg url "${proxy_url[$label]}" --arg nats "$nats_url" \
    '{envoy: {enabled: true, url: $url, natsUrls: [$nats]}, dispatch: {enabled: false}}' \
    >"${config}/envoy.json"
  client_fifo "$label"
  (cd -- "${repo_root}/packages/claude-envoy" &&
    exec env -i PATH="$PATH" HOME="${work}/bun-home" TMPDIR="${TMPDIR:-/tmp}" \
      bun "${drivers}/claude-client.ts" --server "$channel_server" --session "$session" \
      --state-dir "$state" --config-dir "$config" --heartbeat-ms "$heartbeat" \
      --stderr "${work}/logs/${label}-channel.err") \
    <"${work}/${label}.stdin" >"${client_out[$label]}" 2>"${work}/logs/${label}.err" &
  client_pid["$label"]=$!
  scenario_pids+=($!)
  wait_for 120 client_ready "$label" ||
    refuse "the ${label} Claude Code channel server did not start: $(tail -n 10 "${work}/logs/${label}.err") $(tail -n 10 "${work}/logs/${label}-channel.err" 2>/dev/null)"
}

reply=""
# send LABEL JSON: one command, answered on the client's stdout. The answer is left in `reply`.
send() {
  local label="$1" body="$2" id
  next_command_id=$((next_command_id + 1))
  id="c${next_command_id}"
  printf '%s\n' "$(jq -c --arg id "$id" '. + {id: $id}' <<<"$body")" >&"${client_fd[$label]}"
  local answered=()
  answered=(grep -q "\"id\":\"${id}\"" "${client_out[$label]}")
  wait_for "$client_reply_timeout" "${answered[@]}" ||
    refuse "the ${label} client did not answer ${body} within ${client_reply_timeout}s: $(tail -n 10 "${work}/logs/${label}.err")"
  reply="$(grep -F "\"id\":\"${id}\"" "${client_out[$label]}" | tail -n 1)"
  jq -e '.ok == true' <<<"$reply" >/dev/null ||
    refuse "the ${label} client refused ${body}: ${reply}"
}

# ---- reading what a client sent ------------------------------------------------------------------
# claim_status LABEL SESSION PREVIOUS: the status the listener answered that client's last soft
# claim for SESSION naming PREVIOUS (empty PREVIOUS: a claim naming none), empty when it made none.
claim_status() {
  jq -r --arg sid "$2" --arg previous "$3" \
    'select(.path == "/v1/roles/set" and .request.session_id == $sid
            and (.request.previous_session_id // "") == $previous) | .status' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

# claim_holder LABEL SESSION PREVIOUS: the holder the listener named in refusing that claim.
claim_holder() {
  jq -r --arg sid "$2" --arg previous "$3" \
    'select(.path == "/v1/roles/set" and .request.session_id == $sid
            and (.request.previous_session_id // "") == $previous) | .response.holder // ""' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

# first_claim_status LABEL SESSION PREVIOUS: the status the listener answered that client's
# *first* such claim, empty when it made none. A row where one claim is refused and a later one
# granted reads the refusal here and the grant in `claim_status`. Slurped rather than piped
# through `head`, which would leave jq writing into a closed pipe.
first_claim_status() {
  jq -rs --arg sid "$2" --arg previous "$3" \
    'map(select(.path == "/v1/roles/set" and .request.session_id == $sid
                and (.request.previous_session_id // "") == $previous))
     | if length == 0 then "" else (.[0].status | tostring) end' \
    "${proxy_log[$1]}" 2>/dev/null
}

# first_claim_holder LABEL SESSION PREVIOUS: the holder the listener named in refusing that
# first claim.
first_claim_holder() {
  jq -rs --arg sid "$2" --arg previous "$3" \
    'map(select(.path == "/v1/roles/set" and .request.session_id == $sid
                and (.request.previous_session_id // "") == $previous))
     | .[0].response.holder // ""' \
    "${proxy_log[$1]}" 2>/dev/null
}

# instance_id LABEL SESSION: the client instance id that client last registered SESSION with.
instance_id() {
  jq -r --arg sid "$2" \
    'select(.path == "/v1/interests/subscribe" and .request.session_id == $sid)
     | .request.client_instance_id // ""' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

# registered_at LABEL SESSION: when that client last registered SESSION, 0 for never.
registered_at() {
  local at
  at="$(jq -r --arg sid "$2" \
    'select(.path == "/v1/interests/subscribe" and .request.session_id == $sid) | .at' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1)"
  printf '%s' "${at:-0}"
}

# delete_answer LABEL SESSION FIELD: a field of the listener's answer to that client's
# deregistration of SESSION. `//` would read a false `deleted` as absent, so the field is taken
# by name and rendered whatever its type.
delete_answer() {
  jq -r --arg sid "/v1/sessions/$2" --arg field "$3" \
    'select(.method == "DELETE" and .path == $sid)
     | (.response // {})
     | if type == "object" and has($field) then (.[$field] | tostring) else "" end' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

made_claim() {
  [[ -n "$(claim_status "$1" "$2" "$3")" ]]
}

# claim_count LABEL SESSION PREVIOUS: how many such claims that client has made, so a row that
# must see one more claim reads this before the pass it drives and waits for it to rise.
claim_count() {
  jq -rs --arg sid "$2" --arg previous "$3" \
    'map(select(.path == "/v1/roles/set" and .request.session_id == $sid
                and (.request.previous_session_id // "") == $previous)) | length' \
    "${proxy_log[$1]}" 2>/dev/null
}

# release_status LABEL SESSION TOPIC: the status the listener answered that client's last
# hand-back of TOPIC for SESSION (POST /v1/interests/unsubscribe), empty when it made none.
release_status() {
  jq -r --arg sid "$2" --arg topic "$3" \
    'select(.path == "/v1/interests/unsubscribe" and .request.session_id == $sid
            and ((.request.topics // []) | index($topic)) != null) | .status' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

# release_instance LABEL SESSION TOPIC: the process that hand-back named as the one asking.
release_instance() {
  jq -r --arg sid "$2" --arg topic "$3" \
    'select(.path == "/v1/interests/unsubscribe" and .request.session_id == $sid
            and ((.request.topics // []) | index($topic)) != null)
     | .request.client_instance_id // ""' \
    "${proxy_log[$1]}" 2>/dev/null | tail -n 1
}

released() {
  [[ -n "$(release_status "$1" "$2" "$3")" ]]
}

# ---- making a holder answer for its claim --------------------------------------------------------
# A claim is renewed by its holder's registrations, so a row where the holder must still hold the
# role makes it register, and one where it must lose the role simply stops.

# renew_omp LABEL SESSION: one heartbeat pass in that Oh My Pi process, waited out at its proxy.
# The pass re-registers SESSION, which renews every claim naming that session and that process.
renew_omp() {
  local label="$1" session="$2" before
  before="$(registered_at "$label" "$session")"
  registered_again() {
    [[ "$(registered_at "$label" "$session")" -gt "$before" ]]
  }
  send "$label" '{"op":"heartbeat"}'
  wait_for "$listener_wait_timeout" registered_again ||
    refuse "the ${label} client did not re-register ${session} within ${listener_wait_timeout}s"
}

# renew_claude LABEL: one registration from that channel server, answered once it has landed. The
# subscribe tool follows a topic and registers the session on the way (`follow` in
# src/envoy-channel-server.ts), which is this script's way of making a server on the long
# heartbeat answer for its claim at a moment of the row's choosing.
renew_claude() {
  send "$1" "$(jq -nc --arg t "notifications.team.smoke-takeover-${run_id}" \
    '{op:"tool",name:"envoy_subscribe",args:{topics:[$t]}}')"
}
