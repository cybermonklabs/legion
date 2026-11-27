# shellcheck shell=bash
# packages/envoy/scripts/lib/approve-stack.sh
#
# What dev-stack.sh --approve adds to a run: a person's approvals through Cipher's proxy. The
# broker gets its proxy port, a certificate authority this file makes, and one dpop block,
# DISPATCH_APPROVE_TOKEN, bound to APPROVE_HOST and sent through BROKER_PROXY_PRIVATE_UPSTREAM to
# this run's Dispatch on loopback, which BROKER_AIRLOCK_TEST_DIAL_ALLOW lets the proxy dial; and
# Dispatch serves under https://APPROVE_HOST (DISPATCH_SERVER_URL), the origin the proxy signs
# each request's DPoP proof for. Nothing binds port 443: the proxy terminates the session's TLS
# itself and sends the request on to Dispatch's own address in plain HTTP, as it does in a
# cluster. A direct call to Dispatch on this stack signs its proof for that origin too.
#
# The allow list is the approve routes as path prefixes: the reviews and the keep of a document's
# block, the decisions on a credential request, the revocations, the revoke of a personal token and
# the answers of asks, and never an issue's comments or the mint or change of a personal token. A
# prefix takes every route under it, so a document's comments and an ask's decline reach Dispatch
# too, whose approve-only check refuses them (and every ask's answer but an approval request's).
# With --approve-allow-all the list is every method on /api/, the stack a check turns the allow
# list off in, so a request reaches Dispatch's own approve-only check.
#
# It reads dev-stack.sh's WORK_DIR, LOG_DIR, DISPATCH_PORT, BROKER_PID, BROKER_LOG, APPROVE and
# APPROVE_ALLOW_ALL, await with the awaited it sets, and project-access-stack.sh's airlock_addr_in,
# which dev-stack.sh sources first. It sets, for the caller:
#   APPROVE_ENV          the broker's settings for the approve capability, empty without --approve
#   APPROVE_PROXY_URL    http://127.0.0.1:<the proxy port this run picked>
#   APPROVE_CA_FILE      the proxy's certificate authority, which a session trusts it by
#   DISPATCH_SERVER_URL_SETTING
#                        what Dispatch serves under: https://APPROVE_HOST with --approve, else its
#                        own loopback URL, as before

APPROVE_HOST="dispatch.approve.test"
APPROVE_PROFILES_FILE="$WORK_DIR/approve-profiles.json"
APPROVE_CA_FILE="$WORK_DIR/approve-ca.crt"
APPROVE_CA_KEY_FILE="$WORK_DIR/approve-ca.key"
APPROVE_ENV=()
APPROVE_PROXY_URL=""
APPROVE_PROXY_PORT=""

# approve_allow_list prints the dpop block's allow list as JSON: the approve routes, or every
# method on /api/ with --approve-allow-all.
approve_allow_list() {
  if [ "$APPROVE_ALLOW_ALL" = true ]; then
    printf '%s' '[{"method": "GET", "pathPrefix": "/api/"}, {"method": "POST", "pathPrefix": "/api/"},
      {"method": "PATCH", "pathPrefix": "/api/"}, {"method": "PUT", "pathPrefix": "/api/"},
      {"method": "DELETE", "pathPrefix": "/api/"}]'
    return
  fi
  printf '%s' '[{"method": "POST", "pathPrefix": "/api/v1/artifacts/"},
    {"method": "POST", "pathPrefix": "/api/v1/credential-requests/"},
    {"method": "POST", "pathPrefix": "/api/v1/credential-grants/"},
    {"method": "POST", "pathPrefix": "/api/v1/machine-logins/"},
    {"method": "POST", "pathPrefix": "/api/v1/signins/"},
    {"method": "DELETE", "pathPrefix": "/api/v1/me/agent-tokens/"},
    {"method": "POST", "pathPrefix": "/api/v1/asks/"}]'
}

# approve_dispatch_setup sets DISPATCH_SERVER_URL_SETTING and DISPATCH_SIGNIN_SETTINGS;
# dev-stack.sh calls it once Dispatch's port is held. Dispatch's dev sign-in fence takes a loopback
# origin alone, so under --approve, whose origin is https://APPROVE_HOST, Dispatch runs without its
# dev sign-in route: a person reaches it with the token their sign-in traded for, and the run's
# agent token reads it.
# shellcheck disable=SC2034 # both are the caller's: dev-stack.sh starts Dispatch with them
approve_dispatch_setup() {
  DISPATCH_SERVER_URL_SETTING="$DISPATCH_URL"
  DISPATCH_SIGNIN_SETTINGS=(DISPATCH_DEV_SIGNIN=1 DISPATCH_INSECURE_COOKIE=1)
  if [ "$APPROVE" = true ]; then
    DISPATCH_SERVER_URL_SETTING="https://$APPROVE_HOST"
    DISPATCH_SIGNIN_SETTINGS=()
  fi
}

# approve_broker_setup, with --approve, writes the authority and the profiles file, picks the
# proxy's port and fills APPROVE_ENV; without it, it leaves everything empty.
# shellcheck disable=SC2034 # APPROVE_ENV is the caller's: dev-stack.sh starts the broker with it
approve_broker_setup() {
  [ "$APPROVE" = true ] || return 0
  # A P-256 authority of this run's own, the key readable by this user alone. The proxy signs
  # each bound host's certificate with it, and the session trusts the proxy by its certificate.
  (umask 077 && openssl ecparam -name prime256v1 -genkey -noout -out "$APPROVE_CA_KEY_FILE" 2>/dev/null)
  openssl req -x509 -new -key "$APPROVE_CA_KEY_FILE" -days 2 -subj "/CN=dev-stack approve proxy" \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign" \
    -out "$APPROVE_CA_FILE" 2>/dev/null
  printf '{"profiles": [{"name": "DISPATCH_APPROVE_TOKEN", "proxy": {"hosts": ["%s"], "headers": ["Authorization"], "kind": "dpop",\n  "allow": %s}}]}\n' \
    "$APPROVE_HOST" "$(approve_allow_list)" >"$APPROVE_PROFILES_FILE"
  chmod 600 "$APPROVE_PROFILES_FILE"
  # The proxy's port, which the broker is told at boot and binds itself; approve_proxy_check
  # reads the broker's own line back and stops a run whose proxy took another.
  APPROVE_PROXY_PORT="$(python3 -c "
import socket
s = socket.socket()
s.bind(('127.0.0.1', 0))
print(s.getsockname()[1])
s.close()")"
  APPROVE_PROXY_URL="http://127.0.0.1:${APPROVE_PROXY_PORT}"
  APPROVE_ENV=(
    "BROKER_PROFILES_FILE=$APPROVE_PROFILES_FILE"
    "BROKER_AIRLOCK_LISTEN_ADDR=127.0.0.1:${APPROVE_PROXY_PORT}"
    "BROKER_AIRLOCK_PROXY_URL=$APPROVE_PROXY_URL"
    "BROKER_AIRLOCK_CA_CERT_FILE=$APPROVE_CA_FILE"
    "BROKER_AIRLOCK_CA_KEY_FILE=$APPROVE_CA_KEY_FILE"
    "BROKER_PROXY_PRIVATE_UPSTREAM=$APPROVE_HOST=http://127.0.0.1:${DISPATCH_PORT}"
    "BROKER_AIRLOCK_TEST_DIAL_ALLOW=127.0.0.1:${DISPATCH_PORT}"
  )
}

# approve_proxy_check, with --approve, waits for the broker's proxy and its approve capability
# lines and stops the run when the proxy is not on the port this run picked or the capability is
# off.
# shellcheck disable=SC2154 # awaited is set by dev-stack.sh's await
approve_proxy_check() {
  [ "$APPROVE" = true ] || return 0
  await "the broker's proxy" "$BROKER_PID" "$BROKER_LOG" "$BROKER_LOG" 'its proxy address' airlock_addr_in
  if [ "$awaited" != "127.0.0.1:${APPROVE_PROXY_PORT}" ]; then
    echo "dev-stack: the broker's proxy is on $awaited, not the 127.0.0.1:${APPROVE_PROXY_PORT} this run picked: another process took that port first. Start the run again." >&2
    return 1
  fi
  local state
  state="$(jq -Rnr '[inputs | fromjson? | select(.message == "broker: the approve capability") | .state][0] // empty' "$BROKER_LOG")"
  if [ "$state" != on ]; then
    echo "dev-stack: the broker serves no approve capability (state '${state}'); see $BROKER_LOG" >&2
    return 1
  fi
  echo "dev-stack: approve proxy $APPROVE_PROXY_URL, DISPATCH_APPROVE_TOKEN bound to https://$APPROVE_HOST" >&2
}

# approve_exports prints the banner's --approve lines, nothing without the option.
approve_exports() {
  [ "$APPROVE" = true ] || return 0
  printf '%s\n' \
    "  export DEV_STACK_APPROVE_HOST=$APPROVE_HOST" \
    "  export DEV_STACK_APPROVE_PROXY_URL=$APPROVE_PROXY_URL" \
    "  export DEV_STACK_APPROVE_CA_FILE=$APPROVE_CA_FILE" \
    "  export DEV_STACK_DISPATCH_SERVER_URL=$DISPATCH_SERVER_URL_SETTING"
}
