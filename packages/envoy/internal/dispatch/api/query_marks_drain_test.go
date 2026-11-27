package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// drainProbe is the document service TestIssueQueryMarksDrainAfterCommit serves: the real one,
// whose drain a request runs (DrainOwedQueries) it holds at both of its boundaries, on the
// request's own goroutine. before runs once the request's own transaction has committed and before
// the drain begins; after runs once the drain has committed and before the route publishes. While
// suppress is set a request drains nothing, so the mark its write left waits for the block-index
// tick. drains counts the drains requests ran.
type drainProbe struct {
	*docs.Service
	suppress atomic.Bool
	drains   atomic.Int32
	before   func(issueKey string)
	after    func(issueKey string, drained []model.Event)
}

func (p *drainProbe) DrainOwedQueries(ctx context.Context, issueKey string) []model.Event {
	if p.suppress.Load() {
		return nil
	}
	p.drains.Add(1)
	if p.before != nil {
		p.before(issueKey)
	}
	drained := p.Service.DrainOwedQueries(ctx, issueKey)
	if p.after != nil {
		p.after(issueKey, drained)
	}
	return drained
}

// newDrainProbeServer is a server whose document service is a drainProbe, on a store whose every
// statement it runs is recorded in sent (storetest.SentStore), with one broker beside both. The
// database it answers is the store's own, unrecorded, which the test reads committed rows through
// on connections of its own.
func newDrainProbeServer(t *testing.T) (http.Handler, *store.Store, Deps, *drainProbe, *storetest.SentStatements) {
	t.Helper()
	database := storetest.Open(t)
	sent := &storetest.SentStatements{}
	served := storetest.SentStore(t, database, sent)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: served, Events: broker, ServerURL: testServerURL, Settle: time.Hour})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the document service down: %v", err)
		}
	})
	seedPeople(t, database, "alice", "bob")
	probe := &drainProbe{Service: documents}
	deps, err := NewDeps(DepsInput{
		Store: served, Identity: headerIdentity(served), AgentTokens: sharedAgentTokens(t, "agent-token"),
		Docs: probe, Events: broker, ServerURL: testServerURL,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	srv := register(mux, t.Context(), deps)
	t.Cleanup(srv.waitBackground)
	return mux, database, deps, probe, sent
}

// queryMarkRow is an issue's row of issue_query_marks as the test reads it, with the writer of the
// tuple at its greatest_seq, which the drain names its versions after.
type queryMarkRow struct {
	event  *int64
	seq    int64
	writer model.Actor
}

// readQueryMarkRow reads the mark of issueKey, and whether one stands.
func readQueryMarkRow(t *testing.T, database *store.Store, issueKey string) (queryMarkRow, bool) {
	t.Helper()
	var mark queryMarkRow
	err := database.Pool.QueryRow(context.Background(), `
		select m.greatest_event, m.greatest_seq, coalesce(g.who, '{}'::jsonb)
		from issue_query_marks m left join graph_tuples g on g.seq = m.greatest_seq
		where m.issue_key = $1`, issueKey).Scan(&mark.event, &mark.seq, &mark.writer)
	if errors.Is(err, pgx.ErrNoRows) {
		return queryMarkRow{}, false
	}
	if err != nil {
		t.Fatalf("read the query mark of %s: %v", issueKey, err)
	}
	return mark, true
}

// requireMarkCauses fails unless the mark of issueKey stands with event as its greatest_event and
// seq as its greatest_seq, the tuple there written by writer; what names the write that left it.
func requireMarkCauses(t *testing.T, database *store.Store, what, issueKey string, event, seq int64, writer string) {
	t.Helper()
	mark, held := readQueryMarkRow(t, database, issueKey)
	if !held {
		t.Errorf("%s: no issue_query_marks row of %s stands after its COMMIT", what, issueKey)
		return
	}
	if mark.event == nil || *mark.event != event {
		t.Errorf("%s mark cause event %v, want %d", what, mark.event, event)
	}
	if mark.seq != seq {
		t.Errorf("%s mark cause sequence %d, want %d", what, mark.seq, seq)
	}
	if mark.writer.ID != writer {
		t.Errorf("%s mark writer %+v, want %s", what, mark.writer, writer)
	}
}

// requireNoQueryMark fails while a mark of issueKey stands; what says when it was read.
func requireNoQueryMark(t *testing.T, database *store.Store, what, issueKey string) {
	t.Helper()
	if mark, held := readQueryMarkRow(t, database, issueKey); held {
		t.Errorf("issue_query_marks still owed %s: %s holds greatest_event %v, greatest_seq %d", what, issueKey, mark.event, mark.seq)
	}
}

// greatestSeqOf is the greatest graph_tuples.seq of the tuples event caused.
func greatestSeqOf(t *testing.T, database *store.Store, event int64) int64 {
	t.Helper()
	var seq int64
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(max(seq), 0) from graph_tuples where source_event = $1`, event).Scan(&seq); err != nil {
		t.Fatalf("read the tuples event %d caused: %v", event, err)
	}
	if seq == 0 {
		t.Fatalf("event %d caused no tuple", event)
	}
	return seq
}

// documentEventOf is the id and the actor of the first event of type the log holds for version
// number of the document artifactID, and whether it holds one; a failed read fails t, never reading
// as no event.
func documentEventOf(t *testing.T, database *store.Store, eventType, artifactID string, number int) (int64, model.Actor, bool) {
	t.Helper()
	var id int64
	var actor model.Actor
	err := database.Pool.QueryRow(context.Background(), `
		select id, actor from events
		where type = $1 and payload->>'artifact_id' = $2
		  and coalesce((payload->'version'->>'number')::int, (payload->>'version')::int) = $3
		order by id`, eventType, artifactID, number).Scan(&id, &actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, model.Actor{}, false
	}
	if err != nil {
		t.Fatalf("read the %s of version %d of %s: %v", eventType, number, artifactID, err)
	}
	return id, actor, true
}

// requireVersionEvent fails unless the log holds the artifact.version of version number of the
// document artifactID, and answers its id and its actor.
func requireVersionEvent(t *testing.T, database *store.Store, artifactID string, number int) (int64, model.Actor) {
	t.Helper()
	id, actor, held := documentEventOf(t, database, "artifact.version", artifactID, number)
	if !held {
		t.Errorf("drained version %d has no artifact.version event", number)
	}
	return id, actor
}

// requireNamedBy fails unless version number of the document artifactID, whose ref_key is refKey,
// names want, which wanted describes, as its one author, as its node's maker and as the actor of
// its artifact.version, and unless none of the three names one of never, each a writer the drain
// must not name, for the reason why (R7).
func requireNamedBy(t *testing.T, database *store.Store, what, artifactID, refKey string, number int, want model.Actor, wanted, why string, never ...string) {
	t.Helper()
	named := func(where string, actor model.Actor) {
		if actor.Kind != want.Kind || actor.ID != want.ID {
			t.Errorf("%s: %s names %+v, want %s %s, %s", what, where, actor, want.Kind, want.ID, wanted)
		}
		if slices.Contains(never, actor.ID) {
			t.Errorf("%s: %s names %s, %s", what, where, actor.ID, why)
		}
	}
	authors := versionOf(t, database, artifactID, number).Authors
	if len(authors) != 1 {
		t.Errorf("%s: version %d of %s is written by %+v, want %s %s alone, %s", what, number, refKey, authors, want.Kind, want.ID, wanted)
	}
	for _, author := range authors {
		named(fmt.Sprintf("version %d of %s's author", number, refKey), author)
	}
	var maker model.Actor
	if err := database.Pool.QueryRow(context.Background(), `select made_by from graph_nodes where kind = $1 and id = $2`,
		graphstore.KindVersion, graphstore.VersionID(refKey, number)).Scan(&maker); err != nil {
		t.Fatalf("%s: read the node of version %d of %s: %v", what, number, refKey, err)
	}
	named(fmt.Sprintf("the node of version %d of %s", number, refKey), maker)
	_, actor := requireVersionEvent(t, database, artifactID, number)
	named(fmt.Sprintf("the artifact.version of version %d of %s", number, refKey), actor)
}

// requireNamedByTheTick fails unless version number of the document artifactID, whose ref_key is
// refKey, names the tick's drain's own system actor (docs.QueryDrainActor, kind system) as its one
// author, as its node's maker and as the actor of its artifact.version, and unless none of the
// three names one of writers, the people whose writes left the marks the tick drained (R7).
func requireNamedByTheTick(t *testing.T, database *store.Store, what, artifactID, refKey string, number int, writers ...string) {
	t.Helper()
	if docs.QueryDrainActor.Kind != "system" {
		t.Errorf("docs.QueryDrainActor is %+v, want an actor of kind system", docs.QueryDrainActor)
	}
	requireNamedBy(t, database, what, artifactID, refKey, number, docs.QueryDrainActor, "the drain's system actor",
		"whose request wrote a mark the tick drained; the tick's drain names no request writer", writers...)
}

// requestedVersionOf is the version the approval request askID names of the document artifactID.
func requestedVersionOf(t *testing.T, database *store.Store, askID, artifactID string) int {
	t.Helper()
	var version int
	if err := database.Pool.QueryRow(context.Background(), `
		select (d->>'version')::int from asks a, jsonb_array_elements(a.approval->'documents') d
		where a.id = $1 and d->>'artifact_id' = $2`, askID, artifactID).Scan(&version); err != nil {
		t.Fatalf("read the version the approval request %s names of %s: %v", askID, artifactID, err)
	}
	return version
}

// publishedSince is every event the subscription received and holds now, in the order it received
// them.
func publishedSince(published <-chan model.Event) []model.Event {
	var received []model.Event
	for {
		select {
		case event, open := <-published:
			if !open {
				return received
			}
			received = append(received, event)
		default:
			return received
		}
	}
}

// The heads of the statements the trace of a result's write and its drain is read by.
const (
	markUpsertHead   = "insert into issue_query_marks"
	markReadHead     = "select m.greatest_event, m.greatest_seq, g.who from issue_query_marks m"
	markClearHead    = "delete from issue_query_marks"
	ownerLockHead    = "public.owner_lock('exclusive'"
	roomLockHead     = "pg_advisory_xact_lock(hashtext(t.artifact_id::text)) from issue_trees t"
	versionInsertTag = "insert into artifact_versions"
)

// requireDrainTrace holds the statements the server sent for one write whose queries a drain
// follows, from the write's first on (sent), to the shape R7 and R12 give it: the write's batch
// upserts the mark, and no statement of the write's own transaction, up to its COMMIT, inserts a
// version of drained, the query document the drain revises (a DELETE inserts versions of the
// documents whose text it changes, which are its own); after that COMMIT the drain takes the
// issue's owner lock, then its room's lock, reads the mark, and only then inserts drained's version
// and clears the mark, and commits.
func requireDrainTrace(t *testing.T, what, drained string, sent []storetest.SentStatement) {
	t.Helper()
	trace := newDrainTrace(sent)
	mark := trace.at(0, trace.containing(markUpsertHead))
	if mark < 0 {
		t.Errorf("%s: the write's batch upserts no issue_query_marks row", what)
		return
	}
	written := trace.at(mark, trace.commit)
	if written < 0 {
		t.Errorf("%s: no COMMIT follows the mark's upsert", what)
		return
	}
	if inserted := trace.at(0, trace.inserts(drained)); inserted >= 0 && inserted < written {
		t.Errorf("%s: the write's own transaction inserts the drained version of %s (statement %d) before its COMMIT (statement %d)", what, drained, inserted, written)
	}
	trace.require(t, what+": after the write's COMMIT the drain", written+1, drained)
}

// requireFillCommitsBeforeTheDrain holds the statements the server sent from the first of a write
// whose drain meets a record digest left owed (sent) to R7's order: after the write's COMMIT the
// drain's fill records the digest and commits in a transaction of its own, and only after that
// COMMIT does the drain take the issue's owner lock, then its room's lock, read the mark, insert
// drained's version, clear the mark and commit; no statement of the drain's own transaction - its
// room's lock, its read of the mark - comes before the fill's COMMIT.
func requireFillCommitsBeforeTheDrain(t *testing.T, what, drained string, sent []storetest.SentStatement) {
	t.Helper()
	trace := newDrainTrace(sent)
	mark := trace.at(0, trace.containing(markUpsertHead))
	written := trace.at(max(mark, 0), trace.commit)
	if mark < 0 || written < 0 {
		t.Errorf("%s: the write's batch upserts no issue_query_marks row (statement %d) or no COMMIT follows it (statement %d)", what, mark, written)
		return
	}
	fill := trace.at(written+1, trace.containing(sqlFillsDigest))
	if fill < 0 {
		t.Errorf("%s: after the write's COMMIT no fill records the owed digest", what)
		return
	}
	filled := trace.at(fill, trace.commit)
	if filled < 0 {
		t.Errorf("%s: no COMMIT follows the fill's record of the digest (statement %d)", what, fill)
		return
	}
	t.Logf("%s: the fill records the digest at statement %d and commits at %d", what, fill, filled)
	if owner := trace.at(written+1, trace.containing(ownerLockHead)); owner < 0 || owner > fill {
		t.Errorf("%s: the fill recorded the digest (statement %d) with no owner lock taken before it (first after the write's COMMIT: statement %d)", what, fill, owner)
	}
	for _, step := range []struct{ name, head string }{{"the lock of the issue's room", roomLockHead}, {"the read of the mark", markReadHead}} {
		if early := trace.at(written+1, trace.containing(step.head)); early >= 0 && early < filled {
			t.Errorf("%s: the drain sent %s (statement %d) before the fill's COMMIT (statement %d)", what, step.name, early, filled)
		}
	}
	trace.require(t, what+": after the fill's COMMIT the drain", filled+1, drained)
}

// drainTrace is the statements a trace check reads (storetest.SentStatements), each lowered and
// with its whitespace folded (normalizedStatement).
type drainTrace struct {
	sent   []storetest.SentStatement
	shapes []string
}

func newDrainTrace(sent []storetest.SentStatement) drainTrace {
	shapes := make([]string, len(sent))
	for index, statement := range sent {
		shapes[index] = normalizedStatement(statement.SQL)
	}
	return drainTrace{sent: sent, shapes: shapes}
}

// at is the index of the first statement from from on that match holds, -1 for none.
func (d drainTrace) at(from int, match func(int) bool) int {
	for index := from; index < len(d.shapes); index++ {
		if match(index) {
			return index
		}
	}
	return -1
}

func (d drainTrace) containing(head string) func(int) bool {
	return func(index int) bool { return strings.Contains(d.shapes[index], head) }
}

func (d drainTrace) commit(index int) bool { return d.shapes[index] == "commit" }

// inserts matches the insert of a version of the document artifactID, its first bound value.
func (d drainTrace) inserts(artifactID string) func(int) bool {
	return func(index int) bool {
		if !strings.Contains(d.shapes[index], versionInsertTag) {
			return false
		}
		inserted, held := d.sent[index].Text(0)
		return held && inserted == artifactID
	}
}

// require fails t unless, from from on, the drain sends in its order the issue's owner lock, its
// room's lock, the read of the mark, the insert of drained's version, the clear of the mark and
// its COMMIT; what names the drain and where it starts.
func (d drainTrace) require(t *testing.T, what string, from int, drained string) {
	t.Helper()
	steps := []struct {
		name  string
		match func(int) bool
	}{
		{"the issue's owner lock", d.containing(ownerLockHead)},
		{"the lock of the issue's room", d.containing(roomLockHead)},
		{"the read of the mark", d.containing(markReadHead)},
		{"the insert of the version of " + drained, d.inserts(drained)},
		{"the clear of the mark", d.containing(markClearHead)},
		{"the drain's COMMIT", d.commit},
	}
	for _, step := range steps {
		found := d.at(from, step.match)
		if found < 0 {
			t.Errorf("%s sent no %s in its order: owner lock, room lock, mark read, version insert, mark clear, COMMIT", what, step.name)
			return
		}
		t.Logf("%s: %s is statement %d", what, step.name, found)
		from = found + 1
	}
}

// newestTupleSeq is the greatest graph_tuples.seq the store holds.
func newestTupleSeq(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var seq int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(seq), 0) from graph_tuples`).Scan(&seq); err != nil {
		t.Fatalf("read the newest tuple sequence: %v", err)
	}
	return seq
}

// requireOwnerLockFree fails unless a transaction of the test's own takes the owner lock of the
// issue key, which every fill takes first, within a second: what holds the lock waits for nothing.
func requireOwnerLockFree(t *testing.T, database *store.Store, what, key string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the take of %s's owner lock: %v", key, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select set_config('lock_timeout', '1s', true)`); err != nil {
		t.Fatalf("bound the take of %s's owner lock: %v", key, err)
	}
	if err := store.LockOwner(ctx, tx, store.IssueOwner(key)); err != nil {
		t.Errorf("%s: the owner lock of %s, which the fill takes, is held: %v", what, key, err)
	}
}

// TestIssueQueryMarksDrainAfterCommit is c-query-drain's for the two result routes, on a converted
// issue whose Verification document holds one check, through the real routes and their real
// drain, held at the drain's two boundaries (drainProbe) and read on connections of the test's
// own.
//
//   - A recorded result: its transaction's batch marks the issue's queries owed and inserts no
//     version; once it has committed, and before the drain, the result's event, its tuples and the
//     mark stand, the mark caused by result.recorded and by the greatest tuple sequence the result
//     appended, with alice as its writer, and no new version does. The drain then takes the owner
//     lock, then the room's lock, reads, and sends its batch and COMMIT (the server's trace); once it
//     has committed and before the route publishes, nothing is published yet, the version stands
//     with the bytes of the one before and its record's digest, with its artifact.version, the open
//     approval request names it, and the mark is gone. The route publishes result.recorded first and
//     what the drain appended after it, and the version's node is caused by result.recorded.
//   - A result recorded over an approval of the Verification document adds only result nodes to the
//     approved record: the drain carries the approval to its version, with artifact.approved before
//     its artifact.version, and the approval stands at the new version.
//   - Its retraction moves a result node the approval holds: the drain writes the version and
//     carries nothing, and the approval reads changed since. As for the record, once the
//     retraction has committed and before the drain the mark stands, caused by result.retracted
//     and the retraction's greatest tuple sequence, and no new version does; the server's trace
//     has the shape the record's has; and once the drain has committed and before the route
//     publishes, nothing is published yet, the version and its artifact.version stand, and the
//     mark is gone.
//   - Two writes by distinct credentials: with an approval request open again, alice records a
//     result and her request's drain is held at its start, before its revision's transaction
//     (docs.Service.HoldDrains); bob then retracts that result with his own request's drain
//     suppressed, which raises the mark to his event and his tuple. Released, alice's drain writes
//     one version, under bob's event, in the name of the who of the graph_tuples row at the mark's
//     greatest_seq, read on a connection of the test's own - bob - on the version, its node and its
//     artifact.version, never alice, whose request ran the drain; it advances the request and
//     clears the mark.
//   - With the request's drain suppressed, alice's result and bob's retraction of it leave one mark,
//     caused by the greater event and the greater tuple sequence of the two, with bob, the
//     retraction's writer, at its greatest_seq; the tick of the block-index reconciliation writes
//     the version under that event, in the name of the drain's own system actor
//     (docs.QueryDrainActor, kind system) on the version, its node and its artifact.version, never
//     alice's or bob's, advances the request and clears the mark, and a second tick writes nothing.
//   - On an issue whose spec holds a criterion, with the request's drain suppressed and an approval
//     request open on the spec query, a node PATCH moves the criterion under a lens and a result
//     is recorded: one mark, holding the result's event and the greater tuple sequence of the two,
//     the result's, which follows the PATCH's. The tick writes a version of the spec query and of
//     the Verification document for the final state under the result's event, named for the
//     drain's system actor, advances the request and clears the mark. Two PATCHes then leave one
//     eventless mark at the second's sequence, and the tick writes the spec query's version at
//     graphstore.NoEvent; a second tick with no new mark writes nothing.
//   - A node PATCH (the criterion moved under a lens, which changes the spec query's answer and no
//     text) on an issue with an approval request open on the spec query: once it has committed and
//     before its drain the move is visible at graphstore.NoEvent, and the mark stands with
//     greatest_event null, greatest_seq the newest tuple sequence and alice its writer, and no new
//     version does; the trace has the record's shape; at the publish nothing is published yet, the
//     drained version stands with its artifact.version in alice's name, the request names it and
//     the mark is gone; its node is caused at graphstore.NoEvent.
//   - A node DELETE of a decision's reading, which takes it out of the spec's text and out of the
//     decisions query's record: at the start of its drain (docs.Service.HoldDrains) nothing is
//     published yet, the reading reads gone, the mark stands as the PATCH's does, the decisions
//     query holds no new version, and the versions the DELETE itself wrote - the spec's - stand
//     with their artifact.version in alice's name. Its trace inserts no version of the decisions
//     query before its COMMIT; the drain then writes that version alone, under graphstore.NoEvent,
//     in alice's name, published by the request, and clears the mark.
//   - A drain behind an owed record digest: an edit of the spec leaves its version's digest owed,
//     its own fill held, and a result is recorded; the result's drain runs its fill first, held
//     too. While that fill waits the mark stands, the digest is owed, no version of the
//     Verification document is written and the issue's owner lock, which the fill takes, is free.
//     Released, the fill records the digest and commits, and only after that COMMIT does the
//     drain take its locks, revise and clear the mark (the trace).
//   - A result on an issue that has not converted marks nothing and runs no drain.
//
// TestAMoveMarksItsIssuesQueriesOwedAndTheDrainWritesThem (api/node_move_drain_test.go) holds the
// PATCH's case on a server of its own, and a move on an issue that has not converted.
func TestIssueQueryMarksDrainAfterCommit(t *testing.T) {
	handler, database, deps, probe, sent := newDrainProbeServer(t)
	documents := probe.Service
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "QMD")
	issue := createDigestIssue(t, handler, database, "QMD", "The drained issue", revisedSpec, false)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", revisedChecks)
	verification := readDecisionDocument(t, database, issue.Key, "verification-md")
	request := requestApproval(t, handler, verification.id)
	published, stop := deps.Events.Subscribe()
	defer stop()

	recordOne := func(t *testing.T) (model.CheckResult, int64) {
		t.Helper()
		recorded := recordResult(t, handler, issue.Key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1}))
		if recorded.Code != http.StatusCreated {
			t.Fatalf("record a result of c1: status=%d body=%s", recorded.Code, recorded.Body.String())
		}
		result := decodeBody[recordedResult](t, recorded).Result
		return result, eventOf(t, database, "result.recorded", "payload->>'result_id'", result.ID)
	}
	retractOne := func(t *testing.T, result model.CheckResult, login string) int64 {
		t.Helper()
		if retracted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/results/"+result.ID+"/retract",
			map[string]any{"reason": "The run measured the wrong build."}, login); retracted.Code != http.StatusCreated {
			t.Fatalf("retract the result %s as %s: status=%d body=%s", result.ID, login, retracted.Code, retracted.Body.String())
		}
		return eventOf(t, database, "result.retracted", "payload->>'result_id'", result.ID)
	}
	t.Cleanup(func() { probe.before, probe.after = nil, nil })

	t.Run("a recorded result", func(t *testing.T) {
		before := newestQueryVersion(t, database, verification.id)
		publishedSince(published)
		from := len(sent.Statements())
		var committed, drained bool
		probe.before = func(key string) {
			committed = true
			if key != issue.Key {
				t.Errorf("the request drains %s, want the check's issue %s", key, issue.Key)
			}
			event := eventOf(t, database, "result.recorded", "issue_key", issue.Key)
			requireMarkCauses(t, database, "result", issue.Key, event, greatestSeqOf(t, database, event), "alice")
			if held := newestQueryVersion(t, database, verification.id); held.number != before.number {
				t.Errorf("version %d of the Verification document stands once the result committed and before its drain, want it at %d", held.number, before.number)
			}
		}
		probe.after = func(key string, appended []model.Event) {
			drained = true
			if early := publishedSince(published); len(early) > 0 {
				t.Errorf("%s published locally before the drain committed", early[0].Type)
			}
			version := newestQueryVersion(t, database, verification.id)
			if version.number != before.number+1 || version.markdown != before.markdown {
				t.Errorf("the drain committed version %d holding %q, want version %d holding the bytes of the one before, %q",
					version.number, version.markdown, before.number+1, before.markdown)
			}
			requireVersionEvent(t, database, verification.id, version.number)
			if named := requestedVersionOf(t, database, request.ID, verification.id); named != version.number {
				t.Errorf("the approval request names version %d of the Verification document at the publish, want the drained version %d", named, version.number)
			}
			requireNoQueryMark(t, database, "at the publish", key)
			if !slices.ContainsFunc(appended, func(event model.Event) bool { return event.Type == "artifact.version" }) {
				t.Errorf("the drain answers %+v for the route to publish, want its artifact.version among them", appended)
			}
		}
		_, event := recordOne(t)
		probe.before, probe.after = nil, nil
		if !committed || !drained {
			t.Fatalf("the record ran no drain of %s once it committed (before %t, after %t)", issue.Key, committed, drained)
		}
		withResult := newestQueryVersion(t, database, verification.id)
		requireRecordedDigest(t, database, "the drained version of a recorded result", issue.Key, graphstore.QueryVerification, withResult)
		requireRevisedVersion(t, database, "the drained version of a recorded result", verification.id, verification.refKey, withResult.number, event)
		order := publishedSince(published)
		if len(order) == 0 || order[0].Type != "result.recorded" || order[0].ID != event {
			t.Fatalf("the route published %+v, want result.recorded %d first", order, event)
		}
		versionEvent, _ := requireVersionEvent(t, database, verification.id, withResult.number)
		if !slices.ContainsFunc(order[1:], func(published model.Event) bool { return published.ID == versionEvent }) {
			t.Errorf("the route published %+v, want the drained version's artifact.version %d after result.recorded", order, versionEvent)
		}
		for _, later := range order[1:] {
			if later.ID < event {
				t.Errorf("the route published %s %d after result.recorded %d, which it follows in the log", later.Type, later.ID, event)
			}
		}
		if blind := sent.Blind(); len(blind) > 0 {
			t.Fatalf("the statement recorder could not read %q", blind)
		}
		requireDrainTrace(t, "a recorded result", verification.id, sent.Statements()[from:])
		if approval := readApproval(t, handler, verification.id).Approval; approval == nil || approval.State == "approved" {
			t.Errorf("the Verification document reads approved %+v with no approval given", approval)
		}
	})

	var carried model.CheckResult
	t.Run("an approval carried across appended results", func(t *testing.T) {
		approved := newestQueryVersion(t, database, verification.id)
		review := reviewDocument(t, handler, verification.id, "alice", "approved", "")
		if review.Version != approved.number {
			t.Fatalf("alice approved version %d of the Verification document, want its newest, %d", review.Version, approved.number)
		}
		var event int64
		carried, event = recordOne(t)
		version := newestQueryVersion(t, database, verification.id)
		if version.number != approved.number+1 {
			t.Fatalf("a result over the approval leaves the Verification document at version %d, want %d", version.number, approved.number+1)
		}
		requireRevisedVersion(t, database, "the drained version a carry crossed", verification.id, verification.refKey, version.number, event)
		approval := readApproval(t, handler, verification.id).Approval
		if approval == nil || approval.State != "approved" || approval.Version == nil || *approval.Version != version.number {
			t.Fatalf("the approval of the Verification document reads %+v after a result appended to its record, want it carried to version %d", approval, version.number)
		}
		if approval.Reason == nil || !strings.HasPrefix(*approval.Reason, docs.CarriedReviewReasonPrefix) {
			t.Errorf("the carried approval's reason is %v, want one that opens %q", approval.Reason, docs.CarriedReviewReasonPrefix)
		}
		approvedEvent, _, held := documentEventOf(t, database, "artifact.approved", verification.id, version.number)
		versionEvent, _ := requireVersionEvent(t, database, verification.id, version.number)
		if !held || approvedEvent >= versionEvent {
			t.Errorf("the carry appended artifact.approved %d (held %t) and the version artifact.version %d, want artifact.approved first", approvedEvent, held, versionEvent)
		}
		requireNoQueryMark(t, database, "after the carry's drain", issue.Key)
	})

	t.Run("a retraction carries nothing", func(t *testing.T) {
		before := newestQueryVersion(t, database, verification.id)
		publishedSince(published)
		from := len(sent.Statements())
		var committed, drained bool
		probe.before = func(key string) {
			committed = true
			event := eventOf(t, database, "result.retracted", "payload->>'result_id'", carried.ID)
			requireMarkCauses(t, database, "retraction", key, event, greatestSeqOf(t, database, event), "alice")
			if held := newestQueryVersion(t, database, verification.id); held.number != before.number {
				t.Errorf("version %d of the Verification document stands once the retraction committed and before its drain, want it at %d", held.number, before.number)
			}
		}
		probe.after = func(key string, appended []model.Event) {
			drained = true
			if early := publishedSince(published); len(early) > 0 {
				t.Errorf("%s published locally before the retraction's drain committed", early[0].Type)
			}
			version := newestQueryVersion(t, database, verification.id)
			if version.number != before.number+1 {
				t.Errorf("the retraction's drain committed version %d of the Verification document at the publish, want %d", version.number, before.number+1)
			}
			requireVersionEvent(t, database, verification.id, version.number)
			requireNoQueryMark(t, database, "at the publish of the retraction", key)
			if !slices.ContainsFunc(appended, func(event model.Event) bool { return event.Type == "artifact.version" }) {
				t.Errorf("the retraction's drain answers %+v for the route to publish, want its artifact.version among them", appended)
			}
		}
		t.Cleanup(func() { probe.before, probe.after = nil, nil })
		event := retractOne(t, carried, "alice")
		probe.before, probe.after = nil, nil
		if !committed || !drained {
			t.Fatalf("the retraction ran no drain of %s once it committed (before %t, after %t)", issue.Key, committed, drained)
		}
		if blind := sent.Blind(); len(blind) > 0 {
			t.Fatalf("the statement recorder could not read %q", blind)
		}
		requireDrainTrace(t, "a retraction", verification.id, sent.Statements()[from:])
		version := newestQueryVersion(t, database, verification.id)
		if version.number != before.number+1 || version.markdown != before.markdown {
			t.Fatalf("the retraction leaves the Verification document at version %d holding %q, want version %d holding %q",
				version.number, version.markdown, before.number+1, before.markdown)
		}
		requireRevisedVersion(t, database, "the drained version of a retraction", verification.id, verification.refKey, version.number, event)
		requireVersionEvent(t, database, verification.id, version.number)
		if _, _, held := documentEventOf(t, database, "artifact.approved", verification.id, version.number); held {
			t.Errorf("the drain carried the approval across a retraction, which moves a result the approval holds")
		}
		if approval := readApproval(t, handler, verification.id).Approval; approval == nil || approval.State != "stale" {
			t.Errorf("the approval of the Verification document reads %+v after a retraction, want it changed since (stale)", approval)
		}
		requireNoQueryMark(t, database, "after the retraction's drain", issue.Key)
	})

	// open is an approval request of the Verification document opened once the retraction left its
	// approval changed since, which the drains below advance.
	var open carriedAsk
	t.Run("a request's drain names the newest marking write's writer", func(t *testing.T) {
		open = requestApproval(t, handler, verification.id)
		before := newestQueryVersion(t, database, verification.id)
		drains := probe.drains.Load()
		held, release := make(chan struct{}), make(chan struct{})
		var holding, released sync.Once
		letGo := func() { released.Do(func() { close(release) }) }
		documents.HoldDrains(func(key string) {
			if key != issue.Key {
				return
			}
			holding.Do(func() {
				close(held)
				<-release
			})
		})
		t.Cleanup(func() {
			letGo()
			documents.HoldDrains(nil)
		})
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			answered <- recordResult(t, handler, issue.Key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1}))
		}()
		// docs.Service.HoldDrains runs at the start of drainMark: alice's request has committed its
		// result and its fill, and its drain has not begun the revision's transaction.
		testwait.Receive(t, "alice's drain, held before its revision", held)
		var recordedEvent int64
		var result model.CheckResult
		if err := database.Pool.QueryRow(ctx, `
			select id, payload->>'result_id' from events where type = 'result.recorded' and issue_key = $1 order by id desc limit 1`,
			issue.Key).Scan(&recordedEvent, &result.ID); err != nil {
			t.Fatalf("read the result alice's held request recorded: %v", err)
		}
		requireMarkCauses(t, database, "alice's result, its drain held", issue.Key, recordedEvent, greatestSeqOf(t, database, recordedEvent), "alice")
		probe.suppress.Store(true)
		retractedEvent := retractOne(t, result, "bob")
		probe.suppress.Store(false)
		mark, marked := readQueryMarkRow(t, database, issue.Key)
		if !marked {
			t.Fatalf("no issue_query_marks row of %s stands after bob's retraction", issue.Key)
		}
		requireMarkCauses(t, database, "bob's retraction while alice's drain waits", issue.Key, retractedEvent, greatestSeqOf(t, database, retractedEvent), "bob")
		if held := newestQueryVersion(t, database, verification.id); held.number != before.number {
			t.Fatalf("the Verification document stands at version %d while alice's drain waits, want %d", held.number, before.number)
		}
		letGo()
		if recorded := testwait.Receive(t, "alice's answer", answered); recorded.Code != http.StatusCreated {
			t.Fatalf("alice's result answered %d %s, want 201", recorded.Code, recorded.Body.String())
		}
		if ran := probe.drains.Load() - drains; ran != 1 {
			t.Errorf("the two requests ran %d drains, want alice's one: bob's was suppressed", ran)
		}
		version := newestQueryVersion(t, database, verification.id)
		if version.number != before.number+1 {
			t.Fatalf("alice's drain left the Verification document at version %d, want %d", version.number, before.number+1)
		}
		requireRecordedDigest(t, database, "the version alice's drain wrote", issue.Key, graphstore.QueryVerification, version)
		requireRevisedVersion(t, database, "the version alice's drain wrote", verification.id, verification.refKey, version.number, retractedEvent)
		requireNamedBy(t, database, "the version alice's drain wrote after bob's mark", verification.id, verification.refKey, version.number,
			mark.writer, fmt.Sprintf("the who of the graph_tuples row at the mark's greatest_seq %d", mark.seq),
			"whose request started the drain but did not write the newest mark", "alice")
		if named := requestedVersionOf(t, database, open.ID, verification.id); named != version.number {
			t.Errorf("the approval request %s names version %d of the Verification document after alice's drain, want %d", open.ID, named, version.number)
		}
		requireNoQueryMark(t, database, "after alice's drain", issue.Key)
	})

	t.Run("the tick drains a mark a request left", func(t *testing.T) {
		before := newestQueryVersion(t, database, verification.id)
		drains := probe.drains.Load()
		probe.suppress.Store(true)
		result, recordedEvent := recordOne(t)
		retractedEvent := retractOne(t, result, "bob")
		probe.suppress.Store(false)
		if ran := probe.drains.Load(); ran != drains {
			t.Fatalf("the suppressed requests ran %d drains", ran-drains)
		}
		seq := max(greatestSeqOf(t, database, recordedEvent), greatestSeqOf(t, database, retractedEvent))
		requireMarkCauses(t, database, "alice's result and bob's retraction", issue.Key, max(recordedEvent, retractedEvent), seq, "bob")
		if held := newestQueryVersion(t, database, verification.id); held.number != before.number {
			t.Fatalf("the suppressed drain left the Verification document at version %d, want %d", held.number, before.number)
		}

		tick, err := documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil)
		if err != nil {
			t.Fatalf("a tick of the block-index reconciliation: %v", err)
		}
		if tick.Queries != 1 {
			t.Errorf("the tick drained %d marks, want the one %s holds", tick.Queries, issue.Key)
		}
		if mark, held := readQueryMarkRow(t, database, issue.Key); held {
			t.Fatalf("issue_query_marks still owed after the tick: %s caused by event %v and sequence %d", issue.Key, mark.event, mark.seq)
		}
		version := newestQueryVersion(t, database, verification.id)
		if version.number != before.number+1 || version.markdown != before.markdown {
			t.Fatalf("the tick leaves the Verification document at version %d holding %q, want version %d holding %q",
				version.number, version.markdown, before.number+1, before.markdown)
		}
		requireRecordedDigest(t, database, "the version the tick drained", issue.Key, graphstore.QueryVerification, version)
		requireRevisedVersion(t, database, "the version the tick drained", verification.id, verification.refKey, version.number, retractedEvent)
		requireNamedByTheTick(t, database, "the version the tick drained", verification.id, verification.refKey, version.number, "alice", "bob")
		if open.ID == "" {
			t.Errorf("no approval request of the Verification document is open for the tick to advance: the case before did not open one")
		} else if named := requestedVersionOf(t, database, open.ID, verification.id); named != version.number {
			t.Errorf("the approval request %s names version %d of the Verification document after the tick, want the tick's %d", open.ID, named, version.number)
		}

		again, err := documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil)
		if err != nil {
			t.Fatalf("a second tick of the block-index reconciliation: %v", err)
		}
		if held := newestQueryVersion(t, database, verification.id); again.Queries != 0 || held.number != version.number {
			t.Errorf("a second tick with no mark drained %d marks and left version %d, want none and %d", again.Queries, held.number, version.number)
		}
	})

	t.Run("the tick drains the marks of node PATCHes and a result", func(t *testing.T) {
		moved := createDigestIssue(t, handler, database, "QMD", "Moves and a result the tick drains", moveDrainSpec, false)
		key := moved.Key
		uploadDocument(t, handler, "/api/v1/issues/"+key, "verification.md", revisedChecks)
		criterion := readDecisionDocument(t, database, key, "spec").address("qk1")
		specQuery := queryArtifactOf(t, database, key, graphstore.QuerySpec)
		var specRef string
		if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, specQuery).Scan(&specRef); err != nil {
			t.Fatalf("read the ref_key of the spec query of %s: %v", key, err)
		}
		checks := readDecisionDocument(t, database, key, "verification-md")
		specRequest := requestApproval(t, handler, specQuery)
		lenses := []string{moveDrainLens(t, handler, key), moveDrainLens(t, handler, key), moveDrainLens(t, handler, key)}
		move := func(t *testing.T, under string) int64 {
			t.Helper()
			_, tag := placementOf(t, database, "block", criterion)
			if response := patchNode(t, handler, key, criterion, tag, map[string]any{"parent": under}, "alice"); response.Code != http.StatusOK {
				t.Fatalf("the move of %s under %s answered %d %s, want 200", criterion, under, response.Code, response.Body.String())
			}
			placed, _ := placementOf(t, database, "block", criterion)
			if placed.SourceEvent != graphstore.NoEvent {
				t.Errorf("the move of %s under %s appended its part_of under event %d, want graphstore.NoEvent", criterion, under, placed.SourceEvent)
			}
			return placed.Seq
		}
		tickDrains := func(t *testing.T, what string) {
			t.Helper()
			tick, err := documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil)
			if err != nil {
				t.Fatalf("a tick of the block-index reconciliation after %s: %v", what, err)
			}
			if tick.Queries != 1 {
				t.Errorf("the tick after %s drained %d marks, want the one %s holds", what, tick.Queries, key)
			}
			if mark, held := readQueryMarkRow(t, database, key); held {
				t.Fatalf("issue_query_marks still owed after the tick after %s: %s caused by event %v and sequence %d", what, key, mark.event, mark.seq)
			}
		}
		drains := probe.drains.Load()
		probe.suppress.Store(true)
		t.Cleanup(func() { probe.suppress.Store(false) })

		// A PATCH, then a result: one mark, caused by the result's event and by the greater tuple
		// sequence of the two writes, the result's, which follows the PATCH's.
		specBefore, checksBefore := newestQueryVersion(t, database, specQuery), newestQueryVersion(t, database, checks.id)
		movedSeq := move(t, lenses[0])
		if mark, held := readQueryMarkRow(t, database, key); !held || mark.event != nil || mark.seq < movedSeq {
			t.Errorf("the PATCH's mark of %s is %+v (held %t), want greatest_event null and greatest_seq at least %d, the seq of the part_of it appended", key, mark, held, movedSeq)
		}
		recorded := recordResult(t, handler, key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1}))
		if recorded.Code != http.StatusCreated {
			t.Fatalf("record a result of %s's c1: status=%d body=%s", key, recorded.Code, recorded.Body.String())
		}
		resultEvent := eventOf(t, database, "result.recorded", "payload->>'result_id'", decodeBody[recordedResult](t, recorded).Result.ID)
		resultSeq := greatestSeqOf(t, database, resultEvent)
		if resultSeq <= movedSeq {
			t.Errorf("the result's greatest tuple sequence %d does not follow the PATCH's %d", resultSeq, movedSeq)
		}
		requireMarkCauses(t, database, "a PATCH and then a result", key, resultEvent, max(movedSeq, resultSeq), "alice")
		if spec, held := newestQueryVersion(t, database, specQuery), newestQueryVersion(t, database, checks.id); spec.number != specBefore.number || held.number != checksBefore.number {
			t.Fatalf("the suppressed drains left the spec query at version %d and the Verification document at %d, want %d and %d",
				spec.number, held.number, specBefore.number, checksBefore.number)
		}
		tickDrains(t, "a PATCH and a result")
		spec, held := newestQueryVersion(t, database, specQuery), newestQueryVersion(t, database, checks.id)
		if spec.number != specBefore.number+1 || held.number != checksBefore.number+1 {
			t.Fatalf("the tick left the spec query at version %d and the Verification document at %d, want %d and %d",
				spec.number, held.number, specBefore.number+1, checksBefore.number+1)
		}
		requireRecordedDigest(t, database, "the spec query's version the tick drained", key, graphstore.QuerySpec, spec)
		requireRecordedDigest(t, database, "the Verification document's version the tick drained", key, graphstore.QueryVerification, held)
		requireRevisedVersion(t, database, "the spec query's version the tick drained", specQuery, specRef, spec.number, resultEvent)
		requireRevisedVersion(t, database, "the Verification document's version the tick drained", checks.id, checks.refKey, held.number, resultEvent)
		requireNamedByTheTick(t, database, "the spec query's version the tick drained", specQuery, specRef, spec.number, "alice")
		requireNamedByTheTick(t, database, "the Verification document's version the tick drained", checks.id, checks.refKey, held.number, "alice")
		if named := requestedVersionOf(t, database, specRequest.ID, specQuery); named != spec.number {
			t.Errorf("the approval request %s names version %d of the spec query after the tick, want the tick's %d", specRequest.ID, named, spec.number)
		}

		// Two PATCHes: one eventless mark at the greater tuple sequence, the second PATCH's, which
		// leaves the criterion under a lens it did not stand under at the tick before.
		specBefore, checksBefore = spec, held
		firstSeq := move(t, lenses[1])
		secondSeq := move(t, lenses[2])
		mark, marked := readQueryMarkRow(t, database, key)
		switch {
		case !marked:
			t.Fatalf("two PATCHes of %s left no issue_query_marks row", key)
		case mark.event != nil:
			t.Errorf("the mark of two PATCHes of %s holds greatest_event %d, want null: both write at graphstore.NoEvent", key, *mark.event)
		case secondSeq <= firstSeq || mark.seq < secondSeq:
			t.Errorf("the mark of two PATCHes of %s holds greatest_seq %d after the PATCHes appended %d and %d, want at least the second's", key, mark.seq, firstSeq, secondSeq)
		}
		tickDrains(t, "two PATCHes")
		spec, held = newestQueryVersion(t, database, specQuery), newestQueryVersion(t, database, checks.id)
		if spec.number != specBefore.number+1 || held.number != checksBefore.number {
			t.Fatalf("the tick left the spec query at version %d and the Verification document at %d, want %d and %d",
				spec.number, held.number, specBefore.number+1, checksBefore.number)
		}
		requireRecordedDigest(t, database, "the spec query's version the tick drained of two PATCHes", key, graphstore.QuerySpec, spec)
		requireRevisedVersion(t, database, "the spec query's version the tick drained of two PATCHes", specQuery, specRef, spec.number, graphstore.NoEvent)
		requireNamedByTheTick(t, database, "the spec query's version the tick drained of two PATCHes", specQuery, specRef, spec.number, "alice")
		if named := requestedVersionOf(t, database, specRequest.ID, specQuery); named != spec.number {
			t.Errorf("the approval request %s names version %d of the spec query after the tick of two PATCHes, want the tick's %d", specRequest.ID, named, spec.number)
		}
		if ran := probe.drains.Load(); ran != drains {
			t.Errorf("the suppressed requests ran %d drains", ran-drains)
		}

		again, err := documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil)
		if err != nil {
			t.Fatalf("a second tick of the block-index reconciliation: %v", err)
		}
		if after := newestQueryVersion(t, database, specQuery); again.Queries != 0 || after.number != spec.number {
			t.Errorf("a second tick with no mark drained %d marks and left the spec query at version %d, want none and %d", again.Queries, after.number, spec.number)
		}
	})

	t.Run("a node PATCH", func(t *testing.T) {
		moved := createDigestIssue(t, handler, database, "QMD", "A move its request drains", moveDrainSpec, false)
		key := moved.Key
		criterion := readDecisionDocument(t, database, key, "spec").address("qk1")
		lens := moveDrainLens(t, handler, key)
		specQuery := queryArtifactOf(t, database, key, graphstore.QuerySpec)
		specRequest := requestApproval(t, handler, specQuery)
		before := newestQueryVersion(t, database, specQuery)
		_, tag := placementOf(t, database, "block", criterion)
		publishedSince(published)
		from := len(sent.Statements())
		var committed, drained bool
		probe.before = func(drainedKey string) {
			committed = true
			if drainedKey != key {
				t.Errorf("the PATCH drains %s, want the issue it moved a node of, %s", drainedKey, key)
			}
			placed, _ := placementOf(t, database, "block", criterion)
			if storeAddress(placed.ToKind, placed.ToID) != lens || placed.SourceEvent != graphstore.NoEvent {
				t.Errorf("once the PATCH committed %s stands under %s at event %d, want under %s at graphstore.NoEvent", criterion, storeAddress(placed.ToKind, placed.ToID), placed.SourceEvent, lens)
			}
			mark, marked := readQueryMarkRow(t, database, key)
			newest := newestTupleSeq(t, database)
			switch {
			case !marked:
				t.Errorf("the PATCH of %s committed and left no issue_query_marks row of %s", criterion, key)
			case mark.event != nil:
				t.Errorf("the PATCH's mark of %s holds greatest_event %d, want null: the move writes its tuples at graphstore.NoEvent", key, *mark.event)
			case mark.seq != newest:
				t.Errorf("PATCH mark cause sequence %d, want %d, the newest tuple sequence once the PATCH committed", mark.seq, newest)
			case mark.writer.ID != "alice":
				t.Errorf("the tuple at the PATCH's mark's greatest_seq %d was written by %+v, want alice", mark.seq, mark.writer)
			}
			if version := newestQueryVersion(t, database, specQuery); version.number != before.number {
				t.Errorf("the spec query of %s stands at version %d once the PATCH committed and before its drain, want %d", key, version.number, before.number)
			}
		}
		probe.after = func(drainedKey string, appended []model.Event) {
			drained = true
			if early := publishedSince(published); len(early) > 0 {
				t.Errorf("%s published locally before the PATCH's drain committed", early[0].Type)
			}
			version := newestQueryVersion(t, database, specQuery)
			if version.number != before.number+1 {
				t.Errorf("the PATCH's drain left the spec query of %s at version %d, want %d", key, version.number, before.number+1)
			}
			if _, actor := requireVersionEvent(t, database, specQuery, version.number); actor.ID != "alice" {
				t.Errorf("the artifact.version of the PATCH's drained version names %+v, want alice, who wrote the tuple at the mark's greatest_seq", actor)
			}
			if named := requestedVersionOf(t, database, specRequest.ID, specQuery); named != version.number {
				t.Errorf("the request on %s names version %d at the publish of the PATCH, want %d", specQuery, named, version.number)
			}
			requireNoQueryMark(t, database, "at the publish of the PATCH", drainedKey)
			if !slices.ContainsFunc(appended, func(event model.Event) bool { return event.Type == "artifact.version" }) {
				t.Errorf("the PATCH's drain answers %+v for the route to publish, want its artifact.version among them", appended)
			}
		}
		t.Cleanup(func() { probe.before, probe.after = nil, nil })
		response := patchNode(t, handler, key, criterion, tag, map[string]any{"parent": lens}, "alice")
		probe.before, probe.after = nil, nil
		if response.Code != http.StatusOK {
			t.Fatalf("the PATCH of %s under %s answered %d %s, want 200", criterion, lens, response.Code, response.Body.String())
		}
		if !committed || !drained {
			t.Fatalf("the PATCH ran no drain of %s once it committed (before %t, after %t)", key, committed, drained)
		}
		if blind := sent.Blind(); len(blind) > 0 {
			t.Fatalf("the statement recorder could not read %q", blind)
		}
		requireDrainTrace(t, "the PATCH of "+criterion, specQuery, sent.Statements()[from:])
		version := newestQueryVersion(t, database, specQuery)
		requireRecordedDigest(t, database, "the PATCH's drained version", key, graphstore.QuerySpec, version)
		requireRevisedVersion(t, database, "the PATCH's drained version", specQuery, refKeyOf(t, database, specQuery), version.number, graphstore.NoEvent)
	})

	t.Run("a node DELETE", func(t *testing.T) {
		removed := createDigestIssue(t, handler, database, "QMD", "A delete its request drains", drainDeleteSpec, false)
		key := removed.Key
		reading := blockAddressOf(t, database, key, "dr1")
		decisions := queryArtifactOf(t, database, key, graphstore.QueryDecisions)
		var decisionsSlug string
		if err := database.Pool.QueryRow(ctx, `select slug from artifacts where id = $1::uuid`, decisions).Scan(&decisionsSlug); err != nil {
			t.Fatalf("read the slug of the decisions query of %s: %v", key, err)
		}
		alice := personCaller(t, handler, "alice")
		count := deletedCount(t, "the dry run of "+reading, deleteNodeAs(alice, key, reading, true, -1, ""))
		_, tag := placementOf(t, database, "block", reading)
		versions := issueVersionCounts(t, database.Pool, key)
		before := newestQueryVersion(t, database, decisions)
		publishedSince(published)
		from := len(sent.Statements())
		preceding := newestTupleSeq(t, database)
		// The DELETE writes the spec's version through the document service's ledger, whose commit
		// drains the mark (docs' Ledger.Commit, drainOwed) before the route's own drain, which then
		// finds none; so the case reads the store at the start of the first drain of the issue
		// (docs.Service.HoldDrains), once the DELETE and the drain's fill have committed.
		var committed bool
		var written map[string]int
		documents.HoldDrains(func(drainedKey string) {
			if drainedKey != key || committed {
				return
			}
			committed = true
			if early := publishedSince(published); len(early) > 0 {
				t.Errorf("%s published locally before the DELETE's drain", early[0].Type)
			}
			if state := nodeStateOf(t, database, "block", reading); state != graphstore.StateGone {
				t.Errorf("once the DELETE committed %s reads %s, want %s", reading, state, graphstore.StateGone)
			}
			mark, marked := readQueryMarkRow(t, database, key)
			newest := newestTupleSeq(t, database)
			switch {
			case !marked:
				t.Errorf("the DELETE of %s committed and left no issue_query_marks row of %s", reading, key)
			case mark.event != nil:
				t.Errorf("the DELETE's mark of %s holds greatest_event %d, want null: the delete writes its tuples at graphstore.NoEvent", key, *mark.event)
			case mark.seq != newest:
				t.Errorf("DELETE mark cause sequence %d, want %d, the newest tuple sequence once the DELETE committed", mark.seq, newest)
			case mark.writer.ID != "alice":
				t.Errorf("the tuple at the DELETE's mark's greatest_seq %d was written by %+v, want alice", mark.seq, mark.writer)
			}
			// The DELETE's own node write states its tuples at graphstore.NoEvent. The version it
			// writes of the spec states the reading's block gone in its own index pass
			// (node_delete.go's head), whose facts that version's write causes; the case names
			// those facts and their cause in its log and asserts nothing of them (TickActor-r5.md,
			// item 1: the spec's "The PATCH and the DELETE write their tuples and states at
			// graphstore.NoEvent" against that pass).
			rows, err := database.Pool.Query(ctx, `
				select g.seq, g.kind, g.asserted, g.source_event, coalesce(e.type, ''), coalesce(e.payload->>'artifact_id', '')
				from graph_tuples g left join events e on e.id = g.source_event
				where g.from_kind = 'block' and g.from_id = $1 and g.seq > $2 order by g.seq`, reading, preceding)
			if err != nil {
				t.Fatalf("read the tuples the DELETE wrote out of %s: %v", reading, err)
			}
			unevented := 0
			for rows.Next() {
				var seq, cause int64
				var kind, causeType, causeArtifact string
				var asserted bool
				if err := rows.Scan(&seq, &kind, &asserted, &cause, &causeType, &causeArtifact); err != nil {
					rows.Close()
					t.Fatalf("read a tuple the DELETE wrote out of %s: %v", reading, err)
				}
				if cause == graphstore.NoEvent {
					unevented++
					continue
				}
				t.Logf("the DELETE's transaction wrote the %s tuple %d out of %s (asserted %t) under event %d, %s of %s", kind, seq, reading, asserted, cause, causeType, causeArtifact)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatalf("read the tuples the DELETE wrote out of %s: %v", reading, err)
			}
			if unevented == 0 {
				t.Errorf("the DELETE wrote no tuple out of %s at graphstore.NoEvent, want its own node write's there", reading)
			}
			var nodeCause int64
			var nodeCauseType string
			if err := database.Pool.QueryRow(ctx, `
				select n.source_event, coalesce(e.type, '') from graph_nodes n left join events e on e.id = n.source_event
				where n.kind = 'block' and n.id = $1`, reading).Scan(&nodeCause, &nodeCauseType); err != nil {
				t.Fatalf("read the node of %s: %v", reading, err)
			}
			t.Logf("the node of %s, gone, is caused by event %d (%s)", reading, nodeCause, nodeCauseType)
			if version := newestQueryVersion(t, database, decisions); version.number != before.number {
				t.Errorf("the decisions query of %s stands at version %d once the DELETE committed and before its drain, want %d", key, version.number, before.number)
			}
			// The versions the DELETE itself writes, of each document whose text it changes, are
			// committed with it, each with its artifact.version.
			written = issueVersionCounts(t, database.Pool, key)
			for slug, number := range written {
				if number == versions[slug] {
					continue
				}
				if slug == decisionsSlug {
					t.Errorf("the DELETE's own transaction wrote version %d of the decisions query, which is its drain's to write", number)
					continue
				}
				document := readDecisionDocument(t, database, key, slug)
				for own := versions[slug] + 1; own <= number; own++ {
					if _, actor, held := documentEventOf(t, database, "artifact.version", document.id, own); !held {
						t.Errorf("version %d of %s, which the DELETE itself wrote, has no artifact.version once the DELETE committed", own, slug)
					} else if actor.ID != "alice" {
						t.Errorf("the artifact.version of version %d of %s, which the DELETE itself wrote, names %+v, want alice", own, slug, actor)
					}
				}
			}
			if written["spec"] != versions["spec"]+1 {
				t.Errorf("the DELETE left the spec at version %d once it committed, want %d: it takes the reading out of the spec's text", written["spec"], versions["spec"]+1)
			}
		})
		t.Cleanup(func() { documents.HoldDrains(nil) })
		response := deleteNodeAs(alice, key, reading, false, count, tag)
		documents.HoldDrains(nil)
		if response.Code != http.StatusOK {
			t.Fatalf("the DELETE of %s answered %d %s, want 200", reading, response.Code, response.Body.String())
		}
		if !committed {
			t.Fatalf("the DELETE ran no drain of %s once it committed", key)
		}
		version := newestQueryVersion(t, database, decisions)
		if version.number != before.number+1 {
			t.Errorf("the DELETE's drain left the decisions query of %s at version %d, want %d", key, version.number, before.number+1)
		}
		versionEvent, actor := requireVersionEvent(t, database, decisions, version.number)
		if actor.ID != "alice" {
			t.Errorf("the artifact.version of the DELETE's drained version names %+v, want alice, who wrote the tuple at the mark's greatest_seq", actor)
		}
		if !slices.ContainsFunc(publishedSince(published), func(event model.Event) bool { return event.ID == versionEvent }) {
			t.Errorf("the DELETE's request published no artifact.version %d of the drained version", versionEvent)
		}
		requireNoQueryMark(t, database, "once the DELETE answered", key)
		want := maps.Clone(written)
		want[decisionsSlug] = before.number + 1
		if now := issueVersionCounts(t, database.Pool, key); !maps.Equal(now, want) {
			t.Errorf("the DELETE's drain left the documents of %s at the versions %v, want %v: one version of the decisions query and no other", key, now, want)
		}
		if blind := sent.Blind(); len(blind) > 0 {
			t.Fatalf("the statement recorder could not read %q", blind)
		}
		requireDrainTrace(t, "the DELETE of "+reading, decisions, sent.Statements()[from:])
		requireRecordedDigest(t, database, "the DELETE's drained version", key, graphstore.QueryDecisions, version)
		requireRevisedVersion(t, database, "the DELETE's drained version", decisions, refKeyOf(t, database, decisions), version.number, graphstore.NoEvent)
	})

	t.Run("a drain behind an owed record digest", func(t *testing.T) {
		owing := createDigestIssue(t, handler, database, "QMD", "A drain behind an owed digest", revisedSpec, false)
		key := owing.Key
		spec := owing.PrimaryArtifactID
		uploadDocument(t, handler, "/api/v1/issues/"+key, "verification.md", revisedChecks)
		checks := readDecisionDocument(t, database, key, "verification-md")
		awaitDigestsFilled(t, database, spec)
		awaitDigestsFilled(t, database, checks.id)
		requireNoQueryMark(t, database, "before the edit", key)
		// The first fill of the issue is the edit's own, held at its start; the second is the fill
		// the result's drain runs before its revision, held until the test has read the store.
		editHeld, editRelease := make(chan struct{}), make(chan struct{})
		fillHeld, fillRelease := make(chan struct{}), make(chan struct{})
		var fills atomic.Int32
		var letEdit, letFill sync.Once
		releaseEdit := func() { letEdit.Do(func() { close(editRelease) }) }
		releaseFill := func() { letFill.Do(func() { close(fillRelease) }) }
		documents.HoldFills(func(issueKey string) {
			if issueKey != key {
				return
			}
			switch fills.Add(1) {
			case 1:
				close(editHeld)
				<-editRelease
			case 2:
				close(fillHeld)
				<-fillRelease
			}
		})
		t.Cleanup(func() {
			releaseFill()
			releaseEdit()
			documents.HoldFills(nil)
		})
		edited := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			edited <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", map[string]any{
				"ops": []map[string]string{{"op": "replace", "find": "The spec's opening words.", "with": "The spec's first words."}},
			}, "alice")
		}()
		testwait.Receive(t, "the edit's own fill, held", editHeld)
		owedVersion := newestQueryVersion(t, database, spec).number
		if row := versionFillRowOf(t, database, spec, owedVersion); !row.owed || row.at == nil {
			t.Fatalf("the edit's version %d of the spec holds at %v and digest_owed %t, want a cut and its digest owed", owedVersion, row.at, row.owed)
		}
		checksBefore := newestQueryVersion(t, database, checks.id)

		// The marked change: a result, whose request's drain runs the fill first.
		from := len(sent.Statements())
		recorded := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			recorded <- recordResult(t, handler, key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1}))
		}()
		testwait.Receive(t, "the fill the result's drain runs, held", fillHeld)
		mark, marked := readQueryMarkRow(t, database, key)
		row := versionFillRowOf(t, database, spec, owedVersion)
		switch {
		case !marked && row.owed:
			t.Errorf("query mark cleared with digest_owed=true: %s, version %d of the spec, while the drain's fill waits", key, owedVersion)
		case !marked:
			t.Errorf("the mark of %s is gone while the drain's fill waits", key)
		case !row.owed:
			t.Errorf("version %d of the spec owes no digest while the drain's fill waits: something else filled it", owedVersion)
		default:
			t.Logf("while the drain's fill waits %s stands marked (greatest_event %v, greatest_seq %d) and version %d of the spec owes its digest", key, mark.event, mark.seq, owedVersion)
		}
		if held := newestQueryVersion(t, database, checks.id); held.number != checksBefore.number {
			t.Errorf("the drain revised the Verification document to version %d while its fill waits, want it at %d", held.number, checksBefore.number)
		}
		requireOwnerLockFree(t, database, "while the drain's fill waits", key)
		releaseFill()
		if answer := testwait.Receive(t, "the result's answer", recorded); answer.Code != http.StatusCreated {
			t.Fatalf("the result answered %d %s, want 201", answer.Code, answer.Body.String())
		}
		if row := versionFillRowOf(t, database, spec, owedVersion); row.owed || row.digest == nil {
			t.Errorf("after the result's drain version %d of the spec holds record_sha256 %s and digest_owed %t, want its digest filled", owedVersion, versionFillWords(row.digest), row.owed)
		}
		version := newestQueryVersion(t, database, checks.id)
		if version.number != checksBefore.number+1 {
			t.Errorf("the result's drain left the Verification document at version %d, want %d", version.number, checksBefore.number+1)
		}
		requireRecordedDigest(t, database, "the version the drain wrote behind the fill", key, graphstore.QueryVerification, version)
		requireNoQueryMark(t, database, "after the result's drain", key)
		if blind := sent.Blind(); len(blind) > 0 {
			t.Fatalf("the statement recorder could not read %q", blind)
		}
		requireFillCommitsBeforeTheDrain(t, "the result's drain behind the edit's owed digest", checks.id, sent.Statements()[from:])
		releaseEdit()
		if answer := testwait.Receive(t, "the edit's answer", edited); answer.Code != http.StatusOK {
			t.Fatalf("the edit answered %d %s, want 200", answer.Code, answer.Body.String())
		}
		requireNoQueryMark(t, database, "after the edit answered", key)
	})

	t.Run("an unconverted issue takes no mark", func(t *testing.T) {
		unconverted := createDigestIssue(t, handler, database, "QMD", "An issue that has not converted", revisedSpec, true)
		uploadDocument(t, handler, "/api/v1/issues/"+unconverted.Key, "verification.md", revisedChecks)
		versions := issueVersionCounts(t, database.Pool, unconverted.Key)
		drains := probe.drains.Load()
		if recorded := recordResult(t, handler, unconverted.Key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1})); recorded.Code != http.StatusCreated {
			t.Fatalf("record a result of %s's c1: status=%d body=%s", unconverted.Key, recorded.Code, recorded.Body.String())
		}
		requireNoQueryMark(t, database, "after a result on an issue that has not converted", unconverted.Key)
		if ran := probe.drains.Load(); ran != drains {
			t.Errorf("a result on %s, which has not converted, ran %d drains, want none", unconverted.Key, ran-drains)
		}
		if after := issueVersionCounts(t, database.Pool, unconverted.Key); !maps.Equal(after, versions) {
			t.Errorf("a result on %s, which has not converted, leaves its documents at the versions %v, want %v", unconverted.Key, after, versions)
		}
	})
}

// drainDeleteSpec is the spec of the DELETE's case: a requirement, then a decision with a reading
// after it, as the reading tests write one. The DELETE of the reading takes it out of the spec's
// text, a version the DELETE writes itself, and out of the decisions query's record, whose version
// its drain writes.
const drainDeleteSpec = ":::requirement{#r1}\nThe list comes first.\n:::\n\n" +
	"::::decision{#dd1}\n" + readingDecisionOne + "\n\n:::reading{#dr1}\n" + readingOfOne + "\n:::\n::::\n"

// fenceSpec is a spec of two requirements, and fenceChecks the Verification document of the issue
// key, whose one check serves the first alone, so the second has no check.
const fenceSpec = ":::requirement{#r1}\nThe first requirement.\n:::\n\n:::requirement{#r2}\nThe second requirement, which no check serves.\n:::\n"

func fenceChecks(key string) string {
	return fmt.Sprintf(":::check{#c1 serves=\"dispatch://%s/spec#b-r1\" surface=\"cli\"}\nThe first requirement holds.\n:::\n", key)
}

// TestARevisedVersionLeavesNoDocumentOwedItsBlockIndex: a result recorded on a converted issue, and
// its retraction, each give the Verification document a version holding the bytes of the one
// before (the drain, docs.Service.DrainOwedQueries), whose insert owes the document its block index
// (migration 0122's trigger). Where the block index stood at the version before, the drain stands it
// at the new one in the same batch (refs.RevisionFenceStatement), so the coverage read made at once
// lists no document owed and still draws the requirement with no check: `r2 has no check`. Without
// the stand, owed_documents names verification.md and the read draws no missing check until the
// block-index tick indexes the version, up to BlockIndexReconcileInterval later.
func TestARevisedVersionLeavesNoDocumentOwedItsBlockIndex(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "FENCE")
	issue := createDigestIssue(t, handler, database, "FENCE", "The fenced issue", fenceSpec, false)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", fenceChecks(issue.Key))
	verification := readDecisionDocument(t, database, issue.Key, "verification-md")
	reconcileUntilNothingOwed(t, handler, deps, issue.Key)
	missing := noCheckGap("r2")
	requireIndexed := func(what string, version int) {
		t.Helper()
		if held := newestQueryVersion(t, database, verification.id); held.number != version {
			t.Fatalf("%s leaves the Verification document at version %d, want %d", what, held.number, version)
		}
		coverage := readCoverage(t, handler, issue.Key)
		if len(coverage.OwedDocuments) > 0 {
			t.Errorf("%s: owed_documents names %+v, with the gaps %q, want no document owed its block index", what, coverage.OwedDocuments, coverage.Gaps)
		}
		if !slices.Contains(coverage.Gaps, missing) {
			t.Errorf("%s: the coverage read answers the gaps %q, want %q among them", what, coverage.Gaps, missing)
		}
	}
	uploaded := newestQueryVersion(t, database, verification.id).number
	requireIndexed("the upload, once indexed", uploaded)

	recorded := recordResult(t, handler, issue.Key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result of c1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	requireIndexed("a recorded result", uploaded+1)

	result := decodeBody[recordedResult](t, recorded).Result
	if retracted := retractAsPerson(t, handler, result.ID, "The run measured the wrong build."); retracted.Code != http.StatusCreated {
		t.Fatalf("retract the result: status=%d body=%s", retracted.Code, retracted.Body.String())
	}
	requireIndexed("its retraction", uploaded+2)
}
