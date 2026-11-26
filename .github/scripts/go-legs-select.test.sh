#!/usr/bin/env bash
# Tests for go_legs_select.py and go-legs-select.sh: which legs and packages a change selects,
# held to an oracle the selection does not use, and each kind of change that selects every package,
# some packages or none, with a control beside each that shows the case can fail.
#
# The cases run on this repository itself, in a throwaway worktree of HEAD: a change is an edit of
# the worktree, measured from HEAD (`--base HEAD` reads the working tree), and is undone after each
# case. The oracle for the import graph is `go list -deps -test`'s Deps field, the transitive list
# go itself computes for each test binary, where the selection follows Imports one step at a time.
#
# Run from anywhere: .github/scripts/go-legs-select.test.sh
# CI runs it in ci.yaml's test job, after setup-go. Its go commands run GOPROXY=off over an empty
# module cache, as the selection's do, so it downloads nothing.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$script_dir/../.." && pwd)
# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
tree="$work/tree"
trap 'git -C "$repo" worktree remove --force "$tree" >/dev/null 2>&1; rm -rf "$work"' EXIT
git -C "$repo" worktree add -q --detach "$tree" HEAD
cd "$tree"

# select: the selection of the worktree's change from HEAD, as JSON; its reasons go to $work/said.
select_change() { python3 .github/scripts/go_legs_select.py --base HEAD 2>"$work/said"; }
# undo: the worktree back to HEAD.
undo() { git reset -q && git checkout -q -- . && git clean -qfd; }
# packages <json> <job>: the job's selected packages, one per line, or `all`.
packages() { python3 -c 'import json,sys; v=json.loads(sys.argv[1])[sys.argv[2]]["packages"]; print(v if v == "all" else "\n".join(v))' "$1" "$2"; }
# legs <json> <job>: the job's legs, space-separated.
legs() { python3 -c 'import json,sys; print(" ".join(map(str, json.loads(sys.argv[1])[sys.argv[2]]["legs"])))' "$1" "$2"; }

# importers <module> <job> <import path>: the job's packages (as go-legs.yaml names them) whose test
# binary compiles that import path, by go list's own Deps; one per line, sorted.
importers() {
  local module=$1 job=$2 target=$3
  python3 - "$module" "$job" "$target" <<'PY'
import json, os, subprocess, sys, tempfile
sys.path.insert(0, ".github/scripts")
import go_legs
module, job, target = sys.argv[1:]
legs = go_legs.module_entry(module, go_legs.ROOT / go_legs.LEGS)[job]
flags = ["-race"] if "race" in job else []
with tempfile.TemporaryDirectory() as cache:
    env = dict(os.environ, GOPROXY="off", GOMODCACHE=cache, GOFLAGS="", CGO_ENABLED="1" if flags else os.environ.get("CGO_ENABLED", "0"))
    out = subprocess.run(["go", "list", "-e", "-deps", "-test", *flags, "-f",
                          "{{.ImportPath}}\t{{.Dir}}\t{{.ForTest}}\t{{join .Deps \" \"}}",
                          *[go_legs.package_argument(p) for p in go_legs.job_packages(legs)]],
                         cwd=module, env=env, capture_output=True, text=True, check=True).stdout
deps, dirs = {}, {}
for line in out.splitlines():
    path, directory, for_test, rest = (line.split("\t") + ["", "", ""])[:4]
    deps[path] = {d.split(" [")[0] for d in rest.split()}
    if directory and not for_test and not path.endswith(".test"):
        dirs[path] = os.path.relpath(directory, os.path.abspath(module))
found = []
for package in go_legs.job_packages(legs):
    path = next(p for p, d in dirs.items() if d == package)
    binary = deps.get(path + ".test", set()) | deps.get(path, set())
    if path == target or target in binary:
        found.append(package)
print("\n".join(sorted(found)))
PY
}

echo "case: a change to nothing selects no package, and only the legs that carry steps of their own"
out=$(select_change)
check "no envoy-go package" "$(is "$(packages "$out" envoy-go)" "")"
check "daemon-go starts legs 2 and 3 alone, which run the Stage 1 proof, the forced-order tests and the stamped shape gate" "$(is "$(legs "$out" daemon-go)" "2 3")"
check "envoy-go-race starts leg 5 alone, which runs the forced-order tests" "$(is "$(legs "$out" envoy-go-race)" "5")"
check "envoy-go starts no leg" "$(is "$(legs "$out" envoy-go)" "")"

echo "case: a change to a shared package's source selects every package whose test binary compiles it"
echo "// a change" >> packages/envoy/internal/bus/async.go
out=$(select_change)
for job in envoy-go envoy-go-race; do
  want=$(importers packages/envoy "$job" github.com/sjawhar/envoy/internal/bus)
  got=$(packages "$out" "$job" | sort)
  missing=$(comm -23 <(echo "$want") <(echo "$got"))
  check "$job: every importer go list's Deps names is selected ($(wc -l <<<"$want") of them)" "$(is "$missing" "")"
  # Beyond the importers, only packages a //ci: line of theirs makes read or build the file.
  extra=$(comm -13 <(echo "$want") <(echo "$got"))
  explained=true
  for package in $extra; do
    grep -q "^  $package: .*//ci:\(reads\|builds\)" "$work/said" || explained=false
  done
  check "$job: each package beyond them reads or builds the file by a //ci: line (${extra//$'\n'/ })" "$explained"
done
check "the control: go list's Deps does name importers here" "$(is "$([ -n "$(importers packages/envoy envoy-go-race github.com/sjawhar/envoy/internal/bus)" ] && echo y)" y)"
undo

echo "case: a change to a package's own test file selects that package alone"
echo "// a change" >> packages/envoy/internal/bus/release_test.go
out=$(select_change)
check "envoy-go-race: internal/bus alone" "$(is "$(packages "$out" envoy-go-race)" "internal/bus")"
# A package whose //ci:reads line names every .go file of the module (a check of the module's own
# source) is selected too, and by that line alone.
unexplained=
for package in $(packages "$out" envoy-go); do
  grep -q "^  $package: .*//ci:reads" "$work/said" || unexplained="$unexplained $package"
done
check "envoy-go: only packages a //ci:reads line of theirs makes read the file" "$(is "$unexplained" "")"
check "daemon-go-race: none" "$(is "$(packages "$out" daemon-go-race)" "")"
undo

echo "case: a file deleted from a package directory selects its importers, which no closure of the head names"
git rm -q packages/envoy/internal/bus/async.go
out=$(select_change)
check "envoy-go-race selects internal/bus and cmd/listener" \
  "$( [ -n "$(grep -x internal/bus <<<"$(packages "$out" envoy-go-race)")" ] && [ -n "$(grep -x cmd/listener <<<"$(packages "$out" envoy-go-race)")" ] && echo true || echo false)"
undo

echo "case: a non-Go file only a //ci:reads line names selects that line's package, and nothing else"
python3 -c "import json; p='packages/contracts/fixtures/session-grant.json'; d=json.load(open(p)); open(p, 'w').write(json.dumps(d, indent=1) + '\n')"
out=$(select_change)
check "envoy-go: natsgrant alone" "$(is "$(packages "$out" envoy-go)" "natsgrant")"
check "the reason names the //ci:reads line" "$(contains "$(cat "$work/said")" 'natsgrant: .*//ci:reads packages/contracts/fixtures/session-grant.json')"
undo

echo "case: a changelog fragment and a dashboard file select no package (no legs test reads either)"
echo "x" > packages/envoy/.changes/unreleased/go-legs-select-case.yaml
git add -N packages/envoy/.changes/unreleased/go-legs-select-case.yaml
echo "// x" >> packages/dispatch/e2e/project-default-assignee.e2e.ts
out=$(select_change)
for job in envoy-go envoy-go-race daemon-go daemon-go-race; do
  check "$job: none" "$(is "$(packages "$out" "$job")" "")"
done
undo

echo "case: go-legs.yaml selects the packages whose place it changes, and a comment there selects none"
python3 - <<'PY'
p = ".github/go-legs.yaml"
t = open(p).read()
t = t.replace("# 501 s", "# 502 s", 1)
open(p, "w").write(t)
PY
out=$(select_change)
check "a comment: no daemon-go package" "$(is "$(packages "$out" daemon-go)" "")"
undo
# Move internal/workflow out of whichever daemon-go leg holds it, to the first line of another leg,
# so the case holds whatever layout the file has.
python3 - <<'PY'
import re
p = ".github/go-legs.yaml"
lines = open(p).read().split("\n")
start = lines.index("  daemon-go:")
end = next(i for i in range(start + 1, len(lines)) if re.match(r"^  \S", lines[i]))
headers = [i for i in range(start + 1, end) if re.match(r"^    \d+:$", lines[i])]
at = next(i for i in range(start + 1, end) if re.match(r"^      - internal/workflow\b", lines[i]))
own = max(h for h in headers if h < at)
other = next(h for h in headers if h != own)
del lines[at]
lines.insert(other + 1 if other < at else other, "      - internal/workflow")
open(p, "w").write("\n".join(lines))
PY
out=$(select_change)
check "internal/workflow moved to another daemon-go leg: it alone" "$(is "$(packages "$out" daemon-go)" "internal/workflow")"
undo

echo "case: a workflow selects every package of a job only where its legs' definition changed"
sed -i '0,/# One leg.s failure/s//# One failing leg/' .github/workflows/ci-job-envoy-go-race.yaml
out=$(select_change)
check "a comment in envoy-go-race's file: no package" "$(is "$(packages "$out" envoy-go-race)" "")"
undo
sed -i 's/xargs -r go test -count=1 -race -timeout 30m$/xargs -r go test -count=1 -race -timeout 31m/' .github/workflows/ci-job-envoy-go-race.yaml
out=$(select_change)
check "the legs step's text: every envoy-go-race package" "$(is "$(packages "$out" envoy-go-race)" all)"
check "and no other job's" "$(is "$(packages "$out" envoy-go)" "")"
undo
python3 - <<'PY'
import re
p = ".github/workflows/ci.yaml"
t = open(p).read()
t = re.sub(r"(\n  changes:\n(?:.*\n)*?    timeout-minutes: )(\d+)", lambda m: m.group(1) + str(int(m.group(2)) + 1), t, count=1)
open(p, "w").write(t)
PY
out=$(select_change)
check "ci.yaml's changes job: no package" "$(is "$(packages "$out" envoy-go)" "")"
undo
python3 - <<'PY'
import re
p = ".github/workflows/ci.yaml"
t = open(p).read()
t = re.sub(r"(\n  affected:\n(?:.*\n)*?    timeout-minutes: )(\d+)", lambda m: m.group(1) + str(int(m.group(2)) + 1), t, count=1)
open(p, "w").write(t)
PY
out=$(select_change)
check "ci.yaml's affected job: every package of every job" \
  "$(is "$(packages "$out" envoy-go)$(packages "$out" daemon-go)" allall)"
undo

echo "case: a go.sum change selects the importers of the module it moves; go.mod's go line every package"
python3 - <<'PY'
p = "packages/envoy/go.sum"
lines = open(p).read().splitlines()
line = next(l for l in lines if l.startswith("github.com/nats-io/nats.go "))
open(p, "a").write(line.replace(line.split()[1], "v9.9.9", 1) + "\n")
PY
out=$(select_change)
want=$(importers packages/envoy envoy-go-race github.com/nats-io/nats.go)
check "envoy-go-race: the importers of nats.go, by go list's Deps" "$(is "$(packages "$out" envoy-go-race | sort)" "$want")"
check "envoy-go: not every package" "$( [ "$(packages "$out" envoy-go)" != all ] && echo true || echo false)"
undo
sed -i 's/^\(go [0-9.]*\)$/\1\ngodebug default=go1.27/' packages/envoy/go.mod
out=$(select_change)
check "a godebug line in envoy's go.mod: every package of every job" "$(is "$(packages "$out" daemon-go)" all)"
undo

echo "case: --check holds the //ci: lines to the files they name"
status=0
out=$(python3 .github/scripts/go_legs_select.py --check 2>&1) || status=$?
check "this tree passes" "$(is "$status" 0)"
grant=packages/contracts/fixtures/session-grant.json
sed -i "s|^//ci:reads $grant|//ci:reads ${grant%session-grant.json}no-such-grant.json|" \
  "$(git grep -l "^//ci:reads $grant" -- '*.go' | head -1)"
status=0
out=$(python3 .github/scripts/go_legs_select.py --check 2>&1) || status=$?
check "a glob that names no file fails" "$(is "$status" 1)"
check "and names it" "$(contains "$out" 'no-such-grant.json names no file of the repository')"
undo

echo "case: go-legs-select.sh"
run_wrapper() { env -u GITHUB_OUTPUT "$@" .github/scripts/go-legs-select.sh 2>&1 | sed -n 's/^affected_packages: //p'; }
every=$(python3 .github/scripts/go_legs_select.py --every)
check "a dispatch of main: every package" "$(is "$(run_wrapper GITHUB_EVENT_NAME=workflow_dispatch GITHUB_REF=refs/heads/main)" "$every")"
check "a merge group while the switch is off: every package" \
  "$(is "$(run_wrapper GITHUB_EVENT_NAME=merge_group GO_LEGS_BASE="$(git rev-parse HEAD)" CI_MERGE_GROUP_SELECTS=false)" "$every")"
selected=$(run_wrapper GITHUB_EVENT_NAME=merge_group GO_LEGS_BASE="$(git rev-parse HEAD)" CI_MERGE_GROUP_SELECTS=true)
check "a merge group while the switch is on: the entry's own change (none here)" \
  "$(is "$(packages "$selected" envoy-go)" "")"
check "an event it reads no change of: every package" "$(is "$(run_wrapper GITHUB_EVENT_NAME=push)" "$every")"

summary "go_legs_select.py and go-legs-select.sh"
