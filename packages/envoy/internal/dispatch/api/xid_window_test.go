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
// have run first (the block ask).

// xidProject is the fixture's project key.
const xidProject = "XID"

// xidAnchoredComments and xidAnchoredAsks are the fixture's open anchors: twenty, as on the
// harness's large specs once their comments have accumulated.
const (
	xidAnchoredComments = 12
	xidAnchoredAsks     = 8
)

// xidSpec is the fixture spec, storetest.XidSpec on the fixture's project: about 40 KB of tables
// and prose as the harness's large specs, and its requirement cells.
func xidSpec(seed uint64) (string, []string) { return storetest.XidSpec(xidProject, seed) }

// xidFixture is a server on a traced pool holding one issue whose spec is xidSpec with twenty open
// anchors on it, written by a session through the API as the harness's seed writes them.
type xidFixture struct {
	handler  http.Handler
	database *store.Store
	deps     Deps
	tracer   *storetest.XidTracer
	issueKey string
	specID   string
	spec     string
	quotes   []string
	// spare are requirement cells no fixture anchor quotes, for the measured request's own.
	spare    []string
	comments []string
	asks     []string
	// mayHoldNoXid passes a measured request that holds no xid at all
	// (storetest.XidReport.MayHoldNoXid): one that may write nothing.
	mayHoldNoXid bool
}

// newXidFixture builds the fixture on a server with options, whose tracer it sets, and with spec
// as the issue's spec (xidSpec(1) when empty).
func newXidFixture(t *testing.T, options testServerOptions, spec string) *xidFixture {
	t.Helper()
	fixture := &xidFixture{tracer: storetest.NewXidTracer()}
	if options.settle == 0 {
		options.settle = time.Hour
	}
	options.tracer = fixture.tracer
	fixture.handler, fixture.database, fixture.deps = newTestServer(t, options)
	_, fixture.quotes = xidSpec(1)
	if spec == "" {
		spec, _ = xidSpec(1)
	}
	fixture.spec = spec
	started := time.Now()
	issue := createInteractionIssue(t, fixture.handler, xidProject, "Large spec under review", spec)
	fixture.issueKey, fixture.specID = issue.Key, issue.PrimaryArtifactID

	anchors := xidAnchoredComments + xidAnchoredAsks
	step := len(fixture.quotes) / (anchors + 4)
	anchored := map[int]bool{}
	for i := range anchors {
		index := i * step
		anchored[index] = true
		quote := fixture.quotes[index]
		if i < xidAnchoredComments {
			created := sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/comments", map[string]any{
				"body":   fmt.Sprintf("Seed comment %d: does %q still hold after the migration?", i, quote),
				"anchor": map[string]any{"artifact": "spec", "quote": quote}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("seed anchored comment %d: status=%d body=%s", i, created.Code, created.Body.String())
			}
			fixture.comments = append(fixture.comments, decodeBody[model.Comment](t, created).ID)
			continue
		}
		created := sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/asks", map[string]any{
			"question": fmt.Sprintf("Seed ask %d: who owns %q?", i, quote),
			"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
			"anchor":   map[string]any{"artifact": "spec", "quote": quote}, "actor": sessionActor(),
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("seed anchored ask %d: status=%d body=%s", i, created.Code, created.Body.String())
		}
		fixture.asks = append(fixture.asks, decodeBody[model.Ask](t, created).ID)
	}
	for index := len(fixture.quotes) - 1; index >= 0 && len(fixture.spare) < 8; index-- {
		if !anchored[index] {
			fixture.spare = append(fixture.spare, fixture.quotes[index])
		}
	}
	t.Logf("xid fixture: spec of %d bytes, %d requirement cells, %d open anchored comments and %d open anchored asks, seeded in %s",
		len(spec), len(fixture.quotes), len(fixture.comments), len(fixture.asks), time.Since(started).Round(time.Millisecond))
	return fixture
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
	report.MayHoldNoXid = f.mayHoldNoXid
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

// suggest opens a session's anchored suggestion on a spare requirement cell.
func (f *xidFixture) suggest(t *testing.T) model.Comment {
	t.Helper()
	quote := f.spare[0]
	created := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
		"body": "This requirement no longer holds as worded.", "anchor": map[string]any{"artifact": "spec", "quote": quote},
		"suggestion": map[string]string{"replace_with": strings.Replace(quote, "holds", "stands", 1)}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("open suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[model.Comment](t, created)
}

// awaitSettledDocuments waits until no document owes a settlement and no backend has held an xid
// for a quiet spell, so a test that needed settlement to run first measures its request alone.
func awaitSettledDocuments(t *testing.T, database *store.Store) {
	t.Helper()
	const quiet = 300 * time.Millisecond
	deadline := time.Now().Add(30 * time.Second)
	var calm time.Time
	for time.Now().Before(deadline) {
		var busy bool
		if err := database.Pool.QueryRow(context.Background(), `
			select exists (select 1 from doc_settlements_pending)
			    or exists (select 1 from pg_stat_activity
			               where datname = current_database() and backend_xid is not null and pid <> pg_backend_pid())
		`).Scan(&busy); err != nil {
			t.Fatalf("read settlement state: %v", err)
		}
		switch {
		case busy:
			calm = time.Time{}
		case calm.IsZero():
			calm = time.Now()
		case time.Since(calm) >= quiet:
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("documents never finished settling")
}

func TestXidWindowCommentCreateAnchored(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusCreated, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
			"body": "Does this still hold once the outbox is sharded?", "anchor": map[string]any{"artifact": "spec", "quote": f.spare[0]},
			"actor": sessionActor(),
		})
	})
}

func TestXidWindowCommentCreateReplyOnAnchoredThread(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusCreated, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
			"body": "It does: the listener keeps its interests in the bucket.", "reply_to": f.comments[0], "actor": sessionActor(),
		})
	})
}

func TestXidWindowAskCreateAnchored(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusCreated, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/asks", map[string]any{
			"question": "Which team signs off on this requirement?",
			"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
			"anchor":   map[string]any{"artifact": "spec", "quote": f.spare[0]}, "actor": sessionActor(),
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
	f, askID := newBlockAskFixture(t)
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
			"text": "REST first.",
		}, "alice")
	})
}

func TestXidWindowAskResolveBlock(t *testing.T) {
	f, askID := newBlockAskFixture(t)
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/asks/"+askID+"/resolve", map[string]any{
			"kind": "resolved", "reason": "Settled in the thread",
		}, "alice")
	})
}

func TestXidWindowAskEditBlockText(t *testing.T) {
	f, askID := newBlockAskFixture(t)
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
func TestXidWindowAskedOf(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	created := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/asks", map[string]any{
		"question": "Who signs off on the migration section?",
		"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
		"anchor":   map[string]any{"artifact": "spec", "quote": f.spare[0]}, "actor": sessionActor(),
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

func TestXidWindowCommentReopen(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.resolveComment(t, f.comments[0])
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+f.comments[0]+"/reopen", nil, "alice")
	})
}

func TestXidWindowCommentEdit(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	created := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
		"body": "First wording.", "anchor": map[string]any{"artifact": "spec", "quote": f.spare[0]},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create alice's anchored comment: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{"body": "Second wording."}, "alice")
	})
}

func TestXidWindowCommentResolve(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+f.comments[0]+"/resolve", nil, "alice")
	})
}

func TestXidWindowSuggestionAccept(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	suggestion := f.suggest(t)
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+suggestion.ID+"/accept", map[string]any{}, "alice")
	})
}

func TestXidWindowSuggestionReject(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	suggestion := f.suggest(t)
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+suggestion.ID+"/reject", map[string]any{}, "alice")
	})
}

func TestXidWindowSpecEdit(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/edits", map[string]any{
			"ops":   []map[string]string{{"op": "replace", "find": "Revision token r0", "with": "Revision token r1"}},
			"actor": sessionActor(),
		})
	})
}

// An upload under the spec's name replaces it: the whole document is re-read, its anchors re-found
// and a version written.
func TestXidWindowUploadReplace(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	revised := strings.Replace(f.spec, "Revision token r0", "Revision token r1", 1)
	f.measureAdvised(t, http.StatusCreated, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
			"name": "spec.md", "content": revised, "summary": "Bump the revision", "actor": sessionActor(),
		})
	})
}

// An upload of the text a person approved carries his approval to the version it writes
// (docs.CarryRestoredApproval). Everything that carry is decided on - the document's newest
// review, the approved version's text, its decision blocks' rows and the open approval request -
// is read in the route's read phase, and what it writes - the request's answer, when one is open,
// and the review - is queued with the route's own writes, so the window is that one batch and the
// commit ending it.
func TestXidWindowUploadRestoringTheApprovedText(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	approval := reviewDocument(t, f.handler, f.specID, "alice", "approved", "")
	approved := versionMarkdown(t, f.handler, f.specID, approval.Version)
	revised := strings.Replace(f.spec, "Revision token r0", "Revision token r1", 1)
	if uploaded := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
		"name": "spec.md", "content": revised, "summary": "Bump the revision", "actor": sessionActor(),
	}); uploaded.Code != http.StatusCreated {
		t.Fatalf("stale the approval before the restore: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	waitForArtifactVersion(t, f.handler, f.specID, approval.Version+1)
	f.measureBatched(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
			"name": "spec.md", "content": approved, "actor": sessionActor(),
		})
	})
	// The measured request is the restore only while it carries the approval: a window measured
	// over a route that carried nothing would report a window this write path never ran in.
	waitForArtifactVersion(t, f.handler, f.specID, approval.Version+2)
	requireReviewCarriedWith(t, f.handler, f.specID, approval.Version+2, approval.Version, restoreSentence(approval.Version), nil)
}

// A named version of a document changed since its last version captures and renders the live
// document.
func TestXidWindowNamedVersion(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	if edited := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": "Revision token r0", "with": "Revision token r1"}},
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
	var broken *invalidLoadOnceStore
	f := newXidFixture(t, testServerOptions{
		persistence: func(database *store.Store) docs.VersionedStore {
			broken = &invalidLoadOnceStore{VersionedStore: docs.NewPgVersioned(database), invalid: make(map[string]bool)}
			return broken
		},
	}, "")
	if err := f.deps.Docs.(*docs.Service).Evict(context.Background(), f.specID); err != nil {
		t.Fatalf("evict the spec: %v", err)
	}
	broken.failNextLoad(f.specID)
	revised := strings.Replace(f.spec, "Revision token r0", "Revision token r1", 1)
	f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/rebuild", map[string]any{"markdown": revised}, "alice")
	})
}

// An approval request on a document with no open request inserts the ask, follows its author,
// indexes its question and appends ask.opened. What it decides on - the document, its settled
// version and the approval it already carries - is read before the first of those writes.
func TestXidWindowApprovalRequestOpened(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureBatched(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/approval-requests", map[string]any{
			"summary": "The migration section is rewritten; the rest of the spec stands.", "actor": sessionActor(),
		})
	})
}

// A version moved the open request, so it waits on its agent: a request naming a new summary
// rewords it (docs.RewriteApprovalAsk) and hands it back. The rewording plans the question's
// references and reads the row's edit time in the read phase, and the hand-back's update and both
// events follow in the write phase.
func TestXidWindowApprovalRequestHandedBack(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	if opened := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+f.specID+"/approval-requests", map[string]any{
		"summary": "The first pass is ready to read.", "actor": sessionActor(),
	}); opened.Code != http.StatusCreated {
		t.Fatalf("open the approval request: status=%d body=%s", opened.Code, opened.Body.String())
	}
	revised := strings.Replace(f.spec, "Revision token r0", "Revision token r1", 1)
	if uploaded := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
		"name": "spec.md", "content": revised, "summary": "Bump the revision", "actor": sessionActor(),
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
func TestXidWindowPlanReview(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	markdown, _ := xidSpec(3)
	plan := uploadDocument(t, f.handler, "/api/v1/issues/"+f.issueKey, "plan.md", markdown)
	waitForArtifactVersion(t, f.handler, plan.Artifact.ID, 1)
	f.measureBatched(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews", map[string]any{
			"version": 1, "actor": sessionActor(),
		})
	})
}

func TestXidWindowIssueCreateWithSpec(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	spec, _ := xidSpec(2)
	f.measureAdvised(t, http.StatusCreated, storetest.XidWindowOf(10,
		"the create's inline steering writes - the project's next number, the rank lock, the rank read, the issue row, its blocked_by links and its artifact row - "+
			"the two reads that load the issue it answers with, the queued batch and its COMMIT"), func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": xidProject, "title": "Second large spec for the reviewers", "spec": spec, "force": true,
			"actor": sessionActor(),
		})
	})
}

// A create naming a GitHub issue Dispatch already tracks answers that issue
// (respondWithExistingExternalIssue), with its advice. It writes nothing: today its row lock gives
// it an xid, and once the owner lock is advisory (plan M1) it may hold none at all.
func TestXidWindowExistingExternalIssue(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	if mapped := dispatchRequest(t, f.handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": xidProject,
	}, "alice"); mapped.Code != http.StatusOK {
		t.Fatalf("map the repository: status=%d body=%s", mapped.Code, mapped.Body.String())
	}
	body := map[string]any{"external": "owner/repo#41", "title": "Tracked upstream", "actor": sessionActor()}
	if created := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues", body); created.Code != http.StatusCreated {
		t.Fatalf("create the external issue: status=%d body=%s", created.Code, created.Body.String())
	}
	f.mayHoldNoXid = true
	f.measureAdvised(t, http.StatusOK, storetest.XidWindowOf(1,
		"its COMMIT alone: the answer writes nothing and locks no row, so no statement of it holds a transaction id"), func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues", body)
	})
}

func TestXidWindowIssuePatch(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusOK, storetest.XidWindowOf(5,
		"the issue row's own update, the two reads that load the issue it answers with, the queued batch and its COMMIT"), func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPatch, "/api/v1/issues/"+f.issueKey, map[string]any{
			"title": "Large spec under review, second pass", "actor": sessionActor(),
		})
	})
}

func TestXidWindowMessageCreate(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusCreated, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/messages", map[string]any{
			"body":  "Status: the spec at dispatch://" + f.issueKey + " has twenty open anchors; replies welcome.",
			"actor": sessionActor(),
		})
	})
}

func TestXidWindowClaim(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	f.measureAdvised(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
		return dispatchRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/claim", map[string]any{}, "alice")
	})
}

// A close withdraws the issue's open asks: the eight anchored asks on its spec and the block ask
// settlement opened from the spec's `:::ask` block, whose block and copies it writes resolved into
// the spec (docs.WriteIssueAskStates) before it records each ask withdrawn. That document work
// reads and rewrites the whole spec, so it is the close's read phase, before its first write.
func TestXidWindowIssueCloseWithOpenAsks(t *testing.T) {
	f, _ := newBlockAskFixture(t)
	f.measure(t, http.StatusOK, storetest.XidWindowOf(5,
		"the issue row's own update, the two reads that load the issue it answers with, the batch withdrawing its asks and its COMMIT"), func(h http.Handler) *httptest.ResponseRecorder {
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
func TestXidWindowResultRecorded(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	verification, _ := xidSpec(4)
	uploaded := uploadDocument(t, f.handler, "/api/v1/issues/"+f.issueKey, "verification.md", verification+"\n"+twoChecks)
	waitForArtifactVersion(t, f.handler, uploaded.Artifact.ID, 1)
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
func TestXidWindowResultRetracted(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	verification, _ := xidSpec(4)
	uploaded := uploadDocument(t, f.handler, "/api/v1/issues/"+f.issueKey, "verification.md", verification+"\n"+twoChecks)
	waitForArtifactVersion(t, f.handler, uploaded.Artifact.ID, 1)
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
func TestXidWindowUserIssueState(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
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
func TestXidWindowUserThreadRead(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
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
// lockedCommentForDelivery); the climb from it to the head of its thread, the ask it hangs
// under (threadHeadOf); the block the reply names, with the index its comment may owe planned
// there and the block itself addressed there, so the reply carries it instead of reading it
// back; and the reply's own block index and references, planned for an id and a created_at
// reserved there (planCommentReply). Each refusal those reads raise is raised where it was
// raised, after the mention and the attempt have been checked, so the answers of this route are
// what they were. The mention rows are read without a lock, so the window opens at the attempt
// row.
func TestXidWindowCommentDeliveryReply(t *testing.T) {
	f := newXidDeliveryFixture(t)
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
func TestXidWindowMessageDeliveryReply(t *testing.T) {
	f := newXidDeliveryFixture(t)
	message := createIssueMessage(t, f.handler, f.issueKey, map[string]any{
		"body":   "Can this ship?\n\nThe migration section is the one to read.",
		"target": "session:s1", "delivery": "steer",
	}, "alice")
	f.measure(t, http.StatusCreated, storetest.XidWindowOf(10,
		"the parent message's lock, the attempt row's lock, the reply's insert, the one batch writing "+
			"its block index and its references, its thread, the thread's logins, the batch appending "+
			"message.answered, the stamp of its references, the attempt's update and the COMMIT ending "+
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
func TestXidWindowDeliveryAccepted(t *testing.T) {
	f := newXidDeliveryFixture(t)
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
func TestXidWindowPendingDeliveryRecorded(t *testing.T) {
	f := newXidDeliveryFixture(t)
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
