#!/usr/bin/env bash
# up-dispatch.sh refuses to start Dispatch when Compose resolves neither of its identity settings,
# reading them from Compose's own resolution (`docker compose config`). The script runs from a copy
# of the deploy directory, with the real compose file, so no real compose/.env is read. A docker on
# PATH hands `compose version` and `compose ... config ...` to the real binary and records every
# other command, running nothing.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
real_docker=$(command -v docker || true)
[[ -n "$real_docker" ]] || { printf 'up-dispatch: docker is not on PATH; this test reads the resolution of docker compose config\n' >&2; exit 1; }
"$real_docker" compose version >/dev/null || { printf 'up-dispatch: docker compose version failed with %s; this test needs Docker Compose\n' "$real_docker" >&2; exit 1; }
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/deploy/scripts" "$root/deploy/compose" "$root/bin"
cp "$here/up-dispatch.sh" "$here/compose-refusal.sh" "$root/deploy/scripts/"
cp "$here/../compose/dispatch.compose.yml" "$root/deploy/compose/"
cat >"$root/bin/docker" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  "compose version"* | *" config "*) exec "$UP_DISPATCH_TEST_REAL_DOCKER" "$@" ;;
esac
printf '%s\n' "$*" >>"$UP_DISPATCH_TEST_LOG"
EOF
chmod +x "$root/bin/docker"
log="$root/docker.log"
output=

fail() {
  printf 'up-dispatch: %s\n--- output:\n%s\n--- docker:\n%s\n' "$1" "$output" "$(cat "$log" 2>/dev/null)" >&2
  exit 1
}
# The database's canary password, which holds an @ as the deployer test's does: no output may
# carry it.
canary='s@cret-canary'
# up_with LINE [VAR=value...] runs up-dispatch.sh with compose/.env holding the settings the compose
# file requires and LINE, with no identity setting in the environment unless a VAR=value names it.
# test_path, when set, is the PATH the script sees: the missing-python row uses it to hide python3.
up_with() {
  local line=$1
  shift
  cat >"$root/deploy/compose/.env" <<EOF
DATABASE_URL=postgres://dispatch:${canary//@/%40}@db.test:5432/dispatch
DISPATCH_AGENT_TOKEN=agent-token
DISPATCH_SERVER_URL=http://browser.test:8766
NATS_URLS=nats://nats.test:4222
$line
EOF
  : >"$log"
  env -u ENVOY_ACCOUNT_GOOGLE_AUDIENCE -u ENVOY_ACCOUNT_TOKEN_FILE -u UP_DISPATCH_TEST_UNSET "$@" \
    PATH="${test_path:-$root/bin:$PATH}" UP_DISPATCH_TEST_LOG="$log" UP_DISPATCH_TEST_REAL_DOCKER="${test_real_docker:-$real_docker}" \
    bash "$root/deploy/scripts/up-dispatch.sh" 2>&1
}
started='compose -f compose/dispatch.compose.yml up -d --build'

for name in ENVOY_ACCOUNT_GOOGLE_AUDIENCE ENVOY_ACCOUNT_TOKEN_FILE; do
  # Compose resolves these empty: the start is refused naming both settings, and nothing is started.
  for spelling in "$name=\"\"" "$name=\"\${UP_DISPATCH_TEST_UNSET:-}\""; do
    if output=$(up_with "$spelling"); then
      fail "started Dispatch with $spelling in compose/.env, which Compose resolves empty"
    fi
    if [[ "$output" != *"neither ENVOY_ACCOUNT_GOOGLE_AUDIENCE nor ENVOY_ACCOUNT_TOKEN_FILE"* ]] || [[ -s "$log" ]]; then
      fail "did not refuse $spelling by name before starting anything"
    fi
  done
  # Compose resolves these to the value: Dispatch is started.
  for spelling in "export $name=envoy-listener" "$name=envoy-listener # a note" "$name=\${UP_DISPATCH_TEST_VALUE}"; do
    output=$(up_with "$spelling" UP_DISPATCH_TEST_VALUE=envoy-listener) ||
      fail "refused $spelling in compose/.env, which Compose resolves to envoy-listener"
    [[ $(<"$log") == "$started" ]] || fail "did not start Dispatch with $spelling in compose/.env"
  done
done

# The shell's value wins, as Compose takes it: one set empty refuses though compose/.env sets the
# setting, and one set in the shell starts with a compose/.env that sets neither.
if output=$(up_with "ENVOY_ACCOUNT_GOOGLE_AUDIENCE=envoy-listener" ENVOY_ACCOUNT_GOOGLE_AUDIENCE=); then
  fail "started Dispatch with ENVOY_ACCOUNT_GOOGLE_AUDIENCE set empty in the shell, which Compose uses over compose/.env"
fi
[[ ! -s "$log" ]] || fail "started Dispatch with ENVOY_ACCOUNT_GOOGLE_AUDIENCE set empty in the shell"
output=$(up_with "# no identity here" ENVOY_ACCOUNT_TOKEN_FILE=/srv/token) ||
  fail "refused ENVOY_ACCOUNT_TOKEN_FILE set in the shell"
[[ $(<"$log") == "$started" ]] || fail "did not start Dispatch with ENVOY_ACCOUNT_TOKEN_FILE set in the shell"

# A refusal prints no text of Compose's. A required variable missing a value behind a `${...}`
# warning is named alone; an unterminated quote around a password (one with an @, one with a space)
# or around the agent token prints no value, and names the command that shows Compose's message.
# The second .env line is written as the file holds it, `${...}` unexpanded.
# shellcheck disable=SC2016
if output=$(up_with $'ENVOY_ACCOUNT_GOOGLE_AUDIENCE=envoy-listener\nDISPATCH_PORT=${UP_DISPATCH_TEST_UNSET}' NATS_URLS=); then
  fail "started Dispatch with NATS_URLS empty, which Compose refuses"
fi
[[ "$output" == *"Compose refused"*"a required variable is missing a value: NATS_URLS"* && "$output" != *level=warning* && ! -s "$log" ]] ||
  fail "did not name NATS_URLS alone when Compose warned first"
for line in "DATABASE_URL=\"postgres://dispatch:$canary@db.test:5432/dispatch" \
  "DATABASE_URL=\"postgres://dispatch@db.test:5432/dispatch?password='se cret'" 'DISPATCH_AGENT_TOKEN="agent-token-CANARY-1234'; do
  if output=$(up_with "$line"); then
    fail "started Dispatch with an unterminated quote in ${line%%=*}"
  fi
  [[ "$output" == *"Compose refused"*"docker compose config exited "[0-9]*"; its message is not shown"*"docker compose -f compose/dispatch.compose.yml config"* && ! -s "$log" ]] ||
    fail "did not refuse an unterminated quote in ${line%%=*} with the command to read Compose's message"
  [[ "$output" != *cret* && "$output" != *CANARY* ]] || fail "the refusal carries a value of ${line%%=*}: $output"
done

# Without `docker compose`, the script refuses by name.
if output=$(test_real_docker=/bin/false up_with "ENVOY_ACCOUNT_GOOGLE_AUDIENCE=envoy-listener"); then
  fail "started Dispatch when docker compose cannot run"
fi
[[ "$output" == *"docker compose cannot run"* && ! -s "$log" ]] ||
  fail "did not refuse by name when docker compose cannot run"

# `python3` first has to be on PATH, then has to read Compose's JSON. Both refusals stop before up.
mkdir "$root/no-python"
ln -s "$(command -v bash)" "$root/no-python/bash"
ln -s "$(command -v dirname)" "$root/no-python/dirname"
test_path="$root/bin:$root/no-python"
if output=$(up_with "ENVOY_ACCOUNT_GOOGLE_AUDIENCE=envoy-listener"); then
  fail 'started Dispatch without python3 on PATH'
fi
[[ "$output" == *"python3 is not on PATH"* && ! -s "$log" ]] ||
  fail 'did not refuse by name without python3 on PATH'
unset test_path
cat >"$root/bin/python3" <<'EOF'
#!/usr/bin/env bash
exit 3
EOF
chmod +x "$root/bin/python3"
if output=$(up_with "ENVOY_ACCOUNT_GOOGLE_AUDIENCE=envoy-listener"); then
  fail 'started Dispatch when python3 failed to read Compose JSON'
fi
[[ "$output" == *"could not read the dispatch service's environment from docker compose config"* && ! -s "$log" ]] ||
  fail 'did not refuse by name when python3 failed to read Compose JSON'
rm "$root/bin/python3"

# The script reads only through `docker compose config --format json`, so it runs on Compose
# releases that lack `config --environment` (before 2.28): it starts Dispatch through Compose 2.12.2,
# the oldest release measured, taken by digest out of this repository's copy of the image that
# ships it (compose-2.12.2_test.sh), through a `docker` that hands `compose ...` to it and counts
# the calls.
# shellcheck source=SCRIPTDIR/compose-2.12.2_test.sh
. "$here/compose-2.12.2_test.sh"
compose_2_12="$root/docker-compose-2.12.2"
take_compose_2_12 "$real_docker" "$compose_2_12" ||
  fail 'UNKNOWN: could not take Compose 2.12.2 out of its image, so the oldest release was not judged'
cat >"$root/docker-2.12" <<EOF
#!/usr/bin/env bash
[[ "\$1" == compose ]] || exec "$real_docker" "\$@"
shift
printf '%s\n' "\$*" >>"$root/compose-2.12.calls"
exec "$compose_2_12" "\$@"
EOF
chmod +x "$root/docker-2.12"
: >"$root/compose-2.12.calls"
output=$(test_real_docker="$root/docker-2.12" up_with "ENVOY_ACCOUNT_GOOGLE_AUDIENCE=envoy-listener") ||
  fail "refused to start Dispatch through Compose 2.12.2"
[[ $(<"$log") == "$started" ]] || fail "did not start Dispatch through Compose 2.12.2"
grep -q ' config --format json' "$root/compose-2.12.calls" || fail 'UNKNOWN: Compose 2.12.2 served no config read'

printf 'up-dispatch: refusal and start for each spelling, the shell override, Compose'"'"'s refusal, no Compose and Compose 2.12.2: PASS\n'
