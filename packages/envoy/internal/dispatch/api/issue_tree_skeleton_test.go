package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// commentPageRead is what the comment page test reads of a tree read with no view: its nodes and
// their parents, and its comments_before, absent, null or a position.
type commentPageRead struct {
	Nodes []struct {
		Kind     string     `json:"kind"`
		ID       string     `json:"id"`
		Supertag string     `json:"supertag"`
		MadeAt   *time.Time `json:"made_at"`
		Parent   *struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"parent"`
	} `json:"nodes"`
	CommentsBefore json.RawMessage `json:"comments_before"`
}

// comments is the ids of the read's comment nodes.
func (read commentPageRead) comments() []string {
	var ids []string
	for _, node := range read.Nodes {
		if node.Kind == graphstore.KindComment {
			ids = append(ids, node.ID)
		}
	}
	return ids
}

// position is the read's comments_before, "" where it is null.
func (read commentPageRead) position(t *testing.T, page string) string {
	t.Helper()
	var position *string
	if err := json.Unmarshal(read.CommentsBefore, &position); err != nil || len(read.CommentsBefore) == 0 {
		t.Fatalf("%s answers comments_before %q, want a position or null: %v", page, read.CommentsBefore, err)
	}
	if position == nil {
		return ""
	}
	return *position
}

// readCommentPage reads the tree of key with no view as alice, from the position before where it
// is not "", and fails unless it answers 200.
func readCommentPage(t *testing.T, handler http.Handler, key, before string) commentPageRead {
	t.Helper()
	target := "/api/v1/issues/" + key + "/tree"
	if before != "" {
		target += "?comments_before=" + url.QueryEscape(before)
	}
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return decodeBody[commentPageRead](t, response)
}

// requireCommentsBeforeRefused fails unless the tree read of target answers 400 INVALID_QUERY naming
// comments_before and the field it reads.
func requireCommentsBeforeRefused(t *testing.T, handler http.Handler, key, query, what string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/tree?"+query, nil, "alice")
	if response.Code == http.StatusOK && what == "past the end" {
		t.Errorf("comments_before past the end of %s answered 200, not 400", key)
		return
	}
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusBadRequest || refusal.Code != "INVALID_QUERY" ||
		!strings.Contains(refusal.Error, "comments_before") || !strings.Contains(refusal.Error, refs.CommentPositionShape) {
		t.Errorf("comments_before %s on %s (%s) answered status=%d body=%s, want 400 INVALID_QUERY naming comments_before and %s",
			what, key, query, response.Code, response.Body.String(), refs.CommentPositionShape)
	}
}

// The read with no view pages an issue's comments, newest first (R8): a person's issue with a spec
// of one requirement, one ask and 450 comments, the last a reply to the first, reads as three
// pages of 200, 200 and 50 comments, each the comments by made_at and then id just older than the
// page before it, which the comments_before of the page before names as `<made_at>,<id>`; the last
// answers comments_before null. Only the first holds the requirement and the ask. The three pages
// hold each comment the comment list reads once, and the reply on the first page names the first
// comment, on the third, as its parent. A position past the end, a position of another shape and
// one beside a view are each 400 INVALID_QUERY, naming comments_before and its field, and the
// route's description names both.
func TestTheTreeReadPagesTheCommentsNewestFirst(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	createTestProject(t, handler, "CORE")
	key := decodeBody[struct {
		Key string `json:"key"`
	}](t, created(t, "the issue", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "An issue of many comments",
		"spec": ":::requirement{#r1}\nThe tree read pages the comments of an issue.\n:::\n",
	}, "alice"))).Key
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, created(t, "the ask", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/asks",
		map[string]any{"question": "Which page comes first?"}, "alice"))).ID
	var first string
	for index := range 450 {
		body := map[string]any{"body": fmt.Sprintf("Comment %d of the issue.", index+1)}
		if index == 449 {
			body["reply_to"] = first
		}
		id := decodeBody[struct {
			ID string `json:"id"`
		}](t, created(t, fmt.Sprintf("comment %d", index+1), dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", body, "alice"))).ID
		if index == 0 {
			first = id
		}
	}
	indexOwedDocuments(t, database, deps)

	type listedComment struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/comments", nil, "alice")
	succeeded(t, "list the comments", response)
	listed := decodeBody[[]listedComment](t, response)
	if len(listed) != 450 {
		t.Fatalf("the comment list of %s holds %d comments, want 450", key, len(listed))
	}
	slices.SortFunc(listed, func(left, right listedComment) int {
		return cmp.Or(right.CreatedAt.Compare(left.CreatedAt), strings.Compare(right.ID, left.ID))
	})
	madeAt := map[string]time.Time{}
	for _, comment := range listed {
		madeAt[comment.ID] = comment.CreatedAt
	}
	reply := listed[0].ID
	positionOf := func(id string) string {
		return madeAt[id].UTC().Format("2006-01-02T15:04:05.000000Z") + "," + id
	}

	seen := map[string]string{}
	before := ""
	for page, want := range [][2]int{{0, 200}, {200, 400}, {400, 450}} {
		name := fmt.Sprintf("page %d of %s", page+1, key)
		read := readCommentPage(t, handler, key, before)
		comments := read.comments()
		expected := make([]string, 0, want[1]-want[0])
		for _, comment := range listed[want[0]:want[1]] {
			expected = append(expected, comment.ID)
		}
		held := map[string]bool{}
		for _, id := range comments {
			held[id] = true
			if earlier, twice := seen[id]; twice {
				t.Errorf("%s answers comment %s, which %s answered too", name, id, earlier)
			}
			seen[id] = name
		}
		for _, id := range comments {
			if !slices.Contains(expected, id) {
				for _, left := range expected {
					if !held[left] && madeAt[id].Before(madeAt[left]) {
						t.Errorf("%s answers comment %s of %s, older than comment %s it leaves out", name, id, madeAt[id].Format(time.RFC3339Nano), left)
						break
					}
				}
			}
		}
		if len(comments) != len(expected) {
			t.Errorf("%s answers %d comments, want %d", name, len(comments), len(expected))
		}
		for _, id := range expected {
			if !held[id] {
				t.Errorf("%s leaves out comment %s of %s, one of the %d it should answer", name, id, madeAt[id].Format(time.RFC3339Nano), len(expected))
			}
		}
		requirement, asked := false, false
		for _, node := range read.Nodes {
			switch {
			case node.Supertag == requirementBlockType:
				requirement = true
			case node.Kind == graphstore.KindAsk && node.ID == ask:
				asked = true
			case node.Kind != graphstore.KindComment && node.Kind != refs.ItemComment.BlockKind() && page > 0:
				t.Errorf("%s answers the %s node %s, want its comments alone", name, node.Kind, node.ID)
			}
			if node.Kind == graphstore.KindComment && (node.MadeAt == nil || !node.MadeAt.Equal(madeAt[node.ID])) {
				t.Errorf("%s answers comment %s made at %v, want %s as the comment list reads it", name, node.ID, node.MadeAt, madeAt[node.ID])
			}
		}
		if requirement != (page == 0) || asked != (page == 0) {
			t.Errorf("%s answers the requirement %t and the ask %t, want both %t", name, requirement, asked, page == 0)
		}
		position := read.position(t, name)
		switch {
		case want[1] == 450 && position != "":
			t.Errorf("%s answers comments_before %q, want null: no older comment remains", name, position)
		case want[1] < 450 && position != positionOf(listed[want[1]-1].ID):
			t.Errorf("%s answers comments_before %q, want %q, the position of its oldest comment", name, position, positionOf(listed[want[1]-1].ID))
		}
		if page == 0 {
			found := false
			for _, node := range read.Nodes {
				if node.Kind != graphstore.KindComment || node.ID != reply {
					continue
				}
				found = true
				if node.Parent == nil || node.Parent.Kind != graphstore.KindComment || node.Parent.ID != first {
					t.Errorf("%s answers the reply %s under the parent %+v, want the first comment %s, on page 3", name, reply, node.Parent, first)
				}
			}
			if !found {
				t.Errorf("%s does not answer the reply %s, the newest comment", name, reply)
			}
		}
		before = position
		if before == "" {
			before = positionOf(listed[want[1]-1].ID)
		}
	}
	if len(seen) != len(listed) {
		t.Errorf("the three pages of %s answer %d comments, want the %d the comment list reads", key, len(seen), len(listed))
	}

	requireCommentsBeforeRefused(t, handler, key, "comments_before="+url.QueryEscape(positionOf(listed[len(listed)-1].ID)), "past the end")
	requireCommentsBeforeRefused(t, handler, key, "comments_before=abc", "of another shape")
	requireCommentsBeforeRefused(t, handler, key, "view=spec&comments_before="+url.QueryEscape(positionOf(listed[0].ID)), "beside view=spec")

	for _, route := range (&server{}).routes() {
		if route.Method == http.MethodGet && route.Pattern == "/api/v1/issues/{key}/tree" &&
			(!strings.Contains(route.Description, "comments_before") || !strings.Contains(route.Description, refs.CommentPositionShape)) {
			t.Errorf("the description of GET /api/v1/issues/{key}/tree names no comments_before and its field %s: %s", refs.CommentPositionShape, route.Description)
		}
	}
}

// requireLifted fails unless the skeleton of key, read, answers node once, standing under the
// issue's own node.
func requireLifted(t *testing.T, read treeAnswer, key, what string, node model.IssueTreeEnd, under string) {
	t.Helper()
	held := read.nodesNamed(node.Kind, node.ID)
	switch {
	case len(held) != 1:
		t.Errorf("the skeleton of %s answers %s %s:%s %d times, want once: it stands under the block %s of a document the skeleton does not "+
			"open, and stands under the issue in its place", key, what, node.Kind, node.ID, len(held), under)
	case held[0].Parent == nil || held[0].Parent.Kind != graphstore.KindIssue || held[0].Parent.ID != key:
		t.Errorf("the skeleton of %s answers %s %s:%s under %+v, want it under the issue %s: it stands under the block %s of a document the "+
			"skeleton does not open", key, what, node.Kind, node.ID, held[0].Parent, key, under)
	}
}

// The skeleton of the read with no view lifts what stands under a block of a document it does not
// open to the issue, as a node under a hidden node stands under a node the answer holds (R8): a
// lens invoked on a check of an upload, notes.md, with its finding under it, and a child issue
// whose node stands on a paragraph of notes.md. Each stands under the issue, though the check
// itself stands under the requirement it serves: the head rows a read with no view reads hold no
// block of notes.md, so neither read knows that block's parent, and the walk of the tuples takes
// none of them either. The read answers no block of notes.md, and lists notes.md in its
// document_nodes. The results route records a result only against a check of the issue's
// Verification document, so the lens stands for a node under a check of notes.md. The child's
// part_of is written through the store's writer, with its head row, since no route at this head
// places an issue's node on a block. After each write the read answers, from the head rows, every
// view and none, as each of two people, as the walk of the tuples answers it, and the census of
// the head rows reads no difference (headRig).
func TestTheSkeletonLiftsWhatStandsUnderAnotherDocumentToTheIssue(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, deps := newTestServer(t, testServerOptions{store: traced, settle: time.Hour})
	ctx := context.Background()

	const key = "LIFT-1"
	issue := createInteractionIssue(t, handler, "LIFT", "The skeleton lifts what stands under another document",
		":::requirement{#r1}\nThe skeleton lifts what it does not open.\n:::\n")
	if issue.Key != key {
		t.Fatalf("the issue is %s, want %s", issue.Key, key)
	}
	capture := &sqlCapture{pool: database.Pool}
	if _, err := refs.TreeSubtrees(ctx, capture, []refs.Target{{Kind: graphstore.KindIssue, ID: key}}, key, refs.TreeCut{ReadSeq: 1}); err != nil || len(capture.sql) != 1 {
		t.Fatalf("read the walk's subtrees statement: %d statements, %v", len(capture.sql), err)
	}
	r := headRig{t: t, handler: handler, database: database, counter: counter, walked: capture.sql[0]}
	child := decodeBody[struct {
		Key string `json:"key"`
	}](t, created(t, "the child issue", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "LIFT", "title": "A child placed on the notes", "parent": key,
	}, "alice"))).Key
	notes := uploadDocument(t, handler, "/api/v1/issues/"+key, "notes.md",
		"A paragraph of the notes.\n\n"+checkServing("n1", refs.BlockRef("dispatch://"+key+"/spec", "r1")))
	indexOwedDocuments(t, database, deps)
	r.after("upload of notes.md", key)

	check := refs.BlockAddress(notes.Artifact.RefKey, "n1")
	reasoning := reasoningRig{t: t, handler: handler, database: database, issue: key}
	lens := reasoning.invoke(refs.BlockRef("dispatch://"+key+"/artifact/"+notes.Artifact.Slug, "n1"), "trade-off", "alice",
		map[string]any{"verdict": "Keep the check.", "findings": []string{"The check misses the edge."}})
	r.after("lens on a check of notes.md", key)

	var paragraph string
	if err := database.Pool.QueryRow(ctx, `select node_id from document_blocks where artifact_id = $1::uuid and excerpt like 'A paragraph of the notes%'`,
		notes.Artifact.ID).Scan(&paragraph); err != nil {
		t.Fatalf("read the paragraph of notes.md: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the placement of %s: %v", child, err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindIssue, FromID: child, Kind: graphstore.TuplePartOf, ToKind: refs.BlockNodeKind, ToID: paragraph,
		At: time.Now(), Who: model.Actor{Kind: "user", ID: "alice"}, Asserted: true,
	}}); err != nil {
		t.Fatalf("place %s on the paragraph %s: %v", child, paragraph, err)
	}
	if err := graphstore.WriteHeads(ctx, tx, []graphstore.NodeKey{{Kind: graphstore.KindIssue, ID: child}}); err != nil {
		t.Fatalf("write the head row of %s: %v", child, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the placement of %s: %v", child, err)
	}
	r.after("child placed on a paragraph of notes.md", key)

	read := treeRead(t, handler, key, "", "alice", true)
	requireLifted(t, read, key, "the lens", treeEnd(graphstore.KindLens, lens.ID), check)
	requireLifted(t, read, key, "the child issue", treeEnd(graphstore.KindIssue, child), paragraph)
	for _, finding := range lens.Findings {
		held := read.nodesNamed(refs.BlockNodeKind, finding.ID)
		if len(held) != 1 || held[0].Parent == nil || held[0].Parent.Kind != graphstore.KindLens || held[0].Parent.ID != lens.ID {
			t.Errorf("the skeleton of %s answers the finding %s as %+v, want it once under the lens %s", key, finding.ID, held, lens.ID)
		}
	}
	for _, node := range read.Nodes {
		if strings.HasPrefix(node.ID, notes.Artifact.RefKey+"#") {
			t.Errorf("the skeleton of %s answers the block %s:%s of notes.md, a document it does not open", key, node.Kind, node.ID)
		}
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+notes.Artifact.ID+"/blocks", nil, "alice")
	succeeded(t, "read the blocks of notes.md", response)
	blocks := len(decodeBody[[]json.RawMessage](t, response))
	listed := slices.ContainsFunc(read.DocumentNodes, func(document model.IssueTreeDocumentNode) bool {
		return document.ArtifactID == notes.Artifact.ID && document.BlockCount == blocks
	})
	if !listed {
		t.Errorf("the skeleton of %s lists %+v in document_nodes, want notes.md %s with the %d blocks the blocks read lists", key, read.DocumentNodes, notes.Artifact.ID, blocks)
	}
}
