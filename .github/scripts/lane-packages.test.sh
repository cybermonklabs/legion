#!/usr/bin/env bash
# Tests for .github/scripts/lane_packages.py: the table of the slow suites' own packages
# (.github/lane-packages.yaml), the filter the changes job writes from it for a merge group, and
# ci-ok's lane check (`lane_packages.py gate`), which fails a merge group where a slow suite did not
# succeed though the group changes one of its own packages
# (.github/ci-notes/ci.md#ci-ok-fails-a-merge-group-that-skipped-a-slow-suite-whose-own-package-it-touches).
#
# The two readings share the table and nothing of their matching: the filter is globs that
# dorny/paths-filter evaluates, and the gate maps a path to its package in Python. The first cases
# hold them to the same answer on every file of this tree and on probe paths of each shape the
# table has, through workflow_paths.PathsFilter, the repository's model of dorny/paths-filter; a
# filter with one suite's entry dropped shows that case fails by name. The next hold the table to
# what it must agree with: ci.yaml's outputs, the go.mod files inside a legs module, and every go
# test package a slow suite's own workflow names. The gate cases run the real script in a small git
# repository of each shape it reads: a merge group's head against its base with entries ahead of
# it, a base it must fetch, a group it reads no file of, NEEDS whose changes outputs say the opposite
# of the job results, which the gate must not read, and the pull request and dispatched runs it
# does not judge.
#
# Run from anywhere: .github/scripts/lane-packages.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$script_dir/../.." && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# No __pycache__ in the fixture repository, where `git add -A` would take it for a change.
export PYTHONDONTWRITEBYTECODE=1

# py <program> <argument>...: a Python program with this repository's scripts importable and the
# repository as its working directory.
py() { (cd "$repo" && PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 -c "$@"); }

# disagreements <filters file or ''> <table root>: every path where the gate's owners and the
# written filter's verdicts differ, as `<path>: gate <jobs> filter <jobs>`; the filter is the one
# the script writes from the table under <table root> unless a file is given.
read -r -d '' agree <<'PY' || true
import subprocess, sys, yaml
from pathlib import Path
import lane_packages as lp
from workflow_paths import PathsFilter
given, root = sys.argv[1], Path(sys.argv[2])
lanes = lp.load(root)
written = yaml.safe_load(open(given)) if given else lp.filters(lanes)
by_output = {lane.output: job for job, lane in lanes.items()}
matchers = {by_output[o]: PathsFilter(p, "some-with-excludes") for o, p in written.items() if o in by_output}
files = [f for f in subprocess.run(["git", "ls-files"], capture_output=True, text=True, check=True).stdout.split("\n") if f]
probes = ["packages/envoy/internal/no-such-package/a.go", "packages/envoy/internal/no-such-package/testdata/b.go",
          "packages/envoy/internal/no-such-package/c.txt", "packages/envoy/cmd/listener/README.md",
          "packages/envoy/cmd/listener/testdata/fixture.md", "packages/envoy/cmd/listener/no-such-sub/d.go",
          "packages/envoy/infra/no-such-package/e.go", "packages/envoy/f.go", "packages/daemon/internal/no-such/g.go",
          "packages/pi-legion/.changes/unreleased/h.yaml", "packages/dispatch/e2e/specs/cross-build.multiprocess.e2e.ts",
          ".github/scripts/ci-ok.no-such-subject", ".github/scripts/ci_ok.sh", "docs/i.md"]
if not files:
    print("UNKNOWN: git ls-files listed nothing")
for path in files + probes:
    gate = set(lp.owners(lanes, path))
    filt = {job for job, m in matchers.items() if m.covers(path)}
    if gate != filt:
        print(f"{path}: gate {' '.join(sorted(gate - filt)) or '-'} filter {' '.join(sorted(filt - gate)) or '-'}")
print(f"read {len(files)} files and {len(probes)} probes", file=sys.stderr)
PY
disagreements() { py "$agree" "$1" "$2" 2>"$work/agree.err"; }

echo "case: the gate and the written filter give every file of this tree and every probe the same owners"
got=$(disagreements '' "$repo")
check "they differ nowhere" "$(is "$got" "")"
check "and they read the tree, not nothing" "$(contains "$(cat "$work/agree.err")" '^read [1-9][0-9]* files and 14 probes$')"

echo "case: a written filter missing one suite's entry differs from the gate, by that suite"
"$script_dir/lane_packages.py" filters "$work/filters.yaml"
py 'import sys, yaml; d = yaml.safe_load(open(sys.argv[1])); del d["envoy_go_race"]; yaml.safe_dump(d, open(sys.argv[1], "w"))' \
  "$work/filters.yaml"
got=$(disagreements "$work/filters.yaml" "$repo")
check "it names cmd/listener's files for envoy-go-race" \
  "$(contains "$got" '^packages/envoy/cmd/listener/[^ ]*\.go: gate envoy-go-race filter -$')"

echo "case: the table's jobs are the outputs a lane label turns on, each under ci.yaml's own output"
got=$(py '
import json, subprocess, yaml
import lane_packages as lp
ci = yaml.safe_load(open(".github/workflows/ci.yaml"))
selected_by = json.loads(ci["env"]["CI_OK_SELECTED_BY"])
held = set(subprocess.run([".github/scripts/ci_router_outputs.py", ".github/workflows/ci.yaml", "--label-readers"],
                          capture_output=True, text=True, check=True).stdout.split())
lanes = lp.load()
for job, lane in sorted(lanes.items()):
    if selected_by.get(job) != lane.output:
        print(f"{job}: table output {lane.output}, CI_OK_SELECTED_BY {selected_by.get(job)}")
for output in sorted(held - {lane.output for lane in lanes.values()}):
    print(f"{output}: read by a lane label and in no table entry")
for output in sorted({lane.output for lane in lanes.values()} - held):
    print(f"{output}: in the table and read by no lane label")
print(f"{len(lanes)} entries")
')
check "fifteen entries, each the output CI_OK_SELECTED_BY maps it to" "$(is "$got" "15 entries")"

echo "case: the nested modules are every go.mod inside a legs module"
got=$(py '
import subprocess, yaml
legs = yaml.safe_load(open(".github/go-legs.yaml"))
nested = set(yaml.safe_load(open(".github/lane-packages.yaml"))["nested"])
mods = {f.rsplit("/", 1)[0] for f in subprocess.run(["git", "ls-files", "*go.mod"], capture_output=True, text=True,
        check=True).stdout.split()}
inside = {m for m in mods for module in legs
          if m.startswith(module + "/") and "/testdata/" not in m + "/"}
print(" ".join(sorted(inside ^ nested)) or "same")
print(len(inside))
')
check "the same set, and not an empty one" "$(is "$got" $'same\n1')"

# go_packages_named <table root>: each go test package a slow suite's workflow names that the
# table does not give that suite, as `<job>: <package>`.
read -r -d '' named <<'PY' || true
import os, re, sys, yaml
from pathlib import Path
import lane_packages as lp
root = Path(sys.argv[1])
lanes = lp.load(root)
ci = yaml.safe_load(open(".github/workflows/ci.yaml"))
token = re.compile(r"(?<![\w./$-])(\./[\w./-]+?)/?(?=[\s\"';)]|$)")
count, missing = 0, set()
for job in sorted(lanes):
    uses = str(ci["jobs"][job].get("uses", ""))
    if not uses.startswith("./"):
        continue
    workflow = yaml.safe_load(open(uses[2:]))
    for wjob in (workflow.get("jobs") or {}).values():
        workdir = ((wjob.get("defaults") or {}).get("run") or {}).get("working-directory", "")
        for step in wjob.get("steps") or []:
            run = str(step.get("run", ""))
            if not re.search(r"\bgo test\b|go-tagged-tests\.sh|forced-order\.sh|database-skips\.sh", run):
                continue
            base = step.get("working-directory", workdir) or ""
            for line in run.splitlines():
                if not re.search(r"\bgo test\b|go-tagged-tests\.sh|forced-order\.sh|database-skips\.sh|\bfor pkg in\b", line):
                    continue
                for path in token.findall(line):
                    directory = os.path.normpath(os.path.join(base, path))
                    if not os.path.isdir(directory) or not any(f.endswith(".go") for f in os.listdir(directory)):
                        continue
                    count += 1
                    probe = next(f"{directory}/{f}" for f in sorted(os.listdir(directory)) if f.endswith(".go"))
                    if job not in lp.owners(lanes, probe):
                        missing.add(f"{job}: {directory}")
print("\n".join(sorted(missing)))
print(f"read {count} packages", file=sys.stderr)
PY

echo "case: every go test package a slow suite's workflow names is that suite's own"
got=$(py "$named" "$repo" 2>"$work/named.err")
check "none is missing" "$(is "$got" "")"
check "and it read some, not none" "$(contains "$(cat "$work/named.err")" '^read [1-9][0-9]* packages$')"

echo "case: a table without one of them names it"
mkdir -p "$work/table/.github/workflows"
cp "$repo/.github/go-legs.yaml" "$repo/.github/lane-packages.yaml" "$work/table/.github/"
cp "$repo/.github/workflows/ci-job-test.yaml" "$repo/.github/workflows/ci-job-envoy-go-checks.yaml" "$work/table/.github/workflows/"
py 'import sys
p = sys.argv[1]
text = open(p).read()
old = "    packages: [packages/envoy/internal/testnats/hooktest]\n"
assert text.count(old) == 1, "the table does not hold the line this case removes once"
open(p, "w").write(text.replace(old, ""))' "$work/table/.github/lane-packages.yaml"
got=$(py "$named" "$work/table" 2>/dev/null)
check "it names envoy-go-race's hooktest package" "$(is "$got" "envoy-go-race: packages/envoy/internal/testnats/hooktest")"

# The gate's cases: a repository holding the script, what it imports and what its table reads,
# where each case makes the commits of one shape and runs the gate in it.
fixture=$work/fixture
mkdir -p "$fixture/.github/scripts" "$fixture/.github/workflows"
cp "$script_dir/lane_packages.py" "$script_dir/workflow_paths.py" "$fixture/.github/scripts/"
cp "$repo/.github/go-legs.yaml" "$repo/.github/lane-packages.yaml" "$fixture/.github/"
cp "$repo/.github/workflows/ci-job-test.yaml" "$repo/.github/workflows/ci-job-envoy-go-checks.yaml" "$fixture/.github/workflows/"
g() { git -C "$fixture" -c user.name=t -c user.email=t@example.com -c commit.gpgsign=false "$@"; }
g init -q -b main
# The shallow clone below fetches the base by its id, as the ci-ok job's checkout does from GitHub.
g config uploadpack.allowAnySHA1InWant true
g add -A
g commit -q -m base
base=$(g rev-parse HEAD)
# group <path>...: a merge group on the base, one entry's commit for each path, as the queue builds
# a group on main with the entries ahead of it. HEAD is the group's head.
group() {
  g checkout -q -B main "$base"
  local path
  for path in "$@"; do
    mkdir -p "$fixture/$(dirname "$path")"
    echo x > "$fixture/$path"
    g add -A
    g commit -q -m "entry $path"
  done
}
# needs <job>=<result>...: NEEDS with changes' outputs saying every suite is selected, which the
# gate must not read, and each job given at its result.
needs() {
  py 'import json, sys
doc = {"changes": {"result": "success", "outputs": {"envoy_go_race": "true", "envoy_go": "true", "daemon_go_tests": "true"}}}
for pair in sys.argv[1:]:
    job, _, result = pair.partition("=")
    doc[job] = {"result": result, "outputs": {}}
print(json.dumps(doc))' "$@"
}
# gate <event> <needs> [<merge group base>]: the gate's output, its status in $status.
gate() {
  set +e
  out=$(cd "$fixture" && EVENT=$1 NEEDS=$2 MERGE_GROUP_BASE=${3:-} python3 .github/scripts/lane_packages.py gate 2>&1)
  status=$?
  set -e
}

echo "case: a merge group that changes a race package, the race suite skipped"
group packages/envoy/cmd/listener/new_test.go
gate merge_group "$(needs envoy-go-race=skipped envoy-go=success)" "$base"
check "fails" "$(is "$status" 1)"
check "names envoy-go-race, its result and the file" \
  "$(contains "$out" '^::error::envoy-go-race is skipped in this merge group, and the group changes its own packages (packages/envoy/cmd/listener/new_test.go)')"
check "names no error for envoy-go, which ran" "$(is "$(contains "$out" '::error::envoy-go is')" false)"
check "does not take changes' word that envoy_go_race was selected: it names the output to read" \
  "$(contains "$out" 'read needs.changes.outputs.envoy_go_race')"

echo "case: the same group, the race suite ran"
gate merge_group "$(needs envoy-go-race=success envoy-go=success)" "$base"
check "passes" "$(is "$status" 0)"
check "says it read one file and two suites own it" "$(contains "$out" '^lane check: 1 files change between [0-9a-f]\{12\} and [0-9a-f]\{12\}; 2 of 15 slow suites own one of them$')"
check "names the race suite as run" "$(contains "$out" '^envoy-go-race: success, and this change touches its own packages')"

echo "case: the same group, the race suite absent from NEEDS"
gate merge_group "$(needs envoy-go=success)" "$base"
check "fails, naming it absent" "$(contains "$out" '^::error::envoy-go-race is absent in this merge group')"

echo "case: the same group, the router suppressing the race suite its paths selected"
# The case the gate exists for: changes' output says false and the suite skipped, which ci-ok.sh
# accepts as a job changes did not select. The gate does not read that output, so it refuses.
suppressed=$(py 'import json, sys
doc = json.loads(sys.argv[1])
doc["changes"]["outputs"]["envoy_go_race"] = "false"
print(json.dumps(doc))' "$(needs envoy-go-race=skipped envoy-go=success)")
gate merge_group "$suppressed" "$base"
check "fails" "$(is "$status" 1)"
check "names envoy-go-race skipped" "$(contains "$out" '^::error::envoy-go-race is skipped in this merge group')"

echo "case: a group whose one labelled suite ran is not refused"
# The quiet twin: a pull request that carried lane:envoy_go_race, whose record cites that job,
# reaches its group with the suite run and passed, and nothing of it is refused.
gate merge_group "$(needs envoy-go-race=success envoy-go=success)" "$base"
check "passes" "$(is "$status" 0)"
check "and writes no error" "$(is "$(contains "$out" '::error::')" false)"

echo "case: a group no slow suite owns a file of, every suite skipped"
group docs/notes/lane-free.md
gate merge_group "$(needs envoy-go-race=skipped envoy-go=skipped daemon-go=skipped test=skipped)" "$base"
check "passes" "$(is "$status" 0)"
check "says none owns one" "$(contains "$out" '^lane check: 1 files change between [0-9a-f]\{12\} and [0-9a-f]\{12\}; 0 of 15 slow suites own one of them$')"

echo "case: a group read against its base, the entries ahead of the last included"
group packages/daemon/internal/daemon/entry_one.go docs-entry-two.md
gate merge_group "$(needs daemon-go=skipped)" "$base"
check "fails on the entry ahead's daemon change" \
  "$(contains "$out" '^::error::daemon-go is skipped in this merge group, and the group changes its own packages (packages/daemon/internal/daemon/entry_one.go)')"
gate merge_group "$(needs daemon-go=success)" "$base"
check "passes once daemon-go ran" "$(is "$status" 0)"
gate merge_group "$(needs daemon-go=success)" ""
check "refuses a merge group run with no base" "$(contains "$out" "^::error::the lane check could not run: MERGE_GROUP_BASE is ''")"

echo "case: a group whose base is not in the checkout is fetched"
shallow=$work/shallow
git clone -q --depth=1 "file://$fixture" "$shallow"
cp "$script_dir/lane_packages.py" "$shallow/.github/scripts/lane_packages.py"
set +e
out=$(cd "$shallow" && EVENT=merge_group NEEDS="$(needs daemon-go=skipped)" MERGE_GROUP_BASE=$base \
  python3 .github/scripts/lane_packages.py gate 2>&1)
status=$?
set -e
check "it still names daemon-go" "$(contains "$out" '^::error::daemon-go is skipped in this merge group')"

echo "case: a group git reads no file of is UNKNOWN, never a pass"
g checkout -q -B main "$base"
g commit -q --allow-empty -m "empty entry"
gate merge_group "$(needs)" "$base"
check "fails" "$(is "$status" 1)"
check "says which suites the group needs is UNKNOWN" "$(contains "$out" 'is UNKNOWN')"

echo "case: a pull request and a dispatched run are not judged, and say so"
group packages/envoy/cmd/listener/new_test.go
for event in pull_request workflow_dispatch; do
  gate "$event" "$(needs envoy-go-race=skipped)"
  check "$event passes with the race suite skipped" "$(is "$status" 0)"
  check "$event says the merge group judges the slow suites" \
    "$(contains "$out" "^lane check: not judged on a $event run: a pull request holds the slow suites back, and the merge group runs and judges them\$")"
done

summary "the slow suites' own packages read the same through the filter and the gate, the table agrees with ci.yaml, the go.mod files and the suites' workflows, and ci-ok's lane check fails a merge group that skipped a suite whose own package it changes"
