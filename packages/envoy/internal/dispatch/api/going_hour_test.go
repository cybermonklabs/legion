package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// laterRunSource is the source a later release's History turn states of its interaction node,
// which no going_requests row projects and which the sweep never states gone at one hour.
const laterRunSource = "going_runs"

// requireRunSource fails unless the interaction node of run holds the source going_requests, both
// as its attribute and as its one source value tuple, read straight from graph_nodes and
// graph_tuples, naming the run and the values it read. when says at which point of the test.
func requireRunSource(t *testing.T, database *store.Store, run, when string) {
	t.Helper()
	var attribute *string
	var values []string
	if err := database.Pool.QueryRow(context.Background(), `
		select (select n.attributes->>$3 from graph_nodes n where n.kind = $1 and n.id = $2),
		       array(select t.to_id from graph_tuples t
		             where t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.to_kind = $4 order by t.seq)
	`, graphstore.KindInteraction, run, graphstore.AttributeSource, graphstore.KindValue).Scan(&attribute, &values); err != nil {
		t.Fatalf("%s, read the source of run %s: %v", when, run, err)
	}
	read := "no source attribute"
	if attribute != nil {
		read = fmt.Sprintf("the source attribute %q", *attribute)
	}
	if attribute == nil || *attribute != graphstore.SourceGoingRequests || !slices.Equal(values, []string{graphstore.SourceGoingRequests}) {
		t.Fatalf("%s, the interaction node of run %s holds %s and the source value tuples %q, want the source %q, as its attribute and as one value tuple",
			when, run, read, values, graphstore.SourceGoingRequests)
	}
}

// tuplesOutOfRun is how many tuples of kind graph_tuples holds out of the run's interaction node,
// read straight from the table, whatever their state.
func tuplesOutOfRun(t *testing.T, database *store.Store, run, kind string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3
	`, graphstore.KindInteraction, run, kind).Scan(&count); err != nil {
		t.Fatalf("count the %s tuples out of run %s: %v", kind, run, err)
	}
	return count
}

// runRows is how many going_requests rows and going_events rows the record holds of the run.
func runRows(t *testing.T, database *store.Store, run string) (int, int) {
	t.Helper()
	var requests, events int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from going_requests where id = $1::uuid),
		       (select count(*) from going_events where request_id = $1::uuid)
	`, run).Scan(&requests, &events); err != nil {
		t.Fatalf("count the rows of run %s: %v", run, err)
	}
	return requests, events
}

// TestARunIsGoneAfterOneHour is c-run-kept: a run is readable for one hour, and then the sweep
// states its node gone and removes its rows, while the node, its words and its frames stay in the
// store. It records three runs of one person, each with one frame of its own. While the first is
// live, a comment written under its header changes one node, so a changed_by_run tuple points from
// that node to the run. The first and the third are dated an hour and a minute back by their
// created_at, and their ends_at and their nodes' made_at with them, and the second fifty-nine
// minutes back, which leaves it past its ends_at; the third then has its rows deleted, as a process
// of the release before deletes a run an hour old. Beside them stands an interaction node of the
// same person of a later release's source, going_runs, dated an hour and a minute back, which no
// row projects. Every run's node holds the source going_requests before the sweep and after it.
//
// One pass of the sweep then runs. The first run's node reads gone: its address answers it absent
// to its author, as its stream does, the author's references of the node it changed no longer name
// it, and its going_requests and going_events rows are gone, while its node row, its words value,
// its frame values - its own frame and the deadline's ending the sweep stored before the hour - and
// the changed_by_run into it stay in the store. The second run is still live, read by its author at
// its address and through its stream, which carries its frame and the ending the sweep stored, with
// its rows, and read by nobody else either way. The third run's node reads gone though the sweep
// removed none of its rows, and the later release's node stays live.
func TestARunIsGoneAfterOneHour(t *testing.T) {
	t.Parallel()
	routes := newGoingRoutesRig(t, nil)
	rig := goingRig{server: routes.server, database: routes.database}
	ctx := context.Background()
	pool := rig.database.Pool
	author, other := goingAsker.ID, "bob"
	issue := createTestIssue(t, routes.handler, "HOUR", "The issue a run writes on")

	record := func(words string) goingRequest {
		t.Helper()
		run, err := rig.server.recordGoingRequest(ctx, goingAsker, "", words)
		if err != nil {
			t.Fatalf("record the run asking %q: %v", words, err)
		}
		if _, err := appendGoingEvent(ctx, pool, run.ID, going.Text("Half an ")); err != nil {
			t.Fatalf("append the frame of run %s: %v", run.ID, err)
		}
		return run
	}
	// dateBack moves the run's created_at, its ends_at and its node's made_at back by the same
	// span, as a run asked that long ago reads.
	dateBack := func(run goingRequest, by time.Duration) {
		t.Helper()
		rig.age(t, run.ID, by, by-contracts.GoingRunWindow)
		if _, err := pool.Exec(ctx, `update graph_nodes set made_at = now() - $3::interval where kind = $1 and id = $2`,
			graphstore.KindInteraction, run.ID, by.String()); err != nil {
			t.Fatalf("date the node of run %s back %v: %v", run.ID, by, err)
		}
	}
	interactionRef := func(run string) string { return "dispatch://interaction/" + run }
	// readsAt answers what the run's address and the run's stream each answer login, as the status
	// and the error code: both are reads of the run, and a run no read shows answers each with the
	// route's own not-found (addressGone, streamGone), not some other 404.
	readsAt := func(run, login string) (string, string) {
		t.Helper()
		address := referencesAt(t, routes.handler, "from", interactionRef(run), login)
		stream := routes.stream(t, run, login, "")
		return fmt.Sprintf("%d %s", address.Code, responseCode(t, address)), fmt.Sprintf("%d %s", stream.Code, responseCode(t, stream))
	}
	addressGone, streamGone := "404 NOT_FOUND", "404 "+codeGoingRequestNotFound
	// namesRun reports whether login's references out of ref name the run.
	namesRun := func(ref, login, run string) bool {
		t.Helper()
		response := referencesAt(t, routes.handler, "from", ref, login)
		if response.Code != http.StatusOK {
			t.Fatalf("%s's references out of %s answer %d %s", login, ref, response.Code, response.Body.String())
		}
		return slices.ContainsFunc(decodeBody[model.GraphReferences](t, response).Edges, func(edge model.GraphEdge) bool {
			return edge.Node.Kind == graphstore.KindInteraction && edge.Node.ID == run
		})
	}

	gone := record("asked an hour and a minute ago")
	written := requestForRun(t, routes.handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		map[string]any{"body": "The run's comment on the issue."}, author, gone.ID)
	if written.Code != http.StatusCreated {
		t.Fatalf("write the comment under run %s's header: status=%d body=%s", gone.ID, written.Code, written.Body.String())
	}
	comment := decodeBody[writtenRecord](t, written).ID
	commentRef := "dispatch://" + issue + "/comment/" + comment
	if count := changedByRunCount(t, rig.database, graphstore.KindComment, comment, gone.ID); count != 1 {
		t.Fatalf("the comment written under run %s holds %d changed_by_run tuples to it, want one", gone.ID, count)
	}
	live := record("asked fifty-nine minutes ago")
	rowless := record("asked an hour and a minute ago, its rows deleted by a process of the release before")
	dateBack(gone, time.Hour+time.Minute)
	dateBack(live, 59*time.Minute)
	dateBack(rowless, time.Hour+time.Minute)
	if _, err := pool.Exec(ctx, `delete from going_requests where id = $1::uuid`, rowless.ID); err != nil {
		t.Fatalf("delete the rows of run %s as a process of the release before does: %v", rowless.ID, err)
	}

	asked := time.Now().Add(-time.Hour - time.Minute).UTC()
	later := graphstore.Interaction{
		ID: uuid.NewString(), Author: goingAsker.Recorded(), Words: "a History turn of a later release",
		At: asked, EndsAt: asked.Add(contracts.GoingRunWindow),
	}
	nodes, tuples := graphstore.InteractionFacts(later)
	nodes[0].Attributes[graphstore.AttributeSource] = laterRunSource
	for index := range tuples {
		if tuples[index].Kind == graphstore.AttributeSource {
			tuples[index].ToID = laterRunSource
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the later release's node: %v", err)
	}
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		t.Fatalf("write the later release's interaction node %s: %v", later.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the later release's interaction node %s: %v", later.ID, err)
	}

	// Before the sweep each run is read at its address alone: a read of its stream would end the
	// run itself, past its ends_at, and store the ending the sweep is to store.
	for _, run := range []goingRequest{gone, live, rowless} {
		requireRunSource(t, rig.database, run.ID, "before the sweep")
		if address := referencesAt(t, routes.handler, "from", interactionRef(run.ID), author).Code; address != http.StatusOK {
			t.Fatalf("before the sweep, run %s's address answers its author %d, want it read", run.ID, address)
		}
	}
	if !namesRun(commentRef, author, gone.ID) {
		t.Fatalf("before the sweep, the author's references of the comment %s name no run %s", comment, gone.ID)
	}
	if requests, events := runRows(t, rig.database, gone.ID); requests != 1 || events != 1 {
		t.Fatalf("before the sweep, run %s holds %d going_requests and %d going_events rows, want one of each", gone.ID, requests, events)
	}
	ownFrame := runFrameValues(t, rig.database, gone.ID)
	if len(ownFrame) != 1 {
		t.Fatalf("before the sweep, run %s holds the frame values %+v, want its own one", gone.ID, ownFrame)
	}

	if err := sweepGoingRequests(ctx, pool); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// The run asked an hour and a minute ago: gone at every read, its rows removed, its node, its
	// words, its frames and the changed_by_run into it kept.
	if node, held, err := graphstore.ReadNode(ctx, pool, graphstore.KindInteraction, gone.ID); err != nil || !held || node.State != graphstore.StateGone {
		t.Errorf("run %s, asked an hour and a minute ago, reads its node row held %t at state %q (%v) after the sweep, want its node row standing at state gone",
			gone.ID, held, node.State, err)
	}
	if address, stream := readsAt(gone.ID, author); address != addressGone || stream != streamGone {
		t.Errorf("run %s, asked an hour and a minute ago, still reads after the sweep: its address answers its author %q and its stream %q, want %q and %q",
			gone.ID, address, stream, addressGone, streamGone)
	}
	if namesRun(commentRef, author, gone.ID) {
		t.Errorf("after the sweep, the author's references of the comment %s still name run %s, which is gone", comment, gone.ID)
	}
	if requests, events := runRows(t, rig.database, gone.ID); requests != 0 || events != 0 {
		t.Errorf("run %s, asked an hour and a minute ago, holds %d going_requests and %d going_events rows after the sweep, want none",
			gone.ID, requests, events)
	}
	var words []string
	if err := pool.QueryRow(ctx, `
		select array(select to_id from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 order by seq)
	`, graphstore.KindInteraction, gone.ID, graphstore.AttributeWords, graphstore.KindValue).Scan(&words); err != nil {
		t.Fatalf("read the words of run %s: %v", gone.ID, err)
	}
	if !slices.Equal(words, []string{gone.Words}) {
		t.Errorf("run %s holds the words value tuples %q after the sweep, want its words %q kept", gone.ID, words, gone.Words)
	}
	frames := runFrameValues(t, rig.database, gone.ID)
	if len(frames) != 2 || frames[0].Sequence != ownFrame[0].Sequence || frames[0].Name != ownFrame[0].Name ||
		frames[1].Name != contracts.GoingEventError {
		t.Errorf("run %s holds the frame values %+v after the sweep, want its own frame %+v kept and the deadline's ending the sweep stored",
			gone.ID, frames, ownFrame[0])
	}
	if count := tuplesOutOfRun(t, rig.database, gone.ID, graphstore.AttributeFrame); count != 2 {
		t.Errorf("graph_tuples holds %d frame tuples of run %s after the sweep, want its two kept", count, gone.ID)
	}
	if count := changedByRunCount(t, rig.database, graphstore.KindComment, comment, gone.ID); count != 1 {
		t.Errorf("the comment %s holds %d changed_by_run tuples to run %s after the sweep, want its one kept", comment, count, gone.ID)
	}
	requireRunSource(t, rig.database, gone.ID, "after the sweep")

	// The run asked fifty-nine minutes ago: live, with its rows, read by its author alone.
	if node, held, err := graphstore.ReadNode(ctx, pool, graphstore.KindInteraction, live.ID); err != nil || !held || node.State != graphstore.StateLive {
		t.Errorf("run %s, asked fifty-nine minutes ago, reads its node held %t at state %q (%v) after the sweep, want it live",
			live.ID, held, node.State, err)
	}
	if requests, events := runRows(t, rig.database, live.ID); requests != 1 || events != 2 {
		t.Errorf("run %s, asked fifty-nine minutes ago, holds %d going_requests and %d going_events rows after the sweep, want its row, its frame and the deadline's ending",
			live.ID, requests, events)
	}
	if address := referencesAt(t, routes.handler, "from", interactionRef(live.ID), author).Code; address != http.StatusOK {
		t.Errorf("run %s, asked fifty-nine minutes ago, answers its author %d at its address after the sweep, want it read", live.ID, address)
	}
	streamed := routes.stream(t, live.ID, author, "")
	wantStream := fmt.Sprintf("id: 1\nevent: %s\ndata: {\"text\":\"Half an \"}\n\nid: 2\nevent: %s\ndata: {\"code\":\"%s\"}\n\n",
		going.Text("").Name, contracts.GoingEventError, contracts.GoingErrorAnswerDidNotArrive)
	if streamed.Code != http.StatusOK || streamed.Body.String() != wantStream {
		t.Errorf("run %s, asked fifty-nine minutes ago, streams its author %d %q after the sweep, want 200 and %q",
			live.ID, streamed.Code, streamed.Body.String(), wantStream)
	}
	if address, stream := readsAt(live.ID, other); address != addressGone || stream != streamGone {
		t.Errorf("run %s, asked fifty-nine minutes ago, answers %s %q at its address and %q through its stream after the sweep, want %q and %q: it is its author's alone",
			live.ID, other, address, stream, addressGone, streamGone)
	}
	requireRunSource(t, rig.database, live.ID, "after the sweep")

	// The run whose rows a process of the release before deleted: its node gone all the same.
	if node, held, err := graphstore.ReadNode(ctx, pool, graphstore.KindInteraction, rowless.ID); err != nil || !held || node.State != graphstore.StateGone {
		t.Errorf("run %s, asked an hour and a minute ago and its rows deleted before the sweep, reads its node row held %t at state %q (%v) after the sweep, want its node row standing at state gone",
			rowless.ID, held, node.State, err)
	}
	if address, _ := readsAt(rowless.ID, author); address != addressGone {
		t.Errorf("run %s, its rows deleted before the sweep, still reads after the sweep: its address answers its author %q, want %q",
			rowless.ID, address, addressGone)
	}
	if count := tuplesOutOfRun(t, rig.database, rowless.ID, graphstore.AttributeWords); count != 1 {
		t.Errorf("graph_tuples holds %d words tuples of run %s after the sweep, want its one kept", count, rowless.ID)
	}
	requireRunSource(t, rig.database, rowless.ID, "after the sweep")

	// The later release's node, of another source, is never swept at one hour.
	if node, held, err := graphstore.ReadNode(ctx, pool, graphstore.KindInteraction, later.ID); err != nil || !held || node.State != graphstore.StateLive {
		t.Errorf("the interaction node %s of the source %s, made an hour and a minute ago, reads held %t at state %q (%v) after the sweep, want it live",
			later.ID, laterRunSource, held, node.State, err)
	}
}

// TestTheSweepReadsOfTheRunsPastTheHourAreBounded holds the sweep's two reads of the runs past
// the hour (goRunsPastTheHour) to reading what they want and no more, under the plan Postgres
// builds for the values a pass sends and under the generic plan a prepared statement settles on.
// The read of the interaction nodes whose row is gone (goingRowlessRuns) reads graph_nodes
// through graph_nodes_interaction_live (migration 20261122230407) alone, keyed on made_at, and
// never by a scan of the table: the gone nodes grow with every run asked, and that index holds the
// nodes of the source going_requests not gone, about an hour of runs. The read of the runs whose
// row is past the hour (goingRunsPastTheHour) reads going_requests, which holds about an hour of
// runs, and probes graph_nodes for each run's node, never a scan of it: every scan of graph_nodes
// in its plan carries an index condition keyed by the run's id. Both plans are built with
// sequential scans off, under which a read of every node is an index scan and not a sequential
// one, so that condition is what tells a probe from a scan.
func TestTheSweepReadsOfTheRunsPastTheHourAreBounded(t *testing.T) {
	t.Parallel()
	rig := newGoingRig(t)
	ctx := context.Background()
	const index = "graph_nodes_interaction_live"
	connection, err := rig.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the generic plans: %v", err)
	}
	defer connection.Release()
	keys := explaintest.NewIndexKeys(rig.database.Pool)
	hour := contracts.GoingRequestTTL.Seconds()
	literals := []string{fmt.Sprint(hour), fmt.Sprint(goingGoneBatch)}
	for _, read := range []struct {
		name, prepared, statement string
		keyed                     bool
	}{
		{"the read of the runs past the hour", "going_runs_past_the_hour", goingRunsPastTheHour, false},
		{"the read of the runs whose row is gone", "going_rowless_runs", goingRowlessRuns, true},
	} {
		custom := planWithoutSeqScans(t, rig.database, read.statement, hour, goingGoneBatch)
		generic := explaintest.GenericPlan(t, ctx, connection, read.prepared, read.statement,
			[]string{"double precision", "bigint"}, literals, "set local enable_seqscan = off")
		for _, planned := range []struct {
			name string
			plan json.RawMessage
		}{{read.name, custom}, {read.name + " under a generic plan", generic}} {
			if explaintest.SeqScansRelation(t, planned.plan, "graph_nodes") {
				t.Errorf("%s scans graph_nodes:\n%s", planned.name, planned.plan)
			}
			if !read.keyed {
				probed := false
				for _, scan := range explaintest.IndexScans(t, planned.plan) {
					if scan.Relation != "graph_nodes" {
						continue
					}
					probed = true
					if !strings.Contains(scan.Cond, "r.id") {
						t.Errorf("%s reads %s on `%s`, want a probe of the run's node keyed by the run's id:\n%s",
							planned.name, scan, scan.Cond, planned.plan)
					}
				}
				if !probed {
					t.Errorf("%s probes graph_nodes nowhere:\n%s", planned.name, planned.plan)
				}
				continue
			}
			keys.RequireIndexKeyed(t, planned.plan, index, planned.name, explaintest.CondRequired)
			for _, scan := range explaintest.IndexScans(t, planned.plan) {
				if scan.Index == index && !strings.Contains(scan.Cond, "made_at <") {
					t.Errorf("%s reads %s on `%s`, want its bound of an hour back on made_at in the index condition:\n%s",
						planned.name, scan, scan.Cond, planned.plan)
				}
			}
		}
	}
}
