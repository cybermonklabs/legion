# shellcheck shell=bash disable=SC2154 # the variables below are the caller's (see the header)
# The host-side rig helpers a stage proof shares: the proof's guard and its one wait, ports, the
# processes the run starts, and their teardown. Everything they touch is the run's own — its work
# directory, its processes, and the ports the kernel chose for it.
#
# Sourced, never run. The caller sets
#   root           the checkout
#   work           the run's scratch directory; every process whose command line or working
#                  directory names it belongs to the run, and wait_for keeps its last poll's
#                  output there
#   evidence       the evidence directory; each service started by start_process logs to
#                  $evidence/logs/<name>.log
#   timeout_hook   empty, or a function a wait that gives up runs before it fails
# and defines note and fail (which exits). A production guard that finds a violation writes it to
# $evidence/pane-endpoint-violation.txt, and the next until_true aborts naming it.
#
# Under the stage scripts' pipefail, an early-exiting consumer — `head -N`, `grep -q`, a
# `while … break` — never sits downstream of a live producer (kubectl, gh, jj): the producer's
# SIGPIPE (141) becomes the pipeline's status the moment the consumer stops reading, and the
# assertion dies on a correct system. Ask the producer for the bounded output (`--limit 1`, `--jq`)
# or capture its whole output first and judge the variable; a `| head -1` is safe only inside a
# `fail` message, guarded by `|| true`, or on a local-file grep whose few lines fit the pipe buffer.
#
# A run has several trees live at once, in one namespace and in clones that fetch every branch, so
# every read of tree state scopes to the tree under test: a jj revset is intersected with that
# tree's own chain (`& ::<its pull request's head>`, `@|@-` of its workspace), a kubectl selector
# carries its issue, tree or role label, and a daemon-log read is keyed by its sandbox and role. A
# query over the whole repository or namespace answers for whichever tree acted last, and "the
# newest" is a sibling's as soon as two trees act within the same minute. A deliberately run-wide
# read — every pod's shape, the admission cap, a negative over the whole log — says so in its comment.

# real_bin_path DEST NAME... symlinks DEST/NAME to the real NAME binary mise resolves for the
# caller's own environment, for every NAME given, and prints DEST. The daemon and every pane it
# launches run with HOME relocated to the run's own isolated home (make_omp_home,
# lib/omp-home.sh), which hides mise's global config from the dotfiles jj shim: it searches PATH
# for mise's own shim first and, under a HOME mise has no pin for, gets handed the call straight
# back — jj is unusable inside a pane until this is worked around. A caller prepends this
# directory to the daemon's own PATH before launching it, so the daemon and every pane it spawns
# use it: `PATH="$(real_bin_path "$work/realbin" gh jj tmux):$PATH"`. PATH reaches every pane
# (paneEnvAllowList, packages/daemon/internal/runtime/tmux/environment.go). gh still needs this
# directory too: resolveTools runs in the daemon's own process, under the same relocated HOME, and
# the dotfiles gh shim hits mise's own shim-recursion guard there the same way jj's shim loops —
# without this, the daemon itself has no real gh to resolve at all. A pane's own gh is the daemon's
# wrapper (legion gh, LEGION_GH_PATH), which resolves "the real gh" by walking its own PATH the
# same way (resolveTools, packages/daemon/internal/daemon/tools.go) — so this directory's gh is
# what LEGION_GH_PATH ends up naming, one step removed from a pane's own PATH lookup, not instead
# of it. jj has no such wrapper and keeps resolving through this directory directly. tmux needs it
# for the daemon itself: the tmux runtime looks tmux up on the pane PATH at boot and runs it with
# the pane environment's allow-list alone, which moves the XDG directories under the state
# directory. Run from a directory under the operator's real HOME, a dotfiles or mise shim's tmux
# then finds that HOME's mise config as a parent directory's config with no trust record and exits
# with status 1. The daemon's boot reads that as a failed scrub of its private server's environment,
# so boot refuses.
real_bin_path() {
  local dest=$1 name target
  shift
  mkdir -p "$dest"
  for name in "$@"; do
    target=$(mise which "$name" 2>/dev/null) || fail "real_bin_path: mise which $name failed; is $name pinned in mise.toml?"
    ln -sfn "$target" "$dest/$name"
  done
  printf '%s\n' "$dest"
}

# A proof a workflow runs (.github/e2e-scripts.json's hosted list, and ci.yaml's Stage 1 step)
# sets its guard once, at its head, with the reading of it beside the number, and waits through
# wait_for alone, never through a helper that takes a bound of its own. until_true and await_start
# are the bounded waits of the proofs only a person runs, which set no guard; no proof that sets
# one calls them.
#
# proof_margin_seconds is how long before the guard's deadline every wait of the proof gives up,
# so the wait's own failure, and the cleanup after it, print before anything stops the run: the
# margin the Go tests' testwait keeps before the test binary's deadline, so a Go driver that takes
# the time left as its -test.timeout (proof_seconds_left) gives up at the same moment.
proof_margin_seconds=30
proof_guard_seconds=''
proof_deadline_ms=''
# wait_impossible is what a wait_for COMMAND sets when it finds that the fact can no longer
# happen, such as the process that would answer having exited (serving, below).
wait_impossible=''

# proof_now_ms prints the wall clock in epoch milliseconds, the unit LEGION_PROOF_DEADLINE_MS
# carries.
proof_now_ms() {
  local now=${EPOCHREALTIME/[.,]/}
  printf '%s\n' "$((10#$now / 1000))"
}

# proof_guard SECONDS sets this proof's one guard: the run may take SECONDS from here. It records
# the deadline and exports LEGION_PROOF_DEADLINE_MS, the moment in epoch milliseconds that every
# wait of the proof gives up by, proof_margin_seconds before the deadline; the proof's Bun drivers
# read it (@legion/test-fixtures/wait). It arms nothing: what stops a run that hangs outside a
# wait is the limit of the CI job that runs the proof, which is at least the guard plus the job's
# setup. SECONDS is at least twice the longest green run on record, and the call carries that
# reading in a comment beside it.
proof_guard() {
  local seconds=${1:-}
  [ -n "${EPOCHREALTIME:-}" ] || fail "proof_guard: bash 5 or later is required (EPOCHREALTIME)"
  if ! [[ "$seconds" =~ ^[1-9][0-9]*$ ]] || ((seconds <= proof_margin_seconds)); then
    fail "proof_guard: '$seconds' is not a whole number of seconds above the ${proof_margin_seconds}s margin"
  fi
  [ -z "$proof_deadline_ms" ] || fail "proof_guard: this proof set its guard already"
  proof_guard_seconds=$seconds
  proof_deadline_ms=$(($(proof_now_ms) + seconds * 1000))
  export LEGION_PROOF_DEADLINE_MS=$((proof_deadline_ms - proof_margin_seconds * 1000))
}

# proof_seconds_left prints the whole seconds left before the guard's deadline, and at least 1: a
# Go driver's -test.timeout, under which testwait gives up proof_margin_seconds before it, where
# wait_for does.
proof_seconds_left() {
  local left=$(((proof_deadline_ms - $(proof_now_ms)) / 1000))
  printf '%s\n' "$((left > 0 ? left : 1))"
}

# wait_for WHAT COMMAND... is the one wait of a proof that set its guard. It runs COMMAND in this
# shell every half second until it succeeds, and gives up proof_margin_seconds before the guard's
# deadline, failing the run with a line that names WHAT and the last output of COMMAND; a wait
# that starts past that moment runs COMMAND once and then fails, so the cleanup keeps the margin
# to run in. COMMAND's output goes to $work/wait-for.out, so a COMMAND prints what it read for the
# failure line to show. It takes no bound and no pace: a step that is only slow never fails the
# run, and a fact that never comes fails it by name. It says how long it has waited every 60 s.
#
# COMMAND runs in this shell, so it can set variables. One that finds the fact can no longer
# happen sets wait_impossible to why, and the wait fails at once saying so rather than at the
# deadline: serving below is that COMMAND for a service the run started.
wait_for() {
  local what=$1 output=$work/wait-for.out polls=0 started=$SECONDS beat=$SECONDS
  shift
  [ -n "$proof_deadline_ms" ] || fail "wait_for: this proof set no guard (proof_guard), so the wait for $what has no deadline to end by"
  if [ -z "$work" ] || [ ! -d "$work" ]; then
    fail "wait_for: the run has no work directory to keep the output of its wait for $what in"
  fi
  while :; do
    polls=$((polls + 1))
    wait_impossible=''
    if "$@" >"$output" 2>&1; then return 0; fi
    [ -z "$wait_impossible" ] || fail "$what: $wait_impossible"
    (($(proof_now_ms) < LEGION_PROOF_DEADLINE_MS)) || break
    if ((SECONDS - beat >= 60)); then
      beat=$SECONDS
      note "still waiting for $what: poll $polls, $((SECONDS - started))s"
    fi
    sleep 0.5
  done
  if [ -n "${timeout_hook:-}" ]; then "$timeout_hook" || true; fi
  fail "gave up waiting for $what after $polls polls, ${proof_margin_seconds}s before this proof's guard of ${proof_guard_seconds}s; its last output: $(tail -n 5 "$output" 2>/dev/null || true)"
}

# serving NAME PID COMMAND... runs COMMAND for a wait_for on the service NAME, started as PID
# (start_process): once PID has exited it sets wait_impossible, naming the service's log, so the
# wait ends at once.
serving() {
  local name=$1 pid=$2
  shift 2
  "$@" && return 0
  kill -0 "$pid" 2>/dev/null || wait_impossible="the process exited; see $evidence/logs/$name.log"
  return 1
}

# until_true SECONDS DESCRIPTION COMMAND... — the bounded wait of a proof that sets no guard (see
# proof_guard). It polls COMMAND every half second until SECONDS of wall time have passed, each
# poll's own run included, and says what it waits for on entry and every 60 s while it does (polls
# and seconds), so a healthy wait and a stall read differently to anyone watching a run's output.
# A poll that never returns would hold the wait past its bound, so a caller's remote calls carry
# their own timeouts.
until_true() {
  local limit=$1 what=$2 polls=0 started=$SECONDS beat=$SECONDS
  shift 2
  note "waiting up to ${limit}s for $what"
  while ((SECONDS - started < limit)); do
    [ ! -s "$evidence/pane-endpoint-violation.txt" ] || fail "ABORT: $(cat "$evidence/pane-endpoint-violation.txt")"
    polls=$((polls + 1))
    if "$@" >/dev/null 2>&1; then return 0; fi
    if ((SECONDS - beat >= 60)); then
      beat=$SECONDS
      note "still waiting for $what: poll $polls, $((SECONDS - started))s of ${limit}s"
    fi
    sleep 0.5
  done
  if [ -n "$timeout_hook" ]; then "$timeout_hook" || true; fi
  fail "timed out after ${limit}s ($polls polls) waiting for $what"
}

# lib/loopback-dsn.sh holds require_loopback_dsn, require_run_database and exec_run_database, the
# helpers a proof that writes a PostgreSQL server reads its caller's DSN with, and the one copy of
# why they ask libpq. It needs nothing of this rig's, and envoy-session-delivery.sh sources it
# without this file.
# shellcheck source-path=SCRIPTDIR source=loopback-dsn.sh
. "$root/scripts/e2e/lib/loopback-dsn.sh"

# lib/containers.sh holds the starts of a run's NATS and Postgres containers, which publish no
# port, and the polls that wait for each and read its address back. It needs nothing of this
# rig's, and stage2-tmux-supervision.sh and verifiers-staging-token.sh source it without this file.
# shellcheck source-path=SCRIPTDIR source=containers.sh
. "$root/scripts/e2e/lib/containers.sh"

# A service the run starts serves on a port the kernel chose, held from that choice until the
# service serves on it, so no other process can take it in between (lib/held-port.py). A port
# picked free and passed by number could be taken before the service binds it. Each helper below
# starts its process with start_process (the service's log, <NAME>_pid) and keeps its port file
# and keeper socket in $work.

# shellcheck source-path=SCRIPTDIR source=port-file.sh
. "$root/scripts/e2e/lib/port-file.sh"

# await_port_file NAME FILE VAR waits for FILE, the port file held-port.py writes before it runs
# the service NAME, while NAME's process (<NAME>_pid) lives, and assigns VAR its port. A proof that
# set its guard waits through wait_for; one that set none keeps read_port_file's own bound
# (lib/port-file.sh). The locals' names are ones no caller passes.
await_port_file() {
  local _name=$1 _file=$2 _var=$3 _pid_var _value _status=0
  _pid_var=${_name}_pid
  if [ -n "$proof_deadline_ms" ]; then
    wait_for "$_name to hold a port" serving "$_name" "${!_pid_var}" test -s "$_file"
    read -r _value <"$_file"
    printf -v "$_var" '%s' "$_value"
    return 0
  fi
  read_port_file "$_file" "${!_pid_var}" "$_var" || _status=$?
  case $_status in
    0) ;;
    1) fail "$_name exited before it held a port; see $evidence/logs/$_name.log" ;;
    *) fail "timed out after ${port_file_seconds}s waiting for $_name to hold a port" ;;
  esac
}

# start_held NAME PORT_VAR FD_ENV [--env K=V]... -- COMMAND... starts COMMAND as the service NAME on
# a listening socket held-port.py opens on 127.0.0.1:0 and hands over as the descriptor FD_ENV
# names (DISPATCH_LISTEN_FD, ENVOY_LISTEN_FD, LEGION_LISTEN_FD), and assigns PORT_VAR its port, read
# back before COMMAND runs. {port} in an --env value or in COMMAND is that port. The helper executes
# COMMAND in place, so <NAME>_pid is the service's own pid. A COMMAND that clears its environment
# (`env -i`) names FD_ENV=3 itself: held-port.py always hands the socket over as descriptor 3.
start_held() {
  _held_exec start_held "$1" "$3" --port-file "$work/$1.port" "${@:4}"
  await_port_file "$1" "$work/$1.port" "$2"
}

# _held_exec CALLER NAME FD_ENV SOURCE_FLAG SOURCE_VALUE [--env K=V]... -- COMMAND... starts
# held-port.py exec as the service NAME, its socket from SOURCE_FLAG SOURCE_VALUE (--port-file, a
# fresh one whose port it writes there, removed first; --from, a keeper's), handed over as FD_ENV.
# A missing COMMAND is refused naming CALLER.
_held_exec() {
  local _caller=$1 _name=$2 _fd_env=$3 _flag=$4 _value=$5 _options=()
  shift 5
  while [ $# -gt 0 ] && [ "$1" != -- ]; do
    _options+=("$1")
    shift
  done
  [ $# -gt 1 ] || fail "$_caller $_name: no command after --"
  shift
  [ "$_flag" != --port-file ] || rm -f "$_value"
  start_process "$_name" python3 "$root/scripts/e2e/lib/held-port.py" exec "$_flag" "$_value" \
    --fd-env "$_fd_env" "${_options[@]}" -- "$@"
}

# keep_port NAME PORT_VAR starts the keeper NAME, a held-port.py keep that holds one listening
# socket on 127.0.0.1 for the run, and assigns PORT_VAR its port once the keeper hands out copies of
# it. The caller's cleanup stops it (<NAME>_pid). Every start_kept on it serves that port, so a
# configuration can name the port before its service first starts, and a restart cannot lose the
# port to another process. Between two servers the port still listens and nothing answers: the
# kernel queues a connection until the next server accepts it, so every poll of a kept port carries
# a timeout (curl --max-time) and takes the timeout for down.
keep_port() {
  local _name=$1 _port_var=$2
  rm -f "$work/$_name.port" "$work/$_name.sock"
  start_process "$_name" python3 "$root/scripts/e2e/lib/held-port.py" keep --port-file "$work/$_name.port" \
    --socket "$work/$_name.sock"
  await_port_file "$_name" "$work/$_name.port" "$_port_var"
}

# start_kept NAME KEEPER FD_ENV [--env K=V]... -- COMMAND... starts COMMAND as the service NAME, as
# start_held does, on a copy of the socket the keeper KEEPER (keep_port) holds.
start_kept() {
  _held_exec start_kept "$1" "$3" --from "$work/$2.sock" "${@:4}"
}

# report_port LOG PATTERN prints the port a process that bound port 0 itself reported: the digits
# right after the first match of the extended regular expression PATTERN in LOG (nats-server -p -1:
# 'Listening for client connections on [^ ]*:'). It fails while LOG holds no such line, so it is
# also the poll that waits for one.
report_port() {
  local _match
  _match=$(grep -m1 -oE -- "$2[0-9]+" "$1" 2>/dev/null) || return 1
  _match=${_match%%$'\n'*}
  printf '%s\n' "${_match##*[!0-9]}"
}

# port_answers PORT succeeds when something accepts a connection on 127.0.0.1:PORT.
port_answers() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# listening_holders PORT prints, one per line in order, the pid of every process holding the one
# socket that listens on PORT: its inode from the LISTEN row (state 0A) of /proc/net/tcp and
# /proc/net/tcp6, and each process with a descriptor naming socket:[<inode>]. It fails when no
# socket listens on PORT, or more than one does.
listening_holders() {
  local inodes
  inodes=$(awk -v port=":$(printf '%04X' "$1")" \
    '$4 == "0A" && substr($2, length($2) - 4) == port { print $10 }' /proc/net/tcp /proc/net/tcp6 2>/dev/null)
  case "$inodes" in
    '' | *[!0-9]*)
      echo "listening_holders: $(wc -w <<<"$inodes") sockets listen on port $1, not one" >&2
      return 1
      ;;
  esac
  # A process that exits during the walk leaves find an unreadable directory: its status is no answer.
  { find /proc/[0-9]*/fd -maxdepth 1 -lname "socket:\\[$inodes\\]" 2>/dev/null || true; } | cut -d/ -f3 | sort -un
}

# held_only_by PORT PID... succeeds when the processes holding the socket listening on PORT
# (listening_holders) are exactly the PIDs given, and otherwise prints each holder with its command
# and fails: a descriptor that leaked into another process (a tmux server, a pane, a CLI call) keeps
# the port listening after the service stops.
held_only_by() {
  local port=$1 holders want pid
  shift
  holders=$(listening_holders "$port") || return 1
  want=$(printf '%s\n' "$@" | sort -un)
  [ "$holders" != "$want" ] || return 0
  echo "the socket listening on port $port is held by:" >&2
  for pid in $holders; do echo "  $pid $(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null)" >&2; done
  echo "and should be held by $(tr '\n' ' ' <<<"$want")alone" >&2
  return 1
}

# await_start NAME PID SECONDS DESCRIPTION COMMAND... waits, bounded, for COMMAND to succeed while
# the service NAME started as PID lives, and fails the check naming the service's log the moment it
# exits. A proof that set its guard waits with `wait_for DESCRIPTION serving NAME PID COMMAND...`.
await_start() {
  local name=$1 pid=$2 limit=$3 what=$4 i
  shift 4
  for ((i = 0; i < limit * 2; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    kill -0 "$pid" 2>/dev/null || fail "$what: the process exited; see $evidence/logs/$name.log"
    sleep 0.5
  done
  fail "timed out after ${limit}s waiting for $what"
}

# start_process NAME COMMAND... starts COMMAND in the background, logging to the service's log, and
# sets <NAME>_pid to its pid. Monitor mode is on for the start alone, so the service leads a process
# group of its own: stop_tree stops what it kills with SIGSTOP first, and a stopped member of this
# shell's own process group has the kernel hang up every member of that group — this shell, the
# command that started it and the `bun test` that started that — as soon as any of them exits and
# the group is orphaned. In a group of its own the service's stop reaches nothing but itself.
#
# The redirection from /dev/null is what monitor mode takes away: bash gives a background job
# /dev/null on stdin only while monitor mode is off, so without it a service inherits this script's
# own stdin. From a terminal that is the tty, with the service in a background process group, and
# the service's first read stops it with SIGTTIN while await_start waits out its limit.
start_process() {
  local name=$1
  shift
  set -m
  "$@" </dev/null >>"$evidence/logs/$name.log" 2>&1 &
  printf -v "${name}_pid" '%s' "$!"
  set +m
}

# signalable PID is whether PID can name one process the run started: a number without a leading
# zero other than 1, this shell, and this shell's process group leader. 0, however many digits spell
# it, and a negative number name a whole process group, so `kill -STOP 0` would stop the caller with
# everything else in its group and a later `kill -KILL 0` end them all; no pid is written with a
# leading zero. A refusal is printed, since a caller that hands one over has a bug.
signalable() {
  local pid=$1
  case "$pid" in
    '' | *[!0-9]* | 0* | 1) ;;
    *)
      [ "$pid" != "$$" ] && [ "$pid" != "$BASHPID" ] &&
        [ "$pid" != "$(ps -o pgid= -p "$$" 2>/dev/null | tr -d ' ')" ] && return 0
      ;;
  esac
  echo "refused to signal pid '$pid': it is not one process this run started" >&2
  return 1
}

# stop_pid is intentionally best effort: a failed cleanup must never obscure the check that failed.
stop_pid() {
  local pid=${1:-} i
  [ -n "$pid" ] || return 0
  signalable "$pid" || return 0
  kill -TERM "$pid" 2>/dev/null || return 0
  for i in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.2
  done
  kill -KILL "$pid" 2>/dev/null || true
  return 0
}

# stop_tree PID stops PID and every process under it. A watcher is a loop whose kubectl, jq or sleep
# outlives the loop when only the loop is stopped; the loop is frozen first, so it starts nothing new
# while its children go. PID is signalled only while its parent is PARENT (default this shell), so a
# pid the watcher left and another process reused is never hit. An empty PID is a watcher that
# never started: nothing to stop.
# pgrep exits 1 for a process with no children, which is no error: under a driver's `set -E` it would
# run the ERR trap inside the substitution, and a trap that prints there would hand its words over
# as child pids (the `0` of an `exit 0` among them).
stop_tree() {
  local pid=${1:-} parent=${2:-$$} child
  [ -n "$pid" ] || return 0
  signalable "$pid" || return 0
  [ "$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')" = "$parent" ] || return 0
  kill -STOP "$pid" 2>/dev/null || return 0
  for child in $(pgrep -P "$pid" || true); do stop_tree "$child" "$pid"; done
  kill -KILL "$pid" 2>/dev/null || true
}

# run_processes prints every pid of this run still alive: any whose working directory is under
# $work, and any whose command line names $work. A process whose working directory this user cannot
# read is another user's, so it is none of the run's and gets no further look, as a kill of it would
# fail anyway.
# One `find` reads every /proc/<pid>/cwd link, and the command lines are read by the shell itself:
# a `readlink` and a `grep` for each of a devbox's thousands of /proc entries cost seconds of every
# teardown, the sweeps of a checkpoint among them. `%l` is empty for a link find could not read.
run_processes() {
  local p cwd arg entry
  local -a args
  while IFS= read -r -d '' entry; do
    p=${entry%% *}
    cwd=${entry#* }
    [ -n "$cwd" ] || continue
    case "$cwd/" in
      "$work"/*) printf '%s\n' "${p#/proc/}"; continue ;;
    esac
    args=()
    { mapfile -d '' -t args <"$p/cmdline"; } 2>/dev/null || continue
    for arg in ${args[@]+"${args[@]}"}; do
      case $arg in
        *"$work"*) printf '%s\n' "${p#/proc/}"; break ;;
      esac
    done
    # find exits 1 for the cwd links it cannot read, every root process's among them, which is no
    # error here: under the callers' `set -E` an unguarded status would run their ERR trap inside
    # this substitution and print a warning, or a check's FAIL line, on a run that is going well.
  done < <(find /proc/[0-9]*/cwd -maxdepth 0 -printf '%h %l\0' 2>/dev/null || :)
}
