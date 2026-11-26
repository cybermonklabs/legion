#!/usr/bin/env bash
# The answer loop's live proof (INFRA-204; scripts/e2e/README.md, "answer-loop.sh"): the branch's
# daemon, Envoy listener and Dispatch, run on this host with real Anthropic models (and OpenAI's
# GPT-6.1 for A10's GPT-6.1 clause, where the model route serves it), under both of a tmux daemon's
# conversation stores (runtime.tmux.session_store files, then postgres), with stopped agents whose
# real conversations hold the decisions scripts/e2e/answer-loop/questions.json names.
# Every acceptance criterion of the plan's table (A1-A10, A12-A19), and the rows after it (A20: a
# person's reply in a thread under a stopped agent's answer; A21: a role mention of a failed agent
# on an issue still in the workflow; A22: the loop's dispatch tool; A23: its web_search tool), is
# one check that prints its evidence; the summary lists each as PASS, FAIL or WAITING with what it
# waits on. Each answering row that ran has a control row of its own, `A<n> control`, run in A13's
# phase with `answers` removed (run_controls): the row's own read over a message of the control's
# own, which must fail. It runs in a Legion issue pod (no docker, no root, no psql, no tmux) and on
# a devbox.
#
#   bash scripts/e2e/answer-loop.sh
#
# Inputs (environment):
#   LEGION_E2E_MODEL_ROUTE       how the run's model calls are keyed and where they go, as Stage 2's
#                                and Stage 3's proofs take it: `key-file`, the default, or
#                                `gateway`. Every call of the run, the agents', the daemon's answer
#                                loop's and Claude Code's, goes through the recording proxy to the
#                                route's endpoint. Whether that endpoint serves GPT-6.1 is asked of
#                                it once, at bring-up (gpt_preflight), and A10's GPT-6.1 clause
#                                runs only where it does.
#   LEGION_E2E_ANTHROPIC_KEY_FILE, LEGION_E2E_CLAUDE_KEY_FILE, ANTHROPIC_API_KEY
#                                under `key-file`, where the key comes from: a file holding it, or
#                                the key itself. Whichever is set is copied once into a 0600 file of
#                                the run's work directory and never echoed; the agents' key command,
#                                Claude Code's apiKeyHelper and the daemon's
#                                answers.providers.anthropic.api_key_file all read that copy.
#   LEGION_E2E_MODEL_BASE_URL, ANTHROPIC_BASE_URL
#                                the endpoint that key is for (lib/model-endpoint.sh), Anthropic's
#                                own when neither is set: the recording proxy's upstream.
#   LEGION_E2E_MODEL_GATEWAY_URL under `gateway`, the base URL under which the model gateway
#                                serves /v1/messages with its own credential, a LiteLLM
#                                gateway's root (lib/install-model-gateway.sh): the proxy's
#                                upstream. `hawk-token`
#                                keys every call — the agents' key command, the daemon's answers
#                                block through api_key_command and Claude Code through its
#                                apiKeyHelper — unless the next input names another command.
#   LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND
#                                under `gateway`, optional: shell text that prints the bearer the
#                                gateway takes, for a gateway keyed by a short-lived token instead
#                                of by `hawk-token` (a CI runner's federated identity, whose token
#                                expires long before a run ends). The run writes it into a 0700
#                                script of its work directory, carrying this shell's PATH, HOME,
#                                XDG base directories and session bus address, and the agents', the
#                                daemon's and Claude Code's commands all call that script, which
#                                runs the text again for every model call and keeps nothing. The
#                                token is echoed nowhere and written to no evidence, and the
#                                recording proxy sends it to the gateway as
#                                `Authorization: Bearer <token>` (--api-key-as-bearer), the header
#                                shape none of the three clients can be told to send itself. Set,
#                                `hawk-token` is not required.
#   LEGION_E2E_IMPLEMENT_APP_KEY_COMMAND, LEGION_E2E_REVIEW_APP_KEY_COMMAND
#                                shell text that prints each role App's PEM private key on stdout
#                                (lib/github-apps.sh). The run runs each once, here on the driver,
#                                in the operator's own environment, into $work/app-key/<role>/
#                                key.pem (0600, in a directory of mode 0700 that cleanup removes
#                                on every exit), and every token of the run — the daemon's, which
#                                reads the key through that App's private_key_command, and the
#                                rig's own — is minted from that key. The run asks the secrets
#                                broker for nothing and needs no agent-secrets on PATH. Both
#                                required for the workflow, with no default and no fallback;
#                                with either unset the run says which and runs claims-only, where
#                                the daemon has no dispatch_url and no github_apps, so the answer
#                                loop is off and every answering criterion WAITS on that input.
#   LEGION_E2E_IMPLEMENT_APP_ID, LEGION_E2E_REVIEW_APP_ID
#                                each App's numeric id (lib/github-apps.sh; the defaults are this
#                                repository's own deployment's). The implement App is proved
#                                installed on the smoke repository before anything else.
#   LEGION_E2E_REPO_SECOND_OWNER  optional: owner/name of a repository of another owner both Apps are
#                                installed on (A17's refusal); never written in this tree.
#   LEGION_E2E_BASE_REV           the base commit A13 compares against (default main@origin).
#   LEGION_E2E_OMP_BIN            the Oh My Pi binary (default /opt/omp/bin/omp, else the .omp-pin
#                                mise resolves).
#   LEGION_E2E_ANSWER_LOOP_EVIDENCE_DIR, LEGION_E2E_ANSWER_LOOP_CACHE
#                                where the evidence goes (default a new directory under $TMPDIR) and
#                                where the pinned downloads and the base commit's build are kept
#                                between runs (default $TMPDIR/legion-e2eal-cache).
#   LEGION_E2E_ANSWER_LOOP_ONLY, LEGION_E2E_ANSWER_LOOP_STORES
#                                narrow a run while a check is fixed: the criteria to run (e.g.
#                                "boot A6"; default all) and the stores (default "files postgres").
#                                The bring-up and the stopped agents are made as always.
#   PLAYWRIGHT_BROWSERS_PATH     where Playwright's Chromium lives, for A3's conversation-page leg,
#                                the one check of this proof that opens a page (default the run's
#                                cache; it is installed there once when it is missing). A run with
#                                no workflow, and one narrowed past A3, opens no page and installs
#                                nothing.
#
# Everything the run starts is its own and stopped by exact pid on exit: Postgres (the zonky
# embedded-postgres binaries with Debian's pgvector), nats-server, the recording proxy, the Envoy
# inbox, the listener, Dispatch, each daemon and its private tmux server, and the keepers holding
# the listener's, Dispatch's and the daemons' ports. The run's GitHub writes
# are on the smoke repository (A17, A18, A19's pull requests, comments and reviews, each recorded by
# id and closed or deleted on exit) and A17's refused write of a workflow file on the second
# owner's repository, which GitHub refuses the review App for want of its `workflows` permission.
set -Eeuo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp_root=${TMPDIR:-/tmp}
tmp_root=${tmp_root%/}
work=$(mktemp -d "$tmp_root/legion-e2eal.$$.XXXXXXXX")
evidence=${LEGION_E2E_ANSWER_LOOP_EVIDENCE_DIR:-$(mktemp -d "$tmp_root/legion-e2eal-evidence.XXXXXXXX")}
cache=${LEGION_E2E_ANSWER_LOOP_CACHE:-$tmp_root/legion-e2eal-cache}
mkdir -p "$evidence/logs" "$evidence/a13" "$evidence/sessions" "$cache"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
# Every program the run execs from here on carries its mark, and cleanup's sweep ends what carries it
# and nothing else (marked_processes, lib/process-tree.sh). The services and the daemons start
# under `env -i`, so service_env and start_daemon name the mark again. A fork of this shell that
# execs nothing, the asker's heartbeat, carries none and is stopped by its pid. The transcript's
# tee, started above, carries none, so it outlives the sweep and writes the run's last line.
export LEGION_E2E_RUN=$work

# ---- pins ------------------------------------------------------------------------------------------
# Postgres 17 from the zonky embedded-postgres jar on Maven Central, with Debian trixie's pgvector
# for 17 copied into it (Dispatch's migrations create the vector extension).
pg_version=17.6.0
pg_jar_url=https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-linux-amd64/$pg_version/embedded-postgres-binaries-linux-amd64-$pg_version.jar
pg_jar_sha256=23da5a044b4fb7a5a081a45008c95749c873305328d73f86aefd56922ce1d29d
debian_pool=https://deb.debian.org/debian/pool/main
pgvector_deb=p/pgvector/postgresql-17-pgvector_0.8.0-1_amd64.deb
pgvector_sha256=6e8a457eb252617b66cf7b2908c736127eae43703fffec94492ac131ca921515
# tmux and the libraries it links, unpacked with dpkg-deb when the host has no tmux.
tmux_debs=(
  "t/tmux/tmux_3.5a-3_amd64.deb e6b6aa51780044e2c4a0e618c33f832b70ec80caa30303e6c254a0c851b5af78"
  "libe/libevent/libevent-core-2.1-7t64_2.1.13-stable-1~deb13u1_amd64.deb faa7d20cc8fa05ac3b0497065fa60bc920eacd3711ff039649d43a7aa34421ce"
  "libu/libutempter/libutempter0_1.2.1-5_amd64.deb 92872367e010acf25765c18006f40317606628739ace4187c37e8230419b8d20"
  "j/jemalloc/libjemalloc2_5.3.1-2_amd64.deb 115b797f5a25d70cbe81373c9f1cce78536ed2a8a6d9dd5a061682a2340a25df"
)
# nats-server at packages/envoy/go.mod's own pin, installed with `go install` so its pid is its own.
# The Envoy listener and the daemon refuse a nats-server below v2.11.12, v2.12.4 or v2.14.0
# (packages/envoy/natsversion).
nats_version=v2.15.1
# Claude Code at the repository's own .claude-code-pin, the file every other caller reads.
claude_code_version=$(tr -d '[:space:]' <"$root/.claude-code-pin")
# Anthropic's list prices, USD per million tokens: input, output, cache write (5 minutes), cache read.
# GPT-6.1's are the model gateway's own for it, as its GET /model/info answered on 2026-11-18:
# model_info.input_cost_per_token 2e-06, output_cost_per_token 1e-05,
# cache_creation_input_token_cost 2.5e-06 and cache_read_input_token_cost 1e-07 per token.
answers_prices='    anthropic/claude-opus-5-5: { input: 5, output: 25, cache_write: 6.25, cache_read: 0.5 }
    anthropic/claude-sonnet-5-5: { input: 3, output: 15, cache_write: 3.75, cache_read: 0.3 }
    openai/gpt-6.1-sol: { input: 2, output: 10, cache_write: 2.5, cache_read: 0.1 }'
# answers.web_search (A23): the Anthropic model each search runs on, priced above as the loop's
# own model is, and the fee per search, a plain number of USD (internal/websearch), which every
# branch daemon's answers block names but A23's absence leg's.
web_search_model=anthropic/claude-sonnet-5-5
web_search_fee=0.01
# The answer bound the loop answers the GPT-6.1 merger under, in A10's GPT-6.1 clause and A23's
# GPT-6.1 search leg, each on a daemon booted for that leg (gpt_bound_leg), and every wait for one
# of those answers: the bound and a minute for the post after it. Every other row's daemon keeps
# its own bounds, answers-<store>.yaml the product's default answer_seconds of 180. In two proof
# runs one GPT-6.1 call took 191 s (A23, postgres) and 208 s (A10, postgres), and the loop ended each
# answer with its own answer_seconds fault, as A12 requires, failing rows that prove the GPT-6.1
# agent's resume and search, not the bound.
gpt_answer_seconds=300
gpt_answer_wait=$((gpt_answer_seconds + 60))
repo=sjawhar/legion-smoke
questions_file=$root/scripts/e2e/answer-loop/questions.json

# ---- run state -------------------------------------------------------------------------------------
ok=
check=setup
timeout_hook=
mode=claims-only
# The App key command inputs this run was not given, set in prerequisites and empty in workflow
# mode: what the mode note and every answering criterion's WAITING line name
# (lib/answer-loop-reads.sh, app_keys_missing_names and answering_blockers).
app_keys_missing=()
base_rev=${LEGION_E2E_BASE_REV:-main@origin}
second_repo=${LEGION_E2E_REPO_SECOND_OWNER:-}
# The model route, settled in prerequisites: which route was taken, the endpoint the recording
# proxy forwards to, and the key the daemon's answers block, Claude Code's apiKeyHelper and the
# agents' route are keyed by — a 0600 file of the run's own under `key-file`, a command under
# `gateway`. Exactly one of the two is set. gateway_token_helper is the run's 0700 script holding
# LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND, set only when that input is, and the one thing the
# gateway route's key commands then run.
model_route=
model_upstream=
model_key_file=
model_key_command=
gateway_token_helper=
# The key command install_model_route gave each profile's anthropic provider, by profile: the
# command install_gpt_route keys the same profile's openai provider with.
declare -A profile_key_command=()
# A10's GPT-6.1 agent, the branch daemon's one merger: its model, as the conversation names it and
# as its provider does (`openai/gpt-6.1-sol`, `gpt-6.1-sol`), and the thinking level it runs at
# (questions.json, agents.gpt); why the model route does not serve that model, empty when it does
# (gpt_preflight); and the Oh My Pi overlay the merger's panes name in PI_CONFIG_FILES
# (install_gpt_route, role_config_files.merger), written only when the route serves the model.
gpt_model=
gpt_model_id=
gpt_level=
gpt_refusal='the model route was not asked'
gpt_overlay=$work/gpt-merger-overlay.yml
# What the daemon gpt_bound_leg booted for the current GPT-6.1 leg reads back of its answer bound,
# in the words that leg's PASS names it with.
gpt_bound_said=
# LEGION_E2E_ANSWER_LOOP_ONLY (criteria, e.g. "boot A1 A6") and LEGION_E2E_ANSWER_LOOP_STORES
# (default "files postgres") narrow a run while a check is being fixed: the bring-up and the stopped
# agents are made as always, and only the named checks run. A run that proves the criteria sets
# neither.
only=${LEGION_E2E_ANSWER_LOOP_ONLY:-}
only=" ${only//,/ } "
stores=${LEGION_E2E_ANSWER_LOOP_STORES:-files postgres}
wanted() { [ "$only" = "  " ] || [[ "$only" == *" $1 "* ]]; }
run_tag=$(((($$ + $(date +%s)) % 90000) + 10000))
# The run's refusal marker and the reason its stand-in shared public leak check refuses it
# (install_leak_check, A17's leak-guard leg): the marker is the run's own, so no text of another
# run meets it, and it holds neither an apostrophe nor a glob character, because the stand-in
# matches it with `case` and prints the reason with `echo '…'`.
leak_marker=answer-loop-leak-$run_tag
leak_reason='the answer loop proof stand-in refuses this call'
proof_human_user=smoke
dispatch_actor=
dispatch_base=
project=
pg_pid=
nats_pid=
proxy_pid=
inbox_pid=
asker_heartbeat_pid=
listener_pid=
dispatch_pid=
daemon_pid=
listener_keeper_pid=
dispatch_keeper_pid=
daemon_keeper_pid=
port_pg=
port_nats=
port_proxy=
port_listener=
port_dispatch=
port_daemon=
store=
# The daemon under test in the current step: its binary, config, log, HOME and profile.
daemon_bin=
daemon_config=
daemon_log=
daemon_home=
daemon_profile=
daemon_plugin_dir=
# The dispatch command each daemon's answer loop runs as its dispatch tool (A22), handed to it as
# LEGION_DISPATCH_PATH: the branch's own pi-envoy pack's `bin/dispatch`, the shim the worker image
# installs, set once the branch's plugins are installed (bring-up). A22's absence leg boots one
# daemon with it empty, on a PATH holding no dispatch either. daemon_env is every variable a
# daemon boots with (daemon_environment), which `legion config check` is run with too.
daemon_dispatch=
daemon_env=()
# The claude-envoy plugin directory each daemon loads, built from its own tree (build_branch,
# build_base): a Claude Code pane's plugin, which the daemon refuses unbuilt at boot.
branch_plugin=
base_plugin=
# A3's conversation-page leg alone needs a page and a browser (browser_leg): the SPA this checkout
# builds, which Dispatch then serves, and Playwright's Chromium. Both stay empty for a run that
# drives no browser.
web_dist=
web_browser=
github_writes=()
results=()
declare -A agent=()
# The answering rows that ran in each pass, by store, as criterion lets each run, and what each
# row's control (run_controls) sent and keeps for its read, keyed `<store>:<row>.<field>`.
declare -A rows_ran=()
declare -A control=()
# What the last read said (each row's read sets it, `<row>_read`); what a control's reads said is
# control[<store>:<row>.said] (read_failed).
read_said=
# The one wait of the controls of a pass, after every control's message is sent and before any is
# read: the longest a row's own read waits for its answer (A12's, A17's, A18's and A20's 300 s) but
# for the GPT-6.1 merger's legs, whose gpt_answer_wait covers the rig's own bound on that model's
# answers (gpt_answer_seconds), which a daemon with answers removed has no block to hold.
control_wait=300
# The row that asks the loop questions now, as answers_begin opened it (criterion for an answering
# row, run_a13 for A13's control): its name and store, the store's daemon log (every boot of the
# pass appends to the one file) and how many lines it held when the row began, which questions of
# those lines are its (answers_named: empty for criterion's rows, `every` for A13's control;
# row_answers), and the writes the row's questions ask for (asks_writes), none unless the row says.
# record_result reads the row's answers out of that log and keeps what it took in answers_taken,
# one JSON line an answer, which read_pass_answers reads again once the pass is over, over the
# pass's lines since answers_pass_from; that read writes each answer of the pass as a line of
# answer_calls_tsv.
answers_row=
answers_store=
answers_log=
answers_from=
answers_named=
answers_asked=()
answers_taken=
answers_pass_from=0
answer_calls_tsv=$evidence/answer-calls.tsv

begin() { check=$1; printf '== %s\n' "$check"; }
note() { printf '   %s\n' "$*"; }
pass() { printf 'ok %s\n' "$check"; }
fail() { printf 'FAIL %s: %s\n' "$check" "$*" >&2; exit 1; }
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/omp-home.sh
. "$root/scripts/e2e/lib/omp-home.sh"
# shellcheck source-path=SCRIPTDIR source=lib/workflow.sh
. "$root/scripts/e2e/lib/workflow.sh"
# shellcheck source-path=SCRIPTDIR source=lib/model-endpoint.sh
. "$root/scripts/e2e/lib/model-endpoint.sh"
# shellcheck source-path=SCRIPTDIR source=lib/answer-loop-reads.sh
# Every read a criterion is judged by, apart from the rig that produces what they read, so that
# lib/answer-loop-reads.test.ts runs each of them over fixtures with no model and no services.
. "$root/scripts/e2e/lib/answer-loop-reads.sh"
# shellcheck source-path=SCRIPTDIR source=lib/dispatch-identity.sh
# Dispatch's own identity at the listener, as main's rigs that start both give it: a Dispatch that
# presents none refuses every send for a `role:` route, a role mention among them (A3's thread leg).
. "$root/scripts/e2e/lib/dispatch-identity.sh"
# Nothing the run starts inherits the operator's Dispatch bearer.
unset DISPATCH_TOKEN

# log_size NAME is the size of the service's log now, so a restart's wait reads only what this boot
# wrote: lib/rig.sh's start_process appends every boot of a service to the one log.
log_size() { stat -c %s "$evidence/logs/$1.log" 2>/dev/null || printf '0\n'; }
# log_lines FILE is how many lines FILE holds now. It prints nothing and fails for a file it cannot
# read, and each caller says what it takes then.
log_lines() { wc -l 2>/dev/null <"$1"; }

# record_result CRITERION VERDICT DETAIL: one summary row, for the current store ("-" for a
# criterion that does not run per store). VERDICT is PASS, FAIL or WAITING; DETAIL says why.
# Every row passes through here, so the answers of a row that asks the loop are read here and in no
# row of its own: for the row answers_begin opened in this store, every question the daemon's log
# first names since the row began (row_answers), or for A13's control every question it names since
# then, counted from there (answers_named `every`). The row records FAIL when the writes of those
# answers that posted or may have are not the writes its questions ask for (asks_writes; none
# unless the row says), when a call that ran logs no `write` (writes_fault), or when its log could
# not be read; and its detail ends with each answer's counts (answers_clause), which no verdict
# reads. The read moves the row's start to where it stopped, so a second result of the row would
# read only what came after the first, and it keeps each answer it took, with the summary line it
# took it for and the lines it read, for read_pass_answers, which reads what the answer did after.
# answers_begin opens each answering row (criterion) and A13's control, whose daemon runs with
# `answers` on, its questions asking for no write (run_a13). A row it did not open and a row of
# another store read nothing: the other controls run on a daemon with no `answers`, where no loop
# answers.
record_result() {
  local verdict=$2 detail=$3 end answers fault
  if [ -n "$answers_row" ] && [ "$1" = "$answers_row" ] && [ "${store:--}" = "$answers_store" ]; then
    if [[ $answers_from =~ ^[0-9]+$ ]] && end=$(log_lines "$answers_log") &&
      answers=$(head -n "$end" "$answers_log" | row_answers "$answers_from" "$answers_named"); then
      answers_from=$end
      if ! fault=$(writes_fault "$answers" ${answers_asked[@]+"${answers_asked[@]}"}); then
        verdict=FAIL
        detail+="; $fault"
      fi
      detail+=$(answers_clause "$answers")
      answers_taken+=$(jq -c --arg row "$1" --argjson entry "${#results[@]}" --argjson upto "$end" \
        '.[] | {question, row: $row, entry: $entry, upto: $upto}' <<<"$answers" 2>/dev/null || true)$'\n'
    else
      verdict=FAIL
      detail+="; its answers could not be read from $answers_log after its line ${answers_from:-(none counted)}, so no write of theirs was judged"
    fi
  fi
  results+=("${store:--}|$1|$verdict|$detail")
  printf '%s %s (%s): %s\n' "$verdict" "$1" "${store:--}" "$detail"
}
# fail_entry ENTRY WHY records FAIL for results[ENTRY], a row's summary line recorded earlier, WHY
# put before the answers' counts its detail ends with (answers_clause), and says so in the
# transcript. An entry the summary does not hold is left.
fail_entry() {
  local r_store r_row r_verdict r_detail head counts=
  [[ $1 =~ ^[0-9]+$ ]] && [ -n "${results[$1]:-}" ] || return 0
  IFS='|' read -r r_store r_row r_verdict r_detail <<<"${results[$1]}"
  head=${r_detail%"$answers_clause_lead"*}
  [ "$head" = "$r_detail" ] || counts="$answers_clause_lead${r_detail##*"$answers_clause_lead"}"
  results[$1]="$r_store|$r_row|FAIL|$head; $2$counts"
  printf 'FAIL %s (%s), recorded %s before the pass ended: %s\n' "$r_row" "$r_store" "$r_verdict" "$2"
}
# read_pass_answers LOG reads, once the pass's last daemon has stopped, every answer the store's
# daemon log LOG first names since the pass began (pass_answers over answers_pass_from and
# answers_taken, under the rule of the row answers_begin opened last, answers_named: after A13's
# control, every answer named since its count), so no call of an answer goes unread: an answer may
# still be running when its row records its result, and what it does after is read here. A row one
# of whose answers, past the lines the row read, made a write that posted or may have, or a call
# that logs no `write`, records FAIL now (fail_entry), naming each such call. An answer no row's
# read took, first named between two rows, is no row's to fail: the transcript names it with its
# counts and each write it made, and its line of answer_calls_tsv names the row `between rows`.
# Every answer of the pass is then a line of answer_calls_tsv, counted over the whole pass. A read
# that fails fails every row of the pass that took an answer, since none of their later calls was
# read.
read_pass_answers() {
  local answers entry late line
  if ! answers=$(pass_answers "$answers_pass_from" "$answers_taken" "$answers_named" <"$1" 2>/dev/null); then
    for entry in $(jq -r '.entry' <<<"$answers_taken" 2>/dev/null | sort -un); do
      fail_entry "$entry" "the read of its answers' calls over $1 once the pass ended failed, so a write they made after its result is unread"
    done
    note "the answers of the $store pass could not be read from $1 once the pass ended"
    return 0
  fi
  while IFS=$'\t' read -r entry late; do
    fail_entry "$entry" "after it recorded its result, its answers made calls its read had not reached: $late"
  done < <(jq -r '[.[] | select(.entry != null and (.late | length) > 0)] | group_by(.entry)[]
    | "\(.[0].entry)\t\([.[].late[]] | join(", "))"' <<<"$answers")
  while IFS= read -r line; do note "$line"; done < <(jq -r "$answer_tool_calls_jq"'.[] | select(.entry == null)
    | "an answer no row read, first named between two rows: \(counts_said)\(if (.written | length) > 0 then "; it wrote " + (.written | map(.said) | join(", ")) else "" end)"' <<<"$answers")
  answers_tsv "$store" "between rows" "$answers" >>"$answer_calls_tsv" || note "the answers of the $store pass could not be written to $answer_calls_tsv"
}
# said_text TEXT prints TEXT's first 300 bytes on one line, or `none` for no text: how a read quotes
# an answer it read (read_said), so a summary row holds one line.
said_text() {
  if [ -n "${1:-}" ]; then
    head -c 300 <<<"$1" | tr '\n' ' '
  else
    printf 'none'
  fi
}

# ---- cleanup ---------------------------------------------------------------------------------------
# github_cleanup closes each pull request (and deletes its branch) and deletes each comment a check
# made on the smoke repository, by the id it recorded (`pr:<number>:<branch>`, `comment:<id>`), as
# the implement App that made them (lib/smoke-pull-request.ts), so nothing the run made outlives it.
github_cleanup() {
  local entry kind id
  for entry in "${github_writes[@]}"; do
    kind=${entry%%:*}
    id=${entry#*:}
    case "$kind" in
      pr) timeout 120 bun "$root/scripts/e2e/lib/smoke-pull-request.ts" close "$work/github-apps.yaml" "$repo" "${id%%:*}" "${id#*:}" >&2 ||
        printf 'could not close %s pull request #%s or delete its branch\n' "$repo" "${id%%:*}" >&2 ;;
      comment) timeout 120 bun "$root/scripts/e2e/lib/smoke-pull-request.ts" delete-comment "$work/github-apps.yaml" "$repo" "$id" >&2 ||
        printf 'could not delete %s comment %s\n' "$repo" "$id" >&2 ;;
    esac
  done
}

kill_tmux_servers() {
  local socket
  for socket in "$work"/tmux/tmux-*/*; do
    [ -S "$socket" ] || continue
    TMUX_TMPDIR="$work/tmux" "$tmux_bin" -S "$socket" kill-server >/dev/null 2>&1 || true
  done
}

cleanup() {
  local status=$?
  set +e
  trap 'printf "cleanup warning: line %s exited %s\n" "$LINENO" "$?" >&2' ERR
  rm -f "$work/anthropic-key" "$work/anthropic-key-header" "$work/gpt-preflight-header" "$work/provision-token"
  stop_pid "$daemon_pid"
  [ -z "${tmux_bin:-}" ] || kill_tmux_servers
  stop_pid "$dispatch_pid"
  stop_pid "$listener_pid"
  stop_pid "$dispatch_identity_issuer_pid"
  stop_tree "$asker_heartbeat_pid"
  stop_pid "$inbox_pid"
  stop_pid "$proxy_pid"
  stop_pid "$nats_pid"
  stop_pid "$pg_pid"
  stop_pid "$daemon_keeper_pid"
  stop_pid "$dispatch_keeper_pid"
  stop_pid "$listener_keeper_pid"
  stop_marked_processes "$work" || true
  [ "${#github_writes[@]}" = 0 ] || github_cleanup
  # The App keys go next, and unconditionally. They cannot go first, as the other proofs' traps
  # take them: the undo above acts as the implement App and mints its token from that very file.
  # Everything after it is local, so nothing between the two can hang on the network.
  rm -rf -- "$work/app-key"
  # The model key file's path is in the agents' key command and Claude Code's apiKeyHelper, both
  # under the work directory: the directory goes on every exit, and the evidence stays.
  rm -rf "$work"
  # A private key never expires, so a run that left one on this host fails itself for it
  # (lib/github-apps.sh, app_key_left), whatever else this teardown did.
  app_key_left "$work/app-key" || status=1
  printf "the run's evidence is %s\n" "$evidence" >&2
  [ -n "${ok:-}" ] || [ "$status" != 0 ] || status=1
  exit "$status"
}
trap cleanup EXIT
trap 'printf "FAIL %s: line %s exited %s: %s\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- provisioning ----------------------------------------------------------------------------------
# fetch_pinned URL SHA256 FILE downloads URL to FILE once, kept in the cache, and refuses a file whose
# sha256 is not the pinned one.
fetch_pinned() {
  local url=$1 want=$2 file=$3 got
  if [ ! -f "$file" ] || [ "$(sha256sum "$file" | cut -d' ' -f1)" != "$want" ]; then
    curl -fsSL --max-time 600 -o "$file.part" "$url" || fail "could not download $url"
    mv "$file.part" "$file"
  fi
  got=$(sha256sum "$file" | cut -d' ' -f1)
  [ "$got" = "$want" ] || fail "$url has sha256 $got, not the pinned $want"
}

# provision_postgres unpacks the zonky jar's postgres-linux-x86_64.txz (Python's lzma and tarfile:
# a pod has no xz) and copies pgvector's library and extension files into it, once per cache.
provision_postgres() {
  pg_root=$cache/postgres-$pg_version
  [ -x "$pg_root/bin/postgres" ] && [ -f "$pg_root/lib/postgresql/vector.so" ] && return 0
  fetch_pinned "$pg_jar_url" "$pg_jar_sha256" "$cache/postgres-$pg_version.jar"
  fetch_pinned "$debian_pool/$pgvector_deb" "$pgvector_sha256" "$cache/$(basename "$pgvector_deb")"
  rm -rf "$pg_root.part" "$work/pgvector"
  python3 - "$cache/postgres-$pg_version.jar" "$pg_root.part" <<'PY'
import io, lzma, sys, tarfile, zipfile
jar = zipfile.ZipFile(sys.argv[1])
name = next(n for n in jar.namelist() if n.endswith(".txz"))
tarfile.open(fileobj=io.BytesIO(lzma.decompress(jar.read(name)))).extractall(sys.argv[2], filter="tar")
PY
  dpkg-deb -x "$cache/$(basename "$pgvector_deb")" "$work/pgvector"
  cp "$work/pgvector/usr/lib/postgresql/17/lib/vector.so" "$pg_root.part/lib/postgresql/"
  cp "$work/pgvector/usr/share/postgresql/17/extension/"vector* "$pg_root.part/share/postgresql/extension/"
  mv "$pg_root.part" "$pg_root"
}

# pg_sql DATABASE SQL prints the statement's rows as one JSON array, through Bun's Postgres client
# (the host has no psql).
# The reads of the session table through it (check_postgres_store, lib/answer-loop-reads.sh's
# conversation_text) name each table public.<table> and set no pin, unlike lib/session-db.sh's
# session_psql: they run on the run's own scratch Postgres, which the rig starts and whose only
# clients are the rig, its daemon and its panes. The daemon's own reads of it are pinned.
pg_sql() {
  bun "$work/pg-sql.ts" "postgres://postgres@127.0.0.1:$port_pg/$1?sslmode=disable" "$2"
}

# start_postgres starts the run's Postgres, the one service of the run on a port number it picks
# rather than on one the kernel chose and something holds (lib/rig.sh): postgres binds its port
# itself, takes no inherited socket and refuses port 0. The pick is below the kernel's ephemeral
# range, from 10000, where the kernel numbers no connection's own port, and it can still be taken
# before postgres binds it, so a start that fails is made again on another pick, five times in all.
start_postgres() {
  local attempt low
  read -r low _ </proc/sys/net/ipv4/ip_local_port_range
  [ "$low" -gt 11000 ] || fail "the ephemeral port range starts at $low, leaving Postgres no room above 10000"
  cat >"$work/pg-sql.ts" <<'TS'
const [dsn, query] = process.argv.slice(2);
const sql = new Bun.SQL(dsn as string);
const rows = await sql.unsafe(query as string);
console.log(JSON.stringify([...rows]));
await sql.close();
TS
  LD_LIBRARY_PATH="$pg_root/lib" "$pg_root/bin/initdb" -D "$work/pg-data" -U postgres --auth=trust -E UTF8 --locale=C \
    >>"$evidence/logs/postgres.log" 2>&1 || fail "initdb failed; see $evidence/logs/postgres.log"
  mkdir -p "$work/pg-sock"
  for attempt in 1 2 3 4 5; do
    port_pg=$((10000 + RANDOM % (low - 10000)))
    if LD_LIBRARY_PATH="$pg_root/lib" "$pg_root/bin/pg_ctl" -D "$work/pg-data" -l "$evidence/logs/postgres.log" -w -t 60 \
      -o "-p $port_pg -h 127.0.0.1 -k $work/pg-sock" start >/dev/null; then
      pg_pid=$(head -1 "$work/pg-data/postmaster.pid")
      record_start "$pg_pid" || started_at[$pg_pid]=gone
      return 0
    fi
    note "Postgres did not start on port $port_pg (attempt $attempt); picking another"
  done
  fail "Postgres did not start; see $evidence/logs/postgres.log"
}

# provision_tmux sets tmux_bin: the host's tmux, else tmux unpacked from Debian with a wrapper that
# binds its libraries (docs/solutions/testing/a-worker-pod-ships-no-go-or-tmux-…): the tmux runtime
# starts its server with the pane allow-list, which passes PATH and drops LD_LIBRARY_PATH.
provision_tmux() {
  local entry deb sha root_dir=$cache/tmux-root
  mkdir -p "$work/bin"
  if command -v tmux >/dev/null 2>&1; then
    ln -sfn "$(command -v tmux)" "$work/bin/tmux"
  else
    if [ ! -x "$root_dir/usr/bin/tmux" ]; then
      rm -rf "$root_dir.part"
      for entry in "${tmux_debs[@]}"; do
        deb=${entry% *}
        sha=${entry#* }
        fetch_pinned "$debian_pool/$deb" "$sha" "$cache/$(basename "$deb")"
        dpkg-deb -x "$cache/$(basename "$deb")" "$root_dir.part"
      done
      mv "$root_dir.part" "$root_dir"
    fi
    printf '#!/bin/sh\nLD_LIBRARY_PATH=%s/usr/lib/x86_64-linux-gnu exec %s/usr/bin/tmux "$@"\n' "$root_dir" "$root_dir" >"$work/bin/tmux"
    chmod 0755 "$work/bin/tmux"
  fi
  tmux_bin=$work/bin/tmux
  note "tmux: $("$tmux_bin" -V) ($(readlink -f "$work/bin/tmux"))"
}

provision_nats() {
  nats_bin=$cache/gobin/nats-server
  if [ ! -x "$nats_bin" ] || [ "$("$nats_bin" --version 2>&1)" != "nats-server: $nats_version" ]; then
    (cd "$root/packages/envoy" && GOBIN="$cache/gobin" GOFLAGS=-mod=mod go install "github.com/nats-io/nats-server/v2@$nats_version") ||
      fail "go install github.com/nats-io/nats-server/v2@$nats_version failed"
  fi
  note "nats-server: $("$nats_bin" --version)"
}

provision_claude() {
  local dir=$cache/claude-code-$claude_code_version
  if [ ! -e "$dir/node_modules/.bin/claude" ]; then
    mkdir -p "$dir"
    bun add --cwd "$dir" --exact "@anthropic-ai/claude-code@$claude_code_version" >&2 ||
      fail "bun add @anthropic-ai/claude-code@$claude_code_version failed"
  fi
  claude_bin=$(readlink -f "$dir/node_modules/.bin/claude")
  local version
  version=$("$claude_bin" --version 2>&1) || fail "$claude_bin --version failed: $version"
  [[ "$version" == "$claude_code_version"* ]] || fail "Claude Code reports $version, not the pinned $claude_code_version"
  note "Claude Code: $claude_bin ($version)"
}

resolve_omp() {
  local pin
  if [ -n "${LEGION_E2E_OMP_BIN:-}" ]; then
    omp_bin=$LEGION_E2E_OMP_BIN
  elif [ -x /opt/omp/bin/omp ]; then
    omp_bin=/opt/omp/bin/omp
  else
    pin=$(<"$root/.omp-pin")
    omp_bin=$(mise where "$pin")/bin/omp || fail "no Oh My Pi: set LEGION_E2E_OMP_BIN, or install the .omp-pin with mise"
  fi
  [ -x "$omp_bin" ] || fail "the Oh My Pi binary $omp_bin is not executable"
  note "Oh My Pi: $omp_bin ($("$omp_bin" --version 2>&1 | head -1)); .omp-pin names $(<"$root/.omp-pin")"
}

# real_tools writes $work/realbin, the daemon's PATH ahead of the system's: the first gh on PATH that
# is a compiled program (a Legion pane's first gh is the `legion gh` wrapper script, which the daemon
# refuses), and the host's jj, git and bun.
real_tools() {
  local dir gh=
  mkdir -p "$work/realbin"
  local IFS=:
  for dir in $PATH; do
    [ -x "$dir/gh" ] && [ ! -d "$dir/gh" ] || continue
    [ "$(head -c 2 "$dir/gh")" = '#!' ] && continue
    gh=$dir/gh
    break
  done
  unset IFS
  [ -n "$gh" ] || fail "no compiled gh on PATH (the daemon refuses a gh wrapper script)"
  ln -sfn "$gh" "$work/realbin/gh"
  for tool in jj git bun; do ln -sfn "$(command -v "$tool")" "$work/realbin/$tool"; done
  daemon_path="$work/bin:$work/realbin:/usr/local/bin:/usr/bin:/bin"
}

# install_leak_check writes the run's stand-in shared public leak check, $work/bin's
# public-leak-check.sh — the name the guard looks for on PATH, and $work/bin is the first entry of
# the PATH every daemon of this run boots with (real_tools, start_daemon). The real check is a
# private script no clone of this repository holds (a pod installs it from a private repository),
# and the guard refuses a write it can run no check for, so without this file every gh call of the
# answer loop would be refused before gh started and every other gh leg of this run — A15's two
# answers, A17's comment, A18's restart and resume, A19's review — would fail with it. It is
# written once, before the first daemon boots.
#
# The shape is the one packages/daemon/internal/leakguard/leakguardtest/leakguardtest.go states
# for the gh guard, and scripts/e2e/leak-guard-image-probe.sh writes stand-ins of its own the same
# way: it reads its standard input, because a check that did not would leave the guard writing
# into a pipe nobody drains; it exits 3 with a report line of its own for a call whose arguments
# hold the run's refusal marker, which is the whole account of the refusal anyone gets; and it
# exits 0 for every other call, so every verdict of this run but A17's leak-guard leg is the
# daemon's own. The arguments are what it reads: the loop hands gh no standard input
# (ghtool's Answer.runGH), so the text of a call it makes stands in the call's own arguments, and
# the exit-4 handshake that would buffer that input is never asked for.
install_leak_check() {
  local check=$work/bin/public-leak-check.sh
  mkdir -p "$work/bin"
  cat >"$check" <<SH
#!/bin/sh
# The stand-in shared public leak check of one run of scripts/e2e/answer-loop.sh, written by it.
cat >/dev/null 2>&1
case " \$* " in
  *$leak_marker*)
    echo 'LEAK CHECK: refusing: $leak_reason'
    exit 3
    ;;
esac
exit 0
SH
  chmod 0700 "$check"
  note "stand-in shared public leak check: $check (exit 3 for a call naming $leak_marker, exit 0 for every other)"
}

# ---- builds ----------------------------------------------------------------------------------------
# build_claude_plugin SOURCE DEST builds SOURCE's claude-envoy into DEST, a new directory outside
# the checkout, as claude-backend-tmux.sh does: a tree carries no built dist/ or legion/
# (packages/claude-envoy/scripts/build.ts --out), and the daemon refuses a plugin directory without
# dist/envoy-channel.js at boot. DEST is printed, so a caller sets its plugin directory from it.
build_claude_plugin() {
  bun "$1/packages/claude-envoy/scripts/build.ts" --out "$2" >&2 ||
    fail "could not build $1's claude-envoy plugin into $2"
  [ -f "$2/dist/envoy-channel.js" ] || fail "$2 holds no dist/envoy-channel.js after the build"
  printf '%s' "$2"
}

build_branch() {
  mkdir -p "$work/branch"
  (cd "$root/packages/daemon" && go build -o "$work/branch/legion" ./cmd/legion)
  (cd "$root/packages/envoy" && go build -o "$work/branch/envoy-listener" ./cmd/listener && go build -o "$work/branch/envoy-dispatch" ./cmd/dispatch)
  branch_plugin=$(build_claude_plugin "$root" "$work/claude-envoy-branch")
  built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/branch/legion" "$work/branch/envoy-listener" "$work/branch/envoy-dispatch") ||
    fail "lib/built-from.sh could not say what the run built"
  while IFS= read -r line; do note "branch $line"; done <<<"$built"
  note "branch claude-envoy plugin: $branch_plugin"
}

# browser_leg: whether this run drives a browser, which only A3's conversation-page leg does. It
# needs the daemon's workflow (without both App key commands every answering criterion, A3 among
# them, records WAITING and runs nothing) and A3 among the criteria a narrowed run names, so a
# claims-only run and a run narrowed to other criteria neither build the SPA nor install Chromium.
browser_leg() { [ "$mode" = workflow ] && wanted A3; }

# provision_browser: the Chromium A3's conversation-page leg drives, Playwright's own, installed
# once under PLAYWRIGHT_BROWSERS_PATH — the caller's when it names one, else the run's cache, never
# the home directory of a pod whose disk a browser fills. It sets web_browser to the executable
# Playwright resolves, which the helper then launches with no executablePath of its own.
provision_browser() {
  export PLAYWRIGHT_BROWSERS_PATH=${PLAYWRIGHT_BROWSERS_PATH:-$cache/ms-playwright}
  web_browser=$(playwright_chromium) || web_browser=
  if [ -z "$web_browser" ] || [ ! -x "$web_browser" ]; then
    (cd "$root/packages/dispatch" && bunx playwright install chromium >"$evidence/logs/playwright-install.log" 2>&1) ||
      fail "playwright install chromium failed; see $evidence/logs/playwright-install.log"
    web_browser=$(playwright_chromium) ||
      fail "Playwright resolves no Chromium under PLAYWRIGHT_BROWSERS_PATH=$PLAYWRIGHT_BROWSERS_PATH after installing it"
  fi
  [ -x "$web_browser" ] || fail "Playwright's Chromium $web_browser is not executable"
  note "Chromium for A3's conversation-page leg: $web_browser (PLAYWRIGHT_BROWSERS_PATH=$PLAYWRIGHT_BROWSERS_PATH)"
}

# playwright_chromium prints the Chromium Playwright would launch, from the packages/dispatch
# install the page helper resolves it from; it fails when none is installed.
playwright_chromium() {
  (cd "$root/packages/dispatch" && bun -e 'console.log(require("@playwright/test").chromium.executablePath())' 2>/dev/null)
}

# build_web builds Dispatch's SPA from the checkout into packages/dispatch/web/dist and sets
# web_dist, which start_dispatch then serves (DISPATCH_WEB_DIST): the page A3's conversation-page
# leg opens. Nothing else in this proof reads a page, so only browser_leg runs builds it.
build_web() {
  (cd "$root/packages/dispatch" && bun run build:web >"$evidence/logs/build-web.log" 2>&1) ||
    fail "Dispatch's web app did not build; see $evidence/logs/build-web.log"
  web_dist=$root/packages/dispatch/web/dist
  [ -r "$web_dist/index.html" ] ||
    fail "$web_dist holds no index.html after bun run build:web"
  note "Dispatch's SPA: $web_dist (built from this checkout)"
}

# build_base builds the base commit's three binaries from a `git archive` of it (the checkout is
# read, never changed) and installs its workspace, whose plugins the base daemon loads. The base
# commit is the merge base of LEGION_E2E_BASE_REV and this checkout (merge_base_commit), never the
# revision itself when it is ahead of the branch: a change the base revision carries and the branch
# does not would otherwise read as the branch's own (proof run 44095624771, main's #4281).
build_base() {
  local resolved rev_commit head_commit
  resolved=$(merge_base_commit "$root" "$base_rev") ||
    fail "the base revision $base_rev, or this checkout's head, does not resolve to one commit sharing history with the other"
  read -r base_commit rev_commit head_commit <<<"$resolved"
  if [ "$base_commit" = "$rev_commit" ]; then
    note "A13's base is $base_commit, the base revision $base_rev itself: it is an ancestor of this checkout's head $head_commit"
  else
    note "A13's base is $base_commit, the merge base of the base revision $base_rev ($rev_commit) and this checkout's head $head_commit: the revision is ahead of the branch, and what it carries beyond the merge base is none of the branch's change"
  fi
  # The base commit's tree, its three binaries and its installed workspace are kept in the cache by
  # commit: a commit's build is the same every run.
  base_src=$cache/base-$base_commit/src
  if [ ! -x "$cache/base-$base_commit/bin/envoy-dispatch" ]; then
    rm -rf "$cache/base-$base_commit"
    mkdir -p "$base_src" "$cache/base-$base_commit/bin"
    git -C "$root" archive "$base_commit" | tar -x -C "$base_src"
    (cd "$base_src/packages/daemon" && GOFLAGS=-buildvcs=false go build -o "$cache/base-$base_commit/bin/legion" ./cmd/legion)
    (cd "$base_src/packages/envoy" && GOFLAGS=-buildvcs=false go build -o "$cache/base-$base_commit/bin/envoy-listener" ./cmd/listener &&
      GOFLAGS=-buildvcs=false go build -o "$cache/base-$base_commit/bin/envoy-dispatch" ./cmd/dispatch)
    (cd "$base_src" && bun install --frozen-lockfile >/dev/null)
  fi
  ln -sfn "$cache/base-$base_commit/bin" "$work/base"
  base_plugin=$(build_claude_plugin "$base_src" "$work/claude-envoy-base")
  local b
  for b in legion envoy-listener envoy-dispatch; do
    note "base $b: sha256 $(sha256sum "$work/base/$b" | cut -d' ' -f1), built from git archive $base_commit ($base_rev)"
  done
  note "base claude-envoy plugin: $base_plugin"
}

# install_plugins SOURCE HOME PROFILE DEST installs SOURCE's pi-envoy and pi-legion into the profile,
# each packed by SOURCE's own lib/pack-plugin.sh as the release packs it, then `omp plugin install`
# with the run's Oh My Pi, as lib/install-plugin-profile.sh does with the mise-pinned one (a pod has
# no mise).
install_plugins() {
  local source=$1 home=$2 profile=$3 dest=$4 package tarball listing name version
  for package in pi-envoy pi-legion; do
    tarball=$(bash "$source/scripts/e2e/lib/pack-plugin.sh" "$package" "$dest/$package-pack")
    mkdir -p "$dest/$package"
    tar xzf "$tarball" -C "$dest/$package" --strip-components=1
    (cd "$dest/$package" && HOME=$home OMP_PROFILE=$profile "$omp_bin" plugin install "$dest/$package" >&2)
    name=$(jq -r .name "$dest/$package/package.json")
    version=$(jq -r .version "$dest/$package/package.json")
    listing=$(cd "$dest/$package" && HOME=$home OMP_PROFILE=$profile "$omp_bin" plugin list --json)
    jq -e --arg name "$name" --arg v "$version" '[.npm[]? | select(.name == $name and .version == $v and .enabled == true)] | length == 1' \
      <<<"$listing" >/dev/null || fail "profile $profile under $home does not list $name@$version enabled"
    note "$name@$version enabled in profile $profile ($dest/$package)"
  done
}

# install_model_route HOME PROFILE LABEL installs the run's model route into the profile — the key
# file route (lib/install-key-file-model-route.sh) or the gateway's (lib/install-model-gateway.sh),
# whichever LEGION_E2E_MODEL_ROUTE named — then points it at the recording proxy under LABEL and at
# the questions file's model, so every agent turn is recorded and reaches the route's own endpoint
# through the proxy. The key command it installed is kept as the profile's (profile_key_command).
install_model_route() {
  local home=$1 profile=$2 label=$3 agent_dir model proxy_url key_command
  local -a keying=()
  model=$(jq -r .model "$questions_file")
  proxy_url=http://127.0.0.1:$port_proxy/tag/$label
  if [ "$model_route" = gateway ]; then
    # hawk-token mints and keeps one key; the run's own token helper is run again for every call,
    # since the token it prints expires long before the run ends.
    if [ -n "$gateway_token_helper" ]; then
      keying=(--token-command "$gateway_token_helper")
    else
      keying=(--cache-dir "$work/key-$label-cache")
    fi
    key_command=$(bash "$root/scripts/e2e/lib/install-model-gateway.sh" --profile "$profile" --home "$home" \
      --dest "$evidence/model-route-$label" "${keying[@]}") ||
      fail "the agents' model route through the model gateway could not be installed (the reason is above)"
    # The daemon's answers block and Claude Code's apiKeyHelper take the first profile's key
    # command: every one of them keys the same gateway the same way.
    [ -n "$model_key_command" ] || model_key_command=$key_command
  else
    key_command=$(bash "$root/scripts/e2e/lib/install-key-file-model-route.sh" --profile "$profile" --home "$home" \
      --dest "$evidence/model-route-$label" --key-dir "$work/key-$label" --key-file "$model_key_file" \
      --base-url "$model_upstream") ||
      fail "the agents' model route could not be installed (the reason is above)"
  fi
  profile_key_command[$profile]=$key_command
  agent_dir=$home/.omp/profiles/$profile/agent
  sed -i "s#baseUrl: $model_upstream#baseUrl: $proxy_url#" "$agent_dir/models.yml"
  sed -i "s#anthropic/claude-opus-4-8#anthropic/$model#g" "$agent_dir/config.yml"
  grep -q "tag/$label" "$agent_dir/models.yml" || fail "the profile's models.yml does not name the proxy"
  note "profile $profile: anthropic/$model through the recording proxy as $label, upstream $model_upstream"
}

# gpt_preflight asks the run's model route, once and before any agent is made, whether it serves
# the questions file's GPT model: one POST /v1/responses for it, a one-line input with store false,
# through the recording proxy under a label of its own, gpt-preflight, so the evidence holds the
# call and its answer (model-calls.jsonl). It is keyed as the daemon's loop keys an OpenAI call,
# `Authorization: Bearer <key>` (packages/envoy/agentloop/transport.go), with the key the route
# keys every other call with: the key-file route's copy, else the gateway route's key command, run
# once as the daemon runs it. The key reaches curl in a 0600 header file removed as soon as the
# calls end, and is taken out of the refusal wherever the answer echoes it (preflight_refusal).
# An answer that says nothing of the model — no answer at all, the proxy's own 502, 408, 429 or
# another 5xx — is asked again, ten seconds later, up to three times. It sets gpt_refusal to
# nothing when the call answered 200 with a response whose status is `completed`; to the route's
# refusal of the model for any other answer; and to the route not answering the preflight where
# its last answer is still one of those. A10's GPT-6.1 clause records WAITING on the line.
gpt_preflight() {
  local header=$work/gpt-preflight-header body=$work/gpt-preflight.json key code status=0 refusal attempt transient
  if [ -n "$model_key_file" ]; then
    key=$(<"$model_key_file")
  else
    key=$("$model_key_command" 2>/dev/null) || status=$?
  fi
  key=${key//[$' \t\r\n']/}
  if [ "$status" != 0 ] || [ -z "$key" ]; then
    gpt_refusal="the $model_route route's key command $model_key_command printed no key (exit $status), so nothing asked the route for $gpt_model_id"
  else
    (umask 077 && printf 'Authorization: Bearer %s\n' "$key" >"$header")
    for attempt in 1 2 3 4; do
      rm -f "$body"
      code=$(curl -sS --max-time 300 -o "$body" -w '%{http_code}' -H "@$header" -H 'content-type: application/json' \
        --data "$(jq -cn --arg model "$gpt_model_id" '{model: $model, input: "Reply with the one word: ready.", store: false}')" \
        "http://127.0.0.1:$port_proxy/tag/gpt-preflight/v1/responses") || code="unreachable (curl exited $?)"
      case "$code" in
        408 | 429 | 5[0-9][0-9]) transient=1 ;;
        [0-9][0-9][0-9]) transient= ;;
        *) transient=1 ;;
      esac
      [ -n "$transient" ] && [ "$attempt" != 4 ] || break
      note "GPT-6.1: the preflight's call $attempt answered $code, which says nothing of the model; asking again in 10 s"
      sleep 10
    done
    rm -f "$header"
    refusal=$(preflight_refusal "$code" "$body" "$key")
    gpt_refusal=
    if [ -n "$refusal" ] && [ -n "$transient" ]; then
      gpt_refusal="the $model_route route ($model_upstream) did not answer the preflight ($attempt calls of POST /v1/responses for $gpt_model_id through the recording proxy, gpt-preflight, ten seconds apart; the last $refusal)"
    elif [ -n "$refusal" ]; then
      gpt_refusal="the $model_route route ($model_upstream) does not serve $gpt_model_id: POST /v1/responses through the recording proxy (gpt-preflight) $refusal"
    fi
  fi
  unset key
  if [ -z "$gpt_refusal" ]; then
    note "GPT-6.1: the $model_route route serves $gpt_model_id: POST /v1/responses through the recording proxy (gpt-preflight) answered 200 at call $attempt, response $(jq -r '.id' "$body") completed on $(jq -r '.model' "$body"); A10's GPT-6.1 clause runs"
  else
    note "GPT-6.1: $gpt_refusal; A10's GPT-6.1 clause records WAITING"
  fi
}

# gpt_route_setting HOME PROFILE KEY [OVERLAY] prints, as JSON, what the run's Oh My Pi resolves
# the setting KEY to under PROFILE, and with OVERLAY laid over it as a pane's PI_CONFIG_FILES lays
# it: `omp config get`, run as install_plugins runs omp, with none of the operator's own session's
# variables (omp_session_unsets).
gpt_route_setting() {
  (cd "$work" && env "${omp_session_unsets[@]}" "HOME=$1" "OMP_PROFILE=$2" ${4:+"PI_CONFIG_FILES=$4"} \
    "$omp_bin" config get "$3" --json) | jq -c '.value'
}

# install_gpt_route HOME PROFILE LABEL adds GPT-6.1 to the profile install_model_route routed, for
# the run's one GPT-6.1 agent and for no other pane. Its models.yml gains the openai provider on
# OpenAI's Responses API at the recording proxy under LABEL, plus the /v1 that Oh My Pi's
# openai-responses client appends `responses` to, keyed by the very key command the profile's
# anthropic provider names (profile_key_command). Its config.yml adds openai to disabledProviders:
# a pane's subagents and retries choose from every enabled provider (the installers' own
# config.yml comments), and no pane but the merger may fall back to GPT-6.1. The overlay
# role_config_files gives the merger alone (write_legion_config) makes GPT-6.1 the model a
# merger's pane starts on (every pane starts `--model @default`, internal/runtime/tmux/prompt.go)
# at the questions file's thinking level, which Oh My Pi writes into the session as its
# thinking_level_change: the level the loop answers that conversation at. Oh My Pi takes a list
# of an overlay whole, so the overlay names the profile's disabledProviders as they were before
# openai joined them. What Oh My Pi then resolves is read back, and a profile or overlay that does
# not resolve as written fails the run.
install_gpt_route() {
  local home=$1 profile=$2 label=$3 agent_dir=$1/.omp/profiles/$2/agent disabled resolved
  disabled=$(gpt_route_setting "$home" "$profile" disabledProviders) &&
    jq -e 'type == "array" and all(.[]; type == "string") and (index("openai") | not)' <<<"$disabled" >/dev/null ||
    fail "the profile $profile's disabledProviders read as ${disabled:-nothing}, not a list without openai"
  cat >>"$agent_dir/models.yml" <<EOF
  openai:
    baseUrl: http://127.0.0.1:$port_proxy/tag/$label/v1
    api: openai-responses
    auth: apiKey
    apiKey: "!exec ${profile_key_command[$profile]}"
    models:
      - id: $gpt_model_id
        name: GPT-6.1
        reasoning: true
EOF
  sed -i '/^disabledProviders:$/a\  - openai' "$agent_dir/config.yml"
  {
    cat <<EOF
# Written by scripts/e2e/answer-loop.sh: role_config_files.merger, the Oh My Pi overlay of the run's
# one GPT-6.1 agent and of no other pane. A pane starts \`--model @default\`, so the default role is
# the model it runs on, and defaultThinkingLevel the level it runs at, which Oh My Pi records in
# the session as its thinking_level_change. Oh My Pi merges modelRoles into the profile's key by
# key, so the other roles stay the profile's, and takes a list whole: enabledModels names both
# providers, the profile's anthropic and this agent's openai, and disabledProviders is the
# profile's own list without the openai that keeps every other pane off GPT-6.1.
enabledModels:
  - anthropic/*
  - openai/*
modelRoles:
  default: $gpt_model
defaultThinkingLevel: $gpt_level
EOF
    jq -r 'if length == 0 then "disabledProviders: []" else "disabledProviders:", (.[] | "  - \(.)") end' <<<"$disabled"
  } >"$gpt_overlay"
  resolved=$(gpt_route_setting "$home" "$profile" disabledProviders) || resolved=null
  jq -e --argjson before "$disabled" 'type == "array" and sort == ($before + ["openai"] | sort)' <<<"$resolved" >/dev/null ||
    fail "the profile $profile resolves disabledProviders to $resolved, not $disabled with openai"
  resolved=$(jq -cn --argjson disabled "$(gpt_route_setting "$home" "$profile" disabledProviders "$gpt_overlay")" \
    --argjson roles "$(gpt_route_setting "$home" "$profile" modelRoles "$gpt_overlay")" \
    --argjson level "$(gpt_route_setting "$home" "$profile" defaultThinkingLevel "$gpt_overlay")" \
    '{disabled: $disabled, default: $roles.default, level: $level}') || resolved=null
  jq -e --argjson before "$disabled" --arg model "$gpt_model" --arg level "$gpt_level" \
    '.disabled == $before and .default == $model and .level == $level' <<<"$resolved" >/dev/null ||
    fail "under the merger's overlay $gpt_overlay the profile $profile resolves $resolved, not disabledProviders $disabled, default $gpt_model and thinking level $gpt_level"
  note "profile $profile: $gpt_model through the recording proxy as $label, openai among its disabledProviders; the merger's overlay $gpt_overlay resolves $resolved"
}

# ---- services --------------------------------------------------------------------------------------
service_env=(env -i "PATH=$PATH" "LANG=C.UTF-8" "TZ=UTC" "LEGION_E2E_RUN=$work")

# start_nats starts nats-server on a port the kernel chooses (`-p -1`), read back from the line this
# start logged (lib/rig.sh's report_port): nats-server takes no inherited socket, so no keeper holds
# its port. Each start serves a port of its own, A13's restarts included, and everything that dials
# NATS after a restart (the listener, Dispatch and each daemon's legion.yaml) is started or written
# after it. The inbox too, once, in A13's branch phase, whose control rows read it (run_a13); the
# base phase restarts NATS again and reads no inbox.
start_nats() {
  local offset
  mkdir -p "$work/nats-store"
  offset=$(log_size nats)
  start_process nats "${service_env[@]}" "$nats_bin" -js -a 127.0.0.1 -p -1 -sd "$work/nats-store"
  await_start nats "$nats_pid" 60 "nats-server to be ready" \
    sh -c "tail -c +$((offset + 1)) '$evidence/logs/nats.log' | grep -q 'Server is ready'"
  port_nats=$(report_port <(tail -c +$((offset + 1)) "$evidence/logs/nats.log") 'Listening for client connections on [^ ]*:') ||
    fail "nats-server logged no client port; see $evidence/logs/nats.log"
}

start_proxy() {
  local offset
  local -a bearer=()
  # Oh My Pi, Claude Code and the daemon's answer loop all key an Anthropic call with x-api-key.
  # A gateway keyed by LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND reads the token from
  # `Authorization: Bearer` instead, and this hop, the one every call of the run already goes
  # through, is where that translation lives (lib/recording-proxy.ts).
  [ -z "$gateway_token_helper" ] || bearer=(--api-key-as-bearer)
  mkdir -p "$work/proxy-hold" "$work/proxy-stand-in"
  offset=$(log_size proxy)
  # lib/recording-proxy.ts takes a port number and no inherited socket, so it binds port 0 itself
  # and logs the port it got. The run starts it once, before anything names its port.
  start_process proxy "${service_env[@]}" "HOME=$work" bun "$root/scripts/e2e/lib/recording-proxy.ts" --port 0 \
    --upstream "$model_upstream" --record "$evidence/model-calls.jsonl" \
    --hold-dir "$work/proxy-hold" --stand-in-dir "$work/proxy-stand-in" "${bearer[@]}"
  await_start proxy "$proxy_pid" 30 "the recording proxy" \
    sh -c "tail -c +$((offset + 1)) '$evidence/logs/proxy.log' | grep -q 'recording-proxy listening'"
  port_proxy=$(report_port <(tail -c +$((offset + 1)) "$evidence/logs/proxy.log") 'recording-proxy listening on [^ ]*:') ||
    fail "the recording proxy logged no port; see $evidence/logs/proxy.log"
}

# start_inbox subscribes the run's inbox to the relay subjects of its two stand-in sessions,
# `notifications.envoy.deliver.<session>`, where the listener relays each session's own consumer:
# no session receives from a plain NATS subscription ("Session consumers" in
# packages/envoy/AGENTS.md). The asker's consumer carries the answers to its questions, and the
# controller stand-in's the notices on both passes' controller lanes, whose roles it holds
# (register_asker). The inbox answers each frame `done` once it has recorded it, and the
# listener's connection probe on the subject `done` too (lib/envoy-inbox.ts). It subscribes before
# the first registration, so the relay of a consumer that registration creates finds it there.
# The inbox also watches the first holder's session (holder_session), which A21's last leg registers
# to claim a role that nobody holds.
start_inbox() {
  local offset
  offset=$(log_size inbox)
  start_process inbox "${service_env[@]}" "HOME=$work" bun "$root/scripts/e2e/lib/envoy-inbox.ts" \
    --nats "nats://127.0.0.1:$port_nats" --record "$evidence/inbox.jsonl" \
    --watch "$asker_session=notifications.envoy.deliver.$asker_session" \
    --watch "$controller_session=notifications.envoy.deliver.$controller_session" \
    --watch "$holder_session=notifications.envoy.deliver.$holder_session"
  await_start inbox "$inbox_pid" 30 "the Envoy inbox" \
    sh -c "tail -c +$((offset + 1)) '$evidence/logs/inbox.log' | grep -q 'envoy-inbox listening'"
}

# start_listener BINARY_DIR and start_dispatch BINARY_DIR DATABASE start the listener and Dispatch
# as claude-backend-tmux.sh's functions of those names do, each on a socket held for it and handed
# over as ENVOY_LISTEN_FD or DISPATCH_LISTEN_FD, but for two things. First, each serves on a copy
# of the socket a keeper holds for the whole run (lib/rig.sh's keep_port, run by its first start,
# and start_kept), where claude-backend-tmux.sh, which starts each once, opens a fresh one
# (start_held): A13 restarts both, the base commit's binaries among them, and a restart cannot lose
# a kept port. The asker's heartbeat, a subshell forked at bring-up, keeps calling the listener's
# port it was forked with. Between two servers a kept port listens and nothing answers, so each
# poll of it carries a timeout (curl --max-time). Second, every variable is named (env -i), so a
# pod's own ENVOY_*, DISPATCH_* and LEGION_* never reach them, and the command names its descriptor
# variable itself, =3, the descriptor held-port.py hands the socket over as: env -i drops the one
# held-port.py sets.
# The listener also serves GitHub's webhook route, `/webhook/github`, as a deployment's does, since
# a running daemon hears a review only as its webhook (A19, deliver_review): ENVOY_WEBHOOKS=github,
# the route's secret the run's own random value ($work/github-webhook-secret, 0600, printed
# nowhere), and ENVOY_REVIEWER_APP_ID, without which the route refuses to start. That id only tells
# the reviewer's verdict check runs from another App's, and this proof delivers no check run, so a
# run that read no review App names lib/github-apps.sh's default. The mention trigger is the
# listener's own default, and the CI store the route records checks in is the listener's own
# bucket on the run's NATS. The base commit's listener (A13) is started the same way.
# start_listener's words after BINARY_DIR, each NAME=VALUE, are more of the listener's environment:
# A13 gives both of its listeners one role claim reaper period and one live window (run_a13), and
# the bring-up's start gives none, so the pass's listener keeps the deployment's defaults. Each
# start records the log's size before it and the second its listener first answered
# (listener_log_from, listener_opened), which await_reaper_pass reads. The listener's registry is
# open by that second: /v1 answers 503 until it is, and the start waits for a 2xx.
start_listener() {
  local bin=$1
  shift
  [ -n "$listener_keeper_pid" ] || keep_port listener_keeper port_listener
  listener_log_from=$(log_size listener)
  start_kept listener listener_keeper ENVOY_LISTEN_FD -- "${service_env[@]}" "HOME=$work/listener-home" \
    "ENVOY_API_TOKEN=$(cat "$work/envoy-token")" ENVOY_LISTEN_FD=3 "PORT={port}" ENVOY_LISTEN_HOST=127.0.0.1 \
    "ENVOY_MACHINE_ID=legion-e2eal-$$" ENVOY_WEBHOOKS=github "ENVOY_GITHUB_WEBHOOK_SECRET=$(cat "$work/github-webhook-secret")" \
    "ENVOY_REVIEWER_APP_ID=${review_app_id:-$default_review_app_id}" "${dispatch_identity_listener_env[@]}" \
    "NATS_URLS=nats://127.0.0.1:$port_nats" "$@" "$bin/envoy-listener"
  await_start listener "$listener_pid" 60 "the Envoy listener to answer /v1/sessions" \
    curl -fsS --max-time 5 -H "@$work/envoy-auth-header" "http://127.0.0.1:$port_listener/v1/sessions"
  listener_opened=$(date +%s)
}

# await_reaper_pass LIVE PERIOD waits until the listener start_listener last started has been open
# longer than LIVE seconds, its ENVOY_SESSION_LIVE_WINDOW, and has then logged a pass of its role
# claim reaper (`role claim reaper cycle`), which runs every PERIOD seconds, its
# ENVOY_ROLE_CLAIM_REAP_INTERVAL. A claim the listener restored at its start is kept until it has
# been open for that window, its restart grace, and a gone holder's claim is released only on a
# pass (ReapRoleClaims and releaseReadExpiredRoleClaim, packages/envoy/internal/store). It reads
# only this start's lines and judges each by the second the listener logged it in, in UTC
# (service_env's TZ=UTC): a line counts from the second after listener_opened + LIVE, which is past
# the window's end, since listener_opened is no earlier than the second the registry opened in. No
# line logged before that end can count, so it sleeps through the rest of the grace, then waits up
# to three periods; when the grace has ended already it does not sleep, and returns at once when
# such a pass is logged already. A wait that gives up is noted (soft_wait), and the caller goes on.
await_reaper_pass() {
  local live=$1 period=$2 grace left
  grace=$(date -u -d "@$((listener_opened + live))" +%FT%T)
  left=$((listener_opened + live - $(date +%s)))
  if ((left > 0)); then
    note "waiting ${left}s for the listener's restart grace to end at ${grace}Z"
    sleep "$left"
  fi
  soft_wait "$((3 * period))" "the listener's role claim reaper to pass after its restart grace ended at ${grace}Z" \
    sh -c "tail -c +$((listener_log_from + 1)) '$evidence/logs/listener.log' | grep -F '\"msg\":\"role claim reaper cycle\"' | jq -se 'any(.[]; .time[:19] > \"$grace\")'" ||
    true
}

start_dispatch() {
  local bin=$1 database=$2
  local -a web=()
  # The SPA only a run with A3's conversation-page leg builds; without it Dispatch serves the API
  # alone, which every other criterion reads.
  [ -z "$web_dist" ] || web=("DISPATCH_WEB_DIST=$web_dist")
  mkdir -p "$work/dispatch-home"
  chmod 0700 "$work/dispatch-home"
  [ -n "$dispatch_keeper_pid" ] || keep_port dispatch_keeper port_dispatch
  start_kept dispatch dispatch_keeper DISPATCH_LISTEN_FD -- "${service_env[@]}" "HOME=$work/dispatch-home" \
    "XDG_CONFIG_HOME=$work/dispatch-home/.config" "DATABASE_URL=postgres://postgres@127.0.0.1:$port_pg/$database?sslmode=disable" \
    "DISPATCH_AGENT_TOKEN=$(cat "$work/dispatch-token")" "ENVOY_TOKEN=$(cat "$work/envoy-token")" \
    DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_IDENTITY_HEADER_TRUSTED=1 DISPATCH_LISTEN_FD=3 DISPATCH_LISTEN_HOST=127.0.0.1 \
    "DISPATCH_PORT={port}" "DISPATCH_SERVER_URL=http://127.0.0.1:{port}" "${web[@]}" \
    "NATS_URLS=nats://127.0.0.1:$port_nats" "ENVOY_URL=http://127.0.0.1:$port_listener" "${dispatch_identity_dispatch_env[@]}" \
    "$bin/envoy-dispatch"
  dispatch_base="http://127.0.0.1:$port_dispatch"
  await_start dispatch "$dispatch_pid" 120 "the scratch Dispatch server" \
    curl -fsS --max-time 5 "$dispatch_base/api/v1"
}

stop_service() {
  local name=$1 pid_var="${1}_pid"
  stop_pid "${!pid_var}"
  printf -v "$pid_var" '%s' ""
  note "stopped $name"
}

# ---- the daemon ------------------------------------------------------------------------------------
# write_legion_config FILE ANSWERS [BOUNDS] [WEB_SEARCH] writes the daemon's legion.yaml for the
# current store: claims-only (no dispatch_url, no github_apps) unless both role Apps were given, the
# agents' reviewer on Claude Code, the merger's overlay (role_config_files, the GPT-6.1 agent's
# model and thinking level) where the model route serves GPT-6.1, the store's runtime, and with
# ANSWERS=yes the answers block: each provider the loop calls, anthropic and openai, at the model
# route's endpoint through the recording proxy (openai's under /v1, which agentloop's OpenAI client
# appends `responses` to), keyed by the route's own key file or key command, the prices,
# answers.web_search (the loop's web_search tool on web_search_model, at web_search_fee a search;
# WEB_SEARCH=no leaves it out, A23's absence leg), and BOUNDS (a YAML flow mapping) over the
# defaults. Every search the tool runs is posted to answers.providers.anthropic, so it reaches the
# recording proxy under the daemon's own label. ANSWERS=base is the base commit's daemon's file, in
# the same mode, on runtime tmux's files store (it has neither session_store nor answers, and runs
# no merger): an agent it starts works in the same workspace the branch daemon later resumes it in,
# as an upgrade leaves it.
write_legion_config() {
  local file=$1 answers=$2 bounds=${3:-} web_search=${4:-yes} provider
  {
    printf 'project: %s\nport: %s\npostgres_dsn: postgres://postgres@127.0.0.1:%s/%s?sslmode=disable\n' \
      "$project" "$port_daemon" "$port_pg" "$daemon_db"
    printf 'state_dir: %s\noperator_token_file: %s\n' "$state" "$work/operator-token"
    printf 'envoy_url: http://127.0.0.1:%s\nenvoy_token_file: %s\nnats_urls:\n  - nats://127.0.0.1:%s\n' \
      "$port_listener" "$work/envoy-token" "$port_nats"
    printf 'probe_interval_seconds: 5\nworker_backends:\n  reviewer: claude\n'
    if [ "$answers" != base ] && [ -f "$gpt_overlay" ]; then
      printf 'role_config_files:\n  merger:\n    - %s\n' "$gpt_overlay"
    fi
    if [ "$store" = postgres ] && [ "$answers" != base ]; then
      printf 'runtime:\n  tmux:\n    session_store: postgres\n    session_dsn_file: %s\n' "$work/sessions-dsn-$store"
    fi
    if [ "$mode" = workflow ]; then
      printf 'dispatch_url: http://127.0.0.1:%s\ndispatch_token_file: %s\nprojects:\n  %s: {}\ngates:\n  design: off\n' \
        "$port_dispatch" "$work/dispatch-token" "$project"
      cat "$work/github-apps.yaml"
    fi
    if [ "$answers" = yes ]; then
      printf 'answers:\n  providers:\n'
      for provider in anthropic openai; do
        printf '    %s:\n      base_url: http://127.0.0.1:%s/tag/daemon-%s%s\n' \
          "$provider" "$port_proxy" "$store" "$([ "$provider" = anthropic ] || printf /v1)"
        if [ -n "$model_key_file" ]; then
          printf '      api_key_file: %s\n' "$model_key_file"
        else
          printf '      api_key_command: %s\n' "$(printf '%s' "$model_key_command" | jq -Rs .)"
        fi
      done
      printf '  prices:\n%s\n' "$answers_prices"
      [ "$web_search" = no ] ||
        printf '  web_search:\n    model: %s\n    price_per_search: %s\n' "$web_search_model" "$web_search_fee"
      [ -z "$bounds" ] || printf '  bounds: %s\n' "$bounds"
    fi
  } >"$file"
}

# daemon_environment HOME PROFILE PLUGIN_DIR sets daemon_env, every variable a daemon of this run
# boots with (start_daemon), and `legion config check` reads its environment as boot does
# (config_check_tools): the run's HOME and profile, the run's tools first on PATH, the run's Oh My
# Pi (LEGION_OMP_PATH: the daemon takes omp_invocation only as `mise x <tool> -- omp`), the run's
# tmux directory, the Claude Code launch, bun, and the dispatch command the loop's dispatch tool
# runs (LEGION_DISPATCH_PATH, daemon_dispatch), where one is set. Of the host's own variables it
# passes the three a pane's jj and Oh My Pi need, each only when this shell has it: a pod has no
# mise and no MISE_DATA_DIR, and an empty one names the working directory.
daemon_environment() {
  local name
  daemon_env=("LEGION_E2E_RUN=$work" "HOME=$1" "OMP_PROFILE=$2" "PATH=$daemon_path" "TMPDIR=$work/tmp"
    "TMUX_TMPDIR=$work/tmux" "XDG_STATE_HOME=$work/xdg")
  for name in XDG_CONFIG_HOME MISE_DATA_DIR JJ_CONFIG; do
    [ -z "${!name:-}" ] || daemon_env+=("$name=${!name}")
  done
  daemon_env+=(LANG=C.UTF-8 TZ=UTC "USER=$(id -un)" "LOGNAME=$(id -un)" SHELL=/bin/bash TERM=xterm-256color
    "LEGION_OMP_PATH=$omp_bin" "LEGION_CLAUDE_PATH=$claude_bin" "LEGION_BUN_PATH=$work/realbin/bun"
    "LEGION_CLAUDE_PLUGIN_DIR=$3")
  [ -z "$daemon_dispatch" ] || daemon_env+=("LEGION_DISPATCH_PATH=$daemon_dispatch")
}

# start_daemon BINARY CONFIG LOG HOME PROFILE PLUGIN_DIR boots a daemon with every variable named
# (daemon_environment). The daemon serves on a copy of the socket the keeper daemon_keeper holds
# for the whole run (keep_port at bring-up), handed over as LEGION_LISTEN_FD, so every boot serves
# the port each legion.yaml names. This runs what lib/rig.sh's start_kept runs, held-port.py exec
# --from the keeper's socket, itself: start_kept logs to $evidence/logs/<NAME>.log, and each boot
# appends to LOG, which the checks read from this boot's offset. It waits for /healthz, each poll
# bounded (a kept port takes a connection before the daemon serves it), and records the log offset
# this boot starts at; a daemon that exits first fails the check with its log's last lines.
start_daemon() {
  daemon_bin=$1 daemon_config=$2 daemon_log=$3 daemon_home=$4 daemon_profile=$5 daemon_plugin_dir=$6
  local started=$SECONDS
  daemon_environment "$daemon_home" "$daemon_profile" "$daemon_plugin_dir"
  printf '=== boot at %s (%s)\n' "$(date -u +%FT%TZ)" "$daemon_config" >>"$daemon_log"
  boot_offset=$(stat -c %s "$daemon_log")
  # The daemon leads a process group of its own with its start recorded (lib/rig.sh's
  # start_grouped, start_process's recipe): stop_pid stops it with SIGSTOP first, and a stopped
  # member of this shell's own group would have the kernel hang up this shell too.
  start_grouped daemon_pid "$daemon_log" python3 "$root/scripts/e2e/lib/held-port.py" exec --from "$work/daemon_keeper.sock" --fd-env LEGION_LISTEN_FD -- \
    env -i "${daemon_env[@]}" LEGION_LISTEN_FD=3 "$daemon_bin" start --config "$daemon_config"
  note "waiting up to 300s for the daemon ($daemon_config) to answer /healthz"
  until curl -fsS --max-time 5 "http://127.0.0.1:$port_daemon/healthz" >/dev/null 2>&1; do
    [ "$(proc_start "$daemon_pid")" = "${started_at[$daemon_pid]:-}" ] ||
      fail "the daemon exited at boot; its log $daemon_log says: $(tail -c +$((boot_offset + 1)) "$daemon_log" | grep -v '^{' | tail -3 | tr '\n' ' ')"
    ((SECONDS - started < 300)) || fail "the daemon did not answer /healthz within 300 s; see $daemon_log"
    sleep 0.5
  done
}
# config_check_tools CONFIG prints the line the branch's `legion config check --config CONFIG`
# prints naming the answer loop's tools (check_tools_line), run under the environment the running
# daemon booted with (daemon_environment, as start_daemon last set it): the check reads the
# dispatch command and bun from its environment as boot does. A check that refused the file prints
# no such line, and its refusal goes to the notes.
config_check_tools() {
  local out
  out=$(env -i "${daemon_env[@]}" "$work/branch/legion" config check --config "$1" 2>&1) ||
    note "legion config check --config $1 refused it: $(tail -3 <<<"$out" | tr '\n' ' ')" >&2
  check_tools_line "$out"
}
stop_daemon() {
  [ -n "$daemon_pid" ] || return 0
  stop_pid "$daemon_pid"
  daemon_pid=
  [ -z "$tmux_bin" ] || kill_tmux_servers
}

legion_cmd() { "$daemon_bin" "$@" --config "$daemon_config"; }
claims_cmd() {
  local sub=$1
  shift
  "$daemon_bin" claims "$sub" --config "$daemon_config" --operator-token-file "$work/operator-token" "$@"
}
claim_json() { claims_cmd list --json | jq -ce --arg token "$1" '.claims[] | select(.token == $token)'; }
claim_is() { claim_json "$1" | jq -e "$2" >/dev/null; }
# gpt_bound_leg LEG COMMAND... runs COMMAND, a leg in which the loop answers the GPT-6.1 merger
# (A10's GPT-6.1 clause, A23's GPT-6.1 search leg), on a daemon whose answers.bounds sets
# answer_seconds to gpt_answer_seconds and nothing else, every other setting answers-<store>.yaml's,
# as A12 and A23's fourth search boot a file of their own: the daemon is stopped, gpt-<store>.yaml
# written and the daemon started on it, and once COMMAND returns it is stopped and started on
# answers-<store>.yaml again, each start_daemon setting daemon_config and boot_offset afresh. So each
# read COMMAND makes of this boot's log (boot_log) reads the boot that answered the leg, and the
# next row reads the daemon it always had. Neither the boot's lines nor `legion config check` name
# the bounds, so the note for LEG reads the bound back from the daemon's own state
# (settings.answers.bounds.answer_seconds of `legion state --json`), and gpt_bound_said keeps it for
# the leg's PASS. COMMAND runs as it would in this call's place, under errexit unless the call is a
# condition, and its status is the one returned.
gpt_bound_leg() {
  local leg=$1 file=$work/gpt-$store.yaml bounds="{ answer_seconds: $gpt_answer_seconds }" got status
  shift
  stop_daemon
  write_legion_config "$file" yes "$bounds"
  start_daemon "$work/branch/legion" "$file" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
  got=$(legion_cmd state --json 2>/dev/null | jq -r '.settings.answers.bounds.answer_seconds // "none"') || got=unread
  gpt_bound_said="the rig booted the daemon with answers.bounds.answer_seconds $gpt_answer_seconds for the GPT-6.1 merger's answers, where the product's default is 180, and the daemon's state read it back as ${got:-none}"
  note "$leg: the daemon answers the GPT-6.1 merger under a ${gpt_answer_seconds}-second answer bound, where the product's default is 180 s: booted on $file (answers.bounds $bounds), its own state reads settings.answers.bounds.answer_seconds ${got:-none}"
  "$@"
  status=$?
  stop_daemon
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
  return "$status"
}

# ---- Envoy -----------------------------------------------------------------------------------------
envoy() { curl -sS --max-time 30 -H "@$work/envoy-auth-header" -H 'content-type: application/json' "$@"; }
listener_url() { printf 'http://127.0.0.1:%s%s' "$port_listener" "$1"; }
# session_gone SESSION: the listener's session bucket no longer holds SESSION. A stopped pane does
# not unregister its session; the entry lapses with the bucket's 5-minute TTL, so a wait for it is
# session_ttl_wait long.
session_gone() { [ "$(envoy -o /dev/null -w '%{http_code}' "$(listener_url "/v1/registry/$1")")" = 404 ]; }
session_live() { [ "$(envoy -o /dev/null -w '%{http_code}' "$(listener_url "/v1/registry/$1")")" = 200 ]; }
session_ttl_wait=420
# more_than N COMMAND...: COMMAND prints a number above N.
more_than() {
  local n=$1
  shift
  [ "$("$@")" -gt "$n" ]
}
# register_asker registers the run's two stand-in sessions on the listener as every session
# registers now: each asks for a consumer of its own (`delivery.consumer`), since the listener
# refuses a registration it can give no consumer (`409 not_requested`) and writes nothing for it.
# Both carry one client instance for the run, which is also their consumers' epoch, as a plugin
# mints one per NATS connection: the inbox holds one connection for the whole run. The first is the
# asker, the sender of the run's questions: with it live in the listener's registry a send in its
# name resolves, the answer to that send is stored on its agent subject, and its consumer relays
# the answer to the inbox (start_inbox). The second stands in for the project's controller (the
# operator launches none under tmux). It holds both passes' controller roles,
# `legion-<project token>-controller`, because the listener stores a project's controller lane,
# `notifications.legion.<project token>.controller`, only for the session holding that role
# (bus.ReachesPrivateStreamSubject), and registers both lanes, so the notices the daemons publish
# there reach the inbox through its consumer. Every registration renews the roles its session
# holds; the roles are claimed, under the run's client instance, only after a registration whose
# answer left a lane out, and that registration is made again. Each registration's answer is kept
# in $work/registration-<session>.json, and register_asker fails unless both answers name consumer
# delivery on the session's relay subject with every topic it registered (consumer_registration).
asker_session=legion-e2eal-asker-$$
controller_session=legion-e2eal-controller-$$
holder_session=legion-e2eal-holder-$$
stand_in_instance=legion-e2eal-$$-$run_tag
controller_lanes=("notifications.legion.alf$run_tag.controller" "notifications.legion.alp$run_tag.controller")
# register_stand_in SESSION TITLE [TOPIC...] registers SESSION with a consumer of its own and every
# TOPIC, keeps the listener's answer, and fails unless that answer gives both.
register_stand_in() {
  local session=$1 title=$2 answer
  shift 2
  answer=$(envoy --data "$(jq -cn --arg s "$session" --arg dir "$work" --arg title "$title" --arg instance "$stand_in_instance" \
    '{session_id: $s, topics: $ARGS.positional, dir: $dir, title: $title, capabilities: ["steer"], driving: true,
      client_instance_id: $instance, delivery: {consumer: true, epoch: $instance}}' --args "$@")" \
    "$(listener_url /v1/interests/subscribe)") || answer=
  printf '%s\n' "$answer" >"$work/registration-$session.json"
  consumer_registration "$answer" "$session" "$@"
}
register_asker() {
  local lane lane_project
  register_stand_in "$asker_session" "answer-loop rig asker" || return 1
  register_stand_in "$controller_session" "answer-loop rig controller" "${controller_lanes[@]}" && return 0
  for lane in "${controller_lanes[@]}"; do
    lane_project=${lane#notifications.legion.}
    lane_project=${lane_project%.controller}
    envoy -f -o /dev/null --data "$(jq -cn --arg s "$controller_session" --arg role "legion-$lane_project-controller" --arg instance "$stand_in_instance" \
      '{session_id: $s, role: $role, soft: true, client_instance_id: $instance}')" "$(listener_url /v1/roles/set)" || return 1
  done
  register_stand_in "$controller_session" "answer-loop rig controller" "${controller_lanes[@]}"
}

# ---- the pass's Dispatch project -------------------------------------------------------------------
# map_repo PROJECT makes the smoke repository PROJECT's default repository in the Dispatch that runs
# now (PUT /api/v1/projects/<key>/default-repository, main's #4457, which removed the repository
# mapping): every rig issue names no repository of its own, so Dispatch resolves each to its
# project's default, and the daemon takes each issue's repository from Dispatch (main's #4493).
# Each project holds a default of its own, so the two passes' projects and the base daemon's each
# set theirs once, and nothing moves it after. The second owner's repository is no project's: A17
# reaches it through the implement App's installations, and its question names it.
map_repo() {
  dispatch_human PUT "projects/$1/default-repository" "$(jq -cn --arg repo "$repo" '{repo: $repo}')" >/dev/null
}

# ---- producing the stopped agents ------------------------------------------------------------------
# A Dispatch issue of the pass's project, as a person writes one (backlog, Dispatch's default): no
# `legion` label, so a workflow daemon admits nothing of it; its claims are the operator's (legion
# claims spawn).
new_rig_issue() {
  local title=$1 parent=${2:-} payload
  payload=$(jq -cn --arg project "$project" --arg title "$title" --arg parent "$parent" \
    '{project: $project, title: $title, force: true} + (if $parent == "" then {} else {parent: $parent} end)')
  dispatch_human POST issues "$payload" | jq -er .key
}

write_prompt() {
  printf 'You are the %s of the Legion issue %s in a proof run of Legion'"'"'s answer loop. Do exactly what each message asks, in one short reply. Run no tool unless a message asks you to.\n' \
    "$1" "$2" >"$work/prompt-$1-$2.md"
  printf '%s' "$work/prompt-$1-$2.md"
}

# spawn_agent NAME ROLE TREE ISSUE TASK spawns the claim through the daemon's operator route and
# records its token; with the workflow on, the issue's workspace is provisioned first.
spawn_agent() {
  local name=$1 role=$2 tree=$3 issue=$4 task=$5 token
  token=$(claim_token "$issue" "$role")
  provision_workspace "$issue"
  claims_cmd spawn --json --tree "$tree" --issue "$issue" --role "$role" --prompt-file "$(write_prompt "$role" "$issue")" \
    --task "$task" >/dev/null
  agent[$store:$name.token]=$token
  agent[$store:$name.issue]=$issue
  agent[$store:$name.role]=$role
  note "spawned $name: $role of $issue (tree $tree), claim $token"
}

# await_turn NAME waits for the agent's task to run and end, then records its session and file.
await_turn() {
  local name=$1 token json
  token=${agent[$store:$name.token]}
  until_true 900 "$name ($token) to finish its task" claim_is "$token" '.state == "idle" and .pending == null'
  json=$(claim_json "$token")
  agent[$store:$name.session]=$(jq -r .session <<<"$json")
  agent[$store:$name.file]=$(jq -r .sessionFile <<<"$json")
  note "$name: session ${agent[$store:$name.session]}, conversation ${agent[$store:$name.file]}"
}

# write_claude_route TOKEN LABEL [EFFORT] gives a Claude Code claim its model before the claim
# exists: settings.json in its claim directory names the model, the model route's key command (the
# one that prints the run's key copy under `key-file`, the gateway route's own under `gateway`),
# and ANTHROPIC_BASE_URL at the recording proxy. Claude Code sends what its apiKeyHelper prints as
# x-api-key; a gateway that reads `Authorization: Bearer` gets it from the proxy
# (lib/recording-proxy.ts, --api-key-as-bearer). EFFORT, when given, is the effort the claim's
# Claude Code runs at, as CLAUDE_CODE_EFFORT_LEVEL in the same file's `env`, which outranks every
# other effort source; without it Claude Code sends its own default, `medium` on the run's model.
write_claude_route() {
  local dir=$state/claude/$1 helper=$model_key_command
  if [ -n "$model_key_file" ]; then
    helper=$work/claude-key-command
    (umask 077 && printf '#!/bin/sh\nexec cat %s\n' "$model_key_file" >"$helper")
    chmod 0700 "$helper"
  fi
  (umask 077 && mkdir -p "$dir" &&
    jq -n --arg helper "$helper" --arg model "$(jq -r .model "$questions_file")" \
      --arg base "http://127.0.0.1:$port_proxy/tag/$2" --arg effort "${3:-}" \
      '{apiKeyHelper: $helper, model: $model,
        env: ({ANTHROPIC_BASE_URL: $base} + if $effort == "" then {} else {CLAUDE_CODE_EFFORT_LEVEL: $effort} end)}' >"$dir/settings.json")
}

# ---- the issues' workspaces ------------------------------------------------------------------------
# With the workflow on, an operator's spawn spec carries the repository Dispatch resolves the issue
# to, its project's default (the smoke repository, map_repo), and the tmux runtime then works in
# the issue's jj workspace at workspace.Location's <state>/workspaces/<owner>/<repo>/<issue
# lower-cased>, which must exist already (internal/runtime/tmux/spawn.go, workspaceDir): the
# workflow's outbox provisions it for an issue it admits, and it admits no rig issue (none has the
# `legion` label).
# provision_workspace ISSUE provisions it as an issue pod's two init containers do, with the branch
# daemon's own `legion workspace-init`: `fetch` fills the store pass's feed from GitHub once (no
# repository cache), under an installation token of the implement App for the smoke repository
# alone, `contents: read`, in a 0600 file removed as soon as the fetch ends; `provision` then makes
# the shared clone at <state>/repos/github.com/<owner>/<repo> and the issue's workspace on
# legion/<ISSUE> from main, its clone's credential helper the one a tmux daemon gives every clone it
# provisions and rewrites at boot (workspace.GitHubCredentialHelper, `!legion git-credential`).
# Claims-only, a spec has no repository and the runtime makes <state>/workspaces/<issue> itself.
# The fetch and the provisioning log to $evidence/logs/workspaces-<store>.log.
provision_workspace() {
  local issue=$1 feed=$work/feed-$store dir log=$evidence/logs/workspaces-$store.log out code
  [ "$mode" = workflow ] || return 0
  dir=$state/workspaces/$repo/${issue,,}
  [ ! -d "$dir" ] || return 0
  if [ ! -d "$feed/$repo.git" ]; then
    rm -f "$work/provision-token"
    mint_provision_token "$work/provision-token" ||
      fail "could not mint the implement App's installation token on $repo for the workspaces' fetch"
    code=0
    env -u LEGION_REPOSITORY_CACHE_URL -u LEGION_REPOSITORY_CACHE_TOKEN_FILE "PATH=$daemon_path" "TMPDIR=$work/tmp" \
      "LEGION_PROVISION_TOKEN_FILE=$work/provision-token" \
      "$work/branch/legion" workspace-init fetch --repo "$repo" --feed "$feed" >>"$log" 2>&1 || code=$?
    rm -f "$work/provision-token"
    [ "$code" = 0 ] || fail "legion workspace-init fetch --repo $repo exited $code; see $log"
    note "fetched $repo into the $store pass's feed $feed: $(grep '^workspace-init fetch:' "$log" | tail -1)"
  fi
  code=0
  out=$(env -u LEGION_PROVISION_TOKEN_FILE -u LEGION_REPOSITORY_CACHE_URL -u LEGION_REPOSITORY_CACHE_TOKEN_FILE \
    "PATH=$daemon_path" "TMPDIR=$work/tmp" \
    "$work/branch/legion" workspace-init provision --issue "$issue" --repo "$repo" --default-branch main --root "$state" \
    --credential-helper '!legion git-credential' --feed "$feed" 2>&1) || code=$?
  printf '%s\n' "$out" >>"$log"
  [ "$code" = 0 ] || fail "legion workspace-init provision --issue $issue exited $code: $(tail -1 <<<"$out")"
  # Its last line names the directory it provisioned, which must be the one spawn.go looks in.
  [ "$(tail -1 <<<"$out")" = "workspace-init: $dir on legion/$issue from main" ] ||
    fail "legion workspace-init provision --issue $issue provisioned \"$(tail -1 <<<"$out")\", not $dir"
  note "provisioned $issue's workspace $dir (legion/$issue from main)"
}

# mint_provision_token FILE writes the read-only installation token of github_apps.implement in
# $work/github-apps.yaml on the smoke repository's owner to the new 0600 FILE: minted from that
# App's own credential and narrowed as the daemon narrows one for a read
# (lib/app-installation.ts, installationFor), and the token is printed nowhere.
mint_provision_token() {
  cat >"$work/mint-provision-token.ts" <<'TS'
import { readFileSync, writeFileSync } from "node:fs";
const [lib, config, repo, out] = process.argv.slice(2) as [string, string, string, string];
const { appCredentialSources } = await import(`${lib}/app-logins.ts`);
const { installationFor } = await import(`${lib}/app-installation.ts`);
const source = appCredentialSources(readFileSync(config, "utf8"), ["implement"]).implement;
if (source === undefined) throw new Error("github_apps.implement is not configured");
const owner = repo.split("/")[0] as string;
const installation = await installationFor("implement", source, owner, true);
if (installation === undefined) {
  throw new Error(`the implement App's ${source.kind} named no installation for ${owner}`);
}
writeFileSync(out, installation.token, { mode: 0o600, flag: "wx" });
TS
  (umask 077 && bun "$work/mint-provision-token.ts" "$root/scripts/e2e/lib" "$work/github-apps.yaml" "$repo" "$1")
}

# ---- the record a check seeds ----------------------------------------------------------------------
# A6's and A19's trees are a person's Dispatch issues (new_rig_issue: no `legion` label), so the
# workflow records nothing of them, and each check writes the record it needs itself, with the
# daemon stopped. That record is in two tables, and both are needed: the issue rows, and the
# tree's lifecycle.
#
# record_tree_issue KEY TREE PARENT PHASE TITLE writes one issue row as admission records a member
# of a tree it admits (admit's promoteHolds): handed to Legion, in_progress at PHASE, generation
# one, at the Dispatch event sequence the issue has reached (so the boot listing changes nothing)
# and on the smoke repository. PARENT is empty for the tree's own root. The daemon must be stopped.
record_tree_issue() {
  local key=$1 tree=$2 parent=$3 phase=$4 title=$5 seq
  seq=$(dispatch_events "$key" | jq '[.[].seq] | max // 0')
  pg_sql "$daemon_db" "insert into issues (key, tree, project, title, parent, phase, generation, status, rank, handed_over, last_dispatch_seq, dispatch_status, repo, default_branch, github_repo)
    values ('$key', '$tree', '$project', '$title', $([ -z "$parent" ] && printf null || printf "'%s'" "$parent"), '$phase', 1, 'in_progress', 'a', true, $seq, 'in_progress', '$repo', 'main', '$repo')" >/dev/null
}

# open_tree_lifecycle ROOT opens ROOT's tree lifecycle under the workflow's own authority, the
# other half of that record and the half that makes the daemon keep the tree: a tree is live only
# while its lifecycle stands open under the workflow (record.TreeLive over the tree_lifecycles row,
# treelifecycle.Lifecycle.OpenUnderWorkflow), and the next boot's admission pass takes a root at a
# running status whose tree is not live out of the workflow altogether, phase `done` and the tree
# lingering (admit's releaseUnslotted, logged `released a root no tree of which runs and nothing
# would ever start`), which leaves the check proving nothing. The row is keyed by the project
# token the claims are (claim.ProjectToken, as claim_token spells it), at epoch one, which is the
# epoch admission's first open of a tree writes.
#
# An operator's spawn of the root's architect opens that row first, under the operator's authority
# (internal/api's spawn, OpenOperatorTreeLifecycle), and a tree stands open under one authority
# only, so this takes the row over at the epoch the architect's claim is already bound to rather
# than writing a second. That is also why every such spawn comes before the seeding: an operator's
# spawn on a tree the workflow holds is refused (treelifecycle.ErrAuthorityHeld). The daemon must
# be stopped.
open_tree_lifecycle() {
  pg_sql "$daemon_db" "insert into tree_lifecycles (project, tree, epoch, authority) values ('${project,,}', '$1', 1, 'workflow')
    on conflict (project, tree) do update set authority = 'workflow', cleanup_started = false, cleanup_confirmed_at = null, updated_at = now()" >/dev/null
}

# produce_pre_change runs the base commit's daemon on the pass's database and state directory, as the
# release before this change: one Claude Code reviewer, on a child of a tree of its own, has its
# conversation, then is suspended and the base daemon stopped. The tree's architect is spawned with
# no task first, since an operator's spawn opens a tree's lifecycle only for its root architect, and
# is suspended with it. The branch daemon then boots on the same database and directory, as an
# upgrade does, and finds the conversation only by its title (A2's conversation from before the change).
# The base daemon runs in the run's mode, so with the workflow on both issues' workspaces are
# provisioned first (provision_workspace): the base daemon starts the reviewer there, and the branch
# daemon resumes it there (A10).
#
# The reviewer is made to think so that its transcript holds thinking for A10 to replay byte for
# byte. Its task was once one short restatement, which at Claude Code's default on the run's model
# (`medium`) it answered with no thinking at all in proof run 43996451813. On that model thinking
# is adaptive and cannot be given a budget (the API refuses `thinking.type: enabled`, so
# MAX_THINKING_TOKENS changes nothing): effort is what makes it think, the more readily the higher
# it is, and the task is what gives it something to think about. So it runs at effort `xhigh`, and
# its task (questions.json, agents.pre_change) has it weigh, in one sentence and with no tool,
# whether the missing SBOM alone blocks the release before it restates the blocker and its marker,
# the two facts pre-change-scan reads. `xhigh` and not `max`, since the loop answers this reviewer
# at the effort its transcript last ran at (A2, A10, A15, A17) within answers.bounds.output_tokens,
# thinking included. Neither guarantees a thinking block, so the note below says what the
# transcript holds, and A10 fails a transcript with no signature.
produce_pre_change() {
  local token architect tree=${agent[$store:pre_change.tree]} transcript
  write_legion_config "$work/base-$store.yaml" base
  token=$(claim_token "${agent[$store:pre_change.issue]}" reviewer)
  architect=$(claim_token "$tree" architect)
  write_claude_route "$token" "claude-$store" xhigh
  start_daemon "$work/base/legion" "$work/base-$store.yaml" "$evidence/logs/base-daemon-$store.log" "$base_home" "$base_profile" \
    "$base_plugin"
  provision_workspace "$tree"
  claims_cmd spawn --json --tree "$tree" --issue "$tree" --role architect --prompt-file "$(write_prompt architect "$tree")" >/dev/null
  spawn_agent pre_change reviewer "$tree" "${agent[$store:pre_change.issue]}" "$(jq -r .agents.pre_change.task "$questions_file")"
  await_turn pre_change
  claims_cmd suspend --claim "$token" >/dev/null
  until_true 120 "the pre-change reviewer to be suspended" claim_is "$token" '.state == "suspended"'
  claims_cmd suspend --claim "$architect" >/dev/null
  until_true 120 "the pre-change tree's architect to be suspended" claim_is "$architect" '.state == "suspended"'
  stop_daemon
  note "the base daemon ran the Claude Code reviewer ${agent[$store:pre_change.session]} and stopped"
  transcript=$(conversation_text pre_change 2>/dev/null) || transcript=
  note "the pre-change reviewer's transcript: effort \"$(transcript_effort "$transcript")\", $(transcript_thinking "$transcript") thinking blocks, $(grep -c . <<<"$(transcript_signatures "$transcript")" || true) thinking signatures (A10 replays each byte for byte)"
}

# ask_instruction ISSUE WHAT prints the sentence that ends a task whose agent opens one Dispatch
# ask on ISSUE, asking a person whether WHAT, and waits (produce_agents, check_a6). It names the
# command and its flags, as the GPT-6.1 merger's task names `pwd` (questions.json): told only to
# open one Dispatch ask, the parked child's tester of proof run 45573435593 ran `dispatch ask
# --issue … --body '…'`, which dispatch refused (`unknown flag --body`; the question is
# `--question`), and, allowed one tool call, made no other, so it opened no ask and neither A6's
# second leg nor its control had an ask to reply on.
ask_instruction() {
  printf '%s\n' "Then make exactly one tool call: run \`dispatch ask --issue $1 --question '<your question>'\` with your bash tool, asking a person whether $2, and wait."
}

# produce_agents runs the branch daemon's own agents and stops each the way its kind is made:
#   architect, the tree's root: suspended, as a person parking the tree leaves it;
#   planner, on a child: stopped (retired), as a closed child's claim ends;
#   tester, on a child: suspended, its claim's session forgotten as the workflow's issue close
#   forgets it — through the daemon's own operator route (`legion claims forget-session`, which
#   takes the session being abandoned so a newly registered one cannot be cleared by mistake) —
#   resumed with a new session, given its second task, and suspended again: the first session is
#   the one the second replaced;
#   the GPT-6.1 merger, on a child, where the model route serves GPT-6.1 (gpt_preflight): suspended,
#   as the pre-change reviewer is, since A10 answers it and resumes it as it does that reviewer
#   (spawn_gpt_agent, produce_gpt_agent).
produce_agents() {
  local tester old task replacement
  task=$(jq -r .agents.architect.task "$questions_file")
  replacement=$(jq -r .agents.tester.replacement_task "$questions_file")
  # With the workflow on, a pane has Dispatch: the architect also opens the ask A1 replies on, and
  # the replacing tester the ask A6's first leg replies on.
  if [ "$mode" = workflow ]; then
    task+=" $(ask_instruction "${agent[$store:root]}" "to keep the 7-minute TTL")"
    replacement+=" $(ask_instruction "${agent[$store:tester.issue]}" "to keep the 20-run rerun as the release gate")"
  fi
  spawn_agent architect architect "${agent[$store:root]}" "${agent[$store:root]}" "$task"
  spawn_agent planner planner "${agent[$store:root]}" "${agent[$store:planner.issue]}" "$(jq -r .agents.planner.task "$questions_file")"
  spawn_agent tester tester "${agent[$store:root]}" "${agent[$store:tester.issue]}" "$(jq -r .agents.tester.task "$questions_file")"
  [ -z "${agent[$store:gpt.issue]:-}" ] || spawn_gpt_agent
  await_turn architect
  await_turn planner
  await_turn tester
  claims_cmd suspend --claim "${agent[$store:architect.token]}" >/dev/null
  claims_cmd stop --claim "${agent[$store:planner.token]}" >/dev/null
  claims_cmd suspend --claim "${agent[$store:tester.token]}" >/dev/null
  until_true 120 "the architect to be suspended" claim_is "${agent[$store:architect.token]}" '.state == "suspended"'
  until_true 120 "the planner to be retired" claim_is "${agent[$store:planner.token]}" '.state == "retired"'
  until_true 120 "the tester to be suspended" claim_is "${agent[$store:tester.token]}" '.state == "suspended"'
  [ -z "${agent[$store:gpt.issue]:-}" ] || [ -n "${agent[$store:gpt.why]:-}" ] || produce_gpt_agent
  tester=${agent[$store:tester.token]}
  old=${agent[$store:tester.session]}
  agent[$store:tester_old.session]=$old
  agent[$store:tester_old.issue]=${agent[$store:tester.issue]}
  agent[$store:tester_old.file]=${agent[$store:tester.file]}
  claims_cmd forget-session --claim "$tester" --session "$old" >/dev/null
  until_true 120 "the tester's claim to forget session $old" claim_is "$tester" '.session == ""'
  note "the tester's claim forgets session $old (legion claims forget-session)"
  claims_cmd resume --claim "$tester" >/dev/null
  until_true 600 "the tester to register a new session" claim_is "$tester" \
    "(.state | IN(\"ready\", \"idle\")) and .session != \"\" and .session != \"$old\""
  deliver_task "$tester" "$replacement"
  await_turn tester
  claims_cmd suspend --claim "$tester" >/dev/null
  until_true 120 "the replacing tester to be suspended" claim_is "$tester" '.state == "suspended"'
  local name
  for name in architect planner tester_old tester pre_change; do
    until_true "$session_ttl_wait" "$name's session to leave the listener's registry" session_gone "${agent[$store:$name.session]}"
  done
  [ -z "${agent[$store:gpt.session]:-}" ] ||
    soft_wait "$session_ttl_wait" "the GPT-6.1 merger's session to leave the listener's registry" session_gone "${agent[$store:gpt.session]}" ||
    agent[$store:gpt.why]+="${agent[$store:gpt.why]:+; }its session ${agent[$store:gpt.session]} stayed in the listener's registry"
  note "stopped: architect ${agent[$store:architect.session]} (parked), planner ${agent[$store:planner.session]} (closed child), tester $old replaced by ${agent[$store:tester.session]}, pre-change reviewer ${agent[$store:pre_change.session]}${agent[$store:gpt.session]:+, GPT-6.1 merger ${agent[$store:gpt.session]}}"
}

# spawn_gpt_agent spawns the GPT-6.1 merger as spawn_agent spawns an agent, but softly: its
# workspace and its claim are made in a command substitution, where a refusal ends that alone, and
# a merger that could not be spawned fails A10's GPT-6.1 clause, naming why
# (agent[<store>:gpt.why]), and no other criterion.
spawn_gpt_agent() {
  local issue=${agent[$store:gpt.issue]} out
  agent[$store:gpt.token]=$(claim_token "$issue" merger)
  agent[$store:gpt.role]=merger
  if out=$({ provision_workspace "$issue" && claims_cmd spawn --json --tree "${agent[$store:root]}" --issue "$issue" --role merger \
    --prompt-file "$(write_prompt merger "$issue")" --task "$(jq -r .agents.gpt.task "$questions_file")" >/dev/null; } 2>&1); then
    [ -z "$out" ] || printf '%s\n' "$out"
    note "spawned gpt: merger of $issue (tree ${agent[$store:root]}), claim ${agent[$store:gpt.token]}"
  else
    agent[$store:gpt.why]="the GPT-6.1 merger could not be spawned: $(grep -v '^[[:space:]]*$' <<<"$out" | tail -1)"
    note "${agent[$store:gpt.why]}"
  fi
}

# produce_gpt_agent waits out the GPT-6.1 merger's task and suspends it. The wait is soft: a route
# that serves the preflight's one call and then fails the agent's turn fails A10's GPT-6.1 clause,
# naming why (agent[<store>:gpt.why]), and no other criterion. The claim is suspended whatever its
# turn did, and retired where it will not suspend, so A1's parked tree holds no running claim of
# it. The note says what the transcript holds: the thinking level it last ran at, the provider, API
# and model of its turns, its tool calls and its reasoning items, all of them and those with
# encrypted content, which A10 replays byte for byte.
produce_gpt_agent() {
  local token=${agent[$store:gpt.token]} json transcript
  soft_wait 900 "the GPT-6.1 merger ($token) to finish its task" claim_is "$token" '.state == "idle" and .pending == null' ||
    agent[$store:gpt.why]="the GPT-6.1 merger did not finish its task within 900 s: $(claim_json "$token" | jq -c '{state, pending, session}' || true)"
  json=$(claim_json "$token") || json='{}'
  agent[$store:gpt.session]=$(jq -r '.session // ""' <<<"$json")
  agent[$store:gpt.file]=$(jq -r '.sessionFile // ""' <<<"$json")
  claims_cmd suspend --claim "$token" >/dev/null || true
  if ! soft_wait 120 "the GPT-6.1 merger to be suspended" claim_is "$token" '.state == "suspended"'; then
    agent[$store:gpt.why]+="${agent[$store:gpt.why]:+; }its claim was not suspended"
    claims_cmd stop --claim "$token" >/dev/null || true
    soft_wait 120 "the GPT-6.1 merger to be retired" claim_is "$token" '.state == "retired"' ||
      agent[$store:gpt.why]+=", nor retired: $(claim_json "$token" | jq -c '{state, session}' || true)"
  fi
  transcript=$(conversation_text gpt 2>/dev/null) || transcript=
  note "the GPT-6.1 merger ${agent[$store:gpt.session]:-(no session)}: thinking level \"$(omp_thinking_level "$transcript")\" (its last thinking_level_change); its turns $(omp_openai_summary "$transcript") (A10 replays each reasoning item with encrypted content byte for byte)"
}

save_conversations() {
  local name
  mkdir -p "$evidence/sessions/$store"
  for name in architect planner tester_old tester pre_change ${agent[$store:gpt.file]:+gpt}; do
    conversation_text "$name" >"$evidence/sessions/$store/$name.jsonl" 2>/dev/null || true
  done
}

# ---- prerequisites ---------------------------------------------------------------------------------
begin prerequisites
for tool in go bun jq curl python3 dpkg-deb tar flock pgrep git jj gh sha256sum; do
  command -v "$tool" >/dev/null || fail "$tool is required"
done
# The Bun on PATH must be the release .bun-version pins, the file
# packages/claude-envoy/scripts/build.ts reads: the run builds each daemon's claude-envoy with that
# script, and the build refuses any other Bun, since its bundler output depends on the exact build
# (LEGION-568). Checked here, before the first bun this run starts.
bun_pin=$(tr -d '[:space:]' <"$root/.bun-version")
bun_running=$(bun --version 2>/dev/null | tr -d '[:space:]') || bun_running=
[ "$bun_running" = "$bun_pin" ] ||
  fail "refusing to run under Bun ${bun_running:-<no version>}: .bun-version pins $bun_pin, and the run builds each daemon's claude-envoy with packages/claude-envoy/scripts/build.ts (LEGION-568). Put the pinned Bun first on PATH ($(command -v bun) is the one found)"
# The model route, as Stage 2's and Stage 3's proofs take it: where every model call of the run
# goes and what keys it. Under `key-file` one 0600 copy of the key, made here and removed on exit,
# serves the agents' key command, Claude Code's apiKeyHelper and the daemon's answers block; under
# `gateway` one key command serves all three and the gateway is the endpoint — hawk-token's, or
# the run's own helper when LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND names the command that prints
# the gateway's bearer.
model_route=${LEGION_E2E_MODEL_ROUTE:-key-file}
case "$model_route" in
key-file | gateway) ;;
*) fail "LEGION_E2E_MODEL_ROUTE must be key-file or gateway, not '$model_route'" ;;
esac
if [ "$model_route" = gateway ]; then
  model_upstream=$(bash "$root/scripts/e2e/lib/model-gateway-url.sh") ||
    fail "LEGION_E2E_MODEL_GATEWAY_URL is not a model gateway endpoint (the reason is above)"
  if [ -n "${LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND:-}" ]; then
    # The caller's own shell text, in a 0700 script of the work directory (never the evidence, and
    # removed with the work directory on exit) that the run's three key commands call. The daemon
    # runs a key command under `env -i` and a pane's HOME is the run's own, so the script carries
    # this shell's values for the variables such a command reads; anything else it needs it names
    # itself. It keeps nothing: the token expires long before a run of this length ends, so every
    # model call runs the text again.
    gateway_token_helper=$work/model-gateway-token
    (umask 077 && {
      printf '#!/bin/sh\n'
      printf '# Written by scripts/e2e/answer-loop.sh: LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND, run\n'
      printf '# again for every model call of the run. Stdout is the gateway bearer.\n'
      for var in PATH HOME XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME \
        XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS; do
        [ -z "${!var:-}" ] || printf 'export %s=%q\n' "$var" "${!var}"
      done
      printf '%s\n' "$LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND"
    } >"$gateway_token_helper")
    chmod 0700 "$gateway_token_helper"
    # One call before anything is created, as the key-file route's own preflight read is: the
    # command runs, and what it prints is a bearer an Authorization header can carry. Its value is
    # checked and dropped, never printed, and its stderr is quoted only on a failure.
    token_status=0
    token=$("$gateway_token_helper" 2>"$work/model-gateway-token.err") || token_status=$?
    token_reason=$(tr -d '\r' <"$work/model-gateway-token.err" | grep -v '^[[:space:]]*$' | tail -1) || token_reason=
    rm -f "$work/model-gateway-token.err"
    [ "$token_status" = 0 ] ||
      fail "LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND exited $token_status without a token${token_reason:+ ($token_reason)}"
    token=${token//[$' \t\r\n']/}
    [ -n "$token" ] ||
      fail "LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND printed no token${token_reason:+ ($token_reason)}"
    case $token in
    *[!!-~]*) fail "LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND printed something other than one word an Authorization header can carry" ;;
    esac
    unset token
    note "model route: gateway, every call through the recording proxy to $model_upstream, keyed by LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND through $gateway_token_helper (0700, run again for every call, printed nowhere), which the proxy sends on as Authorization: Bearer"
  else
    command -v hawk-token >/dev/null ||
      fail "hawk-token is required under LEGION_E2E_MODEL_ROUTE=gateway, unless LEGION_E2E_MODEL_GATEWAY_TOKEN_COMMAND names the command that prints the gateway's bearer"
    note "model route: gateway, every call through the recording proxy to $model_upstream, keyed by hawk-token"
  fi
else
  model_upstream=${LEGION_E2E_MODEL_BASE_URL:-${ANTHROPIC_BASE_URL:-$anthropic_base_url}}
  why=$(model_base_url_refusal "$model_upstream")
  [ -z "$why" ] ||
    fail "the model endpoint $why: name it in LEGION_E2E_MODEL_BASE_URL, or leave it and ANTHROPIC_BASE_URL unset for Anthropic's own"
  model_key_file=$work/anthropic-key
  key_source=${LEGION_E2E_ANTHROPIC_KEY_FILE:-${LEGION_E2E_CLAUDE_KEY_FILE:-}}
  if [ -n "$key_source" ]; then
    [ -f "$key_source" ] && [ -r "$key_source" ] ||
      fail "the key file $key_source is not a readable file"
    (umask 077 && tr -d '[:space:]' <"$key_source" >"$model_key_file")
  elif [ -n "${ANTHROPIC_API_KEY:-}" ]; then
    key_source='ANTHROPIC_API_KEY'
    (umask 077 && printf '%s' "$ANTHROPIC_API_KEY" | tr -d '[:space:]' >"$model_key_file")
  else
    fail "LEGION_E2E_ANTHROPIC_KEY_FILE, LEGION_E2E_CLAUDE_KEY_FILE and ANTHROPIC_API_KEY are all unset: under LEGION_E2E_MODEL_ROUTE=key-file one of them holds the key every model call of the run carries"
  fi
  unset ANTHROPIC_API_KEY
  [ -s "$model_key_file" ] || fail "$key_source holds no key"
  # The endpoint takes the key, and refuses a call without one, so its 200 says something about
  # this key (lib/model-endpoint.sh); the key reaches curl by file and is printed nowhere.
  model_key_preflight "${LEGION_E2E_MODEL_BASE_URL:-${ANTHROPIC_BASE_URL:-}}" "$model_key_file" "$work/anthropic-key-header" "$key_source"
  note "model route: key-file, every call through the recording proxy to $model_upstream, keyed by the copy of $key_source in $model_key_file (0600) and nowhere else"
fi
# The questions and their answer keys, read before any model call.
jq -e '.agents.architect.task and .agents.planner.task and .agents.tester.task and .agents.tester.replacement_task and .agents.pre_change.task and .agents.gpt.task and .agents.gpt.model and .agents.gpt.thinking and (.questions | length > 0)' \
  "$questions_file" >/dev/null || fail "$questions_file is not the questions file this run reads"
note "questions: $questions_file, sha256 $(sha256sum "$questions_file" | cut -d' ' -f1), $(jq '.questions | length' "$questions_file") questions"
# A10's GPT-6.1 agent: an OpenAI model the daemon's answers block prices, since the loop answers a
# conversation only on a model answers.prices names.
gpt_model=$(jq -r .agents.gpt.model "$questions_file")
gpt_model_id=${gpt_model#openai/}
gpt_level=$(jq -r .agents.gpt.thinking "$questions_file")
[[ $gpt_model == openai/?* ]] && [[ $answers_prices == *" $gpt_model: "* ]] ||
  fail "$questions_file's GPT agent runs on $gpt_model, which is no OpenAI model the answers block prices"
# The role Apps. Each App's credential is a key command of the operator's,
# LEGION_E2E_<ROLE>_APP_KEY_COMMAND (lib/github-apps.sh): the run runs each once, here, on this
# host, in the operator's own environment, into $work/app-key/<role>/key.pem (0600, in a directory
# of mode 0700 that cleanup removes), and every token of the run is minted from that file — the
# daemon's, through the private_key_command that `cat`s it, and the rig's own, which
# lib/app-logins.ts, lib/app-installation.ts and lib/smoke-pull-request.ts mint from the same key.
# Nothing here asks the secrets broker for anything, so the run needs no agent-secrets on PATH and
# files no request in anyone's inbox.
#
# The workflow boots only with two distinct Apps, and the answer loop is part of the workflow, so
# a run given both key commands is in workflow mode and a run missing either runs claims-only,
# where the daemon has no dispatch_url and no github_apps and every answering criterion records
# WAITING naming the input it waits on (lib/answer-loop-reads.sh, answering_blockers). With the
# implement key command alone the run still proves that App against GitHub and on the smoke
# repository, as the GitHub-writing criteria need; with neither, it proves no App and reads
# nothing of GitHub, which claims-only writes nothing to anyway.
[ -n "${LEGION_E2E_IMPLEMENT_APP_KEY_COMMAND:-}" ] || app_keys_missing+=(LEGION_E2E_IMPLEMENT_APP_KEY_COMMAND)
[ -n "${LEGION_E2E_REVIEW_APP_KEY_COMMAND:-}" ] || app_keys_missing+=(LEGION_E2E_REVIEW_APP_KEY_COMMAND)
roles=
if [ "${#app_keys_missing[@]}" = 0 ]; then
  require_app_inputs implement review
  write_github_apps --key-dir "$work/app-key" implement review
  read_app_logins
  roles=implement,review
  mode=workflow
elif [ -n "${LEGION_E2E_IMPLEMENT_APP_KEY_COMMAND:-}" ]; then
  require_app_inputs implement
  write_github_apps --key-dir "$work/app-key" implement
  check_github_apps implement
  implement_app_login=$(jq -er .implement <<<"$github_app_logins") ||
    fail "lib/app-logins.ts printed no implement App login: $github_app_logins"
  roles=implement
fi
if [ -n "$roles" ]; then
  installed=$(bun "$root/scripts/e2e/lib/app-installation.ts" --roles "$roles" "$work/github-apps.yaml" "$repo") ||
    fail "lib/app-installation.ts could not read the Apps' installations on $repo"
  note "installations on $repo (GET /installation/repositories under each App's own installation token): $installed"
  jq -e '.implement.installed == true' <<<"$installed" >/dev/null ||
    fail "the implement App ($implement_app_id) is not installed on $repo: the GitHub checks cannot run there"
  [ "$mode" != workflow ] || jq -e '.review.installed == true' <<<"$installed" >/dev/null ||
    fail "the review App ($review_app_id) is not installed on $repo"
  if [ -n "$second_repo" ]; then
    [[ "$second_repo" =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$ ]] || fail "LEGION_E2E_REPO_SECOND_OWNER must be owner/name"
    second_installed=$(bun "$root/scripts/e2e/lib/app-installation.ts" --roles implement "$work/github-apps.yaml" "$second_repo") ||
      fail "lib/app-installation.ts could not read the implement App's installation on the second owner's repository"
    note "the second owner's repository: $second_installed"
  fi
fi
note "mode: $mode$([ "$mode" = workflow ] || printf ' (%s is unset: the daemon runs without dispatch_url and github_apps, so its workflow, and the answer loop with it, is off)' "$(app_keys_missing_names)")"
pass

begin provision
bringup_started=$(date +%s)
provision_postgres
provision_tmux
provision_nats
provision_claude
resolve_omp
real_tools
install_leak_check
if browser_leg; then
  provision_browser
else
  note "no browser: A3's conversation-page leg does not run in this run"
fi
mkdir -p "$work/tmp" "$work/tmux" "$work/xdg" "$work/listener-home"
chmod 0700 "$work/tmux" "$work/xdg"
pass

begin build
build_branch
build_base
! browser_leg || build_web
pass

begin rig
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/envoy-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/dispatch-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/dispatch-token")" >"$work/dispatch-auth-header" &&
  printf 'X-Dispatch-User: %s\n' "$proof_human_user" >"$work/dispatch-human-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/operator-token" &&
  head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/github-webhook-secret")
start_postgres
for db in dispatch dispatch_base "legion_files_$run_tag" "legion_postgres_$run_tag" "legion_base_$run_tag" "omp_sessions_$run_tag"; do
  pg_sql postgres "create database $db" >/dev/null
done
(umask 077 && printf 'postgres://postgres@127.0.0.1:%s/omp_sessions_%s?sslmode=disable\n' "$port_pg" "$run_tag" >"$work/sessions-dsn-postgres")
note "Postgres $pg_version (pid $pg_pid, port $port_pg) with pgvector $(pg_sql dispatch 'create extension if not exists vector' >/dev/null && pg_sql dispatch "select extversion from pg_extension where extname = 'vector'" | jq -r '.[0].extversion')"
start_nats
start_proxy
start_inbox
# The issuer outlives every restart of the listener and Dispatch, A13's base binaries included: its
# key lives in its memory alone, so each listener verifies Dispatch's token only while it runs.
dispatch_identity_start
start_listener "$work/branch"
start_dispatch "$work/branch" dispatch
# Every daemon of the run, the base commit's and the branch's in both passes, serves this one kept
# port (start_daemon), which each legion.yaml names before its daemon boots.
keep_port daemon_keeper port_daemon
note "NATS $port_nats (pid $nats_pid), proxy $port_proxy (pid $proxy_pid), inbox (pid $inbox_pid), listener $port_listener (pid $listener_pid), Dispatch $port_dispatch (pid $dispatch_pid), the daemons' kept port $port_daemon"
# The stand-ins are registered once here, and a registration the listener refuses, or answers with
# no consumer, ends the run naming the listener's answers: every answering criterion reads what the
# asker is delivered. Their sessions lapse with the session bucket's TTL like any other, so a loop
# renews them every minute, and an answer that comes late still finds the asker live.
register_asker ||
  fail "the listener gave a stand-in no consumer of its own: asker $(cat "$work/registration-$asker_session.json" 2>/dev/null); controller $(cat "$work/registration-$controller_session.json" 2>/dev/null)"
note "the stand-ins registered with their own consumers: $(jq -c '{session_id, delivery, topics}' "$work/registration-$asker_session.json" "$work/registration-$controller_session.json" | tr '\n' ' ')"
# asker_heartbeat renews the stand-ins' registration every minute while the driver lives. It
# starts in a process group of its own, as start_daemon's daemon does (lib/rig.sh's
# start_grouped), since cleanup's stop_tree stops it with SIGSTOP first; out of the driver's group
# a signal to that group no longer reaches it, so it ends of itself once the driver has gone
# (lib/process-tree.sh's run_marks).
asker_heartbeat() {
  while kill -0 "$$" 2>/dev/null; do
    register_asker >/dev/null 2>&1 || true
    sleep 60
  done
}
start_grouped asker_heartbeat_pid "$evidence/logs/asker-heartbeat.log" asker_heartbeat
# The base daemon's Oh My Pi home: the base commit's plugins (its daemon refuses another contract).
base_home=$work/base-omp-home
base_profile=legion-e2eal-base-$$
make_omp_home "$base_home"
install_plugins "$base_src" "$base_home" "$base_profile" "$work/base-plugins"
install_model_route "$base_home" "$base_profile" agents-base
branch_home=$work/omp-home
branch_profile=legion-e2eal-$$
make_omp_home "$branch_home"
install_plugins "$root" "$branch_home" "$branch_profile" "$work/branch-plugins"
# The dispatch command the branch daemons' answer loop runs as its dispatch tool (A22): the branch
# pi-envoy pack's own shim, which execs bun on the CLI the pack bundles, as the worker image's
# /opt/legion/pi-envoy/bin/dispatch does. The daemon finds no other: its PATH holds none, and a
# runner has no worker image's install.
daemon_dispatch=$work/branch-plugins/pi-envoy/bin/dispatch
[ -x "$daemon_dispatch" ] && [ -f "$work/branch-plugins/pi-envoy/dist/dispatch.js" ] ||
  fail "the branch's pi-envoy pack holds no runnable bin/dispatch with its dist/dispatch.js under $work/branch-plugins/pi-envoy"
note "the answer loop's dispatch tool runs $daemon_dispatch (LEGION_DISPATCH_PATH)"
install_model_route "$branch_home" "$branch_profile" agents
# Whether the route serves GPT-6.1, asked once the proxy runs and the route's key command exists:
# only then do the branch's profile and the merger's overlay get the model, and A10's GPT-6.1 agent
# is made; otherwise its clause records WAITING on what the route answered.
gpt_preflight
[ -n "$gpt_refusal" ] || install_gpt_route "$branch_home" "$branch_profile" agents-gpt
note "bring-up took $(($(date +%s) - bringup_started)) s"
pass

# ---- the criteria ----------------------------------------------------------------------------------
# criterion ID DESCRIPTION PREREQ... records WAITING for each blocker that applies (and returns 1),
# or returns 0 so the caller runs the check, and then counts ID among the rows that ran in the
# pass (rows_ran), each of which has a control row in A13's phase (run_controls). A PREREQ names a
# prerequisite beyond the answering ones, each read from what the run has: infra448 (the listener's
# registry routes in this checkout, A3, A20 and A21), openai (a model route that serves the GPT
# model, as gpt_preflight asked it, A10's and A23's GPT-6.1 clauses), second-owner
# (LEGION_E2E_REPO_SECOND_OWNER). A row it lets run begins here (answers_begin): it ends the row
# before it, and notes the daemon log's lines now, so record_result reads as the row's answers each
# question that log first names after them (row_answers), however often the row restarts the
# daemon. The row's questions ask for no write until it says otherwise (asks_writes).
criterion() {
  local id=$1 description=$2 blockers line from
  shift 2
  answers_row=
  printf '== %s (%s): %s\n' "$id" "$store" "$description"
  blockers=$(answering_blockers)
  for line in "$@"; do
    case "$line" in
      infra448) grep -rqF '/v1/legion/agents' "$root/packages/envoy/cmd/listener" ||
        blockers+=$'\n'"WAITING (needs INFRA-448): the listener in this checkout has no /v1/legion/agents routes, and Dispatch no Collaborators view" ;;
      openai) [ -z "$gpt_refusal" ] ||
        blockers+=$'\n'"WAITING (needs a model route serving $gpt_model): $gpt_refusal; no GPT-6.1 conversation is made" ;;
      second-owner) [ -n "$second_repo" ] || blockers+=$'\n'"WAITING (needs LEGION_E2E_REPO_SECOND_OWNER): no second owner's repository was given" ;;
    esac
  done
  blockers=$(sed '/^$/d' <<<"$blockers")
  if [ -z "$blockers" ]; then
    [[ " ${rows_ran[$store]:-} " == *" $id "* ]] || rows_ran[$store]+=" $id"
    from=$(log_lines "$daemon_log") || from=
    answers_begin "$id" "$daemon_log" "$from"
    return 0
  fi
  while IFS= read -r line; do record_result "$id" WAITING "${line#WAITING }"; done <<<"$blockers"
  return 1
}
# answers_begin ROW LOG FROM [every]: ROW's answers, in the current store, are the questions the
# store's daemon log LOG first names after its first FROM lines, and its questions ask for no write
# until it says otherwise (asks_writes); the row before it ends here. record_result reads them.
# criterion begins each answering row here, and run_a13 A13's control, whose daemon runs with
# `answers` on, with `every`: that daemon answers again at its boot a question the pass's last
# daemon left generating, named first before FROM, so each question a line after FROM names is the
# control's, counted over those lines alone (row_answers).
answers_begin() {
  answers_row=$1
  answers_store=$store
  answers_log=$2
  answers_asked=()
  answers_from=$3
  answers_named=${4:-}
}
# asks_writes WRITE... says which writes the questions of the row criterion just let run ask for,
# each once, as write_command names it (lib/answer-loop-reads.sh): A17's comment on its pull
# request, A18's two comments, A19's approval, A22's post leg's comment. A17's other two writes are
# none: the leak guard refuses its leak-guard leg's comment before gh starts, and GitHub refuses
# its reviewer's workflow file, a failed write (gh exits above 0), which is counted apart and posts
# nothing. Every other row's questions ask for none, and record_result fails a row whose answers
# wrote anything else, more or less.
asks_writes() { answers_asked=("$@"); }

# envoy_ask TARGET_KIND TARGET QUESTION_ID [KEY]: the rig's asker sends question ID to a stopped
# agent's session (TARGET_KIND session) or role token (role), and prints `<status> <event id>`.
envoy_ask() { envoy_ask_text "$1" "$2" "$(question_text "$3") (question $3 of run $run_tag)" "${4:-}"; }
# envoy_ask_text TARGET_KIND TARGET TEXT [KEY] is envoy_ask with a text of the check's own; KEY is the
# send's idempotency key, so the same KEY is the same question. Its one line on stdout is the
# status line every caller reads with `$(…)`: whatever else it says goes to stderr, which the
# transcript keeps as well.
envoy_ask_text() {
  local kind=$1 target=$2 text=$3 key=${4:-} body out code
  register_asker || note "the listener refused a stand-in's registration (its answers are in $work/registration-*.json); the send follows anyway" >&2
  out=$work/ask-$RANDOM$RANDOM.json
  if [ "$kind" = session ]; then
    body=$(jq -cn --arg s "$target" --arg m "$text" --arg from "$asker_session" --arg key "$key" \
      '{target_session: $s, source: "agent", source_session: $from, message: $m} + (if $key == "" then {} else {idempotency_key: $key} end)')
    code=$(envoy -o "$out" -w '%{http_code}' --data "$body" "$(listener_url /v1/messages/send)")
  else
    body=$(jq -cn --arg t "notifications.role.$target" --arg m "$text" --arg from "$asker_session" --arg key "$key" \
      '{topic: $t, source: "agent", source_session: $from, message: $m} + (if $key == "" then {} else {idempotency_key: $key} end)')
    code=$(envoy -o "$out" -w '%{http_code}' --data "$body" "$(listener_url /v1/messages/publish)")
  fi
  printf '%s %s\n' "$code" "$(jq -r '.event_id // empty' "$out" 2>/dev/null)"
}

# ask_and_expect NAME KIND TARGET QUESTION_ID: the Envoy half every answering criterion shares: a 200
# instead of a 404, then a reply on the asker's topic within four minutes holding the answer key
# (replied_with_keys). It leaves asked_event set to the event the listener answered the send with,
# "" where it answered no 200, and asked_question to the question the reply answers (its
# legion-answer marker), "" where no reply came, so a check judges that answer's own log lines and
# no later one's, and a row's read takes its answer by the event (A2, A15).
ask_and_expect() {
  local label=$1 kind=$2 target=$3 id=$4 sent code event reply missing
  asked_question=
  asked_event=
  sent=$(envoy_ask "$kind" "$target" "$id")
  code=${sent%% *}
  event=${sent#* }
  note "$label: the listener answered $code (event $event)"
  [ "$code" = 200 ] || return 1
  asked_event=$event
  soft_wait 240 "the answer to $id on the asker's topic" inbox_reply "$event" || return 1
  reply=$(inbox_reply "$event")
  asked_question=$(answer_question_id "$reply")
  note "$label: reply: $(head -c 400 <<<"$reply" | tr '\n' ' ')"
  if missing=$(replied_with_keys "$event" "$id"); then
    return 0
  fi
  note "$label: the reply lacks the answer key's \"$missing\""
  return 1
}

# check_boot: the daemon with answers configured records every stopped agent's conversation, the
# pre-change Claude Code one by its title (a scan row), and mirrors each to the listener's Legion
# agent registry: each session's entry names its issue and role, and the replaced tester's names
# the session that replaced it. Control: no registry route answered the daemon 404 this boot.
check_boot() {
  local log routes rows scanned name entry entries=() ok=1
  printf '== boot (%s): the daemon boots with answers configured\n' "$store"
  if [ "$mode" != workflow ]; then
    record_result boot WAITING "(needs $(app_keys_missing_names)): the daemon runs without its workflow, which carries the answer loop and its registry mirror"
    return 0
  fi
  log=$(boot_log)
  routes=$(registry_404_routes "$log")
  note "registry 404 lines this boot, by route (want none): $routes"
  rows=$(pg_sql "$daemon_db" "select session, issue, role, harness, source from agent_conversations where project = '${project,,}' order by recorded_at")
  note "agent_conversations: $rows"
  scanned=$(scan_rows "$rows" "${agent[$store:pre_change.session]}")
  for name in architect planner tester_old tester pre_change; do
    entry=$(envoy "$(listener_url "/v1/legion/agents/sessions/${agent[$store:$name.session]}")" | jq -c '(.agent // .) | {session_id, issue, role, harness, replaced_by, live}' 2>/dev/null) || entry=null
    entries+=("$name $entry")
    registry_entry_matches "$entry" "${agent[$store:$name.issue]:-${agent[$store:root]}}" || ok=0
  done
  printf '   registry: %s\n' "${entries[@]}"
  registry_replaced_by "${entries[2]#* }" "${agent[$store:tester.session]}" || ok=0
  if [ "$routes" = "[]" ] && [ "$scanned" = 1 ] && [ "$ok" = 1 ]; then
    record_result boot PASS "the pre-change Claude Code conversation recorded by its title (scan); every stopped agent in the listener's registry, not live, the replaced tester naming its replacement; no registry route answered 404"
  else
    record_result boot FAIL "404 routes $routes, pre-change scan rows $scanned, registry entries as listed $ok"
  fi
}

check_postgres_store() {
  local rows files
  printf '== store (%s): the panes keep their conversations in the session table\n' "$store"
  rows=$(pg_sql "omp_sessions_$run_tag" "select path, byte_len from public.omp_session_files order by path")
  files=0
  for name in architect planner tester_old tester; do
    [ ! -e "${agent[$store:$name.file]}" ] || files=$((files + 1))
  done
  note "omp_session_files: $rows"
  note "session files on disk at the claims' paths: $files (want 0)"
  if [ "$files" = 0 ] && rows_hold_paths "$rows" "${agent[$store:architect.file]}" "${agent[$store:tester.file]}"; then
    record_result store PASS "every pane's Oh My Pi session is a row of omp_session_files, none a file"
  else
    record_result store FAIL "the session table or the disk disagrees with session_store postgres"
  fi
}

# soft_wait SECONDS DESCRIPTION COMMAND... is lib/rig.sh's until_true for a criterion's step: a
# timeout returns 1, and the check records its FAIL, instead of ending the run.
soft_wait() {
  local limit=$1 what=$2 started=$SECONDS
  shift 2
  note "waiting up to ${limit}s for $what"
  while ((SECONDS - started < limit)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  note "gave up after ${limit}s waiting for $what"
  return 1
}

# hold_at LABEL N lets LABEL's calls through up to ordinal N and holds the next (recording proxy).
hold_at() { printf '%s\n' "$2" >"$work/proxy-hold/$1"; }
release() { rm -f "$work/proxy-hold/$1"; }
# held_count LABEL: how many calls of LABEL the proxy has held so far, the lines of <label>.held,
# to which it appends each call's ordinal as the call starts waiting and from which it removes
# none. held_since LABEL K prints the ordinal of the first call it held after those K, and fails
# while it has held none since: a check reads the call it holds here, never as the ordinal after
# the records' count (proxy_ordinal), which only bounds the ordinals the proxy has given.
held_count() {
  local lines=()
  [ ! -f "$work/proxy-hold/$1.held" ] || mapfile -t lines <"$work/proxy-hold/$1.held"
  printf '%s\n' "${#lines[@]}"
}
held_since() {
  local lines=()
  [ -f "$work/proxy-hold/$1.held" ] || return 1
  mapfile -t lines <"$work/proxy-hold/$1.held"
  [ "${#lines[@]}" -gt "$2" ] && [ -n "${lines[$2]}" ] || return 1
  printf '%s\n' "${lines[$2]}"
}

# smoke_gh ARG...: one gh call on the smoke repository as the implement App, printing gh's own
# output. The rig's own gh reads nothing there: in a Legion pane the first gh on PATH is the
# `legion gh` wrapper, and the pane's own token is narrowed to another owner, so either answers
# `{"message":"Not Found",...}` (A17 read that body and found none of the comments the implementer
# had just posted). So the rig mints a read-only installation token of its own, as provisioning
# does (mint_provision_token), and runs the compiled gh with it; the token is printed nowhere and
# is removed again.
smoke_gh() {
  local token_file=$work/gh-read-token code=0
  rm -f "$token_file"
  mint_provision_token "$token_file" >&2 || return 1
  GH_TOKEN=$(cat "$token_file") timeout 60 "$work/realbin/gh" "$@" || code=$?
  rm -f "$token_file"
  return "$code"
}

# pane_count prints how many panes the daemon's private tmux server holds now, 0 while it runs
# none: a check that must start nothing reads it before its question and after the answer (A1,
# A20, A21).
pane_count() { TMUX_TMPDIR="$work/tmux" "$tmux_bin" -L "legion-${project,,}" list-panes -a -F '#{pane_id}' 2>/dev/null | wc -l || true; }

# a1_read ASK COMMENT PANES [RECORD] is A1's read, which the row and its control both run: the
# architect's reply on ask ASK written after the person's reply COMMENT holds question
# architect-decision's keys, the daemon's tmux server holds no more than PANES panes, and every
# claim of the tree stands suspended or retired. RECORD is ASK's record as the control read it
# once, read now where not given (ask_reply_after). It says what it read in read_said.
a1_read() {
  local reply panes claims
  reply=$(ask_reply_after "$1" "$2" "${agent[$store:architect.session]}" "${4:-}" 2>/dev/null) || reply=
  panes=$(pane_count)
  claims=$(claims_cmd list --json 2>/dev/null) || claims='{}'
  read_said="the architect's reply on ask $1 after $2: $(said_text "$reply"); panes $3 → $panes; the tree's claims $(jq -c --arg t "${agent[$store:root]}" '[.claims[]? | select(.tree == $t) | {role, state}]' <<<"$claims" 2>/dev/null || true)"
  [ -n "$reply" ] && answer_holds architect-decision "$reply" >/dev/null && [ "$panes" -le "$3" ] &&
    tree_claims_parked "$claims" "${agent[$store:root]}"
}
check_a1() {
  criterion A1 "a question on a parked tree's architect's ask is answered in the thread within four minutes, nothing of the tree starting" || return 0
  local ask a1_comment before panes_before
  ask=$(ask_of "${agent[$store:architect.session]}" "${agent[$store:root]}")
  [ -n "$ask" ] || {
    record_result A1 FAIL "the parked architect opened no ask on ${agent[$store:root]}"
    return 0
  }
  agent[$store:a1.ask]=$ask
  panes_before=$(pane_count)
  before=$(date +%s)
  a1_comment=$(dispatch_human POST "issues/${agent[$store:root]}/comments" "$(jq -cn --arg ask "$ask" --arg body "$(question_text architect-decision)" \
    '{body: $body, ask_id: $ask}')" | jq -r .id)
  agent[$store:a1.comment]=$a1_comment
  if soft_wait 240 "the architect's answer in ask $ask's thread" ask_reply_after "$ask" "$a1_comment" "${agent[$store:architect.session]}"; then
    note "answered after $(($(date +%s) - before)) s"
    if a1_read "$ask" "$a1_comment" "$panes_before"; then
      note "$read_said"
      record_result A1 PASS "answered in the thread as ${agent[$store:architect.session]} within four minutes; no claim or pane of the tree started"
    else
      note "$read_said"
      record_result A1 FAIL "the answer lacks its key, or a claim or pane of the tree started: $read_said"
    fi
  else
    record_result A1 FAIL "no answer in ask $ask's thread within four minutes"
  fi
}

# a2_read ROLE_EVENT OLD_EVENT PRE_EVENT CODE is A2's read: the tester's role topic, its replaced
# session and the pre-change reviewer's session were each answered on the asker's topic, the reply
# to each event holding its question's keys (tester-role, tester-old-session, pre-change-scan), and
# a send to a session nobody registered answered CODE 404.
a2_read() {
  local id event missing passed=0 said=''
  for id in tester-role tester-old-session pre-change-scan; do
    event=$1
    shift
    if missing=$(replied_with_keys "$event" "$id"); then
      passed=$((passed + 1))
      said+="$id answered with its keys; "
    else
      said+="$id: $missing; "
    fi
  done
  read_said="${said}$passed of 3 answered; the unregistered session answered $1"
  [ "$passed" = 3 ] && [ "$1" = 404 ]
}
# a2_unregistered_send prints what the listener answers a send to a session nobody registered,
# A2's own control within the row, its body kept in $work/a2-control.json.
a2_unregistered_send() {
  envoy -o "$work/a2-control.json" -w '%{http_code}' --data "$(jq -cn --arg s "legion-e2eal-unregistered-$$" \
    '{target_session: $s, source: "agent", message: "control: nobody registered this session"}')" "$(listener_url /v1/messages/send)"
}
check_a2() {
  criterion A2 "a finished tester's role topic, its replaced session and a conversation known only from the title scan each answer instead of a 404" || return 0
  local code
  local -a events=()
  ask_and_expect "role ${agent[$store:tester.token]}" role "${agent[$store:tester.token]}" tester-role || true
  events+=("$asked_event")
  ask_and_expect "replaced session ${agent[$store:tester_old.session]}" session "${agent[$store:tester_old.session]}" tester-old-session || true
  events+=("$asked_event")
  ask_and_expect "pre-change session ${agent[$store:pre_change.session]}" session "${agent[$store:pre_change.session]}" pre-change-scan || true
  events+=("$asked_event")
  code=$(a2_unregistered_send)
  note "control: an unregistered session answers $code: $(cat "$work/a2-control.json")"
  if a2_read "${events[@]}" "$code"; then
    record_result A2 PASS "role, replaced session and scanned conversation answered on the asker's topic; an unregistered session still 404"
  else
    record_result A2 FAIL "$read_said"
  fi
}

# check_a3: a person closes the tester's issue, finds its tester through the issue's collaborators
# (the Legion agent registry's entries for the issue, the list the mention picker offers stopped
# agents from), and asks on the closed issue's own thread with a role mention of that tester; the
# answer is the tester's reply to that comment, in its own name, holding what only the replacing
# tester's conversation holds. The mention's delivery is read again from Dispatch until it is sent
# to that tester (comment_delivered_to): the comment's own POST answer carries it pending, since
# Dispatch settles a mention after it answers. Control: a mention of a session nobody ever
# registered, on the same closed issue, fails its delivery `no live session <id>`: the listener
# answers such a send 404 and the Legion agent registry names no such session. A role nobody holds
# is no control: under a project whose daemon answers, Dispatch keeps a mention of a role the
# registry holds no entry for, for the role's first holder, rather than refuse it.
# Dispatch settles the refusal only after it has waited out a listener take-over (takeoverRetry,
# 12 s), so the control's delivery is read again until it is refused. Then the second leg,
# a3_conversation_page: the same person, in a browser, opens the closed issue's Collaborators
# view, follows the tester's own link to its conversation page and asks a second question there.
# A3 passes only on both legs. Its comment is kept (agent[<store>:a3.comment]): A20 replies in that
# comment's thread, under the tester's answer. Its read, a3_read, is the thread leg's answer and
# delivery and the conversation-page leg's record, which its control runs too; the page's own
# thread is the browser's alone (a3_conversation_page).
a3_control_refused() { one_delivery_in "$(comment_deliveries "$1")" failed "no live session $2"; }
# a3_read ISSUE COMMENT SESSION BODY [COMMENTS [MESSAGES]] is A3's read: the tester SESSION's reply
# to the role mention COMMENT on closed ISSUE holds tester-closed-thread's keys, and the mention's
# one delivery is sent to SESSION (the thread leg); and SESSION's reply to the person's message
# BODY, in the record the tester's conversation page reads (GET agents/<session>/messages), holds
# tester-conversation-page's keys (the conversation-page leg's record). COMMENTS are ISSUE's
# comments and MESSAGES the proof person's messages to SESSION (message_listing) as A3's control
# read them once, each read now where not given.
a3_read() {
  local reply record_reply thread=no delivered=no record=no
  reply=$(comment_reply_by "$1" "$2" "$3" "${5:-}" 2>/dev/null) || reply=
  [ -n "$reply" ] && answer_holds tester-closed-thread "$reply" >/dev/null && thread=yes
  comment_delivered_to "$2" "$3" && delivered=yes
  record_reply=$(message_reply_by "$3" "$4" "${6:-}" 2>/dev/null) || record_reply=
  [ -n "$record_reply" ] && answer_holds tester-conversation-page "$record_reply" >/dev/null && record=yes
  read_said="the tester's reply to comment ${2:-(none)} holding its keys: $thread ($(said_text "$reply")); its one delivery sent to $3: $delivered ($(comment_delivery_records "$2")); its reply in the page's record holding its keys: $record ($(said_text "$record_reply"))"
  [ "$thread$delivered$record" = yesyesyes ]
}
check_a3() {
  criterion A3 "a question on a closed issue's own thread, by a role mention of its stopped tester, is answered there, and a second asked from the tester's conversation page reached through the issue's Collaborators view is answered there" infra448 || return 0
  local issue=${agent[$store:tester.issue]} session=${agent[$store:tester.session]} collaborators token comment id reply='' control control_id nobody=legion-e2eal-nobody-$run_tag before refused='' page_leg='' page_body
  set_status "$issue" "done"
  note "closed $issue: status $(dispatch_get "issues/$issue" | jq -r .status)"
  collaborators=$(dispatch_get "issues/$issue/collaborators") || collaborators='{"agents":[],"legion_registry":"unavailable"}'
  note "collaborators of $issue: registry $(jq -r .legion_registry <<<"$collaborators"); agents $(jq -c '[.agents[] | {session_id, legion: (.legion | if . then {role, role_token, replaced_by} else null end)}]' <<<"$collaborators")"
  token=$(collaborator_role_token "$collaborators" "$session")
  if [ -z "$token" ]; then
    record_result A3 FAIL "the collaborators of closed $issue name no current tester $session with its role token, so neither the thread leg nor the conversation-page leg can run"
    return 0
  fi
  agent[$store:a3.token]=$token
  before=$(date +%s)
  comment=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg body "@tester $(question_text tester-closed-thread) (question tester-closed-thread of run $run_tag)" \
    --arg t "role:$token" '{body: $body, mentions: [{target: $t}]}')")
  id=$(jq -r .id <<<"$comment")
  agent[$store:a3.comment]=$id
  note "the person's comment $id on closed $issue mentions role:$token; deliveries $(jq -c '[.deliveries[]? | {target, state, session_id, error}]' <<<"$comment")"
  if soft_wait 240 "the tester's reply to comment $id" comment_reply_by "$issue" "$id" "$session"; then
    reply=$(comment_reply_by "$issue" "$id" "$session")
    note "answered after $(($(date +%s) - before)) s as $session: $(head -c 400 <<<"$reply" | tr '\n' ' ')"
  fi
  soft_wait 60 "comment $id's mention to be delivered to tester $session" comment_delivered_to "$id" "$session" || true
  note "comment $id's deliveries as Dispatch holds them now: $(comment_delivery_records "$id")"
  control_id=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg t "session:$nobody" \
    '{body: "control: a session nobody registered", mentions: [{target: $t}]}')" | jq -r '.id // empty') || control_id=
  soft_wait 60 "the control's mention of session $nobody to be refused" a3_control_refused "$control_id" "$nobody" || true
  control=$(comment_deliveries "$control_id")
  note "control: a mention of session $nobody, which nobody registered, on closed $issue (comment $control_id): $control"
  one_delivery_in "$control" failed "no live session $nobody" && refused=1
  page_body="$(question_text tester-conversation-page) (question tester-conversation-page of run $run_tag)"
  a3_conversation_page "$issue" "$session" "$page_body" && page_leg=1
  if a3_read "$issue" "$id" "$session" "$page_body" && [ -n "$page_leg" ] && [ -n "$refused" ]; then
    note "$read_said"
    record_result A3 PASS "the role mention on closed $issue reached tester $session, which answered in the thread from its conversation, and a question asked from that tester's conversation page, reached through the issue's Collaborators view, was answered there; the mention of a session nobody registered failed no live session"
  else
    note "$read_said"
    record_result A3 FAIL "$read_said; the page's own thread showed an answer holding its keys: $([ -n "$page_leg" ] && printf yes || printf no); the mention of a session nobody registered failed no live session: $([ -n "$refused" ] && printf yes || printf no)"
  fi
}

# a3_conversation_page ISSUE SESSION BODY is the browser's half of A3's second leg: as the proof
# human, in a browser, the closed issue's page is opened, its Collaborators view read, and the
# current tester's own link followed to its conversation page, where BODY, question
# tester-conversation-page, is asked and waited out for four minutes
# (lib/dispatch-user-turns.ts's `collaborator`, a screenshot per step in <evidence>/a3). The answer
# must be on the page's own thread and hold the question's key; the record that page reads, the
# leg's other half, is a3_read's. It prints the leg's evidence and returns 0 when the page's own
# answer holds the key.
a3_conversation_page() {
  local issue=$1 session=$2 body=$3 shots page='' record='' page_answer='' missing
  if ! browser_leg; then
    note "conversation-page leg: no browser in this run"
    return 1
  fi
  shots=$evidence/a3/$store
  mkdir -p "$shots"
  if ! page=$(bun "$root/scripts/e2e/lib/dispatch-user-turns.ts" collaborator "$dispatch_base" "$proof_human_user" \
    "$issue" "$session" tester "$body" 240 "$shots/a3" 2>"$work/a3-page-$store.err"); then
    note "conversation-page leg: the page stopped: $(tr '\n' ' ' <"$work/a3-page-$store.err")"
  fi
  # The helper prints what it saw whether it reached the answer or not, so the evidence below is
  # the page's own either way; only a crash before its first write leaves nothing.
  [ -n "$page" ] || page='{}'
  note "conversation-page leg: $(jq -c 'del(.listed, .answer)' <<<"$page")"
  note "the Collaborators view listed: $(jq -c '.listed // []' <<<"$page")"
  page_answer=$(jq -r '.answer // empty' <<<"$page")
  [ -z "$page_answer" ] || note "the page's thread shows as the tester's answer: $(head -c 400 <<<"$page_answer" | tr '\n' ' ')"
  # The page's own record of that message, read as the person whose page it is (the route serves
  # the signed-in person's own messages with that session, and refuses the agents' bearer 403):
  # the delivery it recorded for the stopped tester, which the listener's hand-off makes `sent`.
  record=$(message_record "$session" "$body") || record='{}'
  note "the record the page reads (GET agents/$session/messages): deliveries $(jq -c '[.message.deliveries[]? | {state, session_id, delivery}]' <<<"$record")"
  [ -n "$page_answer" ] || return 1
  if ! missing=$(answer_holds tester-conversation-page "$page_answer"); then
    note "the page's answer does not hold \"$missing\""
    return 1
  fi
  note "conversation-page leg: the Collaborators view named tester $session, its link opened $(jq -r '.conversation // "nothing"' <<<"$page"), and the answer on the page holds the question's key"
}

# check_a20: a person's reply that mentions nobody, under the loop's own answer in A3's thread, is
# answered there by the stopped tester. A3's role mention was answered in the tester's name as a
# reply in the thread of A3's comment; the proof person replies under that answer (`reply_to` its
# id: Dispatch climbs it to the thread's head, A3's comment, and keeps the answer as the comment
# the reply `answers`), asking question tester-thread-reply, whose answer the tester's own
# conversation holds. Only a person's reply notifies the sessions that wrote in a thread, and the
# proof person is the rig's user (dispatch_human), so Dispatch sends the reply to the tester's
# session, the one session in the thread. The listener holds a message on the agent subject of a
# session it has no record of for 60 s before it hands it to that agent's daemon, so the answer is
# waited for 300 s: the four minutes the other rows give an answer, and that minute. The loop
# answers a reply in a comment thread when the newest comment before it is the agent's, else when
# the thread's root is, and posts its answer in that thread under the reply, so PASS reads a second
# reply of the tester's under A3's comment, written under the person's reply and holding the
# question's keys. A loop that takes every comment with no mention's delivery and no ask for no
# question leaves the reply unanswered, and the row fails on it. Control: a second reply the person
# posts at once under their own follows a person's comment, in a thread whose root a person wrote,
# so it is no agent's to answer: the tester's daemon takes it for no question for that reason
# (answers_lines, read by its `because`) and no reply of the tester's is written under it, so the
# thread holds the tester's two replies and no third. The two waits share one 300 s deadline,
# taken when both replies are posted: the control's wait has what is left of it once the answer's
# wait ends. Nothing of the tester starts: its claim stays suspended and the daemon's tmux server
# holds no more panes (pane_count), as A1 reads its tree. No later check posts a reply in this
# thread, or in another thread the tester wrote in, which the loop would now answer as well. Its
# read, a20_read, holds its last sentence apart as a20_nothing_posted, the read A20's control
# places a reply carrying the loop's mark for (run_controls).
a20_nobody='a reply in a comment thread whose root and newest comment before the reply no agent wrote'
# a20_skipped SESSION: how many hand-offs of SESSION this boot took for no question for the reason a
# reply following a person's own in a person's thread is given (a20_nobody).
a20_skipped() { answers_lines "answers: hand-off is no question; not answered" "$1" "" "$a20_nobody"; }
# a20_nothing_posted HEAD CONTROL [REPLIES] is A20's last sentence: in the thread of comment HEAD the
# tester wrote no reply under CONTROL, the person's reply that follows their own. REPLIES are the
# tester's replies in that thread, read now where not given. A CONTROL Dispatch took none of shows
# nothing.
a20_nothing_posted() {
  local replies=${3:-$(thread_replies_by "$1" "${agent[$store:tester.session]}")} under
  under=$(reply_answering "$replies" "${2:-}") || under=
  read_said="the tester's reply under ${2:-(no reply)} in the thread of $1: $(said_text "$under")"
  [ -n "${2:-}" ] && [ -z "$under" ]
}
# a20_answer is the tester's answer under the person's reply, as a20_read last read it.
a20_answer=
# a20_read HEAD REPLY CONTROL SKIPPED PANES COUNT [RECORD] is A20's read: in the thread of comment
# HEAD the tester wrote a reply under the person's REPLY holding tester-thread-reply's keys, and
# COUNT replies in all (the row's two: A3's answer and this one; its control's, the replies it found
# and the one its answer would add); nothing under CONTROL (a20_nothing_posted); its daemon took
# more than SKIPPED hand-offs for no question for the reason CONTROL is none (a20_skipped); its claim
# is suspended; and the daemon's tmux server holds no more than PANES panes. RECORD is HEAD's record
# as A20's control read it once (comment_record), read now where not given.
a20_read() {
  local session=${agent[$store:tester.session]} record=${7:-} replies thread answer missing='' held=no skipped state panes nothing=no
  [ -n "$record" ] || record=$(comment_record "$1")
  replies=$(thread_replies_in "$record" "$session")
  thread=$(jq -c 'map({id, answers})' <<<"$replies") || thread=$replies
  answer=$(reply_answering "$replies" "$2") || answer=
  a20_answer=$answer
  if [ -n "$answer" ]; then
    missing=$(answer_holds tester-thread-reply "$answer") && held=yes
  fi
  a20_nothing_posted "$1" "$3" "$replies" && nothing=yes
  skipped=$(($(a20_skipped "$session") - $4))
  state=$(claim_json "${agent[$store:tester.token]}" | jq -r .state) || state=unread
  panes=$(pane_count)
  read_said="tester $session's replies in the thread of $1: $(reply_count "$replies") (want $6) ($thread); its answer under the person's reply $2 $(if [ -z "$answer" ]; then printf none; elif [ "$held" = yes ]; then printf 'holds the keys'; else printf 'lacks "%s"' "$missing"; fi); nothing under ${3:-(no reply)}: $nothing; taken for no question as \"$a20_nobody\" $skipped time(s); the tester's claim $state; panes $5 → $panes"
  [ "$held" = yes ] && [ "$(reply_count "$replies")" = "$6" ] && [ "$nothing" = yes ] && [ "$skipped" -gt 0 ] &&
    [ "$state" = suspended ] && [ "$panes" -le "$5" ]
}
check_a20() {
  criterion A20 "a person's reply that mentions nobody, under the loop's own answer in a closed issue's thread, is answered there by the stopped tester, nothing of it starting; a reply that follows the person's own is no agent's to answer" infra448 || return 0
  local issue=${agent[$store:tester.issue]} session=${agent[$store:tester.session]}
  local head=${agent[$store:a3.comment]:-} replies first reply_id control_id skipped_before panes_before before deadline ok=0
  if [ -z "$head" ]; then
    record_result A20 FAIL "A3 posted no comment on $issue whose thread this row replies in"
    return 0
  fi
  replies=$(thread_replies_by "$head" "$session")
  first=$(first_reply_id "$replies")
  if [ -z "$first" ]; then
    record_result A20 FAIL "the thread of A3's comment $head holds no reply of tester $session to reply under: $replies"
    return 0
  fi
  agent[$store:a20.first]=$first
  skipped_before=$(a20_skipped "$session")
  panes_before=$(pane_count)
  before=$(date +%s)
  reply_id=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg to "$first" \
    --arg body "$(question_text tester-thread-reply) (question tester-thread-reply of run $run_tag)" '{body: $body, reply_to: $to}')" |
    jq -r '.id // empty') || reply_id=
  if [ -z "$reply_id" ]; then
    record_result A20 FAIL "Dispatch took no reply of the proof person's under tester $session's answer $first on $issue"
    return 0
  fi
  control_id=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg to "$reply_id" \
    --arg body "And how many runs did that rerun pass? (control: a reply under the person's own reply, run $run_tag)" '{body: $body, reply_to: $to}')" |
    jq -r '.id // empty') || control_id=
  deadline=$((SECONDS + 300))
  note "the person's reply $reply_id under tester $session's answer $first in the thread of A3's comment $head, mentioning nobody; the control $control_id under that reply"
  a20_answered() { reply_answering "$(thread_replies_by "$head" "$session")" "$reply_id" >/dev/null; }
  soft_wait $((deadline - SECONDS)) "tester $session's answer under reply $reply_id in the thread of $head" a20_answered || true
  a20_control_skipped() { more_than "$skipped_before" a20_skipped "$session"; }
  [ -z "$control_id" ] || soft_wait $((deadline - SECONDS)) "the tester's daemon to take the control $control_id for no question" a20_control_skipped || true
  a20_read "$head" "$reply_id" "$control_id" "$skipped_before" "$panes_before" 2 && ok=1
  [ -z "$a20_answer" ] || note "answered within $(($(date +%s) - before)) s as $session: $(head -c 400 <<<"$a20_answer" | tr '\n' ' ')"
  if [ "$ok" = 1 ]; then
    note "$read_said"
    record_result A20 PASS "tester $session answered the person's reply $reply_id, which mentions nobody, under its own answer in the thread of A3's comment $head, with the question's keys; the control under the person's own reply was taken for no question (\"$a20_nobody\") and answered by nobody; the tester's claim stayed suspended and no pane started"
  else
    note "$read_said"
    record_result A20 FAIL "$read_said"
  fi
}

# passed_on_read ID EVENT is the read of A4 and A5 (question ID): the planner's reply to EVENT came,
# its `answers: answered` line is of the kind question ID expects, `legion state` lists the question
# for the controller for that kind, and the planner's claim stands retired. A fault turns any reply
# into kind not_answered (answer.Answer), so the kind alone can read as wanted when the reply was
# refused: the read names the line's fault, and what the controller was told, beside it.
passed_on_read() {
  local expect reply line kind state reason
  expect=$(question "$1" | jq -r .expect_kind)
  reply=$(inbox_reply "$2" 2>/dev/null) || reply=
  line=$(answered_line "$(answer_question_id "$reply")") || line=
  kind=$(jq -r '.kind // empty' <<<"${line:-null}")
  state=$(claim_json "${agent[$store:planner.token]}" | jq -r .state) || state=unread
  reason=$(legion_cmd state --json 2>/dev/null | jq -r --arg q "${2:-(no event)}" '[.questionsForController[]? | select((((.id // "") + " " + (.ref // "")) | contains($q))) | .reason] | if length == 0 then "none" else join(",") end') || reason=unread
  read_said="the planner's reply to ${2:-(no event)}: $(said_text "$reply"); its answered line ${line:-none}; kind ${kind:-none} (want $expect), fault $(jq -r '(.fault // "") | if . == "" then "none" else . end' <<<"${line:-null}")$(jq -r 'if (.detail // "") == "" then "" else " (" + .detail + ")" end' <<<"${line:-null}"), controller reason $reason, claim $state"
  [ -n "$reply" ] && [ "$kind" = "$expect" ] && passed_to_controller "$2" "$expect" && [ "$state" = retired ]
}
check_passed_on() {
  local id=$1 crit=$2 expect sent code event reply line
  expect=$(question "$id" | jq -r .expect_kind)
  criterion "$crit" "$(question_text "$id")" || return 0
  sent=$(envoy_ask session "${agent[$store:planner.session]}" "$id")
  code=${sent%% *}
  event=${sent#* }
  if [ "$code" != 200 ] || ! soft_wait 240 "the planner's reply to $id" inbox_reply "$event"; then
    record_result "$crit" FAIL "the listener answered $code, or no reply reached the asker"
    return 0
  fi
  # The controller's notice follows only an answer of the kind the question expects, so a minute is
  # waited for it only then; passed_on_read says what the answered line and the state hold.
  reply=$(inbox_reply "$event")
  line=$(answered_line "$(answer_question_id "$reply")") || line=
  [ "$(jq -r '.kind // empty' <<<"${line:-null}")" != "$expect" ] ||
    soft_wait 60 "the question in questionsForController" passed_to_controller "$event" "$expect" || true
  if passed_on_read "$id" "$event"; then
    note "$read_said"
    record_result "$crit" PASS "replied as $expect, passed to the controller with the asker; the planner's claim stayed retired"
  else
    note "$read_said"
    record_result "$crit" FAIL "$read_said"
  fi
}

# decide_and_ask LEG NAME QUESTION_ID ROUTE: a person replies on the ask agent NAME opened, with a
# decision that also asks something (question QUESTION_ID). The agent's answer must come on the ask's
# thread in its name, holding the answer key, and the daemon must pass the decision on once, to ROUTE
# ("the controller", read on the controller stand-in's topic, or an architect NAME, read in its
# conversation), and to nobody else, a minute later still once (decide_and_ask_read, which A6's
# control runs too). It prints the leg's evidence and returns 0 when all of that holds.
decide_and_ask() {
  local leg=$1 name=$2 id=$3 route=$4 session issue ask comment
  session=${agent[$store:$name.session]}
  issue=${agent[$store:$name.issue]}
  ask=$(ask_of "$session" "$issue")
  if [ -z "$ask" ]; then
    note "$leg: $name ($session) opened no ask on $issue"
    return 1
  fi
  comment=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg ask "$ask" --arg body "$(question_text "$id") (question $id of run $run_tag)" \
    '{body: $body, ask_id: $ask}')" | jq -r .id)
  note "$leg: the person's reply $comment on $name's ask $ask on $issue decides and asks"
  soft_wait 240 "$name's answer on ask $ask's thread" ask_reply_after "$ask" "$comment" "$session" || true
  if [ "$route" = "the controller" ]; then
    delivered() { more_than 0 controller_notices agent-decision "$comment"; }
  else
    delivered() { more_than 0 notices_in_conversation "$route" "$comment"; }
  fi
  soft_wait 240 "the decision to reach $route" delivered || true
  sleep 60
  if decide_and_ask_read "$name" "$id" "$route" "$ask" "$comment"; then
    note "$leg: $read_said"
    return 0
  fi
  note "$leg: $read_said"
  return 1
}
# decide_and_ask_read NAME ID ROUTE ASK COMMENT [RECORD] is the read of one leg of A6: NAME's reply
# on ask ASK written after the person's reply COMMENT holds question ID's keys; the decision COMMENT
# carries reached ROUTE once ("the controller": the controller stand-in's agent-decision notices
# holding it; an architect NAME: the notices its conversation holds); this boot logged it passed on
# once, to ROUTE; and a decision routed to an architect reached the controller not at all
# (a6_nothing_posted). A COMMENT Dispatch took none of names no decision. RECORD is ASK's record as
# A6's control read it once, read now where not given (ask_reply_after).
decide_and_ask_read() {
  local reply lines delivered want_to=$3 at_controller quiet=no
  reply=$(ask_reply_after "$4" "$5" "${agent[$store:$1.session]}" "${6:-}" 2>/dev/null) || reply=
  lines=$(decision_lines "$5")
  at_controller=$(controller_notices agent-decision "$5")
  if [ "$3" = "the controller" ]; then
    delivered=$at_controller
  else
    delivered=$(notices_in_conversation "$3" "$5" 2>/dev/null) || delivered=0
    want_to=${agent[$store:$3.token]}
    a6_nothing_posted "$5" && quiet=yes
  fi
  read_said="$1's answer on ask $4 after ${5:-(no reply)}: $(said_text "$reply"); the daemon's decision lines: $lines; agent-decision notices holding it at $3: $delivered, at the controller stand-in: $at_controller"
  [ -n "$5" ] && [ -n "$reply" ] && answer_holds "$2" "$reply" >/dev/null && [ "$delivered" = 1 ] &&
    decision_passed_once "$lines" "$want_to" && { [ "$3" = "the controller" ] || [ "$quiet" = yes ]; }
}
# a6_nothing_posted COMMENT [INBOX] is the part of A6's second leg that says the loop posts nothing
# to the controller: no agent-decision notice holding the person's reply COMMENT, a decision the
# loop routes to the running tree's architect, reached the controller stand-in. A6's control places
# such a notice there, carrying the loop's mark (place_controller_notice), and this read, over the
# inbox it read once (INBOX, inbox_listing; read now where not given), must see it. A COMMENT
# Dispatch took none of names no decision, and shows nothing.
a6_nothing_posted() {
  local notices
  notices=$(controller_notices agent-decision "${1:-}" "${2:-}")
  read_said="agent-decision notices holding ${1:-(no reply)} at the controller stand-in: $notices"
  [ -n "${1:-}" ] && [ "$notices" = 0 ]
}

# check_a6: A6's two legs. The first: a person's reply on the replacing tester's ask (a child of the
# parked tree, its architect suspended) decides and asks; the decision reaches the controller once,
# since nobody in that tree runs. The second: a tree whose root architect runs, with a child parked
# (its tester opened an ask and was suspended); a person's reply there reaches that architect once,
# as an agent-decision notice, and not the controller. The daemon routes a notice only through the
# trees it records, and the rig's issues are a person's (no `legion` label, so the workflow records
# none of them): the running tree is written into the daemon's record with the daemon stopped, as
# admission writes a root, the child it records under it and the tree's own lifecycle
# (record_tree_issue, open_tree_lifecycle), a departure like A19's. The tree's architect is spawned
# before that stop, because its spawn is what opens the tree's lifecycle row and an operator's
# spawn on a tree the workflow holds is refused; the daemon's stop takes its pane with it, and the
# daemon launches it again on the same conversation at the next boot. Nothing of the second leg is
# run until the daemon's own state says the seeded tree is one it keeps live and its architect
# holds its role, so a leg whose tree admission released can only fail. Each leg's question gets
# its own answer on the ask's thread.
check_a6() {
  criterion A6 "a decision on a parked tree's child and on a parked child of a running tree reaches the controller or architect once, and its question is answered" || return 0
  local root6 child6 architect ok1=0 ok2=0 why=
  decide_and_ask "parked tree's child tester" tester a6-tester-decision "the controller" && ok1=1
  root6=$(new_rig_issue "Answer loop proof: a running tree (A6, $store)")
  child6=$(new_rig_issue "Answer loop proof child: parked under a running tree (A6)" "$root6")
  architect=$(claim_token "$root6" architect)
  provision_workspace "$root6"
  claims_cmd spawn --json --tree "$root6" --issue "$root6" --role architect --prompt-file "$(write_prompt architect "$root6")" >/dev/null
  agent[$store:a6_architect.token]=$architect
  agent[$store:a6_architect.issue]=$root6
  soft_wait 600 "the running tree's architect to hold its role" claim_is "$architect" '.state | IN("ready", "idle")' || true
  stop_daemon
  record_tree_issue "$root6" "$root6" "" implementing "A6 $root6"
  record_tree_issue "$child6" "$root6" "$root6" implementing "A6 $child6"
  open_tree_lifecycle "$root6"
  note "recorded the running tree $root6 and its child $child6 in the daemon's issues table, with $root6's tree lifecycle open under the workflow's own authority"
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
  if ! soft_wait 600 "the running tree's architect to hold its role again" claim_is "$architect" '.state | IN("ready", "idle")'; then
    why="the running tree's architect $architect did not hold its role after the restart: $(claim_json "$architect" | jq -c '{state, session}' || true)"
  elif ! soft_wait 300 "the daemon to keep the seeded tree $root6 live" tree_is_live "$root6" implementing; then
    why="the seeded tree $root6 was not live in the daemon's own state ($(legion_cmd state --json | jq -c --arg root "$root6" '{trees: .admission.trees, phase: .issues[$root].phase}'))"
  fi
  note "the seeded tree in the daemon's own state: $(legion_cmd state --json | jq -c --arg root "$root6" '{trees: .admission.trees, phase: .issues[$root].phase}'), its architect $(claim_json "$architect" | jq -c '{state, session}' || true); its admission lines this boot: $(boot_log | jq -s -c --arg root "$root6" '[.[] | select(.issue == $root and ((.msg // "") | startswith("admission:"))) | .msg]')"
  agent[$store:a6_architect.session]=$(claim_json "$architect" | jq -r .session || true)
  agent[$store:a6_architect.file]=$(claim_json "$architect" | jq -r .sessionFile || true)
  if [ -n "$why" ]; then
    note "the second leg is not run: $why"
  else
    spawn_agent a6_child tester "$root6" "$child6" \
      "$(jq -r .agents.a6_child.task "$questions_file") $(ask_instruction "$child6" "to quarantine TestGarnetFalcon")"
    await_turn a6_child
    if suspend_agent a6_child; then
      note "the parked child's tester ${agent[$store:a6_child.session]} is suspended; the tree's architect $architect is $(claim_json "$architect" | jq -c '{state, session}')"
      agent[$store:a6.leg2]=1
      decide_and_ask "parked child of a running tree" a6_child a6-child-decision a6_architect && ok2=1
    else
      note "the parked child's tester was not suspended, or its session stayed live"
    fi
  fi
  claims_cmd suspend --claim "$architect" >/dev/null || true
  if [ "$ok1$ok2" = 11 ]; then
    record_result A6 PASS "each decision reached its route once (the parked tree's: the controller; the running tree's parked child's: its architect $architect, not the controller), and each question was answered on its ask's thread"
  else
    record_result A6 FAIL "parked tree's child $ok1, parked child of a running tree $ok2 (1 = answered, and the decision passed once to its route)${why:+; $why}"
  fi
}

# suspend_agent NAME suspends NAME's claim and waits until its session has left the listener's
# session bucket (session_gone), so the next message to it is handed to the daemon rather than sent
# to the pane that is gone; it fails (returns 1) when either does not happen in time.
suspend_agent() {
  local token=${agent[$store:$1.token]}
  claims_cmd suspend --claim "$token" >/dev/null || return 1
  soft_wait 300 "$1 to be suspended" claim_is "$token" '.state == "suspended"' &&
    soft_wait "$session_ttl_wait" "$1's session to leave the listener's registry" session_gone "${agent[$store:$1.session]}"
}
# resume_agent NAME resumes NAME's claim on its kept conversation and waits until it holds its
# session in the listener again.
resume_agent() {
  local token=${agent[$store:$1.token]}
  claims_cmd resume --claim "$token" >/dev/null || return 1
  soft_wait 300 "$1 to run again" claim_is "$token" '.state | IN("ready", "idle")' &&
    soft_wait 120 "$1's session to be live in the listener" session_live "${agent[$store:$1.session]}"
}
# deliver_task TOKEN TASK delivers TASK to TOKEN's claim (`legion claims deliver`) once the claim
# holds no task. A resume of a claim whose suspension cut a running turn has the daemon queue a
# delivery of its own a moment after the claim is ready again, the sentence that tells the agent
# what the relaunch ended (supervise's queueRelaunch), and a delivery made while that one is pending
# is refused `a delivery is already pending`: proof run 45082273709 met it in A9 under the postgres
# store, 220 ms after the daemon queued its own. So it waits, up to 600 s each time, until the
# claim's `pending` is gone, which it is once the turn its task started ends, then delivers, and on
# that refusal waits and delivers again, three times at most. Any other refusal fails it at once,
# and the notes name it.
deliver_task() {
  local token=$1 task=$2 out attempt
  for attempt in 1 2 3; do
    soft_wait 600 "$token to hold no pending task" claim_is "$token" '.pending == null' || true
    if out=$(claims_cmd deliver --claim "$token" --task "$task" 2>&1); then
      return 0
    fi
    case "$out" in
      *"a delivery is already pending"*) note "delivery $attempt to $token met a pending delivery: $(tr '\n' ' ' <<<"$out")" ;;
      *)
        note "the delivery to $token was refused: $(tr '\n' ' ' <<<"$out")"
        return 1
        ;;
    esac
  done
  return 1
}
# compact_claim TOKEN asks the daemon to compact TOKEN's session (`legion claims compact`) once the
# claim is ready or idle and holds no task, and keeps the daemon's answer in compact_answer. After
# a resume the daemon's own relaunch delivery can start a turn a moment after the claim is ready
# (deliver_task), and the daemon refuses a compaction of a claim in a turn (CLAIM_IN_A_TURN) or
# between states (CLAIM_UNSETTLED); such a refusal is waited out and asked again, three times at
# most.
compact_answer=
compact_claim() {
  local token=$1 attempt
  for attempt in 1 2 3; do
    soft_wait 600 "$token to be ready or idle with no pending task" claim_is "$token" '(.state | IN("ready", "idle")) and .pending == null' || true
    if compact_answer=$(claims_cmd compact --claim "$token" 2>&1); then
      return 0
    fi
    case "$compact_answer" in
      *CLAIM_IN_A_TURN* | *CLAIM_UNSETTLED*) note "compaction $attempt of $token met a turn: $(tr '\n' ' ' <<<"$compact_answer")" ;;
      *) return 1 ;;
    esac
  done
  return 1
}
# settled_pr_comments NUMBER MARKER prints pr_comments NUMBER MARKER once GitHub's listing of the
# pull request's comments holds one that carries MARKER, read every 5 s for up to 60 s, or the last
# listing read when none does: the comment an answer's gh call just posted can be missing from the
# listing a moment longer than from the call's own answer. A check that must show a write was
# never made reads pr_comment_listing, never this.
settled_pr_comments() {
  local comments started=$SECONDS
  while :; do
    comments=$(pr_comments "$1" "$2")
    [ "$(jq length <<<"$comments")" = 0 ] && ((SECONDS - started < 60)) || break
    sleep 5
  done
  printf '%s\n' "$comments"
}

# answers_line_count prints how many lines of this boot's daemon log are the loop's (`answers:`).
answers_line_count() { boot_log | jq -s '[.[] | select((.msg // "") | startswith("answers:"))] | length'; }
# a7_loop_posted_nothing REPLY is the part of A7's running-agent leg that says the loop posted
# nothing: REPLY, what stands in the thread where the loop's answer would be, carries no mark of the
# loop's (`legion-answer:<id>`, which every answer the loop posts carries). The leg reads the asker's
# reply to its question; A7's control reads row A1's thread, the architect's reply on the ask, which
# the loop posted with its mark.
a7_loop_posted_nothing() {
  local mark
  mark=$(answer_question_id "${1:-}")
  read_said="the loop's mark in what the thread holds: ${mark:-none} ($(said_text "${1:-}"))"
  [ -z "$mark" ]
}
# a7_alone_read SENT NEEDLE LINES is the read of A7's first leg: the listener answered the send
# SENT (`<status> <event>`) 200, the architect's conversation holds NEEDLE, the question's own
# suffix, this boot logged no `answers:` line beyond LINES, and the asker's reply to the send's event
# holds no mark of the loop's (a7_loop_posted_nothing): the question reached the running agent alone,
# and the loop took nothing of it.
a7_alone_read() {
  local lines held=no quiet=no reply
  lines=$(answers_line_count)
  conversation_holds architect "$2" && held=yes
  reply=$(inbox_reply "${1#* }" 2>/dev/null) || reply=
  a7_loop_posted_nothing "$reply" && quiet=yes
  read_said="to the architect: listener $1; the daemon's answers lines $3 → $lines; its conversation holds the question: $held; the loop's mark in a reply on the asker's topic: $([ "$quiet" = yes ] && printf none || printf found)"
  [ "$lines" = "$3" ] && [ "${1%% *}" = 200 ] && [ "$held" = yes ] && [ "$quiet" = yes ]
}
# a7_nothing_posted EVENT [INBOX] is the read of A7's part that the loop posts nothing: no reply to
# EVENT reached the asker (no_reply_to), the question the loop dropped its answer to and forwarded.
# INBOX is the inbox as A7's control read it once (inbox_listing), read now where not given.
a7_nothing_posted() {
  read_said="replies on the asker's topic to ${1:-(no event)}: $(inbox_replies "${1:-}" "${2:-}")"
  no_reply_to "${1:-}" "${2:-}"
}
# a7_forwarded_read EVENT NEEDLE APPENDED is the read of A7's second leg: this boot dropped an
# answer of the architect's because its conversation started, and forwarded the question; the
# running architect's conversation holds NEEDLE, the question's own suffix; nothing was posted in
# answer to EVENT (a7_nothing_posted); and the conversation holds APPENDED exchanges still.
a7_forwarded_read() {
  local dropped=no held=no nothing=no appended
  answer_dropped "${agent[$store:architect.session]}" && dropped=yes
  conversation_holds architect "$2" && held=yes
  a7_nothing_posted "$1" && nothing=yes
  appended=$(appended_exchanges architect)
  read_said="the answer dropped and the question forwarded: $dropped; the running architect holds the question: $held; nothing posted in answer to ${1:-(no event)}: $nothing; the exchanges appended to its conversation $3 → $appended"
  [ "$dropped$held$nothing" = yesyesyes ] && [ "$appended" = "$3" ]
}
check_a7() {
  criterion A7 "a question to a running agent reaches it alone; a resume during generation appends nothing and forwards the question" || return 0
  local token=${agent[$store:architect.token]} label=daemon-$store before sent event n k appended_before appended_after first_ok=0 second_ok=0 said1 said2
  if ! resume_agent architect; then
    record_result A7 FAIL "the architect did not run again"
    return 0
  fi
  before=$(answers_line_count)
  sent=$(envoy_ask session "${agent[$store:architect.session]}" in-order-1)
  holds_first() { conversation_holds architect "question in-order-1 of run $run_tag"; }
  soft_wait 180 "the running architect's conversation to hold the question" holds_first || true
  sleep 20
  a7_alone_read "$sent" "question in-order-1 of run $run_tag" "$before" && first_ok=1
  said1=$read_said
  note "$said1"
  if ! suspend_agent architect; then
    record_result A7 FAIL "the architect was not suspended, or its session stayed live"
    return 0
  fi
  appended_before=$(appended_exchanges architect)
  # The loop's model call is the first call the proxy holds once the hold is placed, at whatever
  # ordinal it came: the records' count is only a floor of the ordinals the proxy has given
  # (hold_after_tool), so the call after it is not waited for by its number.
  n=$(proxy_ordinal "$label")
  k=$(held_count "$label")
  hold_at "$label" "$n"
  sent=$(envoy_ask session "${agent[$store:architect.session]}" in-order-2)
  event=${sent#* }
  if ! soft_wait 240 "the loop's model call to be held at the proxy" held_since "$label" "$k"; then
    release "$label"
    record_result A7 FAIL "the loop made no model call for the question to the stopped architect (listener $sent)"
    return 0
  fi
  note "the proxy holds the loop's model call for the question at $label ordinal $(held_since "$label" "$k")"
  claims_cmd resume --claim "$token" >/dev/null
  soft_wait 300 "the architect to start" claim_is "$token" '.state | IN("launching", "shim_connected", "registered", "ready", "working", "idle")' || true
  release "$label"
  dropped() { answer_dropped "${agent[$store:architect.session]}"; }
  delivered() { conversation_holds architect "question in-order-2 of run $run_tag"; }
  soft_wait 300 "the answer to be dropped and the question forwarded" dropped || true
  soft_wait 300 "the running architect to receive the forwarded question" delivered || true
  note "the loop: $(boot_log | jq -s -c '[.[] | select((.msg | startswith("answers:")) and (.session // "") == "'"${agent[$store:architect.session]}"'") | {msg, state, started}] | .[-6:]')"
  appended_after=$(appended_exchanges architect)
  a7_forwarded_read "$event" "question in-order-2 of run $run_tag" "$appended_before" && second_ok=1
  said2=$read_said
  note "$said2"
  if [ "$first_ok$second_ok" = 11 ]; then
    record_result A7 PASS "the running architect alone took the first question (no answers line); the second, its claim started mid-answer, was dropped and forwarded with no exchange appended (still $appended_after) and nothing posted"
  else
    record_result A7 FAIL "running agent alone $first_ok ($said1), started mid-answer forwarded with no exchange appended $second_ok ($said2) (1 = shown)"
  fi
  suspend_agent architect || true
}

# a8_once_read STORED SENT... is the read of A8's first leg: SENT..., the listener's answers
# (`<status> <event>`) to one envelope sent under one key, are each 200 and each name the first
# send's event; the stream stored one copy of them beyond STORED; one reply to that event reached
# the asker; and this boot named the reply's question in no line, a second hand-off of it among
# them: the daemon was handed the question once and answered it once.
a8_once_read() {
  local stored_before=$1 planner=${agent[$store:planner.session]} sent event codes='' events='' want='' same='' qid stored replies taken again
  shift
  event=${1#* }
  for sent in "$@"; do
    codes+="${codes:+ }${sent%% *}"
    events+="${events:+ }${sent#* }"
    want+="${want:+ }200"
    same+="${same:+ }$event"
  done
  qid=$(answer_question_id "$(inbox_reply "$event" 2>/dev/null || true)")
  again=$(answers_lines "answers: question handed off again; not answered again" "$planner")
  taken=$(question_lines "$qid")
  stored=$(($(listener_stored_handoffs_to "$planner") - stored_before))
  # shellcheck disable=SC2086 # each answered event is a word of its own, and an empty one none
  replies=$(replies_to $events)
  read_said="one envelope $# time(s) (listener $codes, answered events $events): hand-offs the stream stored for them $stored; replies on the asker's topic $replies; this boot's lines naming the question ${qid:-(none)}: $taken, of them handed off again $again"
  [ "$codes" = "$want" ] && [ -n "$event" ] && [ "$events" = "$same" ] && [ "$stored" = 1 ] && [ "$replies" = 1 ] &&
    [ -n "$qid" ] && [ "$taken" = 0 ] && [ "$again" = 0 ]
}
# a8_kill_read EVENT is the read of A8's second leg: one reply to EVENT reached the asker, across
# the kill just after it posted and the redelivery.
a8_kill_read() {
  local replies
  replies=$(inbox_replies "${1:-}")
  read_said="replies on the asker's topic to ${1:-(no event)}: $replies"
  [ -n "${1:-}" ] && [ "$replies" = 1 ]
}
a8_delivered_msg="answers: the loop's own answer is delivered into the asking agent's conversation"
# a8_own_delivered_read POSTED HANDED APPENDED QUESTION is the read of A8's third leg's delivery:
# the planner's answers posted this boot rose above POSTED, the newest of them to QUESTION; the
# listener's hand-offs to the replaced tester rose above HANDED; and the daemon delivered the
# answer into that tester's conversation once: one delivered line, one appended entry more than
# APPENDED, which holds the answer's own marker once.
a8_own_delivered_read() {
  local tester_old=${agent[$store:tester_old.session]} posted handed delivered appended marker
  posted=$(answers_lines "answers: question" "${agent[$store:planner.session]}" posted)
  handed=$(listener_handoffs_to "$tester_old")
  delivered=$(answers_lines "$a8_delivered_msg" "$tester_old")
  appended=$(appended_exchanges tester_old)
  marker=0
  [ -z "$4" ] || marker=$(occurrences "$(conversation_text tester_old 2>/dev/null || true)" "legion-answer:$4")
  read_said="the planner's answers posted $1 → $posted, the newest's question ${4:-(none)}; hand-offs to the replaced tester $2 → $handed, delivered lines $delivered; that conversation's appended entries $3 → $appended, holding the answer's marker $marker time(s)"
  [ "$posted" -gt "$1" ] && [ -n "$4" ] && [ "$handed" -gt "$2" ] && [ "$delivered" = 1 ] && [ "$marker" = 1 ] &&
    [ "$appended" = "$(($3 + 1))" ]
}
# a8_own_unanswered_read QUESTIONS REFUSED QUESTION [LINES] is the read of the third leg's part that
# says the loop answers nothing of its own answer: this boot logged no question of the replaced
# tester's beyond QUESTIONS and no answer of it, no model call's body holds QUESTION's marker, and
# nothing was passed to the controller as post_refused beyond REFUSED. LINES is this boot's log as
# A8's control read it once (boot_listing), read now where not given.
a8_own_unanswered_read() {
  local tester_old=${agent[$store:tester_old.session]} lines=${4:-} questions answers request=null refused
  [ -n "$lines" ] || lines=$(boot_listing)
  questions=$(answers_lines_in "$lines" "answers: question" "$tester_old")
  answers=$(answers_lines_in "$lines" "answers: answered" "$tester_old")
  [ -z "$3" ] || request=$(daemon_request_for "legion-answer:$3")
  refused=$(controller_questions post_refused) || refused=unread
  read_said="questions for the replaced tester $1 → $questions, answers of it $answers; a model call whose body holds the marker of ${3:-(no question)}: $([ "$request" = null ] && printf none || printf one); post_refused questions for the controller $2 → $refused"
  [ -n "$3" ] && [ "$questions" = "$1" ] && [ "$answers" = 0 ] && [ "$request" = null ] && [ "$refused" = "$2" ]
}
# a8_edit_unanswered_read QUESTIONS [LINES] is the read of A8's fourth leg: this boot logged no
# question of the architect's beyond QUESTIONS, so the person's edit of a comment was answered
# nothing. LINES is this boot's log as A8's control read it once, read now where not given.
a8_edit_unanswered_read() {
  local lines=${2:-} questions
  [ -n "$lines" ] || lines=$(boot_listing)
  questions=$(answers_lines_in "$lines" "answers: question" "${agent[$store:architect.session]}")
  read_said="questions for the architect $1 → $questions"
  [ "$questions" = "$1" ]
}
# a8_own_send TEXT sends TEXT to the planner as the replaced tester's session, as the planner's
# answer to that tester is what A8's third leg hands the daemon, and prints the listener's status.
a8_own_send() {
  envoy -o /dev/null -w '%{http_code}' --data "$(jq -cn --arg s "${agent[$store:planner.session]}" --arg from "${agent[$store:tester_old.session]}" --arg m "$1" \
    '{target_session: $s, source: "agent", source_session: $from, message: $m}')" "$(listener_url /v1/messages/send)"
}
check_a8() {
  criterion A8 "one question delivered twice, across a restart and across a kill just after its answer posted, gets one answer; the loop's own answer is delivered into its asker's conversation unanswered, and an edit is not answered" || return 0
  local planner=${agent[$store:planner.session]} key=a8-$run_tag key3=a8-kill-$run_tag sent resend resent event event3 stored_before posted_before own_before edit_before ok_twice=0 ok_kill=0 ok_own=0 ok_edit=0 said=''
  local tester_old handed_before appended_before calls_before refused_before own_qid
  # One envelope sent three times under one idempotency key, the third after a restart. The
  # listener hands each send off, and every hand-off carries the envelope's dedupe key as its
  # MsgId, so the stream stores one copy and answers each repeat with it: the second and third
  # sends are answered the first send's event (the sender's own repeat gets its first copy whole,
  # cmd/listener's answerHandedOff). The daemon is handed the question once and answers it once,
  # and the restarted daemon is handed nothing — no second hand-off for it to log as handed off
  # again, and no second answer. The replies are counted over the distinct events the sends were
  # answered, since all three name one (replies_to).
  stored_before=$(listener_stored_handoffs_to "$planner")
  sent=$(envoy_ask session "$planner" unanswerable "$key")
  event=${sent#* }
  resend=$(envoy_ask session "$planner" unanswerable "$key")
  soft_wait 240 "the answer" inbox_reply "$event" || true
  stop_daemon
  start_daemon "$daemon_bin" "$daemon_config" "$daemon_log" "$daemon_home" "$daemon_profile" "$daemon_plugin_dir"
  resent=$(envoy_ask session "$planner" unanswerable "$key")
  sleep 60
  a8_once_read "$stored_before" "$sent" "$resend" "$resent" && ok_twice=1
  note "the third across a restart: $read_said"
  said+="$read_said; "
  # A kill just after the answer posted: the asker's inbox is polled every tenth of a second and the
  # daemon killed the moment the reply lands, then the same envelope is delivered again.
  sent=$(envoy_ask session "$planner" unanswerable "$key3")
  event3=${sent#* }
  for _ in $(seq 1 3000); do
    [ "$(inbox_replies "$event3")" = 0 ] || break
    sleep 0.1
  done
  freeze_if "$daemon_pid" "${started_at[$daemon_pid]:-}" KILL || note "the daemon $daemon_pid was already gone before the kill"
  daemon_pid=
  note "killed the daemon $(inbox_replies "$event3") reply after its post; the question's last state before the kill: $(boot_log | jq -s -c --arg s "$planner" '[.[] | select(.msg == "answers: question" and .session == $s) | .state] | last')"
  start_daemon "$work/branch/legion" "$daemon_config" "$daemon_log" "$daemon_home" "$daemon_profile" "$daemon_plugin_dir"
  envoy_ask session "$planner" unanswerable "$key3" >/dev/null
  sleep 60
  note "after the kill and restart: this boot's lines for the planner: $(boot_log | jq -s -c --arg s "$planner" '[.[] | select((.msg | startswith("answers:")) and .session == $s) | {msg, state}]')"
  a8_kill_read "$event3" && ok_kill=1
  note "$read_said"
  said+="$read_said; "
  # The loop's own answer: a question to the planner sent as the stopped, replaced tester session.
  # The planner's answer goes back to that session in the planner's name; no live session holds
  # it, so the listener hands the answer to this daemon, which never answers an answer of its own
  # — no model call for it, no question of it, nothing posted, nothing passed to the controller —
  # and writes it into the stopped tester's conversation instead, once, as a message it received,
  # so that tester holds the answer when it runs again. The delivery is counted by the marker
  # entry the append ends with (appended_exchanges) and by the answer's own
  # `legion-answer:<question>` marker inside the entry, which only the delivered message carries.
  tester_old=${agent[$store:tester_old.session]}
  posted_before=$(answers_lines "answers: question" "$planner" posted)
  own_before=$(answers_lines "answers: question" "$tester_old")
  handed_before=$(listener_handoffs_to "$tester_old")
  appended_before=$(appended_exchanges tester_old)
  calls_before=$(proxy_count "daemon-$store")
  refused_before=$(controller_questions post_refused)
  a8_own_send "$(question_text repository-file) (question own-answer of run $run_tag)" >/dev/null
  soft_wait 300 "the planner's answer to the tester's session to post" more_than "$posted_before" answers_lines "answers: question" "$planner" posted || true
  own_qid=$(newest_question "$planner" posted)
  own_delivered() { [ "$(answers_lines "$a8_delivered_msg" "$tester_old")" -ge 1 ]; }
  soft_wait 300 "the planner's answer to reach the stopped tester's conversation" own_delivered || true
  sleep 60
  if a8_own_delivered_read "$posted_before" "$handed_before" "$appended_before" "$own_qid"; then
    note "$read_said"
    said+="$read_said; "
    a8_own_unanswered_read "$own_before" "$refused_before" "$own_qid" && ok_own=1
  fi
  note "$read_said; the daemon's model calls $calls_before → $(proxy_count "daemon-$store")"
  said+="$read_said; "
  # An edit: the person edits the comment A1's question was, on the open root issue. A closed
  # issue refuses every comment edit (409 ISSUE_CLOSED, comment_actions.go:19-31 lockedComment).
  if [ -n "${agent[$store:a1.comment]:-}" ]; then
    edit_before=$(answers_lines "answers: question" "${agent[$store:architect.session]}")
    dispatch_human PATCH "comments/${agent[$store:a1.comment]}" "$(jq -cn --arg b "$(question_text architect-decision) (edited)" '{body: $b}')" >/dev/null
    sleep 60
    a8_edit_unanswered_read "$edit_before" && ok_edit=1
    note "after the person edited comment ${agent[$store:a1.comment]}: $read_said"
    said+="$read_said"
  fi
  if [ "$ok_twice$ok_kill$ok_own$ok_edit" = 1111 ]; then
    record_result A8 PASS "one envelope sent three times under one key, the third across a restart: each send handed off, the stream stored one copy, one answer and nothing handed to the restarted daemon; killed just after the post and delivered again: one answer; the loop's own answer to the stopped tester was handed to the daemon, written into that tester's conversation once with no model call, no answer and nothing passed to the controller; an edit was not answered"
  else
    record_result A8 FAIL "twice and restart $ok_twice, kill after post $ok_kill, own answer delivered unanswered $ok_own, edit unanswered $ok_edit (1 = shown): $said"
  fi
}

# a9_read E1 E2 MARKER M NEEDLE is A9's read: the two questions in-order-1 and in-order-2, asked
# under MARKER (question_suffix), were answered E1 first; the architect's first request after its
# first M model calls (agents) holds each question once; its conversation holds one appended
# exchange for each answer's question; and every reply it gave after the last entry holding
# NEEDLE, the task it was resumed with, quotes both answers (after-resume's keys). Every part
# reads what followed those two questions alone: their events, their markers and their answers'
# questions.
a9_read() {
  local first q1 q2 request counts reply in_order=no
  first=$(first_reply_of "$1" "$2" 2>/dev/null) || first=
  [ -n "$first" ] && grep -qF "In Reply To: $1" <<<"$first" && in_order=yes
  q1=$(answer_question_id "$(inbox_reply "$1" 2>/dev/null || true)")
  q2=$(answer_question_id "$(inbox_reply "$2" 2>/dev/null || true)")
  request=$(first_request_after agents "$4")
  counts="question 1 $(question_occurrences "$request" in-order-1 "$run_tag" "$3"), question 2 $(question_occurrences "$request" in-order-2 "$run_tag" "$3"), exchange 1 $([ -z "$q1" ] && printf 0 || appended_exchanges architect "$q1"), exchange 2 $([ -z "$q2" ] && printf 0 || appended_exchanges architect "$q2")"
  # Every reply the architect gave since the task was delivered, not its last one: on resume it may
  # also answer a person's earlier reply in the same turn, and its last words then answer that.
  reply=$(assistant_texts_after architect "$5" 2>/dev/null) || reply=
  read_said="the first reply on the asker's topic answers question 1: $in_order; the resumed architect's first request (agents, after its first $4 model calls) and its conversation's exchanges (questions ${q1:-none} and ${q2:-none}): $counts; the resumed architect, since the task: $(said_text "$reply")"
  [ "$in_order" = yes ] && [ -n "$q1" ] && [ -n "$q2" ] && [ "$counts" = "question 1 1, question 2 1, exchange 1 1, exchange 2 1" ] &&
    answer_holds after-resume "$reply" >/dev/null
}
check_a9() {
  criterion A9 "two questions at once are answered in order, and the resumed agent holds both once and carries on from them" || return 0
  local token=${agent[$store:architect.token]} t1 t2 s1 s2 e1 e2 m ok=0
  t1="$(question_text in-order-1) $(question_suffix in-order-1 "$run_tag" A9)"
  t2="$(question_text in-order-2) $(question_suffix in-order-2 "$run_tag" A9)"
  s1=$(envoy_ask_text session "${agent[$store:architect.session]}" "$t1")
  s2=$(envoy_ask_text session "${agent[$store:architect.session]}" "$t2")
  e1=${s1#* }
  e2=${s2#* }
  both() { inbox_reply "$e1" && inbox_reply "$e2"; }
  soft_wait 300 "both answers" both || {
    record_result A9 FAIL "not both questions were answered (listener $s1; $s2)"
    return 0
  }
  note "answers: 1: $(head -c 200 <<<"$(inbox_message "$e1")" | tr '\n' ' ') | 2: $(head -c 200 <<<"$(inbox_message "$e2")" | tr '\n' ' ')"
  m=$(proxy_count agents)
  resume_agent architect || true
  deliver_task "$token" "$(question_text after-resume)"
  soft_wait 300 "the resumed architect's first model call" more_than "$m" proxy_count agents || true
  soft_wait 600 "the resumed architect's turn" claim_is "$token" '.state == "idle" and .pending == null' || true
  a9_read "$e1" "$e2" A9 "$m" "$(question_text after-resume)" && ok=1
  note "$read_said"
  suspend_agent architect || true
  if [ "$ok" = 1 ]; then
    record_result A9 PASS "answered one after the other; the resumed architect's first request held each question once, its conversation holds one appended exchange per question, and it quoted both answers"
  else
    record_result A9 FAIL "out of order, or the first request did not hold each question once, or the conversation does not hold one appended exchange per question, or the resumed agent did not carry on from them: $read_said"
  fi
}

# a10_gpt is A10's GPT-6.1 clause: the stopped merger, whose conversation ran on GPT-6.1 with
# reasoning and a tool call (produce_gpt_agent), is asked two questions within the cache lifetime,
# each read from the smoke repository with the gh tool, so an answer takes more than one model
# step; then, while no answer so far had a step that returned reasoning with a later step after
# it, the next question of the questions file's a10.reasoning list, each a new question that makes
# the model reason before a gh call (a10_next_question); and it is then resumed in its own harness.
# A step returns reasoning only when the model reasons, and the cache questions rarely make it:
# in proof run 44461333495 none of the eight steps of their four answers returned any
# (reasoning_tokens 0 in each), so (c) owed nothing. The list is three questions long, which is
# the leg's bound. Measured through the model gateway on 2026-11-19 with the daemon's own loop (a
# throwaway harness over answer.Answer on that run's merger conversation, its gh calls answered
# with the bytes go.mod holds), each of the three, asked after the two cache questions, had a step
# that returned reasoning with a later step after it in 34 of 34 answers, and each later request
# carried it; the two cache questions had one in 0 of 34 each. So the first of the list all but
# always ends the asking, and the other two stay for a model that reasons less on one kind of
# question: the first and the third make it reason before the answer's first gh call, the second
# between its two. A fourth would cost another answer's wait (up to gpt_answer_wait, 360 s) and
# another question the resume must hold, against a miss none of the three showed. It is judged
# from the recording proxy's records of each answer's own calls (answer_calls: the daemon's label,
# the question's marker and not the next question's) and from the daemon's `answers: answered`
# lines:
#   the conversation: its turns that did not fail hold a tool call (omp_openai_summary);
#   the model: every call of every answer is a POST /v1/responses for the conversation's model,
#   and each answered line names it (calls_on_model);
#   (a) every request carries `reasoning.effort` at the level the conversation last ran at, in
#   agentloop's words for it (omp_thinking_level, openai_effort, efforts_all);
#   (b) each of the two cache answers' first request carries every reasoning item the transcript
#   stores, its id and encrypted content byte for byte (reasoning_replayed), the transcript read
#   just before that question, so the second answer's holds the first answer's appended turns too;
#   (c) over every answer asked, every request after a step whose response returned reasoning
#   carries it as that response's `response.output_item.done` event holds it (reasoning_carried,
#   summed by carried_total); a call the provider cut for length (`response.incomplete`), which
#   the daemon made again sent the cut call's own request (agentloop's CallAgainWhenCut), owes
#   nothing, since by design no later request holds what it returned;
#   (d) the second answer, asked ten seconds after the first answer is posted, as the Claude Code
#   leg asks its second, reads the conversation from cache: its first call's own response reports
#   cached input tokens (call_cached_tokens), and its cost line a cache read (cache_read_tokens).
#   The line's count is summed over the answer's calls, and a later call of an answer reads what
#   an earlier call of the same answer wrote, so the line alone shows nothing of the first call;
#   (e) each reply is the loop's, carrying its mark, and holds its question's answer key
#   (loop_answer_holds): the merger, resumed after the answers, may answer a question itself;
#   (f) resumed on its own route (agents-gpt), the merger's first request is for the model and
#   holds every question asked once each.
# A transcript with no tool call, or with no reasoning item carrying encrypted content, which
# leaves (b) nothing to replay, and answers within the bound none of which had a step that
# returned reasoning followed by another, which leave (c) nothing to carry, each fail the clause
# and say what went unexercised, since a request carries every item of none. The asking is
# a10_gpt's and the judging a10_gpt_read's, which A10's control runs over a question of its own.
# check_a10 runs it on the daemon gpt_bound_leg boots for it, which answers the merger under
# gpt_answer_seconds, and each answer is waited for gpt_answer_wait.
a10_gpt() {
  local session=${agent[$store:gpt.session]:-} token=${agent[$store:gpt.token]:-}
  local id s e n m calls
  local -a ids=(gpt-repository-module gpt-repository-go) pairs=() carries=()
  # The conversation each cache answer's first request replays, kept as it was just before its
  # question: the first beside save_conversations' gpt.jsonl, the second ten seconds after the
  # first answer is posted.
  mkdir -p "$evidence/sessions/$store"
  conversation_text gpt >"$evidence/sessions/$store/gpt-before-A10-gpt-1.jsonl" 2>/dev/null || true
  n=0
  while :; do
    if [ "$n" -lt 2 ]; then
      id=${ids[$n]}
    else
      id=$(a10_next_question "$((n - 2))" "${carries[@]}")
      [ -n "$id" ] || break
      ids+=("$id")
    fi
    n=$((n + 1))
    if [ "$n" = 2 ]; then
      sleep 10
      conversation_text gpt >"$evidence/sessions/$store/gpt-before-A10-gpt-2.jsonl" 2>/dev/null || true
    fi
    s=$(envoy_ask_text session "$session" "$(question_text "$id") $(question_suffix "$id" "$run_tag" "A10-gpt-$n")")
    e=${s#* }
    pairs+=("$id=$e")
    note "GPT-6.1: A10-gpt-$n ($id): listener $s"
    soft_wait "$gpt_answer_wait" "the GPT-6.1 merger's answer to $id (A10-gpt-$n)" inbox_reply "$e" || true
    calls=$(answer_calls "daemon-$store" "A10-gpt-$n)" "A10-gpt-$((n + 1)))")
    carries+=("$(reasoning_carried "$calls")")
  done
  m=$(proxy_count agents-gpt)
  resume_agent gpt || true
  deliver_task "$token" "While you were stopped you were asked $n questions about go.mod. What did you answer each time?" || true
  soft_wait 300 "the resumed GPT-6.1 merger's first model call" more_than "$m" proxy_count agents-gpt || true
  soft_wait 600 "the resumed merger's turn" claim_is "$token" '.state == "idle" and .pending == null' || true
  if a10_gpt_read A10-gpt "$m" "${pairs[@]}"; then
    record_result A10 PASS "$read_pass_said; $gpt_bound_said"
  else
    record_result A10 FAIL "$read_said"
  fi
  suspend_agent gpt || true
}
# a10_gpt_read PREFIX M ID=EVENT... is the read of A10's GPT-6.1 clause over the answers to the
# questions ID..., asked in that order under the markers PREFIX-1), PREFIX-2), … (question_suffix)
# and answered to EVENT...: (a) to (f) above, from the conversations the two cache answers'
# first requests replayed, kept as sessions/<store>/gpt-before-<PREFIX>-1.jsonl and -2.jsonl, and
# from the merger's first request after its first M model calls on agents-gpt. Every answer's calls
# are read now that the last is posted, so that none is judged short of a call the proxy had not
# yet recorded when the leg decided whether to ask again. It sets read_said to what went wrong and
# read_pass_said to the row's PASS.
a10_gpt_read() {
  local prefix=$1 m=$2 slug k pair id event transcript='' second='' summary level want calls line reply missing request
  local replay1 replay2 rgot1 rof1 rmiss1 rgot2 rof2 rmiss2 carried owed cached1 cached2 counts='' want_counts='' exercised=''
  local unexercised='' uncarried='' unreplayed='' unkeyed='' ok_tools=0 ok_model=1 ok_effort=1 ok_replay=0 ok_carried=0 ok_cache=0 ok_keys=1 ok_resume=0
  local -a ids=() answers=() lines=() carries=()
  shift 2
  slug=${prefix// /-}
  [ ! -f "$evidence/sessions/$store/gpt-before-$slug-1.jsonl" ] || transcript=$(<"$evidence/sessions/$store/gpt-before-$slug-1.jsonl")
  [ ! -f "$evidence/sessions/$store/gpt-before-$slug-2.jsonl" ] || second=$(<"$evidence/sessions/$store/gpt-before-$slug-2.jsonl")
  summary=$(omp_openai_summary "$transcript")
  level=$(omp_thinking_level "$transcript")
  want=$(openai_effort "$level")
  note "GPT-6.1: the merger's conversation before $prefix-1 last ran at thinking level \"$level\", effort \"$want\" in OpenAI's words; its turns $summary"
  if [ "$(jq -r .tool_calls <<<"$summary")" -gt 0 ]; then
    ok_tools=1
  else
    unexercised="the GPT-6.1 transcript holds no tool call, which the conversation answered must hold"
  fi
  if [ "$(jq -r .encrypted <<<"$summary")" = 0 ]; then
    unexercised+="${unexercised:+; }"
    if [ "$(jq -r .reasoning <<<"$summary")" = 0 ]; then
      unexercised+="the GPT-6.1 transcript holds no reasoning item"
    else
      unexercised+="the GPT-6.1 transcript's $(jq -r .reasoning <<<"$summary") reasoning items carry no encrypted content"
    fi
    unexercised+=", so the clause that the replay carries each reasoning item byte for byte is unexercised"
  fi
  k=0
  for pair in "$@"; do
    k=$((k + 1))
    id=${pair%%=*}
    event=${pair#*=}
    ids+=("$id")
    reply=$(inbox_reply "$event" 2>/dev/null) || reply=
    calls=$(answer_calls "daemon-$store" "$prefix-$k)" "$prefix-$((k + 1)))")
    answers+=("$calls")
    carries+=("$(reasoning_carried "$calls")")
    line=$(answered_line "$(answer_question_id "$reply")") || line=
    lines+=("$line")
    note "GPT-6.1: $prefix-$k ($id): event ${event:-(none)}; its calls on daemon-$store $(jq -c '[.[] | {ordinal, path, model: .request_body.model, status}]' <<<"$calls"), efforts $(calls_efforts "$calls") (want \"$want\"); of the reasoning its earlier steps returned, its later requests carried ${carries[k - 1]} (carried, owed); answered line $(jq -c '{model, calls, cacheReadTokens, cacheWriteTokens, costUSD}' <<<"${line:-null}"); reply: $(said_text "$reply")"
    calls_on_model "$calls" /v1/responses "$gpt_model_id" && [ "$(jq -r '.model // ""' <<<"${line:-null}")" = "$gpt_model" ] || ok_model=0
    efforts_all "$calls" "$want" || ok_effort=0
    if ! missing=$(loop_answer_holds "$id" "$reply"); then
      ok_keys=0
      unkeyed+="${unkeyed:+; }the reply to $prefix-$k ($id) lacks \"$missing\""
    fi
    [ "${carries[k - 1]#* }" = 0 ] || exercised+="${exercised:+, }$prefix-$k ($id, ${carries[k - 1]% *} of ${carries[k - 1]#* } carried)"
  done
  replay1=$(reasoning_replayed "$transcript" "$(jq -c '.[0].request_body // null' <<<"${answers[0]:-[]}")")
  replay2=$(reasoning_replayed "$second" "$(jq -c '.[0].request_body // null' <<<"${answers[1]:-[]}")")
  cached1=$(call_cached_tokens "$(jq -c '.[0] // null' <<<"${answers[0]:-[]}")")
  cached2=$(call_cached_tokens "$(jq -c '.[0] // null' <<<"${answers[1]:-[]}")")
  note "GPT-6.1: of the transcript's reasoning items each cache answer's first request carried $replay1 and $replay2 (carried, stored, missing); each cache answer's first call read $cached1 and $cached2 cached input tokens (its response's usage)"
  read -r rgot1 rof1 rmiss1 <<<"$replay1"
  read -r rgot2 rof2 rmiss2 <<<"$replay2"
  [ "$rof1" -gt 0 ] && [ "$rgot1" = "$rof1" ] && [ "$rof2" -gt 0 ] && [ "$rgot2" = "$rof2" ] && ok_replay=1
  [ -z "$rmiss1" ] || unreplayed="the first answer's first request lacks the stored reasoning items $rmiss1"
  [ -z "$rmiss2" ] || unreplayed+="${unreplayed:+; }the second answer's first request lacks the stored reasoning items $rmiss2"
  read -r carried owed <<<"$(carried_total "${carries[@]}")"
  if [ "$owed" = 0 ]; then
    uncarried="no step of the $k answers asked (the two cache questions and $((k > 2 ? k - 2 : 0)) of the questions file's a10.reasoning list, which is the bound) that returned reasoning was followed by another step, so the clause that a later step carries an earlier step's reasoning is unexercised"
  elif [ "$carried" = "$owed" ]; then
    ok_carried=1
  else
    uncarried="later requests carried $carried of the $owed pairs of a reasoning item an earlier step returned and a later request of its answer: $exercised"
  fi
  [ "$(cache_read_tokens "${lines[1]:-}")" -gt 0 ] && [ "$cached2" -gt 0 ] && ok_cache=1
  request=$(first_request_after agents-gpt "$m")
  for ((k = 1; k <= ${#ids[@]}; k++)); do
    counts+="${counts:+ }$(question_occurrences "$request" "${ids[k - 1]}" "$run_tag" "$prefix-$k")"
    want_counts+="${want_counts:+ }1"
  done
  note "GPT-6.1: the resumed merger's first request (agents-gpt, after its first $m model calls), for $(jq -r '.model // "no model"' <<<"$request"), holds each of the ${#ids[@]} questions this many times: $counts"
  [ "$counts" = "$want_counts" ] && [ "$(jq -r '.model // ""' <<<"$request")" = "$gpt_model_id" ] && ok_resume=1
  read_said="GPT-6.1: a tool call in the conversation $ok_tools, on the model $ok_model, effort $ok_effort, the transcript's reasoning replayed $ok_replay, earlier steps' reasoning carried $ok_carried (${carried:-0} of ${owed:-0} over ${#ids[@]} answers), the second answer read from cache $ok_cache (its first call $cached2 tokens, its cost line $(cache_read_tokens "${lines[1]:-}")), answer keys $ok_keys, resumed holding each question once $ok_resume (1 = shown)${unexercised:+; $unexercised}${unreplayed:+; $unreplayed}${uncarried:+; $uncarried}${unkeyed:+; $unkeyed}"
  read_pass_said="GPT-6.1: a conversation with $(jq -r .tool_calls <<<"$summary") tool call(s) answered ${#ids[@]} times on $gpt_model_id through /v1/responses (${ids[*]}), every request at effort $want, the level it last ran at; each cache answer's first request carried every reasoning item its transcript stored ($rof1, then $rof2) with its encrypted content byte for byte, and each later step the reasoning an earlier step returned, all $owed carried byte for byte ($exercised); the second answer's first call read $cached2 tokens from cache, its cost line $(cache_read_tokens "${lines[1]:-}"); every reply holds its keys; resumed in Oh My Pi on $gpt_model_id, its first request holding each question once"
  [ "$ok_tools$ok_model$ok_effort$ok_replay$ok_carried$ok_cache$ok_keys$ok_resume" = 11111111 ]
}

# a10_claude_read E1 E2 MARK1 MARK2 LINE M TRANSCRIPT is the read of A10's Claude Code leg: the
# replies to E1 and E2, the pre-change reviewer's questions asked under MARK1 and MARK2
# (question_suffix), are the loop's answers, each carrying its mark, and hold pre-change-scan's
# keys (loop_answer_holds: the reviewer, resumed after them, may answer the questions itself); the
# loop's first request for MARK1 carries the effort TRANSCRIPT last ran at and every thinking
# signature it holds, byte for byte; LINE, the cost line of the answer one organization served
# after another, read the conversation from cache; and the resumed reviewer's first request after
# its first M model calls (claude-<store>) holds the two questions once each. TRANSCRIPT is the
# file the conversation was kept in just before MARK1's question
# (sessions/<store>/pre_change-before-<MARK1>.jsonl), never the conversation as it stands: the
# answers the loop gave since, and the resumed reviewer's turn, write thinking signatures into it
# (conversation/claude's AppendFile) that no request of the first answer could hold. It sets
# read_pass_said to the row's PASS words for what it judged.
a10_claude_read() {
  local transcript='' want sigs request effort missing=0 sig resumed counts keys=no lacks=''
  [ ! -f "$7" ] || transcript=$(<"$7")
  want=$(transcript_effort "$transcript")
  sigs=$(transcript_signatures "$transcript")
  request=$(daemon_request_for "$3)")
  effort=$(request_effort "$request")
  while IFS= read -r sig; do
    [ -z "$sig" ] || grep -qF -- "$sig" <<<"$request" || missing=$((missing + 1))
  done <<<"$sigs"
  resumed=$(first_request_after "claude-$store" "$6")
  counts="$(question_occurrences "$resumed" pre-change-scan "$run_tag" "$3") $(question_occurrences "$resumed" pre-change-scan "$run_tag" "$4")"
  lacks=$(loop_answer_holds pre-change-scan "$(inbox_reply "$1" 2>/dev/null || true)") &&
    lacks=$(loop_answer_holds pre-change-scan "$(inbox_reply "$2" 2>/dev/null || true)") && keys=yes
  read_said="Claude Code: both answers the loop's and holding their keys: $keys${lacks:+ (one lacks \"$lacks\")}; the transcript's last effort \"$want\", the request effort of the answer to $3) \"$effort\"; its thinking signatures $(grep -c . <<<"$sigs" || true), missing from that request $missing; the cache line read $(cache_read_tokens "$5") tokens; the resumed reviewer's first request (claude-$store, after its first $6 model calls) holds the two questions: $counts"
  read_pass_said="Claude Code: effort \"$effort\" and each of the transcript's $(grep -c . <<<"$sigs" || true) thinking signatures replayed"
  [ "$keys" = yes ] && [ -n "$want" ] && [ "$effort" = "$want" ] && [ -n "$sigs" ] && [ "$missing" = 0 ] &&
    [ "$(cache_read_tokens "$5")" -gt 0 ] && [ "$counts" = "1 1" ]
}
# a10_compaction_read E3 MARKER M is the read of A10's compaction leg: the architect's conversation
# holds a compaction written as text; the reply to E3, the question asked under MARKER after it, is
# the loop's answer, carrying its mark, and holds in-order-2's keys (loop_answer_holds: the
# architect, resumed after it, may answer the question itself); and the resumed architect's first
# request after its first M model calls (agents) holds that question once.
a10_compaction_read() {
  local compactions reply lacks held=no request after
  compactions=$(text_compactions architect 2>/dev/null) || compactions=0
  reply=$(inbox_reply "$1" 2>/dev/null) || reply=
  lacks=$(loop_answer_holds in-order-2 "$reply") && held=yes
  request=$(first_request_after agents "$3")
  after=$(question_occurrences "$request" in-order-2 "$run_tag" "$2")
  read_said="compactions written as text in the architect's conversation: ${compactions:-0}; the answer from the compacted conversation: $(said_text "$reply"), the loop's and holding its keys: $held${lacks:+ (it lacks \"$lacks\")}; the resumed architect's first request (agents, after its first $3 model calls) holds the question asked after the compaction $after time(s)"
  [ "${compactions:-0}" -gt 0 ] 2>/dev/null && [ "$held" = yes ] && [ "$after" = 1 ]
}

# check_a10: A10's GPT-6.1 clause (a10_gpt, on the daemon gpt_bound_leg boots for it, where the
# model route serves the model), its Claude Code leg (the pre-change reviewer, answered twice
# within the cache lifetime: the first request carries the effort the transcript last ran at and
# every thinking signature it holds, byte for byte; an answer of a pair one Anthropic organization
# served reads the conversation from cache; resumed after, its first request holds both exchanges)
# and its compaction leg (the architect's session compacted by the daemon's own compaction, is
# answered from the compacted conversation, and resumes after holding the exchange). The
# byte-for-byte clause needs thinking to replay, which is why the pre-change reviewer runs at a
# raised effort (produce_pre_change); a transcript with no thinking signature fails the leg and
# says why, since any request carries every signature of a transcript that holds none.
check_a10() {
  if criterion A10 "a GPT-6.1 conversation with reasoning and a tool call is answered by that model at its effort, its reasoning replayed and carried byte for byte, its second answer read from cache, and it resumes in Oh My Pi" openai; then
    if [ -n "${agent[$store:gpt.why]:-}" ] || [ -z "${agent[$store:gpt.session]:-}" ]; then
      record_result A10 FAIL "GPT-6.1: ${agent[$store:gpt.why]:-the merger registered no session}"
    else
      agent[$store:a10.gpt]=1
      gpt_bound_leg "A10's GPT-6.1 clause" a10_gpt
    fi
  fi
  criterion A10 "a Claude Code worker and a compacted Oh My Pi conversation are answered from their conversations and resume after" || return 0
  local session=${agent[$store:pre_change.session]} token=${agent[$store:pre_change.token]} t1 t2 s1 s2 e1 e2 line transcript sigs thinking unexercised='' m ok_claude=0 ok_compact=0 t3 s3 e3 compaction said1 said2 claude_said
  local before=$evidence/sessions/$store/pre_change-before-A10-1.jsonl
  local asked_at k marker again pair='' pairs='' cache_why='' found judged=''
  local -a markers events organizations
  # The conversation as it stands before the first question, kept as A10's GPT-6.1 clause keeps its own:
  # the first request must carry the signatures this holds, and not those the answers below add to it.
  mkdir -p "$evidence/sessions/$store"
  conversation_text pre_change >"$before"
  transcript=$(<"$before")
  sigs=$(transcript_signatures "$transcript")
  thinking=$(transcript_thinking "$transcript")
  if [ -z "$sigs" ]; then
    if [ "$thinking" = 0 ]; then
      unexercised="the pre-change transcript holds no thinking"
    else
      unexercised="the pre-change transcript's $thinking thinking blocks carry no signature"
    fi
    unexercised+=", so the clause that the replay carries each thinking signature byte for byte is unexercised"
    note "$unexercised: the Claude Code leg fails"
  fi
  t1="$(question_text pre-change-scan) $(question_suffix pre-change-scan "$run_tag" A10-1)"
  t2="$(question_text pre-change-scan) $(question_suffix pre-change-scan "$run_tag" A10-2)"
  asked_at=$SECONDS
  s1=$(envoy_ask_text session "$session" "$t1")
  e1=${s1#* }
  soft_wait 240 "the first answer" inbox_reply "$e1" || true
  # The second question waits for the first answer to be posted and then ten seconds more. A cache
  # entry is not always readable the moment it is written: in the run of 2026-11-08 this check's
  # second call (daemon-files ordinal 35) read 0 tokens of the 2,125 its first (ordinal 34) had
  # written 2.5 s earlier, while calls of that run further apart did read what an earlier call
  # wrote (ordinal 20 read the 1,707 ordinal 18 wrote, 5 s before it). The cache-read clause below
  # judges the loop's cache policy, so it must not turn on the provider's propagation delay.
  sleep 10
  s2=$(envoy_ask_text session "$session" "$t2")
  e2=${s2#* }
  soft_wait 240 "the second answer" inbox_reply "$e2" || true
  # Nor must it turn on which Anthropic organization served each call. A prompt cache is kept per
  # organization, and the model gateway of proof run 44035756045 sent the two answers' calls to two
  # of them (the `anthropic-organization-id` of each proxied answer, call_organization), so the
  # second read 0 tokens. So the clause is judged on the first answer whose organization an
  # earlier answer's also was (same_organization_pair), whatever was served between them: in proof
  # run 44095624771 six answers strictly alternated two organizations and each read what its own
  # organization's answer before it wrote, so a pairing of neighbours alone found none. While no
  # two answers share an organization, the second question is asked again under a marker of its
  # own, `A10-2r<N>)`, which holds no `A10-2)`, so the resume's counts still read the first two
  # questions. Each is asked once the answer before it is posted and ten seconds more, while the
  # first answer's cache entry can still be alive (five minutes from its request, counted here from
  # before its question was sent), so every answer of the leg is inside the cache's lifetime, at
  # most four times. Answers whose calls name no organization at all (a route that sends no such
  # header) share one, as before.
  markers=("A10-1)" "A10-2)")
  events=("$e1" "$e2")
  while :; do
    k=$((${#markers[@]} - 1))
    organizations=()
    pairs=
    for marker in "${markers[@]}"; do
      organizations+=("$(call_organization "$(daemon_call_for "$marker")")")
      pairs+="${pairs:+, }$marker ${organizations[-1]:-none}"
    done
    note "the answers so far were served by the organizations: $pairs"
    if found=$(same_organization_pair "${organizations[@]}"); then
      judged=${found#* }
      pair="${markers[${found% *}]} and ${markers[judged]}"
      break
    fi
    if ! inbox_reply "${events[k]}" >/dev/null; then
      cache_why="the answer to ${markers[k]} was not posted, so the second question was not asked again (the answers' organizations: $pairs)"
      break
    fi
    if [ "$k" -gt 4 ] || ((SECONDS + 10 - asked_at >= 300)); then
      cache_why="the model gateway served no two answers from one organization ($pairs), within the cache lifetime and four asks again, and a prompt cache is kept per organization, so no cache read was observed"
      break
    fi
    sleep 10
    marker="A10-2r$k)"
    again=$(envoy_ask_text session "$session" "$(question_text pre-change-scan) (question pre-change-scan of run $run_tag, $marker")
    markers+=("$marker")
    events+=("${again#* }")
    soft_wait 240 "the answer to the second question asked again ($marker" inbox_reply "${events[k + 1]}" || true
  done
  line=
  if [ -n "$pair" ]; then
    line=$(answered_line "$(answer_question_id "$(inbox_reply "${events[judged]}" || true)")") || line=
    note "the cache-read clause reads the cost line of the answer to ${markers[judged]}, the later of the first two answers one organization served ($pair): ${line:-none}"
  else
    note "the cache-read clause has no pair to read: $cache_why"
  fi
  note "answers: $(head -c 200 <<<"$(inbox_reply "$e1")" | tr '\n' ' ') | $(head -c 200 <<<"$(inbox_reply "$e2")" | tr '\n' ' ')"
  # The first request's effort and every thinking signature are judged by a10_claude_read, from the
  # conversation kept above.
  m=$(proxy_count "claude-$store")
  resume_agent pre_change || true
  deliver_task "$token" "While you were stopped you were asked what blocked the release. How many times were you asked, and what did you answer?"
  soft_wait 300 "the resumed Claude Code reviewer's first model call" more_than "$m" proxy_count "claude-$store" || true
  soft_wait 600 "the resumed reviewer's turn" claim_is "$token" '.state == "idle" and .pending == null' || true
  a10_claude_read "$e1" "$e2" A10-1 A10-2 "$line" "$m" "$before" && ok_claude=1
  claude_said=$read_pass_said
  said1=$read_said
  note "$said1"
  suspend_agent pre_change || true
  # The compaction leg: the architect's session compacted as Legion compacts one, by the daemon's
  # own compaction (`legion claims compact`, packages/daemon/internal/api/compact.go), which has Oh
  # My Pi write the summary as text to instructions of its own (compactInstructions). A compaction
  # that carries no instructions, a person's bare `/compact` among them, is Oh My Pi's image-based
  # snapcompact under the pinned release, and the loop's reader declines that one: the question
  # after it is passed on, not answered (compactionSummary, conversation/omp/context.go). So the
  # leg waits for a compaction whose summary is text (text_compactions) and counts no other kind.
  resume_agent architect || true
  if compact_claim "${agent[$store:architect.token]}"; then compaction=$compact_answer; else compaction="refused: $compact_answer"; fi
  note "legion claims compact: $(tr '\n' ' ' <<<"$compaction")"
  compacted() { [ "$(text_compactions architect)" -gt 0 ]; }
  soft_wait 300 "the architect's conversation to hold a compaction written as text" compacted || true
  note "the architect's conversation holds a compaction written as text: $(compacted && echo yes || echo no) ($(occurrences "$(conversation_text architect)" '"type":"compaction"') compaction entries of any kind)"
  suspend_agent architect || true
  t3="$(question_text in-order-2) $(question_suffix in-order-2 "$run_tag" "A10 after compaction")"
  s3=$(envoy_ask_text session "${agent[$store:architect.session]}" "$t3")
  e3=${s3#* }
  soft_wait 240 "the answer from the compacted conversation" inbox_reply "$e3" || true
  m=$(proxy_count agents)
  resume_agent architect || true
  deliver_task "${agent[$store:architect.token]}" "Reply with one word: ready."
  soft_wait 300 "the resumed architect's first model call" more_than "$m" proxy_count agents || true
  a10_compaction_read "$e3" "A10 after compaction" "$m" && ok_compact=1
  said2=$read_said
  note "$said2"
  suspend_agent architect || true
  if [ "$ok_claude$ok_compact" = 11 ]; then
    record_result A10 PASS "$claude_said, the answer to ${markers[judged]}, the later of the pair $pair one organization served, read $(cache_read_tokens "$line") tokens from cache, resumed holding both; the compacted conversation answered and resumed holding the exchange"
  else
    record_result A10 FAIL "Claude Code leg $ok_claude ($said1), compaction leg $ok_compact ($said2) (1 = shown)${unexercised:+; $unexercised}${cache_why:+; $cache_why}"
  fi
}

# a12_stand_in FAULT writes the recording proxy's stand-in reply for the leg that needs one, the
# daemon's own label answered with a reply of the proof's own, and removes it for a leg that needs
# none. Two legs need one, each a model step no real model gives on demand:
#   malformed — a reply that is not the form the daemon checks;
#   model_calls — a first reply that calls the gh tool, so the one call the bound allows still
#   called a tool (agentloop's FaultCalls, "the last of 1 model calls still called a tool", the
#   daemon's model_calls). A real question cannot be trusted to reach that bound: the loop's own
#   prompt tells the model its last call offers no tool and must be the reply, and the go.mod
#   question these legs ask is the one A15 had the planner answer earlier in the pass, which the
#   loop answered again from the conversation in one call with no tool (`calls: 1`, `fault: ""`,
#   the answer posted) in the run of 2026-11-08. The tool call is never run: the loop reads the
#   bound off the reply that made it. Both replies report usage, since an answer the provider
#   meters nothing for is the `unmetered` fault instead.
# A23 uses the same file for its fourth search (four_searches): a reply that calls the web_search
# tool four times in one step. The stand-in answers every call of the label, the daemon's three
# searches it lets run among them, which read no text and no source in it and fail; the fourth is
# refused before any call, which is what A23 reads.
a12_stand_in() {
  local file=$work/proxy-stand-in/daemon-$store.json
  case "$1" in
    malformed)
      jq -n '{type: "message", role: "assistant", content: [{type: "text", text: "not the reply form"}], stop_reason: "end_turn", usage: {input_tokens: 10, output_tokens: 5}}' >"$file"
      ;;
    model_calls)
      jq -n --arg repo "$repo" '{type: "message", role: "assistant", content: [{type: "tool_use", id: "toolu_a12_model_calls", name: "gh", input: {args: ["api", "repos/\($repo)/commits/main"]}}], stop_reason: "tool_use", usage: {input_tokens: 10, output_tokens: 5}}' >"$file"
      ;;
    four_searches)
      jq -n '{type: "message", role: "assistant", content: [range(1; 5) as $n | {type: "tool_use", id: "toolu_a23_search_\($n)", name: "web_search", input: {query: "newest jq release, search \($n)"}}], stop_reason: "tool_use", usage: {input_tokens: 10, output_tokens: 5}}' >"$file"
      ;;
    *) rm -f "$file" ;;
  esac
}

# a12_leg_read FAULT EVENT is the read of one leg of A12: this boot passed a question to the
# controller for a reason naming FAULT, the daemon's own reply to EVENT reached the asker, and that
# reply is not the stand-in's out-of-form text (a12_not_posted).
a12_leg_read() {
  local passed=no reply
  fault_passed_on "$1" && passed=yes
  reply=$(inbox_reply "$2" 2>/dev/null) || reply=
  read_said="$1: passed to the controller: $passed; the reply to ${2:-(no event)}: $(said_text "$reply")"
  [ "$passed" = yes ] && [ -n "$reply" ] && a12_not_posted "$2"
}
# a12_not_posted EVENT [INBOX] is A12's part that says a reply out of form is not posted: the reply
# to EVENT on the asker's topic does not carry the stand-in's own out-of-form text. Its control
# places such a reply there, carrying the loop's mark, and this read, over the inbox it read once
# (INBOX, inbox_listing; read now where not given), must see it.
a12_not_posted() {
  local reply
  reply=$(inbox_reply "${1:-}" "${2:-}" 2>/dev/null) || reply=
  ! grep -qF 'not the reply form' <<<"$reply"
}
# a12_calls_read EVENT is the read of A12's model_calls leg: the `answers: answered` line of the
# question the reply to EVENT answers carries fault model_calls after its one call.
a12_calls_read() {
  local answered
  answered=$(answered_line "$(answer_question_id "$(inbox_reply "${1:-}" 2>/dev/null || true)")") || answered=
  read_said="the model_calls leg's answered line: ${answered:-none}"
  answered_fault_is "$answered" model_calls 1
}
check_a12() {
  criterion A12 "a reply past a bound or out of form is not posted; the daemon replies naming the fault and passes the question on" || return 0
  local entry bounds fault sent event passed=0 ok_calls=0 said=''
  for entry in "{ model_calls: 1 }|model_calls" "{ answer_seconds: 1 }|answer_seconds" "{ output_tokens: 16 }|length" "{}|malformed" "{ daily_usd: 0.01 }|over_limit"; do
    bounds=${entry%|*}
    fault=${entry#*|}
    stop_daemon
    write_legion_config "$work/a12-$store.yaml" yes "$bounds"
    a12_stand_in "$fault"
    start_daemon "$work/branch/legion" "$work/a12-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
    sent=$(envoy_ask session "${agent[$store:planner.session]}" repository-file "a12-$fault-$run_tag")
    event=${sent#* }
    passed_on() { fault_passed_on "$fault"; }
    soft_wait 300 "the $fault fault to be passed on" passed_on && soft_wait 60 "the daemon's reply" inbox_reply "$event" || true
    a12_leg_read "$fault" "$event" && passed=$((passed + 1))
    note "$read_said"
    said+="$read_said; "
    if [ "$fault" = model_calls ]; then
      a12_calls_read "$event" && ok_calls=1
      note "$read_said"
      said+="$read_said; "
    fi
    a12_stand_in none
  done
  stop_daemon
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
  if [ "$passed" = 5 ] && [ "$ok_calls" = 1 ]; then
    record_result A12 PASS "each of model_calls, answer_seconds, length, malformed and over_limit ended with the daemon's own reply and the question passed on; the model_calls leg's first reply called the gh tool, so the one model call the bound allows still called a tool (answers: answered, fault model_calls, calls 1)"
  else
    record_result A12 FAIL "$passed of 5 faults ended as they must; the model_calls leg's answered line carries that fault after its one call: $ok_calls (1 = shown): $said"
  fi
}

# a14_read LINES is A14's read: LINES, the `answers: answered` lines (one a line) of answers with
# no fault, are at least one, and every one of them recomputes from its tokens at its model's
# prices plus its web searches' cost (bad_cost_lines), at a model the run prices
# (unpriced_cost_lines).
a14_read() {
  local bad unpriced
  bad=$(bad_cost_lines "$1")
  unpriced=$(unpriced_cost_lines "$1")
  read_said="cost lines $(grep -c . <<<"$1" || true), of them with web searches $(jq -s '[.[] | select((.webSearches // 0) > 0)] | length' <<<"$1" 2>/dev/null || printf 0); not recomputing: $bad; of a model with no price: $unpriced"
  [ -n "$1" ] && [ "$bad" = "[]" ] && [ "$unpriced" = "[]" ]
}
check_a14() {
  criterion A14 "a cost line per answer, cache writes and web searches included, recomputed from the captured usage and the prices" || return 0
  local lines
  # Every boot of the run, not this one: A12 restarts the daemon five times before this check, and
  # its own boot has answered nothing yet. A line of a model the read has no price for is one whose
  # cost nothing recomputes, and fails the criterion rather than passing unread. A23 runs before
  # it, so the lines hold answers that searched the web, whose webSearchUSD is part of the cost.
  lines=$(run_log | jq -c 'select(.msg == "answers: answered" and .fault == "")')
  note "cost lines this run: $(jq -s -c 'map({question, model, inputTokens, outputTokens, cacheWriteTokens, cacheReadTokens, webSearches, webSearchUSD, costUSD})' <<<"$lines")"
  if a14_read "$lines"; then
    record_result A14 PASS "every answer's costUSD equals its tokens at the configured prices of its model, Claude Sonnet 5.5 or GPT-6.1, cache writes included, plus its web searches' cost, the per-search fee included ($read_said; every boot of the run)"
  else
    record_result A14 FAIL "no cost line, or a cost that does not recompute: $read_said"
  fi
}

# check_a15 asks each of the two stopped agents to read go.mod at a fixed commit of the smoke
# repository, which neither can answer from its own conversation: the loop must call the gh tool,
# as the review App for the repository's owner, and the answer must come back with the file's
# words. That holds only while this is the first time the pass asks it — A8's own-answer leg and
# A12's five legs ask the same question later, and a conversation that already holds the file is
# one the loop may answer with no call at all — so the matrix runs this check before them. Its
# read, a15_read, reads each conversation after the question's own suffix only, so A15's control,
# whose question the conversation never received, does not find A15's own answer there.
# a15_one NAME EVENT AFTER is a15_read's read of one agent: its reply to EVENT holds
# repository-file's keys, one `answers: gh` line of the reply's question is the review App's call
# on owner sjawhar that exited 0, and NAME's conversation holds the file's module path after the
# entry holding AFTER, the question's own suffix. It adds what it read to read_said.
a15_one() {
  local reply question lines keys=no gh=no held=no
  reply=$(inbox_reply "$2" 2>/dev/null) || reply=
  replied_with_keys "$2" repository-file >/dev/null && keys=yes
  question=$(answer_question_id "$reply")
  lines=$(question_gh_lines "$question") || lines='[]'
  first_line_where "$lines" gh_line_is review sjawhar 0 >/dev/null && gh=yes
  conversation_holds_after "$1" stage4b-fixture "$3" && held=yes
  read_said+="${read_said:+; }$1: its reply holds the keys $keys; the gh lines of its answer ${question:-(none)}: $lines, one the review App's call on sjawhar that exited 0 $gh; its conversation holds the file after the question $held"
  [ "$keys$gh$held" = yesyesyes ]
}
# a15_read PLANNER_EVENT REVIEWER_EVENT SUFFIX is A15's read: the planner answered PLANNER_EVENT
# and the Claude Code reviewer REVIEWER_EVENT from go.mod read with gh (a15_one), each question
# asked with SUFFIX.
a15_read() {
  local ok=0
  read_said=
  a15_one planner "$1" "$3" || ok=1
  a15_one pre_change "$2" "$3" || ok=1
  return "$ok"
}
check_a15() {
  criterion A15 "an answer read from the repository with gh, the log naming App and owner, the call in the conversation; the same for the Claude Code worker" || return 0
  local planner_event reviewer_event
  ask_and_expect "planner, repository file" session "${agent[$store:planner.session]}" repository-file || true
  planner_event=$asked_event
  ask_and_expect "Claude Code reviewer, repository file" session "${agent[$store:pre_change.session]}" repository-file || true
  reviewer_event=$asked_event
  if a15_read "$planner_event" "$reviewer_event" "(question repository-file of run $run_tag)"; then
    note "$read_said"
    record_result A15 PASS "both answered from go.mod with gh as the review App on sjawhar; each conversation holds the call"
  else
    note "$read_said"
    record_result A15 FAIL "$read_said"
  fi
}

# check_a16: the search for the loop described as tool-less over the loop's packages (its dispatch
# and web_search tools' among them), the daemon's prompts, skills/, docs/ and every AGENTS.md, then
# the unit test that the loop's tool list holds gh, dispatch and web_search, run by its whole name
# as the Verification document's A16 check runs it.
check_a16() {
  local pattern hits unrelated paths
  store=-
  printf '== A16: the search for the loop described as tool-less finds no line about the loop\n'
  pattern=$(a16_pattern)
  paths=(packages/daemon/internal/answer packages/daemon/internal/answerloop packages/daemon/internal/ghtool
    packages/daemon/internal/dispatchtool packages/daemon/internal/websearch
    packages/daemon/internal/conversation packages/daemon/internal/agenthistory packages/daemon/internal/prompts skills docs)
  hits=$(cd "$root" && {
    grep -rniE --exclude-dir=node_modules --exclude-dir=dist --exclude-dir=.astro "$pattern" "${paths[@]}" | grep -vE '^docs/(plans|solutions)/' || true
    find . -name AGENTS.md -not -path '*/node_modules/*' -print0 | xargs -0 grep -niE "$pattern" /dev/null || true
  })
  printf '   $ grep -rniE "%s" %s (docs/plans and docs/solutions left out) and every AGENTS.md\n' "$pattern" "${paths[*]}"
  while IFS= read -r line; do [ -z "$line" ] || note "$line"; done <<<"$hits"
  unrelated=$(while IFS= read -r line; do
    [ -n "$line" ] || continue
    file=${line%%:*}
    text=${line#*:}
    text=${text#*:}
    recorded_hit "$file" "$text" || printf '%s\n' "$line"
  done <<<"$hits")
  local test_out
  test_out=$(cd "$root/packages/daemon" && go test -count=1 -v -run '^TestTheLoopsToolListHoldsItsThreeTools$' ./internal/answer/ 2>&1) || true
  note "$(grep -E '^(--- |ok|FAIL)' <<<"$test_out" | tr '\n' ' ')"
  if [ -z "$unrelated" ] && grep -q '^--- PASS: TestTheLoopsToolListHoldsItsThreeTools' <<<"$test_out"; then
    record_result A16 PASS "every hit is one the questions file records as not about the loop; TestTheLoopsToolListHoldsItsThreeTools passes"
  else
    record_result A16 FAIL "hits about the loop or unrecorded: ${unrelated:-none}; the unit test: $(grep -E '^(--- |ok|FAIL)' <<<"$test_out" | tr '\n' ' ')"
  fi
}

# implementer_with_pull_request makes, once per store, what A17 and A18 ask: a pull request on the
# smoke repository as the implement App (lib/smoke-pull-request.ts; recorded for cleanup) and a
# stopped implementer whose conversation holds it.
implementer_with_pull_request() {
  [ -z "${agent[$store:implementer.session]:-}" ] || return 0
  local issue branch pr token
  issue=$(new_rig_issue "Answer loop proof child: the implementation" "${agent[$store:root]}")
  branch=legion-e2eal/${issue,,}
  pr=$(timeout 120 bun "$root/scripts/e2e/lib/smoke-pull-request.ts" open "$work/github-apps.yaml" "$repo" "$branch" \
    "Answer loop proof $issue (closed by the run)") || return 1
  agent[$store:implementer.pr]=$(jq -r .number <<<"$pr")
  github_writes+=("pr:${agent[$store:implementer.pr]}:$branch")
  note "opened $repo pull request #${agent[$store:implementer.pr]} from $branch as the implement App"
  spawn_agent implementer implementer "${agent[$store:root]}" "$issue" \
    "Record this and keep it: your pull request is #${agent[$store:implementer.pr]} on $repo, from the branch $branch. Reply with one sentence restating it. Run no tool."
  await_turn implementer
  token=${agent[$store:implementer.token]}
  claims_cmd suspend --claim "$token" >/dev/null
  soft_wait 120 "the implementer to be suspended" claim_is "$token" '.state == "suspended"' &&
    soft_wait "$session_ttl_wait" "the implementer's session to leave the registry" session_gone "${agent[$store:implementer.session]}"
}

# comment_request MARKER: the question asking the stopped implementer for a comment on its pull request.
comment_request() {
  printf 'Please post a comment on your pull request #%s on %s whose body is exactly: %s. Then tell me what you did.' \
    "${agent[$store:implementer.pr]}" "$repo" "$1"
}

# person_message ISSUE SESSION BODY sends BODY as the proof person's targeted Dispatch message to
# SESSION on ISSUE, the way a person asks a stopped agent (A17: "A person asks"), and notes and
# fails on a message Dispatch did not take; message_reply_by reads the reply.
person_message() {
  dispatch_human POST "issues/$1/messages" \
    "$(jq -cn --arg t "session:$2" --arg b "$3" '{body: $b, target: $t, delivery: "steer"}')" >/dev/null && return 0
  note "the proof person's message to $2 on $1 was not taken"
  return 1
}

# message_listing SESSION prints the proof person's targeted messages to SESSION as Dispatch lists
# them (GET agents/<session>/messages, which serves the signed-in person's own messages and refuses
# the agents' bearer), and `null` for a read that failed: the listing a control reads once, checks
# for its own message (control_seen) and hands to the reads below as MESSAGES.
message_listing() {
  local messages
  messages=$(dispatch_human GET "agents/$1/messages" 2>/dev/null) || messages=null
  printf '%s\n' "$messages"
}
# message_record SESSION BODY [MESSAGES] prints the proof person's record of their targeted message
# to SESSION whose body is BODY, from MESSAGES (message_listing) where given and from the listing
# read now where not, {} for none.
message_record() {
  local messages=${3:-}
  [ -n "$messages" ] || messages=$(message_listing "$1")
  jq -c --arg b "$2" 'first(.[]? | select(.message.body == $b)) // {}' <<<"$messages" 2>/dev/null || printf '{}\n'
}

# message_reply_by SESSION BODY [MESSAGES] prints SESSION's reply to the proof person's targeted
# message whose body is BODY, from that person's record of it (message_record), and fails while it
# holds none.
message_reply_by() {
  local reply
  reply=$(record_reply_by "$(message_record "$1" "$2" "${3:-}")" "$1")
  [ -n "$reply" ] && printf '%s\n' "$reply"
}

# a17_impl_read BODY MARKER [COMMENTS [MESSAGES]] is the read of A17's first leg: the implementer
# answered the proof person's message BODY; its pull request's comments holding MARKER (COMMENTS,
# read now where not given) are one, the implement App's, footed with its session; and one
# `answers: gh` line of the answer's question is the implement App's call on sjawhar that exited 0.
# MESSAGES are the proof person's messages to the implementer as A17's control read them once
# (message_listing), read now where not given.
a17_impl_read() {
  local session=${agent[$store:implementer.session]} reply comments lines
  reply=$(message_reply_by "$session" "$1" "${4:-}" 2>/dev/null) || reply=
  comments=${3:-$(pr_comments "${agent[$store:implementer.pr]}" "$2")}
  lines=$(question_gh_lines "$(answer_question_id "$reply")") || lines='[]'
  read_said="the implementer's reply: $(said_text "$reply"); comments holding $2: $(jq -c 'map({id, user, footer: (.body | [capture("legion: (?<f>\\{[^}]*\\})").f] | first // null)})' <<<"$comments" 2>/dev/null || printf '%s' "$comments"); its answer's gh lines: $lines"
  [ -n "$reply" ] && one_comment_by "$comments" "$implement_app_login" "$session" &&
    first_line_where "$lines" gh_line_is implement sjawhar 0 >/dev/null
}
# a17_leak_read BODY MARKER [LISTING [MESSAGES]] is the read of A17's leak-guard leg: the
# implementer answered the message BODY; GitHub's listing of its pull request's comments (LISTING,
# read now where not given) holds the comment carrying MARKER,
# the first leg's, and none carrying the run's refusal marker (refused_write_absent; that last part
# read alone is a17_nothing_written, which A17's control runs); the reply quotes the check's own
# reason; and one of the answer's leak-guard refusals names the issue, the role, the repository and
# that reason. MESSAGES are as a17_impl_read's.
a17_leak_read() {
  local session=${agent[$store:implementer.session]} reply listing lines
  reply=$(message_reply_by "$session" "$1" "${4:-}" 2>/dev/null) || reply=
  listing=${3:-$(pr_comment_listing "${agent[$store:implementer.pr]}")}
  lines=$(question_leak_refusals "$(answer_question_id "$reply")") || lines='[]'
  read_said="the implementer's reply: $(said_text "$reply"); comments on #${agent[$store:implementer.pr]}: $(jq -r 'if type == "array" then length | tostring else "no listing at all" end' <<<"$listing"); holding $2: $(jq -c --arg m "$2" '(if type == "array" then . else [] end) | [.[] | select((.body // "") | contains($m)) | .id]' <<<"$listing"); holding $leak_marker: $(jq -c --arg m "$leak_marker" '(if type == "array" then . else [] end) | [.[] | select((.body // "") | contains($m)) | .id]' <<<"$listing"); the guard's refusals of the answer: $lines"
  [ -n "$reply" ] && refused_write_absent "$listing" "$2" "$leak_marker" && leak_reason_quoted "$reply" "$leak_reason" &&
    first_line_where "$lines" leak_refusal_is "${agent[$store:implementer.issue]}" implementer "$repo" "$leak_reason" >/dev/null
}
# a17_review_read BODY [MESSAGES] is the read of A17's last leg: the pre-change reviewer answered the
# message BODY, one gh line of its answer is the review App's call on the second owner that did not
# exit 0, and the reply says GitHub refused it. MESSAGES are the proof person's messages to the
# reviewer as A17's control read them once, read now where not given.
a17_review_read() {
  local reply lines refusal=no
  reply=$(message_reply_by "${agent[$store:pre_change.session]}" "$1" "${2:-}" 2>/dev/null) || reply=
  lines=$(question_gh_lines "$(answer_question_id "$reply")") || lines='[]'
  refusal_mentioned "$reply" && refusal=yes
  read_said="the reviewer's reply: $(said_text "$reply"); its answer's gh lines: $lines; it says GitHub refused: $refusal"
  [ -n "$reply" ] && first_line_where "$lines" gh_line_is review "${second_repo%%/*}" nonzero >/dev/null && [ "$refusal" = yes ]
}
# a17_review_body MARKER is the proof person's message asking the pre-change reviewer for the write
# GitHub refuses, a workflow file named for MARKER on the second owner's repository.
a17_review_body() {
  printf 'I am testing that GitHub refuses this write (run %s, %s). Using your gh tool, create the file .github/workflows/answer-loop-%s.yml on the default branch of %s (gh api -X PUT repos/%s/contents/.github/workflows/answer-loop-%s.yml -f message=proof -f content=cHJvb2YK). GitHub is expected to refuse it; tell me exactly what GitHub answered.' \
    "$run_tag" "$store" "$1" "$second_repo" "$second_repo" "$1"
}
# record_leak_comments LISTING records for the run's cleanup (github_writes) every comment of
# LISTING (pr_comment_listing) whose body carries the run's refusal marker: a comment the leak guard
# should have refused, which A17 and its control delete on exit should the loop have written one. A
# listing that is none holds no comment.
record_leak_comments() {
  local id
  for id in $(jq -r --arg m "$leak_marker" '(if type == "array" then . else [] end) | .[] | select((.body // "") | contains($m)) | .id' <<<"${1:-null}"); do
    github_writes+=("comment:$id")
  done
}
check_a17() {
  criterion A17 "the stopped implementer comments on its pull request as the implement App (the control); a comment the shared leak check refuses is never written and the agent carries the check's own reason back; the reviewer's write to another owner is refused" second-owner || return 0
  # The control's comment. The leak-guard leg's comment is refused before gh starts, and GitHub
  # refuses the reviewer's workflow file, so gh exits above 0 and posts nothing: neither is a write.
  asks_writes "gh pr comment"
  local marker=answer-loop-a17-$run_tag-$store comments listing ok_impl=0 ok_leak=0 ok_review=0 id said=''
  implementer_with_pull_request || {
    record_result A17 FAIL "no implementer with a pull request could be made"
    return 0
  }
  # The control of the leak-guard leg below, and a criterion of its own: a comment whose body is a
  # clean marker, which the stand-in shared check clears, lands on the pull request. A stand-in
  # that refused every call, and a path that wrote nothing at all, both fail here, so the refusal
  # below is a refusal of this one call and not of everything. Each ask of the implementer is the
  # proof person's Dispatch message, as the criterion's person asks, and so is the reviewer's.
  local impl_issue=${agent[$store:implementer.issue]} impl_session=${agent[$store:implementer.session]} impl_body
  impl_body="$(comment_request "$marker") (run $run_tag, $store)"
  impl_reply() { message_reply_by "$impl_session" "$impl_body"; }
  if person_message "$impl_issue" "$impl_session" "$impl_body" &&
    soft_wait 300 "the implementer's reply to the proof person's message" impl_reply; then
    comments=$(settled_pr_comments "${agent[$store:implementer.pr]}" "$marker")
    for id in $(jq -r '.[].id' <<<"$comments"); do github_writes+=("comment:$id"); done
    a17_impl_read "$impl_body" "$marker" "$comments" && ok_impl=1
    note "$read_said"
    said+="the control's comment: $read_said; "
  fi
  # The same implementer and the same pull request, asked for a comment whose body is the run's
  # refusal marker, which the stand-in shared check refuses (install_leak_check). Both halves are
  # read and never the agent's answer alone — a leg that read the reply alone would pass with the
  # guard wired to nothing at all:
  #   (a) the listing GitHub answered holds the control's own comment and no comment carrying the
  #       refusal marker, so the write was never made and the listing that shows it was read;
  #   (b) the agent's reply to the asker carries the check's own reason, so what refused the call
  #       is the check and not some other refusal of the tool's.
  # The daemon's own line for it is read beside them: one refusal line naming the issue, the role,
  # the repository and that reason.
  impl_body="$(comment_request "$leak_marker") If a tool refuses the call, quote what it answered word for word. (run $run_tag, $store)"
  if person_message "$impl_issue" "$impl_session" "$impl_body" &&
    soft_wait 300 "the implementer's reply about the refused comment" impl_reply; then
    listing=$(pr_comment_listing "${agent[$store:implementer.pr]}")
    record_leak_comments "$listing"
    a17_leak_read "$impl_body" "$marker" "$listing" && ok_leak=1
    note "$read_said"
    said+="the leak guard's refusal: $read_said; "
  fi
  # The write is a workflow file because the review App's lease holds no `workflows` permission
  # (appauth's writePermissions): every gh directory of a bundle holds the role's own lease on that
  # owner, contents write included, so a plain file there could land, while GitHub refuses a
  # workflow file to an App that lacks the permission, which is the refusal this leg reads.
  # The ask is the proof person's, a Dispatch message to the reviewer's session on its own issue,
  # as A17 says ("A person asks"): the loop's gh does what a message asks when the agent judges it
  # should, and in proof run 44278025821 the reviewer rightly declined this same write when an
  # Envoy message from another session asked for it. The message says the write is a test GitHub
  # is expected to refuse, so the agent knows what it is asked to show.
  local review_body review_issue=${agent[$store:pre_change.issue]} review_session=${agent[$store:pre_change.session]}
  review_body=$(a17_review_body "$marker")
  review_reply() { message_reply_by "$review_session" "$review_body"; }
  if person_message "$review_issue" "$review_session" "$review_body" &&
    soft_wait 300 "the reviewer's reply to the proof person's message" review_reply; then
    a17_review_read "$review_body" && ok_review=1
    note "$read_said"
    said+="the reviewer's refusal: $read_said"
  fi
  if [ "$ok_impl$ok_leak$ok_review" = 111 ]; then
    record_result A17 PASS "the clean-marker comment is the implement App's one comment with its session's footer, logged with App and owner (the control: a check refusing everything, and a path writing nothing, fail this leg); the pull request's listing holds that comment and no comment naming $leak_marker, and the implementer's reply carries the check's own reason, with the daemon's refusal line naming the issue, the role, the repository and that reason; the reviewer's write to the second owner was refused and it said so"
  else
    record_result A17 FAIL "the control's comment $ok_impl, the leak guard's refusal $ok_leak (both halves: a listing holding the control's comment and none holding $leak_marker, and the check's own reason in the reply and in the daemon's refusal line), reviewer refusal $ok_review (1 = shown): $said"
  fi
}

# hold_after_tool LABEL K: with LABEL held (hold_at) from when its <label>.held named K calls
# (held_count), the first call the proxy holds after those, the loop's first model call, is let
# through and the next one it holds, the call after the loop's gh call, waited for, so the loop
# stands between the gh call and its reply. Each is read off what the proxy holds and not reckoned
# from the records: the proxy numbers a call as it arrives and records it only once it ends, so the
# records' count is a floor of the ordinals it has given. In the resume leg of proof run
# 43996451813 the loop's first call came at another ordinal than the one after that count, nothing
# let it through, and the leg gave up.
hold_after_tool() {
  local first
  soft_wait 300 "the loop's first model call" held_since "$1" "$2" || return 1
  first=$(held_since "$1" "$2")
  note "the proxy holds the loop's first model call at $1 ordinal $first; letting it through and holding the next"
  hold_at "$1" "$first"
  soft_wait 300 "the loop's model call after its gh call" held_since "$1" "$(($2 + 1))" || return 1
  note "the proxy holds the loop's model call after its gh call at $1 ordinal $(held_since "$1" "$(($2 + 1))")"
}

# a18_restart_read MARKER EVENT [COMMENTS] is the read of A18's restart leg: the pull request's
# comments holding MARKER (COMMENTS, read now where not given) are one, and one reply to EVENT
# reached the asker.
a18_restart_read() {
  local comments replies
  comments=${3:-$(pr_comments "${agent[$store:implementer.pr]}" "$1")}
  replies=$(inbox_replies "${2:-}")
  read_said="comments holding $1: $(jq length <<<"$comments"); replies to ${2:-(no event)}: $replies; gh lookups this boot $(boot_log | jq -s -c '[.[] | select(.msg == "answers: gh lookup")] | length')"
  [ "$(jq length <<<"$comments")" = 1 ] && [ -n "${2:-}" ] && [ "$replies" = 1 ]
}
# a18_resume_read MARKER EVENT [COMMENTS] is the read of A18's resume leg: the pull request's
# comments holding MARKER (COMMENTS, read now where not given) are one, this boot forwarded the
# implementer's question with its call, and nothing was posted to the asker in reply to EVENT
# (a18_nothing_posted).
a18_resume_read() {
  local comments forwarded=no nothing=no
  comments=${3:-$(pr_comments "${agent[$store:implementer.pr]}" "$1")}
  question_forwarded implementer && forwarded=yes
  a18_nothing_posted "${2:-}" && nothing=yes
  read_said="comments holding $1: $(jq length <<<"$comments"); the question forwarded to the implementer: $forwarded; replies to ${2:-(no event)}: $(inbox_replies "${2:-}"); the running implementer's conversation names the gh call: $(conversation_text implementer 2>/dev/null | grep -c -- "$1" || true)"
  [ "$(jq length <<<"$comments")" = 1 ] && [ "$forwarded" = yes ] && [ "$nothing" = yes ]
}
# a18_nothing_posted EVENT [INBOX] is the part of A18's resume leg that says the loop posts
# nothing: no reply to EVENT reached the asker (no_reply_to), the question the loop dropped its
# answer to and forwarded with its gh call to the running implementer. A18's control places such a
# reply, carrying the loop's mark, and this read, over the inbox it read once (INBOX,
# inbox_listing; read now where not given), must see it.
a18_nothing_posted() {
  read_said="replies on the asker's topic to ${1:-(no event)}: $(inbox_replies "${1:-}" "${2:-}")"
  no_reply_to "${1:-}" "${2:-}"
}
check_a18() {
  criterion A18 "a restart after the comment's gh call posts the answer once and the comment once; a resume at that point hands the agent the call" || return 0
  # One comment in each leg: the loop's gh call made it before the kill and before the resume.
  asks_writes "gh pr comment" "gh pr comment"
  local label=daemon-$store marker=answer-loop-a18-$run_tag-$store sent event n k comments token ok_restart=0 ok_resume=0 id said=''
  implementer_with_pull_request || {
    record_result A18 FAIL "no implementer with a pull request could be made"
    return 0
  }
  token=${agent[$store:implementer.token]}
  n=$(proxy_ordinal "$label")
  k=$(held_count "$label")
  hold_at "$label" "$n"
  sent=$(envoy_ask_text session "${agent[$store:implementer.session]}" "$(comment_request "$marker-restart")" "a18-restart-$run_tag-$store")
  event=${sent#* }
  if hold_after_tool "$label" "$k"; then
    note "the comment's gh line before the kill: $(boot_log | jq -c 'select(.msg == "answers: gh" and .role == "implementer")' | tail -1)"
    freeze_if "$daemon_pid" "${started_at[$daemon_pid]:-}" KILL || note "the daemon $daemon_pid was already gone before the kill"
    daemon_pid=
    start_daemon "$daemon_bin" "$daemon_config" "$daemon_log" "$daemon_home" "$daemon_profile" "$daemon_plugin_dir"
    release "$label"
    soft_wait 300 "the answer after the restart" inbox_reply "$event" || true
    comments=$(settled_pr_comments "${agent[$store:implementer.pr]}" "$marker-restart")
    for id in $(jq -r '.[].id' <<<"$comments"); do github_writes+=("comment:$id"); done
    a18_restart_read "$marker-restart" "$event" "$comments" && ok_restart=1
    note "after the restart: $read_said"
    said+="the restart: $read_said; "
  fi
  release "$label"
  n=$(proxy_ordinal "$label")
  k=$(held_count "$label")
  hold_at "$label" "$n"
  sent=$(envoy_ask_text session "${agent[$store:implementer.session]}" "$(comment_request "$marker-resume")" "a18-resume-$run_tag-$store")
  event=${sent#* }
  if hold_after_tool "$label" "$k"; then
    claims_cmd resume --claim "$token" >/dev/null
    soft_wait 300 "the implementer to start" claim_is "$token" '.state | IN("launching", "shim_connected", "registered", "ready", "working", "idle")' || true
    release "$label"
    forwarded_count() { question_forwarded implementer; }
    soft_wait 300 "the question to be forwarded with its gh call" forwarded_count || true
    soft_wait 600 "the running implementer's turn" claim_is "$token" '.state == "idle" and .pending == null' || true
    comments=$(settled_pr_comments "${agent[$store:implementer.pr]}" "$marker-resume")
    for id in $(jq -r '.[].id' <<<"$comments"); do github_writes+=("comment:$id"); done
    a18_resume_read "$marker-resume" "$event" "$comments" && ok_resume=1
    note "after the resume: $read_said"
    said+="the resume: $read_said"
    claims_cmd suspend --claim "$token" >/dev/null || true
  fi
  release "$label"
  if [ "$ok_restart$ok_resume" = 11 ]; then
    record_result A18 PASS "killed after the comment's gh call: one answer, one comment; resumed at that point: the agent got the call, one comment"
  else
    record_result A18 FAIL "restart $ok_restart, resume $ok_resume (1 = one comment and the answer or the hand-over as required): $said"
  fi
}

# deliver_review NUMBER REVIEW LABEL delivers review REVIEW of the smoke pull request NUMBER to the
# listener's GitHub webhook route as GitHub delivers it, and prints the route's answer,
# {delivery, status, answer}. A running daemon reads a pull request's listed reviews only as its
# workflow starts and after a handoff is recorded (packages/daemon/internal/daemon/issue_reads.go,
# readIssuePullRequest); in a deployment a review decides by its `pull_request_review` webhook,
# which the listener verifies and publishes on notifications.github.<owner>.<repo>.> for the
# daemon's GitHub intake. The smoke repository sends its webhooks to no listener of this run's, so
# the rig makes GitHub's delivery itself: it reads the review and its pull request from GitHub as
# the implement App (smoke_gh, read-only), and lib/github-review-webhook.ts builds the event from
# those two answers, signs its exact body with the route's secret ($work/github-webhook-secret) and
# posts it. The two answers and the body sent are kept as $evidence/a19/<store>/LABEL.*.json. It
# fails when either read fails or the route answers anything but 2xx.
deliver_review() {
  local number=$1 review=$2 label=$3 dir=$evidence/a19/$store
  mkdir -p "$dir"
  if ! smoke_gh api "repos/$repo/pulls/$number/reviews/$review" >"$dir/$label.review.json" ||
    ! smoke_gh api "repos/$repo/pulls/$number" >"$dir/$label.pull.json"; then
    printf '{"error":"GitHub answered no review %s of #%s"}\n' "$review" "$number"
    return 1
  fi
  bun "$root/scripts/e2e/lib/github-review-webhook.ts" "$work/github-webhook-secret" "$dir/$label.review.json" \
    "$dir/$label.pull.json" "$(listener_url /webhook/github)" "$dir/$label.event.json"
}

# a19_pr_mergeable NUMBER LIMIT waits, up to LIMIT seconds, until GitHub reads the smoke pull request
# NUMBER as one it would merge now: its REST mergeable_state clean, unstable or has_hooks, the three
# states lib/workflow.sh's smoke_pr_mergeable merges at. The smoke main requires the `gate` check,
# which has not run when the pull request opens, and until it passes GitHub reads the pull request
# blocked. It reads as the implement App (smoke_gh, which mints a token per read, so it asks every
# 10 s), and keeps the last state read in a19_merge_state, `unread` for a read that failed, for the
# row's failure to name.
a19_merge_state=
a19_pr_mergeable() {
  local number=$1 limit=$2 started=$SECONDS
  note "waiting up to ${limit}s for GitHub to read #$number's mergeable_state as clean, unstable or has_hooks"
  while :; do
    a19_merge_state=$(smoke_gh api "repos/$repo/pulls/$number" --jq .mergeable_state 2>/dev/null) || a19_merge_state=unread
    case "$a19_merge_state" in
      clean | unstable | has_hooks) return 0 ;;
    esac
    ((SECONDS - started < limit)) || return 1
    sleep 10
  done
}
# a19_seed_round ISSUE BRANCH NUMBER HEAD TOKEN writes the review round's two rows as the workflow
# writes them when a reviewer completes undecided: the reviewer's `phases` row naming its claim
# TOKEN, its handoff commit HEAD and its completion time, and the `pull_requests` row of the smoke
# pull request NUMBER on BRANCH at HEAD. check_a19 writes them once and A19's control writes them
# again (control_prepare_a19); the issue's own row is the caller's, and the daemon must be stopped.
a19_seed_round() {
  pg_sql "$daemon_db" "insert into phases (issue, role, claim, handoff_commit, rounds, verdict, completed_at) values ('$1', 'reviewer', '$5', '$4', 1, '', now() - interval '2 minutes')" >/dev/null &&
    pg_sql "$daemon_db" "insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at, failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, pushes, head_counted, state)
      values ('$1', '$repo', $3, '$2', '$4', now(), '[]', '[]', 0, 0, '[]', 0, '', '[]', '', 'open')" >/dev/null
}

# check_a19: a review round the daemon's record holds open on a pull request of the smoke
# repository, with its reviewer's completion recorded and the reviewer stopped. The record is
# written with the daemon stopped, as the workflow writes it when a reviewer completes undecided
# (issue in reviewing, its tree's lifecycle open under the workflow; the reviewer's phase row
# naming its claim, its handoff commit and completion time; the pull request at its head): driving
# a tree there through planning, implementing and testing with real agents is the work of the
# stage proofs, not of this one. The lifecycle is as much of that record as the issue row: without
# it the next boot's admission pass finds a root at a running status whose tree nothing keeps live
# and takes the whole round out of the workflow (admit's releaseUnslotted; open_tree_lifecycle).
# The reviewer is a real Claude Code reviewer the daemon ran on that issue, its conversation
# holding its review. Each review reaches the running daemon as a deployment's does, by its GitHub
# webhook (deliver_review). The control comes first: an approval of the head by the review App
# whose footer names another session is refused within 120 s of its delivery, the workflow's
# `a review decides nothing: the review App submitted it from a session that is not the
# reviewer's` line naming the issue (workflow's RefusesRound), and the round stays open with no
# decision. Then the stopped reviewer is asked to approve: its gh tool posts the approval with its
# recorded session's footer, and once that is delivered the daemon moves the issue from reviewing
# to accepting within 120 s, the round ended on that review, as the reviewer's running approval
# would.
check_a19() {
  criterion A19 "the stopped reviewer's approval with its recorded session ends the round; another session's footer ends nothing" || return 0
  asks_writes "gh pr review"
  local issue branch pr number head token session control control_after control_delivery='' sent event reply review approval_delivery='' ok_control=0 ok_reviewer=0
  issue=$(new_rig_issue "Answer loop proof: the review round ($store)")
  branch=legion-e2eal/${issue,,}
  if ! pr=$(timeout 120 bun "$root/scripts/e2e/lib/smoke-pull-request.ts" open "$work/github-apps.yaml" "$repo" "$branch" "Answer loop proof $issue (closed by the run)"); then
    record_result A19 FAIL "no pull request could be opened on $repo"
    return 0
  fi
  number=$(jq -r .number <<<"$pr")
  head=$(jq -r .head <<<"$pr")
  github_writes+=("pr:$number:$branch")
  note "opened $repo pull request #$number from $branch at $head as the implement App"
  token=$(claim_token "$issue" reviewer)
  write_claude_route "$token" "claude-$store"
  provision_workspace "$issue"
  claims_cmd spawn --json --tree "$issue" --issue "$issue" --role architect --prompt-file "$(write_prompt architect "$issue")" >/dev/null
  agent[$store:a19_architect.token]=$(claim_token "$issue" architect)
  spawn_agent reviewer reviewer "$issue" "$issue" \
    "Record this and keep it: you are the reviewer of pull request #$number on $repo, head $head. You reviewed it, found nothing to change, and completed your round. Reply with one sentence restating it. Run no tool."
  await_turn reviewer
  session=${agent[$store:reviewer.session]}
  agent[$store:a19.number]=$number
  agent[$store:a19.head]=$head
  agent[$store:a19.issue]=$issue
  suspend_agent reviewer || true
  claims_cmd suspend --claim "${agent[$store:a19_architect.token]}" >/dev/null || true
  # The daemon stops next, and a suspension it had not finished would leave the claim mid-way for
  # the next boot to take up.
  soft_wait 120 "the round's architect to be suspended" claim_is "${agent[$store:a19_architect.token]}" '.state == "suspended"' || true
  stop_daemon
  record_tree_issue "$issue" "$issue" "" reviewing "Answer loop proof: the review round ($store)"
  open_tree_lifecycle "$issue"
  a19_seed_round "$issue" "$branch" "$number" "$head" "$token"
  note "recorded $issue's review round open on #$number at $head, the reviewer $token (session $session) completed undecided: $(review_record "$issue")"
  # A review round whose pull request GitHub cannot merge waits on its merge state (the workflow's
  # classify.RoundWaitsOnMergeState), and in this rig no check webhook reaches the listener to have
  # the daemon read that state again once the smoke main's required check has run. So the daemon
  # boots on the round only once GitHub reads the pull request mergeable (a19_pr_mergeable), and its
  # first read of the round finds it so; a pull request GitHub never reads mergeable fails the row
  # naming the state GitHub last gave.
  if ! a19_pr_mergeable "$number" 600; then
    start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
    record_result A19 FAIL "GitHub still read $repo#$number's mergeable_state as ${a19_merge_state:-never read} 600 s after it opened, so no review could end its round"
    return 0
  fi
  note "GitHub reads $repo#$number's mergeable_state as $a19_merge_state; the daemon boots on the round"
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
  note "the daemon keeps the round's tree live: $(legion_cmd state --json | jq -c --arg i "$issue" '{trees: .admission.trees, phase: .issues[$i].phase}')"
  # The control's footer names the pull request's head, as the reviewer's own approval will, so the
  # session is the one thing that differs: a footer naming no head decides nothing whoever submits
  # it (main's refusesRound), and a control refused for that would show nothing about the session.
  control=$(timeout 120 bun "$root/scripts/e2e/lib/smoke-pull-request.ts" approve "$work/github-apps.yaml" "$repo" "$number" "$head" \
    "Approved (control: another session's footer). <!-- legion: {\"session\":\"legion-e2eal-another-session-$run_tag\",\"phase\":\"reviewing\",\"head\":\"$head\"} -->") || control=
  note "control: the review App's approval at $head with another session's footer: $control"
  control_refused() { review_refused "$issue" "the review App submitted it from a session that is not the reviewer's"; }
  if [ -n "$control" ]; then
    control_delivery=$(deliver_review "$number" "$(jq -r .id <<<"$control")" control) || true
    note "the control delivered to the listener's /webhook/github as GitHub delivers it: $control_delivery"
    soft_wait 120 "the daemon to refuse the control" control_refused || true
  fi
  control_after=$(review_record "$issue")
  note "after the control's delivery: $control_after; refused as another session's: $(control_refused && echo yes || echo no); the reviewer's claim: $(claim_json "$token" | jq -c '{state, session}')"
  [ -n "$control" ] && control_refused && review_round_undecided "$control_after" && ok_control=1
  sent=$(envoy_ask_text session "$session" "Please approve your pull request #$number on $repo at its head $head now, with your gh tool: gh pr review $number -R $repo --approve --body 'Approved: nothing to change.' Then tell me what you did.")
  event=${sent#* }
  note "to the stopped reviewer: listener $sent"
  if soft_wait 300 "the reviewer's reply" inbox_reply "$event"; then
    reply=$(inbox_reply "$event")
    note "the reviewer's reply: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
    note "its gh line: $(boot_log | jq -c --arg i "$issue" 'select(.msg == "answers: gh" and .issue == $i)' | tail -1)"
  fi
  approved() { approval_by "$(reviewer_approvals "$number" "$session")" "$review_app_login" "$head"; }
  soft_wait 60 "the reviewer's approval of $head on GitHub" approved || true
  review=$(reviewer_approvals "$number" "$session")
  note "the reviewer's approvals on GitHub: $review"
  accepted() { phase_changed "$issue" reviewing accepting; }
  if approval_by "$review" "$review_app_login" "$head"; then
    approval_delivery=$(deliver_review "$number" "$(approval_id "$review")" approval) || true
    note "the reviewer's approval delivered to the listener's /webhook/github as GitHub delivers it: $approval_delivery"
    soft_wait 120 "the daemon to move $issue from reviewing to accepting" accepted || true
  fi
  note "the daemon's lines for $issue: $(boot_log | jq -s -c --arg i "$issue" '[.[] | select((.issue // "") == $i and ((.msg | startswith("answers:")) | not)) | .msg] | .[-6:]')"
  a19_read "$number" "$session" "$head" "$issue" "" && ok_reviewer=1
  note "$read_said"
  if [ "$ok_control$ok_reviewer" = 11 ]; then
    record_result A19 PASS "the review App's approval with another session's footer, delivered by webhook, was refused as another session's and left the round open; the stopped reviewer's approval, footed with its recorded session $session and delivered the same way, moved $issue from reviewing to $(jq -r .phase <<<"$(review_record "$issue")") on that review"
  else
    record_result A19 FAIL "control refused as another session's with the round left open $ok_control, the reviewer's approval moved the issue from reviewing to accepting on that review $ok_reviewer (1 = shown): $read_said; the listener answered the control ${control_delivery:-(not delivered)} and the approval ${approval_delivery:-(not delivered)}"
  fi
}
# a19_read NUMBER SESSION HEAD ISSUE SINCE [REVIEWS] is the read of A19's reviewer leg: the review
# App's one approval of HEAD footed with SESSION on pull request NUMBER, submitted since SINCE where
# SINCE is given (A19's control, asked after A19's own approval stands), moved ISSUE from reviewing
# to accepting this boot, and the round ended on that review. REVIEWS is the pull request's reviews
# as A19's control listed them once (pr_review_listing), read now where not given.
a19_read() {
  local review after accepted=no
  review=$(reviewer_approvals "$1" "$2" "${5:-}" "${6:-}")
  after=$(review_record "$4" 2>/dev/null) || after=null
  phase_changed "$4" reviewing accepting && accepted=yes
  read_said="the reviewer's approvals${5:+ since $5}: $review; moved from reviewing to accepting this boot: $accepted; the round: $after"
  approval_by "$review" "$review_app_login" "$3" && [ "$accepted" = yes ] &&
    review_round_ended "$after" "$(approval_id "$review")"
}

# check_a21: a person's picker mention of a failed agent's role, on an issue the daemon records in
# implementing, and a message to that role's topic are each answered from that agent's own
# conversation. The issue is a tree of its own, as A19's is, with an architect spawned with no task
# (an operator's spawn opens a tree's lifecycle only for its root's architect) and an Oh My Pi
# implementer whose task is the answer to question failed-implementer-role. Both are suspended, the
# implementer until its session has left the listener's registry (suspend_agent), since the
# listener hands off no message for a session it still holds; an implementer that is not suspended
# ends the row there, its architect suspended and its FAIL recorded, with nothing seeded. Then, with
# the daemon stopped, the issue is written into the daemon's record in implementing with its tree's
# lifecycle open under the workflow, as A6 writes its running tree (record_tree_issue,
# open_tree_lifecycle), and the implementer's claim is written failed as the supervisor fails a claim
# whose deaths ran out (supervise's fail: the state, the cause `died` with its sentence and its
# time, no locator, nothing that suspended it), since no `legion claims` command fails a claim. The
# daemon boots on that record: nothing relaunches a failed claim, and an operator's suspension holds
# against the boot's resume of a phase's owner. It also holds the issue, as it holds any issue whose
# worker failed with work outstanding (api.Issue.HeldFrom names the phase it was held from), and a
# held issue is still in the workflow and in its tree. The mention is asked only once the daemon's
# own state keeps the tree live with the issue in implementing or held from implementing
# (tree_is_live ... held), the claim still stands failed, and the listener's registry entry for the
# implementer's session, the role's one and so its newest, reads under the role's token
# (registry_entry_of): the daemon's state is what makes this an issue that has not left the
# workflow, and that entry what makes it a role with no live holder whose newest entry the listener
# hands to its daemon. Then the proof person comments on the issue with a picker mention of the role
# (`role:<token>`), as A3 does. A resolution that names a role's newest entry only once that entry's
# issue has left the workflow settles this mention's delivery `failed` with `no holder for role
# <token>`, pins no session, and nobody answers, which is what this row's FAIL then reads out (the
# delivery's state and error). Then the rig's asker sends question failed-implementer-role to the
# role's topic, `notifications.role.<token>`, under a marker of its own, and its answer is the one
# reply on the asker's topic. Control: once the implementer's answer is waited for, the proof person
# comments on the same issue with a picker mention of the issue's reviewer role (claim_token), which
# no session of the run ever took, so the registry holds no entry for it. Under the run's answering
# project Dispatch keeps that mention for the role's first holder: its one delivery settles `queued`
# naming no session, read again for up to 60 s until it does, and no session replies to it. PASS
# when the mention's one delivery is `sent` to the implementer's session, that session's reply to
# the comment holds the question's keys, the control's one delivery is `queued` naming no session
# and the implementer's session wrote no reply to it, the role topic's message was answered by one
# reply holding the same keys from that session, every claim of the issue still stands failed or
# suspended (tree_claims_in: every claim of the row is spawned on the issue's own tree), the
# implementer's claim stands failed, and no pane started across both legs (a21_read, which A21's
# control runs too, its part that the implementer wrote no reply to the control apart as
# a21_nothing_posted, the read A21's control places a reply carrying the loop's mark for); and a
# message kept for a role nobody holds reaches the first session to hold it (a21_first_holder, apart
# from that read, from the pane baseline and from the control; its read, a21_holder_read, A21's
# control runs over a mention of a role the first holder did not claim). The verdict judges the
# deliveries, the replies and the claims listing the row read and printed, and the FAIL prints the
# control's deliveries beside the mention's.
check_a21() {
  criterion A21 "a picker mention of a failed implementer's role and a message to its role topic, on an issue still in the workflow (implementing, or held from it), are answered from its conversation, nothing of the issue starting; a message kept for a role nobody holds reaches its first holder" infra448 || return 0
  local issue token architect reviewer session title comment id control_id before why='' panes_before role_sent role_event
  title="Answer loop proof: an issue in implementing whose implementer failed ($store)"
  issue=$(new_rig_issue "$title")
  token=$(claim_token "$issue" implementer)
  architect=$(claim_token "$issue" architect)
  reviewer=$(claim_token "$issue" reviewer)
  provision_workspace "$issue"
  claims_cmd spawn --json --tree "$issue" --issue "$issue" --role architect --prompt-file "$(write_prompt architect "$issue")" >/dev/null
  spawn_agent failed_implementer implementer "$issue" "$issue" "$(jq -r .agents.failed_implementer.task "$questions_file")"
  await_turn failed_implementer
  session=${agent[$store:failed_implementer.session]}
  if ! suspend_agent failed_implementer; then
    why="the implementer $token was not suspended, or its session $session stayed in the listener's registry"
    claims_cmd suspend --claim "$architect" >/dev/null || true
    note "the mention is not asked: $why"
    record_result A21 FAIL "$why"
    return 0
  fi
  claims_cmd suspend --claim "$architect" >/dev/null || true
  # The daemon stops next, and a suspension it had not finished would leave the claim mid-way for
  # the next boot to take up.
  soft_wait 120 "the issue's architect to be suspended" claim_is "$architect" '.state == "suspended"' || true
  stop_daemon
  record_tree_issue "$issue" "$issue" "" implementing "$title"
  open_tree_lifecycle "$issue"
  pg_sql "$daemon_db" "update claims set state = 'failed', suspended_by = '', failure = 'deaths with work outstanding ran out', failure_cause = 'died', failed_at = now(), locator = null where token = '$token'" >/dev/null
  note "recorded $issue in implementing, its tree's lifecycle open under the workflow, and its implementer $token (session $session) failed: $(pg_sql "$daemon_db" "select state, suspended_by, failure_cause, failure from claims where token = '$token'")"
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
  a21_entry() { envoy "$(listener_url "/v1/legion/agents/sessions/$session")" 2>/dev/null || true; }
  a21_registered() { registry_entry_of "$(a21_entry)" "$session" "$token"; }
  if ! soft_wait 300 "the daemon to keep the seeded tree $issue live in implementing, or held from it" tree_is_live "$issue" implementing held; then
    why="the seeded tree $issue was not live in implementing, nor held from it, in the daemon's own state ($(legion_cmd state --json | jq -c --arg i "$issue" '{trees: .admission.trees, phase: .issues[$i].phase, heldFrom: .issues[$i].heldFrom}' || true))"
  fi
  if [ -z "$why" ] && ! claim_is "$token" '.state == "failed"'; then
    why="the implementer's claim $token did not stand failed after the boot: $(claim_json "$token" | jq -c '{state, session}' || true)"
  fi
  if [ -z "$why" ] && ! soft_wait 180 "the registry's entry for $session to read under $token" a21_registered; then
    why="the listener's registry entry for $session did not read under $token: $(a21_entry | jq -c '(.agent // .) | {session_id, role_token, live}' || true)"
  fi
  if [ -n "$why" ]; then
    note "the mention is not asked: $why"
    record_result A21 FAIL "$why"
    return 0
  fi
  note "the daemon's state of $issue: $(legion_cmd state --json | jq -c --arg i "$issue" '{phase: .issues[$i].phase, heldFrom: .issues[$i].heldFrom}' || true); the registry's entry for $session: $(a21_entry | jq -c '(.agent // .) | {session_id, role_token, issue, live}' || true); the claims of $issue: $(issue_claim_roles "$(claims_cmd list --json)" "$issue")"
  panes_before=$(pane_count)
  before=$(date +%s)
  comment=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg body "@implementer $(question_text failed-implementer-role) (question failed-implementer-role of run $run_tag)" \
    --arg t "role:$token" '{body: $body, mentions: [{target: $t}]}')") || comment='{}'
  id=$(jq -r '.id // empty' <<<"$comment")
  note "the person's comment ${id:-(not taken)} on $issue mentions role:$token; deliveries $(jq -c '[.deliveries[]? | {target, state, session_id, error}]' <<<"$comment")"
  if [ -n "$id" ] && soft_wait 240 "the failed implementer's reply to comment $id" comment_reply_by "$issue" "$id" "$session"; then
    note "answered after $(($(date +%s) - before)) s as $session: $(said_text "$(comment_reply_by "$issue" "$id" "$session" || true)")"
  fi
  # The same role by its topic: the failed implementer's role, which the registry resolves to its
  # session, answers the asker's message there, under a marker of its own.
  role_sent=$(envoy_ask_text role "$token" "$(question_text failed-implementer-role) (question failed-implementer-role of run $run_tag, role topic)" "a21-role-$run_tag-$store")
  role_event=${role_sent#* }
  note "the message to the role topic of $token: listener $role_sent"
  [ -z "$role_event" ] || soft_wait 240 "the failed implementer's reply to the role topic message" inbox_reply "$role_event" || true
  control_id=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg body "@reviewer $(question_text failed-implementer-role) (control: a role of this issue no session of the run took, run $run_tag)" \
    --arg t "role:$reviewer" '{body: $body, mentions: [{target: $t}]}')" | jq -r '.id // empty') || control_id=
  note "the control: the person's comment ${control_id:-(not taken)} on $issue mentions role:$reviewer, which no session of the run took"
  [ -z "$id" ] || soft_wait 60 "comment $id's mention to be delivered to implementer $session" comment_delivered_to "$id" "$session" || true
  a21_control_kept() { one_delivery_for "$(comment_delivery_records "$control_id")" queued ''; }
  [ -z "$control_id" ] || soft_wait 60 "the control's mention of role:$reviewer to settle queued, naming no session" a21_control_kept || true
  if a21_read "$issue" "$id" "$control_id" "$panes_before" "$role_event"; then
    note "$read_said"
    if a21_first_holder "$issue"; then
      note "$read_said"
      record_result A21 PASS "the role mention of the failed implementer on $issue, in the workflow, was sent to its session $session, which answered in the thread from its conversation with the question's keys; the message to its role topic was answered once, by $session, with the same keys; the control's mention of role:$reviewer, a role of $issue no session took, settled queued naming no session, kept for the role's first holder, and $session wrote no reply to it; every claim of $issue stayed failed or suspended and no pane started across both legs; $read_said"
    else
      note "$read_said"
      record_result A21 FAIL "the picker mention and the role topic's message were answered as required, but the first holder's leg did not hold: $read_said"
    fi
  else
    note "$read_said"
    record_result A21 FAIL "$read_said"
  fi
}
# a21_read ISSUE COMMENT CONTROL PANES ROLE_EVENT [COMMENTS] is A21's read: the failed implementer's
# reply to the role mention COMMENT on ISSUE holds failed-implementer-role's keys and the mention's
# one delivery is sent to its session; the mention CONTROL of a role no session took settled queued
# naming no session, and the implementer wrote no reply to it (a21_nothing_posted); the message to
# the failed role's topic, sent as ROLE_EVENT, was answered by one reply on the asker's topic that
# holds the same keys and whose answer's log line names the implementer's session; every claim of
# ISSUE stands failed, suspended or retired, the implementer's failed; and the daemon's tmux server
# holds no more than PANES panes: nothing started across the two legs. COMMENTS are ISSUE's
# comments as A21's control read them once, read now where not given.
a21_read() {
  local session=${agent[$store:failed_implementer.session]} token=${agent[$store:failed_implementer.token]} reply missing='' held=no deliveries control nothing=no claims panes
  local role_reply role_replies role_keys=no role_by=no
  reply=$(comment_reply_by "$1" "$2" "$session" "${6:-}" 2>/dev/null) || reply=
  if [ -n "$reply" ]; then
    missing=$(answer_holds failed-implementer-role "$reply") && held=yes
  fi
  deliveries=$(comment_delivery_records "$2")
  control=$(comment_delivery_records "$3")
  a21_nothing_posted "$1" "$3" "${6:-}" && nothing=yes
  role_reply=$(inbox_reply "$5" 2>/dev/null) || role_reply=
  role_replies=$(inbox_replies "$5")
  [ -n "$role_reply" ] && answer_holds failed-implementer-role "$role_reply" >/dev/null && role_keys=yes
  answered_by "$(answered_line "$(answer_question_id "$role_reply")" || true)" "$session" && role_by=yes
  claims=$(claims_cmd list --json 2>/dev/null) || claims='{}'
  panes=$(pane_count)
  read_said="the role mention ${2:-(not taken by Dispatch)}: deliveries $deliveries (want one sent to $session); the implementer's reply to it $(if [ -z "$reply" ]; then printf none; elif [ "$held" = yes ]; then printf 'holds the keys'; else printf 'lacks "%s"' "$missing"; fi); the control ${3:-(not taken by Dispatch)}: deliveries $control (want one queued naming no session), nothing of $session's in reply to it: $nothing; the message to the role's topic ${5:-(not sent)}: $role_replies reply (want one), holding the keys: $role_keys, answered as $session: $role_by ($(said_text "$role_reply")); the claims of $1 $(issue_claim_roles "$claims" "$1"); panes $4 → $panes"
  [ "$held" = yes ] && one_delivery_for "$deliveries" sent "$session" &&
    one_delivery_for "$control" queued '' && [ "$nothing" = yes ] &&
    [ "$role_keys" = yes ] && [ "$role_replies" = 1 ] && [ "$role_by" = yes ] &&
    tree_claims_in "$claims" "$1" suspended failed retired && listed_claim_is "$claims" "$token" failed &&
    [ "$panes" -le "$4" ]
}
# a21_nothing_posted ISSUE CONTROL [COMMENTS] is the part of A21's read that says the failed
# implementer posts nothing under the control: its session wrote no reply on ISSUE to the mention
# CONTROL of a role no session took. A21's control places such a reply, in that session's name and
# carrying the loop's mark, and this read, over ISSUE's comments as the control read them once
# (COMMENTS; read now where not given), must see it. A CONTROL Dispatch took none of shows nothing.
a21_nothing_posted() {
  local reply replied=no
  if reply=$(comment_reply_by "$1" "${2:-}" "${agent[$store:failed_implementer.session]}" "${3:-}" 2>/dev/null); then
    replied=yes
  else
    reply=
  fi
  read_said="the failed implementer's reply to ${2:-(no mention)} on $1: $(said_text "$reply")"
  [ -n "${2:-}" ] && [ "$replied" = no ]
}
# a21_first_holder ISSUE is A21's last leg, apart from its read, the pane baseline and the control: a
# message kept for a role nobody holds reaches the first session to hold it. The proof person
# mentions, on ISSUE, a role of ISSUE no session of the run took, its tester, with a marker of its own
# (a21_holder_body); Dispatch keeps that mention for the role's first holder, its one delivery
# settling `queued` naming no session (a21_holder_kept); then the rig's third stand-in session, the
# first holder, registers with a consumer of its own and claims the role (a21_holder_claim), and the
# marker must reach that session's inbox, which the listener relays from the role's kept lane within
# the hour it keeps a copy for. This rig waits 180 s. Its read is a21_holder_read.
a21_first_holder() {
  local role marker=answer-loop-a21-holder-$run_tag-$store comment id claim='' queued=no
  role=$(claim_token "$1" tester)
  comment=$(dispatch_human POST "issues/$1/comments" "$(a21_holder_body tester "$role" "$marker")") || comment='{}'
  id=$(jq -r '.id // empty' <<<"$comment")
  if [ -n "$id" ] && soft_wait 60 "the mention of the unclaimed role:$role to settle queued, naming no session" a21_holder_kept "$id"; then queued=yes; fi
  if [ "$queued" = yes ] && claim=$(a21_holder_claim "$role"); then
    soft_wait 180 "the mention kept for role:$role to reach its first holder $holder_session" inbox_holds "$holder_session" "$marker" || true
  fi
  a21_holder_read "$role" "$id" "$marker" "$claim" "$queued"
}
# a21_holder_body NAME TOKEN MARKER is the proof person's comment of A21's first holder's leg: a
# mention of role:TOKEN, the issue's role NAME, carrying MARKER.
a21_holder_body() {
  jq -cn --arg body "@$1 A message for the role's first holder ($3)" --arg t "role:$2" '{body: $body, mentions: [{target: $t}]}'
}
# a21_holder_kept ID: the mention ID's one delivery settled `queued` naming no session, as Dispatch
# keeps a mention of a role nobody holds for the role's first holder.
a21_holder_kept() { one_delivery_for "$(comment_delivery_records "${1:-}")" queued ''; }
# a21_holder_claim TOKEN registers the first holder's session (holder_session) with a consumer of its
# own and claims role TOKEN for it, and prints the listener's answer to the claim, nothing where it
# gave none. It fails when the registration does.
a21_holder_claim() {
  local claim
  register_stand_in "$holder_session" "answer-loop rig first holder" || return 1
  claim=$(envoy --data "$(jq -cn --arg s "$holder_session" --arg role "$1" --arg instance "$stand_in_instance" \
    '{session_id: $s, role: $role, soft: true, client_instance_id: $instance}')" "$(listener_url /v1/roles/set)") || claim=
  printf '%s\n' "$claim"
}
# a21_holder_read TOKEN ID MARKER CLAIM [QUEUED [INBOX]] is the read of A21's first holder's leg: the
# mention ID of role:TOKEN, a role no session of the run took, settled `queued` naming no session
# (a21_holder_kept; QUEUED, yes or no, is that as the row read it before the first holder claimed the
# role, or as A21's control read the mention's record once, read now where not given), and the first
# holder's inbox holds MARKER, the mention's own. CLAIM is the listener's answer to the first
# holder's claim of a role, which the read says. A21's control runs it over a mention of a role the
# first holder did not claim, whose marker must then not reach it, and over the inbox it read once
# (INBOX, inbox_listing; read now where not given).
a21_holder_read() {
  local queued=${5:-} reached=no
  if [ -z "$queued" ]; then
    queued=no
    a21_holder_kept "${2:-}" && queued=yes
  fi
  inbox_holds "$holder_session" "${3:-}" "${6:-}" && reached=yes
  read_said="a mention of role:$1, which no session of the run took (comment ${2:-(not taken)}), settled queued naming no session: $queued; the first holder $holder_session's claim: $(said_text "${4:-}"); its inbox holds ${3:-(no marker)}: $reached"
  [ "$queued$reached" = yesyes ]
}

# ---- A22: the loop's dispatch tool -----------------------------------------------------------------
# check_a22: a stopped agent answers with its dispatch tool, which runs the pane's own `dispatch`
# command as the agent's session (daemon_dispatch). The agent is the planner, stopped since the pass
# made it, on its own issue. After it stopped the proof person writes a comment on that issue
# holding a fact its conversation never held (questions.json's a22-late-comment `comment`), then
# asks it, by a Dispatch message on the issue as A17's person asks, which codename the comment
# names: the answer must hold it, and one `answers: dispatch` line of that answer must be a read
# (read, doc-read or search) that ran as the planner's session and exited 0. Asked next to post a
# comment whose body is the run's marker on a new issue of the pass, mentioning the replaced
# tester's session, the planner posts it there in its own name, the dispatch tool's mark in its
# body, and the loop answers nothing of it: the mention reaches the replaced tester's daemon, which
# takes it for no question because it carries the loop's own mark, and that session writes no
# reply to it. Asked then to set that issue's status, to claim it and to edit its document, the
# tool refuses each before dispatch starts, the reply says so, and the issue's status and claim do
# not move. Every dispatch line of the three answers names the planner's session. The boot line
# `answers: tools` and `legion config check` list dispatch for this daemon; then the daemon boots
# once with no dispatch command (no LEGION_DISPATCH_PATH, none on its PATH, none at the worker
# image's /opt/legion/pi-envoy/bin/dispatch), and both say dispatch is absent and why, before the
# daemon boots again as it was. A host that holds a dispatch command where the daemon looks cannot
# show that, and the row fails naming it. Each leg's read is a function of its own, which A22's
# control runs over messages of its own (run_controls).
a22_own_answer='the loop'"'"'s own answer'
# a22_post_request ISSUE MARKER SESSION is the proof person's message asking the planner to post a
# comment on ISSUE whose body is MARKER, mentioning SESSION.
a22_post_request() {
  printf 'Using your dispatch tool, post a comment on issue %s whose body is exactly %s, mentioning the session %s: the arguments are ["comment", "--issue", "%s", "--body", "%s", "--mention", "session:%s"]. Then tell me what you did.' \
    "$1" "$2" "$3" "$1" "$2" "$3"
}
# a22_refusal_request ISSUE is the proof person's message asking the planner for a status write, a
# claim and a document edit of ISSUE, which its dispatch tool must refuse.
a22_refusal_request() {
  printf 'I am testing that your dispatch tool refuses these writes. Make each of these three calls with your dispatch tool, even though each is expected to be refused, and then tell me word for word what each answered: 1. set the status of issue %s, the arguments ["issue-update", "--issue", "%s", "--status", "in_progress"]; 2. claim issue %s, the arguments ["claim", "--issue", "%s"]; 3. edit its document, the arguments ["doc-edit", "--issue", "%s", "--artifact", "spec", "--ops-json", "[]"].' \
    "$1" "$1" "$1" "$1" "$1"
}
# a22_own_skipped SESSION: how many hand-offs of SESSION this boot took for no question because
# they carried the loop's own mark.
a22_own_skipped() { answers_lines "answers: hand-off is no question; not answered" "$1" "" "$a22_own_answer"; }
# a22_read_leg BODY [MESSAGES] is the read of A22's first leg: the planner answered the person's
# message BODY with a22-late-comment's keys, one `answers: dispatch` line of that answer is a read
# that ran as the planner's session and exited 0, and every dispatch line of it names that session.
# MESSAGES, in each leg's read, are the proof person's messages to the planner as A22's control read
# them once (message_listing), read now where not given.
a22_read_leg() {
  local session=${agent[$store:planner.session]} reply question lines keys=no
  reply=$(message_reply_by "$session" "$1" "${2:-}" 2>/dev/null) || reply=
  [ -n "$reply" ] && answer_holds a22-late-comment "$reply" >/dev/null && keys=yes
  question=$(answer_question_id "$reply")
  lines=$(question_dispatch_lines "$question") || lines='[]'
  read_said="the planner's reply: $(said_text "$reply"); it holds a22-late-comment's keys: $keys; the dispatch lines of its answer ${question:-(none)}: $(jq -c 'map({seq, session, command, exit, ran})' <<<"$lines" 2>/dev/null || printf '%s' "$lines")"
  [ "$keys" = yes ] && first_line_where "$lines" dispatch_read_line_is "$session" >/dev/null && lines_all_as "$lines" "$session"
}
# a22_post_leg BODY ISSUE MARKER MENTIONED SKIPPED [MESSAGES [COMMENTS]] is the read of A22's second
# leg: the planner answered the message BODY; the comments of ISSUE holding MARKER are one, in the
# planner's own name, carrying the dispatch tool's mark; every dispatch line of the answer names the
# planner's session; and the loop answered nothing of the comment: the MENTIONED session's daemon
# took more than SKIPPED hand-offs for no question as the loop's own answer, and that session wrote
# no reply to the comment (a22_nothing_posted). COMMENTS are ISSUE's comments as A22's control read
# them once, read now where not given.
a22_post_leg() {
  local session=${agent[$store:planner.session]} reply lines comments all=${7:-} skipped nothing=no
  reply=$(message_reply_by "$session" "$1" "${6:-}" 2>/dev/null) || reply=
  lines=$(question_dispatch_lines "$(answer_question_id "$reply")") || lines='[]'
  [ -n "$all" ] || all=$(dispatch_get "issues/$2/comments" 2>/dev/null) || all=
  comments=$(issue_comments_in "$all" "$3")
  skipped=$(($(a22_own_skipped "$4") - $5))
  a22_nothing_posted "$2" "$3" "$4" "$all" && nothing=yes
  read_said="the planner's reply: $(said_text "$reply"); comments on $2 holding $3: $(jq -c 'map({id, author, mark: (.body | test("legion-answer:[0-9a-f]+\\.[0-9]+ -->"))})' <<<"$comments" 2>/dev/null || printf '%s' "$comments"); the dispatch lines of the answer: $(jq -c 'map({seq, session, command, exit, ran})' <<<"$lines" 2>/dev/null || printf '%s' "$lines"); hand-offs of $4 taken for no question as the loop's own answer since: $skipped; nothing of $4's in reply to the comment: $nothing"
  [ -n "$reply" ] && one_tool_marked_comment_by "$comments" "$session" && lines_all_as "$lines" "$session" &&
    [ "$skipped" -gt 0 ] && [ "$nothing" = yes ]
}
# a22_nothing_posted ISSUE MARKER MENTIONED [COMMENTS] is the part of A22's second leg that says the
# loop answers nothing of the comment the planner posted: the first of ISSUE's comments holding
# MARKER has no reply in the MENTIONED session's name, where the loop would post its answer to the
# mention. COMMENTS are ISSUE's comments, read now where not given. A22's control places the comment
# the dispatch tool would post, carrying the control's marker and the tool's mark, and under it such
# a reply, carrying the loop's mark, and this read must see it. An ISSUE holding no comment with
# MARKER shows nothing.
a22_nothing_posted() {
  local all=${4:-} posted reply replied=no
  [ -n "$all" ] || all=$(dispatch_get "issues/$1/comments" 2>/dev/null) || all=
  posted=$(jq -r '.[0].id // empty' <<<"$(issue_comments_in "$all" "$2")" 2>/dev/null) || posted=
  if [ -n "$posted" ] && reply=$(comment_reply_in "$all" "$posted" "$3" 2>/dev/null); then
    replied=yes
  else
    reply=
  fi
  read_said="a reply of $3 to ${posted:-(no comment holding $2)} on $1: $(said_text "$reply")"
  [ -n "$posted" ] && [ "$replied" = no ]
}
# a22_refusal_leg BODY ISSUE STANDING [MESSAGES] is the read of A22's third leg: the planner
# answered the message BODY saying the writes were refused; the answer's dispatch lines hold a
# refusal, as the planner's session, of a status write (issue-update), a claim (claim) and a
# document edit (doc-edit), each refused before dispatch started as none of the commands the tool
# runs, and every dispatch line of it names that session; and ISSUE's status and claim are STANDING
# still.
a22_refusal_leg() {
  local session=${agent[$store:planner.session]} reply lines command refused='' said='' now
  reply=$(message_reply_by "$session" "$1" "${4:-}" 2>/dev/null) || reply=
  lines=$(question_dispatch_lines "$(answer_question_id "$reply")") || lines='[]'
  for command in issue-update claim doc-edit; do
    if first_line_where "$lines" dispatch_refusal_is "$session" "$command" >/dev/null; then
      refused+="${refused:+ }$command"
    fi
  done
  now=$(issue_standing "$2")
  refusal_mentioned "$reply" && said=yes || said=no
  read_said="the planner's reply: $(said_text "$reply"); it says the writes were refused: $said; refused as none of the tool's commands: ${refused:-none} (want issue-update claim doc-edit); the dispatch lines of the answer: $(jq -c 'map({seq, session, command, ran})' <<<"$lines" 2>/dev/null || printf '%s' "$lines"); $2's status and claim $3 → $now"
  [ "$said" = yes ] && [ "$refused" = "issue-update claim doc-edit" ] && lines_all_as "$lines" "$session" &&
    standing_unchanged "$3" "$now"
}
# tools_leg TOOL WHY WITHOUT TOOLS CHECK ABSENT_TOOLS ABSENT_CHECK is the read of a configuration leg
# (A22's, A23's): the daemon's boot line TOOLS and `legion config check`'s line CHECK list TOOL, and
# those of a daemon with no WITHOUT, ABSENT_TOOLS and ABSENT_CHECK, list none and say WHY.
tools_leg() {
  read_said="the boot's tools line: ${4:-none}; config check: ${5:-none}; with no $3, the boot's tools line: ${6:-none}; config check: ${7:-none}"
  tools_line_offers "$4" "$1" && check_tools_lists "$5" "$1" && tools_line_absent "$6" "$1" "$2" &&
    check_tools_absent "$7" "$1" "$2"
}
# a22_tools_leg TOOLS CHECK ABSENT_TOOLS ABSENT_CHECK is A22's: the dispatch tool, absent from the
# daemon with no dispatch command.
a22_tools_leg() { tools_leg dispatch 'no dispatch command on PATH or at /opt/legion/pi-envoy/bin/dispatch' 'dispatch command' "$@"; }
# host_dispatch prints the dispatch command a daemon of this run would find with no
# LEGION_DISPATCH_PATH, on its PATH or at the worker image's install, and nothing where it finds
# none (daemon.answersDispatch's search).
host_dispatch() {
  local dir
  local IFS=:
  for dir in $daemon_path; do
    [ ! -x "$dir/dispatch" ] || [ -d "$dir/dispatch" ] || {
      printf '%s\n' "$dir/dispatch"
      return 0
    }
  done
  [ ! -x /opt/legion/pi-envoy/bin/dispatch ] || printf '%s\n' /opt/legion/pi-envoy/bin/dispatch
}
check_a22() {
  criterion A22 "a stopped agent reads a comment written after it stopped with its dispatch tool, posts where asked in its own name with the loop's mark and the loop answers none of it, the tool refuses a status write, a claim and a document edit, the log names each call's session, and a daemon with no dispatch command offers no dispatch tool" || return 0
  # The post leg's comment; the read leg reads, and the refusal leg's three writes are refused.
  asks_writes "dispatch comment"
  local planner=${agent[$store:planner.session]} issue=${agent[$store:planner.issue]} mentioned=${agent[$store:tester_old.session]}
  local target marker comment read_body post_body refuse_body standing skipped tools check absent_tools='' absent_check='' found keep
  local ok_read=0 ok_post=0 ok_refuse=0 ok_tools=0 said=''
  tools=$(boot_tools_line)
  check=$(config_check_tools "$daemon_config")
  target=$(new_rig_issue "Answer loop proof: A22's target ($store)")
  marker=answer-loop-a22-$run_tag-$store
  agent[$store:a22.target]=$target
  # The fact the planner's conversation never held, written once the planner stopped.
  comment=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg body "$(question a22-late-comment | jq -r .comment)" '{body: $body}')" |
    jq -r '.id // empty') || comment=
  note "the proof person's comment ${comment:-(not taken)} on $issue, written after the planner $planner stopped"
  read_body="$(question_text a22-late-comment) (question a22-late-comment of run $run_tag)"
  if person_message "$issue" "$planner" "$read_body"; then
    soft_wait 300 "the planner's answer from the comment" message_reply_by "$planner" "$read_body" || true
  fi
  a22_read_leg "$read_body" && ok_read=1
  note "$read_said"
  said+="the late comment read: $read_said; "
  skipped=$(a22_own_skipped "$mentioned")
  post_body="$(a22_post_request "$target" "$marker" "$mentioned") (run $run_tag, $store)"
  if person_message "$issue" "$planner" "$post_body"; then
    soft_wait 300 "the planner's answer about the comment it posted" message_reply_by "$planner" "$post_body" || true
    soft_wait 120 "the mention of $mentioned in the posted comment to be taken for no question" more_than "$skipped" a22_own_skipped "$mentioned" || true
  fi
  a22_post_leg "$post_body" "$target" "$marker" "$mentioned" "$skipped" && ok_post=1
  note "$read_said"
  said+="the comment posted: $read_said; "
  standing=$(issue_standing "$target")
  refuse_body="$(a22_refusal_request "$target") (run $run_tag, $store)"
  if person_message "$issue" "$planner" "$refuse_body"; then
    soft_wait 300 "the planner's answer about the writes its tool refused" message_reply_by "$planner" "$refuse_body" || true
  fi
  a22_refusal_leg "$refuse_body" "$target" "$standing" && ok_refuse=1
  note "$read_said"
  said+="the writes refused: $read_said; "
  # A daemon with no dispatch command: booted once without LEGION_DISPATCH_PATH, then as it was.
  found=$(host_dispatch)
  if [ -n "$found" ]; then
    note "this host holds a dispatch command where the daemon looks, $found, so no daemon of it can boot without one"
    said+="this host holds a dispatch command where the daemon looks ($found), so no daemon can show the tool absent; "
  else
    keep=$daemon_dispatch
    stop_daemon
    daemon_dispatch=
    start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
    absent_tools=$(boot_tools_line)
    absent_check=$(config_check_tools "$work/answers-$store.yaml")
    agent[$store:a22.absent_tools]=$absent_tools
    agent[$store:a22.absent_check]=$absent_check
    stop_daemon
    daemon_dispatch=$keep
    start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
  fi
  a22_tools_leg "$tools" "$check" "$absent_tools" "$absent_check" && ok_tools=1
  note "$read_said"
  said+="the tools: $read_said"
  if [ "$ok_read$ok_post$ok_refuse$ok_tools" = 1111 ]; then
    record_result A22 PASS "the planner read the comment written on $issue after it stopped with its dispatch tool, as its own session, and answered with its codename; it posted the comment asked for on $target in its own name, the dispatch tool's mark in its body, and the loop answered nothing of it; its tool refused the status write, the claim and the document edit, the reply said so, and $target's status and claim did not move; the log named the planner's session on every dispatch call; the boot line and legion config check listed dispatch, and with no dispatch command listed none and said why"
  else
    record_result A22 FAIL "the comment read $ok_read, the comment posted and answered by nothing $ok_post, the writes refused $ok_refuse, the tool offered and absent $ok_tools (1 = shown): $said"
  fi
}

# ---- A23: the loop's web_search tool ---------------------------------------------------------------
# check_a23: a stopped agent answers with its web_search tool (answers.web_search, which
# write_legion_config names: web_search_model at web_search_fee a search). The architect is asked
# for a fact a web search finds (questions.json's a23-web-search), on a public page that is not on
# GitHub, and to cite the address of the search result it used. The row judges that a search ran and
# that every address the answer cites came out of its searches, none reached with a tool, not
# whether the fact is right: an `answers: web_search` line of the answer made a search; the
# recording proxy holds the Messages call that search made, to answers.providers.anthropic under
# the daemon's own label, offering Anthropic's web search server tool (`web_search_20250305`) and
# answered 200; every address the answer names is among the results of the searches that ran, taken
# together, and no `answers: gh` or `answers: dispatch` call of the answer names one in its
# arguments (citation_faults); each search line's cost is its call's tokens at the search model's
# price plus the fee for each search it ran, and the answer's cost line counts those searches and
# their cost (search_line_recomputes, search_fee_carried); and the architect, resumed, holds each
# search's call and result: its first request holds a call of web_search with each search's query
# and an address it returned (request_holds_search), as A9 reads a resumed agent, so an answer whose
# transcript holds no web_search call fails. Then a fourth search in one answer is refused: the
# daemon boots with model_calls 2 and the proxy's stand-in step calls web_search four times
# (a12_stand_in four_searches), as A12 drives a reply no model gives on demand; one `answers:
# web_search` line of that answer is the refusal. Then the daemon boots once with no
# answers.web_search, and its boot line and `legion config check` list no web_search and say why; the
# boot line and the check of the daemon as it was list it. The same search leg runs for the GPT-6.1
# merger where the route serves GPT-6.1, a row of its own as A10's GPT-6.1 clause is, and as that
# clause does, on the daemon gpt_bound_leg boots for it, its answer waited for gpt_answer_wait.
# a23_search_read EVENT M LABEL NAME is the read of a search leg of A23: the reply to EVENT, NAME's
# answer, made at least one search; each search line that ran has its call at the proxy, answered
# 200 with the server tool offered, and recomputes from it; every address the reply cites is among
# the results of the answer's searches that ran, all together, and none is named by a gh or dispatch
# call line of the answer (citation_faults, which names the address and the way it broke); the
# answer's cost line carries the searches' cost; and NAME's first request after its first M model
# calls on LABEL holds a web_search call with each search's query and an address its result
# returned.
a23_search_read() {
  local reply message question lines answered calls line call query url faults results='[]' ran=0 recomputed=yes resumed=yes request gh dispatch
  reply=$(inbox_reply "$1" 2>/dev/null) || reply=
  message=$(reply_message "$reply")
  question=$(answer_question_id "$reply")
  lines=$(question_search_lines "$question") || lines='[]'
  gh=$(question_gh_lines "$question") || gh='[]'
  dispatch=$(question_dispatch_lines "$question") || dispatch='[]'
  answered=$(answered_line "$question" || true)
  calls=$(search_calls "daemon-$store")
  request=$(first_request_after "$3" "$2")
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    query=$(jq -r '.query // ""' <<<"$line")
    call=$(search_call_for "$calls" "$query")
    if [ "$(jq -r '(.searches // 0) > 0 and (.error // "") == ""' <<<"$line")" = true ]; then
      ran=$((ran + 1))
      [ "$(jq -r '.status // 0' <<<"$call")" = 200 ] || recomputed=no
      results=$(jq -c --argjson c "$call" '. + ($c.urls // []) | unique' <<<"$results")
      url=$(jq -r '.urls[0] // ""' <<<"$call")
      request_holds_search "$request" "$query" "$url" || resumed=no
    fi
    [ "$call" = null ] || search_line_recomputes "$line" "$call" "$web_search_fee" || recomputed=no
  done < <(jq -c '.[]?' <<<"$lines" 2>/dev/null)
  faults=$(citation_faults "$message" "$results" "$gh" "$dispatch")
  read_said="$4's reply to ${1:-(no event)}: $(said_text "$message"); the web_search lines of its answer ${question:-(none)}: $(jq -c 'map({seq, query, searches, costUSD, error})' <<<"$lines" 2>/dev/null || printf '%s' "$lines"), $ran of them a search that ran; each with its call answered 200 and its cost its call's tokens and its fee: $recomputed; the addresses the searches returned: $results; what is wrong with the addresses the reply cites: ${faults:-nothing}; the cost line: $(jq -c '{webSearches, webSearchUSD, costUSD}' <<<"${answered:-null}" 2>/dev/null || printf none); $4's first request on $3 after $2 holds a web_search call and its result for each search: $resumed"
  [ "$ran" -gt 0 ] && [ "$recomputed" = yes ] && [ -z "$faults" ] && [ "$resumed" = yes ] &&
    search_fee_carried "$answered" "$lines" "$web_search_fee"
}
# a23_fourth_read EVENT is the read of A23's fourth search: one `answers: web_search` line of the
# answer the reply to EVENT names is the tool's refusal of a fourth search in one answer.
a23_fourth_read() {
  local question lines refused
  question=$(answer_question_id "$(inbox_reply "${1:-}" 2>/dev/null || true)")
  lines=$(question_search_lines "$question") || lines='[]'
  refused=$(search_refused "$lines") || refused=
  read_said="the web_search lines of the answer ${question:-(none)}: $(jq -c 'map({seq, searches, error})' <<<"$lines" 2>/dev/null || printf '%s' "$lines"); the fourth refused: ${refused:-no}"
  [ -n "$refused" ]
}
# a23_tools_leg TOOLS CHECK OFF_TOOLS OFF_CHECK is A23's: the web_search tool, absent from the daemon
# whose answers set no web_search.
a23_tools_leg() { tools_leg web_search 'no answers.web_search block' 'answers.web_search' "$@"; }
# a23_search_leg NAME ID MARKER LABEL WAIT asks NAME the web search question ID under MARKER, waits
# up to WAIT seconds for the answer, resumes NAME on its own route LABEL with a one-word task, waits
# for its first model call, reads the leg (a23_search_read) and suspends NAME again. It returns the
# read's verdict.
a23_search_leg() {
  local name=$1 sent event m ok=1
  sent=$(envoy_ask_text session "${agent[$store:$name.session]}" "$(question_text "$2") $(question_suffix "$2" "$run_tag" "$3")")
  event=${sent#* }
  note "$name: $2 ($3): listener $sent"
  soft_wait "$5" "$name's answer to $2" inbox_reply "$event" || true
  m=$(proxy_count "$4")
  resume_agent "$name" || true
  deliver_task "${agent[$store:$name.token]}" "Reply with one word: ready." || true
  soft_wait 300 "the resumed $name's first model call" more_than "$m" proxy_count "$4" || true
  a23_search_read "$event" "$m" "$4" "$name" && ok=0
  note "$read_said"
  suspend_agent "$name" || true
  return "$ok"
}
check_a23() {
  local tools check off_tools off_check sent event ok_search=0 ok_fourth=0 ok_tools=0 said=''
  if criterion A23 "the GPT-6.1 merger answers with its web_search tool, every address it cites among its searches' results and none reached with gh or dispatch, the fee in its cost line, and resumes holding the search" openai; then
    if [ -n "${agent[$store:gpt.why]:-}" ] || [ -z "${agent[$store:gpt.session]:-}" ]; then
      record_result A23 FAIL "GPT-6.1: ${agent[$store:gpt.why]:-the merger registered no session}"
    else
      agent[$store:a23.gpt]=1
      if gpt_bound_leg "A23's GPT-6.1 search leg" a23_search_leg gpt a23-web-search-gpt A23-gpt agents-gpt "$gpt_answer_wait"; then
        record_result A23 PASS "GPT-6.1: the merger searched with its web_search tool, every address its answer cites is among its searches' results and none is named by a gh or dispatch call, each search's cost line is its tokens and its fee and the answer's cost line carries them, and resumed in Oh My Pi it holds the search's call and result; $gpt_bound_said: $read_said"
      else
        record_result A23 FAIL "GPT-6.1: $read_said"
      fi
    fi
  fi
  criterion A23 "a stopped agent answers with its web_search tool, every address it cites among its searches' results and none reached with gh or dispatch, the fee in its cost line, a fourth search refused, and resumes holding the search; a daemon whose answers set no web_search offers no web_search tool" || return 0
  tools=$(boot_tools_line)
  check=$(config_check_tools "$daemon_config")
  a23_search_leg architect a23-web-search A23 agents 300 && ok_search=1
  said+="the search: $read_said; "
  # A fourth search in one answer: one model step of the proof's own calls web_search four times.
  stop_daemon
  write_legion_config "$work/a23-$store.yaml" yes "{ model_calls: 2 }"
  a12_stand_in four_searches
  start_daemon "$work/branch/legion" "$work/a23-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
  sent=$(envoy_ask session "${agent[$store:planner.session]}" a23-fourth-search "a23-fourth-$run_tag-$store")
  event=${sent#* }
  soft_wait 300 "the loop's fourth search to be refused" fault_passed_on model_calls && soft_wait 60 "the daemon's reply" a23_fourth_read "$event" || true
  a23_fourth_read "$event" && ok_fourth=1
  note "$read_said"
  said+="the fourth search: $read_said; "
  a12_stand_in none
  # A daemon whose answers set no web_search.
  stop_daemon
  write_legion_config "$work/a23-off-$store.yaml" yes "" no
  start_daemon "$work/branch/legion" "$work/a23-off-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
  off_tools=$(boot_tools_line)
  off_check=$(config_check_tools "$work/a23-off-$store.yaml")
  agent[$store:a23.off_tools]=$off_tools
  agent[$store:a23.off_check]=$off_check
  stop_daemon
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
  a23_tools_leg "$tools" "$check" "$off_tools" "$off_check" && ok_tools=1
  note "$read_said"
  said+="the tools: $read_said"
  if [ "$ok_search$ok_fourth$ok_tools" = 111 ]; then
    record_result A23 PASS "the architect searched with its web_search tool, every address its answer cites is among its searches' results and none is named by a gh or dispatch call, each search's cost line is its tokens at $web_search_model's price and its fee of $web_search_fee and the answer's cost line carries them, and resumed it holds each search's call and result; a fourth search in one answer was refused; the boot line and legion config check listed web_search, and with no answers.web_search listed none and said why"
  else
    record_result A23 FAIL "the search $ok_search, the fourth search refused $ok_fourth, the tool offered and absent $ok_tools (1 = shown): $said"
  fi
}

# ---- the control rows ------------------------------------------------------------------------------
# Every answering row that ran in a pass (rows_ran) has a control row, `A<n> control`, run in A13's
# phase with `answers` removed: the branch's daemon on plain-<store>.yaml, after A13's own probes of
# that store (run_controls). A control row reads PASS only when its row's own read failed, so a
# control does every step that read depends on, and a daemon that still answered would make the read
# hold: a read that fails whatever the daemon does proves nothing. For each row the control sends a
# message of its own to that row's stopped agent, of the kind the row sends, under its own question
# suffix (control_suffix: `(question <id> of run <tag>, control of A<n>)`), and once every control's
# message is sent and one wait of control_wait has passed, runs the row's own read over what follows
# that message alone: its event, its comment, its marker. A control row's detail quotes what each
# part of that read said. What a row's read needs besides the message is done for it:
#   - a stopped agent's first request after a resume (A9, A10, A23's search legs): after every read
#     that counts claims or panes, each agent is resumed once with the task its row resumes it
#     with, waited for as the row waits, and every read that needs it is run in that one resume
#     (control_agents). A10's Claude Code leg reads the conversation as it stood before the
#     control's first question, kept under its own name, as the row does;
#   - A7's forwarded leg: the row's hold-then-resume, replayed in the architect's one resume. Its
#     running-agent leg's read that the loop posted nothing is run over row A1's thread, where the
#     loop answered a stopped agent, and must fail on that answer (control_read_a7);
#   - A12's out-of-form leg and A23's fourth search: the proxy's stand-in step the row uses, each in
#     a window of its own after every other read, since a stand-in answers every call of the
#     daemon's label (control_window_a12, control_window_a23). A12's four legs that a bound of
#     `answers.bounds` makes cannot be reproduced where no answers block exists, and are not read;
#   - A19: the round recorded as reviewing again with the daemon booted on it (control_prepare_a19),
#     and the approval the reviewer posted delivered as the row delivers it (control_read_a19);
#   - A21: the row's own role mention of a role no session took, and its message to the failed
#     role's topic, both of its own; and for its first holder's leg a mention of another role of the
#     issue no session took, its merger, under a marker of its own, with the first holder registered
#     again and claiming the leg's role, its tester, and not that one (control_send_a21);
#   - A22 and A23's configuration legs: the row's own absent and off lines, with this daemon's boot
#     line and `legion config check` line, each read once for the pass.
# For a part of a row that says the loop posts nothing the control places, where the row looks,
# a post carrying the loop's mark (control_mark), after the row's main read, which holds that
# part too, but for A8's, which go in with the control's messages (control_send_a8): a reply
# on the asker's topic in reply to the control's own question (A7, A12's out-of-form leg,
# A18's resume leg); a comment in the name of the session whose reply the row reads none
# of (place_comment): the tester's in A20's thread under the control's reply, the failed
# implementer's under A21's control mention, and on A22's target the comment the planner's
# dispatch tool would post, in the planner's name with the control's marker and the tool's mark,
# and under it a reply in the mentioned session's name; for A6's second leg an agent-decision
# notice of the control's reply on the controller stand-in's topic (place_controller_notice);
# for A17's leak-guard leg a comment on the implementer's pull request as the implement App,
# carrying the run's refusal marker (control_read_a17); and, for A8, whose read of that part is
# the daemon's log, a JSON line appended to the log naming the session and saying that no daemon
# wrote it (place_log_line). That part's own read must then fail too. A21's first holder's leg,
# whose read says a kept message reaches the role's first holder, is read over its control's
# mention of a role the first holder did not claim, whose marker must not reach it. A control
# whose message was not taken reads FAIL, since a read of nothing shows nothing.
# A read fails as surely over a listing nobody could read, or one that holds nothing, as over the
# control's own message or placement, so a control counts a read's failure only over the listing it
# read once where that read looks for what the loop would post, found holding what the control sent
# or placed there, and handed to the read (control_seen): an ask's thread or a comment's thread
# holding the control's reply, an issue's comments its mention and its placed comments, the proof
# person's messages to a session the control's message, a pull request's comments the placed
# comment, the inbox the placed reply or notice, the daemon's log the placed lines. A listing the
# control's own message cannot stand in (the asker's inbox, which holds replies alone, a daemon
# log a daemon with answers removed writes nothing of the question to, a pull request's comments
# or reviews before any placement) must have been read and hold entries. A control whose listing
# does not show that reads FAIL however its read ends.
# control_suffix ID ROW: the suffix of the control of ROW's question ID.
control_suffix() { question_suffix "$1" "$run_tag" "control of $2"; }
# control_question ROW: the id of the question a placement of ROW's control names, an answer id of
# the run's own no answer of the loop's names.
control_question() { printf 'c0de%s%s' "$run_tag" "${1#A}"; }
# control_mark ROW: the loop's marker as a placement of ROW's control carries it.
control_mark() { printf '<!-- legion-answer:%s -->' "$(control_question "$1")"; }
# control_tool_mark ROW: the loop's dispatch tool's mark, `<!-- legion-answer:<question>.<seq> -->`,
# as a comment a placement of ROW's control stands in for carries it, its seq the first call's.
control_tool_mark() { printf '<!-- legion-answer:%s.1 -->' "$(control_question "$1")"; }
# control_notice COMMENT ROW: an agent-decision notice of the person's reply COMMENT, as the loop
# passes a decision on to the controller, saying that ROW's control placed it and carrying the
# loop's mark (place_controller_notice).
control_notice() {
  jq -cn --arg c "$1" --arg r "$2" --arg m "$(control_mark "$2")" \
    '{kind: "agent-decision", agent_decision: {id: ("dispatch:" + $c), text: ("Placed by scripts/e2e/answer-loop.sh, the control of " + $r + "; the loop passed no decision on here. " + $m)}}'
}
# control_not_taken ROW WHAT notes that WHAT of ROW's control was not taken, keeps why and marks the
# control unsent, so that it reads FAIL. It fails.
control_not_taken() {
  note "$1 control: $2: (not taken)"
  control[$store:$1.why]+="${control[$store:$1.why]:+; }$2 was not taken"
  control[$store:$1.unsent]=1
  return 1
}
# control_sent ROW WHAT VALUE notes what ROW's control sent, and is control_not_taken when VALUE,
# what the send was answered with, is empty: the message was not taken.
control_sent() {
  if [ -n "${3:-}" ]; then
    note "$1 control: $2: $3"
  else
    control_not_taken "$1" "$2" || return 1
  fi
}
# control_seen ROW WHAT LISTING ENTRIES [MATCH [VALUE...]] checks LISTING, the listing a read of
# ROW's control is handed (WHAT names it), for what the control sent or placed there
# (listing_fault). Where it does not show that, it notes why, keeps it and marks the control unseen,
# so that it reads FAIL however that read ends, and fails. control_inbox_seen ROW WHAT and
# control_log_seen ROW WHAT are its check of the inbox (inbox_listing) and of this boot's daemon log
# (boot_listing), where the control's own question cannot stand, read now: each must have been read
# and hold entries. WHAT says what the read looks for there.
control_seen() {
  local row=$1 what=$2 fault
  shift 2
  fault=$(listing_fault "$@") && return 0
  note "$row control: $what: $fault"
  control[$store:$row.why]+="${control[$store:$row.why]:+; }$what: $fault"
  control[$store:$row.unseen]=1
  return 1
}
control_inbox_seen() { control_seen "$1" "the inbox, where $2" "$(inbox_listing)" .; }
control_log_seen() { control_seen "$1" "this boot's daemon log, where $2" "$(boot_listing)" .; }
# read_failed ROW WHAT READ ARG... runs a row's read, READ ARG..., for ROW's control, and adds what
# the read said under WHAT to what ROW's control says (control[<store>:ROW.said]) either way. A read
# that holds marks the control held (control[<store>:ROW.held]): the control then reads FAIL. It
# always succeeds, so that the parts of one control, which may be read in different phases, all run.
read_failed() {
  local row=$1 what=$2
  shift 2
  read_said=
  if "$@"; then
    control[$store:$row.held]=1
    control[$store:$row.said]+="${control[$store:$row.said]:+; }$what held: $read_said"
  else
    control[$store:$row.said]+="${control[$store:$row.said]:+; }$what failed: $read_said"
  fi
}
# place_inbox_reply EVENT TEXT places TEXT on the asker's topic in reply to EVENT, a send in the
# controller stand-in's name that names EVENT as the message it answers, as the loop's answer names
# its question: what a row reading the asker's replies to EVENT would find had the loop posted one.
place_inbox_reply() {
  local code
  code=$(envoy -o /dev/null -w '%{http_code}' --data "$(jq -cn --arg s "$asker_session" --arg from "$controller_session" --arg m "$2" --arg e "$1" \
    '{target_session: $s, source: "agent", source_session: $from, message: $m, in_reply_to: $e}')" "$(listener_url /v1/messages/send)") || code=unanswered
  note "placed on the asker's topic in reply to $1: \"$2\" (listener $code)"
  [ -n "$1" ] && [ "$code" = 200 ]
}
# place_log_line SESSION ROW appends to the current daemon's log one `answers: question` line of
# SESSION carrying the loop's marker, saying that ROW's control placed it and no daemon wrote it:
# what A8's read of the daemon's log would find had the loop taken a question of SESSION.
place_log_line() {
  local line
  line=$(jq -cn --arg s "$1" --arg q "$(control_question "$2")" --arg r "$2" --arg m "$(control_mark "$2")" --arg t "$(date -u +%FT%T.000Z)" \
    '{time: $t, level: "INFO", msg: "answers: question", question: $q, session: $s, state: "posted", marker: $m,
      placed_by: ("scripts/e2e/answer-loop.sh, the control of " + $r + "; no daemon wrote this line")}')
  printf '%s\n' "$line" >>"$daemon_log"
  note "placed in the daemon's log $daemon_log: $line"
}
# place_comment SESSION ISSUE BODY [REPLY_TO] posts BODY on ISSUE in SESSION's own name
# (dispatch_agent, the route a session's own write takes), as a reply under REPLY_TO where one is
# given, and prints its id, nothing where Dispatch took none: what a row reading ISSUE's comments
# would find had the loop posted there in SESSION's name.
place_comment() {
  local dispatch_agent_session=$1
  dispatch_agent POST "issues/$2/comments" "$(jq -cn --arg body "$3" --arg to "${4:-}" '{body: $body} + (if $to == "" then {} else {reply_to: $to} end)')" |
    jq -r '.id // empty'
}
# place_controller_notice COMMENT ROW places on the controller stand-in's topic ROW's control_notice
# of the person's reply COMMENT: a send in the asker's name to the controller stand-in's session,
# whose consumer relays it to the inbox, where a row counts the controller's notices of COMMENT
# (controller_notices). It waits up to 60 s for the notice to reach the inbox, and fails when the
# listener took no send or the notice never got there.
place_controller_notice() {
  local notice code
  notice=$(control_notice "$1" "$2")
  code=$(envoy -o /dev/null -w '%{http_code}' --data "$(jq -cn --arg s "$controller_session" --arg from "$asker_session" --arg m "$notice" \
    '{target_session: $s, source: "agent", source_session: $from, message: $m}')" "$(listener_url /v1/messages/send)") || code=unanswered
  note "placed on the controller stand-in's topic: $notice (listener $code)"
  [ -n "$1" ] && [ "$code" = 200 ] &&
    soft_wait 60 "the placed notice of $1 to reach the controller stand-in's inbox" more_than 0 controller_notices agent-decision "$1"
}
# control_reply_placed ROW EVENT TEXT WHAT READ is where ROW's control places a reply of the loop's
# on the asker's topic: TEXT, which carries ROW's mark (control_mark), in reply to EVENT, the
# control's own question (place_inbox_reply), waited for up to 60 s until the inbox holds it. Then
# it reads the inbox once (inbox_listing), and runs READ EVENT INBOX, the row's read that the loop
# posts nothing in reply to EVENT, over that listing (read_failed, under WHAT), whose failure counts
# only where the listing holds the placed reply (control_seen). A reply the listener took none of is
# not taken.
control_reply_placed() {
  local row=$1 event=$2 mark inbox
  mark=$(control_mark "$row")
  if place_inbox_reply "$event" "$3"; then
    soft_wait 60 "the placed reply to $event to reach the asker's inbox" inbox_holds "$asker_session" "$mark" || true
  else
    control_not_taken "$row" "the reply carrying the loop's mark placed on the asker's topic" || true
  fi
  inbox=$(inbox_listing)
  control_seen "$row" "the inbox, where that read looks for a reply to ${event:-(no event)}" "$inbox" . '(.text // "") | contains(value)' "$mark" || true
  read_failed "$row" "$4" "$5" "$event" "$inbox"
}
# control_ask ROW KIND TARGET ID KEY [IDEMPOTENCY_KEY] asks question ID of the agent at TARGET (KIND
# session or role) under ROW's control suffix, keeps its event as control[<store>:ROW.KEY], and fails
# when the listener named none. The sixth argument is the send's idempotency key, which makes the
# same key the same question (envoy_ask_text): A12's control passes one, the others none.
control_ask() {
  local sent
  sent=$(envoy_ask_text "$2" "$3" "$(question_text "$4") $(control_suffix "$4" "$1")" "${6:-}")
  control[$store:$1.$5]=${sent#* }
  control_sent "$1" "$4 to $2 $3 (listener ${sent%% *})" "${sent#* }"
}
# control_ask_marked ROW KIND TARGET ID MARKER KEY is control_ask with the suffix a row reads by its
# marker, `(question <id> of run <tag>, MARKER)`.
control_ask_marked() {
  local sent
  sent=$(envoy_ask_text "$2" "$3" "$(question_text "$4") $(question_suffix "$4" "$run_tag" "$5")")
  control[$store:$1.$6]=${sent#* }
  control_sent "$1" "$4 to $2 $3 under \"$5\" (listener ${sent%% *})" "${sent#* }"
}
# control_comment ROW KEY ISSUE BODY_JSON posts the proof person's comment BODY_JSON on ISSUE, keeps
# its id as control[<store>:ROW.KEY], and fails when Dispatch took none.
control_comment() {
  local id
  id=$(dispatch_human POST "issues/$3/comments" "$4" | jq -r '.id // empty') || id=
  control[$store:$1.$2]=$id
  control_sent "$1" "the person's comment on $3" "$id"
}

control_send_a1() {
  local ask=${agent[$store:a1.ask]:-}
  control_sent A1 "A1's ask" "$ask" || return 1
  control[$store:A1.panes]=$(pane_count)
  control_comment A1 comment "${agent[$store:root]}" "$(jq -cn --arg ask "$ask" --arg body "$(question_text architect-decision) $(control_suffix architect-decision A1)" '{body: $body, ask_id: $ask}')"
}
# control_read_a1 reads the ask's thread once, where A1's read looks for the architect's reply, and
# counts that read over it only where it holds the control's reply.
control_read_a1() {
  local ask=${agent[$store:a1.ask]} record
  record=$(dispatch_get "asks/$ask" 2>/dev/null) || record=null
  control_seen A1 "ask $ask's thread, where its read looks for the architect's reply" "$record" .replies '.id == value' "${control[$store:A1.comment]}" || true
  read_failed A1 "A1's read" a1_read "$ask" "${control[$store:A1.comment]}" "${control[$store:A1.panes]}" "$record"
}

control_send_a2() {
  control_ask A2 role "${agent[$store:tester.token]}" tester-role role || true
  control_ask A2 session "${agent[$store:tester_old.session]}" tester-old-session old || true
  control_ask A2 session "${agent[$store:pre_change.session]}" pre-change-scan pre || true
  control[$store:A2.code]=$(a2_unregistered_send)
}
control_read_a2() {
  control_inbox_seen A2 "its read looks for the replies to the control's three questions" || true
  read_failed A2 "A2's read" a2_read "${control[$store:A2.role]}" "${control[$store:A2.old]}" "${control[$store:A2.pre]}" "${control[$store:A2.code]}"
}

control_send_a3() {
  local issue=${agent[$store:tester.issue]} session=${agent[$store:tester.session]} token=${agent[$store:a3.token]:-} body
  control_sent A3 "A3's role token" "$token" || return 1
  control_comment A3 comment "$issue" "$(jq -cn --arg body "@tester $(question_text tester-closed-thread) $(control_suffix tester-closed-thread A3)" \
    --arg t "role:$token" '{body: $body, mentions: [{target: $t}]}')" || true
  # The conversation page's message, sent as the page sends it to a stopped agent (Aside).
  body="$(question_text tester-conversation-page) $(control_suffix tester-conversation-page A3)"
  control[$store:A3.body]=$body
  if dispatch_human POST "agents/$session/messages" "$(jq -cn --arg b "$body" '{body: $b, delivery: "aside"}')" >/dev/null; then
    note "A3 control: the person's message to $session as its conversation page sends one (aside)"
  else
    control_not_taken A3 "the person's message to $session" || true
  fi
}
# control_read_a3 reads once the closed issue's comments and the proof person's messages to the
# tester, where A3's read looks for the tester's replies, and counts that read over them only where
# they hold the control's mention and its message.
control_read_a3() {
  local issue=${agent[$store:tester.issue]} session=${agent[$store:tester.session]} comments messages
  comments=$(dispatch_get "issues/$issue/comments" 2>/dev/null) || comments=null
  messages=$(message_listing "$session")
  control_seen A3 "$issue's comments, where its read looks for the tester's reply to the control's mention" "$comments" . '.id == value' "${control[$store:A3.comment]}" || true
  control_seen A3 "the proof person's messages to $session, where its read looks for the tester's reply to the control's message" "$messages" . '.message.body == value' "${control[$store:A3.body]}" || true
  read_failed A3 "A3's read" a3_read "$issue" "${control[$store:A3.comment]}" "$session" "${control[$store:A3.body]}" "$comments" "$messages"
}

# A20's control: the person's reply under the tester's answer, and a second reply under that one,
# which follows a person's own. With answers the thread would hold the one reply more, the tester's
# answer to the first (control[<store>:A20.count], the tester's replies it holds now, and one).
control_send_a20() {
  local issue=${agent[$store:tester.issue]} session=${agent[$store:tester.session]} first=${agent[$store:a20.first]:-} head=${agent[$store:a3.comment]:-}
  control_sent A20 "A20's reply under the tester's answer" "$first" || return 1
  control[$store:A20.skipped]=$(a20_skipped "$session")
  control[$store:A20.panes]=$(pane_count)
  control[$store:A20.count]=$(reply_count "$(thread_replies_by "$head" "$session")")
  control_comment A20 reply "$issue" "$(jq -cn --arg to "$first" --arg body "$(question_text tester-thread-reply) $(control_suffix tester-thread-reply A20)" '{body: $body, reply_to: $to}')" || true
  control_comment A20 control "$issue" "$(jq -cn --arg to "${control[$store:A20.reply]}" \
    --arg body "And how many runs did that rerun pass? (a reply under the person's own reply, control of A20, run $run_tag)" '{body: $body, reply_to: $to}')" || true
}
# control_read_a20 reads the thread once, where A20's read looks for the tester's replies, and runs
# that read over it, counted only where it holds the control's two replies. Then, where its last
# sentence looks, it places a reply in the tester's own name under the control's reply, carrying
# the loop's mark, reads the thread again, and the read that the tester posted nothing there, run
# over that thread, counts only where it holds the placed reply. The mark is placed after the main
# read, which holds that part too.
control_read_a20() {
  local head=${agent[$store:a3.comment]} count=${control[$store:A20.count]:-0} session=${agent[$store:tester.session]} under=${control[$store:A20.control]} record placed
  record=$(comment_record "$head")
  record=${record:-null}
  control_seen A20 "the thread of $head, where its read looks for the tester's replies" "$record" .replies '.id == value' "${control[$store:A20.reply]}" "$under" || true
  read_failed A20 "A20's read" a20_read "$head" "${control[$store:A20.reply]}" "$under" "${control[$store:A20.skipped]}" "${control[$store:A20.panes]}" "$((count + 1))" "$record"
  placed=$(place_comment "$session" "${agent[$store:tester.issue]}" "Placed by the control of A20 in the tester's name, as a reply of the loop's would stand: $(control_mark A20)" "$under") || placed=
  control_sent A20 "the reply carrying the loop's mark placed in $session's name under $under" "$placed" || true
  record=$(comment_record "$head")
  record=${record:-null}
  [ -z "$placed" ] || control_seen A20 "the thread of $head as that read is handed it" "$record" .replies '.id == value' "$placed" || true
  read_failed A20 "with a reply carrying the loop's mark placed in the tester's name under $under, its read that the tester posts nothing there" \
    a20_nothing_posted "$head" "$under" "$(thread_replies_in "$record" "$session")"
}

control_send_a4() { control_ask A4 session "${agent[$store:planner.session]}" unanswerable event || true; }
control_read_a4() {
  control_inbox_seen A4 "its read looks for the planner's reply" || true
  read_failed A4 "A4's read" passed_on_read unanswerable "${control[$store:A4.event]}"
}
control_send_a5() { control_ask A5 session "${agent[$store:planner.session]}" working-copy event || true; }
control_read_a5() {
  control_inbox_seen A5 "its read looks for the planner's reply" || true
  read_failed A5 "A5's read" passed_on_read working-copy "${control[$store:A5.event]}"
}

control_send_a15() {
  control_ask A15 session "${agent[$store:planner.session]}" repository-file planner || true
  control_ask A15 session "${agent[$store:pre_change.session]}" repository-file reviewer || true
}
control_read_a15() {
  control_inbox_seen A15 "its read looks for the two agents' replies" || true
  read_failed A15 "A15's read" a15_read "${control[$store:A15.planner]}" "${control[$store:A15.reviewer]}" "$(control_suffix repository-file A15)"
}

# a6_control_leg NAME ID N replies, as A6's leg N does, on the ask agent NAME opened, with question
# ID under A6's control suffix.
a6_control_leg() {
  local ask
  ask=$(ask_of "${agent[$store:$1.session]}" "${agent[$store:$1.issue]}")
  control[$store:A6.ask$3]=$ask
  control_sent A6 "$1's ask" "$ask" || return 1
  control_comment A6 "comment$3" "${agent[$store:$1.issue]}" "$(jq -cn --arg ask "$ask" --arg body "$(question_text "$2") $(control_suffix "$2" A6)" '{body: $body, ask_id: $ask}')"
}
control_send_a6() {
  a6_control_leg tester a6-tester-decision 1 || true
  [ -z "${agent[$store:a6.leg2]:-}" ] || a6_control_leg a6_child a6-child-decision 2 || true
}
# control_read_a6 reads each leg's ask thread once, where the leg's read looks for the agent's reply,
# and runs that read over it, counted only where it holds the control's reply. Then, where the
# second leg's part that the decision reached the controller not at all looks, it places an
# agent-decision notice of the control's reply there, carrying the loop's mark, reads the inbox
# once, and that part's read, run over it, counts only where the inbox holds the placed notice. The
# notice is placed after the leg's read, which holds that part too.
control_read_a6() {
  local ask1=${control[$store:A6.ask1]} ask2=${control[$store:A6.ask2]:-} comment=${control[$store:A6.comment2]:-} record inbox
  record=$(dispatch_get "asks/$ask1" 2>/dev/null) || record=null
  control_seen A6 "ask $ask1's thread, where its first leg's read looks for the tester's reply" "$record" .replies '.id == value' "${control[$store:A6.comment1]}" || true
  read_failed A6 "A6's first leg" decide_and_ask_read tester a6-tester-decision "the controller" "$ask1" "${control[$store:A6.comment1]}" "$record"
  [ -n "${agent[$store:a6.leg2]:-}" ] || return 0
  record=$(dispatch_get "asks/$ask2" 2>/dev/null) || record=null
  control_seen A6 "ask $ask2's thread, where its second leg's read looks for the child's reply" "$record" .replies '.id == value' "$comment" || true
  read_failed A6 "A6's second leg" decide_and_ask_read a6_child a6-child-decision a6_architect "$ask2" "$comment" "$record"
  place_controller_notice "$comment" A6 ||
    control_not_taken A6 "the notice of $comment carrying the loop's mark placed on the controller stand-in's topic" || true
  inbox=$(inbox_listing)
  control_seen A6 "the inbox, where that part's read looks for the controller's notices" "$inbox" . '(.text // "") | contains(value)' "$(control_mark A6)" || true
  read_failed A6 "with an agent-decision notice of $comment carrying the loop's mark placed on the controller stand-in's topic, A6's second leg's read that the decision reached the controller not at all" \
    a6_nothing_posted "$comment" "$inbox"
}

# A7's control has two parts. (a) With answers removed A7's forwarded leg is replayed: the row's
# hold-then-resume, in the architect's one resume (control_replay_a7), and the read of that leg
# must fail. (b) The running-agent leg's read that the loop posted nothing (a7_loop_posted_nothing)
# is run over row A1's thread, where the loop answered a stopped agent: the architect's reply on
# A1's ask, which the loop posted with its mark, so the read finds the loop's answer there and reads
# as failed. (b) makes no model call.
control_send_a7() {
  control_sent A7 "A1's ask, whose thread the running-agent leg's read is run over" "${agent[$store:a1.ask]:-}" || true
}
# control_read_a7 reads row A1's ask thread once and runs the running-agent leg's read over the
# architect's reply there, counted only where the thread holds A1's own reply.
control_read_a7() {
  local ask=${agent[$store:a1.ask]:-} comment=${agent[$store:a1.comment]:-} record reply
  record=$(dispatch_get "asks/$ask" 2>/dev/null) || record=null
  control_seen A7 "ask $ask's thread, row A1's, where its read looks for the loop's answer" "$record" .replies '.id == value' "$comment" || true
  reply=$(ask_reply_after "$ask" "$comment" "${agent[$store:architect.session]}" "$record" 2>/dev/null) || reply=
  read_failed A7 "A7's running-agent leg's read that the loop posted nothing, run over row A1's thread (the architect's reply on ask $ask after the person's reply $comment, which the loop posted)" \
    a7_loop_posted_nothing "$reply"
}
# control_replay_a7 replays A7's second leg with answers removed, as the first part of the one resume
# of the architect: the proxy's hold on the loop's label placed, control of A7's in-order-2 sent to
# the stopped architect, the claim resumed while the loop's model call would be held (with no
# answers there is none, and the wait for it is the row's own, 240 s), the hold released and, where a
# call was held, the drop and the forward waited for as the row waits, and A7's read of the leg made.
# Then a reply carrying the loop's mark is placed on the asker's topic in reply to the question, and
# A7's read that the loop posts nothing must fail on it (control_reply_placed).
control_replay_a7() {
  local label=daemon-$store token=${agent[$store:architect.token]} session=${agent[$store:architect.session]} n k held=no event appended
  local needle="question in-order-2 of run $run_tag, control of A7"
  appended=$(appended_exchanges architect)
  n=$(proxy_ordinal "$label")
  k=$(held_count "$label")
  hold_at "$label" "$n"
  control_ask A7 session "$session" in-order-2 event || true
  event=${control[$store:A7.event]:-}
  if soft_wait 240 "the loop's model call to be held at the proxy" held_since "$label" "$k"; then held=yes; fi
  claims_cmd resume --claim "$token" >/dev/null || true
  soft_wait 300 "the architect to start" claim_is "$token" '.state | IN("launching", "shim_connected", "registered", "ready", "working", "idle")' || true
  release "$label"
  dropped() { answer_dropped "$session"; }
  delivered() { conversation_holds architect "$needle"; }
  if [ "$held" = yes ]; then
    soft_wait 300 "the answer to be dropped and the question forwarded" dropped || true
    soft_wait 300 "the running architect to receive the forwarded question" delivered || true
  fi
  soft_wait 300 "the architect to run again" claim_is "$token" '.state | IN("ready", "idle")' || true
  soft_wait 120 "the architect's session to be live in the listener" session_live "$session" || true
  control_log_seen A7 "its forwarded leg's read looks for the loop's dropped answer" || true
  read_failed A7 "A7's forwarded leg, replayed (the loop's model call held: $held)" a7_forwarded_read "$event" "$needle" "$appended"
  control_reply_placed A7 "$event" "Placed by the control of A7, a reply to the question as the loop's would stand. $(control_mark A7)" \
    "with a reply carrying the loop's mark placed on the asker's topic in reply to it, A7's read that the loop posts nothing" a7_nothing_posted
}

control_send_a8() {
  local planner=${agent[$store:planner.session]} tester_old=${agent[$store:tester_old.session]} sent code
  control[$store:A8.stored]=$(listener_stored_handoffs_to "$planner")
  sent=$(envoy_ask_text session "$planner" "$(question_text unanswerable) $(control_suffix unanswerable A8)" "a8-control-$run_tag-$store")
  control[$store:A8.sent]=$sent
  control_sent A8 "unanswerable to the planner (listener ${sent%% *})" "${sent#* }" || true
  control[$store:A8.posted]=$(answers_lines "answers: question" "$planner" posted)
  control[$store:A8.own]=$(answers_lines "answers: question" "$tester_old")
  control[$store:A8.handed]=$(listener_handoffs_to "$tester_old")
  control[$store:A8.appended]=$(appended_exchanges tester_old)
  control[$store:A8.refused]=$(controller_questions post_refused)
  code=$(a8_own_send "$(question_text repository-file) $(control_suffix own-answer A8)")
  if [ "$code" = 200 ]; then
    note "A8 control: the planner's question sent as the replaced tester: listener $code"
  else
    control_not_taken A8 "the planner's question sent as the replaced tester (listener $code)" || true
  fi
  control[$store:A8.edit]=$(answers_lines "answers: question" "${agent[$store:architect.session]}")
  if [ -n "${agent[$store:a1.comment]:-}" ]; then
    dispatch_human PATCH "comments/${agent[$store:a1.comment]}" "$(jq -cn --arg b "$(question_text architect-decision) (edited, control of A8)" '{body: $b}')" >/dev/null ||
      control_not_taken A8 "the person's edit of comment ${agent[$store:a1.comment]}" || true
  fi
  # A8's read of the parts that say the loop answers nothing is the daemon's log, so that is where
  # the control places what a loop that answered would have written.
  place_log_line "$tester_old" A8
  place_log_line "${agent[$store:architect.session]}" A8
}
# control_read_a8 counts its first two legs' reads only over an inbox that holds entries, where they
# look for the planner's replies. It reads this boot's daemon log once, where its third leg looks
# for the planner's answer and its last two parts for a question the loop took, and counts those
# reads, the last two run over that listing, only where it holds the line the control placed for
# each session.
control_read_a8() {
  local tester_old=${agent[$store:tester_old.session]} architect=${agent[$store:architect.session]} log
  control_inbox_seen A8 "its first two legs look for the planner's replies" || true
  read_failed A8 "A8's first leg" a8_once_read "${control[$store:A8.stored]}" "${control[$store:A8.sent]}"
  read_failed A8 "A8's second leg" a8_kill_read "${control[$store:A8.sent]#* }"
  log=$(boot_listing)
  control_seen A8 "this boot's daemon log, where its third leg looks for the planner's answer and its last two parts for a question the loop took" "$log" . \
    '.msg == "answers: question" and "\(.session) \(.question)" == value' "$tester_old $(control_question A8)" "$architect $(control_question A8)" || true
  read_failed A8 "A8's third leg's delivery" a8_own_delivered_read "${control[$store:A8.posted]}" "${control[$store:A8.handed]}" "${control[$store:A8.appended]}" \
    "$(newest_question "${agent[$store:planner.session]}" posted)"
  read_failed A8 "with a line carrying the loop's mark placed in the daemon's log for the replaced tester (placed by the control, no daemon wrote it), A8's read that the loop answers nothing of its own answer" \
    a8_own_unanswered_read "${control[$store:A8.own]}" "${control[$store:A8.refused]}" "$(control_question A8)" "$log"
  read_failed A8 "with such a line placed for the architect, A8's read that the loop answers no edit" a8_edit_unanswered_read "${control[$store:A8.edit]}" "$log"
}

# A9's control asks the two questions of the stopped architect; the architect's one resume
# (control_agents) is the row's, its task the row's after-resume question, and the row's read is run
# over the first request after the resume's mark.
control_send_a9() {
  local n
  for n in 1 2; do
    control_ask A9 session "${agent[$store:architect.session]}" "in-order-$n" "e$n" || true
  done
}
control_resumed_a9() {
  control_inbox_seen A9 "its read looks for the replies to the control's two questions" || true
  read_failed A9 "A9's read" a9_read "${control[$store:A9.e1]}" "${control[$store:A9.e2]}" "control of A9" "${control[$store:architect.m]}" "$(question_text after-resume)"
}

# A10's control asks what the row asks: the pre-change reviewer both questions of the Claude Code leg,
# ten seconds apart, its conversation first kept as the row keeps it before its first question
# (sessions/<store>/pre_change-before-control-of-A10-1.jsonl); the architect the question after the
# compaction the row made; and, where the row ran the GPT-6.1 clause, the merger the two cache
# questions and the first of the questions file's a10.reasoning list, its conversation kept before the
# first and before the second (sessions/<store>/gpt-before-control-of-A10-gpt-{1,2}.jsonl), that
# first reasoning question being what makes an answer of the loop owe the read a carried item
# (control[<store>:A10.gpt], `id=event` of each). Each agent is resumed once (control_agents) and
# the row's read of it is run there.
control_send_a10() {
  local dir=$evidence/sessions/$store reasoning id n
  mkdir -p "$dir"
  conversation_text pre_change >"$dir/pre_change-before-control-of-A10-1.jsonl" 2>/dev/null || true
  control_ask_marked A10 session "${agent[$store:pre_change.session]}" pre-change-scan "control of A10-1" e1 || true
  sleep 10
  control_ask_marked A10 session "${agent[$store:pre_change.session]}" pre-change-scan "control of A10-2" e2 || true
  control_ask_marked A10 session "${agent[$store:architect.session]}" in-order-2 "control of A10 after compaction" e3 || true
  [ -n "${agent[$store:a10.gpt]:-}" ] || return 0
  reasoning=$(jq -r '.a10.reasoning[0] // empty' "$questions_file")
  n=0
  control[$store:A10.gpt]=
  for id in gpt-repository-module gpt-repository-go $reasoning; do
    n=$((n + 1))
    [ "$n" != 3 ] || sleep 10
    [ "$n" -gt 2 ] || conversation_text gpt >"$dir/gpt-before-control-of-A10-gpt-$n.jsonl" 2>/dev/null || true
    control_ask_marked A10 session "${agent[$store:gpt.session]}" "$id" "control of A10-gpt-$n" "g$n" || true
    control[$store:A10.gpt]+="${control[$store:A10.gpt]:+ }$id=${control[$store:A10.g$n]}"
    [ "$n" != 1 ] || sleep 10
  done
}
control_resumed_a10() {
  local line
  control_inbox_seen A10 "its $1 leg's read looks for the replies to the control's questions" || true
  case "$1" in
    pre_change)
      line=$(answered_line "$(answer_question_id "$(inbox_reply "${control[$store:A10.e2]}" 2>/dev/null || true)")") || line=
      read_failed A10 "A10's Claude Code leg" a10_claude_read "${control[$store:A10.e1]}" "${control[$store:A10.e2]}" "control of A10-1" "control of A10-2" "$line" \
        "${control[$store:pre_change.m]}" "$evidence/sessions/$store/pre_change-before-control-of-A10-1.jsonl"
      ;;
    architect)
      read_failed A10 "A10's compaction leg" a10_compaction_read "${control[$store:A10.e3]}" "control of A10 after compaction" "${control[$store:architect.m]}"
      ;;
    gpt)
      # shellcheck disable=SC2086 # each id=event is a word of its own
      read_failed A10 "A10's GPT-6.1 clause" a10_gpt_read "control of A10-gpt" "${control[$store:gpt.m]}" ${control[$store:A10.gpt]}
      ;;
  esac
}

# A12's control keeps the one leg a daemon with no answers block can be driven through, the reply
# out of form (malformed), which the proxy's stand-in step makes with no bound set: a daemon that
# still answered would pass that fault on. The four legs a bound of `answers.bounds` makes
# (model_calls, answer_seconds, length, over_limit) and the model_calls leg's answered line can only
# be driven by an answers block, which this daemon does not have, so they are not read. The stand-in
# answers every call of the daemon's label, so the leg is a window of its own, after every other
# read; the row's wait for the fault is its own, control_wait.
control_window_a12() {
  local event
  a12_stand_in malformed
  control_ask A12 session "${agent[$store:planner.session]}" repository-file event "a12-control-$run_tag-$store" || true
  event=${control[$store:A12.event]:-}
  soft_wait "$control_wait" "the malformed fault to be passed on" fault_passed_on malformed || true
  control_inbox_seen A12 "its malformed leg's read looks for the daemon's own reply" || true
  read_failed A12 "A12's malformed leg" a12_leg_read malformed "$event"
  a12_stand_in none
  # Where A12's last sentence looks, a reply out of form carrying the loop's mark, placed after the
  # leg's read, which holds that part too: the read that such a reply is not posted must fail on it.
  control_reply_placed A12 "$event" "not the reply form (placed by the control of A12, a reply out of form as it would stand had the loop posted it) $(control_mark A12)" \
    "with a reply out of form carrying the loop's mark placed in reply to it, A12's read that such a reply is not posted" a12_not_posted_read
}
# a12_not_posted_read EVENT [INBOX] is a12_not_posted as a read of its own, saying what it read.
a12_not_posted_read() {
  read_said="the reply to ${1:-(no event)} on the asker's topic: $(said_text "$(inbox_reply "${1:-}" "${2:-}" 2>/dev/null || true)")"
  a12_not_posted "${1:-}" "${2:-}"
}

control_send_a14() { control_ask A14 session "${agent[$store:planner.session]}" repository-file event || true; }
# control_read_a14 reads this boot's daemon log once and runs A14's read over its cost lines, counted
# only where the log holds entries: a daemon with answers removed writes none of the control's.
control_read_a14() {
  local log
  log=$(boot_listing)
  control_seen A14 "this boot's daemon log, where its read looks for the cost lines" "$log" . || true
  read_failed A14 "A14's read of the cost lines since the control's question" a14_read "$(jq -c '.[]? | select(.msg == "answers: answered" and .fault == "")' <<<"$log")"
}

control_send_a17() {
  local marker=answer-loop-a17-control-$run_tag-$store issue=${agent[$store:implementer.issue]} session=${agent[$store:implementer.session]}
  control[$store:A17.marker]=$marker
  control[$store:A17.impl]="$(comment_request "$marker") (control of A17, run $run_tag, $store)"
  control[$store:A17.leak]="$(comment_request "$leak_marker") If a tool refuses the call, quote what it answered word for word. (control of A17, run $run_tag, $store)"
  control[$store:A17.review]="$(a17_review_body "$marker") (control of A17)"
  person_message "$issue" "$session" "${control[$store:A17.impl]}" || control_not_taken A17 "the comment request to $session" || true
  person_message "$issue" "$session" "${control[$store:A17.leak]}" || control_not_taken A17 "the refused comment's request to $session" || true
  person_message "${agent[$store:pre_change.issue]}" "${agent[$store:pre_change.session]}" "${control[$store:A17.review]}" ||
    control_not_taken A17 "the second owner's write request to ${agent[$store:pre_change.session]}" || true
}
# The pull request's comments are listed once: the first leg reads the comments holding the
# control's marker, the leak-guard leg the listing, and whatever a control that failed made
# on GitHub goes on exit, as A17's own writes do: the comments holding the control's marker,
# and any carrying the run's refusal marker (record_leak_comments). The proof person's messages to
# each agent are listed once too, where each leg looks for the agent's reply. Each leg's read counts
# only where the messages hold the control's own message to that agent, and the first two only
# where GitHub answered a listing of the pull request's comments that holds some. Then, where the
# leak-guard leg's part that the refused comment was never written looks, the control places on
# the same pull request, as the implement App the loop's gh tool writes as
# (lib/smoke-pull-request.ts comment), a comment carrying the run's refusal marker and the loop's
# mark (a17_control_comment), recorded to go on exit too, and waits until GitHub's listing holds it
# (settled_pr_comments); that part's read alone (a17_nothing_written) must then see it, run over
# the listing read once more, and counts only where that listing holds the placed comment. The
# comment is placed after the leg's read, which holds that part too.
control_read_a17() {
  local pr=${agent[$store:implementer.pr]} session=${agent[$store:implementer.session]} listing comments messages id placed
  local reviewer=${agent[$store:pre_change.session]}
  listing=$(pr_comment_listing "$pr")
  comments=$(pr_comments_holding "$listing" "${control[$store:A17.marker]}")
  messages=$(message_listing "$session")
  control_seen A17 "the proof person's messages to $session, where its first two legs look for the implementer's replies" "$messages" . '.message.body == value' \
    "${control[$store:A17.impl]}" "${control[$store:A17.leak]}" || true
  control_seen A17 "#$pr's comments as GitHub listed them, where its first two legs look for the implementer's comments" "$listing" . || true
  read_failed A17 "A17's first leg" a17_impl_read "${control[$store:A17.impl]}" "${control[$store:A17.marker]}" "$comments" "$messages"
  read_failed A17 "A17's leak-guard leg" a17_leak_read "${control[$store:A17.leak]}" "${control[$store:A17.marker]}" "$listing" "$messages"
  messages=$(message_listing "$reviewer")
  control_seen A17 "the proof person's messages to $reviewer, where its reviewer leg looks for the reviewer's reply" "$messages" . '.message.body == value' "${control[$store:A17.review]}" || true
  read_failed A17 "A17's reviewer leg" a17_review_read "${control[$store:A17.review]}" "$messages"
  for id in $(jq -r '.[].id' <<<"$comments"); do github_writes+=("comment:$id"); done
  record_leak_comments "$listing"
  placed=$(timeout 120 bun "$root/scripts/e2e/lib/smoke-pull-request.ts" comment "$work/github-apps.yaml" "$repo" "$pr" "$(a17_control_comment)" |
    jq -r '.id // empty') || placed=
  [ -z "$placed" ] || github_writes+=("comment:$placed")
  if control_sent A17 "the comment carrying $leak_marker and the loop's mark placed as the implement App on #$pr" "$placed"; then
    settled_pr_comments "$pr" "$(control_mark A17)" >/dev/null
  fi
  listing=$(pr_comment_listing "$pr")
  [ -z "$placed" ] || control_seen A17 "#$pr's comments as GitHub listed them for that read" "$listing" . '(.id | tostring) == value' "$placed" || true
  read_failed A17 "with a comment carrying $leak_marker and the loop's mark placed as the implement App on #$pr, A17's leak-guard leg's read that the refused comment was never written" \
    a17_nothing_written "$listing"
}
# a17_control_comment is the body of the comment A17's control places on the implementer's pull
# request: the run's refusal marker, as the comment the leak guard refused would carry had it been
# written, and the loop's mark (control_mark).
a17_control_comment() {
  printf 'Placed by the control of A17 as the implement App, as the comment the leak guard refused would stand had it been written: %s %s\n' \
    "$leak_marker" "$(control_mark A17)"
}
# a17_nothing_written [LISTING] is the part of A17's leak-guard leg that says the refused comment was
# never written: no comment of the implementer's pull request carries the run's refusal marker
# (refused_marker_absent), read alone, apart from the first leg's comment refused_write_absent reads
# beside it. LISTING is the pull request's comments as GitHub listed them (pr_comment_listing), read
# now where not given. A17's control places such a comment as the implement App, carrying the loop's
# mark (a17_control_comment), and this read must see it.
a17_nothing_written() {
  local listing=${1:-}
  [ -n "$listing" ] || listing=$(pr_comment_listing "${agent[$store:implementer.pr]}")
  read_said="comments on #${agent[$store:implementer.pr]} holding $leak_marker: $(jq -r --arg m "$leak_marker" \
    'if type == "array" then [.[] | select((.body // "") | contains($m)) | .id] | tojson else "no listing at all" end' <<<"$listing" 2>/dev/null ||
    printf 'no listing at all')"
  refused_marker_absent "$listing" "$leak_marker"
}

control_send_a18() {
  local marker=answer-loop-a18-control-$run_tag-$store sent
  control[$store:A18.marker]=$marker
  sent=$(envoy_ask_text session "${agent[$store:implementer.session]}" "$(comment_request "$marker") (control of A18, run $run_tag)" "a18-control-$run_tag-$store")
  control[$store:A18.event]=${sent#* }
  control_sent A18 "the comment request to the implementer (listener ${sent%% *})" "${sent#* }" || true
}
# control_read_a18 lists the pull request's comments once, where both legs look for the
# implementer's comment, and runs both legs' reads over the control's request, counted only where
# GitHub answered a listing that holds some and the inbox, where they look for the reply, holds
# entries. Then, where the resume leg's part that the loop posts nothing looks, it places a reply
# carrying the loop's mark on the asker's topic in reply to the request (control_reply_placed). The
# reply is placed after both reads, which hold that part too.
control_read_a18() {
  local pr=${agent[$store:implementer.pr]} event=${control[$store:A18.event]} listing comments id
  listing=$(pr_comment_listing "$pr")
  comments=$(pr_comments_holding "$listing" "${control[$store:A18.marker]}")
  control_seen A18 "#$pr's comments as GitHub listed them, where both legs look for the implementer's comment" "$listing" . || true
  control_inbox_seen A18 "both legs look for the reply to the control's request" || true
  read_failed A18 "A18's restart leg" a18_restart_read "${control[$store:A18.marker]}" "$event" "$comments"
  read_failed A18 "A18's resume leg" a18_resume_read "${control[$store:A18.marker]}" "$event" "$comments"
  for id in $(jq -r '.[].id' <<<"$comments"); do github_writes+=("comment:$id"); done
  control_reply_placed A18 "$event" "Placed by the control of A18, a reply to the question as the loop's would stand. $(control_mark A18)" \
    "with a reply carrying the loop's mark placed on the asker's topic in reply to it, A18's resume leg's read that the loop posts nothing" a18_nothing_posted
}

# A19's read needs the round open in `reviewing` on this boot, which only the approval's delivery
# moves on, and A19 itself moved the issue on. So before the controls' messages the daemon is
# stopped, the round written again as check_a19 writes it (a19_seed_round) and the daemon booted on
# it, on the plain configuration, once GitHub reads the pull request mergeable. The control asks
# the reviewer for an approval, and delivers the one the reviewer posted since, as the row does.
control_prepare_a19() {
  local issue=${agent[$store:a19.issue]:-} number=${agent[$store:a19.number]:-} head=${agent[$store:a19.head]:-} token
  if [ -z "$issue" ] || [ -z "$number" ]; then
    control_not_taken A19 "A19's review round" || true
    return 0
  fi
  token=$(claim_token "$issue" reviewer)
  a19_pr_mergeable "$number" 120 || control_not_taken A19 "GitHub reading $repo#$number as mergeable (it read ${a19_merge_state:-never})" || true
  stop_daemon
  if pg_sql "$daemon_db" "update issues set phase = 'reviewing', held_from = null where key = '$issue'" >/dev/null &&
    pg_sql "$daemon_db" "delete from phases where issue = '$issue'" >/dev/null &&
    pg_sql "$daemon_db" "delete from pull_requests where issue = '$issue'" >/dev/null &&
    a19_seed_round "$issue" "legion-e2eal/${issue,,}" "$number" "$head" "$token"; then
    note "recorded $issue's review round open again on #$number at $head: $(review_record "$issue")"
  else
    control_not_taken A19 "the review round of $issue written again" || true
  fi
  start_daemon "$daemon_bin" "$daemon_config" "$daemon_log" "$daemon_home" "$daemon_profile" "$daemon_plugin_dir"
}
control_send_a19() {
  local number=${agent[$store:a19.number]:-} head=${agent[$store:a19.head]:-} sent
  control_sent A19 "A19's pull request" "$number" || return 1
  control[$store:A19.since]=$(date -u +%FT%TZ)
  sent=$(envoy_ask_text session "${agent[$store:reviewer.session]}" "Please approve your pull request #$number on $repo at its head $head now, with your gh tool: gh pr review $number -R $repo --approve --body 'Approved: nothing to change.' Then tell me what you did. (control of A19, run $run_tag)")
  control_sent A19 "the approval request to the reviewer (listener ${sent%% *})" "${sent#* }" || true
}
# control_read_a19 delivers the reviewer's approval where one was posted since the control asked,
# then lists the pull request's reviews once, where A19's read looks for that approval, and counts
# that read over the listing only where GitHub answered one that holds some: A19's own approval
# stands on that pull request.
control_read_a19() {
  local number=${agent[$store:a19.number]} session=${agent[$store:reviewer.session]} head=${agent[$store:a19.head]} issue=${agent[$store:a19.issue]} since=${control[$store:A19.since]} review delivery reviews
  approved() { approval_by "$(reviewer_approvals "$number" "$session" "$since")" "$review_app_login" "$head"; }
  accepted() { phase_changed "$issue" reviewing accepting; }
  soft_wait 60 "the reviewer's approval of $head on GitHub since $since" approved || true
  review=$(reviewer_approvals "$number" "$session" "$since")
  if approval_by "$review" "$review_app_login" "$head"; then
    delivery=$(deliver_review "$number" "$(approval_id "$review")" control-approval) || true
    note "A19 control: the approval delivered to the listener's /webhook/github as GitHub delivers it: $delivery"
    soft_wait 120 "the daemon to move $issue from reviewing to accepting" accepted || true
  fi
  reviews=$(pr_review_listing "$number")
  control_seen A19 "#$number's reviews as GitHub listed them, where its read looks for the reviewer's approval" "$reviews" . || true
  read_failed A19 "A19's read" a19_read "$number" "$session" "$head" "$issue" "$since" "$reviews"
}

# A21's control sends what the row sends: the person's picker mention of the failed implementer's
# role, the person's mention of the reviewer's role, which no session of the run took (its id is
# control[<store>:A21.control], the row's own control mention), and a message to the failed role's
# topic under a marker of its own. The row's read is run over them. For the first holder's leg it
# mentions the issue's merger role, which no session of the run took either, under a marker of its
# own (control[<store>:A21.holder], control[<store>:A21.holder_marker]), and the first holder
# registers again and claims the leg's own role, the issue's tester, as the leg does
# (control[<store>:A21.claim]): a mention kept for a role the first holder did not claim, whose
# marker the leg's read must then not find in that holder's inbox.
control_send_a21() {
  local issue=${agent[$store:failed_implementer.issue]:-} token=${agent[$store:failed_implementer.token]:-} reviewer
  control_sent A21 "A21's issue" "$issue" || return 1
  reviewer=$(claim_token "$issue" reviewer)
  control[$store:A21.panes]=$(pane_count)
  control_comment A21 comment "$issue" "$(jq -cn --arg body "@implementer $(question_text failed-implementer-role) $(control_suffix failed-implementer-role A21)" \
    --arg t "role:$token" '{body: $body, mentions: [{target: $t}]}')" || true
  control_comment A21 control "$issue" "$(jq -cn --arg body "@reviewer $(question_text failed-implementer-role) (control of A21: a role of this issue no session of the run took, run $run_tag)" \
    --arg t "role:$reviewer" '{body: $body, mentions: [{target: $t}]}')" || true
  control_ask_marked A21 role "$token" failed-implementer-role "control of A21, role topic" role_event || true
  control[$store:A21.holder_marker]=answer-loop-a21-holder-control-$run_tag-$store
  control_comment A21 holder "$issue" "$(a21_holder_body merger "$(claim_token "$issue" merger)" "${control[$store:A21.holder_marker]}")" || true
  control[$store:A21.claim]=$(a21_holder_claim "$(claim_token "$issue" tester)") ||
    control_not_taken A21 "the first holder's registration" || true
}
# control_read_a21 reads the issue's comments once, where A21's read looks for the failed
# implementer's replies to the control's two mentions, and runs that read over them, counted only
# where they hold both mentions and the inbox, where it looks for the reply to the control's message
# to the role's topic, holds entries. It reads the record of the first holder's mention and the
# inbox once, and runs that leg's read over them, counted only where the record is the mention's
# and the inbox holds entries. Then, where the read's part that the implementer posts nothing under
# the control's mention looks, it places a reply in the failed implementer's name under that
# mention, carrying the loop's mark, reads the comments again, and that part's read, run over them,
# counts only where they hold the placed reply. The reply is placed after the main read, which holds
# that part too.
control_read_a21() {
  local issue=${agent[$store:failed_implementer.issue]} session=${agent[$store:failed_implementer.session]} mention=${control[$store:A21.control]} holder=${control[$store:A21.holder]}
  local comments record kept=no inbox placed
  comments=$(dispatch_get "issues/$issue/comments" 2>/dev/null) || comments=null
  control_seen A21 "$issue's comments, where its read looks for the failed implementer's replies to the control's mentions" "$comments" . '.id == value' "${control[$store:A21.comment]}" "$mention" || true
  control_inbox_seen A21 "its read looks for the reply to the control's message to the role's topic" || true
  read_failed A21 "A21's read" a21_read "$issue" "${control[$store:A21.comment]}" "$mention" "${control[$store:A21.panes]}" "${control[$store:A21.role_event]}" "$comments"
  record=$(comment_record "$holder")
  one_delivery_for "$(delivery_records_in "$record")" queued '' && kept=yes
  inbox=$(inbox_listing)
  control_seen A21 "the record of the control's mention $holder, where the first holder's leg reads it kept" "${record:-null}" '[.comment | objects]' '.id == value' "$holder" || true
  control_seen A21 "the inbox, where the first holder's leg looks for the mention's marker" "$inbox" . || true
  read_failed A21 "A21's first holder's leg, over a mention of role:$(claim_token "$issue" merger), a role its first holder $holder_session did not claim (it claimed role:$(claim_token "$issue" tester))" \
    a21_holder_read "$(claim_token "$issue" merger)" "$holder" "${control[$store:A21.holder_marker]}" "${control[$store:A21.claim]}" "$kept" "$inbox"
  placed=$(place_comment "$session" "$issue" "Placed by the control of A21 in the failed implementer's name, as a reply of the loop's would stand: $(control_mark A21)" "$mention") || placed=
  control_sent A21 "the reply carrying the loop's mark placed in $session's name under $mention" "$placed" || true
  comments=$(dispatch_get "issues/$issue/comments" 2>/dev/null) || comments=null
  [ -z "$placed" ] || control_seen A21 "$issue's comments as that read is handed them" "$comments" . '.id == value' "$placed" || true
  read_failed A21 "with a reply carrying the loop's mark placed in the failed implementer's name under $mention, its read that the implementer posts nothing there" \
    a21_nothing_posted "$issue" "$mention" "$comments"
}

control_send_a22() {
  local planner=${agent[$store:planner.session]} issue=${agent[$store:planner.issue]} target=${agent[$store:a22.target]:-} mentioned=${agent[$store:tester_old.session]}
  control_sent A22 "A22's target issue" "$target" || return 1
  control[$store:A22.marker]=answer-loop-a22-control-$run_tag-$store
  control[$store:A22.read]="$(question_text a22-late-comment) $(control_suffix a22-late-comment A22)"
  control[$store:A22.post]="$(a22_post_request "$target" "${control[$store:A22.marker]}" "$mentioned") (control of A22, run $run_tag, $store)"
  control[$store:A22.refuse]="$(a22_refusal_request "$target") (control of A22, run $run_tag, $store)"
  control[$store:A22.skipped]=$(a22_own_skipped "$mentioned")
  control[$store:A22.standing]=$(issue_standing "$target")
  person_message "$issue" "$planner" "${control[$store:A22.read]}" || control_not_taken A22 "the question about the late comment" || true
  person_message "$issue" "$planner" "${control[$store:A22.post]}" || control_not_taken A22 "the comment request" || true
  person_message "$issue" "$planner" "${control[$store:A22.refuse]}" || control_not_taken A22 "the refused writes' request" || true
}
# A22's configuration legs are read of this daemon's own boot line and `legion config check` line,
# computed once for the pass (run_controls), and of the row's own absent lines, which the row kept,
# and count only where this boot's daemon log holds entries. Its three legs' reads look for the
# planner's replies in the proof person's messages to it, read once, and count only where those
# hold the control's three messages; the second leg's looks for the planner's comment in the
# target's comments too, read once, and counts only where those hold some.
# After every leg's read, where the second leg's part that the loop answers nothing of the comment
# looks, the control places on the target the comment the planner's dispatch tool would post, in the
# planner's name with the control's marker and the tool's mark,
# `<!-- legion-answer:<question>.<seq> -->`, and under it a reply in the mentioned session's name
# carrying the loop's mark, and that part's read must see it: it is run over the target's comments
# read once more, and counts only where they hold both the placed comment and the placed reply. The
# placed comment mentions nobody: the read is of its replies alone, and a mention would hand the
# replaced tester's daemon a message, whose hand-offs A8's control counts.
control_read_a22() {
  local target=${agent[$store:a22.target]} planner=${agent[$store:planner.session]} mentioned=${agent[$store:tester_old.session]} marker=${control[$store:A22.marker]}
  local messages all posted placed=
  messages=$(message_listing "$planner")
  control_seen A22 "the proof person's messages to $planner, where its three legs look for the planner's replies" "$messages" . '.message.body == value' \
    "${control[$store:A22.read]}" "${control[$store:A22.post]}" "${control[$store:A22.refuse]}" || true
  all=$(dispatch_get "issues/$target/comments" 2>/dev/null) || all=null
  control_seen A22 "$target's comments, where its second leg looks for the planner's comment" "$all" . || true
  read_failed A22 "A22's first leg" a22_read_leg "${control[$store:A22.read]}" "$messages"
  read_failed A22 "A22's second leg" a22_post_leg "${control[$store:A22.post]}" "$target" "$marker" "$mentioned" "${control[$store:A22.skipped]}" "$messages" "$all"
  read_failed A22 "A22's third leg" a22_refusal_leg "${control[$store:A22.refuse]}" "$target" "${control[$store:A22.standing]}" "$messages"
  control_log_seen A22 "its configuration legs look for this boot's tools line" || true
  read_failed A22 "A22's configuration legs, read of this daemon's own boot and of its configuration, and of the row's own daemon with no dispatch command" \
    a22_tools_leg "${control[$store:tools]}" "${control[$store:check]}" "${agent[$store:a22.absent_tools]:-}" "${agent[$store:a22.absent_check]:-}"
  posted=$(place_comment "$planner" "$target" "$marker"$'\n\n'"$(control_tool_mark A22)") || posted=
  control_sent A22 "the comment carrying $marker and the dispatch tool's mark placed in $planner's name on $target" "$posted" || true
  [ -z "$posted" ] || placed=$(place_comment "$mentioned" "$target" "Placed by the control of A22 in the mentioned session's name, as the loop's answer to the comment would stand: $(control_mark A22)" "$posted") || placed=
  control_sent A22 "the reply carrying the loop's mark placed in $mentioned's name under ${posted:-(no comment)}" "$placed" || true
  all=$(dispatch_get "issues/$target/comments" 2>/dev/null) || all=null
  [ -z "$placed" ] || control_seen A22 "$target's comments as that read is handed them" "$all" . '.id == value' "$posted" "$placed" || true
  read_failed A22 "with the dispatch tool's comment placed in the planner's name on $target and a reply carrying the loop's mark placed in $mentioned's name under it, A22's second leg's read that the loop answers nothing of the comment" \
    a22_nothing_posted "$target" "$marker" "$mentioned" "$all"
}

# A23's control asks the architect (and the merger where the row ran its GPT-6.1 leg) what the row
# asks; each is resumed once (control_agents) and the row's search read is run there. Its fourth
# search is a window of its own (control_window_a23).
control_send_a23() {
  control_ask_marked A23 session "${agent[$store:architect.session]}" a23-web-search "control of A23" event || true
  if [ -n "${agent[$store:a23.gpt]:-}" ]; then
    control_ask_marked A23 session "${agent[$store:gpt.session]}" a23-web-search-gpt "control of A23-gpt" gpt || true
  fi
}
control_read_a23() {
  control_log_seen A23 "its configuration legs look for this boot's tools line" || true
  read_failed A23 "A23's configuration legs, read of this daemon's own boot and of its configuration, and of the row's own daemon with no answers.web_search" \
    a23_tools_leg "${control[$store:tools]}" "${control[$store:check]}" "${agent[$store:a23.off_tools]:-}" "${agent[$store:a23.off_check]:-}"
}
control_resumed_a23() {
  control_inbox_seen A23 "its $1 search leg's read looks for the reply to the control's question" || true
  case "$1" in
    architect) read_failed A23 "A23's search leg" a23_search_read "${control[$store:A23.event]}" "${control[$store:architect.m]}" agents architect ;;
    gpt) read_failed A23 "A23's GPT-6.1 search leg" a23_search_read "${control[$store:A23.gpt]}" "${control[$store:gpt.m]}" agents-gpt gpt ;;
  esac
}
# control_window_a23 is A23's fourth search with answers removed: under the stand-in step that calls
# web_search four times, as the row runs it, in a window of its own after every other read, since
# the stand-in answers every call of the daemon's label. The daemon has no answers block to hold the
# row's `model_calls: 2`; the answer is waited for as it is posted, up to control_wait.
control_window_a23() {
  a12_stand_in four_searches
  control_ask A23 session "${agent[$store:planner.session]}" a23-fourth-search fourth || true
  soft_wait "$control_wait" "the reply to the fourth search's question" inbox_reply "${control[$store:A23.fourth]:-}" || true
  control_inbox_seen A23 "its fourth search's read looks for the reply to the control's question" || true
  read_failed A23 "A23's fourth search" a23_fourth_read "${control[$store:A23.fourth]:-}"
  a12_stand_in none
}

# control_ran ROW: ROW ran in this pass (rows_ran) and every message of its control was sent.
control_ran() { [[ " ${rows_ran[$store]:-} " == *" $1 "* ]] && [ -z "${control[$store:$1.unsent]:-}" ]; }
# control_turn NAME LABEL TASK: NAME's task is delivered once its claim holds none, its first model
# call after the mark control[<store>:NAME.m] on LABEL waited for, and its turn's end, as the rows wait.
control_turn() {
  local token=${agent[$store:$1.token]}
  deliver_task "$token" "$3" || true
  soft_wait 300 "the resumed $1's first model call" more_than "${control[$store:$1.m]}" proxy_count "$2" || true
  soft_wait 600 "the resumed $1's turn" claim_is "$token" '.state == "idle" and .pending == null' || true
}
# control_park NAME suspends NAME's claim and waits for it; control_agents waits for each session to
# leave the listener's registry once for all of them, since the lapses run side by side.
control_park() {
  claims_cmd suspend --claim "${agent[$store:$1.token]}" >/dev/null || true
  soft_wait 300 "$1 to be suspended" claim_is "${agent[$store:$1.token]}" '.state == "suspended"' || true
}
# control_agents runs what needs a stopped agent running, after every read that counts claims or
# panes: the architect's one resume (A7's replay, A9's read, A10's compaction leg, A23's search leg),
# the pre-change reviewer's (A10's Claude Code leg) and the merger's (A10's GPT-6.1 clause and A23's
# GPT-6.1 search leg), each with the task its row resumes it with. Each agent is suspended again, as
# A13's own probes need it stopped.
control_agents() {
  local task name token
  local -a resumed=()
  if control_ran A7 || control_ran A9 || control_ran A10 || control_ran A23; then
    task="Reply with one word: ready."
    ! control_ran A9 || task=$(question_text after-resume)
    control[$store:architect.m]=$(proxy_count agents)
    if control_ran A7; then control_replay_a7; else resume_agent architect || true; fi
    control_turn architect agents "$task"
    if control_ran A9; then control_resumed_a9; fi
    if control_ran A10; then control_resumed_a10 architect; fi
    if control_ran A23; then control_resumed_a23 architect; fi
    control_park architect
    resumed+=(architect)
  fi
  if control_ran A10; then
    control[$store:pre_change.m]=$(proxy_count "claude-$store")
    resume_agent pre_change || true
    control_turn pre_change "claude-$store" "While you were stopped you were asked what blocked the release. How many times were you asked, and what did you answer?"
    control_resumed_a10 pre_change
    control_park pre_change
    resumed+=(pre_change)
  fi
  if { control_ran A10 && [ -n "${control[$store:A10.gpt]:-}" ]; } || { control_ran A23 && [ -n "${control[$store:A23.gpt]:-}" ]; }; then
    task="Reply with one word: ready."
    # shellcheck disable=SC2086 # each id=event is a word of its own
    ! control_ran A10 || [ -z "${control[$store:A10.gpt]:-}" ] || task=$(set -- ${control[$store:A10.gpt]} && printf 'While you were stopped you were asked %s questions about go.mod. What did you answer each time?' "$#")
    control[$store:gpt.m]=$(proxy_count agents-gpt)
    resume_agent gpt || true
    control_turn gpt agents-gpt "$task"
    if control_ran A10 && [ -n "${control[$store:A10.gpt]:-}" ]; then control_resumed_a10 gpt; fi
    if control_ran A23 && [ -n "${control[$store:A23.gpt]:-}" ]; then control_resumed_a23 gpt; fi
    control_park gpt
    resumed+=(gpt)
  fi
  for name in ${resumed[@]+"${resumed[@]}"}; do
    soft_wait "$session_ttl_wait" "$name's session to leave the listener's registry" session_gone "${agent[$store:$name.session]}" || true
  done
}

# run_controls runs the control row of every answering row that ran in the current store's pass
# (rows_ran), against the daemon running now, the branch's with `answers` removed. A row's control
# may have a preparation (control_prepare_<row>), run first, its messages (control_send_<row>), sent
# before any wait, then one wait of control_wait, then its reads (control_read_<row>) over what
# follows its messages, then the reads that need a stopped agent running (control_agents) and the
# windows that need a stand-in step or a hold of their own (control_window_<row>). Each is recorded
# as `A<n> control` (control_result).
run_controls() {
  local row rows=${rows_ran[$store]:-}
  printf "== controls (%s): each answering row's own read over a message of its own, with answers removed\n" "$store"
  if [ -z "$rows" ]; then
    note "no answering row ran under $store"
    return 0
  fi
  register_asker || note "the listener refused a stand-in's registration; the controls follow anyway"
  for row in $rows; do
    control[$store:$row.why]=
    control[$store:$row.unsent]=
    control[$store:$row.unseen]=
    control[$store:$row.said]=
    control[$store:$row.held]=
  done
  for row in $rows; do
    if declare -F "control_prepare_${row,,}" >/dev/null; then "control_prepare_${row,,}" || true; fi
  done
  # This daemon's own boot line and `legion config check` line, read once for A22's and A23's
  # configuration legs.
  if [[ " $rows " == *" A22 "* || " $rows " == *" A23 "* ]]; then
    control[$store:tools]=$(boot_tools_line)
    control[$store:check]=$(config_check_tools "$daemon_config")
  fi
  for row in $rows; do
    if declare -F "control_send_${row,,}" >/dev/null; then "control_send_${row,,}" || true; fi
  done
  note "every control's message is sent; one wait of ${control_wait}s, the longest a row's own read waits but for the GPT-6.1 merger's ${gpt_answer_wait}s under the rig's own ${gpt_answer_seconds}-second bound, which no daemon with answers removed holds, before any is read"
  sleep "$control_wait"
  for row in $rows; do
    if declare -F "control_read_${row,,}" >/dev/null && [ -z "${control[$store:$row.unsent]}" ]; then "control_read_${row,,}" || true; fi
  done
  control_agents
  for row in $rows; do
    if declare -F "control_window_${row,,}" >/dev/null && [ -z "${control[$store:$row.unsent]}" ]; then "control_window_${row,,}" || true; fi
  done
  for row in $rows; do
    control_result "$row"
  done
}
# control_result ROW records ROW's control row, `A<n> control`: FAIL when a message of it was not
# taken; when a listing a read of it was handed did not show what the control sent or placed there
# (control_seen), so that read's failure shows nothing; when none of the row's reads ran; and when a
# part of the row's read held. PASS when every part of the row's read failed over what the control
# sent or placed.
control_result() {
  local row=$1
  if [ -n "${control[$store:$row.unsent]:-}" ]; then
    record_result "$row control" FAIL "with answers removed, the control's message was not all sent, so its read shows nothing: ${control[$store:$row.why]:-see the notes}"
  elif [ -n "${control[$store:$row.unseen]:-}" ]; then
    record_result "$row control" FAIL "with answers removed, a listing its read was handed shows nothing of what the control sent or placed there, so that read's failure shows nothing: ${control[$store:$row.why]}; ${control[$store:$row.said]:-no read ran}"
  elif [ -z "${control[$store:$row.said]:-}" ]; then
    record_result "$row control" FAIL "with answers removed, none of the row's reads ran"
  elif [ -n "${control[$store:$row.held]:-}" ]; then
    record_result "$row control" FAIL "with answers removed, ${control[$store:$row.said]}"
  else
    record_result "$row control" PASS "with answers removed, ${control[$store:$row.said]}"
  fi
}

# ---- A13: refused exactly as today without answers --------------------------------------------------
# a13_probe LABEL STORE: against whatever listener, Dispatch and daemon run now, the four answers a
# stopped agent's message gets today, saved under $evidence/a13/<LABEL>/<STORE>/: an envoy_send to
# its session, a Dispatch message to its session and to its role (the delivery records, their ids
# and times left out), and a publish to its role topic, last because it is the one call that can
# change what the bus holds for the role. Both stacks probe the same snapshot of the bus, so each
# answer is the other's but for what a fresh event mints (a13_same).
a13_probe() {
  local label=$1 probe_store=$2 dir s r issue
  dir=$evidence/a13/$label/$probe_store
  mkdir -p "$dir"
  s=${agent[$probe_store:architect.session]}
  r=${agent[$probe_store:architect.token]}
  envoy -o "$dir/send.body" -w '%{http_code}\n' --data "$(jq -cn --arg s "$s" '{target_session: $s, source: "agent", source_session: "legion-e2eal-a13", message: "A13 probe: what did you decide?"}')" \
    "$(listener_url /v1/messages/send)" >"$dir/send.status"
  issue=$(dispatch_human POST issues "$(jq -cn --arg p "$a13_project" '{project: $p, title: "A13 probe", force: true}')" | jq -er .key)
  dispatch_human POST "issues/$issue/messages" "$(jq -cn --arg t "session:$s" '{body: "A13 probe: what did you decide?", target: $t, delivery: "steer"}')" |
    jq -S '.deliveries | map(del(.message_id, .created_at, .requested_by.id, .requested_by.name, .requested_by.login))' >"$dir/dispatch-session.json"
  dispatch_human POST "issues/$issue/messages" "$(jq -cn --arg t "role:$r" '{body: "A13 probe: what did you decide?", target: $t, delivery: "steer"}')" |
    jq -S '.deliveries | map(del(.message_id, .created_at, .requested_by.id, .requested_by.name, .requested_by.login))' >"$dir/dispatch-role.json"
  envoy -o "$dir/publish.body" -w '%{http_code}\n' --data "$(jq -cn --arg t "notifications.role.$r" '{topic: $t, source: "agent", source_session: "legion-e2eal-a13", message: "A13 probe: what did you decide?"}')" \
    "$(listener_url /v1/messages/publish)" >"$dir/publish.status"
  note "$label $probe_store: send $(cat "$dir/send.status") $(cat "$dir/send.body"); publish $(cat "$dir/publish.status") $(cat "$dir/publish.body")"
  note "$label $probe_store: Dispatch to session $(jq -c 'map({state, error, session_id})' "$dir/dispatch-session.json"); to role $(jq -c 'map({state, error, session_id})' "$dir/dispatch-role.json")"
}

restart_nats_from() {
  stop_service nats
  rm -rf "$work/nats-store"
  cp -a "$1" "$work/nats-store"
  start_nats
}

# ---- the matrix ------------------------------------------------------------------------------------
# The run's one file of every answer of every pass, one line each (read_pass_answers).
answer_calls_header >"$answer_calls_tsv"
wanted A16 && check_a16

for store in $stores; do
  begin "store-$store"
  project="AL${store:0:1}$run_tag"
  project=${project^^}
  daemon_db=legion_${store}_$run_tag
  state=$work/state-$store
  mkdir -p "$state"
  dispatch_human POST projects "$(jq -cn --arg key "$project" --arg name "Answer loop proof $project" '{key: $key, name: $name}')" >/dev/null
  map_repo "$project"
  agent[$store:root]=$(new_rig_issue "Answer loop proof tree ($store)")
  agent[$store:planner.issue]=$(new_rig_issue "Answer loop proof child: the plan" "${agent[$store:root]}")
  agent[$store:tester.issue]=$(new_rig_issue "Answer loop proof child: the tests" "${agent[$store:root]}")
  agent[$store:pre_change.tree]=$(new_rig_issue "Answer loop proof tree before the change ($store)")
  agent[$store:pre_change.issue]=$(new_rig_issue "Answer loop proof child: before the change" "${agent[$store:pre_change.tree]}")
  # A10's GPT-6.1 merger, a child of the tree, only where the model route serves the model.
  [ -n "$gpt_refusal" ] ||
    agent[$store:gpt.issue]=$(new_rig_issue "Answer loop proof child: the GPT-6.1 merger" "${agent[$store:root]}")
  note "project $project, issues: root ${agent[$store:root]}, planner ${agent[$store:planner.issue]}, tester ${agent[$store:tester.issue]}, pre-change ${agent[$store:pre_change.issue]} (tree ${agent[$store:pre_change.tree]})${agent[$store:gpt.issue]:+, GPT-6.1 merger ${agent[$store:gpt.issue]}}"
  produce_pre_change
  write_legion_config "$work/answers-$store.yaml" yes
  write_legion_config "$work/plain-$store.yaml" no
  # The pass's answers are those its daemon log first names from here on (read_pass_answers), under
  # criterion's rule whatever row was opened last.
  answers_taken=
  answers_named=
  answers_pass_from=$(log_lines "$evidence/logs/daemon-$store.log") || answers_pass_from=0
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$evidence/logs/daemon-$store.log" "$branch_home" "$branch_profile" \
    "$branch_plugin"
  produce_agents
  save_conversations
  # The registry's 404 is retried each minute; a boot that has run past a minute shows it logged once.
  sleep 65
  wanted boot && check_boot
  [ "$store" != postgres ] || ! wanted store || check_postgres_store
  wanted A1 && check_a1
  wanted A2 && check_a2
  wanted A3 && check_a3
  # A20 right after A3, whose thread it replies in, under the tester's answer there.
  wanted A20 && check_a20
  wanted A4 && check_passed_on unanswerable A4
  wanted A5 && check_passed_on working-copy A5
  # A15 before every other check that asks the planner to read go.mod: A8's own-answer leg and
  # A12's five legs ask that same question (`repository-file`), and once the planner's conversation
  # holds the file the loop is right to answer from it with no tool call at all, which is no proof
  # of a `gh` read. Under postgres in the run of 2026-11-08 that is what happened (question
  # 405247a2bac791f9503bd9b5dbf175f8, `calls: 1`, no `answers: gh` line, the reply itself saying it
  # had read the file earlier in the conversation), while the same check passed under files, where
  # the model chose to read again. Asked first, the question is one neither conversation can answer
  # without the tool.
  wanted A15 && check_a15
  wanted A6 && check_a6
  wanted A7 && check_a7
  wanted A8 && check_a8
  wanted A9 && check_a9
  wanted A10 && check_a10
  wanted A12 && check_a12
  # A22 and A23 before A14, so A14 recomputes the cost lines of answers that ran the dispatch and
  # web_search tools, A23's web search fees among them. Both restart the daemon, as A12 does.
  wanted A22 && check_a22
  wanted A23 && check_a23
  wanted A14 && check_a14
  wanted A17 && check_a17
  wanted A18 && check_a18
  wanted A19 && check_a19
  # A21 last: it seeds a tree of its own in implementing and fails its implementer, with a daemon
  # restart between, so no other check of the pass runs after it on the record it leaves.
  wanted A21 && check_a21
  stop_daemon
  read_pass_answers "$evidence/logs/daemon-$store.log"
  pass
done

# A13: the same stopped agents, the same bus state, the branch with `answers` removed against the
# base commit's binaries. In the branch's phase, after A13's own probes of each store, the control
# rows of that store's answering rows run against the same daemon (run_controls); they read the
# asker's inbox, so the inbox is started again on the restarted NATS, whose port is a new one. Then
# A13's own control: the same probe against the branch's daemon with `answers` on, whose
# comparison with the base commit must fail. The loop answers that probe's messages, so the
# control's row reads every answer its daemon gave as an answering row reads its own, asking for no
# write.
# Both stacks are probed at one point of the role claim reaper, so the comparison reads the
# branch's change and not the reaper's timing: both listeners run it every reap_period seconds
# (scripts/e2e/envoy-session-states.sh's 20 s, not the deployment's five minutes) over a live
# window of live_window seconds, the listener's own default set here so the rig owns the grace it
# waits for, and each probe, the branch's, its control's and the base's, runs only once its own
# listener has been open past that grace and its reaper has passed after it (await_reaper_pass).
# Without that, proof run 45542053007 compared under postgres a branch listener some 25 minutes
# old, whose reaper had released a gone holder's role claim, with a base listener probed seconds
# after its start, which still kept that claim as one it restored inside its grace. The first probe
# of each listener waits out the rest of its window; every later one finds the pass logged already.
run_a13() {
  local differs probe_store branch control_differs live_window=300 reap_period=20
  local -A control_from=()
  local reaper_env=("ENVOY_ROLE_CLAIM_REAP_INTERVAL=${reap_period}s" "ENVOY_SESSION_LIVE_WINDOW=${live_window}s")
  begin a13
  store=-
  a13_project=A13$run_tag
  stop_service dispatch
  stop_service listener
  stop_service nats
  cp -a "$work/nats-store" "$work/nats-snapshot"
  start_nats
  stop_service inbox
  start_inbox
  start_listener "$work/branch" "${reaper_env[@]}"
  start_dispatch "$work/branch" dispatch
  dispatch_human POST projects "$(jq -cn --arg key "$a13_project" '{key: $key, name: "A13 probes"}')" >/dev/null
  declare -gA a13_quiet=()
  for store in $stores; do
    project="AL${store:0:1}$run_tag"
    project=${project^^}
    daemon_db=legion_${store}_$run_tag
    state=$work/state-$store
    # The pass's project kept the smoke repository as its default since the pass set it
    # (map_repo), and the daemon reads each rig issue's repository from Dispatch, which resolves it
    # to that default. Each file is written again for the NATS port this phase serves.
    write_legion_config "$work/plain-$store.yaml" no
    write_legion_config "$work/answers-$store.yaml" yes
    start_daemon "$work/branch/legion" "$work/plain-$store.yaml" "$evidence/logs/daemon-$store.log" "$branch_home" "$branch_profile" \
      "$branch_plugin"
    sleep 65
    a13_quiet[$store]=0
    check_boot_without_answers || a13_quiet[$store]=1
    await_reaper_pass "$live_window" "$reap_period"
    a13_probe branch "$store"
    run_controls
    stop_daemon
    # The daemon started next answers A13's control, and its row reads every answer this log names
    # past the lines it holds now (answers_begin, below).
    control_from[$store]=$(log_lines "$evidence/logs/daemon-$store.log") || control_from[$store]=
    start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$evidence/logs/daemon-$store.log" "$branch_home" "$branch_profile" \
      "$branch_plugin"
    sleep 65
    await_reaper_pass "$live_window" "$reap_period"
    a13_probe control "$store"
    stop_daemon
  done
  stop_service dispatch
  stop_service listener
  restart_nats_from "$work/nats-snapshot"
  start_listener "$work/base" "${reaper_env[@]}"
  start_dispatch "$work/base" dispatch_base
  dispatch_human POST projects "$(jq -cn --arg key "$a13_project" '{key: $key, name: "A13 probes"}')" >/dev/null
  store=files
  project=A13B$run_tag
  daemon_db=legion_base_$run_tag
  state=$work/state-base
  mkdir -p "$state"
  # The base stack's Dispatch is a database of its own, so the base daemon's project is made there
  # and takes the smoke repository as its default, as each pass's does in the branch stack's
  # Dispatch (map_repo).
  dispatch_human POST projects "$(jq -cn --arg key "$project" '{key: $key, name: "A13 base daemon"}')" >/dev/null
  map_repo "$project"
  write_legion_config "$work/base-a13.yaml" base
  start_daemon "$work/base/legion" "$work/base-a13.yaml" "$evidence/logs/base-daemon-a13.log" "$base_home" "$base_profile" \
    "$base_plugin"
  for probe_store in $stores; do
    await_reaper_pass "$live_window" "$reap_period"
    a13_probe base "$probe_store"
  done
  stop_daemon
  for store in $stores; do
    differs=$(a13_differs)
    printf '== A13 (%s): with answers removed, a stopped agent is refused exactly as the base commit refuses it\n' "$store"
    note "branch vs base ($base_commit): $(cd "$evidence/a13" && for f in send.body publish.body; do printf '%s %s ' "$f" "$(sha256sum "branch/$store/$f" | cut -c1-12)/$(sha256sum "base/$store/$f" | cut -c1-12)"; done)"
    # Beside the comparison, what both commits answer today: the send is stored for the stopped
    # session (200, state stopped) and Dispatch's delivery to that session is queued for it, while
    # Dispatch's delivery to the role fails and the role publish is kept for the role's next holder.
    # None of those is a hand-off: a send handed to a stopped agent's daemon carries no state, and
    # a role delivery handed to it is queued for that agent's session.
    branch=$evidence/a13/branch/$store
    if [ -z "$differs" ] && [ "${a13_quiet[$store]}" = 0 ] && grep -q '^200$' "$branch/send.status" &&
      stored_for_stopped "$(cat "$branch/send.body")" "${agent[$store:architect.session]}" &&
      one_delivery_in "$(cat "$branch/dispatch-session.json")" queued &&
      one_delivery_in "$(cat "$branch/dispatch-role.json")" failed; then
      record_result A13 PASS "envoy_send stored for the stopped session (200, state stopped), Dispatch's deliveries (to the session queued, to the role failed) and the role publish ($(cat "$branch/publish.status")) say what the base commit says; the daemon wrote nothing to the registry and ran no loop"
    else
      record_result A13 FAIL "differs: ${differs:-nothing}; registry or loop lines with answers removed: ${a13_quiet[$store]}; the branch's send $(cat "$branch/send.status") $(jq -c '{recipient, state}' "$branch/send.body" 2>/dev/null), Dispatch to the session $(jq -c 'map({state, error})' "$branch/dispatch-session.json" 2>/dev/null), to the role $(jq -c 'map({state, error})' "$branch/dispatch-role.json" 2>/dev/null)"
    fi
    # A13's control: the same probe of the branch's daemon with `answers` on. Its comparison with
    # the base commit must fail, a missing capture being no difference (a13_captured).
    printf '== A13 control (%s): with answers on, the branch answers a stopped agent unlike the base commit\n' "$store"
    control_differs=$(a13_differs control)
    branch=$evidence/a13/control/$store
    # The row reads every answer the control's daemon gave as an answering row reads its own, asking
    # for no write: each question the store's log names past the lines it held before that daemon
    # started (control_from), counted over those lines alone, a question the pass left generating
    # and that daemon answered again at its boot among them (answers_begin's `every`). That daemon
    # is the last that wrote the log, since the base daemon writes base-daemon-a13.log, so those
    # lines are its alone. Once the row's result is recorded, the pass's read takes them again from
    # there under the same rule, each a line of answer_calls_tsv under the row's name.
    answers_taken=
    answers_begin "A13 control" "$evidence/logs/daemon-$store.log" "${control_from[$store]}" every
    if ! a13_captured control; then
      record_result "A13 control" FAIL "the probe of the branch with answers on, or the base commit's, captured nothing to compare under $evidence/a13"
    elif [ -n "$control_differs" ]; then
      record_result "A13 control" PASS "with answers on, A13's comparison with the base commit failed: ${control_differs% } differ; the branch's send $(cat "$branch/send.status") $(jq -c '{recipient, state}' "$branch/send.body" 2>/dev/null), Dispatch to the session $(jq -c 'map({state, error})' "$branch/dispatch-session.json" 2>/dev/null), to the role $(jq -c 'map({state, error})' "$branch/dispatch-role.json" 2>/dev/null)"
    else
      record_result "A13 control" FAIL "with answers on, A13's comparison with the base commit held: every capture said what the base commit says"
    fi
    answers_pass_from=${control_from[$store]}
    read_pass_answers "$evidence/logs/daemon-$store.log"
  done
  pass
}
wanted A13 && run_a13

# ---- summary ---------------------------------------------------------------------------------------
begin summary
printf '%-10s %-8s %-8s %s\n' STORE CRITERION VERDICT DETAIL
for row in "${results[@]}"; do
  IFS='|' read -r r_store r_criterion r_verdict r_detail <<<"$row"
  printf '%-10s %-8s %-8s %s\n' "$r_store" "$r_criterion" "$r_verdict" "$r_detail"
done
printf '%s\n' "${results[@]}" >"$evidence/summary.txt"
# Each store's answers, as answer-calls.tsv holds them: a measurement, which no row is judged by.
answer_totals_said=$(answer_totals "$answer_calls_tsv") || answer_totals_said="$answer_calls_tsv could not be read"
while IFS= read -r r_totals; do note "answers: $r_totals"; done <<<"${answer_totals_said:-no pass logged an answer}"
failed=$(printf '%s\n' "${results[@]}" | awk -F'|' '$3 == "FAIL"' | wc -l)
waiting=$(printf '%s\n' "${results[@]}" | awk -F'|' '$3 == "WAITING" { print $2 }' | sort -u | tr '\n' ' ')
passed=$(printf '%s\n' "${results[@]}" | awk -F'|' '$3 == "PASS" { print $1 ":" $2 }' | tr '\n' ' ')
note "PASS: ${passed:-none}"
note "WAITING: ${waiting:-none}"
note "FAIL: $failed"
[ "$failed" = 0 ] || fail "$failed criterion checks failed"
pass
ok=1
echo "answer loop e2e: every runnable check passed; the rest are WAITING as listed"
