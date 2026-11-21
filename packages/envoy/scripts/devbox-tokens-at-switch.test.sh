#!/usr/bin/env bash
# packages/envoy/scripts/devbox-tokens-at-switch.test.sh
# Runs devbox-tokens-at-switch.sh against a stand-in for the broker's count route and a stand-in for
# the box's trade, and checks each verdict and its exit code:
#   CLOSED   refused invalid_target naming the project, 0 live         exit 0
#   CLOSING  refused invalid_target naming the project, 2 live         exit 1
#   OPEN     answered, the count rising from 1 to 2                     exit 3
#   UNKNOWN  answered, the count reading 0 both times (a blind count)   exit 2
#   UNKNOWN  refused for another reason                                 exit 2
#   UNKNOWN  no account token, so no count                              exit 2
#   UNKNOWN  the count route refusing the account                       exit 2
# The blind row is the script's own control: an instrument that does not count the token the box
# just got must say UNKNOWN, never CLOSED or OPEN. It prints PASS or FAIL per row and exits 0 only
# when every row passed.
set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/devbox-tokens-at-switch.sh"
for tool in python3 curl jq; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "devbox-tokens-at-switch.test: UNKNOWN: $tool is not installed"
    exit 2
  }
done
WORK="$(mktemp -d "${TMPDIR:-/tmp}/devbox-tokens-at-switch-test.XXXXXX")" || exit 2
STAND_PID=""
# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  [ -z "$STAND_PID" ] || kill "$STAND_PID" 2>/dev/null
  case $WORK in */devbox-tokens-at-switch-test.?*) rm -rf -- "$WORK" ;; esac
}
trap cleanup EXIT

# The stand-in answers GET /v1/admin/machine-tokens?project=KEY to the bearer "dispatch-account"
# alone, with the counts the file $WORK/counts lists one per line, each read once in turn, the last
# kept for every later read.
cat >"$WORK/stand.py" <<'PY'
import json, sys, http.server, urllib.parse
work = sys.argv[1]
class Stand(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def do_GET(self):
        url = urllib.parse.urlparse(self.path)
        if url.path != "/v1/admin/machine-tokens":
            self.send_response(404); self.end_headers(); return
        if self.headers.get("Authorization") != "Bearer dispatch-account":
            self.send_response(403); self.end_headers()
            self.wfile.write(b'{"code":"ACCOUNT_LACKS_POWER","error":"not dispatch"}'); return
        with open(work + "/counts") as f:
            counts = [line.strip() for line in f if line.strip()]
        live = int(counts[0])
        if len(counts) > 1:
            with open(work + "/counts", "w") as f:
                f.write("\n".join(counts[1:]) + "\n")
        project = urllib.parse.parse_qs(url.query).get("project", [""])[0]
        body = {"project": project, "live_tokens": live, "last_expires_at": "2026-11-21T01:00:00Z" if live else None}
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers()
        self.wfile.write(json.dumps(body).encode())
server = http.server.HTTPServer(("127.0.0.1", 0), Stand)
print(server.server_address[1], flush=True)
server.serve_forever()
PY
python3 "$WORK/stand.py" "$WORK" >"$WORK/port" 2>"$WORK/stand.err" &
STAND_PID=$!
for _ in $(seq 100); do
  [ -s "$WORK/port" ] && break
  sleep 0.05
done
PORT="$(tr -dc '0-9' <"$WORK/port")"
[ -n "$PORT" ] || {
  echo "devbox-tokens-at-switch.test: UNKNOWN: the stand-in wrote no port: $(cat "$WORK/stand.err")"
  exit 2
}
printf 'dispatch-account' >"$WORK/account"
printf 'not-dispatch' >"$WORK/stranger"

# The trades the rows make, as print-trade.ts answers: exit 0 with the introspection, or exit 3
# with the broker's refusal on standard error. Each is a command the script runs with bash -c, so
# $AGENT_SECRETS_PROJECTS expands there, in the trade's own environment, never here.
# shellcheck disable=SC2016
REFUSED='echo "the broker refused this machine'"'"'s identity: invalid_target: The requested resource is invalid, missing, unknown, or malformed. The token names the project $AGENT_SECRETS_PROJECTS, which is private or turning private, and a developer box gets no token of such a project: work on it in a session of the project'"'"'s own namespace." >&2; exit 3'
# shellcheck disable=SC2016
ANSWERED='printf "{\"active\":true,\"person\":\"ada@example.com\",\"service\":\"\",\"project_scope\":\"person\",\"projects\":[\"%s\"]}\n" "$AGENT_SECRETS_PROJECTS"'
# shellcheck disable=SC2016
OTHER='echo "the broker refused this machine'"'"'s identity: invalid_request: The token names a project its person cannot see: $AGENT_SECRETS_PROJECTS." >&2; exit 3'

passed=0 failed=0
row() { # <name> <counts> <trade> <account file> <want exit> <want verdict>
  tr ' ' '\n' <<<"$2" >"$WORK/counts"
  local out rc
  out="$(AGENT_SECRETS_URL="http://127.0.0.1:$PORT" AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$4" DEVBOX_TOKENS_TRADE="$3" \
    bash "$SCRIPT" PRIV 2>&1)"
  rc=$?
  if [ "$rc" = "$5" ] && grep -q "^devbox-tokens-at-switch: $6" <<<"$out"; then
    echo "PASS $1"
    passed=$((passed + 1))
  else
    echo "FAIL $1: exit $rc, want $5 and a line starting devbox-tokens-at-switch: $6"
    printf '      %s\n' "${out//$'\n'/$'\n'      }"
    failed=$((failed + 1))
  fi
}

row "the after reading: refused, none live" "0 0" "$REFUSED" "$WORK/account" 0 "CLOSED: PRIV"
row "refused while tokens minted before are live" "2 2" "$REFUSED" "$WORK/account" 1 "CLOSING: PRIV"
row "the before reading: answered and counted" "1 2" "$ANSWERED" "$WORK/account" 3 "OPEN: PRIV"
row "the control: answered and never counted" "0 0" "$ANSWERED" "$WORK/account" 2 "UNKNOWN: a trade naming PRIV was answered"
row "refused for another reason" "0 0" "$OTHER" "$WORK/account" 2 "UNKNOWN: the trade naming PRIV was refused for another reason"
row "no account token" "0 0" "$REFUSED" "" 2 "UNKNOWN: AGENT_SECRETS_ACCOUNT_TOKEN_FILE names no file"
row "a count route that refuses the account" "0 0" "$REFUSED" "$WORK/stranger" 2 "UNKNOWN: the before count answered 403"

echo "devbox-tokens-at-switch.test: $((passed + failed)) rows ran, $passed passed"
[ "$failed" = 0 ] && [ "$passed" = 7 ]
