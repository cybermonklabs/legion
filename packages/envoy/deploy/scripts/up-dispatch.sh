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
# neither setting for is refused here. Both are read from Compose's own resolution (`docker compose
# config --format json`, the dispatch service's environment), never from the .env's text, so the
# check sees what the container would get.
if ! err=$(docker compose version 2>&1 >/dev/null); then
  echo "up-dispatch.sh: docker compose cannot run (docker compose version: ${err:-no output}); it is needed to start Dispatch, and to read its settings" >&2
  exit 1
fi
err_file=$(mktemp)
if ! identity=$(docker compose -f compose/dispatch.compose.yml config --format json 2>"$err_file" | python3 -c '
import json, sys
environment = json.load(sys.stdin)["services"]["dispatch"].get("environment") or {}
print((environment.get("ENVOY_ACCOUNT_GOOGLE_AUDIENCE") or "") + (environment.get("ENVOY_ACCOUNT_TOKEN_FILE") or ""))
' 2>/dev/null); then
  echo "up-dispatch.sh: Compose refused compose/dispatch.compose.yml: $(head -n 1 "$err_file" | sed -E -e 's#://([^:/@ ]+):.*@#://\1:***@#' -e 's#([Pp][Aa][Ss][Ss][Ww][Oo][Rr][Dd]=)[^ ]*#\1***#g')" >&2
  rm -f "$err_file"
  exit 1
fi
rm -f "$err_file"
if [[ -z "$identity" ]]; then
  echo "up-dispatch.sh: Compose resolves neither ENVOY_ACCOUNT_GOOGLE_AUDIENCE nor ENVOY_ACCOUNT_TOKEN_FILE, and Dispatch refuses to start without its identity at the Envoy listener; set one in compose/.env (see deploy/README.md, \"Dispatch's identity at the Envoy listener\")" >&2
  exit 1
fi
docker compose -f compose/dispatch.compose.yml up -d --build
