#!/usr/bin/env bash
# Installs the Ubuntu packages a CI step needs and the GitHub-hosted runner image lacks, with every
# apt call bounded: tmux, zsh, the PostgreSQL client.
#
# Those steps ran `sudo apt-get update && sudo apt-get install ...` with nothing around it, which
# waits as long as a mirror makes it wait. In job 136053067213 of run 44665276896 `apt-get update`
# printed its last line at 03:06:14, read nothing more for nine and a half minutes, and was
# cancelled with the job at its 10-minute timeout: no test ran, and the log said only "The
# operation was canceled". apt's own inactivity timeout did not end it, so here `timeout` ends each
# call, and the step retries by count and fails naming what it was doing after the last attempt.
#
# Usage: apt-install.sh --budget <seconds> <package>...
#
# A package the image carries already - dpkg-query reads it `installed` - takes no apt call at all:
# the x64 ubuntu-24.04 image carries tmux (job 136078635450 of run 44673073324 printed `tmux is
# already the newest version (3.4-1ubuntu0.2)`), so a step that needs only tmux fetches nothing.
# For the packages it lacks:
#
# Each attempt runs `apt-get update` and then `apt-get install -y --no-install-recommends
# <package>...`, each call under `timeout`, so a call that hangs is ended - and with it the
# acquire methods apt starts, which share its process group - and counts as a failed attempt. Two
# attempts, 10 s apart; after the second the script prints an ::error:: line naming the job, the
# packages, the attempts and how the last call ended, and exits 1.
#
# The budget is the most the whole step may take, and the script divides it: two attempts of two
# calls are four calls, each bounded at (budget - the wait) / 4 - 10 s, since `timeout` sends TERM
# at a call's bound and KILL 10 s later. So the worst case - every call running to its bound and
# then ignoring TERM - is 4 x (bound + 10 s) + the wait, which is never more than the budget, and
# the named error prints inside it. The budget is the caller's to choose, because the caller is
# what has a job timeout: .github/scripts/check-retry-budgets.sh, in ci.yaml's lint job, refuses a
# call whose budget is more than half its job's timeout-minutes, so the other half is left for
# the steps before the call and the error line. Every caller passes 300 s today: four calls of
# 62 s, where a healthy runner's update takes 6 to 16 s and its install a few seconds.
#
# On x64 apt's own knobs are passed on every call, so a runner image of any age acquires the same
# way. An x64 runner image points the archive at a list of mirrors, azure.archive.ubuntu.com
# first, and apt tries an item on a mirror Acquire::Retries more times, each waiting up to
# Acquire::http::Timeout for a byte, before it moves to the next mirror; it does that for every
# item, one item at a time. Measured in the pinned ubuntu:24.04 image with that mirror list and a
# first mirror that accepts the connection and never answers, `apt-get update` from no index at
# all took more than 20 minutes with apt's defaults, 19 minutes with the image's own values (one
# retry, 15 s, actions/runner-images' images/ubuntu/scripts/build/configure-apt.sh), 571 s with no
# retry and 15 s, and 192 s with no retry and 5 s, after which `apt-get install tmux` took 34 s. A
# healthy mirror answers within a second, so on x64 no retry and 5 s it is. An arm64 runner reads
# ports.ubuntu.com, which that list does not cover, so a retry is its only way past a refused item,
# and there the script passes nothing and leaves the image's own values: three retries, 30 s.
#
# A package named with a version, `name=version`, is on the image only when that version is the
# one installed; any other version, or none, is installed with apt, the pin passed as written.
#
# Inputs, from the environment:
#   GITHUB_JOB          the job, as Actions sets it, named in the error.
#   APT_RETRY_SECONDS   the wait before the second attempt, 10 by default; the tests set 0. It is
#                       taken out of the budget like every other second.
#
# Its tests are in apt-install.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

readonly ATTEMPTS=2 CALLS=2 KILL_SECONDS=10
if [ "${1:-}" != --budget ] || ! [[ ${2:-} =~ ^[0-9]+$ ]]; then
  echo "::error::apt-install.sh takes --budget <seconds> first, the most the whole step may take; .github/scripts/check-retry-budgets.sh holds it to half its job's timeout-minutes" >&2
  exit 2
fi
budget=$2
shift 2
if [ "$#" -eq 0 ]; then
  echo "::error::apt-install.sh needs the packages to install" >&2
  exit 2
fi

retry_seconds=${APT_RETRY_SECONDS:-10}
waits=$((retry_seconds * ATTEMPTS * (ATTEMPTS - 1) / 2))
call_seconds=$(((budget - waits) / (ATTEMPTS * CALLS) - KILL_SECONDS))
if [ "$call_seconds" -lt 1 ]; then
  echo "::error::apt-install.sh's budget of $budget s leaves no second for a call: $ATTEMPTS attempts of $CALLS calls, each with $KILL_SECONDS s between TERM and KILL, and $waits s of waiting between attempts" >&2
  exit 2
fi

options=()
if [ "$(uname -m)" = x86_64 ]; then
  options=(-o Acquire::Retries=0 -o Acquire::http::Timeout=5 -o Acquire::https::Timeout=5)
fi
packages=()
for package in "$@"; do
  name=${package%%=*}
  carried=$(dpkg-query -W -f='${db:Status-Status} ${Version}' "$name" 2> /dev/null || true)
  if [ "${carried%% *}" = installed ] && { [ "$name" = "$package" ] || [ "${carried#* }" = "${package#*=}" ]; }; then
    echo "$name ${carried#* } is on the runner image already: no apt call for it"
  else
    packages+=("$package")
  fi
done
[ "${#packages[@]}" -gt 0 ] || exit 0

# bounded <call> <apt-get argument>...: runs one apt-get call under its bound; on a failure sets
# `ended` to how it ended and returns non-zero.
bounded() {
  local call=$1 status=0
  shift
  sudo timeout -k "$KILL_SECONDS" "$call_seconds" apt-get "${options[@]}" "$@" || status=$?
  case $status in
    0) return 0 ;;
    124 | 137) ended="apt-get $call hung past its $call_seconds s bound and was killed" ;;
    *) ended="apt-get $call exited $status" ;;
  esac
  return 1
}

ended=""
for ((attempt = 1; attempt <= ATTEMPTS; attempt++)); do
  if bounded update update && bounded install install -y --no-install-recommends "${packages[@]}"; then
    exit 0
  fi
  if [ "$attempt" -lt "$ATTEMPTS" ]; then
    wait_seconds=$((attempt * retry_seconds))
    echo "attempt $attempt of $ATTEMPTS to install ${packages[*]} failed: $ended; trying again in $wait_seconds s" >&2
    sleep "$wait_seconds"
  fi
done
echo "::error::job ${GITHUB_JOB:-?} could not install ${packages[*]} with apt-get: $ATTEMPTS attempts of \`apt-get update\` and \`apt-get install\`, each call bounded at $call_seconds s within the step's $budget s budget, all failed; the last: $ended" >&2
exit 1
