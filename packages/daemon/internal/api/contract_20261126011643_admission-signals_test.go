package api

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The rows admission by signals adds to this package's tests: the report of model requests a host
// sends, and its answer, which names every id of the batch the daemon holds.
func init() {
	registerEntryTestRows(entryTestRows{
		entry: admissionSignalsContract,
		requests: map[string]reflect.Type{
			"POST /legion/v1/claims/model-requests": reflect.TypeFor[claim.ModelRequestsRequest](),
		},
		answers: func(*testing.T) map[AnswerID]answerFixture {
			return map[AnswerID]answerFixture{
				AnswerModelRequests: {roots: typesOf(claim.ModelRequestsResponse{}), bodies: []any{claim.ModelRequestsResponse{
					Kept: []string{"req-1", "req-2"},
				}}},
			}
		},
	})
}
