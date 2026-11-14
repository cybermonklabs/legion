package docs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// One resumption pass arms at most settlementResumeBatch documents, closed and open issues'
// together, and a document a pass armed waits behind every document no pass has armed yet
// (resumeOwedSettlements): its row's tried_at says so in the database, so the order holds
// whichever process runs the next pass. Each test seeds documents whose every settlement fails,
// marked before all the others, in two ways: a settlement whose version write the test fails
// (afterSettleVersionWrite), which retries and then fails its room, and a document outside the
// Proof schema (the fixture of TestSettleSkipsVersionWhenTreeLeavesTheSchema), whose settlement
// ends on ErrDocSchema with no retry and no failure counted. Neither commits, so both keep their
// rows. The other documents must settle within the passes their count needs: their count over the
// bound, rounded up, plus one. The failing counts are chosen for that bound: with N others, more
// than 50 + (50 × ⌈N/50⌉ − N) failing documents would take enough of the passes that the others
// miss it on a correct build. The tests run each pass at age 0 and wait until every settlement it
// armed has ended before the next, so each pass meets every row free.

const (
	// owedBeforeThePass is the paragraph each seeded document's live text holds, which its store
	// holds too (SeedText).
	owedBeforeThePass = "Owed before the pass."
	// versionedBeforeThePass is each seeded document's version 1. It is not the live text, so each
	// settlement writes a version, and the one the test fails fails at that write.
	versionedBeforeThePass = "Versioned before the document was edited.\n"
)

// errEverySettlementFails is what afterSettleVersionWrite answers for a document whose every
// settlement the test fails.
var errEverySettlementFails = errors.New("the test fails every settlement of this document")

// TestTheResumptionArmsNoMoreThanItsBoundInOnePass seeds, in the order of their pending rows, 5
// documents outside the Proof schema and 50 whose version write fails, then 120 others, by turns
// on CORE-2, which is closed, and on CORE-1, which is open: the bound is one bound over both
// issues. The passes go as settlementResumeBatch = 50 makes them:
//
//  1. the 55 failing documents are all untried, so the pass takes the 50 marked first;
//  2. the 5 failing documents not yet tried and the 45 others marked first;
//  3. the next 50 others;
//  4. the last 25 others, and then the tried documents, oldest tried_at first: the 25 marked first
//     of the 50 pass 1 tried, which share its tried_at.
//
// So the 120 others have all settled after ⌈120/50⌉ + 1 = 4 passes, and no pass arms more than 50.
// The failing documents still owe a settlement at the end.
func TestTheResumptionArmsNoMoreThanItsBoundInOnePass(t *testing.T) {
	if settlementResumeBatch != 50 {
		t.Fatalf("settlementResumeBatch is %d, want 50: the spec gives the reason for 50, and this test's counts are chosen for it", settlementResumeBatch)
	}
	database, service := boundedResumptionService(t)
	schema := seedOwedDocuments(t, service, "CORE", "CORE-1", "outside-the-schema", 5)
	for _, id := range schema {
		leaveTheSchema(t, service, id)
	}
	hooked := seedOwedDocuments(t, service, "CORE", "CORE-1", "failing", 50)
	open := seedOwedDocuments(t, service, "CORE", "CORE-1", "open", 60)
	closed := seedOwedDocuments(t, service, "CORE", "CORE-2", "closed", 60)
	setIssueClosedHere(t, service, "CORE-2", true)
	var others []string
	for i := range closed {
		others = append(others, closed[i], open[i])
	}
	failing := slices.Concat(schema, hooked)
	order := slices.Concat(failing, others)
	markOwedInOrder(t, database, order)
	kinds := documentKinds(map[string][]string{
		"outside the schema": schema, "failing at the version write": hooked,
		"on the closed CORE-2": closed, "on the open CORE-1": open,
	})
	service.afterSettleVersionWrite = failOnTheHook(hooked)
	service.settle = closedSettlementDelay

	for i, want := range [][]string{
		failing[:50],
		slices.Concat(failing[50:], others[:45]),
		others[45:95],
		slices.Concat(others[95:], failing[:25]),
	} {
		pass := fmt.Sprintf("pass %d", i+1)
		armed := resumptionPass(t, service, order)
		if len(armed) > settlementResumeBatch {
			t.Fatalf("%s armed %d documents (%s), above the bound of %d", pass, len(armed), describeDocuments(armed, kinds), settlementResumeBatch)
		}
		requireArmed(t, pass, armed, want, kinds)
		waitForSettlementsToEnd(t, service, armed)
	}
	if left := unsettledDocuments(t, database, others); len(left) > 0 {
		t.Fatalf("%d of the 120 other documents (%s) have not settled after 4 passes, the most their count allows", len(left), describeDocuments(left, kinds))
	}
	requireStillFailing(t, database, failing)
}

// TestFailingSettlementsWaitBehindOnWhicheverServiceRunsThePass runs the resumption on two
// services of one database (newProcessPair), one pass on each. It seeds, in the order of their
// pending rows, 5 documents outside the Proof schema and 55 whose version write fails, then 40
// others, all on DOC-1. Pass 1 runs on task A and takes the 50 failing documents marked first.
// Pass 2 runs on task B, which ran none of them: it takes the 10 failing documents no pass has
// tried and the 40 others, since what pass 1 tried is in the rows, not in task A's memory. So the
// 40 have all settled after ⌈40/50⌉ + 1 = 2 passes. Read from B's record of its passes, pass 2 ran
// there, and A's record shows it armed nothing more.
func TestFailingSettlementsWaitBehindOnWhicheverServiceRunsThePass(t *testing.T) {
	if settlementResumeBatch != 50 {
		t.Fatalf("settlementResumeBatch is %d, want 50: this test's counts are chosen for it", settlementResumeBatch)
	}
	pair := newProcessPair(t)
	a, b := pair.taskA, pair.taskB
	// Each pass a service runs is counted on its own hooks (countSettlementResumption).
	a.hooks, b.hooks = newTestHooks(), newTestHooks()
	schema := seedOwedDocuments(t, a, "DOC", "DOC-1", "outside-the-schema", 5)
	for _, id := range schema {
		leaveTheSchema(t, a, id)
	}
	hooked := seedOwedDocuments(t, a, "DOC", "DOC-1", "failing", 55)
	others := seedOwedDocuments(t, a, "DOC", "DOC-1", "owed", 40)
	failing := slices.Concat(schema, hooked)
	order := slices.Concat(failing, others)
	markOwedInOrder(t, pair.database, order)
	kinds := documentKinds(map[string][]string{
		"outside the schema": schema, "failing at the version write": hooked, "other": others,
	})
	fail := failOnTheHook(hooked)
	a.afterSettleVersionWrite, b.afterSettleVersionWrite = fail, fail
	a.settle, b.settle = closedSettlementDelay, closedSettlementDelay

	first := resumptionPass(t, a, order)
	requireArmed(t, "pass 1, on task A", first, failing[:50], kinds)
	waitForSettlementsToEnd(t, a, first)
	closeIdleConnections(pair.database)

	armedOnA := resumptionCounts(t, a, order)
	second := resumptionPass(t, b, order)
	if len(second) == 0 {
		t.Fatal("pass 2 armed no document on task B, by task B's record of its passes")
	}
	if after := resumptionCounts(t, a, order); !maps.Equal(after, armedOnA) {
		t.Fatal("task A's record of its passes moved while pass 2 ran on task B: the pass did not run on task B alone")
	}
	waitForSettlementsToEnd(t, b, second)
	if left := unsettledDocuments(t, pair.database, others); len(left) > 0 {
		t.Fatalf("%d of the 40 other documents have not settled after 2 passes, the most their count allows: pass 2 on task B armed %d documents (%s)",
			len(left), len(second), describeDocuments(second, kinds))
	}
	requireArmed(t, "pass 2, on task B", second, slices.Concat(failing[50:], others), kinds)
	requireStillFailing(t, pair.database, failing)
}

// boundedResumptionService is a document service with its test hooks on and an hour's settle
// delay, on a database holding project CORE and its issues CORE-1 and CORE-2, both open.
func boundedResumptionService(t *testing.T) (*store.Store, *Service) {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U'),
			('CORE-2', 'CORE', 2, 'Two', '{"kind":"user","id":"alice"}', 'V');
	`); err != nil {
		t.Fatalf("seed project CORE: %v", err)
	}
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
		TestHooks: true,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return database, service
}

// seedOwedDocuments creates count documents of issueKey in project projectKey, named slug-1 to
// slug-count, in one statement, each with version 1 reading versionedBeforeThePass; service then
// seeds each one's live text, owedBeforeThePass, into the store (SeedText), which leaves it owing
// a settlement that writes a version. It answers their ids.
func seedOwedDocuments(t *testing.T, service *Service, projectKey, issueKey, slug string, count int) []string {
	t.Helper()
	rows, err := service.store.Pool.Query(context.Background(), `
		with created as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			select $1, $2, $3 || '-' || n, $3 || '-' || n || '.md', 'doc', false, '{"kind":"user","id":"alice"}'
			from generate_series(1, $4::int) n
			returning id
		), versions as (
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select id, 1, $5, '[{"kind":"user","id":"alice"}]' from created
		)
		select id::text from created
	`, issueKey, projectKey, slug, count, versionedBeforeThePass)
	if err != nil {
		t.Fatalf("create the %s documents: %v", slug, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("create the %s documents: %v", slug, err)
	}
	if len(ids) != count {
		t.Fatalf("created %d %s documents, want %d", len(ids), slug, count)
	}
	for _, id := range ids {
		seedServiceText(t, service, id, owedBeforeThePass+"\n")
	}
	return ids
}

// leaveTheSchema has service's room of artifactID take an element the Proof schema does not hold,
// as TestSettleSkipsVersionWhenTreeLeavesTheSchema does, and waits until the store holds it. Every
// settlement of the document then ends on ErrDocSchema and leaves its row.
func leaveTheSchema(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	ctx := context.Background()
	if err := service.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		transact(func(txn *crdt.Transaction) {
			fragment.InsertElement(txn, 0, crdt.NewYXmlElement("callout"))
		})
	}); err != nil {
		t.Fatalf("put the document outside the Proof schema: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	if _, err := service.Text(ctx, artifactID); !errors.Is(err, ErrDocSchema) {
		t.Fatalf("read the document put outside the Proof schema = %v, want ErrDocSchema", err)
	}
}

// markOwedInOrder stamps the pending-settlement rows of ids an hour ago and a second apart, in
// the order of ids, so the resumption meets the untried rows in that order. It fails unless each
// of ids owes a settlement, no other document does, and no row has been tried.
func markOwedInOrder(t *testing.T, database *store.Store, ids []string) {
	t.Helper()
	ctx := context.Background()
	marked, err := database.Pool.Exec(ctx, `
		update doc_settlements_pending p set marked_at = now() - interval '1 hour' + make_interval(secs => o.n)
		from unnest($1::uuid[]) with ordinality as o(id, n)
		where p.artifact_id = o.id
	`, ids)
	if err != nil {
		t.Fatalf("mark the seeded documents owed in order: %v", err)
	}
	if marked.RowsAffected() != int64(len(ids)) {
		t.Fatalf("%d of the %d seeded documents owe a settlement, want every one", marked.RowsAffected(), len(ids))
	}
	var owed, tried int
	if err := database.Pool.QueryRow(ctx, `
		select count(*), count(tried_at) from doc_settlements_pending
	`).Scan(&owed, &tried); err != nil {
		t.Fatalf("count the owed settlements: %v", err)
	}
	if owed != len(ids) || tried != 0 {
		t.Fatalf("%d documents owe a settlement and %d of them were tried, want the %d seeded ones and none tried", owed, tried, len(ids))
	}
}

// failOnTheHook is an afterSettleVersionWrite that fails the version write of each document of
// ids, so its settlement stops short and is retried until its room fails (retrySettleLocked), and
// lets every other settlement go on.
func failOnTheHook(ids []string) func(room string) error {
	fails := make(map[string]bool, len(ids))
	for _, id := range ids {
		fails[id] = true
	}
	return func(room string) error {
		if fails[room] {
			return errEverySettlementFails
		}
		return nil
	}
}

// resumptionPass runs one resumption pass on service for every row however young
// (resumeOwedSettlements) and answers the documents of ids it armed, in the order of ids, read from
// service's own record of its passes (countSettlementResumption). It fails unless the pass armed
// each of them at most once.
func resumptionPass(t *testing.T, service *Service, ids []string) []string {
	t.Helper()
	before := resumptionCounts(t, service, ids)
	if err := service.resumeOwedSettlements(context.Background(), 0); err != nil {
		t.Fatalf("run a resumption pass: %v", err)
	}
	after := resumptionCounts(t, service, ids)
	var armed []string
	for _, id := range ids {
		switch after[id] - before[id] {
		case 0:
		case 1:
			armed = append(armed, id)
		default:
			t.Fatalf("one resumption pass armed document %s %d times, want at most once", id, after[id]-before[id])
		}
	}
	return armed
}

// resumptionCounts is how many of service's resumption passes armed each document of ids.
func resumptionCounts(t *testing.T, service *Service, ids []string) map[string]int {
	t.Helper()
	counts := make(map[string]int, len(ids))
	for _, id := range ids {
		counters, err := service.RoomCounters(id)
		if err != nil {
			t.Fatalf("read the room counters of %s: %v", id, err)
		}
		counts[id] = len(counters.SettlementResumptions)
	}
	return counts
}

// waitForSettlementsToEnd waits until no settlement of rooms is armed or running on service and
// no room of them is failed and not yet evicted: each settlement a pass armed has ended, its
// retries and its room's failure with it (retrySettleLocked, failRoomLocked), so the next pass
// meets no row a settlement holds.
func waitForSettlementsToEnd(t *testing.T, service *Service, rooms []string) {
	t.Helper()
	waitFor(t, roomWait(t), fmt.Sprintf("the %d settlements the pass armed to end", len(rooms)), func() bool {
		return !slices.ContainsFunc(rooms, func(room string) bool { return settlementUnderway(service, room) })
	})
}

// settlementUnderway is whether a settlement of room is armed or running on service, or its room
// has failed and its eviction has not handed the room's record over yet (evictFailed).
func settlementUnderway(service *Service, room string) bool {
	state := service.lockExistingState(room)
	if state == nil {
		return false
	}
	defer service.unlockState(room, state)
	return state.failed != nil || state.settling > 0 || service.isSettleTimerArmed(state.settle)
}

// closeIdleConnections closes each connection database's shared pool holds idle, which pgx keeps
// for half an hour. A pass's settlements of one issue's documents queue on the issue's row
// (lockArtifactOwner), each holding a connection of its process's shared pool, so one pass leaves
// up to settlementResumeBatch of them idle. Two processes holding that many pass the 100
// connections a default Postgres serves, which the test server is (cmd/dispatch/README.md sizes
// the database for two processes), so the process that ran a pass gives them back before the
// other runs the next.
func closeIdleConnections(database *store.Store) {
	ctx := context.Background()
	for _, conn := range database.Pool.AcquireAllIdle(ctx) {
		_ = conn.Hijack().Close(ctx)
	}
}

// requireArmed fails unless pass armed exactly the documents of want, and names what it armed
// instead by kind.
func requireArmed(t *testing.T, pass string, armed, want []string, kinds map[string]string) {
	t.Helper()
	if len(armed) == 0 {
		t.Fatalf("%s armed no document, want %d (%s)", pass, len(want), describeDocuments(want, kinds))
	}
	var missing, extra []string
	for _, id := range want {
		if !slices.Contains(armed, id) {
			missing = append(missing, id)
		}
	}
	for _, id := range armed {
		if !slices.Contains(want, id) {
			extra = append(extra, id)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("%s armed %d documents (%s), want %d (%s): it left out %d (%s) and armed %d it should not have (%s)",
			pass, len(armed), describeDocuments(armed, kinds), len(want), describeDocuments(want, kinds),
			len(missing), describeDocuments(missing, kinds), len(extra), describeDocuments(extra, kinds))
	}
}

// unsettledDocuments is the documents of ids that still owe a settlement, or that have no version
// past version 1 holding owedBeforeThePass.
func unsettledDocuments(t *testing.T, database *store.Store, ids []string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select o.id::text from unnest($1::uuid[]) with ordinality as o(id, n)
		where exists (select from doc_settlements_pending p where p.artifact_id = o.id)
			or not exists (
				select from artifact_versions v
				where v.artifact_id = o.id and v.number > 1 and strpos(v.markdown, $2) > 0
			)
		order by o.n
	`, ids, owedBeforeThePass)
	if err != nil {
		t.Fatalf("read which documents have not settled: %v", err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read which documents have not settled: %v", err)
	}
	return left
}

// requireStillFailing fails unless each document of failing still owes a settlement and has no
// version past version 1: its every settlement failed, as the test made it.
func requireStillFailing(t *testing.T, database *store.Store, failing []string) {
	t.Helper()
	var owing int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from unnest($1::uuid[]) o(id)
		where exists (select from doc_settlements_pending p where p.artifact_id = o.id)
			and not exists (select from artifact_versions v where v.artifact_id = o.id and v.number > 1)
	`, failing).Scan(&owing); err != nil {
		t.Fatalf("read whether the failing documents still owe a settlement: %v", err)
	}
	if owing != len(failing) {
		t.Fatalf("%d of the %d failing documents still owe a settlement with no new version, want every one: a settlement the test fails committed", owing, len(failing))
	}
}

// documentKinds names each document by the kind it was seeded as.
func documentKinds(groups map[string][]string) map[string]string {
	kinds := make(map[string]string)
	for kind, ids := range groups {
		for _, id := range ids {
			kinds[id] = kind
		}
	}
	return kinds
}

// describeDocuments counts ids by kind, the kinds in alphabetical order: "45 failing at the
// version write, 5 outside the schema". It is "none" for no document.
func describeDocuments(ids []string, kinds map[string]string) string {
	counts := make(map[string]int)
	for _, id := range ids {
		counts[kinds[id]]++
	}
	names := make([]string, 0, len(counts))
	for kind := range counts {
		names = append(names, kind)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, kind := range names {
		parts = append(parts, fmt.Sprintf("%d %s", counts[kind], kind))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
