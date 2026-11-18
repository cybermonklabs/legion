# shellcheck shell=bash disable=SC2154 # $work and fail are the caller's (see below)
# The starts of a run's NATS and Postgres containers, and the polls that wait for each server and
# read its address back. lib/rig.sh sources this file, and so do stage2-tmux-supervision.sh and
# verifiers-staging-token.sh, which source no rig. Sourced, never run: it needs docker, the
# caller's fail (which exits) and $work, the run's scratch directory, and nothing else of its
# caller's.
#
# A container the run starts publishes no port. Docker chooses a published host port with an
# allocator of its own: the port after its last choice in the kernel's ephemeral range
# (/proc/sys/net/ipv4/ip_local_port_range), which it binds without asking the kernel whether
# another program holds it, trying the next on a refusal, and the container's start fails after
# ten taken ports in a row (docs/testing.md, "A container a test starts publishes no port"). So
# NATS runs on the host's network and binds a loopback port the kernel chooses, and Postgres
# listens on a unix socket alone, in a directory of the run's own. Each start returns once docker
# has started the container, and the caller waits for the server through its own wait (wait_for in
# a proof that set its guard, until_true in one that set none, or the script's own) on the poll
# beside the start, which also reads the address back. A poll that finds the container stopped sets
# wait_impossible, which lib/rig.sh's wait_for reads, so that wait ends at once.

# start_nats_container NAME [DOCKER_ARG]... -- IMAGE [SERVER_ARG]... runs the NATS image IMAGE as
# the container NAME on the host's network, the DOCKER_ARGs before the image (--cpus, --memory,
# --label) and the SERVER_ARGs after it, then -a 127.0.0.1 -p -1: the server listens for clients
# on 127.0.0.1, on a port the kernel chooses when the server binds it. -m -1 among the SERVER_ARGs
# opens its monitoring the same way. nats_container_ready reads the ports back, and the caller's
# cleanup removes NAME.
start_nats_container() {
  local _name=$1 _docker=()
  shift
  while [ $# -gt 0 ] && [ "$1" != -- ]; do
    _docker+=("$1")
    shift
  done
  [ $# -gt 1 ] || fail "start_nats_container $_name: no image after --"
  shift
  docker run -d --name "$_name" --network host ${_docker[@]+"${_docker[@]}"} "$@" -a 127.0.0.1 -p -1 >/dev/null ||
    fail "start_nats_container: docker run of the NATS container $_name failed"
}

# nats_container_ready NAME PORT_VAR [MONITOR_VAR] is the poll a wait runs on the server
# start_nats_container started as NAME. It succeeds once the server has logged 'Server is ready',
# and assigns PORT_VAR the client port it logged ('Listening for client connections on
# 127.0.0.1:<port>') and MONITOR_VAR, when given, its monitoring port ('Starting http monitor on
# 127.0.0.1:<port>', which -m -1 opens). The locals' names are ones no caller passes.
nats_container_ready() {
  local _log _client='Listening for client connections on 127\.0\.0\.1:([0-9]+)'
  local _monitor='Starting http monitor on 127\.0\.0\.1:([0-9]+)'
  _log=$(docker logs "$1" 2>&1) || return 1
  if [[ $_log != *'Server is ready'* ]]; then
    _container_stopped "$1" "the NATS container" "$_log"
    return 1
  fi
  [[ $_log =~ $_client ]] || return 1
  printf -v "$2" '%s' "${BASH_REMATCH[1]}"
  [ -n "${3:-}" ] || return 0
  [[ $_log =~ $_monitor ]] || return 1
  printf -v "$3" '%s' "${BASH_REMATCH[1]}"
}

# start_postgres_container NAME DIR_VAR [DOCKER_ARG]... -- IMAGE [SERVER_ARG]... runs the Postgres
# image IMAGE as the container NAME with no TCP listener (listen_addresses empty) and its socket,
# .s.PGSQL.5432, in a directory of the run's own mounted at /sockets, beside the image's own
# /var/run/postgresql, where a `docker exec NAME psql` still reaches the server. It assigns DIR_VAR
# that directory, so the server's address on this machine is
# postgres://USER:PASSWORD@/DATABASE?host=<DIR_VAR>. The DOCKER_ARGs go before the image and the
# SERVER_ARGs after it (-c max_connections=…). The directory is under $work when the socket's path
# there fits the 107 bytes a unix socket path holds and needs no quoting in a URL, and under /tmp
# otherwise; the caller's cleanup removes it after the container. It is mode 0777, since the
# image's server runs as a user of the image's own, and it is not /var/run/postgresql, which the
# entrypoint makes that user's with the sticky bit, so the run can remove a socket a killed server
# left in it. postgres_container_ready is the poll that waits for the server.
start_postgres_container() {
  local _name=$1 _dir_var=$2 _docker=() _dir
  shift 2
  while [ $# -gt 0 ] && [ "$1" != -- ]; do
    _docker+=("$1")
    shift
  done
  [ $# -gt 1 ] || fail "start_postgres_container $_name: no image after --"
  shift
  _dir=$(mktemp -d "$work/pg.XXXXXX") || fail "start_postgres_container $_name: no socket directory under $work"
  if ! [[ $_dir =~ ^[A-Za-z0-9._/-]+$ ]] || ((${#_dir} + ${#_postgres_socket} + 1 > 107)); then
    rmdir "$_dir"
    _dir=$(mktemp -d /tmp/legion-pg.XXXXXX) || fail "start_postgres_container $_name: no socket directory under /tmp"
  fi
  chmod 0777 "$_dir"
  printf -v "$_dir_var" '%s' "$_dir"
  docker run -d --name "$_name" ${_docker[@]+"${_docker[@]}"} --mount "type=bind,source=$_dir,target=/sockets" \
    "$@" -c listen_addresses= -c unix_socket_directories=/var/run/postgresql,/sockets >/dev/null ||
    fail "start_postgres_container: docker run of the Postgres container $_name failed"
}

# _postgres_socket is the server's socket in its socket directory, for the port it keeps, 5432.
_postgres_socket=.s.PGSQL.5432

# postgres_container_ready NAME is the poll a wait runs on the server start_postgres_container
# started as NAME. It succeeds once the server has logged 'database system is ready to accept
# connections' twice: the image's entrypoint first runs a server on the same socket to make the
# database, and stops it, and the second line is the server that stays.
postgres_container_ready() {
  local _log
  _log=$(docker logs "$1" 2>&1) || return 1
  [ "$(grep -c 'database system is ready to accept connections' <<<"$_log")" -lt 2 ] || return 0
  _container_stopped "$1" "the Postgres container" "$_log"
  return 1
}

# postgres_container_sockets NAME prints the socket directory of the container NAME that
# start_postgres_container started: the source of its /sockets mount, for a process of the run that
# did not start it and so holds no DIR_VAR. It prints nothing for a container with no such mount.
postgres_container_sockets() {
  docker inspect -f '{{range .Mounts}}{{if eq .Destination "/sockets"}}{{.Source}}{{end}}{{end}}' "$1"
}

# _container_stopped NAME WHAT LOG sets wait_impossible when the container NAME is not running,
# naming it as WHAT with the last lines of LOG, its log.
_container_stopped() {
  [ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" != true ] || return 0
  # shellcheck disable=SC2034 # lib/rig.sh's wait_for reads it
  wait_impossible="$2 $1 stopped; its log ends: $(tail -n 3 <<<"$3" | tr '\n' ' ')"
}
