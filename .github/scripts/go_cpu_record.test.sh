#!/usr/bin/env bash
# Tests for `go_cpu_record.py`, the reader of the envoy-go jobs' wall times over several runs of
# one commit and of the factors that may account for their spread. `spread` refuses fewer than
# five runs, runs of more than one commit and a run whose attempt is not 1; otherwise it prints,
# for each counted job, its successful job-runs' n, min, median and max and the spread (max −
# min) ÷ median, with the job ids of its min and its max, then each job-run's id, conclusion and
# wall, `completed_at − started_at`, and exits 1 naming each job with fewer than five successful
# job-runs. `factors` prints one row per job-run with the factors the jobs API, the job's log and
# the CPU rows of its artifact give, UNKNOWN where they give none, and the decision rule's
# statistics and verdict for each branch. `cpu` prints each Go job's CPU seconds by package from
# the rows of its artifact, and holds the job's total to its log's `go-cpu job:` line; `coverage`
# holds each package a Go job's events show running a test binary to having a row. `rule` runs
# the decision rule over a table given as a file, and `postgres-packages` splits the package
# directories under a root into those whose tests use Postgres and the others.
#
# The reader reads GitHub only through gh. Here gh is a stand-in on PATH that answers `gh api`
# from fixture files by the URL's path and query, and `gh run download <run> -R
# cybermonklabs/legion -n <name> -D <dir>` by copying the fixture artifact of that name into
# <dir>, and logs each call. A log read without --allow-escape-sequences gets the refusal gh
# itself prints for a job log, and a download of a name no fixture artifact has gets gh's `no
# artifact matches any of the names or patterns provided` and exit 1 (both read from the real gh
# 2.98.0); any call that is not one of the reader's four reads is refused with exit 1 and a
# message, so an unexpected call fails the case that made it. Nothing here reaches the network.
#
# The fixtures under testdata/go-cpu/ were captured from runs of ci.yaml on 2026-11-28:
#
#   run-<id>.json
#       gh api repos/cybermonklabs/legion/actions/runs/<id> | jq -c '{id, name, head_sha,
#       head_branch, event, status, conclusion, run_attempt, created_at, run_started_at,
#       updated_at}'
#     for INFRA-5794's six runs 46040397906, 46040403701, 46040410283, 46040421210, 46040426159
#     and 46040431439 (head 09bf0bc6aa0a, attempt 1); for run 46084080784, a merge-group run of
#     main's 91e57a4886cb; and for run 46131638614, this pull request's first CI run (head
#     3f0944fad455, attempt 1), whose Go jobs kept the first CPU record.
#   jobs-<id>-p<n>.json
#       gh api 'repos/cybermonklabs/legion/actions/runs/<id>/attempts/1/jobs?per_page=100&page=<n>'
#       | jq -c '{total_count, jobs: [.jobs[] | {id, run_id, run_attempt, name, status,
#       conclusion, created_at, started_at, completed_at, runner_name} + (if (.name |
#       test("^envoy-go / |^envoy-go-checks / ")) then {steps: [.steps[]? | {name, number,
#       status, conclusion, started_at, completed_at}]} else {steps: []} end)]}'
#     for n = 1 and 2 of the six runs: each run's 111 jobs, 100 on page 1 and 11 on page 2, where
#     the aggregator `envoy-go / envoy-go` is; and for n = 1 of run 46131638614, whose 68 jobs fit
#     one page. Only the envoy-go lane's jobs keep their steps, to keep the fixtures small; every
#     other job has an empty list. Run 46084080784's jobs are not kept: `spread` refuses its set
#     before it reads any run's jobs.
#   log-<job id>.excerpt.txt
#       gh api --allow-escape-sequences repos/cybermonklabs/legion/actions/jobs/<job id>/logs
#       | tr -d '\033' | grep -E '^.{29}(go-cpu |Region: |Worker ID: |Version: |Image: |Cache hit
#       for restore-key: |Cache restored from key: |Cache not found for input keys|Cache Size: )'
#     (the grep's pattern is one line), the lines the reader parses, for job 140647302858, leg 2
#     of run 46040397906 (Region centralus, a restore-key hit of the build cache), and for job
#     140585658657, a probe job of another workflow's run (Region northcentralus), which restores
#     no cache, so its excerpt holds the Set up job lines alone. Neither job printed a `go-cpu`
#     line: both ran before the CPU record. And for six jobs of run 46131638614: leg 5
#     (140938293835), leg 2 (140938293813), the skip counter (140938293829), alone (140938293806),
#     race-impossible (140938293848), which go-cpu.test.sh's R4 reads, and envoy-go-checks
#     (140938293854), which prints a runner and a usage line and no job line. The job line of each
#     of the first four is rewritten by hand to the sums of the rows its artifact below keeps
#     (binaries, nested, runs, user, sys, cpu and wall, added up as go-cpu-runner.py's job line
#     adds them), so that a job's total in its log is that of its cut artifact; their real cpu= are
#     138.39, 270.29, 13.59 and 84.10. Every other line is as captured.
#   log-miss.hand-made.txt
#     written by hand, not captured: the Set up job lines and the line actions/cache prints when
#     it finds no entry for any of its keys, since neither captured job missed.
#   go-test-events-<job id>/
#       gh run download 46131638614 -R cybermonklabs/legion -n go-test-events-<job id> -D <dir>
#     the artifacts of the same four Go jobs of run 46131638614, cut: cpu/runner/ whole but for
#     sampler.pid, which the repository's .gitignore keeps out (*.pid) and nothing here reads, and
#     of the rows in cpu/ those whose ids end in these pids, each class the job holds:
#       leg 5          3113 (run) and the test rows of internal/pgmigrate (3560), loadtest (3620),
#                      internal/pglisten (10448), internal/dispatch/asof (10675) and
#                      internal/testpg/testdb (12085), whose tests use Postgres, and of
#                      internal/testnats (3553), internal/cistore (3629), clitest (5951) and
#                      internal/bus/kvrecords (11099), whose tests do not
#       leg 2          3110 (run), the test row of cmd/cipher (3615) with its two nested rows
#                      (24090 and 24174), the run's only nested rows, and that of
#                      internal/cipher/api (3652)
#       skip counter   9040 (list) and 7905 (outside), both of internal/dispatch/api, 2963 (run),
#                      and the test rows of internal/cipher/api (3135 and 3574), cmd/cipher
#                      (3134), cmd/dispatch-route-adopt (17229) and internal/dispatch/api (4523),
#                      a control run of the database-skips harness, which keeps no events
#       alone          3218 (run) and the test rows of cmd/cipher (3508), internal/cistore (4571),
#                      internal/cipher/natscallout (6072) and internal/dispatch/docs (7153)
#     Run 46131638614 kept no events, so beside cpu/ each holds the events of the same job of
#     INFRA-6313's run 46131305951, whose jobs ran the same packages:
#       gh run download 46131305951 -R cybermonklabs/legion -n go-test-events-<job id> -D <dir>
#     for leg 5 (140937008585), leg 2 (140937008517), the skip counter (140937008472) and alone
#     (140937008441), each file under its own name, cut to the packages of the rows kept: of each,
#     its package-level events and those of the first two top-level tests it ran, with their
#     subtests, but for the tests' outputs, which say nothing of the package and cite test files
#     by line, which ci_citations.py refuses under .github/; and each build event of the package's
#     test binary (alone's internal/dispatch/docs.test prints one and still runs). Leg 5's keeps
#     internal/testpg, a package with no test files, too. Its hand-made-build-failed.jsonl is
#     written by hand, not captured: internal/webhook failing to build, in the shapes go 1.27.3's
#     `go test -json` printed for a package that failed to build here, since no captured run failed
#     to build one; the file its compiler error names, handmade.go, is no file of the repository.
#   lscpu-140585658657.txt, raw-runner-facts-140938293854.txt, cpu-stat-140938293848/
#     read by go-cpu.test.sh alone, whose header says how each was captured.
#
# The stand-in serves the first excerpt as its own job's log, the second as the log of leg 2 of
# run 46040403701 and the hand-made one as that of leg 2 of run 46040410283, so that one set of
# runs holds all three, and answers every other job's log as gh answers a log GitHub does not
# hold, `gh: Not Found (HTTP 404)`. It serves run 46131638614's excerpts as their own jobs' logs,
# and its artifacts as their own; INFRA-5794's runs have none.
#
# The cases of `cpu` (P1-P4) and `coverage` (C1-C4) read run 46131638614 with its jobs page cut to
# the four Go jobs whose artifacts are kept, so that every Go job it holds has one, and P2 reads
# it whole. F3 and the applied F5 read it as one of five runs of its commit, since `factors`
# refuses fewer: the other four, 9000000001 to 9000000004, are made up here as copies of it, each
# job id raised by 10^12 for each place the run stands after the first, and each serves leg 5's
# artifact as its own. The copy in the second run has the wall_s of its rows of the Postgres
# packages raised by half, by hand and their CPU left as it was, as a wait on Postgres would. These
# cases read the module from a go.mod of their own, which names the module as packages/envoy/go.mod
# does, beside a tree in which the tests of leg 5's five Postgres packages, as postgres-packages
# found them in packages/envoy on 2026-11-28, import storetest, so a change to packages/envoy
# changes none of them.
#
# Everything is written under $TMPDIR (mktemp -d honours it), never /tmp by name.
#
# Run from anywhere: .github/scripts/go_cpu_record.test.sh
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
reader="$script_dir/go_cpu_record.py"
fixtures="$script_dir/testdata/go-cpu"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

six=(46040397906 46040403701 46040410283 46040421210 46040426159 46040431439)
sha=09bf0bc6aa0aaa1a1d208260d5d6447196db1d38
leg1="envoy-go / envoy-go (leg 1)"
leg2="envoy-go / envoy-go (leg 2)"
leg4="envoy-go / envoy-go (leg 4)"
checks="envoy-go-checks / envoy-go-checks"
skips="envoy-go / envoy-go (skip counter)"
aggregator="envoy-go / envoy-go"

mkdir -p "$work/bin"
cat > "$work/bin/gh" << 'EOF'
#!/usr/bin/env bash
# A stand-in for gh: answers the reader's four reads, a run, a page of its first attempt's jobs, a
# job's log and an artifact's download, from the files under $GH_FIXTURE, logs each call to
# $GH_CALLS, and refuses every other call with exit 1.
set -euo pipefail
printf '%s\n' "$*" >> "$GH_CALLS"
refuse() {
  echo "stand-in gh: no answer for: $*" >&2
  exit 1
}
if [[ ${1:-} == run && ${2:-} == download ]]; then
  # gh run download <run> -R cybermonklabs/legion -n <name> -D <dir>, the one form the reader
  # runs, which puts the artifact's files into <dir> itself.
  [[ $# -eq 9 && $3 =~ ^[0-9]+$ && $4 == -R && $5 == cybermonklabs/legion && $6 == -n && $8 == -D ]] || refuse "$@"
  if [[ ! -d $GH_FIXTURE/artifacts/$7 ]]; then
    echo "no artifact matches any of the names or patterns provided" >&2
    exit 1
  fi
  mkdir -p "$9"
  cp -R "$GH_FIXTURE/artifacts/$7/." "$9/"
  exit 0
fi
[[ ${1:-} == api ]] || refuse "$@"
escapes=false
endpoint=""
for word in "${@:2}"; do
  case "$word" in
    --allow-escape-sequences) escapes=true ;;
    -*) refuse "$@" ;;
    *)
      [[ -z $endpoint ]] || refuse "$@"
      endpoint=$word
      ;;
  esac
done
base='^repos/cybermonklabs/legion/actions/'
run_path="${base}runs/([0-9]+)\$"
jobs_path="${base}runs/([0-9]+)/attempts/1/jobs[?]per_page=100&page=([0-9]+)\$"
log_path="${base}jobs/([0-9]+)/logs\$"
if [[ $endpoint =~ $run_path ]]; then
  file="$GH_FIXTURE/run-${BASH_REMATCH[1]}.json"
elif [[ $endpoint =~ $jobs_path ]]; then
  file="$GH_FIXTURE/jobs-${BASH_REMATCH[1]}-p${BASH_REMATCH[2]}.json"
elif [[ $endpoint =~ $log_path ]]; then
  if ! $escapes; then
    echo "the response contains terminal escape sequences; pass --allow-escape-sequences to output it anyway" >&2
    exit 1
  fi
  file="$GH_FIXTURE/logs/${BASH_REMATCH[1]}.txt"
else
  refuse "$@"
fi
if [[ ! -f $file ]]; then
  echo "gh: Not Found (HTTP 404)" >&2
  exit 1
fi
cat "$file"
EOF
chmod +x "$work/bin/gh"

# fixture <dir>: a directory the stand-in serves: every run and jobs fixture, and the three logs
# under the job ids the header names.
fixture() {
  mkdir -p "$1/logs"
  cp "$fixtures"/run-*.json "$fixtures"/jobs-*.json "$1"/
  cp "$fixtures/log-140647302858.excerpt.txt" "$1/logs/140647302858.txt"
  cp "$fixtures/log-140585658657.excerpt.txt" "$1/logs/$northcentral.txt"
  cp "$fixtures/log-miss.hand-made.txt" "$1/logs/$miss.txt"
}

# leg2_of <run id>: the job id of leg 2 in that run's jobs fixture.
leg2_of() { jq -r --arg name "$leg2" '.jobs[] | select(.name == $name) | .id' "$fixtures/jobs-$1"-p*.json; }

# fixture_wall <job id>: completed_at − started_at of that job in the jobs fixtures, in seconds.
fixture_wall() {
  jq -r --argjson id "$1" '.jobs[] | select(.id == $id) | (.completed_at | fromdate) - (.started_at | fromdate)' "$fixtures"/jobs-*.json
}

# fixture_step <job id> <step name prefix>: completed_at − started_at of that job's first step
# whose name starts so, in seconds.
fixture_step() {
  jq -r --argjson id "$1" --arg prefix "$2" '.jobs[] | select(.id == $id) | [.steps[] | select(.name | startswith($prefix))][0] | (.completed_at | fromdate) - (.started_at | fromdate)' "$fixtures"/jobs-*.json
}

northcentral=$(leg2_of 46040403701)
miss=$(leg2_of 46040410283)
fixture "$work/fixture"

run1=46131638614
leg5="envoy-go / envoy-go (leg 5)"
alone="envoy-go / envoy-go (alone)"
leg5_job=140938293835
leg2_job=140938293813
skips_job=140938293829
alone_job=140938293806
checks_job=140938293854
kept=("$leg5_job" "$leg2_job" "$skips_job" "$alone_job")

# run1_fixture <dir>: a directory the stand-in serves run 46131638614 from, as the header says:
# its run, its jobs page cut to the four Go jobs whose artifacts are kept, the excerpts of those
# four and of envoy-go-checks as their jobs' logs, and the four artifacts.
run1_fixture() {
  mkdir -p "$1/logs" "$1/artifacts"
  cp "$fixtures/run-$run1.json" "$1/"
  jq -c --argjson kept "[$(IFS=,; echo "${kept[*]}")]" \
    '.jobs |= map(select((.name | startswith("envoy-go / envoy-go (") | not) or (.id as $id | any($kept[]; . == $id)))) | .total_count = (.jobs | length)' \
    "$fixtures/jobs-$run1-p1.json" > "$1/jobs-$run1-p1.json"
  for job in "${kept[@]}" "$checks_job"; do
    cp "$fixtures/log-$job.excerpt.txt" "$1/logs/$job.txt"
  done
  for job in "${kept[@]}"; do
    cp -R "$fixtures/go-test-events-$job" "$1/artifacts/"
  done
}
run1_fixture "$work/run1"

# The module the cases of the CPU record read, as the header says: its go.mod's module line, and
# a test file in each of leg 5's nine packages kept, those of the five Postgres ones importing
# storetest.
module="$work/module"
postgres=(internal/pgmigrate loadtest internal/pglisten internal/dispatch/asof internal/testpg/testdb)
mkdir -p "$module"
printf 'module github.com/sjawhar/envoy\n\ngo 1.27.3\n' > "$module/go.mod"
for package in "${postgres[@]}" internal/testnats internal/cistore clitest internal/bus/kvrecords; do
  mkdir -p "$module/$package"
  printf 'package p\n\nimport "testing"\n\nfunc TestP(t *testing.T) {}\n' > "$module/$package/p_test.go"
done
for package in "${postgres[@]}"; do
  printf 'package p\n\nimport "github.com/sjawhar/envoy/internal/dispatch/store/storetest"\n\nvar _ = storetest.Open\n' > "$module/$package/db_test.go"
done

# The five runs of F3 and the applied F5, as the header says: run 46131638614 with its whole jobs
# page, its logs and its artifacts, and four made-up copies of it, each with leg 5's artifact.
five=("$run1" 9000000001 9000000002 9000000003 9000000004)
copy_shift=1000000000000
mkdir -p "$work/five/logs" "$work/five/artifacts"
for k in "${!five[@]}"; do
  jq -c --argjson run "${five[$k]}" '.id = $run' "$fixtures/run-$run1.json" > "$work/five/run-${five[$k]}.json"
  jq -c --argjson run "${five[$k]}" --argjson shift "$((k * copy_shift))" '.jobs |= map(.id += $shift | .run_id = $run)' \
    "$fixtures/jobs-$run1-p1.json" > "$work/five/jobs-${five[$k]}-p1.json"
  cp -R "$fixtures/go-test-events-$leg5_job" "$work/five/artifacts/go-test-events-$((leg5_job + k * copy_shift))"
done
for job in "${kept[@]}" "$checks_job"; do
  cp "$fixtures/log-$job.excerpt.txt" "$work/five/logs/$job.txt"
done
for job in "$leg2_job" "$skips_job" "$alone_job"; do
  cp -R "$fixtures/go-test-events-$job" "$work/five/artifacts/"
done
# The second run's leg 5 waits on Postgres: each row of a Postgres package takes half as long
# again on the wall, for the same CPU.
for row in "$work/five/artifacts/go-test-events-$((leg5_job + copy_shift))"/cpu/*.json; do
  cwd=$(jq -r .cwd "$row")
  if [[ " ${postgres[*]} " == *" ${cwd#packages/envoy/} "* ]]; then
    jq -c '.wall_s *= 1.5' "$row" > "$row.scaled"
    mv "$row.scaled" "$row"
  fi
done

# read_with <fixture dir> <argument>...: runs the reader with the stand-in gh serving <fixture
# dir> and TMPDIR an empty directory of its own; sets out (its stdout), err (its stderr), status,
# calls (the gh calls it made, one a line) and left (what it left in its TMPDIR).
read_with() {
  local dir=$1
  shift
  rm -rf "$work/tmp"
  mkdir -p "$work/tmp"
  : > "$work/calls"
  status=0
  out=$(TMPDIR="$work/tmp" GH_FIXTURE="$dir" GH_CALLS="$work/calls" PATH="$work/bin:$PATH" "$reader" "$@" 2> "$work/err") || status=$?
  err=$(< "$work/err")
  calls=$(< "$work/calls")
  left=$(ls -A "$work/tmp")
}

# read_run <argument>...: read_with over the fixtures as the header says.
read_run() { read_with "$work/fixture" "$@"; }

# pick <title> <column>=<value> <column>...: from the table of $out whose title line starts
# `== <title>`, the rows whose <column> is <value>, each as its values of the other columns,
# tab-separated. A missing table or column prints as such, so it fails the check that reads it
# rather than ending this script.
pick() {
  python3 -c '
import sys

title, (key, value), columns = sys.argv[1], sys.argv[2].split("=", 1), sys.argv[3:]
lines = sys.stdin.read().split("\n")
starts = [i for i, line in enumerate(lines) if line.startswith("== " + title)]
if not starts:
    print("no table " + title)
    sys.exit(0)
header = lines[starts[0] + 1].split("\t")
for line in lines[starts[0] + 2:]:
    if not line:
        break
    row = dict(zip(header, line.split("\t")))
    if row.get(key) == value:
        print("\t".join(row.get(c, "no column " + c) for c in columns))
' "$@" <<< "$out"
}

# count_calls <regex>: how many of $calls match <regex>.
count_calls() { grep -c -- "$1" <<< "$calls" || true; }

echo "case S1: spread refuses fewer than five runs, and a run given twice"
read_run spread --runs "${six[@]:0:4}"
check "four runs exit 2" "$(is "$status" 2)"
check "saying it needs five" "$(contains "$err" 'at least five runs')"
check "before any call to gh" "$(is "$calls" "")"
read_run spread --runs "${six[@]:0:4}" 46040397906
check "five ids, one of them twice, exit 2 naming it" "$(is "$status:$(contains "$err" 'given more than once: 46040397906')" 2:true)"

echo "case S2: spread refuses five of INFRA-5794's runs beside a run of another commit"
read_run spread --runs "${six[@]:0:5}" 46084080784
check "it exits 2" "$(is "$status" 2)"
check "naming 46084080784 and its sha" "$(contains "$err" '46084080784 91e57a4886cb323c388287c9184e7e02f2cb1a9a')"
check "and each other run with its own" "$(is "$(grep -c " $sha" <<< "$err")" 5)"
check "having read no run's jobs" "$(is "$(count_calls /jobs)" 0)"

echo "case S3: spread refuses a run whose run_attempt is not 1"
mkdir -p "$work/attempt2"
fixture "$work/attempt2"
jq -c '.run_attempt = 2' "$fixtures/run-46040431439.json" > "$work/attempt2/run-46040431439.json"
read_with "$work/attempt2" spread --runs "${six[@]}"
check "it exits 2" "$(is "$status" 2)"
check "naming the run and its attempt" "$(contains "$err" 'run 46040431439 is attempt 2')"
check "having read no run's jobs" "$(is "$(count_calls /jobs)" 0)"
read_run spread --runs "${six[@]:0:5}" 1
check "and a run gh cannot read exits 2 with gh's reason" "$(is "$status:$(contains "$err" 'actions/runs/1 exited 1: gh: Not Found (HTTP 404)')" 2:true)"

echo "case S4: spread over INFRA-5794's six runs"
read_run spread --runs "${six[@]}"
check "leg 1: 6 job-runs, 713, 715 and 732 s, 2.7 %" "$(is "$(pick spread "job=$leg1" n min_s median_s max_s spread)" $'6\t713\t715\t732\t2.7 %')"
check "leg 4: 302, 328 and 348 s, 14.0 %" "$(is "$(pick spread "job=$leg4" n min_s median_s max_s spread)" $'6\t302\t328\t348\t14.0 %')"
check "envoy-go-checks: 403, 525.5 and 650 s, 47.0 %" "$(is "$(pick spread "job=$checks" n min_s median_s max_s spread)" $'6\t403\t525.5\t650\t47.0 %')"
check "with the job ids of each one's min and max" "$(is "$(pick spread "job=$leg1" min_job max_job)|$(pick spread "job=$leg4" min_job max_job)|$(pick spread "job=$checks" min_job max_job)" $'140647678558\t140646999944|140647678439\t140646999777|140646999273\t140647677367')"
min4=$(pick spread "job=$leg4" min_job)
check "leg 4's min job-run is listed with its wall, which is completed_at − started_at of its job id in the fixture" "$(is "$(pick job-runs "id=$min4" job conclusion wall_s)" "$leg4"$'\tsuccess\t'"$(fixture_wall "$min4")")"
check "every job-run listed: the 11 counted jobs of each of the six runs" "$(is "$(pick job-runs "conclusion=success" id | wc -l):$(pick job-runs "conclusion=failure" id | wc -l)" 54:12)"
spread_ids=$(pick job-runs "conclusion=success" id && pick job-runs "conclusion=failure" id)

echo "case S5: the jobs that failed in all six runs have no spread, and spread exits 1 naming them"
check "the skip counter: no successful job-run and no spread, six failures" "$(is "$(pick spread "job=$skips" n min_s median_s max_s spread min_job max_job not_counted)" $'0\t-\t-\t-\t-\t-\t-\tfailure 6')"
check "the aggregator the same" "$(is "$(pick spread "job=$aggregator" n spread not_counted)" $'0\t-\tfailure 6')"
check "each of their six job-runs is listed with its conclusion" "$(is "$(pick job-runs "job=$skips" conclusion | uniq -c | tr -s ' '):$(pick job-runs "job=$aggregator" conclusion | uniq -c | tr -s ' ')" ' 6 failure: 6 failure')"
check "the aggregator of run 46040397906 is 140656851170, on page 2 of its jobs" "$(is "$(pick job-runs "id=140656851170" run job conclusion)" $'46040397906\tenvoy-go / envoy-go\tfailure')"
check "it read pages 1 and 2 of each run's jobs and no third" "$(is "$(count_calls 'page=1$'):$(count_calls 'page=2$'):$(count_calls 'page=3$')" 6:6:0)"
check "it exits 1" "$(is "$status" 1)"
check "naming the two jobs with fewer than five successful job-runs, and no other" "$(is "$(grep -c 'fewer than five' <<< "$err"):$(contains "$err" "$skips (0)"):$(contains "$err" "$aggregator (0)"):$(contains "$err" "$leg1")" 1:true:true:false)"

read_run factors --runs "${six[@]}"
factors_status=$status

echo "case F1: factors reads the Region, the Worker ID and the image's Version from the captured log excerpts"
check "it exits 0" "$(is "$factors_status" 0)"
check "job 140647302858: centralus, its worker, and the image's version, not the provisioner's" "$(is "$(pick factors id=140647302858 region worker_id image_version)" $'centralus\t{027fb59d-b96c-4b46-b497-4a79a1bd0697}\t20261122.193.1')"
check "the job served 140585658657's excerpt: northcentralus and that job's worker" "$(is "$(pick factors "id=$northcentral" region worker_id image_version)" $'northcentralus\t{a8b9f97e-0d1a-44af-b46a-762737943959}\t20261122.193.1')"
check "a job whose log gh does not have: each of its log's factors UNKNOWN" "$(is "$(pick factors id=140647302735 region worker_id image_version model cpus probe_one busy postgres_cpu cache_kind)" $'UNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN')"
check "job 140647302858's wall and d: 448 s over leg 2's median of 459 s, less 1" "$(is "$(pick factors id=140647302858 conclusion wall_s d)" $'success\t448\t-0.0240')"
check "its queue time: its started_at less its run's created_at, 17:23:04 less 17:15:33" "$(is "$(pick factors id=140647302858 queue_s)" 451)"
check "it read the log of each of the 66 counted job-runs, each with --allow-escape-sequences" "$(is "$(count_calls '^api --allow-escape-sequences repos/cybermonklabs/legion/actions/jobs/[0-9]*/logs$'):$(count_calls /logs)" 66:66)"
check "and of no other job" "$(is "$(sed -n 's|.*/actions/jobs/\([0-9]*\)/logs$|\1|p' <<< "$calls" | sort)" "$(sort <<< "$spread_ids")")"
check "it says which logs it could not read, and why" "$(is "$(pick 'logs not read' id=140647302735 why)" 'gh api --allow-escape-sequences repos/cybermonklabs/legion/actions/jobs/140647302735/logs exited 1: gh: Not Found (HTTP 404)')"
check "and leaves nothing in its temporary directory" "$(is "$left" "")"
check "the stand-in refuses a log read without --allow-escape-sequences with gh's message" "$(is "$(GH_FIXTURE="$work/fixture" GH_CALLS="$work/calls" "$work/bin/gh" api repos/cybermonklabs/legion/actions/jobs/140647302858/logs 2>&1 > /dev/null || echo "exit $?")" $'the response contains terminal escape sequences; pass --allow-escape-sequences to output it anyway\nexit 1')"

echo "case F2: the restored cache key and whether it hit, and the restore step's seconds"
check "job 140647302858: a restore-key hit, and the key it restored" "$(is "$(pick factors id=140647302858 cache_kind cache_key)" $'restore-key\tgo-envoy-test-Linux-ARM64-go1.27.3-141e0e3ae129e2d89ae28522473caf17c5242170')"
check "the hand-made log: a miss, which restored no key" "$(is "$(pick factors "id=$miss" cache_kind cache_key)" $'miss\tnone')"
check "the probe job's excerpt, which restored no cache: UNKNOWN" "$(is "$(pick factors "id=$northcentral" cache_kind cache_key)" $'UNKNOWN\tUNKNOWN')"
check "the restore step's seconds are those of the jobs fixture's Run actions/cache/restore step" "$(is "$(pick factors id=140647302858 cache_restore_s)" "$(fixture_step 140647302858 'Run actions/cache/restore')")"
check "the region's statistic over all job-runs leaves out the 63 whose log gave none" "$(is "$(pick 'statistics: all job-runs, whole wall, Spearman' factor=region n left_out values)" $'3\t63\t2')"
check "and over the successful ones, the 51 of 54" "$(is "$(pick 'statistics: successful job-runs, whole wall, Spearman' factor=region n left_out)" $'3\t51')"
check "every branch has its statistics and a verdict" "$(is "$(grep -c '^== statistics: ' <<< "$out"):$(grep -c '^cause: ' <<< "$out")" 8:8)"
check "a Go job-run whose artifact gh does not have: each of its six per-package ratios UNKNOWN" "$(is "$(pick factors id=140647302858 package_cpu package_wall postgres_package_cpu postgres_package_wall other_package_cpu other_package_wall)" $'UNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN')"
check "and it is listed under artifacts not read, with what gh said" "$(is "$(pick 'artifacts not read' id=140647302858 why)" 'no artifact go-test-events-140647302858')"
six_pages=()
for run in "${six[@]}"; do
  six_pages+=("$fixtures/jobs-$run"-p*.json)
done
go_ran=$(jq -r '.jobs[] | select((.name | startswith("envoy-go / envoy-go (")) and .conclusion != "skipped") | .id' "${six_pages[@]}" | sort)
check "it downloaded, by its name, the artifact of each Go job-run that ran, and of no other job" "$(is "$(sed -n 's|^run download [0-9]* -R cybermonklabs/legion -n go-test-events-\([0-9]*\) -D .*|\1|p' <<< "$calls" | sort)" "$go_ran")"
check "each into its temporary directory, which it leaves empty, and read no list of artifacts" "$(is "$(count_calls "^run download [0-9]* -R cybermonklabs/legion -n go-test-events-[0-9]* -D $work/tmp/"):$(count_calls /artifacts):$left" "$(wc -l <<< "$go_ran"):0:")"

echo "case F3: factors reads the go-cpu runner: and go-cpu usage: lines of run 46131638614's logs"
read_with "$work/five" factors --runs "${five[@]}" --root "$module"
five_out=$out
check "factors over the five runs of its commit exits 0" "$(is "$status" 0)"
check "leg 5's excerpt prints these runner and usage lines, between which its job line stands" "$(is "$(cut -c30- "$fixtures/log-$leg5_job.excerpt.txt" | grep -c '^go-cpu ' || true):$(contains "$(< "$fixtures/log-$leg5_job.excerpt.txt")" ' probe_one=2162.6 probe_all=8345.8 .* model="Neoverse-N2"$'):$(contains "$(< "$fixtures/log-$leg5_job.excerpt.txt")" 'go-cpu usage: busy=267.43 user=180.49 sys=86.94 iowait=4.52 steal=0.00 idle=233.64 new_cache_entries=596 postgres_cpu=45.31$')" 3:true:true)"
check "leg 5: the runner line's model, cpus and probes, and the usage line's busy, steal, iowait, new cache entries and Postgres CPU" "$(is "$(pick factors "id=$leg5_job" model cpus probe_one probe_all busy steal iowait new_cache_entries postgres_cpu)" $'Neoverse-N2\t4\t2162.6\t8345.8\t267.43\t0\t4.52\t596\t45.31')"
check "the skip counter's, from its own lines" "$(is "$(pick factors "id=$skips_job" model cpus probe_one probe_all busy steal iowait new_cache_entries postgres_cpu)" $'Neoverse-N2\t4\t2162.9\t7119.6\t391.05\t0\t2.29\t4826\t2.99')"
check "alone's" "$(is "$(pick factors "id=$alone_job" model cpus probe_one probe_all busy steal iowait new_cache_entries postgres_cpu)" $'Neoverse-N2\t4\t2163.1\t8340.6\t255.06\t0\t4.02\t556\t123.82')"
check "envoy-go-checks, whose log prints no job line and a usage line without the cache entries and the Postgres CPU: its x64 model, spaces and all, and those two UNKNOWN" "$(is "$(grep -c 'go-cpu job:' "$fixtures/log-$checks_job.excerpt.txt" || true):$(pick factors "id=$checks_job" model cpus probe_one probe_all busy steal iowait new_cache_entries postgres_cpu)" $'0:INTEL(R) XEON(R) PLATINUM 8573C\t4\t1456.1\t3362.7\t1844.79\t0\t8.34\tUNKNOWN\tUNKNOWN')"
check "and leg 5's Set up job and cache lines: centralus, its worker, the arm image's version, a restore-key hit" "$(is "$(pick factors "id=$leg5_job" region worker_id image_version cache_kind)" $'centralus\t{ee44a854-d24c-4e46-9847-038119c94e44}\t20261122.193.1\trestore-key')"

echo "case F4: the decision rule over tables built by hand"
# A: d rises as probe falls, image has one value and worker a value for each job-run. The bar is
# half the median spread: 0.1.
cat > "$work/a.json" << 'EOF'
{"median_spread": 0.2, "rows": [
  {"d": -0.10, "probe": 1300, "image": "v1", "worker": "w1"},
  {"d": -0.05, "probe": 1250, "image": "v1", "worker": "w2"},
  {"d": 0.00, "probe": 1200, "image": "v1", "worker": "w3"},
  {"d": 0.05, "probe": 1150, "image": "v1", "worker": "w4"},
  {"d": 0.10, "probe": 1100, "image": "v1", "worker": "w5"},
  {"d": 0.15, "probe": 1050, "image": null, "worker": "w6"}
]}
EOF
read_run rule --table "$work/a.json"
check "a numeric factor that tracks d is named with its rho and effect" "$(contains "$out" '^cause: probe (rho=-1.000, effect=-0.2500)$')"
check "a factor of one value is excluded with that value, its unknown row left out" "$(is "$(pick statistics factor=image n left_out meets note)" $'5\t1\texcluded\tone value: v1')"
check "a factor of one job-run a value is not named" "$(is "$(pick statistics factor=worker statistic value meets note)" $'eta2\t1.000\tno\ta value with one job-run')"
check "it exits 0, and called no gh" "$(is "$status:$calls" 0:)"

# B: d is low in region a and high in region b; zone's third value has one job-run.
cat > "$work/b.json" << 'EOF'
{"median_spread": 0.2, "rows": [
  {"d": -0.12, "region": "a", "zone": "x"},
  {"d": -0.10, "region": "a", "zone": "x"},
  {"d": -0.08, "region": "a", "zone": "x"},
  {"d": 0.08, "region": "b", "zone": "y"},
  {"d": 0.10, "region": "b", "zone": "y"},
  {"d": 0.12, "region": "b", "zone": "z"}
]}
EOF
read_run rule --table "$work/b.json"
check "a categorical factor with eta2 at least 0.5 and two job-runs a value is named" "$(contains "$out" '^cause: region (eta2=0.974, effect=0.2000)$')"
check "one with a value of a single job-run is not, though its eta2 is higher" "$(is "$(pick statistics factor=zone value meets note)" $'0.984\tno\ta value with one job-run')"

# C: noise and region do not track d, and steady tracks it but by less than the bar, half of a
# median spread of 1.0.
cat > "$work/c.json" << 'EOF'
{"median_spread": 1.0, "rows": [
  {"d": -0.10, "noise": 3, "region": "a", "steady": 5.0},
  {"d": -0.06, "noise": 1, "region": "b", "steady": 5.2},
  {"d": -0.02, "noise": 2, "region": "a", "steady": 5.4},
  {"d": 0.02, "noise": 3, "region": "b", "steady": 5.6},
  {"d": 0.06, "noise": 1, "region": "a", "steady": 5.8},
  {"d": 0.10, "noise": 2, "region": "b", "steady": 6.0}
]}
EOF
read_run rule --table "$work/c.json"
check "where no factor meets its bar: cause: not separated" "$(contains "$out" '^cause: not separated$')"
check "steady's rho is 1.000, and its effect of 0.2000 is under the bar of 0.5" "$(is "$(pick statistics factor=steady value effect meets note)" $'1.000\t0.2000\tno\teffect under the bar')"
read_run rule --table "$work/c.json" --method pearson
check "with Pearson in place of Spearman, the same" "$(contains "$out" '^cause: not separated$')"
check "the Pearson branch names its statistic r" "$(is "$(pick statistics factor=steady statistic value)" $'r\t1.000')"

# D: probe and region both meet their bars, probe with the larger effect.
cat > "$work/d.json" << 'EOF'
{"median_spread": 0.2, "rows": [
  {"d": -0.10, "probe": 1300, "region": "a"},
  {"d": -0.08, "probe": 1280, "region": "a"},
  {"d": -0.06, "probe": 1260, "region": "a"},
  {"d": 0.06, "probe": 1060, "region": "b"},
  {"d": 0.08, "probe": 1040, "region": "b"},
  {"d": 0.10, "probe": 1020, "region": "b"}
]}
EOF
read_run rule --table "$work/d.json"
check "where two meet, each is printed with its numbers" "$(is "$(grep '^meets: ' <<< "$out")" $'meets: probe (rho=-1.000, effect=-0.1884)\nmeets: region (eta2=0.960, effect=0.1600)')"
check "the one of the larger effect is the cause" "$(contains "$out" '^cause: probe (rho=-1.000, effect=-0.1884)$')"
check "and its statistic within each value of the second" "$(is "$(grep '^within ' <<< "$out")" $'within region=a (n=3): probe rho=-1.000\nwithin region=b (n=3): probe rho=-1.000')"

echo "case F5: the package directories whose tests use Postgres, and the others"
tree="$work/envoy"
mkdir -p "$tree/store" "$tree/broker" "$tree/comment" "$tree/lookalike" "$tree/aliased" "$tree/plain" "$tree/testdata/ignored" "$tree/nested"
cat > "$tree/store/store_test.go" << 'EOF'
package store

import (
	"testing"

	"github.com/cybermonklabs/legion/packages/envoy/internal/dispatch/store/storetest"
)

func TestStore(t *testing.T) { storetest.Open(t) }
EOF
cat > "$tree/broker/broker_test.go" << 'EOF'
package broker

import (
	"os"
	"testing"
)

func TestBroker(t *testing.T) { _ = os.Getenv("BROKER_TEST_DATABASE_URL") }
EOF
cat > "$tree/comment/comment.go" << 'EOF'
// Package comment is read by tests elsewhere that set DISPATCH_TEST_DATABASE_URL.
package comment
EOF
cat > "$tree/comment/comment_test.go" << 'EOF'
package comment

import "testing"

func TestComment(t *testing.T) {}
EOF
cat > "$tree/lookalike/lookalike_test.go" << 'EOF'
package lookalike

import (
	// "example.com/storetest" was this test's import once; a comment is no import.
	"example.com/other/notstoretest"
)

var _ = notstoretest.X
EOF
cat > "$tree/aliased/aliased_test.go" << 'EOF'
package aliased

import (
	"testing"

	pg "github.com/cybermonklabs/legion/packages/envoy/internal/testpg/testdb"
)

func TestAliased(t *testing.T) { pg.Open(t) }
EOF
cat > "$tree/plain/plain.go" << 'EOF'
package plain
EOF
cat > "$tree/testdata/ignored/ignored_test.go" << 'EOF'
package ignored

import "example.com/storetest"
EOF
printf 'module example.com/nested\n\ngo 1.27.3\n' > "$tree/nested/go.mod"
cat > "$tree/nested/nested_test.go" << 'EOF'
package nested

import "example.com/storetest"
EOF
read_run postgres-packages --root "$tree"
check "a package whose test imports storetest uses Postgres" "$(is "$(pick packages package=store postgres why)" $'yes\tstore/store_test.go imports github.com/cybermonklabs/legion/packages/envoy/internal/dispatch/store/storetest')"
check "so does one whose test names BROKER_TEST_DATABASE_URL" "$(is "$(pick packages package=broker postgres why)" $'yes\tbroker/broker_test.go names BROKER_TEST_DATABASE_URL')"
check "and one whose test imports testpg/testdb under another name" "$(is "$(pick packages package=aliased postgres)" yes)"
check "a package that names the variable only in a comment of a file that is no test does not" "$(is "$(pick packages package=comment postgres)" no)"
check "nor one whose import only ends in the same letters, under a comment that names storetest" "$(is "$(pick packages package=lookalike postgres)" no)"
check "nor one with no test file" "$(is "$(pick packages package=plain postgres)" no)"
check "and a directory go ignores, testdata, is no package" "$(is "$(pick packages package=testdata/ignored postgres)" "")"
check "nor is a nested module, a directory with its own go.mod, which go test ./... here does not walk" "$(is "$(pick packages package=nested postgres)" "")"
check "it exits 0" "$(is "$status" 0)"

echo "case F5 applied: the per-package ratios over five runs of leg 5, whose second waits on Postgres"
out=$five_out
# ratios <job id>: that job-run's six per-package ratios, in the order the factors table prints them.
ratios() { pick factors "id=$1" package_cpu package_wall postgres_package_cpu postgres_package_wall other_package_cpu other_package_wall; }
check "the second run's leg 5: its Postgres packages' wall ratio 1.5 and CPU ratio 1, its other packages' both 1, and over all nine, five of them Postgres ones, CPU 1 and wall 1.5" "$(is "$(ratios $((leg5_job + copy_shift)))" $'1.0000\t1.5000\t1.0000\t1.5000\t1.0000\t1.0000')"
check "each other run's leg 5, whose packages' seconds are each package's median: every ratio 1" "$(is "$(for k in 0 2 3 4; do ratios $((leg5_job + k * copy_shift)); done | sort -u)" $'1.0000\t1.0000\t1.0000\t1.0000\t1.0000\t1.0000')"
check "leg 2 of run 46131638614, whose packages have no directory under --root: its ratios over all its packages, and none in either group" "$(is "$(ratios "$leg2_job")" $'1.0000\t1.0000\tUNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN')"
check "leg 2 of the second run, which kept no artifact: every ratio UNKNOWN, and listed under artifacts not read" "$(is "$(ratios $((leg2_job + copy_shift))):$(pick 'artifacts not read' "id=$((leg2_job + copy_shift))" why)" $'UNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN\tUNKNOWN:no artifact go-test-events-'"$((leg2_job + copy_shift))")"
check "envoy-go-checks, which keeps no artifact, is not listed there" "$(is "$(pick 'artifacts not read' "id=$checks_job" why)" "")"
check "postgres_package_wall is a numeric factor of the decision rule, which the five leg 5 job-runs give" "$(is "$(pick 'statistics: all job-runs, whole wall, Spearman' factor=postgres_package_wall kind n)" $'numeric\t5')"

echo "case P1: cpu sums each package's test rows, its own CPU and its children's apart, leaves nested rows out and lists the others apart"
read_with "$work/run1" cpu --run "$run1" --root "$module"
p1_out=$out
check "it exits 0, and prints nothing on stderr" "$(is "$status:$err" 0:)"
check "the skip counter's ./internal/cipher/api: its two test rows' user_s + sys_s, self, children and wall_s added up" "$(is "$(pick "packages: $skips" package=./internal/cipher/api import_path cpu_s self_s children_s wall_s binaries)" $'github.com/sjawhar/envoy/internal/cipher/api\t0.23\t0.21\t0.00\t0.44\t2')"
check "leg 5's ./clitest, whose CPU is nearly all its children's" "$(is "$(pick "packages: $leg5" package=./clitest import_path cpu_s self_s children_s wall_s binaries)" $'github.com/sjawhar/envoy/clitest\t3.06\t0.08\t2.95\t15.20\t1')"
check "leg 2's ./cmd/cipher: its test row's numbers alone, without the two nested rows under it" "$(is "$(pick "packages: $leg2" package=./cmd/cipher cpu_s self_s children_s wall_s binaries)" $'45.43\t14.27\t31.14\t421.49\t1')"
check "which are counted apart" "$(is "$(pick 'cpu: run' "job=$leg2" test nested)" $'2\t2')"
check "the skip counter's run row is listed apart and out of the total, its list and outside rows apart and in it" "$(is "$(pick "rows apart: $skips" class=run cwd binary cpu_s in_total)|$(pick "rows apart: $skips" class=list binary cpu_s in_total)|$(pick "rows apart: $skips" class=outside cwd cpu_s in_total)" $'packages/envoy\tconnections\t0.01\tno|api.test\t0.06\tyes|/tmp/tmp.gFUlrwAyhK/tree/packages/envoy/internal/dispatch/api\t0.06\tyes')"
check "the skip counter: five test rows, a list, an outside and a run row, and 12.32 s, as its log says" "$(is "$(pick 'cpu: run' "job=$skips" id test list outside run nested bad cpu_s log_cpu_s verdict)" "$skips_job"$'\t5\t1\t1\t1\t0\t0\t12.32\t12.32\tagrees')"
check "a Go job the cut page does not hold is listed as absent" "$(is "$(pick 'cpu: run' "job=$leg1" id verdict)" $'-\tabsent')"
check "it downloaded each of the four artifacts once, by its name, read each one's log, and read no list of artifacts" "$(is "$(count_calls '^run download '):$(count_calls '^api --allow-escape-sequences .*/logs$'):$(count_calls /artifacts)" 4:4:0)"
check "and it leaves no download behind" "$(is "$left" "")"

echo "case P2: a Go job with no artifact, and one whose artifact holds no row, print UNKNOWN and exit 1"
cp -R "$work/run1" "$work/p2"
cp "$fixtures/jobs-$run1-p1.json" "$work/p2/"
rm "$work/p2/artifacts/go-test-events-$alone_job"/cpu/*.json
read_with "$work/p2" cpu --run "$run1" --root "$module"
check "over the whole jobs page and alone's artifact without its rows, cpu exits 1" "$(is "$status" 1)"
check "leg 1, whose artifact gh does not have: UNKNOWN, with what gh said" "$(is "$(pick 'cpu: run' "job=$leg1" cpu_s verdict)" $'UNKNOWN\tUNKNOWN: no artifact go-test-events-140938293923')"
check "alone, whose artifact holds cpu/runner/ and no row: UNKNOWN" "$(is "$(pick 'cpu: run' "job=$alone" cpu_s verdict)" $'UNKNOWN\tUNKNOWN: no row in the cpu/ of artifact go-test-events-'"$alone_job")"
check "stderr names each of the six: legs 1, 3 and 4, race-impossible, natslive and alone" "$(is "$(grep -c '^go_cpu_record.py cpu: .*: UNKNOWN: ' <<< "$err"):$(contains "$err" "^go_cpu_record.py cpu: $alone ($alone_job): UNKNOWN: ")" 6:true)"
check "the jobs that have rows are printed still, and agree" "$(is "$(pick 'cpu: run' "job=$leg5" verdict)" agrees)"
check "and it leaves no download behind" "$(is "$left" "")"

echo "case P3: each job's total is its log's cpu=, and with one row fewer it is not"
out=$p1_out
# logged <job id>: the cpu= of that job's excerpt's go-cpu job: line.
logged() { sed -n 's/.* go-cpu job: .* cpu=\([^ ]*\) .*/\1/p' "$fixtures/log-$1.excerpt.txt"; }
check "each of the four jobs' total is its log's cpu=, and agrees" "$(is "$(for job in "${kept[@]}"; do pick 'cpu: run' "id=$job" cpu_s log_cpu_s verdict; done)" "$(for job in "${kept[@]}"; do printf '%s\t%s\tagrees\n' "$(logged "$job")" "$(logged "$job")"; done)")"
check "leg 5's is 124.42" "$(is "$(pick 'cpu: run' "job=$leg5" cpu_s)" 124.42)"
cp -R "$work/run1" "$work/p3"
rm "$work/p3/artifacts/go-test-events-$leg5_job"/cpu/*-3560.json
read_with "$work/p3" cpu --run "$run1" --root "$module"
check "without leg 5's row of internal/pgmigrate, 97.89 s, cpu exits 1" "$(is "$status" 1)"
check "naming leg 5, its total and its log's, and no other job" "$(is "$err" "go_cpu_record.py cpu: $leg5 ($leg5_job): differs: 26.53 here, 124.42 in its log")"

echo "case P4: cpu reads the artifact as it is laid out: rows in cpu/, the runner's files in cpu/runner/, events beside cpu/"
out=$p1_out
check "leg 5's ten files in cpu/ are its rows, nine test and one run, and none of the files in cpu/runner/ counts, as a row or as a file that is no row" "$(is "$(find "$fixtures/go-test-events-$leg5_job/cpu" -maxdepth 1 -name '*.json' | wc -l):$(pick 'cpu: run' "job=$leg5" test run bad)" $'10:9\t1\t0')"
lacking=""
for job in "${kept[@]}"; do
  if [ ! -s "$fixtures/go-test-events-$job/cpu/runner/stat-start.txt" ] || ! compgen -G "$fixtures/go-test-events-$job/*.jsonl" > /dev/null; then
    lacking+="$job "
  fi
done
check "each kept artifact holds the runner's files in cpu/runner/ and events beside cpu/" "$(is "$lacking" "")"
cp -R "$work/run1" "$work/p4"
for job in "${kept[@]}"; do
  rm -r "$work/p4/artifacts/go-test-events-$job/cpu/runner" "$work/p4/artifacts/go-test-events-$job"/*.jsonl
done
read_with "$work/p4" cpu --run "$run1" --root "$module"
check "with cpu/runner/ and the events taken out of every artifact, cpu prints the same" "$(is "$status:$out" "0:$p1_out")"
cp -R "$work/run1" "$work/p4-bad"
printf '{"v": 1, "id": "1-1",' > "$work/p4-bad/artifacts/go-test-events-$leg5_job/cpu/1-1.json"
# A row the wrapper is still writing, under the dot name it renames into place, is no row yet.
jq -c '.user_s = 100' "$fixtures/go-test-events-$leg5_job/cpu/1795859065694326460-3113.json" | jq -c '.binary = "x.test"' > "$work/p4-bad/artifacts/go-test-events-$leg5_job/cpu/.2-2.json"
read_with "$work/p4-bad" cpu --run "$run1" --root "$module"
check "a file in cpu/ that is no row counts as bad, and a dot file is skipped, so the total and its agreement stand" "$(is "$status:$(pick 'cpu: run' "job=$leg5" test bad cpu_s verdict)" $'0:9\t1\t124.42\tagrees')"

echo "case C1: coverage passes when each package the events show running a test binary has a test row"
read_with "$work/run1" coverage --run "$run1" --root "$module"
c1_out=$out
check "it exits 0, and prints nothing on stderr" "$(is "$status:$err" 0:)"
check "each of the four jobs is covered" "$(is "$(for job in "${kept[@]}"; do pick 'coverage: run' "id=$job" verdict; done | sort -u)" covered)"
check "leg 5: two events files, nine packages that ran a test binary, nine covered" "$(is "$(pick 'coverage: run' "job=$leg5" events_files bad_lines ran covered missing)" $'2\t0\t9\t9\t0')"
check "its internal/pgmigrate, with its one row" "$(is "$(pick "coverage: $leg5" package=github.com/sjawhar/envoy/internal/pgmigrate events rows verdict)" $'ran a test binary\t1\tcovered')"
check "the skip counter's internal/cipher/api, with its two" "$(is "$(pick "coverage: $skips" package=github.com/sjawhar/envoy/internal/cipher/api events rows verdict)" $'ran a test binary\t2\tcovered')"
check "alone's internal/dispatch/docs, whose test binary's build printed a line, ran one all the same" "$(is "$(pick "coverage: $alone" package=github.com/sjawhar/envoy/internal/dispatch/docs events rows verdict)" $'ran a test binary\t1\tcovered')"
check "and it leaves no download behind" "$(is "$left" "")"

echo "case C2: the same with one package's rows removed exits 1 naming that job and package (the negative control of check c-cpu-record)"
cp -R "$work/run1" "$work/c2"
rm "$work/c2/artifacts/go-test-events-$leg5_job"/cpu/*-5951.json
read_with "$work/c2" coverage --run "$run1" --root "$module"
check "without leg 5's row of clitest, coverage exits 1" "$(is "$status" 1)"
check "naming leg 5 and github.com/sjawhar/envoy/clitest, and nothing else" "$(is "$err" "go_cpu_record.py coverage: $leg5 ($leg5_job): github.com/sjawhar/envoy/clitest ran a test binary, and no test or outside row is of it")"
check "whose line in leg 5's table is MISSING" "$(is "$(pick "coverage: $leg5" package=github.com/sjawhar/envoy/clitest events rows verdict)" $'ran a test binary\t0\tMISSING')"
check "and the other jobs are covered still" "$(is "$(pick 'coverage: run' "job=$skips" verdict)" covered)"

echo "case C3: a package with no test files and one that failed to build need no row"
out=$c1_out
check "leg 5's internal/testpg, which has no test files" "$(is "$(pick "coverage: $leg5" package=github.com/sjawhar/envoy/internal/testpg events rows verdict)" $'no test files\t0\tneeds no row')"
check "the hand-made internal/webhook, which failed to build" "$(is "$(pick "coverage: $leg5" package=github.com/sjawhar/envoy/internal/webhook events rows verdict)" $'failed to build\t0\tneeds no row')"
# events_as <dir> <file> <jq filter>: a copy of run 46131638614's fixture in <dir> whose leg 5
# events file <file> is rewritten by <jq filter>, one event a line.
events_as() {
  cp -R "$work/run1" "$1"
  jq -c "$3" "$fixtures/go-test-events-$leg5_job/$2" > "$1/artifacts/go-test-events-$leg5_job/$2"
}
events_as "$work/c3-built" hand-made-build-failed.jsonl 'del(.FailedBuild) | if has("Output") then .Output |= sub(" \\[build failed\\]"; "") else . end'
read_with "$work/c3-built" coverage --run "$run1" --root "$module"
check "the control: without FailedBuild and [build failed], its fail says it ran one, and coverage exits 1 naming it" "$(is "$status:$(contains "$err" "github.com/sjawhar/envoy/internal/webhook ran a test binary")" 1:true)"
events_as "$work/c3-setup" hand-made-build-failed.jsonl 'del(.FailedBuild) | if has("Output") then .Output |= sub("build failed"; "setup failed") else . end'
read_with "$work/c3-setup" coverage --run "$run1" --root "$module"
check "with [setup failed] in their place it needs no row again" "$(is "$status:$(pick "coverage: $leg5" package=github.com/sjawhar/envoy/internal/webhook events verdict)" $'0:failed to build\tneeds no row')"
events_as "$work/c3-pass" database-skips-3q1NYF.jsonl 'if .Package == "github.com/sjawhar/envoy/internal/testpg" and .Action == "skip" then .Action = "pass" else . end'
read_with "$work/c3-pass" coverage --run "$run1" --root "$module"
check "and a package-level pass whose output says [no test files] needs none either" "$(is "$status:$(pick "coverage: $leg5" package=github.com/sjawhar/envoy/internal/testpg events verdict)" $'0:no test files\tneeds no row')"

echo "case C4: list, run, nested and outside rows need no event, and a job with rows and no events is UNKNOWN"
out=$c1_out
check "the skip counter's list and outside rows of internal/dispatch/api, and its test row of a control run, have no events, and need none" "$(is "$(pick "coverage: $skips" package=github.com/sjawhar/envoy/internal/dispatch/api verdict):$(pick 'coverage: run' "job=$skips" verdict)" :covered)"
check "nor do leg 2's nested rows, or any job's run row, of the module's root" "$(is "$(pick "coverage: $leg2" package=github.com/sjawhar/envoy verdict):$(pick 'coverage: run' "job=$leg2" verdict)" :covered)"
cp -R "$work/run1" "$work/c4"
rm "$work/c4/artifacts/go-test-events-$alone_job"/*.jsonl
read_with "$work/c4" coverage --run "$run1" --root "$module"
check "without alone's events, coverage exits 1" "$(is "$status" 1)"
check "printing alone UNKNOWN, and naming it alone on stderr" "$(is "$(pick 'coverage: run' "job=$alone" verdict)|$err" "UNKNOWN: no events: no *.jsonl beside the cpu/ of artifact go-test-events-$alone_job|go_cpu_record.py coverage: $alone ($alone_job): UNKNOWN: no events: no *.jsonl beside the cpu/ of artifact go-test-events-$alone_job")"

summary "go_cpu_record.py"
