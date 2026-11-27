#!/usr/bin/env bash
set -euo pipefail
# The identity provider the harness's Dispatch servers sign people in at under
# DISPATCH_E2E_SIGNIN=oidc or google (e2e/harness-signin.ts): packages/envoy/cmd/dev-oidc-issuer,
# serving the authorization code flow for one confidential client on a loopback port the OS
# chooses. Under oidc it signs in DISPATCH_E2E_SIGNIN_AS at every authorization (nothing queues
# another person), as a member of the sign-in pool group DISPATCH_E2E_SIGNIN_GROUP, the group the
# servers admit; both are required then. Under google it stands in for Google and signs in nobody
# of its own: each authorization signs in the person a spec queued at the issuer's
# POST /_dev/sign-in-as, with the claims Google gives that account, and one with nobody queued is
# refused, so it takes neither. Once it serves it reports
#   dev-oidc-issuer: issuer listening url=http://127.0.0.1:<port>
# on stderr, the line e2e/harness-start.ts reads the port from and hands to run-server.sh as
# DISPATCH_E2E_SIGNIN_ISSUER_PORT. The issuer has no way to bind a given port, so that variable
# set here is refused rather than ignored.
#
# DISPATCH_E2E_SIGNIN_CLIENT_ID and DISPATCH_E2E_SIGNIN_CLIENT_SECRET are the client every server
# is handed; the issuer reads the secret from a file once, at its start, here a pipe this script
# writes it to, so the secret never reaches an argv or the disk. The client's one callback is the
# servers' /auth/callback on the loopback dashboard origin, which the issuer takes at whatever
# port that origin is on.
: "${DISPATCH_E2E_SIGNIN_CLIENT_ID:?DISPATCH_E2E_SIGNIN_CLIENT_ID must name the Dispatch client at the dev OIDC issuer}"
: "${DISPATCH_E2E_SIGNIN_CLIENT_SECRET:?DISPATCH_E2E_SIGNIN_CLIENT_SECRET must give the secret of that client}"
if [ -n "${DISPATCH_E2E_SIGNIN_ISSUER_PORT:-}" ]; then
  echo "run-signin-issuer.sh: the dev OIDC issuer serves on a port the OS chooses; unset DISPATCH_E2E_SIGNIN_ISSUER_PORT" >&2
  exit 2
fi
case "${DISPATCH_E2E_SIGNIN:-}" in
  oidc)
    : "${DISPATCH_E2E_SIGNIN_AS:?DISPATCH_E2E_SIGNIN_AS must name the person the dev OIDC issuer signs in}"
    : "${DISPATCH_E2E_SIGNIN_GROUP:?DISPATCH_E2E_SIGNIN_GROUP must name the sign-in pool group the Dispatch servers admit}"
    people=(--sign-in-as "$DISPATCH_E2E_SIGNIN_AS" --sign-in-group "$DISPATCH_E2E_SIGNIN_GROUP")
    ;;
  google)
    people=()
    ;;
  *)
    echo "run-signin-issuer.sh: DISPATCH_E2E_SIGNIN must be oidc or google, not '${DISPATCH_E2E_SIGNIN:-}'" >&2
    exit 2
    ;;
esac
secret="$DISPATCH_E2E_SIGNIN_CLIENT_SECRET"
client_id="$DISPATCH_E2E_SIGNIN_CLIENT_ID"
unset DISPATCH_E2E_SIGNIN_CLIENT_SECRET

cd "$(dirname "$0")/../../envoy"
# Built and exec'd as run-server.sh builds and execs the server, so the pid a caller started is the
# issuer's: one binary per checkout, under a lock of its own, rewritten only when the source changed.
flock ./.dev-oidc-issuer-e2e.lock go build -o ./dev-oidc-issuer-e2e ./cmd/dev-oidc-issuer
exec ./dev-oidc-issuer-e2e \
  --code-flow "$client_id="<(printf '%s' "$secret")"=http://127.0.0.1/auth/callback" \
  "${people[@]}"
