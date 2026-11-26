#!/usr/bin/env bash
# packages/envoy/scripts/proxy-chain-check.test.sh
#
# Proves, with no stack, no broker build and no Postgres, the parts of proxy-chain-check.sh its
# verdicts rest on:
#   (a) its arguments: a part it does not run, two parts and --old-client-ref with no ref each
#       exit 2 and start nothing, and --help prints the usage;
#   (b) call reads a call's answer as the check reads it - the client's exit, the HTTP status, how
#       many certificates the broker presented, the leaf's start and end, and the serial of the
#       certificate that signed it - from a transcript shaped like `openssl s_client -showcerts`
#       output, holding a chain made here, and a call whose client failed reads as failed;
#   (c) intermediates reads each "proxy intermediate" line of a broker's log, and await_count
#       answers a count the log already holds at once, with no wait;
#   (d) set_clock_at writes the offset that makes clock_now read the moment asked for, through a
#       file renamed over the clock file, and serial_norm reads openssl's serial as the broker's;
#   (e) report says UNKNOWN, and fails, when the run evaluated no property, and names how many
#       of how many failed.
# Each function is read out of the script that defines it and run in this shell. Needs bash,
# jq and openssl.
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$script_dir/proxy-chain-check.sh"
# shellcheck source-path=SCRIPTDIR source=lib/check-stack.sh
. "$script_dir/lib/check-stack.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/proxy-chain-check-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT
failed=0
check() { # <label> <command>...
	local label="$1"
	shift
	if "$@"; then
		printf 'PASS: %s\n' "$label"
	else
		printf 'FAIL: %s\n' "$label"
		failed=$((failed + 1))
	fi
}

# --- (a) the arguments. ---
usage_exit() { # <want> <args>...
	local want="$1" status=0
	shift
	TMPDIR="$work" bash "$script" "$@" >"$work/usage.out" 2>&1 || status=$?
	[ "$status" = "$want" ]
}
check "a part the check does not run exits 2" usage_exit 2 k99
check "two parts at once exit 2" usage_exit 2 k27 k21
check "--old-client-ref with no ref exits 2" usage_exit 2 compat --old-client-ref
check "--help exits 0" usage_exit 0 --help
check "--help prints the usage" grep -q '^usage: proxy-chain-check.sh' "$work/usage.out"
check "a usage error leaves no work directory behind" test -z "$(find "$work" -mindepth 1 -maxdepth 1 -name 'proxy-chain-check-*' -print -quit)"

for function in serial_norm set_clock_at clock_now intermediates intermediate_count await_count call assert report digest shape; do
	# A one-line function is its one line; any other runs to the first line that is a lone "}".
	definition="$(grep -E "^${function}\(\) \{.*\}$" "$script" || true)"
	if [ -z "$definition" ]; then
		definition="$(sed -n "/^${function}() {/,/^}/p" "$script")"
	fi
	[[ -n "$definition" ]] || {
		printf 'FAIL: proxy-chain-check.sh defines no %s()\n' "$function"
		exit 1
	}
	eval "$definition"
done

# --- (b) call, on a transcript of a chain made here: a root, an intermediate of path length 0
# that it signs, and a leaf for s.airlock.test the intermediate signs. The stand-in agent-secrets
# runs the client it is given as agent-secrets does, and the stand-in client writes what
# s-client.sh writes: the transcript, the stderr and the exit status. ---
cd "$work"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout root.key -out root.pem \
	-days 30 -subj '/CN=test root' -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign' 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout mid.key -out mid.csr -subj '/CN=test intermediate' 2>/dev/null
printf 'basicConstraints=critical,CA:TRUE,pathlen:0\nkeyUsage=critical,keyCertSign\n' >mid.ext
openssl x509 -req -in mid.csr -CA root.pem -CAkey root.key -set_serial 0x00ab12 -days 7 -extfile mid.ext -out mid.pem 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout leaf.key -out leaf.csr -subj '/CN=s.airlock.test' 2>/dev/null
openssl x509 -req -in leaf.csr -CA mid.pem -CAkey mid.key -set_serial 7 -days 1 -out leaf.pem 2>/dev/null
cd - >/dev/null
{
	echo "CONNECTED(00000003)"
	echo "---"
	echo "Certificate chain"
	cat "$work/leaf.pem" "$work/mid.pem"
	echo "---"
	echo "Verify return code: 0 (ok)"
	printf 'HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok\n'
} >"$work/transcript.fixture"
mkdir -p "$work/bin"
cat >"$work/bin/agent-secrets" <<'EOF'
#!/usr/bin/env bash
# agent-secrets NAME -- COMMAND ARGS...: run COMMAND as the real one does, with the proxy URL in
# HTTPS_PROXY, the placeholder in NAME, and a trust bundle in SSL_CERT_FILE.
[ "$2" = -- ] || exit 2
name="$1"
shift 2
HTTPS_PROXY="$(printf 'http://%s:%s@%s' agent-secrets the-stand-in-proxy-password 127.0.0.1:9)"
export HTTPS_PROXY SSL_CERT_FILE=/dev/null
export "$name=agent-secrets-placeholder-test"
exec "$@"
EOF
cat >"$work/s-client.sh" <<'EOF'
#!/usr/bin/env bash
out=$5
mkdir -p "$out"
cp "$PROXY_CHAIN_TEST_TRANSCRIPT" "$out/transcript"
printf 'verify error:num=10:certificate has expired\n' >"$out/stderr"
echo "$PROXY_CHAIN_TEST_STATUS" >"$out/status"
EOF
chmod +x "$work/bin/agent-secrets" "$work/s-client.sh"
# shellcheck disable=SC2034 # call reads it
WORK_DIR="$work"
leaf_start="$(date -d "$(openssl x509 -in "$work/leaf.pem" -noout -startdate | cut -d= -f2)" +%s)"
leaf_end="$(date -d "$(openssl x509 -in "$work/leaf.pem" -noout -enddate | cut -d= -f2)" +%s)"
PATH="$work/bin:$PATH" PROXY_CHAIN_TEST_TRANSCRIPT="$work/transcript.fixture" PROXY_CHAIN_TEST_STATUS=0 \
	call good s.airlock.test /ok "$(date +%s)" "$work/root.pem"
check "call reads the client's exit, the HTTP status and two certificates presented" \
	test "$CALL_STATUS $CALL_HTTP $CALL_CERTS" = "0 200 2"
check "call reads the leaf's subject, start and end from the first certificate" \
	test "$CALL_LEAF_SUBJECT $CALL_LEAF_START $CALL_LEAF_END" = "CN=s.airlock.test $leaf_start $leaf_end"
check "call reads the signer's serial as the broker logs it (ab12, from openssl's 00AB12)" \
	test "$CALL_SIGNER_SERIAL" = ab12
PATH="$work/bin:$PATH" PROXY_CHAIN_TEST_TRANSCRIPT=/dev/null PROXY_CHAIN_TEST_STATUS=1 \
	call failed s.airlock.test /ok "$(date +%s)" "$work/root.pem"
check "a call whose client failed reads as failed, with no certificate, and says why" \
	test "$CALL_STATUS ${CALL_HTTP:-none} $CALL_CERTS" = "1 none 0"
check "a failed call's summary carries the client's own error" grep -q 'certificate has expired' <<<"$CALL_SUMMARY"

# --- (c) intermediates, on a log holding two intermediate lines among others. ---
cat >"$work/broker.log" <<'EOF'
{"time":"2026-01-01T00:00:00Z","severity":"INFO","message":"broker listening","addr":"127.0.0.1:1"}
{"time":"2026-01-01T00:00:01Z","severity":"INFO","message":"proxy intermediate","serial":"ab12","names":"a.example b.example","not_before":"2026-01-01T00:00:00Z","not_after":"2026-01-08T00:00:00Z","self_signed":false}
{"time":"2026-01-01T00:00:02Z","severity":"WARN","message":"proxy intermediate request failed","error":"the pool answered 503"}
not a JSON line
{"time":"2026-01-06T00:00:01Z","severity":"INFO","message":"proxy intermediate","serial":"cd34","names":"a.example","not_before":"2026-01-06T00:00:00Z","not_after":"2026-01-13T00:00:00Z","self_signed":false}
EOF
got="$(intermediates "$work/broker.log")"
want="$(printf 'ab12 1767225600 1767830400 a.example,b.example\ncd34 1767657600 1768262400 a.example')"
check "intermediates reads each intermediate line, its serial, dates and names, and nothing else" test "$got" = "$want"
[ "$got" = "$want" ] || printf 'intermediates read:\n%s\n' "$got"
check "intermediate_count counts them" test "$(intermediate_count "$work/broker.log")" = 2
check "intermediate_count of a log with none is 0" test "$(intermediate_count /dev/null)" = 0
# await_count is the shared polling loop behind the two log counters. A log that already has the
# count answers at once and records no wait.
AWAITED="unset"
check "await_count answers an already-reached count" await_count intermediate_count "$work/broker.log" 2 1
check "await_count answers an already-reached count without waiting" test "$AWAITED" = 0

# --- (d) the clock file and the serials. ---
PROXY_CLOCK_OFFSET_FILE="$work/clock-offset"
printf '0s\n' >"$PROXY_CLOCK_OFFSET_FILE"
target=$(($(date +%s) + 5 * 86400 + 60))
set_clock_at "$target"
read_back="$(clock_now)"
check "set_clock_at makes clock_now read the moment asked for" test $((read_back - target)) -ge 0 -a $((read_back - target)) -le 2
check "the clock file holds a Go duration in seconds" grep -qE '^[0-9]+s$' "$PROXY_CLOCK_OFFSET_FILE"
check "set_clock_at leaves no half-written file beside the clock file" test ! -e "$PROXY_CLOCK_OFFSET_FILE.next"
check "serial_norm reads openssl's serial=00AB12 as ab12" test "$(serial_norm serial=00AB12)" = ab12
check "serial_norm keeps a lone zero" test "$(serial_norm serial=00)" = 0

# --- (e) report. ---
# shellcheck disable=SC2034 # report reads each
PART=test EXPECTED=() OBSERVED=() CONTROL=() VERDICTS=() PASSED=0 TOTAL=0
report_status=0
report >"$work/report.out" || report_status=$?
check "a run that evaluated no property reports UNKNOWN and fails" test "$report_status:$(tail -n1 "$work/report.out")" = "1:check test: UNKNOWN (no property was evaluated)"
assert 0 "one property" "seen"
assert 1 "another property" "not seen"
report_status=0
report >"$work/report.out" || report_status=$?
check "a run with one property of two failing says so and fails" test "$report_status:$(tail -n1 "$work/report.out")" = "1:check test: failed (1 of 2)"

if [[ "$failed" -ne 0 ]]; then
	echo "proxy-chain-check.test: $failed failed" >&2
	exit 1
fi
echo 'proxy-chain-check.test: passed'
