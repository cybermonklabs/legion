package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// composedUploadQuestion is the question the document asks, and composedUploadOtherQuestion is
// another one, which only the control case below writes.
const (
	composedUploadQuestion      = "Which transport should we expose?"
	composedUploadOtherQuestion = "Which transport should we drop?"
)

// composedUploadDocument is the document a person answers an ask on: one paragraph and one ask
// block, whose id is the document's own.
const composedUploadDocument = "Context\n\n" +
	":::ask{#transport urgency=\"med\" multiple=\"false\"}\n" + composedUploadQuestion + "\n\n- REST: Matches the platform.\n- gRPC: Streams.\n:::\n"

// composedUploadAnsweredOpener is the ask block's opener once the settlement has written the
// person's answer onto it (setAskServerAttributes), and composedUploadComposedOpener is the
// opener an agent composing the body again writes in its place: the question is the same, and the
// block names neither an id of its own nor any attribute the server keeps.
const (
	composedUploadAnsweredOpener = `:::ask{#transport urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-08T12:00:00Z" selected="[&#x22;REST&#x22;]"}`
	composedUploadComposedOpener = `:::ask{urgency="med" multiple="false"}`
)

// composedUploadAnsweredAt is the moment the person's answer carries.
var composedUploadAnsweredAt = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// composedUploadAgent is the session that uploads the body it composed.
var composedUploadAgent = model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

// An agent that composes a document's body again writes each ask it holds from the question
// alone: no id, since a body written rather than rendered names none, and none of the attributes
// the server keeps, since the agent cannot know the answer a person gave. The upload leaves that
// ask where it stands, so the block keeps the id the document gave it, the row holding the
// person's answer stays the issue's one row for the question, and the settlement writes the
// answer onto the block again.
func TestAnAgentsComposedUploadLeavesTheAnsweredAskOnItsBlockAndItsRow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ask := composedUploadAnsweredAsk(t, service, artifactID)
	answered := documentText(t, service, artifactID)
	composed := strings.Replace(answered, composedUploadAnsweredOpener, composedUploadComposedOpener, 1)
	if composed == answered {
		t.Fatalf("the answered document reads\n%s\nwant it to hold the opener the settlement writes, %s, which the agent composes without", answered, composedUploadAnsweredOpener)
	}

	if _, err := joinedReplaceText(service, artifactID, composed, composedUploadAgent); err != nil {
		t.Fatalf("upload the body the agent composed: %v", err)
	}
	// Each of the three measurements is reported, so a run against a server that gives the block
	// a fresh id names that id, the document it left and the rows it opened, rather than the
	// first of them alone.
	if ids := documentAskBlockIDs(t, service, artifactID); len(ids) != 1 || ids[0] != *ask.BlockID {
		t.Errorf("the document after the upload holds the ask blocks %q, want the one block %q the answered ask was given on", ids, *ask.BlockID)
	}

	settleCurrentGeneration(t, service, artifactID)
	if text := documentText(t, service, artifactID); text != answered {
		t.Errorf("the document after the upload and its settlement reads\n%s\nwant the answer written onto the block again, as it read before the upload\n%s", text, answered)
	}
	rows := askRowsOn(t, service, artifactID)
	if len(rows) != 1 {
		t.Fatalf("the issue holds %d ask rows after the upload, want the one row %s the person answered: %s", len(rows), ask.ID, askRowSummaries(rows))
	}
	row := rows[0]
	if row.ID != ask.ID {
		t.Fatalf("the issue's one ask row after the upload is %s, want the answered row %s: %s", row.ID, ask.ID, askRowSummary(row))
	}
	if row.Question != composedUploadQuestion {
		t.Fatalf("the ask row after the upload asks %q, want the question the agent composed again, %q", row.Question, composedUploadQuestion)
	}
	if row.BlockID == nil || *row.BlockID != *ask.BlockID {
		t.Fatalf("the ask row after the upload is on block %s, want the block %q it was answered on: %s", blockOf(row), *ask.BlockID, askRowSummary(row))
	}
	if row.State != "answered" {
		t.Fatalf("the ask row after the upload is %s, want it still answered: %s", row.State, askRowSummary(row))
	}
	if row.Answer == nil || row.Answer.User != "alice" || !slices.Equal(row.Answer.Selected, []string{"REST"}) {
		t.Fatalf("the ask row after the upload carries %s, want alice's answer selecting REST", answerOf(row))
	}
}

// An agent that composes the body with another question writes an ask the document does not hold:
// the block is no longer the one the person answered, so it takes a fresh id and opens an ask of
// its own, and the answered row stays as the record of the question it was given on. This is
// where the rule above stops.
func TestAnAgentsComposedUploadOfAnotherQuestionOpensAnAskOfItsOwn(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ask := composedUploadAnsweredAsk(t, service, artifactID)
	answered := documentText(t, service, artifactID)
	composed := strings.Replace(answered, composedUploadAnsweredOpener, composedUploadComposedOpener, 1)
	composed = strings.Replace(composed, composedUploadQuestion, composedUploadOtherQuestion, 1)
	if !strings.Contains(composed, composedUploadComposedOpener) || !strings.Contains(composed, composedUploadOtherQuestion) {
		t.Fatalf("the composed body reads\n%s\nwant the ask written with no id and the question changed to %q", composed, composedUploadOtherQuestion)
	}

	if _, err := joinedReplaceText(service, artifactID, composed, composedUploadAgent); err != nil {
		t.Fatalf("upload the body the agent composed with another question: %v", err)
	}
	ids := documentAskBlockIDs(t, service, artifactID)
	if len(ids) != 1 || ids[0] == *ask.BlockID {
		t.Fatalf("the document after the upload holds the ask blocks %q, want one block with an id of its own, not the answered ask's %q", ids, *ask.BlockID)
	}
	fresh := ids[0]

	settleCurrentGeneration(t, service, artifactID)
	opener := fmt.Sprintf(`:::ask{#%s urgency="med" multiple="false" state="open"}`, fresh)
	if text := documentText(t, service, artifactID); !strings.Contains(text, opener) {
		t.Fatalf("the document after the upload and its settlement reads\n%s\nwant the other question open under its own id, %s", text, opener)
	}
	rows := askRowsOn(t, service, artifactID)
	if len(rows) != 2 {
		t.Fatalf("the issue holds %d ask rows after the upload of another question, want the answered row and the new question's: %s", len(rows), askRowSummaries(rows))
	}
	answeredRow, openRow := rows[0], rows[1]
	if answeredRow.ID != ask.ID {
		answeredRow, openRow = openRow, answeredRow
	}
	if answeredRow.ID != ask.ID || answeredRow.State != "answered" || answeredRow.Question != composedUploadQuestion {
		t.Fatalf("the answered row after the upload is %s, want the row %s still answered on %q", askRowSummary(answeredRow), ask.ID, composedUploadQuestion)
	}
	if openRow.State != "open" || openRow.Question != composedUploadOtherQuestion {
		t.Fatalf("the row the upload opened is %s, want it open on %q", askRowSummary(openRow), composedUploadOtherQuestion)
	}
	if openRow.BlockID == nil || *openRow.BlockID != fresh {
		t.Fatalf("the row the upload opened is on block %s, want the fresh block %q the other question took", blockOf(openRow), fresh)
	}
}

// composedUploadAnsweredAsk seeds composedUploadDocument on artifactID, lets the settlement index
// its ask, answers that ask as alice the way its route stores an answer, and lets the settlement
// write the answer onto the block. It returns the answered row.
func composedUploadAnsweredAsk(t *testing.T, service *Service, artifactID string) model.Ask {
	t.Helper()
	seedServiceText(t, service, artifactID, composedUploadDocument)
	settleCurrentGeneration(t, service, artifactID)
	ask := blockAskOn(t, service, artifactID, "transport")
	if ask.State != "open" || ask.Question != composedUploadQuestion {
		t.Fatalf("the seeded ask is %s, want it open on %q before the person answers", askRowSummary(ask), composedUploadQuestion)
	}
	answer, err := json.Marshal(model.AskAnswer{User: "alice", Selected: []string{"REST"}, At: composedUploadAnsweredAt})
	if err != nil {
		t.Fatalf("encode the person's answer: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(),
		`update asks set state = 'answered', answer = $2 where id = $1`, ask.ID, answer); err != nil {
		t.Fatalf("answer the ask as the person: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	answered := blockAskOn(t, service, artifactID, "transport")
	if answered.State != "answered" || answered.Answer == nil {
		t.Fatalf("the ask the person answered is %s, want it answered before the upload", askRowSummary(answered))
	}
	return answered
}

// documentAskBlockIDs is the id of every ask block the live document holds, in document order.
func documentAskBlockIDs(t *testing.T, service *Service, artifactID string) []string {
	t.Helper()
	ids := []string{}
	pmdoc.Walk(liveTree(t, service, artifactID), func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			ids = append(ids, id)
		}
		return true
	})
	return ids
}

// askRowsOn is every ask row of the document, oldest first.
func askRowsOn(t *testing.T, service *Service, artifactID string) []model.Ask {
	t.Helper()
	rows, err := service.store.Pool.Query(context.Background(),
		`select `+AskColumns+` from asks a where a.block_artifact_id = $1 order by a.created_at, a.id`, artifactID)
	if err != nil {
		t.Fatalf("read the document's asks: %v", err)
	}
	defer rows.Close()
	var asks []model.Ask
	for rows.Next() {
		ask, err := ScanAsk(rows)
		if err != nil {
			t.Fatalf("scan one of the document's asks: %v", err)
		}
		asks = append(asks, ask)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the document's asks: %v", err)
	}
	return asks
}

// askRowSummary names an ask row in a failure message: what it is, where it is and what it holds.
func askRowSummary(ask model.Ask) string {
	return fmt.Sprintf("row %s on block %s asking %q, %s, carrying %s", ask.ID, blockOf(ask), ask.Question, ask.State, answerOf(ask))
}

// askRowSummaries names every row of a set in a failure message.
func askRowSummaries(asks []model.Ask) string {
	summaries := make([]string, len(asks))
	for index, ask := range asks {
		summaries[index] = askRowSummary(ask)
	}
	return strings.Join(summaries, "; ")
}

func blockOf(ask model.Ask) string {
	if ask.BlockID == nil {
		return "none"
	}
	return *ask.BlockID
}

func answerOf(ask model.Ask) string {
	if ask.Answer == nil {
		return "no answer"
	}
	return fmt.Sprintf("%s's answer selecting %v", ask.Answer.User, ask.Answer.Selected)
}
