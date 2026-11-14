#!/usr/bin/env bash
# Tests for `postgres-service-settings.sh`: the read-back of a job's Postgres service settings
# that thirteen steps of ci.yaml call. A server on all five settings passes and prints them; a
# server off any of them fails the step, naming the service and every setting that is off; a
# setting docker cannot read, an empty container id and a wrong call each fail it by name.
#
# Each case runs the real script with a fake `docker` first on PATH, which records every call and
# answers `docker exec <id> psql -U postgres -Atc "show <setting>"` with that setting's value: the
# one the services' command: line asks for, unless FAKE_PG_VALUES names another, and no answer at
# all for the setting FAKE_PG_UNREADABLE names.
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
[ "$1 $3 $4 $5 $6" = "exec psql -U postgres -Atc" ] || { echo "fake docker: unexpected call: $*" >&2; exit 2; }
setting=${7#show }
[ "$setting" != "${FAKE_PG_UNREADABLE:-}" ] || { echo "Error response from daemon: container $2 is not running" >&2; exit 1; }
case $setting in
  fsync | full_page_writes | synchronous_commit) value=off ;;
  max_connections) value=500 ;;
  max_prepared_transactions) value=20 ;;
  *) echo "fake docker: no setting $setting" >&2; exit 2 ;;
esac
for pair in ${FAKE_PG_VALUES:-}; do
  [ "${pair%%=*}" != "$setting" ] || value=${pair#*=}
done
echo "$value"
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

all_five="fsync=off full_page_writes=off synchronous_commit=off max_connections=500 max_prepared_transactions=20"

echo "case: a server on all five settings"
run -- c0ffee postgres_routes
check "exits 0" "$(is "$status" 0)"
check "prints the label, the container and the five settings as read" \
  "$(is "$out" "postgres_routes: c0ffee: $all_five")"
check "asks the server once per setting" "$(is "$(grep -c '' <<<"$calls")" 5)"
check "asks the named container with show" \
  "$(contains "$calls" '^exec c0ffee psql -U postgres -Atc show max_prepared_transactions$')"

echo "case: a server on the image's default for one setting"
run FAKE_PG_VALUES=fsync=on -- c0ffee postgres
check "exits 1" "$(is "$status" 1)"
check "names the service, the setting, what it read and what it wants" \
  "$(contains "$out" '^::error::the Postgres service postgres reports fsync=on, not off$')"
check "still reads the other four" "$(is "$(grep -c '' <<<"$calls")" 5)"
check "and prints what it read" "$(contains "$out" '^postgres: c0ffee: fsync=on full_page_writes=off ')"

echo "case: a server off two settings"
run FAKE_PG_VALUES="max_connections=100 max_prepared_transactions=0" -- c0ffee postgres_pgmigrate
check "exits 1" "$(is "$status" 1)"
check "names the first" "$(contains "$out" '^::error::the Postgres service postgres_pgmigrate reports max_connections=100, not 500$')"
check "names the second" "$(contains "$out" '^::error::the Postgres service postgres_pgmigrate reports max_prepared_transactions=0, not 20$')"
check "and no setting that is as asked" "$(is "$(grep -c '::error::' <<<"$out")" 2)"

echo "case: a setting docker cannot read"
run FAKE_PG_UNREADABLE=synchronous_commit -- c0ffee postgres_shutdown
check "exits 1" "$(is "$status" 1)"
check "names the service, the container and the setting" \
  "$(contains "$out" '^::error::postgres_shutdown: docker exec into c0ffee could not read synchronous_commit$')"
check "stops there, printing no read-back line" "$(is "$(contains "$out" '^postgres_shutdown: c0ffee:')" false)"

echo "case: an empty container id, a service the job does not declare"
run -- "" postgres_routes
check "exits 1" "$(is "$status" 1)"
check "names the label and says why the id is empty" \
  "$(contains "$out" '^::error::postgres_routes: the container id is empty')"
check "asks docker nothing" "$(is "$calls" "")"

echo "case: a call without two arguments"
run -- c0ffee
check "one argument exits 1" "$(is "$status" 1)"
check "and says how it is called" "$(contains "$out" 'takes two arguments, the service.s container id and its service id')"
run -- c0ffee postgres extra
check "three arguments exit 1" "$(is "$status" 1)"
run -- c0ffee ""
check "an empty label exits 1" "$(is "$status" 1)"
check "asks docker nothing" "$(is "$calls" "")"

summary "postgres-service-settings.sh"
