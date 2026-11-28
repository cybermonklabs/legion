#!/usr/bin/env bash
# runner-load.sh <file>: appends to <file> one line a second, the runner's load at that moment,
# until it is killed.
#
# A job whose shell harnesses run beside each other through harness-pool.sh starts it in the
# background before it queues its first harness, and stops it before it uploads the pool's record,
# which keeps the file beside each harness's own times (.github/actions/harness-times-upload). An
# entry whose CPU is far below its wall waited, and the difference of two of these lines says on
# what over the interval between them: the processors' busy, idle, iowait and steal time, the run
# queue, the forks, and the time tasks stalled for a processor, for I/O or for memory.
#
# Each line is tab-separated, and each field after the first is the kernel's own text:
#   - the time, epoch seconds with microseconds;
#   - /proc/stat's aggregate `cpu` line (ticks of user, nice, system, idle, iowait, irq, softirq
#     and steal since boot), its `ctxt` and `processes` (context switches and forks since boot)
#     and its `procs_running` and `procs_blocked` (the tasks runnable, and those waiting on I/O,
#     at that moment);
#   - where the kernel keeps pressure stall information, each line of /proc/pressure/cpu, io and
#     memory, led by its kind: `some` and `full`, each with its `total`, the microseconds since
#     boot in which some task, or every task, stalled for that resource.
# Every file is read with the shell's own builtins, and the one program it starts is a `sleep` a
# second, so the sampler adds next to nothing to the load it reads.
set -euo pipefail

file=${1:?usage: runner-load.sh <file>}
while :; do
  line=$EPOCHREALTIME
  while IFS= read -r stat; do
    case $stat in
      'cpu '* | 'ctxt '* | 'processes '* | procs_*) line+=$'\t'$stat ;;
    esac
  done </proc/stat
  for kind in cpu io memory; do
    [[ -r /proc/pressure/$kind ]] || continue
    while IFS= read -r stat; do
      line+=$'\t'"$kind $stat"
    done <"/proc/pressure/$kind"
  done
  printf '%s\n' "$line" >>"$file"
  sleep 1
done
