package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A close withdraws the spec's approval request and a reopen restores it with the turn it had, so
// a person who was asked to approve the spec is asked again after the reopen, as before the close
// (prepareRestoration, recordRestoredAsksTx). The settlements the closed period and the reopen leave owed save the
// blocks the close and the reopen wrote, and nothing else: neither is a change the request's agent
// made to the text it handed the person. So after both have run, the request still waits on the
// person, the spec still reads awaiting approval from the person, and the request is still in the
// person's Inbox. The browser suite drives the same path (e2e/close-withdraws-asks.e2e.ts, whose
// "Waiting on you (3)" counts this request).
func TestAReopenedIssuesApprovalRequestStillWaitsOnThePersonOnceItsDocumentsSettle(t *testing.T) {
	t.Parallel()
	f := newCloseAsksFixtureSettlingAfter(t, time.Hour)
	ctx := context.Background()
	before := readBlockAsk(t, f.handler, f.approvalAsk)
	if before.State != "open" || before.WaitingOn != "human" {
		t.Fatalf("approval request before the close = %s waiting on %q, want open waiting on human", before.State, before.WaitingOn)
	}

	if closed := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{
		"status": "done",
	}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	// The settlement the close's write leaves owed, on the closed issue, as the resumption, the
	// room's next load or its timer runs it.
	if err := f.documents.SettleIssueDocuments(ctx, f.issue); err != nil {
		t.Fatalf("settle the closed issue's documents: %v", err)
	}
	awaitSettled(t, f.database, f.spec, f.copy)

	settled := readIssueDetail(t, f.handler, f.issue)
	if reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{
		"status": "todo", "expected_sequence": settled.LastSeq,
	}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	// The settlement the reopen's write leaves owed.
	if err := f.documents.SettleIssueDocuments(ctx, f.issue); err != nil {
		t.Fatalf("settle the reopened issue's documents: %v", err)
	}
	awaitSettled(t, f.database, f.spec, f.copy)

	if got := readBlockAsk(t, f.handler, f.approvalAsk); got.State != "open" || got.WaitingOn != "human" {
		t.Fatalf("approval request after the reopen's settlement = %s waiting on %q, want open waiting on human as before the close",
			got.State, got.WaitingOn)
	}
	if approval := readApproval(t, f.handler, f.spec).Approval; approval == nil || approval.State != "awaiting" || approval.WaitingOn != "human" {
		t.Fatalf("the spec's approval after the reopen's settlement = %#v, want awaiting the person", approval)
	}
	if inbox := inboxAskIDs(t, f.handler); !slices.Contains(inbox, f.approvalAsk) {
		t.Fatalf("inbox after the reopen's settlement = %v, want the restored approval request %s", inbox, f.approvalAsk)
	}
}

// closedEditFixture is an issue whose spec holds the one paragraph "Context", with alice's
// browser connected to the spec before the issue closes.
type closedEditFixture struct {
	handler   http.Handler
	database  *store.Store
	documents *docs.Service
	spec      string
	peer      *syncedPeer
}

// replaceFirstParagraph is a browser edit that replaces the text of the document's first
// paragraph, which must be the one text run from, with to.
func replaceFirstParagraph(from, to string) func(*pmdoc.Node) error {
	return func(tree *pmdoc.Node) error {
		if len(tree.Children) == 0 || len(tree.Children[0].Children) != 1 || tree.Children[0].Children[0].Text != from {
			return fmt.Errorf("the first paragraph is not the one text run %q", from)
		}
		tree.Children[0].Children[0].Text = to
		return nil
	}
}

// settleClosedEdit has alice's browser make edit in the spec of a closed issue, waits until the
// store holds it, which leaves the spec owed, and runs the resumption, which settles the spec on
// the closed issue. It answers the spec's latest version once the spec owes nothing.
func settleClosedEdit(t *testing.T, f closedEditFixture, edit func(*pmdoc.Node) error) int {
	t.Helper()
	stored := storedUpdateCount(t, f.database, f.spec)
	f.peer.edit(t, edit)
	f.peer.barrier(t)
	awaitStoredUpdates(t, f.database, f.spec, stored+1)
	if err := f.documents.ResumeOwedSettlementsForTest(context.Background(), 0); err != nil {
		t.Fatalf("resume the closed issue's owed settlements: %v", err)
	}
	awaitSettled(t, f.database, f.spec)
	return readApproval(t, f.handler, f.spec).Approval.LatestVersion
}

// reopenAndSettle reopens issue through PATCH /api/v1/issues/{key} as alice and runs the
// resumption, which settles whatever the reopen left owed, as the room's next load, its timer or
// the resumption does.
func reopenAndSettle(t *testing.T, f closedEditFixture, issue string) {
	t.Helper()
	if reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if err := f.documents.ResumeOwedSettlementsForTest(context.Background(), 0); err != nil {
		t.Fatalf("resume the reopened issue's owed settlements: %v", err)
	}
	awaitSettled(t, f.database, f.spec)
}

// describeApproval is a document's approval as approvalRead holds it, for a failure message.
func describeApproval(read approvalRead) string {
	approval := read.Approval
	if approval == nil {
		return "no approval"
	}
	described := fmt.Sprintf("%s, latest version %d", approval.State, approval.LatestVersion)
	if approval.WaitingOn != "" {
		described += ", waiting on " + approval.WaitingOn
	}
	if approval.Version != nil {
		described += fmt.Sprintf(", reviewed version %d", *approval.Version)
	}
	if approval.By != nil {
		described += " by " + approval.By.ID
	}
	if approval.Reason != nil {
		described += fmt.Sprintf(", reason %q", *approval.Reason)
	}
	return described
}

// A settlement of a closed issue's document moves no approval request (versionWrite.closedIssue),
// so the reopen moves each request it restores to the version such a settlement wrote, as that
// version's own write moves it on an open issue (docs.Service.FollowClosedIssueVersions). The
// spec's approval request waits on alice at version 1. The issue closes, and a browser edit the
// store admits inside the close margin is settled while the issue is closed, into version 2. The
// reopen through PATCH leaves the restored request naming version 2, and waiting on its agent
// since version 2 is not the text alice was asked about, as a settlement of the same edit on an
// open issue leaves it. The reopen moves it with one ask.edited in the name of alice, the editor
// the closed issue's settlement of her edit credited (doc_closed_edit_sources), which is not quiet
// and so wakes the request's agent (events.Broker.Notify), as that settlement's version on an open
// issue would have moved it. alice's approval from the request then reviews the latest version,
// and the spec reads approved.
func TestAReopenedApprovalRequestNamesTheVersionAClosedSettlementWrote(t *testing.T) {
	t.Parallel()
	handler, database, documentService := newClosedSettlementHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approval and a margin edit", "Context\n")
	f := closedEditFixture{handler: handler, database: database, documents: documentService, spec: issue.PrimaryArtifactID}
	awaitSettled(t, database, f.spec)
	request := requestApproval(t, handler, f.spec)
	named := request.Approval.Version
	if before := readBlockAsk(t, handler, request.ID); before.State != "open" || before.WaitingOn != "human" {
		t.Fatalf("approval request before the close = %s waiting on %q, want open waiting on human", before.State, before.WaitingOn)
	}

	f.peer = connectBrowserPeer(t, documentService, f.spec)
	closeIssueThroughPatch(t, handler, database, issue.Key)
	awaitSettled(t, database, f.spec)
	closed := settleClosedEdit(t, f, replaceFirstParagraph("Context", "Context, edited in the margin"))
	if _, written := documentTexts(t, handler, f.spec); closed <= named || !strings.Contains(written, "edited in the margin") {
		t.Fatalf("the closed issue's settlement left the spec at version %d reading:\n%s\nwant a version above the %d the request names, holding the edit",
			closed, written, named)
	}
	if still := readCarriedAsk(t, handler, request.ID); still.Approval.Version != named {
		t.Fatalf("the closed issue's settlement moved the withdrawn request to version %d, want it left at %d", still.Approval.Version, named)
	}

	seq := latestIssueSeq(t, database, issue.Key)
	reopenAndSettle(t, f, issue.Key)
	var restored carriedAsk
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if restored = readCarriedAsk(t, handler, request.ID); restored.Approval.Version == closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the reopen the restored request names version %d (requested %d), want version %d, the one the closed issue's settlement wrote",
				restored.Approval.Version, restored.Approval.RequestedVersion, closed)
		}
	}
	if ask := readBlockAsk(t, handler, request.ID); ask.State != "open" || ask.WaitingOn != "agent" || restored.Approval.RequestedVersion != named {
		t.Fatalf("restored request = %s waiting on %q requested at version %d, want open waiting on its agent, requested at %d, since version %d changed the text",
			ask.State, ask.WaitingOn, restored.Approval.RequestedVersion, named, closed)
	}
	var edits []loggedEvent
	for _, event := range issueEventsAfter(t, database, issue.Key, seq) {
		if event.kind == "ask.edited" {
			edits = append(edits, event)
		}
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	if len(edits) != 1 || edits[0].quiet || !edits[0].notify || !edits[0].actor.SameAs(alice) {
		t.Fatalf("the reopen's ask.edited events = %+v, want one move in the name of %+v that wakes the request's agent", edits, alice)
	}

	review := reviewDocument(t, handler, f.spec, "alice", "approved", "")
	if read := readApproval(t, handler, f.spec); read.Approval == nil || read.Approval.State != "approved" || read.Approval.LatestVersion != closed || review.Version != closed {
		t.Fatalf("alice approved from the restored request: review of version %d, approval %s, want approved at version %d", review.Version, describeApproval(read), closed)
	}
}

// A settlement of a closed issue's document carries no approval (versionWrite.closedIssue), so the
// reopen carries the approval to a version such a settlement wrote that holds the approved text
// again, as that version's own write carries it on an open issue (docs.Service.
// FollowClosedIssueVersions, docs.carryApproval's restore). alice approves the spec at version N,
// and her browser's edit on the open issue makes version N+1, which her approval does not cover.
// The issue closes, her browser's edit inside the close margin writes back version N's text, and
// the settlement on the closed issue writes version N+2 with that text and carries nothing. After
// the reopen through PATCH the spec reads approved at version N+2, carried from her approval of
// version N.
func TestAReopenCarriesAnApprovalToTheApprovedTextAClosedSettlementWroteBack(t *testing.T) {
	t.Parallel()
	handler, database, documentService := newClosedSettlementHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "An approved text written back while closed", "Context\n")
	f := closedEditFixture{handler: handler, database: database, documents: documentService, spec: issue.PrimaryArtifactID}
	awaitSettled(t, database, f.spec)
	approved := reviewDocument(t, handler, f.spec, "alice", "approved", "")
	_, approvedText := documentTexts(t, handler, f.spec)

	f.peer = connectBrowserPeer(t, documentService, f.spec)
	stored := storedUpdateCount(t, database, f.spec)
	f.peer.edit(t, replaceFirstParagraph("Context", "Context, changed while open"))
	f.peer.barrier(t)
	awaitStoredUpdates(t, database, f.spec, stored+1)
	awaitSettledVersionHolding(t, handler, database, f.spec, "changed while open")
	if read := readApproval(t, handler, f.spec); read.Approval == nil || read.Approval.State != "stale" || read.Approval.LatestVersion != approved.Version+1 {
		t.Fatalf("approval after the edit on the open issue = %s, want stale at version %d", describeApproval(read), approved.Version+1)
	}

	closeIssueThroughPatch(t, handler, database, issue.Key)
	awaitSettled(t, database, f.spec)
	closed := settleClosedEdit(t, f, replaceFirstParagraph("Context, changed while open", "Context"))
	if _, written := documentTexts(t, handler, f.spec); closed != approved.Version+2 || written != approvedText {
		t.Fatalf("the closed issue's settlement left version %d reading:\n%s\nwant version %d holding the approved version %d's text:\n%s",
			closed, written, approved.Version+2, approved.Version, approvedText)
	}
	if read := readApproval(t, handler, f.spec); read.Approval == nil || read.Approval.State != "stale" || read.Approval.LatestVersion != closed {
		t.Fatalf("approval after the closed issue's settlement = %s, want stale at version %d: such a settlement carries nothing", describeApproval(read), closed)
	}

	reopenAndSettle(t, f, issue.Key)
	read := readApproval(t, handler, f.spec)
	carried := read.Approval
	if carried == nil || carried.State != "approved" || carried.LatestVersion != closed || carried.Version == nil || *carried.Version != closed ||
		carried.By == nil || carried.By.ID != "alice" || carried.Reason == nil ||
		!strings.HasPrefix(*carried.Reason, fmt.Sprintf("Carried forward from the approval of version %d:", approved.Version)) {
		t.Fatalf("approval after the reopen = %s, want approved at version %d by alice, carried from her approval of version %d",
			describeApproval(read), closed, approved.Version)
	}
}

// loggedEvent is one event of an issue as the event log stores it: its type, its actor, whether
// it notifies, the id its payload names (an ask event's ask), whether its payload marks it quiet
// (model.AskEditEventPayload.Quiet), and the editor an ask.edited payload names
// (model.AskEditEventPayload.EditedBy), zero for any other event.
type loggedEvent struct {
	kind     string
	actor    model.Actor
	notify   bool
	id       string
	quiet    bool
	editedBy model.Actor
}

// issueEventsAfter is every event of issue whose sequence is above seq (latestIssueSeq), oldest
// first.
func issueEventsAfter(t *testing.T, database *store.Store, issue string, seq int) []loggedEvent {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select type, actor, notify, coalesce(payload->>'id', ''), coalesce((payload->>'quiet')::boolean, false), payload->'edited_by'
		from events where issue_key = $1 and seq > $2 order by seq
	`, issue, seq)
	if err != nil {
		t.Fatalf("read the events of %s: %v", issue, err)
	}
	defer rows.Close()
	var logged []loggedEvent
	for rows.Next() {
		var event loggedEvent
		var actor, editedBy []byte
		if err := rows.Scan(&event.kind, &actor, &event.notify, &event.id, &event.quiet, &editedBy); err != nil {
			t.Fatalf("read an event of %s: %v", issue, err)
		}
		if err := json.Unmarshal(actor, &event.actor); err != nil {
			t.Fatalf("decode an event's actor: %v", err)
		}
		if editedBy != nil {
			if err := json.Unmarshal(editedBy, &event.editedBy); err != nil {
				t.Fatalf("decode an ask.edited event's edited_by: %v", err)
			}
		}
		logged = append(logged, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the events of %s: %v", issue, err)
	}
	return logged
}

// eventKinds is the type of each of logged, in order.
func eventKinds(logged []loggedEvent) []string {
	kinds := make([]string, len(logged))
	for index, event := range logged {
		kinds[index] = event.kind
	}
	return kinds
}

// A reopen that moves a restored request to a version a settlement wrote while the issue was
// closed keeps the request with the person when that version stores byte for byte the text the
// person was asked about (docs.MoveApprovalAsk), and the move is quiet, as a version of that text
// written on an open issue keeps it. The spec's request waits on alice at version N. While the
// issue is closed her browser edits the spec and then undoes the edit, each settled on the closed
// issue: version N+1 holds the edit, and version N+2 holds version N's text again. After the
// reopen through PATCH the request names version N+2 and still waits on alice, and the reopen
// moved it with one quiet ask.edited, which notifies nobody, in the name of alice, the editor the
// closed issue's settlement of her undo credited (doc_closed_edit_sources), as that settlement's
// version on an open issue would have moved it.
func TestAReopenKeepsTheRequestWithThePersonWhenAClosedSettlementWroteBackItsText(t *testing.T) {
	t.Parallel()
	handler, database, documentService := newClosedSettlementHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A requested text written back while closed", "Context\n")
	f := closedEditFixture{handler: handler, database: database, documents: documentService, spec: issue.PrimaryArtifactID}
	awaitSettled(t, database, f.spec)
	request := requestApproval(t, handler, f.spec)
	named := request.Approval.Version
	_, namedText := documentTexts(t, handler, f.spec)

	f.peer = connectBrowserPeer(t, documentService, f.spec)
	closeIssueThroughPatch(t, handler, database, issue.Key)
	awaitSettled(t, database, f.spec)
	if edited := settleClosedEdit(t, f, replaceFirstParagraph("Context", "Context, edited in the margin")); edited != named+1 {
		t.Fatalf("the closed issue's settlement of the edit left the spec at version %d, want %d", edited, named+1)
	}
	closed := settleClosedEdit(t, f, replaceFirstParagraph("Context, edited in the margin", "Context"))
	if _, written := documentTexts(t, handler, f.spec); closed != named+2 || written != namedText {
		t.Fatalf("the closed issue's settlement of the undo left version %d reading:\n%s\nwant version %d holding the requested version %d's text:\n%s",
			closed, written, named+2, named, namedText)
	}

	seq := latestIssueSeq(t, database, issue.Key)
	reopenAndSettle(t, f, issue.Key)
	restored := readCarriedAsk(t, handler, request.ID)
	if ask := readBlockAsk(t, handler, request.ID); ask.State != "open" || ask.WaitingOn != "human" ||
		restored.Approval.Version != closed || restored.Approval.RequestedVersion != closed {
		t.Fatalf("restored request = %s waiting on %q at version %d requested %d, want open waiting on the person at version %d requested %d",
			ask.State, ask.WaitingOn, restored.Approval.Version, restored.Approval.RequestedVersion, closed, closed)
	}
	if read := readApproval(t, handler, f.spec); read.Approval == nil || read.Approval.State != "awaiting" || read.Approval.WaitingOn != "human" {
		t.Fatalf("the spec's approval after the reopen = %s, want awaiting the person", describeApproval(read))
	}
	var edits []loggedEvent
	for _, event := range issueEventsAfter(t, database, issue.Key, seq) {
		if event.kind == "ask.edited" {
			edits = append(edits, event)
		}
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	if len(edits) != 1 || !edits[0].quiet || edits[0].notify || !edits[0].actor.SameAs(alice) {
		t.Fatalf("the reopen's ask.edited events = %+v, want one quiet move in the name of %+v that notifies nobody", edits, alice)
	}
}

// Only a settlement's version keeps an approval request waiting on the person when it stores byte
// for byte the text the request names (docs.MoveApprovalAsk's bySettlement). A version a route
// writes moves the request to its agent whatever text it stores, as every move did before the
// rule, since the Legion daemon answers the agent's gate wait with that version's
// artifact.version as the request handed back: a person's named version of the spec with no
// edit, and the agent's upload of the spec's unchanged text, each leave the request waiting on its
// agent at the new version, requested at the named one, with a move that is not quiet. A
// settlement's version of the named text keeps the request with the person: the spec holds a
// decision the close withdraws and writes resolved, the settlement on the closed issue saves that,
// the reopen restores the decision and writes it open, and the settlement it leaves owed writes the
// named version's text again, which leaves the request waiting on the person, now naming and
// requesting that version. The settle delay is an hour, so the test runs each settlement itself.
func TestOnlyASettlementsVersionKeepsAnApprovalRequestWithThePerson(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		spec string
		// write writes the case's version of the text the request names.
		write     func(t *testing.T, c reopenCarryCase)
		keepsTurn bool
	}{
		{
			name: "a person's named version",
			spec: "Context\n",
			write: func(t *testing.T, c reopenCarryCase) {
				if named := dispatchRequest(t, c.handler, http.MethodPost, "/api/v1/artifacts/"+c.spec+"/versions", map[string]any{"summary": "checkpoint while reviewing"}, "alice"); named.Code != http.StatusCreated {
					t.Fatalf("name a version: status=%d body=%s", named.Code, named.Body.String())
				}
			},
		},
		{
			name: "an upload of the unchanged text",
			spec: "Context\n",
			write: func(t *testing.T, c reopenCarryCase) {
				_, text := documentTexts(t, c.handler, c.spec)
				if text == "" {
					t.Fatal("the spec's latest version reads no text to upload again")
				}
				// The upload replaces the spec the issue already holds, so it names the token of the
				// version it replaces, as every replacement of this server must.
				if uploaded := sessionRequest(t, c.handler, http.MethodPost, "/api/v1/issues/"+c.issue+"/artifacts", map[string]any{
					"actor": sessionActor(), "name": "spec.md", "content": text,
					"precondition": uploadBase(t, c.handler, c.spec),
				}); uploaded.Code != http.StatusCreated {
					t.Fatalf("upload the spec's text again: status=%d body=%s", uploaded.Code, uploaded.Body.String())
				}
			},
		},
		{
			name:      "a settlement's version after a reopen",
			spec:      reopenDecisionSpec,
			keepsTurn: true,
			write: func(t *testing.T, c reopenCarryCase) {
				closeArtifactIssue(t, c.handler, c.issue)
				c.settle(t, "the closed issue's spec")
				if reopened := dispatchRequest(t, c.handler, http.MethodPatch, "/api/v1/issues/"+c.issue, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
					t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
				}
				c.settle(t, "the reopened issue's spec")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, database, documents := settlingCarryDocuments(t, time.Hour)
			issue := createInteractionIssue(t, handler, "TEST", "A version of the requested text: "+test.name, test.spec)
			c := reopenCarryCase{handler: handler, database: database, documents: documents, issue: issue.Key, spec: issue.PrimaryArtifactID}
			c.settle(t, "the spec")
			request := requestApproval(t, handler, c.spec)
			named := request.Approval.Version
			_, namedText := documentTexts(t, handler, c.spec)
			if before := readApproval(t, handler, c.spec); before.Approval == nil || before.Approval.WaitingOn != "human" || namedText == "" {
				t.Fatalf("before the version the spec's approval = %s and version %d reads %q, want the request waiting on the person over a text", describeApproval(before), named, namedText)
			}
			seq := latestIssueSeq(t, database, c.issue)

			test.write(t, c)
			_, written := documentTexts(t, handler, c.spec)
			latest := latestVersion(t, database, c.spec).Number
			if latest <= named || written != namedText {
				t.Fatalf("the spec's latest version is %d reading:\n%s\nwant a version above %d holding its text:\n%s", latest, written, named, namedText)
			}
			moved := readCarriedAsk(t, handler, request.ID)
			read := readApproval(t, handler, c.spec)
			var edits []loggedEvent
			for _, event := range issueEventsAfter(t, database, c.issue, seq) {
				if event.kind == "ask.edited" {
					edits = append(edits, event)
				}
			}
			if len(edits) == 0 || read.Approval == nil || read.Approval.State != "awaiting" || moved.Approval.Version != latest {
				t.Fatalf("after version %d the request names version %d with ask.edited events %+v, and the approval reads %s; want the request moved to version %d and awaiting approval",
					latest, moved.Approval.Version, edits, describeApproval(read), latest)
			}
			if test.keepsTurn {
				if read.Approval.WaitingOn != "human" || moved.Approval.RequestedVersion != latest || !edits[len(edits)-1].quiet {
					t.Fatalf("after a settlement's version of the named text the request waits on %q requested at version %d, last move %+v; want it waiting on the person, requested at version %d, moved quietly",
						read.Approval.WaitingOn, moved.Approval.RequestedVersion, edits[len(edits)-1], latest)
				}
			} else if read.Approval.WaitingOn != "agent" || moved.Approval.RequestedVersion != named || edits[len(edits)-1].quiet {
				t.Fatalf("after a route's version of the named text the request waits on %q requested at version %d, last move %+v; want it waiting on its agent, requested at version %d, moved with a move that is not quiet",
					read.Approval.WaitingOn, moved.Approval.RequestedVersion, edits[len(edits)-1], named)
			}
		})
	}
}

// A reopen after which every document's latest version is one written while the issue was open
// moves no request and carries no approval: each such version's own write already moved the
// request (docs.Service.FollowClosedIssueVersions). The spec's request waits on alice at version
// N, and her browser's edit on the open issue makes version N+1, which moves the request onto its
// agent's turn. The issue closes, with nothing for a settlement to save, and reopens through
// PATCH: the reopen appends the restored request's ask.opened and its own issue.updated and
// nothing else, and the request names version N+1, requested at N, as before the close.
func TestAReopenWithNoClosedVersionMovesAndCarriesNothing(t *testing.T) {
	t.Parallel()
	handler, database, documentService := newClosedSettlementHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A reopen with no closed version", "Context\n")
	spec := issue.PrimaryArtifactID
	awaitSettled(t, database, spec)
	request := requestApproval(t, handler, spec)
	named := request.Approval.Version
	peer := connectBrowserPeer(t, documentService, spec)
	stored := storedUpdateCount(t, database, spec)
	peer.edit(t, replaceFirstParagraph("Context", "Context, changed while open"))
	peer.barrier(t)
	awaitStoredUpdates(t, database, spec, stored+1)
	awaitSettledVersionHolding(t, handler, database, spec, "changed while open")
	moved := readCarriedAsk(t, handler, request.ID)
	if moved.Approval.Version != named+1 || moved.Approval.RequestedVersion != named {
		t.Fatalf("the request after the edit on the open issue names version %d requested %d, want %d requested %d",
			moved.Approval.Version, moved.Approval.RequestedVersion, named+1, named)
	}

	closeIssueThroughPatch(t, handler, database, issue.Key)
	awaitSettled(t, database, spec)
	if latest := readApproval(t, handler, spec).Approval.LatestVersion; latest != named+1 {
		t.Fatalf("the spec is at version %d after the close, want %d: the close left nothing to save", latest, named+1)
	}
	seq := latestIssueSeq(t, database, issue.Key)
	if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	awaitSettled(t, database, spec)
	if kinds := eventKinds(issueEventsAfter(t, database, issue.Key, seq)); !slices.Equal(kinds, []string{"ask.opened", "issue.updated"}) {
		t.Fatalf("the reopen appended %v, want only the restored request's ask.opened and the issue.updated", kinds)
	}
	if after := readCarriedAsk(t, handler, request.ID); after.Approval.Version != named+1 || after.Approval.RequestedVersion != named {
		t.Fatalf("the restored request names version %d requested %d, want %d requested %d as before the close",
			after.Approval.Version, after.Approval.RequestedVersion, named+1, named)
	}
}

// An approval a version's own write already carried to the latest version is carried again by no
// reopen: the reopen runs the carry only while the newest review approves a version before the
// latest one (docs.Service.FollowClosedIssueVersions). alice approves the spec at version N, her
// browser's edit on the open issue makes version N+1, and her undo makes version N+2 with
// version N's text, whose own write carries her approval to it. The issue closes and reopens
// through PATCH: the spec still reads approved at version N+2 with the reviews it had, and the
// reopen appends no artifact.approved, only its own issue.updated.
func TestAReopenWritesNoSecondReviewForAnApprovalAlreadyCarriedToTheLatestVersion(t *testing.T) {
	t.Parallel()
	handler, database, documentService := newClosedSettlementHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "An approval carried before the close", "Context\n")
	spec := issue.PrimaryArtifactID
	awaitSettled(t, database, spec)
	approved := reviewDocument(t, handler, spec, "alice", "approved", "")
	peer := connectBrowserPeer(t, documentService, spec)
	for _, edit := range []struct{ from, to, settled string }{
		{"Context", "Context, changed while open", "changed while open"},
		{"Context, changed while open", "Context", "Context"},
	} {
		stored := storedUpdateCount(t, database, spec)
		peer.edit(t, replaceFirstParagraph(edit.from, edit.to))
		peer.barrier(t)
		awaitStoredUpdates(t, database, spec, stored+1)
		awaitSettledVersionHolding(t, handler, database, spec, edit.settled)
	}
	carried := approved.Version + 2
	if read := readApproval(t, handler, spec); read.Approval == nil || read.Approval.State != "approved" || read.Approval.LatestVersion != carried ||
		read.Approval.Version == nil || *read.Approval.Version != carried {
		t.Fatalf("approval after the undo on the open issue = %s, want approved at version %d by its own write's carry", describeApproval(read), carried)
	}
	reviews := artifactReviewCount(t, database, spec)

	closeIssueThroughPatch(t, handler, database, issue.Key)
	awaitSettled(t, database, spec)
	seq := latestIssueSeq(t, database, issue.Key)
	if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	awaitSettled(t, database, spec)
	if kinds := eventKinds(issueEventsAfter(t, database, issue.Key, seq)); !slices.Equal(kinds, []string{"issue.updated"}) {
		t.Fatalf("the reopen appended %v, want only its issue.updated", kinds)
	}
	if got := artifactReviewCount(t, database, spec); got != reviews {
		t.Fatalf("the spec has %d reviews after the reopen, want the %d it had", got, reviews)
	}
	if read := readApproval(t, handler, spec); read.Approval == nil || read.Approval.State != "approved" || read.Approval.Version == nil || *read.Approval.Version != carried {
		t.Fatalf("approval after the reopen = %s, want approved at version %d", describeApproval(read), carried)
	}
}

// artifactReviewCount is how many reviews of document artifact_reviews holds.
func artifactReviewCount(t *testing.T, database *store.Store, document string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_reviews where artifact_id = $1`, document).Scan(&count); err != nil {
		t.Fatalf("count the reviews of %s: %v", document, err)
	}
	return count
}

// reopenDecisionSpec is the spec of
// TestAnApprovalGivenBetweenTheReopenAndItsSettlementStaysApproved: a decision the close withdraws
// and the reopen restores.
const reopenDecisionSpec = "Context\n\n" +
	":::ask{#reopen-decision urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// A person's approval of a document given after its issue reopens, before the settlement the
// reopen leaves owed has run, still stands once that settlement has written its version, as it
// stood before a closed issue's document settled. The spec holds a decision; the close withdraws
// it and writes its block resolved, and the settlement on the closed issue saves that as version
// L. The reopen through PATCH restores the decision and writes its block open, which leaves the
// spec owed, and alice approves the spec from its header before that settlement runs: there is no
// approval request, and her review pins L, the latest version. The settlement then writes L+1,
// which differs from L only in the block the reopen wrote open again, and the spec reads approved
// by alice at L+1, carried from her approval of L (docs.reopenedApproval). Before a closed issue's
// document settled, no version came of the close, alice's review pinned the version from before
// it, and the settlement after the reopen, which rendered that version's text again, wrote none,
// so her approval stood. The settle delay is an hour, so the test runs each settlement itself
// (SettleIssueDocuments), and nothing settles between the reopen and alice's review.
func TestAnApprovalGivenBetweenTheReopenAndItsSettlementStaysApproved(t *testing.T) {
	t.Parallel()
	handler, database, documentService := settlingCarryDocuments(t, time.Hour)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "TEST", "An approval between the reopen and its settlement", reopenDecisionSpec)
	spec := issue.PrimaryArtifactID
	settle := func(what string) {
		t.Helper()
		if err := documentService.SettleIssueDocuments(ctx, issue.Key); err != nil {
			t.Fatalf("settle %s: %v", what, err)
		}
		awaitSettled(t, database, spec)
	}
	settle("the spec")
	awaitIndexedAskBlock(t, handler, spec, "reopen-decision", "Ship it?")
	before := latestVersion(t, database, spec).Number

	closeArtifactIssue(t, handler, issue.Key)
	settle("the closed issue's spec")
	closed := latestVersion(t, database, spec).Number
	if _, written := documentTexts(t, handler, spec); closed <= before || !strings.Contains(written, `#reopen-decision urgency="med" multiple="false" state="resolved"`) {
		t.Fatalf("the closed issue's settlement left the spec at version %d reading:\n%s\nwant a version above %d showing the withdrawn decision resolved", closed, written, before)
	}

	if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if !documentOwesSettlement(t, database, spec) {
		t.Fatal("the reopen left the spec owing no settlement: this test is for the window before that settlement")
	}
	if review := reviewDocument(t, handler, spec, "alice", "approved", ""); review.Version != closed {
		t.Fatalf("alice's approval from the spec's header reviews version %d, want %d, the latest version while the reopen's settlement is owed", review.Version, closed)
	}

	settle("the reopened issue's spec")
	reopened := latestVersion(t, database, spec).Number
	if _, written := documentTexts(t, handler, spec); reopened != closed+1 || !strings.Contains(written, `#reopen-decision urgency="med" multiple="false" state="open"`) {
		t.Fatalf("the reopened issue's settlement left the spec at version %d reading:\n%s\nwant version %d showing the restored decision open", reopened, written, closed+1)
	}
	if read := readApproval(t, handler, spec); read.Approval == nil || read.Approval.State != "approved" || read.Approval.LatestVersion != reopened ||
		read.Approval.Version == nil || *read.Approval.Version != reopened || read.Approval.By == nil || read.Approval.By.ID != "alice" {
		t.Fatalf("the spec's approval after the reopen's settlement = %s, want approved by alice at version %d", describeApproval(read), reopened)
	}
}

// reopenCarryCase is one issue of TestAReopenCarriesAnApprovalOnlyOverTheAsksTheCloseWithdrew,
// served with a settle delay of an hour, so the test runs each settlement itself.
type reopenCarryCase struct {
	handler   http.Handler
	database  *store.Store
	documents *docs.Service
	issue     string
	spec      string
}

// settle runs the settlement each document of the case's issue owes (SettleIssueDocuments), what
// naming it, and waits until the spec owes none.
func (c reopenCarryCase) settle(t *testing.T, what string) {
	t.Helper()
	if err := c.documents.SettleIssueDocuments(context.Background(), c.issue); err != nil {
		t.Fatalf("settle %s: %v", what, err)
	}
	awaitSettled(t, c.database, c.spec)
}

// edit has peer make change in the case's spec and waits until the store holds it.
func (c reopenCarryCase) edit(t *testing.T, peer *syncedPeer, change func(*pmdoc.Node) error) {
	t.Helper()
	stored := storedUpdateCount(t, c.database, c.spec)
	peer.edit(t, change)
	peer.barrier(t)
	awaitStoredUpdates(t, c.database, c.spec, stored+1)
}

// requireRetracted fails t unless the ask of the case's spec block blockID is one a settlement
// retracted.
func (c reopenCarryCase) requireRetracted(t *testing.T, blockID, when string) {
	t.Helper()
	ask := readBlockAsk(t, c.handler, requireBlockAsk(t, c.handler, c.issue, c.spec, blockID))
	if ask.State != "resolved" || ask.Resolution == nil || ask.Resolution.Kind != "retracted" {
		t.Fatalf("%s the ask of block %s reads %s %+v, want retracted", when, blockID, ask.State, ask.Resolution)
	}
}

// A reopen carries an approval of a closed issue's version over the asks the close withdrew and
// the reopen restored, and over no other question (docs.reopenedApproval). In each case alice
// approves the spec from its header between the reopen through PATCH and the settlement the reopen
// leaves owed, so her review pins A, the version a settlement wrote while the issue was closed, in
// which the case's block reads resolved; that settlement then writes A+1, which differs from A
// only in the block, open again. Her approval carries to A+1 when the block's ask is one the close
// withdrew. It stands at A, stale, when the block's ask was opened or restored after A: the block
// alice's browser types inside the close margin has no ask until the settlement after the reopen
// opens one, and the block of an ask a settlement retracted before the close, which her browser
// brings back inside the margin, stays retracted until that settlement restores it. Neither
// question was open when the issue closed, so A+1 asks something no approval of alice's covered.
// The settle delay is an hour, so the test runs each settlement itself (SettleIssueDocuments), and
// nothing settles between the reopen and alice's review.
func TestAReopenCarriesAnApprovalOnlyOverTheAsksTheCloseWithdrew(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		spec    string
		block   string
		carries bool
		// closed is the case's closed period, from its spec settled on the open issue to the
		// settlement on the closed issue that writes A, the spec's latest version once it returns.
		closed func(t *testing.T, c reopenCarryCase)
	}{
		{
			name:    "an ask the close withdrew",
			spec:    reopenDecisionSpec,
			block:   "reopen-decision",
			carries: true,
			closed: func(t *testing.T, c reopenCarryCase) {
				awaitIndexedAskBlock(t, c.handler, c.spec, "reopen-decision", "Ship it?")
				closeArtifactIssue(t, c.handler, c.issue)
				if ask := readBlockAsk(t, c.handler, requireBlockAsk(t, c.handler, c.issue, c.spec, "reopen-decision")); ask.Resolution == nil || ask.Resolution.Kind != model.AskResolutionWithdrawn {
					t.Fatalf("the close left the decision's ask %s %+v, want it withdrawn", ask.State, ask.Resolution)
				}
				c.settle(t, "the closed issue's spec")
			},
		},
		{
			name:  "a block with no ask at the close",
			spec:  "Context\n",
			block: "margin-decision",
			closed: func(t *testing.T, c reopenCarryCase) {
				peer := connectBrowserPeer(t, c.documents, c.spec)
				closeIssueThroughPatch(t, c.handler, c.database, c.issue)
				c.settle(t, "what the close left owed")
				c.edit(t, peer, func(tree *pmdoc.Node) error {
					typed, err := pmdoc.Parse(":::ask{#margin-decision urgency=\"med\" multiple=\"false\"}\nShip without the rollback path?\n:::\n")
					if err != nil {
						return err
					}
					tree.Children = append(tree.Children, typed.Children...)
					return nil
				})
				c.settle(t, "the closed issue's spec")
				if id := findBlockAsk(t, c.handler, c.issue, c.spec, "margin-decision"); id != "" {
					t.Fatalf("the block typed inside the close margin is ask %s on the closed issue, want no ask", id)
				}
			},
		},
		{
			name: "an ask a settlement retracted before the close",
			spec: "Context\n\n" +
				":::ask{#retracted-decision urgency=\"med\" multiple=\"false\"}\nShip it on Friday?\n\n- Yes: Now.\n- No: Later.\n:::\n",
			block: "retracted-decision",
			closed: func(t *testing.T, c reopenCarryCase) {
				awaitIndexedAskBlock(t, c.handler, c.spec, "retracted-decision", "Ship it on Friday?")
				peer := connectBrowserPeer(t, c.documents, c.spec)
				var taken *pmdoc.Node
				c.edit(t, peer, func(tree *pmdoc.Node) error {
					kept := tree.Children[:0]
					for _, child := range tree.Children {
						if child.Type == "ask" {
							taken = child
							continue
						}
						kept = append(kept, child)
					}
					tree.Children = kept
					if taken == nil {
						return errors.New("the spec holds no decision block to delete")
					}
					return nil
				})
				c.settle(t, "the deletion of the decision's block")
				c.requireRetracted(t, "retracted-decision", "once its block was deleted")
				closeIssueThroughPatch(t, c.handler, c.database, c.issue)
				c.settle(t, "what the close left owed")
				c.edit(t, peer, func(tree *pmdoc.Node) error {
					tree.Children = append(tree.Children, taken)
					return nil
				})
				c.settle(t, "the closed issue's spec")
				c.requireRetracted(t, "retracted-decision", "on the closed issue")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, database, documents := closedSettlementHandlerSettlingAfter(t, time.Hour)
			issue := createInteractionIssue(t, handler, "TEST", "A reopen carry: "+test.name, test.spec)
			c := reopenCarryCase{handler: handler, database: database, documents: documents, issue: issue.Key, spec: issue.PrimaryArtifactID}
			c.settle(t, "the spec")
			test.closed(t, c)
			closed := latestVersion(t, database, c.spec).Number
			if _, written := documentTexts(t, handler, c.spec); !strings.Contains(written, `#`+test.block+` urgency="med" multiple="false" state="resolved"`) {
				t.Fatalf("the closed issue's settlement left the spec at version %d reading:\n%s\nwant block %s resolved", closed, written, test.block)
			}

			if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+c.issue, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
				t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
			}
			if !documentOwesSettlement(t, database, c.spec) {
				t.Fatal("the reopen left the spec owing no settlement: this test is for the window before that settlement")
			}
			if review := reviewDocument(t, handler, c.spec, "alice", "approved", ""); review.Version != closed {
				t.Fatalf("alice's approval from the spec's header reviews version %d, want %d, the latest version while the reopen's settlement is owed", review.Version, closed)
			}

			c.settle(t, "the reopened issue's spec")
			reopened := latestVersion(t, database, c.spec).Number
			if _, written := documentTexts(t, handler, c.spec); reopened != closed+1 || !strings.Contains(written, `#`+test.block+` urgency="med" multiple="false" state="open"`) {
				t.Fatalf("the reopened issue's settlement left the spec at version %d reading:\n%s\nwant version %d showing block %s open", reopened, written, closed+1, test.block)
			}
			if ask := readBlockAsk(t, handler, requireBlockAsk(t, handler, c.issue, c.spec, test.block)); ask.State != "open" {
				t.Fatalf("after the reopened issue's settlement the ask of block %s reads %s, want open", test.block, ask.State)
			}
			read := readApproval(t, handler, c.spec)
			if read.Approval == nil || read.Approval.LatestVersion != reopened || read.Approval.Version == nil || read.Approval.By == nil || read.Approval.By.ID != "alice" {
				t.Fatalf("the spec's approval after the reopened issue's settlement = %s, want alice's review read against latest version %d", describeApproval(read), reopened)
			}
			if test.carries {
				if read.Approval.State != "approved" || *read.Approval.Version != reopened || read.Approval.Reason == nil ||
					!strings.HasPrefix(*read.Approval.Reason, fmt.Sprintf("Carried forward from the approval of version %d:", closed)) {
					t.Fatalf("the spec's approval = %s, want approved at version %d, carried from alice's approval of version %d", describeApproval(read), reopened, closed)
				}
			} else if read.Approval.State != "stale" || *read.Approval.Version != closed {
				t.Fatalf("the spec's approval = %s, want alice's approval of version %d standing stale: version %d asks a question her approval never covered",
					describeApproval(read), closed, reopened)
			}
		})
	}
}
