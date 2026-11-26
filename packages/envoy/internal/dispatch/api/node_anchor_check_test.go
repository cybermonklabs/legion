package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphseedtest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// nodeAnchorWireVariable names the file c-node-anchor's run hands the test: each answer of the
// comments reads and each comment.created payload the test reads is written there, one JSON value a
// line, for packages/contracts/scripts/wire-against-main.ts to parse with main's strict schemas.
const nodeAnchorWireVariable = "NODE_ANCHOR_WIRE"

// nodeAnchorWireLine is one line of that file: the schema of main's packages/contracts the value is
// parsed with, what the test names the value by, and the value as the server answered it.
type nodeAnchorWireLine struct {
	Schema string          `json:"schema"`
	Label  string          `json:"label"`
	Value  json.RawMessage `json:"value"`
}

// nodeAnchorTreeNode is what c-node-anchor reads of a node of the tree read: its kind and id, the
// node it stands under, and the anchor a comment on text answers (model.IssueTreeAnchor), with the
// container_node_id of an orphaned document anchor, nil where the answer holds none.
type nodeAnchorTreeNode struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Parent *struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"parent"`
	Anchor *struct {
		Node *struct {
			Kind  string `json:"kind"`
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"node"`
		BlockID         *string `json:"block_id"`
		Quote           string  `json:"quote"`
		Occurrence      *int    `json:"occurrence"`
		Orphaned        bool    `json:"orphaned"`
		ContainerNodeID *string `json:"container_node_id"`
	} `json:"anchor"`
}

// nodeAnchored is one comment c-node-anchor writes on a node's text: what the test names it, its
// id, and the node it is on as the store keys it.
type nodeAnchored struct {
	name, id, kind, node string
}

// nodeAnchorTree reads the tree of key as alice, the read with no view, by each node's kind and id.
func nodeAnchorTree(t *testing.T, handler http.Handler, key string) map[string]nodeAnchorTreeNode {
	t.Helper()
	return nodeAnchorTreeOf(t, handler, key, "", "alice")
}

// nodeAnchorTreeOf reads the tree of key with query, "" for the read with no view, as login, by
// each node's kind and id.
func nodeAnchorTreeOf(t *testing.T, handler http.Handler, key, query, login string) map[string]nodeAnchorTreeNode {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/tree"+query, nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("the tree read of %s%s as %s: status=%d body=%s", key, query, login, response.Code, response.Body.String())
	}
	read := decodeBody[struct {
		Nodes []nodeAnchorTreeNode `json:"nodes"`
	}](t, response)
	nodes := make(map[string]nodeAnchorTreeNode, len(read.Nodes))
	for _, node := range read.Nodes {
		nodes[node.Kind+":"+node.ID] = node
	}
	return nodes
}

// requireAnchoredUnder fails t unless the tree read holds the comment id once, under the node kind
// and parent name.
func requireAnchoredUnder(t *testing.T, tree map[string]nodeAnchorTreeNode, name, id, kind, parent string) nodeAnchorTreeNode {
	t.Helper()
	node, held := tree[graphstore.KindComment+":"+id]
	switch {
	case !held:
		t.Errorf("the tree read answers no %s %s: want it under %s:%s", name, id, kind, parent)
	case node.Parent == nil || node.Parent.Kind != kind || node.Parent.ID != parent:
		t.Errorf("the tree read answers %s %s under %+v, want it under %s:%s", name, id, node.Parent, kind, parent)
	}
	return node
}

// requirePair fails t unless comment id states, now, its anchored_in and its part_of to the node
// kind and node names where held, and neither of them where it does not.
func requirePair(t *testing.T, database *store.Store, name, id, kind, node string, held bool) {
	t.Helper()
	for _, relation := range []string{graphstore.TupleAnchoredIn, graphstore.TuplePartOf} {
		stated, err := graphstore.CurrentOut(context.Background(), database.Pool, graphstore.KindComment, id, []string{relation})
		if err != nil {
			t.Fatalf("read the %s of %s %s: %v", relation, name, id, err)
		}
		found := false
		for _, tuple := range stated {
			if tuple.ToKind == kind && tuple.ToID == node {
				found = true
			}
		}
		if found != held {
			t.Errorf("%s %s states %s %+v, want it to the node %s:%s %t", name, id, relation, stated, kind, node, held)
		}
	}
}

// appendText states text as the newest text value of the node kind and id names, through the
// store's writer in a transaction of its own that runs the node-anchor refresh of that write
// (docs.RefreshNodeAnchors), as each later write that appends a node's text value calls it (R31),
// and answers how many comments the refresh orphaned.
func appendText(t *testing.T, database *store.Store, kind, id, text string) int {
	t.Helper()
	ctx := context.Background()
	who := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now().UTC()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the text of %s:%s: %v", kind, id, err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
		FromKind: kind, FromID: id, Kind: graphstore.AttributeText, ToKind: graphstore.KindValue, ToID: text,
		At: at, Who: who, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}); err != nil {
		t.Fatalf("append the text of %s:%s: %v", kind, id, err)
	}
	orphans, err := docs.RefreshNodeAnchors(ctx, tx, graphstore.NodeKey{Kind: kind, ID: id}, text, who, at)
	if err != nil {
		t.Fatalf("refresh the node anchors on %s:%s: %v", kind, id, err)
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		orphans.Queue(b)
		return nil
	}); err != nil {
		t.Fatalf("write the node anchors the refresh of %s:%s orphaned: %v", kind, id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the text of %s:%s: %v", kind, id, err)
	}
	return len(orphans.Issues())
}

// readStoredNodeAnchor is comments.node_anchor of comment id as stored, and whether
// comments.anchor is null beside it.
func readStoredNodeAnchor(t *testing.T, database *store.Store, id string) (*model.NodeAnchor, bool) {
	t.Helper()
	var encoded []byte
	var documentless bool
	if err := database.Pool.QueryRow(context.Background(),
		`select node_anchor, anchor is null from comments where id = $1::uuid`, id).Scan(&encoded, &documentless); err != nil {
		t.Fatalf("read the anchors of comment %s: %v", id, err)
	}
	if encoded == nil {
		return nil, documentless
	}
	var anchor model.NodeAnchor
	if err := json.Unmarshal(encoded, &anchor); err != nil {
		t.Fatalf("decode comments.node_anchor of %s: %v", id, err)
	}
	return &anchor, documentless
}

// activityThread is one thread of the comment page of order=activity, which R31's merge-order rule
// names: its first comment and its replies, oldest first, each shaped as the comment list shapes a
// row.
type activityThread struct {
	Comment map[string]json.RawMessage   `json:"comment"`
	Replies []map[string]json.RawMessage `json:"replies"`
}

// activityCommentPage is the comment page of order=activity: its threads, the position after the
// page, and how many threads follow it.
type activityCommentPage struct {
	Threads   []activityThread `json:"threads"`
	Before    *string          `json:"before"`
	Remaining int              `json:"remaining"`
}

// readActivityPage is response, the read of the comment page of order=activity that what names,
// failing t unless it answered 200 with the object {threads, before, remaining}.
func readActivityPage(t *testing.T, what string, response *httptest.ResponseRecorder) activityCommentPage {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s, want 200 with its page", what, response.Code, response.Body.String())
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &members); err != nil {
		t.Fatalf("%s answered no object {threads, before, remaining}: %v: %s", what, err, response.Body.String())
	}
	for _, member := range []string{"threads", "before", "remaining"} {
		if _, held := members[member]; !held {
			t.Fatalf("%s answered no member %s, want the object {threads, before, remaining}: %s", what, member, response.Body.String())
		}
	}
	var page activityCommentPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode %s: %v: %s", what, err, response.Body.String())
	}
	return page
}

// firsts is the id of each thread's first comment, in the page's order.
func (page activityCommentPage) firsts() []string {
	ids := make([]string, 0, len(page.Threads))
	for _, thread := range page.Threads {
		var id string
		_ = json.Unmarshal(thread.Comment["id"], &id)
		ids = append(ids, id)
	}
	return ids
}

// first is the first comment of the thread the comment id begins, as the page answers it, and
// false where the page holds no such thread.
func (page activityCommentPage) first(id string) (map[string]json.RawMessage, bool) {
	for index, held := range page.firsts() {
		if held == id {
			return page.Threads[index].Comment, true
		}
	}
	return nil, false
}

// TestACommentOnANodesTextHangsUnderThatNode is c-node-anchor (this issue's spec R31, Verification
// v220). On a test server a person writes an issue whose spec holds the requirement r1 and whose
// Verification document holds the check k1 with one result. She invokes a pre-mortem lens on the
// requirement with one finding and one risk, an author-only lens of her own beside it, and a
// constraint of the issue. Each node's own text as the tree read answers it - the lens's verdict,
// the finding's text, the risk's heading, the constraint's standard, the result's observed words -
// holds the word bound twice, and she writes one comment on each of the five with the anchor
// {node, quote: bound, occurrence: 1}, the zero-based second match, as a quote's beside block_id
// is counted (R8). An agent replies to the comment on the lens. A comment on a second finding,
// `the **upper** bound of [the pool](...)`, quotes `upper bound of the pool`, words that cross its
// bold and its link, which its plain text holds. She writes a prose block in the room's own
// fragment and a comment anchored in it by artifact, the issue's tree artifact (the row of kind
// tree), and block_id, which answers 201 and not 400 NOT_DOCUMENT. The test then writes, as a
// process of the release before would, a comments row anchored in r1 with no anchored_in and no
// part_of, and runs the backfill's passes.
//
// The read with no view answers each of the five under the node its anchor names, with its
// part_of and anchored_in to that node in the store, and its anchor holding the node, bound,
// occurrence 1 and orphaned false; the reply under the comment on the lens; the comment on the
// prose block under that block; and the levelled row under r1, its part_of dated at its
// created_at. comments.node_anchor holds each node anchor, comments.anchor is null on each. The
// test then appends a finding text that holds bound once through the store's writer, in a
// transaction that runs the node-anchor refresh, reads the committed orphaned mark and the
// retractions before any tree read, and then the comments read with node_anchor=true answers
// orphaned true with the same quote and occurrence, the tree answers the comment under the issue,
// and the tree read changed nothing stored. The store census reads comment_anchor_block at zero
// differences. The comments reads answer each of the five with anchor null and no node_anchor,
// written to NODE_ANCHOR_WIRE for main's CommentSchema with each comment.created payload; with
// node_anchor=true they answer each one's node_anchor, written there for this branch's
// CommentWithNodeAnchorSchema. Each refusal is 400 INVALID_ANCHOR naming its field and writes no
// comment, a quote of the risk's early warning, which is no part of its text, among them.
//
// Where the base holds the comment page of order=activity, which a list answers where it does not
// (the test then logs so and asserts nothing more of it), the page answers each of the five
// threads' first comment with anchor null and no node_anchor, and with node_anchor=true its
// node_anchor, the same threads in the same order and the same remaining, under another ETag that
// the read with node_anchor=true does not answer 304 to; and person=in holds an agent's thread on
// the constraint's text that no person answered.
//
// She writes a paragraph of the spec holding limit twice and two comments anchored in it by
// artifact and block_id, on the second limit and on the first: the read with no view answers the
// first's anchor with occurrence 1 and the second's with occurrence 0, and the comments read
// answers neither with an occurrence. Once an edit leaves the paragraph holding the first limit
// alone, the read answers the first's anchor orphaned with no occurrence and the second's with
// occurrence 0.
//
// An orphaned document anchor answers container_node_id, the tree node id of the nearest typed
// block that holds its block at the read's cut (R31). She writes a requirement r2, a callout n2
// whose list holds a paragraph, and a requirement r3, and a comment anchored by artifact and
// block_id in each one's paragraph; each stands under its typed block, its anchor holding no
// container_node_id. A version of the spec query is written while the three anchors stand. One
// edit then drops the quoted words and keeps each paragraph and its typed block: the read with no
// view answers each comment under the issue, its block_id, orphaned, no occurrence and the
// container_node_id of r2, n2 or r3, each a node of the answer, and its anchored_in and its part_of
// stay retracted; it answers none on the second limit's orphaned anchor, which no typed block
// holds, none on the first limit's, which is not orphaned, and none on the finding's node anchor.
// Once r3 is deleted, the read with no view answers none on the comment in r3's paragraph, which
// is gone, and ?view=spec at the version written before the orphaning answers r3, the container
// at that version's cut.
//
// Then she uploads a pre-mortem document with one risk and anchors a comment in a block of it.
// Its conversion (POST /api/v1/issues/{key}/artifacts/{slug}/premortem-convert) leaves the
// comment's stored anchor, its anchored_in and its part_of naming that block, moves the
// document's address alone to the lens, and moves no comment to the risk; the skeleton, which
// holds no block of the pre-mortem, answers the comment under the issue named by its anchor, its
// parent naming the block, with its anchor naming the block, and the comments read lists it.
//
// Last, bob's read answers r2 as the container of the comment in its paragraph until r2 is
// reserved for its author, alice; then it answers none there, and n2 still on the comment in n2.
func TestACommentOnANodesTextHangsUnderThatNode(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	rig := reasoningRigOn(t, handler, database)
	key := rig.issue
	var wire []nodeAnchorWireLine
	keep := func(schema, label string, value json.RawMessage) {
		wire = append(wire, nodeAnchorWireLine{Schema: schema, Label: label, Value: value})
	}

	// Each node's own text, as the tree read answers it, holds bound twice: a lens's verdict, a
	// finding's text, a risk's heading, a constraint's standard and a result's observed words.
	premortem := rig.invoke(rig.requirement, graphstore.LensKindPreMortem, "alice", map[string]any{
		"verdict": "The write is bound by its key, and the retry is bound by it too.",
	})
	added := rig.post(nodePath("lens/"+premortem.ID, "findings"), map[string]any{
		"text": "A retry is bound to its key, and a replay is bound to it too.",
	}, "alice")
	if added.Code != http.StatusCreated {
		t.Fatalf("add a finding to the lens: status=%d body=%s", added.Code, added.Body.String())
	}
	finding := decodeBody[lensFinding](t, added)
	crossing := rig.post(nodePath("lens/"+premortem.ID, "findings"), map[string]any{
		"text": "the **upper** bound of [the pool](https://example.com/pool)",
	}, "alice")
	if crossing.Code != http.StatusCreated {
		t.Fatalf("add a second finding to the lens: status=%d body=%s", crossing.Code, crossing.Body.String())
	}
	crossed := decodeBody[lensFinding](t, crossing)
	risked := rig.post("/risks", map[string]any{
		"lens": premortem.Address, "heading": "The queue is bound to fill, and the writer is bound to wait.",
		"early_warning": "The invoice grows each month.", "prevention": "Size the queue.",
	}, "alice")
	if risked.Code != http.StatusCreated {
		t.Fatalf("write a risk: status=%d body=%s", risked.Code, risked.Body.String())
	}
	risk := decodeBody[riskWritten](t, risked)
	written := rig.post("/constraints", map[string]any{
		"standard": "Latency is bound above, and memory is bound above too.", "reference": "section 9.2.2",
	}, "alice")
	if written.Code != http.StatusCreated {
		t.Fatalf("write a constraint: status=%d body=%s", written.Code, written.Body.String())
	}
	constraint := decodeBody[constraintRead](t, written)
	recorded := recordResult(t, handler, key, resultBody("k1", "pass", headCommit, map[string]any{
		"observed": "The run stayed bound, and the store stayed bound.",
	}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result of k1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	result := decodeBody[recordedResult](t, recorded).Result
	hidden := rig.invoke(rig.requirement, "trade-off", "alice", map[string]any{
		"visibility": graphstore.VisibilityAuthor, "verdict": "A hidden lens is bound once and bound twice.",
	})
	gone := rig.invoke(rig.requirement, "trade-off", "alice", map[string]any{"verdict": "A gone lens is bound once and bound twice."})
	rig.writeState(gone.Address, graphstore.StateGone)

	// The API's occurrence is zero-based, as a quote's beside block_id is (R8): 1 names the second
	// bound.
	anchor := func(node string) map[string]any {
		return map[string]any{"node": node, "quote": "bound", "occurrence": 1}
	}
	person := func(body string, input map[string]any) string {
		t.Helper()
		return createThreadComment(t, handler, key, map[string]any{"body": body, "anchor": input}, "alice").ID
	}
	onNodes := []nodeAnchored{
		{name: "the comment on the lens", kind: graphstore.KindLens, node: premortem.ID},
		{name: "the comment on the finding", kind: refs.BlockNodeKind, node: finding.ID},
		{name: "the comment on the risk", kind: graphstore.KindRisk, node: risk.ID},
		{name: "the comment on the constraint", kind: graphstore.KindConstraint, node: constraint.ID},
		{name: "the comment on the result", kind: graphstore.KindResult, node: result.ID},
	}
	addresses := []string{"lens/" + premortem.ID, finding.ID, "risk/" + risk.ID, "constraint/" + constraint.ID, "result/" + result.ID}
	for index := range onNodes {
		onNodes[index].id = person("What bounds it?", anchor(addresses[index]))
	}
	lensComment, findingComment := onNodes[0], onNodes[1]
	reply := createThreadComment(t, handler, key, map[string]any{
		"body": "The key bounds it.", "reply_to": lensComment.id, "actor": sessionActor(),
	}, "").ID
	// The quote crosses the bold and the link of the finding's Markdown source and stands in its
	// plain text, the text runs joined, so the write takes it.
	const crossingQuote = "upper bound of the pool"
	across := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body": "Which pool?", "anchor": map[string]any{"node": crossed.ID, "quote": crossingQuote},
	}, "alice")
	if across.Code != http.StatusCreated {
		refusal := decodeBody[refusalAnswer](t, across)
		t.Fatalf("quote across inline Markdown of %s answered %d %s, want 201: %s", crossed.Address, across.Code, refusal.Code, across.Body.String())
	}
	crossedComment := decodeBody[model.Comment](t, across).ID

	var tree string
	if err := database.Pool.QueryRow(ctx, `select id::text from artifacts where issue_key = $1 and kind = 'tree'`, key).Scan(&tree); err != nil {
		t.Fatalf("read the tree artifact of %s: %v", key, err)
	}
	const prose = "A prose note on the bound of a write."
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+tree+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": prose}}, "summary": "a prose note",
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("write a prose block into the room's own fragment: status=%d body=%s", edited.Code, edited.Body.String())
	}
	// The tree artifact's own fragment is read through its text route, which answers for a tree
	// as for a doc (R8), its blocks with blocks=true; the blocks route answers a doc alone.
	fragment := decodeBody[struct {
		Markdown string                `json:"markdown"`
		Blocks   []model.ArtifactBlock `json:"blocks"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+tree+"/text?blocks=true", nil, "alice"))
	var proseBlock string
	for _, block := range fragment.Blocks {
		if fragment.Markdown[block.From:block.To] == prose {
			proseBlock = block.ID
		}
	}
	if proseBlock == "" {
		t.Fatalf("the room's own fragment of %s lists no block holding %q", key, prose)
	}
	// The issue's tree artifact, the row of kind tree, is an anchor's document as a doc is (R31).
	proseAnswer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body": "Which write?", "anchor": map[string]any{"artifact": tree, "block_id": proseBlock, "quote": "prose note"},
	}, "alice")
	if proseAnswer.Code != http.StatusCreated {
		refusal := decodeBody[refusalAnswer](t, proseAnswer)
		t.Fatalf("comment anchored in %s of the tree artifact answered %d %s, want 201: %s", proseBlock, proseAnswer.Code, refusal.Code, proseAnswer.Body.String())
	}
	proseComment := decodeBody[model.Comment](t, proseAnswer).ID
	var proseNode string
	if err := database.Pool.QueryRow(ctx, `select `+graphstore.RoomRefKeySQL("$1::uuid")+` || '#' || $2::text`, tree, proseBlock).Scan(&proseNode); err != nil {
		t.Fatalf("read the address of the prose block: %v", err)
	}

	spec := readDecisionDocument(t, database, key, "spec")
	earlier := uuid.NewString()
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (id, issue_key, author, body, anchor, created_at)
		values ($1::uuid, $2, '{"kind":"user","id":"alice"}', 'Idempotent how?',
		        jsonb_build_object('artifact_id', $3::text, 'block_id', 'r1', 'mark_id', $1::text, 'version', 1,
		                           'quote', 'idempotent', 'orphaned', false),
		        now() - interval '2 days')
	`, earlier, key, spec.id); err != nil {
		t.Fatalf("write the comment a process of the release before writes: %v", err)
	}
	requirePair(t, database, "the release before's comment", earlier, refs.BlockNodeKind, rig.requirementNode, false)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("run the backfill's passes: %v", err)
	}
	requirePair(t, database, "the release before's comment", earlier, refs.BlockNodeKind, rig.requirementNode, true)
	var levelledAt, createdAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		select t.at, c.created_at from comments c
		join graph_tuples t on t.from_kind = c.node_kind and t.from_id = c.node_address and t.kind = $2 and t.asserted
		where c.id = $1::uuid order by t.seq desc limit 1
	`, earlier, graphstore.TuplePartOf).Scan(&levelledAt, &createdAt); err != nil {
		t.Fatalf("read the part_of the backfill stated: %v", err)
	}
	if !levelledAt.Equal(createdAt) {
		t.Errorf("the backfill dates the part_of of the release before's comment at %s, want its created_at %s", levelledAt, createdAt)
	}

	read := nodeAnchorTree(t, handler, key)
	for _, comment := range onNodes {
		requirePair(t, database, comment.name, comment.id, comment.kind, comment.node, true)
		node := requireAnchoredUnder(t, read, comment.name, comment.id, comment.kind, comment.node)
		if node.Anchor == nil || node.Anchor.Node == nil || node.Anchor.Node.Kind != comment.kind || node.Anchor.Node.ID != comment.node ||
			node.Anchor.Quote != "bound" || node.Anchor.Occurrence == nil || *node.Anchor.Occurrence != 1 || node.Anchor.Orphaned {
			t.Errorf("the tree read answers %s %s with the anchor %+v, want its node %s:%s, bound, occurrence 1 and not orphaned",
				comment.name, comment.id, node.Anchor, comment.kind, comment.node)
		}
		stored, documentless := readStoredNodeAnchor(t, database, comment.id)
		if stored == nil || stored.NodeKind != comment.kind || stored.NodeID != comment.node || stored.Quote != "bound" ||
			stored.Occurrence != 1 || stored.Orphaned || !documentless {
			t.Errorf("%s %s stores node_anchor %+v and anchor null %t, want the node %s:%s, bound, occurrence 1, not orphaned, and no anchor",
				comment.name, comment.id, stored, documentless, comment.kind, comment.node)
		}
	}
	requireAnchoredUnder(t, read, "the agent's reply to the comment on the lens", reply, graphstore.KindComment, lensComment.id)
	requireAnchoredUnder(t, read, "the comment on the prose block", proseComment, refs.BlockNodeKind, proseNode)
	requireAnchoredUnder(t, read, "the release before's comment", earlier, refs.BlockNodeKind, rig.requirementNode)
	if crossedNode := requireAnchoredUnder(t, read, "the comment across bold and a link", crossedComment, refs.BlockNodeKind, crossed.ID); crossedNode.Anchor == nil ||
		crossedNode.Anchor.Quote != crossingQuote || crossedNode.Anchor.Orphaned {
		t.Errorf("the tree read answers the comment across bold and a link %s with the anchor %+v, want its quote %q and not orphaned",
			crossedComment, crossedNode.Anchor, crossingQuote)
	}

	// The refresh: a finding text that holds bound once orphans the comment at occurrence 1, in
	// the store, before any read.
	if orphaned := appendText(t, database, refs.BlockNodeKind, finding.ID, "A retry is bound to its key."); orphaned != 1 {
		t.Errorf("the refresh of the finding's new text orphaned anchors of %d issues, want the one of %s", orphaned, key)
	}
	stored, _ := readStoredNodeAnchor(t, database, findingComment.id)
	if stored == nil || !stored.Orphaned || stored.Quote != "bound" || stored.Occurrence != 1 {
		t.Fatalf("node anchor %s is not orphaned after its quote changed: comments.node_anchor holds %+v", findingComment.id, stored)
	}
	requirePair(t, database, findingComment.name, findingComment.id, findingComment.kind, findingComment.node, false)
	after := nodeAnchorTree(t, handler, key)
	requireAnchoredUnder(t, after, findingComment.name, findingComment.id, graphstore.KindIssue, key)
	if again, _ := readStoredNodeAnchor(t, database, findingComment.id); again == nil || *again != *stored {
		t.Errorf("the tree read moved the stored node anchor of %s from %+v to %+v", findingComment.id, stored, again)
	}

	report, err := graphcensus.StoreCensus(ctx, database.Pool.Config().ConnString(), false, docs.ReturnDeclined)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	counted := false
	for _, kind := range report.Kinds {
		if kind.Kind != "comment_anchor_block" {
			continue
		}
		counted = true
		t.Logf("comment_anchor_block: rows=%d nodes=%d row differences=%d node differences=%d", kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences)
		if kind.Rows == 0 || kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
			var printed strings.Builder
			report.Write(&printed)
			t.Errorf("the store census reads comment_anchor_block with %d rows and %d and %d differences, want rows and none:\n%s",
				kind.Rows, kind.RowDifferences, kind.NodeDifferences, printed.String())
		}
	}
	if !counted {
		t.Errorf("the store census reads no kind comment_anchor_block")
	}

	// The comments reads without the parameter answer each of the five with anchor null and no
	// node_anchor, as main's strict CommentSchema reads a comment; with it, each one's node_anchor.
	plain := decodeBody[[]json.RawMessage](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/comments", nil, "alice"))
	withAnchors := decodeBody[[]json.RawMessage](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/comments?node_anchor=true", nil, "alice"))
	members := func(raw json.RawMessage) map[string]json.RawMessage {
		var held map[string]json.RawMessage
		if err := json.Unmarshal(raw, &held); err != nil {
			t.Fatalf("decode a comment the comments read answered: %v", err)
		}
		return held
	}
	byID := func(list []json.RawMessage) map[string]map[string]json.RawMessage {
		held := map[string]map[string]json.RawMessage{}
		for _, raw := range list {
			comment := members(raw)
			var id string
			_ = json.Unmarshal(comment["id"], &id)
			held[id] = comment
		}
		return held
	}
	// requirePlain fails t unless answered, comment as what answered it, holds anchor null and no
	// node_anchor, as main's strict CommentSchema reads a comment.
	requirePlain := func(what string, comment nodeAnchored, answered map[string]json.RawMessage) {
		t.Helper()
		if _, carried := answered["node_anchor"]; carried || string(answered["anchor"]) != "null" {
			t.Errorf("%s answers %s %s with anchor %s and node_anchor %t, want anchor null and no node_anchor",
				what, comment.name, comment.id, answered["anchor"], carried)
		}
	}
	// requireNodeAnchor fails t unless answered, comment as what answered it with node_anchor=true,
	// holds the node_anchor of comment: its node, live, bound at occurrence 1, and orphaned where the
	// refresh orphaned it.
	requireNodeAnchor := func(what string, comment nodeAnchored, answered map[string]json.RawMessage) {
		t.Helper()
		want := model.CommentNodeAnchor{Quote: "bound", Orphaned: comment.id == findingComment.id}
		want.Node.Kind, want.Node.ID, want.Node.State = comment.kind, comment.node, graphstore.StateLive
		raw, carried := answered["node_anchor"]
		if !carried {
			t.Errorf("%s answered %s %s with no node_anchor, want %+v at occurrence 1", what, comment.name, comment.id, want)
			return
		}
		var got *model.CommentNodeAnchor
		if err := json.Unmarshal(raw, &got); err != nil || got == nil || got.Occurrence == nil ||
			*got.Occurrence != 1 || got.Node != want.Node || got.Quote != want.Quote || got.Orphaned != want.Orphaned {
			t.Errorf("%s answers %s %s with node_anchor %s (%v), want %+v at occurrence 1", what, comment.name, comment.id, raw, err, want)
		}
	}
	listed, anchored := byID(plain), byID(withAnchors)
	for _, comment := range onNodes {
		held, found := listed[comment.id]
		if !found {
			t.Errorf("the comments read of %s lists no %s %s", key, comment.name, comment.id)
			continue
		}
		requirePlain("the comments read", comment, held)
		encoded, _ := json.Marshal(held)
		keep("CommentSchema", "GET /api/v1/issues/"+key+"/comments: "+comment.name, encoded)
		single := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.id, nil, "alice")
		if single.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/comments/%s: status=%d body=%s", comment.id, single.Code, single.Body.String())
		}
		body := members(single.Body.Bytes())
		requirePlain("GET /api/v1/comments/"+comment.id, comment, members(body["comment"]))
		keep("CommentSchema", "GET /api/v1/comments/"+comment.id+": "+comment.name, body["comment"])

		// With the parameter, each read answers the node_anchor, which this branch's
		// CommentWithNodeAnchorSchema parses (NODE_ANCHOR_WIRE).
		for _, answer := range []struct {
			what    string
			comment map[string]json.RawMessage
		}{
			{"GET /api/v1/issues/" + key + "/comments?node_anchor=true", anchored[comment.id]},
			{"GET /api/v1/comments/" + comment.id + "?node_anchor=true", members(members(dispatchRequest(t, handler, http.MethodGet,
				"/api/v1/comments/"+comment.id+"?node_anchor=true", nil, "alice").Body.Bytes())["comment"])},
		} {
			requireNodeAnchor(answer.what, comment, answer.comment)
			encoded, _ := json.Marshal(answer.comment)
			keep("CommentWithNodeAnchorSchema", answer.what+": "+comment.name, encoded)
		}
	}
	ids := make([]string, len(onNodes))
	for index, comment := range onNodes {
		ids[index] = comment.id
	}
	rows, err := database.Pool.Query(ctx, `select payload from events where type = 'comment.created' and payload->>'id' = any($1) order by id`, ids)
	if err != nil {
		t.Fatalf("read the comment.created payloads: %v", err)
	}
	payloads := 0
	for rows.Next() {
		var payload json.RawMessage
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("read a comment.created payload: %v", err)
		}
		if _, carried := members(payload)["node_anchor"]; carried || string(members(payload)["anchor"]) != "null" {
			t.Errorf("a comment.created payload carries anchor %s and node_anchor %t, want anchor null and no node_anchor: %s",
				members(payload)["anchor"], carried, payload)
		}
		keep("WrittenCommentEventPayloadSchema", "comment.created", payload)
		payloads++
	}
	rows.Close()
	if payloads != len(onNodes) {
		t.Errorf("the events hold %d comment.created payloads of the five comments on nodes, want %d", payloads, len(onNodes))
	}

	// The comment page of order=activity answers the node anchor where this branch's base holds
	// that page (R31's merge-order rule). A base without the page ignores order and answers the
	// comment list, a JSON list, and the child that adds the page then carries its node anchor.
	activity := func(query, etag string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+key+"/comments?order=activity"+query, nil)
		request.Header.Set("X-Dispatch-User", "alice")
		if etag != "" {
			request.Header.Set("If-None-Match", etag)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	probe := activity("", "")
	if probe.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/%s/comments?order=activity: status=%d body=%s", key, probe.Code, probe.Body.String())
	}
	if strings.HasPrefix(strings.TrimSpace(probe.Body.String()), "[") {
		t.Logf("c-node-anchor: the base holds no order=activity comment page, so the child that adds that page carries the node-anchored page")
	} else {
		// One page holds every thread of the issue, so each read answers all five.
		const pageQuery = "&limit=100"
		pagePath := "GET /api/v1/issues/" + key + "/comments?order=activity" + pageQuery
		plainRead, anchoredRead := activity(pageQuery, ""), activity(pageQuery+"&node_anchor=true", "")
		plainPage := readActivityPage(t, pagePath, plainRead)
		anchoredPage := readActivityPage(t, pagePath+"&node_anchor=true", anchoredRead)
		if !slices.Equal(plainPage.firsts(), anchoredPage.firsts()) || plainPage.Remaining != anchoredPage.Remaining {
			t.Errorf("order=activity pages without and with node_anchor answered the threads %v with %d remaining and %v with %d remaining, want the same threads in the same order",
				plainPage.firsts(), plainPage.Remaining, anchoredPage.firsts(), anchoredPage.Remaining)
		}
		for _, comment := range onNodes {
			plainFirst, plainHeld := plainPage.first(comment.id)
			anchoredFirst, anchoredHeld := anchoredPage.first(comment.id)
			if !plainHeld || !anchoredHeld {
				t.Errorf("order=activity pages without and with node_anchor hold the thread of %s %s %t and %t, want it on both",
					comment.name, comment.id, plainHeld, anchoredHeld)
				continue
			}
			requirePlain("order=activity page without node_anchor", comment, plainFirst)
			requireNodeAnchor("order=activity page with node_anchor=true", comment, anchoredFirst)
		}
		plainTag, anchoredTag := plainRead.Header().Get("ETag"), anchoredRead.Header().Get("ETag")
		switch {
		case plainTag == "" || anchoredTag == "":
			t.Errorf("order=activity pages without and with node_anchor answered the ETags %q and %q, want one each", plainTag, anchoredTag)
		case plainTag == anchoredTag:
			t.Errorf("order=activity pages with and without node_anchor answered one ETag %s", plainTag)
		default:
			conditional := activity(pageQuery+"&node_anchor=true", plainTag)
			if conditional.Code != http.StatusOK {
				t.Errorf("order=activity page with node_anchor=true, sent the ETag %s of the page without it, answered %d, want 200 with its page",
					plainTag, conditional.Code)
			} else if again := readActivityPage(t, pagePath+"&node_anchor=true sent "+plainTag, conditional); !slices.Equal(again.firsts(), anchoredPage.firsts()) {
				t.Errorf("order=activity page with node_anchor=true, sent the ETag %s of the page without it, answered the threads %v, want %v",
					plainTag, again.firsts(), anchoredPage.firsts())
			}
		}
		// An agent's thread on a node's text, which no person answers, is a comment thread on text,
		// so person=in holds it.
		agentComment := createThreadComment(t, handler, key, map[string]any{
			"body": "Bounded by which budget?", "anchor": anchor("constraint/" + constraint.ID), "actor": sessionActor(),
		}, "").ID
		personIn := readActivityPage(t, pagePath+"&person=in", activity(pageQuery+"&person=in", ""))
		if _, held := personIn.first(agentComment); !held {
			t.Errorf("%s&person=in answered the threads %v, want the agent's thread %s on the text of %s among them",
				pagePath, personIn.firsts(), agentComment, constraint.Address)
		}
	}

	// Each refusal writes no comment.
	before := len(commentsOnTextListed(t, handler, key))
	for _, refused := range []struct {
		name   string
		anchor map[string]any
		names  []string
	}{
		{"artifact beside node", map[string]any{"artifact": "spec", "node": "lens/" + premortem.ID, "quote": "bound"}, []string{"artifact", "node"}},
		{"neither artifact nor node", map[string]any{"quote": "bound"}, []string{"artifact", "node"}},
		{"block_id beside node", map[string]any{"node": "lens/" + premortem.ID, "block_id": "r1", "quote": "bound"}, []string{"block_id", "node"}},
		{"a node that is a requirement block", map[string]any{"node": rig.requirementNode, "quote": "idempotent"}, []string{"node"}},
		{"node anchor on author-only " + hidden.Address, anchor("lens/" + hidden.ID), []string{"node"}},
		{"node anchor on gone " + gone.Address, anchor("lens/" + gone.ID), []string{"node"}},
		{"quote not in " + risk.Address, map[string]any{"node": "risk/" + risk.ID, "quote": "unbounded"}, []string{"quote"}},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments",
			map[string]any{"body": "Refused?", "anchor": refused.anchor}, "alice")
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, not 400: want 400 INVALID_ANCHOR: %s", refused.name, response.Code, response.Body.String())
			continue
		}
		requireRefusedNaming(t, refused.name, response, http.StatusBadRequest, "INVALID_ANCHOR", refused.names...)
	}
	// Only the node's own text is quotable: a risk's early warning is no part of its text, which is
	// its heading (R31).
	warned := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body": "Refused?", "anchor": map[string]any{"node": "risk/" + risk.ID, "quote": "invoice grows"},
	}, "alice")
	if warned.Code != http.StatusBadRequest {
		t.Errorf("quote of the early_warning of %s answered %d, want 400 INVALID_ANCHOR: %s", risk.Address, warned.Code, warned.Body.String())
	} else {
		requireRefusedNaming(t, "quote of the early_warning of "+risk.Address, warned, http.StatusBadRequest, "INVALID_ANCHOR", "quote")
	}
	if after := len(commentsOnTextListed(t, handler, key)); after != before {
		t.Errorf("the refused anchors wrote comments: the comments read of %s lists %d, want %d", key, after, before)
	}

	// The tree read answers a document anchor's zero-based occurrence within its block (R31): the
	// number of the block's matches of its quote that start before its mark, none where the anchor
	// is orphaned. The comments read answers no occurrence, since model.Anchor stores none.
	const limits = "A read takes a limit, and a write takes a limit too."
	editSpec(t, handler, key, []map[string]any{{"op": "insert", "after": "end", "markdown": limits}})
	limitBlock := blockIDsByText(t, handler, spec.id)[limits]
	if limitBlock == "" {
		t.Fatalf("the spec of %s lists no block holding %q", key, limits)
	}
	limited := func(occurrence int) map[string]any {
		return map[string]any{"artifact": spec.id, "block_id": limitBlock, "quote": "limit", "occurrence": occurrence}
	}
	secondLimit := person("Which limit binds the write?", limited(1))
	firstLimit := person("Which limit binds the read?", limited(0))
	// requireOccurrence fails t unless the tree read answers the anchor of comment id in
	// limitBlock with the quote limit, orphaned as orphaned says, and the occurrence want, or none
	// where want is negative.
	requireOccurrence := func(read map[string]nodeAnchorTreeNode, id string, want int, orphaned bool) {
		t.Helper()
		node, held := read[graphstore.KindComment+":"+id]
		switch {
		case !held || node.Anchor == nil:
			t.Errorf("the tree read answers no anchor of %s, want one in %s", id, limitBlock)
		case node.Anchor.BlockID == nil || *node.Anchor.BlockID != limitBlock || node.Anchor.Quote != "limit" || node.Anchor.Orphaned != orphaned:
			encoded, _ := json.Marshal(node.Anchor)
			t.Errorf("the tree read answers the anchor of %s as %s, want the block %s, the quote limit and orphaned %t", id, encoded, limitBlock, orphaned)
		case want < 0 && node.Anchor.Occurrence != nil:
			t.Errorf("the tree read answers the anchor of %s with occurrence %d, want no occurrence", id, *node.Anchor.Occurrence)
		case want >= 0 && node.Anchor.Occurrence == nil:
			t.Errorf("the tree read answers the anchor of %s with no occurrence, want %d", id, want)
		case want >= 0 && *node.Anchor.Occurrence != want:
			t.Errorf("the tree read answers the anchor of %s with occurrence %d, want %d", id, *node.Anchor.Occurrence, want)
		}
	}
	limitRead := nodeAnchorTree(t, handler, key)
	requireOccurrence(limitRead, secondLimit, 1, false)
	requireOccurrence(limitRead, firstLimit, 0, false)
	listedLimits := byID(decodeBody[[]json.RawMessage](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/comments", nil, "alice")))
	for _, id := range []string{secondLimit, firstLimit} {
		comment, held := listedLimits[id]
		if !held {
			t.Errorf("the comments read of %s lists no comment %s", key, id)
			continue
		}
		if _, carried := members(comment["anchor"])["occurrence"]; carried {
			t.Errorf("the comments read answers the anchor of %s with an occurrence, want none, since model.Anchor stores none: %s", id, comment["anchor"])
		}
	}
	// The paragraph then holds limit once, the first: the second comment's mark goes with the
	// words it was on, and the refresh orphans its anchor.
	editSpec(t, handler, key, []map[string]any{{"op": "replace", "find": "and a write takes a limit too", "with": "and a write takes none"}})
	if _, orphaned := storedCommentAnchor(t, database, secondLimit); !orphaned {
		t.Errorf("the edit that drops the second limit leaves the anchor of %s standing in the store, want it orphaned", secondLimit)
	}
	limitRead = nodeAnchorTree(t, handler, key)
	requireOccurrence(limitRead, secondLimit, -1, true)
	requireOccurrence(limitRead, firstLimit, 0, false)

	// An orphaned document anchor answers container_node_id, the tree node id of the nearest typed
	// block that holds its block at the read's cut, which the margin draws its thread beside (R31).
	// Each of three paragraphs stands in a typed block: r2's own, one in the list of the callout
	// n2, and r3's own.
	const (
		retryWords  = "A retry backs off before it tries again."
		burstWords  = "A burst queues behind the writer."
		replayWords = "A replay reads the ledger first."
	)
	editSpec(t, handler, key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::requirement{#r2}\n" + retryWords + "\n:::\n\n" +
		":::callout{#n2}\n- " + burstWords + "\n:::\n\n:::requirement{#r3}\n" + replayWords + "\n:::\n"}})
	contained := blockIDsByText(t, handler, spec.id)
	// paragraphOf is the id of the block of the spec whose text is words: the one the blocks read
	// lists over exactly those words, else the one whose text trimmed is those words.
	paragraphOf := func(words string) string {
		t.Helper()
		if id := contained[words]; id != "" {
			return id
		}
		var found []string
		for text, id := range contained {
			if strings.TrimSpace(text) == words {
				found = append(found, id)
			}
		}
		if len(found) != 1 {
			t.Fatalf("the spec of %s lists the blocks %v holding %q, want one", key, found, words)
		}
		return found[0]
	}
	// inTyped is one comment anchored in a paragraph a typed block holds: what the test names it, its
	// id, its quote, the block id its anchor names as the write answers it and that block's node, and
	// the typed block's node, the tree node id its orphaned anchor answers as its container_node_id.
	type inTyped struct {
		name, id, quote, block, node, container string
	}
	retry := inTyped{name: "the comment in r2's paragraph", quote: "backs off", block: paragraphOf(retryWords), container: spec.address("r2")}
	burst := inTyped{name: "the comment in the list of n2", quote: "queues behind", block: paragraphOf(burstWords), container: spec.address("n2")}
	replay := inTyped{name: "the comment in r3's paragraph", quote: "reads the ledger", block: paragraphOf(replayWords), container: spec.address("r3")}
	inTypedBlocks := []*inTyped{&retry, &burst, &replay}
	for _, comment := range inTypedBlocks {
		created := createBlockAnchoredComment(t, handler, key, map[string]any{"artifact": spec.id, "block_id": comment.block, "quote": comment.quote})
		comment.id, comment.block = created.ID, *created.Anchor.BlockID
		comment.node = anchoredBlockAddress(t, database, spec.id, comment.block)
	}
	// requireContainer fails t unless read answers the anchor of the comment id, which the test
	// names what, with the container_node_id want, or with none where want is "".
	requireContainer := func(read map[string]nodeAnchorTreeNode, what, id, want string) {
		t.Helper()
		node, held := read[graphstore.KindComment+":"+id]
		switch {
		case !held || node.Anchor == nil:
			t.Errorf("the tree read answers no anchor of %s %s", what, id)
		case want == "" && node.Anchor.ContainerNodeID != nil:
			t.Errorf("the tree read answers the anchor of %s %s with container_node_id %s, want none", what, id, *node.Anchor.ContainerNodeID)
		case want != "" && node.Anchor.ContainerNodeID == nil:
			t.Errorf("the tree read answers the orphaned anchor of %s %s with no container_node_id, want %s, the typed block that holds its block",
				what, id, want)
		case want != "" && *node.Anchor.ContainerNodeID != want:
			t.Errorf("the tree read answers the orphaned anchor of %s %s with container_node_id %s, want %s, the typed block that holds its block",
				what, id, *node.Anchor.ContainerNodeID, want)
		}
	}
	// requireOrphanedIn fails t unless read answers the anchor of comment orphaned, with the block_id
	// and the quote it was written with, no occurrence, and the container_node_id want, none where
	// want is "".
	requireOrphanedIn := func(read map[string]nodeAnchorTreeNode, comment inTyped, want string) {
		t.Helper()
		node, held := read[graphstore.KindComment+":"+comment.id]
		if !held || node.Anchor == nil {
			t.Errorf("the tree read answers no anchor of %s %s, want one in %s", comment.name, comment.id, comment.block)
			return
		}
		if anchor := node.Anchor; anchor.BlockID == nil || *anchor.BlockID != comment.block || anchor.Quote != comment.quote || !anchor.Orphaned || anchor.Occurrence != nil {
			encoded, _ := json.Marshal(anchor)
			t.Errorf("the tree read answers the anchor of %s %s as %s, want the block %s, the quote %s, orphaned and no occurrence",
				comment.name, comment.id, encoded, comment.block, comment.quote)
		}
		requireContainer(read, comment.name, comment.id, want)
	}
	// Each comment stands under its typed block, as the read returns no prose of a role document,
	// and its anchor, which is not orphaned, holds no container_node_id.
	onText := nodeAnchorTree(t, handler, key)
	for _, comment := range inTypedBlocks {
		requireAnchoredUnder(t, onText, comment.name, comment.id, refs.BlockNodeKind, comment.container)
		requireContainer(onText, comment.name, comment.id, "")
	}
	// A version of the spec query is written while the three anchors stand, so a read at it carries
	// each comment on the text it quotes.
	editSpec(t, handler, key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::requirement{#r4}\nA reader names the key it reads.\n:::\n"}})
	specQuery := treeReadViewsSpecQuery(t, database, key)
	var pinned int
	if err := database.Pool.QueryRow(ctx, `select coalesce(max(number), 0) from artifact_versions where artifact_id = $1::uuid`, specQuery).Scan(&pinned); err != nil {
		t.Fatalf("read the newest version of the spec query of %s: %v", key, err)
	}
	if treeReadViewsRecorded(t, database, specQuery, pinned) != 1 {
		t.Fatalf("version %d of the spec query of %s holds no record of the store revision it read: this case proves nothing", pinned, key)
	}
	var pinnedAt *time.Time
	var replayMade time.Time
	if err := database.Pool.QueryRow(ctx, `
		select r.at, c.created_at from version_renders r, comments c
		where r.artifact_id = $1::uuid and r.number = $2 and c.id = $3::uuid
	`, specQuery, pinned, replay.id).Scan(&pinnedAt, &replayMade); err != nil {
		t.Fatalf("read when version %d of the spec query of %s was read and when %s was written: %v", pinned, key, replay.name, err)
	}
	if pinnedAt == nil || pinnedAt.Before(replayMade) {
		t.Fatalf("version %d of the spec query of %s was read at %v, before %s %s was written at %s: this case proves nothing",
			pinned, key, pinnedAt, replay.name, replay.id, replayMade)
	}
	// One edit drops each quoted word and keeps each paragraph and the typed block that holds it:
	// the refresh orphans the three anchors and retracts their anchored_in and their part_of.
	editSpec(t, handler, key, []map[string]any{
		{"op": "replace", "find": "backs off before", "with": "pauses before"},
		{"op": "replace", "find": "queues behind the writer", "with": "stands aside"},
		{"op": "replace", "find": "reads the ledger first", "with": "starts from scratch"},
	})
	for _, comment := range inTypedBlocks {
		if block, orphaned := storedCommentAnchor(t, database, comment.id); !orphaned || block != comment.block {
			t.Errorf("the edit that drops the words of %s %s leaves its stored anchor in %q orphaned %t, want it in %s orphaned",
				comment.name, comment.id, block, orphaned, comment.block)
		}
		requirePair(t, database, comment.name, comment.id, refs.BlockNodeKind, comment.node, false)
		requirePair(t, database, comment.name, comment.id, refs.BlockNodeKind, comment.container, false)
	}
	// The read answers each where an orphaned comment stands, under the issue, and its anchor's
	// block_id, orphaned and no occurrence, as before, with the container_node_id of its typed
	// block, a node the answer holds; and none on an orphaned anchor no typed block holds, on an
	// anchor that is not orphaned, or on a node anchor.
	orphanedRead := nodeAnchorTree(t, handler, key)
	for _, comment := range inTypedBlocks {
		requireAnchoredUnder(t, orphanedRead, comment.name, comment.id, graphstore.KindIssue, key)
		requireOrphanedIn(orphanedRead, *comment, comment.container)
		if _, drawn := orphanedRead[refs.BlockNodeKind+":"+comment.container]; !drawn {
			t.Errorf("the tree read answers no node %s, the container_node_id of %s %s", comment.container, comment.name, comment.id)
		}
	}
	requireContainer(orphanedRead, "the comment on the second limit", secondLimit, "")
	requireContainer(orphanedRead, "the comment on the first limit", firstLimit, "")
	requireContainer(orphanedRead, findingComment.name, findingComment.id, "")
	// Once r3 is deleted, its paragraph is gone at the head, so the read with no view answers no
	// container of the comment in it; the read at the version written before the orphaning answers
	// r3, which held that paragraph at its cut, and not what the head holds.
	editSpec(t, handler, key, []map[string]any{{"op": "delete", "block": "r3"}})
	removedRead := nodeAnchorTree(t, handler, key)
	if _, drawn := removedRead[refs.BlockNodeKind+":"+replay.container]; drawn {
		t.Errorf("the tree read answers %s, which the spec no longer holds", replay.container)
	}
	requireAnchoredUnder(t, removedRead, replay.name, replay.id, graphstore.KindIssue, key)
	requireOrphanedIn(removedRead, replay, "")
	requireContainer(removedRead, retry.name, retry.id, retry.container)
	historical := nodeAnchorTreeOf(t, handler, key, fmt.Sprintf("?view=spec&v=%d", pinned), "alice")
	if _, drawn := historical[refs.BlockNodeKind+":"+replay.container]; !drawn {
		t.Errorf("?view=spec&v=%d of %s answers no node %s, which held %s at that version", pinned, key, replay.container, replay.block)
	}
	requireOrphanedIn(historical, replay, replay.container)

	// The pre-mortem conversion moves only the document's address to its lens (R31): a person's
	// comment anchored in a block of the pre-mortem keeps its anchor, its anchored_in and its part_of
	// to that block; the skeleton, which opens no block of the document, answers it under the issue
	// with its anchor naming the block; the comments read lists it; and no comment moves to a risk.
	const premortemVerdict = "The pool is bound to fill under a burst."
	premortemDocument := uploadDocument(t, handler, "/api/v1/issues/"+key, "pool-premortem.md",
		"## Verdict\n\n"+premortemVerdict+"\n\n"+
			"## Risk 0, the pool fills before the burst ends\n\n"+
			"- **Early warning:** The waits for a connection grow.\n"+
			"- **Prevention:** Size the pool to the burst.\n\n"+
			"## What a world-class implementation looks like\n\nOne pool, sized to its burst.\n\n"+
			"## The smallest first slice that proves the model\n\nOne pool and one read.\n").Artifact
	premortemBlock := blockIDsByText(t, handler, premortemDocument.ID)[premortemVerdict]
	if premortemBlock == "" {
		t.Fatalf("the pre-mortem %s lists no block holding %q", premortemDocument.Slug, premortemVerdict)
	}
	const retiredName = "the comment in a block of the pre-mortem"
	retiredComment := person("Which burst?", map[string]any{"artifact": premortemDocument.ID, "block_id": premortemBlock, "quote": "fill under a burst"})
	retiredNode := anchoredBlockAddress(t, database, premortemDocument.ID, premortemBlock)
	requirePair(t, database, retiredName, retiredComment, refs.BlockNodeKind, retiredNode, true)
	storedAnchor := func() string {
		t.Helper()
		var stored string
		if err := database.Pool.QueryRow(ctx, `select coalesce(anchor::text, 'null') from comments where id = $1::uuid`, retiredComment).Scan(&stored); err != nil {
			t.Fatalf("read the anchor of %s %s: %v", retiredName, retiredComment, err)
		}
		return stored
	}
	anchorFacts := func() [2][]string {
		return [2][]string{
			rig.current(graphstore.KindComment, retiredComment, graphstore.TupleAnchoredIn),
			rig.current(graphstore.KindComment, retiredComment, graphstore.TuplePartOf),
		}
	}
	anchoredBefore, factsBefore := storedAnchor(), anchorFacts()
	converted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts/"+premortemDocument.Slug+"/premortem-convert", map[string]any{
		"prevented_by": []map[string]any{{"risk": 0, "checks": []string{}, "reason": "No check of this issue covers the pool."}},
	}, "alice")
	if converted.Code != http.StatusOK {
		t.Fatalf("convert the pre-mortem %s: status=%d body=%s", premortemDocument.Slug, converted.Code, converted.Body.String())
	}
	conversion := decodeBody[PremortemReport](t, converted)
	if conversion.Lens.Address == nil || len(conversion.Risks) != 1 || conversion.Risks[0].Address == nil {
		t.Fatalf("the conversion of %s answers %+v, want its lens and its one risk", premortemDocument.Slug, conversion)
	}
	convertedLens, convertedRisk := *conversion.Lens.Address, *conversion.Risks[0].Address
	if anchored := storedAnchor(); anchored != anchoredBefore {
		t.Errorf("the conversion moved the anchor of %s %s from %s to %s, want it naming the block %s of the retired document still",
			retiredName, retiredComment, anchoredBefore, anchored, premortemBlock)
	}
	if facts := anchorFacts(); !slices.Equal(facts[0], factsBefore[0]) || !slices.Equal(facts[1], factsBefore[1]) {
		t.Errorf("the conversion moved the anchored_in and the part_of of %s %s from %v to %v", retiredName, retiredComment, factsBefore, facts)
	}
	requirePair(t, database, retiredName+" after its conversion", retiredComment, refs.BlockNodeKind, retiredNode, true)
	if moved := rig.current(graphstore.KindArtifact, premortemDocument.RefKey, graphstore.TupleMovedTo); !slices.Equal(moved,
		[]string{graphstore.KindLens + "/" + strings.TrimPrefix(convertedLens, "dispatch://"+graphstore.KindLens+"/")}) {
		t.Errorf("the address of the pre-mortem %s moved to %v, want the lens %s alone", premortemDocument.Slug, moved, convertedLens)
	}
	if moved := rig.current(refs.BlockNodeKind, retiredNode, graphstore.TupleMovedTo); len(moved) != 0 {
		t.Errorf("the conversion moved the block %s to %v, want the document's address alone moved", retiredNode, moved)
	}
	// The skeleton opens no block of the pre-mortem, so the read hangs the comment under the issue,
	// named by its anchor (docs' nameAnchors): its parent names the block, which the answer does not
	// hold, as a comment whose parent the answer does not hold names it (model.IssueTreeRead).
	retiredTree := nodeAnchorTree(t, handler, key)
	retiredRead := requireAnchoredUnder(t, retiredTree, retiredName, retiredComment, refs.BlockNodeKind, retiredNode)
	if _, opened := retiredTree[refs.BlockNodeKind+":"+retiredNode]; opened {
		t.Errorf("the skeleton answers the block %s of the pre-mortem %s, want none of its blocks, so %s %s stands under the issue",
			retiredNode, premortemDocument.Slug, retiredName, retiredComment)
	}
	if held := retiredRead.Anchor; held == nil || held.BlockID == nil || *held.BlockID != premortemBlock || held.Quote != "fill under a burst" || held.Orphaned {
		encoded, _ := json.Marshal(held)
		t.Errorf("the skeleton answers %s %s with the anchor %s, want it naming the block %s with its quote, not orphaned",
			retiredName, retiredComment, encoded, premortemBlock)
	}
	if _, listed := commentsOnTextListed(t, handler, key)[retiredComment]; !listed {
		t.Errorf("the comments read of %s lists no %s %s after the conversion", key, retiredName, retiredComment)
	}
	onRisk, err := readStrings(ctx, database.Pool, `
		select distinct from_id from graph_tuples where from_kind = $1 and to_kind = $2 and to_id = $3
	`, graphstore.KindComment, graphstore.KindRisk, strings.TrimPrefix(convertedRisk, "dispatch://"+graphstore.KindRisk+"/"))
	if err != nil {
		t.Fatalf("read the comments that name the risk %s: %v", convertedRisk, err)
	}
	if len(onRisk) != 0 {
		t.Errorf("the conversion moved the comments %v to its risk %s, want none moved to a risk", onRisk, convertedRisk)
	}

	// The container answers only where the reader may see it at the cut (R31): bob's read answers r2
	// as the container of the comment in its paragraph until r2 is reserved for its author, alice,
	// as a write the store's writers refuse would store it (graphseedtest.SeedAuthorOnly); then it
	// answers none there, and no block above r2 in its stead, and still n2, which he may see, on the
	// comment in n2's list.
	requireContainer(nodeAnchorTreeOf(t, handler, key, "", "bob"), retry.name, retry.id, retry.container)
	reserve, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the reservation of %s: %v", retry.container, err)
	}
	defer reserve.Rollback(ctx)
	if err := graphseedtest.SeedAuthorOnly(ctx, reserve, graphstore.Node{
		Kind: refs.BlockNodeKind, ID: retry.container, Supertag: requirementBlockType, State: graphstore.StateLive,
		MadeAt: time.Now().UTC(), MadeBy: privateNodeAuthor(), SourceEvent: graphstore.NoEvent,
	}); err != nil {
		t.Fatalf("reserve %s for its author: %v", retry.container, err)
	}
	if err := reserve.Commit(ctx); err != nil {
		t.Fatalf("commit the reservation of %s: %v", retry.container, err)
	}
	unreadable := nodeAnchorTreeOf(t, handler, key, "", "bob")
	if _, drawn := unreadable[refs.BlockNodeKind+":"+retry.container]; drawn {
		t.Fatalf("bob's tree read of %s answers %s, which is reserved for alice: this case proves nothing", key, retry.container)
	}
	requireOrphanedIn(unreadable, retry, "")
	requireContainer(unreadable, burst.name, burst.id, burst.container)

	if path := os.Getenv(nodeAnchorWireVariable); path != "" {
		var lines strings.Builder
		for _, line := range wire {
			encoded, err := json.Marshal(line)
			if err != nil {
				t.Fatalf("encode a line of %s: %v", nodeAnchorWireVariable, err)
			}
			lines.Write(encoded)
			lines.WriteByte('\n')
		}
		if err := os.WriteFile(path, []byte(lines.String()), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %d lines of the wire to %s", len(wire), path)
	}
}
