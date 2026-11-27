#!/usr/bin/env bash
# packages/envoy/scripts/judge-check.test.sh
#
# Proves, with no stack, no broker build and no Postgres, how judge-check.sh and
# proxy-chain-check.sh start the stack and a broker of their own (lib/check-stack.sh, which both
# source), the one helper they, airlock-check.sh and dev-stack.sh start a process with
# (lib/exec-env.sh's exec_with_env), and how judge-check.sh says a run's result:
#   (a) start_extra_broker gives the broker exactly the stack broker's environment and the
#       overrides it names, copied from a stand-in stack broker whose BROKER_DATABASE_URL holds a
#       password, and that password is in no process's arguments: not the broker's own, and not
#       those of any env the start ran, which a recording env first on PATH would show (env -i
#       once carried every setting there);
#   (b) a readonly variable the starting shell exports reaches the started program no more: no
#       unset removes it, and exec_with_env takes off its export instead;
#   (c) a start that names a readonly variable among its settings is refused, naming the variable,
#       and runs nothing, in a subshell that keeps set -e as the callers' do;
#   (d) a stack broker whose environment holds SHELLOPTS and BASHOPTS, bash's own readonly
#       variables, still gives an extra broker that starts, holding neither;
#   (e) under bash -x, broker_env_of and with_broker_env trace nothing of what they copy;
#   (f) stack_start passes its options to dev-airlock.sh and reads its banner: every export, each
#       broker's API and proxy address, pid and log, and the broker binary;
#   (g) judge-check.sh and proxy-chain-check.sh each source lib/check-stack.sh and define none of
#       its six functions themselves, so a copy that comes back is caught here;
#   (h) judge-check.sh's report says UNKNOWN, and fails, when the run evaluated no property, and
#       counts a run with one property failing as failed;
#   (i) log_records returns the complete JSON lines of its named message alone, omits malformed
#       lines and answers one empty array for a missing log;
#   (j) k10's base-control guard (k10_base_control) reports UNKNOWN, naming JUDGE_CHECK_BASE, and
#       builds nothing, for a base whose policy.go defines TagJudge: that broker reads the judge
#       from the key's tags, so it is no control, whatever its profiles.go holds. A base that
#       reads the judge from the proxy block and defines no TagJudge passes the guard and is built;
#   (k) page_of and post_decision, the approval k10's card case makes, post back each
#       profile_digest field the page prints, against a stand-in page that refuses an approval
#       naming none, as the broker does (403 Profile changed); with page_of's profile_digest taken
#       out, the same approval is refused.
# The lib is sourced into this shell, each other function is read out of the script that defines
# it, and all of them run here. Needs bash, jq and Linux's /proc.
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/judge-check-test.XXXXXX")"
stand_in=""
trap '[[ -z "$stand_in" ]] || kill "$stand_in" 2>/dev/null || true; rm -rf "$work"' EXIT
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

# shellcheck source-path=SCRIPTDIR source=lib/exec-env.sh
. "$script_dir/lib/exec-env.sh"
CHECK_NAME=judge-check
# shellcheck source-path=SCRIPTDIR source=lib/check-stack.sh
. "$script_dir/lib/check-stack.sh"

# --- (i) log_records. ---
# log_records is the one JSON-lines reader every shared helper and proxy-chain check takes.
# A malformed line is ignored, and only the named message reaches its caller.
cat >"$work/log-records.log" <<'EOF'
{"message":"broker listening","addr":"127.0.0.1:1"}
not JSON
{"message":"watch","count":2}
EOF
log_records_filters() {
  local got
  got="$(log_records watch "$work/log-records.log")" || return 1
  jq -e 'length == 1 and .[0].count == 2' <<<"$got" >/dev/null
}
check "log_records reads the complete JSON lines of its named message alone" log_records_filters
log_records_missing_is_one_empty_array() {
  local got
  got="$(log_records watch "$work/no-such-log")"
  test "$got" = '[]'
}
check "log_records reads a missing log as one empty array" log_records_missing_is_one_empty_array

# --- (a) start_extra_broker, on a stand-in stack broker whose database URL holds a password. ---
mkdir -p "$work/bin"
real_env="$(command -v env)"
cat >"$work/bin/env" <<EOF
#!/usr/bin/env bash
printf 'env %s\n' "\$*" >>"$work/argv"
exec ${real_env} "\$@"
EOF
cat >"$work/broker" <<EOF
#!/usr/bin/env bash
tr '\0' ' ' </proc/\$\$/cmdline >"$work/broker-cmdline"
tr '\0' '\n' </proc/\$\$/environ | sed 's/=.*//' | sort >"$work/broker-environ"
echo '{"message":"broker listening","addr":"127.0.0.1:1"}'
echo '{"message":"airlock proxy listening","addr":"127.0.0.1:2"}'
exec sleep 60
EOF
chmod +x "$work/bin/env" "$work/broker"
# The stand-in's environment is exactly these three; the real env starts it, not the recording one.
"$real_env" -i PATH="$work/bin:/usr/bin:/bin" BROKER_DATABASE_URL='postgres://broker:not-a-secret@127.0.0.1:5999/db' \
  STAND_IN=1 sleep 60 &
stand_in=$!
# broker_env reads the stand-in's environment once it is sleep's, not env's before it execs sleep;
# a stand-in that is not sleep by the end of the wait ends the test, naming what it is.
stand_in_cmdline() { tr '\0' ' ' <"/proc/$stand_in/cmdline" 2>/dev/null; }
for _ in $(seq 1 50); do
  [[ "$(stand_in_cmdline)" == sleep* ]] && break
  sleep 0.1
done
[[ "$(stand_in_cmdline)" == sleep* ]] ||
  { printf 'FAIL: the stand-in broker is not sleep after 5 s: %s\n' "$(stand_in_cmdline)"; exit 1; }
# shellcheck disable=SC2034 # each is read by the functions eval'd above
BROKER_PIDS=("$stand_in") WORK_DIR="$work" BROKER_BIN="$work/broker" EXTRA_BROKER_PIDS=()
PATH="$work/bin:$PATH" start_extra_broker test BROKER_JUDGE_MODEL=
for pid in "${EXTRA_BROKER_PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
no_password_in_arguments() {
  [[ -s "$work/broker-cmdline" ]] && ! grep -q not-a-secret "$work/broker-cmdline" &&
    ! grep -qs not-a-secret "$work/argv"
}
check "the shared extra broker start puts the stack broker's password in no process's arguments" \
  no_password_in_arguments
if ! no_password_in_arguments; then cat "$work/argv" "$work/broker-cmdline" 2>/dev/null || true; fi
want_environ="$(printf '%s\n' BROKER_AIRLOCK_LISTEN_ADDR BROKER_DATABASE_URL BROKER_JUDGE_MODEL BROKER_LISTEN_ADDR BROKER_METRICS_LISTEN_ADDR PATH STAND_IN | sort)"
got_environ="$(cat "$work/broker-environ" 2>/dev/null || true)"
check "the shared extra broker start gives the broker exactly the stack broker's environment and its overrides" \
  test "$got_environ" = "$want_environ"
[[ "$got_environ" == "$want_environ" ]] || printf 'the broker had: %s\n' "$(tr '\n' ' ' <<<"$got_environ")"
check "the shared extra broker start answers the addresses, the log and the pid of the broker it started" \
  test "${EXTRA_API_ADDR:-} ${EXTRA_PROXY_ADDR:-} ${EXTRA_LOG:-} ${EXTRA_PID:-}" = "127.0.0.1:1 127.0.0.1:2 $work/broker-test.log ${EXTRA_BROKER_PIDS[0]:-none}"

# --- (b) and (c) exec_with_env and a readonly variable of the starting shell, each run in a
# subshell of its own in the background, as its callers run it: `( exec_with_env ... ) >log &`.
# That subshell keeps this test's set -e, as theirs keep their scripts'; a command substitution
# would drop it, and with it the way a failed assignment ends the shell. ---
(
  export JUDGE_CHECK_TEST_READONLY=not-a-secret-readonly
  readonly JUDGE_CHECK_TEST_READONLY
  exec_with_env ONLY=1 -- "$real_env"
) >"$work/readonly.out" 2>&1 &
wait "$!" || true
readonly_reached="$(cat "$work/readonly.out")"
check "a readonly variable the starting shell exports reaches the started program no more" \
  test "$readonly_reached" = "ONLY=1"
[[ "$readonly_reached" == "ONLY=1" ]] || printf 'the program had: %s\n' "$readonly_reached"
refused_status=0
(
  # shellcheck disable=SC2034 # the start below names it: the row is that it cannot take a value
  readonly JUDGE_CHECK_TEST_NAMED=1
  exec_with_env JUDGE_CHECK_TEST_NAMED=2 -- "$real_env"
) >"$work/refused.out" 2>&1 &
wait "$!" || refused_status=$?
refused_said="$(cat "$work/refused.out")"
check "a start that names a readonly variable among its settings is refused with 2, naming it, and runs nothing" \
  test "$refused_status:$refused_said" = "2:${0##*/}: refused to start ${real_env}: JUDGE_CHECK_TEST_NAMED is readonly in this shell, so it cannot take the value the start gives it"
[[ "$refused_status" -eq 2 ]] || printf 'the start exited %s, saying: %s\n' "$refused_status" "$refused_said"

# --- (d) A stack broker started from a shell that exported SHELLOPTS and BASHOPTS carries both,
# bash's own readonly variables; broker_env_of leaves them out of the copy, so judge-check's extra
# broker still starts, as env -i once started it. ---
"$real_env" -i PATH="$work/bin:/usr/bin:/bin" SHELLOPTS=braceexpand:hashall:interactive-comments \
  BASHOPTS=checkwinsize BROKER_DATABASE_URL='postgres://broker:not-a-secret@127.0.0.1:5999/db' sleep 60 &
shellopts_stand_in=$!
for _ in $(seq 1 50); do
  [[ "$(tr '\0' ' ' <"/proc/$shellopts_stand_in/cmdline" 2>/dev/null)" == sleep* ]] && break
  sleep 0.1
done
rm -f "$work/broker-environ"
shellopts_status=0
(
  # shellcheck disable=SC2034,SC2030 # start_extra_broker reads it, in this subshell alone
  BROKER_PIDS=("$shellopts_stand_in") EXTRA_BROKER_PIDS=()
  PATH="$work/bin:$PATH" start_extra_broker shellopts
  for pid in "${EXTRA_BROKER_PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
) >"$work/shellopts.out" 2>&1 || shellopts_status=$?
kill "$shellopts_stand_in" 2>/dev/null || true
shellopts_copied="$(grep -c -E '^(SHELLOPTS|BASHOPTS)$' "$work/broker-environ" 2>/dev/null || true)"
check "a stack broker whose environment holds an exported SHELLOPTS and BASHOPTS still gives a broker that starts, without either" \
  test "${shellopts_status}:${shellopts_copied}" = "0:0"
if [[ "${shellopts_status}:${shellopts_copied}" != "0:0" ]]; then
  printf 'the start exited %s and copied %s of the two; it said:\n' "$shellopts_status" "${shellopts_copied:-none}"
  cat "$work/shellopts.out" "$work/broker-shellopts.log" 2>/dev/null || true
fi

# --- (e) Under bash -x, broker_env_of and with_broker_env trace nothing of the environment they
# copy: the stand-in stack broker of (a), still running, holds the password in its database URL. ---
cat >"$work/trace-caller" <<'EOF'
. "$EXEC_ENV_LIB"
broker_env_of "$TRACE_STAND_IN"
(with_broker_env "$TRACE_ENV") >/dev/null
EOF
EXEC_ENV_LIB="$script_dir/lib/exec-env.sh" TRACE_STAND_IN="$stand_in" TRACE_ENV="$real_env" \
  bash -x "$work/trace-caller" 2>"$work/trace" || true
traced="$(grep -n not-a-secret "$work/trace" | cut -c1-160 || true)"
check "under bash -x, broker_env_of and with_broker_env trace no line holding a setting of the environment they copy" \
  test -z "$traced"
[[ -z "$traced" ]] || printf 'trace lines holding the password: %s\n' "$traced"

# --- (f) stack_start, on a stand-in dev-airlock.sh that prints the banner's shape: dev-broker.sh's
# export block, two brokers, the ready line and the airlock's own exports, then waits as the real
# one waits. ---
mkdir -p "$work/stack-scripts"
cat >"$work/stack-scripts/dev-airlock.sh" <<'EOF'
#!/usr/bin/env bash
printf 'options: %s\n' "$*" >"${0%/*}/options"
cat <<'BANNER'
  export DEV_BROKER_DIR=/stand-in/broker-dir
  export AIRLOCK_PROXY_URL=http://dev-broker-front:1
dev-airlock: ready.

  broker 1:            api 127.0.0.1:4001, proxy 127.0.0.1:5001, pid 101, log /stand in/broker-1.log
  broker 2:            api 127.0.0.1:4002, proxy 127.0.0.1:5002, pid 102, log /stand in/broker-2.log
  export AIRLOCK_PROXY_URL=http://127.0.0.1:13381
BANNER
exec sleep 60
EOF
# shellcheck disable=SC2034 # each is read by stack_start
SCRIPT_DIR="$work/stack-scripts" STACK_LOG="$work/stack.log"
stack_start --one-broker --judge-skip-checks
kill "$STACK_PID" 2>/dev/null || true
check "stack_start passes its options to dev-airlock.sh" \
  test "$(cat "$work/stack-scripts/options" 2>/dev/null)" = "options: --one-broker --judge-skip-checks"
# Each list is joined with "|" and counted, so a reader that split a log path at its space into two
# entries, which a join with spaces would read back unchanged, fails here.
# shellcheck disable=SC2031 # stack_start set BROKER_PIDS in this shell; (d)'s was its subshell's
stack_read="$(IFS='|'; echo "${#BROKER_API_ADDRS[@]} ${BROKER_API_ADDRS[*]}/${#BROKER_PROXY_ADDRS[@]} ${BROKER_PROXY_ADDRS[*]}/${#BROKER_PIDS[@]} ${BROKER_PIDS[*]}/${#BROKER_LOGS[@]} ${BROKER_LOGS[*]}/$BROKER_BIN")"
check "stack_start reads each broker's API and proxy address, pid and log, a log path with a space whole, and the broker binary" \
  test "$stack_read" = "2 127.0.0.1:4001|127.0.0.1:4002/2 127.0.0.1:5001|127.0.0.1:5002/2 101|102/2 /stand in/broker-1.log|/stand in/broker-2.log//stand-in/broker-dir/bin/broker"
[[ "$stack_read" == "2 127.0.0.1:4001|"* && "$stack_read" == *"/2 /stand in/broker-1.log|"* ]] || printf 'stack_start read: %s\n' "$stack_read"
check "stack_start evaluates every export, the airlock's last, so it wins where both name one" \
  test "${AIRLOCK_PROXY_URL:-}" = "http://127.0.0.1:13381"

# --- (g) Each check sources the lib and keeps no copy of what it holds. ---
for script in judge-check.sh proxy-chain-check.sh; do
  # shellcheck disable=SC2016 # the line as each script writes it, $SCRIPT_DIR unexpanded
  check "$script sources lib/check-stack.sh" grep -qxF '. "$SCRIPT_DIR/lib/check-stack.sh"' "$script_dir/$script"
  copies="$(grep -cE '^(await|stack_start|broker_env|start_extra_broker|log_records|logged_addr)\(\) \{' "$script_dir/$script" || true)"
  check "$script defines none of the lib's six functions itself" test "$copies" = 0
done

# --- (h) judge-check.sh's result: a run that asserted nothing proved nothing. Its pass, fail,
# assert and report are read out of the script and run in a subshell, apart from this test's
# own. ---
judge_report() { # <assert status>...: report's exit status and its last line
  (
    for name in pass fail assert report; do
      definition="$(sed -n "/^${name}() {/,/^}/p" "$script_dir/judge-check.sh")"
      [[ -n "$definition" ]] || { echo "judge-check.sh defines no ${name}()"; exit 0; }
      eval "$definition"
    done
    # shellcheck disable=SC2034 # report reads each
    CHECK_ID=test EXPECTED=() OBSERVED=() CONTROL=() VERDICTS=() PASSED=0 TOTAL=0
    for status in "$@"; do assert "$status" "a property" "seen"; done
    status=0
    report >"$work/judge-report.out" || status=$?
    echo "$status:$(tail -n1 "$work/judge-report.out")"
  )
}
said="$(judge_report)"
check "judge-check.sh's report says UNKNOWN and fails when the run evaluated no property" \
  test "$said" = "1:check test: UNKNOWN (no property was evaluated)"
[[ "$said" == "1:check test: UNKNOWN"* ]] || printf 'a run of no property: %s\n' "$said"
check "judge-check.sh's report fails a run with one property of two failing" \
  test "$(judge_report 0 1)" = "1:check test: failed (1 of 2)"
check "judge-check.sh's report passes a run whose every property passed" \
  test "$(judge_report 0 0)" = "0:check test: passed"
# --- (j) k10_base_control's guard, against two fixture bases in a git repository of their own,
# read through JUDGE_CHECK_BASE. Each base holds the two files the guard reads; the stand-in go
# records that a build was started and fails it, so neither case goes on to start a broker. ---
definition="$(sed -n '/^k10_base_control() {/,/^}/p' "$script_dir/judge-check.sh")"
[[ -n "$definition" ]] || { printf 'FAIL: judge-check.sh defines no k10_base_control()\n'; exit 1; }
eval "$definition"
base_repo="$work/base-repo"
mkdir -p "$base_repo/internal/cipher/policy" "$work/base-bin"
git -C "$base_repo" init -q
cat >"$work/base-bin/go" <<EOF
#!/usr/bin/env bash
touch "$work/base-go-ran"
exit 1
EOF
chmod +x "$work/base-bin/go"
base_commit() { # <policy.go's text> <profiles.go's text>
  printf '%s\n' "$1" >"$base_repo/internal/cipher/policy/policy.go"
  printf '%s\n' "$2" >"$base_repo/internal/cipher/policy/profiles.go"
  git -C "$base_repo" add -A
  git -C "$base_repo" -c user.name=t -c user.email=t@example.com -c commit.gpgsign=false commit -q -m base
  git -C "$base_repo" rev-parse HEAD
}
guard_run() { # <base commit>: the guard's control lines, after a run of k10_base_control
  rm -f "$work/base-go-ran" "$work/base-control"
  rm -rf "$work/base-work"
  mkdir -p "$work/base-work"
  (
    # shellcheck disable=SC2329 # k10_base_control, eval'd above, calls both
    control() { printf '%s\n' "$*" >>"$work/base-control"; }
    # shellcheck disable=SC2329 # as control above
    fail() { printf 'FAIL-CALLED: %s\n' "$*" >>"$work/base-control"; }
    WORK_DIR="$work/base-work" ENVOY_DIR="$base_repo" JUDGE_CHECK_BASE="$1" PATH="$work/base-bin:$PATH" k10_base_control
  ) >/dev/null 2>&1 || true
  cat "$work/base-control" 2>/dev/null || true
}
# shellcheck disable=SC2016 # Go source text, with its backquotes, written as it is
tagged_base="$(base_commit 'const TagJudge = "judge"' 'Judge json.RawMessage `json:"judge"`')"
said="$(guard_run "$tagged_base")"
# shellcheck disable=SC2016 # the bash -c text is expanded by the inner shell, from its arguments
check "k10's base control reports UNKNOWN, naming JUDGE_CHECK_BASE, for a base that reads the key's judge tag, and builds nothing" \
  bash -c '[[ "$1" == *UNKNOWN* && "$1" == *JUDGE_CHECK_BASE* && "$1" == *TagJudge* && "$1" == *FAIL-CALLED* && ! -e "$2" ]]' _ "$said" "$work/base-go-ran"
# shellcheck disable=SC2016 # Go source text, with its backquotes, written as it is
file_base="$(base_commit 'const TagOwner = "owner"' 'Judge *uint32 `json:"judge"`')"
said="$(guard_run "$file_base")"
check "k10's base control builds a base that reads the judge from the proxy block and defines no TagJudge" \
  test -e "$work/base-go-ran"

# --- (k) page_of and post_decision against a stand-in approval page: it prints a form with two
# profile_digest fields, each "<record> <name> <digest>" with spaces, as the broker prints one for
# each profile a request names, and holds the POST to the broker's rule (approval.profilesMoved):
# every printed fingerprint must come back as printed, or the answer is 403 and nothing is decided.
# JUDGE_CHECK_TEST_PAGE_OF, when set, names a file holding another page_of to run in its place. ---
for function in page_of post_decision; do
  definition="$(sed -n "/^${function}() {/,/^}/p" "$script_dir/judge-check.sh")"
  [[ -n "$definition" ]] || { printf 'FAIL: judge-check.sh defines no %s()\n' "$function"; exit 1; }
  eval "$definition"
done
if [[ -n "${JUDGE_CHECK_TEST_PAGE_OF:-}" ]]; then
  # shellcheck disable=SC1090 # a page_of the caller names, for the red run against an older one
  . "$JUDGE_CHECK_TEST_PAGE_OF"
fi
page_dir="$work/page"
mkdir -p "$page_dir"
cat >"$page_dir/stand.py" <<'PY'
import http.server, urllib.parse
REC = "ab" * 32
DIGESTS = [REC + " AIRLOCK_JUDGED_TOKEN " + "1f" * 32, REC + " AIRLOCK_UNJUDGED_TOKEN " + "2e" * 32]
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, code, body=b""):
        self.send_response(code)
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_GET(self):
        if self.path != "/approve/" + REC or "__Host-broker-session=s" not in self.headers.get("Cookie", ""):
            return self.send(404)
        fields = '<input type="hidden" name="printed" value="%s">' % REC
        fields += "".join('<input type="hidden" name="profile_digest" value="%s">' % d for d in DIGESTS)
        fields += '<input type="hidden" name="token" value="tok"><input type="hidden" name="expires" value="9">'
        fields += '<input type="hidden" name="record" value="%s">' % REC
        self.send(200, ("<form>" + fields + "</form>").encode())
    def do_POST(self):
        form = urllib.parse.parse_qs(self.rfile.read(int(self.headers["Content-Length"])).decode(), keep_blank_values=True)
        ok = (self.headers.get("Origin") and form.get("decision") == ["approve"] and form.get("record") == [REC]
              and form.get("token") == ["tok"] and sorted(form.get("profile_digest", [])) == sorted(DIGESTS))
        self.send(303 if ok else 403, b"" if ok else b"Profile changed")
s = http.server.HTTPServer(("127.0.0.1", 0), H)
print(s.server_port, flush=True)
s.serve_forever()
PY
python3 "$page_dir/stand.py" >"$page_dir/port" &
page_stand=$!
for _ in $(seq 1 50); do [[ -s "$page_dir/port" ]] && break; sleep 0.1; done
record="$(printf 'ab%.0s' $(seq 1 32))"
# shellcheck disable=SC2034 # read by page_of and post_decision, eval'd above
AGENT_SECRETS_URL="http://127.0.0.1:$(cat "$page_dir/port")" SIGNED_IN_COOKIE="__Host-broker-session=s" WORK_DIR="$page_dir"
page_of "$record"
posted="$(post_decision "$record" "$PAGE_TOKEN_FIELDS")"
check "page_of and post_decision post back each profile_digest the page printed, and the approval is decided (303)" \
  test "$posted" = 303
[[ "$posted" == 303 ]] || printf 'the POST answered %s with the fields %s\n' "$posted" "$PAGE_TOKEN_FIELDS"
# The control: the same page_of with profile_digest taken out of what it reads off the page.
without="$(sed -n '/^page_of() {/,/^}/p' "$script_dir/judge-check.sh")"
without="${without//\\|profile_digest/}"
[[ "$without" != *profile_digest\\\)* ]] || { printf 'FAIL: the control could not take profile_digest out of page_of\n'; exit 1; }
eval "$without"
page_of "$record"
posted="$(post_decision "$record" "$PAGE_TOKEN_FIELDS")"
check "control: the same approval without the page's profile_digest fields is refused 403" \
  test "$posted" = 403
kill "$page_stand" 2>/dev/null || true

if [[ "$failed" -ne 0 ]]; then
  printf 'judge-check.test: %s failed\n' "$failed"
  exit 1
fi
echo 'judge-check.test: passed'
