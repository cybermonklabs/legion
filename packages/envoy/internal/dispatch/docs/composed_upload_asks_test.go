package docs

import (
	"context"
	"errors"
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

// composedUploadSurroundedDocument is that same document with a paragraph below the ask as well,
// so an upload can change the block above the ask and the block below it at once.
const composedUploadSurroundedDocument = composedUploadDocument + "\nThe decision lands this week.\n"

// composedUploadReadOpener is the ask block's opener as an agent reads the document through the
// text routes (TextWithAsks), before the person answers and after: the block under its id, with
// none of the attributes the server keeps, since a person's answer is held once, on the ask, and
// read beside the text in its asks member (textAsks). composedUploadComposedOpener is the opener
// an agent composing the body again writes in its place: the question is the same, and the block
// names no id of its own. composedUploadAnsweredEntry is the asks member's entry for the block once
// the person has answered it, as askLines writes it.
const (
	composedUploadReadOpener     = `:::ask{#transport urgency="med" multiple="false"}`
	composedUploadComposedOpener = `:::ask{urgency="med" multiple="false"}`
	composedUploadAnsweredEntry  = `#transport state="answered" answered_by="alice" answered_at="2026-10-08T12:00:00Z" selected=["REST"]`
)

// composedUploadAnsweredAt is the moment the person's answer carries.
var composedUploadAnsweredAt = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// composedUploadAgent is the session that uploads the body it composed.
var composedUploadAgent = model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

// An agent that composes a document's body again writes each ask it holds from the question
// alone: no id, since a body written rather than rendered names none, and none of the attributes
// the server keeps, which the text it reads does not print. The upload leaves that ask where it
// stands, so the block keeps the id the document gave it, the row holding the person's answer
// stays the issue's one row for the question, and the document reads as it read before the
// upload: the block under its id, and the person's answer in the asks member of the text read.
func TestAnAgentsComposedUploadLeavesTheAnsweredAskOnItsBlockAndItsRow(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ask := composedUploadAnsweredAsk(t, service, artifactID, composedUploadDocument)
	read := asAgentReadsIt(t, service, artifactID)
	answered := composedUploadAgentMarkdown(t, service, artifactID)
	composed := strings.Replace(answered, composedUploadReadOpener, composedUploadComposedOpener, 1)
	if composed == answered {
		t.Fatalf("the answered document reads\n%s\nwant it to hold the opener an agent reads, %s, which the agent composes without its id", answered, composedUploadReadOpener)
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
	if after := asAgentReadsIt(t, service, artifactID); after != read {
		t.Errorf("the document after the upload and its settlement reads, as an agent reads it,\n%s\nwant the block under its id and the person's answer in its asks member, as it read before the upload\n%s", after, read)
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
// the block is not the one the person answered, so it takes a fresh id and opens an ask of its
// own, and the answered row stays as the record of the question it was given on. This is where
// the rule above stops. Written in place of the answered question, the other question would take
// the answered ask's block out of the document, which no writer but a person may do
// (ErrAnsweredAskKept): that body is refused and writes nothing, so the agent asks it beside the
// answered question, which it writes from the question alone as above.
func TestAnAgentsComposedUploadOfAnotherQuestionOpensAnAskOfItsOwn(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ask := composedUploadAnsweredAsk(t, service, artifactID, composedUploadDocument)
	read := asAgentReadsIt(t, service, artifactID)
	answered := composedUploadAgentMarkdown(t, service, artifactID)
	start := strings.Index(answered, composedUploadReadOpener)
	if start < 0 {
		t.Fatalf("the answered document reads\n%s\nwant it to hold the opener an agent reads, %s", answered, composedUploadReadOpener)
	}
	keptBlock := strings.Replace(answered, composedUploadReadOpener, composedUploadComposedOpener, 1)
	otherBlock := strings.Replace(strings.Replace(answered[start:], composedUploadReadOpener, composedUploadComposedOpener, 1),
		composedUploadQuestion, composedUploadOtherQuestion, 1)
	inPlace := strings.Replace(keptBlock, composedUploadQuestion, composedUploadOtherQuestion, 1)
	composed := keptBlock + "\n" + otherBlock
	for _, body := range []string{inPlace, composed} {
		if !strings.Contains(body, composedUploadComposedOpener) || !strings.Contains(body, composedUploadOtherQuestion) {
			t.Fatalf("the composed body reads\n%s\nwant the ask written with no id and asking %q", body, composedUploadOtherQuestion)
		}
	}

	var kept *ErrAnsweredAskKept
	if _, err := joinedReplaceText(service, artifactID, inPlace, composedUploadAgent); !errors.As(err, &kept) ||
		len(kept.Asks) != 1 || kept.Asks[0].Question != composedUploadQuestion {
		t.Fatalf("the upload of the body asking the other question in place of the answered one = %v, want it refused for the answered ask %s asking %q", err, ask.ID, composedUploadQuestion)
	}
	if after := asAgentReadsIt(t, service, artifactID); after != read {
		t.Fatalf("the document after the refused upload reads, as an agent reads it,\n%s\nwant it as it read before\n%s", after, read)
	}

	if _, err := joinedReplaceText(service, artifactID, composed, composedUploadAgent); err != nil {
		t.Fatalf("upload the body the agent composed with another question: %v", err)
	}
	ids := documentAskBlockIDs(t, service, artifactID)
	if len(ids) != 2 || ids[0] != *ask.BlockID || ids[1] == *ask.BlockID || ids[1] == "" {
		t.Fatalf("the document after the upload holds the ask blocks %q, want the answered ask's %q and then one block with an id of its own", ids, *ask.BlockID)
	}
	fresh := ids[1]

	settleCurrentGeneration(t, service, artifactID)
	opener := fmt.Sprintf(`:::ask{#%s urgency="med" multiple="false"}`+"\n"+composedUploadOtherQuestion+"\n", fresh)
	entries := fmt.Sprintf("\nasks:\n%s\n#%s state=\"open\"\n", composedUploadAnsweredEntry, fresh)
	if read := asAgentReadsIt(t, service, artifactID); !strings.Contains(read, opener) || !strings.HasSuffix(read, entries) {
		t.Fatalf("the document after the upload and its settlement reads, as an agent reads it,\n%s\nwant the other question under its own id, %q, and its asks member answering the answered ask and the other question open, %q", read, opener, entries)
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

// The shape the orphaned answer was first measured in is wider than the two cases above: the
// agent writes the question word for word, and rewrites the paragraph above the ask and the
// paragraph below it, and puts a heading, a block of another type, above the ask. The alignment
// the upload's write uses (pmdoc.carryChildren) pairs each uploaded child with a live child it
// leaves unchanged, wherever that child stands, before it pairs the changed ones by place. The
// uploaded ask is unchanged from the live one outside the server state an upload resets, so the
// two are paired and the id is carried, whatever the rewritten blocks around it pair with: a
// block of another type inserted above the ask does not cost the ask its id, and the heading and
// the two rewritten paragraphs take fresh ones. The row holding the person's answer stays the
// issue's one row for the question, and the text read answers that answer for the block in its
// asks member.
func TestAnAgentsComposedUploadKeepsTheAnsweredAskWhenItsNeighboursChange(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ask := composedUploadAnsweredAsk(t, service, artifactID, composedUploadSurroundedDocument)
	answered := composedUploadAgentMarkdown(t, service, artifactID)
	composed := strings.Replace(answered, composedUploadReadOpener, "## Transport\n\n"+composedUploadComposedOpener, 1)
	composed = strings.Replace(composed, "Context\n", "Context, revised.\n", 1)
	composed = strings.Replace(composed, "The decision lands this week.\n", "The decision lands on Friday.\n", 1)
	neighbourhood := []string{"Context, revised.\n", "## Transport\n", "The decision lands on Friday.\n"}
	for _, want := range append([]string{composedUploadComposedOpener}, neighbourhood...) {
		if !strings.Contains(composed, want) {
			t.Fatalf("the composed body reads\n%s\nwant it to hold %q", composed, want)
		}
	}

	if _, err := joinedReplaceText(service, artifactID, composed, composedUploadAgent); err != nil {
		t.Fatalf("upload the body the agent composed around the ask: %v", err)
	}
	if ids := documentAskBlockIDs(t, service, artifactID); len(ids) != 1 || ids[0] != *ask.BlockID {
		t.Errorf("the document after the upload holds the ask blocks %q, want the one block %q the answered ask was given on, which the upload leaves unchanged outside its server state", ids, *ask.BlockID)
	}

	settleCurrentGeneration(t, service, artifactID)
	read := asAgentReadsIt(t, service, artifactID)
	text, _, _ := strings.Cut(read, "\nasks:\n")
	if !strings.Contains(text, composedUploadReadOpener) || !strings.HasSuffix(read, "\nasks:\n"+composedUploadAnsweredEntry+"\n") {
		t.Errorf("the document after the upload and its settlement reads, as an agent reads it,\n%s\nwant the block under its id, %s, and the person's answer in its asks member, %s", read, composedUploadReadOpener, composedUploadAnsweredEntry)
	}
	for _, want := range neighbourhood {
		if !strings.Contains(text, want) {
			t.Errorf("the document after the upload and its settlement reads\n%s\nwant the neighbourhood the agent composed, holding %q", text, want)
		}
	}
	rows := askRowsOn(t, service, artifactID)
	if len(rows) != 1 {
		t.Fatalf("the issue holds %d ask rows after the upload, want the one row %s the person answered: %s", len(rows), ask.ID, askRowSummaries(rows))
	}
	row := rows[0]
	if row.ID != ask.ID || row.Question != composedUploadQuestion {
		t.Fatalf("the issue's one ask row after the upload is %s, want the answered row %s on %q", askRowSummary(row), ask.ID, composedUploadQuestion)
	}
	if row.BlockID == nil || *row.BlockID != *ask.BlockID {
		t.Fatalf("the ask row after the upload is on block %s, want the block %q it was answered on: %s", blockOf(row), *ask.BlockID, askRowSummary(row))
	}
	if row.State != "answered" || row.Answer == nil || row.Answer.User != "alice" || !slices.Equal(row.Answer.Selected, []string{"REST"}) {
		t.Fatalf("the ask row after the upload is %s, want it still answered with alice's answer selecting REST", askRowSummary(row))
	}
}

// composedUploadAnsweredAsk seeds document on artifactID, lets the settlement index its ask,
// answers that ask as alice the way its route stores an answer, and lets the settlement run over
// the answered ask. It returns the answered row, once the text read answers the person's answer
// for the block in its asks member, which is where an agent reads it.
func composedUploadAnsweredAsk(t *testing.T, service *Service, artifactID, document string) model.Ask {
	t.Helper()
	seedServiceText(t, service, artifactID, document)
	settleCurrentGeneration(t, service, artifactID)
	ask := blockAskOn(t, service, artifactID, "transport")
	if ask.State != "open" || ask.Question != composedUploadQuestion {
		t.Fatalf("the seeded ask is %s, want it open on %q before the person answers", askRowSummary(ask), composedUploadQuestion)
	}
	answerAskOnItsNode(t, service, ask.ID, model.AskAnswer{User: "alice", Selected: []string{"REST"}, At: composedUploadAnsweredAt})
	settleCurrentGeneration(t, service, artifactID)
	answered := blockAskOn(t, service, artifactID, "transport")
	if answered.State != "answered" || answered.Answer == nil {
		t.Fatalf("the ask the person answered is %s, want it answered before the upload", askRowSummary(answered))
	}
	if read := asAgentReadsIt(t, service, artifactID); !strings.HasSuffix(read, "\nasks:\n"+composedUploadAnsweredEntry+"\n") {
		t.Fatalf("the answered document reads, as an agent reads it,\n%s\nwant its asks member to answer the block as alice answered it, %s", read, composedUploadAnsweredEntry)
	}
	return answered
}

// composedUploadAgentMarkdown is the markdown an agent reads of the document through the text
// routes (TextWithAsks), which it composes its body from.
func composedUploadAgentMarkdown(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	text, err := service.TextWithAsks(context.Background(), artifactID, false)
	if err != nil {
		t.Fatalf("read the document as an agent reads it: %v", err)
	}
	return text.Markdown
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
