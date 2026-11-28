package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/testexec"
)

// stubDispatch is the stand-in for the dispatch command that the tests hand ci-report through
// --dispatch-cli. It is this test's own program, not the real command. Each call writes its
// arguments, NUL-separated, to <n>.args, its standard input to <n>.stdin, the value of
// DISPATCH_RESULTS_TOKEN to <n>.token, the names of the DISPATCH_TOKEN* variables set, one a line,
// to <n>.bearers, and the names of the ACTIONS_ID_TOKEN_* variables set, one a line, to <n>.oidc,
// in the directory CI_REPORT_STUB_DIR names, n counting from 0. Then it prints what the real
// command prints for the answer CI_REPORT_STUB_MODE names (stubModes), and exits as it does.
const stubDispatch = `#!/bin/sh
calls="$CI_REPORT_STUB_DIR"
n=0
while [ -e "$calls/$n.args" ]; do n=$((n + 1)); done
printf '%s\0' "$@" > "$calls/$n.args"
cat > "$calls/$n.stdin"
printf '%s' "${DISPATCH_RESULTS_TOKEN-unset}" > "$calls/$n.token"
env | sed -n 's/^\(DISPATCH_TOKEN[A-Za-z0-9_]*\)=.*/\1/p' > "$calls/$n.bearers"
env | sed -n 's/^\(ACTIONS_ID_TOKEN_[A-Za-z0-9_]*\)=.*/\1/p' > "$calls/$n.oidc"
check=
while [ $# -gt 0 ]; do
  if [ "$1" = --check ]; then check=$2; fi
  shift
done
case $CI_REPORT_STUB_MODE in
refuse) echo "` + airlockRefusal + `"; exit 1 ;;
grant-ended) echo "` + tunnelRefused407Line + `"; exit 1 ;;
ends-after-first) if [ "$n" -ge 1 ]; then echo "` + tunnelRefused407Line + `"; exit 1; fi ;;
grant-refused) echo "` + grantRefusedLine + `"; exit 1 ;;
grant-ended-unknown) echo '{"write_outcome_unknown":"GRANT_NOT_LIVE"}'; exit 1 ;;
tunnel-unknown) echo '{"write_outcome_unknown":"AIRLOCK_TUNNEL_REFUSED"}'; exit 1 ;;
tunnel-503) echo "` + tunnelRefused503Line + `"; exit 1 ;;
landed) echo '{"landed_without_answer":"` + contracts.IdempotencyKeyCommitted + `"}'; exit 0 ;;
answer-lost) echo '{"landed_without_answer":"` + answerLost + `"}'; exit 0 ;;
no-id) echo '{"landed_without_answer":""}'; exit 0 ;;
unknown) echo '{"write_outcome_unknown":"ECONNRESET"}'; exit 1 ;;
unknown-exit-0) echo '{"write_outcome_unknown":"ECONNRESET"}'; exit 0 ;;
hang) echo "the write went out" >&2; exec sleep 30 ;;
esac
cat <<EOF
{
  "id": "row-$check",
  "seq": $n,
  "kind": "run",
  "attestation": {
    "_type": "https://in-toto.io/Statement/v1",
    "predicate": {
      "project": "ACME",
      "check": "$check",
      "recorder": {
        "session": "` + recorderSession + `",
        "principal": "workflow-results:example-org/example-repo/.github/workflows/ci-results.yaml"
      }
    }
  }
}
EOF
`

// stubModes names what the stub prints in each CI_REPORT_STUB_MODE, as the real command prints it:
//   - store, or no mode: a stored row, whole and over many lines, with the id row-<check> and the
//     run that recorded it, its session and principal; exit 0;
//   - refuse: the airlock's refusal of a request with no placeholder (airlockRefusal); exit 1;
//   - grant-ended: the airlock's 407 at the CONNECT to a grant that is not live
//     (tunnelRefused407Line); exit 1;
//   - ends-after-first: a stored row for the first call, and grant-ended for each later one, as
//     when the grant ends between two calls;
//   - grant-refused: the airlock's own GRANT_NOT_LIVE inside an open tunnel (grantRefusedLine);
//     exit 1;
//   - grant-ended-unknown: {"write_outcome_unknown":"GRANT_NOT_LIVE"}; exit 1;
//   - tunnel-unknown: {"write_outcome_unknown":"AIRLOCK_TUNNEL_REFUSED"}, a later tunnel refused
//     after an attempt went out, with no status; exit 1;
//   - tunnel-503: the airlock's 503 at the CONNECT (tunnelRefused503Line); exit 1;
//   - landed: {"landed_without_answer":"KEY_COMMITTED"}; exit 0;
//   - answer-lost: {"landed_without_answer":"ANSWER_LOST"}, a 2xx whose body the command could
//     not read; exit 0;
//   - no-id: {"landed_without_answer":""}; exit 0;
//   - unknown: {"write_outcome_unknown":"ECONNRESET"}, a write that went out and got no answer;
//     exit 1; unknown-exit-0 the same line with exit 0;
//   - hang: a line on standard error, then 30 s with no end, as a command that hangs.
const stubModes = "store refuse grant-ended ends-after-first grant-refused grant-ended-unknown tunnel-unknown tunnel-503 landed answer-lost no-id unknown unknown-exit-0 hang"

// airlockRefusal is what the stub prints in refuse mode: the airlock proxy's refusal, before it
// sent anything on, of a request that does not carry the grant's placeholder as its one
// Authorization header, as the real command prints it (packages/envoy-client/src/__tests__/
// dispatch-job-result.test.ts, "the airlock's refusal inside an open connection prints its code
// and its words").
const airlockRefusal = "NO_PLACEHOLDER: airlock: a request to this host carries the grant's [redacted] as its one Authorization header, DPoP ; this one does not…; nothing was stored"

// tunnelRefused407Line is what the real command prints when the airlock refused its tunnel 407 at
// the CONNECT, its answer to a placeholder whose result grant is not live: the tunnel's status and
// no code (dispatch-job-result.test.ts, "a CONNECT the proxy refuses with 407 ends the write at
// once").
const tunnelRefused407Line = "AIRLOCK_TUNNEL_REFUSED: the airlock proxy refused the connection (407 Proxy Authentication Required); nothing was sent to Dispatch"

// tunnelRefused503Line is what the real command prints when the airlock refused its tunnel 503 at
// the CONNECT, a grant it could not read just then, which is no end of the grant
// (dispatch-job-result.test.ts, "a CONNECT the proxy refuses with 503 is asked again within the
// bound").
const tunnelRefused503Line = "AIRLOCK_TUNNEL_REFUSED: the airlock proxy refused the connection (503 Service Unavailable); nothing was sent to Dispatch"

// grantRefusedLine is what the real command prints for the airlock's own GRANT_NOT_LIVE inside an
// open tunnel, before the write left it: "<code>: <words>; nothing was stored" (`refuse` of an
// AirlockRefusedError in job-result.ts), with the airlock's words of dispatch-job-result.test.ts.
const grantRefusedLine = "GRANT_NOT_LIVE: airlock: grant is expired, revoked, or its session ended (answer 7f3a); nothing was stored"

// answerLost is the code that the dispatch command gives a write whose 2xx it got and whose body
// it could not read: the client's own code, DISPATCH_ANSWER_LOST in
// packages/envoy-client/src/dispatch-http.ts, and none of Dispatch's.
const answerLost = "ANSWER_LOST"

// recorderSession is the session of the run that recorded every row the stub prints: a part of
// the row that is not its id.
const recorderSession = "ci-results:example-org/example-repo/.github/workflows/ci-results.yaml"

// call is one call of the stub: its arguments, its standard input, its DISPATCH_RESULTS_TOKEN
// ("unset" when it had none), the names of its DISPATCH_TOKEN* variables and the names of its
// ACTIONS_ID_TOKEN_* variables.
type call struct {
	args          []string
	stdin, token  string
	bearers, oidc []string
}

// stub is one copy of stubDispatch, with a directory of its own for its calls.
type stub struct {
	path, dir string
}

// newStub writes stubDispatch into a new directory, beside an empty directory for its calls.
func newStub(t *testing.T) stub {
	t.Helper()
	root := t.TempDir()
	s := stub{path: filepath.Join(root, "dispatch"), dir: filepath.Join(root, "calls")}
	if err := os.Mkdir(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testexec.WriteFile(s.path, []byte(stubDispatch), 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

// env is the environment that points ci-report's calls of the stub at its directory, in mode,
// one of stubModes.
func (s stub) env(mode string) []string {
	if !slices.Contains(strings.Fields(stubModes), mode) {
		panic("the stub has no mode " + mode)
	}
	return []string{"CI_REPORT_STUB_DIR=" + s.dir, "CI_REPORT_STUB_MODE=" + mode}
}

// calls is each call the stub got, in order.
func (s stub) calls(t *testing.T) []call {
	t.Helper()
	var calls []call
	for n := 0; ; n++ {
		args, err := os.ReadFile(filepath.Join(s.dir, fmt.Sprintf("%d.args", n)))
		if errors.Is(err, os.ErrNotExist) {
			return calls
		} else if err != nil {
			t.Fatal(err)
		}
		read := func(part string) string {
			t.Helper()
			content, err := os.ReadFile(filepath.Join(s.dir, fmt.Sprintf("%d.%s", n, part)))
			if err != nil {
				t.Fatal(err)
			}
			return string(content)
		}
		calls = append(calls, call{
			args:  strings.Split(strings.TrimSuffix(string(args), "\x00"), "\x00"),
			stdin: read("stdin"), token: read("token"),
			bearers: strings.Fields(read("bearers")), oidc: strings.Fields(read("oidc")),
		})
	}
}

// equalCall reports whether two calls have the same arguments and standard input and reached the
// command with the same ACTIONS_ID_TOKEN_* variables. The token and the bearers are
// TestTheActionRunsTheReporter's to read.
func equalCall(a, b call) bool {
	return slices.Equal(a.args, b.args) && a.stdin == b.stdin && slices.Equal(a.oidc, b.oidc)
}
