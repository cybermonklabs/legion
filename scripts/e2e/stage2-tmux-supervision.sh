#!/usr/bin/env bash
# Stage 2's gate for the Go coordinator: supervision on tmux, proven against the real things. The
# Go daemon launches a real Oh My Pi — the build LEGION_TEST_OMP names, with this checkout's two
# plugins in an isolated OMP profile — in panes of its private tmux server, against a real Envoy
# listener and NATS and a real Postgres. Every gate behaviour is one named check, which prints what
# it observed and then `CHECK <name>: PASS`; the first check that does not hold prints
# `CHECK <name>: FAIL: <why>` and ends the run with exit 1. The agent-harness job's tmux-scripts
# part runs it as a step (.github/workflows/ci-job-agent-harness.yaml).
#
# Stage 1's discipline holds: the cleanup cannot fail, every wait is bounded, a process that
# ignores its stop is SIGKILLed, and everything the run takes is its own — its mktemp work
# directory, its database, its OMP profile (under the HOME its Oh My Pi processes run under, inside
# the work directory: make_omp_home, lib/omp-home.sh), its ports, its project keys, its tmux
# servers (TMUX_TMPDIR under the work directory), and its legions registry (XDG_STATE_HOME). The
# work directory survives a failure, because its logs are the evidence, and goes when the run passed.
#
# Inputs, both required, and both set by the job: LEGION_TEST_OMP, the Oh My Pi every pane and the
# boot gate run (the build .omp-pin pins, which .github/actions/install-omp installs), and
# LEGION_TEST_PG_DSN, a loopback PostgreSQL the run creates its daemons' database in and drops on
# exit. Nothing else comes from outside the runner:
#   - NATS is natsd (packages/envoy/internal/testnats/cmd/natsd), built from this checkout and
#     started on a port of its own with JetStream;
#   - the model is lib/messages-standin.ts, an Anthropic Messages stand-in on loopback that answers
#     every turn `ready`. The agents reach it through the key-file route
#     (lib/install-key-file-model-route.sh --base-url), whose key command reads a placeholder key
#     from a 0600 file of the run's own on every call, and the stand-in refuses a request without
#     that key, so a turn it served proves the route did;
#   - the provider-key path is proven with a placeholder too: `provider_keys` names
#     `GEMINI_API_KEY: file:<a 0600 file of the run's own>`, which the daemon reads at boot and
#     hands every pane's shim as a daemon-held file; the run reads it only to compare.
set -euo pipefail
# This rig's NATS is a throwaway server with no users. nats.go refuses an nkey when the server sends
# no nonce ("nats: nkeys not supported by the server"), so no process here inherits an operator's
# pane seed (NATS_NKEY_SEED, NATS_NKEY_SEED_FILE) or daemon seed (NATS_DAEMON_NKEY_SEED,
# NATS_DAEMON_NKEY_SEED_FILE).
unset NATS_NKEY_SEED NATS_NKEY_SEED_FILE NATS_DAEMON_NKEY_SEED NATS_DAEMON_NKEY_SEED_FILE

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d "/tmp/legion-e2e2.$$.XXXXXXXX")
# Every line of the run also goes to $work/transcript.log (lib/transcript.sh), so the driver and its
# cleanup, which stops the panes and drops the database, never fail on a write whoever is reading.
# The transcript is in the work directory, kept with it on a failure.
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$work/transcript.log"
# Every program the run execs from here on carries its mark, and the sweeps in cleanup end what
# carries it and nothing else (marked_processes, lib/process-tree.sh); a fork of this shell that
# execs nothing carries none and is stopped by its pid (run_marks' comment there). The
# transcript's tee, started above, carries none, so it outlives the sweeps and writes the run's
# last line.
export LEGION_E2E_RUN=$work
ok=
daemon_pid=
deadline_pid=
listener_pid=
natsd_pid=
standin_pid=
daemon_keeper_pid=
deadline_keeper_pid=
database=
profile=legion-e2e2-$$-$(date +%s)
project="S2E$$$(date +%s)"            # a retired claim is never spawned again: a fresh key per run
deadline_project="S2D$$$(date +%s)"
ptoken=${project,,} # the daemon's project token: its tmux socket, its claim tokens
deadline_ptoken=${deadline_project,,}
state=$work/state
omp_home=$work/omp-home
profile_agent=$omp_home/.omp/profiles/$profile/agent
daemon_log=$work/daemon.log
check=setup
TZ=UTC printf -v check_started '%(%FT%TZ)T' -1 # when the current check began (lib/model-gateway-unserved.sh)
timeout_hook=
# shellcheck source-path=SCRIPTDIR source=lib/omp-home.sh
. "$root/scripts/e2e/lib/omp-home.sh"
# shellcheck source-path=SCRIPTDIR source=lib/loopback-dsn.sh
. "$root/scripts/e2e/lib/loopback-dsn.sh"
# shellcheck source-path=SCRIPTDIR source=lib/process-tree.sh
. "$root/scripts/e2e/lib/process-tree.sh"

# ---- reporting and waiting ------------------------------------------------------------------------

begin() {
  check=$1
  TZ=UTC printf -v check_started '%(%FT%TZ)T' -1
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "CHECK $check: PASS"; }
fail() {
  echo "CHECK $check: FAIL: $*"
  exit 1
}
# until_true SECONDS WHAT CMD… polls CMD every half second, failing the current check with WHAT.
# A check that set timeout_hook has it run first, to say what the last poll saw (lib/rig.sh's).
until_true() {
  local limit=$1 what=$2 i
  shift 2
  for ((i = 0; i < limit * 2; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  if [ -n "$timeout_hook" ]; then "$timeout_hook" || true; fi
  fail "timed out after ${limit}s waiting for $what"
}

# ---- cleanup: never fails, leaves nothing of the run behind ---------------------------------------

cleanup() {
  local status=$? p
  # A second Ctrl-C, TERM or HUP must not end the teardown part way: its own signal traps would
  # exit from inside this one, leaving panes running. Every wait below is bounded, so ignoring them
  # costs at most those bounds.
  trap '' HUP INT TERM
  # First, before anything slower can hang or be SIGKILLed mid-teardown: the key-file route's own
  # key command, which bakes in the key file's resolved path, must never survive this script on
  # any exit, so it is never conditioned on how the rest of cleanup goes.
  rm -rf "$work/model-gateway-key" || true
  stop_pid "$daemon_pid"
  stop_pid "$deadline_pid"
  stop_pid "$daemon_keeper_pid"
  stop_pid "$deadline_keeper_pid"
  for p in "$ptoken" "$deadline_ptoken"; do
    TMUX_TMPDIR=$work/tmux tmux -L "legion-$p" kill-server >/dev/null 2>&1 || true
  done
  stop_pid "$listener_pid"
  stop_pid "$natsd_pid"
  stop_pid "$standin_pid"
  stop_marked_processes "$work" || true
  if [ -n "$database" ]; then
    psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 -c "drop database if exists \"$database\" with (force)" >/dev/null 2>&1 ||
      echo "could not drop database $database; drop it by hand"
  fi
  if [ -n "${ok:-}" ]; then
    rm -rf "$work" || true
  else
    echo "the run's workspace is $work (transcript: $work/transcript.log; daemon log: $daemon_log; model stand-in requests: $work/standin-requests.jsonl; model key command log: $(shopt -s nullglob; set -- "$work"/model-gateway/*.log; echo "${1:-none yet}"))"
    # A diagnostic: it never sets the run's status (errexit is on here). A hangup, an interrupt or
    # a termination (129, 130, 143, as trapped below) stopped the run and gets none; a child a signal
    # killed, which is a failure, still does.
    [[ $status =~ ^(129|130|143)$ ]] || bash "$root/scripts/e2e/lib/model-gateway-unserved.sh" --notes "$work/model-gateway" "$check_started" "$check" || true
  fi
  return 0
}
trap cleanup EXIT
# Any command that fails outside a check's own assertions still names the check it ended.
trap 'echo "CHECK $check: FAIL: line $LINENO exited $?: $BASH_COMMAND"' ERR
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- the pieces ------------------------------------------------------------------------------------

legion() { "$work/legion" "$@"; }
claims() { # claims <sub> [flags…] — the operator CLI against the main daemon
  local sub=$1
  shift
  legion claims "$sub" --config "$work/legion.yaml" --operator-token-file "$work/operator-token" "$@"
}
claim_json() { claims list --json | jq -ce --arg t "$1" '.claims[] | select(.token == $t)'; }
claim_is() { claim_json "$1" | jq -e "$2" >/dev/null; } # claim_is TOKEN JQ-PREDICATE
tm() { tmux -L "legion-$ptoken" "$@"; }
panes() { tm list-panes -a -F '#{pane_id}' 2>/dev/null | sort; }
# pane_gone PANE: PANE is not among the daemon's tmux panes. It reads tmux's own answer rather than
# `panes`, which drops tmux's stderr and so reads any failed listing as no panes at all: a listing
# that fails is no answer, except that tmux's server exits with its last pane, so a gone server
# holds none: tmux leaves its socket behind, so "no server running" is the usual answer, and a
# socket that no longer exists is the other. report_panes is the timeout_hook that says what tmux
# last answered.
pane_gone() {
  local listed
  if ! listed=$(tm list-panes -a -F '#{pane_id}' 2>&1); then
    case "$listed" in
      *"no server running"* | *"error connecting to"*"(No such file or directory)"*) return 0 ;;
    esac
    return 1
  fi
  ! grep -qxF "$1" <<<"$listed"
}
report_panes() { note "tmux list-panes answered: $(tm list-panes -a -F '#{pane_id}' 2>&1 | tr '\n' ' ')"; }
# log_count MSG [DELIVERY]: how many JSON lines of the daemon log carry MSG (and that delivery id).
log_count() {
  jq -R --arg m "$1" --arg d "${2:-}" 'fromjson? | select(.msg == $m and ($d == "" or .delivery == $d))' \
    "$daemon_log" | jq -s length
}
# user_turns FILE MARKER: how many user messages in an OMP session file carry MARKER — the turns
# a task started.
user_turns() {
  jq -R --arg m "$2" 'fromjson? | select(.type == "message" and .message.role == "user")
    | [.message.content[]? | .text? // empty] | join(" ") | select(contains($m))' "$1" | jq -s length
}
# standin_turns MARKER: how many model requests carrying MARKER in their last user message the
# stand-in answered with the route's key, and how many it refused for want of it.
standin_turns() {
  jq -R --arg m "$1" --argjson keyed "$2" 'fromjson? | select(.method == "POST" and (.path | endswith("/v1/messages"))
    and .keyed == $keyed and (.text | contains($m)))' "$work/standin-requests.jsonl" | jq -s length
}
# omp_of PANE_PID: the pane's Oh My Pi, the one process below the pane's own process, the pid the
# claim's locator recorded, that runs the Oh My Pi under proof $expected_omp by the kernel's
# record (launched, lib/process-tree.sh), whatever name it was called by. No argv[0] is read: a
# wrapper or an argv[0] of another name cannot stand in for it or hide it.
omp_of() { launched "$1" "$expected_omp"; }
# argv0 PID prints the program PID was called as, read whole (process_args, lib/process-tree.sh), for
# a failure line; it prints nothing for a process that has gone.
argv0() {
  local -a args=()
  process_args "$1" args || return 0
  printf '%s\n' "${args[0]}"
}
# incarnation_of CLAIM_JSON prints the pane pid of the claim's locator incarnation `<pid>:<start
# ticks>`, and only while that pid is still the process that started then (incarnation_pid,
# lib/process-tree.sh), never a pid the kernel has since given to another.
incarnation_of() { incarnation_pid "$(jq -r '.locator.incarnation // empty' <<<"$1")"; }
# start_daemon: the main daemon, in the background, its log appended to daemon.log, on a copy of the
# socket its keeper holds (below). HOME and OMP_PROFILE select the run's isolated profile for the
# plugin gate and — through the pane allow-list — every pane, and LEGION_OMP_PATH the Oh My Pi
# every pane and the gate run. Each poll has a timeout: until the daemon serves, the kept port
# queues a request unanswered instead of refusing it.
start_daemon() {
  echo "=== boot at $(date -u +%FT%TZ)" >>"$daemon_log"
  python3 "$held_port" exec --from "$work/daemon.sock" --fd-env LEGION_LISTEN_FD -- \
    env LEGION_OMP_PATH="$omp" HOME="$omp_home" OMP_PROFILE="$profile" "$work/legion" start --config "$work/legion.yaml" >>"$daemon_log" 2>&1 &
  daemon_pid=$!
  record_start "$daemon_pid" || true
  until_true 180 "the daemon to answer /healthz" curl -fs --max-time 5 "http://127.0.0.1:$port/healthz"
}
# stop_daemon PID HOW: bounded, and the exit status judged — daemon.Run returns 0 on a cancelled
# context, so anything else is a defect, not a stop.
stop_daemon() {
  local pid=$1 i st=0
  for i in $(seq 1 100); do
    kill -0 "$pid" 2>/dev/null || break
    [ "$i" = 100 ] && {
      kill -KILL "$pid" 2>/dev/null || true
      fail "the daemon ignored $2 for 20s (SIGKILLed)"
    }
    sleep 0.2
  done
  wait "$pid" || st=$?
  [ "$st" = 0 ] || fail "the daemon exited $st on $2"
}
# expect_refusal NAME PATTERN: `legion start` must refuse, printing PATTERN.
expect_refusal() {
  local out=$work/refusal-$1.log st=0
  env LEGION_OMP_PATH="$omp" HOME="$omp_home" OMP_PROFILE="$profile" timeout 300 "$work/legion" start --config "$work/legion.yaml" >"$out" 2>&1 || st=$?
  [ "$st" != 0 ] || fail "legion start was not refused (exit 0)"
  [ "$st" != 124 ] || fail "legion start neither refused nor served within 300s"
  grep -qF "$2" "$out" || fail "the refusal (exit $st) does not say \"$2\": $(grep -v '^{' "$out" | head -3)"
  note "legion start exit $st: $(grep -F "$2" "$out" | head -1)"
}
# gate_loaders SKILL: the files the boot gate's refusal names as loading SKILL, in its order, read
# from the same files the gate reads (promptReferences and kindRefusal,
# packages/daemon/internal/daemon/promptreferences.go): each Markdown file under the installed
# pi-legion's `omp.skills` directories and then its agents/, by its path under the plugin, then each
# role prompt the daemon embeds from this checkout, as roles/<file> (prompts.RoleReferences). Each
# directory is walked as Go's WalkDir walks it, its entries in lexical order, and a file loads SKILL
# when it writes skill://SKILL as promptrefs reads a skill's name.
gate_loaders() {
  python3 - "$1" "$work/pi-legion" "$root/packages/daemon/internal/prompts/roles" <<'PY'
import json, os, re, sys
from pathlib import Path

skill, plugin, roles = sys.argv[1].encode(), sys.argv[2], sys.argv[3]
reference = re.compile(rb"skill://([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)")
loaders = []

def walk(directory, under, embedded=False):
    with os.scandir(directory) as scan:
        entries = sorted(scan, key=lambda entry: entry.name)
    for entry in entries:
        # go:embed leaves out of an embedded directory every name that begins with . or _.
        if embedded and entry.name[0] in "._":
            continue
        if entry.is_dir(follow_symlinks=False):
            walk(entry.path, os.path.join(under, entry.name), embedded)
        elif embedded or entry.name.endswith(".md"):
            names = (match.group(1) for match in reference.finditer(Path(entry.path).read_bytes()))
            if skill in names:
                loaders.append(os.path.join(under, entry.name))

manifest = json.loads(Path(plugin, "package.json").read_text())
for directory in (manifest.get("omp") or {}).get("skills") or []:
    walk(os.path.realpath(os.path.join(plugin, directory)), os.path.normpath(directory))
if os.path.exists(os.path.join(plugin, "agents")):
    walk(os.path.realpath(os.path.join(plugin, "agents")), "agents")
walk(roles, "roles", embedded=True)
print(", ".join(loaders))
PY
}

# ---- setup -----------------------------------------------------------------------------------------

for tool in go jq curl python3 tmux bun socat psql; do
  command -v "$tool" >/dev/null || fail "$tool is required"
done
omp=${LEGION_TEST_OMP:-}
[ -n "$omp" ] || fail "LEGION_TEST_OMP is unset: name the Oh My Pi every pane runs (.github/actions/install-omp exports it in CI)"
[ -x "$omp" ] || fail "LEGION_TEST_OMP names $omp, which is not an executable file"
pg_dsn=${LEGION_TEST_PG_DSN:-}
[ -n "$pg_dsn" ] || fail "LEGION_TEST_PG_DSN is unset: name the loopback PostgreSQL the run creates its daemons' database in (the agent-harness job's service in CI)"
require_loopback_dsn db_host LEGION_TEST_PG_DSN "$pg_dsn" "the run creates and drops a database there" || fail "$db_host"
# The guard reads the DSN alone, and psql would read these three besides it (lib/loopback-dsn.sh).
unset PGHOSTADDR PGSERVICE PGSERVICEFILE
mkdir -p "$state" "$work/xdg" "$work/tmux" "$work/stub"
make_omp_home "$omp_home"
expected_omp=$(readlink -f "$omp")
echo "Oh My Pi under proof: $expected_omp ($("$omp" --version 2>&1 | head -1)); ordinary PATH omp: $(command -v omp || echo none)"
# profile_omp ARGS…: that Oh My Pi on the run's own profile, as the daemon runs it.
profile_omp() { HOME="$omp_home" OMP_PROFILE="$profile" "$omp" "$@"; }

# The stand-in is Bun with this checkout's installed workspace, and so are the plugin packs below.
(cd "$root" && bun install --frozen-lockfile >/dev/null)

# Every port is one the kernel chose, held from that choice until its process serves on it
# (lib/held-port.py), so nothing else can take it in between. A keeper holds each daemon's for the
# whole run, so its legion.yaml can name the port before the daemon starts and the main daemon's
# restart below serves the same port; the listener's helper holds its own and becomes the listener.
# The stand-in and natsd bind their own and report it.
held_port=$root/scripts/e2e/lib/held-port.py
# shellcheck source-path=SCRIPTDIR source=lib/port-file.sh
. "$root/scripts/e2e/lib/port-file.sh"
# port_of WHAT FILE PID VAR LOG reads the port held-port.py wrote to FILE into VAR, failing the
# check naming WHAT, and LOG, the moment PID exits first (read_port_file, lib/port-file.sh).
port_of() {
  local rc=0
  read_port_file "$2" "$3" "$4" || rc=$?
  case $rc in
    0) ;;
    1) fail "$1 exited before it held a port; see $5" ;;
    *) fail "$1 held no port within ${port_file_seconds}s; see $5" ;;
  esac
}

# The model route, installed before the run's own XDG directories are exported: the model
# stand-in first, keyed by a placeholder of the run's own, then the key-file route at it, whose
# own preflight read stops the run here if the file cannot be read.
(umask 077 && printf 'stage2-standin-%s\n' "$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')" >"$work/model-key")
bun "$root/scripts/e2e/lib/messages-standin.ts" "$work/standin.port" "$work/model-key" "$work/standin-requests.jsonl" >"$work/standin.log" 2>&1 &
standin_pid=$!
record_start "$standin_pid" || true
standin_port=''
port_of "the model stand-in" "$work/standin.port" "$standin_pid" standin_port "$work/standin.log"
key_command=$(bash "$root/scripts/e2e/lib/install-key-file-model-route.sh" --profile "$profile" --home "$omp_home" --dest "$work/model-gateway" --key-dir "$work/model-gateway-key" --key-file "$work/model-key" --base-url "http://127.0.0.1:$standin_port") ||
  fail "the agents' model route to the stand-in could not be installed (the reason is above)"
# The call record (.log/.calls) sits under $work/model-gateway; the key command itself lives under
# its own --key-dir, which the run's cleanup removes unconditionally, so the record is addressed
# separately.
key_record=$work/model-gateway/$(basename "$key_command")
note "the model is the stand-in on 127.0.0.1:$standin_port, keyed by $key_command"
export XDG_STATE_HOME=$work/xdg # the legions registry this run writes is its own
export TMUX_TMPDIR=$work/tmux   # so are the daemons' private tmux servers

python3 "$held_port" keep --port-file "$work/daemon.port" --socket "$work/daemon.sock" >>"$work/keepers.log" 2>&1 &
daemon_keeper_pid=$!
record_start "$daemon_keeper_pid" || true
python3 "$held_port" keep --port-file "$work/deadline.port" --socket "$work/deadline.sock" >>"$work/keepers.log" 2>&1 &
deadline_keeper_pid=$!
record_start "$deadline_keeper_pid" || true
port='' deadline_port='' envoy_port='' nats_port=''
port_of "the daemon's keeper" "$work/daemon.port" "$daemon_keeper_pid" port "$work/keepers.log"
port_of "the second daemon's keeper" "$work/deadline.port" "$deadline_keeper_pid" deadline_port "$work/keepers.log"
(cd "$root/packages/daemon" && go build -o "$work/legion" ./cmd/legion)
(cd "$root/packages/envoy" && go build -o "$work/envoy-listener" ./cmd/listener)
(cd "$root/packages/envoy" && go build -o "$work/natsd" ./internal/testnats/cmd/natsd)
# The binary under proof, checkable after the run: the source it was built from, what a changed
# working copy held (a negative control's), and its hash (lib/built-from.sh).
built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/legion") || fail "lib/built-from.sh could not say what the run built"
while IFS= read -r line; do note "$line"; done <<<"$built"

# shellcheck source-path=SCRIPTDIR source=lib/leftovers.sh
. "$root/scripts/e2e/lib/leftovers.sh"
refuse_leftovers legion-e2e2

# The daemons' database, the run's own on LEGION_TEST_PG_DSN's server, as a libpq connection
# string (lib/loopback-dsn.sh's run_database_url) that legion.yaml takes as a single-quoted YAML
# scalar, in which a single quote is written twice.
database=stage2_$$_$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 -c "create database \"$database\"" >/dev/null ||
  fail "could not create database $database on LEGION_TEST_PG_DSN's server"
run_database_url database_url LEGION_TEST_PG_DSN "$pg_dsn" "$database" "the run creates and drops a database there" ||
  fail "$database_url"
daemon_dsn=${database_url//\'/\'\'}

# NATS and the Envoy listener on the host: natsd with JetStream on a port it reports, and the
# listener built from packages/envoy with its API bearer, which reaches every pane as a 0600 file
# (envoy_token_file).
"$work/natsd" -p -1 -store_dir "$work/nats-store" >"$work/natsd.log" 2>&1 &
natsd_pid=$!
record_start "$natsd_pid" || true
until_true 60 "natsd to be ready" grep -q 'Server is ready' "$work/natsd.log"
nats_port=$(grep -m1 -oE 'Listening for client connections on [^ ]*:[0-9]+' "$work/natsd.log" | sed 's/.*://')
[ -n "$nats_port" ] || fail "natsd logged no client port; see $work/natsd.log"
nats_url=nats://127.0.0.1:$nats_port
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/envoy-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header")
ENVOY_API_TOKEN="$(cat "$work/envoy-token")" ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID="legion-e2e2-$$" NATS_URLS=$nats_url \
  python3 "$held_port" exec --port-file "$work/listener.port" --fd-env ENVOY_LISTEN_FD --env 'PORT={port}' -- \
  "$work/envoy-listener" >"$work/listener.log" 2>&1 &
listener_pid=$!
record_start "$listener_pid" || true
port_of "the Envoy listener" "$work/listener.port" "$listener_pid" envoy_port "$work/listener.log"
until_true 60 "the Envoy listener to answer /v1/sessions" \
  curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$envoy_port/v1/sessions"
envoy_role() { curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$envoy_port/v1/roles/$1"; }

# The branch plugins, each packed as the release packs it, into this run's own OMP profile: the
# Envoy plugin first, then the Legion plugin, whose manifest the daemon's contract gate reads. The
# plugin installs run the same build as the daemon and panes, LEGION_TEST_OMP.
envoy_manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --package pi-envoy --profile "$profile" --home "$omp_home" --dest "$work/pi-envoy" --omp-path "$omp")
manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --package pi-legion --profile "$profile" --home "$omp_home" --dest "$work/pi-legion" --omp-path "$omp")
want_contract=$(jq -r .legion.daemonApiVersion "$root/packages/pi-legion/package.json")
echo "plugins: $(jq -r '.name + "@" + .version' "$envoy_manifest") and $(jq -r '.name + "@" + .version' "$manifest") in OMP profile $profile (daemonApiVersion $want_contract)"

(umask 077 && printf 'stage2-operator-%s\n' "$project" >"$work/operator-token")
# The provider key every pane's Oh My Pi is handed, a placeholder in a 0600 file the daemon reads
# at boot (`provider_keys: {GEMINI_API_KEY: file:/abs/path}`, runtime tmux alone).
(umask 077 && printf 'stage2-gemini-%s\n' "$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')" >"$work/gemini-key")
cat >"$work/legion.yaml" <<EOF
project: $project
port: $port
postgres_dsn: '$daemon_dsn'
state_dir: $state
operator_token_file: $work/operator-token
provider_keys:
  GEMINI_API_KEY: file:$work/gemini-key
envoy_url: http://127.0.0.1:$envoy_port
nats_urls:
  - $nats_url
envoy_token_file: $work/envoy-token
probe_interval_seconds: 5
# The wake pacing off: this proof starts many roles and waits on bounded times, and at the default
# of 8 wakes a minute every wake after the first would spend those waits queueing.
worker_wakes_per_minute: 0
EOF
cat >"$work/architect.md" <<'EOF'
You are the root architect of a Legion stage-2 proof. You have no work to do. Reply to every
message with the single word `ready`, run no tools, and wait.
EOF
cat >"$work/worker.md" <<'EOF'
You are a worker in a Legion stage-2 proof. Do exactly what each message asks, in one short reply,
and run no tools.
EOF
pass

# ---- the gate refuses first ------------------------------------------------------------------------

begin gate-refuses-another-contract
cp -p "$work/pi-legion/package.json" "$work/manifest.orig"
bad_contract=$((want_contract + 1))
jq --argjson v "$bad_contract" '.legion.daemonApiVersion = $v' "$work/manifest.orig" >"$work/pi-legion/package.json"
note "the installed pi-legion manifest now declares daemonApiVersion $bad_contract (the checkout's is $want_contract)"
expect_refusal contract "speaks daemon API contract $bad_contract; this daemon requires $want_contract"
cp -p "$work/manifest.orig" "$work/pi-legion/package.json"
cmp -s "$work/manifest.orig" "$work/pi-legion/package.json" || fail "the installed manifest was not restored"
pass

begin gate-refuses-a-disabled-plugin
profile_omp plugin disable @sjawhar/pi-legion >/dev/null
note "omp plugin disable: $(profile_omp plugin list --json | jq -c '[.npm[]? | select(.name == "@sjawhar/pi-legion") | {name, enabled}]')"
expect_refusal disabled "is installed but not loaded by omp (disabled or unregistered)"
profile_omp plugin enable @sjawhar/pi-legion >/dev/null
profile_omp plugin list --json |
  jq -e '[.npm[]? | select(.name == "@sjawhar/pi-legion" and .enabled == true)] | length == 1' >/dev/null ||
  fail "the plugin did not come back enabled"
pass

begin gate-refuses-without-the-envoy-plugin
# The gate's load probe also reads whether pi-envoy publishes the plugin interface pi-legion
# speaks. The profile with pi-envoy disabled is refused naming the package to install, before any
# of the skills it ships is reported missing.
profile_omp plugin disable @sjawhar/pi-envoy >/dev/null
note "omp plugin disable: $(profile_omp plugin list --json | jq -c '[.npm[]? | select(.name == "@sjawhar/pi-envoy") | {name, enabled}]')"
expect_refusal no-envoy "but no pi-envoy is: install the @sjawhar/pi-envoy release"
profile_omp plugin enable @sjawhar/pi-envoy >/dev/null
profile_omp plugin list --json |
  jq -e '[.npm[]? | select(.name == "@sjawhar/pi-envoy" and .enabled == true)] | length == 1' >/dev/null ||
  fail "pi-envoy did not come back enabled"
pass

begin gate-refuses-a-missing-skill
# The gate's load probe resolves every skill Legion's prompts load through the pane's own Oh My Pi.
# The reviewer's pair ships in the Envoy plugin; the installed set without the rubric is refused,
# naming every file the gate reads that loads it: pi-legion's and the daemon's role prompts, never
# pi-envoy's own thermonuclear-code-quality (promptReferences, daemon/promptreferences.go).
mv "$work/pi-envoy/dist/skills/thermonuclear-deep-review" "$work/rubric.aside"
expect_refusal missing-skill "finds no skill thermonuclear-deep-review (loaded by agents/thermonuclear-deep-review.md, roles/core/reviewer.md)"
mv "$work/rubric.aside" "$work/pi-envoy/dist/skills/thermonuclear-deep-review"
[ -f "$work/pi-envoy/dist/skills/thermonuclear-deep-review/SKILL.md" ] || fail "the rubric was not restored"
pass

begin gate-refuses-a-skill-only-a-role-prompt-loads
# The gate also resolves the skills the daemon's role prompts load, beside the plugin's own: the
# refusal of the plugin without legion-controller names roles/controller-root.md, which only a gate
# that reads the daemon's own role prompts can name, after the pi-legion files that load it, which
# gate_loaders reads as the gate does, since the guides move references between files.
mv "$work/pi-legion/dist/skills/legion-controller" "$work/controller-skill.aside"
loaders=$(gate_loaders legion-controller) || fail "could not read the files that load skill legion-controller"
case ", $loaders, " in
*", roles/controller-root.md, "*) ;;
*) fail "roles/controller-root.md no longer loads skill legion-controller (its loaders: ${loaders:-none}), so this check proves nothing" ;;
esac
note "the files that load legion-controller, read as the gate reads them: $loaders"
expect_refusal prompt-only-skill "finds no skill legion-controller (loaded by $loaders)"
mv "$work/controller-skill.aside" "$work/pi-legion/dist/skills/legion-controller"
[ -f "$work/pi-legion/dist/skills/legion-controller/SKILL.md" ] || fail "the legion-controller skill was not restored"
pass

begin gate-refuses-an-unconfigured-model-role
# The gate's load probe also resolves each of those task agents' models as the task tool resolves a
# subagent's, in the pane's own Oh My Pi under this profile. The profile without modelRoles.oracle
# is refused, naming the agent and the role no one configured: the task tool would otherwise run
# every oracle consult on the session's own model without a word.
roles=$profile_agent/config.yml
cp -p "$roles" "$work/roles.orig"
grep -v '^  oracle: ' "$work/roles.orig" >"$roles"
! grep -q '^  oracle: ' "$roles" || fail "modelRoles.oracle is still in $roles"
note "modelRoles.oracle removed from $roles"
expect_refusal model-role "on its model @oracle: role oracle is not configured"
grep -qF "task agent oracle (dispatched by " "$work/refusal-model-role.log" ||
  fail "the refusal does not name task agent oracle and the prompts that dispatch it"
cp -p "$work/roles.orig" "$roles"
cmp -s "$work/roles.orig" "$roles" || fail "the profile's model roles were not restored"
pass

# ---- one real agent: register, claim its role, ready ----------------------------------------------

begin architect-registers-and-is-ready
start_daemon
grep -q '"msg":"boot gate: pi-legion speaks this daemon' "$daemon_log" ||
  fail "the daemon served without its gate's pass line in the log"
jq -R -e --arg binary "$expected_omp" '
  fromjson? | select(.msg == "legion daemon resolved OMP invocation for boot probes and panes" and (.invocation | contains($binary)))
' "$daemon_log" >/dev/null || fail "the daemon did not log the Oh My Pi under proof, $expected_omp, for its boot probes and panes"
note "$(jq -R -c 'fromjson? | select(.msg | startswith("boot gate")) | {msg, version, daemonApiVersion}' "$daemon_log" | sed -n 1p)"
note "$(jq -R -c 'fromjson? | select(.msg == "legion daemon resolved OMP invocation for boot probes and panes") | {msg, invocation}' "$daemon_log" | sed -n 1p)"
c1=$(claims spawn --json --tree S2-1 --issue S2-1 --role architect --prompt-file "$work/architect.md" | jq -r .token)
until_true 240 "claim $c1 to be ready" claim_is "$c1" '.state == "ready"'
c1_json=$(claim_json "$c1")
session1=$(jq -r .session <<<"$c1_json")
[ -n "$session1" ] && [ "$session1" != null ] || fail "the ready claim records no session"
registered=$(jq -R -c --arg c "$c1" 'fromjson? | select(.msg == "api: claim registered" and .claim == $c) | {generation, session, pluginContract}' "$daemon_log" | sed -n 1p)
[ -n "$registered" ] || fail "the daemon logged no registration for $c1"
note "claim $c1 ready: generation $(jq -r .generation <<<"$c1_json"), session $session1, pane $(jq -r .locator.tmux.pane <<<"$c1_json")"
note "registered: $registered"
pass

begin envoy-role-held
# pi-legion takes the claim's Envoy name last: after claims/ready, at the session's first turn or,
# when no turn starts, at the 30 s bound (bootLegionClaim, packages/pi-shared/src/legion-claim-core.ts;
# DEFAULT_CLAIM_FIRST_TURN_BOUND_MS). This architect has no task, so it takes the name at the bound.
role_held_by() { envoy_role "$1" | jq -e --arg s "$2" '.holder == $s' >/dev/null; }
until_true 60 "GET /v1/roles/$c1 on the Envoy listener to name $session1" role_held_by "$c1" "$session1"
note "GET /v1/roles/$c1 → holder $(envoy_role "$c1" | jq -r .holder)"
pass

begin ready-in-state
legion state --json --port "$port" >"$work/state-ready.json"
jq -e --arg s "$session1" '.issues["S2-1"].architect | .state == "ready" and .session == $s and .locator.runtime == "tmux"' \
  "$work/state-ready.json" >/dev/null || fail "legion state does not show S2-1's architect ready: $(jq -c '.issues["S2-1"].architect' "$work/state-ready.json")"
note "legion state: .issues[\"S2-1\"].architect = $(jq -c '.issues["S2-1"].architect | {session, state, locator: .locator.tmux}' "$work/state-ready.json")"
pass

# ---- delivery: queued before ready, sent once; a retried frame starts no second turn --------------

begin task-queued-before-ready-runs-once
marker1="S2E-TASK-$RANDOM$RANDOM"
spawned=$(claims spawn --json --tree S2-1 --issue S2-2 --role implementer --prompt-file "$work/worker.md" \
  --task "Reply with the single word pong. ($marker1)")
c2=$(jq -r .token <<<"$spawned")
jq -e '.pending.id != null and .pending.deliveredAt == null and (.state | IN("queued", "launching", "shim_connected"))' \
  <<<"$spawned" >/dev/null || fail "the spawn did not queue its task before the agent was ready: $spawned"
note "spawned $c2 in state $(jq -r .state <<<"$spawned") holding delivery $(jq -r .pending.id <<<"$spawned"), not yet sent"
until_true 300 "claim $c2's task to run and end" claim_is "$c2" '.state == "idle" and .pending == null'
c2_json=$(claim_json "$c2")
session_file2=$(jq -r .sessionFile <<<"$c2_json")
turns=$(user_turns "$session_file2" "$marker1")
[ "$turns" = 1 ] || fail "the task reached the agent $turns times (session file $session_file2)"
note "idle, no pending delivery; the agent's session file holds the task $turns time"
pass

begin model-turn-through-the-route
# The task's reply came through the configured route: every assistant turn in the session file ran
# as the profile's pinned model on the anthropic provider, the one provider the profile routes and
# leaves enabled, and none ended in an error; the stand-in answered the task's turn with the
# route's key and refused none of it for want of the key; and the key command served more calls
# than the preflight, which records none.
pinned=$(sed -n 's/^  default: //p' "$profile_agent/config.yml")
[ -n "$pinned" ] || fail "the profile's config.yml names no default model role: $profile_agent/config.yml"
replies=$(jq -c 'select(.type == "message" and .message.role == "assistant")
  | {provider: .message.provider, model: .message.model, stopReason: .message.stopReason}' "$session_file2" | jq -sc .)
jq -e --arg pinned "$pinned" 'length > 0 and all(.provider == "anthropic" and "anthropic/" + .model == $pinned and .stopReason != "error")' \
  <<<"$replies" >/dev/null || fail "claim $c2's replies did not all come from $pinned through the route: $replies"
note "claim $c2's replies, from its session file: $replies"
served=$(standin_turns "$marker1" true)
refused=$(standin_turns "$marker1" false)
[ "$served" -ge 1 ] || fail "the stand-in answered no model request carrying $marker1 with the route's key ($work/standin-requests.jsonl)"
[ "$refused" = 0 ] || fail "the stand-in refused $refused model request(s) carrying $marker1 for want of the route's key"
calls=$(grep -c ' invoked by pid ' "$key_record.log")
[ "$calls" -ge 2 ] || fail "the key command $key_command ran $calls time(s), the preflight's alone"
note "the stand-in answered $served model request(s) of the task with the route's key and refused none; the key command $key_command ran $calls times and served $(grep -c $'\tserved\t' "$key_record.calls") ($key_record.log, $key_record.calls)"
pass

begin retried-frame-starts-no-second-turn
# The daemon re-sends a delivery under the same id when a send is lost in transit. Stopping the
# shim makes every send go unanswered: each times out (worker_rpc_timeout_seconds), the delivery
# waits, and the next sweep sends the same frame again. Resumed, the shim hands OMP the first
# prompt and answers every repeat from its dedupe record.
pane2_pid=$(incarnation_of "$c2_json") || fail "claim $c2's pane process is not the one its locator names: $(jq -c .locator <<<"$c2_json")"
# The shim is the pane's process's one child (child_of, lib/process-tree.sh), and its argv[1] is
# worker-shim, as every reader of a Legion process tree takes it, never one naming it in a later
# argv word. It is stopped only while it is that process (freeze_if, lib/process-tree.sh).
shim2=$(child_of "$pane2_pid") || fail "pane process $pane2_pid: $shim2"
shim2_start=$(proc_start "$shim2")
shim2_args=()
process_args "$shim2" shim2_args && [ "${shim2_args[1]-}" = worker-shim ] ||
  fail "the child of pane process $pane2_pid is not the shim"
freeze_if "$shim2" "$shim2_start" STOP || fail "shim $shim2 is no longer the process that started at $shim2_start"
marker2="S2E-RETRY-$RANDOM$RANDOM"
delivery2=$(claims deliver --json --claim "$c2" --task "Reply with the single word pong. ($marker2)" | jq -r .pending.id)
lost_msg="supervise: the prompt was lost to the transport; the delivery waits"
until_true 90 "the daemon to send delivery $delivery2 twice into the stopped shim" \
  sh -c "[ \"\$(jq -R --arg m '$lost_msg' --arg d '$delivery2' 'fromjson? | select(.msg == \$m and .delivery == \$d)' '$daemon_log' | jq -s length)\" -ge 2 ]"
sends=$(log_count "$lost_msg" "$delivery2")
freeze_if "$shim2" "$shim2_start" CONT || fail "shim $shim2 is no longer the process that started at $shim2_start"
note "shim $shim2 stopped; delivery $delivery2 sent $sends times, every send unanswered; shim resumed"
until_true 180 "claim $c2's retried delivery to run and end" claim_is "$c2" '.state == "idle" and .pending == null'
turns=$(user_turns "$session_file2" "$marker2")
[ "$turns" = 1 ] || fail "delivery $delivery2, sent $sends+ times, reached the agent $turns times"
jq -e '.budgets.promptFailures == 0' <<<"$(claim_json "$c2")" >/dev/null || fail "a transport loss was charged as a prompt failure"
note "the agent's session file holds the task $turns time; promptFailures 0"
pass

# ---- the process: death, suspension, resumption ---------------------------------------------------

begin kill-pane-resumes-the-same-session
(umask 077 && cp "$state/secrets/$c1" "$work/stale-boot-token") # generation 1's boot token, for later
before=$(claim_json "$c1")
old_pane=$(jq -r .locator.tmux.pane <<<"$before")
old_gen=$(jq -r .generation <<<"$before")
tm kill-pane -t "$old_pane"
until_true 240 "claim $c1 to be ready again after its pane was killed" \
  claim_is "$c1" ".generation == $((old_gen + 1)) and .state == \"ready\""
after=$(claim_json "$c1")
[ "$(jq -r .session <<<"$after")" = "$session1" ] || fail "the relaunch registered another session: $(jq -r .session <<<"$after")"
[ "$(jq -r .locator.tmux.pane <<<"$after")" != "$old_pane" ] || fail "the claim still names the killed pane"
pane_after=$(incarnation_of "$after") || fail "claim $c1's new pane process is not the one its locator names: $(jq -c .locator <<<"$after")"
omp1=$(omp_of "$pane_after") || fail "no omp process under the new pane: $omp1"
resume_file=$(jq -r .sessionFile <<<"$after")
omp_args=()
{ mapfile -d '' -t omp_args <"/proc/$omp1/cmdline"; } 2>/dev/null || fail "the relaunched omp's ($omp1) command line could not be read"
resumed=
for arg in ${omp_args[@]+"${omp_args[@]}"}; do [ "$arg" != "--resume=$resume_file" ] || resumed=1; done
[ -n "$resumed" ] || fail "the relaunched omp ($omp1) was not started with --resume=$resume_file: $(printf '%s ' "${omp_args[@]}" | cut -c1-200)"
[ "$(envoy_role "$c1" | jq -r .holder)" = "$session1" ] || fail "the resumed session does not hold its Envoy role"
note "killed $old_pane (generation $old_gen) → pane $(jq -r .locator.tmux.pane <<<"$after"), generation $(jq -r .generation <<<"$after"), same session $session1"
note "omp $omp1 started with --resume=$resume_file; the Envoy role still held by $session1"
pass

begin suspend-keeps-the-session
before=$(claim_json "$c1")
pane=$(jq -r .locator.tmux.pane <<<"$before")
claims suspend --claim "$c1" >/dev/null
until_true 60 "claim $c1 to be suspended" claim_is "$c1" '.state == "suspended" and .locator == null'
pane_gone "$pane" || { report_panes; fail "pane $pane still exists after the suspension"; }
[ "$(claim_json "$c1" | jq -r .session)" = "$session1" ] || fail "the suspension dropped the session"
session_file1=$(claim_json "$c1" | jq -r .sessionFile)
[ -s "$session_file1" ] || fail "the session file $session_file1 is gone"
note "suspended: pane $pane gone, no locator, session $session1 kept ($session_file1)"
pass

begin resume-on-demand
gen=$(claim_json "$c1" | jq -r .generation)
claims resume --claim "$c1" >/dev/null
until_true 240 "claim $c1 to be ready after its resume" claim_is "$c1" ".state == \"ready\" and .generation == $((gen + 1))"
after=$(claim_json "$c1")
pane=$(jq -r .locator.tmux.pane <<<"$after")
panes | grep -qxF "$pane" || fail "the resumed claim names pane $pane, which tmux does not list"
[ "$(jq -r .session <<<"$after")" = "$session1" ] || fail "the resume registered another session"
note "resumed: pane $pane, generation $(jq -r .generation <<<"$after"), same session $session1"
pass

begin stale-generation-hello-refused
refused_msg="worker-stream: rejected hello (stale worker generation)"
before_count=$(log_count "$refused_msg")
# The hello2 a pane's shim sends (shimwire.Hello2; a tmux pane's carries no agentSecrets). The
# listener refuses a v1 hello before it reads the boot token, so one would never reach the
# generation check.
reply=$(printf '{"type":"hello2","bootToken":"%s"}\n' "$(cat "$work/stale-boot-token")" |
  socat -t 5 - "UNIX-CONNECT:$state/worker-stream.sock" 2>&1) || true
[ -z "$reply" ] || fail "the daemon answered a stale hello: $reply"
until_true 10 "the daemon to log the stale hello" sh -c "[ \"\$(jq -R --arg m '$refused_msg' 'fromjson? | select(.msg == \$m)' '$daemon_log' | jq -s length)\" -gt $before_count ]"
note "generation 1's hello on $state/worker-stream.sock: closed with nothing written; logged \"$refused_msg\""
pass

begin unregistered-agent-retired-at-the-deadline
# A second daemon whose OMP answers the plugin gate and otherwise never runs the plugins: its pane's
# process lives and its agent never registers. The stub answers as a pane that loads pi-legion
# beside pi-envoy does (the installed pi-envoy's manifest owns the file it names). The gate also
# asks the load probe for the task agents and skills Legion's prompts name (LEGION_PROMPT_AGENTS,
# LEGION_PROMPT_SKILLS) and whether each of those agents' models resolves (LEGION_AGENT_MODELS),
# and the stub answers that they all resolve, as the real Oh My Pi does for this checkout's plugins
# on the harness's model roles. After the gate the daemon reads the host's retry.modelFallback
# (`omp config get retry.modelFallback --json`), and the stub answers it as the real Oh My Pi does
# by default: a stub that slept there would hold the boot past its budget, so the daemon would never
# serve. The deadline is
# worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals = 10 s.
cat >"$work/stub/omp" <<EOF
#!/bin/sh
if [ "\$1" = models ]; then
  echo LEGION_PLUGIN_LOADED=yes >&2
  echo "LEGION_PLUGIN_LOADED_FROM=file://$work/pi-legion/dist/legion.js" >&2
  echo LEGION_PLUGIN_ENVOY_INTERFACE=1 >&2
  echo LEGION_ENVOY_INTERFACE=1 >&2
  echo "LEGION_ENVOY_LOADED_FROM=file://$work/pi-envoy/dist/envoy.js" >&2
  [ -n "\${LEGION_PROMPT_AGENTS:-}" ] && echo LEGION_PROMPT_AGENTS=resolved >&2
  [ -n "\${LEGION_PROMPT_AGENTS:-}" ] && [ -z "\${LEGION_SKIP_AGENT_MODELS:-}" ] && echo LEGION_AGENT_MODELS=resolved >&2
  [ -n "\${LEGION_PROMPT_SKILLS:-}" ] && echo LEGION_PROMPT_SKILLS=resolved >&2
  exit 0
fi
if [ "\$*" = "config get retry.modelFallback --json" ]; then
  echo '{"key":"retry.modelFallback","value":true,"type":"boolean"}'
  exit 0
fi
exec sleep 3600
EOF
chmod 0755 "$work/stub/omp"
mkdir -p "$work/deadline-state"
cat >"$work/deadline.yaml" <<EOF
project: $deadline_project
port: $deadline_port
postgres_dsn: '$daemon_dsn'
state_dir: $work/deadline-state
operator_token_file: $work/operator-token
worker_boot_timeout_seconds: 5
worker_boot_registration_deadline_intervals: 2
# The wake pacing off, as the run's first daemon has it: the deadlines this second daemon is read
# against are seconds long, and a paced wake would spend them waiting its turn.
worker_wakes_per_minute: 0
EOF
python3 "$held_port" exec --from "$work/deadline.sock" --fd-env LEGION_LISTEN_FD -- \
  env LEGION_OMP_PATH="$work/stub/omp" HOME="$omp_home" OMP_PROFILE="$profile" \
  "$work/legion" start --config "$work/deadline.yaml" >"$work/deadline.log" 2>&1 &
deadline_pid=$!
record_start "$deadline_pid" || true
until_true 120 "the second daemon to answer /healthz" curl -fs --max-time 5 "http://127.0.0.1:$deadline_port/healthz"
dclaims() {
  local sub=$1
  shift
  legion claims "$sub" --config "$work/deadline.yaml" --operator-token-file "$work/operator-token" "$@"
}
spawned_at=$(date +%s)
cd3=$(dclaims spawn --json --tree S2-9 --issue S2-9 --role architect --prompt-file "$work/architect.md" | jq -r .token)
until_true 30 "the unregistered claim's shim to connect" sh -c \
  "'$work/legion' claims list --json --config '$work/deadline.yaml' --operator-token-file '$work/operator-token' | jq -e '.claims[0].state == \"shim_connected\"'"
first=$(dclaims list --json | jq -c '.claims[0]')
first_inc=$(jq -r '.locator.incarnation // empty' <<<"$first")
first_pid=$(incarnation_pid "$first_inc") || fail "the pane's process of incarnation $first_inc is not alive before the deadline"
retired_msg="supervise: the agent never registered; retired its process"
until_true 60 "the second daemon to retire the unregistered agent" grep -qF "\"msg\":\"$retired_msg\"" "$work/deadline.log"
retired_after=$(($(date +%s) - spawned_at))
counted=$(dclaims list --json | jq -c '.claims[0] | {state, generation, budgets}')
jq -e '.budgets.launchFailures == 1' <<<"$counted" >/dev/null || fail "the retirement counted no launch failure: $counted"
! incarnation_pid "$first_inc" >/dev/null || fail "the retired incarnation's process $first_pid is still alive"
note "claim $cd3: shim_connected with process $first_pid alive, never registered; retired ${retired_after}s after the spawn (deadline 10s); $counted"
legion stop --config "$work/deadline.yaml" >/dev/null
stop_daemon "$deadline_pid" "legion stop"
deadline_pid=
tmux -L "legion-$deadline_ptoken" kill-server >/dev/null 2>&1 || true
pass

# ---- a daemon restart re-adopts; a stray is reaped -------------------------------------------------

begin restart-readopts-the-live-panes
before1=$(claim_json "$c1")
before2=$(claim_json "$c2")
panes_before=$(panes)
boots_before=$(legion state --json --port "$port" | jq -r .daemon.boots)
kill -TERM "$daemon_pid"
stop_daemon "$daemon_pid" SIGTERM
[ "$(panes)" = "$panes_before" ] || fail "the panes changed while the daemon was down"
start_daemon
boots=$(legion state --json --port "$port" | jq -r .daemon.boots)
[ "$boots" = $((boots_before + 1)) ] || fail "the restart recorded boots $boots, want $((boots_before + 1))"
for pair in "$c1:$before1" "$c2:$before2"; do
  token=${pair%%:*}
  was=${pair#*:}
  now=$(claim_json "$token")
  for field in .generation .locator.incarnation .locator.tmux.pane .session; do
    [ "$(jq -r "$field" <<<"$now")" = "$(jq -r "$field" <<<"$was")" ] ||
      fail "$token's $field moved across the restart: $(jq -r "$field" <<<"$was") → $(jq -r "$field" <<<"$now")"
  done
done
[ "$(panes)" = "$panes_before" ] || fail "the restart opened or closed a pane: $(panes | tr '\n' ' ') (before: $(tr '\n' ' ' <<<"$panes_before"))"
# The shim's reconnect hello is what carries a task now: a task delivered after the restart runs.
marker3="S2E-READOPT-$RANDOM$RANDOM"
until_true 60 "claim $c1 to be ready on the restarted daemon" claim_is "$c1" '.state == "ready" or .state == "idle"'
claims deliver --claim "$c1" --task "Reply with the single word ready. ($marker3)" >/dev/null
until_true 180 "the task sent after the restart to run and end" claim_is "$c1" '.state == "idle" and .pending == null'
turns=$(user_turns "$session_file1" "$marker3")
[ "$turns" = 1 ] || fail "the task sent after the restart reached the agent $turns times"
restart_line=$(grep -n '^=== boot' "$daemon_log" | tail -1 | cut -d: -f1)
grep -qF 'rejected hello' <<<"$(tail -n +"$restart_line" "$daemon_log")" && fail "the restarted daemon rejected a hello"
note "boots $boots_before → $boots; $c1 and $c2 kept generation, incarnation $(jq -r .locator.incarnation <<<"$before1") / $(jq -r .locator.incarnation <<<"$before2"), and pane; no pane opened or closed"
note "the shims' reconnect hellos were accepted: a task sent after the restart ran $turns time"
pass

begin omp-child-environment
now1=$(claim_json "$c1")
pane1_pid=$(incarnation_of "$now1") || fail "claim $c1's pane process is not the one its locator names: $(jq -c .locator <<<"$now1")"
shell_argv=()
process_args "$pane1_pid" shell_argv || fail "the command line of pane process $pane1_pid could not be read"
[ "${shell_argv[0]}" = /bin/sh ] && [ "${shell_argv[1]}" = -c ] ||
  fail "the pane's process runs '${shell_argv[0]} ${shell_argv[1]}', not sh -c"
omp1=$(omp_of "$pane1_pid") || fail "no omp process under pane process $pane1_pid: $omp1"
# The line from the pane's process down to its Oh My Pi, read upwards through each process's
# parent, and each process's command line read whole, with no pipe between the read and the check.
chain_pids=$omp1
p=$omp1
while [ "$p" != "$pane1_pid" ]; do
  p=$(proc_parent "$p") && [ -n "$p" ] && [ "$p" -gt 1 ] || fail "omp $omp1 is not below pane process $pane1_pid"
  chain_pids="$p $chain_pids"
done
chain=
for p in $chain_pids; do
  args=()
  { mapfile -d '' -t args <"/proc/$p/cmdline"; } 2>/dev/null || fail "the command line of process $p, on the line to omp $omp1, could not be read"
  for arg in ${args[@]+"${args[@]}"}; do
    case $arg in *"$HOME/.dotfiles/shims/omp"*) fail "pane process chain runs the dotfiles OMP wrapper: ${args[*]}" ;; esac
  done
  chain="${chain:+$chain → }$p ($(basename -- "${args[0]-}"))"
done
shim1=$(child_of "$pane1_pid") || fail "pane process $pane1_pid: $shim1"
# LEGION-206 P1: the pane's shell and its OMP keep the daemon's own XDG directories and carry no
# session bus address.
for p in "$pane1_pid" "$omp1"; do
  for name in XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME; do
    dir=$(env_of "$p" "$name") || fail "the environment of process $p could not be read"
    case "$dir" in "$state"/home/*) ;; *) fail "process $p ($(basename "$(argv0 "$p")"))'s $name is '$dir', not under $state/home" ;; esac
  done
  st=0
  env_has "$p" DBUS_SESSION_BUS_ADDRESS || st=$?
  [ "$st" = 1 ] || fail "process $p ($(basename "$(argv0 "$p")")) carries DBUS_SESSION_BUS_ADDRESS, or its environment could not be read (env_has status $st)"
done
xdg=$(env_of "$omp1" XDG_CONFIG_HOME) || fail "omp $omp1's environment could not be read"
st=0
env_has "$omp1" ANTHROPIC_API_KEY || st=$?
[ "$st" = 1 ] || fail "omp's environment carries ANTHROPIC_API_KEY, or could not be read (env_has status $st)"
actual_omp=$(readlink -f "/proc/$omp1/exe")
[ "$actual_omp" = "$expected_omp" ] || fail "omp $omp1 executes $actual_omp, not the Oh My Pi under proof, $expected_omp"
pane_path=$(env_of "$shim1" PATH) || fail "shim $shim1's environment could not be read"
omp_path=$(env_of "$omp1" PATH) || fail "omp $omp1's environment could not be read"
[ "${omp_path%%:*}" = "${pane_path%%:*}" ] ||
  fail "OMP PATH begins ${omp_path%%:*}, not the pane PATH head ${pane_path%%:*}"
# The key's length, and the shim's and the three names' lengths, are read without any value standing
# in a variable of this shell (env_length, lib/process-tree.sh), so a run under `bash -x` writes none
# of them to the transcript. The provider key is the daemon-held file's value, whole, in omp's own
# environment, and in the shim's own environment not at all; the value and the file are compared
# through pipes, so neither stands in a variable either.
key_length=$(env_length "$omp1" GEMINI_API_KEY) || fail "omp $omp1's environment could not be read"
[ "$key_length" -gt 0 ] || fail "omp's environment carries no GEMINI_API_KEY"
cmp -s <(env_of "$omp1" GEMINI_API_KEY | tr -d '\n') <(tr -d '\n' <"$work/gemini-key") ||
  fail "omp's environment does not carry GEMINI_API_KEY as provider_keys' file holds it (it carries $key_length bytes)"
shim_key_length=$(env_length "$shim1" GEMINI_API_KEY) || fail "shim $shim1's environment could not be read"
[ "$shim_key_length" = 0 ] || fail "the shim's own environment carries the provider key"
for name in GEMINI_API_KEY_TESTS SOPS_AGE_KEY_FILE SECRETSD_CONFIG; do
  length=$(env_length "$omp1" "$name") || fail "omp $omp1's environment could not be read"
  [ "$length" = 0 ] || fail "omp's environment carries $name"
done
note "pane process $pane1_pid is /bin/sh -c; its line to omp: $chain"
note "omp $omp1 executes the Oh My Pi under proof $actual_omp, no process in its chain runs the dotfiles wrapper, and its PATH head ${omp_path%%:*} matches the pane's; XDG_CONFIG_HOME=$xdg; GEMINI_API_KEY of $key_length bytes, the daemon-held file's, absent from the shim; no ANTHROPIC_API_KEY, GEMINI_API_KEY_TESTS, SOPS_AGE_KEY_FILE or SECRETSD_CONFIG"
note "the pane's shell $pane1_pid and omp $omp1: all four XDG base directories under $state/home, no DBUS_SESSION_BUS_ADDRESS"
pass

begin stray-pane-reaped-after-the-grace
# A window marked as this daemon's that holds no pane any claim records — what a crash between
# opening a window and persisting its locator leaves — and, beside it, a window a human opened.
stray=$(tm new-window -d -t "legion-$ptoken" -P -F '#{window_id}' 'sleep 3600')
tm set-option -w -t "$stray" @legion_owner "legion-$ptoken"
human=$(tm new-window -d -t "legion-$ptoken" -P -F '#{window_id}' 'sleep 3600')
made=$(date +%s)
windows() { tm list-windows -a -F '#{window_id}' 2>/dev/null; }
stray_gone() { ! windows | grep -qxF "$stray"; }
until_true 300 "the periodic orphan sweep to reap window $stray" stray_gone
reaped_after=$(($(date +%s) - made))
[ "$reaped_after" -ge 120 ] || fail "window $stray was reaped ${reaped_after}s after it opened, inside the 120s grace"
windows | grep -qxF "$human" || fail "the unmarked window $human was reaped"
grep -qF '"msg":"tmux runtime: killing an orphaned window"' "$daemon_log" || fail "the daemon logged no orphan reap"
for token in "$c1" "$c2"; do
  pane=$(claim_json "$token" | jq -r .locator.tmux.pane)
  panes | grep -qxF "$pane" || fail "the sweep took $token's pane $pane"
done
tm kill-window -t "$human"
note "stray window $stray (marked, no recorded pane) reaped ${reaped_after}s after it opened; the unmarked window $human and both claims' panes kept"
pass

# ---- done ------------------------------------------------------------------------------------------

begin stop
# The tree's root claim ends only when its tree closes: the operator's stop of it is refused, names
# suspend, and changes nothing — not its state, its generation, or its pane. No workflow issue backs
# S2-1, so the refusal also names the operator's close, which ends its tree. Suspending the root
# stops its process. A worker's claim is the operator's to stop: a second worker of S2-1 is stopped,
# retired with its pane gone. The operator then closes S2-1 while its first worker still runs: the
# close ends the root claim and then every other claim of the tree, so that worker's claim is
# retired and its pane is gone too.
root_before=$(claim_json "$c1" | jq -c '{generation, pane: .locator.tmux.pane}')
if refusal=$(claims stop --claim "$c1" 2>&1 >/dev/null); then
  fail "the operator's stop of the root claim $c1 was accepted"
fi
case "$refusal" in
  *"409 Conflict: stop refused: the tree's root claim ends only when its tree closes; suspend it to stop its process; no workflow issue backs its tree, so legion claims close ends it"*) ;;
  *) fail "the root's stop was refused with '$refusal', not the root rule naming suspend and the close" ;;
esac
claim_is "$c1" '.state == "ready" or .state == "idle"' ||
  fail "the refused stop moved $c1: $(claim_json "$c1" | jq -c '{state, generation}')"
root_after=$(claim_json "$c1" | jq -c '{generation, pane: .locator.tmux.pane}')
[ "$root_after" = "$root_before" ] || fail "the refused stop moved $c1 from $root_before to $root_after"
claims suspend --claim "$c1" >/dev/null
until_true 60 "the root to be suspended" claim_is "$c1" '.state == "suspended"'
timeout_hook=report_panes
c3=$(claims spawn --json --tree S2-1 --issue S2-3 --role tester --prompt-file "$work/worker.md" | jq -r .token)
until_true 240 "the second worker $c3 to be ready" claim_is "$c3" '.state == "ready"'
stopped_pane=$(claim_json "$c3" | jq -r .locator.tmux.pane)
claims stop --claim "$c3" >/dev/null || fail "the operator's stop of the worker $c3 was refused"
until_true 60 "the stopped worker $c3 to be retired" claim_is "$c3" '.state == "retired"'
until_true 30 "the stopped worker's pane $stopped_pane to be gone" pane_gone "$stopped_pane"
worker_pane=$(claim_json "$c2" | jq -r .locator.tmux.pane)
claim_is "$c2" '.state == "ready" or .state == "idle"' || fail "the worker $c2 is not live before the close: $(claim_json "$c2" | jq -c '{state}')"
panes | grep -qxF "$worker_pane" || fail "the worker's pane $worker_pane is gone before the close"
closed=$(claims close --json --claim "$c1") || fail "the operator's close of S2-1 through its root claim $c1 was refused"
jq -e '.state == "retired"' <<<"$closed" >/dev/null || fail "the close of S2-1 left its root claim $(jq -c '{state, generation}' <<<"$closed")"
claim_is "$c2" '.state == "retired"' || fail "the close of S2-1 left its worker $c2 $(claim_json "$c2" | jq -c '{state}'): a worker on a closed tree"
until_true 30 "the worker's pane $worker_pane to be gone" pane_gone "$worker_pane"
# The close names a claim, and only a tree's root closes its tree. The worker the close just
# retired is not one: answered as the close that succeeded, it would run the fan-out again from a
# claim that never held the tree. A retired claim is refused where a live one is.
if close_refusal=$(claims close --claim "$c2" 2>&1 >/dev/null); then
  fail "the operator's close of the retired worker $c2 was taken, not refused"
fi
case $close_refusal in
*"409 Conflict: close refused: $c2 is not its tree's root claim; stop it instead"*) ;;
*) fail "the close of the retired worker was refused with '$close_refusal', not the non-root rule" ;;
esac
timeout_hook=
legion stop --config "$work/legion.yaml" >/dev/null
stop_daemon "$daemon_pid" "legion stop"
daemon_pid=
note "the root's stop was refused, generation and pane unchanged ($root_after): $refusal"
note "the root suspended; the operator's stop of the second worker $c3 retired it and its pane $stopped_pane is gone"
note "a second close, naming the retired worker $c2, was refused: $close_refusal"
note "the operator's close of S2-1, with its worker $c2 still live in pane $worker_pane, retired the root and the worker, and the pane is gone; the daemon stopped with exit 0"
pass

begin every-turn-through-the-route
# Every agent turn the run recorded, in every session of the isolated profile, each subagent's
# included, was served by the anthropic provider, the route's; and the same check refuses a copy
# of one captured session with a turn rewritten as Bedrock's.
route=$(bash "$root/scripts/e2e/lib/check-model-route.sh" --sessions "$profile_agent/sessions" \
  --control "$work/model-route-control") || fail "an agent turn left the configured route, or the check proved nothing (the reason is above)"
note "$route"
pass

begin profile-stays-in-the-run
# Every session the run's agents wrote is under the run's own home, inside its work directory, and
# the caller's profile root holds none of the run's profile (make_omp_home, lib/omp-home.sh).
[ -d "$profile_agent/sessions" ] || fail "no agent session directory at $profile_agent/sessions"
sessions=$(find "$profile_agent/sessions" -name '*.jsonl' -type f | wc -l)
[ "$sessions" -gt 0 ] || fail "no agent session under $profile_agent/sessions"
[ ! -e "$HOME/.omp/profiles/$profile" ] || fail "the run wrote the caller's profile root: $HOME/.omp/profiles/$profile exists"
note "$sessions agent session files under $profile_agent/sessions; $HOME/.omp/profiles/$profile does not exist"
pass

ok=1
echo "stage 2 e2e: PASS"
