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
# restart-loops under `restart: unless-stopped` while `up -d` exits 0, so a start with neither
# setting, in this environment or in compose/.env (which compose reads), is refused here.
identity_set() {
  [[ -n "${!1:-}" ]] || { [[ -f compose/.env ]] && grep -qE "^$1=.+" compose/.env; }
}
if ! identity_set ENVOY_ACCOUNT_GOOGLE_AUDIENCE && ! identity_set ENVOY_ACCOUNT_TOKEN_FILE; then
  echo "up-dispatch.sh: neither ENVOY_ACCOUNT_GOOGLE_AUDIENCE nor ENVOY_ACCOUNT_TOKEN_FILE is set, and Dispatch refuses to start without its identity at the Envoy listener; set one in compose/.env (see deploy/README.md, \"Dispatch's identity at the Envoy listener\")" >&2
  exit 1
fi
docker compose -f compose/dispatch.compose.yml up -d --build
