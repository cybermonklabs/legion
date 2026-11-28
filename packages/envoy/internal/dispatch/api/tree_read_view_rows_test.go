package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAViewReadOfItsRowsAnswersAsTheWalk holds a view's head read, which reads the issue's rows of
// the nodes it may answer and of those above them alone (readViewRows, refs.HeadViewRows,
// docs.WalkIssueView) and walks the plan's tuples only where a plan node stands below
// (refs.PlanOrder), to the walk of the whole tree over the tuples (R8: a view answers its query's
// nodes and its context as before), at each state below, with no view and with each view, as
// alice and as bob (headRig.after), and each page of each view's comments_before as each.
//
// Each state holds an edge such a read could miss where it read less than it must: a requirement
// under a person's comment, which only the comment's row places; typed blocks of a document of no
// role among its prose; comments on text whose node a view holds, and whose node it does not; a
// node of a child under the issue's comment, and the same node placed above in the child's walk;
// a risk under a lens bob may not see, and a comment on a risk's text; and more comments on text
// than a view's first read of its rows chooses, the newest on a node the spec view does not hold,
// with a thread whose first comment is older than every comment of that choice and whose reply is
// the newest of them. There the spec view reads its rows again with every comment on text, and the
// plan view, whose choice holds more than its page, reads them once. Last, a view's rows hold none
// of the prose of a document of no role and none of the comments it did not choose, which the rows
// of the whole issue hold.
func TestAViewReadOfItsRowsAnswersAsTheWalk(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, deps, _ := newRegisteredTestServer(t, testServerOptions{store: traced, settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()

	const key = "VRW-1"
	issue := createInteractionIssue(t, handler, "VRW", "A view reads its rows alone", "An opening paragraph.\n\n"+decisionsViewSpec(key))
	if issue.Key != key {
		t.Fatalf("the issue is %s, want %s, which its spec's pointers name", issue.Key, key)
	}
	r := newHeadRig(t, handler, database, counter, key)
	r.requireMarked(key)
	settle := func() {
		t.Helper()
		settleIssue(t, documents, key)
		reconcileBlockIndexPass(t, deps)
	}
	settle()
	r.after("issue create", key)

	comment, block := personComment(t, handler, database, key, "We need the list sized.\n\nThen ordered.")
	editSpec(t, handler, key, []map[string]any{{"op": "insert", "after": "end",
		"markdown": ":::requirement{#r9 parent=\"" + block + "\"}\nEach ask carries its size.\n:::\n"}})
	settle()
	r.after("a requirement under a person's comment", key)

	owner := "/api/v1/issues/" + key
	spec := "dispatch://" + key + "/spec"
	plan := uploadDocument(t, handler, owner, "plan.md", ":::plan_task{#p1}\nBuild the index first.\n:::\n\n:::plan_task{#p2}\nShip the list.\n:::\n")
	notes := uploadDocument(t, handler, owner, "notes.md", "A first note on the list.\n\n"+
		":::requirement{#n1}\nThe notes hold a requirement of their own.\n:::\n\n"+
		checkServing("nk1", refs.BlockRef(spec, "r1"))+"\n"+
		":::plan_task{#np1}\nThe notes plan a task.\n:::\n\nA last note on the list.\n")
	settle()
	r.after("a plan, and notes holding typed blocks among their prose", key)

	onText := func(artifact, blockID, quote, body, login string, extra map[string]any) string {
		t.Helper()
		anchor := map[string]any{"artifact": artifact, "quote": quote}
		if blockID != "" {
			anchor["block_id"] = blockID
		}
		input := map[string]any{"body": body, "anchor": anchor}
		if login == "" {
			input["actor"] = sessionActor()
		}
		maps.Copy(input, extra)
		return createThreadComment(t, handler, key, input, login).ID
	}
	const r1Quote, r2Quote, p1Quote, p2Quote = "The list orders the asks by urgency.", "The list shows each ask once.", "Build the index first.", "Ship the list."
	onText(issue.PrimaryArtifactID, "r1", r1Quote, "The agent reads r1 as urgency alone.", "", nil)
	onText(plan.Artifact.ID, "p1", p1Quote, "Which index?", "alice", nil)
	onText(notes.Artifact.ID, "", "A first note on the list", "A note on the note.", "alice", nil)
	r.after("comments on the text of nodes each view holds and does not", key)

	child := createDigestChild(t, handler, database, "VRW", "The owning child", ":::requirement{#cr1}\nThe child's own requirement.\n:::\n", key, true)
	created(t, "a criterion of the child under its parent's comment", uploadChecks(t, handler, child.Key,
		":::criterion{#ck1 parent=\""+block+"\"}\nI want it quick.\n:::\n"))
	r.after("a node of the child under the issue's comment", key)
	r.after("a node placed above in the child's walk", child.Key)

	// The plan view's walk of a marked issue goes only where a plan node stands below: here a plan
	// task under the person's comment, which the walk reaches through the conversation, and one of
	// the child placed under it, which the parent's rows hold and its own walk does not reach.
	uploadDocument(t, handler, owner, "talk-plan.md", ":::plan_task{#tp1 parent=\""+block+"\"}\nSize the list as the talk asks.\n:::\n")
	uploadDocument(t, handler, "/api/v1/issues/"+child.Key, "child-plan.md",
		":::plan_task{#cp1 parent=\""+block+"\"}\nThe child sizes it too.\n:::\n\n:::plan_task{#cp2}\nThe child ships its own.\n:::\n")
	settle()
	r.after("plan tasks under the issue's comment, one the child's", key)
	r.after("a plan task of the child placed above in its walk", child.Key)
	uploadDocument(t, handler, owner, "talk-plan.md", ":::plan_task{#tp1}\nSize the list as the talk asks.\n:::\n")
	settle()
	r.after("a plan task moved from under the comment to its document", key)

	reasoning := reasoningRig{t: t, handler: handler, database: database, issue: key}
	reserved := reasoning.invoke(refs.BlockRef(spec, "r1"), graphstore.LensKindPreMortem, "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	created(t, "a risk under alice's reserved lens", reasoning.post("/risks", map[string]any{
		"lens": reserved.Address, "heading": "Her own worry.", "early_warning": "She sees it.", "prevention": "She acts.",
	}, "alice"))
	premortem := reasoning.invoke(refs.BlockRef(spec, "r2"), graphstore.LensKindPreMortem, "alice", nil)
	risk := decodeBody[riskWritten](t, created(t, "a risk", reasoning.post("/risks", map[string]any{
		"lens": premortem.Address, "heading": "The sizes drift from the asks.", "early_warning": "Counts disagree.", "prevention": "Count once.",
	}, "alice")))
	createThreadComment(t, handler, key, map[string]any{"body": "Which sizes?", "anchor": map[string]any{"node": "risk/" + risk.ID, "quote": "sizes drift"}}, "alice")
	r.after("a risk under a lens bob may not see, and a comment on a risk's text", key)

	head := onText(issue.PrimaryArtifactID, "r2", r2Quote, "Once per list, or once overall?", "alice", nil)
	for index := range 30 {
		onText(issue.PrimaryArtifactID, "r1", r1Quote, fmt.Sprintf("Agent note %d on r1.", index+1), "", nil)
	}
	for index := range viewTextChoice + 60 {
		onText(plan.Artifact.ID, "p2", p2Quote, fmt.Sprintf("Note %d on shipping.", index+1), "alice", nil)
	}
	createThreadComment(t, handler, key, map[string]any{"body": "Once overall.", "reply_to": head}, "alice")
	r.after("more comments on text than a view's first choice", key)

	// The spec view's first choice, the newest comments on text, holds the reply alone of those it
	// carries, so it reads its rows again with every comment on text; the plan view's holds more
	// than its page of those it carries.
	capture := &sqlCapture{pool: database.Pool}
	if _, _, err := refs.HeadViewRows(ctx, capture, model.Actor{}, key, refs.HeadView{}); err != nil || len(capture.sql) == 0 {
		t.Fatalf("read the statement of a view's rows: %d statements, %v", len(capture.sql), err)
	}
	readsOfRows := func(view string) int {
		t.Helper()
		_, _, sent := r.read(key, view, "alice")
		count := 0
		for _, statement := range sent {
			if statement == capture.sql[0] {
				count++
			}
		}
		return count
	}
	if reads := readsOfRows(graphstore.QuerySpec); reads != 2 {
		t.Errorf("the spec view of %s read its rows %d times, want twice: its first choice of the comments on text carries one", key, reads)
	}
	if reads := readsOfRows(graphstore.QueryPlan); reads != 1 {
		t.Errorf("the plan view of %s read its rows %d times, want once: its first choice carries more than its page", key, reads)
	}

	for _, view := range issueTreeViews {
		for _, login := range []string{"alice", "bob"} {
			requireViewCommentPagesAsWalk(t, r, key, view, login)
		}
	}

	alice := model.Actor{Kind: "user", ID: "alice"}
	viewed, marked, err := refs.HeadViewRows(ctx, database.Pool, alice, key, refs.HeadView{Types: viewTypes[graphstore.QuerySpec]})
	if err != nil || !marked {
		t.Fatalf("read the spec view's rows of %s: marked %t, %v", key, marked, err)
	}
	whole, _, err := refs.HeadRows(ctx, database.Pool, alice, key, nil)
	if err != nil {
		t.Fatalf("read every row of %s: %v", key, err)
	}
	count := func(rows map[refs.Target]*refs.HeadRow, kind string) int {
		held := 0
		for node := range rows {
			if node.Kind == kind {
				held++
			}
		}
		return held
	}
	// Of the blocks that stand under the issue's own node a view reads the first alone, whatever its
	// type: the memo is the first of the issue's children that is no conversation node, where it is a
	// callout (docs' memoFirst), and the spec takes it (docs' queryDefinitions), so that block tells
	// whether the issue holds one. Of every other block of the notes it reads those of the spec view's
	// types alone.
	issueNode := refs.Target{Kind: graphstore.KindIssue, ID: key}
	for node, row := range viewed.Rows {
		switch {
		case node.Kind == graphstore.KindComment && node.ID != comment:
			t.Errorf("the spec view's rows of %s hold the comment %s, which it did not choose and which stands above none of its nodes", key, node.ID)
		case node.Kind == refs.BlockNodeKind && strings.HasPrefix(node.ID, notes.Artifact.RefKey+"#") && row.Parent != issueNode:
			if _, typed, _ := strings.Cut(node.ID, "#"); typed != "n1" && typed != "nk1" {
				text, blockType := "", ""
				if row.Text != nil {
					text = *row.Text
				}
				if row.Block != nil {
					blockType = row.Block.Type
				}
				t.Errorf("the spec view's rows of %s hold %s, a block of the notes that is none of the spec view's types and stands under %v: supertag %q, type %q, text %q",
					key, node.ID, row.Parent, row.Supertag, blockType, text)
			}
		}
	}
	if comments, held := count(viewed.Rows, graphstore.KindComment), count(whole.Rows, graphstore.KindComment); comments > 1 || held < viewTextChoice {
		t.Errorf("the spec view's rows of %s hold %d comments and the issue's rows %d, want the comment above r9 alone of %d or more", key, comments, held, viewTextChoice)
	}
}

// requireViewCommentPagesAsWalk follows each comments_before page of view of key as login, from its
// first page, holding the head read of each to the walk of the tuples (headRig.withoutMark).
func requireViewCommentPagesAsWalk(t *testing.T, r headRig, key, view, login string) {
	t.Helper()
	read := func(target string) (int, []byte) {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("X-Dispatch-User", login)
		request.Header.Set(displayTreesHeader, "1")
		response := httptest.NewRecorder()
		r.handler.ServeHTTP(response, request)
		return response.Code, response.Body.Bytes()
	}
	before := ""
	for page := 1; page <= 20; page++ {
		target := "/api/v1/issues/" + key + "/tree?view=" + url.QueryEscape(view)
		if before != "" {
			target += "&comments_before=" + url.QueryEscape(before)
		}
		status, head := read(target)
		walkStatus, walk := r.withoutMark(t, key, true, func() (int, []byte) { return read(target) })
		if status != http.StatusOK || walkStatus != status || !bytes.Equal(head, walk) {
			node, got, want, _ := treeDifference(head, walk)
			t.Errorf("page %d of %s's %s view as %s (%s): the head read answers status %d, %s as %s; the walk status %d, as %s",
				page, key, view, login, target, status, node, got, walkStatus, want)
			return
		}
		var answered struct {
			CommentsBefore *string `json:"comments_before"`
		}
		if err := json.Unmarshal(head, &answered); err != nil {
			t.Fatalf("decode page %d of %s's %s view: %v", page, key, view, err)
		}
		if answered.CommentsBefore == nil {
			return
		}
		before = *answered.CommentsBefore
	}
	t.Errorf("%s's %s view as %s answers a comments_before past 20 pages", key, view, login)
}
