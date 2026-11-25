#!/usr/bin/env bash
# packages/envoy/scripts/judge-check.test.sh
#
# Proves, with no stack, no broker build and no Postgres, how judge-check.sh starts a broker of its
# own (start_extra_broker, which the k10 family runs), and the one helper it, airlock-check.sh and
# dev-stack.sh start a process with (lib/exec-env.sh's exec_with_env):
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
#   (e) under bash -x, broker_env_of and with_broker_env trace nothing of what they copy.
# Each function is read out of the script that defines it and run in this shell. Needs bash, jq
# and Linux's /proc.
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
for function in await logged_addr broker_env start_extra_broker; do
  definition="$(sed -n "/^${function}() {/,/^}/p" "$script_dir/judge-check.sh")"
  [[ -n "$definition" ]] || { printf 'FAIL: judge-check.sh defines no %s()\n' "$function"; exit 1; }
  eval "$definition"
done

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
check "judge-check's extra broker start puts the stack broker's password in no process's arguments" \
  no_password_in_arguments
if ! no_password_in_arguments; then cat "$work/argv" "$work/broker-cmdline" 2>/dev/null || true; fi
want_environ="$(printf '%s\n' BROKER_AIRLOCK_LISTEN_ADDR BROKER_DATABASE_URL BROKER_JUDGE_MODEL BROKER_LISTEN_ADDR BROKER_METRICS_LISTEN_ADDR PATH STAND_IN | sort)"
got_environ="$(cat "$work/broker-environ" 2>/dev/null || true)"
check "judge-check's extra broker start gives the broker exactly the stack broker's environment and its overrides" \
  test "$got_environ" = "$want_environ"
[[ "$got_environ" == "$want_environ" ]] || printf 'the broker had: %s\n' "$(tr '\n' ' ' <<<"$got_environ")"
check "judge-check's extra broker start answers the addresses the broker logged" \
  test "${EXTRA_API_ADDR:-} ${EXTRA_PROXY_ADDR:-}" = "127.0.0.1:1 127.0.0.1:2"

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
  # shellcheck disable=SC2034 # start_extra_broker, eval'd above, reads it
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

if [[ "$failed" -ne 0 ]]; then
  printf 'judge-check.test: %s failed\n' "$failed"
  exit 1
fi
echo 'judge-check.test: passed'
