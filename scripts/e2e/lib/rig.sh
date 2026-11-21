# shellcheck shell=bash disable=SC2154 # the variables below are the caller's (see the header)
# The host-side rig helpers a stage proof shares: the proof's guard and its one wait, ports, the
# processes the run starts, and their teardown. Everything they touch is the run's own — its work
# directory, its processes, and the ports the kernel chose for it.
#
# Sourced, never run. The caller sets
#   root           the checkout
#   work           the run's scratch directory; every process that carries the run's mark, which
#                  the caller exports as LEGION_E2E_RUN=$work (run_marks, lib/process-tree.sh),
#                  belongs to the run, and wait_for keeps its last poll's output there
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

# lib/proof-wait.sh holds the proof's one guard and its one wait (proof_guard, proof_seconds_left,
# wait_for and its returning form wait_held), which the pi-envoy smokes share through
# packages/pi-envoy/scripts/local-stack.sh. It reads the caller's work, timeout_hook, note and
# fail, which this rig's caller sets and defines. until_true and await_start below are the bounded
# waits of the proofs only a person runs, which set no guard; no proof that sets one calls them.
# shellcheck source-path=SCRIPTDIR source=proof-wait.sh
. "$root/scripts/e2e/lib/proof-wait.sh"

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
# proof_guard). It polls COMMAND at once and then every half second, each poll's own run included,
# and gives up at the first poll that fails once SECONDS of wall time have passed: it polls before
# it judges the bound, so a wait that starts late, on a box that lost the CPU before its first
# poll, still reads a fact that is already true. It says what it waits for on entry and every 60 s
# while it does (polls and seconds), so a healthy wait and a stall read differently to anyone
# watching a run's output. A poll that never returns would hold the wait past its bound, so a
# caller's remote calls carry their own timeouts.
until_true() {
  local limit=$1 what=$2 polls=0 started=$SECONDS beat=$SECONDS
  shift 2
  note "waiting up to ${limit}s for $what"
  while :; do
    [ ! -s "$evidence/pane-endpoint-violation.txt" ] || fail "ABORT: $(cat "$evidence/pane-endpoint-violation.txt")"
    polls=$((polls + 1))
    if "$@" >/dev/null 2>&1; then return 0; fi
    ((SECONDS - started < limit)) || break
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
# /proc/net/tcp6, and each process of `${proc_root:-/proc}` with a descriptor naming
# socket:[<inode>] (the host's /proc, unless a test points proc_root at a proc root of its own,
# scripts/e2e/lib/proc-root.ts). It fails with 1 when no socket listens on PORT, or more than one
# does, and with 2, saying why, when /proc/net/tcp cannot be read, so an unread table is never
# read as a port nothing listens on, and when the proc root lists no process at all.
# The shell's glob hands the <pid>/fd entries to find on stdin (-files0-from -), never as its
# arguments: as arguments, past ARG_MAX (about 80,000 processes), find could not start, and the
# `|| true` this had turned that E2BIG into no holder at all. An fd directory this user may not read
# (another user's process) or whose process ended during the walk is none of the answer; any other
# complaint of the walk's (proc_walk_complaint) prints and fails with 2.
listening_holders() {
  local proc=${proc_root:-/proc} inodes err status=0 complaint holder table
  local -a holders=() tables=() entries=()
  for table in /proc/net/tcp /proc/net/tcp6; do
    [ ! -e "$table" ] || tables+=("$table")
  done
  [ -r /proc/net/tcp ] || {
    echo "listening_holders: /proc/net/tcp cannot be read, so the socket listening on port $1 was not looked for" >&2
    return 2
  }
  inodes=$(awk -v port=":$(printf '%04X' "$1")" \
    '$4 == "0A" && substr($2, length($2) - 4) == port { print $10 }' "${tables[@]}") || {
    echo "listening_holders: awk could not read ${tables[*]}, so the socket listening on port $1 was not looked for" >&2
    return 2
  }
  case "$inodes" in
    '' | *[!0-9]*)
      echo "listening_holders: $(wc -w <<<"$inodes") sockets listen on port $1, not one" >&2
      return 1
      ;;
  esac
  entries=("$proc"/[0-9]*/fd)
  [ -e "${entries[0]}" ] || {
    echo "listening_holders: $proc lists no process's fd directory, so the holders of port $1 were not read" >&2
    return 2
  }
  err=$(mktemp "${TMPDIR:-/tmp}/listening-holders.XXXXXX") || {
    echo "listening_holders: no file to keep find's complaints in, so $proc was not read" >&2
    return 2
  }
  # shellcheck disable=SC2185 # find reads its paths from stdin (-files0-from -), not from argv
  # `|| exit` keeps find's status and keeps a caller's ERR trap (`set -E`) from firing inside the
  # substitution for the unreadable entries every walk meets.
  mapfile -t holders < <(printf '%s\0' "${entries[@]}" | LC_ALL=C find -files0-from - -maxdepth 1 -lname "socket:\\[$inodes\\]" 2>"$err" || exit)
  wait "$!" || status=$?
  complaint=
  [ "$status" = 0 ] || complaint=$(proc_walk_complaint "$err" "$proc") || complaint=
  # GNU find exits 0 or 1 of its own accord; any other status is a find that a signal or the shell
  # ended, and the lines it wrote before are no answer, even when it printed no complaint.
  [ "$status" -le 1 ] || complaint=${complaint:-"find ended with status $status and printed nothing"}
  rm -f -- "$err"
  if [ -n "$complaint" ]; then
    printf 'listening_holders: find exited %s reading %s: %s\n' "$status" "$proc" "$complaint" >&2
    return 2
  fi
  for holder in ${holders[@]+"${holders[@]}"}; do
    holder=${holder#"$proc"/}
    printf '%s\n' "${holder%%/*}"
  done | sort -un
}

# held_only_by PORT PID... succeeds when the processes holding the socket listening on PORT
# (listening_holders) are exactly the PIDs given, and otherwise prints each holder with its command
# and fails: a descriptor that leaked into another process (a tmux server, a pane, a CLI call) keeps
# the port listening after the service stops. A reading listening_holders could not make fails with
# its status and its reason.
held_only_by() {
  local port=$1 holders want pid
  shift
  holders=$(listening_holders "$port") || return
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
# sets <NAME>_pid to its pid, with its start time recorded (record_start, lib/process-tree.sh), so
# stop_pid signals it only while it is still that process. A service that has already exited when
# the start is read, or is a zombie, is recorded `gone` (started_at), so stop_pid and signal_group
# later send it nothing and say nothing; start_process still succeeds, so await_start, not a
# driver's `set -e`, names its log. Monitor mode is on for the start alone,
# so the service leads a process group of its own: stop_tree stops what it kills with SIGSTOP
# first, and a stopped member of this shell's own process group has the kernel hang up every member
# of that group — this shell, the command that started it and the `bun test` that started that — as
# soon as any of them exits and the group is orphaned. In a group of its own the service's stop
# reaches nothing but itself.
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
  record_start "$!" || started_at[$!]=gone
}

# run_processes prints the pid of every process of this run still alive: each that carries the run's
# mark (marked_processes and run_marks, lib/process-tree.sh), which the run exports as LEGION_E2E_RUN
# once its transcript runs. A process that works under $work or names it on its command line and
# carries no mark, a person's shell or `tail -f` watching the run among them, is none of the run's.
# It prints why and fails with 2 when the run's processes could not be read, so no caller reads an
# unread machine as one with nothing left. stop_marked_processes "$work" ends what it names.
run_processes() {
  local pairs pid
  pairs=$(marked_processes "$work") || return 2
  while read -r pid _; do
    [ -z "$pid" ] || printf '%s\n' "$pid"
  done <<<"$pairs"
}

# shellcheck source-path=SCRIPTDIR source-path=SCRIPTDIR/lib source=process-tree.sh
. "$root/scripts/e2e/lib/process-tree.sh"
