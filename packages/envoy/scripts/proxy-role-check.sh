#!/usr/bin/env bash
# packages/envoy/scripts/proxy-role-check.sh
#
# Runs a Legion role's GitHub access through its forwarder (`legion forwarder`, built from this
# tree) and the secrets broker's proxy on the broker's local stack, and says whether every step
# worked. It is the driver of a role's GitHub behind the proxy, and its acceptance check.
#
# What it starts:
#
#   - scripts/dev-airlock.sh --one-broker: Postgres, the local token issuer, the six built
#     binaries, the stack's broker and one enrolled pod session, whose key directory stands in for
#     the role's, renewed for as long as the stack runs.
#   - One stand-in GitHub that mints (dev-airlock-stands, plain HTTP), with the App installed on
#     two accounts, acme-alpha and acme-beta: the broker mints `ghs_standin_<account>` for each.
#   - One stand-in GitHub that answers (proxy-role/github_standin.py), over TLS for api.github.com,
#     github.com and uploads.github.com on 127.0.0.20:443, with a certificate the stack's authority
#     signs: REST, GraphQL and git's smart HTTP. It records the credential of every call by kind and
#     by the first 16 hex digits of its SHA-256, never by value.
#   - One broker of this check's own on the stack's database, authority and session, as
#     airlock-check.sh's k7 starts one: a profiles file that declares the kind rolegithub (GitHub's
#     three hosts, by the way mint) beside the stack's kinds, and holds PROXY_ROLE_APP, a github-app
#     secret owned by the stack's service, agent tier, on both accounts, whose proxy tag names that
#     kind, so it is behind the proxy; and PROXY_ROLE_PLAIN, the same App from a source with no
#     proxy tag, released as its tokens.
#   - `legion forwarder --role implementer`, asking that broker with the session's key for
#     PROXY_ROLE_APP and writing the role's gh bundle.
#
# What it checks, as the role's own tools run: `gh api repos/acme-alpha/<repo>`, `gh pr list -R
# acme-beta/<repo>`, `gh api graphql` with a viewer query, `gh stack --help`, `git ls-remote` and
# `git push --dry-run`, each through the forwarder, with the stand-in recording acme-alpha's
# installation token for an acme-alpha call and acme-beta's for an acme-beta call, from one grant
# that names no repository; a revocation of the grant, after which the role's calls fail for at
# most 60 seconds and then work; a fetch from a host the grant does not bind while the broker's
# proxy port answers nothing; and that the role's hosts.yml files and its git credential helper
# hold placeholders and no token. Each gh call runs with GH_CONFIG_DIR set to the directory of the
# account the call names, which is the directory `legion gh` routes such a call to in a pod.
#
# The negative control: the same placeholder check run over a hosts.yml holding the token the
# daemon reads without the role map (its token mode releases PROXY_ROLE_PLAIN with `agent-secrets
# <PROFILE> -- printenv <PROFILE>`) must fail.
#
# It prints a PASS or FAIL line for each property, then `proxy-role-check: passed (<n> of <n>)` or
# `proxy-role-check: failed (<k> of <n>)`, and a last line naming the scenario's outcome, the head
# commit and how long the run took. It exits 0 only when every property passed, 1 when one did not,
# and 2 on a usage error. No line it prints is a credential.
#
# Usage: packages/envoy/scripts/proxy-role-check.sh [--isolate] [--keep] [--help]
#   --isolate  run in a user and network namespace of this check's own, for a machine whose
#              loopback port 443 this user may not bind (the stands bind 443 on loopback, which a
#              Legion pod allows and a developer box often does not). Needs unshare, ip and socat,
#              and DEV_BROKER_POSTGRES_URL naming a Postgres on 127.0.0.1, whose port is bridged into
#              the namespace through a unix socket.
#   --keep     leave the work directory, for a person to read a failure in.
#
# Needs bash, curl, jq, go, python3, openssl, git, a real gh (PROXY_ROLE_GH names it; else the first
# gh on PATH that is no script) with the gh-stack extension, and what dev-airlock.sh needs.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck source-path=SCRIPTDIR source=lib/exec-env.sh
. "$SCRIPT_DIR/lib/exec-env.sh"
# shellcheck source-path=SCRIPTDIR source=proxy-role/assert.sh
. "$SCRIPT_DIR/proxy-role/assert.sh"
# shellcheck source-path=SCRIPTDIR source=lib/pg-url.sh
. "$SCRIPT_DIR/lib/pg-url.sh"

usage() {
	sed -n '/^# Usage:/,/^# gh on PATH/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

ISOLATE=false
KEEP=false
for arg in "$@"; do
	case "$arg" in
	--isolate) ISOLATE=true ;;
	--keep) KEEP=true ;;
	--help | -h)
		usage
		exit 0
		;;
	*)
		echo "proxy-role-check: unexpected argument $arg" >&2
		usage >&2
		exit 2
		;;
	esac
done

STARTED_AT="$(date -u +%s)"
HEAD_COMMIT="$(git -C "$REPO_ROOT" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"

# --- --isolate: run the rest in a namespace of its own, bridging the database's port in. ---
if [ "$ISOLATE" = true ] && [ -z "${PROXY_ROLE_IN_NAMESPACE:-}" ]; then
	for tool in unshare ip socat; do
		command -v "$tool" >/dev/null || {
			echo "proxy-role-check: --isolate needs $tool" >&2
			exit 2
		}
	done
	if ! [[ "${DEV_BROKER_POSTGRES_URL:-}" =~ ^postgres(ql)?://[^@/]*@?127\.0\.0\.1:([0-9]+)/ ]]; then
		echo "proxy-role-check: --isolate needs DEV_BROKER_POSTGRES_URL naming a Postgres on 127.0.0.1:<port>" >&2
		exit 2
	fi
	pg_port="${BASH_REMATCH[2]}"
	bridge="$(mktemp -d "${TMPDIR:-/tmp}/proxy-role-bridge.XXXXXX")"
	socat "UNIX-LISTEN:$bridge/pg.sock,fork,mode=600" "TCP:127.0.0.1:$pg_port" &
	bridge_pid=$!
	status=0
	# shellcheck disable=SC2016 # expanded by the namespace's own shell
	PROXY_ROLE_IN_NAMESPACE=1 PROXY_ROLE_BRIDGE="$bridge/pg.sock" PROXY_ROLE_PG_PORT="$pg_port" \
		unshare --user --map-root-user --net bash -c '
			ip link set lo up
			socat "TCP-LISTEN:$PROXY_ROLE_PG_PORT,bind=127.0.0.1,fork,reuseaddr" "UNIX-CONNECT:$PROXY_ROLE_BRIDGE" &
			bridge=$!
			trap "kill $bridge 2>/dev/null" EXIT
			bash "$0" "$@"' "${BASH_SOURCE[0]}" "$@" || status=$?
	kill "$bridge_pid" 2>/dev/null || true
	rm -rf "$bridge"
	exit "$status"
fi

# --- Preflight. ---
for tool in curl jq go python3 openssl git; do
	command -v "$tool" >/dev/null || {
		echo "proxy-role-check: needs $tool on PATH" >&2
		exit 2
	}
done
# real_gh is PROXY_ROLE_GH, or the first gh on PATH that is a binary rather than a script: a box's
# own gh wrapper routes by its own rules, and a pod's gh is the image's binary.
real_gh() {
	if [ -n "${PROXY_ROLE_GH:-}" ]; then
		printf '%s\n' "$PROXY_ROLE_GH"
		return
	fi
	local dir candidate
	IFS=: read -r -a dirs <<<"$PATH"
	for dir in "${dirs[@]}"; do
		candidate="$dir/gh"
		if [ -x "$candidate" ] && [ "$(head -c 2 "$candidate")" != '#!' ]; then
			printf '%s\n' "$candidate"
			return
		fi
	done
}
GH="$(real_gh)"
if [ -z "$GH" ] || ! [ -x "$GH" ]; then
	echo "proxy-role-check: no real gh found: set PROXY_ROLE_GH to the gh binary" >&2
	exit 2
fi

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/proxy-role-check.XXXXXX")"
STACK_LOG="$WORK_DIR/stack.log"
PIDS=()
STACK_PID=""
# BROKER_DB_URL is this check's broker's database once its broker is started, and DROP_TRIGGER
# true once the renewal step has made its back-dating trigger, which cleanup then drops: the stack
# drops its database at its own stop, but --keep leaves the work directory for a person, and the
# trigger is dropped whether or not the run passed, so no later grant of that database is
# back-dated. Cleanup then reads back how many such triggers the database holds and says so, so a
# drop that did not happen is seen.
BROKER_DB_URL=""
DROP_TRIGGER=false
cleanup() {
	local pid left
	if [ -n "$BROKER_DB_URL" ]; then
		if [ "$DROP_TRIGGER" = true ]; then
			pg_url_run "$BROKER_DB_URL" psql -v ON_ERROR_STOP=1 -q -c \
				'drop trigger if exists proxy_role_check_backdate on grants; drop function if exists proxy_role_check_backdate();' >/dev/null 2>&1 ||
				echo "proxy-role-check: could not drop the back-dating trigger proxy_role_check_backdate on grants; drop it by hand" >&2
		fi
		left="$(pg_url_run "$BROKER_DB_URL" psql -Atqc "select count(*) from pg_trigger where tgname = 'proxy_role_check_backdate'" 2>/dev/null)" || left="unknown (the database could not be read)"
		echo "proxy-role-check: back-dating triggers left in this check's database: $left" >&2
	fi
	for pid in "${PIDS[@]+"${PIDS[@]}"}"; do
		kill -CONT "$pid" 2>/dev/null || true
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
	if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
		kill "$STACK_PID" 2>/dev/null || true
		wait "$STACK_PID" 2>/dev/null || true
	fi
	if [ "$KEEP" = true ]; then
		echo "proxy-role-check: work directory kept at $WORK_DIR" >&2
	else
		rm -rf "$WORK_DIR"
	fi
}
trap cleanup EXIT

# --- What a run says. ---
PASSED=0
TOTAL=0
pass() {
	TOTAL=$((TOTAL + 1))
	PASSED=$((PASSED + 1))
	echo "PASS: $1"
}
fail() {
	TOTAL=$((TOTAL + 1))
	echo "FAIL: $1"
}
# judge <status> <property>: "0" passes; anything else, an empty answer included, fails.
judge() { if [ "${1:-}" = 0 ]; then pass "$2"; else fail "$2"; fi; }
digest() { printf '%s' "$1" | sha256sum | cut -c1-16; }

# await <what> <seconds> <log> <pattern> <pid>: waits for a line, and ends the run the moment the
# process that writes it dies.
await() {
	local i
	for ((i = 0; i < $2 * 2; i++)); do
		if grep -qE "$4" "$3" 2>/dev/null; then return 0; fi
		if ! kill -0 "$5" 2>/dev/null; then
			echo "proxy-role-check: $1 exited before it was ready; see $3" >&2
			tail -n 20 "$3" >&2 || true
			exit 1
		fi
		sleep 0.5
	done
	echo "proxy-role-check: $1 was not ready within $2 seconds; see $3" >&2
	exit 1
}

free_port() { python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'; }

# --- The stack. ---
echo "proxy-role-check: building legion from this tree..." >&2
mkdir -p "$WORK_DIR/bin"
(cd "$REPO_ROOT/packages/daemon" && go build -o "$WORK_DIR/bin/legion" ./cmd/legion)
LEGION="$WORK_DIR/bin/legion"

echo "proxy-role-check: starting the stack (dev-airlock.sh --one-broker)..." >&2
bash "$SCRIPT_DIR/dev-airlock.sh" --one-broker >"$STACK_LOG" 2>&1 &
STACK_PID=$!
await "the stack" 900 "$STACK_LOG" '^dev-airlock: ready\.$' "$STACK_PID"
eval "$(sed -n 's/^  \(export [A-Z][A-Z_0-9]*=.*\)$/\1/p' "$STACK_LOG")"
STACK_BROKER_PID="$(sed -n 's/^  broker [0-9]*:.*pid \([0-9]*\),.*/\1/p' "$STACK_LOG" | head -n 1)"
STANDS_BIN="$DEV_BROKER_DIR/bin/dev-airlock-stands"
BROKER_BIN="$DEV_BROKER_DIR/bin/broker"
AGENT_SECRETS_BIN="$DEV_BROKER_DIR/bin/agent-secrets"
CA="$AIRLOCK_CA_CERT_FILE"

# --- The minting stand-in GitHub: the App on two accounts. ---
ACCOUNT_A=acme-alpha
ACCOUNT_B=acme-beta
REPO=proxy-role-check
mkdir -p "$WORK_DIR/stands"
"$STANDS_BIN" --dir "$WORK_DIR/stands" --installation "$ACCOUNT_A=11" --installation "$ACCOUNT_B=12" \
	>"$WORK_DIR/stands.json" 2>"$WORK_DIR/stands.log" &
PIDS+=($!)
await "the minting stand-in" 60 "$WORK_DIR/stands.log" 'stands listening records=' "${PIDS[-1]}"
MINT_URL="$(jq -r '.stands.github.url' "$WORK_DIR/stands.json")"
APP_KEY_FILE="$(jq -r .github_app_key_file "$WORK_DIR/stands.json")"

# --- The answering stand-in GitHub, signed by the stack's authority. ---
STANDIN_ADDRESS=127.0.0.20
openssl req -new -newkey rsa:2048 -nodes -keyout "$WORK_DIR/standin.key" -subj /CN=api.github.com \
	-out "$WORK_DIR/standin.csr" 2>/dev/null
printf 'subjectAltName=DNS:api.github.com,DNS:github.com,DNS:uploads.github.com,IP:%s\n' "$STANDIN_ADDRESS" >"$WORK_DIR/standin.ext"
openssl x509 -req -in "$WORK_DIR/standin.csr" -CA "$CA" -CAkey "$AIRLOCK_CA_KEY_FILE" -CAcreateserial \
	-days 1 -extfile "$WORK_DIR/standin.ext" -out "$WORK_DIR/standin.pem" 2>/dev/null
RECORDS="$WORK_DIR/github-records.jsonl"
: >"$RECORDS"
python3 "$SCRIPT_DIR/proxy-role/github_standin.py" --address "$STANDIN_ADDRESS:443" \
	--cert "$WORK_DIR/standin.pem" --key "$WORK_DIR/standin.key" --records "$RECORDS" 2>"$WORK_DIR/standin.log" &
PIDS+=($!)
await "the answering stand-in" 30 "$WORK_DIR/standin.log" 'github-standin listening' "${PIDS[-1]}"

# --- This check's broker: the stack's own, with the rolegithub kind, PROXY_ROLE_APP and
# PROXY_ROLE_PLAIN added. A profile is behind the proxy when its source's proxy tag names a kind,
# so the App is stored twice: proxy-role-app tagged with rolegithub, and proxy-role-plain-app with
# no proxy tag, the source of PROXY_ROLE_PLAIN. ---
STACK_APP="$(jq -c '[.secrets[] | select(.name | endswith("airlock-github-app"))][0]' "$AIRLOCK_FAKE_SECRETS_FILE")"
SECRET_PREFIX="$(jq -r '.name | sub("airlock-github-app$"; "")' <<<"$STACK_APP")"
ROLE_KIND=rolegithub
jq --arg prefix "$SECRET_PREFIX" --arg kind "$ROLE_KIND" --argjson stack "$STACK_APP" --rawfile key "$APP_KEY_FILE" \
	--arg owners "$ACCOUNT_A $ACCOUNT_B" --arg appid "$(jq -r .github_app_id "$WORK_DIR/stands.json")" \
	'({owner: $stack.tags.owner, tier: "agent", type: "github-app", "github-owners": $owners}) as $tags
	 | ({app_id: $appid, private_key: $key} | tojson) as $value
	 | .secrets += [
	   {name: ($prefix + "proxy-role-app"), kms_key_id: $stack.kms_key_id, tags: ($tags + {proxy: $kind}), value: $value},
	   {name: ($prefix + "proxy-role-plain-app"), kms_key_id: $stack.kms_key_id, tags: $tags, value: $value}]' \
	"$AIRLOCK_FAKE_SECRETS_FILE" >"$WORK_DIR/fake-secrets.json"
chmod 600 "$WORK_DIR/fake-secrets.json"
PERMISSIONS='{"contents": "read", "metadata": "read"}'
# The GitHub kind's hosts, as the profiles file's rolegithub kind binds them and each forwarder is
# told them (--github-host, the role's runtime.kubernetes.agent_secrets.github_hosts in a pod).
GITHUB_HOSTS=(api.github.com github.com uploads.github.com)
GITHUB_HOST_FLAGS=()
for host in "${GITHUB_HOSTS[@]}"; do GITHUB_HOST_FLAGS+=(--github-host "$host"); done
jq --arg a "$ACCOUNT_A" --arg b "$ACCOUNT_B" --argjson permissions "$PERMISSIONS" --arg kind "$ROLE_KIND" \
	--argjson hosts "$(printf '%s\n' "${GITHUB_HOSTS[@]}" | jq -R . | jq -sc .)" \
	'.kinds += [{name: $kind, hosts: $hosts, ways: ["mint"]}]
	 | .profiles += [
	   {name: "PROXY_ROLE_APP", github: {accounts: [$a, $b], permissions: $permissions}},
	   {name: "PROXY_ROLE_PLAIN", source: "PROXY_ROLE_PLAIN_APP", github: {accounts: [$a, $b], permissions: $permissions}},
	   {name: "PROXY_ROLE_RENEW", source: "PROXY_ROLE_APP", github: {accounts: [$a, $b], permissions: $permissions}}]' \
	"$AIRLOCK_PROFILES_FILE" >"$WORK_DIR/profiles.json"
API_PORT="$(free_port)"
PROXY_PORT="$(free_port)"
BROKER_URL="http://127.0.0.1:$API_PORT"
broker_env_of "$STACK_BROKER_PID" \
	BROKER_PROFILES_FILE="$WORK_DIR/profiles.json" \
	BROKER_FAKE_SECRETS_FILE="$WORK_DIR/fake-secrets.json" \
	BROKER_LISTEN_ADDR="127.0.0.1:$API_PORT" \
	BROKER_PUBLIC_URL="$BROKER_URL" \
	BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:$PROXY_PORT" \
	BROKER_AIRLOCK_PROXY_URL="http://127.0.0.1:$PROXY_PORT" \
	BROKER_METRICS_LISTEN_ADDR="127.0.0.1:0" \
	BROKER_AIRLOCK_TEST_GITHUB_API_BASE="$MINT_URL" \
	BROKER_AIRLOCK_TEST_RESOLVE="api.github.com=$STANDIN_ADDRESS github.com=$STANDIN_ADDRESS uploads.github.com=$STANDIN_ADDRESS" \
	BROKER_AIRLOCK_TEST_DIAL_ALLOW="api.github.com:443 github.com:443 uploads.github.com:443"
(with_broker_env "$BROKER_BIN") >"$WORK_DIR/broker.log" 2>&1 &
BROKER_PID=$!
PIDS+=("$BROKER_PID")
await "this check's broker" 120 "$WORK_DIR/broker.log" '"message":"airlock proxy listening"' "$BROKER_PID"
for entry in "${BROKER_ENV[@]}"; do
	case "$entry" in BROKER_DATABASE_URL=*) BROKER_DB_URL="${entry#BROKER_DATABASE_URL=}" ;; esac
done

# --- The role's forwarder. ---
FORWARDER_PORT="$(free_port)"
FORWARDER="http://127.0.0.1:$FORWARDER_PORT"
GH_DIR="$WORK_DIR/role/gh"
AGENT_SECRETS_URL="$BROKER_URL" AGENT_SECRETS_KEY_DIR="$AGENT_SECRETS_KEY_DIR" \
	"$LEGION" forwarder --role implementer --listen "127.0.0.1:$FORWARDER_PORT" --secret PROXY_ROLE_APP \
	--reason "proxy-role-check: the implementer of ACME-1 through its forwarder" "${GITHUB_HOST_FLAGS[@]}" --gh-dir "$GH_DIR" \
	--default-owner "$ACCOUNT_A" >"$WORK_DIR/forwarder.log" 2>&1 &
PIDS+=($!)
await "the forwarder's first grant" 60 "$WORK_DIR/forwarder.log" 'holds a grant of the role' "${PIDS[-1]}"
grant_of() { grep -o 'grant=[0-9a-f-]*' "$WORK_DIR/forwarder.log" | tail -n 1 | cut -d= -f2; }
FIRST_GRANT="$(grant_of)"

# --- The role's tools, as a pod runs them. ---
mkdir -p "$WORK_DIR/home"
role_env=(env -i "PATH=$PATH" "HOME=$HOME" "USER=${USER:-legion}" "TMPDIR=$WORK_DIR"
	"HTTPS_PROXY=$FORWARDER" "https_proxy=$FORWARDER" "NO_PROXY=" "no_proxy="
	"SSL_CERT_FILE=$CA" "GIT_SSL_CAINFO=$CA" "GH_NO_UPDATE_NOTIFIER=1" "GH_PROMPT_DISABLED=1" "NO_COLOR=1"
	"LEGION_GH_OWNERS_DIR=$GH_DIR" "GIT_CONFIG_NOSYSTEM=1" "GIT_CONFIG_GLOBAL=/dev/null" "GIT_TERMINAL_PROMPT=0")
# gh_as <account> <gh args...>: the role's gh with the account's directory, as `legion gh` routes
# a command naming that account.
gh_as() {
	local account="$1"
	shift
	"${role_env[@]}" "GH_CONFIG_DIR=$GH_DIR/$account" timeout 30 "$GH" "$@"
}
git_as() {
	"${role_env[@]}" "GH_CONFIG_DIR=$GH_DIR/$ACCOUNT_A" timeout 30 git \
		-c credential.helper= -c "credential.helper=!$LEGION git-credential" -c credential.useHttpPath=true "$@"
}
records_since() { tail -n +"$(($1 + 1))" "$RECORDS"; }
record_count() { wc -l <"$RECORDS"; }
# call_reached <mark> <host> <account>: the stand-in's records after mark hold a call to host
# carrying an installation token the minting stand-in made for account, and none carrying a
# placeholder or another credential.
call_reached() {
	records_since "$1" | jq -e -s --arg host "$2" --arg account "$3" \
		'(map(select(.host == $host and .kind == "app" and .account == $account)) | length > 0)
		 and (map(select(.kind == "placeholder" or .kind == "other")) | length == 0)' >/dev/null 2>&1
}
# step <name> <command...>: runs one of the role's commands, keeping its output in
# $WORK_DIR/steps/<name>.out for a person to read, and answers its exit status.
mkdir -p "$WORK_DIR/steps"
step() {
	local name="$1"
	shift
	"$@" >"$WORK_DIR/steps/$name.out" 2>"$WORK_DIR/steps/$name.err"
}

mark="$(record_count)"
step gh-api gh_as "$ACCOUNT_A" api "repos/$ACCOUNT_A/$REPO" --jq .full_name && ok=0 || ok=1
judge "$([ "$ok" = 0 ] && [ "$(cat "$WORK_DIR/steps/gh-api.out")" = "$ACCOUNT_A/$REPO" ] && call_reached "$mark" api.github.com "$ACCOUNT_A" && echo 0)" \
	"gh api repos/$ACCOUNT_A/$REPO answers through the forwarder, and the stand-in got an installation token of $ACCOUNT_A"

mark="$(record_count)"
step gh-pr-list gh_as "$ACCOUNT_B" pr list -R "$ACCOUNT_B/$REPO" && ok=0 || ok=1
judge "$([ "$ok" = 0 ] && call_reached "$mark" api.github.com "$ACCOUNT_B" && echo 0)" \
	"gh pr list -R $ACCOUNT_B/$REPO answers, and the stand-in got an installation token of $ACCOUNT_B from the same grant"

mark="$(record_count)"
step gh-graphql gh_as "$ACCOUNT_A" api graphql -f 'query=query { viewer { login } }' --jq .data.viewer.login && ok=0 || ok=1
judge "$([ "$ok" = 0 ] && [ "$(cat "$WORK_DIR/steps/gh-graphql.out")" = "proxy-role-check[bot]" ] && call_reached "$mark" api.github.com "$ACCOUNT_A" && echo 0)" \
	"gh api graphql answers the viewer query with the issue owner's token"

step gh-stack gh_as "$ACCOUNT_A" stack --help && ok=0 || ok=1
judge "$([ "$ok" = 0 ] && grep -qi 'stack' "$WORK_DIR/steps/gh-stack.out" "$WORK_DIR/steps/gh-stack.err" && echo 0)" "gh stack --help runs as the role's gh extension"

mark="$(record_count)"
step git-ls-remote git_as ls-remote "https://github.com/$ACCOUNT_A/$REPO.git" && ok=0 || ok=1
judge "$([ "$ok" = 0 ] && grep -q 'refs/heads/main' "$WORK_DIR/steps/git-ls-remote.out" && call_reached "$mark" github.com "$ACCOUNT_A" && echo 0)" \
	"git ls-remote https://github.com/$ACCOUNT_A/$REPO.git answers through legion git-credential's placeholder"

git init -q "$WORK_DIR/repo"
git -C "$WORK_DIR/repo" -c user.name=proxy-role-check -c user.email=check@example.test commit -q --allow-empty -m 'proxy-role-check'
mark="$(record_count)"
step git-push git_as -C "$WORK_DIR/repo" push --dry-run "https://github.com/$ACCOUNT_B/$REPO.git" HEAD:refs/heads/proxy-role-check && ok=0 || ok=1
judge "$([ "$ok" = 0 ] && call_reached "$mark" github.com "$ACCOUNT_B" && echo 0)" \
	"git push --dry-run to $ACCOUNT_B/$REPO reads the receive-pack advertisement with $ACCOUNT_B's token"

# --- The role's files and helper hold placeholders alone. ---
placeholders="$(assert_placeholders_only "$GH_DIR")" && ok=0 || ok=1
judge "$ok" "the role's hosts.yml files hold placeholders alone ($placeholders)"
helper_ok=0
for account in "$ACCOUNT_A" "$ACCOUNT_B"; do
	answer="$(printf 'protocol=https\nhost=github.com\npath=%s/%s.git\n\n' "$account" "$REPO" |
		"${role_env[@]}" "GH_CONFIG_DIR=$GH_DIR/$ACCOUNT_A" "$LEGION" git-credential get 2>/dev/null)" || helper_ok=1
	password="$(printf '%s\n' "$answer" | sed -n 's/^password=//p')"
	case "$password" in agent-secrets-placeholder-*) ;; *) helper_ok=1 ;; esac
done
judge "$helper_ok" "legion git-credential answers each account's placeholder and no token"

# --- A revocation: the role's calls work again within 60 seconds. ---
revoked_at="$(date +%s)"
AGENT_SECRETS_URL="$BROKER_URL" AGENT_SECRETS_KEY_DIR="$AGENT_SECRETS_KEY_DIR" "$AGENT_SECRETS_BIN" revoke "$FIRST_GRANT" >/dev/null 2>&1 && ok=0 || ok=1
judge "$ok" "the role's grant ($FIRST_GRANT) is revoked at the broker"
failures=0
recovered=""
while [ $(($(date +%s) - revoked_at)) -le 75 ]; do
	if gh_as "$ACCOUNT_A" api "repos/$ACCOUNT_A/$REPO" --jq .full_name >/dev/null 2>&1; then
		recovered="$(($(date +%s) - revoked_at))"
		break
	fi
	failures=$((failures + 1))
	sleep 2
done
second_grant="$(grant_of)"
judge "$([ -n "$recovered" ] && [ "$recovered" -le 60 ] && [ "$second_grant" != "$FIRST_GRANT" ] && echo 0)" \
	"after the revocation the role's gh worked again in ${recovered:-no} seconds, after $failures failed calls, under a new grant"
# Over the whole run, the revocation's failed call among them: a connection the broker answered
# 407 is failed by the forwarder rather than carried under the new grant with the old placeholder.
placeholder_records="$(jq -s 'map(select(.kind == "placeholder")) | length' "$RECORDS")"
judge "$([ "$placeholder_records" = 0 ] && echo 0)" "no placeholder reached the stand-in GitHub, after the revocation either ($(record_count) calls recorded)"

# --- A renewal: a client that read its placeholder before the forwarder renewed, and opens a new
# connection after it, while the previous grant is still live. A gh process alive across the
# renewal does that whenever it opens a second connection. The forwarder's first grant of
# PROXY_ROLE_RENEW is written into the broker's database as if 49 min 45 s of its hour had passed,
# so the forwarder asks for the next one 15 seconds after it holds it, and the broker, which reuses
# an automatic grant only in its first half, gives a new grant then; the old grant stays live. ---
[ -n "$BROKER_DB_URL" ] || {
	echo "proxy-role-check: this check's broker names no BROKER_DATABASE_URL, so the renewal step cannot back-date a grant" >&2
	exit 1
}
# The trigger matches this reason, which no other step's request carries.
RENEW_REASON="proxy-role-check: a renewal of the implementer of ACME-1"
pg_url_run "$BROKER_DB_URL" psql -v ON_ERROR_STOP=1 -q >/dev/null <<'SQL'
create or replace function proxy_role_check_backdate() returns trigger as $$
declare why text;
begin
  select r.reason into why from requests r where r.id = new.request_id;
  if why = 'proxy-role-check: a renewal of the implementer of ACME-1' and not exists (
      select 1 from grants g join requests r on r.id = g.request_id where r.reason = why) then
    new.created_at := now() - interval '49 minutes 45 seconds';
    new.expires_at := new.created_at + interval '60 minutes';
  end if;
  return new;
end $$ language plpgsql;
create trigger proxy_role_check_backdate before insert on grants for each row execute function proxy_role_check_backdate();
SQL
DROP_TRIGGER=true
RENEW_PORT="$(free_port)"
RENEW_LOG="$WORK_DIR/forwarder-renew.log"
RENEW_GH="$WORK_DIR/role-renew/gh"
AGENT_SECRETS_URL="$BROKER_URL" AGENT_SECRETS_KEY_DIR="$AGENT_SECRETS_KEY_DIR" \
	"$LEGION" forwarder --role implementer --listen "127.0.0.1:$RENEW_PORT" --secret PROXY_ROLE_RENEW \
	--reason "$RENEW_REASON" "${GITHUB_HOST_FLAGS[@]}" --gh-dir "$RENEW_GH" --default-owner "$ACCOUNT_A" >"$RENEW_LOG" 2>&1 &
RENEW_PID=$!
PIDS+=("$RENEW_PID")
await "the renewing forwarder's first grant" 60 "$RENEW_LOG" 'holds a grant of the role' "$RENEW_PID"
old_placeholder="$(sed -n 's/^ *oauth_token: //p' "$RENEW_GH/$ACCOUNT_A/hosts.yml" | head -n 1)"
await "the renewing forwarder's renewal" 60 "$RENEW_LOG" 'the grant nears its expiry' "$RENEW_PID"
mapfile -t renew_grants < <(grep -o 'grant=[0-9a-f-]*' "$RENEW_LOG" | cut -d= -f2)
mark="$(record_count)"
# The header goes on standard input, as every credential-shaped value of these scripts does, so
# no process's arguments hold it.
status="$(printf 'Authorization: token %s\n' "$old_placeholder" | "${role_env[@]}" "HTTPS_PROXY=http://127.0.0.1:$RENEW_PORT" "https_proxy=http://127.0.0.1:$RENEW_PORT" \
	curl -sS -o /dev/null -w '%{http_code}' --max-time 20 -H @- \
	"https://api.github.com/repos/$ACCOUNT_A/$REPO")" || status=failed
judge "$([ "$status" = 200 ] && [ "${#renew_grants[@]}" -ge 2 ] && [ "${renew_grants[0]}" != "${renew_grants[1]}" ] && call_reached "$mark" api.github.com "$ACCOUNT_A" && echo 0)" \
	"after a renewal (grant ${renew_grants[0]:-none} to ${renew_grants[1]:-none}), a new connection carrying the previous grant's placeholder, read before the renewal, gets $ACCOUNT_A's token, and no placeholder reaches the stand-in GitHub (answer $status)"
kill -TERM "$RENEW_PID"
wait "$RENEW_PID" 2>/dev/null || true
live="$(pg_url_run "$BROKER_DB_URL" psql -Atqc "select count(*) from grants g join requests r on r.id = g.request_id where r.reason = '$RENEW_REASON' and g.revoked_at is null")"
judge "$([ "${#renew_grants[@]}" -ge 2 ] && [ "$live" = 0 ] && echo 0)" \
	"the forwarder's stop revoked both grants it held, as the broker's grants table reads ($live live)"

# --- A public fetch while the broker's proxy port answers nothing. ---
kill -STOP "$BROKER_PID"
public="$(curl -sS --max-time 15 --proxy "$FORWARDER" --cacert "$WORK_DIR/standin.pem" "https://$STANDIN_ADDRESS/public" 2>&1)" && ok=0 || ok=1
bound=1
curl -sS --max-time 8 --proxy "$FORWARDER" --cacert "$CA" "https://api.github.com/repos/$ACCOUNT_A/$REPO" >/dev/null 2>&1 || bound=0
kill -CONT "$BROKER_PID"
judge "$([ "$ok" = 0 ] && [ "$public" = public ] && echo 0)" "a fetch from a host the grant does not bind works while the broker's proxy port answers nothing"
judge "$bound" "control: a call to a bound host fails while the broker's proxy port answers nothing"

# --- The negative control: the token the daemon reads without the role map fails the check. ---
CONTROL_DIR="$WORK_DIR/control/gh"
mkdir -p "$CONTROL_DIR/$ACCOUNT_A"
release="$(AGENT_SECRETS_URL="$BROKER_URL" AGENT_SECRETS_KEY_DIR="$AGENT_SECRETS_KEY_DIR" "$AGENT_SECRETS_BIN" PROXY_ROLE_PLAIN \
	--ttl 10m --reason 'proxy-role-check: the negative control' -- printenv PROXY_ROLE_PLAIN 2>/dev/null)" || release=""
token="$(printf '%s' "$release" | jq -r --arg a "$ACCOUNT_A" '.installations[$a].token // empty' 2>/dev/null || true)"
printf 'github.com:\n    oauth_token: %s\n    user: x-access-token\n    git_protocol: https\n    users:\n        x-access-token:\n            oauth_token: %s\n' \
	"$token" "$token" >"$CONTROL_DIR/$ACCOUNT_A/hosts.yml"
control="$(assert_placeholders_only "$CONTROL_DIR")" && control_status=0 || control_status=1
judge "$([ -n "$token" ] && [ "$control_status" = 1 ] && echo 0)" \
	"negative control: a hosts.yml holding the App token the daemon reads without the role map (len ${#token}, sha256 $(digest "$token")) fails the placeholder check ($control)"

echo
duration=$(($(date -u +%s) - STARTED_AT))
if [ "$PASSED" -eq "$TOTAL" ]; then
	echo "proxy-role-check: passed ($PASSED of $TOTAL)"
	echo "scenario s-roles: RAN, passed, head $HEAD_COMMIT, ${duration}s"
	exit 0
fi
echo "proxy-role-check: failed ($((TOTAL - PASSED)) of $TOTAL)"
echo "scenario s-roles: RAN, failed, head $HEAD_COMMIT, ${duration}s"
exit 1
