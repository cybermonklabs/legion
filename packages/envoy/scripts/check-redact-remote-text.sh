#!/usr/bin/env bash
# packages/envoy/scripts/check-redact-remote-text.sh
#
# The check that a remote's words, and a configured URL, print with no credential: the redaction
# of remote text (packages/envoy/AGENTS.md, "redact.Error(err, base)"). Run it from anywhere in
# the repository; it prints one line per inventory row and exits 0 only when every step and every
# row passed, 1 when one failed, 2 when it refused to run or to read a judge of its rows (below),
# and 3 when nothing failed but a run went past its bound (UNKNOWN: neither a pass nor a failure;
# run it again).
#
# It reads its inventory, packages/envoy/internal/redact/testdata/redact-check.json: each row a
# requirement, a Go package (relative to packages/envoy) or a Bun test file (relative to the
# repository root), a test, and for a print-site row the production code that test calls. It
#
#   - refuses an inventory it cannot parse, one with no Go row or no Bun row, one whose row lacks
#     a field or repeats an id, and one whose gate.row_timeout (the time one row's test may take,
#     its own budget) is not a whole count of seconds;
#   - runs `go test -json -count=1 -run '^(<names>)$'` once per package the inventory names, where
#     <names> are the top-level names of that package's inventory tests, each escaped and
#     anchored, under a timeout this run derives for that package from its own start-up run
#     (below, at the loop); the whole packages are CI's. It keeps each exit status, and fails on
#     a non-zero status, on a package that did not end in a pass, and on an inventory test or
#     subtest that did not run, did not pass, or was skipped; for a package that failed, it prints
#     each failed test with its first and last reported lines from go test's events (a test's
#     t.Log lines read as its t.Error lines do, and the last is the one nearest its failure), the
#     compiler's errors
#     of one that does not build, and the tail of the output of one that failed with no failing
#     test. A package that ran past its timeout is UNKNOWN, not failed: it prints the timeout's
#     panic line, the tests it was running and the goroutine running each;
#   - runs `bun test` on each Bun file in its package with Bun's JUnit reporter, keeps Bun's exit
#     status, and fails on a non-zero status or on an inventory test the report holds no passing
#     testcase for. It reads the report rather than Bun's console, which prints no line for a
#     passing test where AGENT or CLAUDECODE is set, as it is in an agent's shell;
#   - runs `bun run typecheck` in packages/contracts, packages/cipher-client and
#     packages/envoy-client, and fails on a non-zero status;
#   - reads the verdicts of its three judges, each a bun script - the Go rows', the Bun rows' and
#     the print test's - only from a judge that exited 0 and printed one verdict line for each
#     package, row or print test it judges, and refuses (exit 2) otherwise: a judge that failed to
#     start, or stopped at a line of go.json that a killed go command cut short, prints nothing,
#     and a judge's empty or short output holds no FAIL line to count.
#
# Around those steps it runs the mutation gate, packages/envoy/cmd/redact-gate, whose stages run
# in this order and stop at the first that fails, naming it: (1) the inventory; (2) the censuses -
# the call census (every call of the rule the change adds is a row's mutation), the boundary
# census (every call of golang.org/x/oauth2, go-oidc, google.golang.org/api/impersonate or
# cloud.google.com/go/cloudsqlconn is across redact.OAuth2, a text-only one under a CallScope, and
# every client or token source handed a library is one the boundary made), the field census
# (every field the boundary sets is a closed value or the rule's reading), and the answer census
# (outside internal/redact, every Go client reads an HTTP answer's body only through
# redact.ReadAnswer or redact.StreamAnswer, whose redact.Answer holds the record of the request
# it sent, decodes it only through Answer.Decode, and reads no answer's status line, which it
# names through redact.StatusLine), each Go census reading every build CI makes (each GOOS/GOARCH
# and tag list), and the plan's BOUNDARY api lines naming each build target; (3) each mutation's
# binding, before any test runs; then the steps above and (4) each row's test unmutated; then
# (5) each mutation alone, whose test must fail at its designated assertion with a marker in the
# output it captured there. `--controls` runs the gate's negative controls instead, each from a
# passing baseline in its own scratch worktree.
#
# A package's own needs stay its own: a row's test that needs a database reads the variable its
# suite reads - DISPATCH_TEST_DATABASE_URL in the envoy module (internal/dispatch/api among them),
# BROKER_TEST_DATABASE_URL for the secrets broker's rigs, and LEGION_TEST_PG_DSN in the daemon
# module, daemon/internal/daemon among them - and a row whose test skips for want of one fails
# ("test skip"), so set all three, to one database. No line this script prints is a real
# credential: the tests' credentials are made up (not-a-secret-...), and a failure line it prints
# may quote one.
#
# GO names the go binary every step and the gate run (default: the go on PATH). The script puts
# its directory first on PATH and sets GOROOT to the root that go names for itself (a GOROOT the
# shell exports for another version would make every build fail), so each go a step or the gate
# starts is that one, and it refuses a go that is a version manager's shim, which can put another
# PATH back in front.
set -uo pipefail

go_bin=${GO:-$(command -v go)} || { echo "check: no go on PATH and no GO"; exit 2; }
case "$(readlink -f "$go_bin")" in
*/shims/* | */bin/mise) echo "check: GO=$go_bin is a shim; name the toolchain's own go"; exit 2 ;;
esac
GOROOT=$(cd "$(git rev-parse --show-toplevel)" && env -u GOROOT "$go_bin" env GOROOT) || { echo "check: $go_bin env GOROOT failed"; exit 2; }
export GOROOT GO="$GOROOT/bin/go" PATH="$GOROOT/bin:$PATH"
echo "check: go is $("$GO" version) at $GO"

controls=0
case "${1:-}" in
--controls) controls=1 ;;
"") ;;
*)
  echo "usage: $0 [--controls]"
  exit 2
  ;;
esac

root=$(git rev-parse --show-toplevel) || exit 2
inventory="$root/packages/envoy/internal/redact/testdata/redact-check.json"
work=$(mktemp -d "${TMPDIR:-/tmp}/redact-check.XXXXXX") || exit 2
trap 'rm -rf "$work"' EXIT
module=github.com/sjawhar/envoy

# rows.tsv: kind, id, requirement, package or file, test; row-timeout: gate.row_timeout's seconds.
# The JavaScript is in single quotes on purpose: its ${...} are its own.
# shellcheck disable=SC2016
if ! bun -e '
const [path, budget] = process.argv.slice(1);
let inventory;
try {
  inventory = JSON.parse(require("node:fs").readFileSync(path, "utf8"));
} catch (error) {
  console.error(`the inventory ${path} cannot be parsed: ${error.message}`);
  process.exit(1);
}
const go = Array.isArray(inventory?.go) ? inventory.go : [];
const bun = Array.isArray(inventory?.bun) ? inventory.bun : [];
if (go.length === 0 || bun.length === 0) {
  console.error(`the inventory ${path} names no Go row or no Bun row`);
  process.exit(1);
}
const rowTimeout = inventory?.gate?.row_timeout;
if (typeof rowTimeout !== "string" || !/^[1-9][0-9]*s$/.test(rowTimeout)) {
  console.error(`the inventory ${path} names no gate.row_timeout in whole seconds (such as "48s"), the time the test of one row may take`);
  process.exit(1);
}
require("node:fs").writeFileSync(budget, rowTimeout.slice(0, -1));
const seen = new Set();
for (const [kind, rows, place] of [["go", go, "package"], ["bun", bun, "file"]]) {
  for (const row of rows) {
    for (const field of ["id", "requirement", place, "test"]) {
      if (typeof row?.[field] !== "string" || row[field] === "" || /[\t\n]/.test(row[field])) {
        console.error(`an inventory row (${JSON.stringify(row?.id ?? null)}) has no ${field}`);
        process.exit(1);
      }
    }
    if (seen.has(row.id)) {
      console.error(`the inventory repeats the id ${row.id}`);
      process.exit(1);
    }
    seen.add(row.id);
    console.log([kind, row.id, row.requirement, row[place], row.test].join("\t"));
  }
}
' "$inventory" "$work/row-timeout" >"$work/rows.tsv" 2>"$work/inventory.err"; then
  echo "check-redact-remote-text: refused: $(cat "$work/inventory.err")"
  exit 2
fi

failed=0
step() { # step <name> <status>
  if [ "$2" -eq 0 ]; then echo "PASS step $1"; else echo "FAIL step $1 (exit $2)"; failed=$((failed + 1)); fi
}

# The mutation gate (packages/envoy/cmd/redact-gate) pins the change's base and head and reads
# that head alone. Stages 1-3 (the inventory, the call census, each mutation's binding) run before
# any test; a failure stops the check there and names its stage.
gate="$work/redact-gate"
if ! (cd "$root/packages/envoy" && go build -o "$gate" ./cmd/redact-gate) >"$work/gate-build.txt" 2>&1; then
  echo "check c-redact-remote-text: the mutation gate does not build: $(tail -n 3 "$work/gate-build.txt")"
  exit 2
fi
if [ "$controls" = 1 ]; then
  "$gate" -controls
  exit $?
fi
"$gate" -stage plan >"$work/plan.txt" 2>&1
status=$?
cat "$work/plan.txt"
if [ "$status" -ne 0 ]; then
  echo "check c-redact-remote-text: failed before any test ran"
  exit 1
fi

# The printed bound names each build target the boundary census read (cmd/redact-gate's
# ciTargets) on its BOUNDARY api lines; a plan that printed no such line read nothing, and fails.
boundary_targets=0
if grep -q '^BOUNDARY api ' "$work/plan.txt"; then
  for target in linux/amd64 darwin/arm64 linux/arm64 darwin/amd64; do
    if ! grep '^BOUNDARY api ' "$work/plan.txt" | grep -qF "$target"; then
      echo "check: no BOUNDARY api line of the plan names the build target $target"
      boundary_targets=1
    fi
  done
else
  echo "check: the plan printed no BOUNDARY api line, so it names no build target"
  boundary_targets=1
fi
step "the boundary census names each build target it read" "$boundary_targets"

# Each package's inventory tests, and no other test of it: a row's test is `Top` or `Top/sub`, and
# `-run '^(Top|...)$'` runs each Top whole, every subtest with it. Each package's timeout is derived
# in this run, not set ahead, because the box's load moves from one run to the next: on 2026-11-22
# the one row of daemon/internal/workflow took 46s at 14:06Z and 1.5s at 15:31Z.
#   - The start-up run: `go test -json -count=1 -run '^$' <package>`, timed. It builds and links
#     the package's test binary and runs it with no test (its TestMain's start and end), under the
#     box's load of that moment. Its own bound is go test's default, ten minutes.
#   - The rows' budget: row_timeout (the inventory's gate.row_timeout, the time the test of one row
#     may take, which cmd/redact-gate gives each row it runs alone) once for each top-level test
#     the run selects.
#   - The bound: the rows' budget plus four times the start-up run's time, in whole seconds, so the
#     rows' budget is its floor. go test's -timeout counts from the first test, but the go command
#     kills the binary one minute past it counted from the binary's start, so the bound holds the
#     binary's start and end as well as its tests: twice the start-up run holds that start and end
#     (the start-up run made them once, with no test; a run's end also drops what its rows made,
#     each database they cloned among it), and twice that again holds the load moving between the
#     two runs.
# A start-up run that does not exit 0 is its package's outcome, and the package's rows do not run.
# Each package's stderr is kept apart (go-<n>.err), and its verdict below prints it.
row_timeout=$(cat "$work/row-timeout")
mapfile -t packages < <(awk -F'\t' '$1 == "go" { print $4 }' "$work/rows.tsv" | sort -u)
: >"$work/go.json"
: >"$work/go-steps.tsv"
for i in "${!packages[@]}"; do
  package=${packages[$i]}
  mapfile -t tops < <(awk -F'\t' -v p="$package" '$1 == "go" && $4 == p { sub(/\/.*/, "", $5); print $5 }' "$work/rows.tsv" | sort -u)
  names=$(printf '%s\n' "${tops[@]}" | sed 's/[][\.*^$+?(){}|\\]/\\&/g' | paste -sd'|')
  started=$(date +%s%N)
  (cd "$root/packages/envoy" && go test -json -count=1 -run '^$' "$package") >"$work/startup.json" 2>"$work/go-$i.err"
  status=$?
  startup=$((($(date +%s%N) - started + 999999999) / 1000000000))
  if [ "$status" -ne 0 ]; then
    cat "$work/startup.json" >>"$work/go.json"
    echo "go test $package: its start-up run (-run '^\$') exited $status after ${startup}s, so its rows did not run"
  else
    bound=$((${#tops[@]} * row_timeout + 4 * startup))
    started=$(date +%s)
    (cd "$root/packages/envoy" && go test -json -count=1 -timeout "${bound}s" -run "^($names)\$" "$package") >>"$work/go.json" 2>>"$work/go-$i.err"
    status=$?
    echo "go test $package: $(($(date +%s) - started))s under a bound of ${bound}s (${#tops[@]} top-level tests at ${row_timeout}s, and four times its start-up run's ${startup}s)"
  fi
  printf '%s\t%s\t%s\n' "$package" "$status" "$work/go-$i.err" >>"$work/go-steps.tsv"
done

mapfile -t files < <(awk -F'\t' '$1 == "bun" { print $4 }' "$work/rows.tsv" | sort -u)
for i in "${!files[@]}"; do
  file=${files[$i]}
  package=$(printf '%s\n' "$file" | cut -d/ -f1-2)
  (cd "$root/$package" && bun test --reporter=junit --reporter-outfile="$work/bun-$i.xml" "${file#"$package"/}") >"$work/bun-$i.txt" 2>&1
  step "bun test $file" $?
done

for package in contracts cipher-client envoy-client; do
  (cd "$root/packages/$package" && bun run typecheck) >"$work/typecheck-$package.txt" 2>&1
  step "bun run typecheck in packages/$package" $?
done

# judged <judge> <its exit status> <its output> [<owed> <what> <awk pattern>]...: prints the
# judge's output, then refuses (exit 2), naming the judge and both counts, unless the judge exited
# 0 and, for each triple, exactly <owed> lines of its output match the pattern. The FAIL and
# UNKNOWN lines counted after each judge are its verdicts only when it ran to its end and printed
# one for each package, row or print test it judges: an empty or short output counts no failure.
judged() {
  local judge=$1 code=$2 output=$3 owed what pattern got
  shift 3
  cat "$output"
  if [ "$code" -ne 0 ]; then
    echo "check c-redact-remote-text: refused: the $judge failed (exit $code), so the rows it judges have no verdict"
    exit 2
  fi
  while [ "$#" -gt 0 ]; do
    owed=$1 what=$2 pattern=$3
    shift 3
    got=$(awk "$pattern { n++ } END { print n + 0 }" "$output")
    if [ "$got" != "$owed" ]; then
      echo "check c-redact-remote-text: refused: the $judge printed $got $what where it owes $owed"
      exit 2
    fi
  done
}
go_rows=$(awk -F'\t' '$1 == "go" { n++ } END { print n + 0 }' "$work/rows.tsv")
bun_rows=$(awk -F'\t' '$1 == "bun" { n++ } END { print n + 0 }' "$work/rows.tsv")

# The verdict of each package and each Go row, from go test's own events and each package's exit
# status. A package passed when it exited 0 and ended in a pass. One that did not is UNKNOWN when it
# ran past its bound and no test of it failed (go test -json carries the test binary's stderr, so
# the timeout's panic, or the go command's kill a minute later, is among its output events), and
# failed otherwise. For each that did not pass: the compiler's errors of one that does not build;
# each failed test with its first and last reported lines; the panic line, the tests it was running
# and the goroutine running each, of one that ran past its bound; the tail of the output of one
# that failed with no failing test; and the tail of its stderr. A row whose test passed or did not
# run in an UNKNOWN package is UNKNOWN too.
# shellcheck disable=SC2016
bun -e '
const [events, rows, module, steps] = process.argv.slice(1);
const fs = require("node:fs");
const verdict = new Map();
const said = new Map();
const last = new Map();
const output = new Map(); // each package: what its test binary printed, in order
const built = new Map(); // each build target (ImportPath): the compiler output
const brokenBuild = new Map(); // each package whose build failed: the target that failed
const failing = (line) => /_test\.go:\d+:|^panic:|\bpanic: |^FAIL\b|build failed|timed out|cannot|undefined:/.test(line.trim());
for (const line of fs.readFileSync(events, "utf8").split("\n")) {
  if (!line.startsWith("{")) continue;
  const event = JSON.parse(line);
  if (event.Action === "build-output") built.set(event.ImportPath, (built.get(event.ImportPath) ?? "") + (event.Output ?? ""));
  if (event.Action === "build-output" || event.Action === "build-fail") continue;
  const key = `${event.Package}\t${event.Test ?? ""}`;
  if (event.Action === "output") {
    output.set(event.Package, (output.get(event.Package) ?? "") + (event.Output ?? ""));
    if (failing(event.Output ?? "")) {
      if (!said.has(key)) said.set(key, event.Output.trim());
      last.set(key, event.Output.trim());
    }
  }
  if (event.Action === "fail" && event.Test === undefined && event.FailedBuild) brokenBuild.set(event.Package, event.FailedBuild);
  if (["pass", "fail", "skip"].includes(event.Action)) verdict.set(key, event.Action);
}
// A package of this module is named relative to packages/envoy; one of another module of the
// workspace, the daemon, by its import path.
const nameOf = (pkg) => (pkg.startsWith("./") ? `${module}/${pkg.replace(/^\.\//, "")}`.replace(/\/$/, "") : pkg);
const lines = (key) => {
  const first = said.get(key) ?? "(no failure line)";
  const final = last.get(key);
  return final === undefined || final === first ? first : `${first}\n      last: ${final}`;
};
const tail = (text, n) => {
  const all = text.replace(/\n+$/, "").split("\n");
  if (text.trim() === "") return ["(nothing)"];
  return all.length > n ? [`... (${all.length - n} lines before these)`, ...all.slice(-n)] : all;
};
// The binary ran past its -timeout (its own panic), or past the minute after it that the go
// command gives it (the go command kills it with SIGQUIT, and it prints every goroutine).
const ranPast = /^panic: test timed out after |^\*\*\* Test killed with quit: ran too long/m;
// stall: the timeout panic line with the tests it was running, the go command kill line, and the
// goroutines of the dump that run a test - each whose frames name a test function other than
// TestMain - or, where none does, those in TestMain (a start or an end that ran past the bound).
// cmd/redact-gate prints the same for a run of one row (rows.go, stall).
const stall = (text) => {
  const all = text.split("\n");
  const shown = [];
  const panic = all.findIndex((line) => line.startsWith("panic: test timed out after "));
  if (panic >= 0) {
    shown.push(all[panic]);
    for (let i = panic + 1; i < all.length && all[i].trim() !== ""; i++) shown.push(all[i]);
  }
  const killed = all.find((line) => line.startsWith("*** Test killed with quit: ran too long"));
  if (killed !== undefined) shown.push(killed);
  const blocks = [];
  let block = null;
  for (const line of all) {
    if (/^goroutine \d+ /.test(line)) {
      if (block !== null) blocks.push(block);
      block = [line];
    } else if (block !== null && line.trim() === "") {
      blocks.push(block);
      block = null;
    } else if (block !== null) block.push(line);
  }
  if (block !== null) blocks.push(block);
  const tests = (b) => b.map((line) => /\.(Test(?:[A-Z0-9_]\w*)?)[.(]/.exec(line)?.[1]).filter((name) => name !== undefined);
  let running = blocks.filter((b) => tests(b).some((name) => name !== "TestMain"));
  if (running.length === 0) running = blocks.filter((b) => tests(b).includes("TestMain"));
  for (const b of running.slice(0, 4)) shown.push("", ...b.slice(0, 24), ...(b.length > 24 ? [`... (${b.length - 24} more lines)`] : []));
  if (running.length > 4) shown.push(`... (${running.length - 4} more goroutines running a test)`);
  if (running.length === 0) shown.push("(no goroutine of the dump runs a test)");
  return shown;
};
const outcome = new Map();
const why = [];
for (const step of fs.readFileSync(steps, "utf8").split("\n")) {
  if (step === "") continue;
  const [pkg, code, errFile] = step.split("\t");
  const name = nameOf(pkg);
  const status = Number(code);
  const whole = verdict.get(`${name}\t`) ?? "did not end";
  if (status === 0 && whole === "pass") {
    outcome.set(name, "pass");
    console.log(`PASS step go test ${pkg}`);
    continue;
  }
  const text = output.get(name) ?? "";
  const stderr = fs.existsSync(errFile) ? fs.readFileSync(errFile, "utf8") : "";
  const failed = [...verdict].filter(([key, action]) => action === "fail" && key.startsWith(`${name}\t`)).map(([key]) => key.split("\t")[1]).filter((test) => test !== "");
  const leaves = failed.filter((test) => !failed.some((other) => other.startsWith(`${test}/`)));
  const late = ranPast.test(text);
  const broken = brokenBuild.get(name);
  let state = "FAIL";
  let what = `no test failed, and it ended in ${whole}`;
  if (broken !== undefined) what = `it does not build (${broken})`;
  else if (leaves.length > 0) what = `${leaves.length} tests failed${late ? ", and it ran past its bound" : ""}`;
  else if (late) {
    state = "UNKNOWN";
    what = "it ran past its bound and no test failed, so its rows neither passed nor failed: run it again";
  }
  outcome.set(name, state === "UNKNOWN" ? "unknown" : "fail");
  console.log(`${state} step go test ${pkg} (exit ${status}): ${what}`);
  why.push(`  why ${name} did not pass:`);
  if (broken !== undefined) for (const line of tail(built.get(broken) ?? "", 40)) why.push(`    ${line}`);
  for (const test of leaves) why.push(`    ${test}: ${lines(`${name}\t${test}`)}`);
  if (late) for (const line of stall(text)) why.push(`    ${line}`);
  if (broken === undefined && leaves.length === 0 && !late) for (const line of tail(text, 20)) why.push(`    ${line}`);
  if (stderr.trim() !== "") {
    why.push("    its stderr, last lines:");
    for (const line of tail(stderr, 20)) why.push(`      ${line}`);
  }
}
for (const row of fs.readFileSync(rows, "utf8").split("\n")) {
  const [kind, id, requirement, pkg, test] = row.split("\t");
  if (kind !== "go") continue;
  const name = nameOf(pkg);
  const ran = verdict.get(`${name}\t${test}`) ?? "did not run";
  const whole = verdict.get(`${name}\t`) ?? "did not end";
  const ok = ran === "pass" && whole === "pass";
  const state = ok ? "PASS" : outcome.get(name) === "unknown" && ran !== "fail" ? "UNKNOWN" : "FAIL";
  console.log(`${state} ${id} (${requirement}) ${pkg} ${test}${ok ? "" : `: test ${ran}, package ${state === "UNKNOWN" ? "ran past its bound" : whole}`}`);
}
for (const line of why) console.log(line);
' "$work/go.json" "$work/rows.tsv" "$module" "$work/go-steps.tsv" >"$work/go-rows.txt"
status=$?
judged "Go-row judge" "$status" "$work/go-rows.txt" \
  "${#packages[@]}" "package step lines (one for each package it ran)" '/^(PASS|FAIL|UNKNOWN) step go test /' \
  "$go_rows" "Go row lines (one for each Go row of the inventory)" '/^(PASS|FAIL|UNKNOWN) / && !/^[A-Z]+ step go test /'
failed=$((failed + $(grep -c '^FAIL' "$work/go-rows.txt")))
unknown=$(grep -c '^UNKNOWN' "$work/go-rows.txt")

# The verdict of each Bun row, from its file's JUnit report: the testcase whose full name (the
# names of the describe blocks around it and its own, joined by " > ", as Bun prints them) is the
# row's test, whole, so a longer name that begins with it is not that row. It passed when that
# testcase holds no failure, error or skipped child.
bun_reports=()
for i in "${!files[@]}"; do bun_reports+=("${files[$i]}=$work/bun-$i.xml"); done
# shellcheck disable=SC2016
bun -e '
const [rows, ...reports] = process.argv.slice(1);
const fs = require("node:fs");
const entities = { apos: "\x27", quot: "\"", amp: "&", lt: "<", gt: ">" };
const decode = (text) =>
  text.replace(/&(#x[0-9a-f]+|#[0-9]+|apos|quot|amp|lt|gt);/gi, (_, entity) =>
    entity[0] === "#"
      ? String.fromCodePoint(entity[1].toLowerCase() === "x" ? parseInt(entity.slice(2), 16) : parseInt(entity.slice(1), 10))
      : entities[entity.toLowerCase()]);
function cases(xml) {
  const found = new Map();
  const suites = [];
  let open = null;
  const close = () => {
    if (open === null) return;
    const before = found.get(open.name);
    found.set(open.name, before !== undefined && before !== "passed" ? before : open.state);
    open = null;
  };
  for (const [, closing, tag, attrs, self] of xml.matchAll(/<(\/?)(testsuite|testcase|failure|error|skipped)\b([^>]*?)(\/?)>/g)) {
    const name = decode(/\bname="([^"]*)"/.exec(attrs)?.[1] ?? "");
    if (tag === "testsuite") {
      if (closing) suites.pop();
      else if (!self) suites.push(name);
    } else if (tag === "testcase") {
      if (closing) close();
      else {
        open = { name: [...suites.slice(1), name].join(" > "), state: "passed" };
        if (self) close();
      }
    } else if (open !== null && !closing) {
      open.state = tag === "skipped" ? "was skipped" : "failed";
    }
  }
  return found;
}
const byFile = new Map();
for (const report of reports) {
  const at = report.indexOf("=");
  const path = report.slice(at + 1);
  byFile.set(report.slice(0, at), fs.existsSync(path) ? cases(fs.readFileSync(path, "utf8")) : null);
}
for (const row of fs.readFileSync(rows, "utf8").split("\n")) {
  const [kind, id, requirement, file, test] = row.split("\t");
  if (kind !== "bun") continue;
  const found = byFile.get(file);
  const state = found === null || found === undefined ? "has no JUnit report" : (found.get(test) ?? "is not in the report");
  console.log(`${state === "passed" ? "PASS" : "FAIL"} ${id} (${requirement}) ${file} ${test}${state === "passed" ? "" : `: the test ${state}`}`);
}
' "$work/rows.tsv" "${bun_reports[@]}" >"$work/bun-rows.txt"
status=$?
judged "Bun-row judge" "$status" "$work/bun-rows.txt" \
  "$bun_rows" "Bun row lines (one for each Bun row of the inventory)" '/^(PASS|FAIL) /'
failed=$((failed + $(grep -c '^FAIL' "$work/bun-rows.txt")))

# The box print test's rows, from each holder's REDACT-PRINT line in both languages' output: the
# rows whose twin sat at the row's own placement in the row's own shape (same) apart from those
# whose twin was reshaped or relocated, which show only that the printer's output is captured
# there. A run that printed no such line has tested no holder, and fails.
# shellcheck disable=SC2016
bun -e '
const fs = require("node:fs");
const [events, ...texts] = process.argv.slice(1);
const lines = [];
for (const line of fs.readFileSync(events, "utf8").split("\n")) {
  if (!line.startsWith("{")) continue;
  const out = JSON.parse(line).Output ?? "";
  const at = out.indexOf("REDACT-PRINT ");
  if (at >= 0) lines.push(out.slice(at).trim());
}
for (const text of texts) {
  for (const line of fs.existsSync(text) ? fs.readFileSync(text, "utf8").split("\n") : []) {
    if (line.startsWith("REDACT-PRINT ")) lines.push(line.trim());
  }
}
const total = { rows: 0, same: 0, reshaped: 0, relocated: 0, blind: 0 };
for (const line of lines) {
  for (const [, k, v] of line.matchAll(/(rows|same|reshaped|relocated|blind)=(\d+)/g)) total[k] += Number(v);
}
if (lines.length === 0) {
  console.log("FAIL print-test: no holder printed a REDACT-PRINT line, so no print row ran");
} else {
  console.log(`PRINT ${lines.length} holders: ${total.rows} printer and placement rows, ${total.same} with a twin at their own placement and shape, ${total.reshaped} reshaped and ${total.relocated} relocated (each shows only that the printer output is captured there), ${total.blind} blind`);
  for (const line of lines) console.log(`  ${line}`);
}
' "$work/go.json" "$work"/bun-*.txt >"$work/print-rows.txt"
status=$?
judged "print judge" "$status" "$work/print-rows.txt" \
  1 "PRINT or FAIL print-test lines (one for the print test)" '/^(PRINT [0-9]+ holders: |FAIL print-test: )/'
failed=$((failed + $(grep -c '^FAIL' "$work/print-rows.txt")))

rows=$(wc -l <"$work/rows.tsv")
if [ "$failed" -ne 0 ]; then
  echo "check c-redact-remote-text: failed at stage 4, the normal verification ($failed failures and $unknown UNKNOWN over $rows rows and the steps); the gate's stages 4 and 5 did not run"
  exit 1
fi
if [ "$unknown" -ne 0 ]; then
  echo "check c-redact-remote-text: UNKNOWN at stage 4, the normal verification ($unknown UNKNOWN over $rows rows and the steps, and no failure): a package ran past its bound, so run the check again; the gate's stages 4 and 5 did not run"
  exit 3
fi

# Stages 4 and 5 of the gate: each row's test unmutated, then each mutation alone. The gate exits 3
# when a run went past its bound and nothing failed.
"$gate" -stage mutate
status=$?
if [ "$status" -eq 3 ]; then
  echo "check c-redact-remote-text: UNKNOWN in the mutation gate: a run went past its bound and nothing failed, so run the check again"
  exit 3
fi
if [ "$status" -ne 0 ]; then
  echo "check c-redact-remote-text: failed in the mutation gate (exit $status)"
  exit 1
fi
cat <<'EOF'
What these results do not establish: printers outside the print test's set, payloads other than
the probe, and holder states the print test did not build; errors of libraries other than
golang.org/x/oauth2, github.com/coreos/go-oidc and openid-client, which reach output only
through redact.Error and redact.Cause; a call into one of those libraries made through
reflection (reflect.Value.MethodByName(...).Call), which no census lists and review sees; the
ways an answer's words leave it that no census reads from the types alone - the bytes StreamTo
or StreamAnswer hands out, printed; a test given to Holds that keeps the bytes; a member decoded
into a plain string, printed; and a response's header, trailer or Location, printed - which
review sees; and that the list of remote fields, which the spec's census and review settle, is
complete. The two forms the fixture's known-residual rows pin print by design: a bare
"label: secret@host", with no scheme and no key before it (split-user-info-bare), and the same
form as a JSON key's value (split-user-info-json-key).
EOF
echo "check c-redact-remote-text: passed ($rows rows, and the gate)"
exit 0
