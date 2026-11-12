#!/usr/bin/env bash
# packages/envoy/scripts/github-narrowing-check.sh
#
# Runs the four checks of agent-secrets' GitHub App narrowing that need a broker, a GitHub and a
# session - k1, k2 and k4 of dispatch://INFRA-2687's verification document, and k1 of
# dispatch://INFRA-3099's - end to end on a local stack this script starts, and says whether each
# one passed.
#
# It starts scripts/dev-broker.sh, which holds the three github-app secrets and the stand-in
# GitHub those secrets are minted against, enrols one pod session of the registered service that
# owns two of them, as the Legion daemon enrols a worker pod, and then runs every command of the
# four checks against that stack. It prints one PASS or FAIL line for each property a check
# names, and a last line `all <n> assertions passed` when every one passed, so a reader tells a
# complete run from a cut-off one. It exits 0 only then, 1 when a property failed, and 2 on a
# usage error.
#
# Every line names the exit status of the command it read, because a property that failed and a
# command that never ran look alike in a transcript that holds neither.
#
# What each check drives:
#
#   - k1: the narrowed command with --github-token reads the repository the narrowing named and
#     is refused on another repository of that account and on a repository of another account;
#     the stand-in records that the narrowed mint asked for that one repository; and the same
#     command without the flag puts the JSON object of installations in the child, which the
#     stand-in refuses.
#   - k2: the stand-in refuses the narrowed mint (HTTP 422), and then answers wider than the
#     narrowing asked; each time the command does not run and agent-secrets exits non-zero
#     naming the secret and the refusal. Its negative control is the same command with the
#     stand-in answering ordinarily again.
#   - k4: under the secret whose release holds two installations, --github-token with no
#     narrowing and --github-token narrowed to the account that still leaves two are each
#     refused, with no child run and, for the first, no token request at the stand-in at all.
#     Its negative control is the same two runs without the flag.
#   - k1 of INFRA-3099: under the one github-app secret whose request reaches a person, a
#     read-only narrowing to a repository the stand-in refuses is refused before the approval
#     request is opened. The command does not run, agent-secrets exits 1 naming the secret, that
#     repository and the sentence the refusal carries, its stderr names no page to approve
#     anything on, and the broker's own credential_requests holds no record it did not hold
#     before. The stand-in's records hold the search for the cause - a token request that left
#     the repositories out, and one that named that repository alone - and every token it minted
#     for the run was given back, read both as the number of revokes it was sent and as a read
#     with that token being refused afterwards. Its negative control is the same request for a
#     repository the stand-in reaches, with a wait of its own, approved on the broker's page as
#     the person does: the command runs.
#
# The stand-in's answer to a token request is set while it runs, at the answer route the stack's
# banner names (POST {"mode": "ordinary"|"refuse"|"refuse-repository"|"refuse-permissions"|
# "widen", "repository": "<name>"}, the repository being the one refuse-repository reads), since
# k2 and INFRA-3099's k1 drive four answers against one stack.
#
# The curl calls the child makes carry no --cacert and no --resolve, which the check's own text
# would have them carry for a stand-in behind TLS. The GitHub stand of cmd/dev-airlock-stands is
# served over plain http on 127.0.0.1 (its plainStands): the broker mints a kind's value with an
# HTTP client that trusts this host's own roots and nothing else, so a minter endpoint behind TLS
# would want a root in the pod's trust store. There is therefore no certificate to carry a name
# and nothing for a child to verify, and each call names the stand's own address.
#
# No line it prints is a credential. A released value never reaches the output: the child reads
# its token out of its own environment and prints the repository it asked for and the status it
# got, and a refusal is printed as agent-secrets wrote it, which names no part of any token.
#
# Whether a command ran is read from the child's standard output alone, apart from
# agent-secrets' own standard error, and the test is for a line that is exactly `ran`, the word
# the child prints. agent-secrets writes `the grant from ...` and `was granted ...` on reachable
# paths of these same runs, and each of those holds the letters of `ran`, so a test for `ran`
# anywhere in both streams together would call a correct run a failure.
#
# approve_record and approve_watch are copied from scripts/airlock-check.sh in the shape they
# have there: the approval a person makes on the broker's own page, which INFRA-3099's control
# needs and which nothing else here does. They are copied rather than shared because each check
# script of this directory stands alone.
#
# One property is about what the broker itself wrote, so this script reads the broker's own
# database: dev-broker.sh makes a database of its own for the run and logs the name it gave it,
# and this script puts that name in place of the path of DEV_BROKER_POSTGRES_URL as the caller
# gave it, so no password is ever parsed out of a banner. That makes DEV_BROKER_POSTGRES_URL
# necessary here rather than optional: a run without it exits 2 and starts nothing.
#
# Usage: DEV_BROKER_POSTGRES_URL=postgres://<user>@<host>:<port>/<database> \
#          packages/envoy/scripts/github-narrowing-check.sh [--keep-stack]
#        --keep-stack leaves the stack and the work directory up for a person to look at a
#        failure with. --help prints this usage.
#
# Needs bash, curl, jq, go and psql. DEV_BROKER_POSTGRES_URL is required: it is passed to
# dev-broker.sh as it is, so a pod with no container runtime names its test server there, and
# this script reads the broker's own database of that same server through psql.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
	cat <<'EOF'
usage: DEV_BROKER_POSTGRES_URL=postgres://<user>@<host>:<port>/<database> \
         github-narrowing-check.sh [--keep-stack]

Runs checks k1, k2 and k4 of dispatch://INFRA-2687 and k1 of dispatch://INFRA-3099 on a local
stack of its own and prints one PASS or FAIL line for each property they name.

  --keep-stack   leave the stack and this run's work directory up, to look at a failure with
  --help         print this

DEV_BROKER_POSTGRES_URL is required: the stack is driven against the server it names, and one
property is read from the broker's own database on that server.

It exits 0 only when every property passed, 1 when one did not, and 2 on a usage error.
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
		echo "github-narrowing-check: unexpected argument $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

# The server the stack is driven against, and the one whose database the pending-record property
# is read from. It is read before anything is created or started, so a run that cannot make that
# reading leaves nothing behind and costs nobody the minutes a stack takes.
if [ -z "${DEV_BROKER_POSTGRES_URL:-}" ]; then
	echo "github-narrowing-check: DEV_BROKER_POSTGRES_URL is unset; it is the Postgres server this check drives dev-broker.sh against, and the one whose database this check reads the broker's own credential_requests from" >&2
	usage >&2
	exit 2
fi

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/github-narrowing-check.XXXXXX")"
STACK_LOG="$WORK_DIR/stack.log"
KEY_DIR="$WORK_DIR/session-key"
READ_REPOSITORIES="$WORK_DIR/read-repositories.sh"
STACK_PID=""
# The approver sitting at the broker's page while the control run waits (approve_watch), for as
# long as that one run takes.
APPROVER_PID=""

cleanup() {
	# The approver loop first, and under --keep-stack as well: it is this script's own sitting
	# at the broker's page, and a stack left up for a person to drive must not have one of
	# those still approving every request behind them.
	if [ -n "$APPROVER_PID" ] && kill -0 "$APPROVER_PID" 2>/dev/null; then
		kill "$APPROVER_PID" 2>/dev/null || true
		wait "$APPROVER_PID" 2>/dev/null || true
	fi
	if [ "$KEEP_STACK" = true ] && [ -n "$STACK_PID" ]; then
		echo "github-narrowing-check: the stack is still up (dev-broker.sh pid $STACK_PID, log $STACK_LOG);" >&2
		echo "github-narrowing-check: stop it with kill $STACK_PID, which drops its database" >&2
		return
	fi
	# SIGTERM, never SIGKILL: dev-broker.sh's own EXIT trap is what stops its processes and
	# drops the database it made.
	if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
		kill "$STACK_PID" 2>/dev/null || true
		wait "$STACK_PID" 2>/dev/null || true
	fi
	if [ "$KEEP_STACK" = true ]; then
		echo "github-narrowing-check: work directory kept at $WORK_DIR" >&2
	else
		rm -rf "$WORK_DIR"
	fi
}
trap cleanup EXIT

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
# without the text reaching the output. is compares two strings. lacks is holds' negative, and
# counted reports a count of at least one. printed_ran reports whether the child printed its
# word, read from the child's standard output alone (see the header). ran reports a command that
# exited as it should and printed that word.
holds() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }
is() { [ "$1" = "$2" ]; }
lacks() { ! holds "$1" "$2"; }
# counted judges a property that is about what a run asked GitHub for rather than how many times
# it asked: the mint of a refused read-only narrowing asks once more without actions (secrets'
# optionalReadOnlyPermission), so one ask of the search below is one record at the stand-in or
# two.
counted() { # <how many>
	[ "${1:-0}" -gt 0 ]
}
printed_ran() { # <the child's standard output>
	printf '%s\n' "$1" | grep -qx ran
}
ran() { # <want status> <got status> <the child's standard output>
	is "$1" "$2" && printed_ran "$3"
}
refused() { # <want status> <got status> <the child's standard output>
	is "$1" "$2" && ! printed_ran "$3"
}
failed_without_running() { # <got status> <the child's standard output>
	! is 0 "$1" && ! printed_ran "$2"
}

# answered is the status one line of the child's output carries for a repository, and "nothing"
# for a repository the child never reached, so each line below names what it read.
answered() { # <repository> <the child's output>
	local status
	status="$(printf '%s' "$2" | sed -n "s#^$1 ##p" | head -n 1 || true)"
	printf '%s' "${status:-nothing}"
}

# await waits for a line a process logs, and ends the run the moment that process dies: a
# stack that never came up proves nothing, and each property below would fail for a reason
# that is not the property's. It is the helper airlock-check.sh and judge-check.sh each carry,
# in the same shape.
await() { # <what> <seconds> <log> <pattern> <pid>
	local i
	for ((i = 0; i < $2 * 2; i++)); do
		if grep -qE "$4" "$3" 2>/dev/null; then return 0; fi
		if ! kill -0 "$5" 2>/dev/null; then
			echo "github-narrowing-check: $1 exited before it was ready; see $3" >&2
			tail -n 20 "$3" >&2 || true
			exit 1
		fi
		sleep 0.5
	done
	echo "github-narrowing-check: $1 was not ready within $2 seconds; see $3" >&2
	exit 1
}

# --- The stack -----------------------------------------------------------------------------------

echo "github-narrowing-check: starting the stack (dev-broker.sh)..." >&2
: >"$STACK_LOG"
bash "$SCRIPT_DIR/dev-broker.sh" >"$STACK_LOG" 2>&1 &
STACK_PID=$!
await "the stack" 300 "$STACK_LOG" '^dev-broker: ready\.$' "$STACK_PID"
# Everything the banner exports, which is where this check's addresses, secret names and the pod
# and service it enrols come from.
eval "$(sed -n 's/^  \(export [A-Z][A-Z_0-9]*=.*\)$/\1/p' "$STACK_LOG")"
AGENT_SECRETS_BIN="$DEV_BROKER_DIR/bin/agent-secrets"
STANDS_BIN="$DEV_BROKER_DIR/bin/dev-airlock-stands"
# The database dev-broker.sh made for this run, which the one property about what the broker
# itself wrote is read from. The name is the one dev-broker.sh logged as it created it, and the
# server, the role and any password are DEV_BROKER_POSTGRES_URL's own, left exactly as the
# caller gave them: the banner stars a password out, so a URL built from the banner could not
# connect, and one parsed back out of it would put that password through this script.
BROKER_DB_NAME="$(sed -n 's/^dev-broker: created isolated database \(.*\)$/\1/p' "$STACK_LOG" | head -n 1)"
if [ -z "$BROKER_DB_NAME" ] || ! [[ "$DEV_BROKER_POSTGRES_URL" =~ ^(postgres(ql)?://[^/?]+)/[^/?]*(\?.*)?$ ]]; then
	echo "github-narrowing-check: the stack named no database of its own (dev-broker.sh logged ${BROKER_DB_NAME:-none}), or DEV_BROKER_POSTGRES_URL is not postgres://<user>@<host>:<port>/<database>" >&2
	exit 1
fi
BROKER_DB_URL="${BASH_REMATCH[1]}/${BROKER_DB_NAME}${BASH_REMATCH[3]}"
echo "github-narrowing-check: the broker's own database for this run is $BROKER_DB_NAME" >&2
echo "github-narrowing-check: the stack is up (broker $AGENT_SECRETS_URL, GitHub stand $DEV_BROKER_GITHUB_URL)" >&2

# --- The session: a pod of the service that owns the two github-app secrets, enrolled as the
# Legion daemon enrols a worker pod, so each request for one is granted without asking. The pod
# and the service are the stack's own exports, the ones its banner's enrolment command names, so
# this check enrols the session that command describes rather than one of its own invention. ---
mkdir -p "$KEY_DIR"
THUMBPRINT="$("$AGENT_SECRETS_BIN" keygen --out "$KEY_DIR")"
"$STANDS_BIN" enroll-pod \
	--url "$AGENT_SECRETS_URL" \
	--service "$DEV_BROKER_SERVICE" \
	--token-file "$DEV_BROKER_DIR/registrar-token" \
	--pod-token-file "$DEV_BROKER_DIR/pod-token" \
	--pod-uid "$DEV_BROKER_POD_UID" \
	--thumbprint "$THUMBPRINT" \
	>"$WORK_DIR/enrollment.json"
ENROLLMENT_ID="$(jq -r .enrollment_id "$WORK_DIR/enrollment.json")"
if [ -z "$ENROLLMENT_ID" ] || [ "$ENROLLMENT_ID" = null ]; then
	echo "github-narrowing-check: the enrolment answered no enrollment id" >&2
	exit 1
fi
printf '%s' "$ENROLLMENT_ID" >"$KEY_DIR/enrollment"
export AGENT_SECRETS_KEY_DIR="$KEY_DIR"
echo "github-narrowing-check: enrolled pod session $ENROLLMENT_ID (pod $DEV_BROKER_POD_UID, service $DEV_BROKER_SERVICE)" >&2

# --- The child the checks run ---------------------------------------------------------------
#
# It reads the token out of the variable whose name it is given - the released value never
# reaches a command line - and prints one line per repository: the repository and the status
# GitHub answered it, and no part of the answer's body.
cat >"$READ_REPOSITORIES" <<'EOF'
#!/usr/bin/env bash
set -u
name="$1"
base="$2"
shift 2
for target in "$@"; do
	printf '%s %s\n' "$target" "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer ${!name}" "$base/repos/$target")"
done
EOF
chmod +x "$READ_REPOSITORIES"

# run_agent_secrets runs one agent-secrets command of this check. It puts its exit status in
# STATUS, everything it wrote in OUTPUT - its own stderr and the child's output together, as a
# person at a terminal reads them - the child's standard output on its own in CHILD_OUT, which
# is what every "no command ran" property is judged by, and agent-secrets' own standard error on
# its own in ERR_OUT, which is what the property that a refusal names no page to approve
# anything on is judged by: a pending request prints that page there and nowhere else.
STATUS=0
OUTPUT=""
CHILD_OUT=""
ERR_OUT=""
run_agent_secrets() {
	STATUS=0
	"$AGENT_SECRETS_BIN" "$@" >"$WORK_DIR/stdout" 2>"$WORK_DIR/stderr" || STATUS=$?
	CHILD_OUT="$(cat "$WORK_DIR/stdout")"
	ERR_OUT="$(cat "$WORK_DIR/stderr")"
	OUTPUT="$(cat "$WORK_DIR/stdout" "$WORK_DIR/stderr")"
}

# token_requests is how many installation token requests the stand-in has been sent so far, and
# narrowed_body the body of the one at that index, which is what a narrowed mint asked GitHub
# for. A stand that was sent nothing answers null rather than an empty array, so each reads its
# answer as a list of its own. The records carry the App JWT in their headers, so nothing here
# reads a record whole.
token_requests() {
	curl -sS "$DEV_BROKER_GITHUB_RECORDS_URL" | jq '[(. // [])[] | select(.path | endswith("/access_tokens"))] | length'
}
narrowed_body() { # <index, from 0>
	curl -sS "$DEV_BROKER_GITHUB_RECORDS_URL" | jq -r --argjson at "$1" '[(. // [])[] | select(.path | endswith("/access_tokens"))] | .[$at].body // ""'
}

# revokes is how many token revokes the stand-in has been sent so far (DELETE
# /installation/token, the minter giving back a token it minted and will not release),
# minted_tokens how many of the token requests from one index on it answered with a token, and
# asked_repositories how many of those asked for exactly the repositories named. The last two
# read the body each request sent and nothing else of its record.
#
# Which requests were answered with a token is read from those bodies and the answer in force,
# a record holding the request alone: under refuse-repository every token request but the ones
# whose repositories name the refused repository is minted.
revokes() {
	curl -sS "$DEV_BROKER_GITHUB_RECORDS_URL" | jq '[(. // [])[] | select(.method == "DELETE" and .path == "/installation/token")] | length'
}
minted_tokens() { # <index, from 0> <the repository the stand-in is refusing>
	curl -sS "$DEV_BROKER_GITHUB_RECORDS_URL" |
		jq --argjson at "$1" --arg refused "$2" '[[(. // [])[] | select(.path | endswith("/access_tokens"))][$at:][] | (.body | fromjson? // {} | .repositories // [])] | map(select(([.[] | ascii_downcase] | index($refused | ascii_downcase)) == null)) | length'
}
asked_repositories() { # <index, from 0> <the repositories asked for, as a JSON array>
	curl -sS "$DEV_BROKER_GITHUB_RECORDS_URL" |
		jq --argjson at "$1" --argjson want "$2" '[[(. // [])[] | select(.path | endswith("/access_tokens"))][$at:][] | (.body | fromjson? // {} | .repositories // [])] | map(select(. == $want)) | length'
}

# standin_read is the status the stand-in answers a repository read with one token, and no part
# of the answer's body. It is the one read this check makes with a token of its own rather than
# through a child, and it names a token the stand-in builds from an owner (GitHubStand.token),
# never one this stack released.
standin_read() { # <token> <owner/repository>
	curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" "$DEV_BROKER_GITHUB_URL/repos/$2"
}

# pending_records is how many credential-request records the broker holds. That is the record a
# request a person must decide is written as, and a request refused before anybody is asked
# writes none, so the count before a run and after it is how that is read. The broker serves no
# count of its own, so it comes from the broker's own database (BROKER_DB_URL).
pending_records() {
	psql "$BROKER_DB_URL" -v ON_ERROR_STOP=1 -Atqc 'select count(*) from credential_requests'
}

# answers sets what the stand-in answers every token request from then on: ordinary, refuse,
# refuse-repository, refuse-permissions or widen, with the repository the answer refuses where
# the mode reads one (refuse-repository alone does). A route that refused the mode, or a
# refuse-repository naming no repository, leaves the last answer in place, so the run ends here
# rather than reading the next check against an answer it did not set.
answers() { # <mode> [repository]
	local reply body
	body="$(jq -nc --arg mode "$1" --arg repository "${2:-}" '{mode: $mode} + (if $repository == "" then {} else {repository: $repository} end)')"
	if ! reply="$(curl -sS -f -X POST "$DEV_BROKER_GITHUB_ANSWER_URL" -H 'Content-Type: application/json' -d "$body")"; then
		echo "github-narrowing-check: the stand-in refused the answer $1${2:+ $2}" >&2
		exit 1
	fi
	echo "github-narrowing-check: the GitHub stand now answers $reply" >&2
}

# --- The approval a person makes --------------------------------------------------------------
#
# approve_record and approve_watch are copied from scripts/airlock-check.sh in the shape they
# have there, and each check script of this directory stands alone, which is why they are copied
# rather than shared. They need the broker's URL and this run's work directory, both of which
# this script has. judge-check.sh's sign_in is not used: it needs an issuer URL dev-broker.sh
# does not export, and these two need none.

# approve_record approves one pending request on the broker's own page, as the person does: the
# page sends a browser to the stack's issuer, which signs the approver in with no form to fill,
# and the decision is posted back with an Origin header, which the page requires. curl's own
# cookie jar cannot be used - both cookies are Secure and this broker is on http - so each
# Set-Cookie is read out of the headers and sent back by hand.
approve_record() { # <record id>
	local rec="$1" d="$WORK_DIR/approve"
	mkdir -p "$d"
	curl -sS -D "$d/start" -o /dev/null "$AGENT_SECRETS_URL/approve/$rec"
	local authorize signin callback session
	authorize="$(sed -n 's/^Location: //p' "$d/start" | tr -d '\r')"
	signin="$(sed -n 's/^Set-Cookie: \(__Host-broker-signin=[^;]*\).*/\1/p' "$d/start" | tr -d '\r')"
	[ -n "$authorize" ] || return 1
	curl -sS -D "$d/authorize" -o /dev/null "$authorize"
	callback="$(sed -n 's/^Location: //p' "$d/authorize" | tr -d '\r')"
	curl -sS -D "$d/callback" -o /dev/null -H "Cookie: $signin" "$callback"
	session="$(sed -n 's/^Set-Cookie: \(__Host-broker-session=[^;]*\).*/\1/p' "$d/callback" | tr -d '\r')"
	[ -n "$session" ] || return 1
	curl -sS -H "Cookie: $session" -o "$d/page.html" "$AGENT_SECRETS_URL/approve/$rec"
	local field
	field() { sed -n "s/.*name=\"$1\" value=\"\([^\"]*\)\".*/\1/p" "$d/page.html" | head -n 1; }
	curl -sS -o "$d/decided.html" -w '%{http_code}' -H "Cookie: $session" -H "Origin: $AGENT_SECRETS_URL" \
		--data-urlencode "decision=approve" --data-urlencode "record=$(field record)" \
		--data-urlencode "printed=$(field printed)" --data-urlencode "token=$(field token)" \
		--data-urlencode "expires=$(field expires)" "$AGENT_SECRETS_URL/approve" >>"$d/decisions"
	echo >>"$d/decisions"
}

# approve_watch is the approver sitting at the page while a command waits: it reads the record
# ids out of the CLI's own stderr, where each pending request prints its page, and approves each
# one once. A pod session has no operator and the broker takes this decision on its page alone,
# so this is the only way a human-tier name is released to a check.
approve_watch() { # <the file the CLI's stderr goes to>
	local log="$1" seen=" " rec
	while :; do
		for rec in $(grep -o '/approve/[0-9a-f]\{64\}' "$log" 2>/dev/null | sed 's|/approve/||' | sort -u); do
			case "$seen" in *" $rec "*) continue ;; esac
			seen="$seen$rec "
			approve_record "$rec" >/dev/null 2>&1 || true
		done
		sleep 1
	done
}

# --- k1: a narrowed token reaches the repository the narrowing named and nothing else -------------

NAMED_REPOSITORY="proofs"
UNNAMED_REPOSITORY="widgets"
OTHER_OWNERS_REPOSITORY="$DEV_BROKER_GITHUB_CASE_OWNER/$NAMED_REPOSITORY"

k1_mark="$(token_requests)"
run_agent_secrets "$DEV_BROKER_GITHUB_SECRET" \
	--github-owner "$DEV_BROKER_GITHUB_OWNER" --github-repo "$NAMED_REPOSITORY" --github-token \
	-- bash "$READ_REPOSITORIES" "$DEV_BROKER_GITHUB_SECRET" "$DEV_BROKER_GITHUB_URL" \
	"$DEV_BROKER_GITHUB_OWNER/$NAMED_REPOSITORY" \
	"$DEV_BROKER_GITHUB_OWNER/$UNNAMED_REPOSITORY" \
	"$OTHER_OWNERS_REPOSITORY"
k1_status="$STATUS"
k1_output="$OUTPUT"
judge "k1 the narrowed token reads the repository the narrowing named (agent-secrets exited $k1_status; $DEV_BROKER_GITHUB_OWNER/$NAMED_REPOSITORY answered $(answered "$DEV_BROKER_GITHUB_OWNER/$NAMED_REPOSITORY" "$k1_output"))" \
	holds "$DEV_BROKER_GITHUB_OWNER/$NAMED_REPOSITORY 200" "$k1_output"
judge "k1 that token is refused on another repository of the same account (agent-secrets exited $k1_status; $DEV_BROKER_GITHUB_OWNER/$UNNAMED_REPOSITORY answered $(answered "$DEV_BROKER_GITHUB_OWNER/$UNNAMED_REPOSITORY" "$k1_output"))" \
	holds "$DEV_BROKER_GITHUB_OWNER/$UNNAMED_REPOSITORY 404" "$k1_output"
judge "k1 that token is refused on a repository of another account (agent-secrets exited $k1_status; $OTHER_OWNERS_REPOSITORY answered $(answered "$OTHER_OWNERS_REPOSITORY" "$k1_output"))" \
	holds "$OTHER_OWNERS_REPOSITORY 404" "$k1_output"
k1_body="$(narrowed_body "$k1_mark")"
judge "k1 the stand-in records that the narrowed mint asked for $NAMED_REPOSITORY alone (its body was ${k1_body:-none})" \
	is "{\"repositories\":[\"$NAMED_REPOSITORY\"]}" "$k1_body"

run_agent_secrets "$DEV_BROKER_GITHUB_SECRET" \
	--github-owner "$DEV_BROKER_GITHUB_OWNER" --github-repo "$NAMED_REPOSITORY" \
	-- bash "$READ_REPOSITORIES" "$DEV_BROKER_GITHUB_SECRET" "$DEV_BROKER_GITHUB_URL" \
	"$DEV_BROKER_GITHUB_OWNER/$NAMED_REPOSITORY"
judge "k1 control: without --github-token the child holds the JSON object, which the stand-in refuses (agent-secrets exited $STATUS; $DEV_BROKER_GITHUB_OWNER/$NAMED_REPOSITORY answered $(answered "$DEV_BROKER_GITHUB_OWNER/$NAMED_REPOSITORY" "$OUTPUT"))" \
	holds "$DEV_BROKER_GITHUB_OWNER/$NAMED_REPOSITORY 401" "$OUTPUT"

# --- k2: a mint the stand-in refuses, and one it answers wider than asked, run nothing ------------

for mode in refuse widen; do
	# What each refusal is called where agent-secrets prints it: the 422 is the caller's
	# narrowing, which the broker answers NARROWING_REFUSED, and a token wider than asked is
	# the upstream's, which it answers MINT_FAILED.
	refusal="NARROWING_REFUSED"
	if [ "$mode" = widen ]; then
		refusal="MINT_FAILED"
	fi
	answers "$mode"
	run_agent_secrets "$DEV_BROKER_GITHUB_SECRET" \
		--github-owner "$DEV_BROKER_GITHUB_OWNER" --github-repo "$NAMED_REPOSITORY" --github-token \
		-- sh -c 'echo ran'
	judge "k2 the $mode answer makes agent-secrets exit non-zero and run no command (it exited $STATUS and printed no ran)" \
		failed_without_running "$STATUS" "$CHILD_OUT"
	judge "k2 the $mode answer's refusal names the secret (agent-secrets exited $STATUS and said: $OUTPUT)" \
		holds "$DEV_BROKER_GITHUB_SECRET" "$OUTPUT"
	judge "k2 the $mode answer's refusal names it ($refusal; agent-secrets exited $STATUS)" \
		holds "$refusal" "$OUTPUT"
done

answers ordinary
run_agent_secrets "$DEV_BROKER_GITHUB_SECRET" \
	--github-owner "$DEV_BROKER_GITHUB_OWNER" --github-repo "$NAMED_REPOSITORY" --github-token \
	-- sh -c 'echo ran'
judge "k2 control: with the ordinary answer the command runs and gets a value (agent-secrets exited $STATUS)" \
	ran 0 "$STATUS" "$CHILD_OUT"

# --- k4: a release of two installations is no one token to hand over ------------------------------

k4_mark="$(token_requests)"
run_agent_secrets "$DEV_BROKER_GITHUB_CASE_SECRET" --github-token -- sh -c 'echo ran'
judge "k4 --github-token with no narrowing exits 2 and runs nothing (it exited $STATUS and said: $OUTPUT)" \
	refused 2 "$STATUS" "$CHILD_OUT"
k4_asked="$(($(token_requests) - k4_mark))"
judge "k4 that run asked the stand-in for no token at all (it sent $k4_asked token requests)" \
	is 0 "$k4_asked"

run_agent_secrets "$DEV_BROKER_GITHUB_CASE_SECRET" --github-owner "$DEV_BROKER_GITHUB_CASE_OWNER" --github-token -- sh -c 'echo ran'
k4_narrowed_status="$STATUS"
k4_narrowed_output="$OUTPUT"
k4_narrowed_child="$CHILD_OUT"
judge "k4 --github-token narrowed to $DEV_BROKER_GITHUB_CASE_OWNER exits 1 and runs nothing (it exited $k4_narrowed_status and said: $k4_narrowed_output)" \
	refused 1 "$k4_narrowed_status" "$k4_narrowed_child"
judge "k4 that refusal names the secret (agent-secrets exited $k4_narrowed_status)" \
	holds "$DEV_BROKER_GITHUB_CASE_SECRET" "$k4_narrowed_output"
judge "k4 that refusal names --github-token (agent-secrets exited $k4_narrowed_status)" \
	holds "--github-token" "$k4_narrowed_output"

run_agent_secrets "$DEV_BROKER_GITHUB_CASE_SECRET" -- sh -c 'echo ran'
judge "k4 control: the same run without the flag prints ran and exits 0 (it exited $STATUS)" \
	ran 0 "$STATUS" "$CHILD_OUT"
run_agent_secrets "$DEV_BROKER_GITHUB_CASE_SECRET" --github-owner "$DEV_BROKER_GITHUB_CASE_OWNER" -- sh -c 'echo ran'
judge "k4 control: the narrowed run without the flag prints ran and exits 0 (it exited $STATUS)" \
	ran 0 "$STATUS" "$CHILD_OUT"

# --- k1 of INFRA-3099: a narrowing the broker cannot serve is refused before anybody is asked ---
#
# Driven under the one github-app secret of this stack whose request reaches a person, with the
# stand-in refusing every token request whose repositories name REFUSED_REPOSITORY and minting
# every other. --wait 0s is what makes a refusal the only thing this run can be: a request put
# to a person ends at once under it, with exit 75 and no child run, and the two look alike in a
# child's output alone. So the properties below read the refusal's own words, the page it does
# not name and the record the broker did not write.

REFUSED_REPOSITORY="missing"

answers refuse-repository "$REFUSED_REPOSITORY"
n3099_records="$(pending_records)"
n3099_mark="$(token_requests)"
n3099_revokes="$(revokes)"
run_agent_secrets "$DEV_BROKER_GITHUB_HUMAN_SECRET" --reason 'INFRA-3099 k1' --wait 0s \
	--github-owner "$DEV_BROKER_GITHUB_OWNER" --github-repo "$REFUSED_REPOSITORY" --github-read-only \
	-- sh -c 'echo ran'
n3099_status="$STATUS"
n3099_output="$OUTPUT"
n3099_err="$ERR_OUT"
n3099_child="$CHILD_OUT"
judge "INFRA-3099 k1 the narrowing to $REFUSED_REPOSITORY exits 1 and runs nothing (it exited $n3099_status and said: $n3099_output)" \
	refused 1 "$n3099_status" "$n3099_child"
judge "INFRA-3099 k1 that refusal names the secret (agent-secrets exited $n3099_status)" \
	holds "$DEV_BROKER_GITHUB_HUMAN_SECRET" "$n3099_output"
judge "INFRA-3099 k1 that refusal names the repository $REFUSED_REPOSITORY (agent-secrets exited $n3099_status)" \
	holds "$REFUSED_REPOSITORY" "$n3099_output"
judge "INFRA-3099 k1 that refusal says the narrowing cannot be served (agent-secrets exited $n3099_status)" \
	holds "the narrowing cannot be served" "$n3099_output"
judge "INFRA-3099 k1 that refusal names no page to approve anything on, as a pending request would (agent-secrets exited $n3099_status)" \
	lacks "/approve/" "$n3099_err"
n3099_records_after="$(pending_records)"
judge "INFRA-3099 k1 the broker wrote no credential-request record for it (it held $n3099_records before the run and $n3099_records_after after it)" \
	is "$n3099_records" "$n3099_records_after"
n3099_without_repositories="$(asked_repositories "$n3099_mark" '[]')"
judge "INFRA-3099 k1 the search for the cause asked GitHub again with the repositories left out ($n3099_without_repositories of its token requests named none)" \
	counted "$n3099_without_repositories"
n3099_one_repository="$(asked_repositories "$n3099_mark" "[\"$REFUSED_REPOSITORY\"]")"
judge "INFRA-3099 k1 the search asked GitHub for $REFUSED_REPOSITORY alone ($n3099_one_repository of its token requests named it alone)" \
	counted "$n3099_one_repository"
n3099_minted="$(minted_tokens "$n3099_mark" "$REFUSED_REPOSITORY")"
n3099_given_back="$(($(revokes) - n3099_revokes))"
judge "INFRA-3099 k1 every token the stand-in minted for that run was given back (it minted $n3099_minted and was sent $n3099_given_back revokes)" \
	is "$n3099_minted" "$n3099_given_back"
n3099_read="$(standin_read "ghs_standin_$DEV_BROKER_GITHUB_OWNER" "$DEV_BROKER_GITHUB_OWNER/$NAMED_REPOSITORY")"
judge "INFRA-3099 k1 the token the stand-in mints for $DEV_BROKER_GITHUB_OWNER with no repositories reads nothing afterwards, a stand-in forgetting a token it revoked (its read of $DEV_BROKER_GITHUB_OWNER/$NAMED_REPOSITORY answered $n3099_read)" \
	is 401 "$n3099_read"

# Its control: the same request for a repository the stand-in reaches, with that same answer
# still in place, so the repository the narrowing names is the only thing that differs between
# the two runs. It waits for a person rather than --wait 0s, which would end a pending request
# at once and run no child, and the person is approve_watch, approving each record agent-secrets
# names on its stderr as the approver does on the broker's page.
: >"$WORK_DIR/stderr"
approve_watch "$WORK_DIR/stderr" &
APPROVER_PID=$!
run_agent_secrets "$DEV_BROKER_GITHUB_HUMAN_SECRET" --reason 'INFRA-3099 k1 control' --wait 2m \
	--github-owner "$DEV_BROKER_GITHUB_OWNER" --github-repo "$NAMED_REPOSITORY" --github-read-only \
	-- sh -c 'echo ran'
kill "$APPROVER_PID" 2>/dev/null || true
wait "$APPROVER_PID" 2>/dev/null || true
APPROVER_PID=""
judge "INFRA-3099 k1 control: the same request for $NAMED_REPOSITORY is put to its approver, approved on the broker's page, and the command runs (agent-secrets exited $STATUS and said: $OUTPUT)" \
	ran 0 "$STATUS" "$CHILD_OUT"

if [ "$PASSED" -eq "$TOTAL" ]; then
	printf 'all %d assertions passed\n' "$TOTAL"
	exit 0
fi
printf '%d of %d assertions failed\n' "$((TOTAL - PASSED))" "$TOTAL"
exit 1
