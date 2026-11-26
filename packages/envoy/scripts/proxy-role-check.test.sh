#!/usr/bin/env bash
# packages/envoy/scripts/proxy-role-check.test.sh
#
# Tests the parts of proxy-role-check.sh that decide its verdict, with no stack, broker or network:
#
#   - its usage: --help exits 0 and prints the usage, and an unknown argument exits 2;
#   - its placeholder assertion (proxy-role/assert.sh): a bundle of placeholders passes; a bundle
#     whose hosts.yml holds an App token fails, which is the driver's negative control; a bundle
#     with a token in another file fails; and a root holding no hosts.yml is UNKNOWN and fails;
#   - its stand-in GitHub (proxy-role/github_standin.py), on a loopback port of its own: an
#     installation token is answered and recorded by kind and account, a placeholder and a call
#     with no credential are refused 401 and recorded as such, git's advertisement carries the
#     ref, and no record holds a credential.
#
# Needs bash, python3, openssl, curl and jq. Run from anywhere:
#   bash packages/envoy/scripts/proxy-role-check.test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source-path=SCRIPTDIR source=proxy-role/assert.sh
. "$SCRIPT_DIR/proxy-role/assert.sh"

work="$(mktemp -d "${TMPDIR:-/tmp}/proxy-role-check-test.XXXXXX")"
standin_pid=""
cleanup() {
	if [ -n "$standin_pid" ]; then
		kill "$standin_pid" 2>/dev/null || true
		wait "$standin_pid" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT

failures=0
check() {
	if [ "$2" = 0 ]; then
		echo "PASS: $1"
	else
		echo "FAIL: $1"
		failures=$((failures + 1))
	fi
}

echo "case: usage"
status=0
help="$(bash "$SCRIPT_DIR/proxy-role-check.sh" --help 2>&1)" || status=$?
check "--help exits 0 and prints the usage" "$([ "$status" = 0 ] && grep -q '^Usage: packages/envoy/scripts/proxy-role-check.sh' <<<"$help" && echo 0 || echo 1)"
status=0
bash "$SCRIPT_DIR/proxy-role-check.sh" --bogus >/dev/null 2>"$work/usage.err" || status=$?
check "an unknown argument exits 2 naming it" "$([ "$status" = 2 ] && grep -q 'unexpected argument --bogus' "$work/usage.err" && echo 0 || echo 1)"

echo "case: the placeholder assertion"
hosts() { # hosts <dir> <token>: a gh hosts.yml in dir holding token
	mkdir -p "$1"
	printf 'github.com:\n    oauth_token: %s\n    user: x-access-token\n    users:\n        x-access-token:\n            oauth_token: %s\n' "$2" "$2" >"$1/hosts.yml"
}
hosts "$work/good/acme-alpha" agent-secrets-placeholder-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
hosts "$work/good/acme-beta" agent-secrets-placeholder-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
said="$(assert_placeholders_only "$work/good")" && status=0 || status=1
check "a bundle of placeholders passes ($said)" "$([ "$status" = 0 ] && grep -q '^read 2 hosts.yml: 4 placeholder token lines, 0 other' <<<"$said" && echo 0 || echo 1)"
hosts "$work/token/acme-alpha" ghs_standin_acme-alpha
said="$(assert_placeholders_only "$work/token")" && status=0 || status=1
check "negative control: a hosts.yml holding an App token fails ($said)" "$([ "$status" = 1 ] && grep -q '2 other token lines, 1 files holding a GitHub token' <<<"$said" && echo 0 || echo 1)"
hosts "$work/side/acme-alpha" agent-secrets-placeholder-cccccccccccccccccccccccccccccccc
printf 'token=ghs_standin_acme-alpha\n' >"$work/side/acme-alpha/notes"
said="$(assert_placeholders_only "$work/side")" && status=0 || status=1
check "a token in another file of the bundle fails ($said)" "$([ "$status" = 1 ] && echo 0 || echo 1)"
mkdir -p "$work/empty"
said="$(assert_placeholders_only "$work/empty")" && status=0 || status=1
check "a root holding no hosts.yml is UNKNOWN and fails ($said)" "$([ "$status" = 1 ] && grep -q '^UNKNOWN' <<<"$said" && echo 0 || echo 1)"

echo "case: the stand-in GitHub"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=api.github.com \
	-addext 'subjectAltName=DNS:api.github.com,DNS:github.com' \
	-keyout "$work/standin.key" -out "$work/standin.pem" 2>/dev/null
port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
records="$work/records.jsonl"
: >"$records"
python3 "$SCRIPT_DIR/proxy-role/github_standin.py" --address "127.0.0.1:$port" \
	--cert "$work/standin.pem" --key "$work/standin.key" --records "$records" 2>"$work/standin.log" &
standin_pid=$!
for _ in $(seq 1 50); do
	grep -q 'github-standin listening' "$work/standin.log" 2>/dev/null && break
	sleep 0.1
done
call() { # call <host> <path> [curl args...]: the status of one call, its body in $work/body
	local host="$1" path="$2"
	shift 2
	curl -sS -o "$work/body" -w '%{http_code}' --cacert "$work/standin.pem" \
		--resolve "$host:$port:127.0.0.1" "$@" "https://$host:$port$path"
}
# The stand-in's fake credentials, built from parts so that no line holds a credential's shape.
alpha_token="ghs_standin_""acme-alpha"
beta_token="ghs_standin_""acme-beta"
placeholder="agent-secrets-placeholder-""dddddddddddddddddddddddddddddddd"
# call_as <scheme> <token> <host> <path> [curl args...]: call with an Authorization header of
# scheme and token, which curl reads on standard input, so no process's arguments hold it.
call_as() {
	local scheme="$1" token="$2"
	shift 2
	if [ "$scheme" = Basic ]; then
		token="$(printf '%s' "x-access-token:$token" | base64 -w0)"
	fi
	printf 'Authorization: %s %s\n' "$scheme" "$token" | call "$@" -H @-
}
status="$(call_as token "$alpha_token" api.github.com /repos/acme-alpha/demo)"
check "an installation token reads the repository" "$([ "$status" = 200 ] && [ "$(jq -r .full_name "$work/body")" = acme-alpha/demo ] && echo 0 || echo 1)"
check "and is recorded as acme-alpha's App token, by digest" "$(jq -se --arg sha "$(printf '%s' "$alpha_token" | sha256sum | cut -c1-16)" \
	'.[-1] | .kind == "app" and .account == "acme-alpha" and .token_sha16 == $sha' "$records" >/dev/null && echo 0 || echo 1)"
status="$(call_as token "$placeholder" api.github.com /repos/acme-alpha/demo)"
check "a placeholder is refused 401 and recorded as a placeholder" "$([ "$status" = 401 ] && jq -se '.[-1].kind == "placeholder"' "$records" >/dev/null && echo 0 || echo 1)"
status="$(curl -sS -o /dev/null -D "$work/headers" -w '%{http_code}' --cacert "$work/standin.pem" --resolve "github.com:$port:127.0.0.1" \
	"https://github.com:$port/acme-alpha/demo.git/info/refs?service=git-upload-pack")"
check "git with no credential gets 401 and a Basic challenge" "$([ "$status" = 401 ] && grep -qi '^www-authenticate: basic' "$work/headers" && echo 0 || echo 1)"
status="$(call_as Basic "$beta_token" github.com '/acme-beta/demo.git/info/refs?service=git-receive-pack')"
check "git with acme-beta's token reads the receive-pack advertisement" "$([ "$status" = 200 ] && grep -q 'refs/heads/main' "$work/body" && jq -se '.[-1] | .kind == "app" and .account == "acme-beta"' "$records" >/dev/null && echo 0 || echo 1)"
status="$(call_as bearer "$alpha_token" api.github.com /graphql -X POST -d '{"query":"query { viewer { login } }"}')"
check "GraphQL answers the viewer" "$([ "$status" = 200 ] && [ "$(jq -r .data.viewer.login "$work/body")" = 'proxy-role-check[bot]' ] && echo 0 || echo 1)"
check "no record holds a credential" "$(! grep -q 'ghs_standin_\|agent-secrets-placeholder-' "$records" && [ "$(wc -l <"$records")" -ge 5 ] && echo 0 || echo 1)"

echo
if [ "$failures" -eq 0 ]; then
	echo "proxy-role-check.test.sh: passed"
	exit 0
fi
echo "proxy-role-check.test.sh: $failures failed"
exit 1
