package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A document of the issue whose stored history does not load is one nothing can read or open, so
// it shows nobody an ask block open, and it stays unloadable however often a write meets it: the
// close of its issue goes on without it and withdraws the plain ask beside it, naming the closer,
// the reopen restores that ask, and the repair passes over the issue, withdrawing for a close an
// older release made and restoring for its reopen, succeed rather than fail on that document for
// good.
func TestClosingAnIssueWithAnUnloadableDocumentWithdrawsItsAsksAndReopeningRestoresThem(t *testing.T) {
	t.Parallel()
	// Past the run (pastTheRun), so no settlement timer loads the unloadable document between the
	// requests.
	f := newRepairFixtureSettlingAfter(t, pastTheRun)
	issue := createInteractionIssue(t, f.handler, "TEST", "Closing beside an unloadable document", "Context\n")
	uploaded := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "notes.md", "content": "Notes\n",
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the notes: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	plainAsk := createSessionAsk(t, f.handler, issue.Key, "Which region?")

	// The notes are a fragment of the issue's room, so the history that does not load is the
	// room's: every stored update of it, the upload's among them.
	if corrupted := makeDocumentUnloadable(t, f, notes); corrupted < 1 {
		t.Fatalf("the notes' room holds %d stored updates, want the upload's among them", corrupted)
	}

	closed := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close beside the unloadable notes: status=%d body=%s", closed.Code, closed.Body.String())
	}
	requireWithdrawn(t, f.handler, issue.Key, plainAsk, model.Actor{Kind: "user", ID: "alice", Proven: true})
	reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice")
	if reopened.Code != http.StatusOK {
		t.Fatalf("reopen beside the unloadable notes: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if got := readBlockAsk(t, f.handler, plainAsk); got.State != "open" || got.Resolution != nil {
		t.Fatalf("ask %s after the reopen = %s %#v, want open with no record", plainAsk, got.State, got.Resolution)
	}

	closer := repairActor("session-older-closer")
	closeWithoutWithdrawing(t, f, issue.Key, &closer, time.Now())
	if repair := repairNow(t, f); repair.Withdrawn != 1 {
		t.Fatalf("repair of the closed issue = %+v, want its plain ask withdrawn", repair)
	}
	requireWithdrawn(t, f.handler, issue.Key, plainAsk, closer)
	reopenWithoutRestoring(t, f, issue.Key, "todo", repairActor("session-older-reopener"))
	if repair := repairNow(t, f); repair.Restored != 1 {
		t.Fatalf("repair of the reopened issue = %+v, want its plain ask restored", repair)
	}
	if got := readBlockAsk(t, f.handler, plainAsk); got.State != "open" || got.Resolution != nil {
		t.Fatalf("ask %s after the repair's restoration = %s %#v, want open with no record", plainAsk, got.State, got.Resolution)
	}
}

// A document the close of its issue left as it was, because its stored history does not load, is
// rebuilt from its latest version, saved while the issue was open and showing its ask block
// open, with no state on it, as a version keeps an open ask (docs' keptAskState). The rebuild
// writes the close's states into the rebuilt document in its own transaction, so once it commits
// the block reads resolved, as the withdrawn ask's row does, rather than open on a closed issue
// until a reopen. The reopen then opens the block again with its ask, taking its state off. The
// test reads the live tree, and the text an agent reads, which answers each state in its asks
// member.
func TestRebuildingAClosedIssuesUnloadableDocumentShowsItsWithdrawnDecisionBlockResolved(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	issue := createInteractionIssue(t, f.handler, "TEST", "Rebuilding a closed issue's document", "Context\n")
	uploaded := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "notes.md",
		"content": "Notes\n\n:::ask{#rebuilt-decision urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n",
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the notes: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	awaitIndexedAskBlock(t, f.handler, notes, "rebuilt-decision", "Ship it?")
	blockAsk := requireBlockAsk(t, f.handler, issue.Key, notes, "rebuilt-decision")
	open := `#rebuilt-decision urgency="med" multiple="false"}`
	awaitVersionText(t, f.handler, notes, "the open decision in the latest version", contains(open))
	awaitSettled(t, f.database, notes)

	makeDocumentUnloadable(t, f, notes)

	if closed := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close beside the unloadable notes: status=%d body=%s", closed.Code, closed.Body.String())
	}
	requireWithdrawn(t, f.handler, issue.Key, blockAsk, model.Actor{Kind: "user", ID: "alice", Proven: true})

	rebuilt := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+notes+"/rebuild", map[string]any{}, "alice")
	if rebuilt.Code != http.StatusOK {
		t.Fatalf("rebuild the notes: status=%d body=%s", rebuilt.Code, rebuilt.Body.String())
	}
	live := liveTree(t, f.documents, notes)
	if !strings.Contains(live, `#rebuilt-decision urgency="med" multiple="false" state="resolved"`) {
		t.Fatalf("the rebuilt notes' live tree:\n%s\nwant the withdrawn ask block resolved", live)
	}
	if strings.Contains(live, open) {
		t.Fatalf("the rebuilt notes' live tree:\n%s\nshows the withdrawn ask block open on a closed issue", live)
	}
	requireLiveText(t, f.handler, notes, `#rebuilt-decision state="resolved"`)
	requireWithdrawn(t, f.handler, issue.Key, blockAsk, model.Actor{Kind: "user", ID: "alice", Proven: true})

	if reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if got := readBlockAsk(t, f.handler, blockAsk); got.State != "open" || got.Resolution != nil {
		t.Fatalf("ask %s after the reopen = %s %#v, want open with no record", blockAsk, got.State, got.Resolution)
	}
	requireLiveTree(t, f.documents, notes, open)
	requireLiveText(t, f.handler, notes, `#rebuilt-decision state="open"`)
}

// Rebuilding a closed issue's document writes the close's final sweep into the rebuilt document as
// the close itself writes it (docs' readIssueAskStateWrites). The document holds an ask block of
// its own, which the close withdrew, and a copy of the spec's decision, which alice answered. Each
// block keeps what a version keeps for the release before's readers (docs' keptAskState), live and
// in the version its settlement writes: the withdrawn block its resolved state, and the copy who
// answered and when and its source, with none of the answer's words; the asks member of the text
// an agent reads still holds both blocks' states from the store.
func TestRebuildingAClosedIssuesDocumentWritesTheAskAttributesAVersionKeeps(t *testing.T) {
	f := newRepairFixture(t)
	issue := createInteractionIssue(t, f.handler, "TEST", "Rebuilding a closed issue's answered copy", closeDecision)
	awaitIndexedAskBlock(t, f.handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	source := requireBlockAsk(t, f.handler, issue.Key, issue.PrimaryArtifactID, "close-decision")
	answerDecisionBlock(t, f.handler, source, "alice", []string{"Yes"}, "Ship it this week.", nil)
	uploaded := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "notes.md",
		"content": "Notes\n\n:::ask{#rebuilt-decision urgency=\"med\" multiple=\"false\"}\nShip the notes?\n\n- Yes: Now.\n- No: Later.\n:::\n\n" +
			strings.TrimPrefix(closeDecision, "Context\n\n"),
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the notes: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	awaitIndexedAskBlock(t, f.handler, notes, "rebuilt-decision", "Ship the notes?")
	own := requireBlockAsk(t, f.handler, issue.Key, notes, "rebuilt-decision")
	awaitTextShows(t, f.handler, notes, "the notes' copy of the answered decision", contains(`copied_from="`+source+`"`), nil)
	awaitSettled(t, f.database, notes)

	makeDocumentUnloadable(t, f, notes)
	if closed := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close beside the unloadable notes: status=%d body=%s", closed.Code, closed.Body.String())
	}
	requireWithdrawn(t, f.handler, issue.Key, own, model.Actor{Kind: "user", ID: "alice", Proven: true})
	if rebuilt := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+notes+"/rebuild", map[string]any{}, "alice"); rebuilt.Code != http.StatusOK {
		t.Fatalf("rebuild the notes: status=%d body=%s", rebuilt.Code, rebuilt.Body.String())
	}

	live := liveTree(t, f.documents, notes)
	ownRow, sourceRow := readBlockAsk(t, f.handler, own), readBlockAsk(t, f.handler, source)
	requireAskAttributesInStep(t, live, "rebuilt-decision", ownRow, "")
	requireAskAttributesInStep(t, live, "close-decision", sourceRow, "dispatch://"+issue.Key+"/spec")
	if err := f.documents.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
		t.Fatalf("settle the rebuilt notes: %v", err)
	}
	awaitSettled(t, f.database, notes)
	_, version := documentTexts(t, f.handler, notes)
	requireAskAttributesInStep(t, version, "rebuilt-decision", ownRow, "")
	requireAskAttributesInStep(t, version, "close-decision", sourceRow, "dispatch://"+issue.Key+"/spec")
	requireLiveText(t, f.handler, notes,
		`#rebuilt-decision state="resolved"`,
		`#close-decision state="answered" answered_by="alice"`,
	)
}

// requireAskAttributesInStep fails t unless the ask block blockID of markdown, a document's
// rendering, carries the attributes a version keeps of ask (docs' keptAskState), each as ask's row
// holds it, and no other of the seven a version can store: its state; for an answered ask, who
// answered it and when; and for a copy, whose source ask is ask on the document copied, that ask
// and that document. The ask is no open one, whose block keeps no state.
func requireAskAttributesInStep(t *testing.T, markdown, blockID string, ask model.Ask, copied string) {
	t.Helper()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("parse the document:\n%s\n%v", markdown, err)
	}
	var block *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); node.Type == "ask" && id == blockID {
			block = node
		}
		return block == nil
	})
	if block == nil {
		t.Fatalf("the document holds no ask block %s:\n%s", blockID, markdown)
	}
	want := map[string]any{"state": ask.State}
	if ask.State == "answered" && ask.Answer != nil {
		want["answered_by"] = ask.Answer.User
		want["answered_at"] = asks.AnsweredAtValue(ask.Answer.At)
	}
	if copied != "" {
		want["copied_from"] = ask.ID
		want["copied_from_document"] = copied
	}
	for _, name := range []string{"state", "answered_by", "answered_at", "selected", "answer", "copied_from", "copied_from_document"} {
		got, held := block.Attrs[name]
		wanted, isWanted := want[name]
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(wanted)
		if held != isWanted || string(gotJSON) != string(wantJSON) {
			t.Errorf("ask block %s %s = %s (held %t), want %s (held %t), as its ask's row holds it", blockID, name, gotJSON, held, wantJSON, isWanted)
		}
	}
}

// makeDocumentUnloadable replaces every stored update of the room document is edited in
// (documentRoomOf: its issue room, or the document itself) with bytes that do not decode, with no
// room holding the document and no settlement owed that the close's own settlement would load it
// for, and returns how many stored updates it replaced, so a test can check it corrupted the
// history it expects.
func makeDocumentUnloadable(t *testing.T, f repairFixture, document string) int64 {
	t.Helper()
	ctx := context.Background()
	room, _ := documentRoomOf(t, f.database.Pool, document)
	if err := f.documents.Evict(ctx, room); err != nil {
		t.Fatalf("evict the notes' room: %v", err)
	}
	if _, err := f.database.Pool.Exec(ctx, `delete from doc_settlements_pending where artifact_id = any($1::uuid[])`, []string{document, room}); err != nil {
		t.Fatalf("clear the notes' owed settlement: %v", err)
	}
	corrupted, err := f.database.Pool.Exec(ctx, `update doc_updates set update = '\xff'::bytea where artifact_id = $1`, room)
	if err != nil {
		t.Fatalf("corrupt the notes' history: %v", err)
	}
	if _, err := docs.NewPgVersioned(f.database).LoadDocument(ctx, room); !errors.Is(err, docs.ErrDocumentUnloadable) {
		t.Fatalf("load the corrupted notes: %v, want ErrDocumentUnloadable", err)
	}
	return corrupted.RowsAffected()
}
