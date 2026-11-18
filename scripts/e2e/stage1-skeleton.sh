#!/usr/bin/env bash
# Stage 1's gate for the Go coordinator: it boots against a real Postgres, answers /healthz,
# /readyz and GET /legion/v1/state, registers itself, survives a restart against the same store
# with its first boot time intact, and refuses an unreachable Postgres by the host it could not
# reach and never by the password. Every step is fatal — no `|| true` outside the cleanup a trap
# must never fail on — and the run leaves no container, no daemon, and nothing in the real state
# home.
#
# Everything it takes is this run's own: its work directory, its container name, its project key
# and its port. Two runs on one box (a CI job and a devbox session, or two sessions) do not
# collide, and neither reports the other as a leftover.
#
# The daemon supervises its agents under tmux, so it refuses to start without what a launch
# needs: tmux on PATH, an operator token file for the spawn surface, and an OMP to run, which its
# plugin gate holds to this daemon's contract before anything else. This proof launches no agent,
# so the OMP it names is a stub that answers the gate's load probe and nothing else, under a home
# of the run's own that holds the checkout's plugin manifest where the gate reads it.
#
# The daemon's store is a database of the run's own, made with psql on the server
# LEGION_E2E_PG_DSN names (CI's service), or on the container the run starts when it names none,
# and dropped by the cleanup. The DSN must name a loopback host, which lib/loopback-dsn.py asks
# libpq for, so the run needs psql and python3 with libpq beside tmux. The daemon is handed that
# database in LEGION_POSTGRES_DSN, in its own environment alone: legion.yaml names no
# postgres_dsn, and no variable of this shell holds the URI. The run's own container listens on a
# unix socket in a directory of the run's own and publishes no port (start_postgres_container,
# lib/rig.sh). Its guard is proof_guard's below, and every wait goes through lib/rig.sh's
# wait_for, which gives up 30 s before it.
set -euo pipefail

pid=
# lib/rig.sh's start_process (through start_daemon below) and keep_port set these: the daemon's
# pid, copied to pid on each start, and its port keeper's.
daemon_pid=
daemon_keeper_pid=
ok=
container=legion-e2e-pg-$$
# The server the run makes its database on, and that database, once made: cleanup drops it. The
# run's own container's socket directory, once it has one: cleanup removes it after the container.
pg_server=
database=
pg_sockets=
work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e.XXXXXXXX")
# lib/rig.sh's process helpers log each service to $evidence/logs/<name>.log.
evidence=$work
timeout_hook=
mkdir -p "$work/logs"
# cleanup never returns non-zero: a trap that fails under set -e would mask the real status. The
# work directory survives a failure — its state documents, refusal log and daemon log are the
# evidence, and the daemon log is printed — and goes when the run passed.
cleanup() {
  if [ -n "${pid:-}" ]; then kill -TERM "$pid" 2>/dev/null || true; fi
  if [ -n "${daemon_keeper_pid:-}" ]; then kill -TERM "$daemon_keeper_pid" 2>/dev/null || true; fi
  if [ -n "${database:-}" ]; then
    psql "$pg_server" -X -q -v ON_ERROR_STOP=1 -c "drop database if exists \"$database\" with (force)" >/dev/null 2>&1 ||
      echo "could not drop database $database; drop it by hand"
  fi
  docker rm -f -v "$container" >/dev/null 2>&1 || true
  [ -z "${pg_sockets:-}" ] || rm -rf "$pg_sockets" || true
  if [ -n "${ok:-}" ]; then
    rm -rf "$work"
  else
    if [ -s "$work/logs/daemon.log" ]; then
      echo "--- the daemon's log ($work/logs/daemon.log)"
      cat "$work/logs/daemon.log" || true
    fi
    echo "the run's workspace is $work"
  fi
  return 0
}

# daemon_exited is whether the daemon ($pid) has exited, for stop_daemon's wait_for.
daemon_exited() {
  kill -0 "$pid" 2>/dev/null || return 0
  echo "the daemon ($pid) is still running"
  return 1
}
# kill_daemon is stop_daemon's timeout_hook: SIGKILL on the way out of a wait the daemon never
# ended, since a leaked daemon holds the port and, under $(...) or a pipe, the caller.
kill_daemon() {
  kill -KILL "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  pid=
}
# stop_daemon: $1 is how it was asked to stop. It waits for the exit through wait_for, so a daemon
# that ignores the stop fails the run by name at the guard's deadline, killed on the way out, and
# then reads and judges the exit status (daemon.Run returns 0 on a cancelled context, so a
# non-zero status is a defect, not a stop).
stop_daemon() {
  timeout_hook=kill_daemon
  wait_for "the daemon ($pid) to exit on $1" daemon_exited
  timeout_hook=
  st=0
  wait "$pid" || st=$?
  [ "$st" = 0 ] || {
    echo "daemon exited $st on $1"
    exit 1
  }
  pid=
}

root=$(cd "$(dirname "$0")/../.." && pwd)
trap cleanup EXIT
mkdir -p "$work/state" "$work/xdg" "$work/home"
# The daemon runs from here, so mise resolves its tmux from here too, whatever directory the run
# was started from.
cd "$root/packages/daemon"
# lib/rig.sh's real_bin_path calls it inside $(…), which would swallow a message on stdout.
fail() {
  echo "$*" >&2
  exit 1
}
note() { echo "   $*"; }
# healthz_answered [SUFFIX] is the daemon's ($pid) answer to /healthz on $port, for wait_for: a
# daemon that exits first ends the wait at once, naming its exit status, rather than at the
# guard's deadline. Each poll has a timeout: the port is kept (below), so a daemon that is not
# serving yet leaves a request queued, unanswered, instead of refused.
healthz_answered() {
  local status=0
  curl -fsS --max-time 2 "http://127.0.0.1:$port/healthz" >/dev/null && return 0
  kill -0 "$pid" 2>/dev/null && return 1
  wait "$pid" || status=$?
  pid= # reaped: cleanup must not signal a number the kernel may reuse
  wait_impossible="the daemon exited ($status) before it answered"
  return 1
}
# wait_healthz [SUFFIX] waits for the daemon to answer /healthz.
wait_healthz() {
  wait_for "the daemon to answer /healthz${1:-}" healthz_answered
}
# held_by_daemon_and_keeper: after a healthy start, the socket the daemon serves on is held by the
# daemon and the keeper alone. The daemon adopts it close-on-exec, so no process it starts (its
# tmux server, a pane) inherits it, and no `legion` CLI call holds it; a process that did would keep
# the port listening, unanswered, after the daemon stops (lib/rig.sh's held_only_by).
held_by_daemon_and_keeper() {
  held_only_by "$port" "$pid" "$daemon_keeper_pid" ||
    fail "the socket the daemon serves on port $port is not held by exactly the daemon ($pid) and its keeper ($daemon_keeper_pid)${1:-}"
}
# readyz_200 [SUFFIX] waits for GET /readyz on the daemon ($pid) on $port to answer 200, the answer
# the daemon's one-way serving latch gives from the moment it serves: the boot built its runtime,
# installed its launcher acceptor and began serving (the daemon's servingLatch). While the boot
# runs the daemon answers nothing, because the API is served only once the boot is done; from the
# stop on, a connection the server still holds through its drain gets 503 `stopping`; and nothing
# is read out of the database on the way.
# It waits rather than reading once, because the daemon sets the latch on a goroutine of its own
# that starts when the serving goroutine does: /healthz can be answered in the instant before that
# goroutine runs, when /readyz still answers 503 `starting`, and a single read would fail a daemon
# that is about to say so. A daemon that answers /healthz and never latches still fails here, at
# the guard's deadline, which is what this proves.
# The status is read off curl's stdout rather than asserted by curl's -f, so the failure names what
# it got, and the body with it, which says whether the daemon called itself starting or stopping; a
# connection curl never made writes no body and exits non-zero, which the run names 000 and fails
# on the same way.
readyz_answered_200() {
  local status
  : >"$work/readyz.json" # curl writes nothing here when it never connected, and this is read below
  status=$(curl -sS -o "$work/readyz.json" -w '%{http_code}' --max-time 2 \
    "http://127.0.0.1:$port/readyz") || status=000
  [ "$status" = 200 ] && return 0
  echo "GET /readyz answered $status $(cat "$work/readyz.json"), want 200"
  return 1
}
readyz_200() {
  wait_for "GET /readyz${1:-} to answer 200" readyz_answered_200
}
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# Twice the longest green run on record, rounded up to the minute: e2e.yaml's 24 green jobs up to
# 2026-11-15, the longest proof step 111 s (run 43611577738), against ci.yaml's Stage 1 step, which
# builds from a warm Go cache and took at most 7 s in 140 green test jobs (run 43570026107).
proof_guard 240
# A devbox's first tmux can be a wrapper script or a mise shim. mise runs it from the daemon's
# working directory, under the daemon's tmux environment, which moves the XDG directories under the
# state directory; this run's own HOME (pane_home, below) has the same effect. From a working
# directory under the operator's real HOME, mise then finds that HOME's config as a parent
# directory's config with no trust record, so it exits 1. The daemon's boot scrubs its private tmux
# server's environment (`tmux show-environment`) and reads that exit as a failure, not as no
# server, so boot refuses.
# So when the first tmux is a script or mise itself, the run's PATH, and so the daemon's, starts
# with the real binary (lib/rig.sh, real_bin_path), which fails loudly when mise cannot name it.
# CI's tmux is the runner's apt binary, which this leaves alone. mise reads which configs are
# trusted under XDG_STATE_HOME, so this runs before the run moves that.
first_tmux=$(command -v tmux || true)
mise_bin=$(command -v mise || true)
if [ -n "$first_tmux" ] && {
  [ "$(head -c 2 "$first_tmux")" = "#!" ] || {
    [ -n "$mise_bin" ] && [ "$(readlink -f "$first_tmux")" = "$(readlink -f "$mise_bin")" ]
  }
}; then
  PATH="$(real_bin_path "$work/realbin" tmux):$PATH"
fi
for tool in tmux psql; do
  command -v "$tool" >/dev/null || {
    case $tool in
      tmux) echo "tmux is required: the Go daemon supervises under tmux and refuses to start without it" ;;
      psql) echo "psql is required: the run makes the daemon's database of its own with it" ;;
    esac
    exit 1
  }
done
export XDG_STATE_HOME="$work/xdg" # the registry lands here, never in the devbox's real one
project="E2E$$$(date +%s)"        # daemon_boot counts per project; a fresh key makes boots==1 true on any store
# A port this run holds alone, so a daemon of another run is never mistaken for this one's: a keeper
# holds a socket on a port the kernel chose for the whole run (lib/rig.sh's keep_port), legion.yaml
# names that port, and each start serves on a copy of the socket (LEGION_LISTEN_FD), so the restart
# serves the same port and nothing else can take it in between.
keep_port daemon_keeper port
go build -o "$work/legion" ./cmd/legion
bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/legion" | sed 's/^/   /'

# CI passes its service's DSN; the devbox brings its own container, on a unix socket in a
# directory of the run's own. Either way the daemon's store is a database the run makes there, so
# nothing the run writes lands in a database the caller's DSN names.
if [ -n "${LEGION_E2E_PG_DSN:-}" ]; then
  pg_server=$LEGION_E2E_PG_DSN
else
  docker ps >/dev/null # a broken docker is a failure of this run, not of the daemon
  # ghcr.io/cybermonklabs/legion/ci, the copy of PostgreSQL 18 (.github/ci-images.yaml), the major
  # version CI's service and Cloud SQL run
  pg_password=legion # the scratch container's own login, which nothing outside this run reaches
  start_postgres_container "$container" pg_sockets --cpus 2 --memory 2g --mount type=tmpfs,destination=/var/lib/postgresql \
    -e POSTGRES_USER=legion -e POSTGRES_PASSWORD="$pg_password" \
    -e POSTGRES_DB=legion -- ghcr.io/cybermonklabs/legion/ci@sha256:979bccfde35445db618c65107d27d08c6c21339d8dc3ad4c830c6633947be0e4
  # Its second readiness line: the entrypoint's bootstrap server answers on the same socket first,
  # then stops, and a daemon that connected to it would be reset.
  wait_for "postgres in $container to be ready" postgres_container_ready "$container"
  pg_server="postgres://legion:${pg_password}@/legion?host=$pg_sockets"
fi
# lib/loopback-dsn.sh's require_loopback_dsn says why these three go before the first connection.
unset PGHOSTADDR PGSERVICE PGSERVICEFILE
run_db=stage1_$$_$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
# The URI form: the settings check below reads a DSN that leaks into the state by its
# `postgres://`, and that is the form the daemon is given, in LEGION_POSTGRES_DSN (with_store
# below). A caller's DSN goes through the guard, which reads it with libpq and refuses any host
# but a loopback one, here, before the database is made; the run's own container's is a socket
# directory the run made, which the guard would refuse as no loopback host, so the run writes that
# URI itself.
refusal='' # require_run_database assigns it with printf -v: the refusal line, or nothing
if [ -z "$pg_sockets" ]; then
  require_run_database refusal LEGION_E2E_PG_DSN "$pg_server" "$run_db" \
    "the run creates and drops a database there" --uri || fail "$refusal"
fi
psql "$pg_server" -X -q -v ON_ERROR_STOP=1 -c "create database \"$run_db\"" >/dev/null ||
  fail "could not create database $run_db on the run's PostgreSQL server"
database=$run_db
note "the daemon's store is the run's own database $database"

# with_store COMMAND... runs COMMAND, a legion command that reads legion.yaml, in place of the
# subshell that calls it, with LEGION_POSTGRES_DSN naming the run's own database as a URI. The
# file names no postgres_dsn, so the loader takes that variable
# (packages/daemon/internal/config/config.go), and every command that loads the file needs it:
# the daemon, and `legion stop`. A caller's DSN reaches the daemon through lib/loopback-dsn.sh's
# exec_run_database, so the URI is in no variable of this shell, no argument list and no file;
# the run's own container's is the one the run writes, as an assignment. Run it in ( … ) or in
# the background.
with_store() {
  if [ -n "$pg_sockets" ]; then
    LEGION_POSTGRES_DSN="postgres://legion:${pg_password}@/$run_db?host=$pg_sockets" exec "$@"
  fi
  exec_run_database LEGION_E2E_PG_DSN "$pg_server" "$run_db" \
    "the run creates and drops a database there" --uri --env LEGION_POSTGRES_DSN -- "$@"
}
# start_daemon starts the daemon as the service `daemon` on a copy of the socket daemon_keeper
# holds, as lib/rig.sh's start_kept starts one, and copies its pid to pid. with_store runs first
# and held-port.py after it, where start_kept would run held-port.py first: the DSN reaches the
# helper in the environment of the subshell start_process runs. The first start and the restart
# both run it, so each daemon is handed its store the same way, and every exec keeps the pid, so
# pid is the daemon's own.
start_daemon() {
  start_process daemon with_store python3 "$root/scripts/e2e/lib/held-port.py" exec \
    --from "$work/daemon_keeper.sock" --fd-env LEGION_LISTEN_FD -- \
    "${pane_home[@]}" "$work/legion" start --config "$work/legion.yaml"
  pid=$daemon_pid
}

# The operator bearer, as the 0600 file operator_token_file names, and the OMP the daemon resolves
# at boot (LEGION_OMP_PATH): a stub that answers the plugin gate's load probe (`omp models
# --extension <probe> --json`) as a pane that loads pi-legion beside pi-envoy does — pi-legion
# loaded, from the package whose manifest the gate read, speaking Envoy plugin interface 1, and
# pi-envoy publishing interface 1 from a file an @sjawhar/pi-envoy manifest owns (the gate holds
# the Envoy entry's file to a manifest of that name) — says why anything else should never have
# run, and fails it. The gate's contract probe reads the pi-legion manifest where Oh My Pi finds it
# under the pane's HOME and default profile: the daemon runs with HOME=$work/home and neither
# profile variable, and the manifest there is the checkout's own, so it declares the contract this
# checkout's daemon requires. The gate also asks the load probe for the task agents and skills the
# role prompts name (LEGION_PROMPT_AGENTS, LEGION_PROMPT_SKILLS) and whether each of those agents'
# models resolves (LEGION_AGENT_MODELS), and the stub answers that they all resolve.
(umask 077 && printf 'stage1-operator-%s\n' "$project" >"$work/operator-token")
cat >"$work/omp" <<'EOF'
#!/bin/sh
if [ "$1" = models ]; then
  echo LEGION_PLUGIN_LOADED=yes >&2
  echo "LEGION_PLUGIN_LOADED_FROM=file://$HOME/.omp/plugins/node_modules/@sjawhar/pi-legion/dist/legion.js" >&2
  echo LEGION_PLUGIN_ENVOY_INTERFACE=1 >&2
  echo LEGION_ENVOY_INTERFACE=1 >&2
  echo "LEGION_ENVOY_LOADED_FROM=file://$HOME/.omp/plugins/node_modules/@sjawhar/pi-envoy/dist/envoy.js" >&2
  [ -n "${LEGION_PROMPT_AGENTS:-}" ] && echo LEGION_PROMPT_AGENTS=resolved >&2
  [ -n "${LEGION_PROMPT_AGENTS:-}" ] && [ -z "${LEGION_SKIP_AGENT_MODELS:-}" ] && echo LEGION_AGENT_MODELS=resolved >&2
  [ -n "${LEGION_PROMPT_SKILLS:-}" ] && echo LEGION_PROMPT_SKILLS=resolved >&2
  exit 0
fi
echo "stage 1 launches no agent" >&2
exit 1
EOF
chmod 0755 "$work/omp"
export LEGION_OMP_PATH="$work/omp"
plugins="$work/home/.omp/plugins/node_modules/@sjawhar"
mkdir -p "$plugins/pi-legion" "$plugins/pi-envoy"
jq '{name, version, legion}' "$root/packages/pi-legion/package.json" >"$plugins/pi-legion/package.json"
jq '{name, version}' "$root/packages/pi-envoy/package.json" >"$plugins/pi-envoy/package.json"
pane_home=(env -u OMP_PROFILE -u PI_PROFILE HOME="$work/home")

# Beyond what boot needs, the file plants what the state's `settings` must show and must not
# (LEGION-791): a review_round_cap the default would not answer, a github_apps block whose
# inline PEM fixture is the App key the state must never print, and an envoy_url carrying userinfo
# at the default host and port, the credential the settings must mask. The loader takes github_apps
# as the workflow configured and then wants projects too (resolveStage3: `projects must declare at
# least one project`, `github_apps is required`), so the two come together; a project's
# repositories are Dispatch's mapping, so its entry is empty. The one project is not this daemon's
# own, so boot resolves no host git, jj or clone credential for it (readTmux reads tools only for
# cfg.Projects[cfg.Project]), and with no dispatch_url there is no workflow and no App token mint
# (openWorkflow): the proof still launches nothing and needs nothing new. It names no postgres_dsn:
# the daemon reads its store from LEGION_POSTGRES_DSN (with_store above).
cat >"$work/legion.yaml" <<EOF
project: $project
port: $port
state_dir: $work/state
operator_token_file: $work/operator-token
review_round_cap: 5
# The wake pacing off (INFRA-976): this proof's waits are bounded, and at the default of 8 wakes a
# minute every wake after the first would spend those waits queueing for its turn.
worker_wakes_per_minute: 0
envoy_url: http://legion:fixture-envoy-password@127.0.0.1:9020
projects:
  FIXTURE: {}
github_apps:
  implement:
    app_id: "1001"
    private_key: |
      -----BEGIN PRIVATE KEY-----
      fixture
      -----END PRIVATE KEY-----
  review:
    app_id: "1002"
    private_key: |
      -----BEGIN PRIVATE KEY-----
      fixture
      -----END PRIVATE KEY-----
EOF

# 1. refuses without Postgres, naming the host and never the password. The bad DSN carries its
#    own credentials so both halves of that assertion mean something wherever this runs. It is the
#    file's own postgres_dsn, which outranks LEGION_POSTGRES_DSN, and this start is given none. Its
#    password is random bytes of this run, so no file holds it and the assertion below reads a
#    value only this refusal could have printed.
bad_password=$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')
{
  cat "$work/legion.yaml"
  printf 'postgres_dsn: postgres://legion:%s@127.0.0.1:5499/legion\n' "$bad_password"
} >"$work/bad.yaml"
if "${pane_home[@]}" "$work/legion" start --config "$work/bad.yaml" 2>"$work/refusal.log"; then
  echo "expected refusal"
  exit 1
fi
grep -q '127.0.0.1:5499' "$work/refusal.log" || {
  echo "the refusal does not name the host it could not reach"
  exit 1
}
# `! grep …` would be read as a negation set -e ignores (shellcheck SC2251): the assertion could
# never fail. The password must fail the run, so the run says so itself.
grep -q -- "$bad_password" "$work/refusal.log" && {
  echo "the refusal printed the password"
  exit 1
}

# 2. starts, answers, restarts against the same store
start_daemon
wait_healthz
# The daemon says it is ready, beside the /healthz above and the state read below (INFRA-3169).
# It is read here and after the restart both, never at one boot alone: the latch is one-way and
# the booting process's own, so each start begins unlatched and this read says nothing about the
# next process; /healthz is answered whether or not the latch was set, so a daemon that answers it
# while /readyz never latches is exactly the state this proof exists to catch, and the
# restart reaches serving down the other path — an existing store, a second boot — where it could
# part from this one.
readyz_200
held_by_daemon_and_keeper
"$work/legion" state --json --port "$port" >"$work/state1.json"
jq -e --arg p "$project" \
  '.daemon.project == $p and .daemon.boots == 1 and .admission.cap == 4 and (.issues | length) == 0' \
  "$work/state1.json" || {
  echo "the first boot's state is not the project, boot 1, cap 4 and no issues stage 1 promises"
  exit 1
}
# The deployment's capability report rides the same document (LEGION-578): every row of the table,
# and under tmux with nothing decided the resource-limits row open with the legion.yaml line that
# records a decision — a report, never a refusal, so the daemon served it — and the pool-capacity
# row present, since the tmux runtime runs no probe pod. Both carry `source` "configuration"
# (contract 25): under tmux, resource-limits is what the runtime alone decides and pool-capacity
# reads that tmux runs no probe pod, so neither is boot's measurement; and no live, image or
# withheld row carries `source` at all. The twenty-first row is `claude-code` (INFRA-220): the
# worker image carries Claude Code's pin and the Legion plugin unconditionally — the binary itself
# a pod installs at launch — so every deployment's image is probed for the pin and the plugin,
# this one included.
jq -e '(.capabilities | length) == 21
  and (.capabilities | map(select(.name == "resource-limits" and .status == "open"
    and .configLine == "capabilities.decided.resource-limits: \"<reason>\""
    and .source == "configuration")) | length) == 1
  and (.capabilities | map(select(.name == "pool-capacity" and .status == "present"
    and .source == "configuration")) | length) == 1
  and (.capabilities | map(select((.name != "resource-limits" and .name != "pool-capacity"
    and .name != "secrets" and .name != "model-fallback") and has("source"))) | length) == 0' \
  "$work/state1.json" || {
  echo "the state does not carry the 21-row capability report with resource-limits and pool-capacity's source under tmux"
  exit 1
}
# The daemon's resolved settings ride the same document (LEGION-791, contract 35): the value the
# fixture set over its default, a default the fixture left alone, the runtime, the envoy_url with
# its userinfo masked, and none of what the fixture planted as a credential, a credential file or an
# App key — the App rides as its id and installations alone, and nothing anywhere in the document
# reads as a PEM, a DSN (the store's, which the daemon read from LEGION_POSTGRES_DSN), the planted
# password or a `user:password@` of any URL.
jq -e '.settings.review_round_cap == 5
  and .settings.worker_boot_timeout_seconds == 120
  and .settings.runtime.name == "tmux"
  and .settings.envoy_url == "http://xxxxx@127.0.0.1:9020"
  and ((.settings | has("postgres_dsn") or has("operator_token_file")) | not)
  and ([.. | strings | select(test("BEGIN|PRIVATE KEY|postgres://|fixture-envoy-password|://[^/@ ]+:[^/@ ]+@"))] | length) == 0
  and (.settings.github_apps.implement | keys) == ["app_id", "installations"]' \
  "$work/state1.json" || {
  echo "the state does not carry the resolved settings with the fixture's review_round_cap, the masked envoy_url and no credential, credential file or App key"
  exit 1
}
"$work/legion" legions --json |
  jq -e --arg p "$project" --argjson port "$port" 'map(select(.team == $p and .port == $port)) | length == 1' || {
  echo "the legions registry does not carry exactly one entry for $project on $port"
  exit 1
}
kill -TERM "$pid"
stop_daemon SIGTERM

start_daemon
wait_healthz " after restart"
# The restart's own latch, which the first boot's proves nothing about: a second boot, over a store
# that already holds this project, must reach serving and say so, and its 503 would be as invisible
# to the /healthz above as the first boot's would have been.
readyz_200 " after restart"
held_by_daemon_and_keeper " after restart"
"$work/legion" state --json --port "$port" >"$work/state2.json"
jq -e '.daemon.boots == 2' "$work/state2.json" || {
  echo "the restart did not record a second boot"
  exit 1
}
[ "$(jq -r .daemon.firstBootAt "$work/state1.json")" = "$(jq -r .daemon.firstBootAt "$work/state2.json")" ] || {
  echo "firstBootAt changed across the restart: the store is not the same one"
  exit 1
}
# `legion stop` loads legion.yaml as the daemon does, so it is handed the store the same way.
(with_store "$work/legion" stop --config "$work/legion.yaml")
stop_daemon "legion stop"

ok=1
echo "stage 1 e2e: PASS"
