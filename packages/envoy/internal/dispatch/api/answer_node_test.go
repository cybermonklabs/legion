package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Every writer of an ask's answer state writes it on the ask's node first (asks.Settle), and the
// asks row is the projection of what it wrote. After each writer the
// node's answer_state, its answer's words, selection and time, its answered_by and decided_by, and
// its resolution read what the row reads, the answer_state carrying the event that write appended
// as its cause. The writers: the answer route, a change of the answer, the resolve route, the
// decline route, an issue's close (its withdrawal) and its reopen, the approval carry's answer of an
// open approval request, and settlement's retraction of an ask whose block left its document and
// its restoration when the block comes back.
func TestEveryWriterOfAnAnswerWritesItOnTheNode(t *testing.T) {
	t.Run("the ask routes and an issue's close and reopen", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "NODE", "Answers on the node", "A spec")

		answered := createChangeableAsk(t, handler, issue.Key)
		first := answerAskForChange(t, handler, answered.ID, "alice", "Ship", nil)
		requireTheAnswerOnTheNode(t, database, "the answer route", answered.ID, "ask.answered")
		firstAt := timestampValue(first.Answer.At)
		answerAskForChange(t, handler, answered.ID, "alice", "Hold", &firstAt)
		requireTheAnswerOnTheNode(t, database, "a change of the answer", answered.ID, "ask.answered")

		resolved := createChangeableAsk(t, handler, issue.Key)
		if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+resolved.ID+"/resolve", map[string]any{
			"kind": "resolved", "reason": "Settled in the thread.", "actor": sessionActor(),
		}); response.Code != http.StatusOK {
			t.Fatalf("resolve the ask: status=%d body=%s", response.Code, response.Body.String())
		}
		requireTheAnswerOnTheNode(t, database, "the resolve route", resolved.ID, "ask.resolved")

		declined := createChangeableAsk(t, handler, issue.Key)
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+declined.ID+"/decline",
			map[string]any{"note": "Not mine to answer."}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("decline the ask: status=%d body=%s", response.Code, response.Body.String())
		}
		requireTheAnswerOnTheNode(t, database, "the decline route", declined.ID, "ask.resolved")

		withdrawn := createChangeableAsk(t, handler, issue.Key)
		closeIssue(t, handler, issue.Key)
		requireTheAnswerOnTheNode(t, database, "an issue's close", withdrawn.ID, "ask.resolved")
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
			map[string]any{"status": "todo"}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("reopen the issue: status=%d body=%s", response.Code, response.Body.String())
		}
		requireTheAnswerOnTheNode(t, database, "an issue's reopen", withdrawn.ID, "ask.opened")
		requireTheAnswerOnTheNode(t, database, "the answered ask beside the reopen", answered.ID, "ask.answered")
	})

	t.Run("the approval carry", func(t *testing.T) {
		// A person's answer writes no version (R3), so the carry that answers an open approval
		// request here is the restore's: the agent's undo of its edit holds the approved text.
		handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
		issue := createInteractionIssue(t, handler, "TEST", "The carry's answer on the node", "A spec\n")
		reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
		reviseDocument(t, handler, issue.PrimaryArtifactID, "A spec", "A revised spec", 2)
		request := requestApproval(t, handler, issue.PrimaryArtifactID)
		reviseDocument(t, handler, issue.PrimaryArtifactID, "A revised spec", "A spec", 3)
		requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 3, 1, restoreSentence(1), &request.ID)
		requireTheAnswerOnTheNode(t, database, "the approval carry's answer", request.ID, "ask.answered")
	})

	t.Run("settlement's retraction and restoration", func(t *testing.T) {
		handler, database, documentService := blockAskHandlerWithDocuments(t)
		issue, askID := seedBlockAsk(t, handler, "A block ask retracted and restored", "decision", transportAsk, "Which transport?")
		removeBlockAsABrowserDoes(t, documentService, issue.PrimaryArtifactID, "decision")
		settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-delete")
		requireTheAnswerOnTheNode(t, database, "settlement's retraction", askID, "ask.resolved")
		if restored := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": transportAsk}},
		}, "alice"); restored.Code != http.StatusOK {
			t.Fatalf("restore the ask block: status=%d body=%s", restored.Code, restored.Body.String())
		}
		settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-restore")
		requireTheAnswerOnTheNode(t, database, "settlement's restoration", askID, "ask.opened")
	})
}

// heldValue is one value an ask's node holds now: the value, and the event its tuple carries.
type heldValue struct {
	value string
	event int64
}

// requireTheAnswerOnTheNode fails unless the node of askID holds the answer state its asks row
// holds, after the write what names: the row names its node; answer_state is the row's state and
// carries the newest event of eventType that names the ask; an answer the row holds is on the node
// as its words (absent for an answer of none), its selection and its time, with answered_by and
// decided_by at its person, each carrying that event where the write was an answer; no answer
// leaves none of them; and the resolution likewise.
func requireTheAnswerOnTheNode(t *testing.T, database *store.Store, what, askID, eventType string) {
	t.Helper()
	ctx := context.Background()
	var state string
	var answerJSON, resolutionJSON []byte
	var marked bool
	if err := database.Pool.QueryRow(ctx, `
		select state, answer, resolution, node_kind is not null from asks where id = $1::uuid
	`, askID).Scan(&state, &answerJSON, &resolutionJSON, &marked); err != nil {
		t.Fatalf("%s: read the row of ask %s: %v", what, askID, err)
	}
	if !marked {
		t.Fatalf("%s: the row of ask %s names no node", what, askID)
	}
	var event int64
	if err := database.Pool.QueryRow(ctx, `
		select coalesce(max(id), 0) from events where type = $1 and payload->>'id' = $2
	`, eventType, askID).Scan(&event); err != nil || event == 0 {
		t.Fatalf("%s: read the %s of ask %s: event %d, %v", what, eventType, askID, event, err)
	}
	values := map[string]heldValue{}
	rows, err := database.Pool.Query(ctx, `
		select kind, to_id, source_event from (
			select distinct on (kind) kind, to_id, source_event, asserted from graph_tuples
			where from_kind = $1 and from_id = $2 and to_kind = $3
			order by kind, seq desc) newest
		where asserted
	`, graphstore.KindAsk, askID, graphstore.KindValue)
	if err != nil {
		t.Fatalf("%s: read the values of ask %s: %v", what, askID, err)
	}
	for rows.Next() {
		var kind string
		var held heldValue
		if err := rows.Scan(&kind, &held.value, &held.event); err != nil {
			t.Fatalf("%s: read a value of ask %s: %v", what, askID, err)
		}
		values[kind] = held
	}
	rows.Close()
	relations, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindAsk, askID,
		[]string{graphstore.TupleAnsweredBy, graphstore.TupleDecidedBy})
	if err != nil {
		t.Fatalf("%s: read the relations of ask %s: %v", what, askID, err)
	}
	related := map[string][]string{}
	for _, relation := range relations {
		related[relation.Kind] = append(related[relation.Kind], relation.ToKind+"/"+relation.ToID)
	}
	t.Logf("%s: ask %s row state=%s answer=%s resolution=%s; node %v %v; %s %d",
		what, askID, state, answerJSON, resolutionJSON, values, related, eventType, event)

	if got := values[graphstore.TupleAnswerState]; got.value != state || got.event != event {
		t.Errorf("%s: the node's answer_state is %q under event %d, want the row's %q under the %s %d", what, got.value, got.event, state, eventType, event)
	}
	requireValue := func(kind, want string, cause bool) {
		t.Helper()
		got, held := values[kind]
		if !held || got.value != want {
			t.Errorf("%s: the node's %s is %q (held %t), want the row's %q", what, kind, got.value, held, want)
		}
		if cause && got.event != event {
			t.Errorf("%s: the node's %s carries event %d, want the %s %d that wrote it", what, kind, got.event, eventType, event)
		}
	}
	requireNone := func(kind string) {
		t.Helper()
		if got, held := values[kind]; held {
			t.Errorf("%s: the node holds %s %q, which the row holds none of", what, kind, got.value)
		}
	}
	answeredNow := eventType == "ask.answered"
	if len(answerJSON) > 0 && string(answerJSON) != "null" {
		var answer model.AskAnswer
		if err := json.Unmarshal(answerJSON, &answer); err != nil {
			t.Fatalf("%s: decode the answer of ask %s: %v", what, askID, err)
		}
		if answer.Text != nil {
			requireValue(graphstore.TupleAnswer, *answer.Text, answeredNow)
		} else {
			requireNone(graphstore.TupleAnswer)
		}
		requireValue(graphstore.TupleSelected, asks.SelectedValue(answer.Selected), answeredNow)
		requireValue(graphstore.TupleAnsweredAt, asks.AnsweredAtValue(answer.At), answeredNow)
		person := []string{graphstore.KindPerson + "/" + model.CanonicalLogin(answer.User)}
		for _, kind := range []string{graphstore.TupleAnsweredBy, graphstore.TupleDecidedBy} {
			if !slices.Equal(related[kind], person) {
				t.Errorf("%s: the node's %s is %v, want the answer's person %v", what, kind, related[kind], person)
			}
		}
	} else {
		for _, kind := range []string{graphstore.TupleAnswer, graphstore.TupleSelected, graphstore.TupleAnsweredAt} {
			requireNone(kind)
		}
		for _, kind := range []string{graphstore.TupleAnsweredBy, graphstore.TupleDecidedBy} {
			if len(related[kind]) > 0 {
				t.Errorf("%s: the node's %s is %v, and the row holds no answer", what, kind, related[kind])
			}
		}
	}
	if len(resolutionJSON) > 0 && string(resolutionJSON) != "null" {
		var resolution model.AskResolution
		if err := json.Unmarshal(resolutionJSON, &resolution); err != nil {
			t.Fatalf("%s: decode the resolution of ask %s: %v", what, askID, err)
		}
		want, err := asks.ResolutionValue(resolution)
		if err != nil {
			t.Fatal(err)
		}
		requireValue(graphstore.TupleResolution, want, eventType == "ask.resolved")
	} else {
		requireNone(graphstore.TupleResolution)
	}
}
