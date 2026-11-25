//go:build !race

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The xid window tests (INFRA-636, plan test T1) drive each write route through the real handler
// on a fixture the size of the load harness's large specs, and judge the transaction the request
// ran with storetest's xid rig: from the statement that gave the transaction its xid to its
// COMMIT, no statement-free gap over 5 ms, and no more time in gaps than in statements (plan D6).
// Every measurement declares the shape of the window it shows (storetest.XidShape): the most
// statements that window may hold and what they are, which the rig holds it to in every build -
// storetest.XidBatchedWindow for a route whose write phase is one queued batch, and
// storetest.XidWindowOf for one whose window legitimately holds more. A route that runs an
// advisory read in its transaction (write advice, the claim's Legion-managed walk) is also held
// to running none inside the window (refuseAdvisoryReadsInXidWindow): those reads are queries,
// back to back, which D6 alone would pass. Each request runs under a marked context
// (storetest.MarkXid), so only the transactions the request itself ran are judged; the server's
// background work shares the pool but not the verdict. The server settles documents after an
// hour, so no settlement races the measured request, except where a route needs settlement to
// have run first (the block ask). The fixture itself is xid_fixture_test.go's.
//
// This file, the route window table's (route_xid_window_test.go and the files of its rows),
// issue_ask_repair_xid_window_test.go and delivery's xid_window_test.go build without the race
// detector (`//go:build !race`). Under it the rig judges no gap (storetest.XidJudgesGaps) and the
// shape is the same rule in either build, so a race run of these tests proved nothing the plain
// run does not, at about four times the plain run's cost: envoy-go's step "Test what the race
// build cannot run at all or at full size" runs them, judging shape and gaps both.
//
// Seeding the fixture is most of what a window test costs: twenty anchored writes on a 40 KB
// spec, each of which renders the spec, about fifteen seconds of a race-built run on a two-core
// pod against well under one for the measured request. So the measurements run as steps on as
// few fixtures as their requests allow (runXidSteps), each step a subtest that seeds what its
// own request needs, waits for the database to fall quiet and measures that request alone. A
// step anchors its writes on a spare cell of its own (takeSpare) and acts on a seed item no
// other step touches, so it meets the fixture as a fixture of its own would show it, plus the
// rows the steps before it added beside its own; and the steps of a run are ordered so that no
// step meets a state that would send its request down another path: the anchored writes before
// the spec is versioned, the versions before an approval request is open (every version moves an
// open request, a path of its own on the route window table), and the claim last. A measurement
// whose request needs what no run can leave it - a spec its approver approved, a request over two
// documents, a document store that fails a load, an ask a step before it closed - keeps a fixture
// of its own.

// xidStep is one measurement of a run of steps on one fixture (runXidSteps): the subtest's name,
// and the step itself, which seeds what its request needs on the fixture the steps before it
// left and measures that request.
type xidStep struct {
	name    string
	measure func(t *testing.T, f *xidFixture)
}

// runXidSteps runs steps in order on f, each as a subtest. Before each step it waits for the
// database to fall quiet (awaitQuietDatabase) - and where settled, for every document to settle
// as well - so the work the steps before it left running, a mention's send or a version's
// settlement, has finished and the step measures its request alone. A failed step does not stop
// the run: a window's verdict is read after its request answered, so the fixture holds what that
// request wrote and each later step still answers for its own window alone; a step whose request
// was refused fails as a rig problem, which names it.
func runXidSteps(t *testing.T, f *xidFixture, settled bool, steps []xidStep) {
	t.Helper()
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			awaitQuietDatabase(t, f.database, settled)
			step.measure(t, f)
		})
	}
}

// revise moves the spec's revision token on by one in f.spec, and answers the token as it stood
// and as it now stands: each step that versions the spec moves the token from where the step
// before it left it, so the version it writes changes one line of the spec as a version of a
// fixture of its own would.
func (f *xidFixture) revise() (from, to string) {
	from = fmt.Sprintf("Revision token r%d", f.revision)
	f.revision++
	to = fmt.Sprintf("Revision token r%d", f.revision)
	f.spec = strings.Replace(f.spec, from, to, 1)
	return from, to
}

// measure runs one request through the fixture's handler under a marked context and judges the
// transactions it ran against shape, the window the route is declared to hold. A request that
// does not answer want is the rig's failure, not a finding, and stops the test before any
// verdict.
func (f *xidFixture) measure(t *testing.T, want int, shape storetest.XidShape, request func(http.Handler) *httptest.ResponseRecorder) *storetest.XidReport {
	t.Helper()
	marked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handler.ServeHTTP(w, r.WithContext(storetest.MarkXid(r.Context())))
	})
	var response *httptest.ResponseRecorder
	report := f.tracer.Measure(t, f.database.Pool, func() { response = request(marked) })
	if response.Code != want {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the measured request answered %d, want %d: %s", response.Code, want, response.Body.String())
	}
	storetest.AssertXidWindows(t, report, shape)
	return report
}

// measureAdvised is measure for a write that runs an advisory read (advisoryStatement), which also
// refuses that read inside the xid window.
func (f *xidFixture) measureAdvised(t *testing.T, want int, shape storetest.XidShape, request func(http.Handler) *httptest.ResponseRecorder) {
	t.Helper()
	refuseAdvisoryReadsInXidWindow(t, f.measure(t, want, shape, request))
}

// measureBatched is measure for a write whose route runs the two phases as one batch
// (write_queue.go), so its window is the shape the plan asks of every route that queues its
// writes: that batch and the COMMIT ending it (storetest.XidBatchedWindow).
func (f *xidFixture) measureBatched(t *testing.T, want int, request func(http.Handler) *httptest.ResponseRecorder) {
	t.Helper()
	f.measure(t, want, storetest.XidBatchedWindow, request)
}

// advisoryStatement opens the savepoint of each advisory read a write runs in its transaction: a
// session's write advice (writeAdvice, write_advice.go) and a claim's Legion-managed walk
// (claimLegionManagedStatus, issue_claim.go). Each bounds its queries with it.
const advisoryStatement = "set local statement_timeout = '500ms'"

// refuseAdvisoryReadsInXidWindow fails t when a write ran an advisory read while it held its xid.
// An advisory read is queries, so D6 cannot see it: each runs back to back with the next. The plan
// moves write advice to the read phase of every caller (M3) and makes the claim's walk xid-free
// (M1), so the queries a write runs to advise its caller hold back no reader's snapshot.
func refuseAdvisoryReadsInXidWindow(t *testing.T, report *storetest.XidReport) {
	t.Helper()
	for _, window := range report.Windows {
		for index, statement := range window.Statements {
			if strings.TrimSpace(statement.SQL) == advisoryStatement {
				t.Errorf("XID WINDOW VIOLATION (INFRA-636 M3/M1) %s\n  an advisory read (write advice or the claim's Legion-managed walk) ran inside the xid window, from statement %d at %s after the window opened\n%s",
					window.Summary(), index, storetest.XidMillis(statement.Start.Sub(window.Start)), window.Detail())
				break
			}
		}
	}
}

// resolveComment resolves a fixture comment, as a person does.
func (f *xidFixture) resolveComment(t *testing.T, commentID string) {
	t.Helper()
	if resolved := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/comments/"+commentID+"/resolve", nil, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve comment: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
}

// accepted records in f.spec the text an accepted suggestion left in the spec.
func (f *xidFixture) accepted(suggestion xidSuggestion) {
	f.spec = strings.Replace(f.spec, suggestion.quote, suggestion.replaceWith, 1)
}

// uploadVerification uploads the issue's Verification document, a document of the harness's
// size holding the two checks a result is recorded against (twoChecks), the first time a step
// asks for it.
func (f *xidFixture) uploadVerification(t *testing.T) {
	t.Helper()
	if f.verification {
		return
	}
	verification, _ := xidSpec(4)
	uploaded := uploadDocument(t, f.handler, "/api/v1/issues/"+f.issueKey, "verification.md", verification+"\n"+twoChecks)
	waitForArtifactVersion(t, f.handler, uploaded.Artifact.ID, 1)
	f.verification = true
}

// TestXidWindowsOnTheLargeSpec measures, as steps on one fixture, every route whose request needs
// nothing a step before it could have changed: the anchored writes and the writes beside the spec
// first, then the versions of the spec, then the approval request and its hand-back, and the
// claim last.
func TestXidWindowsOnTheLargeSpec(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process
	// (XidTracer.Measure fails in a parallel test).
	runXidSteps(t, newXidFixture(t, testServerOptions{}, ""), false, []xidStep{
		{"comment create anchored", xidWindowCommentCreateAnchored},
		{"comment create reply on anchored thread", xidWindowCommentCreateReplyOnAnchoredThread},
		{"ask create anchored", xidWindowAskCreateAnchored},
		{"asked of", xidWindowAskedOf},
		{"comment edit", xidWindowCommentEdit},
		{"comment delete", xidWindowCommentDelete},
		{"comment resolve", xidWindowCommentResolve},
		{"comment reopen", xidWindowCommentReopen},
		{"suggestion reject", xidWindowSuggestionReject},
		{"issue patch", xidWindowIssuePatch},
		{"message create", xidWindowMessageCreate},
		{"message edit", xidWindowMessageEdit},
		{"message delete", xidWindowMessageDelete},
		{"user issue state", xidWindowUserIssueState},
		{"user thread read", xidWindowUserThreadRead},
		{"result recorded", xidWindowResultRecorded},
		{"result retracted", xidWindowResultRetracted},
		{"plan review", xidWindowPlanReview},
		{"issue create with spec", xidWindowIssueCreateWithSpec},
		{"existing external issue", xidWindowExistingExternalIssue},
		{"upload replace", xidWindowUploadReplace},
		{"spec edit", xidWindowSpecEdit},
		{"named version", xidWindowNamedVersion},
		{"suggestion accept", xidWindowSuggestionAccept},
		{"approval request opened", xidWindowApprovalRequestOpened},
		{"approval request handed back", xidWindowApprovalRequestHandedBack},
		{"claim", xidWindowClaim},
	})
}

func xidWindowCommentCreateAnchored(t *testing.T, f *xidFixture) {
	quote := f.takeSpare(t)
	f.measureAdvised(t, http.StatusCreated, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
			"body": "Does this still hold once the outbox is sharded?", "anchor": map[string]any{"artifact": "spec", "quote": quote},
			"actor": sessionActor(),
		})
	})
}

func xidWindowCommentCreateReplyOnAnchoredThread(t *testing.T, f *xidFixture) {
	f.measureAdvised(t, http.StatusCreated, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
			"body": "It does: the listener keeps its interests in the bucket.", "reply_to": f.comments[0], "actor": sessionActor(),
		})
	})
}

func xidWindowAskCreateAnchored(t *testing.T, f *xidFixture) {
	quote := f.takeSpare(t)
	f.measureAdvised(t, http.StatusCreated, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/asks", map[string]any{
			"question": "Which team signs off on this requirement?",
			"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
			"anchor":   map[string]any{"artifact": "spec", "quote": quote}, "actor": sessionActor(),
		})
	})
}

// newBlockAskFixture is the fixture with the transport `:::ask` block at the end of its spec, and
// the id of the ask settlement opened from it. A block ask's row is opened by settlement, so this
// fixture settles twenty milliseconds after each write, as the default server does, and waits for
// every settlement to finish before it answers, so the measured request runs alone. A block ask is
// the one whose transitions write its block back into the document (SetBlockAttributes,
// SetAskBlockText); an anchored ask's touch only its row.
func newBlockAskFixture(t *testing.T) (*xidFixture, string) {
	t.Helper()
	spec, _ := xidSpec(1)
	f := newXidFixture(t, testServerOptions{settle: 20 * time.Millisecond}, spec+"\n"+transportAsk)
	awaitIndexedAskBlock(t, f.handler, f.specID, "decision", "Which transport?")
	askID := blockAskID(t, f.handler, f.issueKey, "decision")
	awaitSettledDocuments(t, f.database)
	return f, askID
}

func TestXidWindowAskAnswerBlock(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process.
	f, askID := newBlockAskFixture(t)
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
			"text": "REST first.",
		}, "alice")
	})
}

func TestXidWindowAskResolveBlock(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process.
	f, askID := newBlockAskFixture(t)
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/asks/"+askID+"/resolve", map[string]any{
			"kind": "resolved", "reason": "Settled in the thread",
		}, "alice")
	})
}

// TestXidWindowAskBlockEditThenIssueClose measures, on one block ask fixture, the edit of the
// block ask's text, which leaves the ask open, and then the close that withdraws it with the
// issue's anchored asks: the edit's version settles before the close is measured, as the block
// ask's own opening did before the edit.
func TestXidWindowAskBlockEditThenIssueClose(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process.
	f, askID := newBlockAskFixture(t)
	runXidSteps(t, f, true, []xidStep{
		{"ask edit block text", func(t *testing.T, f *xidFixture) { xidWindowAskEditBlockText(t, f, askID) }},
		{"issue close with open asks", xidWindowIssueCloseWithOpenAsks},
	})
}

func xidWindowAskEditBlockText(t *testing.T, f *xidFixture, askID string) {
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
			"question": "Which transport ships first?",
		}, "alice")
	})
}

// Handing an open ask to another person (PUT /api/v1/asks/{id}/asked-of, putAskAskedOf) is
// main's route, added by #3429 and brought here by the forward merge. It records
// `ask.asked_of`, so it falls under A1, and this test is what declares the window it holds:
// database statements only - the ask's own update, the batch appending the event, the two
// reads that load the ask it answers with (askReadColumns and the opened event ids attached to
// it) and the COMMIT ending them. No document render, no anchor walk, no reply planning and no
// write advice runs among them. Everything the hand-on is decided on comes before the first of
// them: the person it names, held to having signed in against the shared pool before the
// transaction opens (allowedLogin); the ask read unlocked; its owner's lock, which is advisory
// and assigns no transaction id (store.LockOwner, through requireOpenOwner); the ask read for
// the transition under that lock, which takes no row lock of its own
// (readAskForTransition); and the person it waited on beside its issue's assignee. The
// `update asks ... returning asked_of_at` is the first statement that writes, so it is where
// the window opens.
func xidWindowAskedOf(t *testing.T, f *xidFixture) {
	created := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/asks", map[string]any{
		"question": "Who signs off on the migration section?",
		"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
		"anchor":   map[string]any{"artifact": "spec", "quote": f.takeSpare(t)}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("open the ask to hand on: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[model.Ask](t, created)
	f.measure(t, http.StatusOK, storetest.XidWindowOf(5,
		"the ask row's own update, the batch appending ask.asked_of, the two reads that load the "+
			"ask it answers with and the COMMIT ending them"), func(h http.Handler) *httptest.ResponseRecorder {
		return handAskTo(t, h, ask.ID, "bob", "alice")
	})
}

// xidWindowCommentReopen reopens a seed comment of its own, which it resolves first; the resolve
// step resolves another (xidWindowCommentResolve).
func xidWindowCommentReopen(t *testing.T, f *xidFixture) {
	f.resolveComment(t, f.comments[2])
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+f.comments[2]+"/reopen", nil, "alice")
	})
}

func xidWindowCommentEdit(t *testing.T, f *xidFixture) {
	comment := createThreadComment(t, f.handler, f.issueKey, map[string]any{
		"body": "First wording.", "anchor": map[string]any{"artifact": "spec", "quote": f.takeSpare(t)},
	}, "alice")
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{"body": "Second wording."}, "alice")
	})
}

// xidWindowCommentDelete deletes an anchored comment of alice's own, as its author does: the
// history row, the clear, the index of the empty body, the tombstone of its blocks, the margin
// record its projection writes and the comment.deleted event are queued on the ledger in the
// read phase and sent as its one batch (deleteComment).
func xidWindowCommentDelete(t *testing.T, f *xidFixture) {
	comment := createThreadComment(t, f.handler, f.issueKey, map[string]any{
		"body": "A wording to withdraw.", "anchor": map[string]any{"artifact": "spec", "quote": f.takeSpare(t)},
	}, "alice")
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodDelete, "/api/v1/comments/"+comment.ID, nil, "alice")
	})
}

func xidWindowCommentResolve(t *testing.T, f *xidFixture) {
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+f.comments[1]+"/resolve", nil, "alice")
	})
}

func xidWindowSuggestionAccept(t *testing.T, f *xidFixture) {
	suggestion := f.suggest(t)
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+suggestion.ID+"/accept", map[string]any{}, "alice")
	})
	f.accepted(suggestion)
}

func xidWindowSuggestionReject(t *testing.T, f *xidFixture) {
	suggestion := f.suggest(t)
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+suggestion.ID+"/reject", map[string]any{}, "alice")
	})
}

func xidWindowSpecEdit(t *testing.T, f *xidFixture) {
	from, to := f.revise()
	f.measureAdvised(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/edits", map[string]any{
			"ops":   []map[string]string{{"op": "replace", "find": from, "with": to}},
			"actor": sessionActor(),
		})
	})
}

// An upload under the spec's name replaces it: the whole document is re-read, its anchors re-found
// and a version written. It names the token of the version it replaces, as every replacement does,
// read before the measured request.
func xidWindowUploadReplace(t *testing.T, f *xidFixture) {
	f.revise()
	base := uploadBase(t, f.handler, f.specID)
	f.measureAdvised(t, http.StatusCreated, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
			"name": "spec.md", "content": f.spec, "summary": "Bump the revision", "actor": sessionActor(),
			"precondition": base,
		})
	})
}

// An upload of the text a person approved carries his approval to the version it writes
// (docs.CarryUploadedApproval). Everything that carry is decided on - the document's newest
// review, the approved version's text, its decision blocks' rows and the open approval request -
// is read in the route's read phase, and what it writes - the request's answer, when one is open,
// and the review - is queued with the route's own writes, so the window is that one batch and the
// commit ending it.
func TestXidWindowUploadRestoringTheApprovedText(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process.
	f := newXidFixture(t, testServerOptions{}, "")
	approval := reviewDocument(t, f.handler, f.specID, "alice", "approved", "")
	approved := versionMarkdown(t, f.handler, f.specID, approval.Version)
	f.revise()
	if uploaded := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
		"name": "spec.md", "content": f.spec, "summary": "Bump the revision", "actor": sessionActor(),
		"precondition": uploadBase(t, f.handler, f.specID),
	}); uploaded.Code != http.StatusCreated {
		t.Fatalf("stale the approval before the restore: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	waitForArtifactVersion(t, f.handler, f.specID, approval.Version+1)
	// The restore names the token of the version it replaces, read before the measured request.
	base := uploadBase(t, f.handler, f.specID)
	f.measureBatched(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
			"name": "spec.md", "content": approved, "actor": sessionActor(), "precondition": base,
		})
	})
	// The measured request is the restore only while it carries the approval: a window measured
	// over a route that carried nothing would report a window this write path never ran in.
	waitForArtifactVersion(t, f.handler, f.specID, approval.Version+2)
	requireReviewCarriedWith(t, f.handler, f.specID, approval.Version+2, approval.Version, restoreSentence(approval.Version), nil)
}

// A named version of a document changed since its last version captures and renders the live
// document.
func xidWindowNamedVersion(t *testing.T, f *xidFixture) {
	from, to := f.revise()
	if edited := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": from, "with": to}},
		"actor": sessionActor(),
	}); edited.Code != http.StatusOK {
		t.Fatalf("edit the spec before naming a version: status=%d body=%s", edited.Code, edited.Body.String())
	}
	f.measure(t, http.StatusCreated, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/versions", map[string]any{
			"summary": "Revision one", "actor": sessionActor(),
		})
	})
}

// A rebuild repairs a history ygo cannot load: this server's document store hands the spec's next
// load a history that does not decode (invalidLoadOnceStore), and the rebuild writes the supplied
// markdown as the document's next version.
func TestXidWindowDocumentRebuild(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process.
	var broken *invalidLoadOnceStore
	f := newXidFixture(t, testServerOptions{
		persistence: func(database *store.Store) docs.VersionedStore {
			broken = newInvalidLoadOnceStore(database)
			return broken
		},
	}, "")
	if err := f.deps.Docs.(*docs.Service).Evict(context.Background(), f.specID); err != nil {
		t.Fatalf("evict the spec: %v", err)
	}
	broken.failNextLoad(t, f.specID)
	f.revise()
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/rebuild", map[string]any{"markdown": f.spec}, "alice")
	})
}

// An approval request on a document with no open request inserts the ask, follows its author,
// indexes its question and appends ask.opened. What it decides on - the document, its settled
// version and the approval it already carries - is read before the first of those writes.
func xidWindowApprovalRequestOpened(t *testing.T, f *xidFixture) {
	f.measureBatched(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/approval-requests", map[string]any{
			"summary": "The migration section is rewritten; the rest of the spec stands.", "actor": sessionActor(),
		})
	})
}

// A version moved the open request, so it waits on its agent: a request naming a new summary
// rewords it (docs.RewriteApprovalAsk) and hands it back. The rewording plans the question's
// references and reads the row's edit time in the read phase, and the hand-back's update and both
// events follow in the write phase. The request is the one the step before this one opened
// (xidWindowApprovalRequestOpened).
func xidWindowApprovalRequestHandedBack(t *testing.T, f *xidFixture) {
	f.revise()
	if uploaded := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
		"name": "spec.md", "content": f.spec, "summary": "Bump the revision", "actor": sessionActor(),
		"precondition": uploadBase(t, f.handler, f.specID),
	}); uploaded.Code != http.StatusCreated {
		t.Fatalf("version the spec under the open request: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	f.measureBatched(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/approval-requests", map[string]any{
			"summary": "Only the revision token moved; the rest stands as you read it.", "actor": sessionActor(),
		})
	})
}

// approvalRequestOverTwoDocuments uploads the issue's Verification document, a second document of
// the harness's size, and opens one approval request naming the fixture's spec and it
// (include_verification), each at its own settled version. It answers the ask the person answers
// and the Verification document's id.
func (f *xidFixture) approvalRequestOverTwoDocuments(t *testing.T) (string, string) {
	t.Helper()
	verification, _ := xidSpec(2)
	uploaded := uploadDocument(t, f.handler, "/api/v1/issues/"+f.issueKey, "verification.md", verification)
	waitForArtifactVersion(t, f.handler, uploaded.Artifact.ID, 1)
	requested := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/approval-requests", map[string]any{
		"summary":              "The migration section is rewritten, and the verification follows it.",
		"include_verification": true, "actor": sessionActor(),
	})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval of the spec and its Verification document: status=%d body=%s", requested.Code, requested.Body.String())
	}
	request := decodeBody[approvalRequestAsk](t, requested)
	if request.Ask == nil || len(request.Ask.Approval.Documents) != 2 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): approval ask = %#v, want one ask naming two documents", request.Ask)
	}
	return request.Ask.ID, uploaded.Artifact.ID
}

// A request may name several documents, and the person's one answer reviews each of them at the
// version the request named for it: the answer closes the ask (closeAsk) and then queues one
// review per named document (queueApprovalReviews), reading the row of every document but the one
// the request was made on (artifactMetadata) between the queue calls. Those reads are still the
// route's read phase: a queued write runs no statement of its own, since the ledger sends the
// whole batch at Commit, so everything the answer is decided on - the ask under its owner's
// advisory lock, which assigns no xid (store.LockOwner), the named document and each further
// document's row - is read before the transaction's first write. Behind them the window holds one
// batch: the ask's answer and its answered row, its parts' index, ask.answered, and one review row
// with its artifact.approved per named document.
func TestXidWindowApprovalAnswerOverTwoDocuments(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process.
	f := newXidFixture(t, testServerOptions{}, "")
	askID, verificationID := f.approvalRequestOverTwoDocuments(t)
	f.measureBatched(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
			"selected": []string{"Approve"},
		}, "alice")
	})
	// The measured request is this write path only while it reviewed both documents: a window
	// measured over an answer that wrote one review would report a window this path never ran in.
	for _, artifactID := range []string{f.specID, verificationID} {
		reviews := listReviews(t, f.handler, artifactID)
		if len(reviews) != 1 || reviews[0].State != "approved" || reviews[0].Kind != "person" ||
			reviews[0].AskID == nil || *reviews[0].AskID != askID {
			t.Fatalf("XID RIG PROBLEM (not a finding about the code): reviews of %s = %#v, want the one approval the measured answer wrote", artifactID, reviews)
		}
	}
}

// A plan is approved by the plan review an agent runs over it, never by a person, and this route
// records that approval of one plan version (createArtifactPlanReview). Its one write is the
// review, queued on the ledger and sent by Commit; everything it is decided on - the document's
// row, its owner, taken under the advisory lock that assigns no xid, and its settled versions - is
// read before it, so the window is that batch and the COMMIT ending it.
func xidWindowPlanReview(t *testing.T, f *xidFixture) {
	markdown, _ := xidSpec(3)
	plan := uploadDocument(t, f.handler, "/api/v1/issues/"+f.issueKey, "plan.md", markdown)
	waitForArtifactVersion(t, f.handler, plan.Artifact.ID, 1)
	f.measureBatched(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews", map[string]any{
			"version": 1, "actor": sessionActor(),
		})
	})
}

func xidWindowIssueCreateWithSpec(t *testing.T, f *xidFixture) {
	spec, _ := xidSpec(2)
	f.measureAdvised(t, http.StatusCreated, storetest.XidWindowOf(11,
		"the create's inline steering writes - the project's next number, the rank lock, the rank read, the issue's node, the issue row, its blocked_by links and its artifact row - "+
			"the two reads that load the issue it answers with, the queued batch and its COMMIT"), func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": xidProject, "title": "Second large spec for the reviewers", "spec": spec, "force": true,
			"actor": sessionActor(),
		})
	})
}

// A create naming a GitHub issue Dispatch already tracks answers that issue
// (respondWithExistingExternalIssue), with its advice, and creates nothing. The answer's own
// transaction holds no xid: it writes no row, and the issue's owner lock it reads under is the
// advisory lock alone, which assigns none while no earlier build is in the fleet (store.LockOwner).
// The request still holds one xid window on every run, since it records the related-item check it
// did not make (reason already_exists) in a transaction of its own after that commit
// (insertRelatedChecks), and the record's insert assigns that transaction its xid.
func xidWindowExistingExternalIssue(t *testing.T, f *xidFixture) {
	body := map[string]any{"project": xidProject, "external": "owner/repo#41", "title": "Tracked upstream", "actor": sessionActor()}
	if created := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues", body); created.Code != http.StatusCreated {
		t.Fatalf("create the external issue: status=%d body=%s", created.Code, created.Body.String())
	}
	// The record of a related-item check is written in a transaction of its own, after the write
	// that ran the check has committed (insertRelatedChecks). That transaction is the window: its
	// xid comes with the batch that inserts the related_checks row, and the window holds that batch
	// and the COMMIT ending it. Nothing here holds a lock the insert waits on, so the record always
	// lands within the request and a run with no window fails.
	f.measureAdvised(t, http.StatusOK, storetest.XidWindowOf(2,
		"the related-item check's record in its own transaction (insertRelatedChecks): the batch inserting "+
			"the related_checks row, and the COMMIT ending it"), func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues", body)
	})
}

func xidWindowIssuePatch(t *testing.T, f *xidFixture) {
	f.measureAdvised(t, http.StatusOK, storetest.XidWindowOf(5,
		"the issue row's own update, the two reads that load the issue it answers with, the queued batch and its COMMIT"), func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPatch, "/api/v1/issues/"+f.issueKey, map[string]any{
			"title": "Large spec under review, second pass", "actor": sessionActor(),
		})
	})
}

func xidWindowMessageCreate(t *testing.T, f *xidFixture) {
	f.measureAdvised(t, http.StatusCreated, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/messages", map[string]any{
			"body":  "Status: the spec at dispatch://" + f.issueKey + " has twenty open anchors; replies welcome.",
			"actor": sessionActor(),
		})
	})
}

// xidWindowMessageEdit edits an issue message of alice's own: the history row, the update, the
// index of the new body and the message.edited event go in the one batch the route runs itself
// (editMessage).
func xidWindowMessageEdit(t *testing.T, f *xidFixture) {
	message := createIssueMessage(t, f.handler, f.issueKey, map[string]any{"body": "Status: the first wording."}, "alice")
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPatch, "/api/v1/messages/"+message.ID, map[string]any{"body": "Status: the second wording."}, "alice")
	})
}

// xidWindowMessageDelete deletes an issue message of alice's own: the history row, the clear,
// the index of the empty body, the tombstone of its blocks, the settle of its pending sends and
// the message.deleted event go in the one batch the route runs itself (deleteMessage).
func xidWindowMessageDelete(t *testing.T, f *xidFixture) {
	message := createIssueMessage(t, f.handler, f.issueKey, map[string]any{"body": "Status: a wording to withdraw."}, "alice")
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodDelete, "/api/v1/messages/"+message.ID, nil, "alice")
	})
}

func xidWindowClaim(t *testing.T, f *xidFixture) {
	f.measureAdvised(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/claim", map[string]any{}, "alice")
	})
}

// A close withdraws the issue's open asks: the eight anchored asks on its spec and the block ask
// settlement opened from the spec's `:::ask` block, whose block and copies it writes resolved into
// the spec (docs.WriteIssueAskStates) before it records each ask withdrawn. That document work
// reads and rewrites the whole spec, so it is the close's read phase, before its first write.
func xidWindowIssueCloseWithOpenAsks(t *testing.T, f *xidFixture) {
	f.measure(t, http.StatusOK, storetest.XidWindowOf(5,
		"the issue row's own update, the two reads that load the issue it answers with, the batch withdrawing its asks and its COMMIT"), func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPatch, "/api/v1/issues/"+f.issueKey, map[string]any{"status": "done"}, "alice")
	})
}

// TestXidWindowsUnderTheParentStatusRule measures, with DISPATCH_PARENT_STATUS_RULE on, the two
// writes to which the rule adds a statement inside the xid window: a create under a parent,
// whose write phase reads the parent's row `for share` just before its insert, and a close, whose
// write phase reads the issue's open children again after its own update. With the rule off
// neither statement runs, and xidWindowIssueCreateWithSpec and xidWindowIssueCloseWithOpenAsks
// hold those two writes to the windows they hold without it. The create goes first and files
// its issue under a parent of its own, so the fixture's issue has no child when it closes. The
// fixture settles each document twenty milliseconds after a write, and each step waits for every
// document to settle first, so the close settles nothing before its transaction.
func TestXidWindowsUnderTheParentStatusRule(t *testing.T) {
	f := newXidFixture(t, testServerOptions{settle: 20 * time.Millisecond, parentStatusRule: true}, "")
	runXidSteps(t, f, true, []xidStep{
		{"issue create under a parent", xidWindowIssueCreateUnderAParent},
		{"issue close with open asks", xidWindowIssueCloseUnderTheRule},
	})
}

// A create with a spec under a parent, with the rule on: the create's window of
// xidWindowIssueCreateWithSpec, the read of the parent's row `for share` before the insert, and
// the walk of the new issue's ancestors that its issue.created event names, which a create under
// a parent runs whatever the setting.
func xidWindowIssueCreateUnderAParent(t *testing.T, f *xidFixture) {
	parent := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": xidProject, "title": "Parent of the measured create", "force": true, "actor": sessionActor(),
	})
	if parent.Code != http.StatusCreated {
		t.Fatalf("create the parent: status=%d body=%s", parent.Code, parent.Body.String())
	}
	parentKey := decodeBody[model.Issue](t, parent).Key
	awaitQuietDatabase(t, f.database, true)
	spec, _ := xidSpec(3)
	f.measureAdvised(t, http.StatusCreated, storetest.XidWindowOf(12,
		"the create's inline steering writes - the project's next number, the rank lock, the rank read, the parent's row read for share, the issue row, its blocked_by links and its artifact row - "+
			"the two reads that load the issue it answers with, the walk of its ancestors for its event, the queued batch and its COMMIT"), func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": xidProject, "title": "Large spec filed under a parent", "spec": spec, "parent": parentKey, "force": true,
			"actor": sessionActor(),
		})
	})
}

// A close with open asks, with the rule on: the close's window of xidWindowIssueCloseWithOpenAsks,
// and the second read of the issue's open children after its update.
func xidWindowIssueCloseUnderTheRule(t *testing.T, f *xidFixture) {
	f.measure(t, http.StatusOK, storetest.XidWindowOf(6,
		"the issue row's own update, the read of its open children, the two reads that load the issue it answers with, the batch withdrawing its asks and its COMMIT"), func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPatch, "/api/v1/issues/"+f.issueKey, map[string]any{"status": "done"}, "alice")
	})
}

// A result is recorded against a `check` block of the issue's Verification document, a document of
// the harness's size. The route joins no ledger and writes its two rows inline, so its window
// holds three statements: the batch that appends `result.recorded` (the event owner's lock, its
// row, and the insert), the result row's own insert, and the COMMIT ending them. Everything the
// record is decided on comes before the first of them - the check's words, read before the
// transaction opens; the two issues' owner locks, which assign no transaction id
// (store.LockOwner, lockIssueOwners); whether each issue is open; the screenshots the result
// names; and the version and digest the index holds for the check, read under those locks - and
// the one read of the live document a moved version or digest needs runs with those locks given
// back (recordResultWithFreshWords), so no read of a document is inside this window or behind
// these locks.
func xidWindowResultRecorded(t *testing.T, f *xidFixture) {
	f.uploadVerification(t)
	f.measure(t, http.StatusCreated, storetest.XidWindowOf(3,
		"the batch appending result.recorded, the result row's insert and the COMMIT ending them"), func(h http.Handler) *httptest.ResponseRecorder {
		return recordResult(t, h, f.issueKey, resultBody("k1", "pass", headCommit, map[string]any{
			"observed": "41 passed", "negative_control": "Cut the route and the row fails.",
		}))
	})
}

// A recorded result is retracted. The route joins no ledger and writes its two rows inline, so
// its window holds three statements: the batch that appends `result.retracted` (the event
// owner's lock, which this transaction already holds, the issue's row and the insert), the
// retraction row's own insert, and the COMMIT ending them. Everything the retraction is decided
// on comes before the first of them - the result row, read plainly and with no row lock, for
// the owner it names (readRetractedResult); that owner's lock, the check issue's, which the
// append would otherwise be the first to take and which assigns no transaction id
// (store.LockOwner); the claim of the issue the result was recorded on, which decides a session
// caller (refuseRetractionBy); and whether a retraction of this result stands already
// (resultIsRetracted), both read under that lock.
func xidWindowResultRetracted(t *testing.T, f *xidFixture) {
	f.uploadVerification(t)
	recorded := recordResult(t, f.handler, f.issueKey, resultBody("k1", "pass", headCommit, map[string]any{
		"observed": "41 passed", "negative_control": "Cut the route and the row fails.",
	}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record the result this test retracts: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	result := decodeBody[recordedResult](t, recorded).Result
	f.measure(t, http.StatusCreated, storetest.XidWindowOf(3,
		"the batch appending result.retracted, the retraction row's insert and the COMMIT ending them"), func(h http.Handler) *httptest.ResponseRecorder {
		return retractAsPerson(t, h, result.ID, "The run was taken against the wrong commit.")
	})
}

// A person writes their own state for the issue: the pin, the read mark and the asks they
// dismissed, which records `user_state.updated` on the issue's project. The route writes its row
// inline, so its window holds three statements: the row's upsert, the batch appending the event
// (the project owner's lock, which this transaction already holds, the project's row and the
// insert), and the COMMIT ending them. Everything the write is decided on comes before the first
// of them - the issue's project, read plainly because it names the owner; that owner's lock,
// which the append would otherwise be the first to take and which assigns no transaction id
// (store.LockOwner); and the caller's own row, read under that lock with no row lock of its own,
// which the stale check and the merge of the body's fields decide on (putUserState).
func xidWindowUserIssueState(t *testing.T, f *xidFixture) {
	// The row is written once before the measurement, so the measured write is a write over a row
	// that stands, which is the common one. A row lock of a row that does not exist assigns no
	// transaction id, so a first write would hide the `for update` this window is declared
	// against.
	seeded := dispatchRequest(t, f.handler, http.MethodPut, "/api/v1/me/issues/"+f.issueKey+"/state",
		map[string]any{"pinned": false}, "alice")
	if seeded.Code != http.StatusOK {
		t.Fatalf("seed the viewer's state row: status=%d body=%s", seeded.Code, seeded.Body.String())
	}
	f.measure(t, http.StatusOK, storetest.XidWindowOf(3,
		"the state row's upsert, the batch appending user_state.updated and the COMMIT ending them"), func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPut, "/api/v1/me/issues/"+f.issueKey+"/state", map[string]any{
			"pinned": true, "last_read_seq": 3, "dismissed": []string{"ask-1"},
		}, "alice")
	})
}

// A person marks one Inbox thread read, which records `user_thread_state.updated` on the
// thread's project. The route writes its row inline, so its window holds three statements: the
// row's update, the batch appending the event (the project owner's lock, which this transaction
// already holds, the project's row and the insert), and the COMMIT ending them. Everything the
// mark is decided on comes before the first of them - the caller's row, read plainly because it
// names both owners; the thread's owner lock, taken exclusively and assigning no transaction id
// (store.LockOwner), which is what lets the project be taken before the row at all
// (putThreadRead); whether the mark moves the row, read under that lock by the update's own
// predicate; and the project's owner lock, which the append would otherwise be the first to
// take.
func xidWindowUserThreadRead(t *testing.T, f *xidFixture) {
	// Alice's own comment gives her the thread's user_threads row - the trigger writes one for a
	// person's write (migration 0095) - and a session's reply to it leaves that row unread, so
	// the measured mark moves it. A mark that moved nothing would write nothing and hold no
	// transaction id, which is no window to measure.
	root := createThreadComment(t, f.handler, f.issueKey, map[string]any{
		"body": "Does the migration section still hold?",
	}, "alice")
	reply := sessionComment(t, f.handler, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
		"body": "It does, and the next release carries it.", "reply_to": root.ID,
	})
	f.measure(t, http.StatusNoContent, storetest.XidWindowOf(3,
		"the thread row's update, the batch appending user_thread_state.updated and the COMMIT ending them"),
		func(h http.Handler) *httptest.ResponseRecorder {
			return markThreadRead(t, h, "alice", root.ID, reply.CreatedAt)
		})
}

// xidDeliverySessions is the fake listener's one live session, s1, the recipient every delivery
// window test is measured on.
const xidDeliverySessions = `[{"session_id":"s1","title":"planner","dir":"/w/legion","machine_id":"m1","roles":[],"capabilities":["aside","btw","steer"],"last_seen":42}]`

// newXidDeliveryFixture is the fixture on a server whose Envoy listener is the package's fake
// (newBroadcastListener) holding that one live session, so a mention, a targeted message and a
// direct message all reach it and each delivery route runs its whole path. The sends it records
// are of no interest here; what the fixture needs is a listener that answers every lookup and
// every send. The deliveries are on the fixture's issue, whose spec is the harness's large one,
// so each route climbs and reads what it reads in production.
func newXidDeliveryFixture(t *testing.T) *xidFixture {
	t.Helper()
	listener, _ := newBroadcastListener(t, xidDeliverySessions)
	return newXidFixture(t, testServerOptions{envoyURL: listener.URL}, "")
}

// TestXidWindowDeliveries measures, as steps on one delivery fixture, each delivery route on a
// delivery of its own: a mention's reply, a targeted message's reply, a direct message's accept
// and a targeted message's retry.
func TestXidWindowDeliveries(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process.
	runXidSteps(t, newXidDeliveryFixture(t), false, []xidStep{
		{"comment delivery reply", xidWindowCommentDeliveryReply},
		{"message delivery reply", xidWindowMessageDeliveryReply},
		{"delivery accepted", xidWindowDeliveryAccepted},
		{"pending delivery recorded", xidWindowPendingDeliveryRecorded},
	})
}

// xidSessionReply is the mentioned session answering one delivery attempt, as its host answers
// the frame it was handed.
func xidSessionReply(t *testing.T, h http.Handler, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body["actor"] = map[string]any{"kind": "session", "id": "s1"}
	return bearerRequest(t, h, http.MethodPost, path, body)
}

// xidCitingBody is a reply body that makes its index do every piece of document work a body can
// ask of it: it writes issueKey bare, so the index resolves a reference and stores an edge for
// it, and a `#id`, so the index reads the document the reply belongs to - the issue's spec, of
// the harness's size - and scans its lines for question blocks (refs.QuestionMap). A body citing
// neither sends neither read, so a window measured on one proves nothing about where the index
// runs.
func xidCitingBody(text, issueKey string) string {
	return text + " " + issueKey + " says so, and #migration-scope is the block to read."
}

// A mentioned session answers its delivery with a reply naming a block of the comment it
// answers. The mention hangs under one of the fixture's open asks, so the reply joins that ask's
// thread as every reply under an ask does. The reply's body cites an issue key and a `#id`
// (xidCitingBody), so its index resolves a reference and reads the issue's whole spec for the
// `#id` scope: the document work this window is measured for runs at all.
//
// The route writes as it goes, so its window holds its statements rather than one batch, and
// everything the reply is decided on comes before the first of them: the comment and its owner,
// taken under the advisory lock that assigns no transaction id (store.LockOwner,
// lockedCommentAllowingClosed); the climb from it to the head of its thread, the ask it hangs
// under (threadHeadOf); the block the reply names, with the index its comment may owe planned
// there and the block itself addressed there, so the reply carries it instead of reading it
// back; and the reply's own block index and references, planned for an id and a created_at
// reserved there (planCommentReply). Each refusal those reads raise is raised where it was
// raised, after the mention and the attempt have been checked, so the answers of this route are
// what they were. The mention rows are read without a lock, so the window opens at the attempt
// row.
func xidWindowCommentDeliveryReply(t *testing.T, f *xidFixture) {
	comment := decodeMentionedComment(t, postMentionedComment(t, f.handler, f.issueKey, map[string]any{
		"body":     "Does this still hold once the outbox is sharded?\n\nThe migration section is the one to read.",
		"ask_id":   f.asks[0],
		"mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, f.database, comment.ID)
	f.measure(t, http.StatusCreated, storetest.XidWindowOf(9,
		"the attempt row's lock, the reply's insert, its author's follow of the ask it joins, the one "+
			"batch writing its block index and its references, the attempt's update, the thread's "+
			"logins, the batch appending comment.answered, the stamp of its references and the COMMIT "+
			"ending them"), func(h http.Handler) *httptest.ResponseRecorder {
		return xidSessionReply(t, h, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
			"attempt": 1, "body": xidCitingBody("It holds: the listener keeps its interests in the bucket.", f.issueKey),
			"reply_to_block": "b2",
		})
	})
}

// A session answers a targeted message with a reply naming a block of it, whose body cites an
// issue key and a `#id` (xidCitingBody), so the index resolves a reference and reads the
// issue's whole spec for the `#id` scope. The window opens at the parent message's lock, which
// the route takes in the order the accept route takes its own - issue, message, attempt. The
// block the reply names is read before it, with the index that read finds the parent owes
// written under the lock, the order refs.IndexItemIfOwed states; the block itself is addressed
// there too, so the reply carries it instead of reading it back, and the reply's own block
// index and references are planned there, for an id and a created_at reserved there
// (planMessageReply), and written as one batch behind its insert. The issue's owner lock
// assigns no transaction id.
func xidWindowMessageDeliveryReply(t *testing.T, f *xidFixture) {
	message := createIssueMessage(t, f.handler, f.issueKey, map[string]any{
		"body":   "Can this ship?\n\nThe migration section is the one to read.",
		"target": "session:s1", "delivery": "steer",
	}, "alice")
	f.measure(t, http.StatusCreated, storetest.XidWindowOf(11,
		"the parent message's lock, the attempt row's lock, the reply's node, the reply's insert, the one batch writing "+
			"its block index and its references, its thread, the thread's logins, the batch appending "+
			"message.answered and what the node write stated, the stamp of its references, the attempt's update and the COMMIT ending "+
			"them"), func(h http.Handler) *httptest.ResponseRecorder {
		return xidSessionReply(t, h, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
			"attempt": 1, "body": xidCitingBody("It can: the outbox keys each copy.", f.issueKey),
			"in_reply_to_block": "b2",
		})
	})
}

// A session takes a person's direct message as its own user turn. Its window is wider than one
// batch, and every statement in it is a database statement - the message row's lock, the one
// read its ten checks are judged by, the attempt's update and the batch appending
// message.accepted - with no document work and no write advice among them. The route takes no
// owner lock: the event of a direct message has no owner, and a message on an issue is refused
// before any write, so the message row's lock, the window's first statement, is the one that
// orders two accepts.
func xidWindowDeliveryAccepted(t *testing.T, f *xidFixture) {
	created := postAgentMessage(t, f.handler, "s1", map[string]any{
		"body": "Read the migration section of the spec and tell me what it costs.", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to s1: status=%d body=%s", created.Code, created.Body.String())
	}
	root := decodeBody[model.Message](t, created)
	f.measure(t, http.StatusOK, storetest.XidWindowOf(5,
		"the message row's lock, the read its checks are judged by, the attempt's update, the batch "+
			"appending message.accepted and the COMMIT ending them"), func(h http.Handler) *httptest.ResponseRecorder {
		return xidSessionReply(t, h, "/api/v1/messages/"+root.ID+"/deliveries/1/accept", map[string]any{})
	})
}

// A person retries the delivery of a targeted message that is itself a reply naming a block, so
// the claim transaction builds the frame the send carries from the stored message: the block it
// answers and the thread it belongs to. That route changes no code for INFRA-636 either, and
// this test records why: those are database reads, and no document work or write advice runs
// under the message row's lock. The issue is locked before the message row, the order
// recordPendingMessageDelivery states, under the advisory lock that assigns no transaction id.
//
// Two things this measurement does not hold. The request runs both of the delivery's
// transactions - the claim, whose six statements the shape names, and the settle behind the
// send, whose three are the attempt's update, the batch appending message.delivery and its
// COMMIT - and the listener call between them holds neither; but one shape judges both windows,
// so the bound of six is the claim's alone and the settle could grow to six statements with
// nothing here going red. And the claim's measured path is its widest branch only by luck of
// this fixture: the mode-change branch (recordPendingMessageDelivery, an attempt stranded
// pending in another mode) settles that attempt, appends its receipt and then opens this send's
// own, three statements where this path runs one. Nothing declares that branch. It is database
// statements throughout - an update, an event batch and an insert - which is why it is left
// undeclared rather than measured.
func xidWindowPendingDeliveryRecorded(t *testing.T, f *xidFixture) {
	parent := createIssueMessage(t, f.handler, f.issueKey, map[string]any{
		"body": "Status: the migration section is rewritten.\n\nThe outbox section stands as you read it.",
	}, "alice")
	message := createIssueMessage(t, f.handler, f.issueKey, map[string]any{
		"body": "Can this ship?", "target": "session:s1", "delivery": "steer",
		"in_reply_to": parent.ID, "in_reply_to_block": "b2",
	}, "alice")
	f.measure(t, http.StatusCreated, storetest.XidWindowOf(6,
		"the message row's lock, the read of the pending attempt it would resume, the new attempt's insert, "+
			"the block the message answers, its thread and the COMMIT ending them"), func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/messages/"+message.ID+"/deliveries",
			map[string]any{"delivery": "aside"}, "alice")
	})
}
