// Package counttest holds what tests whose verdict is a count, and not a clock, share across
// packages: the largest registration production has seen (LargestRegistration), which
// cmd/listener's registration row and internal/bus's counts of the reduction each weigh, and a run
// of the test binary for one test alone (RunAlone), so that a count of the process's allocations is
// that test's alone. It is an ordinary package rather than a _test.go file because Go cannot share
// test-only code across packages (see internal/stacktest's identical reasoning).
package counttest

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// LargestRegistration is the topics of a registration by holder, the holder of role, of about twice
// the largest shape production has seen (802 topics, 2026-11-16): a holder following 800 Dispatch
// issues, each as `<KEY>` and `<KEY>.>`, with the per-kind topics of one issue that its `<KEY>.>`
// already holds, and the lane topic of role. The per-kind topics are those of an issue that sorts
// among the others, as production's did, and not first: each is then found contained only after
// the pairs before it are weighed.
//
// body is the 1,609 topics the registration's body names. planned is body with holder's own agent
// subject, the 1,610 topics the registration route plans from that body (cmd/listener's
// subscribeHandler); their forms (contracts.TopicForms) are 1,612 subjects, which reduce to 1,604
// filters. A consumer call of the registration reduces neither list: its filter leaves the role
// lane out (cmd/listener's sessionFilter, bus.RoleLane), so it reduces 1,609 topics in 1,610 forms
// to the 1,602 filters the server is handed. The role lane's two forms sort after every other form
// and overlap none, so in planned they are two filters more, and every other filter is the same.
func LargestRegistration(role, holder string) (body, planned []string) {
	const issues = 800
	body = make([]string, 0, 2*issues+9)
	for issue := range issues {
		topic := fmt.Sprintf("notifications.dispatch.issue.K-%d", issue)
		body = append(body, topic, topic+".>")
	}
	for _, kind := range []string{"message", "issue", "comment", "ask"} {
		topic := "notifications.dispatch.issue.K-400." + kind
		body = append(body, topic, topic+".>")
	}
	body = append(body, contracts.RoleTopicPrefix+role)
	return body, append(body[:len(body):len(body)], contracts.LegacyAgentSubject(holder))
}

// RunAlone runs the test binary again for t's test alone, with env set in that run's environment,
// for a test whose measurement must be its own: testing.AllocsPerRun counts every allocation the
// process makes while it measures, and a package's other tests leave servers and watchers running
// that allocate meanwhile.
//
// In that run, where env is set, RunAlone reports true, and the test measures there; that run is
// never parallel, since testing.AllocsPerRun refuses to measure in a parallel test, so a test whose
// starting run is parallel calls t.Parallel only where env is unset. In the run that starts it,
// RunAlone waits for that run, logs its output, and fails t unless it passed and its output holds
// marker exactly want times; it then reports false, and the test returns. marker is the phrase the
// test logs once for each measurement it makes, one constant of the test's in both places, so a run
// that reported another number of measurements, or none, fails t.
func RunAlone(t *testing.T, env, marker string, want int) bool {
	t.Helper()
	if os.Getenv(env) != "" {
		return true
	}
	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), env+"=1")
	output, err := child.CombinedOutput()
	t.Logf("the run of this test alone:\n%s", output)
	if err != nil {
		t.Fatalf("the run of this test alone failed: %v", err)
	}
	if measured := strings.Count(string(output), marker); measured != want {
		t.Fatalf("the run of this test alone reported %d measurements; want %d", measured, want)
	}
	return false
}
