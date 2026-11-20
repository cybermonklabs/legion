#!/bin/sh
# network-install.sh <kind> -- <command> [argument…]
# network-install.sh --bounds
#
# Runs one install that waits on the network, bounded, retried and failed by name. A required job
# of .github/workflows/ci.yaml runs every such install through this script, in its steps and in
# the Dockerfiles it builds, and .github/scripts/check-ci-filters.sh (rule 22, in
# .github/scripts/ci_network_installs.py) refuses one that does not. `--bounds` prints the table
# below, one line per kind (`<kind> <bound s> <attempts> <s before SIGKILL> <pause s>`), which
# rule 22 reads to hold each job's limit to the worst case of the installs it runs.
#
# WHY. On 20 November 2026 the daemon-go job of run 44665181416 waited in its "Install tmux" step,
# `sudo apt-get update && sudo apt-get install -y --no-install-recommends tmux`, from 03:05:59Z
# until the job's 70-minute limit cancelled it at 04:14:57Z. Its last line was `Get:5
# https://archive.ubuntu.com/ubuntu noble-security InRelease [126 kB]`: the mirror had sent the
# answer's headers and then all but stopped. apt's own `Acquire::http::Timeout` is a wait for the
# next byte, so a mirror that keeps sending a byte now and then is never timed out, and the job
# failed for a cause outside the change it tested. A required job that waits on the network
# therefore waits under a bound of its own.
#
# WHAT IT DOES.
#   * Each attempt runs the command under timeout(1) for the kind's bound per attempt (the table
#     below), then SIGKILL KILL_AFTER seconds after the SIGTERM where the command has not ended by
#     then.
#   * Whether the bound ended the attempt is read from the attempt itself, never from a clock read
#     after it: timeout(1) runs this script again as `--attempt`, which runs the command and writes
#     a marker file when timeout's SIGTERM reaches it. A command that exits 124 or 137 by itself (an
#     OOM kill is 137) writes no marker, so it is the command's own exit, however long the caller
#     took to read it.
#   * An attempt its bound ended is the wait this script exists for, so it is tried again, at most
#     ATTEMPTS attempts in all, with PAUSE seconds between them. So is an attempt whose output names
#     a network fault (NETWORK_FAULT below): no client of these kinds tells a refused, reset or
#     5xx answer apart from its own failure by its exit status (bun 1.4.4 exits 1 for a refused
#     registry and for a stale bun.lock alike; docker pull exits 1 for a refused connection and for
#     a digest the registry does not hold), but each says which it was in its words.
#   * Any other exit is the command's own: its output names no network fault and its bound did not
#     end it, so it is not tried again, and its error says it is the command's own, since a second
#     attempt would fail the same way and an error that blamed the network would send its reader to
#     re-run a change that needs a fix.
#   * The command's standard output and standard error stay its own (a caller may read either);
#     this script's lines go to standard error. Every attempt prints one line: its number, how it
#     ended and how long it took. When none succeeded, one `::error::` names the kind, the command
#     and how each attempt ended, and says whether the network or the command itself is what
#     failed; the script exits 1.
#   * The streams pass through a FIFO each, and the attempt ends when their last writer has. A
#     process the command leaves holding them (a daemon an install starts, a child left in the
#     background) is waited for only as long as the attempt's bound leaves, then ended by its
#     process group, SIGTERM then SIGKILL KILL_AFTER seconds later, and named, so no attempt
#     outlasts its bound and its kill.
#   * It is a POSIX sh script and needs only timeout(1), mkfifo, tee, grep, sed, cut, readlink and
#     /proc, so a Dockerfile's RUN runs it on the images the required jobs build: Debian's and
#     Ubuntu's (dash, coreutils) and Alpine's (BusyBox's sh and timeout, whose `timeout` signals the
#     command alone, so the attempt sends the SIGKILL itself after KILL_AFTER seconds too).
#
# NETWORK_FAULT. The words each kind's client prints when the network or the server failed it,
# measured on 2026-11-20 against a refused port, a server that resets each connection and one that
# answers 503 (round 3's evidence, item5/faults): bun `ConnectionRefused`, `ConnectionClosed`,
# `GET <url> - 503`; go `connect: connection refused`, `connection reset by peer`, `503 Service
# Unavailable`; apt `Could not connect to`, `Connection failed`, `503  Service Unavailable`; elan
# `Could not connect to server`, `Connection reset by peer`, `tunnel failed, response 503`;
# Playwright `ECONNREFUSED`, `ECONNRESET`, `server returned code 503`; docker `connect: connection
# refused`. And the words Go's own HTTP client, which docker, containerd and the go command are
# built on, gives a server that fails it otherwise, read from their sources (go 1.27.3's net/http,
# containerd 1.7.30's remotes/docker): `net/http: timeout awaiting response headers` (docker gave up
# with it after 41 s on a registry that accepted and never answered), `Client.Timeout exceeded`,
# `net/http: TLS handshake timeout`, `i/o timeout`, `request canceled while waiting for
# connection`, `unexpected EOF` (a body cut short), `stream error: stream ID <n>; INTERNAL_ERROR`
# (an HTTP/2 stream the server reset, the line envoy-infra-go failed on in run 44317642380) and
# `http2: server sent GOAWAY`; and a registry's rate limit, `429 Too Many Requests`, containerd's
# `toomanyrequests` and bun's `GET <url> - 429`. Their own faults name none of these: bun `lockfile
# had changes, but lockfile is frozen`, go `404 Not Found … unknown revision`, apt `Unable to locate
# package`, elan `no such release`, Playwright `Invalid installation targets`, docker `not found`.
# apt-get update exits 0 on a mirror it could not read unless it is given `--error-on=any`, so each
# wrapped update is.
NETWORK_FAULT='connection ?refused|connection ?reset|connectionclosed|econnrefused|econnreset|etimedout|eai_again|could not connect|connection failed|failed to connect|connection timed out|operation timed out|i/o timeout|tls handshake timeout|timeout awaiting response headers|client\.timeout exceeded|request canceled while waiting for connection|unexpected eof|stream error: stream id [0-9]+|server sent goaway|too ?many ?requests|temporary failure (in name )?resolv|could not resolve host|no such host|network is unreachable|tunnel failed|failure when receiving data from the peer|server returned code (429|5[0-9][0-9])|get [^ ]+ - (429|5[0-9][0-9])|5[0-9][0-9] +(service unavailable|bad gateway|gateway time-?out|internal server error)'
#
# THE KINDS. A kind's bound is what the installs it guards took at worst, with room: the slowest
# green run of those installs, times two, rounded up to the next minute. Two, because an install
# that is slow and not stuck must still pass; no more, because a stuck one costs ATTEMPTS bounds of
# its job's limit. A kind's line names what it measured. The steps were read from the 20 newest
# merge-group runs of ci.yaml (44633135919 to 44668680855, 2026-11-19 22:55Z to 2026-11-20 03:34Z);
# an install in a Dockerfile's RUN from that RUN's BuildKit time (`#<n> DONE <s>s`) in the 20
# newest green builds of each image (envoy-smoke's, worker-image's on pull requests and on
# release.yaml, to 2026-11-20 11:15Z), where a RUN's whole time bounds each install in it. A kind
# named `<installer>-<what>` guards installs of that installer measured apart (rule 22 holds an
# install to a kind of its own installer).
#   apt         apt-get update, and apt-get install --download-only: an install's download alone.
#               The install from what it downloaded (--no-download) reads no network and runs
#               outside this script, since a bound that ended it while dpkg wrote would leave every
#               later attempt failing on `dpkg was interrupted` (worker-image run 44765474341, whose
#               runtime-base RUN downloaded 471 packages, 442 MB, in 21.4 s and was still unpacking
#               when the bound ended it; check-ci-filters.sh's rule 22 refuses such a wrap). Slowest
#               of 54 green steps (median 12 s): 25 s, claude-envoy's "Install zsh" in job
#               135990304720 of run 44646446447, whose update read 15.2 MB of package lists, its
#               install included; that 442 MB download, 21.4 s. 2 × 25 s is 50 s, so 60 s an attempt.
#               The worker image's runtime-base RUN, its two installs included, took 28.3 s in the 4
#               of 40 green builds that ran it, and bounds nothing now: its unpacking is no network wait.
#   bun         bun install and bun add from the npm registry. Slowest of 499 green steps (median
#               6 s): 55 s, proof-sdk-offline's cold install behind its fault proxy in job
#               135990304816 of the same run; of the Dockerfiles' bun RUNs but the one below: 12.1 s,
#               envoy's web stage in job 136203160053. 2 × 55 s is 110 s, so 120 s an attempt. Bun's
#               own registry client does end a request that gets no answer, but only after minutes
#               and with retries of its own: an install pointed at a registry that accepts and never
#               answers had not ended after 20 minutes, longer than every job that installs with it
#               may run.
#   bun-image   bun install of the whole root workspace in a Dockerfile's RUN, from an empty cache:
#               the worker image's plugin stage. Slowest of 40 green builds: 134.1 s, job
#               136248354728. 2 × 134.1 s is 268.2 s, so 300 s an attempt.
#   docker      docker pull of an image from ghcr.io. Slowest of 127 green steps: 41 s, the visual
#               job's "Pull the pinned Playwright copy" in job 136044109917. 2 × 41 s is 82 s, so
#               120 s an attempt. docker gives up after 40 s on a registry that sends no headers,
#               but against one that sends them and then one byte every 5 s a pull had not ended
#               after 900 s: that wait for the body has no bound of docker's own.
#   playwright  Playwright's download of its Chromium (`bunx playwright install chromium`).
#               Slowest of 64 green steps: 10 s, the docs job's step in job 136019959029. 2 × 10 s
#               is 20 s, so 60 s an attempt. Playwright 1.64.1's own timeout is a socket's idle
#               time, 30 s, which a server that sends one byte every 5 s never reaches: such a
#               download had not ended after 900 s.
#   go          a go download alone: `go mod download`, and the `go get` with which go-fetch.sh
#               fetches a tool's modules. A go command that compiles runs outside this script, from
#               the module cache with the network off (go-fetch.sh's header says why; rule 22
#               refuses a compile under this kind). Slowest of the downloads: 44.1 s, the worker
#               image's `go mod download` of its 380 MiB of modules, in job 136046630907, of 40 green
#               builds; gopls's downloads ended at 6.1 s of its install in worker-image run
#               44772738928. 2 × 44.1 s is 88.2 s, so 120 s an attempt.
#   elan        elan's install of the Lean toolchain lean-toolchain names, with its check for a newer
#               elan before it (a read of github.com; both happen only when the toolchain is not
#               installed). Slowest of the 37 green airlock-lean jobs of 30 days that downloaded it:
#               27 s for the whole step, job 135268429397 (the download 10.9 s, its unpack 12.0 s).
#               2 × 27 s is 54 s, so 60 s an attempt. Against a server that answers and then sends
#               one byte every 5 s, either read had not ended after 1,260 s.
#   apk         apk add of Alpine packages. Slowest of 40 green builds of the worker image's go
#               stage: 1.0 s, job 136243259140. 2 × 1.0 s, so 60 s an attempt.
#   npm         npm ci of a project's lockfile. The worker image's toolchain RUN, which holds its
#               one npm ci beside seven downloads: 43.2 s at its slowest of 40 green builds, job
#               136004778019. 2 × 43.2 s is 86.4 s, so 120 s an attempt.
#   uv          uv making a virtual environment of a managed Python and installing into it. The
#               worker image's azure-cli RUN, which holds both: 33.5 s at its slowest of 40 green
#               builds, job 136046630907. 2 × 33.5 s is 67 s, so 120 s an attempt.
#   gcloud      the Google Cloud SDK's install.sh, which reads the channel's component list from
#               dl.google.com. The worker image's SDK RUN, which holds its download, install.sh and
#               the bytecode compile: 30.2 s at its slowest of 40 green builds, job 136124064333.
#               2 × 30.2 s is 60.4 s, so 120 s an attempt.
#   mise needs no kind: its own 30 s bound on each request covers the body too, so a stalled
#   `mise install` ends by itself (rule 22's docstring gives the measurement).
#   yarn has no kind: no required job installs with Yarn, so no run measures a bound for it, and
#   npm's would be borrowed. Rule 22 refuses a Yarn install by name ("no bound measured for
#   yarn"); a job that adds one measures that install's runs and adds the kind here.
#
# TEST SEAM. NETWORK_INSTALL_BOUND_SECONDS, NETWORK_INSTALL_KILL_AFTER_SECONDS and
# NETWORK_INSTALL_PAUSE_SECONDS replace the kind's bound, the 15 s before SIGKILL and the pause,
# for .github/scripts/network-install.test.sh alone, so its cases end in seconds. No workflow sets
# them, and `--bounds` prints the table's own values whatever they say.
#
# Run from anywhere: .github/scripts/network-install.sh apt -- sudo apt-get update --error-on=any
# Its tests: .github/scripts/network-install.test.sh, in ci.yaml's test job.
set -eu

ATTEMPTS=3
DEFAULT_PAUSE=10
DEFAULT_KILL_AFTER=15
KINDS="apt bun bun-image docker playwright go elan apk npm uv gcloud"

# bound_of <kind>: the kind's bound per attempt in seconds, from THE KINDS above, or nothing.
bound_of() {
  case "$1" in
    apt) echo 60 ;;
    bun) echo 120 ;;
    bun-image) echo 300 ;;
    docker) echo 120 ;;
    playwright) echo 60 ;;
    go) echo 120 ;;
    elan) echo 60 ;;
    apk) echo 60 ;;
    npm) echo 120 ;;
    uv) echo 120 ;;
    gcloud) echo 120 ;;
  esac
}

die() {
  echo "::error::network-install: $*" >&2
  exit 1
}

# group_of <pid>: sets `pgrp` to the process group of a process, read from /proc with no process
# started (so a scan of every process stays cheap), or returns 1 where it is gone.
group_of() {
  pgrp=
  stat=
  read -r stat < "/proc/$1/stat" 2> /dev/null || return 1
  set -- ${stat##*) }
  pgrp=$3
}

# network-install.sh --attempt <marker> <seconds before SIGKILL> -- <command…>: one attempt's
# command, which timeout(1) runs. It runs the command with the caller's standard input (handed in
# on descriptor 3, since a command a POSIX shell starts in the background reads /dev/null), and
# when timeout's SIGTERM reaches it, it writes <marker>, passes the signal to the command, and
# sends the command SIGKILL <seconds before SIGKILL> later where it is still running (the sleep
# that waits for that holds none of the command's streams, so the caller's copies of them end with
# the command). It exits with the command's status. It writes its process group, which timeout(1)
# gave it and its command, to <marker>.group, where the caller looks for what the command left.
if [ "${1:-}" = --attempt ]; then
  marker=$2
  kill_after=$3
  shift 4
  group_of "$$" && echo "$pgrp" > "$marker.group"
  child=
  killer=
  trap 'echo fired > "$marker"; if [ -n "$child" ]; then kill -TERM "$child" 2> /dev/null || :; (sleep "$kill_after"; kill -KILL "$child" 2> /dev/null || :) < /dev/null > /dev/null 2>&1 & killer=$!; fi' TERM
  "$@" <&3 3<&- &
  child=$!
  [ ! -e "$marker" ] || kill -TERM "$child" 2> /dev/null || :
  status=0
  wait "$child" || status=$?
  while kill -0 "$child" 2> /dev/null; do
    status=0
    wait "$child" || status=$?
  done
  [ -z "$killer" ] || kill "$killer" 2> /dev/null || :
  exit "$status"
fi

if [ "$#" -eq 1 ] && [ "$1" = --bounds ]; then
  for kind in $KINDS; do
    echo "$kind $(bound_of "$kind") $ATTEMPTS $DEFAULT_KILL_AFTER $DEFAULT_PAUSE"
  done
  exit 0
fi

[ "$#" -ge 3 ] && [ "$2" = "--" ] ||
  die "usage: network-install.sh <kind> -- <command> [argument…], and this call gave: $*"
kind=$1
shift 2

bound=$(bound_of "$kind")
[ -n "$bound" ] ||
  die "'$kind' is no kind this script bounds; the kinds are: $KINDS. A new kind is a line of the table above with its own measured bound"
bound=${NETWORK_INSTALL_BOUND_SECONDS:-$bound}
PAUSE=${NETWORK_INSTALL_PAUSE_SECONDS:-$DEFAULT_PAUSE}
KILL_AFTER=${NETWORK_INSTALL_KILL_AFTER_SECONDS:-$DEFAULT_KILL_AFTER}

command_text=$*
work=$(mktemp -d "${TMPDIR:-/tmp}/network-install.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkfifo "$work/out" "$work/err"
group_of "$$" || :
own_group=$pgrp

# end_holders <signal>: each process of the attempt's process group (or of this script's own, where
# the attempt's timeout(1) made none, as BusyBox's does not) that still holds the command's two
# streams after the attempt ended (a daemon an install started, a child it left in the background),
# sent <signal> by that process group (by its pid alone where the group is this script's own), and
# named in $work/held once. While one holds them the copies do not end, nor this script with them.
end_holders() {
  attempt_group=
  read -r attempt_group < "$work/fired.group" 2> /dev/null || :
  for proc in /proc/[0-9]*; do
    pid=${proc#/proc/}
    case $pid in "$copy_out" | "$copy_err" | "$$") continue ;; esac
    group_of "$pid" || continue
    [ "$pgrp" = "$attempt_group" ] || [ "$pgrp" = "$own_group" ] || continue
    for fd in "$proc"/fd/*; do
      [ "$fd" -ef "$work/out" ] || [ "$fd" -ef "$work/err" ] || continue
      name=?
      read -r name < "$proc/comm" 2> /dev/null || :
      grep -qx "$pid" "$work/held.pids" 2> /dev/null ||
        { echo "$pid" >> "$work/held.pids"; echo "$name (pid $pid, process group $pgrp)" >> "$work/held"; }
      if [ "$pgrp" != "$own_group" ]; then
        kill -s "$1" -- "-$pgrp" 2> /dev/null || :
      else
        kill -s "$1" "$pid" 2> /dev/null || :
      fi
      break
    done
  done
}
endings=
attempt=1
while :; do
  rm -f "$work/fired" "$work/fired.group"
  : > "$work/output"
  # The command's own two streams pass through, each copied into one file the fault words are read from.
  tee -a "$work/output" < "$work/out" &
  copy_out=$!
  tee -a "$work/output" < "$work/err" >&2 &
  copy_err=$!
  started=$(date +%s)
  status=0
  timeout -k "$KILL_AFTER" "$bound" sh "$0" --attempt "$work/fired" "$KILL_AFTER" -- "$@" 3<&0 \
    > "$work/out" 2> "$work/err" || status=$?
  ended=$(date +%s)
  # The copies end when the last writer of the two streams has; a process the command left holding
  # them is waited for only as long as the attempt's bound leaves, then ended by its process group
  # (KILL_AFTER seconds after SIGTERM, KILL), so the attempt still ends inside its bound and its kill.
  : > "$work/held"
  (
    left=$((started + bound - ended))
    while [ "$left" -gt 0 ]; do sleep 1; left=$((left - 1)); done
    end_holders TERM
    left=$KILL_AFTER
    while [ "$left" -gt 0 ] && kill -0 "$copy_out" 2> /dev/null; do sleep 1; left=$((left - 1)); done
    end_holders KILL
    sleep 1
    # A holder the scan could not reach (one that left both process groups) still ends nothing of
    # this script's: the copies themselves end, and the attempt with them.
    kill "$copy_out" "$copy_err" 2> /dev/null || :
  ) < /dev/null > /dev/null 2>&1 &
  watchdog=$!
  wait "$copy_out" "$copy_err" || :
  kill "$watchdog" 2> /dev/null || :
  wait "$watchdog" 2> /dev/null || :
  took=$((ended - started))
  if [ -s "$work/held" ]; then
    echo "network-install $kind: attempt $attempt left $(sed ':a;N;$!ba;s/\n/, /g' "$work/held") holding its output when it exited, and its bound ended them: $command_text" >&2
  fi
  if [ "$status" -eq 0 ]; then
    echo "network-install $kind: attempt $attempt of $ATTEMPTS finished in ${took} s: $command_text" >&2
    exit 0
  fi
  if [ -e "$work/fired" ] && [ "$status" -eq 137 ]; then
    ending="attempt $attempt was killed ${KILL_AFTER} s after its ${bound} s bound, since it ignored SIGTERM"
  elif [ -e "$work/fired" ]; then
    ending="attempt $attempt timed out after ${bound} s"
  elif fault=$(grep -Eio -m 1 "$NETWORK_FAULT" "$work/output"); then
    ending="attempt $attempt exited $status after ${took} s, and its output names a network fault ($fault)"
  else
    ending="attempt $attempt exited $status after ${took} s, and its ${bound} s bound did not end it"
    echo "network-install $kind: $ending: $command_text" >&2
    endings="${endings:+$endings; }$ending"
    die "$kind install \`$command_text\` failed ($endings). Exit $status is the command's own: its output names no network fault and its bound did not end it, so it was not tried again; its own error is in its output above."
  fi
  echo "network-install $kind: $ending: $command_text" >&2
  endings="${endings:+$endings; }$ending"
  [ "$attempt" -lt "$ATTEMPTS" ] || break
  attempt=$((attempt + 1))
  sleep "$PAUSE"
done
die "$kind install \`$command_text\` did not finish ($endings). Each attempt waited on the network past its bound of ${bound} s or failed on a network fault its output names, so what failed is the network or the server it reads, not the change under test."
