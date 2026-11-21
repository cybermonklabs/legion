#!/usr/bin/env bash
# The answer loop's live proof (INFRA-204; scripts/e2e/README.md, "answer-loop.sh"): the branch's
# daemon, Envoy listener and Dispatch, run on this host with real Anthropic models (and OpenAI's
# GPT-6.1 for A10's GPT-6.1 clause, where the model route serves it), under both of a tmux daemon's
# conversation stores (runtime.tmux.session_store files, then postgres), with stopped agents whose
# real conversations hold the decisions scripts/e2e/answer-loop/questions.json names.
# Every acceptance criterion of the plan's table (A1-A10, A12-A19) is one check that prints its
# evidence and a negative control; the summary lists each as PASS, FAIL or WAITING with what it
# waits on. It runs in a Legion issue pod (no docker, no root, no psql, no tmux) and on a devbox.
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

# record_result CRITERION VERDICT DETAIL: one summary row, for the current store ("-" for a
# criterion that does not run per store). VERDICT is PASS, FAIL or WAITING; DETAIL says why.
record_result() {
  results+=("${store:--}|$1|$2|$3")
  printf '%s %s (%s): %s\n' "$2" "$1" "${store:--}" "$3"
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
# after it. The inbox is not: its one connection names the bring-up's port, and no check reads the
# inbox once A13 has restarted NATS.
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
start_inbox() {
  local offset
  offset=$(log_size inbox)
  start_process inbox "${service_env[@]}" "HOME=$work" bun "$root/scripts/e2e/lib/envoy-inbox.ts" \
    --nats "nats://127.0.0.1:$port_nats" --record "$evidence/inbox.jsonl" \
    --watch "$asker_session=notifications.envoy.deliver.$asker_session" \
    --watch "$controller_session=notifications.envoy.deliver.$controller_session"
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
start_listener() {
  local bin=$1
  [ -n "$listener_keeper_pid" ] || keep_port listener_keeper port_listener
  start_kept listener listener_keeper ENVOY_LISTEN_FD -- "${service_env[@]}" "HOME=$work/listener-home" \
    "ENVOY_API_TOKEN=$(cat "$work/envoy-token")" ENVOY_LISTEN_FD=3 "PORT={port}" ENVOY_LISTEN_HOST=127.0.0.1 \
    "ENVOY_MACHINE_ID=legion-e2eal-$$" ENVOY_WEBHOOKS=github "ENVOY_GITHUB_WEBHOOK_SECRET=$(cat "$work/github-webhook-secret")" \
    "ENVOY_REVIEWER_APP_ID=${review_app_id:-$default_review_app_id}" "${dispatch_identity_listener_env[@]}" \
    "NATS_URLS=nats://127.0.0.1:$port_nats" "$bin/envoy-listener"
  await_start listener "$listener_pid" 60 "the Envoy listener to answer /v1/sessions" \
    curl -fsS --max-time 5 -H "@$work/envoy-auth-header" "http://127.0.0.1:$port_listener/v1/sessions"
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
# write_legion_config FILE ANSWERS [BOUNDS] writes the daemon's legion.yaml for the current store:
# claims-only (no dispatch_url, no github_apps) unless both role Apps were given, the agents'
# reviewer on Claude Code, the merger's overlay (role_config_files, the GPT-6.1 agent's model and
# thinking level) where the model route serves GPT-6.1, the store's runtime, and with ANSWERS=yes
# the answers block: each provider the loop calls, anthropic and openai, at the model route's
# endpoint through the recording proxy (openai's under /v1, which agentloop's OpenAI client appends
# `responses` to), keyed by the route's own key file or key command, the prices, and BOUNDS (a YAML
# flow mapping) over the defaults. ANSWERS=base is the base commit's daemon's file, in the same
# mode, on runtime tmux's files store (it has neither session_store nor answers, and runs no
# merger): an agent it starts works in the same workspace the branch daemon later resumes it in, as
# an upgrade leaves it.
write_legion_config() {
  local file=$1 answers=$2 bounds=${3:-} provider
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
      [ -z "$bounds" ] || printf '  bounds: %s\n' "$bounds"
    fi
  } >"$file"
}

# start_daemon BINARY CONFIG LOG HOME PROFILE PLUGIN_DIR boots a daemon with every variable named: the
# run's HOME and profile, the run's tools first on PATH, the run's Oh My Pi (LEGION_OMP_PATH: the
# daemon takes omp_invocation only as `mise x <tool> -- omp`), the run's tmux directory, and the
# Claude Code launch. Of the host's own variables it passes the three a pane's jj and Oh My Pi
# need, each only when this shell has it: a pod has no mise and no MISE_DATA_DIR, and an empty one
# names the working directory. The daemon serves on a copy of the socket the keeper daemon_keeper
# holds for the whole run (keep_port at bring-up), handed over as LEGION_LISTEN_FD, so every boot
# serves the port each legion.yaml names. This runs what lib/rig.sh's start_kept runs, held-port.py
# exec --from the keeper's socket, itself: start_kept logs to $evidence/logs/<NAME>.log, and each
# boot appends to LOG, which the checks read from this boot's offset. It waits for /healthz, each
# poll bounded (a kept port takes a connection before the daemon serves it), and records the log
# offset this boot starts at; a daemon that exits first fails the check with its log's last lines.
start_daemon() {
  daemon_bin=$1 daemon_config=$2 daemon_log=$3 daemon_home=$4 daemon_profile=$5 daemon_plugin_dir=$6
  local started=$SECONDS name
  local -a host_env=()
  for name in XDG_CONFIG_HOME MISE_DATA_DIR JJ_CONFIG; do
    [ -z "${!name:-}" ] || host_env+=("$name=${!name}")
  done
  printf '=== boot at %s (%s)\n' "$(date -u +%FT%TZ)" "$daemon_config" >>"$daemon_log"
  boot_offset=$(stat -c %s "$daemon_log")
  # The daemon leads a process group of its own with its start recorded (lib/rig.sh's
  # start_grouped, start_process's recipe): stop_pid stops it with SIGSTOP first, and a stopped
  # member of this shell's own group would have the kernel hang up this shell too.
  start_grouped daemon_pid "$daemon_log" python3 "$root/scripts/e2e/lib/held-port.py" exec --from "$work/daemon_keeper.sock" --fd-env LEGION_LISTEN_FD -- \
    env -i "LEGION_E2E_RUN=$work" "HOME=$daemon_home" "OMP_PROFILE=$daemon_profile" "PATH=$daemon_path" "TMPDIR=$work/tmp" \
    "TMUX_TMPDIR=$work/tmux" "XDG_STATE_HOME=$work/xdg" "${host_env[@]}" \
    LANG=C.UTF-8 TZ=UTC "USER=$(id -un)" "LOGNAME=$(id -un)" SHELL=/bin/bash TERM=xterm-256color \
    "LEGION_OMP_PATH=$omp_bin" "LEGION_CLAUDE_PATH=$claude_bin" "LEGION_BUN_PATH=$work/realbin/bun" \
    "LEGION_CLAUDE_PLUGIN_DIR=$daemon_plugin_dir" LEGION_LISTEN_FD=3 \
    "$daemon_bin" start --config "$daemon_config"
  note "waiting up to 300s for the daemon ($daemon_config) to answer /healthz"
  until curl -fsS --max-time 5 "http://127.0.0.1:$port_daemon/healthz" >/dev/null 2>&1; do
    [ "$(proc_start "$daemon_pid")" = "${started_at[$daemon_pid]:-}" ] ||
      fail "the daemon exited at boot; its log $daemon_log says: $(tail -c +$((boot_offset + 1)) "$daemon_log" | grep -v '^{' | tail -3 | tr '\n' ' ')"
    ((SECONDS - started < 300)) || fail "the daemon did not answer /healthz within 300 s; see $daemon_log"
    sleep 0.5
  done
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
# map_repo PROJECT points the smoke repository at PROJECT in the Dispatch that runs now, as its
# default, and the second owner's repository at it too, never as the default: every rig issue works
# the smoke repository, and A17 names the second one in its question. Dispatch keys a mapping by
# repository, so a repository names one project at a time and the second pass's call moves the
# mapping off the first pass's project. The daemon reads no mapping (main's #4493): it takes each
# issue's repository from Dispatch, which resolves an issue that names none to its project's
# default repository, so every boot on a pass's project — A13's second round of daemons as much as
# the pass's own — sets that default first, or the pass's rig issues would resolve to none.
map_repo() {
  dispatch_human PUT "settings/repo-projects/$repo" "$(jq -cn --arg project "$1" '{project: $project, default: true}')" >/dev/null
  [ -z "$second_repo" ] ||
    dispatch_human PUT "settings/repo-projects/$second_repo" "$(jq -cn --arg project "$1" '{project: $project, default: false}')" >/dev/null
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
# With the workflow on, an operator's spawn spec carries the repository Dispatch's repo-projects
# mapping gives the issue (the smoke repository), and the tmux runtime then works in the issue's jj
# workspace at workspace.Location's <state>/workspaces/<owner>/<repo>/<issue lower-cased>, which
# must exist already (internal/runtime/tmux/spawn.go, workspaceDir): the workflow's outbox
# provisions it for an issue it admits, and it admits no rig issue (none has the `legion` label).
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
    task+=" Then make exactly one tool call: open one Dispatch ask on ${agent[$store:root]} asking a person whether to keep the 7-minute TTL, and wait."
    replacement+=" Then make exactly one tool call: open one Dispatch ask on ${agent[$store:tester.issue]} asking a person whether to keep the 20-run rerun as the release gate, and wait."
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
  claims_cmd deliver --claim "$tester" --task "$replacement" >/dev/null
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
# or returns 0 so the caller runs the check. A PREREQ names a prerequisite beyond the answering ones,
# each read from what the run has: infra448 (the listener's registry routes in this checkout, A3),
# openai (a model route that serves the GPT model, as gpt_preflight asked it, A10's GPT-6.1
# clause), second-owner (LEGION_E2E_REPO_SECOND_OWNER).
criterion() {
  local id=$1 description=$2 blockers line
  shift 2
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
  [ -n "$blockers" ] || return 0
  while IFS= read -r line; do record_result "$id" WAITING "${line#WAITING }"; done <<<"$blockers"
  return 1
}

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
# instead of a 404, then a reply on the asker's topic within four minutes holding the answer key.
# It leaves asked_question set to the question the reply answers (its legion-answer marker), ""
# where no reply came, so a check judges that answer's own log lines and no later one's.
ask_and_expect() {
  local label=$1 kind=$2 target=$3 id=$4 sent code event reply missing
  asked_question=
  sent=$(envoy_ask "$kind" "$target" "$id")
  code=${sent%% *}
  event=${sent#* }
  note "$label: the listener answered $code (event $event)"
  [ "$code" = 200 ] || return 1
  soft_wait 240 "the answer to $id on the asker's topic" inbox_reply "$event" || return 1
  reply=$(inbox_reply "$event")
  asked_question=$(answer_question_id "$reply")
  note "$label: reply: $(head -c 400 <<<"$reply" | tr '\n' ' ')"
  if missing=$(answer_holds "$id" "$reply"); then
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
  rows=$(pg_sql "omp_sessions_$run_tag" "select path, byte_len from omp_session_files order by path")
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

check_a1() {
  criterion A1 "a question on a parked tree's architect's ask is answered in the thread within four minutes, nothing of the tree starting" || return 0
  local ask a1_comment before panes_before panes_after reply
  ask=$(ask_of "${agent[$store:architect.session]}" "${agent[$store:root]}")
  [ -n "$ask" ] || {
    record_result A1 FAIL "the parked architect opened no ask on ${agent[$store:root]}"
    return 0
  }
  panes_before=$(TMUX_TMPDIR="$work/tmux" "$tmux_bin" -L "legion-${project,,}" list-panes -a -F '#{pane_id}' 2>/dev/null | wc -l || true)
  before=$(date +%s)
  a1_comment=$(dispatch_human POST "issues/${agent[$store:root]}/comments" "$(jq -cn --arg ask "$ask" --arg body "$(question_text architect-decision)" \
    '{body: $body, ask_id: $ask}')" | jq -r .id)
  agent[$store:a1.comment]=$a1_comment
  if soft_wait 240 "the architect's answer in ask $ask's thread" ask_reply_by "$ask" "${agent[$store:architect.session]}"; then
    reply=$(ask_reply_by "$ask" "${agent[$store:architect.session]}")
    panes_after=$(TMUX_TMPDIR="$work/tmux" "$tmux_bin" -L "legion-${project,,}" list-panes -a -F '#{pane_id}' 2>/dev/null | wc -l || true)
    note "answered after $(($(date +%s) - before)) s: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
    note "claims of the tree: $(claims_cmd list --json | jq -c --arg t "${agent[$store:root]}" '[.claims[] | select(.tree == $t) | {role, state}]'); panes $panes_before → $panes_after"
    if answer_holds architect-decision "$reply" >/dev/null && [ "$panes_after" -le "$panes_before" ] &&
      tree_claims_parked "$(claims_cmd list --json)" "${agent[$store:root]}"; then
      record_result A1 PASS "answered in the thread as ${agent[$store:architect.session]} within four minutes; no claim or pane of the tree started"
    else
      record_result A1 FAIL "the answer lacks its key, or a claim or pane of the tree started"
    fi
  else
    record_result A1 FAIL "no answer in ask $ask's thread within four minutes"
  fi
}

check_a2() {
  criterion A2 "a finished tester's role topic, its replaced session and a conversation known only from the title scan each answer instead of a 404" || return 0
  local passed=0 code
  ask_and_expect "role ${agent[$store:tester.token]}" role "${agent[$store:tester.token]}" tester-role && passed=$((passed + 1))
  ask_and_expect "replaced session ${agent[$store:tester_old.session]}" session "${agent[$store:tester_old.session]}" tester-old-session && passed=$((passed + 1))
  ask_and_expect "pre-change session ${agent[$store:pre_change.session]}" session "${agent[$store:pre_change.session]}" pre-change-scan && passed=$((passed + 1))
  code=$(envoy -o "$work/a2-control.json" -w '%{http_code}' --data "$(jq -cn --arg s "legion-e2eal-unregistered-$$" \
    '{target_session: $s, source: "agent", message: "control: nobody registered this session"}')" "$(listener_url /v1/messages/send)")
  note "control: an unregistered session answers $code: $(cat "$work/a2-control.json")"
  if [ "$passed" = 3 ] && [ "$code" = 404 ]; then
    record_result A2 PASS "role, replaced session and scanned conversation answered on the asker's topic; an unregistered session still 404"
  else
    record_result A2 FAIL "$passed of 3 answered with their keys; the unregistered control answered $code"
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
# is no control: the listener keeps a role message with no live holder for an hour, for the role's
# next holder, on the base commit as on the branch, so that mention's delivery stays pending.
# Dispatch settles the refusal only after it has waited out a listener take-over (takeoverRetry,
# 12 s), so the control's delivery is read again until it is refused. Then the second leg,
# a3_conversation_page: the same person, in a browser, opens the closed issue's Collaborators
# view, follows the tester's own link to its conversation page and asks a second question there.
# A3 passes only on both legs.
a3_control_refused() { one_delivery_in "$(comment_deliveries "$1")" failed "no live session $2"; }
check_a3() {
  criterion A3 "a question on a closed issue's own thread, by a role mention of its stopped tester, is answered there, and a second asked from the tester's conversation page reached through the issue's Collaborators view is answered there" infra448 || return 0
  local issue=${agent[$store:tester.issue]} session=${agent[$store:tester.session]} collaborators token comment id reply='' control control_id nobody=legion-e2eal-nobody-$run_tag before thread_leg='' page_leg=''
  set_status "$issue" "done"
  note "closed $issue: status $(dispatch_get "issues/$issue" | jq -r .status)"
  collaborators=$(dispatch_get "issues/$issue/collaborators") || collaborators='{"agents":[],"legion_registry":"unavailable"}'
  note "collaborators of $issue: registry $(jq -r .legion_registry <<<"$collaborators"); agents $(jq -c '[.agents[] | {session_id, legion: (.legion | if . then {role, role_token, replaced_by} else null end)}]' <<<"$collaborators")"
  token=$(collaborator_role_token "$collaborators" "$session")
  if [ -z "$token" ]; then
    record_result A3 FAIL "the collaborators of closed $issue name no current tester $session with its role token, so neither the thread leg nor the conversation-page leg can run"
    return 0
  fi
  before=$(date +%s)
  comment=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg body "@tester $(question_text tester-closed-thread) (question tester-closed-thread of run $run_tag)" \
    --arg t "role:$token" '{body: $body, mentions: [{target: $t}]}')")
  id=$(jq -r .id <<<"$comment")
  note "the person's comment $id on closed $issue mentions role:$token; deliveries $(jq -c '[.deliveries[]? | {target, state, session_id, error}]' <<<"$comment")"
  if soft_wait 240 "the tester's reply to comment $id" comment_reply_by "$issue" "$id" "$session"; then
    reply=$(comment_reply_by "$issue" "$id" "$session")
    note "answered after $(($(date +%s) - before)) s as $session: $(head -c 400 <<<"$reply" | tr '\n' ' ')"
  fi
  soft_wait 60 "comment $id's mention to be delivered to tester $session" comment_delivered_to "$id" "$session" || true
  note "comment $id's deliveries as Dispatch holds them now: $(dispatch_get "comments/$id" 2>/dev/null | jq -c '[.comment.deliveries[]? | {target, state, session_id, error}]' || true)"
  control_id=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg t "session:$nobody" \
    '{body: "control: a session nobody registered", mentions: [{target: $t}]}')" | jq -r '.id // empty') || control_id=
  soft_wait 60 "the control's mention of session $nobody to be refused" a3_control_refused "$control_id" "$nobody" || true
  control=$(comment_deliveries "$control_id")
  note "control: a mention of session $nobody, which nobody registered, on closed $issue (comment $control_id): $control"
  if [ -n "$reply" ] && answer_holds tester-closed-thread "$reply" >/dev/null &&
    comment_delivered_to "$id" "$session" &&
    one_delivery_in "$control" failed "no live session $nobody"; then
    thread_leg=1
    note "thread leg: the role mention on closed $issue reached tester $session, which answered in the thread; the mention of a session nobody registered failed no live session"
  else
    note "thread leg: no reply with the answer key from $session to comment $id, or Dispatch does not record its delivery as sent to it, or the control was not refused no live session $nobody"
  fi
  a3_conversation_page "$issue" "$session" && page_leg=1
  if [ -n "$thread_leg" ] && [ -n "$page_leg" ]; then
    record_result A3 PASS "the role mention on closed $issue reached tester $session, which answered in the thread from its conversation, and a question asked from that tester's conversation page, reached through the issue's Collaborators view, was answered there; the mention of a session nobody registered failed no live session"
  else
    record_result A3 FAIL "the thread leg $([ -n "$thread_leg" ] && printf 'held' || printf 'did not hold') and the conversation-page leg $([ -n "$page_leg" ] && printf 'held' || printf 'did not hold') (each leg's evidence is above)"
  fi
}

# a3_conversation_page ISSUE SESSION is A3's second leg: as the proof human, in a browser, the
# closed issue's page is opened, its Collaborators view read, and the current tester's own link
# followed to its conversation page, where question tester-conversation-page is asked and waited
# out for four minutes (lib/dispatch-user-turns.ts's `collaborator`, a screenshot per step in
# <evidence>/a3). The answer must be on the page's own thread and in the record that page reads
# (`GET /agents/{session}/messages`, the tester's own reply there), and hold the question's key in
# both. It prints the leg's evidence and returns 0 when all of that holds.
a3_conversation_page() {
  local issue=$1 session=$2 body shots page='' record='' page_answer='' api_answer='' missing
  if ! browser_leg; then
    note "conversation-page leg: no browser in this run"
    return 1
  fi
  body="$(question_text tester-conversation-page) (question tester-conversation-page of run $run_tag)"
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
  # the delivery it recorded for the stopped tester, which the listener's hand-off makes `sent`,
  # and the tester's reply to it.
  record=$(message_record "$session" "$body") || record='{}'
  note "the record the page reads (GET agents/$session/messages): deliveries $(jq -c '[.message.deliveries[]? | {state, session_id, delivery}]' <<<"$record")"
  api_answer=$(record_reply_by "$record" "$session")
  note "that record holds as the tester's reply: $(head -c 400 <<<"$api_answer" | tr '\n' ' ')"
  [ -n "$page_answer" ] && [ -n "$api_answer" ] || return 1
  if ! missing=$(answer_holds tester-conversation-page "$page_answer"); then
    note "the page's answer does not hold \"$missing\""
    return 1
  fi
  if ! missing=$(answer_holds tester-conversation-page "$api_answer"); then
    note "the recorded answer does not hold \"$missing\""
    return 1
  fi
  note "conversation-page leg: the Collaborators view named tester $session, its link opened $(jq -r '.conversation // "nothing"' <<<"$page"), and the answer there holds the question's key on the page and in Dispatch's own record"
}

check_passed_on() {
  local id=$1 crit=$2 kind expect sent code event line reply
  expect=$(question "$id" | jq -r .expect_kind)
  criterion "$crit" "$(question_text "$id")" || return 0
  sent=$(envoy_ask session "${agent[$store:planner.session]}" "$id")
  code=${sent%% *}
  event=${sent#* }
  if [ "$code" != 200 ] || ! soft_wait 240 "the planner's reply to $id" inbox_reply "$event"; then
    record_result "$crit" FAIL "the listener answered $code, or no reply reached the asker"
    return 0
  fi
  reply=$(inbox_reply "$event")
  line=$(answered_line "$(answer_question_id "$reply")") || line=
  kind=$(jq -r .kind <<<"$line")
  note "reply: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
  note "answered line: $line"
  note "questionsForController: $(legion_cmd state --json | jq -c '.questionsForController')"
  if [ "$kind" = "$expect" ] && soft_wait 60 "the question in questionsForController" passed_to_controller "$event" "$expect" &&
    [ "$(claim_json "${agent[$store:planner.token]}" | jq -r .state)" = retired ]; then
    record_result "$crit" PASS "replied as $expect, passed to the controller with the asker; the planner's claim stayed retired"
  else
    # A fault turns any reply into kind not_answered (answer.Answer), so the kind alone can read
    # as wanted when the reply was refused: the line names the fault, and what the controller
    # was told, beside it.
    record_result "$crit" FAIL "kind $kind (want $expect), fault $(jq -r '(.fault // "") | if . == "" then "none" else . end' <<<"${line:-null}")$(jq -r 'if (.detail // "") == "" then "" else " (" + .detail + ")" end' <<<"${line:-null}"), controller reason $(legion_cmd state --json | jq -r --arg q "$event" '[.questionsForController[]? | select((((.id // "") + " " + (.ref // "")) | contains($q))) | .reason] | if length == 0 then "none" else join(",") end'), claim $(claim_json "${agent[$store:planner.token]}" | jq -r .state)"
  fi
}

# decide_and_ask LEG NAME QUESTION_ID ROUTE: a person replies on the ask agent NAME opened, with a
# decision that also asks something (question QUESTION_ID). The agent's answer must come on the ask's
# thread in its name, holding the answer key, and the daemon must pass the decision on once, to ROUTE
# ("the controller", read on the controller stand-in's topic, or an architect NAME, read in its
# conversation), and to nobody else, a minute later still once. It prints the leg's evidence and
# returns 0 when all of that holds.
decide_and_ask() {
  local leg=$1 name=$2 id=$3 route=$4 session issue ask comment reply lines delivered want_to ok=0
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
  soft_wait 240 "$name's answer on ask $ask's thread" ask_reply_by "$ask" "$session" || true
  reply=$(ask_reply_by "$ask" "$session" 2>/dev/null || true)
  note "$leg: $name's answer as $session: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
  if [ "$route" = "the controller" ]; then
    delivered() { more_than 0 controller_notices agent-decision "$comment"; }
  else
    delivered() { more_than 0 notices_in_conversation "$route" "$comment"; }
  fi
  soft_wait 240 "the decision to reach $route" delivered || true
  sleep 60
  lines=$(decision_lines "$comment")
  if [ "$route" = "the controller" ]; then
    delivered=$(controller_notices agent-decision "$comment")
  else
    delivered=$(notices_in_conversation "$route" "$comment")
  fi
  note "$leg: the daemon's decision lines: $lines"
  note "$leg: agent-decision notices holding $comment at $route: $delivered; at the controller stand-in: $(controller_notices agent-decision "$comment")"
  want_to=$route
  [ "$route" = "the controller" ] || want_to=${agent[$store:$route.token]}
  if [ -n "$reply" ] && answer_holds "$id" "$reply" >/dev/null && [ "$delivered" = 1 ] &&
    decision_passed_once "$lines" "$want_to" &&
    { [ "$route" = "the controller" ] || [ "$(controller_notices agent-decision "$comment")" = 0 ]; }; then
    ok=1
  fi
  [ "$ok" = 1 ]
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
      "$(jq -r .agents.a6_child.task "$questions_file") Then make exactly one tool call: open one Dispatch ask on $child6 asking a person whether to quarantine TestGarnetFalcon, and wait."
    await_turn a6_child
    if suspend_agent a6_child; then
      note "the parked child's tester ${agent[$store:a6_child.session]} is suspended; the tree's architect $architect is $(claim_json "$architect" | jq -c '{state, session}')"
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

check_a7() {
  criterion A7 "a question to a running agent reaches it alone; a resume during generation appends nothing and forwards the question" || return 0
  local token=${agent[$store:architect.token]} label=daemon-$store before after sent event n k appended_before appended_after first_ok=0 second_ok=0
  if ! resume_agent architect; then
    record_result A7 FAIL "the architect did not run again"
    return 0
  fi
  before=$(boot_log | jq -s '[.[] | select(.msg | startswith("answers:"))] | length')
  sent=$(envoy_ask session "${agent[$store:architect.session]}" in-order-1)
  holds_first() { conversation_holds architect "question in-order-1 of run $run_tag"; }
  soft_wait 180 "the running architect's conversation to hold the question" holds_first || true
  sleep 20
  after=$(boot_log | jq -s '[.[] | select(.msg | startswith("answers:"))] | length')
  note "to the running architect: listener $sent; the daemon's answers lines $before → $after; its conversation holds the question: $(holds_first && echo yes || echo no)"
  [ "$before" = "$after" ] && [ "${sent%% *}" = 200 ] && holds_first && first_ok=1
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
  note "the running architect holds the forwarded question: $(delivered && echo yes || echo no); the exchanges appended to its conversation: $appended_before → $appended_after; replies on the asker's topic: $(inbox_replies "$event")"
  dropped && delivered && [ "$(inbox_replies "$event")" = 0 ] && [ "$appended_after" = "$appended_before" ] && second_ok=1
  if [ "$first_ok$second_ok" = 11 ]; then
    record_result A7 PASS "the running architect alone took the first question (no answers line); the second, its claim started mid-answer, was dropped and forwarded with no exchange appended (still $appended_after) and nothing posted"
  else
    record_result A7 FAIL "running agent alone $first_ok, started mid-answer forwarded with no exchange appended $second_ok (1 = shown; the appended exchanges went $appended_before → $appended_after)"
  fi
  suspend_agent architect || true
}

check_a8() {
  criterion A8 "one question delivered twice, across a restart and across a kill just after its answer posted, gets one answer; the loop's own answer is delivered into its asker's conversation unanswered, and an edit is not answered" || return 0
  local planner=${agent[$store:planner.session]} key=a8-$run_tag key3=a8-kill-$run_tag sent resend resent event event3 qid again taken codes handoffs replies posted_before own_before edit_before ok_twice=0 ok_kill=0 ok_own=0 ok_edit=0
  local tester_old delivered_msg handed_before appended_before appended_after calls_before refused_before own_qid own_marker
  # One envelope sent three times under one idempotency key, the third after a restart. The
  # listener hands each send off, and every hand-off carries the envelope's dedupe key as its
  # MsgId, so the stream stores one copy: the daemon is handed the question once and answers it
  # once, and the restarted daemon is handed nothing — no second hand-off for it to log as handed
  # off again, and no second answer.
  sent=$(envoy_ask session "$planner" unanswerable "$key")
  event=${sent#* }
  resend=$(envoy_ask session "$planner" unanswerable "$key")
  soft_wait 240 "the answer" inbox_reply "$event" || true
  stop_daemon
  start_daemon "$daemon_bin" "$daemon_config" "$daemon_log" "$daemon_home" "$daemon_profile" "$daemon_plugin_dir"
  resent=$(envoy_ask session "$planner" unanswerable "$key")
  sleep 60
  qid=$(answer_question_id "$(inbox_reply "$event" || true)")
  again=$(answers_lines "answers: question handed off again; not answered again" "$planner")
  taken=$(question_lines "$qid")
  codes="${sent%% *} ${resend%% *} ${resent%% *}"
  handoffs="$(listener_handoffs "$event") $(listener_handoffs "${resend#* }") $(listener_handoffs "${resent#* }")"
  replies=$(($(inbox_replies "$event") + $(inbox_replies "${resend#* }") + $(inbox_replies "${resent#* }")))
  note "one envelope three times (listener $codes), the third across a restart: hand-offs published for the three sends $handoffs; replies on the asker's topic $replies; the restarted daemon's lines naming the question $qid: $taken, of them handed off again $again"
  [ "$codes" = "200 200 200" ] && [ "$handoffs" = "1 1 1" ] && [ "$replies" = 1 ] && [ -n "$qid" ] && [ "$taken" = 0 ] && [ "$again" = 0 ] && ok_twice=1
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
  note "after the kill and restart: replies $(inbox_replies "$event3"); this boot's lines for the planner: $(boot_log | jq -s -c --arg s "$planner" '[.[] | select((.msg | startswith("answers:")) and .session == $s) | {msg, state}]')"
  [ "$(inbox_replies "$event3")" = 1 ] && ok_kill=1
  # The loop's own answer: a question to the planner sent as the stopped, replaced tester session.
  # The planner's answer goes back to that session in the planner's name; no live session holds
  # it, so the listener hands the answer to this daemon, which never answers an answer of its own
  # — no model call for it, no question of it, nothing posted, nothing passed to the controller —
  # and writes it into the stopped tester's conversation instead, once, as a message it received,
  # so that tester holds the answer when it runs again. The delivery is counted by the marker
  # entry the append ends with (appended_exchanges) and by the answer's own
  # `legion-answer:<question>` marker inside the entry, which only the delivered message carries.
  tester_old=${agent[$store:tester_old.session]}
  delivered_msg="answers: the loop's own answer is delivered into the asking agent's conversation"
  posted_before=$(answers_lines "answers: question" "$planner" posted)
  own_before=$(answers_lines "answers: question" "$tester_old")
  handed_before=$(listener_handoffs_to "$tester_old")
  appended_before=$(appended_exchanges tester_old)
  calls_before=$(proxy_count "daemon-$store")
  refused_before=$(controller_questions post_refused)
  envoy -o /dev/null --data "$(jq -cn --arg s "$planner" --arg from "$tester_old" --arg m "$(question_text repository-file) (question own-answer of run $run_tag)" \
    '{target_session: $s, source: "agent", source_session: $from, message: $m}')" "$(listener_url /v1/messages/send)"
  soft_wait 300 "the planner's answer to the tester's session to post" more_than "$posted_before" answers_lines "answers: question" "$planner" posted || true
  own_qid=$(newest_question "$planner" posted)
  own_delivered() { [ "$(answers_lines "$delivered_msg" "$tester_old")" -ge 1 ]; }
  soft_wait 300 "the planner's answer to reach the stopped tester's conversation" own_delivered || true
  sleep 60
  appended_after=$(appended_exchanges tester_old)
  own_marker=$(occurrences "$(conversation_text tester_old)" "legion-answer:$own_qid")
  note "the planner's answers posted $posted_before → $(answers_lines "answers: question" "$planner" posted), its answer's question $own_qid; hand-offs to the replaced tester $handed_before → $(listener_handoffs_to "$tester_old"), delivered lines $(answers_lines "$delivered_msg" "$tester_old"); that conversation's appended entries $appended_before → $appended_after, holding the answer's marker $own_marker time(s); questions for it $own_before → $(answers_lines "answers: question" "$tester_old"), answers $(answers_lines "answers: answered" "$tester_old"); a model call whose body holds the answer's marker: $(daemon_request_for "legion-answer:$own_qid"); the daemon's model calls $calls_before → $(proxy_count "daemon-$store"); post_refused questions for the controller $refused_before → $(controller_questions post_refused)"
  [ "$(answers_lines "answers: question" "$planner" posted)" -gt "$posted_before" ] && [ -n "$own_qid" ] &&
    [ "$(listener_handoffs_to "$tester_old")" -gt "$handed_before" ] &&
    [ "$(answers_lines "$delivered_msg" "$tester_old")" = 1 ] && [ "$own_marker" = 1 ] &&
    [ "$appended_after" = "$((appended_before + 1))" ] &&
    [ "$(answers_lines "answers: question" "$tester_old")" = "$own_before" ] &&
    [ "$(answers_lines "answers: answered" "$tester_old")" = 0 ] &&
    [ "$(daemon_request_for "legion-answer:$own_qid")" = null ] &&
    [ "$(controller_questions post_refused)" = "$refused_before" ] && ok_own=1
  # An edit: the person edits the comment A1's question was, on the open root issue. A closed
  # issue refuses every comment edit (409 ISSUE_CLOSED, comment_actions.go:19-31 lockedComment).
  if [ -n "${agent[$store:a1.comment]:-}" ]; then
    edit_before=$(answers_lines "answers: question" "${agent[$store:architect.session]}")
    dispatch_human PATCH "comments/${agent[$store:a1.comment]}" "$(jq -cn --arg b "$(question_text architect-decision) (edited)" '{body: $b}')" >/dev/null
    sleep 60
    note "after the person edited comment ${agent[$store:a1.comment]}: questions for the architect $edit_before → $(answers_lines "answers: question" "${agent[$store:architect.session]}")"
    [ "$(answers_lines "answers: question" "${agent[$store:architect.session]}")" = "$edit_before" ] && ok_edit=1
  fi
  if [ "$ok_twice$ok_kill$ok_own$ok_edit" = 1111 ]; then
    record_result A8 PASS "one envelope sent three times under one key, the third across a restart: each send handed off, the stream stored one copy, one answer and nothing handed to the restarted daemon; killed just after the post and delivered again: one answer; the loop's own answer to the stopped tester was handed to the daemon, written into that tester's conversation once with no model call, no answer and nothing passed to the controller; an edit was not answered"
  else
    record_result A8 FAIL "twice and restart $ok_twice, kill after post $ok_kill, own answer delivered unanswered $ok_own, edit unanswered $ok_edit (1 = shown)"
  fi
}

check_a9() {
  criterion A9 "two questions at once are answered in order, and the resumed agent holds both once and carries on from them" || return 0
  local token=${agent[$store:architect.token]} t1 t2 s1 s2 e1 e2 first reply m request q1 q2 counts
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
  first=$(first_reply_of "$e1" "$e2")
  q1=$(answer_question_id "$(inbox_reply "$e1")")
  q2=$(answer_question_id "$(inbox_reply "$e2")")
  note "answers: 1: $(head -c 200 <<<"$(inbox_message "$e1")" | tr '\n' ' ') | 2: $(head -c 200 <<<"$(inbox_message "$e2")" | tr '\n' ' ')"
  m=$(proxy_count agents)
  resume_agent architect || true
  claims_cmd deliver --claim "$token" --task "$(question_text after-resume)" >/dev/null
  soft_wait 300 "the resumed architect's first model call" more_than "$m" proxy_count agents || true
  soft_wait 600 "the resumed architect's turn" claim_is "$token" '.state == "idle" and .pending == null' || true
  request=$(first_request_after agents "$m")
  counts="question 1 $(question_occurrences "$request" in-order-1 "$run_tag" A9), question 2 $(question_occurrences "$request" in-order-2 "$run_tag" A9), exchange 1 $(appended_exchanges architect "$q1"), exchange 2 $(appended_exchanges architect "$q2")"
  # Every reply the architect gave since the task was delivered, not its last one: on resume it may
  # also answer a person's earlier reply in the same turn, and its last words then answer that.
  reply=$(assistant_texts_after architect "$(question_text after-resume)")
  note "the first reply on the asker's topic answers question 1: $(grep -qF "In Reply To: $e1" <<<"$first" && echo yes || echo no)"
  note "the resumed architect's first request (agents, after its first $m model calls) holds each question, and its conversation the exchange appended for each (questions $q1 and $q2): $counts"
  note "the resumed architect, since the task: $(head -c 800 <<<"$reply" | tr '\n' ' ')"
  suspend_agent architect || true
  if grep -qF "In Reply To: $e1" <<<"$first" && [ -n "$q1" ] && [ -n "$q2" ] && [ "$counts" = "question 1 1, question 2 1, exchange 1 1, exchange 2 1" ] &&
    answer_holds after-resume "$reply" >/dev/null; then
    record_result A9 PASS "answered one after the other; the resumed architect's first request held each question once, its conversation holds one appended exchange per question, and it quoted both answers"
  else
    record_result A9 FAIL "out of order, or the first request did not hold each question once, or the conversation does not hold one appended exchange per question ($counts), or the resumed agent did not carry on from them"
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
# between its two. A fourth would cost another answer's wait (up to 240 s) and another question
# the resume must hold, against a miss none of the three showed. It is judged from the recording
# proxy's records of each answer's own calls (answer_calls: the daemon's label, the question's
# marker and not the next question's) and from the daemon's `answers: answered` lines:
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
#   (e) each reply holds its question's answer key;
#   (f) resumed on its own route (agents-gpt), the merger's first request is for the model and
#   holds every question asked once each.
# A transcript with no tool call, or with no reasoning item carrying encrypted content, which
# leaves (b) nothing to replay, and answers within the bound none of which had a step that
# returned reasoning followed by another, which leave (c) nothing to carry, each fail the clause
# and say what went unexercised, since a request carries every item of none.
a10_gpt() {
  local session=${agent[$store:gpt.session]:-} token=${agent[$store:gpt.token]:-}
  local transcript second summary level want id s e n k calls line reply missing cached1 cached2
  local replay1 replay2 rgot1 rof1 rmiss1 rgot2 rof2 rmiss2 carried owed exercised='' m request counts='' want_counts=''
  local unexercised='' uncarried='' unreplayed='' unkeyed='' ok_tools=0 ok_model=1 ok_effort=1 ok_replay=0 ok_carried=0 ok_cache=0 ok_keys=1 ok_resume=0
  local -a ids=(gpt-repository-module gpt-repository-go) sent=() replies=() answers=() lines=() carries=()
  if [ -n "${agent[$store:gpt.why]:-}" ] || [ -z "$session" ]; then
    record_result A10 FAIL "GPT-6.1: ${agent[$store:gpt.why]:-the merger registered no session}"
    return 0
  fi
  transcript=$(conversation_text gpt)
  summary=$(omp_openai_summary "$transcript")
  level=$(omp_thinking_level "$transcript")
  want=$(openai_effort "$level")
  note "GPT-6.1: the merger's conversation last ran at thinking level \"$level\", effort \"$want\" in OpenAI's words; its turns $summary"
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
  [ -z "$unexercised" ] || note "$unexercised: the GPT-6.1 clause fails"
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
      # The conversation the second answer's first request replays, kept beside the one the first
      # answer's replays (save_conversations' gpt.jsonl).
      second=$(conversation_text gpt)
      mkdir -p "$evidence/sessions/$store"
      printf '%s\n' "$second" >"$evidence/sessions/$store/gpt-before-A10-gpt-2.jsonl"
    fi
    s=$(envoy_ask_text session "$session" "$(question_text "$id") $(question_suffix "$id" "$run_tag" "A10-gpt-$n")")
    sent+=("$s")
    e=${s#* }
    soft_wait 240 "the GPT-6.1 merger's answer to $id (A10-gpt-$n)" inbox_reply "$e" || true
    reply=$(inbox_reply "$e" || true)
    replies+=("$reply")
    calls=$(answer_calls "daemon-$store" "A10-gpt-$n)" "A10-gpt-$((n + 1)))")
    carries+=("$(reasoning_carried "$calls")")
  done
  # Every answer's calls read again now that the last is posted, so that none is judged short of a
  # call the proxy had not yet recorded when the leg decided whether to ask again.
  carries=()
  for ((k = 1; k <= n; k++)); do
    calls=$(answer_calls "daemon-$store" "A10-gpt-$k)" "A10-gpt-$((k + 1)))")
    answers+=("$calls")
    carries+=("$(reasoning_carried "$calls")")
    line=$(answered_line "$(answer_question_id "${replies[k - 1]}")") || line=
    lines+=("$line")
    note "GPT-6.1: A10-gpt-$k (${ids[k - 1]}): listener ${sent[k - 1]}; its calls on daemon-$store $(jq -c '[.[] | {ordinal, path, model: .request_body.model, status}]' <<<"$calls"), efforts $(calls_efforts "$calls") (want \"$want\"); of the reasoning its earlier steps returned, its later requests carried ${carries[k - 1]} (carried, owed); answered line $(jq -c '{model, calls, cacheReadTokens, cacheWriteTokens, costUSD}' <<<"${line:-null}"); reply: $(head -c 300 <<<"${replies[k - 1]}" | tr '\n' ' ')"
    calls_on_model "$calls" /v1/responses "$gpt_model_id" && [ "$(jq -r '.model // ""' <<<"${line:-null}")" = "$gpt_model" ] || ok_model=0
    efforts_all "$calls" "$want" || ok_effort=0
    if ! missing=$(answer_holds "${ids[k - 1]}" "${replies[k - 1]}"); then
      ok_keys=0
      unkeyed+="${unkeyed:+; }the reply to A10-gpt-$k (${ids[k - 1]}) lacks \"$missing\""
    fi
    [ "${carries[k - 1]#* }" = 0 ] || exercised+="${exercised:+, }A10-gpt-$k (${ids[k - 1]}, ${carries[k - 1]% *} of ${carries[k - 1]#* } carried)"
  done
  replay1=$(reasoning_replayed "$transcript" "$(jq -c '.[0].request_body // null' <<<"${answers[0]}")")
  replay2=$(reasoning_replayed "$second" "$(jq -c '.[0].request_body // null' <<<"${answers[1]}")")
  cached1=$(call_cached_tokens "$(jq -c '.[0] // null' <<<"${answers[0]}")")
  cached2=$(call_cached_tokens "$(jq -c '.[0] // null' <<<"${answers[1]}")")
  note "GPT-6.1: of the transcript's reasoning items each cache answer's first request carried $replay1 and $replay2 (carried, stored, missing); each cache answer's first call read $cached1 and $cached2 cached input tokens (its response's usage)"
  read -r rgot1 rof1 rmiss1 <<<"$replay1"
  read -r rgot2 rof2 rmiss2 <<<"$replay2"
  [ "$rof1" -gt 0 ] && [ "$rgot1" = "$rof1" ] && [ "$rof2" -gt 0 ] && [ "$rgot2" = "$rof2" ] && ok_replay=1
  [ -z "$rmiss1" ] || unreplayed="the first answer's first request lacks the stored reasoning items $rmiss1"
  [ -z "$rmiss2" ] || unreplayed+="${unreplayed:+; }the second answer's first request lacks the stored reasoning items $rmiss2"
  read -r carried owed <<<"$(carried_total "${carries[@]}")"
  if [ "$owed" = 0 ]; then
    uncarried="no step of the $n answers asked (the two cache questions and $((n - 2)) of the questions file's a10.reasoning list, which is the bound) that returned reasoning was followed by another step, so the clause that a later step carries an earlier step's reasoning is unexercised"
    note "$uncarried"
  elif [ "$carried" = "$owed" ]; then
    ok_carried=1
  else
    uncarried="later requests carried $carried of the $owed pairs of a reasoning item an earlier step returned and a later request of its answer: $exercised"
  fi
  [ "$(cache_read_tokens "${lines[1]}")" -gt 0 ] && [ "$cached2" -gt 0 ] && ok_cache=1
  m=$(proxy_count agents-gpt)
  resume_agent gpt || true
  claims_cmd deliver --claim "$token" --task "While you were stopped you were asked $n questions about go.mod. What did you answer each time?" >/dev/null || true
  soft_wait 300 "the resumed GPT-6.1 merger's first model call" more_than "$m" proxy_count agents-gpt || true
  soft_wait 600 "the resumed merger's turn" claim_is "$token" '.state == "idle" and .pending == null' || true
  request=$(first_request_after agents-gpt "$m")
  for ((k = 1; k <= n; k++)); do
    counts+="${counts:+ }$(question_occurrences "$request" "${ids[k - 1]}" "$run_tag" "A10-gpt-$k")"
    want_counts+="${want_counts:+ }1"
  done
  note "GPT-6.1: the resumed merger's first request (agents-gpt, after its first $m model calls), for $(jq -r '.model // "no model"' <<<"$request"), holds each of the $n questions this many times: $counts"
  [ "$counts" = "$want_counts" ] && [ "$(jq -r '.model // ""' <<<"$request")" = "$gpt_model_id" ] && ok_resume=1
  suspend_agent gpt || true
  if [ "$ok_tools$ok_model$ok_effort$ok_replay$ok_carried$ok_cache$ok_keys$ok_resume" = 11111111 ]; then
    record_result A10 PASS "GPT-6.1: a conversation with $(jq -r .tool_calls <<<"$summary") tool call(s) answered $n times on $gpt_model_id through /v1/responses (${ids[*]}), every request at effort $want, the level it last ran at; each cache answer's first request carried every reasoning item its transcript stored ($rof1, then $rof2) with its encrypted content byte for byte, and each later step the reasoning an earlier step returned, all $owed carried byte for byte ($exercised); the second answer's first call read $cached2 tokens from cache, its cost line $(cache_read_tokens "${lines[1]}"); every reply holds its keys; resumed in Oh My Pi on $gpt_model_id, its first request holding each question once"
  else
    record_result A10 FAIL "GPT-6.1: a tool call in the conversation $ok_tools, on the model $ok_model, effort $ok_effort, the transcript's reasoning replayed $ok_replay, earlier steps' reasoning carried $ok_carried ($carried of $owed over $n answers), the second answer read from cache $ok_cache (its first call $cached2 tokens, its cost line $(cache_read_tokens "${lines[1]}")), answer keys $ok_keys, resumed holding each question once $ok_resume (1 = shown)${unexercised:+; $unexercised}${unreplayed:+; $unreplayed}${uncarried:+; $uncarried}${unkeyed:+; $unkeyed}"
  fi
}

# check_a10: A10's GPT-6.1 clause (a10_gpt, where the model route serves the model), its Claude
# Code leg (the pre-change reviewer, answered twice within the cache lifetime: the first request
# carries the effort the transcript last ran at and every thinking signature it holds, byte for
# byte; an answer of a pair one Anthropic organization served reads the conversation from cache;
# resumed after, its first request holds both exchanges) and its compaction leg (the architect's
# session compacted by the daemon's own compaction, is answered from the compacted conversation,
# and resumes after holding the exchange). The byte-for-byte clause needs thinking to replay, which
# is why the pre-change reviewer runs at a raised effort (produce_pre_change); a transcript with no
# thinking signature fails the leg and says why, since any request carries every signature of a
# transcript that holds none.
check_a10() {
  if criterion A10 "a GPT-6.1 conversation with reasoning and a tool call is answered by that model at its effort, its reasoning replayed and carried byte for byte, its second answer read from cache, and it resumes in Oh My Pi" openai; then
    a10_gpt
  fi
  criterion A10 "a Claude Code worker and a compacted Oh My Pi conversation are answered from their conversations and resume after" || return 0
  local session=${agent[$store:pre_change.session]} token=${agent[$store:pre_change.token]} t1 t2 s1 s2 e1 e2 line request transcript effort want_effort sigs thinking unexercised='' missing m counts ok_claude=0 ok_compact=0 t3 s3 e3 compaction after
  local asked_at k marker again pair='' pairs='' cache_why='' found judged=''
  local -a markers events organizations
  transcript=$(conversation_text pre_change)
  want_effort=$(transcript_effort "$transcript")
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
  fi
  request=$(daemon_request_for "A10-1)")
  effort=$(request_effort "$request")
  missing=0
  while IFS= read -r sig; do
    [ -z "$sig" ] || grep -qF -- "$sig" <<<"$request" || missing=$((missing + 1))
  done <<<"$sigs"
  note "the transcript's last effort \"$want_effort\", the first answer's request effort \"$effort\"; the transcript's thinking blocks $thinking, its thinking signatures $(grep -c . <<<"$sigs" || true), missing from the request $missing"
  note "answers: $(head -c 200 <<<"$(inbox_reply "$e1")" | tr '\n' ' ') | $(head -c 200 <<<"$(inbox_reply "$e2")" | tr '\n' ' ')"
  if [ -n "$pair" ]; then
    note "the cache-read clause reads the cost line of the answer to ${markers[judged]}, the later of the first two answers one organization served ($pair): ${line:-none}"
  else
    note "the cache-read clause has no pair to read: $cache_why"
  fi
  m=$(proxy_count "claude-$store")
  resume_agent pre_change || true
  claims_cmd deliver --claim "$token" --task "While you were stopped you were asked what blocked the release. How many times were you asked, and what did you answer?" >/dev/null
  soft_wait 300 "the resumed Claude Code reviewer's first model call" more_than "$m" proxy_count "claude-$store" || true
  soft_wait 600 "the resumed reviewer's turn" claim_is "$token" '.state == "idle" and .pending == null' || true
  request=$(first_request_after "claude-$store" "$m")
  counts="$(question_occurrences "$request" pre-change-scan "$run_tag" A10-1) $(question_occurrences "$request" pre-change-scan "$run_tag" A10-2)"
  note "the resumed Claude Code reviewer's first request (claude-$store, after its first $m model calls) holds the two questions: $counts"
  suspend_agent pre_change || true
  if answer_holds pre-change-scan "$(inbox_reply "$e1")" >/dev/null && answer_holds pre-change-scan "$(inbox_reply "$e2")" >/dev/null &&
    [ -n "$want_effort" ] && [ "$effort" = "$want_effort" ] && [ -n "$sigs" ] && [ "$missing" = 0 ] &&
    [ "$(cache_read_tokens "$line")" -gt 0 ] && [ "$counts" = "1 1" ]; then
    ok_claude=1
  fi
  # The compaction leg: the architect's session compacted as Legion compacts one, by the daemon's
  # own compaction (`legion claims compact`, packages/daemon/internal/api/compact.go), which has Oh
  # My Pi write the summary as text to instructions of its own (compactInstructions). A compaction
  # that carries no instructions, a person's bare `/compact` among them, is Oh My Pi's image-based
  # snapcompact under the pinned release, and the loop's reader declines that one: the question
  # after it is passed on, not answered (compactionSummary, conversation/omp/context.go). So the
  # leg waits for a compaction whose summary is text (text_compactions) and counts no other kind.
  resume_agent architect || true
  compaction=$(claims_cmd compact --claim "${agent[$store:architect.token]}" 2>&1) || compaction="refused: $compaction"
  note "legion claims compact: $(tr '\n' ' ' <<<"$compaction")"
  compacted() { [ "$(text_compactions architect)" -gt 0 ]; }
  soft_wait 300 "the architect's conversation to hold a compaction written as text" compacted || true
  note "the architect's conversation holds a compaction written as text: $(compacted && echo yes || echo no) ($(occurrences "$(conversation_text architect)" '"type":"compaction"') compaction entries of any kind)"
  suspend_agent architect || true
  t3="$(question_text in-order-2) $(question_suffix in-order-2 "$run_tag" "A10 after compaction")"
  s3=$(envoy_ask_text session "${agent[$store:architect.session]}" "$t3")
  e3=${s3#* }
  soft_wait 240 "the answer from the compacted conversation" inbox_reply "$e3" || true
  note "from the compacted conversation: $(head -c 300 <<<"$(inbox_reply "$e3" 2>/dev/null)" | tr '\n' ' ')"
  m=$(proxy_count agents)
  resume_agent architect || true
  claims_cmd deliver --claim "${agent[$store:architect.token]}" --task "Reply with one word: ready." >/dev/null
  soft_wait 300 "the resumed architect's first model call" more_than "$m" proxy_count agents || true
  request=$(first_request_after agents "$m")
  after=$(question_occurrences "$request" in-order-2 "$run_tag" "A10 after compaction")
  note "the resumed architect's first request holds the question asked after the compaction $after time(s)"
  compacted && inbox_reply "$e3" >/dev/null && answer_holds in-order-2 "$(inbox_reply "$e3")" >/dev/null &&
    [ "$after" = 1 ] && ok_compact=1
  suspend_agent architect || true
  if [ "$ok_claude$ok_compact" = 11 ]; then
    record_result A10 PASS "Claude Code: effort $effort and each of the transcript's $(grep -c . <<<"$sigs") thinking signatures replayed, the answer to ${markers[judged]}, the later of the pair $pair one organization served, read $(cache_read_tokens "$line") tokens from cache, resumed holding both; the compacted conversation answered and resumed holding the exchange"
  else
    record_result A10 FAIL "Claude Code leg $ok_claude, compaction leg $ok_compact (1 = shown)${unexercised:+; $unexercised}${cache_why:+; $cache_why}"
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
a12_stand_in() {
  local file=$work/proxy-stand-in/daemon-$store.json
  case "$1" in
    malformed)
      jq -n '{type: "message", role: "assistant", content: [{type: "text", text: "not the reply form"}], stop_reason: "end_turn", usage: {input_tokens: 10, output_tokens: 5}}' >"$file"
      ;;
    model_calls)
      jq -n --arg repo "$repo" '{type: "message", role: "assistant", content: [{type: "tool_use", id: "toolu_a12_model_calls", name: "gh", input: {args: ["api", "repos/\($repo)/commits/main"]}}], stop_reason: "tool_use", usage: {input_tokens: 10, output_tokens: 5}}' >"$file"
      ;;
    *) rm -f "$file" ;;
  esac
}

check_a12() {
  criterion A12 "a reply past a bound or out of form is not posted; the daemon replies naming the fault and passes the question on" || return 0
  local entry bounds fault sent event reply answered passed=0 ok_calls=0
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
    reply=
    if soft_wait 300 "the $fault fault to be passed on" passed_on && soft_wait 60 "the daemon's reply" inbox_reply "$event"; then
      reply=$(inbox_reply "$event")
      note "$fault: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
      grep -qF 'not the reply form' <<<"$reply" || passed=$((passed + 1))
    fi
    if [ "$fault" = model_calls ]; then
      answered=$(answered_line "$(answer_question_id "$reply")") || answered=
      note "the model_calls leg's answered line: ${answered:-none}"
      answered_fault_is "$answered" model_calls 1 && ok_calls=1
    fi
    a12_stand_in none
  done
  stop_daemon
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$branch_plugin"
  if [ "$passed" = 5 ] && [ "$ok_calls" = 1 ]; then
    record_result A12 PASS "each of model_calls, answer_seconds, length, malformed and over_limit ended with the daemon's own reply and the question passed on; the model_calls leg's first reply called the gh tool, so the one model call the bound allows still called a tool (answers: answered, fault model_calls, calls 1)"
  else
    record_result A12 FAIL "$passed of 5 faults ended as they must; the model_calls leg's answered line carries that fault after its one call: $ok_calls (1 = shown)"
  fi
}

check_a14() {
  criterion A14 "a cost line per answer, cache writes included, recomputed from the captured usage and the prices" || return 0
  local lines bad unpriced
  # Every boot of the run, not this one: A12 restarts the daemon five times before this check, and
  # its own boot has answered nothing yet. A line of a model the read has no price for is one whose
  # cost nothing recomputes, and fails the criterion rather than passing unread.
  lines=$(run_log | jq -c 'select(.msg == "answers: answered" and .fault == "")')
  bad=$(bad_cost_lines "$lines")
  unpriced=$(unpriced_cost_lines "$lines")
  note "cost lines this run: $(jq -s -c 'map({question, model, inputTokens, outputTokens, cacheWriteTokens, cacheReadTokens, costUSD})' <<<"$lines")"
  if [ -n "$lines" ] && [ "$bad" = "[]" ] && [ "$unpriced" = "[]" ]; then
    record_result A14 PASS "every answer's costUSD equals its tokens at the configured prices of its model, Claude Sonnet 5.5 or GPT-6.1, cache writes included ($(grep -c . <<<"$lines") cost lines, every boot of the run)"
  else
    record_result A14 FAIL "no cost line, or a cost that does not recompute: $bad; lines of a model with no price to recompute them at: $unpriced"
  fi
}

# check_a15 asks each of the two stopped agents to read go.mod at a fixed commit of the smoke
# repository, which neither can answer from its own conversation: the loop must call the gh tool,
# as the review App for the repository's owner, and the answer must come back with the file's
# words. That holds only while this is the first time the pass asks it — A8's own-answer leg and
# A12's five legs ask the same question later, and a conversation that already holds the file is
# one the loop may answer with no call at all — so the matrix runs this check before them.
check_a15() {
  criterion A15 "an answer read from the repository with gh, the log naming App and owner, the call in the conversation; the same for the Claude Code worker" || return 0
  local ok_omp=0 ok_claude=0 unread= lines
  if ask_and_expect "planner, repository file" session "${agent[$store:planner.session]}" repository-file; then
    lines=$(question_gh_lines "$asked_question") || lines='[]'
    note "the gh lines of the planner's answer ${asked_question:-none}: $lines"
    [ "$lines" != "[]" ] || unread="$unread the planner"
    first_line_where "$lines" gh_line_is review sjawhar 0 >/dev/null && conversation_holds planner 'stage4b-fixture' && ok_omp=1
  fi
  if ask_and_expect "Claude Code reviewer, repository file" session "${agent[$store:pre_change.session]}" repository-file; then
    lines=$(question_gh_lines "$asked_question") || lines='[]'
    note "the gh lines of the Claude Code reviewer's answer ${asked_question:-none}: $lines"
    [ "$lines" != "[]" ] || unread="$unread the Claude Code reviewer"
    first_line_where "$lines" gh_line_is review sjawhar 0 >/dev/null && conversation_holds pre_change 'stage4b-fixture' && ok_claude=1
  fi
  if [ "$ok_omp$ok_claude" = 11 ]; then
    record_result A15 PASS "both answered from go.mod with gh as the review App on sjawhar; each conversation holds the call"
  else
    record_result A15 FAIL "Oh My Pi planner $ok_omp, Claude Code reviewer $ok_claude (1 = answered with the call in the log and the conversation)${unread:+; no gh call is logged for the answer of:$unread}"
  fi
}

check_a16() {
  local pattern hits unrelated paths
  store=-
  printf '== A16: the search for the loop described as tool-less finds no line about the loop\n'
  pattern=$(a16_pattern)
  paths=(packages/daemon/internal/answer packages/daemon/internal/answerloop packages/daemon/internal/ghtool
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
  test_out=$(cd "$root/packages/daemon" && go test -count=1 ./internal/answer -run TestTheLoopsToolListHoldsGh -v 2>&1) || true
  note "$(grep -E '^(--- |ok|FAIL)' <<<"$test_out" | tr '\n' ' ')"
  if [ -z "$unrelated" ] && grep -q '^--- PASS: TestTheLoopsToolListHoldsGh' <<<"$test_out"; then
    record_result A16 PASS "every hit is one the questions file records as not about the loop; TestTheLoopsToolListHoldsGh passes"
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

# message_record SESSION BODY prints the proof person's record of their targeted message to SESSION
# whose body is BODY (GET agents/<session>/messages, which serves the signed-in person's own
# messages and refuses the agents' bearer), {} for none.
message_record() {
  dispatch_human GET "agents/$1/messages" | jq -c --arg b "$2" 'first(.[]? | select(.message.body == $b)) // {}'
}

# message_reply_by SESSION BODY prints SESSION's reply to the proof person's targeted message whose
# body is BODY, from that person's record of it (message_record), and fails while it holds none.
message_reply_by() {
  local reply
  reply=$(record_reply_by "$(message_record "$1" "$2")" "$1")
  [ -n "$reply" ] && printf '%s\n' "$reply"
}

check_a17() {
  criterion A17 "the stopped implementer comments on its pull request as the implement App (the control); a comment the shared leak check refuses is never written and the agent carries the check's own reason back; the reviewer's write to another owner is refused" second-owner || return 0
  local marker=answer-loop-a17-$run_tag-$store comments reply refusal listing refused_comments lines
  local ok_impl=0 ok_leak=0 ok_review=0 id
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
    comments=$(pr_comments "${agent[$store:implementer.pr]}" "$marker")
    for id in $(jq -r '.[].id' <<<"$comments"); do github_writes+=("comment:$id"); done
    reply=$(impl_reply)
    lines=$(question_gh_lines "$(answer_question_id "$reply")") || lines='[]'
    note "comments: $(jq -c 'map({id, user, footer: (.body | [capture("legion: (?<f>\\{[^}]*\\})").f] | first // null)})' <<<"$comments")"
    note "the implementer's answer's gh lines: $lines"
    note "reply: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
    one_comment_by "$comments" "$implement_app_login" "$impl_session" &&
      first_line_where "$lines" gh_line_is implement sjawhar 0 >/dev/null && ok_impl=1
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
    refused_comments=$(jq -c --arg m "$leak_marker" \
      '(if type == "array" then . else [] end) | [.[] | select((.body // "") | contains($m)) | {id, user: .user.login}]' <<<"$listing")
    for id in $(jq -r '.[].id' <<<"$refused_comments"); do github_writes+=("comment:$id"); done
    reply=$(impl_reply)
    lines=$(question_leak_refusals "$(answer_question_id "$reply")") || lines='[]'
    note "comments on #${agent[$store:implementer.pr]}: $(jq -r 'if type == "array" then length | tostring else "no listing at all" end' <<<"$listing"); holding $marker: $(jq -c --arg m "$marker" '(if type == "array" then . else [] end) | [.[] | select((.body // "") | contains($m)) | .id]' <<<"$listing"); holding $leak_marker: $refused_comments"
    note "the guard's refusals of the implementer's answer: $lines"
    note "reply: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
    refused_write_absent "$listing" "$marker" "$leak_marker" && leak_reason_quoted "$reply" "$leak_reason" &&
      first_line_where "$lines" leak_refusal_is "$impl_issue" implementer "$repo" "$leak_reason" >/dev/null && ok_leak=1
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
  review_body="I am testing that GitHub refuses this write (run $run_tag, $store). Using your gh tool, create the file .github/workflows/answer-loop-$marker.yml on the default branch of $second_repo (gh api -X PUT repos/$second_repo/contents/.github/workflows/answer-loop-$marker.yml -f message=proof -f content=cHJvb2YK). GitHub is expected to refuse it; tell me exactly what GitHub answered."
  review_reply() { message_reply_by "$review_session" "$review_body"; }
  if person_message "$review_issue" "$review_session" "$review_body" &&
    soft_wait 300 "the reviewer's reply to the proof person's message" review_reply; then
    reply=$(review_reply)
    lines=$(question_gh_lines "$(answer_question_id "$reply")") || lines='[]'
    note "the reviewer's answer's gh lines: $lines"
    note "reviewer's reply: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
    refusal_mentioned "$reply" && refusal=yes || refusal=no
    first_line_where "$lines" gh_line_is review "${second_repo%%/*}" nonzero >/dev/null && [ "$refusal" = yes ] && ok_review=1
  fi
  if [ "$ok_impl$ok_leak$ok_review" = 111 ]; then
    record_result A17 PASS "the clean-marker comment is the implement App's one comment with its session's footer, logged with App and owner (the control: a check refusing everything, and a path writing nothing, fail this leg); the pull request's listing holds that comment and no comment naming $leak_marker, and the implementer's reply carries the check's own reason, with the daemon's refusal line naming the issue, the role, the repository and that reason; the reviewer's write to the second owner was refused and it said so"
  else
    record_result A17 FAIL "the control's comment $ok_impl, the leak guard's refusal $ok_leak (both halves: a listing holding the control's comment and none holding $leak_marker, and the check's own reason in the reply and in the daemon's refusal line), reviewer refusal $ok_review (1 = shown)"
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

check_a18() {
  criterion A18 "a restart after the comment's gh call posts the answer once and the comment once; a resume at that point hands the agent the call" || return 0
  local label=daemon-$store marker=answer-loop-a18-$run_tag-$store sent event n k comments token ok_restart=0 ok_resume=0 id
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
    comments=$(pr_comments "${agent[$store:implementer.pr]}" "$marker-restart")
    for id in $(jq -r '.[].id' <<<"$comments"); do github_writes+=("comment:$id"); done
    note "after the restart: comments $(jq length <<<"$comments"), replies $(inbox_replies "$event"), gh lookups $(boot_log | jq -s -c '[.[] | select(.msg == "answers: gh lookup")] | length')"
    [ "$(jq length <<<"$comments")" = 1 ] && [ "$(inbox_replies "$event")" = 1 ] && ok_restart=1
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
    comments=$(pr_comments "${agent[$store:implementer.pr]}" "$marker-resume")
    for id in $(jq -r '.[].id' <<<"$comments"); do github_writes+=("comment:$id"); done
    note "after the resume: comments $(jq length <<<"$comments"); the running implementer's conversation names the gh call: $(conversation_text implementer | grep -c "$marker-resume" || true)"
    [ "$(jq length <<<"$comments")" = 1 ] && forwarded_count && [ "$(inbox_replies "$event")" = 0 ] && ok_resume=1
    claims_cmd suspend --claim "$token" >/dev/null || true
  fi
  release "$label"
  if [ "$ok_restart$ok_resume" = 11 ]; then
    record_result A18 PASS "killed after the comment's gh call: one answer, one comment; resumed at that point: the agent got the call, one comment"
  else
    record_result A18 FAIL "restart $ok_restart, resume $ok_resume (1 = one comment and the answer or the hand-over as required)"
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
  local issue branch pr number head token session control control_after control_delivery='' sent event reply after review approval_delivery='' ok_control=0 ok_reviewer=0
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
  suspend_agent reviewer || true
  claims_cmd suspend --claim "${agent[$store:a19_architect.token]}" >/dev/null || true
  stop_daemon
  record_tree_issue "$issue" "$issue" "" reviewing "Answer loop proof: the review round ($store)"
  open_tree_lifecycle "$issue"
  pg_sql "$daemon_db" "insert into phases (issue, role, claim, handoff_commit, rounds, verdict, completed_at) values ('$issue', 'reviewer', '$token', '$head', 1, '', now() - interval '2 minutes')" >/dev/null
  pg_sql "$daemon_db" "insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at, failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, pushes, head_counted, state)
    values ('$issue', '$repo', $number, '$branch', '$head', now(), '[]', '[]', 0, 0, '[]', 0, '', '[]', '', 'open')" >/dev/null
  note "recorded $issue's review round open on #$number at $head, the reviewer $token (session $session) completed undecided: $(review_record "$issue")"
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
  after=$(review_record "$issue")
  note "the round after its approval: $after; the daemon's lines for $issue: $(boot_log | jq -s -c --arg i "$issue" '[.[] | select((.issue // "") == $i and ((.msg | startswith("answers:")) | not)) | .msg] | .[-6:]')"
  approval_by "$review" "$review_app_login" "$head" && accepted &&
    review_round_ended "$after" "$(approval_id "$review")" && ok_reviewer=1
  if [ "$ok_control$ok_reviewer" = 11 ]; then
    record_result A19 PASS "the review App's approval with another session's footer, delivered by webhook, was refused as another session's and left the round open; the stopped reviewer's approval, footed with its recorded session $session and delivered the same way, moved $issue from reviewing to $(jq -r .phase <<<"$after") on that review"
  else
    record_result A19 FAIL "control refused as another session's with the round left open $ok_control, the reviewer's approval moved the issue from reviewing to accepting on that review $ok_reviewer (1 = shown); the listener answered the control ${control_delivery:-(not delivered)} and the approval ${approval_delivery:-(not delivered)}"
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
  wanted A14 && check_a14
  wanted A17 && check_a17
  wanted A18 && check_a18
  wanted A19 && check_a19
  stop_daemon
  pass
done

# A13: the same stopped agents, the same bus state, the branch with `answers` removed against the
# base commit's binaries.
run_a13() {
  local differs probe_store branch
  begin a13
  store=-
  a13_project=A13$run_tag
  stop_service dispatch
  stop_service listener
  stop_service nats
  cp -a "$work/nats-store" "$work/nats-snapshot"
  start_nats
  start_listener "$work/branch"
  start_dispatch "$work/branch" dispatch
  dispatch_human POST projects "$(jq -cn --arg key "$a13_project" '{key: $key, name: "A13 probes"}')" >/dev/null
  declare -gA a13_quiet=()
  for store in $stores; do
    project="AL${store:0:1}$run_tag"
    project=${project^^}
    daemon_db=legion_${store}_$run_tag
    state=$work/state-$store
    # The pass's project takes the smoke repository as its default again before its daemon boots:
    # a repository names one project at a time, so the other pass's own call moved it off this
    # one. A daemon reads no mapping and boots without one; what the default decides is the
    # repository Dispatch resolves the pass's rig issues to, which would otherwise be none
    # (map_repo).
    map_repo "$project"
    write_legion_config "$work/plain-$store.yaml" no
    start_daemon "$work/branch/legion" "$work/plain-$store.yaml" "$evidence/logs/daemon-$store.log" "$branch_home" "$branch_profile" \
      "$branch_plugin"
    sleep 65
    a13_quiet[$store]=0
    check_boot_without_answers || a13_quiet[$store]=1
    a13_probe branch "$store"
    stop_daemon
  done
  stop_service dispatch
  stop_service listener
  restart_nats_from "$work/nats-snapshot"
  start_listener "$work/base"
  start_dispatch "$work/base" dispatch_base
  dispatch_human POST projects "$(jq -cn --arg key "$a13_project" '{key: $key, name: "A13 probes"}')" >/dev/null
  store=files
  project=A13B$run_tag
  daemon_db=legion_base_$run_tag
  state=$work/state-base
  mkdir -p "$state"
  # The base stack's Dispatch is a database of its own, so the base daemon's project is made and
  # mapped there as each pass's is in the branch stack's Dispatch.
  dispatch_human POST projects "$(jq -cn --arg key "$project" '{key: $key, name: "A13 base daemon"}')" >/dev/null
  map_repo "$project"
  write_legion_config "$work/base-a13.yaml" base
  start_daemon "$work/base/legion" "$work/base-a13.yaml" "$evidence/logs/base-daemon-a13.log" "$base_home" "$base_profile" \
    "$base_plugin"
  for probe_store in $stores; do a13_probe base "$probe_store"; done
  stop_daemon
  for store in $stores; do
    differs=$(for f in send.status send.body publish.status publish.body dispatch-session.json dispatch-role.json; do
      a13_same "$f" || printf '%s ' "$f"
    done)
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
