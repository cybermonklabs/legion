#!/usr/bin/env bash
# packages/envoy/loadtest/common.test.sh
#
# Proves the run lock of common.sh (lock_root) and the stack's ports, which the operator gives and
# common.sh never chooses (stack_ports), without a stack. Each lock taker below sources common.sh and
# calls lock_root, as run-series.sh, run-step.sh and stack.sh do.
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
# (i) the stack chooses no port of its own: with no port given it refuses, naming the variables to
# set; LOADTEST_PORT_BASE gives the nine ports as its offsets 0 to 8; a LOADTEST_*_PORT overrides its
# own port alone; and a base or a port outside 1024-65535 is refused.
# (j) a port is inside the machine's ephemeral range, which stack.sh start refuses (check_port), from
# the range's first port to its last, under a gVisor pod's range and Linux's default, and a range
# that cannot be read, or a file that holds no range, holds no port.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly helper="${here}/common.sh"
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

# ports_for [NAME=VALUE...]: the stack's ports, "KEY PORT" a line, from common.sh, in a shell that
# holds no LOADTEST_PORT_BASE and no LOADTEST_*_PORT but the ones given; its stderr follows.
ports_for() {
  env -u LOADTEST_PORT_BASE -u LOADTEST_PG_PORT -u LOADTEST_PROXY_PORT -u LOADTEST_PROXY_STATS_PORT \
    -u LOADTEST_NATS_PORT -u LOADTEST_NATS_MONITOR_PORT -u LOADTEST_LISTENER_PORT -u LOADTEST_DISPATCH_PORT \
    -u LOADTEST_PPROF_PORT -u LOADTEST_ENVOY_PROXY_PORT "$@" bash -c 'source "$1"; stack_ports' ports "$helper" 2>&1
}

# case_stack_ports: the stack's ports are the operator's. A port the stack chose and tested free
# could be taken by another program between the test and the server's bind, so with no port given
# stack_ports refuses and names what to set.
case_stack_ports() {
  out=""
  local none based set_pg bad rc
  none=$(ports_for) && rc=0 || rc=$?
  ((rc != 0)) || { out="$none" fail "(i) stack_ports chose ports with none given"; return; }
  [[ $none == *LOADTEST_PORT_BASE* && $none == *LOADTEST_PG_PORT* ]] ||
    { out="$none" fail "(i) the refusal does not name LOADTEST_PORT_BASE and the port variables"; return; }
  based=$(ports_for LOADTEST_PORT_BASE=20000) || { out="$based" fail "(i) LOADTEST_PORT_BASE=20000 was refused"; return; }
  [ "$based" = "$(printf '%s\n' 'postgres 20000' 'proxy 20001' 'proxy_stats 20007' 'nats 20002' 'nats_monitor 20003' \
    'listener 20004' 'dispatch 20005' 'pprof 20006' 'envoy_proxy 20008')" ] ||
    { out="$based" fail "(i) LOADTEST_PORT_BASE=20000 did not give the nine ports 20000-20008"; return; }
  set_pg=$(ports_for LOADTEST_PORT_BASE=20000 LOADTEST_PG_PORT=23456) || { out="$set_pg" fail "(i) LOADTEST_PG_PORT was refused"; return; }
  [ "$(awk '$1 == "postgres" {print $2}' <<<"$set_pg")" = 23456 ] ||
    { out="$set_pg" fail "(i) LOADTEST_PG_PORT did not set the Postgres port"; return; }
  [ "$(grep -v '^postgres ' <<<"$set_pg")" = "$(grep -v '^postgres ' <<<"$based")" ] ||
    { out="$set_pg" fail "(i) LOADTEST_PG_PORT moved another port than Postgres'"; return; }
  for bad in LOADTEST_PORT_BASE=65530 LOADTEST_PORT_BASE=80 LOADTEST_PORT_BASE=x; do
    out=$(ports_for "$bad") && { fail "(i) stack_ports took $bad"; return; }
    [[ $out == *"not a port"* ]] || { fail "(i) the refusal of $bad does not say it is not a port"; return; }
  done
  out=""
  printf 'PASS: the stack chooses no port: it refuses with none given, LOADTEST_PORT_BASE gives nine, and each LOADTEST_*_PORT overrides its own\n'
}

# range_file LOW HIGH: a file holding an ephemeral range as /proc/sys/net/ipv4/ip_local_port_range
# does, for LOADTEST_PORT_RANGE_FILE.
range_file() {
  local file="${temporary_dir}/range-$1-$2"
  printf '%s\t%s\n' "$1" "$2" >"$file"
  printf '%s\n' "$file"
}

# in_range FILE PORT: common.sh's port_in_ephemeral PORT, with the ephemeral range read from FILE
# (LOADTEST_PORT_RANGE_FILE).
in_range() {
  LOADTEST_PORT_RANGE_FILE=$1 bash -c 'source "$1"; port_in_ephemeral "$2"' in-range "$helper" "$2"
}

# case_port_in_ephemeral_range: stack.sh start refuses a port inside the machine's ephemeral range
# (check_port, through port_in_ephemeral), since any outgoing connection may take such a port as
# its source port before the stack's server binds it. A range's first and last ports are inside it
# and the ports next to them are not, under a gVisor pod's range (16000-65535) and Linux's default
# (32768-60999); a range that cannot be read, or a file that holds no range, holds no port.
case_port_in_ephemeral_range() {
  out=""
  local gvisor linux p
  gvisor=$(range_file 16000 65535)
  linux=$(range_file 32768 60999)
  for p in 16000 65535; do
    in_range "$gvisor" "$p" || { fail "(j) port $p is not inside the range 16000-65535"; return; }
  done
  ! in_range "$gvisor" 15999 || { fail "(j) port 15999 is inside the range 16000-65535"; return; }
  for p in 32768 60999; do
    in_range "$linux" "$p" || { fail "(j) port $p is not inside the range 32768-60999"; return; }
  done
  for p in 32767 61000 20008; do
    ! in_range "$linux" "$p" || { fail "(j) port $p is inside the range 32768-60999"; return; }
  done
  ! in_range "${temporary_dir}/no-such-range" 40000 ||
    { fail "(j) a range that cannot be read holds port 40000"; return; }
  printf 'no range\n' >"${temporary_dir}/range-none"
  ! in_range "${temporary_dir}/range-none" 40000 || { fail "(j) a file that holds no range holds port 40000"; return; }
  printf 'PASS: a port is inside the ephemeral range from its first port to its last, and a range that cannot be read holds none\n'
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
case_port_in_ephemeral_range || failed=$((failed + 1))
if ((failed > 0)); then
  printf 'FAIL: %d of 11 cases failed\n' "$failed" >&2
  exit 1
fi
