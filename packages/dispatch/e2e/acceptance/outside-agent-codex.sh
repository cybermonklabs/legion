#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR
# The Codex leg of the outside agent's acceptance, rows C0 to C7: what the outside person's Codex
# CLI does through the three edge origins with the codex-envoy plugin, under the device login row
# P2 made. The public leg (outside-agent-public.sh) runs it after P10, with P2's HOME, so it runs
# where P2 ran, on a runner on the public internet; outside-agent-acceptance.md, beside this file,
# says what each row reads.
#
# It is written for bash 3.2 and later, as the public leg is, and sources outside-agent-lib.sh from
# beside it. It installs one exact Codex release, @openai/codex@0.171.0, with npm into its own
# scratch directory, checks npm's record of its integrity against the one this file pins, and runs
# that binary; installs the plugin as the outside person does, from a local marketplace whose
# plugin source is the npm release OUTSIDE_CODEX_ENVOY_VERSION names, into a CODEX_HOME of its own
# (`codex plugin marketplace add`, `codex plugin add`); reads that release's provenance and checks
# that the tree Codex installed is the attested tarball (C0); and runs the command the outside
# person is told to run, `codex --no-daemon -m gpt-5.5 "<prompt>"`, in a tmux pane, as the public
# leg runs Oh My Pi, with no hook-trust bypass. It reads each screen of the interactive start and
# answers the two first-run ones as he is told to: "Hooks need review" with "Trust all and
# continue", "Meet GPT-6 Sol" with "Use existing model"; a screen it does not expect holds the run,
# and the rows read UNKNOWN naming it. Codex runs against outside-agent-codex-model.ts beside this
# file, a keyless stand-in model that speaks the Responses API and plays one scripted turn per run.
# Codex 0.171.0 with gpt-6.2-sol runs no tool at all, which is why the model name is gpt-5.5: with
# it, 0.171.0 lists the plugin's tools behind `tool_search`, and the stand-in finds them there.
# `--no-daemon` keeps the session out of Codex's shared app server, where the plugin registers no
# session. Row C7 runs the installed plugin's own `dispatch` entry from a terminal, `bun <installed
# plugin root>/dist/dispatch.js`, the entry the outside person's install text logs in with; the
# root is the one `codex plugin add` names ($CODEX_HOME/plugins/cache/<marketplace>/codex-envoy/
# <version>).
#
# Every process it starts runs with `env -i`, the device login's HOME and these settings alone,
# the outside person's (section (c) of the public leg):
#   ENVOY_URL=<OUTSIDE_LISTENER_URL> ENVOY_INBOUND=listener DISPATCH_URL=<OUTSIDE_DISPATCH_URL>
#   DISPATCH_TOKEN_BROKER=1 AGENT_SECRETS_URL=<OUTSIDE_BROKER_URL>
#   AGENT_SECRETS_MACHINE_IDENTITY=device-login AGENT_SECRETS_PROJECTS=<OUTSIDE_PROJECT>
# beside PATH, HOME, TERM, TMPDIR and CODEX_HOME. No model credential reaches any of them. Calls
# that carry the agent's broker credential go through outside-agent-probe.ts beside this file.
#
# Inputs:
#   OUTSIDE_DISPATCH_URL, OUTSIDE_BROKER_URL, OUTSIDE_LISTENER_URL
#                             the three edge origins, as the public leg takes them.
#   OUTSIDE_INSIDE_DISPATCH_URL
#                             Dispatch's private origin, which C2's control points DISPATCH_URL at:
#                             the tool must not answer there as at the edge. No default; without it
#                             C2 reads UNKNOWN.
#   OUTSIDE_PROJECT, OUTSIDE_PERSON, OUTSIDE_PEER_SESSION
#                             the scratch project, the test person, our peer session (C4's control
#                             sends to <peer>-foreign, the session outside the project of row P6).
#   OUTSIDE_DEVICE_LOGIN_HOME the HOME whose ~/.config/legion/device-login.json is P2's login.
#   OUTSIDE_CODEX_ENVOY_VERSION
#                             the exact @sjawhar/codex-envoy release to install, as the outside
#                             person installs it: a local marketplace whose plugin source is
#                             {"source": "npm", "package": "@sjawhar/codex-envoy", "version": it}.
#                             No default and no range; without it every row reads UNKNOWN.
#   OUTSIDE_CODEX_ENVOY_COMMIT, OUTSIDE_DEVICE_LOGIN_COMMIT
#                             row C0: the commit the release was built from, which the package's
#                             provenance must name, and the merge commit of the device login,
#                             which that commit must hold. Without either C0 reads UNKNOWN.
# Optional: OUTSIDE_CODEX_TSV, a file that gets a copy of the TSV; OUTSIDE_LEGION_CHECKOUT, the
# git checkout C0 reads commits in and builds the release's commit from (default the one this file
# is in); OUTSIDE_CODEX_ENVOY_SOURCE=checkout, a development run that installs the plugin built
# from that checkout instead of the npm release (C0 then reads UNKNOWN, so such a run never passes
# the gate); OUTSIDE_CODEX_FIRST_START=no-trust or new-model, a control run that answers the first
# hooks screen "Continue without trusting" or the model screen "Try new model", which no row may
# pass on (C1, C4 and C5 with the hooks untrusted; C2 and C3 with the new model), its evidence
# naming the control; OUTSIDE_CODEX_LIST_SECONDS (default 30, as P4 waits for its session), how
# long Codex may take to record its session once its turn began, and the edge to list it;
# OUTSIDE_CODEX_START_SECONDS (default 90), how long an interactive start, its screens answered,
# may take before its turn begins; OUTSIDE_NPM_REGISTRY
# (default https://registry.npmjs.org), the registry C0 reads the release's record, tarball and
# provenance from; OUTSIDE_ALLOW_HTTP=1, which lets http://127.0.0.1 origins through for a local
# stack and this file's tests.
#
# Output: on stdout a TSV, the header `row want got verdict` and rows C0 to C7. Progress goes to
# stderr. A row reads PASS, FAIL or UNKNOWN: UNKNOWN when it could not run, its `got` naming what it
# could not read, never PASS. Each row's `got` ends with what ran: the Codex release, the model
# name, and the plugin's version, integrity and installed root. Exit 0 when every row passed, 1
# when any failed, 2 otherwise.
#
# Usage: OUTSIDE_DISPATCH_URL=https://dispatch.edge.example ... bash outside-agent-codex.sh

if [ -z "${BASH_VERSION:-}" ]; then
  echo 'UNKNOWN: run this with bash: bash outside-agent-codex.sh'
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

# The one Codex release this leg runs, and the model name it runs it with.
readonly codex_version=0.171.0
readonly codex_model=gpt-5.5
# The npm integrity of that release and of its Linux x64 binary package, as the registry names
# them: npm checks each tarball it installs against the integrity it records, and the leg checks
# that what it recorded is this, so the run's Codex is these bytes and no other.
readonly codex_integrity=sha512-1IOFzL2Xg86tLdw6kDo3GoKbkQENgQi7TI6rlEvN132R7ngSw7SNOIQoTcnKzSZZ8kf9N9GynCweSoaFbqk2Eg==
readonly codex_linux_x64_integrity=sha512-l66NZkQgdk2C3pbcfULDz42yEUB581MVA6I+Y43Wp4OIqEy73HCdmZ0GTjeAHY2ycuH4N00d9VsLrwT1jxshTw==
# The local marketplace's name, which names the plugin's data directory too.
readonly market=outside-e2e
# How long one run may take: the main run sleeps up to wait_calls times wait_seconds for the
# probe's message; the other two make one call each.
readonly wait_calls=60
readonly wait_seconds=2
readonly main_seconds=300
readonly short_seconds=150
list_seconds=${OUTSIDE_CODEX_LIST_SECONDS:-30}
case $list_seconds in '' | *[!0-9]*) list_seconds=30 ;; esac
# How long an interactive start may take, its screens answered, before its turn begins.
start_seconds=${OUTSIDE_CODEX_START_SECONDS:-90}
case $start_seconds in '' | *[!0-9]*) start_seconds=90 ;; esac
# C4: the message the probe sends is in a model request within this many seconds; C5: the
# probe reads the session's message within this many seconds of the model's send.
readonly inbound_bound=20
readonly outbound_bound=10
# C6's edge listener, refused: a port below 1024, which no process but root's can listen on, so no
# other run on the machine can take it while the run reads it as refused; the leg reads that it
# refuses a connection before the run, and after.
readonly refused_origin=http://127.0.0.1:1

tsv=${OUTSIDE_CODEX_TSV:-}
if [ -n "$tsv" ]; then
  : >"$tsv" || tsv=''
fi
emit() {
  printf '%s\n' "$1"
  [ -z "$tsv" ] || printf '%s\n' "$1" >>"$tsv"
}
fails=0
unknowns=0
# row NAME WANT GOT VERDICT: prints the row, its `got` ending with what ran. A verdict that is none
# of PASS, FAIL and UNKNOWN counts as UNKNOWN.
row() {
  emit "$1$tab$(cell "$2")$tab$(cell "$3; $ran")$tab$4"
  case $4 in
    PASS) ;;
    FAIL) fails=$((fails + 1)) ;;
    *) unknowns=$((unknowns + 1)) ;;
  esac
}
say() { printf 'outside-agent-codex: %s\n' "$1" >&2; }

emit "row${tab}want${tab}got${tab}verdict"

dispatch=${OUTSIDE_DISPATCH_URL:-}
broker=${OUTSIDE_BROKER_URL:-}
listener=${OUTSIDE_LISTENER_URL:-}
inside=${OUTSIDE_INSIDE_DISPATCH_URL:-}
project=${OUTSIDE_PROJECT:-}
person=${OUTSIDE_PERSON:-}
person_lower=$(printf '%s' "$person" | tr '[:upper:]' '[:lower:]')
peer=${OUTSIDE_PEER_SESSION:-}
login_home=${OUTSIDE_DEVICE_LOGIN_HOME:-}
checkout=${OUTSIDE_LEGION_CHECKOUT:-$here/../../../..}
# Where the plugin comes from: the npm release (the default, and the outside person's way), or a
# build of the checkout, a development run whose C0 reads UNKNOWN.
source=${OUTSIDE_CODEX_ENVOY_SOURCE:-npm}
envoy_version=${OUTSIDE_CODEX_ENVOY_VERSION:-}
envoy_commit=${OUTSIDE_CODEX_ENVOY_COMMIT:-}
login_commit=${OUTSIDE_DEVICE_LOGIN_COMMIT:-}
readonly envoy_package=@sjawhar/codex-envoy
# The workflow a release of the package must be built by, as its provenance names it.
readonly release_repository=https://github.com/cybermonklabs/legion
readonly release_workflow=.github/workflows/release.yaml
ran="ran: @openai/codex@$codex_version, -m $codex_model on the stand-in model"

# The answers to the interactive start's two first-run screens the outside person is told to give,
# and the control runs that answer otherwise (OUTSIDE_CODEX_FIRST_START): "no-trust" leaves the
# plugin's hooks untrusted, so no session record, registration or delivery comes (C1, C4, C5);
# "new-model" takes GPT-6 Sol, so the session lists no plugin tool (C2, C3).
first_start=${OUTSIDE_CODEX_FIRST_START:-op3}
hooks_choice='Trust all and continue'
model_choice='Use existing model'
control_note=''
case $first_start in
  no-trust) hooks_choice='Continue without trusting' ;;
  new-model) model_choice='Try new model' ;;
esac
[ "$first_start" = op3 ] ||
  control_note="; a control run (OUTSIDE_CODEX_FIRST_START=$first_start): '$hooks_choice' and '$model_choice'"
screens=''

# --- The scratch directory and its cleanup: every background child and every Codex's tmux server,
# stopped and waited for, then the directory. ---
work=''
background=''
tmux_sockets=''
# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  local sock
  reap
  for sock in $tmux_sockets; do
    tmux -S "$sock" kill-server 2>/dev/null </dev/null
  done
  [ -z "$work" ] || rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
nonce=$(od -An -N6 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n')
[ -n "$nonce" ] || nonce=$$
work=$(mktemp -d "${TMPDIR:-/tmp}/outside-agent-codex.XXXXXX") || work=''

want0="$envoy_package@${envoy_version:-<OUTSIDE_CODEX_ENVOY_VERSION>} has npm provenance: built by $release_repository's $release_workflow on refs/heads/main from ${envoy_commit:-<OUTSIDE_CODEX_ENVOY_COMMIT>}, which holds ${login_commit:-<OUTSIDE_DEVICE_LOGIN_COMMIT>} (the device login); its attestation is verified (npm audit signatures) and attests the registry's tarball, which is the tree Codex installed, file by file"
want1="the Codex session registers at ${listener:-<OUTSIDE_LISTENER_URL>} under its own session id, the Codex thread"
want2="dispatch whoami through the plugin's dispatch tool, in the Codex session, names that session, ${person_lower:-<OUTSIDE_PERSON>} and ${project:-<OUTSIDE_PROJECT>}; with DISPATCH_URL at ${inside:-<OUTSIDE_INSIDE_DISPATCH_URL>} the same call does not"
want3="a comment the Codex session writes through the dispatch tool on the scratch issue lands: the probe reads it, written by ${person_lower:-<OUTSIDE_PERSON>}'s agent as the Codex session, in ${project:-<OUTSIDE_PROJECT>}, with no scope"
want4="a message the probe sends the Codex session through the edge is in the model's next request (the plugin's PostToolUse hook) within $inbound_bound s; one sent to ${peer:-<OUTSIDE_PEER_SESSION>}-foreign, a session of another project, is answered 404 and never reaches it"
want5="the Codex session's envoy_send to the probe's session reaches the probe within $outbound_bound s, from the Codex session"
want6="with the edge listener refused (ENVOY_URL at $refused_origin, which refuses a connection), dispatch whoami through the tool still names ${person_lower:-<OUTSIDE_PERSON>} and ${project:-<OUTSIDE_PROJECT>}"
want7="the installed plugin's own entry from a terminal, bun <installed plugin root>/dist/dispatch.js whoami under P2's login, names ${person_lower:-<OUTSIDE_PERSON>} and ${project:-<OUTSIDE_PROJECT>}"
all_unknown() {
  row C0 "$want0" "$1" UNKNOWN
  row C1 "$want1" "$1" UNKNOWN
  row C2 "$want2" "$1" UNKNOWN
  row C3 "$want3" "$1" UNKNOWN
  row C4 "$want4" "$1" UNKNOWN
  row C5 "$want5" "$1" UNKNOWN
  row C6 "$want6" "$1" UNKNOWN
  row C7 "$want7" "$1" UNKNOWN
}
finish() {
  if [ "$fails" -gt 0 ]; then exit 1; fi
  if [ "$unknowns" -gt 0 ]; then exit 2; fi
  exit 0
}

problem=$(lacking OUTSIDE_DISPATCH_URL OUTSIDE_BROKER_URL OUTSIDE_LISTENER_URL OUTSIDE_PROJECT OUTSIDE_PERSON OUTSIDE_PEER_SESSION OUTSIDE_DEVICE_LOGIN_HOME)
[ -n "$problem" ] || [ -n "$work" ] || problem="mktemp -d made no scratch directory"
if [ -z "$problem" ] && [ ! -f "$login_home/.config/legion/device-login.json" ]; then
  problem="no device login at $login_home/.config/legion/device-login.json"
fi
[ -n "$problem" ] || problem=$(tool_problem bun curl tmux)
case $first_start in
  op3 | no-trust | new-model) ;;
  *) problem="${problem:+$problem; }OUTSIDE_CODEX_FIRST_START is $first_start, not op3, no-trust or new-model" ;;
esac
if [ -z "$problem" ]; then
  case $source in
    npm)
      # An exact version, MAJOR.MINOR.PATCH with a pre-release tag at most: never a range or a
      # dist-tag, which would let the registry choose what the run installs.
      case $envoy_version in
        '') problem="OUTSIDE_CODEX_ENVOY_VERSION is unset: it names the exact $envoy_package release to install" ;;
        *[!0-9A-Za-z.-]* | [!0-9]*) problem="OUTSIDE_CODEX_ENVOY_VERSION is no exact version: $envoy_version" ;;
        [0-9]*.[0-9]*.[0-9]*) ;;
        *) problem="OUTSIDE_CODEX_ENVOY_VERSION is no exact version: $envoy_version" ;;
      esac
      ;;
    checkout) ;;
    *) problem="OUTSIDE_CODEX_ENVOY_SOURCE is $source, not npm or checkout" ;;
  esac
fi
if [ -n "$problem" ]; then
  all_unknown "$problem"
  finish
fi

# --- Codex, pinned: one exact release from npm, its integrity and its binary package's checked
# against the leg's own record of them before it runs. ---
real_tools "$work/tools" bun npm node git tmux
tools_path="$work/tools:$PATH"
if [ -n "$(PATH=$tools_path tool_problem npm)" ]; then
  all_unknown "npm not on PATH, so @openai/codex@$codex_version is not installed"
  finish
fi
say "npm install @openai/codex@$codex_version"
env -i "PATH=$tools_path" "HOME=$work/npm-home" "TMPDIR=${TMPDIR:-/tmp}" npm install \
  --prefix "$work/codex" --no-save --no-audit --no-fund --ignore-scripts --loglevel=error \
  "@openai/codex@$codex_version" >"$work/npm.log" 2>&1 </dev/null
installed=$?
# What npm recorded of the two packages it checked the tarballs against (its hidden lockfile).
recorded=$(env -i "PATH=$tools_path" "HOME=$work/npm-home" bun -e '
  const lock = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8")).packages ?? {};
  console.log((lock["node_modules/@openai/codex"]?.integrity ?? "none") + " " +
    (lock["node_modules/@openai/codex-linux-x64"]?.integrity ?? "none"));
' "$work/codex/node_modules/.package-lock.json" 2>/dev/null </dev/null)
if [ "$recorded" != "$codex_integrity $codex_linux_x64_integrity" ]; then
  all_unknown "npm install @openai/codex@$codex_version exited $installed, and the integrity it recorded is not the pinned one (got ${recorded:-none}; want $codex_integrity $codex_linux_x64_integrity, the Linux x64 build): $(text_of "$work/npm.log")"
  finish
fi
codex_bin=''
# The native binary itself, not npm's node wrapper.
for each in "$work"/codex/node_modules/@openai/codex-linux-x64/vendor/*/bin/codex; do
  if [ -x "$each" ]; then
    codex_bin=$each
    break
  fi
done
if [ -z "$codex_bin" ]; then
  all_unknown "npm install @openai/codex@$codex_version left no native codex binary: $(text_of "$work/npm.log")"
  finish
fi
version=$(env -i "PATH=$tools_path" "HOME=$work/npm-home" "$codex_bin" --version 2>/dev/null </dev/null)
if [ "$version" != "codex-cli $codex_version" ]; then
  all_unknown "the Codex binary printed '$(cell "${version:0:80}")' for --version, not codex-cli $codex_version"
  finish
fi

# --- The local marketplace the plugin installs from: its source is the npm release (the outside
# person's install), or, for a development run, a build of the checkout. ---
codex_home="$work/codex-home"
market_dir="$work/market"
data_dir="$codex_home/plugins/data/codex-envoy-$market"
mkdir -p "$codex_home" "$market_dir/.agents/plugins" "$work/session"
session_dir=$(cd "$work/session" && pwd -P)
checkout_commit=$(git -C "$checkout" rev-parse HEAD 2>/dev/null </dev/null) || checkout_commit=''
if [ "$source" = checkout ]; then
  say "building the plugin from ${checkout_commit:-$checkout} (a development run)"
  build="$checkout/packages/claude-envoy/scripts/build.ts"
  if [ ! -f "$build" ]; then
    all_unknown "no plugin build at $build"
    finish
  fi
  env -i "PATH=$tools_path" "HOME=$work/npm-home" "TMPDIR=${TMPDIR:-/tmp}" \
    bun "$build" --codex-out "$market_dir/plugins/codex-envoy" >"$work/build.log" 2>&1 </dev/null
  built=$?
  if [ "$built" -ne 0 ]; then
    all_unknown "the plugin build exited $built: $(text_of "$work/build.log")"
    finish
  fi
  plugin_source='{"path":"./plugins/codex-envoy","source":"local"}'
else
  plugin_source="{\"package\":\"$envoy_package\",\"source\":\"npm\",\"version\":\"$envoy_version\"}"
fi
printf '%s\n' "{\"name\":\"$market\",\"plugins\":[{\"category\":\"Productivity\",\"name\":\"codex-envoy\",\"policy\":{\"authentication\":\"ON_INSTALL\",\"installation\":\"AVAILABLE\"},\"source\":$plugin_source}]}" \
  >"$market_dir/.agents/plugins/marketplace.json"
codex_env=(
  "PATH=$tools_path"
  "HOME=$login_home"
  "TERM=${TERM:-xterm-256color}"
  "TMPDIR=${TMPDIR:-/tmp}"
  "CODEX_HOME=$codex_home"
  "ENVOY_URL=$listener"
  "ENVOY_INBOUND=listener"
  "DISPATCH_URL=$dispatch"
  "DISPATCH_TOKEN_BROKER=1"
  "AGENT_SECRETS_URL=$broker"
  "AGENT_SECRETS_MACHINE_IDENTITY=device-login"
  "AGENT_SECRETS_PROJECTS=$project"
)

# probe COMMAND [ARGS...]: outside-agent-probe.ts as the session `probe_as` names (the leg's own
# by default), under the device login, its one JSON line.
probe_session="outside-e2e-codex-probe-$nonce"
probe_env=(
  "PATH=$tools_path"
  "HOME=$login_home"
  "TMPDIR=${TMPDIR:-/tmp}"
  "ENVOY_URL=$listener"
  "ENVOY_INBOUND=listener"
  "DISPATCH_URL=$dispatch"
  "DISPATCH_TOKEN_BROKER=1"
  "AGENT_SECRETS_URL=$broker"
  "AGENT_SECRETS_MACHINE_IDENTITY=device-login"
  "AGENT_SECRETS_PROJECTS=$project"
  "OUTSIDE_LISTENER_URL=$listener"
  "OUTSIDE_DISPATCH_URL=$dispatch"
)
probe() {
  env -i "${probe_env[@]}" OUTSIDE_PROBE_SESSION="${probe_as:-$probe_session}" \
    bun "$here/outside-agent-probe.ts" "$@" 2>>"$work/probe.log" </dev/null
}
# probe_exec COMMAND [ARGS...]: probe for a background child; its last word is exec, so `$!` of
# `probe_exec ... &` names outside-agent-probe.ts itself, which reap stops.
probe_exec() {
  exec env -i "${probe_env[@]}" OUTSIDE_PROBE_SESSION="${probe_as:-$probe_session}" \
    bun "$here/outside-agent-probe.ts" "$@" 2>>"$work/probe.log" </dev/null
}
unknown_of() { json_field unknown "$1"; }

# --- The scratch issue C3 comments on: the one the tailnet leg found or made. ---
title=$(outside_issue_title)
found=$(probe issue "$project" "$title")
issue=$(json_field key "$found")
[ "$issue" != null ] || issue=''
issue_problem=''
[ -n "$issue" ] || issue_problem="no issue of $project titled '$title' was read: ${found:0:200}"

# --- The stand-in model, and Codex's configuration that points at it. ---
comment_text="outside-e2e codex-comment $nonce"
inbound_text="outside-e2e codex-in $nonce"
control_text="outside-e2e codex-control $nonce"
outbound_text="outside-e2e codex-out $nonce"
receiver="outside-e2e-codex-$nonce"
printf '{"nonce":%s,"issue":%s,"comment":%s,"inbound":%s,"control":%s,"outbound":%s,"outboundTo":%s,"waitCalls":%s,"waitSeconds":%s}\n' \
  "$(json_string "$nonce")" "$(json_string "$issue")" "$(json_string "$comment_text")" \
  "$(json_string "$inbound_text")" "$(json_string "$control_text")" \
  "$(json_string "$outbound_text")" "$(json_string "$receiver")" "$wait_calls" "$wait_seconds" \
  >"$work/plan.json"
: >"$work/model.log"
env -i "PATH=$tools_path" "HOME=$work/npm-home" "TMPDIR=${TMPDIR:-/tmp}" \
  bun "$here/outside-agent-codex-model.ts" "$work/plan.json" "$work/model.log" \
  >"$work/model.out" 2>&1 </dev/null &
background="$background $!"
# The model binds a port of the kernel's choosing and prints it, and answers GET /_ready with this
# run's nonce: the port is used only once the server there answers that nonce, so a line that
# names another server (another run's stand-in, a port read before the line was whole) is never
# what Codex is pointed at.
model_port=''
ready_port=''
deadline=$(($(now) + 30))
while [ -z "$ready_port" ] && [ "$(now)" -lt "$deadline" ]; do
  model_port=$(json_field port "$(cat "$work/model.out")")
  case $model_port in
    '' | *[!0-9]*) ;;
    *)
      [ "$(curl -q -sf --max-time 2 "http://127.0.0.1:$model_port/_ready" 2>/dev/null </dev/null)" != "$nonce" ] ||
        ready_port=$model_port
      ;;
  esac
  [ -n "$ready_port" ] || sleep 1
done
if [ -z "$ready_port" ]; then
  all_unknown "the stand-in model did not answer this run's nonce at the port it printed (${model_port:-none}) within 30 s: $(text_of "$work/model.out")"
  finish
fi
model_port=$ready_port
printf '%s\n' "model = \"$codex_model\"" "model_provider = \"$market\"" 'approval_policy = "never"' \
  'sandbox_mode = "danger-full-access"' "[model_providers.$market]" \
  'name = "outside-e2e stand-in"' "base_url = \"http://127.0.0.1:$model_port/v1\"" \
  'wire_api = "responses"' 'requires_openai_auth = false' "[projects.\"$session_dir\"]" \
  'trust_level = "trusted"' >"$codex_home/config.toml"

# --- The plugin's install, from the local marketplace, after the configuration: `codex plugin
# marketplace add` and `codex plugin add` write their own tables into config.toml
# (`[marketplaces.<name>]`, `[plugins."codex-envoy@<name>"] enabled = true`), and a configuration
# written after them would drop the plugin. ---
# codex_install WORDS...: one `codex plugin` command of the install, or all rows UNKNOWN.
codex_install() {
  local added
  env -i "${codex_env[@]}" "$codex_bin" "$@" >>"$work/install.log" 2>&1 </dev/null
  added=$?
  [ "$added" -eq 0 ] && return 0
  all_unknown "codex $* exited $added: $(text_of "$work/install.log")"
  finish
}
codex_install plugin marketplace add "$market_dir"
codex_install plugin add "codex-envoy@$market"
# The installed plugin's root, as `codex plugin add` names it ("Installed plugin root: <dir>",
# $CODEX_HOME/plugins/cache/<marketplace>/codex-envoy/<version> on 0.171.0): what Codex runs, and
# where the outside person's terminal entry is.
plugin_root=''
while IFS= read -r line; do
  case $line in *'Installed plugin root: '*) plugin_root=${line##*Installed plugin root: } ;; esac
done <"$work/install.log"
plugin_version=''
[ ! -f "$plugin_root/package.json" ] ||
  plugin_version=$(json_field version "$(tr -d ' \n' <"$plugin_root/package.json")")
if [ -z "$plugin_version" ]; then
  all_unknown "codex plugin add named no installed plugin root with a package.json (root: ${plugin_root:-none}): $(text_of "$work/install.log")"
  finish
fi
if [ "$source" = npm ] && [ "$plugin_version" != "$envoy_version" ]; then
  all_unknown "Codex installed $envoy_package $plugin_version at $plugin_root, not $envoy_version"
  finish
fi
# The release's record at the registry, read once: its integrity for every row's evidence, and its
# provenance for C0. A development run reads none.
release=''
integrity='not read'
if [ "$source" = npm ]; then
  release=$(env -i "PATH=$tools_path" "HOME=$work/npm-home" "TMPDIR=${TMPDIR:-/tmp}" \
    ${OUTSIDE_NPM_REGISTRY:+"OUTSIDE_NPM_REGISTRY=$OUTSIDE_NPM_REGISTRY"} \
    bun "$here/outside-agent-codex-provenance.ts" attestation "$envoy_package" "$envoy_version" "$work/release.tgz" \
    2>>"$work/provenance.log" </dev/null)
  integrity=$(json_field integrity "$release") || integrity='not read'
  ran="ran: $version (@openai/codex@$codex_version), -m $codex_model on the stand-in model, $envoy_package@$plugin_version from npm (integrity $integrity), installed at $plugin_root"
else
  ran="ran: $version (@openai/codex@$codex_version), -m $codex_model on the stand-in model, codex-envoy $plugin_version built from the checkout at ${checkout_commit:-a commit not read} (OUTSIDE_CODEX_ENVOY_SOURCE=checkout, a development run), installed at $plugin_root"
fi

# --- C7's reading: the installed plugin's own `dispatch` entry, run from a terminal under P2's
# login with the outside person's settings, as the install text logs in with it. A terminal names
# its host and its own session, as the command's refusal tells a person to (DISPATCH_HOST, and the
# session: CODEX_THREAD_ID for the host codex). ---
entry="$plugin_root/dist/dispatch.js"
terminal_session="outside-e2e-codex-terminal-$nonce"
terminal_out=''
terminal_exit=''
if [ -f "$entry" ]; then
  terminal_out=$(cd "$session_dir" && env -i "${codex_env[@]}" DISPATCH_HOST=codex \
    "CODEX_THREAD_ID=$terminal_session" bun "$entry" whoami 2>&1 </dev/null)
  terminal_exit=$?
fi

# --- C0's reading: the release's provenance, and that what Codex installed is the attested tarball.
# A release of the package is published by the repository's release workflow on main with `npm
# publish --provenance`, from the release run's commit; a version published by hand has no
# provenance. C0 reads, from the registry, that the release's SLSA provenance names that workflow,
# refs/heads/main and OUTSIDE_CODEX_ENVOY_COMMIT, and attests the registry's tarball; that the
# tarball the registry serves is the bytes its integrity names, and that every file of the tree
# Codex installed is that tarball's; that npm verifies the attestation (`npm audit signatures`, in
# a project of its own holding that release alone); and that the commit holds
# OUTSIDE_DEVICE_LOGIN_COMMIT. It records, and does not judge by, how a build of that commit's
# bundles compare with the installed ones: the release sets the version before it builds, which a
# bundle can hold. ---
c0_wrong=''
c0_unread=''
c0_read=''
c0_built='not built'
# commit_problem NAME VALUE: why VALUE, the input NAME, is no whole commit id, or nothing.
commit_problem() {
  case $2 in
    '') printf '%s is unset' "$1" ;;
    *[!0-9a-f]*) printf '%s is no 40-character commit id: %s' "$1" "$2" ;;
    ????????????????????????????????????????) ;;
    *) printf '%s is no 40-character commit id: %s' "$1" "$2" ;;
  esac
}
# git_has COMMIT: whether the checkout holds COMMIT, fetching it from its origin once if not.
git_has() {
  git -C "$checkout" cat-file -e "$1^{commit}" 2>/dev/null </dev/null && return 0
  git -C "$checkout" fetch --quiet --no-tags origin "$1" >/dev/null 2>&1 </dev/null
  git -C "$checkout" cat-file -e "$1^{commit}" 2>/dev/null </dev/null
}
if [ "$source" = npm ]; then
  c0_unread=$(commit_problem OUTSIDE_CODEX_ENVOY_COMMIT "$envoy_commit")
  problem=$(commit_problem OUTSIDE_DEVICE_LOGIN_COMMIT "$login_commit")
  [ -z "$problem" ] || c0_unread="${c0_unread:+$c0_unread; }$problem"
  # The registry's record and its provenance.
  if [ -n "$(json_field unknown "$release")" ] || [ -z "$(json_field status "$release")" ]; then
    c0_unread="${c0_unread:+$c0_unread; }the registry's record of $envoy_package@$envoy_version: ${release:0:200}"
  elif [ "$(json_field provenance "$release")" != true ]; then
    c0_wrong="$envoy_package@$envoy_version has no provenance attestation: it was not published by a workflow with npm publish --provenance"
  else
    attested_repository=$(json_field repository "$release")
    attested_workflow=$(json_field workflow "$release")
    attested_commit=$(json_field commit "$release")
    attested_ref=$(json_field ref "$release")
    c0_read="provenance: $attested_repository $attested_workflow at $attested_ref, commit $attested_commit"
    [ "$attested_repository" = "$release_repository" ] && [ "$attested_workflow" = "$release_workflow" ] ||
      c0_wrong="the provenance names $attested_repository's $attested_workflow, not $release_repository's $release_workflow"
    # The release workflow publishes from main alone; a run of it on another branch names that ref.
    [ "$attested_ref" = refs/heads/main ] ||
      c0_wrong="${c0_wrong:+$c0_wrong; }the provenance names the ref $attested_ref, not refs/heads/main"
    [ -n "$envoy_commit" ] && [ "$attested_commit" != "$envoy_commit" ] &&
      c0_wrong="${c0_wrong:+$c0_wrong; }the provenance names commit $attested_commit, not OUTSIDE_CODEX_ENVOY_COMMIT $envoy_commit"
    [ "$(json_field subject_matches "$release")" = true ] ||
      c0_wrong="${c0_wrong:+$c0_wrong; }the provenance attests another tarball than the registry's ($integrity)"
  fi
  # The tree Codex installed is the tarball whose integrity the registry and the attestation name.
  if [ -n "$(json_field status "$release")" ] && [ -z "$(json_field unknown "$release")" ]; then
    if [ "$(json_field tarball_matches "$release")" != true ]; then
      c0_wrong="${c0_wrong:+$c0_wrong; }the tarball the registry serves is not the bytes its integrity ($integrity) names"
    elif ! { mkdir -p "$work/tarball" && tar -xzf "$work/release.tgz" -C "$work/tarball" 2>/dev/null </dev/null &&
      [ -d "$work/tarball/package" ]; }; then
      c0_unread="${c0_unread:+$c0_unread; }the release's tarball did not extract to a package directory"
    else
      tree=$(env -i "PATH=$tools_path" "HOME=$work/npm-home" bun \
        "$here/outside-agent-codex-provenance.ts" tree "$work/tarball/package" "$plugin_root" 2>/dev/null </dev/null)
      if [ -n "$(json_field unknown "$tree")" ] || [ -z "$(json_field equal "$tree")" ]; then
        c0_unread="${c0_unread:+$c0_unread; }the installed tree was not compared with the tarball: ${tree:0:200}"
      elif [ -n "$(json_field differ "$tree")$(json_field only_tarball "$tree")$(json_field only_installed "$tree")" ]; then
        c0_wrong="${c0_wrong:+$c0_wrong; }the tree Codex installed is not the tarball: differ: $(json_field differ "$tree"); only in the tarball: $(json_field only_tarball "$tree"); only installed: $(json_field only_installed "$tree")"
      else
        c0_read="${c0_read:+$c0_read; }the tree Codex installed is the tarball, file by file ($(json_field equal "$tree") files)"
      fi
    fi
  fi
  # npm's own verification of the release's registry signature and attestation.
  mkdir -p "$work/audit"
  printf '%s\n' '{"name":"outside-e2e-audit","version":"0.0.0","private":true}' >"$work/audit/package.json"
  (cd "$work/audit" && env -i "PATH=$tools_path" "HOME=$work/npm-home" "TMPDIR=${TMPDIR:-/tmp}" \
    npm install --no-audit --no-fund --ignore-scripts --loglevel=error \
    "$envoy_package@$envoy_version" >"$work/audit.log" 2>&1 </dev/null)
  audit_installed=$?
  audit=$(cd "$work/audit" && env -i "PATH=$tools_path" "HOME=$work/npm-home" \
    "TMPDIR=${TMPDIR:-/tmp}" npm audit signatures 2>&1 </dev/null)
  audited=$?
  if [ "$audit_installed" -ne 0 ]; then
    c0_unread="${c0_unread:+$c0_unread; }npm install $envoy_package@$envoy_version for npm audit signatures exited $audit_installed: $(text_of "$work/audit.log")"
  elif [ "$audited" -eq 1 ]; then
    c0_wrong="${c0_wrong:+$c0_wrong; }npm audit signatures found an invalid signature or attestation: $(cell "${audit:0:200}")"
  elif [ "$audited" -ne 0 ]; then
    c0_unread="${c0_unread:+$c0_unread; }npm audit signatures exited $audited: $(cell "${audit:0:200}")"
  else
    case $audit in
      *'audited 1 package'*'1 package has a verified attestation'*)
        c0_read="${c0_read:+$c0_read; }npm audit signatures: the attestation is verified"
        ;;
      *'audited 1 package'*) c0_wrong="${c0_wrong:+$c0_wrong; }npm audit signatures verified no attestation of $envoy_package@$envoy_version" ;;
      *) c0_unread="${c0_unread:+$c0_unread; }npm audit signatures did not audit $envoy_package alone: $(cell "${audit:0:200}")" ;;
    esac
  fi
  # The commit holds the device login.
  if [ -z "$(commit_problem x "$envoy_commit")$(commit_problem x "$login_commit")" ]; then
    if [ "$(git -C "$checkout" rev-parse --is-shallow-repository 2>/dev/null </dev/null)" = true ]; then
      c0_unread="${c0_unread:+$c0_unread; }$checkout is a shallow clone, so whether $envoy_commit holds $login_commit is not read: check it out with its whole history"
    elif ! git_has "$envoy_commit" || ! git_has "$login_commit"; then
      c0_unread="${c0_unread:+$c0_unread; }$checkout holds $envoy_commit or $login_commit not, nor fetched it from its origin"
    else
      git -C "$checkout" merge-base --is-ancestor "$login_commit" "$envoy_commit" 2>/dev/null </dev/null
      case $? in
        0) c0_read="${c0_read:+$c0_read; }$envoy_commit holds the device login $login_commit" ;;
        1) c0_wrong="${c0_wrong:+$c0_wrong; }$envoy_commit does not hold the device login $login_commit" ;;
        *) c0_unread="${c0_unread:+$c0_unread; }git merge-base could not read whether $envoy_commit holds $login_commit" ;;
      esac
      # The recorded field: a build of the release's commit, as the release builds it, against
      # the installed bundles.
      mkdir -p "$work/release-src"
      if git -C "$checkout" archive --format=tar "$envoy_commit" 2>/dev/null </dev/null |
        tar -x -C "$work/release-src" 2>/dev/null &&
        (cd "$work/release-src" && env -i "PATH=$tools_path" "HOME=$work/npm-home" \
          "TMPDIR=${TMPDIR:-/tmp}" bun install --frozen-lockfile >"$work/release-install.log" 2>&1 </dev/null) &&
        (cd "$work/release-src/packages/claude-envoy" && env -i "PATH=$tools_path" \
          "HOME=$work/npm-home" "TMPDIR=${TMPDIR:-/tmp}" bun scripts/build.ts --codex-out \
          "$work/release-build" >"$work/release-build.log" 2>&1 </dev/null); then
        compared=$(env -i "PATH=$tools_path" "HOME=$work/npm-home" bun \
          "$here/outside-agent-codex-provenance.ts" compare "$work/release-build" "$plugin_root" 2>/dev/null </dev/null)
        c0_built="a build of $envoy_commit against the installed bundles: $(json_field equal "$compared") equal, differ: $(json_field differ "$compared"), only in the build: $(json_field only_built "$compared"), only installed: $(json_field only_installed "$compared")"
      else
        c0_built="no build of $envoy_commit: $(text_of "$work/release-build.log") $(text_of "$work/release-install.log")"
      fi
    fi
  fi
fi

# --- The runs: `codex --no-daemon -m gpt-5.5 "<prompt>"`, the command the outside person is told to
# run, in a tmux pane of its own, as the public leg runs Oh My Pi; never `codex exec`, which shows
# none of the interactive start's screens and can bypass the hook trust. ---
# codex_start RUN [NAME=VALUE...]: starts RUN's Codex in a tmux server of its own, with codex_env
# and then the settings given, which replace its own; its prompt names RUN for the stand-in model.
codex_start() {
  local run=$1
  shift
  tmux_sockets="${tmux_sockets:+$tmux_sockets }$work/tmux-$run.sock"
  known_records=$(ls "$data_dir/codex-sessions"/*.json 2>/dev/null)
  env -i "${codex_env[@]}" "$@" tmux -S "$work/tmux-$run.sock" -f /dev/null new-session -d \
    -s codex -x 200 -y 50 -c "$session_dir" "$codex_bin" --no-daemon -m "$codex_model" \
    "outside-e2e codex-run $run $nonce" >>"$work/tmux.log" 2>&1 </dev/null
}
# codex_pane RUN: what RUN's pane shows now.
codex_pane() { tmux -S "$work/tmux-$1.sock" capture-pane -p -J -t codex 2>/dev/null </dev/null; }
# codex_alive RUN: whether RUN's Codex still runs.
codex_alive() { tmux -S "$work/tmux-$1.sock" has-session -t codex 2>/dev/null </dev/null; }
# codex_stop RUN: stops RUN's tmux server and waits up to 10 s for its Codex to exit.
codex_stop() {
  local sock="$work/tmux-$1.sock" pane _
  pane=$(tmux -S "$sock" display-message -p -t codex '#{pane_pid}' 2>/dev/null </dev/null)
  tmux -S "$sock" kill-server 2>/dev/null </dev/null
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    { [ -n "$pane" ] && kill -0 "$pane" 2>/dev/null; } || break
    sleep 1
  done
}
# screen_text TEXT: TEXT's lines that are not blank, joined, at most 300 characters.
screen_text() {
  local line out=''
  while IFS= read -r line; do
    case $line in *[![:space:]]*) out="${out:+$out / }$line" ;; esac
  done <<EOF
$1
EOF
  out=$(cell "$out")
  printf '%s' "${out:0:300}"
}
# choose RUN LABEL: on the menu RUN's pane shows, moves to the entry that reads LABEL and takes it.
# The entry the menu has chosen is the numbered line holding `›`. False when no entry of LABEL is
# reached.
choose() {
  local tries=0 line selected
  while [ "$tries" -lt 10 ]; do
    tries=$((tries + 1))
    selected=''
    while IFS= read -r line; do
      case $line in *'›'*[0-9].' '*) selected=$line ;; esac
    done <<EOF
$(codex_pane "$1")
EOF
    case $selected in *"$2"*)
      tmux -S "$work/tmux-$1.sock" send-keys -t codex Enter 2>/dev/null </dev/null
      return 0
      ;;
    esac
    tmux -S "$work/tmux-$1.sock" send-keys -t codex Down 2>/dev/null </dev/null
    sleep 1
  done
  return 1
}
# drive RUN SECONDS: waits up to SECONDS for RUN's turn to begin (the stand-in model logs the tools
# of its first request), answering the interactive start's screens as the outside person is told
# to: "Hooks need review" with hooks_choice, "Meet GPT-6 Sol" with model_choice. Adds each screen
# and its answer to `screens`. False, with `drive_problem` naming the screen, when another screen
# holds the start, a screen lacks its entry, or Codex ends first.
drive() {
  local run=$1 deadline screen='' title choice
  drive_problem=''
  deadline=$(($(now) + $2))
  while [ "$(now)" -lt "$deadline" ]; do
    [ -z "$(logged "$run" tools)" ] || return 0
    if ! codex_alive "$run"; then
      drive_problem="Codex ended before its turn began; its last screen: $(screen_text "$screen")"
      return 1
    fi
    screen=$(codex_pane "$run")
    title=''
    case $screen in
      *'Hooks need review'*) title='Hooks need review' choice=$hooks_choice ;;
      *'Meet GPT-6 Sol'*) title='Meet GPT-6 Sol' choice=$model_choice ;;
    esac
    if [ -n "$title" ]; then
      if ! choose "$run" "$choice"; then
        drive_problem="the screen '$title' offered no '$choice': $(screen_text "$screen")"
        return 1
      fi
      screens="${screens:+$screens; }$run: $title -> $choice"
      # The screen goes once the choice is taken; it is not answered twice.
      for _ in 1 2 3 4 5 6 7 8 9 10; do
        case $(codex_pane "$run") in *"$title"*) sleep 1 ;; *) break ;; esac
      done
    fi
    sleep 1
  done
  drive_problem="Codex's turn did not begin within $2 s, held by a screen this leg does not expect: $(screen_text "$screen")"
  return 1
}
# session_of SECONDS: sets `session_found` to the session id of the record the plugin's
# SessionStart hook files for the Codex started last (one not there before it started), waited
# for up to SECONDS; empty when none came.
session_of() {
  local deadline each
  session_found=''
  deadline=$(($(now) + $1))
  while [ -z "$session_found" ] && [ "$(now)" -lt "$deadline" ]; do
    for each in "$data_dir/codex-sessions"/*.json; do
      [ -f "$each" ] || continue
      case $newline$known_records$newline in *"$newline$each$newline"*) continue ;; esac
      session_found=$(json_field session_id "$(tr -d '\n' <"$each")")
      [ -z "$session_found" ] || break
    done
    [ -n "$session_found" ] || sleep 1
  done
}
# logged RUN STEP: the stand-in model's log line of RUN's STEP, or nothing.
logged() {
  local each
  while IFS= read -r each; do
    case $each in *"\"run\":\"$1\",\"step\":\"$2\""*)
      printf '%s' "$each"
      return 0
      ;;
    esac
  done <"$work/model.log"
}
# finish_run RUN SECONDS: waits up to SECONDS for RUN's turn to end (the stand-in model logs its
# end), then stops its Codex, which the interactive start leaves open; sets `run_end`.
finish_run() {
  local deadline
  deadline=$(($(now) + $2))
  while [ -z "$(logged "$1" end)" ] && codex_alive "$1" && [ "$(now)" -lt "$deadline" ]; do sleep 1; done
  if [ -n "$(logged "$1" end)" ]; then
    run_end="the turn of $1 ended"
  elif codex_alive "$1"; then
    run_end="the turn of $1 had not ended within $2 s"
  else
    run_end="Codex $1 ended before its turn did"
  fi
  codex_stop "$1"
}

# --- The main run: C1 to C5. The probe's receiver listens first, as the session C5 sends to. ---
probe_as=$receiver probe_exec receive "$main_seconds" "$outbound_text" >"$work/receive.out" &
receive_pid=$!
background="$background $receive_pid"
receiver_ready=''
deadline=$(($(now) + 30))
while [ -z "$receiver_ready" ] && [ "$(now)" -lt "$deadline" ] && running_child "$receive_pid"; do
  case $(cat "$work/receive.out") in *'"ready":true'*) receiver_ready=yes ;; *) sleep 1 ;; esac
done
receiver_problem=''
[ -n "$receiver_ready" ] || receiver_problem="the probe's receiver did not start: $(text_of "$work/receive.out")"

say "codex --no-daemon main in tmux (codex-envoy@$market, CODEX_HOME $codex_home)"
codex_start main
started=$(now)
main_problem=''
drive main "$start_seconds" || main_problem=$drive_problem
session=''
# The SessionStart hook writes the record before the turn's first request, so once the turn began
# it is there or never comes (hooks that did not run).
[ -n "$main_problem" ] || { session_of "$list_seconds"; session=$session_found; }
listed=''
list_answer=''
list_read=''
if [ -n "$session" ]; then
  deadline=$(($(now) + list_seconds))
  while [ -z "$listed" ] && [ "$(now)" -lt "$deadline" ] && codex_alive main; do
    list_answer=$(probe sessions)
    if [ -z "$(unknown_of "$list_answer")" ] && [ "$(json_field status "$list_answer")" = 200 ]; then
      list_read=yes
      case $list_answer in *"\"$session\""*) listed=yes ;; esac
    fi
    [ -n "$listed" ] || sleep 2
  done
fi
control_answer=''
inbound_answer=''
inbound_sent_at=''
listing_ran_out=''
if [ -n "$listed" ]; then
  control_answer=$(probe send "$peer-foreign" "$control_text")
  inbound_sent_at=$(now)
  inbound_answer=$(probe send "$session" "$inbound_text")
  finish_run main $((main_seconds - ($(now) - started)))
  main_end=$run_end
elif [ -z "$main_problem" ] && codex_alive main; then
  # Running with no session to send to: never listed, or no record filed. No message can
  # reach the session, so its turn, which sleeps until one does, is stopped, not waited out.
  [ -z "$session" ] || listing_ran_out=yes
  codex_stop main
  if [ -n "$session" ]; then
    main_end="Codex main was stopped once its session had not been listed in $list_seconds s"
  else
    main_end="Codex main was stopped once it had filed no session record in $list_seconds s"
  fi
else
  codex_stop main
  main_end=${main_problem:-Codex main ended}
fi
if [ -n "$receiver_ready" ]; then
  await_child "$receive_pid" 15 1
  [ -n "$child_exit" ] || stop_child "$receive_pid"
fi

# --- C6: the same whoami with the edge listener refused; C2's control: at the inside origin. ---
# refuses ORIGIN: whether a connection to ORIGIN is refused (curl's exit 7), not answered or lost.
refuses() {
  curl -q -s -o /dev/null --max-time 5 "$1/" </dev/null
  [ "$?" -eq 7 ]
}
refused_session=''
refused_before=''
refused_after=''
refused_end="not run: $refused_origin did not refuse a connection before the run"
if refuses "$refused_origin"; then
  refused_before=yes
  say "codex --no-daemon edge-refused (ENVOY_URL $refused_origin)"
  codex_start edge-refused "ENVOY_URL=$refused_origin"
  if drive edge-refused "$start_seconds"; then
    session_of "$list_seconds"
    refused_session=$session_found
    finish_run edge-refused "$short_seconds"
    refused_end=$run_end
  else
    codex_stop edge-refused
    refused_end=$drive_problem
  fi
  if refuses "$refused_origin"; then refused_after=yes; fi
fi
inside_end=''
if [ -n "$inside" ]; then
  problem=$(lacking OUTSIDE_INSIDE_DISPATCH_URL)
  if [ -n "$problem" ]; then
    inside_end=$problem
  else
    say "codex --no-daemon inside (DISPATCH_URL $inside)"
    codex_start inside "DISPATCH_URL=$inside"
    if drive inside "$start_seconds"; then
      finish_run inside "$short_seconds"
      inside_end=$run_end
    else
      codex_stop inside
      inside_end=$drive_problem
    fi
  fi
fi
ran="$ran; first-start screens: ${screens:-none}${control_note}"

# --- The rows. ---
tools_line=$(logged main tools)
tools_note=''
[ -z "$tools_line" ] || [ -n "$(json_field send "$tools_line")" ] ||
  tools_note="; Codex listed no envoy_send to the model"
[ -z "$tools_line" ] || [ -n "$(json_field dispatch "$tools_line")" ] ||
  tools_note="; Codex listed no dispatch tool to the model"

# C0: the release's provenance; the build comparison is recorded beside it, never judged.
if [ "$source" = checkout ]; then
  row C0 "$want0" "the plugin was built from the checkout (OUTSIDE_CODEX_ENVOY_SOURCE=checkout), a development run: no release's provenance is read" UNKNOWN
elif [ -n "$c0_wrong" ]; then
  row C0 "$want0" "$c0_wrong${c0_unread:+; $c0_unread}${c0_read:+; read: $c0_read}; recorded: $c0_built" FAIL
elif [ -n "$c0_unread" ]; then
  row C0 "$want0" "$c0_unread${c0_read:+; read: $c0_read}; recorded: $c0_built" UNKNOWN
else
  row C0 "$want0" "$c0_read; recorded: $c0_built" PASS
fi

# C1: the session on the edge's session list.
if [ -z "$session" ]; then
  row C1 "$want1" "Codex filed no session record within $list_seconds s of its turn's start ($main_end)$tools_note" UNKNOWN
elif [ -n "$listed" ]; then
  row C1 "$want1" "session $session listed at $listener" PASS
elif [ -z "$list_read" ]; then
  row C1 "$want1" "session $session: the session list was not read: ${list_answer:0:200}" UNKNOWN
elif [ -n "$listing_ran_out" ]; then
  row C1 "$want1" "session $session was not on $listener's session list $list_seconds s after Codex recorded it, while Codex ran" FAIL
else
  row C1 "$want1" "session $session was not listed before $main_end" UNKNOWN
fi

# whoami_reading LINE SESSION: why LINE, a whoami log line, does not name SESSION, the person and
# the project; nothing when it does.
whoami_reading() {
  local owner project_read session_read elsewhere wrong=''
  owner=$(json_field owner "$1")
  project_read=$(json_field project "$1")
  session_read=$(json_field session "$1")
  elsewhere=$(json_field elsewhere "$1")
  [ "$owner" = "$person_lower" ] || wrong="owner ${owner:-none}"
  [ "$project_read" = "$project" ] || wrong="${wrong:+$wrong, }project ${project_read:-none}"
  [ -z "$2" ] || [ "$session_read" = "$2" ] || wrong="${wrong:+$wrong, }session ${session_read:-none}, not $2"
  [ -z "$elsewhere" ] || wrong="${wrong:+$wrong, }the credential names session $elsewhere"
  printf '%s' "$wrong"
}

# C2: whoami through the tool, held to its control at the inside origin.
whoami=$(logged main whoami)
main_whoami=''
if [ -z "$session" ]; then
  row C2 "$want2" "not run: Codex filed no session record ($main_end)" UNKNOWN
elif [ -z "$whoami" ]; then
  row C2 "$want2" "the model's dispatch whoami call got no output ($main_end)$tools_note" UNKNOWN
else
  wrong=$(whoami_reading "$whoami" "$session")
  whoami_exit=$(json_field exit "$whoami")
  if [ "$whoami_exit" != 0 ]; then
    # The plugin's own configuration refusal: the installed release cannot use the device login
    # with the outside person's settings. Anything else is no answer read.
    case $(json_field detail "$whoami") in
      *'dispatch config: '*) row C2 "$want2" "the plugin's dispatch refused its configuration (exit $whoami_exit): $(json_field detail "$whoami")" FAIL ;;
      *) row C2 "$want2" "dispatch whoami through the tool exited $whoami_exit: $(json_field detail "$whoami")" UNKNOWN ;;
    esac
  elif [ -n "$wrong" ]; then
    row C2 "$want2" "dispatch whoami through the tool named $wrong" FAIL
  else
    main_whoami=yes
    reading="session $session, owner $person_lower, project $project"
    inside_line=$(logged inside whoami)
    if [ -z "$inside" ]; then
      row C2 "$want2" "$reading; OUTSIDE_INSIDE_DISPATCH_URL is unset, so the control at the inside origin did not run" UNKNOWN
    elif [ -z "$inside_line" ]; then
      row C2 "$want2" "$reading; the control's whoami at $inside got no output (${inside_end:-not run})" UNKNOWN
    elif [ "$(json_field exit "$inside_line")" = 0 ] && [ -z "$(whoami_reading "$inside_line" '')" ]; then
      row C2 "$want2" "$reading; at $inside the tool answered the same, so this reading does not tell the edge from the inside origin" UNKNOWN
    else
      row C2 "$want2" "$reading; at $inside it exited $(json_field exit "$inside_line"), owner $(json_field owner "$inside_line")" PASS
    fi
  fi
fi

# C3: the comment through the tool, read back by the probe.
comment=$(logged main comment)
if [ -n "$issue_problem" ]; then
  row C3 "$want3" "$issue_problem" UNKNOWN
elif [ -z "$comment" ]; then
  row C3 "$want3" "the model's dispatch comment call on $issue got no output ($main_end)$tools_note" UNKNOWN
elif [ "$(json_field exit "$comment")" = 1 ]; then
  row C3 "$want3" "Dispatch refused dispatch comment on $issue through the tool (exit 1)" FAIL
elif [ "$(json_field exit "$comment")" != 0 ]; then
  row C3 "$want3" "dispatch comment on $issue through the tool exited $(json_field exit "$comment")" UNKNOWN
else
  read_back=$(probe comment-find "$issue" "$comment_text")
  kind=$(json_field author_kind "$read_back")
  owner=$(json_field owner "$read_back")
  written_in=$(json_field project "$read_back")
  written_as=$(json_field session "$read_back")
  scopes=$(json_field scope_count "$read_back")
  wrong=''
  [ "$kind" = agent ] || wrong="author kind ${kind:-none}"
  [ "$owner" = "$person_lower" ] || wrong="${wrong:+$wrong, }owner ${owner:-none}"
  case $written_in in '' | null | "$project") ;; *) wrong="${wrong:+$wrong, }project $written_in" ;; esac
  # The session the write proved, held to the one Codex recorded; with no record (hooks that did
  # not run) there is none to hold it to, and C1 says so.
  if [ -n "$session" ]; then
    case $written_as in '' | null | "$session") ;; *) wrong="${wrong:+$wrong, }session $written_as" ;; esac
  fi
  case $scopes in '' | null | 0) ;; *) wrong="${wrong:+$wrong, }$scopes scopes" ;; esac
  if [ -n "$(unknown_of "$read_back")" ] || [ "$(json_field status "$read_back")" != 200 ]; then
    row C3 "$want3" "dispatch comment exited 0; the probe's read of $issue's comments: ${read_back:0:200}" UNKNOWN
  elif [ "$(json_field found "$read_back")" != true ]; then
    row C3 "$want3" "dispatch comment exited 0, and no comment of $issue holds '$comment_text'" FAIL
  elif [ -n "$wrong" ]; then
    row C3 "$want3" "comment $(json_field id "$read_back") on $issue was written with $wrong" FAIL
  else
    row C3 "$want3" "comment $(json_field id "$read_back") on $issue: by $owner's agent as session ${written_as:-not named}, in ${written_in:-no project named}, ${scopes:-unread} scopes" PASS
  fi
fi

# C4: the probe's message in the model's next request, held to its control.
inbound=$(logged main inbound)
control=$(logged main control)
control_status=$(json_field status "$control_answer")
inbound_status=$(json_field status "$inbound_answer")
if [ -z "$listed" ]; then
  row C4 "$want4" "not run: the session was not listed at the edge (C1)" UNKNOWN
elif [ -n "$control" ]; then
  row C4 "$want4" "the message sent to $peer-foreign, a session of another project (answered ${control_status:-unread}), reached the Codex session's model" FAIL
elif [ -n "$(unknown_of "$inbound_answer")" ] || [ -z "$inbound_status" ] || [ "${inbound_status#5}" != "$inbound_status" ]; then
  row C4 "$want4" "the probe's send to $session got no answer it could read: ${inbound_answer:0:200}" UNKNOWN
elif [ "${inbound_status#2}" = "$inbound_status" ]; then
  row C4 "$want4" "the edge refused the probe's send to $session, a session of its own project ($inbound_status)" FAIL
elif [ -z "$inbound" ]; then
  if [ -n "$(logged main wait-ended)" ]; then
    row C4 "$want4" "the send to $session answered $inbound_status, and no model request held it after $wait_calls sleeps of $wait_seconds s" FAIL
  else
    row C4 "$want4" "the send to $session answered $inbound_status, and no model request held it before $main_end" UNKNOWN
  fi
elif [ "$control_status" != 404 ]; then
  row C4 "$want4" "the message reached the model; the control's send to $peer-foreign answered ${control_status:-no status}, not 404, so it is not the refusal the control reads: ${control_answer:0:200}" UNKNOWN
else
  inbound_at=$(json_field at "$inbound")
  case $inbound_at in '' | *[!0-9]*) inbound_at='' ;; esac
  if [ -z "$inbound_at" ]; then
    row C4 "$want4" "the message reached the model at a time the model's log does not give" UNKNOWN
  elif [ $((inbound_at / 1000 - inbound_sent_at)) -gt "$inbound_bound" ]; then
    row C4 "$want4" "the message reached the model $((inbound_at / 1000 - inbound_sent_at)) s after the send" FAIL
  else
    row C4 "$want4" "the message reached the model $((inbound_at / 1000 - inbound_sent_at)) s after the send, after $(json_field waits "$inbound") sleeps; the control's send to $peer-foreign answered 404 and never reached it" PASS
  fi
fi

# C5: the session's envoy_send, read by the probe's receiver.
send=$(logged main send)
sent=$(logged main sent)
received=$(cat "$work/receive.out" 2>/dev/null)
received_line=''
while IFS= read -r each; do
  case $each in *'"read":'*) received_line=$each ;; esac
done <<EOF
$received
EOF
if [ -z "$inbound" ]; then
  row C5 "$want5" "not run: the session never held the probe's message, after which the model sends" UNKNOWN
elif [ -n "$receiver_problem" ]; then
  row C5 "$want5" "$receiver_problem" UNKNOWN
elif [ -z "$sent" ]; then
  row C5 "$want5" "the model's envoy_send call got no output ($main_end)$tools_note" UNKNOWN
elif [ "$(json_field ok "$sent")" != true ]; then
  row C5 "$want5" "envoy_send answered an error: $(json_field detail "$sent")" FAIL
elif [ -z "$received_line" ] || [ "$(json_field read "$received_line")" = null ]; then
  row C5 "$want5" "envoy_send answered $(json_field detail "$sent"); the probe's session $receiver got no message holding it" FAIL
elif [ "$(json_field from "$received_line")" != "$session" ]; then
  row C5 "$want5" "the probe read the message from $(json_field from "$received_line"), not from the Codex session $session" FAIL
else
  read_at=$(json_field at "$received_line")
  send_at=$(json_field at "$send")
  case $read_at$send_at in '' | *[!0-9]*) read_at='' ;; esac
  if [ -z "$read_at" ] || [ -z "$send_at" ]; then
    row C5 "$want5" "the probe read the message from $session, at times the logs do not give" UNKNOWN
  elif [ $(((read_at - send_at) / 1000)) -gt "$outbound_bound" ]; then
    row C5 "$want5" "the probe read the message $(((read_at - send_at) / 1000)) s after the model's send" FAIL
  else
    row C5 "$want5" "the probe's session $receiver read it from $session $(((read_at - send_at) / 1000)) s after the model's send" PASS
  fi
fi

# C6: whoami with the edge listener refused.
refused=$(logged edge-refused whoami)
if [ -z "$main_whoami" ]; then
  row C6 "$want6" "not read: C2's whoami, with the edge listener there, did not name the session, the person and the project, so this one compares with nothing" UNKNOWN
elif [ -z "$refused_before" ] || [ -z "$refused_after" ]; then
  row C6 "$want6" "$refused_origin did not refuse a connection $([ -z "$refused_before" ] && printf 'before' || printf 'after') the run, so the run did not read a refused listener ($refused_end)" UNKNOWN
elif [ -z "$refused_session" ]; then
  row C6 "$want6" "Codex filed no session record for the run with the edge listener refused ($refused_end)" UNKNOWN
elif [ -z "$refused" ]; then
  row C6 "$want6" "the model's dispatch whoami call got no output with the edge listener refused ($refused_end)" UNKNOWN
else
  wrong=$(whoami_reading "$refused" "$refused_session")
  if [ "$(json_field exit "$refused")" != 0 ]; then
    row C6 "$want6" "with the edge listener refused, dispatch whoami through the tool exited $(json_field exit "$refused")" FAIL
  elif [ -n "$wrong" ]; then
    row C6 "$want6" "with the edge listener refused, dispatch whoami through the tool named $wrong" FAIL
  else
    row C6 "$want6" "session $refused_session, owner $person_lower, project $project, with ENVOY_URL at $refused_origin, which refused a connection before and after the run" PASS
  fi
fi

# C7: the installed plugin's own entry, from a terminal.
if [ ! -f "$entry" ]; then
  row C7 "$want7" "the installed plugin at $plugin_root has no dist/dispatch.js, the entry the install text logs in with" FAIL
elif [ "$terminal_exit" != 0 ]; then
  case $terminal_out in
    *'dispatch config: '*) row C7 "$want7" "bun $entry whoami, as session $terminal_session, refused its configuration (exit $terminal_exit): $(cell "${terminal_out:0:300}")" FAIL ;;
    *) row C7 "$want7" "bun $entry whoami, as session $terminal_session, exited $terminal_exit: $(cell "${terminal_out:0:300}")" UNKNOWN ;;
  esac
else
  case $terminal_out in
    *'Your credential names session '*)
      row C7 "$want7" "bun $entry whoami: the credential names another session than $terminal_session: $(cell "${terminal_out:0:300}")" FAIL
      ;;
    *" acts for $person_lower:"*"may write project $project and no other"*)
      row C7 "$want7" "bun $entry whoami, as session $terminal_session: acts for $person_lower, may write project $project and no other" PASS
      ;;
    *) row C7 "$want7" "bun $entry whoami named another person or project: $(cell "${terminal_out:0:300}")" FAIL ;;
  esac
fi

finish
