#!/usr/bin/env bash
# packages/envoy/scripts/check-redact-remote-text.sh
#
# The check that a remote's words, and a configured URL, print with no credential: the redaction
# of remote text (packages/envoy/AGENTS.md, "redact.Error(err, base)"). Run it from anywhere in
# the repository; it prints one line per inventory row and exits 0 only when every step and every
# row passed, 1 when one failed, 2 when it refused to run or the normal verification judged no row
# (below), and 3 when nothing failed but a run went past its bound, or a run of the mutation gate
# skipped a row's test (UNKNOWN: neither a pass nor a failure; run it again).
#
# It reads its inventory, packages/envoy/internal/redact/testdata/redact-check.json: each row a
# requirement, a Go package (relative to packages/envoy) or a Bun test file (relative to the
# repository root), a test, and for a print-site row the production code that test calls. It
#
#   - runs the normal verification of the rows, the mutation gate's -verify (cmd/redact-gate,
#     verify.go), in the working tree. It refuses an inventory it cannot parse, one with no Go row
#     or no Bun row, one whose row lacks a field or repeats an id, and one whose gate.row_timeout
#     (the time one row's test may take, its own budget) is not a whole count of seconds. It runs
#     `go test -json -count=1 -run '^(<names>)$'` once per package the inventory names, where
#     <names> are the top-level names of that package's inventory tests, each escaped and
#     anchored, under a timeout it derives for that package in this run from its own start-up run
#     (the gate's runBound says how and why); the whole packages are CI's. It fails on a non-zero
#     status, on a package that did not end in a pass, and on an inventory test or subtest that
#     did not run, did not pass, or was skipped; for a package that failed, it prints each failed
#     test with its first and last reported lines from go test's events (a test's t.Log lines read
#     as its t.Error lines do, and the last is the one nearest its failure), the compiler's errors
#     of one that does not build, and the tail of the output of one that failed with no failing
#     test. A package that ran past its timeout, and no test of which failed, is UNKNOWN, not
#     failed: it prints the timeout's panic line, the tests it was running and the goroutine
#     running each. It runs `bun test` on each Bun file in its package with Bun's JUnit reporter,
#     and fails on a non-zero status or on an inventory test the report holds no passing testcase
#     for; it reads the report rather than Bun's console, which prints no line for a passing test
#     where AGENT or CLAUDECODE is set, as it is in an agent's shell. It fails when no holder of
#     the print test printed a REDACT-PRINT line. It prints one verdict line for each package,
#     row, Bun file and the print test, and exits 0, 1 or 3; any other exit (a Go panic's 2 among
#     them) judged no row, and the script refuses it;
#   - runs `bun run typecheck` in packages/contracts, packages/cipher-client and
#     packages/envoy-client, and fails on a non-zero status.
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
# module (daemon/internal/daemon among them) and in the envoy module's loadtest, whose row
# TestABundlesMembersOfNoShapePrintNowhere writes to the loop seed's Postgres - and a row whose
# test skips for want of one fails ("test skip"), so set all three, to one database. No line this
# script prints is a real credential: the tests' credentials are made up (not-a-secret-...), and a
# failure line it prints may quote one.
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
work=$(mktemp -d "${TMPDIR:-/tmp}/redact-check.XXXXXX") || exit 2
trap 'rm -rf "$work"' EXIT

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

# Stage 4's normal verification of the rows: the gate's -verify (cmd/redact-gate, verify.go), in
# the working tree. It exits 0, 1 or 3, and its last line counts its verdicts ("verify: <rows>
# rows, <failed> failed, <unknown> UNKNOWN (...)"); any other exit - 2 where it refused the
# inventory, or a Go panic's - judged no row.
"$gate" -verify | tee "$work/verify.txt"
verify=${PIPESTATUS[0]}
read -r rows verify_failed unknown < <(awk '/^verify: [0-9]+ rows, [0-9]+ failed, [0-9]+ UNKNOWN \(/ { print $2, $4, $6 }' "$work/verify.txt")
case "$verify" in
0 | 1 | 3) ;;
*)
  echo "check c-redact-remote-text: refused: the gate's -verify exited $verify, so no row has a verdict"
  exit 2
  ;;
esac
if [ -z "${rows:-}" ]; then
  echo "check c-redact-remote-text: refused: the gate's -verify printed no count of its verdicts"
  exit 2
fi
failed=$((failed + verify_failed))

for package in contracts cipher-client envoy-client; do
  (cd "$root/packages/$package" && bun run typecheck) >"$work/typecheck-$package.txt" 2>&1
  step "bun run typecheck in packages/$package" $?
done

if [ "$failed" -ne 0 ]; then
  echo "check c-redact-remote-text: failed at stage 4, the normal verification ($failed failures and $unknown UNKNOWN over $rows rows and the steps); the gate's stages 4 and 5 did not run"
  exit 1
fi
if [ "$unknown" -ne 0 ]; then
  echo "check c-redact-remote-text: UNKNOWN at stage 4, the normal verification ($unknown UNKNOWN over $rows rows and the steps, and no failure): a package ran past its bound, so run the check again; the gate's stages 4 and 5 did not run"
  exit 3
fi

# Stages 4 and 5 of the gate: each row's test unmutated, then each mutation alone. The gate exits 3
# when a run went past its bound or skipped a row's test, or a subtest of it, and nothing failed.
"$gate" -stage mutate
status=$?
if [ "$status" -eq 3 ]; then
  echo "check c-redact-remote-text: UNKNOWN in the mutation gate: a run went past its bound or skipped a test and nothing failed, so run the check again"
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
into a plain string, printed; a member a type's own UnmarshalJSON keeps as it came (for example
githubText.Name, packages/daemon/internal/daemon/base_rules.go:75-86), printed; a response's
header, trailer, Location or cookies (Cookies, Set-Cookie), printed; the URL of a response's
request (Request.URL), printed, which after a followed redirect is the Location another party
wrote; net/http.ReadResponse given a nil request through a variable, or a request that carries
none of what was sent, whose answer then holds no record of the request's credentials; a response
held only behind an unexported pointer field, which fmt prints as an address and encoding/json
leaves out, printed by a printer that follows it (go-spew does; a census that walked every such
pointer would refuse each *net/http.Transport handed out as a RoundTripper); and a response handed
to a library function that takes it by its own type and prints it - which review sees; what a read
that a guarded_package entry names holds at run time beyond what the reflection census checks of
it (the one read guardedPackageRead in packages/envoy/cmd/redact-gate/guarded.go states), which
counts a func field as holding nothing and reads no type's methods, since a func's captured values
are out of reflect's reach and its results and a method's are reached only through a method the
census refuses; a credential cut into pieces shorter than redact.TokenPiece (8 characters) in
another party's words, which prints, since the rule redacts only runs of 8 or more of its
characters, and which reads whole once the gaps between the pieces are taken out; a value public
by its contract on a route, which is public only where the route matches the request's whole
escaped path, so a broker or listener served under a path prefix matches none of its routes and
its public values are redacted as credentials (an echo of one is cut, and SendBack can drop an id
that repeats one), which errs toward redacting;
and that the list of remote fields, which the spec's census and review settle, is complete. The
two forms the fixture's known-residual rows pin print by design: a bare "label: secret@host",
with no scheme and no key before it (split-user-info-bare), and the same form as a JSON key's
value (split-user-info-json-key).
EOF
echo "check c-redact-remote-text: passed ($rows rows, and the gate)"
exit 0
