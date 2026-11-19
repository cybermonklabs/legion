package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestACommentAskedForAuthorIsRefusedNamingItsKind holds the refusal of `author` on a kind the
// read registry does not flag to a route of a kind that exists today
// (dispatch://INFRA-1950/spec#b-r-node-visibility): a comment asked for `author` on a credential
// that proves an author is refused by the store, 400 VISIBILITY_NOT_ADMITTED, whose message names
// the kind comment, and nothing of it is stored - the issue's comment list, the comments table and
// the store's comment nodes hold what they held. On the shared bearer the same ask is refused
// earlier, 403 AUTHOR_NAMES_NOBODY, by the one author rule; a word that is no visibility is 400
// INVALID_ATTRIBUTE_VALUE. `project`, or no visibility at all, writes the comment as today,
// project-visible.
func TestACommentAskedForAuthorIsRefusedNamingItsKind(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "VIS", "A comment asked for author", "# Spec\n\nA passage.\n")
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	held := func() (listed, rows, nodes int) {
		t.Helper()
		listed = countCommentRows(t, handler, issue.Key)
		if err := database.Pool.QueryRow(context.Background(), `
			select (select count(*) from comments where issue_key = $1),
			       (select count(*) from graph_nodes where kind = $2)`, issue.Key, graphstore.KindComment).Scan(&rows, &nodes); err != nil {
			t.Fatalf("count the comments: %v", err)
		}
		return listed, rows, nodes
	}
	beforeListed, beforeRows, beforeNodes := held()

	for _, refused := range []struct {
		name    string
		send    func() *httptest.ResponseRecorder
		status  int
		code    string
		message string
	}{
		{"author on a person's cookie", func() *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, comments, map[string]any{"body": "Only for me.", "visibility": "author"}, "alice")
		}, http.StatusBadRequest, "VISIBILITY_NOT_ADMITTED", "the node kind comment takes no node reserved for its author"},
		{"author on the shared bearer", func() *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, comments, map[string]any{
				"body": "Only for me.", "visibility": "author", "actor": sessionActor(),
			})
		}, http.StatusForbidden, "AUTHOR_NAMES_NOBODY", "names nobody"},
		{"a word that is no visibility", func() *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, comments, map[string]any{"body": "For whom?", "visibility": "public"}, "alice")
		}, http.StatusBadRequest, "INVALID_ATTRIBUTE_VALUE", `the node visibility "public" is none of project, author`},
	} {
		response := refused.send()
		t.Logf("%s: status=%d body=%s", refused.name, response.Code, response.Body.String())
		if response.Code != refused.status {
			t.Fatalf("%s: status=%d body=%s, want %d", refused.name, response.Code, response.Body.String(), refused.status)
		}
		refusal := decodeBody[refusalAnswer](t, response)
		if refusal.Code != refused.code || !strings.Contains(refusal.Error, refused.message) {
			t.Errorf("%s is refused %+v, want %s naming %q", refused.name, refusal, refused.code, refused.message)
		}
		if listed, rows, nodes := held(); listed != beforeListed || rows != beforeRows || nodes != beforeNodes {
			t.Errorf("after %s the issue lists %d comments, holds %d comment rows and %d comment nodes, and held %d, %d and %d before",
				refused.name, listed, rows, nodes, beforeListed, beforeRows, beforeNodes)
		}
	}

	for _, written := range []struct {
		name string
		body map[string]any
	}{
		{"no visibility", map[string]any{"body": "For everyone."}},
		{"project", map[string]any{"body": "For everyone, said so.", "visibility": "project"}},
	} {
		created := createThreadComment(t, handler, issue.Key, written.body, "alice")
		node, found, err := graphstore.ReadNode(context.Background(), database.Pool, graphstore.KindComment, created.ID)
		if err != nil || !found || node.Visibility != graphstore.VisibilityProject {
			t.Errorf("the comment written with %s reads %+v (found %t, %v), want it project-visible", written.name, node, found, err)
		}
	}
	if listed, _, _ := held(); listed != beforeListed+2 {
		t.Errorf("the issue lists %d comments after the two project ones, want %d", listed, beforeListed+2)
	}
}

// TestAPersonPageAnswersOnlyWhereThePersonHoldsANodeTheReaderMaySee holds the person page to its
// rule: GET /api/v1/people/{login}/nodes answers 200 only where that
// person holds at least one node the reader may see, through each node's own kind's predicate,
// or where the reader is that person, and otherwise, byte for byte, what a login nobody holds
// answers. carol's only node is a comment reserved for her: bob reads her page as a login nobody
// holds, and carol reads her own with the comment as author. alice holds a project comment, one
// reserved for her, a lens on the issue reserved for her, a run of the command bar her cookie
// asked and one her personal token asked: bob's page of her lists the project comment alone; the
// page her cookie reads lists the three, the lens titled by its verdict and opening the issue's
// page, and the cookie's run titled by the first line of its words and opening nothing; and the
// page her token reads lists the project comment, the comment reserved for her and the lens,
// whose predicates both compare the author a credential proved (dispatch://INFRA-1149's one rule),
// and the token's run, and not the run her cookie asked, whose predicate compares the whole
// credential. Each row answers its state and its visibility.
//
// The store's writer takes no comment reserved for its author (graphstore.ErrVisibilityNotAdmitted),
// so each such comment's node is seeded as such a write would store it
// (graphstoretest.SeedAuthorOnly), with its row and its authored_by tuple beside it. The lens is
// written through the one write path of its kind (writeReasoningNodes), and each run through the
// going POST's own record (recordGoingRequest), the token's as the token resolves.
func TestAPersonPageAnswersOnlyWhereThePersonHoldsANodeTheReaderMaySee(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	server := directServer(deps)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "PAGE", "A person's page", "# Spec\n\nA passage.\n")
	public := createThreadComment(t, handler, issue.Key, map[string]any{"body": "A remark alice wrote for everyone."}, "alice")
	alicesPrivate := seedAuthorOnlyComment(t, database, issue.Key, model.Actor{Kind: "user", ID: "alice"}, "A remark alice keeps.")
	carolsPrivate := seedAuthorOnlyComment(t, database, issue.Key, model.Actor{Kind: "user", ID: "carol"}, "A remark carol keeps.")
	alicesLens := writeAuthorOnlyLens(t, database, issue.Key, model.Actor{Kind: "user", ID: "alice"}, "A verdict alice keeps.")
	cookieRun, err := server.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "",
		"Which risk did alice's cookie ask about?\nA second line no title shows.")
	if err != nil {
		t.Fatalf("record the run alice's cookie asked: %v", err)
	}
	token := personalTokenOwnedBy(t, database, "alice", model.ScopePerson)
	resolving := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/people/alice/nodes", nil)
	resolving.Header.Set("Authorization", "Bearer "+token)
	tokenActor, _, err := server.optionalActor(resolving)
	if err != nil || tokenActor.Owner == nil || *tokenActor.Owner != "alice" {
		t.Fatalf("alice's personal token resolves as %+v (%v), want a credential alice owns", tokenActor, err)
	}
	tokenRun, err := server.recordGoingRequest(ctx, tokenActor, "", "Which risk did alice's token ask about?")
	if err != nil {
		t.Fatalf("record the run alice's token asked: %v", err)
	}
	page := func(person, reader string) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/"+person+"/nodes", nil, reader)
	}

	nobody := page("nobody-at-all", "bob")
	if nobody.Code != http.StatusNotFound {
		t.Fatalf("a login nobody holds answers %d %s, want 404, so the comparison proves nothing", nobody.Code, nobody.Body.String())
	}
	if _, found, err := graphstore.ReadNode(context.Background(), database.Pool, graphstore.KindPerson, "carol"); err != nil || !found {
		t.Fatalf("the store holds no person node of carol (%v), so her page would be a 404 for that alone and the comparison would prove nothing", err)
	}
	hidden := page("carol", "bob")
	t.Logf("bob reads carol's page: %d %s; and a login nobody holds: %d %s", hidden.Code, hidden.Body.String(), nobody.Code, nobody.Body.String())
	if hidden.Code != nobody.Code || hidden.Body.String() != nobody.Body.String() ||
		hidden.Header().Get("Content-Type") != nobody.Header().Get("Content-Type") {
		t.Errorf("bob reads carol's page, whose only node is reserved for her, as %d %s, and a login nobody holds as %d %s",
			hidden.Code, hidden.Body.String(), nobody.Code, nobody.Body.String())
	}
	comment := func(visibility string) personPageRow {
		return personPageRow{kind: graphstore.KindComment, visibility: visibility}
	}
	lens := personPageRow{kind: graphstore.KindLens, visibility: graphstore.VisibilityAuthor,
		title: "A verdict alice keeps.", href: "/issues/" + issue.Key}
	requirePage(t, page("carol", "carol"), "carol", map[string]personPageRow{carolsPrivate: comment(graphstore.VisibilityAuthor)})

	requirePage(t, page("alice", "bob"), "alice", map[string]personPageRow{public.ID: comment(graphstore.VisibilityProject)})
	requirePage(t, page("alice", "alice"), "alice", map[string]personPageRow{
		public.ID: comment(graphstore.VisibilityProject), alicesPrivate: comment(graphstore.VisibilityAuthor), alicesLens: lens,
		cookieRun.ID: {kind: graphstore.KindInteraction, visibility: graphstore.VisibilityAuthor,
			title: "Which risk did alice's cookie ask about?", opensNothing: true},
	})
	requirePage(t, personPageRequest(t, handler, "/api/v1/people/alice/nodes", token), "alice", map[string]personPageRow{
		public.ID: comment(graphstore.VisibilityProject), alicesPrivate: comment(graphstore.VisibilityAuthor), alicesLens: lens,
		tokenRun.ID: {kind: graphstore.KindInteraction, visibility: graphstore.VisibilityAuthor,
			title: "Which risk did alice's token ask about?", opensNothing: true},
	})
}

// personPageRow is what a row of a person's page is held to: its kind and visibility, and, where
// they are set, its title and the dashboard path it opens, or that it opens none.
type personPageRow struct {
	kind, visibility string
	title, href      string
	opensNothing     bool
}

// requirePage fails unless response is person's page, 200, listing exactly the nodes want names,
// each live, of the kind and at the visibility want gives it, with the title and the href want
// gives it where want gives one, and with no href where want says it opens nothing.
func requirePage(t *testing.T, response *httptest.ResponseRecorder, person string, want map[string]personPageRow) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("the page of %s answers %d %s, want 200", person, response.Code, response.Body.String())
	}
	read := decodeBody[personNodes](t, response)
	t.Logf("the page of %s: %+v", person, read)
	if read.Person != person || read.Total != len(want) || len(read.Rows) != len(want) {
		t.Fatalf("the page of %s = %+v, want %d rows", person, read, len(want))
	}
	for _, row := range read.Rows {
		wanted, listed := want[row.ID]
		if !listed || row.Kind != wanted.kind {
			t.Errorf("the page of %s lists %+v, which is none of %v", person, row, want)
			continue
		}
		if row.State != graphstore.StateLive || row.Visibility != wanted.visibility {
			t.Errorf("the page of %s answers %s at state %q and visibility %q, want %q and %q",
				person, row.ID, row.State, row.Visibility, graphstore.StateLive, wanted.visibility)
		}
		if (wanted.title != "" && row.Title != wanted.title) || (wanted.href != "" && row.Href != wanted.href) ||
			(wanted.opensNothing && row.Href != "") {
			t.Errorf("the page of %s titles %s %q and opens it at %q, want %q at %q (opening nothing: %t)",
				person, row.ID, row.Title, row.Href, wanted.title, wanted.href, wanted.opensNothing)
		}
	}
}

// writeAuthorOnlyLens writes a lens on issue reserved for author, a cookie's person, with verdict
// as its verdict, through the one write path of a lens (writeReasoningNodes), and answers its id.
func writeAuthorOnlyLens(t *testing.T, database *store.Store, issue string, author model.Actor, verdict string) string {
	t.Helper()
	id := uuid.NewString()
	writeReasoningAs(t, database, issue, author, reasoningNode{
		kind: graphstore.KindLens, id: id, visibility: graphstore.VisibilityAuthor,
		parentKind: graphstore.KindIssue, parentID: issue,
		attributes: map[string]any{graphstore.AttributeVerdict: verdict},
		tuples: []graphstore.Tuple{{FromKind: graphstore.KindLens, FromID: id, Kind: graphstore.TupleHasKind,
			ToKind: graphstore.KindLensKind, ToID: "trade-off"}},
	})
	return id
}

// seedAuthorOnlyComment writes a comment on issue reserved for author as the comment write would
// store it if the store took one: its node, its row naming that node, and its authored_by tuple
// with the author's own person node (graphstore.Authored). It answers the comment's id.
func seedAuthorOnlyComment(t *testing.T, database *store.Store, issue string, author model.Actor, body string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := database.Pool.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("draw the comment's id: %v", err)
	}
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the comment's write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstoretest.SeedAuthorOnly(ctx, tx, graphstore.Node{
		Kind: graphstore.KindComment, ID: id, Supertag: graphstore.KindComment, State: graphstore.StateLive,
		MadeAt: at, MadeBy: author, SourceEvent: graphstore.NoEvent,
	}); err != nil {
		t.Fatalf("seed the comment's node: %v", err)
	}
	encoded, err := encodeJSON(author)
	if err != nil {
		t.Fatalf("encode the comment's author: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into comments (id, issue_key, author, body, node_kind, node_address, created_at)
		values ($1::uuid, $2, $3, $4, $5, $1, $6)
	`, id, issue, encoded, body, graphstore.KindComment, at); err != nil {
		t.Fatalf("write the comment's row: %v", err)
	}
	if err := graphstore.Authored(ctx, tx, graphstore.KindComment, id, author, at, graphstore.NoEvent); err != nil {
		t.Fatalf("record who wrote the comment: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the comment's write: %v", err)
	}
	return id
}
