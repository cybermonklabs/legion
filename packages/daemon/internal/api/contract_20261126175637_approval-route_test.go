package api

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

func init() {
	registerEntryTestRows(entryTestRows{
		entry: approvalRouteContract,
		requests: map[string]reflect.Type{
			"POST /legion/v1/claims/approval": reflect.TypeFor[ClaimApprovalRequest](),
		},
		answers: func(t *testing.T) map[AnswerID]answerFixture {
			// The route's two goldens carry a standing approval and an explicit null.
			approved, err := approvalAnswer(&record.StandingApproval{Root: ownRoot, Spec: ownRoot, ArtifactID: ownDocument, Version: 24})
			if err != nil {
				t.Fatalf("the approval route's answer: %v", err)
			}
			return map[AnswerID]answerFixture{
				AnswerClaimApproval: {roots: typesOf(ClaimApprovalResponse{}), bodies: []any{approved, ClaimApprovalResponse{}}},
			}
		},
		states:      func(t *testing.T) []State { return []State{workHeldState(t)} },
		carry:       withWorkHeldIssue,
		valueStrips: workHoldValueStripCases,
	})
}

func workHoldValueStripCases(number int) map[string]valueStripCase {
	return map[string]valueStripCase{
		// A pod recording the arrival of why.state cannot read held. The claim at this
		// entry's contract states its own number, so it is not part of the pod's reckoning.
		// A caller below the field's arrival loses all why fields to the band instead.
		droppedWhy: {
			state: narrowingState, low: whyAndLineFieldsContract, high: number,
			carries: func(t *testing.T, body []byte) bool {
				for _, issue := range issueViews(t, body) {
					if why, carried := issue["why"].(map[string]any); carried && why["state"] == string(WhyHeld) {
						return true
					}
				}
				return false
			},
			stays: func(t *testing.T, whole, mixed []byte) {
				served := issueViews(t, mixed)
				others := 0
				for key, issue := range issueViews(t, whole) {
					got, carried := served[key]
					if !carried {
						t.Fatalf("the {%d, %d} pod is served no %s: the strip takes an answer and never an issue; %s",
							whyAndLineFieldsContract, number, key, mixed)
					}
					why, answers := issue["why"].(map[string]any)
					switch {
					case !answers:
						continue
					case why["state"] == string(WhyHeld):
						if got["why"] != nil {
							t.Errorf("the {%d, %d} pod is served %s's why %v, want none: the pod's %d process refuses the whole body for it",
								whyAndLineFieldsContract, number, key, got["why"], whyAndLineFieldsContract)
						}
					default:
						others++
						if !reflect.DeepEqual(got["why"], issue["why"]) {
							t.Errorf("the {%d, %d} pod is served %s's why %v, want %v: the strip takes the word the pod's %d process refuses and nothing else",
								whyAndLineFieldsContract, number, key, got["why"], issue["why"], whyAndLineFieldsContract)
						}
					}
				}
				if others == 0 {
					t.Fatalf("no issue of the fixture's state carries a why of a word that arrived with the field; this proves nothing: %s", whole)
				}
			},
			undeclaredIssue: Issue{Key: "LEGION-214", Why: &WhyView{State: undeclaredValue, Reason: "The body carries this state."}},
		},
	}
}
