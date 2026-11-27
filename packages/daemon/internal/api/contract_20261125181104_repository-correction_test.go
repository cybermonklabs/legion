package api

import (
	"reflect"
	"testing"
)

// The test rows of the repository correction's entry (dispatch://INFRA-3136): the request of the
// owed route it adds, a row of pluginRequests, and the owed answer's fixture, a row of
// answerFixtures. The four keys it adds to each issue are on the populated state golden already
// (LEGION-210 and LEGION-212), so it adds no state body and writes nothing into narrowingState.
func init() {
	registerEntryTestRows(entryTestRows{
		entry: repositoryCorrectionContract,
		requests: map[string]reflect.Type{
			"POST /legion/v1/claims/owed": reflect.TypeFor[OwedRequest](),
		},
		answers: func(*testing.T) map[AnswerID]answerFixture {
			return map[AnswerID]answerFixture{
				// The owed golden's own value (owed.json, claims_owed_test.go): a claim that owes
				// its phase, named with the architect that owns it, which between them carry every
				// key of an OwedResponse.
				AnswerOwed: {roots: typesOf(OwedResponse{}), bodies: []any{OwedResponse{
					Owes: true, Work: "the testing phase of LEGION-208", Architect: "legion-legion-legion-208-architect",
				}}},
			}
		},
	})
}
