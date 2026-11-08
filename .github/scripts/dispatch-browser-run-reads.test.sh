#!/usr/bin/env bash
# Tests for `dispatch-browser-run-reads.sh`, the two reads dispatch-browser.yaml's merge job
# makes of its own run: the jobs of this attempt, which the merge job's readings name legs from,
# and the line that says a blob download could not be completed.
#
# Each case writes the one slurped page a stand-in `gh` answers with - the array of page objects
# `gh api --paginate --slurp` prints - and how many calls that stand-in refuses first, then runs
# the real script with the stand-in first on PATH and reads the file it wrote, the arguments the
# stand-in recorded and the lines the script printed. The ladder is the `retry_fetch` of
# .github/scripts/ci-retry.sh: five attempts, with waits of 10, 20, 40 and 80 s between them,
# and no knob that shortens them. Those waits are ci-retry.test.sh's to prove, by measuring
# them; paying for them here would add 150 s to every case that exhausts the ladder, so `sleep`
# is a no-op stand-in on this PATH, as the retry delay this file used to set to zero was. It
# records the seconds each call asked for, so a case still reads which waits the ladder made
# without waiting them, and one case reads bash's own SECONDS to hold that a drive of five
# refused attempts costs no time at all - which is where a lost stand-in shows up as a red
# check rather than as a suite that takes two and a half minutes longer.
#
# NO CASE REACHES THE NETWORK: every case is files, a stand-in gh, a stand-in `sleep`, a
# stand-in `timeout` that four cases opt into call by call and that otherwise execs the real
# one, and the script.
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

mkdir -p "$work/bin" "$work/calls"
cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
# The stand-in. It records the whole argument list of every call, one file per call, so a case
# can hold the route to the attempt endpoint, and answers from the files the case wrote: it
# refuses its first \`refuse\` calls with \`stderr\` on its own stderr and prints \`page.json\`
# for the calls after those. A case whose attempts are refused for different reasons writes
# \`stderr.<n>\` for the n-th call, counting from 0, and that file answers in place of
# \`stderr\` for that call alone.
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
set -euo pipefail
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
if [ "\$((calls + 1))" -le "\$(cat "$work/refuse")" ]; then
  if [ -f "$work/stderr.\$calls" ]; then
    cat "$work/stderr.\$calls" >&2
  else
    cat "$work/stderr" >&2
  fi
  exit 1
fi
cat "$work/page.json"
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
# killed wrapper go red: 6 checks fail, four in the first and two in the second. Two of those
# reds name the staging, and none of them names the host this was run on, which is what this
# line is for.
[ -r /proc/self/stat ] ||
  { echo "dispatch-browser-run-reads.test.sh needs /proc (Linux) for the cases that stage a" \
      "killed wrapper" >&2
    exit 1; }

# The stand-in `timeout`, beside the stand-in `gh` and opted into per call. Each attempt of the
# ladder calls `timeout` twice, because the attempt carries two bounds: the ladder's own 40 s
# around the wrapper, and the wrapper's `timeout 30` around gh. This stand-in records the whole
# argument list of both, one file per call, whether or not it stands in for that call, so
# `timeout_calls` and `ladder_args` read the ladder's bound and `gh_bounds` and `gh_bound_args`
# read the wrapper's, in every drive in this file.
#
# Standing in is what is opt-in, and the reason is not the recording: a stand-in every case
# used would still catch a dropped bound through `timeout_calls`. It is that a call it stands
# in for exits at once without running gh - with 124, which is what the real timeout exits
# when its bound fires, or with whatever status the case asked for instead - and every other
# case in this file needs gh's answer: the page it writes, the stderr of the calls it refuses,
# the arguments it records.
#
# It stands in for the wrapper's bound alone, never the ladder's, and the wrapper's is the one
# the cases are about: a 124 the wrapper's own `timeout` returns is what writes 124 to that
# attempt's status file, which is the only thing describe_attempt reads the bound off. The
# call that is stood in for is picked by the command being bounded - `gh` is the wrapper's
# bound and `bash` the ladder's - so a case asks for the bound of attempt n + 1 by writing
# `$work/stub-timeout.<n>`, counting gh's bounds from 0. There is one of those per attempt,
# whether this stand-in answers it or execs the real timeout, so `stub-timeout.<n>` names
# attempt n + 1 and nothing drifts. An empty file is the bound firing, which is 124, and a
# file holding a status is answered with that status instead, which is how the case about an
# attempt exiting 127 is staged.
#
# The stand-in gh's `stderr.<n>` counts from 0 too and counts something else - gh's own calls,
# which hold no call for an attempt this stand-in answered itself. So `stderr.<n>` names gh's
# own n-th call rather than attempt n + 1, and in a case that mixes the two the two
# numberings drift apart. `fixture` removes those files again, and a call no file was written
# for runs the real timeout this execs, so that attempt's gh is bounded as the script asked.
cat > "$work/bin/timeout" <<TIMEOUT
#!/usr/bin/env bash
set -euo pipefail
calls=\$(find "$work/timeout-calls" -maxdepth 1 -type f | wc -l)
printf '%s\n' "\$*" > "$work/timeout-calls/call.\$calls"
if [ "\${2:-}" != gh ]; then
  exec $real_timeout "\$@"
fi
bounds=\$(find "$work/gh-bounds" -maxdepth 1 -type f | wc -l)
printf '%s\n' "\$*" > "$work/gh-bounds/call.\$bounds"
if [ ! -f "$work/stub-timeout.\$bounds" ]; then
  exec $real_timeout "\$@"
fi
answered=\$(cat "$work/stub-timeout.\$bounds")
exit "\${answered:-124}"
TIMEOUT
chmod +x "$work/bin/timeout"

# The stand-in `sleep`: the ladder's four waits, 150 s of them, return at once, and each call's
# seconds are recorded so that `waited` reads the ladder a case drove without paying for it.
# The head says why the waits themselves are not this file's to measure. `sleep` is no bash
# builtin, so this file on PATH is the one the helper runs.
cat > "$work/bin/sleep" <<SLEEP
#!/usr/bin/env bash
set -euo pipefail
calls=\$(find "$work/sleeps" -maxdepth 1 -type f | wc -l)
printf '%s\n' "\$*" > "$work/sleeps/sleep.\$calls"
SLEEP
chmod +x "$work/bin/sleep"

# page <id>|<name>|<conclusion>...: the page the stand-in answers with. `--slurp` collects the
# pages into one array, so the answer is an array of page objects and each holds a `jobs` array.
# `run_attempt` is in each job because the jobs API puts it there; nothing in the script reads
# it any more, which is what the entries this writes are held against.
page() {
  jq -n '[{total_count: ($ARGS.positional | length),
    jobs: [$ARGS.positional[] | split("|")
      | {id: (.[0] | tonumber), name: .[1],
         conclusion: (if .[2] == "null" then null else .[2] end), run_attempt: 2}]}]' \
    --args "$@" > "$work/page.json"
}

# happy_page: one attempt of a run of this lane, as the jobs API names it. A called workflow's
# job is named `<the caller's job> / <this job>`, with its matrix leg in brackets after it, and
# the caller's own jobs are in the same list: `lint` and `dispatch-multiprocess`, the second of
# which the name filter keeps out only by the hyphen before `multiprocess`.
happy_page() {
  page "11|dispatch-browser / shard (1)|success" \
    "12|dispatch-browser / shard (2)|failure" \
    "13|dispatch-browser / alone (webkit-alone)|success" \
    "14|dispatch-browser / multiprocess|skipped" \
    "15|dispatch-browser / merge|null" \
    "16|lint|success" \
    "17|dispatch-multiprocess|success" \
    "18|dispatch-browser / visual|failure"
}

# fixture <refused calls> [<stderr of a refused call>]: a fresh call log and no out-file, a
# stand-in that refuses that many calls and then answers, and the happy page for the answer.
# Any per-call `stderr.<n>` an earlier case wrote goes with it, so one case's faults never
# answer another's call, and so do the stand-in `timeout`'s per-call `stub-timeout.<n>` files
# and its two call logs, the stand-in gh's per-call `kill-wrapper.<n>` files and the stand-in
# `sleep`'s record of the waits the last case's ladder made.
fixture() {
  rm -rf "$work/calls" "$work/out.json" "$work/timeout-calls" "$work/gh-bounds" "$work/sleeps"
  rm -f "$work"/stderr.* "$work"/stub-timeout.* "$work"/kill-wrapper.*
  mkdir -p "$work/calls" "$work/timeout-calls" "$work/gh-bounds" "$work/sleeps"
  printf '%s\n' "${1:-0}" > "$work/refuse"
  printf '%s\n' "${2:-HTTP 502}" > "$work/stderr"
  happy_page
}

# drive <argument>...: the real script with the stand-in first on PATH. Sets `out` to its
# combined output, `status` to its exit code and `seconds` to how long the drive took. Not a
# subshell, so all three survive the call.
drive() {
  local started=$SECONDS
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
    GITHUB_RUN_ATTEMPT=$run_attempt "$read_script" "$@" 2>&1)
  status=$?
  set -e
  seconds=$((SECONDS - started))
}

calls() { find "$work/calls" -maxdepth 1 -type f | wc -l | tr -d ' '; }
args() { cat "$work/calls/call.0"; }
timeout_calls() { find "$work/timeout-calls" -maxdepth 1 -type f | wc -l | tr -d ' '; }
ladder_args() { cat "$work/timeout-calls/call.0"; }
gh_bounds() { find "$work/gh-bounds" -maxdepth 1 -type f | wc -l | tr -d ' '; }
gh_bound_args() { cat "$work/gh-bounds/call.0"; }
sleeps() { find "$work/sleeps" -maxdepth 1 -type f | wc -l | tr -d ' '; }
# waited: the seconds the ladder asked `sleep` for, in the order it asked, as one line.
waited() {
  local n total said=""
  total=$(sleeps)
  for ((n = 0; n < total; n++)); do said="$said $(cat "$work/sleeps/sleep.$n")"; done
  echo "${said# }"
}
entries() { jq -r 'length' "$work/out.json"; }
field() { jq -r --argjson id "$1" ".[] | select(.id == \$id) | $2" "$work/out.json"; }
# Every `::error::` line the drive printed. A refused jobs read prints two: the ladder's own
# exhaustion line, which ci-retry.sh writes and ci-retry.test.sh holds, and the script's
# refusal line, which is the one that names the route and the attempt that decided the failure.
errors() { grep -c '::error::' <<<"$out" || true; }
# The script's own line of those two: the ladder's is the one that says it failed to download.
error_line() { grep '::error::' <<<"$out" | grep -v 'failed to download after' || true; }
wrote_out_file() { [ -e "$work/out.json" ] && echo yes || echo no; }
at_least() { [ "$1" -ge "$2" ] && echo true || echo false; }
at_most() { [ "$1" -le "$2" ] && echo true || echo false; }

echo "case: this attempt's jobs become the four fields the readings consume"
fixture 0
drive jobs "$work/out.json"
check "exits 0" "$(is "$status" 0)"
check "writes this workflow's six jobs and none of the caller's" "$(is "$(entries)" 6)"
check "each entry holds the four fields and no other" \
  "$(is "$(jq -r '.[0] | keys | join(",")' "$work/out.json")" "conclusion,id,leg,name")"
check "a shard job's id, name and conclusion travel unchanged" \
  "$(is "$(field 12 '"\(.id) \(.name) \(.conclusion)"')" '12 dispatch-browser / shard (2) failure')"
check "the alone job's too" \
  "$(is "$(field 13 '"\(.id) \(.name) \(.conclusion)"')" \
    '13 dispatch-browser / alone (webkit-alone) success')"
check "the first shard leg reads shard-1" "$(is "$(field 11 .leg)" shard-1)"
check "the second reads shard-2" "$(is "$(field 12 .leg)" shard-2)"
check "the alone leg reads alone-webkit-alone" "$(is "$(field 13 .leg)" alone-webkit-alone)"
check "the multiprocess job carries a null leg" "$(is "$(field 14 .leg)" null)"
check "and keeps its conclusion" "$(is "$(field 14 .conclusion)" skipped)"
check "the merge job carries a null leg" "$(is "$(field 15 .leg)" null)"
check "and its conclusion, which is null while it runs" "$(is "$(field 15 .conclusion)" null)"
check "the visual job is kept, with a null leg" "$(is "$(field 18 .leg)" null)"
check "and keeps its conclusion, so reading three can name it when it is red" \
  "$(is "$(field 18 .conclusion)" failure)"
check "the caller's own jobs are dropped" \
  "$(is "$(jq -r '[.[] | select(.id == 16 or .id == 17)] | length' "$work/out.json")" 0)"
check "prints no ::error:: line" "$(is "$(errors)" 0)"

echo "case: the route is this run's attempt endpoint"
# The same drive: what the stand-in recorded is what the script asked GitHub for.
check "names this run's attempt" "$(contains "$(args)" "/attempts/$run_attempt/jobs")"
check "under this repository and run" "$(contains "$(args)" 'repos/o/r/actions/runs/41/')"
check "a hundred to a page" "$(contains "$(args)" 'jobs?per_page=100')"
check "and holds no filter=all" "$(is "$(contains "$(args)" 'filter=all')" false)"
check "slurps the pages, since gh refuses --slurp beside --jq" \
  "$(contains "$(args)" '--paginate --slurp')"

echo "case: a project name holding a digit, a capital and a space is one leg"
# The capture takes any project that holds no `)`. It read `[a-z-]*` once, which silently
# dropped a project named with a digit or a capital, and a leg absent from reading one's list
# is a leg whose missing blob nothing reports.
fixture 0
page "21|dispatch-browser / alone (Edge 2 alone)|success"
drive jobs "$work/out.json"
check "exits 0" "$(is "$status" 0)"
check "reads one leg, not three" "$(is "$(entries)" 1)"
check "holding the project whole" "$(is "$(field 21 .leg)" 'alone-Edge 2 alone')"

echo "case: every attempt refused with HTTP 502"
fixture 5 "HTTP 502"
drive jobs "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "tried five times" "$(is "$(calls)" 5)"
check "prints its own ::error:: line and the ladder's, and no third" "$(is "$(errors)" 2)"
check "naming the endpoint it read" \
  "$(contains "$(error_line)" 'GET repos/o/r/actions/runs/41/attempts/4/jobs?per_page=100')"
check "the status gh last printed" "$(contains "$(error_line)" 'HTTP 502')"
check "the ladder's attempts and this run attempt" \
  "$(contains "$(error_line)" '5 attempts, all on run attempt 4')"
check "and that this is a failed read and not a failed row" \
  "$(contains "$(error_line)" 'failed read of the jobs API and not a failed browser row')"
check "while the ladder's own line says it exhausted the five" \
  "$(contains "$out" "^::error::this run's attempt's jobs failed to download after 5 attempts")"
check "every attempt's gh output still reaches the log" \
  "$(is "$(grep -c '^HTTP 502$' <<<"$out" || true)" 5)"
check "and the ladder waited 10, 20, 40 and 80 s between them" "$(is "$(waited)" '10 20 40 80')"
# Where a lost stand-in `sleep` shows up: those four waits are 150 s, and this drive pays none
# of them. The bound is an upper one because SECONDS counts whole seconds and a drive that
# straddles a tick reads 1 however short it was.
check "and the drive waited none of those 150 s" "$(at_most "$seconds" 2)"

echo "case: a refusal gh printed no HTTP status for"
# A resolution failure prints no status line, and a line that named one anyway would send a
# reader looking for a 502 that never happened.
fixture 5 "could not resolve host: api.github.com"
drive jobs "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "prints its own ::error:: line and the ladder's, and no third" "$(is "$(errors)" 2)"
check "which names the exit status and says gh printed none" \
  "$(contains "$(error_line)" 'exited 1, printing no HTTP status')"
check "and names no status of its own" \
  "$(is "$(contains "$(error_line)" 'HTTP [0-9][0-9][0-9]')" false)"
check "while gh's own words reach the log" \
  "$(contains "$out" '^could not resolve host: api.github.com$')"

echo "case: an earlier attempt answered a status and the last attempt did not"
# The ladder's five attempts need not fail for one reason: during the 2026-11-08 incident the
# jobs API answered 502 and GitHub's hosts were also refusing connections, and a refused
# connection prints no status at all. The line reports the attempt that decided the failure,
# which is the last one, so it must not hand that attempt a status an earlier attempt
# answered: a reader sent to GitHub's API health by a 502 that did not decide the failure is
# exactly the mis-reading this lane's messages exist to end.
fixture 5 "error connecting to api.github.com"
printf '%s\n' 'gh: Server Error (HTTP 502)' > "$work/stderr.0"
drive jobs "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "tried five times" "$(is "$(calls)" 5)"
check "prints its own ::error:: line and the ladder's, and no third" "$(is "$(errors)" 2)"
check "does not hand the deciding attempt a status only an earlier one answered" \
  "$(is "$(contains "$(error_line)" 'decided the failure, answered HTTP 502')" false)"
check "says that attempt exited with no status of its own" \
  "$(contains "$(error_line)" 'decided the failure, exited 1, printing no HTTP status')"
check "and names the earlier attempt that was answered one, with its own attempt number" \
  "$(contains "$(error_line)" 'where attempt 1 answered HTTP 502')"
check "while every attempt's own words still reach the log" \
  "$(contains "$out" '^gh: Server Error (HTTP 502)$')"
check "the later attempts' too" \
  "$(is "$(grep -c '^error connecting to api.github.com$' <<<"$out" || true)" 4)"

echo "case: an attempt the 30 s bound ended is reported as a timeout and not as a bare refusal"
# The first of the three cases that read the script's own `timeout 30` as the bound firing,
# through the stand-in timeout they opt into per call. When this case was written nothing else
# in this file held the bound: `timeout 30` taken out of a scratch copy of the script on
# 2026-11-08 failed no check. The two cases that stage a killed wrapper now fail without it
# as well, the kill they stage reaching the wrapper only through the `timeout` process between
# gh and the wrapper, and with `timeout 30` taken out of a scratch copy on 2026-11-08 26 checks
# fail, in this case and the five after it, the two killed-wrapper cases among them. The
# stand-in answers 124 at once, so this case costs none of the 150 s a hanging gh under the
# real bound would, and it bites either way the bound could go - dropped, and the stand-in is
# never called at all; changed, and the 30 it recorded is not a 30. The gh here would answer,
# so the bound is the only thing that can fail this drive.
#
# It is also where this file holds both of the attempt's bounds at once: the wrapper's 30 s
# around gh, which is the one these cases stand in for, and the ladder's 40 s around the
# wrapper, which the stand-in records and never answers for. The script's comment says why
# the inner one has to be there - it is the only bound whose firing reaches that attempt's
# status file, and so the only one describe_attempt can tell from a refusal gh printed no
# status for.
#
# All five calls are asked for, so every attempt ends at the bound and the line has one thing
# to say about all five. That is why this case alone holds nothing about which attempt the
# line read: with every attempt the same, a script reading `status.1`, or reading a 124 on any
# attempt as the deciding one, answers the same words. The cases after this one are where
# the attempts differ.
fixture 0
touch "$work/stub-timeout.0" "$work/stub-timeout.1" "$work/stub-timeout.2" \
  "$work/stub-timeout.3" "$work/stub-timeout.4"
drive jobs "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "bounds gh on each of the ladder's five attempts" "$(is "$(gh_bounds)" 5)"
check "with 30 seconds" "$(is "$(cut -d' ' -f1 <<<"$(gh_bound_args)")" 30)"
check "and gh as the command it wraps" "$(is "$(cut -d' ' -f2 <<<"$(gh_bound_args)")" gh)"
check "which is this run's attempt read itself" \
  "$(contains "$(gh_bound_args)" '^30 gh api --paginate --slurp repos/o/r/actions/runs/41/')"
check "while the ladder bounds the wrapper around each of them, ten calls in all" \
  "$(is "$(timeout_calls)" 10)"
check "at 40 seconds, above the bound inside it" \
  "$(contains "$(ladder_args)" '^40 bash -c ')"
check "so no gh call is made outside the bound" "$(is "$(calls)" 0)"
check "and the drive waits for none of it" "$(at_most "$seconds" 2)"
check "prints its own ::error:: line and the ladder's, and no third" "$(is "$(errors)" 2)"
check "naming the bound that ended the attempt that decided the failure" \
  "$(contains "$(error_line)" 'attempt 5, which decided the failure, stopped at its 30 s bound')"
check "and not the words an exit with no status of its own gets" \
  "$(is "$(contains "$(error_line)" 'printing no HTTP status')" false)"
check "and says nothing of the earlier attempts, which ended the same way" \
  "$(is "$(contains "$(error_line)" 'where attempt')" false)"
# The ladder reads any 124 as its own bound having fired, so the line it prints for an attempt
# the inner bound ended names the outer bound's seconds. The refusal line above is read from
# that attempt's own status file and names the bound that actually fired, which is why the two
# say different seconds about the same attempt.
check "while the ladder's own line reads that 124 as its own bound" \
  "$(contains "$out" 'attempt 5 of 5 reached its 40s bound (exit 124)')"

echo "case: the bound ended the first four attempts and the fifth answered a status"
# What the line reports is what the attempt that decided the failure did, and that attempt is
# the last one the ladder made. Here the first four calls are bounded and gh answers the fifth
# with HTTP 502, so a script that read the deciding attempt's words from `status.1`, or that
# took a 124 on any attempt as the one that decided, would call this read a timeout it was
# not. Both mutations were made of a scratch copy of the script on 2026-11-08: the copy
# reading `status.1` fails 16 checks, three of them in this case, and the copy taking a 124 on
# any attempt as the deciding one fails 3, all of them in this case. With this case and the
# one after it taken out of the file, measured the same day, the second copy fails nothing
# that is left, while the first is still caught in four later cases, failing 10 checks: three
# where an earlier attempt answered a status and the last did not, two in the 127 case, three
# in the first killed-wrapper case and two in the second.
fixture 5 "HTTP 502"
touch "$work/stub-timeout.0" "$work/stub-timeout.1" "$work/stub-timeout.2" \
  "$work/stub-timeout.3"
drive jobs "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "bounds gh on each of the ladder's five attempts" "$(is "$(gh_bounds)" 5)"
check "and gh runs only on the attempt the bound did not end" "$(is "$(calls)" 1)"
check "prints its own ::error:: line and the ladder's, and no third" "$(is "$(errors)" 2)"
check "naming the status the deciding attempt was answered" \
  "$(contains "$(error_line)" 'attempt 5, which decided the failure, answered HTTP 502')"
check "and not saying that attempt stopped at the bound" \
  "$(is "$(contains "$(error_line)" 'decided the failure, stopped at its 30 s bound')" false)"
check "while the four that did stop at it are named, each with its own attempt number" \
  "$(contains "$(error_line)" \
    'where attempt 1 stopped at its 30 s bound and attempt 2 stopped at its 30 s bound and attempt 3 stopped at its 30 s bound and attempt 4 stopped at its 30 s bound')"

echo "case: the first four attempts answered a status and the bound ended the fifth"
# The reverse, and a shape the bound exists for rather than one this lane has measured: the
# jobs API answering 502 while a later attempt is left hanging until the bound ends it. The
# 2026-11-08 incident was not that shape - the three attempts of the ladder this read had that
# day failed on 502s in 17, 19 and 18 seconds, which the script's own comment reads as answered
# rather than left hanging. The attempt the bound ended is the one that decided the failure, so
# the line must not hand it the status the earlier four were answered, and must still say what
# those four did, since they did something else.
fixture 5 "HTTP 502"
touch "$work/stub-timeout.4"
drive jobs "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "bounds gh on each of the ladder's five attempts" "$(is "$(gh_bounds)" 5)"
check "and gh runs on the four the bound did not end" "$(is "$(calls)" 4)"
check "prints its own ::error:: line and the ladder's, and no third" "$(is "$(errors)" 2)"
check "naming the bound for the attempt that decided the failure" \
  "$(contains "$(error_line)" 'attempt 5, which decided the failure, stopped at its 30 s bound')"
check "and not handing that attempt the status only the earlier four were answered" \
  "$(is "$(contains "$(error_line)" 'decided the failure, answered HTTP 502')" false)"
check "while those four are named with it, each with its own attempt number" \
  "$(contains "$(error_line)" \
    'where attempt 1 answered HTTP 502 and attempt 2 answered HTTP 502 and attempt 3 answered HTTP 502 and attempt 4 answered HTTP 502')"

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
fixture 5 "HTTP 502"
printf '%s\n' 127 > "$work/stub-timeout.4"
drive jobs "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "bounds gh on each of the ladder's five attempts" "$(is "$(gh_bounds)" 5)"
check "and gh runs on the four the stand-in did not answer for" "$(is "$(calls)" 4)"
check "prints its own ::error:: line and the ladder's, and no third" "$(is "$(errors)" 2)"
check "naming the status the deciding attempt exited" \
  "$(contains "$(error_line)" 'decided the failure, exited 127, printing no HTTP status')"
check "and not the 1 that the attempts gh refused exited" \
  "$(is "$(contains "$(error_line)" 'decided the failure, exited 1,')" false)"
check "nor the bound, which no call in this drive was answered with" \
  "$(is "$(contains "$(error_line)" 'stopped at its 30 s bound')" false)"
check "while the four that were answered a status are named with their own numbers" \
  "$(contains "$(error_line)" \
    'where attempt 1 answered HTTP 502 and attempt 2 answered HTTP 502 and attempt 3 answered HTTP 502 and attempt 4 answered HTTP 502')"

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
# writes that 70 to `status.5`, and the line reads `exited 70, printing no HTTP status`. With
# the kill pointed at pid 999999 in a scratch copy on 2026-11-08, both cases that stage a
# killed wrapper go red - four checks here and two in the one after it, 6 in all.
#
# The attempt is still counted, because the wrapper counts it before it runs gh, so the line
# says five attempts and names the four that answered.
fixture 5 "HTTP 502"
touch "$work/kill-wrapper.4"
drive jobs "$work/out.json"
check "the harness staged the kill" \
  "$(is "$(contains "$out" 'could not stage a killed wrapper')" false)"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "tried five times" "$(is "$(calls)" 5)"
check "prints its own ::error:: line and the ladder's, and no third" "$(is "$(errors)" 2)"
check "says the deciding attempt left no exit status on record and printed no HTTP status" \
  "$(contains "$(error_line)" \
    'attempt 5, which decided the failure, left no exit status on record, printing no HTTP status')"
check "and names no exit status for it, there being none to name" \
  "$(is "$(contains "$(error_line)" 'decided the failure, exited')" false)"
check "nor hands it a status an earlier attempt was answered" \
  "$(is "$(contains "$(error_line)" 'decided the failure, answered HTTP 502')" false)"
check "while the four that were answered are named, each with its own attempt number" \
  "$(contains "$(error_line)" \
    'where attempt 1 answered HTTP 502 and attempt 2 answered HTTP 502 and attempt 3 answered HTTP 502 and attempt 4 answered HTTP 502')"
check "and the attempt the wrapper never finished is still counted" \
  "$(contains "$(error_line)" 'the ladder made 5 attempts')"
# What the ladder saw of that attempt, which is what holds the kill: a wrapper killed with
# SIGKILL leaves the `timeout` above it exiting 137, and the ladder names that status on its
# own line while nothing wrote `status.5` for the refusal line to read.
check "and the ladder saw the killed wrapper exit 137" \
  "$(contains "$out" 'attempt 5 of 5 failed (exit 137)')"

echo "case: a killed wrapper whose gh had printed a status before it died"
# The other half of that shape. The wrapper dies with no `status.<n>` written either way, but
# here gh had already put `HTTP 503` on the stderr the redirection made, so that attempt is
# described from its own stderr and the fourth branch is not what it gets. This case is what
# holds the order of those two reads: measured 2026-11-08, a copy of describe_attempt asking
# whether the status file is missing before it greps the stderr calls this attempt one that
# left no exit status on record, and fails 2 checks, both in this case.
#
# The order is held only while the kill reaches the wrapper. A kill that failed leaves the
# `HTTP 503` in `stderr.5` and a 70 in `status.5`, and the line still reads `answered HTTP
# 503`, so this case holds the staging twice over: that the stand-in printed none of its own
# words, and that nothing echoed `stderr.5` to the log, which the wrapper does after gh
# returns and a killed wrapper never reaches.
fixture 5 "HTTP 502"
printf '%s\n' 'gh: Server Error (HTTP 503)' > "$work/stderr.4"
touch "$work/kill-wrapper.4"
drive jobs "$work/out.json"
check "the harness staged the kill" \
  "$(is "$(contains "$out" 'could not stage a killed wrapper')" false)"
check "and the killed wrapper never echoed the stderr its gh had written" \
  "$(is "$(grep -c '^gh: Server Error (HTTP 503)$' <<<"$out" || true)" 0)"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "tried five times" "$(is "$(calls)" 5)"
check "prints its own ::error:: line and the ladder's, and no third" "$(is "$(errors)" 2)"
check "reads the deciding attempt from the stderr its gh had written" \
  "$(contains "$(error_line)" 'attempt 5, which decided the failure, answered HTTP 503')"
check "rather than from the status file nothing wrote" \
  "$(is "$(contains "$(error_line)" 'decided the failure, left no exit status')" false)"
check "while the four earlier attempts keep the status they were answered" \
  "$(contains "$(error_line)" \
    'where attempt 1 answered HTTP 502 and attempt 2 answered HTTP 502 and attempt 3 answered HTTP 502 and attempt 4 answered HTTP 502')"
check "and the attempt the wrapper never finished is still counted" \
  "$(contains "$(error_line)" 'the ladder made 5 attempts')"

echo "case: refused once, answered on the second attempt"
fixture 1
drive jobs "$work/out.json"
check "exits 0" "$(is "$status" 0)"
check "tried twice" "$(is "$(calls)" 2)"
check "writes the attempt that answered" "$(is "$(entries)" 6)"
check "with its legs" "$(is "$(field 13 .leg)" alone-webkit-alone)"
check "prints no ::error:: line" "$(is "$(errors)" 0)"
check "and waited the ladder's first 10 s before the attempt that answered" \
  "$(is "$(waited)" 10)"

echo "case: the negative control, a gh that answers at once"
# Without this, the case above would pass for a script that waited whether or not an attempt
# was refused: an unconditional wait is one `sleep` call here, and the ladder makes none.
fixture 0
drive jobs "$work/out.json"
check "exits 0" "$(is "$status" 0)"
check "calls gh once" "$(is "$(calls)" 1)"
check "and the ladder waits not at all" "$(is "$(sleeps)" 0)"
check "which the drive measures too" "$(at_most "$seconds" 1)"

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
out=$(PATH="$work/bin:$PATH" GITHUB_REPOSITORY= GITHUB_RUN_ID= \
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
check "and that this is a failed read and not a failed row" \
  "$(contains "$(error_line)" "failed read of the run's artifacts and not a failed browser row")"
check "while naming no GET route at all" "$(is "$(contains "$(error_line)" 'GET ')" false)"
check "no retry count this job cannot see" \
  "$(is "$(contains "$(error_line)" 'retr')" false)"
check "and nothing about what a re-run would find on the run" \
  "$(is "$(contains "$(error_line)" 're-run')" false)"

echo "case: an environment variable the step did not set"
fixture 0
set +e
out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
  GITHUB_RUN_ATTEMPT= "$read_script" jobs "$work/out.json" 2>&1)
status=$?
set -e
check "fails the step" "$(is "$status" 1)"
check "names the variable" \
  "$(contains "$out" 'dispatch-browser-run-reads.sh needs GITHUB_RUN_ATTEMPT in its environment')"
check "makes no request" "$(is "$(calls)" 0)"
check "and writes no out-file" "$(is "$(wrote_out_file)" no)"

fixture 0
set +e
out=$(PATH="$work/bin:$PATH" GH_TOKEN= GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
  GITHUB_RUN_ATTEMPT=$run_attempt "$read_script" jobs "$work/out.json" 2>&1)
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
  "$(contains "$out" 'dispatch-browser-run-reads.sh jobs <out-file>')"
check "makes no request" "$(is "$(calls)" 0)"

fixture 0
drive jobs
check "a jobs call naming no out-file fails too" "$(is "$status" 1)"
check "naming what it was given" "$(contains "$out" 'It was given: jobs$')"
check "and makes no request" "$(is "$(calls)" 0)"

summary "dispatch-browser-run-reads.sh reads this run's attempt"
