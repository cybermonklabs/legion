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

# case_holder_names_escaped: a holder's command line is whatever its process gave itself, and a
# refusal prints it: lock_holders (the lock's descriptor 9), the lock record's command, which a
# refusal quotes, and stack.sh's port_holder (a listening socket) print each one with a carriage
# return, a newline, a tab, an escape, a backslash and a byte that is not ASCII written as escapes,
# so nothing a process names itself reaches a terminal or a log as it is. lock_root's refusal prints
# a record another writer left with its control characters escaped, and one lock_take wrote, which
# is escaped already, as it is.
case_holder_names_escaped() {
  out=""
  local dir="${temporary_dir}/escaped" weird want holder listener port line i writer taker
  mkdir -p "$dir"
  weird=$'a\rb\nc\td\e[0m\\e\xff'
  want='a\rb\nc\td\x1b[0m\\e\xff'
  : >"$dir/lock"
  # The holder execs sleep under the command line under test (`exec -a`), so its pid, the one
  # cleanup kills, is the sleep that holds descriptor 9, and no process outlives the test. Its output
  # goes to /dev/null, as every other background process's here does.
  bash -c 'exec 9>>"$1"; exec -a "$2" sleep 300' holder "$dir/lock" "$weird" >/dev/null 2>&1 &
  holder=$!
  pids+=("$holder")
  # -ef compares the files, as lock_holders does: readlink gives the resolved path, which is not
  # $dir/lock as written where TMPDIR has a symlink in it.
  for ((i = 0; i < 100; i++)); do [ "/proc/$holder/fd/9" -ef "$dir/lock" ] && break; sleep 0.1; done
  out=$(bash -c 'source "$1"; lock_holders "$2" 10' check "$helper" "$dir/lock")
  [[ $out == *"pid $holder ($want 300)"* ]] ||
    { fail "lock_holders did not print the holder's command line escaped"; return; }
  [[ $out != *[$'\r\n\t\e']* && $out != *$'\xff'* ]] ||
    { fail "lock_holders printed a control character or a byte that is not ASCII as it is"; return; }

  # The record lock_take writes: one line per field, its command escaped.
  bash -c 'source "$1"; exec 9>"$2"; lock_take "$2"' taker "$helper" "$dir/record" "$weird"
  out=$(<"$dir/record")
  [ "$(wc -l <"$dir/record")" = 5 ] || { fail "lock_take's record is not five lines"; return; }
  line=$(grep '^command=' "$dir/record")
  [ "$line" = "command=bash -c source \"\$1\"; exec 9>\"\$2\"; lock_take \"\$2\" taker $helper $dir/record $want" ] ||
    { fail "lock_take's record does not hold the command line escaped"; return; }

  # A record another writer left, such as an older checkout on a shared LOADTEST_ROOT: written as
  # it is, its command and its start with control characters in them (none with a newline, which
  # would end the field), by a process of this PID namespace that holds the lock on descriptor 7
  # under its own pid. lock_root's refusal names that run and must print those fields escaped. Its
  # backslash stays as it is: a record lock_take wrote holds only printable ASCII, its escapes
  # among it, and must print once, as it is (the next row).
  mkdir -p "$dir/root-foreign"
  bash -c 'exec 7<>"$1/.run.lock"; flock 7
printf "pid=%s\npidns=%s\ncommand=%s\nstarted=%s\ntoken=0123456789abcdef0123456789abcdef\n" "$$" "$(readlink /proc/self/ns/pid)" "$2" "$3" >"$1/.run.lock"
echo ready >"$4"; exec sleep 300' writer "$dir/root-foreign" $'a\rb\tc\e[0m\\e\xff' $'2026-11-17T00:00:00Z\e[2J' \
    "$dir/foreign-ready" >/dev/null 2>&1 &
  writer=$!
  pids+=("$writer")
  wait_for "$dir/foreign-ready" || { fail "the other writer wrote no record"; return; }
  try_lock "$dir/root-foreign"
  ((rc != 0)) || { fail "a caller was admitted to a root another run holds"; return; }
  [[ $out == *"(pid $writer, started 2026-11-17T00:00:00Z\x1b[2J: a\rb\tc\x1b[0m\e\xff)"* ]] ||
    { fail "lock_root did not print another writer's record with its control characters escaped"; return; }
  [[ $out != *[$'\r\t\e']* && $out != *$'\xff'* ]] ||
    { fail "lock_root printed a control character or a byte that is not ASCII as it is"; return; }

  # A record lock_take wrote, through a lock_root run whose command line holds the line under test.
  bash -c 'source "$1"; lock_root "$2"; echo ready >"$3"; exec sleep 300' taker "$helper" "$dir/root-taken" \
    "$dir/taken-ready" "$weird" >/dev/null 2>&1 &
  taker=$!
  pids+=("$taker")
  wait_for "$dir/taken-ready" || { fail "the lock_root run was not admitted"; return; }
  try_lock "$dir/root-taken"
  ((rc != 0)) || { fail "a caller was admitted to a root another run holds"; return; }
  [[ $out == *"(pid $taker, started "*": bash -c "*" taker $helper $dir/root-taken $dir/taken-ready $want)"* ]] ||
    { fail "lock_root did not print lock_take's record as it is, escaped once"; return; }

  # stack.sh's port_holder, its function as stack.sh defines it, over a listener named the same.
  # Its proc root holds the network tables and the listener alone: over the host's /proc the look
  # reads a link of every descriptor of every process. The listener is Python by its own path: a
  # version manager's python3 shim passes its arguments on as text, the 0xff byte replaced by
  # U+FFFD, and the listener's command line would no longer be the one under test.
  # port_holder runs with bash's `read` wrapped in a function that leaves a mark, exported, so the
  # shell its walk runs in (common.sh's bounded) has it too: a port_holder that read the tables with
  # `read` again, whose seeks rebuild a /proc table once for each row, would still answer, only
  # slower, so the mark is what fails the case.
  "$(python3 -c 'import sys; print(sys.executable)')" -c '
import os, socket, sys, time
s = socket.socket()
s.bind(("127.0.0.1", 0))
s.listen()
open(sys.argv[1] + ".tmp", "w").write(str(s.getsockname()[1]))
os.replace(sys.argv[1] + ".tmp", sys.argv[1])
time.sleep(300)
' "$dir/port" "$weird" >/dev/null 2>&1 &
  listener=$!
  pids+=("$listener")
  wait_for "$dir/port" || { fail "the listener wrote no port"; return; }
  port=$(<"$dir/port")
  mkdir -p "$dir/proc/net"
  ln -s /proc/net/tcp "$dir/proc/net/tcp"
  ln -s /proc/net/tcp6 "$dir/proc/net/tcp6"
  ln -s "/proc/$listener" "$dir/proc/$listener"
  out=$(proc_root="$dir/proc" bash -c 'source "$1"; eval "$(sed -n "/^port_holder() {/,/^}/p;/^port_holder_walk() {/,/^}/p" "$2")"; export mark=$4; read() { : >"$mark"; builtin read "$@"; }; export -f read; port_holder "$3" 10' \
    check "$helper" "$here/stack.sh" "$port" "$dir/port-holder-read")
  [[ ! -e $dir/port-holder-read ]] ||
    { fail "port_holder read a table with bash's read, whose every row seeks the table back and the kernel makes it again"; return; }
  [[ $out == *"pid $listener ("*" $dir/port $want)"* ]] ||
    { fail "port_holder did not print the listener's command line escaped"; return; }
  [[ $out != *[$'\r\n\t\e']* && $out != *$'\xff'* ]] ||
    { fail "port_holder printed a control character or a byte that is not ASCII as it is"; return; }
  out=""
  printf 'PASS: lock_holders, the lock record and port_holder print a command line with its control characters escaped\n'
}

# Programs that never answer, each a `sleep 600` in its place, as a holder whose command line is a
# FIFO no writer opens holds its reader, so a look that waits on one unbounded holds its caller's
# refusal for ten minutes: stuck-tr holds `tr`, which lock_holders reads a holder's command line
# with, and stuck-links holds `readlink` and `find`, which port_holder reads a descriptor's link
# with one of. lock_root itself reads its PID namespace with readlink, so its case holds `tr` alone.
readonly stuck_tr="${temporary_dir}/stuck-tr" stuck_links="${temporary_dir}/stuck-links"
mkdir -p "$stuck_tr" "$stuck_links"
for tool in "$stuck_tr/tr" "$stuck_links/readlink" "$stuck_links/find"; do
  printf '#!/usr/bin/env bash\nexec sleep 600\n' >"$tool"
  chmod +x "$tool"
done

# case_holder_look_bounded: lock_root's look for the lock's holders walks every process of the
# machine and reads each holder's command line, so it is held to lock_root's 3 s wait, and the
# refusal names that bound when the look ends there. The holder holds the lock on descriptor 9
# under a complete record whose pid cannot run (one above pid_max), so lock_root looks for it; the
# caller's `tr` never answers. The caller runs under a 20 s timeout of its own: a look held by the
# `tr` holds the refusal past it.
case_holder_look_bounded() {
  out=""
  local root="${temporary_dir}/root-bounded" holder ready="${temporary_dir}/bounded-ready" gone status=0
  mkdir -p "$root"
  gone=$(($(</proc/sys/kernel/pid_max) + 1))
  bash -c 'exec 9>>"$1/.run.lock"; flock 9
printf "pid=%s\npidns=%s\ncommand=a run that has ended\nstarted=2026-11-20T00:00:00Z\ntoken=0123456789abcdef0123456789abcdef\n" "$2" "$(readlink /proc/self/ns/pid)" >"$1/.run.lock"
echo ready >"$3"; exec sleep 300' holder "$root" "$gone" "$ready" >/dev/null 2>&1 &
  holder=$!
  pids+=("$holder")
  wait_for "$ready" || { fail "the holder wrote no record"; return; }
  out=$(timeout 20 env -u LOADTEST_LOCK_TOKEN PATH="${stuck_tr}:$PATH" bash -c 'source "$1"; lock_root "$2"; echo ADMITTED' \
    caller "$helper" "$root" 2>&1 9>&-) || status=$?
  ((status != 124)) ||
    { fail "lock_root's refusal did not come within 20 s: its look for the holders waited on a command line that never answers"; return; }
  [[ $out != *ADMITTED* ]] || { fail "a caller was admitted to a root another run holds"; return; }
  [[ $out =~ stopped\ at\ its\ [0-9.]+\ s\ bound ]] ||
    { fail "lock_root's refusal does not name the bound its look for the holders ended at"; return; }
  out=""
  printf 'PASS: lock_root'"'"'s look for the holders ends at its bound and the refusal names it\n'
}

# case_port_look_bounded: port_holder's look for a listener reads a link of every descriptor of
# every process of its proc root, so check_port holds it to PORT_HOLDER_SECONDS and names that bound
# when the look ends there. The proc root is a listener's alone, as case_holder_names_escaped's is;
# `readlink` and `find` never answer. check_port runs from stack.sh with its neighbours stubbed:
# the port is open and none of the stack's. A 20 s timeout of the case's own catches a look that
# waits on a link unbounded.
case_port_look_bounded() {
  out=""
  local dir="${temporary_dir}/port-bounded" listener port status=0
  mkdir -p "$dir/proc/net"
  "$(python3 -c 'import sys; print(sys.executable)')" -c '
import os, socket, sys, time
s = socket.socket()
s.bind(("127.0.0.1", 0))
s.listen()
open(sys.argv[1] + ".tmp", "w").write(str(s.getsockname()[1]))
os.replace(sys.argv[1] + ".tmp", sys.argv[1])
time.sleep(300)
' "$dir/port" >/dev/null 2>&1 &
  listener=$!
  pids+=("$listener")
  wait_for "$dir/port" || { fail "the listener wrote no port"; return; }
  port=$(<"$dir/port")
  ln -s /proc/net/tcp "$dir/proc/net/tcp"
  ln -s /proc/net/tcp6 "$dir/proc/net/tcp6"
  ln -s "/proc/$listener" "$dir/proc/$listener"
  out=$(timeout 20 env proc_root="$dir/proc" PATH="${stuck_links}:$PATH" bash -c 'source "$1"
eval "$(sed -n "/^port_holder() {/,/^}/p;/^port_holder_walk() {/,/^}/p;/^check_port() {/,/^}/p" "$2")"
PORT_HOLDER_SECONDS=2
declare -A PORT_VAR=([proxy]=LOADTEST_PROXY_PORT)
listened=$3
port() { echo "$listened"; }
port_in_ephemeral() { return 1; }
port_open() { return 0; }
port_block() { echo 7; }
default_port() { echo 1; }
die() { echo "DIE: $*"; exit 1; }
check_port proxy proxy' check "$helper" "$here/stack.sh" "$port" 2>&1) || status=$?
  ((status != 124)) ||
    { fail "check_port did not refuse within 20 s: port_holder waited on a descriptor's link that never answers"; return; }
  [[ $out == *"stopped at its 2 s bound"* ]] ||
    { fail "check_port's refusal does not name the bound port_holder's look ended at"; return; }
  out=""
  printf 'PASS: port_holder'"'"'s look ends at check_port'"'"'s bound and the refusal names it\n'
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
case_holder_names_escaped || failed=$((failed + 1))
case_holder_look_bounded || failed=$((failed + 1))
case_port_look_bounded || failed=$((failed + 1))
case_ports_below_ephemeral_range || failed=$((failed + 1))
if ((failed > 0)); then
  printf 'FAIL: %d of 14 cases failed\n' "$failed" >&2
  exit 1
fi
