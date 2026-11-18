package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// leavingDecisionSpec is the spec of
// TestAWithdrawnAskWhoseBlockLeftWhileClosedIsRestoredAtTheReopen: a decision whose block alice's
// browser deletes while the issue is closed.
const leavingDecisionSpec = "Context\n\n" +
	":::ask{#leaving-decision urgency=\"med\" multiple=\"false\"}\nWill it stay?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// An ask the close withdrew, whose block leaves its document while the issue is closed, ends once
// the issue reopens as it ended before a closed issue's document settled (R2): retracted by the
// settlement after the reopen, as settlement retracts an open ask whose block left, and in no
// open-ask list. alice's browser, connected before the close, deletes the decision's block once
// the close has withdrawn the ask and swept the document, and the resumption settles the document
// on the closed issue: the version lacks the block, and the ask stays withdrawn, since a closed
// issue's settlement retracts nothing. That settlement deletes the document's pending row, and the
// reopen's ask-state write finds no block of the restored ask to write, so the reopen marks the
// document owed itself, as the block document of an open ask (docs.MarkReopenOwedAskDocuments).
// The resumption after the reopen settles it on the open issue.
func TestAWithdrawnAskWhoseBlockLeftWhileClosedIsRestoredAtTheReopen(t *testing.T) {
	handler, database, documentService := newClosedSettlementHandler(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "TEST", "An ask whose block leaves while closed", leavingDecisionSpec)
	spec := issue.PrimaryArtifactID
	awaitIndexedAskBlock(t, handler, spec, "leaving-decision", "Will it stay?")
	ask := requireBlockAsk(t, handler, issue.Key, spec, "leaving-decision")
	awaitSettled(t, database, spec)
	peer := connectBrowserPeer(t, documentService, spec)
	closeIssueThroughPatch(t, handler, database, issue.Key)
	alice := model.Actor{Kind: "user", ID: "alice", Proven: true}
	requireWithdrawn(t, handler, issue.Key, ask, alice)

	// The close's ask-state write left the spec owed, and its settlement may store updates of its
	// own: it is done before the count the browser's deletion is waited for against.
	awaitSettled(t, database, spec)
	stored := storedUpdateCount(t, database, spec)
	peer.edit(t, func(tree *pmdoc.Node) error {
		held := len(tree.Children)
		tree.Children = slices.DeleteFunc(tree.Children, func(child *pmdoc.Node) bool {
			id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
			return id == "leaving-decision"
		})
		if len(tree.Children) == held {
			return fmt.Errorf("the document holds no block under %q", "leaving-decision")
		}
		return nil
	})
	peer.barrier(t)
	awaitStoredUpdates(t, database, spec, stored+1)
	if err := documentService.ResumeOwedSettlementsForTest(ctx, 0); err != nil {
		t.Fatalf("resume the closed issue's owed settlements: %v", err)
	}
	testwait.Until(t, "the closed issue's settlement to save a version without the deleted block", func() (string, bool) {
		_, version := documentTexts(t, handler, spec)
		return version, strings.Contains(version, "Context") && !strings.Contains(version, "leaving-decision") &&
			!documentOwesSettlement(t, database, spec)
	})
	requireWithdrawn(t, handler, issue.Key, ask, alice)
	if live, _ := documentTexts(t, handler, spec); !strings.Contains(live, "Context") || strings.Contains(live, "leaving-decision") {
		t.Fatalf("the closed issue's document reads:\n%s\nwant it without the deleted block", live)
	}

	if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	// The reopen restores the ask, as it restores every ask the close withdrew, though no block
	// shows it: until a settlement retracts it, every open-ask list lists it.
	if got := readBlockAsk(t, handler, ask); got.State != "open" || got.Resolution != nil {
		t.Fatalf("ask %s reads %s %+v after the reopen, want restored open", ask, got.State, got.Resolution)
	}
	if lists := openAskListsHolding(t, handler, issue.Key, ask); len(lists) != len(openAskLists) {
		t.Fatalf("after the reopen the restored ask %s is in %v, want it in every one of %v", ask, lists, openAskLists)
	}
	if !documentOwesSettlement(t, database, spec) {
		t.Fatal("the reopen left the document owing no settlement, so nothing retracts the restored ask whose block left it")
	}

	if err := documentService.ResumeOwedSettlementsForTest(ctx, 0); err != nil {
		t.Fatalf("resume the reopened issue's owed settlements: %v", err)
	}
	retracted := testwait.Until(t, fmt.Sprintf("the reopened issue's settlement to retract ask %s, whose block left its document", ask), func() (model.Ask, bool) {
		retracted := readBlockAsk(t, handler, ask)
		return retracted, retracted.State != "open"
	})
	// The settlement's retraction, as docs.settlementRetracted reads it: a later return of the
	// block restores the ask.
	resolution := retracted.Resolution
	if retracted.State != "resolved" || resolution == nil || resolution.Kind != "retracted" ||
		!(resolution.Actor.SameAs(docs.SettlementActor) || strings.HasPrefix(resolution.Reason, docs.SettlementRetractionReason)) {
		t.Fatalf("ask %s ends %s %+v, want retracted by the document's settlement (actor %+v or a reason opening %q)",
			ask, retracted.State, resolution, docs.SettlementActor, docs.SettlementRetractionReason)
	}
	if lists := openAskListsHolding(t, handler, issue.Key, ask); len(lists) != 0 {
		t.Fatalf("the retracted ask %s is still in %v, want it in no open-ask list", ask, lists)
	}
}

// rewordedDecisionSpec is the spec of
// TestAWithdrawnAsksBlockRewordedInsideTheCloseMarginRewordsItsAskAtTheReopen: a decision whose
// question alice's browser rewords while the issue is closed.
const rewordedDecisionSpec = "Context\n\n" +
	":::ask{#rewording-decision urgency=\"med\" multiple=\"false\"}\nShip it on Friday?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// An ask the close withdrew, whose block a person rewords inside the close margin, takes the
// person's question once the issue reopens, as it did before a closed issue's document settled
// (R2): a withdrawal is no decision, since the reopen undoes it, so the closed issue's settlement
// leaves the block as the person wrote it rather than writing the row's question back into it as
// it does for a decided ask. alice's browser, connected before the close, rewords the decision's
// question once the close has withdrawn the ask and swept the document, and the resumption
// settles the document on the closed issue: the version holds the person's question, and the ask
// stays withdrawn with the question it was asked. The reopen restores the ask and writes its block
// open, which leaves the document owed, and the resumption after the reopen settles it on the
// open issue, which takes the block's question into the open ask, as settlement takes the
// rewording of any open ask's block.
func TestAWithdrawnAsksBlockRewordedInsideTheCloseMarginRewordsItsAskAtTheReopen(t *testing.T) {
	f := rewordWithdrawnDecisionWhileClosed(t)
	ctx := context.Background()
	if reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if !documentOwesSettlement(t, f.database, f.spec) {
		t.Fatal("the reopen left the document owing no settlement, so nothing takes the block's question into the restored ask")
	}
	if err := f.documents.ResumeOwedSettlementsForTest(ctx, 0); err != nil {
		t.Fatalf("resume the reopened issue's owed settlements: %v", err)
	}
	awaitRewordedAsk(t, f)
	version := awaitSettledVersionHolding(t, f.handler, f.database, f.spec, rewordedQuestion)
	if open := `#rewording-decision urgency="med" multiple="false" state="open"`; !strings.Contains(version, open) || strings.Contains(version, askedQuestion) {
		t.Fatalf("the reopened issue's newest version reads:\n%s\nwant the block open (%s) asking %q, not %q", version, open, rewordedQuestion, askedQuestion)
	}
}

// The rewording a person gives a withdrawn ask's block inside the close margin reaches the ask
// after the reopen in that person's name, as it did before a closed issue's document settled,
// when the document's pending row kept its latest editor through the closed period: the closed
// issue's settlement that saves the rewording keeps the editor it credited
// (doc_closed_edit_sources), and the reopen's ask-state write, which credits no one, writes the
// pending row again naming that editor (settlementPendingStatement). The closed period is
// TestAWithdrawnAsksBlockRewordedInsideTheCloseMarginRewordsItsAskAtTheReopen's: alice's browser
// rewords the decision once the close has withdrawn it, and the resumption saves the rewording on
// the closed issue. bob reopens the issue through PATCH, so an edit credited to the reopener would
// read bob, and the resumption settles the document on the open issue: its one ask.edited of the
// decision is alice's, edited_by alice, and it notifies.
func TestAMarginRewordReachesTheAskInTheEditorsNameAfterTheReopen(t *testing.T) {
	f := rewordWithdrawnDecisionWhileClosed(t)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	seq := latestIssueSeq(t, f.database, f.issue)
	if reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]string{"status": "todo"}, "bob"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen as bob: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if err := f.documents.ResumeOwedSettlementsForTest(ctx, 0); err != nil {
		t.Fatalf("resume the reopened issue's owed settlements: %v", err)
	}
	awaitRewordedAsk(t, f)
	var edits []loggedEvent
	for _, event := range issueEventsAfter(t, f.database, f.issue, seq) {
		if event.kind == "ask.edited" && event.id == f.ask {
			edits = append(edits, event)
		}
	}
	if len(edits) != 1 || !edits[0].editedBy.SameAs(alice) || !edits[0].actor.SameAs(alice) || !edits[0].notify {
		t.Fatalf("after the reopen ask %s has the ask.edited events %+v, want one, edited_by and in the name of alice, whose rewording it takes, and notifying",
			f.ask, edits)
	}
}

// askedQuestion and rewordedQuestion are the question rewordedDecisionSpec's decision asks and the
// one alice's browser rewords it to while the issue is closed.
const askedQuestion, rewordedQuestion = "Ship it on Friday?", "Ship it on Monday instead?"

// rewordedWhileClosed is an issue whose spec holds rewordedDecisionSpec's decision, closed through
// PATCH, whose decision block alice's browser reworded once the close had withdrawn its ask, and
// whose document a settlement saved on the closed issue.
type rewordedWhileClosed struct {
	handler   http.Handler
	database  *store.Store
	documents *docs.Service
	issue     string
	spec      string
	ask       string
}

// rewordWithdrawnDecisionWhileClosed builds rewordedWhileClosed. alice's browser, connected before
// the close, rewords the decision's question once the close has withdrawn the ask and swept the
// document, and the resumption settles the document on the closed issue: the version holds the
// person's question, and the ask stays withdrawn with the question it was asked.
func rewordWithdrawnDecisionWhileClosed(t *testing.T) rewordedWhileClosed {
	t.Helper()
	handler, database, documentService := newClosedSettlementHandler(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "TEST", "An ask whose block is reworded while closed", rewordedDecisionSpec)
	spec := issue.PrimaryArtifactID
	awaitIndexedAskBlock(t, handler, spec, "rewording-decision", askedQuestion)
	ask := requireBlockAsk(t, handler, issue.Key, spec, "rewording-decision")
	awaitSettled(t, database, spec)
	peer := connectBrowserPeer(t, documentService, spec)
	closeIssueThroughPatch(t, handler, database, issue.Key)
	alice := model.Actor{Kind: "user", ID: "alice", Proven: true}
	requireWithdrawn(t, handler, issue.Key, ask, alice)

	// The close's ask-state write left the spec owed, and its settlement may store updates of its
	// own: it is done before the count the browser's reword is waited for against.
	awaitSettled(t, database, spec)
	stored := storedUpdateCount(t, database, spec)
	peer.edit(t, func(tree *pmdoc.Node) error {
		for _, child := range tree.Children {
			if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); id != "rewording-decision" {
				continue
			}
			if len(child.Children) == 0 || len(child.Children[0].Children) != 1 || child.Children[0].Children[0].Text != askedQuestion {
				return fmt.Errorf("the block's question is not the one text run %q", askedQuestion)
			}
			child.Children[0].Children[0].Text = rewordedQuestion
			return nil
		}
		return fmt.Errorf("the document holds no block under %q", "rewording-decision")
	})
	peer.barrier(t)
	awaitStoredUpdates(t, database, spec, stored+1)
	if err := documentService.ResumeOwedSettlementsForTest(ctx, 0); err != nil {
		t.Fatalf("resume the closed issue's owed settlements: %v", err)
	}
	// The browser's update marked the document owed in the transaction that stored it, so the
	// document owes nothing once a settlement after it has committed.
	awaitSettled(t, database, spec)
	if live, version := documentTexts(t, handler, spec); !strings.Contains(version, rewordedQuestion) || strings.Contains(version, askedQuestion) ||
		!strings.Contains(live, rewordedQuestion) {
		t.Fatalf("the closed issue's settlement left the latest version:\n%s\nand the live text:\n%s\nwant both to hold the person's question %q in place of %q",
			version, live, rewordedQuestion, askedQuestion)
	}
	requireWithdrawn(t, handler, issue.Key, ask, alice)
	if got := readBlockAsk(t, handler, ask); got.Question != askedQuestion {
		t.Fatalf("the withdrawn ask asks %q on the closed issue, want %q, the question it was asked", got.Question, askedQuestion)
	}
	return rewordedWhileClosed{handler: handler, database: database, documents: documentService, issue: issue.Key, spec: spec, ask: ask}
}

// awaitRewordedAsk waits until the settlement after f's reopen has taken the block's question into
// the restored ask: open, asking rewordedQuestion.
func awaitRewordedAsk(t *testing.T, f rewordedWhileClosed) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the reopened issue's settlement to take the block's question into ask %s, open asking %q", f.ask, rewordedQuestion), func() (model.Ask, bool) {
		restored := readBlockAsk(t, f.handler, f.ask)
		return restored, restored.State == "open" && restored.Question == rewordedQuestion
	})
}

// openAskLists are the open-ask lists openAskListsHolding reads: the issue's own open asks, the
// person's Inbox, and the project's open asks (dispatch open-asks).
var openAskLists = []string{"the issue's open asks", "alice's Inbox", "the project TEST's open asks"}

// openAskListsHolding names each of openAskLists that lists ask, an ask of issue.
func openAskListsHolding(t *testing.T, handler http.Handler, issue, ask string) []string {
	t.Helper()
	var lists []string
	if slices.Contains(openAskIDs(readIssueDetail(t, handler, issue)), ask) {
		lists = append(lists, openAskLists[0])
	}
	if slices.Contains(inboxAskIDs(t, handler), ask) {
		lists = append(lists, openAskLists[1])
	}
	for _, listed := range listProjectOpenAsks(t, handler, "TEST").Asks {
		if listed.ID == ask {
			lists = append(lists, openAskLists[2])
			break
		}
	}
	return lists
}
