package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// The test rows of the wait entry (contract_20261127164432_wait.go, dispatch://INFRA-2040): the
// requests of the two claim routes it adds, rows of pluginRequests; and the fixtures of its two
// answers, and the owed answer's bodies with the two keys it adds there, rows of answerFixtures.
// It carries nothing into narrowingState: the populated state golden holds both keys it adds to
// a claim view (LEGION-208's implementer's wait, LEGION-221's merger's hold), so its band finds
// both to take and writes its own line. It adds no state body: the two keys are on every claim
// view of contract_shapes_test.go's stateCarryingEveryDeclaredValue, which carries every value of
// `hold.step` at both key paths.
func init() {
	registerEntryTestRows(entryTestRows{
		entry: waitContract,
		requests: map[string]reflect.Type{
			"POST /legion/v1/claims/wait":      reflect.TypeFor[WaitRequest](),
			"POST /legion/v1/claims/wait-news": reflect.TypeFor[WaitNewsRequest](),
		},
		answers: func(*testing.T) map[AnswerID]answerFixture {
			return map[AnswerID]answerFixture{
				// The wait golden's own value (TestWaitResponseGolden): the wait as the daemon
				// recorded it, whose four keys are always written.
				AnswerWait: {roots: typesOf(WaitResponse{}), bodies: []any{WaitResponse{Wait: WaitView{
					ID: "3f6c2a9e-8b14-4d7a-9e25-1c0b7f4d2a68", WaitingOn: "the review of LEGION-207's pull request",
					WakeAt:     time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC),
					DeclaredAt: time.Date(2026, 11, 14, 6, 0, 0, 123456000, time.UTC),
				}}}},
				// The two goldens of the news report (TestWaitNewsResponseGolden): a report that
				// ended its wait, and the claim's own update ask, which ended nothing and says so
				// under `updateAsk`. Between them they carry every key this answer writes.
				AnswerWaitNews: {roots: typesOf(WaitNewsResponse{}), bodies: []any{
					WaitNewsResponse{Ended: true}, WaitNewsResponse{UpdateAsk: true},
				}},
				// The owed answer's second golden (owed-wait-hold.json, claims_owed_test.go): the
				// merger's answer with a wait and a hold, beside the repository correction's own
				// body, so the bodies carry every key of an OwedResponse. The hold is written once on
				// each step the entry declares for it (holdSteps), so the bodies carry every value of
				// `hold.step`.
				AnswerOwed: {roots: typesOf(OwedResponse{}), bodies: owedWaitingAndHeldBodies()},
			}
		},
	})
}

// owedWaitingAndHeldBodies is the owed golden of a waiting, held merger (owedWaitingAndHeld) on
// each step of holdSteps in turn.
func owedWaitingAndHeldBodies() []any {
	bodies := make([]any, 0, len(holdSteps))
	for _, step := range holdSteps {
		bodies = append(bodies, owedWaitingAndHeld(record.DesignStep(step.Value)))
	}
	return bodies
}
