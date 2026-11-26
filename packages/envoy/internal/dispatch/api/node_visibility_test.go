package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphseedtest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The phrase the private block carries, rare enough that a search for it names what holds it and
// nothing else.
const privateNodePhrase = "wolfram tangerine"

// privateNodeFixture is the seed both halves of the test read: a node reserved for its author in
// public company. The private node is a block of a public document, so a walk down the
// document's part_of edges reaches it; a public block points at it and it points at a second
// public block, so a chain of three runs through it; and a public comment answers it, so a walk
// up from a visible child reaches it. A second private node, a comment its author wrote and
// keeps to herself, is the comment the person page and a comment's own address are read for.
// Its id starts with the first characters of the id of her public comment
// (sharedCommentPrefix), so a short id written as those characters is one both comments carry.
// The document is the spec of an issue born converted, so its blocks are rows and nodes of its
// issue's room, keyed by the room's tree artifact, and a reference to one of them at the spec's
// own address resolves to the room's node.
type privateNodeFixture struct {
	handler  http.Handler
	database *store.Store
	// recorder keeps the statements of a read made on a recording context (recordingKey): the
	// fixture's store is traced through it, so a route's statements are read as the route
	// issued them.
	recorder *statementRecorder
	issue    string
	specID   string
	refKey   string
	// first and last are the room's nodes of the two public blocks of the chain; private is the
	// node of the block in between, absent from the fixture seeded without it.
	first, private, last string
	// privateBlock is the private block's own id inside its document, which a search hit names
	// as the block it matched (model.SearchResult.BlockID), absent with it.
	privateBlock string
	// comment is the public comment that answers the private block, absent with it.
	comment string
	// privateComment is the comment its author keeps to herself, absent with the private block;
	// publicComment is a comment she wrote for everyone, in both fixtures, so her person page
	// answers in both, and the start of its id is the start of the private comment's too.
	privateComment, publicComment string
	// broker is the stub secrets broker the server asks about a DPoP-bound token, so a read can
	// be made under a session the broker verified as well as under a cookie and a token.
	broker *ciphertokentest.StubBroker
}

// author and reader are the two people of this test: the one who owns the private node and one
// who owns none of it and acts for every read below.
func privateNodeAuthor() model.Actor { return model.Actor{Kind: "user", ID: "alice"} }
func privateNodeReader() model.Actor { return model.Actor{Kind: "user", ID: "bob"} }

// The paragraphs of the fixture's spec beside its opening one: the first and the last public
// block of the chain, and the private block between them, whose text holds the phrase.
const (
	firstPassage   = "The first passage."
	privatePassage = "A passage about " + privateNodePhrase + " its author keeps to herself."
	lastPassage    = "The last passage."
)

// sharedCommentLength is how many characters of the public comment's id the private comment's id
// starts with too: eight hex digits, a hyphen and four more, a short id text.IsIDPrefix reads.
const sharedCommentLength = len("01234567-89ab")

// sharedCommentPrefix is the short id the public comment's id and the private comment's start
// with, in the fixture seeded with the private node; in the one seeded without it, the public
// comment alone carries it.
func (f privateNodeFixture) sharedCommentPrefix() string {
	return f.publicComment[:sharedCommentLength]
}

// TestNoReadAnswersANodeTheActingPersonMayNotSee: a node whose `visibility` is `author` is
// absent from every read a second person makes, and present in each of them for its author.
//
// The store's writer takes a node reserved for its author of no kind these reads answer - the
// kinds cybermonklabs/legion#3178 registers, none of which every read binds the predicate of
// (graphstore.ErrVisibilityNotAdmitted) - so the seed writes the rows such a write would store
// straight into the store (graphseedtest.SeedAuthorOnly): the rule under test is the read, not
// the write that asks for it.
//
// The bound of a node-level predicate: the keyword `document` arm of search ranks a document's
// whole body, and the private block's text is part of its document's body, so that document
// still matches a search for the phrase - for the second person as for the author. The node
// itself is absent from every read; what a document's own text says is the document's, and the
// document is public. The test asserts both halves of that where it meets them.
//
// Every read the second person makes is recorded, a route's statements as that route issued them
// through the traced store, and every one of them is held to binding the registry's predicate in
// its SQL (requirePredicatesInSQL).
//
// The reads above are made under a person's cookie. The author rule answers alike under each
// other credential that proves a person, a personal token and a session the secrets broker
// verified (requireEveryCredentialReadsAsItsPerson), and for the shared bearer, which proves
// none (requireOneAuthorRule).
func TestNoReadAnswersANodeTheActingPersonMayNotSee(t *testing.T) {
	t.Parallel()
	held := seedPrivateNode(t, true)
	// The same seed without the private node ever written, which the counts are compared
	// against: every count the second person reads has to equal the count they would read in a
	// world where that node does not exist.
	absent := seedPrivateNode(t, false)

	reader, author := privateNodeReader(), privateNodeAuthor()
	pool := held.database.Pool
	var reads []recordedRead
	// asReader reads a route as the second person, recording what it issued.
	asReader := func(name, target string) *httptest.ResponseRecorder {
		t.Helper()
		response, statements := held.read(t, target, "bob")
		reads = append(reads, recordedRead{name: name, statements: statements})
		return response
	}
	// recordReader records one direct read the second person makes.
	recordReader := func(name string, read func(ctx context.Context)) {
		t.Helper()
		reads = append(reads, recordedRead{name: name, statements: held.recorded(t, read)})
	}
	document := "dispatch://" + held.refKey
	blockRef := refs.BlockRef(document, held.privateBlock)

	// 1. The list of its kind: every link from a block of the document to a block of it. The
	// chain runs first -> private -> last, so a reader who may not see the middle sees neither
	// of its links.
	linksQuery := "/api/v1/block-links?" + url.Values{"from": {document}, "to": {document}}.Encode()
	listed := asReader("the block links of the document", linksQuery)
	if listed.Code != http.StatusOK {
		t.Fatalf("list the document's block links: status=%d body=%s", listed.Code, listed.Body.String())
	}
	links := decodeBody[model.BlockLinks](t, listed).Links
	for _, link := range links {
		if link.From.ID == held.private || link.To.ID == held.private {
			t.Errorf("the block list answers the private node: %+v", link)
		}
	}
	if len(links) != 0 {
		t.Errorf("the second person reads %d block link, want none: both run through the private node", len(links))
	}
	if authored := dispatchRequest(t, held.handler, http.MethodGet, linksQuery, nil, "alice"); authored.Code != http.StatusOK {
		t.Fatalf("list the document's block links as its author: status=%d body=%s", authored.Code, authored.Body.String())
	} else if count := len(decodeBody[model.BlockLinks](t, authored).Links); count != 2 {
		t.Errorf("the author reads %d block link, want the two the chain holds", count)
	}

	// 2. Search, with an embedder configured, so the meaning body runs and its window of the
	// private block is a candidate. The document matches for both people, because its own body
	// holds the phrase; for the second person no result is either private node and none names
	// the private block as the block it matched, and the author finds both.
	searchTarget := "/api/v1/search?q=" + url.QueryEscape(privateNodePhrase)
	searches := map[string]model.SearchResponse{}
	for _, who := range []string{"bob", "alice"} {
		var response *httptest.ResponseRecorder
		if who == "bob" {
			response = asReader("search", searchTarget)
		} else {
			response = dispatchRequest(t, held.handler, http.MethodGet, searchTarget, nil, who)
		}
		if response.Code != http.StatusOK {
			t.Fatalf("search as %s: status=%d body=%s", who, response.Code, response.Body.String())
		}
		found := decodeBody[model.SearchResponse](t, response)
		if found.Degraded != "" {
			t.Fatalf("search as %s answered degraded %q: the meaning body did not run", who, found.Degraded)
		}
		searches[who] = found
		if !slices.ContainsFunc(found.Results, func(result model.SearchResult) bool { return result.Kind == "document" }) {
			t.Errorf("search as %s names no document, and the document's own body holds the phrase", who)
		}
	}
	for _, result := range searches["bob"].Results {
		if result.ID == held.private || result.ID == held.privateComment || namesPrivateBlock(result, held) {
			block := "none"
			if result.BlockID != nil {
				block = *result.BlockID
			}
			t.Errorf("search as the second person answers a private node: the %s hit %s names block %q and links %s",
				result.Kind, result.ID, block, result.Href)
		}
	}
	if !slices.ContainsFunc(searches["alice"].Results, func(result model.SearchResult) bool {
		return result.Kind == "document" && result.ID == held.specID && namesPrivateBlock(result, held)
	}) {
		t.Errorf("the author's search names no hit of the document at the private block: %+v", searches["alice"].Results)
	}
	if !slices.ContainsFunc(searches["alice"].Results, func(result model.SearchResult) bool {
		return result.Kind == "comment" && result.ID == held.privateComment
	}) {
		t.Errorf("the author's search does not answer her private comment: %+v", searches["alice"].Results)
	}
	if never := searchCount(t, absent, searchTarget, "bob"); searches["bob"].Total != never {
		t.Errorf("the second person's search counts %d matches, and %d where the private nodes were never written",
			searches["bob"].Total, never)
	}

	// 3. The tuples of each public neighbour, in both directions, through the route a
	// `dispatch://` address is read by. The author reads the private block among the
	// neighbours' edges: the first block points at it, it points at the last, and the comment
	// cites it. Each block is read at the spec's own address of it, by its id inside the spec.
	_, firstBlock, _ := strings.Cut(held.first, "#")
	_, lastBlock, _ := strings.Cut(held.last, "#")
	neighbours := []struct {
		name, ref string
		// authorFinds is the direction the author reads the private block in.
		authorFinds string
	}{
		{"the first block", refs.BlockRef(document, firstBlock), "from"},
		{"the last block", refs.BlockRef(document, lastBlock), "to"},
		{"the public comment", "dispatch://" + held.issue + "/comment/" + held.comment, "from"},
	}
	for _, neighbour := range neighbours {
		for _, direction := range []string{"to", "from"} {
			target := "/api/v1/references?" + url.Values{direction: {neighbour.ref}}.Encode()
			response := asReader(direction+" "+neighbour.name, target)
			if response.Code != http.StatusOK {
				t.Fatalf("read %s of %s: status=%d body=%s", direction, neighbour.name, response.Code, response.Body.String())
			}
			for _, edge := range decodeBody[model.GraphReferences](t, response).Edges {
				if edge.Node.ID == held.private || (edge.Block != nil && edge.Block.ID == held.private) {
					t.Errorf("the %s edges of %s answer the private node", direction, neighbour.name)
				}
				if edge.Excerpt != nil && strings.Contains(edge.Excerpt.Text, privateNodePhrase) {
					t.Errorf("the %s edges of %s carry the private node's text in an excerpt", direction, neighbour.name)
				}
			}
			if direction != neighbour.authorFinds {
				continue
			}
			authored := dispatchRequest(t, held.handler, http.MethodGet, target, nil, "alice")
			if authored.Code != http.StatusOK {
				t.Fatalf("read %s of %s as the author: status=%d body=%s", direction, neighbour.name, authored.Code, authored.Body.String())
			}
			if !privateNodeAmong(decodeBody[model.GraphReferences](t, authored).Edges, held.private) {
				t.Errorf("the author's %s edges of %s do not answer the private node", direction, neighbour.name)
			}
		}
	}

	// 4. The backlink count on the public document and its blocks, and 5. the chain read down
	// its part_of tuples. Each count is compared with the same read in the database seeded
	// without the node; the author's count of the block the private one points at holds that
	// link.
	for _, kind := range []string{"artifact", "block", "comment"} {
		ids := map[string][]string{
			"artifact": {held.refKey},
			"block":    {held.first, held.last},
			"comment":  {held.comment},
		}[kind]
		var counted map[string]int
		recordReader("the "+kind+" backlink counts", func(ctx context.Context) {
			var err error
			if counted, err = refs.BacklinkCounts(ctx, pool, reader, kind, ids, nil); err != nil {
				t.Fatalf("count %s backlinks: %v", kind, err)
			}
		})
		never, err := refs.BacklinkCounts(t.Context(), absent.database.Pool, reader, kind, privateNodeIDs(absent, kind), nil)
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
	if authored, err := refs.BacklinkCounts(t.Context(), pool, author, "block", []string{held.last, held.private}, nil); err != nil {
		t.Fatalf("count block backlinks as the author: %v", err)
	} else if authored[held.last] == 0 || authored[held.private] == 0 {
		t.Errorf("the author counts %v, want a backlink of the last block from the private one and of the private one from the chain", authored)
	}
	var parts []model.GraphEdge
	recordReader("the walk down the document", func(ctx context.Context) {
		var err error
		if parts, err = refs.Edges(ctx, pool, reader, refs.Query{Direction: "in", Kind: "artifact", ID: held.refKey}); err != nil {
			t.Fatalf("walk down the document's part_of tuples: %v", err)
		}
	})
	for _, edge := range parts {
		if edge.Node.ID == held.private || (edge.Block != nil && edge.Block.ID == held.private) {
			t.Errorf("the walk down the document answers the private node: %+v", edge)
		}
	}
	if authored, err := refs.Edges(t.Context(), pool, author, refs.Query{Direction: "in", Kind: "artifact", ID: held.refKey}); err != nil {
		t.Fatalf("walk down the document's part_of tuples as its author: %v", err)
	} else if !privateNodeAmong(authored, held.private) {
		t.Errorf("the author's walk down the document does not answer the private node")
	}

	// 6. The walk up from the public comment: its replies_to edge names the block it answers.
	var up []model.GraphEdge
	recordReader("the walk up from the public comment", func(ctx context.Context) {
		var err error
		if up, err = refs.Edges(ctx, pool, reader, refs.Query{Direction: "out", Kind: "comment", ID: held.comment}); err != nil {
			t.Fatalf("walk up from the public comment: %v", err)
		}
	})
	if privateNodeAmong(up, held.private) {
		t.Errorf("the walk up from the public comment answers the private node")
	}
	if authored, err := refs.Edges(t.Context(), pool, author, refs.Query{Direction: "out", Kind: "comment", ID: held.comment}); err != nil {
		t.Fatalf("walk up from the public comment as the author: %v", err)
	} else if !privateNodeAmong(authored, held.private) {
		t.Errorf("the author's walk up from the public comment does not answer the private node")
	}

	// 7. The person page read, through its own route: the author's page as the second person
	// reads it holds her public comment and not her private one, and counts what it would count
	// had the private one never been written; her own read of it holds both.
	recordReader("the author's person node", func(ctx context.Context) {
		if _, found, err := refs.Node(ctx, pool, reader, graphstore.KindPerson, "alice", true); err != nil {
			t.Fatalf("read the person node: %v", err)
		} else if !found {
			t.Fatalf("the store holds no person node for the author, so the page read proves nothing")
		}
	})
	const pageTarget = "/api/v1/people/alice/nodes"
	page := asReader("the author's person page", pageTarget)
	if page.Code != http.StatusOK {
		t.Fatalf("read the author's person page as the second person: status=%d body=%s", page.Code, page.Body.String())
	}
	seen := decodeBody[personNodes](t, page)
	if personNodeOn(seen, held.privateComment) {
		t.Errorf("the person page answers the private comment to the second person: %+v", seen.Rows)
	}
	if !personNodeOn(seen, held.publicComment) {
		t.Errorf("the person page answers the second person %+v, and not the author's public comment, so the read proves nothing", seen.Rows)
	}
	if never := decodeBody[personNodes](t, dispatchRequest(t, absent.handler, http.MethodGet, pageTarget, nil, "bob")); seen.Total != never.Total {
		t.Errorf("the second person's person page counts %d, and %d where the private nodes were never written", seen.Total, never.Total)
	}
	if own := dispatchRequest(t, held.handler, http.MethodGet, pageTarget, nil, "alice"); own.Code != http.StatusOK {
		t.Fatalf("read the author's own person page: status=%d body=%s", own.Code, own.Body.String())
	} else if mine := decodeBody[personNodes](t, own); !personNodeOn(mine, held.privateComment) || !personNodeOn(mine, held.publicComment) {
		t.Errorf("the author's own person page is %+v, want both her comments", mine.Rows)
	}

	// 8. The render of an earlier version, through the as-of index, and 9. the node's own
	// address, on a block and on a comment. Each answers what an address nobody holds answers,
	// byte for byte once the address asked for is substituted: the two refusals differ in the
	// node they name and in nothing else. The version the pin names recorded the private
	// block's text (document_block_texts), so a pinned read that took the hidden block for a
	// removed one would answer that text where the address nobody holds answers a 404.
	nobodyComment := "4c6e7b00-0000-4000-8000-0000000000ff"
	for _, address := range []struct {
		name, private, nobody, privateID, nobodyID string
	}{
		{"the private block's address", blockRef, refs.BlockRef(document, "nosuchblock"), held.privateBlock, "nosuchblock"},
		{"the private block's address at version 1", blockRef + "@v1", refs.BlockRef(document, "nosuchblock") + "@v1", held.privateBlock, "nosuchblock"},
		{"the private comment's address", "dispatch://" + held.issue + "/comment/" + held.privateComment,
			"dispatch://" + held.issue + "/comment/" + nobodyComment, held.privateComment, nobodyComment},
	} {
		private := asReader(address.name, "/api/v1/references?to="+url.QueryEscape(address.private))
		nobody := dispatchReferences(t, held.handler, address.nobody, "bob")
		if private.Code != nobody.Code {
			t.Errorf("%s answers %d and an address nobody holds answers %d", address.name, private.Code, nobody.Code)
		}
		if nobody.Code < 400 {
			t.Errorf("an address nobody holds answers %d for %s, so the comparison proves nothing", nobody.Code, address.name)
		}
		want := bytes.ReplaceAll(nobody.Body.Bytes(), []byte(address.nobodyID), []byte(address.privateID))
		if !bytes.Equal(private.Body.Bytes(), want) {
			t.Errorf("%s answers\n%s\nand an address nobody holds answers\n%s", address.name, private.Body.Bytes(), want)
		}
		authored := dispatchReferences(t, held.handler, address.private, "alice")
		if authored.Code != http.StatusOK {
			t.Errorf("the author's read of %s: status=%d body=%s", address.name, authored.Code, authored.Body.String())
		}
	}
	if pinned := dispatchReferences(t, held.handler, blockRef+"@v1", "alice"); !strings.Contains(pinned.Body.String(), privateNodePhrase) {
		t.Errorf("the author's read of the private block at version 1 does not carry the text that version recorded: %s", pinned.Body.String())
	}

	// 10. The batched read of what references point at. To the second person a reference to
	// either private node is gone, as a reference nothing addresses is, and a short id that the
	// private comment alone carries names nothing. A short id that it shares with the public
	// comment names the public comment alone, and is not ambiguous: the flag would tell the
	// second person that a node they may not see carries that prefix, though no name of it is
	// answered. That answer is the one the second person reads where the private nodes were
	// never written, byte for byte once the short id asked for is substituted. The author reads
	// both private nodes named, her private comment's own short id resolved to it, and the
	// shared short id ambiguous between her two comments.
	commentRef := func(fixture privateNodeFixture, id string) string {
		return "dispatch://" + fixture.issue + "/comment/" + id
	}
	shared := commentRef(held, held.sharedCommentPrefix())
	// The private comment's id through its fourth group, which carries ten characters of the
	// uuid drawn for it past the shared short id, so the public comment's id does not start so.
	privateOnly := commentRef(held, held.privateComment[:len("01234567-89ab-cdef-0123")])
	targetsQuery := referenceTargetsOf(blockRef, commentRef(held, held.privateComment), privateOnly, shared)
	batched := asReader("the batched read of the private nodes' references", targetsQuery)
	if batched.Code != http.StatusOK {
		t.Fatalf("read the private nodes' references in one batch: status=%d body=%s", batched.Code, batched.Body.String())
	}
	members := decodeBody[model.ReferenceTargets](t, batched).Targets
	if len(members) != 4 {
		t.Fatalf("the batched read answers %d member for 4 references: %+v", len(members), members)
	}
	for _, member := range members[:3] {
		if !member.Gone || member.Name != "" || member.Ambiguous {
			t.Errorf("the batched read answers the second person %s as %+v; want it gone with no name, as a reference nothing addresses", member.Ref, member)
		}
	}
	if public := members[3]; public.Ambiguous {
		t.Errorf("the batched read answers the second person the short id %s, which the private comment shares with the public one, as ambiguous: the flag tells them a node they may not see carries it", public.Ref)
	} else if public.Gone || public.Name != "A remark its author wrote for everyone." {
		t.Errorf("the batched read answers the second person the short id %s as %+v; want the public comment's name alone", public.Ref, public)
	}
	alone := asReader("the batched read of the shared short id", referenceTargetsOf(shared))
	absentShared := commentRef(absent, absent.sharedCommentPrefix())
	never := dispatchRequest(t, absent.handler, http.MethodGet, referenceTargetsOf(absentShared), nil, "bob")
	if want := bytes.ReplaceAll(never.Body.Bytes(), []byte(absentShared), []byte(shared)); alone.Code != never.Code || !bytes.Equal(alone.Body.Bytes(), want) {
		t.Errorf("the batched read answers the second person the shared short id as %d\n%s\nand where the private nodes were never written as %d\n%s",
			alone.Code, alone.Body.Bytes(), never.Code, want)
	}
	authoredTargets := dispatchRequest(t, held.handler, http.MethodGet, targetsQuery, nil, "alice")
	if authoredTargets.Code != http.StatusOK {
		t.Fatalf("read the private nodes' references in one batch as their author: status=%d body=%s", authoredTargets.Code, authoredTargets.Body.String())
	}
	mine := decodeBody[model.ReferenceTargets](t, authoredTargets).Targets
	if len(mine) != 4 {
		t.Fatalf("the batched read answers the author %d member for 4 references: %+v", len(mine), mine)
	}
	// The private block is named by its own words, the passage the fixture writes into it.
	privateRemark := "A private remark about " + privateNodePhrase + "."
	for index, want := range []string{privatePassage, privateRemark, privateRemark} {
		if member := mine[index]; member.Gone || member.Name != want {
			t.Errorf("the batched read answers the author %s as %+v; want it named %q", member.Ref, member, want)
		}
	}
	if member := mine[3]; !member.Ambiguous || !member.Gone {
		t.Errorf("the batched read answers the author the short id %s, which both her comments carry, as %+v; want it ambiguous", member.Ref, member)
	}

	// The predicate sits in the SQL of each read and not in Go.
	requirePredicatesInSQL(t, reads)
	if !strings.Contains(searchQuery, refs.VisibilityMarker) {
		t.Error("the keyword search query binds no predicate of the registry's")
	}
	if !strings.Contains(searchQueryMeaning, refs.VisibilityMarker) {
		t.Error("the meaning search query binds no predicate of the registry's")
	}

	requireOneAuthorRule(t)
	requireEveryCredentialReadsAsItsPerson(t, held)
}

// TestAWritersShortIDResolvesOnlyAmongTheNodesTheWriterMaySee: a short id a comment writes
// resolves among the nodes its writer may see, and no other (refs.NewReader, R9, R19). The
// fixture's author keeps a comment to herself whose id starts as her public comment's does
// (sharedCommentPrefix), so that short id is one two comments carry. The second person, who may
// not see the private comment, writes a comment naming that short id: its stored reference is the
// public comment's whole id, with no ambiguity, and the short id is not left as the text wrote
// it, which would tell the second person that a comment they may not see carries it. The author
// may see both comments, so the same short id written by her is ambiguous, and her comment stores
// it as she wrote it.
func TestAWritersShortIDResolvesOnlyAmongTheNodesTheWriterMaySee(t *testing.T) {
	t.Parallel()
	fixture := seedPrivateNode(t, true)
	shared := fixture.sharedCommentPrefix()
	body := "See dispatch://" + fixture.issue + "/comment/" + shared + "."
	// stored is the comment references the comment login writes with body stores, read from
	// refs, where a comment write stores what its text cites.
	stored := func(login string) []string {
		t.Helper()
		comment := createThreadComment(t, fixture.handler, fixture.issue, map[string]any{"body": body}, login)
		rows, err := fixture.database.Pool.Query(context.Background(), `
			select to_id from refs where from_kind = 'comment' and from_id = $1 and to_kind = 'comment' order by to_id
		`, comment.ID)
		if err != nil {
			t.Fatalf("read the references of %s's comment: %v", login, err)
		}
		cited, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the references of %s's comment: %v", login, err)
		}
		return cited
	}
	switch written := stored(privateNodeReader().ID); {
	case slices.Equal(written, []string{shared}):
		t.Errorf("the short id %s the second person wrote stays unresolved as ambiguous, want the public node %s", shared, fixture.publicComment)
	case !slices.Equal(written, []string{fixture.publicComment}):
		t.Errorf("the second person's comment naming the short id %s stores the comment references %v, want the public node %s alone",
			shared, written, fixture.publicComment)
	}
	if authored := stored(privateNodeAuthor().ID); !slices.Equal(authored, []string{shared}) {
		t.Errorf("the author's comment naming the short id %s stores the comment references %v, want the short id as she wrote it: she may see both comments that carry it",
			shared, authored)
	}
}

// TestAnAnswersShortIDResolvesAmongTheNodesTheAnswererMaySee: an ask's parts are texts of two
// writers, and each part's short ids resolve for the one who wrote it (R9): the question for the
// ask's author, the answer for its answerer, the maker its part records (ask_blocks.made_by). The
// fixture's author, who may see both her comments that carry the shared short id
// (sharedCommentPrefix), asks a question naming it, and the second person, who may not see the
// private one, answers naming it too. The answer part's stored link is the public comment's whole
// id, not the short id as the answer wrote it, which would tell the answerer that a comment they
// may not see carries it; the question part's stays the short id the author wrote, ambiguous to
// her.
func TestAnAnswersShortIDResolvesAmongTheNodesTheAnswererMaySee(t *testing.T) {
	t.Parallel()
	fixture := seedPrivateNode(t, true)
	shared := fixture.sharedCommentPrefix()
	reference := "dispatch://" + fixture.issue + "/comment/" + shared
	created := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issue+"/asks", map[string]any{
		"question": "Which remark is meant by " + reference + "?",
	}, privateNodeAuthor().ID)
	if created.Code != http.StatusCreated {
		t.Fatalf("the author's ask: status=%d body=%s", created.Code, created.Body.String())
	}
	askID := decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
	answered := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"text": "The one at " + reference + ".",
	}, privateNodeReader().ID)
	if answered.Code != http.StatusOK {
		t.Fatalf("the second person's answer: status=%d body=%s", answered.Code, answered.Body.String())
	}
	// linked is the comment each current part of the ask links, by the part's type, read from
	// ask_block_refs, where an ask's parts store what their texts cite.
	rows, err := fixture.database.Pool.Query(context.Background(), `
		select b.type, r.to_id
		from ask_block_refs r
		join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
		where r.ask_id = $1::uuid and r.to_kind = 'comment' and b.position is not null
		order by b.type, r.to_id
	`, askID)
	if err != nil {
		t.Fatalf("read the links of the ask's parts: %v", err)
	}
	linked := map[string][]string{}
	for rows.Next() {
		var part, to string
		if err := rows.Scan(&part, &to); err != nil {
			rows.Close()
			t.Fatalf("scan a link of the ask's parts: %v", err)
		}
		linked[part] = append(linked[part], to)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the links of the ask's parts: %v", err)
	}
	switch answer := linked[refs.AskAnswerPart]; {
	case slices.Equal(answer, []string{shared}):
		t.Errorf("the short id %s the answerer wrote stays unresolved as ambiguous, want the public node %s", shared, fixture.publicComment)
	case !slices.Equal(answer, []string{fixture.publicComment}):
		t.Errorf("the answer part naming the short id %s links the comments %v, want the public node %s alone", shared, answer, fixture.publicComment)
	}
	if question := linked[refs.AskQuestionPart]; !slices.Equal(question, []string{shared}) {
		t.Errorf("the question part naming the short id %s links the comments %v, want the short id as its author wrote it: she may see both comments that carry it",
			shared, question)
	}
}

// recordedRead is one read the second person made, with every statement it issued.
type recordedRead struct {
	name       string
	statements []recordedStatement
}

// storeTableRead matches a statement reading one of the node and tuple store's own tables: the
// nodes, the tuples and the edge view over them, which every visibility predicate reads too.
var storeTableRead = regexp.MustCompile(`(?i)\b(?:from|join)\s+(?:graph_nodes|graph_tuples|graph_edges)\b`)

// requirePredicatesInSQL holds every read the second person made to the predicate sitting in its
// SQL, statement by statement:
//
//   - each read issues at least one statement that binds the registry's predicate, so a read
//     whose rule moved out of its SQL - a filter in Go over what a statement answered - fails
//     here by name;
//   - each statement that reads the store's own tables binds the predicate at least once, so a
//     statement that stopped binding it fails here, whatever else the read binds; a count of
//     the markers against the rules a statement states would pass such a statement at 0 == 0;
//   - every visibility rule a statement states is one the registry rendered
//     (refs.VisibilityMarker opens each of them), so no read carries a copy of the rule; the
//     column a loader selects to answer a node's stored state and visibility reads
//     `visibility` too and tests nothing, and refs.StoredNodeMarker opens each such read.
func requirePredicatesInSQL(t *testing.T, reads []recordedRead) {
	t.Helper()
	if len(reads) == 0 {
		t.Fatal("no read was recorded, so nothing was checked")
	}
	for _, read := range reads {
		if len(read.statements) == 0 {
			t.Errorf("%s issued no statement through the traced store, so nothing of it was read", read.name)
			continue
		}
		bound := 0
		for _, statement := range read.statements {
			markers := strings.Count(statement.SQL, refs.VisibilityMarker)
			if markers > 0 {
				bound++
			}
			if storeTableRead.MatchString(statement.SQL) && markers == 0 {
				t.Errorf("%s reads the store's tables in a statement that binds no predicate of the registry's:\n%s",
					read.name, statement.SQL)
			}
			if rules := strings.Count(statement.SQL, ".visibility") - strings.Count(statement.SQL, refs.StoredNodeMarker); markers != rules {
				t.Errorf("%s states %d visibility rule and the registry rendered %d of them:\n%s",
					read.name, rules, markers, statement.SQL)
			}
		}
		if bound == 0 {
			t.Errorf("%s issued %d statement and none of them binds the registry's predicate", read.name, len(read.statements))
		}
	}
}

// read is one route read made as login on a recording context, with every statement it issued
// through the fixture's traced store.
func (f privateNodeFixture) read(t *testing.T, target, login string) (*httptest.ResponseRecorder, []recordedStatement) {
	t.Helper()
	response := httptest.NewRecorder()
	statements := f.recorder.record(func() {
		ctx := context.WithValue(t.Context(), recordingKey{}, true)
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		request.Header.Set("X-Dispatch-User", login)
		f.handler.ServeHTTP(response, request)
	})
	return response, statements
}

// recorded runs one direct read on a recording context and answers the statements it issued.
func (f privateNodeFixture) recorded(t *testing.T, read func(ctx context.Context)) []recordedStatement {
	t.Helper()
	return f.recorder.record(func() {
		read(context.WithValue(t.Context(), recordingKey{}, true))
	})
}

// searchCount is how many matches a search counts for login in one fixture.
func searchCount(t *testing.T, fixture privateNodeFixture, target, login string) int {
	t.Helper()
	response := dispatchRequest(t, fixture.handler, http.MethodGet, target, nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("search as %s: status=%d body=%s", login, response.Code, response.Body.String())
	}
	return decodeBody[model.SearchResponse](t, response).Total
}

// namesPrivateBlock reports whether a search hit names the private block as the block it matched,
// by its id in the document or by its node address.
func namesPrivateBlock(result model.SearchResult, fixture privateNodeFixture) bool {
	return result.BlockID != nil && (*result.BlockID == fixture.privateBlock || *result.BlockID == fixture.private)
}

// personNodeOn reports whether a person page holds a row of the node id.
func personNodeOn(page personNodes, id string) bool {
	return slices.ContainsFunc(page.Rows, func(row personNode) bool { return row.ID == id })
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
	// The going routes call the one rule for the asker, and hold a run's owner to the
	// credential that asked it (graphstore.NamesCredential beside model.Actor.SameCredential),
	// which is no second copy of the rule: an interaction compares the whole credential. Read
	// from the source, because what is under test is that no second copy of the rule exists to
	// drift from it.
	source, err := os.ReadFile("going_routes.go")
	if err != nil {
		t.Fatalf("read the going routes: %v", err)
	}
	if strings.Contains(string(source), "func goingActorNamesSomebody") {
		t.Error("the going routes keep a copy of the author rule")
	}
	if calls := strings.Count(string(source), "graphstore.ProvesAuthor("); calls != 1 {
		t.Errorf("the going routes call the one author rule %d time(s), want once, the asker's test", calls)
	}
	if calls := strings.Count(string(source), "graphstore.NamesCredential("); calls != 1 {
		t.Errorf("the going routes hold a run's author to a named credential %d time(s), want once, the run owner's test", calls)
	}
}

// requireEveryCredentialReadsAsItsPerson holds the author rule to each credential that proves a
// person, beside the cookie every read above is made under: a personal token, which proves its
// owner, and a session the secrets broker verified, which proves the person the broker names.
// Under each, a credential proving the author reads the private block's address and the private
// comment's, and one proving the second person reads each as an address nobody holds, byte for
// byte once the address asked for is substituted.
func requireEveryCredentialReadsAsItsPerson(t *testing.T, fixture privateNodeFixture) {
	t.Helper()
	document := "dispatch://" + fixture.refKey
	nobodyComment := "4c6e7b00-0000-4000-8000-0000000000fe"
	addresses := []struct{ name, private, nobody, privateID, nobodyID string }{
		{"the private block's address", refs.BlockRef(document, fixture.privateBlock), refs.BlockRef(document, "nosuchblock"), fixture.privateBlock, "nosuchblock"},
		{"the private comment's address", "dispatch://" + fixture.issue + "/comment/" + fixture.privateComment,
			"dispatch://" + fixture.issue + "/comment/" + nobodyComment, fixture.privateComment, nobodyComment},
	}
	caller := newBrokerCaller(t)
	// A session the broker verified for a person, scoped to the fixture's project, as the
	// broker's exchange answers for a token it minted to act as that session.
	verified := func(person, session string) string {
		token := "a-token-proving-" + person + "s-session"
		answer := liveNamedToken(caller.jkt)
		answer.Person, answer.Session = person, session
		answer.Projects = []string{strings.SplitN(fixture.issue, "-", 2)[0]}
		fixture.broker.Says(token, answer)
		return token
	}
	type credential struct {
		name string
		read func(target string) *httptest.ResponseRecorder
	}
	underToken := func(name, token string) credential {
		return credential{name, func(target string) *httptest.ResponseRecorder {
			return bearerTokenRequest(t, fixture.handler, http.MethodGet, target, token, nil)
		}}
	}
	underSession := func(name, token string) credential {
		return credential{name, func(target string) *httptest.ResponseRecorder {
			response := httptest.NewRecorder()
			fixture.handler.ServeHTTP(response, caller.presenting(t, http.MethodGet, target, token, nil))
			return response
		}}
	}
	for _, pair := range []struct{ author, reader credential }{
		{
			underToken("the author's personal token", personalTokenOwnedBy(t, fixture.database, privateNodeAuthor().ID, model.Scopes...)),
			underToken("the second person's personal token", personalTokenOwnedBy(t, fixture.database, privateNodeReader().ID, model.Scopes...)),
		},
		{
			underSession("a session the broker verified as the author's", verified(privateNodeAuthor().ID, "ses_a1b2c3d4e5f6")),
			underSession("a session the broker verified as the second person's", verified(privateNodeReader().ID, "ses_f6e5d4c3b2a1")),
		},
	} {
		for _, address := range addresses {
			target := "/api/v1/references?to=" + url.QueryEscape(address.private)
			if authored := pair.author.read(target); authored.Code != http.StatusOK {
				t.Errorf("%s reads %s: status=%d body=%s, want the node", pair.author.name, address.name, authored.Code, authored.Body.String())
			} else if !strings.Contains(authored.Body.String(), address.privateID) {
				t.Errorf("%s reads %s and its answer does not name it: %s", pair.author.name, address.name, authored.Body.String())
			}
			private := pair.reader.read(target)
			nobody := pair.reader.read("/api/v1/references?to=" + url.QueryEscape(address.nobody))
			if nobody.Code < 400 {
				t.Errorf("an address nobody holds answers %s %d for %s, so the comparison proves nothing", pair.reader.name, nobody.Code, address.name)
			}
			want := bytes.ReplaceAll(nobody.Body.Bytes(), []byte(address.nobodyID), []byte(address.privateID))
			if private.Code != nobody.Code || !bytes.Equal(private.Body.Bytes(), want) {
				t.Errorf("%s reads %s as %d\n%s\nand an address nobody holds as %d\n%s",
					pair.reader.name, address.name, private.Code, private.Body.Bytes(), nobody.Code, want)
			}
		}
		t.Logf("%s reads the private nodes and %s reads each as an address nobody holds", pair.author.name, pair.reader.name)
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
// same nodes of two databases, each block named by the id its issue's create gave it, under each
// database's own room.
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
// `dispatch://` address is read by: the edges into it (referencesAt's `to`).
func dispatchReferences(t *testing.T, handler http.Handler, reference, login string) *httptest.ResponseRecorder {
	t.Helper()
	return referencesAt(t, handler, "to", reference, login)
}

// seedPrivateNode builds the fixture, with the private nodes or without them ever having been
// written. The two differ in those nodes - the private paragraph the spec is created with, whose
// text its version 1 records, and the private comment - the two links that run through the
// block and the comment that answers it, and in nothing else: what the second person reads of
// the one has to be what they read of the other.
//
// The spec's paragraphs are written by the issue's create, which indexes them in its room: their
// rows, their nodes, their memberships of the spec and their texts at version 1 are the block
// index's. The links of the chain are rows naming no node, keyed by the room's tree artifact, as
// the block index of the release before keyed a document's own.
//
// The server runs with an embedder configured, and the document's blocks are windowed and
// embedded, the private block's window among them, so a search runs its meaning body over a
// window that covers the private block.
func seedPrivateNode(t *testing.T, private bool) privateNodeFixture {
	t.Helper()
	ctx := context.Background()
	embedder := &fakeEmbedder{vectors: map[string][]float32{privateNodePhrase: queryVector()}}
	recorder := &statementRecorder{}
	database := storetest.TracedStore(t, storetest.Open(t), recorder)
	// The broker-token rig's server is the package's one test server with the broker check
	// wired to a stub broker, so a read can present a session the broker verified.
	rig := newBrokerTokenRigOn(t, testServerOptions{store: database, embedder: embedder})
	handler := rig.handler
	passages := []string{firstPassage, lastPassage}
	if private {
		passages = []string{firstPassage, privatePassage, lastPassage}
	}
	issue := createInteractionIssue(t, handler, "PRIV", "A private node in public company",
		"# Spec\n\nA passage about "+privateNodePhrase+".\n\n"+strings.Join(passages, "\n\n")+"\n")
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	if spec.room == spec.id {
		t.Fatalf("the spec of %s is its own room, want it converted into its issue's room", issue.Key)
	}
	fixture := privateNodeFixture{handler: handler, database: database, recorder: recorder, issue: issue.Key,
		specID: issue.PrimaryArtifactID, refKey: spec.refKey, broker: rig.broker}
	fixture.publicComment = createThreadComment(t, handler, fixture.issue,
		map[string]any{"body": "A remark its author wrote for everyone."}, "alice").ID
	ids := blockIDsByText(t, handler, fixture.specID)
	blockOf := func(passage string) string {
		t.Helper()
		id := ids[passage]
		if id == "" {
			t.Fatalf("the spec of %s lists no block holding %q", issue.Key, passage)
		}
		return id
	}
	fixture.first, fixture.last = spec.address(blockOf(firstPassage)), spec.address(blockOf(lastPassage))
	var markdown strings.Builder
	var windowed []model.ArtifactBlock
	for _, passage := range passages {
		if markdown.Len() > 0 {
			markdown.WriteString("\n\n")
		}
		windowed = append(windowed, model.ArtifactBlock{
			ID: blockOf(passage), Type: "paragraph", From: markdown.Len(), To: markdown.Len() + len(passage),
		})
		markdown.WriteString(passage)
	}
	// The windows of those blocks, written by the one writer of a document's windows under the
	// owner key it reads, the room's tree artifact (documentWindowOwner), and embedded near the
	// query: the private block's window is the meaning body's candidate.
	windows, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the document's windows: %v", err)
	}
	defer windows.Rollback(ctx)
	if err := refs.WriteDocumentWindows(ctx, windows, fixture.specID, markdown.String()+"\n", windowed); err != nil {
		t.Fatalf("write the document's windows: %v", err)
	}
	if err := windows.Commit(ctx); err != nil {
		t.Fatalf("commit the document's windows: %v", err)
	}
	embedWindowsAs(t, database, embedder, "document_block", documentWindowOwner(t, database, fixture.specID), angledVector(0.9, 2))
	processAllPending(t, database, embedder)
	if !private {
		return fixture
	}
	fixture.privateBlock = blockOf(privatePassage)
	fixture.private = spec.address(fixture.privateBlock)
	// The chain of three: a public block points at the private one, and it points at a second
	// public block.
	for _, link := range []struct{ from, fromNode, to string }{
		{blockOf(firstPassage), fixture.first, fixture.private},
		{fixture.privateBlock, fixture.private, fixture.last},
	} {
		seedExec(t, database, "a link of the chain", `
			insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
			values ($1::uuid, $2, $3, 'block', $4, 'mentions', 1)
		`, spec.room, link.from, link.fromNode, link.to)
	}
	// The public comment that hangs under the private block, so a walk up from a visible child
	// reaches it: it answers the block, and its body cites it, which is the edge the graph
	// holds (a comment's reply to a document's block is no arm of graph_edges; its mention is).
	// The citation keeps the address a reference writes, the spec's own, which resolves to the
	// room's node.
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body, replies_to_block)
		values ($1, '{"kind":"user","id":"bob"}', 'A public answer.', $2)
		returning id::text
	`, fixture.issue, fixture.private).Scan(&fixture.comment); err != nil {
		t.Fatalf("seed the public comment: %v", err)
	}
	seedExec(t, database, "the comment's citation of the private block", `
		insert into refs (from_kind, from_id, kind, to_kind, to_id) values ('comment', $1, 'mentions', 'block', $2)
	`, fixture.comment, spec.pointed(fixture.privateBlock))
	// The nodes themselves, reserved for their author. The store's writer takes a node reserved
	// for its author of no kind these reads answer (graphstore.ErrVisibilityNotAdmitted), so the
	// rows a route that asked for `author` would store are seeded straight into the store
	// (graphseedtest.SeedAuthorOnly).
	author := privateNodeAuthor()
	// The private comment's id starts as the public comment's does (sharedCommentPrefix), so the
	// two comments carry that short id, and goes on as a uuid drawn for it.
	var drawn string
	if err := database.Pool.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&drawn); err != nil {
		t.Fatalf("draw the private comment's id: %v", err)
	}
	fixture.privateComment = fixture.sharedCommentPrefix() + drawn[sharedCommentLength:]
	at := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the private nodes' write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphseedtest.SeedAuthorOnly(ctx, tx, graphstore.Node{
		Kind: "block", ID: fixture.private, Supertag: "block", State: graphstore.StateLive,
		MadeAt: at, MadeBy: author, SourceEvent: graphstore.NoEvent,
	}, graphstore.Node{
		Kind: graphstore.KindComment, ID: fixture.privateComment, Supertag: graphstore.KindComment, State: graphstore.StateLive,
		MadeAt: at, MadeBy: author, SourceEvent: graphstore.NoEvent,
	}); err != nil {
		t.Fatalf("write the private nodes: %v", err)
	}
	// The private comment's row, the projection of its node, written after the node as the
	// comment write writes it.
	if _, err := tx.Exec(ctx, `
		insert into comments (id, issue_key, author, body, node_kind, node_address)
		values ($1::uuid, $2, '{"kind":"user","id":"alice"}', $3, $4, $1)
	`, fixture.privateComment, fixture.issue, "A private remark about "+privateNodePhrase+".", graphstore.KindComment); err != nil {
		t.Fatalf("write the private comment's row: %v", err)
	}
	// Who wrote each: the author's own person node, which the person page reads.
	for _, written := range []struct{ kind, id string }{{"block", fixture.private}, {graphstore.KindComment, fixture.privateComment}} {
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			return graphstore.QueueAuthored(b, written.kind, written.id, author, at, graphstore.NoEvent)
		}); err != nil {
			t.Fatalf("record who wrote the private %s: %v", written.kind, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the private nodes' write: %v", err)
	}
	return fixture
}

// A meaning hit on a window of a document of an issue room names no block the reader may not
// see: the covered block is held to the predicate of the node its address resolves to
// (refs.ResolvedBlockSQL), the room's node `<tree ref_key>#<block>`, where the room keeps the
// block. The spec, its three paragraphs and their nodes are written by the issue's create and
// indexed in its room. The store's writers take no block reserved for its author
// (graphstore.ErrVisibilityNotAdmitted), so the node of the paragraph holding the query's words
// is reserved for its author, alice, as such a write would store it (graphseedtest.SeedAuthorOnly).
// Alice's hit names that paragraph; bob's names another of the window.
//
// The control is the predicate read at the spec's own address, which names no node of the store:
// the paragraph reads as visible to bob, and his hit names it.
func TestMeaningSearchNamesNoBlockOfAnIssueRoomItsReaderMayNotSee(t *testing.T) {
	const query = "theodolite recalibration"
	const best = "The theodolite needs recalibration before the survey."
	ctx := context.Background()
	embedder := &fakeEmbedder{vectors: map[string][]float32{query: queryVector()}}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "RPRIV", "Survey gear",
		"Alpha notes on the mounting bracket.\n\nBeta notes on the tripod legs.\n\n"+best+"\n")
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	if spec.room == spec.id {
		t.Fatalf("the spec of %s is its own room, want it converted into its issue's room", issue.Key)
	}
	private := blockIDsByText(t, handler, issue.PrimaryArtifactID)[best]
	if private == "" {
		t.Fatalf("the spec of %s lists no block holding %q", issue.Key, best)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the reservation of the paragraph's node: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphseedtest.SeedAuthorOnly(ctx, tx, graphstore.Node{
		Kind: refs.BlockNodeKind, ID: spec.address(private), Supertag: refs.BlockNodeKind, State: graphstore.StateLive,
		MadeAt: time.Now().UTC(), MadeBy: privateNodeAuthor(), SourceEvent: graphstore.NoEvent,
	}); err != nil {
		t.Fatalf("reserve the paragraph's node for its author: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the reservation of the paragraph's node: %v", err)
	}
	windowOwedOwners(t, deps)
	// The room's tree artifact keys the spec's windows (documentWindowOwner), and the spec is the
	// one document of the room holding a block, so every window under the tree is its.
	owner := documentWindowOwner(t, database, issue.PrimaryArtifactID)
	if windows := windowTexts(t, database, "document_block", owner); len(windows) != 1 {
		t.Fatalf("the spec's windows = %v, want one window over its three paragraphs", windows)
	}
	embedWindowsAs(t, database, embedder, "document_block", owner, angledVector(0.9, 2))
	processAllPending(t, database, embedder)

	hitOf := func(login string) string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/search?q="+url.QueryEscape(query), nil, login)
		if response.Code != http.StatusOK {
			t.Fatalf("%s's search: status=%d body=%s", login, response.Code, response.Body.String())
		}
		hit := searchHit(t, decodeBody[model.SearchResponse](t, response), "document", issue.PrimaryArtifactID)
		if hit.BlockID == nil {
			return ""
		}
		return *hit.BlockID
	}
	if named := hitOf(privateNodeAuthor().ID); named != private {
		t.Fatalf("the author's hit names block %q, want %q, her paragraph holding the query's words", named, private)
	}
	if named := hitOf(privateNodeReader().ID); named == "" || named == private {
		t.Fatalf("the second person's hit names block %q, want another paragraph of the window than %q, which he may not see",
			named, private)
	}
}
