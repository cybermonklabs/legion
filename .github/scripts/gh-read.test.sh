#!/usr/bin/env bash
# Tests for gh-read.sh: gh_wait_reset, which waits out a spent API budget when its reset falls
# before the job's or the step's limit and fails by name when it does not, alone in front of a gh
# command and in front of retry_fetch, the one ladder (.github/scripts/ci-retry.sh), whose whole
# arithmetic the wait must leave room for.
#
# Every case runs the real gh against a stand-in GitHub, a local HTTP server
# (.github/scripts/gh-read-stand-in.py), so the answers the helper reads, their status and
# headers, are the ones gh gets. gh reaches it as GitHub's local host (GH_HOST=github.localhost,
# which gh calls over plain http at api.github.localhost) through HTTP_PROXY. The server spends the
# repository's budget at the first route a case calls, for as many seconds as the case asks, so
# the refusal does not hang on how fast gh starts on a loaded machine: until then it answers every
# route 403 with x-ratelimit-remaining: 0, x-ratelimit-reset: that epoch and GitHub's own message,
# and its rate_limit route reports the core budget at 0 with that reset; after it, a route answers
# what the case asked for (a body, or a 404).
#
# The helper reads the job's start and the step's start off the process tree, as the Runner.Worker
# process GitHub's runner starts for each job and that process's child, the step's shell. A case
# runs the helper in a bash under a stand-in Runner.Worker (bash under that name), so the job
# starts when the case does. The last case reads the real one, on a GitHub runner only.
#
# Run from anywhere: .github/scripts/gh-read.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
servers=()
# `|| true`: a stand-in already gone makes kill fail, and under set -e the trap would stop there,
# leave $work behind and exit 1.
trap '[ ${#servers[@]} -eq 0 ] || kill "${servers[@]}" 2>/dev/null || true; rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

if ! command -v gh > /dev/null; then
  echo "gh-read.test.sh needs gh on PATH: every case runs the real gh against the stand-in GitHub" >&2
  exit 1
fi

# The stand-in GitHub is .github/scripts/gh-read-stand-in.py: its header names the files a case
# writes to its state directory ($work/state here) and what each makes it answer.

# stand_in <state dir> <port file> [<stderr file>]: starts a stand-in GitHub on that state, its
# stderr the job log (or that file), and waits until it has written its port. Each one's process is
# kept in servers, which the exit trap stops.
stand_in() {
  if [ -n "${3:-}" ]; then
    python3 "$script_dir/gh-read-stand-in.py" "$1" "$2" 2> "$3" &
  else
    python3 "$script_dir/gh-read-stand-in.py" "$1" "$2" &
  fi
  servers+=("$!")
  for _ in $(seq 50); do
    [ -s "$2" ] && return
    sleep 0.1
  done
  echo "the stand-in GitHub wrote no port" >&2
  exit 1
}

mkdir -p "$work/state" "$work/bin" "$work/gh-config"
stand_in "$work/state" "$work/port"
port=$(<"$work/port")

# The stand-in runner process: bash under the name GitHub's runner starts each job as.
ln -s "$(command -v bash)" "$work/bin/Runner.Worker"

# github <spend for seconds|never> [<answer>]: this case's GitHub. `never` leaves the budget
# whole; a number spends it, at the case's first route request, until that many seconds after it.
# The answer once the budget is back is a 200 with {"title": "t"} unless the case names 404. The
# log starts empty.
github() {
  rm -f "$work/state"/*
  [ "$1" = never ] || echo "$1" > "$work/state/spend-for"
  echo "${2:-200}" > "$work/state/answer"
  echo '{"title": "t"}' > "$work/state/body"
  : > "$work/state/log"
}

# drive [--no-worker] [NAME=value...] -- <shell code>: runs the code in a bash that has sourced
# gh-read.sh, as a step's shell under the stand-in Runner.Worker (none with --no-worker), with gh
# pointed at the stand-in GitHub and the NAME=value pairs set; JOB_TIMEOUT_MINUTES and
# STEP_TIMEOUT_MINUTES are unset unless a pair sets them. Sets out (stdout), err (stderr) and
# status.
drive() {
  local worker=yes
  local -a assignments=()
  if [ "$1" = --no-worker ]; then
    worker=''
    shift
  fi
  while [ "$1" != -- ]; do
    assignments+=("$1")
    shift
  done
  shift
  local -a command=(bash -c "source \"\$GH_READ\"; $1")
  # `; exit` keeps the stand-in worker a process of its own: a `-c` holding one command alone
  # would exec it, and the worker would be gone from the tree the helper reads.
  [ -z "$worker" ] || command=("$work/bin/Runner.Worker" -c '"$@"; exit $?' worker "${command[@]}")
  set +e
  out=$(cd "$work" && env -u JOB_TIMEOUT_MINUTES -u STEP_TIMEOUT_MINUTES -u GITHUB_TOKEN \
    GH_READ="$script_dir/gh-read.sh" GH_HOST=github.localhost GH_TOKEN=t \
    GH_CONFIG_DIR="$work/gh-config" GH_NO_UPDATE_NOTIFIER=1 GH_PROMPT_DISABLED=1 \
    HTTP_PROXY="http://127.0.0.1:$port" http_proxy="http://127.0.0.1:$port" NO_PROXY= no_proxy= \
    "${assignments[@]}" "${command[@]}" 2> "$work/err")
  status=$?
  set -e
  err=$(<"$work/err")
}

# calls <path>: how many requests the stand-in GitHub answered for that path.
calls() { awk -v path="$1" '$3 == path' "$work/state/log" | wc -l | tr -d ' '; }
# nth_at <path> <n>: the epoch (whole seconds) of the n-th request for that path.
nth_at() { awk -v path="$1" -v n="$2" '$3 == path && ++seen == n { split($1, t, "."); print t[1] }' "$work/state/log"; }
spent_until() { cat "$work/state/spent-until"; }
utc() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
errors() { grep -c '^::error::' <<<"$err" || true; }
warnings() { grep -c '^::warning::' <<<"$err" || true; }
at_least() { [ "$1" -ge "$2" ] && echo true || echo false; }
below() { [ "$1" -lt "$2" ] && echo true || echo false; }
# refusals: how many requests the stand-in refused for the secondary limit.
refusals() { [ -f "$work/state/refused" ] && wc -l < "$work/state/refused" | tr -d ' ' || echo 0; }
# gap_at_least <path> <seconds>: whether the second request for that path came at least that many
# seconds after the first, by the stand-in's own clock.
gap_at_least() {
  awk -v path="$1" -v want="$2" '$3 == path { t[++n] = $1 } END { print (n >= 2 && t[2] - t[1] >= want) ? "true" : "false" }' \
    "$work/state/log"
}
# logged_wait_names <pattern>: whether the helper logged no wait (a host slower than the wait has
# none left to sleep and logs none, gh-read.sh's header) or exactly one, matching the pattern.
logged_wait_names() {
  { [ "$(warnings)" -eq 0 ] || { [ "$(warnings)" -eq 1 ] && grep -q -e "$1" <<<"$err"; }; } && echo true || echo false
}

# The stand-in clock (the stand-in GitHub's header says what it is). $work/clockbin holds a sleep
# that waits one real second, then adds its whole seconds to the stand-in's clock-offset, and a date whose
# `+%s` answers the real time plus that offset, every other form of it being the real date's.
mkdir -p "$work/clockbin"
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\nstate=%q\nreal_sleep=%q\n' "$work/state" "$(command -v sleep)"
  cat <<'SH'
if ! [[ $# -eq 1 && $1 =~ ^[0-9]+$ ]]; then
  echo "the stand-in clock's sleep takes whole seconds, not '$*'" >&2
  exit 2
fi
# A second of real time before the clock moves: a helper that did not wait for its sleep (one
# that sent it to the background) makes its next call inside that second, before the clock has
# moved, and the stand-in refuses it as a real GitHub would.
"$real_sleep" 1
offset=$(cat "$state/clock-offset" 2>/dev/null || echo 0)
echo "$((offset + $1))" > "$state/clock-offset.next"
mv "$state/clock-offset.next" "$state/clock-offset"
SH
} > "$work/clockbin/sleep"
{
  printf '#!/usr/bin/env bash\nstate=%q\nreal_date=%q\n' "$work/state" "$(command -v date)"
  cat <<'SH'
if [ "$*" = +%s ]; then
  echo "$(($("$real_date" +%s) + $(cat "$state/clock-offset" 2>/dev/null || echo 0)))"
else
  exec "$real_date" "$@"
fi
SH
} > "$work/clockbin/date"
chmod +x "$work/clockbin/sleep" "$work/clockbin/date"
# drive_on_clock <as for drive>: drive with the stand-in clock's sleep and date first on PATH, so a
# wait the helper sleeps takes a second and a call it makes after the wait reaches the stand-in that
# many seconds later by the stand-in's clock. The case's `github` starts the clock at the real time.
drive_on_clock() { drive PATH="$work/clockbin:$PATH" "$@"; }

echo "case: a spent budget whose reset falls before the job's limit is waited out"
# The wait's line is logged when the helper sleeps; a host slower than the 10 s window has nothing
# left to sleep and logs none (gh-read.sh's header). So these rows, and the like rows below, read the
# wait off the stand-in's clock (the second call comes no sooner than the reset) and read the line
# only when one was logged.
github 10
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
reset=$(spent_until)
check "exits 0" "$(is "$status" 0)"
check "prints the answer of the call it made after the wait" "$(is "$out" '{"title":"t"}')"
check "calls the route twice: refused, then answered" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "reads the reset from the rate_limit route once" "$(is "$(calls /rate_limit)" 1)"
check "makes the second call no sooner than the reset" "$(at_least "$(nth_at /repos/o/r/pulls/1 2)" "$reset")"
check "logs at most one line about the wait, naming the reset and the job's limit" \
  "$(logged_wait_names "^::warning::the API budget is spent until $(utc "$reset"); waiting [0-9]* s for it, within this job's limit at ")"
check "and fails nothing" "$(is "$(errors)" 0)"
check "gh's own words for the refusal stay in the log, once: the answered call printed none" \
  "$(is "$(grep -c 'API rate limit exceeded for installation ID 1' <<<"$err")" 1)"

# The rows above read the wait's line only when one was logged, since a slow host may have no wait
# left to sleep. These two hold a wait the helper sleeps to its line: the wait is an hour off under a
# two-hour limit, so the helper sleeps on any host, and a stand-in sleep first on PATH records the
# seconds and returns at once. The seconds are the hour less the time the helper took to decide, so
# the rows hold them to the hour less ten minutes, which no host takes.
# sleep_stand_in: a sleep for the next drive that records its arguments in $work/sleepbin/slept.
sleep_stand_in() {
  rm -rf "$work/sleepbin"
  mkdir -p "$work/sleepbin"
  printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\n" "$*" >> "$(dirname "$0")/slept"' > "$work/sleepbin/sleep"
  chmod +x "$work/sleepbin/sleep"
}
slept() { cat "$work/sleepbin/slept" 2>/dev/null; }
# drive_recording_sleep <as for drive>: drive with that stand-in sleep first on PATH, so a case that
# must not sleep reads whether the helper slept from the record, not from the time it took.
drive_recording_sleep() {
  sleep_stand_in
  if [ "$1" = --no-worker ]; then
    shift
    drive --no-worker PATH="$work/sleepbin:$PATH" "$@"
  else
    drive PATH="$work/sleepbin:$PATH" "$@"
  fi
}

echo "case: a wait the helper sleeps is logged, naming the reset and the seconds it sleeps"
github 3600
sleep_stand_in
drive JOB_TIMEOUT_MINUTES=120 PATH="$work/sleepbin:$PATH" -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "the helper slept once, for the hour to the reset" \
  "$([[ $(slept) =~ ^[0-9]+$ ]] && [ "$(slept)" -ge 3000 ] && echo true || echo false)"
check "and logged that wait, once, naming the reset, the seconds and the job's limit" \
  "$(is "$(grep -c "^::warning::the API budget is spent until $(utc "$(spent_until)"); waiting $(slept) s for it, within this job's limit at " <<<"$err")|$(warnings)" '1|1')"
check "then failed by name, the budget still spent after the wait" \
  "$(is "$status|$(contains "$err" '^::error::the API budget is still spent after the wait')" '75|true')"

echo "case: a secondary wait the helper sleeps is logged, naming the retry-after and the seconds it sleeps"
github never
echo "403 3600" > "$work/state/secondary"
sleep_stand_in
drive JOB_TIMEOUT_MINUTES=120 PATH="$work/sleepbin:$PATH" -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "the helper slept once, for the hour GitHub asked for" \
  "$([[ $(slept) =~ ^[0-9]+$ ]] && [ "$(slept)" -ge 3000 ] && echo true || echo false)"
check "and logged that wait, once, naming the retry-after, the seconds and the job's limit" \
  "$(is "$(grep -c "^::warning::GitHub's secondary rate limit asks this call to wait 3600 s (its retry-after), until .*; waiting $(slept) s for it, within this job's limit at " <<<"$err")|$(warnings)" '1|1')"
check "then answered the call after it" "$(is "$status|$out" '0|{"title":"t"}')"
rm -rf "$work/sleepbin"

# A case that must not wait spends the budget for a minute or more, so a wait would take that
# long, and reads from a stand-in sleep's record that the helper never slept.
echo "case: a spent budget whose reset falls past the job's limit fails by name at once"
github 3600
drive_recording_sleep JOB_TIMEOUT_MINUTES=1 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(is "$(slept)" '')"
check "calls the route once" "$(is "$(calls /repos/o/r/pulls/1)" 1)"
check "prints exactly one error" "$(is "$(errors)" 1)"
check "naming the reset and the limit" \
  "$(contains "$err" "^::error::the API budget is spent until $(utc "$(spent_until)"), past this job's limit at ")"
check "and nothing on stdout" "$(is "$out" '')"

echo "case: a reset before the limit but inside the margin is not waited for"
# The margin is the time the call and the rest of the step still need after the wait: a reset 40 s
# away under a one-minute limit leaves less than the 30 s margin. With no margin this case sleeps
# 40 s.
github 40
drive_recording_sleep JOB_TIMEOUT_MINUTES=1 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(is "$(slept)" '')"
check "naming the limit it would outlive" "$(contains "$err" "past this job's limit at ")"

echo "case: a limit that has already passed is not waited for"
github 60
drive_recording_sleep JOB_TIMEOUT_MINUTES=0 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(is "$(slept)" '')"
check "naming the limit" "$(contains "$err" "past this job's limit at ")"

echo "case: a step limit tighter than the job's binds"
github 60
drive_recording_sleep JOB_TIMEOUT_MINUTES=10 STEP_TIMEOUT_MINUTES=0 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(is "$(slept)" '')"
check "naming the step's limit" "$(contains "$err" "past this step's limit at ")"

echo "case: a step limit alone is a limit"
github 10
drive_on_clock STEP_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "waits and passes" "$(is "$status|$out" '0|{"title":"t"}')"
check "any wait it logs is held to the step's limit" "$(logged_wait_names "within this step's limit at ")"
github 60
drive STEP_TIMEOUT_MINUTES=0 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "and binds: a reset past it fails by name, naming the step's limit" \
  "$(is "$status|$(contains "$err" "past this step's limit at ")" '75|true')"

echo "case: a plain 404 fails as it did before"
github never 404
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "exits as gh does" "$(is "$status" 1)"
check "gh's words reach stderr" "$(contains "$err" '^gh: Not Found (HTTP 404)$')"
check "the body gh prints reaches stdout, as with a bare gh" "$(contains "$out" '"message":"Not Found"')"
check "reads no rate limit" "$(is "$(calls /rate_limit)" 0)"
check "calls once" "$(is "$(calls /repos/o/r/pulls/1)" 1)"
check "logs nothing of its own" "$(is "$(warnings)|$(errors)" '0|0')"

echo "case: a call that succeeds passes through untouched"
github never
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "exits 0" "$(is "$status" 0)"
check "prints gh's stdout" "$(is "$out" '{"title":"t"}')"
check "and no stderr" "$(is "$err" '')"
check "reads no rate limit" "$(is "$(calls /rate_limit)" 0)"

echo "case: the caller's stdin reaches gh, as with a bare gh (gh api --input -)"
github never
drive JOB_TIMEOUT_MINUTES=5 -- 'printf "%s" "{\"title\":\"from stdin\"}" | gh_wait_reset gh api --method PATCH repos/o/r/pulls/1 --input -'
check "exits 0" "$(is "$status" 0)"
check "the stand-in got the body from stdin" "$(contains "$(cat "$work/state/bodies" 2> /dev/null)" '"title":"from stdin"')"
github never
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1 <&-'
check "and a caller that closed stdin still gets its answer, and no word on stderr" \
  "$(is "$status|$out|$(calls /repos/o/r/pulls/1)|$err" '0|{"title":"t"}|1|')"

echo "case: a step that names no limit does not wait"
github 60
drive_recording_sleep -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(is "$(slept)" '')"
check "naming the reset and the variable it lacks" \
  "$(contains "$err" "^::error::the API budget is spent until $(utc "$(spent_until)"), and this step names no time limit to wait within (JOB_TIMEOUT_MINUTES is not set)")"

echo "case: a limit that is no whole number of minutes does not wait"
github 60
drive_recording_sleep JOB_TIMEOUT_MINUTES=ten -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(is "$(slept)" '')"
check "naming the value" "$(contains "$err" "JOB_TIMEOUT_MINUTES is 'ten', not a whole number of minutes")"

echo "case: no Runner.Worker among the step's parents, so no start to measure from"
if [ "${GITHUB_ACTIONS:-}" = true ]; then
  # On a runner the real Runner.Worker is an ancestor of every process this harness starts.
  echo "  SKIP: on a GitHub runner every process of this harness runs under its job's Runner.Worker"
else
  github 60
  drive_recording_sleep --no-worker JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
  check "fails" "$(is "$status" 75)"
  check "with no sleep" "$(is "$(slept)" '')"
  check "naming why" "$(contains "$err" 'no Runner.Worker process is among its parents')"
fi

# Every script that calls the helper runs under set -euo pipefail, most with inherit_errexit: a
# refused run inside the helper must not end the caller before the helper decides.
strict='set -euo pipefail; shopt -s inherit_errexit;'
echo "case: a caller under set -e still gets the wait"
github 3
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- "$strict"' gh_wait_reset gh api repos/o/r/pulls/1 > answer; cat answer'
check "exits 0" "$(is "$status" 0)"
check "with the answer after the wait" "$(is "$out" '{"title":"t"}')"
check "having called the route twice" "$(is "$(calls /repos/o/r/pulls/1)" 2)"

echo "case: a caller under set -e gets gh's own status for a 404, and stops there"
github never 404
drive JOB_TIMEOUT_MINUTES=5 -- "$strict"' gh_wait_reset gh api repos/o/r/pulls/1 > answer; echo reached'
check "exits as gh does" "$(is "$status" 1)"
check "and runs nothing after the call" "$(is "$(contains "$out" reached)" false)"

echo "case: a caller under set -e stops at a budget spent past the limit, named"
github 3600
drive JOB_TIMEOUT_MINUTES=1 -- "$strict"' x=$(gh_wait_reset gh api repos/o/r/pulls/1); echo reached'
check "exits with the helper's status" "$(is "$status" 75)"
check "naming it" "$(contains "$err" '^::error::the API budget is spent until ')"
check "and runs nothing after the call" "$(is "$(contains "$out" reached)" false)"

echo "case: the budget is spent again when the wait ends"
github 2
touch "$work/state/always-spent"
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "after waiting once" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "naming it" "$(contains "$err" "^::error::the API budget is still spent after the wait for its reset at $(utc "$(spent_until)")")"

echo "case: --stderr keeps the deciding call's words in a file and the helper's own lines in the log"
github 10 404
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset --stderr "$PWD/words" gh api repos/o/r/pulls/1 > body; echo "status=$?"; cat words'
check "the call after the wait decides" "$(contains "$out" '^status=1$')"
check "the file holds that call's words" "$(contains "$out" '^gh: Not Found (HTTP 404)$')"
check "and not the refusal's" "$(is "$(contains "$out" 'rate limit exceeded')" false)"
check "the call after the wait came no sooner than the reset" "$(at_least "$(nth_at /repos/o/r/pulls/1 2)" "$(spent_until)")"
check "and any wait it logged reached the log" "$(logged_wait_names '^::warning::the API budget is spent until ')"
rm -f "$work/words"

# The form every retried call of this repository's CI writes, `gh_wait_reset retry_fetch "<what>"
# <bound> -- gh …` (dispatch://INFRA-1545, round 55): gh_wait_reset runs the ladder once, and runs
# it again after a wait only when the wait, the ladder's whole arithmetic, 5 x <bound> + 150 s,
# and GH_WAIT_MARGIN end before the nearer of the job's and the step's limit. LADDER sources
# ci-retry.sh beside the code and stands a function in for `sleep` that records each call, as
# `<caller> <seconds>`, in $work/sleeps, and returns at once when the ladder asks, so a case does
# not spend the ladder's 150 s; gh_wait_reset's own wait goes to the `sleep` on PATH, the stand-in
# clock's under drive_on_clock, so a case reads it as a fact of that clock rather than a span.
# shellcheck disable=SC2016 # expanded by the driven shell
LADDER='source "${GH_READ%/*}/ci-retry.sh"
sleep() { echo "${FUNCNAME[1]} $1" >> "$PWD/sleeps"; [ "${FUNCNAME[1]}" != gh_wait_reset ] || command sleep "$1"; }
'
ladder_slept=$'retry_fetch 10\nretry_fetch 20\nretry_fetch 40\nretry_fetch 80'
sleeps() { cat "$work/sleeps" 2> /dev/null || true; }
# bare_errors: how many `::error::` lines of the log stand outside every stop-commands block, the
# lines the runner turns into annotations. It reads the log as the runner does: a
# `::stop-commands::<token>` stops command processing until the line `::<token>::`, and while it
# is stopped no other line is a command, a second `::stop-commands::` or another token included,
# so a ladder's own blocks inside the helper's change nothing.
bare_errors() {
  awk '
    open == "" && /^::stop-commands::/ { open = substr($0, 18); next }
    open != "" && $0 == "::" open "::" { open = ""; next }
    open == "" && /^[ \t]*::error::/ { n++ }
    END { print n + 0 }' <<<"$err"
}

echo "case: composed, a spent budget whose wait and one more ladder fit the limit is waited out"
# 60 s: the ladder's five refused attempts and the rate_limit read land before the reset on any
# host, and the wait costs a second on the stand-in clock. Bound 5 s: the wait, about 61 s, plus
# 5 x 5 + 150 = 175 s and 30 s, about 266 s, fits 5 minutes.
github 60
rm -f "$work/sleeps"
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- "$LADDER"'gh_wait_reset retry_fetch "the pull request" 5 -- gh api repos/o/r/pulls/1'
reset=$(spent_until)
check "exits 0" "$(is "$status" 0)"
check "prints the answer of the ladder run after the wait" "$(is "$out" '{"title":"t"}')"
check "calls the route six times: the ladder's five, refused, then one after the wait" \
  "$(is "$(calls /repos/o/r/pulls/1)" 6)"
check "the sixth no sooner than the reset" "$(at_least "$(nth_at /repos/o/r/pulls/1 6)" "$reset")"
check "the ladder's four sleeps, then one wait of gh_wait_reset's own" \
  "$(is "$(sed '$d' <<<"$(sleeps)")|$(tail -n 1 <<<"$(sleeps)" | cut -d' ' -f1)" "$ladder_slept|gh_wait_reset")"
check "logs one wait, naming the reset, the ladder it runs again and the job's limit" \
  "$(is "$(grep -c "^::warning::the API budget is spent until $(utc "$reset"); waiting [0-9]* s for it, then one more run of its ladder, 5 x 5 + 150 = 175 s, and 30 s after it, within this job's limit at " <<<"$err")" 1)"
check "and no ::error:: of its own: the one is the first ladder's exhaustion" \
  "$(is "$(errors)|$(contains "$err" '^::error::the pull request failed to download after 5 attempts')" '1|true')"
check "which stays inside the refused run's stop-commands block: no ::error:: outside a block annotates the step" \
  "$(is "$(bare_errors)" 0)"

echo "case: composed, a wait that does not fit with one more ladder fails by name at once"
# Bound 30 s: the ladder alone, 5 x 30 + 150 = 300 s, and 30 s fit a 6-minute job, and the
# 60 s wait on top of them does not. A margin that left the ladder out would wait the 60 s.
github 60
rm -f "$work/sleeps"
drive JOB_TIMEOUT_MINUTES=6 -- "$LADDER"'gh_wait_reset retry_fetch "the pull request" 30 -- gh api repos/o/r/pulls/1'
check "fails by name" "$(is "$status" 75)"
check "at once: no wait of gh_wait_reset's own, nothing slept but the ladder's own four waits" \
  "$(is "$(sleeps)" "$ladder_slept")"
check "after one ladder, five calls" "$(is "$(calls /repos/o/r/pulls/1)" 5)"
check "naming the reset, the ladder it would run again and the job's limit" \
  "$(contains "$err" "^::error::the API budget is spent until $(utc "$(spent_until)"), and that time with one more run of its ladder, 5 x 30 + 150 = 300 s, and 30 s after it ends at [0-9TZ:-]*, past this job's limit at ")"
check "and nothing on stdout" "$(is "$out" '')"

echo "case: composed, a step limit nearer than the job's is the one the wait and the ladder are held to"
# Bound 10 s: 5 x 10 + 150 = 200 s and 30 s fit the step's 4 minutes alone, and the 40 s wait on
# top of them does not; all three fit the job's 10 minutes, so a helper that read the job's limit
# alone would wait the 40 s.
github 40
rm -f "$work/sleeps"
drive JOB_TIMEOUT_MINUTES=10 STEP_TIMEOUT_MINUTES=4 -- "$LADDER"'gh_wait_reset retry_fetch "the pull request" 10 -- gh api repos/o/r/pulls/1'
check "fails by name" "$(is "$status" 75)"
check "at once: no wait of gh_wait_reset's own, nothing slept but the ladder's own four waits" \
  "$(is "$(sleeps)" "$ladder_slept")"
check "naming the step's limit, not the job's" \
  "$(contains "$err" "^::error::the API budget is spent until .*, past this step's limit at ")"

echo "case: composed, stdout the caller sends to stderr is written inside a stop-commands block"
# retry_fetch, run with its stdout in the helper's file, cannot see the caller's `>&2`, so the
# helper puts the answer inside a block of its own (ci-log-guard.sh) rather than after the
# ladder's last block closed, where the runner would act on a workflow command in it.
github never
drive JOB_TIMEOUT_MINUTES=5 -- "$LADDER"'gh_wait_reset retry_fetch "the pull request" 5 -- gh api repos/o/r/pulls/1 >&2'
check "exits 0" "$(is "$status" 0)"
check "nothing on stdout" "$(is "$out" '')"
check "the answer is on stderr inside a block that opens before it and closes after it" \
  "$(awk -v want='{"title":"t"}' '
      /^::stop-commands::/ { open = substr($0, 18); next }
      open != "" && $0 == "::" open "::" { open = ""; next }
      $0 == want { print (open != "" ? "inside" : "outside"); exit }' <<<"$err" | grep -qx inside &&
    echo true || echo false)"

echo "case: composed, an answer the caller's stdout cannot take fails the call"
# The answer is written last, from the helper's file to the caller's stdout, which here is
# /dev/full: every write to it fails. A caller that writes the answer to a file inside an `if`
# (ci-ok-reconcile.sh's runs, ci-ok-report.sh's jobs, post-check-run.sh's posted) must see that,
# as a bare retry_fetch shows it (ci-retry.sh writes `cat … || status=$?`). With the helper's
# final `cat` bare it returned 0 (review round 37 of dispatch://INFRA-1545).
github never
drive JOB_TIMEOUT_MINUTES=5 -- "$LADDER"'gh_wait_reset retry_fetch "the pull request" 5 -- gh api repos/o/r/pulls/1 > /dev/full'
check "exits with the failed write's status" "$(is "$status" 1)"
check "the write's own error reaches stderr" "$(contains "$err" 'write error')"
check "after one call, which the host answered" "$(is "$(calls /repos/o/r/pulls/1)" 1)"

echo "case: composed, any other refusal climbs the whole ladder and is the ladder's"
# A 404 is no rate limit: the helper hands the ladder's own result through, after its five
# attempts, and reads no rate limit.
github never 404
rm -f "$work/sleeps"
drive JOB_TIMEOUT_MINUTES=5 -- "$LADDER"'gh_wait_reset retry_fetch "the pull request" 5 -- gh api repos/o/r/pulls/1'
check "fails as gh does" "$(is "$status" 1)"
check "after five attempts" "$(is "$(calls /repos/o/r/pulls/1)" 5)"
check "having slept nothing but the ladder's own four waits" "$(is "$(sleeps)" "$ladder_slept")"
check "and read no rate limit" "$(is "$(calls /rate_limit)" 0)"

# GraphQL answers a spent budget with HTTP 200, an error of type RATE_LIMITED and
# x-ratelimit-remaining: 0, and gh fails on it. Composed, the helper reads that answer through
# retry_fetch: each attempt runs gh by name under timeout, so the gh it runs is the helper's own,
# and the answer the helper reads is the last attempt's.
echo "case: composed, GraphQL's refusal for its budget, a 200 with x-ratelimit-remaining: 0, is waited out"
github 60
touch "$work/state/graphql-200"
rm -f "$work/sleeps"
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- "$LADDER"'gh_wait_reset retry_fetch "the viewer" 5 -- gh api graphql -f query="{viewer{login}}"'
reset=$(spent_until)
check "exits 0" "$(is "$status" 0)"
check "prints the answer of the ladder run after the wait" "$(is "$out" '{"title":"t"}')"
check "calls GraphQL six times: the ladder's five, refused, then one after the wait" \
  "$(is "$(calls /graphql)" 6)"
check "reads the reset from the rate_limit route once" "$(is "$(calls /rate_limit)" 1)"
check "the sixth no sooner than the reset" "$(at_least "$(nth_at /graphql 6)" "$reset")"
check "the ladder's four sleeps, then one wait of gh_wait_reset's own" \
  "$(is "$(sed '$d' <<<"$(sleeps)")|$(tail -n 1 <<<"$(sleeps)" | cut -d' ' -f1)" "$ladder_slept|gh_wait_reset")"
check "and no ::error:: outside a block annotates the step" "$(is "$(bare_errors)" 0)"

echo "case: composed, a paginated read is decided by the rate_limit route once, after the ladder"
# A paginated read leaves the helper no answer to read (gh-read.sh's header), so it asks the
# rate_limit route about the core budget once, after the ladder's last attempt exited 1, and not
# after each attempt. The refusal's message names no rate limit, so the route alone tells.
github 60
echo "Forbidden" > "$work/state/spent-message"
rm -f "$work/sleeps"
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- "$LADDER"'gh_wait_reset retry_fetch "the pull request" 5 -- gh api --paginate repos/o/r/pulls/1'
reset=$(spent_until)
check "exits 0 with the answer of the ladder run after the wait" "$(is "$status|$out" '0|{"title":"t"}')"
check "calls the route six times, the sixth no sooner than the reset" \
  "$(is "$(calls /repos/o/r/pulls/1)|$(at_least "$(nth_at /repos/o/r/pulls/1 6)" "$reset")" '6|true')"
check "reads the rate_limit route once, after the ladder" "$(is "$(calls /rate_limit)" 1)"

# A secondary rate limit spends no budget: GitHub's answer names how long to wait in a retry-after
# header. Its documentation asks for at least a minute where the answer names none, but such an
# answer is told apart from any other 403 by its message alone, which the helper does not read
# (the case below).
# secondary <status> <retry-after|none> [always]: this case's GitHub refuses the first route call
# (every route call, with `always`) for a secondary rate limit.
secondary() {
  echo "$1 $2" > "$work/state/secondary"
  [ "${3:-}" != always ] || touch "$work/state/secondary-always"
}
# reply <status> <message> [spent]: this case's GitHub answers every route request that status and
# message, with no rate-limit header, or with `spent` the headers of an answer that spent the last
# of the budget (x-ratelimit-remaining: 0, its reset an hour off).
reply() {
  echo "$1 $2" > "$work/state/reply"
  [ "${3:-}" != spent ] ||
    printf 'x-ratelimit-remaining: 0\nx-ratelimit-reset: %s\n' "$(($(date +%s) + 3600))" > "$work/state/reply-headers"
}
# own_failure <route> <stderr pattern>: the checks of a failure that is gh's own, in a case driven
# with drive_recording_sleep: gh's status, no sleep, one call to the route, no rate_limit read,
# gh's words on stderr, and no line of the helper's own.
own_failure() {
  check "exits as gh does" "$(is "$status" 1)"
  check "with no sleep" "$(is "$(slept)" '')"
  check "calls once" "$(is "$(calls "$1")" 1)"
  check "reads no rate limit" "$(is "$(calls /rate_limit)" 0)"
  check "gh's words reach stderr" "$(contains "$err" "$2")"
  check "and the helper logs nothing of its own" "$(is "$(warnings)|$(errors)" '0|0')"
}

echo "case: a secondary rate limit's retry-after is waited out"
github never
secondary 429 3
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "exits 0" "$(is "$status" 0)"
check "prints the answer of the call after the wait" "$(is "$out" '{"title":"t"}')"
check "calls the route twice" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "reads no rate limit, since no budget is spent" "$(is "$(calls /rate_limit)" 0)"
check "waits the 3 s GitHub asked for, not a minute" \
  "$(is "$(gap_at_least /repos/o/r/pulls/1 3)|$(gap_at_least /repos/o/r/pulls/1 60)" 'true|false')"
check "logs at most one line about the wait, naming the retry-after and the job's limit" \
  "$(logged_wait_names "^::warning::GitHub's secondary rate limit asks this call to wait 3 s (its retry-after), until .*, within this job's limit at ")"
check "gh's own words for the refusal stay in the log, once" \
  "$(is "$(grep -c 'secondary rate limit.* (HTTP 429)$' <<<"$err")" 1)"
check "and none of gh's request trace does" "$(is "$(contains "$err" '^\* Request\|^< \|^> ')" false)"

echo "case: a 403 that names a secondary rate limit, with no retry-after and the budget whole, is gh's own failure"
# Its status and headers are a plain 403's, and the helper decides a rate limit from those alone:
# the message is the server's text. The composed form's ladder, or the caller, decides what comes
# next.
github never
secondary 403 none
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
own_failure /repos/o/r/pulls/1 '^gh: You have exceeded a secondary rate limit.* (HTTP 403)$'

echo "case: a secondary rate limit whose retry-after falls past the limit fails by name at once"
github never
secondary 403 3600
drive_recording_sleep JOB_TIMEOUT_MINUTES=1 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "with no sleep" "$(is "$(slept)" '')"
check "calls once" "$(is "$(calls /repos/o/r/pulls/1)" 1)"
check "naming the retry-after and the limit" \
  "$(contains "$err" "^::error::GitHub's secondary rate limit asks this call to wait 3600 s (its retry-after), until .*, past this job's limit at ")"

echo "case: a secondary rate limit that refuses again after the wait fails by name"
github never
secondary 403 2 always
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "fails" "$(is "$status" 75)"
check "after waiting once" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "naming it" "$(contains "$err" "^::error::GitHub refused this call for its rate limit again after the wait")"

echo "case: a wrapped command's answer is read through the wrapper"
# The gh on PATH while the command runs is the helper's own (gh-read.sh's header), so a wrapper
# that runs gh by name hands the helper the answer of the last gh it ran: the browser lane's bash
# wrapper, and a shell function that runs the command under timeout, as `wrap` here does.
wrap='wrap() { timeout 30 "$@"; };'
github never
secondary 403 3
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset bash -c "gh api repos/o/r/pulls/1"'
check "a retry-after through bash -c is waited out, not a minute" \
  "$(is "$status|$out|$(gap_at_least /repos/o/r/pulls/1 3)|$(gap_at_least /repos/o/r/pulls/1 60)" '0|{"title":"t"}|true|false')"
github 3
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- "$wrap"' gh_wait_reset wrap gh api repos/o/r/pulls/1'
check "a spent budget through a function that runs gh under timeout is waited for" \
  "$(is "$status|$out|$(at_least "$(nth_at /repos/o/r/pulls/1 2)" "$(spent_until)")" '0|{"title":"t"}|true')"
github never
reply 504 "API rate limit exceeded for installation ID 1."
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- "$wrap"' gh_wait_reset wrap gh api repos/o/r/pulls/1'
check "a 504 naming the budget through the function is gh's own failure, with no sleep" \
  "$(is "$status|$(slept)|$(calls /repos/o/r/pulls/1)|$(calls /rate_limit)" '1||1|0')"
check "and gh's words through the function reach stderr" \
  "$(contains "$err" '^gh: API rate limit exceeded for installation ID 1\. (HTTP 504)$')"

echo "case: composed, a secondary rate limit the ladder outlasts takes no wait"
# The stand-in refuses the route's first call for a secondary limit, its retry-after 2 s, and
# answers the next: the ladder's second attempt is answered, so the helper meets no refusal and
# sleeps nothing of its own.
github never
secondary 403 2
rm -f "$work/sleeps"
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- "$LADDER"'gh_wait_reset retry_fetch "the pull request" 5 -- gh api repos/o/r/pulls/1'
check "exits 0" "$(is "$status" 0)"
check "calls the route twice" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "the ladder's first sleep alone, none of gh_wait_reset's own" "$(is "$(sleeps)" 'retry_fetch 10')"
check "and logs no wait" "$(is "$(warnings)" 0)"

# Only the answer decides a rate limit: its status, and its x-ratelimit-remaining and retry-after
# headers, as GitHub's documentation defines each limit. The message is the server's text, which
# a gateway's 504 can carry as well as GitHub's own refusal.

echo "case: a 504 whose message names the API budget is gh's own failure"
github never
reply 504 "API rate limit exceeded for installation ID 1."
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
own_failure /repos/o/r/pulls/1 '^gh: API rate limit exceeded for installation ID 1\. (HTTP 504)$'

echo "case: a 404 that spent the last of the budget is gh's own failure"
# The call that spends the budget's last unit carries x-ratelimit-remaining: 0 with its own
# answer; only a 403, a 429 or GraphQL's 200 is a refusal.
github never
reply 404 "Not Found" spent
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
own_failure /repos/o/r/pulls/1 '^gh: Not Found (HTTP 404)$'

echo "case: a 504 whose message names a secondary rate limit is gh's own failure"
github never
reply 504 "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
own_failure /repos/o/r/pulls/1 '^gh: You have exceeded a secondary rate limit.* (HTTP 504)$'

echo "case: composed, the ladder climbs for a 504 whose message names the API budget, and the helper names no rate limit"
github never
reply 504 "API rate limit exceeded for installation ID 1."
rm -f "$work/sleeps"
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- "$LADDER"'gh_wait_reset retry_fetch "the pull request" 5 -- gh api repos/o/r/pulls/1'
check "fails as gh does" "$(is "$status" 1)"
check "after five attempts, one call each" "$(is "$(calls /repos/o/r/pulls/1)" 5)"
check "sleeping only the ladder's own four waits" "$(is "$(sleeps)|$(slept)" "$ladder_slept|")"
check "reading no rate limit" "$(is "$(calls /rate_limit)" 0)"
check "and naming none: the one ::error:: is the ladder's exhaustion" \
  "$(is "$(warnings)|$(errors)|$(contains "$err" '^::error::the pull request failed to download after 5 attempts')" '0|1|true')"

echo "case: a 403 with x-ratelimit-remaining: 0 is waited for, whatever its message says"
github 3
echo "Forbidden" > "$work/state/spent-message"
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "exits 0 with the answer of the call after the wait" "$(is "$status|$out" '0|{"title":"t"}')"
check "makes the second call no sooner than the reset" "$(at_least "$(nth_at /repos/o/r/pulls/1 2)" "$(spent_until)")"
check "reads the reset from the rate_limit route once" "$(is "$(calls /rate_limit)" 1)"

echo "case: a 403 with a retry-after is waited out, whatever its message says"
github never
secondary 403 3
echo "Forbidden" > "$work/state/secondary-message"
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "exits 0 with the answer of the call after the wait" "$(is "$status|$out" '0|{"title":"t"}')"
check "the second call came the 3 s retry-after after the first" "$(gap_at_least /repos/o/r/pulls/1 3)"
check "reads no rate limit" "$(is "$(calls /rate_limit)" 0)"

echo "case: GraphQL's refusal for its budget, a 200 with x-ratelimit-remaining: 0, is waited for"
github 3
touch "$work/state/graphql-200"
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api graphql -f query="{viewer{login}}"'
check "exits 0 with the answer of the call after the wait" "$(is "$status|$out" '0|{"title":"t"}')"
check "calls GraphQL twice, the second no sooner than the reset" \
  "$(is "$(calls /graphql)|$(at_least "$(nth_at /graphql 2)" "$(spent_until)")" '2|true')"

echo "case: a REST 200 that spent the last of the budget, with gh failing on its own filter, is gh's own failure"
# Only GraphQL answers its refusal with a 200; a REST 200 is an answer, whatever gh then does.
github never
reply 200 "ok" spent
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1 --jq "error(\"filter failed\")"'
own_failure /repos/o/r/pulls/1 'filter failed'
echo "case: the same for the REST route of a repository named graphql"
github never
reply 200 "ok" spent
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/graphql --jq "error(\"filter failed\")"'
own_failure /repos/o/graphql 'filter failed'

echo "case: a redirect gh follows leaves no answer read; the rate_limit route decides"
# The next request's log comes after the redirect's body, so no line of it is surely gh's.
github 3
touch "$work/state/redirect"
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/moved'
check "exits 0 with the answer of the call after the wait" "$(is "$status|$out" '0|{"title":"t"}')"
check "read the spent budget off the rate_limit route, once" "$(is "$(calls /rate_limit)" 1)"
check "followed the redirect each time, and called again no sooner than the reset" \
  "$(is "$(calls /repos/o/r/moved)|$(calls /repos/o/r/pulls/1)|$(at_least "$(nth_at /repos/o/r/pulls/1 2)" "$(spent_until)")" '2|2|true')"
github never
touch "$work/state/redirect"
printf '%s\n' '* Request at 2026-11-23 00:00:00 +0000 UTC' '* Request to http://api.github.localhost/repos/o/r/pulls/1' \
  '< HTTP/1.1 403 Forbidden' '< X-Ratelimit-Remaining: 0' '< Retry-After: 5' 'the body' > "$work/state/redirect-body"
reply 504 "Gateway Timeout"
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/moved'
check "a redirect whose body reads like gh's log of a refusal, to a 504: gh's own failure, no sleep" \
  "$(is "$status|$(slept)|$(warnings)|$(errors)" '1||0|0')"

echo "case: a command whose answer the helper does not read decides by the rate_limit route"
# A paginated read (several requests) and gh's other commands run untraced (gh-read.sh's header),
# so there is no answer of theirs to read; GitHub's rate_limit route, free of the budget, says
# whether the budget the command spends is spent.
github never
reply 504 "API rate limit exceeded for installation ID 1."
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api --paginate repos/o/r/pulls/1'
check "a 504 naming the budget is gh's own failure, with no sleep" \
  "$(is "$status|$(slept)|$(calls /repos/o/r/pulls/1)" '1||1')"
check "after one rate_limit read, which found the budget whole" "$(is "$(calls /rate_limit)" 1)"
check "and the helper logs nothing of its own" "$(is "$(warnings)|$(errors)" '0|0')"
github 3
echo "Forbidden" > "$work/state/spent-message"
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api --paginate repos/o/r/pulls/1'
check "a spent budget whose refusal names no rate limit is waited for" \
  "$(is "$status|$out|$(at_least "$(nth_at /repos/o/r/pulls/1 2)" "$(spent_until)")" '0|{"title":"t"}|true')"
github 3
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset bash -c "gh api --paginate repos/o/r/pulls/1; exit 124"'
check "a command its timeout ended (124) is answered nothing: no rate_limit read, no sleep" \
  "$(is "$status|$(calls /rate_limit)|$(slept)|$(calls /repos/o/r/pulls/1)" '124|0||1')"
# A gh api whose log holds no answer, as a failed connection or a stand-in gh leaves: the gh first
# on PATH here runs the real one with GH_DEBUG unset, so the helper's gh finds no request in the log.
rm -rf "$work/nonebin"
mkdir -p "$work/nonebin"
printf '%s\n' '#!/usr/bin/env bash' "exec env -u GH_DEBUG $(command -v gh) \"\$@\"" > "$work/nonebin/gh"
chmod +x "$work/nonebin/gh"
github 3
drive_on_clock JOB_TIMEOUT_MINUTES=5 PATH="$work/clockbin:$work/nonebin:$PATH" -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "a gh api whose log holds no answer is decided by the rate_limit route too: the spent budget is waited for" \
  "$(is "$status|$out|$(calls /rate_limit)|$(at_least "$(nth_at /repos/o/r/pulls/1 2)" "$(spent_until)")" '0|{"title":"t"}|1|true')"
rm -rf "$work/nonebin"

echo "case: the rate_limit route is read for the budget the command spends, and only where gh ran"
# A paginated REST read spends the core budget: a 404 of its own while the graphql budget is spent
# (another caller spent it) is no refusal of this read. A command that ran no gh by name (git, a
# shell's own exit) spends no budget at all.
github never 404
touch "$work/state/graphql-spent"
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api --paginate repos/o/r/pulls/1'
check "a REST 404 with the graphql budget spent is gh's own failure, with no sleep" \
  "$(is "$status|$(slept)|$(calls /repos/o/r/pulls/1)|$(warnings)|$(errors)" '1||1|0|0')"
check "after one rate_limit read, of the core budget, which was whole" "$(is "$(calls /rate_limit)" 1)"
github never
touch "$work/state/graphql-spent"
drive_recording_sleep JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset bash -c "exit 1"'
check "a command that ran no gh fails as it did, with no rate_limit read and no sleep" \
  "$(is "$status|$(calls /rate_limit)|$(slept)|$(warnings)|$(errors)" '1|0||0|0')"

echo "case: gh run, gh cache and gh workflow spend the core budget alone, and a dispatch is sent once"
# They call the actions API's REST routes. A failure of their own while the graphql budget is spent
# (another caller spent it) is no refusal of theirs: no wait, and so no second dispatch after a
# failed one. The job's limit is two hours, so a wait for the graphql reset an hour off would be
# taken. A `gh workflow run` that names no --ref asks GraphQL for the default branch first, so a
# refusal of that lookup is waited for.
github never 404
touch "$work/state/graphql-spent"
drive_recording_sleep JOB_TIMEOUT_MINUTES=120 -- 'gh_wait_reset gh run list --repo o/r --workflow ci.yaml --limit 1'
check "a gh run list 404 with the graphql budget spent is gh's own failure, with no sleep" \
  "$(is "$status|$(slept)|$(calls /repos/o/r/actions/workflows/ci.yaml)|$(calls /rate_limit)|$(warnings)|$(errors)" '1||1|1|0|0')"
github never 404
touch "$work/state/graphql-spent"
drive_recording_sleep JOB_TIMEOUT_MINUTES=120 -- 'gh_wait_reset gh cache list --repo o/r --limit 1'
check "a gh cache list 404 with the graphql budget spent is gh's own failure, with no sleep" \
  "$(is "$status|$(slept)|$(calls /repos/o/r/actions/caches)|$(calls /rate_limit)|$(warnings)|$(errors)" '1||1|1|0|0')"
github never 404
touch "$work/state/graphql-spent"
drive_recording_sleep JOB_TIMEOUT_MINUTES=120 -- 'gh_wait_reset gh cache delete 123 --repo o/r'
check "a gh cache delete 404 with the graphql budget spent is gh's own failure, with no sleep" \
  "$(is "$status|$(slept)|$(calls /repos/o/r/actions/caches/123)|$(calls /rate_limit)|$(warnings)|$(errors)" '1||1|1|0|0')"
dispatch=/repos/o/r/actions/workflows/7/dispatches
github never 404
touch "$work/state/graphql-spent" "$work/state/workflow"
drive_recording_sleep JOB_TIMEOUT_MINUTES=120 -- 'gh_wait_reset gh workflow run ci.yaml --repo o/r --ref main -f migration-base=abc'
check "a gh workflow run --ref whose dispatch fails on its own (404), the graphql budget spent: one dispatch, no sleep" \
  "$(is "$status|$(slept)|$(calls "$dispatch")|$(calls /graphql)|$(warnings)|$(errors)" '1||1|0|0|0')"
github never
touch "$work/state/graphql-spent" "$work/state/workflow"
drive_recording_sleep JOB_TIMEOUT_MINUTES=120 -- 'gh_wait_reset gh workflow run ci.yaml --repo o/r --ref main -f migration-base=abc'
check "a gh workflow run --ref that is answered dispatches once, and reads no rate limit" \
  "$(is "$status|$(slept)|$(calls "$dispatch")|$(calls /rate_limit)" '0||1|0')"
github never
touch "$work/state/graphql-spent" "$work/state/workflow"
drive_recording_sleep JOB_TIMEOUT_MINUTES=120 -- 'gh_wait_reset gh workflow run ci.yaml --repo o/r -f migration-base=abc'
check "a gh workflow run with no --ref, its default-branch lookup refused for the spent graphql budget: waits once for that reset, and dispatches nothing" \
  "$(is "$(slept | awk '$1 >= 3000 { n++ } END { print n + 0 }')|$(warnings)|$(calls /graphql)|$(calls "$dispatch")" '1|1|2|0')"

echo "case: in a Legion pane, whose gh refuses unless it is the first gh on PATH, the call goes through"
# A pane's gh (`legion gh`) exits 2 when the first gh on PATH is not its own wrapper. The helper's
# gh takes its own directory off PATH before it runs the real one; the gh first on PATH here does
# what the pane's does.
rm -rf "$work/panebin"
mkdir -p "$work/panebin"
printf '%s\n' '#!/usr/bin/env bash' \
  '[ "$(type -P gh)" = "$0" ] || { echo "the pane gh: the first gh on PATH is $(type -P gh), not this wrapper" >&2; exit 2; }' \
  "exec $(command -v gh) \"\$@\"" > "$work/panebin/gh"
chmod +x "$work/panebin/gh"
github never
drive JOB_TIMEOUT_MINUTES=5 PATH="$work/panebin:$PATH" -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "a call through the pane's gh is answered" "$(is "$status|$out|$err" '0|{"title":"t"}|')"
github never
drive JOB_TIMEOUT_MINUTES=5 PATH="$work/panebin:$PATH" -- 'gh_wait_reset bash -c "gh api --paginate repos/o/r/pulls/1"'
check "and so is one through bash -c, gh's answer unread" "$(is "$status|$out|$err" '0|{"title":"t"}|')"
rm -rf "$work/panebin"

# left_of <token>: the processes still running whose command line carries the token. It reads
# /proc with builtins alone: a `tr` forked per process took 120 s over this dev box's 22,000
# processes, longer than the 30 s hold an orphaned gh outlives the helper by, so the scan missed
# it; this one takes about half a second there.
left_of() {
  local proc left='' args=()
  for proc in /proc/[0-9]*; do
    mapfile -d '' args 2> /dev/null < "$proc/cmdline" || continue
    [[ " ${args[*]} " != *"$1"* ]] || left+="${proc#/proc/} "
  done
  echo "$left"
}

echo "case: a TERM, an INT or a HUP sent to the helper's gh alone (timeout --foreground) reaches the real gh"
# The helper's gh runs the real one as a child for `gh api`, so it passes the signal on and waits.
# The route holds its answer 30 s, and the stand-in logs the request before that hold, so one call
# logged shows the real gh had started and sent it before the signal came, 5 s in: the trap's
# forwarding is then the only thing that ends it before the hold does. The case names its request
# with a header no other process carries, and looks for it among the processes left once the
# helper has returned; -k 10 makes a helper's gh that does not end on the signal a KILL's 137.
for signal in TERM INT HUP; do
  github never
  echo 30 > "$work/state/route-delay"
  token="case-$(date +%s%N)"
  drive JOB_TIMEOUT_MINUTES=5 -- "gh_wait_reset timeout --foreground -s $signal -k 10 5 gh api repos/o/r/pulls/1 -H 'X-Case: $token'"
  check "$signal sent: the real gh had sent its request, the call exits 124 (not the KILL's 137), and no process of it is left" \
    "$(is "$(calls /repos/o/r/pulls/1)|$status|$(left_of "$token")" '1|124|')"
done

echo "case: the real gh starts with INT and QUIT at their default, not ignored"
# A background simple command starts with both ignored, and gh keeps an ignored INT until it
# installs a handler of its own, so an INT that comes first would be lost; the helper's gh starts gh
# from a subshell, whose exec does not ignore them. The gh first on PATH here records the signals it
# was started with ignored, as a process's SigIgn mask, which an exec keeps, and then runs the real
# gh; the helper takes it for the real gh.
rm -rf "$work/maskbin"
mkdir -p "$work/maskbin"
printf '%s\n' '#!/usr/bin/env bash' "awk '/^SigIgn:/ { print \$2 }' /proc/self/status > \"$work/sigign\"" \
  "exec $(command -v gh) \"\$@\"" > "$work/maskbin/gh"
chmod +x "$work/maskbin/gh"
github never
drive JOB_TIMEOUT_MINUTES=5 PATH="$work/maskbin:$PATH" -- 'gh_wait_reset gh api repos/o/r/pulls/1'
sigign=$(cat "$work/sigign" 2> /dev/null)
check "the call was answered through the recording gh" "$(is "$status|$out" '0|{"title":"t"}')"
check "and its mask was read" "$([[ $sigign =~ ^[0-9a-f]+$ ]] && echo true || echo false)"
check "with neither INT (bit 2) nor QUIT (bit 3) ignored" \
  "$([[ $sigign =~ ^[0-9a-f]+$ ]] && (( (16#$sigign & 0x6) == 0 )) && echo true || echo false)"
rm -rf "$work/maskbin" "$work/sigign"

echo "case: a KILL that ends the command leaves nothing of the helper's in TMPDIR"
# The helper's work directory holds the request's log, and it removes that directory itself.
github never
echo 30 > "$work/state/route-delay"
rm -rf "$work/tmpcase"
mkdir "$work/tmpcase"
drive JOB_TIMEOUT_MINUTES=5 TMPDIR="$work/tmpcase" -- 'gh_wait_reset timeout -s KILL 1 gh api repos/o/r/pulls/1'
check "exits 137, timeout's status for a KILL" "$(is "$status" 137)"
check "and TMPDIR is empty" "$(is "$(ls -A "$work/tmpcase")" '')"
rm -rf "$work/tmpcase"

# gh release view, edit and upload send a GraphQL lookup and a REST request together, so both are
# open at once. The helper reads gh's request trace (GH_DEBUG=api) for `gh api` alone, which has
# one request open at a time: a trace of two open requests let the second answer's headers and
# body through to the log, a draft release's name and body among them, ahead of release-push.sh's
# leak guard.
echo "case: gh release view through the helper prints the release and nothing on stderr"
github never
touch "$work/state/release"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh release view v1 --repo o/r --json name,body -q ".name, .body"'
check "exits 0" "$(is "$status" 0)"
check "prints the name and the body on stdout" "$(is "$out" "the draft's name
the draft's body, before the leak guard")"
check "and nothing on stderr: no answer, no header, no trace" "$(is "$err" '')"
check "both requests were made, the REST answer closing second" \
  "$(is "$(calls /graphql)|$(calls /repos/o/r/releases/tags/v1)" '1|1')"

echo "case: gh release edit through the helper writes nothing on stderr"
github never
touch "$work/state/release"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh release edit v1 --repo o/r --draft=false'
check "exits 0" "$(is "$status" 0)"
check "and nothing on stderr" "$(is "$err" '')"
check "it patched the release" "$(is "$(calls /repos/o/r/releases/1)" 1)"

echo "case: gh release view under a spent budget waits, and the log holds no release"
github 10
touch "$work/state/release"
drive_on_clock JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh release view v1 --repo o/r --json name,body -q ".name, .body"'
check "exits 0" "$(is "$status" 0)"
check "prints the release on stdout" "$(contains "$out" "^the draft's name$")"
check "the second run came no sooner than the reset" "$(at_least "$(nth_at /graphql 2)" "$(spent_until)")"
check "and logged at most the one wait, naming the reset" "$(logged_wait_names "^::warning::the API budget is spent until $(utc "$(spent_until)");")"
check "with nothing of the release on stderr" "$(is "$(contains "$err" "the draft's")" false)"
check "and no line of a trace" "$(is "$(contains "$err" '^\* Request\|^< \|^> \|^{')" false)"

echo "case: gh release view under a secondary rate limit: the stand-in refuses one of its two requests, not both"
# gh sends the GraphQL lookup and the REST read together, so both requests reach the secondary
# step at once. The hold-secondary knob holds the first until the second has made the same check:
# under the lock the second waits, finds the refusal given and is answered; without it both would
# be refused. Which of the two takes the lock first is the threads' race, so what gh then does is
# not this case's to fix: when the GraphQL lookup is refused gh reads the release from the REST
# answer, and when the REST read is refused the helper has no answer of gh release view's to read
# and the rate_limit route finds the budget whole, so gh's failure is the helper's.
github never
touch "$work/state/release"
secondary 403 3
date +%s%N > "$work/state/hold-secondary"
drive JOB_TIMEOUT_MINUTES=1 -- 'gh_wait_reset gh release view v1 --repo o/r --json name,body -q ".name, .body"'
check "the stand-in refused one request for the secondary limit, not both" "$(is "$(refusals)" 1)"
check "both requests reached it" "$(is "$(calls /graphql)|$(calls /repos/o/r/releases/tags/v1)" '1|1')"

# A raw answer is printed into gh's request trace as it came, and a line of it can read like gh's
# own end of a request. The trace of one request runs from its `* Request at` line to its last
# `* Request took` line, so no line of the body can end it early; a paginated read, several
# requests in turn, is not traced at all.
echo "case: a raw answer's line that reads like gh's own end of a request does not end the trace"
github never
printf '%s\n' "a raw file's first line" '* Request took 1.5ms' 'a raw line after the forged end of the request' \
  > "$work/state/raw"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/raw -H "Accept: application/vnd.github.raw"'
check "exits 0" "$(is "$status" 0)"
check "prints the raw answer on stdout, whole" "$(contains "$out" '^a raw line after the forged end of the request$')"
check "and nothing on stderr" "$(is "$err" '')"

echo "case: a paginated raw read through the helper writes nothing on stderr"
github never
printf '%s\n' "a raw file's first line" '* Request took 1.5ms' 'a raw line after the forged end of the request' \
  > "$work/state/raw"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api --paginate repos/o/r/raw -H "Accept: application/vnd.github.raw"'
check "exits 0" "$(is "$status" 0)"
check "prints the raw answer on stdout, whole" "$(contains "$out" '^a raw line after the forged end of the request$')"
check "and nothing on stderr" "$(is "$err" '')"

echo "case: the wait is measured from after the rate_limit read, not before it"
# The rate_limit route takes 5 s to answer a reset 3 s after the refusal: the budget is back by the
# time the answer arrives, so the call goes again at once and logs no wait. A clock read before the
# probe would see the reset still ahead and sleep for it, logging the wait. The 5 s are real, so the
# helper's own bound on that read (timeout 30) meets a slow answer too.
github 3
echo 5 > "$work/state/rate-limit-delay"
drive JOB_TIMEOUT_MINUTES=5 -- 'gh_wait_reset gh api repos/o/r/pulls/1'
check "exits 0" "$(is "$status" 0)"
check "calls the route twice" "$(is "$(calls /repos/o/r/pulls/1)" 2)"
check "logs no wait: the reset passed while the rate-limit read answered" "$(is "$(warnings)" 0)"

echo "case: composed, with no limit set the ladder still makes its retries for a rate limit"
# With no limit the helper cannot wait; the ladder inside it makes its later attempts all the same,
# so a budget back by the third attempt is read there and the helper meets no refusal. The stand-in
# refuses the route's first two calls for the spent budget, its reset an hour off, and answers the
# third: refusals counted, not a reset the attempts must straddle, so a slow host changes nothing.
github never
echo 2 > "$work/state/refuse-calls"
rm -f "$work/sleeps"
drive -- "$LADDER"'gh_wait_reset retry_fetch "the pull request" 5 -- gh api repos/o/r/pulls/1'
check "exits 0" "$(is "$status" 0)"
check "prints the answer" "$(is "$out" '{"title":"t"}')"
check "after three attempts" "$(is "$(calls /repos/o/r/pulls/1)" 3)"
check "the ladder's first two sleeps, and none of gh_wait_reset's own" \
  "$(is "$(sleeps)" $'retry_fetch 10\nretry_fetch 20')"
check "and nothing named the missing limit: the helper met no refusal" \
  "$(is "$(contains "$err" 'names no time limit to wait within')" false)"

echo "case: a rate-limit read that overlaps the first spend reads the reset whole"
# The hold-spend-read knob makes a rate_limit read and the first spend's write overlap: the read
# holds until the spend's request has arrived and opened spent-until, and the spend holds the
# opened, empty file until the read is done. Under the lock the read comes before the spend can
# open the file; read outside it, it would find the file empty, raise, and answer nothing (an EOF
# to gh). The case starts the spend once the stand-in has logged the read, not after a pause.
github 10
date +%s%N > "$work/state/hold-spend-read"
cat > "$work/overlap.sh" <<'SH'
gh api rate_limit --jq .resources.core.remaining > limit 2> limit.err &
reader=$!
for _ in $(seq 600); do
  grep -q ' /rate_limit$' state/log && break
  sleep 0.05
done
gh api repos/o/r/pulls/1 > /dev/null 2>&1
echo "spend=$?"
wait "$reader"
echo "read=$? limit=$(cat limit)"
SH
drive -- 'bash "$PWD/overlap.sh"'
check "the rate-limit read is answered" "$(contains "$out" '^read=0 limit=[0-9][0-9]*$')"
check "and the spend's call is refused for the spent budget" "$(contains "$out" '^spend=1$')"
check "both reached the stand-in" "$(is "$(calls /rate_limit)|$(calls /repos/o/r/pulls/1)" '1|1')"
rm -f "$work/overlap.sh" "$work/limit" "$work/limit.err"

echo "case: an error of the stand-in's own reaches the log as its traceback"
# The stand-in drops one kind of error alone: a ConnectionError, its write into a socket gh
# closed. Any other error is a fault of its own, and it prints the traceback, so a case the fault
# breaks shows why beside it; a fault that printed nothing left gh's EOF as its only trace (run
# 45066092231: an int('') on a half-written spent-until). A second stand-in, its stderr kept in a
# file, takes a spend that is no number. The server prints an error before it closes the
# connection, so the traceback is in the file by the time the request has ended.
mkdir -p "$work/fault-state"
echo 'not a number' > "$work/fault-state/spend-for"
stand_in "$work/fault-state" "$work/fault-port" "$work/fault-err"
answer=$(python3 - "$(<"$work/fault-port")" <<'PY'
import http.client
import sys

try:
    connection = http.client.HTTPConnection("127.0.0.1", int(sys.argv[1]), timeout=30)
    connection.request("GET", "/repos/o/r/pulls/1")
    print(connection.getresponse().status)
except http.client.RemoteDisconnected:
    print("no answer")
except OSError as error:
    print(f"error: {error}")
PY
)
check "the stand-in answers nothing" "$(is "$answer" 'no answer')"
check "and prints a traceback that names the fault" \
  "$(contains "$(<"$work/fault-err")" "^ValueError: invalid literal for int() with base 10: 'not a number'$")"

echo "case: a socket closed under the stand-in's answer prints nothing"
# gh drops the slower of two requests it raced, and the stand-in's write into that socket raises a
# ConnectionError, which is no fault and prints nothing. A third stand-in, its stderr kept in a
# file, holds a release's answer release-delay seconds. The client resets its connection once the
# stand-in has logged the request, a second before the answer is due, so the answer is written
# into a reset socket. The stand-in's thread count back at idle says the request has ended, and
# with it any traceback the stand-in would print, before the file is read.
mkdir -p "$work/drop-state"
touch "$work/drop-state/release"
echo 1 > "$work/drop-state/release-delay"
: > "$work/drop-state/log"
stand_in "$work/drop-state" "$work/drop-port" "$work/drop-err"
threads() { awk '$1 == "Threads:" { print $2 }' "/proc/${servers[-1]}/status"; }
idle=$(threads)
closed=$(python3 - "$(<"$work/drop-port")" "$work/drop-state/log" <<'PY'
import socket
import struct
import sys
import time

port, log = int(sys.argv[1]), sys.argv[2]
try:
    client = socket.create_connection(("127.0.0.1", port), timeout=30)
    client.sendall(b"GET /repos/o/r/releases/tags/v1 HTTP/1.1\r\nHost: api.github.localhost\r\n\r\n")
    for _ in range(600):
        with open(log) as handle:
            if any(line.split()[2:] == ["/repos/o/r/releases/tags/v1"] for line in handle):
                break
        time.sleep(0.05)
    else:
        sys.exit("the stand-in logged no request")
    client.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
    client.close()
    print("reset")
except OSError as error:
    print(f"error: {error}")
PY
) || true
for _ in $(seq 600); do
  [ "$(threads)" -le "$idle" ] && break
  sleep 0.05
done
check "the client resets its request once the stand-in has read it" "$(is "$closed" reset)"
check "the stand-in ends the request" "$(is "$(threads)" "$idle")"
check "and prints nothing for the closed socket" "$(is "$(<"$work/drop-err")" '')"

echo "case: on a GitHub runner, the job's start is read off the real Runner.Worker"
if [ "${GITHUB_ACTIONS:-}" = true ]; then
  # This harness's step runs inside ci.yaml's test job, whose limit is 30 minutes, so a job still
  # running started less than 30 minutes ago and no later than now.
  now=$(date +%s)
  if deadline=$(JOB_TIMEOUT_MINUTES=30 bash -c 'source "$1"; _gh_read_deadline' _ "$script_dir/gh-read.sh"); then
    deadline=${deadline%% *}
    check "finds this job's Runner.Worker" true
    check "whose start is no later than now" "$(at_least "$((now + 1))" "$((deadline - 1800))")"
    check "and less than the job's 30 minutes ago" "$(at_least "$((deadline - 1800))" "$((now - 1800))")"
  else
    check "finds this job's Runner.Worker ($deadline)" false
  fi
else
  echo "  SKIP: not on a GitHub runner (GITHUB_ACTIONS is not true)"
fi

summary "gh-read.sh's wait for a spent API budget, alone and in front of retry_fetch's ladder"
