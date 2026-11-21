# shellcheck shell=bash disable=SC2154,SC2034 # root, work and evidence are the caller's (lib/rig.sh); the array is the caller's to read
# Dispatch's own identity, and Dispatch's own Envoy listener, for a stage proof that runs Dispatch,
# as a deployment gives Dispatch both: a send for a `role:` route carries its role mark, which the
# listener takes from Dispatch's verified identity alone, and a Dispatch with no identity refuses
# every such send. It runs packages/envoy/cmd/dev-oidc-issuer, the local stand-in for a cluster's
# ServiceAccount token issuer, which writes Dispatch's token into a file and rewrites it before it
# lapses, as a kubelet rewrites a projected token, and a listener of the proof's own build that
# verifies that token (ENVOY_OIDC_ISSUER, ENVOY_OIDC_AUDIENCE, ENVOY_OIDC_ACCOUNTS naming Dispatch's
# subject alone) and takes role marks from it (ENVOY_DISPATCH_PRINCIPAL).
#
# The listener is Dispatch's alone because a verifier closes /v1 to a caller that presents no
# credential, on loopback as anywhere (apiAuth in packages/envoy/cmd/listener). The proof's
# sessions, its daemon and its own reads call the sessions' listener, which the proof starts on
# loopback with no credential, so it serves them with none; Dispatch calls this one. Both are on
# the proof's one NATS, so each reads the registry, the roles and the sessions the other writes,
# as a deployment's Dispatch listener and session listener do.
#
# Sourced after lib/rig.sh, never run.
#   dispatch_identity_start LISTENER NATS_URL MACHINE_ID [NAME=VALUE]...
# builds the issuer into $work/bin, starts it as the service dispatch_identity_issuer, and starts
# LISTENER (the proof's envoy-listener binary) as the service dispatch_identity_listener on a port
# the kernel chose (start_held), with `env -i` and only what it names: loopback, NATS_URL, the
# machine id MACHINE_ID, the verifier, and each NAME=VALUE given (a proof's session windows, so
# Dispatch reads the states the sessions' listener reads). It waits until the listener admits
# Dispatch's token, fails unless the listener answers a request with no credential 401, and sets
#   dispatch_identity_dispatch_env  ENVOY_URL (this listener) and ENVOY_ACCOUNT_TOKEN_FILE, which
#                                   the caller adds to Dispatch's command line, after any
#                                   ENVOY_URL of its own
# Dispatch is never given ENVOY_TOKEN: it refuses to start with one. The issuer's key lives in its
# memory alone, so the listener verifies Dispatch's tokens only while it runs.
# dispatch_identity_stop, which each proof's cleanup calls, stops the listener and then the issuer
# (stop_pid), and does nothing for one never started.

dispatch_identity_audience=envoy-listener-e2e
dispatch_identity_principal=system:serviceaccount:e2e:dispatch
# Empty until dispatch_identity_start fills it, so a proof that runs no Dispatch adds nothing.
dispatch_identity_dispatch_env=()
# The pids once started, which dispatch_identity_stop stops; the listener's port and URL.
dispatch_identity_issuer_pid=
dispatch_identity_listener_pid=
dispatch_identity_listener_port=
dispatch_identity_listener_url=
dispatch_identity_token_file=

dispatch_identity_start() {
  local listener=$1 nats_url=$2 machine_id=$3 log="$evidence/logs/dispatch_identity_issuer.log" url status
  shift 3
  dispatch_identity_token_file="$work/dispatch-listener-token"
  mkdir -p "$work/bin"
  go -C "$root/packages/envoy" build -o "$work/bin/dev-oidc-issuer" ./cmd/dev-oidc-issuer ||
    fail "./cmd/dev-oidc-issuer did not build"
  start_process dispatch_identity_issuer "$work/bin/dev-oidc-issuer" \
    --token "$dispatch_identity_audience=$dispatch_identity_principal=$dispatch_identity_token_file"
  await_start dispatch_identity_issuer "$dispatch_identity_issuer_pid" 60 "dev-oidc-issuer to report its URL" \
    grep -q 'issuer listening url=' "$log"
  url=$(sed -n 's/.*issuer listening url=\([^ ]*\).*/\1/p' "$log" | head -n1)
  [ -n "$url" ] || fail "dev-oidc-issuer reported no URL in $log"
  [ -s "$dispatch_identity_token_file" ] || fail "dev-oidc-issuer wrote no token into $dispatch_identity_token_file"

  start_held dispatch_identity_listener dispatch_identity_listener_port ENVOY_LISTEN_FD -- \
    env -i PATH="$PATH" HOME=/nonexistent LEGION_E2E_RUN="$work" ENVOY_LISTEN_FD=3 PORT='{port}' \
    ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID="$machine_id" NATS_URLS="$nats_url" \
    ENVOY_OIDC_ISSUER="$url" ENVOY_OIDC_AUDIENCE="$dispatch_identity_audience" \
    ENVOY_OIDC_ACCOUNTS="$dispatch_identity_principal" ENVOY_DISPATCH_PRINCIPAL="$dispatch_identity_principal" \
    "$@" "$listener"
  dispatch_identity_listener_url="http://127.0.0.1:$dispatch_identity_listener_port"
  await_start dispatch_identity_listener "$dispatch_identity_listener_pid" 60 \
    "Dispatch's listener to admit Dispatch's token on /v1/sessions" dispatch_identity_get /v1/sessions
  status=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' "$dispatch_identity_listener_url/v1/sessions") ||
    fail "Dispatch's listener at $dispatch_identity_listener_url did not answer a request with no credential"
  [ "$status" = 401 ] ||
    fail "Dispatch's listener answered a request with no credential HTTP $status, not 401: its OIDC pair must close /v1 to every caller but Dispatch"
  dispatch_identity_dispatch_env=(
    ENVOY_URL="$dispatch_identity_listener_url"
    ENVOY_ACCOUNT_TOKEN_FILE="$dispatch_identity_token_file"
  )
  note "Dispatch's own listener: $dispatch_identity_listener_url, which admits $dispatch_identity_principal alone, minted by dev-oidc-issuer at $url"
}

# dispatch_identity_get PATH prints what Dispatch's listener answers GET PATH with Dispatch's own
# token, read from its file at each call, since the issuer rewrites it; it fails on any answer
# but 2xx. The token reaches curl on stdin, never on its command line.
dispatch_identity_get() {
  printf 'Authorization: Bearer %s\n' "$(cat "$dispatch_identity_token_file")" |
    curl -fsS --max-time 5 -H @- "$dispatch_identity_listener_url$1"
}

dispatch_identity_stop() {
  stop_pid "$dispatch_identity_listener_pid"
  stop_pid "$dispatch_identity_issuer_pid"
}
