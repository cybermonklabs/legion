package docs

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// namedPartiesText is an answer's text that names an agent and a role, as the dashboard writes
// their mentions.
const namedPartiesText = "REST first. [@Planner](dispatch://agent/session-planner) and [@Reviewer](dispatch://role/reviewer) take it."

// answeredEvent is one ask.answered event of an ask as the event log holds it: whether it is a
// restore, the answer's text, and the parties it records (model.AskAnsweredEventPayload's
// named_targets), present false where the payload carries no such field.
type answeredEvent struct {
	restored bool
	text     string
	targets  []string
	present  bool
}

// answeredEvents is every ask.answered event of askID, oldest first.
func answeredEvents(t *testing.T, database *store.Store, askID string) []answeredEvent {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select coalesce((payload->>'restored')::boolean, false), coalesce(payload->'answer'->>'text', ''),
		       payload->'named_targets'
		from events where type = 'ask.answered' and payload->>'id' = $1 order by id
	`, askID)
	if err != nil {
		t.Fatalf("read the ask.answered events of ask %s: %v", askID, err)
	}
	defer rows.Close()
	var found []answeredEvent
	for rows.Next() {
		var event answeredEvent
		var raw []byte
		if err := rows.Scan(&event.restored, &event.text, &raw); err != nil {
			t.Fatalf("scan an ask.answered event of ask %s: %v", askID, err)
		}
		if raw != nil {
			event.present = true
			if err := json.Unmarshal(raw, &event.targets); err != nil {
				t.Fatalf("decode named_targets %s: %v", raw, err)
			}
		}
		found = append(found, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the ask.answered events of ask %s: %v", askID, err)
	}
	return found
}

// R14: a person's edit that brings back the block of an answered ask a settlement retracted
// restores the answer, and the ask.answered that restore appends (restoreRetractedAsk, marked
// restored) records no party: the answer route recorded the agent and the role its text names
// with the answer, which the outbox sent it to then, and the restore tells no party again. The
// answer is written as the answer route writes it, its event recording those parties. Settlement
// leaves an answered row as it is when its block leaves, so the retraction is the one a release
// before that rule wrote, whose answer survives only in events, seeded as api's
// TestListMyAnswersCollapsesRestoredAnswerEvents seeds it.
func TestARestoredAnswerOfADecisionBlockTellsNoNamedPartyAgain(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const askMarkdown = ":::ask{#ask-block multiple=\"false\" state=\"open\" urgency=\"high\"}\nWhich transport should we expose?\n\n- REST: Matches the existing platform\n- gRPC: Adds streaming\n:::\n"
	seedServiceText(t, service, artifactID, askMarkdown)
	service.settleRoom(artifactID, 0)
	ctx := context.Background()
	var askID string
	var issueKey, ownerArtifact *string
	if err := service.store.Pool.QueryRow(ctx, `
		select id::text, issue_key, artifact_id::text from asks where block_artifact_id = $1
	`, artifactID).Scan(&askID, &issueKey, &ownerArtifact); err != nil {
		t.Fatalf("read the indexed ask: %v", err)
	}

	text := namedPartiesText
	answer := model.AskAnswer{User: "alice", Selected: []string{"REST"}, Text: &text, At: time.Now().UTC()}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode the answer: %v", err)
	}
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the answer: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, askID, encoded); err != nil {
		t.Fatalf("answer the ask: %v", err)
	}
	answered := model.Ask{ID: askID, IssueKey: issueKey, ArtifactID: ownerArtifact, State: "answered", Answer: &answer}
	if _, err := service.events.Append(ctx, tx, model.Event{
		IssueKey: issueKey, ArtifactID: ownerArtifact, Type: "ask.answered", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.AskAnsweredEventPayload{
			AskEventPayload: model.NewAskEventPayload(answered, model.ReferenceChanges{}),
			NamedTargets:    []string{"session:session-planner", "role:reviewer"},
		},
	}); err != nil {
		t.Fatalf("append the answer's event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the answer: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)

	editLiveTree(t, service, artifactID, func(*pmdoc.Node) *pmdoc.Node {
		return &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{
			Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "replacement"},
			Children: []*pmdoc.Node{{Type: "text", Text: "No decision remains."}},
		}}}
	})
	settleCurrentGeneration(t, service, artifactID)
	var state string
	if err := service.store.Pool.QueryRow(ctx, `select state from asks where id = $1`, askID).Scan(&state); err != nil {
		t.Fatalf("read the ask whose block left: %v", err)
	}
	if state != "answered" {
		t.Fatalf("the answered ask whose block left reads state %q, want answered", state)
	}
	if _, err := service.store.Pool.Exec(ctx, `
		update asks
		set state = 'resolved', answer = null,
		    resolution = jsonb_build_object(
		      'kind', 'retracted',
		      'reason', 'removed from the document in version 3',
		      'actor', jsonb_build_object('kind', 'system', 'id', 'document-settlement'),
		      'at', now()
		    )
		where id = $1
	`, askID); err != nil {
		t.Fatalf("seed the earlier release's retraction: %v", err)
	}

	editLiveTree(t, service, artifactID, func(*pmdoc.Node) *pmdoc.Node {
		restored, err := pmdoc.Parse(askMarkdown)
		if err != nil {
			t.Fatalf("parse the restored block: %v", err)
		}
		return restored
	})
	settleCurrentGeneration(t, service, artifactID)
	if err := service.store.Pool.QueryRow(ctx, `select state from asks where id = $1`, askID).Scan(&state); err != nil {
		t.Fatalf("read the restored ask: %v", err)
	}
	if state != "answered" {
		t.Fatalf("the restored ask reads state %q, want answered", state)
	}
	found := answeredEvents(t, service.store, askID)
	if len(found) != 2 {
		t.Fatalf("the ask holds %d ask.answered events %+v, want the answer and its restore", len(found), found)
	}
	if first := found[0]; first.restored || !slices.Equal(first.targets, []string{"session:session-planner", "role:reviewer"}) {
		t.Fatalf("the answer's event = %+v, want the parties its text names recorded", first)
	}
	if restore := found[1]; !restore.restored || restore.text != namedPartiesText || restore.present {
		t.Fatalf("the restore's event = %+v, want a restore of the same text that records no party", restore)
	}
}

// R14: a carried approval answers the open approval request in its reviewer's name
// (answerApprovalAsk), with the carry's reason as the answer's text, and records no party that
// text names: only the answer route reads the parties of an answer. The reason here names an agent
// and a role, which no reason the carry writes does, so the row holds for any text.
func TestACarriedApprovalTellsNoNamedParty(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# Spec\n\nShip it.\n")
	askID := openStoredApprovalAsk(t, database, artifactID, 1)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	queued, ledger := queueWrites(ctx, tx)
	open, err := OpenApprovalAsk(queued, tx, artifactID)
	if err != nil || open == nil || open.ID != askID {
		t.Fatalf("read the open request = %+v, %v; want ask %s", open, err, askID)
	}
	if err := answerApprovalAsk(queued, tx, events.NewBroker(), artifactID, open, model.Actor{Kind: "user", ID: "alice"},
		namedPartiesText, "https://dispatch.example", func(model.Event) {}); err != nil {
		t.Fatalf("carry the approval: %v", err)
	}
	if err := ledger.runQueue(ctx, nil); err != nil {
		t.Fatalf("write the carry: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the carry: %v", err)
	}
	found := answeredEvents(t, database, askID)
	if len(found) != 1 || !strings.Contains(found[0].text, "dispatch://agent/session-planner") || found[0].present {
		t.Fatalf("the carry's ask.answered events = %+v, want one whose text names the planner and that records no party", found)
	}
}
