# shellcheck shell=bash disable=SC2154 # the variables named below are the caller's to set
# clients.sh — the client processes smoke-role-takeover.sh drives its rows with, and what each
# one sent: a recording proxy and a process per client (start_proxy, client_fifo, client_ready,
# start_omp, and start_claude, which is launch_claude and then claude_started, a wait until the
# server answers a tool: claude_answers_tools), one command written to a client's stdin and read
# back off its stdout later (post, answer_to, has_answered) or waited for there (send), the
# readers of that client's proxy log — the claims it made (claim_status, claim_holder,
# first_claim_status, first_claim_holder, first_claim_arrived_at, made_claim, claims_answered,
# claim_count), the registrations it sent (instance_id, registered_at, first_registered_at,
# first_registration_answered_at, second_registration_arrived_at), the requests
# of either kind (claim_writes), its deregistration (delete_answer), its hand-backs
# (release_status, release_instance, released) and how old a holder's renewal was when a claim
# was weighed against it (renewal_age) — and the ways a row makes a holder answer for its claim
# (renew_omp, renew_claude, renew_claude_until_claimed, renew_claude_until_started).
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
# and defines refuse, note, wait_for, pick_port and port_open, and the scenario_pids and
# scenario_fds its teardown kills and closes, which every process and descriptor started here is
# added to.

# ---- the clients ---------------------------------------------------------------------------------
declare -A proxy_url proxy_log client_fd client_out client_pid
next_command_id=0

# start_proxy LABEL: this client's own HTTP path to the listener, recording every request as it
# arrives (`<log>.arrived`) and again once the listener has answered it (the log itself).
start_proxy() {
  local label="$1" port
  pick_port port
  proxy_log["$label"]="${work}/proxies/${label}.jsonl"
  : >"${proxy_log[$label]}"
  : >"${proxy_log[$label]}.arrived"
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
# server under it (launch_claude), waited for until it is up (claude_started).
start_claude() {
  launch_claude "$@"
  claude_started "$1"
}

# launch_claude LABEL SESSION STATE_DIR HEARTBEAT_MS [HOLDER]: that process, started and not
# waited for. The channel server claims the role its record names as it boots, so a row whose
# holder must be answering for its claim when that claim is weighed launches the claimant here,
# names the holder's label as HOLDER, and has the holder answer while it boots
# (renew_claude_until_claimed, renew_claude_until_started). HOLDER answers (renew_claude) once
# LABEL's proxy listens and immediately before LABEL's process starts: the proxy's own start is
# a process start a loaded machine stretches too, and the claim must never be weighed against an
# answer older than it. Two labels given one STATE_DIR are two `claude` processes of one user,
# which share one plugin data directory, as they do on a real machine.
launch_claude() {
  local label="$1" session="$2" state="$3" heartbeat="$4" holder="${5:-}"
  local config="${work}/${1}-claude-config"
  start_proxy "$label"
  mkdir -p "$config" "$state"
  jq -n --arg url "${proxy_url[$label]}" --arg nats "$nats_url" \
    '{envoy: {enabled: true, url: $url, natsUrls: [$nats]}, dispatch: {enabled: false}}' \
    >"${config}/envoy.json"
  client_fifo "$label"
  [[ -z "$holder" ]] || renew_claude "$holder"
  (cd -- "${repo_root}/packages/claude-envoy" &&
    exec env -i PATH="$PATH" HOME="${work}/bun-home" TMPDIR="${TMPDIR:-/tmp}" \
      bun "${drivers}/claude-client.ts" --server "$channel_server" --session "$session" \
      --state-dir "$state" --config-dir "$config" --heartbeat-ms "$heartbeat" \
      --stderr "${work}/logs/${label}-channel.err") \
    <"${work}/${label}.stdin" >"${client_out[$label]}" 2>"${work}/logs/${label}.err" &
  client_pid["$label"]=$!
  scenario_pids+=($!)
}

# claude_started LABEL: the launched process's ready line, which it writes once the channel server
# under it has answered MCP's initialize, and then a tool call the server answers as a started
# server does. The listener has the session's registration before the server's start ends, and
# until it ends the server answers every tool "still starting", so a row that calls a tool next (a
# role claim, a subscription that renews one) would otherwise read that refusal as the tool's own
# answer. Refused when either has not come within 120 s of the call.
claude_started() {
  local label="$1" deadline=$((SECONDS + 120))
  wait_for 120 client_ready "$label" ||
    refuse "the ${label} Claude Code channel server did not start: $(tail -n 10 "${work}/logs/${label}.err") $(tail -n 10 "${work}/logs/${label}-channel.err" 2>/dev/null)"
  until claude_answers_tools "$label"; do
    ((SECONDS < deadline)) ||
      refuse "the ${label} Claude Code channel server answered no tool within 120s: ${reply}"
    sleep 0.2
  done
}

# claude_answers_tools LABEL: that channel server answers envoy_whoami, which reads nothing but
# the session's own identity, with a result rather than an error.
claude_answers_tools() {
  send "$1" '{"op":"tool","name":"envoy_whoami","args":{}}'
  jq -e '.result.error == null and .result.result != null' <<<"$reply" >/dev/null
}

posted=""
# post LABEL JSON: one command written to the client's stdin and not waited for. Its id is left in
# `posted`, and answer_to reads the answer once it has come. send is a post waited out; a row posts
# by itself where it must go on driving another client while this one works on the command
# (renew_claude_until_started).
post() {
  local label="$1" body="$2"
  next_command_id=$((next_command_id + 1))
  posted="c${next_command_id}"
  printf '%s\n' "$(jq -c --arg id "$posted" '. + {id: $id}' <<<"$body")" >&"${client_fd[$label]}"
}

# answer_to LABEL ID: that client's answer to the command ID, empty while it has not answered. Not
# having answered yet is a normal state (a posted command is read back on a later pass), so the
# empty answer comes with status 0: grep's 1 for no match would otherwise fail an assignment from
# this under the smoke's errexit and end the whole run with no row table.
answer_to() {
  grep -F "\"id\":\"${2}\"" "${client_out[$1]}" 2>/dev/null | tail -n 1 || true
}

# has_answered LABEL ID: that client has answered the command ID.
has_answered() {
  [[ -n "$(answer_to "$1" "$2")" ]]
}

reply=""
# send LABEL JSON: one command (post), waited for until the client has answered it on its stdout.
# The answer is left in `reply`. Refused when it has not come within client_reply_timeout, or when
# it is not `ok`.
send() {
  local label="$1" body="$2" id
  post "$label" "$body"
  id="$posted"
  wait_for "$client_reply_timeout" has_answered "$label" "$id" ||
    refuse "the ${label} client did not answer ${body} within ${client_reply_timeout}s: $(tail -n 10 "${work}/logs/${label}.err")"
  reply="$(answer_to "$label" "$id")"
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

# first_claim_arrived_at LABEL SESSION PREVIOUS: when that client's first soft claim for SESSION
# naming PREVIOUS (`*`: any predecessor or none) reached its proxy (`<log>.arrived`), whether or
# not the listener has answered it; empty when it made none.
first_claim_arrived_at() {
  jq -rs --arg sid "$2" --arg previous "$3" \
    'map(select(.path == "/v1/roles/set" and .request.session_id == $sid
                and ($previous == "*" or (.request.previous_session_id // "") == $previous)) | .at)
     | if length == 0 then "" else (min | tostring) end' \
    "${proxy_log[$1]}.arrived" 2>/dev/null
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

# first_registered_at LABEL SESSION: when that client first registered SESSION, 0 for never. A
# row that times something from a process's first registration reads it here, since every later
# pass of that process registers again.
first_registered_at() {
  local at
  at="$(jq -rs --arg sid "$2" \
    'map(select(.path == "/v1/interests/subscribe" and .request.session_id == $sid))
     | if length == 0 then "" else (.[0].at | tostring) end' \
    "${proxy_log[$1]}" 2>/dev/null)"
  printf '%s' "${at:-0}"
}

# first_registration_answered_at LABEL SESSION: when the listener answered that client's first
# registration of SESSION, empty when it sent none.
first_registration_answered_at() {
  jq -rs --arg sid "$2" \
    'map(select(.path == "/v1/interests/subscribe" and .request.session_id == $sid) | .at + .ms)
     | if length == 0 then "" else (min | tostring) end' \
    "${proxy_log[$1]}" 2>/dev/null
}

# second_registration_arrived_at LABEL SESSION: when that client's second registration of
# SESSION reached its proxy (`<log>.arrived`), empty when it sent fewer than two.
second_registration_arrived_at() {
  jq -rs --arg sid "$2" \
    'map(select(.path == "/v1/interests/subscribe" and .request.session_id == $sid) | .at) | sort
     | if length < 2 then "" else (.[1] | tostring) end' \
    "${proxy_log[$1]}.arrived" 2>/dev/null
}

# claim_writes LABEL SESSION: how many of that client's requests for SESSION wrote the session's
# claims — its registrations, each of which renews the claims it holds, and its claims — so a
# row that must see none after some moment reads it there and again later. A request is logged
# once the listener has answered it, so one still in flight at the first reading is counted at
# the second.
claim_writes() {
  jq -rs --arg sid "$2" \
    'map(select((.path == "/v1/interests/subscribe" or .path == "/v1/roles/set")
                and .request.session_id == $sid)) | length' \
    "${proxy_log[$1]}" 2>/dev/null
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

# claims_answered LABEL SESSION PREVIOUS MS: every soft claim of that client for SESSION naming
# PREVIOUS (`*`: any predecessor or none) that reached its proxy before MS has been answered: each
# such arrival (`<log>.arrived`, which the proxy writes before it forwards a request) has an answer
# of the same seq in the log. A claim is logged only once the listener has answered it, so a check
# that reads every claim before a moment waits for this first (expect_renewed_inside), and a claim
# still in flight then is read once it is answered, or read as unread if it never is.
claims_answered() {
  jq -n -e --slurpfile arrived "${proxy_log[$1]}.arrived" --slurpfile answered "${proxy_log[$1]}" \
    --arg sid "$2" --arg previous "$3" --argjson before "$4" '
    [$answered[] | .seq] as $done
    | all($arrived[] | select(.path == "/v1/roles/set" and .request.session_id == $sid
                              and ($previous == "*" or (.request.previous_session_id // "") == $previous)
                              and .at < $before);
          .seq as $seq | any($done[]; . == $seq))' >/dev/null 2>&1
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

# renewal_age CLAIMANT SESSION PREVIOUS HOLDER HOLDER_SESSION [BEFORE_MS]: the most the holder's
# renewal can have aged by the time the listener weighed CLAIMANT's soft claim for SESSION naming
# PREVIOUS, in milliseconds — from the arrival of the holder's latest registration of
# HOLDER_SESSION that the listener had answered with a 200 before that claim arrived, to the
# listener's answer to the claim, both read off the two proxies' logs, which stamp one wall clock.
# The claim is the client's first such claim; given BEFORE_MS, it is each such claim that arrived
# before that moment, and the age is the oldest of theirs, for a row whose claimant asks again and
# again while the holder must keep the role. PREVIOUS `*` takes a claim naming any predecessor or
# none, for a claimant that names its predecessor only while it reads that predecessor as live,
# and claims naming none once the holder's answer has lapsed. The claims are the proxy's arrivals
# (`<log>.arrived`), each read with its answer of the same seq, so a claim still in flight is never
# left out. Empty where the claimant made no such claim, where one of them has no answer yet, or
# where the holder registered nothing before one of them: an age nobody could read, which a row
# reports as such rather than as inside the window.
#
# A registration answered 200 is read as a renewal, though the listener answers 200 whatever the
# renewal wrote (its registration route logs a renewal it could not write and goes on). A
# registration writes no renewal where the claim is no longer its process's, where a hand-back
# marked it, or where its write lost a compare-and-swap to another write of the record. In the
# rows that read this, the holder's claim is its own and unmarked when the row starts, and the
# only other writer of that record is the claimant, whose refused claims write nothing. So a 200
# that renewed nothing can only follow a claim the listener granted the claimant, and each row
# reads the age at every claim it needs refused, in order, from the first. A renewal whose read or
# write failed is recorded, and the listener then refuses the next claim under that session id
# (its grant guard), so it can make a row pass by a refusal, never by a grant.
renewal_age() {
  jq -nr --slurpfile arrived "${proxy_log[$1]}.arrived" --slurpfile answered "${proxy_log[$1]}" \
    --slurpfile renewals "${proxy_log[$4]}" \
    --arg sid "$2" --arg previous "$3" --arg holder_sid "$5" --arg before "${6:-}" '
    [$arrived[] | select(.path == "/v1/roles/set" and .request.session_id == $sid
                         and ($previous == "*" or (.request.previous_session_id // "") == $previous))]
    | if $before == "" then .[:1] else map(select(.at < ($before | tonumber))) end
    | map(.seq as $seq | [$answered[] | select(.seq == $seq)] | .[0])
    | if length == 0 or any(. == null) then "" else
        map(. as $claim
            | [$renewals[] | select(.path == "/v1/interests/subscribe"
                                    and .request.session_id == $holder_sid
                                    and .status == 200 and .at + .ms <= $claim.at) | .at]
            | if length == 0 then null else $claim.at + $claim.ms - max end)
        | if any(. == null) then "" else (max | tostring) end
      end' 2>/dev/null
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

# renew_claude_until_claimed SECONDS HOLDER CLAIMANT SESSION PREVIOUS: HOLDER's channel server
# answers for its claim (renew_claude) again and again until CLAIMANT, launched and booting
# (launch_claude, which has HOLDER answer right before the launch), has made a soft claim for
# SESSION naming PREVIOUS, so that claim is weighed against a renewal one round trip old however
# long the boot took. For a row whose holder must stop answering once that first claim is made;
# one whose holder must keep the role while the claimant asks again runs
# renew_claude_until_started. 1 when no such claim came within SECONDS, which the row then fails
# on by name.
renew_claude_until_claimed() {
  local deadline=$((SECONDS + $1)) holder="$2" claimant="$3" session="$4" previous="$5"
  until made_claim "$claimant" "$session" "$previous"; do
    ((SECONDS < deadline)) || return 1
    renew_claude "$holder"
    sleep 0.2
  done
}

# renew_claude_until_started SECONDS HOLDER CLAIMANT: HOLDER's channel server answers for its
# claim (renew_claude) on every pass until CLAIMANT, launched (launch_claude, which has HOLDER
# answer right before the launch), has ended its start: its ready line, and then a tool it answers
# as a started server does (claude_answers_tools). A claimant refused at its boot claims again on
# every heartbeat, and the rest of its start after that first claim (the sweeps of its state
# directories) is file work a loaded machine stretches past the window too, so the holder answers
# through it. The tool call is posted and its answer read on a later pass, never waited on: a
# claimant whose start has stalled answers late, and the holder answering meanwhile is the point.
# A probe still unanswered on a pass is that normal state, and the loop reads it again on the
# next. 1 when CLAIMANT has not ended its start within SECONDS, after a note of where it stands
# (no ready line, or the last answer its probe got), so the row that needs the start fails by its
# own name and the run goes on to its row table; refused only when the client refuses the probe.
renew_claude_until_started() {
  local deadline=$((SECONDS + $1)) holder="$2" claimant="$3" probe="" answer=""
  while :; do
    renew_claude "$holder"
    if [[ -n "$probe" ]]; then
      answer="$(answer_to "$claimant" "$probe")"
      if [[ -n "$answer" ]]; then
        jq -e '.ok == true' <<<"$answer" >/dev/null ||
          refuse "the ${claimant} client refused envoy_whoami: ${answer}"
        jq -e '.result.error == null and .result.result != null' <<<"$answer" >/dev/null && return 0
        probe=""
      fi
    fi
    if [[ -z "$probe" ]] && client_ready "$claimant"; then
      post "$claimant" '{"op":"tool","name":"envoy_whoami","args":{}}'
      probe="$posted"
    fi
    if ((SECONDS >= deadline)); then
      if client_ready "$claimant"; then
        note "   the ${claimant} Claude Code channel server answered no tool as a started server within ${1}s: ${answer:-its probe ${probe:-} has no answer}"
      else
        note "   the ${claimant} Claude Code channel server wrote no ready line within ${1}s: $(tail -n 10 "${work}/logs/${claimant}.err") $(tail -n 10 "${work}/logs/${claimant}-channel.err" 2>/dev/null)"
      fi
      return 1
    fi
    sleep 0.2
  done
}
