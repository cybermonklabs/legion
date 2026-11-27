#!/usr/bin/env bash
# Tests for `dispatch-browser-run-reads.sh`, the three reads dispatch-browser.yaml's merge job
# makes of its own run: the jobs of this attempt, which the merge job's readings name legs from,
# the line that says a blob download could not be completed, and the lane's machine time, summed
# out of the jobs file the first read wrote.
#
# Each case writes the attempt's jobs a stand-in `gh` answers with - the list the jobs API holds
# for the attempt, which the stand-in hands out in pages of the route's size and past the first
# page only under `--paginate`, as gh does - and how many calls that stand-in refuses first, then
# runs the real script with the stand-in first on PATH and reads the file it wrote, the arguments
# the stand-in recorded and the lines the script printed. The ladder's waits are the case's own,
# `retry_seconds` seconds for the first and twice that for the second, which `fixture` resets to
# 0. A stand-in `sleep` records each wait the ladder asks for through the `sleep` on PATH, and
# the cases read those records, not how long the drive took: two cases set the knob to 1 and read
# the waits 1 s and 2 s, or 1 s. The control runs the drive of a gh answering at once under
# strace and holds it to none of the calls TIMED_WAITS lists, the census of the kernel's calls
# that wait out a time their caller gives them, arm a clock their caller sets, or wait for a
# signal or a lock (a futex, an fcntl or an ioctl counted only where it waits, leases or sets a
# terminal's attributes or a timed break). A `sleep` by any path, a `read -t`, a timed futex or
# futex_wait, semtimedop, mq_timedreceive, io_uring, a timerfd, alarm and pause, flock, a lock
# wait by fcntl, any setsockopt (SO_RCVTIMEO, SO_LINGER and the rest) and a terminal's VTIME set
# by any of its setters each make one. The waits the comment above TIMED_WAITS names as not
# seen - a `timeout N cmd` wait, which makes only the calls the drive's own `timeout` bounds
# make, a blocking call a timer_create timer or a process outside the drive's tree ends, a timer
# the kernel keeps itself (TCP's SYN and retransmission timers), and a busy loop in user space -
# make none.
#
# NO CASE REACHES THE NETWORK: every case is files, a stand-in gh, a stand-in `timeout` that
# four cases opt into call by call and that otherwise execs the real one, a stand-in `sleep` that
# records and returns, and the script.
#
# Run from anywhere: .github/scripts/dispatch-browser-run-reads.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# The fixtures are built in command substitutions, and bash does not carry errexit into one on
# its own: without this, a page that failed to build is read anyway and the case reports on
# whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
read_script="$script_dir/dispatch-browser-run-reads.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The run attempt every case drives with: the route carries it, and so does every refusal line.
run_attempt=4
# The id of the job that calls dispatch-browser.yaml, which every `jobs` drive passes: only the
# jobs named `<caller> / <job>` are that lane's.
caller=dispatch-browser

mkdir -p "$work/bin" "$work/calls"
cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
# The stand-in. It records the whole argument list of every call, one file per call, so a case
# can hold the route to the attempt endpoint, and answers from the files the case wrote: it
# refuses its first \`refuse\` calls with \`stderr\` on its own stderr and answers the calls after
# those with \`attempt.json\`, the attempt's jobs, in pages. A case whose attempts are refused for
# different reasons writes \`stderr.<n>\` for the n-th call, counting from 0, and that file
# answers in place of \`stderr\` for that call alone.
#
# The pages are the ones gh 2.98.0 asks GitHub for and prints (pkg/cmd/api in cli/cli), so a
# script that does not follow them reads the first page alone, as it would on a runner. A page
# holds the \`per_page\` the route names; a route that names none gets GitHub's default of 30
# without \`--paginate\`, and with it the 100 gh adds. Without \`--paginate\` the call prints the
# first page, one object; with it, every page, one object after another, or one array of them
# with \`--slurp\`; and \`--slurp\` without \`--paginate\` is refused in gh's own words.
#
# A case that writes \`refused-page\` moves the refusal of its refused calls from the first page
# to the page that file names: such a call prints every page before that one and then, as gh
# does, the error's body where that page would have been, then its stderr, and exits 1 - a read
# whose first pages answered and whose later one did not. A refused call that never asks for
# that page, one that does not follow the pages or whose attempt has fewer, is answered.
#
# A case that writes \`body\` gets it printed in place of the pages, with exit 0: an answer gh took
# whole that holds no jobs list.
#
# A case that writes \`kill-wrapper.<n>\` stages, for that call, a wrapper killed during the
# attempt: the stand-in kills its own grandparent, which is the script's \`bash -c\` wrapper,
# so that attempt's redirection has made \`stderr.<n>\` and nothing writes \`status.<n>\`. Two
# things are what let that kill reach the wrapper. The pid comes out of field 4 of
# \`/proc/<pid>/stat\`, the parent pid of this stand-in's own parent, so a host without
# \`/proc\` cannot stage the case at all and is refused by name below; and the stand-in
# \`timeout\` runs the real one with \`exec\`, so the process between gh and the wrapper is the
# real \`timeout\` itself rather than a stand-in holding the wrapper one generation further
# off. A kill that fails is this harness failing to stage the case and not the script
# misbehaving, so it says that in its own words and exits 70, a status nothing else in this
# file answers with. Those words reach the drive's output - the script's wrapper redirects
# this stand-in's stderr to a file of its own and puts that file back on stderr afterwards -
# and both cases that stage a kill check that they are not in it, so a kill that failed is
# red under a name that says this harness could not stage the case.
#
# A case that wrote \`stderr.<n>\` for the same call gets that file printed before the kill,
# which stages a killed wrapper whose gh had printed a status. The two numberings differ:
# \`kill-wrapper.<n>\` and \`stderr.<n>\` count this stand-in's own calls from 0, while the
# script's \`stderr.<n>\` and \`status.<n>\`, in its own work directory, are per-attempt files
# counting from 1, so \`kill-wrapper.2\` is the attempt that leaves the script's \`stderr.3\`
# and no \`status.3\` where the same drive wrote no \`stub-timeout.<n>\` file. An attempt the
# stand-in \`timeout\` answers itself calls no gh, so each such attempt before this one moves
# the attempt a \`kill-wrapper.<n>\` reaches one further on: the same drift the stand-in
# \`timeout\`'s own head describes for \`stderr.<n>\`. No case in this file writes both.
#
# \`gh api rate_limit\`, which gh-read.sh's wait reads a spent budget's reset from, is answered
# apart: it prints \`reset\` (what gh's \`--jq\` would print, the reset of a budget at 0, or 0) and
# is recorded under \`rate-calls\`, not \`calls\`, so it neither counts as an attempt nor moves the
# per-call files above.
set -euo pipefail
if [ "\${1:-}" = api ] && [ "\${2:-}" = rate_limit ]; then
  n=\$(find "$work/rate-calls" -maxdepth 1 -type f | wc -l)
  printf '%s\n' "\$*" > "$work/rate-calls/call.\$n"
  cat "$work/reset"
  exit 0
fi
calls=\$(find "$work/calls" -maxdepth 1 -type f | wc -l)
printf '%s\n' "\$*" > "$work/calls/call.\$calls"
if [ -f "$work/kill-wrapper.\$calls" ]; then
  [ ! -f "$work/stderr.\$calls" ] || cat "$work/stderr.\$calls" >&2
  if ! kill -9 "\$(awk '{print \$4}' "/proc/\$PPID/stat")"; then
    echo "dispatch-browser-run-reads.test.sh could not stage a killed wrapper: the kill of" \
      "the wrapper this stand-in was run by failed, so what follows is this harness failing" \
      "and not the script" >&2
    exit 70
  fi
  exit 1
fi
# refuse_stderr: what a refused call prints on its stderr.
refuse_stderr() {
  if [ -f "$work/stderr.\$calls" ]; then
    cat "$work/stderr.\$calls" >&2
  else
    cat "$work/stderr" >&2
  fi
}
refused_at=0
if [ "\$((calls + 1))" -le "\$(cat "$work/refuse")" ]; then
  if [ ! -f "$work/refused-page" ]; then
    refuse_stderr
    exit 1
  fi
  refused_at=\$(cat "$work/refused-page")
fi
if [ -f "$work/body" ]; then
  cat "$work/body"
  exit 0
fi
paginate=false slurp=false route=
for arg in "\${@:2}"; do
  case \$arg in
    --paginate) paginate=true ;;
    --slurp) slurp=true ;;
    -*) ;;
    *) [ -n "\$route" ] || route=\$arg ;;
  esac
done
if [ "\$slurp" = true ] && [ "\$paginate" = false ]; then
  echo '\`--paginate\` required when passing \`--slurp\`' >&2
  exit 1
fi
per_page=30
per_page_re='[?&]per_page=([0-9]+)'
if [[ \$route =~ \$per_page_re ]]; then
  per_page=\${BASH_REMATCH[1]}
elif [ "\$paginate" = true ]; then
  per_page=100
fi
# The pages this call asks for: the first alone, or with --paginate every page the attempt
# fills. A refusal on a page past those is one this call never meets.
jobs=\$(jq length "$work/attempt.json")
served=1
if [ "\$paginate" = true ] && [ "\$jobs" -gt "\$per_page" ]; then
  served=\$(( (jobs + per_page - 1) / per_page ))
fi
[ "\$refused_at" -le "\$served" ] || refused_at=0
jq -c --argjson per "\$per_page" --argjson served "\$served" --argjson refused "\$refused_at" \
  --argjson slurp "\$slurp" 'length as \$n | . as \$jobs
  | [range(0; \$served) | {total_count: \$n, jobs: \$jobs[(. * \$per):((. + 1) * \$per)]}]
  | if \$refused > 0 then .[:\$refused - 1] + [{message: "Server Error"}] else . end
  | if \$slurp then . else .[] end' "$work/attempt.json"
if [ "\$refused_at" -gt 0 ]; then
  refuse_stderr
  exit 1
fi
GH
chmod +x "$work/bin/gh"

# The real `timeout`, resolved before the stand-in below can shadow it: the stand-in execs it
# for every call no case asked it to stand in for. A host without `timeout` on PATH is refused
# here by name, because `type -P` answering nothing under `set -euo pipefail` ends this file at
# this line with no output at all - which tells such a host less than the failing cases naming
# `timeout: command not found` that it got before the stand-in existed.
real_timeout=$(type -P timeout) ||
  { echo "dispatch-browser-run-reads.test.sh needs timeout (GNU coreutils) on PATH" >&2
    exit 1; }

# The kill the stand-in gh above stages reads the wrapper's pid out of `/proc/<pid>/stat`, so a
# host without `/proc` is refused here by name too, in the same shape. Measured 2026-11-08 with
# this guard taken off a scratch copy and the kill given a `/proc` path that is not there, the
# awk cannot open the file, the kill is given no pid and fails, and both cases that stage a
# killed wrapper go red: 5 checks fail, three in the first and two in the second. Two of those
# reds name the staging, and none of them names the host this was run on, which is what this
# line is for.
[ -r /proc/self/stat ] ||
  { echo "dispatch-browser-run-reads.test.sh needs /proc (Linux) for the cases that stage a" \
      "killed wrapper" >&2
    exit 1; }

# The control's tracer, refused by name where there is none, in the shape of the guards above.
# GitHub's ubuntu-24.04-arm image, on which ci.yaml's test job runs this file, carries strace:
# that job's log read on 2026-11-21 (image version 20261116.188.1) holds `strace is already the
# newest version (6.8-0ubuntu2)`. So no step installs it, and an image without it fails here.
real_strace=$(type -P strace) ||
  { echo "dispatch-browser-run-reads.test.sh needs strace on PATH for its negative control" >&2
    exit 1; }
# The system calls the control's trace lists. They are the census of the kernel's x86_64, arm64
# and i386 syscall tables (torvalds/linux master c44104f3642d, read on 2026-11-21) for calls that
# wait out a time their caller gives them, arm a clock their caller sets, or wait for a signal or
# a lock, in three groups, with every other such call named below as unlisted and why.
#
# Calls that take a timeout of their own: nanosleep, clock_nanosleep, select (and i386's old
# select), _newselect (i386), pselect6, poll, ppoll, epoll_wait, epoll_pwait, epoll_pwait2,
# rt_sigtimedwait, futex, futex_wait (kernel 6.7), futex_waitv, semtimedop, mq_timedreceive,
# mq_timedsend, recvmmsg, io_getevents, io_pgetevents and io_uring_enter, with the i386 ABI's
# *_time64 names for the ones that have them. A sleep binary and bash's sleep builtin make
# clock_nanosleep or nanosleep, `read -t` makes pselect6, a thread or a helper waiting on a futex
# with a timeout makes futex. arm64 has no select, poll, epoll_wait, alarm or pause: on GitHub's
# ubuntu-24.04-arm runner (kernel 7.0.0-1017-azure, strace 6.8, 2026-11-21) glibc's alarm made
# setitimer and its pause made ppoll, and io_uring was allowed (io_uring_disabled 0): a 1 s
# io_uring_enter waited and ended ETIME there.
#
# Calls that arm a clock a later call waits out: timerfd_settime (a read of the timerfd waits),
# setitimer and alarm, sched_setattr (under SCHED_DEADLINE, sched_yield sleeps to the next
# period), io_uring_setup and io_uring_register, so that every use of io_uring, whose timeouts
# are its own (IORING_OP_TIMEOUT, a cancel with a timeout), is counted, and every setsockopt. Of
# socket(7) and tcp(7)'s options these set a time a later call waits out: SO_RCVTIMEO and
# SO_SNDTIMEO (recv, send, accept, connect), SO_LINGER (close and shutdown wait up to l_linger
# seconds for unsent data), SO_BUSY_POLL (a blocking recv, poll or epoll_wait busy-polls that
# many microseconds), TCP_USER_TIMEOUT (unacknowledged data ends the connection after that
# long), TCP_KEEPIDLE and TCP_KEEPINTVL (a silent peer ends a waiting recv after them),
# TCP_DEFER_ACCEPT (accept waits up to that long for data), TCP_LINGER2 and TCP_SYNCNT (the
# kernel's own FIN_WAIT2 and SYN timers). The drive makes no setsockopt at all, so the list
# counts every one rather than name the options one by one; i386 sets them through socketcall,
# whose subcall strace names setsockopt. Of the file and terminal options, every request that
# sets a terminal's attributes sets VTIME, which a later read of the terminal waits out: TCSETS,
# TCSETSW and TCSETSF (termios, as tcsetattr makes them), TCSETS2, TCSETSW2 and TCSETSF2
# (termios2), TCSETA, TCSETAW and TCSETAF (the legacy termio) and TCSETX, TCSETXW and TCSETXF
# (termiox) are the setters x86_64's and arm64's ioctl lists hold, the BSD TIOCSETP and TIOCSETN
# being absent from both. TCSBRKP and TCSBRK send a break that lasts a given number of
# deciseconds, or a fixed quarter second, and the call waits it out; TIOCSSERIAL sets a serial
# port's closing_wait, which a later close waits out. fcntl's F_SETLEASE makes another process's
# open or truncate of the file wait up to fs.lease-break-time for the holder. All of these are
# counted.
#
# Calls that wait for a signal or a lock and that the drive at this file's head makes none of:
# pause, flock, and fcntl or fcntl64 with F_SETLKW or F_OFD_SETLKW.
#
# Not listed, with why:
# - timer_create, timer_settime, rt_sigsuspend, sigsuspend (i386), wait4, waitid and waitpid
#   (i386): the drive's own `timeout` bounds arm their clock with timer_settime and wait in
#   rt_sigsuspend and wait4 (2, 2 and 40 of them traced on 2026-11-21 at the head of pull request #4755), so a
#   `timeout N cmd` wrapping a blocking command, or any blocking call a timer_create timer's
#   signal ends, makes only calls the drive makes too, and is not seen.
# - read, readv, pread64, preadv, preadv2, write and its kin, recvfrom, recvmsg, accept, accept4,
#   connect, sendto, sendmsg, sendmmsg, sendfile, splice, tee, vmsplice, msgrcv, msgsnd, semop,
#   getrandom and ptrace: they wait on a pipe, a socket, a device, a queue or another process,
#   and their caller gives them no time to wait out, and the drive makes reads and writes all the
#   time. Such a call can still wait out a timer the kernel keeps itself: a blocking connect to a
#   loopback listener whose accept queue is full waits out TCP's SYN retransmission timeout (1 s
#   at first), and a send waits out TCP's retransmission timer. Those kernel-internal timers are
#   a class no list of calls can see. In 3 traced runs on 2026-11-21 the drive made 8 connect
#   calls per run, all AF_UNIX to the absent nscd socket, non-blocking, none a TCP connect.
#   Another process in the drive's tree is traced under -f; one outside it, such as a network
#   peer, is not seen.
# - close and shutdown: they wait out SO_LINGER's time, and the setsockopt that set it is
#   counted; the drive closes files all the time.
# - restart_syscall: it resumes an interrupted wait, whose first call is already traced.
# - sched_yield and rseq_slice_yield: they give up the CPU and wait out no clock (sched_yield's
#   SCHED_DEADLINE case is sched_setattr's, above).
# - clock_gettime, clock_getres, gettimeofday, time, times, getitimer, timer_gettime,
#   timerfd_gettime, sched_rr_get_interval, clock_settime, clock_adjtime, adjtimex,
#   settimeofday, stime, utime, utimes, utimensat and futimesat, and their 64 and _time64 forms:
#   they read or set a clock or a file's times and wait for nothing.
# - prctl with PR_SET_TIMERSLACK lengthens a timed wait that is counted already; setrlimit and
#   prlimit64 with RLIMIT_CPU or RLIMIT_RTTIME signal after CPU time is used, which ends only a
#   busy loop (below). The drive's prlimit64 calls read RLIMIT_STACK, RLIMIT_NPROC and
#   RLIMIT_NOFILE.
# - A device's own ioctl requests that carry a timeout (SG_IO, USBDEVFS_BULK, I2C_TIMEOUT,
#   TIOCSRS485's delays and the like): no list can name every driver's requests. The drive's
#   ioctl calls are TCGETS, TIOCGPGRP and TIOCGWINSZ, and none of them sets a time.
# - i386's ipc and socketcall multiplexers need no names of their own: strace names their
#   subcalls (semtimedop, setsockopt), and the list matches those, as a 32-bit helper traced on
#   2026-11-21 showed.
# - A busy loop in user space, which spins on the clock with no system call. io_uring with
#   SQPOLL polled from user space is one, after an io_uring_setup the list counts.
#
# A regex, so strace matches only the calls the host's architectures have. What the trace
# establishes is that the drive made none of these calls, counted as timed_waits counts them, and
# no more. Every unlisted case named above can still wait unseen: a blocking call a timer_create
# timer's signal ends, which its caller gave a time through timer_settime, a `timeout N cmd`, a
# process outside the traced tree, a device's own timed ioctl request and a busy loop; so can a
# timer the kernel keeps itself (TCP's SYN and retransmission timers and the like). No check here
# reads elapsed time to make up for those.
TIMED_WAITS='/^(nanosleep|clock_nanosleep(_time64)?|select|_newselect|pselect6(_time64)?|poll|ppoll(_time64)?|epoll_wait|epoll_pwait|epoll_pwait2|rt_sigtimedwait(_time64)?|futex(_time64|_waitv|_wait)?|semtimedop(_time64)?|mq_timedreceive(_time64)?|mq_timedsend(_time64)?|recvmmsg(_time64)?|io_getevents|io_pgetevents(_time64)?|io_uring_setup|io_uring_enter|io_uring_register|timerfd_settime(64)?|setitimer|alarm|sched_setattr|setsockopt|pause|flock|fcntl(64)?|ioctl)$'

# timed_waits <trace>: the calls of a strace log the control counts, one per line. A call begins
# a line `<pid> <name>(`, where strace pads a short pid with spaces (`4     futex(`, as in a fresh
# PID namespace or on a runner whose pids are short), so the separator is any run of blanks; one
# another process's call interrupted goes on in a `<... name resumed>` line, which is no new call
# and is passed over. A futex, futex_time64, futex_wait or futex_waitv call counts only where its
# timeout argument is a struct, `{tv_sec=`: a lock's FUTEX_WAIT has NULL there and FUTEX_WAKE has
# no timeout at all. An fcntl or fcntl64 counts only where it is F_SETLKW (F_SETLKW64 on i386),
# F_OFD_SETLKW or F_SETLEASE, and an ioctl only where it is one of the terminal requests above
# (TCSET followed by S, A or X, TCSBRK, TCSBRKP or TIOCSSERIAL). Every call of the other names
# counts. The drive at this file's head makes none that counts: traced on 2026-11-21, in each of
# 3 runs it made 0 of every listed call but futex, fcntl and ioctl, 27 futex(FUTEX_WAKE_PRIVATE)
# calls, none of them timed, 51 fcntl calls (F_GETFD, F_SETFD, F_DUPFD, F_DUPFD_CLOEXEC and
# F_GETFL), none of them a lock wait or a lease, and 26 ioctl calls (TCGETS, TIOCGPGRP and
# TIOCGWINSZ), none of them a setter. A `/usr/bin/sleep 1` after gh answered made
# clock_nanosleep, a `read -t 1` on a FIFO pselect6, and a 1 s FUTEX_WAIT_PRIVATE futex with
# `{tv_sec=1, tv_nsec=0}`.
timed_waits() {
  { grep -E '^[0-9]+[[:space:]]+[a-z_0-9]+\(' "$1" || true; } |
    { grep -Ev '^[0-9]+[[:space:]]+(futex(_time64|_waitv|_wait)?|fcntl(64)?|ioctl)\(' || true; }
  grep -E '^[0-9]+[[:space:]]+futex(_time64|_waitv|_wait)?\(.*\{tv_sec=' "$1" || true
  grep -E '^[0-9]+[[:space:]]+fcntl(64)?\([^,]*, F_((OFD_)?SETLKW|SETLEASE)' "$1" || true
  grep -E '^[0-9]+[[:space:]]+ioctl\([^,]*, (TCSET[SAX]|TCSBRK|TIOCSSERIAL)' "$1" || true
}

# The stand-in `timeout`, beside the stand-in `gh` and opted into per call. It records the
# whole argument list of every call it is given but gh-read.sh's own rate_limit read (the last
# paragraph here), one file per call, whether or not it stands in for that call, so
# `timeout_calls` and `timeout_args` read the bound and the command that was wrapped in every
# drive in this file.
#
# Standing in is what is opt-in, and the reason is not the recording: a stand-in every case
# used would still catch a dropped bound through `timeout_calls`. It is that a call it stands
# in for exits at once without running gh - with 124, which is what the real timeout exits
# when its bound fires, or with whatever status the case asked for instead - and every other
# case in this file needs gh's answer: the page it writes, the stderr of the calls it refuses,
# the arguments it records.
#
# A case asks for call `<n>` to be stood in for by writing `$work/stub-timeout.<n>`, counting
# from 0, so `stub-timeout.<n>` names attempt n + 1 of the ladder: this stand-in is called
# once for every attempt, whether it stands in for that call or execs the real timeout. An
# empty file is the bound firing, which is 124, and a file holding a status is answered with
# that status instead, which is how the case about an attempt exiting 127 is staged.
#
# The stand-in gh's `stderr.<n>` counts from 0 too and counts something else - gh's own calls,
# which hold no call for an attempt this stand-in answered itself. So `stderr.<n>` names gh's
# own n-th call rather than attempt n + 1, and in a case that mixes the two the two
# numberings drift apart. `fixture` removes those files again, and a call no file was written
# for runs the real timeout this execs, so that attempt's gh is bounded as the script asked.
#
# gh-read.sh's own read of the rate_limit route, `timeout 30 gh api rate_limit ...`, is no
# attempt: the helper makes it after an attempt that exited 1, since a paginated read leaves it
# no answer to read. It goes to the real timeout unrecorded, and the stand-in gh answers it apart.
cat > "$work/bin/timeout" <<TIMEOUT
#!/usr/bin/env bash
set -euo pipefail
if [ "\${2:-} \${3:-} \${4:-}" = "gh api rate_limit" ]; then
  exec $real_timeout "\$@"
fi
calls=\$(find "$work/timeout-calls" -maxdepth 1 -type f | wc -l)
printf '%s\n' "\$*" > "$work/timeout-calls/call.\$calls"
if [ ! -f "$work/stub-timeout.\$calls" ]; then
  exec $real_timeout "\$@"
fi
answered=\$(cat "$work/stub-timeout.\$calls")
exit "\${answered:-124}"
TIMEOUT
chmod +x "$work/bin/timeout"

# The stand-in `sleep`, which the ladder in gh-read.sh calls between attempts. It records the
# seconds of each call, one file per call, and returns at once: what the cases read is the wait
# the ladder asked for, not how long it took, so no case waits it out. The cases about the waits
# read these records and not the drive's wall-clock time, which a loaded host stretches. Two
# checks here held a drive to at most 1 s of SECONDS: measured 2026-11-21 on a shared 288-CPU dev
# box, one of them failed in an unforced run with nothing wrong in the script, and with each `cat`
# delayed both did.
cat > "$work/bin/sleep" <<SLEEP
#!/usr/bin/env bash
set -euo pipefail
calls=\$(find "$work/sleep-calls" -maxdepth 1 -type f | wc -l)
printf '%s\n' "\$*" > "$work/sleep-calls/call.\$calls"
SLEEP
chmod +x "$work/bin/sleep"

# The stand-in for the runner's per-job process, which gh-read.sh reads the job's start off: bash
# under the name Runner.Worker (drive_limited runs the script under it).
ln -s "$(type -P bash)" "$work/bin/Runner.Worker"

# page <id>|<name>|<conclusion>[|<seconds>]...: the attempt's jobs, in the order the jobs API
# lists them, which the stand-in answers in pages of the route's size (its head says how).
# `run_attempt` is in each job because the jobs API puts it there; nothing in the script reads
# it any more, which is what the entries this writes are held against.
#
# `<seconds>` is how long that job took, and the two timestamps the jobs API answers with are
# written from it: `started_at` at the fixed epoch below and `completed_at` that many seconds
# later. A job given no seconds gets a null `completed_at`, which is what the API answers for a
# job that has not finished - the merge job itself while it reports - and `-1` writes the
# backwards pair the API answers for a skipped job (run 43002633713's alone job, started
# 2026-11-11T10:22:27Z and completed 10:22:26Z, read 2026-11-11).
started_epoch=1794392547  # 2026-11-11T10:22:27Z
page() {
  jq -n --argjson started "$started_epoch" '[$ARGS.positional[] | split("|")
      | {id: (.[0] | tonumber), name: .[1],
         conclusion: (if .[2] == "null" then null else .[2] end),
         started_at: ($started | todateiso8601),
         completed_at: (if (.[3] // "") == "" then null
           else ($started + (.[3] | tonumber) | todateiso8601) end),
         run_attempt: 2}]' \
    --args "$@" > "$work/attempt.json"
}

# happy_page: one attempt of a run of this lane, as the jobs API names it. A called workflow's
# job is named `<the caller's job> / <this job>`, with its matrix leg in brackets after it, and
# the caller's own jobs are in the same list: `lint` and `dispatch-multiprocess`, which no
# caller's prefix names. So are the jobs of ci.yaml's other caller of the workflow, which a pull
# request labelled full-ci runs in the same run under a different caller id.
#
# The seconds are a full lane of this file's shape, in round numbers rather than readings: the
# eight jobs of run 43074443876 took binaries 171, shard (1) 577, shard (2) 705, alone 199,
# multiprocess 125, embedder 55, visual 114 and merge 28 seconds (the jobs API, read
# 2026-11-11), and this page carries numbers of that order, no embedder job and a skipped
# multiprocess job, because what the cases read is the arithmetic and not the run. The five jobs
# of this workflow under dispatch-browser that this page gives a conclusion and an end, the merge
# job apart, sum to 1,750 s, which is inside the full lane's budget and over the pull-request
# lane's, so one fixture drives both arms of the machine-time cases below. The caller's two jobs
# carry 9,999 s each: a sum that held either of them would be past every budget this file has.
# The other caller's real and stub legs total 800 s, inside the pull-request lane's budget.
# Neither belongs to the full lane's sum. The retired embedder job name is dropped.
happy_page() {
  page "10|dispatch-browser / binaries|success|150" \
    "11|dispatch-browser / shard (1)|success|600" \
    "12|dispatch-browser / shard (2)|failure|700" \
    "13|dispatch-browser / alone|success|200" \
    "14|dispatch-browser / multiprocess|skipped|-1" \
    "15|dispatch-browser / merge|null" \
    "16|lint|success|9999" \
    "17|dispatch-multiprocess|success|9999" \
    "18|dispatch-browser / visual|failure|100" \
    "19|dispatch-browser-pr / real|failure|500" \
    "20|dispatch-browser-pr / merge|null" \
    "27|dispatch-browser-pr / stub|success|300" \
    "28|dispatch-browser-pr / embedder|failure|9999"
}

# two_page_attempt: an attempt of 40 jobs, which the route's 30 a page answers over two pages,
# with every lane's jobs on both. That is the shape of run 45913688080, whose attempts 2 and 3
# answered 95 jobs over four pages of 30 and held the full lane's jobs on three of attempt 2's
# pages and two of attempt 3's (read 2026-11-27). The first page holds the full lane's binaries
# job, the pull-request lane's real leg, the references lane's first shard and 27 of ci.yaml's
# own jobs at 60 s each, which no caller's prefix names; the second holds the rest of each lane.
# The full lane's jobs are the happy page's and sum to the same 1,750 s, 150 s of them on the
# first page and 1,600 s on the second, and the pull-request lane's 800 s are 500 s on the first
# and 300 s on the second, so a read of either page alone sums to a figure no case accepts.
two_page_attempt() {
  local -a ci_jobs=()
  local n
  for ((n = 1; n <= 27; n++)); do
    ci_jobs+=("$((100 + n))|test ($n)|success|60")
  done
  page "10|dispatch-browser / binaries|success|150" \
    "19|dispatch-browser-pr / real|failure|500" \
    "40|dispatch-references / shard (1)|success|157" \
    "${ci_jobs[@]}" \
    "11|dispatch-browser / shard (1)|success|600" \
    "12|dispatch-browser / shard (2)|failure|700" \
    "13|dispatch-browser / alone|success|200" \
    "14|dispatch-browser / multiprocess|skipped|-1" \
    "18|dispatch-browser / visual|failure|100" \
    "15|dispatch-browser / merge|null" \
    "27|dispatch-browser-pr / stub|success|300" \
    "20|dispatch-browser-pr / merge|null" \
    "41|dispatch-references / shard (2)|success|189" \
    "42|dispatch-references / merge|null"
}

# lane_jobs <name>|<conclusion>[|<seconds>]...: a jobs file of the shape the `jobs` mode writes,
# for the machine-time cases that need a sum the happy page does not give. The case above it
# drives the two modes one after the other over one page, so that what this writes is held to
# the shape the script itself writes; these entries are the arithmetic.
lane_jobs() {
  jq -n --argjson started "$started_epoch" '[$ARGS.positional | to_entries[]
    | (.value | split("|")) as $f
    | {id: (100 + .key), name: $f[0],
       conclusion: (if $f[1] == "null" then null else $f[1] end),
       started_at: ($started | todateiso8601),
       completed_at: (if ($f[2] // "") == "" then null
         else ($started + ($f[2] | tonumber) | todateiso8601) end),
       leg: null}]' --args "$@" > "$work/jobs.json"
}

# fixture <refused calls> [<stderr of a refused call>]: a fresh call log and no out-file, a
# stand-in that refuses that many calls, each on its first page, and then answers, and the happy
# page for the answer. It also puts the ladder's wait back to 0, so a case that does not ask for
# one waits not at all and the three that do set `retry_seconds` themselves after calling it. Any
# per-call `stderr.<n>` an earlier case wrote goes with it, so one case's faults never answer
# another's call, and so do the stand-in `timeout`'s per-call `stub-timeout.<n>` files and its
# own call log, the stand-in gh's per-call `kill-wrapper.<n>` files, its `refused-page` and its
# `body`, and the stand-in `sleep`'s call log. The machine-time cases' own two go with them: the
# jobs file a case wrote for that mode, and the step summary, which is no file at all until a case
# names one, so a case that does not ask for a summary drives the script with GITHUB_STEP_SUMMARY
# empty, as a shell that runs it outside Actions has it.
fixture() {
  rm -rf "$work/calls" "$work/out.json" "$work/timeout-calls" "$work/sleep-calls" "$work/rate-calls"
  rm -f "$work"/stderr.* "$work"/stub-timeout.* "$work"/kill-wrapper.* "$work/jobs.json" \
    "$work/summary.md" "$work/refused-page" "$work/body"
  mkdir -p "$work/calls" "$work/timeout-calls" "$work/sleep-calls" "$work/rate-calls"
  printf '%s\n' "${1:-0}" > "$work/refuse"
  printf '%s\n' "${2:-HTTP 502}" > "$work/stderr"
  printf '0\n' > "$work/reset"
  retry_seconds=0
  summary_file=
  happy_page
}

# drive <argument>...: the real script with the stand-in first on PATH. Sets `out` to its
# combined output and `status` to its exit code. Not a subshell, so both survive the call.
drive() {
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
    GITHUB_RUN_ATTEMPT=$run_attempt GH_RETRY_SECONDS=$retry_seconds \
    GITHUB_STEP_SUMMARY="$summary_file" "$read_script" "$@" 2>&1)
  status=$?
  set -e
}

# drive_limited [NAME=value...] -- <argument>...: drive, under a stand-in for the runner's
# per-job process (bash under the name Runner.Worker, which gh-read.sh reads the job's start
# off), with the NAME=value pairs set and JOB_TIMEOUT_MINUTES and STEP_TIMEOUT_MINUTES unset
# unless a pair sets them, so a case decides the limit gh-read.sh's wait is held to.
drive_limited() {
  local -a assignments=()
  while [ "$1" != -- ]; do
    assignments+=("$1")
    shift
  done
  shift
  set +e
  out=$(env -u JOB_TIMEOUT_MINUTES -u STEP_TIMEOUT_MINUTES PATH="$work/bin:$PATH" GH_TOKEN=t \
    GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 GITHUB_RUN_ATTEMPT=$run_attempt \
    GH_RETRY_SECONDS=$retry_seconds "${assignments[@]}" \
    "$work/bin/Runner.Worker" -c '"$@"; exit $?' worker "$read_script" "$@" 2>&1)
  status=$?
  set -e
}
rate_calls() { find "$work/rate-calls" -maxdepth 1 -type f | wc -l | tr -d ' '; }
utc() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }

calls() { find "$work/calls" -maxdepth 1 -type f | wc -l | tr -d ' '; }
args() { cat "$work/calls/call.0"; }
timeout_calls() { find "$work/timeout-calls" -maxdepth 1 -type f | wc -l | tr -d ' '; }
timeout_args() { cat "$work/timeout-calls/call.0"; }
entries() { jq -r 'length' "$work/out.json"; }
field() { jq -r --argjson id "$1" ".[] | select(.id == \$id) | $2" "$work/out.json"; }
errors() { grep -c '::error::' <<<"$out" || true; }
error_line() { grep '::error::' <<<"$out" || true; }
wrote_out_file() { [ -e "$work/out.json" ] && echo yes || echo no; }
notice_line() { grep '::notice' <<<"$out" || true; }
summary_text() { cat "$work/summary.md" 2>/dev/null || true; }
# sleeps: the arguments of each call the ladder made to the stand-in `sleep`, in order and joined
# by `|`, so one call given two arguments (`1 2`) reads apart from two calls; nothing where it made
# none.
sleeps() {
  local n=0 waits=""
  while [ -f "$work/sleep-calls/call.$n" ]; do
    waits="$waits${waits:+|}$(cat "$work/sleep-calls/call.$n")"
    n=$((n + 1))
  done
  echo "$waits"
}

echo "case: this attempt's jobs become the six fields the readings consume"
fixture 0
drive jobs "$work/out.json" "$caller"
check "exits 0" "$(is "$status" 0)"
check "writes every job of this workflow and none of the caller's" "$(is "$(entries)" 7)"
check "each entry holds the six fields and no other" \
  "$(is "$(jq -r '.[0] | keys | join(",")' "$work/out.json")" \
    "completed_at,conclusion,id,leg,name,started_at")"
check "a job's two timestamps travel as the API answered them, for reading five to sum" \
  "$(is "$(field 11 '"\(.started_at) \(.completed_at)"')" \
    '2026-11-11T10:22:27Z 2026-11-11T10:32:27Z')"
check "and a job still running carries the null completed_at the API gives it" \
  "$(is "$(field 15 .completed_at)" null)"
check "a shard job's id, name and conclusion travel unchanged" \
  "$(is "$(field 12 '"\(.id) \(.name) \(.conclusion)"')" '12 dispatch-browser / shard (2) failure')"
check "the alone job's too" \
  "$(is "$(field 13 '"\(.id) \(.name) \(.conclusion)"')" \
    '13 dispatch-browser / alone success')"
check "the first shard leg reads shard-1" "$(is "$(field 11 .leg)" shard-1)"
check "the second reads shard-2" "$(is "$(field 12 .leg)" shard-2)"
check "the alone job reads the leg alone" "$(is "$(field 13 .leg)" alone)"
check "the multiprocess job carries a null leg" "$(is "$(field 14 .leg)" null)"
check "and keeps its conclusion" "$(is "$(field 14 .conclusion)" skipped)"
check "the merge job carries a null leg" "$(is "$(field 15 .leg)" null)"
check "and its conclusion, which is null while it runs" "$(is "$(field 15 .conclusion)" null)"
check "the visual job is kept, with a null leg" "$(is "$(field 18 .leg)" null)"
check "and keeps its conclusion, so reading three can name it when it is red" \
  "$(is "$(field 18 .conclusion)" failure)"
check "the binaries job is kept, with a null leg" "$(is "$(field 10 .leg)" null)"
check "and keeps its conclusion, so reading three can name a build that failed" \
  "$(is "$(field 10 .conclusion)" success)"
check "the caller's own jobs are dropped" \
  "$(is "$(jq -r '[.[] | select(.id == 16 or .id == 17)] | length' "$work/out.json")" 0)"
check "and so are the other caller's" \
  "$(is "$(jq -r '[.[] | select(.id == 19 or .id == 20 or .id == 27 or .id == 28)] | length' "$work/out.json")" 0)"
check "prints no ::error:: line" "$(is "$(errors)" 0)"

echo "case: the route is this run's attempt endpoint"
# The same drive: what the stand-in recorded is what the script asked GitHub for. Thirty to a
# page, named in the route, because a page of 100 answered HTTP 502 on every try for run
# 45913688080 and a paginated route that names no size reads 100 a page (the script's head).
check "names this run's attempt" "$(contains "$(args)" "/attempts/$run_attempt/jobs")"
check "under this repository and run" "$(contains "$(args)" 'repos/o/r/actions/runs/41/')"
check "thirty to a page" "$(contains "$(args)" 'jobs?per_page=30$')"
check "and holds no filter=all" "$(is "$(contains "$(args)" 'filter=all')" false)"
check "slurps the pages, since gh refuses --slurp beside --jq" \
  "$(contains "$(args)" '--paginate --slurp')"

echo "case: the other caller's merge job reads its own jobs alone"
# ci.yaml's dispatch-browser-pr job calls the same workflow, and on a pull request labelled
# full-ci it runs in the same run as dispatch-browser: its merge job must read its own legs and
# none of the full lane's, or it would name a red leg of that lane as its own.
fixture 0
drive jobs "$work/out.json" dispatch-browser-pr
check "exits 0" "$(is "$status" 0)"
check "writes that caller's three current jobs and no other" "$(is "$(entries)" 3)"
check "its real job is the real leg" "$(is "$(field 19 .leg)" real)"
check "its stub job is the stub leg" "$(is "$(field 27 .leg)" stub)"
check "the retired embedder job is not kept" "$(is "$(field 28 .name)" "")"
check "with its own conclusion" "$(is "$(field 19 .conclusion)" failure)"
check "and the full lane's shard (1) is not in it" "$(is "$(jq -r '[.[] | select(.id == 11)] | length' "$work/out.json")" 0)"

echo "case: the alone leg is the bare job name, and a shard leg's number is read whole"
# The leg is read off the name after the caller's prefix, so both callers' jobs read the same
# leg: a leg absent from reading one's list is a leg whose missing blob nothing reports. The
# alone job runs its three projects on one machine and writes one blob report, so its leg is the
# word itself, with nothing read out of brackets (dispatch://INFRA-1109). A bare `alone` or
# `shard (1)`, with no caller's prefix, is no job of this lane, as `lint` is not, so the two
# unprefixed jobs here are dropped. A shard number is read whole, so a tenth leg is `shard-10`.
fixture 0
page "21|dispatch-browser / alone|success" "22|alone|success" \
  "23|dispatch-browser / shard (10)|success" "24|shard (1)|success" \
  "25|dispatch-browser-pr / alone|skipped" "26|dispatch-browser-pr / shard (2)|success"
drive jobs "$work/out.json" "$caller"
check "exits 0" "$(is "$status" 0)"
check "reads this caller's two jobs" "$(is "$(entries)" 2)"
check "the prefixed alone job is the alone leg" "$(is "$(field 21 .leg)" alone)"
check "a two-digit shard leg is read whole" "$(is "$(field 23 .leg)" shard-10)"
check "a job named under no caller is no leg of this lane" \
  "$(is "$(jq -r '[.[] | select(.id == 22 or .id == 24)] | length' "$work/out.json")" 0)"
drive jobs "$work/out.json" dispatch-browser-pr
check "the other caller's read exits 0" "$(is "$status" 0)"
check "and reads its own two jobs" "$(is "$(entries)" 2)"
check "under that caller's prefix the alone job is the alone leg too" "$(is "$(field 25 .leg)" alone)"
check "and its shard job is its own leg" "$(is "$(field 26 .leg)" shard-2)"

echo "case: every attempt refused with HTTP 502"
fixture 3 "HTTP 502"
retry_seconds=1
drive jobs "$work/out.json" "$caller"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "tried three times" "$(is "$(calls)" 3)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the endpoint it read" \
  "$(contains "$(error_line)" 'GET repos/o/r/actions/runs/41/attempts/4/jobs?per_page=30,')"
check "the status gh last printed" "$(contains "$(error_line)" 'HTTP 502')"
check "the ladder's attempts and this run attempt" \
  "$(contains "$(error_line)" '3 attempts, all on run attempt 4')"
check "and that this error is a failed read and not a failed row" \
  "$(contains "$(error_line)" 'This error is a failed read of the jobs API, not a failed browser row\. Re-running this job reads the list again\.$')"
check "nor that no row failed, which this job did not read" \
  "$(is "$(contains "$(error_line)" 'no row of the suite failed')" false)"
check "every attempt's gh output still reaches the log" \
  "$(is "$(grep -c '^HTTP 502$' <<<"$out" || true)" 3)"
check "and the ladder waited 1 s and then 2 s" "$(is "$(sleeps)" "1|2")"

echo "case: a refusal gh printed no HTTP status for"
# A resolution failure prints no status line, and a line that named one anyway would send a
# reader looking for a 502 that never happened.
fixture 3 "could not resolve host: api.github.com"
drive jobs "$work/out.json" "$caller"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "which names the exit status and says gh printed none" \
  "$(contains "$(error_line)" 'exited 1, printing no HTTP status')"
check "and names no status of its own" \
  "$(is "$(contains "$(error_line)" 'HTTP [0-9][0-9][0-9]')" false)"
check "while gh's own words reach the log" \
  "$(contains "$out" '^could not resolve host: api.github.com$')"

echo "case: an earlier attempt answered a status and the last attempt did not"
# The ladder's three attempts need not fail for one reason: during the 2026-11-08 incident the
# jobs API answered 502 and GitHub's hosts were also refusing connections, and a refused
# connection prints no status at all. The line reports the attempt that decided the failure,
# which is the last one, so it must not hand that attempt a status an earlier attempt
# answered: a reader sent to GitHub's API health by a 502 that did not decide the failure is
# exactly the mis-reading this lane's messages exist to end.
fixture 3 "error connecting to api.github.com"
printf '%s\n' 'gh: Server Error (HTTP 502)' > "$work/stderr.0"
drive jobs "$work/out.json" "$caller"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "tried three times" "$(is "$(calls)" 3)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "does not hand the deciding attempt a status only an earlier one answered" \
  "$(is "$(contains "$(error_line)" 'decided the failure, answered HTTP 502')" false)"
check "says that attempt exited with no status of its own" \
  "$(contains "$(error_line)" 'decided the failure, exited 1, printing no HTTP status')"
check "and names the earlier attempt that was answered one, with its own attempt number" \
  "$(contains "$(error_line)" 'where attempt 1 answered HTTP 502')"
check "while every attempt's own words still reach the log" \
  "$(contains "$out" '^gh: Server Error (HTTP 502)$')"
check "the later attempts' too" \
  "$(is "$(grep -c '^error connecting to api.github.com$' <<<"$out" || true)" 2)"

echo "case: an attempt the 30 s bound ended is reported as a timeout and not as a bare refusal"
# The first of the three cases that read the script's own `timeout 30` as the bound firing,
# through the stand-in timeout they opt into per call. When this case was written nothing else
# in this file held the bound: `timeout 30` taken out of a scratch copy of the script on
# 2026-11-08 failed no check. The two cases that stage a killed wrapper now fail without it
# as well, the kill they stage reaching the wrapper only through the `timeout` process between
# gh and the wrapper, and with `timeout 30` taken out of a scratch copy on 2026-11-08 32 checks
# fail, in this case and the five after it, the two killed-wrapper cases among them. The
# stand-in answers 124 at once, so this case costs none of the 90 s a hanging gh under the real
# bound would, and it bites either way the bound could go - dropped, and the stand-in is never
# called at all; changed, and the 30 it recorded is not a 30. The gh here would answer, so the
# bound is the only thing that can fail this drive.
#
# All three calls are asked for, so every attempt ends at the bound and the line has one thing
# to say about all three. That is why this case alone holds nothing about which attempt the
# line read: with every attempt the same, a script reading `status.1`, or reading a 124 on any
# attempt as the deciding one, answers the same words. The cases after this one are where
# the attempts differ.
fixture 0
touch "$work/stub-timeout.0" "$work/stub-timeout.1" "$work/stub-timeout.2"
drive jobs "$work/out.json" "$caller"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "bounds each of the ladder's three attempts" "$(is "$(timeout_calls)" 3)"
check "with 30 seconds" "$(is "$(cut -d' ' -f1 <<<"$(timeout_args)")" 30)"
check "and gh as the command it wraps" "$(is "$(cut -d' ' -f2 <<<"$(timeout_args)")" gh)"
check "which is this run's attempt read itself" \
  "$(contains "$(timeout_args)" '^30 gh api --paginate --slurp repos/o/r/actions/runs/41/')"
check "so no gh call is made outside the bound" "$(is "$(calls)" 0)"
check "and the ladder's two waits are this case's 0 s" "$(is "$(sleeps)" "0|0")"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the bound that ended the attempt that decided the failure" \
  "$(contains "$(error_line)" 'attempt 3, which decided the failure, stopped at its 30 s bound')"
check "and not the words an exit with no status of its own gets" \
  "$(is "$(contains "$(error_line)" 'printing no HTTP status')" false)"
check "and says nothing of the earlier attempts, which ended the same way" \
  "$(is "$(contains "$(error_line)" 'where attempt')" false)"

echo "case: the bound ended the first two attempts and the third answered a status"
# What the line reports is what the attempt that decided the failure did, and that attempt is
# the last one the ladder made. Here the first two calls are bounded and gh answers the third
# with HTTP 502, so a script that read the deciding attempt's words from `status.1`, or that
# took a 124 on any attempt as the one that decided, would call this read a timeout it was
# not. Both mutations were made of a scratch copy of the script on 2026-11-08: the copy
# reading `status.1` fails 8 checks and the copy taking a 124 on any attempt as the deciding
# one fails 3, all of them in this case. With this case and the one after it taken out of the
# file, measured the same day, the second copy fails nothing that is left, while the first is
# still caught in two later cases, failing 4 checks: two in the 127 case and two in the first
# killed-wrapper case.
fixture 3 "HTTP 502"
touch "$work/stub-timeout.0" "$work/stub-timeout.1"
drive jobs "$work/out.json" "$caller"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "bounds each of the ladder's three attempts" "$(is "$(timeout_calls)" 3)"
check "and gh runs only on the attempt the bound did not end" "$(is "$(calls)" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the status the deciding attempt was answered" \
  "$(contains "$(error_line)" 'attempt 3, which decided the failure, answered HTTP 502')"
check "and not saying that attempt stopped at the bound" \
  "$(is "$(contains "$(error_line)" 'decided the failure, stopped at its 30 s bound')" false)"
check "while the two that did stop at it are named, each with its own attempt number" \
  "$(contains "$(error_line)" \
    'where attempt 1 stopped at its 30 s bound and attempt 2 stopped at its 30 s bound')"

echo "case: the first two attempts answered a status and the bound ended the third"
# The reverse, and a shape the bound exists for rather than one this lane has measured: the
# jobs API answering 502 while a later attempt is left hanging until the bound ends it. The
# 2026-11-08 incident was not that shape - its three attempts failed on 502s in 17, 19 and 18
# seconds, which the script's own comment reads as answered rather than left hanging. The
# attempt the bound ended is the one that decided the failure, so the line must not hand it the
# status the earlier two were answered, and must still say what those two did, since they did
# something else.
fixture 3 "HTTP 502"
touch "$work/stub-timeout.2"
drive jobs "$work/out.json" "$caller"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "bounds each of the ladder's three attempts" "$(is "$(timeout_calls)" 3)"
check "and gh runs on the two the bound did not end" "$(is "$(calls)" 2)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the bound for the attempt that decided the failure" \
  "$(contains "$(error_line)" 'attempt 3, which decided the failure, stopped at its 30 s bound')"
check "and not handing that attempt the status only the earlier two were answered" \
  "$(is "$(contains "$(error_line)" 'decided the failure, answered HTTP 502')" false)"
check "while those two are named with it, each with its own attempt number" \
  "$(contains "$(error_line)" \
    'where attempt 1 answered HTTP 502 and attempt 2 answered HTTP 502')"

echo "case: the deciding attempt exited a status that is neither 1 nor 124"
# The third branch names the status it read rather than calling the attempt refused, and the
# statuses that are not 1 are the reason: `timeout` exits 127 where it could not run gh at all
# (`timeout: failed to run command 'gh'`) and the attempt exits 137 for a gh that was killed,
# and nothing refused anything in either. Every other case that reaches that branch exits 1,
# so without this one a script printing a fixed `exited 1, printing no HTTP status` passes the
# whole file: measured 2026-11-08, that copy fails 2 checks, both in this case, and with this
# case taken out of the file it fails none, which is what the file as it stands fails without
# this case too. The stand-in timeout answers with the status the case wrote in its file, 127
# here, and with 124 where a case wrote no status at all.
fixture 3 "HTTP 502"
printf '%s\n' 127 > "$work/stub-timeout.2"
drive jobs "$work/out.json" "$caller"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "bounds each of the ladder's three attempts" "$(is "$(timeout_calls)" 3)"
check "and gh runs on the two the stand-in did not answer for" "$(is "$(calls)" 2)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the status the deciding attempt exited" \
  "$(contains "$(error_line)" 'decided the failure, exited 127, printing no HTTP status')"
check "and not the 1 that the attempts gh refused exited" \
  "$(is "$(contains "$(error_line)" 'decided the failure, exited 1,')" false)"
check "nor the bound, which no call in this drive was answered with" \
  "$(is "$(contains "$(error_line)" 'stopped at its 30 s bound')" false)"
check "while the two that were answered a status are named with their own numbers" \
  "$(contains "$(error_line)" \
    'where attempt 1 answered HTTP 502 and attempt 2 answered HTTP 502')"

echo "case: an attempt whose wrapper was killed leaves no exit status on record"
# describe_attempt's fourth branch, the one shape no stand-in gh can reach by answering
# differently: the wrapper writes `status.<n>` after gh returns, so leaving that file unwritten
# needs a gh that does not return. The stand-in kills the wrapper instead, which is the shape
# the branch exists for - the attempt's redirection has made `stderr.<n>`, gh has put nothing
# on it, and nothing has written `status.<n>`.
#
# The lock is not an empty one: with the fourth branch removed, so that the third branch
# prints the empty status it holds, this case reads `exited , printing no HTTP status` and the
# suite fails 2 checks, both in this case, measured 2026-11-08.
#
# The kill is held as well, because a kill that does not reach the wrapper leaves a case that
# holds nothing: the stand-in then says it could not stage the case and exits 70, the wrapper
# writes that 70 to `status.3`, and the line reads `exited 70, printing no HTTP status`. With
# the kill pointed at pid 999999 in a scratch copy on 2026-11-08, both cases that stage a
# killed wrapper go red - three checks here and two in the one after it, 5 in all.
#
# The attempt is still counted, because the wrapper counts it before it runs gh, so the line
# says three attempts and names the two that answered.
fixture 3 "HTTP 502"
touch "$work/kill-wrapper.2"
drive jobs "$work/out.json" "$caller"
check "the harness staged the kill" \
  "$(is "$(contains "$out" 'could not stage a killed wrapper')" false)"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "tried three times" "$(is "$(calls)" 3)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "says the deciding attempt left no exit status on record and printed no HTTP status" \
  "$(contains "$(error_line)" \
    'attempt 3, which decided the failure, left no exit status on record, printing no HTTP status')"
check "and names no exit status for it, there being none to name" \
  "$(is "$(contains "$(error_line)" 'decided the failure, exited')" false)"
check "nor hands it a status an earlier attempt was answered" \
  "$(is "$(contains "$(error_line)" 'decided the failure, answered HTTP 502')" false)"
check "while the two that were answered are named, each with its own attempt number" \
  "$(contains "$(error_line)" \
    'where attempt 1 answered HTTP 502 and attempt 2 answered HTTP 502')"
check "and the attempt the wrapper never finished is still counted" \
  "$(contains "$(error_line)" 'the ladder made 3 attempts')"

echo "case: a killed wrapper whose gh had printed a status before it died"
# The other half of that shape. The wrapper dies with no `status.<n>` written either way, but
# here gh had already put `HTTP 503` on the stderr the redirection made, so that attempt is
# described from its own stderr and the fourth branch is not what it gets. This case is what
# holds the order of those two reads: measured 2026-11-08, a copy of describe_attempt asking
# whether the status file is missing before it greps the stderr calls this attempt one that
# left no exit status on record, and fails 2 checks, both in this case.
#
# The order is held only while the kill reaches the wrapper. A kill that failed leaves the
# `HTTP 503` in `stderr.3` and a 70 in `status.3`, and the line still reads `answered HTTP
# 503`, so this case holds the staging twice over: that the stand-in printed none of its own
# words, and that nothing echoed `stderr.3` to the log, which the wrapper does after gh
# returns and a killed wrapper never reaches.
fixture 3 "HTTP 502"
printf '%s\n' 'gh: Server Error (HTTP 503)' > "$work/stderr.2"
touch "$work/kill-wrapper.2"
drive jobs "$work/out.json" "$caller"
check "the harness staged the kill" \
  "$(is "$(contains "$out" 'could not stage a killed wrapper')" false)"
check "and the killed wrapper never echoed the stderr its gh had written" \
  "$(is "$(grep -c '^gh: Server Error (HTTP 503)$' <<<"$out" || true)" 0)"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "tried three times" "$(is "$(calls)" 3)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "reads the deciding attempt from the stderr its gh had written" \
  "$(contains "$(error_line)" 'attempt 3, which decided the failure, answered HTTP 503')"
check "rather than from the status file nothing wrote" \
  "$(is "$(contains "$(error_line)" 'decided the failure, left no exit status')" false)"
check "while the two earlier attempts keep the status they were answered" \
  "$(contains "$(error_line)" \
    'where attempt 1 answered HTTP 502 and attempt 2 answered HTTP 502')"
check "and the attempt the wrapper never finished is still counted" \
  "$(contains "$(error_line)" 'the ladder made 3 attempts')"

echo "case: every attempt answered the first page and was refused the second"
# A read of two pages whose second GitHub refuses: gh exits 1 at that page, having printed the
# first and then the error's body in the second's place, so each refused attempt leaves a JSON
# array on its stdout whose first element is a whole page of this attempt's jobs. No attempt took
# the read whole, so it is refused as a read refused on its first page is, and that first page
# reaches no file and no line of the log. A script that read the first page alone would never
# ask for the second and would pass here, which is what the first check holds.
fixture 3 "gh: Server Error (HTTP 502)"
two_page_attempt
printf '2\n' > "$work/refused-page"
drive jobs "$work/out.json" "$caller"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "tried three times" "$(is "$(calls)" 3)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the route of thirty to a page" \
  "$(contains "$(error_line)" 'GET repos/o/r/actions/runs/41/attempts/4/jobs?per_page=30,')"
check "and the status the second page was answered" \
  "$(contains "$(error_line)" 'attempt 3, which decided the failure, answered HTTP 502')"
check "while gh's words for each refused page still reach the log" \
  "$(is "$(grep -c '^gh: Server Error (HTTP 502)$' <<<"$out" || true)" 3)"
check "and the first page each attempt printed reaches none of it" \
  "$(is "$(contains "$out" 'dispatch-browser / binaries')" false)"

echo "case: an attempt refused on the second page, and the next attempt answered both"
# The ladder's next attempt reads the whole list again, and it is that attempt's answer the file
# holds: the first page the refused attempt printed is no part of it, so every job is written
# once, and reading five sums the jobs of both pages.
fixture 1 "gh: Server Error (HTTP 502)"
two_page_attempt
printf '2\n' > "$work/refused-page"
drive jobs "$work/out.json" "$caller"
check "exits 0" "$(is "$status" 0)"
check "tried twice" "$(is "$(calls)" 2)"
check "writes the full lane's seven jobs" "$(is "$(entries)" 7)"
check "each of them once" "$(is "$(jq -r '[.[].id] | unique | length' "$work/out.json")" 7)"
check "prints no ::error:: line" "$(is "$(errors)" 0)"
drive machine-time "$work/out.json" full
check "and reading five sums the jobs of both pages" \
  "$(contains "$(notice_line)" 'the full lane cost 1750 s over 5 concluded jobs')"

echo "case: an answer gh took whole that holds no jobs list writes no out-file"
# gh exits 0 on an answer it took whole, whatever that answer holds, and the filter is then what
# refuses it. That refusal must leave no file behind: reading three reads an empty jobs file as
# every leg of this workflow having concluded success, and reading five reads one as a lane with
# no concluded job, where a missing file is named as missing by both. The answer here is a page
# that is no JSON at all, such as a proxy in front of GitHub serves with a 200.
fixture 0
printf '%s\n' '<html><body>Sign in to continue</body></html>' > "$work/body"
drive jobs "$work/out.json" "$caller"
check "fails the step" "$([ "$status" -ne 0 ] && echo true || echo false)"
check "after one call, an answer gh exited 0 on being no refused attempt" "$(is "$(calls)" 1)"
check "and writes no out-file" "$(is "$(wrote_out_file)" no)"
drive machine-time "$work/out.json" full
check "so reading five names the jobs file as missing" \
  "$(contains "$(error_line)" "$work/out.json and that file is not there")"

echo "case: refused once, answered on the second attempt"
fixture 1
retry_seconds=1
drive jobs "$work/out.json" "$caller"
check "exits 0" "$(is "$status" 0)"
check "tried twice" "$(is "$(calls)" 2)"
check "writes the attempt that answered" "$(is "$(entries)" 7)"
check "with its legs" "$(is "$(field 13 .leg)" alone)"
check "prints no ::error:: line" "$(is "$(errors)" 0)"
check "and waited the ladder's first second" "$(is "$(sleeps)" 1)"

echo "case: the negative control, a gh that answers at once"
# The two cases above read the waits the ladder asked of `sleep`, and this one holds that a gh
# answering at once makes the drive make none of the calls TIMED_WAITS lists, as timed_waits
# counts them. A wait the stand-in does not record - a sleep binary by its absolute path,
# `read -t`, a timed futex or futex_wait, semtimedop, mq_timedreceive, io_uring_enter with a
# timeout, a timerfd, alarm and pause, flock, an F_OFD_SETLKW lock wait, SO_RCVTIMEO, a close that
# waits out SO_LINGER, a terminal read that waits out a VTIME set by TCSETS, TCSETA or TCSETS2,
# any of them after the answer, of any length - makes one, so the drive runs under strace and
# timed_waits must find none. The waits the comment above TIMED_WAITS names as not seen pass. The
# knob is 100000 and the real `timeout 120` still bounds the drive, so a wait the size of the
# knob also ends it with 124.
fixture 0
set +e
out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
  GITHUB_RUN_ATTEMPT=$run_attempt GH_RETRY_SECONDS=100000 "$real_strace" -f -qq -e signal=none \
  -e trace="$TIMED_WAITS" -o "$work/waits" "$real_timeout" 120 "$read_script" jobs "$work/out.json" "$caller" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "calls gh once" "$(is "$(calls)" 1)"
check "and asks sleep for no wait" "$(is "$(sleeps)" "")"
waits=$(timed_waits "$work/waits")
check "and makes none of the calls the trace lists" "$(is "$(grep -c . <<<"$waits" || true)" 0)"
[ -z "$waits" ] || sed 's/^/    traced: /' <<<"$waits"

echo "case: timed_waits reads a trace line whatever the padding of its pid and the ABI's names"
# The parser of the control's trace, on lines in the shape strace 6.8 writes. The pid-4 and pid-5
# lines were written in a fresh PID namespace on this file's x86_64 box, where strace pads a
# one-digit pid with blanks (`4     futex(`), the *_time64, _newselect, i386 multiplexer and
# fcntl64 F_SETLKW64 and F_SETFD ones by a 32-bit helper; the pid-2149, 2156, 2164, 2178, 2224,
# 2229, 2234 and 2239 ones on GitHub's ubuntu-24.04-arm runner; the seven-digit ones on the x86_64
# box outside a namespace; all on 2026-11-21. The others are those lines with the pid re-padded or
# under the name i386 gives the call (fcntl64 with F_SETLKW), or a lock's NULL-timeout wait, a
# wake, a setsockopt of another option and a resumed call built in the same shape. Each `yes`
# line counts, each `no` line does not.
while IFS='|' read -r want line; do
  printf '%s\n' "$line" > "$work/trace-line"
  got=no
  [ -z "$(timed_waits "$work/trace-line")" ] || got=yes
  check "$want: $line" "$(is "$got" "$want")"
done <<'TRACE'
yes|4     futex(0x7ffc5cf8d2dc, FUTEX_WAIT_PRIVATE, 0, {tv_sec=1, tv_nsec=0}) = -1 ETIMEDOUT (Connection timed out)
yes|3278182 futex(0x7ffc5cf8d2dc, FUTEX_WAIT_PRIVATE, 0, {tv_sec=1, tv_nsec=0}) = -1 ETIMEDOUT (Connection timed out)
yes|4     futex(0x7ffc5cf8d2dc, FUTEX_WAIT_PRIVATE, 0, {tv_sec=1, tv_nsec=0} <unfinished ...>
yes|5     clock_nanosleep(CLOCK_REALTIME, 0, {tv_sec=0, tv_nsec=10000000}, 0x7ffeea4c2ac0) = 0
yes|3662081 clock_nanosleep(CLOCK_REALTIME, 0, {tv_sec=1, tv_nsec=0}, 0x7fff1f4bea20) = 0
yes|27    pselect6(1, [0], NULL, NULL, {tv_sec=0, tv_nsec=999984000}, {sigmask=[CHLD], sigsetsize=8}) = 0 (Timeout)
yes|4     futex_waitv([{val=0, uaddr=0x7fff242ac83c, flags=FUTEX2_SIZE_U32|FUTEX2_PRIVATE}], 1, 0, {tv_sec=1403474, tv_nsec=902920133}, CLOCK_MONOTONIC) = -1 ETIMEDOUT (Connection timed out)
yes|4     clock_nanosleep_time64(CLOCK_MONOTONIC, 0, {tv_sec=0, tv_nsec=10000000}, NULL) = 0
yes|4     pselect6_time64(0, NULL, NULL, NULL, {tv_sec=0, tv_nsec=10000000}, NULL) = 0 (Timeout)
yes|4     ppoll_time64(NULL, 0, {tv_sec=0, tv_nsec=0}, NULL, 8) = 0 (Timeout)
yes|4     rt_sigtimedwait_time64([USR1], NULL, {tv_sec=0, tv_nsec=0}, 8) = -1 EAGAIN (Resource temporarily unavailable)
yes|4     futex_time64(0x804b010, FUTEX_WAIT_PRIVATE, 0, {tv_sec=1, tv_nsec=0}) = -1 ETIMEDOUT (Connection timed out)
yes|3250631 futex_time64(0x804b010, FUTEX_WAIT_PRIVATE, 0, {tv_sec=1, tv_nsec=0}) = -1 ETIMEDOUT (Connection timed out)
yes|4     _newselect(0, NULL, NULL, NULL, {tv_sec=0, tv_usec=10000}) = 0 (Timeout)
yes|4     semtimedop(21, [{sem_num=0, sem_op=-1, sem_flg=0}], 1, {tv_sec=1, tv_nsec=0}) = -1 EAGAIN (Resource temporarily unavailable)
yes|4     mq_timedreceive(3, 0x7ffe9d83f428, 8, NULL, {tv_sec=1795260801, tv_nsec=515330836}) = -1 ETIMEDOUT (Connection timed out)
yes|4     timerfd_settime(3, 0, {it_interval={tv_sec=0, tv_nsec=0}, it_value={tv_sec=1, tv_nsec=0}}, NULL) = 0
yes|4     alarm(1)                          = 0
yes|4     pause()                           = ? ERESTARTNOHAND (To be restarted if no handler)
yes|4     flock(4, LOCK_EX)                 = ? ERESTARTSYS (To be restarted if SA_RESTART is set)
yes|4     setsockopt(3, SOL_SOCKET, SO_RCVTIMEO_OLD, "\1\0\0\0\0\0\0\0\0\0\0\0\0\0\0\0", 16) = 0
yes|4     setsockopt(3, SOL_SOCKET, SO_RCVTIMEO_OLD, "\1\0\0\0\0\0\0\0", 8) = 0
yes|4     semtimedop(47, [{sem_num=0, sem_op=-1, sem_flg=0}], 1, {tv_sec=0, tv_nsec=10000000}) = -1 EAGAIN (Resource temporarily unavailable)
yes|4     io_uring_enter(3, 0, 1, IORING_ENTER_GETEVENTS|IORING_ENTER_EXT_ARG, 0x7ffd7597e5e0, 24) = -1 ETIME (Timer expired)
yes|2224  io_uring_enter(3, 0, 1, IORING_ENTER_GETEVENTS|IORING_ENTER_EXT_ARG, 0xffffeaa74640, 24) = -1 ETIME (Timer expired)
yes|4     futex_wait(0x7ffd40f58180, 0, FUTEX_BITSET_MATCH_ANY, FUTEX2_SIZE_U32|FUTEX2_PRIVATE, {tv_sec=1414019, tv_nsec=781690059}, CLOCK_MONOTONIC) = -1 ETIMEDOUT (Connection timed out)
yes|2229  futex_wait(0xffffcd665e70, 0, FUTEX_BITSET_MATCH_ANY, FUTEX2_SIZE_U32|FUTEX2_PRIVATE, {tv_sec=774, tv_nsec=700571771}, CLOCK_MONOTONIC) = -1 ETIMEDOUT (Connection timed out)
yes|4     fcntl(4, F_OFD_SETLKW, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=0, l_len=0}) = ? ERESTARTSYS (To be restarted if SA_RESTART is set)
yes|2239  fcntl(4, F_OFD_SETLKW, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=0, l_len=0}) = ? ERESTARTSYS (To be restarted if SA_RESTART is set)
yes|4     fcntl64(4, F_SETLKW, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=0, l_len=0}) = ? ERESTARTSYS (To be restarted if SA_RESTART is set)
yes|4     sched_setattr(0, {size=56, sched_policy=SCHED_DEADLINE, sched_flags=0, sched_nice=0, sched_priority=0, sched_runtime=10000000, sched_deadline=1000000000, sched_period=1000000000, sched_util_min=0, sched_util_max=0}, 0) = -1 EPERM (Operation not permitted)
yes|4     setsockopt(4, SOL_SOCKET, SO_LINGER, {l_onoff=1, l_linger=1}, 8) = 0
yes|4     setsockopt(3, SOL_TCP, TCP_USER_TIMEOUT, [1000], 4) = 0
yes|4     setsockopt(3, SOL_SOCKET, SO_BUSY_POLL, [1000], 4) = 0
yes|4     setsockopt(3, SOL_SOCKET, SO_RCVBUF, [1024], 4) = 0
yes|4     ioctl(4, TCSETS, {c_iflag=, c_oflag=NL0|CR0|TAB0|BS0|VT0|FF0|ONLCR, c_cflag=B38400|CS8|CREAD, c_lflag=ECHOE|ECHOK|ECHOCTL|ECHOKE, ...}) = 0
yes|4     ioctl(4, TCSETA, {c_iflag=, c_oflag=NL0|CR0|TAB0|BS0|VT0|FF0|, c_cflag=B38400|CS8|CREAD, c_lflag=, ...}) = 0
yes|4     ioctl(4, TCSETAW, {c_iflag=, c_oflag=NL0|CR0|TAB0|BS0|VT0|FF0|, c_cflag=B38400|CS8|CREAD, c_lflag=, ...}) = 0
yes|4     ioctl(4, TCSETS2, {c_iflag=, c_oflag=NL0|CR0|TAB0|BS0|VT0|FF0|, c_cflag=B38400|CS8|CREAD, c_lflag=, ...}) = 0
yes|4     ioctl(4, TCSBRKP, 10)             = 0
yes|4     ioctl(4, TIOCSSERIAL)             = -1 ENOTTY (Inappropriate ioctl for device)
yes|2178  ioctl(4, TCSETA, {c_iflag=, c_oflag=NL0|CR0|TAB0|BS0|VT0|FF0|, c_cflag=B38400|CS8|CREAD, c_lflag=, ...}) = 0
yes|2156  ioctl(4, TCSETAW, {c_iflag=, c_oflag=NL0|CR0|TAB0|BS0|VT0|FF0|, c_cflag=B38400|CS8|CREAD, c_lflag=, ...}) = 0
yes|2149  ioctl(4, TCSETS2, {c_iflag=, c_oflag=NL0|CR0|TAB0|BS0|VT0|FF0|, c_cflag=B38400|CS8|CREAD, c_lflag=, ...}) = 0
yes|2164  ioctl(4, TCSBRKP, 10)             = 0
yes|4     fcntl64(3, F_SETLKW64, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=0, l_len=0}) = 0
yes|4     fcntl(3, F_SETLEASE, F_RDLCK)     = 0
no|4     futex(0x7ffc5cf8d2dc, FUTEX_WAIT_PRIVATE, 2, NULL) = 0
no|2256817 futex(0x76abf580572c, FUTEX_WAKE_PRIVATE, 2147483647) = 0
no|4     futex(0x76abf580572c, FUTEX_WAKE_PRIVATE, 2147483647) = 0
no|4     futex_time64(0x804b010, FUTEX_WAIT_PRIVATE, 2, NULL) = 0
no|4     futex_time64(0x804b010, FUTEX_WAKE_PRIVATE, 2147483647) = 0
no|4     ioctl(4, TCGETS, {c_iflag=ICRNL|IXON, c_oflag=NL0|CR0|TAB0|BS0|VT0|FF0|OPOST|ONLCR, c_cflag=B38400|CS8|CREAD, c_lflag=ISIG|ICANON|ECHO|ECHOE|ECHOK|IEXTEN|ECHOCTL|ECHOKE, ...}) = 0
no|4     ioctl(3, TIOCGPTN, [19])          = 0
no|4     ioctl(4, TCGETA, {c_iflag=ICRNL|IXON, c_oflag=NL0|CR0|TAB0|BS0|VT0|FF0|OPOST|ONLCR, c_cflag=B38400|CS8|CREAD, c_lflag=ISIG|ICANON|ECHO|ECHOE|ECHOK|IEXTEN|ECHOCTL|ECHOKE, ...}) = 0
no|4     ioctl(4, TCGETS2, {c_iflag=ICRNL|IXON, c_oflag=NL0|CR0|TAB0|BS0|VT0|FF0|OPOST|ONLCR, c_cflag=B38400|CS8|CREAD, c_lflag=ISIG|ICANON|ECHO|ECHOE|ECHOK|IEXTEN|ECHOCTL|ECHOKE, ...}) = 0
no|4     fcntl64(4, F_SETFD, FD_CLOEXEC)   = 0
no|4     futex_wait(0x7ffe02ed1ba0, 1, FUTEX_BITSET_MATCH_ANY, FUTEX2_SIZE_U32|FUTEX2_PRIVATE, NULL, CLOCK_MONOTONIC) = -1 EAGAIN (Resource temporarily unavailable)
no|2234  futex_wait(0xffffc227b690, 1, FUTEX_BITSET_MATCH_ANY, FUTEX2_SIZE_U32|FUTEX2_PRIVATE, NULL, CLOCK_MONOTONIC) = -1 EAGAIN (Resource temporarily unavailable)
no|4     fcntl(3, F_OFD_SETLK, {l_type=F_WRLCK, l_whence=SEEK_SET, l_start=0, l_len=0}) = 0
no|4     fcntl(0, F_SETFD, FD_CLOEXEC)     = 0
no|4     <... futex resumed>) = -1 ETIMEDOUT (Connection timed out)
no|3662081 <... clock_nanosleep resumed>0x7fff1f4bea20) = 0
TRACE

# THE MACHINE-TIME CASES. The budgets are 3,146 s on the full lane and 846 s on the
# pull-request lane, which are the committed ceiling of 3,200 s and 900 s less the merge job's
# own worst case of 54 s (the script's own branch says what that figure is made of, and why
# that ceiling is no reading of what this issue delivers). Each case below
# drives the arithmetic to one side of a budget or the other: a budget nothing is ever refused
# by refuses nothing, which is the defect class this mode exists against (R12 of
# dispatch://INFRA-1109/spec).

echo "case: the lane's machine time is summed out of the file the jobs mode wrote"
# The two modes over one page, one after the other: the `jobs` mode writes the file and this
# mode reads it, so the fields one writes and the other reads cannot drift apart. The page is
# the happy one - binaries 150 s, shard (1) 600, shard (2) 700 (red), alone 200, visual 100
# (red), multiprocess skipped, merge still running, the caller's two jobs at 9,999 s each and
# the other caller's real and stub legs at 500 and 300 s.
fixture 0
drive jobs "$work/out.json" "$caller"
check "the jobs mode exits 0" "$(is "$status" 0)"
drive machine-time "$work/out.json" full
check "the full lane passes under its budget" "$(is "$status" 0)"
check "naming the seconds it summed and the budget they are held to" \
  "$(contains "$(notice_line)" \
    'the full lane cost 1750 s over 5 concluded jobs, this merge job apart, against a budget of 3146 s')"
check "which it says is the committed ceiling less this job's own worst case" \
  "$(contains "$(notice_line)" '3200 s less this job.s own worst case of 54 s')"
check "and prints no ::error:: line" "$(is "$(errors)" 0)"
# 1,750 s is binaries, both shards, alone and visual: a red leg cost its machine time like any
# other and is in the sum, while the skipped job (-1 s), the merge job (still running), the
# caller's two 9,999 s jobs and the other caller's 800 s of real and stub legs are not. Any job in
# or out moves the figure above.

echo "case: the same run on the pull-request lane is refused by the lower budget"
# The lane decides the budget and nothing else does: the same jobs, read as the lane whose
# bound is 900 s rather than 3,200, are refused. This is what holds the script to reading the
# lane it is given rather than one budget for every run.
drive machine-time "$work/out.json" pull-request
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the sum and the pull-request lane's budget" \
  "$(contains "$(error_line)" \
    'this pull-request lane has cost 1750 s of machine time over its 5 concluded jobs, this merge job apart, which is above the 846 s')"
check "and the committed ceiling for that lane with this job's own maximum taken off it" \
  "$(contains "$(error_line)" "the committed ceiling of 900 s for this lane, less this job's own worst case of 54 s")"
check "and says this is the suite costing more and not a failed row" \
  "$(contains "$(error_line)" 'costing more machine time than dispatch://INFRA-1109 left it costing, and not a failed browser row')"

echo "case: the other caller's merge job holds its own lane to that lane's budget"
# On a pull request labelled full-ci both callers run in one run, and each merge job's reading
# five sums the jobs its own `jobs` read wrote: dispatch-browser-pr's real and stub legs at 800 s,
# under the pull-request lane's 846 s, with none of dispatch-browser's 1,750 s.
fixture 0
drive jobs "$work/out.json" dispatch-browser-pr
check "the jobs mode exits 0" "$(is "$status" 0)"
drive machine-time "$work/out.json" pull-request
check "the pull-request lane passes under its budget" "$(is "$status" 0)"
check "summing that caller's two concluded jobs against the pull-request lane's budget" \
  "$(contains "$(notice_line)" \
    'the pull-request lane cost 800 s over 2 concluded jobs, this merge job apart, against a budget of 846 s')"
check "and prints no ::error:: line" "$(is "$(errors)" 0)"

echo "case: an attempt of two pages is read whole, and reading five sums the jobs of both"
# The attempt's 40 jobs come over two pages of thirty, every lane's jobs on both, and each lane's
# merge job must read the jobs of both: the full lane's binaries job is on the first page and its
# legs on the second, the pull-request lane's real leg on the first and its stub leg on the
# second, the references lane's two shards one on each. The stand-in answers a call that does not
# follow the pages with the first page alone, as gh does, so a read of one page writes the full
# lane's binaries job and none of its legs, and reading five sums 150 s over 1 job rather than
# 1,750 s over 5. A call that drops `--paginate` but keeps `--slurp` is refused, in gh's words.
fixture 0
two_page_attempt
drive jobs "$work/out.json" "$caller"
check "the jobs mode exits 0" "$(is "$status" 0)"
check "in one call, which read both pages" "$(is "$(calls)" 1)"
check "writes the full lane's seven jobs" "$(is "$(entries)" 7)"
check "its binaries job, from the first page" \
  "$(is "$(field 10 .name)" 'dispatch-browser / binaries')"
check "and its two shard legs, from the second" \
  "$(is "$(field 11 .leg) $(field 12 .leg)" 'shard-1 shard-2')"
check "and none of ci.yaml's own jobs" \
  "$(is "$(jq -r '[.[] | select(.id > 100)] | length' "$work/out.json")" 0)"
drive machine-time "$work/out.json" full
check "the full lane passes under its budget" "$(is "$status" 0)"
check "summing the jobs of both pages" \
  "$(contains "$(notice_line)" \
    'the full lane cost 1750 s over 5 concluded jobs, this merge job apart, against a budget of 3146 s')"
drive jobs "$work/out.json" dispatch-browser-pr
check "the pull-request lane's read exits 0" "$(is "$status" 0)"
check "writing its real leg from the first page and its stub leg from the second" \
  "$(is "$(field 19 .leg) $(field 27 .leg)" 'real stub')"
drive machine-time "$work/out.json" pull-request
check "and that lane's reading five sums both" \
  "$(contains "$(notice_line)" \
    'the pull-request lane cost 800 s over 2 concluded jobs, this merge job apart, against a budget of 846 s')"
drive jobs "$work/out.json" dispatch-references
check "the references lane's read exits 0" "$(is "$status" 0)"
check "writing its three jobs" "$(is "$(entries)" 3)"
check "its first shard leg from the first page and its second from the second" \
  "$(is "$(field 40 .leg) $(field 41 .leg)" 'shard-1 shard-2')"

echo "case: each budget bites at its own second"
# The one case that would catch a budget written one second out, or a `-ge` where the rule is
# `-gt`: the bound itself passes and one second more does not, on each lane.
fixture 0
lane_jobs "dispatch-browser / shard (1)|success|3146"
drive machine-time "$work/jobs.json" full
check "the full lane passes at 3,146 s exactly" "$(is "$status" 0)"
lane_jobs "dispatch-browser / shard (1)|success|3147"
drive machine-time "$work/jobs.json" full
check "and is refused at 3,147" "$(is "$status" 1)"
check "naming the second it was over by" \
  "$(contains "$(error_line)" 'cost 3147 s of machine time over its 1 concluded jobs')"
lane_jobs "dispatch-browser / shard (1)|success|846"
drive machine-time "$work/jobs.json" pull-request
check "the pull-request lane passes at 846 s exactly" "$(is "$status" 0)"
lane_jobs "dispatch-browser / shard (1)|success|847"
drive machine-time "$work/jobs.json" pull-request
check "and is refused at 847" "$(is "$status" 1)"

echo "case: this merge job's own seconds are not in the sum"
# The reason the budget is the committed ceiling less this job's maximum rather than the ceiling
# itself: the job doing the reading cannot see its own duration, so it must not be in the sum
# even where the API has one for it - a merge job of an earlier attempt, carried forward with a
# `completed_at` of its own, would otherwise be counted in the lane it is reporting on.
fixture 0
lane_jobs "dispatch-browser / shard (1)|success|10" "dispatch-browser / merge|success|5000"
drive machine-time "$work/jobs.json" full
check "passes, the 5,000 s merge job being none of this lane's sum" "$(is "$status" 0)"
check "and counts the one job that is left" \
  "$(contains "$(notice_line)" 'cost 10 s over 1 concluded jobs')"

echo "case: a skipped job and a job still running are no part of the machine time"
# A skipped job costs nothing and the API gives it a span of -1 s (run 43002633713's alone job,
# read 2026-11-11), so counting it would take a second off the lane's cost; a job with no
# `completed_at` has no duration to count at all. Both are left out, and the one job that ran
# is the whole sum.
fixture 0
lane_jobs "dispatch-browser / binaries|success|100" \
  "dispatch-browser / alone|skipped|-1" \
  "dispatch-browser / shard (2)|null"
drive machine-time "$work/jobs.json" full
check "passes" "$(is "$status" 0)"
check "summing the one job that concluded with a duration, with no second taken off it" \
  "$(contains "$(notice_line)" 'cost 100 s over 1 concluded jobs')"

echo "case: a lane whose jobs hold no duration refuses rather than passes"
# A budget that passes when it cannot read its number would report the cheapest run there has
# ever been on the day the reading breaks. An empty list and a list whose every job is skipped
# or still running are both that case.
fixture 0
lane_jobs
drive machine-time "$work/jobs.json" full
check "an empty list fails the step" "$(is "$status" 1)"
check "saying no job concluded with a duration" \
  "$(contains "$(error_line)" 'no job of this full lane has concluded with a duration')"
check "and that this is a refused reading and not a lane inside its budget" \
  "$(contains "$(error_line)" 'refused reading and not a lane inside its budget')"
lane_jobs "dispatch-browser / alone|skipped|-1" "dispatch-browser / merge|null"
drive machine-time "$work/jobs.json" pull-request
check "and so does a list holding only a skipped job and a running one" "$(is "$status" 1)"
check "naming that lane" \
  "$(contains "$(error_line)" 'no job of this pull-request lane has concluded with a duration')"

echo "case: a jobs file that is not there, or that jq cannot read, refuses"
# Reading one failing leaves no file at all, and this reading must not then report a lane
# inside its budget: its own line says the file is missing and that no budget was taken.
fixture 0
drive machine-time "$work/jobs.json" full
check "a missing file fails the step" "$(is "$status" 1)"
check "naming the file it reads the run's jobs from" \
  "$(contains "$(error_line)" "$work/jobs.json and that file is not there")"
check "and saying the lane was held to no budget" \
  "$(contains "$(error_line)" 'held to no budget')"
printf '%s\n' 'this is not a jobs list' > "$work/jobs.json"
drive machine-time "$work/jobs.json" full
check "a file jq cannot read fails the step too" "$(is "$status" 1)"
check "naming it as a list jq could not read" \
  "$(contains "$(error_line)" 'is not a list of this run.s jobs that jq could read')"

echo "case: a lane this workflow does not run is refused by name"
fixture 0
lane_jobs "dispatch-browser / shard (1)|success|10"
drive machine-time "$work/jobs.json" nightly
check "fails the step" "$(is "$status" 1)"
check "naming what it was given and what the lanes are" \
  "$(contains "$(error_line)" 'the lane this run is, which is pull-request or full, not .nightly.')"
check "and holds no lane to a budget it made up" \
  "$(is "$(contains "$out" 'budget')" false)"

echo "case: the summary a person reads names each job's seconds"
# The four readings before this one write their figures to the step summary as well as to the
# log, and this one does the same: the log line says what the lane cost and the table says
# which job cost it, which is the first thing a reader of a refusal wants. Outside Actions
# GITHUB_STEP_SUMMARY names no file and the script writes no table, which every case above
# drives.
fixture 0
summary_file="$work/summary.md"
lane_jobs "dispatch-browser / shard (1)|success|600" "dispatch-browser / binaries|success|150"
drive machine-time "$work/jobs.json" full
check "passes" "$(is "$status" 0)"
check "writes the reading's own heading" \
  "$(contains "$(summary_text)" '^### The lane.s machine time$')"
check "the longest job first" \
  "$(contains "$(summary_text)" '^| dispatch-browser / shard (1) | 600 |$')"
check "the next after it" \
  "$(contains "$(summary_text)" '^| dispatch-browser / binaries | 150 |$')"
check "and the sum against the budget, with the subtraction that made it" \
  "$(contains "$(summary_text)" \
    "cost 750 s against a budget of 3146 s, which is the committed ceiling of 3200 s less this job's own worst case of 54 s")"

echo "case: a machine-time call that names no lane, or too much, is a usage error"
fixture 0
lane_jobs "dispatch-browser / shard (1)|success|10"
drive machine-time "$work/jobs.json"
check "fails the step" "$(is "$status" 1)"
check "naming what it was given" \
  "$(contains "$out" "It was given: machine-time $work/jobs.json\$")"
check "and what the mode takes" \
  "$(contains "$out" 'dispatch-browser-run-reads.sh machine-time <jobs-file> <lane>')"
drive machine-time "$work/jobs.json" full extra
check "a fourth argument fails too" "$(is "$status" 1)"
check "naming all of what it was given" \
  "$(contains "$out" "It was given: machine-time $work/jobs.json full extra\$")"

# RATE_LIMITED: the jobs read refused for the repository's spent API budget, as run 44995550797's
# merge job was at 07:10:31Z on 2026-11-22 (`gh: API rate limit exceeded for installation (HTTP
# 403)`), where main's ladder made its three attempts in seconds and decided the read failed. The
# read goes through gh_read and so through gh-read.sh's gh_wait_reset. A paginated read leaves it
# no answer of its own to read, so it reads the spent budget and its reset from `gh api
# rate_limit`, not from gh's words, and waits for the reset when it falls at least 30 s before the
# job's limit. The stand-in `sleep` records the wait and returns at once, so no case waits it out.
rate_limited="gh: API rate limit exceeded for installation (HTTP 403)"

echo "case: RATE_LIMITED, the reset before the job's limit: the read waits for it and answers"
fixture 1 "$rate_limited"
reset=$(($(date +%s) + 20))
echo "$reset" > "$work/reset"
drive_limited JOB_TIMEOUT_MINUTES=10 -- jobs "$work/out.json" "$caller"
wait_asked=$(sleeps)
check "exits 0" "$(is "$status" 0)"
check "reads the reset from the rate_limit route once" "$(is "$(rate_calls)" 1)"
check "calls the jobs route twice: refused, then answered" "$(is "$(calls)" 2)"
check "writes the attempt that answered" "$(is "$(entries)" 7)"
check "logs the wait, naming the reset and the job's limit" \
  "$(contains "$out" "^::warning::the API budget is spent until $(utc "$reset"); waiting [0-9]* s for it, within this job's limit at ")"
check "asks sleep once, for the seconds to a second past the reset" \
  "$([[ $wait_asked =~ ^[0-9]+$ ]] && [ "$wait_asked" -ge 15 ] && [ "$wait_asked" -le 21 ] && echo true || echo false)"
check "prints no ::error:: line" "$(is "$(errors)" 0)"

echo "case: RATE_LIMITED, the reset past the job's limit: the read fails by name at once"
fixture 3 "$rate_limited"
echo "$(($(date +%s) + 3600))" > "$work/reset"
drive_limited JOB_TIMEOUT_MINUTES=10 -- jobs "$work/out.json" "$caller"
check "fails the step" "$(is "$status" 1)"
check "calls the jobs route once: no ladder after a wait past the limit" "$(is "$(calls)" 1)"
check "asks sleep for nothing" "$(is "$(sleeps)" '')"
check "names the reset and the job's limit it falls past" \
  "$(contains "$out" "^::error::the API budget is spent until .*, past this job's limit at ")"
check "and the read's own line names the attempt that decided it" \
  "$(contains "$out" "attempt 1, which decided the failure, answered HTTP 403")"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"

echo "case: RATE_LIMITED, no limit set: no wait, and the ladder still makes its three attempts"
fixture 3 "$rate_limited"
echo "$(($(date +%s) + 3600))" > "$work/reset"
drive_limited -- jobs "$work/out.json" "$caller"
check "fails the step" "$(is "$status" 1)"
check "after the ladder's three attempts" "$(is "$(calls)" 3)"
check "each naming the limit it lacks" \
  "$(is "$(grep -c 'names no time limit to wait within' <<<"$out")" 3)"
check "asks sleep only for the ladder's two waits" "$(is "$(sleeps)" '0|0')"

echo "case: blobs-refused names the download step that failed and nothing beyond it"
# What this job knows of a refused blob download is which step failed and on which attempt.
# It does not know what the download action called: with no `github-token` the action lists
# over the internal Twirp `ListArtifacts` and GETs a signed blob-storage URL, which job
# 128735037875 of #2983 shows as `Redirecting to blob download url:
# https://<storage account>.blob.core.windows.net/actions-results/...` before its five-retry
# error (read 2026-11-08), and a digest mismatch fails the same step with no retry at all.
# So the line names no route and no retry count, and the drive gives the mode neither the
# repository nor the run id, which it no longer reads.
fixture 0
set +e
out=$(PATH="$work/bin:$PATH" GITHUB_REPOSITORY='' GITHUB_RUN_ID='' \
  GITHUB_RUN_ATTEMPT=$run_attempt "$read_script" blobs-refused 2>&1)
status=$?
set -e
check "exits 0, since the download step already failed the job" "$(is "$status" 0)"
check "makes no request, so it needs no GH_TOKEN" "$(is "$(calls)" 0)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the step that failed" \
  "$(contains "$(error_line)" "the step .Download every leg's blob report. failed")"
check "the run attempt it failed on" "$(contains "$(error_line)" 'on run attempt 4')"
check "and that step's own error line as where the reason is" \
  "$(contains "$(error_line)" "that step's own error line in this job's log")"
check "and that this error is a failed read and not a failed row" \
  "$(contains "$(error_line)" "This error is a failed read of the run's artifacts, not a failed browser row\.$")"
check "nor that no row failed, which this job did not read" \
  "$(is "$(contains "$(error_line)" 'no row of the suite failed')" false)"
check "while naming no GET route at all" "$(is "$(contains "$(error_line)" 'GET ')" false)"
check "no retry count this job cannot see" \
  "$(is "$(contains "$(error_line)" 'retr')" false)"
check "and nothing about what a re-run would find on the run" \
  "$(is "$(contains "$(error_line)" 're-run')" false)"

echo "case: an environment variable the step did not set"
fixture 0
set +e
out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
  GITHUB_RUN_ATTEMPT='' "$read_script" jobs "$work/out.json" "$caller" 2>&1)
status=$?
set -e
check "fails the step" "$(is "$status" 1)"
check "names the variable" \
  "$(contains "$out" 'dispatch-browser-run-reads.sh needs GITHUB_RUN_ATTEMPT in its environment')"
check "makes no request" "$(is "$(calls)" 0)"
check "and writes no out-file" "$(is "$(wrote_out_file)" no)"

fixture 0
set +e
out=$(PATH="$work/bin:$PATH" GH_TOKEN='' GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
  GITHUB_RUN_ATTEMPT=$run_attempt "$read_script" jobs "$work/out.json" "$caller" 2>&1)
status=$?
set -e
check "a jobs read without a token is refused by name too" "$(is "$status" 1)"
check "naming it" \
  "$(contains "$out" 'dispatch-browser-run-reads.sh needs GH_TOKEN in its environment')"
check "and makes no request" "$(is "$(calls)" 0)"

echo "case: a mode this script does not have"
fixture 0
drive rows "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "names what it was given" "$(contains "$out" 'It was given: rows')"
check "and what it takes" \
  "$(contains "$out" 'dispatch-browser-run-reads.sh jobs <out-file> <job>')"
check "makes no request" "$(is "$(calls)" 0)"

fixture 0
drive jobs
check "a jobs call naming no out-file fails too" "$(is "$status" 1)"
check "naming what it was given" "$(contains "$out" 'It was given: jobs$')"
check "and makes no request" "$(is "$(calls)" 0)"

fixture 0
drive jobs "$work/out.json"
check "a jobs call naming no caller fails too" "$(is "$status" 1)"
check "naming what it was given" "$(contains "$out" "It was given: jobs $work/out.json\$")"
check "and makes no request" "$(is "$(calls)" 0)"
check "and writes no out-file" "$(is "$(wrote_out_file)" no)"

summary "dispatch-browser-run-reads.sh reads this run's attempt"
