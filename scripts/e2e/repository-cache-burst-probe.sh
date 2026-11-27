#!/usr/bin/env bash
# The probe of scripts/e2e/repository-cache-burst-job.sh, run inside the worker image by one
# Kubernetes Job of three containers that share one volume at /scratch. It is verification check o2
# of INFRA-5789: with the repository cache's bucket on, 20 `workspace-init fetch` runs at once leave
# the cache's CPU below 0.2 CPU, each logging the bucket as its source; with it off, the same 20 read
# /tar and drive the cache's CPU above 0.5 CPU. Cloud Storage is a double (package fakegcs of
# packages/daemon/internal/repocache, compiled in the Job from the tested checkout), because no
# identity a Job may use can sign an address (iam.serviceAccounts.signBlob): what the burst proves
# is the cache's CPU and every fetch's source, and a real bucket's signed addresses are the
# production check's to prove.
#
# One text, three roles, chosen by the first argument; the container's name says which:
#
#   double    compiles the double and serves it on loopback. Its CPU is not measured.
#   cache     the conductor. It runs the image's real `legion workspace-init cache` as a process of
#             its own and reads only THIS container's cumulative CPU counter (cgroup v2 cpu.stat
#             usage_usec, or v1 cpuacct.usage), found from /proc/self/cgroup and
#             /proc/self/mountinfo, so the double's and the fetches' CPU is not in it. This
#             container's request and limit are the production cache's default, 1 CPU and 4Gi.
#   fetchers  runs the image's real `legion workspace-init fetch`, 20 at once, each on a feed of its
#             own, released together by a file on the shared volume.
#
# The sequence, all of it in the cache container's log, which the harness reads:
#
#   1. The image, `legion version`, the tested revision, the volume and the cgroup files the counter
#      is found from (sanitized: no pod uid), and the counter read twice.
#   2. The image's real `legion config check` on a fixture legion.yaml with both
#      runtime.kubernetes.repository_cache keys (it must pass), on one with neither (it must pass),
#      and on one with the bucket alone (it must refuse, naming the missing key).
#   3. Attribution. A 3-second CPU loop in the fetchers container must move this container's
#      counter by under 0.5 CPU-seconds, and the same loop here by 2.4 to 3.6. The measuring loop's
#      own cost, idle for 20 seconds, must be under 0.02 CPU, a tenth of the enabled ceiling. A
#      preflight Job measured 0.090 and 2.970 for the first two with this shape. A failed
#      attribution is VOID: nothing is proved.
#   4. The enabled arm: the cache starts with STORAGE_EMULATOR_HOST at the double and
#      LEGION_REPOSITORY_CACHE_BUCKET at the fixture bucket (first clone from GitHub, then its
#      first upload of every pack and index to the double). The probe waits for /healthz ready and
#      for GET /packs to answer 200 with a manifest that names every pack and index of the clone
#      with its size (printed: the count, the bytes and the names, never an address), takes the
#      counter, releases the 20 fetches, takes it every 10 seconds and when the last fetch exits,
#      and stops the cache. --refresh is 24h, so no refresh or repack lands in a timed interval.
#   5. The 20 feeds are deleted. The disabled arm starts the cache on the SAME clone root with
#      neither variable (its start-up fetch from GitHub is before the release) and does the same.
#      Each arm prints the clone's refs count and pack sizes once.
#   6. The verdict, one line, computed from the samples and the facts alone (compute_verdict):
#      each interval's CPU is its counter's change over its wall seconds, the final partial
#      interval included. PASS needs, in the enabled arm, 20 fetches that exit 0, each with
#      `source=bucket`, the object lines of the manifest it was given (name, size, SHA-256), no
#      `source=/tar`, at least one sample whose /healthz read answered and no /tar stream in
#      flight at any sample, and every interval below 0.2 CPU; and, in the disabled arm, 20
#      fetches that exit 0, each with `source=/tar reason=/packs is not served`, /packs answering
#      404, and at least one FULL interval (9.5 s or more) above 0.5 CPU.
#      A disabled arm with no such interval is VOID, not a pass: the run is too short to tell the
#      arms apart. A fetch's output is searched for X-Goog-Signature, Bearer, Authorization and both
#      tokens before anything of it is printed; one hit fails the run and prints nothing of that
#      file. The last line is `JOB-RESULT pass`, `JOB-RESULT fail` or `JOB-RESULT void`, and the
#      probe exits 0 on each: the harness reads the line, and a non-zero exit would have the Job
#      start the whole run again. A leak fails the run even where it stopped before any arm ended.
#
# `self-test` runs, with no Job, on any Linux host with bash 4 or later, GNU coreutils, jq and awk,
# the checks that need no cluster: the cgroup parser against the files under
# testdata/repository-cache-burst (cgroup-v1.* is the shape of a gVisor pod as measured, with the
# container id made up; cgroup-v2.* is the unified layout of a containerd pod, written from the
# documented format, not captured), the counter reads, the manifest check, and the verdict against
# synthetic series: one passing, an enabled interval at 0.25, one at exactly 0.2, a disabled arm
# that never crosses 0.5 in a full interval (void), one that crosses it only in a partial interval
# (void), a wrong source, an enabled arm whose /healthz was never read (fail) and one read once
# (pass), a leak, a run stopped void before any arm ended with a leak (fail) and without one
# (void), and a failed attribution.
#
# Environment (set by the harness): BURST_REPO (owner/name), BURST_IMAGE, BURST_IMAGE_REVISION,
# BURST_REVISION (the tested checkout's commit), BURST_DEADLINE (the Job's deadline, seconds).
set -u

export TZ=UTC
export LC_ALL=C

scratch=/scratch
run=$scratch/run
burst=$scratch/burst
double_dir=$scratch/double
cache_dir=$scratch/cache
fetch_dir=$scratch/fetchers
fixtures=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/testdata/repository-cache-burst

repo=${BURST_REPO:-nodejs/node}
double_listen=127.0.0.1:8480
cache_listen=127.0.0.1:8418
fixture_bucket=example-legion-packs
fixture_account=legion-repository-cache
emulator_email=legion-repository-cache@emulator.invalid

fetch_count=20
interval_us=10000000          # the sampling period
full_interval_us=9500000      # an interval at least this wide is a full 10-second one
enabled_ceiling=0.2           # every enabled interval is below this many CPU
control_floor=0.5             # a disabled full interval is above this many CPU
attrib_loop_s=3
attrib_other_max=0.5          # CPU-seconds the other container's loop may move this container's counter
attrib_own_min=2.4
attrib_own_max=3.6
sampler_idle_s=20
sampler_idle_max=0.02         # CPU the measuring loop may cost (CPU-seconds a second), a tenth of enabled_ceiling
clone_wait_s=7200             # the first clone of a large repository (workspace.CacheFetchTimeout)
packs_wait_s=3600             # the first upload of every pack and index to the double
arm_budget_s=5400             # one arm's 20 fetches
double_wait_s=900             # the double's build
deadline_s=${BURST_DEADLINE:-21600}

# --------------------------------------------------------------------------------------------
# Helpers every role uses.
# --------------------------------------------------------------------------------------------
say() { printf '[%(%FT%TZ)T] %s\n' -1 "$*"; }

# nap <seconds>: sleep with no fork, so the conductor's waiting costs the cache's counter nothing
# worth reading. It reads a pipe nothing writes to until the timeout.
nap_fd=""
open_nap() {
  local fifo
  fifo=$(mktemp -u "${TMPDIR:-/tmp}/burst-nap.XXXXXX") || return 1
  mkfifo "$fifo" && exec {nap_fd}<> "$fifo" && rm -f "$fifo"
}
nap() { read -r -t "$1" -u "$nap_fd" _ 2> /dev/null || true; }

# wait_file <path> <seconds> [<poll seconds>]: 0 once the file exists, 1 at the limit, 2 when the
# run was aborted or has finished.
wait_file() {
  local path=$1 limit_us=$(($2 * 1000000)) poll=${3:-1} started=${EPOCHREALTIME/./}
  while [ ! -e "$path" ]; do
    if [ -e "$run/abort" ] || [ -e "$run/finished" ]; then return 2; fi
    [ $((${EPOCHREALTIME/./} - started)) -lt "$limit_us" ] || return 1
    nap "$poll"
  done
}

# group <n>: n with its digits grouped in threes.
group() {
  local n=$1 grouped=""
  [[ $n =~ ^[0-9]+$ ]] || { printf '%s' "$n"; return; }
  while [ "${#n}" -gt 3 ]; do
    grouped=",${n: -3}$grouped"
    n=${n:0:${#n}-3}
  done
  printf '%s%s' "$n" "$grouped"
}

# show_log <file> [<lines>]: the last lines of a log, each after a "| ", unless the file holds a
# header, a credential or a token (leak_scan), which it names and prints nothing of.
show_log() {
  [ -s "$1" ] || { echo "| (empty: $1)"; return; }
  if ! leak_scan "$1" > /dev/null; then
    echo "| (withheld: $(leak_scan "$1"))"
    return
  fi
  tail -n "${2:-20}" "$1" | sed 's/^/| /'
}

# leak_scan <file>…: 0 when no file holds X-Goog-Signature (or any X-Goog- parameter), Bearer,
# Authorization, or either token; else 1, saying which pattern in which file, never the line.
leak_scan() {
  local file hit=0 token
  for file in "$@"; do
    [ -e "$file" ] || continue
    if grep -qiE 'x-goog-|bearer|authorization' "$file"; then
      echo "LEAK: $(basename -- "$file") holds a signed-address parameter, a bearer or an authorization header"
      hit=1
    fi
    for token in "$run"/secrets/provision-token "$run"/secrets/cache-token; do
      [ -s "$token" ] || continue
      if grep -qF -- "$(<"$token")" "$file"; then
        echo "LEAK: $(basename -- "$file") holds the value of $(basename -- "$token")"
        hit=1
      fi
    done
  done
  [ "$hit" -eq 0 ]
}

# --------------------------------------------------------------------------------------------
# The CPU counter: which file of which cgroup holds THIS container's cumulative CPU, and its read.
# --------------------------------------------------------------------------------------------
counter_kind="" counter_file="" counter_why=""

# cgroup_relative <mount root> <cgroup path>: sets cgroup_rel to the path below the mount's root,
# with no trailing slash, and answers 1 where the cgroup is outside the mount.
cgroup_rel=""
cgroup_relative() {
  local root=$1 path=$2
  cgroup_rel=""
  if [ "$root" = / ]; then
    cgroup_rel=$path
  elif [ "$path" = "$root" ]; then
    cgroup_rel=""
  elif [ "${path#"$root"/}" != "$path" ]; then
    cgroup_rel=${path#"$root"}
  else
    return 1
  fi
  cgroup_rel=${cgroup_rel%/}
}

# find_counter <cgroup file> <mountinfo file>: sets counter_kind (v1 or v2) and counter_file, from
# the cgroup path of /proc/self/cgroup and the mount of /proc/self/mountinfo that holds it: a v1
# cpuacct mount's cpuacct.usage (nanoseconds), else a cgroup2 mount's cpu.stat (usage_usec).
find_counter() {
  local cgroup_file=$1 mountinfo_file=$2 id controllers path line cg1="" cg2="" v1="" v2="" i dash
  local -a f
  counter_kind="" counter_file="" counter_why=""
  [ -r "$cgroup_file" ] || { counter_why="$cgroup_file cannot be read"; return 1; }
  [ -r "$mountinfo_file" ] || { counter_why="$mountinfo_file cannot be read"; return 1; }
  while IFS=: read -r id controllers path; do
    : "$id"
    if [ -z "$controllers" ]; then
      cg2=$path
    else
      case ",$controllers," in *,cpuacct,*) cg1=$path ;; esac
    fi
  done < "$cgroup_file"
  while IFS= read -r line; do
    read -r -a f <<< "$line"
    dash=-1
    for i in "${!f[@]}"; do
      if [ "${f[$i]}" = "-" ]; then dash=$i; break; fi
    done
    [ "$dash" -ge 6 ] || continue
    case ${f[$((dash + 1))]} in
      cgroup)
        if [ -n "$v1" ] || [ -z "$cg1" ]; then continue; fi
        case ",${f[$((dash + 3))]:-}," in *,cpuacct,*) ;; *) continue ;; esac
        cgroup_relative "${f[3]}" "$cg1" || continue
        v1=${f[4]}$cgroup_rel/cpuacct.usage
        ;;
      cgroup2)
        if [ -n "$v2" ] || [ -z "$cg2" ]; then continue; fi
        cgroup_relative "${f[3]}" "$cg2" || continue
        v2=${f[4]}$cgroup_rel/cpu.stat
        ;;
    esac
  done < "$mountinfo_file"
  if [ -n "$v1" ]; then
    counter_kind=v1 counter_file=$v1
  elif [ -n "$v2" ]; then
    counter_kind=v2 counter_file=$v2
  else
    counter_why="no cpuacct or cgroup2 mount of $mountinfo_file holds the cgroup path of $cgroup_file"
    return 1
  fi
}

# read_counter: sets counter_us to the container's cumulative CPU in microseconds, with no fork.
counter_us=""
read_counter() {
  local k v=""
  counter_us=""
  case $counter_kind in
    v1)
      read -r v < "$counter_file" 2> /dev/null || return 1
      [[ $v =~ ^[0-9]+$ ]] || return 1
      counter_us=$((v / 1000))
      ;;
    v2)
      while read -r k v; do
        if [ "$k" = usage_usec ]; then counter_us=$v; break; fi
      done < "$counter_file" 2> /dev/null
      [[ $counter_us =~ ^[0-9]+$ ]] || { counter_us=""; return 1; }
      ;;
    *) return 1 ;;
  esac
}

# print_cgroup_files <cgroup file> <mountinfo file>: both, once, as a reader of the log sees them:
# the cgroup file whole and the mountinfo lines of the cgroup mounts, with no pod uid.
print_cgroup_files() {
  local uid='[0-9a-fA-F]{8}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{12}'
  echo "cat /proc/self/cgroup:"
  sed -E "s/pod$uid/pod<uid>/g; s/$uid/<uid>/g; s/^/  /" "$1"
  echo "cat /proc/self/mountinfo (the cgroup lines):"
  awk '/ - cgroup2? / || $5 == "/sys/fs/cgroup"' "$2" | sed -E "s/pod$uid/pod<uid>/g; s/$uid/<uid>/g; s/^/  /"
}

# --------------------------------------------------------------------------------------------
# Samples, intervals and the verdict.
# --------------------------------------------------------------------------------------------
samples=""
sample_us=""

# take_sample <arm> <kind>: one line of the samples file, `<arm> <kind> <clock µs> <counter µs>`,
# and the same on the log. No fork: the sampler's own cost is in the counter it reads.
take_sample() {
  local t=${EPOCHREALTIME/./}
  read_counter || { say "the CPU counter could not be read"; return 1; }
  printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$t" "$counter_us" >> "$samples"
  sample_us=$t
  printf 'SAMPLE %s %s clock_us=%s cpu_us=%s\n' "$1" "$2" "$t" "$counter_us"
}

# intervals <samples file>: for each arm, one line per pair of consecutive samples:
# `<arm> <n> <wall s> <cpu s> <CPU> <full 0|1>`, CPU being the counter's change over the wall time;
# a pair whose wall time is not positive or whose counter fell has CPU -1.
intervals() {
  awk -F'\t' -v full="$full_interval_us" '
    $1 != arm { arm = $1; n = 0; have = 0 }
    {
      if (have) {
        n++
        wall = ($3 - pt) / 1e6
        cpu = ($4 - pc) / 1e6
        rate = (wall > 0 && cpu >= 0) ? cpu / wall : -1
        isfull = (($3 - pt) >= full) ? 1 : 0
        printf "%s\t%d\t%.3f\t%.3f\t%.4f\t%d\n", arm, n, wall, cpu, rate, isfull
      }
      pt = $3; pc = $4; have = 1
    }' "$1"
}

# arm_figures <intervals file> <arm>: `<count> <max CPU> <full count> <max full CPU> <invalid>`
# of one arm; max CPU is -1 with none.
arm_figures() {
  awk -F'\t' -v arm="$2" '
    BEGIN { n = 0; mx = -1; fn = 0; fmx = -1; bad = 0 }
    $1 == arm {
      n++
      if ($5 < 0) bad++
      if ($5 > mx) mx = $5
      if ($6 == 1) { fn++; if ($5 > fmx) fmx = $5 }
    }
    END { printf "%d %s %d %s %d\n", n, mx, fn, fmx, bad }' "$1"
}

declare -a failed_checks=() void_checks=()
# check <name> <pass|fail|void> <got> <want> [why]: one CHECK line, kept for the verdict.
check() {
  local name=$1 word=$2 got=${3//$'\n'/ } want=${4//$'\n'/ } why=${5:-}
  why=${why//$'\n'/ }
  case $word in
    pass) printf 'CHECK %s: got [%s] want [%s] PASS\n' "$name" "$got" "$want" ;;
    fail)
      printf 'CHECK %s: got [%s] want [%s] FAIL%s\n' "$name" "$got" "$want" "${why:+ ($why)}"
      failed_checks+=("$name")
      ;;
    void)
      printf 'CHECK %s: got [%s] want [%s] VOID%s\n' "$name" "$got" "$want" "${why:+ ($why)}"
      void_checks+=("$name")
      ;;
  esac
}

# below <a> <b>: a < b as numbers.
below() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 < b + 0) }'; }
above() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 > b + 0) }'; }

# compute_verdict <samples file> <facts file> [<arm>…]: prints every interval and every CHECK of the
# arms named (both by default), then sets verdict_word to fail (a property did not hold), void
# (nothing is proved: the control did not discriminate or attribution failed) or pass; fail
# outranks void. The facts file is `key=value` lines: attribution (pass|fail), leak (0|1), and for
# each arm `<arm>.fetches`, `.ok` (exit 0), `.timeout`, `.streams_reads` (the samples whose /healthz
# read answered), and for enabled `.bucket`, `.tar`, `.objects_match`, `.streams_max`,
# `.packs_complete`, for disabled `.unserved`, `.packs_status`.
verdict_word=""
compute_verdict() {
  local -A fact=()
  local k v table n max full_n full_max bad
  local -a judged=("${@:3}")
  [ "${#judged[@]}" -gt 0 ] || judged=(enabled disabled)
  while IFS='=' read -r k v; do
    [ -z "$k" ] || fact[$k]=$v
  done < "$2"
  failed_checks=() void_checks=()
  table=$(mktemp "${TMPDIR:-/tmp}/burst-intervals.XXXXXX") || return 1
  intervals "$1" > "$table"
  awk -F'\t' '{ printf "INTERVAL %s #%s wall=%ss cpu=%ss CPU=%s%s\n", $1, $2, $3, $4, $5, ($6 == 1 ? " full" : " partial") }' "$table"

  check "attribution" "$([ "${fact[attribution]:-}" = pass ] && echo pass || echo void)" "${fact[attribution]:-unchecked}" \
    "a loop in another container leaves this container's counter alone, and one here moves it"
  check "no credential, signed address or token in any output" "$([ "${fact[leak]:-1}" = 0 ] && echo pass || echo fail)" "${fact[leak]:-unchecked}" "0 hits"

  if [[ " ${judged[*]} " == *" enabled "* ]]; then
    read -r n max full_n full_max bad <<< "$(arm_figures "$table" enabled)"
    check "enabled: the arm was measured" "$([ "$n" -ge 1 ] && echo pass || echo fail)" "$n intervals" "at least 1"
    check "enabled: every interval is readable" "$([ "$bad" -eq 0 ] && echo pass || echo fail)" "$bad unreadable" "0"
    if [ "$n" -ge 1 ] && below "$max" "$enabled_ceiling"; then
      check "enabled: every interval below $enabled_ceiling CPU" pass "largest $max over $n intervals" "below $enabled_ceiling"
    else
      check "enabled: every interval below $enabled_ceiling CPU" fail "largest $max over $n intervals" "below $enabled_ceiling"
    fi
    check "enabled: the manifest was complete before the release" "$([ "${fact["enabled.packs_complete"]:-0}" = 1 ] && echo pass || echo fail)" "${fact["enabled.packs_complete"]:-0}" "1"
    check "enabled: $fetch_count fetches ran inside the budget" "$([ "${fact["enabled.fetches"]:-0}" = "$fetch_count" ] && [ "${fact["enabled.timeout"]:-1}" = 0 ] && echo pass || echo fail)" \
      "${fact["enabled.fetches"]:-0} results, timeout ${fact["enabled.timeout"]:-unset}" "$fetch_count results, no timeout"
    check "enabled: every fetch exits 0" "$([ "${fact["enabled.ok"]:-0}" = "$fetch_count" ] && echo pass || echo fail)" "${fact["enabled.ok"]:-0} of $fetch_count" "$fetch_count of $fetch_count"
    check "enabled: every fetch logs source=bucket" "$([ "${fact["enabled.bucket"]:-0}" = "$fetch_count" ] && echo pass || echo fail)" "${fact["enabled.bucket"]:-0} of $fetch_count" "$fetch_count of $fetch_count"
    check "enabled: no fetch logs source=/tar" "$([ "${fact["enabled.tar"]:-1}" = 0 ] && echo pass || echo fail)" "${fact["enabled.tar"]:-unread}" "0"
    check "enabled: every fetch verified the manifest's objects, name, size and SHA-256" "$([ "${fact["enabled.objects_match"]:-0}" = "$fetch_count" ] && echo pass || echo fail)" \
      "${fact["enabled.objects_match"]:-0} of $fetch_count" "$fetch_count of $fetch_count"
    if [ "${fact["enabled.streams_reads"]:-0}" -ge 1 ] && [ "${fact["enabled.streams_max"]:-1}" = 0 ]; then
      check "enabled: no /tar stream was in flight at any sample" pass "largest ${fact["enabled.streams_max"]:-unread} over ${fact["enabled.streams_reads"]:-0} /healthz reads" "0, over at least 1 read"
    else
      check "enabled: no /tar stream was in flight at any sample" fail "largest ${fact["enabled.streams_max"]:-unread} over ${fact["enabled.streams_reads"]:-0} /healthz reads" "0, over at least 1 read" \
        "a sample whose /healthz was never read shows nothing in flight"
    fi
  fi

  if [[ " ${judged[*]} " == *" disabled "* ]]; then
    read -r n max full_n full_max bad <<< "$(arm_figures "$table" disabled)"
    check "disabled: the arm was measured" "$([ "$n" -ge 1 ] && echo pass || echo fail)" "$n intervals" "at least 1"
    check "disabled: every interval is readable" "$([ "$bad" -eq 0 ] && echo pass || echo fail)" "$bad unreadable" "0"
    check "disabled: /packs is not served" "$([ "${fact["disabled.packs_status"]:-}" = 404 ] && echo pass || echo fail)" "${fact["disabled.packs_status"]:-unread}" "404"
    check "disabled: $fetch_count fetches ran inside the budget" "$([ "${fact["disabled.fetches"]:-0}" = "$fetch_count" ] && [ "${fact["disabled.timeout"]:-1}" = 0 ] && echo pass || echo fail)" \
      "${fact["disabled.fetches"]:-0} results, timeout ${fact["disabled.timeout"]:-unset}" "$fetch_count results, no timeout"
    check "disabled: every fetch exits 0" "$([ "${fact["disabled.ok"]:-0}" = "$fetch_count" ] && echo pass || echo fail)" "${fact["disabled.ok"]:-0} of $fetch_count" "$fetch_count of $fetch_count"
    check "disabled: every fetch logs source=/tar reason=/packs is not served" "$([ "${fact["disabled.unserved"]:-0}" = "$fetch_count" ] && echo pass || echo fail)" \
      "${fact["disabled.unserved"]:-0} of $fetch_count" "$fetch_count of $fetch_count"
    if [ "$full_n" -ge 1 ] && above "$full_max" "$control_floor"; then
      check "disabled: a full interval above $control_floor CPU (the control)" pass "largest full $full_max over $full_n full of $n intervals" "above $control_floor in at least one full interval"
    else
      check "disabled: a full interval above $control_floor CPU (the control)" void "largest full $full_max over $full_n full of $n intervals" "above $control_floor in at least one full interval" \
        "the run is too short to tell the arms apart, which is no pass"
    fi
  fi
  rm -f "$table"

  if [ "${#failed_checks[@]}" -gt 0 ]; then
    verdict_word=fail
  elif [ "${#void_checks[@]}" -gt 0 ]; then
    verdict_word=void
  else
    verdict_word=pass
  fi
}

# stopped_verdict <fail|void> <facts file>: sets verdict_word for a run that stopped before any arm
# took its end sample, which leaves no arm to judge: the stop's own word, or fail where the facts
# record a leak, since an output that holds a credential fails the run whatever else it proved.
stopped_verdict() {
  verdict_word=$1
  if grep -qx 'leak=1' "$2" 2> /dev/null; then
    check "no credential, signed address or token in any output" fail 1 "0 hits"
    verdict_word=fail
  fi
}

# --------------------------------------------------------------------------------------------
# The manifest check: /packs' answer against the clone it was made from.
# --------------------------------------------------------------------------------------------
manifest_summary=""

# manifest_complete <manifest file> <clone dir> <repo> <bucket> <tsv out>: 0 when the file is a
# manifest of that bucket whose objects are every pack and index of the clone, each with the clone
# file's size, every pack with its index; the tsv out then holds `name size sha256` for each. It
# prints nothing of an address; manifest_summary is the line to print.
manifest_complete() {
  local file=$1 clone=$2 repository=$3 bucket=$4 out=$5 prefix local_list got want f
  manifest_summary=""
  prefix=${repository,,}/objects/pack/
  jq -e --arg bucket "$bucket" '
    (.bucket == $bucket) and (.refs | type == "array" and length > 0) and (.objects | type == "array" and length > 0) and
    all(.objects[]; (.name | type == "string") and (.size | type == "number") and (.sha256 | type == "string" and test("^[0-9a-f]{64}$")) and (.url | type == "string" and length > 0))' \
    "$file" > /dev/null 2>&1 || { manifest_summary="the answer is no manifest of bucket $bucket with refs and objects"; return 1; }
  jq -r '.objects[] | [.name, (.size | tostring), .sha256] | @tsv' "$file" | sort > "$out"
  local_list=$(mktemp "${TMPDIR:-/tmp}/burst-local.XXXXXX") || return 1
  for f in "$clone"/objects/pack/pack-*.pack "$clone"/objects/pack/pack-*.idx; do
    [ -e "$f" ] || continue
    printf '%s\t%s\n' "$prefix${f##*/}" "$(stat -c %s "$f")"
  done | sort > "$local_list"
  got=$(cut -f1,2 "$out")
  want=$(<"$local_list")
  rm -f "$local_list"
  if [ -z "$want" ]; then
    manifest_summary="the clone holds no pack"
    return 1
  fi
  if [ "$got" != "$want" ]; then
    manifest_summary="the manifest's objects and sizes are not the clone's packs and indexes: manifest $(wc -l < "$out") objects, clone $(wc -l <<< "$want")"
    return 1
  fi
  if ! awk -F'\t' '{ n = $1; if (n ~ /\.pack$/) { sub(/\.pack$/, ".idx", n); want[n] = 1 } else have[$1] = 1 }
    END { for (n in want) if (!(n in have)) exit 1 }' "$out"; then
    manifest_summary="a pack of the manifest has no index"
    return 1
  fi
  manifest_summary="objects=$(wc -l < "$out") bytes=$(group "$(awk -F'\t' '{ s += $2 } END { printf "%d", s }' "$out")") refs=$(jq '.refs | length' "$file")"
}

# --------------------------------------------------------------------------------------------
# self-test: everything here runs on any Linux host with bash 4 or later, GNU coreutils, jq and awk.
# --------------------------------------------------------------------------------------------
self_failures=0
expect() { # expect <name> <got> <want>
  if [ "$2" = "$3" ]; then
    echo "ok   $1"
  else
    echo "FAIL $1: got [$2] want [$3]"
    self_failures=$((self_failures + 1))
  fi
}

# mk_samples <out> <<< "<arm> <kind> <seconds> <cpu seconds>"…: a samples file from decimal seconds.
mk_samples() {
  awk '{ printf "%s\t%s\t%d\t%d\n", $1, $2, ($3 * 1e6) + 1795000000000000, $4 * 1e6 }' > "$1"
}

# mk_facts <out> <key=value>…: a facts file of the passing run, with the given keys replaced.
mk_facts() {
  local out=$1 k
  shift
  local -A f=([attribution]=pass [leak]=0
    ["enabled.fetches"]=20 ["enabled.ok"]=20 ["enabled.timeout"]=0 ["enabled.streams_reads"]=3 ["enabled.bucket"]=20 ["enabled.tar"]=0 ["enabled.objects_match"]=20 ["enabled.streams_max"]=0 ["enabled.packs_complete"]=1
    ["disabled.fetches"]=20 ["disabled.ok"]=20 ["disabled.timeout"]=0 ["disabled.streams_reads"]=3 ["disabled.unserved"]=20 ["disabled.packs_status"]=404)
  for k in "$@"; do f[${k%%=*}]=${k#*=}; done
  for k in "${!f[@]}"; do printf '%s=%s\n' "$k" "${f[$k]}"; done > "$out"
}

self_test() {
  local dir out samples_file facts_file
  dir=$(mktemp -d "${TMPDIR:-/tmp}/burst-self.XXXXXX") || exit 2
  # shellcheck disable=SC2064 # the directory is this call's own
  trap "rm -rf '$dir'" EXIT
  samples_file=$dir/samples.tsv facts_file=$dir/facts.txt

  echo "== the cgroup parser, against the fixtures"
  find_counter "$fixtures/cgroup-v1.cgroup" "$fixtures/cgroup-v1.mountinfo"
  expect "v1 fixture: kind" "$counter_kind" v1
  expect "v1 fixture: file" "$counter_file" /sys/fs/cgroup/cpuacct/cpuacct.usage
  find_counter "$fixtures/cgroup-v2.cgroup" "$fixtures/cgroup-v2.mountinfo"
  expect "v2 fixture: kind" "$counter_kind" v2
  expect "v2 fixture: file" "$counter_file" /sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod00000000_0000_0000_0000_000000000000.slice/cri-containerd-0000000000000000000000000000000000000000000000000000000000000001.scope/cpu.stat
  printf '0::/other.slice/work\n' > "$dir/other.cgroup"
  printf '1210 1198 0:26 /kubepods.slice /sys/fs/cgroup ro - cgroup2 cgroup rw\n' > "$dir/narrow.mountinfo"
  find_counter "$dir/other.cgroup" "$dir/narrow.mountinfo" 2> /dev/null
  expect "a cgroup path outside its mount's root is refused" "${counter_kind:-none}" none
  : > "$dir/empty.mountinfo"
  find_counter "$fixtures/cgroup-v1.cgroup" "$dir/empty.mountinfo" 2> /dev/null
  expect "a mountinfo with no cgroup mount is refused" "${counter_kind:-none}" none
  find_counter "$dir/nothing" "$fixtures/cgroup-v1.mountinfo" 2> /dev/null
  expect "an unreadable cgroup file is refused" "${counter_kind:-none}" none

  echo "== the counter reads"
  printf '12176590000000\n' > "$dir/cpuacct.usage"
  counter_kind=v1 counter_file=$dir/cpuacct.usage
  read_counter
  expect "v1 reads nanoseconds as microseconds" "$counter_us" 12176590000
  printf 'usage_usec 4242\nuser_usec 4000\nsystem_usec 242\nnr_periods 0\n' > "$dir/cpu.stat"
  counter_kind=v2 counter_file=$dir/cpu.stat
  read_counter
  expect "v2 reads usage_usec" "$counter_us" 4242
  printf 'user_usec 4000\n' > "$dir/cpu.stat"
  read_counter 2> /dev/null
  expect "a cpu.stat without usage_usec is refused" "${counter_us:-none}" none

  echo "== the manifest check"
  mkdir -p "$dir/clone/objects/pack"
  printf 'pack-bytes' > "$dir/clone/objects/pack/pack-0123.pack"
  printf 'index' > "$dir/clone/objects/pack/pack-0123.idx"
  local sha
  sha=$(printf '%064d' 7)
  jq -n --arg sha "$sha" '{bucket: "example-legion-packs", refs: [{path: "HEAD", bytes: "cmVm"}],
    objects: [{name: "acme/widgets/objects/pack/pack-0123.pack", size: 10, sha256: $sha, url: "http://127.0.0.1:1/x"},
              {name: "acme/widgets/objects/pack/pack-0123.idx", size: 5, sha256: $sha, url: "http://127.0.0.1:1/y"}]}' > "$dir/manifest.json"
  manifest_complete "$dir/manifest.json" "$dir/clone" Acme/Widgets example-legion-packs "$dir/manifest.tsv"
  expect "a complete manifest passes" "$? $manifest_summary" "0 objects=2 bytes=15 refs=1"
  jq '.objects |= map(select(.name | endswith(".pack")))' "$dir/manifest.json" > "$dir/missing.json"
  manifest_complete "$dir/missing.json" "$dir/clone" Acme/Widgets example-legion-packs "$dir/manifest.tsv"
  expect "a manifest without the index is refused" "$?" 1
  jq '.objects[0].size = 11' "$dir/manifest.json" > "$dir/size.json"
  manifest_complete "$dir/size.json" "$dir/clone" Acme/Widgets example-legion-packs "$dir/manifest.tsv"
  expect "a manifest with another size is refused" "$?" 1
  manifest_complete "$dir/manifest.json" "$dir/clone" Acme/Widgets another-bucket "$dir/manifest.tsv"
  expect "a manifest of another bucket is refused" "$?" 1
  jq '.refs = []' "$dir/manifest.json" > "$dir/norefs.json"
  manifest_complete "$dir/norefs.json" "$dir/clone" Acme/Widgets example-legion-packs "$dir/manifest.tsv"
  expect "a manifest with no refs is refused" "$?" 1
  printf '{"error":"x"}' > "$dir/error.json"
  manifest_complete "$dir/error.json" "$dir/clone" Acme/Widgets example-legion-packs "$dir/manifest.tsv"
  expect "an answer that is no manifest is refused" "$?" 1

  echo "== the verdict, against synthetic series"
  # A passing run: the enabled arm well under 0.2 CPU in each interval, the last one partial; the
  # disabled arm over 0.5 in full intervals.
  mk_samples "$samples_file" << 'SERIES'
enabled start 0 100
enabled tick 10.000 100.5
enabled tick 20.001 101.2
enabled end 27.400 101.5
disabled start 0 200
disabled tick 10.000 209.0
disabled tick 20.000 218.5
disabled tick 30.001 227.0
disabled end 36.000 230.0
SERIES
  mk_facts "$facts_file"
  compute_verdict "$samples_file" "$facts_file" > "$dir/verdict.out"
  out=$(<"$dir/verdict.out")
  expect "a passing series passes" "$verdict_word" pass
  expect "its enabled intervals are 0.05, 0.07 and 0.04 CPU" "$(grep -c -E 'INTERVAL enabled #[123] .* CPU=0\.0[0-9]+' <<< "$out")" 3
  expect "its last interval is partial" "$(grep -c -E 'INTERVAL enabled #3 .* partial$' <<< "$out")" 1

  # An enabled interval at 0.25 CPU fails.
  mk_samples "$samples_file" << 'SERIES'
enabled start 0 100
enabled tick 10.000 100.5
enabled tick 20.000 103.0
enabled end 27.000 103.2
disabled start 0 200
disabled tick 10.000 209.0
disabled tick 20.000 218.5
disabled end 25.000 223.0
SERIES
  compute_verdict "$samples_file" "$facts_file" > "$dir/verdict.out"
  out=$(<"$dir/verdict.out")
  expect "an enabled interval at 0.25 CPU fails" "$verdict_word" fail
  expect "and names the 0.2 ceiling" "$(grep -c 'FAIL.*enabled: every interval below 0.2 CPU\|enabled: every interval below 0.2 CPU.*FAIL' <<< "$out")" 1

  # An enabled interval at exactly 0.2 CPU is not below 0.2.
  mk_samples "$samples_file" << 'SERIES'
enabled start 0 100
enabled tick 10.000 102.0
enabled end 17.000 102.5
disabled start 0 200
disabled tick 10.000 209.0
disabled end 20.000 218.0
SERIES
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "an enabled interval at exactly 0.2 CPU fails" "$verdict_word" fail

  # A partial last interval that is the one over the ceiling still fails.
  mk_samples "$samples_file" << 'SERIES'
enabled start 0 100
enabled tick 10.000 100.1
enabled end 12.000 100.9
disabled start 0 200
disabled tick 10.000 209.0
disabled end 20.000 218.0
SERIES
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a partial enabled interval over the ceiling fails" "$verdict_word" fail

  # A disabled arm that never crosses 0.5 in a full interval is void.
  mk_samples "$samples_file" << 'SERIES'
enabled start 0 100
enabled tick 10.000 100.5
enabled end 17.000 100.9
disabled start 0 200
disabled tick 10.000 204.0
disabled tick 20.000 208.0
disabled end 26.000 211.0
SERIES
  compute_verdict "$samples_file" "$facts_file" > "$dir/verdict.out"
  out=$(<"$dir/verdict.out")
  expect "a disabled arm below 0.5 in each full interval is void" "$verdict_word" void
  expect "and says it is the control that did not discriminate" "$(grep -c 'a full interval above 0.5 CPU (the control).*VOID' <<< "$out")" 1

  # A disabled arm over 0.5 only in a partial interval is void too.
  mk_samples "$samples_file" << 'SERIES'
enabled start 0 100
enabled tick 10.000 100.5
enabled end 17.000 100.9
disabled start 0 200
disabled tick 10.000 204.0
disabled end 15.000 209.0
SERIES
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a disabled arm over 0.5 only in a partial interval is void" "$verdict_word" void

  # A disabled arm whose only full interval is just under the width of one is partial.
  mk_samples "$samples_file" << 'SERIES'
enabled start 0 100
enabled tick 10.000 100.5
enabled end 17.000 100.9
disabled start 0 200
disabled end 9.4 209.0
SERIES
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "an interval of 9.4 seconds is no full interval" "$verdict_word" void

  # A run whose enabled arm failed and whose control did not discriminate is a failure, not void.
  mk_samples "$samples_file" << 'SERIES'
enabled start 0 100
enabled tick 10.000 103.0
enabled end 17.000 103.4
disabled start 0 200
disabled tick 10.000 204.0
disabled end 17.000 208.0
SERIES
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a failure outranks a void control" "$verdict_word" fail

  mk_samples "$samples_file" << 'SERIES'
enabled start 0 100
enabled tick 10.000 100.5
enabled end 17.000 100.9
disabled start 0 200
disabled tick 10.000 209.0
disabled end 20.000 218.0
SERIES
  mk_facts "$facts_file" enabled.bucket=19 enabled.tar=1
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a fetch that read /tar in the enabled arm fails" "$verdict_word" fail
  mk_facts "$facts_file" enabled.ok=19
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a fetch that exited non-zero fails" "$verdict_word" fail
  mk_facts "$facts_file" enabled.objects_match=19
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a fetch whose object lines differ from the manifest fails" "$verdict_word" fail
  mk_facts "$facts_file" enabled.streams_max=1
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a /tar stream in flight in the enabled arm fails" "$verdict_word" fail
  mk_facts "$facts_file" enabled.streams_reads=0
  compute_verdict "$samples_file" "$facts_file" > "$dir/verdict.out"
  out=$(<"$dir/verdict.out")
  expect "an enabled arm whose /healthz was never read fails" "$verdict_word" fail
  expect "and says no stream count was read" "$(grep -c 'no /tar stream was in flight at any sample: got \[largest 0 over 0 /healthz reads\].*FAIL' <<< "$out")" 1
  mk_facts "$facts_file" enabled.streams_reads=1
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "an enabled arm whose /healthz was read once, with no stream in flight, passes" "$verdict_word" pass
  mk_facts "$facts_file" disabled.unserved=19
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a disabled fetch with another reason fails" "$verdict_word" fail
  mk_facts "$facts_file" disabled.packs_status=200
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a disabled arm that serves /packs fails" "$verdict_word" fail
  mk_facts "$facts_file" leak=1
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a leak fails" "$verdict_word" fail
  mk_facts "$facts_file" attribution=fail
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a failed attribution is void" "$verdict_word" void
  mk_facts "$facts_file" enabled.timeout=1
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "an arm that ran out its budget fails" "$verdict_word" fail
  mk_facts "$facts_file" enabled.packs_complete=0
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "an incomplete manifest at the release fails" "$verdict_word" fail
  mk_facts "$facts_file" leak=1
  stopped_verdict void "$facts_file" > /dev/null
  expect "a run stopped void before any arm ended, with a leak, fails" "$verdict_word" fail
  mk_facts "$facts_file"
  stopped_verdict void "$facts_file" > /dev/null
  expect "a run stopped void before any arm ended, with no leak, is void" "$verdict_word" void
  : > "$facts_file"
  stopped_verdict void "$facts_file" > /dev/null
  expect "a run stopped void before the leak scan ran is void" "$verdict_word" void
  # A counter that fell is unreadable, which fails.
  mk_samples "$samples_file" << 'SERIES'
enabled start 0 100
enabled tick 10.000 99.0
enabled end 17.000 99.5
disabled start 0 200
disabled tick 10.000 209.0
disabled end 20.000 218.0
SERIES
  mk_facts "$facts_file"
  compute_verdict "$samples_file" "$facts_file" > /dev/null
  expect "a counter that fell fails" "$verdict_word" fail

  echo "== the leak scan"
  mkdir -p "$dir/secrets-run/secrets"
  printf 'abc123token' > "$dir/secrets-run/secrets/cache-token"
  printf 'plain line\n' > "$dir/clean.log"
  printf 'GET /x?X-Goog-Signature=zz\n' > "$dir/signed.log"
  printf 'Authorization: Bearer q\n' > "$dir/bearer.log"
  printf 'the token abc123token is here\n' > "$dir/token.log"
  local saved_run=$run
  run=$dir/secrets-run
  leak_scan "$dir/clean.log" > /dev/null
  expect "a clean log passes" "$?" 0
  leak_scan "$dir/signed.log" > /dev/null
  expect "a signed address's parameter is found" "$?" 1
  leak_scan "$dir/bearer.log" > /dev/null
  expect "a bearer header is found" "$?" 1
  leak_scan "$dir/token.log" > /dev/null
  expect "a token's value is found" "$?" 1
  expect "a finding names the file and no line" "$(leak_scan "$dir/token.log")" "LEAK: token.log holds the value of cache-token"
  run=$saved_run

  echo
  if [ "$self_failures" -eq 0 ]; then
    echo "SELF-TEST pass"
    return 0
  fi
  echo "SELF-TEST fail: $self_failures"
  return 1
}

# --------------------------------------------------------------------------------------------
# Role: double.
# --------------------------------------------------------------------------------------------
role_double() {
  open_nap || exit 2
  mkdir -p "$double_dir"/{home,gocache,gopath,tmp,objects}
  say "== the double: sources staged by the Job"
  (cd "$burst/module" && find . -name '*.go' -print0 | sort -z | xargs -0 sha256sum)
  export HOME=$double_dir/home GOCACHE=$double_dir/gocache GOPATH=$double_dir/gopath TMPDIR=$double_dir/tmp
  export GOWORK=off GOFLAGS=-mod=mod GOTOOLCHAIN=local CGO_ENABLED=0
  say "== go build, with the image's Go ($(go version 2>&1))"
  if ! (cd "$burst/module" && go build -o "$double_dir/fakegcs" ./internal/repocache/fakegcs/cmd/fakegcs) > "$double_dir/build.log" 2>&1; then
    echo "the double did not build:"
    show_log "$double_dir/build.log" 40
    echo "the double did not build" > "$run/double.failed"
    wait_file "$run/finished" "$deadline_s"
    exit 0
  fi
  sha256sum "$double_dir/fakegcs"
  "$double_dir/fakegcs" -listen "$double_listen" -root "$double_dir/objects" -email "$emulator_email" \
    > "$run/double.addr.tmp" 2> "$double_dir/fakegcs.log" &
  local pid=$!
  local waited=0
  while [ ! -s "$run/double.addr.tmp" ]; do
    kill -0 "$pid" 2> /dev/null || break
    [ "$waited" -lt 100 ] || break
    nap 0.1
    waited=$((waited + 1))
  done
  if [ -s "$run/double.addr.tmp" ] && kill -0 "$pid" 2> /dev/null; then
    mv "$run/double.addr.tmp" "$run/double.addr"
    say "the double serves on $(<"$run/double.addr")"
  else
    echo "the double did not start:"
    show_log "$double_dir/fakegcs.log" 20
    echo "the double did not start" > "$run/double.failed"
    wait_file "$run/finished" "$deadline_s"
    exit 0
  fi
  while kill -0 "$pid" 2> /dev/null; do
    if [ -e "$run/finished" ] || [ -e "$run/abort" ]; then break; fi
    nap 1
  done
  if kill -0 "$pid" 2> /dev/null; then
    kill -TERM "$pid" 2> /dev/null
    wait "$pid" 2> /dev/null
    say "the double stopped"
  else
    echo "the double ended on its own:"
    show_log "$double_dir/fakegcs.log" 20
    echo "the double ended on its own" > "$run/double.failed"
  fi
  exit 0
}

# --------------------------------------------------------------------------------------------
# Role: fetchers.
# --------------------------------------------------------------------------------------------
fetch_worker() { # fetch_worker <arm> <n>: waits at the barrier, then one fetch on a feed of its own
  local arm=$1 n=$2 start finish status
  local dir=$fetch_dir/$arm
  while [ ! -e "$run/$arm/go" ]; do
    if [ -e "$run/abort" ] || [ -e "$run/finished" ]; then return 0; fi
    nap 0.02
  done
  start=${EPOCHREALTIME/./}
  LEGION_PROVISION_TOKEN_FILE=$run/secrets/provision-token LEGION_REPOSITORY_CACHE_TOKEN_FILE=$run/secrets/cache-token \
    LEGION_DEFAULT_BRANCH=$default_branch \
    timeout -k 30 "$arm_budget_s" legion workspace-init fetch --repo "$repo" --feed "$dir/feed-$n" --cache-url "http://$cache_listen" \
    > "$dir/fetch-$n.log" 2>&1 < /dev/null
  status=$?
  finish=${EPOCHREALTIME/./}
  printf '%s\t%s\t%s\n' "$status" "$start" "$finish" > "$dir/fetch-$n.result"
}

default_branch=""
role_fetchers() {
  open_nap || exit 2
  mkdir -p "$fetch_dir/home" "$fetch_dir/tmp"
  export HOME=$fetch_dir/home TMPDIR=$fetch_dir/tmp XDG_CONFIG_HOME=$fetch_dir/home/.config
  export GIT_AUTHOR_NAME='Cache Burst' GIT_COMMITTER_NAME='Cache Burst' GIT_AUTHOR_EMAIL=burst@example.invalid GIT_COMMITTER_EMAIL=burst@example.invalid
  unset STORAGE_EMULATOR_HOST LEGION_REPOSITORY_CACHE_BUCKET GCE_METADATA_HOST
  say "== fetchers: the image's legion $(legion version 2>&1 | head -n 1), $fetch_count fetches an arm"

  # Attribution: a CPU loop here, for the conductor to read as another container's.
  if wait_file "$run/attrib/other-go" "$deadline_s"; then
    timeout "$attrib_loop_s" bash -c 'while :; do :; done'
    : > "$run/attrib/other-done"
    say "attribution loop done"
  fi

  local arm n pids pid
  for arm in enabled disabled; do
    wait_file "$run/$arm/armed" "$deadline_s" || { say "no $arm arm: the run ended"; exit 0; }
    default_branch=$(<"$run/default-branch")
    mkdir -p "$fetch_dir/$arm"
    pids=()
    for n in $(seq 1 "$fetch_count"); do
      fetch_worker "$arm" "$n" &
      pids+=("$!")
    done
    : > "$run/$arm/waiting"
    say "$arm: $fetch_count workers wait at the barrier"
    while [ -n "$(jobs -rp)" ]; do
      if [ -e "$run/$arm/kill" ]; then
        for pid in "${pids[@]}"; do pkill -TERM -P "$pid" 2> /dev/null; done
        break
      fi
      if [ -e "$run/abort" ]; then
        for pid in "${pids[@]}"; do pkill -TERM -P "$pid" 2> /dev/null; done
        break
      fi
      nap 1
    done
    wait
    : > "$run/$arm/fetchers-done"
    say "$arm: every fetch has exited"
  done
  wait_file "$run/finished" "$deadline_s"
  exit 0
}

# --------------------------------------------------------------------------------------------
# Role: cache, the conductor.
# --------------------------------------------------------------------------------------------
cache_pid=""
clone=""
stop_kind=""
stop_why=""
finished=0
facts=""
pack_bytes=0
arm_manifest_tsv=""
packs_complete=0
packs_status_disabled=""
release_us=0
arm_timeout=0
streams_max=0
streams_reads=0
streams_now=""
leak_hit=0
ready_health=""
f_fetches=0 f_ok=0 f_bucket=0 f_tar=0 f_unserved=0 f_match=0

# stop_run <fail|void> <why>: the run ends here, with that verdict unless its checks decide worse.
stop_run() {
  stop_kind=$1 stop_why=$2
  say "STOPPED ($1): $2"
}

start_cache() { # start_cache <arm>
  local arm=$1 how
  local -a envs=()
  mkdir -p "$cache_dir/logs" "$cache_dir/home" "$cache_dir/tmp" "$cache_dir/root"
  how="neither variable set"
  if [ "$arm" = enabled ]; then
    envs=("STORAGE_EMULATOR_HOST=$double_listen" "LEGION_REPOSITORY_CACHE_BUCKET=$fixture_bucket")
    how="STORAGE_EMULATOR_HOST and LEGION_REPOSITORY_CACHE_BUCKET set"
  fi
  # The disabled arm's process has neither variable, and the enabled arm's has exactly these two.
  env -u STORAGE_EMULATOR_HOST -u LEGION_REPOSITORY_CACHE_BUCKET -u GCE_METADATA_HOST \
    HOME="$cache_dir/home" TMPDIR="$cache_dir/tmp" "${envs[@]}" \
    LEGION_PROVISION_TOKEN_FILE="$run/secrets/provision-token" LEGION_REPOSITORY_CACHE_TOKEN_FILE="$run/secrets/cache-token" \
    setsid legion workspace-init cache --repo "$repo" --root "$cache_dir/root" --listen "$cache_listen" --refresh 24h \
    > "$cache_dir/logs/cache-$arm.log" 2>&1 < /dev/null &
  cache_pid=$!
  say "the $arm cache is process $cache_pid, $how"
}

stop_cache() {
  local waited=0
  [ -n "$cache_pid" ] || return 0
  kill -TERM -- "-$cache_pid" 2> /dev/null || kill -TERM "$cache_pid" 2> /dev/null
  while kill -0 "$cache_pid" 2> /dev/null && [ "$waited" -lt 600 ]; do
    nap 0.1
    waited=$((waited + 1))
  done
  if kill -0 "$cache_pid" 2> /dev/null; then
    kill -KILL -- "-$cache_pid" 2> /dev/null || kill -KILL "$cache_pid" 2> /dev/null
  fi
  wait "$cache_pid" 2> /dev/null
  cache_pid=""
}

# wait_ready <limit s>: 0 when /healthz answers ready, 1 at the limit, 2 when the cache ended.
wait_ready() {
  local started=$SECONDS health
  while :; do
    if health=$(curl -fsS --max-time 10 "http://$cache_listen/healthz" 2> /dev/null) && [ "$(jq -r .ready <<< "$health" 2> /dev/null)" = true ]; then
      ready_health=$health
      return 0
    fi
    kill -0 "$cache_pid" 2> /dev/null || return 2
    [ $((SECONDS - started)) -lt "$1" ] || return 1
    nap 5
  done
}

# wait_packs <limit s> <tsv out>: 0 once GET /packs answers 200 with a manifest of every pack and
# index of the clone; the manifest is read into a file that is removed at once, since it holds
# signed addresses.
wait_packs() {
  local started=$SECONDS status body=$cache_dir/tmp/manifest.json
  while :; do
    status=$(curl -sS --max-time 60 -o "$body" -w '%{http_code}' -H "@$run/secrets/auth-header" "http://$cache_listen/packs" 2> /dev/null) || true
    if [ "$status" = 200 ]; then
      if manifest_complete "$body" "$clone" "$repo" "$fixture_bucket" "$2"; then
        rm -f "$body"
        return 0
      fi
      echo "GET /packs answered 200 and $manifest_summary"
    fi
    rm -f "$body"
    kill -0 "$cache_pid" 2> /dev/null || { echo "the cache's process ended before /packs was complete"; return 2; }
    [ $((SECONDS - started)) -lt "$1" ] || { echo "GET /packs last answered ${status:-000}"; return 1; }
    nap 10
  done
}

# packs_status: the HTTP status GET /packs answers with the cache's token, 000 with none.
packs_status() {
  local status
  status=$(curl -sS --max-time 30 -o /dev/null -w '%{http_code}' -H "@$run/secrets/auth-header" "http://$cache_listen/packs" 2> /dev/null) || true
  printf '%s' "${status:-000}"
}

# read_streams: sets streams_now to /healthz's `streams`, the /tar requests in flight, with no fork.
read_streams() {
  local reply="" line="" hfd tries=0
  streams_now=""
  { exec {hfd}<> "/dev/tcp/${cache_listen%:*}/${cache_listen##*:}"; } 2> /dev/null || return 1
  printf 'GET /healthz HTTP/1.0\r\nHost: cache\r\n\r\n' >&"$hfd"
  while { IFS= read -r -t 5 -u "$hfd" line || [ -n "$line" ]; } && [ "$tries" -lt 100 ]; do
    reply+=$line
    line=""
    tries=$((tries + 1))
  done
  exec {hfd}>&-
  [[ $reply =~ \"streams\":([0-9]+) ]] || return 1
  streams_now=${BASH_REMATCH[1]}
}

# clone_record <arm>: the clone's refs and pack sizes, once per arm; sets default_branch and
# pack_bytes.
clone_record() {
  local arm=$1 refs head f size total=0
  refs=$(/usr/bin/git --git-dir="$clone" for-each-ref --format='%(refname)' 2> /dev/null | wc -l)
  head=$(/usr/bin/git --git-dir="$clone" rev-parse HEAD 2> /dev/null)
  default_branch=$(/usr/bin/git --git-dir="$clone" symbolic-ref --short HEAD 2> /dev/null)
  echo "CLONE $arm: refs=$refs head=${head:-unread} default_branch=${default_branch:-unread}"
  for f in "$clone"/objects/pack/*.pack "$clone"/objects/pack/*.idx; do
    [ -e "$f" ] || continue
    size=$(stat -c %s "$f")
    total=$((total + size))
    echo "CLONE $arm: ${f##*/} $(group "$size") bytes"
  done
  echo "CLONE $arm: packs and indexes $(group "$total") bytes"
  pack_bytes=$total
}

# collect_fetches <arm>: reads the 20 results and logs of an arm, prints one line for each, and sets
# the f_* figures. Nothing derived from a log is printed unless the arm's logs pass leak_scan. For
# the enabled arm the manifest's objects are in arm_manifest_tsv, which each fetch's object lines
# must equal.
re_summary='^workspace-init fetch: ([0-9,]+) bytes from the repository cache [^ ]+ (\(bucket\) )?in ([^,]+), its history verified in ([^;]+); ([0-9,]+) bytes from GitHub for the delta \(([0-9]+) refs?\)$'
collect_fetches() {
  local arm=$1
  local dir=$fetch_dir/$arm n idx status s_us e_us log wall offset source reason objects summary got line via name size sha printable=1
  f_fetches=0 f_ok=0 f_bucket=0 f_tar=0 f_unserved=0 f_match=0
  if ! leak_scan "$dir"/fetch-*.log; then
    leak_hit=1
    printable=0
    echo "the $arm fetches' output is withheld"
  fi
  for n in $(seq -w 1 "$fetch_count"); do
    idx=$((10#$n))
    log=$dir/fetch-$idx.log
    if [ ! -s "$dir/fetch-$idx.result" ]; then
      echo "FETCH $arm $n: no result"
      continue
    fi
    f_fetches=$((f_fetches + 1))
    IFS=$'\t' read -r status s_us e_us < "$dir/fetch-$idx.result"
    wall=$(awk -v s="$s_us" -v e="$e_us" 'BEGIN { printf "%.3f", (e - s) / 1e6 }')
    offset=$(((s_us - release_us) / 1000))
    source=none reason="" objects=0 summary=""
    [ "$status" != 0 ] || f_ok=$((f_ok + 1))
    if grep -qx 'workspace-init fetch: source=bucket' "$log"; then
      source=bucket
      f_bucket=$((f_bucket + 1))
    elif grep -q '^workspace-init fetch: source=/tar' "$log"; then
      source=/tar
      f_tar=$((f_tar + 1))
      reason=$(sed -n 's/^workspace-init fetch: source=\/tar reason=//p' "$log" | head -n 1)
      if [ "$reason" = "/packs is not served" ] && [ "$status" = 0 ]; then f_unserved=$((f_unserved + 1)); fi
    fi
    got=$(grep -E '^workspace-init fetch: bucket object=[^ ]+ size=[0-9]+ sha256=[0-9a-f]{64}$' "$log" |
      sed -E 's/^workspace-init fetch: bucket object=([^ ]+) size=([0-9]+) sha256=([0-9a-f]{64})$/\1\t\2\t\3/' | sort)
    objects=0
    [ -z "$got" ] || objects=$(wc -l <<< "$got")
    if [ "$arm" = enabled ] && [ "$source" = bucket ] && [ "$status" = 0 ] && [ -n "$got" ] && [ "$got" = "$(<"$arm_manifest_tsv")" ]; then
      f_match=$((f_match + 1))
    fi
    if [ "$printable" -eq 0 ]; then
      echo "FETCH $arm $n: exit=$status wall=${wall}s started=+${offset}ms after the release"
      continue
    fi
    line=$(grep -E '^workspace-init fetch: [0-9,]+ bytes from the repository cache ' "$log" | head -n 1)
    if [[ $line =~ $re_summary ]]; then
      via=/tar
      [ -z "${BASH_REMATCH[2]}" ] || via=bucket
      summary="from the cache ${BASH_REMATCH[1]} bytes in ${BASH_REMATCH[3]} ($via), verified in ${BASH_REMATCH[4]}, from GitHub ${BASH_REMATCH[5]} bytes (${BASH_REMATCH[6]} refs)"
    else
      summary="no summary line"
    fi
    echo "FETCH $arm $n: exit=$status wall=${wall}s started=+${offset}ms after the release source=$source${reason:+ reason=$reason} objects=$objects; $summary"
    if [ -n "$got" ]; then
      while IFS=$'\t' read -r name size sha; do
        echo "FETCH $arm $n:   bucket object=$name size=$size sha256=$sha"
      done <<< "$got"
    fi
    if [ "$status" != 0 ]; then show_log "$log" 8; fi
  done
}

# run_arm <enabled|disabled>: the arm's cache, the release of the fetches, the samples and the end.
# It answers 0 once the fetches were released and ended, 1 when the arm could not start (the run is
# stopped, stop_kind says how).
run_arm() {
  local arm=$1 started=$SECONDS released next target k rc avail double_bytes need name size sha
  arm_timeout=0 streams_max=0 streams_reads=0
  mkdir -p "$run/$arm" "$fetch_dir/$arm"
  say "================ the $arm arm"
  start_cache "$arm"
  say "waiting for the cache to answer ready (at most $clone_wait_s s: a first clone of a large repository is long)"
  wait_ready "$clone_wait_s"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "the $arm cache did not answer ready (code $rc); its log:"
    show_log "$cache_dir/logs/cache-$arm.log" 30
    stop_cache
    stop_run void "the $arm cache never answered ready, so nothing was measured"
    return 1
  fi
  say "the $arm cache is ready after $((SECONDS - started)) s: $ready_health"
  clone_record "$arm"
  if [ -z "$default_branch" ]; then
    stop_cache
    stop_run void "the clone names no default branch"
    return 1
  fi

  if [ "$arm" = enabled ]; then
    # Room for the double's copy and the 20 fresh feeds, read now that the clone's size is known.
    avail=$(df -B1 --output=avail "$scratch" | tail -n 1 | tr -d ' ')
    double_bytes=$(du -sb "$double_dir/objects" 2> /dev/null | cut -f1)
    need=$(awk -v p="$pack_bytes" -v d="${double_bytes:-0}" -v n="$fetch_count" 'BEGIN { c = p - d; if (c < 0) c = 0; printf "%d", p * n * 1.05 + c + 2147483648 }')
    echo "ROOM: $(group "$avail") bytes available; $fetch_count feeds of $(group "$pack_bytes") bytes, the double's copy and a margin need $(group "$need")"
    if [ "$avail" -lt "$need" ]; then
      stop_cache
      stop_run void "the volume cannot hold $fetch_count fresh feeds beside the clone and the double's copy"
      return 1
    fi
    say "waiting for GET /packs to answer 200 with a complete manifest (at most $packs_wait_s s)"
    wait_packs "$packs_wait_s" "$cache_dir/manifest-enabled.tsv"
    rc=$?
    if [ "$rc" -ne 0 ]; then
      echo "the cache's /packs was not complete (code $rc); its log:"
      show_log "$cache_dir/logs/cache-enabled.log" 30
      stop_cache
      stop_run fail "the cache with both keys set never answered GET /packs with a complete manifest"
      return 1
    fi
    packs_complete=1
    arm_manifest_tsv=$cache_dir/manifest-enabled.tsv
    echo "PACKS enabled: $manifest_summary"
    while IFS=$'\t' read -r name size sha; do
      echo "PACKS enabled: object $name size=$size sha256=$sha"
    done < "$arm_manifest_tsv"
  else
    packs_status_disabled=$(packs_status)
    echo "PACKS disabled: GET /packs with the cache's token answers $packs_status_disabled"
  fi

  printf '%s' "$default_branch" > "$run/default-branch.tmp" && mv "$run/default-branch.tmp" "$run/default-branch"
  : > "$run/$arm/armed"
  if ! wait_file "$run/$arm/waiting" 120 > /dev/null; then
    stop_cache
    stop_run void "the $fetch_count fetch workers were not waiting at the barrier"
    return 1
  fi

  # The release: the counter just before, then the barrier file the 20 workers wait on.
  if ! take_sample "$arm" start; then
    stop_cache
    stop_run void "the counter could not be read"
    return 1
  fi
  release_us=$sample_us
  released=$SECONDS
  printf '%s\n' "$release_us" > "$run/$arm/go.tmp" && mv "$run/$arm/go.tmp" "$run/$arm/go"
  say "released $fetch_count fetches at clock_us=$release_us"

  k=1
  next=$((release_us + interval_us))
  while :; do
    target=$next
    while [ "${EPOCHREALTIME/./}" -lt "$target" ]; do
      if [ -e "$run/$arm/fetchers-done" ]; then break 2; fi
      nap 0.05
    done
    take_sample "$arm" tick
    if read_streams; then
      streams_reads=$((streams_reads + 1))
      [ "$streams_now" -le "$streams_max" ] || streams_max=$streams_now
      echo "STREAMS $arm tick $k: $streams_now /tar streams in flight"
    fi
    if ! kill -0 "$cache_pid" 2> /dev/null; then
      echo "the $arm cache's process ended during the burst; its log:"
      show_log "$cache_dir/logs/cache-$arm.log" 30
      : > "$run/$arm/kill"
      stop_run fail "the $arm cache ended during the burst"
      break
    fi
    if [ $((SECONDS - released)) -ge "$arm_budget_s" ] && [ ! -e "$run/$arm/kill" ]; then
      echo "the $arm arm passed its budget of $arm_budget_s s: ending its fetches"
      : > "$run/$arm/kill"
      arm_timeout=1
    fi
    if [ $((SECONDS - released)) -ge $((arm_budget_s + 300)) ]; then
      stop_run void "the fetchers container did not report its fetches ended"
      break
    fi
    k=$((k + 1))
    next=$((next + interval_us))
  done
  take_sample "$arm" end
  say "the last fetch of the $arm arm has exited"
  wait_file "$run/$arm/fetchers-done" 120 > /dev/null
  stop_cache
  say "the $arm cache is stopped; its log has $(wc -l < "$cache_dir/logs/cache-$arm.log") lines"
  return 0
}

# record_arm_facts <arm>: reads the arm's fetches and appends what the verdict needs to the facts.
record_arm_facts() {
  local arm=$1
  collect_fetches "$arm"
  {
    printf '%s.fetches=%s\n%s.ok=%s\n%s.timeout=%s\n%s.streams_reads=%s\n' "$arm" "$f_fetches" "$arm" "$f_ok" "$arm" "$arm_timeout" "$arm" "$streams_reads"
    if [ "$arm" = enabled ]; then
      printf 'enabled.bucket=%s\nenabled.tar=%s\nenabled.objects_match=%s\nenabled.streams_max=%s\nenabled.packs_complete=%s\n' "$f_bucket" "$f_tar" "$f_match" "$streams_max" "$packs_complete"
    else
      printf 'disabled.unserved=%s\ndisabled.packs_status=%s\n' "$f_unserved" "$packs_status_disabled"
    fi
  } >> "$facts"
}

# write_config <file> <none|both|bucket>: the smallest legion.yaml `legion config check` accepts for
# a Kubernetes deployment (every value a placeholder), with the cache's two keys, none of them, or
# the bucket alone.
write_config() {
  {
    cat << YAML
project: WIDGETS
state_dir: ./state
bind: 10.0.0.5
daemon_url: http://legion-daemon-widgets.legion.svc:13370
operator_token_file: ./operator-token
dispatch_url: https://dispatch.example.com
dispatch_token_file: ./dispatch-token
envoy_url: http://envoy.example.com:9020
envoy_token_file: ./envoy-token
nats_urls: [nats://nats.example.com:4222]
projects:
  WIDGETS: {}
github_apps:
  implement:
    app_id: "100001"
    private_key_command: cat ./implement-app.pem
  review:
    app_id: "100002"
    private_key_command: cat ./review-app.pem
runtime:
  kubernetes:
    namespace: legion
    kubeconfig: ./kubeconfig
    image: ghcr.io/cybermonklabs/legion-worker@sha256:0000000000000000000000000000000000000000000000000000000000000000
    storage_class: standard
    session_store: postgres
    session_dsn_secret: SESSION_DSN
YAML
    case $2 in
      both) printf '    repository_cache:\n      bucket: %s\n      service_account: %s\n' "$fixture_bucket" "$fixture_account" ;;
      bucket) printf '    repository_cache:\n      bucket: %s\n' "$fixture_bucket" ;;
    esac
  } > "$1"
}

# config_check: the image's real `legion config check` on the three fixtures: both keys (OK),
# neither (OK), the bucket alone (refused, naming service_account). It ends the run (fail) at the
# first answer that is not that, since an image that refuses its own documented keys has no burst
# worth an hour.
config_check() {
  local dir=$cache_dir/config variant out status f
  rm -rf "$dir"
  mkdir -p "$dir"
  for f in operator-token dispatch-token envoy-token; do
    printf 'x\n' > "$dir/$f"
    chmod 0600 "$dir/$f"
  done
  # A kubeconfig the check reads and never dials (a TEST-NET-1 address): a Job's pod mounts no
  # ServiceAccount token, so a fixture naming none would send the check to in-cluster credentials.
  cat > "$dir/kubeconfig" << 'KUBECONFIG'
apiVersion: v1
kind: Config
clusters: [{name: example, cluster: {server: "https://192.0.2.20:6443"}}]
users: [{name: legion-daemon, user: {token: placeholder}}]
contexts: [{name: legion-daemon, context: {cluster: example, user: legion-daemon, namespace: legion}}]
current-context: legion-daemon
KUBECONFIG
  chmod 0600 "$dir/kubeconfig"
  write_config "$dir/enabled.yaml" both
  write_config "$dir/disabled.yaml" none
  write_config "$dir/partial.yaml" bucket
  for variant in enabled disabled partial; do
    out=$(cd "$dir" && LEGION_POSTGRES_DSN=postgres://legion@127.0.0.1:5432/legion legion config check --config "$variant.yaml" 2>&1)
    status=$?
    echo "CONFIG $variant: exit=$status: $(head -n 1 <<< "$out")"
    case $variant:$status in
      enabled:0 | disabled:0) ;;
      partial:1)
        if [[ $out != *repository_cache.service_account* ]]; then
          stop_run fail "legion config check refused the bucket alone without naming runtime.kubernetes.repository_cache.service_account"
          return 1
        fi
        ;;
      *)
        stop_run fail "legion config check exited $status on the $variant fixture"
        return 1
        ;;
    esac
  done
}

# attribution: the counter is this container's alone. Another container's 3-second CPU loop must
# move it by under attrib_other_max CPU-seconds, this container's own by attrib_own_min to
# attrib_own_max, and the measuring loop's own cost, idle for sampler_idle_s seconds, must be under
# sampler_idle_max CPU, a tenth of the ceiling it is read against. Otherwise the run is void: no
# figure of it could be believed.
attribution() {
  local before after other own idle limit ok=1 started_us
  mkdir -p "$run/attrib"
  read_counter
  before=$counter_us
  started_us=${EPOCHREALTIME/./}
  limit=$((started_us + sampler_idle_s * 1000000))
  while [ "${EPOCHREALTIME/./}" -lt "$limit" ]; do
    nap 0.05
  done
  read_counter
  after=$counter_us
  idle=$(awk -v a="$before" -v b="$after" -v s="$started_us" -v e="${EPOCHREALTIME/./}" 'BEGIN { printf "%.4f", ((b - a) / 1e6) / ((e - s) / 1e6) }')
  echo "ATTRIBUTION the measuring loop alone, idle for $sampler_idle_s s, cost $idle CPU (limit $sampler_idle_max)"

  read_counter
  before=$counter_us
  : > "$run/attrib/other-go"
  if ! wait_file "$run/attrib/other-done" 120 > /dev/null; then
    stop_run void "the fetchers container ran no loop to attribute"
    return 1
  fi
  read_counter
  after=$counter_us
  other=$(awk -v a="$before" -v b="$after" 'BEGIN { printf "%.3f", (b - a) / 1e6 }')
  echo "ATTRIBUTION a $attrib_loop_s-second CPU loop in the fetchers container moved this container's counter by $other CPU-seconds (limit $attrib_other_max)"

  read_counter
  before=$counter_us
  timeout "$attrib_loop_s" bash -c 'while :; do :; done'
  read_counter
  after=$counter_us
  own=$(awk -v a="$before" -v b="$after" 'BEGIN { printf "%.3f", (b - a) / 1e6 }')
  echo "ATTRIBUTION the same loop in this container moved its counter by $own CPU-seconds (between $attrib_own_min and $attrib_own_max)"

  below "$idle" "$sampler_idle_max" || ok=0
  below "$other" "$attrib_other_max" || ok=0
  { ! below "$own" "$attrib_own_min" && ! above "$own" "$attrib_own_max"; } || ok=0
  if [ "$ok" -eq 1 ]; then
    echo "ATTRIBUTION holds"
    echo "attribution=pass" >> "$facts"
  else
    echo "attribution=fail" >> "$facts"
    stop_run void "attribution does not hold: the counter is not this container's alone, or the sampler costs too much"
    return 1
  fi
}

# leak_check_all: every output the run kept, scanned once more at the end; the verdict reads it.
leak_check_all() {
  local -a files=("$cache_dir"/logs/*.log "$fetch_dir"/enabled/fetch-*.log "$fetch_dir"/disabled/fetch-*.log "$double_dir"/*.log)
  if leak_scan "${files[@]}" && [ "$leak_hit" -eq 0 ]; then
    echo "LEAK-SCAN: no output file holds a signed-address parameter, a bearer, an authorization header or a token"
    echo "leak=0" >> "$facts"
  else
    echo "leak=1" >> "$facts"
  fi
}

# has_sample <arm> <kind>: whether the samples hold that line.
has_sample() {
  awk -F'\t' -v arm="$1" -v kind="$2" '$1 == arm && $2 == kind { found = 1 } END { exit !found }' "$samples"
}

# role_cache_finish: the checks, the context and the one line, last.
role_cache_finish() {
  local word
  local -a judged=()
  stop_cache
  echo
  echo "======== the verdict"
  if [ -z "$stop_kind" ]; then
    judged=(enabled disabled)
  else
    echo "stopped: $stop_why"
    if has_sample enabled end; then judged+=(enabled); fi
    if has_sample disabled end; then judged+=(disabled); fi
  fi
  if [ -n "$stop_kind" ] && [ "${#judged[@]}" -eq 0 ]; then
    stopped_verdict "$stop_kind" "$facts"
    word=$verdict_word
  else
    compute_verdict "$samples" "$facts" "${judged[@]}"
    word=$verdict_word
    case $stop_kind in
      fail) word=fail ;;
      void) [ "$word" = fail ] || word=void ;;
    esac
  fi
  echo
  echo "image: ${BURST_IMAGE:-unnamed}"
  echo "legion version: $(legion version 2>&1 | head -n 1)"
  echo "tested revision: ${BURST_REVISION:-unread}"
  echo "repository: $repo"
  echo "checks failed: ${failed_checks[*]:-none}"
  echo "checks void: ${void_checks[*]:-none}"
  finished=1
  : > "$run/finished"
  echo "JOB-RESULT $word"
}

# on_exit: an end the conductor did not plan is a void run; the line is printed and the other
# containers are told to end.
on_exit() {
  if [ "$finished" -eq 0 ]; then
    stop_cache
    echo
    echo "the probe ended before its verdict"
    : > "$run/abort"
    finished=1
    echo "JOB-RESULT void"
  fi
  exit 0
}

role_cache() {
  local first email rc
  trap on_exit EXIT
  open_nap || { echo "no nap pipe"; exit 2; }
  mkdir -p "$cache_dir/logs" "$cache_dir/tmp" "$cache_dir/home"
  samples=$cache_dir/samples.tsv facts=$cache_dir/facts.txt
  : > "$samples"
  : > "$facts"
  clone=$cache_dir/root/$repo.git
  say "== the run"
  echo "repository: $repo"
  echo "image: ${BURST_IMAGE:-unnamed} (org.opencontainers.image.revision: ${BURST_IMAGE_REVISION:-unread})"
  echo "tested revision: ${BURST_REVISION:-unread}"
  echo "legion version: $(legion version 2>&1 | head -n 1)"
  echo "git: $(/usr/bin/git --version)"
  echo "uname: $(uname -a)"
  echo "id: $(id)"
  echo "processors: $(nproc)"
  echo "memory: $(grep MemTotal /proc/meminfo); this container's limit $(cat /sys/fs/cgroup/memory.max 2> /dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2> /dev/null || echo unread)"
  echo "the volume: $(awk -v at="$scratch" '$2 == at' /proc/mounts)"
  df -B1 "$scratch" | tail -n 1

  echo
  say "== the CPU counter, found from this container's own cgroup"
  print_cgroup_files /proc/self/cgroup /proc/self/mountinfo
  if ! find_counter /proc/self/cgroup /proc/self/mountinfo; then
    stop_run void "no CPU counter: $counter_why"
    role_cache_finish
    return
  fi
  echo "the counter: $counter_kind $counter_file"
  if ! read_counter; then
    stop_run void "the counter file cannot be read"
    role_cache_finish
    return
  fi
  first=$counter_us
  echo "first read: $first us"
  nap 1
  read_counter
  echo "second read: $counter_us us"
  if [ "$counter_us" -lt "$first" ]; then
    stop_run void "the counter went backwards"
    role_cache_finish
    return
  fi

  echo
  say "== the double"
  wait_file "$run/double.addr" "$double_wait_s" > /dev/null
  if [ -e "$run/double.failed" ] || [ ! -s "$run/double.addr" ]; then
    stop_run void "the double is not serving: $(cat "$run/double.failed" 2> /dev/null || echo 'no answer in time')"
    role_cache_finish
    return
  fi
  email=$(curl -fsS --max-time 10 -H 'Metadata-Flavor: Google' "http://$double_listen/computeMetadata/v1/instance/service-accounts/default/email" 2> /dev/null)
  echo "the double serves on $(<"$run/double.addr") and answers as $email"
  if [ "$email" != "$emulator_email" ]; then
    stop_run void "the double answers as [$email], not as $emulator_email, the identity the cache signs as in emulator mode"
    role_cache_finish
    return
  fi

  echo
  say "== legion config check, on the fixtures"
  config_check || { role_cache_finish; return; }

  echo
  say "== attribution and the sampler's own cost"
  attribution || { role_cache_finish; return; }

  run_arm enabled
  rc=$?
  if [ "$rc" -eq 0 ]; then
    record_arm_facts enabled
    rm -rf "$fetch_dir"/enabled/feed-*
    echo "the $fetch_count enabled feeds are deleted; $(df -B1 --output=avail "$scratch" | tail -n 1 | tr -d ' ') bytes are available"
  fi
  if [ "$rc" -eq 0 ] && [ -z "$stop_kind" ]; then
    run_arm disabled
    rc=$?
    [ "$rc" -ne 0 ] || record_arm_facts disabled
  fi
  leak_check_all
  role_cache_finish
}

# --------------------------------------------------------------------------------------------
# The role the container runs.
# --------------------------------------------------------------------------------------------
case ${1:-} in
  self-test)
    self_test
    exit $?
    ;;
  double) role_double ;;
  cache) role_cache ;;
  fetchers) role_fetchers ;;
  *)
    echo "usage: repository-cache-burst-probe.sh self-test | double | cache | fetchers" >&2
    exit 2
    ;;
esac
