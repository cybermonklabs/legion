package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The task-take entry's rows of this package's test tables (entryTestRows): the three routes it
// adds that decode a body a plugin sends, the fixtures of the two answers it adds and a register
// body carrying the two keys it adds to the registration, and a state body carrying every value
// it adds to the state. Its band takes `unseenTaskValues`, which the populated state golden
// already carries, so it writes nothing into narrowingState (carry).
func init() {
	registerEntryTestRows(entryTestRows{
		entry: taskTakeContract,
		requests: map[string]reflect.Type{
			"POST /legion/v1/claims/task":      reflect.TypeFor[claim.TaskRequest](),
			"POST /legion/v1/claims/task-seen": reflect.TypeFor[claim.TaskSeenRequest](),
			"POST /legion/v1/claims/take":      reflect.TypeFor[claim.TakeRequest](),
		},
		answers: taskTakeAnswerFixtures,
		states:  taskTakeStates,
	})
}

// taskTakeAnswerFixtures is the entry's rows of answerFixtures. The register golden's own value
// (TestRegisterResponseGolden), a claim whose bucket holds a value, writes every key of `task`
// and `finished`. The task read's fixture is richer than its golden, which reads a bucket that
// holds no value, so the record describes every key of the value a read answers. The take's are
// the three goldens' own values (TestTakeResponseGolden), one for each outcome, which between
// them carry every key of the answer.
func taskTakeAnswerFixtures(*testing.T) map[AnswerID]answerFixture {
	value := &claim.TaskValue{Number: 4, Epoch: "2026-11-13T16:21:44.123456789Z", Revision: 12}
	return map[AnswerID]answerFixture{
		AnswerRegister: {bodies: []any{claim.RegisterResponse{
			ClaimToken:   "legion-legion-legion-208-architect",
			Tree:         "LEGION-208",
			Issue:        "LEGION-208",
			Role:         claim.RoleArchitect,
			Generation:   3,
			Secret:       "U3VwZXJ2aXNlZEJ5TGVnaW9u",
			PromptAgents: []string{"deep-worker", "oracle", "plan-gap-analyst"},
			Task:         value,
			Finished:     3,
		}}},
		AnswerTask: {roots: typesOf(claim.TaskResponse{}), bodies: []any{claim.TaskResponse{Task: value, Finished: 3}}},
		AnswerTake: {roots: typesOf(claim.TakeResponse{}), bodies: []any{
			claim.TakeResponse{Outcome: string(supervise.TakeOutcomeAccepted),
				Text: "Implement LEGION-209 on its branch, then complete the phase with legion handoff_complete."},
			claim.TakeResponse{Outcome: string(supervise.TakeOutcomeRefused), Hold: supervise.HoldStalePlugin},
			claim.TakeResponse{Outcome: string(supervise.TakeOutcomeDropped)},
		}},
	}
}

// taskTakeStates is the entry's state body: the populated state golden with a claim of every role
// whose task value nobody reported seeing, and one more issue whose architect and implementer
// carry `never_reconnected` in `failure.cause` and in `pendingCause`, so every value the entry
// adds to the state is carried by a body, at every key path it is reachable at.
func taskTakeStates(t *testing.T) []State {
	state := populatedState(t)
	at := time.Date(2026, 11, 17, 9, 14, 3, 0, time.UTC)
	unseen := make([]UnseenTaskValue, 0, len(floorRoles))
	for i, role := range floorRoles {
		unseen = append(unseen, UnseenTaskValue{Claim: claim.Token("legion-take-TAKE-1-" + string(role)), Issue: "TAKE-1",
			Role: role, Task: uint64(i + 1), WrittenAt: at})
	}
	failed := func(session string) ClaimView {
		return ClaimView{Session: session, State: string(supervise.StateFailed),
			Failure: &ClaimFailureView{Cause: supervise.CauseNeverReconnected, Detail: "launch failures ran out", At: &at,
				Attempts: BudgetsView{LaunchFailures: 3}},
			PendingCause: supervise.CauseNeverReconnected}
	}
	architect := failed("ses_architect_TAKE-1")
	issues := make(map[string]Issue, len(state.Issues)+1)
	for key, issue := range state.Issues {
		issues[key] = issue
	}
	issues["TAKE-1"] = Issue{Key: "TAKE-1", Generation: 1, Phase: phase.Implementing, Status: "in_progress", Tree: "TAKE-1",
		Architect: &architect,
		Workers:   map[claim.Role]PhaseView{claim.RoleImplementer: {Claim: failed("ses_implementer_TAKE-1"), Rounds: 1}}}
	state.Issues = issues
	state.UnseenTaskValues = unseen
	return []State{state}
}
