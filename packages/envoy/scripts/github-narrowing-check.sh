#!/usr/bin/env bash
# packages/envoy/scripts/github-narrowing-check.sh
#
# Runs the three checks of agent-secrets' --github-token that need a broker, a GitHub and a
# session - k1, k2 and k4 of dispatch://INFRA-2687's verification document - end to end on a
# local stack this script starts, and says whether each one passed.
#
# It starts scripts/dev-broker.sh, which holds the two github-app secrets and the stand-in GitHub
# those secrets are minted against, enrols one pod session of the registered service that owns
# them, as the Legion daemon enrols a worker pod, and then runs every command of the three checks
# against that stack. It prints one PASS or FAIL line for each property a check names, and a last
# line `all <n> assertions passed` when every one passed, so a reader tells a complete run from a
# cut-off one. It exits 0 only then, 1 when a property failed, and 2 on a usage error.
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
#
# The stand-in's answer to a token request is set while it runs, at the answer route the stack's
# banner names (POST {"mode": "ordinary"|"refuse"|"widen"}), since k2 drives three answers against
# one stack.
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
# Usage: packages/envoy/scripts/github-narrowing-check.sh [--keep-stack]
#        --keep-stack leaves the stack and the work directory up for a person to look at a
#        failure with. --help prints this usage.
#
# Needs bash, curl, jq, go and psql. DEV_BROKER_POSTGRES_URL is passed to dev-broker.sh as it is,
# so a pod with no container runtime names its test server there.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
	cat <<'EOF'
usage: github-narrowing-check.sh [--keep-stack]

Runs checks k1, k2 and k4 of dispatch://INFRA-2687 on a local stack of its own and prints one
PASS or FAIL line for each property they name.

  --keep-stack   leave the stack and this run's work directory up, to look at a failure with
  --help         print this

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

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/github-narrowing-check.XXXXXX")"
STACK_LOG="$WORK_DIR/stack.log"
KEY_DIR="$WORK_DIR/session-key"
READ_REPOSITORIES="$WORK_DIR/read-repositories.sh"
STACK_PID=""

cleanup() {
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
# without the text reaching the output. is compares two strings. printed_ran reports whether the
# child printed its word, read from the child's standard output alone (see the header). ran
# reports a command that exited as it should and printed that word.
holds() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }
is() { [ "$1" = "$2" ]; }
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

# --- The stack -----------------------------------------------------------------------------------

echo "github-narrowing-check: starting the stack (dev-broker.sh)..." >&2
: >"$STACK_LOG"
bash "$SCRIPT_DIR/dev-broker.sh" >"$STACK_LOG" 2>&1 &
STACK_PID=$!
ready=false
for _ in $(seq 1 600); do
	if grep -q '^dev-broker: ready\.$' "$STACK_LOG" 2>/dev/null; then
		ready=true
		break
	fi
	if ! kill -0 "$STACK_PID" 2>/dev/null; then
		echo "github-narrowing-check: the stack exited before it was ready; see $STACK_LOG" >&2
		tail -n 20 "$STACK_LOG" >&2 || true
		exit 1
	fi
	sleep 0.5
done
if [ "$ready" != true ]; then
	echo "github-narrowing-check: the stack was not ready within 300 seconds; see $STACK_LOG" >&2
	exit 1
fi
# Everything the banner exports, which is where this check's addresses and secret names come from.
eval "$(sed -n 's/^  \(export [A-Z][A-Z_0-9]*=.*\)$/\1/p' "$STACK_LOG")"
AGENT_SECRETS_BIN="$DEV_BROKER_DIR/bin/agent-secrets"
STANDS_BIN="$DEV_BROKER_DIR/bin/dev-airlock-stands"
# The pod and the service the banner's own enrolment command names, so this check enrols the
# session that command describes rather than one of its own invention.
POD_UID="$(sed -n 's/.*--pod-uid \([^ ]*\).*/\1/p' "$STACK_LOG" | head -n 1 || true)"
SERVICE="$(sed -n 's/^  registered service: *\([^ ]*\) .*/\1/p' "$STACK_LOG" | head -n 1 || true)"
if [ -z "$POD_UID" ] || [ -z "$SERVICE" ]; then
	echo "github-narrowing-check: the stack's banner names no pod uid or no registered service" >&2
	exit 1
fi
echo "github-narrowing-check: the stack is up (broker $AGENT_SECRETS_URL, GitHub stand $DEV_BROKER_GITHUB_URL)" >&2

# --- The session: a pod of the service that owns the two github-app secrets, enrolled as the
# Legion daemon enrols a worker pod, so each request for one is granted without asking. ---
mkdir -p "$KEY_DIR"
THUMBPRINT="$("$AGENT_SECRETS_BIN" keygen --out "$KEY_DIR")"
"$STANDS_BIN" enroll-pod \
	--url "$AGENT_SECRETS_URL" \
	--service "$SERVICE" \
	--token-file "$DEV_BROKER_DIR/registrar-token" \
	--pod-token-file "$DEV_BROKER_DIR/pod-token" \
	--pod-uid "$POD_UID" \
	--thumbprint "$THUMBPRINT" \
	>"$WORK_DIR/enrollment.json"
ENROLLMENT_ID="$(jq -r .enrollment_id "$WORK_DIR/enrollment.json")"
if [ -z "$ENROLLMENT_ID" ] || [ "$ENROLLMENT_ID" = null ]; then
	echo "github-narrowing-check: the enrolment answered no enrollment id" >&2
	exit 1
fi
printf '%s' "$ENROLLMENT_ID" >"$KEY_DIR/enrollment"
export AGENT_SECRETS_KEY_DIR="$KEY_DIR"
echo "github-narrowing-check: enrolled pod session $ENROLLMENT_ID (pod $POD_UID, service $SERVICE)" >&2

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
# person at a terminal reads them - and the child's standard output on its own in CHILD_OUT,
# which is what every "no command ran" property is judged by.
STATUS=0
OUTPUT=""
CHILD_OUT=""
run_agent_secrets() {
	STATUS=0
	"$AGENT_SECRETS_BIN" "$@" >"$WORK_DIR/stdout" 2>"$WORK_DIR/stderr" || STATUS=$?
	CHILD_OUT="$(cat "$WORK_DIR/stdout")"
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

# answers sets what the stand-in answers every token request from then on: ordinary, refuse or
# widen. A route that refused the mode leaves the last answer in place, so the run ends here
# rather than reading the next check against an answer it did not set.
answers() { # <mode>
	local reply
	if ! reply="$(curl -sS -f -X POST "$DEV_BROKER_GITHUB_ANSWER_URL" -H 'Content-Type: application/json' -d "{\"mode\": \"$1\"}")"; then
		echo "github-narrowing-check: the stand-in refused the answer $1" >&2
		exit 1
	fi
	echo "github-narrowing-check: the GitHub stand now answers $reply" >&2
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

if [ "$PASSED" -eq "$TOTAL" ]; then
	printf 'all %d assertions passed\n' "$TOTAL"
	exit 0
fi
printf '%d of %d assertions failed\n' "$((TOTAL - PASSED))" "$TOTAL"
exit 1
