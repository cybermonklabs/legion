#!/usr/bin/env bash
# The operator-launched controller's live proof on tmux (LEGION-208 Stage 4b, task 4b.5): the Go
# daemon on a real Postgres with this checkout's two plugins in an isolated OMP profile, a real Envoy
# listener and NATS, and `legion controller start` run in real tmux panes as an operator would. It
# checks `legion config check`, the boot gate's contract refusal, `legion state --config`
# running no key command, the operator token file's mode, the controller's registration, role and
# environment, Ctrl-C reaching Oh My Pi, `legion issue status` from an operator shell, a second
# start revoking the first, the controller liveness probe against the listener, and the command's
# exit code. Each check prints `== <name>`, what it observed, and `CHECK <name>: PASS`; the first
# that fails prints `CHECK <name>: FAIL: <why>` and ends the run with exit 1. The agent-harness
# job's tmux-scripts part runs it as a step (.github/workflows/ci-job-agent-harness.yaml).
#
# Run it as `bash scripts/e2e/controller-start-tmux.sh` from a checkout, with two inputs, both
# required and both set by that job: LEGION_TEST_OMP, the Oh My Pi the daemon's boot gate and the
# controller run (the build .omp-pin pins, which .github/actions/install-omp installs), and
# LEGION_TEST_PG_DSN, a loopback PostgreSQL the run creates the daemon's database in. NATS is natsd
# (packages/envoy/internal/testnats/cmd/natsd), built from this checkout. The controller's one
# model turn goes to a provider that listens nowhere (below), so no model is involved, and its gh
# is the runner's own /usr/bin/gh (LEGION_GH_PATH). Everything the run creates is its own and goes
# on any exit: its scratch directory, which holds the isolated OMP profile under the HOME the run
# gives its Oh My Pi processes (make_omp_home, lib/omp-home.sh), the tmux servers, natsd and its
# database. CONTROLLER_START_EVIDENCE_DIR (default a fresh /tmp directory, kept and printed) keeps
# the transcript and the daemon and listener logs.
set -euo pipefail
# This rig's NATS is a throwaway server with no users. nats.go refuses an nkey when the server sends
# no nonce ("nats: nkeys not supported by the server"), so no process here inherits an operator's
# pane seed (NATS_NKEY_SEED, NATS_NKEY_SEED_FILE) or daemon seed (NATS_DAEMON_NKEY_SEED,
# NATS_DAEMON_NKEY_SEED_FILE).
unset NATS_NKEY_SEED NATS_NKEY_SEED_FILE NATS_DAEMON_NKEY_SEED NATS_DAEMON_NKEY_SEED_FILE

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d "/tmp/legion-e2e-controller.$$.XXXXXXXX")
evidence=${CONTROLLER_START_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e-controller-evidence.XXXXXXXX)}
mkdir -p "$evidence/logs" "$evidence/checks"
# Every line of the run also goes to $evidence/transcript.log (lib/transcript.sh), so the driver and
# its cleanup, which stops the panes and drops the database, never fail on a write whoever is
# reading.
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
# Every program the run execs from here on carries its mark, and the sweep in cleanup ends what
# carries it and nothing else (marked_processes, lib/process-tree.sh); a fork of this shell that
# execs nothing carries none and is stopped by its pid (run_marks' comment there). The
# transcript's tee, started above, carries none, so it outlives the sweep and writes the run's last
# line.
export LEGION_E2E_RUN=$work
ok=
daemon_pid=
listener_pid=
daemon_keeper_pid=
natsd_pid=
database=
daemon_port=
envoy_port=
profile=legion-e2e-controller-$$-$(date +%s)
project="CS$$$(date +%s)"
project=${project:0:10}
ptoken=${project,,}
state=$work/state
omp_home=$work/omp-home
profile_agent=$omp_home/.omp/profiles/$profile/agent
ctl_state=$work/controller-state
daemon_log=$evidence/logs/daemon.log
check=setup
timeout_hook=

begin() { check=$1; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "CHECK $check: PASS"; }
fail() {
  echo "CHECK $check: FAIL: $*"
  exit 1
}
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/leftovers.sh
. "$root/scripts/e2e/lib/leftovers.sh"
# shellcheck source-path=SCRIPTDIR source=lib/omp-home.sh
. "$root/scripts/e2e/lib/omp-home.sh"

# Unconditional: every run removes what it made, whatever it ended on.
cleanup() {
  local p
  set +e
  for p in ctl1 ctl2; do TMUX_TMPDIR=$work/tmux tmux -L accept capture-pane -p -t "$p" >"$evidence/checks/$p.pane" 2>/dev/null; done
  TMUX_TMPDIR=$work/tmux tmux -L accept kill-server >/dev/null 2>&1
  stop_pid "$daemon_pid"
  TMUX_TMPDIR=$work/tmux tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1
  stop_pid "$listener_pid"
  stop_pid "$natsd_pid"
  stop_pid "$daemon_keeper_pid"
  stop_marked_processes "$work"
  if [ -n "$database" ]; then
    psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 -c "drop database if exists \"$database\" with (force)" >/dev/null 2>&1 ||
      echo "could not drop database $database; drop it by hand"
  fi
  rm -rf "$work"
  [ -n "$ok" ] || echo "controller start e2e: FAIL (check $check)"
  echo "evidence: $evidence (transcript.log, logs/daemon.log, logs/listener.log, and checks/: each check's own output and the controller panes)"
  return 0
}
trap cleanup EXIT
trap 'echo "CHECK $check: FAIL: line $LINENO exited $?: $BASH_COMMAND"' ERR
trap 'exit 130' INT
trap 'exit 143' TERM

legion() { "$work/legion" "$@"; }
tm() { tmux -L accept "$@"; }
state_json() { legion state --json --port "$daemon_port"; }
envoy_role() { curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$envoy_port/v1/roles/$1"; }
log_count() { jq -R --arg m "$1" 'fromjson? | select(.msg == $m)' "$daemon_log" | jq -s length; }
# controller_omp STATE_DIR CLI_PID: the pid of the Oh My Pi that the run's `legion controller start`,
# the process CLI_PID its pane recorded as it started it (record_pid_sh, lib/process-tree.sh),
# launched for STATE_DIR. The command starts Oh My Pi through `sh -c` (controller.go's exec.Command),
# so it is the process below CLI_PID that runs the Oh My Pi under proof, $omp_exe (launched,
# lib/process-tree.sh), and its
# environment, read once in this shell with no pipe, must name STATE_DIR. Nothing searches the
# machine's /proc for it: there it shares argv[0] with every other Oh My Pi and its own daemon
# broker child, an Oh My Pi run through another name (an omp_invocation may name any program) has
# none, a walk of every entry costs minutes on a loaded devbox, and an environment read through
# `tr | grep -q` was missed whenever grep stopped before tr had written the rest (pipefail's 141).
# It prints why and fails when no such process is there.
controller_omp() {
  local found entry state='' named=''
  local -a entries
  if ! found=$(launched "$2" "$omp_exe"); then
    printf '%s\n' "$found"
    return 1
  fi
  if ! { mapfile -d '' -t entries <"/proc/$found/environ"; } 2>/dev/null || [ "${#entries[@]}" = 0 ]; then
    printf 'the environment of the Oh My Pi %s of %s could not be read (/proc/%s/environ)\n' "$found" "$2" "$found"
    return 1
  fi
  for entry in "${entries[@]}"; do
    case $entry in LEGION_STATE_DIR=*)
      state=${entry#*=} named=1
      break
      ;;
    esac
  done
  if [ -z "$named" ] || [ "$state" != "$1" ]; then
    printf 'the Oh My Pi %s of %s has LEGION_STATE_DIR=%s, want %s\n' "$found" "$2" "${named:+$state}" "$1"
    return 1
  fi
  printf '%s\n' "$found"
}

# ---- setup -----------------------------------------------------------------------------------------
for tool in go jq curl python3 tmux bun psql; do command -v "$tool" >/dev/null || fail "$tool is required"; done
omp=${LEGION_TEST_OMP:-}
[ -n "$omp" ] || fail "LEGION_TEST_OMP is unset: name the Oh My Pi the daemon and the controller run (.github/actions/install-omp exports it in CI)"
[ -x "$omp" ] || fail "LEGION_TEST_OMP names $omp, which is not an executable file"
# The run knows the controller's Oh My Pi by the program the kernel records it runs (controller_omp),
# so LEGION_TEST_OMP names the binary itself, the Oh My Pi under proof, omp_exe (native_program,
# lib/process-tree.sh): a wrapper script's process runs its interpreter and a mise shim's runs the
# version mise picks.
omp_exe=$(native_program "$omp") || fail "LEGION_TEST_OMP: $omp_exe"
# Every `legion` of the run resolves Oh My Pi from LEGION_OMP_PATH, legion.yaml and controller.yaml
# naming no omp_invocation (omplaunch.ResolveInvocation): `legion config check` resolves it too.
export LEGION_OMP_PATH=$omp_exe
# The operator's shell, less the running session's own OMP and Envoy variables (omp_session_unsets,
# lib/omp-home.sh), with the run's own HOME for Oh My Pi and the Oh My Pi under proof as
# LEGION_OMP_PATH, which omp_session_unsets drops from the caller's environment first: operator_env
# runs a command in it, and the daemon starts in it (start_kept).
operator_shell=(env "${omp_session_unsets[@]}" LEGION_OMP_PATH="$omp_exe" HOME="$omp_home" OMP_PROFILE="$profile")
operator_env() { "${operator_shell[@]}" "$@"; }
pg_dsn=${LEGION_TEST_PG_DSN:-}
[ -n "$pg_dsn" ] || fail "LEGION_TEST_PG_DSN is unset: name the loopback PostgreSQL the run creates the daemon's database in (the agent-harness job's service in CI)"
require_loopback_dsn db_host LEGION_TEST_PG_DSN "$pg_dsn" "the run creates and drops a database there" || fail "$db_host"
# The guard reads the DSN alone, and psql would read these three besides it (lib/loopback-dsn.sh).
unset PGHOSTADDR PGSERVICE PGSERVICEFILE
mkdir -p "$state" "$work/xdg" "$work/tmux"
make_omp_home "$omp_home"
export XDG_STATE_HOME=$work/xdg TMUX_TMPDIR=$work/tmux
# The daemon and `legion controller start` refuse a script as the first gh on PATH (a box's own gh
# wrapper can head it), so every process of the run is told the runner's own gh as the real gh.
LEGION_GH_PATH=/usr/bin/gh
[ -x "$LEGION_GH_PATH" ] || fail "$LEGION_GH_PATH, the gh the run names as the real one, is not an executable file"
export LEGION_GH_PATH
# The daemon's port is one the kernel chose, held by a keeper for the run (lib/rig.sh's keep_port),
# so legion.yaml and controller.yaml can name it before the daemon starts on a copy of its socket.
keep_port daemon_keeper daemon_port
(cd "$root/packages/daemon" && go build -o "$work/legion" ./cmd/legion)
(cd "$root/packages/envoy" && go build -o "$work/envoy-listener" ./cmd/listener)
(cd "$root/packages/envoy" && go build -o "$work/natsd" ./internal/testnats/cmd/natsd)
note "legion $("$work/legion" version); Oh My Pi $omp_exe ($("$omp_exe" --version 2>&1 | head -1)); daemon port $daemon_port"

refuse_leftovers legion-e2e-controller
# The daemon's database, the run's own on LEGION_TEST_PG_DSN's server, as a libpq connection
# string (lib/loopback-dsn.sh's run_database_url) that legion.yaml takes as a single-quoted YAML
# scalar, in which a single quote is written twice.
database=controller_start_$$_$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 -c "create database \"$database\"" >/dev/null ||
  fail "could not create database $database on LEGION_TEST_PG_DSN's server"
run_database_url database_url LEGION_TEST_PG_DSN "$pg_dsn" "$database" "the run creates and drops a database there" ||
  fail "$database_url"
dsn=${database_url//\'/\'\'}

# natsd with JetStream, on a port it reports (lib/rig.sh's report_port).
start_process natsd "$work/natsd" -p -1 -store_dir "$work/nats-store"
await_start natsd "$natsd_pid" 60 "natsd to be ready" grep -q 'Server is ready' "$evidence/logs/natsd.log"
nats_port=$(report_port "$evidence/logs/natsd.log" 'Listening for client connections on [^ ]*:') ||
  fail "natsd logged no client port; see $evidence/logs/natsd.log"
nats_url=nats://127.0.0.1:$nats_port
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/envoy-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header")
ENVOY_API_TOKEN="$(cat "$work/envoy-token")" ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID="legion-e2e-controller-$$" NATS_URLS=$nats_url \
  start_held listener envoy_port ENVOY_LISTEN_FD --env 'PORT={port}' -- "$work/envoy-listener"
until_true 60 "the Envoy listener" curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$envoy_port/v1/sessions"
note "listener port $envoy_port"

(cd "$root" && bun install --frozen-lockfile >/dev/null)
envoy_manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --package pi-envoy --profile "$profile" --home "$omp_home" --dest "$work/pi-envoy" --omp-path "$omp_exe")
manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --package pi-legion --profile "$profile" --home "$omp_home" --dest "$work/pi-legion" --omp-path "$omp_exe")
want_contract=$(jq -r .legion.daemonApiVersion "$root/packages/pi-legion/package.json")
note "plugins $(jq -r '.name + "@" + .version' "$envoy_manifest") and $(jq -r '.name + "@" + .version' "$manifest") in OMP profile $profile, daemonApiVersion $want_contract"
# The boot gate resolves the model of every task agent the prompts dispatch, so the profile names
# their roles and the default one model, served by a static-key provider that listens nowhere: the
# controller's one model turn, the start message (LEGION_CONTROLLER_START_MESSAGE) that pi-legion's
# `/legion-controller-start`, the word `legion controller start` puts on Oh My Pi's command line,
# sends once Oh My Pi's startup is over, fails against it, no check reads its answer, and no
# credential the machine carries decides the gate.
mkdir -p "$profile_agent"
cat >"$profile_agent/models.yml" <<'EOF'
providers:
  offline:
    baseUrl: http://127.0.0.1:9
    auth: apiKey
    api: anthropic-messages
    apiKey: static-key
    models:
      - id: m1
        name: M1
EOF
printf 'modelRoles:\n  default: offline/m1\n  review: offline/m1\n  oracle: offline/m1\n  deep: offline/m1\n' >"$profile_agent/config.yml"
# The controller is an interactive Oh My Pi in a pane, which a fresh profile's setup wizard would hold.
omp_setup_wizard_off "$profile_agent/config.yml"

(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/operator-token")
cat >"$work/legion.yaml" <<EOF
project: $project
port: $daemon_port
postgres_dsn: '$dsn'
state_dir: $state
operator_token_file: $work/operator-token
envoy_url: http://127.0.0.1:$envoy_port
nats_urls:
  - $nats_url
envoy_token_file: $work/envoy-token
probe_interval_seconds: 5
# The wake pacing off: this proof starts roles and waits on bounded times, and at the
# default of 8 wakes a minute every wake after the first would spend those waits queueing. The two
# wake keys add no line to legion config check, which prints the renames, the open capability
# rows, the pause line and, under Kubernetes alone, the pod lines (checkConfig in cmd/legion):
# under tmux the pause line names runtime: tmux whatever the pacing is (pause.Unavailable reads
# the runtime first). config-check-passes below compares everything after the OK line with a fixed
# string.
worker_wakes_per_minute: 0
EOF
printf 'Always answer in one sentence.\n' >"$work/instructions.md"
cat >"$work/controller.yaml" <<EOF
project: $project
daemon_url: http://127.0.0.1:$daemon_port
operator_token_file: ./operator-token
envoy_url: http://127.0.0.1:$envoy_port
envoy_token_file: ./envoy-token
nats_urls: [$nats_url]
instructions: ./instructions.md
state_dir: ./controller-state
EOF

# ---- legion config check ---------------------------------------------------------------------------
begin config-check-passes
out=$(legion config check --config "$work/legion.yaml")
# The OK line names the design-gate steps the file resolved to; this file sets no gates key, which
# is the single step spec.
[ "$(head -1 <<<"$out")" = "Config OK: project=$project design-gate-steps=spec" ] || fail "got $out"
# After the OK line, one line per deployment capability the file alone leaves open, in boot's words
# (LEGION-578): under tmux the secrets and resource-limits rows, each ending with the legion.yaml
# line that records a decision. Then the line that says this daemon cannot pause its project and
# names the setting, which under tmux is the runtime (checkConfig in cmd/legion,
# pause.Unavailable). A report, so the exit stayed 0.
want_gaps='capability secrets is open: the tmux runtime enrolls no process with the secrets broker; to record a decision, add to legion.yaml: capabilities.decided.secrets: "<reason>"
capability resource-limits is open: the tmux runtime sets no requests or limits on a pane; to record a decision, add to legion.yaml: capabilities.decided.resource-limits: "<reason>"
pause unavailable: runtime: tmux'
[ "$(tail -n +2 <<<"$out")" = "$want_gaps" ] || fail "the lines after the OK line are not the two open gaps and the pause line: $out"
note "$out"
pass

begin config-check-names-each-broken-key
marker=$work/private-key-command-ran
workflow_tail="dispatch_url: https://dispatch.test
projects:
  $project: {}
github_apps:
  implement: { app_id: \"1\", private_key_command: \"touch $marker; exit 1\" }
  review: { app_id: \"2\", private_key_command: \"touch $marker; exit 1\" }"
variant() { # variant NAME SAYS: the file is $work/variant-NAME.yaml
  local st=0 out
  out=$(legion config check --config "$work/variant-$1.yaml" 2>&1) || st=$?
  [ "$st" = 1 ] || fail "variant $1 exited $st: $out"
  grep -qF "$2" <<<"$out" || fail "variant $1 does not say \"$2\": $out"
  note "variant $1 → $out"
}
{ cat "$work/legion.yaml"; echo "worker_cap: 3"; } >"$work/variant-tossed.yaml"
variant tossed "unknown key worker_cap"
{ cat "$work/legion.yaml"; echo "admission_cap: 0"; } >"$work/variant-cap.yaml"
variant cap "admission_cap must be a positive integer"
sed "s|^envoy_url: .*|envoy_url: not a url|" "$work/legion.yaml" >"$work/variant-envoy.yaml"
variant envoy "envoy_url must be a valid URL"
{ cat "$work/legion.yaml"; echo "$workflow_tail"; } >"$work/variant-dispatch.yaml"
variant dispatch "dispatch_token_file is required when dispatch_url is configured"
[ ! -e "$marker" ] || fail "a private_key_command ran"
note "no private_key_command ran ($marker absent)"
pass

# ---- the boot gate on the plugin's contract --------------------------------------------------------
begin gate-refuses-the-previous-contract
cp -p "$work/pi-legion/package.json" "$work/manifest.orig"
previous_contract=$((want_contract - 1))
jq --argjson c "$previous_contract" '.legion.daemonApiVersion = $c' "$work/manifest.orig" >"$work/pi-legion/package.json"
st=0
operator_env timeout 300 "$work/legion" start --config "$work/legion.yaml" >"$evidence/checks/refusal-contract.log" 2>&1 || st=$?
cp -p "$work/manifest.orig" "$work/pi-legion/package.json"
[ "$st" != 0 ] && [ "$st" != 124 ] || fail "legion start exited $st"
grep -qF "speaks daemon API contract $previous_contract; this daemon requires $want_contract" "$evidence/checks/refusal-contract.log" ||
  fail "the refusal does not name both contracts: $(head -3 "$evidence/checks/refusal-contract.log")"
note "legion start exit $st: $(grep -oF "speaks daemon API contract $previous_contract; this daemon requires $want_contract" "$evidence/checks/refusal-contract.log" | head -1)"
pass

# ---- the daemon ------------------------------------------------------------------------------------
begin daemon-serves
start_kept daemon daemon_keeper LEGION_LISTEN_FD -- "${operator_shell[@]}" "$work/legion" start --config "$work/legion.yaml"
until_true 180 "the daemon to answer /healthz" curl -fs --max-time 5 "http://127.0.0.1:$daemon_port/healthz"
state_json | jq -e '.controllerLocator == null' >/dev/null || fail "a controller is recorded before any start"
note "healthy; no controllerLocator yet"
pass

begin state-config-runs-no-key-command
{ cat "$work/legion.yaml"; echo "dispatch_token_file: $work/absent-dispatch-token"; echo "$workflow_tail"; } >"$work/key-command.yaml"
got=$(legion state --json --config "$work/key-command.yaml" | jq -r .daemon.project)
[ "$got" = "$project" ] || fail "legion state --config read $got"
[ ! -e "$marker" ] || fail "legion state --config ran a private_key_command"
note "legion state --config read project $got; $marker absent"
pass

# ---- legion controller start ----------------------------------------------------------------------
begin operator-token-file-others-can-read-is-refused
cp "$work/operator-token" "$work/operator-token-0640" && chmod 0640 "$work/operator-token-0640"
sed 's|^operator_token_file: .*|operator_token_file: ./operator-token-0640|' "$work/controller.yaml" >"$work/controller-0640.yaml"
st=0
out=$(operator_env "$work/legion" controller start --config "$work/controller-0640.yaml" 2>&1 </dev/null) || st=$?
[ "$st" = 1 ] || fail "exit $st: $out"
grep -qF "operator_token_file $work/operator-token-0640 is readable by its group or others (mode 0640); chmod 0600 it" <<<"$out" || fail "$out"
[ "$(log_count "api: minted a controller capability; the previous controller's registration and grants are revoked")" = 0 ] ||
  fail "the daemon minted a capability for a refused start"
[ ! -e "$ctl_state" ] || fail "the refused start wrote $ctl_state"
note "exit 1: $out"
note "the daemon minted nothing; $ctl_state not created"
pass

begin controller-claims-the-role
# The pane's command is an operator's shell line: the run's own HOME and profile, the Oh My Pi under
# proof as LEGION_OMP_PATH, which omp_session_unsets drops first, and OMP_SKIP_SETUP=1, which keeps
# the pinned Oh My Pi's first-run setup from holding the interactive session, as
# omp_setup_wizard_off's profile line does for a build that reads it (packages/pi-shared/test/
# omp-harness.ts's spawnInteractive sets the same). It records the pid of the
# `legion controller start` it runs (record_pid_sh, lib/process-tree.sh), which execs in that
# process, so the run knows the command by the pid it started.
tm new-session -d -s ctl1 -x 200 -y 50 "cd '$work' && sh -c '$record_pid_sh' record-pid '$evidence/checks/ctl1.pid' env ${omp_session_unsets[*]} HOME='$omp_home' OMP_PROFILE='$profile' OMP_SKIP_SETUP=1 LEGION_OMP_PATH='$omp_exe' '$work/legion' controller start --config '$work/controller.yaml' 2>'$evidence/checks/ctl1.stderr'; echo \$? >'$evidence/checks/ctl1.exit'; sleep 600"
until_true 180 "controllerLocator in the state" sh -c "'$work/legion' state --json --port $daemon_port | jq -e '.controllerLocator.sessionId != null'"
locator1=$(state_json | jq -c .controllerLocator)
session1=$(jq -r .sessionId <<<"$locator1")
jq -e --arg s "$session1" '.runtime == "tmux" and .external == true and .sessionId == $s and (.registeredAt | length > 0)' <<<"$locator1" >/dev/null ||
  fail "controllerLocator = $locator1"
note "GET /legion/v1/state .controllerLocator = $locator1"
role=legion-$ptoken-controller
until_true 60 "the Envoy role $role to name $session1" sh -c "curl -fsS -H '@$work/envoy-auth-header' 'http://127.0.0.1:$envoy_port/v1/roles/$role' | jq -e --arg s '$session1' '.holder == \$s'"
note "GET /v1/roles/$role → holder $(envoy_role "$role" | jq -r .holder)"
registered=$(jq -R -c --arg s "$session1" 'fromjson? | select(.msg == "api: controller registered" and .session == $s) | {claim, generation, session, pluginContract}' "$daemon_log" | head -1)
[ -n "$registered" ] || fail "the daemon logged no controller registration for $session1"
note "daemon: api: controller registered $registered"
cli1=$(recorded_pid "$evidence/checks/ctl1.pid" "$work/legion") ||
  fail "$evidence/checks/ctl1.pid names no process running $work/legion: $(cat "$evidence/checks/ctl1.pid" 2>&1)"
omp1=$(controller_omp "$ctl_state" "$cli1") || fail "legion controller start ($cli1) runs no Oh My Pi for $ctl_state: $omp1"
# Its environment and its command line are read with no pipe between the read and the check
# (env_of, env_has, process_args).
env_of "$omp1" HOME >/dev/null || fail "omp $omp1's environment could not be read"
for pair in LEGION_CONTROLLER=1 LEGION_ROLE=controller "LEGION_PROJECT=$ptoken" \
  "LEGION_CONTROLLER_SECRET_FILE=$ctl_state/secrets/$role" \
  "ENVOY_TOKEN_FILE=$work/envoy-token" "ENVOY_NATS_URL=$nats_url" "HOME=$omp_home" "OMP_PROFILE=$profile"; do
  [ "$(env_of "$omp1" "${pair%%=*}")" = "${pair#*=}" ] || fail "omp $omp1 has ${pair%%=*}=$(env_of "$omp1" "${pair%%=*}"), want ${pair#*=}"
done
# No grant file: the controller's `legion` tool (read_state, set_status) mints its grants
# in-process, and no agent runs `legion` from bash.
for absent in LEGION_GRANT_FILE LEGION_GRANT; do
  st=0
  env_has "$omp1" "$absent" || st=$?
  [ "$st" = 1 ] || fail "omp $omp1 carries $absent=$(env_of "$omp1" "$absent") (env_has status $st)"
done
[[ $(env_of "$omp1" PI_SHELL_PREFIX) == *"PATH='$ctl_state/bin'\${__legion_path"* ]] || fail "PI_SHELL_PREFIX = $(env_of "$omp1" PI_SHELL_PREFIX)"
case "$(env_of "$omp1" PATH)" in
  "$ctl_state/bin:"*) ;;
  *) fail "PATH = $(env_of "$omp1" PATH), want the controller's legion launcher directory first" ;;
esac
case ":$(env_of "$omp1" PATH):" in
  *worker-bin*) fail "PATH = $(env_of "$omp1" PATH) names a worker-bin directory; no daemon installs a gh shim any more" ;;
esac
[ -z "$(env_of "$omp1" LEGION_CONTROLLER_SECRET)" ] || fail "the controller secret's value is in omp's environment"
omp_args=()
process_args "$omp1" omp_args || fail "omp $omp1's command line could not be read"
prompted=
for arg in "${omp_args[@]}"; do
  case $arg in
    --append-system-prompt) prompted=1 ;;
    --mode | rpc | --resume*) fail "omp runs --mode rpc or --resume: ${omp_args[*]}" ;;
  esac
done
[ -n "$prompted" ] || fail "omp has no --append-system-prompt: ${omp_args[*]}"
[ "$(stat -c %a "$ctl_state/secrets/$role")" = 600 ] || fail "the secret file is not 0600"
note "omp $omp1: LEGION_CONTROLLER=1 PI_SHELL_PREFIX over $ctl_state; secret only as a 0600 file; interactive (no --mode rpc); HOME=$omp_home"
# The profile Oh My Pi runs on is the run's own, inside its work directory (its plugin links and the
# log each Oh My Pi start writes are there), and the operator's profile root holds none of it.
ls "$omp_home/.omp/profiles/$profile/logs"/omp.*.log >/dev/null 2>&1 ||
  fail "no Oh My Pi log under $omp_home/.omp/profiles/$profile"
[ ! -e "$HOME/.omp/profiles/$profile" ] || fail "the run wrote the operator's profile root: $HOME/.omp/profiles/$profile exists"
note "the profile is $omp_home/.omp/profiles/$profile ($(cd "$omp_home/.omp/profiles/$profile" && printf '%s ' *)); $HOME/.omp/profiles/$profile does not exist"
grep -qF "[legion] the daemon has no GitHub App to act as; the controller's gh acts as nobody" "$evidence/checks/ctl1.stderr" ||
  fail "ctl1.stderr does not say the controller's gh acts as nobody: $(cat "$evidence/checks/ctl1.stderr")"
[ -d "$ctl_state/gh" ] || fail "$ctl_state/gh does not exist"
[ "$(stat -c %a "$ctl_state/gh")" = 700 ] || fail "$ctl_state/gh is not mode 0700: $(stat -c %a "$ctl_state/gh")"
[ -z "$(ls -A "$ctl_state/gh")" ] || fail "$ctl_state/gh holds $(find "$ctl_state/gh" -mindepth 1 -maxdepth 1 -printf '%f ')with no GitHub App configured, want nothing"
[ ! -e "$ctl_state/github-credential.log" ] || fail "$ctl_state/github-credential.log exists with no GitHub App configured"
note "no GitHub App: ctl1.stderr says the controller's gh acts as nobody; $ctl_state/gh is an empty 0700 directory and no credential log"
pass

begin ctrl-c-reaches-omp-not-the-cli
cli1=$(recorded_pid "$evidence/checks/ctl1.pid" "$work/legion") || fail "no legion controller start process"
tm send-keys -t ctl1 C-c
sleep 2
recorded_pid "$evidence/checks/ctl1.pid" "$work/legion" >/dev/null || fail "one Ctrl-C ended legion controller start"
kill -0 "$omp1" 2>/dev/null || fail "one Ctrl-C ended Oh My Pi"
[ ! -e "$evidence/checks/ctl1.exit" ] || fail "the command exited on one Ctrl-C"
note "after one Ctrl-C both legion ($cli1) and omp ($omp1) run; Oh My Pi owns the terminal"
pass

begin status-from-an-operator-shell
st=0
out=$(legion issue status "$project-1" backlog --operator-token-file "$work/operator-token" --port "$daemon_port" 2>&1) || st=$?
# This rig has no Dispatch, so the route that took the minted grant answers 500 naming it: the
# bearer minted a grant and the status request reached the Dispatch client. The status write
# itself is the whole-tree kind test's (TestStage4bSandboxTreeOnTheKindRigLive), against a real Dispatch.
grep -qF 'the daemon answered 500 Internal Server Error: Dispatch is unavailable' <<<"$out" || fail "exit $st: $out"
note "operator bearer → controller grant → status route reached Dispatch (this rig has none): exit $st: $out"
printf 'not-the-operator-token\n' >"$work/wrong-token" && chmod 600 "$work/wrong-token"
st=0
out=$(legion issue status "$project-1" backlog --operator-token-file "$work/wrong-token" --port "$daemon_port" 2>&1) || st=$?
if [ "$st" != 1 ] || ! grep -qF 'the daemon answered 403 Forbidden: Invalid operator token' <<<"$out"; then fail "wrong token: exit $st: $out"; fi
note "wrong bearer: exit $st: $out"
pass

begin second-start-revokes-the-first
cap1=$(cat "$ctl_state/secrets/$role")
grant_before=$(printf 'Authorization: Bearer %s' "$(cat "$work/operator-token")" |
  curl -fsS -X POST -H @- -H 'Content-Type: application/json' \
    --data '{}' "http://127.0.0.1:$daemon_port/legion/v1/grants" | jq -r .grantId)
# The grant's positive control: before the second start it redeems, reaching the status route's
# missing Dispatch, so the route takes issue X-1 past the grant check.
redeem_status() {
  curl -sS -o "$evidence/checks/$1" -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    --data "$(jq -cn --arg g "$grant_before" '{grantId: $g, issue: "X-1", status: "backlog"}')" "http://127.0.0.1:$daemon_port/legion/v1/issues/status"
}
code=$(redeem_status grant-before-second-start.json) || fail "the status route could not be reached: $code"
if [ "$code" != 500 ] || ! grep -qF DISPATCH_UNAVAILABLE "$evidence/checks/grant-before-second-start.json"; then
  fail "the grant answered $code $(cat "$evidence/checks/grant-before-second-start.json") before the second start, want 500 DISPATCH_UNAVAILABLE"
fi
note "the grant, before the second start → $code $(cat "$evidence/checks/grant-before-second-start.json")"
tm new-session -d -s ctl2 -x 200 -y 50 "cd '$work' && sh -c '$record_pid_sh' record-pid '$evidence/checks/ctl2.pid' env ${omp_session_unsets[*]} HOME='$omp_home' OMP_PROFILE='$profile' OMP_SKIP_SETUP=1 LEGION_OMP_PATH='$omp_exe' '$work/legion' controller start --config '$work/controller.yaml' 2>'$evidence/checks/ctl2.stderr'; echo \$? >'$evidence/checks/ctl2.exit'; sleep 600"
until_true 180 "controllerLocator to name a second session" sh -c "'$work/legion' state --json --port $daemon_port | jq -e --arg s '$session1' '.controllerLocator.sessionId != null and .controllerLocator.sessionId != \$s'"
locator2=$(state_json | jq -c .controllerLocator)
session2=$(jq -r .sessionId <<<"$locator2")
note "GET /legion/v1/state .controllerLocator = $locator2"
until_true 60 "the Envoy role to move to $session2" sh -c "curl -fsS -H '@$work/envoy-auth-header' 'http://127.0.0.1:$envoy_port/v1/roles/$role' | jq -e --arg s '$session2' '.holder == \$s'"
note "GET /v1/roles/$role → holder $session2"
mints=$(jq -R -c 'fromjson? | select(.msg | startswith("api: minted a controller capability")) | .generation' "$daemon_log" | tr '\n' ' ')
[ "$mints" = "1 2 " ] || fail "capability mints logged: $mints"
note "daemon minted generations: $mints"
body=$(jq -cn --arg t "$cap1" --arg s "$session1" --argjson c "$want_contract" '{bootToken: $t, sessionId: $s, ompSessionFile: "/x.jsonl", agentId: "a", pluginContract: $c}')
code=$(curl -s -o "$evidence/checks/reregister.json" -w '%{http_code}' -X POST -H 'Content-Type: application/json' --data "$body" "http://127.0.0.1:$daemon_port/legion/v1/claims/register")
[ "$code" = 403 ] || fail "registering with the first capability answered $code $(cat "$evidence/checks/reregister.json")"
note "POST /claims/register with the first capability → $code $(cat "$evidence/checks/reregister.json")"
code=$(redeem_status stale-grant.json) || fail "the status route could not be reached: $code"
if [ "$code" != 403 ] || ! grep -qF GRANT_UNAVAILABLE "$evidence/checks/stale-grant.json"; then
  fail "the same grant answered $code $(cat "$evidence/checks/stale-grant.json") after the second start, want 403 GRANT_UNAVAILABLE"
fi
note "the same grant, after the second start → $code $(cat "$evidence/checks/stale-grant.json")"
pass

begin liveness-probe-against-the-listener
# The daemon's own prober (lib/controller-liveprobe.go), run as a main inside packages/daemon.
printf '{"Replace":{"%s":"%s"}}' "$root/packages/daemon/cmd/liveprobe-accept/main.go" "$root/scripts/e2e/lib/controller-liveprobe.go" >"$work/overlay.json"
probe_args=("http://127.0.0.1:$envoy_port" "$work/envoy-token" "$ptoken" "$session2" "$session1")
verdicts=$(cd "$root/packages/daemon" && go run -overlay "$work/overlay.json" ./cmd/liveprobe-accept "${probe_args[@]}" 2>"$evidence/checks/liveprobe.log" | tr '\n' ' ')
[ "$verdicts" = "$session2=live $session1=gone " ] || fail "verdicts: $verdicts; log $(cat "$evidence/checks/liveprobe.log")"
note "controller.Prober on the live listener: $verdicts"
note "$(grep -o 'controller liveness: .*' "$evidence/checks/liveprobe.log" | head -1)"
pass

begin exit-code-is-oh-my-pis
# Oh My Pi: "press Ctrl+C again to exit, or Ctrl+D" — Ctrl+D on an empty editor quits.
tm send-keys -t ctl2 C-d
until_true 30 "legion controller start to exit after Oh My Pi" test -s "$evidence/checks/ctl2.exit"
[ "$(cat "$evidence/checks/ctl2.exit")" = 0 ] ||
  fail "legion controller start exited $(cat "$evidence/checks/ctl2.exit") after Oh My Pi quit cleanly, want 0; stderr: $(cat "$evidence/checks/ctl2.stderr")"
note "the second controller's legion controller start exited 0 after Oh My Pi quit cleanly; stderr: $(cat "$evidence/checks/ctl2.stderr")"
pass

ok=1
echo "controller start e2e: PASS"
