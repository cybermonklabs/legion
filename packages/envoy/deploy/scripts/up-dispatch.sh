#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."
# The SPA compiles into the image (bun stage in docker/Dockerfile) — no host
# build, no bind mount. ENVOY_IMAGE_TAG is required by the compose file; a
# local build doesn't care about its value, so default it here. To run a
# published tag instead of building locally, set ENVOY_IMAGE_TAG and run
# `docker compose -f compose/dispatch.compose.yml pull && docker compose -f
# compose/dispatch.compose.yml up -d` directly.
export ENVOY_IMAGE_TAG="${ENVOY_IMAGE_TAG:-local}"
# Dispatch starts only with its identity at the Envoy listener, and its container otherwise
# restart-loops under `restart: unless-stopped` while `up -d` exits 0, so a start Compose resolves
# neither setting for is refused here. Both are read from Compose's own resolution, never from the
# .env's text: its interpolation environment (`docker compose config --environment`), from which
# dispatch.compose.yml gives the container each setting exactly when it is not empty
# (`${ENVOY_ACCOUNT_GOOGLE_AUDIENCE:-}`, `${ENVOY_ACCOUNT_TOKEN_FILE:+...}`). A refusal prints no text
# of Compose's (which can begin with a warning and can quote a .env line whole, a secret with it):
# only the names in its `required variable <NAME> is missing a value` lines, or else its exit status
# and the command that shows its message.
if ! docker compose version >/dev/null 2>&1; then
  echo "up-dispatch.sh: docker compose cannot run (docker compose version failed); it is needed to start Dispatch, and to read its settings" >&2
  exit 1
fi
err_file=$(mktemp)
status=0
answer=$(docker compose -f compose/dispatch.compose.yml config --environment 2>"$err_file") || status=$?
if ((status != 0)); then
  missing=()
  while IFS= read -r line; do
    if [[ "$line" =~ required\ variable\ ([A-Za-z_][A-Za-z0-9_]*)\ is\ missing\ a\ value ]] &&
      [[ " ${missing[*]} " != *" ${BASH_REMATCH[1]} "* ]]; then
      missing+=("${BASH_REMATCH[1]}")
    fi
  done <"$err_file"
  rm -f "$err_file"
  if ((${#missing[@]})); then
    reason="a required variable is missing a value: ${missing[*]}"
  else
    reason="docker compose config exited $status; its message is not printed, as it can quote a setting's value; run \`docker compose -f compose/dispatch.compose.yml config\` in $PWD to read it"
  fi
  echo "up-dispatch.sh: Compose refused compose/dispatch.compose.yml: $reason" >&2
  exit 1
fi
rm -f "$err_file"
identity=
while IFS= read -r line; do
  case $line in
    ENVOY_ACCOUNT_GOOGLE_AUDIENCE=?* | ENVOY_ACCOUNT_TOKEN_FILE=?*) identity="set" ;;
  esac
done <<<"$answer"
if [[ -z "$identity" ]]; then
  echo "up-dispatch.sh: Compose resolves neither ENVOY_ACCOUNT_GOOGLE_AUDIENCE nor ENVOY_ACCOUNT_TOKEN_FILE, and Dispatch refuses to start without its identity at the Envoy listener; set one in the env file beside compose/dispatch.compose.yml (see deploy/README.md, \"Dispatch's identity at the Envoy listener\")" >&2
  exit 1
fi
docker compose -f compose/dispatch.compose.yml up -d --build
