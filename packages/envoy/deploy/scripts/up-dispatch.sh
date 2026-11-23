#!/bin/bash
set -euo pipefail

# A person runs this at a terminal (deploy/README.md); nothing unattended calls it, the auto-deployer
# (autodeploy.sh) included. So its Compose calls have no time limit: a Compose that does not answer
# is the person's to stop, where the deployer bounds its own reads at 60 s.
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
# .env's text: the dispatch service's environment in `docker compose config --format json` (which
# Compose has had since before 2.12.2, the oldest release it was measured on; `--environment`
# needs 2.28), where dispatch.compose.yml gives the container each setting exactly when it is not
# empty (`${ENVOY_ACCOUNT_GOOGLE_AUDIENCE:-}`, `${ENVOY_ACCOUNT_TOKEN_FILE:+...}`). A refusal prints
# no text of Compose's (compose_refusal, which autodeploy.sh shares).
# shellcheck source=SCRIPTDIR/compose-refusal.sh
. scripts/compose-refusal.sh
if ! docker compose version >/dev/null 2>&1; then
  echo "up-dispatch.sh: docker compose cannot run (docker compose version failed); it is needed to start Dispatch, and to read its settings" >&2
  exit 1
fi
if ! command -v python3 >/dev/null; then
  echo "up-dispatch.sh: python3 is not on PATH; it reads Dispatch's settings from docker compose config" >&2
  exit 1
fi
err_file=$(mktemp)
status=0
answer=$(docker compose -f compose/dispatch.compose.yml config --format json 2>"$err_file") || status=$?
if ((status != 0)); then
  reason=$(compose_refusal "$err_file" "$status" "docker compose -f compose/dispatch.compose.yml config")
  rm -f "$err_file"
  echo "up-dispatch.sh: Compose refused compose/dispatch.compose.yml (in $PWD): $reason" >&2
  exit 1
fi
rm -f "$err_file"
# The identity is set when Compose gives the container either setting non-empty; a JSON that holds
# no dispatch service is refused by name, never read as no identity.
identity=$(python3 -c '
import json, sys
try:
    environment = json.load(sys.stdin)["services"]["dispatch"].get("environment") or {}
except (ValueError, KeyError, TypeError, AttributeError):
    sys.exit(2)
print("set" if environment.get("ENVOY_ACCOUNT_GOOGLE_AUDIENCE") or environment.get("ENVOY_ACCOUNT_TOKEN_FILE") else "")
' <<<"$answer") || {
  echo "up-dispatch.sh: could not read the dispatch service's environment from docker compose config" >&2
  exit 1
}
if [[ -z "$identity" ]]; then
  echo "up-dispatch.sh: Compose resolves neither ENVOY_ACCOUNT_GOOGLE_AUDIENCE nor ENVOY_ACCOUNT_TOKEN_FILE, and Dispatch refuses to start without its identity at the Envoy listener; set one in the env file beside compose/dispatch.compose.yml (see deploy/README.md, \"Dispatch's identity at the Envoy listener\")" >&2
  exit 1
fi
docker compose -f compose/dispatch.compose.yml up -d --build
