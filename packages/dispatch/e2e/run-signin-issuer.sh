#!/usr/bin/env bash
set -euo pipefail
# The identity provider the harness's Dispatch servers sign people in at under
# DISPATCH_E2E_SIGNIN=oidc (e2e/harness-signin.ts): packages/envoy/cmd/dev-oidc-issuer, serving
# the authorization code flow for one confidential client on a loopback port the OS chooses. It
# signs in DISPATCH_E2E_SIGNIN_AS at every authorization (nothing queues another person), as a
# member of the sign-in pool group DISPATCH_E2E_SIGNIN_GROUP, the group the servers admit. Once it
# serves it reports
#   dev-oidc-issuer: issuer listening url=http://127.0.0.1:<port>
# on stderr, the line e2e/harness-start.ts reads the port from and hands to run-server.sh as
# DISPATCH_E2E_SIGNIN_ISSUER_PORT. The issuer has no way to bind a given port, so that variable
# set here is refused rather than ignored.
#
# DISPATCH_E2E_SIGNIN_CLIENT_ID and DISPATCH_E2E_SIGNIN_CLIENT_SECRET are the client both servers
# are handed; the issuer reads the secret from a file once, at its start, here a pipe this script
# writes it to, so the secret never reaches an argv or the disk.
: "${DISPATCH_E2E_SIGNIN_AS:?DISPATCH_E2E_SIGNIN_AS must name the person the dev OIDC issuer signs in}"
: "${DISPATCH_E2E_SIGNIN_CLIENT_ID:?DISPATCH_E2E_SIGNIN_CLIENT_ID must name the Dispatch client at the dev OIDC issuer}"
: "${DISPATCH_E2E_SIGNIN_CLIENT_SECRET:?DISPATCH_E2E_SIGNIN_CLIENT_SECRET must give the secret of that client}"
: "${DISPATCH_E2E_SIGNIN_GROUP:?DISPATCH_E2E_SIGNIN_GROUP must name the sign-in pool group the Dispatch servers admit}"
if [ -n "${DISPATCH_E2E_SIGNIN_ISSUER_PORT:-}" ]; then
  echo "run-signin-issuer.sh: the dev OIDC issuer serves on a port the OS chooses; unset DISPATCH_E2E_SIGNIN_ISSUER_PORT" >&2
  exit 2
fi
secret="$DISPATCH_E2E_SIGNIN_CLIENT_SECRET"
client_id="$DISPATCH_E2E_SIGNIN_CLIENT_ID"
person="$DISPATCH_E2E_SIGNIN_AS"
group="$DISPATCH_E2E_SIGNIN_GROUP"
unset DISPATCH_E2E_SIGNIN_CLIENT_SECRET

cd "$(dirname "$0")/../../envoy"
# Built and exec'd as run-server.sh builds and execs the server, so the pid a caller started is the
# issuer's: one binary per checkout, under a lock of its own (the subshell's fd 9, held for the build
# alone), rewritten only when the source changed. The build reads the module cache alone
# (go_build_offline, GOPROXY=off), as run-server.sh's does and for its reason: each CI job whose
# tests start this script fetches the issuer's modules in a step before them.
# shellcheck source=.github/scripts/go-build-offline.sh
source ../../.github/scripts/go-build-offline.sh
( flock 9 && go_build_offline packages/envoy ./dev-oidc-issuer-e2e ./cmd/dev-oidc-issuer ) 9>./.dev-oidc-issuer-e2e.lock || exit 1
exec ./dev-oidc-issuer-e2e \
  --code-flow "$client_id="<(printf '%s' "$secret") \
  --sign-in-as "$person" \
  --sign-in-group "$group"
