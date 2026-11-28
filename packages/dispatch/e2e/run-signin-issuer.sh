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
# writes it to, so the secret never reaches an argv or the disk. The client's one callback is the
# servers' /auth/callback on the loopback dashboard origin, which the issuer takes at whatever
# port that origin is on. DISPATCH_E2E_SIGNIN_CALLBACK names another one, for a caller whose
# servers a person reaches at an address of the caller's own front door
# (e2e/served-addresses.e2e.ts signs in at https://b.example.test/auth/callback): the issuer takes
# that callback exactly, and refuses an authorization that names any other, as Google refuses one.
#
# DISPATCH_E2E_ISSUER_BIN names a prebuilt dev-oidc-issuer this run executes instead of building
# one from this checkout, as run-server.sh's DISPATCH_E2E_SERVER_BIN does for the server: CI's
# shard containers have no Go, and the binaries job of .github/workflows/dispatch-browser.yaml
# builds this one beside the server. Unset, the build below is this checkout's.
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
callback="${DISPATCH_E2E_SIGNIN_CALLBACK:-http://127.0.0.1/auth/callback}"
unset DISPATCH_E2E_SIGNIN_CLIENT_SECRET
# Made absolute against the caller's directory, since the script changes to packages/envoy below.
issuer_bin="${DISPATCH_E2E_ISSUER_BIN:-}"
if [ -n "$issuer_bin" ]; then
  [[ "$issuer_bin" == /* ]] || issuer_bin="$PWD/$issuer_bin"
  if [ ! -x "$issuer_bin" ]; then
    echo "run-signin-issuer.sh: DISPATCH_E2E_ISSUER_BIN=$DISPATCH_E2E_ISSUER_BIN is not an executable file" >&2
    exit 2
  fi
fi

cd "$(dirname "$0")/../../envoy"
# Built and exec'd as run-server.sh builds and execs the server, so the pid a caller started is the
# issuer's: one binary per checkout, under a lock of its own, rewritten only when the source changed.
if [ -z "$issuer_bin" ]; then
  flock ./.dev-oidc-issuer-e2e.lock go build -o ./dev-oidc-issuer-e2e ./cmd/dev-oidc-issuer
  issuer_bin=./dev-oidc-issuer-e2e
fi
exec "$issuer_bin" \
  --code-flow "$client_id="<(printf '%s' "$secret")"=$callback" \
  --sign-in-as "$person" \
  --sign-in-group "$group"
