#!/usr/bin/env bash
# Tests for `go_cpu_record.py`, the reader of the envoy-go jobs' wall times over several runs of
# one commit and of the factors that may account for their spread. `spread` refuses fewer than
# five runs, runs of more than one commit and a run whose attempt is not 1; otherwise it prints,
# for each counted job, its successful job-runs' n, min, median and max and the spread (max −
# min) ÷ median, with the job ids of its min and its max, then each job-run's id, conclusion and
# wall, `completed_at − started_at`, and exits 1 naming each job with fewer than five successful
# job-runs. `factors` prints one row per job-run with the factors the jobs API and the job's log
# give, UNKNOWN where they give none, and the decision rule's statistics and verdict for each
# branch. `rule` runs that rule over a table given as a file, and `postgres-packages` splits the
# package directories under a root into those whose tests use Postgres and the others.
#
# The reader reads GitHub only through gh. Here gh is a stand-in on PATH that answers `gh api`
# from fixture files by the URL's path and query and logs each call. A log read without
# --allow-escape-sequences gets the refusal gh itself prints for a job log (read from the real gh
# 2.98.0), and any call that is not one of the reader's three reads is refused with exit 1 and a
# message, so an unexpected call fails the case that made it. Nothing here reaches the network.
#
# The fixtures under testdata/go-cpu/ were captured from runs of ci.yaml on 2026-11-28:
#
#   run-<id>.json
#       gh api repos/cybermonklabs/legion/actions/runs/<id> | jq -c '{id, name, head_sha,
#       head_branch, event, status, conclusion, run_attempt, created_at, run_started_at,
#       updated_at}'
#     for INFRA-5794's six runs 46040397906, 46040403701, 46040410283, 46040421210, 46040426159
#     and 46040431439 (head 09bf0bc6aa0a, attempt 1), and for run 46084080784, a merge-group run
#     of main's 91e57a4886cb.
#   jobs-<id>-p<n>.json
#       gh api 'repos/cybermonklabs/legion/actions/runs/<id>/attempts/1/jobs?per_page=100&page=<n>'
#       | jq -c '{total_count, jobs: [.jobs[] | {id, run_id, run_attempt, name, status,
#       conclusion, created_at, started_at, completed_at, runner_name} + (if (.name |
#       test("^envoy-go / |^envoy-go-checks / ")) then {steps: [.steps[]? | {name, number,
#       status, conclusion, started_at, completed_at}]} else {steps: []} end)]}'
#     for n = 1 and 2 of the six runs: each run's 111 jobs, 100 on page 1 and 11 on page 2, where
#     the aggregator `envoy-go / envoy-go` is. Only the envoy-go lane's jobs keep their steps, to
#     keep the fixtures small; every other job has an empty list. Run 46084080784's jobs are not
#     kept: `spread` refuses its set before it reads any run's jobs.
#   log-<job id>.excerpt.txt
#       gh api --allow-escape-sequences repos/cybermonklabs/legion/actions/jobs/<job id>/logs
#       | tr -d '\033' | grep -E '^.{29}(go-cpu |Region: |Worker ID: |Version: |Image: |Cache hit
#       for restore-key: |Cache restored from key: |Cache not found for input keys|Cache Size: )'
#     (the grep's pattern is one line), the lines the reader parses, for job 140647302858, leg 2
#     of run 46040397906 (Region centralus, a restore-key hit of the build cache), and for job
#     140585658657, a probe job of another workflow's run (Region northcentralus), which restores
#     no cache, so its excerpt holds the Set up job lines alone. Neither job printed a `go-cpu`
#     line: both ran before the CPU record.
#   log-miss.hand-made.txt
#     written by hand, not captured: the Set up job lines and the line actions/cache prints when
#     it finds no entry for any of its keys, since neither captured job missed.
#
# The stand-in serves the first excerpt as its own job's log, the second as the log of leg 2 of
# run 46040403701 and the hand-made one as that of leg 2 of run 46040410283, so that one set of
# runs holds all three, and answers every other job's log as gh answers a log GitHub does not
# hold, `gh: Not Found (HTTP 404)`.
#
# The cases of the subcommands that read the CPU rows of a run's artifacts, `cpu` (P1-P4) and
# `coverage` (C1-C4), and F3, which parses the `go-cpu` lines of a job's log, need the artifacts
# and logs of this pull request's first CI run, and come in a later round; their place is marked
# below.
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
# A stand-in for gh: answers the reader's three reads, a run, a page of its first attempt's jobs
# and a job's log, from the files under $GH_FIXTURE, logs each call to $GH_CALLS, and refuses
# every other call with exit 1.
set -euo pipefail
printf '%s\n' "$*" >> "$GH_CALLS"
refuse() {
  echo "stand-in gh: no answer for: $*" >&2
  exit 1
}
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
check "the per-package ratios wait for the CPU rows" "$(contains "$out" '^per-package ratios: need the CPU rows (later round)$')"

# Later round: F3 (the `go-cpu runner:`, `go-cpu job:` and `go-cpu usage:` lines of a job's log,
# captured from this pull request's first CI run) goes here, and the cases of `cpu` (P1-P4) and
# `coverage` (C1-C4) after F5.

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
mkdir -p "$tree/store" "$tree/broker" "$tree/comment" "$tree/lookalike" "$tree/aliased" "$tree/plain" "$tree/testdata/ignored"
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
read_run postgres-packages --root "$tree"
check "a package whose test imports storetest uses Postgres" "$(is "$(pick packages package=store postgres why)" $'yes\tstore/store_test.go imports github.com/cybermonklabs/legion/packages/envoy/internal/dispatch/store/storetest')"
check "so does one whose test names BROKER_TEST_DATABASE_URL" "$(is "$(pick packages package=broker postgres why)" $'yes\tbroker/broker_test.go names BROKER_TEST_DATABASE_URL')"
check "and one whose test imports testpg/testdb under another name" "$(is "$(pick packages package=aliased postgres)" yes)"
check "a package that names the variable only in a comment of a file that is no test does not" "$(is "$(pick packages package=comment postgres)" no)"
check "nor one whose import only ends in the same letters, under a comment that names storetest" "$(is "$(pick packages package=lookalike postgres)" no)"
check "nor one with no test file" "$(is "$(pick packages package=plain postgres)" no)"
check "and a directory go ignores, testdata, is no package" "$(is "$(pick packages package=testdata/ignored postgres)" "")"
check "it exits 0" "$(is "$status" 0)"

# Later round: the cases of `cpu` (P1-P4) and `coverage` (C1-C4) go here.

summary "go_cpu_record.py"
