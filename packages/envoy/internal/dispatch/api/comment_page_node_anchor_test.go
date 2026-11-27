package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestANodeAnchoredCommentPageMovesItsETagWithItsNodes is what the ETag of the comment page read
// with node_anchor=true (R31 of the issue-tree spec) must stand behind. A node_anchor answers its
// node's state, which a write of that state moves without writing a comment row or appending an
// event (writeNodeState), and its orphaned mark, which the node-anchor refresh writes on the
// comment's row. On a test server a person writes a finding of a pre-mortem lens and one comment
// on its text by a node anchor, and one comment anchored in the spec's opening paragraph by its
// document. She reads GET /api/v1/issues/{key}/comments?order=activity&node_anchor=true, and a
// second read is answered from the bytes the first kept, in the one statement its validator is
// read with, which reads the anchored nodes beside the watermark. She then writes the finding
// overtaken and the paragraph proposed through POST /api/v1/issues/{key}/nodes/{id}/state.
// Neither write moves the validator of the page read without the parameter, which stands on the
// watermark and last_seq alone; after each, the read that sends the ETag read before it answers
// 200, not 304, with the node's new state in the node_anchor, and the read that sends the new ETag
// answers 304. Last, a text of the finding that holds bound once orphans the node anchor through
// the refresh, and the read that sends the ETag read before it answers 200 with the node_anchor
// orphaned.
func TestANodeAnchoredCommentPageMovesItsETagWithItsNodes(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	// The caller's identity is resolved on the untraced pool, so a counted read counts its own
	// statements and not the sign-in lookup every request makes.
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{store: traced, identity: headerIdentity(database)})
	rig := reasoningRigOn(t, handler, database)
	key := rig.issue

	premortem := rig.invoke(rig.requirement, graphstore.LensKindPreMortem, "alice", nil)
	added := rig.post(nodePath("lens/"+premortem.ID, "findings"), map[string]any{
		"text": "A retry is bound to its key, and a replay is bound to it too.",
	}, "alice")
	if added.Code != http.StatusCreated {
		t.Fatalf("add a finding to the lens: status=%d body=%s", added.Code, added.Body.String())
	}
	finding := decodeBody[lensFinding](t, added)
	onFinding := createThreadComment(t, handler, key, map[string]any{
		"body": "What bounds it?", "anchor": map[string]any{"node": finding.ID, "quote": "bound", "occurrence": 1},
	}, "alice").ID
	spec := readDecisionDocument(t, database, key, "spec")
	paragraph := firstBlockOf(t, handler, spec.id)
	onParagraph := createThreadComment(t, handler, key, map[string]any{
		"body": "Which opening?", "anchor": map[string]any{"artifact": "spec", "block_id": paragraph, "quote": "opening paragraph"},
	}, "alice").ID

	target := "/api/v1/issues/" + key + "/comments?order=activity&node_anchor=true"
	plain := "/api/v1/issues/" + key + "/comments?order=activity"
	// requireNodeAnchor fails t unless page, as what answered it, answers the thread comment begins
	// with the node_anchor of the node of kind kind and id id, at state, orphaned or not.
	requireNodeAnchor := func(what string, page activityCommentPage, comment, kind, id, state string, orphaned bool) {
		t.Helper()
		first, held := page.first(comment)
		if !held {
			t.Fatalf("%s holds the threads %v, want the thread of the comment %s", what, page.firsts(), comment)
		}
		var anchor *model.CommentNodeAnchor
		if raw, carried := first["node_anchor"]; !carried || json.Unmarshal(raw, &anchor) != nil || anchor == nil {
			t.Fatalf("%s answered the comment %s with no node_anchor: %s", what, comment, first["node_anchor"])
		}
		if anchor.Node.Kind != kind || anchor.Node.ID != id || anchor.Node.State != state || anchor.Orphaned != orphaned {
			t.Errorf("%s answers the comment %s with the node_anchor %+v, want the node %s:%s at %s and orphaned %t",
				what, comment, *anchor, kind, id, state, orphaned)
		}
	}
	// readAfter is the page as the read holding etag, the ETag read before write, is answered: 200
	// with the page under an ETag of its own, which a read holding it is then answered 304 to.
	readAfter := func(write, etag string) (activityCommentPage, string) {
		t.Helper()
		what := "GET " + target + " after " + write
		response := listRead(t, handler, target, listReadOptions{trees: true, etag: etag})
		if response.Code == http.StatusNotModified {
			t.Fatalf("%s, sent the ETag %s read before it, answered 304, want 200 with the node_anchor that moved", what, etag)
		}
		page := readActivityPage(t, what, response)
		moved := listETagOf(t, response, what)
		if moved == etag {
			t.Fatalf("%s answered the ETag %s read before it", what, etag)
		}
		requireNotModified(t, listRead(t, handler, target, listReadOptions{trees: true, etag: moved}), moved, what+" holding its new ETag")
		return page, moved
	}

	first := listRead(t, handler, target, listReadOptions{trees: true})
	page := readActivityPage(t, "GET "+target, first)
	etag := listETagOf(t, first, "GET "+target)
	requireNodeAnchor("GET "+target, page, onFinding, refs.BlockNodeKind, finding.ID, graphstore.StateLive, false)
	requireNodeAnchor("GET "+target, page, onParagraph, refs.BlockNodeKind, spec.address(paragraph), graphstore.StateLive, false)

	// A warm read is answered from the bytes the first read kept, in one statement: the one its
	// validator is read with, over the watermark and the nodes the issue's comments are anchored on.
	answered := served.listAnswers.answered()
	counter.take()
	warm := listRead(t, handler, target, listReadOptions{trees: true, counted: true})
	counted := counter.take()
	if got := listETagOf(t, warm, "the warm read of "+target); got != etag {
		t.Fatalf("the warm read of %s answers %s, want the %s the first gave", target, got, etag)
	}
	if written := served.listAnswers.answered() - answered; written != 1 {
		t.Errorf("the cache wrote %d answers over the warm read of %s, want the one it kept", written, target)
	}
	if len(counted) != 1 || !strings.Contains(counted[0], "comment_watermarks") || !strings.Contains(counted[0], "graph_nodes") {
		t.Fatalf("the warm read of %s ran %d statements, want the one its validator is read with, over the watermark and the anchored nodes:\n%s",
			target, len(counted), strings.Join(roundTripLines(counted), "\n"))
	}
	t.Logf("a warm read of %s costs %d round trip: %s", target, len(counted), strings.Join(roundTripLines(counted), "\n"))

	// Neither write of a node's state writes a comment row or appends an event, so the page read
	// without the parameter, whose validator stands on the watermark and last_seq alone, keeps its
	// validator: only the node anchors' part of the validator can move for them.
	plainTag := commentListValidator(t, handler, plain, "GET "+plain)
	for _, write := range []struct {
		name, address, comment, id, state string
	}{
		{"the finding written overtaken", finding.Address, onFinding, finding.ID, graphstore.StateOvertaken},
		{"the opening paragraph written proposed", rig.paragraph, onParagraph, spec.address(paragraph), graphstore.StateProposed},
	} {
		rig.writeState(write.address, write.state)
		if moved := commentListValidator(t, handler, plain, "GET "+plain+" after "+write.name); moved != plainTag {
			t.Fatalf("%s moved the validator of GET %s from %s to %s; this case needs a write that moves neither the watermark nor last_seq",
				write.name, plain, plainTag, moved)
		}
		page, etag = readAfter(write.name, etag)
		requireNodeAnchor("GET "+target+" after "+write.name, page, write.comment, refs.BlockNodeKind, write.id, write.state, false)
	}

	// The refresh: a text of the finding that holds bound once orphans the comment at occurrence 1,
	// which it writes on the comment's own row.
	if orphaned := appendText(t, database, refs.BlockNodeKind, finding.ID, "A retry is bound to its key."); orphaned != 1 {
		t.Fatalf("the refresh of the finding's new text orphaned anchors of %d issues, want the one of %s", orphaned, key)
	}
	refreshed := "the refresh that orphaned the node anchor on " + finding.ID
	page, _ = readAfter(refreshed, etag)
	requireNodeAnchor("GET "+target+" after "+refreshed, page, onFinding, refs.BlockNodeKind, finding.ID, graphstore.StateOvertaken, true)
}
