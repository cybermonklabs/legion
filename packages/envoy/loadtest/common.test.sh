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
# roots in general share all 10 when their paths hash into one block of the 200, which stack.sh
# refuses), every port of every block lies in 1024-65535, outside this machine's ephemeral range,
# and no two ports of two blocks meet, and a LOADTEST_*_PORT overrides its own port alone; (j) a
# port inside the machine's ephemeral port range is refused, naming the range, the port and the
# LOADTEST_*_PORT that moves it, while a range file that cannot be read or parsed refuses nothing;
# (k) a CPU list taskset refuses, and two CPU lists that share a CPU, are refused naming them, and
# one list alone passes; (l) a CPU pin is taken to bind only when it slows two busy loops 1.6x
# (affinity_verdict over two given durations, no loop timed), and the refusal gives the
# measurement and names gVisor when /proc/version or dmesg shows it; (m) Dispatch's GOMAXPROCS is
# the server list's CPU count when the list is set, nproc otherwise, a LOADTEST_GOMAXPROCS that
# differs from the count is refused naming both, and a step's meta.json that records a GOMAXPROCS
# other than its list's count is refused; (n) wait_for_window prints the window start of a step
# that opens its window, not the zero time its meta.json holds until then, never takes the window
# of an earlier run left in the step's directory, and returns non-zero within a second of the end
# of a step that ends without a window, giving its not-run record's exit status and reason, or
# saying that it left none; (o) the pin's check (check_cpu_affinity, its timings faked) passes on a
# try after two that fall short, refuses after three that fall short naming all three, keeps no
# verdict of a pin that does not bind, keeps one that binds for the later steps of the same boot,
# which take it without measuring, and measures again on a boot whose id differs or cannot be
# read; (p) the quiet-box gate (wait_for_quiet_box, its readings and pauses faked) starts a step at
# once on a reading at or under LOADTEST_MAX_BUSY_BEFORE (0.10, the bar itself included), pauses
# 30 s and reads again while the box is busier, starting on a quiet reading and naming the ones
# before it, refuses once its pauses reach LOADTEST_QUIET_WAIT (900 s: 31 readings), naming every
# reading, the bar and the wait, follows both variables (75 s: pauses of 30, 30 and 15 s; 0: one
# reading), and refuses a bar, a wait or a reading that is not one; (q) the pod's CPU quota is read
# from cgroup v2's cpu.max, else cgroup v1's cpu.cfs_quota_us and cpu.cfs_period_us, as their text
# and in whole CPUs rounded down (quota_cpus), and `max`, -1 and a root with neither file are no
# quota; (r) beside the server list under a CPU quota, LOADTEST_GENERATOR_CPUS=auto and an unset
# list are the first CPUs of the pod outside the server list that the quota leaves, at least one (a
# quota of 8 beside 0,1 on a pod of 0-13: six CPUs, none of 0,1), and a given list too wide for the
# quota is refused naming the quota, both lists and auto's list, while one that fits stands;
# without a quota an unset or given list stands as it is and auto is every CPU of the pod outside
# the server list; auto without the server list is refused; (s) Linux's default ephemeral range
# and a gVisor pod's (16000-65535) keep the blocks at 10000, a range that covers that block (one
# from 11000, or from 10500) moves every default port below it, and a range with no room for 2000
# ports above 1023 is refused naming both ranges unless every port variable is set.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly helper="${here}/common.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly locker="${temporary_dir}/locker.sh"
readonly slow_bin="${temporary_dir}/slow-bin"
readonly elsewhere="${temporary_dir}/elsewhere.sh"
readonly slow_flock="${temporary_dir}/slow-flock"
# A cgroup root with no file: a pod with no CPU quota, whose CPUs are this shell's own.
readonly no_cgroup="${temporary_dir}/no-cgroup"
mkdir -p "$no_cgroup"
# A PID namespace that no process has: a record that names it was written in another namespace.
readonly foreign_pidns="pid:[0]"

pids=()
cleanup() {
  local pid
  for pid in "${pids[@]}"; do
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

# alive PID: PID runs and is not a zombie.
alive() {
  local stat
  { read -r stat <"/proc/$1/stat"; } 2>/dev/null || return 1
  [[ ${stat##*) } != Z* ]]
}

# wait_for FILE: wait up to 10 s for FILE to exist.
wait_for() {
  local i
  for ((i = 0; i < 100; i++)); do
    [[ -s "$1" ]] && return 0
    sleep 0.1
  done
  return 1
}

# wait_gone PID: wait up to 10 s for PID to end.
wait_gone() {
  local i
  for ((i = 0; i < 100; i++)); do
    alive "$1" || return 0
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
  alive "$child" || { fail "(a) the child ended with its holder"; return; }

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
    -u LOADTEST_PPROF_PORT -u LOADTEST_METRICS_PORT -u LOADTEST_ENVOY_PROXY_PORT "$@" \
    bash -c 'source "$1"; stack_ports "$2"' ports "$helper" "$root"
}

# case_stack_ports: the stack's default ports follow LOADTEST_ROOT, so two Legion roles of one pod,
# each on its own root, almost always start a stack each with no port variable set: each root's path
# picks one of the 200 blocks of ports, and no two ports of one block or of two different blocks
# meet. Two roots whose paths pick one block (about 1 pair in 200) hold the same 10 ports; the two
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
  [ "$(wc -l <<<"$one")" = 10 ] || { out="$one" fail "(i) a root's ports are not the 10 stack.json keys"; return; }

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

# check_ports_for ROOT [NAME=VALUE...]: check_ephemeral_ports over ROOT's ports, in a shell that
# holds no LOADTEST_*_PORT but the ones given. Sets out to what it printed and rc to its status.
check_ports_for() {
  local root=$1
  shift
  rc=0
  out=$(env -u LOADTEST_PG_PORT -u LOADTEST_PROXY_PORT -u LOADTEST_NATS_PORT \
    -u LOADTEST_NATS_MONITOR_PORT -u LOADTEST_LISTENER_PORT -u LOADTEST_DISPATCH_PORT \
    -u LOADTEST_PPROF_PORT -u LOADTEST_METRICS_PORT "$@" \
    bash -c 'source "$1"; check_ephemeral_ports "$(stack_ports "$2")"' ports "$helper" "$root" 2>&1) || rc=$?
}

# case_ephemeral_ports: a port inside the machine's ephemeral port range is refused before
# anything tries to bind it. The kernel gives such a port to an outbound connection, which is how
# a root in the old 15000 base lost its Dispatch port to the load generator between two steps
# ("bind: address already in use"); the default blocks now lie below every such range. The range
# file is injected through LOADTEST_PORT_RANGE_FILE, so the test writes nothing under /proc,
# and a file it cannot read or parse refuses nothing.
case_ephemeral_ports() {
  out=""
  local range="${temporary_dir}/ip_local_port_range" root=/legion/scratch/tmp/lt-one
  printf '16000\t65535\n' >"$range"

  check_ports_for "$root" LOADTEST_PORT_RANGE_FILE="$range"
  ((rc == 0)) || { fail "(j) a root's default ports were refused against the ephemeral range 16000-65535"; return; }
  [ -z "$out" ] || { fail "(j) the check printed something for ports below the ephemeral range"; return; }

  check_ports_for "$root" LOADTEST_PORT_RANGE_FILE="$range" LOADTEST_DISPATCH_PORT=16255
  ((rc != 0)) || { out="" fail "(j) a Dispatch port of 16255 passed the ephemeral range 16000-65535"; return; }
  [[ $out == *16000-65535* && $out == *"dispatch 16255"* && $out == *LOADTEST_DISPATCH_PORT* ]] ||
    { fail "(j) the refusal names the range, the port and its variable"; return; }

  check_ports_for "$root" LOADTEST_PORT_RANGE_FILE="${temporary_dir}/no-such-range" LOADTEST_DISPATCH_PORT=16255
  ((rc == 0)) || { fail "(j) a port was refused with no readable range file"; return; }
  printf 'not a range\n' >"$range"
  check_ports_for "$root" LOADTEST_PORT_RANGE_FILE="$range" LOADTEST_DISPATCH_PORT=16255
  ((rc == 0)) || { fail "(j) a port was refused against a range file that holds no two numbers"; return; }
  out=""
  printf 'PASS: a port inside the machine ephemeral range is refused, naming the range, the port and its variable\n'
}

# in_common [NAME=VALUE...] -- CMD...: CMD in a shell that sourced common.sh, without the two CPU
# lists and LOADTEST_GOMAXPROCS but the ones given, and on a cgroup root with no file (no CPU quota)
# unless LOADTEST_CGROUP_ROOT is given. Sets out to what it printed and rc to its status.
in_common() {
  local envs=()
  while [ "$1" != -- ]; do
    envs+=("$1")
    shift
  done
  shift
  rc=0
  out=$(env -u LOADTEST_SERVER_CPUS -u LOADTEST_GENERATOR_CPUS -u LOADTEST_GOMAXPROCS LOADTEST_CGROUP_ROOT="$no_cgroup" "${envs[@]}" \
    bash -c 'source "$1"; shift; "$@"' common "$helper" "$@" 2>&1) || rc=$?
}

# case_cpu_lists: the CPU lists are read as taskset reads them; a list taskset refuses is refused
# naming its variable and taskset's words, and two lists that share a CPU are refused naming both
# and the CPU, before anything starts. One list alone, or none, passes.
case_cpu_lists() {
  in_common -- cpu_list_cpus 0-4:2,7,09
  [ "$(tr '\n' ' ' <<<"$out")" = "0 2 4 7 9 " ] || { fail "(k) cpu_list_cpus 0-4:2,7,09 read as '$out', want 0 2 4 7 9"; return; }
  in_common -- check_cpu_lists
  ((rc == 0)) || { fail "(k) no CPU list was refused"; return; }
  in_common LOADTEST_SERVER_CPUS=0 -- check_cpu_lists
  ((rc == 0)) || { fail "(k) LOADTEST_SERVER_CPUS=0 alone was refused"; return; }
  in_common LOADTEST_SERVER_CPUS=x -- check_cpu_lists
  ((rc != 0)) && [[ $out == *"LOADTEST_SERVER_CPUS='x' is a CPU list taskset refuses: taskset: "* ]] ||
    { fail "(k) the list 'x' was not refused naming LOADTEST_SERVER_CPUS and taskset's words"; return; }
  in_common LOADTEST_GENERATOR_CPUS=4095 -- check_cpu_lists
  ((rc != 0)) && [[ $out == *"LOADTEST_GENERATOR_CPUS='4095' is a CPU list taskset refuses"* ]] ||
    { fail "(k) a CPU this box lacks (4095) was not refused"; return; }
  in_common LOADTEST_SERVER_CPUS=0 LOADTEST_GENERATOR_CPUS=0 -- check_cpu_lists
  ((rc != 0)) && [[ $out == *"LOADTEST_SERVER_CPUS='0' and LOADTEST_GENERATOR_CPUS='0' overlap on CPU 0"* ]] ||
    { fail "(k) two lists sharing CPU 0 were not refused naming both"; return; }
  out=""
  printf 'PASS: a CPU list taskset refuses, and two lists that overlap, are refused naming them\n'
}

# cgroup_root NAME [FILE=TEXT...]: a cgroup root of the test's own under NAME, each FILE (a path
# under it) holding the line TEXT. Prints the root.
cgroup_root() {
  local root="${temporary_dir}/cgroup-$1" spec
  shift
  rm -rf "$root"
  mkdir -p "$root"
  for spec in "$@"; do
    mkdir -p "$(dirname "$root/${spec%%=*}")"
    printf '%s\n' "${spec#*=}" >"$root/${spec%%=*}"
  done
  printf '%s\n' "$root"
}

# quota_of ROOT: cpu_quota and quota_cpus on the cgroup root ROOT, into the caller's quota and
# cpus.
quota_of() {
  in_common LOADTEST_CGROUP_ROOT="$1" -- cpu_quota
  quota=$out
  in_common LOADTEST_CGROUP_ROOT="$1" -- quota_cpus
  cpus=$out
}

# case_cpu_quota: the pod's CPU quota is cgroup v2's cpu.max, else cgroup v1's cpu.cfs_quota_us and
# cpu.cfs_period_us, as their text and in whole CPUs rounded down; `max` and -1 are no quota, and so
# is a root with neither file.
case_cpu_quota() {
  local quota cpus
  quota_of "$(cgroup_root v2-8 'cpu.max=800000 100000')"
  [[ $quota == "800000 100000" && $cpus == 8 ]] ||
    { out="" fail "(q) cgroup v2's cpu.max 800000 100000 read as '$quota', '$cpus' CPUs; want 8"; return; }
  quota_of "$(cgroup_root v2-2.5 'cpu.max=250000 100000')"
  [[ $quota == "250000 100000" && $cpus == 2 ]] ||
    { out="" fail "(q) cgroup v2's cpu.max 250000 100000 read as '$quota', '$cpus' CPUs; want 2, rounded down"; return; }
  quota_of "$(cgroup_root v2-max 'cpu.max=max 100000')"
  [[ $quota == "max 100000" && -z $cpus ]] ||
    { out="" fail "(q) cgroup v2's cpu.max max 100000 read as '$quota', '$cpus' CPUs; want no quota"; return; }
  quota_of "$(cgroup_root v1-3 cpu/cpu.cfs_quota_us=300000 cpu/cpu.cfs_period_us=100000)"
  [[ $quota == "300000 100000" && $cpus == 3 ]] ||
    { out="" fail "(q) cgroup v1's quota 300000 per 100000 read as '$quota', '$cpus' CPUs; want 3"; return; }
  quota_of "$(cgroup_root v1-none cpu/cpu.cfs_quota_us=-1 cpu/cpu.cfs_period_us=100000)"
  [[ $quota == "-1 100000" && -z $cpus ]] ||
    { out="" fail "(q) cgroup v1's quota -1 read as '$quota', '$cpus' CPUs; want no quota"; return; }
  quota_of "$no_cgroup"
  [[ -z $quota && -z $cpus ]] || { out="" fail "(q) a root with no cgroup file read as quota '$quota', '$cpus' CPUs"; return; }
  out=""
  printf 'PASS: the CPU quota is read from cgroup v2, else cgroup v1, in whole CPUs rounded down, and max, -1 and no file are none\n'
}

# case_generator_cpus: beside the server list under a CPU quota, auto and an unset generator list
# are the first CPUs of the pod outside the server list that the quota leaves, at least one, and a
# given list too wide for the quota is refused, naming the quota, both lists and auto's list; when
# the server list alone fills the quota auto gives one CPU, and that list passes again as given, as
# a stack.sh does with the list run-step.sh exported. Without a quota an unset or given list stands
# as it is, and auto is every CPU of the pod outside the server list; auto without a server list is
# refused.
case_generator_cpus() {
  local q8 q1 none
  q8=$(cgroup_root q8 'cpu.max=800000 100000' cpuset.cpus.effective=0-13)
  q1=$(cgroup_root q1 'cpu.max=150000 100000' cpuset.cpus.effective=0-3)
  none=$(cgroup_root none 'cpu.max=max 100000' cpuset.cpus.effective=0-3)

  in_common LOADTEST_CGROUP_ROOT="$q8" LOADTEST_SERVER_CPUS=0,1 LOADTEST_GENERATOR_CPUS=auto -- generator_cpus
  ((rc == 0)) && [ "$(tail -n 1 <<<"$out")" = 2,3,4,5,6,7 ] &&
    [[ $out == *"LOADTEST_GENERATOR_CPUS=auto: the generator runs on 2,3,4,5,6,7, 6 CPUs of the pod outside LOADTEST_SERVER_CPUS='0,1', 2 CPUs, so the two lists ask for 8 CPUs against the pod's CPU quota of 8 CPUs ('800000 100000' under ${q8})"* ]] ||
    { fail "(r) auto under a quota of 8 beside 0,1 did not give and name 2,3,4,5,6,7 (rc $rc)"; return; }
  in_common LOADTEST_CGROUP_ROOT="$q8" LOADTEST_SERVER_CPUS=0,1 -- generator_cpus
  ((rc == 0)) && [ "$(tail -n 1 <<<"$out")" = 2,3,4,5,6,7 ] &&
    [[ $out == *"LOADTEST_GENERATOR_CPUS is unset, which beside LOADTEST_SERVER_CPUS under a CPU quota is auto: the generator runs on 2,3,4,5,6,7"* ]] ||
    { fail "(r) an unset list under a quota of 8 beside 0,1 was not auto's 2,3,4,5,6,7 (rc $rc)"; return; }
  in_common LOADTEST_CGROUP_ROOT="$q8" LOADTEST_SERVER_CPUS=0,1 LOADTEST_GENERATOR_CPUS=2-13 -- generator_cpus
  ((rc != 0)) &&
    [[ $out == *"LOADTEST_GENERATOR_CPUS='2-13', 12 CPUs, and LOADTEST_SERVER_CPUS='0,1', 2 CPUs, ask for 14 CPUs, more than the pod's CPU quota of 8 CPUs ('800000 100000' under ${q8}): CFS throttles every process of the pod once together they pass the quota, the pinned server side with them"*"Set LOADTEST_GENERATOR_CPUS=auto, which is '2,3,4,5,6,7' here"* ]] ||
    { fail "(r) the list 2-13 beside 0,1 under a quota of 8 was not refused naming the quota, both lists and auto's (rc $rc)"; return; }
  in_common LOADTEST_CGROUP_ROOT="$q8" LOADTEST_SERVER_CPUS=0,1 LOADTEST_GENERATOR_CPUS=8-13 -- generator_cpus
  ((rc == 0)) && [ "$out" = 8-13 ] || { fail "(r) the list 8-13, six CPUs beside 0,1 under a quota of 8, did not stand (rc $rc)"; return; }

  in_common LOADTEST_CGROUP_ROOT="$q1" LOADTEST_SERVER_CPUS=0 LOADTEST_GENERATOR_CPUS=auto -- generator_cpus
  ((rc == 0)) && [ "$(tail -n 1 <<<"$out")" = 1 ] || { fail "(r) auto beside a server list that fills a quota of 1 did not give one CPU (rc $rc)"; return; }
  in_common LOADTEST_CGROUP_ROOT="$q1" LOADTEST_SERVER_CPUS=0 LOADTEST_GENERATOR_CPUS=1 -- generator_cpus
  ((rc == 0)) && [ "$out" = 1 ] || { fail "(r) auto's one CPU, given again as the list, was refused (rc $rc)"; return; }
  in_common LOADTEST_CGROUP_ROOT="$q1" LOADTEST_SERVER_CPUS=0 LOADTEST_GENERATOR_CPUS=1-2 -- generator_cpus
  ((rc != 0)) && [[ $out == *"LOADTEST_GENERATOR_CPUS='1-2', 2 CPUs, and LOADTEST_SERVER_CPUS='0', 1 CPU, ask for 3 CPUs, more than the pod's CPU quota of 1 CPU"* ]] ||
    { fail "(r) the list 1-2 beside 0 under a quota of 1 was not refused (rc $rc)"; return; }

  local root
  for root in "$none" "$no_cgroup"; do
    in_common LOADTEST_CGROUP_ROOT="$root" LOADTEST_SERVER_CPUS=0,1 -- generator_cpus
    ((rc == 0)) && [ -z "$out" ] || { fail "(r) without a quota ($root) an unset list did not stay unset (rc $rc)"; return; }
    in_common LOADTEST_CGROUP_ROOT="$root" LOADTEST_SERVER_CPUS=0,1 LOADTEST_GENERATOR_CPUS=2-13 -- generator_cpus
    ((rc == 0)) && [ "$out" = 2-13 ] || { fail "(r) without a quota ($root) the list 2-13 did not stand as given (rc $rc)"; return; }
  done
  in_common LOADTEST_CGROUP_ROOT="$none" LOADTEST_SERVER_CPUS=0,1 LOADTEST_GENERATOR_CPUS=auto -- generator_cpus
  ((rc == 0)) && [ "$(tail -n 1 <<<"$out")" = 2,3 ] &&
    [[ $out == *"LOADTEST_GENERATOR_CPUS=auto: the generator runs on 2,3, every CPU of the pod outside LOADTEST_SERVER_CPUS='0,1'; the pod has no CPU quota"* ]] ||
    { fail "(r) without a quota auto beside 0,1 on a pod of 0-3 was not 2,3 (rc $rc)"; return; }
  in_common LOADTEST_CGROUP_ROOT="$q8" LOADTEST_GENERATOR_CPUS=auto -- generator_cpus
  ((rc != 0)) && [[ $out == *"LOADTEST_GENERATOR_CPUS=auto gives the generator the pod's CPUs outside LOADTEST_SERVER_CPUS, which is unset"* ]] ||
    { fail "(r) auto without a server list was not refused (rc $rc)"; return; }
  out=""
  printf 'PASS: beside the server list, auto and an unset list fit the CPU quota, a list too wide for it is refused, and no quota changes nothing\n'
}

# case_cpu_affinity: the decision of whether a pin binds is affinity_verdict over two durations, so
# it is driven here without timing a loop: a pinned pair about twice as slow binds, one as fast as
# unpinned (gVisor's 9.0 s against 8.5 s) does not. The refusal gives the measurement, and names
# gVisor when /proc/version (LOADTEST_PROC_VERSION_FILE) or dmesg shows it, and not otherwise.
case_cpu_affinity() {
  in_common -- affinity_verdict 4.00 8.10
  ((rc == 0)) && [ "$out" = 2.02 ] || { fail "(l) a pinned pair 2.02x slower did not bind (rc $rc)"; return; }
  in_common -- affinity_verdict 8.5 9.0
  ((rc != 0)) && [ "$out" = 1.06 ] || { fail "(l) a pinned pair 1.06x slower was taken to bind (rc $rc)"; return; }
  in_common -- affinity_verdict 4.00 6.00
  ((rc != 0)) || { fail "(l) a pinned pair 1.5x slower, under the 1.6x bar, was taken to bind"; return; }

  local quiet="${temporary_dir}/quiet-bin" loud="${temporary_dir}/loud-bin" linux="${temporary_dir}/version-linux"
  local gvisor="${temporary_dir}/version-gvisor"
  mkdir -p "$quiet" "$loud"
  printf '#!/usr/bin/env bash\nexit 0\n' >"$quiet/dmesg"
  printf '#!/usr/bin/env bash\necho "[    0.000000] Starting gVisor..."\n' >"$loud/dmesg"
  chmod +x "$quiet/dmesg" "$loud/dmesg"
  printf 'Linux version 6.1.0-28-cloud-amd64 (debian-kernel@lists.debian.org)\n' >"$linux"
  printf 'Linux version 4.19.0-gvisor #1 SMP Sun Jan 10 15:06:54 PST 2016\n' >"$gvisor"

  in_common LOADTEST_PROC_VERSION_FILE="$gvisor" PATH="$quiet:$PATH" -- affinity_refusal 0 8.50 9.00 1.06
  [[ $out == *"this runtime ignores CPU affinity: two busy loops pinned to CPU 0 with taskset took 9.00 s against 8.50 s for the same two unpinned (1.06x)"* &&
    $out == *"this is gVisor (${gvisor}: Linux version 4.19.0-gvisor"* ]] ||
    { fail "(l) the refusal on a gVisor kernel does not give the measurement and name gVisor"; return; }
  in_common LOADTEST_PROC_VERSION_FILE="$linux" PATH="$loud:$PATH" -- affinity_refusal 0 8.50 9.00 1.06
  [[ $out == *"this is gVisor (dmesg: [    0.000000] Starting gVisor...)"* ]] ||
    { fail "(l) the refusal does not name gVisor when dmesg shows it"; return; }
  in_common LOADTEST_PROC_VERSION_FILE="$linux" PATH="$quiet:$PATH" -- affinity_refusal 0 8.50 9.00 1.06
  [[ $out == *"(1.06x)"* && $out != *gVisor* ]] ||
    { fail "(l) the refusal names gVisor where nothing shows it, or lacks the measurement"; return; }
  out=""
  printf 'PASS: a pin that does not slow two busy loops 1.6x is refused, with the measurement and gVisor when it shows\n'
}

# affinity_with ROOT BOOT TIMES...: check_cpu_affinity on ROOT under LOADTEST_SERVER_CPUS=0, with
# LOADTEST_BOOT_ID_FILE holding BOOT (no file when BOOT is -) and busy_pair_seconds faked: its Nth
# call prints the Nth of TIMES, which go unpinned then pinned, try after try, and each call appends
# its arguments to pair-calls. Sets rc, out (what it printed) and calls (the count of fake calls).
affinity_with() {
  local root=$1 boot=$2 boot_file="${temporary_dir}/boot_id"
  shift 2
  printf '%s\n' "$@" >"${temporary_dir}/pair-times"
  : >"${temporary_dir}/pair-calls"
  if [[ $boot == - ]]; then rm -f "$boot_file"; else printf '%s\n' "$boot" >"$boot_file"; fi
  rc=0
  out=$(env -u LOADTEST_GENERATOR_CPUS -u LOADTEST_GOMAXPROCS LOADTEST_SERVER_CPUS=0 LOADTEST_ROOT="$root" \
    LOADTEST_BOOT_ID_FILE="$boot_file" PAIR_TIMES="${temporary_dir}/pair-times" PAIR_CALLS="${temporary_dir}/pair-calls" \
    bash -c 'source "$1"
      busy_pair_seconds() {
        local n
        n=$(wc -l <"$PAIR_CALLS")
        printf "%s\n" "$*" >>"$PAIR_CALLS"
        awk -v n="$((n + 1))" "NR == n" "$PAIR_TIMES"
      }
      resolve_paths
      check_cpu_affinity' affinity "$helper" 2>&1) || rc=$?
  calls=$(wc -l <"${temporary_dir}/pair-calls")
}

# case_affinity_kept_verdict: a step that follows another on the same stack measures the pin on a
# box still busy with what that step left, and a pin that binds then reads short of the 1.6x bar
# (a runc bed after a 10x step: 1.39 s pinned against 1.13 s unpinned, 1.23x). So the check tries
# three times, and a verdict that the pin binds is kept in the root for the rest of the boot.
case_affinity_kept_verdict() {
  out=""
  local root="${temporary_dir}/root-o" refused_root="${temporary_dir}/root-o-refused" kept
  local boot_a=0f1e2d3c-0000-4000-8000-00000000000a boot_b=0f1e2d3c-0000-4000-8000-00000000000b
  mkdir -p "$root" "$refused_root"
  kept="${root}/affinity.json"

  # Two tries short of the bar, then one past it: the pin binds, and the verdict is kept.
  affinity_with "$root" "$boot_a" 1.13 1.39 1.10 1.40 1.00 2.10
  ((rc == 0 && calls == 6)) || { fail "(o) a third try past the bar, after two short of it, did not pass (rc $rc, $calls timings)"; return; }
  [[ $out == *"CPU affinity binds: two busy loops pinned to CPU 0 took 2.10 s against 1.00 s unpinned (2.10x) on try 3 of 3, after 1.39 s against 1.13 s (1.23x), 1.40 s against 1.10 s (1.27x); kept in ${kept}"* ]] ||
    { fail "(o) the pass does not name its try, the tries before it and the kept file"; return; }
  jq -e --arg b "$boot_a" '.boot_id == $b and .cpu == 0 and .unpinned_s == 1 and .pinned_s == 2.1 and (.tries | length) == 3' "$kept" >/dev/null ||
    { out="$(cat "$kept" 2>&1)" fail "(o) ${kept} does not hold the verdict with its boot, CPU, times and tries"; return; }

  # A later step of the same boot takes the kept verdict: it measures nothing, so the box's state
  # (here, times that would be refused) cannot refuse it.
  affinity_with "$root" "$boot_a" 1.13 1.39 1.10 1.40 1.12 1.35
  ((rc == 0 && calls == 0)) || { fail "(o) a step on the boot of a kept verdict measured again (rc $rc, $calls timings)"; return; }
  [[ $out == *"CPU affinity binds, as a step measured at "*" on this boot ("*"${boot_a}, kept in ${kept}): two busy loops pinned to CPU 0 took 2.10 s against 1.00 s unpinned (2.10x); not measured again"* ]] ||
    { fail "(o) a step that took the kept verdict does not say so"; return; }

  # Another boot measures again, and keeps its own verdict; a boot whose id cannot be read measures
  # again and keeps none.
  affinity_with "$root" "$boot_b" 1.00 2.00
  ((rc == 0 && calls == 2)) || { fail "(o) a step on another boot did not measure again (rc $rc, $calls timings)"; return; }
  jq -e --arg b "$boot_b" '.boot_id == $b' "$kept" >/dev/null || { out="$(cat "$kept")" fail "(o) the other boot's verdict was not kept"; return; }
  affinity_with "$root" - 1.00 2.00
  ((rc == 0 && calls == 2)) || { fail "(o) a step whose boot id cannot be read did not measure again (rc $rc, $calls timings)"; return; }
  [[ $out == *"not kept for later steps: ${temporary_dir}/boot_id holds no boot id"* ]] &&
    jq -e --arg b "$boot_b" '.boot_id == $b' "$kept" >/dev/null ||
    { fail "(o) a verdict without a boot id was kept, or the step did not say why it kept none"; return; }

  # Three tries short of the bar, on a root that holds no verdict: refused, naming every try, and
  # nothing kept.
  affinity_with "$refused_root" "$boot_a" 1.13 1.39 1.10 1.40 1.12 1.35
  ((rc != 0 && calls == 6)) || { fail "(o) three tries short of the bar were not refused after three tries (rc $rc, $calls timings)"; return; }
  [[ $out == *"two busy loops pinned to CPU 0 with taskset took 1.39 s against 1.13 s for the same two unpinned (1.23x), then 1.40 s against 1.10 s (1.27x), then 1.35 s against 1.12 s (1.21x) in 3 tries"* ]] ||
    { fail "(o) the refusal does not name all three tries"; return; }
  [[ ! -e "${refused_root}/affinity.json" ]] ||
    { out="$(cat "${refused_root}/affinity.json")" fail "(o) a refused verdict was kept in ${refused_root}/affinity.json"; return; }
  out=""
  printf 'PASS: the pin passes on a third try, refuses after three naming all, keeps a binding verdict for its boot alone and never a refused one\n'
}

# case_gomaxprocs: Dispatch's GOMAXPROCS is the number of CPUs in LOADTEST_SERVER_CPUS, each CPU
# counted once, when the list is set, and nproc otherwise. A LOADTEST_GOMAXPROCS equal to the count
# passes; one that differs is refused naming both numbers, and so is one that is not a positive
# integer. A step's meta.json that records a GOMAXPROCS other than its server list's count, or one
# that could not be read, is refused naming both (gomaxprocs_refusal); one on the count, an
# unpinned one and a missing one are not.
case_gomaxprocs() {
  in_common LOADTEST_SERVER_CPUS=0-6:3 -- server_cpu_count
  ((rc == 0)) && [ "$out" = 3 ] || { fail "(m) server_cpu_count of 0-6:3 is '$out' (rc $rc), want 3"; return; }
  in_common LOADTEST_SERVER_CPUS=0 -- gomaxprocs
  ((rc == 0)) && [ "$out" = 1 ] || { fail "(m) LOADTEST_SERVER_CPUS=0 gave GOMAXPROCS '$out' (rc $rc), want 1, not nproc"; return; }
  in_common LOADTEST_SERVER_CPUS=0,0-6:3,3 -- gomaxprocs
  ((rc == 0)) && [ "$out" = 3 ] || { fail "(m) 0,0-6:3,3 gave GOMAXPROCS '$out' (rc $rc), want each CPU once: 3"; return; }
  in_common -- gomaxprocs
  ((rc == 0)) && [ "$out" = "$(nproc)" ] || { fail "(m) no server list gave GOMAXPROCS '$out' (rc $rc), want nproc"; return; }
  in_common LOADTEST_GOMAXPROCS=5 -- gomaxprocs
  ((rc == 0)) && [ "$out" = 5 ] || { fail "(m) LOADTEST_GOMAXPROCS=5 with no server list gave '$out' (rc $rc)"; return; }
  in_common LOADTEST_SERVER_CPUS=0 LOADTEST_GOMAXPROCS=1 -- gomaxprocs
  ((rc == 0)) && [ "$out" = 1 ] || { fail "(m) LOADTEST_GOMAXPROCS=1 on a one-CPU list was refused: $out"; return; }
  in_common LOADTEST_SERVER_CPUS=0-1 LOADTEST_GOMAXPROCS=32 -- gomaxprocs
  ((rc != 0)) && [[ $out == *"LOADTEST_GOMAXPROCS=32 (run-step.sh --gomaxprocs) differs from LOADTEST_SERVER_CPUS='0-1', 2 CPUs: a Dispatch pinned to 2 CPUs with GOMAXPROCS 32 is not the bed any reading names"* ]] ||
    { fail "(m) LOADTEST_GOMAXPROCS=32 on a two-CPU list was not refused naming both: $out"; return; }
  in_common LOADTEST_SERVER_CPUS=0 LOADTEST_GOMAXPROCS=2 -- gomaxprocs
  ((rc != 0)) && [[ $out == *"LOADTEST_SERVER_CPUS='0', 1 CPU: a Dispatch pinned to 1 CPU with GOMAXPROCS 2"* ]] ||
    { fail "(m) LOADTEST_GOMAXPROCS=2 on a one-CPU list was not refused naming both: $out"; return; }
  in_common LOADTEST_GOMAXPROCS=x -- gomaxprocs
  ((rc != 0)) && [[ $out == *"LOADTEST_GOMAXPROCS='x' is not a positive integer"* ]] || { fail "(m) LOADTEST_GOMAXPROCS=x was not refused: $out"; return; }

  local meta="${temporary_dir}/meta.json"
  printf '%s\n' '{"gomaxprocs":"32","server_cpus":"0-1","server_cpu_count":2}' >"$meta"
  in_common -- gomaxprocs_refusal "$meta"
  ((rc == 0)) && [ "$out" = "Dispatch ran with GOMAXPROCS 32 (meta.json gomaxprocs) pinned to LOADTEST_SERVER_CPUS='0-1', 2 CPUs: a Dispatch pinned to 2 CPUs with GOMAXPROCS 32 is not the bed any reading names" ] ||
    { fail "(m) meta.json's GOMAXPROCS 32 on a two-CPU list was not refused naming both (rc $rc): $out"; return; }
  printf '%s\n' '{"gomaxprocs":"","gomaxprocs_error":"no Dispatch pid","server_cpus":"0","server_cpu_count":1}' >"$meta"
  in_common -- gomaxprocs_refusal "$meta"
  ((rc == 0)) && [[ $out == *"GOMAXPROCS unread (no Dispatch pid)"*"'0', 1 CPU:"* ]] ||
    { fail "(m) a GOMAXPROCS meta.json could not read on a pinned step was not refused (rc $rc): $out"; return; }
  local passes
  for passes in '{"gomaxprocs":"2","server_cpus":"0-1","server_cpu_count":2}' '{"gomaxprocs":"32","server_cpus":"","server_cpu_count":null}'; do
    printf '%s\n' "$passes" >"$meta"
    in_common -- gomaxprocs_refusal "$meta"
    ((rc != 0)) || { fail "(m) meta.json $passes was refused: $out"; return; }
  done
  in_common -- gomaxprocs_refusal "${temporary_dir}/no-such-meta.json"
  ((rc != 0)) || { fail "(m) a missing meta.json was refused: $out"; return; }
  out=""
  printf 'PASS: GOMAXPROCS follows the server list, and one that differs from its count is refused\n'
}

# wait_window ROOT STEP PID: wait_for_window STEP PID on ROOT, in a shell that sourced common.sh.
# Sets rc, out (what it printed) and said (what it said on stderr).
wait_window() {
  rc=0
  out=$(LOADTEST_ROOT="$1" bash -c 'source "$1"; resolve_paths; wait_for_window "$2" "$3"' wait "$helper" "$2" "$3" \
    2>"${temporary_dir}/wait.err") || rc=$?
  said=$(<"${temporary_dir}/wait.err")
}

# case_wait_for_window: a script that acts at a moment of a step's window waits for it with
# wait_for_window, given the step and the pid of the process that runs it. Each step here is a
# process that does to the step's directory what run-step.sh does: one opens its window, one ends
# after it wrote its not-run record beside a directory that held an earlier run, one ends with
# neither.
case_wait_for_window() {
  out=""
  local root="${temporary_dir}/root-w" dir step ended returned after
  mkdir -p "${root}/runs"

  # A step that opens its window 1.5 s in, its meta.json holding Go's zero time until then.
  dir="${root}/runs/opens"
  bash -c 'mkdir -p "$1"
    printf "%s\n" "{\"window_start\":\"0001-01-01T00:00:00Z\"}" >"$1/meta.json"
    sleep 1.5
    printf "{\"window_start\":\"%s\"}\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$1/meta.json"
    exec sleep 30' opens "$dir" &
  step=$!
  pids+=("$step")
  wait_window "$root" opens "$step"
  if ((rc != 0)) || [[ $out == 0001* || $out != "$(jq -r .window_start "${dir}/meta.json")" ]]; then
    out="$out $said" fail "(n) wait_for_window did not print the window start of a step that opened its window (rc $rc)"
    return
  fi
  kill -TERM "$step"
  wait "$step" 2>/dev/null || true

  # A step whose directory holds an earlier run's meta.json, and that ends 1.5 s in once it has
  # written its not-run record, as run-step.sh does when that directory makes it refuse the step.
  dir="${root}/runs/refused"
  mkdir -p "$dir"
  printf '%s\n' '{"window_start":"2026-01-01T00:00:00Z"}' >"${dir}/meta.json"
  bash -c 'sleep 1.5
    printf "%s\n" "{\"name\":\"refused\",\"exit_status\":1,\"reason\":\"run-step.sh: $1 already holds a step\",\"at\":\"2026-11-17T00:00:00Z\",\"last_lines\":[]}" >"$1.not-run.json"
    printf "%s\n" "$EPOCHREALTIME" >"$2"
    exit 1' refused "$dir" "${temporary_dir}/refused-ended" &
  step=$!
  pids+=("$step")
  wait_window "$root" "$dir" "$step"
  returned=$EPOCHREALTIME
  ended=$(cat "${temporary_dir}/refused-ended" 2>/dev/null || echo 0)
  after=$(awk -v a="$ended" -v b="$returned" 'BEGIN { printf "%.2f", b - a }')
  if ((rc == 0)) || [ -n "$out" ]; then
    out="$out" fail "(n) wait_for_window took the window of an earlier run left in the directory of a step that never opened its own (rc $rc)"
    return
  fi
  [[ $said == *"run-step.sh exited 1 at 2026-11-17T00:00:00Z: run-step.sh: ${dir} already holds a step"* ]] ||
    { out="$said" fail "(n) wait_for_window did not give the not-run record's exit status and reason"; return; }
  awk -v s="$after" 'BEGIN { exit !(s < 1) }' ||
    { out="$said" fail "(n) wait_for_window returned ${after} s after the step ended, not within a second"; return; }

  # A step that ends with neither a window nor a record.
  bash -c 'sleep 0.5' gone &
  step=$!
  pids+=("$step")
  wait_window "$root" gone "$step"
  if ((rc == 0)) || [[ $said != *"pid ${step} ended before it and left no not-run record"* ]]; then
    out="$out $said" fail "(n) wait_for_window did not say that a step which ended without a window left no record (rc $rc)"
    return
  fi
  out=""
  printf 'PASS: wait_for_window prints a window start, never an earlier run'"'"'s, and returns within a second (%s s) of a step that ends without one\n' "$after"
}

# quiet_with READINGS [NAME=VALUE...]: wait_for_quiet_box under the NAME=VALUE variables (and no
# other LOADTEST_MAX_BUSY_BEFORE or LOADTEST_QUIET_WAIT), its timings faked as affinity_with fakes
# the pin's: box_busy_reading prints the Nth of READINGS (space separated; the last again past
# them) on its Nth call, and quiet_pause records its pause and returns at once. Sets rc, out (what
# it printed, then, once it returned, a line of QUIET_READINGS, QUIET_BAR and QUIET_WAIT), calls
# (the readings it took) and pauses (its pauses, space separated).
quiet_with() {
  local -a readings
  read -ra readings <<<"$1"
  shift
  printf '%s\n' "${readings[@]}" >"${temporary_dir}/quiet-readings"
  : >"${temporary_dir}/quiet-calls"
  : >"${temporary_dir}/quiet-pauses"
  rc=0
  out=$(env -u LOADTEST_MAX_BUSY_BEFORE -u LOADTEST_QUIET_WAIT "$@" QUIET_TEST_READINGS="${temporary_dir}/quiet-readings" \
    QUIET_TEST_CALLS="${temporary_dir}/quiet-calls" QUIET_TEST_PAUSES="${temporary_dir}/quiet-pauses" \
    bash -c 'source "$1"
      box_busy_reading() {
        local n
        printf "x\n" >>"$QUIET_TEST_CALLS"
        n=$(wc -l <"$QUIET_TEST_CALLS")
        awk -v n="$n" "NR == n { print; found = 1 } { last = \$0 } END { if (!found) print last }" "$QUIET_TEST_READINGS"
      }
      quiet_pause() { printf "%s\n" "$1" >>"$QUIET_TEST_PAUSES"; }
      wait_for_quiet_box
      printf "readings=%s bar=%s wait=%s\n" "${QUIET_READINGS[*]}" "$QUIET_BAR" "$QUIET_WAIT"' quiet "$helper" 2>&1) || rc=$?
  calls=$(wc -l <"${temporary_dir}/quiet-calls")
  pauses=$(paste -sd' ' "${temporary_dir}/quiet-pauses")
}

# case_quiet_box: a step starts only on a box at most LOADTEST_MAX_BUSY_BEFORE busy (default 0.10,
# the bar itself included), reading every 30 s while the box is busier, until the pauses add up to
# LOADTEST_QUIET_WAIT seconds (default 900); past that it is refused, naming every reading, the bar
# and the wait. A bar or a wait that is not one is refused before any reading, and so is a reading
# that is not a fraction.
case_quiet_box() {
  out=""
  local all
  quiet_with "0.0400"
  ((rc == 0 && calls == 1)) && [[ -z $pauses && $out == *"readings=0.0400 bar=0.10 wait=900"* &&
    $out == *"the box is 0.0400 busy (loadgen box-busy), at or under LOADTEST_MAX_BUSY_BEFORE=0.10: the step starts"* ]] ||
    { fail "(p) a box 4% busy did not start the step at once on one reading (rc $rc, $calls readings, pauses '$pauses')"; return; }
  quiet_with "0.1000"
  ((rc == 0 && calls == 1)) && [[ -z $pauses ]] ||
    { fail "(p) a box at the bar, 0.1000, did not start the step at once (rc $rc, $calls readings, pauses '$pauses')"; return; }

  # Just over the bar, then quiet: one pause of 30 s, and the step starts naming both readings.
  quiet_with "0.1001 0.0400"
  ((rc == 0 && calls == 2)) && [[ $pauses == 30 && $out == *"readings=0.1001 0.0400 bar=0.10 wait=900"* &&
    $out == *"the box is 0.1001 busy (loadgen box-busy), above LOADTEST_MAX_BUSY_BEFORE=0.10: the step waits for it to go quiet, reading again in 30 s (0 of LOADTEST_QUIET_WAIT=900 s waited)"* &&
    $out == *"the box is 0.0400 busy (loadgen box-busy), at or under LOADTEST_MAX_BUSY_BEFORE=0.10, after 30 s of waiting: the step starts, its readings 0.1001, 0.0400"* ]] ||
    { fail "(p) a box just over the bar, then quiet, did not wait 30 s and start naming both readings (rc $rc, $calls readings, pauses '$pauses')"; return; }

  # Busy through the default wait: 30 pauses of 30 s and 31 readings, then the refusal naming each.
  quiet_with "0.4200"
  all=$(printf '0.4200, %.0s' {1..31})
  ((rc != 0 && calls == 31)) && [[ $pauses == "$(printf '30 %.0s' {1..29})30" && $out != *"readings="* &&
    $out == *"the box stayed busier than LOADTEST_MAX_BUSY_BEFORE=0.10 through LOADTEST_QUIET_WAIT=900 s, so the step is not run: 31 readings of loadgen box-busy, ${all%, }. "* ]] ||
    { fail "(p) a box busy through the default 900 s was not refused after 31 readings 30 s apart, naming each (rc $rc, $calls readings, pauses '$pauses')"; return; }

  # The wait follows LOADTEST_QUIET_WAIT: 75 s is pauses of 30, 30 and 15 s; 0 is one reading.
  quiet_with "0.4200" LOADTEST_QUIET_WAIT=75
  ((rc != 0 && calls == 4)) && [[ $pauses == "30 30 15" &&
    $out == *"through LOADTEST_QUIET_WAIT=75 s, so the step is not run: 4 readings of loadgen box-busy, 0.4200, 0.4200, 0.4200, 0.4200. "* ]] ||
    { fail "(p) LOADTEST_QUIET_WAIT=75 did not pause 30, 30 and 15 s before it refused (rc $rc, $calls readings, pauses '$pauses')"; return; }
  quiet_with "0.4200 0.0400" LOADTEST_QUIET_WAIT=0
  ((rc != 0 && calls == 1)) && [[ -z $pauses && $out == *"through LOADTEST_QUIET_WAIT=0 s, so the step is not run: 1 reading of loadgen box-busy, 0.4200. "* ]] ||
    { fail "(p) LOADTEST_QUIET_WAIT=0 did not refuse a busy box on its one reading (rc $rc, $calls readings, pauses '$pauses')"; return; }

  # The bar follows LOADTEST_MAX_BUSY_BEFORE.
  quiet_with "0.4200" LOADTEST_MAX_BUSY_BEFORE=0.50
  ((rc == 0 && calls == 1)) && [[ $out == *"at or under LOADTEST_MAX_BUSY_BEFORE=0.50: the step starts"* && $out == *"bar=0.50 "* ]] ||
    { fail "(p) LOADTEST_MAX_BUSY_BEFORE=0.50 did not start the step at once on a box 42% busy (rc $rc, $calls readings)"; return; }
  quiet_with "0.0600 0.0400" LOADTEST_MAX_BUSY_BEFORE=0.05
  ((rc == 0 && calls == 2)) && [[ $pauses == 30 && $out == *"the box is 0.0600 busy (loadgen box-busy), above LOADTEST_MAX_BUSY_BEFORE=0.05"* ]] ||
    { fail "(p) LOADTEST_MAX_BUSY_BEFORE=0.05 did not hold back a box 6% busy (rc $rc, $calls readings, pauses '$pauses')"; return; }

  # A bar, a wait or a reading that is not one is refused, the first two before any reading.
  quiet_with "0.0400" LOADTEST_MAX_BUSY_BEFORE=10%
  ((rc != 0 && calls == 0)) && [[ $out == *"LOADTEST_MAX_BUSY_BEFORE='10%' is not a fraction between 0 and 1 written like 0.10"* ]] ||
    { fail "(p) LOADTEST_MAX_BUSY_BEFORE=10% was not refused before any reading (rc $rc, $calls readings)"; return; }
  quiet_with "0.0400" LOADTEST_MAX_BUSY_BEFORE=1.5
  ((rc != 0 && calls == 0)) || { fail "(p) LOADTEST_MAX_BUSY_BEFORE=1.5 was not refused before any reading (rc $rc, $calls readings)"; return; }
  quiet_with "0.0400" LOADTEST_QUIET_WAIT=15m
  ((rc != 0 && calls == 0)) && [[ $out == *"LOADTEST_QUIET_WAIT='15m' is not a whole number of seconds like 900"* ]] ||
    { fail "(p) LOADTEST_QUIET_WAIT=15m was not refused before any reading (rc $rc, $calls readings)"; return; }
  quiet_with "busy"
  ((rc != 0 && calls == 1)) && [[ $out == *"the quiet-box gate read 'busy' from loadgen box-busy, which is not a fraction between 0 and 1"* ]] ||
    { fail "(p) a reading that is not a fraction was not refused (rc $rc, $calls readings)"; return; }
  out=""
  printf 'PASS: the quiet-box gate starts on a reading at or under its bar, waits 30 s a pause up to its wait, refuses past it naming every reading, and follows both variables\n'
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
# default range (32768-60999) and a gVisor pod's (16000-65535) leave the blocks at 10000-11999, as
# does a range this machine cannot read; ranges that cover that block, from 11000 and from 10500,
# move them to the 2000 ports below the range; and a range starting too low for 2000 ports above
# 1023 is refused naming both ranges, unless every port variable is set.
case_ports_below_ephemeral_range() {
  out=""
  local root=/legion/scratch/tmp/lt-one ports low rc max linux gvisor unread pg
  linux=$(ports_for "$root" LOADTEST_PORT_RANGE_FILE="$(range_file 32768 60999)")
  gvisor=$(ports_for "$root" LOADTEST_PORT_RANGE_FILE="$(range_file 16000 65535)")
  unread=$(ports_for "$root" LOADTEST_PORT_RANGE_FILE="${temporary_dir}/no-such-range")
  [ "$linux" = "$unread" ] && [ "$gvisor" = "$unread" ] ||
    { out="$linux"$'\n'"$gvisor"$'\n'"$unread" fail "(s) Linux's default range or a gVisor pod's moved the blocks from 10000"; return; }
  pg=$(awk '$1 == "postgres" {print $2}' <<<"$linux")
  ((pg % 10 == 0 && pg >= 10000 && pg < 12000)) ||
    { out="$linux" fail "(s) Linux's default range did not keep the blocks at 10000"; return; }
  for low in 11000 10500; do
    ports=$(ports_for "$root" LOADTEST_PORT_RANGE_FILE="$(range_file "$low" 65535)") ||
      { out="$ports" fail "(s) a range from $low refused the stack's ports"; return; }
    max=$(awk '{print $2}' <<<"$ports" | sort -n | tail -1)
    ((max < low)) || { out="$ports" fail "(s) a range from $low left port $max inside it"; return; }
    (($(awk '{print $2}' <<<"$ports" | sort -n | head -1) >= low - 2000)) ||
      { out="$ports" fail "(s) a range from $low put a port more than 2000 below it"; return; }
  done
  out=$(ports_for "$root" LOADTEST_PORT_RANGE_FILE="$(range_file 1500 65535)" 2>&1) && rc=0 || rc=$?
  ((rc != 0)) || { fail "(s) a range from 1500 left room for 2000 ports above 1023"; return; }
  [[ $out == *"1500-65535"* && $out == *"2000 ports"* ]] || { fail "(s) the refusal does not name the range and the ports the stack needs"; return; }
  out=$(ports_for "$root" LOADTEST_PORT_RANGE_FILE="$(range_file 1500 65535)" LOADTEST_PG_PORT=1100 \
    LOADTEST_PROXY_PORT=1101 LOADTEST_PROXY_STATS_PORT=1102 LOADTEST_NATS_PORT=1103 LOADTEST_NATS_MONITOR_PORT=1104 \
    LOADTEST_LISTENER_PORT=1105 LOADTEST_DISPATCH_PORT=1106 LOADTEST_PPROF_PORT=1107 LOADTEST_METRICS_PORT=1108 \
    LOADTEST_ENVOY_PROXY_PORT=1109 2>&1) ||
    { fail "(s) every port variable set, a range from 1500 still refused the stack"; return; }
  out=""
  printf 'PASS: the default ports sit below the ephemeral range, and a range with no room below it is refused naming both\n'
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
case_ephemeral_ports || failed=$((failed + 1))
case_cpu_lists || failed=$((failed + 1))
case_cpu_affinity || failed=$((failed + 1))
case_affinity_kept_verdict || failed=$((failed + 1))
case_gomaxprocs || failed=$((failed + 1))
case_wait_for_window || failed=$((failed + 1))
case_quiet_box || failed=$((failed + 1))
case_cpu_quota || failed=$((failed + 1))
case_generator_cpus || failed=$((failed + 1))
case_ports_below_ephemeral_range || failed=$((failed + 1))
if ((failed > 0)); then
  printf 'FAIL: %d of 20 cases failed\n' "$failed" >&2
  exit 1
fi
