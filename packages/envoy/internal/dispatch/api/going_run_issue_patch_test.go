package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An issue's own PATCH made for a run names the issue as changed by the run only where it changed
// a field of it (spec requirement r-interaction: the tuple comes from each node the
// write created or changed). Under alice's run the labels the issue already holds are sent again:
// the write is accepted and appends its issue.updated, which changes nothing, and no
// changed_by_run goes out of the issue, nor any tuple into the run. The control is the same run's
// PATCH of other labels, which names the issue once, so the run was admitted and the first
// write's none is the write's own.
func TestAPatchForARunThatChangesNoFieldNamesNoChange(t *testing.T) {
	t.Parallel()
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	server := directServer(deps)
	ctx := context.Background()
	issue := createTestIssue(t, handler, "RNC", "The issue a run sends its own labels to")
	held := []string{"held"}
	labelled := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"labels": held}, "alice")
	if labelled.Code != http.StatusOK {
		t.Fatalf("label the issue: status=%d body=%s", labelled.Code, labelled.Body.String())
	}
	sequence := decodeBody[model.Issue](t, labelled).LastSeq
	run, err := server.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "", "Label the issue as it is labelled.")
	if err != nil {
		t.Fatalf("record alice's run: %v", err)
	}
	runWindowAhead(t, database, run.ID)

	resent := requestForRun(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"labels": held}, "alice", run.ID)
	t.Logf("alice's PATCH of the labels the issue holds, under the run: status=%d body=%s", resent.Code, resent.Body.String())
	if resent.Code != http.StatusOK {
		t.Fatalf("send the issue's own labels under the run's header: status=%d body=%s", resent.Code, resent.Body.String())
	}
	if got := decodeBody[model.Issue](t, resent); !slices.Equal(got.Labels, held) || got.LastSeq <= sequence {
		t.Fatalf("the PATCH under the run answers the labels %v at sequence %d, want %v past %d: it appended no issue.updated",
			got.Labels, got.LastSeq, held, sequence)
	}
	if count := changedByRunCount(t, database, graphstore.KindIssue, issue, run.ID); count != 0 {
		t.Errorf("the issue holds %d changed_by_run tuples to the run after a PATCH that changed no field of it, want none", count)
	}
	if count := tuplesIntoRun(t, database, run.ID); count != 0 {
		t.Errorf("%d tuples point at the run after a PATCH that changed nothing, want none", count)
	}

	moved := requestForRun(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"labels": []string{"moved"}}, "alice", run.ID)
	if moved.Code != http.StatusOK {
		t.Fatalf("change the issue's labels under the run's header: status=%d body=%s", moved.Code, moved.Body.String())
	}
	if count := changedByRunCount(t, database, graphstore.KindIssue, issue, run.ID); count != 1 {
		t.Errorf("the issue holds %d changed_by_run tuples to the run after a PATCH that changed its labels, want the one it names", count)
	}
}

// An issue a pod of the release before created during a roll has no node, and its first PATCH
// made for a run gives it the node the backfill gives such a row (plan T14): made by the issue's
// creator at its created_at, which the backfill reads off the issue's row
// (graphcensus/projects_backfill.go's objectKinds), and not by the PATCH's writer at the PATCH's
// time, while the changed_by_run the PATCH appends is its own, signed by its writer, dated within
// the request and caused by its event. Alice creates the issue; its node and every tuple out of it
// are taken away and the backfill is run, which writes the node it gives the row; that node is
// taken away again, and bob changes the issue's labels under his run. The node and its base
// attributes then read exactly as the backfill's did, less their cause: the backfill writes at the
// sentinel, and the PATCH causes what it states by its issue.updated, as every request's write
// does. A second PATCH of the issue for the run moves nothing of the node and appends no second
// changed_by_run.
func TestAPatchForARunGivesAnIssueWithNoNodeTheNodeTheBackfillGivesIt(t *testing.T) {
	t.Parallel()
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	server := directServer(deps)
	ctx := context.Background()
	issue := createTestIssue(t, handler, "RON", "The issue an older pod wrote")
	alice, bob := model.Actor{Kind: "user", ID: "alice"}, model.Actor{Kind: "user", ID: "bob"}
	run, err := server.recordGoingRequest(ctx, bob, "", "Label the issue an older pod wrote.")
	if err != nil {
		t.Fatalf("record bob's run: %v", err)
	}
	runWindowAhead(t, database, run.ID)

	takeNode := func(why string) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `delete from graph_tuples where from_kind = $1 and from_id = $2`, graphstore.KindIssue, issue); err != nil {
			t.Fatalf("take the tuples out of the issue away %s: %v", why, err)
		}
		tag, err := database.Pool.Exec(ctx, `delete from graph_nodes where kind = $1 and id = $2`, graphstore.KindIssue, issue)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("take the issue's node away %s: %d rows, %v", why, tag.RowsAffected(), err)
		}
		// A pod of the release before writes an issue's row naming no node: it knows neither
		// node_kind nor node_address (migration 20261116092210), the marker the backfill's object
		// pass walks issues by (graphcensus/projects.go's objectKinds).
		if _, err := database.Pool.Exec(ctx, `update issues set node_kind = null, node_address = null where key = $1`, issue); err != nil {
			t.Fatalf("take the issue's row's node away %s: %v", why, err)
		}
	}
	// shape is the issue's node as the store holds it, less its cause: its row, and every base
	// attribute tuple ever stated of it (graphstore.History, since a value is no relation
	// CurrentOut answers) with its value, its time and its writer, one line each; and how many of
	// those there are.
	shape := func(what string) (graphstore.Node, string, int) {
		t.Helper()
		node, found, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindIssue, issue)
		if err != nil || !found {
			t.Fatalf("read the issue's node %s: found %t, %v", what, found, err)
		}
		encode := func(who model.Actor) string {
			data, err := json.Marshal(who)
			if err != nil {
				t.Fatalf("encode the writer %+v: %v", who, err)
			}
			return string(data)
		}
		var lines []string
		for _, attribute := range graphstoretest.BaseAttributes() {
			stated, err := graphstore.History(ctx, database.Pool, graphstore.KindIssue, issue, attribute)
			if err != nil {
				t.Fatalf("read the %s of the issue's node %s: %v", attribute, what, err)
			}
			for _, tuple := range stated {
				lines = append(lines, fmt.Sprintf("%s %s=%s asserted=%t at %s by %s", tuple.ToKind, tuple.Kind, tuple.ToID,
					tuple.Asserted, tuple.At.UTC().Format(time.RFC3339Nano), encode(tuple.Who)))
			}
		}
		slices.Sort(lines)
		row := fmt.Sprintf("%s/%s supertag=%s visibility=%s state=%s project=%s parties=%v attributes=%v made at %s by %s",
			node.Kind, node.ID, node.Supertag, node.Visibility, node.State, node.Project, node.Parties, node.Attributes,
			node.MadeAt.UTC().Format(time.RFC3339Nano), encode(node.MadeBy))
		return node, strings.Join(append([]string{row}, lines...), "\n"), len(lines)
	}

	takeNode("before the backfill")
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	backfilled, backfilledShape, stated := shape("the backfill wrote")
	t.Logf("the issue's node as the backfill writes it:\n%s", backfilledShape)
	if !backfilled.MadeBy.SameAs(alice) || backfilled.SourceEvent != graphstore.NoEvent || stated != 3 {
		t.Fatalf("the backfill made the issue's node by %+v under event %d, stating %d base attributes, want alice, its creator, at the sentinel, stating its visibility, state and project",
			backfilled.MadeBy, backfilled.SourceEvent, stated)
	}
	takeNode("as the older pod left it")

	start := time.Now()
	patched := requestForRun(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"labels": []string{"patched"}}, bob.ID, run.ID)
	end := time.Now()
	t.Logf("bob's PATCH of the issue's labels under his run: status=%d body=%s", patched.Code, patched.Body.String())
	if patched.Code != http.StatusOK {
		t.Fatalf("change the labels of the issue with no node under the run's header: status=%d body=%s", patched.Code, patched.Body.String())
	}
	node, patchedShape, _ := shape("the PATCH wrote")
	t.Logf("the issue's node as the PATCH writes it:\n%s", patchedShape)
	if patchedShape != backfilledShape {
		t.Errorf("the PATCH under the run made the issue's node\n%s\nwant the node the backfill makes of the same row\n%s", patchedShape, backfilledShape)
	}
	if node.SourceEvent == graphstore.NoEvent {
		t.Errorf("the PATCH left the issue's node at the sentinel, want it caused by the PATCH's issue.updated")
	}

	changed, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindIssue, issue, []string{graphstore.TupleChangedByRun})
	if err != nil {
		t.Fatalf("read the issue's changed_by_run: %v", err)
	}
	t.Logf("the issue's changed_by_run: %+v", changed)
	if len(changed) != 1 || changed[0].ToKind != graphstore.KindInteraction || changed[0].ToID != run.ID {
		t.Fatalf("the issue holds the changed_by_run %+v, want one to the interaction of run %s", changed, run.ID)
	}
	if mark := changed[0]; !mark.Who.SameAs(bob) || mark.At.Before(start.Truncate(time.Microsecond)) || mark.At.After(end) ||
		mark.SourceEvent != node.SourceEvent {
		t.Errorf("the changed_by_run is signed by %+v at %s under event %d, want bob, the PATCH's writer, between %s and %s, under the PATCH's event %d",
			mark.Who, mark.At.Format(time.RFC3339Nano), mark.SourceEvent, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), node.SourceEvent)
	}

	again := requestForRun(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"labels": []string{"patched-again"}}, bob.ID, run.ID)
	if again.Code != http.StatusOK {
		t.Fatalf("change the issue's labels again under the run's header: status=%d body=%s", again.Code, again.Body.String())
	}
	if kept, keptShape, _ := shape("a second PATCH rewrote"); keptShape != patchedShape || kept.SourceEvent != node.SourceEvent {
		t.Errorf("a second PATCH for the run left the issue's node under event %d\n%s\nwant it as the first left it, under event %d\n%s",
			kept.SourceEvent, keptShape, node.SourceEvent, patchedShape)
	}
	if count := changedByRunCount(t, database, graphstore.KindIssue, issue, run.ID); count != 1 {
		t.Errorf("the issue holds %d changed_by_run tuples to the run after two PATCHes for it, want one", count)
	}
}
