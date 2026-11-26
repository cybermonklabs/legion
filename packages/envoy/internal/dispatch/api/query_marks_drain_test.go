package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
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

// requireDrainTrace holds the statements the server sent for one result's write, from the write's
// first on (sent), to the shape R7 and R12 give it: the write's batch upserts the mark and inserts
// no version before its COMMIT; after that COMMIT the drain takes the issue's owner lock, then its
// room's lock, reads the mark, and only then inserts the version and clears the mark, and commits.
func requireDrainTrace(t *testing.T, what string, sent []storetest.SentStatement) {
	t.Helper()
	at := func(from int, match func(string) bool) int {
		for index := from; index < len(sent); index++ {
			if match(normalizedStatement(sent[index].SQL)) {
				return index
			}
		}
		return -1
	}
	containing := func(head string) func(string) bool {
		return func(shape string) bool { return strings.Contains(shape, head) }
	}
	commit := func(shape string) bool { return shape == "commit" }
	mark := at(0, containing(markUpsertHead))
	if mark < 0 {
		t.Errorf("%s: the write's batch upserts no issue_query_marks row", what)
		return
	}
	written := at(mark, commit)
	if written < 0 {
		t.Errorf("%s: no COMMIT follows the mark's upsert", what)
		return
	}
	if inserted := at(mark, containing(versionInsertTag)); inserted >= 0 && inserted < written {
		t.Errorf("%s: the write's own transaction inserts the drained version (statement %d) before its COMMIT (statement %d)", what, inserted, written)
	}
	steps := []struct {
		name  string
		match func(string) bool
	}{
		{"the issue's owner lock", containing(ownerLockHead)},
		{"the lock of the issue's room", containing(roomLockHead)},
		{"the read of the mark", containing(markReadHead)},
		{"the version's insert", containing(versionInsertTag)},
		{"the clear of the mark", containing(markClearHead)},
		{"the drain's COMMIT", commit},
	}
	from := written + 1
	for _, step := range steps {
		found := at(from, step.match)
		if found < 0 {
			t.Errorf("%s: after the write's COMMIT the drain sent no %s in its order: owner lock, room lock, mark read, version insert, mark clear, COMMIT", what, step.name)
			return
		}
		t.Logf("%s: %s is statement %d", what, step.name, found)
		from = found + 1
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
//     carries nothing, and the approval reads changed since.
//   - With the request's drain suppressed, a result and its retraction on the issue leave one mark,
//     caused by the greater event and the greater tuple sequence of the two, with the retraction's
//     writer; the tick of the block-index reconciliation writes the version under that event, in the
//     drain's name of the newest marking tuple's writer, and clears the mark, and a second tick
//     writes nothing.
//   - A result on an issue that has not converted marks nothing and runs no drain.
//
// The node PATCH's eventless mark (greatest_event null) and its drain are held by
// TestAMoveMarksItsIssuesQueriesOwedAndTheDrainWritesThem (api/node_move_drain_test.go).
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
	retractOne := func(t *testing.T, result model.CheckResult) int64 {
		t.Helper()
		if retracted := retractAsPerson(t, handler, result.ID, "The run measured the wrong build."); retracted.Code != http.StatusCreated {
			t.Fatalf("retract the result %s: status=%d body=%s", result.ID, retracted.Code, retracted.Body.String())
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
		requireDrainTrace(t, "a recorded result", sent.Statements()[from:])
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
		event := retractOne(t, carried)
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

	t.Run("the tick drains a mark a request left", func(t *testing.T) {
		before := newestQueryVersion(t, database, verification.id)
		drains := probe.drains.Load()
		probe.suppress.Store(true)
		result, recordedEvent := recordOne(t)
		retractedEvent := retractOne(t, result)
		probe.suppress.Store(false)
		if ran := probe.drains.Load(); ran != drains {
			t.Fatalf("the suppressed requests ran %d drains", ran-drains)
		}
		seq := max(greatestSeqOf(t, database, recordedEvent), greatestSeqOf(t, database, retractedEvent))
		requireMarkCauses(t, database, "a result and its retraction", issue.Key, max(recordedEvent, retractedEvent), seq, "alice")
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
		if _, actor := requireVersionEvent(t, database, verification.id, version.number); actor.Kind != "user" || actor.ID != "alice" {
			t.Errorf("the tick's artifact.version names %+v, want alice, who wrote the tuple at the mark's greatest_seq", actor)
		}

		again, err := documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil)
		if err != nil {
			t.Fatalf("a second tick of the block-index reconciliation: %v", err)
		}
		if held := newestQueryVersion(t, database, verification.id); again.Queries != 0 || held.number != version.number {
			t.Errorf("a second tick with no mark drained %d marks and left version %d, want none and %d", again.Queries, held.number, version.number)
		}
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
