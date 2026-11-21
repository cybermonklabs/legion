#!/usr/bin/env bash
# packages/envoy/scripts/devbox-tokens-at-switch.sh <KEY>
# The switch reading: whether a developer box can still get, or still holds, a token that lists
# project KEY. Run it on a developer box before the owner switches KEY to private, and again after
# Dispatch completes the switch.
#
# It reads the secrets broker's count of the live developer-box tokens that list KEY
# (GET /v1/admin/machine-tokens?project=KEY), tries one trade from this box naming KEY
# (packages/cipher-client/scripts/print-trade.ts, with AGENT_SECRETS_PROJECTS=KEY), and reads the
# count again. It prints each reading with its UTC time, then one verdict line:
#   CLOSED   the trade was refused invalid_target and no token is live: the switch's after reading
#            passes. Exit 0.
#   CLOSING  the trade was refused, and tokens minted before are still live. Exit 1.
#   OPEN     the trade was answered and the broker counts it: the project is open to developer boxes.
#            Before the switch this is the expected reading, and it is the control that the count
#            sees this box's tokens. Exit 3.
#   UNKNOWN  a reading could not be made, or the trade was answered while the count read 0: an
#            instrument that does not count the token this box just got sees nothing. Exit 2.
#
# It needs, in its environment, what a box's trade needs (AGENT_SECRETS_URL, the machine identity
# or a person's sign-in on this machine, AGENT_SECRETS_SESSION), and AGENT_SECRETS_ACCOUNT_TOKEN_FILE:
# a token of the account the broker lists as the caller "dispatch" in BROKER_API_ACCOUNTS, the only
# caller the count and the trade's introspection answer. It needs curl, jq and bun with this
# repository's packages installed. DEVBOX_TOKENS_TRADE, a command, replaces print-trade.ts; only the
# script's own test sets it.
#
#   AGENT_SECRETS_ACCOUNT_TOKEN_FILE=… bash packages/envoy/scripts/devbox-tokens-at-switch.sh PRIV
set -uo pipefail
export TZ=UTC

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
PRINT_TRADE="$REPO_ROOT/packages/cipher-client/scripts/print-trade.ts"

unknown() {
  printf 'devbox-tokens-at-switch: UNKNOWN: %s\n' "$*"
  exit 2
}

[ "$#" -eq 1 ] || {
  echo "usage: devbox-tokens-at-switch.sh <KEY>" >&2
  exit 2
}
KEY=$1
[[ "$KEY" =~ ^[A-Z][A-Z0-9]{1,15}$ ]] || unknown "'$KEY' is no project key"
for tool in curl jq; do
  command -v "$tool" >/dev/null 2>&1 || unknown "$tool is not installed"
done
BROKER_URL="${AGENT_SECRETS_URL%/}"
[ -n "$BROKER_URL" ] || unknown "AGENT_SECRETS_URL names no broker"
ACCOUNT_TOKEN_FILE="${AGENT_SECRETS_ACCOUNT_TOKEN_FILE:-}"
[ -n "$ACCOUNT_TOKEN_FILE" ] && [ -s "$ACCOUNT_TOKEN_FILE" ] ||
  unknown "AGENT_SECRETS_ACCOUNT_TOKEN_FILE names no file holding a token of the broker's caller \"dispatch\" (BROKER_API_ACCOUNTS), so the count cannot be read"
if [ -z "${DEVBOX_TOKENS_TRADE:-}" ]; then
  command -v bun >/dev/null 2>&1 || unknown "bun is not installed, so the trade cannot be made"
  [ -f "$PRINT_TRADE" ] || unknown "$PRINT_TRADE is missing"
fi

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/devbox-tokens-at-switch.XXXXXX")" || unknown "mktemp failed"
# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  case $WORK_DIR in */devbox-tokens-at-switch.?*) rm -rf -- "$WORK_DIR" ;; esac
}
trap cleanup EXIT

# count reads the broker's count of KEY's live developer-box tokens into LIVE, or ends the run
# UNKNOWN naming what the broker answered.
LIVE=""
count() { # <which reading>
  local status
  status=$(curl -sS -o "$WORK_DIR/count" -w '%{http_code}' --max-time 20 \
    -H @<(printf 'Authorization: Bearer %s\n' "$(tr -d '\n' <"$ACCOUNT_TOKEN_FILE")") \
    "$BROKER_URL/v1/admin/machine-tokens?project=$KEY" 2>"$WORK_DIR/count.err") || status="curl failed: $(tr '\n' ' ' <"$WORK_DIR/count.err")"
  [ "$status" = 200 ] || unknown "the $1 count answered $status: $(head -c 300 "$WORK_DIR/count" 2>/dev/null)"
  [ "$(jq -r '.project' "$WORK_DIR/count" 2>/dev/null)" = "$KEY" ] || unknown "the $1 count answered no count of $KEY: $(head -c 300 "$WORK_DIR/count")"
  LIVE=$(jq -r '.live_tokens | select(type == "number")' "$WORK_DIR/count")
  [ -n "$LIVE" ] || unknown "the $1 count carries no live_tokens: $(head -c 300 "$WORK_DIR/count")"
  printf '%s  %s count: %s live developer-box tokens list %s, the last expiring at %s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$LIVE" "$KEY" "$(jq -r '.last_expires_at // "none"' "$WORK_DIR/count")"
}

count before
BEFORE=$LIVE

# The trade: print-trade.ts exits 0 with the introspection of the token it got, and 3 with the
# broker's refusal on standard error.
trade_rc=0
if [ -n "${DEVBOX_TOKENS_TRADE:-}" ]; then
  AGENT_SECRETS_PROJECTS=$KEY bash -c "$DEVBOX_TOKENS_TRADE" >"$WORK_DIR/trade" 2>"$WORK_DIR/trade.err" || trade_rc=$?
else
  AGENT_SECRETS_PROJECTS=$KEY bun "$PRINT_TRADE" >"$WORK_DIR/trade" 2>"$WORK_DIR/trade.err" || trade_rc=$?
fi
said="$(tr '\n' ' ' <"$WORK_DIR/trade.err" | head -c 400)"
case $trade_rc in
  0)
    projects="$(jq -c '.projects' "$WORK_DIR/trade" 2>/dev/null)"
    printf '%s  trade naming %s: answered, %s token naming %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$KEY" \
      "$(jq -r '.project_scope' "$WORK_DIR/trade" 2>/dev/null)" "$projects"
    trade=answered
    ;;
  3)
    printf '%s  trade naming %s: refused: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$KEY" "$said"
    if grep -q 'invalid_target' "$WORK_DIR/trade.err" && grep -qF "the project $KEY," "$WORK_DIR/trade.err"; then
      trade=closed
    else
      unknown "the trade naming $KEY was refused for another reason than the project's closing: $said"
    fi
    ;;
  *) unknown "the trade naming $KEY could not be made (exit $trade_rc): $said" ;;
esac

count after
AFTER=$LIVE

case $trade in
  closed)
    if [ "$AFTER" = 0 ]; then
      echo "devbox-tokens-at-switch: CLOSED: $KEY: no developer-box token is live, and a trade naming it is refused"
      exit 0
    fi
    echo "devbox-tokens-at-switch: CLOSING: $KEY: a trade naming it is refused, and $AFTER developer-box tokens minted before are still live"
    exit 1
    ;;
  answered)
    if [ "$AFTER" -gt 0 ] && [ "$AFTER" -ge "$BEFORE" ]; then
      echo "devbox-tokens-at-switch: OPEN: $KEY: a trade naming it is answered, and the broker counts $AFTER live developer-box tokens that list it"
      exit 3
    fi
    unknown "a trade naming $KEY was answered, and the count read $BEFORE before it and $AFTER after: the count does not see the token this box just got"
    ;;
esac
