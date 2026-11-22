#!/usr/bin/env bash
# packages/envoy/loadtest/common.test.sh
#
# Proves the run lock of common.sh (lock_root) and the ports it derives from LOADTEST_ROOT
# (stack_ports), without a stack. Each lock taker below sources common.sh and calls lock_root, as
# run-series.sh, run-step.sh and stack.sh do.
#
# The case this covers: a series' run-step.sh that re-opened descriptor 9 lost the lock
# that it inherited from run-series.sh and went on by the token alone. Killing run-series.sh then
# freed the root while its step still ran, and a new run-step.sh reset the stack under that step.
# And a refused caller could name a previous holder that had ended, whose complete record was still
# in the lock file while the new holder had locked but not yet written its own.
#
# Asserts that: (a) a holder's child that took the lock with the inherited token and descriptor
# keeps the root after the holder is killed: a third caller is refused, naming the holder's record
# and the live child, and is admitted once the child has exited; (b) a refusal names the live
# holder, not a dead previous holder whose complete record is still in the file; (c) a caller with
# the holder's token but without its descriptor 9 is refused; (d) a lock held by a process that
# this PID namespace cannot see (here: on a descriptor other than 9, under a record whose pid does
# not exist) is refused as held, naming the record's pid and the lock file not to remove, never as
# free; (e) a caller is admitted when the record's pid is dead and no process holds the lock, also
# when the holder ends while the caller waits for its record; (f) two callers that wait at once on
# such a holder, under a record of another PID namespace, never name each other as the holder: both
# are refused as "not visible"; (g) a record of another PID namespace whose pid is a live process
# here (this test's own) is refused as "not visible", without naming that process as the holder;
# (i) a root always gets the same set of ports, these two roots get two sets that share none (two
# roots in general share all 7 when their paths hash into one block of the 200, which stack.sh
# refuses), every port of every block lies in 1024-65535, outside this machine's ephemeral range,
# and no two ports of two blocks meet, and a LOADTEST_*_PORT overrides its own port alone; (j) under
# an ephemeral range that covers the old block (a gVisor pod's 16000-65535, or one from 15500) every
# default port lies below it, Linux's default range keeps the blocks at 15000, and a range with no
# room for 2000 ports above 1023 is refused naming both ranges unless every port variable is set.
#
# And of test-lib.sh's end_tree, which every suite's exit runs over the processes it recorded: (k)
# it sees a process stopped before it reads its children, so it ends a child forked while the STOP
# was pending; (l) it reads on past a zombie child and ends the live child after it; (m) it names a
# process it cannot see stopped and still ends it and the rest of the tree; (n) it refuses a token
# that is no pid by name and signals nothing.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly helper="${here}/common.sh"
# What the suites share about a process a case has to see end (process_ended, wait_gone).
# shellcheck source=packages/envoy/loadtest/test-lib.sh
source "${here}/test-lib.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly locker="${temporary_dir}/locker.sh"
readonly slow_bin="${temporary_dir}/slow-bin"
readonly elsewhere="${temporary_dir}/elsewhere.sh"
readonly slow_flock="${temporary_dir}/slow-flock"
# A PID namespace that no process has: a record that names it was written in another namespace.
readonly foreign_pidns="pid:[0]"

pids=()
cleanup() {
  local pid
  for pid in "${pids[@]}"; do
    # no-wait: the suite's own exit. The wait below reaps this shell's children, and nothing
    # reads the rest after it.
    kill -TERM "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

# locker.sh HELPER ROOT PIDFILE [CHILD_PIDFILE]: take ROOT's lock, sourcing HELPER as the scripts
# do, write this pid to PIDFILE once admitted, then sleep. With CHILD_PIDFILE it first starts a
# child that does the same with the inherited token and descriptor 9, as run-series.sh starts its
# run-step.sh.
cat >"$locker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=/dev/null
source "$1"
lock_root "$2"
if [ $# -ge 4 ]; then
  bash "${BASH_SOURCE[0]}" "$1" "$2" "$4" &
fi
echo "$$" >"$3.tmp"
mv -f -- "$3.tmp" "$3"
exec sleep 300
EOF

# elsewhere.sh ROOT PID READY SECONDS [PIDNS]: hold ROOT's lock as a run in another PID namespace
# looks from here: on descriptor 7, which lock_holders does not look at, under a complete record that
# names PID, and PIDNS as its PID namespace when given. Write READY once the record is in the file,
# then hold the lock for SECONDS.
cat >"$elsewhere" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
exec 7<>"$1/.run.lock"
flock 7
pidns=""
[ -z "${5:-}" ] || pidns="pidns=$5"$'\n'
printf 'pid=%s\n%scommand=a run in another PID namespace\nstarted=2026-10-27T00:00:00Z\ntoken=0123456789abcdef0123456789abcdef\n' \
  "$2" "$pidns" >"$1/.run.lock"
echo ready >"$3"
exec sleep "$4"
EOF

# A `flock` whose first call creates the directory LOADTEST_TEST_FLOCK_MARK and then takes 4 s before
# it tries: a waiting caller with it on PATH keeps the lock file open, on the descriptor that it
# tries, through another caller's whole 3 s wait.
mkdir -p "$slow_flock"
real_flock="$(command -v flock)"
readonly real_flock
cat >"${slow_flock}/flock" <<EOF
#!/usr/bin/env bash
if mkdir "\${LOADTEST_TEST_FLOCK_MARK:?}" 2>/dev/null; then
  sleep 4
fi
exec "${real_flock}" "\$@"
EOF
chmod +x "${slow_flock}/flock"

# A `date` that takes 1 s: lock_root calls it after its flock and before it writes its record, so
# a holder with it on PATH holds the lock for 1 s without its own record in the file.
mkdir -p "$slow_bin"
cat >"${slow_bin}/date" <<'EOF'
#!/usr/bin/env bash
: >"${LOADTEST_TEST_DATE_MARK:?}"
sleep 1
exec "${LOADTEST_TEST_REAL_DATE:?}" "$@"
EOF
chmod +x "${slow_bin}/date"
real_date="$(command -v date)"
readonly real_date

# wait_for FILE: wait up to 10 s for FILE to exist.
wait_for() {
  local i
  for ((i = 0; i < 100; i++)); do
    [[ -s "$1" ]] && return 0
    sleep 0.1
  done
  return 1
}

# try_lock ROOT [NAME=VALUE...]: call lock_root on ROOT in a new process that holds no descriptor 9
# and no token unless one is given. Sets rc and out (its output, ADMITTED when it was admitted).
try_lock() {
  local root=$1
  shift
  rc=0
  out="$(env -u LOADTEST_LOCK_TOKEN "$@" bash -c 'source "$1"; lock_root "$2"; echo ADMITTED' caller \
    "$helper" "$root" 2>&1 9>&-)" || rc=$?
}

# names TEXT PID: TEXT names "pid PID".
names() { [[ $1 =~ pid\ $2([^0-9]|$) ]]; }

# dead_pid: the pid of a process that has exited and been reaped.
dead_pid() { bash -c 'echo $$'; }

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  [[ -z "${out:-}" ]] || printf '  caller output:\n%s\n' "$out" >&2
  return 1
}

case_orphaned_child() {
  out=""
  local root="${temporary_dir}/root-a"
  bash "$locker" "$helper" "$root" "${temporary_dir}/holder-a" "${temporary_dir}/child-a" \
    >"${temporary_dir}/holder-a.out" 2>&1 &
  local holder=$!
  pids+=("$holder")
  wait_for "${temporary_dir}/child-a" || { out="$(cat "${temporary_dir}/holder-a.out")" fail "(a) the holder's child was not admitted"; return; }
  local child
  child="$(<"${temporary_dir}/child-a")"
  pids+=("$child")
  kill -TERM "$holder"
  wait "$holder" 2>/dev/null || true
  ! process_ended "$child" || { fail "(a) the child ended with its holder"; return; }

  try_lock "$root"
  if ((rc == 0)); then
    fail "(a) a caller was admitted while the child of the killed holder (pid $child) still runs: the child held no lock, so on a live stack a new run-step.sh would reset the stack under a running series step"
    return
  fi
  names "$out" "$holder" || { fail "(a) the refusal does not name the holder's record (pid $holder)"; return; }
  names "$out" "$child" || { fail "(a) the refusal does not name the live child that holds the lock (pid $child)"; return; }

  kill -TERM "$child"
  wait_gone "$child" || { fail "(a) the child (pid $child) did not end"; return; }
  try_lock "$root"
  if ((rc != 0)) || [[ $out != *ADMITTED* ]]; then
    fail "(a) a caller was refused after the child of the killed holder had exited"
    return
  fi
  out=""
  printf 'PASS: a holder'"'"'s child keeps the lock after the holder is killed, and the root is free once the child exits\n'
}

case_stale_record() {
  out=""
  local root="${temporary_dir}/root-b"
  bash "$locker" "$helper" "$root" "${temporary_dir}/previous-b" >/dev/null 2>&1 &
  local previous=$!
  pids+=("$previous")
  wait_for "${temporary_dir}/previous-b" || { fail "(b) the previous holder was not admitted"; return; }
  kill -TERM "$previous"
  wait "$previous" 2>/dev/null || true
  grep -q "^pid=${previous}\$" "${root}/.run.lock" || { fail "(b) the ended holder's record is not in the lock file"; return; }

  PATH="${slow_bin}:${PATH}" LOADTEST_TEST_DATE_MARK="${temporary_dir}/date-b" LOADTEST_TEST_REAL_DATE="$real_date" \
    bash "$locker" "$helper" "$root" "${temporary_dir}/holder-b" >"${temporary_dir}/holder-b.out" 2>&1 &
  local holder=$!
  pids+=("$holder")
  local i
  for ((i = 0; i < 100; i++)); do
    [[ -e "${temporary_dir}/date-b" ]] && break
    sleep 0.1
  done
  [[ -e "${temporary_dir}/date-b" ]] || { out="$(cat "${temporary_dir}/holder-b.out")" fail "(b) the new holder did not reach its record"; return; }

  try_lock "$root"
  if ((rc == 0)); then
    fail "(b) a caller was admitted while the new holder (pid $holder) held the lock"
    return
  fi
  if names "$out" "$previous"; then
    fail "(b) the refusal names the previous holder (pid $previous), which has ended, instead of the live holder (pid $holder)"
    return
  fi
  names "$out" "$holder" || { fail "(b) the refusal does not name the live holder (pid $holder)"; return; }
  kill -TERM "$holder"
  wait "$holder" 2>/dev/null || true
  out=""
  printf 'PASS: a refusal names the live holder, not the ended holder whose record was still in the file\n'
}

case_token_without_descriptor() {
  out=""
  local root="${temporary_dir}/root-c"
  bash "$locker" "$helper" "$root" "${temporary_dir}/holder-c" >/dev/null 2>&1 &
  local holder=$!
  pids+=("$holder")
  wait_for "${temporary_dir}/holder-c" || { fail "(c) the holder was not admitted"; return; }
  local token
  token="$(sed -n 's/^token=//p' "${root}/.run.lock")"
  [[ -n "$token" ]] || { fail "(c) the lock file holds no token"; return; }

  try_lock "$root" LOADTEST_LOCK_TOKEN="$token"
  if ((rc == 0)); then
    fail "(c) a caller with the holder's token but without its descriptor 9 was admitted while the holder (pid $holder) runs"
    return
  fi
  names "$out" "$holder" || { fail "(c) the refusal does not name the holder (pid $holder)"; return; }
  kill -TERM "$holder"
  wait "$holder" 2>/dev/null || true
  out=""
  printf 'PASS: a caller with the holder'"'"'s token but without its descriptor 9 is refused\n'
}

case_invisible_holder() {
  out=""
  local root="${temporary_dir}/root-d" dead
  mkdir -p "$root"
  dead="$(dead_pid)"
  bash "$elsewhere" "$root" "$dead" "${temporary_dir}/ready-d" 300 >/dev/null 2>&1 &
  local holder=$!
  pids+=("$holder")
  wait_for "${temporary_dir}/ready-d" || { fail "(d) the holder on descriptor 7 did not take the lock"; return; }

  try_lock "$root"
  if ((rc == 0)) || [[ $out == *ADMITTED* ]]; then
    fail "(d) a caller was admitted while a process that this PID namespace cannot see held the lock"
    return
  fi
  names "$out" "$dead" || { fail "(d) the refusal does not name the record's pid ($dead)"; return; }
  [[ $out == *"not visible"* ]] || { fail "(d) the refusal does not say that the holder is not visible here"; return; }
  [[ $out == *.run.lock* && $out == *remove* ]] || { fail "(d) the refusal does not tell the operator to leave .run.lock in place"; return; }
  [[ -e "${root}/.run.lock" ]] || { fail "(d) the lock file is gone"; return; }
  kill -TERM "$holder"
  wait "$holder" 2>/dev/null || true
  out=""
  printf 'PASS: a lock held by a process that this PID namespace cannot see is refused as held, not as free\n'
}

case_holder_ended() {
  out=""
  local root="${temporary_dir}/root-e" dead
  mkdir -p "$root"
  dead="$(dead_pid)"
  printf 'pid=%s\ncommand=an ended run\nstarted=2026-10-27T00:00:00Z\ntoken=0123456789abcdef0123456789abcdef\n' \
    "$dead" >"${root}/.run.lock"
  try_lock "$root"
  if ((rc != 0)) || [[ $out != *ADMITTED* ]]; then
    fail "(e) a caller was refused on a root whose record names a dead pid and whose lock no process holds"
    return
  fi

  # The holder ends 1 s in, while the caller waits for a live record.
  bash "$elsewhere" "$root" "$dead" "${temporary_dir}/ready-e" 1 >/dev/null 2>&1 &
  local holder=$!
  pids+=("$holder")
  wait_for "${temporary_dir}/ready-e" || { fail "(e) the holder on descriptor 7 did not take the lock"; return; }
  try_lock "$root"
  if ((rc != 0)) || [[ $out != *ADMITTED* ]]; then
    fail "(e) a caller was refused after the holder (pid $holder) ended during its wait"
    return
  fi
  wait "$holder" 2>/dev/null || true
  out=""
  printf 'PASS: a caller takes the lock once its holder has ended, the record naming a dead pid\n'
}

case_waiting_callers() {
  out=""
  local root="${temporary_dir}/root-f" dead
  mkdir -p "$root"
  dead="$(dead_pid)"
  bash "$elsewhere" "$root" "$dead" "${temporary_dir}/ready-f" 300 "$foreign_pidns" >/dev/null 2>&1 &
  local holder=$!
  pids+=("$holder")
  wait_for "${temporary_dir}/ready-f" || { fail "(f) the holder on descriptor 7 did not take the lock"; return; }

  # Caller 1 sits in its first flock for 4 s; caller 2 starts once caller 1 is in it, and waits its
  # whole 3 s while caller 1 has the lock file open.
  local mark="${temporary_dir}/flock-f" n i waiters=() rcs=()
  for n in 1 2; do
    local path="$PATH"
    [[ $n == 2 ]] || path="${slow_flock}:${PATH}"
    env -u LOADTEST_LOCK_TOKEN PATH="$path" LOADTEST_TEST_FLOCK_MARK="$mark" \
      bash -c 'echo "$$" >"$3"; source "$1"; lock_root "$2"' caller \
      "$helper" "$root" "${temporary_dir}/caller-f${n}.pid" >"${temporary_dir}/caller-f${n}.out" 2>&1 9>&- &
    waiters+=("$!")
    pids+=("$!")
    for ((i = 0; i < 100 && n == 1; i++)); do
      [[ -d "$mark" ]] && break
      sleep 0.1
    done
    [[ -d "$mark" ]] || { fail "(f) caller 1 did not reach its flock"; return; }
  done
  for n in 0 1; do
    rcs[n]=0
    wait "${waiters[n]}" 2>/dev/null || rcs[n]=$?
  done
  local caller1 caller2 out1 out2
  caller1="$(<"${temporary_dir}/caller-f1.pid")"
  caller2="$(<"${temporary_dir}/caller-f2.pid")"
  out1="$(<"${temporary_dir}/caller-f1.out")"
  out2="$(<"${temporary_dir}/caller-f2.out")"
  for n in 1 2; do
    out="$out1"
    [[ $n == 1 ]] || out="$out2"
    if ((rcs[n - 1] == 0)); then
      fail "(f) caller $n was admitted while a process that this PID namespace cannot see held the lock"
      return
    fi
    [[ $out == *"not visible"* ]] || { fail "(f) caller $n's refusal does not say that the holder is not visible here"; return; }
    [[ $out == *.run.lock* && $out == *remove* ]] || { fail "(f) caller $n's refusal does not tell the operator to leave .run.lock in place"; return; }
  done
  out="$out1"
  if names "$out" "$caller2"; then
    fail "(f) caller 1's refusal names caller 2 (pid $caller2), which only waited, as the holder"
    return
  fi
  out="$out2"
  if names "$out" "$caller1"; then
    fail "(f) caller 2's refusal names caller 1 (pid $caller1), which only waited, as the holder"
    return
  fi
  kill -TERM "$holder"
  wait "$holder" 2>/dev/null || true
  out=""
  printf 'PASS: two callers that wait at once on a holder this PID namespace cannot see never name each other\n'
}

case_foreign_record_live_pid() {
  out=""
  local root="${temporary_dir}/root-g"
  mkdir -p "$root"
  bash "$elsewhere" "$root" "$$" "${temporary_dir}/ready-g" 300 "$foreign_pidns" >/dev/null 2>&1 &
  local holder=$!
  pids+=("$holder")
  wait_for "${temporary_dir}/ready-g" || { fail "(g) the holder on descriptor 7 did not take the lock"; return; }

  try_lock "$root"
  if ((rc == 0)) || [[ $out == *ADMITTED* ]]; then
    fail "(g) a caller was admitted while a process that this PID namespace cannot see held the lock"
    return
  fi
  [[ $out == *"not visible"* ]] || { fail "(g) the refusal does not say that the holder is not visible here, although the record was written in another PID namespace"; return; }
  if names "$out" "$$"; then
    fail "(g) the refusal names pid $$ (this test, an unrelated process here) as the holder: the record's pid is of another PID namespace"
    return
  fi
  [[ $out == *.run.lock* && $out == *remove* ]] || { fail "(g) the refusal does not tell the operator to leave .run.lock in place"; return; }
  kill -TERM "$holder"
  wait "$holder" 2>/dev/null || true
  out=""
  printf 'PASS: a record of another PID namespace whose pid runs here is refused as not visible, not naming that pid\n'
}

# case_prepare_stack_built_migration: prepare_stack compares the seed's migration with the Dispatch
# that setup built (bin/envoy-dispatch.json), not with the checkout's migration files, so a seed a
# newer Dispatch made is accepted for a build of that newer Dispatch and refused for an older one.
case_prepare_stack_built_migration() {
  local dir="${temporary_dir}/prepare" rc
  mkdir -p "$dir/lib" "$dir/src/packages/envoy/internal/dispatch/store/migrations" "$dir/root/bin"
  cp -- "$helper" "$dir/lib/common.sh"
  printf '#!/usr/bin/env bash\n[ "$1" = build-info ] && echo "abc false"\nexit 0\n' >"$dir/lib/stack.sh"
  chmod +x "$dir/lib/stack.sh"
  : >"$dir/src/packages/envoy/internal/dispatch/store/migrations/0129_checkout.up.sql"
  printf '{"migration":134,"build_commit":"seedmaker"}\n' >"$dir/root/seed.meta.json"
  printf '{"newest_migration":135,"dispatch_commit":"newer"}\n' >"$dir/root/bin/envoy-dispatch.json"
  out=$(LOADTEST_SRC="$dir/src" LOADTEST_ROOT="$dir/root" bash -c 'source "$1"; resolve_paths; prepare_stack' _ "$dir/lib/common.sh" 2>&1) && rc=0 || rc=$?
  ((rc == 0)) || { fail "(h) prepare_stack refused a seed at 134 for a built Dispatch at 135 (the checkout's files stop at 129)"; return; }
  printf '{"newest_migration":113,"dispatch_commit":"older"}\n' >"$dir/root/bin/envoy-dispatch.json"
  out=$(LOADTEST_SRC="$dir/src" LOADTEST_ROOT="$dir/root" bash -c 'source "$1"; resolve_paths; prepare_stack' _ "$dir/lib/common.sh" 2>&1) && rc=0 || rc=$?
  ((rc != 0)) || { fail "(h) prepare_stack accepted a seed at 134 for a built Dispatch at 113"; return; }
  [[ $out == *"Dispatch older"* ]] || { fail "(h) the refusal does not name the built Dispatch commit"; return; }
  out=""
  printf 'PASS: prepare_stack compares the seed with the built Dispatch'"'"'s migrations, not the checkout'"'"'s\n'
}

# case_prepare_stack_migration_set: with both records holding their sets, prepare_stack refuses a
# build that lacks a version the seed holds even when the highest versions are the same (a
# migration merged below the highest), and accepts one that holds the seed's set.
case_prepare_stack_migration_set() {
  local dir="${temporary_dir}/prepare-set" rc
  mkdir -p "$dir/lib" "$dir/src/packages/envoy/internal/dispatch/store/migrations" "$dir/root/bin"
  cp -- "$helper" "$dir/lib/common.sh"
  printf '#!/usr/bin/env bash\n[ "$1" = build-info ] && echo "abc false"\nexit 0\n' >"$dir/lib/stack.sh"
  chmod +x "$dir/lib/stack.sh"
  printf '{"migration":20261030110000,"migrations":[1,2,20261030100000,20261030110000],"build_commit":"seedmaker"}\n' >"$dir/root/seed.meta.json"
  printf '{"newest_migration":20261030110000,"migrations":[1,2,20261030110000],"dispatch_commit":"lacking"}\n' >"$dir/root/bin/envoy-dispatch.json"
  out=$(LOADTEST_SRC="$dir/src" LOADTEST_ROOT="$dir/root" bash -c 'source "$1"; resolve_paths; prepare_stack' _ "$dir/lib/common.sh" 2>&1) && rc=0 || rc=$?
  ((rc != 0)) || { fail "(i) prepare_stack accepted a build lacking 20261030100000, which the seed holds below a shared highest"; return; }
  [[ $out == *"Dispatch lacking"* && $out == *20261030100000* ]] || { fail "(i) the refusal does not name the build and the missing version"; return; }
  printf '{"newest_migration":20261030110000,"migrations":[1,2,20261030100000,20261030110000],"dispatch_commit":"whole"}\n' >"$dir/root/bin/envoy-dispatch.json"
  out=$(LOADTEST_SRC="$dir/src" LOADTEST_ROOT="$dir/root" bash -c 'source "$1"; resolve_paths; prepare_stack' _ "$dir/lib/common.sh" 2>&1) && rc=0 || rc=$?
  ((rc == 0)) || { fail "(i) prepare_stack refused a build holding the seed's whole set: $out"; return; }
  out=""
  printf 'PASS: prepare_stack compares the seed'"'"'s set of migrations with the built Dispatch'"'"'s set\n'
}

# ports_for ROOT [NAME=VALUE...]: ROOT's ports, "KEY PORT" a line, from common.sh, in a shell that
# holds no LOADTEST_*_PORT but the ones given.
ports_for() {
  local root=$1
  shift
  env -u LOADTEST_PG_PORT -u LOADTEST_PROXY_PORT -u LOADTEST_PROXY_STATS_PORT -u LOADTEST_NATS_PORT \
    -u LOADTEST_NATS_MONITOR_PORT -u LOADTEST_LISTENER_PORT -u LOADTEST_DISPATCH_PORT \
    -u LOADTEST_PPROF_PORT -u LOADTEST_ENVOY_PROXY_PORT "$@" bash -c 'source "$1"; stack_ports "$2"' ports "$helper" "$root"
}

# case_stack_ports: the stack's default ports follow LOADTEST_ROOT, so two Legion roles of one pod,
# each on its own root, almost always start a stack each with no port variable set: each root's path
# picks one of the 200 blocks of ports, and no two ports of one block or of two different blocks
# meet. Two roots whose paths pick one block (about 1 pair in 200) hold the same 9 ports; the two
# roots below do not.
case_stack_ports() {
  out=""
  local one two again overlap
  one=$(ports_for /legion/scratch/tmp/lt-one)
  two=$(ports_for /legion/scratch/tmp/lt-two)
  again=$(ports_for /legion/scratch/tmp/lt-one)
  [ "$one" = "$again" ] || { out="$one"$'\n'"$again" fail "(i) one root got two sets of ports"; return; }
  [ "$one" != "$two" ] || { out="$one" fail "(i) two roots got the same ports"; return; }
  overlap=$(comm -12 <(awk '{print $2}' <<<"$one" | sort) <(awk '{print $2}' <<<"$two" | sort))
  [ -z "$overlap" ] || { out="$one"$'\n'"$two" fail "(i) two roots share the ports $(tr '\n' ' ' <<<"$overlap")"; return; }
  [ "$(wc -l <<<"$one")" = 9 ] || { out="$one" fail "(i) a root's ports are not the 9 stack.json keys"; return; }

  # Every port of every block, against every other: all inside 1024-65535, none twice.
  local report
  report=$(bash -c '
    source "$1"
    declare -A seen=()
    bad=""
    base=$(port_base)
    for ((k = 0; k < PORT_BLOCKS; k++)); do
      for key in "${PORT_KEYS[@]}"; do
        p=$(default_port "$key" "$k" "$base")
        ((p >= 1024 && p <= 65535)) || bad+="block $k $key is on port $p, outside 1024-65535; "
        ! port_in_ephemeral "$p" || bad+="block $k $key is on port $p, inside the ephemeral range $(ephemeral_range); "
        [ -z "${seen[$p]:-}" ] || bad+="block $k $key and ${seen[$p]} are both on port $p; "
        seen[$p]="block $k $key"
      done
    done
    printf "%s" "$bad"' blocks "$helper")
  [ -z "$report" ] || { fail "(i) the port blocks do not hold: $report"; return; }

  # A variable moves its own port alone.
  local set_pg
  set_pg=$(ports_for /legion/scratch/tmp/lt-one LOADTEST_PG_PORT=23456)
  [ "$(awk '$1 == "postgres" {print $2}' <<<"$set_pg")" = 23456 ] ||
    { out="$set_pg" fail "(i) LOADTEST_PG_PORT did not set the Postgres port"; return; }
  [ "$(grep -v '^postgres ' <<<"$set_pg")" = "$(grep -v '^postgres ' <<<"$one")" ] ||
    { out="$set_pg" fail "(i) LOADTEST_PG_PORT moved another port than Postgres'"; return; }
  out=""
  printf 'PASS: the default ports follow LOADTEST_ROOT, two roots in two blocks share no port, and each LOADTEST_*_PORT overrides its own\n'
}

# range_file LOW HIGH: a file holding an ephemeral range as /proc/sys/net/ipv4/ip_local_port_range
# does, for LOADTEST_PORT_RANGE_FILE.
range_file() {
  local file="${temporary_dir}/range-$1-$2"
  printf '%s\t%s\n' "$1" "$2" >"$file"
  printf '%s\n' "$file"
}

# case_ports_below_ephemeral_range: every default port lies below the machine's ephemeral range, so
# no outgoing connection of the stack can take a stack port as its source port first. Linux's
# default range (32768-60999) leaves the blocks at 15000-16998; a gVisor pod's (16000-65535), and
# one that covers the old block from 15500, move them to the 2000 ports below the range; a range
# starting too low for 2000 ports above 1023 is refused naming both ranges, unless every port
# variable is set; and a range this machine cannot read leaves the blocks at 15000.
case_ports_below_ephemeral_range() {
  out=""
  local root=/legion/scratch/tmp/lt-one ports low rc max
  for low in 16000 15500; do
    ports=$(ports_for "$root" LOADTEST_PORT_RANGE_FILE="$(range_file "$low" 65535)") ||
      { out="$ports" fail "(j) a range from $low refused the stack's ports"; return; }
    max=$(awk '{print $2}' <<<"$ports" | sort -n | tail -1)
    ((max < low)) || { out="$ports" fail "(j) a range from $low left port $max inside it"; return; }
    (($(awk '{print $2}' <<<"$ports" | sort -n | head -1) >= low - 2000)) ||
      { out="$ports" fail "(j) a range from $low put a port more than 2000 below it"; return; }
  done
  local linux unread
  linux=$(ports_for "$root" LOADTEST_PORT_RANGE_FILE="$(range_file 32768 60999)")
  unread=$(ports_for "$root" LOADTEST_PORT_RANGE_FILE="${temporary_dir}/no-such-range")
  [ "$linux" = "$unread" ] || { out="$linux"$'\n'"$unread" fail "(j) Linux's default range moved the blocks from 15000"; return; }
  (($(awk '$1 == "postgres" {print $2}' <<<"$linux") % 10 == 0 && $(awk '$1 == "postgres" {print $2}' <<<"$linux") >= 15000)) ||
    { out="$linux" fail "(j) Linux's default range did not keep the blocks at 15000"; return; }
  out=$(ports_for "$root" LOADTEST_PORT_RANGE_FILE="$(range_file 1500 65535)" 2>&1) && rc=0 || rc=$?
  ((rc != 0)) || { fail "(j) a range from 1500 left room for 2000 ports above 1023"; return; }
  [[ $out == *"1500-65535"* && $out == *"2000 ports"* ]] || { fail "(j) the refusal does not name the range and the ports the stack needs"; return; }
  out=$(ports_for "$root" LOADTEST_PORT_RANGE_FILE="$(range_file 1500 65535)" LOADTEST_PG_PORT=1100 \
    LOADTEST_PROXY_PORT=1101 LOADTEST_PROXY_STATS_PORT=1102 LOADTEST_NATS_PORT=1103 LOADTEST_NATS_MONITOR_PORT=1104 \
    LOADTEST_LISTENER_PORT=1105 LOADTEST_DISPATCH_PORT=1106 LOADTEST_PPROF_PORT=1107 LOADTEST_ENVOY_PROXY_PORT=1108 2>&1) ||
    { fail "(j) every port variable set, a range from 1500 still refused the stack"; return; }
  out=""
  printf 'PASS: the default ports sit below the ephemeral range, and a range with no room below it is refused naming both\n'
}

# case_end_tree_stops_before_walk: a child that the recorded parent forks while its STOP is still
# pending is part of the tree end_tree must read and end. The case orders that on events, never on
# a nap. The kill wrapper sends the parent no STOP when end_tree asks for one: it marks the ask.
# The parent forks its child only once the walk has napped after that ask -- the nap of a walk
# that waits to see the parent stopped, which the case's sleep marks -- and then stops itself. So a
# walk that confirms the stop always meets the child, however late the parent runs, and a walk that
# reads the children as soon as its STOP is sent never does: it sends its SIGKILL before the fork,
# which the wrapper notes, and the case fails on that line of its own. end_tree gets 60 s for the
# parent to stop, far above the parent's own steps.
case_end_tree_stops_before_walk() {
  local parent parent_start child mark=$temporary_dir/tree real_nap=$TEST_LIB_SLEEP rc=0
  rm -f -- "$mark"-*
  bash -c 'until [ -e "$2-walk-naps" ]; do "$1" 0.01; done; "$1" 300 & echo "$!" >"$2-child"; kill -STOP "$$"; wait' _ "$real_nap" "$mark" &
  parent=$!
  parent_start=$(process_start "$parent")
  [ -n "$parent_start" ] || { fail "(k) the tree parent had no start time"; return; }
  pids+=("$parent")
  # shellcheck disable=SC2329 # end_tree, which this case runs, calls it in place of the builtin
  kill() {
    if [ "$1" = -STOP ] && [ "${2:-}" = "$parent" ]; then
      : >"$mark-stop-asked"
      return 0
    fi
    if [ "$1" = -KILL ] && [ "${2:-}" = "$parent" ] && [ ! -s "$mark-child" ]; then
      : >"$mark-killed-before-fork"
    fi
    command kill "$@" # no-wait: a pass-through; end_tree, whose kill it is, waits for each process
  }
  # The walk's nap, end_tree's TEST_LIB_SLEEP for this case: it marks a nap taken once the STOP was
  # asked for, and then naps.
  # shellcheck disable=SC2016 # expanded by the nap script it writes
  printf '#!/usr/bin/env bash\n[ ! -e %q ] || : >%q\nexec %q "$@"\n' "$mark-stop-asked" "$mark-walk-naps" "$real_nap" >"$mark-nap"
  chmod +x "$mark-nap"
  TEST_LIB_SLEEP=$mark-nap
  end_tree "$parent" 60 2>"$mark-err" || rc=$?
  TEST_LIB_SLEEP=$real_nap
  unset -f kill
  if [ -e "$mark-killed-before-fork" ]; then
    end_process "$parent" || true
    fail "(k) end_tree sent the parent its SIGKILL before the child its pending STOP let it fork: it read the parent's children before it saw the parent stopped"
    return
  fi
  ((rc == 0)) || {
    end_process "$parent" || true
    fail "(k) end_tree returned $rc for a parent that stopped itself once forked: $(<"$mark-err")"
    return
  }
  [ -s "$mark-child" ] || { fail "(k) the parent did not fork its delayed child"; return; }
  child=$(<"$mark-child")
  pids+=("$child")
  if ! process_ended "$child"; then
    end_process "$child" || true
    fail "(k) end_tree left the child (pid $child) that the parent forked while its STOP was pending"
    return
  fi
  wait "$parent" 2>/dev/null || true
  printf 'PASS: end_tree confirms a process is stopped before it reads and ends children it can still fork\n'
}

# case_end_tree_zombie_child: a child that has ended and that no process reaps -- a zombie, which
# never reads stopped -- is no reason to give up the walk: end_tree reads on past it, and ends the
# live child after it and the parent. The parent is a sleep that a shell became by exec once it had
# started its two children, so nothing reaps the first once it ends, which it does on a mark this
# case makes once the parent is the sleep. The children file lists the zombie first.
case_end_tree_zombie_child() {
  local mark=$temporary_dir/zombie parent zombie live t0 comm rc=0
  rm -f -- "$mark"-*
  # shellcheck disable=SC2016 # expanded by the shell it starts
  bash -c 'bash -c "until [ -e \"\$1\" ]; do \"\$2\" 0.01; done" _ "$2-go" "$1" & z=$!; "$1" 300 & echo "$z $!" >"$2-kids"; exec "$1" 600' _ "$TEST_LIB_SLEEP" "$mark" &
  parent=$!
  pids+=("$parent")
  wait_for "$mark-kids" || { fail "(l) the tree parent wrote no children within 10 s"; return; }
  read -r zombie live <"$mark-kids"
  pids+=("$zombie" "$live")
  t0=$EPOCHSECONDS comm=""
  until [ "$comm" = sleep ]; do
    ((EPOCHSECONDS - t0 <= 10)) || { fail "(l) the tree parent did not become its sleep within 10 s"; return; }
    "$TEST_LIB_SLEEP" 0.05
    { read -r comm <"/proc/$parent/comm"; } 2>/dev/null || comm=""
  done
  : >"$mark-go"
  t0=$EPOCHSECONDS
  until process_ended "$zombie"; do
    ((EPOCHSECONDS - t0 <= 10)) || { fail "(l) the first child (pid $zombie) did not end within 10 s of its mark"; return; }
    "$TEST_LIB_SLEEP" 0.05
  done
  end_tree "$parent" 2>"$mark-err" || rc=$?
  if ! process_ended "$live" || ! process_ended "$parent"; then
    end_process "$live" || true
    end_process "$parent" || true
    fail "(l) end_tree returned $rc and left the parent (pid $parent) or its live child (pid $live) behind a zombie child (pid $zombie): $(<"$mark-err")"
    return
  fi
  ((rc == 0)) || { fail "(l) end_tree ended a tree with a zombie in it but returned $rc: $(<"$mark-err")"; return; }
  wait "$parent" 2>/dev/null || true
  printf 'PASS: end_tree reads on past a zombie child, which has ended, and ends the live child after it and the parent\n'
}

# case_end_tree_names_unconfirmed: a process that end_tree cannot see stopped is named, and the
# walk goes on: that process still gets the SIGKILL, and so do its children and every other process
# of the tree. Here stop_process, end_tree's own, answers that it could not see the middle one of
# three stopped, after its STOP went out as ever.
case_end_tree_names_unconfirmed() {
  local mark=$temporary_dir/unconfirmed parent middle below p rc=0
  rm -f -- "$mark"-*
  # shellcheck disable=SC2016 # expanded by the shell it starts
  bash -c '( "$1" 300 & echo "$BASHPID $!" >"$2-tree.tmp"; mv -- "$2-tree.tmp" "$2-tree"; wait ) & wait' _ "$TEST_LIB_SLEEP" "$mark" &
  parent=$!
  pids+=("$parent")
  wait_for "$mark-tree" || { fail "(m) the tree parent wrote no tree within 10 s"; return; }
  read -r middle below <"$mark-tree"
  pids+=("$middle" "$below")
  eval "orig_$(declare -f stop_process)"
  stop_process() {
    local rc=0
    orig_stop_process "$@" || rc=$?
    [ "$1" != "$middle" ] || return 1
    return "$rc"
  }
  end_tree "$parent" 2>"$mark-err" || rc=$?
  eval "$(declare -f orig_stop_process | sed 1s/orig_stop_process/stop_process/)"
  unset -f orig_stop_process
  for p in "$parent" "$middle" "$below"; do
    process_ended "$p" && continue
    kill -CONT "$p" 2>/dev/null || true
    end_process "$p" || true
    fail "(m) end_tree left pid $p of a tree whose middle process (pid $middle) it could not see stopped: $(<"$mark-err")"
    return
  done
  ((rc == 1)) || { fail "(m) end_tree returned $rc, not 1, for a tree with a process it could not see stopped"; return; }
  grep -qF "end_tree: pid $middle, below pid $parent, was not seen stopped by the walk's deadline, 10 s from its start; it is sent the SIGKILL with the rest, and its children are read" "$mark-err" ||
    { fail "(m) end_tree did not name the process it could not see stopped (pid $middle): $(<"$mark-err")"; return; }
  wait "$parent" 2>/dev/null || true
  printf 'PASS: end_tree names a process it cannot see stopped and still ends it, its children and the rest of the tree\n'
}

# case_end_tree_refuses_no_pid: end_tree refuses a token that is no pid by name and signals
# nothing: `kill -STOP 0` would stop every process of the caller's process group. The caller runs in
# a session of its own, so a fault stops that shell alone, which the case sees as its state T; the
# case then ends it. The case decides by those events, never by a clock: the caller's status file
# (end_tree returned), its state T (end_tree stopped it), or the caller ended with no status
# written (end_tree killed it). A machine that runs the caller late only makes the event come
# later. The 600 s is a hang's backstop, named as one, far above a source and two file writes.
case_end_tree_refuses_no_pid() {
  local mark=$temporary_dir/no-pid job pid t0 state rc
  rm -f -- "$mark"-*
  # shellcheck disable=SC2016 # expanded by the shell it starts
  setsid bash -c 'echo "$$" >"$2-pid.tmp"; mv -- "$2-pid.tmp" "$2-pid"; source "$1"; rc=0; end_tree 0 2>"$2-err" || rc=$?; echo "$rc" >"$2-rc"' _ "$here/test-lib.sh" "$mark" &
  job=$!
  t0=$EPOCHSECONDS
  until [ -s "$mark-pid" ]; do
    if process_ended "$job"; then
      wait "$job" 2>/dev/null || true
      fail "(n) the caller (pid $job) ended before it wrote its pid"
      return
    fi
    ((EPOCHSECONDS - t0 <= 600)) || { fail "(n) the caller (pid $job) wrote no pid within 600 s, the case's backstop for a hang"; return; }
    "$TEST_LIB_SLEEP" 0.05
  done
  pid=$(<"$mark-pid")
  until [ -s "$mark-rc" ]; do
    state=$(process_state "$pid")
    if [ "$state" = T ]; then
      end_process "$pid" || true
      wait "$job" 2>/dev/null || true
      fail "(n) end_tree 0 stopped its caller (pid $pid), which reads state T: it signalled process group 0"
      return
    fi
    if process_ended "$pid" && [ ! -s "$mark-rc" ]; then
      wait "$job" 2>/dev/null || true
      fail "(n) end_tree 0 ended its caller (pid $pid) before the caller wrote its status: it signalled process group 0"
      return
    fi
    if ((EPOCHSECONDS - t0 > 600)); then
      end_process "$pid" || true
      wait "$job" 2>/dev/null || true
      fail "(n) end_tree 0 neither returned nor stopped its caller (pid $pid, state ${state:-gone}) within 600 s, the case's backstop for a hang"
      return
    fi
    "$TEST_LIB_SLEEP" 0.05
  done
  wait "$job" 2>/dev/null || true
  rc=$(<"$mark-rc")
  [ "$rc" = 1 ] || { fail "(n) end_tree 0 returned $rc, not 1"; return; }
  grep -qxF "end_tree: refusing invalid process id 0" "$mark-err" ||
    { fail "(n) end_tree 0 did not refuse by name: $(<"$mark-err")"; return; }
  printf 'PASS: end_tree refuses a token that is no pid by name and signals nothing\n'
}

failed=0
case_orphaned_child || failed=$((failed + 1))
case_stale_record || failed=$((failed + 1))
case_token_without_descriptor || failed=$((failed + 1))
case_invisible_holder || failed=$((failed + 1))
case_holder_ended || failed=$((failed + 1))
case_waiting_callers || failed=$((failed + 1))
case_foreign_record_live_pid || failed=$((failed + 1))
case_prepare_stack_built_migration || failed=$((failed + 1))
case_prepare_stack_migration_set || failed=$((failed + 1))
case_stack_ports || failed=$((failed + 1))
case_end_tree_stops_before_walk || failed=$((failed + 1))
case_end_tree_zombie_child || failed=$((failed + 1))
case_end_tree_names_unconfirmed || failed=$((failed + 1))
case_end_tree_refuses_no_pid || failed=$((failed + 1))
case_ports_below_ephemeral_range || failed=$((failed + 1))
if ((failed > 0)); then
  printf 'FAIL: %d of 15 cases failed\n' "$failed" >&2
  exit 1
fi
