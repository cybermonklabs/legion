package delivery

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The sessions the plan fixture below is read for, and what each of them is there to say:
//
//   - titledPlanSession stamped a title once and has written since, so its newest titled event is
//     neither its newest event nor its oldest.
//   - untitledPlanSession never stamped a title, so it has no row and the read answers none.
//   - unknownPlanSession is a session the events table has never heard of.
const (
	titledPlanSession   = "ses_plan_release_sender"
	untitledPlanSession = "ses_plan_never_titled"
	unknownPlanSession  = "ses_plan_unknown"
	titledPlanName      = "Release sender"
)

// sessionTitlePlanSessions is what the read is asked for, in the order a caller would pass them.
var sessionTitlePlanSessions = []string{titledPlanSession, untitledPlanSession, unknownPlanSession}

// newSessionTitlesPlanFixture writes the events the read answers from, through the trigger on
// events that records each stamped title (migration 20261113090412): a session whose only title
// is on an older event, a session whose every event carries none, a user actor sharing the titled
// session's id (no session's title, which the trigger must not record), and a session nobody asks
// about. seedTitlesIssue clears the issue's events and the listed sessions' titles first, so a
// run against the database an earlier run left reads only these.
func newSessionTitlesPlanFixture(t *testing.T) (*store.Pool, context.Context) {
	t.Helper()
	pool, ctx := deliveryTestPool(t)
	seedTitlesIssue(t, ctx, pool, append(slices.Clone(sessionTitlePlanSessions), "ses_plan_not_asked_for")...)

	at := time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC)
	minute := 0
	next := func() time.Time {
		minute++
		return at.Add(time.Duration(minute) * time.Minute)
	}
	untitled := actorJSON(t, model.Actor{Kind: "session", ID: titledPlanSession})
	for range 6 {
		writeSessionEvent(t, ctx, pool, next(), untitled)
	}
	writeSessionEvent(t, ctx, pool, next(), actorJSON(t, model.Actor{
		Kind: "session", ID: titledPlanSession, Origin: &model.ActorOrigin{SessionTitle: titledPlanName},
	}))
	for range 3 {
		writeSessionEvent(t, ctx, pool, next(), untitled)
	}
	for range 4 {
		writeSessionEvent(t, ctx, pool, next(), actorJSON(t, model.Actor{Kind: "session", ID: untitledPlanSession}))
	}
	writeSessionEvent(t, ctx, pool, next(), actorJSON(t, model.Actor{
		Kind: "user", ID: titledPlanSession, Origin: &model.ActorOrigin{SessionTitle: "A person, not the session"},
	}))
	writeSessionEvent(t, ctx, pool, next(), actorJSON(t, model.Actor{
		Kind: "session", ID: "ses_plan_not_asked_for", Origin: &model.ActorOrigin{SessionTitle: "Unasked for"},
	}))
	return pool, ctx
}

// The session titles read reads session_titles by its primary key and no row of events: the title
// a page shows is stored where it is stamped (dispatch://INFRA-2916, R13), and resolving it out of
// events on each read cost 42 to 133 ms a paint. The plan is `EXPLAIN (ANALYZE, BUFFERS)` of the
// read as ResolveSessionTitles sends it, and the read answers the titled session alone: no row for
// a session that never stamped a title, none for one the events table never heard of, and nothing
// from a person's actor that shares a session's id.
func TestTheSessionTitlesReadReadsNoEvents(t *testing.T) {
	pool, ctx := newSessionTitlesPlanFixture(t)
	plan, raw := analyzedPlan(t, ctx, pool, resolveSessionTitlesQuery, sessionTitlePlanSessions)
	if read := scannedRelations(t, plan); !slices.Equal(read, []string{"session_titles"}) {
		t.Fatalf("the session titles read reads %v; want session_titles alone, and no events:\n%s", read, raw)
	}
	if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
		t.Fatalf("the session titles read reads %v, which no index condition keys:\n%s", unkeyed, raw)
	}
	indexScanOf(t, plan, raw, "session_titles", "session_titles_pkey")
	// Every scan carries an index condition (the check above); this holds the primary key's to
	// its first column, the session.
	explaintest.NewIndexKeys(pool).RequireIndexKeyed(t, plan, "session_titles_pkey", "the session titles read", explaintest.CondOptional)

	titles, err := ResolveSessionTitles(ctx, pool, sessionTitlePlanSessions)
	if err != nil {
		t.Fatalf("ResolveSessionTitles: %v", err)
	}
	if want := map[string]string{titledPlanSession: titledPlanName}; !maps.Equal(titles, want) {
		t.Fatalf("ResolveSessionTitles(%v) = %v, want %v", sessionTitlePlanSessions, titles, want)
	}
}

// A retitle is stored in the transaction that appends the event carrying it: another connection
// reads the session's old title while that transaction is open and the new one once it commits,
// and a transaction that appends a retitle and rolls back leaves the title as it was. The events
// go through events.Broker.Append, the one server path that inserts an event.
func TestARetitleChangesTheStoredTitleInTheEventsOwnTransaction(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const session = "ses_retitled_in_its_transaction"
	seedTitlesIssue(t, ctx, pool, session)
	broker := events.NewBroker()
	issue := titlesIssue
	appendTitled := func(t *testing.T, title string) pgx.Tx {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the write of %q: %v", title, err)
		}
		if _, err := broker.Append(ctx, tx, model.Event{
			IssueKey: &issue, Type: "comment.created", Payload: map[string]any{},
			Actor: model.Actor{Kind: "session", ID: session, Origin: &model.ActorOrigin{SessionTitle: title}},
		}); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("append the event titled %q: %v", title, err)
		}
		return tx
	}
	titleOf := func(t *testing.T) string {
		t.Helper()
		titles, err := ResolveSessionTitles(ctx, pool, []string{session})
		if err != nil {
			t.Fatalf("ResolveSessionTitles: %v", err)
		}
		return SessionTitle(titles, session)
	}

	first := appendTitled(t, "Release sender")
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first title: %v", err)
	}
	if got := titleOf(t); got != "Release sender" {
		t.Fatalf("the title once the first event committed = %q, want %q", got, "Release sender")
	}

	retitle := appendTitled(t, "Release captain")
	if got := titleOf(t); got != "Release sender" {
		_ = retitle.Rollback(ctx)
		t.Fatalf("the title another connection reads while the retitle is open = %q, want the committed %q", got, "Release sender")
	}
	if err := retitle.Commit(ctx); err != nil {
		t.Fatalf("commit the retitle: %v", err)
	}
	if got := titleOf(t); got != "Release captain" {
		t.Fatalf("the title once the retitle committed = %q, want %q", got, "Release captain")
	}

	abandoned := appendTitled(t, "Never committed")
	if err := abandoned.Rollback(ctx); err != nil {
		t.Fatalf("roll the abandoned retitle back: %v", err)
	}
	if got := titleOf(t); got != "Release captain" {
		t.Fatalf("the title after a retitle rolled back = %q, want %q", got, "Release captain")
	}
}
