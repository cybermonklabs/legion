package outbox

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
	"github.com/sjawhar/envoy/internal/roles"
)

// TestAClaimStatedAgainUnderItsEventLeavesTheRoleHeld is D5's case of a retried event. The
// publisher records a role's holder under the first event of a batch that routed to the role
// (deliveredGraph), and an event its record left pending is read again and resolves the role
// again: event 10 resolves s1, event 11 resolves s2, and event 10, read again, resolves s1 again,
// the newest of the three resolutions, which the order of resolutions does not hold back. Written
// row by row, the retry appends the retraction of s2 under event 10 and loses the assertion of s1
// to the repeat key, which already holds s1 under event 10, so nobody holds the role. A claim is
// stated whole or not at all (graphstore's claimArms): the retry, whose event already stated this
// claim, appends nothing, and the role keeps the holder the later event resolved. A session's
// part_of the role it claimed is a claim of the same shape and is held to the same rule.
func TestAClaimStatedAgainUnderItsEventLeavesTheRoleHeld(t *testing.T) {
	t.Parallel()
	const (
		role    = "claim-retry-role"
		before  = "claim-retry-before"
		first   = "019a1c0a-3333-7000-8000-000000000001"
		second  = "019a1c0a-3333-7000-8000-000000000002"
		earlier = int64(9430010)
		later   = int64(9430011)
	)
	ctx := context.Background()
	database := storetest.Open(t)
	deps := Deps{Store: database}
	deps.lanes = newLanes(func() {})
	resolved := time.Now()
	record := func(event int64, holder, parent string, at time.Time) {
		t.Helper()
		claims := graphWrite{claims: []graphstore.Claim{
			{
				FromKind: graphstore.KindRole, FromID: role, Kind: graphstore.TupleHeldBy,
				ToKind: graphstore.KindSession, ToID: holder, At: at, Who: graphWriter, SourceEvent: event,
			},
			partOf(graphstore.KindSession, first, graphstore.KindRole, parent, at, event),
		}}
		if _, err := recordBatch(ctx, deps, []outcome{{id: event, state: released}}, uuid.New(), claims); err != nil {
			t.Fatalf("record event %d: %v", event, err)
		}
	}
	record(earlier, first, role, resolved)
	record(later, second, before, resolved.Add(time.Second))
	record(earlier, first, role, resolved.Add(2*time.Second))

	if holder := currentHolder(t, database, role); holder != second {
		t.Fatalf("the role is held by %q after the retry of event %d, want %s, the holder event %d resolved: a retry that loses its assertion and keeps its retraction leaves nobody holding the role",
			holder, earlier, second, later)
	}
	want := []claimTuple{
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: first, Asserted: true},
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: first, Asserted: false},
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: second, Asserted: true},
	}
	if history := claimTuples(t, database, role); !slices.Equal(history, want) {
		t.Fatalf("the role's claim reads %+v, want %+v: the retry appends no row", history, want)
	}
	parts, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindSession, first, []string{graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read what %s is part of: %v", first, err)
	}
	if len(parts) != 1 || parts[0].ToID != before {
		t.Fatalf("%s is part of %+v after the retry, want %s alone", first, parts, before)
	}
}

// TestARetriedEventLeavesTheHolderANewerEventStated is the retry
// TestAClaimStatedAgainUnderItsEventLeavesTheRoleHeld cannot see: one whose event's first record
// wrote no row. Event 9 resolves s1; event 10 resolves s1 again, which the role holds already, so
// its record appends nothing, and the log holds event 9's row alone; event 11 moves the role to
// s2; and event 10's record is written again, carrying the resolution it made, s1 as event 10's
// batch read it before event 11's did. The log holds no row of event 10 for the retry to find, so
// only the order of the resolutions tells it that a newer one stated the claim (graphstore's
// claimArms): an older resolution never moves a claim a newer one stated, so the retry appends
// nothing and the role keeps the holder event 11 resolved. The claims are made as the publisher
// makes them, from each batch's resolution (resolvedRoleWrite), so a record that dated its claims
// by when it is written would date the retry newest and hand the role back to s1. The publisher
// reads that holder back (claimedARole), so a reverted one would change how the next mention is
// recorded too.
func TestARetriedEventLeavesTheHolderANewerEventStated(t *testing.T) {
	t.Parallel()
	const (
		role    = "claim-older-role"
		first   = "019a1c0a-5555-7000-8000-000000000001"
		second  = "019a1c0a-5555-7000-8000-000000000002"
		stated  = int64(9450009)
		retried = int64(9450010)
		newer   = int64(9450011)
	)
	database := storetest.Open(t)
	deps := Deps{Store: database}
	deps.lanes = newLanes(func() {})
	resolved := time.Now()
	statedAt, retriedAt, newerAt := resolved, resolved.Add(time.Second), resolved.Add(2*time.Second)
	recordResolution(t, deps, role, stated, first, statedAt)
	recordResolution(t, deps, role, retried, first, retriedAt)
	held := []claimTuple{{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: first, Asserted: true}}
	if history := claimTuples(t, database, role); !slices.Equal(history, held) {
		t.Fatalf("the role's claim reads %+v after event %d resolved %s again, want %+v: a claim the role holds already appends no row",
			history, retried, first, held)
	}
	if rows := tuplesUnder(t, database, retried); rows != 0 {
		t.Fatalf("the log holds %d row(s) under event %d after it resolved the holder the role holds, want none: the log holds event %d's row alone",
			rows, retried, stated)
	}
	recordResolution(t, deps, role, newer, second, newerAt)
	// The retry carries the resolution its first record made, older than event 11's.
	recordResolution(t, deps, role, retried, first, retriedAt)

	if holder := currentHolder(t, database, role); holder != second {
		t.Fatalf("the role is held by %q after the retry of event %d, want %s, the holder event %d stated: an older resolution never moves a claim a newer one stated",
			holder, retried, second, newer)
	}
	if rows := tuplesUnder(t, database, retried); rows != 0 {
		t.Fatalf("the log holds %d row(s) under event %d after its retry, want none: the retry appends no row", rows, retried)
	}
	want := []claimTuple{
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: first, Asserted: true},
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: first, Asserted: false},
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: second, Asserted: true},
	}
	if history := claimTuples(t, database, role); !slices.Equal(history, want) {
		t.Fatalf("the role's claim reads %+v after the retry of event %d, want %+v: the retry appends no row", history, retried, want)
	}
}

// TestARetriedEventLeavesARoleANewerEventUnclaimedUnheld is the same rule where the newer
// resolution ended the relation rather than moved it. Event 9 resolves s1; event 10 resolves s1
// again, which the role holds already, so its record appends nothing; event 11 reads the role
// unclaimed, a claim with no far end, which retracts s1; and event 10's record is written again,
// carrying the resolution it made before event 11's. The role then holds no far end, so there is
// no current far end whose row the retry could be held back by: the newest row of each far end
// counts whether it asserts the far end or retracts it (graphstore's claimArms), and event 11's
// retraction of s1 is dated after the retry's resolution. The retry appends nothing, and the role
// stays unheld, as the newer resolution left it.
func TestARetriedEventLeavesARoleANewerEventUnclaimedUnheld(t *testing.T) {
	t.Parallel()
	const (
		role    = "claim-unclaimed-role"
		first   = "019a1c0a-6666-7000-8000-000000000001"
		stated  = int64(9460009)
		retried = int64(9460010)
		newer   = int64(9460011)
	)
	database := storetest.Open(t)
	deps := Deps{Store: database}
	deps.lanes = newLanes(func() {})
	resolved := time.Now()
	statedAt, retriedAt, newerAt := resolved, resolved.Add(time.Second), resolved.Add(2*time.Second)
	recordResolution(t, deps, role, stated, first, statedAt)
	recordResolution(t, deps, role, retried, first, retriedAt)
	recordResolution(t, deps, role, newer, "", newerAt)
	// The retry carries the resolution its first record made, older than event 11's.
	recordResolution(t, deps, role, retried, first, retriedAt)

	if holder := currentHolder(t, database, role); holder != "" {
		t.Fatalf("the role is held by %q after the retry of event %d, want none, as event %d left it unclaimed: an older resolution never moves a claim a newer one stated",
			holder, retried, newer)
	}
	if rows := tuplesUnder(t, database, retried); rows != 0 {
		t.Fatalf("the log holds %d row(s) under event %d after its retry, want none: the retry appends no row", rows, retried)
	}
	want := []claimTuple{
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: first, Asserted: true},
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: first, Asserted: false},
	}
	if history := claimTuples(t, database, role); !slices.Equal(history, want) {
		t.Fatalf("the role's claim reads %+v after the retry of event %d, want %+v: the retry appends no row", history, retried, want)
	}
}

// TestALaterResolutionRecordedUnderAnOlderEventMovesTheRole is the case an order of claims by
// their events got wrong. The publisher resolves a role's holder when it publishes a batch, and
// owners publish independently, so an event backed off is published after a later event of
// another owner: issue Y's event 11 records s1, resolved at t1; the role moves to s2; and issue
// X's event 10, published after its back-off, resolves s2 at t2, after t1, and its batch records
// the claim under event 10, the first of its events that routed to the role (deliveredGraph).
// That is the newest resolution the store is told, so it moves the role: the retraction of s1 and
// the assertion of s2, both under event 10, although event 11's row names a later event
// (graphstore's claimArms).
func TestALaterResolutionRecordedUnderAnOlderEventMovesTheRole(t *testing.T) {
	t.Parallel()
	const (
		role   = "claim-later-resolution-role"
		first  = "019a1c0a-7777-7000-8000-000000000001"
		second = "019a1c0a-7777-7000-8000-000000000002"
		older  = int64(9470010)
		newer  = int64(9470011)
	)
	database := storetest.Open(t)
	deps := Deps{Store: database}
	deps.lanes = newLanes(func() {})
	firstAt := time.Now()
	secondAt := firstAt.Add(time.Second)
	recordResolution(t, deps, role, newer, first, firstAt)
	recordResolution(t, deps, role, older, second, secondAt)

	if holder := currentHolder(t, database, role); holder != second {
		t.Fatalf("the role is held by %q after a resolution at %s recorded under the older event %d, want %s: claims are ordered by when their holder was resolved",
			holder, secondAt.Format(time.RFC3339Nano), older, second)
	}
	want := []claimTuple{
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: first, Asserted: false},
		{Kind: graphstore.TupleHeldBy, ToKind: graphstore.KindSession, ToID: second, Asserted: true},
	}
	if under := claimTuplesUnder(t, database, role, older); !slices.Equal(under, want) {
		t.Fatalf("the role's claim under event %d reads %+v, want %+v: the resolution at %s retracts %s and asserts %s under the event its batch recorded it under",
			older, under, want, secondAt.Format(time.RFC3339Nano), first, second)
	}
}

// recordResolution records event released, as a batch whose one event routed to role and whose
// resolution read holder at resolvedAt states it (resolvedRoleWrite).
func recordResolution(t *testing.T, deps Deps, role string, event int64, holder string, resolvedAt time.Time) {
	t.Helper()
	reached := resolvedRoleWrite(role, event, holder, resolvedAt)
	if _, err := recordBatch(context.Background(), deps, []outcome{{id: event, state: released}}, uuid.New(), reached); err != nil {
		t.Fatalf("record event %d: %v", event, err)
	}
}

// resolvedRoleWrite is what the publisher's record of one event states when the event routed to
// role and the batch's resolution read holder at resolvedAt, the role unclaimed for an empty
// holder: the resolution made into the route's forward (forwardOf) and the forward into the
// record's facts (deliveredGraph), the role's held_by and its holder's part_of the role among
// them. The record's other rows are dated by when it is made.
func resolvedRoleWrite(role string, event int64, holder string, resolvedAt time.Time) graphWrite {
	resolution := roles.Resolution{State: roles.Unclaimed}
	if holder != "" {
		resolution = roles.Resolution{State: roles.Live, Holder: holder}
	}
	routed := &batchEvent{
		pendingEvent: pendingEvent{event: model.Event{ID: event}},
		destinationsOf: []destination{{
			subject: contracts.RoleTopicPrefix + role, role: role,
			forward: forwardOf(role, contracts.Envelope{}, resolution, nil, resolvedAt),
		}},
	}
	return deliveredGraph([]*batchEvent{routed}, nil, time.Now())
}

// tuplesUnder is how many rows of the log name event as their cause, whatever they are about.
func tuplesUnder(t *testing.T, database *store.Store, event int64) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from graph_tuples where source_event = $1`, event).Scan(&rows); err != nil {
		t.Fatalf("read the rows under event %d: %v", event, err)
	}
	return rows
}

// claimTuplesUnder is the held_by tuples of one role that name event as their cause, in the
// order the log holds them.
func claimTuplesUnder(t *testing.T, database *store.Store, role string, event int64) []claimTuple {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select kind, to_kind, to_id, asserted from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and source_event = $4
		order by seq
	`, graphstore.KindRole, role, graphstore.TupleHeldBy, event)
	if err != nil {
		t.Fatalf("read the claims of %s under event %d: %v", role, event, err)
	}
	defer rows.Close()
	var claims []claimTuple
	for rows.Next() {
		var claim claimTuple
		if err := rows.Scan(&claim.Kind, &claim.ToKind, &claim.ToID, &claim.Asserted); err != nil {
			t.Fatalf("scan a claim of %s under event %d: %v", role, event, err)
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the claims of %s under event %d: %v", role, event, err)
	}
	return claims
}

// TestAFallbackCopyThePassPublishedIsADelivery holds the fallback pass's record to R14 and R15: a
// copy of a comment the pass published to a session's own topic, once the mention's attempt past
// its lease failed, is a delivery, so the record appends its delivered_to tuple from the comment to
// the session, caused by the comment.created event the copy is the envelope of, and the session's
// node beside it. A copy the pass did not publish delivered nothing and states nothing.
//
// recordFallbackCopies is a store write call site graphstore's TestEveryStoreWriteCallSiteIsListed
// lists, and this is its lock order: one statement on a pooled connection, like recordBatch, that
// takes no owner row and appends no event, writes the session's node row first and the tuple
// after it. Both are held, each in a transaction of its own, and the record must queue behind the
// node before the tuple (lockstep.RequireTheStoreLockOrder).
func TestAFallbackCopyThePassPublishedIsADelivery(t *testing.T) {
	t.Parallel()
	const (
		comment = "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f30"
		reached = "019a1c0a-4444-7000-8000-000000000001"
		missed  = "019a1c0a-4444-7000-8000-000000000002"
		created = int64(9440001)
	)
	ctx := context.Background()
	database := storetest.Open(t)
	single := storetest.SingleConnection(t, database)
	at := time.Now()
	node := graphstore.SessionNode(reached, graphWriter, at, created)
	tuple := deliveredTo(graphstore.KindComment, comment, graphstore.KindSession, reached, at, created)

	nodeHold := &lockstep.Hold{
		Name: "the reached session's node row", Step: lockstep.StepNode, Taken: true,
		Kind: node.Kind, ID: node.ID, Statement: "with hold as (",
	}
	holdStoreWrite(t, database, nodeHold, []graphstore.Node{node}, nil)
	tupleHold := &lockstep.Hold{
		Name: "the comment's delivered_to tuple", Step: lockstep.StepTuple, Taken: true, Statement: "with hold as (",
	}
	holdStoreWrite(t, database, tupleHold, nil, []graphstore.Tuple{tuple})

	var pid int64
	if err := single.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backend the record runs on: %v", err)
	}
	records := []fallbackRecord{
		{held: fallbackCopy{commentID: comment, destination: contracts.LegacyAgentSubject(reached), eventID: created, claim: at}, settled: true, published: true},
		{held: fallbackCopy{commentID: comment, destination: contracts.LegacyAgentSubject(missed), eventID: created, claim: at}, failed: true},
	}
	done := make(chan error, 1)
	go func() {
		_, err := recordFallbackCopies(ctx, Deps{Store: single}, uuid.New(), records)
		done <- err
	}()
	lockstep.Follow(t, database, "recordFallbackCopies", uint32(pid), done, []*lockstep.Hold{nodeHold, tupleHold})

	requireTuple(t, database, graphstore.KindComment, comment, graphstore.TupleDeliveredTo, graphstore.KindSession, reached, true)
	for _, check := range []struct {
		session    string
		nodes, ins int
	}{{reached, 1, 1}, {missed, 0, 0}} {
		var nodes, ins int
		if err := database.Pool.QueryRow(ctx, `
			select (select count(*) from graph_nodes where kind = $1 and id = $2),
			       (select count(*) from graph_tuples where to_kind = $1 and to_id = $2 and kind = $3 and source_event = $4)
		`, graphstore.KindSession, check.session, graphstore.TupleDeliveredTo, created).Scan(&nodes, &ins); err != nil {
			t.Fatalf("read what the record stated of %s: %v", check.session, err)
		}
		if nodes != check.nodes || ins != check.ins {
			t.Errorf("the record left %d node(s) of %s and %d delivered_to tuple(s) into it, want %d and %d",
				nodes, check.session, ins, check.nodes, check.ins)
		}
	}
	// A later pass that publishes the same copy again states nothing new under the repeat key.
	if _, err := recordFallbackCopies(ctx, Deps{Store: database}, uuid.New(), records[:1]); err != nil {
		t.Fatalf("record the copy again: %v", err)
	}
	var repeated int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples where from_id = $1 and kind = $2 and to_id = $3
	`, comment, graphstore.TupleDeliveredTo, reached).Scan(&repeated); err != nil {
		t.Fatalf("count the copy's deliveries: %v", err)
	}
	if repeated != 1 {
		t.Fatalf("the copy recorded twice holds %d delivered_to tuple(s), want one", repeated)
	}
}

// TestAnEventTwoProcessesRecordedKeepsItsArrayTheFarEndsOfItsTuples holds recordBatch to the
// projection deliveredGraph states - an event's published_destinations names, among its session
// and role topics, exactly the far ends of the delivered_to tuples under it (R2's session half) -
// when the publisher hold moves mid-batch. The holder publishes a comment's event to s2 and
// records it; the process that lost the hold had sent the same event to s3 before it stopped, and
// records its own batch after. Its tuple to s3 is a delivery that happened, and the array names
// s3's topic beside s2's, while the row's publish state stays the holder's.
func TestAnEventTwoProcessesRecordedKeepsItsArrayTheFarEndsOfItsTuples(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	deps := Deps{Store: database}
	deps.lanes = newLanes(func() {})
	seedIssue(t, database, "T-1", nil)
	alice := model.Actor{Kind: "user", ID: "alice"}
	comment := seedComment(t, database, "T-1", alice, "For whoever has the issue.", nil)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{"id": comment},
	})
	const owner = "notifications.dispatch.issue.T-1.comment.created"
	record := func(process uuid.UUID, session string) {
		t.Helper()
		sent := &batchEvent{pendingEvent: pendingEvent{event: event}, outcome: outcome{
			id: event.ID, state: published, reached: []string{owner, contracts.LegacyAgentSubject(session)},
		}}
		if _, err := recordBatch(ctx, deps, []outcome{sent.outcome}, process, deliveredGraph([]*batchEvent{sent}, nil, time.Now())); err != nil {
			t.Fatalf("record the batch that reached %s: %v", session, err)
		}
	}
	holder, lost := uuid.New(), uuid.New()
	holdRow(t, database, holder)
	record(holder, "s2")
	publishedAt := readEventRow(t, database, event.ID).publishedAt
	if publishedAt == nil {
		t.Fatalf("the holder's record left event %d unpublished", event.ID)
	}
	record(lost, "s3")

	row := readEventRow(t, database, event.ID)
	destinations, after := row.destinations, row.publishedAt
	if want := []string{owner, contracts.LegacyAgentSubject("s2"), contracts.LegacyAgentSubject("s3")}; !slices.Equal(destinations, want) {
		t.Errorf("event %d was published to %v, want %v: the second record reached s3", event.ID, destinations, want)
	}
	if after == nil || !after.Equal(*publishedAt) {
		t.Errorf("event %d is published at %v after the second record, want the holder's %v", event.ID, after, publishedAt)
	}
	rows, err := database.Pool.Query(ctx, `
		select from_kind || '/' || from_id || ' > ' || to_kind || '/' || to_id from graph_tuples
		where source_event = $1 and kind = $2 order by 1
	`, event.ID, graphstore.TupleDeliveredTo)
	if err != nil {
		t.Fatalf("read the deliveries of event %d: %v", event.ID, err)
	}
	tuples, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the deliveries of event %d: %v", event.ID, err)
	}
	want := []string{
		graphstore.KindComment + "/" + comment + " > " + graphstore.KindSession + "/s2",
		graphstore.KindComment + "/" + comment + " > " + graphstore.KindSession + "/s3",
	}
	if !slices.Equal(tuples, want) {
		t.Errorf("event %d's deliveries are %v, want %v", event.ID, tuples, want)
	}
}

// TestALateRecordUnderARetakenHoldLeavesWhatAPublishedEventOwes holds the other half of a late
// record (recordBatch): a row already published moves only by the destinations the late batch
// reached, and what it owes (owed_keys, owed_seq) stays what the record that published it wrote,
// even when the late batch's process holds the publisher hold again by the time it records.
// Process a holds the hold, publishes a comment's event to s2 and records it owing nothing as of
// the owner's last_seq. Process b, which had sent the same event to s3 alone before a took the
// hold, takes the hold back and records its own batch, which still owes the topic family and s2
// as of an older read of the owner. The array gains s3's topic, and the row still owes nothing as
// of a's read.
func TestALateRecordUnderARetakenHoldLeavesWhatAPublishedEventOwes(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	deps := Deps{Store: database}
	deps.lanes = newLanes(func() {})
	seedIssue(t, database, "T-1", nil)
	alice := model.Actor{Kind: "user", ID: "alice"}
	comment := seedComment(t, database, "T-1", alice, "For whoever has the issue.", nil)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{"id": comment},
	})
	const owner = "notifications.dispatch.issue.T-1.comment.created"
	var ownerSeq int
	if err := database.Pool.QueryRow(ctx, `select last_seq from issues where key = 'T-1'`).Scan(&ownerSeq); err != nil {
		t.Fatalf("read the issue's last_seq: %v", err)
	}
	record := func(what string, sent outcome) {
		t.Helper()
		holder := uuid.New()
		holdRow(t, database, holder)
		batch := &batchEvent{pendingEvent: pendingEvent{event: event}, outcome: sent}
		holds, err := recordBatch(ctx, deps, []outcome{sent}, holder, deliveredGraph([]*batchEvent{batch}, nil, time.Now()))
		if err != nil {
			t.Fatalf("record %s: %v", what, err)
		}
		if !holds {
			t.Fatalf("record %s: the publisher hold was not its process's", what)
		}
	}
	record("a's batch, which published the event", outcome{
		id: event.ID, state: published, reached: []string{owner, contracts.LegacyAgentSubject("s2")},
		owed: []string{}, owedKnown: true, owedSeq: ownerSeq,
	})
	record("b's late batch, which reached s3 alone", outcome{
		id: event.ID, state: backedOff, attempts: 1, failure: "no responders",
		reached: []string{contracts.LegacyAgentSubject("s3")},
		owed:    []string{model.OwedTopicFamily, contracts.LegacyAgentSubject("s2")}, owedKnown: true, owedSeq: ownerSeq - 1,
	})

	// A null owed_seq reads as -1, which no issue's last_seq is.
	var destinations, owed []string
	var owedSeq int
	if err := database.Pool.QueryRow(ctx, `
		select published_destinations, owed_keys, coalesce(owed_seq, -1) from events where id = $1
	`, event.ID).Scan(&destinations, &owed, &owedSeq); err != nil {
		t.Fatalf("read event %d: %v", event.ID, err)
	}
	if want := []string{owner, contracts.LegacyAgentSubject("s2"), contracts.LegacyAgentSubject("s3")}; !slices.Equal(destinations, want) {
		t.Errorf("event %d was published to %v, want %v: the late record reached s3", event.ID, destinations, want)
	}
	if owed == nil || len(owed) != 0 {
		t.Errorf("event %d owes %v after the late record, want nothing, as the record that published it wrote", event.ID, owed)
	}
	if owedSeq != ownerSeq {
		t.Errorf("event %d's owed_seq is %d after the late record, want %d, the publishing record's read", event.ID, owedSeq, ownerSeq)
	}
}

// holdStoreWrite writes nodes and tuples in hold's own transaction and leaves it uncommitted: a
// writer's insert of the same key waits to learn whether this row commits.
func holdStoreWrite(t *testing.T, database *store.Store, hold *lockstep.Hold, nodes []graphstore.Node, tuples []graphstore.Tuple) {
	t.Helper()
	lockstep.Begin(t, database, hold)
	if err := graphstore.Write(context.Background(), hold.Tx, nodes, tuples); err != nil {
		t.Fatalf("hold %s: %v", hold.Name, err)
	}
}
