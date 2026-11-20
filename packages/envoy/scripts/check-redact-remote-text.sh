#!/usr/bin/env bash
# packages/envoy/scripts/check-redact-remote-text.sh
#
# The check that a remote's words, and a configured URL, print with no credential: the redaction
# of remote text (packages/envoy/AGENTS.md, "redact.Error(err, base)"). Run it from anywhere in
# the repository; it prints one line per inventory row and exits 0 only when every step and every
# row passed.
#
# It reads its inventory, packages/envoy/internal/redact/testdata/redact-check.json: each row a
# requirement, a Go package (relative to packages/envoy) or a Bun test file (relative to the
# repository root), a test, and for a print-site row the production code that test calls. It
#
#   - refuses an inventory it cannot parse, one with no Go row or no Bun row, and one whose row
#     lacks a field or repeats an id;
#   - runs `go test -json -count=1 -run '^(<names>)$'` once per package the inventory names, where
#     <names> are the top-level names of that package's inventory tests, each escaped and
#     anchored, under that package's own timeout (go_timeout, twice its measured correct run);
#     the whole packages are CI's. It keeps each exit status, and fails on a non-zero status, on a
#     package that did not end in a pass, and on an inventory test or subtest that did not run,
#     did not pass, or was skipped; for a package that failed, it prints each failed test with its
#     first and last reported lines from go test's events (a test's t.Log lines read as its
#     t.Error lines do, and the last is the one nearest its failure);
#   - runs `bun test` on each Bun file in its package with Bun's JUnit reporter, keeps Bun's exit
#     status, and fails on a non-zero status or on an inventory test the report holds no passing
#     testcase for. It reads the report rather than Bun's console, which prints no line for a
#     passing test where AGENT or CLAUDECODE is set, as it is in an agent's shell;
#   - runs `bun run typecheck` in packages/contracts, packages/broker-client and
#     packages/envoy-client, and fails on a non-zero status.
#
# Around those steps it runs the mutation gate, packages/envoy/cmd/redact-gate, whose stages run
# in this order and stop at the first that fails, naming it: (1) the inventory; (2) the censuses -
# the call census (every call of the rule the change adds is a row's mutation), the boundary
# census (every call of golang.org/x/oauth2, go-oidc, google.golang.org/api/impersonate or
# cloud.google.com/go/cloudsqlconn is across redact.OAuth2, a text-only one under a CallScope, and
# every client or token source handed a library is one the boundary made), and the field census
# (every field the boundary sets is a closed value or the rule's reading); (3) each mutation's
# binding, before any test runs; then the steps above and (4) each row's test unmutated; then
# (5) each mutation alone, whose test must fail at its designated assertion with a marker in the
# output it captured there. `--controls` runs the gate's negative controls instead, each from a
# passing baseline in its own scratch worktree.
#
# A package's own needs stay its own: internal/dispatch/api and internal/broker/helper read the
# test databases their suites read (DISPATCH_TEST_DATABASE_URL, LEGION_TEST_PG_DSN) where a test
# of theirs needs one. No line this script prints is a real credential: the tests' credentials
# are made up (not-a-secret-...), and a failure line it prints may quote one.
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

# rows.tsv: kind, id, requirement, package or file, test. The JavaScript is in single quotes on
# purpose: its ${...} are its own.
# shellcheck disable=SC2016
if ! bun -e '
const [path] = process.argv.slice(1);
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
' "$inventory" >"$work/rows.tsv" 2>"$work/inventory.err"; then
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
if ! "$gate" -stage plan; then
  echo "check c-redact-remote-text: failed before any test ran"
  exit 1
fi

# Each package's inventory tests, and no other test of it: a row's test is `Top` or `Top/sub`, and
# `-run '^(Top|...)$'` runs each Top whole, every subtest with it. Each scoped run's test binary
# takes seconds at most even on a loaded box, so twice the longest is under a minute; the timeout
# is that minute, since the same test's time varies more than twofold with the box's load.
go_timeout=1m
mapfile -t packages < <(awk -F'\t' '$1 == "go" { print $4 }' "$work/rows.tsv" | sort -u)
: >"$work/go.json"
: >"$work/go.err"
for package in "${packages[@]}"; do
  names=$(awk -F'\t' -v p="$package" '$1 == "go" && $4 == p { sub(/\/.*/, "", $5); print $5 }' "$work/rows.tsv" |
    sort -u | sed 's/[][\.*^$+?(){}|\\]/\\&/g' | paste -sd'|')
  started=$(date +%s)
  (cd "$root/packages/envoy" && go test -json -count=1 -timeout "$go_timeout" -run "^($names)\$" "$package") >>"$work/go.json" 2>>"$work/go.err"
  status=$?
  echo "go test $package: $(($(date +%s) - started))s under $go_timeout"
  step "go test $package" "$status"
done

mapfile -t files < <(awk -F'\t' '$1 == "bun" { print $4 }' "$work/rows.tsv" | sort -u)
for i in "${!files[@]}"; do
  file=${files[$i]}
  package=$(printf '%s\n' "$file" | cut -d/ -f1-2)
  (cd "$root/$package" && bun test --reporter=junit --reporter-outfile="$work/bun-$i.xml" "${file#"$package"/}") >"$work/bun-$i.txt" 2>&1
  step "bun test $file" $?
done

for package in contracts broker-client envoy-client; do
  (cd "$root/packages/$package" && bun run typecheck) >"$work/typecheck-$package.txt" 2>&1
  step "bun run typecheck in packages/$package" $?
done

# The verdict of each Go row, and of each package, from go test's own events; for a package that
# did not pass, each failed test with its first and last reported lines, and the package's own
# where no test of it failed (a build failure, a panic, a timeout).
# shellcheck disable=SC2016
bun -e '
const [events, rows, module] = process.argv.slice(1);
const fs = require("node:fs");
const verdict = new Map();
const said = new Map();
const last = new Map();
const failing = (line) => /_test\.go:\d+:|^panic:|\bpanic: |^FAIL\b|build failed|timed out|cannot|undefined:/.test(line.trim());
for (const line of fs.readFileSync(events, "utf8").split("\n")) {
  if (!line.startsWith("{")) continue;
  const event = JSON.parse(line);
  const key = `${event.Package}\t${event.Test ?? ""}`;
  if (event.Action === "output" && failing(event.Output ?? "")) {
    if (!said.has(key)) said.set(key, event.Output.trim());
    last.set(key, event.Output.trim());
  }
  if (["pass", "fail", "skip"].includes(event.Action)) verdict.set(key, event.Action);
}
const named = new Set();
for (const row of fs.readFileSync(rows, "utf8").split("\n")) {
  const [kind, id, requirement, pkg, test] = row.split("\t");
  if (kind !== "go") continue;
  // A package of this module is named relative to packages/envoy; one of another module of the
  // workspace, the daemon, by its import path.
  const name = pkg.startsWith("./") ? `${module}/${pkg.replace(/^\.\//, "")}`.replace(/\/$/, "") : pkg;
  const ran = verdict.get(`${name}\t${test}`) ?? "did not run";
  const whole = verdict.get(`${name}\t`) ?? "did not end";
  const ok = ran === "pass" && whole === "pass";
  console.log(`${ok ? "PASS" : "FAIL"} ${id} (${requirement}) ${pkg} ${test}${ok ? "" : `: test ${ran}, package ${whole}`}`);
  if (whole !== "pass") named.add(name);
}
for (const name of named) {
  const failed = [...verdict].filter(([key, action]) => action === "fail" && key.startsWith(`${name}\t`)).map(([key]) => key.split("\t")[1]).filter((test) => test !== "");
  const leaves = failed.filter((test) => !failed.some((other) => other.startsWith(`${test}/`)));
  console.log(`  why ${name} failed:`);
  const lines = (key) => {
    const first = said.get(key) ?? "(no failure line)";
    const final = last.get(key);
    return final === undefined || final === first ? first : `${first}\n      last: ${final}`;
  };
  for (const test of leaves) console.log(`    ${test}: ${lines(`${name}\t${test}`)}`);
  if (leaves.length === 0) console.log(`    no test failed: ${lines(`${name}\t`)}`);
}
' "$work/go.json" "$work/rows.tsv" "$module" >"$work/go-rows.txt"
cat "$work/go-rows.txt"
failed=$((failed + $(grep -c '^FAIL' "$work/go-rows.txt")))

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
cat "$work/bun-rows.txt"
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
cat "$work/print-rows.txt"
failed=$((failed + $(grep -c '^FAIL' "$work/print-rows.txt")))

rows=$(wc -l <"$work/rows.tsv")
if [ "$failed" -ne 0 ]; then
  echo "check c-redact-remote-text: failed at stage 4, the normal verification ($failed failures over $rows rows and the steps); the gate's stages 4 and 5 did not run"
  exit 1
fi

# Stages 4 and 5 of the gate: each row's test unmutated, then each mutation alone.
"$gate" -stage mutate
status=$?
if [ "$status" -ne 0 ]; then
  echo "check c-redact-remote-text: failed in the mutation gate (exit $status)"
  exit 1
fi
cat <<'EOF'
What these results do not establish: printers outside the print test's set, payloads other than
the probe, and holder states the print test did not build; errors of libraries other than
golang.org/x/oauth2 and openid-client, which reach output only through redact.Error and
redact.Cause; and that the list of remote fields, which the spec's census and review settle, is
complete. A bare "label: secret@host", with no scheme and no key before it, prints by design: the
fixture's known-residual row pins it.
EOF
echo "check c-redact-remote-text: passed ($rows rows, and the gate)"
exit 0
