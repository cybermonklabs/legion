# shellcheck shell=bash disable=SC2154,SC2034 # the variables below are the caller's (listed below)
# Every read the answer loop's live proof judges a criterion by (scripts/e2e/answer-loop.sh): the
# daemon's log, the stopped agents' conversations, the asker's inbox, the listener's log, the
# recording proxy's records, Dispatch, GitHub and the daemon's own state. They live here, apart
# from the rig that produces what they read, so that scripts/e2e/lib/answer-loop-reads.test.ts can
# run each of them over fixtures of the shapes a run writes (lib/testdata/answer-loop) with no
# model, no network, no Postgres and no NATS. A read is the one part of a live proof a live run
# cannot check: a read that is wrong reports a pass for evidence that holds nothing, and every
# round of this proof has had one — jq over an array as an object, a reachability preflight
# counted as a model call, the agent's own words counted as the loop's append, a bigint the
# record carries as a string compared as a number, a pipe that fails under `pipefail` on an empty
# result and takes the whole run with it. Each read here is given a passing fixture, a failing
# one and an empty one by that test.
#
# Sourced, never run. The caller sets
#   evidence       the run's evidence directory, holding inbox.jsonl, model-calls.jsonl,
#                  logs/listener.log and a13/<branch|base>/<store>/
#   daemon_log     the current store's daemon log, every boot of it appended to one file
#   boot_offset    its size in bytes when the daemon booted, so boot_log reads this boot alone
#   store          the conversation store of the pass ("files" or "postgres"), part of the
#                  evidence's paths and of the daemon's proxy label
#   run_tag        the run's own number, part of the Oh My Pi session database's name
#   daemon_db      the daemon's own database for this pass, which review_record reads the round from
#   repo           the smoke repository, owner/name, whose comments and reviews the reads below read
#   agent          the run's agents, keyed `<store>:<name>.<field>` (file, session, issue, token)
#   questions_file scripts/e2e/answer-loop/questions.json, the questions and their answer keys
#   controller_session  the stand-in session the daemons' controller notices are heard on
#   mode           `workflow` when the daemon runs its workflow, else `claims-only`
# and defines, beside the reads below, nothing of its own. The reads call, and the caller defines:
#   pg_sql DATABASE SQL      the statement's rows as one JSON array
#   legion_cmd ARG...        the daemon's own binary against its configuration
#   dispatch_get PATH        a Dispatch read as the agents' bearer
#   smoke_gh ARG...          one gh call on the smoke repository as the implement App
#   note TEXT                one line of a check's evidence
# Every read judges what it is given: an empty log, an empty conversation, an inbox with nothing
# in it, a GitHub error body in place of a list. None of them ends the run under `set -e`.

# ---- the daemon's log ------------------------------------------------------------------------------
# boot_log prints the current boot's daemon log lines, JSON only.
boot_log() { tail -c "+$((boot_offset + 1))" "$daemon_log" | jq -c -R 'fromjson? // empty'; }
# run_log prints every line of this store's daemon log, JSON only: the whole run, every boot of it,
# which is what a check judges when another check restarted the daemon before it (A14 after A12's
# five restarts, whose own boot holds no answer at all).
run_log() { jq -c -R 'fromjson? // empty' "$daemon_log"; }

# answers_lines MSG [SESSION [STATE]]: how many lines of this boot's daemon log carry MSG, for
# SESSION and in STATE when given.
answers_lines() {
  boot_log | jq -s --arg m "$1" --arg s "${2:-}" --arg st "${3:-}" \
    '[.[] | select(.msg == $m and ($s == "" or .session == $s) and ($st == "" or .state == $st))] | length'
}
# question_lines QUESTION: how many lines of this boot's daemon log name QUESTION, whatever the
# `answers:` message. Zero says the boot was handed nothing of it at all.
question_lines() {
  boot_log | jq -s --arg q "$1" '[.[] | select(((.msg // "") | startswith("answers:")) and .question == $q)] | length'
}
# newest_question SESSION STATE: the id of the newest question of SESSION this boot logged in
# STATE, empty for none.
newest_question() {
  boot_log | jq -s -r --arg s "$1" --arg st "$2" \
    '[.[] | select(.msg == "answers: question" and .session == $s and .state == $st) | .question] | last // ""'
}
# answer_dropped SESSION: this boot dropped an answer of SESSION's because its conversation had
# started, and forwarded the question to the agent instead (A7).
answer_dropped() {
  boot_log | jq -e -s --arg s "$1" 'any(.[];
    .msg == "answers: the agent'"'"'s conversation is running or moved; the answer is dropped and the question forwarded"
    and .started == true and .session == $s)' >/dev/null
}
# question_forwarded ROLE: this boot forwarded a question of ROLE's to its agent, which a resume
# mid-answer does (A18).
question_forwarded() {
  boot_log | jq -e -s --arg r "$1" 'any(.[]; .msg == "answers: question" and .state == "forwarded" and .role == $r)' >/dev/null
}
# fault_passed_on FAULT: this boot passed a question to the controller for a reason naming FAULT
# (A12).
fault_passed_on() {
  boot_log | jq -e -s --arg f "$1" \
    'any(.[]; .msg == "answers: question passed to the controller" and ((.reason // "") | contains($f)))' >/dev/null
}
# answered_fault_is LINE FAULT CALLS: the `answers: answered` LINE reports FAULT after CALLS model
# calls. An empty line (the boot logged none) is no such line.
answered_fault_is() {
  jq -e --arg f "$2" --argjson c "$3" '.fault == $f and .calls == $c' <<<"${1:-null}" >/dev/null 2>&1
}
# decision_lines COMMENT: this boot's `decision passed on` lines for the person's reply COMMENT, as
# an array of what each says.
decision_lines() {
  boot_log | jq -s -c --arg d "dispatch:$1" \
    '[.[] | select(.msg == "answers: decision passed on" and .decision == $d) | {decision, to, issue, role, session, asker, ref}]'
}
# decision_passed_once LINES TO: LINES (decision_lines) holds exactly one line, to TO.
decision_passed_once() {
  jq -e --arg to "$2" 'length == 1 and .[0].to == $to' <<<"${1:-[]}" >/dev/null 2>&1
}
# registry_404_routes LOG: the registry routes the boot's LOG (boot_log) says the listener answered
# 404 on, with how many lines each: `[]` when it answered none, which is what a boot must show.
registry_404_routes() {
  jq -s -c '[.[] | select(((.msg // "") | startswith("the Envoy listener answers 404 on"))) | .route]
    | group_by(.) | map({route: .[0], lines: length})' <<<"${1:-}"
}
# answering_blockers prints, one per line, what the answering criteria wait on in this run: the
# daemon's workflow (the answer loop runs in it, and it boots only with both role Apps), and the
# listener's hand-off (plan task 6), read from this boot's log: the daemon writes the project's
# answering heartbeat at boot, and a listener without the hand-off answers its route 404.
answering_blockers() {
  [ "$mode" = workflow ] ||
    printf 'WAITING (needs the review App key grant): the daemon runs without its workflow, which carries the answer loop and boots only with both role Apps\n'
  if boot_log | jq -e -s 'any(.[]; (.msg // "") | startswith("the Envoy listener answers 404 on PUT /v1/legion/projects/{project}"))' >/dev/null; then
    printf 'WAITING (needs the listener hand-off, task 6): the listener answers the heartbeat route PUT /v1/legion/projects/{project} 404, so it hands nothing off\n'
  fi
}
# check_boot_without_answers: the negative control for the boot and A13's daemon half: the same
# daemon with `answers` removed writes no conversation to the agent registry and runs no loop. It
# still writes the project's answering heartbeat, saying it does not answer — a daemon whose
# operator removed `answers` stops the listener's hand-offs at its next boot rather than when the
# last answering key ages out (internal/daemon, newConversationHistory) — and on a listener
# without the hand-off that heartbeat's route answers 404, one line a minute. So the heartbeat
# route's own 404 is what this boot must show, and a registry write, a 404 on any other route and
# any `answers:` line are what it must not.
heartbeat_route='PUT /v1/legion/projects/{project}'
check_boot_without_answers() {
  local lines beats
  lines=$(boot_log | jq -s -c --arg beat "$heartbeat_route" \
    '[.[] | select((.route // "") != $beat) | select((((.msg // "") | test("agent registry|answers:")) or ((.msg // "") | startswith("the Envoy listener answers 404 on"))))] | length')
  beats=$(boot_log | jq -s -c --arg beat "$heartbeat_route" '[.[] | select((.route // "") == $beat)] | length')
  note "with answers removed: $lines registry, loop or other-route 404 lines this boot (want 0); heartbeat-route lines $beats"
  [ "$lines" = 0 ]
}
# bad_cost_lines LINES: the `answers: answered` LINES (one JSON object a line) whose costUSD is not
# the configured price of the tokens they report, cache writes and cache reads included. `[]` says
# every one of them recomputes.
bad_cost_lines() {
  jq -s -c '[.[] | select(((.model // "") | test("claude-sonnet-5-5")))
    | select(((((.inputTokens // 0) * 3) + ((.outputTokens // 0) * 15) + ((.cacheWriteTokens // 0) * 3.75) + ((.cacheReadTokens // 0) * 0.3)) / 1000000 - (.costUSD // 0) | fabs) > 0.000001)]' <<<"${1:-}"
}
# cache_read_tokens LINE: the tokens an `answers: answered` LINE read from the provider's cache, 0
# for a line that reports none and for no line at all.
cache_read_tokens() { jq -r '.cacheReadTokens // 0' <<<"${1:-null}" 2>/dev/null || printf '0\n'; }
# gh_line_is LINE APP OWNER EXIT: the `answers: gh` LINE is a call of APP on OWNER that ended as
# EXIT says — `0`, `nonzero` for a refused call, or `any`. No line at all is no such call.
gh_line_is() {
  local exit_test
  case "$4" in
    0) exit_test='.exit == 0' ;;
    nonzero) exit_test='.exit != 0' ;;
    any) exit_test='true' ;;
    *) return 2 ;;
  esac
  jq -e --arg app "$2" --arg owner "$3" ".app == \$app and .owner == \$owner and ($exit_test)" <<<"${1:-null}" >/dev/null 2>&1
}

# ---- the stopped agents' conversations ---------------------------------------------------------------
# conversation_text NAME prints the agent's conversation as its store holds it: the session file
# under the files store, the session table's row under postgres (Oh My Pi's sessions), and a Claude
# Code transcript as a file under either.
conversation_text() {
  local file=${agent[$store:$1.file]}
  if [ "$store" = postgres ] && [ "$1" != pre_change ]; then
    pg_sql "omp_sessions_$run_tag" "select coalesce(string_agg(content, '' order by start_offset), '') as text from (select 0 as start_offset, content from omp_session_files where path = '$file' union all select start_offset, content from omp_session_files_parts where path = '$file') p" |
      jq -r '(.[0].text // "")'
  else
    cat -- "$file"
  fi
}
# conversation_holds NAME NEEDLE: NAME's conversation holds NEEDLE, whatever entry carries it.
conversation_holds() { conversation_text "$1" | grep -qF -- "$2"; }
# notices_in_conversation NAME NEEDLE: how many entries of NAME's conversation that are not its own
# replies hold NEEDLE (what reached it, as prompts and notices).
notices_in_conversation() {
  conversation_text "$1" | jq -R -s --arg n "$2" '[split("\n")[] | fromjson? | select((.message.role // "") != "assistant") | tojson | select(contains($n))] | length'
}
# appended_exchanges NAME [QUESTION_ID]: how many exchanges the loop appended to NAME's
# conversation, all of them or QUESTION_ID's alone. Every append ends in the marker entry naming
# the question it answered, a `legion-answer` custom entry in an Oh My Pi session
# (conversation/omp.AnswerEntryType) and a `legion-answer` line in a Claude Code transcript
# (conversation/claude.AnswerLineType). The model never sees that entry and no agent writes one,
# so it counts the loop's appends where the question's or the answer's own words cannot: an agent
# that was asked the same thing before it stopped holds those words itself.
appended_exchanges() {
  conversation_text "$1" | jq -R -s --arg q "${2:-}" '[split("\n")[] | fromjson?
    | select((.type == "custom" and .customType == "legion-answer" and ($q == "" or (.data.questionId? // "") == $q))
      or (.type == "legion-answer" and ($q == "" or (.questionId? // "") == $q)))] | length'
}
# last_assistant_text NAME: the words of the last thing NAME's Oh My Pi conversation holds as its
# own reply, which is what an agent resumed after its answers says next (A9).
last_assistant_text() {
  conversation_text "$1" |
    jq -R -r 'fromjson? | select(.type == "message" and .message.role == "assistant") | [.message.content[]? | .text? // empty] | join(" ")' |
    tail -1
}
# transcript_effort TRANSCRIPT: the effort a Claude Code transcript last ran at, which the loop's
# replay of it must carry; empty for a transcript that records none.
transcript_effort() { jq -R -r 'fromjson? | select(.type == "assistant") | .effort // empty' <<<"${1:-}" | tail -1; }
# transcript_signatures TRANSCRIPT: every thinking signature a Claude Code transcript holds, one a
# line, which the loop's replay must carry byte for byte.
transcript_signatures() {
  jq -R -r 'fromjson? | select(.type == "assistant") | .message.content[]? | select(.type == "thinking") | .signature // empty' <<<"${1:-}"
}
# answer_question_id REPLY prints the question id of the loop's answer REPLY: the id in the
# `<!-- legion-answer:<id> -->` marker every reply the loop posts carries (intake.AnswerMarker).
# A reply carrying no marker, and no reply at all, print nothing.
answer_question_id() { grep -oE 'legion-answer:[0-9a-f]+' <<<"$1" | head -1 | cut -d: -f2 || true; }
# occurrences TEXT NEEDLE: how many times NEEDLE occurs in TEXT.
occurrences() { grep -oF -- "$2" <<<"$1" | wc -l || true; }

# ---- the asker's inbox -------------------------------------------------------------------------------
# inbox_reply EVENT prints the text the asker received in reply to EVENT, or fails; inbox_replies
# EVENT counts them. A reply that never came prints nothing rather than jq's `null`, which an
# answer key would then be looked for in.
inbox_reply() {
  [ -f "$evidence/inbox.jsonl" ] || return 1
  jq -s -e -r --arg e "In Reply To: $1" '[.[] | select(.text | contains($e)) | .text] | (.[0] // empty)' "$evidence/inbox.jsonl"
}
# inbox_message EVENT prints only the message of that reply: the inbox renders an envelope as a
# reply header, `Message:`, the message, a blank line and `Topic: …` (lib/envoy-inbox.ts).
inbox_message() {
  inbox_reply "$1" | jq -R -s -r 'split("\nMessage:\n") | (.[1] // .[0]) | sub("\n\nTopic: [^\n]*\n?$"; "")'
}
inbox_replies() {
  if [ -f "$evidence/inbox.jsonl" ]; then
    jq -s --arg e "In Reply To: $1" '[.[] | select(.text | contains($e))] | length' "$evidence/inbox.jsonl"
  else
    printf '0\n'
  fi
}
# first_reply_of EVENT EVENT: the text of whichever of the two answers reached the asker first, so
# a check reads the order they were answered in (A9).
first_reply_of() {
  [ -f "$evidence/inbox.jsonl" ] || return 1
  jq -s -r --arg a "In Reply To: $1" --arg b "In Reply To: $2" \
    '[.[] | select((.text | contains($a)) or (.text | contains($b)))] | (.[0].text // empty)' "$evidence/inbox.jsonl"
}
# controller_notices KIND NEEDLE: how many notices of KIND the controller stand-in received whose
# text holds NEEDLE.
controller_notices() {
  if [ -f "$evidence/inbox.jsonl" ]; then
    jq -s --arg s "$controller_session" --arg k "\"kind\":\"$1\"" --arg n "$2" \
      '[.[] | select(.session == $s and (.text | contains($k)) and (.text | contains($n)))] | length' "$evidence/inbox.jsonl"
  else
    printf '0\n'
  fi
}

# ---- the listener's log ------------------------------------------------------------------------------
# listener_handoffs EVENT: how many hand-offs the listener published for the envelope EVENT (its
# "listener handed a message to its legion daemon" lines). Each is published with the envelope's
# dedupe key in its MsgId, `legion.handoff.<dedupe key>:<subject>`, so however many are published
# for one key the stream stores one message (cmd/listener/legion_handoff.go).
listener_handoffs() {
  jq -R -s --arg e "$1" \
    '[split("\n")[] | fromjson? | select(.msg == "listener handed a message to its legion daemon" and .event_id == $e)] | length' \
    "$evidence/logs/listener.log"
}
# listener_handoffs_to SESSION: how many hand-offs the listener published for SESSION's own
# daemon, whatever the envelope ("listener handed a message to its legion daemon" naming it).
listener_handoffs_to() {
  jq -R -s --arg s "$1" \
    '[split("\n")[] | fromjson? | select(.msg == "listener handed a message to its legion daemon" and .session_id == $s)] | length' \
    "$evidence/logs/listener.log"
}

# ---- the recording proxy's records ---------------------------------------------------------------------
# Each read below takes jq's answer into a variable and prints that: a jq that fails (the records
# file is not written yet) must leave the fallback alone, where a `jq … || printf` pipeline prints
# jq's own empty answer and the fallback's after it, two lines where a check reads one.
#
# proxy_count LABEL: how many model calls of LABEL the recording proxy has finished. A model call
# is a request whose body holds a messages array: the proxy records every request of a label, and
# two of them are no call of the model — Claude Code's `HEAD /api/hello` reachability preflight,
# which it sends as each session starts, and an Oh My Pi agent's `GET /v1/models`, both with no
# body at all. (A10 counted those too, so the resumed Claude Code reviewer's "first request" was
# the preflight, which holds no question.)
proxy_count() {
  local count
  count=$(jq -s --arg l "$1" '[.[] | select(.label == $l and (.request_body | type) == "object" and (.request_body.messages | type) == "array")] | length' \
    "$evidence/model-calls.jsonl" 2>/dev/null) || count=
  printf '%s\n' "${count:-0}"
}
# proxy_ordinal LABEL: the ordinal LABEL has reached, every request the proxy recorded for it
# counted, model call or not, which is what a hold names (hold_at, held, hold_after_tool).
proxy_ordinal() {
  local count
  count=$(jq -s --arg l "$1" '[.[] | select(.label == $l)] | length' "$evidence/model-calls.jsonl" 2>/dev/null) || count=
  printf '%s\n' "${count:-0}"
}
# first_request_after LABEL N prints the request body of LABEL's first model call after its first
# N, counted as proxy_count counts them: the preflight and the model listing the proxy records for
# the label are no model call of it, and neither is the body a check reads here.
first_request_after() {
  local body
  body=$(jq -s -c --arg l "$1" --argjson n "${2:-0}" '[.[] | select(.label == $l and (.request_body | type) == "object" and (.request_body.messages | type) == "array")] | sort_by(.ordinal) | (.[$n].request_body // null)' \
    "$evidence/model-calls.jsonl" 2>/dev/null) || body=
  printf '%s\n' "${body:-null}"
}
# daemon_request_for TEXT prints the request body of the loop's first model call (this store's
# daemon label) whose body holds TEXT.
daemon_request_for() {
  local body
  body=$(jq -s -c --arg l "daemon-$store" --arg t "$1" '[.[] | select(.label == $l and (.request_body | tostring | contains($t)))] | sort_by(.ordinal) | (.[0].request_body // null)' \
    "$evidence/model-calls.jsonl" 2>/dev/null) || body=
  printf '%s\n' "${body:-null}"
}
# request_effort REQUEST: the effort a model request carries, wherever in the body it stands;
# empty for a request that carries none and for no request at all.
request_effort() {
  local effort
  effort=$(jq -r '[.. | objects | .effort? // empty] | first // ""' <<<"${1:-null}" 2>/dev/null) || effort=
  printf '%s\n' "$effort"
}

# ---- the questions file ---------------------------------------------------------------------------
question() { jq -ce --arg id "$1" '.questions[] | select(.id == $id)' "$questions_file"; }
question_text() { question "$1" | jq -r .text; }
# answer_holds ID TEXT: TEXT holds every must_contain string of question ID, case-insensitively, and
# prints the first one missing when it does not. A question the file does not hold is no pass: it
# prints what is missing and fails, where reading its keys as none would have every text hold them.
answer_holds() {
  local key keys
  keys=$(question "$1" | jq -r '.must_contain[]?') || {
    printf 'no question %s in %s\n' "$1" "$questions_file"
    return 1
  }
  while IFS= read -r key; do
    [ -n "$key" ] || continue
    grep -qiF -- "$key" <<<"$2" || {
      printf '%s\n' "$key"
      return 1
    }
  done <<<"$keys"
}
# recorded_hit FILE TEXT: the questions file records FILE's line TEXT as a hit of A16's search that
# is not about the answer loop (a16.unrelated_hits, by its whole text or the start of it).
recorded_hit() {
  jq -e --arg f "$1" --arg t "$2" \
    'any(.a16.unrelated_hits[]; .file == $f and (.text == $t or (.starts_with as $s | $s != null and ($t | startswith($s)))))' \
    "$questions_file" >/dev/null
}

# ---- Dispatch -------------------------------------------------------------------------------------
# ask_of SESSION ISSUE prints the id of the newest ask SESSION opened on ISSUE, empty for none;
# ask_reply_by ASK SESSION prints SESSION's newest reply on ASK's thread, or fails. The asks route
# answers an array, so a body that is not one (an error object) names no ask.
ask_of() {
  dispatch_get "issues/$2/asks?thread=1" |
    jq -r --arg s "$1" 'if type == "array" then ([.[] | select(.author.id == $s)] | last | .id? // empty) else empty end' || true
}
ask_reply_by() { dispatch_get "asks/$1" | jq -e -r --arg s "$2" '[.replies[]? | select(.author.id == $s)] | (last | .body? // empty)'; }
# comment_reply_by ISSUE COMMENT SESSION prints SESSION's newest reply to COMMENT on ISSUE's own
# thread, or fails: the answer a role mention of a stopped agent is answered with (A3).
comment_reply_by() {
  dispatch_get "issues/$1/comments" |
    jq -e -r --arg id "$2" --arg s "$3" '[.[]? | select(.reply_to == $id and .author.id == $s)] | (last | .body? // empty)'
}
# record_reply_by RECORD SESSION prints SESSION's first reply in the message record a conversation
# page reads (GET agents/<session>/messages), empty for none.
record_reply_by() {
  jq -r --arg s "$2" 'first(.replies[]? | select(.author.id == $s) | .body) // empty' <<<"${1:-null}" 2>/dev/null || true
}
# collaborator_role_token COLLABORATORS SESSION prints the role token the issue's Collaborators
# view names for the current tester SESSION (the one no later session replaced), empty for none.
collaborator_role_token() {
  jq -r --arg s "$2" \
    '[.agents[]? | select(.session_id == $s and .legion.role == "tester" and .legion.replaced_by == null) | .legion.role_token] | first // empty' \
    <<<"${1:-null}" 2>/dev/null || true
}
# comment_delivered_to COMMENT SESSION: the comment Dispatch answered with records its one
# delivery as sent to SESSION.
comment_delivered_to() {
  jq -e --arg s "$2" '.deliveries[0].state == "sent" and .deliveries[0].session_id == $s' <<<"${1:-null}" >/dev/null 2>&1
}
# one_failed_delivery DELIVERIES: the delivery records are one, and it failed — what a message to a
# session or role nobody holds records, the negative control A3 and A13 share.
one_failed_delivery() {
  jq -e 'type == "array" and length == 1 and .[0].state == "failed"' <<<"${1:-null}" >/dev/null 2>&1
}
# rows_hold_paths ROWS PATH...: the omp_session_files rows hold a row for each PATH.
rows_hold_paths() {
  local rows=$1 path
  shift
  for path in "$@"; do
    jq -e --arg p "$path" 'type == "array" and any(.[]; .path == $p)' <<<"${rows:-null}" >/dev/null 2>&1 || return 1
  done
}
# scan_rows ROWS SESSION: how many of the daemon's agent_conversations ROWS record SESSION's Claude
# Code conversation found by the title scan, which is the one row a boot must have written for it.
scan_rows() {
  jq --arg s "$2" '[.[] | select(.session == $s and .source == "scan" and .harness == "claude")] | length' \
    <<<"${1:-[]}" 2>/dev/null || printf '0\n'
}
# registry_entry_matches ENTRY ISSUE: the listener's Legion agent registry holds ENTRY for the
# stopped agent of ISSUE, and says it is not live.
registry_entry_matches() {
  jq -e --arg i "$2" '.issue == $i and .live == false' <<<"${1:-null}" >/dev/null 2>&1
}
# registry_replaced_by ENTRY SESSION: that registry entry names SESSION as the session that
# replaced it (the tester the daemon restarted under a new session).
registry_replaced_by() {
  jq -e --arg s "$2" '.replaced_by == $s' <<<"${1:-null}" >/dev/null 2>&1
}

# ---- GitHub ----------------------------------------------------------------------------------------
# pr_comments NUMBER MARKER prints the smoke repository's comments on the pull request that hold
# MARKER, as {id, user, body}. GitHub's error body is an object, not the array of comments, and so
# is no comment: it prints [], as a gh call that printed nothing or failed does. The call's body is
# read into a variable rather than piped, so a failing gh leaves one list and not two (the pipe
# would print the empty list and the fallback another, under `pipefail`).
pr_comments() {
  local body
  body=$(smoke_gh api "repos/$repo/issues/$1/comments?per_page=100" 2>/dev/null) || body=
  jq -s -c --arg m "$2" '(.[0] // []) | if type == "array" then [.[] | select(.body | contains($m)) | {id, user: .user.login, body}] else [] end' <<<"$body" 2>/dev/null ||
    printf '[]\n'
}
# one_comment_by COMMENTS LOGIN SESSION: the comments holding the marker are one, posted by LOGIN,
# and its Legion footer names SESSION.
one_comment_by() {
  jq -e --arg login "$2" --arg s "$3" \
    'type == "array" and length == 1 and .[0].user == $login and (.[0].body | contains("\"session\":\"" + $s + "\""))' \
    <<<"${1:-null}" >/dev/null 2>&1
}
# refusal_mentioned REPLY: the agent's reply says GitHub refused its write.
refusal_mentioned() { grep -qiE 'refus|denied|403|404|not accessible|permission' <<<"${1:-}"; }
# reviewer_approvals NUMBER SESSION prints the approving reviews of the pull request whose Legion
# footer names SESSION, as {id, user, commit_id, footer}. GitHub's error body is an object, not the
# array of reviews, and so is no review: it prints [], as a gh call that printed nothing or failed
# does, and one list either way (pr_comments).
reviewer_approvals() {
  local body
  body=$(smoke_gh api "repos/$repo/pulls/$1/reviews" 2>/dev/null) || body=
  jq -s -c --arg s "$2" '(.[0] // []) | if type == "array" then
      [.[] | select(.state == "APPROVED" and (.body | contains("\"session\":\"" + $s + "\"")))
        | {id, user: .user.login, commit_id, footer: (.body | [capture("legion: (?<f>\\{[^}]*\\})").f] | first // null)}]
      else [] end' <<<"$body" 2>/dev/null ||
    printf '[]\n'
}
# approval_by APPROVALS LOGIN HEAD: the approvals are one, the review App LOGIN's, on HEAD.
approval_by() {
  jq -e --arg login "$2" --arg h "$3" 'type == "array" and length == 1 and .[0].user == $login and .[0].commit_id == $h' \
    <<<"${1:-null}" >/dev/null 2>&1
}
# approval_id APPROVALS: the id of the one approval, 0 for none. A review id is a bigint, which
# every reader of it compares as a string.
approval_id() { jq -r '(.[0].id // 0) | tostring' <<<"${1:-[]}" 2>/dev/null || printf '0\n'; }

# ---- the daemon's own record and state ----------------------------------------------------------------
# review_record ISSUE prints the daemon's record of ISSUE's review round: its phase, the reviewer's
# recorded decision, and the newest deciding review its pull request has seen.
review_record() {
  pg_sql "$daemon_db" "select i.phase, p.decision, r.review_seen from issues i join phases p on p.issue = i.key and p.role = 'reviewer' join pull_requests r on r.issue = i.key where i.key = '$1'" | jq -c '.[0]'
}
# review_round_undecided RECORD: the round is still open, the reviewer has decided nothing, and the
# pull request has seen no deciding review. review_seen is a bigint, which the record carries as a
# string ("0" before any review is seen), so every read of it compares strings.
review_round_undecided() {
  jq -e '.phase == "reviewing" and .decision == null and ((.review_seen // 0) | tostring) == "0"' <<<"${1:-null}" >/dev/null 2>&1
}
# review_round_ended RECORD ID: the round is no longer open and the review the daemon ended it on
# is ID (a bigint, compared as a string). No record at all ended nothing.
review_round_ended() {
  jq -e --arg id "$2" 'type == "object" and .phase != "reviewing" and ((.review_seen // 0) | tostring) == $id' <<<"${1:-null}" >/dev/null 2>&1
}
# review_round_open RECORD: the round still stands at reviewing, which no record at all also says.
review_round_open() { [ "$(jq -r '(.phase? // "reviewing")' <<<"${1:-null}" 2>/dev/null || printf 'reviewing\n')" = reviewing ]; }
# tree_is_live ROOT PHASE: the daemon's own state says ROOT's tree is live and does not linger —
# admission.trees is record.LiveRoots, the very set admission's release pass judges a root against
# (admit's releaseUnslotted) — and ROOT still stands at PHASE, which that release would have left
# at `done`.
tree_is_live() {
  legion_cmd state --json |
    jq -e --arg root "$1" --arg phase "$2" '(.admission.trees | index($root)) != null and .issues[$root].phase == $phase' >/dev/null
}
# passed_to_controller EVENT REASON: legion state lists a question passed on for REASON whose id or
# reference names EVENT.
passed_to_controller() {
  legion_cmd state --json | jq -e --arg q "$1" --arg r "$2" 'any(.questionsForController[]?; (((.id // "") + " " + (.ref // "")) | contains($q)) and .reason == $r)' >/dev/null
}
# controller_questions REASON: how many questions `legion state` lists for the controller with
# REASON.
controller_questions() {
  legion_cmd state --json | jq --arg r "$1" '[.questionsForController[]? | select(.reason == $r)] | length'
}
# tree_claims_parked CLAIMS TREE: every claim of TREE in `legion claims list --json` stands
# suspended or retired, so nothing of the tree started (A1). A listing that is no listing says
# nothing of the kind.
tree_claims_parked() {
  jq -e --arg t "$2" 'type == "object" and all(.claims[]? | select(.tree == $t); .state | IN("suspended", "retired"))' <<<"${1:-null}" >/dev/null 2>&1
}

# ---- A13: branch against base ------------------------------------------------------------------------
# a13_same FILE: branch's and base's FILE for the current store say the same thing — byte for byte,
# except the identifiers a listener mints for each event it accepts (its own and its source's id,
# its dedupe key, its trace id and the time it was issued), which no two calls can share. A role
# topic whose holder record has lapsed answers 404 with a body of its own; one whose record has
# aged out of the bus altogether answers 202 with a fresh event's, and both stacks answer the same
# way on the same snapshot.
a13_minted='del(.event_id, .source_event_id, .dedupe_key, .trace_id, .issued_at)'
a13_same() {
  local branch=$evidence/a13/branch/$store/$1 base=$evidence/a13/base/$store/$1 left right
  cmp -s "$branch" "$base" && return 0
  [ "$1" = publish.body ] || return 1
  left=$(jq -S "$a13_minted" "$branch") && right=$(jq -S "$a13_minted" "$base") || return 1
  [ "$left" = "$right" ]
}
