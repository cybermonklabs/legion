#!/usr/bin/env bash
set -euo pipefail
# The token issuer the harness's Dispatch server verifies the secrets broker's projected
# service-account token against under DISPATCH_E2E_ACCESS_ISSUER=1 (e2e/harness-access.ts):
# packages/envoy/cmd/dev-oidc-issuer, the stand-in for a cluster's ServiceAccount token issuer, on a
# loopback port the OS chooses. It keeps one token in DISPATCH_E2E_ACCESS_TOKEN_FILE, the path the
# harness names in a directory of its own (e2e/harness-start.ts): the account and audience of
# e2e/access-account.sh, minted for an hour and rewritten in place before it expires, as the
# kubelet keeps a pod's projected token. Once the file is written it reports
#   dev-oidc-issuer: issuer listening url=http://127.0.0.1:<port>
# on stderr, the line e2e/harness-start.ts reads the port from and hands to run-server.sh as
# DISPATCH_E2E_ACCESS_ISSUER_PORT. The issuer has no way to bind a given port, so that variable set
# here is refused rather than ignored, as e2e/run-signin-issuer.sh refuses its own.
: "${DISPATCH_E2E_ACCESS_TOKEN_FILE:?DISPATCH_E2E_ACCESS_TOKEN_FILE must name the file the dev OIDC issuer keeps the secrets broker token in}"
if [ -n "${DISPATCH_E2E_ACCESS_ISSUER_PORT:-}" ]; then
  echo "run-access-issuer.sh: the dev OIDC issuer serves on a port the OS chooses; unset DISPATCH_E2E_ACCESS_ISSUER_PORT" >&2
  exit 2
fi
# The issuer writes the file from packages/envoy, where this script changes directory below, so a
# path relative to the caller's directory is made absolute first.
token_file="$DISPATCH_E2E_ACCESS_TOKEN_FILE"
[[ "$token_file" == /* ]] || token_file="$PWD/$token_file"
# shellcheck source-path=SCRIPTDIR source=access-account.sh
. "$(dirname "$0")/access-account.sh"

cd "$(dirname "$0")/../../envoy"
# The binary and lock e2e/run-signin-issuer.sh builds, so a harness that runs both issuers builds
# it once; exec'd, so the pid a caller started is the issuer's.
flock ./.dev-oidc-issuer-e2e.lock go build -o ./dev-oidc-issuer-e2e ./cmd/dev-oidc-issuer
exec ./dev-oidc-issuer-e2e --token "$access_audience=$access_account=$token_file"
