package workflow

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A failed CI job reaches the implementer while the tester or the reviewer works, and moves
// nothing: k1 and k2 are checks that no CI job runs, and k3 is one a CI job runs, whose state names
// its job's case. The CI result of #42 that settles while the tester works, its job acme-unit
// failed with GitHub's word failure, is told to the implementer at once (tellChecks), before any
// state of k3 arrives, and starts no implementer; the tester's completion then moves the issue by
// every check, k3 among them, as the testing move reads them with the engine's TestingLeavesCI
// false, the daemon's wiring. A state of k3 that says its job ended at the head of the pull
// request without a pass is told to the implementer alone, at once, in any phase: one message
// whose row names the pull request, the check, the head and the job's case, and no start of the
// implementer. A state at a head the pull request has left, and a second state that says the same
// failure, tell nobody. The test reads the rows' fields and the lines written from them, never a
// word of a message.
func TestAFailedCIJobReachesTheImplementerWhileTheReviewerWorks(t *testing.T) {
	k3Failed := ofSet(inCI(stateOf("k3", "no", "fail", "ci"), "failure"), "k1", "k2", "k3")

	t.Run("the CI result settles while the tester works", func(t *testing.T) {
		r := newStatesRig(t, phase.Testing)
		r.states(headOne, 1, passOf("k1", "tester"), passOf("k2", "tester"))
		applyRefusingNothing(t, r.pool, r.engine, settledRunsAt(headOne, 1, []record.AttemptRun{{Name: "build", ID: 1},
			{Name: "acme-unit", ID: 1, Conclusion: record.RunFailed, GitHubConclusion: "failure"}}, "acme-unit"))
		implementer := roleToken(t, claim.RoleImplementer)
		told := roleMessagesTo(t, r, claim.RoleImplementer)
		if len(told) != 1 || told[0].PullRequest != 42 || !slices.Contains(told[0].Checks, "acme-unit=failure") {
			t.Fatalf("before any state of k3 the implementer was told %+v, want one message of #42 whose checks hold acme-unit=failure", told)
		}
		lines := r.lines(settlementTold)
		if len(lines) != 1 || lines[0]["issue"] != "LEGION-208" || lines[0]["pull_request"] != float64(42) ||
			!slices.Contains(lineStrings(lines[0], "checks"), "acme-unit=failure") || !slices.Contains(lineStrings(lines[0], "told"), implementer) {
			t.Fatalf("the lines %q = %v, want one of #42 whose checks hold acme-unit=failure and whose told holds %s", settlementTold, lines, implementer)
		}
		r.noStartIn(phase.Testing)

		r.states(headOne, 3, k3Failed)
		r.complete(headOne, 3)
		r.movedBackFailing(headOne, failingOf("k3", "no", "fail"))
	})

	t.Run("a state of k3 arrives while the tester works", func(t *testing.T) {
		r := newStatesRig(t, phase.Testing)
		r.states(headOne, 1, passOf("k1", "tester"), passOf("k2", "tester"))
		r.states(headOne, 3, k3Failed)
		r.toldTheImplementerAlone(headOne)
		r.noStartIn(phase.Testing)

		r.states(headOne, 4, k3Failed)
		r.toldTheImplementerAlone(headOne)
	})

	t.Run("a state of k3 at the head the pull request left", func(t *testing.T) {
		r := newStatesRig(t, phase.Testing)
		seedPR(t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: statesRepo, Number: 42, Branch: "legion/LEGION-208",
			HeadSHA: headTwo, CheckedHead: headTwo, Failing: []string{}})
		r.states(headOne, 1, k3Failed)
		if told := roleMessages(t, r); len(told) != 0 {
			t.Fatalf("a failure of k3 at %s, a head #42 has left, told %+v, want nobody", headOne, told)
		}
		if lines := r.lines(implementerToldOfCIFailure); len(lines) != 0 {
			t.Fatalf("a failure of k3 at %s, a head #42 has left, logged %v, want no line %q", headOne, lines, implementerToldOfCIFailure)
		}
		r.states(headTwo, 2, k3Failed)
		r.toldTheImplementerAlone(headTwo)
	})

	t.Run("a state of k3 arrives while the reviewer works", func(t *testing.T) {
		r := newStatesRig(t, phase.Reviewing)
		r.states(headOne, 1, k3Failed)
		r.toldTheImplementerAlone(headOne)
		r.noStartIn(phase.Reviewing)
	})
}

// settlementTold and implementerToldOfCIFailure are the lines the engine writes where it writes a
// CI settlement's role messages (workflow's tellChecks) and the implementer's message of a failed
// CI job (tellCIFailure), each from the rows it writes.
const (
	settlementTold             = "workflow: a CI settlement is told"
	implementerToldOfCIFailure = "workflow: the implementer is told of a failed CI job"
)

// roleMessages is every role message LEGION-208's outbox holds, in the order written, each
// decoded whole as the role receives it.
func roleMessages(t *testing.T, r *statesRig) []record.RoleMessage {
	t.Helper()
	var messages []record.RoleMessage
	for _, row := range issueOutboxOf(t, r.pool, "LEGION-208", "role_message") {
		var message record.RoleMessage
		if err := json.Unmarshal(row.payload, &message); err != nil {
			t.Fatalf("decode role message %s: %v", row.payload, err)
		}
		messages = append(messages, message)
	}
	return messages
}

// roleMessagesTo is each role message published to role's topic of LEGION-208.
func roleMessagesTo(t *testing.T, r *statesRig, role claim.Role) []record.RoleMessage {
	t.Helper()
	var told []record.RoleMessage
	for _, message := range roleMessages(t, r) {
		if message.Role == roleToken(t, role) {
			told = append(told, message)
		}
	}
	return told
}

// toldTheImplementerAlone fails unless LEGION-208's outbox holds exactly one role message, to the
// implementer's topic, whose row names #42, k3, head and the job's case `failure`, and the engine's
// log holds exactly one line of the implementer told of a failed CI job naming the same. It reads
// no word of the message's body.
func (r *statesRig) toldTheImplementerAlone(head string) {
	r.t.Helper()
	messages := roleMessages(r.t, r)
	if len(messages) != 1 || messages[0].Role != roleToken(r.t, claim.RoleImplementer) || messages[0].PullRequest != 42 ||
		messages[0].Check != "k3" || messages[0].Head != head || messages[0].CICase != "failure" {
		r.t.Fatalf("the role messages are %+v, want one, to the implementer, of #42, k3, %s and the case failure", messages, head)
	}
	lines := r.lines(implementerToldOfCIFailure)
	if len(lines) != 1 || lines[0]["issue"] != "LEGION-208" || lines[0]["pull_request"] != float64(42) || lines[0]["check"] != "k3" ||
		lines[0]["head"] != head || lines[0]["ci_case"] != "failure" {
		r.t.Fatalf("the lines %q = %v, want one of #42, k3, %s and the case failure", implementerToldOfCIFailure, lines, head)
	}
}

// noStartIn fails unless LEGION-208 stays in at with no worker started.
func (r *statesRig) noStartIn(at phase.Phase) {
	r.t.Helper()
	assertPhase(r.t, r.pool, at)
	if starts := r.starts(); starts != 0 {
		r.t.Fatalf("a failed CI job started %d workers, want none", starts)
	}
}
