package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// correctionRunRig is a server and its store, an issue alice writes her posts on, and a run of
// alice's, its window moved an hour ahead, that a correction of one of those posts is made for.
type correctionRunRig struct {
	handler  http.Handler
	database *store.Store
	issue    string
	run      string
}

func newCorrectionRunRig(t *testing.T, project string) correctionRunRig {
	t.Helper()
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	issue := createTestIssue(t, handler, project, "The issue whose posts a run corrects")
	run, err := directServer(deps).recordGoingRequest(context.Background(), model.Actor{Kind: "user", ID: "alice"}, "", "Correct my posts on the issue.")
	if err != nil {
		t.Fatalf("record alice's run: %v", err)
	}
	runWindowAhead(t, database, run.ID)
	return correctionRunRig{handler: handler, database: database, issue: issue, run: run.ID}
}

// requireNoChangeByRun fails unless the node kind/id states no changed_by_run, to the rig's run or
// any other, and no tuple points at the rig's run: what a correction made for no run leaves.
func (r correctionRunRig) requireNoChangeByRun(t *testing.T, what, kind, id string) {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), r.database.Pool, kind, id, []string{graphstore.TupleChangedByRun})
	if err != nil {
		t.Fatalf("read the changed_by_run of %s/%s: %v", kind, id, err)
	}
	if len(held) != 0 {
		t.Errorf("%s, made for no run, left %s/%s stating the changed_by_run %+v, want none", what, kind, id, held)
	}
	if count := tuplesIntoRun(t, r.database, r.run); count != 0 {
		t.Errorf("%s, made for no run, left %d tuples pointing at run %s, want none", what, count, r.run)
	}
}

// requireOneChangeByRun fails unless the node kind/id states exactly one changed_by_run, to the
// interaction of the rig's run, signed by alice, who made the write, and caused by the write's own
// event, of type eventType, which the write appended in its own transaction; and unless no other
// node holds a changed_by_run to the run.
func (r correctionRunRig) requireOneChangeByRun(t *testing.T, what, kind, id, eventType string) {
	t.Helper()
	ctx := context.Background()
	held, err := graphstore.CurrentOut(ctx, r.database.Pool, kind, id, []string{graphstore.TupleChangedByRun})
	if err != nil {
		t.Fatalf("read the changed_by_run of %s/%s: %v", kind, id, err)
	}
	t.Logf("%s, made for run %s, left %s/%s stating the changed_by_run %+v", what, r.run, kind, id, held)
	if len(held) != 1 || held[0].ToKind != graphstore.KindInteraction || held[0].ToID != r.run {
		t.Fatalf("%s, made for run %s, left %s/%s stating the changed_by_run %+v, want one to the run's interaction",
			what, r.run, kind, id, held)
	}
	mark := held[0]
	if alice := (model.Actor{Kind: "user", ID: "alice"}); !mark.Who.SameAs(alice) {
		t.Errorf("the changed_by_run of %s/%s is signed by %+v, want alice, who made %s", kind, id, mark.Who, what)
	}
	var cause string
	if err := r.database.Pool.QueryRow(ctx, `select type from events where id = $1`, mark.SourceEvent).Scan(&cause); err != nil {
		t.Fatalf("read the event %d the changed_by_run of %s/%s is caused by: %v", mark.SourceEvent, kind, id, err)
	}
	if cause != eventType {
		t.Errorf("the changed_by_run of %s/%s is caused by event %d, a %s, want the %s %s appended", kind, id, mark.SourceEvent, cause, eventType, what)
	}
	if into, want := changedByRunInto(t, r.database, r.run), []string{kind + "/" + id}; !slices.Equal(into, want) {
		t.Errorf("the nodes holding a changed_by_run to run %s are %v, want %v alone, the one %s changed", r.run, into, want, what)
	}
}

// An edit of an issue message's words made for a run names the message changed by that run (spec
// requirement r-interaction), as an edit of a comment's words does: one changed_by_run out of the
// message's node to the run's interaction, signed by the edit's writer and caused by the
// message.edited the edit appends in its own transaction, and no other node named changed by the
// run. The same edit of another of alice's messages, with no run header, names nothing changed.
func TestAMessageEditMadeForARunNamesTheMessageChangedByIt(t *testing.T) {
	t.Parallel()
	rig := newCorrectionRunRig(t, "RME")
	unrun := createIssueMessage(t, rig.handler, rig.issue, map[string]any{"body": "A message alice edits for no run."}, "alice")
	edited := createIssueMessage(t, rig.handler, rig.issue, map[string]any{"body": "A message alice edits for her run."}, "alice")

	editIssueMessage(t, rig.handler, unrun.ID, "A message alice edited for no run.", "alice")
	rig.requireNoChangeByRun(t, "the edit of message "+unrun.ID, graphstore.KindMessage, unrun.ID)

	const body = "A message alice edited for her run."
	response := requestForRun(t, rig.handler, http.MethodPatch, "/api/v1/messages/"+edited.ID, map[string]any{"body": body}, "alice", rig.run)
	t.Logf("alice's edit of message %s under run %s: status=%d body=%s", edited.ID, rig.run, response.Code, response.Body.String())
	if response.Code != http.StatusOK || decodeBody[model.Message](t, response).Body != body {
		t.Fatalf("edit message %s under run %s's header: status=%d body=%s, want 200 with the new body", edited.ID, rig.run, response.Code, response.Body.String())
	}
	rig.requireOneChangeByRun(t, "the edit of message "+edited.ID, graphstore.KindMessage, edited.ID, "message.edited")
}

// A delete of a comment made for a run names the comment changed by that run, as an edit of its
// words does: one changed_by_run out of the comment's node to the run's interaction, signed by the
// delete's writer and caused by the comment.deleted the delete appends in its own transaction, and
// no other node named changed by the run. The same delete of another of alice's comments, with no
// run header, names nothing changed.
func TestACommentDeleteMadeForARunNamesTheCommentChangedByIt(t *testing.T) {
	t.Parallel()
	rig := newCorrectionRunRig(t, "RCD")
	unrun := createThreadComment(t, rig.handler, rig.issue, map[string]any{"body": "A remark alice deletes for no run."}, "alice")
	deleted := createThreadComment(t, rig.handler, rig.issue, map[string]any{"body": "A remark alice deletes for her run."}, "alice")

	deleteThreadComment(t, rig.handler, unrun.ID, "alice")
	rig.requireNoChangeByRun(t, "the delete of comment "+unrun.ID, graphstore.KindComment, unrun.ID)

	response := requestForRun(t, rig.handler, http.MethodDelete, "/api/v1/comments/"+deleted.ID, nil, "alice", rig.run)
	t.Logf("alice's delete of comment %s under run %s: status=%d body=%s", deleted.ID, rig.run, response.Code, response.Body.String())
	if response.Code != http.StatusOK || decodeBody[model.Comment](t, response).DeletedAt == nil {
		t.Fatalf("delete comment %s under run %s's header: status=%d body=%s, want 200 with the delete mark", deleted.ID, rig.run, response.Code, response.Body.String())
	}
	rig.requireOneChangeByRun(t, "the delete of comment "+deleted.ID, graphstore.KindComment, deleted.ID, "comment.deleted")
}

// A delete of an issue message made for a run names the message changed by that run, as a
// comment's delete does: one changed_by_run out of the message's node to the run's interaction,
// signed by the delete's writer and caused by the message.deleted the delete appends in its own
// transaction, and no other node named changed by the run. The same delete of another of alice's
// messages, with no run header, names nothing changed.
func TestAMessageDeleteMadeForARunNamesTheMessageChangedByIt(t *testing.T) {
	t.Parallel()
	rig := newCorrectionRunRig(t, "RMD")
	unrun := createIssueMessage(t, rig.handler, rig.issue, map[string]any{"body": "A message alice deletes for no run."}, "alice")
	deleted := createIssueMessage(t, rig.handler, rig.issue, map[string]any{"body": "A message alice deletes for her run."}, "alice")

	deleteIssueMessage(t, rig.handler, unrun.ID, "alice")
	rig.requireNoChangeByRun(t, "the delete of message "+unrun.ID, graphstore.KindMessage, unrun.ID)

	response := requestForRun(t, rig.handler, http.MethodDelete, "/api/v1/messages/"+deleted.ID, nil, "alice", rig.run)
	t.Logf("alice's delete of message %s under run %s: status=%d body=%s", deleted.ID, rig.run, response.Code, response.Body.String())
	if response.Code != http.StatusOK || decodeBody[model.Message](t, response).DeletedAt == nil {
		t.Fatalf("delete message %s under run %s's header: status=%d body=%s, want 200 with the delete mark", deleted.ID, rig.run, response.Code, response.Body.String())
	}
	rig.requireOneChangeByRun(t, "the delete of message "+deleted.ID, graphstore.KindMessage, deleted.ID, "message.deleted")
}
