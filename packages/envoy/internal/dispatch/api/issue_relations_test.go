package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// readRelationsIssue reads key through the issue read.
func readRelationsIssue(t *testing.T, handler http.Handler, key string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// currentRelations is every relation of kind the node of issue key states now, by far end.
func currentRelations(t *testing.T, database *store.Store, key, kind string) []graphstore.Tuple {
	t.Helper()
	tuples, err := graphstore.CurrentOut(context.Background(), database.Pool, graphstore.KindIssue, key, []string{kind})
	if err != nil {
		t.Fatalf("read the %s of %s: %v", kind, key, err)
	}
	return tuples
}

// relationEnds is the kind:id of each tuple, in the order given.
func relationEnds(tuples []graphstore.Tuple) []string {
	ends := make([]string, len(tuples))
	for index, tuple := range tuples {
		ends[index] = tuple.ToKind + ":" + tuple.ToID
	}
	return ends
}

// requireRelationRefused fails unless response is status with code and every word of named in
// its text.
func requireRelationRefused(t *testing.T, what string, response *httptest.ResponseRecorder, status int, code string, named ...string) refusalAnswer {
	t.Helper()
	if response.Code != status {
		t.Fatalf("%s: status=%d body=%s, want %d %s", what, response.Code, response.Body.String(), status, code)
	}
	refusal := decodeBody[refusalAnswer](t, response)
	if refusal.Code != code {
		t.Fatalf("%s: code %q (%s), want %s", what, refusal.Code, refusal.Error, code)
	}
	for _, word := range named {
		if !strings.Contains(refusal.Error, word) {
			t.Errorf("%s: the refusal %q does not name %q", what, refusal.Error, word)
		}
	}
	return refusal
}

// issueLinkRow reads the issue_links row of the pair: whether it stands, and the node it names.
func issueLinkMarker(t *testing.T, database *store.Store, issue, target string) (bool, string) {
	t.Helper()
	var kind *string
	err := database.Pool.QueryRow(context.Background(), `
		select node_kind from issue_links where issue_key = $1 and target_key = $2 and kind = 'blocked_by'
	`, issue, target).Scan(&kind)
	if err != nil {
		return false, ""
	}
	if kind == nil {
		return true, ""
	}
	return true, *kind
}

// TestNeedsCrossesAProjectAndRefusesAProjectScopedCaller is c-one-dependency's Go half
// (R10 of the issue-tree spec). An issue's wait is the needs tuple of the issue's node, which the
// issue_links row of the pair projects, and the issue read answers it under needs alone (R27 of
// dispatch://INFRA-1098). Through the agent bearer, which names no project, a needs to an issue
// of another project is taken, its tuple appended and its row naming the issue's node. A list past
// contracts.MaxIssueBlockers is refused 400 NEEDS_INPUT, and a request naming blocked_by alone is
// refused 400 BLOCKED_BY_INPUT naming needs. A request naming both is served from needs alone: one
// with the same targets appends one needs tuple for each target, and one whose blocked_by names
// another issue appends no tuple for it. A wait that closes a ring through the waits is refused
// 409 DEPENDENCY_CYCLE. Last, a credential that names one project is refused a needs target
// outside it 403 PROJECT_SCOPE, naming needs, and the issue keeps no wait.
func TestNeedsCrossesAProjectAndRefusesAProjectScopedCaller(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	core := createListedIssues(t, handler, "CORE", 4)
	other := createListedIssues(t, handler, "CORE2", 1)

	taken := bearerRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core[0], map[string]any{"needs": []string{other[0]}, "actor": sessionActor()})
	if taken.Code != http.StatusOK {
		t.Fatalf("a needs to another project's issue through the agent bearer: status=%d body=%s", taken.Code, taken.Body.String())
	}
	issue := readRelationsIssue(t, handler, core[0])
	if !slices.Equal(issue.Needs, []string{other[0]}) {
		t.Errorf("%s reads needs %v, want [%s]", core[0], issue.Needs, other[0])
	}
	if got := relationEnds(currentRelations(t, database, core[0], graphstore.TupleNeeds)); !slices.Equal(got, []string{"issue:" + other[0]}) {
		t.Errorf("%s's node states needs %v, want [issue:%s]", core[0], got, other[0])
	}
	if standing, marker := issueLinkMarker(t, database, core[0], other[0]); !standing || marker != graphstore.KindIssue {
		t.Errorf("the issue_links row %s -> %s stands %t and names node kind %q, want it standing and naming the issue's node", core[0], other[0], standing, marker)
	}

	tooMany := make([]string, contracts.MaxIssueBlockers+1)
	for index := range tooMany {
		tooMany[index] = core[1]
	}
	requireRelationRefused(t, "needs past the cap", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core[0],
		map[string]any{"needs": tooMany}, "alice"), http.StatusBadRequest, codeNeedsInput, "needs")
	requireRelationRefused(t, "blocked_by alone", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core[0],
		map[string]any{"blocked_by": tooMany}, "alice"), http.StatusBadRequest, codeBlockedByInput, "blocked_by", "needs")
	requireRelationRefused(t, "a create's needs past the cap", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "CORE", "title": "Waits on too much", "needs": tooMany, "force": true}, "alice"),
		http.StatusBadRequest, codeNeedsInput, "needs")

	same := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core[0], map[string]any{
		"needs": []string{core[1], core[2]}, "blocked_by": []string{core[2], core[1]},
	}, "alice")
	if same.Code != http.StatusOK {
		t.Fatalf("needs and blocked_by with the same targets: status=%d body=%s", same.Code, same.Body.String())
	}
	if got := relationEnds(currentRelations(t, database, core[0], graphstore.TupleNeeds)); !slices.Equal(got, []string{"issue:" + core[1], "issue:" + core[2]}) {
		t.Errorf("%s's node states needs %v after the request naming both, want one tuple for each of %s and %s", core[0], got, core[1], core[2])
	}
	var asserted int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples where from_kind = 'issue' and from_id = $1 and kind = 'needs' and asserted and to_id = any($2)
	`, core[0], []string{core[1], core[2]}).Scan(&asserted); err != nil {
		t.Fatalf("count the needs tuples: %v", err)
	}
	if asserted != 2 {
		t.Errorf("the request naming both members appended %d needs tuples for its two targets, want one each", asserted)
	}

	beside := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core[0], map[string]any{
		"needs": []string{core[1], core[2]}, "blocked_by": []string{core[3]},
	}, "alice")
	if beside.Code != http.StatusOK {
		t.Fatalf("needs with a blocked_by naming another issue: status=%d body=%s", beside.Code, beside.Body.String())
	}
	if got := relationEnds(currentRelations(t, database, core[0], graphstore.TupleNeeds)); !slices.Equal(got, []string{"issue:" + core[1], "issue:" + core[2]}) {
		t.Errorf("the request served from needs left %s stating needs %v, want [issue:%s issue:%s] and none for %s", core[0], got, core[1], core[2], core[3])
	}

	requireRelationRefused(t, "a ring under needs", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core[1],
		map[string]any{"needs": []string{core[0]}}, "alice"), http.StatusConflict, "DEPENDENCY_CYCLE")
	requireRelationRefused(t, "a ring named by blocked_by alone", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core[2],
		map[string]any{"blocked_by": []string{core[0]}}, "alice"), http.StatusBadRequest, codeBlockedByInput, "needs")

	t.Run("a credential that names one project is refused a target outside it", func(t *testing.T) {
		rig := newBrokerTokenRig(t)
		caller := newBrokerCaller(t)
		const token = "a-token-waiting-across-projects"
		rig.broker.Says(token, liveNamedToken(caller.jkt))
		for _, key := range []string{brokerTestProject, "OTH"} {
			if response := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": key}); response.Code != http.StatusCreated {
				t.Fatalf("create project %s: status=%d body=%s", key, response.Code, response.Body.String())
			}
		}
		createIssue := func(project, title string) string {
			t.Helper()
			response := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{"project": project, "title": title, "force": true})
			if response.Code != http.StatusCreated {
				t.Fatalf("create %q in %s: status=%d body=%s", title, project, response.Code, response.Body.String())
			}
			return decodeBody[model.Issue](t, response).Key
		}
		own := createIssue(brokerTestProject, "Its own project's issue")
		outside := createIssue("OTH", "An issue of another project")
		refused := rig.serve(caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+own, token, map[string]any{"needs": []string{outside}}))
		requireRelationRefused(t, "a scoped caller's needs across projects", refused, http.StatusForbidden, codeProjectScope,
			brokerTestProject, "OTH", "needs issue "+outside)
		created := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
			"project": brokerTestProject, "title": "Created waiting outside", "needs": []string{outside}, "force": true,
		}))
		requireRelationRefused(t, "a scoped caller's create waiting across projects", created, http.StatusForbidden, codeProjectScope, "needs issue "+outside)
		read := rig.asPerson(t, http.MethodGet, "/api/v1/issues/"+own, nil)
		if got := decodeBody[model.Issue](t, read); len(got.Needs) != 0 {
			t.Errorf("the refused writes left needs %v, want none", got.Needs)
		}
	})
}

// TestAnIssueRelationAndItsProjectionCommitTogether holds the writers of child_of and needs to
// their projections (R9, R10 of the issue-tree spec): each tuple is appended in the transaction
// that writes the row projecting it, caused by the issue's own event. A create with a parent and a
// wait appends child_of and needs under issue.created, beside issues.parent_key and an issue_links
// row naming the issue's node, each tuple dated at its row's created_at. A PATCH that moves the
// wait retracts the tuple it drops and appends the one it adds under issue.updated, and deletes and
// writes the rows to match. A row a process of the release before wrote, naming no node, that a
// later set keeps is named on its node and its tuple dated at the row's own created_at. A write
// refused after its rows were read writes neither a row nor a tuple. A reparent moves child_of and
// retracts the issue's part_of to words that are on neither its new parent nor above it, and
// keeps those that are (R2). A PATCH stating a relation of an issue whose row the release before
// wrote, naming no node, stamps the issue's marker on the row and claims the parent the row names,
// though the parent does not move, dated at the row's created_at and caused by issue.updated. The
// census reads zero differences for both projections.
func TestAnIssueRelationAndItsProjectionCommitTogether(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	keys := createListedIssues(t, handler, "REL", 5)
	parent, first, second, old, unstamped := keys[0], keys[1], keys[2], keys[3], keys[4]
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "REL", "title": "A child that waits", "parent": parent, "needs": []string{first}, "force": true,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the child: status=%d body=%s", created.Code, created.Body.String())
	}
	child := decodeBody[model.Issue](t, created).Key
	eventOf := func(eventType string) int64 {
		t.Helper()
		var id int64
		if err := database.Pool.QueryRow(ctx, `
			select id from events where issue_key = $1 and type = $2 order by id desc limit 1
		`, child, eventType).Scan(&id); err != nil {
			t.Fatalf("read %s's newest %s: %v", child, eventType, err)
		}
		return id
	}
	createdEvent := eventOf("issue.created")
	var issueCreated time.Time
	var issueMarker *string
	if err := database.Pool.QueryRow(ctx, `select created_at, node_kind from issues where key = $1`, child).Scan(&issueCreated, &issueMarker); err != nil {
		t.Fatalf("read %s's row: %v", child, err)
	}
	childOf := currentRelations(t, database, child, graphstore.TupleChildOf)
	if len(childOf) != 1 || childOf[0].ToID != parent || childOf[0].SourceEvent != createdEvent || !childOf[0].At.Equal(issueCreated) || issueMarker == nil {
		t.Errorf("%s states child_of %+v with its row naming node %v, want one tuple to %s caused by issue.created %d at the row's created_at %s",
			child, childOf, issueMarker, parent, createdEvent, issueCreated)
	}
	needs := currentRelations(t, database, child, graphstore.TupleNeeds)
	var linkCreated time.Time
	if err := database.Pool.QueryRow(ctx, `select created_at from issue_links where issue_key = $1 and target_key = $2`, child, first).Scan(&linkCreated); err != nil {
		t.Fatalf("read the link's row: %v", err)
	}
	if len(needs) != 1 || needs[0].ToID != first || needs[0].SourceEvent != createdEvent || !needs[0].At.Equal(linkCreated) {
		t.Errorf("%s states needs %+v, want one tuple to %s caused by issue.created %d at the row's created_at %s", child, needs, first, createdEvent, linkCreated)
	}

	if _, err := database.Pool.Exec(ctx, `
		insert into issue_links (issue_key, kind, target_key, created_at) values ($1, 'blocked_by', $2, now() - interval '2 days')
	`, child, old); err != nil {
		t.Fatalf("write the link the release before wrote: %v", err)
	}
	var oldCreated time.Time
	if err := database.Pool.QueryRow(ctx, `select created_at from issue_links where issue_key = $1 and target_key = $2`, child, old).Scan(&oldCreated); err != nil {
		t.Fatalf("read the old link's row: %v", err)
	}
	moved := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{"needs": []string{second, old}}, "alice")
	if moved.Code != http.StatusOK {
		t.Fatalf("move the wait: status=%d body=%s", moved.Code, moved.Body.String())
	}
	updated := eventOf("issue.updated")
	needs = currentRelations(t, database, child, graphstore.TupleNeeds)
	if got := relationEnds(needs); !slices.Equal(got, []string{"issue:" + second, "issue:" + old}) && !slices.Equal(got, []string{"issue:" + old, "issue:" + second}) {
		t.Fatalf("%s states needs %v after the move, want %s and %s", child, got, second, old)
	}
	for _, tuple := range needs {
		if tuple.SourceEvent != updated {
			t.Errorf("the needs to %s is caused by event %d, want issue.updated %d", tuple.ToID, tuple.SourceEvent, updated)
		}
		if tuple.ToID == old && !tuple.At.Equal(oldCreated) {
			t.Errorf("the needs to %s, the release before's row, is dated %s, want the row's created_at %s", old, tuple.At, oldCreated)
		}
	}
	var retracted int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples
		where from_kind = 'issue' and from_id = $1 and kind = 'needs' and to_id = $2 and not asserted and source_event = $3
	`, child, first, updated).Scan(&retracted); err != nil {
		t.Fatalf("read the retraction: %v", err)
	}
	if retracted != 1 {
		t.Errorf("the move appended %d retractions of the needs to %s under issue.updated, want one", retracted, first)
	}
	if standing, _ := issueLinkMarker(t, database, child, first); standing {
		t.Errorf("the issue_links row %s -> %s stands after the move dropped it", child, first)
	}
	for _, target := range []string{second, old} {
		if standing, marker := issueLinkMarker(t, database, child, target); !standing || marker != graphstore.KindIssue {
			t.Errorf("the issue_links row %s -> %s stands %t naming %q, want it naming the issue's node", child, target, standing, marker)
		}
	}

	requireRelationRefused(t, "a write refused after its rows were read", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child,
		map[string]any{"needs": []string{first}, "duplicate_of": "NOPE-1"}, "alice"), http.StatusBadRequest, codeDuplicateOfInput, "duplicate_of")
	if standing, _ := issueLinkMarker(t, database, child, first); standing {
		t.Errorf("the refused write left a row %s -> %s", child, first)
	}
	if got := relationEnds(currentRelations(t, database, child, graphstore.TupleNeeds)); slices.Contains(got, "issue:"+first) {
		t.Errorf("the refused write left the needs %v", got)
	}

	// A reparent: words on the old parent's chain the new parent reaches stay; words it does not
	// reach are retracted.
	under := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "REL", "title": "Under the parent", "parent": parent, "force": true,
	}, "alice")
	if under.Code != http.StatusCreated {
		t.Fatalf("create the issue under the parent: status=%d body=%s", under.Code, under.Body.String())
	}
	sibling := decodeBody[model.Issue](t, under).Key
	words := decodeBody[model.Comment](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+parent+"/comments",
		map[string]any{"body": "File the child under these words."}, "alice"))
	if err := graphstoreWrite(ctx, database, graphstore.Tuple{
		FromKind: graphstore.KindIssue, FromID: child, Kind: graphstore.TuplePartOf, ToKind: graphstore.KindComment, ToID: words.ID,
		At: time.Now().UTC(), Who: model.Actor{Kind: "user", ID: "alice"}, Asserted: true,
	}); err != nil {
		t.Fatalf("place the child under the parent's words: %v", err)
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{"parent": sibling}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("reparent under the parent's child: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := relationEnds(currentRelations(t, database, child, graphstore.TupleChildOf)); !slices.Equal(got, []string{"issue:" + sibling}) {
		t.Errorf("%s states child_of %v after the reparent, want [issue:%s]", child, got, sibling)
	}
	if got := relationEnds(currentRelations(t, database, child, graphstore.TuplePartOf)); !slices.Equal(got, []string{"comment:" + words.ID}) {
		t.Errorf("%s hangs from %v, want the words on %s, which stands above its new parent", child, got, parent)
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{"parent": nil}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("clear the parent: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := currentRelations(t, database, child, graphstore.TupleChildOf); len(got) != 0 {
		t.Errorf("%s states child_of %v with no parent, want none", child, relationEnds(got))
	}
	if got := currentRelations(t, database, child, graphstore.TuplePartOf); len(got) != 0 {
		t.Errorf("%s hangs from %v with no parent, want the words retracted", child, relationEnds(got))
	}

	// A row the release before wrote: under the parent, naming no node. A PATCH that states its
	// wait stamps the row and claims the parent the row names beside the wait.
	if _, err := database.Pool.Exec(ctx, `update issues set parent_key = $2, node_kind = null, node_address = null where key = $1`, unstamped, parent); err != nil {
		t.Fatalf("write %s as the release before: %v", unstamped, err)
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+unstamped, map[string]any{"needs": []string{first}}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("state %s's wait: status=%d body=%s", unstamped, response.Code, response.Body.String())
	}
	var unstampedCreated time.Time
	var unstampedMarker, unstampedAddress *string
	if err := database.Pool.QueryRow(ctx, `select created_at, node_kind, node_address from issues where key = $1`, unstamped).Scan(
		&unstampedCreated, &unstampedMarker, &unstampedAddress); err != nil {
		t.Fatalf("read %s's row: %v", unstamped, err)
	}
	if unstampedMarker == nil || *unstampedMarker != graphstore.KindIssue || unstampedAddress == nil || *unstampedAddress != unstamped {
		t.Errorf("after the PATCH stating its wait %s's row names node %v/%v, want %s/%s", unstamped, unstampedMarker, unstampedAddress, graphstore.KindIssue, unstamped)
	}
	var stampedBy int64
	if err := database.Pool.QueryRow(ctx, `
		select id from events where issue_key = $1 and type = 'issue.updated' order by id desc limit 1
	`, unstamped).Scan(&stampedBy); err != nil {
		t.Fatalf("read %s's issue.updated: %v", unstamped, err)
	}
	if held := currentRelations(t, database, unstamped, graphstore.TupleChildOf); len(held) != 1 || held[0].ToID != parent ||
		!held[0].At.Equal(unstampedCreated) || held[0].SourceEvent != stampedBy {
		t.Errorf("after the PATCH stating its wait %s states child_of %+v, want one tuple to %s at the row's created_at %s caused by issue.updated %d",
			unstamped, held, parent, unstampedCreated, stampedBy)
	}
	if rows, _ := viewAnswers(t, database, rollbackEdge{"issue", unstamped, "child_of", "issue", parent}); rows != 1 {
		t.Errorf("graph_edges answers %s's parent %s %d times, want once", unstamped, parent, rows)
	}
	report, err := graphcensus.StoreCensus(ctx, database.Pool.Config().ConnString(), false, docs.ReturnDeclined)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	for _, kind := range report.Kinds {
		if kind.Kind != "issue_parent" && kind.Kind != "issue_link" {
			continue
		}
		if kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
			t.Errorf("the census of %s reads %d row and %d node differences: %+v %+v", kind.Kind, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
		}
	}
}

// graphstoreWrite appends tuple through the store's writer, as a test seeds a fact no route of
// this build writes yet.
func graphstoreWrite(ctx context.Context, database *store.Store, tuple graphstore.Tuple) error {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{tuple}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TestDuplicateOfTakesAnIssueOfTheProjectAlone holds duplicate_of (R9 of the issue-tree spec) to
// one issue key of the issue's own project, other than the issue's own: a create and a PATCH take
// one, the read answers it as a key and the issue's node states it as its duplicate_of, and a PATCH
// clears it with null. A key of another project, a key no issue holds and the issue's own key are
// each refused 400 DUPLICATE_OF_INPUT naming the field and the value, and the issue keeps what it
// held.
func TestDuplicateOfTakesAnIssueOfTheProjectAlone(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	core := createListedIssues(t, handler, "DUP", 2)
	other := createListedIssues(t, handler, "OTHERDUP", 1)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "DUP", "title": "A repeat", "duplicate_of": core[0], "force": true,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create with duplicate_of: status=%d body=%s", created.Code, created.Body.String())
	}
	repeat := decodeBody[model.Issue](t, created)
	if repeat.DuplicateOf == nil || *repeat.DuplicateOf != core[0] {
		t.Errorf("the create answers duplicate_of %v, want %s", repeat.DuplicateOf, core[0])
	}
	if got := readRelationsIssue(t, handler, repeat.Key).DuplicateOf; got == nil || *got != core[0] {
		t.Errorf("the read answers duplicate_of %v, want %s", got, core[0])
	}
	if got := relationEnds(currentRelations(t, database, repeat.Key, graphstore.TupleDuplicateOf)); !slices.Equal(got, []string{"issue:" + core[0]}) {
		t.Errorf("%s states duplicate_of %v, want [issue:%s]", repeat.Key, got, core[0])
	}
	for _, refused := range []struct{ what, value string }{
		{"a key of another project", other[0]},
		{"a key no issue holds", "DUP-999"},
		{"the issue's own key", repeat.Key},
	} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+repeat.Key, map[string]any{"duplicate_of": refused.value}, "alice")
		requireRelationRefused(t, refused.what, response, http.StatusBadRequest, codeDuplicateOfInput, "duplicate_of", refused.value)
		if got := readRelationsIssue(t, handler, repeat.Key).DuplicateOf; got == nil || *got != core[0] {
			t.Errorf("after %s the issue answers duplicate_of %v, want %s", refused.what, got, core[0])
		}
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+repeat.Key, map[string]any{"duplicate_of": core[1]}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("replace duplicate_of: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := relationEnds(currentRelations(t, database, repeat.Key, graphstore.TupleDuplicateOf)); !slices.Equal(got, []string{"issue:" + core[1]}) {
		t.Errorf("%s states duplicate_of %v after the replace, want [issue:%s]", repeat.Key, got, core[1])
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+repeat.Key, map[string]any{"duplicate_of": nil}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("clear duplicate_of: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := readRelationsIssue(t, handler, repeat.Key).DuplicateOf; got != nil {
		t.Errorf("after the clear the issue answers duplicate_of %s, want null", *got)
	}
	if got := currentRelations(t, database, repeat.Key, graphstore.TupleDuplicateOf); len(got) != 0 {
		t.Errorf("after the clear %s states duplicate_of %v, want none", repeat.Key, relationEnds(got))
	}
}

// TestFiledFromTakesAPublicItemOfTheIssuesProjectAlone holds filed_from (R9 of the issue-tree spec,
// c-issue-relations-read) to the canonical reference of one public ask, comment or message of the
// issue's project. It takes a comment, an ask and a message of an issue and an ask of a project
// document, each read back as that reference and stated as the issue's filed_from to the item's
// node. It refuses, each 400 FILED_FROM_INPUT naming the field and the value with the issue's
// filed_from unchanged, the five values c-issue-relations-read sends - an issue key, the comment's
// block reference, a comment of another project, a comment reference whose uuid no row holds, and
// a spec block's reference - and a comment of the project reserved for its author, refused with
// the same text as the item no row holds and the comment of another project, and a comment's uuid
// written under another issue's key. Its subtests take an ask, a comment and a message the release
// before wrote, with no node, each of which the write gives its node; a comment anchored on a
// document past the store's address bound, which the write leaves out of the store as its own
// writer does; and they hold the read of a comment to reading its comment.created event only where
// the comment has no node, and then within its owner, through that owner's index.
func TestFiledFromTakesAPublicItemOfTheIssuesProjectAlone(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	source := createInteractionIssue(t, handler, "FILE", "Where the words were", "# Spec\n\n:::requirement{#r-one}\nR1. One.\n:::\n")
	elsewhere := createInteractionIssue(t, handler, "ELSE", "Another project's issue", "# Spec\n")
	filed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "FILE", "title": "Filed out of a conversation", "force": true,
	}, "alice")
	if filed.Code != http.StatusCreated {
		t.Fatalf("create the filed issue: status=%d body=%s", filed.Code, filed.Body.String())
	}
	key := decodeBody[model.Issue](t, filed).Key
	other := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "FILE", "title": "A sibling", "force": true,
	}, "alice")
	sibling := decodeBody[model.Issue](t, other).Key

	comment := decodeBody[model.Comment](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source.Key+"/comments",
		map[string]any{"body": "We should split this out."}, "alice"))
	ask := createIssueAsk(t, handler, source.Key, "Split it out?")
	message := createIssueMessage(t, handler, source.Key, map[string]any{"body": "Splitting it out."}, "alice")
	document := createProjectDocument(t, handler, "FILE", "Notes", "# Notes\n\nA project's notes.\n")
	documentAsk := decodeBody[model.Ask](t, sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
		"question": "File this?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}}, "actor": sessionActor(),
	}))
	foreign := decodeBody[model.Comment](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+elsewhere.Key+"/comments",
		map[string]any{"body": "Words of another project."}, "alice"))
	private := decodeBody[model.Comment](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source.Key+"/comments",
		map[string]any{"body": "Words for their author alone."}, "alice"))
	if _, err := database.Pool.Exec(ctx, `update graph_nodes set visibility = $3 where kind = $1 and id = $2`,
		graphstore.KindComment, private.ID, graphstore.VisibilityAuthor); err != nil {
		t.Fatalf("reserve the comment for its author: %v", err)
	}

	takes := []struct{ what, kind, id, ref string }{
		{"a comment of an issue", graphstore.KindComment, comment.ID, "dispatch://" + source.Key + "/comment/" + comment.ID},
		{"an ask of an issue", graphstore.KindAsk, ask, "dispatch://" + source.Key + "/ask/" + ask},
		{"a message of an issue", graphstore.KindMessage, message.ID, "dispatch://" + source.Key + "/message/" + message.ID},
		{"an ask of a project document", graphstore.KindAsk, documentAsk.ID, "dispatch://FILE/artifact/" + document.Slug + "/ask/" + documentAsk.ID},
	}
	for _, take := range takes {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"filed_from": take.ref}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("take %s: status=%d body=%s", take.what, response.Code, response.Body.String())
		}
		if got := decodeBody[model.Issue](t, response).FiledFrom; got == nil || *got != take.ref {
			t.Errorf("the PATCH taking %s answers filed_from %v, want %s", take.what, got, take.ref)
		}
		if got := readRelationsIssue(t, handler, key).FiledFrom; got == nil || *got != take.ref {
			t.Errorf("after %s the read answers filed_from %v, want %s", take.what, got, take.ref)
		}
		if got := relationEnds(currentRelations(t, database, key, graphstore.TupleFiledFrom)); !slices.Equal(got, []string{take.kind + ":" + take.id}) {
			t.Errorf("after %s %s states filed_from %v, want [%s:%s]", take.what, key, got, take.kind, take.id)
		}
	}
	held := takes[len(takes)-1].ref

	unseen := "names no public ask, comment or message of project FILE"
	missing := "11111111-2222-4333-8444-555555555555"
	for _, refused := range []struct{ what, value, says string }{
		{"an issue key", sibling, ""},
		{"the comment's block reference", "dispatch://" + source.Key + "/comment/" + comment.ID + "#b-1", ""},
		{"a comment of another project", "dispatch://" + elsewhere.Key + "/comment/" + foreign.ID, unseen},
		{"a comment no row holds", "dispatch://" + source.Key + "/comment/" + missing, unseen},
		{"a spec block's reference", "dispatch://" + source.Key + "/spec#b-r-one", ""},
		{"a comment reserved for its author", "dispatch://" + source.Key + "/comment/" + private.ID, unseen},
		{"a comment's uuid under another issue's key", "dispatch://" + sibling + "/comment/" + comment.ID, unseen},
	} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"filed_from": refused.value}, "alice")
		answer := requireRelationRefused(t, refused.what, response, http.StatusBadRequest, codeFiledFromInput, "filed_from", refused.value)
		if refused.says != "" && !strings.Contains(answer.Error, refused.says) {
			t.Errorf("%s is refused %q, want the one text %q an item the caller may not see is refused with", refused.what, answer.Error, refused.says)
		}
		if got := readRelationsIssue(t, handler, key).FiledFrom; got == nil || *got != held {
			t.Errorf("after %s the issue answers filed_from %v, want %s", refused.what, got, held)
		}
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"filed_from": nil}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("clear filed_from: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := currentRelations(t, database, key, graphstore.TupleFiledFrom); len(got) != 0 {
		t.Errorf("after the clear %s states filed_from %v, want none", key, relationEnds(got))
	}

	t.Run("an ask, a comment and a message the release before wrote, with no node", func(t *testing.T) {
		var oldAsk, oldMessage string
		if err := database.Pool.QueryRow(ctx, `
			insert into asks (issue_key, author, question) values ($1, '{"kind":"user","id":"alice"}', 'Split it out, as asked before?')
			returning id::text
		`, source.Key).Scan(&oldAsk); err != nil {
			t.Fatalf("write an ask as the release before: %v", err)
		}
		oldComment, stored := seedNodelessComment(t, database, source.Key, "Split it out, as said before.")
		if err := database.Pool.QueryRow(ctx, `
			insert into messages (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'Splitting it out, as said before.')
			returning id::text
		`, source.Key).Scan(&oldMessage); err != nil {
			t.Fatalf("write a message as the release before: %v", err)
		}
		for _, item := range []struct{ what, kind, id string }{
			{"an ask", graphstore.KindAsk, oldAsk},
			{"a comment", graphstore.KindComment, oldComment},
			{"a message", graphstore.KindMessage, oldMessage},
		} {
			if _, held, err := graphstore.ReadNode(ctx, database.Pool, item.kind, item.id); err != nil || held {
				t.Fatalf("the store holds the node of %s the release before wrote %t (err %v), so this proves nothing", item.what, held, err)
			}
			ref := "dispatch://" + source.Key + "/" + item.kind + "/" + item.id
			if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"filed_from": ref}, "alice"); response.Code != http.StatusOK {
				t.Fatalf("take %s with no node: status=%d body=%s", item.what, response.Code, response.Body.String())
			}
			node, held, err := graphstore.ReadNode(ctx, database.Pool, item.kind, item.id)
			if err != nil || !held || node.Project != "FILE" || node.Visibility != graphstore.VisibilityProject || node.State != graphstore.StateLive {
				t.Errorf("after the PATCH the store holds %s's node %t as %+v (err %v), want it live and public in FILE", item.what, held, node, err)
			}
			if got := relationEnds(currentRelations(t, database, key, graphstore.TupleFiledFrom)); !slices.Equal(got, []string{item.kind + ":" + item.id}) {
				t.Errorf("after taking %s %s states filed_from %v, want [%s:%s]", item.what, key, got, item.kind, item.id)
			}
			if rows, _ := viewAnswers(t, database, rollbackEdge{"issue", key, graphstore.TupleFiledFrom, item.kind, item.id}); rows != 1 {
				t.Errorf("graph_edges answers %s's filed_from to %s %d times, want once", key, item.what, rows)
			}
		}
		var marker *string
		if err := database.Pool.QueryRow(ctx, `select node_kind from comments where id = $1::uuid`, oldComment).Scan(&marker); err != nil {
			t.Fatalf("read the comment's row: %v", err)
		}
		if marker == nil || *marker != graphstore.KindComment {
			t.Errorf("the comment's row names node %v, want %s: the write that stated its node names it", marker, graphstore.KindComment)
		}
		authored, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComment, oldComment, []string{graphstore.TupleAuthoredBy})
		if err != nil {
			t.Fatalf("read who wrote the comment: %v", err)
		}
		if len(authored) != 1 || authored[0].SourceEvent != stored {
			t.Errorf("the comment states authored_by %+v, want one tuple caused by the comment.created %d that stored it", authored, stored)
		}
	})

	t.Run("a comment anchored on a document past the address bound stays out of the store", func(t *testing.T) {
		slug := randText(maxNewArtifactKeyBytes - len("FILE/"))
		var anchorID, refKey string
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (project_key, slug, name, kind, created_by)
			values ('FILE', $1, $1, 'doc', '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, slug).Scan(&anchorID, &refKey); err != nil {
			t.Fatalf("create the long-keyed document: %v", err)
		}
		if graphstore.AddressFits(refKey) {
			t.Fatalf("the document's ref_key fits the store bound, so this proves nothing")
		}
		var remark string
		if err := database.Pool.QueryRow(ctx, `
			insert into comments (issue_key, author, body, anchor)
			values ($1, '{"kind":"user","id":"alice"}', 'A remark on a long-keyed document.', jsonb_build_object('artifact_id', $2::text))
			returning id::text
		`, source.Key, anchorID).Scan(&remark); err != nil {
			t.Fatalf("write the anchored comment as the release before: %v", err)
		}
		ref := "dispatch://" + source.Key + "/comment/" + remark
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"filed_from": ref}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("take the comment anchored past the bound: status=%d body=%s", response.Code, response.Body.String())
		}
		if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindComment, remark); err != nil || held {
			t.Errorf("the store holds the node of the comment anchored past the bound %t (err %v), want none", held, err)
		}
		var marker *string
		if err := database.Pool.QueryRow(ctx, `select node_kind from comments where id = $1::uuid`, remark).Scan(&marker); err != nil {
			t.Fatalf("read the comment's row: %v", err)
		}
		if marker != nil {
			t.Errorf("the comment's row names node %q, want none, so the old comments arm answers its edges", *marker)
		}
		if got := relationEnds(currentRelations(t, database, key, graphstore.TupleFiledFrom)); !slices.Equal(got, []string{"comment:" + remark}) {
			t.Errorf("%s states filed_from %v, want [comment:%s]", key, got, remark)
		}
		requireGraphEdge(t, database, graphstore.KindComment, remark, graphstore.TupleAnchoredTo, refKey)
	})

	t.Run("a comment's event is read within its owner, and only for a comment with no node", func(t *testing.T) {
		// eventReads reads the item ref names as a filed_from does, and answers the statements the
		// read sent over events.
		eventReads := func(what, ref string) []sentStatement {
			t.Helper()
			parsed, ok := text.ParseRef(ref, "")
			if !ok {
				t.Fatalf("%s is no reference", ref)
			}
			kept := &keepingQueryer{queryer: database.Pool}
			if _, public, err := readFiledFromItem(ctx, kept, "FILE", parsed); err != nil || !public {
				t.Fatalf("read %s as a filed_from: public %t, err %v", what, public, err)
			}
			var reads []sentStatement
			for _, sent := range kept.sent {
				if strings.Contains(sent.sql, "from events") {
					reads = append(reads, sent)
				}
			}
			return reads
		}
		if reads := eventReads("a comment with its node", "dispatch://"+source.Key+"/comment/"+comment.ID); len(reads) != 0 {
			t.Errorf("reading a comment the store holds the node of sends %d statements over events, want none: %q", len(reads), reads[0].sql)
		}
		nodeless, _ := seedNodelessComment(t, database, source.Key, "Said before the store held comments.")
		reads := eventReads("a comment with no node", "dispatch://"+source.Key+"/comment/"+nodeless)
		if len(reads) != 1 {
			t.Fatalf("reading a comment the store holds no node of sends %d statements over events, want one", len(reads))
		}
		plan := planWithoutSeqScans(t, database, reads[0].sql, reads[0].args...)
		var keyed []explaintest.IndexScan
		for _, scan := range explaintest.IndexScans(t, plan) {
			if scan.Relation == "events" || strings.HasPrefix(scan.Index, "events") {
				keyed = append(keyed, scan)
			}
		}
		if explaintest.SeqScansRelation(t, plan, "events") || len(keyed) == 0 {
			t.Errorf("the read of the comment's event scans events through no index: %s\n%s", reads[0].sql, plan)
		}
		for _, scan := range keyed {
			if !strings.Contains(scan.Cond, "issue_key") {
				t.Errorf("the read of the comment's event reads %s keyed on %q, not on the comment's owner, its issue_key: %s", scan, scan.Cond, reads[0].sql)
			}
		}
	})
}

// seedNodelessComment writes a comment on issue as the release before writes one, naming no node,
// with the comment.created event that stored it, and answers its id and that event's.
func seedNodelessComment(t *testing.T, database *store.Store, issue, body string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text
	`, issue, body).Scan(&id); err != nil {
		t.Fatalf("write a comment as the release before: %v", err)
	}
	var event int64
	if err := database.Pool.QueryRow(ctx, `
		with bumped as (
			update issues set last_seq = last_seq + 1 where key = $1 returning key, last_seq
		)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select key, last_seq, 'comment.created', '{"kind":"user","id":"alice"}', jsonb_build_object('id', $2::text), false
		from bumped
		returning id
	`, issue, id).Scan(&event); err != nil {
		t.Fatalf("append the comment.created of the comment the release before wrote: %v", err)
	}
	return id, event
}

// keepingQueryer is a queryer that keeps every statement a read sends through it, with its
// arguments, so a test reads what the read sent and can explain it.
type keepingQueryer struct {
	queryer
	sent []sentStatement
}

func (k *keepingQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	k.sent = append(k.sent, sentStatement{sql: sql, args: args})
	return k.queryer.QueryRow(ctx, sql, args...)
}

func (k *keepingQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	k.sent = append(k.sent, sentStatement{sql: sql, args: args})
	return k.queryer.Query(ctx, sql, args...)
}
