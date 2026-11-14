#!/usr/bin/env bash
# Tests for `dispatch-browser-run-reads.sh`, the three reads dispatch-browser.yaml's merge job
# makes of its own run: the jobs of this attempt, which the merge job's readings name legs from,
# the line that says a blob download could not be completed, and the lane's machine time, summed
# out of the jobs file the first read wrote.
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
# four cases opt into call by call and that otherwise execs the real one, and the script.
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
# killed wrapper go red: 5 checks fail, three in the first and two in the second. Two of those
# reds name the staging, and none of them names the host this was run on, which is what this
# line is for.
[ -r /proc/self/stat ] ||
  { echo "dispatch-browser-run-reads.test.sh needs /proc (Linux) for the cases that stage a" \
      "killed wrapper" >&2
    exit 1; }

# The stand-in `timeout`, beside the stand-in `gh` and opted into per call. It records the
# whole argument list of every call it is given, one file per call, whether or not it stands in
# for that call, so `timeout_calls` and `timeout_args` read the bound and the command that was
# wrapped in every drive in this file.
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
cat > "$work/bin/timeout" <<TIMEOUT
#!/usr/bin/env bash
set -euo pipefail
calls=\$(find "$work/timeout-calls" -maxdepth 1 -type f | wc -l)
printf '%s\n' "\$*" > "$work/timeout-calls/call.\$calls"
if [ ! -f "$work/stub-timeout.\$calls" ]; then
  exec $real_timeout "\$@"
fi
answered=\$(cat "$work/stub-timeout.\$calls")
exit "\${answered:-124}"
TIMEOUT
chmod +x "$work/bin/timeout"

# page <id>|<name>|<conclusion>[|<seconds>]...: the page the stand-in answers with. `--slurp`
# collects the pages into one array, so the answer is an array of page objects and each holds a
# `jobs` array. `run_attempt` is in each job because the jobs API puts it there; nothing in the
# script reads it any more, which is what the entries this writes are held against.
#
# `<seconds>` is how long that job took, and the two timestamps the jobs API answers with are
# written from it: `started_at` at the fixed epoch below and `completed_at` that many seconds
# later. A job given no seconds gets a null `completed_at`, which is what the API answers for a
# job that has not finished - the merge job itself while it reports - and `-1` writes the
# backwards pair the API answers for a skipped job (run 43002633713's alone job, started
# 2026-11-11T10:22:27Z and completed 10:22:26Z, read 2026-11-11).
started_epoch=1794392547  # 2026-11-11T10:22:27Z
page() {
  jq -n --argjson started "$started_epoch" '[{total_count: ($ARGS.positional | length),
    jobs: [$ARGS.positional[] | split("|")
      | {id: (.[0] | tonumber), name: .[1],
         conclusion: (if .[2] == "null" then null else .[2] end),
         started_at: ($started | todateiso8601),
         completed_at: (if (.[3] // "") == "" then null
           else ($started + (.[3] | tonumber) | todateiso8601) end),
         run_attempt: 2}]}]' \
    --args "$@" > "$work/page.json"
}

# happy_page: one attempt of a run of this lane, as the jobs API names it. A called workflow's
# job is named `<the caller's job> / <this job>`, with its matrix leg in brackets after it, and
# the caller's own jobs are in the same list: `lint` and `dispatch-multiprocess`, the second of
# which the name filter keeps out only by the hyphen before `multiprocess`.
#
# The seconds are a full lane of this file's shape, in round numbers rather than readings: the
# eight jobs of run 43074443876 took binaries 171, shard (1) 577, shard (2) 705, alone 199,
# multiprocess 125, embedder 55, visual 114 and merge 28 seconds (the jobs API, read
# 2026-11-11), and this page carries numbers of that order, no embedder job and a skipped
# multiprocess job, because what the cases read is the arithmetic and not the run. The five jobs
# of this workflow that this page gives a conclusion and an end, the merge job apart, sum to
# 1,750 s, which is inside the full lane's budget and over the pull-request lane's, so one
# fixture drives both arms of the machine-time cases below. The caller's two jobs carry 9,999 s
# each: a sum that held either of them would be past every budget this file has.
happy_page() {
  page "10|dispatch-browser / binaries|success|150" \
    "11|dispatch-browser / shard (1)|success|600" \
    "12|dispatch-browser / shard (2)|failure|700" \
    "13|dispatch-browser / alone|success|200" \
    "14|dispatch-browser / multiprocess|skipped|-1" \
    "15|dispatch-browser / merge|null" \
    "16|lint|success|9999" \
    "17|dispatch-multiprocess|success|9999" \
    "18|dispatch-browser / visual|failure|100"
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
# stand-in that refuses that many calls and then answers, and the happy page for the answer. It
# also puts the ladder's wait back to 0, so a case that does not ask for one waits not at all
# and the three that do set `retry_seconds` themselves after calling it. Any per-call `stderr.<n>`
# an earlier case wrote goes with it, so one case's faults never answer another's call, and so
# do the stand-in `timeout`'s per-call `stub-timeout.<n>` files and its own call log, and the
# stand-in gh's per-call `kill-wrapper.<n>` files. The machine-time cases' own two go with
# them: the jobs file a case wrote for that mode, and the step summary, which is no file at
# all until a case names one, so a case that does not ask for a summary drives the script with
# GITHUB_STEP_SUMMARY empty, as a shell that runs it outside Actions has it.
fixture() {
  rm -rf "$work/calls" "$work/out.json" "$work/timeout-calls"
  rm -f "$work"/stderr.* "$work"/stub-timeout.* "$work"/kill-wrapper.* "$work/jobs.json" \
    "$work/summary.md"
  mkdir -p "$work/calls" "$work/timeout-calls"
  printf '%s\n' "${1:-0}" > "$work/refuse"
  printf '%s\n' "${2:-HTTP 502}" > "$work/stderr"
  retry_seconds=0
  summary_file=
  happy_page
}

# drive <argument>...: the real script with the stand-in first on PATH. Sets `out` to its
# combined output, `status` to its exit code and `seconds` to how long the drive took. Not a
# subshell, so all three survive the call.
drive() {
  local started=$SECONDS
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
    GITHUB_RUN_ATTEMPT=$run_attempt GH_RETRY_SECONDS=$retry_seconds \
    GITHUB_STEP_SUMMARY="$summary_file" "$read_script" "$@" 2>&1)
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
notice_line() { grep '::notice' <<<"$out" || true; }
summary_text() { cat "$work/summary.md" 2>/dev/null || true; }
at_least() { [ "$1" -ge "$2" ] && echo true || echo false; }
at_most() { [ "$1" -le "$2" ] && echo true || echo false; }

echo "case: this attempt's jobs become the six fields the readings consume"
fixture 0
drive jobs "$work/out.json"
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
check "prints no ::error:: line" "$(is "$(errors)" 0)"

echo "case: the route is this run's attempt endpoint"
# The same drive: what the stand-in recorded is what the script asked GitHub for.
check "names this run's attempt" "$(contains "$(args)" "/attempts/$run_attempt/jobs")"
check "under this repository and run" "$(contains "$(args)" 'repos/o/r/actions/runs/41/')"
check "a hundred to a page" "$(contains "$(args)" 'jobs?per_page=100')"
check "and holds no filter=all" "$(is "$(contains "$(args)" 'filter=all')" false)"
check "slurps the pages, since gh refuses --slurp beside --jq" \
  "$(contains "$(args)" '--paginate --slurp')"

echo "case: the alone leg is the bare job name, and a shard leg's number is read whole"
# The alone job ran a matrix of three projects, one leg each, and its leg was read out of the
# brackets after the job name; it runs the three on one machine now and writes one blob report,
# so the leg is the word itself (dispatch://INFRA-1109). The filter admits the name with either
# caller's prefix and with none at all, and both shapes must read the same leg: a leg absent
# from reading one's list is a leg whose missing blob nothing reports. The shard row with no
# prefix is here because the shard capture wanted that leading space until
# dispatch://INFRA-1109, where the `alone` test beside it did not: both read the same leg now,
# whichever shape the caller's job name takes. The shard capture still takes a run of digits
# rather than one, so a tenth leg would be read whole.
fixture 0
page "21|dispatch-browser / alone|success" "22|alone|success" \
  "23|dispatch-browser / shard (10)|success" "24|shard (1)|success"
drive jobs "$work/out.json"
check "exits 0" "$(is "$status" 0)"
check "reads the four jobs" "$(is "$(entries)" 4)"
check "the prefixed alone job is the alone leg" "$(is "$(field 21 .leg)" alone)"
check "and so is one with no prefix" "$(is "$(field 22 .leg)" alone)"
check "a two-digit shard leg is read whole" "$(is "$(field 23 .leg)" shard-10)"
check "and a shard job with no prefix is its own leg" "$(is "$(field 24 .leg)" shard-1)"

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
# not. Both mutations were made of a scratch copy of the script on 2026-11-08: the copy
# reading `status.1` fails 8 checks and the copy taking a 124 on any attempt as the deciding
# one fails 3, all of them in this case. With this case and the one after it taken out of the
# file, measured the same day, the second copy fails nothing that is left, while the first is
# still caught in two later cases, failing 4 checks: two in the 127 case and two in the first
# killed-wrapper case.
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
drive jobs "$work/out.json"
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
drive jobs "$work/out.json"
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
drive jobs "$work/out.json"
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

echo "case: refused once, answered on the second attempt"
fixture 1
retry_seconds=1
drive jobs "$work/out.json"
check "exits 0" "$(is "$status" 0)"
check "tried twice" "$(is "$(calls)" 2)"
check "writes the attempt that answered" "$(is "$(entries)" 7)"
check "with its legs" "$(is "$(field 13 .leg)" alone)"
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

# THE MACHINE-TIME CASES. The budgets are 2,363 s on the full lane and 863 s on the
# pull-request lane, which are R5's 2,400 s and 900 s less the merge job's own worst case of
# 37 s (the script's own branch says what that figure is made of). Each case below
# drives the arithmetic to one side of a budget or the other: a budget nothing is ever refused
# by refuses nothing, which is the defect class this mode exists against (R12 of
# dispatch://INFRA-1109/spec).

echo "case: the lane's machine time is summed out of the file the jobs mode wrote"
# The two modes over one page, one after the other: the `jobs` mode writes the file and this
# mode reads it, so the fields one writes and the other reads cannot drift apart. The page is
# the happy one - binaries 150 s, shard (1) 600, shard (2) 700 (red), alone 200, visual 100
# (red), multiprocess skipped, merge still running, and the caller's two jobs at 9,999 s each.
fixture 0
drive jobs "$work/out.json"
check "the jobs mode exits 0" "$(is "$status" 0)"
drive machine-time "$work/out.json" full
check "the full lane passes under its budget" "$(is "$status" 0)"
check "naming the seconds it summed and the budget they are held to" \
  "$(contains "$(notice_line)" \
    'the full lane cost 1750 s over 5 concluded jobs, this merge job apart, against a budget of 2363 s')"
check "which it says is R5's bound less this job's own worst case" \
  "$(contains "$(notice_line)" '2400 s less this job.s own worst case of 37 s')"
check "and prints no ::error:: line" "$(is "$(errors)" 0)"
# 1,750 s is binaries, both shards, alone and visual: a red leg cost its machine time like any
# other and is in the sum, while the skipped job (-1 s), the merge job (still running) and the
# caller's two 9,999 s jobs are not. Any one of those five in or out moves the figure above.

echo "case: the same run on the pull-request lane is refused by the lower budget"
# The lane decides the budget and nothing else does: the same jobs, read as the lane whose
# bound is 900 s rather than 2,400, are refused. This is what holds the script to reading the
# lane it is given rather than one budget for every run.
drive machine-time "$work/out.json" pull-request
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the sum and the pull-request lane's budget" \
  "$(contains "$(error_line)" \
    'this pull-request lane has cost 1750 s of machine time over its 5 concluded jobs, this merge job apart, which is above the 863 s')"
check "and R5's bound for that lane with this job's own maximum taken off it" \
  "$(contains "$(error_line)" "R5's bound of 900 s for this lane, less this job's own worst case of 37 s")"
check "and says this is the suite costing more and not a failed row" \
  "$(contains "$(error_line)" 'costing more machine time than dispatch://INFRA-1109 left it costing, and not a failed browser row')"

echo "case: each budget bites at its own second"
# The one case that would catch a budget written one second out, or a `-ge` where the rule is
# `-gt`: the bound itself passes and one second more does not, on each lane.
fixture 0
lane_jobs "dispatch-browser / shard (1)|success|2363"
drive machine-time "$work/jobs.json" full
check "the full lane passes at 2,363 s exactly" "$(is "$status" 0)"
lane_jobs "dispatch-browser / shard (1)|success|2364"
drive machine-time "$work/jobs.json" full
check "and is refused at 2,364" "$(is "$status" 1)"
check "naming the second it was over by" \
  "$(contains "$(error_line)" 'cost 2364 s of machine time over its 1 concluded jobs')"
lane_jobs "dispatch-browser / shard (1)|success|863"
drive machine-time "$work/jobs.json" pull-request
check "the pull-request lane passes at 863 s exactly" "$(is "$status" 0)"
lane_jobs "dispatch-browser / shard (1)|success|864"
drive machine-time "$work/jobs.json" pull-request
check "and is refused at 864" "$(is "$status" 1)"

echo "case: this merge job's own seconds are not in the sum"
# The reason the budget is R5's bound less this job's maximum rather than R5's bound itself:
# the job doing the reading cannot see its own duration, so it must not be in the sum even
# where the API has one for it - a merge job of an earlier attempt, carried forward with a
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
    "cost 750 s against a budget of 2363 s, which is R5's 2400 s less this job's own worst case of 37 s")"

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
