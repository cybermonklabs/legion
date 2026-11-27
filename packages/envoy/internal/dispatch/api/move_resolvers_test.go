package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// plantMovedTo appends one moved_to tuple for each pair of block addresses, from the first to the
// second, as the restore and the conversion state a move, in one
// transaction it commits.
func plantMovedTo(t *testing.T, database *store.Store, pairs ...[2]string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the moves: %v", err)
	}
	defer tx.Rollback(ctx)
	tuples := make([]graphstore.Tuple, 0, len(pairs))
	for _, pair := range pairs {
		tuples = append(tuples, graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: pair[0], Kind: graphstore.TupleMovedTo,
			ToKind: refs.BlockNodeKind, ToID: pair[1],
			At: time.Now().UTC(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
	}
	if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
		t.Fatalf("plant the moves: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the moves: %v", err)
	}
}

// plantMovedToAsk appends one moved_to tuple from the block address from to the node of ask, as
// the conversion states one from the heading of a section that cites an answered ask a person
// removed from its spec, in one transaction it commits.
func plantMovedToAsk(t *testing.T, database *store.Store, from, ask string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the move to the ask: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
		FromKind: refs.BlockNodeKind, FromID: from, Kind: graphstore.TupleMovedTo,
		ToKind: graphstore.KindAsk, ToID: ask,
		At: time.Now().UTC(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}); err != nil {
		t.Fatalf("plant the move to the ask: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the move to the ask: %v", err)
	}
}

// movedChain is the moved_to pairs of a chain from from through hops more addresses of document
// (`<document>#<prefix><n>`), the last of which moved to to: hops+1 tuples in all.
func movedChain(document, prefix, from, to string, hops int) [][2]string {
	addresses := []string{from}
	for hop := 1; hop <= hops; hop++ {
		addresses = append(addresses, document+"#"+prefix+strconv.Itoa(hop))
	}
	addresses = append(addresses, to)
	pairs := make([][2]string, 0, len(addresses)-1)
	for index := 1; index < len(addresses); index++ {
		pairs = append(pairs, [2]string{addresses[index-1], addresses[index]})
	}
	return pairs
}

// closureRefKeys is the ref_key of each member of issue key's reference closure.
func closureRefKeys(t *testing.T, handler http.Handler, key string) []string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/references", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET the closure of %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	var keys []string
	for _, member := range decodeBody[model.IssueReferences](t, response).Members {
		keys = append(keys, member.Artifact.RefKey)
	}
	return keys
}

// Each of the three resolvers of a moved block follows a planted chain of moved_to
// tuples from an address no block stands at, and answers the block it reached: GET
// /api/v1/references?to= names that block with its whole current text, the text a reference of the
// block's own address reads (blockReferenceNode); GET /api/v1/block-links lists the citing block's
// link to it (refs.BlockLinks); and the closure of the issue whose comment cites the old address
// holds the document it moved into (refs.Closure). A chain of two hops resolves and so does one of
// eight, the most a resolver follows (graphstore.MovedToHops); a cycle and a chain of nine stop,
// and each resolver answers them as an address nobody holds: 404, no link, no member. A chain that
// ends at an ask node - a section's heading whose answered ask a person removed - ends there: the
// reference names the ask's node, the block link names the ask, and the closure holds the
// document the ask's row names, the spec.
func TestEachResolverFollowsAMove(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents, _ := deps.Docs.(*docs.Service)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	holdIt := ":::callout{#hold-it kind=\"note\" title=\"\"}\nHold it.\n:::"
	const askQuestion = "Which store holds the corpus?"
	askBlock := ":::ask{#the-ask urgency=\"med\" multiple=\"false\"}\n" + askQuestion + "\n:::"
	issue := createSpecIssue(t, handler, "CORE", shipIt("Ship it.")+"\n\n"+holdIt+"\n\n"+askBlock+"\n\nOther paragraph.\n")
	askID := settledDecisionBlocks(t, handler, documents, issue.Key, issue.PrimaryArtifactID, map[string]string{"the-ask": askQuestion})["the-ask"]
	askAddress := "dispatch://" + issue.Key + "/ask/" + askID
	specKey := issue.Key + "/spec"
	notes := uploadIssueDocument(t, handler, issue.Key, "notes.md", "Notes the moves start from.\n")
	notesRef := "dispatch://" + issue.Key + "/artifact/" + notes.Slug

	// The planted moves. The chain of the main citation is two hops, through an address of the
	// notes no version held; the cycle returns to where it began; the chain of nine is one hop past
	// the bound, and the chain of eight is at it.
	oldOne := notes.RefKey + "#old-one"
	cycleA, cycleB := notes.RefKey+"#cycle-a", notes.RefKey+"#cycle-b"
	nineFrom, eightFrom := notes.RefKey+"#nine-0", notes.RefKey+"#eight-0"
	shipItKey, holdItKey := specKey+"#ship_it", specKey+"#hold-it"
	pairs := [][2]string{{oldOne, notes.RefKey + "#hop-two"}, {notes.RefKey + "#hop-two", shipItKey}, {cycleA, cycleB}, {cycleB, cycleA}}
	pairs = append(pairs, movedChain(notes.RefKey, "nine-", nineFrom, holdItKey, 8)...)
	pairs = append(pairs, movedChain(notes.RefKey, "eight-", eightFrom, holdItKey, 7)...)
	if nine, eight := len(movedChain(notes.RefKey, "nine-", nineFrom, holdItKey, 8)), len(movedChain(notes.RefKey, "eight-", eightFrom, holdItKey, 7)); nine != graphstore.MovedToHops+1 || eight != graphstore.MovedToHops {
		t.Fatalf("the chains are %d and %d tuples, want %d and %d", nine, eight, graphstore.MovedToHops+1, graphstore.MovedToHops)
	}
	plantMovedTo(t, database, pairs...)
	// The chain to the ask: a block address no version held, then the ask's node.
	toAsk := notes.RefKey + "#to-ask-0"
	plantMovedTo(t, database, [2]string{toAsk, notes.RefKey + "#to-ask-1"})
	plantMovedToAsk(t, database, notes.RefKey+"#to-ask-1", askID)

	// The citations, each written by a writer of this build: a document of the issue whose blocks
	// cite each old address, and one comment on an issue of its own for each, so each closure
	// reads one citation.
	cite := func(block string) string { return notesRef + "#b-" + block }
	citing := uploadIssueDocument(t, handler, issue.Key, "citing.md",
		"Per "+cite("old-one")+".\n\nPer "+cite("cycle-a")+".\n\nPer "+cite("nine-0")+".\n\nPer "+cite("eight-0")+".\n\nPer "+cite("to-ask-0")+".\n")
	citingRef := "dispatch://" + issue.Key + "/artifact/" + citing.Slug
	commented := map[string]string{}
	for _, block := range []string{"old-one", "cycle-a", "nine-0", "eight-0", "to-ask-0"} {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": "Cites " + block, "force": true,
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the issue citing %s: status=%d body=%s", block, created.Code, created.Body.String())
		}
		other := decodeBody[model.Issue](t, created)
		createReferenceAPIComment(t, handler, other.Key, "As "+cite(block)+" says.")
		commented[block] = other.Key
	}

	// graphstore.MovedTo, the reading blockReferenceNode takes.
	for _, read := range []struct {
		from, want, ask string
		moved           bool
	}{
		{oldOne, shipItKey, "", true}, {eightFrom, holdItKey, "", true}, {shipItKey, shipItKey, "", true},
		{toAsk, askAddress, askID, true},
		{cycleA, "", "", false}, {nineFrom, "", "", false},
	} {
		reached, moved, err := graphstore.MovedTo(ctx, database.Pool, read.from)
		if err != nil || moved != read.moved || reached.Address != read.want || reached.Ask != read.ask {
			t.Fatalf("MovedTo(%s) = %+v, %t (%v), want %q (ask %q), %t", read.from, reached, moved, err, read.want, read.ask, read.moved)
		}
	}

	// GET /api/v1/references?to=<old address>.
	textAt := func(reference string) string {
		response := dispatchReferences(t, handler, reference, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/references?to=%s: status=%d body=%s", reference, response.Code, response.Body.String())
		}
		return decodeBody[model.GraphReferences](t, response).Node.Text
	}
	shipItText := textAt("dispatch://" + issue.Key + "/spec#b-ship%5Fit")
	holdItText := textAt("dispatch://" + issue.Key + "/spec#b-hold-it")
	for _, resolved := range []struct{ block, wantID, wantBlock, wantText string }{
		{"old-one", shipItKey, "ship_it", shipItText},
		{"eight-0", holdItKey, "hold-it", holdItText},
	} {
		response := dispatchReferences(t, handler, cite(resolved.block), "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/references?to=%s: status=%d body=%s, want the block its moves reach", cite(resolved.block), response.Code, response.Body.String())
		}
		node := decodeBody[model.GraphReferences](t, response).Node
		if node.Kind != refs.BlockNodeKind || node.ID != resolved.wantID || node.BlockID != resolved.wantBlock ||
			node.Text == "" || node.Text != resolved.wantText {
			t.Fatalf("GET /api/v1/references?to=%s names %s %s (block %q) with text %q, want block %s with %q",
				cite(resolved.block), node.Kind, node.ID, node.BlockID, node.Text, resolved.wantID, resolved.wantText)
		}
	}
	for _, stopped := range []string{"cycle-a", "nine-0"} {
		if response := dispatchReferences(t, handler, cite(stopped), "alice"); response.Code != http.StatusNotFound {
			t.Fatalf("GET /api/v1/references?to=%s: status=%d body=%s, want 404: its moves stop before a block", cite(stopped), response.Code, response.Body.String())
		}
	}
	askNode := dispatchReferences(t, handler, askAddress, "alice")
	if askNode.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/references?to=%s: status=%d body=%s", askAddress, askNode.Code, askNode.Body.String())
	}
	wantAsk := decodeBody[model.GraphReferences](t, askNode).Node
	if response := dispatchReferences(t, handler, cite("to-ask-0"), "alice"); response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/references?to=%s: status=%d body=%s, want the ask its moves reach", cite("to-ask-0"), response.Code, response.Body.String())
	} else if node := decodeBody[model.GraphReferences](t, response).Node; node.Kind != graphstore.KindAsk || node.ID != askID ||
		node.Text != wantAsk.Text || node.Ref != wantAsk.Ref {
		t.Fatalf("GET /api/v1/references?to=%s names %s %s %q (%s), want the ask %s as its own address reads it: %q (%s)",
			cite("to-ask-0"), node.Kind, node.ID, node.Text, node.Ref, askID, wantAsk.Text, wantAsk.Ref)
	}

	// GET /api/v1/block-links from the citing document to the spec.
	links := readBlockLinks(t, handler, citingRef, "dispatch://"+issue.Key+"/spec").Links
	reached := map[string]bool{}
	for _, link := range links {
		reached[link.To.Kind+":"+link.To.ID] = true
	}
	if len(links) != 3 || !reached["block:"+shipItKey] || !reached["block:"+holdItKey] || !reached[graphstore.KindAsk+":"+askID] {
		t.Fatalf("the block links from %s to the spec are %#v, want one to %s, one to %s and one to the ask %s, and none through the cycle or the chain of nine",
			citingRef, links, shipItKey, holdItKey, askID)
	}

	// GET /api/v1/issues/{key}/references, each issue's comment citing one old address: the chain
	// to the ask reaches the document the ask's row names, the spec.
	for block, wantSpec := range map[string]bool{"old-one": true, "eight-0": true, "to-ask-0": true, "cycle-a": false, "nine-0": false} {
		members := closureRefKeys(t, handler, commented[block])
		holds := len(members) == 1 && members[0] == specKey
		if wantSpec != holds || (!wantSpec && len(members) != 0) {
			t.Fatalf("the closure of %s, whose comment cites %s, holds %v; want the spec %s: %t", commented[block], cite(block), members, specKey, wantSpec)
		}
	}
}
