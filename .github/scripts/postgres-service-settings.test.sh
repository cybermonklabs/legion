#!/usr/bin/env bash
# Tests for `postgres-service-settings.sh`: the command line every CI Postgres service starts with,
# and the read-back of it from the running server that each such job's "Read the service's
# settings back" step runs. --command prints the line; a server on every setting, with a 1 GiB
# /dev/shm, passes and prints what it read; a server off any of them fails the step, naming the
# service and every value that is off; a read docker cannot make, an empty container id and a
# wrong call each fail it by name.
#
# Each case runs the real script with a fake `docker` first on PATH, which records every call. It
# answers `docker exec <id> psql -U postgres -XAt -v ON_ERROR_STOP=1 -c "show <a>" -c "show <b>"
# ...` with one line per `show`, the value the command line asks for unless FAKE_PG_VALUES names
# another, and `docker inspect --format {{.HostConfig.ShmSize}} <id>` with 1073741824 for the
# container c0ffee (unless FAKE_SHM says otherwise) and with docker's default of 67108864 for any
# other, so an inspect of the wrong container fails as it would on a runner. FAKE_DOCKER_FAIL=exec
# or =inspect makes that call fail, and FAKE_PG_LINES=<n> keeps only the first n lines of the
# exec's answer.
#
# Run from anywhere: .github/scripts/postgres-service-settings.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$script_dir/postgres-service-settings.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

mkdir -p "$work/bin"
cat > "$work/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_DOCKER_LOG"
case $1 in
  exec)
    [ "${FAKE_DOCKER_FAIL:-}" != exec ] || { echo "Error response from daemon: container $2 is not running" >&2; exit 1; }
    [ "$3 $4 $5 $6 $7 $8" = "psql -U postgres -XAt -v ON_ERROR_STOP=1" ] || { echo "fake docker: unexpected exec: $*" >&2; exit 2; }
    shift 8
    lines=()
    while [ "$#" -gt 0 ]; do
      [ "$1" = -c ] || { echo "fake docker: expected -c, got $1" >&2; exit 2; }
      setting=${2#show }
      case $setting in
        fsync | full_page_writes | synchronous_commit) value=off ;;
        max_connections) value=1000 ;;
        max_prepared_transactions) value=10 ;;
        shared_memory_size) value=171MB ;;
        *) echo "fake docker: no setting $setting" >&2; exit 2 ;;
      esac
      for pair in ${FAKE_PG_VALUES:-}; do
        [ "${pair%%=*}" != "$setting" ] || value=${pair#*=}
      done
      lines+=("$value")
      shift 2
    done
    printf '%s\n' "${lines[@]:0:${FAKE_PG_LINES:-${#lines[@]}}}"
    ;;
  inspect)
    [ "${FAKE_DOCKER_FAIL:-}" != inspect ] || { echo "Error: No such object: $4" >&2; exit 1; }
    [ "$#" -eq 4 ] && [ "$2 $3" = "--format {{.HostConfig.ShmSize}}" ] || { echo "fake docker: unexpected inspect: $*" >&2; exit 2; }
    if [ "$4" = c0ffee ]; then echo "${FAKE_SHM:-1073741824}"; else echo 67108864; fi
    ;;
  *) echo "fake docker: unexpected call: $*" >&2; exit 2 ;;
esac
EOF
chmod +x "$work/bin/docker"

# run [VAR=value ...] -- <arguments>: the script under the fake docker, its output and status in
# $out and $status, and what it asked docker in $calls.
run() {
  local env=()
  while [ "$1" != -- ]; do
    env+=("$1")
    shift
  done
  shift
  : > "$work/docker.log"
  set +e
  out=$(env PATH="$work/bin:$PATH" FAKE_DOCKER_LOG="$work/docker.log" "${env[@]}" "$script" "$@" 2>&1)
  status=$?
  set -e
  calls=$(cat "$work/docker.log")
}

all="fsync=off full_page_writes=off synchronous_commit=off max_connections=1000 max_prepared_transactions=10 shm_size=1073741824"

echo "case: --command prints the server's command line"
run -- --command
check "exits 0" "$(is "$status" 0)"
check "prints postgres and one -c per setting, in the read-back's order" \
  "$(is "$out" "postgres -c fsync=off -c full_page_writes=off -c synchronous_commit=off -c max_connections=1000 -c max_prepared_transactions=10")"
check "asks docker nothing" "$(is "$calls" "")"

echo "case: a server on every setting, with a 1 GiB /dev/shm"
run -- c0ffee postgres
check "exits 0" "$(is "$status" 0)"
check "prints the service, the container, each value as read and the shared memory it costs" \
  "$(is "$out" "postgres: c0ffee: $all (shared_memory_size=171MB)")"
check "reads every setting in one docker exec, and the shm size in one docker inspect" \
  "$(is "$(grep -c '' <<<"$calls")" 2)"
check "asks the named container with show" \
  "$(contains "$calls" '^exec c0ffee psql -U postgres -XAt -v ON_ERROR_STOP=1 -c show fsync .* -c show max_prepared_transactions -c show shared_memory_size$')"
check "and inspects that container's /dev/shm" \
  "$(contains "$calls" '^inspect --format {{.HostConfig.ShmSize}} c0ffee$')"

echo "case: a server on the image's default for one setting"
run FAKE_PG_VALUES=fsync=on -- c0ffee postgres
check "exits 1" "$(is "$status" 1)"
check "names the service, the setting, what it read and what it wants" \
  "$(contains "$out" '^::error::the Postgres service postgres reports fsync=on, not off$')"
check "and prints what it read" "$(contains "$out" '^postgres: c0ffee: fsync=on full_page_writes=off ')"

echo "case: a server on the image's default for full_page_writes"
run FAKE_PG_VALUES=full_page_writes=on -- c0ffee postgres
check "exits 1" "$(is "$status" 1)"
check "names the setting, what it read and what it wants" \
  "$(contains "$out" '^::error::the Postgres service postgres reports full_page_writes=on, not off$')"
check "and refuses nothing else" "$(is "$(grep -c '::error::' <<<"$out")" 1)"

echo "case: a server on the image's default for synchronous_commit"
run FAKE_PG_VALUES=synchronous_commit=on -- c0ffee postgres
check "exits 1" "$(is "$status" 1)"
check "names the setting, what it read and what it wants" \
  "$(contains "$out" '^::error::the Postgres service postgres reports synchronous_commit=on, not off$')"
check "and refuses nothing else" "$(is "$(grep -c '::error::' <<<"$out")" 1)"

echo "case: a server off two settings, as a service with no command: line runs"
run FAKE_PG_VALUES="max_connections=100 max_prepared_transactions=0" -- c0ffee db
check "exits 1" "$(is "$status" 1)"
check "names the first" "$(contains "$out" '^::error::the Postgres service db reports max_connections=100, not 1000$')"
check "names the second" "$(contains "$out" '^::error::the Postgres service db reports max_prepared_transactions=0, not 10$')"
check "and no value that is as asked" "$(is "$(grep -c '::error::' <<<"$out")" 2)"

echo "case: a container with docker's default /dev/shm"
run FAKE_SHM=67108864 -- c0ffee postgres
check "exits 1" "$(is "$status" 1)"
check "names the service, what it read and what it wants" \
  "$(contains "$out" '^::error::the Postgres service postgres has a /dev/shm of 67108864 bytes, not 1073741824 (--shm-size=1g)$')"
check "and refuses nothing else" "$(is "$(grep -c '::error::' <<<"$out")" 1)"

echo "case: a docker exec that fails"
run FAKE_DOCKER_FAIL=exec -- c0ffee postgres
check "exits 1" "$(is "$status" 1)"
check "names the service and the container" \
  "$(contains "$out" '^::error::postgres: docker exec into c0ffee could not read the server.s settings$')"
check "stops there, printing no read-back line" "$(is "$(contains "$out" '^postgres: c0ffee:')" false)"

echo "case: an answer with a line missing"
run FAKE_PG_LINES=3 -- c0ffee postgres
check "exits 1" "$(is "$status" 1)"
check "says how many lines it read for how many settings" \
  "$(contains "$out" '^::error::postgres: the server in c0ffee answered 3 lines for 6 settings')"
check "prints no read-back line" "$(is "$(contains "$out" '^postgres: c0ffee:')" false)"

echo "case: a docker inspect that fails"
run FAKE_DOCKER_FAIL=inspect -- c0ffee postgres
check "exits 1" "$(is "$status" 1)"
check "names the service and the container" \
  "$(contains "$out" '^::error::postgres: docker inspect could not read the shared memory size of c0ffee$')"

echo "case: an empty container id, a service the job does not declare"
run -- "" postgres_events
check "exits 1" "$(is "$status" 1)"
check "names the service and says why the id is empty" \
  "$(contains "$out" '^::error::postgres_events: the container id is empty')"
check "asks docker nothing" "$(is "$calls" "")"

echo "case: a call without two arguments"
run -- c0ffee
check "one argument exits 1" "$(is "$status" 1)"
check "and says how it is called" "$(contains "$out" 'takes --command, or two arguments, the service.s')"
run -- c0ffee postgres extra
check "three arguments exit 1" "$(is "$status" 1)"
run -- --command extra
check "--command with an argument exits 1" "$(is "$status" 1)"
check "and says how it is called" "$(contains "$out" 'takes --command, or two arguments, the service.s')"
check "and asks docker nothing" "$(is "$calls" "")"
run -- c0ffee ""
check "an empty service id exits 1" "$(is "$status" 1)"
check "asks docker nothing" "$(is "$calls" "")"

summary "postgres-service-settings.sh"
