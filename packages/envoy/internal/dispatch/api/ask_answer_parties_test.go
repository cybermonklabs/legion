package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// recordedParties is what one ask.answered event records of the parties its answer's text names:
// the routes it holds, and whether its payload carries the field at all.
type recordedParties struct {
	targets []string
	present bool
}

// answeredParties is what each ask.answered event of askID records of the parties its answer's
// text names, oldest first.
func answeredParties(t *testing.T, database *store.Store, askID string) []recordedParties {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select payload->'named_targets' from events
		where type = 'ask.answered' and payload->>'id' = $1 order by id
	`, askID)
	if err != nil {
		t.Fatalf("read the ask.answered events of ask %s: %v", askID, err)
	}
	defer rows.Close()
	var recorded []recordedParties
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan an ask.answered event of ask %s: %v", askID, err)
		}
		var parties recordedParties
		if raw != nil {
			parties.present = true
			if err := json.Unmarshal(raw, &parties.targets); err != nil {
				t.Fatalf("decode named_targets %s: %v", raw, err)
			}
		}
		recorded = append(recorded, parties)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the ask.answered events of ask %s: %v", askID, err)
	}
	return recorded
}

// answerWithText answers askID as alice with the Ship option and text, and fails unless the route
// takes it. replaces is the `at` of the answer a change replaces, nil for a first answer.
func answerWithText(t *testing.T, handler http.Handler, askID, text string, replaces *string) model.Ask {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{"Ship"}, "text": text, "expected_answer_at": replaces,
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("answer %q: status=%d body=%s", text, response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response)
}

// R14: the answer route records with the answer the agents and the roles its text names, read by
// a comment's rules, for the outbox to send the answer to. A mention link as the dashboard writes
// one names its party; a person is not told; a reference inside code names nobody; a party named
// twice is one; an address no route reads names nobody, and the answer is taken all the same; and
// eleven agents, past a comment's cap of ten, are each recorded, with no refusal. An answer that
// names no agent and no role records no field at all. With the party read taken out of the
// answer route, every row that names an agent or a role records nothing.
func TestAnAnswerRecordsTheAgentsAndRolesItsTextNames(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Answer parties", "A spec")
	eleven := make([]string, 0, 11)
	elevenRoutes := make([]string, 0, 11)
	for index := range 11 {
		eleven = append(eleven, fmt.Sprintf("[@Agent %d](dispatch://agent/session-%d)", index, index))
		elevenRoutes = append(elevenRoutes, fmt.Sprintf("session:session-%d", index))
	}
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"an agent", "Ship it. [@Planner](dispatch://agent/session-planner) takes it from here.", []string{"session:session-planner"}},
		{"a role", "Ship it. [@Reviewer](dispatch://role/reviewer) confirms.", []string{"role:reviewer"}},
		{"a person", "Ship it. [Bob](dispatch://person/bob@example.com) reads this.", nil},
		{"an agent in a code span", "Ship it. `dispatch://agent/session-planner` is the address.", nil},
		{"an agent in a fenced block", "Ship it.\n\n```\ndispatch://agent/session-planner\n```", nil},
		{"an agent named twice", "[@Planner](dispatch://agent/session-planner), then [@Planner](dispatch://agent/session-planner) again.", []string{"session:session-planner"}},
		{"an address no route reads", "Ship it. [@Odd](dispatch://agent/a.b) and [@Planner](dispatch://agent/session-planner).", []string{"session:session-planner"}},
		{
			"an agent, a role, a person and code together",
			"[@Reviewer](dispatch://role/reviewer), [Bob](dispatch://person/bob@example.com), `dispatch://role/in-code` " +
				"and [@Planner](dispatch://agent/session-planner).",
			[]string{"role:reviewer", "session:session-planner"},
		},
		{"eleven agents", "Ship it. " + strings.Join(eleven, " "), elevenRoutes},
		{"nobody", "Ship it.", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ask := createChangeableAsk(t, handler, issue.Key)
			answerWithText(t, handler, ask.ID, tc.text, nil)
			recorded := answeredParties(t, database, ask.ID)
			if len(recorded) != 1 {
				t.Fatalf("the answer appended %d ask.answered events, want one", len(recorded))
			}
			if got := recorded[0]; !slices.Equal(got.targets, tc.want) || got.present != (tc.want != nil) {
				t.Fatalf("the answer %q records named_targets %#v (present %v), want %#v", tc.text, got.targets, got.present, tc.want)
			}
		})
	}
}

// A changed answer records the parties its new text names, and the answer it replaced keeps its
// own: each ask.answered event carries the parties of the text it carries.
func TestAChangedAnswerRecordsThePartiesOfItsNewText(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Changed answer parties", "A spec")
	ask := createChangeableAsk(t, handler, issue.Key)
	first := answerWithText(t, handler, ask.ID, "Ship it. [@Planner](dispatch://agent/session-planner) takes it.", nil)
	firstAt := timestampValue(first.Answer.At)
	answerWithText(t, handler, ask.ID, "Ship it. [@Reviewer](dispatch://role/reviewer) confirms first.", &firstAt)
	recorded := answeredParties(t, database, ask.ID)
	if len(recorded) != 2 {
		t.Fatalf("the ask holds %d ask.answered events, want the first answer and its change", len(recorded))
	}
	if !slices.Equal(recorded[0].targets, []string{"session:session-planner"}) {
		t.Fatalf("the first answer records %#v, want the planner it named", recorded[0].targets)
	}
	if !slices.Equal(recorded[1].targets, []string{"role:reviewer"}) {
		t.Fatalf("the changed answer records %#v, want the role its new text names alone", recorded[1].targets)
	}
}

// The review route answers an open approval request with the review's reason as the answer's
// text, through the answer transition with no party read (reviews.go), so a reason that names an
// agent and a role records neither: only the answer route tells the parties a text names.
func TestAReviewsAnswerOfAnApprovalRequestRecordsNoParty(t *testing.T) {
	t.Parallel()
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "Review names parties", "A spec")
	request := requestApproval(t, handler, issue.PrimaryArtifactID)
	reason := "Approved. [@Planner](dispatch://agent/session-planner) and [@Reviewer](dispatch://role/reviewer) go on."
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", reason)
	var text string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(payload->'answer'->>'text', '') from events
		where type = 'ask.answered' and payload->>'id' = $1 order by id desc limit 1
	`, request.ID).Scan(&text); err != nil {
		t.Fatalf("read the request's answer: %v", err)
	}
	if !strings.Contains(text, "dispatch://agent/session-planner") {
		t.Fatalf("the request's answer reads %q, want the reason that names the planner", text)
	}
	for _, recorded := range answeredParties(t, database, request.ID) {
		if recorded.present {
			t.Fatalf("the review's answer records named_targets %#v, want none", recorded.targets)
		}
	}
}
