package docs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// keptEditorQuestion and keptEditorReword are the question keptEditorSpec's decision asks and the
// one alice's browser rewords it to while its issue is closed.
const keptEditorQuestion, keptEditorReword = "Ship it on Friday?", "Ship it on Monday instead?"

// keptEditorSpec is the document of TestAVersionNamesNoEditorWhoseTextItDoesNotHold: a decision
// whose question alice's browser rewords while its issue is closed.
const keptEditorSpec = "Context.\n\n:::ask{#kept-editor-decision urgency=\"med\" multiple=\"false\"}\n" + keptEditorQuestion + "\n:::\n"

// keptEditorSweep is a document whose issue a close by an older release left with its asks open,
// whose decision's question alice's browser reworded inside the close margin, which a closed
// issue's settlement saved, keeping alice as the document's closed edit source
// (doc_closed_edit_sources), and whose blocks the ask repair then swept, withdrawing the decision
// (WriteIssueAskStates): a write that names no editor of its own and leaves the document owing the
// settlement this does not run.
type keptEditorSweep struct {
	service    *Service
	artifactID string
	alice      model.Actor
	decision   model.Ask
	// approval is the approval request opened over the document before the close, "" for none.
	approval string
	// edited is the version the closed issue's settlement of alice's rewording wrote.
	edited int
}

// sweepAfterAClosedEdit builds keptEditorSweep, with an approval request opened over the document
// before the close when requestBeforeClose is set. The close is written by SQL a minute ahead, with
// nothing telling the room and no ask withdrawn, as a close by an older release left an issue, so
// the store's close margin admits alice's rewording however long the run takes
// (observedBeforeClose). The ask repair withdraws the decision and the request, writing the
// decision's block resolved first, all in one transaction holding the issue's row, as the repair's
// final sweep does (api.sweepIssueAskBlocksTx). That write, and the reopen's that writes the block
// open again, are made while another build serves (serveTwoBuilds), whose pages read an ask's
// state from its block: on a fleet of one build neither changes a byte of the document
// (WriteIssueAskStates), and no settlement would follow either.
func sweepAfterAClosedEdit(t *testing.T, requestBeforeClose bool) keptEditorSweep {
	t.Helper()
	service, artifactID := newTestService(t)
	serveTwoBuilds(t, service.store)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, keptEditorSpec)
	settleCurrentGeneration(t, service, artifactID)
	f := keptEditorSweep{service: service, artifactID: artifactID, alice: model.Actor{Kind: "user", ID: "alice@example.com"}}
	f.decision = blockAskOn(t, service, artifactID, "kept-editor-decision")
	if f.decision.State != "open" || f.decision.Question != keptEditorQuestion {
		t.Fatalf("the seeded decision's ask reads %s asking %q, want open asking %q", f.decision.State, f.decision.Question, keptEditorQuestion)
	}
	if requestBeforeClose {
		f.approval = openStoredApprovalAsk(t, service.store, artifactID, latestVersionNumber(t, service, artifactID))
	}

	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load the document's room while DOC-1 is open: %v", err)
	}
	if _, err := service.store.Pool.Exec(ctx, `update issues set closed_at = now() + interval '1 minute' where key = 'DOC-1'`); err != nil {
		t.Fatalf("close DOC-1 in the database alone: %v", err)
	}
	// The room is warm already: warming it again is an injection the closed issue refuses.
	service.addConnection(artifactID, 1, f.alice)
	editAsPeer(t, service, artifactID, replaceRun(keptEditorQuestion, keptEditorReword))
	service.removeConnection(artifactID, 1)
	waitForLandedAppends(t, service, artifactID)
	f.settleOwed(t, "the closed issue's settlement of alice's rewording")
	edited, markdown, authors := latestDocumentVersion(t, service.store, artifactID)
	if !strings.Contains(markdown, keptEditorReword) || !slices.ContainsFunc(authors, f.alice.SameAs) {
		t.Fatalf("the closed issue's settlement left version %d reading:\n%s\nlisting %+v, want alice's rewording, listing her", edited, markdown, authors)
	}
	if kept := keptClosedEditSource(t, service, artifactID); kept == nil || !kept.SameAs(f.alice) {
		t.Fatalf("the closed issue's settlement of alice's rewording kept %+v as the document's closed edit source, want alice", kept)
	}
	f.edited = edited

	closer := model.Actor{Kind: "user", ID: "carol@example.com"}
	withdrawn := []string{f.decision.ID}
	if f.approval != "" {
		withdrawn = append(withdrawn, f.approval)
	}
	if err := joinedWrite(service, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `select 1 from issues where key = 'DOC-1' for no key update`); err != nil {
			return fmt.Errorf("lock the issue: %w", err)
		}
		if _, err := service.WriteIssueAskStates(ctx, "DOC-1", []model.Ask{withdrawnAsk(f.decision, closer)}, closer, true); err != nil {
			return err
		}
		// The sweep's ask.resolved events are no part of what this test reads, so each withdrawal
		// states its facts on the ask's node at the sentinel (nil held, asks.Settle).
		for _, id := range withdrawn {
			if _, err := WriteAskResolution(ctx, tx, model.Ask{ID: id}, model.AskResolutionWithdrawn, "Withdrawn by the close of DOC-1.", closer, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("the ask repair's sweep of the closed issue: %v", err)
	}
	if owed, named, err := readOwedSettlement(ctx, service.store.Pool, artifactID); err != nil || !owed || named == nil || !named.SameAs(f.alice) {
		t.Fatalf("after the sweep the document owes %t naming %+v (%v), want it owed naming alice, the editor the closed issue's settlement kept", owed, named, err)
	}
	return f
}

// settleOwed runs the resumption over what f's document owes and waits until it owes nothing,
// what naming that settlement, and answers the id of the newest event before the resumption ran.
// Every write before and after it leaves its settlement to the next call: the settle delay is an
// hour outside it.
func (f keptEditorSweep) settleOwed(t *testing.T, what string) int64 {
	t.Helper()
	lastEvent := latestEventID(t, f.service.store)
	f.service.settle = 20 * time.Millisecond
	if err := f.service.resumeOwedSettlements(context.Background(), 0); err != nil {
		t.Fatalf("resume %s: %v", what, err)
	}
	waitForNoPendingSettlement(t, f.service, f.artifactID, what)
	f.service.settle = time.Hour
	return lastEvent
}

// reopen reopens DOC-1 as the API's reopen does (api.restoreIssueAsksTx), in one transaction that
// holds the issue's row once it clears closed_at: it restores the asks the sweep withdrew, writes
// the decision's block open again (the reopen's ask-state write, which names no editor of its own,
// so the pending row it inserts names alice), marks owed the documents whose ask blocks wait for
// the reopen, and follows the versions written while the issue was closed. The document owes the
// settlement of the ask-state write, so the follow moves no request and keeps alice for that
// settlement.
func (f keptEditorSweep) reopen(t *testing.T) {
	t.Helper()
	reopener := model.Actor{Kind: "user", ID: "dave@example.com"}
	restored := []string{f.decision.ID}
	if f.approval != "" {
		restored = append(restored, f.approval)
	}
	var followed []model.Event
	if err := joinedWrite(f.service, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `update issues set closed_at = null where key = 'DOC-1'`); err != nil {
			return fmt.Errorf("clear the issue's closed_at: %w", err)
		}
		if _, err := tx.Exec(ctx, `update asks set state = 'open', answer = null, resolution = null where id = any($1::uuid[])`, restored); err != nil {
			return fmt.Errorf("restore the withdrawn asks: %w", err)
		}
		if _, err := f.service.WriteIssueAskStates(ctx, "DOC-1", []model.Ask{f.decision}, reopener, false); err != nil {
			return err
		}
		if err := f.service.MarkReopenOwedAskDocuments(ctx, "DOC-1"); err != nil {
			return err
		}
		var err error
		followed, err = f.service.FollowClosedIssueVersions(ctx, "DOC-1")
		return err
	}); err != nil {
		t.Fatalf("reopen the issue: %v", err)
	}
	for _, event := range followed {
		f.service.events.Publish(event)
	}
	f.service.issueOpenChanged(context.Background(), "DOC-1")
	if kept := keptClosedEditSource(t, f.service, f.artifactID); kept == nil || !kept.SameAs(f.alice) {
		t.Fatalf("after the reopen the document keeps %+v as its closed edit source, want alice for the settlement it owes", kept)
	}
}

// settledAfterTheReopen settles what f's reopen left owed and checks the version that settlement
// wrote: the one after swept, the version the closed issue's settlement of the sweep wrote, holding
// the decision written open again with alice's question, listing no one and appended in the name of
// SettlementActor, since alice's rewording is in the version the closed issue's settlement wrote and
// this one holds only the reopen's ask-state write. The settlement still takes alice's question into
// the restored decision with one ask.edited in her name, which notifies: that edit is her text. It
// answers the events the settlement appended.
func (f keptEditorSweep) settledAfterTheReopen(t *testing.T, swept int) []appendedEvent {
	t.Helper()
	lastEvent := f.settleOwed(t, "the settlement after the reopen")
	reopened, markdown, authors := latestDocumentVersion(t, f.service.store, f.artifactID)
	const open = `#kept-editor-decision urgency="med" multiple="false" state="open"`
	if reopened != swept+1 || !strings.Contains(markdown, open) || !strings.Contains(markdown, keptEditorReword) {
		t.Fatalf("the settlement after the reopen left version %d reading:\n%s\nwant version %d holding the decision open (%s) asking %q",
			reopened, markdown, swept+1, open, keptEditorReword)
	}
	if slices.ContainsFunc(authors, f.alice.SameAs) {
		t.Fatalf("the settlement after the reopen wrote version %d listing %+v, want no alice: it holds only the reopen's ask-state write", reopened, authors)
	}
	appended := eventsAfter(t, f.service.store, lastEvent)
	versions := eventsOfType(appended, "artifact.version")
	if len(versions) != 1 || !versions[0].actor.SameAs(SettlementActor) {
		t.Fatalf("the settlement after the reopen appended the artifact.version events %+v, want one in the name of %+v, not alice: its version holds none of her text",
			versions, SettlementActor)
	}
	edits := askEventsOf(t, appended, "ask.edited", f.decision.ID)
	if len(edits) != 1 || !edits[0].actor.SameAs(f.alice) || !edits[0].notify {
		t.Fatalf("the settlement after the reopen appended the ask.edited events %+v of the decision, want one in alice's name, taking her question, and notifying", edits)
	}
	return appended
}

// eventsOfType is each of appended of type kind.
func eventsOfType(appended []appendedEvent, kind string) []appendedEvent {
	var typed []appendedEvent
	for _, event := range appended {
		if event.kind == kind {
			typed = append(typed, event)
		}
	}
	return typed
}

// askEventsOf is each of appended of type kind whose payload names the ask id.
func askEventsOf(t *testing.T, appended []appendedEvent, kind, id string) []appendedEvent {
	t.Helper()
	if id == "" {
		t.Fatalf("no ask id to read the %s events of", kind)
	}
	var named []appendedEvent
	for _, event := range eventsOfType(appended, kind) {
		if event.id == id {
			named = append(named, event)
		}
	}
	return named
}

// The editor a closed issue's settlement keeps (doc_closed_edit_sources) is credited with the text
// that editor wrote, and no version names them that adds none of it (settlementAuthors,
// settlementVersionCredit.kept). In each case alice's browser rewords a decision while its issue is
// closed, a closed issue's settlement saves the rewording and keeps alice, and the ask repair's
// sweep then withdraws the decision, writing its block resolved: a write naming no editor of its
// own, whose pending row names alice (settlementPendingStatement).
//
//   - The closed issue's settlement of the sweep writes a version that lists no one, appended in the
//     name of SettlementActor rather than alice's, and keeps alice as the closed edit source, for the
//     reopen.
//   - After the reopen, whose ask-state write writes the decision's block open again, the settlement
//     writes a version of that write alone, listing no one and appended in the name of
//     SettlementActor, while the ask.edited taking alice's question into the restored decision is
//     hers. An approval request opened before the close moves from its version from before the
//     close, over the versions the closed issue's settlements wrote, which hold alice's rewording,
//     so its move is in alice's name, as the reopen's own move to such a version is
//     (FollowClosedIssueVersions).
//   - A request opened after the reopen names the version the sweep's settlement wrote, so its move
//     goes over the reopen's ask-state write alone, in the name of SettlementActor.
func TestAVersionNamesNoEditorWhoseTextItDoesNotHold(t *testing.T) {
	t.Run("a closed issue's settlement of the ask repair's sweep", func(t *testing.T) {
		f := sweepAfterAClosedEdit(t, false)
		lastEvent := f.settleOwed(t, "the closed issue's settlement of the sweep")
		swept, markdown, authors := latestDocumentVersion(t, f.service.store, f.artifactID)
		const resolved = `#kept-editor-decision urgency="med" multiple="false" state="resolved"`
		if swept != f.edited+1 || !strings.Contains(markdown, resolved) {
			t.Fatalf("the closed issue's settlement of the sweep left version %d reading:\n%s\nwant version %d holding the decision resolved (%s)",
				swept, markdown, f.edited+1, resolved)
		}
		if slices.ContainsFunc(authors, f.alice.SameAs) {
			t.Fatalf("the closed issue's settlement of the sweep wrote version %d listing %+v, want no alice: it holds only the sweep", swept, authors)
		}
		versions := eventsOfType(eventsAfter(t, f.service.store, lastEvent), "artifact.version")
		if len(versions) != 1 || !versions[0].actor.SameAs(SettlementActor) {
			t.Fatalf("the closed issue's settlement of the sweep appended the artifact.version events %+v, want one in the name of %+v, not alice: its version holds none of her text",
				versions, SettlementActor)
		}
		if kept := keptClosedEditSource(t, f.service, f.artifactID); kept == nil || !kept.SameAs(f.alice) {
			t.Fatalf("after the closed issue's settlement of the sweep the document keeps %+v as its closed edit source, want alice still, for the reopen", kept)
		}
	})

	t.Run("the settlement after the reopen, moving a request from before the close", func(t *testing.T) {
		f := sweepAfterAClosedEdit(t, true)
		f.settleOwed(t, "the closed issue's settlement of the sweep")
		swept := latestVersionNumber(t, f.service, f.artifactID)
		f.reopen(t)
		appended := f.settledAfterTheReopen(t, swept)
		moves := askEventsOf(t, appended, "ask.edited", f.approval)
		if len(moves) != 1 || !moves[0].actor.SameAs(f.alice) {
			t.Fatalf("the settlement after the reopen moved the approval request with the ask.edited events %+v, want one in alice's name: it moves over the versions holding her rewording", moves)
		}
		if kept := keptClosedEditSource(t, f.service, f.artifactID); kept != nil {
			t.Fatalf("after the settlement on the open issue the document keeps %+v as its closed edit source, want none", *kept)
		}
	})

	t.Run("the settlement after the reopen, moving a request opened after the reopen", func(t *testing.T) {
		f := sweepAfterAClosedEdit(t, false)
		f.settleOwed(t, "the closed issue's settlement of the sweep")
		swept := latestVersionNumber(t, f.service, f.artifactID)
		f.reopen(t)
		approval := openStoredApprovalAsk(t, f.service.store, f.artifactID, swept)
		appended := f.settledAfterTheReopen(t, swept)
		moves := askEventsOf(t, appended, "ask.edited", approval)
		if len(moves) != 1 || !moves[0].actor.SameAs(SettlementActor) {
			t.Fatalf("the settlement after the reopen moved the approval request with the ask.edited events %+v, want one in the name of %+v: it moves over the reopen's ask-state write alone",
				moves, SettlementActor)
		}
	})
}
