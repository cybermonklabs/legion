# shellcheck shell=bash disable=SC2154,SC2034 # root, work and evidence are the caller's (lib/rig.sh); the arrays are the caller's to read
# go-offline-file: the four proofs that source it run on a devbox alone; e2e.yaml refuses each.
# Dispatch's own identity at the Envoy listener, for a stage proof that runs both, as a deployment
# gives Dispatch one: a send for a `role:` route carries its role mark, which the listener takes from
# Dispatch's verified identity alone, and a Dispatch with no identity refuses every such send. It runs
# packages/envoy/cmd/dev-oidc-issuer, the local stand-in for a cluster's ServiceAccount token issuer,
# which writes Dispatch's token into a file and rewrites it before it lapses, as a kubelet rewrites
# a projected token.
#
# Sourced after lib/rig.sh, never run. dispatch_identity_start builds the issuer into $work/bin,
# starts it as the service dispatch_identity_issuer, waits for its URL, and sets two arrays the
# caller adds to its `env -i` command lines:
#   dispatch_identity_listener_env  ENVOY_OIDC_ISSUER, ENVOY_OIDC_AUDIENCE and ENVOY_DISPATCH_PRINCIPAL,
#                                   for each listener Dispatch calls
#   dispatch_identity_dispatch_env  ENVOY_ACCOUNT_TOKEN_FILE, for Dispatch
# The listeners these proofs run take the shared token as well (ENVOY_API_TOKEN), so a verifier
# beside it changes nothing for their sessions. The issuer's key lives in its memory alone, so a
# listener verifies its tokens only while it runs; the rig stops it with every process of $work.

dispatch_identity_audience=envoy-listener-e2e
dispatch_identity_principal=system:serviceaccount:e2e:dispatch
# Empty until dispatch_identity_start fills them, so a proof that runs no Dispatch adds nothing.
dispatch_identity_listener_env=()
dispatch_identity_dispatch_env=()
# The issuer's pid once started; each proof's cleanup stops it beside its listener (stop_pid).
dispatch_identity_issuer_pid=

dispatch_identity_start() {
  local token_file="$work/dispatch-listener-token" log="$evidence/logs/dispatch_identity_issuer.log" url
  mkdir -p "$work/bin"
  go -C "$root/packages/envoy" build -o "$work/bin/dev-oidc-issuer" ./cmd/dev-oidc-issuer ||
    fail "./cmd/dev-oidc-issuer did not build"
  start_process dispatch_identity_issuer "$work/bin/dev-oidc-issuer" \
    --token "$dispatch_identity_audience=$dispatch_identity_principal=$token_file"
  await_start dispatch_identity_issuer "$dispatch_identity_issuer_pid" 60 "dev-oidc-issuer to report its URL" \
    grep -q 'issuer listening url=' "$log"
  url=$(sed -n 's/.*issuer listening url=\([^ ]*\).*/\1/p' "$log" | head -n1)
  [ -n "$url" ] || fail "dev-oidc-issuer reported no URL in $log"
  [ -s "$token_file" ] || fail "dev-oidc-issuer wrote no token into $token_file"
  dispatch_identity_listener_env=(
    ENVOY_OIDC_ISSUER="$url"
    ENVOY_OIDC_AUDIENCE="$dispatch_identity_audience"
    ENVOY_DISPATCH_PRINCIPAL="$dispatch_identity_principal"
  )
  dispatch_identity_dispatch_env=(ENVOY_ACCOUNT_TOKEN_FILE="$token_file")
  note "Dispatch's identity at the listener: $dispatch_identity_principal, minted by dev-oidc-issuer at $url"
}
