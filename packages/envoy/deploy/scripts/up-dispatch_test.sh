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
cp "$here/up-dispatch.sh" "$root/deploy/scripts/"
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
    PATH="$root/bin:$PATH" UP_DISPATCH_TEST_LOG="$log" UP_DISPATCH_TEST_REAL_DOCKER="${test_real_docker:-$real_docker}" \
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

# A required variable missing a value is Compose's own refusal, by its line.
if output=$(up_with "ENVOY_ACCOUNT_GOOGLE_AUDIENCE=envoy-listener" NATS_URLS=); then
  fail "started Dispatch with NATS_URLS empty, which Compose refuses"
fi
[[ "$output" == *"Compose refused"*"NATS_URLS"* && ! -s "$log" ]] ||
  fail "did not refuse, by Compose's line, a configuration Compose refuses"
# A DATABASE_URL Compose cannot read is quoted back in Compose's line; its password, which holds an
# @, is cut from what the script prints.
if output=$(up_with "DATABASE_URL=\"postgres://dispatch:$canary@db.test:5432/dispatch"); then
  fail "started Dispatch with a DATABASE_URL Compose cannot read"
fi
[[ "$output" == *"Compose refused"* && ! -s "$log" ]] || fail "did not refuse a DATABASE_URL Compose cannot read"
[[ "$output" != *cret-canary* && "$output" != *cret%40* ]] || fail "the refusal carries the database password: $output"

# Without `docker compose`, the script refuses by name.
if output=$(test_real_docker=/bin/false up_with "ENVOY_ACCOUNT_GOOGLE_AUDIENCE=envoy-listener"); then
  fail "started Dispatch when docker compose cannot run"
fi
[[ "$output" == *"docker compose cannot run"* && ! -s "$log" ]] ||
  fail "did not refuse by name when docker compose cannot run"

printf 'up-dispatch: refusal and start for each spelling, the shell override, Compose'"'"'s refusal and no Compose: PASS\n'
