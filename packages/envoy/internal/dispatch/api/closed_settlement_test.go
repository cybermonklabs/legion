package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
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

// newClosedSettlementHandler is closedSettlementHandlerSettlingAfter with a settle delay of 20 ms:
// the settlement a write arms runs while the test does.
func newClosedSettlementHandler(t *testing.T) (http.Handler, *store.Store, *docs.Service) {
	t.Helper()
	return closedSettlementHandlerSettlingAfter(t, 20*time.Millisecond)
}

// closedSettlementHandlerSettlingAfter is the API over a document service whose settle delay is
// settle, whose identity admits a browser peer's socket (connectBrowserPeer).
func closedSettlementHandlerSettlingAfter(t *testing.T, settle time.Duration) (http.Handler, *store.Store, *docs.Service) {
	t.Helper()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: settle})
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
	closeArtifactIssue(t, handler, issue)
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

// awaitSettledVersionHolding waits until document owes no settlement and its latest version holds
// part, and answers that version's markdown. Its one bound is the test binary's deadline
// (testwait.UntilPolling), and its failure prints the latest version it read.
func awaitSettledVersionHolding(t *testing.T, handler http.Handler, database *store.Store, document, part string) string {
	t.Helper()
	return testwait.UntilPolling(t, 20*time.Millisecond, fmt.Sprintf("document %s to settle a version holding %q (its latest version)", document, part), func() (string, bool) {
		_, version := documentTexts(t, handler, document)
		return version, strings.Contains(version, part) && !documentOwesSettlement(t, database, document)
	})
}

// A settlement that runs after an issue's close leaves every ask of the issue as the close left
// it (R2). The spec holds a decision the close withdraws and one alice answered before the close,
// and alice's browser, connected before the close, writes a third decision into the room once the
// close has swept the documents. The closed issue's documents then settle (SettleIssueDocuments).
// Each ask row is the row the close left, the issue has no ask.* event and no ask row more than
// after the close, the third block has no ask, the newest version holds the third block and none
// of the server-owned ask attributes, and the text an agent reads shows the withdrawn decision and
// the third block resolved, and the answered decision answered.
func TestASettlementAfterACloseChangesNoAsk(t *testing.T) {
	t.Parallel()
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
	alice := model.Actor{Kind: "user", ID: "alice", Proven: true}
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
	stored := storedUpdateCount(t, database, spec)
	peer.appendParagraph(t, ":::ask{#late-decision urgency=\"low\" multiple=\"false\"}\nWritten after the close's sweep?\n:::\n")
	peer.barrier(t)
	awaitStoredUpdates(t, database, spec, stored+1)
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
	// An ask's state is held on its node, not in the document: the
	// newest version holds none of the seven attributes, and the asks member of the text an agent
	// reads holds the withdrawn decision resolved and the answered one answered from their rows,
	// and the block with no row resolved from the issue's close, since it opens no ask while the
	// issue is closed (docs.TextWithAsks).
	requireNoAnswerAttributes(t, "the newest version", version)
	text := awaitTextShows(t, handler, spec, "the decisions as the close left them", contains(
		`#withdrawn-decision state="resolved"`,
		`#answered-decision state="answered"`,
		`#late-decision state="resolved"`+"\n",
	), nil)
	if !strings.Contains(text, "Written after the close's sweep?") {
		t.Fatalf("the document's text reads:\n%s\nwant it to hold the block written after the close", text)
	}
}

// An ask block that lands in a closed issue's document after the close's sweep, and has no ask
// row, is resolved while the issue is closed - it opens no ask and gets no row - and the issue's
// reopen through PATCH makes it an open ask credited to its author (R2). alice's browser,
// connected before the close, writes the block. The resumption settles the document on the closed
// issue: the version holds the block with no state, since on a fleet of one build no stored
// version holds an ask's state, the asks member of the text an agent
// reads holds it resolved from the issue's close (docs.TextWithAsks), no ask row names it, and
// doc_ask_block_authors records its author. The document holds no block of an ask the close
// withdrew, so the reopen's ask-state write leaves the document's own markdown, as the writes to
// its live tree left it (liveTree), and its stored updates as they were, and the reopen marks it
// owing a settlement instead (docs.MarkReopenOwedAskDocuments). The resumption after the reopen
// opens the block's ask, credited to the recorded author, and the text an agent reads shows it
// open.
func TestAnAskBlockWithNoRowIsResolvedWhileClosedAndOpensAtTheReopen(t *testing.T) {
	t.Parallel()
	handler, database, documentService := newClosedSettlementHandler(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "TEST", "An ask block across a close", "Context\n")
	spec := issue.PrimaryArtifactID
	awaitSettled(t, database, spec)
	peer := connectBrowserPeer(t, documentService, spec)
	closeIssueThroughPatch(t, handler, database, issue.Key)

	awaitSettled(t, database, spec)
	stored := storedUpdateCount(t, database, spec)
	peer.appendParagraph(t, ":::ask{#closed-decision urgency=\"med\" multiple=\"false\"}\nAsked while the issue is closed?\n:::\n")
	peer.barrier(t)
	awaitStoredUpdates(t, database, spec, stored+1)
	author := model.Actor{Kind: "user", ID: "alice"}
	if err := documentService.ResumeOwedSettlementsForTest(ctx, 0); err != nil {
		t.Fatalf("resume the closed issue's owed settlements: %v", err)
	}
	version := awaitSettledVersionHolding(t, handler, database, spec, "#closed-decision")
	if id := findBlockAsk(t, handler, issue.Key, spec, "closed-decision"); id != "" {
		t.Fatalf("the block written on the closed issue is ask %s, want no ask while the issue is closed", id)
	}
	requireNoAnswerAttributes(t, "the saved version", version)
	if !strings.Contains(version, `#closed-decision urgency="med" multiple="false"}`) {
		t.Fatalf("the saved version reads:\n%s\nwant the block saved with no state: an ask's state is held on its node alone", version)
	}
	if recorded, ok := recordedAskAuthor(t, database, spec, "closed-decision"); !ok || !recorded.SameAs(author) {
		t.Fatalf("the block's recorded author is %+v (recorded %t), want %+v, whose browser wrote it", recorded, ok, author)
	}
	requireLiveText(t, handler, spec, `#closed-decision state="resolved"`+"\n")
	live := liveTree(t, documentService, spec)

	updates := storedUpdateCount(t, database, spec)
	if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if after := liveTree(t, documentService, spec); after == "" || after != live {
		t.Fatalf("the reopen's ask-state write changed the document:\n%s\nwant it as it was:\n%s", after, live)
	}
	if after := storedUpdateCount(t, database, spec); after != updates {
		t.Fatalf("the reopen's ask-state write stored %d updates, want none", after-updates)
	}
	if !documentOwesSettlement(t, database, spec) {
		t.Fatal("the reopen left the document owing no settlement, so nothing opens the block's ask")
	}

	if err := documentService.ResumeOwedSettlementsForTest(ctx, 0); err != nil {
		t.Fatalf("resume the reopened issue's owed settlements: %v", err)
	}
	opened := testwait.UntilPolling(t, 20*time.Millisecond, "the reopened issue's settlement to open the block's ask (the ask last read)", func() (model.Ask, bool) {
		var ask model.Ask
		if id := findBlockAsk(t, handler, issue.Key, spec, "closed-decision"); id != "" {
			ask = readBlockAsk(t, handler, id)
		}
		return ask, ask.State == "open"
	})
	if !opened.Author.SameAs(author) {
		t.Fatalf("the block's ask is credited to %+v, want %+v, its recorded author", opened.Author, author)
	}
	requireLiveText(t, handler, spec, `#closed-decision state="open"`)
}
