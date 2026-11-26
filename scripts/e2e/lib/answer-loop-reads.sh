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
#   app_keys_missing  the App key command inputs the run was not given, an array of their names,
#                  empty in workflow mode
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
# boot_listing prints the current boot's daemon log lines (boot_log) as one JSON array, and `null`
# where no log could be read: the listing a control reads once, checks for what it placed there
# (listing_fault) and hands to the read it counts (A8's lines, A14's cost lines).
boot_listing() {
  local lines
  lines=$(boot_log 2>/dev/null) || {
    printf 'null\n'
    return 0
  }
  jq -s -c . <<<"$lines"
}

# answers_lines MSG [SESSION [STATE [BECAUSE]]]: how many lines of this boot's daemon log carry MSG,
# for SESSION, in STATE and for the reason BECAUSE, whole, when each is given. A20's control reads
# the hand-offs of the tester's session the loop took for no question (`answers: hand-off is no
# question; not answered`) for the reason it gives a reply in a comment thread that follows no
# agent's comment. answers_lines_in LINES MSG… counts the same over LINES, a listing of the log
# read before (boot_listing), 0 over one that is none.
answers_lines() { answers_lines_in "$(boot_listing)" "$@"; }
answers_lines_in() {
  jq --arg m "$2" --arg s "${3:-}" --arg st "${4:-}" --arg b "${5:-}" \
    '[.[]? | select(.msg == $m and ($s == "" or .session == $s) and ($st == "" or .state == $st) and ($b == "" or .because == $b))] | length' <<<"${1:-null}"
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
# answer_dropped SESSION [LINES]: this boot dropped an answer of SESSION's because its conversation
# had started, and forwarded the question to the agent instead (A7). LINES is this boot's log as
# A7's control read it once (boot_listing), read now where not given.
answer_dropped() {
  local lines=${2:-}
  [ -n "$lines" ] || lines=$(boot_listing)
  jq -e --arg s "$1" 'any(.[]?;
    .msg == "answers: the agent'"'"'s conversation is running or moved; the answer is dropped and the question forwarded"
    and .started == true and .session == $s)' <<<"$lines" >/dev/null
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
# tools_line_in LINES prints the last `answers: tools` line of LINES, a listing of the daemon's log
# (boot_listing), the one line the daemon logs as it builds the answer loop (internal/daemon's
# answersToolsLine): `tools`, the tools the loop offers, and under each tool it does not offer a
# key of that tool's name saying why. It prints nothing for a log that logged none, as a daemon
# with `answers` removed logs none, and for a listing that is none. boot_tools_line is that line of
# this boot's log, read now; A22's and A23's controls read the log once and hand it to the read.
tools_line_in() { jq -c '[.[]? | select(.msg == "answers: tools")] | last // empty' <<<"${1:-null}"; }
boot_tools_line() { tools_line_in "$(boot_listing)"; }
# tools_line_offers LINE TOOL: the `answers: tools` LINE lists TOOL among the loop's tools. No line
# at all offers nothing.
tools_line_offers() {
  jq -e --arg t "$2" 'type == "object" and any((.tools // [])[]; . == $t)' <<<"${1:-null}" >/dev/null 2>&1
}
# tools_line_absent LINE TOOL WHY: the `answers: tools` LINE does not list TOOL and says why under
# TOOL's own key, a reason that starts with WHY: A22's daemon with no dispatch command, A23's whose
# answers set no web_search. A line that lists the tool, or names no reason, or no line at all,
# says nothing of the kind.
tools_line_absent() {
  jq -e --arg t "$2" --arg why "$3" 'type == "object" and (any((.tools // [])[]; . == $t) | not)
    and ((.[$t] // "") | type == "string" and . != "" and startswith($why))' <<<"${1:-null}" >/dev/null 2>&1
}
# check_tools_line OUTPUT prints the line `legion config check` printed naming the loop's tools, in
# the words of the boot's line: `answers: tools [gh dispatch web_search]`, then `; <tool>: <why>`
# for each tool the loop does not offer (daemon.AnswersToolsLine). Nothing for output holding none.
check_tools_line() { grep -m1 '^answers: tools \[' <<<"${1:-}" || true; }
# check_tools_lists LINE TOOL: that line lists TOOL between its brackets.
check_tools_lists() {
  local listed re='^answers: tools \[([^]]*)\]'
  [[ ${1:-} =~ $re ]] || return 1
  listed=" ${BASH_REMATCH[1]} "
  [[ $listed == *" $2 "* ]]
}
# check_tools_absent LINE TOOL WHY: that line does not list TOOL and says why after its list,
# `; TOOL: WHY…`.
check_tools_absent() {
  local re='^answers: tools \[[^]]*\]'
  [[ ${1:-} =~ $re ]] || return 1
  ! check_tools_lists "$1" "$2" && [[ ${1:-} == *"; $2: $3"* ]]
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
# app_keys_missing_names prints the App key command inputs this run was not given, as the mode
# note and the WAITING line below name them: one name, or both joined by ` and `. A run given
# both is in workflow mode and neither line is written, so the empty list names both rather than
# nothing.
app_keys_missing_names() {
  case "${#app_keys_missing[@]}" in
    1) printf '%s' "${app_keys_missing[0]}" ;;
    2) printf '%s and %s' "${app_keys_missing[0]}" "${app_keys_missing[1]}" ;;
    *) printf 'LEGION_E2E_IMPLEMENT_APP_KEY_COMMAND and LEGION_E2E_REVIEW_APP_KEY_COMMAND' ;;
  esac
}
# answering_blockers prints, one per line, what the answering criteria wait on in this run: the
# daemon's workflow (the answer loop runs in it, and it boots only with both role Apps, each from
# its own key command, so the line names the input the run was not given), and the listener's
# hand-off (plan task 6), read from this boot's log: the daemon writes the project's answering
# heartbeat at boot, and a listener without the hand-off answers its route 404.
answering_blockers() {
  [ "$mode" = workflow ] ||
    printf 'WAITING (needs %s): the daemon runs without its workflow, which carries the answer loop and boots only with both role Apps, each minting its tokens from the key its command prints\n' "$(app_keys_missing_names)"
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
# cost_price is jq's price table of the models the run answers on and prices (answer-loop.sh's
# answers_prices), USD per million tokens as [input, output, cache write, cache read]: Claude
# Sonnet 5.5, whichever harness names it, and GPT-6.1; null for any other model.
# shellcheck disable=SC2016 # a jq program: $m is jq's variable, not the shell's
cost_price='def cost_price: (.model // "") as $m
  | if ($m | test("claude-sonnet-5-5")) then [3, 15, 3.75, 0.3]
    elif $m == "openai/gpt-6.1-sol" then [2, 10, 2.5, 0.1]
    else null end; '
# bad_cost_lines LINES: the `answers: answered` LINES (one JSON object a line) of a model cost_price
# prices whose costUSD is not the price of the tokens they report, cache writes and cache reads
# included, plus webSearchUSD, what the answer's web searches cost (their tokens at the search
# model's price and the per-search fee, which the line's tokens do not hold: internal/answer's
# Reply.CostUSD). `[]` says every one of them recomputes. A line of another model is not judged
# here; unpriced_cost_lines names it.
bad_cost_lines() {
  jq -s -c "$cost_price"'[.[] | cost_price as $p | select($p != null)
    | select(((((.inputTokens // 0) * $p[0]) + ((.outputTokens // 0) * $p[1]) + ((.cacheWriteTokens // 0) * $p[2]) + ((.cacheReadTokens // 0) * $p[3])) / 1000000 + (.webSearchUSD // 0) - (.costUSD // 0) | fabs) > 0.000001)]' <<<"${1:-}"
}
# unpriced_cost_lines LINES: those of the `answers: answered` LINES whose model cost_price does not
# price, each as {question, model}, so no cost of theirs is recomputed. `[]` says every line is of
# a model A14 recomputes.
unpriced_cost_lines() {
  jq -s -c "$cost_price"'[.[] | select(cost_price == null) | {question, model}]' <<<"${1:-}"
}
# cache_read_tokens LINE: the tokens an `answers: answered` LINE read from the provider's cache, 0
# for a line that reports none and for no line at all.
cache_read_tokens() { jq -r '.cacheReadTokens // 0' <<<"${1:-null}" 2>/dev/null || printf '0\n'; }
# question_log_lines MSG QUESTION prints this boot's lines of QUESTION that carry MSG, an array
# in the order they were logged, `[]` for a question with none. QUESTION is the id every answer
# the loop posts carries in its marker (answer_question_id). A reply that never came names no
# question, and that is no line rather than the lines of some answer logged with no id, so an
# empty id reads no log at all.
question_log_lines() {
  [ -n "$2" ] || {
    printf '[]\n'
    return 0
  }
  boot_log | jq -s -c --arg m "$1" --arg q "$2" '[.[] | select(.msg == $m and .question == $q)]'
}
# first_line_where LINES TEST ARG... prints the first element of the JSON array LINES that TEST
# passes, called as `TEST <element> ARG...`, and fails when none does. A check judges the lines of
# the answer it reads (question_gh_lines, question_leak_refusals) and passes when one of them is
# the line it needs: the model may make another call after that one, and another answer may log
# its own after both. In proof run 44163479734 the Claude Code reviewer's answer read go.mod with
# two good calls, then made a third gh call with no arguments, which the tool refused; the last gh
# line of the role was that refusal, owner "" and exit -1, and A15 read the answer as made with
# no read.
first_line_where() {
  local lines=$1 predicate=$2 line
  shift 2
  while IFS= read -r line; do
    if "$predicate" "$line" "$@"; then
      printf '%s\n' "$line"
      return 0
    fi
  done < <(jq -c '.[]' <<<"${lines:-[]}" 2>/dev/null)
  return 1
}
# answered_line QUESTION prints this boot's `answers: answered` line for QUESTION, and nothing
# for a question this boot answered nothing of. A check asking one agent several questions reads
# each one's own line this way, where the session's last line is whichever answer was logged last.
answered_line() { question_log_lines "answers: answered" "$1" | jq -c 'last // empty'; }
# answered_by LINE SESSION: the `answers: answered` LINE (answered_line) is the answer of SESSION's
# agent, the session the loop answered in the name of (A21: a message to a failed role's topic,
# answered as the failed implementer). No line at all is no answer.
answered_by() { jq -e --arg s "$2" 'type == "object" and .session == $s' <<<"${1:-null}" >/dev/null 2>&1; }
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
# question_gh_lines QUESTION prints this boot's `answers: gh` lines of QUESTION
# (question_log_lines).
question_gh_lines() { question_log_lines "answers: gh" "$1"; }
# question_leak_refusals QUESTION prints this boot's lines of QUESTION where the shared leak
# guard refused a gh call (question_log_lines): the loop writes one `answers: gh call refused by
# the leak guard` line before it refuses the call, carrying the question, the seq, the issue, the
# role, the repository and the reason (internal/ghtool's Run).
question_leak_refusals() { question_log_lines "answers: gh call refused by the leak guard" "$1"; }
# leak_refusal_is LINE ISSUE ROLE REPOSITORY REASON: the refusal LINE is a call of ROLE's on
# ISSUE to REPOSITORY refused for REASON — the report the shared check printed itself, which the
# refusal carries back word for word (leakguard's Refusal). The reason is read and not the line
# alone: a guard that resolved no check at all refuses every call and writes this same line, with
# a setup failure's words in `why` and nothing of any check in it. No line at all is no refusal.
leak_refusal_is() {
  jq -e --arg i "$2" --arg r "$3" --arg repo "$4" --arg why "$5" \
    '.issue == $i and .role == $r and .repository == $repo and ((.why // "") | contains($why))' \
    <<<"${1:-null}" >/dev/null 2>&1
}
# question_dispatch_lines QUESTION prints this boot's `answers: dispatch` lines of QUESTION
# (question_log_lines): the dispatch tool's one line a call, naming the question and seq, the
# session the call ran as, its command, its arguments as run, its exit (-1 where dispatch never
# ran), whether it ran, and for a call the tool refused, the refusal (internal/dispatchtool's log).
question_dispatch_lines() { question_log_lines "answers: dispatch" "$1"; }
# dispatch_read_line_is LINE SESSION: the `answers: dispatch` LINE is a read of Dispatch — read,
# doc-read or search — that ran as SESSION and exited 0 (A22: the agent read the comment written
# after it stopped). No line at all is no read.
dispatch_read_line_is() {
  jq -e --arg s "$2" '.session == $s and .ran == true and .exit == 0 and ((.command // "") | IN("read", "doc-read", "search"))' \
    <<<"${1:-null}" >/dev/null 2>&1
}
# dispatch_refusal_is LINE SESSION COMMAND: the line is a call of COMMAND, made as SESSION, that the
# tool refused before dispatch started because COMMAND is none of the commands it runs
# (dispatchtool's refusal): A22's status write (`issue-update`), claim (`claim`) and document edit
# (`doc-edit`). A call dispatch ran, or one refused for another reason, is no such refusal.
dispatch_refusal_is() {
  jq -e --arg s "$2" --arg c "$3" '.session == $s and .ran == false and .command == $c
    and ((.refused // "") | startswith("refused: `dispatch " + $c + "` is none of the commands this tool runs"))' \
    <<<"${1:-null}" >/dev/null 2>&1
}
# lines_all_as LINES SESSION: LINES (an array of `answers: dispatch` lines) are at least one, and
# each names SESSION as the session its call ran as (A22: the log names each dispatch call with
# the agent's session).
lines_all_as() {
  jq -e --arg s "$2" 'type == "array" and length > 0 and all(.[]; .session == $s)' <<<"${1:-null}" >/dev/null 2>&1
}
# question_search_lines QUESTION prints this boot's `answers: web_search` lines of QUESTION
# (question_log_lines): the web_search tool's one line a call, with its query, the searches it
# made, what it cost (its tokens at the search model's price plus the per-search fee for each
# search) and, for a failed or refused call, what the model read (`error`; internal/websearch).
question_search_lines() { question_log_lines "answers: web_search" "$1"; }
# search_fee_carried ANSWERED LINES FEE: the `answers: answered` line ANSWERED counts the searches
# the question's `answers: web_search` LINES made, at least one, as its webSearches, and their cost
# together as its webSearchUSD; and each line that made searches cost at least FEE, the per-search
# fee answers.web_search.price_per_search sets, for each of them (A23: the cost line carries the
# search's fee). No answered line, and lines that made no search, carry no fee.
search_fee_carried() {
  jq -e -n --argjson a "${1:-null}" --argjson lines "${2:-[]}" --argjson fee "${3:-0}" '
    ($lines | if type == "array" then . else [] end) as $l
    | ([$l[] | .searches // 0] | add // 0) as $searches
    | ([$l[] | .costUSD // 0] | add // 0) as $usd
    | $a != null and $searches > 0 and ($a.webSearches // 0) == $searches
      and (((($a.webSearchUSD // 0) - $usd) | fabs) < 0.000001)
      and all($l[]; (.searches // 0) == 0 or (.costUSD // 0) >= ((.searches // 0) * $fee) - 0.000001)' >/dev/null 2>&1
}
# search_refused LINES prints the first of a question's `answers: web_search` LINES the tool
# refused because the answer had made its three searches already (websearch.MaxSearches), and
# fails when none is (A23: a fourth search in one answer is refused).
search_refused() {
  local line
  line=$(jq -c 'first(.[]? | select((.error // "") | startswith("refused: this answer has made its 3 web searches"))) // empty' \
    <<<"${1:-[]}" 2>/dev/null) || line=
  [ -n "$line" ] && printf '%s\n' "$line"
}
# review_refused ISSUE REASON: this boot set aside a review of ISSUE's pull request for REASON, the
# workflow's `workflow: a review decides nothing: <reason>` line naming ISSUE (workflow's review,
# RefusesRound). A review the review App submitted from a session that is not the reviewer's is
# refused `the review App submitted it from a session that is not the reviewer's` (A19's control).
review_refused() {
  boot_log | jq -e -s --arg i "$1" --arg m "workflow: a review decides nothing: $2" 'any(.[]; .msg == $m and .issue == $i)' >/dev/null
}
# phase_changed ISSUE FROM TO: this boot moved ISSUE from phase FROM to phase TO, the workflow's
# `workflow: phase changed` line (workflow's engine), as an approval that ends a review round moves
# its issue from reviewing to accepting (A19).
phase_changed() {
  boot_log | jq -e -s --arg i "$1" --arg f "$2" --arg t "$3" \
    'any(.[]; .msg == "workflow: phase changed" and .issue == $i and .from == $f and .to == $t)' >/dev/null
}

# ---- every tool call of every answer ---------------------------------------------------------------
# Each tool call of an answer logs one line (the `log` of internal/ghtool, internal/dispatchtool and
# internal/websearch). An `answers: gh` and an `answers: dispatch` line carry the question, the seq,
# the arguments as run, the exit, `ran` (false for a call the tool refused before it started) and
# `write`, true only for a call that ran one of the commands that write: a read, a help call and a
# refused call are no write, so no read here judges a call's arguments to know whether it wrote.
# `write` says nothing of how the call ended, so its exit is read beside it. A write that exited 0
# posted. One that exited above 0 posted nothing (a usage error, a reply to no open ask, a 4xx, a
# merge GitHub refused): it is a failed write, counted apart and no write to any verdict, so a model
# whose first try at an asked write was refused and whose second posted made the one write asked
# for. One that exited below 0 (stopped by a signal or its time bound) may have posted, and is a
# write. An `answers: web_search` line writes nothing, and a search the tool refused carries its
# refusal as `error` (store.RefusedPrefix). A daemon from before `write` was logged writes lines
# with none, and an answer with such a line has its writes and failed writes unread (`?`), never 0.
# An answer's model calls are its `answers: answered` lines' `calls`, summed over each run of it (a
# restart, or a forwarded question the loop takes back, answers it again), and its kind, model and
# fault are its last line's.
# answers_model_calls_bound is the model calls one answer may make, answers.bounds.model_calls'
# default (config's AnswerBounds), which every answers block of the run keeps but A12's model_calls
# leg's and A23's fourth search's, which set a lower one to make its fault. The run's totals count
# the answers that reached it (answer_totals); no row passes or fails on a count.
answers_model_calls_bound=8
# answer_tool_calls_jq holds the jq definitions the reads below share. write_command is the kind
# of write a call is, which a row names the writes its questions ask for by (the rig's
# asks_writes): `dispatch <command>`; `gh pr comment` for a comment on a pull request or an issue,
# by `gh pr comment`, `gh issue comment` or a gh api POST to
# `repos/<owner>/<name>/issues/<n>/comments`; `gh pr review` for a review, by `gh pr review` or a
# POST to `…/pulls/<n>/reviews`; `gh api <METHOD> <segment>` for another gh api call, its method as
# gh reads it (-X or --method, else POST with a field and GET without) and the segment the first of
# its endpoint after `repos/<owner>/<name>/` (`contents` for A17's workflow file); and `gh <command
# words>` for any other gh call, its words read as the tool reads them (ghtool's commandWords).
# tool_name is a call's tool, `gh`, `dispatch` or `web_search`. call_said is a call on one line,
# its tool, seq and arguments, cut to 120 characters, with no `|` in it, since a summary line is
# split on its first three; write_said adds the write's kind and, for one stopped below exit 0,
# that it may have posted. counts_said is an answer's counts (answer_of) on one line, its question
# cut to 8 characters (answers_clause, the rig's read_pass_answers). answers_after reads the
# answers a log names first after its first FROM lines (row_answers, pass_answers), or with EVERY
# true every answer a line after them names, keeping their `answers:` lines from there, each
# numbered (`@line`) as it stands in the log.
# shellcheck disable=SC2016 # jq's variables, not the shell's
answer_tool_calls_jq='def tool_call: .msg == "answers: gh" or .msg == "answers: dispatch" or .msg == "answers: web_search";
def tool_name: .msg | ltrimstr("answers: ");
def answers_line: ((.msg // "") | tostring | startswith("answers:")) and ((.question | type) == "string") and .question != "";
def refused_call: if .msg == "answers: web_search" then ((.error // "") | tostring | startswith("refused: ")) else .ran == false end;
def write_unread: .msg != "answers: web_search" and .ran != false and ((.write | type) != "boolean");
def posted_write: .write == true and ((.exit | type) != "number" or .exit <= 0);
def failed_write: .write == true and (.exit | type) == "number" and .exit > 0;
def gh_words: reduce (.[] | tostring) as $a ({words: [], skip: false, done: false};
  if .done then .
  elif .skip then .skip = false
  elif $a == "-R" or $a == "--repo" then .skip = true
  elif ($a | startswith("--repo=")) or (($a | startswith("-R")) and ($a | length) > 2) or $a == "--help" or ($a | startswith("--help=")) or $a == "-h" then .
  elif ($a | startswith("-")) then (if (.words | length) == 0 then .words = [$a] else . end) | .done = true
  else .words += [$a] | .done = ((.words | length) >= 2) end) | .words;
def api_path: (if (ascii_downcase | startswith("https://api.github.com/")) then .[23:]
  elif (ascii_downcase | startswith("https://uploads.github.com/")) then .[27:] else . end)
  | ltrimstr("/") | (split("?")[0] // "");
def api_call: reduce (.[] | tostring) as $a ({positionals: [], next: null, rest: false, method: null, fields: false};
  if .next != null then (if .next == "method" then .method = $a elif .next == "field" then .fields = true else . end) | .next = null
  elif .rest or ($a | startswith("-") | not) or $a == "-" then .positionals += [$a]
  elif $a == "--" then .rest = true
  elif ($a | startswith("--")) then ($a[2:] | split("=")) as $kv
    | if ($kv | length) > 1 then
        (if $kv[0] == "method" then .method = ($kv[1:] | join("=")) elif ($kv[0] | IN("field", "raw-field")) then .fields = true else . end)
      elif $kv[0] == "method" then .next = "method"
      elif ($kv[0] | IN("field", "raw-field")) then .next = "field"
      elif ($kv[0] | IN("header", "jq", "template", "input", "preview", "hostname", "cache")) then .next = "value"
      else . end
  else $a[1:2] as $c
    | if ($a | length) > 2 then (if $c == "X" then .method = $a[2:] elif ($c | IN("f", "F")) then .fields = true else . end)
      elif $c == "X" then .next = "method"
      elif ($c | IN("f", "F")) then .next = "field"
      elif ($c | IN("H", "q", "t", "p")) then .next = "value"
      else . end end)
  | {method: ((.method // (if .fields then "POST" else "GET" end)) | ascii_upcase), endpoint: ((.positionals[1] // "") | api_path)};
def write_command:
  if .msg == "answers: dispatch" then "dispatch \(.command // ((.args // [])[0]) // "")"
  elif .msg == "answers: gh" then ((.args // []) | if type == "array" then . else [] end) as $argv
    | ($argv | gh_words) as $words
    | if ($words[0] // "") == "api" then ($argv | api_call) as $api
        | (($api.endpoint | capture("^repos/[^/]+/[^/]+/(?<rest>.*)$") | .rest) // null) as $rest
        | if $api.method == "POST" and (($rest // "") | test("^issues/[0-9]+/comments$")) then "gh pr comment"
          elif $api.method == "POST" and (($rest // "") | test("^pulls/[0-9]+/reviews$")) then "gh pr review"
          else "gh api \($api.method) \((($rest // $api.endpoint) | split("/")[0]) // "")" end
      elif ($words | join(" ")) | IN("pr comment", "issue comment") then "gh pr comment"
      else "gh \($words | join(" "))" end
  else .msg end;
def call_said: "\(tool_name) #\(.seq) \((.args // []) | if type == "array" then map(tostring) | join(" ") else tostring end)"
  | gsub("\\s+"; " ") | gsub("\\|"; "¦") | if length > 120 then .[0:119] + "…" else . end;
def write_said: "\(call_said) [\(write_command)\(if (.exit | type) == "number" and .exit < 0 then ", exit \(.exit): stopped by a signal or its time bound, so it may have posted" else "" end)]";
def unread_said: "\(call_said) ran and its line carries no write field, so whether it wrote is unread";
def counts_said: "\(.question[0:8]) model calls \(.calls), gh \(.gh), dispatch \(.dispatch), web_search \(.web_search), writes \(.writes), failed writes \(.failed_writes), refused \(.refused)";
def questions_named: reduce (.[] | select(answers_line) | .question) as $q ({seen: {}, order: []};
  if .seen[$q] then . else .seen[$q] = true | .order += [$q] end) | .order;
def answers_after($from; $every): split("\n") as $raw
  | (if $every then {} else reduce ($raw[:$from][] | fromjson? | objects | select(answers_line) | .question) as $q ({}; .[$q] = true) end) as $before
  | [range($from; $raw | length) as $i | $raw[$i] | fromjson? | objects | select(answers_line) | . + {"@line": $i}] as $lines
  | {lines: $lines, questions: [($lines | questions_named)[] | select(. as $q | $before | has($q) | not)]};
def answer_of($q; $lines): [$lines[] | select(.question == $q)] as $own
  | [$own[] | select(tool_call)] as $calls
  | [$own[] | select(.msg == "answers: answered")] as $answered
  | ($answered | last) as $last
  | any($calls[]; write_unread) as $unread
  | {question: $q, kind: ($last | .kind), model: ($last | .model), calls: ([$answered[] | .calls // 0] | add // 0),
     gh: ([$calls[] | select(.msg == "answers: gh")] | length), dispatch: ([$calls[] | select(.msg == "answers: dispatch")] | length),
     web_search: ([$calls[] | select(.msg == "answers: web_search")] | length),
     writes: (if $unread then "?" else ([$calls[] | select(posted_write)] | length) end),
     failed_writes: (if $unread then "?" else ([$calls[] | select(failed_write)] | length) end),
     refused: ([$calls[] | select(refused_call)] | length), fault: ($last | .fault),
     written: [$calls[] | select(posted_write) | {tool: tool_name, seq, command: write_command, said: write_said}],
     unread: [$calls[] | select(write_unread) | {tool: tool_name, seq, said: unread_said}]}; '
# answer_counts [QUESTION...] reads a daemon log's lines on its standard input, every boot of it or
# any part, lines that are not JSON among them, and prints one JSON object a line for each answer:
# each QUESTION's, in the order given, or with none given each question an `answers:` line of the
# log names, in the order the log first names them. Each is {question, kind, model, calls, gh,
# dispatch, web_search, writes, failed_writes, refused, fault}: kind, model and fault null for a
# question the log holds no `answers: answered` line of (forwarded, dropped, or not answered yet);
# calls its model calls; gh, dispatch and web_search its calls of each tool, one a line; writes
# its writes that posted or may have, failed_writes those that exited above 0, each `?` where a call
# that ran logs no `write`; refused its calls the tool refused before they started. It reads its
# standard input and not boot_log, so a finished run's daemon log is read with no rig:
#   . scripts/e2e/lib/answer-loop-reads.sh && answer_counts <"$evidence/logs/daemon-files.log"
answer_counts() {
  jq -R -s -c "$answer_tool_calls_jq"'[split("\n")[] | fromjson? | objects] as $lines
    | (if ($ARGS.positional | length) > 0 then $ARGS.positional else ($lines | questions_named) end)[]
    | answer_of(.; $lines) | del(.written, .unread)' --args "$@"
}
# row_answers FROM [every] reads one store's daemon log on its standard input, every boot of it, and
# prints as one JSON array the answers of the row that began when the log held FROM lines: each
# question an `answers:` line after those names and no line before them names. So an answer is the
# row's whose first line came while the row ran, however often the daemon restarted inside it, since
# every boot appends to the one file, and the row before it, whose lines named the question first,
# holds none of it. With `every` (A13's control alone) each question an `answers:` line after those
# names is the row's, counted over those lines alone: a daemon answers again at its boot a question
# an earlier daemon left generating, whose first line came before. Each is answer_counts' object
# with `written`, every write that posted or may have as {tool, seq, command, said} (write_command,
# write_said), and `unread`, every call that ran and logs no `write`, as {tool, seq, said}.
row_answers() {
  jq -R -s -c --argjson from "$1" --arg named "${2:-}" "$answer_tool_calls_jq"'answers_after($from; $named == "every") as $after
    | [$after.questions[] | answer_of(.; $after.lines)]'
}
# pass_answers FROM TAKEN [every] reads one store's daemon log on its standard input once its pass
# is over, and prints as one JSON array every answer the pass's lines, those after the first FROM,
# name first (with `every`, every answer they name, row_answers' rule of that name):
# row_answers' object over the whole pass, with the row that read it and the summary entry it read
# it for (`row`, `entry`; null for an answer no row's read took) and `late`, each call of it from the
# lines that row's read had not reached that posted, may have, or logs no `write`, on one line each.
# TAKEN is what each row's read took, one JSON object a line: {question, row, entry, upto}, `upto`
# the lines of the log its read covered. A row records its result once its own read holds, and an
# answer may still be running then; what it does after is read here and nowhere else.
pass_answers() {
  jq -R -s -c --argjson from "$1" --arg taken "${2:-}" --arg named "${3:-}" "$answer_tool_calls_jq"'answers_after($from; $named == "every") as $after
    | (reduce ($taken | split("\n")[] | fromjson? | objects | select((.question | type) == "string")) as $t ({}; .[$t.question] = $t)) as $by
    | [$after.questions[] as $q | ($by[$q] // null) as $t
      | answer_of($q; $after.lines) + {row: ($t | .row), entry: ($t | .entry), late: (if $t == null then [] else
          [$after.lines[] | select(.question == $q and tool_call and .["@line"] >= $t.upto and (posted_write or write_unread))]
          | map("\($q[0:8]) \(if write_unread then unread_said else write_said end)") end)}]'
}
# writes_fault ANSWERS [WRITE...]: the writes of ANSWERS (row_answers) that posted or may have are
# WRITE..., each a write_command, no more, no fewer and no other, and every call that ran says
# whether it wrote. Otherwise it prints on one line each call that ran and logs no `write`, or, when
# every call says, each write by its question cut to 8 characters beside the writes asked for, and
# fails. A failed write is no write here. A read that fails says so, and fails: a read that cannot
# tell must not pass.
writes_fault() {
  local answers=$1 fault
  shift
  fault=$(jq -r 'if type == "array" then . else error("no answers") end
    | [.[] | .question as $q | .written[] | . + {question: $q}] as $written
    | [.[] | .question as $q | .unread[] | . + {question: $q}] as $unread
    | ($ARGS.positional | sort) as $asked
    | if ($unread | length) > 0 then [$unread[] | "\(.question[0:8]) \(.said)"] | join("; ")
      elif ($written | map(.command) | sort) == $asked then empty
      else "its answers wrote \(if ($written | length) == 0 then "nothing" else ($written | map("\(.question[0:8]) \(.said)") | join(", ")) end), where its questions ask for \(if ($asked | length) == 0 then "no write" else ($asked | join(", ")) end)" end' \
    --args "$@" <<<"${answers:-null}" 2>/dev/null) || {
    printf "the read of its answers' writes failed\n"
    return 1
  }
  [ -z "$fault" ] || {
    printf '%s\n' "$fault"
    return 1
  }
}
# answers_clause ANSWERS prints what a row's detail ends with: answers_clause_lead, `; its
# answers: `, then for each of ANSWERS (row_answers) its counts (counts_said: its question cut to 8
# characters, its model calls, its calls of each tool, its writes, its failed writes and its refused
# calls), `; ` between two, or `none`. A read that fails says so. The rig's fail_entry finds the
# clause in a summary line by answers_clause_lead.
answers_clause_lead='; its answers: '
answers_clause() {
  local clause
  clause=$(jq -r "$answer_tool_calls_jq"'if length == 0 then "none" else map(counts_said) | join("; ") end' \
    <<<"${1:-null}" 2>/dev/null) || clause=
  printf '%s%s\n' "$answers_clause_lead" "${clause:-unread}"
}
# answer_calls_header prints the header of the run's answer-calls.tsv, and answers_tsv STORE ROW
# ANSWERS one line of it for each of ANSWERS (pass_answers): the store, the row that read it (ROW
# for an answer that names none), the question, the answer's kind and model, its model calls, its
# gh, dispatch and web_search calls, its writes, its failed writes, its refused calls and its fault,
# tab-separated, `-` for none.
answer_calls_header() {
  printf 'store\trow\tquestion\tkind\tmodel\tmodel_calls\tgh_calls\tdispatch_calls\tweb_search_calls\twrites\tfailed_writes\trefused_calls\tfault\n'
}
answers_tsv() {
  jq -r --arg store "$1" --arg row "$2" 'def dash: if . == null or . == "" then "-" else tostring end;
    .[]? | [$store, (.row // $row), .question, (.kind | dash), (.model | dash), .calls, .gh, .dispatch, .web_search, .writes, .failed_writes, .refused, (.fault | dash)]
    | map(tostring) | @tsv' <<<"${3:-[]}"
}
# answer_totals FILE prints, one line a store, the totals of answer-calls.tsv FILE: its answers, their
# model calls, their tool calls and each tool's, their writes and failed writes (`?` when any
# answer's are unread), their refused calls, and how many answers reached answers_model_calls_bound.
# Nothing for a file that holds its header alone.
answer_totals() {
  jq -R -s -r --argjson bound "$answers_model_calls_bound" 'def n: tonumber? // 0;
    def sum($rows; $i): if any($rows[]; .[$i] == "?") then "?" else ([$rows[] | .[$i] | n] | add) end;
    [split("\n")[1:][] | select(. != "") | split("\t")] | group_by(.[0])[] | . as $rows
    | ([$rows[] | .[6] | n] | add) as $gh | ([$rows[] | .[7] | n] | add) as $dispatch | ([$rows[] | .[8] | n] | add) as $web
    | "\($rows[0][0]): \($rows | length) answers, \([$rows[] | .[5] | n] | add) model calls, \($gh + $dispatch + $web) tool calls (gh \($gh), dispatch \($dispatch), web_search \($web)), writes \(sum($rows; 9)), failed writes \(sum($rows; 10)), refused \([$rows[] | .[11] | n] | add), \([$rows[] | select((.[5] | n) >= $bound)] | length) reached the \($bound)-call bound"' "$1"
}

# ---- the stopped agents' conversations ---------------------------------------------------------------
# conversation_text NAME prints the agent's conversation as its store holds it: the session file
# under the files store, the session table's row under postgres (Oh My Pi's sessions), and a Claude
# Code transcript as a file under either.
conversation_text() {
  local file=${agent[$store:$1.file]}
  if [ "$store" = postgres ] && [ "$1" != pre_change ]; then
    pg_sql "omp_sessions_$run_tag" "select coalesce(string_agg(content, '' order by start_offset), '') as text from (select 0 as start_offset, content from public.omp_session_files where path = '$file' union all select start_offset, content from public.omp_session_files_parts where path = '$file') p" |
      jq -r '(.[0].text // "")'
  else
    cat -- "$file"
  fi
}
# conversation_holds NAME NEEDLE: NAME's conversation holds NEEDLE, whatever entry carries it. The
# whole conversation is read before it is searched, never piped into a search that stops at its
# first match: under the session table it is one jq string, and a `grep -q` that closes the pipe
# early fails jq's write, which `pipefail` makes the read's answer (A7 and A15 read a conversation
# holding the needle as one that did not, beside `jq: error: writing output failed: Broken pipe`).
# A session file longer than the pipe's buffer does the same to `cat`.
conversation_holds() {
  local text
  text=$(conversation_text "$1") || return 1
  grep -qF -- "$2" <<<"$text"
}
# conversation_holds_after NAME NEEDLE AFTER: NAME's conversation holds NEEDLE in an entry after the
# first entry that holds AFTER, and fails when no entry holds AFTER: what followed one question
# alone, its own suffix the AFTER (A15, and its control, whose question a conversation still
# holding earlier answers never received). The conversation is read whole first, as
# conversation_holds reads it.
conversation_holds_after() {
  local text
  text=$(conversation_text "$1") || return 1
  jq -R -s -e --arg n "$2" --arg a "$3" '$a != "" and ([split("\n")[] | fromjson?] as $e
    | ([range($e | length) | select($e[.] | tojson | contains($a))] | first) as $at
    | $at != null and any($e[$at + 1:][]; tojson | contains($n)))' <<<"$text" >/dev/null 2>&1
}
# text_compactions NAME: how many compactions NAME's Oh My Pi conversation holds whose summary is
# text the loop's reader reads. The reader declines the two others (compactionSummary in
# packages/daemon/internal/conversation/omp/context.go): Oh My Pi's image-based snapcompact, an
# entry whose preserveData carries `snapcompact`, which it writes for a compaction that carries no
# instructions, a person's bare `/compact` among them; and a context rollover (details.kind
# `experimental-context-rollover`). A question to a conversation holding either is passed on, not
# answered.
text_compactions() {
  conversation_text "$1" | jq -R -s '[split("\n")[] | fromjson? | select(.type == "compaction")
    | select(((.preserveData? | objects | has("snapcompact")) // false) | not)
    | select((.details.kind? // "") != "experimental-context-rollover")] | length'
}
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
# assistant_texts_after NAME NEEDLE prints, one a line, the words of every reply NAME's Oh My Pi
# conversation holds after the last entry not its own that holds NEEDLE: everything the agent said
# in the turn that entry started, which is what an agent resumed after its answers says next (A9,
# whose NEEDLE is the task it was delivered). Every reply of the turn is read and not the last: a
# resumed agent may answer a person's earlier reply in the same turn, and its last words then
# answer that. A reply holding no text (a tool call alone) prints no line, and a conversation with
# no such entry prints nothing.
assistant_texts_after() {
  conversation_text "$1" | jq -R -s -r --arg n "$2" '[split("\n")[] | fromjson?] as $entries
    | ([range($entries | length) | select(($entries[.].message.role? // "") != "assistant" and ($entries[.] | tojson | contains($n)))] | last) as $at
    | if $at == null then empty else
        $entries[$at + 1:][] | select(.type == "message" and .message.role == "assistant")
          | [.message.content[]? | .text? // empty] | join(" ") | select(. != "")
      end'
}
# transcript_effort TRANSCRIPT: the effort a Claude Code transcript last ran at, which the loop's
# replay of it must carry; empty for a transcript that records none.
transcript_effort() { jq -R -r 'fromjson? | select(.type == "assistant") | .effort // empty' <<<"${1:-}" | tail -1; }
# transcript_signatures TRANSCRIPT: every thinking signature a Claude Code transcript holds, one a
# line, which the loop's replay must carry byte for byte.
transcript_signatures() {
  jq -R -r 'fromjson? | select(.type == "assistant") | .message.content[]? | select(.type == "thinking") | .signature // empty' <<<"${1:-}"
}
# transcript_thinking TRANSCRIPT: how many thinking blocks a Claude Code transcript's own turns
# hold. None leaves A10's byte-for-byte clause with nothing to replay, which the check reads as a
# FAIL rather than as every signature carried.
transcript_thinking() {
  jq -R -s '[split("\n")[] | fromjson? | select(.type == "assistant") | .message.content[]? | select(.type? == "thinking")] | length' <<<"${1:-}"
}
# omp_thinking_level TRANSCRIPT: the thinking level an Oh My Pi conversation last ran at, as the
# loop's reader reads it (conversation/omp's buildContext): its last thinking_level_change, `off`
# for a change that names no level and for a conversation with no change at all.
omp_thinking_level() {
  local level
  level=$(jq -R -s -r '[split("\n")[] | fromjson? | objects | select(.type == "thinking_level_change")]
    | last | if . == null then "off" else (.thinkingLevel // "off") end' <<<"${1:-}" 2>/dev/null) || level=
  printf '%s\n' "${level:-off}"
}
# omp_turns holds two jq definitions over an Oh My Pi session's raw text: assistant_turns, its
# assistant messages in order, and failed, a turn the loop's reader drops, one that ended aborted
# or in error (conversation/omp's dropFailedTurns), with what it said and asked.
# openai_reasoning's encrypted_reasoning reads an OpenAI reasoning item, as an Oh My Pi turn stores
# it, a Responses request carries it or a response returns it: {id, encrypted_content} for an
# object of type `reasoning` with both, which is the only kind a stateless call can carry back,
# and nothing for anything else.
omp_turns='def assistant_turns: [split("\n")[] | fromjson? | objects
  | select(.type == "message" and .message.role? == "assistant") | .message];
def failed: (.stopReason? // "") | IN("aborted", "error"); '
openai_reasoning='def encrypted_reasoning: objects
  | select(.type? == "reasoning" and (.id? | type) == "string" and (.encrypted_content? | type) == "string")
  | {id, encrypted_content}; '
# omp_reasoning_items TRANSCRIPT prints, as one JSON array, the OpenAI reasoning items an Oh My Pi
# conversation's assistant turns store, each as {id, encrypted_content}: the items of type
# `reasoning` in a turn's providerPayload (an openaiResponsesHistory), those with both an id and
# encrypted content, which the loop's replay must carry as they are stored. A failed turn's items
# are none of them, since the loop's reader drops that turn. `[]` for none.
omp_reasoning_items() {
  local items
  items=$(jq -R -s -c "$omp_turns$openai_reasoning"'[assistant_turns[] | select(failed | not)
    | (.providerPayload.items? // [])[]? | encrypted_reasoning]' <<<"${1:-}" 2>/dev/null) || items=
  printf '%s\n' "${items:-[]}"
}
# omp_openai_summary TRANSCRIPT: what an Oh My Pi conversation's own turns hold, as one JSON object:
# how many assistant turns and how many of them failed (omp_turns), the distinct
# `<provider>/<api>/<model>` they ran on, and, of the turns that did not fail, the tool calls and
# the OpenAI reasoning items, all of them (`reasoning`) and those with an id and encrypted content
# (`encrypted`, omp_reasoning_items). A GPT-6.1 transcript whose `encrypted` or `tool_calls` is 0
# leaves A10's GPT-6.1 clause short of what it replays, which the check reads as a FAIL.
omp_openai_summary() {
  local summary
  summary=$(jq -R -s -c "$omp_turns$openai_reasoning"'assistant_turns as $all | [$all[] | select(failed | not)] as $turns
    | [$turns[] | (.providerPayload.items? // [])[]? | objects | select(.type? == "reasoning")] as $reasoning
    | {turns: ($all | length), failed: ([$all[] | select(failed)] | length),
       models: ([$all[] | [.provider, .api, .model] | map(. // "-") | join("/")] | unique),
       tool_calls: ([$turns[] | .content[]? | objects | select(.type == "toolCall")] | length),
       reasoning: ($reasoning | length), encrypted: ([$reasoning[] | encrypted_reasoning] | length)}' \
    <<<"${1:-}" 2>/dev/null) || summary=
  [ -n "$summary" ] || summary='{"turns":0,"failed":0,"models":[],"tool_calls":0,"reasoning":0,"encrypted":0}'
  printf '%s\n' "$summary"
}
# answer_question_id REPLY prints the question id of the loop's answer REPLY: the id in the
# `<!-- legion-answer:<id> -->` marker every reply the loop posts carries (intake.AnswerMarker).
# A reply carrying no marker, and no reply at all, print nothing.
answer_question_id() { grep -oE 'legion-answer:[0-9a-f]+' <<<"$1" | head -1 | cut -d: -f2 || true; }
# occurrences TEXT NEEDLE: how many times NEEDLE occurs in TEXT.
occurrences() { grep -oF -- "$2" <<<"$1" | wc -l || true; }
# question_suffix ID TAG MARKER: the suffix the rig ends a question it will count with, which names
# the question ID, the run TAG and the MARKER of the check that asks it. The ask and the count
# (question_occurrences) both take it from here, so the two cannot drift apart.
question_suffix() { printf '(question %s of run %s, %s)' "$1" "$2" "$3"; }
# question_occurrences REQUEST ID TAG MARKER: how many times the question ID of run TAG asked under
# MARKER occurs in REQUEST, counted by the whole suffix the rig asks it with (question_suffix) and
# not by MARKER alone: a model's own reply can name the marker ("the same as for the earlier
# questions, A10-1 included"), and a later request carries that reply in the conversation, so the
# marker alone counts the question twice (proof run 44858316560, A10 under the postgres store).
question_occurrences() { occurrences "$1" "$(question_suffix "$2" "$3" "$4")"; }

# ---- the asker's inbox -------------------------------------------------------------------------------
# inbox_listing prints the inbox, every envelope the run's stand-in sessions were relayed, as one
# JSON array of {at, session, text}, oldest first, and `null` where the run recorded no inbox: the
# listing a control reads once, checks (listing_fault) and hands to the read it counts. The reads
# below that a control counts take it as INBOX after their own arguments, and read the inbox as it
# stands now where it is not given. inbox_jq INBOX ARG... runs jq ARG... over INBOX, or over the
# inbox file read as one array, and fails where neither is.
inbox_listing() {
  local inbox=null
  if [ -f "$evidence/inbox.jsonl" ]; then inbox=$(jq -s -c . "$evidence/inbox.jsonl" 2>/dev/null) || inbox=null; fi
  printf '%s\n' "$inbox"
}
inbox_jq() {
  local inbox=$1
  shift
  if [ -n "$inbox" ]; then
    jq "$@" <<<"$inbox"
  else
    [ -f "$evidence/inbox.jsonl" ] || return 1
    jq -s "$@" "$evidence/inbox.jsonl"
  fi
}
# inbox_reply EVENT [INBOX] prints the text the asker received in reply to EVENT, or fails;
# inbox_replies EVENT [INBOX] counts them. A reply that never came prints nothing rather than jq's
# `null`, which an answer key would then be looked for in. An empty EVENT, a send the listener named
# no event for, is answered by nothing: `In Reply To: ` alone is in every reply, and would read
# another question's answer as this one's.
# shellcheck disable=SC2016 # jq's variables, run through inbox_jq
inbox_reply() {
  [ -n "${1:-}" ] || return 1
  inbox_jq "${2:-}" -e -r --arg e "In Reply To: $1" '[.[]? | select(.text | contains($e)) | .text] | (.[0] // empty)'
}
# reply_message TEXT prints only the message of the reply TEXT: the inbox renders an envelope as a
# reply header, `Message:`, the message, a blank line and `Topic: …` (lib/envoy-inbox.ts). Nothing
# for no text.
reply_message() {
  [ -n "${1:-}" ] || return 0
  jq -R -s -r 'split("\nMessage:\n") | (.[1] // .[0]) | sub("\n\nTopic: [^\n]*\n?$"; "")' <<<"$1"
}
# inbox_message EVENT prints only the message of the reply to EVENT (reply_message), nothing for
# none.
inbox_message() {
  reply_message "$(inbox_reply "${1:-}" 2>/dev/null || true)"
}
# inbox_holds SESSION MARKER [INBOX]: the inbox recorded an envelope relayed to SESSION whose text
# holds MARKER (A21's first holder: the message the listener kept for a role reaches the session
# that claimed it). An empty MARKER is no message, and holds nothing.
# shellcheck disable=SC2016 # jq's variables, run through inbox_jq
inbox_holds() {
  [ -n "${2:-}" ] &&
    inbox_jq "${3:-}" -e --arg s "$1" --arg m "$2" 'any(.[]?; .session == $s and (.text | contains($m)))' >/dev/null
}
# shellcheck disable=SC2016 # jq's variables, run through inbox_jq
inbox_replies() {
  local count=0
  if [ -n "${1:-}" ]; then
    count=$(inbox_jq "${2:-}" --arg e "In Reply To: $1" '[.[]? | select(.text | contains($e))] | length') || count=0
  fi
  printf '%s\n' "$count"
}
# replied_with_keys EVENT ID [INBOX]: the asker received a reply to EVENT (inbox_reply), and it holds
# every key of question ID (answer_holds). It prints what is missing, the reply or the first key it
# lacks, and nothing when it holds. A2's, A4's and A15's answers are read so, each by its own event.
replied_with_keys() {
  local reply
  reply=$(inbox_reply "$1" "${3:-}") || {
    printf 'no reply to %s\n' "${1:-a send the listener named no event for}"
    return 1
  }
  answer_holds "$2" "$reply"
}
# no_reply_to EVENT [INBOX]: the asker received no reply to EVENT, a send the listener named an
# event for: nothing was posted on the asker's topic in answer to it (A7's question forwarded to the
# running agent, which the loop posts nothing for). An empty EVENT is no send at all, and shows
# nothing.
no_reply_to() { [ -n "${1:-}" ] && [ "$(inbox_replies "$1" "${2:-}")" = 0 ]; }
# replies_to INBOX EVENT...: the replies the asker received to the sends whose answers named EVENT...,
# each event's replies counted once. The listener answers a sender's own repeat under one key with
# its first copy's event (cmd/listener's answerHandedOff), so every send of one envelope can name
# one event, and a sum over the sends would count that event's one reply once a send (proof run
# 45136574601, A8: one reply read as 3). INBOX is the inbox as A8's control read it once
# (inbox_listing), empty to read it now.
replies_to() {
  local inbox=$1 event total=0
  shift
  for event in $(printf '%s\n' "$@" | sort -u); do
    total=$((total + $(inbox_replies "$event" "$inbox")))
  done
  printf '%s\n' "$total"
}
# first_reply_of EVENT EVENT [INBOX]: the text of whichever of the two answers reached the asker
# first, so a check reads the order they were answered in (A9). Either event empty is no pair of
# sends.
# shellcheck disable=SC2016 # jq's variables, run through inbox_jq
first_reply_of() {
  [ -n "${1:-}" ] && [ -n "${2:-}" ] || return 1
  inbox_jq "${3:-}" -r --arg a "In Reply To: $1" --arg b "In Reply To: $2" \
    '[.[]? | select((.text | contains($a)) or (.text | contains($b)))] | (.[0].text // empty)'
}
# controller_notices KIND NEEDLE [INBOX]: how many notices of KIND the controller stand-in received
# whose text holds NEEDLE. An empty NEEDLE, a person's reply Dispatch took none of, names no notice.
# shellcheck disable=SC2016 # jq's variables, run through inbox_jq
controller_notices() {
  local count=0
  if [ -n "${2:-}" ]; then
    count=$(inbox_jq "${3:-}" --arg s "$controller_session" --arg k "\"kind\":\"$1\"" --arg n "$2" \
      '[.[]? | select(.session == $s and (.text | contains($k)) and (.text | contains($n)))] | length') || count=0
  fi
  printf '%s\n' "$count"
}

# ---- the listener's log ------------------------------------------------------------------------------
# listener_handoffs_to SESSION: how many hand-offs the listener published for SESSION's own
# daemon, whatever the envelope ("listener handed a message to its legion daemon" naming it).
listener_handoffs_to() {
  jq -R -s --arg s "$1" \
    '[split("\n")[] | fromjson? | select(.msg == "listener handed a message to its legion daemon" and .session_id == $s)] | length' \
    "$evidence/logs/listener.log"
}
# listener_stored_handoffs_to SESSION: how many of those hand-offs the stream stored as a new copy.
# A hand-off under a key the stream already holds is logged too, with `duplicate: true`, and is
# answered with the copy stored first; a line that names no `duplicate` counts as stored.
listener_stored_handoffs_to() {
  jq -R -s --arg s "$1" \
    '[split("\n")[] | fromjson? | select(.msg == "listener handed a message to its legion daemon" and .session_id == $s and (.duplicate // false) == false)] | length' \
    "$evidence/logs/listener.log"
}

# ---- the listener's answers ----------------------------------------------------------------------------
# consumer_registration ANSWER SESSION [TOPIC...]: the listener's ANSWER to SESSION's registration
# (POST /v1/interests/subscribe) gives SESSION a consumer of its own, relayed on SESSION's relay
# subject (`notifications.envoy.deliver.<session>`, the one subject the run's inbox reads for it),
# and stores every TOPIC. A registration that asks for no consumer is refused `409 not_requested`
# with no delivery at all, and a controller lane is stored only for a session holding that
# project's controller role, so an answer that left one out does not hold it.
consumer_registration() {
  local answer=$1 session=$2
  shift 2
  jq -e --arg s "$session" '.session_id == $s and .delivery.mode == "consumer"
    and .delivery.subject == "notifications.envoy.deliver." + $s
    and ((.topics // []) as $stored | all($ARGS.positional[]; . as $topic | any($stored[]; . == $topic)))' \
    --args "$@" <<<"${answer:-null}" >/dev/null 2>&1
}
# stored_for_stopped BODY SESSION: the listener's answer to a send stored the message for SESSION as
# a session that is not running (`state: stopped`), the answer a send to a stopped agent gets with
# `answers` removed, on the base commit as on the branch. A send handed to a stopped agent's daemon
# names its recipient with no state, and a session Envoy knows nothing of is a 404 naming none.
stored_for_stopped() {
  jq -e --arg s "$2" '.recipient == $s and .state == "stopped"' <<<"${1:-null}" >/dev/null 2>&1
}

# ---- the recording proxy's records ---------------------------------------------------------------------
# Each read below takes jq's answer into a variable and prints that: a jq that fails (the records
# file is not written yet) must leave the fallback alone, where a `jq … || printf` pipeline prints
# jq's own empty answer and the fallback's after it, two lines where a check reads one.
#
# proxy_count LABEL: how many model calls of LABEL the recording proxy has finished. A model call
# is a request whose body holds a messages array (Anthropic's Messages API) or an input array
# (OpenAI's Responses API), which jq's model_call below tests: the proxy records every request of a
# label, and two of them are no call of the model — Claude Code's `HEAD /api/hello` reachability
# preflight, which it sends as each session starts, and an Oh My Pi agent's `GET /v1/models`, both
# with no body at all. (A10 counted those too, so the resumed Claude Code reviewer's "first
# request" was the preflight, which holds no question.)
model_call='def model_call: (.request_body | type) == "object"
  and (((.request_body.messages | type) == "array") or ((.request_body.input | type) == "array")); '
proxy_count() {
  local count
  count=$(jq -s --arg l "$1" "$model_call"'[.[] | select(.label == $l and model_call)] | length' \
    "$evidence/model-calls.jsonl" 2>/dev/null) || count=
  printf '%s\n' "${count:-0}"
}
# proxy_ordinal LABEL: the ordinal LABEL has reached, every request the proxy recorded for it
# counted, model call or not, which is what a hold names (hold_at). The proxy records a request
# once it ends, so a request still in flight has an ordinal past this count: a check that must
# know the ordinal of a held call reads it off the proxy's own held list (held_since).
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
  body=$(jq -s -c --arg l "$1" --argjson n "${2:-0}" "$model_call"'[.[] | select(.label == $l and model_call)] | sort_by(.ordinal) | (.[$n].request_body // null)' \
    "$evidence/model-calls.jsonl" 2>/dev/null) || body=
  printf '%s\n' "${body:-null}"
}
# answer_calls LABEL MARKER [LATER] prints, as one JSON array in ordinal order, the proxy's records
# of LABEL's model calls whose request body holds MARKER and not LATER: the calls of the one answer
# MARKER's question was (A10's GPT-6.1 clause). Every call of an answer carries its question, and
# every call of a later answer carries the exchange the loop appended for it, that question's
# marker with it, so LATER, the next question's own marker, leaves the later answer's calls out.
# `[]` for none.
answer_calls() {
  local calls
  calls=$(jq -s -c --arg l "$1" --arg m "$2" --arg later "${3:-}" "$model_call"'[.[] | select(.label == $l and model_call)
    | select((.request_body | tostring) as $b | ($b | contains($m)) and ($later == "" or ($b | contains($later) | not)))]
    | sort_by(.ordinal)' "$evidence/model-calls.jsonl" 2>/dev/null) || calls=
  printf '%s\n' "${calls:-[]}"
}
# calls_on_model CALLS PATH MODEL: CALLS (answer_calls) are at least one call, and each is a POST
# to PATH whose request names MODEL: the answer was made on that API and on that model.
calls_on_model() {
  jq -e --arg p "$2" --arg m "$3" \
    'type == "array" and length > 0 and all(.[]; .method == "POST" and .path == $p and .request_body.model? == $m)' \
    <<<"${1:-null}" >/dev/null 2>&1
}
# calls_efforts CALLS prints the reasoning effort each of CALLS' requests carries, OpenAI's
# `reasoning.effort`, as one JSON array in call order, null for a request that carries none.
# efforts_all CALLS EFFORT: CALLS are at least one call, and every request carries EFFORT.
calls_efforts() {
  local efforts
  efforts=$(jq -c '[(if type == "array" then .[] else empty end) | (.request_body.reasoning.effort? // null)]' \
    <<<"${1:-null}" 2>/dev/null) || efforts=
  printf '%s\n' "${efforts:-[]}"
}
efforts_all() {
  jq -e --arg e "$2" 'length > 0 and all(.[]; . == $e)' <<<"$(calls_efforts "$1")" >/dev/null 2>&1
}
# reasoning_replayed TRANSCRIPT REQUEST prints `<carried> <stored> [<missing>]`: how many reasoning
# items an Oh My Pi TRANSCRIPT stores (omp_reasoning_items), how many of them REQUEST, the body of
# an OpenAI Responses call, carries as a reasoning input item with that id and that encrypted
# content, byte for byte, and the ids of those it does not carry, joined by commas. The replay
# holds when the transcript stores some and the request carries each one; `0 0` is a transcript
# that stores none and leaves nothing to replay. The two go to jq on its standard input, never as
# arguments: a conversation's encrypted content outgrows one. A request that is no JSON carries
# none of them.
reasoning_replayed() {
  local stored answer
  stored=$(omp_reasoning_items "$1")
  answer=$(jq -n -r "$openai_reasoning"'(input) as $stored | (input) as $request
    | [$request | .. | encrypted_reasoning] as $sent
    | [$stored[] | select(. as $item | any($sent[]; . == $item) | not) | .id] as $missing
    | "\(($stored | length) - ($missing | length)) \($stored | length) \($missing | join(","))"' \
    <<<"${stored:-[]}"$'\n'"${2:-null}" 2>/dev/null) ||
    answer=$(jq -r '"0 \(length) \(map(.id) | join(","))"' <<<"${stored:-[]}" 2>/dev/null) || answer=
  answer=${answer% }
  printf '%s\n' "${answer:-0 0}"
}
# reasoning_carried CALLS prints `<carried> <owed>` for one answer's model calls (answer_calls):
# owed counts each reasoning item a call's response returned, paired with each later call of the
# answer, and carried the pairs whose later request holds a reasoning item of that id with that
# encrypted content. What a response returned is what its `response.output_item.done` events hold:
# the SDK's stream adapter builds the next step's reasoning from that event
# (grafana/ai-sdk providers/openai, stream_adapter.go, handleOutputItemDone), and agentloop keeps
# the same items as the step's Step.Native (packages/envoy/agentloop/run.go, readRaw), which the
# append stores. The proxy keeps a streamed answer as the text it streamed, which also holds each
# item in `response.output_item.added`, shorter, and in `response.completed`, whose encrypted
# content differs from the done event's (measured through the model gateway on 2026-11-18):
# neither is what the next step carries. The answer's last call has no later one, so what it alone
# returned is owed nowhere, and `0 0` says no step but the last returned reasoning at all. A call
# whose response ended incomplete (a `response.incomplete` event: the provider cut it for length)
# owes nothing either: the daemon makes such a call once more (agentloop's
# Request.CallAgainWhenCut, which it sets to 1), sent the cut call's request byte for byte, so by
# design no later request holds anything the cut call returned. A cut call that is not the
# answer's last is always one made again; the last call owes nothing anyway.
reasoning_carried() {
  local answer
  answer=$(jq -r "$openai_reasoning"'def returned:
      if type == "string" then
        [split("\n")[] | rtrimstr("\r") | select(startswith("data:")) | ltrimstr("data:") | ltrimstr(" ")
          | fromjson? | objects] as $events
        | if any($events[]; .type == "response.incomplete") then []
          else [$events[] | select(.type == "response.output_item.done") | .item | encrypted_reasoning] end
      else [] end;
    def sent: [.request_body | .. | encrypted_reasoning];
    (if type == "array" then . else [] end) as $calls
    | [range($calls | length) as $i | ($calls[$i].response_body | returned)[] as $item
        | range($i + 1; $calls | length) as $j | ($calls[$j] | sent) | any(.[]; . == $item)]
    | "\(map(select(.)) | length) \(length)"' <<<"${1:-[]}" 2>/dev/null) || answer=
  printf '%s\n' "${answer:-0 0}"
}
# carried_total CARRIED... sums reasoning_carried's `<carried> <owed>` lines, one an answer's, into
# one such line: of the reasoning earlier steps of all those answers returned, how many pairs a
# later request of the same answer carried, and how many were owed. A line that is not two counts
# adds nothing, so no answer at all is `0 0`.
carried_total() {
  local line carried owed sum_carried=0 sum_owed=0
  for line in "$@"; do
    read -r carried owed <<<"$line" || true
    [[ $carried =~ ^[0-9]+$ && $owed =~ ^[0-9]+$ ]] || continue
    sum_carried=$((sum_carried + carried))
    sum_owed=$((sum_owed + owed))
  done
  printf '%s %s\n' "$sum_carried" "$sum_owed"
}
# a10_next_question ASKED CARRIED... prints the id of the question A10's GPT-6.1 leg asks next,
# given that it has asked ASKED of the questions file's a10.reasoning list and that CARRIED are the
# reasoning_carried lines of every answer it has asked, the two cache answers' first: the list's
# next question while those answers owe nothing, since then no step that returned reasoning had a
# later step after it and clause (c) has nothing to judge. It prints nothing once some answer owed
# a carry, whether or not the later request carried it, and nothing once the list is spent, which
# is the leg's bound. In proof run 44461333495 no step of the four cache answers returned
# reasoning at all (reasoning_tokens 0 in each), which is the case this read asks again for.
a10_next_question() {
  local asked=$1 total next
  shift
  total=$(carried_total "$@")
  [ "${total#* }" = 0 ] || return 0
  next=$(jq -r --argjson n "$asked" 'if ($n | type) == "number" and $n >= 0 then (.a10.reasoning[$n]? // empty) else empty end' \
    "$questions_file" 2>/dev/null) || next=
  printf '%s\n' "$next"
}
# call_cached_tokens CALL prints the input tokens the model read from the provider's cache for one
# OpenAI Responses call, a record of the proxy (answer_calls): the response's own
# usage.input_tokens_details.cached_tokens, off the `response.completed` event of a streamed
# answer or off the response itself. 0 for a call that reports none and for no call at all. It is
# the one call's read, where an `answers: answered` line's cacheReadTokens sums every call of the
# answer, and a later call of an answer reads what an earlier call of that same answer wrote.
call_cached_tokens() {
  local tokens
  tokens=$(jq -r '(.response_body? // null)
    | if type == "string" then
        [split("\n")[] | rtrimstr("\r") | select(startswith("data:")) | ltrimstr("data:") | ltrimstr(" ")
          | fromjson? | objects | select(.type == "response.completed")
          | (.response.usage.input_tokens_details.cached_tokens? // 0)] | last // 0
      elif type == "object" then (.usage.input_tokens_details.cached_tokens? // 0)
      else 0 end' <<<"${1:-null}" 2>/dev/null) || tokens=
  printf '%s\n' "${tokens:-0}"
}
# daemon_call_for TEXT prints the proxy's record of the loop's first model call (this store's daemon
# label) whose request body holds TEXT, `null` for none, and daemon_request_for TEXT that call's
# request body. The first is the earliest by ordinal: a later answer's request carries the
# exchanges the loop appended before it, and with them the text of their questions.
daemon_call_for() {
  local call
  call=$(jq -s -c --arg l "daemon-$store" --arg t "$1" '[.[] | select(.label == $l and (.request_body | tostring | contains($t)))] | sort_by(.ordinal) | (.[0] // null)' \
    "$evidence/model-calls.jsonl" 2>/dev/null) || call=
  printf '%s\n' "${call:-null}"
}
daemon_request_for() {
  local body
  body=$(jq -c '.request_body? // null' <<<"$(daemon_call_for "$1")" 2>/dev/null) || body=
  printf '%s\n' "${body:-null}"
}
# call_organization CALL prints the Anthropic organization that served CALL, a record of the proxy
# (daemon_call_for): the `anthropic-organization-id` header of the answer it forwarded. A prompt
# cache is kept per organization, so a call a gateway sent to another organization than the call
# before it reads nothing that call wrote (A10). A stand-in's answer carries no such header, and
# neither does no call at all: each prints an empty line.
call_organization() {
  local organization
  organization=$(jq -r '(.response_headers? // {})["anthropic-organization-id"]? // ""' <<<"${1:-null}" 2>/dev/null) || organization=
  printf '%s\n' "$organization"
}
# same_organization_pair ORGANIZATION...: given the answers in the order they were asked, each as
# the organization that served it (call_organization, "" where its call names none), prints
# "<earlier> <later>", the indexes of the first answer whose organization an earlier answer's also
# was and of the latest such earlier answer, and fails when no two answers share one. The later
# answer reads the cache the earlier one wrote whatever was served between them, since a prompt cache
# is kept per organization and not per call: in proof run 44095624771 six answers strictly
# alternated two organizations and each read what its own organization's answer before it wrote
# (A10-2r1 read the 3184 tokens A10-1 wrote). Answers naming no organization at all share one, a
# route that sends no such header. The caller keeps every answer inside the cache's lifetime.
same_organization_pair() {
  local organizations=("$@") earlier later
  for ((later = 1; later < ${#organizations[@]}; later++)); do
    for ((earlier = later - 1; earlier >= 0; earlier--)); do
      if [ "${organizations[earlier]}" = "${organizations[later]}" ]; then
        printf '%s %s\n' "$earlier" "$later"
        return 0
      fi
    done
  done
  return 1
}
# request_effort REQUEST: the effort a model request carries, wherever in the body it stands;
# empty for a request that carries none and for no request at all.
request_effort() {
  local effort
  effort=$(jq -r '[.. | objects | .effort? // empty] | first // ""' <<<"${1:-null}" 2>/dev/null) || effort=
  printf '%s\n' "$effort"
}
# openai_effort LEVEL: the reasoning effort the loop asks of an OpenAI model for a conversation at
# thinking LEVEL (agentloop's openaiEffort): `none` for `off`, which both of OpenAI's protocols
# name so, and every other level as it is.
openai_effort() {
  if [ "${1:-off}" = off ]; then
    printf 'none\n'
  else
    printf '%s\n' "$1"
  fi
}
# search_calls LABEL prints, as one JSON array in ordinal order, the recording proxy's records of
# LABEL's web searches: each a Messages call offering Anthropic's web search server tool
# (`web_search_20250305`), the one call the daemon makes for each web_search call it runs, to
# answers.providers.anthropic (internal/websearch's search). Each is {ordinal, status, query,
# searches, urls, usage, model}: the query, read back out of the one message the search sends
# (`Search the web for: <query>`, websearch's prompt); the searches its reply ran, its result
# blocks that hold a list of results; the address of every result and every citation of the
# reply, each once; the reply's usage; and the model the call names. A search's reply is no
# stream, so the proxy records it as the object it is. `[]` for none.
search_calls() {
  local calls
  calls=$(jq -s -c --arg l "$1" '[.[] | select(.label == $l and (.request_body | type) == "object"
      and any((.request_body.tools // [])[]?; (.type? // "") == "web_search_20250305"))
    | (if (.response_body | type) == "object" then .response_body else {} end) as $reply
    | {ordinal, status,
       query: ((.request_body.messages // []) | if type == "array" then (.[0].content? // "") else "" end
         | if type == "string" then (ltrimstr("Search the web for: ") | split("\n\nAnswer briefly from what the search finds")[0]) else "" end),
       searches: ([($reply.content // [])[]? | select(.type? == "web_search_tool_result" and (.content | type) == "array")] | length),
       urls: ([($reply.content // [])[]? | if .type? == "web_search_tool_result" then (.content | if type == "array" then .[] else empty end | .url?)
         else ((.citations // [])[]? | .url?) end | strings] | unique),
       usage: ($reply.usage // {}), model: .request_body.model}]
    | sort_by(.ordinal)' "$evidence/model-calls.jsonl" 2>/dev/null) || calls=
  printf '%s\n' "${calls:-[]}"
}
# search_call_for CALLS QUERY prints the first of CALLS (search_calls) whose query is QUERY, the
# call of one `answers: web_search` line, and `null` for none.
search_call_for() {
  local call
  call=$(jq -c --arg q "$2" 'first(.[]? | select(.query == $q)) // null' <<<"${1:-[]}" 2>/dev/null) || call=
  printf '%s\n' "${call:-null}"
}
# reply_urls TEXT prints every web address TEXT names, one a line, each once, with the punctuation
# that closes a sentence after it left off, and what closes a markdown link or a quote never taken
# in. Nothing for a text that names none.
reply_urls() {
  jq -R -s -r '[scan("https?://[^\\s<>\"()\\[\\]`]+") | sub("[.,;:!?\u0027*]+$"; "")] | unique[]' <<<"${1:-}" 2>/dev/null || true
}
# url_among URL URLS: URL is among URLS, a JSON array of addresses, a trailing slash aside.
url_among() {
  jq -e --arg u "$1" 'type == "array" and any(.[]; rtrimstr("/") == ($u | rtrimstr("/")))' <<<"${2:-null}" >/dev/null 2>&1
}
# tool_names_url URL LINES: some line of LINES, a JSON array of the `answers: gh` or `answers:
# dispatch` lines of an answer, holds URL in an argument of its call (`args`), a trailing slash
# aside: the agent reached that address with the tool.
tool_names_url() {
  jq -e --arg u "$1" '($u | rtrimstr("/")) as $b | type == "array"
    and any(.[]; any((.args // [])[]?; type == "string" and contains($b)))' <<<"${2:-[]}" >/dev/null 2>&1
}
# citation_faults REPLY RESULTS GH DISPATCH prints, one a line, what is wrong with the addresses
# REPLY, an answer's reply, cites (reply_urls), and nothing when nothing is. A23 judges that the
# addresses the answer cites came out of its searches, not that its fact is right: every address
# REPLY names must be among RESULTS, the JSON array of the result addresses of the answer's searches
# that ran, all of them together (a trailing slash aside), and none may be named by a call of the
# answer's gh or dispatch tool, GH's and DISPATCH's lines (tool_names_url), since an address the
# agent read with a tool came from no search. A reply that cites no address is at fault as well. Each
# fault names the address and which of the two it broke.
citation_faults() {
  local url urls
  urls=$(reply_urls "${1:-}")
  if [ -z "$urls" ]; then
    printf 'the reply cites no address\n'
    return 0
  fi
  while IFS= read -r url; do
    [ -n "$url" ] || continue
    url_among "$url" "${2:-[]}" || printf '%s is not among the results of the answer'"'"'s searches\n' "$url"
    ! tool_names_url "$url" "${3:-[]}" || printf '%s is named by an answers: gh call of the answer\n' "$url"
    ! tool_names_url "$url" "${4:-[]}" || printf '%s is named by an answers: dispatch call of the answer\n' "$url"
  done <<<"$urls"
}
# search_line_recomputes LINE CALL FEE: the `answers: web_search` LINE made the searches its call
# (CALL, a record of search_calls) ran, and its costUSD is that call's tokens at the price
# cost_price gives the call's model plus FEE for each of those searches: the search's fee is in
# the line, at the configured price, and nothing else is. No call, or a model with no price, is no
# such line.
search_line_recomputes() {
  jq -e -n --argjson line "${1:-null}" --argjson call "${2:-null}" --argjson fee "${3:-0}" "$cost_price"'
    ($call | if type == "object" then cost_price else null end) as $p
    | $p != null and ($line | type) == "object"
      and ($line.searches // 0) == ($call.searches // 0)
      and (((((($call.usage.input_tokens // 0) * $p[0]) + (($call.usage.output_tokens // 0) * $p[1])
          + (($call.usage.cache_creation_input_tokens // 0) * $p[2]) + (($call.usage.cache_read_input_tokens // 0) * $p[3])) / 1000000
          + ($call.searches // 0) * $fee - ($line.costUSD // 0)) | fabs) < 0.000001)' >/dev/null 2>&1
}
# request_holds_search REQUEST QUERY URL: a model request (first_request_after) holds a web search's
# call and its result: QUERY as the input of a call of the web_search tool, as Anthropic's tool_use
# (an `input` object) and OpenAI's function_call (`arguments`, a string of JSON) carry a call; and
# URL, the address the result named, in some text of it (A23: the resumed agent's transcript holds
# each search's call and result). A request that holds no call of web_search, whatever else it
# holds, holds no search, and so do an empty QUERY or URL and no request.
request_holds_search() {
  jq -e --arg q "$2" --arg u "$3" '$q != "" and $u != ""
    and any(..; type == "object" and .name? == "web_search"
      and ((((.input? | objects | .query?) // (.arguments? | strings | fromjson? | objects | .query?)) // null) == $q))
    and any(.. | strings; contains($u))' <<<"${1:-null}" >/dev/null 2>&1
}

# ---- the model route's answer ---------------------------------------------------------------------
# preflight_refusal CODE BODY [KEY] prints how the model route refused answer-loop.sh's GPT-6.1
# preflight (gpt_preflight), from the HTTP status CODE its POST /v1/responses answered (or curl's
# `unreachable (curl exited N)` where nothing answered) and the file BODY that holds the answer,
# and prints nothing where the route served the model: 200 with a response whose status is
# `completed`. A refusal says `answered <code>`, the response's own status where it has one, and
# the message the body's error gives — `error.message` in OpenAI's and LiteLLM's shape, a bare
# `error` string, or `message` or `detail` — on one line, cut to 300 characters. What a LiteLLM
# refusal says of the key it was sent, its masked tail and its hash (`Received API Key = sk-...abcd,
# Key Hash (Token) =<hex>`), is left out of it, and so is KEY itself wherever the message echoes
# it, before the cut, so that no head of a long key is left where the cut fell inside it.
preflight_refusal() {
  local code=$1 body=$2 key=${3:-} status message
  status=$(jq -r '.status? | strings' "$body" 2>/dev/null) || status=
  [ "$code" != 200 ] || [ "$status" != completed ] || return 0
  message=$(jq -r '((.error?.message? | strings) // (.error? | strings) // (.message? | strings) // (.detail? | strings) // empty)
    | gsub("(?i)api key = [^, ]*"; "API key = <left out>") | gsub("(?i)key hash \\(token\\) = ?[0-9a-f]*"; "key hash <left out>")' \
    "$body" 2>/dev/null) || message=
  message=${message//[$'\r\n\t']/ }
  [ -z "$key" ] || message=${message//"$key"/<the key>}
  case "$code" in
    [0-9][0-9][0-9]) printf 'answered %s' "$code" ;;
    *) printf '%s' "$code" ;;
  esac
  printf '%s%s\n' "${status:+, response status $status}" "${message:+: ${message:0:300}}"
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
# loop_answer_holds ID REPLY: REPLY is the loop's answer to question ID. It carries the loop's mark
# (answer_question_id), which every answer the loop posts carries and an agent's own reply does
# not, and holds every key of ID (answer_holds). It prints what is missing, the mark or the first
# key REPLY lacks, and nothing when it holds. A control resumes the agent it asked
# (control_agents), which may then answer the control's question itself, keys and all: that is no
# answer of the loop's (A10's legs).
loop_answer_holds() {
  [ -n "$(answer_question_id "$2")" ] || {
    printf '%s\n' "the loop's mark legion-answer:<id>"
    return 1
  }
  answer_holds "$1" "$2"
}
# recorded_hit FILE TEXT: the questions file records FILE's line TEXT as a hit of A16's search that
# is not about the answer loop (a16.unrelated_hits, by its whole text or the start of it).
recorded_hit() {
  jq -e --arg f "$1" --arg t "$2" \
    'any(.a16.unrelated_hits[]; .file == $f and (.text == $t or (.starts_with as $s | $s != null and ($t | startswith($s)))))' \
    "$questions_file" >/dev/null
}
# a16_pattern prints A16's own search pattern: the questions file's a16.phrases joined with `|`,
# an extended regular expression. Each phrase says the meaning, not a bare word: the absence of
# tools said of the loop or an agent within one clause (`no tools`, `without tools` or `tool-less`
# and `loop` or `agent`, either order, with no `.` and at most 60 characters between), or `never
# give(s)`/`hand(s)` with `tool(s)` in one clause. A bare `never give` hit rules about labels and
# peers' fields, and `no tools` an eval's arms; a word boundary keeps `never given up`, the outbox
# retry's sentence, out. a16_hit TEXT: TEXT holds one of the phrases, case-insensitively, as a
# line of the search does; no text at all is no hit, and no run of its own ends here.
a16_pattern() { jq -r '.a16.phrases | join("|")' "$questions_file"; }
a16_hit() {
  local pattern
  pattern=$(a16_pattern) || return 1
  grep -qiE -- "$pattern" <<<"${1:-}"
}

# ---- Dispatch -------------------------------------------------------------------------------------
# ask_of SESSION ISSUE prints the id of the newest ask SESSION opened on ISSUE, empty for none;
# ask_reply_after ASK COMMENT SESSION [RECORD] prints SESSION's first reply on ASK's thread written
# after the reply COMMENT, a person's (A1's, A6's and their controls'), and fails when none is or
# when the thread holds no COMMENT: SESSION's answer to that reply. Not its answer to an earlier
# reply, which a thread the loop answered before still holds, and not a later post of SESSION's,
# which an agent resumed since writes on its own ask (A7's control reads A1's thread after the
# architect's resumes). RECORD is ASK's record (GET asks/<ask>, its replies oldest first) as a
# control read it once, read now where not given. The asks route answers an array, so a body that
# is not one (an error object) names no ask.
ask_of() {
  dispatch_get "issues/$2/asks?thread=1" |
    jq -r --arg s "$1" 'if type == "array" then ([.[] | select(.author.id == $s)] | last | .id? // empty) else empty end' || true
}
ask_reply_after() {
  local record=${4:-}
  [ -n "$record" ] || record=$(dispatch_get "asks/$1" 2>/dev/null) || record=null
  jq -e -r --arg c "$2" --arg s "$3" '[.replies[]?] as $r
    | ([range($r | length) | select($r[.].id == $c)] | first) as $at
    | if $at == null then empty else ([$r[$at + 1:][] | select(.author.id == $s)] | first | .body? // empty) end' <<<"$record"
}
# comment_reply_in COMMENTS COMMENT SESSION prints SESSION's first reply to COMMENT in COMMENTS, an
# issue's list of comments (GET issues/<issue>/comments), or fails: the answer a role mention of a
# stopped agent is answered with (A3, A21). Dispatch climbs a reply's `reply_to` to its thread's
# head, so a later reply of SESSION's in COMMENT's thread names COMMENT too: A20's answer, written
# under the person's reply, or a post of the agent once resumed.
comment_reply_in() {
  jq -e -r --arg id "$2" --arg s "$3" '[.[]? | select(.reply_to == $id and .author.id == $s)] | (first | .body? // empty)' <<<"${1:-null}"
}
# comment_reply_by ISSUE COMMENT SESSION [COMMENTS] is comment_reply_in over COMMENTS, ISSUE's list
# as a control read it once, or over the list read from Dispatch now where it is not given.
comment_reply_by() {
  local comments=${4:-}
  [ -n "$comments" ] || comments=$(dispatch_get "issues/$1/comments") || comments=null
  comment_reply_in "$comments" "$2" "$3"
}
# record_reply_by RECORD SESSION prints SESSION's first reply in the message record a conversation
# page reads (GET agents/<session>/messages), empty for none.
record_reply_by() {
  jq -r --arg s "$2" 'first(.replies[]? | select(.author.id == $s) | .body) // empty' <<<"${1:-null}" 2>/dev/null || true
}
# issue_comments_in COMMENTS NEEDLE prints the comments of COMMENTS, an issue's list of comments,
# whose body holds NEEDLE, each as {id, author, reply_to, body}, the author its id: `[]` for none,
# for Dispatch's error body and for a read that failed (A22: the comment the stopped agent's
# dispatch tool posted where it was asked).
issue_comments_in() {
  jq -c --arg n "$2" '(if type == "array" then . else [] end)
    | [.[] | select((.body // "") | contains($n)) | {id, author: .author.id?, reply_to, body}]' <<<"${1:-null}" 2>/dev/null ||
    printf '[]\n'
}
# one_tool_marked_comment_by COMMENTS SESSION: the comments (issue_comments_in) are one,
# written as SESSION, its body carrying the mark the dispatch tool adds to what it posts, the
# loop's marker naming the call, `<!-- legion-answer:<question>.<seq> -->` (dispatchtool's Mark),
# which the loop answers nothing carrying. The loop's own reply carries `<question>` with no seq,
# and a comment carrying that alone is no comment the tool posted.
one_tool_marked_comment_by() {
  jq -e --arg s "$2" 'type == "array" and length == 1 and .[0].author == $s
    and ((.[0].body // "") | test("legion-answer:[0-9a-f]+\\.[0-9]+ -->"))' <<<"${1:-null}" >/dev/null 2>&1
}
# issue_standing ISSUE prints ISSUE's status and claim as Dispatch holds them now, {status, claim},
# and `null` for a read that failed or answered no issue (A22: the issue whose status write and
# claim the dispatch tool refused).
issue_standing() {
  local body
  body=$(dispatch_get "issues/$1" 2>/dev/null) || body=
  jq -c 'if type == "object" and has("status") then {status, claim} else null end' <<<"${body:-null}" 2>/dev/null ||
    printf 'null\n'
}
# standing_unchanged BEFORE AFTER: both are an issue's standing (issue_standing), and the same: its
# status and its claim did not move. A read that failed on either side shows nothing.
standing_unchanged() {
  jq -e -n --argjson a "${1:-null}" --argjson b "${2:-null}" '$a != null and $a == $b' >/dev/null 2>&1
}
# collaborator_role_token COLLABORATORS SESSION prints the role token the issue's Collaborators
# view names for the current tester SESSION (the one no later session replaced), empty for none.
collaborator_role_token() {
  jq -r --arg s "$2" \
    '[.agents[]? | select(.session_id == $s and .legion.role == "tester" and .legion.replaced_by == null) | .legion.role_token] | first // empty' \
    <<<"${1:-null}" 2>/dev/null || true
}
# comment_record COMMENT prints Dispatch's record of COMMENT as it holds it now (GET comments/<id>:
# the comment with its delivery records, and every reply in its thread, oldest first), and nothing
# for a read that failed. Every read of a comment below reads it here. The comment's own POST answer
# is no such record: Dispatch settles a mention after it answers, so that answer carries the
# delivery pending, and it can carry it pending while its sender waits out a listener take-over.
comment_record() {
  local body
  body=$(dispatch_get "comments/$1" 2>/dev/null) || return 0
  printf '%s\n' "$body"
}
# comment_delivery_records COMMENT prints COMMENT's delivery records (comment_record), each as
# {target, state, session_id, error}, and `[]` for a comment Dispatch answers no record of and for
# a read that failed: what A3's note and A21 print and judge. delivery_records_in RECORD prints the
# same of RECORD, a comment's record read before (A21's control reads it once).
comment_delivery_records() { delivery_records_in "$(comment_record "$1")"; }
delivery_records_in() {
  jq -c '[.comment.deliveries[]? | {target, state, session_id, error}]' <<<"${1:-null}" 2>/dev/null || printf '[]\n'
}
# one_delivery_for DELIVERIES STATE SESSION: the delivery records (comment_delivery_records) are
# one, in STATE, and name SESSION, or no session at all for an empty SESSION. A21 reads its
# mention's delivery `sent` to the failed implementer's session, and its control's `queued` naming
# none: Dispatch keeps a mention of a role the registry holds no entry for, for the role's first
# holder, under a project whose daemon answers.
one_delivery_for() {
  jq -e --arg state "$2" --arg s "${3:-}" \
    'type == "array" and length == 1 and .[0].state == $state and (.[0].session_id // "") == $s' \
    <<<"${1:-null}" >/dev/null 2>&1
}
# comment_delivered_to COMMENT SESSION: Dispatch's record of COMMENT as it holds it now holds one
# delivery, sent to SESSION (A3 waits for this read, as it waits for its control's refusal, and A21
# for its mention's).
comment_delivered_to() { one_delivery_for "$(comment_delivery_records "$1")" sent "$2"; }
# comment_deliveries COMMENT prints COMMENT's delivery records (comment_record), each as
# {state, error}, and `[]` for a comment Dispatch answers no record of.
comment_deliveries() {
  local body
  body=$(comment_record "$1")
  jq -c '[.comment.deliveries[]? | {state, error}]' <<<"${body:-null}" 2>/dev/null || printf '[]\n'
}
# one_delivery_in DELIVERIES STATE [ERROR]: the delivery records are one, in STATE, and, with ERROR
# given, its error begins with ERROR. A3's control reads a mention of a session nobody registered
# as `failed` with `no live session <id>`, the refusal no listener keeps; A13 reads Dispatch's
# delivery to a stopped agent's role as `failed` and to its session as `queued`, the message stored
# for a session that may run again.
one_delivery_in() {
  jq -e --arg state "$2" --arg error "${3:-}" \
    'type == "array" and length == 1 and .[0].state == $state and ((.[0].error // "") | startswith($error))' \
    <<<"${1:-null}" >/dev/null 2>&1
}
# thread_replies_by HEAD SESSION prints SESSION's replies in the thread of comment HEAD as Dispatch
# holds it now (comment_record, which lists every reply under the head, oldest first), each as
# {id, answers, body}: `answers` is the comment a reply was written under, which Dispatch keeps
# beside the thread's head (`reply_to`, the head of every reply in a thread). It prints `[]` for
# none, for Dispatch's error body, for a read that failed and for no comment id at all (A20).
# thread_replies_in RECORD SESSION reads the same of RECORD, HEAD's record read before (A20's
# control reads it once).
thread_replies_by() { thread_replies_in "$(comment_record "$1")" "$2"; }
thread_replies_in() {
  jq -c --arg s "$2" '[.replies[]? | select(.author.id == $s) | {id, answers, body}]' <<<"${1:-null}" 2>/dev/null ||
    printf '[]\n'
}
# first_reply_id REPLIES prints the id of the oldest of those replies, empty for none: in A3's
# thread, the loop's answer to the mention, which A20's person replies under.
first_reply_id() { jq -r 'if type == "array" then (.[0].id? // empty) else empty end' <<<"${1:-null}" 2>/dev/null || true; }
# reply_count REPLIES: how many replies the list holds, 0 for a list that is none.
reply_count() { jq 'if type == "array" then length else 0 end' <<<"${1:-null}" 2>/dev/null || printf '0\n'; }
# reply_answering REPLIES COMMENT prints the body of the first of those replies written under
# COMMENT (its `answers`), and fails while none is: the answer to a person's reply in a thread, not
# a later reply the agent, once resumed, writes under it.
reply_answering() {
  jq -e -r --arg c "$2" 'if type == "array" then ([.[] | select(.answers == $c)] | first | .body? // empty) else empty end' \
    <<<"${1:-null}" 2>/dev/null
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
# registry_entry_of ANSWER SESSION ROLE_TOKEN: the listener's answer for SESSION's Legion agent
# registry entry (GET /v1/legion/agents/sessions/<session>, the entry inside `agent` or bare, as
# check_boot reads it) is SESSION's under ROLE_TOKEN. A21 asks its role mention only once the
# registry holds that entry: the role's newest, one the listener hands to its daemon.
registry_entry_of() {
  jq -e --arg s "$2" --arg r "$3" '(.agent // .) | type == "object" and .session_id == $s and .role_token == $r' \
    <<<"${1:-null}" >/dev/null 2>&1
}

# ---- GitHub ----------------------------------------------------------------------------------------
# pr_comment_listing NUMBER prints the smoke repository's comments on the pull request as GitHub
# answered them, a JSON array, and `null` where no listing was read at all: a gh call that failed
# or printed nothing, and GitHub's error body, which is an object and no list of comments. The
# call's body is read into a variable rather than piped, so a failing gh leaves one answer and not
# two (the pipe would print the empty answer and the fallback another, under `pipefail`). A half
# that must show a write was never made reads this and not pr_comments: an empty list of matches
# is what a listing nobody could read prints too.
pr_comment_listing() {
  local body
  body=$(smoke_gh api "repos/$repo/issues/$1/comments?per_page=100" 2>/dev/null) || body=
  jq -s -c '(.[0] // null) | if type == "array" then . else null end' <<<"$body" 2>/dev/null ||
    printf 'null\n'
}
# pr_comments_holding LISTING MARKER prints the comments of LISTING (pr_comment_listing) whose body
# carries MARKER, as {id, user, body}: the one filter of the pull request's comments by a marker. A
# comment with a null body holds nothing and does not end the read, and a listing that is none —
# GitHub's error body, a gh call that printed nothing or failed — holds no comment and prints [].
pr_comments_holding() {
  jq -c --arg m "$2" '(if type == "array" then . else [] end) | [.[] | select((.body // "") | contains($m)) | {id, user: .user.login, body}]' \
    <<<"${1:-null}" 2>/dev/null || printf '[]\n'
}
# comment_ids_holding LISTING MARKER prints, as one JSON array, the ids of the comments
# pr_comments_holding finds in LISTING, and `[]` for a listing that is none: A17's comments by the
# run's refusal marker, which its reads say and record_leak_comments records for the cleanup.
comment_ids_holding() {
  jq -c 'map(.id)' <<<"$(pr_comments_holding "${1:-null}" "$2")"
}
# pr_comments NUMBER MARKER is pr_comments_holding over the listing read from GitHub now.
pr_comments() {
  local listing
  listing=$(pr_comment_listing "$1") || listing=null
  pr_comments_holding "$listing" "$2"
}
# one_comment_by COMMENTS LOGIN SESSION: the comments holding the marker are one, posted by LOGIN,
# and its Legion footer names SESSION.
one_comment_by() {
  jq -e --arg login "$2" --arg s "$3" \
    'type == "array" and length == 1 and .[0].user == $login and (.[0].body | contains("\"session\":\"" + $s + "\""))' \
    <<<"${1:-null}" >/dev/null 2>&1
}
# refused_marker_absent LISTING REFUSED: GitHub answered a listing of the pull request's comments
# (pr_comment_listing), and none of its comments holds REFUSED, the run's refusal marker: the part
# of A17's leak-guard leg that says the write the leak guard refused was never made, read alone.
# A17's control runs it over the comment it places carrying REFUSED, and it must then fail. Alone it
# holds over a listing of no comment at all, since jq's `all` over an empty array is true, so the
# leg reads it beside the comment its first leg posted (refused_write_absent).
#
# A listing that is no listing (`null`: the read failed, or GitHub answered an error body) fails, as
# does an empty REFUSED: a read nobody could make, or of no marker, has shown nothing.
refused_marker_absent() {
  jq -e --arg refused "$2" \
    'type == "array" and $refused != "" and all(.[]; ((.body // "") | contains($refused)) | not)' \
    <<<"${1:-null}" >/dev/null 2>&1
}
# refused_write_absent LISTING KEPT REFUSED: GitHub answered a listing of the pull request's
# comments (pr_comment_listing), that listing holds the comment whose body carries KEPT, and none
# of its comments holds REFUSED (refused_marker_absent) — the write the leak guard refused was never
# made, read on the listing that proves it was read at all.
#
# The KEPT half is what makes the REFUSED half mean anything. jq's `all` over an empty array is
# true, so a listing of nothing passes "no comment holds the marker" having examined no comment:
# a listing of the wrong pull request, one GitHub answered empty, and a comment page that moved
# would each have read the absence of a write nobody looked for. KEPT is the comment the leg
# posted minutes before, on that pull request, which is known to be there, so a listing without
# it is the wrong listing rather than an empty pull request.
#
# A listing that is no listing (`null`: the read failed, or GitHub answered an error body) fails
# here too: a read nobody could make has shown nothing.
refused_write_absent() {
  refused_marker_absent "${1:-}" "$3" &&
    jq -e --arg kept "$2" 'type == "array" and any(.[]; (.body // "") | contains($kept))' <<<"${1:-null}" >/dev/null 2>&1
}
# refusal_mentioned REPLY: the agent's reply says GitHub refused its write.
refusal_mentioned() { grep -qiE 'refus|denied|403|404|not accessible|permission' <<<"${1:-}"; }
# leak_reason_quoted REPLY REASON: the agent's reply to the asker carries REASON, the shared
# check's own report of why it refused the call, which the refusal handed the tool word for word.
# It is one half of the leak-guard leg and never the whole: a reply saying a call was refused is
# what an agent writes for any refusal, so the write that was not made is read beside it
# (refused_write_absent). No reply at all carries nothing.
leak_reason_quoted() { grep -qiF -- "$2" <<<"${1:-}"; }
# pr_review_listing NUMBER prints the smoke repository's reviews of the pull request as GitHub
# answered them, a JSON array, and `null` where no listing was read at all: a gh call that failed or
# printed nothing, and GitHub's error body, which is an object and no list of reviews
# (pr_comment_listing's rule). A19's control reads it once and hands it to its read.
pr_review_listing() {
  local body
  body=$(smoke_gh api "repos/$repo/pulls/$1/reviews" 2>/dev/null) || body=
  jq -s -c '(.[0] // null) | if type == "array" then . else null end' <<<"$body" 2>/dev/null ||
    printf 'null\n'
}
# reviewer_approvals NUMBER SESSION [SINCE [REVIEWS]] prints the approving reviews of the pull
# request whose Legion footer names SESSION, as {id, user, commit_id, submitted_at, footer}, and
# with SINCE (an RFC 3339 UTC time, as `date -u +%FT%TZ` writes it) only those GitHub says were
# submitted then or later: the approval that followed one ask and no earlier one (A19, and its
# control after A19's own approval). REVIEWS is the listing (pr_review_listing) as a control read it
# once, read now where not given. A listing that is none is no review: it prints [], and one list
# either way (pr_comments).
reviewer_approvals() {
  local reviews=${4:-}
  [ -n "$reviews" ] || reviews=$(pr_review_listing "$1")
  jq -c --arg s "$2" --arg since "${3:-}" 'if type == "array" then
      [.[] | select(.state == "APPROVED" and (.body | contains("\"session\":\"" + $s + "\"")))
        | select($since == "" or ((.submitted_at // "") >= $since))
        | {id, user: .user.login, commit_id, submitted_at, footer: (.body | [capture("legion: (?<f>\\{[^}]*\\})").f] | first // null)}]
      else [] end' <<<"$reviews" 2>/dev/null ||
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
# tree_is_live ROOT PHASE [PHASE|held]...: the daemon's own state says ROOT's tree is live and does
# not linger — admission.trees is record.LiveRoots, the very set admission's release pass judges a
# root against (admit's releaseUnslotted) — and ROOT still stands at PHASE, which that release would
# have left at `done`. Each further argument widens what stands: another phase ROOT may stand at
# (A6's seeded parent, which a daemon with a parent waiting for its open children takes from
# `implementing` to `awaiting_children` at boot and keeps live), or `held`, which lets a root the
# workflow holds out of PHASE stand there too: `phase` held and `heldFrom` PHASE (api.Issue.HeldFrom).
# The daemon holds an issue whose worker failed with work outstanding, and a held issue is still in
# the workflow and in its tree. A root outside admission.trees, whatever its phase, is not live.
tree_is_live() {
  local root=$1 phase=$2
  shift 2
  legion_cmd state --json |
    jq -e --arg root "$root" --arg phase "$phase" '(.admission.trees | index($root)) != null
      and (.issues[$root] | .phase == $phase
        or (.phase | IN($ARGS.positional[] | select(. != "held")))
        or (.phase == "held" and .heldFrom == $phase and ($ARGS.positional | index("held")) != null))' --args "$@" >/dev/null
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
# tree_claims_in CLAIMS TREE STATE...: the listing `legion claims list --json` printed holds at least
# one claim of TREE, and every claim of TREE in it stands in one of STATE..., so nothing of the tree
# started. The claim it must hold is what makes the rest mean anything: jq's `all` over no claims
# is true, so a listing of nothing, or of other trees alone, would pass having examined no claim of
# TREE. A listing that is no listing says nothing of the kind. A21 reads its tree's claims
# suspended, failed or retired: its implementer's claim is failed and its architect's suspended.
tree_claims_in() {
  local claims=$1 tree=$2
  shift 2
  jq -e --arg t "$tree" 'type == "object"
    and any(.claims[]?; .tree == $t)
    and all(.claims[]? | select(.tree == $t); .state | IN($ARGS.positional[]))' --args "$@" <<<"${claims:-null}" >/dev/null 2>&1
}
# tree_claims_parked CLAIMS TREE: every claim of TREE stands suspended or retired (tree_claims_in),
# so nothing of the tree started (A1).
tree_claims_parked() { tree_claims_in "$1" "$2" suspended retired; }
# listed_claim_is CLAIMS TOKEN STATE: the listing `legion claims list --json` printed holds the
# claim TOKEN, standing in STATE (A21: its implementer's claim, failed).
listed_claim_is() {
  jq -e --arg t "$2" --arg st "$3" 'any(.claims[]?; .token == $t and .state == $st)' <<<"${1:-null}" >/dev/null 2>&1
}
# issue_claim_roles CLAIMS ISSUE prints, as one JSON array, the role and state of each claim of
# ISSUE in that listing, `[]` for none and for a listing that is none (A21's note and its FAIL).
issue_claim_roles() {
  local roles
  roles=$(jq -c --arg i "$2" '[.claims[]? | select(.issue == $i) | {role, state}]' <<<"${1:-null}" 2>/dev/null) || roles=
  printf '%s\n' "${roles:-[]}"
}

# ---- what a control sent or placed ------------------------------------------------------------------
# A control row (answer-loop.sh's run_controls) reads PASS when its row's read fails over a message
# of its own with answers removed. A read fails as surely over a listing nobody could read, or one
# that holds nothing, as over the control's own message or placement: a GitHub listing that is
# `null`, an issue's comments missing the comment the control placed, an inbox the run never wrote.
# So a control counts a read's failure only over a listing it read once, found holding what it sent
# or placed there, and handed to that read (answer-loop.sh's control_seen).
#
# listing_fault LISTING ENTRIES [MATCH [VALUE...]] prints what keeps LISTING from showing what a
# control sent or placed, and fails; it prints nothing when LISTING shows every VALUE. LISTING is
# the JSON a read was answered; ENTRIES the jq path of its entries, which yields one array (`.` for
# a list, `.replies` for an ask's or a comment's record); MATCH a jq test of one entry, `value` its
# VALUE (`.id == value`, `(.text // "") | contains(value)`). It prints `no listing at all` for a
# LISTING that is no JSON or whose ENTRIES are no array (`null` where the read failed, an error
# object), `an empty listing` for one of no entries, and for one that lacks a VALUE `a listing of N
# entries holding no VALUE[, no VALUE…]`, each cut to 80 characters. With no VALUE, a listing of any
# entry shows what the read needs: one where the control's own message cannot stand (the asker's
# inbox, which holds replies alone), which must have been read and must hold something.
listing_fault() {
  local listing=$1 entries=$2 match=${3:-true} fault
  shift "$(($# < 3 ? $# : 3))"
  fault=$(jq -R -s -r '(fromjson? // null) as $l
    | (if $l == null then null else (first(($l | '"$entries"')?) // null) end) as $e
    | if ($e | type) != "array" then "no listing at all"
      elif ($e | length) == 0 then "an empty listing"
      else [$ARGS.positional[] as $v | def value: $v; select(any($e[]; ('"$match"')? // false) | not) | $v]
        | if length == 0 then empty
          else "a listing of \($e | length) \(if ($e | length) == 1 then "entry" else "entries" end) holding no \(map(if length > 80 then .[0:79] + "…" else . end) | join(", no "))" end
      end' --args "$@" <<<"$listing" 2>/dev/null) || fault='no listing at all'
  [ -n "$fault" ] || return 0
  printf '%s\n' "$fault"
  return 1
}

# ---- A13: branch against base ------------------------------------------------------------------------
# merge_base_commit ROOT REV prints "<merge base> <rev> <head>": the commit A13's base build is of,
# the merge base of REV and ROOT's checkout, then REV and that checkout's head, each a full commit.
# The head is the working copy's commit under jj and HEAD under git (CI's checkout). A13 asks
# whether the branch answers as the commit it changed answers, so a base ahead of the branch must
# not be built: in proof run 44095624771 the base was main's tip, a2b379d1, which carries #4281's
# `requested_by.proven` and the branch does not, and the criterion read main's change as the
# branch's. REV an ancestor of the head is its own merge base. It fails when REV or the head does
# not resolve, or when the two share no history.
merge_base_commit() {
  local root=$1 rev=$2 commit head base
  if command -v jj >/dev/null && jj -R "$root" root >/dev/null 2>&1; then
    commit=$(jj -R "$root" log -r "$rev" --no-graph -T 'commit_id' 2>/dev/null) &&
      head=$(jj -R "$root" log -r @ --no-graph -T 'commit_id' 2>/dev/null) &&
      base=$(jj -R "$root" log -r "fork_point($commit | $head)" --no-graph -T 'commit_id' 2>/dev/null) || return 1
  else
    commit=$(git -C "$root" rev-parse --verify --quiet "$rev^{commit}") &&
      head=$(git -C "$root" rev-parse --verify --quiet HEAD) &&
      base=$(git -C "$root" merge-base "$commit" "$head") || return 1
  fi
  # One full commit each: a revision naming several commits names no base, and jj answers two
  # histories that share nothing with its root commit, which is no commit of either.
  local full='^[0-9a-f]{40}$'
  [[ $commit =~ $full ]] && [[ $head =~ $full ]] && [[ $base =~ $full ]] || return 1
  [ "$base" != 0000000000000000000000000000000000000000 ] || return 1
  printf '%s %s %s\n' "$base" "$commit" "$head"
}
# a13_same FILE [SIDE]: SIDE's and base's FILE for the current store say the same thing — byte for
# byte, but for what each stack minted for its own call, which no two calls can share. SIDE is
# `branch`, the branch with `answers` removed (A13), by default, or `control`, the branch with
# `answers` on (A13's control, whose comparison must fail). A listener's answer
# (send.body, publish.body) mints its envelope's ids — its own and its source's, its dedupe key and
# its trace id — and the time it was issued, and its answer to a role message it keeps for the
# role's next holder names the end of that copy's hour, `kept_until`, the time the stream stored the
# copy plus the hour. Dispatch's delivery records (dispatch-session.json, dispatch-role.json) mint
# the envelope each delivery sent, `envelope_id`. A minted field is compared by whether it is there
# at all: one side that carries it and one that carries none, or null, still differ. The status
# files are compared byte for byte. Both captures must exist and hold bytes before any comparison:
# `cmp` reads two empty files, or two missing ones, as the same, so a probe that captured nothing
# on either stack would pass.
a13_minted_answer='with_entries(if (.key | IN("event_id", "source_event_id", "dedupe_key", "trace_id", "issued_at", "kept_until")) and .value != null then .value = "minted" else . end)'
a13_minted_deliveries='map(with_entries(if .key == "envelope_id" and .value != null then .value = "minted" else . end))'
a13_same() {
  local branch=$evidence/a13/${2:-branch}/$store/$1 base=$evidence/a13/base/$store/$1 minted left right
  [ -s "$branch" ] && [ -s "$base" ] || return 1
  cmp -s "$branch" "$base" && return 0
  case "$1" in
    send.body | publish.body) minted=$a13_minted_answer ;;
    dispatch-session.json | dispatch-role.json) minted=$a13_minted_deliveries ;;
    *) return 1 ;;
  esac
  left=$(jq -S "$minted" "$branch" 2>/dev/null) && right=$(jq -S "$minted" "$base" 2>/dev/null) || return 1
  [ "$left" = "$right" ]
}
# a13_files are the six captures A13 compares, which a13_probe writes under
# $evidence/a13/<label>/<store>/.
a13_files=(send.status send.body publish.status publish.body dispatch-session.json dispatch-role.json)
# a13_differs [SIDE] prints, each followed by a space, the a13_files whose captures of SIDE (branch
# by default) a13_same finds unlike the base commit's.
a13_differs() {
  local file
  for file in "${a13_files[@]}"; do
    a13_same "$file" "${1:-branch}" || printf '%s ' "$file"
  done
}
# a13_captured SIDE: SIDE's probe (a13_probe) and the base commit's both captured every one of the
# six files A13 compares for the current store, each holding bytes. a13_same answers a missing
# capture as a difference, so A13's control, which passes on a difference, reads this first: a
# probe that captured nothing differs from nothing.
a13_captured() {
  local file side
  for side in "$1" base; do
    for file in "${a13_files[@]}"; do
      [ -s "$evidence/a13/$side/$store/$file" ] || return 1
    done
  done
}
