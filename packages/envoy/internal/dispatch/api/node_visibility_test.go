package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The phrase the private block carries, rare enough that a search for it names what holds it and
// nothing else.
const privateNodePhrase = "wolfram tangerine"

// privateNodeFixture is the seed both halves of the test read: a node reserved for its author in
// public company. The private node is a block of a public document, so a walk down the
// document's part_of edges reaches it; a public block points at it and it points at a second
// public block, so a chain of three runs through it; and a public comment answers it, so a walk
// up from a visible child reaches it.
type privateNodeFixture struct {
	handler  http.Handler
	database *store.Store
	issue    string
	specID   string
	refKey   string
	// first and last are the two public blocks of the chain; private is the block in between,
	// absent from the fixture seeded without it.
	first, private, last string
	// comment is the public comment that answers the private block, absent with it.
	comment string
}

// author and reader are the two people of this test: the one who owns the private node and one
// who owns none of it and acts for every read below.
func privateNodeAuthor() model.Actor { return model.Actor{Kind: "user", ID: "alice"} }
func privateNodeReader() model.Actor { return model.Actor{Kind: "user", ID: "bob"} }

// TestNoReadAnswersANodeTheActingPersonMayNotSee: a node whose `visibility` is `author` is
// absent from every read a second person makes, and present in each of them for its author.
//
// Nothing writes such a node from a production path yet - no route asks for `author` - so the
// seed writes it through the store's own writer (graphstore.Write), which is the write this
// ticket's read side is built against: the rule under test is the read, not the write that one
// day asks for it.
//
// The bound of a node-level predicate: the keyword `document` arm of search ranks a document's
// whole body, and the private block's text is part of its document's body, so that document
// still matches a search for the phrase - for the second person as for the author. The node
// itself is absent from every read; what a document's own text says is the document's, and the
// document is public. The test asserts both halves of that where it meets them.
func TestNoReadAnswersANodeTheActingPersonMayNotSee(t *testing.T) {
	ctx := context.Background()
	held := seedPrivateNode(t, true)
	// The same seed without the private node ever written, which the counts are compared
	// against: every count the second person reads has to equal the count they would read in a
	// world where that node does not exist.
	absent := seedPrivateNode(t, false)

	reader, author := privateNodeReader(), privateNodeAuthor()
	pool := held.database.Pool
	blockRef := refs.BlockRef("dispatch://"+held.refKey, "private")
	// 1. The list of its kind: every link from a block of the document to a block of it. The
	// chain runs first -> private -> last, so a reader who may not see the middle sees neither
	// of its links.
	links, err := refs.BlockLinks(ctx, pool, reader, held.specID, held.specID)
	if err != nil {
		t.Fatalf("list the document's block links: %v", err)
	}
	for _, link := range links {
		if link.From.ID == held.private || link.To.ID == held.private {
			t.Errorf("the block list answers the private node: %+v", link)
		}
	}
	if len(links) != 0 {
		t.Errorf("the second person reads %d block link, want none: both run through the private node", len(links))
	}
	if authored, err := refs.BlockLinks(ctx, pool, author, held.specID, held.specID); err != nil {
		t.Fatalf("list the document's block links as its author: %v", err)
	} else if len(authored) != 2 {
		t.Errorf("the author reads %d block link, want the two the chain holds", len(authored))
	}

	// 2. Search. The document matches for both people, because its own body holds the phrase;
	// no result is the private node, and none names it as the block it matched.
	for _, who := range []string{"bob", "alice"} {
		response := dispatchRequest(t, held.handler, http.MethodGet,
			"/api/v1/search?q="+url.QueryEscape(privateNodePhrase), nil, who)
		if response.Code != http.StatusOK {
			t.Fatalf("search as %s: status=%d body=%s", who, response.Code, response.Body.String())
		}
		found := decodeBody[model.SearchResponse](t, response)
		document := false
		for _, result := range found.Results {
			if result.ID == held.private || (result.BlockID != nil && *result.BlockID == held.private) {
				t.Errorf("search as %s answers the private node: %+v", who, result)
			}
			if result.Kind == "document" {
				document = true
			}
		}
		if !document {
			t.Errorf("search as %s names no document, and the document's own body holds the phrase", who)
		}
	}

	// 3. The tuples of each public neighbour, in both directions.
	for _, neighbour := range []struct{ kind, id string }{
		{"block", held.first}, {"block", held.last}, {"comment", held.comment},
	} {
		for _, direction := range []string{"in", "out"} {
			query := refs.Query{Direction: direction, Kind: neighbour.kind, ID: neighbour.id}
			edges, err := refs.Edges(ctx, pool, reader, query)
			if err != nil {
				t.Fatalf("read the %s edges of %s %s: %v", direction, neighbour.kind, neighbour.id, err)
			}
			for _, edge := range edges {
				if edge.Node.ID == held.private {
					t.Errorf("the %s edges of %s %s answer the private node", direction, neighbour.kind, neighbour.id)
				}
				if edge.Excerpt != nil && strings.Contains(edge.Excerpt.Text, privateNodePhrase) {
					t.Errorf("the %s edges of %s %s carry the private node's text in an excerpt",
						direction, neighbour.kind, neighbour.id)
				}
			}
		}
	}

	// 4. The backlink count on the public document, and 5. the chain read down its part_of
	// tuples. Each is compared with the same read in the database seeded without the node.
	for _, kind := range []string{"artifact", "block", "comment"} {
		ids := map[string][]string{
			"artifact": {held.refKey},
			"block":    {held.first, held.last},
			"comment":  {held.comment},
		}[kind]
		counted, err := refs.BacklinkCounts(ctx, pool, reader, kind, ids, nil)
		if err != nil {
			t.Fatalf("count %s backlinks: %v", kind, err)
		}
		never, err := refs.BacklinkCounts(ctx, absent.database.Pool, reader, kind, privateNodeIDs(absent, kind), nil)
		if err != nil {
			t.Fatalf("count %s backlinks where the node was never written: %v", kind, err)
		}
		if len(counted) != len(never) {
			t.Errorf("the second person's %s counts are %v, and %v where the private node was never written",
				kind, counted, never)
			continue
		}
		for id, count := range counted {
			if want := never[privateNodePeer(held, absent, id)]; count != want {
				t.Errorf("the second person counts %d backlink of %s %s, and %d where the private node was never written",
					count, kind, id, want)
			}
		}
	}
	parts, err := refs.Edges(ctx, pool, reader, refs.Query{Direction: "in", Kind: "artifact", ID: held.refKey})
	if err != nil {
		t.Fatalf("walk down the document's part_of tuples: %v", err)
	}
	for _, edge := range parts {
		if edge.Node.ID == held.private || (edge.Block != nil && edge.Block.ID == held.private) {
			t.Errorf("the walk down the document answers the private node: %+v", edge)
		}
	}
	if authored, err := refs.Edges(ctx, pool, author, refs.Query{Direction: "in", Kind: "artifact", ID: held.refKey}); err != nil {
		t.Fatalf("walk down the document's part_of tuples as its author: %v", err)
	} else if !privateNodeAmong(authored, held.private) {
		t.Errorf("the author's walk down the document does not answer the private node")
	}

	// 6. The walk up from the public comment: its replies_to edge names the block it answers.
	up, err := refs.Edges(ctx, pool, reader, refs.Query{Direction: "out", Kind: "comment", ID: held.comment})
	if err != nil {
		t.Fatalf("walk up from the public comment: %v", err)
	}
	if privateNodeAmong(up, held.private) {
		t.Errorf("the walk up from the public comment answers the private node")
	}
	if authored, err := refs.Edges(ctx, pool, author, refs.Query{Direction: "out", Kind: "comment", ID: held.comment}); err != nil {
		t.Fatalf("walk up from the public comment as the author: %v", err)
	} else if !privateNodeAmong(authored, held.private) {
		t.Errorf("the author's walk up from the public comment does not answer the private node")
	}

	// 7. The person page read: what the author's own node answers, and what points at it.
	if _, found, err := refs.Node(ctx, pool, reader, graphstore.KindPerson, "alice", true); err != nil {
		t.Fatalf("read the person node: %v", err)
	} else if !found {
		t.Fatalf("the store holds no person node for the author, so the page read proves nothing")
	}
	page, err := refs.Edges(ctx, pool, reader, refs.Query{Direction: "in", Kind: graphstore.KindPerson, ID: "alice"})
	if err != nil {
		t.Fatalf("read the person page: %v", err)
	}
	if privateNodeAmong(page, held.private) {
		t.Errorf("the person page answers the private node")
	}

	// 8. The render of an earlier version, through the as-of index, and 9. the node's own
	// address. Each answers what an address nobody holds answers, byte for byte once the
	// address asked for is substituted: the two refusals differ in the block they name and in
	// nothing else.
	for _, pin := range []string{"", "@v1"} {
		private := dispatchReferences(t, held.handler, blockRef+pin, "bob")
		nobody := dispatchReferences(t, held.handler,
			refs.BlockRef("dispatch://"+held.refKey, "nosuchblock")+pin, "bob")
		if private.Code != nobody.Code {
			t.Errorf("the private node's address%s answers %d and an address nobody holds answers %d",
				pin, private.Code, nobody.Code)
		}
		want := strings.ReplaceAll(nobody.Body.String(), "nosuchblock", "private")
		if private.Body.String() != want {
			t.Errorf("the private node's address%s answers\n%s\nand an address nobody holds answers\n%s",
				pin, private.Body.String(), want)
		}
	}
	if authored := dispatchReferences(t, held.handler, blockRef, "alice"); authored.Code != http.StatusOK {
		t.Errorf("the author's read of the private node's address: status=%d body=%s",
			authored.Code, authored.Body.String())
	}

	// The predicate sits in the SQL of each read and not in Go: every statement these reads
	// issue states its visibility rule in SQL, and every such rule is one the registry rendered
	// (refs.VisibilityMarker).
	recorder := &recordedReads{inner: pool}
	if _, err := refs.Edges(ctx, recorder, reader, refs.Query{Direction: "in", Kind: "artifact", ID: held.refKey}); err != nil {
		t.Fatalf("record the document's read: %v", err)
	}
	if _, err := refs.BacklinkCounts(ctx, recorder, reader, "artifact", []string{held.refKey}, nil); err != nil {
		t.Fatalf("record the document's count: %v", err)
	}
	if len(recorder.statements) == 0 {
		t.Fatal("the reads issued no statement to read")
	}
	// A statement that binds no predicate at all passes the count below at 0 == 0, so the
	// recorded reads are first held to binding the registry somewhere: a read that stopped
	// binding it is the regression this block exists to catch, and only this can fail on it.
	if !strings.Contains(strings.Join(recorder.statements, "\n"), refs.VisibilityMarker) {
		t.Error("the recorded reads bind no predicate of the registry's")
	}
	for _, statement := range recorder.statements {
		if strings.Count(statement, refs.VisibilityMarker) != strings.Count(statement, ".visibility") {
			t.Errorf("a read states a visibility rule the registry did not render:\n%s", statement)
		}
	}
	if !strings.Contains(searchQuery, refs.VisibilityMarker) {
		t.Error("the keyword search query binds no predicate of the registry's")
	}
	if !strings.Contains(searchQueryMeaning, refs.VisibilityMarker) {
		t.Error("the meaning search query binds no predicate of the registry's")
	}

	requireOneAuthorRule(t)
}

// requireOneAuthorRule is the author rule's three parts: the deployment's shared agent bearer
// proves no author, a write that asks to reserve a node for its author on that bearer is
// refused and the refusal says why, and the going routes ask that one rule rather than keeping
// a copy of it.
func requireOneAuthorRule(t *testing.T) {
	t.Helper()
	// The shared bearer resolves as {}: it names no session, no person and no workload, and
	// model.Actor.SameCredential reports two of them as one credential.
	bearer := model.Actor{}
	if graphstore.ProvesAuthor(bearer) {
		t.Error("the deployment's shared agent bearer proves an author")
	}
	if !bearer.SameCredential(model.Actor{}) {
		t.Error("two holders of the shared bearer are two credentials, and the refusal below has no reason")
	}
	if refs.VisibilityArg(bearer) != nil {
		t.Error("a credential that proves no author binds a name an author-private read could match")
	}
	visibility, err := graphstore.VisibilityFor(graphstore.VisibilityAuthor, bearer)
	if !errors.Is(err, graphstore.ErrAuthorNamesNobody) {
		t.Fatalf("asking for an author-private node on the shared bearer = %q, %v; want the refusal", visibility, err)
	}
	for _, reason := range []string{"names nobody", "compare as one credential"} {
		if !strings.Contains(err.Error(), reason) {
			t.Errorf("the refusal does not say why: %q does not hold %q", err, reason)
		}
	}
	if written, err := graphstore.VisibilityFor("", bearer); err != nil || written != graphstore.VisibilityProject {
		t.Errorf("a node written on the shared bearer = %q, %v; want %q", written, err, graphstore.VisibilityProject)
	}
	// The going routes call the one rule. Read from the source, because what is under test is
	// that no second copy of the rule exists to drift from it.
	source, err := os.ReadFile("going_routes.go")
	if err != nil {
		t.Fatalf("read the going routes: %v", err)
	}
	if strings.Contains(string(source), "func goingActorNamesSomebody") {
		t.Error("the going routes keep a copy of the author rule")
	}
	if calls := strings.Count(string(source), "graphstore.ProvesAuthor("); calls != 2 {
		t.Errorf("the going routes call the one author rule %d time, want both the asker's test and the run's author's", calls)
	}
}

// privateNodeAmong reports whether any edge, or the block any edge sits at, is the node.
func privateNodeAmong(edges []model.GraphEdge, node string) bool {
	for _, edge := range edges {
		if edge.Node.ID == node || (edge.Block != nil && edge.Block.ID == node) {
			return true
		}
	}
	return false
}

// privateNodeIDs are the addresses of one kind in the fixture seeded without the private node,
// which carries the same document and the same public blocks under ids of its own.
func privateNodeIDs(fixture privateNodeFixture, kind string) []string {
	switch kind {
	case "artifact":
		return []string{fixture.refKey}
	case "block":
		return []string{fixture.first, fixture.last}
	default:
		return []string{fixture.comment}
	}
}

// privateNodePeer is the address in the fixture seeded without the private node that answers to
// the address id in the one seeded with it: the two documents and the two public blocks are the
// same nodes of two databases, named by the same block ids under each database's own document.
func privateNodePeer(held, absent privateNodeFixture, id string) string {
	switch id {
	case held.refKey:
		return absent.refKey
	case held.first:
		return absent.first
	case held.last:
		return absent.last
	case held.comment:
		return absent.comment
	}
	return id
}

// dispatchReferences reads one address through GET /api/v1/references, the route a
// `dispatch://` address is read by.
func dispatchReferences(t *testing.T, handler http.Handler, reference, login string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/references?to="+url.QueryEscape(reference), nil, login)
}

// seedPrivateNode builds the fixture, with the private node or without it ever having been
// written. The two differ in that node, the two links that run through it and the comment that
// answers it, and in nothing else: what the second person reads of the one has to be what they
// read of the other.
func seedPrivateNode(t *testing.T, private bool) privateNodeFixture {
	t.Helper()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "PRIV", "A private node in public company",
		"# Spec\n\nA passage about "+privateNodePhrase+".")
	fixture := privateNodeFixture{handler: handler, database: database, issue: issue.Key, specID: issue.PrimaryArtifactID}
	if err := database.Pool.QueryRow(context.Background(),
		`select ref_key from artifacts where id = $1::uuid`, fixture.specID).Scan(&fixture.refKey); err != nil {
		t.Fatalf("read the spec's address: %v", err)
	}
	node := func(blockID string) string { return fixture.refKey + "#" + blockID }
	fixture.first, fixture.private, fixture.last = node("first"), node("private"), node("last")
	blocks := []struct{ id, excerpt string }{{"first", "The first passage."}, {"last", "The last passage."}}
	if private {
		blocks = append(blocks, struct{ id, excerpt string }{"private", "A passage about " + privateNodePhrase + "."})
	}
	for _, block := range blocks {
		seedExec(t, database, "a block of the spec", `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, $2, $3, 'paragraph', '', $4, 1, now())
		`, fixture.specID, block.id, node(block.id), block.excerpt)
	}
	if !private {
		return fixture
	}
	// The chain of three: a public block points at the private one, and it points at a second
	// public block.
	for _, link := range []struct{ from, fromNode, to string }{
		{"first", fixture.first, fixture.private},
		{"private", fixture.private, fixture.last},
	} {
		seedExec(t, database, "a link of the chain", `
			insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
			values ($1::uuid, $2, $3, 'block', $4, 'mentions', 1)
		`, fixture.specID, link.from, link.fromNode, link.to)
	}
	// The public comment that hangs under the private block, so a walk up from a visible child
	// reaches it: it answers the block, and its body cites it, which is the edge the graph
	// holds (a comment's reply to a document's block is no arm of graph_edges; its mention is).
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body, replies_to_block)
		values ($1, '{"kind":"user","id":"bob"}', 'A public answer.', $2)
		returning id::text
	`, fixture.issue, fixture.private).Scan(&fixture.comment); err != nil {
		t.Fatalf("seed the public comment: %v", err)
	}
	seedExec(t, database, "the comment's citation of the private block", `
		insert into refs (from_kind, from_id, kind, to_kind, to_id) values ('comment', $1, 'mentions', 'block', $2)
	`, fixture.comment, fixture.private)
	// The node itself, reserved for its author. No route writes one yet, so the store's own
	// writer writes it, exactly as a route that asked for `author` would
	// (graphstore.VisibilityFor answers the visibility, and refuses this one to a credential
	// that proves no author).
	author := privateNodeAuthor()
	visibility, err := graphstore.VisibilityFor(graphstore.VisibilityAuthor, author)
	if err != nil {
		t.Fatalf("reserve the node for its author: %v", err)
	}
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the private node's write: %v", err)
	}
	defer tx.Rollback(context.Background())
	if err := graphstore.Write(context.Background(), tx, []graphstore.Node{{
		Kind: "block", ID: fixture.private, Supertag: "block",
		Visibility: visibility, State: graphstore.StateLive,
		MadeAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), MadeBy: author,
		SourceEvent: graphstore.NoEvent,
	}}, nil); err != nil {
		t.Fatalf("write the private node: %v", err)
	}
	// The author's own person node, which the page read reads.
	if err := graphstore.Authored(context.Background(), tx, "block", fixture.private, author,
		time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), graphstore.NoEvent); err != nil {
		t.Fatalf("record who wrote the private node: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit the private node's write: %v", err)
	}
	return fixture
}

// recordedReads is the query surface a read runs on while the test reads the statements it
// issues.
type recordedReads struct {
	inner      refs.Queryer
	statements []string
}

func (r *recordedReads) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.statements = append(r.statements, sql)
	return r.inner.Query(ctx, sql, args...)
}

func (r *recordedReads) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.statements = append(r.statements, sql)
	return r.inner.QueryRow(ctx, sql, args...)
}
