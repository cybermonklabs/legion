package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A failed CI job reaches the implementer while the tester or the reviewer works, and moves
// nothing: k1 and k2 are checks that no CI job runs, and k3 is one a CI job runs, whose state names
// its job's case. The CI result of #42 that settles while the tester works is told to the
// implementer at once (tellChecks), before any state of k3 arrives, and starts no implementer; the
// tester's completion then moves the issue by the checks that no CI job runs, which pass, so k3,
// whose failure the end of the task holds, sends no work back. A state of k3 that says its
// job ended at the head of the pull request without a pass is told to the implementer alone, at
// once, in any phase: one message naming the check, the head and the job's case, and no start of
// the implementer. A state at a head the pull request has left, and a second state that says the
// same failure, tell nobody.
func TestAFailedCIJobReachesTheImplementerWhileTheReviewerWorks(t *testing.T) {
	k3Failed := ofSet(inCI(stateOf("k3", "no", "fail", "ci"), "failure"), "k1", "k2", "k3")

	t.Run("the CI result settles while the tester works", func(t *testing.T) {
		r := newStatesRig(t, phase.Testing)
		r.states(headOne, 1, passOf("k1", "tester"), passOf("k2", "tester"))
		applyRefusingNothing(t, r.pool, r.engine, settledAt(headOne, 1, "unit"))
		told := roleMessagesTo(t, r, claim.RoleImplementer)
		if len(told) != 1 || !strings.Contains(told[0], "Pull request #42") || !strings.Contains(told[0], `"unit" failed`) {
			t.Fatalf("before any state of k3 the implementer was told %q, want one message naming #42 and the failed job", told)
		}
		r.noStartIn(phase.Testing)

		r.states(headOne, 3, k3Failed)
		r.complete(headOne, 3)
		assertPhase(t, r.pool, phase.Reviewing)
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

// roleMessage is one role message LEGION-208's outbox holds: the role token it is published to,
// and its body.
type roleMessage struct{ role, body string }

// roleMessages is every role message LEGION-208's outbox holds, in the order written.
func roleMessages(t *testing.T, r *statesRig) []roleMessage {
	t.Helper()
	var messages []roleMessage
	for _, row := range issueOutboxOf(t, r.pool, "LEGION-208", "role_message") {
		var message record.RoleMessage
		if err := json.Unmarshal(row.payload, &message); err != nil {
			t.Fatalf("decode role message %s: %v", row.payload, err)
		}
		messages = append(messages, roleMessage{role: message.Role, body: message.Body})
	}
	return messages
}

// roleMessagesTo is the body of each role message published to role's topic of LEGION-208.
func roleMessagesTo(t *testing.T, r *statesRig, role claim.Role) []string {
	t.Helper()
	var bodies []string
	for _, message := range roleMessages(t, r) {
		if message.role == roleToken(t, role) {
			bodies = append(bodies, message.body)
		}
	}
	return bodies
}

// toldTheImplementerAlone fails unless LEGION-208's outbox holds exactly one role message, to the
// implementer's topic, naming k3, head and the job's case `failure`.
func (r *statesRig) toldTheImplementerAlone(head string) {
	r.t.Helper()
	messages := roleMessages(r.t, r)
	if len(messages) != 1 || messages[0].role != roleToken(r.t, claim.RoleImplementer) {
		r.t.Fatalf("the role messages are %+v, want one, to the implementer", messages)
	}
	for _, want := range []string{"k3", head, "failure"} {
		if !strings.Contains(messages[0].body, want) {
			r.t.Fatalf("the implementer was told %q, which does not name %q", messages[0].body, want)
		}
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
