#!/bin/sh
# fetch.sh <stage> <seconds> <url> -o <file> [curl argument…]
#
# One download with curl, bounded, retried and failed by name: the one form every curl download of
# packages/daemon/docker/worker.Dockerfile and .github/docker/browser.Dockerfile takes. Each RUN
# that downloads bind-mounts this file into /usr/local/bin and calls it, so a fix to the form is
# made here once and reaches every one of them. The curl arguments after the address are curl's
# own and must name the file the answer goes to (`-o <file>` for a download, `-I -o /dev/null` for
# a check that the address answers), since curl's standard output carries the HTTP status this
# script reads. <stage> names the part of the build that fetches (a stage, or what it installs:
# tools, toolchain, gcloud, notices, postgres) in the failure line. Any checksum check is the
# caller's, after the fetch.
#
# WHY. On 2026-11-23 a single 503 from raw.githubusercontent.com failed the worker image's build
# in job 137719519443: its notices RUN read each file with a bare `curl -fsSL`, which tries once.
# The image's downloads then each carried a form of their own, pasted into five RUNs and written
# out a sixth time for the PostgreSQL key, and the browser image's key had a third, so a fix to one
# reached none of the others.
#
# WHAT IT DOES.
#   * An attempt is cut on a stall, as .github/scripts/ci-tools.sh's ci_tools_curl cuts one: one
#     that moved fewer than 10,000 bytes a second for 30 seconds straight (`--speed-limit 10000
#     --speed-time 30`, curl exit 28), and one that has not connected in 30 s (`--connect-timeout
#     30`). So a host that accepts and sends nothing, one that stops mid-body and one that never
#     answers the connection are each cut 30 s into the stall.
#   * curl's --max-time <seconds> is what is left of the clock: it ends an attempt that keeps
#     moving above that floor and still has not finished, its body included.
#   * Any failure is tried again, twice (--retry 2 --retry-all-errors): a 429, a 5xx, a 404, a
#     refused or reset connection and a cut attempt each get up to three attempts. curl pauses
#     1 s and then 2 s between them, or what the server's Retry-After asks.
#   * No attempt starts once twice the bound has passed since the first (--retry-max-time), and a
#     Retry-After that would wait past that ends the retries at once. A stall is cut 30 s in, so
#     it gets its three attempts under every bound of 60 s or more; only two attempts that each
#     run to the bound while still moving leave no room for a third, so a fetch makes up to 3
#     attempts, or 2 where the first two each ran to the bound. A third attempt of a download
#     that slow would run to the bound too. So a fetch ends inside three bounds and curl's two
#     pauses, and a job limit counts each fetch at 3 × <seconds> + 3 s.
#   * The script holds the result itself rather than curl's exit status alone: the fetch succeeded
#     only when curl exited 0, the last answer's HTTP status (`-w '%{http_code}'`, after any
#     redirect `-L` followed) is a 2xx, and the file -o names is there and not empty. curl 8.14.1,
#     Debian trixie's and so the worker image's toolchain and runtime-base stages', exits 0 after a
#     429 or a 503 whose Retry-After waits past --retry-max-time, having printed its error 22 and
#     written no file; 7.88.1 and 8.5.0 exit 22 there. A 200 with an empty body would otherwise
#     leave a 0-byte file, which the notices stage, whose files have no checksum, would ship.
#   * A file -o names is removed before the fetch, and a retry truncates it and fetches it whole,
#     so the file a checksum then reads holds what this call fetched and no partial body.
#   * When the fetch did not succeed, curl's own error comes first, then one line names the stage,
#     the address, the bound, curl's exit status and the last HTTP status (000 where no answer
#     came):
#       <stage>: <url> failed after up to 3 attempts of <seconds> s, or 2 where the first two each ran to the bound (curl exit <status>, HTTP <code>)
#     and the script exits 1, which fails the RUN under `set -eu` or an `&&` chain.
#   * It is a POSIX sh script that needs only curl, so a RUN runs it on Debian's dash as on Ubuntu's.
#
# THE BOUND A CALLER PASSES holds the download at the slowest moving rate CI has seen: the file's
# size at 250,000 bytes a second, rounded up to the minute, and at least 60 s. In job 137969866080
# (run 45253427255) PyPI served a 17,211,825-byte wheel at about 257 kB/s; a bound of twice the
# slowest green fetch, the rule network-install.sh's kinds follow, held each of these downloads to
# 60 s, which cut Chromium's 206 MB zip below about 3.4 MB/s while it was still moving. The green
# fetches are all far under the size rule (the slowest, Oh My Pi's 173 MB archive, at 3.1 s), so
# the size decides. The Dockerfile that calls this gives each download's size and bound, and the
# notes beside it each download's measurement (.github/ci-notes/worker-image.md,
# .github/ci-notes/browser-image.md). A version bump reads the new file's size again and moves the
# bound, and the job limit derived from it, when the size passes what the bound holds.
#
# Run from anywhere: .github/scripts/fetch.sh tools 60 https://example.com/a.tar.gz -o a.tar.gz
# Its tests: .github/scripts/fetch.test.sh, in ci.yaml's test job.
set -eu

if [ "$#" -lt 3 ]; then
  echo "::error::fetch: usage: fetch.sh <stage> <seconds> <url> -o <file> [curl argument…], and this call gave: $*" >&2
  exit 2
fi
stage=$1
bound=$2
url=$3
shift 3
case $bound in
  '' | *[!0-9]* | 0*)
    echo "::error::fetch: $stage: the bound '$bound' is no whole number of seconds above 0, so $url is not fetched" >&2
    exit 2
    ;;
esac
out=
next_is_out=false
for arg in "$@"; do
  if [ "$next_is_out" = true ]; then
    out=$arg
    next_is_out=false
  fi
  case $arg in -o | --output) next_is_out=true ;; esac
done
if [ -z "$out" ]; then
  echo "::error::fetch: $stage: no -o <file> among the curl arguments, so $url is not fetched: curl's standard output carries the HTTP status fetch.sh reads" >&2
  exit 2
fi
# A regular file alone: -o /dev/null names a device, which stays.
if [ -f "$out" ]; then rm -f -- "$out"; fi

status=0
code=$(curl -fsSL --connect-timeout 30 --speed-limit 10000 --speed-time 30 --max-time "$bound" \
  --retry 2 --retry-all-errors --retry-max-time "$((bound * 2))" -w '%{http_code}' "$@" "$url") || status=$?
case $code in
  2[0-9][0-9]) answered=true ;;
  *) answered=false ;;
esac
# A file of 0 bytes is no fetch either: no caller asks for an empty file (each -o file is an archive,
# a key or a notices text; a check that the address answers writes to /dev/null, a device).
empty=false
if [ "$out" != /dev/null ] && [ -f "$out" ] && [ ! -s "$out" ]; then empty=true; fi
if [ "$status" -ne 0 ] || [ "$answered" = false ] || [ ! -e "$out" ] || [ "$empty" = true ]; then
  why="curl exit $status, HTTP ${code:-000}"
  if [ "$status" -eq 0 ] && [ "$answered" = true ] && [ ! -e "$out" ]; then why="$why, and nothing at $out"; fi
  if [ "$status" -eq 0 ] && [ "$answered" = true ] && [ "$empty" = true ]; then why="$why, and $out is empty"; rm -f -- "$out"; fi
  echo "$stage: $url failed after up to 3 attempts of $bound s, or 2 where the first two each ran to the bound ($why)" >&2
  exit 1
fi
