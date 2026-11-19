#!/usr/bin/env bash
# up-dispatch.sh refuses to start Dispatch with neither of its identity settings, each read as Docker
# Compose reads it: the shell's value when the shell sets the name, an empty one included, and
# compose/.env's otherwise, through dotenv.sh. The script runs from a copy of the deploy directory,
# so no real compose/.env is read, with a docker on PATH that records its arguments and runs nothing.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/deploy/scripts" "$root/deploy/compose" "$root/bin"
cp "$here/up-dispatch.sh" "$here/dotenv.sh" "$root/deploy/scripts/"
cat >"$root/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$UP_DISPATCH_TEST_LOG"
EOF
chmod +x "$root/bin/docker"
log="$root/docker.log"

fail() {
  printf 'up-dispatch: %s\n--- output:\n%s\n--- docker:\n%s\n' "$1" "$output" "$(cat "$log" 2>/dev/null)" >&2
  exit 1
}
# up_with ENV_LINE runs up-dispatch.sh with ENV_LINE as compose/.env's one line, with neither
# identity setting in the environment unless the caller's `env` names it.
up_with() {
  printf '%s\n' "$1" >"$root/deploy/compose/.env"
  : >"$log"
  shift
  env -u ENVOY_ACCOUNT_GOOGLE_AUDIENCE -u ENVOY_ACCOUNT_TOKEN_FILE "$@" \
    PATH="$root/bin:$PATH" UP_DISPATCH_TEST_LOG="$log" bash "$root/deploy/scripts/up-dispatch.sh" 2>&1
}

for name in ENVOY_ACCOUNT_GOOGLE_AUDIENCE ENVOY_ACCOUNT_TOKEN_FILE; do
  # Compose reads these two as empty: the start is refused naming both settings, and docker is not run.
  for spelling in "$name=\"\"" "$name=''"; do
    if output=$(up_with "$spelling"); then
      fail "started Dispatch with $spelling in compose/.env, which Compose reads as empty"
    fi
    if [[ "$output" != *"neither ENVOY_ACCOUNT_GOOGLE_AUDIENCE nor ENVOY_ACCOUNT_TOKEN_FILE"* ]] || [[ -s "$log" ]]; then
      fail "did not refuse $spelling by name before running docker"
    fi
  done
  # Compose reads these two as the value: Dispatch is started.
  for spelling in "export $name=envoy-listener" "$name=envoy-listener # a note"; do
    output=$(up_with "$spelling") || fail "refused $spelling in compose/.env, which Compose reads as envoy-listener"
    if [[ $(<"$log") != "compose -f compose/dispatch.compose.yml up -d --build" ]]; then
      fail "did not start Dispatch with $spelling in compose/.env"
    fi
  done
done

# The shell's value wins, as it does for Compose: one set empty refuses though compose/.env sets
# the setting, and one set in the shell starts with a compose/.env that sets neither.
if output=$(up_with "ENVOY_ACCOUNT_GOOGLE_AUDIENCE=envoy-listener" ENVOY_ACCOUNT_GOOGLE_AUDIENCE=); then
  fail "started Dispatch with ENVOY_ACCOUNT_GOOGLE_AUDIENCE set empty in the shell, which Compose uses over compose/.env"
fi
[[ ! -s "$log" ]] || fail "ran docker with ENVOY_ACCOUNT_GOOGLE_AUDIENCE set empty in the shell"
output=$(up_with "# no identity here" ENVOY_ACCOUNT_TOKEN_FILE=/srv/token) ||
  fail "refused ENVOY_ACCOUNT_TOKEN_FILE set in the shell"
[[ -s "$log" ]] || fail "did not start Dispatch with ENVOY_ACCOUNT_TOKEN_FILE set in the shell"

printf 'up-dispatch: refusal and start for each .env spelling and the shell override: PASS\n'
