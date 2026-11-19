package graphcensus

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// olderPodRun writes a run of the command bar as a process of the release before records it: its
// going_requests row, asked by author under project, and one text frame for each of frames, in
// sequence from 1, each a going_events row, with no interaction node and no frame value. It
// answers the run's id.
func olderPodRun(t *testing.T, database *store.Store, author model.Actor, project, words string, frames ...string) string {
	t.Helper()
	ctx := context.Background()
	recorded, err := json.Marshal(author.Recorded())
	if err != nil {
		t.Fatalf("encode the author of %q: %v", words, err)
	}
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into going_requests (author, project, words, ends_at)
		values ($1::jsonb, $2, $3, now() + interval '30 seconds')
		returning id::text
	`, recorded, project, words).Scan(&id); err != nil {
		t.Fatalf("record the run %q as an older pod does: %v", words, err)
	}
	for index, text := range frames {
		appendOlderPodFrame(t, database, id, index+1, text)
	}
	return id
}

// appendOlderPodFrame appends one text frame of run at sequence as a process of the release before
// appends it: the going_events row alone, with no frame value.
func appendOlderPodFrame(t *testing.T, database *store.Store, run string, sequence int, text string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into going_events (request_id, sequence, name, data) values ($1::uuid, $2, 'text', to_jsonb($3::text))
	`, run, sequence, text); err != nil {
		t.Fatalf("append frame %d of run %s as an older pod does: %v", sequence, run, err)
	}
}

// giveRunItsNode writes the interaction node of a run an older pod recorded, from its row, as this
// build's first frame of the run does before it takes the node (api's lockGoingRun): WriteRun, in
// a transaction of its own.
func giveRunItsNode(t *testing.T, database *store.Store, run string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the node of run %s: %v", run, err)
	}
	defer tx.Rollback(ctx)
	if held, err := WriteRun(ctx, tx, run); err != nil || !held {
		t.Fatalf("write the node of run %s: held %t, %v", run, held, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the node of run %s: %v", run, err)
	}
}

// runFrameValues counts the frame values the interaction node of run holds.
func runFrameValues(t *testing.T, database *store.Store, run string) int64 {
	t.Helper()
	var values int64
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and asserted
	`, graphstore.KindInteraction, run, graphstore.AttributeFrame, graphstore.KindValue).Scan(&values); err != nil {
		t.Fatalf("count the frame values of run %s: %v", run, err)
	}
	return values
}

// TestTheBackfillMendsWhatAnOlderPodWroteOfARun holds graphcensus/backfill.go to the three things a
// process of the release before writes of a run during a roll or after a rollback, each found by an
// anti-join, row by row (backfillRuns, backfillRunFrames, backfillGoneRuns):
//
//   - a run it recorded, with two frames, and no node: the backfill writes its node, its authored_by
//     and a frame value of each frame, through WriteRun;
//   - a frame it appended to a run this build gave its node: the backfill writes that frame's value
//     and no second value of the frame the node held already;
//   - a run whose row it deleted an hour old, frames with it, whose node this build had written: the
//     backfill states that node gone. Its author wrote nothing else, so the census holds that
//     person to the gone run's own authored_by (goneRunsAuthored).
//
// The census reads each as a difference before the backfill, and zero differences of every kind
// after it, the interaction, its frames and the visibility and state of each run among them. A
// second backfill writes nothing of any of them.
func TestTheBackfillMendsWhatAnOlderPodWroteOfARun(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	carol := model.Actor{Kind: "user", ID: "Carol"}

	recorded := olderPodRun(t, database, alice, "CORE", "who answers this?", "Half an ", "answer.")
	framed := olderPodRun(t, database, alice, "", "and this one?", "One ")
	giveRunItsNode(t, database, framed)
	appendOlderPodFrame(t, database, framed, 2, "two.")
	deleted := olderPodRun(t, database, carol, "", "asked and deleted an hour on", "Gone ")
	giveRunItsNode(t, database, deleted)
	if _, err := database.Pool.Exec(ctx, `delete from going_requests where id = $1::uuid`, deleted); err != nil {
		t.Fatalf("delete run %s as an older pod's sweep does: %v", deleted, err)
	}

	before := storeCensusOf(t, database)
	interactions, frames := censusKind(t, before, graphstore.KindInteraction), censusKind(t, before, "frame")
	t.Logf("before the backfill: interaction %d row difference(s) %+v, %d node difference(s) %+v; frame %d row difference(s) %+v",
		interactions.RowDifferences, interactions.ReportedRows, interactions.NodeDifferences, interactions.ReportedNodes,
		frames.RowDifferences, frames.ReportedRows)
	if !hasDifference(interactions.ReportedRows, "interaction "+recorded, "the row names a node the store does not hold") {
		t.Errorf("the census before the backfill reads no row of run %s with no node: %+v", recorded, interactions.ReportedRows)
	}
	if !hasDifference(interactions.ReportedNodes, "interaction "+deleted, "a live node no row projects") {
		t.Errorf("the census before the backfill reads no node of run %s with no row: %+v", deleted, interactions.ReportedNodes)
	}
	if !hasDifference(frames.ReportedRows, "frame "+framed+" 2", "the row and its node disagree") {
		t.Errorf("the census before the backfill reads no frame of run %s with no value: %+v", framed, frames.ReportedRows)
	}

	report, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	for _, kind := range []struct {
		name string
		rows int64
	}{{graphstore.KindInteraction, 1}, {graphstore.AttributeFrame, 1}, {"interaction_gone", 1}} {
		if done := reportedKind(t, report, kind.name); done.Rows != kind.rows || !done.Complete || done.Remaining != 0 {
			t.Errorf("the backfill reports %#v for %s, want %d row and the kind complete", done, kind.name, kind.rows)
		}
	}

	node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindInteraction, recorded)
	if err != nil || !held || node.State != graphstore.StateLive || node.Visibility != graphstore.VisibilityAuthor || node.Project != "CORE" {
		t.Fatalf("the run an older pod recorded reads node %+v (held %t, %v), want it live, reserved for its author, in CORE", node, held, err)
	}
	if author, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindInteraction, recorded,
		[]string{graphstore.TupleAuthoredBy}); err != nil || len(author) != 1 || author[0].ToKind != graphstore.KindPerson || author[0].ToID != "alice" {
		t.Errorf("the run an older pod recorded states authored_by %+v (%v), want person:alice", author, err)
	}
	if values := runFrameValues(t, database, recorded); values != 2 {
		t.Errorf("the run an older pod recorded holds %d frame values, want its 2 frames", values)
	}
	if values := runFrameValues(t, database, framed); values != 2 {
		t.Errorf("the run an older pod appended a frame to holds %d frame values, want 2: the one its node was written with and the older pod's", values)
	}
	if node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindInteraction, deleted); err != nil || !held || node.State != graphstore.StateGone {
		t.Errorf("the run an older pod deleted reads node %+v (held %t, %v), want it kept at state gone", node, held, err)
	}

	after := storeCensusOf(t, database)
	for _, kind := range after.Kinds {
		for _, difference := range append(append([]Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census after the backfill reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	if after.Differences() != 0 {
		t.Errorf("the census after the backfill reads %d difference(s), want none", after.Differences())
	}
	for _, kind := range []struct {
		name        string
		rows, nodes int64
	}{
		{graphstore.KindInteraction, 2, 2},
		{"frame", 4, 2},
		{graphstore.KindInteraction + "_visibility_state", 2, 2},
		{graphstore.KindPerson, 2, 2},
	} {
		if held := censusKind(t, after, kind.name); held.Rows != kind.rows || held.Nodes != kind.nodes {
			t.Errorf("the census after the backfill checked %d rows and %d nodes of %s, want %d and %d",
				held.Rows, held.Nodes, kind.name, kind.rows, kind.nodes)
		}
	}

	second, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store again: %v", err)
	}
	for _, name := range []string{graphstore.KindInteraction, graphstore.AttributeFrame, "interaction_gone"} {
		if done := reportedKind(t, second, name); done.Rows != 0 || done.Nodes != 0 || done.Tuples != 0 || !done.Complete {
			t.Errorf("the second backfill reports %#v for %s, want nothing written and nothing remaining", done, name)
		}
	}
}

// hasDifference reports whether found holds a difference at at whose cause begins with cause.
func hasDifference(found []Difference, at, cause string) bool {
	for _, difference := range found {
		if difference.At == at && strings.HasPrefix(difference.Cause, cause) {
			return true
		}
	}
	return false
}

// requireTheRunFramesLockOrder holds writeRunFrames to its steps. A run stands under no owner row,
// so the writer takes none, and it takes the run's interaction node row `for no key update` first,
// as every frame of this build does (api's appendGoingEvent), before it reads the frames owed
// under that lock and appends their values. It takes no lock on the run's going_requests row, so
// it never queues behind that row, held `for update` for the whole run, and it has read nothing of
// going_events and appended no tuple while it waits at the node row. The run is one this build
// gave its node, to which an older pod then appended a frame with no value.
func requireTheRunFramesLockOrder(t *testing.T) {
	database := storetest.Open(t)
	run := olderPodRun(t, database, model.Actor{Kind: "user", ID: "alice"}, "", "and this one?", "One ")
	giveRunItsNode(t, database, run)
	appendOlderPodFrame(t, database, run, 2, "two.")
	request := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "the run's going_requests row", Step: lockstep.StepProjection},
		`select 1 from going_requests where id = $1::uuid for update`, run)
	node := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the run's interaction node row", Step: lockstep.StepNode, Taken: true,
		Kind: graphstore.KindInteraction, ID: run, Statement: graphstore.NodeRowLockSQL,
		Unlocked: []string{"going_events", "graph_tuples"},
	}, `select 1 from graph_nodes where kind = $1 and id = $2 for update`, graphstore.KindInteraction, run)

	report := followTheBackfill(t, database, "writeRunFrames", []*lockstep.Hold{request, node})
	if done := reportedKind(t, report, graphstore.AttributeFrame); done.Rows != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for frames, want 1 frame and the kind complete", done)
	}
	if values := runFrameValues(t, database, run); values != 2 {
		t.Errorf("the run holds %d frame values after the backfill, want 2: the run the test followed is not one that committed", values)
	}
}

// requireTheGoneRunLockOrder holds writeGoneRun to its steps. It takes no owner row and starts at
// the run's interaction node row, `for no key update`, the lock a frame and the erase of the run
// take it at or above, and reads the node and whether its row stands only under that lock; then it
// moves the node to gone, which appends the state tuple. While it waits at the node row it has
// appended no tuple. The run is one this build gave its node, whose row an older pod deleted.
func requireTheGoneRunLockOrder(t *testing.T) {
	database := storetest.Open(t)
	run := olderPodRun(t, database, model.Actor{Kind: "user", ID: "carol"}, "", "asked and deleted an hour on", "Gone ")
	giveRunItsNode(t, database, run)
	if _, err := database.Pool.Exec(context.Background(), `delete from going_requests where id = $1::uuid`, run); err != nil {
		t.Fatalf("delete run %s as an older pod's sweep does: %v", run, err)
	}
	node := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the run's interaction node row", Step: lockstep.StepNode, Taken: true,
		Kind: graphstore.KindInteraction, ID: run, Statement: graphstore.NodeRowLockSQL,
		Unlocked: []string{"graph_tuples"},
	}, `select 1 from graph_nodes where kind = $1 and id = $2 for update`, graphstore.KindInteraction, run)

	report := followTheBackfill(t, database, "writeGoneRun", []*lockstep.Hold{node})
	if done := reportedKind(t, report, "interaction_gone"); done.Rows != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for runs whose row is gone, want 1 and the kind complete", done)
	}
	if held, found, err := graphstore.ReadNode(context.Background(), database.Pool, graphstore.KindInteraction, run); err != nil || !found || held.State != graphstore.StateGone {
		t.Errorf("the run whose row is gone reads %+v (held %t, %v) after the backfill, want it gone: the run the test followed is not one that committed",
			held, found, err)
	}
}
