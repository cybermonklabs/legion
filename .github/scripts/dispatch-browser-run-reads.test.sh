#!/usr/bin/env bash
# Tests for `dispatch-browser-run-reads.sh`, the two reads dispatch-browser.yaml's merge job
# makes of its own run: the jobs of this attempt, which the merge job's readings name legs from,
# and the line that says a blob download could not be completed.
#
# Each case writes the one slurped page a stand-in `gh` answers with - the array of page objects
# `gh api --paginate --slurp` prints - and how many calls that stand-in refuses first, then runs
# the real script with the stand-in first on PATH and reads the file it wrote, the arguments the
# stand-in recorded and the lines the script printed. The ladder's waits are driven down with
# GH_RETRY_SECONDS=1, which makes them 1 s and then 2 s, and two cases measure them with bash's
# own SECONDS.
#
# NO CASE REACHES THE NETWORK: every case is files, a stand-in gh and the script.
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
# for the calls after those.
set -euo pipefail
calls=\$(find "$work/calls" -maxdepth 1 -type f | wc -l)
printf '%s\n' "\$*" > "$work/calls/call.\$calls"
if [ "\$((calls + 1))" -le "\$(cat "$work/refuse")" ]; then
  cat "$work/stderr" >&2
  exit 1
fi
cat "$work/page.json"
GH
chmod +x "$work/bin/gh"

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
# stand-in that refuses that many calls and then answers, and the happy page for the answer.
fixture() {
  rm -rf "$work/calls" "$work/out.json"
  mkdir -p "$work/calls"
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
    GITHUB_RUN_ATTEMPT=$run_attempt GH_RETRY_SECONDS=1 "$read_script" "$@" 2>&1)
  status=$?
  set -e
  seconds=$((SECONDS - started))
}

calls() { find "$work/calls" -maxdepth 1 -type f | wc -l | tr -d ' '; }
args() { cat "$work/calls/call.0"; }
entries() { jq -r 'length' "$work/out.json"; }
field() { jq -r --argjson id "$1" ".[] | select(.id == \$id) | $2" "$work/out.json"; }
errors() { grep -c '::error::' <<<"$out" || true; }
error_line() { grep '::error::' <<<"$out" || true; }
wrote_out_file() { [ -e "$work/out.json" ] && echo yes || echo no; }
at_least() { [ "$1" -ge "$2" ] && echo true || echo false; }

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
  "$(contains "$(args)" -- '--paginate --slurp')"

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
# A resolution failure or a timeout prints no status line, and a line that named one anyway
# would send a reader looking for a 502 that never happened.
fixture 3 "could not resolve host: api.github.com"
drive jobs "$work/out.json"
check "fails the step" "$(is "$status" 1)"
check "writes no out-file" "$(is "$(wrote_out_file)" no)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "which says gh printed none" "$(contains "$(error_line)" 'gh printed no HTTP status')"
check "and names no status of its own" \
  "$(is "$(contains "$(error_line)" 'HTTP [0-9][0-9][0-9]')" false)"
check "while gh's own words reach the log" \
  "$(contains "$out" '^could not resolve host: api.github.com$')"

echo "case: refused once, answered on the second attempt"
fixture 1
drive jobs "$work/out.json"
check "exits 0" "$(is "$status" 0)"
check "tried twice" "$(is "$(calls)" 2)"
check "writes the attempt that answered" "$(is "$(entries)" 5)"
check "with its legs" "$(is "$(field 13 .leg)" alone-webkit-alone)"
check "prints no ::error:: line" "$(is "$(errors)" 0)"
check "and waited the ladder's first second" "$(at_least "$seconds" 1)"

echo "case: the negative control, a gh that answers at once"
# Without this, the two cases above would pass for a script that slept whatever it was given.
fixture 0
drive jobs "$work/out.json"
check "exits 0" "$(is "$status" 0)"
check "calls gh once" "$(is "$(calls)" 1)"
check "and waits not at all" "$(is "$seconds" 0)"

echo "case: blobs-refused says the legs' blobs could not be downloaded"
fixture 0
set +e
out=$(PATH="$work/bin:$PATH" GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
  GITHUB_RUN_ATTEMPT=$run_attempt "$read_script" blobs-refused 2>&1)
status=$?
set -e
check "exits 0, since the download step already failed the job" "$(is "$status" 0)"
check "makes no request, so it needs no GH_TOKEN" "$(is "$(calls)" 0)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the artifacts endpoint" \
  "$(contains "$(error_line)" 'GET repos/o/r/actions/runs/41/artifacts')"
check "the run attempt" "$(contains "$(error_line)" 'on run attempt 4')"
check "the retries the download action spent" \
  "$(contains "$(error_line)" 'exhausted its own five retries')"
check "and that this is a failed read and not a failed row" \
  "$(contains "$(error_line)" "failed read of the run's artifacts and not a failed browser row")"

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
out=$(PATH="$work/bin:$PATH" GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 \
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
