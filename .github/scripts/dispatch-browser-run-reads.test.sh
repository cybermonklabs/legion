#!/usr/bin/env bash
# Tests for `dispatch-browser-run-reads.sh`, the two reads dispatch-browser.yaml's merge job
# makes of its own run: the jobs of this attempt, which the merge job's readings name legs from,
# and the line that says a blob download could not be completed.
#
# Each case writes the one slurped page a stand-in `gh` answers with - the array of page objects
# `gh api --paginate --slurp` prints - and how many calls that stand-in refuses first, then runs
# the real script with the stand-in first on PATH and reads the file it wrote, the arguments the
# stand-in recorded and the lines the script printed. The ladder's waits are the case's own,
# `retry_seconds` seconds for the first and twice that for the second, which `fixture` resets to
# 0 so that only the three cases about the waits pay for one: two set it to 1 and read that the
# drive took that long with bash's own SECONDS, and the control sets it to 10 and reads that the
# drive took no time at all, which a script that slept whatever it was given would fail.
#
# NO CASE REACHES THE NETWORK: every case is files, a stand-in gh, a stand-in `timeout` that
# three cases opt into call by call and that otherwise execs the real one, and the script.
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
# attempt: the stand-in kills its own grandparent, which is the script's \`bash -c\` wrapper -
# its parent is the \`timeout\` the wrapper runs it under - so that attempt's redirection has
# made \`stderr.<n>\` and nothing writes \`status.<n>\`. That is the one input describe_attempt's
# fourth branch exists for, and nothing else in this file reaches it.
set -euo pipefail
calls=\$(find "$work/calls" -maxdepth 1 -type f | wc -l)
printf '%s\n' "\$*" > "$work/calls/call.\$calls"
if [ -f "$work/kill-wrapper.\$calls" ]; then
  kill -9 "\$(awk '{print \$4}' "/proc/\$PPID/stat")" 2>/dev/null || true
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

# The stand-in `timeout`, beside the stand-in `gh` and opted into per call. It records the
# whole argument list of every call it is given, one file per call, whether or not it stands in
# for that call, so `timeout_calls` and `timeout_args` read the bound and the command that was
# wrapped in every drive in this file.
#
# Standing in is what is opt-in, and the reason is not the recording: a stand-in every case
# used would still catch a dropped bound through `timeout_calls`. It is that a call it stands
# in for exits 124 at once without running gh, which is what the real timeout exits when its
# bound fires, and every other case in this file needs gh's answer - the page it writes, the
# stderr of the calls it refuses, the arguments it records.
#
# A case asks for call `<n>` to be the bound firing by writing `$work/stub-timeout.<n>`,
# counting from 0, so `stub-timeout.<n>` names attempt n + 1 of the ladder: this stand-in is
# called once for every attempt, whether it stands in for that call or execs the real timeout.
# The stand-in gh's `stderr.<n>` counts from 0 too and counts something else - gh's own calls,
# which hold no call for an attempt this stand-in answered with 124 itself. So `stderr.<n>`
# names gh's own n-th call rather than attempt n + 1, and in a case that mixes the two the two
# numberings drift apart. `fixture` removes those files again, and a call no file was written
# for runs the real timeout this execs, so that attempt's gh is bounded as the script asked.
cat > "$work/bin/timeout" <<TIMEOUT
#!/usr/bin/env bash
set -euo pipefail
calls=\$(find "$work/timeout-calls" -maxdepth 1 -type f | wc -l)
printf '%s\n' "\$*" > "$work/timeout-calls/call.\$calls"
if [ ! -f "$work/stub-timeout.\$calls" ]; then
  exec $real_timeout "\$@"
fi
exit 124
TIMEOUT
chmod +x "$work/bin/timeout"

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
    "17|dispatch-multiprocess|success"
}

# fixture <refused calls> [<stderr of a refused call>]: a fresh call log and no out-file, a
# stand-in that refuses that many calls and then answers, and the happy page for the answer. It
# also puts the ladder's wait back to 0, so a case that does not ask for one waits not at all
# and the three that do set `retry_seconds` themselves after calling it. Any per-call `stderr.<n>`
# an earlier case wrote goes with it, so one case's faults never answer another's call, and so
# do the stand-in `timeout`'s per-call `stub-timeout.<n>` files and its own call log, and the
# stand-in gh's per-call `kill-wrapper.<n>` files.
fixture() {
  rm -rf "$work/calls" "$work/out.json" "$work/timeout-calls"
  rm -f "$work"/stderr.* "$work"/stub-timeout.* "$work"/kill-wrapper.*
  mkdir -p "$work/calls" "$work/timeout-calls"
  printf '%s\n' "${1:-0}" > "$work/refuse"
  printf '%s\n' "${2:-HTTP 502}" > "$work/stderr"
  retry_seconds=0
  happy_page
}

# drive <argument>...: the real script with the stand-in first on PATH. Sets `out` to its
# combined output, `status` to its exit code and `seconds` to how long the drive took. Not a
# subshell, so all three survive the call.
drive() {
  local started=$SECONDS
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
    GITHUB_RUN_ATTEMPT=$run_attempt GH_RETRY_SECONDS=$retry_seconds "$read_script" "$@" 2>&1)
  status=$?
  set -e
  seconds=$((SECONDS - started))
}

calls() { find "$work/calls" -maxdepth 1 -type f | wc -l | tr -d ' '; }
args() { cat "$work/calls/call.0"; }
timeout_calls() { find "$work/timeout-calls" -maxdepth 1 -type f | wc -l | tr -d ' '; }
timeout_args() { cat "$work/timeout-calls/call.0"; }
entries() { jq -r 'length' "$work/out.json"; }
field() { jq -r --argjson id "$1" ".[] | select(.id == \$id) | $2" "$work/out.json"; }
errors() { grep -c '::error::' <<<"$out" || true; }
error_line() { grep '::error::' <<<"$out" || true; }
wrote_out_file() { [ -e "$work/out.json" ] && echo yes || echo no; }
at_least() { [ "$1" -ge "$2" ] && echo true || echo false; }
at_most() { [ "$1" -le "$2" ] && echo true || echo false; }

echo "case: this attempt's jobs become the four fields the readings consume"
fixture 0
drive jobs "$work/out.json"
check "exits 0" "$(is "$status" 0)"
check "writes this workflow's five jobs and none of the caller's" "$(is "$(entries)" 5)"
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
fixture 3 "HTTP 502"
retry_seconds=1
drive jobs "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "tried three times" "$(is "$(calls)" 3)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the endpoint it read" \
  "$(contains "$(error_line)" 'GET repos/o/r/actions/runs/41/attempts/4/jobs?per_page=100')"
check "the status gh last printed" "$(contains "$(error_line)" 'HTTP 502')"
check "the ladder's attempts and this run attempt" \
  "$(contains "$(error_line)" '3 attempts, all on run attempt 4')"
check "and that this is a failed read and not a failed row" \
  "$(contains "$(error_line)" 'failed read of the jobs API and not a failed browser row')"
check "every attempt's gh output still reaches the log" \
  "$(is "$(grep -c '^HTTP 502$' <<<"$out" || true)" 3)"
check "and the ladder waited 1 s and then 2 s" "$(at_least "$seconds" 3)"

echo "case: a refusal gh printed no HTTP status for"
# A resolution failure prints no status line, and a line that named one anyway would send a
# reader looking for a 502 that never happened.
fixture 3 "could not resolve host: api.github.com"
drive jobs "$work/out.json"
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
drive jobs "$work/out.json"
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
# The first of the three cases that read the script's own `timeout 30`, through the stand-in
# timeout they opt into per call. Nothing else in this file holds the bound: deep review took
# `timeout 30` out of a scratch copy of the script on 2026-11-08 and this suite still read
# `76 passed, 0 failed`. The stand-in answers 124 at once, so this case costs none of the 90 s
# a hanging gh under the real bound would, and it bites either way the bound could go -
# dropped, and the stand-in is never called at all; changed, and the 30 it recorded is not a
# 30. The gh here would answer, so the bound is the only thing that can fail this drive.
#
# All three calls are asked for, so every attempt ends at the bound and the line has one thing
# to say about all three. That is why this case alone holds nothing about which attempt the
# line read: with every attempt the same, a script reading `status.1`, or reading a 124 on any
# attempt as the deciding one, answers the same words. The two cases after this one are where
# the attempts differ.
fixture 0
touch "$work/stub-timeout.0" "$work/stub-timeout.1" "$work/stub-timeout.2"
drive jobs "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "bounds each of the ladder's three attempts" "$(is "$(timeout_calls)" 3)"
check "with 30 seconds" "$(is "$(cut -d' ' -f1 <<<"$(timeout_args)")" 30)"
check "and gh as the command it wraps" "$(is "$(cut -d' ' -f2 <<<"$(timeout_args)")" gh)"
check "which is this run's attempt read itself" \
  "$(contains "$(timeout_args)" '^30 gh api --paginate --slurp repos/o/r/actions/runs/41/')"
check "so no gh call is made outside the bound" "$(is "$(calls)" 0)"
check "and the drive waits for none of it" "$(at_most "$seconds" 1)"
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
# not. Both mutations were made of a scratch copy of the script on 2026-11-08 and the suite
# without this case and the one after it read `90 passed, 0 failed` for each of them.
fixture 3 "HTTP 502"
touch "$work/stub-timeout.0" "$work/stub-timeout.1"
drive jobs "$work/out.json"
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
drive jobs "$work/out.json"
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

echo "case: an attempt whose wrapper was killed leaves neither status nor stderr to read"
# describe_attempt's fourth branch, the one shape the other cases cannot reach: an attempt
# that left no exit status at all. The wrapper writes `status.<n>` after gh returns, so no
# stand-in gh can keep it from being written by answering differently - it has to not return.
# The stand-in kills the wrapper instead, which is the thing the branch exists for: the
# redirection has made `stderr.<n>` and nothing has written `status.<n>`.
#
# Round 5 left this branch with no case, on the ground that a lock it could not make fail
# would be the third empty lock of this suite. It can be made to fail: with the branch
# removed, so that the third branch prints the empty status it holds, this case reads
# `exited , printing no HTTP status` and three of its assertions go red.
#
# The attempt is still counted, because the wrapper counts it before it runs gh, so the line
# says three attempts and names the two that answered.
fixture 3 "HTTP 502"
touch "$work/kill-wrapper.2"
drive jobs "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "tried three times" "$(is "$(calls)" 3)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "says the deciding attempt left neither of the two" \
  "$(contains "$(error_line)" \
    'attempt 3, which decided the failure, ended with neither an exit status nor an HTTP status')"
check "and names no exit status for it, there being none to name" \
  "$(is "$(contains "$(error_line)" 'decided the failure, exited')" false)"
check "nor hands it a status an earlier attempt was answered" \
  "$(is "$(contains "$(error_line)" 'decided the failure, answered HTTP 502')" false)"
check "while the two that were answered are named, each with its own attempt number" \
  "$(contains "$(error_line)" \
    'where attempt 1 answered HTTP 502 and attempt 2 answered HTTP 502')"
check "and the attempt the wrapper never finished is still counted" \
  "$(contains "$(error_line)" 'the ladder made 3 attempts')"

echo "case: refused once, answered on the second attempt"
fixture 1
retry_seconds=1
drive jobs "$work/out.json"
check "exits 0" "$(is "$status" 0)"
check "tried twice" "$(is "$(calls)" 2)"
check "writes the attempt that answered" "$(is "$(entries)" 5)"
check "with its legs" "$(is "$(field 13 .leg)" alone-webkit-alone)"
check "prints no ::error:: line" "$(is "$(errors)" 0)"
check "and waited the ladder's first second" "$(at_least "$seconds" 1)"

echo "case: the negative control, a gh that answers at once"
# Without this, the two cases above would pass for a script that slept whatever it was given.
# The knob is 10 here and the gh answers at once, so an unconditional sleep would take 10 s and
# this drive takes none; the bound is an upper one because SECONDS counts whole seconds and a
# drive that straddles a tick reads 1 however short it was.
fixture 0
retry_seconds=10
drive jobs "$work/out.json"
check "exits 0" "$(is "$status" 0)"
check "calls gh once" "$(is "$(calls)" 1)"
check "and waits not at all" "$(at_most "$seconds" 1)"

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
