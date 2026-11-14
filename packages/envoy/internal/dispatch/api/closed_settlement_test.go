package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A closed issue's document settles like any other, and its settlement changes no ask of the
// issue: the asks stand as the close left them, and an ask block with no row is saved resolved
// with none until the issue reopens, when it becomes an open ask credited to its author. The
// close and the reopen go through PATCH /api/v1/issues/{key}, as a person's do. No room sync runs
// here, so the issue's rooms never hear of the close, and a browser connected before it goes on
// writing into its room.

// closedSettlementSpec is the spec of TestASettlementAfterACloseChangesNoAsk: a decision the
// close withdraws, and a decision a person answers before the close.
const closedSettlementSpec = "Context\n\n" +
	":::ask{#withdrawn-decision urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n\n" +
	":::ask{#answered-decision urgency=\"med\" multiple=\"false\"}\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n"

// newClosedSettlementHandler is the API over a document service whose settle delay is 20 ms, whose
// identity admits a browser peer's socket (connectBrowserPeer).
func newClosedSettlementHandler(t *testing.T) (http.Handler, *store.Store, *docs.Service) {
	t.Helper()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	return handler, database, documentService
}

// closeIssueThroughPatch closes issue through PATCH /api/v1/issues/{key} as alice, which withdraws
// its open asks and sweeps its documents' ask blocks before it answers, and then moves the close a
// minute ahead in the database: the issue reads closed, and the store's close margin admits a
// browser's update its room takes from now on however long the test runs (observedBeforeClose).
func closeIssueThroughPatch(t *testing.T, handler http.Handler, database *store.Store, issue string) {
	t.Helper()
	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close %s: status=%d body=%s", issue, closed.Code, closed.Body.String())
	}
	if _, err := database.Pool.Exec(context.Background(), `update issues set closed_at = now() + interval '1 minute' where key = $1`, issue); err != nil {
		t.Fatalf("move the close of %s a minute ahead: %v", issue, err)
	}
}

// issueAskEventCount counts issue's ask.* events.
func issueAskEventCount(t *testing.T, database *store.Store, issue string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where issue_key = $1 and type like 'ask.%'
	`, issue).Scan(&count); err != nil {
		t.Fatalf("count the ask events of %s: %v", issue, err)
	}
	return count
}

// issueAskCount counts issue's ask rows.
func issueAskCount(t *testing.T, database *store.Store, issue string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from asks where issue_key = $1`, issue).Scan(&count); err != nil {
		t.Fatalf("count the asks of %s: %v", issue, err)
	}
	return count
}

// recordedAskAuthor is the author doc_ask_block_authors records for document's ask block
// blockID, and whether it records one.
func recordedAskAuthor(t *testing.T, database *store.Store, document, blockID string) (model.Actor, bool) {
	t.Helper()
	var raw []byte
	err := database.Pool.QueryRow(context.Background(), `
		select actor from doc_ask_block_authors where artifact_id = $1 and block_id = $2
	`, document, blockID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Actor{}, false
	}
	if err != nil {
		t.Fatalf("read the recorded author of block %s: %v", blockID, err)
	}
	var actor model.Actor
	if err := json.Unmarshal(raw, &actor); err != nil {
		t.Fatalf("decode the recorded author of block %s: %v", blockID, err)
	}
	return actor, true
}

// documentUpdateCount counts document's stored updates.
func documentUpdateCount(t *testing.T, database *store.Store, document string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, document).Scan(&count); err != nil {
		t.Fatalf("count the stored updates of %s: %v", document, err)
	}
	return count
}

// documentOwesSettlement is whether document has a pending-settlement row.
func documentOwesSettlement(t *testing.T, database *store.Store, document string) bool {
	t.Helper()
	var owed bool
	if err := database.Pool.QueryRow(context.Background(), `
		select exists (select from doc_settlements_pending where artifact_id = $1)
	`, document).Scan(&owed); err != nil {
		t.Fatalf("read whether %s owes a settlement: %v", document, err)
	}
	return owed
}

// awaitStoredUpdate waits until document stores more updates than before, the count it stored
// before a browser's edit: the edit's append has stored it, and the document owes the settlement
// that reads it or has had it.
func awaitStoredUpdate(t *testing.T, database *store.Store, document string, before int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); documentUpdateCount(t, database, document) <= before; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("document %s still stores %d updates, want the browser's edit stored", document, before)
		}
	}
}

// awaitSettledVersionHolding waits until document owes no settlement and its latest version holds
// part, and answers that version's markdown.
func awaitSettledVersionHolding(t *testing.T, handler http.Handler, database *store.Store, document, part string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		_, version := documentTexts(t, handler, document)
		if strings.Contains(version, part) && !documentOwesSettlement(t, database, document) {
			return version
		}
		if time.Now().After(deadline) {
			t.Fatalf("document %s never settled a version holding %q; its latest version reads:\n%s", document, part, version)
		}
	}
}

// A settlement that runs after an issue's close leaves every ask of the issue as the close left
// it (R2). The spec holds a decision the close withdraws and one alice answered before the close,
// and alice's browser, connected before the close, writes a third decision into the room once the
// close has swept the documents. The closed issue's documents then settle (SettleIssueDocuments).
// Each ask row is the row the close left, the issue has no ask.* event and no ask row more than
// after the close, the third block has no ask, and the newest version shows the withdrawn decision
// and the third block resolved, and the answered decision answered.
func TestASettlementAfterACloseChangesNoAsk(t *testing.T) {
	handler, database, documentService := newClosedSettlementHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A settlement after the close", closedSettlementSpec)
	spec := issue.PrimaryArtifactID
	awaitIndexedAskBlock(t, handler, spec, "withdrawn-decision", "Ship it?")
	awaitIndexedAskBlock(t, handler, spec, "answered-decision", "Ship on Monday?")
	withdrawn := requireBlockAsk(t, handler, issue.Key, spec, "withdrawn-decision")
	answered := requireBlockAsk(t, handler, issue.Key, spec, "answered-decision")
	if reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answered+"/answer", map[string]any{
		"selected": []string{"Yes"},
	}, "alice"); reply.Code != http.StatusOK {
		t.Fatalf("answer the decision: status=%d body=%s", reply.Code, reply.Body.String())
	}
	awaitSettled(t, database, spec)
	peer := connectBrowserPeer(t, documentService, spec)

	closeIssueThroughPatch(t, handler, database, issue.Key)
	alice := model.Actor{Kind: "user", ID: "alice"}
	requireWithdrawn(t, handler, issue.Key, withdrawn, alice)
	if got := readBlockAsk(t, handler, answered); got.State != "answered" {
		t.Fatalf("the decision answered before the close reads %s after it, want answered", got.State)
	}
	rows := askRowTexts(t, database, withdrawn, answered)
	askEvents := issueAskEventCount(t, database, issue.Key)
	asks := issueAskCount(t, database, issue.Key)

	// The close's ask-state write left the spec owed, and its settlement may store updates of its
	// own: it is done before the count the browser's edit is waited for against.
	awaitSettled(t, database, spec)
	stored := documentUpdateCount(t, database, spec)
	peer.appendParagraph(t, ":::ask{#late-decision urgency=\"low\" multiple=\"false\"}\nWritten after the close's sweep?\n:::\n")
	peer.barrier(t)
	awaitStoredUpdate(t, database, spec, stored)
	if err := documentService.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
		t.Fatalf("settle the closed issue's documents: %v", err)
	}
	version := awaitSettledVersionHolding(t, handler, database, spec, "#late-decision")

	if id := findBlockAsk(t, handler, issue.Key, spec, "late-decision"); id != "" {
		t.Fatalf("the block written after the close's sweep is ask %s, want no ask on a closed issue", id)
	}
	if got := askRowTexts(t, database, withdrawn, answered); !reflect.DeepEqual(got, rows) {
		t.Fatalf("the settlement after the close changed the asks:\n%v\nwant them as the close left them\n%v", got, rows)
	}
	if got := issueAskEventCount(t, database, issue.Key); got != askEvents {
		t.Fatalf("the settlement after the close appended %d ask events, want none", got-askEvents)
	}
	if got := issueAskCount(t, database, issue.Key); got != asks {
		t.Fatalf("the issue has %d asks after the settlement, want the %d the close left", got, asks)
	}
	requireWithdrawn(t, handler, issue.Key, withdrawn, alice)
	for _, part := range []string{
		`#withdrawn-decision urgency="med" multiple="false" state="resolved"`,
		`#answered-decision urgency="med" multiple="false" state="answered"`,
		`#late-decision urgency="low" multiple="false" state="resolved"`,
	} {
		if !strings.Contains(version, part) {
			t.Fatalf("the newest version reads:\n%s\nwant it to hold %s", version, part)
		}
	}
}

// An ask block that lands in a closed issue's document after the close's sweep, and has no ask
// row, is saved resolved and gets no row, and the issue's reopen through PATCH makes it an open
// ask credited to its author (R2). alice's browser, connected before the close, writes the block.
// The resumption settles the document on the closed issue: the version shows the block resolved,
// no ask row names it, and doc_ask_block_authors records its author. The document holds no block
// of an ask the close withdrew, so the reopen's ask-state write leaves its text and its stored
// updates as they were, and the reopen marks it owing a settlement instead
// (docs.MarkUnindexedAskDocumentsOwed). The resumption after the reopen opens the block's ask,
// credited to the recorded author.
func TestAnAskBlockWithNoRowIsResolvedWhileClosedAndOpensAtTheReopen(t *testing.T) {
	handler, database, documentService := newClosedSettlementHandler(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "TEST", "An ask block across a close", "Context\n")
	spec := issue.PrimaryArtifactID
	awaitSettled(t, database, spec)
	peer := connectBrowserPeer(t, documentService, spec)
	closeIssueThroughPatch(t, handler, database, issue.Key)

	awaitSettled(t, database, spec)
	stored := documentUpdateCount(t, database, spec)
	peer.appendParagraph(t, ":::ask{#closed-decision urgency=\"med\" multiple=\"false\"}\nAsked while the issue is closed?\n:::\n")
	peer.barrier(t)
	awaitStoredUpdate(t, database, spec, stored)
	author := model.Actor{Kind: "user", ID: "alice"}
	if err := documentService.ResumeOwedSettlementsForTest(ctx, 0); err != nil {
		t.Fatalf("resume the closed issue's owed settlements: %v", err)
	}
	version := awaitSettledVersionHolding(t, handler, database, spec, "#closed-decision")
	if id := findBlockAsk(t, handler, issue.Key, spec, "closed-decision"); id != "" {
		t.Fatalf("the block written on the closed issue is ask %s, want no ask while the issue is closed", id)
	}
	const resolved = `#closed-decision urgency="med" multiple="false" state="resolved"`
	if !strings.Contains(version, resolved) {
		t.Fatalf("the saved version reads:\n%s\nwant the block saved resolved: %s", version, resolved)
	}
	if recorded, ok := recordedAskAuthor(t, database, spec, "closed-decision"); !ok || !recorded.SameAs(author) {
		t.Fatalf("the block's recorded author is %+v (recorded %t), want %+v, whose browser wrote it", recorded, ok, author)
	}
	live, _ := documentTexts(t, handler, spec)

	updates := documentUpdateCount(t, database, spec)
	if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if after, _ := documentTexts(t, handler, spec); after == "" || after != live {
		t.Fatalf("the reopen's ask-state write changed the document:\n%s\nwant it as it was:\n%s", after, live)
	}
	if after := documentUpdateCount(t, database, spec); after != updates {
		t.Fatalf("the reopen's ask-state write stored %d updates, want none", after-updates)
	}
	if !documentOwesSettlement(t, database, spec) {
		t.Fatal("the reopen left the document owing no settlement, so nothing opens the block's ask")
	}

	if err := documentService.ResumeOwedSettlementsForTest(ctx, 0); err != nil {
		t.Fatalf("resume the reopened issue's owed settlements: %v", err)
	}
	var opened model.Ask
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if id := findBlockAsk(t, handler, issue.Key, spec, "closed-decision"); id != "" {
			if opened = readBlockAsk(t, handler, id); opened.State == "open" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reopened issue's settlement never opened the block's ask (latest read %+v)", opened)
		}
	}
	if !opened.Author.SameAs(author) {
		t.Fatalf("the block's ask is credited to %+v, want %+v, its recorded author", opened.Author, author)
	}
	requireLiveText(t, handler, spec, `#closed-decision urgency="med" multiple="false" state="open"`)
}
