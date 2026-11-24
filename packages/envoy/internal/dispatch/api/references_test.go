package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/testwait"
)

func createReferenceAPIProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key + " project",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create project %q: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

func createReferenceAPIIssue(t *testing.T, handler http.Handler, project string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": "Reference issue",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create reference issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

func createReferenceAPIComment(t *testing.T, handler http.Handler, issueKey, body string) model.Comment {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]string{
		"body": body,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create reference comment: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Comment](t, response)
}

// createReferenceAPIAsk asks question on the issue as alice with two options, and returns the
// ask. Its parts are its question, b1, and its options, b2 and b3.
func createReferenceAPIAsk(t *testing.T, handler http.Handler, issueKey, question string) model.Ask {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": question, "options": []map[string]string{{"label": "Yes"}, {"label": "No"}},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create reference ask: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response)
}

func issueReferencesRequest(t *testing.T, handler http.Handler, issueKey, etag string) *httptest.ResponseRecorder {
	t.Helper()
	return issueReferencesQueryRequest(t, handler, issueKey, "", etag)
}

// issueReferencesQueryRequest reads an issue's references as alice with query as the raw query
// string ("" for none) and etag, when set, as If-None-Match.
func issueReferencesQueryRequest(t *testing.T, handler http.Handler, issueKey, query, etag string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/issues/" + issueKey + "/references"
	if query != "" {
		target += "?" + query
	}
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// An issue's references read answers in members each document the issue's own content links to,
// at depth 1 and with the node it links from as its via, under an ETag that a conditional read
// matches and a new version of the member changes. Diagram PNG, which only that member links to,
// is two hops from the issue and no member, since the read reads one hop.
func TestIssueReferencesClosureDepthViaAndETag(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	diagram := createProjectDocument(t, handler, "OPS", "Diagram PNG", "# Diagram\n")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\ndispatch://OPS/artifact/diagram-png\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")

	var crossProjectReferences int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'artifact' and to_id = $2
	`, design.ID, diagram.RefKey).Scan(&crossProjectReferences); err != nil {
		t.Fatalf("read cross-project reference: %v", err)
	}
	if crossProjectReferences != 1 {
		t.Fatalf("cross-project reference rows = %d; want 1", crossProjectReferences)
	}

	first := issueReferencesRequest(t, handler, issue.Key, "")
	if first.Code != http.StatusOK {
		t.Fatalf("get issue references: status=%d body=%s", first.Code, first.Body.String())
	}
	firstValue := decodeBody[model.IssueReferences](t, first)
	if firstValue.Truncated || len(firstValue.Members) != 1 {
		t.Fatalf("issue references = %#v; want one untruncated member, Design notes, and not Diagram PNG two hops away", firstValue)
	}
	firstMember := firstValue.Members[0]
	if firstMember.Artifact.ID != design.ID || firstMember.Depth != 1 || firstMember.Via != (model.ReferenceVia{Kind: "comment", ID: comment.ID}) {
		t.Fatalf("reference member = %#v; want Design notes at depth 1 through comment %q", firstMember, comment.ID)
	}
	firstETag := first.Header().Get("ETag")
	if firstETag == "" {
		t.Fatal("issue references response has no ETag")
	}

	notModified := issueReferencesRequest(t, handler, issue.Key, firstETag)
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 || notModified.Header().Get("ETag") != firstETag {
		t.Fatalf("conditional issue references: status=%d etag=%q body=%q", notModified.Code, notModified.Header().Get("ETag"), notModified.Body.String())
	}

	updated := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/artifacts", map[string]any{
		"name": "Design notes", "content": "# Design\nNo references\n",
		"precondition": uploadBase(t, handler, design.ID),
	}, "alice")
	if updated.Code != http.StatusCreated {
		t.Fatalf("update Design notes: status=%d body=%s", updated.Code, updated.Body.String())
	}
	second := issueReferencesRequest(t, handler, issue.Key, firstETag)
	if second.Code != http.StatusOK {
		t.Fatalf("read changed issue references: status=%d body=%s", second.Code, second.Body.String())
	}
	secondValue := decodeBody[model.IssueReferences](t, second)
	if secondValue.Truncated || len(secondValue.Members) != 1 || secondValue.Members[0].Artifact.ID != design.ID || secondValue.Members[0].Depth != 1 {
		t.Fatalf("references after a new version of Design notes = %#v; want only Design notes, at depth 1", secondValue)
	}
	if second.Header().Get("ETag") == firstETag {
		t.Fatal("reference ETag did not change after linked document version changed")
	}
}

func TestIssueReferencesExcludeOwnArtifactsAndDanglingTargets(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	createReferenceAPIComment(t, handler, issue.Key, "dispatch://"+issue.Key+"/spec dispatch://CORE/artifact/ghost")

	response := issueReferencesRequest(t, handler, issue.Key, "")
	if response.Code != http.StatusOK {
		t.Fatalf("get issue references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.IssueReferences](t, response)
	if value.Truncated || len(value.Members) != 0 {
		t.Fatalf("issue references = %#v; want no own or dangling artifacts", value)
	}
}

// A chain of ten documents, each linking to the next, that the issue's comment enters at its first
// answers that first document alone, at depth 1, and truncated false: the read reads one hop, so it
// is never cut. The test keeps the name it had when the read walked eight hops and marked a ninth
// truncated, the name plan.md's and verification.md's lists of checks give it.
func TestIssueReferencesTruncateBeyondDepthEight(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	labels := map[string]string{}
	var first model.Artifact
	for index := 1; index <= 10; index++ {
		name := "Document " + strconv.Itoa(index)
		content := "# " + name + "\n"
		if index < 10 {
			content += "dispatch://CORE/artifact/document-" + strconv.Itoa(index+1) + "\n"
		}
		document := createProjectDocument(t, handler, "CORE", name, content)
		labels[document.ID] = "the chain's document " + strconv.Itoa(index)
		if index == 1 {
			first = document
		}
	}
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/document-1")
	labels[comment.ID] = "the issue's comment"

	response := issueReferencesRequest(t, handler, issue.Key, "")
	if response.Code != http.StatusOK {
		t.Fatalf("get issue references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.IssueReferences](t, response)
	if value.Truncated {
		t.Error("the answer is truncated; a read of one hop is never cut")
	}
	want := []model.ReferenceMember{{Artifact: model.Artifact{ID: first.ID}, Depth: 1, Via: model.ReferenceVia{Kind: "comment", ID: comment.ID}}}
	if got, want := referenceMemberLines(value.Members, labels), referenceMemberLines(want, labels); !slices.Equal(got, want) {
		t.Errorf("members hold\n\t%s\nwant the chain's first document alone, at depth 1\n\t%s", shownLines(got), shownLines(want))
	}
}

// ?versions=all is the full answer, byte for byte and under the same ETag, as no parameter is;
// any value but all and latest is refused, and so is a versions given more than once.
func TestIssueReferencesVersionsAllIsTheFullAnswerAndOtherValuesAreRefused(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")
	seedReferenceDocumentHistory(t, database, design.ID, 4, 0, `{"kind":"human","id":"alice"}`)
	createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/"+design.Slug)

	plain := issueReferencesRequest(t, handler, issue.Key, "")
	if plain.Code != http.StatusOK {
		t.Fatalf("get issue references: status=%d body=%s", plain.Code, plain.Body.String())
	}
	if members := decodeBody[model.IssueReferences](t, plain).Members; len(members) != 1 || len(members[0].Artifact.Versions) != 4 {
		t.Fatalf("issue references members = %#v; want Design notes with 4 versions", members)
	}
	all := issueReferencesQueryRequest(t, handler, issue.Key, "versions=all", "")
	if all.Code != plain.Code ||
		all.Header().Get("ETag") != plain.Header().Get("ETag") ||
		all.Header().Get("Content-Type") != plain.Header().Get("Content-Type") ||
		!bytes.Equal(all.Body.Bytes(), plain.Body.Bytes()) {
		t.Fatalf("versions=all: status=%d etag=%q type=%q body=%s\nwant status=%d etag=%q type=%q body=%s",
			all.Code, all.Header().Get("ETag"), all.Header().Get("Content-Type"), all.Body.String(),
			plain.Code, plain.Header().Get("ETag"), plain.Header().Get("Content-Type"), plain.Body.String())
	}

	for _, query := range []string{"versions=bogus", "versions=latest&versions=bogus", "versions=all&versions=latest"} {
		refused := issueReferencesQueryRequest(t, handler, issue.Key, query, "")
		if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"INVALID_QUERY"`) {
			t.Fatalf("%s: status=%d body=%s; want 400 INVALID_QUERY", query, refused.Code, refused.Body.String())
		}
	}
}

func TestArtifactReferencesListOutgoingAndReferencedBy(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	diagram := createProjectDocument(t, handler, "OPS", "Diagram PNG", "# Diagram\n")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\ndispatch://OPS/artifact/diagram-png\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+design.ID+"/references", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("get artifact references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.ArtifactReferences](t, response)
	if len(value.Outgoing) != 1 || value.Outgoing[0].Kind != "artifact" || value.Outgoing[0].ToID != diagram.RefKey || value.Outgoing[0].Artifact == nil || value.Outgoing[0].Artifact.ID != diagram.ID {
		t.Fatalf("outgoing references = %#v; want resolved Diagram PNG", value.Outgoing)
	}
	if len(value.ReferencedBy) != 1 || value.ReferencedBy[0].Kind != "comment" || value.ReferencedBy[0].ID != comment.ID || value.ReferencedBy[0].IssueKey == nil || *value.ReferencedBy[0].IssueKey != issue.Key {
		t.Fatalf("referenced by = %#v; want issue comment %q", value.ReferencedBy, comment.ID)
	}
}

func TestIssueReferencesRejectUnauthenticatedRequest(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	// Authentication comes before the versions parameter: a value the route refuses still
	// answers 401 to a caller with no identity.
	for _, target := range []string{"/api/v1/issues/CORE-1/references", "/api/v1/issues/CORE-1/references?versions=bogus"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"NO_IDENTITY"`) {
			t.Fatalf("unauthenticated %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

// A closure walk that runs past referencesClosureBound answers 503 REFERENCES_TIMEOUT at about
// that bound and gives its connection back. refs is locked in another connection, so the walk,
// which reads refs, waits however fast it would run, and the bound is what ends it. The control
// is the first read, before the lock: the same request answers 200.
func TestIssueReferencesRefuseAtTheClosureBound(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	if response := issueReferencesRequest(t, handler, issue.Key, ""); response.Code != http.StatusOK {
		t.Fatalf("references before the lock: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	holdLock(t, database, "lock table refs in access exclusive mode")
	acquired := database.Pool.Stat().AcquiredConns()
	started := time.Now()
	response := issueReferencesRequest(t, handler, issue.Key, "")
	took := time.Since(started)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("references past the bound: status=%d body=%s, want 503", response.Code, response.Body.String())
	}
	if refusal := decodeBody[apiErrorBody](t, response); refusal.Code != "REFERENCES_TIMEOUT" || !strings.Contains(refusal.Error, referencesClosureBound.String()) {
		t.Errorf("references past the bound answered %s, want REFERENCES_TIMEOUT naming the %s bound", response.Body.String(), referencesClosureBound)
	}
	if took < referencesClosureBound || took > referencesClosureBound+3*time.Second {
		t.Errorf("references past the bound answered after %s, want about its %s bound", took, referencesClosureBound)
	}
	var conns int32
	testwait.Holds(2*time.Second, 50*time.Millisecond, func() bool {
		conns = database.Pool.Stat().AcquiredConns()
		return conns <= acquired
	})
	if conns > acquired {
		t.Errorf("the pool holds %d acquired connections after the answer, more than the %d it held before the request: the walk kept its connection", conns, acquired)
	}
}

// The statements of an issue's references read share the one budget of referencesClosureBound
// (c-bound-later). The issue's links hold an issue and an ask, whose node loaders,
// loadIssueNodes and loadAskNodes, are the only statements of the read that read events, and
// they run after the out and in statements. events is locked, and refs is locked for 1.5 s, from
// other connections: the out statement waits out the lock on refs, and a loader then waits on
// events. A read that gave each statement the whole bound would wait 1.5 s and then 2 s more, and
// the context's 3 s would end it; with one budget the loader's statement ends at 2 s and the route
// answers 503 REFERENCES_TIMEOUT then. The read gives its connection back: the pool then holds
// exactly the connections it held with events locked, the lock's own among them. The control is
// the first read, before the locks: the same request answers 200.
func TestIssueReferencesRefuseWhenALaterStatementReachesTheBound(t *testing.T) {
	t.Parallel()
	// No document settles during the test, so no write of the server's own waits on events with a
	// connection of the pool.
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	linked := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "CORE", "title": "The issue the comment waits on", "force": true,
	}))
	ask := createReferenceAPIAsk(t, handler, linked.Key, "Is the issue the comment waits on done?")
	createReferenceAPIComment(t, handler, issue.Key, "It waits on dispatch://"+linked.Key+" and dispatch://"+linked.Key+"/ask/"+ask.ID+" here.")
	if response := issueReferencesRequest(t, handler, issue.Key, ""); response.Code != http.StatusOK {
		t.Fatalf("references before the locks: status=%d body=%s, want 200", response.Code, response.Body.String())
	}

	// holdLock's cleanup releases events at the end, whatever the test found.
	holdLock(t, database, "lock table events in access exclusive mode")
	acquired := database.Pool.Stat().AcquiredConns()
	releaseRefs := holdLock(t, database, "lock table refs in access exclusive mode")
	refsReleased := make(chan struct{})
	go func() {
		defer close(refsReleased)
		time.Sleep(1500 * time.Millisecond)
		releaseRefs()
	}()
	// Cleanups run last first, so this wait ends before holdLock's own release of refs runs.
	t.Cleanup(func() { <-refsReleased })
	started := time.Now()
	response := issueReferencesRequest(t, handler, issue.Key, "")
	took := time.Since(started)
	<-refsReleased
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("references with events locked: status=%d after %s body=%s, want 503 after %s", response.Code, took, response.Body.String(), referencesClosureBound)
	}
	if refusal := decodeBody[apiErrorBody](t, response); refusal.Code != "REFERENCES_TIMEOUT" || !strings.Contains(refusal.Error, referencesClosureBound.String()) {
		t.Errorf("references with events locked answered %s, want REFERENCES_TIMEOUT naming the %s bound", response.Body.String(), referencesClosureBound)
	}
	if limit := referencesClosureBound + 500*time.Millisecond; took < referencesClosureBound || took >= limit {
		t.Errorf("references with events locked answered after %s, want at least its %s bound and less than %s", took, referencesClosureBound, limit)
	}
	var conns int32
	testwait.Holds(2*time.Second, 50*time.Millisecond, func() bool {
		conns = database.Pool.Stat().AcquiredConns()
		return conns == acquired
	})
	if conns != acquired {
		t.Errorf("the pool holds %d acquired connections after the answer, want the %d it held with events locked", conns, acquired)
	}
}

// recordingTx is a transaction that records each statement sent through it before sending it.
type recordingTx struct {
	pgx.Tx
	statements []string
}

func (r *recordingTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	r.statements = append(r.statements, sql)
	return r.Tx.Exec(ctx, sql, arguments...)
}

func (r *recordingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.statements = append(r.statements, sql)
	return r.Tx.Query(ctx, sql, args...)
}

func (r *recordingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.statements = append(r.statements, sql)
	return r.Tx.QueryRow(ctx, sql, args...)
}

// The budget function of the read's statements (setStatementBudget) refuses with the bound's
// error and sends no statement when less than 1 ms is left, since a statement_timeout of 0 turns
// the timeout off, and sets the timeout to 1ms at 1 ms left. The transaction starts with a
// statement_timeout of its own, 1234ms, which the refusal must leave as it is.
func TestReferencesBudgetRefusesUnderOneMillisecond(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	// The read of the setting runs under the 1ms it set, so a backend kept off the CPU that long
	// cancels it (SQLSTATE 57014). Such an attempt shows nothing of the setting, and the case runs
	// again in a transaction of its own.
	for attempt := 1; !referencesBudgetCase(t, database); attempt++ {
		if attempt == 5 {
			t.Fatalf("the read of statement_timeout after a budget of 1 ms was cancelled %d times in a row", attempt)
		}
	}
}

// referencesBudgetCase runs both of the budget's cases in one transaction of database, and
// answers false when the read of the setting the 1 ms case set was cancelled by that setting.
func referencesBudgetCase(t *testing.T, database *store.Store) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the budget's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "set local statement_timeout = '1234ms'"); err != nil {
		t.Fatalf("set the transaction's own statement_timeout: %v", err)
	}
	setting := func() (string, error) {
		var value string
		err := tx.QueryRow(ctx, "select current_setting('statement_timeout')").Scan(&value)
		return value, err
	}

	recorded := &recordingTx{Tx: tx}
	if err := setStatementBudget(ctx, recorded, 900*time.Microsecond); !errors.Is(err, errClosureBudgetSpent) || !closureBoundHit(err) {
		t.Errorf("with 0.9 ms left the budget answered %v, want the bound's error, %q, which closureBoundHit reads as the bound", err, errClosureBudgetSpent)
	}
	if len(recorded.statements) > 0 {
		t.Errorf("with 0.9 ms left the budget sent %q, want no statement", recorded.statements)
	}
	if value, err := setting(); err != nil || value != "1234ms" {
		t.Errorf("statement_timeout after the budget refused 0.9 ms reads %q (%v), want the 1234ms it had", value, err)
	}

	if err := setStatementBudget(ctx, tx, time.Millisecond); err != nil {
		t.Fatalf("with 1 ms left the budget answered %v, want the timeout set", err)
	}
	value, err := setting()
	if closureBoundHit(err) {
		return false
	}
	if err != nil || value != "1ms" {
		t.Errorf("statement_timeout after a budget of 1 ms reads %q (%v), want 1ms", value, err)
	}
	return true
}

// fixtureLabel is what labels calls the node or the via id, for a failure to name.
func fixtureLabel(labels map[string]string, id string) string {
	if label, found := labels[id]; found {
		return label
	}
	return "no node of the fixture"
}

// referenceLinkLine is one row of links or linked_from as the tests compare it: its node, the
// kinds of its links and its via, each id followed by its label. The row's name is held apart,
// against the name the graph read gives the node.
func referenceLinkLine(row model.ReferenceLink, labels map[string]string) string {
	return fmt.Sprintf("%s %s (%s) [%s] via %s %s (%s)", row.Node.Kind, row.Node.ID, fixtureLabel(labels, row.Node.ID),
		strings.Join(row.Kinds, ","), row.Via.Kind, row.Via.ID, fixtureLabel(labels, row.Via.ID))
}

// referenceLinkLines is each of rows as referenceLinkLine writes it, sorted.
func referenceLinkLines(rows []model.ReferenceLink, labels map[string]string) []string {
	lines := make([]string, len(rows))
	for index, row := range rows {
		lines[index] = referenceLinkLine(row, labels)
	}
	slices.Sort(lines)
	return lines
}

// referenceMemberLines is each member as its artifact, its depth and its via, sorted.
func referenceMemberLines(members []model.ReferenceMember, labels map[string]string) []string {
	lines := make([]string, len(members))
	for index, member := range members {
		lines[index] = fmt.Sprintf("%s (%s) depth %d via %s %s (%s)", member.Artifact.ID, fixtureLabel(labels, member.Artifact.ID),
			member.Depth, member.Via.Kind, member.Via.ID, fixtureLabel(labels, member.Via.ID))
	}
	slices.Sort(lines)
	return lines
}

// shownLines is lines one to a line, or "(none)".
func shownLines(lines []string) string {
	if len(lines) == 0 {
		return "(none)"
	}
	return strings.Join(lines, "\n\t")
}

// insertRemovedItemBlock writes block b9 of the item id of kind (comment, message or ask) as a
// later revision leaves a block it removed: with no position, and the time it was removed. No
// route writes a removed block of an item at a time the test chooses, and none removes a block of
// a message, which never changes.
func insertRemovedItemBlock(t *testing.T, database *store.Store, kind, id string) {
	t.Helper()
	statements := map[string]string{
		"comment": `insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt, changed_revision, changed_at, removed_at)
			values ($1::uuid, 'b9', $2, null, 'paragraph', 'sum', 'A removed block.', 1, now(), now())`,
		"message": `insert into message_blocks (message_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_at, removed_at)
			values ($1::uuid, 'b9', $2, null, 'paragraph', 'sum', 'A removed block.', now(), now(), now())`,
		"ask": `insert into ask_blocks (ask_id, block_id, node_id, position, type, text_sha256, excerpt, changed_revision, changed_at, removed_at)
			values ($1::uuid, 'b9', $2, null, 'option', 'sum', 'A removed option.', 1, now(), now())`,
	}
	if _, err := database.Pool.Exec(context.Background(), statements[kind], id, kind+"/"+id+"#b9"); err != nil {
		t.Fatalf("write a removed block of %s %s: %v", kind, id, err)
	}
}

// insertRemovedBlockLink writes a link from the removed block b9 of the item id of kind to the
// node toKind toID, as the index wrote it while a revision held the block.
func insertRemovedBlockLink(t *testing.T, database *store.Store, kind, id, toKind, toID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), fmt.Sprintf(`
		insert into %[1]s_block_refs (%[1]s_id, block_id, from_node, to_kind, to_id) values ($1::uuid, 'b9', $2, $3, $4)
	`, kind), id, kind+"/"+id+"#b9", toKind, toID); err != nil {
		t.Fatalf("write a link of the removed block of %s %s: %v", kind, id, err)
	}
}

// An issue's references read answers, in links and linked_from, one row for each node at the
// other end of a link of the issue's own content, one hop each way (c-api-answers).
//
// The issue X is a child of P and the parent of C, and its own content is its spec, which holds a
// requirement, a checks document, a comment, an ask and a message. X's comment links to another
// issue O and to O's ask, comment, message and spec; to a block of a second document, a part of a
// second ask and a block of a second comment of O, which nothing else links to; and to X itself
// and X's spec. X's checks point with serves at a requirement of P's spec, the third document, and
// at a requirement r2 of P's design document, the fourth, whose type then changes to a callout, so
// that pointer is hidden and its target still stands; and its text links to a comment of O. Q's
// documents, a block of one, its comments, a message of it that holds two links, and its ask's
// question link to X, X's spec, a block of it, X's comment, X's ask, a part of it and X's message,
// and C's checks point with serves at X's requirement.
//
// Then the links that give no row, as a revision leaves them: a part of X's ask that a later
// revision removed holds a link; Q links into a removed block of X's comment, a removed block of
// X's message and that removed part of X's ask; and a removed block of a comment of Q holds a link
// into X's message. The test writes those blocks and the links they hold itself
// (insertRemovedItemBlock), since no route writes them.
//
// links holds each node X's content links to once, with the kinds of its links and the lowest of
// X's nodes that links to it, a document by its uuid: a block or a part counts as its document or
// item, and the third document counts with serves alone. linked_from holds each source once, with
// the lowest of X's nodes it links to, a document by its ref_key and a part by its ask, and C's
// checks with serves alone. Neither holds X, its own content, P or C, which only structural edges
// join to X, the fourth document, or a node whose only link a removed block or part holds or
// takes. Each row's name is the name the graph read gives its node, members are the documents of
// links at depth 1, and truncated is false. The ETag moves when another document links to X's spec
// later and when the comment X's checks link to is edited, and neither moves X's last_seq or the
// kinds and via of any row.
func TestIssueReferencesLinksListEachNodeOnce(t *testing.T) {
	t.Parallel()
	// No document settles during the test, so no version but the test's own moves the ETag.
	handler, database := blockIndexHandler(t)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	createIssue := func(project, title, parent, spec string) model.Issue {
		t.Helper()
		body := map[string]any{"project": project, "title": title, "spec": spec, "force": true}
		if parent != "" {
			body["parent"] = parent
		}
		return requireCreatedIssue(t, createIssueRequest(t, handler, body))
	}
	upload := func(issueKey, name, content string) model.Artifact {
		t.Helper()
		return uploadDocument(t, handler, "/api/v1/issues/"+issueKey, name, content).Artifact
	}
	message := func(issueKey, body string) model.Message {
		t.Helper()
		return createIssueMessage(t, handler, issueKey, map[string]any{"body": body}, "alice")
	}

	parent := createIssue("CORE", "The parent of the issue whose references are read", "", ":::requirement{#r1}\nThe parent's requirement.\n:::\n")
	design := upload(parent.Key, "design.md", ":::requirement{#r2}\nThe parent's second requirement.\n:::\n")
	issue := createIssue("CORE", "The issue whose references are read", parent.Key, ":::requirement{#r1}\nThe issue's own requirement.\n:::\n")
	issueRef, specRef := "dispatch://"+issue.Key, "dispatch://"+issue.Key+"/spec"
	var specRefKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, issue.PrimaryArtifactID).Scan(&specRefKey); err != nil {
		t.Fatalf("read the ref_key of the issue's spec: %v", err)
	}
	child := createIssue("CORE", "The child of the issue whose references are read", issue.Key, "The child's spec.\n")
	childChecks := upload(child.Key, "checks.md", ":::check{#k1 serves=\""+specRef+"#b-r1\"}\nIt serves the issue's requirement.\n:::\n")

	other := createIssue("OPS", "Another issue the comment links to", "", "Another issue's spec.\n")
	otherRef := "dispatch://" + other.Key
	otherAsk := createReferenceAPIAsk(t, handler, other.Key, "Does another issue's ask settle it?")
	otherComment := createReferenceAPIComment(t, handler, other.Key, "Another issue's comment.")
	otherMessage := message(other.Key, "Another issue's message.")
	second := upload(other.Key, "second.md", ":::requirement{#q1}\nA block of the second document.\n:::\n")
	secondAsk := createReferenceAPIAsk(t, handler, other.Key, "Is this the second ask?")
	secondComment := createReferenceAPIComment(t, handler, other.Key, "The second comment.")
	rested := createReferenceAPIComment(t, handler, other.Key, "The comment the issue's checks rest on.")
	removedTarget := upload(other.Key, "removed-part-target.md", "Only a removed part of the issue's ask links here.\n")

	checks := upload(issue.Key, "checks.md",
		":::check{#k1 serves=\"dispatch://"+parent.Key+"/spec#b-r1\"}\nIt serves the parent's requirement.\n:::\n\n"+
			":::check{#k2 serves=\"dispatch://"+parent.Key+"/artifact/"+design.Slug+"#b-r2\"}\nIt serves the parent's second requirement.\n:::\n\n"+
			"It rests on "+otherRef+"/comment/"+rested.ID+" here.\n")
	// A write refuses a pointer at a block of a type the attribute does not allow
	// (pmdoc.CheckBlockPointers), so the target's type changes after the pointer is written.
	editIssueDocument(t, handler, parent.Key, design.Slug, []map[string]any{{"op": "retype", "block": "r2", "type": "callout"}})
	var pointers int
	var targetType string
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from block_refs where artifact_id = $1 and block_id = 'k2' and kind = 'serves' and to_kind = 'block' and to_id = $2),
		       coalesce((select type from document_blocks where node_id = $2), '')
	`, checks.ID, design.RefKey+"#r2").Scan(&pointers, &targetType); err != nil {
		t.Fatalf("read the pointer at the fourth document: %v", err)
	}
	if pointers != 1 || targetType != "callout" {
		t.Fatalf("checks.md's serves pointer at %s#r2 has %d block_refs rows, and its target's block is of type %q; want the row kept and the block a callout, so the pointer is hidden and not dangling",
			design.RefKey, pointers, targetType)
	}

	ask := createReferenceAPIAsk(t, handler, issue.Key, "Which way does the issue go?")
	issueMessage := message(issue.Key, "A message on the issue.")
	comment := createReferenceAPIComment(t, handler, issue.Key, strings.Join([]string{
		"It links", otherRef, otherRef + "/ask/" + otherAsk.ID, otherRef + "/comment/" + otherComment.ID,
		otherRef + "/message/" + otherMessage.ID, otherRef + "/spec", otherRef + "/artifact/" + second.Slug + "#b-q1",
		otherRef + "/ask/" + secondAsk.ID + "#b-b1", otherRef + "/comment/" + secondComment.ID + "#b-b1",
		issueRef, specRef, "here.",
	}, " "))
	insertRemovedItemBlock(t, database, "ask", ask.ID)
	insertRemovedBlockLink(t, database, "ask", ask.ID, "artifact", removedTarget.RefKey)
	insertRemovedItemBlock(t, database, "comment", comment.ID)
	insertRemovedItemBlock(t, database, "message", issueMessage.ID)

	citing := createIssue("OPS", "The issue whose content links to the issue whose references are read", "", "The citing issue's spec.\n")
	citesIssue := upload(citing.Key, "cites-the-issue.md", "It cites "+issueRef+" here.\n")
	citesBlock := upload(citing.Key, "cites-a-block.md", "It cites "+specRef+"#b-r1 here.\n")
	citesSpec := createReferenceAPIComment(t, handler, citing.Key, "It cites "+specRef+" here.")
	citesTwo := message(citing.Key, "It cites "+issueRef+"/comment/"+comment.ID+" and "+issueRef+"/ask/"+ask.ID+" here.")
	citesMessage := createReferenceAPIAsk(t, handler, citing.Key, "Does "+issueRef+"/message/"+issueMessage.ID+" settle it?")
	citesPart := createReferenceAPIComment(t, handler, citing.Key, "It cites "+issueRef+"/ask/"+ask.ID+"#b-b2 here.")
	intoRemovedComment := createReferenceAPIComment(t, handler, citing.Key, "It cites "+issueRef+"/comment/"+comment.ID+"#b-b9 here.")
	intoRemovedMessage := message(citing.Key, "It cites "+issueRef+"/message/"+issueMessage.ID+"#b-b9 here.")
	intoRemovedPart := upload(citing.Key, "cites-a-removed-part.md", "It cites "+issueRef+"/ask/"+ask.ID+"#b-b9 here.\n")
	removedSource := createReferenceAPIComment(t, handler, citing.Key, "A comment whose removed block links to the issue's message.")
	insertRemovedItemBlock(t, database, "comment", removedSource.ID)
	insertRemovedBlockLink(t, database, "comment", removedSource.ID, "message", issueMessage.ID)

	labels := map[string]string{
		issue.Key: "the issue", parent.Key: "its parent", child.Key: "its child",
		issue.PrimaryArtifactID: "the issue's spec", specRefKey: "the issue's spec", checks.ID: "the issue's checks",
		ask.ID: "the issue's ask", comment.ID: "the issue's comment", issueMessage.ID: "the issue's message",
		other.Key: "another issue", otherAsk.ID: "its ask", otherComment.ID: "its comment", otherMessage.ID: "its message",
		other.PrimaryArtifactID: "its spec", second.ID: "the second document", secondAsk.ID: "the second ask",
		secondComment.ID: "the second comment", parent.PrimaryArtifactID: "the third document, the parent's spec",
		design.ID: "the fourth document, behind a hidden pointer", rested.ID: "the comment the checks rest on",
		removedTarget.ID: "the target of a removed part's link", childChecks.ID: "the child's checks",
		citesIssue.ID: "a document citing the issue", citesBlock.ID: "a document citing a block of the spec",
		citesSpec.ID: "a comment citing the spec", citesTwo.ID: "a message citing the comment and the ask",
		citesMessage.ID: "an ask citing the message", citesPart.ID: "a comment citing a part of the ask",
		intoRemovedComment.ID: "a comment citing a removed block of the comment",
		intoRemovedMessage.ID: "a message citing a removed block of the message",
		intoRemovedPart.ID:    "a document citing a removed part of the ask",
		removedSource.ID:      "a comment whose removed block cites the message",
	}
	linkRow := func(kind, id string, kinds []string, viaKind, viaID string) model.ReferenceLink {
		return model.ReferenceLink{Node: model.GraphNode{Kind: kind, ID: id}, Kinds: kinds, Via: model.ReferenceVia{Kind: viaKind, ID: viaID}}
	}
	mentions, serves := []string{"mentions"}, []string{"serves"}
	wantLinks := []model.ReferenceLink{
		linkRow("issue", other.Key, mentions, "comment", comment.ID),
		linkRow("ask", otherAsk.ID, mentions, "comment", comment.ID),
		linkRow("comment", otherComment.ID, mentions, "comment", comment.ID),
		linkRow("message", otherMessage.ID, mentions, "comment", comment.ID),
		linkRow("artifact", other.PrimaryArtifactID, mentions, "comment", comment.ID),
		linkRow("artifact", second.ID, mentions, "comment", comment.ID),
		linkRow("ask", secondAsk.ID, mentions, "comment", comment.ID),
		linkRow("comment", secondComment.ID, mentions, "comment", comment.ID),
		linkRow("artifact", parent.PrimaryArtifactID, serves, "artifact", checks.ID),
		linkRow("comment", rested.ID, mentions, "artifact", checks.ID),
	}
	wantLinkedFrom := []model.ReferenceLink{
		linkRow("artifact", citesIssue.ID, mentions, "issue", issue.Key),
		linkRow("artifact", citesBlock.ID, mentions, "artifact", specRefKey),
		linkRow("comment", citesSpec.ID, mentions, "artifact", specRefKey),
		linkRow("message", citesTwo.ID, mentions, "ask", ask.ID),
		linkRow("ask", citesMessage.ID, mentions, "message", issueMessage.ID),
		linkRow("comment", citesPart.ID, mentions, "ask", ask.ID),
		linkRow("artifact", childChecks.ID, serves, "artifact", specRefKey),
	}
	wantMembers := []model.ReferenceMember{
		{Artifact: model.Artifact{ID: other.PrimaryArtifactID}, Depth: 1, Via: model.ReferenceVia{Kind: "comment", ID: comment.ID}},
		{Artifact: model.Artifact{ID: second.ID}, Depth: 1, Via: model.ReferenceVia{Kind: "comment", ID: comment.ID}},
		{Artifact: model.Artifact{ID: parent.PrimaryArtifactID}, Depth: 1, Via: model.ReferenceVia{Kind: "artifact", ID: checks.ID}},
	}
	// listedNever is each node neither field may list, and why.
	listedNever := map[[2]string]string{
		{"issue", issue.Key}:                  "the issue itself",
		{"artifact", issue.PrimaryArtifactID}: "the issue's own spec",
		{"artifact", checks.ID}:               "the issue's own checks",
		{"ask", ask.ID}:                       "the issue's own ask",
		{"comment", comment.ID}:               "the issue's own comment",
		{"message", issueMessage.ID}:          "the issue's own message",
		{"issue", parent.Key}:                 "its parent, which only a structural edge joins to it",
		{"issue", child.Key}:                  "its child, which only a structural edge joins to it",
		{"artifact", design.ID}:               "the fourth document, which only a hidden pointer reaches",
		{"artifact", removedTarget.ID}:        "a node only a removed part of the issue's ask links to",
		{"comment", intoRemovedComment.ID}:    "a source whose only link is into a removed block of the issue's comment",
		{"message", intoRemovedMessage.ID}:    "a source whose only link is into a removed block of the issue's message",
		{"artifact", intoRemovedPart.ID}:      "a source whose only link is into a removed part of the issue's ask",
		{"comment", removedSource.ID}:         "a comment whose only link into the issue's content a removed block holds",
	}

	read := func(what string) (*httptest.ResponseRecorder, model.IssueReferences) {
		t.Helper()
		response := issueReferencesRequest(t, handler, issue.Key, "")
		if response.Code != http.StatusOK {
			t.Fatalf("references %s: status=%d body=%s", what, response.Code, response.Body.String())
		}
		return response, decodeBody[model.IssueReferences](t, response)
	}
	first, answer := read("of the fixture")
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(first.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode the answer's fields: %v", err)
	}
	for _, name := range []string{"members", "truncated", "links", "linked_from"} {
		if value, found := fields[name]; !found {
			t.Errorf("the answer has no %s: %s", name, first.Body.String())
		} else if (name == "links" || name == "linked_from") && !bytes.HasPrefix(value, []byte("[")) {
			t.Errorf("the answer's %s is %s, want a list", name, value)
		}
	}
	for _, field := range []struct {
		name       string
		rows, want []model.ReferenceLink
	}{{"links", answer.Links, wantLinks}, {"linked_from", answer.LinkedFrom, wantLinkedFrom}} {
		listed := map[[2]string]bool{}
		for _, row := range field.rows {
			node := [2]string{row.Node.Kind, row.Node.ID}
			if listed[node] {
				t.Errorf("%s lists %s %s (%s) more than once", field.name, node[0], node[1], fixtureLabel(labels, node[1]))
			}
			listed[node] = true
			if why, never := listedNever[node]; never {
				t.Errorf("%s lists %s %s, %s", field.name, node[0], node[1], why)
			}
			if row.Node.Kind == "block" {
				t.Errorf("%s lists the block %s in place of its document or item", field.name, row.Node.ID)
			}
		}
		if got, want := referenceLinkLines(field.rows, labels), referenceLinkLines(field.want, labels); !slices.Equal(got, want) {
			t.Errorf("%s holds\n\t%s\nwant one row for each node at the other end of a link\n\t%s", field.name, shownLines(got), shownLines(want))
		}
	}
	if rows := slices.Concat(answer.Links, answer.LinkedFrom); len(rows) > 0 {
		nodes := make([]string, len(rows))
		for index, row := range rows {
			nodes[index] = row.Node.Ref
		}
		targets, _ := referenceTargets(t, handler, nodes)
		for index, row := range rows {
			if row.Name == "" || row.Name != targets[index].Name {
				t.Errorf("the row of %s %s (%s) is named %q, want the name the graph read gives it, %q",
					row.Node.Kind, row.Node.ID, fixtureLabel(labels, row.Node.ID), row.Name, targets[index].Name)
			}
		}
	}
	if got, want := referenceMemberLines(answer.Members, labels), referenceMemberLines(wantMembers, labels); !slices.Equal(got, want) {
		t.Errorf("members hold\n\t%s\nwant the documents of links, each at depth 1 with its via\n\t%s", shownLines(got), shownLines(want))
	}
	if answer.Truncated {
		t.Error("the answer is truncated; a read of one hop is never cut")
	}

	// Another document links to the issue's spec later: linked_from gains it and the ETag moves,
	// though the issue's own last_seq does not.
	lastSeq := issueLastSeq(t, database, issue.Key)
	later := upload(citing.Key, "links-later.md", "It links "+specRef+" later.\n")
	labels[later.ID] = "a document that links to the spec later"
	linkedLater, afterLink := read("after a later link into the issue's spec")
	if linkedLater.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Errorf("the ETag stayed %s after another document linked to the issue's spec", first.Header().Get("ETag"))
	}
	if seq := issueLastSeq(t, database, issue.Key); seq != lastSeq {
		t.Errorf("the later link from another issue's document moved the issue's last_seq from %d to %d", lastSeq, seq)
	}
	if lines, line := referenceLinkLines(afterLink.LinkedFrom, labels), referenceLinkLine(linkRow("artifact", later.ID, mentions, "artifact", specRefKey), labels); !slices.Contains(lines, line) {
		t.Errorf("linked_from after the later link holds\n\t%s\nwant among them\n\t%s", shownLines(lines), line)
	}

	// The comment the issue's checks link to is edited: the ETag moves, and neither the issue's
	// last_seq nor the kinds and via of any row do.
	succeeded(t, "edit the comment the issue's checks rest on", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+rested.ID, map[string]any{
		"body": "The comment the issue's checks rest on, edited.",
	}, "alice"))
	edited, afterEdit := read("after an edit of the comment the issue's checks link to")
	if edited.Header().Get("ETag") == linkedLater.Header().Get("ETag") {
		t.Errorf("the ETag stayed %s after the comment the issue's checks link to was edited", linkedLater.Header().Get("ETag"))
	}
	if seq := issueLastSeq(t, database, issue.Key); seq != lastSeq {
		t.Errorf("the edit of another issue's comment moved the issue's last_seq from %d to %d", lastSeq, seq)
	}
	for _, field := range []struct {
		name          string
		before, after []model.ReferenceLink
	}{{"links", afterLink.Links, afterEdit.Links}, {"linked_from", afterLink.LinkedFrom, afterEdit.LinkedFrom}} {
		if before, after := referenceLinkLines(field.before, labels), referenceLinkLines(field.after, labels); !slices.Equal(before, after) {
			t.Errorf("%s after the comment's edit holds\n\t%s\nwant the rows, kinds and vias it held before\n\t%s", field.name, shownLines(after), shownLines(before))
		}
	}
}

func graphRequest(t *testing.T, handler http.Handler, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?"+query.Encode(), nil, "alice")
}

func graphEdges(t *testing.T, handler http.Handler, query url.Values) model.GraphReferences {
	t.Helper()
	response := graphRequest(t, handler, query)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/references?%s: status=%d body=%s", query.Encode(), response.Code, response.Body.String())
	}
	return decodeBody[model.GraphReferences](t, response)
}

// A message on an ACME issue that cites a LEGION ask is a backlink on that ask: the edge names
// the message's block holding the link (R7), under the message's own issue, project, and
// dispatch:// address, carries the block's text as excerpt, and is stamped with the events.id of
// the message.created write. Read from the message, the same edge points out at the ask. Filters
// narrow it and malformed input is 400.
func TestReferencesReadBacklinksAcrossProjectsWithProvenance(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "LEGION")
	createReferenceAPIProject(t, handler, "ACME")
	legion := createReferenceAPIIssue(t, handler, "LEGION")
	acme := createReferenceAPIIssue(t, handler, "ACME")
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+legion.Key+"/asks", map[string]any{
		"question": "Ship the reference graph?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}},
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[model.Ask](t, askResponse)
	askRef := "dispatch://" + legion.Key + "/ask/" + ask.ID
	messageResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+acme.Key+"/messages", map[string]any{
		"body": "Decided in " + askRef + ", see also https://example.com/notes", "actor": sessionActor(),
	})
	if messageResponse.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", messageResponse.Code, messageResponse.Body.String())
	}
	message := decodeBody[model.Message](t, messageResponse)
	var messageEventID int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events where issue_key = $1 and type = 'message.created'
	`, acme.Key).Scan(&messageEventID); err != nil {
		t.Fatalf("read message.created event: %v", err)
	}

	incoming := graphEdges(t, handler, url.Values{"to": {askRef}})
	legionKey := legion.Key
	if want := (model.GraphNode{
		Kind: "ask", ID: ask.ID, IssueKey: &legionKey, Project: "LEGION", Ref: askRef,
		MadeBy: []model.Actor{ask.Author}, MadeAt: &ask.CreatedAt, ChangedBy: []model.Actor{ask.Author}, ChangedAt: &ask.CreatedAt,
		Parent: &model.NodeParent{Kind: "issue", ID: legion.Key, Ref: "dispatch://" + legion.Key},
	}); !reflect.DeepEqual(incoming.Node, want) {
		t.Fatalf("queried node = %#v; want %#v", incoming.Node, want)
	}
	if len(incoming.Edges) != 1 {
		t.Fatalf("incoming edges = %#v; want the ACME message only", incoming.Edges)
	}
	edge := incoming.Edges[0]
	acmeKey := acme.Key
	messageRef := "dispatch://" + acme.Key + "/message/" + message.ID
	// An edge's block node carries no text, only the node a reference read names does; the edge's
	// excerpt holds the block's text.
	wantNode := model.GraphNode{
		Kind: "block", ID: "message/" + message.ID + "#b1", BlockID: "b1", IssueKey: &acmeKey, Project: "ACME", Ref: messageRef + "#b-b1",
		MadeBy: []model.Actor{message.Author}, MadeAt: &message.CreatedAt, ChangedBy: []model.Actor{message.Author}, ChangedAt: &message.CreatedAt,
		Parent: &model.NodeParent{Kind: "message", ID: message.ID, Ref: messageRef},
	}
	if edge.Kind != "mentions" || edge.Direction != "in" || !reflect.DeepEqual(edge.Node, wantNode) {
		t.Fatalf("backlink edge = %#v; want mentions in from %#v", edge, wantNode)
	}
	if edge.Excerpt == nil || edge.Excerpt.BlockID != "b1" || edge.Excerpt.Text != message.Body {
		t.Fatalf("backlink excerpt = %#v; want the message's one block", edge.Excerpt)
	}
	if edge.SourceSeq == nil || *edge.SourceSeq != messageEventID || !edge.CreatedAt.Equal(message.CreatedAt) {
		t.Fatalf("backlink provenance = seq %v at %v; want event %d at %v", edge.SourceSeq, edge.CreatedAt, messageEventID, message.CreatedAt)
	}

	outgoing := graphEdges(t, handler, url.Values{"from": {"dispatch://" + acme.Key + "/message/" + message.ID}})
	if len(outgoing.Edges) != 1 || outgoing.Edges[0].Direction != "out" || outgoing.Edges[0].Kind != "mentions" || outgoing.Edges[0].Node.Ref != askRef || outgoing.Edges[0].Node.Kind != "ask" {
		t.Fatalf("outgoing edges = %#v; want one mention of the ask", outgoing.Edges)
	}
	if outgoing.Edges[0].Excerpt == nil || outgoing.Edges[0].Excerpt.Text != ask.Question {
		t.Fatalf("outgoing excerpt = %#v; want the ask question", outgoing.Edges[0].Excerpt)
	}

	if filtered := graphEdges(t, handler, url.Values{"to": {askRef}, "kind": {"replies_to,followed_by"}}); len(filtered.Edges) != 0 {
		t.Fatalf("kind filter kept %#v; want no edges", filtered.Edges)
	}
	// A newer build may name an edge kind this build does not emit. Its edges are none here, and
	// the answer names the kinds it did not know, rather than refusing the read.
	type knownEdges struct {
		Edges        []model.GraphEdge `json:"edges"`
		UnknownKinds []string          `json:"unknown_kinds"`
	}
	for kinds, wantEdges := range map[string]int{"future": 0, "mentions,future": 1} {
		query := url.Values{"to": {askRef}, "kind": {kinds}}
		response := graphRequest(t, handler, query)
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/references?%s: status=%d body=%s; want 200", query.Encode(), response.Code, response.Body.String())
		}
		if got := decodeBody[knownEdges](t, response); len(got.Edges) != wantEdges || !reflect.DeepEqual(got.UnknownKinds, []string{"future"}) {
			t.Fatalf("kind=%s answered %#v; want %d edges and unknown_kinds [future]", kinds, got, wantEdges)
		}
	}
	if response := graphRequest(t, handler, url.Values{"to": {askRef}}); strings.Contains(response.Body.String(), "unknown_kinds") {
		t.Fatalf("a read naming no kind answered %s; want no unknown_kinds", response.Body.String())
	}
	if since := graphEdges(t, handler, url.Values{"to": {askRef}, "since": {strconv.FormatInt(messageEventID, 10)}}); len(since.Edges) != 0 {
		t.Fatalf("since the introducing event kept %#v; want no edges", since.Edges)
	}
	// The read waits for an observation taken after it to be proven safe, so it answers the
	// message committed before it. A read that answered inside referencesSinceWait had that
	// proof, so its answer is final and a wrong one fails the test at once; awaitSinceRead reads
	// again only a read held past that wait, which an event write of the server's database held
	// open that long can cause, as a loaded machine can keep one.
	since := awaitSinceRead(t, database, "since the event before the message, ?to= "+askRef+", wanting the message's backlink",
		func() model.GraphReferences {
			return graphEdges(t, handler, url.Values{"to": {askRef}, "since": {strconv.FormatInt(messageEventID-1, 10)}})
		},
		func(read model.GraphReferences) bool { return len(read.Edges) == 1 })
	if since.Edges[0].SourceSeq == nil || *since.Edges[0].SourceSeq != messageEventID {
		t.Fatalf("since the prior event = %#v; want the backlink", since.Edges)
	}

	for name, tc := range map[string]struct {
		query  url.Values
		status int
		code   string
	}{
		"neither to nor from": {url.Values{}, http.StatusBadRequest, "INVALID_REFERENCE"},
		"both to and from":    {url.Values{"to": {askRef}, "from": {askRef}}, http.StatusBadRequest, "INVALID_REFERENCE"},
		"not a reference":     {url.Values{"to": {"LEGION-1 spec"}}, http.StatusBadRequest, "INVALID_REFERENCE"},
		"since not an id":     {url.Values{"to": {askRef}, "since": {"yesterday"}}, http.StatusBadRequest, "INVALID_SINCE"},
		"missing node":        {url.Values{"to": {"dispatch://" + legion.Key + "/ask/00000000-0000-0000-0000-000000000000"}}, http.StatusNotFound, "NOT_FOUND"},
		"missing document":    {url.Values{"from": {"dispatch://LEGION/artifact/ghost"}}, http.StatusNotFound, "NOT_FOUND"},
	} {
		response := graphRequest(t, handler, tc.query)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"code":"`+tc.code+`"`) {
			t.Errorf("%s: status=%d body=%s; want %d %s", name, response.Code, response.Body.String(), tc.status, tc.code)
		}
	}
}

// awaitSinceRead reads again until holds accepts the answer, because a since read answers only
// the edges at or below the event log's safe horizon (awaitHorizonRead). The what argument names
// the read in a failure, and database is the one the server reads. It returns the answer holds
// accepted.
func awaitSinceRead(
	t *testing.T,
	database *store.Store,
	what string,
	read func() model.GraphReferences,
	holds func(model.GraphReferences) bool,
) model.GraphReferences {
	t.Helper()
	return awaitHorizonRead(t, database, what, read, holds, func(answer model.GraphReferences) string {
		return fmt.Sprintf("%#v", answer.Edges)
	})
}

// awaitHorizonRead reads again until holds accepts the answer, for a route that answers only what
// stands at or below the event log's safe horizon (sinceHorizon): an event write of the server's
// database holds that horizon back for as long as it stays open - the test's own fixtures, the
// server's own request writes - and a transaction in another database, or one that writes no
// event, holds nothing (events.Horizon).
// Such a read waits out referencesSinceWait and then answers at the horizon it was left with,
// which may be short of what committed before it, so it is read again for as long as the horizon
// says it is held back (horizontest.Watch), however long that is; database is the one the server
// reads. A read that answered within referencesSinceWait was not held back: what it answered is
// what it owed, so holds failing on it is a product failure and fails the test at once. what names
// the read and show its answer in either failure.
func awaitHorizonRead[T any](t *testing.T, database *store.Store, what string, read func() T, holds func(T) bool, show func(T) string) T {
	t.Helper()
	warnings, stop := horizontest.Listen(t)
	defer stop()
	watch := horizontest.NewWatch(t, warnings, database.Pool.Config().ConnConfig.Database, database.Pool)
	for {
		started := time.Now()
		answer := read()
		took := time.Since(started)
		if holds(answer) {
			return answer
		}
		if took < referencesSinceWait {
			t.Fatalf("%s answered %s in %s, inside %s: the horizon held nothing back, so that is all it owes",
				what, show(answer), took, referencesSinceWait)
		}
		if failure := watch.Failure("the whole answer of " + what); failure != "" {
			t.Fatalf("%s; the last read, taking %s, answered %s", failure, took, show(answer))
		}
	}
}

// A since read answers every mention committed before it, also on a server where nothing else
// observes the event log: no event stream has started the relay and no Delivery page the hub.
// Each observation is proven safe only by a later one, so a read that stopped at the horizon
// proven so far would answer nothing here, the first after boot, and each later read would stop
// at the previous read's observation, however old. awaitSinceRead settles which answer is final.
func TestReferencesSinceAnswersAMentionCommittedBeforeTheReadOnAQuietServer(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	target := createReferenceAPIIssue(t, handler, "CORE")
	citingResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"project": "CORE", "title": "Rotate the database credentials"}, "alice")
	if citingResponse.Code != http.StatusCreated {
		t.Fatalf("create the citing issue: status=%d body=%s", citingResponse.Code, citingResponse.Body.String())
	}
	citing := decodeBody[model.Issue](t, citingResponse)
	targetRef := "dispatch://" + target.Key
	comment := createReferenceAPIComment(t, handler, citing.Key, "Blocked on "+targetRef)

	query := url.Values{"to": {targetRef}, "since": {"0"}}
	awaitSinceRead(t, database, "since 0 ?to= "+targetRef+", wanting the mention by comment "+comment.ID,
		func() model.GraphReferences { return graphEdges(t, handler, query) },
		func(read model.GraphReferences) bool {
			return len(read.Edges) == 1 && read.Edges[0].Kind == "mentions" &&
				blockOf(read.Edges[0].Node, "comment", comment.ID)
		})
}

// A poller reads ?since=<the highest source_seq it was shown>. Event ids are not commit order:
// a comment edit that drew event a can still be open after one that drew b > a has committed. A
// read that answered b's mention would move the poller's since to b, and a's mention, committing
// later below it, would never be shown. The since read stops at the safe horizon, which a's open
// transaction holds below a, so it answers neither mention while a is open: each read waits out
// referencesSinceWait for the horizon, then answers the mentions at or below it, which are none.
// Once a commits it answers both. Each edit indexes its comment's body as a comment write does
// (planFoundReferences and the replacement it plans, then refs.Stamp with the edit's event).
func TestReferencesSinceWithholdsEveryMentionAboveAnOpenLowerEvent(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	broker := deps.Events
	createReferenceAPIProject(t, handler, "CORE")
	issue := func(title string) model.Issue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"project": "CORE", "title": title}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	target := issue("Ship the widget launch")
	firstIssue := issue("Rotate the database credentials")
	secondIssue := issue("Repaint the office lobby")
	first := createReferenceAPIComment(t, handler, firstIssue.Key, "No reference yet.")
	second := createReferenceAPIComment(t, handler, secondIssue.Key, "No reference yet.")
	targetRef := "dispatch://" + target.Key

	ctx := t.Context()
	mention := func(tx pgx.Tx, issueKey string, comment model.Comment) int64 {
		t.Helper()
		commentID := comment.ID
		found := text.Refs(refs.NewReader(ctx, tx, comment.Author, "comment", commentID, "").Mentions("Now see " + targetRef))
		replacement, _, err := planFoundReferences(ctx, tx, "comment", commentID, found, nil)
		if err != nil {
			t.Fatalf("plan comment %s's references: %v", commentID, err)
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			replacement.Queue(b)
			return nil
		}); err != nil {
			t.Fatalf("index comment %s: %v", commentID, err)
		}
		event, err := broker.Append(ctx, tx, model.Event{
			IssueKey: new(issueKey), Type: "comment.updated", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: map[string]any{"comment_id": commentID},
		})
		if err != nil {
			t.Fatalf("append comment %s's event: %v", commentID, err)
		}
		if err := refs.Stamp(ctx, tx, "comment", commentID, event.ID); err != nil {
			t.Fatalf("stamp comment %s's mention: %v", commentID, err)
		}
		return event.ID
	}
	open, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lower event's transaction: %v", err)
	}
	t.Cleanup(func() { _ = open.Rollback(context.Background()) })
	low := mention(open, firstIssue.Key, first)
	committed, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the higher event's transaction: %v", err)
	}
	defer func() { _ = committed.Rollback(context.Background()) }()
	high := mention(committed, secondIssue.Key, second)
	if err := committed.Commit(ctx); err != nil {
		t.Fatalf("commit the higher event: %v", err)
	}
	if high <= low {
		t.Fatalf("event ids %d (open) and %d (committed) are not out of commit order, so this test proves nothing", low, high)
	}

	query := url.Values{"to": {targetRef}, "since": {strconv.FormatInt(low-1, 10)}}
	// Two reads, each held for referencesSinceWait by a's transaction: the second is the poller's
	// next read, after an observation the first one took.
	for range 2 {
		if read := graphEdges(t, handler, query); len(read.Edges) != 0 {
			t.Fatalf("since %d while event %d is open = %#v; want no mention, not even the committed event %d's", low-1, low, read.Edges, high)
		}
	}

	if err := open.Commit(ctx); err != nil {
		t.Fatalf("commit the lower event: %v", err)
	}
	read := awaitSinceRead(t, database, "since "+strconv.FormatInt(low-1, 10)+" ?to= "+targetRef+", wanting both mentions once the lower event has committed",
		func() model.GraphReferences { return graphEdges(t, handler, query) },
		func(read model.GraphReferences) bool { return len(read.Edges) == 2 })
	var seqs []int64
	for _, edge := range read.Edges {
		if edge.Kind != "mentions" || edge.SourceSeq == nil {
			t.Fatalf("edge %#v; want a mention with its source_seq", edge)
		}
		seqs = append(seqs, *edge.SourceSeq)
	}
	slices.Sort(seqs)
	if !slices.Equal(seqs, []int64{low, high}) {
		t.Fatalf("since %d after both commits answers mentions of events %v; want %d and %d", low-1, seqs, low, high)
	}
}

// An issue's backlinks are its mentions and its structure in one list: the child issue
// (child_of), its own documents (attached_to), and a document elsewhere that cites it, listed
// from the block holding the mention, whose excerpt is that block. Read from the citing document
// the same mention points out at the issue; read from the issue's spec the structural edge points
// out at it.
func TestReferencesIncludeStructuralEdgesAndDocumentBlockExcerpts(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	parent := createReferenceAPIIssue(t, handler, "CORE")
	childResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Child work", "parent": parent.Key,
	}, "alice")
	if childResponse.Code != http.StatusCreated {
		t.Fatalf("create child issue: status=%d body=%s", childResponse.Code, childResponse.Body.String())
	}
	child := decodeBody[model.Issue](t, childResponse)
	design := createProjectDocument(t, handler, "OPS", "Design notes", "# Design\n\nIntroduction paragraph.\n\nThe plan lives in dispatch://"+parent.Key+" and nowhere else.\n")

	incoming := graphEdges(t, handler, url.Values{"to": {"dispatch://" + parent.Key}})
	byKind := make(map[string]model.GraphEdge, len(incoming.Edges))
	for _, edge := range incoming.Edges {
		byKind[edge.Kind] = edge
	}
	if len(incoming.Edges) != 3 || len(byKind) != 3 {
		t.Fatalf("issue edges = %#v; want one mention, one child_of, one attached_to", incoming.Edges)
	}
	for index := 1; index < len(incoming.Edges); index++ {
		if incoming.Edges[index].CreatedAt.After(incoming.Edges[index-1].CreatedAt) {
			t.Fatalf("edges are not newest first: %#v", incoming.Edges)
		}
	}
	mention := byKind["mentions"]
	if mention.Node.Kind != "block" || mention.Node.Document != "dispatch://OPS/artifact/design-notes" || mention.Node.Project != "OPS" {
		t.Fatalf("mention node = %#v; want a block of the OPS design document", mention.Node)
	}
	if mention.Excerpt == nil || mention.Excerpt.BlockID == "" || mention.Excerpt.Text != "The plan lives in dispatch://"+parent.Key+" and nowhere else." {
		t.Fatalf("mention excerpt = %#v; want the containing paragraph block with its id", mention.Excerpt)
	}
	if mention.SourceSeq == nil {
		t.Fatalf("document mention has no source_seq: %#v", mention)
	}
	childOf := byKind["child_of"]
	if childOf.Node.Kind != "issue" || childOf.Node.ID != child.Key || childOf.Node.Ref != "dispatch://"+child.Key || childOf.Excerpt == nil || childOf.Excerpt.Text != "Child work" || childOf.SourceSeq != nil {
		t.Fatalf("child_of edge = %#v; want the child issue with its title and no source_seq", childOf)
	}
	attached := byKind["attached_to"]
	if attached.Node.Kind != "artifact" || attached.Node.ID != parent.PrimaryArtifactID || attached.Node.Ref != "dispatch://"+parent.Key+"/spec" || attached.Excerpt == nil || attached.Excerpt.Text != "spec.md" {
		t.Fatalf("attached_to edge = %#v; want the issue's spec", attached)
	}

	outgoing := graphEdges(t, handler, url.Values{"from": {"dispatch://OPS/artifact/design-notes"}})
	if outgoing.Node.Kind != "artifact" || outgoing.Node.ID != design.ID {
		t.Fatalf("document node = %#v; want %s resolved from its ref_key", outgoing.Node, design.ID)
	}
	if len(outgoing.Edges) != 1 || outgoing.Edges[0].Kind != "mentions" || outgoing.Edges[0].Node.Ref != "dispatch://"+parent.Key || outgoing.Edges[0].Excerpt == nil || outgoing.Edges[0].Excerpt.Text != "Reference issue" {
		t.Fatalf("document links = %#v; want one mention of the parent issue with its title", outgoing.Edges)
	}

	spec := graphEdges(t, handler, url.Values{"from": {"dispatch://" + parent.Key + "/spec"}})
	if len(spec.Edges) != 1 || spec.Edges[0].Kind != "attached_to" || spec.Edges[0].Direction != "out" || spec.Edges[0].Node.Ref != "dispatch://"+parent.Key {
		t.Fatalf("spec links = %#v; want attached_to the issue", spec.Edges)
	}
	specBacklinks := graphEdges(t, handler, url.Values{"to": {"dispatch://" + parent.Key + "/spec"}})
	if specBacklinks.Node.ID != parent.PrimaryArtifactID || len(specBacklinks.Edges) != 0 {
		t.Fatalf("spec backlinks = %#v; want the spec node with no edges", specBacklinks)
	}
}

// insertUnknownKindEdges plants refs rows whose other end is a node kind this build does not
// know, as a newer build sharing the database would write them.
func insertUnknownKindEdges(t *testing.T, database *store.Store, edges [][4]string) {
	t.Helper()
	for _, edge := range edges {
		if _, err := database.Pool.Exec(context.Background(), `
			insert into refs (from_kind, from_id, to_kind, to_id) values ($1, $2, $3, $4)
		`, edge[0], edge[1], edge[2], edge[3]); err != nil {
			t.Fatalf("insert reference %v: %v", edge, err)
		}
	}
}

// An edge whose other end is a node kind this build cannot address is one a newer build wrote:
// the graph read skips it in both directions instead of answering 500, and the issue header's
// count is the number of edges its panel lists, not one more.
func TestReferencesAnswerAroundAnEdgeOfAKindThisBuildCannotAddress(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	source := createReferenceAPIIssue(t, handler, "OPS")
	comment := createReferenceAPIComment(t, handler, source.Key, "see dispatch://"+target.Key)
	commentRef := "dispatch://" + source.Key + "/comment/" + comment.ID
	insertUnknownKindEdges(t, database, [][4]string{
		{"future", "x#y", "issue", target.Key},
		{"comment", comment.ID, "future", "x#y"},
	})

	// The target's own spec is attached to it and the comment cites it, from its block: two edges
	// this build can address, and the header counts the same edges the panel lists.
	incoming := graphRequest(t, handler, url.Values{"to": {"dispatch://" + target.Key}})
	if incoming.Code != http.StatusOK {
		t.Errorf("GET references to %s: status=%d body=%s", target.Key, incoming.Code, incoming.Body.String())
	} else {
		edges := decodeBody[model.GraphReferences](t, incoming).Edges
		byKind := make(map[string]model.GraphEdge, len(edges))
		for _, edge := range edges {
			byKind[edge.Kind] = edge
		}
		mention, attached := byKind["mentions"], byKind["attached_to"]
		if len(edges) != 2 || !blockOf(mention.Node, "comment", comment.ID) || attached.Node.Kind != "artifact" || attached.Node.ID != target.PrimaryArtifactID {
			t.Errorf("backlinks of %s = %#v; want the comment's mention, from its block, and the attached spec, nothing of kind future", target.Key, edges)
		}
	}

	outgoing := graphRequest(t, handler, url.Values{"from": {commentRef}})
	if outgoing.Code != http.StatusOK {
		t.Errorf("GET references from %s: status=%d body=%s", commentRef, outgoing.Code, outgoing.Body.String())
	} else {
		edges := decodeBody[model.GraphReferences](t, outgoing).Edges
		if len(edges) != 1 || edges[0].Kind != "mentions" || edges[0].Node.Kind != "issue" || edges[0].Node.ID != target.Key {
			t.Errorf("links of %s = %#v; want only the mention of %s, nothing of kind future", commentRef, edges, target.Key)
		}
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+target.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	if count := decodeBody[struct {
		ReferencedByCount int `json:"referenced_by_count"`
	}](t, detail).ReferencedByCount; count != 2 {
		t.Errorf("issue %q referenced_by_count = %d; want 2, the edges its panel lists", target.Key, count)
	}
}

// A document's references read skips an edge of a node kind this build cannot address on both
// sides: its referenced-by list is the comment that cites it, and its outgoing list is what its
// text cites, the known issue and the missing document both unresolved as before.
func TestArtifactReferencesSkipANodeKindThisBuildCannotAddress(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\n\nThe plan lives in dispatch://"+issue.Key+" and dispatch://CORE/artifact/missing.\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")
	insertUnknownKindEdges(t, database, [][4]string{
		{"artifact", design.ID, "future", "x#y"},
		{"future", "x#y", "artifact", design.RefKey},
	})

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+design.ID+"/references", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("get artifact references: status=%d body=%s", response.Code, response.Body.String())
	}
	value := decodeBody[model.ArtifactReferences](t, response)
	if want := []model.OutgoingReference{
		{Kind: "artifact", ToID: "CORE/missing"},
		{Kind: "issue", ToID: issue.Key},
	}; !reflect.DeepEqual(value.Outgoing, want) {
		t.Errorf("outgoing references = %#v; want the unresolved issue and missing document, nothing of kind future", value.Outgoing)
	}
	if len(value.ReferencedBy) != 1 || value.ReferencedBy[0].Kind != "comment" || value.ReferencedBy[0].ID != comment.ID {
		t.Errorf("referenced by = %#v; want only comment %q", value.ReferencedBy, comment.ID)
	}
}

// A spec seeded at issue creation that cites an issue in bold, another project's issue in a code
// span and a third project's issue as an autolink, which the spec stores as a link whose text is
// its target, mentions all three. The source of each mention is the spec document, not the issue
// that owns it: the issue node writes no mention, and each target's backlink names the spec's
// block holding it.
func TestSeededSpecCitationsInMarkdownDelimitersAreDocumentMentions(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "LEGION")
	createReferenceAPIProject(t, handler, "ACME")
	createReferenceAPIProject(t, handler, "CORE")
	sameProject := createReferenceAPIIssue(t, handler, "LEGION")
	otherProject := createReferenceAPIIssue(t, handler, "ACME")
	autolinked := createReferenceAPIIssue(t, handler, "CORE")
	response := createIssueRequest(t, handler, map[string]any{
		"project": "LEGION", "title": "Due dates",
		"spec": "Tied to **dispatch://" + sameProject.Key + "** and `dispatch://" + otherProject.Key + "`, " +
			"see <dispatch://" + autolinked.Key + ">.\n",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue with spec: status=%d body=%s", response.Code, response.Body.String())
	}
	citing := decodeBody[model.Issue](t, response)
	specRef := "dispatch://" + citing.Key + "/spec"
	targets := []model.Issue{sameProject, otherProject, autolinked}

	mentioned := map[string]bool{}
	for _, edge := range graphEdges(t, handler, url.Values{"from": {specRef}, "kind": {"mentions"}}).Edges {
		mentioned[edge.Node.Ref] = true
	}
	want := map[string]bool{}
	for _, target := range targets {
		want["dispatch://"+target.Key] = true
	}
	if !reflect.DeepEqual(mentioned, want) {
		t.Fatalf("spec mentions = %v; want %v", mentioned, want)
	}
	if fromIssue := graphEdges(t, handler, url.Values{"from": {"dispatch://" + citing.Key}, "kind": {"mentions"}}); len(fromIssue.Edges) != 0 {
		t.Fatalf("issue node mentions = %#v; want none, the spec document writes them", fromIssue.Edges)
	}
	for _, target := range targets {
		backlinks := graphEdges(t, handler, url.Values{"to": {"dispatch://" + target.Key}, "kind": {"mentions"}}).Edges
		if len(backlinks) != 1 || backlinks[0].Node.Kind != "block" || backlinks[0].Node.Document != specRef {
			t.Fatalf("%s backlinks = %#v; want one mention from a block of %s", target.Key, backlinks, specRef)
		}
	}
}

// An item id that decodes to a NUL names nothing. The index binds ids as `text[]`, and Postgres
// refuses a NUL there, so a reader that decoded one failed the write holding it: the comment, the
// seeded spec and the author's edit answered 500, and a rebuild stopped at the first stored body
// that held one.
func TestCitationsOfAnIDDecodingToNULDoNotFailTheWrite(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	target := createReferenceAPIIssue(t, handler, "CORE")
	body := "See dispatch://" + target.Key + "/comment/%00, https://dispatch.example/issues/" + target.Key +
		"/comments/%00 and https://dispatch.example/issues/" + target.Key + "/spec?comment=%2500.\n"

	comment := createReferenceAPIComment(t, handler, target.Key, body)
	commentRef := "dispatch://" + target.Key + "/comment/" + comment.ID
	if edges := graphEdges(t, handler, url.Values{"from": {commentRef}, "kind": {"mentions"}}).Edges; len(edges) != 0 {
		t.Fatalf("comment mentions = %#v; want none", edges)
	}
	if seeded := createIssueRequest(t, handler, map[string]any{"project": "CORE", "title": "Seeded", "spec": body}); seeded.Code != http.StatusCreated {
		t.Fatalf("create issue whose spec cites a NUL id: status=%d body=%s", seeded.Code, seeded.Body.String())
	}
	plain := createReferenceAPIComment(t, handler, target.Key, "Plain comment.")
	if edit := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+plain.ID, map[string]string{
		"body": body,
	}, "alice"); edit.Code != http.StatusOK {
		t.Fatalf("edit a comment to cite a NUL id: status=%d body=%s", edit.Code, edit.Body.String())
	}
	if _, err := refs.RebuildAll(context.Background(), database.Pool, "https://dispatch.example"); err != nil {
		t.Fatalf("rebuild refs over bodies citing a NUL id: %v", err)
	}
}

type refRow struct {
	FromKind, FromID, ToKind, ToID, Kind string
	SourceSeq                            *int64
	CreatedAt                            string
}

func readRefRows(t *testing.T, database *store.Store) []refRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select from_kind, from_id, to_kind, to_id, kind, source_seq, created_at::text
		from refs order by from_kind, from_id, to_kind, to_id
	`)
	if err != nil {
		t.Fatalf("read refs: %v", err)
	}
	defer rows.Close()
	var result []refRow
	for rows.Next() {
		var row refRow
		if err := rows.Scan(&row.FromKind, &row.FromID, &row.ToKind, &row.ToID, &row.Kind, &row.SourceSeq, &row.CreatedAt); err != nil {
			t.Fatalf("scan ref: %v", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate refs: %v", err)
	}
	return result
}

// The text is the truth and refs is its index: after the index is doctored (an edge deleted, a
// stale edge planted), a rebuild converges on exactly the edge set the live writes produced,
// keeping the provenance of every edge it did not have to recreate, and a second rebuild is a
// no-op.
func TestRebuildRefsConvergesOnTheLiveIndex(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	createProjectDocument(t, handler, "OPS", "Diagram", "# Diagram\n")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design\n\nSee dispatch://OPS/artifact/diagram and https://dispatch.example/issues/"+issue.Key+".\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes and dispatch://"+issue.Key+"/spec")
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Merge dispatch://CORE/artifact/design-notes?", "options": []map[string]string{{"label": "Yes"}},
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Read dispatch://" + issue.Key + "/comment/" + comment.ID, "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", response.Code, response.Body.String())
	}
	live := readRefRows(t, database)
	if len(live) != 6 {
		t.Fatalf("live index = %#v; want six edges", live)
	}
	for _, row := range live {
		if row.SourceSeq == nil {
			t.Fatalf("live edge %#v has no source_seq", row)
		}
	}

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`delete from refs where from_kind = 'comment' and to_kind = 'artifact' and to_id = $1`, []any{issue.Key + "/spec"}},
		{`insert into refs (from_kind, from_id, to_kind, to_id, source_seq) values ('artifact', $1, 'issue', 'ZZZ-9', 1)`, []any{design.ID}},
		{`insert into refs (from_kind, from_id, to_kind, to_id) values ('message', '00000000-0000-0000-0000-000000000000', 'issue', $1)`, []any{issue.Key}},
		// A message whose state row is gone, as one stored before the item-block state seeded it
		// would be: the rebuild inserts it owed.
		{`delete from message_block_state`, nil},
	} {
		if _, err := database.Pool.Exec(context.Background(), statement.sql, statement.args...); err != nil {
			t.Fatalf("doctor refs: %v", err)
		}
	}
	if doctored := readRefRows(t, database); len(doctored) != 7 {
		t.Fatalf("doctored index = %#v; want seven rows", doctored)
	}

	report, err := refs.RebuildAll(context.Background(), database.Pool, "https://dispatch.example")
	if err != nil {
		t.Fatalf("rebuild refs: %v", err)
	}
	// Every document has a version (three, counting the two specs); the planted row from a message
	// that does not exist is the one orphan. Each of the three is marked owed a block index, and
	// the ask, the comment and the message are each marked owed a block index of their own parts.
	if report != (refs.Rebuild{Documents: 3, Asks: 1, Comments: 1, Messages: 1, Orphans: 1, Edges: 6, BlocksOwed: 3, ItemsOwed: 3}) {
		t.Fatalf("rebuild report = %#v", report)
	}
	var documents, owed int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), count(*) filter (where b.artifact_id is not null and b.version is null)
		from artifacts a left join document_block_versions b on b.artifact_id = a.id
		where a.kind = 'doc'
	`).Scan(&documents, &owed); err != nil {
		t.Fatalf("read the block index of every document: %v", err)
	}
	if documents != 3 || owed != documents {
		t.Fatalf("after rebuild-refs %d of %d documents are owed a block index, want every one", owed, documents)
	}
	var items, owedItems int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from comments) + (select count(*) from messages),
		       (select count(*) from comments c join comment_block_state s on s.comment_id = c.id where s.owed_at is not null) +
		       (select count(*) from messages m join message_block_state s on s.message_id = m.id where s.owed_at is not null)
	`).Scan(&items, &owedItems); err != nil {
		t.Fatalf("read the block state of every comment and message: %v", err)
	}
	if items != 2 || owedItems != items {
		t.Fatalf("after rebuild-refs %d of %d comments and messages are owed a block index, want every one", owedItems, items)
	}
	rebuilt := readRefRows(t, database)
	if len(rebuilt) != len(live) {
		t.Fatalf("rebuilt index = %#v; want the live index %#v", rebuilt, live)
	}
	for index, row := range live {
		got := rebuilt[index]
		if got.FromKind != row.FromKind || got.FromID != row.FromID || got.ToKind != row.ToKind || got.ToID != row.ToID || got.Kind != row.Kind {
			t.Fatalf("rebuilt edge %d = %#v; want %#v", index, got, row)
		}
		recreated := row.FromKind == "comment" && row.ToID == issue.Key+"/spec"
		if recreated {
			if got.SourceSeq != nil || got.CreatedAt == row.CreatedAt {
				t.Fatalf("recreated edge %#v kept provenance %#v", got, row)
			}
			continue
		}
		if got.SourceSeq == nil || *got.SourceSeq != *row.SourceSeq || got.CreatedAt != row.CreatedAt {
			t.Fatalf("surviving edge %#v lost provenance %#v", got, row)
		}
	}
	if _, err := refs.RebuildAll(context.Background(), database.Pool, "https://dispatch.example"); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if again := readRefRows(t, database); !reflect.DeepEqual(again, rebuilt) {
		t.Fatalf("second rebuild changed the index: %#v -> %#v", rebuilt, again)
	}
}
