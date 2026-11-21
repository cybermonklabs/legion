package graphcensus

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
// build's first frame of the run does before it takes the node (api's lockGoingRun):
// graphstore.WriteRun, in a transaction of its own.
func giveRunItsNode(t *testing.T, database *store.Store, run string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the node of run %s: %v", run, err)
	}
	defer tx.Rollback(ctx)
	if written, err := graphstore.WriteRun(ctx, tx, run); err != nil || !written.Held {
		t.Fatalf("write the node of run %s: held %t, %v", run, written.Held, err)
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
//     and a frame value of each frame, through graphstore.WriteRun;
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

// TestTheBackfillGivesEveryRunRecordedBeforeThisReleaseItsNode holds backfillRuns to the runs a
// process of the release before records, which no migration gives a node: `envoy-dispatch
// backfill-graph-store` gives each its node once the roll that brings this release is done. They
// are going_requests rows and their going_events rows, with no interaction node and no frame value
// (olderPodRun), asked by a person's cookie (a login recorded in another spelling than its
// canonical one), by a session a broker's token proved its owner's, by a personal token whose act
// claims a session, by a person whose act names a session the server proved, and by nobody, the
// deployment's shared token. The backfill writes each run's node, reserved for its author and
// live, with its words, its ends_at as Go's RFC 3339 text and its project; the authored_by tuple
// and the author's node where AuthorOf names one, the aliases the server proved and the raw login
// it was recorded under; and a frame value of each frame. A run whose author is nobody gets the
// node and no authored_by. The store census then reads zero differences.
func TestTheBackfillGivesEveryRunRecordedBeforeThisReleaseItsNode(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()

	carol, dave := "Carol", "dave"
	type run struct {
		name       string
		author     model.Actor
		project    string
		frames     []string
		authoredBy string
	}
	runs := []run{
		{"a person's cookie", model.Actor{Kind: "user", ID: "Alice"}, "CORE", []string{"Half an ", "answer."}, "person:alice"},
		{"a session proved its owner's", model.Actor{Kind: "session", ID: "sess-carol", Owner: &carol,
			Act: &model.ActorAct{Session: "sess-carol"}}, "", []string{"One."}, "session:sess-carol"},
		{"a personal token", model.Actor{Kind: "session", Owner: &dave,
			Act: &model.ActorAct{Session: "claimed-in-a-body", TokenID: "tok-1"}}, "", nil, "person:dave"},
		{"a person whose act names a session the server proved", model.Actor{Kind: "user", ID: "erin",
			Act: &model.ActorAct{Session: "sess-erin"}}, "", []string{"Two."}, "person:erin"},
		{"nobody", model.Actor{}, "", []string{"Three."}, ""},
	}
	ids := make([]string, len(runs))
	for index, written := range runs {
		ids[index] = olderPodRun(t, database, written.author, written.project, "What does "+written.name+" ask?", written.frames...)
	}

	done, err := backfillRuns(ctx, database)
	if err != nil {
		t.Fatalf("backfill the runs: %v", err)
	}
	if done.Rows != int64(len(runs)) || done.Remaining != 0 || !done.Complete {
		t.Errorf("the backfill reports %#v, want the %d runs written and none remaining", done, len(runs))
	}

	for index, written := range runs {
		id := ids[index]
		node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindInteraction, id)
		if err != nil || !held {
			t.Fatalf("the run of %s reads no node after the backfill (held %t, %v)", written.name, held, err)
		}
		var endsAt time.Time
		if err := database.Pool.QueryRow(ctx, `select ends_at from going_requests where id = $1::uuid`, id).Scan(&endsAt); err != nil {
			t.Fatalf("read the ends_at of the run of %s: %v", written.name, err)
		}
		if node.Visibility != graphstore.VisibilityAuthor || node.State != graphstore.StateLive || node.Project != written.project ||
			node.Attributes[graphstore.AttributeEndsAt] != endsAt.UTC().Format(time.RFC3339Nano) ||
			node.Attributes[graphstore.AttributeWords] != "What does "+written.name+" ask?" {
			t.Errorf("the run of %s reads node %+v, want it reserved for its author, live, in %q, ending %s", written.name, node,
				written.project, endsAt.UTC().Format(time.RFC3339Nano))
		}
		authors, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindInteraction, id, []string{graphstore.TupleAuthoredBy})
		if err != nil {
			t.Fatalf("read the authored_by of the run of %s: %v", written.name, err)
		}
		var named []string
		for _, author := range authors {
			named = append(named, author.ToKind+":"+author.ToID)
		}
		if want := slices.DeleteFunc([]string{written.authoredBy}, func(s string) bool { return s == "" }); !slices.Equal(named, want) {
			t.Errorf("the run of %s states authored_by %v, want %v", written.name, named, want)
		}
		var frames, values int
		if err := database.Pool.QueryRow(ctx, `
			select (select count(*) from going_events where request_id = $1::uuid),
			       (select count(*) from going_events e where e.request_id = $1::uuid and exists (
			          select 1 from graph_tuples v where v.from_kind = $2 and v.from_id = $5 and v.kind = $3 and v.to_kind = $4
			            and v.to_id = jsonb_build_object('sequence', e.sequence, 'name', e.name, 'data', e.data)::text
			            and v.at = e.created_at and v.who = '{"kind": "system", "id": "going"}'::jsonb))
		`, id, graphstore.KindInteraction, graphstore.AttributeFrame, graphstore.KindValue, id).Scan(&frames, &values); err != nil {
			t.Fatalf("count the frames of the run of %s: %v", written.name, err)
		}
		if frames != len(written.frames) || values != frames {
			t.Errorf("the run of %s holds %d frames and %d frame values stating them, want %d of each", written.name, frames, values, len(written.frames))
		}
	}

	rows, err := database.Pool.Query(ctx, `
		select from_kind || ':' || from_id || ' ' || kind || ' ' || to_kind || ':' || to_id from graph_tuples
		where kind in ($1, $2) order by 1`, graphstore.TupleAliasOf, graphstore.TupleRecordedAs)
	if err != nil {
		t.Fatalf("read the alias tuples: %v", err)
	}
	facts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the alias tuples: %v", err)
	}
	want := []string{"person:alice recorded_as value:Alice", "session:sess-carol alias_of person:carol", "session:sess-erin alias_of person:erin"}
	if !slices.Equal(facts, want) {
		t.Errorf("the backfill states the aliases %v, want %v", facts, want)
	}
	var people, sessions []string
	for kind, into := range map[string]*[]string{graphstore.KindPerson: &people, graphstore.KindSession: &sessions} {
		nodes, err := graphstore.NodesOfKind(ctx, database.Pool, kind)
		if err != nil {
			t.Fatalf("read the %s nodes: %v", kind, err)
		}
		for _, node := range nodes {
			*into = append(*into, node.ID)
		}
	}
	if !slices.Equal(people, []string{"alice", "carol", "dave", "erin"}) || !slices.Equal(sessions, []string{"sess-carol"}) {
		t.Errorf("the backfill made person nodes %v and session nodes %v, want alice, carol, dave and erin, and sess-carol", people, sessions)
	}

	census := storeCensusOf(t, database)
	for _, kind := range census.Kinds {
		for _, difference := range append(append([]Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census after the backfill reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	if census.Differences() != 0 {
		t.Errorf("the census after the backfill reads %d difference(s), want none", census.Differences())
	}
	// Each kind compared the runs: five runs, five frames over the four runs that hold one, the four
	// people and the one session the authors name.
	for _, kind := range []struct {
		name        string
		rows, nodes int64
	}{
		{graphstore.KindInteraction, 5, 5},
		{"frame", 5, 4},
		{graphstore.KindInteraction + "_visibility_state", 5, 5},
		{graphstore.KindPerson, 4, 4},
		{graphstore.KindSession, 1, 1},
	} {
		if held := censusKind(t, census, kind.name); held.Rows != kind.rows || held.Nodes != kind.nodes {
			t.Errorf("the census after the backfill checked %d rows and %d nodes of %s, want %d and %d",
				held.Rows, held.Nodes, kind.name, kind.rows, kind.nodes)
		}
	}
}

// requireTheOwedRunLockOrder holds writeOwedRun to its steps. A run stands under no owner row, so
// the writer takes none. It takes the run's going_requests row `for no key update` first, as the
// one writer of a run's node from its row does (graphstore.WriteRun), and has read nothing of the
// store or of going_events while it waits there; then it writes the run's interaction node and its
// asker's person node in one statement, in ascending (kind, id), each held as the same key a
// transaction still open inserted, and appends their tuples after both, in a later statement of the
// same write phase. That phase is sent as one batch whose statements are all parsed before the
// first runs, so the append's table is locked while the node write waits, as every store write's
// is, and no graph_tuples lock is read for it here. The run is one an older pod recorded with one
// frame and no node, asked by a person no write has recorded. The test follows the pass alone
// (backfillRuns), so a lock another pass of Backfill takes is not read as this batch's.
func requireTheOwedRunLockOrder(t *testing.T) {
	database := storetest.Open(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	run := olderPodRun(t, database, alice, "", "who answers this?", "One ")
	request := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the run's going_requests row", Step: lockstep.StepProjection, Taken: true,
		Statement: "select id::text, author, project, words, created_at, ends_at from going_requests",
		Unlocked:  []string{"going_events", "graph_nodes", "graph_tuples"},
	}, `select 1 from going_requests where id = $1::uuid for update`, run)
	now := time.Now()
	interaction := holdWrite(t, database, &lockstep.Hold{
		Name: "the run's interaction node row", Step: lockstep.StepNode, Taken: true,
		Kind: graphstore.KindInteraction, ID: run, Statement: "insert into graph_nodes (",
	}, []graphstore.Node{{
		Kind: graphstore.KindInteraction, ID: run, Supertag: graphstore.KindInteraction,
		Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
		MadeAt: now, MadeBy: alice, SourceEvent: graphstore.NoEvent,
	}}, nil)
	asker := holdWrite(t, database, &lockstep.Hold{
		Name: "person alice's node row", Step: lockstep.StepNode, Taken: true,
		Kind: graphstore.KindPerson, ID: alice.ID, Statement: "insert into graph_nodes (",
	}, []graphstore.Node{graphstore.PersonNode(alice.ID, alice, now, graphstore.NoEvent)}, nil)

	report := followTheRun(t, database, "writeOwedRun", func(ctx context.Context, database *store.Store) (BackfillReport, error) {
		done, err := backfillRuns(ctx, database)
		return BackfillReport{Kinds: []KindBackfill{done}}, err
	}, []*lockstep.Hold{request, interaction, asker})
	if done := reportedKind(t, report, graphstore.KindInteraction); done.Rows != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for runs with no node, want 1 and the kind complete", done)
	}
	if node, held, err := graphstore.ReadNode(context.Background(), database.Pool, graphstore.KindInteraction, run); err != nil || !held ||
		node.MadeBy.ID != alice.ID || node.Visibility != graphstore.VisibilityAuthor {
		t.Errorf("the run reads node %+v (held %t, %v) after the backfill, want alice's, reserved for her: the run the test followed is not one that committed",
			node, held, err)
	}
	if values := runFrameValues(t, database, run); values != 1 {
		t.Errorf("the run holds %d frame values after the backfill, want its one frame", values)
	}
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
