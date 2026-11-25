package docs

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// rollSession is the agent session whose conversation the stand-in for the release before this
// one uploads a file to.
const rollSession = "ses_rolled_conversation"

// The release before this one writes an issue, a document, an ask and a message with no node, and
// a file it uploads to an agent's conversation appends no event at all. A row it writes while it
// serves beside this build gets its node from this build's next block-index tick, with no second
// backfill, and a tick with this build serving alone does not read those tables whole.
//
// The test drives the tick through the triggers on which its event read owes the object pass
// (readEventsAfterCursor), which writeObjectNodesAnotherBuildWrote runs. The process's first tick
// joins its build's cursor, and so writes the nodes of the issues the seed left without one. The
// tick after it finds this build serving alone after a tick that did too, so it reads nothing. The
// stand-in for the release before then writes, with no event, a conversation's file, an ask on
// CORE-1 and a direct message to a session. A tick with this build still serving alone leaves
// them without their nodes, which the project census names `missing`. With a process of that
// release registered, the next tick writes all three nodes, each in its project or between its
// parties, and the project census reads them complete.
//
// Its control is the tick's object pass taken out of ReconcileBlockIndex: the three rows stay
// `missing` after the roll's tick, and #3178's TestAVersionThePreviousReleaseWritesAfterTheDrainReachesTheStore
// fails the same way on the issue, the spec and the document the release before creates there.
func TestARowThePreviousReleaseWritesDuringARollGetsItsNodeFromTheTick(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	appendIssueEvent(t, database)

	joined := awaitedBlockIndexTick(t, service)
	if joined.AnotherBuild || joined.ObjectNodes != 2 {
		t.Fatalf("the process's first tick wrote %d object node(s) and found another build %t, want the nodes of CORE-1 and CORE-2 and no other build",
			joined.ObjectNodes, joined.AnotherBuild)
	}
	requireProjectComplete(t, database, nil, "after the process's first tick")
	if alone := awaitedBlockIndexTick(t, service); alone.ObjectNodes != 0 {
		t.Fatalf("the second tick with this build serving alone wrote %d object node(s), want none", alone.ObjectNodes)
	}

	file, ask, message := writeRowsAsThePreviousRelease(t, database)
	missing := []string{"artifact " + refKeyOf(t, database, file) + ": missing", "ask " + ask + ": missing", "message " + message + ": missing"}
	if alone := awaitedBlockIndexTick(t, service); alone.AnotherBuild || alone.ObjectNodes != 0 {
		t.Fatalf("a tick with this build serving alone wrote %d object node(s) and found another build %t, want none: the pass reads the four tables whole only on a roll's tick",
			alone.ObjectNodes, alone.AnotherBuild)
	}
	requireProjectComplete(t, database, missing, "after a tick with this build serving alone")

	registerProcess(t, database, "")
	registerProcess(t, database, previousReleaseCommit)
	roll := awaitedBlockIndexTick(t, service)
	if !roll.AnotherBuild || roll.ObjectNodes != 3 {
		t.Errorf("the tick with the release before serving wrote %d object node(s) and found another build %t, want the three nodes it left and another build",
			roll.ObjectNodes, roll.AnotherBuild)
	}
	requireProjectComplete(t, database, nil, "after the tick with the release before serving, with no second backfill")
	for _, want := range []struct {
		kind, id, project string
		parties           []string
	}{
		{graphstore.KindArtifact, file, "", []string{"session:" + rollSession}},
		{graphstore.KindAsk, ask, "CORE", nil},
		{graphstore.KindMessage, message, "", []string{"person:alice", "session:" + rollSession}},
	} {
		id := want.id
		if want.kind == graphstore.KindArtifact {
			id = refKeyOf(t, database, file)
		}
		node, held, err := graphstore.ReadNode(ctx, database.Pool, want.kind, id)
		if err != nil || !held || node.State != graphstore.StateLive || node.Project != want.project ||
			!slices.Equal(slices.Sorted(slices.Values(node.Parties)), want.parties) {
			t.Errorf("the node of %s %s is %+v (held %t, %v), want it live in project %q between %v", want.kind, id, node, held, err, want.project, want.parties)
		}
	}
}

// A row the release before this one creates while it holds the block-index lease gets its node
// from this build's next tick, though no tick of this build runs while that release serves.
//
// The two builds share one lease (BlockIndexLease), so a process of the release before whose ticks
// come first in each period takes every period while it serves (#3178's
// TestAVersionThePreviousReleaseWritesWhileItHoldsTheLeaseReachesTheStore). This build's next tick
// then finds it serving alone, after a tick that found it alone too, and is not its process's
// first: none of the roll's triggers holds. What that release left is in the events after the
// cursor: each create of an issue (with its spec), a document, an ask and a message appends its
// kind's create event (issue.created, artifact.created, ask.opened, message.created) in the
// transaction that writes the row. So the tick owes the object pass where an event after its
// cursor records the create of a row with no node (graphcensus's EventsOweObjectPass), and the
// pass writes those rows' nodes.
//
// block_index_event_cursors.another_build_seen is an extra trigger of the pass, never the one this
// cure depends on: the test forces it false on every cursor row before that tick, so the tick owes
// the pass on its events alone, and the project census then reads zero.
//
// Its control is the tick owing the pass only on the roll's triggers, as before (no
// EventsOweObjectPass): with another_build_seen forced false, the first tick after the release
// before left writes no node, and the project census names the five rows `missing`.
func TestARowThePreviousReleaseCreatesWhileItHoldsTheLeaseGetsItsNodeFromTheNextTick(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	appendIssueEvent(t, database)
	if joined := awaitedBlockIndexTick(t, service); joined.ObjectNodes != 2 {
		t.Fatalf("the process's first tick wrote %d object node(s), want the nodes of CORE-1 and CORE-2", joined.ObjectNodes)
	}
	if alone := awaitedBlockIndexTick(t, service); alone.AnotherBuild || alone.ObjectNodes != 0 {
		t.Fatalf("the second tick with this build serving alone wrote %d object node(s) and found another build %t, want neither",
			alone.ObjectNodes, alone.AnotherBuild)
	}
	requireProjectComplete(t, database, nil, "before the release before serves")

	// A process of the release before serves, takes every period of the lease, creates the rows and
	// leaves: no tick of this build runs meanwhile.
	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)
	created := createRowsAsThePreviousRelease(t, database)
	previousLeaves()

	forced, err := database.Pool.Exec(ctx, `update block_index_event_cursors set another_build_seen = false`)
	if err != nil || forced.RowsAffected() == 0 {
		t.Fatalf("force another_build_seen false on every cursor row: %d row(s), %v; want this build's cursor row at least", forced.RowsAffected(), err)
	}
	tick := awaitedBlockIndexTick(t, service)
	if tick.AnotherBuild || tick.ObjectNodes != len(created) {
		t.Errorf("the first tick after the release before left wrote %d object node(s) and found another build %t, want the %d nodes it left and no other build",
			tick.ObjectNodes, tick.AnotherBuild, len(created))
	}
	requireProjectComplete(t, database, nil, "after the first tick once the release before, which held the lease while it served, left, with no second backfill")
	for _, want := range created {
		node, held, err := graphstore.ReadNode(ctx, database.Pool, want.kind, want.id)
		if err != nil || !held || node.State != graphstore.StateLive || node.Project != want.project ||
			!slices.Equal(slices.Sorted(slices.Values(node.Parties)), want.parties) {
			t.Errorf("the node of %s %s is %+v (held %t, %v), want it live in project %q between %v", want.kind, want.id, node, held, err, want.project, want.parties)
		}
	}
	if next := awaitedBlockIndexTick(t, service); next.ObjectNodes != 0 {
		t.Errorf("the tick after that wrote %d object node(s), want none: every event after its cursor names a row that holds its node", next.ObjectNodes)
	}
}

// A row the release before this one writes with no event, a conversation's file above all, gets
// its node within objectPassInterval of this process's last pass, though no trigger of the roll
// holds and no event names it: a tick owes the object pass once that long has passed since the
// process's last pass finished.
//
// The test moves the service's clock. Within the interval the tick with this build serving alone
// writes nothing, and the project census names the three rows `missing`; once the interval has
// passed the next tick writes their nodes; and a file uploaded after that pass waits for the next
// interval, so the tick after it writes none and the census names that file.
//
// Its control is the tick owing the pass on the roll's triggers and the events alone: the three
// rows stay `missing` however far the clock moves.
func TestARowNoEventNamesGetsItsNodeOnceTheObjectPassIntervalPasses(t *testing.T) {
	database, service := blockIndexService(t)
	var aged atomic.Int64
	service.now = func() time.Time { return time.Now().Add(time.Duration(aged.Load())) }
	appendIssueEvent(t, database)
	if joined := awaitedBlockIndexTick(t, service); joined.ObjectNodes != 2 {
		t.Fatalf("the process's first tick wrote %d object node(s), want the nodes of CORE-1 and CORE-2", joined.ObjectNodes)
	}
	file, ask, message := writeRowsAsThePreviousRelease(t, database)
	missing := []string{"artifact " + refKeyOf(t, database, file) + ": missing", "ask " + ask + ": missing", "message " + message + ": missing"}
	if alone := awaitedBlockIndexTick(t, service); alone.ObjectNodes != 0 {
		t.Fatalf("a tick within the interval wrote %d object node(s), want none", alone.ObjectNodes)
	}
	requireProjectComplete(t, database, missing, "within the interval of the process's last pass")

	aged.Store(int64(objectPassInterval))
	if due := awaitedBlockIndexTick(t, service); due.AnotherBuild || due.ObjectNodes != 3 {
		t.Errorf("the tick once the interval passed wrote %d object node(s) and found another build %t, want the three rows' nodes and no other build",
			due.ObjectNodes, due.AnotherBuild)
	}
	requireProjectComplete(t, database, nil, "once the interval passed, with no second backfill")
	var later string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ($1, 'rolled-later-png', 'rolled-later.png', 'image', jsonb_build_object('kind', 'session', 'id', $1::text))
		returning ref_key
	`, rollSession).Scan(&later); err != nil {
		t.Fatalf("upload a second conversation file as the release before: %v", err)
	}
	if next := awaitedBlockIndexTick(t, service); next.ObjectNodes != 0 {
		t.Errorf("the tick after the interval's pass wrote %d object node(s), want none: the next pass is an interval away", next.ObjectNodes)
	}
	requireProjectComplete(t, database, []string{"artifact " + later + ": missing"}, "within the interval of the pass that wrote the three")
}

// createdRow is a row the stand-in for the release before this one created, at its node's
// address, with the project or the parties its node must name.
type createdRow struct {
	kind, id, project string
	parties           []string
}

// createRowsAsThePreviousRelease has the stand-in for the release before this one create, each
// with no node and in one transaction with its kind's create event, as that release's creates
// append them: issue CORE-3 with its spec (issue.created, the issue's own, naming no document), a
// document of CORE-1 (artifact.created, whose payload carries the artifact), an ask on CORE-1
// (ask.opened, whose payload is the ask) and a direct message from alice to rollSession
// (message.created, which no owner sequences). It answers the rows, at their nodes' addresses.
func createRowsAsThePreviousRelease(t *testing.T, database *store.Store) []createdRow {
	t.Helper()
	ctx := context.Background()
	var spec, document, ask, message string
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-3', 'CORE', 3, 'Created during the roll', '{"kind":"user","id":"alice"}', 'W')
		`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('CORE-3', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
			returning ref_key
		`).Scan(&spec); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify)
			values ('CORE-3', 1, 'issue.created', '{"kind":"user","id":"alice"}',
			        '{"key":"CORE-3","project":"CORE","title":"Created during the roll"}', false)
		`)
		return err
	}); err != nil {
		t.Fatalf("create issue CORE-3 and its spec as the release before: %v", err)
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('CORE-1', 'CORE', 'rolled', 'rolled.md', 'doc', false, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`).Scan(&id, &document); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify)
			select 'CORE-1', coalesce(max(seq), 0) + 1, 'artifact.created', '{"kind":"user","id":"alice"}',
			       jsonb_build_object('artifact', jsonb_build_object('id', $1::text, 'issue_key', 'CORE-1',
			           'project', 'CORE', 'slug', 'rolled', 'name', 'rolled.md', 'kind', 'doc')), false
			from events where issue_key = 'CORE-1'
		`, id)
		return err
	}); err != nil {
		t.Fatalf("upload a document of CORE-1 as the release before: %v", err)
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Created during the roll?')
			returning id::text
		`).Scan(&ask); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify)
			select 'CORE-1', coalesce(max(seq), 0) + 1, 'ask.opened', '{"kind":"user","id":"alice"}',
			       jsonb_build_object('id', $1::text, 'issue_key', 'CORE-1', 'question', 'Created during the roll?'), false
			from events where issue_key = 'CORE-1'
		`, ask)
		return err
	}); err != nil {
		t.Fatalf("open an ask on CORE-1 as the release before: %v", err)
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			insert into messages (issue_key, author, body, target) values (null, '{"kind":"user","id":"alice"}', 'During the roll.', 'session:' || $1)
			returning id::text
		`, rollSession).Scan(&message); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into events (seq, type, actor, payload, notify)
			values (0, 'message.created', '{"kind":"user","id":"alice"}',
			        jsonb_build_object('id', $1::text, 'body', 'During the roll.', 'target', 'session:' || $2::text), false)
		`, message, rollSession)
		return err
	}); err != nil {
		t.Fatalf("send a direct message as the release before: %v", err)
	}
	return []createdRow{
		{kind: graphstore.KindIssue, id: "CORE-3", project: "CORE"},
		{kind: graphstore.KindArtifact, id: spec, project: "CORE"},
		{kind: graphstore.KindArtifact, id: document, project: "CORE"},
		{kind: graphstore.KindAsk, id: ask, project: "CORE"},
		{kind: graphstore.KindMessage, id: message, parties: []string{"person:alice", "session:" + rollSession}},
	}
}

// writeRowsAsThePreviousRelease has the stand-in for the release before this one write, with no
// node and no event, a file uploaded to rollSession's conversation, an ask on CORE-1 and a direct
// message from alice to that session, and answers their ids.
func writeRowsAsThePreviousRelease(t *testing.T, database *store.Store) (file, ask, message string) {
	t.Helper()
	ctx := context.Background()
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ($1, 'rolled-png', 'rolled.png', 'image', jsonb_build_object('kind', 'session', 'id', $1::text))
		returning id::text
	`, rollSession).Scan(&file); err != nil {
		t.Fatalf("upload the conversation's file as the release before: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Does the roll keep it?')
		returning id::text
	`).Scan(&ask); err != nil {
		t.Fatalf("open the ask as the release before: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (issue_key, author, body, target) values (null, '{"kind":"user","id":"alice"}', 'Directly.', 'session:' || $1)
		returning id::text
	`, rollSession).Scan(&message); err != nil {
		t.Fatalf("send the direct message as the release before: %v", err)
	}
	return file, ask, message
}

// refKeyOf is the ref_key of the artifacts row id, the address of its node.
func refKeyOf(t *testing.T, database *store.Store, id string) string {
	t.Helper()
	var ref string
	if err := database.Pool.QueryRow(context.Background(), `select ref_key from artifacts where id = $1::uuid`, id).Scan(&ref); err != nil {
		t.Fatalf("read the address of artifact %s: %v", id, err)
	}
	return ref
}

// requireProjectComplete fails unless the project census names exactly the rows want (none: the
// census reads every row complete), each as `<kind> <id>: <class>`, when names the moment.
func requireProjectComplete(t *testing.T, database *store.Store, want []string, when string) {
	t.Helper()
	incomplete, err := graphcensus.ProjectComplete(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("take the project census %s: %v", when, err)
	}
	var named []string
	for _, line := range incomplete {
		for _, row := range want {
			if len(line) >= len(row) && line[:len(row)] == row {
				named = append(named, row)
			}
		}
	}
	if len(incomplete) != len(want) || len(named) != len(want) {
		t.Fatalf("the project census names %q %s, want exactly %q", incomplete, when, want)
	}
}

// requireTheRollTicksObjectPassLockOrder holds the block-index tick's object pass to the store's
// lock order at the tick's own call site (writeObjectNodesAnotherBuildWrote), as graphcensus's
// TestEveryBackfillBatchTakesTheStoreLockOrder holds writeObjectBatch under the backfill: the tick
// runs on the service's background pool (ReconcileBlockIndex), which hands its one connection to
// each statement and transaction in turn, so every statement of it goes through the backend the
// test reads from that pool, while a process of the release before serves. The pass takes the owner
// lock of the document the release before uploaded, then the document's node row, then appends its
// tuples under the document's own artifact.created event, and never the project's row. While it
// waits behind the owner lock it may hold no lock of the event read that runs before it in the same
// tick: the pass runs after the event read's transaction commits, so the cursor row and the
// document_block_versions rows the read's marking locks are let go of first.
//
// Its controls are the pass run inside the event read's transaction, which holds the cursor row
// while it waits behind the owner lock, and writeObjectBatch without its owner locks, which never
// queues behind the owner lock.
func requireTheRollTicksObjectPassLockOrder(t *testing.T) {
	database, _ := blockIndexService(t)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	now := time.Now()
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode("CORE-1", "CORE", alice, now), graphstore.IssueNode("CORE-2", "CORE", alice, now),
		}, nil)
	}); err != nil {
		t.Fatalf("write the issues' nodes: %v", err)
	}
	var document, ref string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'rolled', 'rolled.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`).Scan(&document, &ref); err != nil {
		t.Fatalf("upload the document as the release before: %v", err)
	}
	var created int64
	if err := database.Pool.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', coalesce(max(seq), 0) + 1, 'artifact.created', '{"kind":"user","id":"alice"}',
		       jsonb_build_object('artifact', jsonb_build_object('id', $1::text, 'issue_key', 'CORE-1')), false
		from events where issue_key = 'CORE-1'
		returning id
	`, document).Scan(&created); err != nil {
		t.Fatalf("append the upload's event as the release before: %v", err)
	}
	registerProcess(t, database, previousReleaseCommit)

	ticker := storetest.SingleConnection(t, database)
	service := blockIndexProcess(t, ticker, "")
	// The backend of the background pool's one connection in use: the tick takes it for each of
	// its statements and transactions in turn, none at the same time as another, and a pool hands
	// out the connection that came back last.
	var pid uint32
	if err := testBackgroundPool(t, service).QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the tick's backend: %v", err)
	}
	// The horizon is proven before the tick, so its event read starts this build's cursor and
	// takes its row, and the document_block_versions row the upload's event names, in a
	// transaction of its own: what the pass must let go of before it takes an owner lock. The
	// horizon observes through a pool of its own (blockIndexHorizon), so the one connection stays
	// the tick's.
	awaited, cancelAwait := context.WithTimeout(ctx, time.Minute)
	defer cancelAwait()
	if _, err := service.blockIndexHorizon().Await(awaited); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}

	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue CORE-1's owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"graph_nodes", "graph_tuples", "block_index_event_cursors", "document_block_versions"},
	}, store.IssueOwner("CORE-1"))
	node := lockstep.Hold{
		Name: ref + "'s node row", Step: lockstep.StepNode, Taken: true,
		Kind: graphstore.KindArtifact, ID: ref, Statement: "insert into graph_nodes (",
	}
	lockstep.Begin(t, database, &node)
	if err := graphstore.Write(ctx, node.Tx, []graphstore.Node{graphstore.ArtifactNode(ref, "CORE", "", alice, now)}, nil); err != nil {
		t.Fatalf("hold %s: %v", node.Name, err)
	}
	state := lockstep.Hold{Name: ref + "'s state tuple", Step: lockstep.StepTuple, Taken: true, Statement: "with written as ("}
	lockstep.Begin(t, database, &state)
	if err := graphstore.Write(ctx, state.Tx, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindArtifact, FromID: ref, Kind: graphstore.AttributeState,
		ToKind: graphstore.KindValue, ToID: graphstore.StateLive,
		At: now, Who: alice, SourceEvent: created, Asserted: true,
	}}); err != nil {
		t.Fatalf("hold %s: %v", state.Name, err)
	}
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project CORE's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "CORE")

	// A tick that waits on its own connection never answers on its own; its context ends it
	// after the holds are let go, so a failed run lets the test's pool close.
	running, stop := context.WithTimeout(ctx, 2*lockstep.Wait)
	defer stop()
	done := make(chan error, 1)
	var tick BlockIndexTick
	go func() {
		var err error
		tick, err = service.ReconcileBlockIndex(running, BlockIndexCursors{}, liveAheadDocuments{})
		done <- err
	}()
	lockstep.Follow(t, database, "the roll tick's object pass", pid, done, []*lockstep.Hold{owner, &node, &state, project})

	if !tick.AnotherBuild || tick.ObjectNodes != 1 {
		t.Errorf("the roll tick wrote %d object node(s) and found another build %t, want the document's node and another build", tick.ObjectNodes, tick.AnotherBuild)
	}
	// The event read's transaction ran and committed in this tick, holding the cursor row while
	// it did: it started the cursor at the horizon.
	if cursor := blockIndexEventCursor(t, database, ""); cursor == 0 {
		t.Errorf("the tick's event read left no cursor: the pass was not followed after the event read's transaction")
	}
	held, found, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, ref)
	if err != nil || !found || held.State != graphstore.StateLive || held.Project != "CORE" || held.SourceEvent != created {
		t.Errorf("the node of %s is %+v (held %t, %v), want it live in CORE under event %d: the run the test followed is not one that committed",
			ref, held, found, err, created)
	}
}
