# shellcheck shell=bash
# common.sh: what run-step.sh, run-series.sh and stack.sh share (README.md). They source it. Not
# executable.
#
# die MESSAGE prints MESSAGE, prefixed with the script's name, and exits 1. When
# LOADTEST_DIE_FILE names a file, die also writes that line there, in place of what the file held:
# run-step.sh points it at a file of its own for each attempt, so the refusal of a stack.sh it
# starts, or of a command substitution, reaches the step's not-run record. note MESSAGE prints
# MESSAGE with the same prefix and goes on. usage prints the script's header comment, from its
# second line to its `set -euo` line, and exits 2. abs_file FLAG FILE prints FILE's absolute path
# and dies, naming FLAG, when FILE is not a file. sha256_of FILE prints FILE's sha256, and
# joined_sha256 FILE... the sha256 of the files' concatenation (stack.sh's builds, replay.sh's
# parts).
#
# resolve_paths sets SRC to LOADTEST_SRC (default: the checkout holding this file) and ROOT to
# LOADTEST_ROOT (default ${TMPDIR:-/tmp}/dispatch-loadtest, created), both as physical paths, and
# exports them as LOADTEST_SRC and LOADTEST_ROOT. It sets DISPATCH_SRC, the checkout envoy-dispatch
# is built from, to LOADTEST_DISPATCH_SRC (default SRC).
#
# stack_ports ROOT: the stack's ports for ROOT, "KEY PORT" a line. The defaults follow the root's
# physical path (port_block, default_port): a root always gets the same block of 10 ports, and two
# roots get two blocks out of 200, so they share every port about 1 pair of roots in 200 (the block
# is a hash modulo 200, not a per-root reservation). Each LOADTEST_*_PORT (PORT_VAR) overrides its
# own port. So two Legion roles of one pod, each with its own LOADTEST_ROOT, almost always run a
# stack each with no port variable set; when their blocks do collide, check_port refuses the second
# stack and the roles set the LOADTEST_*_PORT variables (or pick another root). stack.sh setup
# records the ports in ROOT/stack.json, and `stack.sh status` prints them. The blocks sit below the
# machine's ephemeral range (port_base), and stack_ports refuses, naming both ranges, when they
# cannot.
#
# check_ephemeral_ports PORTS: refuse a port of PORTS that lies inside the machine's ephemeral
# port range (EPHEMERAL_RANGE_FILE), which an outbound connection may hold when the stack comes
# to bind it. stack.sh setup runs it over the ports it records.
#
# CPU pinning (README.md, Environment): LOADTEST_SERVER_CPUS and LOADTEST_GENERATOR_CPUS are two
# CPU lists as `taskset -c` reads them, empty by default, and then nothing is pinned. When set,
# stack.sh starts each server-side process (Postgres, the latency proxies, NATS, the listener and
# Dispatch) under `taskset -c LOADTEST_SERVER_CPUS` and every loadgen under `taskset -c
# LOADTEST_GENERATOR_CPUS` (pin_words). check_cpu_lists refuses a list taskset refuses and two
# lists that share a CPU, naming both, and settles the generator's list (generator_cpus): under the
# server list and the pod's CPU quota (quota_cpus), `auto` and an unset list take the pod's CPUs
# the quota leaves the generator, and a list too wide for the quota is refused, since taskset
# reserves nothing inside a quota; gomaxprocs gives Dispatch's GOMAXPROCS, the server list's
# CPU count when it is set (server_cpu_count), and refuses a LOADTEST_GOMAXPROCS that differs from
# that count; check_cpu_affinity refuses a runtime that does not enforce the pin (gVisor sets the
# mask and ignores it), timing two busy loops pinned to one CPU against the same two unpinned
# (affinity_verdict decides from the two times), up to AFFINITY_TRIES times, and keeps a verdict
# that the pin binds in ROOT/affinity.json for the later steps of the same boot. run-step.sh and
# run-series.sh run all three before anything starts (check_cpu_pinning).
#
# lock_root ROOT: one run at a time per root (README.md, "One run at a time"). It takes an
# exclusive, non-blocking flock on ROOT/.run.lock and keeps it on file descriptor 9. The lock lasts
# until every process that holds that descriptor has exited: the holder and the children that
# inherit it. Once locked, the holder empties the lock file, then writes its pid, its PID namespace,
# command, start time and a random token into it with one printf, and exports the token as
# LOADTEST_LOCK_TOKEN.
#
# A holder's own child (run-series.sh's run-step.sh, a run's stack.sh) goes on without locking when
# its LOADTEST_LOCK_TOKEN equals the file's token and its descriptor 9 is the inherited locked one:
# it keeps that descriptor and sets LOCK_INHERITED=true. So a series step, and the loadgen run that
# it starts, keep the root when run-series.sh is killed. Any other caller that sees a live holder is
# refused at once; while it sees none, it tries the lock for up to 3 s, and is admitted when the
# lock is freed in that time. It tries on descriptor 8 and moves the lock to 9 only once it has it,
# so a waiting caller never holds descriptor 9 on the file. A refused caller exits 1 before it
# touches the stack, and the one message it dies with names the root and the holder: the record's
# pid, once the file holds a complete record whose pid runs in this PID namespace; or else the
# processes here that still hold descriptor 9 on the file (the children of a holder that has
# exited); or else, when this PID namespace shows neither (a holder in another PID namespace, such
# as another Legion role in this pod on a shared root), the pid, start time and command that the
# lock file records for that run, and that the lock file must stay. The record's pid is trusted
# only when the record's PID namespace is this one. stack.sh closes descriptor 9 in every stack
# process that it starts, so no stack process keeps the lock.
#
# prepare_stack runs stack.sh setup (an incremental build of SRC, and of DISPATCH_SRC's
# envoy-dispatch), start and seed, each of which does nothing when its work is already done, and the
# migration check: a Dispatch build that lacks a migration the seed holds is refused with exit 1,
# and one that holds migrations the seed lacks is noted. Then it sets BUILD to `stack.sh build-info`
# ("<commit> <dirty>"). run-step.sh calls it before a step that is not part of a series;
# run-series.sh calls it once, before its first step.
#
# The quiet-box gate (README.md, "A quiet box before each step"): wait_for_quiet_box reads the box's
# busy fraction with box_busy_reading (`stack.sh loadgen box-busy`, the measure of meta.json's
# box_busy_before) and returns once a reading is at or under LOADTEST_MAX_BUSY_BEFORE (default
# 0.10). A reading above it is noted, and the gate reads again after QUIET_READ_EVERY seconds
# (quiet_pause), until its pauses add up to LOADTEST_QUIET_WAIT seconds (default 900); past that it
# dies naming every reading, the bar and the wait. It keeps them in QUIET_READINGS, QUIET_BAR and
# QUIET_WAIT, which run-step.sh passes to loadgen run and writes into a refused step's not-run
# record. run-step.sh runs it after the lock and the stack, before its own box_busy_before read.
#
# A step's window (README.md, "Acting at a moment of the window"): not_run_record DIR is the path
# of the not-run record of the step whose directory is DIR, DIR.not-run.json, which run-step.sh
# writes at every exit before the step's window opens. window_start DIR [AFTER] prints the window
# start that DIR/meta.json names, once it names one; with AFTER, a Unix time, only a start at or
# after it. pid_started_at PID prints the Unix second at which process PID started, from /proc.
# wait_for_window STEP PID waits for the window of the step that process PID runs: it prints the
# window start once meta.json names one, and returns 1 as soon as PID has ended without it, saying
# why from the step's not-run record. A script that acts at a moment of the window waits with it.
#
# The replay's inputs (README.md, The replay): stack.sh replay-get and cluster.sh replay-prepare
# both download and verify the manifest's parts with replay_fetch_parts and replay_get_bundle
# below. The manifest names no issue; LOADTEST_REPLAY_ISSUE names the one the parts are file
# artifacts of, and nothing is read or downloaded without it.

die() {
  printf '%s: %s\n' "${0##*/}" "$*" >&2
  if [ -n "${LOADTEST_DIE_FILE:-}" ]; then
    { printf '%s: %s\n' "${0##*/}" "$*" >"$LOADTEST_DIE_FILE"; } 2>/dev/null || true
  fi
  exit 1
}

note() { printf '%s: %s\n' "${0##*/}" "$*" >&2; }

# abs_file FLAG FILE: FILE's absolute path, refused when it is not a file.
abs_file() {
  [ -f "$2" ] || die "$1 names $2, which is not a file"
  printf '%s/%s\n' "$(cd -- "$(dirname -- "$2")" && pwd -P)" "$(basename -- "$2")"
}

# sha256_of FILE: FILE's sha256, the hex alone.
sha256_of() { sha256sum -- "$1" | cut -d' ' -f1; }

# joined_sha256 FILE...: the sha256 of the files' concatenation.
joined_sha256() { cat -- "$@" | sha256sum | cut -d' ' -f1; }

usage() {
  sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//' >&2
  exit 2
}

resolve_paths() {
  SRC=${LOADTEST_SRC:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd -P)}
  ROOT=${LOADTEST_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest}
  mkdir -p -- "$ROOT"
  ROOT=$(cd -- "$ROOT" && pwd -P)
  export LOADTEST_SRC=$SRC LOADTEST_ROOT=$ROOT
  DISPATCH_SRC=${LOADTEST_DISPATCH_SRC:-$SRC}
}

# ---- the stack's ports ---------------------------------------------------------------------------

# The stack's default ports follow LOADTEST_ROOT, so two roots on one machine (two Legion roles of
# one pod, each with its own root and its own PID namespace) almost always run side by side with no
# port variable set: each root hashes to one of PORT_BLOCKS blocks, so two roots land in one block
# about 1 pair in 200, and then every port of the two stacks is the same one (check_port refuses
# the second stack; the remedy is the LOADTEST_*_PORT variables). PORT_KEYS names stack.json's
# ports, PORT_VAR each one's environment variable and PORT_OFFSET its place in a root's block of
# ports.
PORT_KEYS=(postgres proxy proxy_stats nats nats_monitor listener dispatch pprof metrics envoy_proxy)
declare -A PORT_VAR=(
  [postgres]=LOADTEST_PG_PORT [proxy]=LOADTEST_PROXY_PORT [proxy_stats]=LOADTEST_PROXY_STATS_PORT
  [nats]=LOADTEST_NATS_PORT [nats_monitor]=LOADTEST_NATS_MONITOR_PORT
  [listener]=LOADTEST_LISTENER_PORT [dispatch]=LOADTEST_DISPATCH_PORT
  [pprof]=LOADTEST_PPROF_PORT [metrics]=LOADTEST_METRICS_PORT
  [envoy_proxy]=LOADTEST_ENVOY_PROXY_PORT
)
declare -A PORT_OFFSET=(
  [postgres]=0 [proxy]=1 [nats]=2 [nats_monitor]=3 [listener]=4 [dispatch]=5 [pprof]=6 [metrics]=7
  [proxy_stats]=8 [envoy_proxy]=9
)
PORT_BASE_DEFAULT=10000
PORT_STRIDE=10
PORT_BLOCKS=200

# port_block ROOT: ROOT's block of ports, an index k in [0, PORT_BLOCKS): the first 8 hex digits of
# the sha256 of ROOT's physical path, modulo PORT_BLOCKS. The same root always gets the same block.
port_block() {
  local hex
  hex=$(printf '%s' "$1" | sha256sum | cut -c1-8)
  printf '%s\n' "$((16#$hex % PORT_BLOCKS))"
}

# EPHEMERAL_RANGE_FILE is where the machine's ephemeral port range is read from: two numbers on
# one line, "LOW HIGH", as net.ipv4.ip_local_port_range holds them. A test points
# LOADTEST_PORT_RANGE_FILE at a file of its own, for a range this machine does not have;
# nothing here ever writes the file.
EPHEMERAL_RANGE_FILE=${LOADTEST_PORT_RANGE_FILE:-/proc/sys/net/ipv4/ip_local_port_range}

# ephemeral_range: the machine's ephemeral port range as "LOW HIGH", or nothing when
# EPHEMERAL_RANGE_FILE cannot be read or does not hold two numbers (a kernel or a container that
# does not publish it), in which case no port is refused for it. The kernel takes the source port
# of every outgoing connection from this range, so a stack port inside it can be taken by any
# connection of the stack before its own process binds it, which then fails with "address already
# in use".
ephemeral_range() {
  local low="" high="" rest=""
  [ -r "$EPHEMERAL_RANGE_FILE" ] || return 0
  read -r low high rest <"$EPHEMERAL_RANGE_FILE" || true
  [[ $low =~ ^[0-9]+$ && $high =~ ^[0-9]+$ && -z $rest ]] || return 0
  ((low <= high)) || return 0
  printf '%s %s\n' "$low" "$high"
}

# port_base: the first port of block 0. The PORT_BLOCKS blocks of PORT_STRIDE ports take
# PORT_BLOCKS * PORT_STRIDE ports from it (2000). It is PORT_BASE_DEFAULT (10000) when they end
# below the ephemeral range, which holds for Linux's default range (32768-60999) and a gVisor
# pod's (16000-65535). Otherwise it is the 2000 ports just below the range. When no range can be
# read it is the default. When the range starts too low for 2000 ports above 1023, it prints a
# refusal naming both ranges on stderr and returns 1.
port_base() {
  local span=$((PORT_BLOCKS * PORT_STRIDE)) range low high base
  range=$(ephemeral_range)
  if [ -z "$range" ]; then
    printf '%s\n' "$PORT_BASE_DEFAULT"
    return 0
  fi
  read -r low high <<<"$range"
  if ((PORT_BASE_DEFAULT + span <= low)); then
    printf '%s\n' "$PORT_BASE_DEFAULT"
    return 0
  fi
  base=$((low - span))
  if ((base < 1024)); then
    printf 'loadtest: the stack needs %d ports below the ephemeral range %d-%d (ip_local_port_range) and above 1023, and the range starts too low for them; set every LOADTEST_*_PORT to a port outside %d-%d\n' \
      "$span" "$low" "$high" "$low" "$high" >&2
    return 1
  fi
  printf '%s\n' "$base"
}

# port_in_ephemeral PORT: PORT is inside this machine's ephemeral range.
port_in_ephemeral() {
  local range low high
  range=$(ephemeral_range)
  [ -n "$range" ] || return 1
  read -r low high <<<"$range"
  (($1 >= low && $1 <= high))
}

# default_port KEY BLOCK [BASE]: the default port of KEY in block BLOCK:
#
#   BASE + BLOCK * PORT_STRIDE + PORT_OFFSET[KEY] = BASE + 10k + 0..9
#
# BASE is port_base's unless given. The 10 offsets differ and all are below the stride, so no two
# ports of a block and no two ports of two different blocks ever meet (two roots in one block hold
# the same 10 ports); every port is between BASE and BASE + 1999, inside 1024-65535 and below the
# ephemeral range (port_base), and at the default base between 10000 and 11999, below every
# ephemeral range a machine is likely to have. The base was 15000, whose blocks reached 16997: on a
# pod whose ephemeral range starts at 16000 an outbound connection of the load test took a stack
# port between steps and the next bind failed with "address already in use". 10000 also clears the
# thousand ports the NATS test rig's gates take just under that range (internal/testnats,
# gatePorts). A key added to the table takes the next free offset rather than a place in the
# list's order, so the ports every other key already has stay where they were.
default_port() {
  local base=${3:-}
  if [ -z "$base" ]; then
    base=$(port_base) || return 1
  fi
  printf '%s\n' "$((base + $2 * PORT_STRIDE + PORT_OFFSET[$1]))"
}

# check_ephemeral_ports PORTS: die when a port of PORTS, "KEY PORT" a line as stack_ports prints
# them, lies inside the machine's ephemeral range. A port in that range is one the kernel hands
# to an outbound connection - the load generator's own, between one step and the next - and the
# stack then cannot bind it ("bind: address already in use"). The refusal names the range, every
# port inside it and the LOADTEST_*_PORT variable that moves each. stack.sh setup runs it over
# the ports it records, once write_stack_json has moved a recorded port inside the range to this
# root's default, so a port it refuses there is one a LOADTEST_*_PORT sets.
check_ephemeral_ports() {
  local range low high key p inside=""
  range=$(ephemeral_range)
  [ -n "$range" ] || return 0
  read -r low high <<<"$range"
  while read -r key p; do
    [ -n "$key" ] || continue
    if ((p >= low && p <= high)); then
      inside+="${inside:+, }$key $p (${PORT_VAR[$key]:-no LOADTEST_*_PORT variable})"
    fi
  done <<<"$1"
  [ -z "$inside" ] ||
    die "these stack ports lie inside this machine's ephemeral port range $low-$high ($EPHEMERAL_RANGE_FILE): $inside. The kernel gives ports in that range to outbound connections, so one of them is taken while the stack runs and the next start fails with 'bind: address already in use'. Set each port's LOADTEST_*_PORT variable to a free port below $low and rerun stack.sh setup with them set"
}

# ---- CPU pinning ---------------------------------------------------------------------------------

# AFFINITY_MIN_SLOWDOWN is how much slower two busy loops pinned to one CPU must run than the same
# two unpinned for the pin to count as binding. On a box with two free CPUs a binding pin halves
# the CPU the pair gets, so it runs about 2x slower; a runtime that ignores the mask runs it as
# fast as unpinned (gVisor on a Legion pod: 9.0 s pinned against 8.5 s unpinned, 1.06x). 1.6 sits
# between the two with room for a box that is not idle. AFFINITY_LOOP_ITERATIONS is each loop's
# length: about 2 s of one CPU on a Legion pod.
AFFINITY_MIN_SLOWDOWN=1.6
AFFINITY_LOOP_ITERATIONS=1000000

# AFFINITY_TRIES is how many times check_cpu_affinity measures before it refuses: a step is refused
# only when no try reaches AFFINITY_MIN_SLOWDOWN. A noisy try does not pass a pin that does not
# bind: on a runtime that ignores the mask the pinned pair is the unpinned pair run again, so its
# ratio sits near 1 (gVisor's 1.06x), and to reach the bar other work would have to slow the pinned
# run alone by 60%, while work that runs through both pairs pulls the ratio toward 1. What retrying
# does pass is a pin that binds on a box still busy with what an earlier step left: on a runc bed
# after a 10x step, one try read 1.39 s pinned against 1.13 s unpinned (1.23x).
AFFINITY_TRIES=3

# BOOT_ID_FILE is where the boot's id is read from (affinity_boot_id), the kernel's random id of
# this boot: a kept verdict (check_cpu_affinity) is taken only on the boot it was measured on. A
# test points LOADTEST_BOOT_ID_FILE at a file of its own.
BOOT_ID_FILE=${LOADTEST_BOOT_ID_FILE:-/proc/sys/kernel/random/boot_id}

# PROC_VERSION_FILE is where the kernel's name is read from (affinity_runtime). A test points
# LOADTEST_PROC_VERSION_FILE at a file of its own.
PROC_VERSION_FILE=${LOADTEST_PROC_VERSION_FILE:-/proc/version}

# CGROUP_ROOT is where the pod's cgroup files are read from: cgroup v2's cpu.max and
# cpuset.cpus.effective, or cgroup v1's cpu/cpu.cfs_quota_us and cpu/cpu.cfs_period_us (cpu_quota,
# pod_cpus). A test points LOADTEST_CGROUP_ROOT at a directory of its own.
CGROUP_ROOT=${LOADTEST_CGROUP_ROOT:-/sys/fs/cgroup}

# cpu_list_cpus LIST: each CPU of LIST, one a line, from its comma-separated N, N-M and N-M:S
# items as taskset -c reads them; status 1 for an item of another form.
cpu_list_cpus() {
  local item n lo hi step
  local -a items
  IFS=, read -ra items <<<"$1"
  for item in "${items[@]}"; do
    if [[ $item =~ ^([0-9]+)$ ]]; then
      printf '%s\n' "$((10#${BASH_REMATCH[1]}))"
    elif [[ $item =~ ^([0-9]+)-([0-9]+)(:([1-9][0-9]*))?$ ]]; then
      lo=$((10#${BASH_REMATCH[1]})) hi=$((10#${BASH_REMATCH[2]})) step=$((10#${BASH_REMATCH[4]:-1}))
      for ((n = lo; n <= hi; n += step)); do printf '%s\n' "$n"; done
    else
      return 1
    fi
  done
}

# cpu_count LIST: the number of distinct CPUs in LIST, read as cpu_list_cpus reads it; status 1 for
# a list it cannot read.
cpu_count() {
  local cpus cpu
  local -A distinct=()
  cpus=$(cpu_list_cpus "$1") || return 1
  for cpu in $cpus; do distinct[$cpu]=1; done
  printf '%s\n' "${#distinct[@]}"
}

# cpus_text N: "N CPU" or "N CPUs".
cpus_text() { if [ "$1" = 1 ]; then printf '1 CPU\n'; else printf '%s CPUs\n' "$1"; fi; }

# check_cpu_list VAR: die when the list VAR holds is one taskset refuses (a CPU this box lacks, or
# no list at all), naming VAR, the list and taskset's own words, or one cpu_list_cpus cannot read;
# nothing for an unset VAR.
check_cpu_list() {
  local var=$1 list=${!1:-} err
  [ -n "$list" ] || return 0
  command -v taskset >/dev/null || die "$var='$list' is set, and taskset (util-linux) is required to pin to it"
  err=$(taskset -c "$list" true 2>&1) || die "$var='$list' is a CPU list taskset refuses: ${err:-taskset exited non-zero}"
  cpu_list_cpus "$list" >/dev/null ||
    die "$var='$list' is a list taskset takes but this script cannot read; write it as N, N-M or N-M:S items separated by commas"
}

# check_cpu_lists: check_cpu_list over LOADTEST_SERVER_CPUS, then LOADTEST_GENERATOR_CPUS becomes
# the list generator_cpus settles (`auto`, or an unset list under the server list and a CPU quota,
# becomes the list it stands for) and is exported, so every stack.sh and loadgen this shell starts
# runs on it and `loadgen run` records it; then check_cpu_list over that list, and die when the two
# lists share a CPU, naming both lists and the CPUs they share: the generator is pinned so it runs
# on CPU the server side does not.
check_cpu_lists() {
  local list
  check_cpu_list LOADTEST_SERVER_CPUS
  list=$(generator_cpus) || exit 1
  [ "$list" = "${LOADTEST_GENERATOR_CPUS:-}" ] || export LOADTEST_GENERATOR_CPUS=$list
  check_cpu_list LOADTEST_GENERATOR_CPUS
  [ -n "${LOADTEST_SERVER_CPUS:-}" ] && [ -n "${LOADTEST_GENERATOR_CPUS:-}" ] || return 0
  local shared
  shared=$(comm -12 <(cpu_list_cpus "$LOADTEST_SERVER_CPUS" | sort -u) <(cpu_list_cpus "$LOADTEST_GENERATOR_CPUS" | sort -u) |
    sort -n | paste -sd, -)
  [ -z "$shared" ] ||
    die "LOADTEST_SERVER_CPUS='$LOADTEST_SERVER_CPUS' and LOADTEST_GENERATOR_CPUS='$LOADTEST_GENERATOR_CPUS' overlap on CPU $shared: the generator must have CPUs of its own, apart from the server side's"
}

# server_cpu_count: the number of distinct CPUs in LOADTEST_SERVER_CPUS (cpu_count); nothing when
# the list is unset, status 1 for a list it cannot read.
server_cpu_count() {
  [ -n "${LOADTEST_SERVER_CPUS:-}" ] || return 0
  cpu_count "$LOADTEST_SERVER_CPUS"
}

# cpu_quota: the pod's CPU quota as "QUOTA PERIOD", in microseconds: cgroup v2's cpu.max under
# CGROUP_ROOT as it reads ("max PERIOD" is no quota), else cgroup v1's cpu/cpu.cfs_quota_us and
# cpu/cpu.cfs_period_us in that shape (QUOTA -1 is no quota); nothing when neither can be read or
# holds a quota and a period. meta.json's cpu_quota is the same text, which loadgen run reads.
cpu_quota() {
  local q="" p="" rest=""
  if [ -e "$CGROUP_ROOT/cpu.max" ]; then
    { read -r q p rest <"$CGROUP_ROOT/cpu.max"; } 2>/dev/null || true
  else
    { read -r q <"$CGROUP_ROOT/cpu/cpu.cfs_quota_us"; } 2>/dev/null || true
    { read -r p <"$CGROUP_ROOT/cpu/cpu.cfs_period_us"; } 2>/dev/null || true
  fi
  [[ $q =~ ^(max|-1|[1-9][0-9]*)$ && $p =~ ^[1-9][0-9]*$ && -z $rest ]] || return 0
  printf '%s %s\n' "$q" "$p"
}

# quota_cpus: the pod's CPU quota (cpu_quota) in whole CPUs, rounded down; nothing when the pod has
# no quota. meta.json records it as quota_cpus.
quota_cpus() {
  local q p
  read -r q p <<<"$(cpu_quota)"
  [[ $q =~ ^[1-9][0-9]*$ ]] || return 0
  printf '%s\n' "$((q / p))"
}

# pod_cpus: the CPUs the pod may run on, one a line: cgroup v2's cpuset.cpus.effective under
# CGROUP_ROOT, else this shell's own affinity list (taskset -cp).
pod_cpus() {
  local list=""
  { read -r list <"$CGROUP_ROOT/cpuset.cpus.effective"; } 2>/dev/null || true
  [ -n "$list" ] || list=$(taskset -cp $$ 2>/dev/null) || return 1
  cpu_list_cpus "${list##*: }"
}

# auto_generator_cpus [COUNT]: the pod's CPUs (pod_cpus) outside LOADTEST_SERVER_CPUS, lowest
# first, comma separated: the first COUNT of them, or every one without COUNT. Status 1 when there
# is none.
auto_generator_cpus() {
  local want=${1:-} cpu out="" n=0
  local -A server=()
  for cpu in $(cpu_list_cpus "$LOADTEST_SERVER_CPUS"); do server[$cpu]=1; done
  for cpu in $(pod_cpus | sort -nu); do
    [ -z "${server[$cpu]:-}" ] || continue
    out+="${out:+,}$cpu"
    n=$((n + 1))
    [ -z "$want" ] || ((n < want)) || break
  done
  [ -n "$out" ] || return 1
  printf '%s\n' "$out"
}

# quota_text QUOTA_CPUS: the pod's CPU quota of QUOTA_CPUS whole CPUs as generator_cpus names it,
# with the text it was read from and where.
quota_text() { printf "the pod's CPU quota of %s ('%s' under %s)\n" "$(cpus_text "$1")" "$(cpu_quota)" "$CGROUP_ROOT"; }

# generator_cpus: the list the generator runs on, which check_cpu_lists makes
# LOADTEST_GENERATOR_CPUS (README.md, CPU pinning). taskset reserves nothing inside a CPU quota:
# CFS throttles every process of the pod once together they pass it, the pinned server side with
# them. So under LOADTEST_SERVER_CPUS and a quota (quota_cpus), `auto` and an unset list are the
# first quota_cpus less the server list's count of the pod's CPUs outside the server list, at
# least one (auto_generator_cpus), which it names; and a given list of more CPUs than that count is
# refused, naming the quota, both lists and the list `auto` gives. Without a quota an unset or
# given list stands, and `auto` is every CPU of the pod outside the server list; `auto` without the
# server list is refused. It prints nothing for a list that stays unset. A given list that fits,
# as a script a run starts gets the list its parent settled, costs a read of the quota and two CPU
# counts: stack.sh restart-dispatch runs this inside the window.
generator_cpus() {
  local gen=${LOADTEST_GENERATOR_CPUS:-} server=${LOADTEST_SERVER_CPUS:-} quota="" servers room="" auto n what
  [ -n "$server" ] || [ "$gen" != auto ] ||
    die "LOADTEST_GENERATOR_CPUS=auto gives the generator the pod's CPUs outside LOADTEST_SERVER_CPUS, which is unset: set the server list too, or give the generator a list of its own"
  [ -z "$server" ] || quota=$(quota_cpus)
  if [ -z "$quota" ] && [ "$gen" != auto ]; then
    [ -z "$gen" ] || printf '%s\n' "$gen"
    return 0
  fi
  if [ -n "$quota" ]; then
    servers=$(server_cpu_count)
    room=$((quota - servers))
    ((room >= 1)) || room=1
  fi
  if [ -n "$gen" ] && [ "$gen" != auto ]; then
    # A list this script cannot read is check_cpu_list's to refuse.
    n=$(cpu_count "$gen") || { printf '%s\n' "$gen" && return 0; }
    if ((n > room)); then
      auto=$(auto_generator_cpus "$room") || auto=""
      die "LOADTEST_GENERATOR_CPUS='$gen', $(cpus_text "$n"), and LOADTEST_SERVER_CPUS='$server', $(cpus_text "$servers"), ask for $((n + servers)) CPUs, more than $(quota_text "$quota"): CFS throttles every process of the pod once together they pass the quota, the pinned server side with them, since taskset reserves no CPU inside it. Set LOADTEST_GENERATOR_CPUS=auto, which is '${auto}' here, or leave it unset"
    fi
    printf '%s\n' "$gen"
    return 0
  fi
  auto=$(auto_generator_cpus "$room") ||
    die "the pod's CPUs ($(pod_cpus | paste -sd, -)) hold none outside LOADTEST_SERVER_CPUS='$server', so the generator would have no CPU of its own"
  what="LOADTEST_GENERATOR_CPUS=auto"
  [ -n "$gen" ] || what="LOADTEST_GENERATOR_CPUS is unset, which beside LOADTEST_SERVER_CPUS under a CPU quota is auto"
  if [ -n "$quota" ]; then
    n=$(cpu_count "$auto")
    note "$what: the generator runs on $auto, $(cpus_text "$n") of the pod outside LOADTEST_SERVER_CPUS='$server', $(cpus_text "$servers"), so the two lists ask for $((n + servers)) CPUs against $(quota_text "$quota")"
  else
    note "$what: the generator runs on $auto, every CPU of the pod outside LOADTEST_SERVER_CPUS='$server'; the pod has no CPU quota (under $CGROUP_ROOT)"
  fi
  printf '%s\n' "$auto"
}

# gomaxprocs: Dispatch's GOMAXPROCS. With LOADTEST_SERVER_CPUS set it is the number of CPUs in
# that list, the CPUs Dispatch is pinned to: stack.sh sets GOMAXPROCS in Dispatch's environment,
# which overrides Go's own default (since Go 1.25 the CPU affinity mask and the cgroup quota), and
# nproc is the unpinned shell's count, which gave a Dispatch pinned to two CPUs GOMAXPROCS 32 and
# 48. Without the list it is nproc. LOADTEST_GOMAXPROCS (run-step.sh --gomaxprocs) overrides the
# default; it is refused unless a positive integer, and, with the list set, refused unless it is
# the list's count, naming both: a Dispatch pinned to N CPUs with M processors is not the bed any
# reading names.
gomaxprocs() {
  local cpus n
  cpus=$(server_cpu_count) ||
    die "LOADTEST_SERVER_CPUS='$LOADTEST_SERVER_CPUS' is a list this script cannot read; write it as N, N-M or N-M:S items separated by commas"
  n=${LOADTEST_GOMAXPROCS:-${cpus:-$(nproc)}}
  [[ $n =~ ^[1-9][0-9]*$ ]] || die "LOADTEST_GOMAXPROCS='$n' is not a positive integer"
  [ -z "$cpus" ] || [ "$n" = "$cpus" ] ||
    die "LOADTEST_GOMAXPROCS=$n (run-step.sh --gomaxprocs) differs from LOADTEST_SERVER_CPUS='$LOADTEST_SERVER_CPUS', $(cpus_text "$cpus"): a Dispatch pinned to $(cpus_text "$cpus") with GOMAXPROCS $n is not the bed any reading names. Leave LOADTEST_GOMAXPROCS unset, and GOMAXPROCS follows the list, or set it to $cpus"
  printf '%s\n' "$n"
}

# gomaxprocs_refusal META: why the step whose meta.json is META is refused, when META records a
# server CPU list and Dispatch's GOMAXPROCS, read back from its environment, is not that list's CPU
# count or could not be read (`loadgen run` refuses such a step before its window). It prints why
# and returns 0; it returns non-zero for any other META, and for one that cannot be read.
gomaxprocs_refusal() {
  jq -er 'select(.server_cpu_count != null and .gomaxprocs != (.server_cpu_count | tostring)) |
    (if .server_cpu_count == 1 then "CPU" else "CPUs" end) as $what |
    (if .gomaxprocs == "" then "unread (\(.gomaxprocs_error))" else .gomaxprocs end) as $procs |
    "Dispatch ran with GOMAXPROCS \($procs) (meta.json gomaxprocs) pinned to LOADTEST_SERVER_CPUS='"'"'\(.server_cpus)'"'"', \(.server_cpu_count) \($what): a Dispatch pinned to \(.server_cpu_count) \($what) with GOMAXPROCS \($procs) is not the bed any reading names"' \
    "$1" 2>/dev/null
}

# pin_words LIST: the words that start a process pinned to LIST (`taskset -c LIST`), one a line;
# nothing for an empty LIST, which pins nothing.
pin_words() { [ -z "$1" ] || printf '%s\n' taskset -c "$1"; }

# busy_pair_seconds [PREFIX...]: the wall-clock seconds two busy loops of AFFINITY_LOOP_ITERATIONS
# take when they run side by side, each started under PREFIX (none: unpinned).
busy_pair_seconds() {
  local start end a b loop="i=0; while ((i < $AFFINITY_LOOP_ITERATIONS)); do ((i++)); done"
  start=$EPOCHREALTIME
  "$@" bash -c "$loop" &
  a=$!
  "$@" bash -c "$loop" &
  b=$!
  wait "$a" "$b"
  end=$EPOCHREALTIME
  awk -v s="$start" -v e="$end" 'BEGIN { printf "%.2f\n", e - s }'
}

# affinity_verdict UNPINNED PINNED: the pinned pair's time over the unpinned pair's, two
# decimals, and status 0 when it is at least AFFINITY_MIN_SLOWDOWN: the pin binds.
affinity_verdict() {
  awk -v u="$1" -v p="$2" -v m="$AFFINITY_MIN_SLOWDOWN" \
    'BEGIN { r = (u > 0) ? p / u : 0; printf "%.2f\n", r; exit !(u > 0 && r >= m) }'
}

# affinity_runtime: "gVisor (<what shows it>)" when PROC_VERSION_FILE or dmesg names a gVisor
# kernel, nothing otherwise.
affinity_runtime() {
  local v
  v=$(cat -- "$PROC_VERSION_FILE" 2>/dev/null || true)
  if grep -qi gvisor <<<"$v"; then
    printf 'gVisor (%s: %s)\n' "$PROC_VERSION_FILE" "$v"
    return 0
  fi
  v=$({ dmesg 2>/dev/null || true; } | grep -i -m1 gvisor || true)
  [ -z "$v" ] || printf 'gVisor (dmesg: %s)\n' "$v"
}

# affinity_refusal CPU UNPINNED PINNED RATIO [UNPINNED PINNED RATIO...]: why a step is refused when
# the pin did not bind: every try's measurement, and the runtime when affinity_runtime names it.
affinity_refusal() {
  local cpu=$1 runtime msg tries=1
  shift
  msg="this runtime ignores CPU affinity: two busy loops pinned to CPU $cpu with taskset took $2 s against $1 s for the same two unpinned (${3}x)"
  shift 3
  while [ $# -ge 3 ]; do
    msg+=", then $2 s against $1 s (${3}x)"
    tries=$((tries + 1))
    shift 3
  done
  ((tries == 1)) || msg+=" in $tries tries"
  msg+=", where a pin that binds holds both on one CPU and makes them at least ${AFFINITY_MIN_SLOWDOWN}x slower"
  runtime=$(affinity_runtime)
  [ -z "$runtime" ] || msg+="; this is $runtime, which records the mask and does not enforce it"
  printf '%s. LOADTEST_SERVER_CPUS and LOADTEST_GENERATOR_CPUS would pin nothing here: unset them, or run where the pin binds (runc)\n' "$msg"
}

# affinity_boot_id: this boot's id, from BOOT_ID_FILE; nothing when the file cannot be read or does
# not hold one.
affinity_boot_id() {
  local id=""
  { read -r id <"$BOOT_ID_FILE"; } 2>/dev/null || true
  [[ ! $id =~ ^[0-9A-Fa-f-]+$ ]] || printf '%s\n' "$id"
}

# check_cpu_affinity: when either list is set, die unless a pin to the first CPU of the server
# list (else the generator's) slows two busy loops by AFFINITY_MIN_SLOWDOWN in one of
# AFFINITY_TRIES tries (affinity_refusal names every try), and note the measurement when it does.
#
# A verdict that the pin binds is kept in ROOT/affinity.json (resolve_paths sets ROOT), with its
# measurement, every try, the CPU and this boot's id (affinity_boot_id), and a later step of the
# same boot takes it and says so instead of measuring again. Whether the runtime enforces the mask
# does not change while the kernel runs, and a step that follows another on the same stack measures
# on a box still busy with what that step left. A kept verdict is taken only on the boot it names
# and only while its own times pass the bar; a boot whose id differs or cannot be read, or a file
# that cannot be read, measures again. A verdict that the pin does not bind is never kept, so the
# next run after a refusal measures again. The file is written whole, by a rename, since a run
# checks the pin before it takes the root's lock.
check_cpu_affinity() {
  local list=${LOADTEST_SERVER_CPUS:-${LOADTEST_GENERATOR_CPUS:-}}
  [ -n "$list" ] || return 0
  local -a cpus tries=()
  mapfile -t cpus < <(cpu_list_cpus "$list")
  local file=$ROOT/affinity.json boot kept at cpu unpinned pinned ratio binds try earlier="" json="" said
  boot=$(affinity_boot_id)
  if [ -n "$boot" ] &&
    kept=$(jq -er --arg b "$boot" 'select(.boot_id == $b) | "\(.at) \(.cpu) \(.unpinned_s) \(.pinned_s)"' "$file" 2>/dev/null); then
    # Two decimals, as busy_pair_seconds prints them: a jq may print 2.10 back as 2.1.
    read -r at cpu unpinned pinned < <(awk '{ printf "%s %s %.2f %.2f\n", $1, $2, $3, $4 }' <<<"$kept")
    if ratio=$(affinity_verdict "$unpinned" "$pinned"); then
      note "CPU affinity binds, as a step measured at $at on this boot ($BOOT_ID_FILE $boot, kept in $file): two busy loops pinned to CPU $cpu took $pinned s against $unpinned s unpinned (${ratio}x); not measured again"
      return 0
    fi
  fi
  for ((try = 1; try <= AFFINITY_TRIES; try++)); do
    unpinned=$(busy_pair_seconds)
    pinned=$(busy_pair_seconds taskset -c "${cpus[0]}")
    binds=true
    ratio=$(affinity_verdict "$unpinned" "$pinned") || binds=false
    json+="${json:+,}{\"unpinned_s\":$unpinned,\"pinned_s\":$pinned,\"ratio\":$ratio}"
    if $binds; then
      said="CPU affinity binds: two busy loops pinned to CPU ${cpus[0]} took $pinned s against $unpinned s unpinned (${ratio}x)"
      ((try == 1)) || said+=" on try $try of $AFFINITY_TRIES, after $earlier"
      if [ -z "$boot" ]; then
        said+="; not kept for later steps: $BOOT_ID_FILE holds no boot id"
      elif jq -n --arg b "$boot" --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --argjson cpu "${cpus[0]}" \
        --argjson u "$unpinned" --argjson p "$pinned" --argjson r "$ratio" --argjson tries "[$json]" \
        '{boot_id: $b, at: $at, cpu: $cpu, unpinned_s: $u, pinned_s: $p, ratio: $r, tries: $tries}' \
        >"$file.$$.tmp" 2>/dev/null && mv -f -- "$file.$$.tmp" "$file"; then
        said+="; kept in $file for the later steps of this boot ($BOOT_ID_FILE $boot)"
      else
        rm -f -- "$file.$$.tmp"
        said+="; not kept for later steps: $file could not be written"
      fi
      note "$said"
      return 0
    fi
    earlier+="${earlier:+, }$pinned s against $unpinned s (${ratio}x)"
    tries+=("$unpinned" "$pinned" "$ratio")
  done
  die "$(affinity_refusal "${cpus[0]}" "${tries[@]}")"
}

# check_cpu_pinning: check_cpu_lists, then gomaxprocs (a LOADTEST_GOMAXPROCS other than the server
# list's CPU count is refused), then check_cpu_affinity.
check_cpu_pinning() {
  check_cpu_lists
  gomaxprocs >/dev/null
  check_cpu_affinity
}

# stack_ports ROOT: ROOT's ports, "KEY PORT" a line in PORT_KEYS' order: the block's default port,
# or the value of the key's PORT_VAR when this environment sets it. stack.sh setup records them in
# ROOT/stack.json, which `stack.sh status` prints. It fails, printing nothing, when a key takes its
# default and port_base refuses.
stack_ports() {
  local block key var base="" out=""
  block=$(port_block "$1")
  for key in "${PORT_KEYS[@]}"; do
    var=${PORT_VAR[$key]}
    if [ -z "${!var:-}" ] && [ -z "$base" ]; then
      base=$(port_base) || return 1
    fi
    out+="$key ${!var:-$(default_port "$key" "$block" "$base")}"$'\n'
  done
  printf '%s' "$out"
}

# lock_field NAME RECORD: the value of NAME= in a lock record.
lock_field() { sed -n "s/^$1=//p" <<<"$2"; }

# lock_pidns: this process's PID namespace ("pid:[N]"), empty when /proc does not show it.
lock_pidns() { readlink /proc/self/ns/pid 2>/dev/null || true; }

# lock_pid_alive PID: PID is a running process, not a zombie.
lock_pid_alive() {
  local stat
  [[ $1 =~ ^[0-9]+$ ]] || return 1
  { read -r stat <"/proc/$1/stat"; } 2>/dev/null || return 1
  [[ ${stat##*) } != Z* ]]
}

# lock_record_ns RECORD: where RECORD was written: "here" (this PID namespace), "elsewhere" (another
# one) or "unknown" (the record names no PID namespace).
lock_record_ns() {
  local ns
  ns=$(lock_field pidns "$1")
  if [ -z "$ns" ]; then
    echo unknown
  elif [ "$ns" = "$(lock_pidns)" ]; then
    echo here
  else
    echo elsewhere
  fi
}

# lock_record_runs RECORD: RECORD was written in this PID namespace and its pid runs. A pid of
# another PID namespace names an unrelated process here, or none.
lock_record_runs() {
  [ "$(lock_record_ns "$1")" = here ] && lock_pid_alive "$(lock_field pid "$1")"
}

# lock_holders FILE: "pid N (command)" of each process whose descriptor 9 is open on FILE, joined
# with ", ".
lock_holders() {
  local file=$1 dir cmd out=""
  for dir in /proc/[0-9]*; do
    [ "$dir/fd/9" -ef "$file" ] 2>/dev/null || continue
    cmd=$(tr '\0' ' ' <"$dir/cmdline" 2>/dev/null) || continue
    out+="${out:+, }pid ${dir#/proc/} (${cmd% })"
  done
  [ -z "$out" ] || printf '%s\n' "$out"
}

# lock_take FILE: this process has just locked FILE on descriptor 9; write its record.
lock_take() {
  local file=$1 command token
  # Empty the file before anything else, so that a refused caller never reads the previous
  # holder's record, still complete in the file, as this one's.
  : >"$file"
  command=$(tr '\0' ' ' <"/proc/$$/cmdline")
  token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
  printf 'pid=%s\npidns=%s\ncommand=%s\nstarted=%s\ntoken=%s\n' "$$" "$(lock_pidns)" "${command% }" \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$token" >"$file"
  export LOADTEST_LOCK_TOKEN=$token
}

lock_root() {
  local root=$1
  local file=$root/.run.lock
  command -v flock >/dev/null || die "flock (util-linux) is required"
  mkdir -p -- "$root"
  LOCK_INHERITED=false
  # A holder's own child keeps the descriptor 9 that it inherited, which holds the lock: opening the
  # file again on descriptor 9 would replace it, and the child would hold nothing. flock on the
  # holder's own open file description succeeds; on any other description, while the holder runs, it
  # fails.
  if [ -n "${LOADTEST_LOCK_TOKEN:-}" ] && [ /dev/fd/9 -ef "$file" ] &&
    [ "$(lock_field token "$(cat -- "$file" 2>/dev/null)")" = "$LOADTEST_LOCK_TOKEN" ] && flock -n 9; then
    LOCK_INHERITED=true
    return 0
  fi
  exec 9>&-
  # Try the lock for up to 3 s, on descriptor 8: lock_holders lists the processes with descriptor 9
  # on the file, and a waiting caller is none of them. While it fails, read the holder's record,
  # which the holder writes just after it locks, and stop at a complete record whose pid runs in this
  # PID namespace. A complete record whose pid does not is a holder whose children still hold the
  # lock (stop when lock_holders names them), one that has just ended (the next try takes the lock),
  # or one in another PID namespace, which this one never sees.
  local record="" holders="" runs=false i
  for ((i = 0; i < 30; i++)); do
    ((i == 0)) || sleep 0.1
    exec 8<>"$file"
    if flock -n 8; then
      # The duplicate shares the locked open file description, so descriptor 9 keeps the lock.
      exec 9<&8 8<&-
      lock_take "$file"
      return 0
    fi
    exec 8<&-
    record=$(cat -- "$file" 2>/dev/null || true)
    if [[ $(lock_field token "$record") =~ ^[0-9a-f]{32}$ ]]; then
      if lock_record_runs "$record"; then
        runs=true
        break
      fi
      holders=$(lock_holders "$file")
      [ -n "$holders" ] && break
    fi
  done
  local pid ns run held
  pid=$(lock_field pid "$record")
  ns=$(lock_record_ns "$record")
  # A pid of another PID namespace is not written as "pid N": here, pid N is an unrelated process.
  if [ "$ns" = elsewhere ]; then
    run="pid=$pid in PID namespace $(lock_field pidns "$record")"
  else
    run="pid $pid"
  fi
  run+=", started $(lock_field started "$record"): $(lock_field command "$record")"
  $runs || [ -n "$holders" ] || holders=$(lock_holders "$file")
  # One message names the holder and the rule, so a step's not-run record (run-step.sh), which
  # keeps the message a refusal dies with, names the holder too.
  if $runs; then
    held="LOADTEST_ROOT $root is in use by another run ($run)"
  elif [ -n "$holders" ]; then
    local taker="left no record in the lock file"
    if [ -n "$pid" ]; then
      case $ns in
        here) taker="($run) has exited" ;;
        elsewhere) taker="($run) wrote its record in another PID namespace" ;;
        *) taker="($run) wrote no PID namespace in its record, so its pid is not trusted" ;;
      esac
    fi
    held="LOADTEST_ROOT $root is in use by another run: $holders holds its lock; the run that took it $taker"
  else
    # The lock is held, yet this PID namespace shows no holder: neither the record's pid nor a
    # process with descriptor 9 on the file.
    local named="the lock file holds no record of that run"
    if [ -n "$pid" ]; then
      case $ns in
        here) named="the lock file names that run as $run, which has exited" ;;
        elsewhere) named="the lock file names that run as $run, written in another PID namespace" ;;
        *) named="the lock file names that run as $run, in a record without its PID namespace" ;;
      esac
    fi
    held="LOADTEST_ROOT $root is locked by a process that is not visible in this PID namespace, such as a run in another PID namespace (another Legion role in this pod on a shared LOADTEST_ROOT); $named. Do not remove $file: that run still holds it, and a run that locked a new lock file would reset the stack under it"
  fi
  die "$held. One run at a time per root ($file): wait for that run to end, or use another LOADTEST_ROOT"
}

# build_migration_versions prints the version of each migration in directory $1, one per line: the
# leading digits of every [0-9]*.up.sql name, as the runners read them (internal/pgmigrate).
build_migration_versions() {
  local file
  for file in "$1"/[0-9]*.up.sql; do
    file=${file##*/}
    printf '%s\n' "$((10#${file%%_*}))"
  done
}

# seed_migration_check compares the migration versions $2 (one per line) of the build $3 describes
# with the set the seed's meta file $1 records; a non-empty $4 says $2 is only the build's highest.
# It dies when the seed holds a version this build lacks, since a newer build made that seed, and
# notes the versions this build holds and the seed lacks. It compares the sets, not the highest
# version of each: a migration whose version is below one the seed already holds, a numbered
# migration merged after a timestamped one, leaves the highest version the same on both sides.
seed_migration_check() {
  local meta=$1 build=$2 what=$3 only_highest=${4:-}
  local seed behind ahead seed_max build_max reseed
  reseed="reseed with this build (stack.sh seed --force)"
  if [[ -n "$only_highest" ]] || ! jq -e 'has("migrations")' "$meta" >/dev/null; then
    # A seed.meta.json written before the seed recorded its versions holds only the highest one, and
    # so does a build record written before setup recorded the build's, so only the highest can be
    # compared: neither can say which versions are below it.
    seed_max=$(jq -er .migration "$meta")
    build_max=$(printf '%s\n' "$build" | sort -n | tail -n1)
    if ((build_max < seed_max)); then
      die "this build's ($what) highest migration is $build_max, older than the seed's $seed_max (seed made by $(jq -r .build_commit "$meta")); $reseed or run a newer one"
    fi
    note "only the highest migrations are compared, the seed's $seed_max and this build's ($what) $build_max, since one of the two records only its highest; $reseed to compare the whole set"
    return 0
  fi
  seed=$(jq -r '.migrations[]' "$meta")
  behind=$(grep -Fxv -f <(printf '%s\n' "$build") <<<"$seed" || true)
  ahead=$(grep -Fxv -f <(printf '%s\n' "$seed") <<<"$build" || true)
  if [[ -n "$behind" ]]; then
    die "the seed holds $(wc -l <<<"$behind") migration(s) this build ($what) does not have, the highest $(sort -n <<<"$behind" | tail -n1), so a newer build made it (seed made by $(jq -r .build_commit "$meta")); $reseed or run a newer one"
  fi
  if [[ -n "$ahead" ]]; then
    note "this build ($what) holds $(wc -l <<<"$ahead") migration(s) the seed lacks, the highest $(sort -n <<<"$ahead" | tail -n1). Dispatch migrates each step's copy forward at start, and any backfill those migrations start runs inside the step; $reseed to measure it alone"
  fi
}

prepare_stack() {
  local stack
  stack=$(dirname -- "${BASH_SOURCE[0]}")/stack.sh
  "$stack" setup
  "$stack" start
  "$stack" seed
  # A build cannot start on a seed that a newer build migrated: compare the migrations of the
  # Dispatch that setup built (bin/envoy-dispatch.json, which names LOADTEST_DISPATCH_COMMIT's tree
  # when that is set, and the Dispatch checkout's otherwise) with the set the seed records. Without
  # that record, the Dispatch checkout's migration files stand for it.
  # A record written before setup recorded the build's versions holds only its highest one.
  local build_json=$ROOT/bin/envoy-dispatch.json build_versions build_what only_highest=""
  if [ -f "$build_json" ]; then
    build_what="Dispatch $(jq -r .dispatch_commit "$build_json")"
    if jq -e 'has("migrations")' "$build_json" >/dev/null; then
      build_versions=$(jq -r '.migrations[]' "$build_json")
    else
      build_versions=$(jq -er .newest_migration "$build_json")
      only_highest=1
    fi
  else
    build_versions=$(build_migration_versions "$DISPATCH_SRC/packages/envoy/internal/dispatch/store/migrations")
    build_what="the Dispatch checkout's migrations"
  fi
  seed_migration_check "$ROOT/seed.meta.json" "$build_versions" "$build_what" "$only_highest"
  BUILD=$("$stack" build-info)
}

# ---- the quiet box -------------------------------------------------------------------------------

# QUIET_READ_EVERY is the gate's pause between two readings, in seconds (wait_for_quiet_box): the
# last pause is cut short so that the pauses add up to LOADTEST_QUIET_WAIT. Each reading takes its
# own 5 s besides.
QUIET_READ_EVERY=30

# QUIET_READINGS is every reading wait_for_quiet_box took, in order, and QUIET_BAR and QUIET_WAIT
# the bar and the wait it held them to; empty until it runs. They are globals so that run-step.sh's
# exit trap reads them after the gate's own die.
QUIET_READINGS=()
QUIET_BAR=""
QUIET_WAIT=""

# box_busy_reading: one reading of the box's busy fraction, four decimals: `stack.sh loadgen
# box-busy`, the same 5 s /proc/stat measure as meta.json's box_busy_before (loadgen's boxBusy over
# boxBusyBeforeWindow). A test replaces it.
box_busy_reading() { "$(dirname -- "${BASH_SOURCE[0]}")/stack.sh" loadgen box-busy; }

# quiet_pause SECONDS: the gate's pause before its next reading. A test replaces it.
quiet_pause() { sleep "$1"; }

# quiet_fraction VALUE: VALUE is a fraction between 0 and 1 written as a JSON number (0, 0.10, 1,
# 1.0000), as loadgen box-busy prints one and as loadgen run and the not-run record take it.
quiet_fraction() { [[ $1 =~ ^(0(\.[0-9]+)?|1(\.0+)?)$ ]]; }

# quiet_readings [SEPARATOR]: QUIET_READINGS joined by SEPARATOR, ", " when none is given.
quiet_readings() {
  local separator=${1:-, } text="" reading
  for reading in "${QUIET_READINGS[@]}"; do
    text+=$reading$separator
  done
  printf '%s\n' "${text%"$separator"}"
}

# wait_for_quiet_box: return once the box is quiet enough for a step to start, and die when it does
# not get there in time (README.md, "A quiet box before each step"). Every reading a step takes
# holds the box's other work, so a step on a busy box measures the box and not Dispatch. A reading
# (box_busy_reading) at or under LOADTEST_MAX_BUSY_BEFORE (a fraction, default 0.10) returns at
# once. One above it is noted, and the gate pauses QUIET_READ_EVERY seconds (quiet_pause) and reads
# again, until its pauses add up to LOADTEST_QUIET_WAIT (whole seconds, default 900: 15 minutes and
# at most 31 readings; 0 reads once); then it dies, naming every reading, the bar and the wait. A
# bar or a wait that is not one is refused before the first reading.
wait_for_quiet_box() {
  local bar=${LOADTEST_MAX_BUSY_BEFORE:-0.10} wait_s=${LOADTEST_QUIET_WAIT:-900} reading waited=0 pause
  local n what=readings
  quiet_fraction "$bar" ||
    die "LOADTEST_MAX_BUSY_BEFORE='$bar' is not a fraction between 0 and 1 written like 0.10: it is the busiest the box may read (loadgen box-busy) for a step to start"
  [[ $wait_s =~ ^[0-9]+$ ]] ||
    die "LOADTEST_QUIET_WAIT='$wait_s' is not a whole number of seconds like 900: it is how long a step's pauses may add up to while it waits for a busy box to go quiet, each reading taking 5 s besides"
  QUIET_READINGS=()
  QUIET_BAR=$bar
  QUIET_WAIT=$((10#$wait_s))
  while :; do
    reading=$(box_busy_reading) ||
      die "the quiet-box gate could not read how busy the box is: loadgen box-busy failed (its message above says why)"
    quiet_fraction "$reading" ||
      die "the quiet-box gate read '$reading' from loadgen box-busy, which is not a fraction between 0 and 1"
    QUIET_READINGS+=("$reading")
    if awk -v r="$reading" -v b="$bar" 'BEGIN { exit !(r + 0 <= b + 0) }'; then
      if ((waited == 0)); then
        note "the box is $reading busy (loadgen box-busy), at or under LOADTEST_MAX_BUSY_BEFORE=$bar: the step starts"
      else
        note "the box is $reading busy (loadgen box-busy), at or under LOADTEST_MAX_BUSY_BEFORE=$bar, after $waited s of waiting: the step starts, its readings $(quiet_readings)"
      fi
      return 0
    fi
    ((waited < QUIET_WAIT)) || break
    pause=$((QUIET_WAIT - waited < QUIET_READ_EVERY ? QUIET_WAIT - waited : QUIET_READ_EVERY))
    note "the box is $reading busy (loadgen box-busy), above LOADTEST_MAX_BUSY_BEFORE=$bar: the step waits for it to go quiet, reading again in $pause s ($waited of LOADTEST_QUIET_WAIT=$QUIET_WAIT s waited)"
    quiet_pause "$pause"
    waited=$((waited + pause))
  done
  n=${#QUIET_READINGS[@]}
  ((n > 1)) || what=reading
  die "the box stayed busier than LOADTEST_MAX_BUSY_BEFORE=$bar through LOADTEST_QUIET_WAIT=$QUIET_WAIT s, so the step is not run: $n $what of loadgen box-busy, $(quiet_readings). The box's other work (on a shared node, its other pods) would be in every reading the step takes, making them the box's and not Dispatch's; run the step again once the box is quiet, or on another node"
}

# ---- a step's window -----------------------------------------------------------------------------

# not_run_record DIR: the path of the not-run record of the step whose directory is DIR (README.md,
# "Run one step"): beside that directory and never in it, so the record never makes a later run of
# the step refuse a directory that already holds a step.
not_run_record() { printf '%s.not-run.json\n' "${1%/}"; }

# window_start DIR [AFTER]: the window start that DIR/meta.json names, as it names it (RFC 3339),
# once `loadgen run` has written one; until then meta.json is absent or holds Go's zero time (year
# 0001). With AFTER, a Unix time, a start before that second is not taken. Status 1 when there is
# no start to print.
window_start() {
  local start at
  start=$(jq -er '.window_start | select(type == "string" and (startswith("0001") | not))' "$1/meta.json" 2>/dev/null) ||
    return 1
  if [ -n "${2:-}" ]; then
    at=$(date -d "$start" +%s 2>/dev/null) || return 1
    ((at >= $2)) || return 1
  fi
  printf '%s\n' "$start"
}

# pid_started_at PID: the Unix second at which process PID started, /proc/stat's boot time plus the
# start time in clock ticks that /proc/PID/stat holds. Both are rounded down, so it is never later
# than the true start. Status 1 when /proc does not show PID.
pid_started_at() {
  local stat btime ticks
  local -a fields
  [[ $1 =~ ^[0-9]+$ ]] || return 1
  { read -r stat <"/proc/$1/stat"; } 2>/dev/null || return 1
  # The fields after the command's closing parenthesis start at the third, the state; the start
  # time is the 22nd.
  read -ra fields <<<"${stat##*) }"
  btime=$(awk '$1 == "btime" { print $2 }' /proc/stat 2>/dev/null) || return 1
  ticks=$(getconf CLK_TCK 2>/dev/null) || return 1
  [[ ${fields[19]:-} =~ ^[0-9]+$ && $btime =~ ^[0-9]+$ && $ticks =~ ^[1-9][0-9]*$ ]] || return 1
  printf '%s\n' "$((btime + fields[19] / ticks))"
}

# wait_for_window STEP PID: wait for the window of the step that process PID runs (run-step.sh, or a
# script that runs it), STEP being the step's name, for $ROOT/runs/<name> (resolve_paths sets
# ROOT), or its directory, a path with a /, for a step run with --out. It checks PID every 0.1 s and
# reads the step's meta.json every 0.5 s, since a jq run costs the box more than a read of /proc:
#
# - once meta.json names a window that started at or after PID did, it prints that start, as
#   meta.json writes it, and returns 0. A window that started before PID did is an earlier run's,
#   left in a step directory that this run refuses, and is not taken;
# - once PID has ended (exited, or a zombie) first, it returns 1 at once, so within a second of the
#   end, saying why on stderr: the exit status and reason of the step's not-run record
#   (not_run_record), or, when there is none, that PID ended before the window without one (a step
#   killed by SIGKILL, or one that run-step.sh did not run). A step that opened its window just
#   before it ended has its start printed.
wait_for_window() {
  local dir=$1 pid=$2 started start record why tick=0
  case $dir in
    */*) dir=${dir%/} ;;
    *) dir=$ROOT/runs/$dir ;;
  esac
  # Read while PID runs: /proc forgets a process that has ended.
  started=$(pid_started_at "$pid") || started=""
  while lock_pid_alive "$pid"; do
    if ((tick++ % 5 == 0)) && start=$(window_start "$dir" "$started"); then
      printf '%s\n' "$start"
      return 0
    fi
    sleep 0.1
  done
  # The record first: run-step.sh removes the record as it starts and writes it before it exits,
  # so a record here is this run's, whatever an earlier run left in meta.json.
  record=$(not_run_record "$dir")
  if [ -f "$record" ]; then
    why=$(jq -r '"run-step.sh exited \(.exit_status // "on a signal") at \(.at): \(.reason)"' "$record" 2>/dev/null) ||
      why="its record cannot be read"
    note "step $dir did not open its window: $why (see $record)"
    return 1
  fi
  if start=$(window_start "$dir" "$started"); then
    printf '%s\n' "$start"
    return 0
  fi
  note "step $dir did not open its window: pid $pid ended before it and left no not-run record ($record), so it was killed (SIGKILL) or not run by run-step.sh"
  return 1
}

# random_hex N: N random bytes in hex.
random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

# SEED_COUNTS_SQL: the seed's counts that seed.meta.json records, one JSON object, the same for
# stack.sh seed and cluster.sh seed, so the record tells a seed with the reference documents
# (loadgen seed-refs) from one without them. Every table names public, as every statement of this
# package does: the default search_path is "$user", public, so an unqualified name counts a schema
# of the login's name first wherever one exists (dispatch://INFRA-988).
SEED_COUNTS_SQL="select json_build_object(
  'projects', (select count(*) from public.projects),
  'issues', (select count(*) from public.issues),
  'comments', (select count(*) from public.comments),
  'artifacts', (select count(*) from public.artifacts),
  'artifact_versions', (select count(*) from public.artifact_versions),
  'ref_targets', (select count(*) from public.artifacts where name like 'ref-target-%'),
  'ref_target_versions', (select count(*) from public.artifact_versions v join public.artifacts a on a.id = v.artifact_id where a.name like 'ref-target-%'),
  'ref_sources', (select count(*) from public.artifacts where name = 'references.md'),
  'artifact_refs', (select count(*) from public.refs where to_kind = 'artifact'),
  'events', (select count(*) from public.events))"
export SEED_COUNTS_SQL

# ---- the replay's inputs ------------------------------------------------------------------------------

# replay_download OUT ISSUE SLUG VERSION: one version of a Dispatch file artifact into OUT, with
# the caller's DISPATCH_URL and the bearer token in DISPATCH_TOKEN_FILE. The scripts run in the
# caller's environment (stack.sh starts only its stack processes under env -i), and the token
# reaches curl on its standard input, never on a command line or in the script's output.
replay_download() {
  [ -n "${DISPATCH_URL:-}" ] || die "DISPATCH_URL is not set: set it and DISPATCH_TOKEN_FILE to download the replay's parts, or pass --from-dir"
  [ -r "${DISPATCH_TOKEN_FILE:-}" ] || die "DISPATCH_TOKEN_FILE does not name a readable file: set it to download the replay's parts, or pass --from-dir"
  local token
  token=$(<"$DISPATCH_TOKEN_FILE")
  token=${token//[$'\r\n\t ']/}
  note "downloading dispatch://$2/artifact/$3 version $4"
  printf 'Authorization: Bearer %s\n' "$token" |
    curl -q -fsSL --retry 3 --max-time 900 -H @- -o "$1" "${DISPATCH_URL%/}/api/v1/issues/$2/artifacts/$3/versions/$4"
}

# replay_fetch_part ENTRY ISSUE FROM_DIR PARTS_DIR: put a manifest part (its JSON) at
# PARTS_DIR/<file>: kept when its sha256 matches, else copied from FROM_DIR (when not empty) or
# downloaded, and refused, deleted, when its sha256 differs from the manifest's.
replay_fetch_part() {
  local entry=$1 issue=$2 from_dir=$3 parts_dir=$4 file slug version want
  file=$(jq -er .file <<<"$entry")
  slug=$(jq -er .slug <<<"$entry")
  version=$(jq -er .version <<<"$entry")
  want=$(jq -er .sha256 <<<"$entry")
  [[ $file =~ ^[A-Za-z0-9._-]+$ ]] || die "the manifest's part file '$file' is not a plain file name"
  local dst=$parts_dir/$file
  if [ -f "$dst" ] && [ "$(sha256_of "$dst")" = "$want" ]; then
    note "part $file: kept (sha256 matches)"
    return 0
  fi
  local tmp=$dst.tmp
  rm -f -- "$tmp"
  if [ -n "$from_dir" ]; then
    [ -f "$from_dir/$file" ] || die "part $file: not in --from-dir $from_dir"
    cp -- "$from_dir/$file" "$tmp"
  elif ! replay_download "$tmp" "$issue" "$slug" "$version"; then
    rm -f -- "$tmp"
    die "part $file: the download of dispatch://$issue/artifact/$slug version $version failed"
  fi
  local got
  got=$(sha256_of "$tmp")
  if [ "$got" != "$want" ]; then
    rm -f -- "$tmp" "$dst"
    die "part $file: sha256 $got, the manifest says $want; deleted it"
  fi
  mv -f -- "$tmp" "$dst"
  note "part $file: $(stat -c %s -- "$dst") bytes, sha256 matches"
}

# replay_fetch_parts MANIFEST WHICH PARTS_DIR FROM_DIR: every part of the manifest's WHICH (bundle
# or seed) at PARTS_DIR/<file> (replay_fetch_part), then the joined parts' sha256 against WHICH's
# tar_zst_sha256 (bundle) or dump_sha256 (seed). The parts' paths, in order, go into REPLAY_PARTS.
# The parts are file artifacts of the issue LOADTEST_REPLAY_ISSUE names, which is required.
replay_fetch_parts() {
  local manifest=$1 which=$2 parts_dir=$3 from_dir=$4 key issue entry want got
  case $which in
    bundle) key=tar_zst_sha256 ;;
    seed) key=dump_sha256 ;;
    *) die "replay_fetch_parts: no part list $which" ;;
  esac
  # The manifest names no issue: the operator names it, and nothing is read or downloaded without
  # it (stack.sh replay-get, cluster.sh replay-prepare).
  [ -n "${LOADTEST_REPLAY_ISSUE:-}" ] ||
    die "LOADTEST_REPLAY_ISSUE is not set: set it to the issue the replay's parts are file artifacts of (the manifest names none)"
  issue=$LOADTEST_REPLAY_ISSUE
  mkdir -p -- "$parts_dir"
  REPLAY_PARTS=()
  while IFS= read -r entry; do
    replay_fetch_part "$entry" "$issue" "$from_dir" "$parts_dir"
    REPLAY_PARTS+=("$parts_dir/$(jq -r .file <<<"$entry")")
  done < <(jq -c --arg w "$which" '.[$w].parts[]' "$manifest")
  ((${#REPLAY_PARTS[@]} > 0)) || die "$manifest lists no $which parts"
  want=$(jq -er --arg w "$which" --arg k "$key" '.[$w][$k]' "$manifest")
  got=$(joined_sha256 "${REPLAY_PARTS[@]}")
  [ "$got" = "$want" ] || die "the joined $which parts' sha256 is $got, the manifest's $key $want"
}

# replay_get_bundle MANIFEST DIR FROM_DIR LOADGEN: the bundle's parts under DIR/parts
# (replay_fetch_parts), unpacked with `LOADGEN replay-unpack` into DIR/bundle (not again while
# DIR/bundle.unpacked.json names the same parts and the bundle's bundle.meta.json), then checked
# file by file with `LOADGEN replay-verify-bundle`, whose line goes into REPLAY_BUNDLE_VERIFIED.
# LOADGEN is the caller's command (a function) that runs its loadgen.
replay_get_bundle() {
  local manifest=$1 dir=$2 from_dir=$3 loadgen_cmd=$4 want
  replay_fetch_parts "$manifest" bundle "$dir/parts" "$from_dir"
  want=$(jq -er .bundle.tar_zst_sha256 "$manifest")
  local bundle=$dir/bundle marker=$dir/bundle.unpacked.json meta=bundle.meta.json
  if [ -f "$marker" ] && [ "$(jq -r .tar_zst_sha256 "$marker")" = "$want" ] && [ -f "$bundle/$meta" ] &&
    [ "$(jq -r .bundle_meta_sha256 "$marker")" = "$(sha256_of "$bundle/$meta")" ]; then
    note "bundle at $bundle is already unpacked from these parts; kept"
  else
    rm -f -- "$marker"
    rm -rf -- "$bundle.tmp"
    note "unpacking the bundle into $bundle"
    "$loadgen_cmd" replay-unpack -out "$bundle.tmp" "${REPLAY_PARTS[@]}" || die "loadgen replay-unpack of the bundle's parts failed"
    rm -rf -- "$bundle"
    mv -- "$bundle.tmp" "$bundle"
    jq -n --arg tar "$want" --arg meta "$(sha256_of "$bundle/$meta")" \
      '{tar_zst_sha256: $tar, bundle_meta_sha256: $meta}' >"$marker"
  fi
  REPLAY_BUNDLE_VERIFIED=$("$loadgen_cmd" replay-verify-bundle -dir "$bundle") || die "the bundle at $bundle does not verify (above)"
  note "bundle verified: $REPLAY_BUNDLE_VERIFIED"
}
