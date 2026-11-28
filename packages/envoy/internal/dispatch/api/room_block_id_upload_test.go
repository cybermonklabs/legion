package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// roomUploadWrites is what an upload into an issue's room can write: the issue's artifacts, the
// room's stored updates and the issue's events.
type roomUploadWrites struct {
	artifacts, roomUpdates, events int
}

// readRoomUploadWrites reads roomUploadWrites of the issue key, whose room is the tree artifact
// room.
func readRoomUploadWrites(t *testing.T, database *store.Store, key, room string) roomUploadWrites {
	t.Helper()
	var writes roomUploadWrites
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from artifacts where issue_key = $1),
		       (select count(*) from doc_updates where artifact_id = $2::uuid),
		       (select count(*) from events where issue_key = $1)`,
		key, room).Scan(&writes.artifacts, &writes.roomUpdates, &writes.events); err != nil {
		t.Fatalf("read what %s and its room hold: %v", key, err)
	}
	return writes
}

// requireRoomBlockIDRefused fails unless response refuses an upload 400 INVALID_MARKDOWN in
// words that name the block id id and holder, the name of the document that holds it, and unless
// the refusal wrote nothing: the issue's artifacts, its room's stored updates and its events are
// what they were before it (before).
func requireRoomBlockIDRefused(t *testing.T, step string, response *httptest.ResponseRecorder, database *store.Store, key, room, id, holder string, before roomUploadWrites) {
	t.Helper()
	t.Logf("%s: answered %d %s", step, response.Code, response.Body.String())
	var refusal errorBody
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusBadRequest || refusal.Code != "INVALID_MARKDOWN" ||
		!strings.Contains(refusal.Error, `block id "`+id+`"`) || !strings.Contains(refusal.Error, strconv.Quote(holder)) {
		t.Errorf("%s: answered %d %s, want 400 INVALID_MARKDOWN naming the block id %q and %q, the document that holds it", step, response.Code, response.Body.String(), id, holder)
	}
	if after := readRoomUploadWrites(t, database, key, room); after != before {
		t.Errorf("%s: the issue and its room hold %+v after the refusal, want %+v: a refused upload writes nothing", step, after, before)
	}
}

// requireSpecHoldsItsTask fails unless the spec of the issue key still holds its plan task t9 as
// it was created: the spec's blocks read names t9 a plan_task holding "The spec's task."; t9's node
// in the issue's room (spec's nodeKey#t9) is live, of the supertag plan_task, and part of the spec
// (its in_document and its issue_room_blocks row); the spec's own address of t9 resolves to that
// node; and t8's needs, which names dispatch://<key>/spec#b-t9, points at it.
func requireSpecHoldsItsTask(t *testing.T, step string, handler http.Handler, database *store.Store, key string, spec decisionDocument) {
	t.Helper()
	ctx := context.Background()
	text := artifactTextOf(t, handler, spec.id)
	held := false
	for _, block := range decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+spec.id+"/blocks", nil, "alice")) {
		if block.ID == "t9" && block.Type == "plan_task" && block.From >= 0 && block.To <= len(text) && block.From < block.To &&
			strings.Contains(text[block.From:block.To], "The spec's task.") {
			held = true
		}
	}
	if !held {
		t.Errorf("%s: the spec's blocks hold no plan_task t9 holding \"The spec's task.\"; its text reads %q", step, text)
	}
	node := spec.address("t9")
	if block := readHeldTypedBlock(t, database, node); block.state != "live" || block.supertag != "plan_task" || block.rowType != "plan_task" || block.room != spec.id {
		t.Errorf("%s: the room's node %s is %+v, want the spec's live plan_task, its issue_room_blocks row naming the spec %s", step, node, block, spec.id)
	}
	if in := currentTies(t, database, node, refs.TupleInDocument); len(in) != 1 || in[0].ToKind != graphstore.KindArtifact || in[0].ToID != spec.refKey {
		t.Errorf("%s: the in_document of %s is %+v, want one, to the spec %s", step, node, in, spec.refKey)
	}
	if named, err := refs.NamedBlock(ctx, database.Pool, key+"/spec#t9"); err != nil || named != node {
		t.Errorf("%s: the spec's address %s/spec#t9 names %q (%v), want the spec's node %s", step, key, named, err, node)
	}
	if needs := currentTies(t, database, spec.address("t8"), graphstore.TupleNeeds); len(needs) != 1 || needs[0].ToKind != refs.BlockNodeKind || needs[0].ToID != node {
		t.Errorf("%s: the needs of the spec's t8 are %+v, want one, to the spec's t9 %s", step, needs, node)
	}
}

// TestAnUploadNeverTakesABlockIDAnotherDocumentOfItsIssueRoomHolds reproduces INFRA-6453 (R13 of
// the issue-tree spec, INFRA-1098's architect's decision of 2026-11-28 on INFRA-4062's finding):
// on an issue born converted, whose spec and plan are fragments of one room, the spec holds
// `:::plan_task{#t9}` and a task t8 whose needs names it. An upload of plan.md naming its own t9 -
// as the plan's first version, and as a later version of a plan that exists - is refused 400
// INVALID_MARKDOWN naming t9 and writes nothing, and the spec's t9 stays the spec's: its id, its
// node and every pointer at it, after the room's settlement, which keeps an id for its first holder
// in the room's order (plan-md before the spec) and so would give the spec's t9 a fresh id.
// An upload of plan.md whose task t5 takes another id and needs dispatch://<key>/spec#b-t9 is
// taken, and that needs points at the spec's task. Last, a version of the spec naming the plan's
// t5 is refused naming t5 and plan.md, and writes nothing.
func TestAnUploadNeverTakesABlockIDAnotherDocumentOfItsIssueRoomHolds(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	const project = "IDC"
	// The project's first issue.
	key := project + "-1"
	createRoomProject(t, handler, project)
	specText := ":::plan_task{#t9}\nThe spec's task.\n:::\n\n" +
		":::plan_task{#t8 needs=\"dispatch://" + key + "/spec#b-t9\"}\nThe spec's task after t9.\n:::\n"
	issue, tree := createRoomIssue(t, handler, database, project, "A spec holding t9", specText, "")
	if issue.Key != key || tree == "" {
		t.Fatalf("created %s with tree %q, want %s born converted", issue.Key, tree, key)
	}
	spec := requireIssueRoomFragment(t, database, key, "spec")
	specName := decodeBody[model.Artifact](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+spec.id, nil, "alice")).Name
	requireSpecHoldsItsTask(t, "the issue as created", handler, database, key, spec)

	path := "/api/v1/issues/" + key + "/artifacts"
	upload := func(content string) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodPost, path, map[string]any{
			"name": "plan.md", "content": content, "precondition": uploadBaseNamed(t, handler, path, "plan.md"),
		}, "alice")
	}
	const planT9 = ":::plan_task{#t9}\nThe plan's task.\n:::\n"
	const planT1 = ":::plan_task{#t1}\nThe plan's first task.\n:::\n"

	before := readRoomUploadWrites(t, database, key, tree)
	requireRoomBlockIDRefused(t, "the plan's first version naming t9", upload(planT9), database, key, tree, "t9", specName, before)
	if id := artifactIDNamed(t, handler, path, "plan.md"); id != "" {
		t.Errorf("the refused first upload left the plan %s, want no plan", id)
	}
	settleIssue(t, documents, key)
	requireSpecHoldsItsTask(t, "after the plan's first version naming t9 and the room's settlement", handler, database, key, spec)

	uploadAs(t, handler, key, "plan.md", planT1, "alice")
	plan := requireIssueRoomFragment(t, database, key, "plan-md")
	planText := artifactTextOf(t, handler, plan.id)
	before = readRoomUploadWrites(t, database, key, tree)
	requireRoomBlockIDRefused(t, "a later version of the plan naming t9", upload(planT1+"\n"+planT9), database, key, tree, "t9", specName, before)
	if version := latestVersion(t, database, plan.id); version.Number != 1 {
		t.Errorf("the plan stands at version %d after the refused upload, want 1", version.Number)
	}
	if text := artifactTextOf(t, handler, plan.id); text != planText {
		t.Errorf("the plan reads %q after the refused upload, want %q", text, planText)
	}
	settleIssue(t, documents, key)
	requireSpecHoldsItsTask(t, "after a later version of the plan naming t9 and the room's settlement", handler, database, key, spec)

	uploadAs(t, handler, key, "plan.md", planT1+"\n"+
		":::plan_task{#t5 needs=\"dispatch://"+key+"/spec#b-t9\"}\nThe plan's task after the spec's.\n:::\n", "alice")
	settleIssue(t, documents, key)
	requireSpecHoldsItsTask(t, "after the plan naming the spec's t9 from t5", handler, database, key, spec)
	if needs := currentTies(t, database, plan.address("t5"), graphstore.TupleNeeds); len(needs) != 1 || needs[0].ToKind != refs.BlockNodeKind || needs[0].ToID != spec.address("t9") {
		t.Errorf("the needs of the plan's t5 are %+v, want one, to the spec's t9 %s", needs, spec.address("t9"))
	}
	var planTasks []string
	for _, block := range decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+plan.id+"/blocks", nil, "alice")) {
		if block.Type == "plan_task" || block.ID == "t9" {
			planTasks = append(planTasks, block.ID)
		}
	}
	if strings.Join(planTasks, " ") != "t1 t5" {
		t.Errorf("the plan's tasks are %v, want t1 and t5 and no t9", planTasks)
	}

	// The rule holds in either order of the room: the spec, which the room renders after the plan,
	// takes no id the plan holds either.
	specNow := artifactTextOf(t, handler, spec.id)
	before = readRoomUploadWrites(t, database, key, tree)
	requireRoomBlockIDRefused(t, "a version of the spec naming the plan's t5", dispatchRequest(t, handler, http.MethodPost, path, map[string]any{
		"name": specName, "content": specNow + "\n:::plan_task{#t5}\nThe spec's task under the plan's id.\n:::\n",
		"precondition": uploadBaseNamed(t, handler, path, specName),
	}, "alice"), database, key, tree, "t5", "plan.md", before)
	if text := artifactTextOf(t, handler, spec.id); text != specNow {
		t.Errorf("the spec reads %q after the refused upload, want %q", text, specNow)
	}
}

// newRoomBlockIDIssue creates, on a server whose settlement waits for the test, the project's
// first issue, born converted, whose spec holds `:::plan_task{#t9}` and a task t8 whose needs names
// it, and whose plan holds t1, and answers the spec, the plan, the spec's name and the issue's
// tree, after the room's settlement.
func newRoomBlockIDIssue(t *testing.T, handler http.Handler, database *store.Store, documents *docs.Service, project string) (key string, spec, plan decisionDocument, specName, tree string) {
	t.Helper()
	key = project + "-1"
	createRoomProject(t, handler, project)
	specText := ":::plan_task{#t9}\nThe spec's task.\n:::\n\n" +
		":::plan_task{#t8 needs=\"dispatch://" + key + "/spec#b-t9\"}\nThe spec's task after t9.\n:::\n"
	issue, tree := createRoomIssue(t, handler, database, project, "A spec holding t9", specText, "")
	if issue.Key != key || tree == "" {
		t.Fatalf("created %s with tree %q, want %s born converted", issue.Key, tree, key)
	}
	uploadAs(t, handler, key, "plan.md", ":::plan_task{#t1}\nThe plan's first task.\n:::\n", "alice")
	settleIssue(t, documents, key)
	spec = requireIssueRoomFragment(t, database, key, "spec")
	plan = requireIssueRoomFragment(t, database, key, "plan-md")
	specName = decodeBody[model.Artifact](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+spec.id, nil, "alice")).Name
	requireSpecHoldsItsTask(t, "the issue as created", handler, database, key, spec)
	return key, spec, plan, specName, tree
}

// TestAnEditNeverTakesABlockIDAnotherDocumentOfItsIssueRoomHolds holds the edit route to R13 as
// TestAnUploadNeverTakesABlockIDAnotherDocumentOfItsIssueRoomHolds holds the upload: an insert into
// the plan of a block naming t9, which the spec holds, through POST /api/v1/artifacts/{id}/edits
// with no token and with the plan's token, is refused 400 INVALID_MARKDOWN naming t9 and spec.md
// and writes nothing, and after the room's settlement the spec's t9 is the spec's still. An insert
// naming an id no other document holds is taken.
func TestAnEditNeverTakesABlockIDAnotherDocumentOfItsIssueRoomHolds(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	key, spec, plan, specName, tree := newRoomBlockIDIssue(t, handler, database, documents, "IDE")
	planText := artifactTextOf(t, handler, plan.id)
	insert := func(markdown string) []map[string]any {
		return []map[string]any{{"op": "insert", "after": "end", "markdown": markdown}}
	}
	for _, edit := range []struct {
		step string
		body func() map[string]any
	}{
		{"an edit of the plan naming t9", func() map[string]any {
			return map[string]any{"ops": insert(":::plan_task{#t9}\nThe plan's task.\n:::\n")}
		}},
		{"an edit of the plan naming t9 under the plan's token", func() map[string]any {
			return map[string]any{"ops": insert(":::plan_task{#t9}\nThe plan's task.\n:::\n"), "precondition": uploadBase(t, handler, plan.id)}
		}},
	} {
		before := readRoomUploadWrites(t, database, key, tree)
		requireRoomBlockIDRefused(t, edit.step, dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+plan.id+"/edits", edit.body(), "alice"),
			database, key, tree, "t9", specName, before)
		if text := artifactTextOf(t, handler, plan.id); text != planText {
			t.Errorf("%s: the plan reads %q after the refused edit, want %q", edit.step, text, planText)
		}
		settleIssue(t, documents, key)
		requireSpecHoldsItsTask(t, edit.step+" and the room's settlement", handler, database, key, spec)
	}
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+plan.id+"/edits", map[string]any{
		"ops": insert(":::plan_task{#t5 needs=\"dispatch://" + key + "/spec#b-t9\"}\nThe plan's task after the spec's.\n:::\n"),
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("an edit of the plan naming t5: status=%d body=%s", edited.Code, edited.Body.String())
	}
	settleIssue(t, documents, key)
	requireSpecHoldsItsTask(t, "after an edit of the plan naming t5 and the room's settlement", handler, database, key, spec)
	if needs := currentTies(t, database, plan.address("t5"), graphstore.TupleNeeds); len(needs) != 1 || needs[0].ToKind != refs.BlockNodeKind || needs[0].ToID != spec.address("t9") {
		t.Errorf("the needs of the plan's t5 are %+v, want one, to the spec's t9 %s", needs, spec.address("t9"))
	}
}

// TestABrowsersPasteNeverRenamesAnotherDocumentsBlock: a browser's edit cannot be refused, and a
// paste of a typed block keeps the id its `#id` names, so a browser can put t9, which the spec
// holds, into the plan's fragment of the room. The room's settlement then keeps t9 for the spec's
// task, which the room's block index recorded holding it, and gives the pasted block a fresh id,
// where it kept the id for its first holder in the room's order (the plan) and renamed the spec's
// task (R13 of the issue-tree spec: no write but the conversion renames another document's
// unchanged block).
func TestABrowsersPasteNeverRenamesAnotherDocumentsBlock(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	key, spec, plan, _, _ := newRoomBlockIDIssue(t, handler, database, documents, "IDB")
	pasted, err := pmdoc.Parse(":::plan_task{#t9}\nThe plan's pasted task.\n:::\n")
	if err != nil || len(pasted.Children) != 1 {
		t.Fatalf("parse the pasted task: %v", err)
	}
	peer := connectBrowserPeer(t, handler, documents, plan.id)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, pasted.Children[0])
		return nil
	})
	peer.barrier(t)
	settleIssue(t, documents, key)
	requireSpecHoldsItsTask(t, "after a browser pasted t9 into the plan and the room's settlement", handler, database, key, spec)
	text := artifactTextOf(t, handler, plan.id)
	if !strings.Contains(text, "The plan's pasted task.") || strings.Contains(text, "{#t9") {
		t.Errorf("the plan reads %q after the room's settlement, want the pasted task under a fresh id", text)
	}
}

// TestAnAcceptedSuggestionNeverTakesABlockIDAnotherDocumentOfItsIssueRoomHolds holds the accept of
// a suggestion to R13: an agent's suggestion on the plan replacing its paragraph with a block
// naming t9, which the spec holds, is refused 400 INVALID_MARKDOWN naming t9 and spec.md when
// alice accepts it, writes nothing, and leaves the spec's t9 the spec's after the room's
// settlement.
func TestAnAcceptedSuggestionNeverTakesABlockIDAnotherDocumentOfItsIssueRoomHolds(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	key, spec, plan, specName, tree := newRoomBlockIDIssue(t, handler, database, documents, "IDS")
	uploadAs(t, handler, key, "plan.md", ":::plan_task{#t1}\nThe plan's first task.\n:::\n\nBody.\n", "alice")
	settleIssue(t, documents, key)
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body": "suggest", "anchor": map[string]any{"artifact": "plan-md", "quote": "Body."},
		"suggestion": map[string]string{"replace_with": ":::plan_task{#t9}\nThe plan's task.\n:::"}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	planText := artifactTextOf(t, handler, plan.id)
	before := readRoomUploadWrites(t, database, key, tree)
	requireRoomBlockIDRefused(t, "the accept of a suggestion naming t9", dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice"),
		database, key, tree, "t9", specName, before)
	if text := artifactTextOf(t, handler, plan.id); text != planText {
		t.Errorf("the plan reads %q after the refused accept, want %q", text, planText)
	}
	settleIssue(t, documents, key)
	requireSpecHoldsItsTask(t, "after the refused accept and the room's settlement", handler, database, key, spec)
}

// TestARebuildNeverTakesABlockIDAnotherDocumentOfItsIssueRoomHolds holds the rebuild from supplied
// markdown to R13: with the issue room's history unloadable, a rebuild of the plan whose markdown
// names t9, which the spec's latest version holds, is refused 400 INVALID_MARKDOWN naming t9 and
// spec.md and writes nothing; one naming t5, whose needs names the spec's t9, is taken, and the
// spec's t9 is the spec's after it.
func TestARebuildNeverTakesABlockIDAnotherDocumentOfItsIssueRoomHolds(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	key, spec, plan, specName, tree := newRoomBlockIDIssue(t, handler, database, documents, "IDR")
	makeDocumentUnloadable(t, repairFixture{handler: handler, database: database, documents: documents, deps: deps}, plan.id)
	before := readRoomUploadWrites(t, database, key, tree)
	standing := latestVersion(t, database, plan.id).Number
	requireRoomBlockIDRefused(t, "a rebuild of the plan naming t9", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+plan.id+"/rebuild", map[string]any{
		"markdown": ":::plan_task{#t1}\nThe plan's first task.\n:::\n\n:::plan_task{#t9}\nThe plan's task.\n:::\n",
	}, "alice"), database, key, tree, "t9", specName, before)
	if version := latestVersion(t, database, plan.id); version.Number != standing {
		t.Errorf("the plan stands at version %d after the refused rebuild, want %d", version.Number, standing)
	}
	rebuilt := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+plan.id+"/rebuild", map[string]any{
		"markdown": ":::plan_task{#t1}\nThe plan's first task.\n:::\n\n:::plan_task{#t5 needs=\"dispatch://" + key + "/spec#b-t9\"}\nThe plan's task after the spec's.\n:::\n",
	}, "alice")
	if rebuilt.Code != http.StatusOK {
		t.Fatalf("a rebuild of the plan naming t5: status=%d body=%s", rebuilt.Code, rebuilt.Body.String())
	}
	settleIssue(t, documents, key)
	requireSpecHoldsItsTask(t, "after the rebuild of the plan and the room's settlement", handler, database, key, spec)
}
