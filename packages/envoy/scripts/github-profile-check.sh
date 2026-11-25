#!/usr/bin/env bash
# packages/envoy/scripts/github-profile-check.sh
#
# Runs the end-to-end checks of the broker's named GitHub profiles on a local stack this script
# starts, and says whether each property passed: p1, that the broker serves the profiles file it
# was given; p2, that a GitHub profile's token is asked for with its permissions cut to what the
# installation holds; p3, that a proxied profile's token stays in the proxy and goes only into
# the requests its proxy block allows; and p4, that a body still carrying the removed github_app
# field is refused with a code of its own.
#
# It starts scripts/dev-broker.sh, whose profiles file names the stack's three github-app
# secrets and the two proxied profiles of DEMO_GITHUB_APP, with two settings of that script's:
# DEV_BROKER_GITHUB_PERMISSIONS, so the App's installations hold less than the profiles ask for
# (INSTALLATION_PERMISSIONS below), and DEV_BROKER_PROXIED_GITHUB_IP, so the stand-in GitHub is
# served over TLS on port 443 of a loopback address, where the broker's proxy reaches it under the
# name api.github.test. It is the one driver that passes the stand-in an installation map
# (dev-airlock-stands --installation-permissions); every other driver reads the stand's own. It
# enrols one pod session of the registered service that owns DEMO_GITHUB_APP, as the Legion
# daemon enrols a worker pod, so every request below is granted without asking.
#
# What each check drives:
#
#   - p1: the broker logs `broker: profiles applied` once, serving every entry of the file and
#     refusing none, with the SHA-256 of the file it read; `broker profiles check` of that file
#     exits 0 and reports each of its profiles added; and a github-app secret that no profile
#     names, added to the store while the stack runs, is refused by its name, PROFILE_REFUSED
#     naming github-app-no-profile, and runs nothing.
#   - p2: DEMO_GITHUB_APP's token request names its permissions cut to what the installation
#     holds; the broker logs one line for each entry the cut left out, and the values answer
#     names the same entries (github_left_out); DEMO_GITHUB_APP_CASE_PAIR, whose installations
#     hold none of its permissions but metadata, is refused PROFILE_MINT_REFUSED with no token
#     request to GitHub; and a token wider than the request is refused MINT_FAILED, runs nothing,
#     and is given back.
#   - p3: DEMO_GITHUB_READ's child holds a placeholder, which GitHub refuses when the child sends
#     it straight there, and its call through the proxy reaches GitHub with a token GitHub minted,
#     asked for with the profile's permissions cut; a POST, which the proxy block does not allow,
#     is refused by the proxy and reaches GitHub not at all; and GitHub's three routes that read
#     what a token holds answer by what the cut left in it: a deploy-key list is refused to
#     DEMO_GITHUB_READ and served to DEMO_GITHUB_ADMIN_READ, an organization's installations are
#     refused to DEMO_GITHUB_ADMIN_READ, whose organization_administration the cut left out, and
#     DEMO_GITHUB_APP's statuses at write write a status for the commit the repository holds and
#     are answered 422 for another.
#   - p4: POST /v1/requests and POST /v1/grants/{id}/values, each sent with curl and a body
#     carrying github_app, are each refused 400 GITHUB_APP_FIELD_REMOVED with its message, the
#     stack's broker running with BROKER_GITHUB_APP_FIELD=off.
#
# It prints one PASS or FAIL line for each property, each naming the exit status or the HTTP
# status of what it read, because a property that failed and a command that never ran look alike
# in a transcript that holds neither, and a last line `all <n> assertions passed` when every one
# passed, so a reader tells a complete run from a cut-off one. When the stack did not start, or
# a step every property rests on could not be taken, it prints one UNKNOWN line saying which,
# since no property was read at all. It exits 0 only when every property passed, 1 when one did
# not, 2 on a usage error and 3 on UNKNOWN.
#
# No line it prints is a credential. A child prints the HTTP status of each call it makes and no
# part of any answer's body; a token read back from the stand-in's records is printed by its
# length alone; and a refusal is printed as agent-secrets wrote it, which names no part of any
# token.
#
# Whether a command ran is read from the child's standard output alone, apart from
# agent-secrets' own standard error, and the test is for a line that is exactly `ran`, the word
# the child prints: agent-secrets writes `the grant from ...` and `was granted ...` on reachable
# paths of these same runs, and each of those holds the letters of `ran`.
#
# The stack is waited for in its two phases, each under a bound of its own: dev-broker.sh's six
# go builds, which take many minutes on a cold build cache, and the start of what they built. It
# runs in a session of its own and is stopped as one process group, so a stop in the middle of a
# build - this script's own exit, a wait that ran out, Ctrl-C - ends that build with the stack.
#
# Its negative control swaps the broker and nothing else: DEV_BROKER_BROKER_BIN, which this
# script passes to dev-broker.sh as the caller set it, names a broker binary dev-broker.sh starts
# in place of the one it builds. Built from a commit before the profile mint and the proxy's
# profile token, it fails p2 and p3.
#
# Usage: [DEV_BROKER_POSTGRES_URL=postgres://<user>@<host>:<port>/<database>] \
#          packages/envoy/scripts/github-profile-check.sh [--keep-stack]
#        --keep-stack leaves the stack and the work directory up for a person to look at a
#        failure with. --help prints this usage.
#
# DEV_BROKER_POSTGRES_URL is passed to dev-broker.sh as it is, so a pod with no container runtime
# names its test server there; unset, dev-broker.sh starts its own Postgres container.
# GITHUB_PROFILE_CHECK_GITHUB_IP is the loopback address the stand-in GitHub takes port 443 of,
# 127.0.0.9 unless set, for a machine where another run holds that one.
#
# Needs bash, curl, jq, go and setsid, and a machine that lets this user bind port 443 of a
# loopback address, as a Legion pod does: the broker's proxy decrypts a bound host's port 443
# alone. On a machine that does not, dev-airlock-stands cannot bind it, the stack does not start,
# and this script prints UNKNOWN naming the stands' refusal.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
	cat <<'EOF'
usage: [DEV_BROKER_POSTGRES_URL=postgres://<user>@<host>:<port>/<database>] \
         github-profile-check.sh [--keep-stack]

Runs checks p1 to p4 of the broker's named GitHub profiles on a local stack of its own and prints
one PASS or FAIL line for each property they name.

  --keep-stack   leave the stack and this run's work directory up, to look at a failure with
  --help         print this

DEV_BROKER_BROKER_BIN names a broker binary to run in place of the one the stack builds, which is
how the negative control swaps the broker alone. GITHUB_PROFILE_CHECK_GITHUB_IP is the loopback
address whose port 443 the stand-in GitHub binds (127.0.0.9 unless set).

It exits 0 only when every property passed, 1 when one did not, 2 on a usage error and 3 when
the stack did not start (UNKNOWN).
EOF
}

KEEP_STACK=false
while [ $# -gt 0 ]; do
	case "$1" in
	--keep-stack) KEEP_STACK=true ;;
	--help | -h)
		usage
		exit 0
		;;
	*)
		echo "github-profile-check: unexpected argument $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

# The loopback address the proxied GitHub listens on, checked before anything starts, as
# dev-broker.sh checks it again.
GITHUB_IP="${GITHUB_PROFILE_CHECK_GITHUB_IP:-127.0.0.9}"
if ! [[ "$GITHUB_IP" =~ ^127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
	echo "github-profile-check: GITHUB_PROFILE_CHECK_GITHUB_IP must be a loopback address of 127.0.0.0/8, not $GITHUB_IP" >&2
	exit 2
fi

# What the App's installations hold for this run: acme-org's holds the reads the profiles ask for
# but attestations, holds contents and pull requests at read alone, issues and statuses at write,
# administration and members but not organization_administration, and no workflows; ada's and
# Ada's hold metadata alone, as an installation GitHub lists holds at the least, so any profile of
# theirs that asks for more is an empty cut. Each profile's expected cut below is read off this
# map and the stack's profiles file (dev-broker.sh).
INSTALLATION_PERMISSIONS="acme-org=actions:read,checks:read,contents:read,issues:write,metadata:read,pull_requests:read,statuses:write,administration:read,members:read ada=metadata:read Ada=metadata:read"
# DEMO_GITHUB_APP asks for contents, pull requests, statuses and workflows at write and issues and
# metadata at read; READ asks for eight reads; ADMIN_READ for those and administration, members and
# organization_administration.
APP_CUT='{"contents":"read","issues":"read","metadata":"read","pull_requests":"read","statuses":"write"}'
APP_LEFT_OUT="contents:write pull_requests:write workflows"
READ_CUT='{"actions":"read","checks":"read","contents":"read","issues":"read","metadata":"read","pull_requests":"read","statuses":"read"}'

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/github-profile-check.XXXXXX")"
STACK_LOG="$WORK_DIR/stack.log"
KEY_DIR="$WORK_DIR/session-key"
CHILD="$WORK_DIR/child.sh"
STACK_PID=""

cleanup() {
	if [ "$KEEP_STACK" = true ] && [ -n "$STACK_PID" ]; then
		echo "github-profile-check: the stack is still up (dev-broker.sh pid $STACK_PID, log $STACK_LOG);" >&2
		echo "github-profile-check: stop it with kill -- -$STACK_PID, its process group, which drops its database and ends any build it is still running" >&2
		return
	fi
	# SIGTERM, never SIGKILL: dev-broker.sh's own EXIT trap is what stops its processes and
	# drops the database it made. It goes to the stack's whole process group, a signal
	# dev-broker.sh is written to take (its tees ignore it), so a go build of dev-broker.sh's
	# ends with the stack. The group is signalled whether or not dev-broker.sh still runs: one
	# killed outright in the middle of a build leaves that build in the group, and a group's id
	# is handed to no other process while the group holds one.
	if [ -n "$STACK_PID" ]; then
		kill -TERM -- "-$STACK_PID" 2>/dev/null || true
		wait "$STACK_PID" 2>/dev/null || true
	fi
	if [ "$KEEP_STACK" = true ]; then
		echo "github-profile-check: work directory kept at $WORK_DIR" >&2
	else
		rm -rf "$WORK_DIR"
	fi
}
trap cleanup EXIT

# unknown ends the run having read no property: the stack did not start, or a step every
# property rests on could not be taken. It names which, and the last lines of the log that says
# why, so the run reads as unread rather than as a pass or a failure of anything.
unknown() { # <what could not be done> [log to show]
	printf 'UNKNOWN %s\n' "$1"
	if [ -n "${2:-}" ]; then
		tail -n 20 "$2" >&2 || true
	fi
	exit 3
}

# --- What a run says ---------------------------------------------------------------------------
#
# One line per property, printed as it is judged, and one pair of counters behind the verdict.
# Nothing else decides the exit status.

PASSED=0
TOTAL=0

# judge records one property: it runs the condition its arguments name - never bare, so this
# script's own errexit never ends a run on a property that failed - and prints the line.
judge() { # <what the property is> <condition...>
	local what="$1"
	shift
	TOTAL=$((TOTAL + 1))
	if "$@"; then
		PASSED=$((PASSED + 1))
		printf 'PASS %s\n' "$what"
		return 0
	fi
	printf 'FAIL %s\n' "$what"
	return 0
}

# The conditions the properties are judged by. holds reports whether text holds a string,
# without the text reaching the output; is compares two strings; counted reports a count of at
# least one. printed_ran reports whether the child printed its word, read from the child's
# standard output alone (see the header); ran is a command that exited as it should and printed
# that word, and failed_without_running one that exited non-zero and printed none.
holds() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }
is() { [ "$1" = "$2" ]; }
counted() { [ "${1:-0}" -gt 0 ]; }
printed_ran() { printf '%s\n' "$1" | grep -qx ran; }
ran() { is "$1" "$2" && printed_ran "$3"; }
failed_without_running() { ! is 0 "$1" && ! printed_ran "$2"; }
both() { "$1" "$2" "$3" && "$4" "$5" "$6"; }

# answered is the status one line of the child's output carries for a call, and "nothing" for a
# call the child never made, so each line below names what it read.
answered() { # <label> <the child's output>
	local status
	status="$(printf '%s\n' "$2" | sed -n "s#^$1 ##p" | head -n 1 || true)"
	printf '%s' "${status:-nothing}"
}

# await waits for a line a process logs, and ends the run UNKNOWN the moment that process dies:
# a stack that never came up proves nothing, and each property below would fail for a reason
# that is not the property's.
await() { # <what> <seconds> <log> <pattern> <pid>
	local i
	for ((i = 0; i < $2 * 2; i++)); do
		if grep -qE "$4" "$3" 2>/dev/null; then return 0; fi
		if ! kill -0 "$5" 2>/dev/null; then
			unknown "the stack did not start: $1 exited before it was ready; see $3" "$3"
		fi
		sleep 0.5
	done
	unknown "the stack did not start: $1 was not ready within $2 seconds; see $3" "$3"
}

# --- The stack -----------------------------------------------------------------------------------

echo "github-profile-check: starting the stack (dev-broker.sh, the proxied GitHub at $GITHUB_IP:443)..." >&2
: >"$STACK_LOG"
# dev-broker.sh runs in a session of its own, so it leads a process group of its own whose id is
# the pid $! names: setsid runs a process that leads no group in place rather than forking, and a
# background job of this script leads none. cleanup stops that whole group, because a go build
# dev-broker.sh is running is a process of its own that a signal to dev-broker.sh alone leaves
# running, as an orphan, once dev-broker.sh's EXIT trap has ended it.
# BROKER_GITHUB_APP_FIELD=off is p4's own condition: this broker answers the github_app field of
# an earlier agent-secrets for the requesters and the secrets its deployment names, and p4 holds
# the refusal a deployment gets once it sets this. With the field answered, p4's body, which
# carries no signed request, would be refused REQUEST_INVALID by the request object's own check
# before the field is read, and p4 would hold nothing about the field.
DEV_BROKER_PROXIED_GITHUB_IP="$GITHUB_IP" DEV_BROKER_GITHUB_PERMISSIONS="$INSTALLATION_PERMISSIONS" \
	BROKER_GITHUB_APP_FIELD=off \
	setsid bash "$SCRIPT_DIR/dev-broker.sh" >"$STACK_LOG" 2>&1 &
STACK_PID=$!
STACK_STARTED=$SECONDS
# One wait for each of the stack's two phases, so a wait that ends names the phase it ended in.
# The first holds dev-broker.sh's go builds, with the bound of a cold build cache: on a pod of two
# CPUs with an empty cache the six took 388 seconds, so 1800 leaves room for a pod slower or
# busier. The second holds the start of what they built, longer than dev-broker.sh's own waits
# after its builds add up to, so a process that never starts is named by dev-broker.sh's line
# about it rather than by this script's.
await "the stack's build" 1800 "$STACK_LOG" '^dev-broker: built ' "$STACK_PID"
echo "github-profile-check: the stack's binaries were built $((SECONDS - STACK_STARTED)) seconds after it was started; waiting for it to start them..." >&2
await "the stack, once built," 150 "$STACK_LOG" '^dev-broker: ready\.$' "$STACK_PID"
# Everything the banner exports, which is where this check's addresses, names and the pod and
# service it enrols come from.
eval "$(sed -n 's/^  \(export [A-Z][A-Z_0-9]*=.*\)$/\1/p' "$STACK_LOG")"
AGENT_SECRETS_BIN="$DEV_BROKER_DIR/bin/agent-secrets"
STANDS_BIN="$DEV_BROKER_DIR/bin/dev-airlock-stands"
BROKER_BIN="$DEV_BROKER_DIR/bin/broker"
BROKER_LOG="$DEV_BROKER_DIR/broker.log"
FAKE_SECRETS_FILE="$DEV_BROKER_DIR/fake-secrets.json"
OWNER="$DEV_BROKER_GITHUB_OWNER"
echo "github-profile-check: the stack is up (broker $AGENT_SECRETS_URL, proxy $DEV_BROKER_PROXY_URL, GitHub stand $DEV_BROKER_GITHUB_URL and $DEV_BROKER_GITHUB_PROXIED_URL)" >&2

# --- The session: a pod of the service that owns the stack's github-app secrets, enrolled as the
# Legion daemon enrols a worker pod, by the stack's own exports. ---
mkdir -p "$KEY_DIR"
THUMBPRINT="$("$AGENT_SECRETS_BIN" keygen --out "$KEY_DIR")" || unknown "the session's key could not be made"
"$STANDS_BIN" enroll-pod \
	--url "$AGENT_SECRETS_URL" \
	--service "$DEV_BROKER_SERVICE" \
	--token-file "$DEV_BROKER_DIR/registrar-token" \
	--pod-token-file "$DEV_BROKER_DIR/pod-token" \
	--pod-uid "$DEV_BROKER_POD_UID" \
	--thumbprint "$THUMBPRINT" \
	>"$WORK_DIR/enrollment.json" 2>"$WORK_DIR/enrollment.err" || unknown "the pod session could not be enrolled" "$WORK_DIR/enrollment.err"
ENROLLMENT_ID="$(jq -r .enrollment_id "$WORK_DIR/enrollment.json")"
if [ -z "$ENROLLMENT_ID" ] || [ "$ENROLLMENT_ID" = null ]; then
	unknown "the enrolment answered no enrollment id"
fi
printf '%s' "$ENROLLMENT_ID" >"$KEY_DIR/enrollment"
export AGENT_SECRETS_KEY_DIR="$KEY_DIR"
echo "github-profile-check: enrolled pod session $ENROLLMENT_ID (pod $DEV_BROKER_POD_UID, service $DEV_BROKER_SERVICE)" >&2

# --- The child the checks run ---------------------------------------------------------------
#
# It reads the value of the variable whose name it is given - the released value never reaches a
# command line, curl reading its header on standard input from printf, a shell builtin - and for
# each step prints one line: the step and the HTTP status GitHub, or the proxy, answered it, and
# no part of any answer's body. A value that is the JSON of installations is read for the
# account's token first, which is the token a caller of such a profile spends. direct calls the
# plain stand-in GitHub with no proxy, as a caller holding the value outside the child would;
# every other step calls the proxied GitHub through the proxy the release named, or, for the two
# status steps, the plain stand-in GitHub straight, since that profile has no proxy block.
cat >"$CHILD" <<'EOF'
#!/usr/bin/env bash
set -u
name="$1" direct="$2" proxied="$3" owner="$4" commit="$5"
shift 5
value="${!name}"
case "$value" in
"{"*) value="$(printf '%s' "$value" | jq -r --arg owner "$owner" '.installations[$owner].token // ""')" ;;
esac
call() { # <step> <curl arguments...>
	local step="$1"
	shift
	printf '%s %s\n' "$step" "$(printf 'Authorization: Bearer %s' "$value" | curl -s -o /dev/null -w '%{http_code}' -H @- "$@")"
}
status='{"state":"success","context":"github-profile-check"}'
for step in "$@"; do
	case "$step" in
	direct) call direct --noproxy '*' "$direct/repos/$owner/proofs" ;;
	read) call read "$proxied/repos/$owner/proofs" ;;
	keys) call keys "$proxied/repos/$owner/proofs/keys" ;;
	installations) call installations "$proxied/orgs/$owner/installations" ;;
	post) call post -X POST -d "$status" "$proxied/repos/$owner/proofs/statuses/$commit" ;;
	status) call status --noproxy '*' -X POST -d "$status" "$direct/repos/$owner/proofs/statuses/$commit" ;;
	status-other) call status-other --noproxy '*' -X POST -d "$status" "$direct/repos/$owner/proofs/statuses/0000000000000000000000000000000000000000" ;;
	ran) echo ran ;;
	esac
done
EOF
chmod +x "$CHILD"

# run_agent_secrets runs one agent-secrets command of this check: its exit status in STATUS,
# everything it wrote in OUTPUT - its own stderr and the child's output together, as a person at
# a terminal reads them - and the child's standard output on its own in CHILD_OUT, which every
# "no command ran" property and every step's status is read from.
STATUS=0
OUTPUT=""
CHILD_OUT=""
run_agent_secrets() {
	STATUS=0
	"$AGENT_SECRETS_BIN" "$@" >"$WORK_DIR/stdout" 2>"$WORK_DIR/stderr" || STATUS=$?
	CHILD_OUT="$(cat "$WORK_DIR/stdout")"
	OUTPUT="$(cat "$WORK_DIR/stdout" "$WORK_DIR/stderr")"
}

# child_of runs the child under one profile, with the steps named.
child_of() { # <profile> <step>...
	local profile="$1"
	shift
	run_agent_secrets "$profile" -- bash "$CHILD" "$profile" "$DEV_BROKER_GITHUB_URL" \
		"$DEV_BROKER_GITHUB_PROXIED_URL" "$OWNER" "$DEV_BROKER_GITHUB_COMMIT" "$@"
}

# records is every request one GitHub stand has been sent so far: the plain one, which the
# broker mints at, or the TLS one, which a command reaches through the proxy. A stand that was
# sent nothing answers null, so each reading starts `(. // [])`. The records carry tokens in
# their headers, so nothing here prints a record whole.
records() { curl -sS "$DEV_BROKER_GITHUB_RECORDS_URL"; }
tls_records() { curl -sS "$DEV_BROKER_GITHUB_TLS_RECORDS_URL"; }

# token_requests is how many token requests one installation has been sent so far, and
# asked_permissions the permissions of each one from an index on, one sorted JSON object a line.
token_requests() { # <installation id>
	records | jq --arg path "/app/installations/$1/access_tokens" '[(. // [])[] | select(.path == $path)] | length'
}
asked_permissions() { # <installation id> <index, from 0>
	records | jq -c --arg path "/app/installations/$1/access_tokens" --argjson at "$2" \
		'[(. // [])[] | select(.path == $path)][$at:][] | (.body | fromjson? // {} | .permissions // {})' | jq -cS .
}
# revokes is how many token revokes the stand has been sent (the minter giving back a token it
# minted and will not release).
revokes() {
	records | jq '[(. // [])[] | select(.method == "DELETE" and .path == "/installation/token")] | length'
}
# case_token_requests is token_requests of the two installations of the case pair, together.
case_token_requests() {
	local installation total=0
	for installation in ${DEV_BROKER_GITHUB_CASE_INSTALLATIONS//,/ }; do
		total=$((total + $(token_requests "$installation")))
	done
	printf '%s' "$total"
}

# answers sets what the stand answers every token request from then on. A route that refused the
# mode leaves the last answer in place, so the run ends UNKNOWN rather than read the next
# property against an answer it did not set.
answers() { # <mode>
	if ! curl -sS -f -X POST "$DEV_BROKER_GITHUB_ANSWER_URL" -H 'Content-Type: application/json' \
		-d "{\"mode\": \"$1\"}" >/dev/null; then
		unknown "the stand-in GitHub refused the answer $1"
	fi
	echo "github-profile-check: the GitHub stand now answers $1" >&2
}

# logged_left_out is the entries the broker's log says the cut of one profile left out, each
# once, sorted and space-separated.
logged_left_out() { # <profile>
	jq -Rr --arg name "$1" 'fromjson? | select(.message == "broker: a profile'"'"'s permission was left out, since the installation does not hold it" and .name == $name) | .permission' "$BROKER_LOG" |
		sort -u | tr '\n' ' ' | sed 's/ $//'
}

# release asks for a grant of one name and releases it as the CLI does, leaving the raw answer in
# RELEASE_JSON, the grant's id in GRANT_ID and the HTTP status of the release in RELEASE_STATUS.
# The answer holds a value and is never printed.
release() { # <name>
	local url proof
	GRANT_ID=""
	RELEASE_JSON="{}"
	RELEASE_STATUS=""
	if ! RELEASE_REQUEST="$("$AGENT_SECRETS_BIN" request "$1" --ttl 30m --json 2>"$WORK_DIR/request.err")"; then
		return 1
	fi
	GRANT_ID="$(printf '%s' "$RELEASE_REQUEST" | jq -r '.grant_id // ""')"
	[ -n "$GRANT_ID" ] || return 1
	url="$AGENT_SECRETS_URL/v1/grants/$GRANT_ID/values"
	proof="$("$AGENT_SECRETS_BIN" sign --method POST --url "$url")" || return 1
	RELEASE_STATUS="$(curl -sS -o "$WORK_DIR/release.json" -w '%{http_code}' -X POST -H "Proof: $proof" "$url")"
	RELEASE_JSON="$(cat "$WORK_DIR/release.json")"
}

# --- p1: the broker serves the profiles file it was given --------------------------------------

p1_digest="$(sha256sum "$DEV_BROKER_PROFILES_FILE" | cut -d' ' -f1)"
p1_count="$(jq '.profiles | length' "$DEV_BROKER_PROFILES_FILE")"
p1_lines="$(jq -Rc 'fromjson? | select(.message == "broker: profiles applied") | {served, refused, file_digest}' "$BROKER_LOG")"
judge "p1 the broker logged one profiles-applied line, serving all $p1_count of the file's profiles and refusing none, with the SHA-256 of the file it read (it logged: ${p1_lines:-no such line})" \
	is "{\"served\":$p1_count,\"refused\":0,\"file_digest\":\"$p1_digest\"}" "$p1_lines"

p1_check_status=0
p1_check="$("$BROKER_BIN" profiles check "$DEV_BROKER_PROFILES_FILE" 2>&1)" || p1_check_status=$?
p1_added="$(printf '%s\n' "$p1_check" | sed -n 's/^profile "\([A-Z_]*\)": added$/\1/p' | sort | tr '\n' ' ' | sed 's/ $//')"
p1_names="$(jq -r '.profiles[].name' "$DEV_BROKER_PROFILES_FILE" | sort | tr '\n' ' ' | sed 's/ $//')"
judge "p1 profiles check of the stack's file exits 0 and reports each of its profiles added (it exited $p1_check_status and reported added: ${p1_added:-none})" \
	both is 0 "$p1_check_status" is "$p1_names" "$p1_added"

# A github-app secret no profile names, added to the store as the stack runs: the broker rereads
# it on the session's signed reread and refuses it by its name, since every GitHub App token it
# mints is a profile's.
jq --arg prefix "example/agent-secrets/" '
	(.secrets[] | select(.name == ($prefix + "demo-github-app"))) as $app
	| .secrets += [$app | .name = ($prefix + "demo-github-app-unprofiled")]' \
	"$FAKE_SECRETS_FILE" >"$WORK_DIR/fake-secrets.json"
cat "$WORK_DIR/fake-secrets.json" >"$FAKE_SECRETS_FILE"
p1_reread_url="$AGENT_SECRETS_URL/v1/secrets/DEMO_GITHUB_APP_UNPROFILED/reread"
p1_reread="$(curl -sS -o /dev/null -w '%{http_code}' -X POST -H "Proof: $("$AGENT_SECRETS_BIN" sign --method POST --url "$p1_reread_url")" "$p1_reread_url")"
run_agent_secrets DEMO_GITHUB_APP_UNPROFILED -- sh -c 'echo ran'
judge "p1 a github-app secret no profile names is refused by its name and runs nothing (its reread answered $p1_reread; agent-secrets exited $STATUS)" \
	failed_without_running "$STATUS" "$CHILD_OUT"
judge "p1 that refusal is PROFILE_REFUSED and names github-app-no-profile (agent-secrets said: $OUTPUT)" \
	both holds PROFILE_REFUSED "$OUTPUT" holds github-app-no-profile "$OUTPUT"

# --- p2: a GitHub profile's token is asked for with its permissions cut ----------------------------

p2_mark="$(token_requests "$DEV_BROKER_GITHUB_INSTALLATION")"
child_of "$DEV_BROKER_GITHUB_SECRET" ran
p2_status="$STATUS"
p2_asked="$(asked_permissions "$DEV_BROKER_GITHUB_INSTALLATION" "$p2_mark" | sort -u | tr '\n' ' ' | sed 's/ $//')"
judge "p2 $DEV_BROKER_GITHUB_SECRET is released and its command runs (agent-secrets exited $p2_status)" \
	ran 0 "$p2_status" "$CHILD_OUT"
judge "p2 its token request asked for the profile's permissions cut to what the installation holds, $APP_CUT (it asked: ${p2_asked:-nothing})" \
	is "$APP_CUT" "$p2_asked"
p2_logged="$(logged_left_out "$DEV_BROKER_GITHUB_SECRET")"
judge "p2 the broker logged one left-out line for each entry the cut left out, $APP_LEFT_OUT (it logged: ${p2_logged:-none})" \
	is "$APP_LEFT_OUT" "$p2_logged"
release "$DEV_BROKER_GITHUB_SECRET" || true
p2_answer="$(printf '%s' "$RELEASE_JSON" | jq -r --arg n "$DEV_BROKER_GITHUB_SECRET" --arg o "$OWNER" '(.github_left_out[$n][$o] // []) | sort | join(" ")' 2>/dev/null || true)"
judge "p2 the values answer names the same entries in github_left_out (the release answered ${RELEASE_STATUS:-nothing}; github_left_out: ${p2_answer:-none})" \
	both is 200 "${RELEASE_STATUS:-}" is "$APP_LEFT_OUT" "$p2_answer"
P4_GRANT_ID="$GRANT_ID"

p2_case_before="$(case_token_requests)"
run_agent_secrets "$DEV_BROKER_GITHUB_CASE_SECRET" -- sh -c 'echo ran'
p2_case_status="$STATUS"
p2_case_output="$OUTPUT"
p2_case_child="$CHILD_OUT"
p2_case_after="$(case_token_requests)"
judge "p2 $DEV_BROKER_GITHUB_CASE_SECRET, whose installations hold none of its permissions but metadata, is refused and runs nothing (agent-secrets exited $p2_case_status)" \
	failed_without_running "$p2_case_status" "$p2_case_child"
judge "p2 that empty cut is refused PROFILE_MINT_REFUSED (agent-secrets said: $p2_case_output)" \
	holds PROFILE_MINT_REFUSED "$p2_case_output"
judge "p2 that empty cut sent GitHub no token request (it sent $((p2_case_after - p2_case_before)))" \
	is 0 "$((p2_case_after - p2_case_before))"

answers widen
p2_revokes="$(revokes)"
run_agent_secrets "$DEV_BROKER_GITHUB_SECRET" -- sh -c 'echo ran'
p2_wide_status="$STATUS"
p2_wide_output="$OUTPUT"
p2_wide_child="$CHILD_OUT"
p2_given_back="$(($(revokes) - p2_revokes))"
answers ordinary
judge "p2 a token wider than the request is not released and its command does not run (agent-secrets exited $p2_wide_status)" \
	failed_without_running "$p2_wide_status" "$p2_wide_child"
judge "p2 that refusal is MINT_FAILED (agent-secrets said: $p2_wide_output)" \
	holds MINT_FAILED "$p2_wide_output"
judge "p2 the wider token was given back to GitHub (the stand was sent $p2_given_back revokes)" \
	counted "$p2_given_back"

# --- p3: a proxied profile's token stays in the proxy and goes only where its block allows -------

p3_mark="$(token_requests "$DEV_BROKER_GITHUB_INSTALLATION")"
p3_posts_before="$(tls_records | jq '[(. // [])[] | select(.method == "POST")] | length')"
child_of "$DEV_BROKER_GITHUB_READ_PROFILE" direct read keys post
p3_status="$STATUS"
p3_output="$CHILD_OUT"
judge "p3 $DEV_BROKER_GITHUB_READ_PROFILE's child holds a placeholder, which GitHub refuses when it is sent there straight (agent-secrets exited $p3_status; the direct read answered $(answered direct "$p3_output"))" \
	is 401 "$(answered direct "$p3_output")"
judge "p3 the same child's read through the proxy is served (it answered $(answered read "$p3_output"))" \
	is 200 "$(answered read "$p3_output")"
p3_header="$(tls_records | jq -r --arg path "/repos/$OWNER/proofs" '[(. // [])[] | select(.method == "GET" and .path == $path)] | (last // {}) | .header.Authorization[0] // ""')"
judge "p3 that read reached GitHub carrying a token GitHub minted for $OWNER, not the placeholder (its Authorization header is ${#p3_header} characters)" \
	holds "Bearer ghs_standin_${OWNER}_" "$p3_header"
p3_asked="$(asked_permissions "$DEV_BROKER_GITHUB_INSTALLATION" "$p3_mark" | sort -u | tr '\n' ' ' | sed 's/ $//')"
judge "p3 the profile's token was asked for with its permissions cut, $READ_CUT (the requests asked: ${p3_asked:-nothing})" \
	is "$READ_CUT" "$p3_asked"
p3_posts="$(($(tls_records | jq '[(. // [])[] | select(.method == "POST")] | length') - p3_posts_before))"
judge "p3 a POST, which the proxy block does not allow, is refused by the proxy (it answered $(answered post "$p3_output"))" \
	is 403 "$(answered post "$p3_output")"
judge "p3 that POST reached GitHub not at all (the proxied GitHub recorded $p3_posts POSTs)" \
	is 0 "$p3_posts"
judge "p3 a deploy-key list, which needs administration, is refused to $DEV_BROKER_GITHUB_READ_PROFILE, which does not ask for it (it answered $(answered keys "$p3_output"))" \
	is 403 "$(answered keys "$p3_output")"

child_of "$DEV_BROKER_GITHUB_ADMIN_READ_PROFILE" keys installations
p3_admin_status="$STATUS"
p3_admin_output="$CHILD_OUT"
judge "p3 the same list is served to $DEV_BROKER_GITHUB_ADMIN_READ_PROFILE, whose cut keeps administration (agent-secrets exited $p3_admin_status; it answered $(answered keys "$p3_admin_output"))" \
	is 200 "$(answered keys "$p3_admin_output")"
judge "p3 an organization's installations, which need organization_administration, are refused to $DEV_BROKER_GITHUB_ADMIN_READ_PROFILE, whose cut left it out (it answered $(answered installations "$p3_admin_output"))" \
	is 403 "$(answered installations "$p3_admin_output")"

child_of "$DEV_BROKER_GITHUB_SECRET" status status-other
p3_app_status="$STATUS"
p3_app_output="$CHILD_OUT"
judge "p3 $DEV_BROKER_GITHUB_SECRET's token, whose cut keeps statuses at write, writes a status for the commit the repository holds (agent-secrets exited $p3_app_status; it answered $(answered status "$p3_app_output"))" \
	is 201 "$(answered status "$p3_app_output")"
judge "p3 the same token is answered 422 for a commit the repository does not hold (it answered $(answered status-other "$p3_app_output"))" \
	is 422 "$(answered status-other "$p3_app_output")"

# --- p4: with BROKER_GITHUB_APP_FIELD=off, a body carrying github_app is refused with its own code

# post_github_app sends one body carrying github_app to a route with curl, signed by the session,
# leaving the answer's status in P4_STATUS and its code and message in P4_CODE and P4_MESSAGE.
post_github_app() { # <url>
	local proof
	proof="$("$AGENT_SECRETS_BIN" sign --method POST --url "$1")" || unknown "the session could not sign a call"
	P4_STATUS="$(curl -sS -o "$WORK_DIR/p4.json" -w '%{http_code}' -X POST -H "Proof: $proof" -H 'Content-Type: application/json' \
		-d "{\"github_app\": {\"owner\": \"$OWNER\", \"read_only\": true}}" "$1")"
	P4_CODE="$(jq -r '.code // ""' "$WORK_DIR/p4.json" 2>/dev/null || true)"
	P4_MESSAGE="$(jq -r '.error // ""' "$WORK_DIR/p4.json" 2>/dev/null || true)"
}
P4_REMOVED="github_app was removed: name a profile and narrow it with --target ACCOUNT[/REPOSITORY]"
post_github_app "$AGENT_SECRETS_URL/v1/requests"
judge "p4 POST /v1/requests with a github_app body is refused 400 GITHUB_APP_FIELD_REMOVED (it answered $P4_STATUS ${P4_CODE:-with no code})" \
	both is 400 "$P4_STATUS" is GITHUB_APP_FIELD_REMOVED "$P4_CODE"
judge "p4 that refusal's message says what replaces the field (it said: ${P4_MESSAGE:-nothing})" \
	is "$P4_REMOVED" "$P4_MESSAGE"
post_github_app "$AGENT_SECRETS_URL/v1/grants/${P4_GRANT_ID:-none}/values"
judge "p4 POST /v1/grants/{id}/values with a github_app body is refused 400 GITHUB_APP_FIELD_REMOVED (grant ${P4_GRANT_ID:-none}; it answered $P4_STATUS ${P4_CODE:-with no code})" \
	both is 400 "$P4_STATUS" is GITHUB_APP_FIELD_REMOVED "$P4_CODE"

if [ "$PASSED" -eq "$TOTAL" ]; then
	printf 'all %d assertions passed\n' "$TOTAL"
	exit 0
fi
printf '%d of %d assertions failed\n' "$((TOTAL - PASSED))" "$TOTAL"
exit 1
