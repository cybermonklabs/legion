package api

import (
	"context"
	"encoding/json"
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
// (restoreIssueAsksTx). The settlements the closed period and the reopen leave owed save the
// blocks the close and the reopen wrote, and nothing else: neither is a change the request's agent
// made to the text it handed the person. So after both have run, the request still waits on the
// person, the spec still reads awaiting approval from the person, and the request is still in the
// person's Inbox. The browser suite drives the same path (e2e/close-withdraws-asks.e2e.ts, whose
// "Waiting on you (3)" counts this request).
func TestAReopenedIssuesApprovalRequestStillWaitsOnThePersonOnceItsDocumentsSettle(t *testing.T) {
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
// open issue leaves it. alice's approval from the request then reviews the latest version, and
// the spec reads approved.
func TestAReopenedApprovalRequestNamesTheVersionAClosedSettlementWrote(t *testing.T) {
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
// it notifies, and whether its payload marks it quiet (model.AskEditEventPayload.Quiet).
type loggedEvent struct {
	kind   string
	actor  model.Actor
	notify bool
	quiet  bool
}

// lastIssueSeq is the sequence of issue's newest event.
func lastIssueSeq(t *testing.T, database *store.Store, issue string) int {
	t.Helper()
	var seq int
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(seq), 0) from events where issue_key = $1`, issue).Scan(&seq); err != nil {
		t.Fatalf("read the newest event of %s: %v", issue, err)
	}
	return seq
}

// issueEventsAfter is every event of issue whose sequence is above seq, oldest first.
func issueEventsAfter(t *testing.T, database *store.Store, issue string, seq int) []loggedEvent {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select type, actor, notify, coalesce((payload->>'quiet')::boolean, false)
		from events where issue_key = $1 and seq > $2 order by seq
	`, issue, seq)
	if err != nil {
		t.Fatalf("read the events of %s: %v", issue, err)
	}
	defer rows.Close()
	var logged []loggedEvent
	for rows.Next() {
		var event loggedEvent
		var actor []byte
		if err := rows.Scan(&event.kind, &actor, &event.notify, &event.quiet); err != nil {
			t.Fatalf("read an event of %s: %v", issue, err)
		}
		if err := json.Unmarshal(actor, &event.actor); err != nil {
			t.Fatalf("decode an event's actor: %v", err)
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
// moved it with one quiet ask.edited in the settlement's name, which notifies nobody.
func TestAReopenKeepsTheRequestWithThePersonWhenAClosedSettlementWroteBackItsText(t *testing.T) {
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

	seq := lastIssueSeq(t, database, issue.Key)
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
	if len(edits) != 1 || !edits[0].quiet || edits[0].notify || !edits[0].actor.SameAs(docs.SettlementActor) {
		t.Fatalf("the reopen's ask.edited events = %+v, want one quiet move in the name of %+v that notifies nobody", edits, docs.SettlementActor)
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
	seq := lastIssueSeq(t, database, issue.Key)
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
	seq := lastIssueSeq(t, database, issue.Key)
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
