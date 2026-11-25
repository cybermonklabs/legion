package docs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A copy's `#id` resolves to its source through its block node's copied_from tuple, which
// settlement states (claimCopies), whatever the latest version's opening line holds: an upload
// resets a typed block's server-owned attributes, so the version it writes can hold the copy with
// no copied_from until the settlement after it writes the provenance back (keptAskState), and the
// copy still resolves to its source meanwhile. A version the release before wrote, whose copy no
// settlement of this build has claimed, resolves through its line instead
// (refs' TestQuestionMapReadsTheLatestVersionsQuestionBlocks).
func TestACopysIDResolvesThroughItsNodesCopiedFromWhateverItsLineHolds(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, original, "Context\n\n"+shipBlock)
	settleCurrentGeneration(t, service, original)
	source := blockAskOn(t, service, original, "ship")
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, "Record\n\n"+shipBlock)
	settleCurrentGeneration(t, service, copied)
	provenance := " " + copiedFrom(t, service, source.ID, original)
	latest := latestVersionMarkdown(t, service, copied)
	if !strings.Contains(latest, provenance) {
		t.Fatalf("the copy's settled version reads:\n%s\nwant its opener naming its source,%s", latest, provenance)
	}
	// The version an upload of the copy writes before the settlement after it: the copy's opener
	// holds no provenance.
	number := latestVersionNumber(t, service, copied) + 1
	if _, err := service.store.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, $2, $3, '[{"kind":"session","id":"session-0123456789abcdef"}]')
	`, copied, number, strings.Replace(latest, provenance, "", 1)); err != nil {
		t.Fatalf("write the upload's version of the copy: %v", err)
	}
	if stored := latestVersionMarkdown(t, service, copied); strings.Contains(stored, "copied_from") {
		t.Fatalf("the upload's version reads:\n%s\nwant no copied_from on the copy's opener", stored)
	}
	questions, err := refs.QuestionMap(ctx, service.store.Pool, model.Actor{}, copied)
	if err != nil {
		t.Fatalf("read the copy's question map: %v", err)
	}
	if want := text.Extract("dispatch://DOC-1/ask/"+source.ID, "")[0]; questions["ship"] != want {
		t.Fatalf("the copy's question map = %#v; want ship resolving to its source %#v through its node's copied_from", questions, want)
	}
}

// The text routes' asks member reads each ask's answer off the ask's node, not its row (R3: every
// read of an answer reads the node), as the Decisions read's entries do: while the row holds an
// answer a process of the release before wrote into it alone, before the decisions step's catch-up
// states it on the node, the text reads the ask open, as its node holds it, with no answerer; once
// the node holds an answer, the text reads that one.
func TestTheTextsAsksReadTheAnswerOffTheAsksNodeNotItsRow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "Context\n\n"+shipBlock)
	settleCurrentGeneration(t, service, artifactID)
	ask := blockAskOn(t, service, artifactID, "ship")
	rowAnswer, err := json.Marshal(model.AskAnswer{User: "bob", Selected: []string{"Yes"}, At: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("encode the row's answer: %v", err)
	}
	if _, err := service.store.Pool.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, ask.ID, rowAnswer); err != nil {
		t.Fatalf("answer the ask's row alone, as the release before writes it: %v", err)
	}
	read, err := service.TextWithAsks(ctx, artifactID, false)
	if err != nil {
		t.Fatalf("read the text: %v", err)
	}
	if len(read.Asks) != 1 || read.Asks[0].State != asks.StateOpen || read.Asks[0].AnsweredBy != "" {
		t.Fatalf("the text's asks with the row answered by bob and the node open = %s, want ship open with no answerer, as its node holds it", askLines(read.Asks))
	}

	answerAskOnItsNode(t, service, ask.ID, model.AskAnswer{User: "alice", Selected: []string{"No"}, At: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)})
	if read, err = service.TextWithAsks(ctx, artifactID, false); err != nil {
		t.Fatalf("read the text: %v", err)
	}
	const answered = `#ship state="answered" answered_by="alice" answered_at="2026-11-20T10:00:00Z" selected=["No"]` + "\n"
	if got := askLines(read.Asks); got != answered {
		t.Fatalf("the text's asks once the node holds alice's answer = %s, want %s", got, answered)
	}
}

// The text routes' asks member decodes the values of each ask's node through the reader every read
// of an answer shares (asks.NodeAnswer), so an answer_state outside open, answered and resolved
// refuses the read as it refuses an ask read and drops the Decisions read's entry, where the text
// once served the state it found.
func TestTheTextsAsksRefuseAnAnswerStateTheNodeHoldsOutsideTheThree(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "Context\n\n"+shipBlock)
	settleCurrentGeneration(t, service, artifactID)
	ask := blockAskOn(t, service, artifactID, "ship")
	if _, err := service.store.Pool.Exec(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		values ('ask', $1, 'answer_state', 'value', 'half-answered', now(), '{"kind":"user","id":"alice"}'::jsonb, 0, true)
	`, ask.ID); err != nil {
		t.Fatalf("state an answer_state of none of the three on the ask's node: %v", err)
	}
	if read, err := service.TextWithAsks(ctx, artifactID, false); !errors.Is(err, asks.ErrNodeAnswerUnreadable) {
		t.Fatalf("the text of a document whose ask's node holds the answer state half-answered = %s, %v, want ErrNodeAnswerUnreadable", askLines(read.Asks), err)
	}
}
